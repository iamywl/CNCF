# 12. Istio 트래픽 관리 Deep-Dive

## 목차

1. [개요](#1-개요)
2. [VirtualService](#2-virtualservice)
3. [DestinationRule](#3-destinationrule)
4. [VirtualService → Envoy Route 변환](#4-virtualservice--envoy-route-변환)
5. [DestinationRule → Envoy Cluster 변환](#5-destinationrule--envoy-cluster-변환)
6. [Gateway](#6-gateway)
7. [ServiceEntry](#7-serviceentry)
8. [SidecarScope](#8-sidecarscope)
9. [서킷 브레이커](#9-서킷-브레이커)
10. [로드밸런싱](#10-로드밸런싱)
11. [Fault Injection, Retry, Timeout](#11-fault-injection-retry-timeout)
12. [End-to-End 예제: YAML → Envoy 설정 매핑](#12-end-to-end-예제-yaml--envoy-설정-매핑)

---

## 1. 개요

Istio의 트래픽 관리는 사용자가 Kubernetes CRD(VirtualService, DestinationRule, Gateway, ServiceEntry, Sidecar)로 선언한 의도를 Pilot(istiod)이 Envoy xDS API(Route, Cluster, Listener, Endpoint)로 변환하여 데이터 플레인에 배포하는 과정이다.

```
┌─────────────────────────────────────────────────────────────┐
│  사용자 YAML (Kubernetes CRD)                                │
│  VirtualService / DestinationRule / Gateway / ServiceEntry   │
└───────────────────────┬─────────────────────────────────────┘
                        │ CRD Client (crdclient)
                        v
┌─────────────────────────────────────────────────────────────┐
│  Pilot (istiod) - PushContext                                │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────────┐   │
│  │ route/route.go│  │cluster_builder│  │cluster_traffic   │   │
│  │              │  │   .go        │  │  _policy.go      │   │
│  └──────┬───────┘  └──────┬───────┘  └──────┬───────────┘   │
│         │                 │                  │               │
│         v                 v                  v               │
│  Envoy Route       Envoy Cluster      Traffic Policy        │
│  (RDS)             (CDS)              (CB, LB, TLS)         │
└───────────────────────┬─────────────────────────────────────┘
                        │ xDS (gRPC)
                        v
┌─────────────────────────────────────────────────────────────┐
│  Envoy Sidecar Proxy                                         │
│  Listener → Route → Cluster → Endpoint                       │
└─────────────────────────────────────────────────────────────┘
```

### 핵심 소스코드 파일

| 파일 | 역할 |
|------|------|
| `pilot/pkg/networking/core/route/route.go` | VirtualService → Envoy Route 변환 |
| `pilot/pkg/networking/core/cluster_builder.go` | DestinationRule → Envoy Cluster 변환 |
| `pilot/pkg/networking/core/cluster_traffic_policy.go` | 커넥션 풀, 서킷브레이커, LB 정책 적용 |
| `pilot/pkg/model/sidecar.go` | SidecarScope - 네임스페이스 격리 |
| `pilot/pkg/networking/core/gateway.go` | Gateway 리스너 빌드 |
| `pilot/pkg/serviceregistry/serviceentry/conversion.go` | ServiceEntry → model.Service 변환 |
| `pilot/pkg/config/kube/crdclient/client.go` | Kubernetes CRD 클라이언트 |

---

## 2. VirtualService

VirtualService는 Istio 트래픽 관리의 핵심 리소스로, 서비스에 도달하는 트래픽의 라우팅 규칙을 정의한다.

### 2.1 주요 필드 구조

```yaml
apiVersion: networking.istio.io/v1
kind: VirtualService
metadata:
  name: reviews-route
spec:
  hosts:                    # 라우팅 대상 호스트 (FQDN 또는 short name)
    - reviews.default.svc.cluster.local
  gateways:                 # 적용 대상 게이트웨이 (생략 시 mesh 내부)
    - mesh
  http:                     # HTTP 라우팅 규칙 (순서대로 평가)
    - match:                # 매치 조건
        - uri:
            prefix: /v2
          headers:
            end-user:
              exact: jason
      route:                # 라우팅 목적지
        - destination:
            host: reviews
            subset: v2
          weight: 100
      fault:                # Fault Injection
        delay:
          percentage:
            value: 10
          fixedDelay: 5s
      retries:              # 재시도 정책
        attempts: 3
        perTryTimeout: 2s
        retryOn: "5xx,reset"
      timeout: 10s          # 요청 타임아웃
  tcp:                      # TCP 라우팅 규칙
    - match:
        - port: 27017
      route:
        - destination:
            host: mongo
  tls:                      # TLS 라우팅 규칙
    - match:
        - sniHosts:
            - login.example.com
      route:
        - destination:
            host: login
```

### 2.2 HTTP 매치 조건

`route.go`의 `TranslateRouteMatch` 함수(라인 1059)에서 매치 조건을 Envoy `RouteMatch`로 변환한다.

| Istio 필드 | Envoy 매핑 | 설명 |
|-----------|-----------|------|
| `uri.exact` | `RouteMatch.Path` | 정확한 경로 매칭 |
| `uri.prefix` | `RouteMatch.Prefix` 또는 `PathSeparatedPrefix` | 접두사 매칭 |
| `uri.regex` | `RouteMatch.SafeRegex` | 정규식 매칭 |
| `headers` | `RouteMatch.Headers` | 헤더 기반 매칭 |
| `queryParams` | `RouteMatch.QueryParameters` | 쿼리 파라미터 매칭 |
| `method` | `:method` 헤더 매칭 | HTTP 메서드 매칭 |
| `authority` | `:authority` 헤더 매칭 | Host 헤더 매칭 |
| `scheme` | `:scheme` 헤더 매칭 | HTTP 스키마 매칭 |
| `withoutHeaders` | `HeaderMatcher.InvertMatch=true` | 부정 헤더 매칭 |

소스코드에서 직접 확인할 수 있는 매칭 로직:

```go
// pilot/pkg/networking/core/route/route.go:1059
func TranslateRouteMatch(vs config.Config, in *networking.HTTPMatchRequest) *route.RouteMatch {
    out := &route.RouteMatch{PathSpecifier: &route.RouteMatch_Prefix{Prefix: "/"}}
    if in == nil {
        return out
    }
    // 헤더 매칭
    for name, stringMatch := range in.Headers {
        if metadataMatcher := translateMetadataMatch(name, stringMatch); metadataMatcher != nil {
            out.DynamicMetadata = append(out.DynamicMetadata, metadataMatcher)
        } else {
            matcher := translateHeaderMatch(name, stringMatch)
            out.Headers = append(out.Headers, matcher)
        }
    }
    // URI 매칭
    if in.Uri != nil {
        switch m := in.Uri.MatchType.(type) {
        case *networking.StringMatch_Exact:
            out.PathSpecifier = &route.RouteMatch_Path{Path: m.Exact}
        case *networking.StringMatch_Prefix:
            // Gateway 시맨틱에서는 PathSeparatedPrefix 사용
            if (model.UseIngressSemantics(vs) || model.UseGatewaySemantics(vs)) && m.Prefix != "/" {
                path := strings.TrimSuffix(m.Prefix, "/")
                out.PathSpecifier = &route.RouteMatch_PathSeparatedPrefix{PathSeparatedPrefix: path}
            } else {
                out.PathSpecifier = &route.RouteMatch_Prefix{Prefix: m.Prefix}
            }
        case *networking.StringMatch_Regex:
            out.PathSpecifier = &route.RouteMatch_SafeRegex{
                SafeRegex: &matcher.RegexMatcher{Regex: m.Regex},
            }
        }
    }
    // ...
}
```

### 2.3 소스 매칭 (Gateway/Label 기반)

`sourceMatchHTTP` 함수(라인 447)는 라우팅 규칙이 적용되어야 하는 소스를 결정한다:

```go
// pilot/pkg/networking/core/route/route.go:447
func sourceMatchHTTP(match *networking.HTTPMatchRequest, proxyLabels labels.Instance,
    gatewayNames sets.String, proxyNamespace string) bool {
    if match == nil {
        return true
    }
    // 게이트웨이 이름 매칭
    if len(match.Gateways) > 0 {
        for _, g := range match.Gateways {
            if gatewayNames.Contains(g) { return true }
        }
    } else if labels.Instance(match.GetSourceLabels()).SubsetOf(proxyLabels) {
        return match.SourceNamespace == "" || match.SourceNamespace == proxyNamespace
    }
    return false
}
```

---

## 3. DestinationRule

DestinationRule은 VirtualService에서 라우팅된 트래픽에 대해 실제 적용할 정책(로드밸런싱, 서킷브레이커, TLS, 커넥션 풀)을 정의한다.

### 3.1 주요 필드 구조

```yaml
apiVersion: networking.istio.io/v1
kind: DestinationRule
metadata:
  name: reviews-destination
spec:
  host: reviews.default.svc.cluster.local
  trafficPolicy:            # 전체 서비스에 적용되는 정책
    connectionPool:
      tcp:
        maxConnections: 100
        connectTimeout: 30ms
        tcpKeepalive:
          time: 7200s
          interval: 75s
      http:
        h2UpgradePolicy: DEFAULT
        http1MaxPendingRequests: 1024
        http2MaxRequests: 1024
        maxRequestsPerConnection: 10
        maxRetries: 3
        idleTimeout: 600s
    outlierDetection:
      consecutive5xxErrors: 5
      interval: 10s
      baseEjectionTime: 30s
      maxEjectionPercent: 50
      minHealthPercent: 30
    loadBalancer:
      simple: ROUND_ROBIN
    tls:
      mode: ISTIO_MUTUAL
    portLevelSettings:       # 포트별 정책 오버라이드
      - port:
          number: 8080
        loadBalancer:
          simple: LEAST_REQUEST
  subsets:                   # 서비스 부분집합 정의
    - name: v1
      labels:
        version: v1
    - name: v2
      labels:
        version: v2
      trafficPolicy:         # 서브셋별 정책 오버라이드
        loadBalancer:
          simple: RANDOM
```

### 3.2 트래픽 정책 컴포넌트

`selectTrafficPolicyComponents` 함수(`cluster_traffic_policy.go:72`)에서 정책의 각 구성 요소를 추출한다:

```go
// pilot/pkg/networking/core/cluster_traffic_policy.go:72
func selectTrafficPolicyComponents(policy *networking.TrafficPolicy) (
    *networking.ConnectionPoolSettings,      // 커넥션 풀
    *networking.OutlierDetection,            // 이상 감지 (서킷브레이커)
    *networking.LoadBalancerSettings,         // 로드밸런서
    *networking.ClientTLSSettings,           // TLS 설정
    *networking.TrafficPolicy_ProxyProtocol, // 프록시 프로토콜
    *networking.TrafficPolicy_RetryBudget,   // 재시도 예산
) {
    if policy == nil {
        return nil, nil, nil, nil, nil, nil
    }
    return policy.ConnectionPool, policy.OutlierDetection, policy.LoadBalancer,
           policy.Tls, policy.ProxyProtocol, policy.RetryBudget
}
```

### 3.3 정책 적용 순서

```
┌──────────────────────────────────────┐
│ applyTrafficPolicy (line 43)         │
│                                      │
│  1. selectTrafficPolicyComponents    │
│     └─ connectionPool, outlier,      │
│        loadBalancer, tls, proxy,     │
│        retryBudget 추출              │
│                                      │
│  2. applyH2Upgrade (outbound only)   │
│     └─ HTTP/2 업그레이드 판단         │
│                                      │
│  3. applyConnectionPool              │
│     └─ CircuitBreaker thresholds     │
│     └─ TCP keepalive                 │
│     └─ HTTP protocol options         │
│                                      │
│  4. applyOutlierDetection            │
│     └─ 이상 탐지 설정                 │
│                                      │
│  5. applyLoadBalancer                │
│     └─ LB 알고리즘 선택              │
│     └─ Locality-aware LB             │
│     └─ ConsistentHash (Ring Hash)    │
│                                      │
│  6. buildUpstreamTLSSettings         │
│     └─ mTLS / ISTIO_MUTUAL 설정      │
│                                      │
│  7. ORIGINAL_DST → CLUSTER_PROVIDED  │
└──────────────────────────────────────┘
```

---

## 4. VirtualService → Envoy Route 변환

### 4.1 BuildHTTPRoutesForVirtualService

이 함수(`route.go:400`)는 VirtualService의 HTTP 규칙 배열을 순서대로 반복하면서 Envoy Route 배열을 생성한다.

```go
// pilot/pkg/networking/core/route/route.go:400
func BuildHTTPRoutesForVirtualService(
    node *model.Proxy,
    virtualService config.Config,
    listenPort int,
    gatewayNames sets.String,
    opts RouteOptions,
) ([]*route.Route, error) {
    vs := virtualService.Spec.(*networking.VirtualService)
    out := make([]*route.Route, 0, len(vs.Http))
    catchall := false
    for _, http := range vs.Http {
        if len(http.Match) == 0 {
            // 매치 조건 없음 → catch-all
            if r := TranslateRoute(node, http, nil, listenPort, virtualService,
                gatewayNames, opts); r != nil {
                out = append(out, r)
            }
            catchall = true
        } else {
            for _, match := range http.Match {
                if r := TranslateRoute(node, http, match, listenPort, virtualService,
                    gatewayNames, opts); r != nil {
                    out = append(out, r)
                    if IsCatchAllRoute(r) {
                        catchall = true
                        break
                    }
                }
            }
        }
        if catchall { break }
    }
    return out, nil
}
```

핵심 동작:
- HTTP 규칙들은 **정의된 순서대로** 평가된다
- 매치 조건이 없는 규칙은 **catch-all**로 처리되어 이후 규칙은 무시된다
- 각 매치 조건마다 `TranslateRoute`를 호출하여 개별 Envoy Route를 생성한다

### 4.2 TranslateRoute

`TranslateRoute`(`route.go:467`)는 단일 HTTP 규칙 + 매치 조건 쌍을 Envoy `Route` 객체로 변환한다.

```
TranslateRoute 처리 흐름:

 ┌──────────────┐     ┌────────────────┐     ┌─────────────────┐
 │ Port 매칭     │────>│ Source 매칭     │────>│ RouteMatch 생성  │
 │ (match.Port)  │     │ (Gateway/Label)│     │ TranslateRoute- │
 └──────────────┘     └────────────────┘     │  Match()        │
                                              └────────┬────────┘
                                                       │
                    ┌──────────────────────────────────┘
                    v
 ┌──────────────────────────────────────────────────────────┐
 │ Route 본체 구성                                           │
 │                                                          │
 │  1. Headers 조작 (TranslateHeadersOperations)             │
 │  2. Redirect → Route_Redirect                            │
 │  3. DirectResponse → Route_DirectResponse                │
 │  4. Route Destination → applyHTTPRouteDestination        │
 │     ├─ 단일 destination → RouteAction_Cluster            │
 │     └─ 다중 destination → RouteAction_WeightedClusters   │
 │  5. Fault Injection (TranslateFault)                     │
 │  6. CORS Policy (TranslateCORSPolicy)                    │
 │  7. Retry Policy (retry.ConvertPolicy)                   │
 │  8. Timeout (setTimeout)                                 │
 └──────────────────────────────────────────────────────────┘
```

### 4.3 가중치 클러스터 (Weighted Clusters)

다중 destination이 있을 때의 처리 로직(`route.go:686`):

```go
// pilot/pkg/networking/core/route/route.go:686
if len(in.Route) == 1 {
    // 단일 destination: 직접 클러스터 지정
    hostnames = append(hostnames, processDestination(in.Route[0], opts, listenerPort, out, action))
} else {
    // 다중 destination: WeightedCluster 사용
    weighted := make([]*route.WeightedCluster_ClusterWeight, 0)
    for _, dst := range in.Route {
        if dst.Weight == 0 { continue }  // 가중치 0은 무시
        destinationweight, hostname := processWeightedDestination(dst, opts, listenerPort, action)
        weighted = append(weighted, destinationweight)
    }
    action.ClusterSpecifier = &route.RouteAction_WeightedClusters{
        WeightedClusters: &route.WeightedCluster{Clusters: weighted},
    }
}
```

### 4.4 헤더 조작

`TranslateHeadersOperations`(`route.go:1032`)는 요청/응답 헤더의 추가, 설정, 삭제를 처리한다:

```go
// pilot/pkg/networking/core/route/route.go:1032
func TranslateHeadersOperations(headers *networking.Headers) HeadersOperations {
    req := headers.GetRequest()
    resp := headers.GetResponse()
    // Set은 OVERWRITE_IF_EXISTS_OR_ADD, Add는 APPEND_IF_EXISTS_OR_ADD
    requestHeadersToAdd, setAuthority := translateAppendHeaders(req.GetSet(), false)
    reqAdd, addAuthority := translateAppendHeaders(req.GetAdd(), true)
    requestHeadersToAdd = append(requestHeadersToAdd, reqAdd...)
    // ...
    return HeadersOperations{
        RequestHeadersToAdd:     requestHeadersToAdd,
        ResponseHeadersToAdd:    responseHeadersToAdd,
        RequestHeadersToRemove:  dropInternal(req.GetRemove()),
        ResponseHeadersToRemove: dropInternal(resp.GetRemove()),
        Authority:               auth,
    }
}
```

중요한 설계 결정:
- `:authority`, `:method`, `:scheme` 같은 내부 헤더는 `Remove` 목록에서 **자동으로 제외**된다(`isInternalHeader`)
- Set 연산은 `OVERWRITE_IF_EXISTS_OR_ADD`, Add 연산은 `APPEND_IF_EXISTS_OR_ADD`를 사용한다
- Authority 헤더를 설정하면 Envoy의 `HostRewriteLiteral`로 변환된다

---

## 5. DestinationRule → Envoy Cluster 변환

### 5.1 applyDestinationRule

`applyDestinationRule`(`cluster_builder.go:340`)은 DestinationRule을 기본 클러스터와 서브셋 클러스터들에 적용한다.

```go
// pilot/pkg/networking/core/cluster_builder.go:340
func (cb *ClusterBuilder) applyDestinationRule(mc *clusterWrapper, clusterMode ClusterMode,
    service *model.Service, port *model.Port, eb *endpoints.EndpointBuilder,
    destRule *config.Config, serviceAccounts []string,
) []*cluster.Cluster {
    destinationRule := CastDestinationRule(destRule)
    // 포트별 트래픽 정책 머지
    trafficPolicy, _ := util.GetPortLevelTrafficPolicy(destinationRule.GetTrafficPolicy(), port)

    opts := buildClusterOpts{
        mesh:           cb.req.Push.Mesh,
        mutable:        mc,
        policy:         trafficPolicy,
        port:           port,
        clusterMode:    clusterMode,
        direction:      model.TrafficDirectionOutbound,
    }
    // 기본 클러스터에 트래픽 정책 적용
    cb.applyTrafficPolicy(service, opts)
    maybeApplyEdsConfig(mc.cluster)

    // 서브셋별 클러스터 생성
    subsetClusters := make([]*cluster.Cluster, 0)
    for _, subset := range destinationRule.GetSubsets() {
        subsetCluster := cb.buildSubsetCluster(opts, destRule, subset, service, eb)
        if subsetCluster != nil {
            subsetClusters = append(subsetClusters, subsetCluster)
        }
    }
    return subsetClusters
}
```

### 5.2 서브셋 클러스터 빌드

`buildSubsetCluster`(`cluster_builder.go:280`)는 각 서브셋에 대해 별도의 Envoy Cluster를 생성한다.

```
서브셋 클러스터 생성 과정:

DestinationRule                    Envoy Clusters
┌──────────────────┐              ┌──────────────────────────────────┐
│ host: reviews    │              │ outbound|8080||reviews.default   │
│ subsets:         │              │   (기본 클러스터)                  │
│   - name: v1     │───────────>  │                                  │
│     labels:      │              │ outbound|8080|v1|reviews.default │
│       version:v1 │              │   (v1 서브셋 클러스터)             │
│   - name: v2     │              │                                  │
│     labels:      │              │ outbound|8080|v2|reviews.default │
│       version:v2 │              │   (v2 서브셋 클러스터)             │
└──────────────────┘              └──────────────────────────────────┘
```

클러스터 이름 형식: `model.BuildSubsetKey(direction, subsetName, hostname, port)`

```go
// pilot/pkg/networking/core/cluster_builder.go:287
subsetClusterName = model.BuildSubsetKey(
    model.TrafficDirectionOutbound, subset.Name, service.Hostname, opts.port.Port)
// 예: "outbound|8080|v2|reviews.default.svc.cluster.local"
```

서브셋의 트래픽 정책은 상위 DestinationRule의 정책과 **머지**된다:

```go
// cluster_builder.go:316
opts.policy = util.MergeSubsetTrafficPolicy(opts.policy, subset.TrafficPolicy, opts.port)
// 서브셋 정책이 있으면 상위 정책을 오버라이드
cb.applyTrafficPolicy(service, opts)
```

---

## 6. Gateway

Gateway 리소스는 메시 외부에서 들어오는(인그레스) 또는 메시에서 나가는(이그레스) 트래픽을 처리하는 로드밸런서를 정의한다.

### 6.1 Gateway 리소스 구조

```yaml
apiVersion: networking.istio.io/v1
kind: Gateway
metadata:
  name: my-gateway
spec:
  selector:
    istio: ingressgateway      # Gateway Pod를 선택하는 레이블
  servers:
    - port:
        number: 443
        name: https
        protocol: HTTPS
      hosts:
        - "*.example.com"
      tls:
        mode: SIMPLE
        credentialName: example-tls
    - port:
        number: 80
        name: http
        protocol: HTTP
      hosts:
        - "*.example.com"
```

### 6.2 Gateway 리스너 빌드

`gateway.go`의 `buildGatewayListeners`(라인 117)에서 Gateway 서버 정의를 Envoy Listener로 변환한다:

```go
// pilot/pkg/networking/core/gateway.go:117
func (configgen *ConfigGeneratorImpl) buildGatewayListeners(
    builder *ListenerBuilder) *ListenerBuilder {
    mergedGateway := builder.node.MergedGateway
    for _, port := range mergedGateway.ServerPorts {
        // 권한 없는 Pod에서 1024 이하 포트는 스킵
        if builder.node.IsUnprivileged() && port.Number < 1024 {
            continue
        }
        // ...리스너 생성 및 필터 체인 구성
    }
    return builder
}
```

### 6.3 인그레스 vs 이그레스 게이트웨이

```
인그레스 게이트웨이 흐름:
┌──────────┐     ┌───────────────┐     ┌──────────────┐     ┌──────────┐
│ 외부      │────>│ Ingress       │────>│ VirtualService│────>│ 내부     │
│ 클라이언트 │     │ Gateway Pod   │     │ 라우팅 적용    │     │ 서비스   │
└──────────┘     └───────────────┘     └──────────────┘     └──────────┘

이그레스 게이트웨이 흐름:
┌──────────┐     ┌───────────────┐     ┌──────────────┐     ┌──────────┐
│ 내부      │────>│ Sidecar       │────>│ Egress        │────>│ 외부     │
│ 서비스    │     │ Proxy         │     │ Gateway Pod   │     │ 서비스   │
└──────────┘     └───────────────┘     └──────────────┘     └──────────┘
```

### 6.4 Gateway API 지원

Istio는 Kubernetes Gateway API도 지원한다. `model.UseGatewaySemantics`를 통해 Gateway API 모드인지 판별하고, 라우팅 동작이 약간 달라진다:

- `PathSeparatedPrefix` 사용 (기존 `Prefix` 대신)
- 잘못된 백엔드에 대해 500 응답 코드 반환 (`ClusterNotFoundResponseCode: INTERNAL_SERVER_ERROR`)
- 리다이렉트에서 `%PREFIX()%` 치환 지원

```go
// route.go:617
if model.UseGatewaySemantics(vs) {
    action.ClusterNotFoundResponseCode = route.RouteAction_INTERNAL_SERVER_ERROR
}
```

---

## 7. ServiceEntry

ServiceEntry는 메시 외부의 서비스(예: 외부 API, 레거시 시스템)를 Istio 서비스 레지스트리에 등록하는 리소스이다.

### 7.1 ServiceEntry 구조

```yaml
apiVersion: networking.istio.io/v1
kind: ServiceEntry
metadata:
  name: external-api
spec:
  hosts:
    - api.external.com
  location: MESH_EXTERNAL         # MESH_EXTERNAL 또는 MESH_INTERNAL
  ports:
    - number: 443
      name: https
      protocol: HTTPS
  resolution: DNS                  # NONE, STATIC, DNS, DNS_ROUND_ROBIN
  endpoints:                       # STATIC resolution일 때 명시적 엔드포인트
    - address: 192.168.1.10
      ports:
        https: 8443
```

### 7.2 Resolution 모드

`conversion.go`(라인 201~225)에서 Resolution 모드를 내부 모델로 변환한다:

| ServiceEntry Resolution | 내부 모델 | Envoy 클러스터 타입 | 설명 |
|------------------------|----------|-------------------|------|
| `NONE` | `model.Passthrough` | `ORIGINAL_DST` | 원본 IP로 직접 전달 |
| `STATIC` | `model.ClientSideLB` | `EDS` 또는 `STATIC` | 명시적 엔드포인트 사용 |
| `DNS` | `model.DNSLB` | `STRICT_DNS` | DNS 조회, 모든 결과에 LB |
| `DNS_ROUND_ROBIN` | `model.DNSRoundRobinLB` | `LOGICAL_DNS` | DNS 조회, 첫 번째 결과 사용 |
| `DYNAMIC_DNS` | `model.DynamicDNS` | `STRICT_DNS` (동적) | 동적 DNS 해석 |

```go
// pilot/pkg/serviceregistry/serviceentry/conversion.go:214
switch serviceEntry.Resolution {
case networking.ServiceEntry_NONE:
    resolution = model.Passthrough
case networking.ServiceEntry_DNS:
    resolution = model.DNSLB
case networking.ServiceEntry_DNS_ROUND_ROBIN:
    resolution = model.DNSRoundRobinLB
case networking.ServiceEntry_STATIC:
    resolution = model.ClientSideLB
case networking.ServiceEntry_DYNAMIC_DNS:
    resolution = model.DynamicDNS
}
```

### 7.3 DNS 클러스터 설정

DNS 타입 클러스터는 `buildCluster`(`cluster_builder.go:455`)에서 추가 설정이 적용된다:

```go
// cluster_builder.go:470
case cluster.Cluster_STRICT_DNS, cluster.Cluster_LOGICAL_DNS:
    // IPv4/IPv6 패밀리 자동 감지
    if networkutil.AllIPv4(cb.proxyIPAddresses) {
        c.DnsLookupFamily = cluster.Cluster_V4_ONLY
    } else if networkutil.AllIPv6(cb.proxyIPAddresses) {
        c.DnsLookupFamily = cluster.Cluster_V6_ONLY
    } else { // Dual Stack
        c.DnsLookupFamily = cluster.Cluster_ALL  // Happy Eyeballs
    }
    c.RespectDnsTtl = true
    c.DnsRefreshRate = cb.req.Push.Mesh.DnsRefreshRate
```

---

## 8. SidecarScope

### 8.1 개요

`SidecarScope`(`model/sidecar.go:140`)는 Sidecar CRD를 기반으로 각 프록시 워크로드가 볼 수 있는 서비스, VirtualService, DestinationRule의 범위를 사전 계산한다.

```go
// pilot/pkg/model/sidecar.go:140
type SidecarScope struct {
    Name      string
    Namespace string
    Sidecar   *networking.Sidecar
    Version   string

    EgressListeners    []*IstioEgressListenerWrapper // 이그레스 리스너 목록
    services           []*Service                     // 볼 수 있는 서비스 합집합
    servicesByHostname map[host.Name]*Service          // 호스트명으로 인덱스

    destinationRules        map[host.Name][]*ConsolidatedDestRule
    destinationRulesByNames map[types.NamespacedName]*config.Config

    OutboundTrafficPolicy *networking.OutboundTrafficPolicy // ALLOW_ANY 또는 REGISTRY_ONLY
    configDependencies    sets.Set[ConfigHash]              // 의존 설정 추적
}
```

### 8.2 Sidecar CRD 예제

```yaml
apiVersion: networking.istio.io/v1
kind: Sidecar
metadata:
  name: default
  namespace: bookinfo
spec:
  egress:
    - hosts:
        - "./*"                          # 같은 네임스페이스의 모든 서비스
        - "istio-system/*"               # istio-system의 모든 서비스
    - port:
        number: 27017
        protocol: MONGO
        name: mongo
      bind: 0.0.0.0
      hosts:
        - "bookinfo/mongo.bookinfo.svc.cluster.local"
  outboundTrafficPolicy:
    mode: REGISTRY_ONLY                  # 등록된 서비스만 허용
```

### 8.3 이그레스 리스너와 서비스 범위

`convertIstioListenerToWrapper`(`sidecar.go:430`)에서 각 이그레스 리스너의 서비스 범위를 계산한다:

```go
// pilot/pkg/model/sidecar.go:430
func convertIstioListenerToWrapper(ps *PushContext, configNamespace string,
    istioListener *networking.IstioEgressListener) *IstioEgressListenerWrapper {
    out := &IstioEgressListenerWrapper{
        IstioListener: istioListener,
        matchPort:     needsPortMatch(istioListener),
    }
    // 호스트 패턴 파싱: "namespace/hostname"
    hostsByNamespace := make(map[string]hostClassification)
    for _, h := range istioListener.Hosts {
        ns, name, _ := strings.Cut(h, "/")
        if ns == currentNamespace { ns = configNamespace }
        // ...
    }
    // VirtualService와 Service 선택
    out.virtualServices = SelectVirtualServices(ps.virtualServiceIndex, configNamespace, hostsByNamespace)
    svces := ps.servicesExportedToNamespace(configNamespace)
    out.services = out.selectServices(svces, configNamespace, hostsByNamespace)
    return out
}
```

### 8.4 호스트 매칭 알고리즘

`hostClassification.Matches`(`sidecar.go:71`)는 두 단계로 매칭을 수행한다:

```
매칭 순서:
1. exactHosts (Set) → O(1) 정확한 매칭
2. allHosts (슬라이스) → 와일드카드 매칭 (SubsetOf 체크)

예: Sidecar egress hosts = ["bookinfo/*", "istio-system/istiod.istio-system"]

  서비스 "reviews.bookinfo"            → "bookinfo/*"와 매칭 (와일드카드)
  서비스 "istiod.istio-system"         → exactHosts에서 즉시 매칭
  서비스 "productpage.frontend"        → 매칭 실패 → 이 사이드카에 보이지 않음
```

### 8.5 DestinationRule 선택

`DestinationRule`(`sidecar.go:599`) 메서드는 워크로드 셀렉터를 고려하여 가장 적합한 DR을 반환한다:

```go
// pilot/pkg/model/sidecar.go:599
func (sc *SidecarScope) DestinationRule(direction TrafficDirection,
    proxy *Proxy, svc host.Name) *ConsolidatedDestRule {
    destinationRules := sc.destinationRules[svc]
    var catchAllDr *ConsolidatedDestRule
    for _, destRule := range destinationRules {
        dr := destRule.rule.Spec.(*networking.DestinationRule)
        if dr.GetWorkloadSelector() == nil {
            catchAllDr = destRule
        }
        // outbound에서만 workloadSelector 적용
        if sc.Namespace == destRule.rule.Namespace &&
            dr.GetWorkloadSelector() != nil && direction == TrafficDirectionOutbound {
            workloadSelector := labels.Instance(dr.GetWorkloadSelector().GetMatchLabels())
            if workloadSelector.SubsetOf(proxy.Labels) {
                return destRule  // 워크로드 특정 DR 우선
            }
        }
    }
    return catchAllDr  // catch-all DR 반환
}
```

---

## 9. 서킷 브레이커

### 9.1 커넥션 풀 설정 → CircuitBreaker

`applyConnectionPool`(`cluster_traffic_policy.go:94`)에서 DestinationRule의 `connectionPool` 설정을 Envoy CircuitBreaker로 변환한다.

```go
// pilot/pkg/networking/core/cluster_traffic_policy.go:94
func (cb *ClusterBuilder) applyConnectionPool(mesh *meshconfig.MeshConfig,
    mc *clusterWrapper, settings *networking.ConnectionPoolSettings,
    retryBudget *networking.TrafficPolicy_RetryBudget) {

    threshold := getDefaultCircuitBreakerThresholds()
    // HTTP 설정
    if settings.Http != nil {
        if settings.Http.Http2MaxRequests > 0 {
            threshold.MaxRequests = &wrapperspb.UInt32Value{
                Value: uint32(settings.Http.Http2MaxRequests)}
        }
        if settings.Http.Http1MaxPendingRequests > 0 {
            threshold.MaxPendingRequests = &wrapperspb.UInt32Value{
                Value: uint32(settings.Http.Http1MaxPendingRequests)}
        }
        if settings.Http.MaxRetries > 0 {
            threshold.MaxRetries = &wrapperspb.UInt32Value{
                Value: uint32(settings.Http.MaxRetries)}
        }
    }
    // TCP 설정
    if settings.Tcp != nil {
        if settings.Tcp.MaxConnections > 0 {
            threshold.MaxConnections = &wrapperspb.UInt32Value{
                Value: uint32(settings.Tcp.MaxConnections)}
        }
    }
    mc.cluster.CircuitBreakers = &cluster.CircuitBreakers{
        Thresholds: []*cluster.CircuitBreakers_Thresholds{threshold},
    }
}
```

### 9.2 기본 임계값

Istio의 기본 서킷 브레이커 임계값은 Envoy의 기본값(1024)과 **다르게** MaxUint32로 설정되어, 사실상 비활성화 상태이다:

```go
// cluster_traffic_policy.go:434
func getDefaultCircuitBreakerThresholds() *cluster.CircuitBreakers_Thresholds {
    return &cluster.CircuitBreakers_Thresholds{
        MaxRetries:         &wrapperspb.UInt32Value{Value: math.MaxUint32},
        MaxRequests:        &wrapperspb.UInt32Value{Value: math.MaxUint32},
        MaxConnections:     &wrapperspb.UInt32Value{Value: math.MaxUint32},
        MaxPendingRequests: &wrapperspb.UInt32Value{Value: math.MaxUint32},
        TrackRemaining:     true,  // 잔여 리소스 추적 활성화
    }
}
```

이유: 기본 Envoy 값(1024)은 Kubernetes 환경에서 Pod 롤링 업데이트 시 서킷 브레이커가 너무 쉽게 트리거되어 503 오류를 발생시키기 때문이다.

### 9.3 매핑 테이블

| Istio 설정 | Envoy CircuitBreakers | 설명 |
|-----------|---------------------|------|
| `connectionPool.tcp.maxConnections` | `MaxConnections` | 최대 TCP 연결 수 |
| `connectionPool.http.http1MaxPendingRequests` | `MaxPendingRequests` | 최대 대기 요청 수 |
| `connectionPool.http.http2MaxRequests` | `MaxRequests` | 최대 동시 요청 수 |
| `connectionPool.http.maxRetries` | `MaxRetries` | 최대 동시 재시도 수 |
| `connectionPool.tcp.connectTimeout` | `Cluster.ConnectTimeout` | TCP 연결 타임아웃 |
| `connectionPool.http.idleTimeout` | `CommonHttpProtocolOptions.IdleTimeout` | HTTP 유휴 타임아웃 |
| `connectionPool.http.maxRequestsPerConnection` | `MaxRequestsPerConnection` | 연결당 최대 요청 수 |

### 9.4 Outlier Detection (이상 감지)

`applyOutlierDetection`(`cluster_traffic_policy.go:450`)에서 비정상 엔드포인트를 자동 제거하는 정책을 적용한다:

```go
// pilot/pkg/networking/core/cluster_traffic_policy.go:450
func applyOutlierDetection(service *model.Service, c *cluster.Cluster,
    outlier *networking.OutlierDetection) {
    out := &cluster.OutlierDetection{}

    // Success Rate 기반 감지는 기본 비활성화
    out.EnforcingSuccessRate = &wrapperspb.UInt32Value{Value: 0}

    // 연속 5xx 에러
    if e := outlier.Consecutive_5XxErrors; e != nil {
        v := e.GetValue()
        out.Consecutive_5Xx = &wrapperspb.UInt32Value{Value: v}
        if v > 0 { v = 100 }
        out.EnforcingConsecutive_5Xx = &wrapperspb.UInt32Value{Value: v}
    }
    // 연속 게이트웨이 에러
    if e := outlier.ConsecutiveGatewayErrors; e != nil {
        v := e.GetValue()
        out.ConsecutiveGatewayFailure = &wrapperspb.UInt32Value{Value: v}
        if v > 0 { v = 100 }
        out.EnforcingConsecutiveGatewayFailure = &wrapperspb.UInt32Value{Value: v}
    }

    c.OutlierDetection = out

    // Panic Threshold: k8s 환경에서는 기본 0 (비활성화)
    minHealthPercent := outlier.MinHealthPercent
    if service.SupportsUnhealthyEndpoints() {
        minHealthPercent = 0  // Unready Pod로 트래픽 방지
    }
    c.CommonLbConfig.HealthyPanicThreshold = &xdstype.Percent{Value: float64(minHealthPercent)}
}
```

### 9.5 Retry Budget

Retry Budget(`cluster_traffic_policy.go:182`)은 동시 재시도 요청의 비율을 제한한다:

```go
// cluster_traffic_policy.go:182
func applyRetryBudget(thresholds *cluster.CircuitBreakers_Thresholds,
    retryBudget *networking.TrafficPolicy_RetryBudget) {
    percent := &xdstype.Percent{Value: 0.2}     // 기본 20%
    if retryBudget.Percent != nil {
        percent = &xdstype.Percent{Value: retryBudget.Percent.Value}
    }
    retryConcurrency := &wrapperspb.UInt32Value{Value: 3}  // 기본 최소 3
    if retryBudget.MinRetryConcurrency > 0 {
        retryConcurrency = &wrapperspb.UInt32Value{Value: retryBudget.MinRetryConcurrency}
    }
    thresholds.RetryBudget = &cluster.CircuitBreakers_Thresholds_RetryBudget{
        BudgetPercent:       percent,
        MinRetryConcurrency: retryConcurrency,
    }
}
```

---

## 10. 로드밸런싱

### 10.1 로드밸런서 알고리즘

`applyLoadBalancer`(`cluster_traffic_policy.go:259`)에서 DestinationRule의 LB 설정을 Envoy 클러스터 LB 정책으로 변환한다.

| Istio 설정 | Envoy LbPolicy | 설명 |
|-----------|---------------|------|
| `ROUND_ROBIN` | `Cluster_ROUND_ROBIN` | 라운드 로빈 (순차 분배) |
| `LEAST_REQUEST` | `Cluster_LEAST_REQUEST` | 최소 요청 수 기반 (기본값) |
| `RANDOM` | `Cluster_RANDOM` | 랜덤 분배 |
| `PASSTHROUGH` | `Cluster_CLUSTER_PROVIDED` + `ORIGINAL_DST` | 원본 목적지 직접 전달 |
| (미지정) | `Cluster_LEAST_REQUEST` | **기본 알고리즘** |

```go
// cluster_traffic_policy.go:290
switch lb.GetSimple() {
case networking.LoadBalancerSettings_LEAST_CONN, networking.LoadBalancerSettings_LEAST_REQUEST:
    applyLeastRequestLoadBalancer(c, lb)
case networking.LoadBalancerSettings_RANDOM:
    c.LbPolicy = cluster.Cluster_RANDOM
case networking.LoadBalancerSettings_ROUND_ROBIN:
    applyRoundRobinLoadBalancer(c, lb)
case networking.LoadBalancerSettings_PASSTHROUGH:
    c.LbPolicy = cluster.Cluster_CLUSTER_PROVIDED
    c.ClusterDiscoveryType = &cluster.Cluster_Type{Type: cluster.Cluster_ORIGINAL_DST}
    c.LoadAssignment = nil
default:
    applySimpleDefaultLoadBalancer(c, lb) // LEAST_REQUEST
}
// ConsistentHash가 있으면 RING_HASH 또는 MAGLEV로 오버라이드
ApplyRingHashLoadBalancer(c, lb)
```

기본 알고리즘 확인:
```go
// cluster_traffic_policy.go:351
func defaultLBAlgorithm() cluster.Cluster_LbPolicy {
    return cluster.Cluster_LEAST_REQUEST  // Istio 기본값
}
```

### 10.2 ConsistentHash 로드밸런싱

ConsistentHash를 사용하면 동일한 키를 가진 요청이 항상 같은 엔드포인트로 라우팅된다.

```yaml
# DestinationRule에서 ConsistentHash 설정
spec:
  trafficPolicy:
    loadBalancer:
      consistentHash:
        httpCookie:
          name: user_session
          ttl: 0s
```

`ApplyRingHashLoadBalancer`(`cluster_traffic_policy.go:520`)에서 해시 방식에 따라 RING_HASH 또는 MAGLEV를 선택한다:

```go
// cluster_traffic_policy.go:520
func ApplyRingHashLoadBalancer(c *cluster.Cluster, lb *networking.LoadBalancerSettings) {
    consistentHash := lb.GetConsistentHash()
    if consistentHash == nil { return }
    switch {
    case consistentHash.GetMaglev() != nil:
        c.LbPolicy = cluster.Cluster_MAGLEV
        // MAGLEV 테이블 사이즈 설정
    case consistentHash.GetRingHash() != nil:
        c.LbPolicy = cluster.Cluster_RING_HASH
        // Ring Hash 최소 링 사이즈 설정
    default:
        c.LbPolicy = cluster.Cluster_RING_HASH
        // 기본 최소 링 사이즈: 1024
    }
}
```

`consistentHashToHashPolicy`(`route.go:1468`)에서 해시 키를 Envoy Route의 `HashPolicy`로 변환한다:

| ConsistentHash 키 | Envoy HashPolicy | 설명 |
|-------------------|-----------------|------|
| `httpHeaderName` | `HashPolicy_Header_` | 특정 HTTP 헤더 값으로 해싱 |
| `httpCookie` | `HashPolicy_Cookie_` | 쿠키 값으로 해싱 (TTL, Path, Attributes 지원) |
| `useSourceIp` | `HashPolicy_ConnectionProperties_` | 소스 IP로 해싱 |
| `httpQueryParameterName` | `HashPolicy_QueryParameter_` | 쿼리 파라미터 값으로 해싱 |

```go
// route.go:1468
func consistentHashToHashPolicy(consistentHash *networking.LoadBalancerSettings_ConsistentHashLB) *route.RouteAction_HashPolicy {
    switch consistentHash.GetHashKey().(type) {
    case *networking.LoadBalancerSettings_ConsistentHashLB_HttpHeaderName:
        return &route.RouteAction_HashPolicy{
            PolicySpecifier: &route.RouteAction_HashPolicy_Header_{
                Header: &route.RouteAction_HashPolicy_Header{
                    HeaderName: consistentHash.GetHttpHeaderName(),
                },
            },
        }
    case *networking.LoadBalancerSettings_ConsistentHashLB_HttpCookie:
        cookie := consistentHash.GetHttpCookie()
        return &route.RouteAction_HashPolicy{
            PolicySpecifier: &route.RouteAction_HashPolicy_Cookie_{
                Cookie: &route.RouteAction_HashPolicy_Cookie{
                    Name: cookie.GetName(),
                    Ttl:  cookie.GetTtl(),
                    Path: cookie.GetPath(),
                },
            },
        }
    case *networking.LoadBalancerSettings_ConsistentHashLB_UseSourceIp:
        return &route.RouteAction_HashPolicy{
            PolicySpecifier: &route.RouteAction_HashPolicy_ConnectionProperties_{
                ConnectionProperties: &route.RouteAction_HashPolicy_ConnectionProperties{
                    SourceIp: consistentHash.GetUseSourceIp(),
                },
            },
        }
    }
}
```

### 10.3 Slow Start (워밍업)

새로 추가된 엔드포인트에 점진적으로 트래픽을 증가시키는 기능:

```go
// cluster_traffic_policy.go:409
func setWarmup(warmup *networking.WarmupConfiguration) *cluster.Cluster_SlowStartConfig {
    var aggression, minWeightPercent float64
    if warmup.Aggression == nil { aggression = 1 } else { aggression = warmup.Aggression.GetValue() }
    if warmup.MinimumPercent == nil { minWeightPercent = 10 } else { minWeightPercent = warmup.MinimumPercent.GetValue() }
    return &cluster.Cluster_SlowStartConfig{
        SlowStartWindow:  warmup.Duration,
        Aggression:       &core.RuntimeDouble{DefaultValue: aggression},
        MinWeightPercent: &xdstype.Percent{Value: minWeightPercent},
    }
}
```

### 10.4 Locality-Aware 로드밸런싱

`applyLocalityLoadBalancer`(`cluster_traffic_policy.go:310`)에서 지역 기반 트래픽 분배를 설정한다:

```go
// cluster_traffic_policy.go:310
func applyLocalityLoadBalancer(locality *core.Locality, proxyLabels map[string]string,
    c *cluster.Cluster, wrappedLocalityLbEndpoints *loadbalancer.WrappedLocalityLbEndpoints,
    localityLB *networking.LocalityLoadBalancerSetting, failover bool) {

    enableFailover := failover || c.OutlierDetection != nil

    // LocalityWeightedLbConfig 설정
    if features.EnableLocalityWeightedLbConfig ||
        (enableFailover && (localityLB.GetFailover() != nil || localityLB.GetFailoverPriority() != nil)) ||
        localityLB.GetDistribute() != nil {
        c.CommonLbConfig.LocalityConfigSpecifier = &cluster.Cluster_CommonLbConfig_LocalityWeightedLbConfig_{
            LocalityWeightedLbConfig: &cluster.Cluster_CommonLbConfig_LocalityWeightedLbConfig{},
        }
    }
    // ...
}
```

---

## 11. Fault Injection, Retry, Timeout

### 11.1 Fault Injection

`TranslateFault`(`route.go:1388`)에서 VirtualService의 fault 설정을 Envoy HTTPFault 필터로 변환한다.

```yaml
# VirtualService fault 설정 예제
fault:
  delay:
    percentage:
      value: 10.0         # 10% 요청에 지연 주입
    fixedDelay: 5s         # 5초 지연
  abort:
    percentage:
      value: 5.0           # 5% 요청에 오류 주입
    httpStatus: 503         # 503 응답
```

```go
// pilot/pkg/networking/core/route/route.go:1388
func TranslateFault(in *networking.HTTPFaultInjection) *xdshttpfault.HTTPFault {
    out := xdshttpfault.HTTPFault{}

    if in.Delay != nil {
        out.Delay = &xdsfault.FaultDelay{}
        if in.Delay.Percentage != nil {
            out.Delay.Percentage = translatePercentToFractionalPercent(in.Delay.Percentage)
        }
        switch d := in.Delay.HttpDelayType.(type) {
        case *networking.HTTPFaultInjection_Delay_FixedDelay:
            out.Delay.FaultDelaySecifier = &xdsfault.FaultDelay_FixedDelay{
                FixedDelay: d.FixedDelay,
            }
        }
    }

    if in.Abort != nil {
        out.Abort = &xdshttpfault.FaultAbort{}
        if in.Abort.Percentage != nil {
            out.Abort.Percentage = translatePercentToFractionalPercent(in.Abort.Percentage)
        }
        switch a := in.Abort.ErrorType.(type) {
        case *networking.HTTPFaultInjection_Abort_HttpStatus:
            out.Abort.ErrorType = &xdshttpfault.FaultAbort_HttpStatus{
                HttpStatus: uint32(a.HttpStatus),
            }
        case *networking.HTTPFaultInjection_Abort_GrpcStatus:
            out.Abort.ErrorType = &xdshttpfault.FaultAbort_GrpcStatus{
                GrpcStatus: uint32(grpc.SupportedGRPCStatus[a.GrpcStatus]),
            }
        }
    }
    return &out
}
```

Fault는 Envoy Route의 `TypedPerFilterConfig`에 per-route 필터로 삽입된다:

```go
// route.go:563
if in.Fault != nil {
    out.TypedPerFilterConfig[wellknown.Fault] = protoconv.MessageToAny(TranslateFault(in.Fault))
}
```

### 11.2 Retry

`retry.ConvertPolicy`(`retry/retry.go:96`)에서 VirtualService의 retry 설정을 Envoy `RetryPolicy`로 변환한다.

```yaml
# VirtualService retry 설정 예제
retries:
  attempts: 3              # 최대 재시도 횟수
  perTryTimeout: 2s        # 각 시도별 타임아웃
  retryOn: "5xx,reset,connect-failure,retriable-status-codes"
  retryRemoteLocalities: true   # 다른 지역으로 재시도
  retryIgnorePreviousHosts: true # 이전 실패 호스트 회피
  backoff:
    baseInterval: 100ms    # 지수 백오프 기본 간격
```

```go
// pilot/pkg/networking/core/route/retry/retry.go:96
func ConvertPolicy(in *networking.HTTPRetry, hashPolicy bool) *route.RetryPolicy {
    if in == nil {
        if hashPolicy { return cachedDefaultConsistentHashPolicy }
        return cachedDefaultPolicy
    }
    if in.Attempts <= 0 { return nil }  // 명시적 비활성화

    out := newRetryPolicy(hashPolicy)
    out.NumRetries = &wrappers.UInt32Value{Value: uint32(in.Attempts)}

    if in.RetryOn != "" {
        out.RetryOn, out.RetriableStatusCodes = parseRetryOn(in.RetryOn)
    }
    if in.PerTryTimeout != nil {
        out.PerTryTimeout = in.PerTryTimeout
    }
    if in.RetryIgnorePreviousHosts != nil && in.RetryIgnorePreviousHosts.GetValue() {
        out.RetryHostPredicate = defaultRetryHostPredicate
    }
    if in.RetryRemoteLocalities != nil && in.RetryRemoteLocalities.GetValue() {
        out.RetryPriority = cachedRetryRemoteLocalitiesPredicate
    }
    if in.Backoff != nil {
        out.RetryBackOff = &route.RetryPolicy_RetryBackOff{BaseInterval: in.Backoff}
    }
    return out
}
```

ConsistentHash가 적용된 경우 재시도 정책이 약간 다르다: 이전에 실패한 호스트를 회피하는 `RetryHostPredicate`가 기본으로 설정된다.

### 11.3 Timeout

`setTimeout`(`route.go:1336`)에서 VirtualService의 timeout을 Envoy Route의 타임아웃으로 변환한다:

```go
// pilot/pkg/networking/core/route/route.go:1336
func setTimeout(action *route.RouteAction, vsTimeout *durationpb.Duration, node *model.Proxy) {
    action.Timeout = Notimeout  // 기본: 타임아웃 없음 (0)
    if vsTimeout != nil {
        action.Timeout = vsTimeout
    }
    // gRPC 프록시리스 모드
    if node != nil && node.IsProxylessGrpc() {
        action.MaxStreamDuration = &route.RouteAction_MaxStreamDuration{
            MaxStreamDuration: action.Timeout,
        }
    } else {
        // gRPC timeout 헤더 처리
        if action.Timeout.AsDuration().Nanoseconds() == 0 {
            action.MaxGrpcTimeout = Notimeout  // grpc-timeout 헤더 무시
        } else {
            action.MaxGrpcTimeout = action.Timeout  // VS 타임아웃으로 제한
        }
    }
}
```

타임아웃 우선순위:
1. VirtualService `timeout` 필드 (최우선)
2. gRPC의 경우 `grpc-timeout` 헤더 (VS timeout이 없을 때)
3. 기본값: 0 (타임아웃 없음)

---

## 12. End-to-End 예제: YAML → Envoy 설정 매핑

### 12.1 사용자 YAML

```yaml
# VirtualService
apiVersion: networking.istio.io/v1
kind: VirtualService
metadata:
  name: reviews-route
  namespace: default
spec:
  hosts:
    - reviews
  http:
    - match:
        - headers:
            end-user:
              exact: jason
      route:
        - destination:
            host: reviews
            subset: v2
          weight: 80
        - destination:
            host: reviews
            subset: v3
          weight: 20
      fault:
        delay:
          percentage:
            value: 10
          fixedDelay: 3s
      retries:
        attempts: 3
        perTryTimeout: 2s
      timeout: 10s
    - route:
        - destination:
            host: reviews
            subset: v1
---
# DestinationRule
apiVersion: networking.istio.io/v1
kind: DestinationRule
metadata:
  name: reviews-dr
  namespace: default
spec:
  host: reviews
  trafficPolicy:
    connectionPool:
      tcp:
        maxConnections: 100
      http:
        http2MaxRequests: 1000
        maxRetries: 3
    outlierDetection:
      consecutive5xxErrors: 5
      interval: 10s
      baseEjectionTime: 30s
    loadBalancer:
      simple: ROUND_ROBIN
  subsets:
    - name: v1
      labels:
        version: v1
    - name: v2
      labels:
        version: v2
    - name: v3
      labels:
        version: v3
      trafficPolicy:
        loadBalancer:
          consistentHash:
            httpCookie:
              name: user
              ttl: 0s
```

### 12.2 생성되는 Envoy 설정

#### RDS (Route Discovery Service)

```json
{
  "name": "reviews-route",
  "match": {
    "prefix": "/",
    "headers": [{
      "name": "end-user",
      "string_match": { "exact": "jason" }
    }]
  },
  "route": {
    "weighted_clusters": {
      "clusters": [
        {
          "name": "outbound|8080|v2|reviews.default.svc.cluster.local",
          "weight": 80
        },
        {
          "name": "outbound|8080|v3|reviews.default.svc.cluster.local",
          "weight": 20
        }
      ]
    },
    "timeout": "10s",
    "retry_policy": {
      "retry_on": "connect-failure,refused-stream,unavailable,cancelled,retriable-status-codes",
      "num_retries": 3,
      "per_try_timeout": "2s"
    },
    "hash_policy": [{
      "cookie": { "name": "user", "ttl": "0s" }
    }]
  },
  "typed_per_filter_config": {
    "envoy.filters.http.fault": {
      "delay": {
        "fixed_delay": "3s",
        "percentage": { "numerator": 10, "denominator": "MILLION" }
      }
    }
  }
}
```

#### CDS (Cluster Discovery Service)

```json
[
  {
    "name": "outbound|8080||reviews.default.svc.cluster.local",
    "type": "EDS",
    "lb_policy": "ROUND_ROBIN",
    "circuit_breakers": {
      "thresholds": [{
        "max_connections": 100,
        "max_requests": 1000,
        "max_retries": 3,
        "max_pending_requests": 4294967295
      }]
    },
    "outlier_detection": {
      "consecutive_5xx": 5,
      "enforcing_consecutive_5xx": 100,
      "interval": "10s",
      "base_ejection_time": "30s",
      "enforcing_success_rate": 0
    },
    "common_lb_config": {
      "healthy_panic_threshold": { "value": 0 }
    }
  },
  {
    "name": "outbound|8080|v1|reviews.default.svc.cluster.local",
    "type": "EDS",
    "lb_policy": "ROUND_ROBIN",
    "circuit_breakers": { "thresholds": [{ "max_connections": 100, "max_requests": 1000, "max_retries": 3 }] },
    "outlier_detection": { "consecutive_5xx": 5, "interval": "10s", "base_ejection_time": "30s" }
  },
  {
    "name": "outbound|8080|v2|reviews.default.svc.cluster.local",
    "type": "EDS",
    "lb_policy": "ROUND_ROBIN",
    "circuit_breakers": { "thresholds": [{ "max_connections": 100, "max_requests": 1000, "max_retries": 3 }] },
    "outlier_detection": { "consecutive_5xx": 5, "interval": "10s", "base_ejection_time": "30s" }
  },
  {
    "name": "outbound|8080|v3|reviews.default.svc.cluster.local",
    "type": "EDS",
    "lb_policy": "RING_HASH",
    "lb_config": {
      "ring_hash_lb_config": { "minimum_ring_size": 1024 }
    },
    "circuit_breakers": { "thresholds": [{ "max_connections": 100, "max_requests": 1000, "max_retries": 3 }] },
    "outlier_detection": { "consecutive_5xx": 5, "interval": "10s", "base_ejection_time": "30s" }
  }
]
```

### 12.3 변환 흐름 요약

```
사용자 YAML
    │
    ├── VirtualService ──────────────────────────────────────────────────────────┐
    │   │                                                                        │
    │   │  BuildHTTPRoutesForVirtualService (route.go:400)                       │
    │   │    │                                                                   │
    │   │    ├── match[0]: headers={end-user:jason}                              │
    │   │    │     │                                                             │
    │   │    │     └── TranslateRoute (route.go:467)                            │
    │   │    │           ├── TranslateRouteMatch → RouteMatch (headers)          │
    │   │    │           ├── applyHTTPRouteDestination                           │
    │   │    │           │     ├── processWeightedDestination(v2, 80)            │
    │   │    │           │     └── processWeightedDestination(v3, 20)            │
    │   │    │           │         → WeightedClusters                            │
    │   │    │           ├── TranslateFault → HTTPFault (delay:3s,10%)           │
    │   │    │           ├── retry.ConvertPolicy → RetryPolicy (3회, 2s/try)     │
    │   │    │           └── setTimeout → timeout:10s                            │
    │   │    │                                                                   │
    │   │    └── match[1]: (catch-all, no match conditions)                      │
    │   │          └── TranslateRoute → RouteAction_Cluster(v1)                  │
    │   │                                                                        │
    │   └──────────────────────────────────────── Envoy RDS ─────────────────────┘
    │
    └── DestinationRule ─────────────────────────────────────────────────────────┐
        │                                                                        │
        │  applyDestinationRule (cluster_builder.go:340)                         │
        │    │                                                                   │
        │    ├── 기본 클러스터 (outbound|8080||reviews...)                        │
        │    │     └── applyTrafficPolicy                                        │
        │    │           ├── applyConnectionPool → CircuitBreakers               │
        │    │           ├── applyOutlierDetection → OutlierDetection            │
        │    │           └── applyLoadBalancer → ROUND_ROBIN                     │
        │    │                                                                   │
        │    ├── buildSubsetCluster(v1) → outbound|8080|v1|reviews...            │
        │    │     └── applyTrafficPolicy (상위 정책 상속)                         │
        │    │                                                                   │
        │    ├── buildSubsetCluster(v2) → outbound|8080|v2|reviews...            │
        │    │     └── applyTrafficPolicy (상위 정책 상속)                         │
        │    │                                                                   │
        │    └── buildSubsetCluster(v3) → outbound|8080|v3|reviews...            │
        │          └── MergeSubsetTrafficPolicy                                  │
        │               └── applyTrafficPolicy                                   │
        │                    └── ApplyRingHashLoadBalancer → RING_HASH            │
        │                                                                        │
        └──────────────────────────────────── Envoy CDS ─────────────────────────┘
```

### 12.4 CRD Client의 역할

`crdclient/client.go`의 `Client` 구조체는 Kubernetes API 서버에서 Istio CRD를 감시(watch)하고 캐시한다:

```go
// pilot/pkg/config/kube/crdclient/client.go:58
type Client struct {
    schemas          collection.Schemas            // 지원하는 스키마 세트
    domainSuffix     string                        // 도메인 접미사
    revision         string                        // 컨트롤 플레인 리비전
    kinds            map[config.GroupVersionKind]nsStore  // GVK별 캐시 스토어
    schemasByCRDName map[string]resource.Schema     // CRD 이름으로 스키마 조회
    client           kube.Client                    // Kubernetes 클라이언트
}
```

이 클라이언트는 `model.ConfigStoreController` 인터페이스를 구현하여, VirtualService, DestinationRule, Gateway, ServiceEntry 등의 CRD가 생성/수정/삭제될 때 Pilot의 PushContext를 업데이트하고, 이를 통해 xDS 재배포가 트리거된다.

---

## 참고: 주요 설계 결정 사항

### 왜 기본 서킷 브레이커 임계값이 MaxUint32인가?

Envoy의 기본값(1024)은 소규모 Kubernetes 클러스터에서 롤링 업데이트 중 서킷 브레이커가 과도하게 트리거되어 503 에러를 유발한다. Istio는 이를 사실상 무한대로 설정하여, 사용자가 명시적으로 설정하지 않는 한 서킷 브레이커가 개입하지 않도록 했다.

### 왜 LEAST_REQUEST가 기본 LB 알고리즘인가?

`defaultLBAlgorithm`은 `LEAST_REQUEST`를 반환한다. ROUND_ROBIN보다 부하가 고르지 않은 환경에서 더 나은 성능을 보이며, 특히 Pod마다 처리 능력이 다를 수 있는 Kubernetes 환경에 적합하다.

### 왜 Outlier Detection에서 Success Rate를 기본 비활성화하는가?

`EnforcingSuccessRate`가 0으로 설정되는 이유는, Success Rate 기반 감지는 충분한 트래픽 샘플이 필요한데, 소수의 Pod만 있는 서비스에서는 통계적으로 의미있는 판단이 어렵기 때문이다. 대신 연속 5xx 에러 기반 감지를 주로 사용한다.

### 왜 Panic Threshold를 0으로 설정하는가?

Kubernetes 환경에서 서비스당 Pod 수가 적은 경우, Envoy의 기본 Panic Threshold(50%)가 트리거되면 비정상 엔드포인트로도 트래픽이 전송된다. Istio는 이를 0으로 설정하여 비정상 엔드포인트로의 트래픽 전송을 방지한다.
