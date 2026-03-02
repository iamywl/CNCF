# 17. Cilium CRD 및 Kubernetes 통합 서브시스템

## 목차
1. [개요](#1-개요)
2. [Cilium CRD 전체 목록](#2-cilium-crd-전체-목록)
3. [CRD 타입 정의 상세](#3-crd-타입-정의-상세)
4. [K8s 컨트롤러 패턴](#4-k8s-컨트롤러-패턴)
5. [client-go 사용 패턴](#5-client-go-사용-패턴)
6. [controller-runtime 통합](#6-controller-runtime-통합)
7. [pkg/k8s/ 패키지 구조](#7-pkgk8s-패키지-구조)
8. [CRD에서 내부 데이터 변환](#8-crd에서-내부-데이터-변환)
9. [Status 업데이트 패턴](#9-status-업데이트-패턴)
10. [Operator의 역할](#10-operator의-역할)
11. [Hive 프레임워크와 Resource 추상화](#11-hive-프레임워크와-resource-추상화)
12. [관련 소스 파일 맵](#12-관련-소스-파일-맵)

---

## 1. 개요

Cilium은 Kubernetes의 Custom Resource Definition(CRD) 메커니즘을 광범위하게 활용하여 네트워크 정책, 엔드포인트 관리, BGP 설정, IPAM, L7 프록시 설정 등 다양한 기능을 선언적으로 관리한다. 이 문서에서는 Cilium이 정의하는 20개 이상의 CRD 타입, Kubernetes API와의 통합 패턴, 그리고 내부 데이터 처리 파이프라인을 상세히 분석한다.

### 핵심 아키텍처

```
사용자 (kubectl apply)
    |
    v
K8s API Server  <---> etcd (CRD 저장소)
    |
    |  Watch/List
    v
+------------------------------------------+
|  Cilium Agent / Operator                 |
|                                          |
|  SharedInformer  ->  EventHandler        |
|       |                   |              |
|       v                   v              |
|  Local Cache         WorkQueue           |
|  (Store/Indexer)         |               |
|                          v               |
|                    Reconciler            |
|                    (Controller)          |
|                          |               |
|                          v               |
|                  내부 데이터 구조          |
|                  (Policy, Endpoint, etc.) |
+------------------------------------------+
```

Cilium의 K8s 통합은 두 개의 주요 바이너리에서 동작한다:

1. **cilium-agent**: 각 노드에서 실행되며, CRD 변경을 감시하고 데이터플레인(eBPF)에 반영
2. **cilium-operator**: 클러스터 단위로 실행되며, CRD 관리, GC(가비지 컬렉션), IPAM 할당 등 수행

---

## 2. Cilium CRD 전체 목록

Cilium은 `cilium.io` API 그룹 아래에 v2와 v2alpha1 두 가지 버전의 CRD를 정의한다.

### 2.1 cilium.io/v2 (안정 버전)

| CRD 이름 | 약칭 | Scope | 용도 |
|---------|------|-------|------|
| **CiliumNetworkPolicy** | CNP, ciliumnp | Namespaced | Cilium 확장 네트워크 정책 |
| **CiliumClusterwideNetworkPolicy** | CCNP | Cluster | 클러스터 범위 네트워크 정책 |
| **CiliumEndpoint** | CEP, ciliumep | Namespaced | 엔드포인트 상태 (IP, Identity) |
| **CiliumIdentity** | ciliumid | Cluster | 보안 Identity (레이블 기반) |
| **CiliumNode** | CN, ciliumn | Cluster | 노드별 Cilium 설정/상태 |
| **CiliumNodeConfig** | CNC | Namespaced | 노드별 설정 오버라이드 |
| **CiliumEnvoyConfig** | CEC | Namespaced | Envoy L7 프록시 설정 |
| **CiliumClusterwideEnvoyConfig** | CCEC | Cluster | 클러스터 범위 Envoy 설정 |
| **CiliumLocalRedirectPolicy** | CLRP | Namespaced | 노드 로컬 트래픽 리다이렉트 |
| **CiliumEgressGatewayPolicy** | CEGP | Cluster | 이그레스 게이트웨이 정책 |
| **CiliumCIDRGroup** | CCG | Cluster | CIDR 그룹 (정책에서 참조) |
| **CiliumLoadBalancerIPPool** | lbippool | Cluster | LB IP 풀 관리 |
| **CiliumBGPClusterConfig** | cbgpcluster | Cluster | BGP 클러스터 설정 |
| **CiliumBGPPeerConfig** | - | Cluster | BGP 피어 설정 |
| **CiliumBGPAdvertisement** | - | Cluster | BGP 경로 광고 설정 |
| **CiliumBGPNodeConfig** | - | Cluster | BGP 노드별 설정 |
| **CiliumBGPNodeConfigOverride** | - | Cluster | BGP 노드별 설정 오버라이드 |

### 2.2 cilium.io/v2alpha1 (알파 버전)

| CRD 이름 | 약칭 | Scope | 용도 |
|---------|------|-------|------|
| **CiliumEndpointSlice** | CES | Cluster | 엔드포인트 슬라이스 (CEP 집합) |
| **CiliumL2AnnouncementPolicy** | l2announcement | Cluster | L2 ARP/NDP 광고 정책 |
| **CiliumPodIPPool** | CPIP | Cluster | Multi-pool IPAM 풀 |
| **CiliumGatewayClassConfig** | CGCC | Cluster | Gateway API 클래스 설정 |
| **CiliumBGPClusterConfig** | - | Cluster | (v2alpha1 미러) |
| **CiliumBGPPeerConfig** | - | Cluster | (v2alpha1 미러) |
| **CiliumBGPAdvertisement** | - | Cluster | (v2alpha1 미러) |
| **CiliumBGPNodeConfig** | - | Cluster | (v2alpha1 미러) |
| **CiliumBGPNodeConfigOverride** | - | Cluster | (v2alpha1 미러) |
| **CiliumNodeConfig** | CNC | Namespaced | (v2alpha1 미러) |
| **CiliumLoadBalancerIPPool** | - | Cluster | (v2alpha1 미러) |

### 2.3 CRD 등록 메커니즘

CRD 타입들은 Kubernetes의 runtime.Scheme에 등록되어야 한다. 이 등록은 `register.go` 파일의 `addKnownTypes()` 함수에서 수행된다.

**소스**: `pkg/k8s/apis/cilium.io/v2/register.go`
```go
func addKnownTypes(scheme *runtime.Scheme) error {
    scheme.AddKnownTypes(SchemeGroupVersion,
        &CiliumNetworkPolicy{},
        &CiliumNetworkPolicyList{},
        &CiliumClusterwideNetworkPolicy{},
        &CiliumClusterwideNetworkPolicyList{},
        &CiliumEndpoint{},
        &CiliumEndpointList{},
        &CiliumNode{},
        &CiliumNodeList{},
        &CiliumIdentity{},
        &CiliumIdentityList{},
        // ... 총 30+ 타입 등록
    )
    metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
    return nil
}
```

**리소스 이름 상수**: `pkg/k8s/apis/cilium.io/v2/register.go`
```go
const (
    CNPName  = "ciliumnetworkpolicies.cilium.io"
    CCNPName = "ciliumclusterwidenetworkpolicies.cilium.io"
    CEPName  = "ciliumendpoints.cilium.io"
    CIDName  = "ciliumidentities.cilium.io"
    CNName   = "ciliumnodes.cilium.io"
    // ...
)
```

---

## 3. CRD 타입 정의 상세

### 3.1 CiliumNetworkPolicy (CNP)

**소스**: `pkg/k8s/apis/cilium.io/v2/cnp_types.go`

CNP는 Kubernetes의 기본 NetworkPolicy를 확장한 Cilium 고유의 네트워크 정책이다. L3/L4뿐 아니라 L7 (HTTP, gRPC, Kafka 등) 필터링을 지원한다.

```go
type CiliumNetworkPolicy struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata"`

    // 단일 규칙
    Spec *api.Rule `json:"spec,omitempty"`
    // 복수 규칙 (한 CNP에 여러 규칙)
    Specs api.Rules `json:"specs,omitempty"`
    // 노드별 적용 상태
    Status CiliumNetworkPolicyStatus `json:"status,omitempty"`
}
```

CNP의 핵심 처리 흐름:
1. 사용자가 CNP를 생성하면 K8s API Server에 저장
2. cilium-agent의 Watcher가 CNP 변경을 감지
3. `Parse()` 메서드가 CNP를 내부 `api.Rules`로 변환
4. 변환된 정책은 Policy Repository에 import
5. 영향받는 엔드포인트의 BPF 프로그램이 재생성

### 3.2 CiliumEndpoint (CEP)

**소스**: `pkg/k8s/apis/cilium.io/v2/types.go`

CEP는 각 Pod 엔드포인트의 런타임 상태를 나타낸다. cilium-agent가 생성하고 업데이트한다.

```go
type CiliumEndpoint struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata"`
    Status EndpointStatus `json:"status,omitempty"`
}

type EndpointStatus struct {
    ID          int64              `json:"id,omitempty"`
    Identity    *EndpointIdentity  `json:"identity,omitempty"`
    Networking  *EndpointNetworking `json:"networking,omitempty"`
    State       string             `json:"state,omitempty"`
    Policy      *EndpointPolicy    `json:"policy,omitempty"`
    // ...
}
```

Status 필드의 핵심 정보:
- **Identity**: 보안 Identity ID와 레이블
- **Networking**: IP 주소 목록과 노드 IP
- **State**: 엔드포인트 상태 (creating, ready, disconnected 등)
- **Policy**: ingress/egress 정책 적용 상태

### 3.3 CiliumIdentity (CID)

**소스**: `pkg/k8s/apis/cilium.io/v2/types.go`

CiliumIdentity는 보안 Identity를 CRD로 관리한다. KVStore(etcd)의 대안으로, CRD를 Identity 할당의 글로벌 조정 백엔드로 사용할 수 있다.

```go
type CiliumIdentity struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata"`
    // 이 Identity를 정의하는 보안 레이블
    SecurityLabels map[string]string `json:"security-labels"`
}
```

CRD 이름 자체가 숫자 Identity ID이며, ObjectMeta.Labels에 Kubernetes 소스 레이블이 설정된다.

### 3.4 CiliumNode (CN)

**소스**: `pkg/k8s/apis/cilium.io/v2/types.go`

CiliumNode는 Cilium이 관리하는 노드의 설정과 상태를 나타낸다.

```go
type CiliumNode struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata"`
    Spec   NodeSpec   `json:"spec"`
    Status NodeStatus `json:"status,omitempty"`
}

type NodeSpec struct {
    InstanceID string          `json:"instance-id,omitempty"`
    Addresses  []NodeAddress   `json:"addresses,omitempty"`
    ENI        eniTypes.ENISpec `json:"eni,omitempty"`
    Azure      azureTypes.AzureSpec `json:"azure,omitempty"`
    IPAM       ipamTypes.IPAMSpec `json:"ipam,omitempty"`
    // ...
}
```

NodeSpec에는 클라우드 프로바이더별 설정(AWS ENI, Azure, AlibabaCloud)과 IPAM 설정이 포함된다.

### 3.5 기타 주요 CRD

#### CiliumEnvoyConfig (CEC)
**소스**: `pkg/k8s/apis/cilium.io/v2/cec_types.go`

Envoy L7 프록시 설정을 정의한다. xDS 리소스(Listener, RouteConfiguration, Cluster 등)를 직접 명시하여 서비스별 L7 로드밸런싱을 구성한다.

```go
type CiliumEnvoyConfigSpec struct {
    Services        []*ServiceListener `json:"services,omitempty"`
    BackendServices []*Service         `json:"backendServices,omitempty"`
    Resources       []XDSResource      `json:"resources,omitempty"`
    NodeSelector    *slim_metav1.LabelSelector `json:"nodeSelector,omitempty"`
}
```

#### CiliumEgressGatewayPolicy (CEGP)
**소스**: `pkg/k8s/apis/cilium.io/v2/cegp_types.go`

이그레스 트래픽을 특정 게이트웨이 노드로 리다이렉트하고 SNAT하는 정책이다.

#### CiliumLocalRedirectPolicy (CLRP)
**소스**: `pkg/k8s/apis/cilium.io/v2/clrp_types.go`

노드 로컬 트래픽 리다이렉트 정책으로, 특정 IP:Port로 향하는 트래픽을 같은 노드의 Pod으로 리다이렉트한다.

#### CiliumLoadBalancerIPPool
**소스**: `pkg/k8s/apis/cilium.io/v2/lbipam_types.go`

LoadBalancer 타입 Service에 할당할 IP 풀을 정의한다.

#### CiliumL2AnnouncementPolicy
**소스**: `pkg/k8s/apis/cilium.io/v2alpha1/l2announcement_types.go`

어떤 노드가 어떤 서비스 IP를 L2(ARP/NDP) 네트워크에 광고할지 정의한다.

#### CiliumPodIPPool
**소스**: `pkg/k8s/apis/cilium.io/v2alpha1/ippool_types.go`

Multi-pool IPAM 모드에서 Pod에 할당할 IP 풀을 정의한다.

#### CiliumNodeConfig (CNC)
**소스**: `pkg/k8s/apis/cilium.io/v2/cnc_types.go`

노드별 Cilium 에이전트 설정을 오버라이드한다. `cilium-config` ConfigMap의 값을 노드 셀렉터 기반으로 재정의할 수 있다.

```go
type CiliumNodeConfigSpec struct {
    Defaults     map[string]string         `json:"defaults"`
    NodeSelector *metav1.LabelSelector     `json:"nodeSelector"`
}
```

---

## 4. K8s 컨트롤러 패턴

Cilium은 Kubernetes의 표준 컨트롤러 패턴을 따르되, 성능과 메모리 효율성을 위해 자체 추상화를 도입했다.

### 4.1 표준 컨트롤러 패턴

```
API Server
    |
    | Watch (HTTP streaming)
    v
SharedInformer
    |
    |-- Reflector (List + Watch)
    |       |
    |       v
    |   DeltaFIFO Queue
    |       |
    |       v
    |-- Local Store (Indexer/Cache)
    |
    |-- EventHandler
            |
            v
        WorkQueue (RateLimiting)
            |
            v
        Reconciler (processItem)
```

### 4.2 Cilium의 Watcher 구현

**소스**: `pkg/k8s/watchers/watcher.go`

Cilium은 `K8sWatcher` 구조체를 중심으로 모든 K8s 리소스 감시를 관리한다.

```go
type K8sWatcher struct {
    clientset client.Clientset

    k8sPodWatcher             *K8sPodWatcher
    k8sCiliumNodeWatcher      *K8sCiliumNodeWatcher
    k8sCiliumEndpointsWatcher *K8sCiliumEndpointsWatcher

    k8sResourceSynced *synced.Resources
    k8sAPIGroups      *synced.APIGroups
    cfg               WatcherConfiguration
}
```

각 CRD에 대한 Watcher 매핑이 `ciliumResourceToGroupMapping`에 정의되어 있다:

```go
var ciliumResourceToGroupMapping = map[string]watcherInfo{
    synced.CRDResourceName(cilium_v2.CNPName):  {waitOnly, k8sAPIGroupCiliumNetworkPolicyV2},
    synced.CRDResourceName(cilium_v2.CCNPName): {waitOnly, k8sAPIGroupCiliumClusterwideNetworkPolicyV2},
    synced.CRDResourceName(cilium_v2.CEPName):  {start, k8sAPIGroupCiliumEndpointV2},
    synced.CRDResourceName(cilium_v2.CNName):   {start, k8sAPIGroupCiliumNodeV2},
    // ...
}
```

Watcher의 종류:
- **start**: 직접 Watcher를 시작
- **waitOnly**: 외부 goroutine에서 시작되기를 대기
- **skip**: 이 Watcher가 다른 패키지에서 처리

### 4.3 이벤트 처리 흐름

**소스**: `pkg/k8s/watchers/cilium_endpoint.go`

CEP Watcher의 실제 이벤트 처리:

```go
func (k *K8sCiliumEndpointsWatcher) ciliumEndpointsInit(ctx context.Context) {
    var synced atomic.Bool

    // 동기화 상태 추적
    k.k8sResourceSynced.BlockWaitGroupToSyncResources(
        ctx.Done(), nil,
        func() bool { return synced.Load() },
        k8sAPIGroupCiliumEndpointV2,
    )

    go func() {
        // Resource[T]의 이벤트 스트림 소비
        events := k.ciliumSlimEndpoint.Events(ctx)
        cache := make(map[resource.Key]*types.CiliumEndpoint)

        for event := range events {
            switch event.Kind {
            case resource.Sync:
                synced.Store(true)
            case resource.Upsert:
                oldObj, ok := cache[event.Key]
                if !ok || !oldObj.DeepEqual(event.Object) {
                    k.endpointUpdated(oldObj, event.Object)
                    cache[event.Key] = event.Object
                }
            case resource.Delete:
                k.endpointDeleted(event.Object)
                delete(cache, event.Key)
            }
            event.Done(nil)  // 반드시 호출해야 함
        }
    }()
}
```

핵심 포인트:
1. `Resource[T].Events()` 채널에서 이벤트를 수신
2. `Sync` 이벤트: 초기 동기화 완료
3. `Upsert` 이벤트: 로컬 캐시와 비교 후 변경시에만 처리 (DeepEqual)
4. `Delete` 이벤트: 캐시에서 제거
5. **`event.Done(nil)` 반드시 호출**: 호출하지 않으면 해당 키의 새 이벤트가 발행되지 않음

### 4.4 캐시 동기화

```go
func (k *K8sWatcher) InitK8sSubsystem(ctx context.Context) {
    resources, cachesOnly := k.resourceGroupsFn(k.logger, k.cfg)
    k.enableK8sWatchers(ctx, resources)

    go func() {
        // 모든 리소스가 동기화될 때까지 대기
        allResources := append(resources, cachesOnly...)
        if err := k.k8sResourceSynced.WaitForCacheSyncWithTimeout(
            ctx, option.Config.K8sSyncTimeout, allResources...,
        ); err != nil {
            logging.Fatal(k.logger, "Timed out waiting for resources")
        }
        close(k.k8sCacheStatus)  // 동기화 완료 시그널
    }()
}
```

---

## 5. client-go 사용 패턴

### 5.1 SharedInformer

Cilium은 `k8s.io/client-go/tools/cache` 패키지의 SharedInformer를 사용하되, 자체 래핑 레이어를 추가했다.

**소스**: `pkg/k8s/informer/informer.go`

```go
func NewInformer(
    lw cache.ListerWatcher,
    objType k8sRuntime.Object,
    resyncPeriod time.Duration,
    h cache.ResourceEventHandler,
    transformer cache.TransformFunc,
) (cache.Store, cache.Controller) {
    clientState := cache.NewStore(cache.DeletionHandlingMetaNamespaceKeyFunc)
    return clientState, NewInformerWithStore(lw, objType, resyncPeriod, h, transformer, clientState)
}
```

핵심 차이점:
- **TransformFunc 지원**: 객체가 캐시에 저장되기 전에 변환 수행 (메모리 절약)
- **MutationDetector**: 캐시 객체의 의도치 않은 변경 감지

### 5.2 ListerWatcher

**소스**: `pkg/k8s/resource_ctors.go`

각 CRD 리소스에 대한 ListerWatcher 생성:

```go
func CiliumNodeResource(params CiliumResourceParams, opts ...func(*metav1.ListOptions)) (
    resource.Resource[*cilium_api_v2.CiliumNode], error,
) {
    lw := utils.ListerWatcherWithModifiers(
        utils.ListerWatcherFromTyped[*cilium_api_v2.CiliumNodeList](
            params.ClientSet.CiliumV2().CiliumNodes(),
        ),
        opts...,
    )
    return resource.New[*cilium_api_v2.CiliumNode](
        params.Lifecycle, lw, params.MetricsProvider,
        resource.WithMetric("CiliumNode"),
        resource.WithCRDSync(params.CRDSyncPromise),
    ), nil
}
```

패턴:
1. Typed Client (`CiliumV2().CiliumNodes()`)에서 ListerWatcher 생성
2. `ListerWatcherWithModifiers`로 ListOptions 수정자 적용
3. `resource.New[T]`로 Resource 추상화 생성
4. `WithCRDSync`로 CRD 등록 완료까지 대기 (operator가 CRD를 등록)

### 5.3 WorkQueue

Cilium은 `k8s.io/client-go/util/workqueue` 패키지의 RateLimitingQueue를 사용한다. Resource 추상화 내부에서 자동으로 관리되며, 에러 발생 시 자동 재큐잉을 수행한다.

### 5.4 Indexer

**소스**: `operator/watchers/cilium_endpoint.go`

```go
var indexers = cache.Indexers{
    cache.NamespaceIndex: cache.MetaNamespaceIndexFunc,
    identityIndex:        identityIndexFunc,
}

func identityIndexFunc(obj any) ([]string, error) {
    switch t := obj.(type) {
    case *cilium_api_v2.CiliumEndpoint:
        if t.Status.Identity != nil {
            id := strconv.FormatInt(t.Status.Identity.ID, 10)
            return []string{id}, nil
        }
        return []string{"0"}, nil
    }
    return nil, fmt.Errorf("%w - found %T", errNoCE, obj)
}
```

Indexer를 사용하면 특정 필드 값으로 빠른 조회가 가능하다. 위 예시에서는 Identity ID로 CiliumEndpoint를 조회할 수 있다.

---

## 6. controller-runtime 통합

**소스**: `operator/pkg/controller-runtime/cell.go`

Cilium Operator는 `sigs.k8s.io/controller-runtime` 라이브러리를 Hive 프레임워크와 통합하여 사용한다.

```go
var Cell = cell.Module(
    "controller-runtime",
    "Manages the controller-runtime integration and its components",
    cell.Provide(newScheme),
    cell.Provide(newManager),
)

func newManager(params managerParams) (ctrlRuntime.Manager, error) {
    mgr, err := ctrlRuntime.NewManager(params.K8sClient.RestConfig(), ctrlRuntime.Options{
        Scheme: params.Scheme,
        Metrics: metricsserver.Options{BindAddress: "0"},
    })
    if err != nil {
        return nil, err
    }

    // Hive Job으로 Manager 시작
    params.JobGroup.Add(job.OneShot("manager", func(ctx context.Context, health cell.Health) error {
        return mgr.Start(ctx)
    }))

    return mgr, nil
}
```

controller-runtime Manager의 역할:
1. 여러 Controller의 생명주기 관리
2. 공유 Cache(SharedInformer) 제공
3. Leader Election 지원
4. Webhook Server 관리

Operator에서 controller-runtime을 사용하는 모듈들:
- **Gateway API**: HTTP Route, Gateway 등의 리소스 관리
- **Ingress**: Kubernetes Ingress를 Cilium Envoy Config으로 변환
- **CiliumEnvoyConfig**: L7 프록시 설정 관리
- **CiliumEndpointSlice**: 엔드포인트 슬라이스 관리
- **BGP Control Plane**: BGP 설정 관리
- **LB IPAM**: LoadBalancer IP 할당

---

## 7. pkg/k8s/ 패키지 구조

### 7.1 디렉토리 구조

```
pkg/k8s/
|-- apis/
|   `-- cilium.io/
|       |-- v2/           # v2 CRD 타입 정의
|       |   |-- types.go          # CEP, CID, CN
|       |   |-- cnp_types.go      # CNP
|       |   |-- ccnp_types.go     # CCNP
|       |   |-- cec_types.go      # CEC
|       |   |-- ccec_types.go     # CCEC
|       |   |-- clrp_types.go     # CLRP
|       |   |-- cegp_types.go     # CEGP
|       |   |-- cnc_types.go      # CiliumNodeConfig
|       |   |-- lbipam_types.go   # CiliumLoadBalancerIPPool
|       |   |-- bgp_*.go          # BGP 관련 타입들
|       |   |-- register.go       # Scheme 등록
|       |   `-- validator/        # Webhook Validation
|       `-- v2alpha1/     # v2alpha1 CRD 타입 정의
|           |-- types.go          # CiliumEndpointSlice
|           |-- l2announcement_types.go
|           |-- ippool_types.go   # CiliumPodIPPool
|           `-- register.go
|-- client/               # 생성된 Kubernetes 클라이언트
|   |-- clientset/        # Typed Clientset
|   |-- informers/        # 생성된 SharedInformerFactory
|   |-- listers/          # 생성된 Lister
|   `-- cell.go           # Hive Cell
|-- informer/             # 커스텀 Informer 래퍼
|   `-- informer.go
|-- resource/             # Resource[T] 추상화
|   `-- resource.go
|-- watchers/             # K8s 리소스 감시자
|   |-- cell.go
|   |-- watcher.go
|   |-- cilium_endpoint.go
|   |-- cilium_node.go
|   `-- pod.go
|-- slim/                 # 메모리 최적화된 K8s 타입
|-- synced/               # 동기화 상태 추적
|-- types/                # 내부 타입 정의
|-- resource_ctors.go     # Resource 생성자 함수
`-- factory_functions.go  # 변환 함수
```

### 7.2 주요 패키지 역할

#### `pkg/k8s/apis/cilium.io/v2/`
모든 CRD의 Go 타입이 정의되어 있다. kubebuilder 마커가 포함되어 있으며, 이를 기반으로 CRD YAML, DeepCopy 코드, OpenAPI Schema가 자동 생성된다.

#### `pkg/k8s/client/`
`client-gen`, `informer-gen`, `lister-gen` 도구로 자동 생성된 코드다. Typed Client, SharedInformer, Lister를 제공한다.

#### `pkg/k8s/resource/`
Cilium의 핵심 추상화인 `Resource[T]` 인터페이스가 정의되어 있다. SharedInformer 위에 이벤트 스트림, 에러 처리, 재시도 로직을 추가한다.

#### `pkg/k8s/watchers/`
cilium-agent의 K8s 리소스 감시 로직이 구현되어 있다. 각 리소스별 Watcher가 분리되어 있다.

#### `pkg/k8s/slim/`
메모리 사용량을 줄이기 위해 Kubernetes 코어 타입(Pod, Service, Node 등)에서 불필요한 필드를 제거한 "slim" 버전이 있다.

---

## 8. CRD에서 내부 데이터 변환

### 8.1 CiliumNetworkPolicy 변환

**CNP -> 내부 Policy Rules**

```
CiliumNetworkPolicy (K8s CRD)
    |
    | Parse()
    v
api.Rules (내부 정책 규칙)
    |
    | PolicyRepository.Add()
    v
PolicyRepository (정책 저장소)
    |
    | SelectorCache 업데이트
    v
Endpoint.Regenerate()
    |
    v
BPF Policy Map (데이터플레인)
```

**소스**: `pkg/k8s/apis/cilium.io/v2/cnp_types.go`
```go
func (r *CiliumNetworkPolicy) Parse(logger *slog.Logger, clusterName string) (api.Rules, error) {
    namespace := k8sUtils.ExtractNamespace(&r.ObjectMeta)
    name := r.ObjectMeta.Name
    uid := r.ObjectMeta.UID

    retRules := api.Rules{}
    if r.Spec != nil {
        if err := r.Spec.Sanitize(); err != nil {
            return nil, NewErrParse(...)
        }
        cr := k8sCiliumUtils.ParseToCiliumRule(logger, clusterName, namespace, name, uid, r.Spec)
        retRules = append(retRules, cr)
    }
    // Specs 처리도 유사
    return retRules, nil
}
```

### 8.2 CiliumEndpoint 변환 (Transform)

**소스**: `operator/watchers/cilium_endpoint.go`

Operator에서는 메모리 절약을 위해 CRD 객체의 불필요한 필드를 제거하는 Transform 함수를 사용한다:

```go
func transformToCiliumEndpoint(obj any) (any, error) {
    switch concreteObj := obj.(type) {
    case *cilium_api_v2.CiliumEndpoint:
        p := &cilium_api_v2.CiliumEndpoint{
            TypeMeta: concreteObj.TypeMeta,
            ObjectMeta: metav1.ObjectMeta{
                Name:            concreteObj.Name,
                Namespace:       concreteObj.Namespace,
                ResourceVersion: concreteObj.ResourceVersion,
                OwnerReferences: concreteObj.OwnerReferences,
                UID:             concreteObj.UID,
            },
            Status: cilium_api_v2.EndpointStatus{
                Identity:   concreteObj.Status.Identity,
                Networking: concreteObj.Status.Networking,
                NamedPorts: concreteObj.Status.NamedPorts,
                Encryption: concreteObj.Status.Encryption,
            },
        }
        *concreteObj = cilium_api_v2.CiliumEndpoint{} // GC를 위해 원본 비우기
        return p, nil
    }
}
```

### 8.3 Agent의 CiliumSlimEndpoint 변환

**소스**: `pkg/k8s/resource_ctors.go`

Agent 측에서도 유사하게 LazyTransform을 사용한다:

```go
func CiliumSlimEndpointResource(...) (resource.Resource[*types.CiliumEndpoint], error) {
    return resource.New[*types.CiliumEndpoint](params.Lifecycle, lw, params.MetricsProvider,
        resource.WithLazyTransform(func() k8sRuntime.Object {
            return &cilium_api_v2.CiliumEndpoint{}
        }, TransformToCiliumEndpoint),
        resource.WithMetric("CiliumEndpoint"),
        resource.WithIndexers(indexers),
        resource.WithCRDSync(params.CRDSyncPromise),
    ), nil
}
```

`LazyTransform`은 객체가 실제로 캐시에 저장될 때만 변환을 수행하여, List 응답의 전체 객체를 메모리에 유지하지 않는다.

---

## 9. Status 업데이트 패턴

### 9.1 CiliumEndpoint Status

CEP의 Status는 cilium-agent가 엔드포인트의 실시간 상태를 반영한다.

```
Pod 생성
    |
    v
cilium-agent: endpoint 생성
    |
    v
CEP 생성 (Status.State = "creating")
    |
    v
Identity 할당 (Status.Identity.ID = 12345)
    |
    v
IP 할당 (Status.Networking.Addressing = [{ipv4: "10.0.0.5"}])
    |
    v
BPF 프로그램 생성 (Status.State = "ready")
    |
    v
Policy 적용 (Status.Policy.Ingress.Enforcing = true)
```

### 9.2 CiliumNetworkPolicy Status

CNP의 Status는 각 노드에서의 정책 적용 상태를 추적한다:

```go
type CiliumNetworkPolicyStatus struct {
    DerivativePolicies map[string]CiliumNetworkPolicyNodeStatus `json:"derivativePolicies,omitempty"`
    Conditions         []NetworkPolicyCondition                 `json:"conditions,omitempty"`
}

type CiliumNetworkPolicyNodeStatus struct {
    OK         bool   `json:"ok,omitempty"`
    Error      string `json:"error,omitempty"`
    Enforcing  bool   `json:"enforcing,omitempty"`
    Revision   uint64 `json:"localPolicyRevision,omitempty"`
}
```

### 9.3 Status 업데이트 전략

Cilium은 Status 업데이트 시 다음 전략을 사용한다:

1. **Subresource 분리**: `/status` 서브리소스를 사용하여 Spec과 독립적으로 업데이트
2. **ResourceVersion 체크**: Optimistic Concurrency Control (OCC)로 충돌 방지
3. **Rate Limiting**: 빈번한 업데이트를 제어하여 API Server 부하 방지
4. **배치 업데이트**: 여러 변경을 모아서 한 번에 업데이트

---

## 10. Operator의 역할

**소스**: `operator/cmd/root.go`

### 10.1 주요 기능

Cilium Operator는 클러스터 단위 작업을 수행한다:

```go
// Operator가 관리하는 주요 모듈
- endpointgc        // CiliumEndpoint GC
- endpointslicegc   // EndpointSlice GC
- identitygc        // CiliumIdentity GC
- lbipam            // LoadBalancer IP 할당
- ipam              // Node IPAM (AWS ENI, Azure 등)
- bgp               // BGP 설정 관리
- gatewayapi        // Gateway API 지원
- ingress           // Ingress 지원
- ciliumenvoyconfig // Envoy 설정 관리
- ciliumidentity    // Identity CRD 관리
- ciliumendpointslice // CES 관리
- networkpolicy     // 정책 검증
```

### 10.2 CRD 등록

Operator는 클러스터에 Cilium CRD를 등록하는 책임을 진다. Agent는 CRD가 등록될 때까지 대기한다.

```
Operator 시작
    |
    v
CRD YAML 적용 (CustomResourceDefinition 생성)
    |
    v
CRD 등록 완료 시그널 (CRDSync Promise)
    |
    v
Agent: CRD 사용 가능, Informer 시작
```

### 10.3 Endpoint GC

**소스**: `operator/endpointgc/gc.go`

Operator는 주기적으로 고아 CiliumEndpoint(연결된 Pod이 없는 CEP)를 정리한다:

```go
type GC struct {
    once     bool
    interval time.Duration

    clientset       k8sClient.Clientset
    ciliumEndpoints resource.Resource[*cilium_api_v2.CiliumEndpoint]
    pods            resource.Resource[*slim_corev1.Pod]
}
```

GC 로직:
1. 모든 CiliumEndpoint를 가져옴
2. 각 CEP에 대해 해당 Pod 존재 여부 확인
3. Pod이 없으면 CEP 삭제

### 10.4 Identity GC

**소스**: `operator/identitygc/gc.go`

사용하지 않는 CiliumIdentity를 정리한다:

```go
type GC struct {
    identity            resource.Resource[*v2.CiliumIdentity]
    ciliumEndpoint      resource.Resource[*v2.CiliumEndpoint]
    ciliumEndpointSlice resource.Resource[*v2alpha1.CiliumEndpointSlice]
    allocator           *allocator.Allocator
    rateLimiter         *rate.Limiter
}
```

Identity GC 로직:
1. 모든 CiliumEndpoint/CiliumEndpointSlice의 Identity 참조 수집
2. 참조되지 않는 CiliumIdentity에 heartbeat 없음을 표시
3. heartbeat timeout 후 Identity 삭제
4. Rate Limiter로 삭제 속도 제어

### 10.5 CiliumNode GC

**소스**: `operator/watchers/cilium_node_gc.go`

Kubernetes Node가 삭제된 후 남아있는 CiliumNode 리소스를 정리한다:

```go
func RunCiliumNodeGC(ctx context.Context, ...) {
    // CiliumNode가 K8s Node에 매칭되지 않으면 후보로 등록
    // 일정 시간 후에도 매칭되지 않으면 삭제
    // "cilium.io/do-not-gc" 어노테이션이 있으면 건너뜀
}
```

---

## 11. Hive 프레임워크와 Resource 추상화

### 11.1 Resource[T] 인터페이스

**소스**: `pkg/k8s/resource/resource.go`

Cilium의 핵심 K8s 통합 추상화이다:

```go
type Resource[T k8sRuntime.Object] interface {
    // Observable 패턴
    stream.Observable[Event[T]]

    // 이벤트 채널 반환
    Events(ctx context.Context, opts ...EventsOpt) <-chan Event[T]

    // 읽기 전용 Store 반환 (동기화 완료까지 블로킹)
    Store(context.Context) (Store[T], error)
}
```

`Resource[T]`의 특성:
1. **지연 초기화**: Events() 또는 Store() 호출 전까지 Informer를 시작하지 않음
2. **이벤트 순서**: Upsert(현재 상태 재생) -> Sync(동기화 완료) -> 증분 업데이트
3. **에러 처리**: Done(err)로 에러 보고, 기본적으로 재큐잉
4. **필수 Done()**: Event를 받으면 반드시 Done()을 호출해야 함 (안 하면 panic)

### 11.2 Hive Cell 통합

**소스**: `pkg/k8s/watchers/cell.go`

```go
var Cell = cell.Module(
    "k8s-watcher",
    "K8s Watcher",
    cell.Provide(newK8sWatcher),
    cell.ProvidePrivate(newK8sPodWatcher),
    cell.Provide(newK8sCiliumNodeWatcher),
    cell.ProvidePrivate(newK8sCiliumEndpointsWatcher),
    cell.Provide(newK8sEventReporter),
)
```

의존성 주입을 통해 Resource, Client, 설정 등이 자동으로 연결된다:

```go
type k8sCiliumEndpointsWatcherParams struct {
    cell.In

    CiliumSlimEndpoint  resource.Resource[*types.CiliumEndpoint]
    CiliumEndpointSlice resource.Resource[*cilium_api_v2a1.CiliumEndpointSlice]
    K8sResourceSynced   *k8sSynced.Resources
    IPCache             *ipcache.IPCache
    // ...
}
```

### 11.3 CRDSync Promise

CRD가 등록되기 전에 Informer를 시작하면 에러가 발생한다. `CRDSync Promise`는 이를 해결한다:

```go
func CiliumNodeResource(params CiliumResourceParams, ...) (resource.Resource[*cilium_api_v2.CiliumNode], error) {
    return resource.New[*cilium_api_v2.CiliumNode](
        params.Lifecycle, lw, params.MetricsProvider,
        resource.WithCRDSync(params.CRDSyncPromise), // CRD 등록 완료까지 대기
    ), nil
}
```

---

## 12. 관련 소스 파일 맵

### CRD 타입 정의
| 파일 | 내용 |
|------|------|
| `pkg/k8s/apis/cilium.io/v2/types.go` | CEP, CID, CN 타입 |
| `pkg/k8s/apis/cilium.io/v2/cnp_types.go` | CNP 타입 |
| `pkg/k8s/apis/cilium.io/v2/ccnp_types.go` | CCNP 타입 |
| `pkg/k8s/apis/cilium.io/v2/cec_types.go` | CEC 타입 |
| `pkg/k8s/apis/cilium.io/v2/ccec_types.go` | CCEC 타입 |
| `pkg/k8s/apis/cilium.io/v2/clrp_types.go` | CLRP 타입 |
| `pkg/k8s/apis/cilium.io/v2/cegp_types.go` | CEGP 타입 |
| `pkg/k8s/apis/cilium.io/v2/cnc_types.go` | CNC 타입 |
| `pkg/k8s/apis/cilium.io/v2/lbipam_types.go` | LBIPPool 타입 |
| `pkg/k8s/apis/cilium.io/v2/bgp_cluster_types.go` | BGP 클러스터 설정 |
| `pkg/k8s/apis/cilium.io/v2/bgp_peer_types.go` | BGP 피어 설정 |
| `pkg/k8s/apis/cilium.io/v2/bgp_advert_types.go` | BGP 광고 설정 |
| `pkg/k8s/apis/cilium.io/v2/bgp_node_types.go` | BGP 노드 설정 |
| `pkg/k8s/apis/cilium.io/v2/cidrgroups_types.go` | CIDR 그룹 |
| `pkg/k8s/apis/cilium.io/v2/register.go` | v2 Scheme 등록 |
| `pkg/k8s/apis/cilium.io/v2alpha1/types.go` | CES 타입 |
| `pkg/k8s/apis/cilium.io/v2alpha1/l2announcement_types.go` | L2 광고 |
| `pkg/k8s/apis/cilium.io/v2alpha1/ippool_types.go` | PodIPPool |
| `pkg/k8s/apis/cilium.io/v2alpha1/register.go` | v2alpha1 Scheme 등록 |

### K8s 통합
| 파일 | 내용 |
|------|------|
| `pkg/k8s/resource/resource.go` | Resource[T] 추상화 |
| `pkg/k8s/resource_ctors.go` | Resource 생성자 |
| `pkg/k8s/informer/informer.go` | 커스텀 Informer |
| `pkg/k8s/watchers/watcher.go` | K8sWatcher 코어 |
| `pkg/k8s/watchers/cell.go` | Watcher Hive Cell |
| `pkg/k8s/watchers/cilium_endpoint.go` | CEP Watcher |
| `pkg/k8s/watchers/cilium_node.go` | CN Watcher |
| `pkg/k8s/watchers/pod.go` | Pod Watcher |
| `pkg/k8s/client/cell.go` | Client Hive Cell |
| `pkg/k8s/client/clientset/` | 생성된 Clientset |
| `pkg/k8s/synced/` | 동기화 상태 추적 |

### Operator
| 파일 | 내용 |
|------|------|
| `operator/cmd/root.go` | Operator 진입점 |
| `operator/watchers/cilium_endpoint.go` | Operator CEP 감시 |
| `operator/watchers/cilium_node_gc.go` | CN GC |
| `operator/endpointgc/gc.go` | CEP GC |
| `operator/identitygc/gc.go` | Identity GC |
| `operator/pkg/lbipam/lbipam.go` | LB IPAM |
| `operator/pkg/controller-runtime/cell.go` | controller-runtime 통합 |

---

## 요약

Cilium의 CRD/K8s 통합은 다음과 같은 특징이 있다:

1. **대규모 CRD 생태계**: 20개 이상의 CRD로 네트워크 정책, 엔드포인트, Identity, BGP, IPAM, L7 프록시 등을 선언적으로 관리
2. **성능 최적화**: slim 타입, Transform 함수, 지연 초기화, DeepEqual 비교로 메모리와 API 호출을 최소화
3. **Resource[T] 추상화**: client-go의 SharedInformer 위에 타입 안전하고 에러 처리가 내장된 이벤트 스트림 제공
4. **이중 바이너리 아키텍처**: Agent(노드별)와 Operator(클러스터별)가 역할을 분담
5. **Hive 프레임워크 통합**: 의존성 주입으로 모듈 간 결합도를 낮추고 테스트 용이성을 높임
6. **CRD 생명주기 관리**: Operator가 CRD 등록, Agent가 CRD 등록 완료를 대기 후 Informer 시작
