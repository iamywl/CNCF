# 12. ClusterMesh: 멀티클러스터 네트워킹

## 개요

ClusterMesh는 Cilium의 멀티클러스터 네트워킹 솔루션이다. 여러 Kubernetes 클러스터를 하나의 논리적 네트워크로 연결하여 클러스터 간 서비스 디스커버리, 로드 밸런싱, 보안 정책 적용을 가능하게 한다.

ClusterMesh의 핵심 설계 원칙은 다음과 같다:

| 원칙 | 설명 |
|------|------|
| 독립적 컨트롤 플레인 | 각 클러스터는 자체 K8s API 서버와 etcd를 독립적으로 운영 |
| 상태 공유 | KVStore(etcd)를 통해 노드, 서비스, 엔드포인트, Identity를 공유 |
| 장애 격리 | 한 클러스터 장애가 다른 클러스터에 전파되지 않음 |
| Shared 서비스 모델 | `io.cilium/shared-service: "true"` 어노테이션 기반 선택적 공유 |
| Cluster ID 기반 격리 | 각 클러스터에 고유 ID(1~255 또는 511)를 부여하여 리소스 격리 |

### 아키텍처 전체 흐름

```
+------------------+                              +------------------+
|   Cluster A      |                              |   Cluster B      |
|                  |                              |                  |
|  +----------+   |     +-------------------+     |   +----------+  |
|  | Cilium   |   |     | ClusterMesh       |     |   | Cilium   |  |
|  | Agent    |<--+---->| API Server        |<----+-->| Agent    |  |
|  +----------+   |     | (etcd + sync)     |     |   +----------+  |
|        |        |     +--------+----------+     |        |        |
|  +----------+   |              |                |   +----------+  |
|  | K8s API  |   |     +--------+----------+     |   | K8s API  |  |
|  | Server   |   |     | KVStoreMesh       |     |   | Server   |  |
|  +----------+   |     | (선택적 캐시 계층) |     |   +----------+  |
+------------------+     +-------------------+     +------------------+
```

## ClusterMesh 아키텍처

### ClusterMesh 핵심 구조체

`pkg/clustermesh/clustermesh.go`에 정의된 `ClusterMesh` 구조체가 멀티클러스터의 중심이다:

```go
// pkg/clustermesh/clustermesh.go (lines 107-124)
type ClusterMesh struct {
    // conf is the configuration, it is immutable after NewClusterMesh()
    conf Configuration

    // common implements the common logic to connect to remote clusters.
    common common.ClusterMesh

    // globalServices is a list of all global services. The datastructure
    // is protected by its own mutex inside the structure.
    globalServices *common.GlobalServiceCache

    // syncTimeoutLogOnce ensures that the warning message triggered upon failure
    // waiting for remote clusters synchronization is output only once.
    syncTimeoutLogOnce sync.Once

    // FeatureMetrics will track which features are enabled with in clustermesh.
    FeatureMetrics ClusterMeshMetrics
}
```

`Configuration` 구조체는 ClusterMesh가 동작하는 데 필요한 모든 의존성을 담고 있다:

```go
// pkg/clustermesh/clustermesh.go (lines 32-85)
type Configuration struct {
    cell.In

    common.Config
    wait.TimeoutConfig

    ClusterInfo cmtypes.ClusterInfo
    RemoteClientFactory common.RemoteClientFactoryFn
    ServiceMerger ServiceMerger
    NodeObserver nodeStore.NodeManager
    RemoteIdentityWatcher RemoteIdentityWatcher
    IPCache ipcache.IPCacher
    ClusterSizeDependantInterval kvstore.ClusterSizeDependantIntervalFunc
    ServiceResolver dial.Resolver
    ServiceBackendResolver *dial.ServiceBackendResolver
    IPCacheWatcherExtraOpts IPCacheWatcherOptsFn
    ClusterIDsManager clusterIDsManager
    ObserverFactories []observer.Factory
    Metrics       Metrics
    CommonMetrics common.Metrics
    StoreFactory  store.Factory
    FeatureMetrics ClusterMeshMetrics
    Logger *slog.Logger
}
```

### 초기화 흐름

`NewClusterMesh()` 함수에서 ClusterMesh 인스턴스가 생성된다. 주요 조건은 `ClusterID != 0`이고 `ClusterMeshConfig` 경로가 설정되어 있어야 한다:

```go
// pkg/clustermesh/clustermesh.go (lines 128-163)
func NewClusterMesh(lifecycle cell.Lifecycle, c Configuration) *ClusterMesh {
    if c.ClusterInfo.ID == 0 || c.ClusterMeshConfig == "" {
        return nil
    }

    cm := &ClusterMesh{
        conf:           c,
        globalServices: common.NewGlobalServiceCache(c.Logger),
        FeatureMetrics: c.FeatureMetrics,
    }

    cm.common = common.NewClusterMesh(common.Configuration{
        // ... 설정 전달 ...
        NewRemoteCluster: cm.NewRemoteCluster,
    })

    lifecycle.Append(cm.common)
    return cm
}
```

**왜 ClusterID == 0이면 nil을 반환하는가?** ClusterID 0은 "미설정" 상태를 의미하는 예약 값(`ClusterIDUnset`)이다. 멀티클러스터에서는 각 클러스터가 고유한 양수 ID를 가져야 노드, 서비스, Identity를 클러스터별로 구분할 수 있다.

### 동기화 대기 메커니즘

ClusterMesh는 세 가지 리소스 타입의 동기화를 추적한다:

```
+------------------------------------------------------------------+
|                    ClusterMesh.synced()                           |
|                                                                  |
|  ForEachRemoteCluster ---> remoteCluster.synced.Nodes            |
|                       ---> remoteCluster.synced.Services         |
|                       ---> remoteCluster.synced.IPIdentities     |
|                                                                  |
|  wait.ForAll(ctx, waiters)                                       |
|                                                                  |
|  timeout --> syncTimeoutLogOnce.Do(warn) --> return nil           |
|  (서킷 브레이커: 타임아웃 시 정상 진행하여 무한 블로킹 방지)     |
+------------------------------------------------------------------+
```

`synced()` 메서드는 모든 원격 클러스터의 초기 동기화를 기다리되, 타임아웃이 발생하면 경고를 출력하고 정상 진행한다. 이는 잘못된 설정으로 인한 무한 블로킹을 방지하는 서킷 브레이커 패턴이다:

```go
// pkg/clustermesh/clustermesh.go (lines 273-298)
func (cm *ClusterMesh) synced(ctx context.Context, toWaitFn func(*remoteCluster) wait.Fn) error {
    wctx, cancel := context.WithTimeout(ctx, cm.conf.ClusterMeshSyncTimeout)
    defer cancel()

    waiters := make([]wait.Fn, 0)
    cm.common.ForEachRemoteCluster(func(rci common.RemoteCluster) error {
        rc := rci.(*remoteCluster)
        waiters = append(waiters, toWaitFn(rc))
        return nil
    })

    err := wait.ForAll(wctx, waiters)
    if ctx.Err() == nil && wctx.Err() != nil {
        cm.syncTimeoutLogOnce.Do(func() {
            cm.conf.Logger.Warn("Failed waiting for clustermesh synchronization, ...")
        })
        return nil  // 타임아웃 시 nil 반환 --> 정상 진행
    }
    return err
}
```

## 원격 클러스터 관리

### remoteCluster 구조체

각 원격 클러스터는 `remoteCluster` 구조체로 표현된다:

```go
// pkg/clustermesh/remote_cluster.go (lines 38-98)
type remoteCluster struct {
    name string
    clusterID uint32
    clusterConfigValidator func(cmtypes.CiliumClusterConfig) error
    usedIDs ClusterIDsManager

    mutex lock.RWMutex

    remoteNodes store.WatchStore        // 원격 노드 정보
    remoteServices store.WatchStore     // 원격 서비스 정보
    ipCacheWatcher *ipcache.IPIdentityWatcher  // IP-Identity 매핑
    ipCacheWatcherExtraOpts IPCacheWatcherOptsFn

    remoteIdentityWatcher RemoteIdentityWatcher
    remoteIdentityCache allocator.RemoteIDCache  // Identity 캐시

    observers map[observer.Name]observer.Observer  // 추가 옵저버

    status common.StatusFunc
    storeFactory store.Factory
    registered atomic.Bool
    synced synced

    log *slog.Logger
    featureMetrics ClusterMeshMetrics
    featureMetricMaxClusters string
}
```

### 원격 클러스터 생성 (NewRemoteCluster)

`NewRemoteCluster()`에서 네 가지 핵심 Watcher가 생성된다:

```go
// pkg/clustermesh/clustermesh.go (lines 165-229)
func (cm *ClusterMesh) NewRemoteCluster(name string, status common.StatusFunc) common.RemoteCluster {
    rc := &remoteCluster{
        name:      name,
        clusterID: cmtypes.ClusterIDUnset,
        // ...
    }

    // 1) 원격 노드 Watcher
    rc.remoteNodes = cm.conf.StoreFactory.NewWatchStore(
        name,
        nodeStore.ValidatingKeyCreator(
            nodeStore.ClusterNameValidator(name),
            nodeStore.NameValidator(),
            nodeStore.ClusterIDValidator(&rc.clusterID),
        ),
        nodeStore.NewNodeObserver(cm.conf.NodeObserver, source.ClusterMesh),
        store.RWSWithOnSyncCallback(func(ctx context.Context) { close(rc.synced.nodes) }),
    )

    // 2) 원격 서비스 Watcher
    rc.remoteServices = cm.conf.StoreFactory.NewWatchStore(
        name,
        serviceStore.KeyCreator(
            serviceStore.ClusterNameValidator(name),
            serviceStore.NamespacedNameValidator(),
            serviceStore.ClusterIDValidator(&rc.clusterID),
        ),
        common.NewSharedServicesObserver(/* ... */),
        store.RWSWithOnSyncCallback(func(ctx context.Context) { close(rc.synced.services) }),
    )

    // 3) IP-Identity Watcher
    rc.ipCacheWatcher = ipcache.NewIPIdentityWatcher(/* ... */)

    // 4) 추가 Observer들 (ObserverFactories)
    for _, factory := range cm.conf.ObserverFactories {
        obs := factory(name, onceSynced)
        rc.observers[obs.Name()] = obs
    }

    return rc
}
```

**왜 각 Watcher에 Validator를 부착하는가?** 원격 etcd에서 받은 데이터가 해당 클러스터에서 온 것이 맞는지 검증하기 위함이다. `ClusterNameValidator`는 서비스의 Cluster 필드가 기대하는 클러스터 이름과 일치하는지, `ClusterIDValidator`는 ClusterID가 올바른지 확인한다. 이를 통해 잘못된 데이터가 로컬 상태를 오염시키는 것을 방지한다.

### Run() - 연결 및 감시 시작

```go
// pkg/clustermesh/remote_cluster.go (lines 100-165)
func (rc *remoteCluster) Run(ctx context.Context, backend kvstore.BackendOperations,
    config cmtypes.CiliumClusterConfig, ready chan<- error) {

    // 1) 클러스터 설정 검증
    if err := rc.clusterConfigValidator(config); err != nil {
        ready <- err; close(ready); return
    }

    // 2) Cluster ID 업데이트 (ID 변경 시 기존 엔트리 drain)
    if err := rc.onUpdateConfig(config); err != nil {
        ready <- err; close(ready); return
    }

    // 3) 원격 Identity 감시 시작
    remoteIdentityCache, err := rc.remoteIdentityWatcher.WatchRemoteIdentities(
        rc.name, rc.clusterID, backend, config.Capabilities.Cached)

    // 4) WatchStoreManager 생성 (SyncedCanaries 지원 여부에 따라)
    var mgr store.WatchStoreManager
    if config.Capabilities.SyncedCanaries {
        mgr = rc.storeFactory.NewWatchStoreManager(backend, rc.name)
    } else {
        mgr = store.NewWatchStoreManagerImmediate(rc.log)
    }

    // 5) prefix adapter 결정 (Cached 모드면 cilium/state -> cilium/cache 변환)
    adapter := func(prefix string) string { return prefix }
    if config.Capabilities.Cached {
        adapter = kvstore.StateToCachePrefix
    }

    // 6) 각 리소스 타입의 Watch 등록
    mgr.Register(adapter(nodeStore.NodeStorePrefix), func(ctx context.Context) {
        rc.remoteNodes.Watch(ctx, backend, path.Join(adapter(nodeStore.NodeStorePrefix), rc.name))
    })
    mgr.Register(adapter(serviceStore.ServiceStorePrefix), func(ctx context.Context) {
        rc.remoteServices.Watch(ctx, backend, path.Join(adapter(serviceStore.ServiceStorePrefix), rc.name))
    })
    // ... ipcache, identities 등록 ...

    close(ready)
    mgr.Run(ctx)
}
```

**왜 SyncedCanaries를 사용하는가?** SyncedCanaries는 원격 etcd에 "이 prefix의 초기 동기화가 완료되었음"을 알리는 신호(canary key)이다. 이를 통해 아직 초기 동기화가 완료되지 않은 상태에서 불완전한 데이터를 기반으로 결정을 내리는 것을 방지한다.

### synced 구조체 - 동기화 상태 추적

```go
// pkg/clustermesh/remote_cluster.go (lines 289-355)
type synced struct {
    wait.SyncedCommon
    services       chan struct{}
    nodes          chan struct{}
    ipcache        chan struct{}
    identities     *lock.StoppableWaitGroup
    identitiesDone lock.DoneFunc
    observers      map[observer.Name]chan struct{}
}
```

각 채널은 해당 리소스 타입의 초기 리스트 수신이 완료되면 닫힌다. Identity에는 `StoppableWaitGroup`을 사용하는데, 이는 etcd 재연결 시 콜백이 여러 번 실행될 수 있기 때문이다:

```
동기화 상태 전이:

  초기화 --> [nodes 채널 open] --> 초기 리스트 수신 --> [nodes 채널 close]
  초기화 --> [services 채널 open] --> 초기 리스트 수신 --> [services 채널 close]
  초기화 --> [ipcache 채널 open] --> 초기 리스트 수신 --> [ipcache 채널 close]
  초기화 --> [identities WaitGroup add] --> 초기 동기화 --> [done 호출]

  IPIdentities 동기화 = ipcache + identities + nodes 모두 완료
    (노드 주소도 ipcache 엔트리를 생성하므로 nodes 대기 필요)
```

### 연결 해제 및 정리

```go
// pkg/clustermesh/remote_cluster.go (lines 183-198)
func (rc *remoteCluster) Remove(context.Context) {
    rc.remoteNodes.Drain()
    rc.remoteServices.Drain()
    rc.ipCacheWatcher.Drain()
    rc.remoteIdentityWatcher.RemoveRemoteIdentities(rc.name)

    for _, obs := range rc.observers {
        obs.Drain()
    }
    rc.usedIDs.ReleaseClusterID(rc.clusterID)
}
```

`RevokeCache()`는 부분 캐시 폐기(서비스만)를 수행하고, `Remove()`는 전체 캐시를 삭제한다:

```
RevokeCache() vs Remove():

  RevokeCache():
    - 서비스만 Drain (잠재적으로 오래된 백엔드 방지)
    - 노드/Identity/IPcache는 유지 (기존 연결 보존)
    - 연결 일시적 단절 시 사용

  Remove():
    - 모든 리소스 Drain (노드, 서비스, IPcache, Identity)
    - Cluster ID 해제
    - 설정 파일 삭제 시 사용
```

## KVStore 동기화

### KVStore 키 체계

ClusterMesh의 모든 상태는 etcd에 계층적 키 구조로 저장된다:

```
cilium/
├── state/                                  # 원본 상태 (StatePrefix)
│   ├── nodes/v1/<cluster>/<node>          # 노드 정보
│   ├── services/v1/<cluster>/<ns>/<name>  # 서비스 정보
│   ├── identities/v1/id/<id>             # Identity 정보
│   └── ip/v1/default/<ip>                # IP-Identity 매핑
│
├── cache/                                  # KVStoreMesh 캐시 (CachePrefix)
│   ├── nodes/v1/<cluster>/<node>
│   ├── services/v1/<cluster>/<ns>/<name>
│   ├── identities/v1/id/<id>
│   └── ip/v1/default/<ip>
│
├── cluster-config/<cluster>               # 클러스터 설정
│   └── {"id":1,"capabilities":{...}}
│
└── synced/<cluster>/                      # 동기화 완료 카나리 키
    ├── cilium/state/nodes/v1
    ├── cilium/state/services/v1
    └── cilium/state/identities/v1
```

### prefix 상수 정의

```go
// pkg/kvstore/kvstore.go (lines 23-47)
const (
    BaseKeyPrefix = "cilium"
    StatePrefix   = BaseKeyPrefix + "/state"   // "cilium/state"
    CachePrefix   = BaseKeyPrefix + "/cache"   // "cilium/cache"
    ClusterConfigPrefix = BaseKeyPrefix + "/cluster-config"
    SyncedPrefix  = BaseKeyPrefix + "/synced"
)
```

### State -> Cache prefix 변환

KVStoreMesh 모드에서는 `cilium/state` prefix가 `cilium/cache` prefix로 변환된다:

```go
// pkg/kvstore/kvstore.go (lines 56-61)
func StateToCachePrefix(prefix string) string {
    if strings.HasPrefix(prefix, StatePrefix) {
        return strings.Replace(prefix, StatePrefix, CachePrefix, 1)
    }
    return prefix
}
```

이 변환의 의미:

```
직접 연결 모드:
  Agent --> 원격 etcd --> cilium/state/services/v1/cluster-b/ns/svc

KVStoreMesh 모드:
  Agent --> 로컬 etcd --> cilium/cache/services/v1/cluster-b/ns/svc
  (KVStoreMesh가 원격 state -> 로컬 cache로 미러링)
```

### BackendOperations 인터페이스

모든 KVStore 연산은 `BackendOperations` 인터페이스를 통해 추상화된다:

```go
// pkg/kvstore/backend.go (lines 133-203)
type BackendOperations interface {
    Status() *models.Status
    StatusCheckErrors() <-chan error
    LockPath(ctx context.Context, path string) (KVLocker, error)
    Get(ctx context.Context, key string) ([]byte, error)
    Delete(ctx context.Context, key string) error
    DeletePrefix(ctx context.Context, path string) error
    Update(ctx context.Context, key string, value []byte, lease bool) error
    ListPrefix(ctx context.Context, prefix string) (KeyValuePairs, error)
    ListAndWatch(ctx context.Context, prefix string) EventChan
    // ... 기타 메서드 ...
}
```

**왜 인터페이스로 추상화하는가?** etcd 외에 다른 KVStore 백엔드를 지원할 수 있도록 하고, 테스트에서 목(mock)을 쉽게 주입할 수 있게 하기 위함이다.

### ServiceStorePrefix

서비스 키 경로는 `STABLE API`로 표시되어 하위 호환성이 보장된다:

```go
// pkg/clustermesh/store/store.go (lines 23-29)
var (
    // WARNING - STABLE API: Changing the structure or values of this will
    // break backwards compatibility
    ServiceStorePrefix = path.Join(kvstore.BaseKeyPrefix, "state", "services", "v1")
    // 결과: "cilium/state/services/v1"
)
```

## 서비스 디스커버리

### ClusterService 데이터 모델

클러스터 간 공유되는 서비스는 `ClusterService` 구조체로 표현된다:

```go
// pkg/clustermesh/store/store.go (lines 52-90)
type ClusterService struct {
    Cluster   string `json:"cluster"`     // 클러스터 이름
    Namespace string `json:"namespace"`   // 네임스페이스
    Name      string `json:"name"`        // 서비스 이름

    Frontends map[string]PortConfiguration `json:"frontends"` // 프론트엔드 IP -> 포트
    Backends  map[string]PortConfiguration `json:"backends"`  // 백엔드 IP -> 포트
    Hostnames map[string]string            `json:"hostnames,omitempty"` // 백엔드 호스트명
    Zones     map[string]BackendZone       `json:"zones,omitempty"`     // 존 정보

    Labels    map[string]string `json:"labels"`    // 레이블
    Selector  map[string]string `json:"selector"`  // 셀렉터

    IncludeExternal bool   `json:"includeExternal"` // 외부 엔드포인트 포함 여부
    Shared          bool   `json:"shared"`          // 다른 클러스터에 공유 여부
    ClusterID       uint32 `json:"clusterID"`       // 클러스터 ID
}
```

KVStore 키 구조:

```go
// pkg/clustermesh/store/store.go (lines 102-106)
func (s *ClusterService) GetKeyName() string {
    // WARNING - STABLE API
    return path.Join(s.Cluster, s.Namespace, s.Name)
}
// 결과 키: cilium/state/services/v1/<cluster>/<namespace>/<name>
```

### GlobalService와 GlobalServiceCache

같은 이름의 서비스가 여러 클러스터에 존재할 때, `GlobalServiceCache`가 이를 통합 관리한다:

```go
// pkg/clustermesh/common/services.go (lines 18-26)
type GlobalService struct {
    ClusterServices map[string]*serviceStore.ClusterService  // cluster-name -> service
}

type GlobalServiceCache struct {
    logger *slog.Logger
    mutex  lock.RWMutex
    byName map[types.NamespacedName]*GlobalService  // ns/name -> global service
}
```

동작 흐름:

```
Cluster A: svc "default/nginx" (backends: 10.0.1.1, 10.0.1.2)
Cluster B: svc "default/nginx" (backends: 10.0.2.1, 10.0.2.2)

GlobalServiceCache:
  byName["default/nginx"] = GlobalService{
      ClusterServices: {
          "cluster-a": ClusterService{Backends: {10.0.1.1, 10.0.1.2}},
          "cluster-b": ClusterService{Backends: {10.0.2.1, 10.0.2.2}},
      }
  }

최종 BPF LB에 프로그래밍되는 백엔드:
  nginx -> [10.0.1.1, 10.0.1.2, 10.0.2.1, 10.0.2.2]
```

### Shared 플래그 기반 필터링

모든 서비스가 공유되는 것이 아니라, `Shared: true`인 서비스만 클러스터 간 공유된다:

```go
// pkg/clustermesh/common/services.go (lines 187-204)
func (r *remoteServiceObserver) OnUpdate(key store.Key) {
    svc := &(key.(*serviceStore.ValidatingClusterService).ClusterService)

    // Shared가 false인 서비스는 무시
    if !svc.Shared {
        if r.cache.Has(svc) {
            // 이전에 Shared였다가 해제된 경우 삭제 이벤트 트리거
            r.OnDelete(key)
        } else {
            // 처음부터 Shared가 아닌 경우 무시
        }
        return
    }

    r.cache.OnUpdate(svc)
    r.onUpdate(svc)
}
```

**왜 Shared 플래그가 필요한가?** 모든 서비스를 무조건 공유하면 네임스페이스 격리가 깨지고, 불필요한 동기화 오버헤드가 발생한다. 관리자가 명시적으로 공유할 서비스를 선택할 수 있도록 opt-in 방식을 채택했다.

### ServiceMerger - 서비스 병합

원격 클러스터의 서비스를 로컬 로드밸런서에 병합하는 인터페이스:

```go
// pkg/clustermesh/service_merger.go (lines 24-27)
type ServiceMerger interface {
    MergeExternalServiceUpdate(service *serviceStore.ClusterService)
    MergeExternalServiceDelete(service *serviceStore.ClusterService)
}
```

Update 시 원격 백엔드를 로컬 로드밸런서 테이블에 추가한다:

```go
// pkg/clustermesh/service_merger.go (lines 78-91)
func (sm *serviceMerger) MergeExternalServiceUpdate(service *serviceStore.ClusterService) {
    name := loadbalancer.NewServiceName(service.Namespace, service.Name)
    txn := sm.writer.WriteTxn()
    defer txn.Commit()

    sm.writer.SetBackendsOfCluster(
        txn,
        name,
        source.ClusterMesh,
        service.ClusterID,
        ClusterServiceToBackendParams(service)...,
    )
}
```

Delete 시 해당 클러스터의 백엔드를 제거한다:

```go
// pkg/clustermesh/service_merger.go (lines 66-76)
func (sm *serviceMerger) MergeExternalServiceDelete(service *serviceStore.ClusterService) {
    name := loadbalancer.NewServiceName(service.Namespace, service.Name)
    txn := sm.writer.WriteTxn()
    defer txn.Commit()
    sm.writer.DeleteBackendsOfServiceFromCluster(
        txn, name, source.ClusterMesh, service.ClusterID,
    )
}
```

### 서비스 검증 (Validators)

KVStore에서 수신한 서비스 데이터는 여러 단계의 검증을 거친다:

```go
// pkg/clustermesh/store/store.go (lines 246-284)

// 1) 클러스터 이름 검증
func ClusterNameValidator(clusterName string) clusterServiceValidator {
    return func(_ string, svc *ClusterService) error {
        if svc.Cluster != clusterName {
            return fmt.Errorf("unexpected cluster name: got %s, expected %s",
                svc.Cluster, clusterName)
        }
        return nil
    }
}

// 2) 네임스페이스/이름이 키와 일치하는지 검증
func NamespacedNameValidator() clusterServiceValidator {
    return func(key string, svc *ClusterService) error {
        if got := svc.NamespaceServiceName().String(); got != key {
            return fmt.Errorf("namespaced name does not match key: got %s, expected %s", got, key)
        }
        return nil
    }
}

// 3) Cluster ID 검증
func ClusterIDValidator(clusterID *uint32) clusterServiceValidator {
    return func(_ string, svc *ClusterService) error {
        if svc.ClusterID != *clusterID {
            return fmt.Errorf("unexpected cluster ID: got %d, expected %d",
                svc.ClusterID, *clusterID)
        }
        return nil
    }
}
```

## Identity 공유

### 원격 Identity 감시

ClusterMesh는 `RemoteIdentityWatcher` 인터페이스를 통해 원격 클러스터의 Identity를 감시한다:

```go
// pkg/clustermesh/clustermesh.go (lines 88-100)
type RemoteIdentityWatcher interface {
    WatchRemoteIdentities(remoteName string, remoteID uint32,
        backend kvstore.BackendOperations, cachedPrefix bool) (allocator.RemoteIDCache, error)
    RemoveRemoteIdentities(name string)
}
```

Identity는 KVStore의 `cilium/state/identities/v1/` 경로에 저장되며, master/slave 키 패턴을 사용한다:

```
cilium/state/identities/v1/id/<numeric-id>
  -> value: security labels (e.g., "k8s:app=nginx;k8s:io.kubernetes.pod.namespace=default")
```

Identity 동기화의 특수성:

| 특성 | 설명 |
|------|------|
| StoppableWaitGroup 사용 | 채널 대신 WaitGroup 사용, etcd 재연결 시 다중 콜백 처리 가능 |
| IPIdentities 의존성 | IP-Identity 동기화는 노드 동기화에도 의존 (노드 주소가 ipcache 엔트리 생성) |
| Cached prefix | KVStoreMesh 모드에서 cilium/cache prefix 아래에서 읽기 |

```go
// pkg/clustermesh/remote_cluster.go (lines 340-342)
func (s *synced) IPIdentities(ctx context.Context) error {
    return s.Wait(ctx, s.ipcache, s.identities.WaitChannel(), s.nodes)
    //                  ^^^^^^^    ^^^^^^^^^^^^^^^^^^^^^^^^^    ^^^^^
    //                  ipcache    identities                  nodes
    //                  3개 리소스 모두 동기화 완료 후 반환
}
```

## ClusterID 관리

### ClusterIDsManager

각 원격 클러스터의 ID 고유성을 보장하는 관리자:

```go
// pkg/clustermesh/idsmgr.go (lines 15-18)
type ClusterIDsManager interface {
    ReserveClusterID(clusterID uint32) error
    ReleaseClusterID(clusterID uint32)
}
```

### ID 예약 규칙

```go
// pkg/clustermesh/idsmgr.go (lines 55-74)
func (cm *ClusterMeshUsedIDs) ReserveClusterID(clusterID uint32) error {
    // 규칙 1: ID 0 거부 (예약값)
    if clusterID == cmtypes.ClusterIDUnset {
        return fmt.Errorf("clusterID %d is reserved", clusterID)
    }

    // 규칙 2: 로컬 클러스터 ID와 동일하면 거부
    if clusterID == cm.localClusterID {
        return fmt.Errorf("clusterID %d is assigned to the local cluster", clusterID)
    }

    cm.UsedClusterIDsMutex.Lock()
    defer cm.UsedClusterIDsMutex.Unlock()

    // 규칙 3: 이미 사용 중인 ID 거부
    if _, ok := cm.UsedClusterIDs[clusterID]; ok {
        return fmt.Errorf("clusterID %d is already used", clusterID)
    }

    cm.UsedClusterIDs[clusterID] = struct{}{}
    return nil
}
```

### Cluster ID 범위와 이름 규칙

```go
// pkg/clustermesh/types/types.go (lines 17-26)
const (
    ClusterIDMin    = 0       // 예약 (미설정)
    ClusterIDExt511 = 511     // 확장 최대값
    ClusterIDUnset  = ClusterIDMin
)

var ClusterIDMax uint32 = defaults.MaxConnectedClusters  // 기본 255
```

클러스터 이름 제약:

```go
// pkg/clustermesh/types/types.go (lines 31-36)
const (
    clusterNameMaxLength = 32
    clusterNameRegexStr = `^([a-z0-9][-a-z0-9]*)?[a-z0-9]$`
)
```

| 규칙 | 제한 |
|------|------|
| 최대 길이 | 32자 |
| 허용 문자 | 소문자 알파벳, 숫자, 하이픈 |
| 시작/끝 | 알파벳 또는 숫자만 허용 |
| ID 범위 | 1~255 (기본) 또는 1~511 (확장) |

### CiliumClusterConfig

원격 클러스터의 설정과 기능(capabilities)을 나타내는 구조체:

```go
// pkg/clustermesh/types/types.go (lines 96-117)
type CiliumClusterConfig struct {
    ID           uint32                           `json:"id,omitempty"`
    Capabilities CiliumClusterConfigCapabilities  `json:"capabilities,omitempty"`
}

type CiliumClusterConfigCapabilities struct {
    SyncedCanaries       bool   `json:"syncedCanaries,omitempty"`
    Cached               bool   `json:"cached,omitempty"`
    MaxConnectedClusters uint32 `json:"maxConnectedClusters,omitempty"`
    ServiceExportsEnabled *bool  `json:"serviceExportsEnabled,omitempty"`
}
```

Capabilities 필드의 의미:

| 필드 | 의미 |
|------|------|
| SyncedCanaries | prefix별 초기 동기화 완료 알림 카나리 키 지원 |
| Cached | KVStoreMesh에 의해 캐시됨 (cilium/cache prefix 사용) |
| MaxConnectedClusters | 최대 연결 가능 클러스터 수 |
| ServiceExportsEnabled | MCS-API ServiceExports 활성화 여부 |

### Cluster ID 변경 처리

원격 클러스터의 ID가 변경되면 모든 캐시를 drain하고 새 ID로 재설정한다:

```go
// pkg/clustermesh/remote_cluster.go (lines 237-272)
func (rc *remoteCluster) onUpdateConfig(newConfig cmtypes.CiliumClusterConfig) error {
    if newConfig.ID == rc.clusterID {
        return nil
    }

    // 기존 ID가 설정되어 있었다면 모든 엔트리 drain
    if rc.clusterID != cmtypes.ClusterIDUnset {
        rc.remoteNodes.Drain()
        rc.remoteServices.Drain()
        rc.ipCacheWatcher.Drain()
        rc.remoteIdentityWatcher.RemoveRemoteIdentities(rc.name)
    }

    // 새 ID 예약 (실패하면 에러 반환)
    if err := rc.usedIDs.ReserveClusterID(newConfig.ID); err != nil {
        return err
    }

    // 이전 ID 해제
    rc.usedIDs.ReleaseClusterID(rc.clusterID)
    rc.clusterID = newConfig.ID

    return nil
}
```

**왜 ID 변경 시 drain이 필요한가?** 기존 엔트리들은 이전 ClusterID를 기반으로 검증되었다. ID가 변경되면 이들은 더 이상 유효하지 않으며, 해제된 ID가 다른 클러스터에 재사용될 수 있어 충돌이 발생할 수 있다.

## KVStoreMesh

### 개요 및 목적

KVStoreMesh는 ClusterMesh의 선택적 캐시 계층이다. 원격 클러스터의 etcd에서 데이터를 읽어 로컬 etcd에 캐시함으로써, Cilium Agent가 원격 etcd에 직접 연결하지 않도록 한다:

```
직접 연결 (KVStoreMesh 없음):
  Agent-1 --\
  Agent-2 ---+--> 원격 etcd (클러스터당 N개 연결)
  Agent-N --/

KVStoreMesh 사용:
  Agent-1 --\                     +---------------+
  Agent-2 ---+--> 로컬 etcd <---- | KVStoreMesh   |----> 원격 etcd
  Agent-N --/                     | (클러스터당 1) |      (1개 연결)
                                  +---------------+
```

**왜 KVStoreMesh가 필요한가?**

| 문제 | KVStoreMesh 해결책 |
|------|-------------------|
| 원격 etcd 부하 | N개 Agent 대신 1개 KVStoreMesh만 연결 |
| 네트워크 비용 | 클러스터 간 연결 수 대폭 감소 |
| 장애 격리 | 원격 etcd 장애 시 로컬 캐시에서 계속 서비스 |
| 부트스트랩 속도 | 로컬 etcd에서 읽기 (낮은 지연 시간) |

### KVStoreMesh 구조체

```go
// pkg/clustermesh/kvstoremesh/kvstoremesh.go (lines 53-67)
type KVStoreMesh struct {
    common common.ClusterMesh
    config Config

    client kvstore.Client           // 로컬 etcd 클라이언트
    storeFactory store.Factory
    reflectorFactories []reflector.Factory  // 리소스별 Reflector 팩토리
    logger *slog.Logger
    started chan struct{}
}
```

### KVStoreMesh 설정

```go
// pkg/clustermesh/kvstoremesh/kvstoremesh.go (lines 28-41)
type Config struct {
    PerClusterReadyTimeout time.Duration  // 15초 (기본)
    GlobalReadyTimeout     time.Duration  // 10분 (기본)
    EnableHeartBeat        bool
    DisableDrainOnDisconnection bool
}
```

### Reflector 시스템

Reflector는 원격 etcd의 특정 prefix를 감시하고 로컬 etcd로 미러링하는 컴포넌트이다:

```go
// pkg/clustermesh/kvstoremesh/reflector/reflector.go (lines 22-28)
const (
    Endpoints      Name = "endpoints"
    Identities     Name = "identities"
    Nodes          Name = "nodes"
    Services       Name = "services"
    ServiceExports Name = "service exports"
)
```

Reflector 인터페이스:

```go
// pkg/clustermesh/kvstoremesh/reflector/reflector.go (lines 32-57)
type Reflector interface {
    Name() Name
    Status() Status
    Run(ctx context.Context)
    Register(mgr store.WatchStoreManager, remote kvstore.BackendOperations,
        cfg types.CiliumClusterConfig)
    DeleteCache(ctx context.Context) error
    RevokeCache(ctx context.Context)
}
```

Reflector 팩토리 생성:

```go
// pkg/clustermesh/kvstoremesh/reflector/reflector.go (lines 113-148)
func NewFactory(name Name, prefix string, opts ...opt) Factory {
    return func(local kvstore.Client, sf store.Factory, cluster string, onSync func()) Reflector {
        var rfl = reflector{
            name:        name,
            cluster:     cluster,
            basePrefix:  prefix,
            statePrefix: path.Join(prefix, cluster),
            cachePrefix: path.Join(kvstore.StateToCachePrefix(prefix), cluster),
            // ...
        }

        rfl.syncer = syncer{
            SyncStore: sf.NewSyncStore(
                cluster, local, rfl.cachePrefix,
                store.WSSWithSyncedKeyOverride(kvstore.StateToCachePrefix(rfl.basePrefix)),
            ),
            syncedDone: onSync,
        }

        rfl.watcher = sf.NewWatchStore(
            cluster, store.KVPairCreator, &rfl.syncer,
            store.RWSWithOnSyncCallback(rfl.syncer.OnSync),
        )

        return &rfl
    }
}
```

### prefix 매핑 흐름

```
원격 etcd                          로컬 etcd
(source cluster)                   (KVStoreMesh가 미러링)

cilium/state/services/v1/          cilium/cache/services/v1/
  cluster-b/                         cluster-b/
    default/nginx                      default/nginx
    kube-system/dns                    kube-system/dns

cilium/state/nodes/v1/             cilium/cache/nodes/v1/
  cluster-b/                         cluster-b/
    node-1                             node-1
    node-2                             node-2

cilium/state/identities/v1/        cilium/cache/identities/v1/
  id/12345                           id/12345

cilium/state/ip/v1/                cilium/cache/ip/v1/
  default/10.0.1.1                   default/10.0.1.1
```

### KVStoreMesh remoteCluster

```go
// pkg/clustermesh/kvstoremesh/remote_cluster.go (lines 34-64)
type remoteCluster struct {
    name         string
    localBackend kvstore.BackendOperations
    reflectors   map[reflector.Name]reflector.Reflector
    status       common.StatusFunc
    registered   atomic.Bool
    cancel       context.CancelFunc
    wg           sync.WaitGroup
    storeFactory store.Factory
    synced       synced
    readyTimeout time.Duration
    disableDrainOnDisconnection bool
    logger       *slog.Logger
}
```

Run() 메서드에서 원격 클러스터 설정을 로컬에 전파하며, Cached와 SyncedCanaries를 강제로 true로 설정한다:

```go
// pkg/clustermesh/kvstoremesh/remote_cluster.go (lines 66-103)
func (rc *remoteCluster) Run(ctx context.Context, backend kvstore.BackendOperations,
    srccfg types.CiliumClusterConfig, ready chan<- error) {

    var dstcfg = srccfg
    dstcfg.Capabilities.SyncedCanaries = true
    dstcfg.Capabilities.Cached = true

    stopAndWait, err := clustercfg.Enforce(ctx, rc.name, dstcfg, rc.localBackend, rc.logger)
    // ...

    var mgr store.WatchStoreManager
    if srccfg.Capabilities.SyncedCanaries {
        mgr = rc.storeFactory.NewWatchStoreManager(backend, rc.name)
    } else {
        mgr = store.NewWatchStoreManagerImmediate(rc.logger)
    }

    for _, rfl := range rc.reflectors {
        rfl.Register(mgr, backend, srccfg)
    }

    rc.registered.Store(true)
    close(ready)
    mgr.Run(ctx)
}
```

### 캐시 정리 (drain)

원격 클러스터 연결 해제 시 캐시 정리 절차:

```go
// pkg/clustermesh/kvstoremesh/remote_cluster.go (lines 167-211)
func (rc *remoteCluster) drain(ctx context.Context, withGracePeriod bool) (err error) {
    // 1단계: 클러스터 설정 키 삭제 (새 Agent 연결 방지)
    var cfgkey = path.Join(kvstore.ClusterConfigPrefix, rc.name)
    rc.localBackend.Delete(ctx, cfgkey)

    // 2단계: Grace period (3분) 대기 (Agent들이 먼저 연결 해제하도록)
    if withGracePeriod {
        const drainGracePeriod = 3 * time.Minute
        // ... 대기 ...
    }

    // 3단계: synced prefix 삭제
    var synpfx = path.Join(kvstore.SyncedPrefix, rc.name) + "/"
    rc.localBackend.DeletePrefix(ctx, synpfx)

    // 4단계: 각 Reflector의 캐시 데이터 삭제
    for _, rfl := range sorted {
        rfl.DeleteCache(ctx)
    }

    return nil
}
```

**왜 3분의 grace period가 있는가?** 클러스터 설정 삭제 후 바로 캐시 데이터를 삭제하면, 아직 연결된 Agent들이 불완전한 데이터를 읽을 수 있다. 3분의 유예 기간 동안 Agent들이 설정 변경을 감지하고 스스로 연결을 해제하도록 한다.

### 연결 타임아웃 처리

```go
// pkg/clustermesh/kvstoremesh/remote_cluster.go (lines 216-224)
func (rc *remoteCluster) waitForConnection(ctx context.Context) {
    select {
    case <-ctx.Done():
    case <-rc.synced.connected:
        // 연결 성공
    case <-time.After(rc.readyTimeout):
        // 타임아웃: readiness 체크에서 제외
        rc.synced.resources.ForceAllDone()
    }
}
```

## ClusterMesh API Server

### 역할

ClusterMesh API Server는 로컬 Kubernetes 리소스를 etcd에 동기화하는 컴포넌트이다. Cilium Agent들은 이 etcd를 통해 원격 클러스터의 정보를 얻는다.

```
+-------------------+     +-----------------------+     +-----------+
| K8s API Server    | --> | ClusterMesh           | --> | etcd      |
| (CiliumNode,      |     | API Server            |     | (shared)  |
|  CiliumIdentity,  |     | (Synchronizer)        |     |           |
|  CiliumEndpoint,  |     +-----------------------+     +-----------+
|  Services)        |                                        ^
+-------------------+                                        |
                                                    Cilium Agent (원격)
```

### 동기화 대상 리소스

```go
// clustermesh-apiserver/clustermesh/cells.go (lines 68-131)

// 1) CiliumNode -> cilium/state/nodes/v1/<cluster>/<node>
cell.Invoke(RegisterSynchronizer[*cilium_api_v2.CiliumNode])

// 2) CiliumIdentity -> cilium/state/identities/v1/id/<id>
cell.Invoke(RegisterSynchronizer[*cilium_api_v2.CiliumIdentity])

// 3) CiliumEndpoint -> cilium/state/ip/v1/default/<ip>
cell.Invoke(RegisterSynchronizer[*types.CiliumEndpoint])

// 4) CiliumEndpointSlice -> cilium/state/ip/v1/default/<ip> (CES 모드)
cell.Invoke(RegisterSynchronizer[*cilium_api_v2a1.CiliumEndpointSlice])
```

각 리소스의 Converter:

| 리소스 | Converter | KVStore prefix |
|--------|-----------|---------------|
| CiliumNode | `CiliumNodeConverter` | `cilium/state/nodes/v1` |
| CiliumIdentity | `CiliumIdentityConverter` | `cilium/state/identities/v1/id` |
| CiliumEndpoint | `CachedConverter` | `cilium/state/ip/v1/default` |
| CiliumEndpointSlice | `CachedConverter` | `cilium/state/ip/v1/default` |

### Synchronizer 동작

`RegisterSynchronizer`는 K8s 리소스를 감시하여 KVStore에 반영하는 제네릭 함수이다:

```go
// clustermesh-apiserver/clustermesh/synchronizer.go (lines 76-217)
func RegisterSynchronizer[T runtime.Object](in syncParams[T]) {
    store := in.Factory.NewSyncStore(
        in.ClusterInfo.Name, in.Client,
        in.Options.Prefix, in.Options.StoreOpts...)

    // K8s Resource 이벤트 감시
    for {
        select {
        case event := <-resourceEvents:
            if event.Kind == resource.Sync {
                store.Synced(ctx, synced)  // 초기 동기화 완료
                continue
            }

            // Namespace 기반 필터링 (전역 네임스페이스만 허용)
            if event.Kind != resource.Delete && in.Options.Namespaced {
                isGlobal, _ := in.NamespaceManager.IsGlobalNamespaceByName(ns)
                if !isGlobal {
                    event.Kind = resource.Delete  // 비전역 네임스페이스 -> 삭제 처리
                }
            }

            upserts, deletes := in.Converter.Convert(event)
            for upsert := range upserts {
                store.UpsertKey(ctx, upsert)
            }
            for delete := range deletes {
                store.DeleteKey(ctx, delete)
            }

        case event := <-namespaceEvents:
            // 네임스페이스 변경 시 해당 리소스 재동기화
            for resEvent := range namespaceHandler(in, resourceStore, event) {
                upserts, deletes := in.Converter.Convert(resEvent)
                // ...
            }
        }
    }
}
```

### CiliumNode Converter 상세

```go
// clustermesh-apiserver/clustermesh/converters.go (lines 107-123)
func (nc *CiliumNodeConverter) Convert(event resource.Event[*cilium_api_v2.CiliumNode])
    (upserts iter.Seq[store.Key], deletes iter.Seq[store.NamedKey]) {

    if event.Kind == resource.Delete {
        node := nodeTypes.Node{Cluster: nc.cinfo.Name, Name: event.Key.Name}
        return noneIter[store.Key], singleIter[store.NamedKey](&node)
    }

    node := nodeTypes.ParseCiliumNode(event.Object)
    node.Cluster = nc.cinfo.Name
    node.ClusterID = nc.cinfo.ID
    return singleIter[store.Key](&node), noneIter[store.NamedKey]
}
```

### CiliumEndpoint Converter 상세

하나의 CiliumEndpoint가 여러 IP(IPv4, IPv6)를 가질 수 있으므로 `CachedConverter`를 사용한다:

```go
// clustermesh-apiserver/clustermesh/converters.go (lines 182-213)
func ciliumEndpointMapper(endpoint *types.CiliumEndpoint) iter.Seq[store.Key] {
    return func(yield func(store.Key) bool) {
        if n := endpoint.Networking; n != nil {
            for _, address := range n.Addressing {
                for _, ip := range []string{address.IPV4, address.IPV6} {
                    if ip == "" { continue }
                    entry := identity.IPIdentityPair{
                        IP:                net.ParseIP(ip),
                        HostIP:            net.ParseIP(n.NodeIP),
                        K8sNamespace:      endpoint.Namespace,
                        K8sPodName:        endpoint.Name,
                        K8sServiceAccount: endpoint.ServiceAccount,
                    }
                    if endpoint.Identity != nil {
                        entry.ID = identity.NumericIdentity(endpoint.Identity.ID)
                    }
                    if !yield(&entry) { return }
                }
            }
        }
    }
}
```

## 설정 관리

### 설정 디렉토리 감시

ClusterMesh 설정 파일들은 `/etc/cilium/clustermesh/` 디렉토리에 위치하며, `fsnotify`를 통해 실시간 감시된다:

```go
// pkg/clustermesh/common/config.go (lines 105-123)
type configDirectoryWatcher struct {
    logger     *slog.Logger
    watcher    *fsnotify.Watcher     // 디렉토리 감시
    cfgWatcher *fsnotify.Watcher     // 개별 파일 감시
    lifecycle  clusterLifecycle
    path       string
    tracked    map[string]fhash      // 파일명 -> SHA256 해시
    stop       chan struct{}
}
```

**왜 두 개의 watcher를 사용하는가?**

1. `watcher`: 디렉토리 수준 변경 감시 (파일 추가/삭제)
2. `cfgWatcher`: 개별 파일 감시 (심볼릭 링크가 가리키는 실제 파일 변경 감지)

Kubernetes ConfigMap/Secret이 심볼릭 링크로 마운트되기 때문에, 심볼릭 링크의 대상이 변경될 때 두 watcher의 이벤트 전달 방식이 다르다. `fsnotify`는 부모 디렉토리와 파일 모두 감시할 때 중복 이벤트를 제거하는데, 이 과정에서 심볼릭 링크 변경 이벤트가 누락될 수 있다.

### 설정 파일 검증

```go
// pkg/clustermesh/common/config.go (lines 155-171)
func isEtcdConfigFile(path string) (bool, fhash) {
    if info, err := os.Stat(path); err != nil || info.IsDir() {
        return false, fhash{}
    }

    b, err := os.ReadFile(path)
    if err != nil {
        return false, fhash{}
    }

    // "endpoints:" 문자열 포함 여부로 etcd 설정 파일 판별
    if strings.Contains(string(b), "endpoints:") {
        return true, sha256.Sum256(b)
    }

    return false, fhash{}
}
```

### 설정 파일 변경 처리 흐름

```
파일 시스템 변경 감지 (fsnotify)
         |
         v
     handle(abspath)
         |
         +---> isEtcdConfigFile()
         |         |
         |    [설정 파일 아님] ---> tracked에 있으면 remove() 호출
         |         |
         |    [설정 파일임]
         |         |
         +---> SHA256 해시 비교
         |         |
         |    [해시 동일] ---> 무시 (중복 이벤트 방지)
         |         |
         |    [해시 다름] ---> lifecycle.add(filename, path)
         |
         v
   common.clusterMesh.add()
         |
         +---> ValidateClusterName(name)
         +---> newRemoteCluster(name, path)
         +---> cluster.connect()
```

### 클러스터 추가/제거

```go
// pkg/clustermesh/common/clustermesh.go (lines 168-247)
func (cm *clusterMesh) add(name, path string) {
    // 자기 자신의 클러스터 설정은 무시
    if name == cm.conf.ClusterInfo.Name { return }

    // 클러스터 이름 검증
    if err := types.ValidateClusterName(name); err != nil { return }

    cm.mutex.Lock()
    defer cm.mutex.Unlock()
    cm.addLocked(name, path)
}

func (cm *clusterMesh) remove(name string) {
    cm.mutex.Lock()
    defer cm.mutex.Unlock()

    cluster, ok := cm.clusters[name]
    if !ok { return }

    // tombstone 등록 (비동기 정리)
    cm.tombstones[name] = ""
    delete(cm.clusters, name)

    cm.wg.Go(func() {
        cluster.onRemove(cm.rctx)

        cm.mutex.Lock()
        path := cm.tombstones[name]
        delete(cm.tombstones, name)

        // 정리 중 재추가 요청이 있었다면 지연 재생
        if path != "" {
            cm.addLocked(name, path)
        }
        cm.mutex.Unlock()
    })
}
```

**왜 tombstone 패턴을 사용하는가?** 원격 클러스터 제거는 캐시 정리, 연결 종료 등 시간이 걸리는 작업이다. 제거 중에 같은 클러스터의 설정 파일이 다시 추가되면 충돌이 발생할 수 있다. tombstone은 "이 클러스터가 제거 진행 중"임을 표시하고, 제거 완료 후 대기 중인 add 요청을 재생한다.

## 서비스 어피니티

### 개요

서비스 어피니티는 클러스터 간 트래픽 라우팅 선호도를 제어하는 기능이다:

| 어피니티 | 동작 |
|----------|------|
| `None` | 로컬 + 원격 백엔드 모두 사용 |
| `Local` | 로컬 백엔드 우선, 없으면 원격 사용 |
| `Remote` | 원격 백엔드 우선, 없으면 로컬 사용 |

### SelectBackends 구현

```go
// pkg/clustermesh/selectbackends.go (lines 31-92)
func (sb ClusterMeshSelectBackends) SelectBackends(txn statedb.ReadTxn,
    bes iter.Seq2[loadbalancer.BackendParams, statedb.Revision],
    svc *loadbalancer.Service,
    optionalFrontend *loadbalancer.Frontend,
) iter.Seq2[loadbalancer.BackendParams, statedb.Revision] {

    defaultBackends := sb.w.DefaultSelectBackends(txn, bes, svc, optionalFrontend)
    affinity := annotation.GetAnnotationServiceAffinity(svc)

    useLocal := true
    localActiveBackends := 0
    useRemote := false

    switch {
    case !annotation.GetAnnotationIncludeExternal(svc):
        useRemote = false  // 외부 포함이 비활성화면 원격 사용 안 함

    case affinity == annotation.ServiceAffinityNone:
        useRemote = true   // None이면 무조건 원격 포함

    default:
        // 로컬/원격 건강한 백엔드 수 계산
        localBackends, remoteBackends := 0, 0
        for be := range defaultBackends {
            healthy := be.State == loadbalancer.BackendStateActive ||
                       be.State == loadbalancer.BackendStateTerminating
            healthy = healthy && !be.Unhealthy
            if !healthy { continue }

            if be.Source == source.ClusterMesh {
                remoteBackends++
            } else {
                localBackends++
                if be.State == loadbalancer.BackendStateActive {
                    localActiveBackends++
                }
            }
        }

        switch affinity {
        case annotation.ServiceAffinityLocal:
            useLocal = true
            useRemote = localActiveBackends == 0 && remoteBackends > 0
        case annotation.ServiceAffinityRemote:
            useRemote = true
            useLocal = remoteBackends == 0 && localBackends > 0
        }
    }

    // 필터링된 백엔드 반환
    return func(yield func(loadbalancer.BackendParams, statedb.Revision) bool) {
        for be, rev := range defaultBackends {
            if be.Source == source.ClusterMesh {
                if !useRemote { continue }
            } else if !useLocal {
                continue
            }
            if !yield(be, rev) { break }
        }
    }
}
```

어피니티 결정 흐름:

```
IncludeExternal == false ?
  +---> YES: useRemote = false (원격 백엔드 사용 안 함)
  +---> NO:
          |
          Affinity == None ?
            +---> YES: useRemote = true (모두 사용)
            +---> NO:
                    |
                    건강한 백엔드 카운트
                    |
                    Affinity == Local ?
                      +---> useLocal = true
                      |     useRemote = (로컬 Active == 0 && 원격 > 0)
                      |     --> 로컬 장애 시에만 원격으로 페일오버
                      |
                    Affinity == Remote ?
                      +---> useRemote = true
                            useLocal = (원격 == 0 && 로컬 > 0)
                            --> 원격 장애 시에만 로컬로 페일오버
```

**왜 Terminating 상태도 카운트에 포함하는가?** Terminating 백엔드가 있다는 것은 "아직 연결을 처리 중인 백엔드가 있다"는 뜻이다. 이를 무시하면 페일오버가 불필요하게 트리거될 수 있다. Terminating 백엔드는 새 연결은 받지 않지만, 기존 연결을 유지하므로 건강한 것으로 간주한다.

## ClusterMesh 운영 모드

### 세 가지 모드

```go
// pkg/clustermesh/remote_cluster.go (lines 362-381)
const (
    ClusterMeshModeClusterMeshAPIServer       = "clustermesh-apiserver"
    ClusterMeshModeETCD                       = "etcd"
    ClusterMeshModeKVStoreMesh                = "kvstoremesh"
    ClusterMeshModeClusterMeshAPIServerOrETCD = "clustermesh-apiserver_or_etcd"
)

func ClusterMeshMode(rcc cmtypes.CiliumClusterConfig, identityMode string) string {
    switch {
    case rcc.Capabilities.Cached:
        return ClusterMeshModeKVStoreMesh
    case identityMode == option.IdentityAllocationModeCRD:
        return ClusterMeshModeClusterMeshAPIServer
    case identityMode == option.IdentityAllocationModeKVstore:
        return ClusterMeshModeETCD
    default:
        return ClusterMeshModeClusterMeshAPIServerOrETCD
    }
}
```

| 모드 | 조건 | 데이터 소스 |
|------|------|------------|
| `kvstoremesh` | Cached capability = true | 로컬 etcd (cilium/cache) |
| `clustermesh-apiserver` | CRD Identity 모드 | ClusterMesh API Server의 etcd |
| `etcd` | KVStore Identity 모드 | 직접 원격 etcd 연결 |
| `apiserver_or_etcd` | 그 외 | 혼합 모드 |

## 왜 이 아키텍처인가?

### 1. 왜 etcd 기반 상태 공유인가?

Kubernetes API Server를 직접 사용하지 않고 etcd를 중간 계층으로 두는 이유:

- **확장성**: K8s API Server는 단일 클러스터용으로 설계됨. 원격 클러스터의 모든 Agent가 직접 API Server에 연결하면 부하가 집중됨
- **추상화**: etcd의 `ListAndWatch` 패턴은 효율적인 변경 감시를 제공하며, KVStore 인터페이스로 추상화하여 구현을 교체 가능
- **선택적 공유**: 모든 K8s 리소스가 아닌 ClusterMesh에 필요한 리소스(노드, 서비스, Identity, 엔드포인트)만 선택적으로 동기화

### 2. 왜 KVStoreMesh를 분리했는가?

KVStoreMesh는 선택적 컴포넌트로, 직접 연결 모드와 캐시 모드를 모두 지원한다:

- **직접 연결**: 소규모 클러스터에서 단순성 우선, 추가 컴포넌트 불필요
- **KVStoreMesh**: 대규모 클러스터(수백 노드)에서 원격 etcd 부하 감소, 장애 격리 강화

### 3. 왜 Shared 플래그를 opt-in 방식으로 설계했는가?

- **보안**: 모든 서비스가 자동 공유되면 의도치 않은 서비스 노출 위험
- **성능**: 필요한 서비스만 공유하여 동기화 오버헤드 최소화
- **네임스페이스 격리**: K8s 네임스페이스 경계를 존중

### 4. 왜 Cluster ID에 제한이 있는가?

- **BPF 맵 제약**: Cluster ID는 BPF 맵의 키 또는 Identity 인코딩에 사용되며, 비트 수 제한이 있음
- **기본 255(8비트)**: 대부분의 사용 사례를 커버하면서 메모리 효율적
- **확장 511**: 9비트 필드를 사용하는 확장 모드로, 더 많은 클러스터 연결 필요 시 사용

### 5. 왜 fsnotify 기반 동적 설정인가?

- **무중단 운영**: Agent 재시작 없이 클러스터 추가/제거 가능
- **Kubernetes 통합**: ConfigMap/Secret 변경이 자동 반영 (심볼릭 링크 기반 업데이트)
- **SHA256 해시 비교**: 동일 내용의 중복 재처리 방지

### 6. 왜 tombstone 패턴을 사용하는가?

비동기 정리와 재추가의 경합 조건을 방지하기 위함:

```
시간 ----->

T1: remove("cluster-b") 호출
T2: onRemove() 시작 (캐시 정리, 연결 종료 등 - 수 분 소요)
T3: add("cluster-b") 호출 <-- 새 설정 파일 추가
    --> tombstone에 path 기록, 즉시 반환
T4: onRemove() 완료
    --> tombstone에서 path 꺼내기
    --> addLocked("cluster-b", path) 재생
```

## 참고 파일 목록

| 파일 경로 | 주요 내용 |
|-----------|----------|
| `pkg/clustermesh/clustermesh.go` | ClusterMesh 구조체, Configuration, NewClusterMesh, NewRemoteCluster, 동기화 대기 |
| `pkg/clustermesh/remote_cluster.go` | remoteCluster 구조체, Run(), Remove(), RevokeCache(), synced 구조체 |
| `pkg/clustermesh/common/services.go` | GlobalService, GlobalServiceCache, OnUpdate (Shared 필터링) |
| `pkg/clustermesh/store/store.go` | ClusterService 구조체, ServiceStorePrefix, Validators |
| `pkg/clustermesh/service_merger.go` | ServiceMerger 인터페이스, MergeExternalServiceUpdate/Delete |
| `pkg/clustermesh/idsmgr.go` | ClusterIDsManager, ReserveClusterID, ReleaseClusterID |
| `pkg/clustermesh/selectbackends.go` | SelectBackends, ServiceAffinity (Local/Remote/None) |
| `pkg/clustermesh/types/types.go` | ClusterIDMin/Max, CiliumClusterConfig, 이름 검증 |
| `pkg/clustermesh/common/config.go` | configDirectoryWatcher, fsnotify, isEtcdConfigFile |
| `pkg/clustermesh/common/clustermesh.go` | common.clusterMesh, add/remove, tombstone 패턴 |
| `pkg/kvstore/kvstore.go` | BaseKeyPrefix, StatePrefix, CachePrefix, StateToCachePrefix |
| `pkg/kvstore/backend.go` | BackendOperations 인터페이스 |
| `pkg/clustermesh/kvstoremesh/kvstoremesh.go` | KVStoreMesh 구조체, Config, newRemoteCluster |
| `pkg/clustermesh/kvstoremesh/remote_cluster.go` | KVStoreMesh remoteCluster, Run(), drain() |
| `pkg/clustermesh/kvstoremesh/reflector/reflector.go` | Reflector 인터페이스, Factory, prefix 매핑 |
| `clustermesh-apiserver/main.go` | ClusterMesh API Server 진입점 |
| `clustermesh-apiserver/clustermesh/cells.go` | Synchronization Cell, 리소스별 Synchronizer 등록 |
| `clustermesh-apiserver/clustermesh/synchronizer.go` | RegisterSynchronizer 제네릭 함수 |
| `clustermesh-apiserver/clustermesh/converters.go` | CiliumNode/Identity/Endpoint Converter |
