// SPDX-License-Identifier: Apache-2.0
// Cilium Observability PoC - Hubble Pipeline, Relay Aggregation, Prometheus Metrics Simulation
//
// 이 프로그램은 Cilium의 관측성(Observability) 서브시스템의 핵심 메커니즘을 순수 Go로 시뮬레이션한다.
// 실제 Cilium 소스코드의 아키텍처를 따라 다음을 구현한다:
//
// 1. BPF 이벤트 → Ring Buffer → Observer → Flow 파싱 파이프라인
// 2. Relay 멀티노드 집계 (PriorityQueue 기반 타임스탬프 정렬)
// 3. Prometheus 스타일 메트릭 (Counter, Gauge, Histogram)
// 4. Flow 필터링 (Pod, Namespace, Verdict, Protocol)
// 5. 서비스 맵 구축
//
// 외부 의존성 없이 Go 표준 라이브러리만 사용한다.
//
// 참조 소스코드:
//   - pkg/hubble/observer/local_observer.go (LocalObserverServer)
//   - pkg/hubble/container/ring.go (Ring Buffer)
//   - pkg/hubble/relay/observer/observer.go (retrieveFlowsFromPeer, sortFlows)
//   - pkg/hubble/relay/queue/priority_queue.go (PriorityQueue)
//   - pkg/hubble/metrics/flow/handler.go (flowHandler)
//   - pkg/hubble/parser/parser.go (Parser.Decode)
//   - pkg/hubble/monitor/consumer.go (Hubble Consumer)
//   - pkg/monitor/agent/agent.go (Monitor Agent)

package main

import (
	"container/heap"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// =============================================================================
// 1. Flow 데이터 구조 (api/v1/flow/flow.pb.go 참조)
// =============================================================================

// Verdict는 Flow의 판정 결과를 나타낸다.
// 실제 Cilium: api/v1/flow/flow.proto의 Verdict enum
type Verdict int

const (
	VerdictForwarded  Verdict = iota // 전달됨
	VerdictDropped                   // 드롭됨
	VerdictAudit                     // 감사 모드
	VerdictRedirected                // 프록시로 리다이렉트됨
	VerdictError                     // 오류
)

func (v Verdict) String() string {
	switch v {
	case VerdictForwarded:
		return "FORWARDED"
	case VerdictDropped:
		return "DROPPED"
	case VerdictAudit:
		return "AUDIT"
	case VerdictRedirected:
		return "REDIRECTED"
	case VerdictError:
		return "ERROR"
	default:
		return "UNKNOWN"
	}
}

// DropReason은 패킷 드롭 사유를 나타낸다.
// 실제 Cilium: pkg/monitor/api/drop.go의 드롭 사유 코드
type DropReason int

const (
	DropReasonNone             DropReason = iota
	DropReasonPolicyDenied                // 정책에 의해 차단
	DropReasonInvalidSourceIP             // 잘못된 소스 IP
	DropReasonCTTruncated                 // 연결 추적 손상
	DropReasonUnsupportedProto            // 지원하지 않는 프로토콜
)

func (d DropReason) String() string {
	switch d {
	case DropReasonNone:
		return "NONE"
	case DropReasonPolicyDenied:
		return "POLICY_DENIED"
	case DropReasonInvalidSourceIP:
		return "INVALID_SOURCE_IP"
	case DropReasonCTTruncated:
		return "CT_TRUNCATED"
	case DropReasonUnsupportedProto:
		return "UNSUPPORTED_PROTOCOL"
	default:
		return "UNKNOWN"
	}
}

// TrafficDirection은 트래픽 방향을 나타낸다.
type TrafficDirection int

const (
	TrafficIngress TrafficDirection = iota
	TrafficEgress
)

func (d TrafficDirection) String() string {
	if d == TrafficIngress {
		return "INGRESS"
	}
	return "EGRESS"
}

// Protocol은 L4 프로토콜을 나타낸다.
type Protocol string

const (
	ProtoTCP  Protocol = "TCP"
	ProtoUDP  Protocol = "UDP"
	ProtoICMP Protocol = "ICMP"
	ProtoHTTP Protocol = "HTTP"
	ProtoDNS  Protocol = "DNS"
)

// Endpoint는 Flow의 소스/목적지 엔드포인트를 나타낸다.
// 실제 Cilium: api/v1/flow/flow.proto의 Endpoint message
type Endpoint struct {
	ID        uint32
	Identity  uint32
	Namespace string
	PodName   string
	Labels    []string
}

func (e Endpoint) String() string {
	return fmt.Sprintf("%s/%s", e.Namespace, e.PodName)
}

// Service는 Kubernetes 서비스 정보를 나타낸다.
type Service struct {
	Name      string
	Namespace string
}

func (s Service) String() string {
	if s.Name == "" {
		return ""
	}
	return fmt.Sprintf("%s/%s", s.Namespace, s.Name)
}

// L7Info는 L7 프로토콜 정보를 나타낸다.
type L7Info struct {
	Type       string // HTTP, DNS, Kafka
	HTTPMethod string
	HTTPCode   int
	HTTPURL    string
	DNSQuery   string
	DNSRCode   int
}

// Flow는 네트워크 흐름 데이터를 나타낸다.
// 실제 Cilium: api/v1/flow/flow.pb.go의 Flow struct
type Flow struct {
	Time               time.Time
	UUID               string
	NodeName           string
	Verdict            Verdict
	DropReason         DropReason
	Source             Endpoint
	Destination        Endpoint
	SourceService      Service
	DestinationService Service
	Protocol           Protocol
	SrcPort            uint16
	DstPort            uint16
	TrafficDirection   TrafficDirection
	L7                 *L7Info
	IsReply            bool
	TraceID            string // OpenTelemetry trace ID
}

func (f *Flow) String() string {
	verdict := f.Verdict.String()
	dropInfo := ""
	if f.Verdict == VerdictDropped {
		dropInfo = fmt.Sprintf(" reason=%s", f.DropReason)
	}
	l7Info := ""
	if f.L7 != nil {
		switch f.L7.Type {
		case "HTTP":
			l7Info = fmt.Sprintf(" %s %s -> %d", f.L7.HTTPMethod, f.L7.HTTPURL, f.L7.HTTPCode)
		case "DNS":
			l7Info = fmt.Sprintf(" query=%s rcode=%d", f.L7.DNSQuery, f.L7.DNSRCode)
		}
	}
	return fmt.Sprintf("[%s] %s %s -> %s %s:%d %s%s%s",
		f.Time.Format("15:04:05.000"),
		f.NodeName,
		f.Source, f.Destination,
		f.Protocol, f.DstPort,
		verdict, dropInfo, l7Info)
}

// =============================================================================
// 2. BPF 이벤트 시뮬레이션 (pkg/monitor/ 참조)
// =============================================================================

// BPFEventType은 BPF 데이터패스에서 생성되는 이벤트 타입이다.
// 실제 Cilium: pkg/monitor/api/types.go의 MessageType* 상수
type BPFEventType uint8

const (
	MessageTypeTrace        BPFEventType = 1
	MessageTypeDrop         BPFEventType = 2
	MessageTypePolicyVerdict BPFEventType = 5
	MessageTypeAccessLog    BPFEventType = 6
)

func (t BPFEventType) String() string {
	switch t {
	case MessageTypeTrace:
		return "TRACE"
	case MessageTypeDrop:
		return "DROP"
	case MessageTypePolicyVerdict:
		return "POLICY_VERDICT"
	case MessageTypeAccessLog:
		return "ACCESS_LOG"
	default:
		return "UNKNOWN"
	}
}

// BPFEvent는 BPF perf ring buffer에서 읽은 원시 이벤트를 시뮬레이션한다.
// 실제 Cilium: pkg/hubble/observer/types/types.go의 PerfEvent
type BPFEvent struct {
	Type       BPFEventType
	CPU        int
	Data       []byte // 실제로는 바이너리 데이터, 여기서는 시뮬레이션
	SrcID      uint16
	DstID      uint16
	SrcLabel   uint32
	DstLabel   uint32
	DropReason uint8
	ObsPoint   uint8
}

// MonitorEvent는 모니터 에이전트에서 Observer로 전달되는 이벤트이다.
// 실제 Cilium: pkg/hubble/observer/types/types.go의 MonitorEvent
type MonitorEvent struct {
	UUID      string
	Timestamp time.Time
	NodeName  string
	Payload   interface{} // BPFEvent 또는 LostEvent
}

// LostEvent는 이벤트 손실을 알리는 이벤트이다.
type LostEvent struct {
	Source        string
	NumLostEvents uint64
	CPU           int
}

// =============================================================================
// 3. Ring Buffer (pkg/hubble/container/ring.go 참조)
// =============================================================================

// Event는 Hubble의 내부 이벤트 타입이다.
// 실제 Cilium: pkg/hubble/api/v1/types.go의 Event
type Event struct {
	Timestamp time.Time
	Event     interface{} // *Flow, *LostEvent 등
}

// RingBuffer는 Hubble의 고성능 순환 버퍼를 시뮬레이션한다.
// 실제 Cilium: pkg/hubble/container/ring.go의 Ring struct
// 실제 구현은 원자적 연산과 비트 마스킹을 사용하지만, 여기서는 뮤텍스로 단순화한다.
type RingBuffer struct {
	mu       sync.RWMutex
	data     []*Event
	capacity int
	writePos int
	count    int
	notifyCh chan struct{}
}

// NewRingBuffer는 지정된 용량의 Ring Buffer를 생성한다.
func NewRingBuffer(capacity int) *RingBuffer {
	return &RingBuffer{
		data:     make([]*Event, capacity),
		capacity: capacity,
		notifyCh: make(chan struct{}, 1),
	}
}

// Write는 이벤트를 Ring Buffer에 기록한다.
// 실제 Cilium: pkg/hubble/container/ring.go의 Ring.Write()
func (r *RingBuffer) Write(ev *Event) {
	r.mu.Lock()
	r.data[r.writePos%r.capacity] = ev
	r.writePos++
	if r.count < r.capacity {
		r.count++
	}
	r.mu.Unlock()

	// 대기 중인 리더에게 알림 (비차단)
	select {
	case r.notifyCh <- struct{}{}:
	default:
	}
}

// ReadAll은 버퍼의 모든 유효한 이벤트를 시간순으로 반환한다.
func (r *RingBuffer) ReadAll() []*Event {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]*Event, 0, r.count)
	start := 0
	if r.writePos > r.capacity {
		start = r.writePos - r.capacity
	}
	for i := start; i < r.writePos; i++ {
		ev := r.data[i%r.capacity]
		if ev != nil {
			result = append(result, ev)
		}
	}
	return result
}

// ReadLast는 최근 N개 이벤트를 반환한다.
func (r *RingBuffer) ReadLast(n int) []*Event {
	all := r.ReadAll()
	if len(all) <= n {
		return all
	}
	return all[len(all)-n:]
}

// Len은 버퍼에 저장된 이벤트 수를 반환한다.
func (r *RingBuffer) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.count
}

// Cap은 버퍼의 최대 용량을 반환한다.
func (r *RingBuffer) Cap() int {
	return r.capacity
}

// =============================================================================
// 4. Hubble Observer (pkg/hubble/observer/local_observer.go 참조)
// =============================================================================

// FlowParser는 BPF 이벤트를 Flow로 변환하는 파서이다.
// 실제 Cilium: pkg/hubble/parser/parser.go의 Parser
type FlowParser struct {
	endpoints map[uint16]Endpoint
	services  map[string]Service
}

// NewFlowParser는 새로운 파서를 생성한다.
func NewFlowParser(endpoints map[uint16]Endpoint, services map[string]Service) *FlowParser {
	return &FlowParser{
		endpoints: endpoints,
		services:  services,
	}
}

// Decode는 MonitorEvent를 Event(Flow)로 변환한다.
// 실제 Cilium: pkg/hubble/parser/parser.go의 Parser.Decode()
func (p *FlowParser) Decode(monitorEvent *MonitorEvent) (*Event, error) {
	bpfEvent, ok := monitorEvent.Payload.(*BPFEvent)
	if !ok {
		if lost, ok := monitorEvent.Payload.(*LostEvent); ok {
			return &Event{
				Timestamp: monitorEvent.Timestamp,
				Event:     lost,
			}, nil
		}
		return nil, fmt.Errorf("unknown payload type")
	}

	src := p.endpoints[bpfEvent.SrcID]
	dst := p.endpoints[bpfEvent.DstID]

	flow := &Flow{
		Time:        monitorEvent.Timestamp,
		UUID:        monitorEvent.UUID,
		NodeName:    monitorEvent.NodeName,
		Source:      src,
		Destination: dst,
	}

	// 서비스 매핑
	srcKey := fmt.Sprintf("%s/%s", src.Namespace, src.PodName)
	dstKey := fmt.Sprintf("%s/%s", dst.Namespace, dst.PodName)
	if svc, ok := p.services[srcKey]; ok {
		flow.SourceService = svc
	}
	if svc, ok := p.services[dstKey]; ok {
		flow.DestinationService = svc
	}

	// 이벤트 타입별 파싱
	switch bpfEvent.Type {
	case MessageTypeTrace:
		flow.Verdict = VerdictForwarded
		flow.Protocol = ProtoTCP
		flow.DstPort = 80
	case MessageTypeDrop:
		flow.Verdict = VerdictDropped
		flow.DropReason = DropReason(bpfEvent.DropReason)
		flow.Protocol = ProtoTCP
		flow.DstPort = 80
	case MessageTypePolicyVerdict:
		if bpfEvent.DropReason > 0 {
			flow.Verdict = VerdictDropped
			flow.DropReason = DropReason(bpfEvent.DropReason)
		} else {
			flow.Verdict = VerdictForwarded
		}
		flow.Protocol = ProtoTCP
		flow.DstPort = 443
	case MessageTypeAccessLog:
		flow.Verdict = VerdictForwarded
		flow.Protocol = ProtoHTTP
		flow.DstPort = 8080
		flow.L7 = &L7Info{
			Type:       "HTTP",
			HTTPMethod: "GET",
			HTTPCode:   200,
			HTTPURL:    "/api/v1/pods",
		}
	}

	return &Event{
		Timestamp: monitorEvent.Timestamp,
		Event:     flow,
	}, nil
}

// OnDecodedFlowFunc는 Flow 디코딩 후 호출되는 콜백이다.
// 실제 Cilium: pkg/hubble/observer/observeroption/option.go의 OnDecodedFlow
type OnDecodedFlowFunc func(flow *Flow) bool // bool = stop

// LocalObserver는 노드 로컬 Observer를 시뮬레이션한다.
// 실제 Cilium: pkg/hubble/observer/local_observer.go의 LocalObserverServer
type LocalObserver struct {
	ring             *RingBuffer
	events           chan *MonitorEvent
	parser           *FlowParser
	numObservedFlows atomic.Uint64
	startTime        time.Time
	nodeName         string
	onDecodedFlow    []OnDecodedFlowFunc
	stopped          chan struct{}
}

// NewLocalObserver는 새로운 Observer를 생성한다.
func NewLocalObserver(nodeName string, ringCapacity int, parser *FlowParser) *LocalObserver {
	return &LocalObserver{
		ring:      NewRingBuffer(ringCapacity),
		events:    make(chan *MonitorEvent, 100),
		parser:    parser,
		startTime: time.Now(),
		nodeName:  nodeName,
		stopped:   make(chan struct{}),
	}
}

// AddOnDecodedFlow는 Flow 디코딩 후 콜백을 등록한다.
func (o *LocalObserver) AddOnDecodedFlow(f OnDecodedFlowFunc) {
	o.onDecodedFlow = append(o.onDecodedFlow, f)
}

// Start는 이벤트 처리 루프를 시작한다.
// 실제 Cilium: pkg/hubble/observer/local_observer.go의 Start()
func (o *LocalObserver) Start() {
	go func() {
		defer close(o.stopped)
		for monitorEvent := range o.events {
			// 1단계: 페이로드 파싱
			ev, err := o.parser.Decode(monitorEvent)
			if err != nil {
				continue
			}

			// 2단계: Flow 디코딩 후 처리
			if flow, ok := ev.Event.(*Flow); ok {
				stop := false
				for _, f := range o.onDecodedFlow {
					if f(flow) {
						stop = true
						break
					}
				}
				if stop {
					continue
				}
				o.numObservedFlows.Add(1)
			}

			// 3단계: Ring Buffer에 기록
			o.ring.Write(ev)
		}
	}()
}

// SendEvent는 모니터 이벤트를 Observer에 전송한다.
func (o *LocalObserver) SendEvent(ev *MonitorEvent) {
	select {
	case o.events <- ev:
	default:
		// 채널이 가득 찬 경우 (실제로는 LostEvent 생성)
		fmt.Printf("[%s] WARN: events queue full, dropping event\n", o.nodeName)
	}
}

// Stop은 Observer를 중지한다.
func (o *LocalObserver) Stop() {
	close(o.events)
	<-o.stopped
}

// GetFlows는 Ring Buffer에서 Flow를 읽어 반환한다.
// 실제 Cilium: pkg/hubble/observer/local_observer.go의 GetFlows()
func (o *LocalObserver) GetFlows(filters []FlowFilter, maxCount int) []*Flow {
	events := o.ring.ReadAll()
	var flows []*Flow
	for _, ev := range events {
		if flow, ok := ev.Event.(*Flow); ok {
			if matchFilters(flow, filters) {
				flows = append(flows, flow)
				if maxCount > 0 && len(flows) >= maxCount {
					break
				}
			}
		}
	}
	return flows
}

// Status는 Observer 상태를 반환한다.
func (o *LocalObserver) Status() string {
	return fmt.Sprintf("Node: %s, Flows: %d/%d, Seen: %d, Uptime: %s",
		o.nodeName,
		o.ring.Len(), o.ring.Cap(),
		o.numObservedFlows.Load(),
		time.Since(o.startTime).Round(time.Millisecond))
}

// =============================================================================
// 5. BPF 이벤트 생성기 (Monitor Agent 시뮬레이션)
// =============================================================================

// BPFEventGenerator는 BPF 데이터패스에서 이벤트를 생성하는 것을 시뮬레이션한다.
// 실제 Cilium: pkg/monitor/agent/agent.go의 handleEvents()
type BPFEventGenerator struct {
	observer *LocalObserver
	uuidSeq  atomic.Uint64
}

// NewBPFEventGenerator는 새로운 이벤트 생성기를 만든다.
func NewBPFEventGenerator(observer *LocalObserver) *BPFEventGenerator {
	return &BPFEventGenerator{observer: observer}
}

// GenerateEvent는 단일 BPF 이벤트를 생성하여 Observer에 전송한다.
func (g *BPFEventGenerator) GenerateEvent(eventType BPFEventType, srcID, dstID uint16, dropReason uint8) {
	seq := g.uuidSeq.Add(1)
	ev := &MonitorEvent{
		UUID:      fmt.Sprintf("%s-%06d", g.observer.nodeName, seq),
		Timestamp: time.Now(),
		NodeName:  g.observer.nodeName,
		Payload: &BPFEvent{
			Type:       eventType,
			CPU:        rand.Intn(4),
			SrcID:      srcID,
			DstID:      dstID,
			DropReason: dropReason,
		},
	}
	g.observer.SendEvent(ev)
}

// GenerateTrafficBurst는 일련의 트래픽 이벤트를 생성한다.
func (g *BPFEventGenerator) GenerateTrafficBurst(count int, endpoints [][2]uint16) {
	for i := 0; i < count; i++ {
		pair := endpoints[rand.Intn(len(endpoints))]
		eventType := BPFEventType(1 + rand.Intn(4)) // 1~4 (Trace ~ AccessLog)
		var dropReason uint8
		if eventType == MessageTypeDrop {
			dropReason = uint8(1 + rand.Intn(4)) // 무작위 드롭 사유
		}
		g.GenerateEvent(eventType, pair[0], pair[1], dropReason)
		time.Sleep(time.Duration(rand.Intn(5)) * time.Millisecond)
	}
}

// =============================================================================
// 6. Flow 필터링 (pkg/hubble/filters/ 참조)
// =============================================================================

// FlowFilter는 Flow 필터링 조건을 나타낸다.
// 실제 Cilium: api/v1/flow/flow.proto의 FlowFilter message
type FlowFilter struct {
	SourcePod      string
	DestPod        string
	SourceNS       string
	DestNS         string
	Verdict        *Verdict
	Protocol       *Protocol
	DropReasonDesc *DropReason
}

// matchFilters는 Flow가 모든 필터 조건을 만족하는지 확인한다.
// 실제 Cilium: pkg/hubble/filters/filters.go의 Apply()
func matchFilters(flow *Flow, filters []FlowFilter) bool {
	if len(filters) == 0 {
		return true
	}
	for _, f := range filters {
		if matchSingleFilter(flow, f) {
			return true // OR 매칭: 하나라도 맞으면 통과
		}
	}
	return false
}

func matchSingleFilter(flow *Flow, f FlowFilter) bool {
	if f.SourcePod != "" && !strings.Contains(flow.Source.PodName, f.SourcePod) {
		return false
	}
	if f.DestPod != "" && !strings.Contains(flow.Destination.PodName, f.DestPod) {
		return false
	}
	if f.SourceNS != "" && flow.Source.Namespace != f.SourceNS {
		return false
	}
	if f.DestNS != "" && flow.Destination.Namespace != f.DestNS {
		return false
	}
	if f.Verdict != nil && flow.Verdict != *f.Verdict {
		return false
	}
	if f.Protocol != nil && flow.Protocol != *f.Protocol {
		return false
	}
	if f.DropReasonDesc != nil && flow.DropReason != *f.DropReasonDesc {
		return false
	}
	return true
}

// =============================================================================
// 7. Hubble Relay - 멀티노드 집계 (pkg/hubble/relay/ 참조)
// =============================================================================

// FlowResponse는 Relay에서 클라이언트로 전달되는 응답이다.
// 실제 Cilium: api/v1/observer/observer.proto의 GetFlowsResponse
type FlowResponse struct {
	Time     time.Time
	NodeName string
	Flow     *Flow
}

// PriorityQueue는 타임스탬프 기반 최소 힙이다.
// 실제 Cilium: pkg/hubble/relay/queue/priority_queue.go의 PriorityQueue
type PriorityQueue []*FlowResponse

func (pq PriorityQueue) Len() int { return len(pq) }
func (pq PriorityQueue) Less(i, j int) bool {
	return pq[i].Time.Before(pq[j].Time) // 오래된 것이 우선
}
func (pq PriorityQueue) Swap(i, j int) { pq[i], pq[j] = pq[j], pq[i] }

func (pq *PriorityQueue) Push(x interface{}) {
	*pq = append(*pq, x.(*FlowResponse))
}

func (pq *PriorityQueue) Pop() interface{} {
	old := *pq
	n := len(old)
	if n == 0 {
		return nil
	}
	item := old[n-1]
	old[n-1] = nil
	*pq = old[0 : n-1]
	return item
}

// HubbleRelay는 멀티노드 Flow 집계를 시뮬레이션한다.
// 실제 Cilium: pkg/hubble/relay/observer/observer.go
type HubbleRelay struct {
	observers []*LocalObserver
	queueSize int
}

// NewHubbleRelay는 새로운 Relay를 생성한다.
func NewHubbleRelay(observers []*LocalObserver, queueSize int) *HubbleRelay {
	return &HubbleRelay{
		observers: observers,
		queueSize: queueSize,
	}
}

// GetFlows는 모든 노드에서 Flow를 수집하여 타임스탬프 기준으로 정렬한다.
// 실제 Cilium: pkg/hubble/relay/observer/observer.go의 sortFlows()
func (r *HubbleRelay) GetFlows(filters []FlowFilter, maxCount int) []*FlowResponse {
	// 1단계: 각 노드에서 Flow 수집 (실제로는 gRPC 스트리밍)
	var allFlows []*FlowResponse
	for _, obs := range r.observers {
		flows := obs.GetFlows(filters, 0) // 필터 적용, 제한 없음
		for _, f := range flows {
			allFlows = append(allFlows, &FlowResponse{
				Time:     f.Time,
				NodeName: f.NodeName,
				Flow:     f,
			})
		}
	}

	// 2단계: PriorityQueue를 사용하여 타임스탬프 기준 정렬
	pq := make(PriorityQueue, 0, len(allFlows))
	heap.Init(&pq)
	for _, f := range allFlows {
		heap.Push(&pq, f)
	}

	// 3단계: 정렬된 순서로 추출
	var sorted []*FlowResponse
	for pq.Len() > 0 {
		resp := heap.Pop(&pq).(*FlowResponse)
		sorted = append(sorted, resp)
		if maxCount > 0 && len(sorted) >= maxCount {
			break
		}
	}

	return sorted
}

// GetConnectedNodes는 연결된 노드 목록을 반환한다.
func (r *HubbleRelay) GetConnectedNodes() []string {
	var nodes []string
	for _, obs := range r.observers {
		nodes = append(nodes, obs.nodeName)
	}
	return nodes
}

// =============================================================================
// 8. Prometheus 메트릭 시뮬레이션 (pkg/hubble/metrics/ 참조)
// =============================================================================

// MetricType은 메트릭 타입이다.
type MetricType int

const (
	MetricCounter MetricType = iota
	MetricGauge
	MetricHistogram
)

// Counter는 단조 증가하는 카운터를 시뮬레이션한다.
type Counter struct {
	name   string
	help   string
	labels map[string]float64
	mu     sync.RWMutex
}

func NewCounter(name, help string) *Counter {
	return &Counter{
		name:   name,
		help:   help,
		labels: make(map[string]float64),
	}
}

func (c *Counter) Inc(labelValues ...string) {
	key := strings.Join(labelValues, "|")
	c.mu.Lock()
	c.labels[key]++
	c.mu.Unlock()
}

func (c *Counter) Add(val float64, labelValues ...string) {
	key := strings.Join(labelValues, "|")
	c.mu.Lock()
	c.labels[key] += val
	c.mu.Unlock()
}

func (c *Counter) Get(labelValues ...string) float64 {
	key := strings.Join(labelValues, "|")
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.labels[key]
}

func (c *Counter) Dump() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var lines []string
	lines = append(lines, fmt.Sprintf("# HELP %s %s", c.name, c.help))
	lines = append(lines, fmt.Sprintf("# TYPE %s counter", c.name))
	for labels, val := range c.labels {
		if labels == "" {
			lines = append(lines, fmt.Sprintf("%s %.0f", c.name, val))
		} else {
			lines = append(lines, fmt.Sprintf("%s{labels=\"%s\"} %.0f", c.name, labels, val))
		}
	}
	sort.Strings(lines[2:])
	return strings.Join(lines, "\n")
}

// Gauge는 임의로 증감 가능한 게이지를 시뮬레이션한다.
type Gauge struct {
	name  string
	help  string
	value atomic.Int64
}

func NewGauge(name, help string) *Gauge {
	return &Gauge{name: name, help: help}
}

func (g *Gauge) Set(val int64) { g.value.Store(val) }
func (g *Gauge) Inc()          { g.value.Add(1) }
func (g *Gauge) Dec()          { g.value.Add(-1) }
func (g *Gauge) Get() int64    { return g.value.Load() }

func (g *Gauge) Dump() string {
	return fmt.Sprintf("# HELP %s %s\n# TYPE %s gauge\n%s %d",
		g.name, g.help, g.name, g.name, g.value.Load())
}

// Histogram은 관측값의 분포를 시뮬레이션한다.
type Histogram struct {
	name    string
	help    string
	buckets []float64
	counts  map[float64]uint64
	sum     float64
	count   uint64
	mu      sync.Mutex
}

func NewHistogram(name, help string, buckets []float64) *Histogram {
	return &Histogram{
		name:    name,
		help:    help,
		buckets: buckets,
		counts:  make(map[float64]uint64),
	}
}

func (h *Histogram) Observe(val float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sum += val
	h.count++
	for _, b := range h.buckets {
		if val <= b {
			h.counts[b]++
		}
	}
}

func (h *Histogram) Dump() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var lines []string
	lines = append(lines, fmt.Sprintf("# HELP %s %s", h.name, h.help))
	lines = append(lines, fmt.Sprintf("# TYPE %s histogram", h.name))
	var cumulative uint64
	for _, b := range h.buckets {
		cumulative += h.counts[b]
		lines = append(lines, fmt.Sprintf("%s_bucket{le=\"%.3f\"} %d", h.name, b, cumulative))
	}
	lines = append(lines, fmt.Sprintf("%s_bucket{le=\"+Inf\"} %d", h.name, h.count))
	lines = append(lines, fmt.Sprintf("%s_sum %.3f", h.name, h.sum))
	lines = append(lines, fmt.Sprintf("%s_count %d", h.name, h.count))
	return strings.Join(lines, "\n")
}

// HubbleMetrics는 Hubble 메트릭 핸들러를 시뮬레이션한다.
// 실제 Cilium: pkg/hubble/metrics/flow/handler.go의 flowHandler
type HubbleMetrics struct {
	FlowsProcessed  *Counter   // hubble_flows_processed_total
	DropsTotal      *Counter   // hubble_drop_total
	LostEvents      *Counter   // hubble_lost_events_total
	HTTPRequests    *Counter   // hubble_http_requests_total
	HTTPDuration    *Histogram // hubble_http_request_duration_seconds
	ActiveFlows     *Gauge     // hubble_active_flows (커스텀)
	ConnectedNodes  *Gauge     // hubble_connected_nodes
}

func NewHubbleMetrics() *HubbleMetrics {
	return &HubbleMetrics{
		FlowsProcessed: NewCounter("hubble_flows_processed_total",
			"Total number of flows processed"),
		DropsTotal: NewCounter("hubble_drop_total",
			"Total number of dropped packets"),
		LostEvents: NewCounter("hubble_lost_events_total",
			"Number of lost events"),
		HTTPRequests: NewCounter("hubble_http_requests_total",
			"Total number of HTTP requests"),
		HTTPDuration: NewHistogram("hubble_http_request_duration_seconds",
			"HTTP request duration in seconds",
			[]float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0}),
		ActiveFlows: NewGauge("hubble_active_flows",
			"Number of currently active flows"),
		ConnectedNodes: NewGauge("hubble_connected_nodes",
			"Number of connected Hubble nodes"),
	}
}

// ProcessFlow는 Flow 이벤트를 받아 메트릭을 갱신한다.
// 실제 Cilium: pkg/hubble/metrics/flow/handler.go의 ProcessFlow()
func (m *HubbleMetrics) ProcessFlow(flow *Flow) {
	// Flow 카운터 업데이트
	m.FlowsProcessed.Inc(
		string(flow.Protocol),
		flow.Verdict.String(),
		flow.Source.Namespace,
	)

	// 드롭 카운터
	if flow.Verdict == VerdictDropped {
		m.DropsTotal.Inc(flow.DropReason.String(), string(flow.Protocol))
	}

	// HTTP 메트릭
	if flow.L7 != nil && flow.L7.Type == "HTTP" {
		m.HTTPRequests.Inc(
			flow.L7.HTTPMethod,
			fmt.Sprintf("%d", flow.L7.HTTPCode),
		)
		// 시뮬레이션된 지연 시간
		m.HTTPDuration.Observe(rand.Float64() * 0.1)
	}

	m.ActiveFlows.Inc()
}

// =============================================================================
// 9. 서비스 맵 구축 (Hubble UI 참조)
// =============================================================================

// ServiceEdge는 서비스 간 통신 엣지를 나타낸다.
type ServiceEdge struct {
	Source      string
	Destination string
	Protocol    Protocol
	Port        uint16
	FlowCount   int
	Forwarded   int
	Dropped     int
	LastSeen    time.Time
}

// ServiceMap은 서비스 간 의존성 그래프를 구축한다.
type ServiceMap struct {
	mu    sync.RWMutex
	edges map[string]*ServiceEdge // key: "src->dst"
	nodes map[string]bool
}

func NewServiceMap() *ServiceMap {
	return &ServiceMap{
		edges: make(map[string]*ServiceEdge),
		nodes: make(map[string]bool),
	}
}

// AddFlow는 Flow를 기반으로 서비스 맵을 갱신한다.
func (sm *ServiceMap) AddFlow(flow *Flow) {
	src := flow.Source.String()
	dst := flow.Destination.String()

	if flow.SourceService.Name != "" {
		src = flow.SourceService.String()
	}
	if flow.DestinationService.Name != "" {
		dst = flow.DestinationService.String()
	}

	key := fmt.Sprintf("%s->%s", src, dst)

	sm.mu.Lock()
	defer sm.mu.Unlock()

	sm.nodes[src] = true
	sm.nodes[dst] = true

	edge, exists := sm.edges[key]
	if !exists {
		edge = &ServiceEdge{
			Source:      src,
			Destination: dst,
			Protocol:    flow.Protocol,
			Port:        flow.DstPort,
		}
		sm.edges[key] = edge
	}

	edge.FlowCount++
	edge.LastSeen = flow.Time
	if flow.Verdict == VerdictForwarded {
		edge.Forwarded++
	} else if flow.Verdict == VerdictDropped {
		edge.Dropped++
	}
}

// Print는 서비스 맵을 텍스트로 출력한다.
func (sm *ServiceMap) Print() {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	fmt.Println("\n=== Service Dependency Map ===")
	fmt.Printf("Nodes: %d, Edges: %d\n\n", len(sm.nodes), len(sm.edges))

	// 엣지를 정렬하여 출력
	var keys []string
	for k := range sm.edges {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		edge := sm.edges[k]
		status := "OK"
		if edge.Dropped > 0 {
			status = fmt.Sprintf("WARN (dropped: %d)", edge.Dropped)
		}
		fmt.Printf("  [%s] --%s:%d--> [%s]  flows=%d fwd=%d drop=%d  %s\n",
			edge.Source, edge.Protocol, edge.Port, edge.Destination,
			edge.FlowCount, edge.Forwarded, edge.Dropped, status)
	}
}

// =============================================================================
// 10. 메인 실행 흐름
// =============================================================================

func main() {
	fmt.Println("==================================================================")
	fmt.Println("  Cilium Observability PoC")
	fmt.Println("  Hubble Pipeline / Relay / Prometheus Metrics Simulation")
	fmt.Println("==================================================================")

	// --- 엔드포인트 및 서비스 정의 ---
	endpoints := map[uint16]Endpoint{
		100: {ID: 100, Identity: 1000, Namespace: "default", PodName: "frontend-abc12",
			Labels: []string{"app=frontend", "version=v1"}},
		200: {ID: 200, Identity: 2000, Namespace: "default", PodName: "backend-def34",
			Labels: []string{"app=backend", "version=v1"}},
		300: {ID: 300, Identity: 3000, Namespace: "default", PodName: "database-ghi56",
			Labels: []string{"app=database", "version=v1"}},
		400: {ID: 400, Identity: 4000, Namespace: "kube-system", PodName: "coredns-jkl78",
			Labels: []string{"k8s-app=kube-dns"}},
		500: {ID: 500, Identity: 5000, Namespace: "monitoring", PodName: "prometheus-mno90",
			Labels: []string{"app=prometheus"}},
	}

	services := map[string]Service{
		"default/frontend-abc12":     {Name: "frontend-svc", Namespace: "default"},
		"default/backend-def34":      {Name: "backend-svc", Namespace: "default"},
		"default/database-ghi56":     {Name: "database-svc", Namespace: "default"},
		"kube-system/coredns-jkl78":  {Name: "kube-dns", Namespace: "kube-system"},
		"monitoring/prometheus-mno90": {Name: "prometheus", Namespace: "monitoring"},
	}

	// 통신 패턴 정의 (소스ID, 목적지ID)
	trafficPatterns := [][2]uint16{
		{100, 200}, // frontend -> backend
		{200, 300}, // backend -> database
		{100, 400}, // frontend -> coredns (DNS)
		{200, 400}, // backend -> coredns (DNS)
		{500, 200}, // prometheus -> backend (scrape)
		{500, 300}, // prometheus -> database (scrape)
	}

	// ===================================================================
	// Demo 1: Hubble Observer Pipeline
	// ===================================================================
	fmt.Println("\n[Demo 1] Hubble Observer Pipeline: BPF Event -> Flow Parsing -> Ring Buffer")
	fmt.Println(strings.Repeat("-", 70))

	metrics := NewHubbleMetrics()
	serviceMap := NewServiceMap()

	// 3개 노드에 Observer 생성
	var observers []*LocalObserver
	nodeNames := []string{"node-worker-1", "node-worker-2", "node-worker-3"}

	for _, name := range nodeNames {
		parser := NewFlowParser(endpoints, services)
		obs := NewLocalObserver(name, 1024, parser)

		// OnDecodedFlow 훅: 메트릭 수집 + 서비스 맵 갱신
		obs.AddOnDecodedFlow(func(flow *Flow) bool {
			metrics.ProcessFlow(flow)
			serviceMap.AddFlow(flow)
			return false // 계속 처리
		})

		obs.Start()
		observers = append(observers, obs)
	}

	// BPF 이벤트 생성 (각 노드에서)
	var wg sync.WaitGroup
	for i, obs := range observers {
		wg.Add(1)
		go func(idx int, observer *LocalObserver) {
			defer wg.Done()
			gen := NewBPFEventGenerator(observer)
			gen.GenerateTrafficBurst(30, trafficPatterns)
		}(i, obs)
	}
	wg.Wait()

	// 잠시 대기하여 모든 이벤트 처리 완료
	time.Sleep(100 * time.Millisecond)

	// Observer 상태 출력
	fmt.Println("\nObserver Status:")
	for _, obs := range observers {
		fmt.Printf("  %s\n", obs.Status())
	}

	// ===================================================================
	// Demo 2: Flow Filtering
	// ===================================================================
	fmt.Println("\n[Demo 2] Flow Filtering")
	fmt.Println(strings.Repeat("-", 70))

	// 2.1: 드롭된 Flow만 필터
	dropped := VerdictDropped
	droppedFilter := []FlowFilter{{Verdict: &dropped}}
	droppedFlows := observers[0].GetFlows(droppedFilter, 5)
	fmt.Printf("\nDropped flows on %s (max 5):\n", observers[0].nodeName)
	if len(droppedFlows) == 0 {
		fmt.Println("  (no dropped flows)")
	}
	for _, f := range droppedFlows {
		fmt.Printf("  %s\n", f)
	}

	// 2.2: 특정 네임스페이스 필터
	nsFilter := []FlowFilter{{SourceNS: "default"}}
	nsFlows := observers[0].GetFlows(nsFilter, 5)
	fmt.Printf("\nFlows from namespace 'default' on %s (max 5):\n", observers[0].nodeName)
	for _, f := range nsFlows {
		fmt.Printf("  %s\n", f)
	}

	// 2.3: 프로토콜 필터
	httpProto := ProtoHTTP
	httpFilter := []FlowFilter{{Protocol: &httpProto}}
	httpFlows := observers[0].GetFlows(httpFilter, 5)
	fmt.Printf("\nHTTP flows on %s (max 5):\n", observers[0].nodeName)
	if len(httpFlows) == 0 {
		fmt.Println("  (no HTTP flows)")
	}
	for _, f := range httpFlows {
		fmt.Printf("  %s\n", f)
	}

	// ===================================================================
	// Demo 3: Hubble Relay - Multi-node Aggregation
	// ===================================================================
	fmt.Println("\n[Demo 3] Hubble Relay - Multi-node Flow Aggregation with Timestamp Sorting")
	fmt.Println(strings.Repeat("-", 70))

	relay := NewHubbleRelay(observers, 100)

	fmt.Printf("\nConnected nodes: %v\n", relay.GetConnectedNodes())

	// 모든 노드에서 Flow 수집 및 정렬
	allFlows := relay.GetFlows(nil, 15)
	fmt.Printf("\nAggregated flows (sorted by timestamp, max 15):\n")
	for i, resp := range allFlows {
		fmt.Printf("  [%2d] %s\n", i+1, resp.Flow)
	}

	// 필터링된 집계
	forwarded := VerdictForwarded
	fwdFilter := []FlowFilter{{Verdict: &forwarded}}
	fwdFlows := relay.GetFlows(fwdFilter, 10)
	fmt.Printf("\nForwarded flows across all nodes (max 10): %d flows\n", len(fwdFlows))

	// ===================================================================
	// Demo 4: Prometheus Metrics
	// ===================================================================
	fmt.Println("\n[Demo 4] Prometheus Metrics (Hubble-style)")
	fmt.Println(strings.Repeat("-", 70))

	metrics.ConnectedNodes.Set(int64(len(observers)))

	fmt.Println("\n--- hubble_flows_processed_total ---")
	fmt.Println(metrics.FlowsProcessed.Dump())

	fmt.Println("\n--- hubble_drop_total ---")
	fmt.Println(metrics.DropsTotal.Dump())

	fmt.Println("\n--- hubble_lost_events_total ---")
	fmt.Println(metrics.LostEvents.Dump())

	fmt.Println("\n--- hubble_http_requests_total ---")
	fmt.Println(metrics.HTTPRequests.Dump())

	fmt.Println("\n--- hubble_http_request_duration_seconds ---")
	fmt.Println(metrics.HTTPDuration.Dump())

	fmt.Println("\n--- hubble_active_flows ---")
	fmt.Println(metrics.ActiveFlows.Dump())

	fmt.Println("\n--- hubble_connected_nodes ---")
	fmt.Println(metrics.ConnectedNodes.Dump())

	// ===================================================================
	// Demo 5: Service Map
	// ===================================================================
	fmt.Println("\n[Demo 5] Service Dependency Map (Hubble UI Simulation)")
	fmt.Println(strings.Repeat("-", 70))
	serviceMap.Print()

	// ===================================================================
	// Demo 6: Complete Event Path Visualization
	// ===================================================================
	fmt.Println("\n[Demo 6] Complete BPF Event Collection Path")
	fmt.Println(strings.Repeat("-", 70))

	fmt.Println(`
BPF Datapath (kernel)
    |
    | send_trace_notify() / send_drop_notify()
    v
monitor_output map (BPF perf event array, per-CPU)
    |
    | perf.Reader.Read()    [pkg/monitor/agent/agent.go]
    v
Monitor Agent
    |
    | NotifyPerfEvent(data, cpu)
    v
Hubble Consumer             [pkg/hubble/monitor/consumer.go]
    |
    | MonitorEvent{PerfEvent{Data, CPU}}
    v
Observer events channel     [pkg/hubble/observer/local_observer.go]
    |
    | payloadParser.Decode()
    v
Parser                      [pkg/hubble/parser/parser.go]
    |           |           |           |
    v           v           v           v
  L3/4        L7         Debug       Sock
  Parser     Parser      Parser     Parser
    |           |           |           |
    +-----+-----+-----------+-----------+
          |
          v
     Flow (protobuf)        [api/v1/flow/flow.pb.go]
          |
    +-----+-----+-----+
    |           |     |
    v           v     v
Ring Buffer  Metrics  Exporter
    |           |         |
    v           v         v
GetFlows   Prometheus   File/OTLP
(gRPC)      /metrics    export`)

	// Observer 정리
	for _, obs := range observers {
		obs.Stop()
	}

	fmt.Println("\n\n==================================================================")
	fmt.Println("  PoC Complete - All Cilium Observability mechanisms demonstrated")
	fmt.Println("==================================================================")
}
