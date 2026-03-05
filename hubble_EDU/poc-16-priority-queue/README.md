# PoC-16: 타임스탬프 기반 우선순위 큐

## 개요

Hubble Relay의 Flow 병합 정렬에 사용되는 `PriorityQueue`를 시뮬레이션한다. Go 표준 라이브러리의 `container/heap` 인터페이스를 구현하여, 여러 노드에서 수집된 Flow를 타임스탬프 오름차순으로 정렬한다.

이 자료구조는 Relay가 여러 Hubble 인스턴스에서 수신한 Flow 스트림을 단일 시간순 스트림으로 병합하는 핵심 역할을 한다.

## 핵심 개념

### 1. container/heap 인터페이스

Go 표준 라이브러리의 `heap.Interface`를 구현한다:

```go
type Interface interface {
    sort.Interface        // Len, Less, Swap
    Push(x interface{})   // 요소 추가
    Pop() interface{}     // 요소 제거
}
```

### 2. minHeap (Less 함수)

가장 오래된 타임스탬프가 최우선순위를 가지는 min-heap이다:

```go
func (h minHeap) Less(i, j int) bool {
    if h[i].GetTimeSec() == h[j].GetTimeSec() {
        return h[i].GetTimeNano() < h[j].GetTimeNano()
    }
    return h[i].GetTimeSec() < h[j].GetTimeSec()
}
```

seconds를 먼저 비교하고, 같으면 nanos를 비교한다.

### 3. PopOlderThan

특정 시간 t보다 오래된 모든 이벤트를 시간순으로 추출한다. Relay의 `sortFlows()`에서 `bufferDrainTimeout`과 함께 사용된다:

- 각 노드에서 일정 시간 동안 이벤트를 버퍼링
- drain timeout 이후 오래된 이벤트를 한꺼번에 추출
- 아직 도착하지 않은 이벤트가 있을 수 있는 시간대는 큐에 남겨둠

### 4. 메모리 관리

`Pop()`에서 `old[n-1] = nil`을 설정하여 GC가 제거된 요소를 회수할 수 있게 한다. 이는 Go slice의 메모리 누수를 방지하는 표준 패턴이다.

### 5. 다중 스트림 병합 정렬

```
Node-0: [t1, t4, t7, ...]  ─┐
Node-1: [t2, t5, t8, ...]  ─┤──→ PriorityQueue ──→ [t1, t2, t3, t4, t5, ...]
Node-2: [t3, t6, t9, ...]  ─┘     (min-heap)        (시간순 정렬)
```

## 실행 방법

```bash
go run main.go
```

6가지 테스트를 실행한다: 기본 Push/Pop, 나노초 수준 정렬, PopOlderThan, 다중 노드 스트림 병합, 엣지 케이스, 10,000개 이벤트 성능 테스트.

## 실제 소스코드 참조

| 파일 | 핵심 함수/구조체 |
|------|-----------------|
| `cilium/pkg/hubble/relay/queue/priority_queue.go` | `PriorityQueue` - 공개 인터페이스 (Push/Pop/Len/PopOlderThan) |
| `cilium/pkg/hubble/relay/queue/priority_queue.go` | `minHeap` - heap.Interface 구현 |
| `cilium/pkg/hubble/relay/observer/observer.go` | `sortFlows()` - PriorityQueue를 사용한 정렬 파이프라인 |
| `cilium/pkg/hubble/relay/observer/server.go` | `Server.GetFlows()` - 전체 병합 정렬 흐름 |
