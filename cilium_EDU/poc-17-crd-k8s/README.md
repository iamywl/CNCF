# PoC-17: K8s Resource[T] 프레임워크 (CRD 리소스 동기화)

## 개요

Cilium의 `pkg/k8s/resource/` 패키지에 구현된 **Resource[T] 제네릭 프레임워크**를 시뮬레이션한다.
Kubernetes CRD를 타입 안전하게 감시(Watch)하고, 이벤트 스트림으로 변환하며,
인덱싱된 로컬 캐시(Store)를 유지하는 전체 파이프라인을 Go 표준 라이브러리만으로 재현한다.

---

## 배경 지식

### CRD(Custom Resource Definition)란

Kubernetes는 Pod, Service 같은 내장 리소스 외에 **사용자 정의 리소스**를 선언할 수 있다.
CRD를 등록하면 API Server가 CRUD 엔드포인트를 자동 생성하고, kubectl, Informer, Watch 등 표준 인터페이스를 그대로 사용할 수 있다.

### Cilium이 사용하는 주요 CRD

| CRD | 역할 | 감시 주체 |
|-----|------|-----------|
| **CiliumNetworkPolicy** | L3/L4/L7 네트워크 정책 정의 | Agent |
| **CiliumNode** | 노드별 IPAM 할당, 터널 엔드포인트 | Operator, Agent |
| **CiliumEndpoint** | 엔드포인트(Pod) 네트워킹 상태 | Agent |
| **CiliumClusterwideNetworkPolicy** | 클러스터 범위 네트워크 정책 | Agent |

### Resource[T] 제네릭 패턴의 혁신성

Go 1.18 제네릭 도입 전에는 `interface{}` 캐스팅이 필수였다. Resource[T]는 **컴파일 타임 타입 안전성**을 보장하면서 모든 K8s 리소스에 동일 패턴을 적용한다. `Resource[*CiliumNetworkPolicy]`, `Resource[*CiliumNode]` 등이 런타임 캐스팅 없이 동작한다.

### Lazy Start Informer와 Event[T] 스트림

- **Lazy Start**: `Events()` 또는 `Store()` 호출 시에만 Informer 시작. 미사용 CRD의 Watch 연결을 방지한다.
- **Event[T] 스트림**: 구독자가 채널에서 이벤트를 수신하고 반드시 `Done(err)` 콜백을 호출해야 한다. 미호출 시 `runtime.SetFinalizer`로 panic이 발생하여 이벤트 누수를 원천 차단한다.

---

## 시뮬레이션하는 개념

| 컴포넌트 | 실제 Cilium 경로 | PoC 구현 |
|----------|------------------|----------|
| Resource[T] | `pkg/k8s/resource/resource.go` | 제네릭 리소스 + lazy start + subscriber 관리 |
| Event[T] | `pkg/k8s/resource/event.go` | Sync/Upsert/Delete + Done(err) 콜백 |
| Store[T] | `pkg/k8s/resource/store.go` | List, GetByKey, ByIndex, IterKeys + 인덱서 |
| subscriber | `pkg/k8s/resource/resource.go` | 독립 workqueue, processLoop, lastKnown 추적 |
| ErrorHandler | `pkg/k8s/resource/error.go` | AlwaysRetry, ErrorActionStop, ErrorActionIgnore |
| Key | `pkg/k8s/resource/key.go` | namespace/name 형식의 리소스 식별자 |

---

## 아키텍처 다이어그램

```
  K8s API Server (시뮬레이션)
  ┌──────────────────────────────┐
  │  CiliumNetworkPolicy CRD     │
  │  ┌─────┐ ┌─────┐ ┌─────┐    │
  │  │CNP-1│ │CNP-2│ │CNP-3│    │
  │  └──┬──┘ └──┬──┘ └──┬──┘    │
  └─────┼───────┼───────┼───────┘
        │ List/Watch (Delta)
        ▼
  ┌──────────────────────────────┐
  │  Resource[*CiliumNP]         │       Events(ctx) 호출
  │  (lazy: Events 호출 시 시작)  │         └─ subscriber 생성
  │                              │              ├─ 기존 키 replay
  │  ┌────────────────────┐      │              ├─ Upsert x N
  │  │ Store[*CiliumNP]   │      │              ├─ Sync (완료)
  │  │ ├ items (key→obj)  │      │              └─ 이후 증분
  │  │ ├ idx:namespace    │      │
  │  │ └ idx:tier         │      │       processLoop:
  │  └────────┬───────────┘      │         keyWorkItem → GetByKey
  │           │                  │           ├ 존재 → Upsert
  │  ┌────────▼───────────┐      │           └ 미존재 → lastKnown
  │  │ subscriber         │      │               ├ 있음 → Delete
  │  │ ├ workqueue        │      │               └ 없음 → skip
  │  │ ├ lastKnown map    │      │
  │  │ └ outCh ──────────────→ Event[T] → Done(nil|err)
  │  └────────────────────┘      │
  └──────────────────────────────┘
```

---

## 코드 해설

### 1. `Store[T]` - 인덱싱 지원 타입 캐시

`items`(key-obj 맵), `indexers`(인덱서 함수), `indices`(인덱스 역참조)로 구성된다. `AddIndexer`로 커스텀 인덱서를 등록하면 `upsert`/`delete` 시 인덱스가 자동 갱신된다. `ByIndex("namespace", "default")`처럼 O(1)에 가까운 속도로 조회 가능하다.

### 2. `Event[T]` - Done 콜백 패턴

`Kind`(Sync/Upsert/Delete), `Key`, `Object`, `Done` 콜백으로 구성된다. 모든 소비자는 처리 후 반드시 `Done`을 호출해야 하며, `Done(nil)`은 성공, `Done(err)`은 ErrorHandler에 따라 재큐/중단/무시를 결정한다. backpressure와 에러 복구를 동시에 해결하는 패턴이다.

### 3. `Resource[T].Events()` - Lazy Start 구독

호출 시 subscriber를 생성하고, 기존 Store의 키를 replay한 뒤 Sync 이벤트를 보낸다. 이후 API Server 변경이 발생하면 증분 이벤트를 스트리밍한다. 실제 Cilium에서는 이 호출이 `markNeeded()`를 트리거하여 Informer를 시작한다.

### 4. `subscriber[T].processLoop()` - 독립 큐 처리

각 subscriber는 자체 workqueue를 가지고 독립적으로 이벤트를 처리한다. `keyWorkItem`이 들어오면 Store에서 최신 객체를 조회하여 Upsert/Delete를 판별한다. `lastKnown` 맵으로 삭제된 객체의 마지막 상태를 추적하여 Delete 이벤트에 포함시킨다.

### 5. `ErrorHandler` - 실패 복구 전략

`ErrorActionRetry`(재큐), `ErrorActionStop`(구독 종료), `ErrorActionIgnore`(무시) 세 가지 전략을 제공한다. 기본값은 `AlwaysRetry`이며, `numRequeues` 기반으로 점진적 백오프를 구현할 수 있다.

---

## 실행 방법 및 예상 출력

```bash
cd cilium_EDU/poc-17-crd-k8s
go run main.go
```

주요 출력 흐름:
- **[1]** Store 생성, namespace/tier 인덱서 등록
- **[2]** 초기 CiliumNetworkPolicy 3개 로드 (allow-frontend, allow-backend, metrics-access)
- **[3]** `Events()` 구독 → Upsert 3개 replay + Sync 이벤트 수신
- **[4]** 증분 변경: allow-cache 추가(rv=4), allow-frontend 업데이트(rv=5), metrics-access 삭제
- **[5]** Store 조회: GetByKey, ByIndex(namespace), ByIndex(tier)
- **[6]** ErrorHandler 시뮬레이션: 3회 재시도 후 Ignore 전환
- **[7]** 최종 Store 상태: 3개 정책 (allow-backend, allow-cache, allow-frontend)

(Upsert replay 순서는 Go map 순회에 따라 달라질 수 있음)

---

## 핵심 포인트

1. **Lazy Start**: `Events()`/`Store()` 호출한 리소스만 Informer가 동작한다. 수십 개 CRD를 선언해도 실제 사용하는 것만 비용이 발생한다.

2. **Done 콜백 필수 호출**: `runtime.SetFinalizer`를 통해 Done 미호출 Event가 GC될 때 panic이 발생하여, 개발 단계에서 이벤트 누수 버그를 즉시 발견할 수 있다.

3. **subscriber 독립 큐**: 느린 소비자가 다른 소비자를 블로킹하지 않는다. Agent의 여러 서브시스템이 동일 CRD를 독립적 속도로 처리 가능하다.

4. **lastKnown 추적**: Delete 이벤트에 삭제된 객체의 마지막 상태를 포함시켜 정리(cleanup) 로직 구현이 가능하다.

5. **인덱서 기반 조회**: namespace, label 등 다양한 기준으로 O(1) 조회가 가능하다. 수천 개의 CiliumNetworkPolicy 중 특정 조건만 빠르게 필터링한다.

---

## 실제 Cilium과의 차이점

| 항목 | 실제 Cilium | 이 PoC |
|------|------------|--------|
| Informer | `client-go` SharedIndexInformer + Watch | 메모리 내 직접 이벤트 주입 |
| workqueue | `TypedRateLimitingInterface` (지수 백오프) | 단순 슬라이스 큐 |
| Done 미호출 감지 | `runtime.SetFinalizer`로 GC 시 panic | Done 채널 대기만 구현 |
| 동시성 | RateLimiter + 병렬 processLoop | 단일 goroutine 순차 처리 |
| Store | `cache.Indexer` 래핑 | `sync.RWMutex` + map 직접 구현 |
| CRD 등록 | API Server에 CRD YAML + webhook | 구조체 직접 정의 |
| 리소스 버전 | etcd 기반 ResourceVersion | 수동 설정 정수 |
| 다중 리소스 | CiliumNode, CiliumEndpoint 등 수십 종 | CiliumNetworkPolicy 1종만 |

---

## 관련 문서

- [17-crd-k8s.md](../17-crd-k8s.md) - Cilium K8s CRD 심화 문서
