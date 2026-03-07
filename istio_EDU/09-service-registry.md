# 09. Service Registry Deep-Dive

## 목차

1. [개요](#1-개요)
2. [핵심 인터페이스 계층 구조](#2-핵심-인터페이스-계층-구조)
3. [Aggregate Controller 패턴](#3-aggregate-controller-패턴)
4. [Kubernetes Controller: 리소스 감시자](#4-kubernetes-controller-리소스-감시자)
5. [이벤트 흐름: Informer에서 xDS Push까지](#5-이벤트-흐름-informer에서-xds-push까지)
6. [ConvertService(): K8s에서 Istio 모델로](#6-convertservice-k8s에서-istio-모델로)
7. [EndpointSlice 처리](#7-endpointslice-처리)
8. [PodCache: IP-Pod 매핑과 최종 일관성](#8-podcache-ip-pod-매핑과-최종-일관성)
9. [ServiceEntry Controller: 외부 서비스 등록](#9-serviceentry-controller-외부-서비스-등록)
10. [멀티클러스터 서비스 디스커버리](#10-멀티클러스터-서비스-디스커버리)
11. [왜 이렇게 설계했나](#11-왜-이렇게-설계했나)

---

## 1. 개요

Istio의 Service Registry(서비스 레지스트리)는 메시 내부의 모든 서비스와 그 엔드포인트 정보를 관리하는 핵심 서브시스템이다. Pilot(istiod)은 Kubernetes API 서버, ServiceEntry CRD, 그리고 잠재적으로 다른 플랫폼의 서비스 정보를 통합하여 Envoy 프록시에 전달할 xDS 설정을 생성한다.

서비스 레지스트리의 핵심 역할은 다음 세 가지다:

1. **서비스 발견(Service Discovery)**: 메시 내의 모든 서비스 목록과 그 속성(포트, VIP, 해석 방식 등) 관리
2. **엔드포인트 추적**: 각 서비스에 속하는 실제 워크로드 인스턴스(Pod IP, 포트 등) 추적
3. **변경 전파**: 서비스/엔드포인트 변경 시 xDS 업데이트를 트리거하여 프록시 설정 갱신

```
+------------------------------------------------------------------+
|                         istiod (Pilot)                            |
|                                                                   |
|  +------------------------------------------------------------+  |
|  |              Aggregate Controller                           |  |
|  |                                                             |  |
|  |  +------------------+  +------------------+  +-----------+  |  |
|  |  | K8s Controller   |  | K8s Controller   |  | Service   |  |  |
|  |  | (Cluster A)      |  | (Cluster B)      |  | Entry     |  |  |
|  |  |                  |  |                  |  | Controller|  |  |
|  |  | - Services       |  | - Services       |  |           |  |  |
|  |  | - EndpointSlices |  | - EndpointSlices |  | - SE CRDs |  |  |
|  |  | - Pods           |  | - Pods           |  | - WLE CRDs|  |  |
|  |  | - Nodes          |  | - Nodes          |  |           |  |  |
|  |  +------------------+  +------------------+  +-----------+  |  |
|  +------------------------------------------------------------+  |
|                              |                                    |
|                     ServiceDiscovery                              |
|                     인터페이스 구현                                  |
|                              |                                    |
|                       +------v------+                             |
|                       | XDS Server  |                             |
|                       | (EDS, CDS,  |                             |
|                       |  LDS, RDS)  |                             |
|                       +-------------+                             |
+------------------------------------------------------------------+
```

소스코드 위치:
- 인터페이스 정의: `pilot/pkg/model/service.go`, `pilot/pkg/model/controller.go`
- Aggregate Controller: `pilot/pkg/serviceregistry/aggregate/controller.go`
- K8s Controller: `pilot/pkg/serviceregistry/kube/controller/controller.go`
- ServiceEntry Controller: `pilot/pkg/serviceregistry/serviceentry/controller.go`

---

## 2. 핵심 인터페이스 계층 구조

Istio 서비스 레지스트리의 설계는 인터페이스 기반의 추상화 계층으로 구성된다. 이 계층 구조를 이해하면 전체 시스템의 동작을 파악할 수 있다.

### 2.1 인터페이스 계층 다이어그램

```
+--------------------------------------------------+
|          serviceregistry.Instance                 |
|  (pilot/pkg/serviceregistry/instance.go)          |
|                                                    |
|  model.Controller                                  |
|    + AppendServiceHandler(f ServiceHandler)        |
|    + AppendWorkloadHandler(f func(...))            |
|    + Run(stop <-chan struct{})                      |
|    + HasSynced() bool                              |
|                                                    |
|  model.ServiceDiscovery                            |
|    + Services() []*Service                         |
|    + GetService(hostname) *Service                 |
|    + GetProxyServiceTargets(*Proxy) []ServiceTarget|
|    + GetProxyWorkloadLabels(*Proxy) labels.Instance|
|    + MCSServices() []MCSServiceInfo                |
|    + NetworkGateways() []NetworkGateway            |
|    + AmbientIndexes                                |
|                                                    |
|  Provider() provider.ID                            |
|  Cluster() cluster.ID                              |
+--------------------------------------------------+
          ^                    ^                ^
          |                    |                |
   +------+-----+    +--------+-----+   +------+--------+
   | K8s         |    | ServiceEntry |   | Mock          |
   | Controller  |    | Controller   |   | (테스트용)     |
   +-------------+    +--------------+   +---------------+
```

### 2.2 ServiceDiscovery 인터페이스

`model.ServiceDiscovery`는 서비스 레지스트리의 읽기 측면을 정의한다. 이 인터페이스는 플랫폼에 독립적이며, 어떤 플랫폼이든 이 인터페이스만 구현하면 Istio와 통합할 수 있다.

```go
// pilot/pkg/model/service.go (928행)
type ServiceDiscovery interface {
    NetworkGatewaysWatcher

    // Services: 시스템 내 모든 서비스 목록 반환
    Services() []*Service

    // GetService: 호스트명으로 서비스 조회
    GetService(hostname host.Name) *Service

    // GetProxyServiceTargets: 프록시와 같은 위치에 있는 서비스 타겟 반환
    GetProxyServiceTargets(*Proxy) []ServiceTarget
    GetProxyWorkloadLabels(*Proxy) labels.Instance

    // MCSServices: Multi-Cluster Services 정보 반환
    MCSServices() []MCSServiceInfo
    AmbientIndexes
}
```

핵심 메서드의 역할:

| 메서드 | 용도 | 호출 시점 |
|--------|------|-----------|
| `Services()` | 전체 서비스 목록 | CDS/LDS 생성 시 |
| `GetService()` | 특정 서비스 조회 | EDS 업데이트 시 |
| `GetProxyServiceTargets()` | 프록시의 서비스 타겟 | SDS(인증서) 생성 시 |
| `GetProxyWorkloadLabels()` | 프록시 워크로드 라벨 | RBAC 정책 평가 시 |
| `MCSServices()` | MCS 정보 | 멀티클러스터 동기화 시 |

### 2.3 Controller 인터페이스

`model.Controller`는 변경 이벤트의 구독/발행 측면을 정의한다.

```go
// pilot/pkg/model/controller.go (39행)
type Controller interface {
    // AppendServiceHandler: 서비스 변경 핸들러 등록
    AppendServiceHandler(f ServiceHandler)

    // AppendWorkloadHandler: 워크로드 변경 핸들러 등록
    AppendWorkloadHandler(f func(*WorkloadInstance, Event))

    // Run: 컨트롤러 시작
    Run(stop <-chan struct{})

    // HasSynced: 초기 동기화 완료 여부
    HasSynced() bool
}
```

`AggregateController`는 `Controller`를 확장하여 클러스터별 핸들러 등록을 지원한다:

```go
// pilot/pkg/model/controller.go (58행)
type AggregateController interface {
    Controller
    AppendServiceHandlerForCluster(clusterID cluster.ID, f ServiceHandler)
    UnRegisterHandlersForCluster(clusterID cluster.ID)
}
```

### 2.4 serviceregistry.Instance 인터페이스

개별 서비스 레지스트리 인스턴스(K8s Controller, ServiceEntry Controller)가 구현하는 통합 인터페이스다.

```go
// pilot/pkg/serviceregistry/instance.go (25행)
type Instance interface {
    model.Controller
    model.ServiceDiscovery

    // Provider: 레지스트리 제공자 (Kubernetes, External, Mock)
    Provider() provider.ID

    // Cluster: 레지스트리가 속한 클러스터 ID
    Cluster() cluster.ID
}
```

제공자(Provider) 종류:

```go
// pilot/pkg/serviceregistry/provider/providers.go
const (
    Mock       ID = "Mock"       // 테스트용
    Kubernetes ID = "Kubernetes" // K8s API 서버 기반
    External   ID = "External"   // ServiceEntry 기반
)
```

---

## 3. Aggregate Controller 패턴

### 3.1 설계 목적

Aggregate Controller는 여러 개의 서비스 레지스트리를 하나의 통합 뷰로 제공하는 파사드(Facade) 패턴이다. 멀티클러스터 환경에서 각 클러스터는 자체 K8s Controller를 가지며, 여기에 ServiceEntry Controller가 추가된다. Aggregate Controller는 이들을 모두 통합하여 xDS 서버에 단일 `ServiceDiscovery` 인터페이스를 노출한다.

```
+---------------------------------------------------------------+
|                    Aggregate Controller                        |
|                                                                |
|  registries: []registryEntry                                   |
|  +-----------+-----------+-----------+-----------+             |
|  | K8s       | K8s       | K8s       | Service   |             |
|  | Cluster-A | Cluster-B | Cluster-C | Entry     |             |
|  | (Primary) | (Remote)  | (Remote)  | Controller|             |
|  +-----------+-----------+-----------+-----------+             |
|  ^-- Kubernetes 레지스트리가 항상 먼저 배치됨 --^                  |
|                                                                |
|  handlers: ControllerHandlers (글로벌 핸들러)                    |
|  handlersByCluster: map[cluster.ID]*ControllerHandlers         |
+---------------------------------------------------------------+
```

### 3.2 레지스트리 정렬과 K8s 우선순위

Aggregate Controller는 레지스트리를 추가할 때 **Kubernetes 레지스트리를 비-Kubernetes 레지스트리 앞에 배치**한다. 이 순서는 서비스 조회 시 K8s 서비스가 우선권을 갖도록 보장한다.

```go
// pilot/pkg/serviceregistry/aggregate/controller.go (212행)
func (c *Controller) addRegistry(registry serviceregistry.Instance, stop <-chan struct{}) {
    added := false
    if registry.Provider() == provider.Kubernetes {
        for i, r := range c.registries {
            if r.Provider() != provider.Kubernetes {
                // 첫 번째 비-Kubernetes 레지스트리 위치에 삽입
                c.registries = slices.Insert(c.registries, i,
                    &registryEntry{Instance: registry, stop: stop})
                added = true
                break
            }
        }
    }
    if !added {
        c.registries = append(c.registries, &registryEntry{...})
    }
    // 이벤트 핸들러 연결
    registry.AppendNetworkGatewayHandler(c.NotifyGatewayHandlers)
    registry.AppendServiceHandler(c.handlers.NotifyServiceHandlers)
}
```

이 정렬 전략의 의미:

```
registries 배열 예시:
  [0] K8s-ClusterA (Primary)    <-- Kubernetes 먼저
  [1] K8s-ClusterB (Remote)     <-- Kubernetes 먼저
  [2] ServiceEntry Controller   <-- 비-Kubernetes 뒤에
```

### 3.3 서비스 병합 (mergeService)

동일한 호스트명을 가진 서비스가 여러 클러스터에 존재할 때, Aggregate Controller는 이를 **하나의 서비스로 병합**한다. 각 클러스터의 VIP(ClusterIP)를 `ClusterVIPs` 맵에 저장하고, 서비스 계정(ServiceAccount)을 합친다.

```go
// pilot/pkg/serviceregistry/aggregate/controller.go (340행)
func (c *Controller) Services() []*Service {
    smap := make(map[host.Name]int)  // hostname -> 결과 배열의 인덱스
    index := 0
    services := make([]*model.Service, 0)

    for _, r := range c.GetRegistries() {
        svcs := r.Services()
        if r.Provider() != provider.Kubernetes {
            // 비-K8s 서비스는 병합 없이 그대로 추가
            index += len(svcs)
            services = append(services, svcs...)
        } else {
            for _, s := range svcs {
                previous, ok := smap[s.Hostname]
                if !ok {
                    // 처음 본 서비스: 그대로 추가
                    smap[s.Hostname] = index
                    index++
                    services = append(services, s)
                } else {
                    // 이미 본 서비스: 병합 (ClusterVIPs, ServiceAccounts)
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

병합 로직의 핵심:

```go
// pilot/pkg/serviceregistry/aggregate/controller.go (402행)
func mergeService(dst, src *model.Service, srcRegistry serviceregistry.Instance) {
    // 포트 불일치 시 경고 로그
    if !src.Ports.Equals(dst.Ports) {
        log.Debugf("service %s defined from cluster %s is different", ...)
    }
    // 소스 클러스터의 VIP 추가
    clusterID := srcRegistry.Cluster()
    if len(dst.ClusterVIPs.GetAddressesFor(clusterID)) == 0 {
        newAddresses := src.ClusterVIPs.GetAddressesFor(clusterID)
        dst.ClusterVIPs.SetAddressesFor(clusterID, newAddresses)
    }
    // 서비스 계정 병합 (중복 제거)
    if len(src.ServiceAccounts) > 0 {
        sas := make([]string, 0, len(dst.ServiceAccounts)+len(src.ServiceAccounts))
        sas = append(sas, dst.ServiceAccounts...)
        sas = append(sas, src.ServiceAccounts...)
        dst.ServiceAccounts = slices.FilterDuplicates(sas)
    }
}
```

### 3.4 프록시별 레지스트리 필터링

프록시의 서비스 타겟을 조회할 때, 프록시가 속한 클러스터의 K8s 레지스트리만 검색한다. 이 최적화는 불필요한 교차 클러스터 조회를 방지한다.

```go
// pilot/pkg/serviceregistry/aggregate/controller.go (448행)
func skipSearchingRegistryForProxy(nodeClusterID cluster.ID, r serviceregistry.Instance) bool {
    // 비-K8s 레지스트리(ServiceEntry)는 항상 검색
    // 클러스터 ID가 없으면 모든 레지스트리 검색
    if r.Provider() != provider.Kubernetes || nodeClusterID == "" {
        return false
    }
    // K8s 레지스트리는 프록시와 같은 클러스터만 검색
    return !r.Cluster().Equals(nodeClusterID)
}
```

### 3.5 레지스트리 생명주기 관리

Aggregate Controller는 레지스트리의 동적 추가/삭제/교체를 지원한다:

| 메서드 | 용도 | 사용 시점 |
|--------|------|-----------|
| `AddRegistry()` | 레지스트리 추가 (시작 전) | 초기화 단계 |
| `AddRegistryAndRun()` | 추가 후 즉시 시작 | 멀티클러스터 런타임 추가 |
| `DeleteRegistry()` | 레지스트리 제거 | 클러스터 연결 해제 시 |
| `UpdateRegistry()` | 원자적 교체 | 자격 증명(credential) 갱신 시 |

`UpdateRegistry()`는 서비스 중단 없이 기존 레지스트리를 새 것으로 교체하기 위해 설계되었다:

```go
// pilot/pkg/serviceregistry/aggregate/controller.go (289행)
func (c *Controller) UpdateRegistry(newRegistry serviceregistry.Instance, stop <-chan struct{}) {
    c.storeLock.Lock()
    defer c.storeLock.Unlock()

    index, ok := c.getRegistryIndex(clusterID, providerID)
    if ok {
        // 배열 내 원자적 교체
        c.registries[index] = &registryEntry{Instance: newRegistry, stop: stop}
    } else {
        c.addRegistry(newRegistry, stop)
    }

    if c.running {
        go newRegistry.Run(stop)
    }
}
```

---

## 4. Kubernetes Controller: 리소스 감시자

### 4.1 Controller 구조체

K8s Controller는 Kubernetes API 서버의 여러 리소스를 Informer로 감시하는 복합 컨트롤러다.

```go
// pilot/pkg/serviceregistry/kube/controller/controller.go (193행)
type Controller struct {
    opts   Options
    client kubelib.Client
    queue  queue.Instance                  // 이벤트 직렬화 큐

    namespaces kclient.Client[*v1.Namespace] // 네임스페이스 감시
    services   kclient.Client[*v1.Service]   // 서비스 감시
    endpoints  *endpointSliceController      // EndpointSlice 감시
    nodes      kclient.Client[*v1.Node]      // 노드 감시
    pods       *PodCache                     // Pod 캐시

    exports serviceExportCache   // MCS ServiceExport 캐시
    imports serviceImportCache   // MCS ServiceImport 캐시
    handlers model.ControllerHandlers  // 이벤트 핸들러 목록

    servicesMap map[host.Name]*model.Service           // hostname -> Service
    nodeSelectorsForServices map[host.Name]labels.Instance
    nodeInfoMap map[string]kubernetesNode             // nodeName -> nodeInfo
    workloadInstancesIndex workloadinstances.Index

    *networkManager     // 네트워크 토폴로지 관리
    ambientIndex        // Ambient Mesh 인덱스
}
```

### 4.2 감시하는 K8s 리소스

```
+----------------+     +------------------+     +-----------------+
| Services       |     | EndpointSlices   |     | Pods            |
| (v1.Service)   |     | (discovery/v1)   |     | (v1.Pod)        |
|                |     |                  |     |                 |
| -> onServiceEv |     | -> onEvent       |     | -> pods.onEvent |
+-------+--------+     +--------+---------+     +--------+--------+
        |                       |                        |
        v                       v                        v
+-------+-------+     +---------+--------+     +---------+-------+
| servicesMap   |     | endpointCache    |     | podsByIP        |
| (캐시 갱신)   |     | (EDS 캐시 갱신)  |     | (IP 인덱스 갱신) |
+-------+-------+     +---------+--------+     +---------+-------+
        |                       |                        |
        +----------+------------+------------------------+
                   |
           +-------v-------+
           |  XDSUpdater   |
           |  (push 트리거) |
           +---------------+

+----------------+     +------------------+
| Nodes          |     | Namespaces       |
| (v1.Node)      |     | (v1.Namespace)   |
|                |     |                  |
| -> onNodeEvent |     | -> onSystemNS    |
+-------+--------+     +--------+---------+
        |                       |
     NodePort                네트워크 레이블
     주소 업데이트             변경 처리
```

### 4.3 NewController: 초기화 흐름

`NewController()`는 모든 Informer를 생성하고 이벤트 핸들러를 등록하는 팩토리 함수다.

```go
// pilot/pkg/serviceregistry/kube/controller/controller.go (251행)
func NewController(kubeClient kubelib.Client, options Options) *Controller {
    c := &Controller{
        opts:                     options,
        client:                   kubeClient,
        queue:                    queue.NewQueueWithID(1*time.Second, string(options.ClusterID)),
        servicesMap:              make(map[host.Name]*model.Service),
        nodeSelectorsForServices: make(map[host.Name]labels.Instance),
        nodeInfoMap:              make(map[string]kubernetesNode),
        workloadInstancesIndex:   workloadinstances.NewIndex(),
        initialSyncTimedout:      atomic.NewBool(false),
        configCluster:            options.ConfigCluster,
    }
    c.networkManager = initNetworkManager(c, options)

    // 1. 네임스페이스 Informer
    c.namespaces = kclient.New[*v1.Namespace](kubeClient)

    // 2. 서비스 Informer (ObjectFilter로 네임스페이스 필터링)
    c.services = kclient.NewFiltered[*v1.Service](kubeClient,
        kclient.Filter{ObjectFilter: kubeClient.ObjectFilter()})
    registerHandlers(c, c.services, "Services", c.onServiceEvent, nil)

    // 3. EndpointSlice Informer
    c.endpoints = newEndpointSliceController(c)

    // 4. 노드 Informer (불필요 필드 제거)
    c.nodes = kclient.NewFiltered[*v1.Node](kubeClient,
        kclient.Filter{ObjectTransform: kubelib.StripNodeUnusedFields})
    registerHandlers[*v1.Node](c, c.nodes, "Nodes", c.onNodeEvent, nil)

    // 5. Pod Informer (종료된 Pod 제외)
    c.podsClient = kclient.NewFiltered[*v1.Pod](kubeClient, kclient.Filter{
        ObjectFilter:    kubeClient.ObjectFilter(),
        ObjectTransform: kubelib.StripPodUnusedFields,
        FieldSelector:   "status.phase!=Failed",
    })
    c.pods = newPodCache(c, c.podsClient, func(key types.NamespacedName) {
        c.queue.Push(func() error {
            return c.endpoints.podArrived(key.Name, key.Namespace)
        })
    })
    registerHandlers[*v1.Pod](c, c.podsClient, "Pods", c.pods.onEvent, nil)

    return c
}
```

Informer 필터링 최적화:
- `ObjectFilter`: 특정 네임스페이스만 감시 (디스커버리 네임스페이스 필터)
- `ObjectTransform`: 불필요한 필드 제거로 메모리 절약 (`StripNodeUnusedFields`, `StripPodUnusedFields`)
- `FieldSelector`: 서버 측에서 실패한 Pod 필터링 (`status.phase!=Failed`)

---

## 5. 이벤트 흐름: Informer에서 xDS Push까지

### 5.1 registerHandlers: 이벤트 등록 패턴

모든 K8s 리소스의 이벤트 처리는 제네릭 함수 `registerHandlers`를 통해 등록된다. 이 함수는 Informer의 `AddEventHandler`를 래핑하여 메트릭 기록과 큐잉을 자동으로 처리한다.

```go
// pilot/pkg/serviceregistry/kube/controller/controller.go (629행)
func registerHandlers[T controllers.ComparableObject](
    c *Controller,
    informer kclient.Informer[T],
    otype string,
    handler func(T, T, model.Event) error,
    filter FilterOutFunc[T],
) {
    wrappedHandler := func(prev, curr T, event model.Event) error {
        curr = informer.Get(curr.GetName(), curr.GetNamespace())
        if controllers.IsNil(curr) {
            return nil  // 즉시 삭제된 경우 무시
        }
        return handler(prev, curr, event)
    }

    // 메트릭 사전 생성 (이벤트마다 재계산 방지)
    adds := k8sEvents.With(typeTag.Value(otype), eventTag.Value("add"))
    updates := k8sEvents.With(typeTag.Value(otype), eventTag.Value("update"))
    deletes := k8sEvents.With(typeTag.Value(otype), eventTag.Value("delete"))

    informer.AddEventHandler(controllers.EventHandler[T]{
        AddFunc: func(obj T) {
            adds.Increment()
            c.queue.Push(func() error {
                return wrappedHandler(ptr.Empty[T](), obj, model.EventAdd)
            })
        },
        UpdateFunc: func(old, cur T) {
            if filter != nil && filter(old, cur) {
                return  // 의미 없는 업데이트 필터링
            }
            updates.Increment()
            c.queue.Push(func() error {
                return wrappedHandler(old, cur, model.EventUpdate)
            })
        },
        DeleteFunc: func(obj T) {
            deletes.Increment()
            c.queue.Push(func() error {
                return handler(ptr.Empty[T](), obj, model.EventDelete)
            })
        },
    })
}
```

### 5.2 이벤트 처리 시퀀스

```
K8s API Server
     |
     v (Watch Stream)
+----+----------+
| Informer      |  SharedIndexInformer가 리소스 캐시 갱신
| (kclient)     |
+----+----------+
     |
     v (EventHandler 콜백)
+----+----------+
| registerHdlrs |  메트릭 기록 + filter 확인
| (래퍼 함수)    |
+----+----------+
     |
     v (c.queue.Push)
+----+----------+
| queue.Instance|  1초 재시도 간격의 FIFO 큐
| (직렬화 큐)   |  이벤트 순서 보장
+----+----------+
     |
     v (큐 소비)
+----+----------+
| handler 함수  |  onServiceEvent / onEvent / pods.onEvent
| (비즈니스 로직)|
+----+----------+
     |
     v (캐시 갱신 + 푸시 트리거)
+----+-----------+-----+
| servicesMap    | EDS  |
| endpointCache  | 캐시 |
+----+-----------+-----+
     |
     v
+----+----------+
| XDSUpdater    |  EDSUpdate / ConfigUpdate / SvcUpdate / ProxyUpdate
| (xDS 서버)    |
+----+----------+
     |
     v
+----+----------+
| Envoy Proxy   |  xDS push (CDS/EDS/LDS/RDS)
+---------------+
```

### 5.3 큐 기반 직렬화의 이유

이벤트를 큐에 넣어 직렬화하는 이유:

1. **순서 보장**: K8s Informer는 여러 리소스의 이벤트를 동시에 발생시킬 수 있다. 큐는 이벤트를 FIFO 순서로 처리하여 일관성을 보장한다.
2. **재시도**: 핸들러가 에러를 반환하면 1초 후 자동 재시도한다.
3. **동시성 제어**: 모든 핸들러가 단일 고루틴에서 실행되므로 `servicesMap` 등의 캐시를 안전하게 업데이트할 수 있다.
4. **HasSynced 추적**: 큐가 비어 있으면 모든 초기 이벤트가 처리된 것으로 판단할 수 있다.

### 5.4 Run: 컨트롤러 시작

```go
// pilot/pkg/serviceregistry/kube/controller/controller.go (718행)
func (c *Controller) Run(stop <-chan struct{}) {
    // 동기화 타임아웃 설정
    if c.opts.SyncTimeout != 0 {
        time.AfterFunc(c.opts.SyncTimeout, func() {
            if !c.queue.HasSynced() {
                log.Warnf("kube controller for %s initial sync timed out", c.opts.ClusterID)
                c.initialSyncTimedout.Store(true)
            }
        })
    }

    // MCS 컨트롤러 시작
    go c.imports.Run(stop)
    go c.exports.Run(stop)
    if c.ambientIndex != nil {
        go c.ambientIndex.Run(stop)
    }

    // Informer 캐시 동기화 대기
    kubelib.WaitForCacheSync("kube controller", stop, c.informersSynced)

    // 큐 처리 시작 (이벤트 소비)
    c.queue.Run(stop)
}
```

`informersSynced()`는 모든 Informer의 초기 리스트 응답이 도착했는지 확인한다:

```go
func (c *Controller) informersSynced() bool {
    return c.namespaces.HasSynced() &&
        c.services.HasSynced() &&
        c.endpoints.slices.HasSynced() &&
        c.pods.pods.HasSynced() &&
        c.nodes.HasSynced() &&
        c.imports.HasSynced() &&
        c.exports.HasSynced() &&
        c.networkManager.HasSynced()
}
```

---

## 6. ConvertService(): K8s에서 Istio 모델로

### 6.1 변환 개요

`ConvertService()`는 Kubernetes의 `v1.Service` 객체를 Istio 내부 모델인 `model.Service`로 변환하는 핵심 함수다. 이 변환은 K8s Controller가 서비스 이벤트를 처리할 때 호출된다.

```
v1.Service (Kubernetes)              model.Service (Istio)
+---------------------------+        +---------------------------+
| Name: "my-svc"            |        | Hostname: "my-svc.ns.     |
| Namespace: "ns"           |  --->  |   svc.cluster.local"      |
| Spec.ClusterIP: "10.0.1.1"|        | DefaultAddress: "10.0.1.1"|
| Spec.Ports: [80, 443]     |        | Ports: [80/HTTP, 443/HTTPS]|
| Spec.Type: ClusterIP      |        | Resolution: ClientSideLB  |
| Labels: {app: web}        |        | ClusterVIPs: {clsA: [..]} |
| Annotations: {exportTo..} |        | Attributes: {ExportTo..}  |
+---------------------------+        +---------------------------+
```

### 6.2 변환 규칙 상세

```go
// pilot/pkg/serviceregistry/kube/conversion.go (46행)
func ConvertService(svc corev1.Service, nsAnnotations map[string]string,
    domainSuffix string, clusterID cluster.ID, trustDomain string) *model.Service {

    addrs := []string{constants.UnspecifiedIP}  // 기본값: "0.0.0.0"
    resolution := model.ClientSideLB             // 기본 해석 방식
    externalName := ""

    // 1. ExternalName 타입 처리
    if svc.Spec.Type == corev1.ServiceTypeExternalName && svc.Spec.ExternalName != "" {
        externalName = svc.Spec.ExternalName
        resolution = model.Alias  // DNS 별칭으로 해석
    }

    // 2. Headless 서비스 처리 (ClusterIP=None)
    if svc.Spec.ClusterIP == corev1.ClusterIPNone {
        resolution = model.Passthrough  // 직접 통과
    } else if svc.Spec.ClusterIP != "" {
        addrs[0] = svc.Spec.ClusterIP
        // Dual-Stack 지원
        if len(svc.Spec.ClusterIPs) > 1 {
            addrs = svc.Spec.ClusterIPs
        }
    }

    // 3. 포트 변환 (프로토콜 자동 감지)
    ports := make([]*model.Port, 0, len(svc.Spec.Ports))
    for _, port := range svc.Spec.Ports {
        ports = append(ports, convertPort(port))
    }

    // 4. ExportTo 어노테이션 처리
    if svc.Annotations[annotation.NetworkingExportTo.Name] != "" {
        // "." -> 같은 네임스페이스, "*" -> 모든 네임스페이스, "~" -> 내보내지 않음
    }

    // 5. FQDN 호스트명 생성
    //    형식: <name>.<namespace>.svc.<domainSuffix>
    hostname := ServiceHostname(svc.Name, svc.Namespace, domainSuffix)

    // 6. model.Service 생성
    istioService := &model.Service{
        Hostname:        hostname,
        ClusterVIPs:     model.AddressMap{Addresses: map[cluster.ID][]string{clusterID: addrs}},
        Ports:           ports,
        DefaultAddress:  addrs[0],
        ServiceAccounts: serviceaccounts,
        MeshExternal:    len(externalName) > 0,
        Resolution:      resolution,
    }

    return istioService
}
```

### 6.3 Resolution 유형과 결정 로직

| K8s Service 타입 | ClusterIP | Resolution | 의미 |
|------------------|-----------|------------|------|
| ClusterIP | 있음 | `ClientSideLB` | Envoy가 EDS 기반 로드밸런싱 |
| ClusterIP | None (Headless) | `Passthrough` | 엔드포인트 IP로 직접 전달 |
| ExternalName | N/A | `Alias` | DNS CNAME 별칭 |
| NodePort | 있음 | `ClientSideLB` | + NodePort 주소 매핑 |
| LoadBalancer | 있음 | `ClientSideLB` | + LB Ingress IP/호스트명 |

### 6.4 포트 프로토콜 감지

```go
func convertPort(port corev1.ServicePort) *model.Port {
    return &model.Port{
        Name:     port.Name,
        Port:     int(port.Port),
        Protocol: kube.ConvertProtocol(port.Port, port.Name, port.Protocol, port.AppProtocol),
    }
}
```

Istio는 포트 이름 규칙으로 프로토콜을 감지한다:
- `http-*`, `http2-*` -> HTTP
- `grpc-*` -> gRPC (HTTP/2)
- `tcp-*` -> TCP
- `https-*` -> HTTPS
- `appProtocol` 필드가 있으면 우선 사용

### 6.5 LoadBalancer와 NodePort 특수 처리

LoadBalancer 서비스의 경우, 외부 주소(Ingress IP 또는 호스트명)를 `ClusterExternalAddresses`에 저장한다:

```go
case corev1.ServiceTypeLoadBalancer:
    if len(svc.Status.LoadBalancer.Ingress) > 0 {
        var lbAddrs []string
        for _, ingress := range svc.Status.LoadBalancer.Ingress {
            if len(ingress.IP) > 0 {
                lbAddrs = append(lbAddrs, ingress.IP)
            } else if len(ingress.Hostname) > 0 {
                // DNS 해석하지 않음 - AWS ELB는 IP가 변경될 수 있으므로
                // EDS 대신 strict_dns로 전환해야 함
                lbAddrs = append(lbAddrs, ingress.Hostname)
            }
        }
        istioService.Attributes.ClusterExternalAddresses.SetAddressesFor(clusterID, lbAddrs)
    }
```

---

## 7. EndpointSlice 처리

### 7.1 endpointSliceController 구조

Kubernetes 1.21+에서는 Endpoints 대신 EndpointSlice를 사용한다. Istio의 `endpointSliceController`는 EndpointSlice 이벤트를 처리하여 EDS(Endpoint Discovery Service) 캐시를 업데이트한다.

```go
// pilot/pkg/serviceregistry/kube/controller/endpointslice.go (42행)
type endpointSliceController struct {
    endpointCache *endpointSliceCache                // 호스트명별 엔드포인트 캐시
    slices        kclient.Client[*v1.EndpointSlice]  // EndpointSlice Informer
    c             *Controller                         // 부모 Controller 참조
}
```

### 7.2 endpointSliceCache: 2레벨 맵 구조

EndpointSlice 캐시는 hostname -> sliceName -> endpoints 의 2레벨 맵으로 구성된다. 이 구조는 하나의 서비스에 여러 EndpointSlice가 존재할 수 있기 때문이다(K8s는 슬라이스당 최대 100개의 엔드포인트를 유지).

```
endpointSliceCache
+----------------------------------------------------------------+
| endpointsByServiceAndSlice:                                     |
|   map[host.Name]map[string][]*model.IstioEndpoint               |
|                                                                 |
|   "my-svc.ns.svc.cluster.local":                               |
|     "my-svc-abc12": [ep1, ep2, ep3, ...]  (SliceA)             |
|     "my-svc-def34": [ep4, ep5, ep6, ...]  (SliceB)             |
|                                                                 |
|   "other-svc.ns.svc.cluster.local":                            |
|     "other-svc-xyz": [ep7, ep8, ...]      (SliceC)             |
+----------------------------------------------------------------+
```

```go
// pilot/pkg/serviceregistry/kube/controller/endpointslice.go (358행)
type endpointSliceCache struct {
    mu                         sync.RWMutex
    endpointsByServiceAndSlice map[host.Name]map[string][]*model.IstioEndpoint
}
```

### 7.3 이벤트 처리 흐름

```
EndpointSlice Event
       |
       v
  onEventInternal()
       |
       +-- MCS 라벨 확인 (mcs ServiceName 라벨이 있으면 무시)
       |
       +-- event == Delete ?
       |     |
       |     +-- Yes: deleteEndpointSlice()
       |     |        - endpointCache에서 해당 슬라이스 제거
       |     |        - PodCache의 needResync 정리
       |     |
       |     +-- No:  updateEndpointSlice()
       |              - updateEndpointCacheForSlice() 호출
       |              - Pod 조회하여 메타데이터 추출
       |              - IstioEndpoint 생성
       |              - endpointCache.Update() 호출
       |
       +-- 서비스가 export-to=none이면 종료
       |
       +-- pushEDS(): hostnames에 대해 EDS 업데이트 트리거
       |
       +-- Headless 서비스인 경우:
             - TCP 포트가 있으면 Full Push (리스너 업데이트 필요)
             - HTTP만이면 NDS(Name Discovery Service) Push만
```

### 7.4 updateEndpointCacheForSlice: 엔드포인트 변환

```go
// pilot/pkg/serviceregistry/kube/controller/endpointslice.go (263행)
func (esc *endpointSliceController) updateEndpointCacheForSlice(
    hostName host.Name, epSlice *v1.EndpointSlice) {

    var endpoints []*model.IstioEndpoint
    // FQDN 타입 EndpointSlice는 현재 미지원
    if epSlice.AddressType == v1.AddressTypeFQDN {
        return
    }

    svc := esc.c.GetService(hostName)
    for _, e := range epSlice.Endpoints {
        healthStatus := endpointHealthStatus(svc, e)
        for _, a := range e.Addresses {
            // Pod 조회 (PodCache 사용)
            pod, expectedPod := getPod(esc.c, a, ..., e.TargetRef, hostName)
            if pod == nil && expectedPod {
                continue  // Pod가 아직 도착하지 않음 -> needResync에 등록
            }

            // Dual-Stack 처리
            // IPv6 EndpointSlice는 중복 방지를 위해 건너뜀
            if features.EnableDualStack && expectedPod && len(pod.Status.PodIPs) > 1 {
                if epSlice.AddressType == v1.AddressTypeIPv6 {
                    continue
                }
                overrideAddresses = slices.Map(pod.Status.PodIPs, ...)
            }

            // IstioEndpoint 빌드
            builder := esc.c.NewEndpointBuilder(pod)
            for _, port := range epSlice.Ports {
                istioEndpoint := builder.buildIstioEndpoint(
                    a, portNum, portName,
                    discoverabilityPolicy, healthStatus,
                    svc.SupportsUnhealthyEndpoints(),
                )
                endpoints = append(endpoints, istioEndpoint)
            }
        }
    }
    // 캐시 업데이트 (hostname + sliceName 기반)
    esc.endpointCache.Update(hostName, epSlice.Name, endpoints)
}
```

### 7.5 중복 엔드포인트 처리

EndpointSlice는 슬라이스 간 엔드포인트 중복을 허용한다(전환 과정에서 일시적으로 발생). 캐시의 `get()` 메서드는 `endpointKey`(IP + 포트명)로 중복을 제거한다:

```go
// pilot/pkg/serviceregistry/kube/controller/endpointslice.go (411행)
func (e *endpointSliceCache) get(hostname host.Name) []*model.IstioEndpoint {
    var endpoints []*model.IstioEndpoint
    found := sets.New[endpointKey]()
    for _, eps := range e.endpointsByServiceAndSlice[hostname] {
        for _, ep := range eps {
            key := endpointKey{ep.FirstAddressOrNil(), ep.ServicePortName}
            if found.InsertContains(key) {
                continue  // 이미 추가된 엔드포인트 -> 건너뜀
            }
            endpoints = append(endpoints, ep)
        }
    }
    return endpoints
}
```

### 7.6 EDS Push 트리거

```go
// pilot/pkg/serviceregistry/kube/controller/endpointslice.go (445행)
func (esc *endpointSliceController) pushEDS(hostnames []host.Name, namespace string) {
    shard := model.ShardKeyFromRegistry(esc.c)
    esc.endpointCache.mu.Lock()
    defer esc.endpointCache.mu.Unlock()

    for _, hostname := range hostnames {
        endpoints := esc.endpointCache.get(hostname)
        // WorkloadEntry에서 선택된 엔드포인트도 포함
        if features.EnableK8SServiceSelectWorkloadEntries {
            svc := esc.c.GetService(hostname)
            if svc != nil {
                fep := esc.c.collectWorkloadInstanceEndpoints(svc)
                endpoints = append(endpoints, fep...)
            }
        }
        // xDS 서버에 EDS 업데이트 전달
        esc.c.opts.XDSUpdater.EDSUpdate(shard, string(hostname), namespace, endpoints)
    }
}
```

---

## 8. PodCache: IP-Pod 매핑과 최종 일관성

### 8.1 PodCache 설계 철학

PodCache는 **최종 일관성(Eventual Consistency)** 모델로 설계된 IP-Pod 매핑 캐시다. Kubernetes에서 Pod 이벤트와 EndpointSlice 이벤트는 독립적으로 전달되므로, EndpointSlice에 나타난 Pod IP에 대응하는 Pod 객체가 아직 도착하지 않을 수 있다. PodCache는 이 시간차를 `needResync` 메커니즘으로 처리한다.

```go
// pilot/pkg/serviceregistry/kube/controller/pod.go (34행)
type PodCache struct {
    pods kclient.Client[*v1.Pod]

    sync.RWMutex
    // IP -> Pod 이름 매핑 (실행 중인 Pod만)
    podsByIP map[string]sets.Set[types.NamespacedName]
    // Pod 이름 -> IP 역방향 매핑 (IP 변경 감지용)
    ipByPods map[types.NamespacedName]string

    // IP -> EndpointSlice 이름 매핑
    // Pod가 도착하지 않은 엔드포인트를 추적
    needResync         map[string]sets.Set[types.NamespacedName]
    queueEndpointEvent func(types.NamespacedName)  // Pod 도착 시 호출

    c *Controller
}
```

### 8.2 데이터 구조 관계

```
podsByIP (정방향)                   ipByPods (역방향)
+-------------------------+        +----------------------------+
| "10.0.1.5" -> {         |        | ns/pod-a -> "10.0.1.5"    |
|   ns/pod-a              |        | ns/pod-b -> "10.0.1.6"    |
| }                       |        +----------------------------+
| "10.0.1.6" -> {         |
|   ns/pod-b              |        needResync (재동기화 대기)
| }                       |        +----------------------------+
| "10.0.2.1" -> {         |        | "10.0.3.1" -> {           |
|   ns/pod-c,             |        |   ns/ep-slice-abc         |
|   ns/pod-d (hostNetwork)|        | }                         |
| }                       |        +----------------------------+
+-------------------------+        (Pod 미도착 IP -> ES 이름)
```

### 8.3 Pod 이벤트 처리

```go
// pilot/pkg/serviceregistry/kube/controller/pod.go (147행)
func (pc *PodCache) onEvent(old, pod *v1.Pod, ev model.Event) error {
    ip := pod.Status.PodIP
    if len(ip) == 0 {
        // IP가 없는 경우: Eviction으로 IP가 제거되었을 수 있음
        ip = pc.getIPByPod(config.NamespacedName(pod))
        if len(ip) == 0 {
            return nil  // IP가 할당된 적 없음 -> 무시
        }
    }

    key := config.NamespacedName(pod)
    switch ev {
    case model.EventAdd:
        if shouldPodBeInEndpoints(pod) && IsPodReady(pod) {
            pc.addPod(pod, ip, key, false)
        }
    case model.EventUpdate:
        if !shouldPodBeInEndpoints(pod) || !IsPodReady(pod) {
            if !pc.deleteIP(ip, key) {
                return nil
            }
            ev = model.EventDelete  // Ready가 아니면 삭제로 변환
        } else {
            labelUpdated := pc.labelFilter(old, pod)
            pc.addPod(pod, ip, key, labelUpdated)
        }
    case model.EventDelete:
        if !pc.deleteIP(ip, key) {
            return nil  // 이미 삭제됨 (Update에서 처리됨)
        }
    }
    pc.notifyWorkloadHandlers(pod, ev, ip)
    return nil
}
```

Pod가 엔드포인트에 포함될 조건:

```go
func shouldPodBeInEndpoints(pod *v1.Pod) bool {
    // 1. 터미널 상태(Succeeded/Failed)가 아니어야 함
    if isPodPhaseTerminal(pod.Status.Phase) { return false }
    // 2. IP가 할당되어 있어야 함
    if len(pod.Status.PodIP) == 0 && len(pod.Status.PodIPs) == 0 { return false }
    // 3. 삭제 중이 아니어야 함
    if pod.DeletionTimestamp != nil { return false }
    return true
}
```

### 8.4 needResync 메커니즘: 최종 일관성 보장

EndpointSlice 이벤트가 Pod 이벤트보다 먼저 도착하면, Pod 정보(라벨, 서비스계정 등)를 알 수 없다. 이 경우 `needResync`에 등록하고, 나중에 Pod가 도착하면 엔드포인트 이벤트를 재처리한다.

```
시간축 -->

T1: EndpointSlice 도착 (Pod IP: 10.0.3.1)
    -> getPod() 호출 -> Pod 없음
    -> needResync["10.0.3.1"] = {ns/ep-slice-abc}
    -> 해당 엔드포인트는 건너뜀 (부정확한 정보 방지)

T2: Pod 도착 (IP: 10.0.3.1)
    -> addPod() 호출
    -> needResync["10.0.3.1"] 발견
    -> queueEndpointEvent(ns/ep-slice-abc) 호출
    -> EndpointSlice 재처리 -> 이제 Pod 정보 포함하여 정상 처리
```

```go
// pilot/pkg/serviceregistry/kube/controller/pod.go (267행)
func (pc *PodCache) addPod(pod *v1.Pod, ip string, key types.NamespacedName, labelUpdated bool) {
    pc.Lock()
    // 이미 캐시에 있으면 건너뜀
    if pc.podsByIP[ip].Contains(key) {
        pc.Unlock()
        if labelUpdated { pc.proxyUpdates(pod, true) }
        return
    }
    // IP 변경 감지: 기존 IP 정리
    if current, f := pc.ipByPods[key]; f {
        sets.DeleteCleanupLast(pc.podsByIP, current, key)
    }
    sets.InsertOrNew(pc.podsByIP, ip, key)
    pc.ipByPods[key] = ip

    // needResync에 대기 중인 엔드포인트가 있으면 재처리
    if endpointsToUpdate, f := pc.needResync[ip]; f {
        delete(pc.needResync, ip)
        for epKey := range endpointsToUpdate {
            pc.queueEndpointEvent(epKey)  // 엔드포인트 이벤트 재큐잉
        }
        endpointsPendingPodUpdate.Record(float64(len(pc.needResync)))
    }
    pc.Unlock()
    pc.proxyUpdates(pod, false)
}
```

### 8.5 hostNetwork Pod 처리

`hostNetwork: true`인 Pod는 노드의 IP를 공유하므로 하나의 IP에 여러 Pod가 매핑될 수 있다. `podsByIP`가 `sets.Set[types.NamespacedName]`인 이유가 이것이다:

```go
func (pc *PodCache) getPodsByIP(addr string) []*v1.Pod {
    keys := pc.getPodKeys(addr)
    res := make([]*v1.Pod, 0, len(keys))
    for _, key := range keys {
        p := pc.getPodByKey(key)
        if p != nil {
            res = append(res, p)
        }
    }
    return res
}
```

---

## 9. ServiceEntry Controller: 외부 서비스 등록

### 9.1 설계 목적

ServiceEntry Controller는 Istio의 `ServiceEntry` CRD와 `WorkloadEntry` CRD를 처리하여 메시 외부의 서비스를 서비스 레지스트리에 등록한다. 이를 통해 외부 API, 레거시 VM, 데이터베이스 등을 메시의 일부로 관리할 수 있다.

```
ServiceEntry CRD                    WorkloadEntry CRD
+----------------------------+       +---------------------------+
| apiVersion: networking/v1  |       | apiVersion: networking/v1 |
| kind: ServiceEntry         |       | kind: WorkloadEntry       |
| spec:                      |       | spec:                     |
|   hosts: ["api.ext.com"]   |       |   address: 10.10.1.1      |
|   ports:                   |       |   labels:                 |
|   - number: 443            |       |     app: legacy-vm        |
|     protocol: HTTPS        |       |   serviceAccount: vm-sa   |
|   resolution: DNS          |       +---------------------------+
|   location: MESH_EXTERNAL  |
+----------------------------+
        |                                  |
        v                                  v
+-------+----------------------------------+--------+
|            ServiceEntry Controller                 |
|                                                    |
|  KRT Collections:                                  |
|  +----------------------------------------------+ |
|  | ServiceEntries -> Services + Instances        | |
|  | WorkloadEntries -> Workloads                  | |
|  +----------------------------------------------+ |
+----------------------------------------------------+
```

### 9.2 KRT 기반 리액티브 아키텍처

ServiceEntry Controller는 K8s Controller와 달리 KRT(Kubernetes Resource Tracker) 프레임워크를 사용하여 리액티브(반응형) 파이프라인으로 구현되었다.

```go
// pilot/pkg/serviceregistry/serviceentry/controller.go (50행)
type Controller struct {
    XdsUpdater model.XDSUpdater

    store     model.ConfigStore    // CRD 저장소
    clusterID cluster.ID

    inputs  Inputs   // 입력 KRT 컬렉션
    outputs Outputs  // 출력 KRT 컬렉션
    handlers []krt.HandlerRegistration

    networkIDCallback networkIDCallback  // 네트워크 ID 콜백
    workloadEntryController bool         // WLE 전용 컨트롤러 여부
}
```

입력/출력 컬렉션:

```go
type Inputs struct {
    MeshConfig      krt.Collection[meshwatcher.MeshConfigResource]
    Namespaces      krt.Collection[*v1.Namespace]
    WorkloadEntries krt.Collection[config.Config]
    ServiceEntries  krt.Collection[config.Config]
    ExternalWorkloads krt.StaticCollection[*model.WorkloadInstance]
}

type Outputs struct {
    Services       krt.Collection[ServiceWithInstances]       // 서비스 + 인스턴스
    ServicesByHost krt.Index[string, ServiceWithInstances]    // 호스트명 인덱스
    ServiceInstancesByNamespaceHost krt.Collection[...]       // EDS 업데이트용
    ServiceInstances     krt.Collection[*model.ServiceInstance]
    ServiceInstancesByIP krt.Index[string, *model.ServiceInstance] // IP 인덱스
    Workloads krt.Collection[*model.WorkloadInstance]         // 워크로드 인스턴스
}
```

### 9.3 데이터 흐름 파이프라인

```
                 KRT 파이프라인
+----------------------------------------------------------+
|                                                           |
|  ServiceEntry CRDs ----+                                  |
|                         |                                  |
|  WorkloadEntry CRDs ---+---> services() ---> Services     |
|                         |         |                        |
|  MeshConfig -----------+         |                        |
|                         |         v                        |
|  Namespaces -----------+   ServiceInstances               |
|                                  |                        |
|  ExternalWorkloads ----+         v                        |
|       (Pods에서 온      |   ServiceInstancesByIP           |
|        워크로드 인스턴스) |   ServiceInstancesByNsHost       |
|                                                           |
+----------------------------------------------------------+
                    |
                    v
          +--------+--------+
          | EDS/XDS Push     |
          | Handlers         |
          +-----------------+
```

### 9.4 서비스 이벤트와 EDS 업데이트

ServiceEntry Controller는 KRT의 `RegisterBatch`를 사용하여 변경 이벤트를 배치로 처리한다:

```go
// pilot/pkg/serviceregistry/serviceentry/controller.go (293행)
func (s *Controller) pushServiceEndpointUpdates(events []krt.Event[InstancesByNamespaceHost]) {
    shard := model.ShardKeyFromRegistry(s)
    for _, e := range events {
        obj := e.Latest()
        if e.Event == controllers.EventDelete {
            s.XdsUpdater.SvcUpdate(shard, obj.Hostname, obj.Namespace, model.EventDelete)
            s.XdsUpdater.EDSUpdate(shard, obj.Hostname, obj.Namespace, nil)
        } else {
            instances := slices.Map(obj.Instances, func(i *model.ServiceInstance) *model.IstioEndpoint {
                return i.Endpoint
            })
            s.XdsUpdater.EDSUpdate(shard, obj.Hostname, obj.Namespace, instances)
            // DNS 기반 서비스의 엔드포인트가 변경되면 Full Push 필요
            if obj.HasDNSServiceEndpoint && e.Event == controllers.EventUpdate {
                s.XdsUpdater.ConfigUpdate(&model.PushRequest{
                    Full:           true,
                    ConfigsUpdated: sets.New(model.ConfigKey{
                        Kind: kind.ServiceEntry, Name: obj.Hostname, Namespace: obj.Namespace,
                    }),
                    Reason: model.NewReasonStats(model.EndpointUpdate),
                })
            }
        }
    }
}
```

### 9.5 IP 자동 할당

ServiceEntry로 등록된 서비스에는 자동으로 가상 IP가 할당된다. 이 IP는 Class E 서브넷(240.240.0.0/16)에서 할당되며, DNS 해석에 사용된다:

```go
// Services() 메서드에서 autoAllocateIPs 호출
func (s *Controller) Services() []*model.Service {
    allServices := s.outputs.Services.List()
    copySvcs := make([]*model.Service, len(allServices))
    for i, svc := range allServices {
        copySvcs[i] = svc.Service.ShallowCopy()
    }
    return autoAllocateIPs(copySvcs)  // 자동 IP 할당
}
```

### 9.6 WorkloadInstance 핸들러 연동

K8s Controller의 PodCache와 ServiceEntry Controller는 `WorkloadInstanceHandler`를 통해 연동된다. Pod가 추가/삭제되면 해당 워크로드 인스턴스가 ServiceEntry의 외부 워크로드로 전파된다:

```go
func (s *Controller) WorkloadInstanceHandler(wi *model.WorkloadInstance, event model.Event) {
    switch event {
    case model.EventDelete:
        s.inputs.ExternalWorkloads.DeleteObject(wi.ResourceName())
    default:
        s.inputs.ExternalWorkloads.ConditionalUpdateObject(wi)
    }
}
```

---

## 10. 멀티클러스터 서비스 디스커버리

### 10.1 멀티클러스터 아키텍처

Istio의 멀티클러스터는 "Primary-Remote" 또는 "Multi-Primary" 토폴로지를 지원한다. 각 원격 클러스터에 대해 별도의 K8s Controller가 생성되어 Aggregate Controller에 등록된다.

```
+-------------------------------------------------------------------+
|                      Primary Cluster                               |
|                                                                    |
|  istiod                                                            |
|  +--------------------------------------------------------------+ |
|  |  Aggregate Controller                                         | |
|  |                                                                | |
|  |  +------------------+  +------------------+  +-------------+  | |
|  |  | K8s Controller   |  | K8s Controller   |  | ServiceEntry|  | |
|  |  | (Primary, cls-a) |  | (Remote, cls-b)  |  | Controller  |  | |
|  |  |                  |  |                  |  |             |  | |
|  |  | kubeClient:      |  | kubeClient:      |  | configCtrl: |  | |
|  |  |  in-cluster      |  |  remote secret   |  |  CRD watch  |  | |
|  |  +------------------+  +------------------+  +-------------+  | |
|  +--------------------------------------------------------------+ |
|                                                                    |
|  Remote Secret ---------> kubeconfig for cls-b                     |
|  (istio-system/          Secret에 원격 클러스터 접속 정보 포함       |
|   istio-remote-secret-b)                                           |
+-------------------------------------------------------------------+
```

### 10.2 원격 클러스터 등록

원격 클러스터의 Secret이 생성/변경되면 새로운 K8s Controller가 생성되어 `AddRegistryAndRun()`으로 등록된다:

```go
// Aggregate Controller의 AddRegistryAndRun()
func (c *Controller) AddRegistryAndRun(registry serviceregistry.Instance, stop <-chan struct{}) {
    c.storeLock.Lock()
    defer c.storeLock.Unlock()
    c.addRegistry(registry, stop)
    if c.running {
        go registry.Run(stop)  // 이미 실행 중이면 즉시 시작
    }
}
```

### 10.3 서비스 병합 규칙

동일한 호스트명의 서비스가 여러 클러스터에 존재할 때:

```
Cluster A:                          Cluster B:
  my-svc.ns.svc.cluster.local        my-svc.ns.svc.cluster.local
  ClusterIP: 10.96.1.100             ClusterIP: 10.100.1.200
  ServiceAccounts: [sa-a]             ServiceAccounts: [sa-b]

                    |
                    v (mergeService)

병합된 서비스:
  my-svc.ns.svc.cluster.local
  ClusterVIPs:
    cluster-a: [10.96.1.100]
    cluster-b: [10.100.1.200]
  ServiceAccounts: [sa-a, sa-b]  (통합)
```

### 10.4 교차 클러스터 엔드포인트 필터링

프록시가 서비스 타겟을 요청할 때, Aggregate Controller는 프록시의 클러스터 ID를 기반으로 필터링한다:

```go
func (c *Controller) GetProxyServiceTargets(node *model.Proxy) []ServiceTarget {
    out := make([]model.ServiceTarget, 0)
    nodeClusterID := nodeClusterID(node)
    for _, r := range c.GetRegistries() {
        if skipSearchingRegistryForProxy(nodeClusterID, r) {
            continue  // 다른 클러스터의 K8s 레지스트리는 건너뜀
        }
        instances := r.GetProxyServiceTargets(node)
        out = append(out, instances...)
    }
    return out
}
```

### 10.5 네트워크 인식

K8s Controller는 엔드포인트의 네트워크 ID를 3단계 폴백으로 결정한다:

```go
// pilot/pkg/serviceregistry/kube/controller/controller.go (395행)
func (c *Controller) Network(endpointIP string, labels labels.Instance) network.ID {
    // 1단계: Pod/WorkloadEntry의 topology.istio.io/network 라벨
    if nw := labels[label.TopologyNetwork.Name]; nw != "" {
        return network.ID(nw)
    }
    // 2단계: 시스템 네임스페이스(istio-system)의 네트워크 라벨
    if nw := c.networkFromSystemNamespace(); nw != "" {
        return nw
    }
    // 3단계: MeshNetworks 설정에서 IP 범위 기반 매핑
    if nw := c.networkFromMeshNetworks(endpointIP); nw != "" {
        return nw
    }
    return ""
}
```

### 10.6 MCS (Multi-Cluster Services) 지원

K8s Controller는 `ServiceExport`와 `ServiceImport` CRD를 추적하여 MCS API를 지원한다:

```go
func (c *Controller) MCSServices() []model.MCSServiceInfo {
    outMap := make(map[types.NamespacedName]model.MCSServiceInfo)
    // ServiceExport 정보 추가
    for _, se := range c.exports.ExportedServices() {
        mcsService := outMap[se.namespacedName]
        mcsService.Exported = true
        mcsService.Discoverability = se.discoverability
        outMap[se.namespacedName] = mcsService
    }
    // ServiceImport 정보 추가
    for _, si := range c.imports.ImportedServices() {
        mcsService := outMap[si.namespacedName]
        mcsService.Imported = true
        mcsService.ClusterSetVIP = si.clusterSetVIP
        outMap[si.namespacedName] = mcsService
    }
    return maps.Values(outMap)
}
```

---

## 11. 왜 이렇게 설계했나

### 11.1 왜 인터페이스 기반 추상화인가?

Istio는 처음부터 다중 플랫폼(Kubernetes, Consul, Eureka 등)을 지원하도록 설계되었다. `ServiceDiscovery` 인터페이스는 플랫폼 독립적인 서비스 모델을 정의하고, 각 플랫폼 어댑터가 이를 구현한다. 현재는 Kubernetes와 ServiceEntry만 실제로 사용되지만, 이 구조 덕분에 새로운 서비스 레지스트리를 추가하는 것이 `serviceregistry.Instance` 인터페이스 구현만으로 가능하다.

### 11.2 왜 Aggregate Controller 패턴인가?

단일 레지스트리로는 다음 요구사항을 충족할 수 없다:

1. **멀티클러스터**: 각 클러스터마다 독립적인 K8s API 서버가 있으므로 별도의 컨트롤러가 필요
2. **외부 서비스**: ServiceEntry는 K8s API와 무관한 별도의 CRD이므로 별도의 컨트롤러가 필요
3. **동적 클러스터 추가/제거**: 런타임에 클러스터를 추가/삭제할 수 있어야 함

Aggregate Controller는 이 모든 것을 하나의 일관된 뷰로 통합한다. 특히 K8s 레지스트리를 비-K8s 레지스트리보다 앞에 배치하여 K8s 서비스가 동일 호스트명의 ServiceEntry보다 우선하도록 보장한다.

### 11.3 왜 큐 기반 이벤트 직렬화인가?

K8s Informer는 Watch 스트림에서 이벤트를 받아 콜백을 호출한다. 하지만 콜백 내에서 무거운 작업을 수행하면 Informer 파이프라인이 막혀 다른 이벤트 처리가 지연된다. 큐 기반 패턴의 장점:

- **빠른 콜백 반환**: Informer 콜백은 큐에 넣기만 하고 즉시 반환
- **이벤트 직렬화**: 단일 고루틴에서 순서대로 처리하므로 lock contention 감소
- **재시도**: 일시적 오류 시 자동 재시도 (1초 간격)
- **초기 동기화 추적**: 큐가 비면 초기 동기화 완료로 판단

### 11.4 왜 PodCache의 needResync 메커니즘인가?

Kubernetes에서 리소스 이벤트의 순서는 보장되지 않는다. EndpointSlice 이벤트가 Pod 이벤트보다 먼저 도착하면:

- Pod 없이 엔드포인트를 생성하면 라벨, 서비스계정, TLS 모드 등 중요한 메타데이터가 누락된다
- 이 메타데이터는 RBAC 정책 평가, mTLS 인증서 생성 등에 필수적이다
- 부정확한 메타데이터는 **보안 문제**를 야기할 수 있다

따라서 PodCache는 Pod가 도착하지 않은 엔드포인트를 **건너뛰고**, Pod가 도착하면 **재처리**한다. 이는 약간의 지연을 대가로 **정확성과 보안**을 보장한다.

### 11.5 왜 EndpointSlice 2레벨 캐시인가?

Kubernetes의 EndpointSlice는 서비스당 여러 개가 존재할 수 있다 (슬라이스당 최대 100개 엔드포인트). 2레벨 맵 구조(`hostname -> sliceName -> endpoints`)를 사용하는 이유:

1. **부분 업데이트**: 하나의 슬라이스만 변경되어도 해당 슬라이스의 엔드포인트만 교체하면 된다. 전체 엔드포인트 목록을 재구축할 필요 없다.
2. **삭제 효율성**: 슬라이스 삭제 시 해당 슬라이스의 엔드포인트만 제거하면 된다.
3. **중복 처리**: `get()` 메서드에서 `endpointKey` 기반 중복 제거를 수행하여, 슬라이스 전환 과정에서의 일시적 중복을 처리한다.

### 11.6 왜 ServiceEntry Controller는 KRT를 사용하는가?

K8s Controller는 전통적인 Informer + 큐 패턴을 사용하지만, ServiceEntry Controller는 KRT 프레임워크를 채택했다:

1. **선언적 파이프라인**: ServiceEntry와 WorkloadEntry의 조합은 복잡한 조인 로직이 필요하다. KRT는 이를 선언적으로 표현할 수 있다.
2. **자동 종속성 추적**: ServiceEntry의 workloadSelector로 WorkloadEntry를 선택할 때, KRT가 자동으로 종속성을 추적하여 재계산한다.
3. **배치 업데이트**: `RegisterBatch`로 여러 변경을 하나의 배치로 처리하여 불필요한 xDS push를 줄인다.
4. **인덱스 지원**: `ServiceInstancesByIP`, `ServicesByHost` 등 다양한 인덱스를 쉽게 구축할 수 있다.

### 11.7 왜 K8s 서비스가 ServiceEntry보다 우선하는가?

Aggregate Controller에서 K8s 레지스트리를 앞에 배치하는 이유:

1. **안전성**: K8s Service는 클러스터 관리자가 생성한 신뢰할 수 있는 소스이다. ServiceEntry는 어떤 사용자든 생성할 수 있으므로 K8s Service를 우선하는 것이 안전하다.
2. **예측 가능성**: 같은 호스트명이 K8s Service와 ServiceEntry 양쪽에 정의된 경우, K8s Service의 설정이 적용된다는 규칙이 명확하다.
3. **자동 통합**: `GetService()`에서 K8s 서비스를 먼저 찾으면 추가 조회 없이 반환할 수 있어 성능이 향상된다.

### 11.8 전체 설계 원칙 요약

| 설계 결정 | 이유 | 트레이드오프 |
|-----------|------|------------|
| 인터페이스 추상화 | 플랫폼 독립성, 확장성 | 간접 참조 오버헤드 |
| Aggregate 패턴 | 멀티소스 통합 | 병합 로직의 복잡성 |
| 큐 직렬화 | 일관성, 재시도 | 지연 증가 |
| needResync | 정확성, 보안 | 지연 증가 |
| 2레벨 캐시 | 부분 업데이트 효율 | 메모리 사용량 |
| KRT (ServiceEntry) | 선언적 파이프라인 | 학습 곡선 |
| K8s 우선 | 안전성, 예측성 | 유연성 제한 |

---

## 참고: 핵심 소스 파일 경로

| 파일 | 역할 |
|------|------|
| `pilot/pkg/model/service.go` | Service 구조체, ServiceDiscovery 인터페이스 |
| `pilot/pkg/model/controller.go` | Controller, AggregateController 인터페이스 |
| `pilot/pkg/serviceregistry/instance.go` | serviceregistry.Instance 통합 인터페이스 |
| `pilot/pkg/serviceregistry/provider/providers.go` | Provider ID 상수 (Kubernetes, External, Mock) |
| `pilot/pkg/serviceregistry/aggregate/controller.go` | Aggregate Controller 구현 |
| `pilot/pkg/serviceregistry/kube/controller/controller.go` | K8s Controller 구현 |
| `pilot/pkg/serviceregistry/kube/controller/endpointslice.go` | EndpointSlice 처리 |
| `pilot/pkg/serviceregistry/kube/controller/pod.go` | PodCache 구현 |
| `pilot/pkg/serviceregistry/kube/conversion.go` | ConvertService 변환 함수 |
| `pilot/pkg/serviceregistry/serviceentry/controller.go` | ServiceEntry Controller 구현 |
