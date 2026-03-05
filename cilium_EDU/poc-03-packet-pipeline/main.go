package main

import (
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
)

// =============================================================================
// Cilium BPF 패킷 처리 파이프라인 시뮬레이션
// =============================================================================
//
// Cilium은 BPF 프로그램을 tail call 체인으로 연결하여 패킷을 처리한다.
// 실제 코드:
//   - bpf/bpf_lxc.c: 엔드포인트 TC 프로그램 (from_container, to_container)
//   - bpf/lib/tail_call.h: tail call 매크로 (tail_call_static, invoke_tailcall_if)
//   - bpf/lib/conntrack.h: Connection Tracking
//   - bpf/lib/policy.h: 정책 검사
//
// Tail Call 메커니즘:
//   - BPF 프로그램은 단일 프로그램에 명령어 수 제한이 있음 (1M instructions)
//   - tail call로 다른 BPF 프로그램으로 점프 (스택 유지 안 됨, 리턴 불가)
//   - PROG_ARRAY 맵에 인덱스별로 프로그램을 등록하고 인덱스로 호출
//
// 이 PoC에서:
//   - 각 BPF 프로그램 = goroutine
//   - tail call = channel로 패킷 전달 (프로그램 배열 인덱스 기반)
//   - 패킷 = Packet 구조체
// =============================================================================

// --- 패킷 구조 ---

// EtherType은 이더넷 프레임 타입을 나타낸다.
type EtherType uint16

const (
	EtherTypeIPv4 EtherType = 0x0800
	EtherTypeIPv6 EtherType = 0x86DD
	EtherTypeARP  EtherType = 0x0806
)

func (e EtherType) String() string {
	switch e {
	case EtherTypeIPv4:
		return "IPv4"
	case EtherTypeIPv6:
		return "IPv6"
	case EtherTypeARP:
		return "ARP"
	default:
		return fmt.Sprintf("0x%04x", uint16(e))
	}
}

// Protocol은 IP 프로토콜 번호이다.
type Protocol uint8

const (
	ProtoTCP  Protocol = 6
	ProtoUDP  Protocol = 17
	ProtoICMP Protocol = 1
)

func (p Protocol) String() string {
	switch p {
	case ProtoTCP:
		return "TCP"
	case ProtoUDP:
		return "UDP"
	case ProtoICMP:
		return "ICMP"
	default:
		return fmt.Sprintf("proto-%d", p)
	}
}

// CTState는 Connection Tracking 상태를 나타낸다.
// 실제 코드: bpf/lib/conntrack.h
type CTState int

const (
	CTNew         CTState = iota // 새 연결
	CTEstablished                // 기존 연결
	CTReply                      // 응답 패킷
	CTRelated                    // 관련 연결 (예: ICMP error)
)

func (s CTState) String() string {
	names := []string{"NEW", "ESTABLISHED", "REPLY", "RELATED"}
	if int(s) < len(names) {
		return names[s]
	}
	return "UNKNOWN"
}

// Verdict는 패킷 처리 결과이다.
// 실제 코드: bpf/lib/common.h (TC_ACT_OK, TC_ACT_SHOT 등)
type Verdict int

const (
	VerdictPass    Verdict = iota // TC_ACT_OK — 패킷 허용
	VerdictDrop                   // TC_ACT_SHOT — 패킷 드롭
	VerdictRedirect               // TC_ACT_REDIRECT — 리다이렉트
)

func (v Verdict) String() string {
	names := []string{"PASS", "DROP", "REDIRECT"}
	if int(v) < len(names) {
		return names[v]
	}
	return "UNKNOWN"
}

// Packet은 처리 중인 패킷을 표현한다.
type Packet struct {
	ID        int
	EtherType EtherType
	SrcIP     net.IP
	DstIP     net.IP
	Protocol  Protocol
	SrcPort   uint16
	DstPort   uint16
	SrcID     uint32 // 소스 Security Identity
	DstID     uint32 // 목적지 Security Identity
	CTState   CTState
	Verdict   Verdict
	Trace     []string // 패킷 경로 추적
}

func (p *Packet) AddTrace(msg string) {
	p.Trace = append(p.Trace, msg)
}

func (p *Packet) String() string {
	return fmt.Sprintf("Pkt#%d [%s] %s:%d → %s:%d (%s) SrcID:%d DstID:%d",
		p.ID, p.Protocol, p.SrcIP, p.SrcPort, p.DstIP, p.DstPort,
		p.EtherType, p.SrcID, p.DstID)
}

// --- Tail Call 프로그램 배열 ---

// TailCallIndex는 프로그램 배열의 인덱스이다.
// 실제 코드: bpf/lib/tailcall.h — CILIUM_CALL_* 상수들
type TailCallIndex int

const (
	CALL_IPV4_FROM_LXC     TailCallIndex = 0 // IPv4 패킷 처리 (from container)
	CALL_IPV6_FROM_LXC     TailCallIndex = 1 // IPv6 패킷 처리
	CALL_ARP               TailCallIndex = 2 // ARP 처리
	CALL_CT_LOOKUP         TailCallIndex = 3 // Connection Tracking 조회
	CALL_POLICY_CHECK      TailCallIndex = 4 // 정책 검사
	CALL_NAT               TailCallIndex = 5 // NAT 처리
	CALL_ROUTING           TailCallIndex = 6 // 라우팅 결정
	CALL_ENCAP             TailCallIndex = 7 // 캡슐화 (VXLAN/Geneve)
	CALL_DELIVER           TailCallIndex = 8 // 최종 전달
)

func (i TailCallIndex) String() string {
	names := map[TailCallIndex]string{
		CALL_IPV4_FROM_LXC: "ipv4_from_lxc",
		CALL_IPV6_FROM_LXC: "ipv6_from_lxc",
		CALL_ARP:           "arp_handler",
		CALL_CT_LOOKUP:     "ct_lookup",
		CALL_POLICY_CHECK:  "policy_check",
		CALL_NAT:           "nat",
		CALL_ROUTING:       "routing",
		CALL_ENCAP:         "encap",
		CALL_DELIVER:       "deliver",
	}
	if name, ok := names[i]; ok {
		return name
	}
	return fmt.Sprintf("prog_%d", i)
}

// --- BPF Program Array (tail call 테이블) ---

// BPFProgram은 하나의 BPF 프로그램을 표현한다.
type BPFProgram func(pkt *Packet) (nextCall TailCallIndex, done bool)

// ProgArray는 BPF 프로그램 배열 맵이다.
// 실제 코드: BPF_MAP_TYPE_PROG_ARRAY
// 인덱스로 프로그램을 조회하여 tail call 한다.
type ProgArray struct {
	programs map[TailCallIndex]BPFProgram
}

func NewProgArray() *ProgArray {
	return &ProgArray{programs: make(map[TailCallIndex]BPFProgram)}
}

func (pa *ProgArray) Register(index TailCallIndex, prog BPFProgram) {
	pa.programs[index] = prog
}

// --- Connection Tracking 테이블 ---

// CTKey는 Connection Tracking 키이다.
type CTKey struct {
	SrcIP    string
	DstIP    string
	SrcPort  uint16
	DstPort  uint16
	Protocol Protocol
}

// CTTable은 Connection Tracking 테이블이다.
// 실제 BPF 맵: cilium_ct4_global (LRU Hash)
type CTTable struct {
	mu      sync.RWMutex
	entries map[CTKey]CTState
}

func NewCTTable() *CTTable {
	return &CTTable{entries: make(map[CTKey]CTState)}
}

func (ct *CTTable) Lookup(key CTKey) (CTState, bool) {
	ct.mu.RLock()
	defer ct.mu.RUnlock()
	state, ok := ct.entries[key]
	return state, ok
}

func (ct *CTTable) Create(key CTKey, state CTState) {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	ct.entries[key] = state
}

// --- Policy Map ---

// PolicyKey는 정책 맵 키이다.
// 실제 BPF 맵: cilium_policy_* (per-endpoint)
type PolicyKey struct {
	SrcIdentity uint32
	DstPort     uint16
	Protocol    Protocol
}

type PolicyMap struct {
	mu      sync.RWMutex
	allowed map[PolicyKey]bool
}

func NewPolicyMap() *PolicyMap {
	return &PolicyMap{allowed: make(map[PolicyKey]bool)}
}

func (pm *PolicyMap) Allow(srcID uint32, dstPort uint16, proto Protocol) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.allowed[PolicyKey{srcID, dstPort, proto}] = true
}

func (pm *PolicyMap) IsAllowed(srcID uint32, dstPort uint16, proto Protocol) bool {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	// Identity 0은 항상 거부
	if srcID == 0 {
		return false
	}
	return pm.allowed[PolicyKey{srcID, dstPort, proto}]
}

// --- IP → Identity 매핑 ---

type IPIdentityMap struct {
	mu      sync.RWMutex
	entries map[string]uint32 // IP → Identity
}

func NewIPIdentityMap() *IPIdentityMap {
	return &IPIdentityMap{entries: make(map[string]uint32)}
}

func (m *IPIdentityMap) Set(ip string, identity uint32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[ip] = identity
}

func (m *IPIdentityMap) Get(ip string) (uint32, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.entries[ip]
	return id, ok
}

// --- 파이프라인 구성 ---

// Pipeline은 BPF 프로그램 체인을 실행하는 엔진이다.
type Pipeline struct {
	progArray   *ProgArray
	ctTable     *CTTable
	policyMap   *PolicyMap
	ipIDMap     *IPIdentityMap
	resultCh    chan *Packet
}

func NewPipeline() *Pipeline {
	p := &Pipeline{
		progArray: NewProgArray(),
		ctTable:   NewCTTable(),
		policyMap: NewPolicyMap(),
		ipIDMap:   NewIPIdentityMap(),
		resultCh:  make(chan *Packet, 100),
	}
	p.registerPrograms()
	return p
}

// registerPrograms는 각 tail call 슬롯에 BPF 프로그램을 등록한다.
func (p *Pipeline) registerPrograms() {

	// 프로그램 0: IPv4 패킷 분류 (from_container 진입점)
	// 실제 코드: bpf/bpf_lxc.c — handle_xgress()
	p.progArray.Register(CALL_IPV4_FROM_LXC, func(pkt *Packet) (TailCallIndex, bool) {
		pkt.AddTrace(fmt.Sprintf("[ipv4_from_lxc] 패킷 수신: %s", pkt))

		// IP → Identity 조회 (ipcache 맵)
		if srcID, ok := p.ipIDMap.Get(pkt.SrcIP.String()); ok {
			pkt.SrcID = srcID
		}
		if dstID, ok := p.ipIDMap.Get(pkt.DstIP.String()); ok {
			pkt.DstID = dstID
		}
		pkt.AddTrace(fmt.Sprintf("[ipv4_from_lxc] Identity 조회: src=%d, dst=%d", pkt.SrcID, pkt.DstID))

		// tail call → CT lookup
		return CALL_CT_LOOKUP, false
	})

	// 프로그램 1: IPv6 처리 (간략화)
	p.progArray.Register(CALL_IPV6_FROM_LXC, func(pkt *Packet) (TailCallIndex, bool) {
		pkt.AddTrace("[ipv6_from_lxc] IPv6 패킷 처리 (IPv4와 유사, 생략)")
		return CALL_CT_LOOKUP, false
	})

	// 프로그램 2: ARP 처리
	p.progArray.Register(CALL_ARP, func(pkt *Packet) (TailCallIndex, bool) {
		pkt.AddTrace("[arp_handler] ARP 요청 처리, 직접 응답")
		pkt.Verdict = VerdictPass
		return 0, true // 완료
	})

	// 프로그램 3: Connection Tracking 조회
	// 실제 코드: bpf/lib/conntrack.h — ct_lookup4()
	p.progArray.Register(CALL_CT_LOOKUP, func(pkt *Packet) (TailCallIndex, bool) {
		ctKey := CTKey{
			SrcIP:    pkt.SrcIP.String(),
			DstIP:    pkt.DstIP.String(),
			SrcPort:  pkt.SrcPort,
			DstPort:  pkt.DstPort,
			Protocol: pkt.Protocol,
		}

		state, found := p.ctTable.Lookup(ctKey)
		if found {
			pkt.CTState = state
			pkt.AddTrace(fmt.Sprintf("[ct_lookup] CT 히트: %s", state))
			if state == CTEstablished || state == CTReply {
				// 기존 연결은 정책 검사 건너뜀 — 바로 라우팅으로
				return CALL_ROUTING, false
			}
		} else {
			pkt.CTState = CTNew
			pkt.AddTrace("[ct_lookup] CT 미스: 새 연결")
			// 새 연결 → CT 엔트리 생성
			p.ctTable.Create(ctKey, CTEstablished)
			// 역방향 엔트리도 생성 (응답 패킷 추적용)
			reverseKey := CTKey{
				SrcIP: pkt.DstIP.String(), DstIP: pkt.SrcIP.String(),
				SrcPort: pkt.DstPort, DstPort: pkt.SrcPort,
				Protocol: pkt.Protocol,
			}
			p.ctTable.Create(reverseKey, CTReply)
		}

		// 새 연결 → 정책 검사
		return CALL_POLICY_CHECK, false
	})

	// 프로그램 4: 정책 검사
	// 실제 코드: bpf/lib/policy.h — policy_can_egress4()
	p.progArray.Register(CALL_POLICY_CHECK, func(pkt *Packet) (TailCallIndex, bool) {
		allowed := p.policyMap.IsAllowed(pkt.SrcID, pkt.DstPort, pkt.Protocol)

		if allowed {
			pkt.AddTrace(fmt.Sprintf("[policy_check] 허용: srcID=%d → dstPort=%d/%s",
				pkt.SrcID, pkt.DstPort, pkt.Protocol))
			return CALL_ROUTING, false
		}

		pkt.AddTrace(fmt.Sprintf("[policy_check] 거부: srcID=%d → dstPort=%d/%s",
			pkt.SrcID, pkt.DstPort, pkt.Protocol))
		pkt.Verdict = VerdictDrop
		return 0, true // 드롭
	})

	// 프로그램 5: NAT 처리
	p.progArray.Register(CALL_NAT, func(pkt *Packet) (TailCallIndex, bool) {
		pkt.AddTrace("[nat] NAT 처리 (SNAT/DNAT)")
		return CALL_DELIVER, false
	})

	// 프로그램 6: 라우팅 결정
	// 실제 코드: bpf/bpf_lxc.c — 로컬/리모트/서비스 판별
	p.progArray.Register(CALL_ROUTING, func(pkt *Packet) (TailCallIndex, bool) {
		dstIP := pkt.DstIP.To4()
		if dstIP == nil {
			pkt.AddTrace("[routing] IPv4 주소 변환 실패")
			pkt.Verdict = VerdictDrop
			return 0, true
		}

		// 로컬 Pod 대역 체크 (10.244.0.0/24)
		localNet := &net.IPNet{
			IP:   net.ParseIP("10.244.0.0"),
			Mask: net.CIDRMask(24, 32),
		}

		if localNet.Contains(dstIP) {
			pkt.AddTrace("[routing] 로컬 Pod 대상 → 직접 전달")
			return CALL_DELIVER, false
		}

		// 리모트 노드 → 캡슐화 필요 (VXLAN/Geneve)
		pkt.AddTrace("[routing] 리모트 노드 대상 → VXLAN 캡슐화")
		return CALL_ENCAP, false
	})

	// 프로그램 7: 캡슐화
	p.progArray.Register(CALL_ENCAP, func(pkt *Packet) (TailCallIndex, bool) {
		// VXLAN VNI 생성 (Identity 기반)
		vni := pkt.SrcID
		pkt.AddTrace(fmt.Sprintf("[encap] VXLAN 캡슐화: VNI=%d, 외부 목적지=리모트 노드", vni))
		return CALL_DELIVER, false
	})

	// 프로그램 8: 최종 전달
	p.progArray.Register(CALL_DELIVER, func(pkt *Packet) (TailCallIndex, bool) {
		pkt.Verdict = VerdictPass
		pkt.AddTrace("[deliver] 패킷 전달 완료")
		return 0, true
	})
}

// ProcessPacket은 패킷을 파이프라인에서 처리한다.
// tail call 체인을 따라가며 각 프로그램을 순서대로 실행한다.
func (p *Pipeline) ProcessPacket(pkt *Packet) {
	// 진입점 결정: EtherType에 따라 첫 프로그램 선택
	var startIndex TailCallIndex
	switch pkt.EtherType {
	case EtherTypeIPv4:
		startIndex = CALL_IPV4_FROM_LXC
	case EtherTypeIPv6:
		startIndex = CALL_IPV6_FROM_LXC
	case EtherTypeARP:
		startIndex = CALL_ARP
	default:
		pkt.AddTrace(fmt.Sprintf("[classifier] 알 수 없는 EtherType: %s, 드롭", pkt.EtherType))
		pkt.Verdict = VerdictDrop
		p.resultCh <- pkt
		return
	}

	// Tail call 체인 실행
	// 실제 BPF에서는 tail_call_static() 매크로가 bpf_tail_call()을 호출
	// 최대 33번의 tail call만 허용 (커널 제한)
	maxTailCalls := 33
	currentIndex := startIndex

	for i := 0; i < maxTailCalls; i++ {
		prog, ok := p.progArray.programs[currentIndex]
		if !ok {
			pkt.AddTrace(fmt.Sprintf("[tail_call] 프로그램 인덱스 %d에 등록된 프로그램 없음", currentIndex))
			pkt.Verdict = VerdictDrop
			break
		}

		nextIndex, done := prog(pkt)
		if done {
			break
		}

		pkt.AddTrace(fmt.Sprintf("[tail_call] %s → %s", currentIndex, nextIndex))
		currentIndex = nextIndex
	}

	p.resultCh <- pkt
}

// --- 패킷 생성 헬퍼 ---

func makePacket(id int, srcIP, dstIP string, proto Protocol, srcPort, dstPort uint16) *Packet {
	return &Packet{
		ID:        id,
		EtherType: EtherTypeIPv4,
		SrcIP:     net.ParseIP(srcIP),
		DstIP:     net.ParseIP(dstIP),
		Protocol:  proto,
		SrcPort:   srcPort,
		DstPort:   dstPort,
	}
}

// --- 메인 ---

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════╗")
	fmt.Println("║  Cilium BPF 패킷 처리 파이프라인 시뮬레이션        ║")
	fmt.Println("║  Tail Call 체인 기반 패킷 분류/CT/정책/라우팅       ║")
	fmt.Println("╚══════════════════════════════════════════════════════╝")

	pipeline := NewPipeline()

	// IP → Identity 매핑 설정 (ipcache)
	pipeline.ipIDMap.Set("10.244.0.10", 256) // frontend pod
	pipeline.ipIDMap.Set("10.244.0.20", 257) // backend pod
	pipeline.ipIDMap.Set("10.244.1.30", 258) // 리모트 노드 pod
	pipeline.ipIDMap.Set("8.8.8.8", 2)       // world identity

	// 정책 설정
	// frontend(256) → backend(257) TCP:8080 허용
	pipeline.policyMap.Allow(256, 8080, ProtoTCP)
	// backend(257) → 외부(2) TCP:443 허용
	pipeline.policyMap.Allow(257, 443, ProtoTCP)

	fmt.Println("\n=== 정책 설정 ===")
	fmt.Println("  허용: frontend(ID:256) → *:8080/TCP")
	fmt.Println("  허용: backend(ID:257)  → *:443/TCP")
	fmt.Println("  그 외: 거부")

	// 테스트 패킷들
	packets := []*Packet{
		// 1. 허용되는 패킷: frontend → backend:8080 (로컬)
		makePacket(1, "10.244.0.10", "10.244.0.20", ProtoTCP, 45678, 8080),
		// 2. 거부되는 패킷: frontend → backend:3306 (정책 없음)
		makePacket(2, "10.244.0.10", "10.244.0.20", ProtoTCP, 45679, 3306),
		// 3. 리모트 노드 전달: frontend → 리모트 pod (VXLAN 캡슐화)
		makePacket(3, "10.244.0.10", "10.244.1.30", ProtoTCP, 45680, 8080),
		// 4. 기존 연결 (CT hit): frontend → backend:8080 (두 번째 패킷)
		makePacket(4, "10.244.0.10", "10.244.0.20", ProtoTCP, 45678, 8080),
		// 5. 응답 패킷 (CT reply): backend → frontend
		makePacket(5, "10.244.0.20", "10.244.0.10", ProtoTCP, 8080, 45678),
		// 6. 외부 접근: backend → 8.8.8.8:443
		makePacket(6, "10.244.0.20", "8.8.8.8", ProtoTCP, 50000, 443),
	}

	fmt.Printf("\n=== %d개 패킷 처리 시작 ===\n", len(packets))

	// 패킷 처리 (goroutine = BPF 프로그램 실행 컨텍스트)
	var wg sync.WaitGroup
	for _, pkt := range packets {
		wg.Add(1)
		go func(p *Packet) {
			defer wg.Done()
			pipeline.ProcessPacket(p)
		}(pkt)
	}

	// 모든 처리 완료 대기
	go func() {
		wg.Wait()
		close(pipeline.resultCh)
	}()

	// 결과 수집 및 출력
	results := make([]*Packet, 0, len(packets))
	for pkt := range pipeline.resultCh {
		results = append(results, pkt)
	}

	// ID 순으로 정렬
	sort.Slice(results, func(i, j int) bool {
		return results[i].ID < results[j].ID
	})

	// 결과 출력
	for _, pkt := range results {
		fmt.Printf("\n%s\n", strings.Repeat("─", 70))
		fmt.Printf("패킷 #%d: %s:%d → %s:%d (%s)\n",
			pkt.ID, pkt.SrcIP, pkt.SrcPort, pkt.DstIP, pkt.DstPort, pkt.Protocol)
		fmt.Printf("결과: %s | CT: %s | SrcID: %d | DstID: %d\n",
			pkt.Verdict, pkt.CTState, pkt.SrcID, pkt.DstID)
		fmt.Println("Trace:")
		for _, t := range pkt.Trace {
			fmt.Printf("  %s\n", t)
		}
	}

	// 요약
	fmt.Printf("\n%s\n", strings.Repeat("═", 70))
	fmt.Println("=== 처리 결과 요약 ===")
	passed, dropped := 0, 0
	for _, pkt := range results {
		if pkt.Verdict == VerdictPass {
			passed++
		} else {
			dropped++
		}
	}
	fmt.Printf("  전체: %d | 허용: %d | 드롭: %d\n", len(results), passed, dropped)
	fmt.Println("\n파이프라인 시뮬레이션 완료.")
}
