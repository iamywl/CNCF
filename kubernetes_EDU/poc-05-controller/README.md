# PoC-05: 컨트롤러 패턴 (Informer + WorkQueue + Reconcile)

## 개요

쿠버네티스 컨트롤러의 핵심 메커니즘인 **Informer + WorkQueue + Reconcile 루프**를 구현한다.
ReplicaSet 컨트롤러를 예시로 원하는 파드 수를 유지하는 자가 치유(self-healing) 동작을 시뮬레이션한다.

## 구현 내용

| 컴포넌트 | 역할 | 실제 소스 위치 |
|---------|------|-------------|
| FakeAPIServer | LIST + WATCH 지원 | `apiserver/pkg/registry/` |
| DeltaFIFO | 이벤트 중복 제거 + FIFO 순서 보장 | `client-go/tools/cache/delta_fifo.go` |
| Informer | LIST → WATCH → 로컬 캐시 유지 | `client-go/tools/cache/reflector.go` |
| WorkQueue | 중복 방지 + 지수 백오프 재시도 | `client-go/util/workqueue/rate_limiting_queue.go` |
| RSController | ReplicaSet Reconcile 루프 | `pkg/controller/replicaset/replica_set.go` |

## 데이터 흐름

```
API Server ──WATCH──→ Informer ──→ DeltaFIFO ──→ 로컬캐시 + 핸들러
                                                       │
                                                  WorkQueue (중복 제거 + 재시도)
                                                       │
API Server ←──CRUD──── Reconciler ←──────── Worker Loop
```

## 시뮬레이션 시나리오

1. ReplicaSet(replicas=3) 생성, 기존 파드 1개 존재
2. 컨트롤러 시작 → LIST로 파드 발견 → Reconcile → 부족분 2개 생성
3. 외부에서 파드 삭제 (장애 시뮬레이션) → WATCH 이벤트 → Reconcile → 1개 재생성
4. 최종 상태: 항상 3개 유지

## 실행

```bash
go run main.go
```
