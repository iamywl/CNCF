# 05. Istio 핵심 컴포넌트 동작 원리

## 목차

1. [DiscoveryServer - xDS 푸시 엔진](#1-discoveryserver---xds-푸시-엔진)
2. [ConfigGenerator - Envoy 설정 생성기](#2-configgenerator---envoy-설정-생성기)
3. [Aggregate Service Registry - 통합 서비스 레지스트리](#3-aggregate-service-registry---통합-서비스-레지스트리)
4. [Kubernetes Controller - K8s 리소스 감시기](#4-kubernetes-controller---k8s-리소스-감시기)
5. [Certificate Authority - 인증서 발급 기관](#5-certificate-authority---인증서-발급-기관)
6. [SDS Server - Secret Discovery Service](#6-sds-server---secret-discovery-service)

---

## 1. DiscoveryServer - xDS 푸시 엔진

> 소스 파일: `pilot/pkg/xds/discovery.go`, `pilot/pkg/xds/pushqueue.go`, `pilot/pkg/xds/ads.go`

DiscoveryServer는 Istio의 컨트롤 플레인(istiod)에서 가장 핵심적인 컴포넌트이다. Envoy 프록시가 gRPC 스트림으로 연결하면, 메시 설정 변경 사항을 감지하여 적절한 xDS 응답(CDS, LDS, RDS, EDS)을 생성하고 푸시한다. 이 전체 과정에서 디바운싱, 요청 병합, 동시성 제한이라는 세 가지 핵심 메커니즘이 작동한다.

### 1.1 핵심 구조체

```go
// pilot/pkg/xds/discovery.go:65
type DiscoveryServer struct {
    Env                 *model.Environment
    Generators          map[string]model.XdsResourceGenerator
    concurrentPushLimit chan struct{}           // 세마포어
    pushChannel         chan *model.PushRequest // 디바운스 버퍼
    pushQueue           *PushQueue             // 디바운스 후 실제 푸시 큐
    adsClients          map[string]*Connection  // 활성 gRPC 연결
    DebounceOptions     DebounceOptions
    Cache               model.XdsCache
    // ...
}
```

### 1.2 pushChannel과 디바운스 로직

설정 변경이 발생하면 `ConfigUpdate()` 메서드가 호출되어 `pushChannel`에 `PushRequest`를 전송한다.

```go
// pilot/pkg/xds/discovery.go:311
func (s *DiscoveryServer) ConfigUpdate(req *model.PushRequest) {
    inboundConfigUpdates.Increment()
    s.InboundUpdates.Inc()
    s.pushChannel <- req  // 버퍼 크기 10인 채널로 전송
}
```

`pushChannel`은 **버퍼 크기 10**으로 생성된다 (discovery.go:153).

```go
// pilot/pkg/xds/discovery.go:153
pushChannel: make(chan *model.PushRequest, 10),
```

이 채널을 소비하는 것은 `handleUpdates` 고루틴이며, 내부적으로 `debounce()` 함수를 호출한다.

```go
// pilot/pkg/xds/discovery.go:338
func (s *DiscoveryServer) handleUpdates(stopCh <-chan struct{}) {
    debounce(s.pushChannel, stopCh, s.DebounceOptions, s.Push, s.CommittedUpdates)
}
```

디바운스의 핵심 알고리즘은 다음과 같다 (discovery.go:343-425):

```
+------------------------------------------------------------------+
|  debounce() 루프                                                  |
|                                                                    |
|  1. pushChannel에서 이벤트 수신                                     |
|     - EDS 전용 && EDS 디바운스 비활성화 → 즉시 푸시                   |
|     - 그 외 → 디바운스 타이머 시작                                   |
|                                                                    |
|  2. 타이머 만료 시 pushWorker() 실행                                |
|     - eventDelay >= debounceMax (기본 10초) → 강제 푸시              |
|     - quietTime >= debounceAfter (기본 100ms) → 안정 상태, 푸시      |
|     - 둘 다 아님 → 타이머 재설정 (debounceAfter - quietTime)         |
|                                                                    |
|  3. 여러 이벤트가 들어오면 req.Merge(r)로 병합                       |
|     - ConfigsUpdated 합집합                                        |
|     - Full = 둘 중 하나라도 Full이면 Full                            |
|     - Reason 통계 합산                                              |
+------------------------------------------------------------------+
```

**DebounceOptions** 구조체 (discovery.go:46-62):

| 필드 | 기본값 | 설명 |
|------|--------|------|
| `DebounceAfter` | 100ms | 마지막 이벤트 후 최소 대기 시간 |
| `debounceMax` | 10s | 이벤트가 계속 들어와도 최대 대기 시간 |
| `enableEDSDebounce` | false | EDS 업데이트도 디바운스 할지 여부 |

디바운스에서 중요한 점은 `free` 플래그이다. 이전 푸시가 완료되지 않은 상태에서는 `free = false`이며, 타이머가 만료되더라도 즉시 푸시하지 않는다. 이전 푸시가 완료되어 `freeCh`에 신호가 오면 그때 `pushWorker()`를 다시 호출한다.

```
시간축 →

이벤트1  이벤트2  이벤트3       타이머만료     이전푸시완료
  |        |       |              |              |
  v        v       v              v              v
[수신]   [병합]   [병합]    [free=false→대기]  [pushWorker→실행]
  |                               |              |
  +---debounceAfter---+           |              |
                      |           |              |
               [타이머설정]        |         [실제 Push 시작]
                                  |
                           [이벤트 계속 병합]
```

### 1.3 PushQueue - 프록시별 푸시 큐

디바운스가 완료된 `PushRequest`는 `Push()` 메서드를 통해 `AdsPushAll()`이 호출되며, 모든 연결된 프록시에 대해 `PushQueue.Enqueue()`가 실행된다.

```go
// pilot/pkg/xds/pushqueue.go:23
type PushQueue struct {
    cond       *sync.Cond
    pending    map[*Connection]*model.PushRequest  // 대기 중인 요청
    queue      []*Connection                        // 순서 유지
    processing map[*Connection]*model.PushRequest  // 처리 중인 요청
    shuttingDown bool
}
```

PushQueue의 세 가지 핵심 연산:

**Enqueue** (pushqueue.go:51-74):
```
1. 이미 processing 중인 연결이면 → processing[con]에 병합 (나중에 재큐잉)
2. 이미 pending 중인 연결이면 → pending[con]에 병합
3. 새 연결이면 → pending에 추가, queue에 추가, cond.Signal()
```

이 설계의 핵심은 **요청 병합(request merging)**이다. 동일 프록시에 대해 여러 푸시 요청이 겹치면, 가장 최신 상태만 반영된 하나의 요청으로 병합된다.

**Dequeue** (pushqueue.go:77-104):
```
1. queue가 비어있으면 cond.Wait()로 대기
2. queue[0]에서 연결을 꺼냄
3. pending에서 해당 요청을 삭제
4. processing[con] = nil로 표시 (처리 중 상태)
```

**MarkDone** (pushqueue.go:106-119):
```
1. processing[con]에서 꺼냄
2. 값이 nil이 아니면 (Enqueue 중 새 요청이 들어온 경우)
   → pending에 다시 추가, queue에 재삽입, cond.Signal()
```

이 패턴은 처리 중에 새 업데이트가 도착해도 놓치지 않도록 보장한다.

### 1.4 concurrentPushLimit 세마포어

실제 푸시는 `sendPushes` 고루틴에서 수행되며, `concurrentPushLimit` 채널을 세마포어로 사용하여 동시 푸시 수를 제한한다.

```go
// pilot/pkg/xds/discovery.go:149
concurrentPushLimit: make(chan struct{}, features.PushThrottle),
```

`PILOT_PUSH_THROTTLE` 환경변수로 제어된다 (`pilot/pkg/features/tuning.go:39-54`). 값이 0이면 CPU 코어 수에 기반한 휴리스틱을 적용한다:

| 코어 수 | 동시 푸시 제한 |
|---------|-------------|
| 1 | 20 |
| 2 | 25 |
| 4 | 35 |
| 32 | 100 |

`doSendPushes` 함수의 흐름 (discovery.go:469-514):

```
for {
    semaphore <- struct{}{}        // 세마포어 획득 (가득 차면 블로킹)
    client, push := queue.Dequeue() // 다음 프록시 꺼내기
    doneFunc := func() {
        queue.MarkDone(client)
        <-semaphore                 // 세마포어 해제
    }
    go func() {
        client.PushCh() <- pushEv   // 프록시의 개별 채널로 전송
    }()
}
```

### 1.5 Connection 생명주기

프록시가 gRPC 스트림을 열면 다음 단계를 거친다:

```
[Envoy 연결]
    |
    v
initConnection()                    (ads.go:240)
    |-- initProxyMetadata()         메타데이터 파싱
    |-- ClusterAlias 확인            클러스터 별칭 매핑
    |-- proxy.LastPushContext 설정   단조증가 보장
    |-- con.SetID(connectionID)     연결 ID 생성
    |-- authorize()                 xDS 인증
    |-- addCon()                    연결 등록 (푸시 수신 가능)
    |-- initializeProxy()           프록시 완전 초기화
    |-- con.MarkInitialized()       초기화 완료 표시
    |
    v
computeProxyState()                 (ads.go:385)
    |-- SetServiceTargets()         서비스 타겟 설정
    |-- SetWorkloadLabels()         워크로드 레이블 설정
    |-- setTopologyLabels()         토폴로지 레이블 설정
    |-- SetSidecarScope()           사이드카 스코프 계산
    |-- SetGatewaysForProxy()       게이트웨이 설정 (Router 타입만)
    |
    v
pushConnection()                    (ads.go:472)
    |-- Full이면 computeProxyState() 재실행
    |-- ProxyNeedsPush() 확인
    |-- watchedResourcesByOrder()로 리소스 순회
    |-- pushXds(con, w, pushRequest)  각 리소스 타입별 푸시
```

`computeProxyState()`는 프록시의 전체 상태를 갱신하는 핵심 함수이다 (ads.go:385-446). `request.ConfigsUpdated`를 분석하여 어떤 상태를 다시 계산해야 하는지 최소한으로 결정한다:

- `ServiceEntry`, `DestinationRule`, `VirtualService`, `Sidecar` 변경 시 → SidecarScope 재계산
- `Gateway` 변경 시 → 게이트웨이 재계산
- 서비스 업데이트 시 → ServiceTargets 재설정

### 1.6 Start() - 고루틴 구조

```go
// pilot/pkg/xds/discovery.go:226
func (s *DiscoveryServer) Start(stopCh <-chan struct{}) {
    go s.WorkloadEntryController.Run(stopCh)
    go s.handleUpdates(stopCh)       // 디바운스 루프
    go s.periodicRefreshMetrics(stopCh) // 10초마다 메트릭 갱신
    go s.sendPushes(stopCh)          // 세마포어 기반 푸시 루프
    go s.Cache.Run(stopCh)           // xDS 캐시 관리
}
```

```
+-------------------+     pushChannel     +------------------+
|  ConfigUpdate()   | --(버퍼 10)-------> | handleUpdates()  |
|  (여러 소스에서)   |                     |  (디바운스)       |
+-------------------+                     +--------+---------+
                                                   |
                                          Push() / AdsPushAll()
                                                   |
                                          Enqueue(con, req)
                                                   |
                                          +--------v---------+
                                          |   PushQueue      |
                                          |   (pending/      |
                                          |    processing)   |
                                          +--------+---------+
                                                   |
                                          Dequeue()
                                                   |
                                          +--------v---------+
                                          |  sendPushes()    |
                                          |  (세마포어 제한)   |
                                          +--------+---------+
                                                   |
                                          con.PushCh() <- event
                                                   |
                                          +--------v---------+
                                          | pushConnection() |
                                          | (프록시별 처리)    |
                                          +------------------+
```

---

## 2. ConfigGenerator - Envoy 설정 생성기

> 소스 파일: `pilot/pkg/networking/core/configgen.go`, `pilot/pkg/networking/core/cluster.go`, `pilot/pkg/networking/core/listener.go`, `pilot/pkg/networking/core/httproute.go`, `pilot/pkg/networking/core/route/route.go`

ConfigGenerator는 프록시의 메타데이터와 메시 설정을 기반으로 Envoy가 이해할 수 있는 xDS 리소스(Cluster, Listener, Route)를 생성하는 인터페이스이다.

### 2.1 인터페이스 정의

```go
// pilot/pkg/networking/core/configgen.go:28
type ConfigGenerator interface {
    BuildListeners(node *model.Proxy, push *model.PushContext) []*listener.Listener
    BuildClusters(node *model.Proxy, req *model.PushRequest) ([]*discovery.Resource, model.XdsLogDetails)
    BuildHTTPRoutes(node *model.Proxy, req *model.PushRequest, routeNames []string) ([]*discovery.Resource, model.XdsLogDetails)
    BuildNameTable(node *model.Proxy, push *model.PushContext) *dnsProto.NameTable
    BuildExtensionConfiguration(...) []*core.TypedExtensionConfig
    MeshConfigChanged(mesh *meshconfig.MeshConfig)
    // ...
}
```

실제 구현체는 `ConfigGeneratorImpl`이며, `XdsCache`를 내장한다:

```go
// pilot/pkg/networking/core/configgen.go:56
type ConfigGeneratorImpl struct {
    Cache model.XdsCache
}
```

### 2.2 BuildClusters

Cluster는 Envoy가 트래픽을 전달할 대상(upstream) 엔드포인트 그룹이다. `BuildClusters()`는 프록시 타입에 따라 서비스 목록을 결정한 후, 각 서비스/포트/서브셋 조합에 대해 클러스터를 생성한다.

```go
// pilot/pkg/networking/core/cluster.go:57
func (configgen *ConfigGeneratorImpl) BuildClusters(proxy *model.Proxy, req *model.PushRequest) (...) {
    var services []*model.Service
    if features.FilterGatewayClusterConfig && proxy.Type == model.Router {
        services = req.Push.GatewayServices(proxy, envoyFilterPatches)
    } else {
        services = proxy.SidecarScope.Services()
    }
    return configgen.buildClusters(proxy, req, services, envoyFilterPatches)
}
```

**buildOutboundClusters** (cluster.go:369-460)의 처리 흐름:

```
서비스 목록 순회
    |
    v
각 서비스의 각 포트에 대해:
    |-- UDP 포트 건너뜀
    |-- clusterKey 생성
    |-- 캐시 확인 (getAllCachedSubsetClusters)
    |   |-- 캐시 히트 → 바로 사용
    |   |-- 캐시 미스 → 아래 과정 진행
    |
    v
buildCluster()로 기본 클러스터 생성
    |-- 클러스터 이름: "outbound|{port}||{hostname}"
    |-- discoveryType: 서비스 resolution에 따라 결정
    |   |-- ClientSideLB → EDS
    |   |-- Passthrough → ORIGINAL_DST
    |   |-- DNSLB → STRICT_DNS
    |
    v
applyDestinationRule()
    |-- 기본 TrafficPolicy 적용 (로드밸런싱, 커넥션풀, 서킷브레이커)
    |-- 서브셋(subset)별 클러스터 생성
    |   |-- 서브셋 이름: "outbound|{port}|{subset}|{hostname}"
    |   |-- 각 서브셋에 대해 buildSubsetCluster() 호출
    |
    v
EnvoyFilter 패치 적용 후 반환
```

**클러스터 네이밍 규칙**:

```
outbound|{port}|{subset}|{hostname}

예시:
  outbound|80||reviews.default.svc.cluster.local      (기본)
  outbound|80|v1|reviews.default.svc.cluster.local    (서브셋 v1)
  outbound|443||external-api.example.com              (외부 서비스)
```

이 네이밍은 `model.BuildSubsetKey()` 함수로 생성되며, 방향(inbound/outbound), 포트, 서브셋 이름, 호스트명을 파이프(`|`)로 구분한다.

**buildSubsetCluster** (cluster_builder.go:280-336):

```go
// pilot/pkg/networking/core/cluster_builder.go:280
func (cb *ClusterBuilder) buildSubsetCluster(
    opts buildClusterOpts, destRule *config.Config, subset *networking.Subset,
    service *model.Service, endpointBuilder *endpoints.EndpointBuilder,
) *cluster.Cluster {
    subsetClusterName := model.BuildSubsetKey(
        model.TrafficDirectionOutbound, subset.Name, service.Hostname, opts.port.Port)
    // ...
    subsetCluster := cb.buildCluster(subsetClusterName, clusterType, lbEndpoints, ...)
    // 서브셋 트래픽 정책 적용 (상위 정책과 병합)
    opts.policy = util.MergeSubsetTrafficPolicy(opts.policy, subset.TrafficPolicy, opts.port)
    cb.applyTrafficPolicy(service, opts)
    // ...
}
```

**ClusterBuilder** (cluster_builder.go:127-153): 프록시의 모든 관련 속성을 한 곳에 모아 클러스터 생성 컨텍스트를 제공한다.

```go
// pilot/pkg/networking/core/cluster_builder.go:127
type ClusterBuilder struct {
    serviceTargets     []model.ServiceTarget
    clusterID          string
    proxyType          model.NodeType
    sidecarScope       *model.SidecarScope
    locality           *core.Locality
    proxyLabels        map[string]string
    req                *model.PushRequest
    cache              model.XdsCache
    // ...
}
```

### 2.3 BuildListeners

Listener는 Envoy가 트래픽을 수신하는 진입점이다. 프록시 타입에 따라 다른 빌더를 사용한다.

```go
// pilot/pkg/networking/core/listener.go:115
func (configgen *ConfigGeneratorImpl) BuildListeners(node *model.Proxy, push *model.PushContext) []*listener.Listener {
    builder := NewListenerBuilder(node, push)
    switch node.Type {
    case model.SidecarProxy:
        builder = configgen.buildSidecarListeners(builder)
    case model.Waypoint:
        builder = configgen.buildWaypointListeners(builder)
    case model.Router:
        builder = configgen.buildGatewayListeners(builder)
    }
    builder.patchListeners()  // EnvoyFilter 패치 적용
    return builder.getListeners()
}
```

**Sidecar 리스너 빌드 순서** (listener.go:252-261):

```go
func (configgen *ConfigGeneratorImpl) buildSidecarListeners(builder *ListenerBuilder) *ListenerBuilder {
    if builder.push.Mesh.ProxyListenPort > 0 {
        builder.appendSidecarInboundListeners().   // 인바운드 리스너
            appendSidecarOutboundListeners().       // 아웃바운드 리스너
            buildHTTPProxyListener().               // HTTP 프록시 리스너
            buildVirtualOutboundListener()          // 가상 아웃바운드 (15001)
    }
    return builder
}
```

**ListenerBuilder** (listener_builder.go:55-74):

```go
// pilot/pkg/networking/core/listener_builder.go:55
type ListenerBuilder struct {
    node              *model.Proxy
    push              *model.PushContext
    gatewayListeners  []*listener.Listener
    inboundListeners  []*listener.Listener   // 서비스 포트별 인바운드
    outboundListeners []*listener.Listener   // 서비스별 아웃바운드
    httpProxyListener *listener.Listener     // HTTP 프록시 (선택적)
    virtualOutboundListener *listener.Listener  // 15001 포트
    virtualInboundListener  *listener.Listener  // 15006 포트
    authnBuilder      *authn.Builder         // mTLS 설정
    authzBuilder      *authz.Builder         // 인가 정책
}
```

리스너 구조 개요:

```
+------------------------------------------------------+
| Sidecar Proxy                                        |
|                                                       |
| 인바운드 (15006):                                     |
|   virtualInboundListener                              |
|     -> 서비스 포트별 FilterChain (mTLS, authz)         |
|                                                       |
| 아웃바운드 (15001):                                    |
|   virtualOutboundListener (iptables redirect 수신)    |
|     -> UseOriginalDst: true                           |
|     -> 서비스별 outbound listener로 라우팅              |
|                                                       |
| 개별 아웃바운드 리스너:                                  |
|   {서비스IP}_{포트} 형태                                |
|     -> HCM + RDS 참조 또는 TCP proxy                   |
+------------------------------------------------------+
```

### 2.4 BuildHTTPRoutes

HTTP 라우트는 Listener 내 HCM(HttpConnectionManager)에서 참조하는 RDS 설정이다.

```go
// pilot/pkg/networking/core/httproute.go:55
func (configgen *ConfigGeneratorImpl) BuildHTTPRoutes(
    node *model.Proxy, req *model.PushRequest, routeNames []string,
) ([]*discovery.Resource, model.XdsLogDetails) {
    switch node.Type {
    case model.SidecarProxy, model.Waypoint:
        for _, routeName := range routeNames {
            rc, cached := configgen.buildSidecarOutboundHTTPRouteConfig(
                node, req, routeName, vHostCache, efw, envoyfilterKeys)
            // ...
        }
    case model.Router:
        for _, routeName := range routeNames {
            rc := configgen.buildGatewayHTTPRouteConfig(node, req.Push, routeName)
            // ...
        }
    }
}
```

**라우트 생성 파이프라인**:

```
buildSidecarOutboundHTTPRouteConfig()
    |
    v
BuildSidecarVirtualHostWrapper()          (route/route.go:104)
    |-- VirtualService가 있는 서비스들 처리
    |   |-- buildSidecarVirtualHostsForVirtualService()
    |   |     각 VS의 HTTP 라우트를 Envoy 라우트로 변환
    |-- VirtualService가 없는 서비스들 처리
    |   |-- buildSidecarVirtualHostForService()
    |         기본 라우팅 규칙 생성
    |
    v
TranslateRoute()                          (route/route.go:467)
    |-- match 조건 확인 (포트, 소스 레이블, 게이트웨이)
    |-- TranslateRouteMatch()로 Envoy RouteMatch 생성
    |-- 헤더 조작 적용 (추가/삭제)
    |-- destination에 따라 클러스터 라우팅 또는 가중치 기반 라우팅
```

**TranslateRoute** (route/route.go:467-511):

```go
func TranslateRoute(
    node *model.Proxy, in *networking.HTTPRoute, match *networking.HTTPMatchRequest,
    listenPort int, virtualService config.Config, gatewayNames sets.String, opts RouteOptions,
) *route.Route {
    // 포트 매칭 검증
    if match != nil && match.Port != 0 && match.Port != uint32(listenPort) {
        return nil
    }
    // 소스 매칭 검증
    if !sourceMatchHTTP(match, node.Labels, gatewayNames, node.Metadata.Namespace) {
        return nil
    }
    out := &route.Route{
        Name:     routeName,
        Match:    TranslateRouteMatch(virtualService, match),
        Metadata: util.BuildConfigInfoMetadata(virtualService.Meta),
    }
    // 헤더 조작, 라우팅 대상 설정 등
    // ...
}
```

VirtualService에서 Envoy Route로의 변환 예시:

```yaml
# Istio VirtualService
apiVersion: networking.istio.io/v1alpha3
kind: VirtualService
spec:
  hosts: ["reviews"]
  http:
  - match:
    - headers:
        end-user:
          exact: jason
    route:
    - destination:
        host: reviews
        subset: v2
  - route:
    - destination:
        host: reviews
        subset: v3
```

위 VirtualService는 다음과 같은 Envoy 설정으로 변환된다:

```
RouteConfiguration:
  VirtualHost:
    domains: ["reviews", "reviews.default", "reviews.default.svc", ...]
    routes:
      - match: { headers: [{name: "end-user", exact_match: "jason"}] }
        route: { cluster: "outbound|80|v2|reviews.default.svc.cluster.local" }
      - match: { prefix: "/" }
        route: { cluster: "outbound|80|v3|reviews.default.svc.cluster.local" }
```

---

## 3. Aggregate Service Registry - 통합 서비스 레지스트리

> 소스 파일: `pilot/pkg/serviceregistry/aggregate/controller.go`

Istio는 멀티클러스터 환경과 다양한 서비스 소스(Kubernetes, ServiceEntry, MCP 등)를 지원하기 위해, 여러 레지스트리를 하나의 통합 뷰로 합치는 Aggregate Controller를 사용한다.

### 3.1 구조체

```go
// pilot/pkg/serviceregistry/aggregate/controller.go:42
type Controller struct {
    meshHolder      mesh.Holder
    configClusterID cluster.ID
    storeLock       sync.RWMutex
    registries      []*registryEntry    // 등록된 레지스트리 목록
    running         bool
    handlers        model.ControllerHandlers
    handlersByCluster map[cluster.ID]*model.ControllerHandlers
}
```

### 3.2 AddRegistry - 레지스트리 등록 순서

레지스트리 추가 시, **Kubernetes 레지스트리가 항상 먼저 오도록** 정렬한다 (controller.go:212-236).

```go
// pilot/pkg/serviceregistry/aggregate/controller.go:212
func (c *Controller) addRegistry(registry serviceregistry.Instance, stop <-chan struct{}) {
    added := false
    if registry.Provider() == provider.Kubernetes {
        for i, r := range c.registries {
            if r.Provider() != provider.Kubernetes {
                // 첫 번째 비-K8s 레지스트리 위치에 삽입
                c.registries = slices.Insert(c.registries, i, &registryEntry{...})
                added = true
                break
            }
        }
    }
    if !added {
        c.registries = append(c.registries, &registryEntry{...})
    }
    // 이벤트 핸들러 등록
    registry.AppendNetworkGatewayHandler(c.NotifyGatewayHandlers)
    registry.AppendServiceHandler(c.handlers.NotifyServiceHandlers)
}
```

이 순서가 중요한 이유는 `Services()` 호출 시 **첫 번째로 발견된 서비스의 설정이 기본값으로 사용**되기 때문이다. Primary 클러스터의 Kubernetes 서비스가 우선적으로 반영된다.

```
registries = [K8s-primary, K8s-remote-1, K8s-remote-2, ServiceEntry, MCP, ...]
                 ↑ K8s 레지스트리가 앞에          ↑ 비-K8s 레지스트리가 뒤에
```

### 3.3 Services() - 서비스 목록 병합

```go
// pilot/pkg/serviceregistry/aggregate/controller.go:340
func (c *Controller) Services() []*model.Service {
    smap := make(map[host.Name]int)  // hostname → 인덱스 매핑
    services := make([]*model.Service, 0)

    for _, r := range c.GetRegistries() {
        svcs := r.Services()
        if r.Provider() != provider.Kubernetes {
            // 비-K8s: 그대로 추가 (중복검사 없음)
            services = append(services, svcs...)
        } else {
            for _, s := range svcs {
                previous, ok := smap[s.Hostname]
                if !ok {
                    // 처음 본 호스트명 → 그대로 추가
                    smap[s.Hostname] = index
                    services = append(services, s)
                } else {
                    // 이미 있는 호스트명 → ClusterVIPs 병합
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

**mergeService** (controller.go:402-420): 같은 호스트명의 서비스를 여러 클러스터에서 발견했을 때 호출된다.

```go
func mergeService(dst, src *model.Service, srcRegistry serviceregistry.Instance) {
    clusterID := srcRegistry.Cluster()
    if len(dst.ClusterVIPs.GetAddressesFor(clusterID)) == 0 {
        newAddresses := src.ClusterVIPs.GetAddressesFor(clusterID)
        dst.ClusterVIPs.SetAddressesFor(clusterID, newAddresses)
    }
    // 서비스 계정 병합 (트러스트 도메인이 다를 수 있으므로)
    if len(src.ServiceAccounts) > 0 {
        sas := append(dst.ServiceAccounts, src.ServiceAccounts...)
        dst.ServiceAccounts = slices.FilterDuplicates(sas)
    }
}
```

병합 과정을 도식화하면:

```
클러스터 A (primary):
  reviews.default.svc.cluster.local → ClusterIP: 10.0.1.5

클러스터 B (remote):
  reviews.default.svc.cluster.local → ClusterIP: 10.1.2.8

병합 결과:
  reviews.default.svc.cluster.local
    ClusterVIPs:
      cluster-a: [10.0.1.5]
      cluster-b: [10.1.2.8]
    ServiceAccounts: [합집합]
```

### 3.4 GetProxyServiceTargets() - 클러스터 친화성 필터

프록시가 속한 클러스터의 레지스트리에서만 서비스 타겟을 검색한다.

```go
// pilot/pkg/serviceregistry/aggregate/controller.go:459
func (c *Controller) GetProxyServiceTargets(node *model.Proxy) []model.ServiceTarget {
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

`skipSearchingRegistryForProxy()` (controller.go:448-456):

```go
func skipSearchingRegistryForProxy(nodeClusterID cluster.ID, r serviceregistry.Instance) bool {
    // 비-K8s 레지스트리는 항상 검색
    // 클러스터 ID가 없으면 모든 레지스트리 검색
    if r.Provider() != provider.Kubernetes || nodeClusterID == "" {
        return false
    }
    return !r.Cluster().Equals(nodeClusterID)
}
```

### 3.5 HasSynced() - 모든 레지스트리 동기화 확인

```go
// pilot/pkg/serviceregistry/aggregate/controller.go:518
func (c *Controller) HasSynced() bool {
    for _, r := range c.GetRegistries() {
        if !r.HasSynced() {
            return false
        }
    }
    return true
}
```

**모든** 레지스트리가 초기 동기화를 완료해야 `true`를 반환한다. 이는 DiscoveryServer가 `CachesSynced()`에서 이 값을 확인하여 서버 준비 상태를 판단하기 때문이다. 하나라도 동기화되지 않으면 istiod는 연결을 수락하지 않는다.

---

## 4. Kubernetes Controller - K8s 리소스 감시기

> 소스 파일: `pilot/pkg/serviceregistry/kube/controller/controller.go`, `pilot/pkg/serviceregistry/kube/controller/pod.go`, `pilot/pkg/serviceregistry/kube/conversion.go`

Kubernetes Controller는 K8s API 서버의 리소스 변경을 감시하고, Istio 내부 모델로 변환하여 xDS 푸시를 트리거하는 브릿지 역할을 한다.

### 4.1 Controller 구조체

```go
// pilot/pkg/serviceregistry/kube/controller/controller.go:193
type Controller struct {
    opts      Options
    client    kubelib.Client
    queue     queue.Instance            // 이벤트 처리 큐

    namespaces kclient.Client[*v1.Namespace]
    services   kclient.Client[*v1.Service]
    endpoints  *endpointSliceController
    nodes      kclient.Client[*v1.Node]
    pods       *PodCache
    podsClient kclient.Client[*v1.Pod]

    servicesMap              map[host.Name]*model.Service
    nodeSelectorsForServices map[host.Name]labels.Instance
    nodeInfoMap              map[string]kubernetesNode

    ambientIndex             // Ambient 모드 인덱스
    initialSyncTimedout *atomic.Bool
}
```

### 4.2 리소스 감시자 등록

NewController() (controller.go:251-357)에서 각 리소스 타입별로 감시자를 등록한다:

```go
// controller.go:269 - 네임스페이스
c.namespaces = kclient.New[*v1.Namespace](kubeClient)
registerHandlers[*v1.Namespace](c, c.namespaces, "Namespaces", ...)

// controller.go:305 - 서비스
c.services = kclient.NewFiltered[*v1.Service](kubeClient, kclient.Filter{...})
registerHandlers(c, c.services, "Services", c.onServiceEvent, nil)

// controller.go:309 - 엔드포인트슬라이스
c.endpoints = newEndpointSliceController(c)

// controller.go:312 - 노드
c.nodes = kclient.NewFiltered[*v1.Node](kubeClient, kclient.Filter{...})
registerHandlers[*v1.Node](c, c.nodes, "Nodes", c.onNodeEvent, nil)

// controller.go:315-325 - 파드
c.podsClient = kclient.NewFiltered[*v1.Pod](kubeClient, kclient.Filter{
    FieldSelector: "status.phase!=Failed",  // Failed 파드 제외
})
c.pods = newPodCache(c, c.podsClient, func(key types.NamespacedName) {
    c.queue.Push(func() error {
        return c.endpoints.podArrived(key.Name, key.Namespace)
    })
})
registerHandlers[*v1.Pod](c, c.podsClient, "Pods", c.pods.onEvent, nil)
```

### 4.3 이벤트 처리 흐름

`registerHandlers` 함수 (controller.go:629-675)는 K8s Informer의 이벤트를 Controller의 작업 큐로 변환한다:

```
+---------------+     +----------------+     +---------------+     +------------------+
| K8s Informer  | --> | EventHandler   | --> | queue.Push()  | --> | 핸들러 함수       |
| (watch/list)  |     | (Add/Update/   |     | (작업 큐잉)    |     | (모델 변환+푸시)  |
|               |     |  Delete)       |     |               |     |                  |
+---------------+     +----------------+     +---------------+     +------------------+
```

구체적으로:

```go
// controller.go:648-674
informer.AddEventHandler(controllers.EventHandler[T]{
    AddFunc: func(obj T) {
        adds.Increment()
        c.queue.Push(func() error {
            return wrappedHandler(ptr.Empty[T](), obj, model.EventAdd)
        })
    },
    UpdateFunc: func(old, cur T) {
        if filter != nil && filter(old, cur) {
            updatesames.Increment()
            return  // 필터에 의해 무시
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
```

### 4.4 Service 이벤트 처리

`onServiceEvent` (controller.go:433-454)에서 K8s Service를 Istio 모델로 변환하고 푸시를 트리거한다:

```go
func (c *Controller) onServiceEvent(pre, curr *v1.Service, event model.Event) error {
    // 네임스페이스 어노테이션 가져오기 (트래픽 분배 상속)
    var nsAnnotations map[string]string
    ns := c.namespaces.Get(curr.Namespace, "")
    if ns != nil {
        nsAnnotations = ns.Annotations
    }
    // K8s Service → model.Service 변환
    svcConv := kube.ConvertService(*curr, nsAnnotations, c.opts.DomainSuffix,
                                    c.Cluster(), c.meshWatcher.TrustDomain())
    switch event {
    case model.EventDelete:
        c.deleteService(svcConv)
    default:
        c.addOrUpdateService(pre, curr, svcConv, event, false)
    }
    return nil
}
```

**addOrUpdateService** (controller.go:527-572)의 핵심 흐름:

```
addOrUpdateService()
    |
    |-- 네트워크 게이트웨이 처리 (NodePort/LoadBalancer)
    |-- servicesMap 업데이트 (c.servicesMap[hostname] = svcConv)
    |-- 네트워크 변경 시 Full 푸시 트리거
    |-- EDSCacheUpdate(): 엔드포인트 캐시 업데이트
    |-- SvcUpdate(): 서비스 샤드 업데이트 알림
    |-- serviceUpdateNeedsPush() 확인:
    |   |-- 포트 변경, 주소 변경, 레이블 변경 등
    |   |-- true이면 handlers.NotifyServiceHandlers() 호출
    v
xDS 푸시 트리거
```

### 4.5 ConvertService() - K8s Service를 Istio 모델로 변환

```go
// pilot/pkg/serviceregistry/kube/conversion.go:46
func ConvertService(svc corev1.Service, nsAnnotations map[string]string,
    domainSuffix string, clusterID cluster.ID, trustDomain string) *model.Service {

    addrs := []string{constants.UnspecifiedIP}
    resolution := model.ClientSideLB  // 기본값

    // ExternalName 서비스 → Alias
    if svc.Spec.Type == corev1.ServiceTypeExternalName && svc.Spec.ExternalName != "" {
        resolution = model.Alias
    }
    // ClusterIP=None (Headless) → Passthrough
    if svc.Spec.ClusterIP == corev1.ClusterIPNone {
        resolution = model.Passthrough
    } else if svc.Spec.ClusterIP != "" {
        addrs[0] = svc.Spec.ClusterIP
    }

    // 포트 변환
    ports := make([]*model.Port, 0, len(svc.Spec.Ports))
    for _, port := range svc.Spec.Ports {
        ports = append(ports, convertPort(port))
    }

    return &model.Service{
        Hostname:   ServiceHostname(svc.Name, svc.Namespace, domainSuffix),
        ClusterVIPs: model.AddressMap{
            Addresses: map[cluster.ID][]string{clusterID: addrs},
        },
        Ports:      ports,
        Resolution: resolution,
        Attributes: model.ServiceAttributes{
            ServiceRegistry: provider.Kubernetes,
            Name:            svc.Name,
            Namespace:       svc.Namespace,
            Labels:          svc.Labels,
            LabelSelectors:  svc.Spec.Selector,
        },
    }
}
```

Resolution 결정 로직:

| K8s Service 타입 | Resolution | Envoy Cluster 타입 |
|-----------------|------------|-------------------|
| ClusterIP (일반) | ClientSideLB | EDS |
| Headless (ClusterIP=None) | Passthrough | ORIGINAL_DST |
| ExternalName | Alias | - |

### 4.6 PodCache - IP-Pod 매핑

PodCache는 **최종적 일관성(eventual consistency)** 모델로 동작하며, IP에서 Pod로의 매핑을 유지한다.

```go
// pilot/pkg/serviceregistry/kube/controller/pod.go:34
type PodCache struct {
    pods     kclient.Client[*v1.Pod]
    podsByIP map[string]sets.Set[types.NamespacedName]  // IP → Pod 집합
    ipByPods map[types.NamespacedName]string             // Pod → IP (역방향)
    needResync map[string]sets.Set[types.NamespacedName] // 재동기화 필요 목록
    queueEndpointEvent func(types.NamespacedName)
}
```

PodCache가 최종적 일관성을 유지하는 이유:

```
시나리오: EndpointSlice 이벤트가 Pod 이벤트보다 먼저 도착

1. EndpointSlice 업데이트: Pod IP 10.0.0.5의 엔드포인트 추가
   → PodCache에서 10.0.0.5로 Pod 조회 → 아직 없음!
   → needResync[10.0.0.5]에 기록

2. Pod 이벤트: Pod가 Running 상태로 전환, IP=10.0.0.5
   → podsByIP[10.0.0.5] = {pod-name}
   → needResync[10.0.0.5] 확인 → 재큐잉!
   → queueEndpointEvent() 호출 → endpoints.podArrived()
```

이 메커니즘은 K8s의 이벤트 순서가 보장되지 않는 상황에서 데이터 무결성을 유지한다.

### 4.7 HasSynced()

```go
// controller.go:678
func (c *Controller) HasSynced() bool {
    if c.initialSyncTimedout.Load() {
        return true  // 타임아웃 시 강제 true
    }
    if c.ambientIndex != nil && !c.ambientIndex.HasSynced() {
        return false
    }
    return c.queue.HasSynced()  // 큐의 모든 아이템 처리 완료 확인
}
```

`initialSyncTimedout`은 `SyncTimeout` 옵션이 설정된 경우 사용되며, 초기 동기화가 너무 오래 걸릴 때 서버가 무한히 대기하지 않도록 하는 안전장치이다.

---

## 5. Certificate Authority - 인증서 발급 기관

> 소스 파일: `security/pkg/pki/ca/ca.go`, `security/pkg/pki/ca/selfsignedcarootcertrotator.go`

Istio CA (이전 이름: Citadel)는 메시 내 모든 워크로드에 대해 SPIFFE 기반 X.509 인증서를 발급한다. 이를 통해 워크로드 간 mTLS가 가능해진다.

### 5.1 IstioCA 구조체

```go
// security/pkg/pki/ca/ca.go:356
type IstioCA struct {
    defaultCertTTL  time.Duration        // 기본 인증서 수명
    maxCertTTL      time.Duration        // 최대 인증서 수명
    caRSAKeySize    int                  // RSA 키 크기
    keyCertBundle   *util.KeyCertBundle  // CA 키/인증서 번들
    rootCertRotator *SelfSignedCARootCertRotator  // 루트 인증서 로테이터
}
```

### 5.2 CA 모드

Istio CA는 두 가지 모드로 동작한다 (ca.go:100-105):

```go
const (
    selfSignedCA  caTypes = iota  // 자체 서명 CA
    pluggedCertCA                 // 외부 CA 인증서 사용
)
```

**Self-Signed CA** (ca.go:127-220):
- 자체적으로 루트 인증서와 키를 생성
- `istio-ca-secret` 또는 `cacerts` Secret에 저장하여 재시작 시 복원
- 루트 인증서 로테이션 지원

```
초기화 순서:
1. istio-ca-secret 로드 시도
2. 없으면 cacerts 로드 시도 (useCacertsSecretName=true인 경우)
3. 둘 다 없으면 새 자체서명 인증서 생성
4. Secret에 저장하여 영속화
```

**Plugged-in CA** (ca.go:288-328):
- 운영자가 제공한 CA 인증서/키 파일 사용
- `SigningCAFileBundle`로 파일 경로 지정
- IsCA 플래그 검증

### 5.3 IstioCA.Sign() - CSR 서명 과정

```go
// security/pkg/pki/ca/ca.go:400
func (ca *IstioCA) Sign(csrPEM []byte, certOpts CertOpts) ([]byte, error) {
    return ca.sign(csrPEM, certOpts.SubjectIDs, certOpts.TTL, true, certOpts.ForCA)
}
```

`sign()` 내부 로직 (ca.go:478-516):

```
sign(csrPEM, subjectIDs, requestedLifetime, checkLifetime, forCA)
    |
    |-- 1. CA 준비 상태 확인
    |      signingCert, signingKey = ca.keyCertBundle.GetAll()
    |      if signingCert == nil → CANotReady 에러
    |
    |-- 2. CSR 파싱 및 서명 검증
    |      csr = util.ParsePemEncodedCSR(csrPEM)
    |      csr.CheckSignature() → CSR 서명 유효성 확인
    |
    |-- 3. TTL 결정
    |      requestedLifetime <= 0 → defaultCertTTL 사용
    |      requestedLifetime > maxCertTTL → TTLError 반환
    |
    |-- 4. 인증서 생성
    |      certBytes = util.GenCertFromCSR(csr, signingCert,
    |          csr.PublicKey, signingKey, subjectIDs, lifetime, forCA)
    |
    |-- 5. PEM 인코딩
    |      block := &pem.Block{Type: "CERTIFICATE", Bytes: certBytes}
    |      cert = pem.EncodeToMemory(block)
    |
    v
    반환: PEM 인코딩된 인증서
```

**CertOpts** 구조체 (ca.go:85-98):

```go
type CertOpts struct {
    SubjectIDs []string  // SAN에 포함될 SPIFFE ID
    TTL        time.Duration  // 요청된 인증서 수명
    ForCA      bool      // CA 인증서 여부
    CertSigner string    // 인증서 서명자 정보
}
```

TTL 검증 로직:

```
요청된 TTL이 0 이하     → defaultCertTTL 사용
요청된 TTL > maxCertTTL → 에러 반환 (TTLError)
요청된 TTL > 인증서체인 잔여수명 → 인증서체인 잔여수명으로 제한 (minTTL)
```

`minTTL()` 함수 (ca.go:456-476)는 CA 인증서 체인의 잔여 수명보다 긴 워크로드 인증서가 발급되지 않도록 보장한다:

```go
func (ca *IstioCA) minTTL(defaultCertTTL time.Duration) (time.Duration, error) {
    certChainExpiration, err := util.TimeBeforeCertExpires(certChainPem, time.Now())
    if certChainExpiration <= 0 {
        return 0, fmt.Errorf("cert chain has expired")
    }
    if defaultCertTTL > certChainExpiration {
        return certChainExpiration, nil  // 체인 잔여수명으로 제한
    }
    return defaultCertTTL, nil
}
```

### 5.4 Root Cert Rotation - 루트 인증서 로테이션

자체 서명 모드에서는 `SelfSignedCARootCertRotator`가 주기적으로 루트 인증서 만료를 확인하고 필요 시 로테이션한다.

```go
// security/pkg/pki/ca/selfsignedcarootcertrotator.go:51
type SelfSignedCARootCertRotator struct {
    caSecretController *controller.CaSecretController
    config             *SelfSignedCARootCertRotatorConfig
    backOffTime        time.Duration  // 지터(jitter) 백오프
    ca                 *IstioCA
    onRootCertUpdate   func() error   // 루트 인증서 변경 콜백
}
```

**지터(Jitter) 메커니즘** (selfsignedcarootcertrotator.go:63-84):

멀티 인스턴스 환경에서 모든 istiod가 동시에 루트 인증서를 로테이션하면 충돌이 발생할 수 있다. 이를 방지하기 위해 `enableJitter=true`일 때 `[0, CheckInterval)` 범위의 랜덤 백오프를 적용한다.

```go
if config.enableJitter {
    randBackOff := rand.New(rand.NewSource(time.Now().UnixNano()))
    backOffSeconds := int(time.Duration(randBackOff.Int63n(
        int64(rotator.config.CheckInterval))).Seconds())
    rotator.backOffTime = time.Duration(backOffSeconds) * time.Second
}
```

**Run() 루프** (selfsignedcarootcertrotator.go:87-115):

```
[지터 대기 (enableJitter인 경우)]
    |
    v
[ticker 시작 (CheckInterval 간격)]
    |
    v
checkAndRotateRootCert()
    |-- CA Secret 로드
    |-- checkAndRotateRootCertForSigningCertCitadel()
        |
        |-- 인증서 잔여 수명 확인 (certInspector.GetWaitTime)
        |   |-- waitTime > 0 → 아직 유효, 스킵
        |   |   |-- 단, 메모리의 cert != Secret의 cert이면 reload
        |   |-- waitTime <= 0 → 만료 임박, 로테이션 시작
        |
        |-- 기존 키로 새 루트 인증서 생성
        |   GenRootCertFromExistingKey(options)
        |
        |-- updateRootCertificate() 호출
            |-- Secret 업데이트
            |-- KeyCertBundle 업데이트
            |-- onRootCertUpdate 콜백 호출
            |-- 실패 시 rollback 수행
```

**롤백 메커니즘** (selfsignedcarootcertrotator.go:233-261):

로테이션 중 실패하면 이전 인증서로 롤백한다:

```go
if rollback, err := rotator.updateRootCertificate(caSecret, true, pemCert, pemKey, pemRootCerts); err != nil {
    if !rollback {
        return  // 롤백 불필요한 실패 (Secret 업데이트 전 실패)
    }
    // Secret 업데이트는 성공했지만 KeyCertBundle 업데이트 실패 → 롤백
    _, err = rotator.updateRootCertificate(nil, false, oldCaCert, oldCaPrivateKey, oldRootCerts)
}
```

---

## 6. SDS Server - Secret Discovery Service

> 소스 파일: `security/pkg/nodeagent/sds/sdsservice.go`, `security/pkg/nodeagent/sds/server.go`, `security/pkg/nodeagent/cache/secretcache.go`

SDS(Secret Discovery Service)는 Envoy의 인증서 관리를 위한 xDS API이다. Istio의 각 사이드카 프록시(istio-agent)에서 로컬 Unix Domain Socket을 통해 Envoy에 인증서를 제공한다.

### 6.1 서버 아키텍처

```go
// security/pkg/nodeagent/sds/server.go:35
type Server struct {
    workloadSds          *sdsservice
    grpcWorkloadListener net.Listener    // UDS 리스너
    grpcWorkloadServer   *grpc.Server
    stopped              *atomic.Bool
}
```

**UDS(Unix Domain Socket) 서빙** (server.go:79-126):

```go
func (s *Server) initWorkloadSdsService(opts *security.Options) {
    s.grpcWorkloadServer = grpc.NewServer(s.grpcServerOptions()...)
    s.workloadSds.register(s.grpcWorkloadServer)

    path := security.GetIstioSDSServerSocketPath()
    s.grpcWorkloadListener, _ = uds.NewListener(path)

    go func() {
        // 최대 5회 재시도, 지수 백오프
        for i := 0; i < maxRetryTimes; i++ {
            if err = s.grpcWorkloadServer.Serve(s.grpcWorkloadListener); err != nil {
                time.Sleep(waitTime)
                waitTime *= 2
            }
        }
    }()
}
```

UDS를 사용하는 이유:
1. 네트워크를 통하지 않으므로 동일 노드 내에서만 접근 가능
2. 파일 시스템 권한으로 접근 제어 가능
3. TCP 오버헤드 없음

```
+-------------------------------------------+
| Pod                                       |
|                                            |
| +----------+   UDS    +----------------+  |
| |  Envoy   | <------> | istio-agent    |  |
| |  (SDS    |  /var/   | (SDS Server    |  |
| |  client) |  run/    | + SecretMgr)   |  |
| +----------+  secrets +-------+--------+  |
|               /sock           |           |
|                        gRPC   |           |
|                               v           |
|                        +-----------+      |
|                        | istiod CA |      |
|                        +-----------+      |
+-------------------------------------------+
```

### 6.2 sdsservice - SDS 요청 처리

```go
// security/pkg/nodeagent/sds/sdsservice.go:53
type sdsservice struct {
    st         security.SecretManager
    stop       chan struct{}
    rootCaPath string
    pkpConf    *mesh.PrivateKeyProvider
    sync.Mutex
    clients    map[string]*Context
}
```

**워밍(Pre-generation)** (sdsservice.go:90-125):

서비스 시작 시 워크로드 인증서를 미리 생성하여 시작 지연을 줄인다:

```go
go func() {
    b := backoff.NewExponentialBackOff(backoff.DefaultOption())
    _ = b.RetryWithContext(ctx, func() error {
        _, err := st.GenerateSecret(security.WorkloadKeyCertResourceName)
        if err != nil {
            return err  // 재시도
        }
        _, err = st.GenerateSecret(security.RootCertReqResourceName)
        return err
    })
}()
```

**StreamSecrets** (sdsservice.go:271-277):

```go
func (s *sdsservice) StreamSecrets(stream sds.SecretDiscoveryService_StreamSecretsServer) error {
    return xds.Stream(&Context{
        BaseConnection: xds.NewConnection("", stream),
        s:              s,
        w:              &Watch{},
    })
}
```

**인증서 생성 및 응답** (sdsservice.go:129-155):

```go
func (s *sdsservice) generate(resourceNames []string) (*discovery.DiscoveryResponse, error) {
    resources := xds.Resources{}
    for _, resourceName := range resourceNames {
        secret, err := s.st.GenerateSecret(resourceName)
        if err != nil {
            return nil, fmt.Errorf("failed to generate secret for %v: %v", resourceName, err)
        }
        res := protoconv.MessageToAny(toEnvoySecret(secret, s.rootCaPath, s.pkpConf))
        resources = append(resources, &discovery.Resource{
            Name:     resourceName,
            Resource: res,
        })
    }
    return &discovery.DiscoveryResponse{
        TypeUrl:     model.SecretType,
        VersionInfo: time.Now().Format(time.RFC3339) + "/" + strconv.FormatUint(version.Inc(), 10),
        Nonce:       uuid.New().String(),
        Resources:   xds.ResourcesToAny(resources),
    }, nil
}
```

**인증서 변경 시 푸시** (sdsservice.go:162-173):

```go
func (s *sdsservice) push(secretName string) {
    s.Lock()
    defer s.Unlock()
    for _, client := range s.clients {
        go func(client *Context) {
            select {
            case client.XdsConnection().PushCh() <- secretName:
            case <-client.XdsConnection().StreamDone():
            }
        }(client)
    }
}
```

### 6.3 SecretManagerClient - 캐시와 로테이션

```go
// security/pkg/nodeagent/cache/secretcache.go:83
type SecretManagerClient struct {
    caClient      security.Client         // CA 통신 클라이언트
    configOptions *security.Options
    secretHandler func(resourceName string) // 변경 알림 콜백

    cache         secretCache              // 워크로드 인증서 캐시
    generateMutex sync.Mutex               // 동시 CSR 방지

    certWatcher   *fsnotify.Watcher        // 파일 인증서 감시
    fileCerts     map[FileCert]struct{}     // 감시 중인 파일 인증서
    queue         queue.Delayed            // 인증서 로테이션 큐
    stop          chan struct{}
}
```

**두 가지 인증서 취득 모드**:

1. **파일 기반**: `/etc/certs/{key,cert,root-cert.pem}`에 마운트된 인증서 사용
   - `fsnotify.Watcher`로 파일 변경 감시
   - 심링크(symlink) 지원: `TargetPath` 필드로 실제 경로 추적

2. **온디맨드 CSR**: CA 서버에 CSR을 보내 인증서 발급
   - `caClient`를 통해 istiod에 요청
   - 결과를 캐시하고 만료 전 자동 로테이션

### 6.4 rotateTime - 로테이션 시점 계산

```go
// security/pkg/nodeagent/cache/secretcache.go:858
var rotateTime = func(secret security.SecretItem, graceRatio float64, graceRatioJitter float64) time.Duration {
    // thundering herd 방지를 위한 지터
    jitter := (rand.Float64() * graceRatioJitter) * float64(rand.IntN(2)*2-1)
    jitterGraceRatio := graceRatio + jitter
    if jitterGraceRatio > 1 { jitterGraceRatio = 1 }
    if jitterGraceRatio < 0 { jitterGraceRatio = 0 }

    secretLifeTime := secret.ExpireTime.Sub(secret.CreatedTime)
    gracePeriod := time.Duration(jitterGraceRatio * float64(secretLifeTime))
    delay := time.Until(secret.ExpireTime.Add(-gracePeriod))
    if delay < 0 { delay = 0 }
    return delay
}
```

**`SecretRotationGracePeriodRatio`** (`pkg/security/security.go:231`):

기본값 0.10은 인증서 수명의 마지막 10%에 해당하는 시점에 로테이션을 시작한다는 의미이다.

```
인증서 수명 1시간 (TTL=3600s), graceRatio=0.10인 경우:

생성시점                                              만료시점
|<================== 3600s =====================>|
|                                    |<-- 360s -->|
|                               로테이션 시작       만료
|                               (54분 시점)

gracePeriod = 0.10 * 3600s = 360s
delay = 만료시각 - 360s - 현재시각
```

**`SecretRotationGracePeriodRatioJitter`** (`pkg/security/security.go:233-236`):

대규모 클러스터에서 모든 프록시가 동시에 인증서를 갱신하면 CA에 부하가 집중된다. 이를 방지하기 위해 graceRatio에 랜덤 지터를 추가한다:

```
graceRatio = 0.10
graceRatioJitter = 0.05

실제 적용 비율 = 0.10 + (rand * 0.05 * {-1 또는 +1})
              = [0.05, 0.15] 범위의 랜덤 값

인증서 수명 1시간일 때:
  최소: 만료 3분 전 (graceRatio=0.05)
  최대: 만료 9분 전 (graceRatio=0.15)
```

### 6.5 registerSecret - 로테이션 스케줄링

```go
// security/pkg/nodeagent/cache/secretcache.go:877
func (sc *SecretManagerClient) registerSecret(item security.SecretItem) {
    delay := rotateTime(item, sc.configOptions.SecretRotationGracePeriodRatio,
                               sc.configOptions.SecretRotationGracePeriodRatioJitter)

    // 중복 등록 방지
    if sc.cache.GetWorkload() != nil {
        return  // 이미 스케줄됨
    }
    sc.cache.SetWorkload(&item)

    // 딜레이 큐에 로테이션 작업 등록
    sc.queue.PushDelayed(func() error {
        if cached := sc.cache.GetWorkload(); cached != nil {
            if cached.CreatedTime == item.CreatedTime {
                // 스테일 체크: 현재 캐시의 생성시각이 동일한 경우만 로테이션
                sc.cache.SetWorkload(nil)
                sc.OnSecretUpdate(item.ResourceName)
            }
        }
        return nil
    }, delay)
}
```

스테일(stale) 체크가 중요한 이유:

```
시간순서:
1. 인증서 A 생성 → registerSecret(A) → 로테이션 예약 (10분 후)
2. 5분 후, 설정 변경으로 인증서 B 재발급 → registerSecret(B) → 새 로테이션 예약
3. 10분 후, A의 로테이션 타이머 만료
   → cached.CreatedTime (B의 생성시각) != item.CreatedTime (A의 생성시각)
   → 무시 (B의 로테이션이 이미 스케줄됨)
```

### 6.6 파일 기반 인증서 감시

`handleFileWatch()` (secretcache.go:903+)는 `fsnotify.Watcher`를 사용하여 인증서 파일 변경을 감시한다. Kubernetes의 Secret 볼륨 마운트는 심링크를 사용하므로, `FileCert` 구조체에 `TargetPath` 필드가 있어 심링크 해석을 지원한다:

```go
// security/pkg/nodeagent/cache/secretcache.go:162
type FileCert struct {
    ResourceName string
    Filename     string
    TargetPath   string  // 심링크 해석된 실제 경로
}
```

Kubernetes가 Secret을 업데이트하면:
1. 새 데이터 디렉토리 생성 (예: `..2024_01_15_10_30_00.123456789`)
2. `..data` 심링크를 새 디렉토리로 변경
3. `fsnotify`가 심링크 변경 이벤트 감지
4. SDS 서버가 새 인증서를 Envoy에 푸시

---

## 요약: 컴포넌트 간 상호작용

```
+------------------+
| K8s API Server   |
+--------+---------+
         |
    (watch/list)
         |
+--------v---------+     서비스/엔드포인트    +----------------------+
| K8s Controller   | --------변환--------> | Aggregate Registry   |
| (controller.go)  |     model.Service     | (controller.go)      |
+--------+---------+                       +----------+-----------+
         |                                            |
    ConfigUpdate()                            Services()/
         |                                   GetProxyServiceTargets()
+--------v---------+                                  |
| DiscoveryServer  | <--------------------------------+
| (discovery.go)   |     PushContext 생성
|                  |
|  [디바운스]       |
|  [PushQueue]     |
|  [세마포어]       |
+--------+---------+
         |
    pushConnection()
         |
+--------v---------+
| ConfigGenerator  |     CDS/LDS/RDS 생성
| (configgen.go)   |
+--------+---------+
         |
    xDS Response
         |
+--------v---------+     +------------------+
| Envoy Proxy      | <-- | SDS Server       |
|                  |     | (sdsservice.go)  |
|  CDS: 클러스터   |     |                  |
|  LDS: 리스너     |     | 인증서 제공       |
|  RDS: 라우트     |     +--------+---------+
|  SDS: 인증서     |              |
+------------------+     +--------v---------+
                         | IstioCA          |
                         | (ca.go)          |
                         | CSR 서명/인증서발급 |
                         +------------------+
```

이 여섯 가지 핵심 컴포넌트가 유기적으로 동작하여, Kubernetes 리소스 변경이 최종적으로 Envoy 프록시의 설정 업데이트로 이어지는 Istio의 컨트롤 플레인 파이프라인을 구성한다.
