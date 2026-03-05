package main

import (
	"container/heap"
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"
)

// =============================================================================
// Hubble Relay 멀티노드 Flow 집계 시뮬레이션
//
// 실제 구현 참조:
//   cilium/pkg/hubble/relay/observer/server.go      - Server.GetFlows()
//   cilium/pkg/hubble/relay/observer/observer.go     - retrieveFlowsFromPeer(), sortFlows(), aggregateErrors()
//   cilium/pkg/hubble/relay/queue/priority_queue.go  - PriorityQueue, minHeap
//
// 핵심 개념:
//   1. 병렬 수집: errgroup으로 여러 피어에서 동시에 Flow 수집
//   2. PriorityQueue: container/heap 기반 타임스탬프 정렬 큐
//   3. sortFlows: 큐가 가득 차면 가장 오래된 이벤트를 방출 (정렬 윈도우)
//   4. aggregateErrors: 동일 에러 메시지를 가진 노드 상태를 병합
//   5. bufferDrainTimeout: 신규 이벤트 없으면 오래된 이벤트 자동 방출
// =============================================================================

// --- Flow 데이터 모델 ---

type FlowResponse struct {
	TimeSec  int64
	TimeNano int32
	NodeName string
	Source   string
	Dest     string
	Verdict  string
	Summary  string
}

func (r *FlowResponse) GetTime() time.Time {
	return time.Unix(r.TimeSec, int64(r.TimeNano))
}

// --- minHeap: container/heap 인터페이스 구현 ---
// 실제: queue.minHeap

type minHeap []*FlowResponse

var _ heap.Interface = (*minHeap)(nil)

func (h minHeap) Len() int { return len(h) }

// Less: 타임스탬프가 오래된(작은) 것이 높은 우선순위
// 실제: seconds 비교 후 같으면 nanos 비교
func (h minHeap) Less(i, j int) bool {
	if h[i].TimeSec == h[j].TimeSec {
		return h[i].TimeNano < h[j].TimeNano
	}
	return h[i].TimeSec < h[j].TimeSec
}

func (h minHeap) Swap(i, j int) {
	n := len(h)
	if (i >= 0 && i <= n-1) && (j >= 0 && j <= n-1) {
		h[i], h[j] = h[j], h[i]
	}
}

func (h *minHeap) Push(x interface{}) {
	*h = append(*h, x.(*FlowResponse))
}

func (h *minHeap) Pop() interface{} {
	old := *h
	n := len(old)
	if n == 0 {
		return (*FlowResponse)(nil)
	}
	resp := old[n-1]
	old[n-1] = nil // 메모리 누수 방지
	*h = old[0 : n-1]
	return resp
}

// --- PriorityQueue ---
// 실제: queue.PriorityQueue

type PriorityQueue struct {
	h minHeap
}

func NewPriorityQueue(n int) *PriorityQueue {
	h := make(minHeap, 0, n)
	heap.Init(&h)
	return &PriorityQueue{h}
}

func (pq PriorityQueue) Len() int { return pq.h.Len() }

func (pq *PriorityQueue) Push(resp *FlowResponse) {
	heap.Push(&pq.h, resp)
}

func (pq *PriorityQueue) Pop() *FlowResponse {
	if pq.Len() == 0 {
		return nil
	}
	return heap.Pop(&pq.h).(*FlowResponse)
}

// PopOlderThan: t보다 오래된 이벤트 모두 추출 (시간순)
// 실제: PriorityQueue.PopOlderThan()
func (pq *PriorityQueue) PopOlderThan(t time.Time) []*FlowResponse {
	ret := make([]*FlowResponse, 0, pq.Len())
	for {
		resp := pq.Pop()
		if resp == nil {
			return ret
		}
		if t.Before(resp.GetTime()) {
			pq.Push(resp) // 아직 충분히 오래되지 않음
			return ret
		}
		ret = append(ret, resp)
	}
}

// --- 노드 상태 이벤트 ---
// 실제: relaypb.NodeState

type NodeState int

const (
	NodeConnected   NodeState = iota
	NodeUnavailable
	NodeError
)

func (s NodeState) String() string {
	switch s {
	case NodeConnected:
		return "CONNECTED"
	case NodeUnavailable:
		return "UNAVAILABLE"
	case NodeError:
		return "ERROR"
	default:
		return "UNKNOWN"
	}
}

type NodeStatusEvent struct {
	StateChange NodeState
	NodeNames   []string
	Message     string
}

type GetFlowsResponse struct {
	Flow       *FlowResponse
	NodeStatus *NodeStatusEvent
	Time       time.Time
	NodeName   string
}

// --- Peer 모델 ---

type Peer struct {
	Name      string
	Address   string
	Available bool
}

// --- retrieveFlowsFromPeer ---
// 실제: observer.retrieveFlowsFromPeer()
// 각 피어에서 Flow를 수집하여 공유 채널에 전달

func retrieveFlowsFromPeer(
	ctx context.Context,
	peer Peer,
	count int,
	flows chan<- *GetFlowsResponse,
) error {
	for i := 0; i < count; i++ {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		// 각 노드에서 약간 다른 타이밍의 Flow 생성 (실제: gRPC Recv)
		now := time.Now()
		jitter := time.Duration(rand.Intn(50)) * time.Millisecond
		flowTime := now.Add(-jitter)

		flow := &FlowResponse{
			TimeSec:  flowTime.Unix(),
			TimeNano: int32(flowTime.Nanosecond()),
			NodeName: peer.Name,
			Source:   fmt.Sprintf("%s/pod-%d", peer.Name, rand.Intn(5)),
			Dest:     fmt.Sprintf("svc-%d", rand.Intn(3)),
			Verdict:  []string{"FORWARDED", "DROPPED"}[rand.Intn(2)],
			Summary:  fmt.Sprintf("TCP seq=%d", rand.Intn(99999)),
		}

		resp := &GetFlowsResponse{
			Flow:     flow,
			Time:     flowTime,
			NodeName: peer.Name,
		}

		select {
		case flows <- resp:
		case <-ctx.Done():
			return nil
		}

		// 네트워크 지연 시뮬레이션
		time.Sleep(time.Duration(20+rand.Intn(30)) * time.Millisecond)
	}
	return nil
}

// --- flowCollector ---
// 실제: observer.flowCollector
// 여러 피어에서 동시에 Flow를 수집하고, 이미 연결된 노드 추적

type flowCollector struct {
	mu             sync.Mutex
	connectedNodes map[string]struct{}
}

func newFlowCollector() *flowCollector {
	return &flowCollector{
		connectedNodes: make(map[string]struct{}),
	}
}

// collect: 피어 목록을 순회하며 수집 고루틴 시작
// 실제: flowCollector.collect()
func (fc *flowCollector) collect(
	ctx context.Context,
	wg *sync.WaitGroup,
	peers []Peer,
	flows chan *GetFlowsResponse,
	flowsPerPeer int,
) (connected, unavailable []string) {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	for _, p := range peers {
		if _, ok := fc.connectedNodes[p.Name]; ok {
			connected = append(connected, p.Name)
			continue
		}
		if !p.Available {
			fmt.Printf("  [수집기] 피어 %s 연결 불가, 건너뜀\n", p.Name)
			unavailable = append(unavailable, p.Name)
			continue
		}
		connected = append(connected, p.Name)
		fc.connectedNodes[p.Name] = struct{}{}

		peer := p
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := retrieveFlowsFromPeer(ctx, peer, flowsPerPeer, flows)
			if err != nil {
				fc.mu.Lock()
				delete(fc.connectedNodes, peer.Name)
				fc.mu.Unlock()
				flows <- &GetFlowsResponse{
					NodeStatus: &NodeStatusEvent{
						StateChange: NodeError,
						NodeNames:   []string{peer.Name},
						Message:     err.Error(),
					},
					Time:     time.Now(),
					NodeName: peer.Name,
				}
			}
		}()
	}
	return connected, unavailable
}

// --- sortFlows ---
// 실제: observer.sortFlows()
// PriorityQueue를 사용하여 여러 노드의 Flow를 타임스탬프 순으로 정렬
//
// 핵심 알고리즘:
// 1. flows 채널에서 이벤트 수신
// 2. 큐가 가득 차면 (len == qlen) 가장 오래된 것을 방출
// 3. bufferDrainTimeout 동안 이벤트 없으면 오래된 이벤트 자동 방출
// 4. flows 채널이 닫히면 큐 전체 drain

func sortFlows(
	ctx context.Context,
	flows <-chan *GetFlowsResponse,
	qlen int,
	bufferDrainTimeout time.Duration,
) <-chan *GetFlowsResponse {
	pq := NewPriorityQueue(qlen)
	sortedFlows := make(chan *GetFlowsResponse, qlen)

	go func() {
		defer close(sortedFlows)
	flowsLoop:
		for {
			select {
			case resp, ok := <-flows:
				if !ok {
					break flowsLoop
				}
				// NodeStatus 이벤트는 정렬 없이 바로 전달
				if resp.NodeStatus != nil {
					select {
					case sortedFlows <- resp:
					case <-ctx.Done():
						return
					}
					continue
				}
				// 큐가 가득 차면 가장 오래된 이벤트 방출
				if pq.Len() == qlen {
					oldest := pq.Pop()
					select {
					case sortedFlows <- &GetFlowsResponse{
						Flow:     oldest,
						Time:     oldest.GetTime(),
						NodeName: oldest.NodeName,
					}:
					case <-ctx.Done():
						return
					}
				}
				pq.Push(resp.Flow)

			case t := <-time.After(bufferDrainTimeout):
				// 새 이벤트 없을 때: 오래된 이벤트 자동 drain
				// 실제: bufferDrainTimeout을 정렬 윈도우로 사용
				for _, f := range pq.PopOlderThan(t.Add(-bufferDrainTimeout)) {
					select {
					case sortedFlows <- &GetFlowsResponse{
						Flow:     f,
						Time:     f.GetTime(),
						NodeName: f.NodeName,
					}:
					case <-ctx.Done():
						return
					}
				}

			case <-ctx.Done():
				return
			}
		}
		// 채널 닫힘 → 큐 전체 drain
		for f := pq.Pop(); f != nil; f = pq.Pop() {
			select {
			case sortedFlows <- &GetFlowsResponse{
				Flow:     f,
				Time:     f.GetTime(),
				NodeName: f.NodeName,
			}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return sortedFlows
}

// --- aggregateErrors ---
// 실제: observer.aggregateErrors()
// 동일한 에러 메시지를 가진 NodeStatusEvent를 하나로 병합

func aggregateErrors(
	ctx context.Context,
	responses <-chan *GetFlowsResponse,
	errorAggregationWindow time.Duration,
) <-chan *GetFlowsResponse {
	aggregated := make(chan *GetFlowsResponse, cap(responses))

	var flushPending <-chan time.Time
	var pendingResponse *GetFlowsResponse

	go func() {
		defer close(aggregated)
	aggregateLoop:
		for {
			select {
			case resp, ok := <-responses:
				if !ok {
					if pendingResponse != nil {
						select {
						case aggregated <- pendingResponse:
						case <-ctx.Done():
						}
					}
					return
				}

				// Flow 이벤트는 바로 전달
				if resp.NodeStatus == nil {
					select {
					case aggregated <- resp:
						continue aggregateLoop
					case <-ctx.Done():
						return
					}
				}

				// 에러가 아닌 상태 이벤트도 바로 전달
				if resp.NodeStatus.StateChange != NodeError {
					select {
					case aggregated <- resp:
						continue aggregateLoop
					case <-ctx.Done():
						return
					}
				}

				// 에러 병합: 동일 메시지면 노드 이름 합침
				if pendingResponse != nil && pendingResponse.NodeStatus != nil {
					if resp.NodeStatus.Message == pendingResponse.NodeStatus.Message {
						pendingResponse.NodeStatus.NodeNames = append(
							pendingResponse.NodeStatus.NodeNames,
							resp.NodeStatus.NodeNames...,
						)
						continue aggregateLoop
					}
					// 다른 에러 → 기존 pending flush
					select {
					case aggregated <- pendingResponse:
					case <-ctx.Done():
						return
					}
				}

				pendingResponse = resp
				flushPending = time.After(errorAggregationWindow)

			case <-flushPending:
				select {
				case aggregated <- pendingResponse:
					pendingResponse = nil
					flushPending = nil
				case <-ctx.Done():
					return
				}

			case <-ctx.Done():
				return
			}
		}
	}()
	return aggregated
}

// --- GetFlows (Relay 서버) ---
// 실제: Server.GetFlows()
// 전체 파이프라인: peers.List() → collect() → aggregateErrors() → sortFlows() → Send

func relayGetFlows(
	ctx context.Context,
	peers []Peer,
	flowsPerPeer int,
	qlen int,
	sortTimeout time.Duration,
	errorWindow time.Duration,
) <-chan *GetFlowsResponse {
	output := make(chan *GetFlowsResponse, qlen)

	go func() {
		defer close(output)

		var wg sync.WaitGroup
		flows := make(chan *GetFlowsResponse, qlen)

		fc := newFlowCollector()
		connected, unavailable := fc.collect(ctx, &wg, peers, flows, flowsPerPeer)

		// WaitGroup 완료 시 flows 채널 닫기
		go func() {
			wg.Wait()
			close(flows)
		}()

		// 노드 상태 알림 (실제: stream.Send(nodeStatusEvent(...)))
		if len(connected) > 0 {
			output <- &GetFlowsResponse{
				NodeStatus: &NodeStatusEvent{
					StateChange: NodeConnected,
					NodeNames:   connected,
				},
				Time: time.Now(),
			}
		}
		if len(unavailable) > 0 {
			output <- &GetFlowsResponse{
				NodeStatus: &NodeStatusEvent{
					StateChange: NodeUnavailable,
					NodeNames:   unavailable,
				},
				Time: time.Now(),
			}
		}

		// 파이프라인: flows → aggregateErrors → sortFlows → output
		aggregated := aggregateErrors(ctx, flows, errorWindow)
		sorted := sortFlows(ctx, aggregated, qlen, sortTimeout)

		for resp := range sorted {
			select {
			case output <- resp:
			case <-ctx.Done():
				return
			}
		}
	}()

	return output
}

func main() {
	fmt.Println("=== Hubble Relay 멀티노드 Flow 집계 시뮬레이션 ===")
	fmt.Println()
	fmt.Println("참조: cilium/pkg/hubble/relay/observer/server.go      - Server.GetFlows()")
	fmt.Println("참조: cilium/pkg/hubble/relay/observer/observer.go     - retrieveFlowsFromPeer(), sortFlows()")
	fmt.Println("참조: cilium/pkg/hubble/relay/queue/priority_queue.go  - PriorityQueue, minHeap")
	fmt.Println()

	// === 테스트 1: PriorityQueue 기본 동작 ===
	fmt.Println("--- 테스트 1: PriorityQueue 기본 동작 (타임스탬프 정렬) ---")
	fmt.Println()

	pq := NewPriorityQueue(10)
	baseTime := time.Now()

	// 순서 섞어서 삽입
	offsets := []time.Duration{
		500 * time.Millisecond,
		100 * time.Millisecond,
		300 * time.Millisecond,
		50 * time.Millisecond,
		200 * time.Millisecond,
	}
	nodes := []string{"node-A", "node-B", "node-C", "node-A", "node-B"}
	for i, off := range offsets {
		t := baseTime.Add(off)
		pq.Push(&FlowResponse{
			TimeSec:  t.Unix(),
			TimeNano: int32(t.Nanosecond()),
			NodeName: nodes[i],
			Source:   fmt.Sprintf("%s/pod-%d", nodes[i], i),
		})
		fmt.Printf("  Push: %s (offset=%v)\n", nodes[i], off)
	}
	fmt.Println()

	fmt.Println("  Pop 순서 (타임스탬프 오름차순):")
	for pq.Len() > 0 {
		f := pq.Pop()
		offset := f.GetTime().Sub(baseTime).Truncate(time.Millisecond)
		fmt.Printf("    %s: offset=%v, source=%s\n", f.NodeName, offset, f.Source)
	}
	fmt.Println()

	// === 테스트 2: PopOlderThan ===
	fmt.Println("--- 테스트 2: PopOlderThan (정렬 윈도우) ---")
	fmt.Println()

	pq2 := NewPriorityQueue(20)
	base2 := time.Now().Add(-5 * time.Second)

	for i := 0; i < 10; i++ {
		t := base2.Add(time.Duration(i*500) * time.Millisecond)
		pq2.Push(&FlowResponse{
			TimeSec:  t.Unix(),
			TimeNano: int32(t.Nanosecond()),
			NodeName: fmt.Sprintf("node-%d", i%3),
			Source:   fmt.Sprintf("event-at-T+%dms", i*500),
		})
	}

	cutoff := base2.Add(2 * time.Second)
	older := pq2.PopOlderThan(cutoff)
	fmt.Printf("  cutoff=T+2초 이전 이벤트: %d개 추출\n", len(older))
	for _, f := range older {
		offset := f.GetTime().Sub(base2).Truncate(time.Millisecond)
		fmt.Printf("    %s (offset=%v)\n", f.Source, offset)
	}
	fmt.Printf("  큐에 남은 이벤트: %d개\n\n", pq2.Len())

	// === 테스트 3: sortFlows 파이프라인 ===
	fmt.Println("--- 테스트 3: sortFlows 파이프라인 (큐 크기 3) ---")
	fmt.Println("  (큐가 가득 차면 가장 오래된 이벤트 방출)")
	fmt.Println()

	inFlows := make(chan *GetFlowsResponse, 10)
	ctx3, cancel3 := context.WithCancel(context.Background())
	defer cancel3()

	sorted := sortFlows(ctx3, inFlows, 3, 500*time.Millisecond)

	// 순서가 뒤섞인 5개 Flow 삽입
	base3 := time.Now()
	insertOrder := []struct {
		offset time.Duration
		node   string
	}{
		{300 * time.Millisecond, "node-C"},
		{100 * time.Millisecond, "node-A"},
		{500 * time.Millisecond, "node-B"},
		{200 * time.Millisecond, "node-A"},
		{400 * time.Millisecond, "node-C"},
	}

	for _, item := range insertOrder {
		t := base3.Add(item.offset)
		inFlows <- &GetFlowsResponse{
			Flow: &FlowResponse{
				TimeSec:  t.Unix(),
				TimeNano: int32(t.Nanosecond()),
				NodeName: item.node,
				Source:   fmt.Sprintf("offset=%v", item.offset),
			},
			Time:     t,
			NodeName: item.node,
		}
	}
	close(inFlows)

	fmt.Println("  정렬된 출력:")
	for resp := range sorted {
		if resp.Flow != nil {
			offset := resp.Flow.GetTime().Sub(base3).Truncate(time.Millisecond)
			fmt.Printf("    %s: %s (offset=%v)\n", resp.Flow.NodeName, resp.Flow.Source, offset)
		}
	}
	fmt.Println()

	// === 테스트 4: 전체 Relay 파이프라인 ===
	fmt.Println("--- 테스트 4: Relay 전체 파이프라인 (3개 노드, 각 5개 Flow) ---")
	fmt.Println("  peers → collect → aggregateErrors → sortFlows → output")
	fmt.Println()

	peers := []Peer{
		{Name: "k8s-node-0", Address: "10.0.0.1:4244", Available: true},
		{Name: "k8s-node-1", Address: "10.0.0.2:4244", Available: true},
		{Name: "k8s-node-2", Address: "10.0.0.3:4244", Available: true},
		{Name: "k8s-node-3", Address: "10.0.0.4:4244", Available: false}, // 연결 불가
	}

	ctx4, cancel4 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel4()

	results := relayGetFlows(ctx4, peers, 5, 10, 200*time.Millisecond, 100*time.Millisecond)

	flowCount := 0
	var prevTime time.Time
	outOfOrder := 0

	for resp := range results {
		if resp.NodeStatus != nil {
			ns := resp.NodeStatus
			fmt.Printf("  [상태] %s: %v\n", ns.StateChange, ns.NodeNames)
			continue
		}
		if resp.Flow != nil {
			flowCount++
			t := resp.Flow.GetTime()
			if !prevTime.IsZero() && t.Before(prevTime) {
				outOfOrder++
			}
			if flowCount <= 10 {
				fmt.Printf("  [Flow #%02d] %s %s -> %s [%s] (%s)\n",
					flowCount,
					t.Format("15:04:05.000000"),
					resp.Flow.Source,
					resp.Flow.Dest,
					resp.Flow.Verdict,
					resp.Flow.NodeName,
				)
			}
			prevTime = t
		}
	}
	if flowCount > 10 {
		fmt.Printf("  ... (이하 %d개 생략)\n", flowCount-10)
	}
	fmt.Printf("\n  총 %d개 Flow 수신, 정렬 위반: %d건\n\n", flowCount, outOfOrder)

	// === 테스트 5: 에러 집계 ===
	fmt.Println("--- 테스트 5: 에러 집계 (동일 에러 병합) ---")
	fmt.Println()

	errCh := make(chan *GetFlowsResponse, 10)
	ctx5, cancel5 := context.WithCancel(context.Background())
	defer cancel5()

	aggregated := aggregateErrors(ctx5, errCh, 200*time.Millisecond)

	// 동일 에러 3개를 빠르게 전송
	for _, node := range []string{"node-A", "node-B", "node-C"} {
		errCh <- &GetFlowsResponse{
			NodeStatus: &NodeStatusEvent{
				StateChange: NodeError,
				NodeNames:   []string{node},
				Message:     "connection refused",
			},
			Time: time.Now(),
		}
	}
	// 다른 에러 1개
	errCh <- &GetFlowsResponse{
		NodeStatus: &NodeStatusEvent{
			StateChange: NodeError,
			NodeNames:   []string{"node-D"},
			Message:     "timeout",
		},
		Time: time.Now(),
	}
	close(errCh)

	for resp := range aggregated {
		if resp.NodeStatus != nil {
			ns := resp.NodeStatus
			fmt.Printf("  [집계된 에러] %s: nodes=%v, message=%q\n",
				ns.StateChange, ns.NodeNames, ns.Message)
		}
	}
	fmt.Println()

	fmt.Println("=== 시뮬레이션 완료 ===")
	fmt.Println()
	fmt.Println("핵심 패턴 요약:")
	fmt.Println("  1. 병렬 수집: flowCollector가 각 피어에 goroutine 배정")
	fmt.Println("     → 실제: errgroup.Go(func() { retrieveFlowsFromPeer(...) })")
	fmt.Println("  2. PriorityQueue: container/heap 기반 min-heap")
	fmt.Println("     → Less(): seconds 비교 후 nanos 비교 (가장 오래된 것이 최우선)")
	fmt.Println("  3. sortFlows: 큐가 qlen에 도달하면 oldest Pop → 정렬 윈도우 역할")
	fmt.Println("     → bufferDrainTimeout: 이벤트 없으면 오래된 것 자동 방출")
	fmt.Println("  4. aggregateErrors: 동일 에러 메시지의 노드 이름을 병합")
	fmt.Println("     → errorAggregationWindow 시간 내 동일 에러 합침")
	fmt.Println("  5. 전체 파이프라인: collect → aggregateErrors → sortFlows → Send")
}
