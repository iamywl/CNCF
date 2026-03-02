# PoC: Hubble Relay의 Priority Queue 병합

> **관련 문서**: [02-ARCHITECTURE.md](../02-ARCHITECTURE.md) - Hubble Relay, [04-SEQUENCE-DIAGRAMS.md](../04-SEQUENCE-DIAGRAMS.md) - 멀티 노드 Relay 흐름

## 이 PoC가 보여주는 것

Hubble Relay는 멀티 노드 클러스터에서 각 노드의 Flow를 **타임스탬프 기준으로 정렬**하여 하나의 통합 스트림을 만듭니다.

```
Node-1: ──Flow(t=100)──Flow(t=300)──Flow(t=500)──>
Node-2: ──Flow(t=150)──Flow(t=250)──Flow(t=450)──>     Priority
Node-3: ──Flow(t=200)──Flow(t=350)──Flow(t=400)──>      Queue
                                                           ↓
Client: ──Flow(100)──Flow(150)──Flow(200)──Flow(250)──Flow(300)──...
         (시간순 정렬된 통합 스트림)
```

## 실행 방법

```bash
cd EDU/poc-relay-merge
go run main.go
```

## 핵심 메커니즘: Min-Heap

```go
// Go 표준 라이브러리의 container/heap 사용
type FlowHeap []Flow

func (h FlowHeap) Less(i, j int) bool {
    return h[i].Timestamp.Before(h[j].Timestamp)
}
```

- `Push`: 노드에서 Flow가 도착하면 heap에 삽입 → O(log n)
- `Pop`: 가장 오래된(timestamp가 작은) Flow를 꺼냄 → O(log n)
- 버퍼링: 최소 N개가 쌓인 후 꺼내기 시작하여 정렬 정확도 보장

## 왜 Priority Queue인가?

각 노드의 Flow는 네트워크 지연으로 도착 순서가 뒤섞일 수 있습니다:
- Node-1의 Flow(t=300)이 Node-2의 Flow(t=250)보다 먼저 도착 가능
- 단순히 도착 순서로 전달하면 시간 역전 발생
- Min-Heap으로 버퍼링하면 약간의 지연과 교환하여 정렬 보장
