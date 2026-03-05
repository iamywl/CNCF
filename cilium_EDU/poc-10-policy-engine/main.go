package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// =============================================================================
// Cilium 정책 엔진 시뮬레이션
// =============================================================================
//
// 실제 소스 참조:
//   - pkg/policy/repository.go       : Repository (규칙 저장, resolvePolicyLocked)
//   - pkg/policy/selectorcache.go    : SelectorCache (라벨→Identity 매핑, 선택 캐시)
//   - pkg/policy/mapstate.go         : mapState (bitlpm.Trie 기반 PolicyMap)
//   - pkg/policy/l4.go               : L4Filter, L4Policy (L3/L4 정책 매칭)
//   - pkg/policy/api/rule.go         : Rule (CiliumNetworkPolicy 정의)
//   - pkg/policy/api/selector.go     : EndpointSelector (라벨 셀렉터)
//   - pkg/identity/                  : NumericIdentity (라벨 → 숫자 ID)
//
// 핵심 흐름:
//   CiliumNetworkPolicy → Repository → resolvePolicyLocked()
//   → computePolicyEnforcementAndRules() → resolveL4Policy()
//   → mapState (LPM Trie) → BPF policymap

// =============================================================================
// 1. Identity 모델 — pkg/identity/
// =============================================================================

// NumericIdentity는 보안 Identity의 숫자 표현
// 실제: pkg/identity/numeric_identity.go — 라벨 집합을 하나의 숫자로 매핑
// 예약된 Identity: 1(host), 2(world), 3(unmanaged), 4(health), 5(init), 6(kube-apiserver)
type NumericIdentity uint32

const (
	IdentityHost     NumericIdentity = 1 // 호스트 Identity
	IdentityWorld    NumericIdentity = 2 // 외부 세계 Identity
	IdentityInit     NumericIdentity = 5 // 초기화 중인 엔드포인트
	IdentityMinAlloc NumericIdentity = 256 // 사용자 할당 Identity 시작값
)

// Label은 k8s 라벨을 나타냄
// 실제: pkg/labels/labels.go — Source:Key=Value 형태
type Label struct {
	Source string // "k8s", "reserved" 등
	Key    string
	Value  string
}

func (l Label) String() string {
	if l.Source != "" {
		return fmt.Sprintf("%s:%s=%s", l.Source, l.Key, l.Value)
	}
	return fmt.Sprintf("%s=%s", l.Key, l.Value)
}

// LabelArray는 정렬된 라벨 배열
type LabelArray []Label

func (la LabelArray) String() string {
	parts := make([]string, len(la))
	for i, l := range la {
		parts[i] = l.String()
	}
	return strings.Join(parts, ",")
}

// Has는 특정 키의 라벨이 있는지 확인
func (la LabelArray) Has(key string) bool {
	for _, l := range la {
		if l.Key == key {
			return true
		}
	}
	return false
}

// Get은 특정 키의 값을 반환
func (la LabelArray) Get(key string) string {
	for _, l := range la {
		if l.Key == key {
			return l.Value
		}
	}
	return ""
}

// Identity는 보안 Identity — 라벨 집합의 숫자 표현
// 실제: pkg/identity/identity.go
type Identity struct {
	ID     NumericIdentity
	Labels LabelArray
}

// =============================================================================
// 2. EndpointSelector — pkg/policy/api/selector.go 재현
// =============================================================================

// EndpointSelector는 엔드포인트를 선택하는 라벨 셀렉터
// 실제: pkg/policy/api/selector.go의 EndpointSelector
// matchLabels: 정확히 일치해야 하는 라벨들
type EndpointSelector struct {
	MatchLabels map[string]string
}

// Matches는 라벨 배열이 이 셀렉터와 일치하는지 확인
// 실제: 내부적으로 k8s LabelSelector.Matches()를 사용
func (es EndpointSelector) Matches(labels LabelArray) bool {
	for key, value := range es.MatchLabels {
		found := false
		for _, l := range labels {
			if l.Key == key && l.Value == value {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// Key는 셀렉터의 고유 키를 반환
func (es EndpointSelector) Key() string {
	keys := make([]string, 0, len(es.MatchLabels))
	for k := range es.MatchLabels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, es.MatchLabels[k]))
	}
	return strings.Join(parts, ",")
}

func (es EndpointSelector) String() string {
	return es.Key()
}

// IsWildcard는 셀렉터가 모든 것을 선택하는지 확인
func (es EndpointSelector) IsWildcard() bool {
	return len(es.MatchLabels) == 0
}

// WildcardEndpointSelector는 모든 엔드포인트를 선택하는 셀렉터
var WildcardEndpointSelector = EndpointSelector{MatchLabels: map[string]string{}}

// =============================================================================
// 3. SelectorCache — pkg/policy/selectorcache.go 재현
// =============================================================================
//
// SelectorCache는 다음을 캐싱:
//   1. 모든 알려진 Identity (idCache: NID → labels)
//   2. 사용 중인 셀렉터 (selectors: key → identitySelector)
//   3. 각 셀렉터가 선택하는 Identity 집합 (cachedSelections)
//
// Identity 업데이트 시:
//   UpdateIdentities(added, deleted) → 모든 셀렉터의 선택 집합 재계산

// scIdentity는 SelectorCache가 관리하는 Identity 정보
// 실제: pkg/policy/selectorcache.go의 scIdentity
type scIdentity struct {
	NID       NumericIdentity
	Labels    LabelArray
	Namespace string
}

// scIdentityCache는 Identity 캐시 (NID → scIdentity)
// 실제: pkg/policy/selectorcache.go의 scIdentityCache
type scIdentityCache struct {
	ids         map[NumericIdentity]*scIdentity
	byNamespace map[string]map[*scIdentity]struct{}
}

func newScIdentityCache() *scIdentityCache {
	return &scIdentityCache{
		ids:         make(map[NumericIdentity]*scIdentity),
		byNamespace: make(map[string]map[*scIdentity]struct{}),
	}
}

func (c *scIdentityCache) insert(nid NumericIdentity, labels LabelArray) *scIdentity {
	ns := labels.Get("namespace")
	id := &scIdentity{NID: nid, Labels: labels, Namespace: ns}
	c.ids[nid] = id
	if c.byNamespace[ns] == nil {
		c.byNamespace[ns] = make(map[*scIdentity]struct{})
	}
	c.byNamespace[ns][id] = struct{}{}
	return id
}

func (c *scIdentityCache) delete(nid NumericIdentity) {
	id, exists := c.ids[nid]
	if !exists {
		return
	}
	if m := c.byNamespace[id.Namespace]; m != nil {
		delete(m, id)
		if len(m) == 0 {
			delete(c.byNamespace, id.Namespace)
		}
	}
	delete(c.ids, nid)
}

// identitySelector는 캐싱된 셀렉터 + 매칭 Identity 집합
// 실제: pkg/policy/selectorcache_selector.go의 identitySelector
type identitySelector struct {
	key              string
	source           EndpointSelector
	cachedSelections map[NumericIdentity]struct{}
	users            int // 이 셀렉터를 사용하는 정책 수
}

func newIdentitySelector(key string, source EndpointSelector) *identitySelector {
	return &identitySelector{
		key:              key,
		source:           source,
		cachedSelections: make(map[NumericIdentity]struct{}),
	}
}

func (is *identitySelector) GetSelections() []NumericIdentity {
	result := make([]NumericIdentity, 0, len(is.cachedSelections))
	for nid := range is.cachedSelections {
		result = append(result, nid)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

// SelectorCache는 Identity와 셀렉터를 캐싱
// 실제: pkg/policy/selectorcache.go의 SelectorCache
type SelectorCache struct {
	mu        sync.RWMutex
	revision  uint64
	idCache   *scIdentityCache
	selectors map[string]*identitySelector
}

func NewSelectorCache() *SelectorCache {
	return &SelectorCache{
		idCache:   newScIdentityCache(),
		selectors: make(map[string]*identitySelector),
	}
}

// AddSelector는 셀렉터를 캐시에 추가하고, 현재 알려진 Identity와 매칭
// 실제: addSelectorLocked() — 기존 idCache를 스캔하여 cachedSelections 초기화
func (sc *SelectorCache) AddSelector(selector EndpointSelector) *identitySelector {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	key := selector.Key()
	if sel, exists := sc.selectors[key]; exists {
		sel.users++
		return sel
	}

	sel := newIdentitySelector(key, selector)
	sel.users = 1

	// 모든 알려진 Identity에 대해 매칭 확인
	// 실제: for nid := range sc.idCache.selections(sel) { ... }
	for nid, id := range sc.idCache.ids {
		if selector.Matches(id.Labels) {
			sel.cachedSelections[nid] = struct{}{}
		}
	}

	sc.selectors[key] = sel
	return sel
}

// UpdateIdentities는 Identity 추가/삭제를 전파
// 실제: pkg/policy/selectorcache.go의 UpdateIdentities()
// 반환값: 변경된 셀렉터 목록
func (sc *SelectorCache) UpdateIdentities(added map[NumericIdentity]LabelArray, deleted map[NumericIdentity]struct{}) []string {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	var changedSelectors []string

	// 삭제 처리
	for nid := range deleted {
		sc.idCache.delete(nid)
		// 모든 셀렉터에서 이 Identity 제거
		for key, sel := range sc.selectors {
			if _, exists := sel.cachedSelections[nid]; exists {
				delete(sel.cachedSelections, nid)
				changedSelectors = append(changedSelectors, key)
			}
		}
	}

	// 추가 처리
	for nid, labels := range added {
		sc.idCache.insert(nid, labels)
		// 매칭하는 셀렉터에 추가
		for key, sel := range sc.selectors {
			if sel.source.Matches(labels) {
				if _, exists := sel.cachedSelections[nid]; !exists {
					sel.cachedSelections[nid] = struct{}{}
					changedSelectors = append(changedSelectors, key)
				}
			}
		}
	}

	sc.revision++
	return changedSelectors
}

// =============================================================================
// 4. Policy Rule — pkg/policy/api/rule.go 재현
// =============================================================================

// Verdict는 정책 결과 (Allow/Deny)
type Verdict uint8

const (
	VerdictAllow Verdict = iota
	VerdictDeny
)

func (v Verdict) String() string {
	if v == VerdictAllow {
		return "ALLOW"
	}
	return "DENY"
}

// TrafficDirection은 트래픽 방향
type TrafficDirection uint8

const (
	Ingress TrafficDirection = iota
	Egress
)

func (d TrafficDirection) String() string {
	if d == Ingress {
		return "Ingress"
	}
	return "Egress"
}

// PortRule은 L4 포트 규칙
type PortRule struct {
	Port     uint16
	Protocol string // "TCP", "UDP", "ANY"
}

// PolicyRule은 CiliumNetworkPolicy 규칙
// 실제: pkg/policy/api/rule.go의 Rule + pkg/policy/types/policy_entry.go
type PolicyRule struct {
	Name         string
	Subject      EndpointSelector // 이 규칙이 적용되는 엔드포인트
	Direction    TrafficDirection // Ingress 또는 Egress
	Verdict      Verdict          // Allow 또는 Deny
	Peers        []EndpointSelector // 허용/거부할 피어
	Ports        []PortRule        // L4 포트 규칙 (비어있으면 ANY)
	DefaultDeny  bool              // 이 규칙이 default deny를 활성화하는지
}

// =============================================================================
// 5. PolicyMap (LPM Trie) — pkg/policy/mapstate.go 재현
// =============================================================================
//
// 실제: pkg/policy/mapstate.go의 mapState
// BPF 맵에서는 LPM(Longest Prefix Match) Trie로 구현
// 키: (TrafficDirection, Identity, Protocol, Port)
// 값: (Verdict, ProxyPort 등)
//
// LPM 매칭 순서:
//   1. 가장 구체적인 매치 (specific identity + specific port)
//   2. Identity만 매치 (any port)
//   3. Port만 매치 (any identity)
//   4. 와일드카드 매치 (any identity + any port)

// PolicyMapKey는 PolicyMap의 키
// 실제: pkg/policy/types/key.go의 Key
type PolicyMapKey struct {
	Direction TrafficDirection
	Identity  NumericIdentity
	Protocol  uint8  // 0=ANY, 6=TCP, 17=UDP
	Port      uint16 // 0=ANY
}

func (k PolicyMapKey) String() string {
	proto := "ANY"
	switch k.Protocol {
	case 6:
		proto = "TCP"
	case 17:
		proto = "UDP"
	}
	port := "ANY"
	if k.Port > 0 {
		port = fmt.Sprintf("%d", k.Port)
	}
	id := "ANY"
	if k.Identity > 0 {
		id = fmt.Sprintf("%d", k.Identity)
	}
	return fmt.Sprintf("%s id=%s proto=%s port=%s", k.Direction, id, proto, port)
}

// specificity는 LPM 매칭 우선순위를 반환 (높을수록 구체적)
func (k PolicyMapKey) specificity() int {
	s := 0
	if k.Identity > 0 {
		s += 4
	}
	if k.Protocol > 0 {
		s += 2
	}
	if k.Port > 0 {
		s += 1
	}
	return s
}

// PolicyMapEntry는 PolicyMap의 값
type PolicyMapEntry struct {
	Verdict   Verdict
	ProxyPort uint16 // L7 프록시 포트 (0이면 프록시 없음)
	AuthType  uint8  // 인증 유형 (0=none)
}

// PolicyMap은 LPM Trie 기반 정책 맵
// 실제: pkg/policy/mapstate.go — bitlpm.Trie로 구현
// 여기서는 간단한 맵 + LPM 매칭 로직으로 시뮬레이션
type PolicyMap struct {
	entries map[PolicyMapKey]PolicyMapEntry
}

func NewPolicyMap() *PolicyMap {
	return &PolicyMap{entries: make(map[PolicyMapKey]PolicyMapEntry)}
}

// Insert는 정책 엔트리를 추가
func (pm *PolicyMap) Insert(key PolicyMapKey, entry PolicyMapEntry) {
	pm.entries[key] = entry
}

// Lookup은 LPM 매칭으로 정책을 조회
// 실제: bitlpm.Trie에서 longest prefix match
// 매칭 순서: specific > wildcard (Identity > Protocol > Port)
func (pm *PolicyMap) Lookup(direction TrafficDirection, identity NumericIdentity, protocol uint8, port uint16) (PolicyMapEntry, bool) {
	// 가장 구체적인 매치부터 시도
	candidates := []PolicyMapKey{
		{Direction: direction, Identity: identity, Protocol: protocol, Port: port},
		{Direction: direction, Identity: identity, Protocol: protocol, Port: 0},
		{Direction: direction, Identity: identity, Protocol: 0, Port: 0},
		{Direction: direction, Identity: 0, Protocol: protocol, Port: port},
		{Direction: direction, Identity: 0, Protocol: protocol, Port: 0},
		{Direction: direction, Identity: 0, Protocol: 0, Port: 0},
	}

	for _, key := range candidates {
		if entry, exists := pm.entries[key]; exists {
			return entry, true
		}
	}
	return PolicyMapEntry{}, false
}

// Size는 엔트리 수를 반환
func (pm *PolicyMap) Size() int {
	return len(pm.entries)
}

// =============================================================================
// 6. Policy Repository — pkg/policy/repository.go 재현
// =============================================================================

// Repository는 정책 규칙 저장소
// 실제: pkg/policy/repository.go의 Repository
type Repository struct {
	mu            sync.RWMutex
	rules         []*PolicyRule
	selectorCache *SelectorCache
	revision      uint64
}

func NewRepository(sc *SelectorCache) *Repository {
	return &Repository{
		selectorCache: sc,
		revision:      1,
	}
}

// AddRule은 정책 규칙을 추가
func (r *Repository) AddRule(rule *PolicyRule) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rules = append(r.rules, rule)
	r.revision++
}

// ResolvePolicy는 주어진 Identity에 대한 정책을 계산
// 실제: resolvePolicyLocked() → computePolicyEnforcementAndRules() → resolveL4Policy()
// 반환: 해당 Identity의 PolicyMap
func (r *Repository) ResolvePolicy(identity *Identity) *PolicyMap {
	r.mu.RLock()
	defer r.mu.RUnlock()

	pm := NewPolicyMap()

	// 1. 이 Identity에 적용되는 규칙 찾기
	// 실제: computePolicyEnforcementAndRules()에서 matchesSubject() 호출
	var ingressRules, egressRules []*PolicyRule
	for _, rule := range r.rules {
		if rule.Subject.Matches(identity.Labels) {
			if rule.Direction == Ingress {
				ingressRules = append(ingressRules, rule)
			} else {
				egressRules = append(egressRules, rule)
			}
		}
	}

	// 2. 매칭 규칙이 있으면 default deny 적용
	// 실제: hasIngressDefaultDeny, hasEgressDefaultDeny 계산
	if len(ingressRules) > 0 {
		// Default deny for ingress (identity=0, port=0 → DENY)
		for _, rule := range ingressRules {
			if rule.DefaultDeny {
				pm.Insert(PolicyMapKey{Direction: Ingress}, PolicyMapEntry{Verdict: VerdictDeny})
				break
			}
		}
	}
	if len(egressRules) > 0 {
		for _, rule := range egressRules {
			if rule.DefaultDeny {
				pm.Insert(PolicyMapKey{Direction: Egress}, PolicyMapEntry{Verdict: VerdictDeny})
				break
			}
		}
	}

	// 3. 각 규칙에서 허용/거부 엔트리 생성
	// 실제: resolveL4Policy() → L4Filter 생성
	for _, rule := range append(ingressRules, egressRules...) {
		for _, peer := range rule.Peers {
			// SelectorCache에서 이 peer가 선택하는 Identity 목록 가져오기
			sel := r.selectorCache.AddSelector(peer)
			selectedIDs := sel.GetSelections()

			if len(rule.Ports) == 0 {
				// ANY port
				for _, nid := range selectedIDs {
					pm.Insert(PolicyMapKey{
						Direction: rule.Direction,
						Identity:  nid,
					}, PolicyMapEntry{Verdict: rule.Verdict})
				}
			} else {
				for _, port := range rule.Ports {
					proto := uint8(0) // ANY
					switch port.Protocol {
					case "TCP":
						proto = 6
					case "UDP":
						proto = 17
					}
					for _, nid := range selectedIDs {
						pm.Insert(PolicyMapKey{
							Direction: rule.Direction,
							Identity:  nid,
							Protocol:  proto,
							Port:      port.Port,
						}, PolicyMapEntry{Verdict: rule.Verdict})
					}
				}
			}
		}
	}

	return pm
}

// =============================================================================
// 7. 데모 실행
// =============================================================================

func printSeparator(title string) {
	fmt.Printf("\n━━━ %s ━━━\n\n", title)
}

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║  Cilium 정책 엔진 시뮬레이션                                ║")
	fmt.Println("║  소스: pkg/policy/selectorcache.go, repository.go           ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")

	// =========================================================================
	// 데모 1: Identity 기반 정책 모델
	// =========================================================================
	printSeparator("데모 1: Identity 기반 정책 모델")

	sc := NewSelectorCache()

	// Identity 등록 (실제: IPCache → IdentityAllocator → SelectorCache.UpdateIdentities)
	identities := map[NumericIdentity]LabelArray{
		1000: {{Source: "k8s", Key: "app", Value: "web"}, {Source: "k8s", Key: "namespace", Value: "default"}},
		1001: {{Source: "k8s", Key: "app", Value: "web"}, {Source: "k8s", Key: "namespace", Value: "default"}, {Source: "k8s", Key: "version", Value: "v2"}},
		2000: {{Source: "k8s", Key: "app", Value: "api"}, {Source: "k8s", Key: "namespace", Value: "default"}},
		3000: {{Source: "k8s", Key: "app", Value: "db"}, {Source: "k8s", Key: "namespace", Value: "backend"}},
		4000: {{Source: "k8s", Key: "app", Value: "cache"}, {Source: "k8s", Key: "namespace", Value: "backend"}},
	}

	added := make(map[NumericIdentity]LabelArray)
	for nid, labels := range identities {
		added[nid] = labels
	}
	sc.UpdateIdentities(added, nil)

	fmt.Println("  등록된 Identity:")
	for nid, labels := range identities {
		fmt.Printf("    ID=%d: %s\n", nid, labels)
	}

	// =========================================================================
	// 데모 2: SelectorCache 동작
	// =========================================================================
	printSeparator("데모 2: SelectorCache 동작")

	// 셀렉터 추가 → 매칭 Identity 자동 계산
	webSelector := EndpointSelector{MatchLabels: map[string]string{"app": "web"}}
	apiSelector := EndpointSelector{MatchLabels: map[string]string{"app": "api"}}
	dbSelector := EndpointSelector{MatchLabels: map[string]string{"app": "db"}}
	wildcardSel := WildcardEndpointSelector

	selWeb := sc.AddSelector(webSelector)
	selAPI := sc.AddSelector(apiSelector)
	selDB := sc.AddSelector(dbSelector)
	selAll := sc.AddSelector(wildcardSel)

	fmt.Println("  셀렉터 → 선택된 Identity:")
	fmt.Printf("    app=web     → %v\n", selWeb.GetSelections())
	fmt.Printf("    app=api     → %v\n", selAPI.GetSelections())
	fmt.Printf("    app=db      → %v\n", selDB.GetSelections())
	fmt.Printf("    (wildcard)  → %v\n", selAll.GetSelections())

	// Identity 추가 시 자동 업데이트
	fmt.Println("\n  [이벤트] 새 Identity 추가: ID=1002 (app=web, env=staging)")
	newID := map[NumericIdentity]LabelArray{
		1002: {{Source: "k8s", Key: "app", Value: "web"}, {Source: "k8s", Key: "env", Value: "staging"}, {Source: "k8s", Key: "namespace", Value: "default"}},
	}
	changed := sc.UpdateIdentities(newID, nil)
	fmt.Printf("    변경된 셀렉터: %v\n", changed)
	fmt.Printf("    app=web 선택: %v (1002 추가됨)\n", selWeb.GetSelections())

	// Identity 삭제
	fmt.Println("\n  [이벤트] Identity 삭제: ID=1001")
	deleted := map[NumericIdentity]struct{}{1001: {}}
	changed = sc.UpdateIdentities(nil, deleted)
	fmt.Printf("    변경된 셀렉터: %v\n", changed)
	fmt.Printf("    app=web 선택: %v (1001 제거됨)\n", selWeb.GetSelections())

	// =========================================================================
	// 데모 3: L3/L4 정책 매칭
	// =========================================================================
	printSeparator("데모 3: L3/L4 정책 매칭")

	repo := NewRepository(sc)

	// 정책 1: web 포드에 대한 인그레스 허용 (api → web, TCP/80)
	repo.AddRule(&PolicyRule{
		Name:    "allow-api-to-web",
		Subject: EndpointSelector{MatchLabels: map[string]string{"app": "web"}},
		Direction: Ingress,
		Verdict:   VerdictAllow,
		Peers:     []EndpointSelector{{MatchLabels: map[string]string{"app": "api"}}},
		Ports:     []PortRule{{Port: 80, Protocol: "TCP"}},
		DefaultDeny: true,
	})

	// 정책 2: api 포드에 대한 이그레스 허용 (api → db, TCP/5432)
	repo.AddRule(&PolicyRule{
		Name:    "allow-api-to-db",
		Subject: EndpointSelector{MatchLabels: map[string]string{"app": "api"}},
		Direction: Egress,
		Verdict:   VerdictAllow,
		Peers:     []EndpointSelector{{MatchLabels: map[string]string{"app": "db"}}},
		Ports:     []PortRule{{Port: 5432, Protocol: "TCP"}},
		DefaultDeny: true,
	})

	// 정책 3: Deny 규칙 (cache → web 차단)
	repo.AddRule(&PolicyRule{
		Name:    "deny-cache-to-web",
		Subject: EndpointSelector{MatchLabels: map[string]string{"app": "web"}},
		Direction: Ingress,
		Verdict:   VerdictDeny,
		Peers:     []EndpointSelector{{MatchLabels: map[string]string{"app": "cache"}}},
		Ports:     []PortRule{{Port: 80, Protocol: "TCP"}},
		DefaultDeny: true,
	})

	fmt.Println("  정책 규칙:")
	fmt.Println("    1. allow-api-to-web:  api → web (TCP/80) ALLOW")
	fmt.Println("    2. allow-api-to-db:   api → db (TCP/5432) ALLOW")
	fmt.Println("    3. deny-cache-to-web: cache → web (TCP/80) DENY")

	// web Identity (ID=1000)에 대한 정책 계산
	webIdentity := &Identity{
		ID:     1000,
		Labels: identities[1000],
	}

	pm := repo.ResolvePolicy(webIdentity)
	fmt.Printf("\n  web (ID=1000)의 PolicyMap (%d 엔트리):\n", pm.Size())
	for key, entry := range pm.entries {
		fmt.Printf("    [%s] → %s\n", key, entry.Verdict)
	}

	// =========================================================================
	// 데모 4: PolicyMap LPM 매칭
	// =========================================================================
	printSeparator("데모 4: PolicyMap LPM 매칭")

	fmt.Println("  트래픽 시나리오:")

	testCases := []struct {
		desc     string
		dir      TrafficDirection
		id       NumericIdentity
		proto    uint8
		port     uint16
	}{
		{"api(2000) → web:80/TCP", Ingress, 2000, 6, 80},
		{"cache(4000) → web:80/TCP", Ingress, 4000, 6, 80},
		{"db(3000) → web:80/TCP", Ingress, 3000, 6, 80},
		{"api(2000) → web:443/TCP", Ingress, 2000, 6, 443},
		{"unknown(9999) → web:80/TCP", Ingress, 9999, 6, 80},
	}

	for _, tc := range testCases {
		entry, found := pm.Lookup(tc.dir, tc.id, tc.proto, tc.port)
		result := "DEFAULT DENY"
		if found {
			result = entry.Verdict.String()
		}
		fmt.Printf("    %-35s → %s\n", tc.desc, result)
	}

	// =========================================================================
	// 데모 5: 동적 Identity 업데이트와 정책 재계산
	// =========================================================================
	printSeparator("데모 5: 동적 Identity 업데이트와 정책 재계산")

	fmt.Println("  [이벤트] 새 api 포드 배포: ID=2001 (app=api, version=v2)")
	newAPI := map[NumericIdentity]LabelArray{
		2001: {{Source: "k8s", Key: "app", Value: "api"}, {Source: "k8s", Key: "version", Value: "v2"}, {Source: "k8s", Key: "namespace", Value: "default"}},
	}
	changed = sc.UpdateIdentities(newAPI, nil)
	fmt.Printf("    변경된 셀렉터: %v\n", changed)

	// 정책 재계산
	pm2 := repo.ResolvePolicy(webIdentity)
	fmt.Printf("    web의 PolicyMap 재계산 (%d 엔트리):\n", pm2.Size())

	// 새 api(2001)도 web에 접근 가능한지 확인
	entry, found := pm2.Lookup(Ingress, 2001, 6, 80)
	result := "DEFAULT DENY"
	if found {
		result = entry.Verdict.String()
	}
	fmt.Printf("    api-v2(2001) → web:80/TCP → %s\n", result)
	fmt.Println("    → SelectorCache가 자동으로 새 Identity를 선택에 포함")

	// =========================================================================
	// 데모 6: Deny 우선순위와 충돌 해결
	// =========================================================================
	printSeparator("데모 6: Deny 우선순위와 충돌 해결")

	// Deny는 Always takes precedence
	denyRepo := NewRepository(sc)

	// Allow all ingress
	denyRepo.AddRule(&PolicyRule{
		Name:    "allow-all-to-db",
		Subject: EndpointSelector{MatchLabels: map[string]string{"app": "db"}},
		Direction: Ingress,
		Verdict:   VerdictAllow,
		Peers:     []EndpointSelector{WildcardEndpointSelector},
		DefaultDeny: true,
	})

	// Deny specific
	denyRepo.AddRule(&PolicyRule{
		Name:    "deny-cache-to-db",
		Subject: EndpointSelector{MatchLabels: map[string]string{"app": "db"}},
		Direction: Ingress,
		Verdict:   VerdictDeny,
		Peers:     []EndpointSelector{{MatchLabels: map[string]string{"app": "cache"}}},
		Ports:     []PortRule{{Port: 5432, Protocol: "TCP"}},
		DefaultDeny: true,
	})

	dbIdentity := &Identity{
		ID:     3000,
		Labels: LabelArray{{Source: "k8s", Key: "app", Value: "db"}, {Source: "k8s", Key: "namespace", Value: "backend"}},
	}
	dbPM := denyRepo.ResolvePolicy(dbIdentity)

	fmt.Println("  규칙: allow-all-to-db + deny-cache-to-db")
	fmt.Println("  db (ID=3000)의 PolicyMap:")
	for key, entry := range dbPM.entries {
		fmt.Printf("    [%s] → %s\n", key, entry.Verdict)
	}

	// LPM 매칭에서 더 구체적인 Deny가 우선
	entryAPI, _ := dbPM.Lookup(Ingress, 2000, 6, 5432)
	entryCache, foundCache := dbPM.Lookup(Ingress, 4000, 6, 5432)
	fmt.Printf("\n  api(2000) → db:5432/TCP  → %s (wildcard allow)\n", entryAPI.Verdict)
	if foundCache {
		fmt.Printf("  cache(4000) → db:5432/TCP → %s (specific deny)\n", entryCache.Verdict)
	}
	fmt.Println("  → 더 구체적인 Deny 규칙이 wildcard Allow보다 우선")

	// =========================================================================
	// 요약
	// =========================================================================
	printSeparator("요약")

	fmt.Println("  핵심 동작 흐름:")
	fmt.Println("    1. CiliumNetworkPolicy → Repository에 규칙 추가")
	fmt.Println("    2. SelectorCache가 라벨 → Identity 매핑 캐시")
	fmt.Println("    3. resolvePolicyLocked()가 Identity별 PolicyMap 계산")
	fmt.Println("    4. PolicyMap을 BPF LPM Trie로 변환")
	fmt.Println("    5. 패킷 도착 시 O(log n) LPM 매칭으로 정책 판정")
	fmt.Println()
	fmt.Println("  SelectorCache 설계 이유:")
	fmt.Println("    - Identity 변경 시 O(selectors * added) 복잡도")
	fmt.Println("    - 네임스페이스별 인덱싱으로 불필요한 매칭 최소화")
	fmt.Println("    - 실제: byNamespace 맵으로 네임스페이스 스코프 셀렉터 최적화")
	fmt.Println()
	fmt.Println("═══════════════════════════════════════════════════════════════")
}
