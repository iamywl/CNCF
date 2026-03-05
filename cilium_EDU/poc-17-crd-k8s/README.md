# PoC-17: K8s Resource[T] 프레임워크 (CRD 리소스 동기화)

## 개요

Cilium의 `pkg/k8s/resource/` 패키지에 구현된 Resource[T] 제네릭 프레임워크를 시뮬레이션한다.
타입 안전한 K8s 리소스 접근, Event[T] 기반 이벤트 처리, Store[T] 인덱서,
subscriber 독립 큐, Done 콜백 패턴을 재현한다.

## 시뮬레이션 대상

| 컴포넌트 | 실제 Cilium 경로 | PoC 구현 |
|----------|------------------|----------|
| Resource[T] | `pkg/k8s/resource/resource.go` | 제네릭 리소스 + lazy start + subscriber 관리 |
| Event[T] | `pkg/k8s/resource/event.go` | Sync/Upsert/Delete + Done(err) 콜백 |
| Store[T] | `pkg/k8s/resource/store.go` | List, GetByKey, ByIndex, IterKeys |
| subscriber | `pkg/k8s/resource/resource.go` | 독립 workqueue, processLoop, lastKnown |
| ErrorHandler | `pkg/k8s/resource/error.go` | AlwaysRetry, ErrorActionStop, ErrorActionIgnore |

## 핵심 개념

### Resource[T] - Lazy Start
- `Events()` 또는 `Store()` 호출 시에만 Informer를 시작 (`markNeeded`)
- 미사용 리소스의 API Server 부하를 방지
- 실제: `resource.Events(ctx)` 호출 → `r.markNeeded()` → Informer Start

### Event[T] - Done 콜백 필수 호출
- `Done(nil)`: 성공 → workqueue에서 Forget (재시도 카운터 리셋)
- `Done(err)`: 실패 → ErrorHandler에 따라 Retry/Stop/Ignore
- **Done 미호출 시 `runtime.SetFinalizer`를 통해 panic 발생** (leak 방지)

### Store[T] - 타입 안전한 캐시
- `cache.Indexer`를 래핑하여 제네릭 타입 접근 제공
- `ByIndex(indexName, value)`: 커스텀 인덱서로 효율적 조회
- `IterKeys()`: 키 반복자로 모든 리소스 키 순회

### subscriber - 독립 큐
- 각 구독자는 자체 `workqueue.TypedRateLimitingInterface`를 가짐
- `keyWorkItem`: 특정 키의 리소스 변경 처리
- `syncWorkItem`: 초기 동기화 완료 표시
- `lastKnownObjects`: Delete 이벤트에 마지막 상태를 포함하기 위해 추적

## 실행

```bash
go run main.go
```

## 이벤트 처리 흐름

```
Events(ctx) 호출
  └─ subscriber 생성 (독립 workqueue)
       ├── 기존 키를 큐에 넣음 (store.IterKeys)
       ├── Upsert, Upsert, ... (초기 replay)
       ├── Sync (동기화 완료)
       └── 이후 증분: Upsert, Delete, ...

processLoop:
  workqueue.Get()
  ├── syncWorkItem → Event{Kind: Sync}
  └── keyWorkItem  → store.GetByKey(key)
       ├── 존재 → Event{Kind: Upsert, Object: obj}
       └── 미존재 → lastKnown 확인
            ├── 본 적 있음 → Event{Kind: Delete, Object: lastObj}
            └── 본 적 없음 → 무시 (skip)
```

## 관련 문서

- [17-crd-k8s.md](../17-crd-k8s.md) - Cilium K8s CRD 심화 문서
