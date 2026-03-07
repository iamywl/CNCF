# 02. Istio 데이터 모델

## 1. 데이터 모델 계층 구조

Istio의 데이터 모델은 4개 계층으로 구성된다.

```
┌─────────────────────────────────────────────┐
│  Config Layer (pkg/config/model.go)          │
│  ├─ Meta (GVK, Name, Namespace, Labels)      │
│  ├─ Spec (protobuf 메시지)                    │
│  └─ Status (장기 실행 상태)                    │
└──────────────────┬──────────────────────────┘
                   │
┌──────────────────┴──────────────────────────┐
│  Service Layer (pilot/pkg/model/service.go)  │
│  ├─ Service (Hostname, Ports, VIPs)          │
│  ├─ IstioEndpoint (IP, Port, Locality)       │
│  └─ ServiceInstance (Service + Port + EP)    │
└──────────────────┬──────────────────────────┘
                   │
┌──────────────────┴──────────────────────────┐
│  Proxy Layer (pilot/pkg/model/context.go)    │
│  ├─ Proxy (ID, Type, Labels, SidecarScope)   │
│  ├─ NodeMetadata (워크로드 메타데이터)         │
│  └─ WatchedResource (구독 리소스)             │
└──────────────────┬──────────────────────────┘
                   │
┌──────────────────┴──────────────────────────┐
│  Push Layer (pilot/pkg/model/push_context.go)│
│  ├─ PushContext (불변 설정 스냅샷)            │
│  ├─ ServiceIndex (서비스 인덱스)              │
│  └─ VirtualServiceIndex, DestRuleIndex, ...  │
└─────────────────────────────────────────────┘
```

## 2. Config 모델

### 2.1 Meta — 모든 설정 객체의 메타데이터

```go
// pkg/config/model.go
type Meta struct {
    GroupVersionKind  GroupVersionKind         // 예: networking.istio.io/v1/VirtualService
    UID               string                  // 고유 식별자
    Name              string                  // 네임스페이스 내 고유 이름
    Namespace         string                  // 네임스페이스
    Domain            string                  // DNS 도메인 접미사
    Labels            map[string]string       // 레이블
    Annotations       map[string]string       // 어노테이션
    ResourceVersion   string                  // 불투명 버전 트래커
    CreationTimestamp time.Time               // 생성 시각
    Generation        int64                   // 원하는 상태 버전
}
```

### 2.2 Config — 설정 래퍼

```go
// pkg/config/model.go
type Config struct {
    Meta                         // 임베디드 메타데이터
    Spec   Spec                  // protobuf 메시지 (VirtualService, DestinationRule 등)
    Status Status                // 장기 실행 상태
    Extra  map[string]any        // 내부 처리 메타데이터
}
```

**왜 이렇게 설계했나?**
- `Spec`이 `any` 타입이라 VirtualService, DestinationRule 등 모든 Istio CRD를 하나의 구조체로 표현
- `Config.Key()` → `group/version/kind/namespace/name` 형태의 고유 키 반환
- DeepCopy 지원으로 동시성 안전 보장

## 3. Service 모델

### 3.1 Service — 서비스 정의

```go
// pilot/pkg/model/service.go
type Service struct {
    Attributes   ServiceAttributes    // 서비스 메타데이터
    Ports        PortList             // 네트워크 포트 목록
    Hostname     host.Name            // FQDN (예: catalog.mystore.com)
    ClusterVIPs  AddressMap           // 클러스터별 로드밸런서 IP
    DefaultAddress string             // 기본 서비스 IP
    Resolution   Resolution           // 해석 방식
    MeshExternal bool                 // 메시 외부 서비스 여부
}
```

### 3.2 Resolution — 서비스 해석 방식

| 값 | 의미 | Envoy Discovery 타입 | 사용 사례 |
|----|------|---------------------|----------|
| `ClientSideLB` | 프록시가 로컬 엔드포인트 풀에서 선택 | EDS | K8s Service |
| `DNSLB` | 프록시가 DNS 해석 | STRICT_DNS | ServiceEntry (DNS) |
| `DNSRoundRobinLB` | DNS 라운드로빈 | LOGICAL_DNS | ServiceEntry (논리적 DNS) |
| `Passthrough` | 목적지 IP로 직접 전달 | ORIGINAL_DST | Headless Service |
| `Alias` | 다른 서비스의 별칭 | - | ExternalName |

### 3.3 ServiceAttributes — 서비스 속성

```go
type ServiceAttributes struct {
    ServiceRegistry provider.ID                // 출처 (Kubernetes, Consul 등)
    Name            string                     // destination.service.name
    Namespace       string                     // destination.service.namespace
    Labels          map[string]string          // 서비스 레이블
    ExportTo        sets.Set[visibility.Instance] // 가시성 범위
    LabelSelectors  map[string]string          // 워크로드 선택 레이블
    K8sAttributes   K8sAttributes              // K8s 전용 속성
}
```

### 3.4 IstioEndpoint — 네트워크 엔드포인트

```go
// pilot/pkg/model/service.go
type IstioEndpoint struct {
    // 식별
    Labels          labels.Instance    // 워크로드 레이블
    Addresses       []string           // IP 주소 (Dual Stack 지원)
    ServicePortName string             // 포트 이름
    ServiceAccount  string             // 서비스 어카운트

    // 위치
    Network     network.ID     // 네트워크 ID
    Locality    Locality       // region/zone/subzone + ClusterID
    Namespace   string         // 네임스페이스
    NodeName    string         // K8s 노드

    // 설정
    EndpointPort uint32        // 워크로드 리스닝 포트
    LbWeight     uint32        // 로드밸런싱 가중치 (0 = 1)
    TLSMode      string        // "istio" (mTLS) 또는 "disabled"
    HealthStatus HealthStatus  // Healthy, Unhealthy, Draining, Terminating
}
```

**HealthStatus 상태 머신:**
```
Healthy ──(readiness 실패)──→ Unhealthy
   │                              │
   │ (terminationGracePeriod)     │ (terminationGracePeriod)
   ▼                              ▼
Draining ──────────────────→ Terminating
```

### 3.5 ServiceInstance — 서비스+포트+엔드포인트 바인딩

```go
type ServiceInstance struct {
    Service     *Service        // 서비스 정의
    ServicePort *Port           // 서비스 포트
    Endpoint    *IstioEndpoint  // 네트워크 엔드포인트
}
```

**예시:**
```
Service: catalog.mystore.com
├─ Port: {Name: http, Port: 80, Protocol: HTTP}
└─ ServiceInstances:
   ├─ Endpoint{IP: 172.16.0.1, Port: 8888, Labels: {version: v1}}
   ├─ Endpoint{IP: 172.16.0.2, Port: 8888, Labels: {version: v1}}
   ├─ Endpoint{IP: 172.16.0.3, Port: 8888, Labels: {version: v2}}
   └─ Endpoint{IP: 172.16.0.4, Port: 8888, Labels: {version: v2}}
```

## 4. Proxy 모델

### 4.1 Proxy — xDS 클라이언트 표현

```go
// pilot/pkg/model/context.go
type Proxy struct {
    // 식별
    Type        NodeType          // SidecarProxy, Router, Waypoint, Ztunnel
    ID          string            // "pod-name.namespace"
    IPAddresses []string          // 프록시 IP

    // 설정 컨텍스트
    Locality        *core.Locality     // region/zone/subzone
    DNSDomain       string             // "default.svc.cluster.local"
    ConfigNamespace string             // 네임스페이스
    Labels          map[string]string  // 워크로드 레이블
    Metadata        *NodeMetadata      // 노드 메타데이터

    // 정책 & 범위
    SidecarScope  *SidecarScope    // 현재 사이드카 범위
    MergedGateway *MergedGateway   // 게이트웨이인 경우

    // 서비스 매핑
    ServiceTargets []ServiceTarget  // 이 프록시가 실행하는 서비스

    // 버전 & 인증
    IstioVersion    *IstioVersion     // 프록시 버전
    VerifiedIdentity *spiffe.Identity // 인증된 SPIFFE ID

    // 구독 상태
    WatchedResources map[string]*WatchedResource
    LastPushContext   *PushContext
}
```

### 4.2 NodeType — 프록시 유형

| 타입 | 설명 | 예시 |
|------|------|------|
| `SidecarProxy` | 애플리케이션 사이드카 | istio-proxy 컨테이너 |
| `Router` | 독립형 L7/L4 라우터 | Ingress/Egress Gateway |
| `Waypoint` | Ambient 메시 waypoint 프록시 | L7 정책 처리 |
| `Ztunnel` | Ambient 메시 노드 프록시 | L4 투명 프록시 |

### 4.3 NodeMetadata — 워크로드 메타데이터

```go
// pkg/model/proxy.go
type NodeMetadata struct {
    ProxyConfig      *NodeMetaProxyConfig  // 프록시별 설정
    IstioVersion     string                // 프록시 버전
    Labels           map[string]string     // 워크로드 레이블
    Namespace        string                // 네임스페이스
    ServiceAccount   string                // 서비스 어카운트
    ClusterID        cluster.ID            // 클러스터 ID
    Network          network.ID            // 네트워크 ID
    InterceptionMode TrafficInterceptionMode // REDIRECT 또는 TPROXY
    DNSCapture       StringBool            // DNS 캡처 여부
    EnableHBONE      StringBool            // HBONE 활성화 여부
}
```

## 5. PushContext 모델

### 5.1 PushContext — 불변 설정 스냅샷

PushContext는 xDS 푸시 시점의 **모든 설정을 사전 계산한 불변 스냅샷**이다.

```go
// pilot/pkg/model/push_context.go
type PushContext struct {
    // 서비스 인덱스
    ServiceIndex serviceIndex

    // 정책 인덱스
    virtualServiceIndex  virtualServiceIndex
    destinationRuleIndex destinationRuleIndex
    gatewayIndex         gatewayIndex
    sidecarIndex         sidecarIndex

    // 필터 & 플러그인
    envoyFiltersByNamespace map[string][]*EnvoyFilterWrapper
    wasmPluginsByNamespace  map[string][]*WasmPluginWrapper

    // 인증/인가 정책
    AuthnPolicies *AuthenticationPolicies
    AuthzPolicies *AuthorizationPolicies
    Telemetry     *Telemetries
    ProxyConfigs  *ProxyConfigs

    // 메시 설정
    Mesh     *meshconfig.MeshConfig
    Networks *meshconfig.MeshNetworks

    // 버전
    PushVersion string
}
```

**왜 불변 스냅샷인가?**
- 수천 개 프록시에 동시에 푸시할 때 설정 일관성 보장
- 하나의 PushContext가 모든 프록시의 설정 생성에 사용됨
- 새 설정 변경이 오면 새 PushContext를 생성

### 5.2 serviceIndex — 서비스 검색 인덱스

```go
type serviceIndex struct {
    privateByNamespace  map[string][]*Service       // exportTo "." (자기 네임스페이스만)
    public              []*Service                  // exportTo "*" (전체 공개)
    exportedToNamespace map[string][]*Service       // 특정 네임스페이스 export
    HostnameAndNamespace map[host.Name]map[string]*Service  // 호스트명+네임스페이스로 조회
}
```

## 6. Ambient 메시 전용 모델

### 6.1 Workload API (pkg/workloadapi/workload.proto)

ztunnel에 전달되는 커스텀 xDS 리소스:

```protobuf
message Address {
    oneof type {
        Workload workload = 1;    // Pod/VM 워크로드
        Service service = 2;      // K8s Service
    }
}

message Workload {
    string uid = 1;                        // 전역 고유 ID
    repeated bytes addresses = 2;          // IP 주소
    TunnelProtocol tunnel_protocol = 3;    // NONE, HBONE, LEGACY
    string trust_domain = 4;              // SPIFFE 트러스트 도메인
    string service_account = 5;           // 서비스 어카운트
    GatewayAddress waypoint = 12;         // L7 라우팅용 waypoint
    repeated string authorization_policies = 15;  // 적용할 인가 정책
}

message Service {
    string name = 1;
    string namespace = 2;
    string hostname = 3;
    repeated NetworkAddress addresses = 4;
    repeated Port ports = 5;
    GatewayAddress waypoint = 7;         // 선택적 waypoint
}
```

### 6.2 Authorization API (pkg/workloadapi/security/authorization.proto)

```protobuf
message Authorization {
    string name = 1;
    string namespace = 2;
    Scope scope = 3;              // NAMESPACE 또는 WORKLOAD_SELECTOR
    Action action = 4;            // ALLOW 또는 DENY
    repeated Group groups = 5;    // 규칙 그룹 (AND 연산)
    repeated Rule rules = 6;     // 규칙 목록 (OR 연산)
}
```

## 7. 데이터 흐름 요약

### 프록시 연결 시 데이터 흐름

```
1. Envoy 프록시 gRPC 연결 (NodeMetadata 포함)
   │
2. Istiod: Proxy 객체 생성 (NodeMetadata → Labels, SidecarScope)
   │
3. PushContext 조회 (현재 불변 스냅샷)
   │
4. 각 서비스에 대해:
   │  ├─ ServiceIndex에서 Service 조회
   │  ├─ ServiceInstance 매핑 (Service + Port + Endpoint)
   │  ├─ SidecarScope로 필터링 (네임스페이스, 레이블, 네트워크)
   │  └─ IstioEndpoint를 포트별 그룹핑 (EDS용)
   │
5. PushRequest 생성 (PushContext 포함)
   │
6. xDS 업데이트 전송 (CDS→EDS→LDS→RDS 순서)
```

### 설계의 핵심 원칙

| 원칙 | 구현 |
|------|------|
| **관심사 분리** | Service(무엇/어디), IstioEndpoint(어떻게/네트워크), Proxy(누구/컨텍스트) |
| **유연성** | 다양한 Resolution 모드, 멀티 어드레스, Dual Stack 지원 |
| **성능** | PushContext 인덱스 캐싱, instancesByPort 사전 계산 |
| **멀티클러스터** | Locality에 ClusterID 포함, Network 추적 |
| **불변성** | PushContext는 생성 후 수정 불가 → 동시성 안전 |
