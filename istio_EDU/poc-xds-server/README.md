# PoC: xDS Discovery Service 시뮬레이션

## 개요

Istio Pilot의 xDS Discovery Service 핵심 알고리즘을 Go 표준 라이브러리만으로 시뮬레이션한다.

## 시뮬레이션 대상

| 구성 요소 | 실제 소스 경로 | 시뮬레이션 내용 |
|-----------|---------------|----------------|
| DiscoveryServer | `pilot/pkg/xds/discovery.go` | ConfigUpdate, debounce, Push, sendPushes |
| PushQueue | `pilot/pkg/xds/pushqueue.go` | Enqueue/Dequeue/MarkDone, 병합, 재큐잉 |
| Connection | `pilot/pkg/xds/ads.go` | 프록시 연결, PushOrder 순서 push |
| PushRequest | `pilot/pkg/model/push_request.go` | 이벤트 병합 (Merge) |

## 핵심 알고리즘

### 1. 디바운싱 (debounce)

```
ConfigUpdate → pushChannel → debounce 고루틴 → Push()
                              ↑
                100ms quiet 또는 10s max 대기
                이벤트 병합 (Merge)
```

- `DebounceAfter` (100ms): 마지막 이벤트 이후 이 시간동안 새 이벤트가 없으면 push
- `DebounceMax` (10s): 이벤트가 계속 들어와도 이 시간이 지나면 강제 push
- `free` 플래그: 이전 push가 완료되기 전에는 다음 push를 시작하지 않음

### 2. PushQueue 상태 머신

```
           Enqueue()          Dequeue()         MarkDone()
[없음] ──────────→ [pending] ──────────→ [processing] ──────────→ [없음]
                     ↑                        │
                     │   Enqueue() 시         │ processing[con] != nil이면
                     │   병합(CopyMerge)       │ 자동 재큐잉
                     └────────────────────────┘
```

### 3. Push 순서 (PushOrder)

```
CDS → EDS → LDS → RDS → (기타)
```

Envoy가 참조 무결성을 유지하기 위해 Cluster → Endpoint → Listener → Route 순서로 설정을 받아야 한다.

## 실행 방법

```bash
go run main.go
```

## 시나리오

1. 3개 프록시 연결
2. 단일 설정 변경 → 디바운싱 → 모든 프록시에 push
3. 빠른 연속 변경 3개 → 디바운싱 병합 → 1번의 push
4. PushQueue 병합/재큐잉 동작 검증
5. Push 순서 (CDS→EDS→LDS→RDS) 검증
6. 프록시 연결 해제 후 push (해제된 프록시는 제외)
