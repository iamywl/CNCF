# 11. Cilium 서비스 메시 및 프록시 서브시스템

---

## 전체 구조

```
┌──────────────────────────────────────────────────────────────────────────────┐
│                           Kubernetes Control Plane                           │
│                      (API Server, etcd, CRDs)                                │
└──────┬─────────────────────────────┬──────────────────────┬──────────────────┘
       │ Watch                       │ Watch                │ Watch
       ▼                             ▼                      ▼
┌──────────────┐            ┌─────────────────┐   ┌──────────────────────┐
│ cilium-       │            │ cilium-agent     │   │ CiliumEnvoyConfig    │
│ operator      │            │ (각 노드)         │   │ CRD (CEC/CCEC)       │
│               │            │                  │   └──────────┬───────────┘
│ - Gateway API │            │ ┌──────────────┐ │              │
│   Reconciler  │────CEC────▶│ │ xDS Server   │ │              │
│ - Ingress     │            │ │ (gRPC)       │ │◄─────────────┘
│   Controller  │            │ └──────┬───────┘ │
└──────────────┘            │        │ xDS     │
                             │        ▼         │
                             │ ┌──────────────┐ │
                             │ │ Envoy Proxy  │ │  ◄── Per-node (사이드카 없음!)
                             │ │ (embedded)   │ │
                             │ └──────┬───────┘ │
                             │        │         │
                             │ ┌──────▼───────┐ │
                             │ │  BPF Datapath│ │  ◄── tc/XDP에서 L7 redirect
                             │ └──────────────┘ │
                             │                  │
                             │ ┌──────────────┐ │
                             │ │ DNS Proxy    │ │  ◄── FQDN 기반 정책
                             │ │ (standalone) │ │
                             │ └──────────────┘ │
                             └──────────────────┘
```

---

## 1. Envoy 프록시 통합 (xDS 제어 프로토콜)

### 1.1 아키텍처 개요

Cilium은 **각 노드에 하나의 Envoy 프록시**를 내장(embedded)하여 운영한다. 전통적인 서비스 메시(Istio 등)가 각 Pod마다 사이드카 프록시를 배치하는 것과 달리, Cilium은 노드 단위로 프록시를 공유하여 리소스 오버헤드를 크게 줄인다.

Cilium agent는 **xDS gRPC 서버**를 운영하며, Envoy는 이 서버에 연결하여 설정을 동적으로 수신한다.

### 1.2 핵심 파일 구조

```
pkg/envoy/
├── cell.go                 ← Hive 모듈 정의 (Envoy 프록시 컨트롤 플레인)
├── embedded_envoy.go       ← Envoy 프로세스 시작/관리
├── grpc.go                 ← xDS gRPC 서버 (LDS/RDS/CDS/EDS/SDS 등록)
├── xds_server.go           ← xDS 리소스 관리 상위 인터페이스
├── resources.go            ← xDS 리소스 타입 URL 상수, NPHDS 캐시
├── secretsync.go           ← K8s Secret → Envoy SDS 동기화
├── accesslog_server.go     ← L7 접근 로그 수집
└── xds/
    ├── server.go           ← xDS 스트림 핸들러 (HandleRequestStream)
    ├── cache.go            ← xDS 리소스 캐시 (버전 관리, 변경 통지)
    ├── ack.go              ← ACK/NACK 처리
    ├── watcher.go          ← 리소스 변경 감시
    └── doc.go              ← xDS 패키지 문서
```

**참조 소스 파일:**
- `/Users/ywlee/cilium/pkg/envoy/cell.go` -- Hive Cell 정의
- `/Users/ywlee/cilium/pkg/envoy/xds_server.go` -- XDSServer 인터페이스 및 구현
- `/Users/ywlee/cilium/pkg/envoy/grpc.go` -- gRPC 서비스 등록
- `/Users/ywlee/cilium/pkg/envoy/resources.go` -- 리소스 타입 URL 상수

### 1.3 XDSServer 인터페이스

`XDSServer`는 Envoy에 리소스를 푸시하는 고수준 인터페이스이다:

```go
// pkg/envoy/xds_server.go
type XDSServer interface {
    AddListener(name string, kind policy.L7ParserType, port uint16, ...) error
    RemoveListener(name string, wg *completion.WaitGroup) ...
    UpsertEnvoyResources(ctx context.Context, resources Resources) error
    UpdateEnvoyResources(ctx context.Context, old, new Resources) error
    DeleteEnvoyResources(ctx context.Context, resources Resources) error
    UpdateNetworkPolicy(ep endpoint.EndpointUpdater, policy *policy.EndpointPolicy, ...) (error, func() error)
    RemoveNetworkPolicy(ep endpoint.EndpointInfoSource)
}
```

### 1.4 xDS 리소스 유형

Cilium의 xDS 서버는 7가지 리소스 타입을 관리한다:

| 약어 | 리소스 타입 | Type URL | 역할 |
|------|------------|----------|------|
| LDS | Listener | `envoy.config.listener.v3.Listener` | 수신 포트, 프로토콜 필터 체인 정의 |
| RDS | Route | `envoy.config.route.v3.RouteConfiguration` | HTTP 라우팅 규칙 (경로, 헤더 매칭) |
| CDS | Cluster | `envoy.config.cluster.v3.Cluster` | 업스트림 서비스 클러스터 정의 |
| EDS | Endpoint | `envoy.config.endpoint.v3.ClusterLoadAssignment` | 업스트림 엔드포인트(IP:port) 목록 |
| SDS | Secret | `envoy.extensions.transport_sockets.tls.v3.Secret` | TLS 인증서, 키 |
| NPDS | NetworkPolicy | `cilium.NetworkPolicy` | Cilium 네트워크 정책 (커스텀) |
| NPHDS | NetworkPolicyHosts | `cilium.NetworkPolicyHosts` | IP→Identity 매핑 (커스텀) |

```go
// pkg/envoy/resources.go - 리소스 타입 URL 상수
const (
    ListenerTypeURL           = "type.googleapis.com/envoy.config.listener.v3.Listener"
    RouteTypeURL              = "type.googleapis.com/envoy.config.route.v3.RouteConfiguration"
    ClusterTypeURL            = "type.googleapis.com/envoy.config.cluster.v3.Cluster"
    EndpointTypeURL           = "type.googleapis.com/envoy.config.endpoint.v3.ClusterLoadAssignment"
    SecretTypeURL             = "type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.Secret"
    NetworkPolicyTypeURL      = "type.googleapis.com/cilium.NetworkPolicy"
    NetworkPolicyHostsTypeURL = "type.googleapis.com/cilium.NetworkPolicyHosts"
)
```

### 1.5 xDS 캐시 및 스트리밍 구조

각 리소스 타입마다 독립적인 캐시가 존재한다:

```go
// pkg/envoy/xds_server.go - initializeXdsConfigs()
func (s *xdsServer) initializeXdsConfigs() {
    ldsCache := xds.NewCache(s.logger)        // Listener 캐시
    ldsMutator := xds.NewAckingResourceMutatorWrapper(s.logger, ldsCache, ...)
    // ... rdsCache, cdsCache, edsCache, sdsCache, npdsCache 동일 패턴

    s.resourceConfig = map[string]*xds.ResourceTypeConfiguration{
        ListenerTypeURL:           ldsConfig,
        RouteTypeURL:              rdsConfig,
        ClusterTypeURL:            cdsConfig,
        EndpointTypeURL:           edsConfig,
        SecretTypeURL:             sdsConfig,
        NetworkPolicyTypeURL:      npdsConfig,
        NetworkPolicyHostsTypeURL: nphdsConfig,
    }
}
```

xDS Cache(`pkg/envoy/xds/cache.go`)는 key-value 구조에 버전 번호를 유지한다:

```go
// pkg/envoy/xds/cache.go
type Cache struct {
    *BaseObservableResourceSource
    resources map[cacheKey]cacheValue  // typeURL + name → value
    version   uint64                    // 변경 시 증가
}
```

### 1.6 gRPC 서비스 등록

`pkg/envoy/grpc.go`에서 각 xDS 서비스가 gRPC 서버에 등록된다:

```go
// pkg/envoy/grpc.go
func (s *xdsServer) startXDSGRPCServer(ctx context.Context, config ...) error {
    grpcServer := grpc.NewServer()
    xdsServer := xds.NewServer(s.logger, config, ...)
    dsServer := (*xdsGRPCServer)(xdsServer)

    envoy_service_secret.RegisterSecretDiscoveryServiceServer(grpcServer, dsServer)
    envoy_service_endpoint.RegisterEndpointDiscoveryServiceServer(grpcServer, dsServer)
    envoy_service_cluster.RegisterClusterDiscoveryServiceServer(grpcServer, dsServer)
    envoy_service_route.RegisterRouteDiscoveryServiceServer(grpcServer, dsServer)
    envoy_service_listener.RegisterListenerDiscoveryServiceServer(grpcServer, dsServer)
    cilium.RegisterNetworkPolicyDiscoveryServiceServer(grpcServer, dsServer)
    cilium.RegisterNetworkPolicyHostsDiscoveryServiceServer(grpcServer, dsServer)
}
```

Envoy는 UNIX 도메인 소켓을 통해 이 gRPC 서버에 연결한다.

### 1.7 CiliumEnvoyConfig CRD

사용자가 Envoy 리소스를 직접 정의할 수 있는 CRD이다:

```go
// pkg/k8s/apis/cilium.io/v2/cec_types.go
type CiliumEnvoyConfig struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata"`
    Spec CiliumEnvoyConfigSpec `json:"spec,omitempty"`
}

type CiliumEnvoyConfigSpec struct {
    Services        []*ServiceListener `json:"services,omitempty"`       // 트래픽 리다이렉트할 서비스
    BackendServices []*Service         `json:"backendServices,omitempty"` // EDS로 동기화할 백엔드
    Resources       []XDSResource      `json:"resources,omitempty"`       // Envoy xDS 리소스
    NodeSelector    *LabelSelector     `json:"nodeSelector,omitempty"`    // 적용 노드 선택
}
```

**참조 소스 파일:** `/Users/ywlee/cilium/pkg/k8s/apis/cilium.io/v2/cec_types.go`

CEC는 Listener, Route, Cluster, Endpoint, Secret 5종의 xDS 리소스를 포함할 수 있으며, Cilium agent가 이를 파싱하여 xDS 캐시에 삽입한다. `CiliumClusterwideEnvoyConfig`(CCEC)는 클러스터 범위의 동일한 메커니즘이다.

---

## 2. DNS Proxy

### 2.1 개요

Cilium의 DNS 프록시는 FQDN 기반 네트워크 정책을 구현한다. Pod가 DNS 쿼리를 보내면 BPF 데이터패스가 이를 DNS 프록시로 리다이렉트하고, 프록시가 응답의 IP를 캐싱하여 `toFQDNs` 정책에 활용한다.

### 2.2 핵심 파일 구조

```
pkg/fqdn/
├── doc.go        ← FQDN 서브시스템 전체 아키텍처 문서 (매우 상세)
├── cache.go      ← DNS 조회 캐시 (TTL 관리, IP 추적)
└── lookup.go     ← DNS IP 레코드 타입 정의

pkg/proxy/
├── dns.go        ← DNS 리다이렉트 구현 (dnsRedirect)
└── proxy.go      ← 프록시 매니저 (DNS, Envoy 통합)
```

**참조 소스 파일:**
- `/Users/ywlee/cilium/pkg/fqdn/doc.go` -- DNS 서브시스템 아키텍처 (ASCII 다이어그램 포함)
- `/Users/ywlee/cilium/pkg/fqdn/cache.go` -- DNSCache 구현
- `/Users/ywlee/cilium/pkg/proxy/dns.go` -- DNS 프록시 통합

### 2.3 DNS 데이터 흐름

```
┌─────────────────────┐
│      DNS Proxy      │  ◄── BPF가 DNS 쿼리를 리다이렉트
├─────────────────────┤
│ per-EP Lookup Cache  │  ◄── 엔드포인트별 DNS 결과 캐시
├─────────────────────┤
│ per-EP Zombie Cache  │  ◄── TTL 만료 후 활성 연결 추적
├─────────────────────┤
│  Global DNS Cache    │  ◄── 전체 엔드포인트 통합 캐시
├─────────────────────┤
│    NameManager       │  ◄── IP→FQDN 매핑 관리
├─────────────────────┤
│ Policy toFQDNs       │  ◄── CIDR Identity 생성
│    Selectors         │
├─────────────────────┤
│  per-EP Datapath     │  ◄── BPF 맵에 정책 적용
└─────────────────────┘
```

### 2.4 DNS 캐시 구조

```go
// pkg/fqdn/cache.go
type cacheEntry struct {
    Name           string      `json:"fqdn,omitempty"`
    LookupTime     time.Time   `json:"lookup-time,omitempty"`
    ExpirationTime time.Time   `json:"expiration-time,omitempty"`
    TTL            int         `json:"ttl,omitempty"`
    IPs            []netip.Addr `json:"ips,omitempty"`
}

type DNSCache struct {
    forward map[string]ipEntries    // FQDN → IP 매핑
    reverse map[netip.Addr]nameEntries  // IP → FQDN 역매핑
    // ...
}
```

### 2.5 L7 DNS 정책 예시

```yaml
apiVersion: cilium.io/v2
kind: CiliumNetworkPolicy
spec:
  endpointSelector: {}
  egress:
    - toEndpoints:
      toPorts:
        - ports:
            - port: "53"
              protocol: ANY
          rules:
            dns:
              - matchPattern: "*.example.com"
    - toFQDNs:
        - matchName: "api.example.com"
```

DNS 프록시가 `*.example.com` 패턴을 허용하고, 응답에서 얻은 IP 주소로 `toFQDNs` L3 규칙이 자동 업데이트된다.

### 2.6 Standalone DNS Proxy (SDP)

Cilium은 독립 DNS 프록시 모드도 지원한다. 이는 Envoy 없이 순수 DNS 프록시만 운영하여 FQDN 기반 정책을 적용하는 경량 모드이다.

```go
// pkg/proxy/proxy.go
type Proxy struct {
    envoyIntegration *envoyProxyIntegration
    dnsIntegration   *dnsProxyIntegration   // DNS 전용 통합
}

// pkg/proxy/dns.go
type dnsRedirect struct {
    Redirect
    dnsProxy fqdnproxy.DNSProxier
}
```

---

## 3. L7 프록시 흐름 (BPF → Envoy → Backend)

### 3.1 패킷 처리 흐름

Cilium의 L7 정책 적용은 BPF 데이터패스와 Envoy 프록시가 협력하여 이루어진다:

```
┌─────┐    ┌──────────┐    ┌─────────────┐    ┌──────────┐    ┌─────────┐
│ Pod │───▶│ BPF      │───▶│ Envoy       │───▶│ BPF      │───▶│ Backend │
│ A   │    │ Datapath │    │ Proxy       │    │ Datapath │    │ Pod B   │
└─────┘    │          │    │             │    │          │    └─────────┘
           │ L3/L4    │    │ L7 정책적용  │    │ L3/L4    │
           │ 필터링    │    │ HTTP 매칭   │    │ 재삽입    │
           │ → proxy  │    │ 헤더 검사    │    │          │
           │   redirect│    │ → forward   │    │          │
           │          │    │   or block   │    │          │
           └──────────┘    └─────────────┘    └──────────┘
```

1. **BPF 리다이렉트**: tc 프로그램이 L7 정책이 필요한 패킷을 감지하면 Envoy 프록시 포트로 리다이렉트
2. **Envoy L7 처리**: HTTP 경로 매칭, 헤더 검사, mTLS 종료, 로드 밸런싱
3. **BPF 재삽입**: Envoy가 처리를 완료하면 패킷을 다시 데이터패스에 주입

### 3.2 프록시 리다이렉트 생성

```go
// pkg/proxy/proxy.go - createRedirectImpl()
func (p *Proxy) createRedirectImpl(redir Redirect, l4 policy.ProxyPolicy, ...) (impl RedirectImplementation, err error) {
    switch l4.GetL7Parser() {
    case policy.ParserTypeDNS:
        return p.dnsIntegration.createRedirect(redir)   // DNS 프록시
    default:
        return p.envoyIntegration.createRedirect(redir, wg, cb)  // Envoy 프록시
    }
}
```

```go
// pkg/proxy/envoyproxy.go - createRedirect()
func (p *envoyProxyIntegration) createRedirect(r Redirect, ...) (RedirectImplementation, error) {
    if r.proxyPort.ProxyType == types.ProxyTypeCRD {
        return &CRDRedirect{Redirect: r}, nil  // CEC에서 정의된 리스너
    }
    // Envoy xDS 서버에 Listener 추가
    err := p.xdsServer.AddListener(redirect.listenerName, ...)
    return redirect, err
}
```

**참조 소스 파일:**
- `/Users/ywlee/cilium/pkg/proxy/proxy.go` -- Proxy 매니저
- `/Users/ywlee/cilium/pkg/proxy/envoyproxy.go` -- Envoy 프록시 통합
- `/Users/ywlee/cilium/pkg/proxy/redirect.go` -- RedirectImplementation 인터페이스

### 3.3 Cilium HTTP 필터

Envoy 리스너에는 Cilium 전용 L7 정책 필터가 주입된다:

```go
// pkg/envoy/xds_server.go
func GetCiliumHttpFilter() *envoy_config_http.HttpFilter {
    return &envoy_config_http.HttpFilter{
        Name: "cilium.l7policy",
        ConfigType: &envoy_config_http.HttpFilter_TypedConfig{
            TypedConfig: toAny(&cilium.L7Policy{
                AccessLogPath:  getAccessLogSocketPath(...),
                Denied_403Body: option.Config.HTTP403Message,
            }),
        },
    }
}
```

이 필터가 Envoy 내에서 Cilium 네트워크 정책을 적용하며, 허용/거부 결정을 내린다.

---

## 4. Per-Node 프록시 모델 vs 전통적 서비스 메시

### 4.1 사이드카 모델의 문제점

전통적인 서비스 메시(Istio + Envoy 사이드카):

```
┌──────────────────────────────────┐
│ Pod                               │
│ ┌──────────┐   ┌──────────────┐  │
│ │ App      │──▶│ Envoy        │  │ ← 매 Pod마다 1개 사이드카
│ │ Container│   │ Sidecar      │  │
│ └──────────┘   └──────────────┘  │
└──────────────────────────────────┘
× 수백 개 Pod = 수백 개 Envoy 프로세스
```

- Pod당 100~200MB 메모리 오버헤드
- 사이드카 주입으로 인한 배포 복잡성
- 사이드카 업데이트 시 Pod 재시작 필요
- iptables 기반 트래픽 캡처 (성능 병목)

### 4.2 Cilium Per-Node 모델

```
┌──────────────────────────────────┐
│ Node                              │
│ ┌────┐ ┌────┐ ┌────┐            │
│ │PodA│ │PodB│ │PodC│            │  ← 사이드카 없음!
│ └──┬─┘ └──┬─┘ └──┬─┘            │
│    │      │      │               │
│    └──────┼──────┘               │
│           │                      │
│    ┌──────▼──────┐               │
│    │ BPF Datapath│               │  ← 커널에서 L3/L4 처리
│    └──────┬──────┘               │
│           │ L7 redirect          │
│    ┌──────▼──────┐               │
│    │ Envoy Proxy │               │  ← 노드당 1개 (공유)
│    │ (per-node)  │               │
│    └─────────────┘               │
└──────────────────────────────────┘
```

장점:
- **노드당 1개 Envoy**: 수백 Pod이 하나의 프록시를 공유
- **BPF 기반 리다이렉트**: iptables 대신 BPF 프로그램이 직접 패킷을 리다이렉트
- **선택적 L7 처리**: L7 정책이 필요한 트래픽만 프록시를 거침
- **투명한 동작**: 애플리케이션 변경 불필요, 사이드카 주입 불필요

### 4.3 임베디드 vs 외부 Envoy

```go
// pkg/envoy/cell.go
if !option.Config.ExternalEnvoyProxy {
    // 임베디드 모드: cilium-agent가 직접 Envoy 프로세스 관리
    return &onDemandXdsStarter{...}, nil
}
// 외부 모드: 별도의 Envoy DaemonSet 사용
return xdsServer, nil
```

- **임베디드 모드**: cilium-agent가 Envoy를 자식 프로세스로 시작/관리 (기본값)
- **외부 모드**: 별도 DaemonSet으로 Envoy 운영 (대규모 환경, 독립 업그레이드)

---

## 5. Gateway API 지원

### 5.1 개요

Cilium은 Kubernetes Gateway API를 네이티브로 구현한다. Gateway API는 Kubernetes Ingress의 차세대 표준으로, 더 풍부한 라우팅 기능과 역할 분리를 제공한다.

### 5.2 핵심 파일 구조

```
operator/pkg/gateway-api/
├── cell.go                    ← Gateway API Hive 모듈 정의
├── controller.go              ← 공통 컨트롤러 유틸리티
├── gateway.go                 ← Gateway 리소스 reconciler 정의
├── gateway_reconcile.go       ← Gateway Reconcile 로직
├── gatewayclass.go            ← GatewayClass reconciler
├── gamma.go                   ← GAMMA (Gateway API for Mesh) reconciler
├── helpers.go                 ← 유틸리티 함수
├── secretsync.go              ← TLS Secret 동기화
└── status.go                  ← 상태 관리

operator/pkg/model/translation/
├── types.go                   ← Translator 인터페이스 정의
├── cec_translator.go          ← CiliumEnvoyConfig 변환 로직
├── envoy_listener.go          ← Envoy Listener 생성
├── envoy_route_configuration.go ← Route 생성
├── envoy_cluster.go           ← Cluster 생성
└── envoy_virtual_host.go      ← VirtualHost 생성
```

**참조 소스 파일:**
- `/Users/ywlee/cilium/operator/pkg/gateway-api/cell.go` -- Gateway API 모듈
- `/Users/ywlee/cilium/operator/pkg/gateway-api/gateway.go` -- Gateway reconciler
- `/Users/ywlee/cilium/operator/pkg/gateway-api/gateway_reconcile.go` -- Reconcile 구현
- `/Users/ywlee/cilium/operator/pkg/model/translation/types.go` -- Translator 인터페이스

### 5.3 동작 구조

```
Gateway API CRDs                  Cilium Operator              Cilium Agent
─────────────────                 ──────────────               ────────────
GatewayClass                       │                            │
Gateway          ───Watch──▶      │ Reconciler                 │
HTTPRoute                          │     │                      │
GRPCRoute                          │     ▼                      │
TLSRoute                           │ Translator                 │
                                   │     │                      │
                                   │     ▼                      │
                                   │ CiliumEnvoyConfig ──────▶  │ xDS Server
                                   │ Service (LB)               │     │
                                   │ EndpointSlice              │     ▼
                                   │                            │  Envoy
```

### 5.4 지원하는 리소스 타입

```go
// operator/pkg/gateway-api/cell.go
var requiredGVKs = []schema.GroupVersionKind{
    gatewayv1.SchemeGroupVersion.WithKind("GatewayClass"),
    gatewayv1.SchemeGroupVersion.WithKind("Gateway"),
    gatewayv1.SchemeGroupVersion.WithKind("HTTPRoute"),
    gatewayv1.SchemeGroupVersion.WithKind("GRPCRoute"),
    gatewayv1beta1.SchemeGroupVersion.WithKind("ReferenceGrant"),
}

var optionalGVKs = []schema.GroupVersionKind{
    gatewayv1alpha2.SchemeGroupVersion.WithKind("TLSRoute"),
    mcsapiv1alpha1.SchemeGroupVersion.WithKind("ServiceImport"),
}
```

### 5.5 Gateway → CiliumEnvoyConfig 변환

Translator 인터페이스가 Gateway API 모델을 CEC로 변환한다:

```go
// operator/pkg/model/translation/types.go
type Translator interface {
    Translate(model *model.Model) (*ciliumv2.CiliumEnvoyConfig, *corev1.Service, *discoveryv1.EndpointSlice, error)
}
```

Gateway reconciler가 HTTPRoute, GRPCRoute 등을 수집하여 Model을 구성하고, Translator가 이를 CiliumEnvoyConfig + LoadBalancer Service + EndpointSlice로 변환한다.

### 5.6 Gateway 설정 옵션

```go
// operator/pkg/gateway-api/cell.go - gatewayApiConfig
type gatewayApiConfig struct {
    EnableGatewayAPISecretsSync            bool    // TLS Secret 동기화
    EnableGatewayAPIProxyProtocol          bool    // Proxy Protocol 지원
    EnableGatewayAPIAppProtocol            bool    // Backend Protocol 선택 (GEP-1911)
    EnableGatewayAPIAlpn                   bool    // ALPN (HTTP/2, HTTP/1.1)
    GatewayAPIServiceExternalTrafficPolicy string  // "Cluster" 또는 "Local"
    GatewayAPISecretsNamespace             string  // Secret 동기화 네임스페이스
    GatewayAPIXffNumTrustedHops            uint32  // X-Forwarded-For 신뢰 홉 수
    GatewayAPIHostnetworkEnabled           bool    // 호스트 네트워크 노출
}
```

---

## 6. Kubernetes Ingress 지원

Cilium은 Gateway API 외에도 전통적인 Kubernetes Ingress 리소스를 지원한다. 내부적으로 Ingress 리소스를 Gateway API 모델로 변환한 후 동일한 파이프라인(CEC 생성 → xDS → Envoy)을 통해 처리한다.

```
Ingress Resource → ingestion.Ingress() → Model → Translator → CEC → xDS → Envoy
```

이 접근 방식의 장점:
- Ingress와 Gateway API가 동일한 Envoy 인프라를 공유
- Ingress에서 Gateway API로의 점진적 마이그레이션 가능
- 두 방식 모두 per-node Envoy 프록시를 활용

---

## 7. Ztunnel (Istio Ambient 호환)

### 7.1 개요

Cilium은 Istio의 Ambient Mesh 모드와 호환되는 **ztunnel** 통합을 제공한다. Ztunnel은 Istio Ambient 메시에서 L4 프록시 역할을 수행하며, 사이드카 없이 mTLS와 L4 정책을 제공한다.

### 7.2 핵심 파일 구조

```
pkg/ztunnel/
├── cell.go                    ← Ztunnel Hive 모듈
├── config/
│   └── config.go              ← Ztunnel 설정
├── xds/
│   ├── cell.go                ← xDS 서버 셀
│   ├── xds_server.go          ← Ztunnel 전용 xDS/CA 서버
│   ├── stream_processor.go    ← 워크로드/서비스 이벤트 처리
│   └── endpoint_event.go      ← 엔드포인트 이벤트
├── zds/
│   ├── cell.go                ← ZDS 서버 셀
│   └── server.go              ← Unix 소켓 기반 ZDS 서버
├── reconciler/
│   └── reconciler.go          ← 네임스페이스별 엔드포인트 등록
├── iptables/
│   └── inpod.go               ← In-Pod iptables 설정
└── pb/
    ├── workload_ztunnel.proto  ← 워크로드 프로토콜 정의
    └── zds_ztunnel.proto       ← ZDS 프로토콜 정의
```

**참조 소스 파일:**
- `/Users/ywlee/cilium/pkg/ztunnel/cell.go` -- Ztunnel 모듈 정의
- `/Users/ywlee/cilium/pkg/ztunnel/xds/xds_server.go` -- Ztunnel xDS 서버
- `/Users/ywlee/cilium/pkg/ztunnel/zds/server.go` -- ZDS 서버
- `/Users/ywlee/cilium/pkg/ztunnel/reconciler/reconciler.go` -- 등록 reconciler

### 7.3 Ztunnel 아키텍처

```
┌─────────────────────────────────────────────────────┐
│ Node                                                 │
│                                                      │
│ ┌──────┐  ┌──────┐  ┌──────┐                       │
│ │ Pod  │  │ Pod  │  │ Pod  │   ← 사이드카 없음      │
│ └──┬───┘  └──┬───┘  └──┬───┘                       │
│    │         │         │                             │
│    └─────────┼─────────┘                             │
│              │                                       │
│    ┌─────────▼─────────┐                            │
│    │ Cilium BPF        │                            │
│    │ Datapath          │                            │
│    └─────────┬─────────┘                            │
│              │                                       │
│    ┌─────────▼─────────┐  ┌──────────────────────┐  │
│    │ Ztunnel (L4)      │  │ Cilium Agent         │  │
│    │ - mTLS            │  │ ┌──────────────────┐ │  │
│    │ - L4 정책          │◄─│ │ Ztunnel xDS     │ │  │
│    │ - HBONE 프록시     │  │ │ Server (gRPC)   │ │  │
│    └───────────────────┘  │ └──────────────────┘ │  │
│              │             │ ┌──────────────────┐ │  │
│    (필요 시)  │             │ │ ZDS Server      │ │  │
│    ┌─────────▼─────────┐  │ │ (Unix socket)   │ │  │
│    │ Waypoint Proxy    │  │ └──────────────────┘ │  │
│    │ (Envoy, L7)       │  └──────────────────────┘  │
│    └───────────────────┘                            │
└─────────────────────────────────────────────────────┘
```

### 7.4 Ztunnel xDS 서버

Cilium은 Ztunnel 전용 xDS 서버를 운영하며, 워크로드 및 서비스 정보를 Istio 프로토콜로 전달한다:

```go
// pkg/ztunnel/xds/xds_server.go
const (
    xdsTypeURLAddress       = "type.googleapis.com/istio.workload.Address"
    xdsTypeURLAuthorization = "type.googleapis.com/istio.security.Authorization"
)

type Server struct {
    // CA(인증서 서명) + ADS(워크로드 정보) 기능 모두 구현
    pb.UnimplementedIstioCertificateServiceServer
    v3.UnimplementedAggregatedDiscoveryServiceServer
    // ...
}
```

### 7.5 ZDS (Ztunnel Discovery Service)

ZDS 서버는 Unix 소켓을 통해 Ztunnel과 통신하며, 워크로드 정보 및 네트워크 네임스페이스 fd를 전달한다:

```go
// pkg/ztunnel/zds/server.go
const defaultZDSUnixAddress = "/var/run/cilium/ztunnel.sock"

func (zc *ztunnelConn) sendMsg(req *pb.WorkloadRequest, ns *netns.NetNS) error {
    var rights []byte
    if ns != nil {
        rights = unix.UnixRights(ns.FD())  // 네트워크 NS fd를 전달
    }
    // ...
}
```

---

## 8. TLS 종료 및 mTLS

### 8.1 TLS 종료

Envoy 프록시가 인그레스 트래픽의 TLS를 종료한다. TLS 인증서는 두 가지 방식으로 Envoy에 전달된다:

1. **SDS (Secret Discovery Service)**: Cilium agent의 xDS 서버가 K8s Secret을 Envoy SDS를 통해 전달
2. **인라인 설정**: CEC 리소스 내에 직접 포함

```go
// pkg/envoy/cell.go - registerSecretSyncer
// K8s Secret이 변경되면 Envoy SDS 캐시를 자동 업데이트
func registerSecretSyncer(params syncerParams) error {
    secretSyncer := newSecretSyncer(secretSyncerLogger, params.XdsServer)
    // 네임스페이스별 Secret 감시 → SDS 캐시 업데이트
}
```

### 8.2 mTLS

- **Gateway API**: BackendTLSPolicy를 통해 백엔드와의 mTLS 구성 가능
- **Ztunnel**: HBONE(HTTP-Based Overlay Network Environment) 프로토콜을 사용하여 Pod 간 mTLS 자동 적용
- **Cilium Policy**: SPIFFE ID 기반 상호 인증 지원

---

## 9. 핵심 설계 원칙 요약

### 9.1 성능 최적화 레이어

```
┌──────────────────────────────────────┐
│ L3/L4: BPF Datapath (커널 공간)       │  ← 최대 성능
├──────────────────────────────────────┤
│ L4 mTLS: Ztunnel (노드당 1개)         │  ← Ambient 모드
├──────────────────────────────────────┤
│ L7: Envoy Proxy (노드당 1개)          │  ← 필요 시에만
├──────────────────────────────────────┤
│ DNS: DNS Proxy (에이전트 내장)         │  ← FQDN 정책
└──────────────────────────────────────┘
```

### 9.2 핵심 원칙

1. **사이드카 없는 서비스 메시**: 노드당 공유 프록시로 리소스 절약
2. **BPF 기반 리다이렉트**: iptables 대신 BPF 프로그램으로 최소 지연
3. **선택적 프록시**: L7 정책이 필요한 트래픽만 프록시 경유
4. **표준 xDS 프로토콜**: Envoy 생태계와의 호환성
5. **CRD 기반 확장**: CiliumEnvoyConfig으로 사용자 정의 Envoy 설정
6. **Gateway API 네이티브**: Ingress와 Gateway API 모두 동일 인프라 사용
7. **Ambient 호환**: Istio ztunnel과의 통합으로 점진적 도입 가능
