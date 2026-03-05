# 17. Argo CD 캐싱 및 성능 최적화

## 개요

Argo CD는 수천 개의 애플리케이션과 수백 개의 클러스터를 관리하는 대규모 환경에서도 안정적으로 동작해야 한다. 이를 위해 여러 계층의 캐싱 전략과 병렬 처리 제어, 샤딩(Sharding) 기반의 수평 확장 메커니즘을 갖추고 있다. 이 문서는 Argo CD의 캐시 아키텍처를 레이어별로 분해하고, 각 설계 결정의 "왜(Why)"를 소스코드 수준에서 검증한다.

---

## 1. 캐시 아키텍처 개요

Argo CD의 캐시는 세 가지 독립적인 레이어로 구성된다.

```
┌─────────────────────────────────────────────────────────────────────┐
│                        Argo CD 캐시 레이어                           │
├─────────────────────────────────────────────────────────────────────┤
│                                                                     │
│  ┌─────────────────────────────────┐                               │
│  │    Redis (외부 공유 캐시)          │  ← 모든 replica 가 공유        │
│  │  - 매니페스트 캐시                 │                               │
│  │  - 앱 상태 캐시 (TTL: 1h)         │                               │
│  │  - 클러스터 정보 캐시 (TTL: 10m)  │                               │
│  │  - 리소스 트리 캐시               │                               │
│  └──────────────┬──────────────────┘                               │
│                 │ TwoLevelClient 패턴                               │
│  ┌──────────────▼──────────────────┐                               │
│  │    In-Memory 캐시 (로컬)         │  ← Redis 트래픽 최소화          │
│  │  - gob 인코딩, go-cache 기반     │                               │
│  │  - 동일 값이면 Redis SET 스킵     │                               │
│  └─────────────────────────────────┘                               │
│                                                                     │
│  ┌─────────────────────────────────┐                               │
│  │    ClusterCache (In-Memory)      │  ← K8s Watch API 기반         │
│  │  - clusterCache struct           │                               │
│  │  - parentUIDToChildren 인덱스    │                               │
│  │  - 실시간 Watch 업데이트          │                               │
│  └─────────────────────────────────┘                               │
│                                                                     │
│  ┌─────────────────────────────────┐                               │
│  │    Enforcer Cache (RBAC)         │  ← casbin enforcer 캐싱       │
│  │  - gocache.New(time.Hour, ...)  │                               │
│  │  - 프로젝트별 캐싱               │                               │
│  └─────────────────────────────────┘                               │
└─────────────────────────────────────────────────────────────────────┘
```

각 레이어는 서로 다른 문제를 해결한다.

| 레이어 | 위치 | TTL | 목적 |
|--------|------|-----|------|
| Redis | 외부 (공유) | 1h (앱 상태), 10m (클러스터 정보), 24h (기본) | 다중 replica 간 캐시 공유 |
| In-Memory (TwoLevel) | 각 프로세스 로컬 | Redis 설정을 따름 | Redis 왕복 트래픽 최소화 |
| ClusterCache | controller 프로세스 | 24h 주기 전체 재동기화 | K8s API 서버 부하 감소 |
| Enforcer Cache | API 서버 프로세스 | 1h | casbin 정책 평가 비용 절감 |

---

## 2. Redis 캐시 레이어

### 2.1 Cache struct 구조

**파일:** `util/cache/appstate/cache.go`

```go
type Cache struct {
    Cache                   *cacheutil.Cache
    appStateCacheExpiration time.Duration
}

func NewCache(cache *cacheutil.Cache, appStateCacheExpiration time.Duration) *Cache {
    return &Cache{cache, appStateCacheExpiration}
}
```

`appStateCacheExpiration`의 기본값은 1시간이며, 환경 변수 `ARGOCD_APP_STATE_CACHE_EXPIRATION`으로 재정의할 수 있다.

```go
func AddCacheFlagsToCmd(cmd *cobra.Command, opts ...cacheutil.Options) func() (*Cache, error) {
    var appStateCacheExpiration time.Duration

    cmd.Flags().DurationVar(
        &appStateCacheExpiration,
        "app-state-cache-expiration",
        env.ParseDurationFromEnv("ARGOCD_APP_STATE_CACHE_EXPIRATION", 1*time.Hour, 0, 10*time.Hour),
        "Cache expiration for app state",
    )
    // ...
}
```

### 2.2 캐시 대상별 키 체계

Redis에 저장되는 데이터와 키 패턴은 다음과 같다.

```
app|managed-resources|{appName}         → GetAppManagedResources
app|resources-tree|{appName}            → GetAppResourcesTree (샤드 0)
app|resources-tree|{appName}|{shardN}   → GetAppResourcesTree (샤드 N)
cluster|info|{serverURL}                → GetClusterInfo
```

**관리 리소스 캐시:**

```go
func appManagedResourcesKey(appName string) string {
    return "app|managed-resources|" + appName
}

func (c *Cache) SetAppManagedResources(appName string, managedResources []*appv1.ResourceDiff) error {
    // 정렬 후 저장 → 결정론적 순서로 비교 가능
    sort.Slice(managedResources, func(i, j int) bool {
        return managedResources[i].FullName() < managedResources[j].FullName()
    })
    return c.SetItem(appManagedResourcesKey(appName), managedResources,
        c.appStateCacheExpiration, managedResources == nil)
}
```

**클러스터 정보 캐시:**

```go
const clusterInfoCacheExpiration = 10 * time.Minute

func (c *Cache) SetClusterInfo(server string, info *appv1.ClusterInfo) error {
    return c.SetItem(clusterInfoKey(server), info, clusterInfoCacheExpiration, info == nil)
}
```

클러스터 정보는 앱 상태보다 훨씬 짧은 10분 TTL을 사용한다. 클러스터 연결 상태, K8s 버전, 리소스 수 등은 자주 변하므로 오래된 정보를 오래 유지하면 안 된다.

### 2.3 리소스 트리 샤딩

대규모 애플리케이션은 리소스 트리가 매우 커질 수 있다. Argo CD는 트리를 샤드로 분할해 Redis 트래픽을 줄인다.

```go
func (c *Cache) SetAppResourcesTree(appName string, resourcesTree *appv1.ApplicationTree) error {
    if resourcesTree == nil {
        if err := c.SetItem(appResourcesTreeKey(appName, 0), resourcesTree,
            c.appStateCacheExpiration, true); err != nil {
            return err
        }
    } else {
        // 트리를 샤드로 분할 → Redis SET 호출 최소화
        // TwoLevelClient가 값이 동일한 샤드는 Redis 전송 스킵
        for i, shard := range resourcesTree.GetShards(treeShardSize) {
            if err := c.SetItem(appResourcesTreeKey(appName, int64(i)), shard,
                c.appStateCacheExpiration, false); err != nil {
                return err
            }
        }
    }
    return c.Cache.NotifyUpdated(appManagedResourcesKey(appName))
}
```

코드 주석이 명확히 설명한다: *"Splitting resource tree into shards reduces number of Redis SET calls and therefore amount of traffic sent from controller to Redis. Controller still stores each shard in cache but util/cache/twolevelclient.go forwards request to Redis only if shard actually changes."*

### 2.4 TwoLevelClient 패턴

**파일:** `util/cache/twolevelclient.go`

Redis 트래픽을 최소화하는 핵심 메커니즘이다.

```go
type twoLevelClient struct {
    inMemoryCache *InMemoryCache
    externalCache CacheClient  // Redis
}

// Set: 동일 값이면 Redis 전송 스킵
func (c *twoLevelClient) Set(item *Item) error {
    has, err := c.inMemoryCache.HasSame(item.Key, item.Object)
    if has {
        return nil  // 이미 같은 값 → Redis 불필요
    }
    // ...
    err = c.inMemoryCache.Set(item)
    return c.externalCache.Set(item)  // Redis에도 저장
}

// Get: 메모리 먼저, 없으면 Redis
func (c *twoLevelClient) Get(key string, obj any) error {
    err := c.inMemoryCache.Get(key, obj)
    if err == nil {
        return nil  // 메모리 히트
    }
    err = c.externalCache.Get(key, obj)
    if err == nil {
        _ = c.inMemoryCache.Set(&Item{Key: key, Object: obj})  // 메모리에 캐싱
    }
    return err
}
```

**왜 이 패턴인가?**

Argo CD 컨트롤러는 매 reconcile 주기마다 앱 리소스 트리를 업데이트한다. 대부분의 경우 리소스는 변경되지 않는다. TwoLevelClient는 로컬 메모리에서 값의 동일성을 먼저 확인해, 변경이 없으면 Redis에 불필요한 SET 요청을 보내지 않는다. 컨트롤러가 수백 개의 앱을 관리하는 경우 이 최적화는 Redis 트래픽을 크게 줄인다.

### 2.5 In-Memory Cache 구현

**파일:** `util/cache/inmemory.go`

```go
func NewInMemoryCache(expiration time.Duration) *InMemoryCache {
    return &InMemoryCache{
        memCache: gocache.New(expiration, 1*time.Minute),
    }
}

// gob 인코딩으로 직렬화 → 정확한 바이트 비교로 동일성 검사
func (i *InMemoryCache) HasSame(key string, obj any) (bool, error) {
    var buf bytes.Buffer
    err := gob.NewEncoder(&buf).Encode(obj)
    if err != nil {
        return false, err
    }

    bufIf, found := i.memCache.Get(key)
    if !found {
        return false, nil
    }
    existingBuf, ok := bufIf.(bytes.Buffer)
    if !ok {
        panic(fmt.Errorf("InMemoryCache has unexpected entry: %v", existingBuf))
    }
    return bytes.Equal(buf.Bytes(), existingBuf.Bytes()), nil
}
```

---

## 3. ClusterCache - K8s 리소스 인메모리 캐시

### 3.1 ClusterCache 인터페이스

**파일:** `gitops-engine/pkg/cache/cluster.go`

```go
type ClusterCache interface {
    // K8s Watch API로 캐시 동기화 (필요시)
    EnsureSynced() error
    // API 리소스 정보 반환
    GetServerVersion() string
    GetAPIResources() []kube.APIResourceInfo
    // predicate 기반 리소스 검색
    FindResources(namespace string, predicates ...func(r *Resource) bool) map[kube.ResourceKey]*Resource
    // 부모-자식 트리 순회 (V2)
    IterateHierarchyV2(keys []kube.ResourceKey,
        action func(resource *Resource, namespaceResources map[kube.ResourceKey]*Resource) bool)
    // 관리 대상 live 객체 조회
    GetManagedLiveObjs(targetObjs []*unstructured.Unstructured,
        isManaged func(r *Resource) bool) (map[kube.ResourceKey]*unstructured.Unstructured, error)
    // 캐시 통계
    GetClusterInfo() ClusterInfo
    // 이벤트 핸들러 등록
    OnResourceUpdated(handler OnResourceUpdatedHandler) Unsubscribe
    OnEvent(handler OnEventHandler) Unsubscribe
    OnProcessEventsHandler(handler OnProcessEventsHandler) Unsubscribe
    // ...
}
```

### 3.2 clusterCache 내부 구조

```go
type clusterCache struct {
    syncStatus clusterCacheSync  // 동기화 상태 관리

    apisMeta    map[schema.GroupKind]*apiMeta  // API 메타데이터
    serverVersion string
    apiResources  []kube.APIResourceInfo

    watchResyncTimeout      time.Duration  // 기본값: 10분
    clusterSyncRetryTimeout time.Duration  // 기본값: 10초
    eventProcessingInterval time.Duration  // 기본값: 100ms

    listPageSize       int64  // 기본값: 500 (k8s pager와 동일)
    listPageBufferSize int32  // 기본값: 1 (단일 페이지 프리페치)
    listSemaphore      WeightedSemaphore  // 동시 List 작업 제한 (기본: 50)

    lock      sync.RWMutex
    resources map[kube.ResourceKey]*Resource   // 전체 리소스 인덱스
    nsIndex   map[string]map[kube.ResourceKey]*Resource  // 네임스페이스별 인덱스

    // 부모-자식 관계 인덱스: O(1) 트리 순회
    parentUIDToChildren map[types.UID][]kube.ResourceKey
}
```

### 3.3 parentUIDToChildren 인덱스

패키지 doc.go의 설명:

```
// The parentUIDToChildren index enables efficient O(1) cross-namespace traversal
// by mapping any resource's UID to its direct children, eliminating the need
// for O(n) graph building.
```

```
                 ClusterScoped Resource (UID: "abc")
                          │
          ┌───────────────┼───────────────┐
          │               │               │
      Pod (ns-A)    Deployment (ns-B)  Service (ns-B)

parentUIDToChildren["abc"] = [Pod/ns-A, Deployment/ns-B, Service/ns-B]
```

IterateHierarchyV2는 이 인덱스를 활용해 재귀적으로 트리를 순회한다.

### 3.4 Watch 기반 실시간 업데이트

```
상수 (cluster.go):
  watchResourcesRetryTimeout = 1 * time.Second
  ClusterRetryTimeout        = 10 * time.Second
  defaultClusterResyncTimeout = 24 * time.Hour    // 전체 재동기화 주기
  defaultWatchResyncTimeout   = 10 * time.Minute  // 개별 리소스 Watch 재시작
  defaultListPageSize         = 500
  defaultListSemaphoreWeight  = 50
  defaultEventProcessingInterval = 100 * time.Millisecond
```

Watch 흐름:
1. 초기 `EnsureSynced()` 호출 시 K8s List API로 전체 리소스 로드
2. 이후 Watch 스트림으로 변경 이벤트 수신
3. 이벤트 처리 간격: 100ms 배치 처리 (`eventProcessingInterval`)
4. 24시간 주기로 전체 재동기화 (`defaultClusterResyncTimeout`)
5. 개별 Watch는 10분마다 재시작 (`defaultWatchResyncTimeout`)

---

## 4. 매니페스트 캐시 전략

### 4.1 CachedManifestResponse 구조체

**파일:** `reposerver/cache/cache.go`

```go
// CachedManifestResponse represents a cached result of a previous manifest generation
// operation, including the caching of a manifest generation error, plus additional
// information on previous failures
type CachedManifestResponse struct {
    CacheEntryHash                  string                      `json:"cacheEntryHash"`
    ManifestResponse                *apiclient.ManifestResponse `json:"manifestResponse"`
    MostRecentError                 string                      `json:"mostRecentError"`
    FirstFailureTimestamp           int64                       `json:"firstFailureTimestamp"`
    NumberOfConsecutiveFailures     int                         `json:"numberOfConsecutiveFailures"`
    NumberOfCachedResponsesReturned int                         `json:"numberOfCachedResponsesReturned"`
}
```

각 필드의 역할:

| 필드 | 역할 |
|------|------|
| `CacheEntryHash` | 캐시 무결성 검증용 해시. 구성이 변경되면 캐시 미스 처리 |
| `ManifestResponse` | 실제 매니페스트 생성 결과 |
| `MostRecentError` | 마지막 생성 에러 메시지 |
| `FirstFailureTimestamp` | 첫 번째 실패 시간 (Unix timestamp) |
| `NumberOfConsecutiveFailures` | 연속 실패 횟수 |
| `NumberOfCachedResponsesReturned` | 에러 상태에서 반환된 캐시 응답 수 |

### 4.2 getManifestCacheEntry() 알고리즘

**파일:** `reposerver/repository/repository.go`, L.953-1042

```
getManifestCacheEntry(cacheKey, request) → (cacheHit bool, response, error)

┌─────────────────────────────────────────────────────────────┐
│                  캐시 조회 (GetManifests)                    │
└──────────────────────────┬──────────────────────────────────┘
                           │
              ┌────────────▼────────────┐
              │      캐시 미스?          │
              └────────────┬────────────┘
                           │
           ┌───────────────┴───────────────┐
           │ 미스                         │ 히트
           ▼                              ▼
    return false, nil, nil     ┌──────────────────────────────┐
    (→ 재생성 필요)             │  에러 캐싱 활성화?             │
                               │  (PauseGenerationAfter... > 0)│
                               └──────────────┬───────────────┘
                                              │
                                ┌─────────────┴──────────────┐
                                │ 아니오                     │ 예 (에러 캐시 있음)
                                ▼                           ▼
                         return true,          ┌────────────────────────────┐
                         res.ManifestResponse, │ ConsecutiveFailures >= 임계값?│
                         nil                  └──────────────┬─────────────┘
                         (정상 캐시 히트)                     │
                                               ┌─────────────┴────────────┐
                                               │ 아니오                   │ 예
                                               ▼                         ▼
                                    return false,          ┌─────────────────────────┐
                                    res.ManifestResponse,  │ 시간 초과 OR 요청 초과?   │
                                    nil                    └──────────┬──────────────┘
                                    (에러 임계값 미달,                │
                                     재시도 허용)          ┌──────────┴──────────┐
                                                          │ 예                  │ 아니오
                                                          ▼                    ▼
                                                  캐시 삭제 후          NumberOfCachedResponsesReturned++
                                                  return false          캐시 업데이트
                                                  (재생성)              return true, nil, cachedError
```

소스코드에서 확인한 실제 구현:

```go
func (s *Service) getManifestCacheEntry(cacheKey string, q *apiclient.ManifestRequest,
    refSourceCommitSHAs cache.ResolvedRevisions, firstInvocation bool) (bool, *apiclient.ManifestResponse, error) {

    res := cache.CachedManifestResponse{}
    err := s.cache.GetManifests(cacheKey, q.ApplicationSource, q.RefSources, q,
        q.Namespace, q.TrackingMethod, q.AppLabelKey, q.AppName, &res, refSourceCommitSHAs, q.InstallationID)

    if err == nil {
        // 에러 캐싱 활성화 + 이전 에러 있음
        if s.initConstants.PauseGenerationAfterFailedGenerationAttempts > 0 && res.FirstFailureTimestamp > 0 {
            // 임계값 초과 → 에러 캐싱 상태
            if res.NumberOfConsecutiveFailures >= s.initConstants.PauseGenerationAfterFailedGenerationAttempts {

                // 시간 기반 탈출 조건
                if s.initConstants.PauseGenerationOnFailureForMinutes > 0 {
                    elapsedTimeInMinutes := int((s.now().Unix() - res.FirstFailureTimestamp) / 60)
                    if elapsedTimeInMinutes >= s.initConstants.PauseGenerationOnFailureForMinutes {
                        // 시간 초과 → 캐시 삭제, 재시도
                        err = s.cache.DeleteManifests(...)
                        return false, nil, nil
                    }
                }

                // 요청 수 기반 탈출 조건
                if s.initConstants.PauseGenerationOnFailureForRequests > 0 && res.NumberOfCachedResponsesReturned > 0 {
                    if res.NumberOfCachedResponsesReturned >= s.initConstants.PauseGenerationOnFailureForRequests {
                        // 요청 초과 → 캐시 삭제, 재시도
                        err = s.cache.DeleteManifests(...)
                        return false, nil, nil
                    }
                }

                // 아직 탈출 조건 미충족 → 에러 캐시 반환
                cachedErrorResponse := fmt.Errorf(cachedManifestGenerationPrefix+": %s", res.MostRecentError)

                if firstInvocation {
                    res.NumberOfCachedResponsesReturned++
                    err = s.cache.SetManifests(...)
                }
                return true, nil, cachedErrorResponse
            }

            // 임계값 미달 → 재시도 허용
            return false, res.ManifestResponse, nil
        }

        // 정상 캐시 히트
        return true, res.ManifestResponse, nil
    }

    // 캐시 미스
    return false, nil, nil
}
```

### 4.3 Double-Check Locking

세마포어 대기 후 캐시를 재확인하는 패턴이 있다.

```
Thread 1                      Thread 2
  │                              │
  ├── 캐시 미스                   │
  ├── 세마포어 대기 ←──────────── ├── 세마포어 대기
  │                              │
  ├── 세마포어 획득               │   (대기 중)
  ├── [캐시 재확인]               │
  │   → 아직 미스? → 생성         │
  ├── 결과를 캐시에 저장           │
  ├── 세마포어 해제 ──────────── →│
  │                              ├── 세마포어 획득
  │                              ├── [캐시 재확인]
  │                              │   → 히트! (Thread 1이 이미 저장)
  │                              ├── 캐시 반환 (재생성 불필요)
```

세마포어 획득 후 캐시를 재확인하는 이유: 여러 요청이 동시에 동일 앱의 매니페스트를 요청할 때, 첫 번째 요청만 실제 생성하고 나머지는 캐시를 재사용한다. 이를 통해 Thundering Herd를 방지한다.

---

## 5. Diff 캐시

### 5.1 useDiffCache 조건

**파일:** `controller/state.go`, L.1044-1084

```go
func useDiffCache(noCache bool, manifestInfos []*apiclient.ManifestResponse,
    sources []v1alpha1.ApplicationSource, app *v1alpha1.Application,
    manifestRevisions []string, statusRefreshTimeout time.Duration,
    serverSideDiff bool, log *log.Entry) bool {

    if noCache {
        return false  // 강제 캐시 무효화
    }

    refreshType, refreshRequested := app.IsRefreshRequested()
    if refreshRequested {
        return false  // refresh annotation 있음 → 캐시 사용 안 함
    }

    // serverSideDiff는 status 만료와 무관하게 캐시 사용 허용
    if app.Status.Expired(statusRefreshTimeout) && !serverSideDiff {
        return false  // 상태가 만료됨
    }

    if len(manifestInfos) != len(sources) {
        return false  // 소스 수 불일치
    }

    // revision이 마지막 비교 시점과 동일한가
    revisionChanged := !reflect.DeepEqual(app.Status.GetRevisions(), manifestRevisions)
    if revisionChanged {
        return false
    }

    // Spec의 비교 대상이 변경되었는가
    if !specEqualsCompareTo(app.Spec, sources, app.Status.Sync.ComparedTo) {
        return false
    }

    return true  // 모든 조건 충족 → diff 캐시 사용
}
```

`useDiffCache`가 true이면 diff 결과를 새로 계산하지 않고 이전 결과를 재사용한다.

```go
// controller/state.go
useDiffCache := useDiffCache(noCache, manifestInfos, sources, app,
    manifestRevisions, m.statusRefreshTimeout, serverSideDiff, logCtx)

diffConfigBuilder := argodiff.NewDiffConfigBuilder().
    WithDiffSettings(app.Spec.IgnoreDifferences, resourceOverrides,
        compareOptions.IgnoreAggregatedRoles, m.ignoreNormalizerOpts).
    WithTracking(appLabelKey, string(trackingMethod))

if useDiffCache {
    diffConfigBuilder.WithCache(m.cache, app.InstanceName(m.namespace))
} else {
    diffConfigBuilder.WithNoCache()
}
```

**왜 이런 설계인가?**

Diff 계산은 비용이 높다. 특히 수백 개의 리소스를 가진 앱에서 매번 diff를 재계산하면 CPU와 메모리가 낭비된다. revision, Spec, refresh 요청이 모두 변경되지 않았다면 이전 diff 결과를 재사용해도 안전하다.

---

## 6. 성능 최적화 기법

### 6.1 Parallelism Control

**Repo Server: parallelismLimitSemaphore**

**파일:** `reposerver/repository/repository.go`

```go
type Service struct {
    // ...
    parallelismLimitSemaphore *semaphore.Weighted  // 동시 매니페스트 생성 제한
    // ...
}

func NewService(metricsServer *metrics.MetricsServer, cache *cache.Cache,
    initConstants RepoServerInitConstants, ...) *Service {
    var parallelismLimitSemaphore *semaphore.Weighted
    if initConstants.ParallelismLimit > 0 {
        parallelismLimitSemaphore = semaphore.NewWeighted(initConstants.ParallelismLimit)
    }
    // ...
}

// GenerateManifest 호출 시 세마포어 획득
settings := operationSettings{
    sem:             s.parallelismLimitSemaphore,
    noCache:         q.NoCache,
    noRevisionCache: q.NoRevisionCache,
    allowConcurrent: q.ApplicationSource.AllowsConcurrentProcessing(),
}
```

**Controller: kubectlSemaphore**

**파일:** `controller/appcontroller.go`

```go
type ApplicationController struct {
    // ...
    kubectlSemaphore *semaphore.Weighted  // kubectl 병렬 실행 제한
    // ...
}

func (ctrl *ApplicationController) onKubectlRun(command string) (kube.CleanupFunc, error) {
    ctrl.metricsServer.IncKubectlExec(command)
    if ctrl.kubectlSemaphore != nil {
        if err := ctrl.kubectlSemaphore.Acquire(context.Background(), 1); err != nil {
            return nil, err
        }
        return func() { ctrl.kubectlSemaphore.Release(1) }, nil
    }
    return func() {}, nil
}
```

### 6.2 statusProcessors / operationProcessors 분리

**파일:** `controller/appcontroller.go`

```go
func (ctrl *ApplicationController) Run(ctx context.Context,
    statusProcessors int, operationProcessors int) {
    // ...

    // 상태 갱신 워커 (앱 비교, 헬스 체크)
    for range statusProcessors {
        go wait.Until(func() {
            for ctrl.processAppRefreshQueueItem() {
            }
        }, time.Second, ctx.Done())
    }

    // 동기화 작업 워커 (kubectl apply 등)
    for range operationProcessors {
        go wait.Until(func() {
            for ctrl.processAppOperationQueueItem() {
            }
        }, time.Second, ctx.Done())
    }
}
```

두 큐를 분리하는 이유: 동기화 작업(kubectl apply)은 상태 갱신(diff 계산)보다 훨씬 오래 걸린다. 하나의 큐에 합치면 동기화 작업이 상태 갱신을 블로킹해 앱 상태가 오랫동안 업데이트되지 않는 문제가 생긴다.

| 큐 | 워커 수 기본값 | 역할 |
|----|---------------|------|
| statusProcessors | 20 | 앱 상태 비교, 헬스 체크, 조건 업데이트 |
| operationProcessors | 10 | Sync 작업 실행 (kubectl apply 등) |

### 6.3 manifest-generate-paths 최적화

**어노테이션:** `argocd.argoproj.io/manifest-generate-paths`

**파일:** `controller/state.go`

```go
keyManifestGenerateAnnotationVal, keyManifestGenerateAnnotationExists :=
    app.Annotations[v1alpha1.AnnotationKeyManifestGeneratePaths]

for i, source := range sources {
    // ...
    if keyManifestGenerateAnnotationExists && updateRevisionResult != nil {
        if updateRevisionResult.Changes {
            revisionsMayHaveChanges = true
        }
    } else if !source.IsRef() {
        // annotation 없으면 항상 변경 가능성 있음으로 처리
        revisionsMayHaveChanges = true
    }
}
```

이 어노테이션이 설정된 경우:
1. Git commit의 변경 파일 목록을 조회한다 (`ChangedFiles()`)
2. 어노테이션에 지정된 경로와 변경 파일을 비교한다
3. 관련 없는 경로만 변경된 경우 매니페스트 재생성을 스킵한다

```
예시:
  앱 소스: apps/my-service/
  어노테이션: argocd.argoproj.io/manifest-generate-paths: apps/my-service

  커밋이 docs/ 만 변경 → revisionsMayHaveChanges = false → 재생성 스킵
  커밋이 apps/my-service/ 변경 → revisionsMayHaveChanges = true → 재생성
```

모노레포 환경에서 특히 효과적이다. 수십 개의 앱이 같은 저장소를 참조하는 경우, 무관한 앱의 매니페스트 재생성을 대폭 줄일 수 있다.

---

## 7. 샤딩 (Sharding)

### 7.1 샤딩 아키텍처

**파일:** `controller/sharding/sharding.go`

대규모 환경에서 단일 ApplicationController 인스턴스는 처리 한계에 도달한다. 샤딩은 클러스터를 여러 컨트롤러 인스턴스에 분산한다.

```
┌─────────────────────────────────────────────────┐
│              ApplicationController Pods          │
│                                                 │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐      │
│  │ Shard 0  │  │ Shard 1  │  │ Shard 2  │      │
│  │ Cluster A│  │ Cluster B│  │ Cluster C│      │
│  │ Cluster D│  │ Cluster E│  │          │      │
│  └──────────┘  └──────────┘  └──────────┘      │
│                                                 │
│            ConfigMap: shard-mapping             │
│  [{"ShardNumber":0,"ControllerName":"pod-0",   │
│    "HeartbeatTime":"2024-01-01T00:00:00Z"},    │
│   {"ShardNumber":1,"ControllerName":"pod-1",   │
│    "HeartbeatTime":"2024-01-01T00:00:00Z"},    │
│   {"ShardNumber":2,"ControllerName":"pod-2",   │
│    "HeartbeatTime":"2024-01-01T00:00:00Z"}]    │
└─────────────────────────────────────────────────┘
```

### 7.2 3가지 샤딩 알고리즘

**1. Legacy (기본값, FNV-32a 해시)**

```go
func LegacyDistributionFunction(replicas int) DistributionFunction {
    return func(c *v1alpha1.Cluster) int {
        if replicas == 0 {
            return -1
        }
        if c == nil {
            return 0  // in-cluster는 항상 shard 0
        }
        if c.Shard != nil && int(*c.Shard) < replicas {
            return int(*c.Shard)  // 수동 할당 우선
        }
        id := c.ID
        if id == "" {
            return 0
        }
        h := fnv.New32a()
        _, _ = h.Write([]byte(id))
        shard := int32(h.Sum32() % uint32(replicas))
        return int(shard)
    }
}
```

장점: 경량, 분산 계산 가능. 단점: 불균등 분배 가능성.

**2. RoundRobin (균등 분배)**

```go
func RoundRobinDistributionFunction(clusters clusterAccessor, replicas int) DistributionFunction {
    return func(c *v1alpha1.Cluster) int {
        if replicas > 0 {
            if c == nil {
                return 0
            }
            if c.Shard != nil && int(*c.Shard) < replicas {
                return int(*c.Shard)
            }
            clusterIndexdByClusterIdMap := createClusterIndexByClusterIdMap(clusters)
            clusterIndex, ok := clusterIndexdByClusterIdMap[c.ID]
            if !ok {
                return -1
            }
            shard := int(clusterIndex % replicas)
            return shard
        }
        return -1
    }
}
```

클러스터를 ID 기준으로 정렬 후 index % replicas로 배분. 균등하지만 클러스터 추가/삭제 시 대규모 재셔플이 발생한다.

**3. ConsistentHashingWithBoundedLoads (앱 수 기반 균등)**

```go
func ConsistentHashingWithBoundedLoadsDistributionFunction(
    clusters clusterAccessor, apps appAccessor, replicas int) DistributionFunction {
    return func(c *v1alpha1.Cluster) int {
        if replicas > 0 {
            // ...
            shardIndexedByCluster := createConsistentHashingWithBoundLoads(replicas, clusters, apps)
            shard, ok := shardIndexedByCluster[c.ID]
            // ...
            return shard
        }
        return -1
    }
}

func createConsistentHashingWithBoundLoads(replicas int,
    getCluster clusterAccessor, getApp appAccessor) map[string]int {
    clusters := getSortedClustersList(getCluster)
    appDistribution := getAppDistribution(getCluster, getApp)  // 클러스터별 앱 수
    consistentHashing := consistent.New()

    for i := range replicas {
        consistentHashing.Add(strconv.Itoa(i))
        appsIndexedByShard[strconv.Itoa(i)] = 0
    }

    for _, c := range clusters {
        clusterIndex, _ := consistentHashing.GetLeast(c.ID)  // 부하가 가장 낮은 샤드
        shardIndexedByCluster[c.ID], _ = strconv.Atoi(clusterIndex)
        numApps := appDistribution[c.Server]
        appsIndexedByShard[clusterIndex] += numApps
        consistentHashing.UpdateLoad(clusterIndex, appsIndexedByShard[clusterIndex])  // 부하 갱신
    }

    return shardIndexedByCluster
}
```

앱 수를 부하 지표로 사용해 각 샤드에 비슷한 수의 앱이 배분되도록 한다. 클러스터 변경 시 최소한의 재배분만 발생한다.

| 알고리즘 | 분배 균등성 | 재배분 비용 | 연산 비용 |
|----------|------------|------------|----------|
| Legacy | 낮음 | 없음 (해시) | O(1) |
| RoundRobin | 높음 (클러스터 수 기준) | 높음 | O(n log n) |
| ConsistentHashing | 높음 (앱 수 기준) | 낮음 | O(n log n) |

### 7.3 Heartbeat 프로토콜

**파일:** `controller/sharding/sharding.go`

```go
var (
    HeartbeatDuration = env.ParseNumFromEnv(common.EnvControllerHeartbeatTime, 10, 10, 60)
    HeartbeatTimeout  = 3 * HeartbeatDuration  // 기본: 30초
)

type shardApplicationControllerMapping struct {
    ShardNumber    int
    ControllerName string
    HeartbeatTime  metav1.Time
}
```

Heartbeat 동작 흐름:

```
컨트롤러 시작
  │
  ├── ConfigMap 조회 (argocd-app-controller-shard-cm)
  │
  ├── 자신의 hostname으로 기존 shard 찾기
  │   → 없으면 빈 shard 또는 타임아웃 shard 탈취
  │
  └── 주기적으로 HeartbeatTime 갱신 (10초 기본)

다른 컨트롤러가 타임아웃 감지 (30초 이상 heartbeat 없음)
  │
  └── 해당 shard 인계 (탈취)
```

실제 타임아웃 감지 코드:

```go
func getOrUpdateShardNumberForController(
    shardMappingData []shardApplicationControllerMapping,
    hostname string, replicas, shard int) (int, []shardApplicationControllerMapping) {
    // ...

    // 아직 shard를 찾지 못한 경우
    if shard == -1 {
        for i := range shardMappingData {
            shardMapping := shardMappingData[i]
            if (shardMapping.ControllerName == "") ||
               (metav1.Now().After(shardMapping.HeartbeatTime.Add(
                   time.Duration(HeartbeatTimeout) * time.Second))) {
                shard = int(shardMapping.ShardNumber)
                shardMapping.ControllerName = hostname
                shardMapping.HeartbeatTime = heartbeatCurrentTime()
                shardMappingData[i] = shardMapping
                break
            }
        }
    }
    return shard, shardMappingData
}
```

**왜 Heartbeat 프로토콜인가?**

리더 선출(Leader Election) 없이 샤드를 관리할 수 있다. 각 컨트롤러가 자신이 살아있음을 ConfigMap에 기록하고, 장애 시 다른 컨트롤러가 자동으로 해당 샤드를 인계한다. 단순하면서도 Kubernetes 네이티브한 방식이다.

---

## 8. Refresh 최적화

### 8.1 CompareWith 레벨

**파일:** `controller/appcontroller.go`

```go
const (
    // 리소스 트리만 갱신, 비교 없음
    ComparisonWithNothing CompareWith = 0
    // 마지막 비교에 사용한 revision으로 비교
    CompareWithRecent CompareWith = 1
    // 최신 Git revision으로 비교
    CompareWithLatest CompareWith = 2
    // 최신 Git revision으로 비교 + revision 캐시 무효화
    CompareWithLatestForceResolve CompareWith = 3
)
```

레벨 의미:

```
ComparisonWithNothing (0)
  → K8s Watch 이벤트 수신 시 (비관리 리소스 변경 등)
  → 리소스 트리 업데이트만 수행

CompareWithRecent (1)
  → 관리 리소스 변경 감지 시
  → 이전에 비교한 revision 재사용 (Git 호출 최소화)

CompareWithLatest (2)
  → 자동 동기화 후, 무결성 확인
  → Git에서 최신 revision 가져오기

CompareWithLatestForceResolve (3)
  → 사용자가 직접 refresh 요청 시
  → Spec 변경 감지 시
  → revision 캐시도 무효화
```

### 8.2 단조 증가 원칙

```go
// 더 강한 레벨로만 업그레이드 (다운그레이드 불가)
// requestAppRefresh 내부에서:
if existing.compareWith < level {
    existing.compareWith = level
}
```

테스트에서 확인:

```go
// controller/appcontroller_test.go
ctrl.requestAppRefresh(app.Name, CompareWithRecent.Pointer(), nil)
ctrl.requestAppRefresh(app.Name, ComparisonWithNothing.Pointer(), nil)
// → 결과: CompareWithRecent (더 강한 레벨 유지)
assert.Equal(t, CompareWithRecent, compareWith)
```

**왜 단조 증가인가?**

같은 앱에 대해 여러 이벤트가 동시에 발생할 수 있다. 예를 들어 리소스 변경 이벤트(CompareWithRecent)와 사용자 refresh 요청(CompareWithLatestForceResolve)이 거의 동시에 오는 경우, 더 강한 레벨인 CompareWithLatestForceResolve로 처리해야 한다. 단조 증가 원칙으로 이를 보장한다.

### 8.3 repoErrorGracePeriod

**파일:** `controller/state.go`

```go
type appStateManager struct {
    // ...
    repoErrorCache       goSync.Map     // 첫 에러 발생 시간 저장
    repoErrorGracePeriod time.Duration  // Grace period
}

// CompareAppState 내부:
targetObjs, manifestInfos, revisionsMayHaveChanges, err =
    m.GetRepoObjs(context.Background(), app, sources, ...)

if err != nil {
    if firstSeen, ok := m.repoErrorCache.Load(app.Name); ok {
        if time.Since(firstSeen.(time.Time)) <= m.repoErrorGracePeriod && !noRevisionCache {
            // Grace period 내 → 에러 무시, 기존 상태 유지
            return nil, ErrCompareStateRepo
        }
    } else if !noRevisionCache {
        // 처음 보는 에러 → 시간 기록 + 무시
        m.repoErrorCache.Store(app.Name, time.Now())
        return nil, ErrCompareStateRepo
    }
    failedToLoadObjs = true
} else {
    m.repoErrorCache.Delete(app.Name)  // 성공 시 에러 기록 삭제
}
```

동작 방식:

```
t=0:  repo 에러 발생 → repoErrorCache에 시간 저장, 앱 상태 Unknown 전환 유예
t=1m: 동일 앱 reconcile → grace period 내 → 에러 무시, 앱 상태 유지
t=5m: grace period 초과 → 실제 에러 처리 → 앱 상태 Unknown 전환
t=6m: repo 복구 → repoErrorCache에서 항목 삭제
```

**왜 repoErrorGracePeriod인가?**

Git 서버나 레포 서버의 일시적 장애 시 모든 앱이 Unknown 상태로 전환되면 운영자에게 불필요한 알림이 쏟아진다. Grace period 동안 앱의 마지막 알려진 상태를 유지함으로써 "Flapping"(빠른 상태 전환)을 방지한다.

---

## 9. Timing Instrumentation

### 9.1 TimingStats

**파일:** `util/stats/stats.go`

```go
// TimingStats is a helper to breakdown the timing of an expensive function call
// Usage:
// ts := NewTimingStats()
// ts.AddCheckpoint("checkpoint-1")
// ...
// ts.Timings()
type TimingStats struct {
    StartTime time.Time
    checkpoints []tsCheckpoint
}

type tsCheckpoint struct {
    name string
    time time.Time
}

func NewTimingStats() *TimingStats {
    return &TimingStats{StartTime: now()}
}

func (t *TimingStats) AddCheckpoint(name string) {
    cp := tsCheckpoint{name: name, time: now()}
    t.checkpoints = append(t.checkpoints, cp)
}

func (t *TimingStats) Timings() map[string]time.Duration {
    timings := make(map[string]time.Duration)
    prev := t.StartTime
    for _, cp := range t.checkpoints {
        timings[cp.name] = cp.time.Sub(prev)  // 구간별 소요 시간
        prev = cp.time
    }
    return timings
}
```

실제 사용 예시 (`controller/state.go`):

```go
func (m *appStateManager) GetRepoObjs(...) (...) {
    ts := stats.NewTimingStats()
    // ... git 클론/fetch ...
    ts.AddCheckpoint("git_ms")

    // ... 매니페스트 생성 ...
    ts.AddCheckpoint("generate_ms")

    // 타이밍 로그 출력
    for k, v := range ts.Timings() {
        logCtx = logCtx.WithField(k, v.Milliseconds())
    }
    logCtx = logCtx.WithField("time_ms", time.Since(ts.StartTime).Milliseconds())
    logCtx.Info("GetRepoObjs stats")
}
```

`controller/appcontroller.go`에서도 reconcile 각 단계를 체크포인트로 기록한다:

```go
ts.AddCheckpoint("compare_app_state_ms")
ts.AddCheckpoint("auto_sync_ms")
ts.AddCheckpoint("set_app_conditions_ms")
```

### 9.2 Prometheus 메트릭

**파일:** `controller/metrics/metrics.go`, `reposerver/metrics/metrics.go`

```go
// 앱 reconcile 성능 히스토그램
reconcileHistogram = prometheus.NewHistogramVec(
    prometheus.HistogramOpts{
        Name: "argocd_app_reconcile",
        Help: "Application reconciliation performance in seconds.",
        // 버킷: ~2100ms 평균 reconcile 시간 기준으로 설계
    },
    []string{"namespace", "dest_server"},
)

// repo 대기 중인 요청 수
repoPendingRequestsGauge = prometheus.NewGaugeVec(
    prometheus.GaugeOpts{
        Name: "argocd_repo_pending_request_total",
        Help: "Number of pending requests requiring repository lock",
    },
    // ...
)
```

**docs/operator-manual/metrics.md 에서 확인한 주요 메트릭:**

| 메트릭 | 타입 | 설명 |
|--------|------|------|
| `argocd_app_reconcile` | histogram | 앱 reconcile 시간 (초) |
| `argocd_repo_pending_request_total` | gauge | repo 락 대기 중인 요청 수 |
| `argocd_redis_request_duration_seconds` | histogram | Redis 요청 소요 시간 |
| `argocd_redis_request_total` | counter | Redis 요청 총 수 |
| `argocd_app_k8s_request_total` | counter | 앱별 K8s API 요청 수 |

---

## 10. 메모리 관리

### 10.1 MaxCombinedDirectoryManifestsSize

**파일:** `reposerver/repository/repository.go`

```go
type RepoServerInitConstants struct {
    // ...
    MaxCombinedDirectoryManifestsSize resource.Quantity
    // ...
}

// GenerateManifests 호출 시 적용
manifestGenResult, err = GenerateManifests(ctx, opContext.appPath, repoRoot, commitSHA, q,
    false, s.gitCredsStore,
    s.initConstants.MaxCombinedDirectoryManifestsSize,  // 크기 제한
    ...)
```

디렉토리 타입 앱에서 너무 많은 YAML 파일이 있을 때 메모리 폭발을 방지한다.

### 10.2 ClusterCache 메모리 관리

```
listSemaphore (기본: 50개)
  → 초기화 시 K8s List 작업 동시 실행 제한
  → "Limit is required to avoid memory spikes during cache initialization"
  → 50은 실험 기반 기본값

listPageSize (기본: 500, k8s pager와 동일)
  → 페이지 단위로 리소스 로드
  → 전체 리소스를 한 번에 메모리에 올리지 않음

listPageBufferSize (기본: 1)
  → 프리페치 페이지 수
  → 1로 설정 → 불필요한 선제적 메모리 사용 방지
```

### 10.3 resource.exclusions / inclusions

**파일:** `util/settings/settings.go`

불필요한 리소스를 ClusterCache에서 제외해 메모리를 절약한다.

```yaml
# argocd-cm ConfigMap
resource.exclusions: |
  - apiGroups:
    - "*"
    kinds:
    - "Event"
    clusters:
    - "*"
```

Event 리소스는 자주 변경되고 수가 많지만 GitOps 동기화에는 불필요하다. 이를 제외하면 ClusterCache 메모리가 크게 줄어든다.

---

## 11. RBAC Enforcer 캐시

### 11.1 enforcerCache

**파일:** `util/rbac/rbac.go`

```go
type Enforcer struct {
    lock          sync.Mutex
    enforcerCache *gocache.Cache  // 프로젝트별 casbin enforcer 캐싱
    adapter       *argocdAdapter
    // ...
}

func NewEnforcer(clientset kubernetes.Interface, namespace, configmap string,
    claimsEnforcer ClaimsEnforcerFunc) *Enforcer {
    return &Enforcer{
        enforcerCache: gocache.New(time.Hour, time.Hour),  // TTL: 1시간
        // ...
    }
}
```

캐시 저장 흐름:

```go
// 프로젝트별 enforcer 생성 및 캐싱
enforcer.AddFunction("globOrRegexMatch", matchFunc)
enforcer.EnableLog(e.enableLog)
enforcer.EnableEnforce(e.enabled)
e.enforcerCache.SetDefault(project, &cachedEnforcer{enforcer: enforcer, policy: policy})
```

**왜 enforcer를 캐싱하는가?**

casbin enforcer 초기화는 정책 파싱, 모델 컴파일 등 비용이 높다. API 요청마다 새 enforcer를 생성하면 레이턴시가 크게 증가한다. 1시간 TTL로 캐싱하면 정책이 자주 변경되지 않는 환경에서 성능을 크게 개선한다.

정책 변경 시 캐시는 무효화된다:

```go
func (e *Enforcer) EnableEnforce(s bool) {
    e.invalidateCache(func() {
        e.enabled = s
    })
}
```

---

## 12. 전체 설계의 "왜(Why)"

### 12.1 왜 Redis 외부 캐시인가?

```
단일 replica 환경:
  컨트롤러 A → in-memory 캐시 → 빠름

다중 replica 환경:
  컨트롤러 A → in-memory 캐시 (local) → 다른 replica가 모름
  컨트롤러 B → 동일 앱을 다시 Redis에서 읽어야 함

Redis 공유:
  컨트롤러 A → Redis SET (app|resources-tree|my-app)
  컨트롤러 B → Redis GET (app|resources-tree|my-app) → 히트!
```

HA(고가용성) 배포에서 여러 replica가 같은 캐시를 공유한다. API 서버, 컨트롤러, 레포 서버 모두 Redis를 통해 캐시를 공유한다.

### 12.2 왜 에러를 캐싱하는가? (Thundering Herd 방지)

```
레포 서버 장애 시나리오:
  t=0: 500개 앱이 동시에 reconcile 요청
  t=0: 500개 요청이 모두 레포 서버에 동시 도달 → 레포 서버 과부하
  t=1: 레포 서버가 모두 실패 응답
  t=2: 컨트롤러가 다시 500개 요청... (반복)

에러 캐싱:
  t=0: 첫 요청 실패 → 에러를 캐시에 저장
  t=1~N분: 후속 요청 → 캐시된 에러 반환 (레포 서버 요청 없음)
  t=N+1분: 캐시 만료 → 재시도
```

`PauseGenerationOnFailureForMinutes`와 `PauseGenerationOnFailureForRequests`가 이 메커니즘을 제어한다.

### 12.3 왜 Heartbeat 기반 샤딩인가?

```
리더 선출 방식:
  - etcd 또는 K8s 리더 선출 API 필요
  - 네트워크 분할 시 복잡한 처리 필요
  - 구현 복잡도 높음

Heartbeat 방식:
  - ConfigMap 하나로 관리
  - 각 컨트롤러가 독립적으로 자신의 샤드 결정
  - 장애 감지: HeartbeatTimeout (30초) 초과 시 자동 인계
  - Kubernetes 네이티브, 추가 인프라 불필요
```

### 12.4 왜 TwoLevelClient인가?

```
Redis만 사용:
  AppController가 1000개 앱을 관리
  10초마다 각 앱의 resource tree 업데이트
  → 1000 * 6 = 6000 Redis SET/분

TwoLevelClient:
  대부분의 앱이 변경 없음 (in-memory 동일성 검사)
  변경된 앱만 Redis에 전송
  → 실제 변경된 10개 앱 × 6 = 60 Redis SET/분
  → 99% 트래픽 감소
```

---

## 13. 캐시 설정 참조

주요 환경 변수 및 설정:

| 환경 변수 / 플래그 | 기본값 | 설명 |
|--------------------|--------|------|
| `ARGOCD_APP_STATE_CACHE_EXPIRATION` | 1h | 앱 상태 캐시 TTL |
| `ARGOCD_DEFAULT_CACHE_EXPIRATION` | 24h | 기본 캐시 TTL |
| `ARGOCD_APPLICATION_TREE_SHARD_SIZE` | 0 | 리소스 트리 샤드 크기 (0=샤딩 없음) |
| `ARGOCD_REDIS_COMPRESSION` | gzip | Redis 데이터 압축 알고리즘 |
| `REDIS_RETRY_COUNT` | 3 | Redis 재시도 횟수 |
| `ARGOCD_CONTROLLER_HEARTBEAT_TIME` | 10 | Heartbeat 주기 (초) |
| `CONTROLLER_REPLICAS` | 0 | 컨트롤러 replica 수 |
| `CONTROLLER_SHARD` | -1 | 수동 샤드 번호 (-1=자동) |

| 상수 | 값 | 설명 |
|------|-----|------|
| `clusterInfoCacheExpiration` | 10m | 클러스터 정보 TTL |
| `defaultClusterResyncTimeout` | 24h | ClusterCache 전체 재동기화 주기 |
| `defaultWatchResyncTimeout` | 10m | 개별 Watch 재시작 주기 |
| `defaultListPageSize` | 500 | K8s List 페이지 크기 |
| `defaultListSemaphoreWeight` | 50 | 동시 List 작업 제한 |
| `defaultEventProcessingInterval` | 100ms | 이벤트 배치 처리 간격 |
| `HeartbeatTimeout` | 30s | 샤드 타임아웃 (HeartbeatDuration × 3) |

---

## 14. 데이터 흐름 다이어그램

### 14.1 Reconcile 시 캐시 활용 전체 흐름

```
ApplicationController.processAppRefreshQueueItem()
  │
  ├── needRefreshAppStatus() → CompareWith 레벨 결정
  │
  └── reconcileApp()
        │
        ├── CompareWith == ComparisonWithNothing?
        │    └── 리소스 트리만 갱신 (Redis GET)
        │
        └── CompareWith >= CompareWithRecent?
              │
              ├── GetRepoObjs()
              │    │
              │    ├── manifest-generate-paths 확인
              │    │    └── 변경 없음? → revision 재생성 스킵
              │    │
              │    └── 레포 서버에 GenerateManifests 요청
              │          │
              │          ├── getManifestCacheEntry() 확인
              │          │    ├── 히트 → 캐시 반환
              │          │    └── 미스 → semaphore 획득 → 생성 → 캐시 저장
              │          │
              │          └── 에러? → repoErrorGracePeriod 확인
              │
              ├── useDiffCache 확인
              │    ├── true → 이전 diff 재사용
              │    └── false → ClusterCache.GetManagedLiveObjs() → diff 계산
              │
              └── 결과를 Redis에 저장
                   ├── SetAppManagedResources()
                   ├── SetAppResourcesTree() (샤딩)
                   └── SetClusterInfo()
```

### 14.2 Redis 캐시 키 버전 관리

```go
// util/cache/cache.go
func (c *Cache) generateFullKey(key string) string {
    return fmt.Sprintf("%s|%s", key, common.CacheVersion)
}
```

모든 Redis 키에 버전 접미사가 붙는다. Argo CD 업그레이드 후 이전 캐시가 자동으로 무효화된다.

---

## 15. 요약

Argo CD의 캐싱 및 성능 최적화는 다음 원칙 위에 설계되었다.

1. **계층적 캐싱**: Redis(공유) → In-Memory(로컬) → 불필요한 연산 제거
2. **에러 캐싱**: 장애 전파 방지, Thundering Herd 방지
3. **지능형 샤딩**: 앱 수 기반 균등 분배, Heartbeat로 자동 장애 복구
4. **수준별 refresh**: 불필요한 전체 재비교 방지, 단조 증가 원칙
5. **측정 가능성**: TimingStats, Prometheus 메트릭으로 병목 식별

이 모든 메커니즘은 단일 Argo CD 인스턴스가 수천 개의 앱과 수백 개의 클러스터를 안정적으로 관리할 수 있는 기반을 제공한다.
