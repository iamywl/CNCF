package main

import (
	"fmt"
	"strings"
	"time"
)

// =============================================================================
// Hubble 데이터 모델 시뮬레이션
//
// 실제 구현 참조:
//   - api/v1/flow/flow.pb.go: Flow, Endpoint, Layer4, Layer7, Verdict 등 protobuf 타입
//   - pkg/hubble/api/v1/types.go: Event 래퍼, GetFlow(), GetAgentEvent() 등
//   - pkg/hubble/observer/types/types.go: MonitorEvent, PerfEvent, AgentEvent, LostEvent
//
// Hubble의 데이터 모델 계층:
//   MonitorEvent (최상위) → Decode → Event{Timestamp, Event: any}
//     ├── Event.Event = *Flow (네트워크 플로우)
//     ├── Event.Event = *AgentEvent (에이전트 이벤트)
//     ├── Event.Event = *DebugEvent (디버그 이벤트)
//     └── Event.Event = *LostEvent (유실 이벤트)
// =============================================================================

// -----------------------------------------------------------------------------
// Verdict enum (실제: flow.Verdict)
// Flow의 처리 결과를 나타내는 열거형
// -----------------------------------------------------------------------------

type Verdict int32

const (
	VerdictUnknown   Verdict = 0
	VerdictForwarded Verdict = 1
	VerdictDropped   Verdict = 2
	VerdictError     Verdict = 3
	VerdictAudit     Verdict = 4
	VerdictRedirected Verdict = 5
	VerdictTraced    Verdict = 6
	VerdictTranslated Verdict = 7
)

var verdictNames = map[Verdict]string{
	VerdictUnknown:    "UNKNOWN",
	VerdictForwarded:  "FORWARDED",
	VerdictDropped:    "DROPPED",
	VerdictError:      "ERROR",
	VerdictAudit:      "AUDIT",
	VerdictRedirected: "REDIRECTED",
	VerdictTraced:     "TRACED",
	VerdictTranslated: "TRANSLATED",
}

func (v Verdict) String() string {
	if name, ok := verdictNames[v]; ok {
		return name
	}
	return "UNKNOWN"
}

// -----------------------------------------------------------------------------
// FlowType enum (실제: flow.FlowType)
// -----------------------------------------------------------------------------

type FlowType int32

const (
	FlowTypeUnknown FlowType = 0
	FlowTypeL3L4    FlowType = 1
	FlowTypeL7      FlowType = 2
	FlowTypeSOCK    FlowType = 3
)

var flowTypeNames = map[FlowType]string{
	FlowTypeUnknown: "UNKNOWN_TYPE",
	FlowTypeL3L4:    "L3_L4",
	FlowTypeL7:      "L7",
	FlowTypeSOCK:    "SOCK",
}

func (ft FlowType) String() string {
	if name, ok := flowTypeNames[ft]; ok {
		return name
	}
	return "UNKNOWN_TYPE"
}

// -----------------------------------------------------------------------------
// TrafficDirection enum (실제: flow.TrafficDirection)
// -----------------------------------------------------------------------------

type TrafficDirection int32

const (
	TrafficDirectionUnknown TrafficDirection = 0
	TrafficDirectionIngress TrafficDirection = 1
	TrafficDirectionEgress  TrafficDirection = 2
)

func (td TrafficDirection) String() string {
	switch td {
	case TrafficDirectionIngress:
		return "INGRESS"
	case TrafficDirectionEgress:
		return "EGRESS"
	default:
		return "TRAFFIC_DIRECTION_UNKNOWN"
	}
}

// -----------------------------------------------------------------------------
// DropReason enum (실제: flow.DropReason)
// 패킷이 DROP된 이유
// -----------------------------------------------------------------------------

type DropReason int32

const (
	DropReasonUnspecified DropReason = 0
	DropReasonPolicy     DropReason = 181
	DropReasonAuthRequired DropReason = 203
)

func (dr DropReason) String() string {
	switch dr {
	case DropReasonPolicy:
		return "POLICY_DENIED"
	case DropReasonAuthRequired:
		return "AUTH_REQUIRED"
	default:
		return "DROP_REASON_UNKNOWN"
	}
}

// -----------------------------------------------------------------------------
// Endpoint (실제: flow.Endpoint)
// 네트워크 통신의 출발지/목적지를 나타내는 구조체
// 실제 필드: ID, Identity, Namespace, Labels, PodName, Workloads 등
// -----------------------------------------------------------------------------

type Endpoint struct {
	ID        uint32
	Identity  uint32
	Namespace string
	Labels    []string
	PodName   string
	Workloads []Workload
}

type Workload struct {
	Name string
	Kind string
}

func (e *Endpoint) String() string {
	if e == nil {
		return "<nil>"
	}
	labels := strings.Join(e.Labels, ",")
	return fmt.Sprintf("{ID:%d Identity:%d NS:%s Pod:%s Labels:[%s]}",
		e.ID, e.Identity, e.Namespace, e.PodName, labels)
}

// -----------------------------------------------------------------------------
// IP (실제: flow.IP)
// L3 IP 주소 정보
// -----------------------------------------------------------------------------

type IP struct {
	Source      string
	Destination string
	IPVersion   int // 4 or 6
}

// -----------------------------------------------------------------------------
// Ethernet (실제: flow.Ethernet)
// L2 이더넷 프레임 정보
// -----------------------------------------------------------------------------

type Ethernet struct {
	Source      string
	Destination string
}

// -----------------------------------------------------------------------------
// Layer4 (실제: flow.Layer4)
// L4 프로토콜 정보 - oneof로 TCP/UDP/ICMPv4/ICMPv6/SCTP 중 하나
// -----------------------------------------------------------------------------

type Layer4 struct {
	Protocol string // "TCP", "UDP", "ICMPv4", "ICMPv6", "SCTP"
	// TCP/UDP의 경우
	SourcePort      uint32
	DestinationPort uint32
	// TCP 전용 플래그
	TCPFlags *TCPFlags
}

type TCPFlags struct {
	SYN bool
	ACK bool
	FIN bool
	RST bool
	PSH bool
}

func (tf *TCPFlags) String() string {
	if tf == nil {
		return ""
	}
	var flags []string
	if tf.SYN {
		flags = append(flags, "SYN")
	}
	if tf.ACK {
		flags = append(flags, "ACK")
	}
	if tf.FIN {
		flags = append(flags, "FIN")
	}
	if tf.RST {
		flags = append(flags, "RST")
	}
	if tf.PSH {
		flags = append(flags, "PSH")
	}
	return strings.Join(flags, "|")
}

// -----------------------------------------------------------------------------
// Layer7 (실제: flow.Layer7)
// L7 프로토콜 정보 - oneof로 HTTP/DNS/Kafka 중 하나
// -----------------------------------------------------------------------------

type Layer7 struct {
	Type      string // "HTTP", "DNS", "Kafka"
	Latency   time.Duration
	Record    any // L7 상세 데이터
}

type HTTPRecord struct {
	Method   string
	URL      string
	Code     uint32
	Protocol string
}

type DNSRecord struct {
	Query string
	RCode uint32
	QTypes []string
	IPs   []string
}

// -----------------------------------------------------------------------------
// CiliumEventType (실제: flow.CiliumEventType)
// Cilium 모니터 이벤트 타입
// -----------------------------------------------------------------------------

type CiliumEventType struct {
	Type    int32
	SubType int32
}

// -----------------------------------------------------------------------------
// Service (실제: flow.Service)
// Kubernetes 서비스 정보
// -----------------------------------------------------------------------------

type ServiceInfo struct {
	Name      string
	Namespace string
}

// -----------------------------------------------------------------------------
// Flow (실제: flow.Flow - 40개 이상의 필드)
// Hubble의 핵심 데이터 모델. 네트워크 트래픽의 모든 정보를 담는 구조체
// -----------------------------------------------------------------------------

type Flow struct {
	// 시간 및 식별
	Time     time.Time
	UUID     string
	NodeName string

	// 판정 결과
	Verdict        Verdict
	DropReasonDesc DropReason

	// L2
	Ethernet *Ethernet

	// L3
	IP *IP

	// L4
	L4 *Layer4

	// 소스/목적지 엔드포인트
	Source      *Endpoint
	Destination *Endpoint

	// 플로우 타입
	Type FlowType

	// DNS 이름
	SourceNames      []string
	DestinationNames []string

	// L7 정보 (FlowType이 L7일 때만 설정)
	L7 *Layer7

	// 이벤트 타입
	EventType *CiliumEventType

	// 서비스 정보
	SourceService      *ServiceInfo
	DestinationService *ServiceInfo

	// 트래픽 방향
	TrafficDirection TrafficDirection

	// 노드 레이블
	NodeLabels []string

	// Reply 여부
	IsReply *bool
}

func (f *Flow) String() string {
	var parts []string
	parts = append(parts, fmt.Sprintf("Time: %s", f.Time.Format("15:04:05.000")))
	parts = append(parts, fmt.Sprintf("Verdict: %s", f.Verdict))
	parts = append(parts, fmt.Sprintf("Type: %s", f.Type))
	parts = append(parts, fmt.Sprintf("Direction: %s", f.TrafficDirection))

	if f.IP != nil {
		parts = append(parts, fmt.Sprintf("IP: %s -> %s", f.IP.Source, f.IP.Destination))
	}
	if f.L4 != nil {
		parts = append(parts, fmt.Sprintf("L4: %s %d -> %d",
			f.L4.Protocol, f.L4.SourcePort, f.L4.DestinationPort))
		if f.L4.TCPFlags != nil {
			parts = append(parts, fmt.Sprintf("Flags: %s", f.L4.TCPFlags))
		}
	}
	if f.Source != nil {
		parts = append(parts, fmt.Sprintf("Src: %s/%s", f.Source.Namespace, f.Source.PodName))
	}
	if f.Destination != nil {
		parts = append(parts, fmt.Sprintf("Dst: %s/%s", f.Destination.Namespace, f.Destination.PodName))
	}

	return strings.Join(parts, " | ")
}

// -----------------------------------------------------------------------------
// Event 래퍼 (실제: pkg/hubble/api/v1/types.go의 Event)
// 타임스탬프와 any 타입의 이벤트를 감싸는 래퍼
// GetFlow(), GetAgentEvent(), GetDebugEvent(), GetLostEvent()로 타입 추출
// -----------------------------------------------------------------------------

type Event struct {
	Timestamp time.Time
	Event     any // *Flow, *AgentEvent, *DebugEvent, *LostEvent 중 하나
}

// GetFlow는 이벤트에서 Flow를 추출 (nil이면 nil 반환)
// 실제: func (ev *Event) GetFlow() *pb.Flow
func (ev *Event) GetFlow() *Flow {
	if ev == nil || ev.Event == nil {
		return nil
	}
	if f, ok := ev.Event.(*Flow); ok {
		return f
	}
	return nil
}

// AgentEvent는 에이전트 이벤트를 나타냄
type AgentEvent struct {
	Type    string
	Message string
}

// GetAgentEvent는 이벤트에서 AgentEvent를 추출
// 실제: func (ev *Event) GetAgentEvent() *pb.AgentEvent
func (ev *Event) GetAgentEvent() *AgentEvent {
	if ev == nil || ev.Event == nil {
		return nil
	}
	if a, ok := ev.Event.(*AgentEvent); ok {
		return a
	}
	return nil
}

// LostEvent는 유실된 이벤트를 나타냄
// 실제: flow.LostEvent
type LostEvent struct {
	Source        string
	NumEventsLost uint64
}

// GetLostEvent는 이벤트에서 LostEvent를 추출
// 실제: func (ev *Event) GetLostEvent() *pb.LostEvent
func (ev *Event) GetLostEvent() *LostEvent {
	if ev == nil || ev.Event == nil {
		return nil
	}
	if l, ok := ev.Event.(*LostEvent); ok {
		return l
	}
	return nil
}

// -----------------------------------------------------------------------------
// MonitorEvent (실제: pkg/hubble/observer/types/types.go)
// 최상위 이벤트 타입. 파서에 의해 Event로 디코딩됨
// Payload: *PerfEvent, *AgentEvent, *LostEvent 중 하나
// -----------------------------------------------------------------------------

type MonitorEvent struct {
	UUID      string
	Timestamp time.Time
	NodeName  string
	Payload   any
}

type PerfEvent struct {
	Data []byte
	CPU  int
}

type MonitorLostEvent struct {
	Source        int
	NumLostEvents uint64
	CPU           int
}

// =============================================================================
// 메인 실행: 다양한 데이터 모델 인스턴스 생성 및 출력
// =============================================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║     Hubble 데이터 모델 시뮬레이션                            ║")
	fmt.Println("║     참조: api/v1/flow/flow.pb.go                            ║")
	fmt.Println("║           pkg/hubble/api/v1/types.go                        ║")
	fmt.Println("║           pkg/hubble/observer/types/types.go                ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")

	now := time.Now()

	// --- 예제 1: L3/L4 TCP Flow (FORWARDED) ---
	fmt.Println("\n=== 예제 1: L3/L4 TCP Flow (FORWARDED) ===")
	isReply := false
	tcpFlow := &Flow{
		Time:     now,
		UUID:     "550e8400-e29b-41d4-a716-446655440001",
		NodeName: "k8s-worker-01",
		Verdict:  VerdictForwarded,
		Type:     FlowTypeL3L4,
		Ethernet: &Ethernet{
			Source:      "0a:58:0a:f4:00:01",
			Destination: "0a:58:0a:f4:00:02",
		},
		IP: &IP{
			Source:      "10.244.0.5",
			Destination: "10.244.1.10",
			IPVersion:   4,
		},
		L4: &Layer4{
			Protocol:        "TCP",
			SourcePort:      54321,
			DestinationPort: 8080,
			TCPFlags:        &TCPFlags{SYN: true},
		},
		Source: &Endpoint{
			ID:        1234,
			Identity:  100,
			Namespace: "default",
			PodName:   "frontend-7d9b5c8f4-abc12",
			Labels:    []string{"k8s:app=frontend", "k8s:io.kubernetes.pod.namespace=default"},
			Workloads: []Workload{{Name: "frontend", Kind: "Deployment"}},
		},
		Destination: &Endpoint{
			ID:        5678,
			Identity:  200,
			Namespace: "default",
			PodName:   "backend-6c5d7f8b9-xyz99",
			Labels:    []string{"k8s:app=backend", "k8s:io.kubernetes.pod.namespace=default"},
			Workloads: []Workload{{Name: "backend", Kind: "Deployment"}},
		},
		TrafficDirection: TrafficDirectionEgress,
		IsReply:          &isReply,
		NodeLabels:       []string{"kubernetes.io/os=linux", "node-role.kubernetes.io/worker="},
		SourceService:    &ServiceInfo{Name: "frontend", Namespace: "default"},
		DestinationService: &ServiceInfo{Name: "backend", Namespace: "default"},
	}
	fmt.Println(tcpFlow)
	fmt.Printf("  소스 엔드포인트: %s\n", tcpFlow.Source)
	fmt.Printf("  목적지 엔드포인트: %s\n", tcpFlow.Destination)

	// --- 예제 2: L7 HTTP Flow ---
	fmt.Println("\n=== 예제 2: L7 HTTP Flow ===")
	httpFlow := &Flow{
		Time:     now.Add(1 * time.Millisecond),
		UUID:     "550e8400-e29b-41d4-a716-446655440002",
		NodeName: "k8s-worker-01",
		Verdict:  VerdictForwarded,
		Type:     FlowTypeL7,
		IP: &IP{
			Source:      "10.244.0.5",
			Destination: "10.244.1.10",
			IPVersion:   4,
		},
		L4: &Layer4{
			Protocol:        "TCP",
			SourcePort:      54321,
			DestinationPort: 8080,
		},
		L7: &Layer7{
			Type:    "HTTP",
			Latency: 15 * time.Millisecond,
			Record: &HTTPRecord{
				Method:   "GET",
				URL:      "/api/v1/products",
				Code:     200,
				Protocol: "HTTP/1.1",
			},
		},
		Source: &Endpoint{
			Namespace: "default",
			PodName:   "frontend-7d9b5c8f4-abc12",
		},
		Destination: &Endpoint{
			Namespace: "default",
			PodName:   "backend-6c5d7f8b9-xyz99",
		},
		TrafficDirection: TrafficDirectionEgress,
	}
	fmt.Println(httpFlow)
	if httpFlow.L7 != nil {
		if http, ok := httpFlow.L7.Record.(*HTTPRecord); ok {
			fmt.Printf("  HTTP: %s %s -> %d (%s), 지연: %v\n",
				http.Method, http.URL, http.Code, http.Protocol, httpFlow.L7.Latency)
		}
	}

	// --- 예제 3: DROPPED Flow ---
	fmt.Println("\n=== 예제 3: DROPPED Flow (정책 거부) ===")
	droppedFlow := &Flow{
		Time:           now.Add(2 * time.Millisecond),
		UUID:           "550e8400-e29b-41d4-a716-446655440003",
		NodeName:       "k8s-worker-02",
		Verdict:        VerdictDropped,
		DropReasonDesc: DropReasonPolicy,
		Type:           FlowTypeL3L4,
		IP: &IP{
			Source:      "10.244.2.15",
			Destination: "10.244.1.10",
			IPVersion:   4,
		},
		L4: &Layer4{
			Protocol:        "TCP",
			SourcePort:      44444,
			DestinationPort: 3306,
		},
		Source: &Endpoint{
			Namespace: "untrusted",
			PodName:   "attacker-pod-xyz",
			Labels:    []string{"k8s:app=unknown"},
		},
		Destination: &Endpoint{
			Namespace: "database",
			PodName:   "mysql-0",
			Labels:    []string{"k8s:app=mysql", "k8s:tier=database"},
		},
		TrafficDirection: TrafficDirectionIngress,
	}
	fmt.Println(droppedFlow)
	fmt.Printf("  드롭 사유: %s\n", droppedFlow.DropReasonDesc)

	// --- 예제 4: DNS Flow ---
	fmt.Println("\n=== 예제 4: DNS L7 Flow ===")
	dnsFlow := &Flow{
		Time:     now.Add(3 * time.Millisecond),
		UUID:     "550e8400-e29b-41d4-a716-446655440004",
		NodeName: "k8s-worker-01",
		Verdict:  VerdictForwarded,
		Type:     FlowTypeL7,
		IP: &IP{
			Source:      "10.244.0.5",
			Destination: "10.96.0.10",
			IPVersion:   4,
		},
		L4: &Layer4{
			Protocol:        "UDP",
			SourcePort:      45678,
			DestinationPort: 53,
		},
		L7: &Layer7{
			Type:    "DNS",
			Latency: 2 * time.Millisecond,
			Record: &DNSRecord{
				Query:  "backend.default.svc.cluster.local.",
				RCode:  0,
				QTypes: []string{"A"},
				IPs:    []string{"10.244.1.10"},
			},
		},
		DestinationNames: []string{"kube-dns"},
		TrafficDirection:  TrafficDirectionEgress,
	}
	fmt.Println(dnsFlow)
	if dnsFlow.L7 != nil {
		if dns, ok := dnsFlow.L7.Record.(*DNSRecord); ok {
			fmt.Printf("  DNS: %s -> [%s] (RCode: %d), 지연: %v\n",
				dns.Query, strings.Join(dns.IPs, ","), dns.RCode, dnsFlow.L7.Latency)
		}
	}

	// --- 예제 5: Event 래퍼 및 타입 추출 ---
	fmt.Println("\n=== 예제 5: Event 래퍼 및 타입 추출 ===")

	events := []*Event{
		{Timestamp: now, Event: tcpFlow},
		{Timestamp: now.Add(1 * time.Millisecond), Event: &AgentEvent{Type: "agent", Message: "Endpoint regenerated"}},
		{Timestamp: now.Add(2 * time.Millisecond), Event: &LostEvent{Source: "HUBBLE_RING_BUFFER", NumEventsLost: 5}},
	}

	for i, ev := range events {
		fmt.Printf("\n  이벤트 #%d (시간: %s):\n", i+1, ev.Timestamp.Format("15:04:05.000"))

		if f := ev.GetFlow(); f != nil {
			fmt.Printf("    타입: Flow | %s | %s -> %s\n",
				f.Verdict, f.Source.PodName, f.Destination.PodName)
		}
		if a := ev.GetAgentEvent(); a != nil {
			fmt.Printf("    타입: AgentEvent | %s: %s\n", a.Type, a.Message)
		}
		if l := ev.GetLostEvent(); l != nil {
			fmt.Printf("    타입: LostEvent | 소스: %s, 유실: %d\n", l.Source, l.NumEventsLost)
		}
	}

	// --- 예제 6: MonitorEvent (파서 입력) ---
	fmt.Println("\n=== 예제 6: MonitorEvent (파서 입력 전 원시 이벤트) ===")
	monitorEvents := []MonitorEvent{
		{
			UUID:      "uuid-001",
			Timestamp: now,
			NodeName:  "k8s-worker-01",
			Payload:   &PerfEvent{Data: []byte{0x04, 0x00, 0x02, 0x00}, CPU: 3}, // MessageTypeDrop
		},
		{
			UUID:      "uuid-002",
			Timestamp: now.Add(1 * time.Millisecond),
			NodeName:  "k8s-worker-01",
			Payload: &MonitorLostEvent{
				Source:        1, // PerfRingBuffer
				NumLostEvents: 10,
				CPU:           2,
			},
		},
	}
	for _, me := range monitorEvents {
		fmt.Printf("  MonitorEvent UUID=%s Node=%s ", me.UUID, me.NodeName)
		switch p := me.Payload.(type) {
		case *PerfEvent:
			fmt.Printf("Payload=PerfEvent(Data=%v, CPU=%d)\n", p.Data, p.CPU)
		case *MonitorLostEvent:
			fmt.Printf("Payload=LostEvent(Source=%d, Lost=%d, CPU=%d)\n",
				p.Source, p.NumLostEvents, p.CPU)
		}
	}

	// 데이터 모델 계층 다이어그램
	fmt.Println("\n" + `
┌──────────────────────────────────────────────────────────────────────┐
│                    Hubble 데이터 모델 계층                            │
├──────────────────────────────────────────────────────────────────────┤
│                                                                      │
│  MonitorEvent (관찰 원시 이벤트)                                     │
│  ├── UUID, Timestamp, NodeName                                       │
│  └── Payload: any                                                    │
│       ├── *PerfEvent{Data []byte, CPU int}     ← BPF perf ring      │
│       ├── *AgentEvent{Type int, Message any}   ← 에이전트 메시지     │
│       └── *LostEvent{Source, NumLostEvents}    ← 유실 이벤트        │
│                                                                      │
│            │ Decode (Parser)                                         │
│            V                                                         │
│                                                                      │
│  Event (디코딩된 이벤트 래퍼)                                        │
│  ├── Timestamp                                                       │
│  └── Event: any                                                      │
│       ├── *Flow (40+ 필드)                                           │
│       │    ├── Time, UUID, NodeName                                  │
│       │    ├── Verdict (FORWARDED|DROPPED|...)                       │
│       │    ├── Ethernet{Src, Dst}              ← L2                  │
│       │    ├── IP{Src, Dst, Version}           ← L3                  │
│       │    ├── Layer4{Protocol, Ports, Flags}  ← L4                  │
│       │    ├── Layer7{HTTP|DNS|Kafka}           ← L7                 │
│       │    ├── Source/Destination *Endpoint     ← Pod 정보           │
│       │    ├── TrafficDirection                 ← Ingress/Egress     │
│       │    └── DropReasonDesc, IsReply ...      ← 메타데이터         │
│       ├── *AgentEvent                                                │
│       ├── *DebugEvent                                                │
│       └── *LostEvent{Source, NumEventsLost}                          │
│                                                                      │
└──────────────────────────────────────────────────────────────────────┘`)
}
