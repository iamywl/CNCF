# PoC-11: 3계층 WorkQueue

## 개요

Kubernetes client-go의 3계층 WorkQueue를 시뮬레이션한다.

WorkQueue는 Kubernetes 컨트롤러의 핵심 인프라로, Informer가 감지한 변경 이벤트를 처리하는 큐이다. 기본 큐(Basic) 위에 지연 큐(Delaying), 속도 제한 큐(RateLimiting)가 레이어로 쌓인 구조이다.

## 실제 코드 참조

| 파일 | 역할 |
|------|------|
| `staging/src/k8s.io/client-go/util/workqueue/queue.go` | BasicQueue (dirty/processing/FIFO) |
| `staging/src/k8s.io/client-go/util/workqueue/delaying_queue.go` | DelayingQueue (min-heap) |
| `staging/src/k8s.io/client-go/util/workqueue/rate_limiting_queue.go` | RateLimitingQueue |
| `staging/src/k8s.io/client-go/util/workqueue/default_rate_limiters.go` | ExponentialBackoff, TokenBucket 등 |

## 시뮬레이션하는 개념

### Layer 1: BasicQueue
- **dirty set**: 처리 대기 중인 아이템 (중복 방지)
- **processing set**: 현재 처리 중인 아이템
- **FIFO queue**: 순서 보장
- Done() 후 dirty에 있으면 자동 재추가

### Layer 2: DelayingQueue
- AddAfter(item, duration)으로 지연 추가
- min-heap(priority queue)으로 가장 이른 readyAt이 루트
- waitingLoop goroutine이 시간 도래 시 BasicQueue로 이동

### Layer 3: RateLimitingQueue
- ExponentialBackoff: baseDelay * 2^failures (아이템별)
- TokenBucket: 전역 속도 제한 (rate/s, burst)
- MaxOfRateLimiter: 여러 limiter 중 최대 대기 시간 선택

### 컨트롤러 재시도 패턴
```go
item := queue.Get()
err := processItem(item)
if err != nil {
    queue.Done(item)           // 먼저 Done
    queue.AddRateLimited(item) // 그 다음 재추가
} else {
    queue.Forget(item)         // 성공 시 이력 초기화
    queue.Done(item)
}
```

## 실행

```bash
go run main.go
```
