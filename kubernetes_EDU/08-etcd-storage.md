# 08. etcd 스토리지 심화

## 목차

1. [개요](#1-개요)
2. [etcd3 store 구조체](#2-etcd3-store-구조체)
3. [Create 작업: OptimisticPut](#3-create-작업-optimisticput)
4. [Get 작업](#4-get-작업)
5. [GetList 작업](#5-getlist-작업)
6. [GuaranteedUpdate: Compare-and-Swap](#6-guaranteedupdate-compare-and-swap)
7. [Delete 작업](#7-delete-작업)
8. [Watch 메커니즘](#8-watch-메커니즘)
9. [Cacher 레이어](#9-cacher-레이어)
10. [Encryption at Rest](#10-encryption-at-rest)
11. [ResourceVersion과 일관성 모델](#11-resourceversion과-일관성-모델)
12. [왜 이런 설계인가](#12-왜-이런-설계인가)
13. [정리](#13-정리)

---

## 1. 개요

Kubernetes의 모든 클러스터 상태는 etcd에 저장된다. API Server는 etcd와 직접 통신하는
유일한 컴포넌트이며, `storage.Interface`를 통해 etcd 접근을 추상화한다.

### 스토리지 계층 구조

```
┌─────────────────────────────────────────────────────────┐
│                     API Server                           │
│                                                         │
│  ┌───────────────────────────────────────────────┐      │
│  │           REST Handler (CRUD)                 │      │
│  └──────────────────┬────────────────────────────┘      │
│                     │                                    │
│  ┌──────────────────▼────────────────────────────┐      │
│  │           registry (Store)                    │      │
│  │     genericregistry.Store                      │      │
│  └──────────────────┬────────────────────────────┘      │
│                     │                                    │
│  ┌──────────────────▼────────────────────────────┐      │
│  │           Cacher (캐시 계층)                   │      │
│  │     - watchCache (슬라이딩 윈도우)              │      │
│  │     - 인메모리 인덱스                           │      │
│  │     - Watch 분배                               │      │
│  └──────────────────┬────────────────────────────┘      │
│                     │                                    │
│  ┌──────────────────▼────────────────────────────┐      │
│  │           etcd3 store                         │      │
│  │     - Transformer (암호화/복호화)              │      │
│  │     - Codec (직렬화/역직렬화)                  │      │
│  │     - Versioner (ResourceVersion 관리)         │      │
│  └──────────────────┬────────────────────────────┘      │
│                     │                                    │
└─────────────────────┼────────────────────────────────────┘
                      │ gRPC
                      ▼
               ┌──────────────┐
               │  etcd 클러스터  │
               │  (Raft 합의)   │
               └──────────────┘
```

**핵심 소스 파일:**

```
staging/src/k8s.io/apiserver/pkg/storage/
├── interfaces.go              # storage.Interface 정의
├── etcd3/
│   ├── store.go               # etcd3 store 구현 (핵심)
│   └── watcher.go             # Watch 구현
├── cacher/
│   ├── cacher.go              # Cacher 구현
│   ├── watch_cache.go         # watchCache 구현
│   └── cache_watcher.go       # 개별 watcher 관리
└── value/
    └── transformer.go         # Encryption at Rest
```

---

## 2. etcd3 store 구조체

### 2.1 store 구조체 정의

**파일:** `staging/src/k8s.io/apiserver/pkg/storage/etcd3/store.go` (80행~98행)

```go
type store struct {
    client             *kubernetes.Client
    codec              runtime.Codec
    versioner          storage.Versioner
    transformer        value.Transformer
    pathPrefix         string
    groupResource      schema.GroupResource
    watcher            *watcher
    leaseManager       *leaseManager
    decoder            Decoder
    listErrAggrFactory func() ListErrorAggregator

    resourcePrefix string
    newListFunc    func() runtime.Object
    compactor      Compactor

    collectorMux          sync.RWMutex
    resourceSizeEstimator *resourceSizeEstimator
}

var _ storage.Interface = (*store)(nil)
```

### 2.2 핵심 필드 분석

| 필드 | 타입 | 역할 |
|------|------|------|
| `client` | `*kubernetes.Client` | etcd 클라이언트 (gRPC 연결) |
| `codec` | `runtime.Codec` | Go 객체 ↔ 바이트 변환 |
| `versioner` | `storage.Versioner` | ResourceVersion 관리 |
| `transformer` | `value.Transformer` | Encryption at Rest 처리 |
| `pathPrefix` | `string` | etcd 키 접두사 (기본: `/registry/`) |
| `groupResource` | `schema.GroupResource` | 리소스 그룹 (메트릭/로깅용) |
| `watcher` | `*watcher` | Watch 기능 제공 |
| `leaseManager` | `*leaseManager` | etcd Lease 관리 (TTL 리소스용) |
| `decoder` | `Decoder` | 바이트 → Go 객체 디코딩 |

### 2.3 store 생성

**파일:** `staging/src/k8s.io/apiserver/pkg/storage/etcd3/store.go` (148행~210행)

```go
func New(c *kubernetes.Client, compactor Compactor, codec runtime.Codec,
    newFunc, newListFunc func() runtime.Object,
    prefix, resourcePrefix string,
    groupResource schema.GroupResource,
    transformer value.Transformer,
    leaseManagerConfig LeaseManagerConfig,
    decoder Decoder,
    versioner storage.Versioner) (*store, error) {

    // pathPrefix 정규화: "/registry/"로 끝나도록
    pathPrefix := path.Join("/", prefix)
    if !strings.HasSuffix(pathPrefix, "/") {
        pathPrefix += "/"
    }

    // watcher 생성
    w := &watcher{
        client:        c.Client,
        codec:         codec,
        newFunc:       newFunc,
        groupResource: groupResource,
        versioner:     versioner,
        transformer:   transformer,
    }

    // store 생성
    s := &store{
        client:        c,
        codec:         codec,
        versioner:     versioner,
        transformer:   transformer,
        pathPrefix:    pathPrefix,
        groupResource: groupResource,
        watcher:       w,
        leaseManager:  newDefaultLeaseManager(c.Client, leaseManagerConfig),
        decoder:       decoder,
        ...
    }
    return s, nil
}
```

### 2.4 etcd 키 구조

```
/registry/                              ← pathPrefix
    pods/
        default/                        ← namespace
            nginx-abc123               ← name
            nginx-def456
        kube-system/
            kube-proxy-xyz
    deployments/
        default/
            nginx
    services/
        default/
            kubernetes
    configmaps/
        kube-system/
            kube-proxy
    secrets/
        default/
            my-secret                  ← 암호화되어 저장
```

---

## 3. Create 작업: OptimisticPut

### 3.1 Create 메서드

**파일:** `staging/src/k8s.io/apiserver/pkg/storage/etcd3/store.go` (274행~339행)

```go
func (s *store) Create(ctx context.Context, key string, obj, out runtime.Object,
    ttl uint64) error {

    preparedKey, err := s.prepareKey(key, false)
    if err != nil {
        return err
    }

    ctx, span := tracing.Start(ctx, "Create etcd3", ...)
    defer span.End(500 * time.Millisecond)

    // 1. ResourceVersion이 설정되어 있으면 에러
    //    Create에서는 RV를 지정할 수 없다
    if version, err := s.versioner.ObjectResourceVersion(obj); err == nil && version != 0 {
        return storage.ErrResourceVersionSetOnCreate
    }

    // 2. 저장을 위한 객체 준비 (RV를 0으로 초기화)
    if err := s.versioner.PrepareObjectForStorage(obj); err != nil {
        return fmt.Errorf("PrepareObjectForStorage failed: %v", err)
    }

    // 3. Go 객체 → 바이트 직렬화
    data, err := runtime.Encode(s.codec, obj)
    if err != nil {
        return err
    }

    // 4. TTL이 있으면 Lease 획득 (Events 등)
    var lease clientv3.LeaseID
    if ttl != 0 {
        lease, err = s.leaseManager.GetLease(ctx, int64(ttl))
        if err != nil {
            return err
        }
    }

    // 5. Encryption at Rest: 평문 → 암호문 변환
    newData, err := s.transformer.TransformToStorage(ctx, data,
        authenticatedDataString(preparedKey))
    if err != nil {
        return storage.NewInternalError(err)
    }

    // 6. 조건부 쓰기: 키가 없을 때만 생성 (OptimisticPut)
    startTime := time.Now()
    txnResp, err := s.client.Kubernetes.OptimisticPut(ctx, preparedKey, newData, 0,
        kubernetes.PutOptions{LeaseID: lease})
    metrics.RecordEtcdRequest("create", s.groupResource, err, startTime)
    if err != nil {
        return err
    }

    // 7. 이미 존재하면 에러
    if !txnResp.Succeeded {
        return storage.NewKeyExistsError(preparedKey, 0)
    }

    // 8. 결과 디코딩
    if out != nil {
        err = s.decoder.Decode(data, out, txnResp.Revision)
        if err != nil {
            return err
        }
    }
    return nil
}
```

### 3.2 OptimisticPut의 원리

`OptimisticPut`은 etcd의 트랜잭션(Txn) API를 활용한 조건부 쓰기다:

```
Create 시: OptimisticPut(key, value, expectedRevision=0)

etcd 트랜잭션:
  IF key의 ModRevision == 0 (키가 존재하지 않음)
  THEN PUT key = value
  ELSE 실패 → KeyExistsError
```

```
┌─────────────────────────────────────────┐
│           OptimisticPut 흐름             │
│                                         │
│  Client                    etcd         │
│    │                         │          │
│    │─── Txn Request ────────▶│          │
│    │   IF: key.revision == 0 │          │
│    │   THEN: PUT key=value   │          │
│    │                         │          │
│    │    [키가 없는 경우]       │          │
│    │◀── Succeeded: true ─────│          │
│    │   (Revision: 42)        │          │
│    │                         │          │
│    │    [키가 이미 있는 경우]   │          │
│    │◀── Succeeded: false ────│          │
│    │   → KeyExistsError      │          │
│    │                         │          │
└─────────────────────────────────────────┘
```

### 3.3 Create 데이터 변환 파이프라인

```
Go 객체 (Pod)
    │
    ├── 1. PrepareObjectForStorage()
    │      ResourceVersion = 0, SelfLink 제거
    │
    ├── 2. runtime.Encode(codec, obj)
    │      Go 객체 → JSON/Protobuf 바이트
    │
    ├── 3. transformer.TransformToStorage()
    │      평문 → 암호문 (EncryptionConfig 설정 시)
    │
    └── 4. OptimisticPut(key, encryptedData, revision=0)
           etcd에 조건부 저장
```

---

## 4. Get 작업

### 4.1 Get 메서드

**파일:** `staging/src/k8s.io/apiserver/pkg/storage/etcd3/store.go` (238행~271행)

```go
func (s *store) Get(ctx context.Context, key string, opts storage.GetOptions,
    out runtime.Object) error {

    preparedKey, err := s.prepareKey(key, false)
    if err != nil {
        return err
    }

    // 1. etcd에서 키 조회
    startTime := time.Now()
    getResp, err := s.client.Kubernetes.Get(ctx, preparedKey, kubernetes.GetOptions{})
    metrics.RecordEtcdRequest("get", s.groupResource, err, startTime)
    if err != nil {
        return err
    }

    // 2. ResourceVersion 최소값 검증
    if err = s.validateMinimumResourceVersion(opts.ResourceVersion,
        uint64(getResp.Revision)); err != nil {
        return err
    }

    // 3. 키가 없으면 IgnoreNotFound 옵션에 따라 처리
    if getResp.KV == nil {
        if opts.IgnoreNotFound {
            return runtime.SetZeroValue(out)
        }
        return storage.NewKeyNotFoundError(preparedKey, 0)
    }

    // 4. 암호문 → 평문 복호화
    data, _, err := s.transformer.TransformFromStorage(ctx,
        getResp.KV.Value, authenticatedDataString(preparedKey))
    if err != nil {
        return storage.NewInternalError(err)
    }

    // 5. 바이트 → Go 객체 디코딩
    err = s.decoder.Decode(data, out, getResp.KV.ModRevision)
    if err != nil {
        recordDecodeError(s.groupResource, preparedKey)
        return err
    }
    return nil
}
```

### 4.2 Get 데이터 역변환 파이프라인

```
etcd KV
    │
    ├── 1. getResp.KV.Value (암호화된 바이트)
    │
    ├── 2. transformer.TransformFromStorage()
    │      암호문 → 평문 (복호화)
    │
    ├── 3. decoder.Decode(data, out, ModRevision)
    │      바이트 → Go 객체 + ResourceVersion 설정
    │
    └── 4. Go 객체 반환 (ResourceVersion = etcd ModRevision)
```

---

## 5. GetList 작업

### 5.1 GetList 메서드

**파일:** `staging/src/k8s.io/apiserver/pkg/storage/etcd3/store.go` (736행~)

```go
func (s *store) GetList(ctx context.Context, key string,
    opts storage.ListOptions, listObj runtime.Object) error {

    keyPrefix, err := s.prepareKey(key, opts.Recursive)
    if err != nil {
        return err
    }

    // 1. 리스트 객체의 Items 슬라이스 준비
    listPtr, err := meta.GetItemsPtr(listObj)
    v, err := conversion.EnforcePtr(listPtr)

    // 2. 페이지네이션 설정
    limit := opts.Predicate.Limit
    paging := opts.Predicate.Limit > 0

    // 3. ResourceVersion 및 Continue 토큰 검증
    withRev, continueKey, err := storage.ValidateListOptions(keyPrefix, s.versioner, opts)

    // 4. 페이지 단위 반복 조회
    for {
        getResp, err = s.getList(ctx, keyPrefix, opts.Recursive,
            kubernetes.ListOptions{
                Revision: withRev,
                Limit:    limit,
                Continue: continueKey,
            })

        // 5. ResourceVersion 최소값 검증
        if err = s.validateMinimumResourceVersion(opts.ResourceVersion,
            uint64(getResp.Revision)); err != nil {
            return err
        }

        // 6. 일관된 스냅샷을 위해 첫 번째 응답의 Revision 고정
        if withRev == 0 {
            withRev = getResp.Revision
        }

        // 7. 각 KV 엔트리에 대해:
        for i, kv := range getResp.Kvs {
            // 페이지 크기 제한 확인
            if paging && int64(v.Len()) >= opts.Predicate.Limit {
                hasMore = true
                break
            }

            // 복호화
            data, _, err := s.transformer.TransformFromStorage(ctx,
                kv.Value, authenticatedDataString(kv.Key))

            // 디코딩
            obj, err := s.decoder.DecodeListItem(ctx, data, kv.ModRevision, newItemFunc)

            // Predicate 필터링 (label, field selector)
            if matched, err := opts.Predicate.Matches(obj); matched {
                v.Set(reflect.Append(v, reflect.ValueOf(obj).Elem()))
            }
        }

        // 8. 더 이상 결과가 없으면 종료
        if !hasMore || !paging {
            break
        }

        // 9. 다음 페이지 키 설정
        continueKey = string(lastKey) + "\x00"
        limit = maxLimit // 다음 페이지는 더 크게
    }

    // 10. ListMeta 설정 (ResourceVersion, Continue 토큰)
    return s.versioner.UpdateList(listObj, uint64(withRev), continueKey, ...)
}
```

### 5.2 페이지네이션 흐름

```
Client: GET /api/v1/pods?limit=100

┌────────────────────────────────────────────────────────────────┐
│                     GetList 페이지네이션                         │
│                                                                │
│  1차 요청:                                                      │
│    etcd Range(prefix="/registry/pods/", limit=100)             │
│    ← 100개 반환 + hasMore=true                                 │
│    ← Revision=1234 (이 시점의 스냅샷)                           │
│                                                                │
│  2차 요청:                                                      │
│    etcd Range(prefix="/registry/pods/", limit=10000,           │
│              continue=lastKey+"\x00", revision=1234)           │
│    ← 나머지 반환 + hasMore=false                                │
│    ← 동일한 Revision=1234에서 읽기 (일관성 보장)                 │
│                                                                │
│  응답:                                                          │
│    ListMeta.ResourceVersion = "1234"                           │
│    ListMeta.Continue = "" (모두 반환)                            │
│    Items = [pod1, pod2, ..., podN]                             │
└────────────────────────────────────────────────────────────────┘
```

### 5.3 maxLimit 상수

**파일:** `staging/src/k8s.io/apiserver/pkg/storage/etcd3/store.go` (58행~62행)

```go
const (
    // maxLimit is a maximum page limit increase used when fetching objects from etcd.
    maxLimit = 10000
)
```

첫 번째 페이지는 클라이언트의 `limit` 값을 사용하지만, 후속 페이지에서는 최대 10,000개까지
한 번에 가져온다. 이는 etcd 통신 왕복 횟수를 줄이기 위한 최적화다.

---

## 6. GuaranteedUpdate: Compare-and-Swap

### 6.1 GuaranteedUpdate 메서드

etcd의 MVCC(Multi-Version Concurrency Control)를 활용한 낙관적 동시성 제어의 핵심 구현이다.

**파일:** `staging/src/k8s.io/apiserver/pkg/storage/etcd3/store.go` (463행~)

```go
func (s *store) GuaranteedUpdate(
    ctx context.Context,
    key string,
    destination runtime.Object,
    ignoreNotFound bool,
    preconditions *storage.Preconditions,
    tryUpdate storage.UpdateFunc,
    cachedExistingObject runtime.Object) error {

    preparedKey, err := s.prepareKey(key, false)

    // 1. 현재 상태 조회 함수 준비
    getCurrentState := s.getCurrentState(ctx, preparedKey, v, ignoreNotFound, false)

    // 2. 캐시된 객체 또는 현재 상태 가져오기
    var origState *objState
    if cachedExistingObject != nil {
        origState, err = s.getStateFromObject(cachedExistingObject)
    } else {
        origState, err = getCurrentState()
        origStateIsCurrent = true
    }

    // 3. CAS 루프 시작
    for {
        // 3a. Precondition 검증 (UID 일치 등)
        if err := preconditions.Check(preparedKey, origState.obj); err != nil {
            if origStateIsCurrent {
                return err
            }
            // 캐시가 stale할 수 있으므로 재조회
            origState, err = getCurrentState()
            origStateIsCurrent = true
            continue  // 재시도
        }

        // 3b. tryUpdate 콜백으로 업데이트된 객체 생성
        ret, ttl, err := s.updateState(origState, tryUpdate)
        if err != nil {
            if origStateIsCurrent {
                return err
            }
            // stale 데이터로 인한 실패 시 재조회
            cachedRev := origState.rev
            origState, err = getCurrentState()
            origStateIsCurrent = true
            if cachedRev == origState.rev {
                return cachedUpdateErr  // 진짜 에러
            }
            continue  // 재시도
        }

        // 3c. 직렬화
        data, err := runtime.Encode(s.codec, ret)

        // 3d. 데이터가 변경되지 않았으면 쓰기 생략 (no-op 최적화)
        if !origState.stale && bytes.Equal(data, origState.data) {
            if !origStateIsCurrent {
                origState, err = getCurrentState()
                origStateIsCurrent = true
                if !bytes.Equal(data, origState.data) {
                    continue  // 데이터 변경됨, 재시도
                }
            }
            if !origState.stale {
                err = s.decoder.Decode(origState.data, destination, origState.rev)
                return nil  // 변경 없음, 기존 데이터 반환
            }
        }

        // 3e. 암호화
        newData, err := s.transformer.TransformToStorage(ctx, data, transformContext)

        // 3f. 조건부 쓰기: 현재 revision과 일치할 때만 업데이트
        txnResp, err := s.client.Kubernetes.OptimisticPut(ctx, preparedKey, newData,
            origState.rev,
            kubernetes.PutOptions{
                GetOnFailure: true,  // 실패 시 현재 값 반환
                LeaseID:      lease,
            })

        // 3g. 충돌 발생 시 재시도
        if !txnResp.Succeeded {
            klog.V(4).Infof("GuaranteedUpdate of %s failed because of a conflict, "+
                "going to retry", preparedKey)
            origState, err = s.getState(ctx, txnResp.KV, preparedKey, ...)
            origStateIsCurrent = true
            continue  // CAS 루프 재시도
        }

        // 3h. 성공: 결과 디코딩 및 반환
        err = s.decoder.Decode(data, destination, txnResp.Revision)
        return nil
    }
}
```

### 6.2 CAS(Compare-and-Swap) 흐름

```
┌─────────────────────────────────────────────────────────────────┐
│                   GuaranteedUpdate CAS 루프                      │
│                                                                 │
│  ┌─────────────────────────────────────────┐                    │
│  │ 1. 현재 상태 읽기                        │                    │
│  │    origState = Get(key)                  │                    │
│  │    origState.rev = 42                    │                    │
│  └─────────────┬───────────────────────────┘                    │
│                │                                                │
│                ▼                                                │
│  ┌─────────────────────────────────────────┐                    │
│  │ 2. tryUpdate 콜백 실행                   │                    │
│  │    newObj = tryUpdate(origState.obj)     │                    │
│  │    (클라이언트의 업데이트 로직)            │                    │
│  └─────────────┬───────────────────────────┘                    │
│                │                                                │
│                ▼                                                │
│  ┌─────────────────────────────────────────┐                    │
│  │ 3. 조건부 쓰기 시도                      │                    │
│  │    OptimisticPut(key, newData,           │                    │
│  │                  expectedRev=42)         │                    │
│  └─────────┬───────────┬───────────────────┘                    │
│            │           │                                        │
│     성공   │           │ 실패 (rev != 42)                        │
│            │           │                                        │
│            ▼           ▼                                        │
│     ┌──────────┐  ┌──────────────────────┐                      │
│     │ 반환     │  │ GetOnFailure=true     │                      │
│     │ (rev=43) │  │ 현재 값으로 origState │                      │
│     └──────────┘  │ 갱신 후 루프 재시작    │                      │
│                   └──────────────────────┘                      │
└─────────────────────────────────────────────────────────────────┘
```

### 6.3 no-op 최적화

`GuaranteedUpdate`의 중요한 최적화: 업데이트 후 데이터가 실제로 변경되지 않았으면
etcd에 쓰지 않는다. (553행~577행)

```go
// 데이터가 동일하면 쓰기 생략
if !origState.stale && bytes.Equal(data, origState.data) {
    // ...
    return nil  // etcd 쓰기 없이 반환
}
```

이 최적화가 중요한 이유:
- Status 업데이트에서 실제 변경이 없는 경우가 빈번
- etcd 부하 감소
- Watch 이벤트 불필요한 발생 방지

### 6.4 GetOnFailure 옵션

**파일:** `staging/src/k8s.io/apiserver/pkg/storage/etcd3/store.go` (597행~600행)

```go
txnResp, err := s.client.Kubernetes.OptimisticPut(ctx, preparedKey, newData,
    origState.rev, kubernetes.PutOptions{
        GetOnFailure: true,  // 실패 시 현재 값 반환
        LeaseID:      lease,
    })
```

`GetOnFailure: true`는 CAS 실패 시 etcd에서 현재 값을 함께 반환하도록 한다.
이를 통해 별도의 GET 요청 없이 바로 재시도할 수 있어 성능이 향상된다.

---

## 7. Delete 작업

### 7.1 Delete 메서드

**파일:** `staging/src/k8s.io/apiserver/pkg/storage/etcd3/store.go` (342행~359행)

```go
func (s *store) Delete(
    ctx context.Context, key string, out runtime.Object,
    preconditions *storage.Preconditions,
    validateDeletion storage.ValidateObjectFunc,
    cachedExistingObject runtime.Object,
    opts storage.DeleteOptions) error {

    preparedKey, err := s.prepareKey(key, false)

    v, err := conversion.EnforcePtr(out)

    return s.conditionalDelete(ctx, preparedKey, out, v, preconditions,
        validateDeletion, cachedExistingObject, skipTransformDecode)
}
```

### 7.2 conditionalDelete

**파일:** `staging/src/k8s.io/apiserver/pkg/storage/etcd3/store.go` (361행~)

```go
func (s *store) conditionalDelete(
    ctx context.Context, key string, out runtime.Object,
    v reflect.Value, preconditions *storage.Preconditions,
    validateDeletion storage.ValidateObjectFunc,
    cachedExistingObject runtime.Object,
    skipTransformDecode bool) error {

    // 1. 현재 상태 가져오기
    getCurrentState := s.getCurrentState(ctx, key, v, false, skipTransformDecode)
    origState, err := getCurrentState()

    for {
        // 2. Precondition 검증 (UID, ResourceVersion)
        if preconditions != nil {
            if err := preconditions.Check(key, origState.obj); err != nil {
                // stale 데이터 확인 및 재시도
                ...
            }
        }

        // 3. 삭제 유효성 검증 (Finalizer 등)
        if err := validateDeletion(ctx, origState.obj); err != nil {
            ...
        }

        // 4. 조건부 삭제: 현재 revision과 일치할 때만
        txnResp, err := s.client.Kubernetes.OptimisticDelete(ctx, key,
            kubernetes.DeleteOptions{ExpectedRevision: origState.rev})

        // 5. 충돌 시 재시도
        if !txnResp.Succeeded {
            origState, err = s.getState(ctx, txnResp.KV, key, ...)
            origStateIsCurrent = true
            continue
        }

        // 6. 성공: 삭제된 객체 반환
        return decode(...)
    }
}
```

Delete도 `OptimisticDelete`를 사용하여 CAS 방식으로 동작한다:

```
etcd 트랜잭션:
  IF key의 ModRevision == expectedRevision
  THEN DELETE key
  ELSE 실패 → 재시도
```

---

## 8. Watch 메커니즘

### 8.1 watcher 구조체

**파일:** `staging/src/k8s.io/apiserver/pkg/storage/etcd3/watcher.go` (72행~82행)

```go
type watcher struct {
    client                   *clientv3.Client
    codec                    runtime.Codec
    newFunc                  func() runtime.Object
    objectType               string
    groupResource            schema.GroupResource
    versioner                storage.Versioner
    transformer              value.Transformer
    getCurrentStorageRV      func(context.Context) (uint64, error)
    getResourceSizeEstimator func() *resourceSizeEstimator
}
```

### 8.2 watchChan 구조체

**파일:** `staging/src/k8s.io/apiserver/pkg/storage/etcd3/watcher.go` (85행~97행)

```go
type watchChan struct {
    watcher                  *watcher
    key                      string
    initialRev               int64
    recursive                bool
    progressNotify           bool
    internalPred             storage.SelectionPredicate
    ctx                      context.Context
    cancel                   context.CancelFunc
    incomingEventChan        chan *event       // 수신 버퍼: 100
    resultChan               chan watch.Event  // 송신 버퍼: 100
    getResourceSizeEstimator func() *resourceSizeEstimator
}
```

### 8.3 Watch 시작

**파일:** `staging/src/k8s.io/apiserver/pkg/storage/etcd3/watcher.go` (106행~128행)

```go
func (w *watcher) Watch(ctx context.Context, key string, rev int64,
    opts storage.ListOptions) (watch.Interface, error) {

    // recursive watch는 키가 "/"로 끝나야 함
    if opts.Recursive && !strings.HasSuffix(key, "/") {
        return nil, fmt.Errorf(`recursive key needs to end with "/"`)
    }

    // 시작 ResourceVersion 결정
    startWatchRV, err := w.getStartWatchResourceVersion(ctx, rev, opts)
    if err != nil {
        return nil, err
    }

    // watchChan 생성
    wc := w.createWatchChan(ctx, key, startWatchRV, opts.Recursive,
        opts.ProgressNotify, opts.Predicate)

    // 비동기 실행
    go wc.run(isInitialEventsEndBookmarkRequired(opts),
        areInitialEventsRequired(rev, opts))

    return wc, nil
}
```

### 8.4 버퍼 상수

**파일:** `staging/src/k8s.io/apiserver/pkg/storage/etcd3/watcher.go` (48행~53행)

```go
const (
    incomingBufSize         = 100  // etcd → watchChan 버퍼
    outgoingBufSize         = 100  // watchChan → 클라이언트 버퍼
    processEventConcurrency = 10   // 이벤트 처리 동시성
)
```

### 8.5 Watch 이벤트 흐름

```
┌─────────────────────────────────────────────────────────────────┐
│                    Watch 이벤트 흐름                              │
│                                                                 │
│  etcd Watch Stream                                              │
│       │                                                         │
│       │  etcd WatchResponse (이벤트 배치)                        │
│       ▼                                                         │
│  ┌─────────────────────────────┐                                │
│  │  incomingEventChan (100)    │  이벤트 수신 버퍼               │
│  └─────────────┬───────────────┘                                │
│                │                                                │
│                ▼                                                │
│  ┌─────────────────────────────┐                                │
│  │  이벤트 처리 (10 goroutine) │                                │
│  │  - TransformFromStorage()   │  복호화                        │
│  │  - Decode()                 │  역직렬화                      │
│  │  - Predicate.Matches()      │  Label/Field 필터              │
│  └─────────────┬───────────────┘                                │
│                │                                                │
│                ▼                                                │
│  ┌─────────────────────────────┐                                │
│  │  resultChan (100)           │  결과 송신 버퍼                 │
│  └─────────────┬───────────────┘                                │
│                │                                                │
│                ▼                                                │
│  ┌─────────────────────────────┐                                │
│  │  watch.Interface.ResultChan │  클라이언트가 소비              │
│  └─────────────────────────────┘                                │
└─────────────────────────────────────────────────────────────────┘
```

### 8.6 Store.Watch 메서드

**파일:** `staging/src/k8s.io/apiserver/pkg/storage/etcd3/store.go` (962행~972행)

```go
func (s *store) Watch(ctx context.Context, key string,
    opts storage.ListOptions) (watch.Interface, error) {

    preparedKey, err := s.prepareKey(key, opts.Recursive)
    if err != nil {
        return nil, err
    }
    rev, err := s.versioner.ParseResourceVersion(opts.ResourceVersion)
    if err != nil {
        return nil, err
    }
    return s.watcher.Watch(s.watchContext(ctx), preparedKey, int64(rev), opts)
}
```

### 8.7 watchContext: RequireLeader

**파일:** `staging/src/k8s.io/apiserver/pkg/storage/etcd3/store.go` (974행~983행)

```go
func (s *store) watchContext(ctx context.Context) context.Context {
    // etcd 리더가 없을 때 스트림 종료를 위해 RequireLeader 설정
    // etcd는 3 * electionTimeout (기본 3초) 후 리더 없음을 감지
    return clientv3.WithRequireLeader(ctx)
}
```

---

## 9. Cacher 레이어

### 9.1 Cacher 구조체

`Cacher`는 etcd3 store 위에 위치하는 캐싱 계층이다.
대부분의 GET/LIST 요청을 인메모리에서 처리하고, Watch 이벤트를 효율적으로 분배한다.

**파일:** `staging/src/k8s.io/apiserver/pkg/storage/cacher/cacher.go` (262행~343행)

```go
type Cacher struct {
    // 성능 디버깅용 High Water Mark
    incomingHWM storage.HighWaterMark
    // 이벤트 수신 채널
    incoming chan watchCacheEvent

    resourcePrefix string

    sync.RWMutex

    // 초기화 완료 여부 (초기화 중에는 요청 차단)
    ready *ready

    // 하위 etcd3 store
    storage storage.Interface

    // 캐시 대상 타입
    objectType reflect.Type
    groupResource schema.GroupResource

    // 슬라이딩 윈도우 캐시 + Reflector
    watchCache *watchCache
    reflector  *cache.Reflector

    // Versioner
    versioner storage.Versioner

    // 새 객체 생성 함수
    newFunc     func() runtime.Object
    newListFunc func() runtime.Object

    // Watcher 인덱싱
    indexedTrigger *indexedTriggerFunc
    watcherIdx     int
    watchers       indexedWatchers

    // 이벤트 디스패치 타임아웃
    dispatchTimeoutBudget timeBudget

    // 종료 관리
    stopLock sync.RWMutex
    stopped  bool
    stopCh   chan struct{}
    stopWg   sync.WaitGroup

    clock clock.Clock
    timer *time.Timer

    // 이벤트 디스패치 상태
    dispatching     bool
    watchersBuffer  []*cacheWatcher
    blockedWatchers []*cacheWatcher
    watchersToStop  []*cacheWatcher

    // 북마크 관리
    bookmarkWatchers        *watcherBookmarkTimeBuckets
    expiredBookmarkWatchers []*cacheWatcher
    compactor               *compactor
}
```

### 9.2 Cacher Config

**파일:** `staging/src/k8s.io/apiserver/pkg/storage/cacher/cacher.go` (80행~120행)

```go
type Config struct {
    // 하위 storage.Interface (etcd3 store)
    Storage storage.Interface

    // Versioner
    Versioner storage.Versioner

    // 리소스 그룹 (로깅, 메트릭용)
    GroupResource schema.GroupResource

    // 이벤트 히스토리 윈도우 (최소 DefaultEventFreshDuration)
    EventsHistoryWindow time.Duration

    // etcd 키 접두사
    ResourcePrefix string

    // 키 생성 함수
    KeyFunc func(runtime.Object) (string, error)

    // Label/Field 추출 함수
    GetAttrsFunc func(runtime.Object) (label labels.Set, field fields.Set, err error)

    // Watcher 인덱싱 함수
    IndexerFuncs storage.IndexerFuncs
    Indexers     *cache.Indexers

    // 새 객체/리스트 생성 함수
    NewFunc     func() runtime.Object
    NewListFunc func() runtime.Object
    Codec       runtime.Codec
}
```

### 9.3 Cacher의 동작 원리

```
┌─────────────────────────────────────────────────────────────────┐
│                          Cacher                                  │
│                                                                 │
│  ┌──────────────────────────────────────────┐                   │
│  │              Reflector                    │                   │
│  │  etcd Watch → incoming chan → watchCache  │                   │
│  │  (백그라운드 goroutine)                    │                   │
│  └────────────────────┬─────────────────────┘                   │
│                       │                                         │
│                       ▼                                         │
│  ┌──────────────────────────────────────────┐                   │
│  │            watchCache                     │                   │
│  │  ┌────────────────────────────────────┐  │                   │
│  │  │  store (BTree 인덱스)               │  │                   │
│  │  │  - 모든 객체의 현재 상태             │  │                   │
│  │  │  - namespace, name 인덱스          │  │                   │
│  │  └────────────────────────────────────┘  │                   │
│  │  ┌────────────────────────────────────┐  │                   │
│  │  │  cyclic buffer (이벤트 히스토리)     │  │                   │
│  │  │  - 최근 N분간의 변경 이벤트          │  │                   │
│  │  │  - Watch 재시작 시 히스토리 제공     │  │                   │
│  │  └────────────────────────────────────┘  │                   │
│  └──────────────────────────────────────────┘                   │
│                                                                 │
│  ┌──────────────────────────────────────────┐                   │
│  │           Watcher 분배                    │                   │
│  │  ┌──────┐ ┌──────┐ ┌──────┐ ┌──────┐   │                   │
│  │  │ W1   │ │ W2   │ │ W3   │ │ W4   │   │                   │
│  │  │(pods)│ │(pods)│ │(svcs)│ │(pods)│   │                   │
│  │  │ ns=a │ │ ns=b │ │ all  │ │ all  │   │                   │
│  │  └──────┘ └──────┘ └──────┘ └──────┘   │                   │
│  └──────────────────────────────────────────┘                   │
└─────────────────────────────────────────────────────────────────┘
```

### 9.4 Cacher.Get / Cacher.GetList

**파일:** `staging/src/k8s.io/apiserver/pkg/storage/cacher/cacher.go`

| 메서드 | 행 | 동작 |
|--------|-----|------|
| `Get` | 679행 | watchCache의 store에서 인메모리 조회 |
| `GetList` | 734행 | watchCache의 store에서 인메모리 목록 조회 |
| `Watch` | 508행 | cacheWatcher 생성 후 이벤트 분배 |

Cacher를 통한 Get/List는 etcd에 직접 요청하지 않고 인메모리에서 처리한다.
단, `ResourceVersion="0"` 또는 `ResourceVersionMatch=NotOlderThan`인 경우에만
캐시에서 서빙하고, `ResourceVersion=""`(최신)인 경우 etcd에 직접 요청한다.

### 9.5 DefaultEventFreshDuration

**파일:** `staging/src/k8s.io/apiserver/pkg/storage/cacher/cacher.go` (68행~73행)

```go
const (
    // DefaultEventFreshDuration is the default time duration of events we want to keep.
    DefaultEventFreshDuration = defaultBookmarkFrequency + 15*time.Second
    // defaultBookmarkFrequency defines how frequently watch bookmarks should be sent
    defaultBookmarkFrequency = time.Minute
)
```

이벤트 히스토리는 기본 75초(1분 + 15초) 동안 유지된다.
이 기간 내에 Watch가 재시작되면 히스토리에서 놓친 이벤트를 복구할 수 있다.

---

## 10. Encryption at Rest

### 10.1 Transformer 인터페이스

**파일:** `staging/src/k8s.io/apiserver/pkg/storage/value/transformer.go` (36행~62행)

```go
// Context is additional information that a storage transformation may need
type Context interface {
    AuthenticatedData() []byte
}

// Read transforms data from storage
type Read interface {
    TransformFromStorage(ctx context.Context, data []byte, dataCtx Context) (
        out []byte, stale bool, err error)
}

// Write transforms data to storage
type Write interface {
    TransformToStorage(ctx context.Context, data []byte, dataCtx Context) (
        out []byte, err error)
}

// Transformer allows a value to be transformed before being read from or
// written to the underlying store.
type Transformer interface {
    Read
    Write
}
```

### 10.2 authenticatedDataString

**파일:** `staging/src/k8s.io/apiserver/pkg/storage/etcd3/store.go` (71행~78행)

```go
type authenticatedDataString string

func (d authenticatedDataString) AuthenticatedData() []byte {
    return []byte(string(d))
}
```

etcd 키 자체를 인증 데이터로 사용한다. 이를 통해:
- 다른 키의 암호화된 값을 복사해서 사용할 수 없음
- 키와 값의 무결성이 보장됨

### 10.3 PrefixTransformers

**파일:** `staging/src/k8s.io/apiserver/pkg/storage/value/transformer.go` (76행~100행)

```go
type PrefixTransformer struct {
    Prefix      []byte
    Transformer Transformer
}

// NewPrefixTransformers supports the Transformer interface by checking
// the incoming data against the provided prefixes in order.
func NewPrefixTransformers(err error, transformers ...PrefixTransformer) Transformer {
    return &prefixTransformers{
        transformers: transformers,
        err:          err,
    }
}
```

### 10.4 Encryption 흐름

```
┌─────────────────────────────────────────────────────────────────┐
│                 Encryption at Rest 데이터 흐름                    │
│                                                                 │
│  쓰기 (Create/Update):                                          │
│                                                                 │
│  Go 객체 → Encode → 평문 바이트                                  │
│                        │                                        │
│                        ▼                                        │
│            TransformToStorage()                                  │
│                        │                                        │
│                ┌───────┴────────┐                                │
│                │ PrefixTransform│                                │
│                │ Prefix: "k8s:" │                                │
│                │ ┌────────────┐ │                                │
│                │ │ AES-CBC    │ │                                │
│                │ │ AES-GCM    │ │                                │
│                │ │ Secretbox  │ │                                │
│                │ │ KMS v2     │ │                                │
│                │ │ Identity   │ │ ← 암호화 안 함 (기본)           │
│                │ └────────────┘ │                                │
│                └───────┬────────┘                                │
│                        │                                        │
│                        ▼                                        │
│            "k8s:enc:aescbc:v1:" + 암호문                        │
│                        │                                        │
│                        ▼                                        │
│            etcd에 저장                                           │
│                                                                 │
│  읽기 (Get/List):                                                │
│                                                                 │
│  etcd에서 읽기                                                   │
│        │                                                        │
│        ▼                                                        │
│  "k8s:enc:aescbc:v1:" + 암호문                                  │
│        │                                                        │
│        ▼                                                        │
│  TransformFromStorage()                                          │
│  - Prefix 매칭하여 적절한 Transformer 선택                        │
│  - 복호화                                                        │
│        │                                                        │
│        ▼                                                        │
│  평문 바이트 → Decode → Go 객체                                   │
└─────────────────────────────────────────────────────────────────┘
```

### 10.5 EncryptionConfiguration 예시

```yaml
apiVersion: apiserver.config.k8s.io/v1
kind: EncryptionConfiguration
resources:
  - resources:
      - secrets
      - configmaps
    providers:
      - aescbc:
          keys:
            - name: key1
              secret: <base64-encoded-key>
      - identity: {}  # 복호화 폴백 (암호화 안 된 기존 데이터)
```

### 10.6 stale 플래그

`TransformFromStorage`가 `stale=true`를 반환하면, 이 데이터는 현재 암호화 방식이 아닌
이전 방식으로 암호화되어 있다는 의미다. `GuaranteedUpdate`에서 이를 감지하면,
데이터가 변경되지 않았더라도 새 암호화 방식으로 재암호화하여 저장한다.

이 메커니즘은 암호화 키 로테이션을 가능하게 한다:

1. 새 키를 EncryptionConfiguration에 추가
2. API Server 재시작
3. 기존 데이터 읽기 시 `stale=true` 반환
4. 다음 업데이트 시 자동으로 새 키로 재암호화

---

## 11. ResourceVersion과 일관성 모델

### 11.1 ResourceVersion이란

Kubernetes의 `ResourceVersion`은 etcd의 `ModRevision`(수정 리비전)을 직접 매핑한다.

| etcd 개념 | Kubernetes 개념 | 의미 |
|-----------|-----------------|------|
| `Revision` | 글로벌 RV | 클러스터 전체의 변경 순서 번호 |
| `ModRevision` | 객체의 RV | 해당 키가 마지막으로 수정된 Revision |
| `CreateRevision` | - | 해당 키가 생성된 Revision |

### 11.2 ResourceVersion의 의미론

```
시간 →

Revision:  1    2    3    4    5    6    7    8    9
          ─┼────┼────┼────┼────┼────┼────┼────┼────┼─
          Pod A  Pod B  Pod A  Pod C  Pod B  Pod A
          생성   생성   수정   생성   수정   삭제

Pod A의 ModRevision 변화:
  생성 시: ModRevision=1
  수정 후: ModRevision=3
  삭제:    (키 삭제됨)

Pod B의 ModRevision 변화:
  생성 시: ModRevision=2
  수정 후: ModRevision=5
```

### 11.3 List에서의 일관성

GetList에서 일관된 스냅샷을 보장하는 방법:

```go
// store.go 809행~811행
// 첫 번째 페이지 응답의 Revision을 고정
if withRev == 0 {
    withRev = getResp.Revision
}
```

첫 번째 etcd 요청의 응답에서 Revision을 가져오고, 이후 페이지 요청에서
동일한 Revision을 사용한다. etcd의 MVCC 덕분에 과거 시점의 데이터를 읽을 수 있다.

### 11.4 List 옵션별 동작

| ResourceVersion | ResourceVersionMatch | 동작 |
|-----------------|---------------------|------|
| `""` (빈 문자열) | - | etcd에서 최신 데이터 조회 (Quorum Read) |
| `"0"` | - | Cacher에서 캐시된 데이터 반환 (가장 빠름) |
| `"12345"` | `NotOlderThan` | RV >= 12345인 시점의 데이터 |
| `"12345"` | `Exact` | 정확히 RV=12345 시점의 데이터 |

### 11.5 Watch에서의 ResourceVersion

```
Watch(resourceVersion="100") 요청 시:

etcd 이벤트 스트림:
  Rev 98:  Pod A 생성
  Rev 99:  Pod B 생성
  Rev 100: Pod A 수정  ← 시작 지점
  Rev 101: Pod C 생성  ← 이벤트 수신 시작
  Rev 102: Pod B 삭제  ← 이벤트 수신
  ...
```

### 11.6 validateMinimumResourceVersion

**파일:** `staging/src/k8s.io/apiserver/pkg/storage/etcd3/store.go`

Get/List에서 클라이언트가 제공한 `ResourceVersion`과 etcd 응답의 Revision을 비교한다:

```go
func (s *store) validateMinimumResourceVersion(
    requestedRV string, actualRV uint64) error {
    // requestedRV > actualRV이면 TooLargeResourceVersion 에러 반환
    // → 클라이언트가 미래의 RV를 요청한 경우
}
```

### 11.7 objState 구조체

**파일:** `staging/src/k8s.io/apiserver/pkg/storage/etcd3/store.go` (108행~114행)

```go
type objState struct {
    obj   runtime.Object        // 디코딩된 Go 객체
    meta  *storage.ResponseMeta // 메타 정보
    rev   int64                 // etcd ModRevision
    data  []byte                // 인코딩된 바이트 (no-op 비교용)
    stale bool                  // 암호화 키 로테이션 필요 여부
}
```

`objState`는 `GuaranteedUpdate`와 `conditionalDelete`에서 CAS 루프의 상태를 추적한다.

---

## 12. 왜 이런 설계인가

### 12.1 왜 etcd인가?

| 특성 | etcd가 적합한 이유 |
|------|-------------------|
| **강한 일관성** | Raft 합의로 모든 노드에서 동일한 데이터 보장 |
| **Watch 지원** | 변경 이벤트를 실시간으로 스트리밍 |
| **MVCC** | 과거 시점 데이터 읽기, 낙관적 동시성 제어 |
| **키-값 모델** | Kubernetes 리소스의 자연스러운 매핑 |
| **Lease/TTL** | Events 같은 임시 리소스의 자동 만료 |

### 12.2 왜 OptimisticPut(CAS)인가?

**비관적 잠금(Pessimistic Locking) 대비 장점:**

1. **높은 동시성**: 잠금 대기 없이 동시 요청 처리
2. **데드락 없음**: 잠금이 없으므로 데드락 불가능
3. **간단한 구현**: etcd Txn API로 원자적 비교-교체
4. **충돌 비율이 낮은 환경에 최적**: Kubernetes에서 동일 객체를 동시에 수정하는 빈도가 낮음

**충돌 발생 시 흐름:**

```
Controller A: GET pod (rv=42)
Controller B: GET pod (rv=42)
Controller A: PUT pod (expected rv=42) → 성공 (rv=43)
Controller B: PUT pod (expected rv=42) → 실패 (Conflict 409)
Controller B: GET pod (rv=43) → 재시도
Controller B: PUT pod (expected rv=43) → 성공 (rv=44)
```

### 12.3 왜 Cacher 레이어인가?

| 문제 | Cacher의 해결 |
|------|-------------|
| etcd 부하 | GET/LIST를 인메모리에서 처리하여 etcd 읽기 부하 제거 |
| Watch 효율 | 수천 개의 Watch를 하나의 etcd Watch로 팬아웃 |
| 레이턴시 | 인메모리 조회로 ms 단위 응답 |
| 히스토리 | 슬라이딩 윈도우로 Watch 재시작 시 이벤트 복구 |

Cacher가 없다면 1,000개의 Watch 클라이언트가 동일 리소스를 감시할 때
etcd에 1,000개의 Watch 스트림이 열리지만, Cacher를 사용하면 1개의
etcd Watch로 1,000개의 클라이언트에 이벤트를 분배한다.

### 12.4 왜 Encryption at Rest인가?

1. **규정 준수**: PCI-DSS, HIPAA 등 보안 규정 요구사항
2. **심층 방어**: etcd 디스크 접근 시에도 데이터 보호
3. **키 로테이션**: stale 플래그로 점진적 재암호화 지원
4. **Provider 다양성**: AES-CBC, AES-GCM, KMS v2 등 선택 가능

### 12.5 왜 storage.Interface 추상화인가?

```go
// storage.Interface는 스토리지 백엔드를 추상화
type Interface interface {
    Versioner() Versioner
    Create(ctx, key, obj, out, ttl) error
    Delete(ctx, key, out, preconditions, validateDeletion, ...) error
    Watch(ctx, key, opts) (watch.Interface, error)
    Get(ctx, key, opts, out) error
    GetList(ctx, key, opts, listObj) error
    GuaranteedUpdate(ctx, key, dest, ignoreNotFound, preconditions, tryUpdate, ...) error
    Count(key) (int64, error)
}
```

이 추상화를 통해:
- 테스트에서 인메모리 스토리지 사용 가능
- Cacher가 투명하게 etcd3 store를 래핑
- 향후 다른 스토리지 백엔드 추가 가능 (이론적)

---

## 13. 정리

### 핵심 작업 요약

| 작업 | etcd API | 특징 |
|------|---------|------|
| Create | `OptimisticPut(rev=0)` | 키 없을 때만 생성 |
| Get | `Get` | 단일 키 조회, 복호화, 디코딩 |
| GetList | `Range` (페이지) | 프리픽스 범위 조회, 일관된 Revision |
| Update | `OptimisticPut(rev=N)` | CAS 루프, no-op 최적화 |
| Delete | `OptimisticDelete(rev=N)` | CAS 방식 조건부 삭제 |
| Watch | `Watch` | 이벤트 스트리밍, 필터링 |

### 데이터 변환 파이프라인

```
Go 객체
    │
    ▼
runtime.Encode (codec)
    │ Go 객체 → JSON/Protobuf
    ▼
transformer.TransformToStorage
    │ 평문 → 암호문
    ▼
etcd PUT
    │
    ▼
etcd 저장 (암호화된 바이트)

etcd GET
    │
    ▼
transformer.TransformFromStorage
    │ 암호문 → 평문
    ▼
decoder.Decode
    │ JSON/Protobuf → Go 객체 + ResourceVersion
    ▼
Go 객체
```

### 핵심 파일 참조

| 파일 | 행 | 내용 |
|------|-----|------|
| `etcd3/store.go` | 80~98 | store 구조체 정의 |
| `etcd3/store.go` | 148~210 | New() 생성자 |
| `etcd3/store.go` | 238~271 | Get 메서드 |
| `etcd3/store.go` | 274~339 | Create 메서드 |
| `etcd3/store.go` | 342~359 | Delete 메서드 |
| `etcd3/store.go` | 463~612 | GuaranteedUpdate 메서드 |
| `etcd3/store.go` | 736~ | GetList 메서드 |
| `etcd3/store.go` | 962~972 | Watch 메서드 |
| `etcd3/watcher.go` | 72~82 | watcher 구조체 |
| `etcd3/watcher.go` | 85~97 | watchChan 구조체 |
| `etcd3/watcher.go` | 106~128 | Watch 시작 |
| `cacher/cacher.go` | 80~120 | Cacher Config |
| `cacher/cacher.go` | 262~343 | Cacher 구조체 |
| `value/transformer.go` | 36~62 | Transformer 인터페이스 |
| `value/transformer.go` | 76~100 | PrefixTransformers |
