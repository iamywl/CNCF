# 02. Cilium 데이터 모델

## 개요

Cilium의 데이터 모델은 크게 5가지 핵심 엔티티로 구성된다:
**Endpoint**, **Identity**, **Node**, **Service/Frontend/Backend**, **Policy**.
이들은 Go 구조체로 정의되며, BPF 맵과 K8s CRD를 통해 데이터플레인과 클러스터에 반영된다.

## 핵심 데이터 모델 관계도

```
+------------------+       1:1        +------------------+
|    Endpoint      |----------------->|    Identity      |
|  (pkg/endpoint/  |  SecurityIdentity|  (pkg/identity/  |
|   endpoint.go)   |                  |   identity.go)   |
+--------+---------+                  +--------+---------+
         |                                     |
         | N:1                                 | 1:N
         v                                     v
+------------------+                  +------------------+
|      Node        |                  |   IPIdentityPair |
| (pkg/node/types/ |                  |  (IP -> Identity |
|  node.go)        |                  |   매핑, IPCache) |
+------------------+                  +------------------+

+------------------+       1:N        +------------------+
|    Service       |----------------->|    Frontend      |
| (pkg/loadbalancer|                  | (pkg/loadbalancer|
|  /service.go)    |                  |  /frontend.go)   |
+--------+---------+                  +--------+---------+
         |                                     |
         | 1:N                                 | N:M
         v                                     v
+------------------+                  +------------------+
|    Backend       |                  |   L3n4Addr       |
| (pkg/loadbalancer|                  |  (IP:Port 주소)  |
|  /backend.go)    |                  +------------------+
+------------------+
```

## 1. Endpoint

**파일**: `pkg/endpoint/endpoint.go`

Endpoint는 Cilium이 관리하는 네트워크 엔드포인트(주로 Pod)를 나타낸다.
각 Endpoint는 고유 ID, IP 주소, 보안 Identity, 정책 맵을 가진다.

### Endpoint 구조체 핵심 필드

```go
// pkg/endpoint/endpoint.go:155
type Endpoint struct {
    // ID: 노드 스코프 내 고유 식별자 (uint16)
    ID uint16

    // 컨테이너 정보 (CNI에서 설정, 불변)
    containerName      atomic.Pointer[string]
    containerID        atomic.Pointer[string]
    containerIfName    string
    containerNetnsPath string

    // 네트워크 인터페이스
    ifName  string     // 호스트 측 veth 이름
    ifIndex int        // 호스트 측 veth 인덱스

    // IP 주소 (생성/복원 후 불변)
    IPv4         netip.Addr
    IPv6         netip.Addr
    IPv4IPAMPool string
    IPv6IPAMPool string

    // MAC 주소
    mac     mac.MAC   // 컨테이너 MAC
    nodeMAC mac.MAC   // 노드(에이전트) MAC

    // 보안 Identity (라벨 기반 계산)
    SecurityIdentity *identity.Identity

    // 라벨
    labels labels.OpLabels

    // 정책
    policyRepo     policy.PolicyRepository
    policyMap      *policymap.PolicyMap
    policyRevision uint64

    // 상태
    state State

    // Kubernetes 정보 (불변)
    K8sPodName   string
    K8sNamespace string
    K8sUID       string

    // DNS 관련
    DNSRules   restore.DNSRules
    DNSHistory *fqdn.DNSCache
    DNSZombies *fqdn.DNSZombieMappings

    // 동시성 제어
    mutex      lock.RWMutex   // 읽기/쓰기 보호
    buildMutex lock.Mutex     // 빌드 동기화 (regeneration)
}
```

### Endpoint 상태 머신

Endpoint는 8가지 상태를 가진다. 상태 전이는 엔드포인트 생명주기를 반영한다.

```go
// pkg/endpoint/endpoint.go:118-144
const (
    StateWaitingForIdentity = State(models.EndpointStateWaitingDashForDashIdentity)
    StateReady              = State(models.EndpointStateReady)
    StateWaitingToRegenerate = State(models.EndpointStateWaitingDashToDashRegenerate)
    StateRegenerating       = State(models.EndpointStateRegenerating)
    StateDisconnecting      = State(models.EndpointStateDisconnecting)
    StateDisconnected       = State(models.EndpointStateDisconnected)
    StateRestoring          = State(models.EndpointStateRestoring)
    StateInvalid            = State(models.EndpointStateInvalid)
)
```

**상태 전이 다이어그램**:

```
                         +-----------------------+
                         |    StateRestoring     |
                         | (에이전트 재시작 시    |
                         |  기존 EP 복원)        |
                         +----------+------------+
                                    |
                                    v
+-------------------+     +---------+-----------+
|  StateInvalid     |<----|  StateWaitingFor     |
|  (생성 실패)       |     |  Identity            |
+-------------------+     |  (Identity 할당 대기) |
                          +----------+----------+
                                     |
                                     v
+-------------------+     +----------+----------+     +-------------------+
| StateDisconnecting|<----|     StateReady       |---->| StateWaitingTo    |
| (삭제 진행 중)     |     |  (정상 운영 상태)    |     | Regenerate        |
+--------+----------+     +----------+----------+     | (재생성 대기)     |
         |                           ^                 +--------+----------+
         v                           |                          |
+--------+----------+     +----------+----------+              |
| StateDisconnected  |     | StateRegenerating   |<-------------+
| (완전 삭제)        |     | (BPF 프로그램 재생성)|
+-------------------+     +---------------------+
```

### Endpoint 프로퍼티 상수

```go
// pkg/endpoint/endpoint.go:74-105
const (
    PropertyFakeEndpoint          // 테스트용 가짜 엔드포인트
    PropertyAtHostNS              // 호스트 네임스페이스에서 접근
    PropertyWithouteBPFDatapath   // eBPF 데이터패스 없음
    PropertySkipBPFPolicy         // BPF 정책 재생성 스킵
    PropertySkipBPFRegeneration   // BPF 재생성 전체 스킵
    PropertyCEPOwner              // CiliumEndpoint 소유자
    PropertyCEPName               // CiliumEndpoint 이름
    PropertySkipMasqueradeV4      // IPv4 마스커레이드 스킵
    PropertySkipMasqueradeV6      // IPv6 마스커레이드 스킵
)
```

## 2. Identity (보안 Identity)

**파일**: `pkg/identity/identity.go`, `pkg/identity/numericidentity.go`

Identity는 라벨 집합에 대한 보안 컨텍스트를 나타낸다. 동일한 라벨을 가진 모든 엔드포인트는
같은 Identity를 공유한다. Identity는 숫자 ID(NumericIdentity)로 BPF 맵에서 참조된다.

### Identity 구조체

```go
// pkg/identity/identity.go:27-40
type Identity struct {
    // 숫자 Identity ID
    ID NumericIdentity `json:"id"`

    // 이 Identity에 속하는 라벨 집합
    Labels labels.Labels `json:"labels"`

    // 빠른 조회를 위한 라벨 배열 형태
    LabelArray labels.LabelArray `json:"-"`

    // 이 Identity를 참조하는 엔드포인트 수
    ReferenceCount int `json:"-"`
}
```

### NumericIdentity 스코프

Identity ID는 32비트이며, 상위 8비트가 스코프를 결정한다:

```go
// pkg/identity/numericidentity.go:22-44
const (
    // 0x00: 글로벌 및 예약된 Identity
    IdentityScopeGlobal = NumericIdentity(0)

    // 0x01: 로컬 (CIDR) Identity
    IdentityScopeLocal = NumericIdentity(1 << 24)

    // 0x02: 원격 노드 Identity
    IdentityScopeRemoteNode = NumericIdentity(2 << 24)

    // 스코프 마스크 (상위 8비트)
    IdentityScopeMask = NumericIdentity(0xFF_00_00_00)
)
```

| 스코프 | 비트 패턴 | 범위 | 설명 |
|--------|-----------|------|------|
| Global | `0x00______` | 1~255(예약), 256~16777215(할당) | 클러스터 전체 공유 |
| Local | `0x01______` | `0x01000001`~`0x01FFFFFF` | 노드 로컬 CIDR Identity |
| RemoteNode | `0x02______` | `0x02000001`~`0x02FFFFFF` | 원격 노드 Identity |

### 예약된 Identity

```go
// pkg/identity/numericidentity.go:99-142
const (
    IdentityUnknown              NumericIdentity = 0   // 알 수 없음
    ReservedIdentityHost         NumericIdentity = 1   // 로컬 호스트
    ReservedIdentityWorld        NumericIdentity = 2   // 클러스터 외부
    ReservedIdentityUnmanaged    NumericIdentity = 3   // 비관리 엔드포인트
    ReservedIdentityHealth       NumericIdentity = 4   // cilium-health
    ReservedIdentityInit         NumericIdentity = 5   // 라벨 미할당 초기 상태
    ReservedIdentityRemoteNode   NumericIdentity = 6   // 원격 노드
    ReservedIdentityKubeAPIServer NumericIdentity = 7  // kube-apiserver
    ReservedIdentityIngress      NumericIdentity = 8   // Ingress 프록시
    ReservedIdentityWorldIPv4    NumericIdentity = 9   // 외부 IPv4
    ReservedIdentityWorldIPv6    NumericIdentity = 10  // 외부 IPv6
    ReservedEncryptedOverlay     NumericIdentity = 11  // 암호화 오버레이
)
```

### Identity 타입 분류

```go
// pkg/identity/identity.go:17-23
const (
    NodeLocalIdentityType    = "node_local"      // 노드 로컬
    ReservedIdentityType     = "reserved"         // 예약됨
    ClusterLocalIdentityType = "cluster_local"    // 클러스터 로컬
    WellKnownIdentityType    = "well_known"       // 잘 알려진 컴포넌트
    RemoteNodeIdentityType   = "remote_node"      // 원격 노드
)
```

### IPIdentityPair

IP 주소와 Identity를 매핑하는 구조체로, IPCache에 저장되고 KVStore에 동기화된다.

```go
// pkg/identity/identity.go:49-60
type IPIdentityPair struct {
    IP                net.IP          `json:"IP"`
    Mask              net.IPMask      `json:"Mask"`
    HostIP            net.IP          `json:"HostIP"`
    ID                NumericIdentity `json:"ID"`
    Key               uint8           `json:"Key"`
    Metadata          string          `json:"Metadata"`
    K8sNamespace      string          `json:"K8sNamespace,omitempty"`
    K8sPodName        string          `json:"K8sPodName,omitempty"`
    K8sServiceAccount string          `json:"K8sServiceAccount,omitempty"`
    NamedPorts        []NamedPort     `json:"NamedPorts,omitempty"`
}
```

## 3. Node

**파일**: `pkg/node/types/node.go`

Node는 클러스터의 노드 정보를 나타낸다. 각 노드의 IP, Pod CIDR, 암호화 키 등을 포함.

### Node 구조체

```go
// pkg/node/types/node.go:181-245
type Node struct {
    // 노드 이름 (일반적으로 호스트명)
    Name string

    // 클러스터 이름
    Cluster string

    // 노드 IP 주소 목록 (InternalIP, ExternalIP 등)
    IPAddresses []Address

    // Pod CIDR 할당 (노드에 할당된 Pod IP 범위)
    IPv4AllocCIDR           *cidr.CIDR
    IPv4SecondaryAllocCIDRs []*cidr.CIDR
    IPv6AllocCIDR           *cidr.CIDR
    IPv6SecondaryAllocCIDRs []*cidr.CIDR

    // cilium-health 엔드포인트 IP
    IPv4HealthIP net.IP
    IPv6HealthIP net.IP

    // Ingress 리스너 IP
    IPv4IngressIP net.IP
    IPv6IngressIP net.IP

    // 클러스터 ID (멀티클러스터)
    ClusterID uint32

    // 데이터 소스
    Source source.Source

    // 투명 암호화 키 인덱스
    EncryptionKey uint8

    // 노드 라벨 및 어노테이션
    Labels      map[string]string
    Annotations map[string]string

    // 노드 Identity (숫자)
    NodeIdentity uint32

    // WireGuard 공개 키
    WireguardPubKey string

    // 부팅 ID (고유 노드 식별자)
    BootID string
}
```

### Node Identity

```go
// pkg/node/types/node.go:31-34
type Identity struct {
    Name    string
    Cluster string
}
```

노드 Identity는 `Cluster/Name` 형태의 경로로 표현된다. 멀티클러스터 환경에서
노드를 고유하게 식별하는 데 사용된다.

### Address 타입

```go
// pkg/node/types/node.go:259-263
type Address struct {
    Type addressing.AddressType  // InternalIP, ExternalIP 등
    IP   net.IP
}
```

## 4. Load Balancer 데이터 모델

Cilium의 로드밸런싱은 **Service**, **Frontend**, **Backend** 3계층 모델로 구성된다.

### Service

**파일**: `pkg/loadbalancer/service.go`

```go
// pkg/loadbalancer/service.go:30-100
type Service struct {
    // 서비스 전체 이름: (<cluster>/)<namespace>/<name>
    Name ServiceName

    // 데이터 소스
    Source source.Source

    // 서비스 라벨
    Labels labels.Labels

    // 어노테이션
    Annotations map[string]string

    // Pod 셀렉터 (빈 경우 백엔드가 외부에서 관리됨)
    Selector map[string]string

    // NAT 정책 (NAT46/64)
    NatPolicy SVCNatPolicy

    // 트래픽 정책: North-South (External)
    ExtTrafficPolicy SVCTrafficPolicy

    // 트래픽 정책: East-West (Internal)
    IntTrafficPolicy SVCTrafficPolicy

    // 포워딩 모드: DSR 또는 SNAT
    ForwardingMode SVCForwardingMode

    // 세션 어피니티
    SessionAffinity        bool
    SessionAffinityTimeout time.Duration

    // 헬스 체크 노드 포트
    HealthCheckNodePort uint16

    // 소스 IP 범위 제한
    SourceRanges []netip.Prefix

    // 포트 이름 -> 포트 번호 매핑
    PortNames map[string]uint16

    // 트래픽 분배 설정
    TrafficDistribution TrafficDistribution
}
```

### Frontend

**파일**: `pkg/loadbalancer/frontend.go`

```go
// pkg/loadbalancer/frontend.go:29-48
type FrontendParams struct {
    // 프론트엔드 주소와 포트 (L3n4Addr)
    Address L3n4Addr

    // 서비스 타입 (ClusterIP, NodePort, LoadBalancer 등)
    Type SVCType

    // 연관 서비스 이름
    ServiceName ServiceName

    // 포트 이름 (백엔드 필터링에 사용)
    PortName FEPortName

    // 서비스 포트 (ClusterIP의 포트 번호)
    ServicePort uint16
}

// pkg/loadbalancer/frontend.go:50-81
type Frontend struct {
    FrontendParams

    // 조정(reconciliation) 상태
    Status reconciler.Status

    // 연관 백엔드 이터레이터
    Backends BackendsSeq2

    // 헬스 체크 대상 백엔드
    HealthCheckBackends BackendsSeq2

    // BPF 서비스 맵 키로 사용되는 ID
    ID ServiceID

    // 리다이렉트 대상 (Local Redirect Policy)
    RedirectTo *ServiceName

    // 연관 서비스 (포인터, 업데이트 시 자동 반영)
    Service *Service
}
```

### Backend

**파일**: `pkg/loadbalancer/backend.go`

```go
// pkg/loadbalancer/backend.go:109-121
type Backend struct {
    // 백엔드 주소 (IP:Port)
    Address L3n4Addr

    // 백엔드 인스턴스 맵
    // 키: (ServiceName, SourcePriority)
    // 같은 백엔드가 여러 서비스에서 다른 이름으로 참조될 수 있음
    Instances part.Map[BackendInstanceKey, BackendParams]
}
```

### 데이터 모델 관계 요약

| 관계 | 설명 |
|------|------|
| Service 1:N Frontend | 하나의 서비스에 여러 프론트엔드 (ClusterIP, NodePort, LoadBalancer 등) |
| Frontend N:M Backend | 프론트엔드별로 다른 백엔드 서브셋 가능 (PortName 필터, TrafficPolicy) |
| Backend N:M Service | 하나의 백엔드(Pod)가 여러 서비스에 속할 수 있음 |
| Endpoint 1:1 Identity | 각 엔드포인트는 정확히 하나의 보안 Identity를 가짐 |
| Identity 1:N Endpoint | 동일 라벨의 엔드포인트들은 같은 Identity 공유 |
| Node 1:N Endpoint | 하나의 노드에 여러 엔드포인트가 존재 |
| IPIdentityPair N:1 Identity | 여러 IP가 동일 Identity에 매핑 가능 |

## 5. Policy 데이터 모델

### PolicyRepository

**파일**: `pkg/policy/repository.go`

```go
// pkg/policy/repository.go:63-98
type Repository struct {
    // 전체 정책 트리 보호
    mutex lock.RWMutex

    // 정책 규칙 저장소
    rules            map[ruleKey]*rule
    rulesByNamespace map[string]sets.Set[ruleKey]
    rulesByResource  map[ipcachetypes.ResourceID]map[ruleKey]*rule

    // 정책 리비전 (변경 시 증가, 항상 >0)
    revision atomic.Uint64

    // 정책에서 사용되는 셀렉터 캐시
    selectorCache *SelectorCache

    // 정책 적용 대상 셀렉터 캐시
    subjectSelectorCache *SelectorCache

    // 계산된 SelectorPolicy 캐시
    policyCache *policyCache
}
```

### PolicyRepository 인터페이스

```go
// pkg/policy/repository.go:34-56
type PolicyRepository interface {
    BumpRevision() uint64
    GetAuthTypes(localID, remoteID identity.NumericIdentity) AuthTypes
    GetSelectorPolicy(id *identity.Identity, skipRevision uint64, ...) (SelectorPolicy, uint64, error)
    GetPolicySnapshot() map[identity.NumericIdentity]SelectorPolicy
    GetRevision() uint64
    GetRulesList() *models.Policy
    GetSelectorCache() *SelectorCache
    Iterate(f func(rule *types.PolicyEntry))
    ReplaceByResource(rules types.PolicyEntries, resource ipcachetypes.ResourceID) (...)
    Search() (types.PolicyEntries, uint64)
}
```

## 6. API 모델

**디렉토리**: `api/v1/models/`

go-swagger로 자동 생성되는 API 모델로, REST API 요청/응답에 사용된다.
Endpoint, Identity, Service 등의 외부 표현(external representation)을 정의한다.

주요 모델:
- `models.Endpoint`: API용 엔드포인트 표현
- `models.Identity`: API용 Identity 표현
- `models.Service`: API용 서비스 표현
- `models.IPAMResponse`: IP 할당 응답
- `models.DaemonConfigurationStatus`: 데몬 설정 상태
- `models.EndpointState*`: 엔드포인트 상태 상수

## BPF 맵과의 매핑

Go 데이터 모델은 BPF 맵의 키/값으로 변환되어 데이터플레인에서 사용된다:

| Go 데이터 모델 | BPF 맵 | 용도 |
|---------------|--------|------|
| Endpoint (ID, Identity) | `cilium_lxc` (lxcmap) | 엔드포인트 조회 |
| IPIdentityPair | `cilium_ipcache` | IP → Identity 매핑 |
| Identity (정책) | `cilium_policy_*` (policymap) | 정책 허용/거부 결정 |
| Frontend/Backend | `cilium_lb4_services_v2` 등 | 서비스 로드밸런싱 |
| ConnTrack entry | `cilium_ct4_global` | 연결 추적 |
| NAT entry | `cilium_snat_v4_external` | NAT 매핑 |

이 매핑 구조 덕분에 컨트롤플레인의 Go 데이터가 변경되면 BPF 맵을 업데이트하는 것만으로
데이터플레인 동작이 즉시 반영된다. BPF 프로그램 재컴파일 없이 런타임에 정책/서비스 변경이 가능한
핵심 메커니즘이다.
