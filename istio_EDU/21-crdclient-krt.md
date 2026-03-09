# 21. Config Store (CRD Client)와 krt 프레임워크 Deep-Dive

> Istio CRD의 CRUD/Watch를 담당하는 Config Store와 선언적 데이터 변환 프레임워크 krt의 내부 동작 원리

---

## 1. 개요

Istio의 설정 관리는 두 계층으로 구성된다:

1. **Config Store / CRD Client (`pilot/pkg/config/kube/crdclient/`)**: Kubernetes CRD(VirtualService, DestinationRule 등)를 Istio 내부 `config.Config` 타입으로 변환하고 CRUD 및 Watch 기능을 제공하는 계층. `model.ConfigStoreController` 인터페이스를 구현한다.

2. **krt 프레임워크 (`pkg/kube/krt/`)**: "Kubernetes Declarative Controller Runtime"의 약자로, 선언적 데이터 변환 파이프라인을 제공하는 프레임워크. Kubernetes informer를 추상화하여 임의의 타입 간 변환을 선언적으로 정의할 수 있다.

```
┌──────────────────────────────────────────────────────────┐
│                 Istio 설정 관리 계층도                      │
│                                                          │
│  Kubernetes API Server                                   │
│       │                                                  │
│       ▼                                                  │
│  kclient.Informer (Kubernetes Informer 래퍼)             │
│       │                                                  │
│       ▼                                                  │
│  krt.WrapClient → krt.Collection[controllers.Object]     │
│       │                                                  │
│       ▼                                                  │
│  krt.MapCollection → krt.Collection[config.Config]       │
│       │                                                  │
│       ▼                                                  │
│  CRD Client (nsStore) ← model.ConfigStoreController     │
│       │                                                  │
│       ▼                                                  │
│  PushContext, xDS Server 등 상위 계층                     │
└──────────────────────────────────────────────────────────┘
```

---

## 2. CRD Client 아키텍처

### 2.1 Client 구조체

```
// pilot/pkg/config/kube/crdclient/client.go 라인 58-81

Client:
  - schemas: collection.Schemas           // 지원하는 스키마 집합
  - domainSuffix: string                 // 설정 메타데이터의 도메인 접미사
  - revision: string                     // 이 컨트롤 플레인의 리비전
  - kinds: map[config.GroupVersionKind]nsStore  // GVK → 저장소 매핑
  - started: *atomic.Bool               // 시작 여부 (중복 실행 방지)
  - schemasByCRDName: map[string]resource.Schema  // CRD 이름 → 스키마
  - client: kube.Client                  // Kubernetes 클라이언트
  - logger: *log.Scope                   // 레이블된 로거
  - filtersByGVK: map[GVK]kubetypes.Filter  // GVK별 필터
  - stop: chan struct{}                  // 종료 채널
```

### 2.2 nsStore (네임스페이스 인덱싱된 저장소)

```
// pilot/pkg/config/kube/crdclient/client.go 라인 83-88

nsStore:
  - collection: krt.Collection[config.Config]  // krt 컬렉션
  - index: krt.Index[string, config.Config]    // 네임스페이스 인덱스
  - handlers: []krt.HandlerRegistration        // 등록된 이벤트 핸들러
```

### 2.3 초기화 흐름

```
// pilot/pkg/config/kube/crdclient/client.go 라인 99-143

NewForSchemas(client, opts, schemas) 흐름:
  1. 스키마 매핑 구축:
     for s in schemas.All():
       if !s.IsSynthetic():
         schemasByCRDName[plural.group] = s

  2. krt 옵션 빌더 생성:
     kopts = krt.NewOptionsBuilder(stop, "crdclient", debugger)

  3. 각 CRD에 대해 addCRD 호출:
     for s in schemas.All():
       addCRD(name, kopts)  // krt 컬렉션 + 인포머 설정
```

### 2.4 addCRD 상세

```
// pilot/pkg/config/kube/crdclient/client.go 라인 357-437

addCRD(name, opts) 흐름:
  1. 스키마 조회 및 GVK/GVR 결정
  2. 변환 함수 조회 (translationMap에서)
  3. 필터 구성:
     - 리비전 필터: config.LabelsInRevision 확인
     - 네임스페이스 필터: client.ObjectFilter() (클러스터 스코프 제외)
     - 타입별 추가 필터: filtersByGVK에서 조회
     - 특수 케이스: KubernetesGateway는 리비전 필터 제외

  4. Informer 생성:
     if s.IsBuiltin():
       kclient.NewUntypedInformer(client, gvr, filter)
     else:
       kclient.NewDelayedInformer(client, gvr, filter)

  5. krt 컬렉션 구축:
     wrappedClient = krt.WrapClient(kc, opts...)
     collection = krt.MapCollection(wrappedClient, translateFunc, opts...)
     index = krt.NewNamespaceIndex(collection)

  6. 이벤트 메트릭 핸들러 등록:
     collection.RegisterBatch(incrementEvent(kind, event))
```

---

## 3. CRUD 연산

### 3.1 Get

```
// pilot/pkg/config/kube/crdclient/client.go 라인 230-251

Get(typ, name, namespace):
  h = kinds[typ]           // nsStore 조회
  key = namespace + "/" + name  // 또는 name만 (클러스터 스코프)
  obj = h.collection.GetKey(key)  // krt 컬렉션에서 직접 조회
  return obj               // *config.Config 또는 nil
```

### 3.2 List

```
// pilot/pkg/config/kube/crdclient/client.go 라인 298-309

List(kind, namespace):
  h = kinds[kind]
  if namespace == NamespaceAll:
    return h.collection.List()        // 전체 목록
  return h.index.Lookup(namespace)    // 네임스페이스 인덱스로 필터링
```

### 3.3 Create / Update / Delete

```
Create(cfg):
  create(client, cfg, objectMeta)    // 생성된 코드의 타입별 함수 호출
  return resourceVersion

Update(cfg):
  update(client, cfg, objectMeta)    // 타입별 업데이트 함수
  return resourceVersion

UpdateStatus(cfg):
  updateStatus(client, cfg, objectMeta)  // 상태 서브리소스 업데이트
  return resourceVersion

Delete(typ, name, namespace, resourceVersion):
  delete(client, typ, name, namespace, resourceVersion)
```

### 3.4 JSON Patch 생성

```
// pilot/pkg/config/kube/crdclient/client.go 라인 334-355

genPatchBytes(oldRes, modRes, patchType):
  oldJSON = json.Marshal(oldRes)
  newJSON = json.Marshal(modRes)

  switch patchType:
    JSONPatchType:   jsonpatch.CreatePatch(old, new)  // RFC 6902
    MergePatchType:  jsonmerge.CreateMergePatch(old, new)  // RFC 7386
```

---

## 4. 이벤트 핸들러 등록

```
// pilot/pkg/config/kube/crdclient/client.go 라인 153-171

RegisterEventHandler(kind, handler):
  c = kinds[kind]
  c.collection.RegisterBatch(func(events):
    for event in events:
      switch event.Event:
        EventAdd:    handler(empty, *event.New, EventAdd)
        EventUpdate: handler(*event.Old, *event.New, EventUpdate)
        EventDelete: handler(empty, *event.Old, EventDelete)
  , false)

핵심: krt의 RegisterBatch를 통해 이벤트를 수신하고,
      model.EventHandler 시그니처로 변환하여 상위 계층에 전달
```

---

## 5. krt 프레임워크 아키텍처

### 5.1 핵심 인터페이스: Collection

```
// pkg/kube/krt/core.go 라인 28-44

Collection[T] 인터페이스:
  GetKey(k string) *T     // 키로 단일 객체 조회
  List() []T              // 전체 목록
  EventStream[T]          // 이벤트 처리 (임베드)
  Metadata() Metadata     // 메타데이터

EventStream[T] 인터페이스:
  Syncer                  // 동기화 상태 (HasSynced, WaitUntilSynced)
  Register(func(Event[T])) HandlerRegistration      // 단일 이벤트 핸들러
  RegisterBatch(func([]Event[T]), bool) HandlerRegistration  // 배치 핸들러
```

### 5.2 Event 구조체

```
// pkg/kube/krt/core.go 라인 116-143

Event[T]:
  - Old: *T                      // 이전 객체 (Update, Delete에서 설정)
  - New: *T                      // 새 객체 (Add, Update에서 설정)
  - Event: controllers.EventType // Add, Update, Delete

Items() []T:  Old와 New 모두 반환
Latest() T:   최신 객체 (New가 있으면 New, 없으면 Old)
```

### 5.3 Transformation 유형

```
// pkg/kube/krt/core.go 라인 174-189

TransformationEmpty[T]:    func(ctx) *T              → Singleton
TransformationSingle[I,O]: func(ctx, I) *O           → 1:1 매핑
TransformationMulti[I,O]:  func(ctx, I) []O           → 1:N 매핑
TransformationMultiStatus[I,IS,O]: func(ctx, I) (*IS, []O)  → 1:N + 상태
TransformationSingleStatus[I,IS,O]: func(ctx, I) (*IS, *O)   → 1:1 + 상태
TransformationEmptyToMulti[T]: func(ctx) []T          → 0:N 매핑
```

---

## 6. krt Collection 구현 (manyCollection)

### 6.1 manyCollection 구조체

```
// pkg/kube/krt/collection.go 라인 192-226

manyCollection[I, O]:
  - collectionName: string                  // 이름
  - id: collectionUID                       // 고유 ID
  - parent: Collection[I]                   // 입력 컬렉션

  데이터 상태 (mu로 보호):
  - collectionState: multiIndex[I, O]       // 입출력 매핑
  - dependencyState: dependencyState[I]     // 의존성 상태
  - indexes: map[string]collectionIndex     // 내부 인덱스

  이벤트:
  - eventHandlers: *handlerSet[O]          // 이벤트 핸들러
  - transformation: TransformationMulti[I, O]  // 변환 함수

  제어:
  - synced: chan struct{}                   // 동기화 완료 채널
  - stop: <-chan struct{}                   // 종료 채널
  - queue: queue.Instance                  // 작업 큐
```

### 6.2 multiIndex (입출력 저장소)

```
// pkg/kube/krt/collection.go 라인 318-322

multiIndex[I, O]:
  - outputs: map[Key[O]]O                  // 출력 객체 저장
  - inputs: map[Key[I]]I                   // 입력 객체 저장
  - mappings: map[Key[I]]sets.Set[Key[O]]  // I→O 키 매핑
```

### 6.3 컬렉션 생성과 실행

```
// pkg/kube/krt/collection.go 라인 583-661

newManyCollection 흐름:
  1. manyCollection 초기화
  2. queue 생성 (NewWithSync → 동기화 완료 시 synced 채널 닫기)
  3. runQueue 고루틴 시작

runQueue 흐름:
  1. 부모 컬렉션의 동기화 대기 (WaitUntilSynced)
  2. 부모 컬렉션에 배치 핸들러 등록 (RegisterBatch)
  3. 이벤트 발생 시 queue에 작업 추가
  4. 핸들러 동기화 대기
  5. queue.Run(stop) 실행 → 큐의 작업을 순차 처리
```

---

## 7. krt 이벤트 처리 파이프라인

### 7.1 Primary Input 이벤트

```
// pkg/kube/krt/collection.go 라인 400-422

onPrimaryInputEvent(items []Event[I]):
  1. 각 이벤트의 최신 상태 확인:
     iObj = parent.GetKey(key)
     if nil → 삭제로 변환
     else → New 업데이트
  2. handleChangedPrimaryInputEvents 호출

handleChangedPrimaryInputEvents(items):
  [잠금 없이] 각 비삭제 이벤트에 대해:
    ctx = collectionDependencyTracker 생성
    results = transformation(ctx, input)   // 변환 실행
    결과를 recomputedResults에 저장

  [mu.Lock()]
    삭제 이벤트:
      - mappings에서 모든 출력 키 조회
      - 각 출력 객체에 대해 EventDelete 생성
      - outputs/mappings/inputs/dependencyState 정리

    추가/업데이트 이벤트:
      - 의존성 상태 업데이트
      - 새 키 집합과 기존 키 집합 비교
      - 키별로:
        newExists && oldExists → Equal이면 스킵, 아니면 EventUpdate
        newExists만           → EventAdd
        oldExists만           → EventDelete
      - 인덱스 업데이트
      - 이벤트 분배
```

### 7.2 Secondary Dependency 이벤트

```
// pkg/kube/krt/collection.go 라인 665-703

onSecondaryDependencyEvent(sourceCollection, events):
  1. changedInputKeys 계산:
     - 의존성 역인덱스를 통해 영향받는 입력 키 찾기
     - 전체 의존성 순회 (역인덱스 없는 경우)

  2. 변경된 입력에 대해 Event[I] 생성:
     - 부모 컬렉션에서 최신 객체 조회
     - 없으면 EventDelete, 있으면 EventUpdate

  3. handleChangedPrimaryInputEvents 호출 (1차 이벤트와 동일 로직)
```

### 7.3 의존성 추적 (dependencyState)

```
// pkg/kube/krt/collection.go 라인 40-112

dependencyState[I]:
  - collectionDependencies: sets.Set[collectionUID]  // 의존하는 컬렉션 ID
  - objectDependencies: map[Key[I]][]*dependency     // 입력별 의존성
  - indexedDependencies: map[indexedDependency]sets.Set[Key[I]]  // 역인덱스
  - indexedDependenciesExtractor: map[extractorKey]objectKeyExtractor  // 키 추출기

update(key, deps):
  objectDependencies[key] = deps
  각 의존성의 필터에서 역인덱스 키 추출
  indexedDependencies에 추가

changedInputKeys(source, events):
  역인덱스로 빠른 조회 가능한 경우:
    이벤트의 각 객체에서 역인덱스 키 추출
    해당 키에 매핑된 입력 키 집합 반환
  역인덱스 없는 경우:
    모든 objectDependencies 순회하여 매칭 확인
```

---

## 8. krt Fetch 메커니즘

### 8.1 Fetch 함수

```
// pkg/kube/krt/fetch.go 라인 40-103

Fetch[T](ctx, collection, opts...):
  1. dependency 객체 생성 (collection의 uid, filter)
  2. FetchOption 적용 (FilterKey, FilterLabel 등)
  3. registerDependency 호출:
     - 의존성 등록
     - 처음 의존하는 컬렉션이면 핸들러 등록
     - WaitUntilSynced 대기

  4. 데이터 조회:
     if filter.keys가 있으면:
       각 키에 대해 GetKey 호출 (O(1))
     elif filter.index가 있으면:
       인덱스에서 조회 (O(1))
     else:
       List()로 전체 조회 (O(N))

  5. filter.Matches로 추가 필터링
  6. 결과 반환
```

### 8.2 FetchOne

```
// pkg/kube/krt/fetch.go 라인 21-31

FetchOne[T](ctx, collection, opts...):
  res = Fetch(ctx, collection, opts...)
  if len(res) == 0: return nil
  if len(res) == 1: return &res[0]
  else: panic("FetchOne found more than 1 item")
```

---

## 9. krt 컬렉션 생성 패턴

### 9.1 Informer 기반 (WrapClient)

```
// pkg/kube/krt/informer.go 라인 196-232

WrapClient[I](informer, opts...):
  1. informer[I] 구조체 생성
  2. 고루틴으로 informer 동기화 대기
  3. 동기화 완료 시 synced 채널 닫기
  4. stop 시 핸들러 종료

사용 예:
  pods := krt.WrapClient(kclient.New[*v1.Pod](client))
```

### 9.2 NewCollection (1:1 변환)

```
// pkg/kube/krt/collection.go 라인 555-570

NewCollection[I, O](parent, transform, opts...):
  TransformationSingle을 TransformationMulti로 래핑:
    hm = func(ctx, i) []O { return []O{*hf(ctx, i)} }
  newManyCollection(parent, hm, opts)

사용 예:
  simplePods := krt.NewCollection(pods, func(ctx, pod) *SimplePod {
    return &SimplePod{Name: pod.Name}
  })
```

### 9.3 NewManyCollection (1:N 변환)

```
// pkg/kube/krt/collection.go 라인 575-581

NewManyCollection[I, O](parent, transform, opts...):
  newManyCollection(parent, transform, opts)

사용 예:
  endpoints := krt.NewManyCollection(services, func(ctx, svc) []Endpoint {
    pods := krt.Fetch(ctx, allPods, krt.FilterLabel(svc.Selector))
    return makeEndpoints(svc, pods)
  })
```

### 9.4 NewSingleton (0:1)

```
// pkg/kube/krt/singleton.go 라인 285-298

NewSingleton[O](transform, opts...):
  dummyCollection = NewStatic[dummyValue](&dummyValue{}, true)
  NewCollection(dummyCollection, func(ctx, _) *O {
    return transform(ctx)
  })
  collectionAdapter로 래핑

사용 예:
  count := krt.NewSingleton(func(ctx) *int {
    cms := krt.Fetch(ctx, configMaps)
    return ptr.Of(len(cms))
  })
```

### 9.5 Static (정적 값)

```
// pkg/kube/krt/singleton.go 라인 41-67

NewStatic[T](initial, startSynced, opts...):
  atomic.Pointer로 값 저장
  Set(value) → 이전 값과 비교 → 이벤트 분배
  MarkSynced() → synced 플래그 설정
```

---

## 10. krt 인덱스

### 10.1 네임스페이스 인덱스

```
NewNamespaceIndex(collection):
  collection.index("namespace", func(o) []string {
    return []string{o.GetNamespace()}
  })

사용: CRD Client의 List(kind, namespace)에서 활용
```

### 10.2 collectionIndex

```
// pkg/kube/krt/collection.go 라인 228-268

collectionIndex[I, O]:
  - extract: func(o O) []string    // 인덱스 키 추출
  - index: map[string]sets.Set[Key[O]]  // 키 → 출력 키 집합

Lookup(key):
  keys = index[key]
  outputs에서 각 키에 해당하는 객체 수집

update(event, key):
  Old가 있으면 이전 인덱스 키 삭제
  New가 있으면 새 인덱스 키 추가
```

---

## 11. CRD Client와 krt의 통합 상세

### 11.1 필터 체인 구성

```
// pilot/pkg/config/kube/crdclient/client.go 라인 381-405

필터 구성 순서:
  1. 네임스페이스 필터: client.ObjectFilter() (Discovery Selector)
  2. 리비전 필터: config.LabelsInRevision(labels, revision)
  3. 타입별 필터: filtersByGVK[gvk].ObjectFilter
  4. 필드 선택자: filtersByGVK[gvk].FieldSelector
  5. 변환 함수: filtersByGVK[gvk].ObjectTransform

조합: kubetypes.ComposeFilters(namespaceFilter, revisionFilter, extraFilter)

특수 케이스:
  KubernetesGateway → 리비전 필터 제외 (모든 리비전이 Gateway를 볼 수 있어야 함)
```

### 11.2 HasSynced 체인

```
CRD Client HasSynced:
  for kind in kinds:
    if !kind.collection.HasSynced(): return false  // krt 컬렉션 동기화
    for h in kind.handlers:
      if !h.HasSynced(): return false              // 핸들러 동기화
  return true

krt Collection HasSynced:
  syncer.HasSynced() ← channelSyncer{ synced <-chan struct{} }
  → synced 채널이 닫혔는지 확인
```

---

## 12. 설계 결정과 "왜(Why)"

### 12.1 왜 dynamic informer 대신 코드 생성을 사용하는가?

```
문제:
  - dynamic informer는 Unstructured 객체를 캐시
  - Get/List 때마다 Marshal/Unmarshal 필요 → 성능 저하

해결:
  - istio/client-go로 타입별 informer 코드 생성
  - 캐시에 이미 마샬링된 객체 저장
  - Get/List가 O(1) 복잡도로 동작
```

### 12.2 왜 krt를 도입했는가?

```
기존 문제:
  - 각 컨트롤러가 수동으로 informer 관리
  - 의존성 추적이 암묵적 (코드 리뷰로만 확인)
  - 변환 로직과 이벤트 처리 로직이 혼재
  - 테스트 작성 어려움

krt 해결:
  - 선언적 변환 (func(I) O)
  - 자동 의존성 추적 (Fetch로 명시)
  - 프레임워크가 이벤트 분배/중복 제거 처리
  - 변환 함수만 테스트하면 됨

성능:
  - 최적화된 수동 컨트롤러 대비 ~10% 오버헤드
  - 대부분의 경우 수동 컨트롤러보다 나은 최적화
    (역인덱스, 변경 감지, 배치 처리 자동 적용)
```

### 12.3 왜 Transformation은 상태 없이 멱등해야 하는가?

```
요구사항:
  - Fetch를 통해서만 다른 컬렉션 조회
  - 외부 상태(HTTP 호출, 파일 읽기) 금지
  - 동일 입력에 대해 항상 동일 출력

이유:
  - krt가 언제든지 변환을 재실행할 수 있음
  - 의존성이 변경되면 자동으로 재계산
  - Fetch 외부에서 읽은 데이터는 변경을 감지할 수 없음
  - 위반 시 "정의되지 않은 동작" → 보통 오래된 데이터
```

### 12.4 왜 CRD Client에서 krt를 사용하는가?

```
직접 kclient 사용 시:
  - 네임스페이스 인덱스 수동 관리
  - 이벤트 핸들러 등록/해제 수동 관리
  - 동기화 상태 추적 수동 구현

krt 사용 시:
  - MapCollection으로 변환 자동화
  - NewNamespaceIndex로 인덱스 자동 생성
  - HasSynced 체인 자동 구성
  - RegisterBatch로 이벤트 핸들러 일관된 관리
```

---

## 13. krt 디버깅 지원

```
CollectionDump 구조체:
  - Outputs: map[string]any       // 현재 출력 객체
  - Inputs: map[string]InputDump  // 입력 → 출력 매핑 + 의존성
  - InputCollection: string       // 부모 컬렉션 이름
  - Synced: bool                  // 동기화 상태

InputDump:
  - Outputs: []string             // 출력 키 목록
  - Dependencies: []string        // 의존 컬렉션 목록

DebugHandler:
  maybeRegisterCollectionForDebugging(collection, debugger)
  → 디버그 UI에서 컬렉션 상태 조회 가능
```

---

## 14. 소스 코드 경로 정리

| 파일 | 역할 |
|------|------|
| `pilot/pkg/config/kube/crdclient/client.go` | CRD Client 구현, CRUD/Watch, krt 통합 |
| `pilot/pkg/config/kube/crdclient/types.gen.go` | 코드 생성된 타입별 변환/CRUD 함수 |
| `pilot/pkg/config/kube/crdclient/metrics.go` | CRD 이벤트 메트릭 |
| `pkg/kube/krt/core.go` | Collection/EventStream/Event 인터페이스 |
| `pkg/kube/krt/collection.go` | manyCollection 구현, 이벤트 처리 파이프라인 |
| `pkg/kube/krt/fetch.go` | Fetch/FetchOne/FetchOrList 함수 |
| `pkg/kube/krt/informer.go` | Kubernetes informer 래핑 (WrapClient) |
| `pkg/kube/krt/singleton.go` | Singleton, Static, NewStatic 구현 |
| `pkg/kube/krt/filter.go` | FilterKey/FilterLabel/FilterNamespace 등 |
| `pkg/kube/krt/index.go` | NewNamespaceIndex, 인덱스 생성 |
| `pkg/kube/krt/join.go` | 컬렉션 조인 |
| `pkg/kube/krt/debug.go` | 디버그 핸들러, CollectionDump |
| `pkg/kube/krt/README.md` | 설계 문서, 사용 예제 |

---

## 15. 관련 PoC

- **poc-crd-client**: CRD Config Store 시뮬레이션 (스키마 등록, 리비전 필터링, CRUD, 이벤트 핸들러)
- **poc-krt-framework**: krt 프레임워크 시뮬레이션 (Collection, Singleton, Fetch, 의존성 추적, 인덱스)
