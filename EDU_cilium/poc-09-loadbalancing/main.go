// poc-09-loadbalancing: Cilium 로드 밸런싱 서브시스템 시뮬레이션
//
// 이 PoC는 Cilium의 핵심 LB 메커니즘을 순수 Go로 구현한다:
//   1. Maglev 일관 해싱 (룩업 테이블 구성 + 균등 분배 검증)
//   2. Random 백엔드 선택
//   3. Session Affinity (클라이언트별 고정 백엔드)
//   4. DSR vs non-DSR 패킷 흐름 시뮬레이션
//   5. Socket-level LB (connect-time 주소 변환)
//   6. 백엔드 추가/제거 시 Maglev 테이블 재구성 (최소 혼란 검증)
//
// 외부 의존성 없이 Go 표준 라이브러리만 사용한다.

package main

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// 1. 핵심 타입 정의 (pkg/loadbalancer/ 참조)
// =============================================================================

// BackendID는 백엔드의 고유 식별자 (Cilium: pkg/loadbalancer/loadbalancer.go의 BackendID)
type BackendID uint32

// Backend은 실제 서비스 엔드포인트 (Cilium: bpf/lib/lb.h의 struct lb4_backend)
type Backend struct {
	ID      BackendID
	Address net.IP
	Port    uint16
	Weight  uint16
	State   BackendState
}

// BackendState는 백엔드 상태 (Cilium: pkg/loadbalancer/loadbalancer.go)
type BackendState uint8

const (
	BackendStateActive BackendState = iota
	BackendStateTerminating
	BackendStateQuarantined
	BackendStateMaintenance
)

func (s BackendState) String() string {
	switch s {
	case BackendStateActive:
		return "Active"
	case BackendStateTerminating:
		return "Terminating"
	case BackendStateQuarantined:
		return "Quarantined"
	case BackendStateMaintenance:
		return "Maintenance"
	default:
		return "Unknown"
	}
}

// SVCType은 서비스 유형 (Cilium: pkg/loadbalancer/loadbalancer.go)
type SVCType string

const (
	SVCTypeClusterIP    SVCType = "ClusterIP"
	SVCTypeNodePort     SVCType = "NodePort"
	SVCTypeLoadBalancer SVCType = "LoadBalancer"
	SVCTypeExternalIP   SVCType = "ExternalIP"
)

// ForwardingMode: SNAT 또는 DSR
type ForwardingMode string

const (
	ForwardingModeSNAT ForwardingMode = "snat"
	ForwardingModeDSR  ForwardingMode = "dsr"
)

// LBAlgorithm: Random 또는 Maglev
type LBAlgorithm string

const (
	LBAlgorithmRandom LBAlgorithm = "random"
	LBAlgorithmMaglev LBAlgorithm = "maglev"
)

// Service는 LB 서비스 (Cilium: pkg/loadbalancer/service.go의 Service 구조체)
type Service struct {
	Name             string
	VIP              net.IP
	Port             uint16
	Type             SVCType
	Algorithm        LBAlgorithm
	ForwardingMode   ForwardingMode
	SessionAffinity  bool
	AffinityTimeout  time.Duration
	Backends         []Backend
	RevNatIndex      uint16
}

// =============================================================================
// 2. Maglev 일관 해싱 (pkg/maglev/maglev.go 참조)
// =============================================================================

// MaglevTable은 Maglev 룩업 테이블을 관리한다.
// Cilium에서는 pkg/maglev/maglev.go의 Maglev 구조체가 이 역할을 한다.
type MaglevTable struct {
	TableSize uint64 // M: 소수여야 함
	Seed      uint32
	Entry     []BackendID // 룩업 테이블 (길이 = M)
}

// getOffsetAndSkip은 Maglev 논문의 순열 파라미터를 계산한다.
// Cilium 구현: pkg/maglev/maglev.go의 getOffsetAndSkip()
// 원본은 murmur3.Hash128을 사용하지만 여기서는 sha256으로 시뮬레이션한다.
func getOffsetAndSkip(backendKey string, m uint64, seed uint32) (uint64, uint64) {
	// h1, h2를 별도의 해시로 계산 (원본: murmur3.Hash128)
	data := fmt.Sprintf("%s-%d", backendKey, seed)

	h := sha256.Sum256([]byte(data))
	h1 := binary.LittleEndian.Uint64(h[0:8])
	h2 := binary.LittleEndian.Uint64(h[8:16])

	offset := h1 % m
	skip := (h2 % (m - 1)) + 1
	return offset, skip
}

// backendHashString은 백엔드의 해시 문자열을 생성한다.
// Cilium 구현: pkg/maglev/maglev.go의 BackendInfo.setHashString()
func backendHashString(b Backend) string {
	return fmt.Sprintf("[%s:%d/TCP,State:active]", b.Address.String(), b.Port)
}

// BuildMaglevTable은 Maglev 룩업 테이블을 생성한다.
// Cilium 구현: pkg/maglev/maglev.go의 GetLookupTable() + computeLookupTable()
func BuildMaglevTable(backends []Backend, tableSize uint64, seed uint32) *MaglevTable {
	m := tableSize
	n := len(backends)

	if n == 0 {
		return &MaglevTable{TableSize: m, Seed: seed, Entry: make([]BackendID, m)}
	}

	// 1단계: 백엔드를 해시 문자열 기준으로 정렬 (클러스터 간 일관성)
	// Cilium에서는 slices.SortFunc(backends, cmp.Compare(a.hashString, b.hashString))
	type backendWithHash struct {
		backend  Backend
		hashStr  string
	}
	sorted := make([]backendWithHash, n)
	for i, b := range backends {
		sorted[i] = backendWithHash{backend: b, hashStr: backendHashString(b)}
	}
	// 안정 정렬
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if sorted[i].hashStr > sorted[j].hashStr {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}

	// 2단계: 각 백엔드의 순열 계산
	// Cilium: getPermutation() - 병렬 처리로 offset/skip 기반 순열 생성
	permutations := make([][]uint64, n)
	for i := 0; i < n; i++ {
		offset, skip := getOffsetAndSkip(sorted[i].hashStr, m, seed)
		perm := make([]uint64, m)
		perm[0] = offset
		for j := uint64(1); j < m; j++ {
			perm[j] = (perm[j-1] + skip) % m
		}
		permutations[i] = perm
	}

	// 3단계: 라운드 로빈으로 테이블 채우기
	// Cilium: computeLookupTable()의 핵심 루프
	const sentinel = BackendID(0xFFFFFFFF)
	entry := make([]BackendID, m)
	for j := range entry {
		entry[j] = sentinel
	}

	next := make([]int, n)
	for j := uint64(0); j < m; j++ {
		i := int(j) % n
		c := permutations[i][next[i]]
		for entry[c] != sentinel {
			next[i]++
			c = permutations[i][next[i]]
		}
		entry[c] = sorted[i].backend.ID
		next[i]++
	}

	return &MaglevTable{TableSize: m, Seed: seed, Entry: entry}
}

// Lookup은 5-tuple 해시로 백엔드를 선택한다.
// Cilium BPF: lb4_select_backend_id_maglev() -- hash(tuple) % LB_MAGLEV_LUT_SIZE
func (mt *MaglevTable) Lookup(key string) BackendID {
	h := fnv.New64a()
	h.Write([]byte(key))
	index := h.Sum64() % mt.TableSize
	return mt.Entry[index]
}

// =============================================================================
// 3. Random 백엔드 선택
// =============================================================================

// RandomSelect는 랜덤으로 백엔드를 선택한다.
// Cilium BPF: lb4_select_backend_id_random() -- (get_prandom_u32() % count) + 1
func RandomSelect(backends []Backend) *Backend {
	active := make([]int, 0, len(backends))
	for i, b := range backends {
		if b.State == BackendStateActive {
			active = append(active, i)
		}
	}
	if len(active) == 0 {
		return nil
	}
	idx := active[rand.Intn(len(active))]
	return &backends[idx]
}

// =============================================================================
// 4. Session Affinity (세션 친화성)
// =============================================================================

// AffinityEntry는 세션 친화성 맵 엔트리.
// Cilium BPF: struct lb_affinity_val { last_used, backend_id }
type AffinityEntry struct {
	BackendID BackendID
	LastUsed  time.Time
}

// AffinityMap은 클라이언트별 세션 친화성을 관리한다.
// Cilium BPF: cilium_lb4_affinity (LRU Hash Map)
type AffinityMap struct {
	mu      sync.RWMutex
	entries map[string]*AffinityEntry // key: "clientIP-revNatID"
	timeout time.Duration
}

func NewAffinityMap(timeout time.Duration) *AffinityMap {
	return &AffinityMap{
		entries: make(map[string]*AffinityEntry),
		timeout: timeout,
	}
}

func affinityKey(clientIP string, revNatID uint16) string {
	return fmt.Sprintf("%s-%d", clientIP, revNatID)
}

// Lookup은 클라이언트에 대한 기존 친화성 백엔드를 반환한다.
// Cilium BPF: lb4_affinity_backend_id_by_addr()
func (am *AffinityMap) Lookup(clientIP string, revNatID uint16) (BackendID, bool) {
	am.mu.RLock()
	defer am.mu.RUnlock()

	key := affinityKey(clientIP, revNatID)
	entry, exists := am.entries[key]
	if !exists {
		return 0, false
	}

	// 타임아웃 확인 (Cilium: lb_affinity_is_timeout())
	if time.Since(entry.LastUsed) > am.timeout {
		return 0, false
	}

	return entry.BackendID, true
}

// Update는 세션 친화성을 기록/갱신한다.
func (am *AffinityMap) Update(clientIP string, revNatID uint16, backendID BackendID) {
	am.mu.Lock()
	defer am.mu.Unlock()

	key := affinityKey(clientIP, revNatID)
	am.entries[key] = &AffinityEntry{
		BackendID: backendID,
		LastUsed:  time.Now(),
	}
}

// =============================================================================
// 5. Service Map & Backend Map (BPF 맵 시뮬레이션)
// =============================================================================

// ServiceMapEntry는 서비스 맵의 마스터 엔트리.
// Cilium BPF: struct lb4_service (slot=0)
type ServiceMapEntry struct {
	Count       int
	RevNatIndex uint16
	Flags       uint16
	Algorithm   LBAlgorithm
	Timeout     uint32 // Session affinity timeout (초)
	BackendSlots []BackendID
}

// ServiceMap은 서비스 맵을 시뮬레이션한다.
// Cilium BPF: cilium_lb4_services_v2
type ServiceMap struct {
	entries map[string]*ServiceMapEntry // key: "VIP:port:proto"
}

// BackendMap은 백엔드 맵을 시뮬레이션한다.
// Cilium BPF: cilium_lb4_backends_v3
type BackendMap struct {
	entries map[BackendID]*Backend
}

// RevNatMap은 Reverse NAT 맵을 시뮬레이션한다.
// Cilium BPF: cilium_lb4_reverse_nat
type RevNatMap struct {
	entries map[uint16]*RevNatEntry
}

type RevNatEntry struct {
	Address net.IP
	Port    uint16
}

// MaglevMapStore는 Maglev 외부/내부 맵을 시뮬레이션한다.
// Cilium BPF: cilium_lb4_maglev (Hash-of-Maps)
type MaglevMapStore struct {
	tables map[uint16]*MaglevTable // key: rev_nat_index
}

// SockRevNatMap은 Socket Reverse NAT 맵을 시뮬레이션한다.
// Cilium BPF: cilium_lb_sock_rev_nat4
type SockRevNatMap struct {
	mu      sync.RWMutex
	entries map[string]*SockRevNatEntry // key: "cookie-addr-port"
}

type SockRevNatEntry struct {
	OrigAddr    net.IP
	OrigPort    uint16
	RevNatIndex uint16
}

// =============================================================================
// 6. DSR vs SNAT 패킷 흐름 시뮬레이션
// =============================================================================

// Packet은 네트워크 패킷을 시뮬레이션한다.
type Packet struct {
	SrcIP   net.IP
	DstIP   net.IP
	SrcPort uint16
	DstPort uint16
	Proto   string
	Payload string

	// DSR 전용 필드 (Cilium: IP Option 또는 Geneve 헤더에 인코딩)
	DSRInfo *DSRInfo
}

type DSRInfo struct {
	OriginalVIP  net.IP
	OriginalPort uint16
}

func (p Packet) String() string {
	dsr := ""
	if p.DSRInfo != nil {
		dsr = fmt.Sprintf(" [DSR: VIP=%s:%d]", p.DSRInfo.OriginalVIP, p.DSRInfo.OriginalPort)
	}
	return fmt.Sprintf("%s:%d -> %s:%d/%s%s", p.SrcIP, p.SrcPort, p.DstIP, p.DstPort, p.Proto, dsr)
}

// SimulateSNAT는 SNAT 모드의 패킷 흐름을 시뮬레이션한다.
func SimulateSNAT(client Packet, lbNodeIP net.IP, backend Backend) {
	fmt.Println("  === SNAT 모드 패킷 흐름 ===")

	// 1. 클라이언트 -> LB (원본 패킷)
	fmt.Printf("  [1] Client -> LB:      %s\n", client)

	// 2. LB -> 백엔드 (DNAT + SNAT)
	toBackend := Packet{
		SrcIP: lbNodeIP, SrcPort: 61000, // SNAT: src를 LB 노드로 변환
		DstIP: backend.Address, DstPort: backend.Port, // DNAT: dst를 백엔드로 변환
		Proto: client.Proto,
	}
	fmt.Printf("  [2] LB -> Backend:     %s  (DNAT+SNAT 적용)\n", toBackend)

	// 3. 백엔드 -> LB (응답)
	response := Packet{
		SrcIP: backend.Address, SrcPort: backend.Port,
		DstIP: lbNodeIP, DstPort: 61000,
		Proto: client.Proto,
	}
	fmt.Printf("  [3] Backend -> LB:     %s  (응답이 LB 경유)\n", response)

	// 4. LB -> 클라이언트 (RevNAT 적용)
	toClient := Packet{
		SrcIP: client.DstIP, SrcPort: client.DstPort, // RevNAT: src를 VIP로 복원
		DstIP: client.SrcIP, DstPort: client.SrcPort,
		Proto: client.Proto,
	}
	fmt.Printf("  [4] LB -> Client:      %s  (RevNAT 적용)\n", toClient)
	fmt.Printf("  총 LB 노드 경유 패킷: 4 (요청 2 + 응답 2)\n")
}

// SimulateDSR은 DSR 모드의 패킷 흐름을 시뮬레이션한다.
func SimulateDSR(client Packet, lbNodeIP net.IP, backend Backend) {
	fmt.Println("  === DSR 모드 패킷 흐름 ===")

	// 1. 클라이언트 -> LB (원본 패킷)
	fmt.Printf("  [1] Client -> LB:      %s\n", client)

	// 2. LB -> 백엔드 (DNAT + DSR 헤더 추가, SNAT 없음)
	toBackend := Packet{
		SrcIP: client.SrcIP, SrcPort: client.SrcPort, // SNAT 없음: 클라이언트 주소 유지
		DstIP: backend.Address, DstPort: backend.Port,  // DNAT: dst를 백엔드로 변환
		Proto: client.Proto,
		DSRInfo: &DSRInfo{
			OriginalVIP:  client.DstIP,
			OriginalPort: client.DstPort,
		},
	}
	fmt.Printf("  [2] LB -> Backend:     %s\n", toBackend)

	// 3. 백엔드 -> 클라이언트 (직접 응답, LB 우회)
	directResponse := Packet{
		SrcIP: client.DstIP, SrcPort: client.DstPort, // DSR: src를 VIP로 설정
		DstIP: client.SrcIP, DstPort: client.SrcPort,
		Proto: client.Proto,
	}
	fmt.Printf("  [3] Backend -> Client: %s  (LB 우회, 직접 응답)\n", directResponse)
	fmt.Printf("  총 LB 노드 경유 패킷: 2 (요청만, 응답은 직접)\n")
}

// =============================================================================
// 7. Socket-level LB 시뮬레이션
// =============================================================================

// SocketLB는 cgroup/connect BPF 후크를 시뮬레이션한다.
// Cilium: bpf/bpf_sock.c의 cil_sock4_connect() -> __sock4_xlate_fwd()
type SocketLB struct {
	serviceMap  *ServiceMap
	backendMap  *BackendMap
	maglevStore *MaglevMapStore
	affinityMap *AffinityMap
	sockRevNat  *SockRevNatMap
}

// Socket은 소켓 상태를 시뮬레이션한다.
type Socket struct {
	Cookie      uint64
	OrigDstIP   net.IP // 원래 목적지 (VIP)
	OrigDstPort uint16
	ActualDstIP   net.IP // 변환된 목적지 (백엔드)
	ActualDstPort uint16
	Connected     bool
}

// Connect는 connect() 시스콜을 시뮬레이션한다.
// Cilium BPF: cil_sock4_connect() -> __sock4_xlate_fwd()
func (slb *SocketLB) Connect(sock *Socket, dstIP net.IP, dstPort uint16) error {
	sock.OrigDstIP = make(net.IP, len(dstIP))
	copy(sock.OrigDstIP, dstIP)
	sock.OrigDstPort = dstPort

	// Service Map 조회 (Cilium: sock4_wildcard_lookup + __lb4_lookup_service)
	svcKey := fmt.Sprintf("%s:%d:TCP", dstIP.String(), dstPort)
	svc, ok := slb.serviceMap.entries[svcKey]
	if !ok {
		// 서비스가 아니면 그대로 통과
		sock.ActualDstIP = dstIP
		sock.ActualDstPort = dstPort
		sock.Connected = true
		return nil
	}

	// 백엔드 선택
	var backendID BackendID
	if svc.Algorithm == LBAlgorithmMaglev {
		mt, ok := slb.maglevStore.tables[svc.RevNatIndex]
		if ok {
			tupleKey := fmt.Sprintf("%s:%d->%s:%d", "10.0.0.1", sock.Cookie%65535, dstIP, dstPort)
			backendID = mt.Lookup(tupleKey)
		}
	}
	if backendID == 0 && len(svc.BackendSlots) > 0 {
		// Random 폴백
		slot := rand.Intn(len(svc.BackendSlots))
		backendID = svc.BackendSlots[slot]
	}

	backend, ok := slb.backendMap.entries[backendID]
	if !ok {
		return fmt.Errorf("backend %d not found", backendID)
	}

	// 주소 변환 (Cilium: ctx->user_ip4 = backend->address)
	sock.ActualDstIP = backend.Address
	sock.ActualDstPort = backend.Port
	sock.Connected = true

	// SockRevNAT 기록 (Cilium: sock4_update_revnat)
	slb.sockRevNat.mu.Lock()
	revKey := fmt.Sprintf("%d-%s-%d", sock.Cookie, backend.Address, backend.Port)
	slb.sockRevNat.entries[revKey] = &SockRevNatEntry{
		OrigAddr:    sock.OrigDstIP,
		OrigPort:    sock.OrigDstPort,
		RevNatIndex: svc.RevNatIndex,
	}
	slb.sockRevNat.mu.Unlock()

	return nil
}

// GetPeerName은 getpeername()을 시뮬레이션한다 (원래 VIP 반환).
// Cilium BPF: cil_sock4_getpeername() -> __sock4_xlate_rev()
func (slb *SocketLB) GetPeerName(sock *Socket) (net.IP, uint16) {
	if sock.OrigDstIP != nil {
		return sock.OrigDstIP, sock.OrigDstPort
	}
	return sock.ActualDstIP, sock.ActualDstPort
}

// =============================================================================
// 8. 메인 데모 함수들
// =============================================================================

func printHeader(title string) {
	fmt.Println()
	fmt.Println(strings.Repeat("=", 70))
	fmt.Printf(" %s\n", title)
	fmt.Println(strings.Repeat("=", 70))
}

func printSubHeader(title string) {
	fmt.Printf("\n--- %s ---\n", title)
}

// demoMaglev는 Maglev 일관 해싱을 데모한다.
func demoMaglev() {
	printHeader("1. Maglev 일관 해싱")

	backends := []Backend{
		{ID: 1, Address: net.ParseIP("10.0.1.1"), Port: 8080, Weight: 100, State: BackendStateActive},
		{ID: 2, Address: net.ParseIP("10.0.1.2"), Port: 8080, Weight: 100, State: BackendStateActive},
		{ID: 3, Address: net.ParseIP("10.0.1.3"), Port: 8080, Weight: 100, State: BackendStateActive},
		{ID: 4, Address: net.ParseIP("10.0.1.4"), Port: 8080, Weight: 100, State: BackendStateActive},
		{ID: 5, Address: net.ParseIP("10.0.1.5"), Port: 8080, Weight: 100, State: BackendStateActive},
	}

	// Cilium 기본값: M=16381 (소수). 여기서는 시연을 위해 소규모 소수 사용.
	const tableSize = 251
	const seed = 42

	fmt.Printf("\n  백엔드 수: %d\n", len(backends))
	fmt.Printf("  테이블 크기 M: %d (소수)\n", tableSize)
	fmt.Printf("  해시 시드: %d\n", seed)

	mt := BuildMaglevTable(backends, tableSize, seed)

	// 분배 통계 확인
	printSubHeader("균등 분배 검증")
	distribution := make(map[BackendID]int)
	for _, bid := range mt.Entry {
		distribution[bid]++
	}
	fmt.Printf("  이상적 분배: %d 엔트리/백엔드 (총 %d / %d 백엔드)\n",
		tableSize/uint64(len(backends)), tableSize, len(backends))

	for _, b := range backends {
		count := distribution[b.ID]
		pct := float64(count) / float64(tableSize) * 100.0
		bar := strings.Repeat("#", int(pct*2))
		fmt.Printf("  Backend %d (%s:%d): %3d 엔트리 (%5.1f%%) %s\n",
			b.ID, b.Address, b.Port, count, pct, bar)
	}

	// 해시 기반 조회 테스트
	printSubHeader("5-tuple 기반 조회 (Cilium: hash(tuple) %% M)")
	testFlows := []string{
		"192.168.1.1:12345->10.96.0.1:80",
		"192.168.1.2:23456->10.96.0.1:80",
		"192.168.1.3:34567->10.96.0.1:80",
		"192.168.1.1:12345->10.96.0.1:80", // 동일 flow는 동일 백엔드
		"172.16.0.50:9999->10.96.0.1:80",
	}
	for _, flow := range testFlows {
		bid := mt.Lookup(flow)
		fmt.Printf("  Flow %-40s -> Backend %d\n", flow, bid)
	}
}

// demoMaglevMinimalDisruption은 백엔드 추가/제거 시 최소 혼란을 검증한다.
func demoMaglevMinimalDisruption() {
	printHeader("2. Maglev 최소 혼란 검증 (백엔드 추가/제거)")

	const tableSize = 509
	const seed = 42

	// 초기 백엔드 3개
	backends3 := []Backend{
		{ID: 1, Address: net.ParseIP("10.0.1.1"), Port: 8080, Weight: 100, State: BackendStateActive},
		{ID: 2, Address: net.ParseIP("10.0.1.2"), Port: 8080, Weight: 100, State: BackendStateActive},
		{ID: 3, Address: net.ParseIP("10.0.1.3"), Port: 8080, Weight: 100, State: BackendStateActive},
	}

	// 백엔드 1개 추가 (4개)
	backends4 := append([]Backend{}, backends3...)
	backends4 = append(backends4, Backend{
		ID: 4, Address: net.ParseIP("10.0.1.4"), Port: 8080, Weight: 100, State: BackendStateActive,
	})

	// 백엔드 1개 제거 (ID=2 제거 -> 2개 남음)
	backends2 := []Backend{backends3[0], backends3[2]}

	table3 := BuildMaglevTable(backends3, tableSize, seed)
	table4 := BuildMaglevTable(backends4, tableSize, seed)
	table2 := BuildMaglevTable(backends2, tableSize, seed)

	// 변경된 슬롯 수 계산
	changedAdd := 0
	for i := uint64(0); i < tableSize; i++ {
		if table3.Entry[i] != table4.Entry[i] {
			changedAdd++
		}
	}

	changedRemove := 0
	for i := uint64(0); i < tableSize; i++ {
		if table3.Entry[i] != table2.Entry[i] {
			changedRemove++
		}
	}

	printSubHeader("백엔드 추가 (3 -> 4)")
	fmt.Printf("  테이블 크기: %d\n", tableSize)
	fmt.Printf("  변경된 슬롯: %d / %d (%.1f%%)\n", changedAdd, tableSize,
		float64(changedAdd)/float64(tableSize)*100.0)
	fmt.Printf("  이상적 최소 변경: %.1f%% (1/4 = 새 백엔드 몫)\n", 100.0/4.0)

	printSubHeader("백엔드 제거 (3 -> 2)")
	fmt.Printf("  변경된 슬롯: %d / %d (%.1f%%)\n", changedRemove, tableSize,
		float64(changedRemove)/float64(tableSize)*100.0)
	fmt.Printf("  이상적 최소 변경: %.1f%% (1/3 = 제거된 백엔드 몫)\n", 100.0/3.0)
}

// demoRandom은 Random LB를 데모한다.
func demoRandom() {
	printHeader("3. Random 백엔드 선택")

	backends := []Backend{
		{ID: 1, Address: net.ParseIP("10.0.1.1"), Port: 8080, State: BackendStateActive},
		{ID: 2, Address: net.ParseIP("10.0.1.2"), Port: 8080, State: BackendStateActive},
		{ID: 3, Address: net.ParseIP("10.0.1.3"), Port: 8080, State: BackendStateQuarantined},
		{ID: 4, Address: net.ParseIP("10.0.1.4"), Port: 8080, State: BackendStateActive},
	}

	fmt.Printf("\n  백엔드: ")
	for _, b := range backends {
		fmt.Printf("ID=%d(%s) ", b.ID, b.State)
	}
	fmt.Println()

	// Cilium: get_prandom_u32() % count + 1
	const trials = 10000
	distribution := make(map[BackendID]int)
	for i := 0; i < trials; i++ {
		selected := RandomSelect(backends)
		if selected != nil {
			distribution[selected.ID]++
		}
	}

	fmt.Printf("\n  %d번 선택 결과 (Quarantined 백엔드 ID=3은 제외):\n", trials)
	for _, b := range backends {
		count := distribution[b.ID]
		pct := float64(count) / float64(trials) * 100.0
		bar := strings.Repeat("#", int(pct/2))
		fmt.Printf("  Backend %d (%s): %5d회 (%5.1f%%) %s\n",
			b.ID, b.State, count, pct, bar)
	}
}

// demoSessionAffinity는 Session Affinity를 데모한다.
func demoSessionAffinity() {
	printHeader("4. Session Affinity (세션 친화성)")

	backends := []Backend{
		{ID: 1, Address: net.ParseIP("10.0.1.1"), Port: 8080, State: BackendStateActive},
		{ID: 2, Address: net.ParseIP("10.0.1.2"), Port: 8080, State: BackendStateActive},
		{ID: 3, Address: net.ParseIP("10.0.1.3"), Port: 8080, State: BackendStateActive},
	}

	// Cilium: sessionAffinityTimeout (기본 3시간, 여기서는 2초로 시뮬레이션)
	affinityTimeout := 2 * time.Second
	am := NewAffinityMap(affinityTimeout)
	revNatID := uint16(1)

	clients := []string{"192.168.1.10", "192.168.1.20", "192.168.1.30"}

	fmt.Printf("\n  Affinity 타임아웃: %v\n", affinityTimeout)
	fmt.Printf("  백엔드 수: %d\n", len(backends))

	// 첫 번째 라운드: 각 클라이언트에게 백엔드 할당
	printSubHeader("첫 번째 요청 (새 할당)")
	for _, clientIP := range clients {
		// 친화성 조회 시도
		backendID, found := am.Lookup(clientIP, revNatID)
		if !found {
			// 새 백엔드 선택 (random)
			selected := RandomSelect(backends)
			backendID = selected.ID
			am.Update(clientIP, revNatID, backendID)
			fmt.Printf("  Client %s -> Backend %d (새 할당)\n", clientIP, backendID)
		} else {
			fmt.Printf("  Client %s -> Backend %d (친화성 히트)\n", clientIP, backendID)
		}
	}

	// 두 번째 라운드: 동일 클라이언트 -> 동일 백엔드
	printSubHeader("두 번째 요청 (친화성 히트 예상)")
	for _, clientIP := range clients {
		backendID, found := am.Lookup(clientIP, revNatID)
		if found {
			fmt.Printf("  Client %s -> Backend %d (친화성 히트, timeout 내)\n", clientIP, backendID)
		} else {
			selected := RandomSelect(backends)
			backendID = selected.ID
			am.Update(clientIP, revNatID, backendID)
			fmt.Printf("  Client %s -> Backend %d (친화성 미스, 재할당)\n", clientIP, backendID)
		}
	}

	// 타임아웃 대기
	printSubHeader(fmt.Sprintf("타임아웃 대기 (%v)...", affinityTimeout+500*time.Millisecond))
	time.Sleep(affinityTimeout + 500*time.Millisecond)

	// 세 번째 라운드: 타임아웃 후 재할당
	printSubHeader("세 번째 요청 (타임아웃 후)")
	for _, clientIP := range clients {
		backendID, found := am.Lookup(clientIP, revNatID)
		if found {
			fmt.Printf("  Client %s -> Backend %d (친화성 히트)\n", clientIP, backendID)
		} else {
			selected := RandomSelect(backends)
			backendID = selected.ID
			am.Update(clientIP, revNatID, backendID)
			fmt.Printf("  Client %s -> Backend %d (친화성 만료, 재할당)\n", clientIP, backendID)
		}
	}
}

// demoDSR은 DSR vs SNAT 패킷 흐름 차이를 데모한다.
func demoDSR() {
	printHeader("5. DSR vs SNAT 패킷 흐름 비교")

	lbNodeIP := net.ParseIP("192.168.0.10")
	backend := Backend{
		ID: 1, Address: net.ParseIP("10.0.1.1"), Port: 8080,
	}

	clientPacket := Packet{
		SrcIP: net.ParseIP("203.0.113.50"), SrcPort: 45678,
		DstIP: net.ParseIP("10.96.0.1"), DstPort: 80,
		Proto: "TCP",
	}

	fmt.Printf("\n  클라이언트: %s:%d\n", clientPacket.SrcIP, clientPacket.SrcPort)
	fmt.Printf("  서비스 VIP: %s:%d\n", clientPacket.DstIP, clientPacket.DstPort)
	fmt.Printf("  LB 노드:   %s\n", lbNodeIP)
	fmt.Printf("  백엔드:     %s:%d\n", backend.Address, backend.Port)

	fmt.Println()
	SimulateSNAT(clientPacket, lbNodeIP, backend)

	fmt.Println()
	SimulateDSR(clientPacket, lbNodeIP, backend)

	fmt.Println()
	fmt.Println("  [비교 요약]")
	fmt.Println("  SNAT: LB 노드가 요청/응답 모두 처리 -> 대역폭 병목, 비대칭 부하")
	fmt.Println("  DSR:  응답은 백엔드에서 직접 전송 -> LB 노드 부하 감소, 더 나은 성능")
}

// demoSocketLB는 Socket-level LB를 데모한다.
func demoSocketLB() {
	printHeader("6. Socket-level LB (connect-time 주소 변환)")

	// 서비스 및 백엔드 맵 설정
	svcMap := &ServiceMap{entries: make(map[string]*ServiceMapEntry)}
	beMap := &BackendMap{entries: make(map[BackendID]*Backend)}
	maglevStore := &MaglevMapStore{tables: make(map[uint16]*MaglevTable)}
	sockRevNat := &SockRevNatMap{entries: make(map[string]*SockRevNatEntry)}

	// 백엔드 등록
	backends := []Backend{
		{ID: 1, Address: net.ParseIP("10.0.1.1"), Port: 8080, State: BackendStateActive},
		{ID: 2, Address: net.ParseIP("10.0.1.2"), Port: 8080, State: BackendStateActive},
	}
	for i := range backends {
		beMap.entries[backends[i].ID] = &backends[i]
	}

	// 서비스 등록 (10.96.0.1:80)
	svcMap.entries["10.96.0.1:80:TCP"] = &ServiceMapEntry{
		Count:        2,
		RevNatIndex:  1,
		Algorithm:    LBAlgorithmRandom,
		BackendSlots: []BackendID{1, 2},
	}

	slb := &SocketLB{
		serviceMap:  svcMap,
		backendMap:  beMap,
		maglevStore: maglevStore,
		sockRevNat:  sockRevNat,
	}

	fmt.Println("\n  [패킷 수준 LB vs 소켓 수준 LB 비교]")
	fmt.Println()
	fmt.Println("  패킷 수준 LB:")
	fmt.Println("    connect(10.96.0.1:80) -> 소켓 dst=10.96.0.1:80 유지")
	fmt.Println("    send() -> 매 패킷: DNAT(10.96.0.1 -> 10.0.1.x)")
	fmt.Println("    recv() -> 매 패킷: SNAT(10.0.1.x -> 10.96.0.1)")
	fmt.Println("    = 매 패킷마다 conntrack 조회 + NAT 수행 필요")
	fmt.Println()
	fmt.Println("  소켓 수준 LB (Cilium cgroup/connect):")

	// connect() 시뮬레이션
	sock := &Socket{Cookie: 12345}
	vip := net.ParseIP("10.96.0.1")
	vipPort := uint16(80)

	err := slb.Connect(sock, vip, vipPort)
	if err != nil {
		fmt.Printf("    connect() 실패: %v\n", err)
		return
	}

	fmt.Printf("    connect(10.96.0.1:80) -> 소켓 dst가 %s:%d로 변환됨\n",
		sock.ActualDstIP, sock.ActualDstPort)
	fmt.Println("    send() -> NAT 불필요 (이미 백엔드 주소)")
	fmt.Println("    recv() -> NAT 불필요")

	// getpeername() 시뮬레이션
	peerIP, peerPort := slb.GetPeerName(sock)
	fmt.Printf("    getpeername() -> %s:%d (원래 VIP 반환)\n", peerIP, peerPort)
	fmt.Println("    = connect() 시점에 1회 변환, 이후 NAT/conntrack 제로")

	// 여러 연결 시뮬레이션
	printSubHeader("다중 연결 시뮬레이션 (소켓 수준 LB)")
	for i := 0; i < 6; i++ {
		s := &Socket{Cookie: uint64(10000 + i)}
		slb.Connect(s, vip, vipPort)
		fmt.Printf("  Socket[%d] connect(%s:%d) -> 변환됨: %s:%d\n",
			i, vip, vipPort, s.ActualDstIP, s.ActualDstPort)
	}
}

// demoServiceTypes은 서비스 유형별 처리를 데모한다.
func demoServiceTypes() {
	printHeader("7. 서비스 유형별 BPF 맵 구성")

	services := []Service{
		{
			Name: "nginx-clusterip", VIP: net.ParseIP("10.96.0.1"), Port: 80,
			Type: SVCTypeClusterIP, Algorithm: LBAlgorithmRandom,
			RevNatIndex: 1,
		},
		{
			Name: "nginx-nodeport", VIP: net.ParseIP("0.0.0.0"), Port: 30080,
			Type: SVCTypeNodePort, Algorithm: LBAlgorithmRandom,
			RevNatIndex: 2,
		},
		{
			Name: "nginx-lb", VIP: net.ParseIP("203.0.113.10"), Port: 80,
			Type: SVCTypeLoadBalancer, Algorithm: LBAlgorithmMaglev,
			ForwardingMode: ForwardingModeDSR,
			RevNatIndex: 3,
		},
		{
			Name: "nginx-extip", VIP: net.ParseIP("192.168.1.100"), Port: 80,
			Type: SVCTypeExternalIP, Algorithm: LBAlgorithmRandom,
			RevNatIndex: 4,
		},
	}

	for _, svc := range services {
		fmt.Printf("\n  [%s] %s\n", svc.Type, svc.Name)
		fmt.Printf("    VIP: %s:%d  (RevNatIndex: %d)\n", svc.VIP, svc.Port, svc.RevNatIndex)
		fmt.Printf("    Algorithm: %s\n", svc.Algorithm)

		// 플래그 설명 (Cilium: bpf/lib/lb.h의 SVC_FLAG_* 열거형)
		var flags []string
		switch svc.Type {
		case SVCTypeClusterIP:
			flags = append(flags, "SVC_FLAG_ROUTABLE(내부만)")
		case SVCTypeNodePort:
			flags = append(flags, "SVC_FLAG_NODEPORT", "SVC_FLAG_ROUTABLE")
		case SVCTypeLoadBalancer:
			flags = append(flags, "SVC_FLAG_LOADBALANCER", "SVC_FLAG_ROUTABLE")
			if svc.ForwardingMode == ForwardingModeDSR {
				flags = append(flags, "SVC_FLAG_FWD_MODE_DSR")
			}
		case SVCTypeExternalIP:
			flags = append(flags, "SVC_FLAG_EXTERNAL_IP", "SVC_FLAG_ROUTABLE")
		}
		fmt.Printf("    BPF Flags: %s\n", strings.Join(flags, " | "))

		// Service Map 키 구조 설명
		fmt.Printf("    Service Map Key: {addr=%s, dport=%d, proto=TCP, scope=EXT}\n",
			svc.VIP, svc.Port)
	}
}

// =============================================================================
// 9. 메인 함수
// =============================================================================

func main() {
	fmt.Println("========================================================================")
	fmt.Println("  Cilium 로드 밸런싱 서브시스템 PoC")
	fmt.Println("  (pkg/maglev/, pkg/loadbalancer/, bpf/lib/lb.h, bpf/bpf_sock.c 기반)")
	fmt.Println("========================================================================")

	// 1. Maglev 일관 해싱
	demoMaglev()

	// 2. Maglev 최소 혼란
	demoMaglevMinimalDisruption()

	// 3. Random LB
	demoRandom()

	// 4. Session Affinity
	demoSessionAffinity()

	// 5. DSR vs SNAT
	demoDSR()

	// 6. Socket-level LB
	demoSocketLB()

	// 7. 서비스 유형
	demoServiceTypes()

	fmt.Println()
	fmt.Println("========================================================================")
	fmt.Println("  PoC 완료")
	fmt.Println("========================================================================")
}
