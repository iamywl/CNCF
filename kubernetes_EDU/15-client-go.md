# 15. client-go 심화 (client-go Deep-Dive)

## 목차

1. [개요](#1-개요)
2. [client-go 아키텍처 전체 그림](#2-client-go-아키텍처-전체-그림)
3. [SharedInformer](#3-sharedinformer)
4. [Reflector: ListAndWatch](#4-reflector-listandwatch)
5. [DeltaFIFO: 변경 큐](#5-deltafifo-변경-큐)
6. [Store/Indexer: 로컬 캐시](#6-storeindexer-로컬-캐시)
7. [Controller: processLoop](#7-controller-processloop)
8. [processDeltas: 이벤트 디스패치](#8-processdeltas-이벤트-디스패치)
9. [WorkQueue 타입 계층](#9-workqueue-타입-계층)
10. [기본 WorkQueue (Typed)](#10-기본-workqueue-typed)
11. [DelayingQueue: 지연 큐](#11-delayingqueue-지연-큐)
12. [RateLimitingQueue: 속도 제한 큐](#12-ratelimitingqueue-속도-제한-큐)
13. [Rate Limiter 종류](#13-rate-limiter-종류)
14. [Informer에서 Controller까지: 전체 파이프라인](#14-informer에서-controller까지-전체-파이프라인)
15. [설계 원칙: Why](#15-설계-원칙-why)
16. [정리](#16-정리)

---

## 1. 개요

client-go는 Kubernetes API Server와 통신하기 위한 공식 Go 클라이언트 라이브러리다.
단순한 HTTP 클라이언트를 넘어서, **Informer 패턴**이라 불리는
효율적인 캐싱 및 이벤트 처리 메커니즘을 제공한다.

Kubernetes의 거의 모든 컨트롤러(controller-manager, scheduler, operator 등)가
client-go의 Informer와 WorkQueue를 기반으로 구현되어 있다.

**핵심 소스 경로:**

| 구성요소 | 소스 경로 |
|---------|----------|
| SharedInformer | `staging/src/k8s.io/client-go/tools/cache/shared_informer.go` |
| Reflector | `staging/src/k8s.io/client-go/tools/cache/reflector.go` |
| DeltaFIFO | `staging/src/k8s.io/client-go/tools/cache/delta_fifo.go` |
| Store/Indexer | `staging/src/k8s.io/client-go/tools/cache/store.go` |
| Controller | `staging/src/k8s.io/client-go/tools/cache/controller.go` |
| WorkQueue | `staging/src/k8s.io/client-go/util/workqueue/queue.go` |
| DelayingQueue | `staging/src/k8s.io/client-go/util/workqueue/delaying_queue.go` |
| RateLimitingQueue | `staging/src/k8s.io/client-go/util/workqueue/rate_limiting_queue.go` |
| Rate Limiters | `staging/src/k8s.io/client-go/util/workqueue/default_rate_limiters.go` |

---

## 2. client-go 아키텍처 전체 그림

### 2.1 데이터 흐름 다이어그램

```
                      API Server (etcd)
                           |
                    [List + Watch]
                           |
                           v
+----------------------------------------------------------+
|                     Reflector                             |
|  ListAndWatch() → List 전체 목록 → Watch 변경 스트림       |
+----------------------------------------------------------+
                           |
                     Add/Update/Delete
                           |
                           v
+----------------------------------------------------------+
|                     DeltaFIFO                             |
|  items: map[key]Deltas    queue: []key                    |
|  오브젝트별 변경 이력(Delta) 축적                           |
+----------------------------------------------------------+
                           |
                       Pop()
                           |
                           v
+----------------------------------------------------------+
|                    Controller                             |
|  processLoop() → Pop → processDeltas                     |
+----------------------------------------------------------+
                      /              \
                     /                \
                    v                  v
+-------------------+    +-------------------+
|   Store/Indexer   |    | ResourceEventHandler |
| (로컬 캐시)       |    | OnAdd/OnUpdate/OnDelete |
| thread-safe       |    +-------------------+
| namespace/name 키  |              |
+-------------------+              v
                          +-------------------+
                          |    WorkQueue       |
                          | (rate-limited)     |
                          +-------------------+
                                   |
                              Get()
                                   |
                                   v
                          +-------------------+
                          |  Custom Controller |
                          |  Reconcile 로직    |
                          +-------------------+
```

### 2.2 핵심 구성요소 역할

| 구성요소 | 역할 |
|---------|------|
| **Reflector** | API Server에서 List+Watch로 데이터를 가져와 DeltaFIFO에 넣음 |
| **DeltaFIFO** | 오브젝트별 변경 이력을 FIFO 큐로 관리 |
| **Controller** | DeltaFIFO에서 Pop하여 Store 업데이트 + 이벤트 핸들러 호출 |
| **Store/Indexer** | thread-safe 로컬 캐시, 인덱스로 빠른 조회 |
| **SharedInformer** | 위 구성요소를 조합하고 여러 핸들러 간 공유 |
| **WorkQueue** | 컨트롤러의 재처리 큐, 속도 제한 및 중복 제거 |

---

## 3. SharedInformer

### 3.1 SharedInformer 인터페이스

```go
// staging/src/k8s.io/client-go/tools/cache/shared_informer.go (143행)
type SharedInformer interface {
    AddEventHandler(handler ResourceEventHandler) (ResourceEventHandlerRegistration, error)
    AddEventHandlerWithResyncPeriod(handler ResourceEventHandler, resyncPeriod time.Duration) (ResourceEventHandlerRegistration, error)
    AddEventHandlerWithOptions(handler ResourceEventHandler, options HandlerOptions) (ResourceEventHandlerRegistration, error)
    RemoveEventHandler(handle ResourceEventHandlerRegistration) error
    GetStore() Store
    GetController() Controller
    Run(stopCh <-chan struct{})
    RunWithContext(ctx context.Context)
    HasSynced() bool
    HasSyncedChecker() DoneChecker
    LastSyncResourceVersion() string
    SetWatchErrorHandler(handler WatchErrorHandler) error
    SetTransform(handler TransformFunc) error
}
```

### 3.2 SharedIndexInformer

```go
// shared_informer.go (283-287행)
type SharedIndexInformer interface {
    SharedInformer
    AddIndexers(indexers Indexers) error
    GetIndexer() Indexer
}
```

`SharedIndexInformer`는 `SharedInformer`에 인덱싱 기능을 추가한다.
네임스페이스별 조회 등 효율적인 검색이 가능하다.

### 3.3 sharedIndexInformer 내부 구조

```go
// shared_informer.go (318-339행)
func NewSharedIndexInformerWithOptions(lw ListerWatcher, exampleObject runtime.Object,
    options SharedIndexInformerOptions) SharedIndexInformer {

    return &sharedIndexInformer{
        indexer:                         NewIndexer(DeletionHandlingMetaNamespaceKeyFunc, options.Indexers),
        processor:                       processor,
        listerWatcher:                   lw,
        objectType:                      exampleObject,
        resyncCheckPeriod:               options.ResyncPeriod,
        defaultEventHandlerResyncPeriod: options.ResyncPeriod,
        clock:                           realClock,
        cacheMutationDetector:           NewCacheMutationDetector(fmt.Sprintf("%T", exampleObject)),
    }
}
```

| 필드 | 역할 |
|------|------|
| `indexer` | 로컬 캐시 (Indexer 인터페이스) |
| `processor` | 이벤트 핸들러 디스패처 (sharedProcessor) |
| `listerWatcher` | API Server와의 List/Watch 인터페이스 |
| `objectType` | 처리할 오브젝트 타입 예시 |
| `resyncCheckPeriod` | 리싱크 주기 |
| `cacheMutationDetector` | 캐시 오브젝트 변형 감지 (디버그용) |

### 3.4 Shared의 의미

"Shared"는 **하나의 Informer를 여러 이벤트 핸들러가 공유**한다는 의미다.

```
                SharedInformer
                     |
             +-------+-------+
             |       |       |
         Handler1 Handler2 Handler3
         (controller) (metric) (audit)
```

API Server로의 Watch 연결은 하나뿐이지만,
여러 핸들러가 동일한 이벤트를 수신한다.
이를 통해 API Server 부하를 대폭 줄인다.

### 3.5 HasSynced와 WaitForCacheSync

```go
// shared_informer.go (415-431행)
func WaitForCacheSync(stopCh <-chan struct{}, cacheSyncs ...InformerSynced) bool {
    err := wait.PollImmediateUntil(syncedPollPeriod,
        func() (bool, error) {
            for _, syncFunc := range cacheSyncs {
                if !syncFunc() {
                    return false, nil
                }
            }
            return true, nil
        },
        stopCh)
    return err == nil
}
```

컨트롤러는 작업 시작 전에 반드시 `WaitForCacheSync`를 호출하여
Informer 캐시가 최초 List로 채워질 때까지 기다려야 한다.
그렇지 않으면 빈 캐시를 기반으로 잘못된 결정을 내릴 수 있다.

---

## 4. Reflector: ListAndWatch

### 4.1 Reflector 구조

```go
// staging/src/k8s.io/client-go/tools/cache/reflector.go (296-371행)
func NewReflectorWithOptions(lw ListerWatcher, expectedType interface{},
    store ReflectorStore, options ReflectorOptions) *Reflector {

    r := &Reflector{
        name:            options.Name,
        resyncPeriod:    options.ResyncPeriod,
        minWatchTimeout: minWatchTimeout,     // 기본 5분
        maxWatchTimeout: maxWatchTimeout,     // 기본 10분
        listerWatcher:   ToListerWatcherWithContext(lw),
        store:           store,               // DeltaFIFO
        delayHandler:    backoff.DelayWithReset(clock, defaultBackoffReset),
        expectedType:    reflect.TypeOf(expectedType),
    }
    return r
}
```

### 4.2 List and Watch 패턴

Reflector의 핵심은 "List then Watch" 패턴이다:

```
Reflector.ListAndWatch()
  |
  [1단계: List] ← 전체 오브젝트 목록 가져오기
  |   GET /api/v1/pods → 모든 Pod 반환
  |   → store.Replace(list) → DeltaFIFO에 Replaced 이벤트
  |   → resourceVersion 기록 (예: rv=1000)
  |
  v
  [2단계: Watch] ← 이후 변경만 스트리밍
      GET /api/v1/pods?watch=true&resourceVersion=1000
      → ADDED/MODIFIED/DELETED 이벤트 수신
      → store.Add/Update/Delete → DeltaFIFO에 Delta 추가
      |
      +-- 연결 끊김?
      |     → 재연결 + 재시도 (backoff)
      |     → Watch에서 410 Gone → 다시 List부터 시작
      |
      +-- 타임아웃?
            → 재연결 (서버가 watch 타임아웃을 설정)
```

### 4.3 왜 List then Watch인가?

```
전체 List만 반복하면?
  → 매번 모든 데이터를 전송, 비효율적
  → API Server/etcd 부하 큼

Watch만 사용하면?
  → 초기 상태를 모름
  → 이벤트 유실 시 상태 불일치

List then Watch:
  → List로 초기 상태 확보
  → Watch로 변경만 추적
  → 유실 시 다시 List (resourceVersion 기반)
```

### 4.4 Watch 타임아웃

```go
// reflector.go (55-57행)
// watch 타임아웃은 [minWatchTimeout, 2*minWatchTimeout] 범위에서 랜덤
var (
    minWatchTimeout = 5 * time.Minute
)
```

Watch 타임아웃을 랜덤화하는 이유:
- 모든 Informer가 동시에 재연결하는 "thundering herd" 방지
- API Server 부하 분산

### 4.5 Backoff 설정

```go
// reflector.go (317-326행)
backoff := &wait.Backoff{
    Duration: defaultBackoffInit,   // 초기 대기
    Cap:      defaultBackoffMax,    // 최대 대기
    Steps:    int(math.Ceil(float64(defaultBackoffMax) / float64(defaultBackoffInit))),
    Factor:   defaultBackoffFactor, // 지수 증가 배수
    Jitter:   defaultBackoffJitter, // 지터 (랜덤 요소)
}
```

연결 실패 시 지수 백오프(exponential backoff)로 재시도한다.
이를 통해 API Server 장애 시 재시도 폭주를 방지한다.

---

## 5. DeltaFIFO: 변경 큐

### 5.1 핵심 구조

```go
// staging/src/k8s.io/client-go/tools/cache/delta_fifo.go (108-158행)
type DeltaFIFO struct {
    lock sync.RWMutex
    cond sync.Cond

    // items: 오브젝트 키 → 변경 이력 (Deltas)
    items map[string]Deltas

    // queue: FIFO 순서 유지, 중복 없음
    queue []string

    // 초기 동기화 완료 추적
    populated             bool
    initialPopulationCount int
    synced                chan struct{}

    // 키 생성 함수 (보통 namespace/name)
    keyFunc KeyFunc

    // 알려진 오브젝트 목록 (Store의 키 목록)
    knownObjects KeyListerGetter
}
```

### 5.2 DeltaFIFO의 이름 분해

```
Delta  = 변경 사항 (Added/Updated/Deleted/Replaced/Sync)
FIFO   = First-In-First-Out (선입선출 큐)

Delta + FIFO = 오브젝트별 변경 이력을 FIFO 순서로 관리하는 큐
```

### 5.3 Delta 타입

```go
// delta_fifo.go (178-208행)
type DeltaType string

const (
    Added     DeltaType = "Added"      // 새 오브젝트
    Updated   DeltaType = "Updated"    // 기존 오브젝트 수정
    Deleted   DeltaType = "Deleted"    // 오브젝트 삭제
    Replaced  DeltaType = "Replaced"   // Re-list 시 교체
    Sync      DeltaType = "Sync"       // 주기적 리싱크
)

// Delta: 하나의 변경 사항
type Delta struct {
    Type   DeltaType
    Object interface{}
}

// Deltas: 한 오브젝트에 대한 변경 이력 목록
type Deltas []Delta
```

### 5.4 items와 queue의 관계

```
items = {
    "default/pod-a": [Delta{Added, pod-a-v1}, Delta{Updated, pod-a-v2}],
    "default/pod-b": [Delta{Added, pod-b-v1}],
    "kube-system/pod-c": [Delta{Deleted, pod-c-v1}],
}

queue = ["default/pod-a", "default/pod-b", "kube-system/pod-c"]

불변 조건: queue에 있는 키 = items에 있는 키 (양쪽에 동시 존재)
```

### 5.5 queueActionLocked: 핵심 추가 로직

```go
// delta_fifo.go (491-541행)
func (f *DeltaFIFO) queueActionInternalLocked(
    actionType, internalActionType DeltaType, obj interface{}) error {

    id, err := f.KeyOf(obj)
    if err != nil { return KeyError{obj, err} }

    // TransformFunc 적용 (메모리 최적화용)
    if f.transformer != nil {
        _, isTombstone := obj.(DeletedFinalStateUnknown)
        if !isTombstone && internalActionType != Sync {
            obj, err = f.transformer(obj)
        }
    }

    oldDeltas := f.items[id]
    newDeltas := append(oldDeltas, Delta{actionType, obj})
    newDeltas = dedupDeltas(newDeltas)  // 중복 제거

    if len(newDeltas) > 0 {
        if _, exists := f.items[id]; !exists {
            f.queue = append(f.queue, id)   // 새 키면 queue에 추가
        }
        f.items[id] = newDeltas
        f.cond.Broadcast()                  // 대기 중인 Pop에 알림
    }
    return nil
}
```

#### 흐름 다이어그램

```
queueActionLocked(Updated, pod-a-v2)
  |
  +-- KeyOf(pod-a) → "default/pod-a"
  |
  +-- transformer 적용 (있으면)
  |
  +-- oldDeltas = items["default/pod-a"]
  |     = [Delta{Added, pod-a-v1}]
  |
  +-- newDeltas = append(oldDeltas, Delta{Updated, pod-a-v2})
  |     = [Delta{Added, pod-a-v1}, Delta{Updated, pod-a-v2}]
  |
  +-- dedupDeltas(newDeltas) → 중복 삭제 확인
  |
  +-- items["default/pod-a"] = newDeltas
  |     (키가 이미 존재하므로 queue에 중복 추가 안 함)
  |
  +-- cond.Broadcast() → Pop 대기 해제
```

### 5.6 중복 제거 (dedupDeltas)

```go
// delta_fifo.go (443-478행)
func dedupDeltas(deltas Deltas) Deltas {
    n := len(deltas)
    if n < 2 { return deltas }
    a := &deltas[n-1]
    b := &deltas[n-2]
    if out := isDup(a, b); out != nil {
        deltas[n-2] = *out
        return deltas[:n-1]
    }
    return deltas
}

func isDeletionDup(a, b *Delta) *Delta {
    if b.Type != Deleted || a.Type != Deleted { return nil }
    // 둘 다 Deleted이면, 더 많은 정보를 가진 쪽을 유지
    if _, ok := b.Object.(DeletedFinalStateUnknown); ok {
        return a   // a가 더 최신 정보
    }
    return b       // b가 실제 오브젝트
}
```

현재 구현에서는 **연속된 Deleted 이벤트만** 중복 제거한다.
다른 DeltaType에 대한 중복 제거는 향후 확장 가능하지만
현재는 불필요한 것으로 판단되어 구현하지 않았다.

### 5.7 Pop: 소비자 인터페이스

```go
// delta_fifo.go (562-608행)
func (f *DeltaFIFO) Pop(process PopProcessFunc) (interface{}, error) {
    f.lock.Lock()
    defer f.lock.Unlock()
    for {
        for len(f.queue) == 0 {
            if f.closed { return nil, ErrFIFOClosed }
            f.cond.Wait()   // 큐가 비어있으면 대기
        }

        isInInitialList := !f.hasSynced_locked()
        id := f.queue[0]
        f.queue = f.queue[1:]    // FIFO: 앞에서 꺼냄

        if f.initialPopulationCount > 0 {
            f.initialPopulationCount--
            f.checkSynced_locked()
        }

        item, ok := f.items[id]
        if !ok { continue }     // 있을 수 없지만 방어 코드
        delete(f.items, id)      // items에서 제거

        err := process(item, isInInitialList)   // 처리 함수 호출
        return item, err
    }
}
```

**핵심 특성:**
- `Pop`은 **lock 아래에서** process 함수를 호출한다
- process 함수에서 Store(knownObjects)를 업데이트하면
  DeltaFIFO의 knownObjects와 동기화가 보장된다
- 큐가 비어 있으면 `cond.Wait()`로 블로킹 대기

### 5.8 Replace: Re-List 처리

```go
// delta_fifo.go (619-699행)
func (f *DeltaFIFO) Replace(list []interface{}, _ string) error {
    f.lock.Lock()
    defer f.lock.Unlock()

    keys := make(sets.Set[string], len(list))

    action := Sync
    if f.emitDeltaTypeReplaced {
        action = Replaced
    }

    // 새 목록의 모든 항목에 Replaced/Sync 이벤트 추가
    for _, item := range list {
        key, err := f.KeyOf(item)
        keys.Insert(key)
        f.queueActionInternalLocked(action, Replaced, item)
    }

    // 삭제 감지: items에 있지만 새 목록에 없는 항목
    for k, oldItem := range f.items {
        if keys.Has(k) { continue }
        var deletedObj interface{}
        if n := oldItem.Newest(); n != nil {
            deletedObj = n.Object
        }
        f.queueActionLocked(Deleted, DeletedFinalStateUnknown{k, deletedObj})
    }

    // 삭제 감지: knownObjects에 있지만 새 목록에 없는 항목
    if f.knownObjects != nil {
        knownKeys := f.knownObjects.ListKeys()
        for _, k := range knownKeys {
            if keys.Has(k) { continue }
            if len(f.items[k]) > 0 { continue }
            deletedObj, exists, _ := f.knownObjects.GetByKey(k)
            f.queueActionLocked(Deleted, DeletedFinalStateUnknown{k, deletedObj})
        }
    }

    // 초기 동기화 추적
    if !f.populated {
        f.populated = true
        f.initialPopulationCount = keys.Len() + queuedDeletions
    }
    return nil
}
```

### 5.9 DeletedFinalStateUnknown

```go
// delta_fifo.go (797-800행)
type DeletedFinalStateUnknown struct {
    Key string
    Obj interface{}
}
```

Watch 연결 끊김으로 삭제 이벤트를 놓쳤을 때,
Re-list에서 감지된 삭제 항목에 사용된다.
"최종 상태를 알 수 없다"는 의미를 담고 있으며,
포함된 `Obj`는 **stale**(오래된) 상태일 수 있다.

---

## 6. Store/Indexer: 로컬 캐시

### 6.1 Store 인터페이스

```go
// staging/src/k8s.io/client-go/tools/cache/store.go (41-82행)
type Store interface {
    Add(obj interface{}) error
    Update(obj interface{}) error
    Delete(obj interface{}) error
    List() []interface{}
    ListKeys() []string
    Get(obj interface{}) (item interface{}, exists bool, err error)
    GetByKey(key string) (item interface{}, exists bool, err error)
    Replace([]interface{}, string) error
    Resync() error
    LastStoreSyncResourceVersion() string
    Bookmark(rv string)
}
```

### 6.2 cache 구현체

```go
// store.go (204-212행)
type cache struct {
    cacheStorage ThreadSafeStore      // 실제 저장소 (thread-safe)
    keyFunc      KeyFunc               // 키 생성 함수
    transformer  TransformFunc         // 변환 함수 (메모리 최적화)
}
```

### 6.3 KeyFunc: 키 생성

```go
// store.go (154-163행)
func MetaNamespaceKeyFunc(obj interface{}) (string, error) {
    if key, ok := obj.(ExplicitKey); ok {
        return string(key), nil
    }
    objName, err := ObjectToName(obj)
    if err != nil { return "", err }
    return objName.String(), nil
}
```

키 형식:
- 네임스페이스 리소스: `"namespace/name"` (예: `"default/my-pod"`)
- 클러스터 리소스: `"name"` (예: `"my-node"`)

### 6.4 cache의 CRUD 구현

```go
// store.go (251-290행)
func (c *cache) Add(obj interface{}) error {
    key, err := c.keyFunc(obj)
    if err != nil { return KeyError{obj, err} }
    if c.transformer != nil {
        obj, err = c.transformer(obj)
    }
    c.cacheStorage.Add(key, obj)
    return nil
}

func (c *cache) Update(obj interface{}) error {
    key, err := c.keyFunc(obj)
    if c.transformer != nil {
        obj, err = c.transformer(obj)
    }
    c.cacheStorage.Update(key, obj)
    return nil
}

func (c *cache) Delete(obj interface{}) error {
    key, err := c.keyFunc(obj)
    c.cacheStorage.DeleteWithObject(key, obj)
    return nil
}
```

### 6.5 NewStore와 NewIndexer

```go
// store.go (399-416행)
func NewStore(keyFunc KeyFunc, opts ...StoreOption) Store {
    c := &cache{
        cacheStorage: NewThreadSafeStore(Indexers{}, Indices{}),
        keyFunc:      keyFunc,
    }
    return c
}

func NewIndexer(keyFunc KeyFunc, indexers Indexers) Indexer {
    return &cache{
        cacheStorage: NewThreadSafeStore(indexers, Indices{}),
        keyFunc:      keyFunc,
    }
}
```

### 6.6 Indexer와 인덱스

Indexer는 Store에 인덱싱 기능을 추가한다:

```go
// store.go (319-339행)
func (c *cache) Index(indexName string, obj interface{}) ([]interface{}, error) {
    return c.cacheStorage.Index(indexName, obj)
}

func (c *cache) ByIndex(indexName, indexedValue string) ([]interface{}, error) {
    return c.cacheStorage.ByIndex(indexName, indexedValue)
}
```

자주 사용되는 인덱스:

```
Indexers{
    "namespace": MetaNamespaceIndexFunc,    // 네임스페이스별 조회
}

// 사용 예:
indexer.ByIndex("namespace", "default")
// → default 네임스페이스의 모든 오브젝트 반환
```

### 6.7 SplitMetaNamespaceKey

```go
// store.go (188-200행)
func SplitMetaNamespaceKey(key string) (namespace, name string, err error) {
    parts := strings.Split(key, "/")
    switch len(parts) {
    case 1:
        return "", parts[0], nil          // 클러스터 리소스
    case 2:
        return parts[0], parts[1], nil    // 네임스페이스 리소스
    }
    return "", "", fmt.Errorf("unexpected key format: %q", key)
}
```

WorkQueue에서 꺼낸 키를 namespace와 name으로 분리한다.

---

## 7. Controller: processLoop

### 7.1 Controller 인터페이스

```go
// staging/src/k8s.io/client-go/tools/cache/controller.go (124-153행)
type Controller interface {
    RunWithContext(ctx context.Context)
    Run(stopCh <-chan struct{})
    HasSynced() bool
    HasSyncedChecker() DoneChecker
    LastSyncResourceVersion() string
}
```

### 7.2 controller 구조체

```go
// controller.go (115-120행)
type controller struct {
    config         Config
    reflector      *Reflector
    reflectorMutex sync.RWMutex
    clock          clock.Clock
}
```

### 7.3 Config

```go
// controller.go (44-100행)
type Config struct {
    Queue                           // DeltaFIFO
    ListerWatcher                   // List+Watch 인터페이스
    Process       ProcessFunc       // Pop된 항목 처리 함수
    ProcessBatch  ProcessBatchFunc  // 배치 처리 함수
    ObjectType    runtime.Object    // 대상 오브젝트 타입
    FullResyncPeriod time.Duration  // 리싱크 주기
    ShouldResync  ShouldResyncFunc  // 리싱크 여부 결정
    MinWatchTimeout time.Duration   // 최소 Watch 타임아웃
}
```

### 7.4 RunWithContext

```go
// controller.go (170-209행)
func (c *controller) RunWithContext(ctx context.Context) {
    defer utilruntime.HandleCrashWithContext(ctx)

    // Context 취소 시 큐 닫기
    go func() {
        <-ctx.Done()
        c.config.Queue.Close()
    }()

    // Reflector 생성 및 시작
    r := NewReflectorWithOptions(
        c.config.ListerWatcher,
        c.config.ObjectType,
        c.config.Queue,     // DeltaFIFO
        ReflectorOptions{...},
    )
    r.ShouldResync = c.config.ShouldResync

    c.reflector = r

    var wg wait.Group
    wg.StartWithContext(ctx, r.RunWithContext)  // Reflector 실행 (goroutine)

    // processLoop 실행 (1초 간격으로 재시작)
    wait.UntilWithContext(ctx, c.processLoop, time.Second)
    wg.Wait()
}
```

#### RunWithContext 흐름

```
RunWithContext(ctx)
  |
  +-- goroutine: ctx.Done() → Queue.Close()
  |
  +-- Reflector 생성 (ListerWatcher → Queue)
  |
  +-- goroutine: Reflector.RunWithContext(ctx)
  |     List → Watch → Queue에 Delta 추가
  |
  +-- processLoop (1초 간격으로 반복)
        Queue에서 Pop → Process 함수 호출
```

### 7.5 processLoop

```go
// controller.go (236-261행)
func (c *controller) processLoop(ctx context.Context) {
    for {
        select {
        case <-ctx.Done():
            return
        default:
            _, err := c.config.Pop(PopProcessFunc(c.config.Process))
            if err != nil {
                if errors.Is(err, ErrFIFOClosed) {
                    return
                }
            }
        }
    }
}
```

`processLoop`는 무한 루프에서 DeltaFIFO의 `Pop`을 호출한다.
- `Pop`은 큐가 비어있으면 블로킹 대기
- 새 항목이 들어오면 `Process` 함수 호출
- 에러가 발생해도 계속 실행 (ErrFIFOClosed 제외)

---

## 8. processDeltas: 이벤트 디스패치

### 8.1 processDeltas 함수

```go
// controller.go (607-665행)
func processDeltas(logger klog.Logger,
    handler ResourceEventHandler,
    clientState Store,
    deltas Deltas,
    isInInitialList bool,
    keyFunc KeyFunc) error {

    // oldest to newest (오래된 것부터)
    for _, d := range deltas {
        obj := d.Object

        switch d.Type {
        case Sync, Replaced, Added, Updated:
            if old, exists, err := clientState.Get(obj); err == nil && exists {
                if err := clientState.Update(obj); err != nil { return err }
                handler.OnUpdate(old, obj)          // 이미 존재하면 Update
            } else {
                if err := clientState.Add(obj); err != nil { return err }
                handler.OnAdd(obj, isInInitialList) // 새로 추가면 Add
            }
        case Deleted:
            if err := clientState.Delete(obj); err != nil { return err }
            handler.OnDelete(obj)                   // 삭제
        }
    }
    return nil
}
```

### 8.2 두 가지 역할

processDeltas는 **두 가지 일**을 동시에 수행한다:

```
DeltaFIFO에서 Pop된 Deltas
  |
  +-- [역할 1] clientState (Store/Indexer) 업데이트
  |     로컬 캐시를 최신 상태로 유지
  |
  +-- [역할 2] ResourceEventHandler 호출
        OnAdd / OnUpdate / OnDelete
```

### 8.3 ResourceEventHandler

```go
// controller.go (279-283행)
type ResourceEventHandler interface {
    OnAdd(obj interface{}, isInInitialList bool)
    OnUpdate(oldObj, newObj interface{})
    OnDelete(obj interface{})
}
```

### 8.4 ResourceEventHandlerFuncs

```go
// controller.go (292-317행)
type ResourceEventHandlerFuncs struct {
    AddFunc    func(obj interface{})
    UpdateFunc func(oldObj, newObj interface{})
    DeleteFunc func(obj interface{})
}

func (r ResourceEventHandlerFuncs) OnAdd(obj interface{}, isInInitialList bool) {
    if r.AddFunc != nil {
        r.AddFunc(obj)
    }
}
```

함수형 어댑터로, 필요한 핸들러만 선택적으로 구현할 수 있다.

### 8.5 DeletionHandlingMetaNamespaceKeyFunc

```go
// controller.go (394-399행)
func DeletionHandlingMetaNamespaceKeyFunc(obj interface{}) (string, error) {
    if d, ok := obj.(DeletedFinalStateUnknown); ok {
        return d.Key, nil    // tombstone에서 직접 키 추출
    }
    return MetaNamespaceKeyFunc(obj)
}
```

`DeletedFinalStateUnknown` 오브젝트도 올바르게 키를 추출한다.
이 함수는 Store/Indexer의 keyFunc으로 사용된다.

---

## 9. WorkQueue 타입 계층

### 9.1 3가지 큐 타입

```
TypedInterface[T]               ← 기본 큐
  |
  +-- TypedDelayingInterface[T]  ← 지연 추가 기능
       |
       +-- TypedRateLimitingInterface[T]  ← 속도 제한 추가
```

### 9.2 인터페이스 계층

| 인터페이스 | 추가 메서드 | 용도 |
|-----------|-----------|------|
| `TypedInterface[T]` | `Add`, `Get`, `Done`, `ShutDown` | 기본 FIFO |
| `TypedDelayingInterface[T]` | `AddAfter(item, duration)` | 실패 후 재시도 |
| `TypedRateLimitingInterface[T]` | `AddRateLimited`, `Forget`, `NumRequeues` | 지수 백오프 |

### 9.3 왜 계층 구조인가?

```
기본 컨트롤러:
  → TypedInterface만으로 충분
  → 단순 FIFO 처리

재시도가 필요한 컨트롤러:
  → TypedDelayingInterface
  → 실패 시 일정 시간 후 재큐잉

대부분의 프로덕션 컨트롤러:
  → TypedRateLimitingInterface
  → 지수 백오프 + 전체 처리율 제한
```

---

## 10. 기본 WorkQueue (Typed)

### 10.1 TypedInterface

```go
// staging/src/k8s.io/client-go/util/workqueue/queue.go (30-38행)
type TypedInterface[T comparable] interface {
    Add(item T)
    Len() int
    Get() (item T, shutdown bool)
    Done(item T)
    ShutDown()
    ShutDownWithDrain()
    ShuttingDown() bool
}
```

### 10.2 Typed 구조체

```go
// queue.go (190-222행)
type Typed[t comparable] struct {
    queue      Queue[t]            // 실제 FIFO 큐 (슬라이스)
    dirty      sets.Set[t]         // 처리 대기 중인 항목
    processing sets.Set[t]         // 처리 중인 항목
    cond       *sync.Cond          // 동기화
    shuttingDown bool
    drain        bool
    metrics    queueMetrics[t]
    clock      clock.WithTicker
}
```

### 10.3 dirty / processing / queue 관계

```
                  dirty           processing         queue
                (처리 필요)      (처리 중)          (대기열)

Add("A")        {A}               {}              [A]
Add("B")        {A, B}            {}              [A, B]
Get() → "A"     {B}               {A}             [B]
Add("A")        {A, B}            {A}             [B]
                 ↑ A가 다시 dirty에 추가되지만
                   processing 중이므로 queue에는 추가 안 됨
Done("A")       {A, B}            {}              [B, A]
                 ↑ dirty에 있으므로 다시 queue에 추가
Get() → "B"     {A}               {B}             [A]
Done("B")       {A}               {}              [A]
                 ↑ dirty에 없으므로 queue에 추가 안 됨
```

### 10.4 Add: 중복 제거

```go
// queue.go (227-251행)
func (q *Typed[T]) Add(item T) {
    q.cond.L.Lock()
    defer q.cond.L.Unlock()
    if q.shuttingDown { return }

    if q.dirty.Has(item) {
        // 이미 dirty에 있음 (아직 처리 안 됨)
        if !q.processing.Has(item) {
            q.queue.Touch(item)   // 우선순위 조정 가능
        }
        return
    }

    q.metrics.add(item)
    q.dirty.Insert(item)

    if q.processing.Has(item) {
        return   // 처리 중이면 queue에 추가하지 않음 (Done 후에 재추가)
    }

    q.queue.Push(item)
    q.cond.Signal()   // 대기 중인 Get에 알림
}
```

**핵심 특성: 중복 제거**

같은 키가 여러 번 Add되어도 queue에 한 번만 존재한다.
이를 통해 "같은 오브젝트에 대해 빠르게 연속 변경이 발생해도
하나의 reconcile만 실행"하는 동작이 보장된다.

### 10.5 Get: 블로킹 대기

```go
// queue.go (265-284행)
func (q *Typed[T]) Get() (item T, shutdown bool) {
    q.cond.L.Lock()
    defer q.cond.L.Unlock()

    for q.queue.Len() == 0 && !q.shuttingDown {
        q.cond.Wait()        // 큐가 비어있으면 대기
    }
    if q.queue.Len() == 0 {
        return *new(T), true // 셧다운
    }

    item = q.queue.Pop()     // FIFO에서 꺼냄

    q.processing.Insert(item)
    q.dirty.Delete(item)     // dirty에서 제거

    return item, false
}
```

### 10.6 Done: 처리 완료

```go
// queue.go (289-302행)
func (q *Typed[T]) Done(item T) {
    q.cond.L.Lock()
    defer q.cond.L.Unlock()

    q.processing.Delete(item)

    if q.dirty.Has(item) {
        q.queue.Push(item)   // 처리 중에 다시 Add됐으면 재큐잉
        q.cond.Signal()
    } else if q.processing.Len() == 0 {
        q.cond.Signal()      // ShutDownWithDrain을 위한 시그널
    }
}
```

`Done`을 반드시 호출해야 한다. 그렇지 않으면:
- 같은 항목이 다시 큐에 들어갈 수 없음
- ShutDownWithDrain이 영원히 블로킹

---

## 11. DelayingQueue: 지연 큐

### 11.1 TypedDelayingInterface

```go
// staging/src/k8s.io/client-go/util/workqueue/delaying_queue.go (37-41행)
type TypedDelayingInterface[T comparable] interface {
    TypedInterface[T]
    AddAfter(item T, duration time.Duration)
}
```

### 11.2 delayingType 구조체

```go
// delaying_queue.go (162-181행)
type delayingType[T comparable] struct {
    TypedInterface[T]                    // 내장 기본 큐

    clock           clock.Clock
    stopCh          chan struct{}
    heartbeat       clock.Ticker         // 10초 간격 하트비트
    waitingForAddCh chan *waitFor[T]      // 지연 항목 전달 채널 (버퍼 1000)
    metrics         retryMetrics
}
```

### 11.3 waitFor: 대기 항목

```go
// delaying_queue.go (184-189행)
type waitFor[T any] struct {
    data    T
    readyAt time.Time   // 큐에 추가될 시간
    index   int          // 힙에서의 인덱스
}
```

### 11.4 waitForPriorityQueue: 최소 힙

```go
// delaying_queue.go (199-236행)
type waitForPriorityQueue[T any] []*waitFor[T]

func (pq waitForPriorityQueue[T]) Less(i, j int) bool {
    return pq[i].readyAt.Before(pq[j].readyAt)   // readyAt이 빠른 순
}
```

`container/heap`을 사용한 최소 힙(min-heap) 우선순위 큐.
readyAt이 가장 빠른 항목이 루트에 위치한다.

### 11.5 AddAfter

```go
// delaying_queue.go (249-268행)
func (q *delayingType[T]) AddAfter(item T, duration time.Duration) {
    if q.ShuttingDown() { return }

    q.metrics.retry()

    // 지연 없으면 즉시 추가
    if duration <= 0 {
        q.Add(item)
        return
    }

    select {
    case <-q.stopCh:
    case q.waitingForAddCh <- &waitFor[T]{
        data:    item,
        readyAt: q.clock.Now().Add(duration),
    }:
    }
}
```

### 11.6 waitingLoop: 핵심 루프

```go
// delaying_queue.go (276-352행)
func (q *delayingType[T]) waitingLoop(logger klog.Logger) {
    never := make(<-chan time.Time)
    var nextReadyAtTimer clock.Timer
    waitingForQueue := &waitForPriorityQueue[T]{}
    heap.Init(waitingForQueue)
    waitingEntryByData := map[T]*waitFor[T]{}

    for {
        if q.TypedInterface.ShuttingDown() { return }

        now := q.clock.Now()

        // [1] 준비된 항목을 기본 큐로 이동
        for waitingForQueue.Len() > 0 {
            entry := waitingForQueue.Peek().(*waitFor[T])
            if entry.readyAt.After(now) { break }

            entry = heap.Pop(waitingForQueue).(*waitFor[T])
            q.Add(entry.data)
            delete(waitingEntryByData, entry.data)
        }

        // [2] 다음 준비 시간에 타이머 설정
        nextReadyAt := never
        if waitingForQueue.Len() > 0 {
            entry := waitingForQueue.Peek().(*waitFor[T])
            nextReadyAtTimer = q.clock.NewTimer(entry.readyAt.Sub(now))
            nextReadyAt = nextReadyAtTimer.C()
        }

        // [3] 이벤트 대기
        select {
        case <-q.stopCh:
            return
        case <-q.heartbeat.C():
            // 10초마다 준비된 항목 확인
        case <-nextReadyAt:
            // 다음 항목이 준비됨
        case waitEntry := <-q.waitingForAddCh:
            // 새 지연 항목 수신
            if waitEntry.readyAt.After(q.clock.Now()) {
                insert(waitingForQueue, waitingEntryByData, waitEntry)
            } else {
                q.Add(waitEntry.data)   // 이미 준비됨, 즉시 추가
            }
            // 채널에 추가 항목이 있으면 모두 처리 (드레인)
            drained := false
            for !drained {
                select {
                case waitEntry := <-q.waitingForAddCh:
                    // ...
                default:
                    drained = true
                }
            }
        }
    }
}
```

### 11.7 insert: 중복 처리

```go
// delaying_queue.go (355-369행)
func insert[T comparable](q *waitForPriorityQueue[T],
    knownEntries map[T]*waitFor[T], entry *waitFor[T]) {

    existing, exists := knownEntries[entry.data]
    if exists {
        // 이미 대기 중인 항목이 있으면, 더 빠른 시간으로 갱신
        if existing.readyAt.After(entry.readyAt) {
            existing.readyAt = entry.readyAt
            heap.Fix(q, existing.index)
        }
        return
    }

    heap.Push(q, entry)
    knownEntries[entry.data] = entry
}
```

**핵심**: 같은 항목이 여러 번 AddAfter되면,
가장 빠른 readyAt이 적용된다 (더 이른 시간으로만 갱신).

---

## 12. RateLimitingQueue: 속도 제한 큐

### 12.1 TypedRateLimitingInterface

```go
// staging/src/k8s.io/client-go/util/workqueue/rate_limiting_queue.go (27-40행)
type TypedRateLimitingInterface[T comparable] interface {
    TypedDelayingInterface[T]

    AddRateLimited(item T)    // RateLimiter가 결정한 시간 후 추가
    Forget(item T)            // 재시도 추적 중단
    NumRequeues(item T) int   // 재시도 횟수 조회
}
```

### 12.2 rateLimitingType 구조체

```go
// rate_limiting_queue.go (130-134행)
type rateLimitingType[T comparable] struct {
    TypedDelayingInterface[T]    // 내장 지연 큐
    rateLimiter TypedRateLimiter[T]
}
```

### 12.3 AddRateLimited

```go
// rate_limiting_queue.go (137-139행)
func (q *rateLimitingType[T]) AddRateLimited(item T) {
    q.TypedDelayingInterface.AddAfter(item, q.rateLimiter.When(item))
}
```

`When(item)` → 대기 시간 계산 → `AddAfter(item, duration)`

### 12.4 Forget와 NumRequeues

```go
// rate_limiting_queue.go (141-147행)
func (q *rateLimitingType[T]) NumRequeues(item T) int {
    return q.rateLimiter.NumRequeues(item)
}

func (q *rateLimitingType[T]) Forget(item T) {
    q.rateLimiter.Forget(item)
}
```

**Forget을 호출하지 않으면** 재시도 횟수가 계속 증가하여
대기 시간이 무한히 길어진다. 성공적인 처리 후 반드시 Forget을 호출해야 한다.

---

## 13. Rate Limiter 종류

### 13.1 TypedRateLimiter 인터페이스

```go
// staging/src/k8s.io/client-go/util/workqueue/default_rate_limiters.go (30-38행)
type TypedRateLimiter[T comparable] interface {
    When(item T) time.Duration    // 다음 재시도까지 대기 시간
    Forget(item T)                // 재시도 추적 중단
    NumRequeues(item T) int       // 재시도 횟수
}
```

### 13.2 ItemExponentialFailureRateLimiter (지수 백오프)

```go
// default_rate_limiters.go (84-90행)
type TypedItemExponentialFailureRateLimiter[T comparable] struct {
    failuresLock sync.Mutex
    failures     map[T]int      // 항목별 실패 횟수
    baseDelay    time.Duration  // 기본 대기 시간
    maxDelay     time.Duration  // 최대 대기 시간
}
```

#### When 계산

```go
// default_rate_limiters.go (116-135행)
func (r *TypedItemExponentialFailureRateLimiter[T]) When(item T) time.Duration {
    r.failuresLock.Lock()
    defer r.failuresLock.Unlock()

    exp := r.failures[item]
    r.failures[item] = r.failures[item] + 1

    // backoff = baseDelay * 2^failures
    backoff := float64(r.baseDelay.Nanoseconds()) * math.Pow(2, float64(exp))
    if backoff > math.MaxInt64 {
        return r.maxDelay
    }

    calculated := time.Duration(backoff)
    if calculated > r.maxDelay {
        return r.maxDelay
    }

    return calculated
}
```

지수 백오프 계산 예시 (baseDelay=5ms, maxDelay=1000s):

| 실패 횟수 | 대기 시간 | 계산 |
|-----------|----------|------|
| 0 | 5ms | 5ms * 2^0 |
| 1 | 10ms | 5ms * 2^1 |
| 2 | 20ms | 5ms * 2^2 |
| 3 | 40ms | 5ms * 2^3 |
| 5 | 160ms | 5ms * 2^5 |
| 10 | 5.12s | 5ms * 2^10 |
| 17 | ~10.5min | 5ms * 2^17, capped at maxDelay |

### 13.3 BucketRateLimiter (토큰 버킷)

```go
// default_rate_limiters.go (62-64행)
type TypedBucketRateLimiter[T comparable] struct {
    *rate.Limiter   // golang.org/x/time/rate
}

func (r *TypedBucketRateLimiter[T]) When(item T) time.Duration {
    return r.Limiter.Reserve().Delay()
}
```

전체 처리율을 제한하는 토큰 버킷 알고리즘.
개별 항목이 아닌 **전체 큐의 처리 속도**를 제어한다.

### 13.4 MaxOfRateLimiter (조합)

```go
// default_rate_limiters.go (218-241행)
type TypedMaxOfRateLimiter[T comparable] struct {
    limiters []TypedRateLimiter[T]
}

func (r *TypedMaxOfRateLimiter[T]) When(item T) time.Duration {
    ret := time.Duration(0)
    for _, limiter := range r.limiters {
        curr := limiter.When(item)
        if curr > ret {
            ret = curr   // 가장 긴 대기 시간 사용
        }
    }
    return ret
}
```

여러 RateLimiter 중 **가장 긴 대기 시간**을 반환한다.

### 13.5 DefaultControllerRateLimiter

```go
// default_rate_limiters.go (50-56행)
func DefaultTypedControllerRateLimiter[T comparable]() TypedRateLimiter[T] {
    return NewTypedMaxOfRateLimiter(
        NewTypedItemExponentialFailureRateLimiter[T](5*time.Millisecond, 1000*time.Second),
        &TypedBucketRateLimiter[T]{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
    )
}
```

기본 컨트롤러 RateLimiter는 **두 가지를 조합**한다:

```
DefaultControllerRateLimiter = MaxOf(
    ItemExponentialFailure(base=5ms, max=1000s),  // 항목별 지수 백오프
    BucketRateLimiter(10 QPS, burst=100),          // 전체 속도 제한
)
```

```
                         대기 시간
                            ^
                            |
ItemExponentialFailure      |     /------- maxDelay (1000s)
                            |    /
                            |   /
                            |  /  ← 지수적 증가
                            | /
                            |/
                            +------+-----> 실패 횟수
BucketRateLimiter           |
(10 QPS, burst 100)         |  ← 일정한 전체 처리율

MaxOf: 둘 중 더 긴 대기 시간을 적용
```

### 13.6 ItemFastSlowRateLimiter

```go
// default_rate_limiters.go (156-179행)
type TypedItemFastSlowRateLimiter[T comparable] struct {
    failures        map[T]int
    maxFastAttempts int
    fastDelay       time.Duration
    slowDelay       time.Duration
}

func (r *TypedItemFastSlowRateLimiter[T]) When(item T) time.Duration {
    r.failures[item] = r.failures[item] + 1
    if r.failures[item] <= r.maxFastAttempts {
        return r.fastDelay
    }
    return r.slowDelay
}
```

일정 횟수까지 빠르게 재시도한 후, 느린 재시도로 전환:

```
실패 1~5:  fastDelay (예: 200ms)
실패 6~:   slowDelay (예: 30s)
```

### 13.7 WithMaxWaitRateLimiter

```go
// default_rate_limiters.go (266-278행)
type TypedWithMaxWaitRateLimiter[T comparable] struct {
    limiter  TypedRateLimiter[T]
    maxDelay time.Duration
}

func (w TypedWithMaxWaitRateLimiter[T]) When(item T) time.Duration {
    delay := w.limiter.When(item)
    if delay > w.maxDelay {
        return w.maxDelay
    }
    return delay
}
```

기존 RateLimiter를 감싸서 최대 대기 시간을 강제한다.

---

## 14. Informer에서 Controller까지: 전체 파이프라인

### 14.1 전형적인 컨트롤러 패턴

```go
// 컨트롤러 설정 (의사 코드)
func NewMyController(informer cache.SharedIndexInformer) *MyController {
    queue := workqueue.NewTypedRateLimitingQueue(
        workqueue.DefaultTypedControllerRateLimiter[string](),
    )

    informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
        AddFunc: func(obj interface{}) {
            key, _ := cache.MetaNamespaceKeyFunc(obj)
            queue.Add(key)
        },
        UpdateFunc: func(old, new interface{}) {
            key, _ := cache.MetaNamespaceKeyFunc(new)
            queue.Add(key)
        },
        DeleteFunc: func(obj interface{}) {
            key, _ := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
            queue.Add(key)
        },
    })

    return &MyController{
        informer: informer,
        queue:    queue,
        lister:   informer.GetIndexer(),
    }
}

func (c *MyController) Run(ctx context.Context) {
    defer c.queue.ShutDown()

    go c.informer.Run(ctx.Done())

    // 캐시 동기화 대기
    if !cache.WaitForCacheSync(ctx.Done(), c.informer.HasSynced) {
        return
    }

    // 워커 goroutine 시작
    for i := 0; i < workers; i++ {
        go wait.UntilWithContext(ctx, c.runWorker, time.Second)
    }

    <-ctx.Done()
}

func (c *MyController) runWorker(ctx context.Context) {
    for c.processNextItem(ctx) {
    }
}

func (c *MyController) processNextItem(ctx context.Context) bool {
    key, shutdown := c.queue.Get()
    if shutdown { return false }
    defer c.queue.Done(key)

    err := c.reconcile(ctx, key)
    if err == nil {
        c.queue.Forget(key)    // 성공: 재시도 카운터 초기화
        return true
    }

    // 실패: 속도 제한 재큐잉
    c.queue.AddRateLimited(key)
    return true
}
```

### 14.2 전체 파이프라인 다이어그램

```
API Server
  |
  | List + Watch
  v
Reflector ─────────────────────────────────────────────────┐
  |                                                        |
  | Add/Update/Delete                                      |
  v                                                        |
DeltaFIFO                                                  |
  |                                                        |
  | Pop()                                                  |
  v                                                        |
Controller.processLoop                                     |
  |                                                        |
  | processDeltas()                                        |
  |                                                        |
  +─── Store/Indexer 업데이트 (로컬 캐시)                    |
  |                                                        |
  +─── ResourceEventHandler 호출                            |
         |                                                 |
         | OnAdd/OnUpdate/OnDelete                         |
         v                                                 |
  MetaNamespaceKeyFunc(obj) → "namespace/name"             |
         |                                                 |
         v                                                 |
  WorkQueue.Add(key)                                       |
  (중복 제거: 같은 키는 1번만)                                |
         |                                                 |
         | Get()                                           |
         v                                                 |
  Worker Goroutine                                         |
         |                                                 |
         | Lister.Get(namespace, name) ← Store/Indexer에서  |
         |                               최신 오브젝트 조회  |
         v                                                 |
  Reconcile(key)                                           |
         |                                                 |
         +── 성공 → queue.Forget(key)                       |
         |            queue.Done(key)                       |
         |                                                 |
         +── 실패 → queue.AddRateLimited(key)               |
                      queue.Done(key)                       |
                      (지수 백오프 후 재시도)                 |
```

### 14.3 Level-Triggered vs Edge-Triggered

Kubernetes 컨트롤러는 **Level-Triggered** 방식으로 동작한다:

```
Edge-Triggered (이벤트 기반):
  "Pod가 생성됐다" → 이벤트 처리
  문제: 이벤트 유실 시 상태 불일치

Level-Triggered (상태 기반):
  "Pod의 현재 상태가 원하는 상태와 다르다" → 조정(reconcile)
  장점: 이벤트 유실에도 상태 수렴 보장
```

이것이 컨트롤러가 이벤트 핸들러에서 직접 처리하지 않고
WorkQueue를 통해 키만 넘기고, reconcile에서 **최신 캐시 상태**를
조회하는 이유다.

---

## 15. 설계 원칙: Why

### 15.1 왜 SharedInformer로 Watch를 공유하는가?

**문제**: Kubernetes 클러스터에는 수십~수백 개의 컨트롤러가 동작한다.
각 컨트롤러가 독립적으로 Watch를 열면 API Server에 엄청난 부하가 발생한다.

```
독립 Watch (비효율):
  Controller1 → Watch(pods) ──┐
  Controller2 → Watch(pods) ──┤── API Server (3개의 Watch 연결)
  Controller3 → Watch(pods) ──┘

SharedInformer (효율):
  SharedInformer → Watch(pods) ── API Server (1개의 Watch 연결)
       |
       +── Controller1 (핸들러)
       +── Controller2 (핸들러)
       +── Controller3 (핸들러)
```

### 15.2 왜 DeltaFIFO에서 Deltas를 축적하는가?

**문제**: 오브젝트가 빠르게 변경될 때 (예: 1초에 10번 업데이트),
중간 상태를 모두 처리하면 비효율적이다.

**해결**: DeltaFIFO는 같은 오브젝트에 대한 변경을 모아두고,
Pop할 때 한꺼번에 전달한다.

```
1초 동안의 이벤트:
  pod-a: Added, Updated, Updated, Updated

DeltaFIFO에서 Pop:
  Deltas = [Delta{Added, v1}, Delta{Updated, v2}, Delta{Updated, v3}, Delta{Updated, v4}]

processDeltas가 최종 상태만 캐시에 반영:
  Store["default/pod-a"] = v4
  handler.OnAdd(v4)  또는  handler.OnUpdate(old, v4)
```

### 15.3 왜 WorkQueue에서 중복을 제거하는가?

**문제**: 같은 Pod에 대해 짧은 시간에 여러 이벤트가 발생하면,
불필요한 reconcile이 여러 번 실행된다.

**해결**: dirty set으로 중복을 제거한다.

```
이벤트 발생:
  pod-a Updated → queue.Add("default/pod-a")
  pod-a Updated → queue.Add("default/pod-a")  ← dirty에 이미 있으므로 무시
  pod-a Updated → queue.Add("default/pod-a")  ← dirty에 이미 있으므로 무시

실제 처리:
  reconcile("default/pod-a") ← 1번만 실행
  (최신 상태를 캐시에서 조회하므로 중간 상태 누락 무관)
```

### 15.4 왜 이벤트 핸들러에서 직접 처리하지 않는가?

```go
// 나쁜 패턴 (하지 말 것):
informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
    AddFunc: func(obj interface{}) {
        // 여기서 직접 무거운 작업 수행 ← 위험!
        doHeavyWork(obj)
    },
})
```

**이유**:
1. 이벤트 핸들러는 Informer의 lock 아래에서 실행될 수 있음
2. 긴 처리는 전체 Informer를 블로킹
3. 실패 시 재시도 메커니즘이 없음
4. 동시성 제어 불가

```go
// 올바른 패턴:
informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
    AddFunc: func(obj interface{}) {
        key, _ := cache.MetaNamespaceKeyFunc(obj)
        queue.Add(key)   // 키만 큐에 넣고 즉시 반환
    },
})

// 별도 goroutine에서 처리
func processNextItem() {
    key, _ := queue.Get()
    defer queue.Done(key)
    reconcile(key)   // 시간이 걸려도 OK
}
```

### 15.5 왜 키(key)만 큐에 넣는가?

이벤트 핸들러에서 오브젝트 자체가 아닌 **키만** 큐에 넣는 이유:

1. **최종 일관성**: reconcile 시점에 캐시에서 **최신 상태**를 조회
2. **메모리 절약**: 오브젝트 사본을 큐에 보관하지 않음
3. **중복 제거**: 문자열 키로 동일성 판단이 간단
4. **Level-Triggered**: 현재 상태와 원하는 상태의 차이만 중요

### 15.6 왜 지수 백오프 + 토큰 버킷을 조합하는가?

```
ItemExponentialFailure만 사용하면?
  → 새로운 항목은 즉시 처리 (0ms 대기)
  → 실패 항목만 지연
  → 전체 처리율 제한 없음 → API Server 부하 가능

BucketRateLimiter만 사용하면?
  → 전체 처리율은 제한
  → 반복 실패하는 항목이 즉시 재시도 → 무의미한 반복

둘의 조합 (MaxOf):
  → 새 항목: BucketRateLimiter의 제한만 적용 (10 QPS)
  → 실패 항목: 지수 백오프로 점진적 대기 + 전체 속도 제한
  → 최적의 균형
```

### 15.7 왜 TransformFunc이 있는가?

```go
// delta_fifo.go (160-176행)
type TransformFunc func(interface{}) (interface{}, error)
```

대규모 클러스터에서 Informer 캐시의 메모리 사용량이 문제가 된다.
TransformFunc으로 불필요한 필드를 제거하여 메모리를 절약할 수 있다:

```go
informer.SetTransform(func(obj interface{}) (interface{}, error) {
    pod := obj.(*v1.Pod)
    // 관리 필드, 어노테이션 등 불필요한 데이터 제거
    pod.ManagedFields = nil
    pod.Annotations = nil
    return pod, nil
})
```

---

## 16. 정리

### 16.1 구성요소별 핵심 역할

```
+-------------------------------------------------------------+
|                    SharedInformer                             |
|                                                              |
|  Reflector         DeltaFIFO        Controller    Indexer    |
|  ┌─────────┐      ┌─────────┐     ┌──────────┐  ┌────────┐ |
|  │List     │      │items    │     │processLoop│  │cache   │ |
|  │Watch    │─Add─>│queue    │─Pop>│processDeltas>│indices │ |
|  │         │      │dedupDel │     │           │  │        │ |
|  └─────────┘      └─────────┘     └──────────┘  └────────┘ |
|                                         │                    |
|                               ResourceEventHandler          |
|                               OnAdd/OnUpdate/OnDelete        |
+-------------------------------------------------------------+
                                         │
                                    queue.Add(key)
                                         │
                                         v
+-------------------------------------------------------------+
|                      WorkQueue                               |
|  ┌──────────────────────────────────────────────┐           |
|  │  RateLimitingQueue                            │           |
|  │  ┌──────────────────────────────────┐        │           |
|  │  │  DelayingQueue                    │        │           |
|  │  │  ┌──────────────────────┐        │        │           |
|  │  │  │  Typed (기본 큐)      │        │        │           |
|  │  │  │  dirty + processing   │        │        │           |
|  │  │  │  + FIFO queue         │        │        │           |
|  │  │  └──────────────────────┘        │        │           |
|  │  │  + waitForPriorityQueue (힙)     │        │           |
|  │  │  + waitingLoop goroutine          │        │           |
|  │  └──────────────────────────────────┘        │           |
|  │  + RateLimiter (지수백오프+토큰버킷)           │           |
|  └──────────────────────────────────────────────┘           |
+-------------------------------------------------------------+
                                         │
                                   queue.Get(key)
                                         │
                                         v
                               Custom Controller
                                  Reconcile(key)
```

### 16.2 핵심 인터페이스 요약

| 인터페이스 | 소스 파일 | 핵심 메서드 |
|-----------|----------|-----------|
| `SharedInformer` | shared_informer.go | `AddEventHandler`, `Run`, `HasSynced` |
| `Store` | store.go | `Add`, `Update`, `Delete`, `Get`, `GetByKey` |
| `Queue` (DeltaFIFO) | delta_fifo.go | `Add`, `Update`, `Delete`, `Pop`, `Replace` |
| `Controller` | controller.go | `RunWithContext`, `HasSynced` |
| `ResourceEventHandler` | controller.go | `OnAdd`, `OnUpdate`, `OnDelete` |
| `TypedInterface` | queue.go | `Add`, `Get`, `Done`, `ShutDown` |
| `TypedDelayingInterface` | delaying_queue.go | `AddAfter` |
| `TypedRateLimitingInterface` | rate_limiting_queue.go | `AddRateLimited`, `Forget` |
| `TypedRateLimiter` | default_rate_limiters.go | `When`, `Forget`, `NumRequeues` |

### 16.3 흐름 요약 (한 문장)

API Server에서 List+Watch로 가져온 데이터를 DeltaFIFO에 축적하고,
Controller가 Pop하여 로컬 캐시를 업데이트하면서 이벤트 핸들러를 호출하면,
핸들러가 오브젝트 키를 RateLimitingQueue에 넣고,
워커 goroutine이 키를 꺼내 최신 캐시 상태를 기반으로 reconcile한다.
