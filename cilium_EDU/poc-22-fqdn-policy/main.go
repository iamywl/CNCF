package main

import (
	"fmt"
	"math/rand"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Cilium FQDN 기반 정책 시뮬레이션
//
// 실제 소스: pkg/fqdn/cache.go, pkg/fqdn/dnsproxy/proxy.go,
//           pkg/fqdn/namemanager/manager.go
//
// 핵심 개념:
// 1. DNS 캐시 (TTL 기반 만료)
// 2. DNS 프록시 (정규식 기반 정책 검사)
// 3. NameManager (FQDN 셀렉터 → IP 매핑)
// 4. 좀비 메커니즘 (TTL 만료 후 활성 연결 보호)
// 5. 정규식 캐시 (참조 카운트 기반)
// =============================================================================

// --- 데이터 구조 ---

// cacheEntry는 DNS 캐시의 불변 엔트리 (Cilium: pkg/fqdn/cache.go:34)
type cacheEntry struct {
	Name           string
	LookupTime     time.Time
	ExpirationTime time.Time
	TTL            int
	IPs            []net.IP
}

// UpdateStatus는 DNS 캐시 업데이트 결과 (Cilium: pkg/fqdn/cache.go:54)
type UpdateStatus struct {
	Added   []net.IP
	Kept    []net.IP
	Removed []net.IP
}

// DNSCache는 FQDN→IP 매핑 캐시 (Cilium: pkg/fqdn/cache.go)
type DNSCache struct {
	mu       sync.RWMutex
	forward  map[string][]*cacheEntry // name → entries
	reverse  map[string][]string      // IP string → names
	minTTL   int
	maxPerIP int
}

// NewDNSCache 생성
func NewDNSCache(minTTL int) *DNSCache {
	return &DNSCache{
		forward:  make(map[string][]*cacheEntry),
		reverse:  make(map[string][]string),
		minTTL:   minTTL,
		maxPerIP: 50,
	}
}

// Update는 DNS 응답을 캐시에 반영 (Cilium: DNSCache.Update)
func (c *DNSCache) Update(lookupTime time.Time, name string, ips []net.IP, ttl int) UpdateStatus {
	c.mu.Lock()
	defer c.mu.Unlock()

	name = strings.ToLower(strings.TrimSuffix(name, "."))

	// MinTTL 적용
	if ttl < c.minTTL {
		ttl = c.minTTL
	}

	// 새 엔트리 생성 (불변)
	entry := &cacheEntry{
		Name:           name,
		LookupTime:     lookupTime,
		ExpirationTime: lookupTime.Add(time.Duration(ttl) * time.Second),
		TTL:            ttl,
		IPs:            ips,
	}

	// 기존 엔트리와 비교하여 Added/Kept/Removed 계산
	status := UpdateStatus{}
	existingIPs := make(map[string]bool)
	for _, entries := range c.forward[name] {
		for _, ip := range entries.IPs {
			existingIPs[ip.String()] = true
		}
	}

	newIPs := make(map[string]bool)
	for _, ip := range ips {
		ipStr := ip.String()
		newIPs[ipStr] = true
		if existingIPs[ipStr] {
			status.Kept = append(status.Kept, ip)
		} else {
			status.Added = append(status.Added, ip)
		}
	}

	for ipStr := range existingIPs {
		if !newIPs[ipStr] {
			status.Removed = append(status.Removed, net.ParseIP(ipStr))
		}
	}

	// forward 맵 업데이트
	c.forward[name] = []*cacheEntry{entry}

	// reverse 맵 업데이트
	for _, ip := range ips {
		ipStr := ip.String()
		names := c.reverse[ipStr]
		found := false
		for _, n := range names {
			if n == name {
				found = true
				break
			}
		}
		if !found {
			c.reverse[ipStr] = append(c.reverse[ipStr], name)
		}
	}

	return status
}

// GC는 TTL 만료된 엔트리를 제거 (Cilium: DNSCache.GC)
func (c *DNSCache) GC(now time.Time) (affected map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	affected = make(map[string]bool)

	for name, entries := range c.forward {
		var valid []*cacheEntry
		for _, e := range entries {
			if now.Before(e.ExpirationTime) {
				valid = append(valid, e)
			} else {
				affected[name] = true
				// reverse 맵에서도 제거
				for _, ip := range e.IPs {
					ipStr := ip.String()
					names := c.reverse[ipStr]
					for i, n := range names {
						if n == name {
							c.reverse[ipStr] = append(names[:i], names[i+1:]...)
							break
						}
					}
					if len(c.reverse[ipStr]) == 0 {
						delete(c.reverse, ipStr)
					}
				}
			}
		}
		if len(valid) == 0 {
			delete(c.forward, name)
		} else {
			c.forward[name] = valid
		}
	}

	return affected
}

// Lookup은 이름으로 IP 조회
func (c *DNSCache) Lookup(name string) []net.IP {
	c.mu.RLock()
	defer c.mu.RUnlock()

	name = strings.ToLower(name)
	var result []net.IP
	for _, entry := range c.forward[name] {
		result = append(result, entry.IPs...)
	}
	return result
}

// Count는 캐시 항목 수 반환
func (c *DNSCache) Count() (names int, ips int) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.forward), len(c.reverse)
}

// --- 정규식 캐시 (참조 카운트) ---

// regexCacheEntry는 참조 카운트 기반 캐시 (Cilium: dnsproxy/proxy.go:156)
type regexCacheEntry struct {
	regex    *regexp.Regexp
	refCount int
}

// RegexCache는 동일 패턴의 정규식 중복 컴파일 방지
type RegexCache struct {
	mu    sync.Mutex
	cache map[string]*regexCacheEntry
}

func NewRegexCache() *RegexCache {
	return &RegexCache{cache: make(map[string]*regexCacheEntry)}
}

func (rc *RegexCache) Acquire(pattern string) (*regexp.Regexp, error) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	if entry, ok := rc.cache[pattern]; ok {
		entry.refCount++
		return entry.regex, nil
	}

	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}

	rc.cache[pattern] = &regexCacheEntry{regex: re, refCount: 1}
	return re, nil
}

func (rc *RegexCache) Release(pattern string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	if entry, ok := rc.cache[pattern]; ok {
		entry.refCount--
		if entry.refCount <= 0 {
			delete(rc.cache, pattern)
		}
	}
}

func (rc *RegexCache) Stats() (total int, entries map[string]int) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	entries = make(map[string]int)
	for pattern, entry := range rc.cache {
		entries[pattern] = entry.refCount
		total++
	}
	return
}

// --- DNS 프록시 ---

// FQDNRule은 FQDN 정책 규칙
type FQDNRule struct {
	MatchName    string // 정확한 이름 매치
	MatchPattern string // 와일드카드 패턴 매치
}

// FQDNPolicy는 엔드포인트별 FQDN 정책
type FQDNPolicy struct {
	EndpointID uint64
	Rules      []FQDNRule
}

// DNSProxy는 L7 DNS 프록시 시뮬레이션 (Cilium: dnsproxy/proxy.go:61)
type DNSProxy struct {
	mu         sync.RWMutex
	cache      *DNSCache
	regexCache *RegexCache

	// 엔드포인트별 허용 규칙: EP ID → 컴파일된 정규식 목록
	allowed map[uint64][]*regexp.Regexp

	// 정책 콜백
	onDNSResponse func(name string, ips []net.IP, ttl int)

	// 통계
	totalQueries   int
	allowedQueries int
	deniedQueries  int
}

func NewDNSProxy(cache *DNSCache) *DNSProxy {
	return &DNSProxy{
		cache:      cache,
		regexCache: NewRegexCache(),
		allowed:    make(map[uint64][]*regexp.Regexp),
	}
}

// fqdnRuleToRegex는 FQDN 규칙을 정규식으로 변환 (Cilium: matchpattern)
func fqdnRuleToRegex(rule FQDNRule) string {
	if rule.MatchName != "" {
		// 정확한 이름 매치
		escaped := strings.ReplaceAll(rule.MatchName, ".", "\\.")
		return "^" + escaped + "$"
	}
	if rule.MatchPattern != "" {
		// 와일드카드 패턴 → 정규식
		escaped := strings.ReplaceAll(rule.MatchPattern, ".", "\\.")
		escaped = strings.ReplaceAll(escaped, "*", ".*")
		return "^" + escaped + "$"
	}
	return "^-$" // 매치 불가
}

// UpdatePolicy는 엔드포인트의 FQDN 정책을 업데이트
func (p *DNSProxy) UpdatePolicy(policy FQDNPolicy) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// 기존 정규식 해제
	for _, re := range p.allowed[policy.EndpointID] {
		p.regexCache.Release(re.String())
	}

	var regexes []*regexp.Regexp
	for _, rule := range policy.Rules {
		pattern := fqdnRuleToRegex(rule)
		re, err := p.regexCache.Acquire(pattern)
		if err != nil {
			return fmt.Errorf("failed to compile pattern %q: %w", pattern, err)
		}
		regexes = append(regexes, re)
	}

	p.allowed[policy.EndpointID] = regexes
	return nil
}

// CheckAllowed는 DNS 쿼리가 정책에 허용되는지 검사
func (p *DNSProxy) CheckAllowed(endpointID uint64, queryName string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()

	queryName = strings.ToLower(strings.TrimSuffix(queryName, "."))

	regexes, ok := p.allowed[endpointID]
	if !ok {
		return false
	}

	for _, re := range regexes {
		if re.MatchString(queryName) {
			return true
		}
	}
	return false
}

// HandleDNSQuery는 DNS 쿼리를 처리하는 시뮬레이션
func (p *DNSProxy) HandleDNSQuery(endpointID uint64, queryName string) ([]net.IP, error) {
	p.mu.Lock()
	p.totalQueries++
	p.mu.Unlock()

	// 1. 정책 검사
	if !p.CheckAllowed(endpointID, queryName) {
		p.mu.Lock()
		p.deniedQueries++
		p.mu.Unlock()
		return nil, fmt.Errorf("DNS query for %q denied by policy", queryName)
	}

	p.mu.Lock()
	p.allowedQueries++
	p.mu.Unlock()

	// 2. 시뮬레이션: 업스트림 DNS 조회 (실제로는 UDP 전달)
	ips := simulateDNSLookup(queryName)

	// 3. TTL 시뮬레이션
	ttl := 60 + rand.Intn(240)

	// 4. 캐시 업데이트
	status := p.cache.Update(time.Now(), queryName, ips, ttl)

	// 5. 콜백 호출 (NameManager로 전달)
	if p.onDNSResponse != nil {
		p.onDNSResponse(queryName, ips, ttl)
	}

	fmt.Printf("  [DNS Proxy] Query: %s → IPs: %v (TTL: %ds)\n", queryName, formatIPs(ips), ttl)
	if len(status.Added) > 0 {
		fmt.Printf("  [DNS Proxy]   New IPs added: %v\n", formatIPs(status.Added))
	}

	return ips, nil
}

// --- 좀비 메커니즘 ---

// DNSZombie는 TTL 만료 후에도 활성 연결을 추적 (Cilium: pkg/fqdn)
type DNSZombie struct {
	IP    net.IP
	Names []string
	Alive bool // CT에서 활성 여부
}

// ZombieTracker는 좀비 엔트리를 관리
type ZombieTracker struct {
	mu      sync.Mutex
	zombies []DNSZombie
}

func NewZombieTracker() *ZombieTracker {
	return &ZombieTracker{}
}

func (zt *ZombieTracker) Add(name string, ips []net.IP) {
	zt.mu.Lock()
	defer zt.mu.Unlock()

	for _, ip := range ips {
		zt.zombies = append(zt.zombies, DNSZombie{
			IP:    ip,
			Names: []string{name},
			Alive: rand.Float32() > 0.3, // 70% 확률로 활성 (시뮬레이션)
		})
	}
}

func (zt *ZombieTracker) GC() (alive []DNSZombie, dead []DNSZombie) {
	zt.mu.Lock()
	defer zt.mu.Unlock()

	for _, z := range zt.zombies {
		if z.Alive {
			alive = append(alive, z)
		} else {
			dead = append(dead, z)
		}
	}
	zt.zombies = nil // 모두 처리됨
	return
}

// --- NameManager ---

// FQDNSelector는 FQDN 셀렉터 (Cilium: policy/api)
type FQDNSelector struct {
	MatchName    string
	MatchPattern string
}

func (s FQDNSelector) String() string {
	if s.MatchName != "" {
		return "matchName:" + s.MatchName
	}
	return "matchPattern:" + s.MatchPattern
}

// NameManager는 FQDN 셀렉터 → IP 매핑 관리 (Cilium: namemanager/manager.go)
type NameManager struct {
	mu           sync.RWMutex
	selectors    map[FQDNSelector]*regexp.Regexp
	cache        *DNSCache
	selectorIPs  map[FQDNSelector]map[string]bool // 셀렉터별 매칭된 IP
}

func NewNameManager(cache *DNSCache) *NameManager {
	return &NameManager{
		selectors:   make(map[FQDNSelector]*regexp.Regexp),
		cache:       cache,
		selectorIPs: make(map[FQDNSelector]map[string]bool),
	}
}

// RegisterSelector는 FQDN 셀렉터를 등록 (Cilium: RegisterFQDNSelector)
func (nm *NameManager) RegisterSelector(sel FQDNSelector) error {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	pattern := fqdnRuleToRegex(FQDNRule{
		MatchName:    sel.MatchName,
		MatchPattern: sel.MatchPattern,
	})
	re, err := regexp.Compile(pattern)
	if err != nil {
		return err
	}

	nm.selectors[sel] = re

	// 기존 캐시에서 매칭되는 이름 검색
	nm.cache.mu.RLock()
	for name, entries := range nm.cache.forward {
		if re.MatchString(name) {
			if nm.selectorIPs[sel] == nil {
				nm.selectorIPs[sel] = make(map[string]bool)
			}
			for _, entry := range entries {
				for _, ip := range entry.IPs {
					nm.selectorIPs[sel][ip.String()] = true
				}
			}
		}
	}
	nm.cache.mu.RUnlock()

	return nil
}

// UpdateDNS는 DNS 응답에서 셀렉터 매칭 업데이트
func (nm *NameManager) UpdateDNS(name string, ips []net.IP) {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	name = strings.ToLower(name)

	for sel, re := range nm.selectors {
		if re.MatchString(name) {
			if nm.selectorIPs[sel] == nil {
				nm.selectorIPs[sel] = make(map[string]bool)
			}
			for _, ip := range ips {
				nm.selectorIPs[sel][ip.String()] = true
			}
		}
	}
}

// GetMatchedIPs는 셀렉터에 매칭된 IP 반환
func (nm *NameManager) GetMatchedIPs(sel FQDNSelector) []string {
	nm.mu.RLock()
	defer nm.mu.RUnlock()

	var result []string
	for ip := range nm.selectorIPs[sel] {
		result = append(result, ip)
	}
	return result
}

// --- 헬퍼 함수 ---

func simulateDNSLookup(name string) []net.IP {
	// 시뮬레이션: 이름 기반으로 결정적 IP 생성
	var ips []net.IP
	base := 0
	for _, c := range name {
		base += int(c)
	}
	count := 1 + rand.Intn(3)
	for i := 0; i < count; i++ {
		ip := net.IPv4(byte((base+i)%200+10), byte((base+i*17)%256),
			byte((base+i*31)%256), byte((base+i*47)%256))
		ips = append(ips, ip)
	}
	return ips
}

func formatIPs(ips []net.IP) string {
	strs := make([]string, len(ips))
	for i, ip := range ips {
		strs[i] = ip.String()
	}
	return "[" + strings.Join(strs, ", ") + "]"
}

func main() {
	fmt.Println("=" + strings.Repeat("=", 70))
	fmt.Println(" Cilium FQDN 기반 정책 시뮬레이션")
	fmt.Println(" 소스: pkg/fqdn/cache.go, pkg/fqdn/dnsproxy/proxy.go")
	fmt.Println("=" + strings.Repeat("=", 70))

	// --- 1. DNS 캐시 기본 동작 ---
	fmt.Println("\n[1] DNS 캐시 기본 동작")
	fmt.Println(strings.Repeat("-", 50))

	cache := NewDNSCache(60) // MinTTL = 60초

	// TTL이 MinTTL보다 작은 경우 MinTTL 적용
	status := cache.Update(time.Now(), "api.github.com",
		[]net.IP{net.ParseIP("140.82.114.5"), net.ParseIP("140.82.114.6")}, 30)
	fmt.Printf("  Update 'api.github.com': Added=%v, Kept=%v, Removed=%v\n",
		formatIPs(status.Added), formatIPs(status.Kept), formatIPs(status.Removed))

	// 같은 이름으로 업데이트 (IP 변경)
	status = cache.Update(time.Now(), "api.github.com",
		[]net.IP{net.ParseIP("140.82.114.5"), net.ParseIP("140.82.114.7")}, 120)
	fmt.Printf("  Update 'api.github.com': Added=%v, Kept=%v, Removed=%v\n",
		formatIPs(status.Added), formatIPs(status.Kept), formatIPs(status.Removed))

	names, ips := cache.Count()
	fmt.Printf("  캐시 상태: %d names, %d IPs\n", names, ips)

	// --- 2. 정규식 캐시 (참조 카운트) ---
	fmt.Println("\n[2] 정규식 캐시 (참조 카운트)")
	fmt.Println(strings.Repeat("-", 50))

	rc := NewRegexCache()

	// 동일 패턴을 여러 번 획득
	pattern := `^.*\.github\.com$`
	rc.Acquire(pattern) // EP-1
	rc.Acquire(pattern) // EP-2
	rc.Acquire(pattern) // EP-3

	total, entries := rc.Stats()
	fmt.Printf("  패턴 '%s'\n", pattern)
	fmt.Printf("  캐시 항목 수: %d, 참조 카운트: %d\n", total, entries[pattern])

	// 하나의 엔드포인트 해제
	rc.Release(pattern)
	_, entries = rc.Stats()
	fmt.Printf("  Release 1회 후 참조 카운트: %d\n", entries[pattern])

	// 모두 해제
	rc.Release(pattern)
	rc.Release(pattern)
	total, _ = rc.Stats()
	fmt.Printf("  모두 Release 후 캐시 항목 수: %d (삭제됨)\n", total)

	// --- 3. DNS 프록시 정책 검사 ---
	fmt.Println("\n[3] DNS 프록시 정책 검사")
	fmt.Println(strings.Repeat("-", 50))

	proxy := NewDNSProxy(cache)

	// NameManager 연동
	nm := NewNameManager(cache)
	proxy.onDNSResponse = func(name string, ips []net.IP, ttl int) {
		nm.UpdateDNS(name, ips)
	}

	// FQDN 정책 설정
	proxy.UpdatePolicy(FQDNPolicy{
		EndpointID: 1001,
		Rules: []FQDNRule{
			{MatchName: "api.github.com"},
			{MatchPattern: "*.githubusercontent.com"},
		},
	})

	proxy.UpdatePolicy(FQDNPolicy{
		EndpointID: 1002,
		Rules: []FQDNRule{
			{MatchPattern: "*.google.com"},
		},
	})

	// DNS 쿼리 시뮬레이션
	fmt.Println("\n  EP-1001 쿼리:")
	proxy.HandleDNSQuery(1001, "api.github.com")
	proxy.HandleDNSQuery(1001, "raw.githubusercontent.com")
	_, err := proxy.HandleDNSQuery(1001, "evil.com")
	if err != nil {
		fmt.Printf("  [DNS Proxy] DENIED: %v\n", err)
	}

	fmt.Println("\n  EP-1002 쿼리:")
	proxy.HandleDNSQuery(1002, "maps.google.com")
	_, err = proxy.HandleDNSQuery(1002, "api.github.com")
	if err != nil {
		fmt.Printf("  [DNS Proxy] DENIED: %v\n", err)
	}

	fmt.Printf("\n  프록시 통계: 총 %d 쿼리, 허용 %d, 거부 %d\n",
		proxy.totalQueries, proxy.allowedQueries, proxy.deniedQueries)

	// --- 4. NameManager 셀렉터 매칭 ---
	fmt.Println("\n[4] NameManager 셀렉터 매칭")
	fmt.Println(strings.Repeat("-", 50))

	sel1 := FQDNSelector{MatchName: "api.github.com"}
	sel2 := FQDNSelector{MatchPattern: "*.google.com"}

	nm.RegisterSelector(sel1)
	nm.RegisterSelector(sel2)

	ips1 := nm.GetMatchedIPs(sel1)
	ips2 := nm.GetMatchedIPs(sel2)

	fmt.Printf("  셀렉터 '%s' → IPs: %v\n", sel1, ips1)
	fmt.Printf("  셀렉터 '%s' → IPs: %v\n", sel2, ips2)

	// --- 5. TTL 기반 GC ---
	fmt.Println("\n[5] TTL 기반 GC (Garbage Collection)")
	fmt.Println(strings.Repeat("-", 50))

	shortCache := NewDNSCache(1) // MinTTL = 1초
	shortCache.Update(time.Now(), "short-lived.example.com",
		[]net.IP{net.ParseIP("1.2.3.4")}, 1)
	shortCache.Update(time.Now(), "long-lived.example.com",
		[]net.IP{net.ParseIP("5.6.7.8")}, 3600)

	names, _ = shortCache.Count()
	fmt.Printf("  GC 전 캐시: %d names\n", names)

	// 2초 후 GC (짧은 TTL 만료)
	affected := shortCache.GC(time.Now().Add(2 * time.Second))
	names, _ = shortCache.Count()
	fmt.Printf("  2초 후 GC: 영향받은 이름: %v, 남은 캐시: %d names\n", mapKeys(affected), names)

	// Lookup 확인
	remaining := shortCache.Lookup("short-lived.example.com")
	longLived := shortCache.Lookup("long-lived.example.com")
	fmt.Printf("  'short-lived.example.com' IPs: %v (만료됨)\n", remaining)
	fmt.Printf("  'long-lived.example.com' IPs: %v (유지)\n", formatIPs(longLived))

	// --- 6. 좀비 메커니즘 ---
	fmt.Println("\n[6] 좀비 메커니즘 (활성 연결 보호)")
	fmt.Println(strings.Repeat("-", 50))

	zombieTracker := NewZombieTracker()

	// TTL 만료된 엔트리를 좀비로 추가
	zombieTracker.Add("expired.example.com", []net.IP{
		net.ParseIP("10.0.0.1"),
		net.ParseIP("10.0.0.2"),
		net.ParseIP("10.0.0.3"),
	})

	alive, dead := zombieTracker.GC()
	fmt.Printf("  좀비 GC 결과:\n")
	fmt.Printf("    Alive (CT 활성, 유지): %d개\n", len(alive))
	for _, z := range alive {
		fmt.Printf("      IP: %s, Names: %v\n", z.IP, z.Names)
	}
	fmt.Printf("    Dead (CT 미사용, 삭제): %d개\n", len(dead))
	for _, z := range dead {
		fmt.Printf("      IP: %s, Names: %v\n", z.IP, z.Names)
	}

	// --- 7. FQDN 패턴 매칭 데모 ---
	fmt.Println("\n[7] FQDN 패턴 매칭")
	fmt.Println(strings.Repeat("-", 50))

	patterns := []struct {
		rule  FQDNRule
		tests []string
	}{
		{
			FQDNRule{MatchName: "api.github.com"},
			[]string{"api.github.com", "www.github.com", "github.com"},
		},
		{
			FQDNRule{MatchPattern: "*.github.com"},
			[]string{"api.github.com", "raw.github.com", "github.com"},
		},
		{
			FQDNRule{MatchPattern: "*.*.svc.cluster.local"},
			[]string{"myapp.default.svc.cluster.local", "redis.cache.svc.cluster.local", "svc.cluster.local"},
		},
	}

	for _, p := range patterns {
		regexStr := fqdnRuleToRegex(p.rule)
		re := regexp.MustCompile(regexStr)
		fmt.Printf("\n  규칙: %+v\n", p.rule)
		fmt.Printf("  정규식: %s\n", regexStr)
		for _, test := range p.tests {
			match := re.MatchString(test)
			result := "MISS"
			if match {
				result = "HIT"
			}
			fmt.Printf("    %-45s → %s\n", test, result)
		}
	}

	// --- 요약 ---
	fmt.Println("\n" + strings.Repeat("=", 71))
	fmt.Println(" 시뮬레이션 완료")
	fmt.Println()
	fmt.Println(" 핵심 동작 원리:")
	fmt.Println("   1. DNS 프록시가 Pod DNS 요청을 투명하게 가로챔")
	fmt.Println("   2. 정규식 기반 정책 검사 (참조 카운트 캐시)")
	fmt.Println("   3. 허용된 DNS 응답에서 FQDN→IP 매핑 학습")
	fmt.Println("   4. TTL 기반 캐시 GC + 좀비로 활성 연결 보호")
	fmt.Println("   5. NameManager가 FQDN 셀렉터→IP 매핑 관리")
	fmt.Println("   6. 학습된 IP로 BPF PolicyMap 자동 갱신")
	fmt.Println(strings.Repeat("=", 71))
}

func mapKeys(m map[string]bool) []string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
