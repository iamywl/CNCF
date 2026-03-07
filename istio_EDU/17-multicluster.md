# 17. 멀티클러스터 서비스 메시 Deep Dive

## 목차
1. [멀티클러스터 토폴로지](#1-멀티클러스터-토폴로지)
2. [클러스터 레지스트리: Secret 기반 원격 클러스터 등록](#2-클러스터-레지스트리-secret-기반-원격-클러스터-등록)
3. [서비스 병합: Aggregate Controller의 ClusterVIPs 병합](#3-서비스-병합-aggregate-controller의-clustervips-병합)
4. [엔드포인트 병합: 크로스 클러스터 엔드포인트 디스커버리](#4-엔드포인트-병합-크로스-클러스터-엔드포인트-디스커버리)
5. [네트워크 게이트웨이: East-West 게이트웨이](#5-네트워크-게이트웨이-east-west-게이트웨이)
6. [Locality 로드밸런싱](#6-locality-로드밸런싱)
7. [DNS 프록시: 원격 클러스터 서비스 해석](#7-dns-프록시-원격-클러스터-서비스-해석)
8. [트러스트 도메인: SPIFFE 크로스 클러스터 인증](#8-트러스트-도메인-spiffe-크로스-클러스터-인증)
9. [MCS API: ServiceExport/ServiceImport](#9-mcs-api-serviceexportserviceimport)
10. [설계 이유: 왜 이렇게 설계했는가](#10-설계-이유-왜-이렇게-설계했는가)

---

## 1. 멀티클러스터 토폴로지

### 1.1 핵심 개념

Istio의 멀티클러스터 아키텍처는 두 가지 독립적인 축으로 정의된다.

1. **컨트롤 플레인 토폴로지**: Istiod가 어디에 배포되는가 (Primary vs Remote)
2. **네트워크 모델**: 클러스터 간 Pod 직접 통신이 가능한가 (Single Network vs Multi Network)

이 두 축의 조합으로 네 가지 배포 모델이 만들어진다.

```
                     ┌─────────────────────────────────────────────┐
                     │          컨트롤 플레인 토폴로지              │
                     ├──────────────────┬──────────────────────────┤
                     │   Primary-Remote │     Multi-Primary        │
    ┌────────────────┼──────────────────┼──────────────────────────┤
    │                │                  │                          │
    │ Single Network │  [모델 A]        │  [모델 B]                │
    │ (Pod 직접통신)  │  하나의 Istiod가 │  각 클러스터에 Istiod     │
    │                │  모든 클러스터    │  상호 감시(Watch)         │
    │                │  관리            │                          │
    ├────────────────┼──────────────────┼──────────────────────────┤
    │                │                  │                          │
    │ Multi Network  │  [모델 C]        │  [모델 D]                │
    │ (E/W GW 필요)  │  Primary에만     │  각 클러스터에 Istiod +   │
    │                │  Istiod +        │  East-West 게이트웨이     │
    │                │  E/W 게이트웨이  │                          │
    │                │                  │                          │
    └────────────────┴──────────────────┴──────────────────────────┘
```

### 1.2 Primary-Remote 모델

Primary 클러스터에만 Istiod(컨트롤 플레인)가 배포되고, Remote 클러스터의 사이드카 프록시들은 Primary의 Istiod에 연결한다.

```
    Primary Cluster                Remote Cluster
   ┌──────────────────┐          ┌──────────────────┐
   │                  │          │                  │
   │  ┌────────────┐  │          │                  │
   │  │  Istiod    │◄─┼──────── ┤  Envoy Proxies   │
   │  │            │  │  xDS     │  (sidecar)       │
   │  └─────┬──────┘  │          │                  │
   │        │Watch    │          │                  │
   │        ▼         │          │                  │
   │  K8s API Server  │          │  K8s API Server  │
   │                  │  Secret  │                  │
   │  Remote Secret ──┼─kubeconfig─► API 접근      │
   └──────────────────┘          └──────────────────┘
```

핵심 코드 위치: `pilot/pkg/serviceregistry/kube/controller/multicluster.go`

```go
// Multicluster structure holds the remote kube Controllers and multicluster specific attributes.
type Multicluster struct {
    serverID string
    opts     Options
    s        server.Instance
    serviceEntryController *serviceentry.Controller
    clusterLocal           model.ClusterLocalProvider
    distributeCACert       bool
    caBundleWatcher        *keycertbundle.Watcher
    revision               string
    component *multicluster.Component[*kubeController]
}
```

### 1.3 Multi-Primary 모델

각 클러스터에 독립적인 Istiod가 배포되며, 각 Istiod는 모든 클러스터의 API 서버를 감시한다. 고가용성을 위한 가장 일반적인 프로덕션 배포 방식이다.

```
    Cluster A                      Cluster B
   ┌──────────────────┐          ┌──────────────────┐
   │  ┌────────────┐  │          │  ┌────────────┐  │
   │  │  Istiod-A  │◄─┼── xDS ──┼──│  Istiod-B  │  │
   │  │            │──┼── Watch ─┼─►│            │  │
   │  └────────────┘  │          │  └────────────┘  │
   │  Watch ↕         │          │         ↕ Watch  │
   │  K8s API Server  │          │  K8s API Server  │
   │                  │          │                  │
   │  Remote Secret ──┼──────────┼─► B의 kubeconfig │
   │  B의 kubeconfig ◄┼──────────┼── Remote Secret  │
   └──────────────────┘          └──────────────────┘
```

### 1.4 네트워크 모델

**단일 네트워크(Single Network)**: 모든 클러스터의 Pod이 서로 직접 IP로 통신할 수 있다. 같은 VPC, 같은 flat network에 있는 경우이다.

**다중 네트워크(Multi Network)**: 클러스터 간 Pod IP가 라우팅되지 않는다. East-West 게이트웨이를 통해 트래픽을 터널링해야 한다.

네트워크 식별은 `pkg/network/id.go`에서 정의된 타입으로 관리한다.

```go
// pkg/network/id.go
type ID string

func (id ID) Equals(other ID) bool {
    return identifier.IsSameOrEmpty(string(id), string(other))
}
```

네트워크 할당은 세 가지 방법으로 이루어진다.

| 방법 | 설정 위치 | 우선순위 |
|------|----------|---------|
| 시스템 네임스페이스 레이블 | `topology.istio.io/network` on `istio-system` NS | 기본값 |
| MeshNetworks 설정 | `meshNetworks.networks[name].endpoints[].fromRegistry` | MeshConfig |
| CIDR 기반 | `meshNetworks.networks[name].endpoints[].fromCidr` | IP 매칭 |

---

## 2. 클러스터 레지스트리: Secret 기반 원격 클러스터 등록

### 2.1 Secret Controller 아키텍처

원격 클러스터 등록의 핵심은 `pkg/kube/multicluster/secretcontroller.go`에 구현된 `Controller`다. 이 컨트롤러는 `istio/multiCluster=true` 레이블이 붙은 Kubernetes Secret을 감시하여 원격 클러스터를 동적으로 추가/제거한다.

```
   istio-system 네임스페이스
  ┌──────────────────────────────────────────────┐
  │                                              │
  │   Secret (istio/multiCluster=true)           │
  │  ┌────────────────────────────────────────┐  │
  │  │ data:                                  │  │
  │  │   cluster-a: <kubeconfig-a-base64>     │  │
  │  │   cluster-b: <kubeconfig-b-base64>     │  │
  │  │   cluster-c: <kubeconfig-c-base64>     │  │
  │  └────────────────────────────────────────┘  │
  │         │                                    │
  │         ▼                                    │
  │  ┌─────────────────────┐                     │
  │  │  Secret Controller  │                     │
  │  │  (multicluster pkg) │                     │
  │  └──────┬──────────────┘                     │
  │         │                                    │
  └─────────┼────────────────────────────────────┘
            │
            ▼
  ┌──────────────────────────────┐
  │  각 클러스터별 kube.Client   │
  │  + Service Registry 생성    │
  │  + Informer 시작             │
  └──────────────────────────────┘
```

### 2.2 Secret Controller의 초기화

```go
// pkg/kube/multicluster/secretcontroller.go

const (
    MultiClusterSecretLabel = "istio/multiCluster"
)

func NewController(opts ControllerOptions) *Controller {
    // istio/multiCluster=true 레이블의 Secret만 감시
    secrets := kclient.NewFiltered[*corev1.Secret](informerClient, kclient.Filter{
        Namespace:     opts.SystemNamespace,
        LabelSelector: MultiClusterSecretLabel + "=true",
    })

    controller := &Controller{
        ClientBuilder:   DefaultBuildClientsFromConfig,
        namespace:       opts.SystemNamespace,
        configClusterID: opts.ClusterID,
        configCluster: &Cluster{
            ID:     opts.ClusterID,
            Client: opts.Client,
            // ...
        },
        cs:      NewClustersStore(),
        secrets: secrets,
    }

    // Secret 이벤트를 큐에 추가
    secrets.AddEventHandler(controllers.EventHandler[*corev1.Secret]{
        AddFunc: func(obj *corev1.Secret) {
            secretEvents.With(eventLabel.Value("add")).Increment()
            controller.queue.AddObject(obj)
        },
        UpdateFunc: func(oldObj, newObj *corev1.Secret) {
            secretEvents.With(eventLabel.Value("update")).Increment()
            controller.queue.AddObject(newObj)
        },
        DeleteFunc: func(obj *corev1.Secret) {
            secretEvents.With(eventLabel.Value("delete")).Increment()
            controller.queue.AddObject(obj)
        },
    })

    return controller
}
```

### 2.3 Secret 처리: addSecret 흐름

Secret이 추가되거나 업데이트되면 `addSecret`이 호출된다. 하나의 Secret에 여러 클러스터의 kubeconfig가 포함될 수 있다.

```go
func (c *Controller) addSecret(name types.NamespacedName, s *corev1.Secret) error {
    secretKey := name.String()

    // 1. 먼저 삭제된 클러스터 처리
    existingClusters := c.cs.GetExistingClustersFor(secretKey)
    for _, existingCluster := range existingClusters {
        if _, ok := s.Data[string(existingCluster.ID)]; !ok {
            c.deleteCluster(secretKey, existingCluster)
        }
    }

    // 2. 각 클러스터 항목 처리
    for clusterID, kubeConfig := range s.Data {
        // 자기 자신(config cluster)은 무시
        if cluster.ID(clusterID) == c.configClusterID {
            continue
        }

        // kubeconfig SHA256 해시로 변경 감지
        if prev = c.cs.Get(secretKey, cluster.ID(clusterID)); prev != nil {
            kubeConfigSha := sha256.Sum256(kubeConfig)
            if bytes.Equal(kubeConfigSha[:], prev.kubeConfigSha[:]) {
                continue // 변경 없음
            }
        }

        // 원격 클러스터 생성
        remoteCluster, err := c.createRemoteCluster(name, kubeConfig, clusterID)

        // Make-before-break: 새 클러스터를 먼저 등록하고 이전 것은 나중에 정리
        swap := c.cs.Swap(secretKey, remoteCluster.ID, remoteCluster)
        go func() {
            remoteCluster.Run(c.meshWatcher, c.handlers, action, swap, c.debugger)
        }()
    }
    return nil
}
```

### 2.4 ClusterStore: 클러스터 저장소

`ClusterStore`는 `secretKey -> clusterID -> *Cluster` 형태의 2-레벨 맵이다.

```go
// pkg/kube/multicluster/clusterstore.go

type ClusterStore struct {
    sync.RWMutex
    // secretKey(ns/name) -> clusterID -> *Cluster
    remoteClusters       map[string]map[cluster.ID]*Cluster
    clusters             sets.String
    clustersAwaitingSync sets.Set[cluster.ID]
    *krt.RecomputeTrigger
}
```

`Swap` 메서드는 Make-Before-Break 패턴을 구현한다. 새 클러스터를 먼저 저장하고, `PendingClusterSwap`을 반환하여 새 클러스터가 동기화된 후 이전 클러스터를 정리한다.

```go
type PendingClusterSwap struct {
    clusterID cluster.ID
    prev      *Cluster
}

func (p *PendingClusterSwap) Complete() {
    if p.prev != nil {
        p.prev.Stop()
        p.prev.Client.Shutdown()
    }
}
```

### 2.5 Cluster 동기화 수명주기

```
   Secret 감지
       │
       ▼
   createRemoteCluster()
       │
       ▼
   Cluster.Run()
       │
       ├── 1. NamespaceFilter 초기화
       │       (DiscoveryNamespaces)
       │
       ├── 2. KRT 컬렉션 생성
       │       (Pods, Services, EndpointSlices, Nodes, Gateways)
       │
       ├── 3. Handler 콜백 호출
       │       clusterAdded() / clusterUpdated()
       │
       ├── 4. Client.RunAndWait()
       │       (모든 Informer 동기화 대기)
       │
       ├── 5. Handler Syncer 대기
       │
       ├── 6. KRT 컬렉션 동기화 대기
       │
       └── 7. initialSync = true
               reportStatus("synced")
               closeSyncedCh()

   타임아웃 시: RemoteClusterTimeout (기본 30초) 이후
       initialSyncTimeout = true
       reportStatus("timeout")
```

`Cluster` 구조체(`pkg/kube/multicluster/cluster.go`)에는 동기화 상태를 추적하는 여러 필드가 있다.

```go
type Cluster struct {
    ID     cluster.ID
    Client kube.Client

    kubeConfigSha [sha256.Size]byte
    stop          chan struct{}
    initialSync        *atomic.Bool     // RunAndWait 완료 시 true
    initialSyncTimeout *atomic.Bool     // 타임아웃 시 true
    SyncedCh           chan struct{}     // 동기화 완료 신호

    remoteClusterCollections *atomic.Pointer[remoteClusterCollections]
}
```

### 2.6 Credential Rotation (자격 증명 갱신)

Istio는 Secret이 업데이트되면(kubeconfig 변경) 서비스 중단 없이 자격 증명을 교체한다.

1. 새 `Cluster` 객체 생성 (새 kubeconfig로)
2. `ClusterStore.Swap()`으로 새 클러스터 저장, 이전 클러스터 참조 보존
3. 새 클러스터의 Informer 시작 및 동기화 대기
4. 동기화 완료 후 `aggregate.Controller.UpdateRegistry()`로 원자적 교체
5. `PendingClusterSwap.Complete()`로 이전 클러스터 정리

```go
// pilot/pkg/serviceregistry/kube/controller/multicluster.go
if cluster.Action == multicluster.Update {
    go func() {
        <-cluster.SyncedCh  // 새 클러스터 동기화 대기
        m.opts.MeshServiceController.UpdateRegistry(kubeRegistry, clusterStopCh)
    }()
} else {
    m.opts.MeshServiceController.AddRegistryAndRun(kubeRegistry, clusterStopCh)
}
```

---

## 3. 서비스 병합: Aggregate Controller의 ClusterVIPs 병합

### 3.1 Aggregate Controller 구조

`pilot/pkg/serviceregistry/aggregate/controller.go`의 `Controller`는 여러 서비스 레지스트리의 데이터를 하나의 통합 뷰로 병합한다.

```go
type Controller struct {
    meshHolder      mesh.Holder
    configClusterID cluster.ID

    storeLock  sync.RWMutex
    registries []*registryEntry
    running    bool

    handlers          model.ControllerHandlers
    handlersByCluster map[cluster.ID]*model.ControllerHandlers
    model.NetworkGatewaysHandler
}
```

### 3.2 레지스트리 우선순위

Kubernetes 레지스트리는 비-Kubernetes 레지스트리보다 앞에 배치된다. `addRegistry()`에서 이 순서를 보장한다.

```go
func (c *Controller) addRegistry(registry serviceregistry.Instance, stop <-chan struct{}) {
    added := false
    if registry.Provider() == provider.Kubernetes {
        for i, r := range c.registries {
            if r.Provider() != provider.Kubernetes {
                // 첫 번째 비-Kubernetes 레지스트리 앞에 삽입
                c.registries = slices.Insert(c.registries, i, &registryEntry{...})
                added = true
                break
            }
        }
    }
    if !added {
        c.registries = append(c.registries, &registryEntry{...})
    }
}
```

이 순서는 서비스 병합 시 "첫 번째 클러스터가 기본값" 규칙에 영향을 준다.

### 3.3 서비스 병합 알고리즘

`Services()` 메서드는 모든 레지스트리의 서비스를 순회하며 hostname 기반으로 병합한다.

```
   Registry A          Registry B          Registry C (ServiceEntry)
   ┌──────────┐        ┌──────────┐        ┌──────────┐
   │ svc: foo │        │ svc: foo │        │ svc: bar │
   │ VIP: 1.1 │        │ VIP: 2.2 │        │ VIP: 3.3 │
   └────┬─────┘        └────┬─────┘        └────┬─────┘
        │                   │                    │
        ▼                   ▼                    ▼
   ┌─────────────────────────────────────────────────┐
   │          Aggregate Controller.Services()        │
   │                                                 │
   │  smap["foo"] = 0    (첫 등장, index 0)          │
   │  services[0] = foo(VIP: 1.1)                    │
   │                                                 │
   │  smap["foo"] exists  → mergeService()           │
   │  services[0].ClusterVIPs = {A: 1.1, B: 2.2}    │
   │                                                 │
   │  smap["bar"] = 1    (비-K8s, 바로 추가)          │
   │  services[1] = bar(VIP: 3.3)                    │
   └─────────────────────────────────────────────────┘
```

핵심 코드:

```go
func (c *Controller) Services() []*model.Service {
    smap := make(map[host.Name]int)
    index := 0
    services := make([]*model.Service, 0)

    for _, r := range c.GetRegistries() {
        svcs := r.Services()
        if r.Provider() != provider.Kubernetes {
            // 비-K8s 서비스는 병합하지 않고 바로 추가
            index += len(svcs)
            services = append(services, svcs...)
        } else {
            for _, s := range svcs {
                previous, ok := smap[s.Hostname]
                if !ok {
                    // 처음 보는 서비스
                    smap[s.Hostname] = index
                    index++
                    services = append(services, s)
                } else {
                    // 두 번째 이상 등장 → ClusterVIPs 병합
                    if services[previous].ClusterVIPs.Len() < 2 {
                        services[previous] = services[previous].ShallowCopy()
                    }
                    mergeService(services[previous], s, r)
                }
            }
        }
    }
    return services
}
```

### 3.4 mergeService 함수

```go
func mergeService(dst, src *model.Service, srcRegistry serviceregistry.Instance) {
    // 포트 불일치 로깅 (경고용)
    if !src.Ports.Equals(dst.Ports) {
        log.Debugf("service %s defined from cluster %s is different",
                   src.Hostname, srcRegistry.Cluster())
    }

    // ClusterVIPs 병합: 각 클러스터의 VIP을 맵에 추가
    clusterID := srcRegistry.Cluster()
    if len(dst.ClusterVIPs.GetAddressesFor(clusterID)) == 0 {
        newAddresses := src.ClusterVIPs.GetAddressesFor(clusterID)
        dst.ClusterVIPs.SetAddressesFor(clusterID, newAddresses)
    }

    // ServiceAccount 병합 (각 클러스터의 트러스트 도메인이 다를 수 있음)
    if len(src.ServiceAccounts) > 0 {
        sas := make([]string, 0, len(dst.ServiceAccounts)+len(src.ServiceAccounts))
        sas = append(sas, dst.ServiceAccounts...)
        sas = append(sas, src.ServiceAccounts...)
        dst.ServiceAccounts = slices.FilterDuplicates(sas)
    }
}
```

### 3.5 AddressMap 구조

`ClusterVIPs`는 `AddressMap` 타입으로, 클러스터별 VIP 주소를 관리한다.

```go
// pilot/pkg/model/addressmap.go
type AddressMap struct {
    Addresses map[cluster.ID][]string
}

func (m *AddressMap) GetAddressesFor(c cluster.ID) []string { ... }
func (m *AddressMap) SetAddressesFor(c cluster.ID, addresses []string) { ... }
func (m *AddressMap) AddAddressesFor(c cluster.ID, addresses []string) { ... }
func (m *AddressMap) ForEach(fn func(c cluster.ID, addresses []string)) { ... }
```

```
   AddressMap (ClusterVIPs)
  ┌─────────────────────────────────────┐
  │  Addresses: {                       │
  │    "cluster-a": ["10.96.0.100"],    │
  │    "cluster-b": ["10.104.0.50"],    │
  │    "cluster-c": ["10.108.0.200"],   │
  │  }                                  │
  └─────────────────────────────────────┘
```

프록시가 서비스의 VIP을 요청할 때, 프록시가 속한 클러스터의 VIP이 우선적으로 반환된다.

### 3.6 프록시별 레지스트리 검색 최적화

프록시의 ServiceTarget을 조회할 때는 해당 프록시의 클러스터에 해당하는 레지스트리만 검색한다.

```go
func skipSearchingRegistryForProxy(nodeClusterID cluster.ID, r serviceregistry.Instance) bool {
    // 비-K8s 레지스트리(ServiceEntry)는 항상 검색
    if r.Provider() != provider.Kubernetes || nodeClusterID == "" {
        return false
    }
    // 프록시의 클러스터와 다른 K8s 레지스트리는 건너뜀
    return !r.Cluster().Equals(nodeClusterID)
}
```

---

## 4. 엔드포인트 병합: 크로스 클러스터 엔드포인트 디스커버리

### 4.1 엔드포인트와 Locality

각 `IstioEndpoint`는 `Locality` 필드에 `ClusterID`를 포함한다.

```go
// pilot/pkg/model/service.go
type Locality struct {
    Label     string      // "region/zone/subzone" 형식
    ClusterID cluster.ID  // 이 엔드포인트가 속한 클러스터
}
```

이를 통해 EDS(Endpoint Discovery Service) 빌드 시 엔드포인트가 어느 클러스터에서 왔는지 식별할 수 있다.

### 4.2 EndpointBuilder의 네트워크 필터링

`pilot/pkg/xds/endpoints/ep_filters.go`의 `EndpointsByNetworkFilter`는 멀티네트워크 환경에서 엔드포인트를 필터링하는 핵심 로직이다.

```
   프록시 (Cluster A, Network "net-1")

   엔드포인트 목록:
   ┌──────────────────────────────────────────────────────┐
   │  EP1: 10.1.0.5  (Cluster A, Network "net-1")  ──► 직접 접근 (같은 네트워크) │
   │  EP2: 10.1.0.6  (Cluster A, Network "net-1")  ──► 직접 접근              │
   │  EP3: 10.2.0.5  (Cluster B, Network "net-2")  ──► 게이트웨이 교체        │
   │  EP4: 10.2.0.6  (Cluster B, Network "net-2")  ──► 게이트웨이 교체        │
   └──────────────────────────────────────────────────────┘

   필터링 결과:
   ┌──────────────────────────────────────────────────────┐
   │  EP1: 10.1.0.5  (직접 접근)                 Weight: 2│
   │  EP2: 10.1.0.6  (직접 접근)                 Weight: 2│
   │  GW:  34.x.x.x:15443 (net-2 게이트웨이)    Weight: 4│
   └──────────────────────────────────────────────────────┘
```

### 4.3 게이트웨이 선택 로직

```go
// pilot/pkg/xds/endpoints/ep_filters.go

func (b *EndpointBuilder) selectNetworkGateways(nw network.ID, c cluster.ID) []model.NetworkGateway {
    // 1. 네트워크+클러스터 조합으로 정확 매칭 시도
    gws := b.gateways().GatewaysForNetworkAndCluster(nw, c)
    if len(gws) == 0 {
        // 2. 네트워크만으로 매칭 (fallback)
        gws = b.gateways().GatewaysForNetwork(nw)
    }

    // Ambient 모드: HBONE 포트가 있는 게이트웨이만
    if features.EnableAmbientMultiNetwork && !isSidecarProxy(b.proxy) {
        var ambientGws []model.NetworkGateway
        for _, gw := range gws {
            if gw.HBONEPort == 0 { continue }
            ambientGws = append(ambientGws, gw)
        }
        return ambientGws
    }

    // Sidecar 모드: mTLS 포트가 있는 게이트웨이만
    if isSidecarProxy(b.proxy) {
        var sidecarGws []model.NetworkGateway
        for _, gw := range gws {
            if gw.Port == 0 { continue }
            sidecarGws = append(sidecarGws, gw)
        }
        return sidecarGws
    }
    return gws
}
```

네트워크+클러스터 조합을 먼저 시도하는 이유는 두 가지다.
1. 같은 네트워크 내에서도 지연 시간을 줄이기 위해 가장 가까운 게이트웨이를 선택
2. MCS 유즈케이스에서 Export된 클러스터의 게이트웨이로 정확히 라우팅

### 4.4 가중치(Weight) 분배

게이트웨이 엔드포인트의 가중치는 원래 엔드포인트들의 가중치 합을 게이트웨이 수로 나눈다.

```go
func splitWeightAmongGateways(weight uint32, gateways []model.NetworkGateway,
                               gatewayWeights map[model.NetworkGateway]uint32) {
    weightPerGateway := weight / uint32(len(gateways))
    for _, gateway := range gateways {
        gatewayWeights[gateway] += weightPerGateway
    }
}
```

LCM(최소공배수) 기반 스케일링으로 네트워크별 게이트웨이 수가 다르더라도 균등한 분배를 보장한다.

```go
scaleFactor := b.gateways().GetLBWeightScaleFactor()
```

### 4.5 Sidecar vs Ambient 모드의 차이

| 항목 | Sidecar 모드 | Ambient 모드 |
|------|-------------|-------------|
| E/W 게이트웨이 포트 | mTLS (15443) | HBONE (15008) |
| 크로스 네트워크 요구사항 | mTLS 필수 | HBONE 터널 |
| 게이트웨이 없는 원격 네트워크 | 엔드포인트 유지 (레거시 호환) | 엔드포인트 제외 |
| 엔드포인트 생성 방식 | 게이트웨이 IP:Port 직접 | `inner_connect_originate` 내부 리스너 |

Ambient 모드에서 게이트웨이 엔드포인트 생성:

```go
if features.EnableAmbientMultiNetwork && !isSidecarProxy(b.proxy) {
    gwEp = &endpoint.LbEndpoint{
        HostIdentifier: &endpoint.LbEndpoint_Endpoint{
            Endpoint: &endpoint.Endpoint{
                // 이중 HBONE 터널링을 위한 내부 리스너로 리다이렉트
                Address: util.BuildInternalAddressWithIdentifier(
                    innerConnectOriginate, addr),
            },
        },
        // ...
    }
    // 실제 E/W 게이트웨이 주소는 메타데이터에 기록
    gwEp.Metadata.FilterMetadata[util.OriginalDstMetadataKey] =
        util.BuildTunnelMetadataStruct(gwAddr, gwPort, "")
}
```

---

## 5. 네트워크 게이트웨이: East-West 게이트웨이

### 5.1 NetworkGateway 구조

```go
// pilot/pkg/model/network.go
type NetworkGateway struct {
    Network        network.ID          // 이 게이트웨이가 속한 네트워크
    Cluster        cluster.ID          // 이 게이트웨이가 속한 클러스터
    Addr           string              // 게이트웨이 IP 주소
    Port           uint32              // mTLS 포트 (sidecar용, 기본 15443)
    HBONEPort      uint32              // HBONE 포트 (ambient용, 기본 15008)
    ServiceAccount types.NamespacedName // 게이트웨이의 서비스 어카운트
}
```

### 5.2 게이트웨이 발견 메커니즘

Istio는 세 가지 방법으로 네트워크 게이트웨이를 발견한다.

**방법 1: 서비스 레이블 기반**

Service에 `topology.istio.io/network` 레이블이 있으면 자동으로 네트워크 게이트웨이로 인식한다.

```go
// pilot/pkg/serviceregistry/kube/controller/network.go
func (n *networkManager) getGatewayDetails(svc *model.Service) []model.NetworkGateway {
    if nw := svc.Attributes.Labels[label.TopologyNetwork.Name]; nw != "" {
        hbonePort := DefaultNetworkGatewayHBONEPort   // 15008
        gwPort := DefaultNetworkGatewayPort           // 15443

        _, acceptMTLS := svc.Ports.GetByPort(gwPort)
        _, acceptHBONE := svc.Ports.GetByPort(hbonePort)

        return []model.NetworkGateway{{
            Port:      uint32(gwPort),
            HBONEPort: uint32(hbonePort),
            Network:   network.ID(nw),
        }}
    }
    // ...
}
```

**방법 2: MeshNetworks 설정의 registryServiceName**

```yaml
meshNetworks:
  networks:
    network-1:
      endpoints:
        - fromRegistry: cluster-a
      gateways:
        - registryServiceName: istio-eastwestgateway.istio-system.svc.cluster.local
          port: 15443
```

**방법 3: Kubernetes Gateway API 리소스**

`topology.istio.io/network` 레이블이 있는 Gateway 리소스에서 `auto-passthrough` 리스너를 발견한다.

```go
func (n *networkManager) handleGatewayResource(_ *gatewayv1.Gateway, gw *gatewayv1.Gateway, event model.Event) error {
    if nw := gw.GetLabels()[label.TopologyNetwork.Name]; nw == "" {
        return nil
    }

    base := model.NetworkGateway{
        Network: network.ID(gw.GetLabels()[label.TopologyNetwork.Name]),
        Cluster: n.clusterID,
    }

    for _, addr := range gw.Spec.Addresses {
        for _, l := range slices.Filter(gw.Spec.Listeners, autoPassthrough) {
            networkGateway := base
            networkGateway.Addr = addr.Value
            networkGateway.Port = uint32(l.Port)
            newGateways.Insert(networkGateway)
        }
    }
    return nil
}
```

### 5.3 NetworkManager: 게이트웨이 통합 관리

```go
// pilot/pkg/model/network.go
type NetworkManager struct {
    env        *Environment
    NameCache  *networkGatewayNameCache
    xdsUpdater XDSUpdater

    mu sync.RWMutex
    *NetworkGateways       // IP 해석 완료된 게이트웨이
    Unresolved *NetworkGateways  // DNS 해석 전 원본
}
```

`NetworkManager`는 두 가지 소스의 게이트웨이를 병합한다.

```go
func (mgr *NetworkManager) reload() bool {
    gatewaySet := make(NetworkGatewaySet)

    // 1. MeshNetworks 정적 설정에서 게이트웨이 로드
    meshNetworks := mgr.env.NetworksWatcher.Networks()
    for nw, networkConf := range meshNetworks.Networks {
        for _, gw := range networkConf.Gateways {
            if gw.GetAddress() != "" {
                gatewaySet.Insert(NetworkGateway{
                    Network: network.ID(nw),
                    Addr:    gw.GetAddress(),
                    Port:    gw.Port,
                })
            }
        }
    }

    // 2. 서비스 레지스트리 기반 게이트웨이 병합
    gatewaySet.InsertAll(mgr.env.NetworkGateways()...)

    // 3. 호스트네임 게이트웨이 DNS 해석
    resolvedGatewaySet := mgr.resolveHostnameGateways(gatewaySet)

    return mgr.NetworkGateways.update(resolvedGatewaySet) ||
           mgr.Unresolved.update(gatewaySet)
}
```

### 5.4 게이트웨이 인덱싱

게이트웨이는 두 가지 방식으로 인덱싱되어 빠른 조회를 지원한다.

```go
type NetworkGateways struct {
    mu                  *sync.RWMutex
    lcm                 uint32  // 게이트웨이 수의 최소공배수 (가중치 스케일링용)
    byNetwork           map[network.ID][]NetworkGateway
    byNetworkAndCluster map[networkAndCluster][]NetworkGateway
}
```

```
   게이트웨이 인덱스
  ┌──────────────────────────────────────────────────┐
  │ byNetwork:                                       │
  │   "net-1": [GW{10.0.0.1:15443}, GW{10.0.0.2:15443}] │
  │   "net-2": [GW{20.0.0.1:15443}]                 │
  │                                                  │
  │ byNetworkAndCluster:                             │
  │   {net-1, cluster-a}: [GW{10.0.0.1:15443}]      │
  │   {net-1, cluster-b}: [GW{10.0.0.2:15443}]      │
  │   {net-2, cluster-c}: [GW{20.0.0.1:15443}]      │
  │                                                  │
  │ lcm: 2  (net-1에 2개, net-2에 1개 → LCM(2,1)=2) │
  └──────────────────────────────────────────────────┘
```

### 5.5 호스트네임 게이트웨이 DNS 해석

MeshNetworks나 Service의 LoadBalancer가 DNS 호스트네임을 가진 경우, 컨트롤 플레인에서 DNS를 해석한다.

```go
type networkGatewayNameCache struct {
    NetworkGatewaysHandler
    client *dnsClient
    sync.Mutex
    cache map[string]nameCacheEntry
}

type nameCacheEntry struct {
    value  []string      // 해석된 IP 주소들
    expiry time.Time     // TTL 만료 시간
    timer  *time.Timer   // 자동 갱신 타이머
}
```

해석은 A/AAAA 레코드를 동시에 조회하고, TTL 기반으로 캐싱하며, TTL 만료 시 자동 갱신한다.

```go
func (n *networkGatewayNameCache) resolve(name string) ([]string, time.Duration, error) {
    var wg sync.WaitGroup
    wg.Add(2)
    go doResolve(dns.TypeA)     // IPv4
    go doResolve(dns.TypeAAAA)  // IPv6
    wg.Wait()
    // ...
}
```

---

## 6. Locality 로드밸런싱

### 6.1 Locality 구조와 ClusterID 통합

Istio의 Locality는 `region/zone/subzone` 계층에 `ClusterID`를 추가로 포함한다.

```go
// pilot/pkg/model/service.go
type Locality struct {
    Label     string      // "us-east-1/us-east-1a/rack1"
    ClusterID cluster.ID  // "cluster-east"
}
```

이 구조 덕분에 Envoy의 Locality-Aware 라우팅이 멀티클러스터 환경에서도 동작한다.

### 6.2 Locality 우선순위

```
   우선순위 (높음 → 낮음):
   ┌─────────────────────────────────────────────────────┐
   │  1. 같은 Zone + 같은 Cluster                        │
   │  2. 같은 Zone + 다른 Cluster                        │
   │  3. 같은 Region + 같은 Cluster                      │
   │  4. 같은 Region + 다른 Cluster                      │
   │  5. 다른 Region + 같은 Cluster                      │
   │  6. 다른 Region + 다른 Cluster                      │
   └─────────────────────────────────────────────────────┘
```

### 6.3 EDS에서 Locality 기반 가중치

엔드포인트 빌더는 프록시의 locality와 엔드포인트의 locality를 비교하여 Envoy의 `LocalityLbEndpoints`에 적절한 priority를 설정한다. 멀티클러스터에서는 `ClusterID`가 추가 차원으로 작용하여, 같은 zone이라도 다른 클러스터의 엔드포인트에 더 낮은 우선순위를 부여할 수 있다.

### 6.4 네트워크 인식 Locality

프록시가 엔드포인트를 볼 수 있는지는 `ProxyView`로 결정된다.

```go
// pilot/pkg/model/proxy_view.go
func newProxyView(node *Proxy) ProxyView {
    if node == nil || node.Metadata == nil ||
       len(node.Metadata.RequestedNetworkView) == 0 {
        return ProxyViewAll
    }
    return &proxyViewImpl{
        visible: sets.New[string](node.Metadata.RequestedNetworkView...).
                Insert(identifier.Undefined),
        getValue: func(ep *IstioEndpoint) string {
            return ep.Network.String()
        },
    }
}
```

`InNetwork` 메서드는 프록시와 엔드포인트가 같은 네트워크에 있는지 확인한다.

```go
func (node *Proxy) InNetwork(network network.ID) bool {
    return node == nil ||
           identifier.IsSameOrEmpty(network.String(), node.Metadata.Network.String())
}
```

---

## 7. DNS 프록시: 원격 클러스터 서비스 해석

### 7.1 문제: 원격 클러스터의 서비스 DNS 해석

멀티클러스터에서 Pod이 `svc.ns.svc.cluster.local`로 요청할 때, 해당 서비스가 로컬 클러스터에 없으면 Kubernetes DNS(CoreDNS)는 NXDOMAIN을 반환한다. Istio의 DNS 프록시는 이 문제를 해결한다.

```
   Pod (Cluster A)
     │
     │ DNS Query: myservice.ns.svc.cluster.local
     │
     ▼
   Sidecar (DNS Proxy)
     │
     ├── 로컬 K8s DNS에 없음
     │
     ├── Istiod의 서비스 레지스트리 확인
     │   (Aggregate Controller: 모든 클러스터의 서비스 포함)
     │
     ├── Cluster B에 myservice 존재 확인
     │
     └── 자동 할당된 VIP 반환 (예: 240.240.0.x)
         → Pod은 이 VIP으로 연결
         → Envoy가 실제 엔드포인트로 라우팅
```

### 7.2 자동 VIP 할당

원격 클러스터에만 존재하는 서비스는 로컬 클러스터에 ClusterIP가 없다. Istio는 이런 서비스에 자동으로 VIP을 할당한다(Auto-allocated IP).

```go
// pilot/pkg/model/service.go (Service 구조체 발췌)
type Service struct {
    Hostname   host.Name
    ClusterVIPs AddressMap
    DefaultAddress string

    // 자동 할당 IP (원격 전용 서비스에 사용)
    AutoAllocatedIPv4Address string
    AutoAllocatedIPv6Address string
}
```

### 7.3 게이트웨이 DNS 해석

`NetworkManager`는 E/W 게이트웨이의 호스트네임(예: AWS ELB의 DNS 이름)을 주기적으로 해석한다. 이는 `RESOLVE_HOSTNAME_GATEWAYS` 기능 플래그로 제어된다.

```go
if !features.ResolveHostnameGateways {
    log.Warnf("Failed parsing gateway address %s. "+
              "Set RESOLVE_HOSTNAME_GATEWAYS to enable resolving hostnames.", gw.Addr)
    continue
}
```

---

## 8. 트러스트 도메인: SPIFFE 크로스 클러스터 인증

### 8.1 SPIFFE ID 체계

Istio는 SPIFFE(Secure Production Identity Framework For Everyone) 표준을 사용하여 워크로드 ID를 관리한다.

```
   SPIFFE ID 형식:
   spiffe://<trust-domain>/ns/<namespace>/sa/<service-account>

   예시:
   spiffe://cluster.local/ns/default/sa/httpbin
   spiffe://us-east.example.com/ns/prod/sa/frontend
```

### 8.2 단일 트러스트 도메인 (권장)

모든 클러스터가 같은 루트 CA를 공유하고, 같은 트러스트 도메인(`cluster.local`)을 사용한다.

```
   Cluster A                    Cluster B
  ┌──────────────┐            ┌──────────────┐
  │  Trust Domain:│            │  Trust Domain:│
  │  cluster.local│            │  cluster.local│
  │              │            │              │
  │  Root CA: ───┼────────────┼── Root CA:   │
  │  (shared)    │            │  (shared)    │
  │              │            │              │
  │  SPIFFE ID:  │            │  SPIFFE ID:  │
  │  spiffe://   │            │  spiffe://   │
  │  cluster.    │            │  cluster.    │
  │  local/ns/   │   mTLS     │  local/ns/   │
  │  default/sa/ │◄──────────►│  prod/sa/    │
  │  frontend    │            │  backend     │
  └──────────────┘            └──────────────┘
```

### 8.3 다중 트러스트 도메인

다른 트러스트 도메인의 클러스터 간 통신을 위해 `trustDomainAliases`를 설정한다.

```yaml
meshConfig:
  trustDomain: us-east.example.com
  trustDomainAliases:
    - us-west.example.com
    - eu.example.com
```

### 8.4 CA 인증서 배포

Multicluster 컨트롤러는 CA 번들을 원격 클러스터에 배포한다.

```go
// pilot/pkg/serviceregistry/kube/controller/multicluster.go
if m.distributeCACert && (shouldLead || configCluster) {
    if features.EnableClusterTrustBundles {
        // ClusterTrustBundle 리소스 사용
        c := clustertrustbundle.NewController(client, m.caBundleWatcher)
        c.Run(leaderStop)
    } else {
        // Namespace controller로 각 네임스페이스에 CA 시크릿 복제
        nc := NewNamespaceController(client, m.caBundleWatcher)
        nc.Run(leaderStop)
    }
}
```

### 8.5 서비스 어카운트 병합

`mergeService`에서 각 클러스터의 서비스 어카운트를 병합하여 크로스 클러스터 인증에 필요한 모든 ID를 포함한다.

```go
// pilot/pkg/serviceregistry/aggregate/controller.go
func mergeService(dst, src *model.Service, srcRegistry serviceregistry.Instance) {
    // 각 클러스터의 서비스 어카운트 병합
    // 각 클러스터마다 다른 트러스트 도메인일 수 있으므로 모두 수집
    if len(src.ServiceAccounts) > 0 {
        sas := make([]string, 0, len(dst.ServiceAccounts)+len(src.ServiceAccounts))
        sas = append(sas, dst.ServiceAccounts...)
        sas = append(sas, src.ServiceAccounts...)
        dst.ServiceAccounts = slices.FilterDuplicates(sas)
    }
}
```

---

## 9. MCS API: ServiceExport/ServiceImport

### 9.1 Kubernetes MCS(Multi-Cluster Services) API 개요

MCS API는 Kubernetes SIG-Multicluster에서 정의한 표준으로, 멀티클러스터에서 서비스 가시성을 제어한다.

```
   Cluster A                    Cluster B
  ┌──────────────────┐        ┌──────────────────┐
  │                  │        │                  │
  │  Service: foo    │        │  Service: foo    │
  │  ServiceExport:  │        │  ServiceImport:  │
  │    foo ──────────┼────────┼─► foo            │
  │  (이 서비스를    │  MCS    │  (ClusterSet     │
  │   메시에 공개)   │ Controller│  VIP 할당)     │
  │                  │        │                  │
  │  hostname:       │        │  hostname:       │
  │  foo.ns.svc.     │        │  foo.ns.svc.     │
  │  cluster.local   │        │  clusterset.local│
  └──────────────────┘        └──────────────────┘
```

### 9.2 ServiceExport 처리

`pilot/pkg/serviceregistry/kube/controller/serviceexportcache.go`에서 ServiceExport를 감시하여 엔드포인트 가시성 정책을 결정한다.

```go
type serviceExportCacheImpl struct {
    *Controller
    serviceExports kclient.Untyped

    // cluster.local 호스트에 대한 정책
    clusterLocalPolicySelector    discoverabilityPolicySelector
    // clusterset.local 호스트에 대한 정책
    clusterSetLocalPolicySelector discoverabilityPolicySelector
}
```

가시성 정책은 세 가지가 있다.

| 정책 | 의미 |
|------|------|
| `AlwaysDiscoverable` | 메시 어디서나 접근 가능 (기본) |
| `DiscoverableFromSameCluster` | 같은 클러스터에서만 접근 가능 |

```go
func (ec *serviceExportCacheImpl) EndpointDiscoverabilityPolicy(svc *model.Service)
    model.EndpointDiscoverabilityPolicy {

    if strings.HasSuffix(svc.Hostname.String(), "."+constants.DefaultClusterSetLocalDomain) {
        return ec.clusterSetLocalPolicySelector(svc)
    }
    return ec.clusterLocalPolicySelector(svc)
}
```

`ENABLE_MCS_CLUSTER_LOCAL`이 true이면, `cluster.local` 호스트의 엔드포인트는 같은 클러스터에서만 접근 가능하게 되어, 크로스 클러스터 접근은 반드시 `clusterset.local`을 통해야 한다.

### 9.3 ServiceImport 처리

`pilot/pkg/serviceregistry/kube/controller/serviceimportcache.go`에서 ServiceImport를 감시하여 합성(synthetic) MCS 서비스를 생성한다.

```go
// serviceImportCacheImpl은 ServiceImport를 읽어 ClusterSet VIP을 추출하고
// clusterset.local 도메인의 합성 서비스를 생성한다.
type serviceImportCacheImpl struct {
    *Controller
    serviceImports kclient.Untyped
}
```

합성 MCS 서비스 생성:

```go
func (ic *serviceImportCacheImpl) genMCSService(realService *model.Service,
    mcsHost host.Name, vips []string) *model.Service {

    mcsService := realService.ShallowCopy()
    mcsService.Hostname = mcsHost  // foo.ns.svc.clusterset.local
    mcsService.DefaultAddress = vips[0]
    mcsService.ClusterVIPs.Addresses = map[cluster.ID][]string{
        ic.Cluster(): vips,
    }
    return mcsService
}
```

### 9.4 Auto ServiceExport

`ENABLE_MCS_AUTO_EXPORT` 기능 플래그가 활성화되면, 모든 서비스에 대해 자동으로 ServiceExport를 생성한다.

```go
// pilot/pkg/serviceregistry/kube/controller/autoserviceexportcontroller.go
func (c *autoServiceExportController) Reconcile(key types.NamespacedName) error {
    svc := c.services.Get(key.Name, key.Namespace)
    if svc == nil { return nil }

    // 클러스터 로컬 서비스는 자동 내보내기하지 않음
    if c.isClusterLocalService(svc) { return nil }

    // ServiceExport 생성, 라이프사이클을 Service에 바인딩
    serviceExport := mcsapi.ServiceExport{
        ObjectMeta: metav1.ObjectMeta{
            Namespace: svc.Namespace,
            Name:      svc.Name,
            OwnerReferences: []metav1.OwnerReference{{
                APIVersion: v1.SchemeGroupVersion.String(),
                Kind:       gvk.Service.Kind,
                Name:       svc.Name,
                UID:        svc.UID,
            }},
        },
    }
    // ...
}
```

OwnerReference를 설정하여 Service가 삭제되면 ServiceExport도 자동으로 가비지 수집된다.

### 9.5 MCS 관련 기능 플래그

| 환경 변수 | 기본값 | 설명 |
|----------|--------|------|
| `ENABLE_MCS_AUTO_EXPORT` | false | 모든 서비스 자동 내보내기 |
| `ENABLE_MCS_SERVICE_DISCOVERY` | false | MCS 기반 서비스 디스커버리 |
| `ENABLE_MCS_HOST` | false | `clusterset.local` 호스트 생성 |
| `ENABLE_MCS_CLUSTER_LOCAL` | false | `cluster.local` 엔드포인트를 클러스터 내부로 제한 |
| `MCS_API_VERSION` | "v1alpha1" | MCS API 버전 |

### 9.6 MCS 서비스 정보 흐름

```
   1. Service (cluster.local) 생성
      │
      ▼
   2. AutoServiceExport → ServiceExport 자동 생성
      │
      ▼
   3. MCS Controller (외부) → ServiceImport 생성 (ClusterSet VIP 할당)
      │
      ▼
   4. serviceImportCache → 합성 MCS 서비스 생성
      │                     (hostname: foo.ns.svc.clusterset.local)
      ▼
   5. Aggregate Controller → 모든 클러스터의 MCS 서비스 병합
      │
      ▼
   6. EDS → clusterset.local 서비스의 엔드포인트 = Export된 클러스터의 엔드포인트
```

---

## 10. 설계 이유: 왜 이렇게 설계했는가

### 10.1 왜 Secret 기반 클러스터 등록인가?

**질문**: 왜 별도의 CRD나 API를 만들지 않고 Kubernetes Secret으로 클러스터를 등록하는가?

**답변**: 이 설계에는 네 가지 핵심 이유가 있다.

1. **최소 의존성**: 추가 CRD 없이 순수 Kubernetes 기본 리소스만으로 동작한다. 원격 클러스터에 Istio CRD가 설치되어 있지 않아도 연결할 수 있다.

2. **kubeconfig 표준 활용**: Kubernetes의 표준 인증 메커니즘(kubeconfig)을 재사용한다. ServiceAccount 토큰, 외부 IdP, 클라우드 제공자 인증 등 모든 K8s 인증 방식을 그대로 지원한다.

3. **동적 클러스터 관리**: Secret의 CRUD를 감시하여 런타임에 클러스터를 추가/제거/업데이트할 수 있다. Istiod를 재시작할 필요가 없다.

4. **Credential Rotation**: Secret 업데이트 시 Make-Before-Break 패턴으로 서비스 중단 없이 자격 증명을 교체한다. SHA-256 해시로 변경을 감지하고, 변경되지 않은 kubeconfig는 건너뛴다.

```go
// SHA-256으로 kubeconfig 변경 감지
kubeConfigSha := sha256.Sum256(kubeConfig)
if bytes.Equal(kubeConfigSha[:], prev.kubeConfigSha[:]) {
    logger.Infof("skipping update (kubeconfig are identical)")
    continue
}
```

### 10.2 왜 네트워크(Network) 개념이 필요한가?

**질문**: 클러스터 ID만으로 충분하지 않은가? 왜 별도의 Network ID가 필요한가?

**답변**: 클러스터와 네트워크는 독립적인 차원이다.

```
   시나리오 1: 하나의 네트워크, 여러 클러스터 (같은 VPC)
  ┌────────────────────────────────────────────────┐
  │  Network: "vpc-1"                              │
  │  ┌────────────┐  ┌────────────┐                │
  │  │ Cluster A  │  │ Cluster B  │  Pod 직접 통신 │
  │  │ 10.1.x.x   │  │ 10.2.x.x   │  가능         │
  │  └────────────┘  └────────────┘                │
  └────────────────────────────────────────────────┘

   시나리오 2: 여러 네트워크, 여러 클러스터 (다른 VPC)
  ┌──────────────────┐  ┌──────────────────┐
  │  Network: "vpc-1" │  │  Network: "vpc-2" │
  │  ┌────────────┐  │  │  ┌────────────┐  │
  │  │ Cluster A  │  │  │  │ Cluster C  │  │
  │  └────────────┘  │  │  └────────────┘  │
  │  ┌────────────┐  │  │                  │
  │  │ Cluster B  │  │  │  E/W GW 필요    │
  │  └────────────┘  │  │                  │
  └──────────────────┘  └──────────────────┘
```

1. **같은 네트워크의 클러스터들**: Pod IP가 직접 라우팅 가능하므로 게이트웨이 없이 통신한다. 클러스터만으로는 이 관계를 표현할 수 없다.

2. **다른 네트워크의 클러스터들**: E/W 게이트웨이를 통한 터널링이 필요하다. 네트워크 ID가 있어야 "이 엔드포인트로 가려면 어떤 게이트웨이를 거쳐야 하는가"를 결정할 수 있다.

3. **하이브리드 환경**: 같은 클러스터 내에서도 노드가 다른 네트워크에 있을 수 있다(CIDR 기반 네트워크 할당).

### 10.3 왜 Aggregate Controller인가?

**질문**: 왜 단일 통합 레지스트리 대신 여러 레지스트리를 집계하는 방식인가?

**답변**:

1. **다양한 서비스 소스 지원**: Kubernetes, ServiceEntry(외부 서비스), MCS 등 다른 종류의 서비스 소스를 독립적으로 관리할 수 있다.

2. **동적 레지스트리 추가/제거**: 런타임에 새 클러스터의 레지스트리를 추가하거나 제거할 수 있다. `AddRegistryAndRun()`과 `DeleteRegistry()`가 이를 지원한다.

3. **장애 격리**: 한 클러스터의 API 서버에 문제가 생겨도 다른 클러스터의 서비스 정보에는 영향을 주지 않는다.

4. **구현의 유연성**: 각 레지스트리는 `ServiceDiscovery` 인터페이스만 구현하면 된다. 새로운 서비스 소스를 추가하기 위해 기존 코드를 수정할 필요가 없다.

### 10.4 왜 E/W 게이트웨이에 SNI 라우팅을 쓰는가?

사이드카 모드의 East-West 게이트웨이는 mTLS의 SNI(Server Name Indication)를 사용하여 트래픽을 올바른 서비스로 라우팅한다.

```
   Client Sidecar → E/W Gateway → Target Pod

   1. Client Sidecar:
      TLS ClientHello에 SNI 설정
      SNI = "outbound_.80_._.reviews.default.svc.cluster.local"

   2. E/W Gateway (auto-passthrough):
      SNI를 파싱하여 대상 서비스 식별
      TLS를 종료하지 않고 그대로 전달 (passthrough)

   3. Target Sidecar:
      mTLS 종료, 요청 처리
```

이유:
- 게이트웨이에서 TLS를 종료하지 않으므로 E2E mTLS 보장
- 게이트웨이에 서비스별 설정이 불필요 (auto-passthrough)
- 새 서비스가 추가되어도 게이트웨이 재설정 불필요

### 10.5 왜 RemoteClusterTimeout이 필요한가?

```go
RemoteClusterTimeout = env.Register(
    "PILOT_REMOTE_CLUSTER_TIMEOUT",
    30*time.Second,
    "After this timeout expires, pilot can become ready without syncing data from "+
    "clusters added via remote-secrets.",
).Get()
```

원격 클러스터의 API 서버에 연결할 수 없거나 동기화가 느린 경우, Istiod의 시작이 무한정 지연될 수 있다. `RemoteClusterTimeout`은 이러한 상황에서 Istiod가 이미 동기화된 클러스터의 데이터만으로라도 서비스할 수 있게 한다.

```go
// pkg/kube/multicluster/cluster.go
if features.RemoteClusterTimeout > 0 {
    time.AfterFunc(features.RemoteClusterTimeout, func() {
        if !c.initialSync.Load() {
            log.Errorf("remote cluster %s failed to sync after %v",
                       c.ID, features.RemoteClusterTimeout)
            timeouts.With(clusterLabel.Value(string(c.ID))).Increment()
            c.closeSyncedCh()
        }
        c.initialSyncTimeout.Store(true)
        c.reportStatus(SyncStatusTimeout)
    })
}
```

### 10.6 왜 WorkloadEntry 크로스 클러스터를 지원하는가?

```go
WorkloadEntryCrossCluster = env.Register(
    "PILOT_ENABLE_CROSS_CLUSTER_WORKLOAD_ENTRY", true,
    "If enabled, pilot will read WorkloadEntry from other clusters, "+
    "selectable by Services in that cluster.").Get()
```

VM(가상 머신) 기반 워크로드가 다른 클러스터에 등록된 경우에도 서비스가 이를 선택할 수 있어야 한다. 예를 들어 Cluster A의 Service가 Cluster B에 등록된 WorkloadEntry(VM)를 엔드포인트로 포함할 수 있다.

### 10.7 멀티클러스터 모니터링 메트릭

Secret Controller는 다음 메트릭을 제공한다.

| 메트릭 | 설명 |
|--------|------|
| `istiod_managed_clusters` | 관리 중인 클러스터 수 (local/remote) |
| `istiod_remote_cluster_sync_status` | 원격 클러스터 동기화 상태 |
| `remote_cluster_sync_timeouts_total` | 동기화 타임아웃 횟수 |
| `remote_cluster_secret_events_total` | Secret 이벤트 횟수 (add/update/delete) |

```go
localClusters  = clustersCount.With(clusterType.Value("local"))
remoteClusters = clustersCount.With(clusterType.Value("remote"))

remoteClusterSyncState = monitoring.NewGauge(
    "istiod_remote_cluster_sync_status",
    "Current synchronization state of remote clusters managed by istiod.",
)
```

---

## 부록: 멀티클러스터 관련 주요 소스 파일

| 파일 | 역할 |
|------|------|
| `pkg/cluster/id.go` | `cluster.ID` 타입 정의 |
| `pkg/network/id.go` | `network.ID` 타입 정의 |
| `pkg/kube/multicluster/secretcontroller.go` | Secret 기반 클러스터 등록 컨트롤러 |
| `pkg/kube/multicluster/cluster.go` | `Cluster` 구조체, 동기화 수명주기 |
| `pkg/kube/multicluster/clusterstore.go` | `ClusterStore`, Make-Before-Break Swap |
| `pilot/pkg/serviceregistry/aggregate/controller.go` | Aggregate Controller, 서비스/엔드포인트 병합 |
| `pilot/pkg/serviceregistry/kube/controller/multicluster.go` | 멀티클러스터 kubeController 관리 |
| `pilot/pkg/serviceregistry/kube/controller/network.go` | 네트워크 게이트웨이 관리 |
| `pilot/pkg/model/network.go` | `NetworkGateway`, `NetworkManager`, DNS 캐시 |
| `pilot/pkg/model/service.go` | `Service`, `IstioEndpoint`, `Locality`, `AddressMap` |
| `pilot/pkg/model/addressmap.go` | `AddressMap` (클러스터별 VIP 맵) |
| `pilot/pkg/xds/endpoints/ep_filters.go` | 엔드포인트 네트워크 필터링 |
| `pilot/pkg/serviceregistry/kube/controller/serviceexportcache.go` | MCS ServiceExport 처리 |
| `pilot/pkg/serviceregistry/kube/controller/serviceimportcache.go` | MCS ServiceImport 처리 |
| `pilot/pkg/serviceregistry/kube/controller/autoserviceexportcontroller.go` | 자동 ServiceExport |
| `pilot/pkg/features/pilot.go` | `RemoteClusterTimeout`, `WorkloadEntryCrossCluster` |
| `pilot/pkg/features/experimental.go` | MCS 관련 기능 플래그 |
| `pilot/pkg/features/ambient.go` | `EnableAmbientMultiNetwork` |
