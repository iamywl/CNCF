package main

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// =============================================================================
// Hubble LocalObserverServer 시뮬레이션
//
// 실제 구현 참조:
//   - pkg/hubble/observer/local_observer.go: LocalObserverServer, Start(), GetFlows()
//   - pkg/hubble/container/ring.go: Ring 버퍼
//   - pkg/hubble/filters/filters.go: FilterFuncs, Apply()
//
// Observer 이벤트 루프:
//   events 채널 수신 → OnMonitorEvent 훅 → Decode → OnDecodedFlow 훅
//   → Ring.Write() → GetFlows 스트리밍
//
// GetFlows 흐름:
//   요청 파라미터 해석 → 필터 빌드 → RingReader 생성 (시작 위치 계산)
//   → eventsReader로 반복 읽기 (follow/number/since/until)
//   → 필터 적용 → 클라이언트 스트리밍
// =============================================================================

// ── 데이터 모델 (간소화) ──

type Verdict int

const (
	VerdictForwarded Verdict = 1
	VerdictDropped   Verdict = 2
)

func (v Verdict) String() string {
	switch v {
	case VerdictForwarded:
		return "FORWARDED"
	case VerdictDropped:
		return "DROPPED"
	default:
		return "UNKNOWN"
	}
}

type Flow struct {
	Time      time.Time
	SrcIP     string
	DstIP     string
	SrcPort   uint16
	DstPort   uint16
	Protocol  string
	Verdict   Verdict
	Namespace string
	PodName   string
	NodeName  string
}

func (f *Flow) String() string {
	return fmt.Sprintf("[%s] %s %s:%d -> %s:%d %s (%s/%s)",
		f.Verdict, f.Protocol, f.SrcIP, f.SrcPort,
		f.DstIP, f.DstPort, f.NodeName, f.Namespace, f.PodName)
}

type LostEvent struct {
	Source        string
	NumEventsLost uint64
}

type Event struct {
	Timestamp time.Time
	Event     any // *Flow or *LostEvent
}

func (ev *Event) GetFlow() *Flow {
	if ev == nil || ev.Event == nil {
		return nil
	}
	if f, ok := ev.Event.(*Flow); ok {
		return f
	}
	return nil
}

// ── MonitorEvent (파서 입력) ──

type MonitorEvent struct {
	Timestamp time.Time
	NodeName  string
	Data      []byte
}

// ── 간소화된 Ring 버퍼 ──

type Ring struct {
	mu   sync.Mutex
	data []*Event
	cap  int
	head int
	len  int

	notifyMu sync.Mutex
	notifyCh chan struct{}
}

func NewRing(capacity int) *Ring {
	return &Ring{
		data: make([]*Event, capacity),
		cap:  capacity,
	}
}

func (r *Ring) Write(entry *Event) {
	r.mu.Lock()
	r.notifyMu.Lock()

	idx := r.head % r.cap
	r.data[idx] = entry
	r.head++
	if r.len < r.cap {
		r.len++
	}

	if r.notifyCh != nil {
		close(r.notifyCh)
		r.notifyCh = nil
	}

	r.notifyMu.Unlock()
	r.mu.Unlock()
}

func (r *Ring) Cap() int { return r.cap }

func (r *Ring) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.len
}

func (r *Ring) ReadAll() []*Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]*Event, 0, r.len)
	start := 0
	if r.head > r.cap {
		start = r.head - r.cap
	}
	for i := start; i < r.head; i++ {
		idx := i % r.cap
		if r.data[idx] != nil {
			result = append(result, r.data[idx])
		}
	}
	return result
}

func (r *Ring) WaitForWrite(ctx context.Context) bool {
	r.notifyMu.Lock()
	if r.notifyCh == nil {
		r.notifyCh = make(chan struct{})
	}
	ch := r.notifyCh
	r.notifyMu.Unlock()

	select {
	case <-ch:
		return true
	case <-ctx.Done():
		return false
	}
}

// ── 간소화된 파서 ──

type Decoder interface {
	Decode(monitorEvent *MonitorEvent) (*Event, error)
}

type SimpleDecoder struct{}

func (d *SimpleDecoder) Decode(me *MonitorEvent) (*Event, error) {
	// 간소화된 디코딩: 바이트 길이로 이벤트 타입 결정
	if len(me.Data) == 0 {
		return nil, fmt.Errorf("빈 데이터")
	}

	flow := &Flow{
		Time:      me.Timestamp,
		SrcIP:     fmt.Sprintf("10.244.%d.%d", me.Data[0]%10, me.Data[0]),
		DstIP:     fmt.Sprintf("10.244.%d.%d", me.Data[1]%10, me.Data[1]),
		SrcPort:   uint16(30000 + int(me.Data[0])%10000),
		DstPort:   uint16(me.Data[1])<<8 | uint16(me.Data[2]),
		Protocol:  "TCP",
		NodeName:  me.NodeName,
		Namespace: "default",
		PodName:   fmt.Sprintf("pod-%d", me.Data[0]),
	}

	if me.Data[0]%5 == 0 {
		flow.Verdict = VerdictDropped
	} else {
		flow.Verdict = VerdictForwarded
	}

	return &Event{
		Timestamp: me.Timestamp,
		Event:     flow,
	}, nil
}

// ── 필터 ──

type FilterFunc func(ev *Event) bool
type FilterFuncs []FilterFunc

func (fs FilterFuncs) MatchOne(ev *Event) bool {
	if len(fs) == 0 {
		return true
	}
	for _, f := range fs {
		if f(ev) {
			return true
		}
	}
	return false
}

func (fs FilterFuncs) MatchNone(ev *Event) bool {
	if len(fs) == 0 {
		return true
	}
	for _, f := range fs {
		if f(ev) {
			return false
		}
	}
	return true
}

func FilterApply(whitelist, blacklist FilterFuncs, ev *Event) bool {
	return whitelist.MatchOne(ev) && blacklist.MatchNone(ev)
}

// ── Hook 인터페이스 (실제: observeroption 훅) ──

type OnMonitorEventHook interface {
	OnMonitorEvent(ctx context.Context, me *MonitorEvent) (stop bool, err error)
}

type OnDecodedFlowHook interface {
	OnDecodedFlow(ctx context.Context, flow *Flow) (stop bool, err error)
}

// 메트릭 수집 훅 (예시)
type MetricsHook struct {
	mu         sync.Mutex
	flowCounts map[Verdict]int
}

func NewMetricsHook() *MetricsHook {
	return &MetricsHook{
		flowCounts: make(map[Verdict]int),
	}
}

func (h *MetricsHook) OnDecodedFlow(_ context.Context, flow *Flow) (bool, error) {
	h.mu.Lock()
	h.flowCounts[flow.Verdict]++
	h.mu.Unlock()
	return false, nil // stop=false: 다음 훅으로 전달
}

func (h *MetricsHook) Report() {
	h.mu.Lock()
	defer h.mu.Unlock()
	fmt.Println("  [메트릭 훅] 플로우 카운트:")
	for v, c := range h.flowCounts {
		fmt.Printf("    %s: %d\n", v, c)
	}
}

// ── GetFlows 요청 ──

type GetFlowsRequest struct {
	Number    uint64   // 반환할 최대 이벤트 수 (0=무제한)
	Follow    bool     // true면 새 이벤트를 계속 대기
	Since     *time.Time
	Until     *time.Time
	Whitelist FilterFuncs
	Blacklist FilterFuncs
}

// ── LocalObserverServer (실제: pkg/hubble/observer/local_observer.go) ──

type LocalObserverServer struct {
	ring    *Ring
	events  chan *MonitorEvent
	stopped chan struct{}
	decoder Decoder

	numObservedFlows atomic.Uint64
	startTime        time.Time

	// 훅
	onDecodedFlow []OnDecodedFlowHook
}

// NewLocalServer는 새 옵저버 서버를 생성
// 실제: func NewLocalServer(payloadParser, nsManager, logger, options...) (*LocalObserverServer, error)
func NewLocalServer(capacity int, decoder Decoder) *LocalObserverServer {
	return &LocalObserverServer{
		ring:      NewRing(capacity),
		events:    make(chan *MonitorEvent, 100),
		stopped:   make(chan struct{}),
		decoder:   decoder,
		startTime: time.Now(),
	}
}

func (s *LocalObserverServer) AddOnDecodedFlowHook(h OnDecodedFlowHook) {
	s.onDecodedFlow = append(s.onDecodedFlow, h)
}

// Start는 이벤트 루프를 시작
// 실제: func (s *LocalObserverServer) Start()
// 루프:
//   1. events 채널에서 MonitorEvent 수신
//   2. OnMonitorEvent 훅 실행
//   3. payloadParser.Decode(monitorEvent) → Event
//   4. Flow인 경우 OnDecodedFlow 훅 실행
//   5. OnDecodedEvent 훅 실행
//   6. ring.Write(ev)
func (s *LocalObserverServer) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for monitorEvent := range s.events {
		// 디코딩
		ev, err := s.decoder.Decode(monitorEvent)
		if err != nil {
			continue
		}

		// Flow인 경우 훅 실행
		if flow := ev.GetFlow(); flow != nil {
			stop := false
			for _, h := range s.onDecodedFlow {
				hookStop, hookErr := h.OnDecodedFlow(ctx, flow)
				if hookErr != nil {
					fmt.Printf("  [에러] OnDecodedFlow: %v\n", hookErr)
				}
				if hookStop {
					stop = true
					break
				}
			}
			if stop {
				continue
			}
			s.numObservedFlows.Add(1)
		}

		// 링 버퍼에 저장
		s.ring.Write(ev)
	}
	close(s.stopped)
}

// GetEventsChannel은 이벤트 수신 채널을 반환
func (s *LocalObserverServer) GetEventsChannel() chan *MonitorEvent {
	return s.events
}

// ServerStatus는 서버 상태를 반환
// 실제: func (s *LocalObserverServer) ServerStatus(ctx, req) (*ServerStatusResponse, error)
func (s *LocalObserverServer) ServerStatus() {
	uptime := time.Since(s.startTime)
	fmt.Printf("  버전: v0.1-poc\n")
	fmt.Printf("  최대 플로우: %d\n", s.ring.Cap())
	fmt.Printf("  현재 플로우: %d\n", s.ring.Len())
	fmt.Printf("  관찰된 플로우: %d\n", s.numObservedFlows.Load())
	fmt.Printf("  가동 시간: %v\n", uptime.Round(time.Millisecond))
}

// GetFlows는 필터를 적용하여 플로우를 스트리밍
// 실제: func (s *LocalObserverServer) GetFlows(req, server) error
// 흐름:
//   1. 필터 빌드 (whitelist, blacklist)
//   2. RingReader 시작 위치 계산 (since/number/first/follow)
//   3. eventsReader 반복 (Next 또는 NextFollow)
//   4. 필터 적용 (Apply)
//   5. 클라이언트 전송 (server.Send)
func (s *LocalObserverServer) GetFlows(ctx context.Context, req *GetFlowsRequest, resultCh chan<- *Event) {
	defer close(resultCh)

	events := s.ring.ReadAll()
	var count uint64

	// 비-follow 모드: 링 버퍼에서 기존 이벤트 읽기
	for _, ev := range events {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// 시간 범위 필터
		if req.Since != nil && ev.Timestamp.Before(*req.Since) {
			continue
		}
		if req.Until != nil && ev.Timestamp.After(*req.Until) {
			return
		}

		// LostEvent는 항상 통과 (필터 미적용)
		if _, isLost := ev.Event.(*LostEvent); !isLost {
			if !FilterApply(req.Whitelist, req.Blacklist, ev) {
				continue
			}
		}

		resultCh <- ev
		count++

		if req.Number > 0 && count >= req.Number {
			return
		}
	}

	// Follow 모드: 새 이벤트를 계속 대기
	if req.Follow {
		for {
			if !s.ring.WaitForWrite(ctx) {
				return // context 취소
			}

			// 간소화: 마지막 이벤트만 읽기
			newEvents := s.ring.ReadAll()
			if len(newEvents) == 0 {
				continue
			}
			ev := newEvents[len(newEvents)-1]

			if _, isLost := ev.Event.(*LostEvent); !isLost {
				if !FilterApply(req.Whitelist, req.Blacklist, ev) {
					continue
				}
			}

			select {
			case resultCh <- ev:
			case <-ctx.Done():
				return
			}
		}
	}
}

// =============================================================================
// 메인 실행
// =============================================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║     Hubble 옵저버 시뮬레이션                                ║")
	fmt.Println("║     참조: pkg/hubble/observer/local_observer.go             ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")

	// 옵저버 생성
	observer := NewLocalServer(1000, &SimpleDecoder{})
	metricsHook := NewMetricsHook()
	observer.AddOnDecodedFlowHook(metricsHook)

	// 이벤트 루프 시작
	go observer.Start()

	// --- 데모 1: 이벤트 주입 ---
	fmt.Println("\n=== 데모 1: MonitorEvent 주입 및 디코딩 ===")
	eventsCh := observer.GetEventsChannel()

	for i := 0; i < 20; i++ {
		eventsCh <- &MonitorEvent{
			Timestamp: time.Now().Add(time.Duration(i) * time.Millisecond),
			NodeName:  fmt.Sprintf("worker-%02d", i%3+1),
			Data:      []byte{byte(rand.Intn(256)), byte(rand.Intn(256)), byte(rand.Intn(256))},
		}
	}

	// 처리 대기
	time.Sleep(100 * time.Millisecond)

	// --- 데모 2: ServerStatus ---
	fmt.Println("\n=== 데모 2: ServerStatus ===")
	observer.ServerStatus()

	// --- 데모 3: 메트릭 훅 결과 ---
	fmt.Println("\n=== 데모 3: OnDecodedFlow 훅 결과 ===")
	metricsHook.Report()

	// --- 데모 4: GetFlows (전체 조회) ---
	fmt.Println("\n=== 데모 4: GetFlows (전체 플로우 조회) ===")
	ctx4 := context.Background()
	resultCh4 := make(chan *Event, 100)
	go observer.GetFlows(ctx4, &GetFlowsRequest{Number: 5}, resultCh4)

	count := 0
	for ev := range resultCh4 {
		if flow := ev.GetFlow(); flow != nil {
			count++
			fmt.Printf("  [%d] %s\n", count, flow)
		}
	}
	fmt.Printf("  총 %d개 플로우 반환\n", count)

	// --- 데모 5: GetFlows (필터 적용) ---
	fmt.Println("\n=== 데모 5: GetFlows (DROPPED 필터) ===")
	ctx5 := context.Background()
	resultCh5 := make(chan *Event, 100)
	droppedFilter := FilterFuncs{func(ev *Event) bool {
		flow := ev.GetFlow()
		return flow != nil && flow.Verdict == VerdictDropped
	}}
	go observer.GetFlows(ctx5, &GetFlowsRequest{
		Whitelist: droppedFilter,
	}, resultCh5)

	count = 0
	for ev := range resultCh5 {
		if flow := ev.GetFlow(); flow != nil {
			count++
			fmt.Printf("  [%d] %s\n", count, flow)
		}
	}
	fmt.Printf("  총 %d개 DROPPED 플로우\n", count)

	// --- 데모 6: Follow 모드 ---
	fmt.Println("\n=== 데모 6: GetFlows Follow 모드 (2초간 실시간 스트리밍) ===")
	ctx6, cancel6 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel6()

	resultCh6 := make(chan *Event, 100)
	go observer.GetFlows(ctx6, &GetFlowsRequest{
		Follow: true,
	}, resultCh6)

	// Writer: 주기적으로 새 이벤트 주입
	go func() {
		for i := 0; i < 5; i++ {
			time.Sleep(300 * time.Millisecond)
			eventsCh <- &MonitorEvent{
				Timestamp: time.Now(),
				NodeName:  fmt.Sprintf("worker-%02d", i%3+1),
				Data:      []byte{byte(rand.Intn(256)), byte(100 + i), byte(rand.Intn(256))},
			}
		}
	}()

	count = 0
	for ev := range resultCh6 {
		if flow := ev.GetFlow(); flow != nil {
			count++
			fmt.Printf("  [Follow %d] %s\n", count, flow)
		}
	}
	fmt.Printf("  Follow 종료: %d개 이벤트 수신\n", count)

	// 옵저버 종료
	close(eventsCh)
	<-observer.stopped

	// 최종 상태
	fmt.Println("\n=== 최종 서버 상태 ===")
	observer.ServerStatus()
	metricsHook.Report()

	// 아키텍처 다이어그램
	fmt.Println("\n" + `
┌──────────────────────────────────────────────────────────────────────┐
│                LocalObserverServer 이벤트 루프                       │
├──────────────────────────────────────────────────────────────────────┤
│                                                                      │
│  BPF/Agent ──► events chan ──► Start() 이벤트 루프                   │
│                                  │                                   │
│                     ┌────────────┤                                   │
│                     │            │                                   │
│              OnMonitorEvent      │                                   │
│              (전처리 훅)         │                                   │
│                     │            │                                   │
│              payloadParser.Decode(monitorEvent)                      │
│                     │                                                │
│                     ├── Flow?                                        │
│                     │     ├── trackNamespaces(flow)                  │
│                     │     └── OnDecodedFlow 훅                       │
│                     │           ├── 메트릭 수집                      │
│                     │           └── 커스텀 처리                      │
│                     │                                                │
│              OnDecodedEvent 훅                                       │
│                     │                                                │
│              ring.Write(ev) ──────────────► Ring Buffer              │
│                                                  │                   │
│  GetFlows(req, server) ◄─────────────────────────┘                  │
│     │                                                                │
│     ├── BuildFilterList(whitelist)                                   │
│     ├── BuildFilterList(blacklist)                                   │
│     ├── newRingReader(ring, req) ← 시작 위치 계산                    │
│     └── eventsReader.Next(ctx)                                       │
│           │                                                          │
│           ├── follow? → NextFollow (대기)                            │
│           ├── 시간 범위 체크 (since/until)                           │
│           ├── Apply(whitelist, blacklist, ev)                         │
│           └── server.Send(response)                                  │
│                                                                      │
└──────────────────────────────────────────────────────────────────────┘`)
	_ = strings.Repeat("", 0) // strings 패키지 사용
}
