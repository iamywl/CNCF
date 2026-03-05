# PoC-07: Relay 멀티노드 Flow 집계

## 개요

Hubble Relay가 여러 노드에서 Flow를 병렬로 수집하고, 타임스탬프 기반 우선순위 큐로 정렬하여 단일 스트림으로 병합하는 과정을 시뮬레이션한다.

Relay는 클러스터의 모든 Hubble 인스턴스에 동시 연결하여 Flow를 수집한다. 각 노드에서 도착하는 Flow의 순서가 보장되지 않으므로, `container/heap` 기반 PriorityQueue를 사용하여 타임스탬프 순으로 정렬한 뒤 클라이언트에 전달한다.

## 핵심 개념

### 1. 병렬 수집 (flowCollector)

`flowCollector.collect()`는 피어 목록을 순회하며 각 피어에 goroutine을 배정한다. 이미 연결된 노드는 중복 수집하지 않는다.

```
┌─────────┐    ┌─────────┐    ┌─────────┐
│ node-0  │    │ node-1  │    │ node-2  │
│ Hubble  │    │ Hubble  │    │ Hubble  │
└────┬────┘    └────┬────┘    └────┬────┘
     │              │              │
     │  goroutine   │  goroutine   │  goroutine
     │              │              │
     └──────────────┼──────────────┘
                    ▼
            flows chan (공유)
```

### 2. PriorityQueue (min-heap)

`container/heap` 인터페이스(Len, Less, Swap, Push, Pop)를 구현한 min-heap이다. 타임스탬프가 가장 오래된 Flow가 최우선순위를 가진다.

- `Less()`: seconds 비교 후 같으면 nanos 비교
- `Pop()`: 메모리 누수 방지를 위해 `old[n-1] = nil` 처리
- `PopOlderThan(t)`: 시간 t 이전의 모든 이벤트를 시간순으로 추출

### 3. sortFlows 파이프라인

큐 크기(qlen)가 정렬 윈도우 역할을 한다:

1. flows 채널에서 이벤트 수신
2. 큐가 가득 차면(len == qlen) 가장 오래된 이벤트를 방출
3. `bufferDrainTimeout` 동안 이벤트 없으면 오래된 이벤트 자동 방출
4. flows 채널이 닫히면 큐 전체 drain

### 4. aggregateErrors

동일한 에러 메시지를 가진 NodeStatusEvent를 `errorAggregationWindow` 시간 내에 하나로 병합한다. 이로써 여러 노드에서 같은 에러가 발생할 때 클라이언트에게 단일 에러 이벤트로 전달된다.

### 5. 전체 파이프라인

```
peers.List()
    │
    ▼
flowCollector.collect()  ──→  flows chan
    │                              │
    │                              ▼
    │                     aggregateErrors()  ──→  aggregated chan
    │                                                  │
    │                                                  ▼
    │                                          sortFlows()  ──→  sortedFlows chan
    │                                                                  │
    ▼                                                                  ▼
NodeStatus(CONNECTED)                                          stream.Send()
NodeStatus(UNAVAILABLE)
```

## 실행 방법

```bash
go run main.go
```

외부 의존성 없이 Go 표준 라이브러리만 사용한다 (`container/heap`, `context`, `sync`).

## 테스트 항목

| 테스트 | 내용 |
|--------|------|
| 테스트 1 | PriorityQueue Push/Pop - 타임스탬프 오름차순 추출 검증 |
| 테스트 2 | PopOlderThan - 시간 기준 부분 추출 |
| 테스트 3 | sortFlows 파이프라인 - 큐 크기 제한, 자동 방출 |
| 테스트 4 | Relay 전체 파이프라인 - 3개 노드 병렬 수집 + 정렬 |
| 테스트 5 | 에러 집계 - 동일 에러 메시지 노드 병합 |

## 실제 소스코드 참조

| 파일 | 핵심 함수/구조체 |
|------|-----------------|
| `cilium/pkg/hubble/relay/observer/server.go` | `Server.GetFlows()` - 전체 파이프라인 진입점 |
| `cilium/pkg/hubble/relay/observer/observer.go` | `retrieveFlowsFromPeer()` - 피어별 Flow 수집 |
| `cilium/pkg/hubble/relay/observer/observer.go` | `sortFlows()` - PriorityQueue 기반 정렬 |
| `cilium/pkg/hubble/relay/observer/observer.go` | `aggregateErrors()` - 에러 병합 |
| `cilium/pkg/hubble/relay/observer/observer.go` | `flowCollector.collect()` - 병렬 수집 조율 |
| `cilium/pkg/hubble/relay/queue/priority_queue.go` | `PriorityQueue`, `minHeap` - heap 기반 정렬 큐 |
