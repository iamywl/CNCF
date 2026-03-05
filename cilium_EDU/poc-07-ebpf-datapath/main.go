package main

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"net"
	"strings"
	"time"
)

// =============================================================================
// Cilium eBPF 데이터패스 시뮬레이션
// =============================================================================
// Cilium의 eBPF 프로그램은 패킷이 NIC에 도착하면 커널 네트워크 스택 이전에
// 실행되어 분류, CT 조회, 정책 검사, 라우팅 결정을 수행한다.
// 이 PoC는 그 흐름을 Go 함수 체이닝으로 시뮬레이션한다.
//
// 실제 Cilium 데이터패스 흐름:
//   패킷 도착 → handle_xgress → classify(ethertype)
//   → extract_tuple(5-tuple) → ct_lookup(conntrack)
//   → policy_check(identity+port+proto) → routing_decision
//   → encap/redirect/drop
// =============================================================================

// --- 패킷 및 헤더 구조체 ---

// EtherType 상수 (IEEE 802.3)
const (
	EtherTypeIPv4 uint16 = 0x0800
	EtherTypeIPv6 uint16 = 0x86DD
	EtherTypeARP  uint16 = 0x0806
)

// 프로토콜 상수
const (
	ProtoTCP  uint8 = 6
	ProtoUDP  uint8 = 17
	ProtoICMP uint8 = 1
)

// EthHeader는 이더넷 헤더를 나타낸다
type EthHeader struct {
	DstMAC    net.HardwareAddr
	SrcMAC    net.HardwareAddr
	EtherType uint16
}

// IPv4Header는 IP 헤더의 핵심 필드를 나타낸다
type IPv4Header struct {
	SrcIP    net.IP
	DstIP    net.IP
	Protocol uint8
	TTL      uint8
}

// L4Header는 L4 포트 정보를 나타낸다
type L4Header struct {
	SrcPort uint16
	DstPort uint16
}

// Tuple5은 연결 추적에 사용되는 5-튜플이다
// Cilium의 struct ipv4_ct_tuple에 대응
type Tuple5 struct {
	SrcIP    net.IP
	DstIP    net.IP
	SrcPort  uint16
	DstPort  uint16
	Protocol uint8
}

func (t Tuple5) String() string {
	return fmt.Sprintf("%s:%d → %s:%d (proto=%d)",
		t.SrcIP, t.SrcPort, t.DstIP, t.DstPort, t.Protocol)
}

// ReverseTuple은 역방향 튜플을 생성한다 (응답 패킷 매칭용)
func (t Tuple5) ReverseTuple() Tuple5 {
	return Tuple5{
		SrcIP: t.DstIP, DstIP: t.SrcIP,
		SrcPort: t.DstPort, DstPort: t.SrcPort,
		Protocol: t.Protocol,
	}
}

// --- Packet 구조체: 시뮬레이션 대상 ---

// Packet은 처리할 네트워크 패킷을 나타낸다
type Packet struct {
	Eth  EthHeader
	IP   IPv4Header
	L4   L4Header
	Data []byte // 페이로드
}

// --- Conntrack (CT) 테이블 ---

// CTState는 연결 상태를 나타낸다
type CTState int

const (
	CTNew         CTState = iota // 새 연결
	CTEstablished                // 확립된 연결
	CTReply                      // 응답 방향
	CTRelated                    // 관련 연결 (예: ICMP error)
)

func (s CTState) String() string {
	switch s {
	case CTNew:
		return "NEW"
	case CTEstablished:
		return "ESTABLISHED"
	case CTReply:
		return "REPLY"
	case CTRelated:
		return "RELATED"
	default:
		return "UNKNOWN"
	}
}

// CTEntry는 conntrack 테이블의 엔트리이다
// Cilium의 struct ct_entry에 대응
type CTEntry struct {
	State    CTState
	Lifetime time.Time // 만료 시간
	TxBytes  uint64
	TxPkts   uint32
	RxBytes  uint64
	RxPkts   uint32
	RevNAT   uint16 // 역방향 NAT 서비스 ID
}

// CTTable은 BPF conntrack 맵을 시뮬레이션한다
// 실제 Cilium: cilium_ct4_global (LRU hash map)
type CTTable struct {
	entries map[string]*CTEntry // 5-tuple 키 → CT 엔트리
}

func NewCTTable() *CTTable {
	return &CTTable{entries: make(map[string]*CTEntry)}
}

// Lookup은 CT 테이블에서 튜플을 조회한다
func (ct *CTTable) Lookup(tuple Tuple5) (*CTEntry, CTState) {
	key := tuple.String()
	// 정방향 매칭
	if entry, ok := ct.entries[key]; ok {
		return entry, entry.State
	}
	// 역방향 매칭 (응답 패킷)
	revKey := tuple.ReverseTuple().String()
	if entry, ok := ct.entries[revKey]; ok {
		return entry, CTReply
	}
	// 새 연결
	return nil, CTNew
}

// Create는 새 CT 엔트리를 생성한다
func (ct *CTTable) Create(tuple Tuple5) *CTEntry {
	entry := &CTEntry{
		State:    CTEstablished,
		Lifetime: time.Now().Add(120 * time.Second),
	}
	ct.entries[tuple.String()] = entry
	return entry
}

// --- 보안 Identity 및 Policy ---

// SecurityIdentity는 Cilium의 숫자형 보안 ID이다
// 실제로는 Pod 레이블로부터 할당됨
type SecurityIdentity uint32

const (
	IdentityWorld    SecurityIdentity = 2  // 외부 트래픽
	IdentityHost     SecurityIdentity = 1  // 호스트
	IdentityApp      SecurityIdentity = 1000
	IdentityDB       SecurityIdentity = 1001
	IdentityFrontend SecurityIdentity = 1002
)

// PolicyVerdict는 정책 판정 결과이다
type PolicyVerdict int

const (
	VerdictAllow PolicyVerdict = iota
	VerdictDeny
	VerdictRedirectProxy // L7 프록시로 리다이렉트
)

func (v PolicyVerdict) String() string {
	switch v {
	case VerdictAllow:
		return "ALLOW"
	case VerdictDeny:
		return "DENY"
	case VerdictRedirectProxy:
		return "REDIRECT_PROXY"
	default:
		return "UNKNOWN"
	}
}

// PolicyEntry는 BPF policymap의 엔트리이다
// Cilium의 struct policy_entry에 대응
type PolicyEntry struct {
	SrcIdentity SecurityIdentity
	DstPort     uint16
	Protocol    uint8
	Verdict     PolicyVerdict
	ProxyPort   uint16 // L7 프록시 포트 (리다이렉트 시)
}

// PolicyMap은 엔드포인트의 BPF 정책 맵을 시뮬레이션한다
// 실제 Cilium: cilium_policy_<endpoint_id>
type PolicyMap struct {
	defaultDeny bool
	entries     []PolicyEntry
}

// Check는 트래픽에 대한 정책을 검사한다
func (pm *PolicyMap) Check(srcIdentity SecurityIdentity, dstPort uint16, proto uint8) (PolicyVerdict, uint16) {
	// deny 규칙 우선 검사 (Cilium의 deny-first 원칙)
	for _, e := range pm.entries {
		if e.Verdict == VerdictDeny &&
			e.SrcIdentity == srcIdentity &&
			(e.DstPort == 0 || e.DstPort == dstPort) &&
			(e.Protocol == 0 || e.Protocol == proto) {
			return VerdictDeny, 0
		}
	}
	// allow 규칙 검사
	for _, e := range pm.entries {
		if e.Verdict != VerdictDeny &&
			e.SrcIdentity == srcIdentity &&
			(e.DstPort == 0 || e.DstPort == dstPort) &&
			(e.Protocol == 0 || e.Protocol == proto) {
			return e.Verdict, e.ProxyPort
		}
	}
	if pm.defaultDeny {
		return VerdictDeny, 0
	}
	return VerdictAllow, 0
}

// --- 라우팅 결정 ---

// RouteAction은 패킷의 다음 행동을 결정한다
type RouteAction int

const (
	RouteLocal  RouteAction = iota // 로컬 엔드포인트로 전달
	RouteTunnel                    // VXLAN/Geneve 터널로 전송
	RouteStack                     // 커널 네트워크 스택으로 전달
	RouteDrop                      // 패킷 드롭
)

func (r RouteAction) String() string {
	switch r {
	case RouteLocal:
		return "LOCAL_DELIVERY"
	case RouteTunnel:
		return "TUNNEL_ENCAP"
	case RouteStack:
		return "KERNEL_STACK"
	case RouteDrop:
		return "DROP"
	default:
		return "UNKNOWN"
	}
}

// --- 트레이스/알림 이벤트 ---

// TraceEvent는 데이터패스 트레이싱 이벤트이다
// cilium monitor에서 볼 수 있는 이벤트에 대응
type TraceEvent struct {
	Type    string
	Reason  string
	Tuple   Tuple5
	Verdict string
}

// DropNotification은 드롭 이벤트 알림이다
type DropNotification struct {
	Reason string
	Tuple  Tuple5
}

// --- 데이터패스 파이프라인 (tail call 시뮬레이션) ---

// DatapathContext는 eBPF 프로그램 간 공유되는 컨텍스트이다
// 실제 Cilium에서는 skb/xdp 메타데이터와 per-cpu 변수로 전달됨
type DatapathContext struct {
	Pkt         Packet
	Tuple       Tuple5
	SrcIdentity SecurityIdentity
	DstIdentity SecurityIdentity
	CTState     CTState
	CTEntry     *CTEntry
	Verdict     PolicyVerdict
	Action      RouteAction
	ProxyPort   uint16
	Traces      []TraceEvent
	Drops       []DropNotification
}

// TailCallFunc는 tail call로 호출되는 eBPF 프로그램을 시뮬레이션한다
// 실제 Cilium: bpf_tail_call(ctx, &cilium_calls_xdp, CILIUM_CALL_*)
type TailCallFunc func(ctx *DatapathContext) error

// DatapathPipeline은 eBPF tail call 체인을 시뮬레이션한다
type DatapathPipeline struct {
	ct       *CTTable
	policy   *PolicyMap
	localIPs map[string]bool // 로컬 엔드포인트 IP 목록
}

func NewDatapathPipeline(ct *CTTable, policy *PolicyMap) *DatapathPipeline {
	return &DatapathPipeline{
		ct:     ct,
		policy: policy,
		localIPs: map[string]bool{
			"10.0.0.10": true,
			"10.0.0.11": true,
		},
	}
}

// ProcessPacket은 전체 데이터패스를 실행한다
// 실제 Cilium: handle_xgress → tail call chain
func (dp *DatapathPipeline) ProcessPacket(pkt Packet, srcIdentity, dstIdentity SecurityIdentity) *DatapathContext {
	ctx := &DatapathContext{
		Pkt:         pkt,
		SrcIdentity: srcIdentity,
		DstIdentity: dstIdentity,
	}

	// tail call 체인 시뮬레이션
	// 실제 Cilium에서는 각 단계가 별도의 BPF 프로그램이며 bpf_tail_call()로 호출됨
	stages := []struct {
		name string
		fn   TailCallFunc
	}{
		{"classify_packet", dp.classifyPacket},
		{"extract_tuple", dp.extractTuple},
		{"ct_lookup", dp.ctLookup},
		{"policy_check", dp.policyCheck},
		{"routing_decision", dp.routingDecision},
	}

	for _, stage := range stages {
		if err := stage.fn(ctx); err != nil {
			ctx.Traces = append(ctx.Traces, TraceEvent{
				Type:   "TAIL_CALL",
				Reason: fmt.Sprintf("Stage '%s' failed: %v", stage.name, err),
			})
			break
		}
	}

	return ctx
}

// classifyPacket은 EtherType을 분류한다
// 실제 Cilium: validate_ethertype() in bpf/lib/eps.h
func (dp *DatapathPipeline) classifyPacket(ctx *DatapathContext) error {
	eth := ctx.Pkt.Eth
	switch eth.EtherType {
	case EtherTypeIPv4:
		ctx.Traces = append(ctx.Traces, TraceEvent{
			Type: "CLASSIFY", Reason: "IPv4 packet identified",
		})
		return nil
	case EtherTypeIPv6:
		ctx.Traces = append(ctx.Traces, TraceEvent{
			Type: "CLASSIFY", Reason: "IPv6 packet identified",
		})
		return nil
	case EtherTypeARP:
		ctx.Traces = append(ctx.Traces, TraceEvent{
			Type: "CLASSIFY", Reason: "ARP → pass to stack",
		})
		ctx.Action = RouteStack
		return fmt.Errorf("ARP: pass to kernel stack")
	default:
		ctx.Drops = append(ctx.Drops, DropNotification{
			Reason: fmt.Sprintf("Unknown EtherType: 0x%04X", eth.EtherType),
		})
		ctx.Action = RouteDrop
		return fmt.Errorf("unknown ethertype")
	}
}

// extractTuple은 패킷에서 5-튜플을 추출한다
// 실제 Cilium: ct_extract_tuple4() in bpf/lib/conntrack.h
func (dp *DatapathPipeline) extractTuple(ctx *DatapathContext) error {
	ctx.Tuple = Tuple5{
		SrcIP:    ctx.Pkt.IP.SrcIP,
		DstIP:    ctx.Pkt.IP.DstIP,
		SrcPort:  ctx.Pkt.L4.SrcPort,
		DstPort:  ctx.Pkt.L4.DstPort,
		Protocol: ctx.Pkt.IP.Protocol,
	}
	ctx.Traces = append(ctx.Traces, TraceEvent{
		Type:  "EXTRACT",
		Reason: "5-tuple extracted",
		Tuple: ctx.Tuple,
	})
	return nil
}

// ctLookup은 conntrack 테이블을 조회한다
// 실제 Cilium: ct_lookup4() in bpf/lib/conntrack.h
func (dp *DatapathPipeline) ctLookup(ctx *DatapathContext) error {
	entry, state := dp.ct.Lookup(ctx.Tuple)
	ctx.CTState = state
	ctx.CTEntry = entry

	if state == CTNew {
		// 새 연결: CT 엔트리 생성
		ctx.CTEntry = dp.ct.Create(ctx.Tuple)
		ctx.Traces = append(ctx.Traces, TraceEvent{
			Type:  "CT_LOOKUP",
			Reason: "New connection → CT entry created",
			Tuple: ctx.Tuple,
		})
	} else {
		ctx.Traces = append(ctx.Traces, TraceEvent{
			Type:    "CT_LOOKUP",
			Reason:  fmt.Sprintf("Connection state: %s", state),
			Tuple:   ctx.Tuple,
			Verdict: state.String(),
		})
		// 확립된 연결/응답: 통계 업데이트
		if entry != nil {
			entry.TxPkts++
			entry.TxBytes += uint64(len(ctx.Pkt.Data)) + 54 // 헤더 포함
		}
	}
	return nil
}

// policyCheck는 BPF policymap에서 정책을 검사한다
// 실제 Cilium: policy_can_access_ingress() / policy_can_egress()
func (dp *DatapathPipeline) policyCheck(ctx *DatapathContext) error {
	// 확립된 연결/응답은 정책 검사 건너뜀 (CT hit)
	if ctx.CTState == CTEstablished || ctx.CTState == CTReply {
		ctx.Verdict = VerdictAllow
		ctx.Traces = append(ctx.Traces, TraceEvent{
			Type:    "POLICY",
			Reason:  "CT hit → skip policy check",
			Verdict: "ALLOW (CT)",
		})
		return nil
	}

	// 새 연결: policymap 조회
	verdict, proxyPort := dp.policy.Check(
		ctx.SrcIdentity, ctx.Tuple.DstPort, ctx.Tuple.Protocol,
	)
	ctx.Verdict = verdict
	ctx.ProxyPort = proxyPort

	ctx.Traces = append(ctx.Traces, TraceEvent{
		Type: "POLICY",
		Reason: fmt.Sprintf("identity=%d port=%d proto=%d",
			ctx.SrcIdentity, ctx.Tuple.DstPort, ctx.Tuple.Protocol),
		Verdict: verdict.String(),
	})

	if verdict == VerdictDeny {
		ctx.Action = RouteDrop
		ctx.Drops = append(ctx.Drops, DropNotification{
			Reason: fmt.Sprintf("Policy DENY: src_id=%d dst_port=%d",
				ctx.SrcIdentity, ctx.Tuple.DstPort),
			Tuple: ctx.Tuple,
		})
		return fmt.Errorf("policy denied")
	}

	return nil
}

// routingDecision은 패킷의 다음 홉을 결정한다
// 실제 Cilium: 로컬이면 ipv4_local_delivery(), 원격이면 encap_and_redirect()
func (dp *DatapathPipeline) routingDecision(ctx *DatapathContext) error {
	dstIP := ctx.Tuple.DstIP.String()

	if ctx.Verdict == VerdictRedirectProxy {
		ctx.Action = RouteLocal
		ctx.Traces = append(ctx.Traces, TraceEvent{
			Type:   "ROUTE",
			Reason: fmt.Sprintf("Redirect to L7 proxy port %d", ctx.ProxyPort),
		})
		return nil
	}

	if dp.localIPs[dstIP] {
		ctx.Action = RouteLocal
		ctx.Traces = append(ctx.Traces, TraceEvent{
			Type:   "ROUTE",
			Reason: "Destination is local endpoint",
		})
	} else {
		ctx.Action = RouteTunnel
		ctx.Traces = append(ctx.Traces, TraceEvent{
			Type:   "ROUTE",
			Reason: "Destination is remote → tunnel encapsulation",
		})
	}
	return nil
}

// --- 헬퍼 함수 ---

func randomMAC() net.HardwareAddr {
	mac := make([]byte, 6)
	binary.BigEndian.PutUint32(mac[2:], rand.Uint32())
	mac[0] = 0x02 // locally administered
	return mac
}

func makePacket(srcIP, dstIP string, srcPort, dstPort uint16, proto uint8) Packet {
	return Packet{
		Eth: EthHeader{
			DstMAC:    randomMAC(),
			SrcMAC:    randomMAC(),
			EtherType: EtherTypeIPv4,
		},
		IP: IPv4Header{
			SrcIP:    net.ParseIP(srcIP),
			DstIP:    net.ParseIP(dstIP),
			Protocol: proto,
			TTL:      64,
		},
		L4: L4Header{
			SrcPort: srcPort,
			DstPort: dstPort,
		},
		Data: []byte("Hello from Cilium eBPF datapath"),
	}
}

func printResult(ctx *DatapathContext) {
	fmt.Println(strings.Repeat("─", 70))
	fmt.Printf("  Packet: %s\n", ctx.Tuple)
	fmt.Printf("  Source Identity: %d  |  Dest Identity: %d\n", ctx.SrcIdentity, ctx.DstIdentity)
	fmt.Println()
	for i, t := range ctx.Traces {
		fmt.Printf("  [%d] %-12s %s", i+1, t.Type, t.Reason)
		if t.Verdict != "" {
			fmt.Printf("  → %s", t.Verdict)
		}
		fmt.Println()
	}
	if len(ctx.Drops) > 0 {
		fmt.Println()
		for _, d := range ctx.Drops {
			fmt.Printf("  ⛔ DROP: %s\n", d.Reason)
		}
	}
	fmt.Printf("\n  Final Action: %s\n", ctx.Action)
	fmt.Println(strings.Repeat("─", 70))
}

// =============================================================================
// main
// =============================================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║        Cilium eBPF Datapath 시뮬레이션                              ║")
	fmt.Println("║  패킷 분류 → CT 조회 → 정책 검사 → 라우팅 결정                      ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════════╝")

	// Conntrack 테이블 초기화
	ct := NewCTTable()

	// 정책맵 초기화: Frontend(1002) → App(1000):80/TCP 허용, World(2) → 차단
	policy := &PolicyMap{
		defaultDeny: true,
		entries: []PolicyEntry{
			{SrcIdentity: IdentityFrontend, DstPort: 80, Protocol: ProtoTCP, Verdict: VerdictAllow},
			{SrcIdentity: IdentityApp, DstPort: 3306, Protocol: ProtoTCP, Verdict: VerdictAllow},
			{SrcIdentity: IdentityFrontend, DstPort: 8080, Protocol: ProtoTCP,
				Verdict: VerdictRedirectProxy, ProxyPort: 15001},
			{SrcIdentity: IdentityWorld, DstPort: 0, Protocol: 0, Verdict: VerdictDeny},
		},
	}

	dp := NewDatapathPipeline(ct, policy)

	// --- 시나리오 1: 허용되는 새 연결 ---
	fmt.Println("\n[시나리오 1] Frontend → App:80/TCP (새 연결, 허용)")
	pkt1 := makePacket("10.0.1.5", "10.0.0.10", 45678, 80, ProtoTCP)
	result1 := dp.ProcessPacket(pkt1, IdentityFrontend, IdentityApp)
	printResult(result1)

	// --- 시나리오 2: 동일 연결의 후속 패킷 (CT established) ---
	fmt.Println("\n[시나리오 2] Frontend → App:80/TCP (후속 패킷, CT established)")
	pkt2 := makePacket("10.0.1.5", "10.0.0.10", 45678, 80, ProtoTCP)
	result2 := dp.ProcessPacket(pkt2, IdentityFrontend, IdentityApp)
	printResult(result2)

	// --- 시나리오 3: 응답 패킷 (CT reply) ---
	fmt.Println("\n[시나리오 3] App → Frontend (응답 패킷, CT reply)")
	pkt3 := makePacket("10.0.0.10", "10.0.1.5", 80, 45678, ProtoTCP)
	result3 := dp.ProcessPacket(pkt3, IdentityApp, IdentityFrontend)
	printResult(result3)

	// --- 시나리오 4: 거부되는 트래픽 ---
	fmt.Println("\n[시나리오 4] World → App:80/TCP (외부에서 접근, 거부)")
	pkt4 := makePacket("203.0.113.1", "10.0.0.10", 12345, 80, ProtoTCP)
	result4 := dp.ProcessPacket(pkt4, IdentityWorld, IdentityApp)
	printResult(result4)

	// --- 시나리오 5: L7 프록시 리다이렉트 ---
	fmt.Println("\n[시나리오 5] Frontend → App:8080/TCP (L7 프록시 리다이렉트)")
	pkt5 := makePacket("10.0.1.5", "10.0.0.10", 55555, 8080, ProtoTCP)
	result5 := dp.ProcessPacket(pkt5, IdentityFrontend, IdentityApp)
	printResult(result5)

	// --- 시나리오 6: 원격 노드로의 터널 전송 ---
	fmt.Println("\n[시나리오 6] App → RemoteDB:3306/TCP (원격 노드, 터널 경유)")
	pkt6 := makePacket("10.0.0.10", "10.0.2.20", 33333, 3306, ProtoTCP)
	result6 := dp.ProcessPacket(pkt6, IdentityApp, IdentityDB)
	printResult(result6)

	// --- 시나리오 7: ARP 패킷 (커널 스택으로 전달) ---
	fmt.Println("\n[시나리오 7] ARP 패킷 (커널 스택으로 전달)")
	arpPkt := Packet{
		Eth: EthHeader{DstMAC: randomMAC(), SrcMAC: randomMAC(), EtherType: EtherTypeARP},
	}
	result7 := dp.ProcessPacket(arpPkt, 0, 0)
	printResult(result7)

	// --- CT 테이블 상태 출력 ---
	fmt.Println("\n[Conntrack 테이블 상태]")
	fmt.Printf("  활성 엔트리 수: %d\n", len(ct.entries))
	for key, entry := range ct.entries {
		fmt.Printf("  %-50s state=%-12s tx_pkts=%d tx_bytes=%d\n",
			key, entry.State, entry.TxPkts, entry.TxBytes)
	}
	fmt.Println()
}
