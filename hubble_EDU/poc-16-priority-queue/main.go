package main

import (
	"container/heap"
	"fmt"
	"math/rand"
	"time"
)

// =============================================================================
// Hubble 우선순위 큐 시뮬레이션
//
// 실제 구현 참조:
//   cilium/pkg/hubble/relay/queue/priority_queue.go - PriorityQueue, minHeap
//
// 핵심 개념:
//   1. container/heap 인터페이스: Len, Less, Swap, Push, Pop
//   2. 타임스탬프 기반 정렬: 오래된 이벤트가 높은 우선순위
//   3. PopOlderThan: 특정 시간보다 오래된 이벤트만 추출
//   4. 다중 소스 병합: 여러 Hubble 노드의 Flow를 시간순 병합
//
// Hubble Relay는 여러 노드의 Flow 스트림을 받아서
// 시간순으로 정렬하여 클라이언트에게 전달한다.
// PriorityQueue가 이 병합 정렬의 핵심 자료구조이다.
// =============================================================================

// --- GetFlowsResponse 시뮬레이션 ---
// 실제: observerpb.GetFlowsResponse

type FlowResponse struct {
	TimeSec  int64 // 초 (seconds since epoch)
	TimeNano int32 // 나노초 부분
	NodeName string
	Source   string
	Dest     string
	Verdict  string
	Summary  string
}

func (r *FlowResponse) GetTimeSec() int64  { return r.TimeSec }
func (r *FlowResponse) GetTimeNano() int32 { return r.TimeNano }
func (r *FlowResponse) GetTime() time.Time {
	return time.Unix(r.TimeSec, int64(r.TimeNano))
}

// --- minHeap ---
// 실제: queue.minHeap

type minHeap []*FlowResponse

// heap.Interface 구현 보장
var _ heap.Interface = (*minHeap)(nil)

func (h minHeap) Len() int { return len(h) }

// Less는 타임스탬프 비교: 오래된 것이 우선
// 실제: minHeap.Less()
// 초가 같으면 나노초로 비교
func (h minHeap) Less(i, j int) bool {
	if h[i].GetTimeSec() == h[j].GetTimeSec() {
		return h[i].GetTimeNano() < h[j].GetTimeNano()
	}
	return h[i].GetTimeSec() < h[j].GetTimeSec()
}

// Swap은 두 요소를 교환
func (h minHeap) Swap(i, j int) {
	n := len(h)
	if (i >= 0 && i <= n-1) && (j >= 0 && j <= n-1) {
		h[i], h[j] = h[j], h[i]
	}
}

// Push는 heap.Push에 의해 호출
func (h *minHeap) Push(x interface{}) {
	resp := x.(*FlowResponse)
	*h = append(*h, resp)
}

// Pop은 heap.Pop에 의해 호출
// 마지막 요소를 반환하고 슬라이스 축소
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

// NewPriorityQueue는 초기 용량 n으로 우선순위 큐 생성
// 실제: queue.NewPriorityQueue(n)
func NewPriorityQueue(n int) *PriorityQueue {
	h := make(minHeap, 0, n)
	heap.Init(&h)
	return &PriorityQueue{h}
}

// Len은 큐의 요소 수
func (pq PriorityQueue) Len() int {
	return pq.h.Len()
}

// Push는 응답을 큐에 추가
// 실제: PriorityQueue.Push()
func (pq *PriorityQueue) Push(resp *FlowResponse) {
	heap.Push(&pq.h, resp)
}

// Pop은 가장 오래된(타임스탬프 가장 작은) 응답을 제거하고 반환
// 실제: PriorityQueue.Pop()
func (pq *PriorityQueue) Pop() *FlowResponse {
	if pq.Len() == 0 {
		return nil
	}
	resp := heap.Pop(&pq.h).(*FlowResponse)
	return resp
}

// PopOlderThan은 t보다 오래된 모든 응답을 시간순으로 반환
// 실제: PriorityQueue.PopOlderThan()
//
// 이 메서드는 drain timeout과 함께 사용된다:
// - 각 노드에서 일정 시간 동안 이벤트를 버퍼링
// - drain timeout 이후 모든 오래된 이벤트를 한꺼번에 추출
// - 이로써 여러 노드의 이벤트가 시간순으로 정렬됨
func (pq *PriorityQueue) PopOlderThan(t time.Time) []*FlowResponse {
	// 전체 큐를 flush하는 것이 흔한 패턴이므로 미리 충분히 할당
	ret := make([]*FlowResponse, 0, pq.Len())
	for {
		resp := pq.Pop()
		if resp == nil {
			return ret
		}
		if t.Before(resp.GetTime()) {
			// 이 응답은 아직 충분히 오래되지 않음 → 다시 큐에 넣기
			pq.Push(resp)
			return ret
		}
		ret = append(ret, resp)
	}
}

// --- 다중 스트림 병합 정렬 시뮬레이션 ---
// 실제: Hubble Relay의 Flow 병합 로직

type NodeStream struct {
	NodeName string
	Flows    []*FlowResponse
}

// MergeStreams는 여러 노드의 Flow 스트림을 시간순으로 병합
// Hubble Relay가 여러 Hubble 인스턴스의 스트림을 받아 처리하는 패턴
func MergeStreams(streams []NodeStream, drainTimeout time.Duration) []*FlowResponse {
	pq := NewPriorityQueue(100)

	// 모든 스트림의 이벤트를 큐에 넣기
	for _, stream := range streams {
		for _, flow := range stream.Flows {
			pq.Push(flow)
		}
	}

	fmt.Printf("  큐에 총 %d개 이벤트 삽입\n", pq.Len())

	// drain timeout 적용하여 추출
	// 실제에서는 가장 최근 이벤트로부터 drainTimeout만큼 이전까지의 이벤트만 추출
	// 이렇게 하면 아직 도착하지 않은 이벤트가 있을 수 있는 시간대는 제외
	drainBefore := time.Now().Add(-drainTimeout)
	result := pq.PopOlderThan(drainBefore)

	fmt.Printf("  drain timeout (%v) 적용 후 %d개 추출, %d개 버퍼에 남음\n",
		drainTimeout, len(result), pq.Len())

	// 남은 이벤트도 모두 추출 (최종 flush)
	remaining := pq.PopOlderThan(time.Now().Add(1 * time.Hour)) // 모두 추출
	result = append(result, remaining...)

	return result
}

// --- 테스트 데이터 생성 ---

func generateNodeStream(nodeName string, count int, baseTime time.Time) NodeStream {
	flows := make([]*FlowResponse, count)
	for i := 0; i < count; i++ {
		// 각 노드의 이벤트 시간에 약간의 랜덤 지터 추가
		offset := time.Duration(i*100+rand.Intn(50)) * time.Millisecond
		t := baseTime.Add(offset)
		flows[i] = &FlowResponse{
			TimeSec:  t.Unix(),
			TimeNano: int32(t.Nanosecond()),
			NodeName: nodeName,
			Source:   fmt.Sprintf("%s/pod-%d", nodeName, rand.Intn(5)),
			Dest:     fmt.Sprintf("svc-%d", rand.Intn(3)),
			Verdict:  []string{"FORWARDED", "DROPPED"}[rand.Intn(2)],
			Summary:  fmt.Sprintf("TCP seq=%d", rand.Intn(99999)),
		}
	}
	return NodeStream{NodeName: nodeName, Flows: flows}
}

func main() {
	fmt.Println("=== Hubble 우선순위 큐 시뮬레이션 ===")
	fmt.Println()
	fmt.Println("참조: cilium/pkg/hubble/relay/queue/priority_queue.go")
	fmt.Println()
	fmt.Println("Hubble Relay는 여러 노드의 Flow 스트림을 받아서")
	fmt.Println("PriorityQueue로 타임스탬프 기반 병합 정렬을 수행합니다.")
	fmt.Println()

	// === 테스트 1: 기본 Push/Pop ===
	fmt.Println("--- 테스트 1: 기본 Push/Pop (타임스탬프 순 추출) ---")
	fmt.Println()

	pq := NewPriorityQueue(10)
	baseTime := time.Now()

	// 의도적으로 순서 섞어서 삽입
	times := []time.Duration{
		300 * time.Millisecond,
		100 * time.Millisecond,
		500 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
	}
	for i, d := range times {
		t := baseTime.Add(d)
		pq.Push(&FlowResponse{
			TimeSec:  t.Unix(),
			TimeNano: int32(t.Nanosecond()),
			NodeName: fmt.Sprintf("node-%d", i),
			Source:   fmt.Sprintf("pod-%d", i),
		})
		fmt.Printf("  Push: node-%d (offset=%v)\n", i, d)
	}
	fmt.Println()

	fmt.Println("  Pop 순서 (타임스탬프 순):")
	for pq.Len() > 0 {
		resp := pq.Pop()
		offset := resp.GetTime().Sub(baseTime)
		fmt.Printf("    %s: offset=%v (source=%s)\n",
			resp.NodeName, offset.Truncate(time.Millisecond), resp.Source)
	}
	fmt.Println()

	// === 테스트 2: 동일 초(second)에서 나노초 비교 ===
	fmt.Println("--- 테스트 2: 나노초 수준 정렬 ---")
	fmt.Println()

	pq2 := NewPriorityQueue(5)
	sameSecond := time.Now().Truncate(time.Second)

	nanos := []int{999000000, 100000000, 500000000, 250000000, 750000000}
	for i, ns := range nanos {
		t := sameSecond.Add(time.Duration(ns))
		pq2.Push(&FlowResponse{
			TimeSec:  t.Unix(),
			TimeNano: int32(t.Nanosecond()),
			NodeName: fmt.Sprintf("node-%d", i),
		})
		fmt.Printf("  Push: node-%d (nano=%d)\n", i, ns)
	}
	fmt.Println()

	fmt.Println("  Pop 순서 (나노초 순):")
	for pq2.Len() > 0 {
		resp := pq2.Pop()
		fmt.Printf("    %s: nano=%d\n", resp.NodeName, resp.GetTimeNano())
	}
	fmt.Println()

	// === 테스트 3: PopOlderThan ===
	fmt.Println("--- 테스트 3: PopOlderThan (시간 기준 부분 추출) ---")
	fmt.Println()

	pq3 := NewPriorityQueue(10)
	base := time.Now().Add(-5 * time.Second)

	for i := 0; i < 10; i++ {
		t := base.Add(time.Duration(i) * time.Second)
		pq3.Push(&FlowResponse{
			TimeSec:  t.Unix(),
			TimeNano: int32(t.Nanosecond()),
			NodeName: fmt.Sprintf("node-%d", i),
			Source:   fmt.Sprintf("event-at-T+%ds", i),
		})
	}

	// base+3초 이전의 이벤트만 추출
	cutoff := base.Add(3 * time.Second)
	older := pq3.PopOlderThan(cutoff)
	fmt.Printf("  cutoff=T+3초 이전 이벤트: %d개 추출\n", len(older))
	for _, r := range older {
		offset := r.GetTime().Sub(base).Truncate(time.Second)
		fmt.Printf("    %s (offset=%v)\n", r.Source, offset)
	}
	fmt.Printf("  큐에 남은 이벤트: %d개\n", pq3.Len())
	fmt.Println()

	// === 테스트 4: 다중 노드 스트림 병합 ===
	fmt.Println("--- 테스트 4: 다중 노드 스트림 병합 (Relay 패턴) ---")
	fmt.Println()

	streamBase := time.Now().Add(-2 * time.Second)
	streams := []NodeStream{
		generateNodeStream("node-0", 5, streamBase),
		generateNodeStream("node-1", 5, streamBase.Add(25*time.Millisecond)), // 약간의 시간차
		generateNodeStream("node-2", 5, streamBase.Add(50*time.Millisecond)),
	}

	merged := MergeStreams(streams, 500*time.Millisecond)
	fmt.Printf("\n  병합 결과 (총 %d개, 시간순 정렬):\n", len(merged))

	prevTime := time.Time{}
	outOfOrder := 0
	for i, r := range merged {
		t := r.GetTime()
		marker := " "
		if !prevTime.IsZero() && t.Before(prevTime) {
			marker = "!"
			outOfOrder++
		}
		if i < 10 { // 처음 10개만 출력
			fmt.Printf("  %s [%d] %s %s -> %s (%s) %s\n",
				marker, i,
				t.Format("15:04:05.000000"),
				r.Source, r.Dest, r.NodeName, r.Verdict)
		}
		prevTime = t
	}
	if len(merged) > 10 {
		fmt.Printf("    ... (이하 %d개 생략)\n", len(merged)-10)
	}
	fmt.Printf("\n  정렬 검증: 순서 위반 %d건 (0이면 정렬 정상)\n", outOfOrder)
	fmt.Println()

	// === 테스트 5: 빈 큐 처리 ===
	fmt.Println("--- 테스트 5: 엣지 케이스 ---")
	fmt.Println()

	emptyPQ := NewPriorityQueue(0)
	fmt.Printf("  빈 큐 Pop: %v\n", emptyPQ.Pop())
	fmt.Printf("  빈 큐 PopOlderThan: %v\n", emptyPQ.PopOlderThan(time.Now()))
	fmt.Printf("  빈 큐 Len: %d\n", emptyPQ.Len())
	fmt.Println()

	// 단일 요소
	singlePQ := NewPriorityQueue(1)
	singlePQ.Push(&FlowResponse{
		TimeSec: time.Now().Unix(), TimeNano: 0, NodeName: "only-one",
	})
	fmt.Printf("  단일 요소 Len: %d\n", singlePQ.Len())
	r := singlePQ.Pop()
	fmt.Printf("  단일 요소 Pop: %s\n", r.NodeName)
	fmt.Printf("  Pop 후 Len: %d\n", singlePQ.Len())
	fmt.Println()

	// === 테스트 6: 대량 데이터 성능 ===
	fmt.Println("--- 테스트 6: 성능 테스트 (10,000개 이벤트) ---")
	fmt.Println()

	perfPQ := NewPriorityQueue(10000)
	start := time.Now()

	for i := 0; i < 10000; i++ {
		t := time.Now().Add(-time.Duration(rand.Intn(10000)) * time.Millisecond)
		perfPQ.Push(&FlowResponse{
			TimeSec:  t.Unix(),
			TimeNano: int32(t.Nanosecond()),
			NodeName: fmt.Sprintf("node-%d", rand.Intn(10)),
		})
	}
	pushDuration := time.Since(start)
	fmt.Printf("  Push 10,000개: %v\n", pushDuration)

	start = time.Now()
	// 절반을 PopOlderThan으로 추출
	mid := time.Now().Add(-5000 * time.Millisecond)
	olderHalf := perfPQ.PopOlderThan(mid)
	popDuration := time.Since(start)
	fmt.Printf("  PopOlderThan: %d개 추출 / %d개 남음, %v\n",
		len(olderHalf), perfPQ.Len(), popDuration)

	// 나머지 전체 Pop
	start = time.Now()
	for perfPQ.Len() > 0 {
		perfPQ.Pop()
	}
	drainDuration := time.Since(start)
	fmt.Printf("  나머지 drain: %v\n", drainDuration)
	fmt.Println()

	fmt.Println("=== 시뮬레이션 완료 ===")
	fmt.Println()
	fmt.Println("핵심 패턴 요약:")
	fmt.Println("  1. container/heap 인터페이스: Len/Less/Swap/Push/Pop 5개 메서드")
	fmt.Println("  2. minHeap: 가장 오래된(타임스탬프가 작은) 이벤트가 최우선")
	fmt.Println("  3. Less(): seconds 비교 → 같으면 nanos 비교")
	fmt.Println("  4. PopOlderThan: drain timeout 이전의 이벤트만 추출")
	fmt.Println("  5. 다중 스트림 병합: Relay가 여러 노드의 Flow를 시간순 정렬")
	fmt.Println("  6. 메모리 관리: Pop에서 old[n-1] = nil로 메모리 누수 방지")
}
