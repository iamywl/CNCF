# 10. Cilium Policy Engine 심화

## 1. 개요

Cilium의 정책 엔진(Policy Engine)은 Kubernetes CiliumNetworkPolicy(CNP) CRD로 선언된 네트워크
정책을 최종적으로 BPF 맵 엔트리로 변환하는 핵심 서브시스템이다. 이 파이프라인은 단순한 "규칙 파싱 →
맵 기록"이 아니라, 수만 개의 엔드포인트와 아이덴티티가 동적으로 변하는 환경에서 **증분 업데이트
(incremental update)**를 효율적으로 수행하도록 설계되어 있다.

정책 엔진이 해결해야 하는 핵심 문제는 다음 세 가지다:

1. **규모(Scale)**: 수천 개의 정책 규칙과 수만 개의 아이덴티티 조합을 O(N*M) 완전 탐색 없이 처리
2. **일관성(Consistency)**: 정책 변경 시 모든 관련 엔드포인트의 BPF 맵이 원자적으로 업데이트
3. **성능(Performance)**: 아이덴티티 추가/삭제 시 전체 정책을 재계산하지 않고 변경분만 반영

```
+---------------------+     +------------------+     +-------------------+
|  CiliumNetworkPolicy|     |  PolicyRepository|     |  SelectorCache    |
|  (CRD / K8s)        | --> |  (규칙 저장소)     | --> |  (Identity-Selector|
|                     |     |                  |     |   매핑 캐시)        |
+---------------------+     +------------------+     +-------------------+
                                    |                         |
                                    v                         v
                            +------------------+     +-------------------+
                            |  SelectorPolicy  |     |  Identity 변경     |
                            |  (L4Policy 트리)  |     |  알림(Notification)|
                            +------------------+     +-------------------+
                                    |                         |
                                    v                         v
                            +------------------+     +-------------------+
                            |  EndpointPolicy  |     |  MapChanges       |
                            |  (policyMapState)| <-- |  (증분 변경 큐)     |
                            +------------------+     +-------------------+
                                    |
                                    v
                            +------------------+
                            |  BPF PolicyMap   |
                            |  (cilium_policy_v2_)|
                            +------------------+
```

### 왜 이런 다단계 파이프라인인가?

단순히 "규칙 → BPF 맵"으로 직접 변환하면, 아이덴티티 하나가 추가될 때마다 모든 규칙을 다시
순회해야 한다. Cilium은 중간에 **SelectorCache**라는 캐시 계층을 두어, 셀렉터가 어떤 아이덴티티를
선택하는지를 미리 계산해둔다. 아이덴티티가 변경되면 영향받는 셀렉터만 업데이트하고, 그 셀렉터를
사용하는 엔드포인트에만 증분 변경을 전파한다.

---

## 2. Policy Repository 구조

### 2.1 Repository 인터페이스

`PolicyRepository` 인터페이스는 정책 저장소의 공개 계약을 정의한다.

> 파일: `pkg/policy/repository.go` (라인 34-56)

```go
type PolicyRepository interface {
    BumpRevision() uint64
    GetAuthTypes(localID, remoteID identity.NumericIdentity) AuthTypes
    GetEnvoyHTTPRules(l7Rules *api.L7Rules, ns string) (*cilium.HttpNetworkPolicyRules, bool)
    GetSelectorPolicy(id *identity.Identity, skipRevision uint64,
        stats GetPolicyStatistics, endpointID uint64) (SelectorPolicy, uint64, error)
    GetPolicySnapshot() map[identity.NumericIdentity]SelectorPolicy
    GetRevision() uint64
    GetRulesList() *models.Policy
    GetSelectorCache() *SelectorCache
    GetSubjectSelectorCache() *SelectorCache
    Iterate(f func(rule *types.PolicyEntry))
    ReplaceByResource(rules types.PolicyEntries, resource ipcachetypes.ResourceID) (
        affectedIDs *set.Set[identity.NumericIdentity], rev uint64, oldRevCnt int)
    Search() (types.PolicyEntries, uint64)
}
```

핵심 메서드를 정리하면:

| 메서드 | 역할 | 호출 시점 |
|--------|------|----------|
| `ReplaceByResource()` | 특정 리소스의 규칙을 원자적 교체 | CRD upsert/delete |
| `GetSelectorPolicy()` | 아이덴티티에 대한 SelectorPolicy 계산/캐시 | 엔드포인트 재생성 |
| `BumpRevision()` | 정책 리비전 번호 증가 | 규칙 변경 후 |
| `GetRevision()` | 현재 리비전 조회 | 스킵 판단 기준 |

### 2.2 Repository 구조체

> 파일: `pkg/policy/repository.go` (라인 63-98)

```go
type Repository struct {
    logger *slog.Logger
    mutex  lock.RWMutex

    rules            map[ruleKey]*rule            // 전체 규칙 맵
    rulesByNamespace map[string]sets.Set[ruleKey]  // 네임스페이스별 인덱스
    rulesByResource  map[ipcachetypes.ResourceID]map[ruleKey]*rule  // 리소스별 인덱스

    nextID   uint           // 테스트용 규칙 키 생성
    revision atomic.Uint64  // 정책 리비전 (항상 > 0)

    selectorCache        *SelectorCache  // 피어 셀렉터 캐시
    subjectSelectorCache *SelectorCache  // 서브젝트 셀렉터 캐시
    policyCache          *policyCache    // SelectorPolicy 캐시

    certManager    certificatemanager.CertificateManager
    metricsManager types.PolicyMetrics
    l7RulesTranslator envoypolicy.EnvoyL7RulesTranslator
}
```

**왜 세 가지 맵을 유지하는가?**

- `rules`: O(1) 키 기반 룩업. 규칙 삭제/수정 시 직접 접근.
- `rulesByNamespace`: 특정 엔드포인트에 적용될 규칙을 찾을 때, 전체 규칙을 순회하지 않고
  **클러스터 와이드(`""`) + 해당 네임스페이스** 두 개의 셋만 순회하면 된다.
- `rulesByResource`: CRD 하나가 여러 규칙으로 분해되므로, 해당 CRD가 삭제/수정될 때
  관련 규칙을 O(1)로 찾아 일괄 삭제한다.

### 2.3 리비전(Revision) 관리

리비전은 `atomic.Uint64`로 관리되며, 규칙이 변경될 때마다 `BumpRevision()`으로 증가한다.
엔드포인트가 `GetSelectorPolicy()`를 호출할 때 `skipRevision`을 전달하면, 이미 계산된
리비전 이후에 변경이 없으면 계산을 건너뛸 수 있다.

```
시간축  ──────────────────────────────────────────>

Rev 1: CNP-A 추가
Rev 2: CNP-B 추가
Rev 3: CNP-A 수정

엔드포인트 X (skipRevision=2):
  → Rev 3 > 2 이므로 재계산 필요

엔드포인트 Y (skipRevision=3):
  → Rev 3 >= 3 이므로 계산 스킵
```

---

## 3. 규칙(Rule) 구조와 매칭

### 3.1 PolicyEntry 타입

정책 규칙의 원천 데이터는 `types.PolicyEntry`에 저장된다.

> 파일: `pkg/policy/types/policyentry.go` (라인 44-80+)

```go
type PolicyEntry struct {
    Tier     Tier
    Priority float64

    Authentication *api.Authentication
    Log            api.LogConfig

    Subject *LabelSelector   // 이 규칙이 적용되는 엔드포인트 (Subject)
    L3      Selectors         // 소스/목적지 피어 셀렉터
    L4      api.PortRules     // 포트/프로토콜 규칙

    Labels      labels.LabelArray
    DefaultDeny bool
    Verdict     Verdict         // Allow, Deny, Pass
    Ingress     bool            // true=ingress, false=egress
}
```

### 3.2 내부 rule 구조체

Repository 내부에서 PolicyEntry는 `rule` 구조체로 래핑된다.

> 파일: `pkg/policy/rule.go` (라인 26-32)

```go
type rule struct {
    types.PolicyEntry
    key ruleKey

    // SelectorCache에 등록된 Subject 셀렉터
    subjectSelector CachedSelector
}
```

`subjectSelector`는 이 규칙의 `Subject` 필드가 SelectorCache에 등록된 결과다.
이를 통해 어떤 아이덴티티가 이 규칙의 "대상"인지를 빠르게 조회할 수 있다.

### 3.3 규칙 매칭 로직: computePolicyEnforcementAndRules

특정 아이덴티티에 적용되는 규칙을 찾는 핵심 함수다.

> 파일: `pkg/policy/repository.go` (라인 371-510)

```
computePolicyEnforcementAndRules(securityIdentity) 호출 흐름:

1. 정책 모드 확인 (NeverEnforce이면 즉시 반환)
2. 클러스터 와이드 규칙 매칭 (rulesByNamespace[""])
   └─ 각 규칙의 matchesSubject(identity) 호출
3. 네임스페이스별 규칙 매칭 (rulesByNamespace[namespace])
   └─ 동일하게 matchesSubject() 호출
4. 우선순위 정렬 (rulesIngress.sort(), rulesEgress.sort())
5. 와일드카드 규칙 합성 (필요 시)
   ├─ DefaultDeny 없으면 allow-all 와일드카드 추가
   └─ Pass verdict 있으면 deny 와일드카드 추가
```

**왜 두 단계로 나누어 매칭하는가?**

네임스페이스가 없는 클러스터 와이드 규칙(CCNP)과 네임스페이스 규칙(CNP)을 분리하면,
특정 네임스페이스의 엔드포인트는 최대 2개의 셋만 순회하면 된다.
N개의 네임스페이스에 M개의 규칙이 고르게 분포하면, 전체 순회 대비 **N배** 빠르다.

### 3.4 우선순위와 와일드카드 합성

규칙은 `Tier`(계층)와 `Priority`(우선순위)로 정렬된다.

```
Tier 0 (기본, 최우선)
  ├─ Priority 0 (최우선)  → deny 규칙
  ├─ Priority 100         → allow 규칙 A
  └─ Priority 200         → allow 규칙 B

Tier 1
  ├─ Priority 0           → pass 규칙
  └─ ...

와일드카드 합성:
  └─ 마지막 규칙의 Tier/Priority 사용 → allow-all 또는 deny-all
```

DefaultDeny가 없는 경우(모든 규칙이 `enableDefaultDeny: false`), L7 프록시 리다이렉트 등의
부가 효과를 보존하기 위해 **allow-all 와일드카드 규칙을 가장 낮은 우선순위에 합성**한다.

---

## 4. SelectorCache: Identity-Selector 매핑

### 4.1 SelectorCache 구조

SelectorCache는 정책 엔진의 **심장부**다. 모든 셀렉터가 현재 어떤 아이덴티티를 선택하는지를
미리 계산해두고, 아이덴티티 변경 시 증분 업데이트를 수행한다.

> 파일: `pkg/policy/selectorcache.go` (라인 229-276)

```go
type SelectorCache struct {
    logger *slog.Logger

    readTxn atomic.Pointer[SelectorSnapshot]  // 락 없이 현재 선택 상태 조회

    mutex    lock.RWMutex
    revision types.SelectorRevision            // 선택 상태 리비전

    readableSelections  types.SelectionsMap       // 마지막 커밋 시점의 선택 상태
    writeableSelections types.SelectorWriteTxn     // 쓰기 트랜잭션 (풀링)

    idCache   scIdentityCache   // 전체 아이덴티티 캐시
    selectors selectorMap       // 등록된 모든 셀렉터

    localIdentityNotifier identityNotifier  // FQDN 통합용

    userCond      *sync.Cond
    userMutex     lock.Mutex
    userNotes     []userNotification      // 사용자 알림 FIFO 큐
    notifiedUsers map[CachedSelectionUser]struct{}

    startNotificationsHandlerOnce sync.Once
    userHandlerDone               chan struct{}
}
```

### 4.2 scIdentityCache: 네임스페이스 최적화

아이덴티티 캐시는 단순한 맵이 아니라 **네임스페이스별 인덱스**를 유지한다.

> 파일: `pkg/policy/selectorcache.go` (라인 36-39)

```go
type scIdentityCache struct {
    ids         map[identity.NumericIdentity]*scIdentity
    byNamespace map[string]map[*scIdentity]struct{}
}
```

**왜 네임스페이스별 인덱스가 필요한가?**

셀렉터가 네임스페이스를 지정하면, 해당 네임스페이스의 아이덴티티만 매칭하면 된다.
인덱스 없이는 전체 아이덴티티를 순회해야 하지만, `byNamespace`를 통해
해당 네임스페이스의 아이덴티티만 빠르게 필터링할 수 있다.

```
전체 아이덴티티: 10,000개
네임스페이스 "prod": 500개
네임스페이스 "dev": 300개

네임스페이스 "prod" 셀렉터 매칭:
  인덱스 없음: 10,000번 비교
  인덱스 사용: 500번 비교 (20배 절감)
```

### 4.3 UpdateIdentities: 아이덴티티 변경 처리

아이덴티티가 추가/삭제되면 `UpdateIdentities()`가 호출된다.

> 파일: `pkg/policy/selectorcache.go` (라인 765-873)

```
UpdateIdentities(added, deleted IdentityMap) 흐름:

1. 삭제할 아이덴티티를 idCache에서 제거
   └─ 해당 네임스페이스를 namespaces 맵에 기록
2. 추가할 아이덴티티를 idCache에 삽입
   ├─ 기존과 동일하면 스킵 (labels 비교)
   └─ 네임스페이스를 namespaces 맵에 기록
3. 영향받는 네임스페이스별로 셀렉터 업데이트
   └─ selectors.ByNamespace(ns) 순회
       └─ updateSelections(sel, added, deleted)
4. 변경이 있으면 commit() → readTxn 갱신
5. 사용자(EndpointPolicy) 알림 큐잉
```

핵심은 3단계다. 네임스페이스별로 관련 셀렉터만 업데이트하므로, 전체 셀렉터를 순회하지
않는다.

```
네임스페이스별 처리:
  "" (모든 셀렉터): 추가된 아이덴티티 전체
  "prod":          "prod" 네임스페이스 아이덴티티만
  "dev":           "dev" 네임스페이스 아이덴티티만

  ※ 네임스페이스가 있는 아이덴티티는 ""(비네임스페이스) 셀렉터에도 추가
```

### 4.4 Commit과 SelectorSnapshot

SelectorCache는 **MVCC(Multi-Version Concurrency Control)** 패턴을 사용한다.

```go
func (sc *SelectorCache) commit() SelectorSnapshot {
    sc.revision++
    sc.readableSelections = sc.writeableSelections.Commit()
    readTxn := types.GetSelectorSnapshot(sc.readableSelections, sc.revision)
    sc.readTxn.Store(&readTxn)
    return readTxn
}
```

`readTxn`은 `atomic.Pointer`로 저장되어 **락 없이** 읽을 수 있다. 쓰기는 mutex로 보호되지만,
읽기는 원자적 포인터 교체로 일관된 스냅샷을 제공한다.

**왜 MVCC인가?**

정책 계산 중에 아이덴티티가 변경되면 일관성 문제가 발생한다. MVCC를 통해:
- 읽기 측은 항상 일관된 스냅샷을 본다
- 쓰기 측은 읽기를 차단하지 않는다
- 스냅샷 전환은 원자적 포인터 교체로 무잠금(lock-free)이다

---

## 5. 정책 해석 (Resolve) 흐름

### 5.1 resolvePolicyLocked

특정 아이덴티티에 대한 전체 정책을 계산하는 함수다.

> 파일: `pkg/policy/repository.go` (라인 305-364)

```
resolvePolicyLocked(securityIdentity) 흐름:

1. computePolicyEnforcementAndRules()
   └─ ingress/egress 규칙 분류 및 우선순위 정렬

2. selectorPolicy 생성
   ├─ Revision = 현재 Repository 리비전
   ├─ SelectorCache 참조
   └─ L4Policy 초기화

3. policyContext 생성
   ├─ 네임스페이스 추출
   ├─ defaultDeny 상태 설정
   └─ 트레이싱 활성화 여부

4. Ingress 정책 해석
   └─ rulesIngress.resolveL4Policy(&policyCtx)
       └─ 각 규칙의 L4 포트 규칙 → L4Filter로 변환

5. Egress 정책 해석
   └─ rulesEgress.resolveL4Policy(&policyCtx)

6. Attach(&policyCtx)
   └─ 증분 업데이트를 위한 사용자 등록

7. SelectorCache.Commit()
   └─ 새 셀렉터의 선택 상태 공개
```

### 5.2 PolicyContext 인터페이스

> 파일: `pkg/policy/resolve.go` (라인 28-70)

```go
type PolicyContext interface {
    AllowLocalhost() bool
    GetNamespace() string
    GetSelectorCache() *SelectorCache
    GetTLSContext(tls *api.TLSContext) (ca, public, private string, inlineSecrets bool, err error)
    GetEnvoyHTTPRules(l7Rules *api.L7Rules) (*cilium.HttpNetworkPolicyRules, bool)
    SetPriority(tier types.Tier, priority types.Priority)
    Priority() (tier types.Tier, priority types.Priority)
    DefaultDenyIngress() bool
    DefaultDenyEgress() bool
    SetOrigin(ruleOrigin)
    Origin() ruleOrigin
    GetLogger() *slog.Logger
    PolicyTrace(format string, a ...any)
}
```

**왜 인터페이스로 분리하는가?**

테스트 코드에서 전체 Repository를 모킹하지 않고, 필요한 부분만 구현한 테스트용
PolicyContext를 주입할 수 있다. 이는 정책 해석 로직의 단위 테스트를 크게 단순화한다.

### 5.3 policyCache: SelectorPolicy 캐싱

> 파일: `pkg/policy/distillery.go` (라인 17-26)

```go
type policyCache struct {
    lock.Mutex
    repo     *Repository
    policies map[identityPkg.NumericIdentity]*cachedSelectorPolicy
}
```

`GetSelectorPolicy()`는 먼저 캐시를 확인하고, 캐시된 정책의 리비전이 현재 리비전 이상이면
재계산을 건너뛴다.

> 파일: `pkg/policy/distillery.go` (라인 127-157)

```
updateSelectorPolicy(identity, endpointID) 흐름:

1. 캐시 조회 → cachedSelectorPolicy 가져옴
2. cachedSelectorPolicy 잠금
3. 리비전 비교
   ├─ 캐시된 리비전 >= 현재 리비전 → 스킵 (캐시 히트)
   └─ 아니면 → resolvePolicyLocked() 호출
4. 새 정책 설정: cip.setPolicy(selPolicy, endpointID)
```

**왜 아이덴티티별로 캐싱하는가?**

같은 아이덴티티를 가진 여러 엔드포인트는 동일한 SelectorPolicy를 공유할 수 있다.
100개의 Pod이 같은 레이블을 가지면, 정책 계산은 1번만 하고 100개가 공유한다.

---

## 6. L4Policy와 PerSelectorPolicy

### 6.1 L4Policy 구조

> 파일: `pkg/policy/l4.go` (라인 1498-1518)

```go
type L4Policy struct {
    Ingress L4DirectionPolicy
    Egress  L4DirectionPolicy

    authMap   authMap          // 셀렉터별 인증 요구사항
    Revision  uint64           // 생성 시 Repository 리비전
    redirectTypes redirectTypes // 리다이렉트 유형 비트마스크
    mutex     lock.RWMutex
    users     map[*EndpointPolicy]struct{}  // 이 정책을 사용하는 EndpointPolicy 목록
}
```

### 6.2 L4DirectionPolicy

> 파일: `pkg/policy/l4.go` (라인 1447-1457)

```go
type L4DirectionPolicy struct {
    PortRules L4PolicyMaps

    // 각 Tier의 시작 우선순위
    tierBasePriority []types.Priority

    // 기능 플래그 (리다이렉트, 인증 등)
    features policyFeatures
}
```

`tierBasePriority`는 멀티-티어 정책에서 각 티어의 우선순위 경계를 추적한다.
Tier 0의 규칙이 Priority 0-200을 사용하면, Tier 1의 시작 Priority는 201이 된다.

### 6.3 L4Filter: 포트/프로토콜별 정책

> 파일: `pkg/policy/l4.go` (라인 523-554)

```go
type L4Filter struct {
    Tier     types.Tier
    U8Proto  u8proto.U8proto   // L4 프로토콜 (TCP=6, UDP=17, ...)
    Port     uint16            // 대상 포트 (0 = 모든 포트)
    EndPort  uint16            // 포트 범위의 끝 (0 = 단일 포트)
    Protocol api.L4Proto
    PortName string

    wildcard            CachedSelector     // 와일드카드 셀렉터 (있으면)
    PerSelectorPolicies L7DataMap          // 셀렉터별 L7 정책
    Ingress             bool
    RuleOrigin          map[CachedSelector]ruleOrigin

    policy atomic.Pointer[L4Policy]  // 순환 참조 (Detach에서 정리)
}
```

L4Filter는 **하나의 포트/프로토콜 조합**에 대한 정책을 표현한다. 여러 규칙이 같은
포트/프로토콜을 대상으로 하면, 그 규칙들의 셀렉터가 `PerSelectorPolicies` 맵에 병합된다.

### 6.4 PerSelectorPolicy: 셀렉터별 L7 정책

> 파일: `pkg/policy/l4.go` (라인 126-184)

```go
type PerSelectorPolicy struct {
    L7Parser         L7ParserType      // L7 파서 유형 (http, dns, tls, crd)
    Priority         types.Priority    // 규칙 우선순위
    Verdict          types.Verdict     // Allow, Deny, Pass
    ListenerPriority ListenerPriority  // 리스너 우선순위
    Listener         string            // Envoy 리스너 이름

    TerminatingTLS *TLSContext   // TLS 종단 컨텍스트
    OriginatingTLS *TLSContext   // TLS 원점 컨텍스트
    ServerNames    StringSet     // 허용된 TLS SNI 값

    envoyHTTPRules  *cilium.HttpNetworkPolicyRules  // 사전 계산된 HTTP 규칙
    canShortCircuit bool                            // 단축 평가 가능 여부

    api.L7Rules                       // DNS, HTTP, L7 규칙
    Authentication *api.Authentication // 인증 요구사항
}
```

**PerSelectorPolicy의 계층 구조:**

```
L4Filter (포트 80, TCP)
├─ CachedSelector("app=web")
│   └─ PerSelectorPolicy
│       ├─ L7Parser: HTTP
│       ├─ Verdict: Allow
│       ├─ Priority: 0
│       └─ L7Rules: {HTTP: [{Method: GET, Path: "/api/*"}]}
│
├─ CachedSelector("app=admin")
│   └─ PerSelectorPolicy
│       ├─ L7Parser: HTTP
│       ├─ Verdict: Allow
│       ├─ Priority: 100
│       └─ L7Rules: {HTTP: [{Method: "*", Path: "*"}]}
│
└─ CachedSelector(wildcard)
    └─ PerSelectorPolicy
        ├─ Verdict: Deny
        └─ Priority: 255
```

### 6.5 L7 파서 우선순위

> 파일: `pkg/policy/l4.go` (라인 374-399)

```go
const (
    ListenerPriorityNone ListenerPriority = 0    // 프록시 리다이렉트 없음
    ListenerPriorityHTTP ListenerPriority = 101  // HTTP 파서
    ListenerPriorityTLS  ListenerPriority = 116  // TLS 인터셉터
    ListenerPriorityDNS  ListenerPriority = 121  // DNS 파서
    ListenerPriorityCRD  ListenerPriority = 126  // CRD 커스텀 리스너
)
```

**왜 파서 유형마다 우선순위가 다른가?**

같은 포트에 여러 L7 규칙이 적용될 수 있다. 예를 들어 포트 443에 TLS 인터셉션과 HTTP
파싱이 모두 지정되면, HTTP 파서(101)가 TLS 파서(116)보다 높은 우선순위를 가진다.
이는 TLS → HTTP 프로모션을 자연스럽게 지원한다.

파서 유형 간 병합 규칙:

```
None → HTTP  : 가능 (프로모션)
None → DNS   : 가능 (프로모션)
TLS  → HTTP  : 가능 (프로모션, TLS 종단 후 HTTP 파싱)
TLS  → DNS   : 불가 (충돌)
HTTP → DNS   : 불가 (충돌)
CRD  → HTTP  : 불가 (충돌)
```

---

## 7. EndpointPolicy와 MapState

### 7.1 EndpointPolicy 구조

> 파일: `pkg/policy/resolve.go` (라인 200-233)

```go
type EndpointPolicy struct {
    // 같은 아이덴티티의 모든 엔드포인트가 공유
    SelectorPolicy *selectorPolicy

    // policyMapState 생성 시점의 SelectorCache 스냅샷
    selectors SelectorSnapshot

    // BPF 맵에 기록할 상태
    policyMapState mapState

    // 증분 변경 큐
    policyMapChanges MapChanges

    // 이 정책을 소비하는 엔드포인트
    PolicyOwner PolicyOwner

    // 프록시 리다이렉트 포트 맵
    Redirects map[string]uint16
}
```

**SelectorPolicy vs EndpointPolicy 분리의 이유:**

```
SelectorPolicy (아이덴티티별 공유)
├─ L4Policy: 셀렉터 기반 추상 정책
├─ IngressPolicyEnabled
└─ EgressPolicyEnabled

    ↓ DistillPolicy()로 변환

EndpointPolicy (엔드포인트별 개별)
├─ policyMapState: 구체적 Key → MapStateEntry 맵
├─ policyMapChanges: 증분 변경 큐
├─ Redirects: 실제 프록시 포트
└─ PolicyOwner: named port 해석 등 엔드포인트 컨텍스트
```

SelectorPolicy는 셀렉터 수준의 추상적 정책이고, EndpointPolicy는 이를 구체적인 BPF 맵
엔트리로 변환한 결과다. Named port 해석 등은 엔드포인트마다 다를 수 있으므로 분리한다.

### 7.2 DistillPolicy: SelectorPolicy → EndpointPolicy

> 파일: `pkg/policy/resolve.go` (라인 311-369)

```
DistillPolicy(logger, policyOwner, redirects) 흐름:

1. SelectorCache.WithRLock() 내에서:
   ├─ SelectorSnapshot 획득
   ├─ EndpointPolicy 생성 (policyMapState, policyMapChanges 초기화)
   └─ insertUser() → 증분 업데이트 수신 등록

2. Ingress/Egress 비활성 시 allowAllIdentities() 호출

3. L4Policy.Ingress.toMapState() → policyMapState에 엔트리 추가
4. L4Policy.Egress.toMapState()  → policyMapState에 엔트리 추가

5. localhost ingress 허용 판단

6. EndpointPolicy 반환
```

**왜 WithRLock 안에서 insertUser를 하는가?**

SelectorSnapshot 획득과 증분 업데이트 수신 등록이 원자적이어야 한다.
만약 스냅샷 획득 후 등록 전에 아이덴티티가 변경되면, 그 변경은 스냅샷에도 없고
증분 업데이트로도 전달되지 않아 누락된다.

```
시간축:
  T1: SelectorSnapshot 획득 (rev=5)
  T2: insertUser() 등록
  T3: 아이덴티티 변경 (rev=6)

  WithRLock 없으면:
    T1: 스냅샷 (rev=5)
    T2': 아이덴티티 변경 (rev=6) → 아직 등록 안됨 → 누락!
    T3': insertUser() 등록 → rev=6 변경을 받지 못함

  WithRLock 있으면:
    T1-T2: 원자적 → rev=6 변경은 반드시 T2 이후에 전달됨
```

### 7.3 MapStateEntry

> 파일: `pkg/policy/types/entry.go` (라인 119-141)

```go
type MapStateEntry struct {
    Precedence      Precedence     // 우선순위 (높을수록 우선)
    ProxyPort       uint16         // 프록시 포트 (0이면 리다이렉트 없음)
    invalid         bool           // 무효화 플래그
    AuthRequirement AuthRequirement // 인증 요구사항
    Cookie          uint32         // 정책 로그 쿠키
}
```

### 7.4 Key 구조

> 파일: `pkg/policy/types/types.go` (라인 19-36)

```go
type LPMKey struct {
    // bits: TrafficDirection(최상위 비트) + 포트 prefix 길이(하위 5비트)
    bits    uint8
    Nexthdr u8proto.U8proto  // 프로토콜 (TCP=6, UDP=17, ...)
    DestPort uint16          // 대상 포트 (호스트 바이트 오더)
}

type Key struct {
    LPMKey
    Identity identity.NumericIdentity  // 소스/목적지 아이덴티티
}
```

Key의 `bits` 필드는 단일 바이트에 두 가지 정보를 인코딩한다:

```
bits 레이아웃 (8비트):
┌─────────┬──────────────────┐
│ bit 7   │ bits 4-0         │
│ Direction│ PortPrefixLen   │
│ (0=In,  │ (0-16 비트)      │
│  1=Out) │                  │
└─────────┴──────────────────┘
```

---

## 8. BPF PolicyMap 구조

### 8.1 PolicyKey와 PolicyEntry

> 파일: `pkg/maps/policymap/policymap.go` (라인 104-113, 155-161)

```go
// BPF 맵 키 (bpf/lib/policy.h의 struct policy_key와 동기화 필수)
type PolicyKey struct {
    Prefixlen        uint32 `align:"lpm_key"`
    Identity         uint32 `align:"sec_label"`
    TrafficDirection uint8  `align:"egress"`
    Nexthdr          uint8  `align:"protocol"`
    DestPortNetwork  uint16 `align:"dport"`     // 네트워크 바이트 오더
}

// BPF 맵 값 (bpf/lib/policy.h의 struct policy_entry와 동기화 필수)
type PolicyEntry struct {
    ProxyPortNetwork uint16                      `align:"proxy_port"` // 네트워크 바이트 오더
    Flags            policyEntryFlags            `align:"deny"`
    AuthRequirement  policyTypes.AuthRequirement `align:"auth_type"`
    Precedence       policyTypes.Precedence      `align:"precedence"`
    Cookie           uint32                      `align:"cookie"`
}
```

### 8.2 LPM (Longest Prefix Match) 구조

PolicyMap은 **LPM 트라이(Trie)** 기반 BPF 맵이다. 이를 통해 포트 범위를 효율적으로
표현한다.

```
Prefixlen 구성:
┌────────────────────┬──────────────┬────────────────┐
│ StaticPrefixBits   │ NexthdrBits  │ DestPortBits   │
│ (Identity +        │ (8비트)      │ (0-16비트)     │
│  Direction 등)     │              │                │
└────────────────────┴──────────────┴────────────────┘

포트 범위 예시:
  포트 8080 단일:  PrefixLen = 16 (전체 매칭)
  포트 8080-8095: PrefixLen = 12 (상위 12비트 매칭)
  모든 포트:      PrefixLen = 0  (와일드카드)
```

> 파일: `pkg/maps/policymap/policymap.go` (라인 139-150)

```go
const (
    NexthdrBits    = uint8(sizeofNexthdr) * 8    // 8비트
    DestPortBits   = uint8(sizeofDestPort) * 8   // 16비트
    FullPrefixBits = NexthdrBits + DestPortBits   // 24비트

    StaticPrefixBits = uint32(sizeofPolicyKey-sizeofPrefixlen)*8 - uint32(FullPrefixBits)
)
```

### 8.3 PolicyMap 플래그

> 파일: `pkg/maps/policymap/policymap.go` (라인 58-64)

```go
const (
    policyFlagDeny         policyEntryFlags = 1 << iota  // 비트 0: deny
    policyFlagReserved1                                  // 비트 1: 예약
    policyFlagReserved2                                  // 비트 2: 예약
    policyFlagLPMShift     = iota                        // 비트 3부터: LPM prefix 길이
    policyFlagMaskLPMPrefixLen = ((1 << 5) - 1) << policyFlagLPMShift
)
```

Flags 바이트 레이아웃:

```
┌───────────────────────┬──────┬──────┬──────┐
│ LPM PrefixLen (5비트) │ Rsv2 │ Rsv1 │ Deny │
│ bits 7-3              │ bit2 │ bit1 │ bit0 │
└───────────────────────┴──────┴──────┴──────┘
```

### 8.4 맵 이름 규칙

```go
const (
    PolicyCallMapName       = "cilium_call_policy"         // 정책 tail call 맵
    PolicyEgressCallMapName = "cilium_egresscall_policy"   // egress tail call 맵
    MapName                 = "cilium_policy_v2_"           // 엔드포인트별 정책 맵 접두사
)
```

각 엔드포인트는 `cilium_policy_v2_{endpoint_id}` 이름의 개별 BPF 맵을 가진다.

**왜 엔드포인트별 개별 맵인가?**

전역 정책 맵을 사용하면 키에 엔드포인트 ID를 포함해야 하고, 맵 크기가 기하급수적으로
커진다. 엔드포인트별 맵을 사용하면:
- 각 맵의 크기가 작아 LPM 탐색이 빠르다
- 엔드포인트 삭제 시 맵 전체를 삭제하면 되어 정리가 간단하다
- 맵 업데이트가 해당 엔드포인트에만 영향을 미친다

---

## 9. CRD에서 BPF 맵까지 전체 흐름

### 9.1 전체 파이프라인

```
                           CiliumNetworkPolicy (YAML)
                                    │
                        ┌───────────┴───────────┐
                        │ K8s API Server Watch   │
                        └───────────┬───────────┘
                                    │
                        ┌───────────┴───────────┐
                        │ policyWatcher          │
                        │ (k8s/cilium_network_   │
                        │  policy.go)            │
                        └───────────┬───────────┘
                                    │ cnp.Parse()
                                    │ RulesToPolicyEntries()
                                    v
                        ┌───────────────────────┐
                        │ PolicyImporter.        │
                        │ UpdatePolicy()         │
                        └───────────┬───────────┘
                                    │
                        ┌───────────┴───────────┐
                        │ Repository.            │
                        │ ReplaceByResource()    │
                        └───────────┬───────────┘
                                    │ affectedIDs 반환
                                    v
                        ┌───────────────────────┐
                        │ 영향받는 엔드포인트     │
                        │ 재생성(Regeneration)    │
                        └───────────┬───────────┘
                                    │
                        ┌───────────┴───────────┐
                        │ GetSelectorPolicy()    │
                        │ → resolvePolicyLocked() │
                        │ → SelectorPolicy       │
                        └───────────┬───────────┘
                                    │
                        ┌───────────┴───────────┐
                        │ DistillPolicy()        │
                        │ → EndpointPolicy       │
                        │ → policyMapState       │
                        └───────────┬───────────┘
                                    │
                        ┌───────────┴───────────┐
                        │ BPF PolicyMap 업데이트  │
                        │ (cilium_policy_v2_*)   │
                        └───────────────────────┘
```

### 9.2 단계별 상세

#### 단계 1: CRD 수신

> 파일: `pkg/policy/k8s/cilium_network_policy.go` (라인 127-169)

```go
func (p *policyWatcher) upsertCiliumNetworkPolicyV2(cnp *types.SlimCNP,
    initialRecvTime time.Time, resourceID ipcacheTypes.ResourceID, dc chan uint64) error {

    // CNP를 파싱하여 rules 생성
    rules, err := cnp.Parse(scopedLog,
        cmtypes.LocalClusterNameForPolicies(p.clusterMeshPolicyConfig, p.config.ClusterName))
    if err != nil {
        return fmt.Errorf("failed to parse CiliumNetworkPolicy: %w", err)
    }

    // PolicyImporter로 전달
    p.policyImporter.UpdatePolicy(&policytypes.PolicyUpdate{
        Rules:               policyutils.RulesToPolicyEntries(rules),
        Source:              source.CustomResource,
        ProcessingStartTime: initialRecvTime,
        Resource:            resourceID,
        DoneChan:            dc,
    })
}
```

#### 단계 2: ReplaceByResource

> 파일: `pkg/policy/repository.go` (라인 573-614)

```go
func (p *Repository) ReplaceByResource(rules types.PolicyEntries,
    resource ipcachetypes.ResourceID) (
    affectedIDs *set.Set[identity.NumericIdentity], rev uint64, oldRuleCnt int) {

    p.mutex.Lock()
    defer p.mutex.Unlock()

    affectedIDs = &set.Set[identity.NumericIdentity]{}

    // 1) 기존 규칙 삭제
    oldRules := maps.Clone(p.rulesByResource[resource])
    for key, oldRule := range oldRules {
        for _, subj := range oldRule.getSubjects() {
            affectedIDs.Insert(subj)  // 영향받는 아이덴티티 추적
        }
        p.del(key)
    }

    // 2) 새 규칙 삽입
    if len(rules) > 0 {
        p.rulesByResource[resource] = make(map[ruleKey]*rule, len(rules))
        for i, r := range rules {
            newRule := p.newRule(*r, ruleKey{resource: resource, idx: uint(i)})
            p.insert(newRule)
            for _, subj := range newRule.getSubjects() {
                affectedIDs.Insert(subj)
            }
        }
    }

    // 3) 기존 규칙의 셀렉터 해제
    for _, r := range oldRules {
        p.releaseRule(r)
    }

    // 4) 리비전 증가 및 반환
    return affectedIDs, p.BumpRevision(), len(oldRules)
}
```

**왜 "delete old → insert new" 순서인가?**

1. 기존 규칙의 Subject가 선택하는 아이덴티티를 먼저 `affectedIDs`에 추가한다.
   이 아이덴티티들은 정책이 변경되었으므로 재생성이 필요하다.
2. 새 규칙을 삽입하고, 새 규칙의 Subject가 선택하는 아이덴티티도 추가한다.
3. 이렇게 하면 **기존 규칙에서만 선택되던 아이덴티티**도 놓치지 않는다.

기존 셀렉터 해제(`releaseRule`)는 새 규칙 삽입 후에 수행한다. 이는 기존과 새 규칙이
같은 셀렉터를 사용할 때, 셀렉터가 일시적으로 참조 카운트 0이 되어 해제되는 것을 방지한다.

#### 단계 3: 엔드포인트 재생성

반환된 `affectedIDs`를 기반으로, 해당 아이덴티티를 가진 엔드포인트가 정책 재생성을 수행한다.
재생성 과정에서 `GetSelectorPolicy()`가 호출되고, 캐시 미스 시 `resolvePolicyLocked()`로
전체 정책을 계산한다.

#### 단계 4: DistillPolicy

`SelectorPolicy`의 `DistillPolicy()`가 호출되어, 셀렉터 기반의 추상 정책을 구체적인
Key → MapStateEntry 맵으로 변환한다.

---

## 10. 증분 업데이트 메커니즘

### 10.1 왜 증분 업데이트가 필요한가?

정책 변경 없이 **아이덴티티만 변경**되는 경우가 매우 빈번하다:
- 새 Pod이 생성되어 아이덴티티 할당
- Pod이 삭제되어 아이덴티티 회수
- FQDN 해석으로 새 IP-아이덴티티 매핑 추가

이때 전체 정책을 재계산하면 O(규칙수 * 엔드포인트수)의 비용이 든다.
증분 업데이트는 영향받는 셀렉터 → 영향받는 엔드포인트만 업데이트하여
O(변경된 셀렉터 수 * 해당 엔드포인트 수)로 줄인다.

### 10.2 증분 업데이트 흐름

```
아이덴티티 변경 (Pod 생성/삭제, FQDN 해석)
        │
        v
SelectorCache.UpdateIdentities()
        │
        ├─ 1. idCache 업데이트
        ├─ 2. 영향받는 셀렉터 식별 (네임스페이스 최적화)
        ├─ 3. updateSelections() → 셀렉터별 선택 상태 변경
        ├─ 4. commit() → readTxn 갱신
        └─ 5. 사용자 알림 큐잉
                │
                v
        L4Filter (CachedSelectionUser)
                │
                ├─ IdentitySelectionUpdated() 콜백
                │   └─ 새/삭제된 아이덴티티에 대한 MapState 변경 계산
                │
                └─ policyMapChanges에 mapChange 추가
                        │
                        v
        EndpointPolicy.ConsumeMapChanges()
                │
                ├─ 변경 큐 소비
                ├─ policyMapState 업데이트
                └─ BPF 맵 증분 기록
```

### 10.3 MapChanges 구조

> 파일: `pkg/policy/mapstate.go` (라인 1611-1626)

```go
type MapChanges struct {
    logger    *slog.Logger
    firstRev  types.SelectorRevision  // 이 변경 큐의 시작 리비전
    mutex     lock.Mutex
    changes   []mapChange            // 미처리 변경 목록
    synced    []mapChange            // 동기 알림 변경 목록
    selectors SelectorSnapshot       // 최신 셀렉터 스냅샷
}

type mapChange struct {
    Add               bool              // true=추가, false=삭제
    Tier              types.Tier
    TierMaxPrecedence types.Precedence
    Key               Key               // BPF 맵 키
    Value             mapStateEntry      // BPF 맵 값
}
```

### 10.4 ConsumeMapChanges

> 파일: `pkg/policy/resolve.go` (라인 589-619)

```go
func (p *EndpointPolicy) ConsumeMapChanges() (closer func(), changes ChangeState) {
    features := p.SelectorPolicy.L4Policy.Ingress.features |
                p.SelectorPolicy.L4Policy.Egress.features
    selectors, changes := p.policyMapChanges.consumeMapChanges(p, features)

    // 셀렉터 스냅샷 업데이트
    if selectors.IsValid() {
        if p.selectors.IsValid() {
            p.selectors.Invalidate()
        } else {
            closer = func() { p.Ready() }
        }
        p.selectors = selectors
    }

    return closer, changes
}
```

**왜 closer 함수를 반환하는가?**

기존 셀렉터 스냅샷이 이미 닫혀있으면(`!IsValid()`), 새 스냅샷도 처리 후 닫아야 한다.
이 "닫기" 로직을 호출자에게 위임하여, 정확한 시점에 리소스를 해제할 수 있다.

### 10.5 전체 재계산 vs 증분 업데이트 비교

| 상황 | 전체 재계산 | 증분 업데이트 |
|------|-----------|-------------|
| CNP 추가/수정/삭제 | O | - |
| 아이덴티티 추가/삭제 | - | O |
| FQDN IP 변경 | - | O |
| 엔드포인트 첫 생성 | O | - |
| 셀렉터 선택 변경 | - | O |

전체 재계산은 `ReplaceByResource()` → `GetSelectorPolicy()` → `DistillPolicy()` 경로를
거치고, 증분 업데이트는 `UpdateIdentities()` → `MapChanges` → `ConsumeMapChanges()` 경로를
거친다.

---

## 11. FQDN 정책 통합

### 11.1 identityNotifier 인터페이스

FQDN 정책은 DNS 도메인 이름을 기반으로 트래픽을 제어한다. 문제는 IP 자체에는
도메인 정보가 없으므로, DNS 응답을 통해 IP-도메인 매핑을 학습해야 한다.

> 파일: `pkg/policy/selectorcache.go` (라인 486-499)

```go
type identityNotifier interface {
    // RegisterFQDNSelector: FQDN 셀렉터를 등록하여
    // DNS 응답이 매칭되면 해당 IP의 아이덴티티 레이블을 연관시킨다.
    RegisterFQDNSelector(selector api.FQDNSelector) (ipcacheRevision uint64)

    // UnregisterFQDNSelector: FQDN 셀렉터를 해제하여
    // 더 이상 매칭되는 셀렉터가 없으면 IP를 IPCache에서 제거할 수 있다.
    UnregisterFQDNSelector(selector api.FQDNSelector) (ipcacheRevision uint64)
}
```

### 11.2 FQDN 정책 흐름

```
1. CNP에 FQDN 규칙 작성
   toFQDNs:
     - matchName: "api.example.com"

2. 규칙 파싱 시 FQDNSelector 생성
   └─ SelectorCache에 FQDN 셀렉터 등록

3. SelectorCache.addFQDNSelector()
   └─ identityNotifier.RegisterFQDNSelector()
       └─ FQDN 서브시스템에 도메인 등록

4. DNS 프록시가 DNS 쿼리 인터셉트
   └─ "api.example.com" → 10.0.1.5 응답 학습

5. IPCache에 10.0.1.5 → CIDR 아이덴티티 등록
   └─ SelectorCache.UpdateIdentities() 호출
       └─ FQDN 셀렉터가 새 아이덴티티 선택
           └─ 증분 업데이트로 BPF 맵에 반영

6. DNS TTL 만료 시
   └─ 아이덴티티 삭제 → UpdateIdentities()
       └─ BPF 맵에서 제거
```

**왜 SelectorCache에 FQDN을 통합하는가?**

FQDN 해석 결과를 별도의 메커니즘으로 BPF 맵에 반영하면, 일반 정책과 FQDN 정책
사이의 일관성을 보장하기 어렵다. SelectorCache를 통해 통합하면:

1. FQDN 셀렉터도 일반 셀렉터와 동일한 증분 업데이트 메커니즘을 탈 수 있다.
2. FQDN 정책과 레이블 정책이 같은 엔드포인트에 적용될 때 자연스럽게 병합된다.
3. Precedence 로직이 FQDN과 일반 정책 간에도 일관되게 적용된다.

### 11.3 SetLocalIdentityNotifier

> 파일: `pkg/policy/selectorcache.go` (라인 464-471)

```go
// SetLocalIdentityNotifier는 FQDN 서브시스템을 SelectorCache에 주입한다.
// FQDN 서브시스템은 FQDNSelector에 매칭되는 DNS 응답의 IP에 대해
// CIDR 아이덴티티를 생성/관리한다.
func (sc *SelectorCache) SetLocalIdentityNotifier(pop identityNotifier) {
    sc.localIdentityNotifier = pop
}
```

이 의존성 주입 패턴은 SelectorCache가 FQDN 서브시스템에 직접 의존하지 않으면서도
FQDN 셀렉터를 지원할 수 있게 한다.

---

## 12. Precedence(우선순위) 인코딩

### 12.1 Precedence 비트 레이아웃

> 파일: `pkg/policy/types/entry.go` (라인 15-36)

Precedence는 32비트 정수로, 높은 값이 높은 우선순위를 가진다.

```
Precedence (32비트):
┌────────────────────────────┬──────────────────┐
│ 상위 24비트                 │ 하위 8비트        │
│ (반전된 Priority)          │ (Verdict 유형)    │
│                            │                  │
│ LowestPriority - priority  │ 255 = Deny       │
│                            │ 2-254 = 프록시     │
│                            │ 리다이렉트 우선순위 │
│                            │ 1 = Allow        │
│                            │ 0 = Pass         │
└────────────────────────────┴──────────────────┘
```

핵심 상수:

```go
const (
    precedencePriorityShift = 8        // Priority는 8비트 시프트
    precedencePriorityBits  = 24       // Priority는 24비트 사용

    precedenceByteDeny  Precedence = 255  // Deny
    precedenceByteAllow Precedence = 1    // Allow (프록시 없음)
    precedenceBytePass  Precedence = 0    // Pass
)
```

### 12.2 Priority 변환 예시

```
API Priority = 0 (최우선):
  BasePrecedence = (LowestPriority - 0) << 8
                 = 0x00FFFFFF << 8
                 = 0xFFFFFF00

  Deny:  0xFFFFFF00 + 0xFF = 0xFFFFFFFF (최대값)
  Allow: 0xFFFFFF00 + 0x01 = 0xFFFFFF01
  Pass:  0xFFFFFF00 + 0x00 = 0xFFFFFF00

API Priority = 100:
  BasePrecedence = (0x00FFFFFF - 100) << 8
                 = 0x00FFFF9B << 8
                 = 0xFFFF9B00

  Deny:  0xFFFF9B00 + 0xFF = 0xFFFF9BFF
  Allow: 0xFFFF9B00 + 0x01 = 0xFFFF9B01
```

**왜 Priority를 반전시키는가?**

API에서는 낮은 숫자가 높은 우선순위(Priority 0 > Priority 100)이지만,
BPF 맵에서는 높은 Precedence 값이 우선이다. 반전을 통해 API의 의미론과
데이터패스의 비교 연산을 일치시킨다.

### 12.3 리스너 우선순위 인코딩

> 파일: `pkg/policy/types/entry.go` (라인 67-96)

```go
func (p Precedence) WithListenerPriority(priority ListenerPriority) Precedence {
    p &= ^precedenceByteMask   // 하위 8비트 클리어
    p |= precedenceByteAllow    // 기본 Allow 비트 설정

    if priority > 0 {
        // 반전: priority 1 → 254, priority 100 → 155, priority 126 → 129
        p += precedenceByteMask - 1 - Precedence(priority)
    } else {
        p += 1  // 명시적 우선순위 없는 프록시 포트
    }
    return p
}
```

하위 8비트의 값 매핑:

| 값 | 의미 |
|----|------|
| 255 | Deny |
| 254 | Listener Priority 1 (최우선) |
| 155 | Listener Priority 100 |
| 129 | Listener Priority 126 (최저 비기본) |
| 2 | 명시적 우선순위 없는 프록시 포트 |
| 1 | Allow (프록시 리다이렉트 없음) |
| 0 | Pass |

### 12.4 Precedence 비교 로직

```
규칙 A: Priority=0, Deny    → Precedence = 0xFFFFFFFF
규칙 B: Priority=0, Allow   → Precedence = 0xFFFFFF01
규칙 C: Priority=100, Deny  → Precedence = 0xFFFF9BFF
규칙 D: Priority=100, Allow → Precedence = 0xFFFF9B01

비교: A > C > B > D

즉, 같은 Priority에서 Deny > Allow
다른 Priority에서는 높은 Priority(낮은 숫자)가 항상 우선
```

### 12.5 엔트리 병합 (Merge)

> 파일: `pkg/policy/types/entry.go` (라인 329-354)

같은 Key에 대해 여러 엔트리가 존재할 때 병합 규칙:

```go
func (e *MapStateEntry) Merge(entry MapStateEntry) {
    if e.IsAllow() && entry.IsAllow() {
        // 프록시 포트: 더 높은 Precedence(리스너 우선순위) 선택
        if entry.IsRedirectEntry() {
            if entry.Precedence > e.Precedence ||
               (entry.Precedence == e.Precedence && entry.ProxyPort < e.ProxyPort) {
                e.ProxyPort = entry.ProxyPort
                e.Precedence = entry.Precedence
            }
        }

        // 인증: explicit > derived, 같은 유형이면 높은 값 선택
        if entry.AuthRequirement.IsExplicit() == e.AuthRequirement.IsExplicit() {
            if entry.AuthRequirement > e.AuthRequirement {
                e.AuthRequirement = entry.AuthRequirement
            }
        } else if entry.AuthRequirement.IsExplicit() {
            e.AuthRequirement = entry.AuthRequirement
        }
    }
}
```

병합은 **같은 Priority의 Allow 엔트리 간에만** 발생한다:
- Deny가 있으면 Deny가 항상 우선
- 프록시 리다이렉트는 리스너 우선순위가 높은 것 선택
- 인증 요구사항은 명시적 지정이 파생된 것보다 우선

---

## 13. 왜 이 아키텍처인가?

### 13.1 다단계 캐싱의 필요성

```
                    변경 빈도    비용
CRD 변경            낮음        높음 (전체 재계산)
아이덴티티 변경      높음        중간 (증분 업데이트)
BPF 맵 기록         매우 높음    낮음 (단일 엔트리)
```

변경 빈도가 높은 계층(아이덴티티)에서 비용이 높은 연산(전체 재계산)을 피하기 위해,
중간 캐시 계층(SelectorCache, policyCache)을 두어 증분 업데이트를 가능하게 한다.

### 13.2 MVCC 스냅샷의 일관성 보장

락 없는 읽기를 위해 `atomic.Pointer[SelectorSnapshot]`을 사용하는 것은 단순한 성능
최적화가 아니다. 정책 계산 중에 셀렉터 상태가 바뀌면 **부분적으로 올바른** 정책이
생성될 수 있다. 스냅샷은 이를 방지한다.

### 13.3 네임스페이스 인덱싱의 확장성

10,000개의 규칙이 100개의 네임스페이스에 고르게 분포하면:
- 인덱스 없이: 10,000 + 10,000 = 20,000번 매칭
- 인덱스 있으면: 0(클러스터와이드) + 100(네임스페이스) = 100번 매칭

이 최적화는 대규모 클러스터에서 정책 계산 시간을 **두 자릿수** 줄인다.

### 13.4 리소스별 원자적 교체

`ReplaceByResource()`는 "전체 삭제 후 전체 삽입"이 아니라 "리소스별 원자적 교체"를
수행한다. 이를 통해:
- 다른 리소스의 규칙에 영향을 주지 않는다
- 셀렉터 참조 카운트가 일시적으로 0이 되는 것을 방지한다
- `affectedIDs`를 정확하게 계산할 수 있다

### 13.5 Precedence 인코딩의 효율성

32비트 단일 정수에 Priority, Verdict 유형, 리스너 우선순위를 모두 인코딩함으로써:
- BPF 데이터패스에서 단일 정수 비교로 우선순위 판단
- 사용자 공간에서도 간단한 비교 연산으로 충돌 해소
- 추가 메모리 없이 풍부한 의미론 표현

### 13.6 Subject와 Peer 분리의 설계 근거

Cilium은 `subjectSelectorCache`와 `selectorCache` 두 개의 SelectorCache를 유지한다:
- `subjectSelectorCache`: 규칙이 **어떤 엔드포인트에 적용되는지** (Subject)
- `selectorCache`: 규칙이 **어떤 피어를 허용하는지** (L3 셀렉터)

이 분리의 이유:
1. Subject 셀렉터는 규칙 매칭에만 사용되고, 피어 셀렉터는 BPF 맵 생성에 사용된다.
   라이프사이클이 다르다.
2. Subject 변경 시 전체 재계산이 필요하지만, 피어 변경은 증분 업데이트가 가능하다.
3. 두 캐시의 업데이트 빈도와 패턴이 다르므로 분리하면 각각 최적화할 수 있다.

---

## 14. 참고 파일 목록

| 파일 | 주요 내용 |
|------|----------|
| `pkg/policy/repository.go` | PolicyRepository 인터페이스, Repository 구조체, ReplaceByResource, GetSelectorPolicy, resolvePolicyLocked, computePolicyEnforcementAndRules |
| `pkg/policy/selectorcache.go` | SelectorCache 구조체, scIdentityCache, UpdateIdentities, identityNotifier 인터페이스, MVCC commit |
| `pkg/policy/l4.go` | L4Policy, L4DirectionPolicy, L4Filter, PerSelectorPolicy, L7ParserType, ListenerPriority |
| `pkg/policy/resolve.go` | PolicyContext 인터페이스, selectorPolicy, EndpointPolicy, DistillPolicy, ConsumeMapChanges |
| `pkg/policy/types/entry.go` | MapStateEntry, Precedence 인코딩, Priority 변환, NewMapStateEntry, Merge |
| `pkg/policy/types/types.go` | Key, LPMKey 구조체, TrafficDirection 인코딩 |
| `pkg/policy/types/policyentry.go` | PolicyEntry(원천 규칙 데이터), Tier, Verdict |
| `pkg/policy/rule.go` | rule 구조체, subjectSelector, CachedSelectionUser 구현 |
| `pkg/policy/distillery.go` | policyCache 구조체, updateSelectorPolicy, cachedSelectorPolicy |
| `pkg/policy/mapstate.go` | MapChanges, mapChange, mapState, 증분 변경 큐 |
| `pkg/maps/policymap/policymap.go` | BPF PolicyMap, PolicyKey, PolicyEntry, LPM prefix 구조 |
| `pkg/policy/k8s/cilium_network_policy.go` | policyWatcher, upsertCiliumNetworkPolicyV2, CNP 파싱 → PolicyImporter |

모든 파일 경로는 Cilium 소스코드 루트(`/Users/ywlee/CNCF/cilium/`) 기준이다.
