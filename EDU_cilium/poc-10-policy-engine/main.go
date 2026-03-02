// Cilium Policy Engine PoC
//
// 이 프로그램은 Cilium의 정책 엔진 핵심 메커니즘을 시뮬레이션한다.
// 순수 Go 표준 라이브러리만 사용하며, 실제 Cilium 코드의 설계를 따라
// 다음을 구현한다:
//
//   - PolicyRepository: 정책 규칙 저장소
//   - L3 매칭: Identity 기반 및 CIDR 기반
//   - L4 매칭: Port/Protocol
//   - L7 매칭: HTTP Path/Method
//   - Policy Distillation: 규칙 → 엔드포인트별 허용 Identity+Port 쌍
//   - FQDN 정책 시뮬레이션: DNS 조회 → IP → Identity → Allow
//   - Default Deny 동작
//   - Policy Update 흐름: 규칙 추가 → 재계산 → BPF 맵 업데이트
//
// 참고: 실제 Cilium 코드의 주요 파일:
//   - pkg/policy/repository.go: PolicyRepository
//   - pkg/policy/resolve.go: selectorPolicy, EndpointPolicy, DistillPolicy
//   - pkg/policy/distillery.go: policyCache
//   - pkg/policy/rule.go: rule, mergePortProto
//   - pkg/policy/l4.go: L4Filter, L4Policy
//   - pkg/policy/types/types.go: Key, LPMKey
//   - pkg/policy/types/entry.go: MapStateEntry, Precedence
//   - pkg/policy/types/policyentry.go: PolicyEntry, Tier, Verdict
//   - pkg/maps/policymap/policymap.go: BPF PolicyMap
//   - bpf/lib/policy.h: policy_key, policy_entry
//   - pkg/fqdn/cache.go: DNSCache

package main

import (
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// 1. 기본 타입 정의 (pkg/policy/types/ 에 대응)
// =============================================================================

// NumericIdentity는 보안 아이덴티티 (uint32).
// 실제 Cilium: pkg/identity/numeric_identity.go
type NumericIdentity uint32

const (
	IdentityUnknown    NumericIdentity = 0
	IdentityHost       NumericIdentity = 1
	IdentityWorld      NumericIdentity = 2
	IdentityInit       NumericIdentity = 5
	IdentityLocalStart NumericIdentity = 16384 // CIDR/FQDN Identity 시작
)

// TrafficDirection은 트래픽 방향을 나타낸다.
// 실제 Cilium: pkg/policy/trafficdirection/
type TrafficDirection uint8

const (
	Ingress TrafficDirection = 0
	Egress  TrafficDirection = 1
)

func (td TrafficDirection) String() string {
	if td == Ingress {
		return "Ingress"
	}
	return "Egress"
}

// Verdict는 정책 판정 결과이다.
// 실제 Cilium: pkg/policy/types/policyentry.go
type Verdict uint8

const (
	Allow Verdict = iota
	Deny
	Pass
)

func (v Verdict) String() string {
	switch v {
	case Allow:
		return "ALLOW"
	case Deny:
		return "DENY"
	case Pass:
		return "PASS"
	default:
		return "UNKNOWN"
	}
}

// Tier는 정책의 계층을 나타낸다.
// 실제 Cilium: pkg/policy/types/policyentry.go
type Tier uint8

const (
	TierAdmin    Tier = 0 // 최우선
	TierNormal   Tier = 1 // 기본
	TierBaseline Tier = 2 // 최하위
)

// Protocol은 L4 프로토콜이다.
type Protocol uint8

const (
	ProtoAny  Protocol = 0
	ProtoTCP  Protocol = 6
	ProtoUDP  Protocol = 17
	ProtoSCTP Protocol = 132
)

func (p Protocol) String() string {
	switch p {
	case ProtoTCP:
		return "TCP"
	case ProtoUDP:
		return "UDP"
	case ProtoSCTP:
		return "SCTP"
	case ProtoAny:
		return "ANY"
	default:
		return fmt.Sprintf("Proto(%d)", p)
	}
}

// =============================================================================
// 2. Policy Key & Entry (BPF 맵 구조에 대응)
// =============================================================================

// PolicyKey는 BPF 정책 맵의 키이다.
// 실제 Cilium: pkg/policy/types/types.go의 Key, pkg/maps/policymap/policymap.go의 PolicyKey
type PolicyKey struct {
	Identity         NumericIdentity
	DestPort         uint16
	Protocol         Protocol
	TrafficDirection TrafficDirection
}

func (k PolicyKey) String() string {
	return fmt.Sprintf("Identity=%d, Port=%d, Proto=%s, Dir=%s",
		k.Identity, k.DestPort, k.Protocol, k.TrafficDirection)
}

// PolicyMapEntry는 BPF 정책 맵의 값이다.
// 실제 Cilium: pkg/policy/types/entry.go의 MapStateEntry
type PolicyMapEntry struct {
	Verdict   Verdict
	ProxyPort uint16 // 0이면 프록시 리다이렉트 없음
	Priority  uint32 // 높을수록 우선
}

func (e PolicyMapEntry) String() string {
	s := e.Verdict.String()
	if e.ProxyPort > 0 {
		s += fmt.Sprintf(", ProxyPort=%d", e.ProxyPort)
	}
	return s
}

// =============================================================================
// 3. Label & Selector (pkg/labels, pkg/policy/types/selector.go 에 대응)
// =============================================================================

// Labels는 키-값 라벨 집합이다.
type Labels map[string]string

func (l Labels) String() string {
	parts := make([]string, 0, len(l))
	for k, v := range l {
		parts = append(parts, fmt.Sprintf("%s=%s", k, v))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// Matches는 요구 라벨이 모두 포함되어 있는지 확인한다.
func (l Labels) Matches(requirements Labels) bool {
	for k, v := range requirements {
		if actual, ok := l[k]; !ok || actual != v {
			return false
		}
	}
	return true
}

// EndpointSelector는 라벨 기반으로 엔드포인트를 선택한다.
// 실제 Cilium: pkg/policy/api/selector.go
type EndpointSelector struct {
	MatchLabels Labels
}

func (s EndpointSelector) Matches(labels Labels) bool {
	if len(s.MatchLabels) == 0 {
		return true // 와일드카드: 모든 엔드포인트 선택
	}
	return labels.Matches(s.MatchLabels)
}

func (s EndpointSelector) String() string {
	if len(s.MatchLabels) == 0 {
		return "*"
	}
	return s.MatchLabels.String()
}

// =============================================================================
// 4. CIDR 매칭
// =============================================================================

// CIDRSelector는 CIDR 기반 셀렉터이다.
// 실제 Cilium: pkg/policy/types/selector.go의 CIDRSelector
type CIDRSelector struct {
	CIDR string
	net  *net.IPNet
}

func NewCIDRSelector(cidr string) *CIDRSelector {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		panic(fmt.Sprintf("invalid CIDR: %s", cidr))
	}
	return &CIDRSelector{CIDR: cidr, net: ipNet}
}

func (c *CIDRSelector) Contains(ip net.IP) bool {
	return c.net.Contains(ip)
}

// =============================================================================
// 5. L7 규칙 (pkg/policy/api/http.go, l4.go의 L7Rules 에 대응)
// =============================================================================

// HTTPRule은 L7 HTTP 정책 규칙이다.
// 실제 Cilium: pkg/policy/api/http.go의 PortRuleHTTP
type HTTPRule struct {
	Method string // POSIX regex
	Path   string // POSIX regex
}

func (r HTTPRule) Matches(method, path string) bool {
	if r.Method != "" {
		matched, _ := regexp.MatchString("^"+r.Method+"$", method)
		if !matched {
			return false
		}
	}
	if r.Path != "" {
		matched, _ := regexp.MatchString("^"+r.Path+"$", path)
		if !matched {
			return false
		}
	}
	return true
}

// L7Rules는 L7 규칙의 집합이다.
// 실제 Cilium: pkg/policy/api/l4.go의 L7Rules
type L7Rules struct {
	HTTP []HTTPRule
	DNS  []string // 도메인 패턴 (matchName/matchPattern)
}

func (r L7Rules) IsEmpty() bool {
	return len(r.HTTP) == 0 && len(r.DNS) == 0
}

// =============================================================================
// 6. PortRule (pkg/policy/api/l4.go의 PortRule 에 대응)
// =============================================================================

// PortRule은 L4 포트 및 L7 규칙의 조합이다.
// 실제 Cilium: pkg/policy/api/l4.go의 PortRule
type PortRule struct {
	Port     uint16
	Protocol Protocol
	L7       L7Rules
}

// =============================================================================
// 7. PolicyRule (전체 규칙, pkg/policy/types/policyentry.go의 PolicyEntry에 대응)
// =============================================================================

// PolicyRule은 하나의 완전한 정책 규칙이다.
type PolicyRule struct {
	Name     string
	Tier     Tier
	Priority float64

	// Subject: 이 규칙이 적용되는 대상 엔드포인트
	Subject EndpointSelector

	// Direction
	Direction TrafficDirection
	Verdict   Verdict

	// L3: 허용/차단할 피어 (Identity 기반)
	FromEndpoints []EndpointSelector // Ingress
	ToEndpoints   []EndpointSelector // Egress
	FromCIDR      []*CIDRSelector    // Ingress
	ToCIDR        []*CIDRSelector    // Egress
	ToFQDN        []string           // Egress FQDN

	// L4: 포트/프로토콜
	Ports []PortRule

	// DefaultDeny: true이면 이 규칙이 적용된 엔드포인트에 default deny 활성화
	DefaultDeny bool
}

// =============================================================================
// 8. Endpoint (엔드포인트)
// =============================================================================

// Endpoint는 정책이 적용되는 네트워크 엔드포인트 (Pod)이다.
type Endpoint struct {
	ID       uint64
	Identity NumericIdentity
	Labels   Labels
	IP       net.IP
}

// =============================================================================
// 9. Identity Allocator (아이덴티티 할당기)
// =============================================================================

// IdentityAllocator는 라벨 조합에 대한 아이덴티티를 할당한다.
// 실제 Cilium: pkg/identity/ 패키지
type IdentityAllocator struct {
	mu          sync.RWMutex
	byLabels    map[string]NumericIdentity
	labels      map[NumericIdentity]Labels
	nextID      NumericIdentity
	ipToID      map[string]NumericIdentity // IP → Identity 매핑 (CIDR/FQDN용)
	fqdnLabels  map[string]string          // IP → FQDN 라벨
}

func NewIdentityAllocator() *IdentityAllocator {
	return &IdentityAllocator{
		byLabels:   make(map[string]NumericIdentity),
		labels:     make(map[NumericIdentity]Labels),
		nextID:     100, // 사용자 아이덴티티는 100부터 시작
		ipToID:     make(map[string]NumericIdentity),
		fqdnLabels: make(map[string]string),
	}
}

func (ia *IdentityAllocator) AllocateIdentity(labels Labels) NumericIdentity {
	ia.mu.Lock()
	defer ia.mu.Unlock()
	key := fmt.Sprintf("%v", labels)
	if id, ok := ia.byLabels[key]; ok {
		return id
	}
	id := ia.nextID
	ia.nextID++
	ia.byLabels[key] = id
	ia.labels[id] = labels
	return id
}

func (ia *IdentityAllocator) GetLabels(id NumericIdentity) Labels {
	ia.mu.RLock()
	defer ia.mu.RUnlock()
	return ia.labels[id]
}

// AllocateCIDRIdentity는 CIDR에 대한 Identity를 할당한다.
func (ia *IdentityAllocator) AllocateCIDRIdentity(cidr string) NumericIdentity {
	labels := Labels{"cidr": cidr}
	return ia.AllocateIdentity(labels)
}

// AllocateFQDNIdentity는 FQDN 이름에서 파생된 IP에 대한 Identity를 할당한다.
// 실제 Cilium에서는 DNS 응답의 IP에 fqdn:<domain> 라벨을 가진 Identity를 할당한다.
func (ia *IdentityAllocator) AllocateFQDNIdentity(domain string, ip string) NumericIdentity {
	ia.mu.Lock()
	defer ia.mu.Unlock()

	// FQDN용 라벨 생성
	labels := Labels{"fqdn": domain}
	key := fmt.Sprintf("%v", labels)

	var id NumericIdentity
	if existingID, ok := ia.byLabels[key]; ok {
		id = existingID
	} else {
		id = ia.nextID
		ia.nextID++
		ia.byLabels[key] = id
		ia.labels[id] = labels
	}

	// IP → Identity 매핑 저장
	ia.ipToID[ip] = id
	ia.fqdnLabels[ip] = domain
	return id
}

func (ia *IdentityAllocator) LookupByIP(ip string) (NumericIdentity, bool) {
	ia.mu.RLock()
	defer ia.mu.RUnlock()
	id, ok := ia.ipToID[ip]
	return id, ok
}

// =============================================================================
// 10. DNS Cache (pkg/fqdn/cache.go의 DNSCache에 대응)
// =============================================================================

// DNSCache는 DNS 이름과 IP 주소의 매핑을 관리한다.
// 실제 Cilium: pkg/fqdn/cache.go
type DNSCache struct {
	mu      sync.RWMutex
	entries map[string]*DNSCacheEntry // 도메인 이름 → 엔트리
}

type DNSCacheEntry struct {
	Name       string
	IPs        []net.IP
	TTL        time.Duration
	LookupTime time.Time
}

func NewDNSCache() *DNSCache {
	return &DNSCache{
		entries: make(map[string]*DNSCacheEntry),
	}
}

// Update는 DNS 응답을 캐시에 저장한다.
// 실제 Cilium에서는 DNS 프록시가 DNS 응답을 관찰하여 호출한다.
func (c *DNSCache) Update(name string, ips []net.IP, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[name] = &DNSCacheEntry{
		Name:       name,
		IPs:        ips,
		TTL:        ttl,
		LookupTime: time.Now(),
	}
}

// Lookup은 도메인 이름에 대한 IP 목록을 반환한다.
func (c *DNSCache) Lookup(name string) []net.IP {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if entry, ok := c.entries[name]; ok {
		return entry.IPs
	}
	return nil
}

// =============================================================================
// 11. BPF PolicyMap 시뮬레이션 (pkg/maps/policymap/policymap.go에 대응)
// =============================================================================

// BPFPolicyMap은 BPF 정책 맵을 시뮬레이션한다.
// 실제 Cilium: pkg/maps/policymap/policymap.go의 PolicyMap
// BPF LPM Trie 기반으로 Identity+Direction+Protocol+Port를 키로 사용한다.
type BPFPolicyMap struct {
	mu      sync.RWMutex
	entries map[PolicyKey]PolicyMapEntry
	epID    uint64
}

func NewBPFPolicyMap(epID uint64) *BPFPolicyMap {
	return &BPFPolicyMap{
		entries: make(map[PolicyKey]PolicyMapEntry),
		epID:    epID,
	}
}

func (m *BPFPolicyMap) Update(key PolicyKey, entry PolicyMapEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 이미 존재하는 항목이 있으면 우선순위 비교
	// Deny(더 높은 Priority 값)가 Allow보다 우선한다.
	// 실제 Cilium: pkg/policy/types/entry.go의 Precedence 비교
	if existing, ok := m.entries[key]; ok {
		// Deny는 항상 Allow보다 우선
		if existing.Verdict == Deny && entry.Verdict != Deny {
			return
		}
		if entry.Verdict == Deny && existing.Verdict != Deny {
			m.entries[key] = entry
			return
		}
		// 같은 Verdict이면 Priority가 높은 것이 우선
		if existing.Priority > entry.Priority {
			return
		}
	}

	m.entries[key] = entry
}

func (m *BPFPolicyMap) Delete(key PolicyKey) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, key)
}

// Lookup은 BPF 데이터경로의 정책 조회를 시뮬레이션한다.
// 실제 BPF에서는 LPM Trie를 통한 최장 접두사 매칭을 수행한다.
// 매칭 우선순위: L3+L4 > L3 only > L4 only > Wildcard
// bpf/lib/policy.h의 POLICY_MATCH_* 상수 참조
func (m *BPFPolicyMap) Lookup(identity NumericIdentity, dir TrafficDirection, proto Protocol, port uint16) (PolicyMapEntry, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// 1. L3+L4 완전 매칭 (POLICY_MATCH_L3_L4)
	key := PolicyKey{Identity: identity, DestPort: port, Protocol: proto, TrafficDirection: dir}
	if entry, ok := m.entries[key]; ok {
		return entry, true
	}

	// 2. L3+Protocol 매칭, Port 와일드카드 (POLICY_MATCH_L3_PROTO)
	key = PolicyKey{Identity: identity, DestPort: 0, Protocol: proto, TrafficDirection: dir}
	if entry, ok := m.entries[key]; ok {
		return entry, true
	}

	// 3. L3 only 매칭 (POLICY_MATCH_L3_ONLY)
	key = PolicyKey{Identity: identity, DestPort: 0, Protocol: ProtoAny, TrafficDirection: dir}
	if entry, ok := m.entries[key]; ok {
		return entry, true
	}

	// 4. L4 only 매칭, Identity 와일드카드 (POLICY_MATCH_L4_ONLY)
	key = PolicyKey{Identity: 0, DestPort: port, Protocol: proto, TrafficDirection: dir}
	if entry, ok := m.entries[key]; ok {
		return entry, true
	}

	// 5. Protocol only 매칭 (POLICY_MATCH_PROTO_ONLY)
	key = PolicyKey{Identity: 0, DestPort: 0, Protocol: proto, TrafficDirection: dir}
	if entry, ok := m.entries[key]; ok {
		return entry, true
	}

	// 6. 전체 와일드카드 (POLICY_MATCH_ALL)
	key = PolicyKey{Identity: 0, DestPort: 0, Protocol: ProtoAny, TrafficDirection: dir}
	if entry, ok := m.entries[key]; ok {
		return entry, true
	}

	// 매칭 없음 -> 기본 거부 (POLICY_MATCH_NONE)
	return PolicyMapEntry{Verdict: Deny}, false
}

func (m *BPFPolicyMap) Dump() {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if len(m.entries) == 0 {
		fmt.Printf("    (비어 있음)\n")
		return
	}

	// 정렬된 출력을 위해 키 목록 수집
	type kv struct {
		key   PolicyKey
		entry PolicyMapEntry
	}
	var items []kv
	for k, v := range m.entries {
		items = append(items, kv{k, v})
	}
	sort.Slice(items, func(i, j int) bool {
		a, b := items[i].key, items[j].key
		if a.TrafficDirection != b.TrafficDirection {
			return a.TrafficDirection < b.TrafficDirection
		}
		if a.Identity != b.Identity {
			return a.Identity < b.Identity
		}
		if a.Protocol != b.Protocol {
			return a.Protocol < b.Protocol
		}
		return a.DestPort < b.DestPort
	})

	for _, item := range items {
		fmt.Printf("    %-50s -> %s\n", item.key, item.entry)
	}
}

// =============================================================================
// 12. L7 Proxy 시뮬레이션 (pkg/proxy/ 에 대응)
// =============================================================================

// L7Proxy는 L7 프록시를 시뮬레이션한다.
// 실제 Cilium: pkg/proxy/envoyproxy.go (Envoy), pkg/proxy/dns.go (DNS)
type L7Proxy struct {
	mu    sync.RWMutex
	rules map[uint16][]L7ProxyRule // 프록시 포트 → L7 규칙
}

type L7ProxyRule struct {
	AllowedIdentities []NumericIdentity
	HTTPRules         []HTTPRule
	DNSPatterns       []string
}

func NewL7Proxy() *L7Proxy {
	return &L7Proxy{
		rules: make(map[uint16][]L7ProxyRule),
	}
}

func (p *L7Proxy) AddRule(proxyPort uint16, rule L7ProxyRule) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules[proxyPort] = append(p.rules[proxyPort], rule)
}

func (p *L7Proxy) CheckHTTP(proxyPort uint16, srcIdentity NumericIdentity, method, path string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()

	rules, ok := p.rules[proxyPort]
	if !ok {
		return false
	}

	for _, rule := range rules {
		// Identity 확인
		identityMatch := false
		for _, id := range rule.AllowedIdentities {
			if id == 0 || id == srcIdentity { // 0은 와일드카드
				identityMatch = true
				break
			}
		}
		if !identityMatch {
			continue
		}

		// HTTP 규칙이 비어있으면 모든 HTTP 허용
		if len(rule.HTTPRules) == 0 {
			return true
		}

		for _, hr := range rule.HTTPRules {
			if hr.Matches(method, path) {
				return true
			}
		}
	}
	return false
}

// =============================================================================
// 13. PolicyRepository (pkg/policy/repository.go에 대응)
// =============================================================================

// PolicyRepository는 정책 규칙의 저장소이다.
// 실제 Cilium: pkg/policy/repository.go의 Repository
type PolicyRepository struct {
	mu       sync.RWMutex
	rules    []*PolicyRule
	revision uint64

	identityAllocator *IdentityAllocator
	dnsCache          *DNSCache
	l7Proxy           *L7Proxy

	// 엔드포인트별 BPF 맵
	policyMaps map[uint64]*BPFPolicyMap

	// 엔드포인트 목록
	endpoints []*Endpoint
}

func NewPolicyRepository(ia *IdentityAllocator, dnsCache *DNSCache, proxy *L7Proxy) *PolicyRepository {
	return &PolicyRepository{
		revision:          1,
		identityAllocator: ia,
		dnsCache:          dnsCache,
		l7Proxy:           proxy,
		policyMaps:        make(map[uint64]*BPFPolicyMap),
	}
}

// AddRule은 정책 규칙을 추가한다.
// 실제 Cilium: Repository.ReplaceByResource()
func (r *PolicyRepository) AddRule(rule *PolicyRule) {
	r.mu.Lock()
	r.rules = append(r.rules, rule)
	r.revision++
	r.mu.Unlock()

	fmt.Printf("[PolicyRepository] 규칙 추가: '%s' (rev=%d)\n", rule.Name, r.revision)
}

// DeleteRule은 이름으로 정책 규칙을 삭제한다.
func (r *PolicyRepository) DeleteRule(name string) {
	r.mu.Lock()
	for i, rule := range r.rules {
		if rule.Name == name {
			r.rules = append(r.rules[:i], r.rules[i+1:]...)
			r.revision++
			break
		}
	}
	r.mu.Unlock()

	fmt.Printf("[PolicyRepository] 규칙 삭제: '%s' (rev=%d)\n", name, r.revision)
}

// RegisterEndpoint는 엔드포인트를 등록한다.
func (r *PolicyRepository) RegisterEndpoint(ep *Endpoint) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.endpoints = append(r.endpoints, ep)
	r.policyMaps[ep.ID] = NewBPFPolicyMap(ep.ID)
}

// GetPolicyMap은 엔드포인트의 BPF 정책 맵을 반환한다.
func (r *PolicyRepository) GetPolicyMap(epID uint64) *BPFPolicyMap {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.policyMaps[epID]
}

// =============================================================================
// 14. Policy Resolution & Distillation
//     (pkg/policy/repository.go의 resolvePolicyLocked,
//      pkg/policy/resolve.go의 DistillPolicy에 대응)
// =============================================================================

// computePolicyForEndpoint는 특정 엔드포인트에 대한 정책을 계산한다.
// 실제 Cilium: Repository.resolvePolicyLocked() → computePolicyEnforcementAndRules()
func (r *PolicyRepository) computePolicyForEndpoint(ep *Endpoint) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	policyMap := r.policyMaps[ep.ID]
	if policyMap == nil {
		return
	}

	// 기존 맵 초기화
	policyMap.mu.Lock()
	policyMap.entries = make(map[PolicyKey]PolicyMapEntry)
	policyMap.mu.Unlock()

	// 1단계: 이 엔드포인트를 선택(subject select)하는 규칙 수집
	// 실제 Cilium: computePolicyEnforcementAndRules()
	var matchingIngressRules []*PolicyRule
	var matchingEgressRules []*PolicyRule
	hasIngressDefaultDeny := false
	hasEgressDefaultDeny := false

	for _, rule := range r.rules {
		if !rule.Subject.Matches(ep.Labels) {
			continue
		}

		if rule.Direction == Ingress {
			matchingIngressRules = append(matchingIngressRules, rule)
			if rule.DefaultDeny {
				hasIngressDefaultDeny = true
			}
		} else {
			matchingEgressRules = append(matchingEgressRules, rule)
			if rule.DefaultDeny {
				hasEgressDefaultDeny = true
			}
		}
	}

	// 2단계: 규칙 정렬 (Tier → Priority 순)
	// 실제 Cilium: ruleSlice.sort()
	sortRules := func(rules []*PolicyRule) {
		sort.Slice(rules, func(i, j int) bool {
			if rules[i].Tier != rules[j].Tier {
				return rules[i].Tier < rules[j].Tier
			}
			return rules[i].Priority < rules[j].Priority
		})
	}
	sortRules(matchingIngressRules)
	sortRules(matchingEgressRules)

	// 3단계: L4 정책 해석 (resolveL4Policy)
	// 실제 Cilium: ruleSlice.resolveL4Policy() in rules.go

	// Ingress 처리
	if hasIngressDefaultDeny || len(matchingIngressRules) > 0 {
		r.resolveDirectionRules(ep, matchingIngressRules, Ingress, policyMap, hasIngressDefaultDeny)
	}

	// Egress 처리
	if hasEgressDefaultDeny || len(matchingEgressRules) > 0 {
		r.resolveDirectionRules(ep, matchingEgressRules, Egress, policyMap, hasEgressDefaultDeny)
	}
}

// resolveDirectionRules는 한 방향(Ingress/Egress)의 규칙들을 해석하여 BPF 맵에 기록한다.
// 실제 Cilium: L4PolicyMaps.resolveL4Policy() + L4DirectionPolicy.toMapState()
func (r *PolicyRepository) resolveDirectionRules(
	ep *Endpoint,
	rules []*PolicyRule,
	dir TrafficDirection,
	policyMap *BPFPolicyMap,
	hasDefaultDeny bool,
) {
	nextProxyPort := uint16(10000 + ep.ID*100)

	for _, rule := range rules {
		// L3 피어 Identity 수집
		var peerIdentities []NumericIdentity
		peerIdentities = r.collectPeerIdentities(rule, dir)

		if len(rule.Ports) == 0 {
			// L3-only 규칙: 모든 포트/프로토콜에 대해 Identity 허용
			// 실제 Cilium: L4PolicyMap.mergeL4Filter()에서 Port=0, Proto=ANY로 처리
			for _, peerID := range peerIdentities {
				key := PolicyKey{
					Identity:         peerID,
					DestPort:         0,
					Protocol:         ProtoAny,
					TrafficDirection: dir,
				}
				entry := PolicyMapEntry{
					Verdict:  rule.Verdict,
					Priority: r.computePrecedence(rule),
				}
				policyMap.Update(key, entry)
			}
		} else {
			// L3+L4 규칙
			for _, portRule := range rule.Ports {
				for _, peerID := range peerIdentities {
					key := PolicyKey{
						Identity:         peerID,
						DestPort:         portRule.Port,
						Protocol:         portRule.Protocol,
						TrafficDirection: dir,
					}
					entry := PolicyMapEntry{
						Verdict:  rule.Verdict,
						Priority: r.computePrecedence(rule),
					}

					// L7 규칙이 있으면 프록시 포트 설정
					if !portRule.L7.IsEmpty() {
						entry.ProxyPort = nextProxyPort
						nextProxyPort++

						// L7 프록시에 규칙 추가
						proxyRule := L7ProxyRule{
							AllowedIdentities: peerIdentities,
							HTTPRules:         portRule.L7.HTTP,
							DNSPatterns:       portRule.L7.DNS,
						}
						r.l7Proxy.AddRule(entry.ProxyPort, proxyRule)
					}

					policyMap.Update(key, entry)
				}
			}
		}
	}
}

// collectPeerIdentities는 규칙에서 L3 피어 Identity를 수집한다.
func (r *PolicyRepository) collectPeerIdentities(rule *PolicyRule, dir TrafficDirection) []NumericIdentity {
	var identities []NumericIdentity

	if dir == Ingress {
		// Ingress: FromEndpoints, FromCIDR
		if len(rule.FromEndpoints) == 0 && len(rule.FromCIDR) == 0 {
			// 와일드카드: Identity 0으로 표현
			identities = append(identities, 0)
		}

		for _, sel := range rule.FromEndpoints {
			// 매칭되는 모든 엔드포인트의 Identity 수집
			for _, ep := range r.endpoints {
				if sel.Matches(ep.Labels) {
					identities = append(identities, ep.Identity)
				}
			}
		}

		for _, cidr := range rule.FromCIDR {
			id := r.identityAllocator.AllocateCIDRIdentity(cidr.CIDR)
			identities = append(identities, id)
		}
	} else {
		// Egress: ToEndpoints, ToCIDR, ToFQDN
		if len(rule.ToEndpoints) == 0 && len(rule.ToCIDR) == 0 && len(rule.ToFQDN) == 0 {
			identities = append(identities, 0)
		}

		for _, sel := range rule.ToEndpoints {
			for _, ep := range r.endpoints {
				if sel.Matches(ep.Labels) {
					identities = append(identities, ep.Identity)
				}
			}
		}

		for _, cidr := range rule.ToCIDR {
			id := r.identityAllocator.AllocateCIDRIdentity(cidr.CIDR)
			identities = append(identities, id)
		}

		// FQDN: DNS 캐시에서 IP를 조회하여 Identity 할당
		// 실제 Cilium: FQDN 정책은 DNS 프록시가 DNS 응답을 관찰한 후,
		// IP에 fqdn:<domain> 라벨의 Identity를 할당한다.
		for _, domain := range rule.ToFQDN {
			ips := r.dnsCache.Lookup(domain)
			for _, ip := range ips {
				id := r.identityAllocator.AllocateFQDNIdentity(domain, ip.String())
				identities = append(identities, id)
			}
		}
	}

	return identities
}

// computePrecedence는 규칙의 Precedence를 계산한다.
// 실제 Cilium: pkg/policy/types/entry.go의 Priority.toBasePrecedence()
func (r *PolicyRepository) computePrecedence(rule *PolicyRule) uint32 {
	// Tier + Priority를 Precedence로 인코딩
	tierPrecedence := uint32(3-rule.Tier) * 1000000
	priorityPrecedence := uint32(1000000 - uint32(rule.Priority*1000))

	if rule.Verdict == Deny {
		return tierPrecedence + priorityPrecedence + 255 // Deny가 가장 높은 우선순위
	}
	return tierPrecedence + priorityPrecedence + 1
}

// RegenerateAll은 모든 엔드포인트의 정책을 재계산한다.
// 실제 Cilium: 정책 변경 시 영향받는 엔드포인트의 regeneration이 트리거된다.
func (r *PolicyRepository) RegenerateAll() {
	fmt.Printf("\n[PolicyRepository] 전체 정책 재계산 (rev=%d)...\n", r.revision)
	for _, ep := range r.endpoints {
		r.computePolicyForEndpoint(ep)
		fmt.Printf("  엔드포인트 %d (Identity=%d, Labels=%s) BPF 맵 업데이트 완료\n",
			ep.ID, ep.Identity, ep.Labels)
	}
}

// =============================================================================
// 15. 시뮬레이션 시나리오 실행
// =============================================================================

func printSection(title string) {
	fmt.Printf("\n%s\n%s\n", title, strings.Repeat("=", len(title)+10))
}

func printSubsection(title string) {
	fmt.Printf("\n  --- %s ---\n", title)
}

func main() {
	fmt.Println("=======================================================")
	fmt.Println("  Cilium Policy Engine PoC")
	fmt.Println("  정책 엔진 시뮬레이션")
	fmt.Println("=======================================================")

	// --- 초기화 ---
	identityAlloc := NewIdentityAllocator()
	dnsCache := NewDNSCache()
	l7Proxy := NewL7Proxy()
	repo := NewPolicyRepository(identityAlloc, dnsCache, l7Proxy)

	// --- 엔드포인트 생성 ---
	printSection("1. 엔드포인트 생성")

	frontendLabels := Labels{"app": "frontend", "env": "prod"}
	backendLabels := Labels{"app": "backend", "env": "prod"}
	dbLabels := Labels{"app": "database", "env": "prod"}
	monitorLabels := Labels{"app": "monitor", "role": "observability"}

	frontendID := identityAlloc.AllocateIdentity(frontendLabels)
	backendID := identityAlloc.AllocateIdentity(backendLabels)
	dbID := identityAlloc.AllocateIdentity(dbLabels)
	monitorID := identityAlloc.AllocateIdentity(monitorLabels)

	frontend := &Endpoint{ID: 1, Identity: frontendID, Labels: frontendLabels, IP: net.ParseIP("10.0.0.1")}
	backend := &Endpoint{ID: 2, Identity: backendID, Labels: backendLabels, IP: net.ParseIP("10.0.0.2")}
	database := &Endpoint{ID: 3, Identity: dbID, Labels: dbLabels, IP: net.ParseIP("10.0.0.3")}
	monitor := &Endpoint{ID: 4, Identity: monitorID, Labels: monitorLabels, IP: net.ParseIP("10.0.0.4")}

	repo.RegisterEndpoint(frontend)
	repo.RegisterEndpoint(backend)
	repo.RegisterEndpoint(database)
	repo.RegisterEndpoint(monitor)

	fmt.Printf("  frontend: ID=%d, Identity=%d, Labels=%s\n", frontend.ID, frontend.Identity, frontend.Labels)
	fmt.Printf("  backend:  ID=%d, Identity=%d, Labels=%s\n", backend.ID, backend.Identity, backend.Labels)
	fmt.Printf("  database: ID=%d, Identity=%d, Labels=%s\n", database.ID, database.Identity, database.Labels)
	fmt.Printf("  monitor:  ID=%d, Identity=%d, Labels=%s\n", monitor.ID, monitor.Identity, monitor.Labels)

	// =========================================================================
	// 시나리오 A: Default Deny 동작
	// =========================================================================
	printSection("2. Default Deny 시뮬레이션")
	fmt.Println("  정책이 없는 상태에서 트래픽 확인 (default allow)...")

	repo.RegenerateAll()

	// 정책이 없으므로 BPF 맵이 비어있다 -> 데이터경로에서 기본 허용
	bpfMap := repo.GetPolicyMap(backend.ID)
	fmt.Printf("\n  Backend(EP=%d) BPF Policy Map (정책 없음 = 기본 허용):\n", backend.ID)
	bpfMap.Dump()

	// Ingress 정책 추가 -> default deny 활성화
	fmt.Println("\n  [정책 추가] backend에 대한 ingress 정책 추가 -> default deny 활성화")

	rule1 := &PolicyRule{
		Name:    "allow-frontend-to-backend",
		Tier:    TierNormal,
		Subject: EndpointSelector{MatchLabels: Labels{"app": "backend"}},
		Direction:   Ingress,
		Verdict:     Allow,
		DefaultDeny: true,
		FromEndpoints: []EndpointSelector{
			{MatchLabels: Labels{"app": "frontend"}},
		},
		Ports: []PortRule{
			{Port: 8080, Protocol: ProtoTCP},
		},
	}
	repo.AddRule(rule1)
	repo.RegenerateAll()

	fmt.Printf("\n  Backend(EP=%d) BPF Policy Map (frontend -> backend:8080 허용):\n", backend.ID)
	bpfMap.Dump()

	// 트래픽 테스트
	printSubsection("트래픽 테스트 (Default Deny 활성화)")

	testTraffic := func(epID uint64, srcID NumericIdentity, dir TrafficDirection, proto Protocol, port uint16, desc string) {
		pm := repo.GetPolicyMap(epID)
		entry, found := pm.Lookup(srcID, dir, proto, port)
		verdict := "DROP (default deny)"
		if found && entry.Verdict == Allow {
			verdict = "ALLOW"
			if entry.ProxyPort > 0 {
				verdict += fmt.Sprintf(" (via proxy:%d)", entry.ProxyPort)
			}
		} else if found && entry.Verdict == Deny {
			verdict = "DENY (explicit)"
		}
		fmt.Printf("    %-55s -> %s\n", desc, verdict)
	}

	testTraffic(backend.ID, frontendID, Ingress, ProtoTCP, 8080, "frontend -> backend:8080/TCP")
	testTraffic(backend.ID, frontendID, Ingress, ProtoTCP, 9090, "frontend -> backend:9090/TCP")
	testTraffic(backend.ID, monitorID, Ingress, ProtoTCP, 8080, "monitor  -> backend:8080/TCP")
	testTraffic(backend.ID, dbID, Ingress, ProtoTCP, 8080, "database -> backend:8080/TCP")

	// =========================================================================
	// 시나리오 B: L7 HTTP 정책
	// =========================================================================
	printSection("3. L7 HTTP 정책 시뮬레이션")

	rule2 := &PolicyRule{
		Name:    "allow-frontend-to-backend-http",
		Tier:    TierNormal,
		Subject: EndpointSelector{MatchLabels: Labels{"app": "backend"}},
		Direction:   Ingress,
		Verdict:     Allow,
		DefaultDeny: true,
		FromEndpoints: []EndpointSelector{
			{MatchLabels: Labels{"app": "frontend"}},
		},
		Ports: []PortRule{
			{
				Port:     80,
				Protocol: ProtoTCP,
				L7: L7Rules{
					HTTP: []HTTPRule{
						{Method: "GET", Path: "/api/v1/.*"},
						{Method: "POST", Path: "/api/v1/submit"},
					},
				},
			},
		},
	}
	repo.AddRule(rule2)
	repo.RegenerateAll()

	fmt.Printf("\n  Backend(EP=%d) BPF Policy Map 상태:\n", backend.ID)
	bpfMap.Dump()

	// L7 프록시를 통한 HTTP 트래픽 테스트
	printSubsection("L7 HTTP 트래픽 테스트")

	testHTTP := func(proxyPort uint16, srcID NumericIdentity, method, path, desc string) {
		allowed := l7Proxy.CheckHTTP(proxyPort, srcID, method, path)
		verdict := "DENY (L7)"
		if allowed {
			verdict = "ALLOW (L7)"
		}
		fmt.Printf("    %-55s -> %s\n", desc, verdict)
	}

	// 프록시 포트 찾기 (BPF 맵에서 ProxyPort 확인)
	entry, found := bpfMap.Lookup(frontendID, Ingress, ProtoTCP, 80)
	if found && entry.ProxyPort > 0 {
		proxyPort := entry.ProxyPort
		fmt.Printf("  (프록시 포트: %d)\n", proxyPort)

		testHTTP(proxyPort, frontendID, "GET", "/api/v1/users", "GET /api/v1/users")
		testHTTP(proxyPort, frontendID, "POST", "/api/v1/submit", "POST /api/v1/submit")
		testHTTP(proxyPort, frontendID, "DELETE", "/api/v1/users", "DELETE /api/v1/users")
		testHTTP(proxyPort, frontendID, "GET", "/admin/config", "GET /admin/config")
		testHTTP(proxyPort, monitorID, "GET", "/api/v1/users", "GET /api/v1/users (monitor)")
	}

	// =========================================================================
	// 시나리오 C: CIDR 기반 정책
	// =========================================================================
	printSection("4. CIDR 기반 정책 시뮬레이션")

	rule3 := &PolicyRule{
		Name:    "allow-external-to-frontend",
		Tier:    TierNormal,
		Subject: EndpointSelector{MatchLabels: Labels{"app": "frontend"}},
		Direction:   Ingress,
		Verdict:     Allow,
		DefaultDeny: true,
		FromCIDR: []*CIDRSelector{
			NewCIDRSelector("203.0.113.0/24"),
		},
		Ports: []PortRule{
			{Port: 443, Protocol: ProtoTCP},
		},
	}
	repo.AddRule(rule3)
	repo.RegenerateAll()

	fmt.Printf("\n  Frontend(EP=%d) BPF Policy Map 상태:\n", frontend.ID)
	fpm := repo.GetPolicyMap(frontend.ID)
	fpm.Dump()

	// CIDR Identity 확인
	cidrID := identityAlloc.AllocateCIDRIdentity("203.0.113.0/24")
	fmt.Printf("\n  CIDR 203.0.113.0/24 -> Identity=%d\n", cidrID)

	printSubsection("CIDR 트래픽 테스트")
	testTraffic(frontend.ID, cidrID, Ingress, ProtoTCP, 443, "203.0.113.0/24 -> frontend:443/TCP")
	testTraffic(frontend.ID, cidrID, Ingress, ProtoTCP, 80, "203.0.113.0/24 -> frontend:80/TCP")

	// =========================================================================
	// 시나리오 D: FQDN 정책 (DNS 프록시 연동 시뮬레이션)
	// =========================================================================
	printSection("5. FQDN 정책 시뮬레이션")

	fmt.Println("  [단계 1] FQDN 정책 추가: backend -> api.example.com:443 허용")

	rule4 := &PolicyRule{
		Name:    "allow-backend-to-api",
		Tier:    TierNormal,
		Subject: EndpointSelector{MatchLabels: Labels{"app": "backend"}},
		Direction:   Egress,
		Verdict:     Allow,
		DefaultDeny: true,
		ToFQDN: []string{"api.example.com"},
		Ports: []PortRule{
			{Port: 443, Protocol: ProtoTCP},
		},
	}
	repo.AddRule(rule4)

	// FQDN 정책은 DNS 응답이 없으면 IP를 알 수 없으므로 아직 BPF 맵에 항목 없음
	repo.RegenerateAll()
	fmt.Printf("\n  Backend(EP=%d) Egress BPF Policy Map (DNS 조회 전):\n", backend.ID)
	bpfMap.Dump()

	fmt.Println("\n  [단계 2] DNS 조회 시뮬레이션: api.example.com -> 93.184.216.34")
	fmt.Println("  (실제 Cilium: DNS 프록시가 Pod의 DNS 쿼리를 가로채서 응답을 관찰)")

	// DNS 프록시가 DNS 응답을 관찰
	dnsCache.Update("api.example.com", []net.IP{net.ParseIP("93.184.216.34")}, 300*time.Second)
	fmt.Printf("  DNS Cache 업데이트: api.example.com -> [93.184.216.34] (TTL=300s)\n")

	// IP에 FQDN Identity 할당
	fqdnID := identityAlloc.AllocateFQDNIdentity("api.example.com", "93.184.216.34")
	fmt.Printf("  FQDN Identity 할당: api.example.com(93.184.216.34) -> Identity=%d\n", fqdnID)

	fmt.Println("\n  [단계 3] 정책 재계산 (DNS 응답 반영)")
	repo.RegenerateAll()

	fmt.Printf("\n  Backend(EP=%d) BPF Policy Map 상태 (FQDN 정책 반영):\n", backend.ID)
	bpfMap.Dump()

	printSubsection("FQDN 트래픽 테스트")
	testTraffic(backend.ID, fqdnID, Egress, ProtoTCP, 443, "backend -> api.example.com:443/TCP (93.184.216.34)")
	testTraffic(backend.ID, fqdnID, Egress, ProtoTCP, 80, "backend -> api.example.com:80/TCP")

	// DNS에 새 IP 추가
	fmt.Println("\n  [단계 4] DNS 레코드 변경: api.example.com에 새 IP 추가")
	dnsCache.Update("api.example.com",
		[]net.IP{net.ParseIP("93.184.216.34"), net.ParseIP("93.184.216.35")},
		300*time.Second,
	)
	fqdnID2 := identityAlloc.AllocateFQDNIdentity("api.example.com", "93.184.216.35")
	fmt.Printf("  새 FQDN Identity 할당: api.example.com(93.184.216.35) -> Identity=%d\n", fqdnID2)

	repo.RegenerateAll()
	fmt.Printf("\n  Backend(EP=%d) BPF Policy Map 상태 (새 IP 추가 후):\n", backend.ID)
	bpfMap.Dump()

	// =========================================================================
	// 시나리오 E: Deny 규칙과 우선순위
	// =========================================================================
	printSection("6. Deny 규칙과 우선순위 시뮬레이션")

	fmt.Println("  시나리오: backend에 대한 모든 ingress를 허용하되, monitor로부터의 접근은 차단")

	// 기존 규칙 삭제 후 새 규칙 추가
	repo.DeleteRule("allow-frontend-to-backend")
	repo.DeleteRule("allow-frontend-to-backend-http")

	ruleAllowAll := &PolicyRule{
		Name:    "allow-all-to-backend",
		Tier:    TierBaseline, // 가장 낮은 Tier
		Subject: EndpointSelector{MatchLabels: Labels{"app": "backend"}},
		Direction:   Ingress,
		Verdict:     Allow,
		DefaultDeny: true,
		FromEndpoints: []EndpointSelector{
			{MatchLabels: Labels{}}, // 와일드카드: 모든 엔드포인트
		},
		Ports: []PortRule{
			{Port: 8080, Protocol: ProtoTCP},
		},
	}

	ruleDenyMonitor := &PolicyRule{
		Name:    "deny-monitor-to-backend",
		Tier:    TierNormal, // 상위 Tier -> 우선 적용
		Subject: EndpointSelector{MatchLabels: Labels{"app": "backend"}},
		Direction:   Ingress,
		Verdict:     Deny,
		DefaultDeny: true,
		FromEndpoints: []EndpointSelector{
			{MatchLabels: Labels{"app": "monitor"}},
		},
		Ports: []PortRule{
			{Port: 8080, Protocol: ProtoTCP},
		},
	}

	repo.AddRule(ruleAllowAll)
	repo.AddRule(ruleDenyMonitor)
	repo.RegenerateAll()

	fmt.Printf("\n  Backend(EP=%d) BPF Policy Map 상태:\n", backend.ID)
	bpfMap.Dump()

	printSubsection("우선순위 트래픽 테스트")
	fmt.Println("  (TierNormal Deny > TierBaseline Allow)")

	testTraffic(backend.ID, frontendID, Ingress, ProtoTCP, 8080, "frontend -> backend:8080/TCP")
	testTraffic(backend.ID, dbID, Ingress, ProtoTCP, 8080, "database -> backend:8080/TCP")
	testTraffic(backend.ID, monitorID, Ingress, ProtoTCP, 8080, "monitor  -> backend:8080/TCP (Deny 규칙)")

	// BPF 맵의 직접 조회로 우선순위 확인
	fmt.Println("\n  BPF 맵에서 monitor Identity에 대한 직접 조회:")
	entryMonitor, foundMonitor := bpfMap.Lookup(monitorID, Ingress, ProtoTCP, 8080)
	if foundMonitor {
		fmt.Printf("    Monitor(Identity=%d) -> Verdict=%s, Priority=%d\n",
			monitorID, entryMonitor.Verdict, entryMonitor.Priority)
	}
	entryFrontend, foundFrontend := bpfMap.Lookup(frontendID, Ingress, ProtoTCP, 8080)
	if foundFrontend {
		fmt.Printf("    Frontend(Identity=%d) -> Verdict=%s, Priority=%d\n",
			frontendID, entryFrontend.Verdict, entryFrontend.Priority)
	}

	// =========================================================================
	// 시나리오 F: 정책 업데이트 흐름
	// =========================================================================
	printSection("7. 정책 업데이트 흐름 시뮬레이션")

	fmt.Println("  전체 흐름: 규칙 추가 -> Repository 업데이트 -> Regenerate -> BPF Map 업데이트")

	fmt.Println("\n  [단계 1] 현재 상태 확인")
	fmt.Printf("  Repository revision: %d\n", repo.revision)
	fmt.Printf("  Database(EP=%d) Ingress BPF Map:\n", database.ID)
	dbMap := repo.GetPolicyMap(database.ID)
	dbMap.Dump()

	fmt.Println("\n  [단계 2] 새 규칙 추가: database에 대한 ingress (backend만 허용)")
	ruleDBIngress := &PolicyRule{
		Name:    "allow-backend-to-db",
		Tier:    TierNormal,
		Subject: EndpointSelector{MatchLabels: Labels{"app": "database"}},
		Direction:   Ingress,
		Verdict:     Allow,
		DefaultDeny: true,
		FromEndpoints: []EndpointSelector{
			{MatchLabels: Labels{"app": "backend"}},
		},
		Ports: []PortRule{
			{Port: 5432, Protocol: ProtoTCP},
		},
	}
	repo.AddRule(ruleDBIngress)

	fmt.Println("\n  [단계 3] 정책 재계산 트리거")
	repo.RegenerateAll()

	fmt.Printf("\n  [단계 4] Database(EP=%d) BPF Map 업데이트 확인:\n", database.ID)
	dbMap.Dump()

	printSubsection("Database 트래픽 테스트")
	testTraffic(database.ID, backendID, Ingress, ProtoTCP, 5432, "backend  -> database:5432/TCP")
	testTraffic(database.ID, frontendID, Ingress, ProtoTCP, 5432, "frontend -> database:5432/TCP")
	testTraffic(database.ID, monitorID, Ingress, ProtoTCP, 5432, "monitor  -> database:5432/TCP")

	// =========================================================================
	// 요약
	// =========================================================================
	printSection("8. 정리")
	fmt.Println(`
  이 PoC는 Cilium 정책 엔진의 핵심 메커니즘을 시뮬레이션한다:

  1. PolicyRepository: 정책 규칙을 저장하고 revision을 관리
     (실제: pkg/policy/repository.go)

  2. Identity 기반 정책: 라벨 → NumericIdentity → BPF 맵 키
     (실제: pkg/identity/, pkg/policy/types/selector.go)

  3. L3/L4/L7 계층적 매칭:
     - L3: Identity(라벨) 또는 CIDR
     - L4: Port + Protocol
     - L7: HTTP Method/Path (Envoy 프록시 리다이렉트)
     (실제: pkg/policy/l4.go, pkg/policy/api/http.go)

  4. Policy Distillation: 규칙 → per-endpoint BPF 맵 엔트리
     (실제: pkg/policy/resolve.go의 DistillPolicy)

  5. BPF 맵 우선순위 매칭: L3+L4 > L3 > L4 > Wildcard
     (실제: bpf/lib/policy.h의 POLICY_MATCH_* 열거형)

  6. Default Deny: 정책이 적용되면 미매칭 트래픽은 자동 차단
     (실제: pkg/policy/repository.go의 computePolicyEnforcementAndRules)

  7. FQDN 정책: DNS 프록시 → DNS Cache → FQDN Identity → BPF 맵
     (실제: pkg/fqdn/cache.go, pkg/proxy/dns.go)

  8. Deny > Allow 우선순위 및 Tier 시스템
     (실제: pkg/policy/types/policyentry.go, entry.go)`)
}
