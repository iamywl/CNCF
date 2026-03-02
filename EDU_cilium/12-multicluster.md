# 12. Cilium 멀티클러스터 (ClusterMesh) 심층 분석

---

## 1. 개요

Cilium ClusterMesh는 여러 Kubernetes 클러스터를 하나의 논리적 네트워크로 연결하는 멀티클러스터 솔루션이다. 각 클러스터의 Cilium 에이전트가 원격 클러스터의 etcd를 통해 서비스, 엔드포인트, Identity 정보를 동기화하여, 클러스터 경계를 넘는 서비스 디스커버리와 네트워크 정책 적용을 가능하게 한다.

### 핵심 설계 원칙

| 원칙 | 설명 |
|------|------|
| **분산 제어 평면** | 중앙 컨트롤러 없이 각 클러스터가 독립적으로 동작 |
| **Cluster ID 기반 격리** | 클러스터별 고유 ID로 Identity 충돌 방지 |
| **etcd를 통한 상태 동기화** | kvstore 기반의 비동기 이벤트 기반 동기화 |
| **글로벌 서비스 모델** | 어노테이션 기반으로 서비스의 글로벌 공유 여부 결정 |

---

## 2. ClusterMesh 아키텍처

### 2.1 전체 구조

```
Cluster A (ID=1)                          Cluster B (ID=2)
+----------------------------+           +----------------------------+
|  cilium-agent              |           |  cilium-agent              |
|  +----------------------+  |           |  +----------------------+  |
|  | ClusterMesh          |  |           |  | ClusterMesh          |  |
|  |  - remoteCluster(B)  |  |           |  |  - remoteCluster(A)  |  |
|  |  - globalServices    |  |  etcd     |  |  - globalServices    |  |
|  |  - identityWatcher   |<------------>|  |  - identityWatcher   |  |
|  +----------------------+  |  watch    |  +----------------------+  |
|                            |           |                            |
|  clustermesh-apiserver     |           |  clustermesh-apiserver     |
|  +----------------------+  |           |  +----------------------+  |
|  | Synchronizer         |  |           |  | Synchronizer         |  |
|  |  K8s -> etcd 동기화  |  |           |  |  K8s -> etcd 동기화  |  |
|  +----------+-----------+  |           |  +----------+-----------+  |
|             |              |           |             |              |
|        etcd instance       |           |        etcd instance       |
+----------------------------+           +----------------------------+
```

### 2.2 핵심 컴포넌트

#### clustermesh-apiserver

`clustermesh-apiserver/`는 Kubernetes 리소스를 etcd에 동기화하는 별도의 바이너리로, 두 가지 모드를 지원한다:

| 모드 | 소스 경로 | 설명 |
|------|-----------|------|
| `clustermesh` | `clustermesh-apiserver/clustermesh/` | K8s 리소스를 직접 etcd로 동기화 |
| `kvstoremesh` | `clustermesh-apiserver/kvstoremesh/` | 원격 etcd를 로컬 etcd로 캐싱 (프록시) |

**엔트리포인트** (`clustermesh-apiserver/cmd/root.go`):

```go
// clustermesh-apiserver/cmd/root.go
func init() {
    RootCmd.AddCommand(
        clustermesh.NewCmd(hive.New(common.Cell, clustermesh.Cell)),
        kvstoremesh.NewCmd(hive.New(common.Cell, kvstoremesh.Cell)),
        // ...
    )
}
```

**Synchronizer** (`clustermesh-apiserver/clustermesh/synchronizer.go`):

Kubernetes 리소스(CiliumNode, CiliumIdentity, CiliumEndpoint, Service 등)를 감시하고 etcd의 적절한 prefix 아래에 동기화한다:

```go
// clustermesh-apiserver/clustermesh/synchronizer.go
func RegisterSynchronizer[T runtime.Object](in syncParams[T]) {
    store := in.Factory.NewSyncStore(
        in.ClusterInfo.Name, in.Client,
        in.Options.Prefix, in.Options.StoreOpts...)

    // Kubernetes 이벤트를 감시하여 etcd에 upsert/delete
    for event := range resourceEvents {
        upserts, deletes := in.Converter.Convert(event)
        for upsert := range upserts {
            store.UpsertKey(ctx, upsert)
        }
        for delete := range deletes {
            store.DeleteKey(ctx, delete)
        }
    }
}
```

동기화 대상 리소스 (`clustermesh-apiserver/clustermesh/cells.go`):

```go
// clustermesh-apiserver/clustermesh/cells.go - Synchronization 모듈
var Synchronization = cell.Module(
    "clustermesh-synchronization",
    "Synchronize information from Kubernetes to KVStore",
    // Service 동기화 (operator watcher 활용)
    operatorWatchers.ServiceSyncCell,
    // MCS API ServiceExport 동기화
    mcsapi.ServiceExportSyncCell,
    // CiliumNode 동기화
    cell.Invoke(RegisterSynchronizer[*cilium_api_v2.CiliumNode]),
    // CiliumIdentity 동기화
    cell.Invoke(RegisterSynchronizer[*cilium_api_v2.CiliumIdentity]),
    // CiliumEndpoint 동기화
    cell.Invoke(RegisterSynchronizer[*types.CiliumEndpoint]),
    // CiliumEndpointSlice 동기화
    cell.Invoke(RegisterSynchronizer[*cilium_api_v2a1.CiliumEndpointSlice]),
)
```

#### KVStoreMesh

KVStoreMesh는 원격 클러스터의 etcd 데이터를 로컬 etcd로 미러링하는 프록시 모드다. 이를 통해 각 Cilium 에이전트가 원격 etcd에 직접 연결하지 않아도 되므로, 대규모 클러스터에서 etcd 부하를 크게 줄일 수 있다.

```
                 KVStoreMesh 모드
Cluster A etcd -----> KVStoreMesh -----> Cluster B 로컬 etcd
                     (캐싱 프록시)           |
                                         cilium-agent (B)
                                         cilium-agent (B)
                                         cilium-agent (B)
```

소스: `pkg/clustermesh/kvstoremesh/kvstoremesh.go`

```go
// pkg/clustermesh/kvstoremesh/kvstoremesh.go
type KVStoreMesh struct {
    common common.ClusterMesh
    config Config
    client kvstore.Client          // 로컬 kvstore 인터페이스
    storeFactory store.Factory
    reflectorFactories []reflector.Factory
}
```

KVStoreMesh의 `remoteCluster.Run`에서는 원격 클러스터 설정을 가져와 `Cached: true`로 표시하고, 로컬 etcd에 전파한다 (`pkg/clustermesh/kvstoremesh/remote_cluster.go`):

```go
// pkg/clustermesh/kvstoremesh/remote_cluster.go
func (rc *remoteCluster) Run(ctx context.Context, backend kvstore.BackendOperations,
    srccfg types.CiliumClusterConfig, ready chan<- error) {
    var dstcfg = srccfg
    dstcfg.Capabilities.SyncedCanaries = true
    dstcfg.Capabilities.Cached = true  // KVStoreMesh가 캐싱함을 표시

    // 클러스터 설정을 로컬 etcd에 전파
    clustercfg.Enforce(ctx, rc.name, dstcfg, rc.localBackend, rc.logger)

    // Reflector 등록 후 원격 etcd 감시 시작
    for _, rfl := range rc.reflectors {
        rfl.Register(mgr, backend, srccfg)
    }
    mgr.Run(ctx)
}
```

---

## 3. ClusterMesh 에이전트 측 구현

### 3.1 ClusterMesh 구조체

`pkg/clustermesh/clustermesh.go`에서 정의된 `ClusterMesh`는 에이전트 측 멀티클러스터 로직의 핵심이다:

```go
// pkg/clustermesh/clustermesh.go
type ClusterMesh struct {
    conf Configuration
    common common.ClusterMesh
    globalServices *common.GlobalServiceCache  // 글로벌 서비스 캐시
    syncTimeoutLogOnce sync.Once
    FeatureMetrics ClusterMeshMetrics
}
```

생성 조건 -- ClusterID가 0이 아니고 ClusterMesh 설정 경로가 지정된 경우에만 활성화:

```go
func NewClusterMesh(lifecycle cell.Lifecycle, c Configuration) *ClusterMesh {
    if c.ClusterInfo.ID == 0 || c.ClusterMeshConfig == "" {
        return nil
    }
    // ...
}
```

### 3.2 원격 클러스터 연결

`pkg/clustermesh/common/remote_cluster.go`의 `remoteCluster`가 원격 etcd 연결을 관리한다:

```
[설정 파일 감시] --> [etcd 클라이언트 생성] --> [클러스터 설정 조회]
                                                      |
                                                      v
                                              [remoteCluster.Run]
                                                      |
                          +---------------------------+--+----------------------+
                          |                           |                         |
                  [노드 감시]                [서비스 감시]          [Identity 감시]
                  remoteNodes               remoteServices      remoteIdentityCache
                  WatchStore                WatchStore           RemoteIDCache
```

설정 디렉토리 감시(`pkg/clustermesh/common/config.go`)는 fsnotify를 사용하여 클러스터 설정 파일의 추가/변경/삭제를 실시간으로 감지한다:

```go
// pkg/clustermesh/common/config.go
type configDirectoryWatcher struct {
    watcher    *fsnotify.Watcher     // 디렉토리 감시
    cfgWatcher *fsnotify.Watcher     // 개별 파일 감시
    lifecycle  clusterLifecycle
    path       string
    tracked    map[string]fhash       // 파일 해시로 변경 감지
}
```

### 3.3 remoteCluster.Run 세부 흐름

`pkg/clustermesh/remote_cluster.go`에서 정의된 Run 메서드가 실제 동기화 로직을 실행한다:

```go
// pkg/clustermesh/remote_cluster.go
func (rc *remoteCluster) Run(ctx context.Context, backend kvstore.BackendOperations,
    config cmtypes.CiliumClusterConfig, ready chan<- error) {

    // 1. 클러스터 설정 유효성 검증
    rc.clusterConfigValidator(config)

    // 2. Cluster ID 업데이트 및 예약
    rc.onUpdateConfig(config)

    // 3. 원격 Identity 감시 시작
    remoteIdentityCache, _ := rc.remoteIdentityWatcher.WatchRemoteIdentities(
        rc.name, rc.clusterID, backend, config.Capabilities.Cached)

    // 4. WatchStoreManager를 통한 prefix별 감시 등록
    mgr.Register(nodeStore.NodeStorePrefix, func(ctx context.Context) {
        rc.remoteNodes.Watch(ctx, backend, path.Join(prefix, rc.name))
    })
    mgr.Register(serviceStore.ServiceStorePrefix, func(ctx context.Context) {
        rc.remoteServices.Watch(ctx, backend, path.Join(prefix, rc.name))
    })
    mgr.Register(ipcache.IPIdentitiesPath, func(ctx context.Context) {
        rc.ipCacheWatcher.Watch(ctx, backend, ...)
    })
    mgr.Register(identityCache.IdentitiesPath, func(ctx context.Context) {
        rc.remoteIdentityCache.Watch(ctx, ...)
    })

    // 5. 추가 옵저버 등록
    for _, obs := range rc.observers {
        obs.Register(mgr, backend, config)
    }

    mgr.Run(ctx)  // 감시 시작
}
```

---

## 4. Cluster ID와 Cluster Name 관리

### 4.1 ClusterInfo

`pkg/clustermesh/types/option.go`에서 정의된 `ClusterInfo`는 클러스터의 고유 식별 정보를 담는다:

```go
// pkg/clustermesh/types/option.go
type ClusterInfo struct {
    ID                   uint32  // 클러스터 고유 ID (1~255 또는 1~511)
    Name                 string  // 클러스터 이름 (최대 32자, 소문자 영숫자 + 하이픈)
    MaxConnectedClusters uint32  // 최대 연결 클러스터 수
}
```

### 4.2 Cluster ID 유효성 검증

```go
// pkg/clustermesh/types/types.go
const (
    ClusterIDMin    = 0
    ClusterIDExt511 = 511
    ClusterIDUnset  = ClusterIDMin
)

func ValidateClusterID(clusterID uint32) error {
    if clusterID == ClusterIDMin {
        return fmt.Errorf("ClusterID %d is reserved", ClusterIDMin)
    }
    if clusterID > ClusterIDMax {
        return fmt.Errorf("ClusterID > %d is not supported", ClusterIDMax)
    }
    return nil
}
```

### 4.3 Cluster ID 충돌 방지

`pkg/clustermesh/idsmgr.go`의 `ClusterMeshUsedIDs`가 연결된 클러스터들의 ID 고유성을 보장한다:

```go
// pkg/clustermesh/idsmgr.go
type ClusterMeshUsedIDs struct {
    localClusterID      uint32
    UsedClusterIDs      map[uint32]struct{}
    UsedClusterIDsMutex lock.RWMutex
}

func (cm *ClusterMeshUsedIDs) ReserveClusterID(clusterID uint32) error {
    // ClusterIDUnset(0)은 예약됨
    // 로컬 클러스터 ID와 동일하면 거부
    // 이미 사용 중인 ID면 거부
    if _, ok := cm.UsedClusterIDs[clusterID]; ok {
        return fmt.Errorf("clusterID %d is already used", clusterID)
    }
    cm.UsedClusterIDs[clusterID] = struct{}{}
    return nil
}
```

### 4.4 Identity에서의 ClusterID 역할

Cilium의 Security Identity는 24비트 숫자이며, 상위 비트에 ClusterID가 인코딩된다. 이를 통해 서로 다른 클러스터에서 같은 라벨 집합을 가진 워크로드라도 고유한 Identity를 할당받을 수 있다:

```
Identity 구조 (MaxConnectedClusters=255):
+----------+------------------------+
| ClusterID|     Local Identity     |
| (8 bits) |      (16 bits)         |
+----------+------------------------+

Identity 구조 (MaxConnectedClusters=511):
+----------+------------------------+
| ClusterID|     Local Identity     |
| (9 bits) |      (15 bits)         |
+----------+------------------------+
```

---

## 5. 글로벌 서비스 (Global Service)

### 5.1 어노테이션 기반 제어

서비스를 클러스터 간에 공유하려면 아래 어노테이션을 설정한다 (`pkg/annotation/k8s.go`):

| 어노테이션 | 별칭 | 기본값 | 설명 |
|-----------|------|--------|------|
| `service.cilium.io/global` | `io.cilium/global-service` | `false` | 글로벌 서비스로 표시 |
| `service.cilium.io/shared` | `io.cilium/shared-service` | `true` (global일 때) | 로컬 백엔드를 원격에 공유 |
| `service.cilium.io/affinity` | `io.cilium/service-affinity` | `none` | 백엔드 선호도 (local/remote/none) |
| `service.cilium.io/global-sync-endpoint-slices` | - | `false` | EndpointSlice 동기화 활성화 |

```yaml
apiVersion: v1
kind: Service
metadata:
  name: my-service
  annotations:
    service.cilium.io/global: "true"    # 글로벌 서비스 활성화
    service.cilium.io/shared: "true"    # 로컬 백엔드 공유
    service.cilium.io/affinity: "local" # 로컬 우선
```

### 5.2 GlobalServiceCache

`pkg/clustermesh/common/services.go`에서 `GlobalServiceCache`가 모든 클러스터의 서비스 정보를 통합 관리한다:

```go
// pkg/clustermesh/common/services.go
type GlobalService struct {
    ClusterServices map[string]*serviceStore.ClusterService  // cluster-name -> service
}

type GlobalServiceCache struct {
    mutex  lock.RWMutex
    byName map[types.NamespacedName]*GlobalService
}

func (c *GlobalServiceCache) OnUpdate(svc *serviceStore.ClusterService) {
    globalSvc, ok := c.byName[svc.NamespaceServiceName()]
    if !ok {
        globalSvc = newGlobalService()
        c.byName[svc.NamespaceServiceName()] = globalSvc
    }
    globalSvc.ClusterServices[svc.Cluster] = svc
}
```

### 5.3 ClusterService 데이터 모델

`pkg/clustermesh/store/store.go`에 정의된 `ClusterService`는 kvstore에 저장되는 서비스 정보이다:

```go
// pkg/clustermesh/store/store.go
type ClusterService struct {
    Cluster         string                         // 소속 클러스터 이름
    Namespace       string                         // 네임스페이스
    Name            string                         // 서비스 이름
    Frontends       map[string]PortConfiguration   // 프론트엔드 IP -> 포트 설정
    Backends        map[string]PortConfiguration   // 백엔드 IP -> 포트 설정
    Labels          map[string]string              // 서비스 라벨
    Selector        map[string]string              // 백엔드 셀렉터
    IncludeExternal bool                           // 외부 엔드포인트 포함 여부
    Shared          bool                           // 다른 클러스터에 공유 여부
    ClusterID       uint32                         // 소속 클러스터 ID
}

// kvstore 키 형식: cilium/state/services/v1/{cluster}/{namespace}/{name}
func (s *ClusterService) GetKeyName() string {
    return path.Join(s.Cluster, s.Namespace, s.Name)
}
```

### 5.4 서비스 백엔드 선택 로직

`pkg/clustermesh/selectbackends.go`에서 `ServiceAffinity` 어노테이션에 따른 백엔드 선택 로직이 구현된다:

```go
// pkg/clustermesh/selectbackends.go
func (sb ClusterMeshSelectBackends) SelectBackends(...) iter.Seq2[...] {
    switch {
    case !annotation.GetAnnotationIncludeExternal(svc):
        useRemote = false  // global 아니면 원격 백엔드 미사용

    case affinity == annotation.ServiceAffinityNone:
        useRemote = true   // affinity 없으면 모든 백엔드 사용

    case affinity == ServiceAffinityLocal:
        useLocal = true
        useRemote = localActiveBackends == 0  // 로컬 건강한 백엔드 없으면 원격 사용

    case affinity == ServiceAffinityRemote:
        useRemote = true
        useLocal = remoteBackends == 0  // 원격 백엔드 없으면 로컬 사용
    }
}
```

### 5.5 서비스 병합 (ServiceMerger)

`pkg/clustermesh/service_merger.go`에서 원격 클러스터의 서비스 백엔드를 로컬 로드밸런서에 병합한다:

```go
// pkg/clustermesh/service_merger.go
func (sm *serviceMerger) MergeExternalServiceUpdate(service *serviceStore.ClusterService) {
    name := loadbalancer.NewServiceName(service.Namespace, service.Name)
    txn := sm.writer.WriteTxn()
    defer txn.Commit()
    sm.writer.SetBackendsOfCluster(
        txn, name, source.ClusterMesh, service.ClusterID,
        ClusterServiceToBackendParams(service)...,
    )
}
```

---

## 6. 크로스 클러스터 Identity 동기화

### 6.1 RemoteIdentityWatcher 인터페이스

```go
// pkg/clustermesh/clustermesh.go
type RemoteIdentityWatcher interface {
    WatchRemoteIdentities(remoteName string, remoteID uint32,
        backend kvstore.BackendOperations, cachedPrefix bool) (allocator.RemoteIDCache, error)
    RemoveRemoteIdentities(name string)
}
```

### 6.2 Identity 저장 경로

```go
// pkg/identity/cache/allocator.go
var IdentitiesPath = path.Join(kvstore.BaseKeyPrefix, "state", "identities", "v1")
// 실제 경로: cilium/state/identities/v1/
```

### 6.3 동기화 흐름

```
Cluster A                                    Cluster B
+--------------------------+                +--------------------------+
| CiliumIdentity CR 생성   |                |                          |
|         |                |                |                          |
|         v                |                |                          |
| clustermesh-apiserver    |                |                          |
| (Identity Synchronizer) |                |                          |
|         |                |                |                          |
|         v                |                |                          |
| etcd A: cilium/state/    |  Watch         |                          |
|   identities/v1/id-xxx  |<-------------->| remoteIdentityCache      |
|                          |                |    |                     |
|                          |                |    v                     |
|                          |                | 로컬 Identity 캐시 갱신  |
|                          |                | IPCache 업데이트         |
+--------------------------+                +--------------------------+
```

원격 Identity 캐시는 `remoteCluster.Run` 내에서 시작된다:

```go
// pkg/clustermesh/remote_cluster.go (Run 내부)
remoteIdentityCache, err := rc.remoteIdentityWatcher.WatchRemoteIdentities(
    rc.name, rc.clusterID, backend, config.Capabilities.Cached)

// Identity prefix 감시 등록
mgr.Register(adapter(identityCache.IdentitiesPath), func(ctx context.Context) {
    rc.remoteIdentityCache.Watch(ctx, func(context.Context) {
        rc.synced.identitiesDone()
    })
})
```

---

## 7. 크로스 클러스터 정책 적용 (CCNP)

### 7.1 CiliumClusterwideNetworkPolicy (CCNP)

CCNP는 CiliumNetworkPolicy의 클러스터 범위 버전으로, 모든 네임스페이스에 적용된다. ClusterMesh 환경에서는 원격 클러스터의 Identity를 참조하는 정책을 작성할 수 있다.

### 7.2 정책에서의 클러스터 선택

`pkg/clustermesh/types/option.go`에서 정책의 기본 클러스터 범위를 제어한다:

```go
// pkg/clustermesh/types/option.go
type PolicyConfig struct {
    PolicyDefaultLocalCluster bool  // true면 정책이 기본적으로 로컬 클러스터만 대상
}

func LocalClusterNameForPolicies(cfg PolicyConfig, localClusterName string) string {
    if cfg.PolicyDefaultLocalCluster {
        return localClusterName  // 로컬 클러스터로 제한
    }
    return PolicyAnyCluster      // 모든 클러스터 포함 ("")
}
```

### 7.3 크로스 클러스터 정책 예시

```yaml
apiVersion: "cilium.io/v2"
kind: CiliumNetworkPolicy
metadata:
  name: allow-cross-cluster
spec:
  endpointSelector:
    matchLabels:
      app: backend
  ingress:
    - fromEndpoints:
        - matchLabels:
            app: frontend
            io.cilium.k8s.policy.cluster: cluster-a  # 특정 클러스터 지정
```

---

## 8. MCS API (Multi-Cluster Service API) 지원

### 8.1 개요

Cilium은 Kubernetes SIG Multicluster의 MCS API (Multi-Cluster Service API)를 지원한다. 이는 `ServiceExport`와 `ServiceImport` CRD를 통해 서비스를 클러스터 간에 표준화된 방식으로 공유하는 메커니즘이다.

### 8.2 구현 컴포넌트

| 컴포넌트 | 소스 경로 | 역할 |
|----------|-----------|------|
| ServiceExport 동기화 | `pkg/clustermesh/mcsapi/serviceexportsync.go` | ServiceExport를 kvstore에 동기화 |
| ServiceImport 컨트롤러 | `pkg/clustermesh/mcsapi/serviceimport_controller.go` | ServiceImport에서 derived Service 생성 |
| Service 컨트롤러 | `pkg/clustermesh/mcsapi/service_controller.go` | ServiceImport에서 derived Service 관리 |

### 8.3 동작 흐름

```
Cluster A                                    Cluster B
+---------------------------+               +---------------------------+
| Service + ServiceExport   |               |                           |
|          |                |               |                           |
|          v                |               |                           |
| ServiceExportSync         |  etcd sync    |                           |
| (kvstore에 export 정보)   |<------------>| ServiceImportReconciler   |
|                           |               |          |                |
|                           |               |          v                |
|                           |               | ServiceImport 생성        |
|                           |               |          |                |
|                           |               |          v                |
|                           |               | Derived Service 생성      |
|                           |               | (io.cilium/global-service)|
+---------------------------+               +---------------------------+
```

ServiceImport 컨트롤러는 ServiceImport로부터 derived Service를 생성하며, 이때 Cilium의 글로벌 서비스 어노테이션을 자동으로 추가한다:

```go
// pkg/clustermesh/mcsapi/service_controller.go
// derived Service는 기존 ClusterMesh 인프라를 활용하기 위해
// Cilium의 글로벌 서비스 어노테이션이 자동으로 추가된다.
func derivedName(name types.NamespacedName) string {
    hash := sha256.New()
    hash.Write([]byte(name.String()))
    return "derived-" + strings.ToLower(
        base32.HexEncoding.WithPadding(base32.NoPadding).EncodeToString(hash.Sum(nil)))[:10]
}
```

### 8.4 CiliumClusterConfig의 MCS API 지원 플래그

```go
// pkg/clustermesh/types/types.go
type CiliumClusterConfigCapabilities struct {
    SyncedCanaries        bool   // prefix별 동기화 카나리 지원
    Cached                bool   // KVStoreMesh 캐싱 모드
    MaxConnectedClusters  uint32 // 최대 연결 클러스터 수
    ServiceExportsEnabled *bool  // MCS API ServiceExports 활성화 여부
}
```

---

## 9. 원격 etcd 연결 및 WatcherQueue

### 9.1 etcd 연결 흐름

`pkg/clustermesh/common/remote_cluster.go`에서 원격 etcd 연결이 관리된다:

```go
// pkg/clustermesh/common/remote_cluster.go
func (rc *remoteCluster) restartRemoteConnection() {
    rc.controllers.UpdateController(
        rc.remoteConnectionControllerName,
        controller.ControllerParams{
            DoFunc: func(ctx context.Context) error {
                // 1. 이전 연결 해제
                rc.releaseOldConnection()

                // 2. 새 etcd 클라이언트 생성 (설정 파일 기반)
                backend, errChan := rc.remoteClientFactory(ctx, rc.logger,
                    rc.configPath, extraOpts)

                // 3. 연결 성공 대기
                err := <-errChan

                // 4. 클러스터 설정 조회
                config, err := rc.getClusterConfig(ctx, backend)

                // 5. remoteCluster.Run 실행 (장기 실행)
                go func() {
                    rc.Run(ctx, backend, config, ready)
                }()

                return nil
            },
            CancelDoFuncOnUpdate: true,
        },
    )
}
```

### 9.2 Watcher Cache

`pkg/kvstore/watcher_cache.go`에서 kvstore 감시 캐시가 구현된다:

```go
// pkg/kvstore/watcher_cache.go
type watcherCache map[string]watchState

type watchState struct {
    deletionMark bool
}

// MarkAllForDeletion: 재연결 시 모든 키를 삭제 후보로 표시
func (wc watcherCache) MarkAllForDeletion() {
    for k := range wc {
        wc[k] = watchState{deletionMark: true}
    }
}

// MarkInUse: List 결과에 포함된 키는 삭제 표시 해제
func (wc watcherCache) MarkInUse(key []byte) {
    wc[string(key)] = watchState{deletionMark: false}
}

// RemoveDeleted: 삭제 표시된 키 정리 (stale 키 제거)
func (wc watcherCache) RemoveDeleted(f func(string) bool) bool { ... }
```

이 패턴은 etcd Watch 재연결 시 stale 데이터를 정리하는 데 사용된다:

```
1. 재연결 시 기존 캐시의 모든 키를 "삭제 후보"로 표시
2. List 결과에 포함된 키는 "사용 중"으로 표시
3. 여전히 "삭제 후보"인 키는 실제로 삭제 (이벤트 발생)
```

### 9.3 TTL 기반 캐시 무효화

원격 클러스터 연결이 끊어졌을 때 stale 데이터를 방지하기 위한 TTL 체커 (`pkg/clustermesh/common/remote_cluster.go`):

```go
// pkg/clustermesh/common/remote_cluster.go
type cacheTTLChecker struct {
    ttl          time.Duration
    onExpiration func(context.Context)  // TTL 만료 시 캐시 무효화
    checking     bool
}

// TTL 만료 시 RevokeCache 호출 -> 서비스 캐시 드레인
func (rc *remoteCluster) RevokeCache(ctx context.Context) {
    rc.remoteServices.Drain()
    for _, obs := range rc.observers {
        obs.Revoke()
    }
}
```

---

## 10. pkg/clustermesh/ 패키지 구조

```
pkg/clustermesh/
├── clustermesh.go              # ClusterMesh 메인 구조체 (에이전트 측)
├── remote_cluster.go           # 원격 클러스터 비즈니스 로직
├── idsmgr.go                   # Cluster ID 관리 (충돌 방지)
├── service_merger.go           # 원격 서비스 백엔드 병합
├── selectbackends.go           # ServiceAffinity 기반 백엔드 선택
├── cell.go                     # Hive 셀 정의
├── metrics.go                  # 메트릭
├── notifier.go                 # 이벤트 알림
├── common/                     # 에이전트/operator/kvstoremesh 공통 로직
│   ├── clustermesh.go          # 공통 ClusterMesh (설정 감시, 연결 관리)
│   ├── remote_cluster.go       # 원격 etcd 연결 관리
│   ├── config.go               # 설정 디렉토리 fsnotify 감시
│   ├── services.go             # GlobalServiceCache 구현
│   ├── interceptor.go          # gRPC 인터셉터 (클러스터 잠금)
│   ├── factory.go              # RemoteClientFactory
│   └── metrics.go              # 공통 메트릭
├── types/                      # 타입 정의
│   ├── types.go                # ClusterID 유효성, CiliumClusterConfig
│   └── option.go               # ClusterInfo, PolicyConfig
├── store/                      # kvstore 서비스 모델
│   └── store.go                # ClusterService 정의 (Stable API)
├── kvstoremesh/                # KVStoreMesh 구현
│   ├── kvstoremesh.go          # KVStoreMesh 메인 구조체
│   ├── remote_cluster.go       # 원격 -> 로컬 반사
│   └── reflector/              # prefix별 반사기
├── mcsapi/                     # MCS API 지원
│   ├── serviceexportsync.go    # ServiceExport 동기화
│   ├── serviceimport_controller.go  # ServiceImport 컨트롤러
│   └── service_controller.go   # derived Service 관리
├── observer/                   # 추가 옵저버 인터페이스
│   └── observer.go             # Observer, Factory 정의
├── wait/                       # 동기화 대기 유틸리티
├── operator/                   # 오퍼레이터 측 ClusterMesh
├── namespace/                  # 네임스페이스 관리
└── endpointslicesync/          # EndpointSlice 동기화
```

---

## 11. 동기화 상태 추적

### 11.1 Synced 구조체

`pkg/clustermesh/remote_cluster.go`에서 각 리소스 타입별로 동기화 완료를 추적한다:

```go
// pkg/clustermesh/remote_cluster.go
type synced struct {
    wait.SyncedCommon
    services       chan struct{}             // 서비스 초기 동기화 완료
    nodes          chan struct{}             // 노드 초기 동기화 완료
    ipcache        chan struct{}             // IPCache 초기 동기화 완료
    identities     *lock.StoppableWaitGroup  // Identity 초기 동기화 완료
    observers      map[observer.Name]chan struct{}  // 추가 옵저버별 동기화
}
```

### 11.2 SyncedCanaries

원격 클러스터가 `SyncedCanaries` 기능을 지원하면 WatchStoreManager가 prefix별 카나리 키를 감시하여, 해당 prefix의 데이터가 완전히 기록되었는지 확인한 후에야 감시를 시작한다.

```go
// pkg/clustermesh/remote_cluster.go (Run 내부)
var mgr store.WatchStoreManager
if config.Capabilities.SyncedCanaries {
    mgr = rc.storeFactory.NewWatchStoreManager(backend, rc.name)
} else {
    mgr = store.NewWatchStoreManagerImmediate(rc.log)
}
```

---

## 12. 주요 kvstore 경로

| 경로 | 용도 | 소스 |
|------|------|------|
| `cilium/state/nodes/v1/{cluster}/{node}` | 노드 정보 | `pkg/node/store/` |
| `cilium/state/services/v1/{cluster}/{ns}/{svc}` | 서비스 정보 | `pkg/clustermesh/store/store.go` |
| `cilium/state/identities/v1/{id}` | Security Identity | `pkg/identity/cache/allocator.go` |
| `cilium/state/ip/v1/{ip}` | IP-Identity 매핑 | `pkg/ipcache/` |
| `cilium/cluster-config/{cluster}` | 클러스터 설정 | `pkg/clustermesh/clustercfg/` |
| `cilium/synced/{cluster}/{prefix}` | 동기화 카나리 | WatchStoreManager |
| `cilium/cache/...` | KVStoreMesh 캐시 | KVStoreMesh |

---

## 13. ClusterMesh 연결 모드 비교

```go
// pkg/clustermesh/remote_cluster.go
const (
    ClusterMeshModeClusterMeshAPIServer = "clustermesh-apiserver"
    ClusterMeshModeETCD                 = "etcd"
    ClusterMeshModeKVStoreMesh          = "kvstoremesh"
)

func ClusterMeshMode(rcc cmtypes.CiliumClusterConfig, identityMode string) string {
    switch {
    case rcc.Capabilities.Cached:
        return ClusterMeshModeKVStoreMesh       // KVStoreMesh 캐싱 모드
    case identityMode == "crd":
        return ClusterMeshModeClusterMeshAPIServer  // CRD 기반
    case identityMode == "kvstore":
        return ClusterMeshModeETCD              // 직접 etcd 연결
    }
}
```

| 모드 | 에이전트 연결 대상 | Identity 관리 | 확장성 |
|------|-------------------|---------------|--------|
| Direct etcd | 원격 클러스터 etcd | kvstore | 소규모 |
| clustermesh-apiserver | 로컬 etcd (apiserver 동기화) | CRD | 중규모 |
| KVStoreMesh | 로컬 etcd (KVStoreMesh 캐싱) | CRD | 대규모 |

---

## 14. 장애 처리

### 14.1 연결 끊김 시 동작

| 상황 | 동작 |
|------|------|
| 원격 etcd 일시 장애 | 컨트롤러가 자동 재연결 시도 |
| TTL 만료 | 서비스 캐시 무효화 (stale 방지) |
| 클러스터 설정 제거 | 모든 캐시 드레인 + Identity 제거 |
| KVStoreMesh 연결 끊김 | 캐시된 데이터 제거 (grace period 후) |

### 14.2 에이전트 재시작 시 동작

에이전트가 재시작할 때는 기존 연결을 정리하지만, 캐시 데이터를 즉시 드레인하지 않는다. 이는 재시작 중에도 기존 연결이 유지되도록 하기 위함이다:

```go
// pkg/clustermesh/remote_cluster.go
func (rc *remoteCluster) Remove(context.Context) {
    // 설정 제거 시에만 드레인 수행
    rc.remoteNodes.Drain()
    rc.remoteServices.Drain()
    rc.ipCacheWatcher.Drain()
    rc.remoteIdentityWatcher.RemoveRemoteIdentities(rc.name)
}
```

---

## 15. 참조 소스 파일 요약

| 파일 경로 | 핵심 역할 |
|-----------|-----------|
| `pkg/clustermesh/clustermesh.go` | ClusterMesh 메인 구조체, 에이전트 측 진입점 |
| `pkg/clustermesh/remote_cluster.go` | 원격 클러스터 비즈니스 로직 (Run, Sync) |
| `pkg/clustermesh/idsmgr.go` | Cluster ID 예약/해제 관리 |
| `pkg/clustermesh/service_merger.go` | 원격 서비스 백엔드 병합 |
| `pkg/clustermesh/selectbackends.go` | ServiceAffinity 기반 백엔드 선택 |
| `pkg/clustermesh/common/clustermesh.go` | 공통 ClusterMesh 로직 (연결 관리) |
| `pkg/clustermesh/common/remote_cluster.go` | etcd 연결, 재연결, watchdog |
| `pkg/clustermesh/common/config.go` | 설정 디렉토리 fsnotify 감시 |
| `pkg/clustermesh/common/services.go` | GlobalServiceCache |
| `pkg/clustermesh/types/types.go` | ClusterID 검증, CiliumClusterConfig |
| `pkg/clustermesh/types/option.go` | ClusterInfo, PolicyConfig |
| `pkg/clustermesh/store/store.go` | ClusterService 모델 (Stable API) |
| `pkg/clustermesh/kvstoremesh/kvstoremesh.go` | KVStoreMesh 메인 로직 |
| `pkg/clustermesh/kvstoremesh/remote_cluster.go` | KVStoreMesh 원격 클러스터 반사 |
| `pkg/clustermesh/mcsapi/serviceexportsync.go` | MCS API ServiceExport 동기화 |
| `pkg/clustermesh/mcsapi/serviceimport_controller.go` | MCS API ServiceImport 관리 |
| `pkg/clustermesh/observer/observer.go` | 추가 옵저버 인터페이스 |
| `pkg/kvstore/watcher_cache.go` | kvstore Watch 캐시 (stale 정리) |
| `pkg/kvstore/etcd.go` | etcd 백엔드 구현 |
| `pkg/identity/cache/allocator.go` | Identity 할당기 |
| `pkg/annotation/k8s.go` | 글로벌 서비스 어노테이션 정의 |
| `clustermesh-apiserver/clustermesh/cells.go` | apiserver Hive 셀 구성 |
| `clustermesh-apiserver/clustermesh/synchronizer.go` | K8s -> etcd 동기화 프레임워크 |
| `clustermesh-apiserver/cmd/root.go` | apiserver 엔트리포인트 |
