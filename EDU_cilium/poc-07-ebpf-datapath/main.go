// Cilium eBPF 데이터패스 시뮬레이터
//
// eBPF 데이터패스의 핵심 메커니즘을 순수 Go로 재현:
//   1. BPF 맵 유형 (Hash, LRU Hash, Array, LPM Trie)
//   2. Tail Call 체인 (entry → LB → CT → Policy → NAT → Forward/Drop)
//   3. BPF 검증기 개념 (명령어 한도, bounded loop)
//   4. 프로그램 부착 시뮬레이션
//
// Cilium 실제 코드 참조:
//   bpf/bpf_lxc.c           — Pod 데이터패스 진입점
//   bpf/lib/tailcall.h      — tail call 인프라 (CILIUM_CALL_* 인덱스)
//   bpf/lib/conntrack.h     — 연결 추적
//   bpf/lib/conntrack_map.h — CT 맵 정의 (LRU Hash)
//   bpf/lib/policy.h        — 정책 검사 (LPM Trie)
//   bpf/lib/nat.h           — NAT 처리
//   bpf/lib/lb.h            — 로드밸런싱
//   bpf/lib/local_delivery.h — tail_call_policy()
//   pkg/datapath/loader/    — 컴파일 + 로딩
//
// 실행: go run main.go
package main

import (
	"fmt"
	"net"
	"strings"
	"time"
)

// ============================================================
// 1. BPF 맵 유형 시뮬레이션
// ============================================================

// --- Hash Map: O(1) lookup ---
// 실제: bpf/lib/lb.h의 cilium_lb4_services_v2
type HashMapEntry[K comparable, V any] struct {
	key   K
	value V
}

type HashMap[K comparable, V any] struct {
	name    string
	entries map[K]V
	maxSize int
}

func newHashMap[K comparable, V any](name string, maxSize int) *HashMap[K, V] {
	return &HashMap[K, V]{
		name:    name,
		entries: make(map[K]V),
		maxSize: maxSize,
	}
}

func (m *HashMap[K, V]) Update(key K, val V) bool {
	if len(m.entries) >= m.maxSize {
		return false // 맵 용량 초과
	}
	m.entries[key] = val
	return true
}

func (m *HashMap[K, V]) Lookup(key K) (V, bool) {
	v, ok := m.entries[key]
	return v, ok
}

// --- LRU Hash Map: 용량 초과 시 가장 오래된 엔트리 제거 ---
// 실제: bpf/lib/conntrack_map.h의 cilium_ct4_global
type LRUHashMap[K comparable, V any] struct {
	name    string
	entries map[K]V
	order   []K
	maxSize int
}

func newLRUHashMap[K comparable, V any](name string, maxSize int) *LRUHashMap[K, V] {
	return &LRUHashMap[K, V]{
		name:    name,
		entries: make(map[K]V),
		maxSize: maxSize,
	}
}

func (m *LRUHashMap[K, V]) Lookup(key K) (V, bool) {
	v, ok := m.entries[key]
	if ok {
		m.moveToBack(key)
	}
	return v, ok
}

func (m *LRUHashMap[K, V]) Update(key K, val V) (evictedKey K, evicted bool) {
	if _, exists := m.entries[key]; exists {
		m.entries[key] = val
		m.moveToBack(key)
		return evictedKey, false
	}
	if len(m.entries) >= m.maxSize {
		evictedKey = m.order[0]
		delete(m.entries, evictedKey)
		m.order = m.order[1:]
		evicted = true
	}
	m.entries[key] = val
	m.order = append(m.order, key)
	return evictedKey, evicted
}

func (m *LRUHashMap[K, V]) moveToBack(key K) {
	for i, k := range m.order {
		if k == key {
			m.order = append(m.order[:i], m.order[i+1:]...)
			m.order = append(m.order, key)
			return
		}
	}
}

// --- Array Map: 인덱스 기반 O(1) 접근 ---
// 실제: bpf/lib/tailcall.h의 cilium_calls (BPF_MAP_TYPE_PROG_ARRAY)
type ArrayMap[V any] struct {
	name    string
	entries []V
}

func newArrayMap[V any](name string, size int) *ArrayMap[V] {
	return &ArrayMap[V]{
		name:    name,
		entries: make([]V, size),
	}
}

func (m *ArrayMap[V]) Set(index int, val V) bool {
	if index < 0 || index >= len(m.entries) {
		return false
	}
	m.entries[index] = val
	return true
}

func (m *ArrayMap[V]) Get(index int) (V, bool) {
	if index < 0 || index >= len(m.entries) {
		var zero V
		return zero, false
	}
	return m.entries[index], true
}

// --- LPM Trie: Longest Prefix Match ---
// 실제: bpf/lib/policy.h의 cilium_policy_v2
// 실제: bpf/bpf_xdp.c의 cilium_cidr_v4_dyn
type LPMKey struct {
	PrefixLen uint32
	IP        uint32 // IPv4 주소 (네트워크 바이트 순서)
}

type LPMTrie[V any] struct {
	name    string
	entries []lpmEntry[V]
}

type lpmEntry[V any] struct {
	key   LPMKey
	value V
}

func newLPMTrie[V any](name string) *LPMTrie[V] {
	return &LPMTrie[V]{name: name}
}

func (t *LPMTrie[V]) Insert(prefixLen uint32, ip uint32, val V) {
	mask := uint32(0)
	if prefixLen > 0 {
		mask = ^uint32(0) << (32 - prefixLen)
	}
	t.entries = append(t.entries, lpmEntry[V]{
		key:   LPMKey{PrefixLen: prefixLen, IP: ip & mask},
		value: val,
	})
}

// Lookup: 가장 긴 프리픽스 매치를 반환
func (t *LPMTrie[V]) Lookup(ip uint32) (V, bool) {
	var bestVal V
	bestLen := int32(-1)

	for _, e := range t.entries {
		mask := uint32(0)
		if e.key.PrefixLen > 0 {
			mask = ^uint32(0) << (32 - e.key.PrefixLen)
		}
		if (ip & mask) == e.key.IP {
			if int32(e.key.PrefixLen) > bestLen {
				bestLen = int32(e.key.PrefixLen)
				bestVal = e.value
			}
		}
	}
	return bestVal, bestLen >= 0
}

func ipToUint32(ipStr string) uint32 {
	ip := net.ParseIP(ipStr).To4()
	if ip == nil {
		return 0
	}
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}

func uint32ToIP(ip uint32) string {
	return fmt.Sprintf("%d.%d.%d.%d", ip>>24, (ip>>16)&0xff, (ip>>8)&0xff, ip&0xff)
}

// ============================================================
// 2. 패킷 및 연결 상태 정의
// ============================================================

// 패킷 구조
type Packet struct {
	SrcIP    string
	DstIP    string
	SrcPort  uint16
	DstPort  uint16
	Protocol uint8 // 6=TCP, 17=UDP
	Payload  string
}

func (p Packet) String() string {
	proto := "TCP"
	if p.Protocol == 17 {
		proto = "UDP"
	}
	return fmt.Sprintf("%s:%d -> %s:%d [%s]", p.SrcIP, p.SrcPort, p.DstIP, p.DstPort, proto)
}

// 5-tuple 키 (CT 맵 키)
// 실제: bpf/lib/common.h의 ipv4_ct_tuple
type CTTuple struct {
	SrcIP    string
	DstIP    string
	SrcPort  uint16
	DstPort  uint16
	Protocol uint8
}

// CT 엔트리
// 실제: bpf/lib/conntrack.h의 ct_entry
type CTEntry struct {
	Packets      uint64
	Bytes        uint64
	Lifetime     time.Time
	RevNATIndex  uint16
	SrcSecID     uint32
	RxClosing    bool
	TxClosing    bool
	NodePort     bool
	ProxyRedirect bool
}

// NAT 엔트리
// 실제: bpf/lib/nat.h의 nat_entry
type NATEntry struct {
	OrigSrcIP   string
	OrigSrcPort uint16
	NatSrcIP    string
	NatSrcPort  uint16
}

// 정책 결정
type PolicyVerdict int

const (
	PolicyAllow PolicyVerdict = iota
	PolicyDeny
)

// ============================================================
// 3. Tail Call 체인 시뮬레이션
// ============================================================

// Tail Call 인덱스 (실제: bpf/lib/tailcall.h)
const (
	CILIUM_CALL_IPV4_FROM_LXC      = 7
	CILIUM_CALL_IPV4_FROM_LXC_CONT = 26
	CILIUM_CALL_IPV4_CT_EGRESS     = 30
	CILIUM_CALL_IPV4_TO_ENDPOINT   = 13
	CILIUM_CALL_IPV4_CT_INGRESS    = 28
	CILIUM_CALL_DROP_NOTIFY        = 1
	CILIUM_CALL_SIZE               = 49
)

// BPF 프로그램을 나타내는 함수 타입
type BPFProgFunc func(ctx *ProgContext) int

// 프로그램 실행 컨텍스트 (skb에 해당)
type ProgContext struct {
	Pkt          Packet
	SrcIdentity  uint32
	DstIdentity  uint32
	Metadata     map[string]uint32 // CB_ 슬롯 시뮬레이션
	TraceLog     []string
	TailCallMap  *ArrayMap[BPFProgFunc]
	TailCallDepth int
}

func newContext(pkt Packet, srcID uint32) *ProgContext {
	return &ProgContext{
		Pkt:         pkt,
		SrcIdentity: srcID,
		Metadata:    make(map[string]uint32),
		TraceLog:    nil,
		TailCallDepth: 0,
	}
}

func (ctx *ProgContext) trace(msg string) {
	indent := strings.Repeat("  ", ctx.TailCallDepth)
	ctx.TraceLog = append(ctx.TraceLog, indent+msg)
}

// Tail call 실행 (실제: bpf/lib/tailcall.h의 tail_call_internal)
const maxTailCalls = 33

func (ctx *ProgContext) tailCall(index int) int {
	if ctx.TailCallDepth >= maxTailCalls {
		ctx.trace(fmt.Sprintf("[DROP] tail call 한도 초과 (depth=%d, max=%d)", ctx.TailCallDepth, maxTailCalls))
		return -1 // DROP_MISSED_TAIL_CALL
	}
	prog, ok := ctx.TailCallMap.Get(index)
	if !ok || prog == nil {
		ctx.trace(fmt.Sprintf("[DROP] tail call 실패: 인덱스 %d에 프로그램 없음", index))
		return -1
	}
	ctx.TailCallDepth++
	return prog(ctx)
}

// ============================================================
// 4. BPF 검증기 시뮬레이션
// ============================================================

type BPFVerifier struct {
	maxInstructions int
	maxLoopIters    int
}

func newVerifier() *BPFVerifier {
	return &BPFVerifier{
		maxInstructions: 1000000, // 커널 5.2+ 기준
		maxLoopIters:    8192,    // BPF_COMPLEXITY_LIMIT
	}
}

type VerifyResult struct {
	ProgramName  string
	Instructions int
	Passed       bool
	Reason       string
}

func (v *BPFVerifier) Verify(name string, instructions int, hasUnboundedLoop bool) VerifyResult {
	if instructions > v.maxInstructions {
		return VerifyResult{
			ProgramName:  name,
			Instructions: instructions,
			Passed:       false,
			Reason:       fmt.Sprintf("명령어 수 초과 (%d > %d)", instructions, v.maxInstructions),
		}
	}
	if hasUnboundedLoop {
		return VerifyResult{
			ProgramName:  name,
			Instructions: instructions,
			Passed:       false,
			Reason:       "무한 루프 감지 (bounded loop만 허용)",
		}
	}
	return VerifyResult{
		ProgramName:  name,
		Instructions: instructions,
		Passed:       true,
		Reason:       "검증 통과",
	}
}

// ============================================================
// 5. 데이터패스 시뮬레이터 (전체 조립)
// ============================================================

type DatapathSimulator struct {
	// BPF 맵들
	ctMap      *LRUHashMap[CTTuple, *CTEntry]
	natMap     *HashMap[CTTuple, NATEntry]
	policyMap  *HashMap[uint32, PolicyVerdict] // Identity -> Allow/Deny
	svcMap     *HashMap[string, string]        // VIP:port -> backend:port
	ipcache    *LPMTrie[uint32]                // IP -> Security Identity

	// Tail call 프로그램 배열
	callsMap   *ArrayMap[BPFProgFunc]

	// 통계
	packetsProcessed int
	packetsDropped   int
	packetsForwarded int
}

func newDatapathSimulator() *DatapathSimulator {
	ds := &DatapathSimulator{
		ctMap:     newLRUHashMap[CTTuple, *CTEntry]("cilium_ct4_global", 8),
		natMap:    newHashMap[CTTuple, NATEntry]("cilium_snat_v4_external", 100),
		policyMap: newHashMap[uint32, PolicyVerdict]("cilium_policy_v2", 1000),
		svcMap:    newHashMap[string, string]("cilium_lb4_services_v2", 100),
		ipcache:   newLPMTrie[uint32]("cilium_ipcache"),
		callsMap:  newArrayMap[BPFProgFunc]("cilium_calls", CILIUM_CALL_SIZE),
	}

	// tail call 프로그램 등록 (실제: Endpoint 로딩 시 loader가 수행)
	ds.callsMap.Set(CILIUM_CALL_IPV4_FROM_LXC, ds.tailHandleIPv4)
	ds.callsMap.Set(CILIUM_CALL_IPV4_CT_EGRESS, ds.tailIPv4CTEgress)
	ds.callsMap.Set(CILIUM_CALL_IPV4_FROM_LXC_CONT, ds.tailHandleIPv4Cont)
	ds.callsMap.Set(CILIUM_CALL_DROP_NOTIFY, ds.tailDropNotify)

	return ds
}

// --- 진입점: cil_from_container ---
// 실제: bpf/bpf_lxc.c의 cil_from_container()
func (ds *DatapathSimulator) cilFromContainer(ctx *ProgContext) int {
	ctx.trace("[cil_from_container] 패킷 수신: " + ctx.Pkt.String())
	ctx.trace(fmt.Sprintf("[cil_from_container] Security Identity: %d", ctx.SrcIdentity))
	ds.packetsProcessed++

	ctx.TailCallMap = ds.callsMap

	// 프로토콜에 따라 tail call (실제 코드: switch(proto))
	if ctx.Pkt.Protocol == 6 || ctx.Pkt.Protocol == 17 {
		ctx.trace("[cil_from_container] -> tail_call(CILIUM_CALL_IPV4_FROM_LXC)")
		return ctx.tailCall(CILIUM_CALL_IPV4_FROM_LXC)
	}

	ctx.trace("[DROP] 지원하지 않는 프로토콜")
	ds.packetsDropped++
	return -1
}

// --- tail call 1: IPv4 처리 시작 ---
// 실제: bpf/bpf_lxc.c의 tail_handle_ipv4() -> __tail_handle_ipv4()
func (ds *DatapathSimulator) tailHandleIPv4(ctx *ProgContext) int {
	ctx.trace("[tail_handle_ipv4] IPv4 패킷 처리 시작")

	// 1. 패킷 유효성 검사
	if ctx.Pkt.SrcIP == "" || ctx.Pkt.DstIP == "" {
		ctx.trace("[DROP] 유효하지 않은 IP")
		ds.packetsDropped++
		return -1
	}

	// 2. 서비스 LB (Per-packet LB)
	// 실제: __per_packet_lb_svc_xlate_4() -> lb4_lookup_service()
	svcKey := fmt.Sprintf("%s:%d", ctx.Pkt.DstIP, ctx.Pkt.DstPort)
	if backend, ok := ds.svcMap.Lookup(svcKey); ok {
		ctx.trace(fmt.Sprintf("[LB] 서비스 발견: %s -> Backend %s", svcKey, backend))
		// DNAT: 목적지를 Backend으로 변경
		origDst := ctx.Pkt.DstIP
		origPort := ctx.Pkt.DstPort
		// 간단히 "IP:Port" 파싱
		parts := strings.Split(backend, ":")
		ctx.Pkt.DstIP = parts[0]
		fmt.Sscanf(parts[1], "%d", &ctx.Pkt.DstPort)
		ctx.trace(fmt.Sprintf("[LB] DNAT 적용: %s:%d -> %s:%d", origDst, origPort, ctx.Pkt.DstIP, ctx.Pkt.DstPort))

		// RevNAT 인덱스 저장 (tail call 간 상태 전달)
		ctx.Metadata["CB_CT_STATE"] = 1 // rev_nat_index
	}

	// 3. CT egress로 tail call
	// 실제: tail_call_internal(ctx, CILIUM_CALL_IPV4_CT_EGRESS, &ext_err)
	ctx.trace("[tail_handle_ipv4] -> tail_call(CILIUM_CALL_IPV4_CT_EGRESS)")
	return ctx.tailCall(CILIUM_CALL_IPV4_CT_EGRESS)
}

// --- tail call 2: CT Egress 조회 ---
// 실제: TAIL_CT_LOOKUP4 매크로로 생성된 tail_ipv4_ct_egress()
func (ds *DatapathSimulator) tailIPv4CTEgress(ctx *ProgContext) int {
	ctx.trace("[tail_ipv4_ct_egress] 연결 추적(CT) 조회")

	tuple := CTTuple{
		SrcIP:    ctx.Pkt.SrcIP,
		DstIP:    ctx.Pkt.DstIP,
		SrcPort:  ctx.Pkt.SrcPort,
		DstPort:  ctx.Pkt.DstPort,
		Protocol: ctx.Pkt.Protocol,
	}

	// CT 조회
	entry, found := ds.ctMap.Lookup(tuple)
	if found {
		// 기존 연결: 카운터 업데이트
		entry.Packets++
		entry.Lifetime = time.Now().Add(5 * time.Minute)
		ctx.trace(fmt.Sprintf("[CT] 기존 연결 발견 (packets=%d, identity=%d)", entry.Packets, entry.SrcSecID))
	} else {
		// 새 연결: CT 엔트리 생성
		newEntry := &CTEntry{
			Packets:  1,
			Lifetime: time.Now().Add(5 * time.Minute),
			SrcSecID: ctx.SrcIdentity,
		}
		evKey, evicted := ds.ctMap.Update(tuple, newEntry)
		if evicted {
			ctx.trace(fmt.Sprintf("[CT] LRU 제거: %s:%d -> %s:%d", evKey.SrcIP, evKey.SrcPort, evKey.DstIP, evKey.DstPort))
		}
		ctx.trace(fmt.Sprintf("[CT] 새 연결 생성 (identity=%d)", ctx.SrcIdentity))
	}

	// 다음 단계로 tail call
	// 실제: CILIUM_CALL_IPV4_FROM_LXC_CONT -> tail_handle_ipv4_cont
	ctx.trace("[tail_ipv4_ct_egress] -> tail_call(CILIUM_CALL_IPV4_FROM_LXC_CONT)")
	return ctx.tailCall(CILIUM_CALL_IPV4_FROM_LXC_CONT)
}

// --- tail call 3: 정책 검사 + NAT + 포워딩 ---
// 실제: bpf/bpf_lxc.c의 tail_handle_ipv4_cont() -> handle_ipv4_from_lxc()
func (ds *DatapathSimulator) tailHandleIPv4Cont(ctx *ProgContext) int {
	ctx.trace("[tail_handle_ipv4_cont] 정책 검사 + 라우팅")

	// 1. Identity 조회 (IPCache)
	// 실제: bpf/lib/identity.h의 lookup_ip4_remote_identity()
	dstIP := ipToUint32(ctx.Pkt.DstIP)
	if dstIdentity, ok := ds.ipcache.Lookup(dstIP); ok {
		ctx.DstIdentity = dstIdentity
		ctx.trace(fmt.Sprintf("[IPCache] 목적지 Identity: %d (IP: %s)", dstIdentity, ctx.Pkt.DstIP))
	} else {
		ctx.DstIdentity = 2 // WORLD identity
		ctx.trace(fmt.Sprintf("[IPCache] 목적지 Identity: %d (WORLD, IP: %s)", ctx.DstIdentity, ctx.Pkt.DstIP))
	}

	// 2. 정책 검사
	// 실제: bpf/lib/policy.h의 __policy_check()
	// Identity 기반 정책 조회 (실제로는 LPM Trie에서 Identity+Protocol+Port 매칭)
	verdict, found := ds.policyMap.Lookup(ctx.SrcIdentity)
	if found && verdict == PolicyDeny {
		ctx.trace("[Policy] DENY - 정책에 의해 패킷 드롭")
		ds.packetsDropped++
		return ctx.tailCall(CILIUM_CALL_DROP_NOTIFY)
	}
	if found {
		ctx.trace("[Policy] ALLOW - 정책 통과")
	} else {
		ctx.trace("[Policy] DEFAULT ALLOW - 명시적 정책 없음 (기본 허용)")
	}

	// 3. SNAT (masquerade) 필요 여부
	// 실제: bpf/lib/nat.h의 snat_v4_needs_masquerade()
	if !isPrivateIP(ctx.Pkt.DstIP) {
		origSrcIP := ctx.Pkt.SrcIP
		origSrcPort := ctx.Pkt.SrcPort
		natSrcIP := "10.0.0.1" // 노드 IP로 SNAT
		natSrcPort := uint16(40000 + (ctx.Pkt.SrcPort % 1000))

		natTuple := CTTuple{
			SrcIP: origSrcIP, DstIP: ctx.Pkt.DstIP,
			SrcPort: origSrcPort, DstPort: ctx.Pkt.DstPort,
			Protocol: ctx.Pkt.Protocol,
		}
		ds.natMap.Update(natTuple, NATEntry{
			OrigSrcIP: origSrcIP, OrigSrcPort: origSrcPort,
			NatSrcIP: natSrcIP, NatSrcPort: natSrcPort,
		})
		ctx.Pkt.SrcIP = natSrcIP
		ctx.Pkt.SrcPort = natSrcPort
		ctx.trace(fmt.Sprintf("[NAT] SNAT: %s:%d -> %s:%d (masquerade)", origSrcIP, origSrcPort, natSrcIP, natSrcPort))
	} else {
		ctx.trace("[NAT] SNAT 불필요 (동일 네트워크)")
	}

	// 4. FIB Lookup + Forward
	// 실제: bpf/lib/fib.h의 fib_do_redirect()
	ctx.trace(fmt.Sprintf("[FIB] 라우팅 조회: dst=%s", ctx.Pkt.DstIP))
	ctx.trace(fmt.Sprintf("[FORWARD] 패킷 전달: %s", ctx.Pkt.String()))
	ds.packetsForwarded++

	return 0 // TC_ACT_OK
}

// --- tail call: 드롭 알림 ---
// 실제: bpf/lib/drop.h
func (ds *DatapathSimulator) tailDropNotify(ctx *ProgContext) int {
	ctx.trace("[drop_notify] 드롭 이벤트를 cilium_events 맵으로 전송")
	return -1 // TC_ACT_SHOT
}

func isPrivateIP(ip string) bool {
	for _, prefix := range []string{"10.", "172.16.", "172.17.", "192.168."} {
		if strings.HasPrefix(ip, prefix) {
			return true
		}
	}
	return false
}

// ============================================================
// 메인 시뮬레이션
// ============================================================

func main() {
	fmt.Println("=== Cilium eBPF 데이터패스 시뮬레이터 ===")
	fmt.Println()

	// ──────────────────────────────────────────────
	// Part 1: BPF 맵 유형 데모
	// ──────────────────────────────────────────────
	fmt.Println("[Part 1] BPF 맵 유형 시뮬레이션")
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println()

	// --- 1a. LPM Trie ---
	fmt.Println("  [1a] LPM Trie (Longest Prefix Match)")
	fmt.Println("       실제: bpf/lib/policy.h의 cilium_policy_v2")
	fmt.Println("       실제: bpf/bpf_xdp.c의 cilium_cidr_v4_dyn")
	fmt.Println(strings.Repeat("─", 55))

	ipcache := newLPMTrie[string]("cilium_ipcache")
	// CIDR 기반 Identity 매핑
	ipcache.Insert(0, 0, "WORLD (ID=2)")                                     // 0.0.0.0/0 -> WORLD
	ipcache.Insert(8, ipToUint32("10.0.0.0"), "CLUSTER (ID=100)")             // 10.0.0.0/8 -> cluster
	ipcache.Insert(24, ipToUint32("10.0.1.0"), "NAMESPACE-default (ID=200)")  // 10.0.1.0/24
	ipcache.Insert(32, ipToUint32("10.0.1.5"), "Pod-frontend (ID=48312)")     // 10.0.1.5/32

	lookups := []string{"10.0.1.5", "10.0.1.100", "10.0.2.50", "8.8.8.8"}
	for _, ip := range lookups {
		if identity, ok := ipcache.Lookup(ipToUint32(ip)); ok {
			fmt.Printf("       %-15s -> %s\n", ip, identity)
		}
	}
	fmt.Println()
	fmt.Println("       /32 > /24 > /8 > /0 순으로 가장 구체적인 매치가 우선")
	fmt.Println()

	// --- 1b. LRU Hash Map ---
	fmt.Println("  [1b] LRU Hash Map")
	fmt.Println("       실제: bpf/lib/conntrack_map.h의 cilium_ct4_global")
	fmt.Println(strings.Repeat("─", 55))

	ct := newLRUHashMap[string, int]("cilium_ct4_global", 3)
	fmt.Println("       맵 크기: 3 (LRU 동작 확인용)")

	entries := []string{"conn-A", "conn-B", "conn-C"}
	for _, e := range entries {
		ct.Update(e, 1)
		fmt.Printf("       + %s 추가\n", e)
	}

	// conn-B를 조회 → LRU 순서에서 최신으로 이동
	ct.Lookup("conn-B")
	fmt.Println("       ~ conn-B 조회 (LRU 순서 최신으로 이동)")

	// 4번째 엔트리 → 가장 오래된 conn-A가 제거됨 (conn-B는 최근 조회로 보호)
	evKey, evicted := ct.Update("conn-D", 1)
	if evicted {
		fmt.Printf("       + conn-D 추가 -> [LRU 제거] %s\n", evKey)
	}

	// conn-A가 제거되었는지 확인
	if _, ok := ct.Lookup("conn-A"); !ok {
		fmt.Println("       ? conn-A 조회 -> MISS (LRU에 의해 제거됨)")
	}
	if _, ok := ct.Lookup("conn-B"); ok {
		fmt.Println("       ? conn-B 조회 -> HIT (최근 접근으로 보호됨)")
	}
	fmt.Println()

	// --- 1c. Array Map ---
	fmt.Println("  [1c] Array Map (Prog Array)")
	fmt.Println("       실제: bpf/lib/tailcall.h의 cilium_calls")
	fmt.Println(strings.Repeat("─", 55))

	progNames := newArrayMap[string]("cilium_calls", CILIUM_CALL_SIZE)
	progNames.Set(CILIUM_CALL_DROP_NOTIFY, "drop_notify")
	progNames.Set(CILIUM_CALL_IPV4_FROM_LXC, "tail_handle_ipv4")
	progNames.Set(CILIUM_CALL_IPV4_CT_EGRESS, "tail_ipv4_ct_egress")
	progNames.Set(CILIUM_CALL_IPV4_FROM_LXC_CONT, "tail_handle_ipv4_cont")
	progNames.Set(CILIUM_CALL_IPV4_TO_ENDPOINT, "tail_ipv4_to_endpoint")
	progNames.Set(CILIUM_CALL_IPV4_CT_INGRESS, "tail_ipv4_ct_ingress")

	indices := []int{1, 7, 26, 28, 30, 13}
	for _, idx := range indices {
		if name, ok := progNames.Get(idx); ok && name != "" {
			fmt.Printf("       [%2d] = %s\n", idx, name)
		}
	}
	fmt.Printf("       전체 슬롯: %d (CILIUM_CALL_SIZE)\n", CILIUM_CALL_SIZE)
	fmt.Println()

	// ──────────────────────────────────────────────
	// Part 2: BPF 검증기 시뮬레이션
	// ──────────────────────────────────────────────
	fmt.Println("[Part 2] BPF 검증기(Verifier) 시뮬레이션")
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println()

	verifier := newVerifier()

	programs := []struct {
		name         string
		instructions int
		unbounded    bool
	}{
		{"tail_handle_ipv4", 8500, false},
		{"tail_ipv4_ct_egress", 12000, false},
		{"tail_handle_ipv4_cont", 45000, false},
		{"단일_프로그램_전체_로직", 1500000, false},     // 검증기 한도 초과
		{"무한_루프_프로그램", 500, true},                // 무한 루프
	}

	for _, p := range programs {
		result := verifier.Verify(p.name, p.instructions, p.unbounded)
		status := "PASS"
		if !result.Passed {
			status = "FAIL"
		}
		fmt.Printf("  [%s] %-30s (%d insns) - %s\n", status, result.ProgramName, result.Instructions, result.Reason)
	}

	fmt.Println()
	fmt.Println("  Tail Call로 분리하면 각 프로그램이 한도 이내:")
	splitProgs := []string{"tail_handle_ipv4 (8,500)", "tail_ipv4_ct_egress (12,000)", "tail_handle_ipv4_cont (45,000)"}
	total := 0
	for _, p := range splitProgs {
		fmt.Printf("    + %s\n", p)
	}
	total = 8500 + 12000 + 45000
	fmt.Printf("    = 총 %d 명령어 (각각 < 1,000,000)\n", total)
	fmt.Println()

	// ──────────────────────────────────────────────
	// Part 3: Tail Call 체인 실행
	// ──────────────────────────────────────────────
	fmt.Println("[Part 3] Tail Call 체인 실행 시뮬레이션")
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println()

	ds := newDatapathSimulator()

	// 맵 초기 데이터 설정
	// IPCache: IP -> Security Identity
	ds.ipcache.Insert(32, ipToUint32("10.0.1.5"), 48312)   // frontend pod
	ds.ipcache.Insert(32, ipToUint32("10.0.1.10"), 48313)  // backend pod
	ds.ipcache.Insert(24, ipToUint32("10.0.1.0"), 200)     // namespace
	ds.ipcache.Insert(0, 0, 2)                              // world

	// 정책: Identity 48312(frontend)은 허용
	ds.policyMap.Update(48312, PolicyAllow)

	// 서비스: ClusterIP -> Backend
	ds.svcMap.Update("10.96.0.100:80", "10.0.1.10:8080")

	// ──── 시나리오 1: Pod -> 다른 Pod (직접 통신) ────
	fmt.Println("  --- 시나리오 1: Pod -> Pod (직접) ---")
	pkt1 := Packet{
		SrcIP: "10.0.1.5", DstIP: "10.0.1.10",
		SrcPort: 34567, DstPort: 8080,
		Protocol: 6,
	}
	ctx1 := newContext(pkt1, 48312)
	ds.cilFromContainer(ctx1)
	for _, log := range ctx1.TraceLog {
		fmt.Println("  " + log)
	}
	fmt.Println()

	// ──── 시나리오 2: Pod -> Service (LB + DNAT) ────
	fmt.Println("  --- 시나리오 2: Pod -> Service (ClusterIP LB) ---")
	pkt2 := Packet{
		SrcIP: "10.0.1.5", DstIP: "10.96.0.100",
		SrcPort: 34568, DstPort: 80,
		Protocol: 6,
	}
	ctx2 := newContext(pkt2, 48312)
	ds.cilFromContainer(ctx2)
	for _, log := range ctx2.TraceLog {
		fmt.Println("  " + log)
	}
	fmt.Println()

	// ──── 시나리오 3: Pod -> 외부 (SNAT) ────
	fmt.Println("  --- 시나리오 3: Pod -> 외부 인터넷 (SNAT 필요) ---")
	pkt3 := Packet{
		SrcIP: "10.0.1.5", DstIP: "8.8.8.8",
		SrcPort: 34569, DstPort: 443,
		Protocol: 6,
	}
	ctx3 := newContext(pkt3, 48312)
	ds.cilFromContainer(ctx3)
	for _, log := range ctx3.TraceLog {
		fmt.Println("  " + log)
	}
	fmt.Println()

	// ──── 시나리오 4: 정책에 의한 드롭 ────
	fmt.Println("  --- 시나리오 4: 정책 거부 (DENY) ---")
	// 악성 Identity를 DENY로 등록
	ds.policyMap.Update(99999, PolicyDeny)
	pkt4 := Packet{
		SrcIP: "10.0.2.50", DstIP: "10.0.1.10",
		SrcPort: 55555, DstPort: 8080,
		Protocol: 6,
	}
	ctx4 := newContext(pkt4, 99999)
	ds.cilFromContainer(ctx4)
	for _, log := range ctx4.TraceLog {
		fmt.Println("  " + log)
	}
	fmt.Println()

	// ──── 시나리오 5: 동일 연결 재사용 (CT hit) ────
	fmt.Println("  --- 시나리오 5: 동일 연결 재전송 (CT Hit) ---")
	pkt5 := Packet{
		SrcIP: "10.0.1.5", DstIP: "10.0.1.10",
		SrcPort: 34567, DstPort: 8080,
		Protocol: 6,
	}
	ctx5 := newContext(pkt5, 48312)
	ds.cilFromContainer(ctx5)
	for _, log := range ctx5.TraceLog {
		fmt.Println("  " + log)
	}
	fmt.Println()

	// ──────────────────────────────────────────────
	// Part 4: Tail Call 체인 구조 시각화
	// ──────────────────────────────────────────────
	fmt.Println("[Part 4] Tail Call 체인 구조 (Egress)")
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println()
	fmt.Println("  cil_from_container                     (bpf/bpf_lxc.c)")
	fmt.Println("    |")
	fmt.Println("    +-- tail_call[7] --> tail_handle_ipv4")
	fmt.Println("    |                    |")
	fmt.Println("    |                    +-- 패킷 유효성 검사")
	fmt.Println("    |                    +-- per-packet LB (서비스 DNAT)")
	fmt.Println("    |                    |")
	fmt.Println("    |                    +-- tail_call[30] --> tail_ipv4_ct_egress")
	fmt.Println("    |                                         |")
	fmt.Println("    |                                         +-- CT 조회/생성")
	fmt.Println("    |                                         |")
	fmt.Println("    |                                         +-- tail_call[26] --> tail_handle_ipv4_cont")
	fmt.Println("    |                                                              |")
	fmt.Println("    |                                                              +-- IPCache 조회")
	fmt.Println("    |                                                              +-- 정책 검사")
	fmt.Println("    |                                                              +-- NAT (SNAT)")
	fmt.Println("    |                                                              +-- FIB lookup")
	fmt.Println("    |                                                              +-- redirect")
	fmt.Println("    |")
	fmt.Println("    +-- tail_call[1] --> drop_notify (드롭 시)")
	fmt.Println()

	// ──────────────────────────────────────────────
	// Part 5: 컴파일 파이프라인 시뮬레이션
	// ──────────────────────────────────────────────
	fmt.Println("[Part 5] BPF 컴파일 파이프라인")
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println()

	type compileUnit struct {
		source  string
		output  string
		defines []string
	}

	units := []compileUnit{
		{
			source: "bpf/bpf_lxc.c",
			output: "bpf_lxc.o",
			defines: []string{
				"LXC_ID=1234",
				"SECLABEL=48312",
				"ENABLE_IPV4=1",
				"ENABLE_NODEPORT=1",
				"CT_MAP_SIZE_TCP=524288",
			},
		},
		{
			source: "bpf/bpf_host.c",
			output: "bpf_host.o",
			defines: []string{
				"IS_BPF_HOST=1",
				"ENABLE_IPV4=1",
				"ENABLE_IPV6=1",
				"ENABLE_NODEPORT=1",
			},
		},
		{
			source: "bpf/bpf_xdp.c",
			output: "bpf_xdp.o",
			defines: []string{
				"IS_BPF_XDP=1",
				"ENABLE_NODEPORT_ACCELERATION=1",
			},
		},
	}

	for _, u := range units {
		fmt.Printf("  [컴파일] %s -> %s\n", u.source, u.output)
		fmt.Printf("           clang -O2 --target=bpf -g")
		for _, d := range u.defines[:2] {
			fmt.Printf(" -D%s", d)
		}
		fmt.Printf(" ...\n")

		// 시뮬레이션된 명령어 수
		insns := 8000 + len(u.defines)*2000
		fmt.Printf("           결과: %d 명령어, BTF 포함\n", insns)
		fmt.Println()
	}

	fmt.Println("  [로딩 흐름]")
	fmt.Println("  1. ebpf.LoadCollectionSpec(\"bpf_lxc.o\")")
	fmt.Println("     -> ELF 파싱, 섹션 분석, BTF 추출")
	fmt.Println("  2. 맵 이름 리매핑 (Endpoint별)")
	fmt.Println("     -> cilium_calls_ + endpoint_id")
	fmt.Println("  3. bpf.LoadCollection(spec, opts)")
	fmt.Println("     -> bpf() syscall로 커널에 로드")
	fmt.Println("  4. TCX/TC 부착 (netlink)")
	fmt.Println("     -> veth ingress에 cil_from_container 부착")
	fmt.Println("  5. 맵 핀 커밋")
	fmt.Println("     -> /sys/fs/bpf/tc/globals/cilium_*")
	fmt.Println()

	// 최종 통계
	fmt.Println("[통계]")
	fmt.Println(strings.Repeat("=", 60))
	fmt.Printf("  처리된 패킷: %d\n", ds.packetsProcessed)
	fmt.Printf("  전달된 패킷: %d\n", ds.packetsForwarded)
	fmt.Printf("  드롭된 패킷: %d\n", ds.packetsDropped)
	fmt.Printf("  CT 맵 엔트리: %d/%d\n", len(ds.ctMap.entries), ds.ctMap.maxSize)
	fmt.Printf("  NAT 맵 엔트리: %d/%d\n", len(ds.natMap.entries), ds.natMap.maxSize)

}
