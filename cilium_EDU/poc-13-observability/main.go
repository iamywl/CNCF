package main

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// =============================================================================
// Cilium Observability PoC
// =============================================================================
// Hubble 관측성 파이프라인을 시뮬레이션한다.
// 실제 Cilium에서는 BPF perf events → Monitor Agent → Hubble Observer →
// Ring Buffer 순서로 흐르며, 이 PoC는 그 전체 과정을 재현한다.
//
// 핵심 구성:
//   1. BPF Perf Event 생성기 (커널 이벤트 시뮬레이션)
//   2. Monitor Agent (이벤트 수집 및 디코딩)
//   3. Lock-free Ring Buffer (이벤트 저장, atomic write pointer)
//   4. Hubble Observer (이벤트 소비 및 필터링)
//   5. Prometheus-style Metric Aggregation
// =============================================================================

// --- Flow Event 타입 정의 ---

// FlowVerdict는 패킷 처리 결과를 나타낸다.
// Cilium에서는 FORWARDED, DROPPED, ERROR, AUDIT, REDIRECTED 등이 있다.
type FlowVerdict int

const (
	VerdictForwarded FlowVerdict = iota // 정상 전달
	VerdictDropped                      // 드롭됨
	VerdictError                        // 에러
	VerdictAudit                        // 감사 모드 (드롭하지 않고 로깅만)
)

func (v FlowVerdict) String() string {
	switch v {
	case VerdictForwarded:
		return "FORWARDED"
	case VerdictDropped:
		return "DROPPED"
	case VerdictError:
		return "ERROR"
	case VerdictAudit:
		return "AUDIT"
	default:
		return "UNKNOWN"
	}
}

// FlowType은 이벤트 유형을 나타낸다.
// 실제 Cilium에서는 L3/L4, L7, trace, drop, policy-verdict 등이 있다.
type FlowType int

const (
	FlowTypeTrace         FlowType = iota // 패킷 추적 이벤트
	FlowTypeDrop                          // 패킷 드롭 이벤트
	FlowTypePolicyVerdict                 // 정책 판정 이벤트
)

func (t FlowType) String() string {
	switch t {
	case FlowTypeTrace:
		return "TRACE"
	case FlowTypeDrop:
		return "DROP"
	case FlowTypePolicyVerdict:
		return "POLICY_VERDICT"
	default:
		return "UNKNOWN"
	}
}

// Protocol은 네트워크 프로토콜을 나타낸다.
type Protocol int

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
		return fmt.Sprintf("PROTO_%d", int(p))
	}
}

// FlowEvent는 하나의 네트워크 흐름 이벤트를 나타낸다.
// 실제 Cilium의 flow.Flow protobuf 메시지를 단순화한 것이다.
type FlowEvent struct {
	Timestamp   time.Time   // 이벤트 발생 시각
	Type        FlowType    // 이벤트 유형 (trace/drop/policy-verdict)
	Verdict     FlowVerdict // 처리 결과
	SrcIdentity uint32      // 소스 보안 Identity
	DstIdentity uint32      // 대상 보안 Identity
	SrcIP       string      // 소스 IP
	DstIP       string      // 대상 IP
	SrcPort     uint16      // 소스 포트
	DstPort     uint16      // 대상 포트
	Protocol    Protocol    // 프로토콜
	DropReason  string      // 드롭 사유 (드롭 이벤트인 경우)
}

func (f FlowEvent) String() string {
	base := fmt.Sprintf("[%s] %s %s:%d → %s:%d %s verdict=%s",
		f.Timestamp.Format("15:04:05.000"),
		f.Type, f.SrcIP, f.SrcPort, f.DstIP, f.DstPort,
		f.Protocol, f.Verdict)
	if f.DropReason != "" {
		base += fmt.Sprintf(" reason=%s", f.DropReason)
	}
	return base
}

// =============================================================================
// Lock-Free Ring Buffer
// =============================================================================
// Cilium/Hubble의 핵심 자료구조. 고정 크기 버퍼에서 atomic write pointer를
// 사용하여 lock 없이 이벤트를 저장한다.
// - 버퍼가 가득 차면 가장 오래된 이벤트를 덮어쓴다 (cycle detection)
// - 덮어쓰기 발생 시 lost event를 추적한다
// - 소비자는 자신의 read pointer를 유지하여 독립적으로 읽는다

// RingBuffer는 고정 크기의 순환 버퍼이다.
type RingBuffer struct {
	buffer   []FlowEvent // 이벤트 저장소
	size     uint64      // 버퍼 크기 (2의 거듭제곱)
	mask     uint64      // size - 1 (빠른 모듈로 연산용)
	writePos uint64      // atomic write pointer (단조 증가)
	lost     uint64      // 덮어쓰기로 손실된 이벤트 수
	written  uint64      // 총 기록된 이벤트 수
}

// NewRingBuffer는 주어진 크기의 링 버퍼를 생성한다.
// 크기는 2의 거듭제곱으로 올림한다 (비트 마스크 연산 최적화).
func NewRingBuffer(size int) *RingBuffer {
	// 2의 거듭제곱으로 올림
	actualSize := uint64(1)
	for actualSize < uint64(size) {
		actualSize <<= 1
	}
	return &RingBuffer{
		buffer: make([]FlowEvent, actualSize),
		size:   actualSize,
		mask:   actualSize - 1,
	}
}

// Write는 이벤트를 버퍼에 기록한다.
// atomic CAS 없이 단일 생산자 모델에서는 atomic Add만으로 충분하다.
// Hubble에서는 Monitor Agent가 단일 생산자 역할을 한다.
func (rb *RingBuffer) Write(event FlowEvent) {
	// atomic으로 write position을 가져오고 1 증가시킨다
	pos := atomic.AddUint64(&rb.writePos, 1) - 1
	idx := pos & rb.mask // 비트 마스크로 인덱스 계산 (pos % size 와 동일)

	// 버퍼가 한 바퀴 돌았으면 덮어쓰기 발생 → lost 카운트 증가
	if pos >= rb.size {
		// writePos가 size를 넘었다면 이전 데이터 덮어쓰기
		atomic.AddUint64(&rb.lost, 1)
	}

	rb.buffer[idx] = event
	atomic.AddUint64(&rb.written, 1)
}

// Read는 주어진 위치에서 이벤트를 읽는다.
// 반환값: 이벤트, 유효한지 여부
func (rb *RingBuffer) Read(readPos uint64) (FlowEvent, bool) {
	currentWrite := atomic.LoadUint64(&rb.writePos)

	// readPos가 아직 기록되지 않은 위치를 가리키면 유효하지 않음
	if readPos >= currentWrite {
		return FlowEvent{}, false
	}

	// readPos가 이미 덮어쓰기된 위치를 가리키면 유효하지 않음 (cycle detection)
	if currentWrite-readPos > rb.size {
		return FlowEvent{}, false
	}

	idx := readPos & rb.mask
	return rb.buffer[idx], true
}

// Stats는 링 버퍼의 현재 상태를 반환한다.
func (rb *RingBuffer) Stats() (written, lost, capacity uint64) {
	return atomic.LoadUint64(&rb.written),
		atomic.LoadUint64(&rb.lost),
		rb.size
}

// =============================================================================
// Monitor Agent (이벤트 수집기)
// =============================================================================
// 실제 Cilium에서 Monitor Agent는 BPF perf 이벤트 맵을 polling하여
// 커널에서 올라오는 이벤트를 수집하고 디코딩한다.
// pkg/monitor/agent/agent.go 참조.

// MonitorAgent는 BPF 이벤트를 수집하여 Ring Buffer에 전달한다.
type MonitorAgent struct {
	ringBuffer *RingBuffer
	consumers  []chan FlowEvent // 등록된 소비자들에게 이벤트를 팬아웃
	mu         sync.RWMutex
}

// NewMonitorAgent는 Monitor Agent를 생성한다.
func NewMonitorAgent(rb *RingBuffer) *MonitorAgent {
	return &MonitorAgent{
		ringBuffer: rb,
		consumers:  make([]chan FlowEvent, 0),
	}
}

// RegisterConsumer는 새 소비자를 등록한다. 이벤트를 받을 채널을 반환한다.
func (ma *MonitorAgent) RegisterConsumer(bufSize int) chan FlowEvent {
	ch := make(chan FlowEvent, bufSize)
	ma.mu.Lock()
	ma.consumers = append(ma.consumers, ch)
	ma.mu.Unlock()
	return ch
}

// ProcessEvent는 BPF에서 온 이벤트를 처리한다.
// 링 버퍼에 저장하고, 등록된 모든 소비자에게 팬아웃한다.
func (ma *MonitorAgent) ProcessEvent(event FlowEvent) {
	// 1. 링 버퍼에 저장
	ma.ringBuffer.Write(event)

	// 2. 등록된 소비자에게 팬아웃 (비차단)
	ma.mu.RLock()
	for _, ch := range ma.consumers {
		select {
		case ch <- event:
		default:
			// 소비자 채널이 가득 차면 이벤트 드롭 (backpressure)
		}
	}
	ma.mu.RUnlock()
}

// =============================================================================
// Metric Aggregator (Prometheus 스타일 메트릭 집계)
// =============================================================================
// 실제 Hubble에서는 flows_processed_total{type, verdict, protocol}
// 카운터로 메트릭을 노출한다.

// MetricKey는 메트릭 레이블 조합을 나타낸다.
type MetricKey struct {
	Type     FlowType
	Verdict  FlowVerdict
	Protocol Protocol
}

func (k MetricKey) String() string {
	return fmt.Sprintf("type=%s,verdict=%s,protocol=%s", k.Type, k.Verdict, k.Protocol)
}

// MetricAggregator는 이벤트를 집계하여 Prometheus 스타일 메트릭을 생성한다.
type MetricAggregator struct {
	counters map[MetricKey]*uint64 // flows_processed_total 카운터
	mu       sync.RWMutex
}

// NewMetricAggregator는 메트릭 집계기를 생성한다.
func NewMetricAggregator() *MetricAggregator {
	return &MetricAggregator{
		counters: make(map[MetricKey]*uint64),
	}
}

// Observe는 하나의 이벤트를 관찰하여 메트릭을 업데이트한다.
func (ma *MetricAggregator) Observe(event FlowEvent) {
	key := MetricKey{
		Type:     event.Type,
		Verdict:  event.Verdict,
		Protocol: event.Protocol,
	}

	ma.mu.RLock()
	counter, exists := ma.counters[key]
	ma.mu.RUnlock()

	if exists {
		atomic.AddUint64(counter, 1)
		return
	}

	// 새 키 → 쓰기 잠금 필요
	ma.mu.Lock()
	// 이중 확인 (double-check locking)
	if counter, exists = ma.counters[key]; exists {
		ma.mu.Unlock()
		atomic.AddUint64(counter, 1)
		return
	}
	var val uint64 = 1
	ma.counters[key] = &val
	ma.mu.Unlock()
}

// Dump는 현재 메트릭을 Prometheus exposition 형식으로 출력한다.
func (ma *MetricAggregator) Dump() string {
	var sb strings.Builder
	sb.WriteString("# HELP hubble_flows_processed_total Total number of flows processed\n")
	sb.WriteString("# TYPE hubble_flows_processed_total counter\n")

	ma.mu.RLock()
	defer ma.mu.RUnlock()

	for key, counter := range ma.counters {
		val := atomic.LoadUint64(counter)
		sb.WriteString(fmt.Sprintf(
			"hubble_flows_processed_total{%s} %d\n",
			key, val))
	}
	return sb.String()
}

// =============================================================================
// BPF Event Generator (이벤트 생성기)
// =============================================================================
// 실제 커널에서 발생하는 BPF perf 이벤트를 시뮬레이션한다.

var dropReasons = []string{
	"POLICY_DENIED",
	"CT_TRUNCATED",
	"FRAG_NOSUPPORT",
	"INVALID_SOURCE",
	"UNSUPPORTED_L3",
}

func generateFlowEvent() FlowEvent {
	protocols := []Protocol{ProtoTCP, ProtoUDP, ProtoICMP}
	proto := protocols[rand.Intn(len(protocols))]

	// 80% forwarded, 15% dropped, 5% audit
	r := rand.Float64()
	var verdict FlowVerdict
	var flowType FlowType
	var dropReason string

	switch {
	case r < 0.80:
		verdict = VerdictForwarded
		flowType = FlowTypeTrace
	case r < 0.95:
		verdict = VerdictDropped
		flowType = FlowTypeDrop
		dropReason = dropReasons[rand.Intn(len(dropReasons))]
	default:
		verdict = VerdictAudit
		flowType = FlowTypePolicyVerdict
	}

	return FlowEvent{
		Timestamp:   time.Now(),
		Type:        flowType,
		Verdict:     verdict,
		SrcIdentity: uint32(rand.Intn(10000) + 1),
		DstIdentity: uint32(rand.Intn(10000) + 1),
		SrcIP:       fmt.Sprintf("10.%d.%d.%d", rand.Intn(256), rand.Intn(256), rand.Intn(254)+1),
		DstIP:       fmt.Sprintf("10.%d.%d.%d", rand.Intn(256), rand.Intn(256), rand.Intn(254)+1),
		SrcPort:     uint16(rand.Intn(65535) + 1),
		DstPort:     uint16(rand.Intn(65535) + 1),
		Protocol:    proto,
		DropReason:  dropReason,
	}
}

// =============================================================================
// Hubble Observer (이벤트 소비자)
// =============================================================================
// 실제 Hubble Observer는 Monitor Agent에서 이벤트를 받아
// 필터링, 변환, 저장, API 서빙을 담당한다.

// HubbleObserver는 이벤트를 소비하고 메트릭을 집계한다.
type HubbleObserver struct {
	events     chan FlowEvent
	metrics    *MetricAggregator
	processed  uint64
	dropped    uint64 // 처리 못한 이벤트
	stopCh     chan struct{}
}

// NewHubbleObserver는 Hubble Observer를 생성한다.
func NewHubbleObserver(events chan FlowEvent, metrics *MetricAggregator) *HubbleObserver {
	return &HubbleObserver{
		events:  events,
		metrics: metrics,
		stopCh:  make(chan struct{}),
	}
}

// Run은 Observer의 이벤트 처리 루프를 시작한다.
func (ho *HubbleObserver) Run() {
	for {
		select {
		case event := <-ho.events:
			// 메트릭 집계
			ho.metrics.Observe(event)
			atomic.AddUint64(&ho.processed, 1)
		case <-ho.stopCh:
			return
		}
	}
}

// Stop은 Observer를 중지한다.
func (ho *HubbleObserver) Stop() {
	close(ho.stopCh)
}

// =============================================================================
// main: 전체 파이프라인 시뮬레이션
// =============================================================================

func main() {
	fmt.Println("=== Cilium Observability PoC ===")
	fmt.Println("Hubble 관측성 파이프라인 시뮬레이션")
	fmt.Println()

	// --- 1. 구성 요소 초기화 ---
	fmt.Println("[1] 구성 요소 초기화")

	// 링 버퍼: 크기 64 (작은 크기로 덮어쓰기 시연)
	ringBuffer := NewRingBuffer(64)
	fmt.Printf("  Ring Buffer 생성: capacity=%d (2의 거듭제곱)\n", ringBuffer.size)

	// Monitor Agent
	monitorAgent := NewMonitorAgent(ringBuffer)
	fmt.Println("  Monitor Agent 생성")

	// Metric Aggregator
	metrics := NewMetricAggregator()
	fmt.Println("  Metric Aggregator 생성")

	// Hubble Observer (Monitor Agent에 소비자로 등록)
	eventCh := monitorAgent.RegisterConsumer(32)
	observer := NewHubbleObserver(eventCh, metrics)
	fmt.Println("  Hubble Observer 등록 (buffer=32)")
	fmt.Println()

	// --- 2. Observer 시작 ---
	fmt.Println("[2] Hubble Observer 시작")
	go observer.Run()

	// --- 3. BPF 이벤트 생성 (200개) ---
	fmt.Println("[3] BPF Perf Event 생성 시뮬레이션 (200개)")
	fmt.Println()

	eventCount := 200
	for i := 0; i < eventCount; i++ {
		event := generateFlowEvent()
		monitorAgent.ProcessEvent(event)

		// 처음 5개 이벤트 출력
		if i < 5 {
			fmt.Printf("  Event #%d: %s\n", i+1, event)
		}
	}
	fmt.Println("  ... (195개 더)")
	fmt.Println()

	// Observer가 처리할 시간을 줌
	time.Sleep(100 * time.Millisecond)
	observer.Stop()

	// --- 4. Ring Buffer 통계 ---
	fmt.Println("[4] Ring Buffer 통계")
	written, lost, capacity := ringBuffer.Stats()
	fmt.Printf("  총 기록: %d\n", written)
	fmt.Printf("  덮어쓰기 손실: %d (버퍼 크기 %d 초과분)\n", lost, capacity)
	fmt.Printf("  버퍼 용량: %d\n", capacity)
	fmt.Println()

	// --- 5. Ring Buffer에서 최근 이벤트 읽기 ---
	fmt.Println("[5] Ring Buffer에서 최근 10개 이벤트 읽기")

	// 가장 최근 writePos에서 역방향으로 10개 읽기
	currentWrite := atomic.LoadUint64(&ringBuffer.writePos)
	startPos := currentWrite - 10
	if currentWrite < 10 {
		startPos = 0
	}

	readCount := 0
	for pos := startPos; pos < currentWrite && readCount < 10; pos++ {
		event, valid := ringBuffer.Read(pos)
		if valid {
			fmt.Printf("  [pos=%d] %s\n", pos, event)
			readCount++
		}
	}
	fmt.Println()

	// --- 6. Cycle Detection 시연 ---
	fmt.Println("[6] Cycle Detection 시연")
	// 오래된 위치(0번)를 읽으려 시도 → 덮어쓰기되어 유효하지 않음
	_, valid := ringBuffer.Read(0)
	fmt.Printf("  위치 0 읽기 시도: valid=%v (200개 기록 후 64 크기 버퍼에서 덮어쓰기됨)\n", valid)

	// 유효한 범위 계산
	validStart := currentWrite - capacity
	fmt.Printf("  유효한 읽기 범위: [%d, %d)\n", validStart, currentWrite)
	fmt.Println()

	// --- 7. Prometheus 메트릭 출력 ---
	fmt.Println("[7] Prometheus-style 메트릭 출력")
	fmt.Println(metrics.Dump())

	// --- 8. Observer 처리 통계 ---
	fmt.Println("[8] Observer 처리 통계")
	processed := atomic.LoadUint64(&observer.processed)
	fmt.Printf("  Observer 처리 이벤트: %d\n", processed)
	fmt.Printf("  (채널 버퍼=32, 이벤트=200 → 일부 드롭 가능)\n")
	fmt.Println()

	// --- 구조 요약 ---
	fmt.Println("=== 파이프라인 구조 ===")
	fmt.Println()
	fmt.Println("  BPF Perf Events")
	fmt.Println("       │")
	fmt.Println("       ▼")
	fmt.Println("  Monitor Agent ─── ProcessEvent()")
	fmt.Println("       │")
	fmt.Println("       ├──▶ Ring Buffer (lock-free, atomic write pointer)")
	fmt.Println("       │     └── 고정 크기, 덮어쓰기, cycle detection")
	fmt.Println("       │")
	fmt.Println("       └──▶ Fan-out → Hubble Observer")
	fmt.Println("                        ├── 이벤트 필터링")
	fmt.Println("                        └── Metric Aggregation")
	fmt.Println("                             └── hubble_flows_processed_total{type,verdict,protocol}")
	fmt.Println()
	fmt.Println("=== 시뮬레이션 완료 ===")
}
