package main

import (
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Cilium Maglev 로드밸런싱 시뮬레이션
// =============================================================================
//
// 실제 소스 참조:
//   - pkg/maglev/maglev.go         : Maglev 해싱 알고리즘 (getOffsetAndSkip, computeLookupTable)
//   - pkg/loadbalancer/loadbalancer.go : SVCType, ServiceFlags, BackendState, SessionAffinity
//   - pkg/loadbalancer/frontend.go  : Frontend 구조체, FrontendParams
//   - pkg/loadbalancer/backend.go   : Backend 구조체, BackendParams
//   - pkg/loadbalancer/service.go   : Service 구조체, SessionAffinityTimeout
//
// Maglev 알고리즘 핵심 (Google Maglev 논문 기반):
//   1. 각 백엔드에 대해 offset = h1(backend) % M, skip = (h2(backend) % (M-1)) + 1 계산
//   2. 순열 테이블 생성: perm[i][j] = (offset + j*skip) % M
//   3. 라운드 로빈으로 각 백엔드가 차례대로 lookup table의 빈 슬롯을 채움
//   4. 결과: 크기 M의 lookup table에 백엔드 ID가 균등 분포
//   5. 백엔드 추가/제거 시 최소한의 슬롯만 재분배됨 (일관된 해싱)

// =============================================================================
// 1. 데이터 모델 — Cilium loadbalancer 패키지 재현
// =============================================================================

// BackendID는 백엔드의 고유 식별자
// 실제: pkg/loadbalancer/loadbalancer.go의 BackendID (type BackendID uint32)
type BackendID uint32

// BackendState는 백엔드의 상태
// 실제: pkg/loadbalancer/loadbalancer.go의 BackendState
// Active → Terminating, Quarantined, Maintenance 전이 가능
type BackendState uint8

const (
	BackendStateActive      BackendState = iota // 정상 트래픽 수신 (기본값)
	BackendStateTerminating                     // 종료 중 (폴백 용도로 사용 가능)
	BackendStateQuarantined                     // 격리 (health check 실패)
	BackendStateMaintenance                     // 유지보수 중
)

func (s BackendState) String() string {
	switch s {
	case BackendStateActive:
		return "active"
	case BackendStateTerminating:
		return "terminating"
	case BackendStateQuarantined:
		return "quarantined"
	case BackendStateMaintenance:
		return "maintenance"
	default:
		return "unknown"
	}
}

// Backend은 실제 백엔드 서버를 나타냄
// 실제: pkg/loadbalancer/backend.go의 BackendParams
type Backend struct {
	ID       BackendID
	Address  string       // IP:Port
	Weight   uint16       // 가중치 (기본값 100, 실제: DefaultBackendWeight = 100)
	State    BackendState // 상태
	NodeName string       // 호스팅 노드 이름
}

// hashString은 Maglev 해싱에 사용되는 문자열 표현
// 실제: pkg/maglev/maglev.go의 BackendInfo.setHashString()
// 형식: [IP:Port/Protocol,State:active]
// 이 형식은 안정적이어야 함 — 변경하면 다른 노드와 lookup table이 달라짐
func (b *Backend) hashString() string {
	return fmt.Sprintf("[%s/TCP,State:active]", b.Address)
}

// Frontend는 서비스의 프론트엔드 주소 (VIP)
// 실제: pkg/loadbalancer/frontend.go의 Frontend, FrontendParams
type Frontend struct {
	Address     string // VIP:Port
	ServiceName string
	ServiceType string // ClusterIP, NodePort, LoadBalancer, HostPort 등
}

// Service는 로드밸런싱 서비스 정의
// 실제: pkg/loadbalancer/service.go의 Service
type Service struct {
	Name                   string
	Frontend               Frontend
	Backends               []*Backend
	SessionAffinity        bool          // 세션 어피니티 활성화 여부
	SessionAffinityTimeout time.Duration // 세션 어피니티 타임아웃
	Algorithm              string        // "maglev" 또는 "random"
}

// =============================================================================
// 2. Maglev 해싱 알고리즘 — pkg/maglev/maglev.go 재현
// =============================================================================

// MaglevConfig는 Maglev 설정
// 실제: pkg/maglev/maglev.go의 Config 구조체
//   - TableSize: 반드시 소수 (지원값: 251, 509, 1021, 2039, 4093, 8191, 16381, ...)
//   - SeedMurmur: murmur3 해시 시드 (base64 인코딩된 12바이트에서 파생)
type MaglevConfig struct {
	TableSize uint   // 소수여야 함 (M)
	HashSeed  uint32 // 클러스터 전역 시드
}

// 기본 설정 — 시뮬레이션 용도로 작은 크기 사용
// 실제 기본값: DefaultTableSize = 16381, DefaultHashSeed = "JLfvgnHc2kaSUFaI"
var DefaultMaglevConfig = MaglevConfig{
	TableSize: 251,
	HashSeed:  42,
}

// Maglev는 Maglev 룩업 테이블 계산기
// 실제: pkg/maglev/maglev.go의 Maglev 구조체
// 실제에서는 workerpool을 사용해 permutation 계산을 병렬화함
type Maglev struct {
	config       MaglevConfig
	mu           sync.Mutex
	permutations []uint64
}

// NewMaglev는 새 Maglev 인스턴스를 생성
func NewMaglev(config MaglevConfig) *Maglev {
	return &Maglev{config: config}
}

// getOffsetAndSkip은 백엔드의 offset과 skip 값을 계산
// 실제: pkg/maglev/maglev.go의 getOffsetAndSkip()
//
//	h1, h2 := murmur3.Hash128(addr, seed)
//	offset := h1 % m
//	skip := (h2 % (m - 1)) + 1
//
// offset: 순열의 시작점, skip: 순열의 간격 (0이 되면 안 되므로 +1)
// 이 두 값으로 각 백엔드마다 고유한 M 크기의 순열을 생성
func getOffsetAndSkip(key []byte, m uint64, seed uint32) (uint64, uint64) {
	// h1 시뮬레이션 (실제: murmur3 h1)
	h := fnv.New64a()
	seedBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(seedBytes, seed)
	h.Write(seedBytes)
	h.Write(key)
	h1 := h.Sum64()

	// h2 시뮬레이션 (실제: murmur3 h2)
	md5Hash := md5.Sum(append(seedBytes, key...))
	h2 := binary.LittleEndian.Uint64(md5Hash[:8])

	offset := h1 % m
	skip := (h2 % (m - 1)) + 1
	return offset, skip
}

// GetLookupTable은 주어진 백엔드들에 대한 Maglev 룩업 테이블을 반환
// 실제: pkg/maglev/maglev.go의 GetLookupTable() + computeLookupTable()
//
// 알고리즘 흐름:
//   1. 백엔드를 hashString 기준 정렬 (클러스터 전체에서 동일 순서 보장)
//   2. 각 백엔드의 permutation 계산 (offset+skip 기반)
//   3. 라운드 로빈으로 순열을 순회하며 lookup table의 빈 슬롯 채움
//   4. 가중치 반영: 높은 가중치의 백엔드가 더 자주 차례를 얻음
func (ml *Maglev) GetLookupTable(backends []*Backend) []BackendID {
	ml.mu.Lock()
	defer ml.mu.Unlock()

	if len(backends) == 0 {
		return nil
	}

	m := ml.config.TableSize

	// 백엔드 정보를 hashString 기준으로 정렬
	// 실제: slices.SortFunc(ml.backendInfosBuffer, func(a, b BackendInfo) int { ... })
	// 이는 모든 노드에서 백엔드 ID가 다르더라도 동일한 순서를 보장하기 위함
	type backendInfo struct {
		backend    *Backend
		hashString string
	}
	infos := make([]backendInfo, len(backends))
	for i, b := range backends {
		infos[i] = backendInfo{backend: b, hashString: b.hashString()}
	}
	sort.Slice(infos, func(i, j int) bool {
		return infos[i].hashString < infos[j].hashString
	})

	l := len(infos)

	// 가중치 계산
	// 실제: computeLookupTable()에서 weightSum, weightCntr 계산
	weightSum := uint64(0)
	weightCntr := make([]float64, l)
	for i, info := range infos {
		w := uint64(info.backend.Weight)
		if w == 0 {
			w = 100
		}
		weightSum += w
		weightCntr[i] = float64(w) / float64(l)
	}
	weightsUsed := weightSum/uint64(l) > 1

	// 순열(permutation) 테이블 계산
	// 실제: getPermutation() — workerpool으로 병렬 계산
	// perm[i*m + j] = (offset + j*skip) % M
	perm := make([]uint64, l*int(m))
	for i, info := range infos {
		offset, skip := getOffsetAndSkip([]byte(info.hashString), uint64(m), ml.config.HashSeed)
		start := i * int(m)
		perm[start] = offset
		for j := 1; j < int(m); j++ {
			perm[start+j] = (perm[start+(j-1)] + skip) % uint64(m)
		}
	}

	// 룩업 테이블 생성 — 핵심 Maglev 알고리즘
	// 실제: computeLookupTable()의 for n := range m { ... } 루프
	const sentinel = BackendID(0xFFFFFFFF)
	next := make([]int, l)
	entry := make([]BackendID, m)
	for j := range entry {
		entry[j] = sentinel
	}

	for n := uint(0); n < m; n++ {
		i := int(n) % l
		for {
			info := infos[i]
			w := uint64(info.backend.Weight)
			if w == 0 {
				w = 100
			}

			// 가중치 기반 턴 선택
			// 실제: weightsUsed 분기에서 ((n+1)*weight) < weightCntr[i] 확인
			// 가중치가 낮은 백엔드는 턴을 건너뛰어 할당 빈도를 줄임
			if weightsUsed {
				if (uint64(n+1) * w) < uint64(weightCntr[i]) {
					i = (i + 1) % l
					continue
				}
				weightCntr[i] += float64(weightSum)
			}

			// 순열에서 빈 슬롯 찾기
			c := perm[i*int(m)+next[i]]
			for entry[c] != sentinel {
				next[i]++
				c = perm[i*int(m)+next[i]]
			}
			entry[c] = info.backend.ID
			next[i]++
			break
		}
	}

	return entry
}

// =============================================================================
// 3. 세션 어피니티 — BPF LRU Map 시뮬레이션
// =============================================================================
//
// 실제: BPF 맵 lb4_affinity_map / lb6_affinity_map (LRU hash map)
// 키: (clientIP, serviceAddr), 값: (backendID, timestamp)
// Service.SessionAffinityTimeout으로 만료 관리

// SessionAffinityEntry는 세션 어피니티 항목
type SessionAffinityEntry struct {
	BackendID BackendID
	CreatedAt time.Time
}

// SessionAffinityMap은 클라이언트 IP별 세션 어피니티 매핑
// 실제: BPF LRU hash map으로 구현
type SessionAffinityMap struct {
	mu      sync.RWMutex
	entries map[string]*SessionAffinityEntry // key: clientIP|serviceAddr
	timeout time.Duration
	maxSize int
}

func NewSessionAffinityMap(timeout time.Duration, maxSize int) *SessionAffinityMap {
	return &SessionAffinityMap{
		entries: make(map[string]*SessionAffinityEntry),
		timeout: timeout,
		maxSize: maxSize,
	}
}

func affinityKey(clientIP, serviceAddr string) string {
	return clientIP + "|" + serviceAddr
}

// Lookup은 세션 어피니티 항목을 검색 (타임아웃 확인 포함)
func (m *SessionAffinityMap) Lookup(clientIP, serviceAddr string) (BackendID, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	entry, exists := m.entries[affinityKey(clientIP, serviceAddr)]
	if !exists || time.Since(entry.CreatedAt) > m.timeout {
		return 0, false
	}
	return entry.BackendID, true
}

// Update는 세션 어피니티 항목을 업데이트 (LRU eviction 포함)
func (m *SessionAffinityMap) Update(clientIP, serviceAddr string, backendID BackendID) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.entries) >= m.maxSize {
		// LRU: 만료된 항목 먼저 제거
		now := time.Now()
		for key, entry := range m.entries {
			if now.Sub(entry.CreatedAt) > m.timeout {
				delete(m.entries, key)
			}
		}
	}

	m.entries[affinityKey(clientIP, serviceAddr)] = &SessionAffinityEntry{
		BackendID: backendID,
		CreatedAt: time.Now(),
	}
}

func (m *SessionAffinityMap) Size() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.entries)
}

// =============================================================================
// 4. 로드밸런서 — Frontend → Backend 매핑
// =============================================================================

// LoadBalancer는 서비스별 로드밸런싱을 관리
// 실제 동작 흐름 (BPF datapath):
//   1. tc ingress → lb4_lookup_service() → Frontend 찾기
//   2. SessionAffinity 확인 → lb4_affinity_lookup()
//   3. Maglev lookup table로 백엔드 선택 → svc_lookup_maglev()
//   4. CT(Connection Tracking) 업데이트
type LoadBalancer struct {
	maglev   *Maglev
	affinity *SessionAffinityMap
	services map[string]*Service // key: frontend address
	mu       sync.RWMutex
}

func NewLoadBalancer(config MaglevConfig) *LoadBalancer {
	return &LoadBalancer{
		maglev:   NewMaglev(config),
		affinity: NewSessionAffinityMap(30*time.Second, 10000),
		services: make(map[string]*Service),
	}
}

func (lb *LoadBalancer) RegisterService(svc *Service) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	lb.services[svc.Frontend.Address] = svc
}

// SelectBackend는 패킷에 대한 백엔드를 선택
// 실제 BPF 흐름:
//   1. Session Affinity 확인 → 기존 매핑 존재 시 해당 백엔드 반환
//   2. Maglev lookup table에서 flowHash % M으로 인덱싱
//   3. Session Affinity 업데이트
func (lb *LoadBalancer) SelectBackend(clientIP, frontendAddr string, flowHash uint32) *Backend {
	lb.mu.RLock()
	svc, exists := lb.services[frontendAddr]
	lb.mu.RUnlock()
	if !exists || len(svc.Backends) == 0 {
		return nil
	}

	// Active 상태 백엔드 필터링
	// 실제: BackendStateActive만 정상 선택, Terminating은 폴백
	activeBackends := make([]*Backend, 0)
	for _, b := range svc.Backends {
		if b.State == BackendStateActive {
			activeBackends = append(activeBackends, b)
		}
	}
	if len(activeBackends) == 0 {
		// 폴백: Terminating 백엔드 사용
		// 실제: serviceFlagSessionAffinity 플래그로 Terminating 백엔드를 폴백으로 허용
		for _, b := range svc.Backends {
			if b.State == BackendStateTerminating {
				activeBackends = append(activeBackends, b)
			}
		}
	}
	if len(activeBackends) == 0 {
		return nil
	}

	// 1. 세션 어피니티 확인
	if svc.SessionAffinity {
		if backendID, found := lb.affinity.Lookup(clientIP, frontendAddr); found {
			for _, b := range activeBackends {
				if b.ID == backendID {
					return b
				}
			}
		}
	}

	// 2. Maglev 룩업 테이블로 선택
	lookupTable := lb.maglev.GetLookupTable(activeBackends)
	if len(lookupTable) == 0 {
		return nil
	}
	idx := flowHash % uint32(len(lookupTable))
	selectedID := lookupTable[idx]

	var selected *Backend
	for _, b := range activeBackends {
		if b.ID == selectedID {
			selected = b
			break
		}
	}

	// 3. 세션 어피니티 업데이트
	if selected != nil && svc.SessionAffinity {
		lb.affinity.Update(clientIP, frontendAddr, selected.ID)
	}

	return selected
}

// =============================================================================
// 5. 데모 함수
// =============================================================================

func hashFlow(srcIP string, srcPort, dstPort uint16) uint32 {
	h := fnv.New32a()
	fmt.Fprintf(h, "%s:%d->%d", srcIP, srcPort, dstPort)
	return h.Sum32()
}

func printDistribution(table []BackendID, backends []*Backend) {
	counts := make(map[BackendID]int)
	for _, id := range table {
		counts[id]++
	}
	total := len(table)
	fmt.Printf("  룩업 테이블 크기: %d\n", total)
	fmt.Printf("  백엔드 분포:\n")
	for _, b := range backends {
		count := counts[b.ID]
		pct := float64(count) / float64(total) * 100
		bar := strings.Repeat("█", int(pct/2))
		fmt.Printf("    %-20s (weight=%3d): %4d (%5.1f%%) %s\n",
			b.Address, b.Weight, count, pct, bar)
	}
}

func calculateDisruption(oldTable, newTable []BackendID) float64 {
	if len(oldTable) != len(newTable) {
		return 1.0
	}
	changed := 0
	for i := range oldTable {
		if oldTable[i] != newTable[i] {
			changed++
		}
	}
	return float64(changed) / float64(len(oldTable))
}

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║  Cilium Maglev 로드밸런싱 시뮬레이션                        ║")
	fmt.Println("║  소스: pkg/maglev/maglev.go, pkg/loadbalancer/             ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()

	// =========================================================================
	// 데모 1: Maglev 해싱 기본 동작
	// =========================================================================
	fmt.Println("━━━ 데모 1: Maglev 해싱 기본 동작 ━━━")
	fmt.Println()

	maglev := NewMaglev(DefaultMaglevConfig)
	backends := []*Backend{
		{ID: 1, Address: "10.0.1.1:80", Weight: 100, State: BackendStateActive},
		{ID: 2, Address: "10.0.1.2:80", Weight: 100, State: BackendStateActive},
		{ID: 3, Address: "10.0.1.3:80", Weight: 100, State: BackendStateActive},
	}

	fmt.Println("[초기 상태] 3개 백엔드 (동일 가중치 100)")
	table := maglev.GetLookupTable(backends)
	printDistribution(table, backends)
	ideal := float64(DefaultMaglevConfig.TableSize) / float64(len(backends))
	fmt.Printf("\n  이상적 분배: %.1f 슬롯/백엔드\n", ideal)
	fmt.Println()

	// =========================================================================
	// 데모 2: 백엔드 추가/제거 시 최소 재분배
	// =========================================================================
	fmt.Println("━━━ 데모 2: 백엔드 추가/제거 시 최소 재분배 (일관된 해싱) ━━━")
	fmt.Println()

	backends4 := make([]*Backend, 4)
	copy(backends4, backends)
	backends4[3] = &Backend{ID: 4, Address: "10.0.1.4:80", Weight: 100, State: BackendStateActive}
	table4 := maglev.GetLookupTable(backends4)

	disruption := calculateDisruption(table, table4)
	idealD := 1.0 / float64(len(backends4))
	fmt.Printf("[백엔드 추가] 3→4개: 변경 비율 = %.1f%% (이상적: %.1f%%)\n",
		disruption*100, idealD*100)
	printDistribution(table4, backends4)
	fmt.Println()

	backends2 := backends[:2]
	table2 := maglev.GetLookupTable(backends2)
	disruption2 := calculateDisruption(table, table2)
	fmt.Printf("[백엔드 제거] 3→2개: 변경 비율 = %.1f%% (이상적: 33.3%%)\n", disruption2*100)
	printDistribution(table2, backends2)
	fmt.Println()

	// =========================================================================
	// 데모 3: 가중치 기반 분배
	// =========================================================================
	fmt.Println("━━━ 데모 3: 가중치 기반 분배 ━━━")
	fmt.Println()

	weightedBackends := []*Backend{
		{ID: 10, Address: "10.0.2.1:80", Weight: 200, State: BackendStateActive},
		{ID: 11, Address: "10.0.2.2:80", Weight: 100, State: BackendStateActive},
		{ID: 12, Address: "10.0.2.3:80", Weight: 50, State: BackendStateActive},
	}

	fmt.Println("  가중치 설정: [200, 100, 50]")
	weightedTable := maglev.GetLookupTable(weightedBackends)
	printDistribution(weightedTable, weightedBackends)
	totalWeight := 350.0
	fmt.Printf("\n  예상 비율: %.0f%% / %.0f%% / %.0f%%\n",
		200/totalWeight*100, 100/totalWeight*100, 50/totalWeight*100)
	fmt.Println()

	// =========================================================================
	// 데모 4: 세션 어피니티
	// =========================================================================
	fmt.Println("━━━ 데모 4: 세션 어피니티 (Client IP 기반) ━━━")
	fmt.Println()

	lb := NewLoadBalancer(DefaultMaglevConfig)
	svc := &Service{
		Name: "default/web-service",
		Frontend: Frontend{
			Address:     "10.96.0.1:80",
			ServiceName: "web-service",
			ServiceType: "ClusterIP",
		},
		Backends: []*Backend{
			{ID: 20, Address: "10.0.3.1:80", Weight: 100, State: BackendStateActive},
			{ID: 21, Address: "10.0.3.2:80", Weight: 100, State: BackendStateActive},
			{ID: 22, Address: "10.0.3.3:80", Weight: 100, State: BackendStateActive},
		},
		SessionAffinity:        true,
		SessionAffinityTimeout: 30 * time.Second,
	}
	lb.RegisterService(svc)

	fmt.Println("  SessionAffinity: 활성화 (timeout=30s)")
	fmt.Println()

	// 동일 클라이언트의 연속 요청 → 같은 백엔드
	clientIP := "192.168.1.100"
	fmt.Printf("  클라이언트 %s의 연속 요청:\n", clientIP)
	var firstBackend *Backend
	for i := 0; i < 5; i++ {
		flowH := hashFlow(clientIP, uint16(30000+i), 80)
		selected := lb.SelectBackend(clientIP, "10.96.0.1:80", flowH)
		if i == 0 {
			firstBackend = selected
		}
		sticky := ""
		if selected == firstBackend {
			sticky = " (어피니티 유지)"
		}
		fmt.Printf("    요청 #%d: → %s (ID=%d)%s\n", i+1, selected.Address, selected.ID, sticky)
	}

	fmt.Println()
	otherClients := []string{"192.168.1.101", "192.168.1.102", "192.168.1.103"}
	fmt.Println("  다른 클라이언트들의 첫 요청:")
	for _, client := range otherClients {
		flowH := hashFlow(client, 40000, 80)
		selected := lb.SelectBackend(client, "10.96.0.1:80", flowH)
		fmt.Printf("    %s → %s (ID=%d)\n", client, selected.Address, selected.ID)
	}
	fmt.Println()

	// =========================================================================
	// 데모 5: 백엔드 상태 변경과 Graceful 제거
	// =========================================================================
	fmt.Println("━━━ 데모 5: 백엔드 상태 변경과 Graceful 제거 ━━━")
	fmt.Println()

	fmt.Println("  [초기] 모든 백엔드 Active")
	for _, b := range svc.Backends {
		fmt.Printf("    %s: %s\n", b.Address, b.State)
	}

	svc.Backends[1].State = BackendStateTerminating
	fmt.Println("\n  [변경] 10.0.3.2:80 → Terminating")

	distribution := make(map[string]int)
	for i := 0; i < 100; i++ {
		client := fmt.Sprintf("10.1.1.%d", i)
		flowH := hashFlow(client, 50000, 80)
		selected := lb.SelectBackend(client, "10.96.0.1:80", flowH)
		if selected != nil {
			distribution[selected.Address]++
		}
	}
	fmt.Println("  새 클라이언트 요청 분배 (Active만 선택):")
	for addr, count := range distribution {
		fmt.Printf("    %s: %d건\n", addr, count)
	}

	// 모두 Terminating → 폴백 동작
	svc.Backends[0].State = BackendStateTerminating
	svc.Backends[2].State = BackendStateTerminating
	fmt.Println("\n  [모두 Terminating] 폴백 동작:")
	selected := lb.SelectBackend("10.2.0.1", "10.96.0.1:80", hashFlow("10.2.0.1", 60000, 80))
	if selected != nil {
		fmt.Printf("    10.2.0.1 → %s (%s) — Terminating 폴백\n", selected.Address, selected.State)
	}
	fmt.Println()

	// =========================================================================
	// 데모 6: 클러스터 전체 해시 일관성
	// =========================================================================
	fmt.Println("━━━ 데모 6: 클러스터 전체 해시 일관성 검증 ━━━")
	fmt.Println()

	// hashString 기준 정렬이므로 입력 순서와 무관
	stable := []*Backend{
		{ID: 30, Address: "10.0.4.1:80", Weight: 100, State: BackendStateActive},
		{ID: 31, Address: "10.0.4.2:80", Weight: 100, State: BackendStateActive},
		{ID: 32, Address: "10.0.4.3:80", Weight: 100, State: BackendStateActive},
	}
	reversed := []*Backend{
		{ID: 32, Address: "10.0.4.3:80", Weight: 100, State: BackendStateActive},
		{ID: 31, Address: "10.0.4.2:80", Weight: 100, State: BackendStateActive},
		{ID: 30, Address: "10.0.4.1:80", Weight: 100, State: BackendStateActive},
	}

	t1 := maglev.GetLookupTable(stable)
	t2 := maglev.GetLookupTable(reversed)
	identical := true
	for i := range t1 {
		if t1[i] != t2[i] {
			identical = false
			break
		}
	}
	fmt.Printf("  순서 [1,2,3]과 [3,2,1]의 룩업 테이블 동일: %v\n", identical)
	fmt.Println("  → hashString 기준 정렬로 입력 순서와 무관하게 동일 결과 보장")
	fmt.Println()

	// =========================================================================
	// 데모 7: 재분배 비율 요약
	// =========================================================================
	fmt.Println("━━━ 데모 7: Maglev 재분배 비율 요약 ━━━")
	fmt.Println()

	fmt.Println("  변경 유형       | 영향 슬롯      | 이상적     | 실제")
	fmt.Println("  ─────────────────────────────────────────────────")

	base := make([]*Backend, 5)
	for i := range base {
		base[i] = &Backend{
			ID:      BackendID(100 + i),
			Address: fmt.Sprintf("10.0.5.%d:80", i+1),
			Weight:  100, State: BackendStateActive,
		}
	}
	bt := maglev.GetLookupTable(base)

	addBe := make([]*Backend, 6)
	copy(addBe, base)
	addBe[5] = &Backend{ID: 106, Address: "10.0.5.6:80", Weight: 100, State: BackendStateActive}
	at := maglev.GetLookupTable(addBe)
	d := calculateDisruption(bt, at)
	fmt.Printf("  5→6 (추가)     | %3d/%-3d        | %5.1f%%     | %5.1f%%\n",
		int(d*float64(DefaultMaglevConfig.TableSize)), DefaultMaglevConfig.TableSize, 100.0/6, d*100)

	rt := maglev.GetLookupTable(base[:4])
	d = calculateDisruption(bt, rt)
	fmt.Printf("  5→4 (제거)     | %3d/%-3d        | %5.1f%%     | %5.1f%%\n",
		int(d*float64(DefaultMaglevConfig.TableSize)), DefaultMaglevConfig.TableSize, 20.0, d*100)

	rt2 := maglev.GetLookupTable(base[:3])
	d = calculateDisruption(bt, rt2)
	fmt.Printf("  5→3 (제거2)    | %3d/%-3d        | %5.1f%%     | %5.1f%%\n",
		int(d*float64(DefaultMaglevConfig.TableSize)), DefaultMaglevConfig.TableSize, 40.0, d*100)

	// 표준편차
	countsBase := make(map[BackendID]int)
	for _, id := range bt {
		countsBase[id]++
	}
	idealCount := float64(DefaultMaglevConfig.TableSize) / float64(len(base))
	variance := 0.0
	for _, c := range countsBase {
		diff := float64(c) - idealCount
		variance += diff * diff
	}
	stddev := math.Sqrt(variance / float64(len(countsBase)))
	fmt.Printf("\n  5개 백엔드 분포 표준편차: %.2f (낮을수록 균등)\n", stddev)
	fmt.Println()

	fmt.Println("═══════════════════════════════════════════════════════════════")
	fmt.Println("  핵심 포인트:")
	fmt.Println("  1. Maglev는 O(M*N) 시간에 룩업 테이블 계산")
	fmt.Println("  2. 패킷당 백엔드 선택은 O(1) — 단순 배열 인덱싱")
	fmt.Println("  3. 백엔드 변경 시 이상적으로 1/N만 재분배")
	fmt.Println("  4. hashString 정렬로 모든 노드에서 동일 테이블 보장")
	fmt.Println("  5. Session Affinity는 BPF LRU map으로 타임아웃 관리")
	fmt.Println("═══════════════════════════════════════════════════════════════")
}
