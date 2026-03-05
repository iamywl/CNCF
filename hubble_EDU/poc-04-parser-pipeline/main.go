package main

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"strings"
	"time"
)

// =============================================================================
// Hubble 파서 파이프라인 시뮬레이션
//
// 실제 구현 참조:
//   - pkg/hubble/parser/parser.go: Parser 구조체, Decode() 메서드
//   - pkg/hubble/parser/threefour/parser.go: L3/L4 파서
//   - pkg/hubble/parser/seven/parser.go: L7 파서
//   - pkg/hubble/parser/debug/parser.go: 디버그 이벤트 파서
//   - pkg/hubble/parser/sock/parser.go: 소켓 이벤트 파서
//
// 파싱 흐름:
//   MonitorEvent → Parser.Decode()
//     ├── PerfEvent payload:
//     │     ├── Data[0] == MessageTypeDebug  → dbg.Decode()   → DebugEvent
//     │     ├── Data[0] == MessageTypeTraceSock → sock.Decode() → Flow
//     │     └── 기타                          → l34.Decode()  → Flow
//     ├── AgentEvent payload:
//     │     ├── Type == MessageTypeAccessLog  → l7.Decode()   → Flow
//     │     └── Type == MessageTypeAgent      → AgentEvent
//     └── LostEvent payload                  → LostEvent
// =============================================================================

// Cilium 모니터 메시지 타입 상수
// 실제: pkg/monitor/api/types.go
const (
	MessageTypeDrop         = 1
	MessageTypeDebug        = 4
	MessageTypeCapture      = 5
	MessageTypeTrace        = 6
	MessageTypePolicyVerdict = 11
	MessageTypeTraceSock    = 14
	MessageTypeAccessLog    = 128
	MessageTypeAgent        = 129
)

var messageTypeNames = map[int]string{
	MessageTypeDrop:          "DROP",
	MessageTypeDebug:         "DEBUG",
	MessageTypeCapture:       "CAPTURE",
	MessageTypeTrace:         "TRACE",
	MessageTypePolicyVerdict: "POLICY_VERDICT",
	MessageTypeTraceSock:     "TRACE_SOCK",
	MessageTypeAccessLog:     "ACCESS_LOG",
	MessageTypeAgent:         "AGENT",
}

// ── 데이터 모델 ──

type Verdict int
const (
	VerdictForwarded Verdict = 1
	VerdictDropped   Verdict = 2
)

func (v Verdict) String() string {
	switch v {
	case VerdictForwarded: return "FORWARDED"
	case VerdictDropped:   return "DROPPED"
	default:               return "UNKNOWN"
	}
}

type Flow struct {
	Time        time.Time
	UUID        string
	NodeName    string
	Verdict     Verdict
	Type        string // "L3_L4", "L7", "SOCK"
	SrcIP       string
	DstIP       string
	SrcPort     uint16
	DstPort     uint16
	Protocol    string
	L7Type      string
	L7Detail    string
	EventType   int
	EventSubType int
}

func (f *Flow) String() string {
	base := fmt.Sprintf("[%s] %s %s %s:%d -> %s:%d (%s)",
		f.Verdict, f.Type, f.Protocol, f.SrcIP, f.SrcPort, f.DstIP, f.DstPort, f.NodeName)
	if f.L7Type != "" {
		return base + fmt.Sprintf(" | L7: %s %s", f.L7Type, f.L7Detail)
	}
	return base
}

type DebugEvent struct {
	CPU     int
	Message string
}

type AgentNotification struct {
	Type    string
	Message string
}

type LostEvent struct {
	Source        string
	NumEventsLost uint64
	CPU           int
}

// Event는 파싱 결과를 감싸는 래퍼
type Event struct {
	Timestamp time.Time
	Event     any // *Flow, *DebugEvent, *AgentNotification, *LostEvent
}

// ── MonitorEvent (파서 입력) ──

type MonitorEvent struct {
	UUID      string
	Timestamp time.Time
	NodeName  string
	Payload   any // *PerfEvent, *AgentEvent, *MonitorLostEvent
}

type PerfEvent struct {
	Data []byte
	CPU  int
}

type AgentEvent struct {
	Type    int
	Message any
}

type MonitorLostEvent struct {
	Source        int
	NumLostEvents uint64
	CPU           int
}

// ── L3/L4 파서 (실제: pkg/hubble/parser/threefour/parser.go) ──
// 바이트 데이터에서 이더넷/IP/TCP/UDP 헤더를 디코딩

type L34Parser struct{}

// Decode는 PerfEvent 데이터를 L3/L4 Flow로 파싱
// 실제 흐름: gopacket.DecodingLayerParser로 이더넷 → IP → TCP/UDP 디코딩
func (p *L34Parser) Decode(data []byte, flow *Flow) error {
	if len(data) < 20 {
		return fmt.Errorf("데이터가 너무 짧음: %d bytes", len(data))
	}

	// 메시지 타입 확인 (data[0])
	msgType := int(data[0])
	flow.EventType = msgType

	// 시뮬레이션: 실제로는 gopacket으로 이더넷/IP/TCP 헤더를 파싱
	// 여기서는 간단히 바이트에서 IP/포트를 추출
	switch msgType {
	case MessageTypeDrop:
		flow.Verdict = VerdictDropped
	case MessageTypeTrace:
		flow.Verdict = VerdictForwarded
	case MessageTypePolicyVerdict:
		flow.Verdict = VerdictForwarded
	default:
		flow.Verdict = VerdictForwarded
	}

	flow.Type = "L3_L4"

	// IP 헤더 파싱 시뮬레이션 (실제: gopacket layers.IPv4 디코딩)
	if len(data) >= 12 {
		flow.SrcIP = fmt.Sprintf("%d.%d.%d.%d", data[4], data[5], data[6], data[7])
		flow.DstIP = fmt.Sprintf("%d.%d.%d.%d", data[8], data[9], data[10], data[11])
	}

	// 포트 파싱 시뮬레이션
	if len(data) >= 16 {
		flow.SrcPort = binary.BigEndian.Uint16(data[12:14])
		flow.DstPort = binary.BigEndian.Uint16(data[14:16])
	}

	// 프로토콜 결정
	if len(data) >= 17 {
		switch data[16] {
		case 6:
			flow.Protocol = "TCP"
		case 17:
			flow.Protocol = "UDP"
		case 1:
			flow.Protocol = "ICMP"
		default:
			flow.Protocol = fmt.Sprintf("PROTO_%d", data[16])
		}
	}

	return nil
}

// ── L7 파서 (실제: pkg/hubble/parser/seven/parser.go) ──
// AccessLog 레코드를 L7 Flow로 파싱

type L7Parser struct{}

type AccessLogRecord struct {
	Type     string // "HTTP", "DNS", "Kafka"
	Method   string
	URL      string
	Code     int
	DNSQuery string
}

func (p *L7Parser) Decode(record *AccessLogRecord, flow *Flow) error {
	flow.Type = "L7"
	flow.L7Type = record.Type

	switch record.Type {
	case "HTTP":
		flow.L7Detail = fmt.Sprintf("%s %s -> %d", record.Method, record.URL, record.Code)
	case "DNS":
		flow.L7Detail = fmt.Sprintf("query: %s", record.DNSQuery)
	default:
		flow.L7Detail = record.Type
	}

	return nil
}

// ── 디버그 파서 (실제: pkg/hubble/parser/debug/parser.go) ──

type DebugParser struct{}

func (p *DebugParser) Decode(data []byte, cpu int) (*DebugEvent, error) {
	return &DebugEvent{
		CPU:     cpu,
		Message: fmt.Sprintf("디버그 이벤트 (데이터: %d bytes, CPU: %d)", len(data), cpu),
	}, nil
}

// ── 소켓 파서 (실제: pkg/hubble/parser/sock/parser.go) ──

type SockParser struct{}

func (p *SockParser) Decode(data []byte, flow *Flow) error {
	flow.Type = "SOCK"
	flow.Protocol = "TCP"
	flow.Verdict = VerdictForwarded
	if len(data) >= 12 {
		flow.SrcIP = fmt.Sprintf("%d.%d.%d.%d", data[4], data[5], data[6], data[7])
		flow.DstIP = fmt.Sprintf("%d.%d.%d.%d", data[8], data[9], data[10], data[11])
	}
	return nil
}

// ── 통합 파서 (실제: pkg/hubble/parser/parser.go) ──
// 모든 서브 파서를 조합하여 MonitorEvent를 Event로 디코딩

type Parser struct {
	l34  *L34Parser
	l7   *L7Parser
	dbg  *DebugParser
	sock *SockParser
}

func NewParser() *Parser {
	return &Parser{
		l34:  &L34Parser{},
		l7:   &L7Parser{},
		dbg:  &DebugParser{},
		sock: &SockParser{},
	}
}

// Decode는 MonitorEvent를 분석하여 적절한 서브 파서로 라우팅
// 실제: func (p *Parser) Decode(monitorEvent *observerTypes.MonitorEvent) (*v1.Event, error)
//
// 라우팅 로직:
//   PerfEvent → Data[0] 기반 분기
//     MessageTypeDebug     → dbg.Decode()
//     MessageTypeTraceSock → sock.Decode()
//     기타                 → l34.Decode()
//   AgentEvent → Type 기반 분기
//     MessageTypeAccessLog → l7.Decode()
//     MessageTypeAgent     → AgentNotification
//   LostEvent → LostEvent 직접 생성
func (p *Parser) Decode(monitorEvent *MonitorEvent) (*Event, error) {
	if monitorEvent == nil {
		return nil, fmt.Errorf("빈 이벤트")
	}

	ev := &Event{
		Timestamp: monitorEvent.Timestamp,
	}

	switch payload := monitorEvent.Payload.(type) {
	case *PerfEvent:
		if len(payload.Data) == 0 {
			return nil, fmt.Errorf("빈 PerfEvent 데이터")
		}

		flow := &Flow{
			UUID:     monitorEvent.UUID,
			NodeName: monitorEvent.NodeName,
			Time:     monitorEvent.Timestamp,
		}

		// 메시지 타입에 따른 파서 분기 (핵심 라우팅 로직)
		switch payload.Data[0] {
		case MessageTypeDebug:
			// 디버그 이벤트는 별도 타입으로 처리
			dbgEvent, err := p.dbg.Decode(payload.Data, payload.CPU)
			if err != nil {
				return nil, err
			}
			ev.Event = dbgEvent
			return ev, nil

		case MessageTypeTraceSock:
			// 소켓 이벤트
			if err := p.sock.Decode(payload.Data, flow); err != nil {
				return nil, err
			}

		default:
			// L3/L4 이벤트 (DROP, TRACE, POLICY_VERDICT 등)
			if err := p.l34.Decode(payload.Data, flow); err != nil {
				return nil, err
			}
		}

		ev.Event = flow
		return ev, nil

	case *AgentEvent:
		switch payload.Type {
		case MessageTypeAccessLog:
			// L7 이벤트 (HTTP, DNS 등)
			flow := &Flow{
				UUID:     monitorEvent.UUID,
				NodeName: monitorEvent.NodeName,
				Time:     monitorEvent.Timestamp,
			}
			record, ok := payload.Message.(*AccessLogRecord)
			if !ok {
				return nil, fmt.Errorf("잘못된 에이전트 메시지 타입")
			}
			if err := p.l7.Decode(record, flow); err != nil {
				return nil, err
			}
			ev.Event = flow
			return ev, nil

		case MessageTypeAgent:
			msg, _ := payload.Message.(string)
			ev.Event = &AgentNotification{
				Type:    "AGENT",
				Message: msg,
			}
			return ev, nil

		default:
			return nil, fmt.Errorf("알 수 없는 에이전트 이벤트 타입: %d", payload.Type)
		}

	case *MonitorLostEvent:
		sources := map[int]string{
			1: "PERF_EVENT_RING_BUFFER",
			2: "OBSERVER_EVENTS_QUEUE",
			3: "HUBBLE_RING_BUFFER",
		}
		source := sources[payload.Source]
		if source == "" {
			source = "UNKNOWN"
		}
		ev.Event = &LostEvent{
			Source:        source,
			NumEventsLost: payload.NumLostEvents,
			CPU:           payload.CPU,
		}
		return ev, nil

	default:
		return nil, fmt.Errorf("알 수 없는 페이로드 타입")
	}
}

// ── 테스트용 PerfEvent 데이터 생성 ──

func makeL34PerfData(msgType byte, srcIP, dstIP [4]byte, srcPort, dstPort uint16, proto byte) []byte {
	data := make([]byte, 20)
	data[0] = msgType
	// 예약 바이트
	data[1] = 0
	data[2] = 0
	data[3] = 0
	// 소스 IP
	copy(data[4:8], srcIP[:])
	// 목적지 IP
	copy(data[8:12], dstIP[:])
	// 포트
	binary.BigEndian.PutUint16(data[12:14], srcPort)
	binary.BigEndian.PutUint16(data[14:16], dstPort)
	// 프로토콜
	data[16] = proto
	return data
}

// =============================================================================
// 메인 실행
// =============================================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║     Hubble 파서 파이프라인 시뮬레이션                         ║")
	fmt.Println("║     참조: pkg/hubble/parser/parser.go                       ║")
	fmt.Println("║           pkg/hubble/parser/threefour/parser.go             ║")
	fmt.Println("║           pkg/hubble/parser/seven/parser.go                 ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")

	parser := NewParser()
	now := time.Now()

	// 다양한 MonitorEvent 생성
	monitorEvents := []*MonitorEvent{
		// 1. L3/L4 TRACE 이벤트 (TCP)
		{
			UUID: "uuid-001", Timestamp: now, NodeName: "worker-01",
			Payload: &PerfEvent{
				Data: makeL34PerfData(MessageTypeTrace,
					[4]byte{10, 244, 0, 5}, [4]byte{10, 244, 1, 10},
					54321, 8080, 6),
				CPU: 2,
			},
		},
		// 2. L3/L4 DROP 이벤트
		{
			UUID: "uuid-002", Timestamp: now.Add(1 * time.Millisecond), NodeName: "worker-01",
			Payload: &PerfEvent{
				Data: makeL34PerfData(MessageTypeDrop,
					[4]byte{10, 244, 2, 15}, [4]byte{10, 244, 1, 10},
					44444, 3306, 6),
				CPU: 0,
			},
		},
		// 3. UDP 이벤트
		{
			UUID: "uuid-003", Timestamp: now.Add(2 * time.Millisecond), NodeName: "worker-02",
			Payload: &PerfEvent{
				Data: makeL34PerfData(MessageTypeTrace,
					[4]byte{10, 244, 0, 5}, [4]byte{10, 96, 0, 10},
					45678, 53, 17),
				CPU: 1,
			},
		},
		// 4. Debug 이벤트
		{
			UUID: "uuid-004", Timestamp: now.Add(3 * time.Millisecond), NodeName: "worker-01",
			Payload: &PerfEvent{
				Data: append([]byte{MessageTypeDebug}, make([]byte, 50)...),
				CPU:  3,
			},
		},
		// 5. TraceSock 이벤트
		{
			UUID: "uuid-005", Timestamp: now.Add(4 * time.Millisecond), NodeName: "worker-01",
			Payload: &PerfEvent{
				Data: makeL34PerfData(MessageTypeTraceSock,
					[4]byte{10, 244, 0, 5}, [4]byte{10, 244, 1, 20},
					33333, 443, 6),
				CPU: 0,
			},
		},
		// 6. L7 HTTP 이벤트
		{
			UUID: "uuid-006", Timestamp: now.Add(5 * time.Millisecond), NodeName: "worker-01",
			Payload: &AgentEvent{
				Type: MessageTypeAccessLog,
				Message: &AccessLogRecord{
					Type:   "HTTP",
					Method: "GET",
					URL:    "/api/v1/products",
					Code:   200,
				},
			},
		},
		// 7. L7 DNS 이벤트
		{
			UUID: "uuid-007", Timestamp: now.Add(6 * time.Millisecond), NodeName: "worker-02",
			Payload: &AgentEvent{
				Type: MessageTypeAccessLog,
				Message: &AccessLogRecord{
					Type:     "DNS",
					DNSQuery: "backend.default.svc.cluster.local.",
				},
			},
		},
		// 8. Agent 알림
		{
			UUID: "uuid-008", Timestamp: now.Add(7 * time.Millisecond), NodeName: "worker-01",
			Payload: &AgentEvent{
				Type:    MessageTypeAgent,
				Message: "Endpoint regenerated",
			},
		},
		// 9. Lost 이벤트
		{
			UUID: "uuid-009", Timestamp: now.Add(8 * time.Millisecond), NodeName: "worker-01",
			Payload: &MonitorLostEvent{
				Source:        1,
				NumLostEvents: 42,
				CPU:           2,
			},
		},
	}

	// 파서 파이프라인 실행
	fmt.Println("\n=== 파서 파이프라인 실행 ===")
	fmt.Println(strings.Repeat("-", 80))

	for i, me := range monitorEvents {
		fmt.Printf("\n[이벤트 #%d] MonitorEvent UUID=%s Node=%s\n", i+1, me.UUID, me.NodeName)

		// 페이로드 타입 출력
		switch p := me.Payload.(type) {
		case *PerfEvent:
			typeName := messageTypeNames[int(p.Data[0])]
			if typeName == "" {
				typeName = fmt.Sprintf("TYPE_%d", p.Data[0])
			}
			fmt.Printf("  입력: PerfEvent (타입=%s, 크기=%d, CPU=%d)\n", typeName, len(p.Data), p.CPU)
		case *AgentEvent:
			typeName := messageTypeNames[p.Type]
			fmt.Printf("  입력: AgentEvent (타입=%s)\n", typeName)
		case *MonitorLostEvent:
			fmt.Printf("  입력: LostEvent (유실=%d, CPU=%d)\n", p.NumLostEvents, p.CPU)
		}

		// Decode 실행
		ev, err := parser.Decode(me)
		if err != nil {
			fmt.Printf("  에러: %v\n", err)
			continue
		}

		// 결과 출력
		switch result := ev.Event.(type) {
		case *Flow:
			fmt.Printf("  결과: Flow %s\n", result)
			if pe, ok := me.Payload.(*PerfEvent); ok {
				fmt.Printf("  라우팅: Data[0]=0x%02x → ", pe.Data[0])
				switch pe.Data[0] {
				case MessageTypeTraceSock:
					fmt.Println("sock.Decode()")
				default:
					fmt.Println("l34.Decode()")
				}
			} else {
				fmt.Println("  라우팅: AgentEvent → seven.Decode()")
			}
		case *DebugEvent:
			fmt.Printf("  결과: DebugEvent (CPU=%d) %s\n", result.CPU, result.Message)
			fmt.Println("  라우팅: Data[0]=DEBUG → dbg.Decode()")
		case *AgentNotification:
			fmt.Printf("  결과: AgentNotification %s: %s\n", result.Type, result.Message)
		case *LostEvent:
			fmt.Printf("  결과: LostEvent (소스=%s, 유실=%d)\n", result.Source, result.NumEventsLost)
		}
	}

	// 성능 벤치마크 시뮬레이션
	fmt.Println("\n\n=== 파싱 성능 벤치마크 ===")
	iterations := 100000
	start := time.Now()
	for i := 0; i < iterations; i++ {
		me := &MonitorEvent{
			UUID: fmt.Sprintf("bench-%d", i),
			Timestamp: now,
			NodeName: "bench-node",
			Payload: &PerfEvent{
				Data: makeL34PerfData(MessageTypeTrace,
					[4]byte{10, byte(rand.Intn(255)), byte(rand.Intn(255)), byte(rand.Intn(255))},
					[4]byte{10, byte(rand.Intn(255)), byte(rand.Intn(255)), byte(rand.Intn(255))},
					uint16(rand.Intn(65535)), uint16(rand.Intn(65535)), 6),
				CPU: rand.Intn(8),
			},
		}
		parser.Decode(me)
	}
	elapsed := time.Since(start)
	fmt.Printf("  %d 이벤트 파싱: %v (%.0f events/sec)\n",
		iterations, elapsed, float64(iterations)/elapsed.Seconds())

	// 파이프라인 다이어그램
	fmt.Println("\n" + `
┌──────────────────────────────────────────────────────────────────────┐
│                    파서 파이프라인 구조                               │
├──────────────────────────────────────────────────────────────────────┤
│                                                                      │
│  MonitorEvent                                                        │
│      │                                                               │
│      ├── Payload: *PerfEvent                                         │
│      │     │                                                         │
│      │     ├── Data[0] == MessageTypeDebug                           │
│      │     │     └── dbg.Decode(data, cpu) → DebugEvent              │
│      │     │                                                         │
│      │     ├── Data[0] == MessageTypeTraceSock                       │
│      │     │     └── sock.Decode(data, flow) → Flow{Type: SOCK}     │
│      │     │                                                         │
│      │     └── 기타 (DROP, TRACE, POLICY_VERDICT ...)                │
│      │           └── l34.Decode(data, flow) → Flow{Type: L3_L4}     │
│      │                 └── gopacket: Ethernet → IPv4/6 → TCP/UDP    │
│      │                                                               │
│      ├── Payload: *AgentEvent                                        │
│      │     │                                                         │
│      │     ├── Type == MessageTypeAccessLog                          │
│      │     │     └── l7.Decode(logrecord, flow) → Flow{Type: L7}    │
│      │     │           └── HTTP/DNS/Kafka 파싱                       │
│      │     │                                                         │
│      │     └── Type == MessageTypeAgent                              │
│      │           └── AgentNotification                               │
│      │                                                               │
│      └── Payload: *LostEvent                                         │
│            └── LostEvent{Source, NumEventsLost}                       │
│                                                                      │
│  결과: Event{Timestamp, Event: *Flow | *DebugEvent | ...}            │
│                                                                      │
└──────────────────────────────────────────────────────────────────────┘`)
}
