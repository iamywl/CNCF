// SPDX-License-Identifier: Apache-2.0
// PoC: Hubble Relay의 Priority Queue 병합 패턴
//
// Hubble Relay는 여러 노드에서 오는 Flow 스트림을
// 타임스탬프 기준으로 정렬하여 하나의 통합 스트림으로 병합합니다.
//
// 실행: go run main.go

package main

import (
	"container/heap"
	"fmt"
	"math/rand"
	"sync"
	"time"
)

// ========================================
// 1. 데이터 모델
// ========================================

type Flow struct {
	Timestamp time.Time
	NodeName  string
	Source    string
	Dest     string
	Verdict  string
}

func (f Flow) String() string {
	return fmt.Sprintf("[%s] %s  %s → %s  %s",
		f.Timestamp.Format("15:04:05.000"),
		f.NodeName,
		f.Source, f.Dest, f.Verdict)
}

// ========================================
// 2. Priority Queue (Min-Heap)
// ========================================

// FlowHeap은 타임스탬프 기준 최소 힙입니다.
// Relay가 여러 노드에서 받은 Flow를 시간순으로 정렬하는 데 사용합니다.
//
// 왜 Priority Queue인가?
//   - 각 노드의 Flow가 네트워크 지연으로 순서 없이 도착
//   - Min-Heap으로 항상 가장 오래된(타임스탬프가 작은) Flow를 먼저 꺼냄
//   - 클라이언트는 시간순으로 정렬된 통합 스트림을 받음
type FlowHeap []Flow

func (h FlowHeap) Len() int           { return len(h) }
func (h FlowHeap) Less(i, j int) bool { return h[i].Timestamp.Before(h[j].Timestamp) }
func (h FlowHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

func (h *FlowHeap) Push(x any) {
	*h = append(*h, x.(Flow))
}

func (h *FlowHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}

// ========================================
// 3. 노드 시뮬레이터 (각 노드의 Hubble Server)
// ========================================

// simulateNode는 각 노드의 Hubble Server가 Flow를 생성하는 것을 시뮬레이션합니다.
//
// 실제 Hubble에서는:
//   - 각 노드의 eBPF가 네트워크 이벤트를 캡처
//   - LocalObserverServer가 Flow를 생성
//   - Relay가 gRPC로 각 노드에 GetFlows 호출
func simulateNode(nodeName string, flowCh chan<- Flow, count int, wg *sync.WaitGroup) {
	defer wg.Done()

	pods := []string{"frontend", "backend", "database"}
	baseTime := time.Now()

	for i := 0; i < count; i++ {
		// 각 노드는 독립적인 타이밍으로 이벤트 생성
		// 네트워크 지연을 시뮬레이션하여 시간이 뒤섞임
		delay := time.Duration(rand.Intn(100)) * time.Millisecond
		eventTime := baseTime.Add(time.Duration(i)*50*time.Millisecond + delay)

		flow := Flow{
			Timestamp: eventTime,
			NodeName:  nodeName,
			Source:    fmt.Sprintf("default/%s", pods[rand.Intn(len(pods))]),
			Dest:     fmt.Sprintf("default/%s", pods[rand.Intn(len(pods))]),
			Verdict:  []string{"FORWARDED", "DROPPED"}[rand.Intn(2)],
		}

		flowCh <- flow
		time.Sleep(30 * time.Millisecond) // 이벤트 발생 간격
	}
}

// ========================================
// 4. Relay (Priority Queue로 병합)
// ========================================

// relay는 여러 노드에서 들어오는 Flow를 Priority Queue로 병합합니다.
//
// 실제 Hubble Relay에서는:
//   - PeerManager가 각 노드의 Hubble Server에 gRPC 연결
//   - 각 노드에서 GetFlows 스트림 수신
//   - Priority Queue(min-heap)로 타임스탬프 기준 정렬
//   - 정렬된 Flow를 클라이언트에 전달
func relay(flowCh <-chan Flow, done <-chan struct{}) []Flow {
	h := &FlowHeap{}
	heap.Init(h)

	var merged []Flow
	batchSize := 3 // 최소 이만큼 쌓인 후 꺼내기 시작 (버퍼링)

	fmt.Println("[Relay] Flow 수집 및 정렬 시작...")

	for {
		select {
		case flow, ok := <-flowCh:
			if !ok {
				// 채널 닫힘 → 남은 것 모두 꺼내기
				for h.Len() > 0 {
					f := heap.Pop(h).(Flow)
					merged = append(merged, f)
				}
				return merged
			}

			heap.Push(h, flow)

			// 충분히 쌓이면 가장 오래된 것부터 꺼냄
			for h.Len() > batchSize {
				f := heap.Pop(h).(Flow)
				merged = append(merged, f)
				fmt.Printf("  [Relay 전달] %s\n", f)
			}

		case <-done:
			return merged
		}
	}
}

// ========================================
// 5. 실행
// ========================================

func main() {
	fmt.Println("=== PoC: Hubble Relay의 Priority Queue 병합 ===")
	fmt.Println()
	fmt.Println("이 PoC는 Relay가 멀티 노드 Flow를 시간순으로 병합하는 과정을 보여줍니다:")
	fmt.Println("  - 3개 노드가 독립적으로 Flow 생성 (타이밍이 제각각)")
	fmt.Println("  - Relay가 Priority Queue(min-heap)로 수집")
	fmt.Println("  - 타임스탬프 기준으로 정렬하여 통합 스트림 생성")
	fmt.Println()
	fmt.Println("-------------------------------------------")
	fmt.Println()

	flowCh := make(chan Flow, 100)
	var wg sync.WaitGroup

	nodes := []string{"node-1", "node-2", "node-3"}
	flowsPerNode := 5

	// 각 노드 시뮬레이터 시작
	for _, node := range nodes {
		wg.Add(1)
		go simulateNode(node, flowCh, flowsPerNode, &wg)
	}

	// 모든 노드가 끝나면 채널 닫기
	go func() {
		wg.Wait()
		close(flowCh)
	}()

	// Relay 병합 실행
	done := make(chan struct{})
	merged := relay(flowCh, done)

	fmt.Println()
	fmt.Println("-------------------------------------------")
	fmt.Println()
	fmt.Println("[결과] 시간순으로 정렬된 통합 스트림:")
	fmt.Println()

	// 정렬 검증
	sorted := true
	for i, f := range merged {
		marker := " "
		if i > 0 && f.Timestamp.Before(merged[i-1].Timestamp) {
			marker = "!"
			sorted = false
		}
		fmt.Printf("  %s %s\n", marker, f)
	}

	fmt.Println()
	if sorted {
		fmt.Println("  ✓ 모든 Flow가 타임스탬프 순서대로 정렬되었습니다!")
	} else {
		fmt.Println("  ✗ 정렬 오류가 있습니다 (버퍼 크기를 늘려보세요)")
	}

	fmt.Println()
	fmt.Println("-------------------------------------------")
	fmt.Println("핵심 포인트:")
	fmt.Println("  - 각 노드의 이벤트는 네트워크 지연으로 순서가 뒤섞임")
	fmt.Println("  - Min-Heap(Priority Queue)으로 타임스탬프 기준 정렬")
	fmt.Println("  - 약간의 버퍼링(batchSize)으로 정렬 정확도 향상")
	fmt.Println("  - 실제 Relay: container/heap 기반 priority queue 사용")
}
