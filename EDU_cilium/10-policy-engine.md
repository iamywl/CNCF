# Cilium 정책 엔진 (Policy Engine) 심층 분석

## 1. 개요

Cilium의 정책 엔진은 Kubernetes 클러스터에서 네트워크 보안 정책을 정의, 평가, 적용하는 핵심 서브시스템이다. 전통적인 IP 기반 방화벽과 달리, Cilium은 **Identity(아이덴티티) 기반** 정책 모델을 사용하여 L3/L4/L7 계층에서 세밀한 접근 제어를 수행한다.

핵심 설계 원칙:
- **Identity 기반 정책**: IP 대신 라벨 기반 보안 아이덴티티로 워크로드를 식별
- **BPF 데이터경로 적용**: 사용자 공간에서 계산된 정책이 BPF 맵을 통해 커널 데이터경로에 직접 적용
- **Default Deny**: 정책이 적용된 엔드포인트는 명시적 허용 규칙이 없는 한 모든 트래픽 차단
- **계층적 정책 평가**: L3 → L4 → L7 순서로 점진적 필터링

## 2. 정책 계층 (Policy Layers)

### 2.1 L3 정책: Identity 기반과 CIDR 기반

L3 정책은 트래픽의 **출발지/목적지**를 식별하는 가장 기본적인 계층이다.

#### Identity 기반 정책

Cilium은 각 워크로드에 보안 아이덴티티(Numeric Identity)를 할당한다. 이 아이덴티티는 워크로드의 라벨(labels) 조합에서 파생되며, 동일 라벨 집합을 가진 모든 Pod는 동일한 아이덴티티를 공유한다.

```yaml
apiVersion: "cilium.io/v2"
kind: CiliumNetworkPolicy
metadata:
  name: "l3-identity-policy"
spec:
  endpointSelector:
    matchLabels:
      app: backend
  ingress:
    - fromEndpoints:
        - matchLabels:
            app: frontend
```

Identity는 `pkg/identity/` 패키지에서 관리되며, `identity.NumericIdentity` 타입(uint32)으로 표현된다. 특수 예약 아이덴티티도 존재한다:
- `ReservedIdentityHost` (1): 로컬 호스트
- `ReservedIdentityWorld` (2): 클러스터 외부 모든 트래픽
- `ReservedIdentityInit` (5): 라벨이 아직 할당되지 않은 엔드포인트

관련 코드:
- `pkg/identity/numeric_identity.go`: `NumericIdentity` 타입 정의
- `pkg/policy/types/selector.go`: `LabelSelector`, `CIDRSelector`, `FQDNSelector` 구현

#### CIDR 기반 정책

클러스터 외부 IP 주소 범위에 대한 접근을 제어할 때 사용한다.

```yaml
spec:
  egress:
    - toCIDR:
        - 10.0.0.0/8
    - toCIDRSet:
        - cidr: 192.168.0.0/16
          except:
            - 192.168.1.0/24
```

CIDR 정책은 `pkg/policy/api/cidr.go`에서 정의되며, `CIDRSelector`(`pkg/policy/types/selector.go`)를 통해 CIDR 접두사를 라벨로 변환하여 Identity 시스템과 통합된다. 예를 들어, `10.0.0.0/8`은 `cidr:10.0.0.0/8`이라는 라벨로 변환되어 해당 IP 범위의 Identity를 선택한다.

### 2.2 L4 정책: Port/Protocol

L4 정책은 전송 프로토콜(TCP, UDP, SCTP)과 포트 번호를 기준으로 트래픽을 필터링한다.

```yaml
spec:
  ingress:
    - fromEndpoints:
        - matchLabels:
            app: frontend
      toPorts:
        - ports:
            - port: "80"
              protocol: TCP
            - port: "443"
              protocol: TCP
```

L4 정책은 `pkg/policy/api/l4.go`에 정의된 `PortProtocol` 구조체로 표현된다:

```go
// pkg/policy/api/l4.go
type PortProtocol struct {
    Port     string  `json:"port,omitempty"`
    EndPort  int32   `json:"endPort,omitempty"`
    Protocol L4Proto `json:"protocol,omitempty"`
}
```

지원 프로토콜: `TCP`, `UDP`, `SCTP`, `ICMP`, `ICMPv6`, `VRRP`, `IGMP`, `ANY`

L4 필터링은 `pkg/policy/l4.go`의 `L4Filter` 구조체에서 수행된다. 각 `L4Filter`는 특정 포트/프로토콜 조합에 매칭되는 `PerSelectorPolicy` 맵을 보유하며, 이를 통해 동일한 포트에 대해 다른 Identity에 다른 L7 정책을 적용할 수 있다.

### 2.3 L7 정책: HTTP/gRPC/DNS/Kafka

L7 정책은 애플리케이션 프로토콜 수준에서 세밀한 접근 제어를 제공한다. L7 정책이 활성화되면 해당 트래픽은 **프록시(Envoy 또는 DNS 프록시)**를 통과하게 된다.

#### HTTP 정책

```yaml
spec:
  ingress:
    - fromEndpoints:
        - matchLabels:
            app: frontend
      toPorts:
        - ports:
            - port: "80"
              protocol: TCP
          rules:
            http:
              - method: "GET"
                path: "/api/v1/.*"
              - method: "POST"
                path: "/api/v1/submit"
```

HTTP 규칙은 `pkg/policy/api/http.go`의 `PortRuleHTTP`로 정의된다:

```go
// pkg/policy/api/http.go
type PortRuleHTTP struct {
    Path         string         `json:"path,omitempty"`
    Method       string         `json:"method,omitempty"`
    Host         string         `json:"host,omitempty"`
    Headers      []string       `json:"headers,omitempty"`
    HeaderMatches []*HeaderMatch `json:"headerMatches,omitempty"`
}
```

`Path`와 `Method` 필드는 POSIX 확장 정규표현식(Extended POSIX regex)을 지원한다.

#### DNS 정책

```yaml
spec:
  egress:
    - toFQDNs:
        - matchName: "api.example.com"
        - matchPattern: "*.internal.corp"
      toPorts:
        - ports:
            - port: "443"
              protocol: TCP
```

DNS 정책은 `pkg/policy/api/fqdn.go`의 `FQDNSelector`로 정의된다. FQDN 정책의 동작 원리는 [섹션 8](#8-fqdn-정책의-dns-프록시-연동)에서 상세히 다룬다.

#### Kafka 정책 (Deprecated)

```yaml
rules:
  kafka:
    - apiKey: "produce"
      topic: "my-topic"
```

#### 일반 L7 프로토콜 (L7Proto)

Envoy의 커스텀 필터를 통해 임의의 L7 프로토콜을 지원할 수 있다:

```go
// pkg/policy/api/l4.go
type L7Rules struct {
    HTTP    PortRulesHTTP    `json:"http,omitempty"`
    Kafka   []kafka.PortRule `json:"kafka,omitempty"`
    DNS     PortRulesDNS     `json:"dns,omitempty"`
    L7Proto string           `json:"l7proto,omitempty"`
    L7      PortRulesL7      `json:"l7,omitempty"`
}
```

## 3. 정책 유형 (Policy Types)

### 3.1 FQDN 정책

FQDN(Fully Qualified Domain Name) 정책은 DNS 이름 기반으로 egress 트래픽을 제어한다. 일반적인 CIDR 정책과 달리, FQDN 정책은 DNS 응답을 실시간으로 추적하여 도메인 이름과 IP 주소의 매핑을 자동으로 관리한다.

동작 흐름:
1. `toFQDNs` 규칙이 정책에 추가됨
2. Cilium의 DNS 프록시가 해당 도메인에 대한 DNS 쿼리를 가로채서 응답을 관찰
3. DNS 응답에서 IP 주소를 추출하여 `DNSCache`에 저장
4. 해당 IP 주소에 FQDN 라벨 기반 Identity를 할당
5. Identity에 기반한 BPF 정책 맵 업데이트

관련 코드:
- `pkg/fqdn/cache.go`: `DNSCache` - DNS 이름↔IP 매핑 캐시
- `pkg/fqdn/dnsproxy/`: DNS 프록시 구현
- `pkg/policy/api/fqdn.go`: `FQDNSelector`, `PortRuleDNS` 정의

### 3.2 CIDR Group

CIDR Group은 여러 CIDR 블록을 참조 가능한 별도의 CRD(`CiliumCIDRGroup`)로 관리하여 정책에서 재사용할 수 있게 한다.

```yaml
apiVersion: "cilium.io/v2alpha1"
kind: CiliumCIDRGroup
metadata:
  name: external-apis
spec:
  externalCIDRs:
    - 203.0.113.0/24
    - 198.51.100.0/24
```

정책에서 참조:
```yaml
spec:
  egress:
    - toCIDRSet:
        - cidrGroupRef: external-apis
```

관련 코드:
- `pkg/k8s/apis/cilium.io/v2/cidrgroups_types.go`: CiliumCIDRGroup CRD 정의
- `pkg/policy/types/selector.go`의 `newCIDRRuleSelector`: CIDRGroupRef 처리

### 3.3 Egress Gateway

Egress Gateway 정책은 특정 egress 트래픽을 지정된 게이트웨이 노드를 통해 라우팅하여, 클러스터 외부에서 보이는 소스 IP를 고정시킨다.

관련 코드:
- `pkg/k8s/apis/cilium.io/v2/cegp_types.go`: CiliumEgressGatewayPolicy CRD 정의

### 3.4 Local Redirect Policy (LRP)

Local Redirect Policy는 특정 서비스 트래픽을 동일 노드의 로컬 백엔드 Pod로 리다이렉트한다. 이를 통해 노드 로컬 DNS 캐시 등의 패턴을 구현할 수 있다.

```yaml
apiVersion: "cilium.io/v2"
kind: CiliumLocalRedirectPolicy
metadata:
  name: local-dns-redirect
spec:
  redirectFrontend:
    serviceMatcher:
      serviceName: kube-dns
      namespace: kube-system
  redirectBackend:
    localEndpointSelector:
      matchLabels:
        app: node-local-dns
    toPorts:
      - port: "53"
        protocol: UDP
```

관련 코드:
- `pkg/k8s/apis/cilium.io/v2/clrp_types.go`: CiliumLocalRedirectPolicy CRD 정의

## 4. CiliumNetworkPolicy (CNP) / CiliumClusterwideNetworkPolicy (CCNP) CRD 구조

### 4.1 CiliumNetworkPolicy (CNP)

CNP는 네임스페이스 범위의 정책 리소스이다.

```go
// pkg/k8s/apis/cilium.io/v2/cnp_types.go
type CiliumNetworkPolicy struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata"`
    Spec   *api.Rule              `json:"spec,omitempty"`
    Specs  api.Rules              `json:"specs,omitempty"`
    Status CiliumNetworkPolicyStatus `json:"status,omitempty"`
}
```

`Spec`과 `Specs` 필드의 차이:
- `Spec`: 단일 정책 규칙
- `Specs`: 여러 정책 규칙의 리스트 (하나의 CNP에 여러 규칙을 포함할 때 사용)

### 4.2 CiliumClusterwideNetworkPolicy (CCNP)

CCNP는 클러스터 전체 범위의 정책이다. CNP와 동일한 구조를 가지지만, 클러스터 스코프이므로 `nodeSelector`를 통해 노드 수준 정책도 정의할 수 있다.

```go
// pkg/k8s/apis/cilium.io/v2/ccnp_types.go
type CiliumClusterwideNetworkPolicy struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata"`
    Spec   *api.Rule              `json:"spec,omitempty"`
    Specs  api.Rules              `json:"specs,omitempty"`
    Status CiliumNetworkPolicyStatus `json:"status,omitempty"`
}
```

### 4.3 api.Rule 구조

두 CRD 모두 핵심 규칙 구조인 `api.Rule`을 참조한다:

```go
// pkg/policy/api/rule.go
type Rule struct {
    EndpointSelector EndpointSelector `json:"endpointSelector,omitzero"`
    NodeSelector     EndpointSelector `json:"nodeSelector,omitzero"`
    Ingress          []IngressRule    `json:"ingress,omitempty"`
    IngressDeny      []IngressDenyRule `json:"ingressDeny,omitempty"`
    Egress           []EgressRule     `json:"egress,omitempty"`
    EgressDeny       []EgressDenyRule `json:"egressDeny,omitempty"`
    Labels           labels.LabelArray `json:"labels,omitempty"`
    EnableDefaultDeny DefaultDenyConfig `json:"enableDefaultDeny,omitzero"`
    Description      string           `json:"description,omitempty"`
}
```

- `EndpointSelector` / `NodeSelector`: 규칙이 적용될 **대상**(Subject) 선택
- `Ingress` / `Egress`: 허용 규칙
- `IngressDeny` / `EgressDeny`: 거부 규칙 (항상 허용 규칙보다 우선)
- `EnableDefaultDeny`: 명시적 default deny 설정

## 5. 정책 평가 흐름: CNP -> PolicyRepository -> Distillery -> BPF PolicyMap

### 5.1 전체 흐름 개요

```
Kubernetes API Server
        |
        v
  [K8s Watcher] -- CNP/CCNP 감지
        |
        v
  [PolicyRepository.ReplaceByResource()] -- 규칙을 Repository에 저장
        |
        v
  [PolicyRepository.GetSelectorPolicy()] -- Identity별 SelectorPolicy 계산
        |
        v
  [selectorPolicy.DistillPolicy()] -- SelectorPolicy를 EndpointPolicy로 증류
        |
        v
  [EndpointPolicy.policyMapState] -- (Key, MapStateEntry) 쌍의 맵
        |
        v
  [policymap.PolicyMap] -- BPF LPM Trie 맵에 쓰기
        |
        v
  [BPF Datapath] -- 커널에서 패킷별 정책 판정
```

### 5.2 단계별 상세 설명

#### 단계 1: CNP 수신 및 Repository 저장

K8s Watcher가 CNP/CCNP 리소스 변경을 감지하면, 규칙을 `types.PolicyEntry` 형태로 변환하여 `Repository.ReplaceByResource()`를 호출한다.

```go
// pkg/policy/repository.go
func (p *Repository) ReplaceByResource(
    rules types.PolicyEntries,
    resource ipcachetypes.ResourceID,
) (affectedIDs *set.Set[identity.NumericIdentity], rev uint64, oldRuleCnt int) {
    // 1. 기존 리소스의 규칙 삭제
    // 2. 새 규칙 삽입
    // 3. 영향 받는 Identity 목록 수집
    // 4. Revision 증가
}
```

`PolicyEntry`의 구조:

```go
// pkg/policy/types/policyentry.go
type PolicyEntry struct {
    Tier         Tier
    Priority     float64
    Authentication *api.Authentication
    Subject      *LabelSelector       // 어떤 엔드포인트에 적용되는가
    L3           Selectors             // 어떤 피어를 허용/차단하는가
    L4           api.PortRules         // 어떤 포트/프로토콜인가
    Labels       labels.LabelArray
    DefaultDeny  bool
    Verdict      Verdict               // Allow, Deny, Pass
    Ingress      bool
    Node         bool
}
```

#### 단계 2: SelectorPolicy 계산

특정 Identity의 정책을 계산하기 위해 `GetSelectorPolicy()`가 호출된다. 이 함수는 내부적으로 `policyCache`를 통해 캐싱을 수행한다:

```go
// pkg/policy/repository.go - resolvePolicyLocked()
func (p *Repository) resolvePolicyLocked(securityIdentity *identity.Identity) (*selectorPolicy, error) {
    // 1. computePolicyEnforcementAndRules(): 해당 Identity에 매칭되는 규칙 수집
    // 2. 수집된 Ingress/Egress 규칙을 우선순위로 정렬
    // 3. resolveL4Policy(): L4/L7 필터 계산
    // 4. Attach(): SelectorCache에 변경사항 커밋
}
```

`computePolicyEnforcementAndRules()`는 중요한 함수로, Identity의 라벨과 매칭되는 모든 규칙을 수집하고 정책 강제 여부를 결정한다:

```go
// pkg/policy/repository.go
func (p *Repository) computePolicyEnforcementAndRules(securityIdentity *identity.Identity) (
    hasIngress, hasEgress,
    hasIngressDefaultDeny, hasEgressDefaultDeny bool,
    rulesIngress, rulesEgress ruleSlice,
) {
    // 1. 클러스터 전역 규칙 매칭 (namespace == "")
    // 2. 네임스페이스별 규칙 매칭
    // 3. 우선순위 정렬
    // 4. Default Deny 여부 결정
}
```

#### 단계 3: Policy Distillation (증류)

`selectorPolicy.DistillPolicy()`는 셀렉터 기반 정책을 구체적인 Identity+Port 쌍의 맵 상태로 변환한다:

```go
// pkg/policy/resolve.go
func (p *selectorPolicy) DistillPolicy(
    logger *slog.Logger,
    policyOwner PolicyOwner,
    redirects map[string]uint16,
) *EndpointPolicy {
    // 1. SelectorCache 읽기 잠금 획득
    // 2. SelectorSnapshot 생성 (현재 Identity 매핑 스냅샷)
    // 3. EndpointPolicy 구조체 생성
    // 4. L4Policy를 MapState로 변환 (toMapState)
    // 5. localhost 허용 규칙 적용
}
```

`EndpointPolicy`는 실제 데이터경로에 적용될 정책의 최종 형태이다:

```go
// pkg/policy/resolve.go
type EndpointPolicy struct {
    SelectorPolicy *selectorPolicy
    selectors      SelectorSnapshot
    policyMapState mapState          // Key -> MapStateEntry 매핑
    policyMapChanges MapChanges      // 점진적 변경사항
    PolicyOwner    PolicyOwner
    Redirects      map[string]uint16 // 프록시 리다이렉트 포트
}
```

#### 단계 4: BPF PolicyMap 업데이트

`policyMapState`의 각 항목은 BPF 맵에 기록된다. 키는 `(Identity, Direction, Protocol, Port)` 튜플이고, 값은 `(ProxyPort, Deny Flag, Precedence, AuthType)` 등의 정보를 포함한다.

```go
// pkg/maps/policymap/policymap.go
type PolicyKey struct {
    Prefixlen        uint32 `align:"lpm_key"`
    Identity         uint32 `align:"sec_label"`
    TrafficDirection uint8  `align:"egress"`
    Nexthdr          uint8  `align:"protocol"`
    DestPortNetwork  uint16 `align:"dport"`
}

type PolicyEntry struct {
    ProxyPortNetwork uint16                      `align:"proxy_port"`
    Flags            policyEntryFlags            `align:"deny"`
    AuthRequirement  policyTypes.AuthRequirement `align:"auth_type"`
    Precedence       policyTypes.Precedence      `align:"precedence"`
    Cookie           uint32                      `align:"cookie"`
}
```

BPF 맵은 **LPM Trie**(Longest Prefix Match) 구조를 사용하며, 프로토콜과 포트의 와일드카드 매칭을 효율적으로 수행한다.

BPF 데이터경로(`bpf/lib/policy.h`)에서의 정책 매칭 순서:
1. `POLICY_MATCH_L3_L4`: Identity + Protocol + Port 완전 매칭
2. `POLICY_MATCH_L3_PROTO`: Identity + Protocol (포트 와일드카드)
3. `POLICY_MATCH_L3_ONLY`: Identity만 매칭 (L4 와일드카드)
4. `POLICY_MATCH_L4_ONLY`: Protocol + Port만 매칭 (Identity 와일드카드)
5. `POLICY_MATCH_PROTO_ONLY`: Protocol만 매칭
6. `POLICY_MATCH_ALL`: 전체 와일드카드

## 6. pkg/policy/ 패키지 구조

```
pkg/policy/
├── api/                         # 정책 API 타입 정의
│   ├── rule.go                  # api.Rule - 최상위 규칙 구조체
│   ├── ingress.go               # IngressRule, IngressDenyRule
│   ├── egress.go                # EgressRule, EgressDenyRule
│   ├── l4.go                    # PortProtocol, PortRule, L7Rules
│   ├── l7.go                    # PortRuleL7 (key-value L7 규칙)
│   ├── http.go                  # PortRuleHTTP (HTTP L7 규칙)
│   ├── fqdn.go                  # FQDNSelector, PortRuleDNS
│   ├── cidr.go                  # CIDR, CIDRRule, CIDRRuleSlice
│   ├── selector.go              # EndpointSelector
│   ├── entity.go                # Entity (world, host, cluster 등)
│   ├── decision.go              # 정책 판정 로직 유틸리티
│   ├── groups.go                # AWS Security Group 등 외부 그룹 연동
│   └── service.go               # Service selector
│
├── types/                       # 핵심 정책 타입
│   ├── types.go                 # Key, LPMKey - BPF 맵 키 구조체
│   ├── entry.go                 # MapStateEntry, Precedence, Priority
│   ├── policyentry.go           # PolicyEntry, Tier(Admin/Normal/Baseline), Verdict
│   ├── selector.go              # Selector 인터페이스, LabelSelector, FQDNSelector, CIDRSelector
│   ├── auth.go                  # AuthType, AuthRequirement
│   └── update.go                # PolicyUpdate 관련 타입
│
├── repository.go                # PolicyRepository - 정책 저장소 (규칙 추가/삭제/검색)
├── distillery.go                # policyCache - 정책 캐시 및 SelectorPolicy 관리
├── resolve.go                   # selectorPolicy, EndpointPolicy - 정책 증류/해석
├── rule.go                      # rule - 내부 규칙 표현, PerSelectorPolicy 병합
├── rules.go                     # ruleSlice - 규칙 정렬 및 resolveL4Policy
├── l4.go                        # L4Filter, L4Policy, L4DirectionPolicy, L4PolicyMap
├── mapstate.go                  # mapState - policyMapState 인덱싱 구조
├── selectorcache.go             # SelectorCache - Identity ↔ Selector 매핑 캐시
├── selectorcache_selector.go    # SelectorCache의 개별 셀렉터 관리
├── trigger.go                   # 정책 변경 트리거
├── proxyid.go                   # ProxyID 생성
├── portrange.go                 # 포트 범위 처리
├── cidr.go                      # CIDR 유틸리티
├── config.go                    # 정책 설정 (PolicyEnabled 등)
├── metrics.go                   # 정책 관련 메트릭
├── origin.go                    # ruleOrigin - 규칙 출처 추적
├── lookup.go                    # 정책 조회 유틸리티
├── cookie/                      # 정책 로그 쿠키
├── correlation/                 # 정책 상관관계 분석
├── directory/                   # 정책 디렉터리 관리
├── groups/                      # 외부 그룹(AWS 등) 연동
├── k8s/                         # K8s CNP/CCNP 변환
├── trafficdirection/            # Ingress/Egress 방향 정의
└── utils/                       # 정책 유틸리티
```

### 주요 구조체 관계도

```
Repository (정책 저장소)
 ├── rules map[ruleKey]*rule           -- 모든 규칙
 ├── rulesByNamespace                   -- 네임스페이스별 인덱스
 ├── rulesByResource                    -- 리소스별 인덱스
 ├── selectorCache *SelectorCache       -- Peer 셀렉터 캐시
 ├── subjectSelectorCache               -- Subject 셀렉터 캐시
 └── policyCache *policyCache           -- 계산된 정책 캐시
      └── policies map[NumericIdentity]*cachedSelectorPolicy
           └── cachedSelectorPolicy
                └── selectorPolicy
                     ├── L4Policy
                     │    ├── Ingress: L4DirectionPolicy
                     │    │    └── PortRules: []L4PolicyMap
                     │    │         └── L4Filter
                     │    │              └── PerSelectorPolicies map[CachedSelector]*PerSelectorPolicy
                     │    └── Egress: L4DirectionPolicy
                     └── DistillPolicy() -> EndpointPolicy
                          └── policyMapState: mapState
                               └── entries map[Key]mapStateEntry
```

## 7. Identity 기반 정책 vs IP 기반 정책

### Identity 기반 정책

Cilium의 기본 정책 모델이다. 워크로드의 보안 아이덴티티(라벨에서 파생)를 기준으로 트래픽을 제어한다.

**장점:**
- Pod IP가 변경되어도 정책이 자동으로 유지됨
- 라벨 기반으로 동적 그룹핑이 가능
- 동일 아이덴티티를 공유하는 Pod 간에 정책 계산 결과를 공유하여 효율적

**동작 원리:**
1. Pod에 라벨이 할당됨
2. 라벨 집합의 해시로 NumericIdentity(uint32) 생성
3. 정책 규칙의 EndpointSelector가 이 라벨에 매칭
4. BPF 정책 맵에 `(Identity, Port, Protocol, Direction) -> (Allow/Deny)` 기록

### IP 기반 정책

클러스터 외부 트래픽이나 FQDN 정책 등 Identity를 직접 사용할 수 없는 경우에 사용한다.

**CIDR 정책에서의 IP → Identity 변환:**
1. `fromCIDR: 10.0.0.0/8`과 같은 CIDR 규칙이 정의됨
2. Cilium은 해당 CIDR을 `cidr:10.0.0.0/8` 라벨로 변환
3. IP 주소에 대해 해당 CIDR 라벨을 가진 Identity를 할당 (IPCache를 통해)
4. 결과적으로 Identity 기반 정책 맵에 해당 Identity에 대한 항목이 생성됨

따라서 Cilium에서는 CIDR 기반 정책도 궁극적으로 Identity 기반 메커니즘으로 통합된다.

## 8. Default Deny vs Default Allow 동작

Cilium은 정책 적용 모드(`policyEnforcement`)에 따라 기본 동작이 달라진다.

### 정책 적용 모드

`pkg/policy/config.go`와 `pkg/option/config.go`에서 설정:

| 모드 | 설명 |
|------|------|
| `default` | 정책이 하나라도 적용된 엔드포인트는 default deny, 없으면 allow all |
| `always` | 모든 엔드포인트에 항상 default deny 적용 |
| `never` | 정책을 전혀 적용하지 않음 (모든 트래픽 허용) |

### Default Deny 동작 상세

`Repository.computePolicyEnforcementAndRules()`에서 결정:

```go
// pkg/policy/repository.go
// 1. policyMode == NeverEnforce -> 정책 비활성화
// 2. policyMode == AlwaysEnforce -> 항상 default deny
// 3. policyMode == default:
//    - 매칭 규칙이 없는 경우 -> 정책 비적용 (allow all)
//    - DefaultDeny=true인 규칙이 있는 경우 -> default deny 활성화
//    - DefaultDeny=false인 규칙만 있는 경우 -> 정책 활성화 + wildcard allow 규칙 합성
```

`EnableDefaultDeny` 필드를 통해 규칙 단위로 default deny 여부를 제어할 수 있다:

```yaml
spec:
  enableDefaultDeny:
    ingress: false  # 이 규칙은 ingress에 대해 default deny를 활성화하지 않음
    egress: true
```

default deny가 비활성화된 규칙만 존재하는 경우, Cilium은 자동으로 **wildcard allow 규칙**을 합성하여 매칭되지 않는 트래픽이 여전히 허용되도록 한다. 이 메커니즘은 L7 가시성 전용 정책 등에 유용하다.

## 9. 정책 우선순위와 충돌 해결

### 9.1 Tier (계층)

Cilium은 3단계 Tier 시스템을 지원한다:

```go
// pkg/policy/types/policyentry.go
type Tier uint8
const (
    Admin    Tier = iota  // 0: 최우선 (관리자 정책)
    Normal                // 1: 일반 (기본값)
    Baseline              // 2: 기본선 (가장 낮은 우선순위)
)
```

상위 Tier의 규칙이 하위 Tier의 규칙보다 항상 우선한다.

### 9.2 Priority (우선순위)

같은 Tier 내에서 숫자가 낮을수록 높은 우선순위이다:

```go
type Priority uint32  // Lower values take precedence
```

### 9.3 Verdict 우선순위

같은 Tier + Priority 내에서의 충돌 해결:
1. **Deny > Allow > Pass**: 동일 우선순위에서 Deny가 항상 우선
2. **상위 우선순위 > 하위 우선순위**: 낮은 Priority 값(높은 우선순위)이 우선

### 9.4 Precedence 인코딩

데이터경로에서의 통합 우선순위는 `Precedence` 필드(uint32)로 인코딩된다:

```go
// pkg/policy/types/entry.go
// Precedence 구조:
// [31:8] - 반전된 Priority (높을수록 우선)
// [7:0]  - Verdict 및 Listener Priority
//   255 = Deny
//   2-254 = Allow with proxy port (listener priority)
//   1 = Allow without proxy
//   0 = Pass
```

### 9.5 L4Filter 병합 규칙

동일 포트/프로토콜에 여러 규칙이 매칭되면 `rule.go`의 `mergePortProto()`에서 병합된다:

```go
// pkg/policy/rule.go - mergePortProto()
// 1. 동일 CachedSelector에 대한 규칙이 이미 존재하면:
//    a. 우선순위 비교 (priority)
//    b. 낮은 우선순위의 규칙은 무시
//    c. 같은 우선순위면 Deny가 우선
//    d. 같은 우선순위 Allow끼리면 L7 규칙을 병합 (union)
// 2. 존재하지 않으면: 그대로 추가
```

## 10. FQDN 정책의 DNS 프록시 연동

### 10.1 전체 동작 흐름

```
Pod A (DNS 쿼리: api.example.com)
     |
     v
[Cilium DNS Proxy] -- DNS 패킷 가로채기
     |
     v
[Upstream DNS Server] -- 실제 DNS 응답 수신
     |
     v
[DNSCache.Update()] -- 도메인 → IP 매핑 저장
     |
     v
[Name Manager] -- IP에 FQDN 라벨 기반 Identity 할당
     |
     v
[IPCache 업데이트] -- IP → Identity 매핑 업데이트
     |
     v
[SelectorCache.UpdateIdentities()] -- 관련 셀렉터의 Identity 목록 업데이트
     |
     v
[EndpointPolicy 재계산] -- 새 Identity에 대한 BPF 맵 엔트리 추가
     |
     v
[BPF PolicyMap 업데이트] -- 데이터경로에 반영
     |
     v
Pod A (TCP 연결: api.example.com의 IP) -- 허용됨
```

### 10.2 DNSCache

```go
// pkg/fqdn/cache.go
type DNSCache struct {
    forward map[string]ipEntries    // 도메인 이름 → IP 매핑
    reverse map[netip.Addr]nameEntries // IP → 도메인 이름 역방향 매핑
    lastCleanup time.Time
    cleanup map[int64][]string       // TTL 만료 시간별 정리 스케줄
    perHostLimit int                 // 호스트당 최대 IP 수
    minTTL int                       // 최소 TTL
}
```

각 DNS 캐시 엔트리는 TTL을 가지며, TTL이 만료되면 해당 IP의 Identity가 제거되어 트래픽이 자동으로 차단된다.

### 10.3 DNS 프록시 연동

DNS 프록시는 `pkg/fqdn/dnsproxy/`에 구현되어 있으며, `pkg/proxy/dns.go`의 `dnsRedirect`를 통해 정책 엔진과 연동된다:

```go
// pkg/proxy/dns.go
type dnsRedirect struct {
    Redirect
    dnsProxy fqdnproxy.DNSProxier
}

func (dr *dnsRedirect) UpdateRules(rules policy.L7DataMap) (revert.RevertFunc, error) {
    return dr.setRules(rules)
}
```

DNS 프록시는 `toFQDNs` 규칙에 명시된 도메인 패턴에 매칭되는 DNS 쿼리만 관찰하며, DNS 응답의 IP 주소를 수집하여 정책 업데이트를 트리거한다.

### 10.4 FQDNSelector의 Identity 라벨

FQDN 정책은 도메인 이름에서 파생된 특수 라벨을 사용한다:

```go
// pkg/policy/api/fqdn.go
func (s *FQDNSelector) IdentityLabel() labels.Label {
    match := s.MatchPattern
    if s.MatchName != "" {
        match = s.MatchName
    }
    return labels.NewLabel(match, "", labels.LabelSourceFQDN)
}
```

예를 들어, `toFQDNs: [{matchName: "api.example.com"}]`은 `fqdn:api.example.com` 라벨을 가진 Identity를 선택한다. DNS 응답에서 해당 도메인의 IP가 확인되면, 해당 IP에 이 라벨이 부여되어 FQDN 셀렉터와 매칭된다.

## 11. L7 정책의 Envoy 프록시 연동

### 11.1 동작 흐름

L7 정책이 적용되면 해당 트래픽은 Envoy 프록시를 거치게 된다:

```
Pod A → [BPF] → [Envoy Proxy] → [BPF] → Pod B
                      |
              L7 정책 평가
              (HTTP method/path 등 검사)
```

### 11.2 프록시 리다이렉트 설정

1. 정책 증류 시 L7 규칙이 있는 `L4Filter`가 감지됨
2. `selectorPolicy.RedirectFilters()` 이터레이터로 프록시가 필요한 필터 식별
3. 프록시 포트 할당 (`pkg/proxy/proxyports/`)
4. BPF 정책 맵에 해당 `(Identity, Port)` 항목의 `ProxyPort`를 설정
5. BPF 데이터경로가 `ProxyPort != 0`인 트래픽을 Envoy로 리다이렉트

```go
// pkg/proxy/envoyproxy.go
type envoyRedirect struct {
    Redirect
    listenerName string
    xdsServer    envoy.XDSServer
    adminClient  *envoy.EnvoyAdminClient
}
```

### 11.3 Envoy 구성

Envoy 프록시 설정은 xDS API를 통해 동적으로 관리된다:
- `CiliumEnvoyConfig` (CEC): 네임스페이스 범위의 Envoy 리스너 설정
- `CiliumClusterwideEnvoyConfig` (CCEC): 클러스터 범위의 Envoy 리스너 설정

정책에서 커스텀 Envoy 리스너를 참조할 수 있다:

```yaml
toPorts:
  - ports:
      - port: "80"
    listener:
      envoyConfig:
        kind: CiliumEnvoyConfig
        name: my-listener-config
      name: my-listener
      priority: 10
```

### 11.4 L7 파서 유형과 프록시 포트 우선순위

각 L7 파서 유형에는 고유한 기본 listener 우선순위가 있다:

```go
// pkg/policy/types/entry.go
// 101 - HTTP parser type
// 106 - Kafka parser type
// 111 - proxylib parsers
// 116 - TLS interception parsers
// 121 - DNS parser type
// 126 - CRD parser type (default)
```

같은 포트에 여러 L7 규칙이 병합될 때, 더 높은 listener 우선순위(낮은 숫자)를 가진 파서가 선택된다.

## 12. 참조 파일 경로

| 파일 | 설명 |
|------|------|
| `pkg/policy/repository.go` | PolicyRepository 인터페이스 및 Repository 구현 |
| `pkg/policy/distillery.go` | policyCache, cachedSelectorPolicy |
| `pkg/policy/resolve.go` | selectorPolicy, EndpointPolicy, DistillPolicy |
| `pkg/policy/rule.go` | rule 구조체, L7 규칙 병합 로직 |
| `pkg/policy/l4.go` | L4Filter, L4Policy, L4PolicyMap, PerSelectorPolicy |
| `pkg/policy/mapstate.go` | mapState (BPF 맵 상태 관리) |
| `pkg/policy/selectorcache.go` | SelectorCache (Identity ↔ Selector 매핑) |
| `pkg/policy/types/types.go` | Key, LPMKey (BPF 맵 키 구조체) |
| `pkg/policy/types/entry.go` | MapStateEntry, Precedence, Priority |
| `pkg/policy/types/policyentry.go` | PolicyEntry, Tier, Verdict |
| `pkg/policy/types/selector.go` | Selector 인터페이스, LabelSelector, FQDNSelector, CIDRSelector |
| `pkg/policy/api/rule.go` | api.Rule (CRD 스펙 구조체) |
| `pkg/policy/api/ingress.go` | IngressRule, IngressCommonRule |
| `pkg/policy/api/egress.go` | EgressRule, EgressCommonRule, ToFQDNs |
| `pkg/policy/api/l4.go` | PortProtocol, PortRule, L7Rules |
| `pkg/policy/api/http.go` | PortRuleHTTP |
| `pkg/policy/api/fqdn.go` | FQDNSelector, PortRuleDNS |
| `pkg/maps/policymap/policymap.go` | BPF PolicyMap (커널 맵 인터페이스) |
| `bpf/lib/policy.h` | BPF 데이터경로 정책 구조체 (policy_key, policy_entry) |
| `pkg/fqdn/cache.go` | DNSCache (DNS 이름 ↔ IP 캐시) |
| `pkg/proxy/proxy.go` | Proxy 매니저 |
| `pkg/proxy/dns.go` | DNS 프록시 리다이렉트 |
| `pkg/proxy/envoyproxy.go` | Envoy 프록시 리다이렉트 |
| `pkg/k8s/apis/cilium.io/v2/cnp_types.go` | CiliumNetworkPolicy CRD |
| `pkg/k8s/apis/cilium.io/v2/ccnp_types.go` | CiliumClusterwideNetworkPolicy CRD |
