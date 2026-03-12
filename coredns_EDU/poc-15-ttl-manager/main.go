package main

import (
	"fmt"
	"sync"
	"time"
)

// ============================================================================
// CoreDNS TTL 관리 PoC
// ============================================================================
//
// CoreDNS cache 플러그인의 TTL 처리 메커니즘을 시뮬레이션한다.
//
// 실제 CoreDNS 구현 참조:
//   - plugin/pkg/dnsutil/ttl.go → MinimalTTL(), MinimalDefaultTTL, MaximumDefaultTTL
//   - plugin/cache/cache.go     → Cache 구조체 (pttl, nttl, minpttl, failttl 등)
//   - plugin/cache/handler.go   → TTL 감소 로직, 캐시 히트/미스 처리
//
// TTL 처리 핵심:
//   1. 캐시 저장 시 TTL 클램핑 (min/max 범위 제한)
//   2. 캐시 응답 시 경과 시간만큼 TTL 감소
//   3. TTL=0 도달 시 캐시 만료 → 업스트림 재조회
//   4. SERVFAIL 응답도 짧은 TTL로 캐시 (부정적 캐싱)
//
// CoreDNS 기본값:
//   MinimalDefaultTTL  = 5초   (절대 최소 TTL)
//   MaximumDefaultTTL  = 1시간 (절대 최대 TTL)
//   Cache.minpttl      = 5초   (긍정적 캐시 최소 TTL)
//   Cache.pttl         = 1시간 (긍정적 캐시 최대 TTL)
//   Cache.failttl      = 5초   (SERVFAIL 캐시 TTL)
// ============================================================================

const (
	// CoreDNS 기본 상수 (plugin/pkg/dnsutil/ttl.go)
	MinimalDefaultTTL = 5 * time.Second
	MaximumDefaultTTL = 1 * time.Hour
)

// CacheConfig는 캐시 TTL 설정을 나타낸다.
// CoreDNS Cache 구조체의 TTL 관련 필드 (plugin/cache/cache.go):
//
//	type Cache struct {
//	    pttl    time.Duration  // 긍정적 캐시 최대 TTL
//	    minpttl time.Duration  // 긍정적 캐시 최소 TTL
//	    nttl    time.Duration  // 부정적 캐시 최대 TTL
//	    minnttl time.Duration  // 부정적 캐시 최소 TTL
//	    failttl time.Duration  // SERVFAIL 캐시 TTL
//	}
type CacheConfig struct {
	MaxTTL     time.Duration // 긍정적 캐시 최대 TTL (기본: 1시간)
	MinTTL     time.Duration // 긍정적 캐시 최소 TTL (기본: 5초)
	MaxNegTTL  time.Duration // 부정적 캐시 최대 TTL
	MinNegTTL  time.Duration // 부정적 캐시 최소 TTL
	FailTTL    time.Duration // SERVFAIL 캐시 TTL (기본: 5초)
}

// DefaultCacheConfig는 CoreDNS 기본 캐시 설정을 반환한다.
func DefaultCacheConfig() CacheConfig {
	return CacheConfig{
		MaxTTL:    MaximumDefaultTTL,
		MinTTL:    MinimalDefaultTTL,
		MaxNegTTL: 30 * time.Minute,
		MinNegTTL: MinimalDefaultTTL,
		FailTTL:   MinimalDefaultTTL,
	}
}

// ResponseType은 DNS 응답 유형을 나타낸다.
type ResponseType int

const (
	ResponseSuccess  ResponseType = iota // 정상 응답
	ResponseNXDOMAIN                     // 도메인 없음
	ResponseNODATA                       // 데이터 없음
	ResponseSERVFAIL                     // 서버 실패
)

func (rt ResponseType) String() string {
	switch rt {
	case ResponseSuccess:
		return "SUCCESS"
	case ResponseNXDOMAIN:
		return "NXDOMAIN"
	case ResponseNODATA:
		return "NODATA"
	case ResponseSERVFAIL:
		return "SERVFAIL"
	}
	return "UNKNOWN"
}

// CachedItem은 캐시된 DNS 응답을 나타낸다.
type CachedItem struct {
	Name         string
	Value        string
	OriginalTTL  time.Duration   // 원래 TTL (클램핑 후)
	StoredAt     time.Time       // 캐시 저장 시각
	ResponseType ResponseType
}

// RemainingTTL은 현재 시각 기준 남은 TTL을 계산한다.
// CoreDNS 캐시 응답 시 TTL 감소 로직:
//
//	응답 TTL = 원래 TTL - 경과 시간
//
// 이것이 DNS의 "TTL 감소(decrementing)" 동작이다.
func (ci *CachedItem) RemainingTTL(now time.Time) time.Duration {
	elapsed := now.Sub(ci.StoredAt)
	remaining := ci.OriginalTTL - elapsed
	if remaining < 0 {
		return 0
	}
	return remaining
}

// IsExpired는 캐시 항목이 만료되었는지 확인한다.
func (ci *CachedItem) IsExpired(now time.Time) bool {
	return ci.RemainingTTL(now) <= 0
}

// TTLCache는 TTL 기반 DNS 캐시를 시뮬레이션한다.
type TTLCache struct {
	mu     sync.RWMutex
	items  map[string]*CachedItem
	config CacheConfig
	now    func() time.Time // 테스트용 시간 함수
}

// NewTTLCache는 새 TTL 캐시를 생성한다.
func NewTTLCache(config CacheConfig) *TTLCache {
	return &TTLCache{
		items:  make(map[string]*CachedItem),
		config: config,
		now:    time.Now,
	}
}

// clampTTL은 TTL을 min/max 범위로 클램핑한다.
// CoreDNS MinimalTTL() 함수 참조 (plugin/pkg/dnsutil/ttl.go):
//
//	응답의 모든 RR을 순회하며 최소 TTL을 찾고,
//	MinimalDefaultTTL(5초)과 MaximumDefaultTTL(1시간) 사이로 제한.
func (tc *TTLCache) clampTTL(ttl time.Duration, respType ResponseType) time.Duration {
	var minTTL, maxTTL time.Duration

	switch respType {
	case ResponseSuccess:
		minTTL = tc.config.MinTTL
		maxTTL = tc.config.MaxTTL
	case ResponseNXDOMAIN, ResponseNODATA:
		minTTL = tc.config.MinNegTTL
		maxTTL = tc.config.MaxNegTTL
	case ResponseSERVFAIL:
		// SERVFAIL은 고정 TTL 사용
		return tc.config.FailTTL
	}

	if ttl < minTTL {
		return minTTL
	}
	if ttl > maxTTL {
		return maxTTL
	}
	return ttl
}

// Store는 응답을 캐시에 저장한다.
func (tc *TTLCache) Store(name string, value string, ttl time.Duration, respType ResponseType) {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	clampedTTL := tc.clampTTL(ttl, respType)

	tc.items[name] = &CachedItem{
		Name:         name,
		Value:        value,
		OriginalTTL:  clampedTTL,
		StoredAt:     tc.now(),
		ResponseType: respType,
	}
}

// LookupResult는 캐시 조회 결과를 나타낸다.
type LookupResult struct {
	Item         *CachedItem
	RemainingTTL time.Duration
	Hit          bool
	Expired      bool
}

// Lookup은 캐시에서 항목을 조회한다.
func (tc *TTLCache) Lookup(name string) LookupResult {
	tc.mu.RLock()
	defer tc.mu.RUnlock()

	item, ok := tc.items[name]
	if !ok {
		return LookupResult{Hit: false}
	}

	now := tc.now()
	remaining := item.RemainingTTL(now)

	if item.IsExpired(now) {
		return LookupResult{
			Item:         item,
			RemainingTTL: 0,
			Hit:          true,
			Expired:      true,
		}
	}

	return LookupResult{
		Item:         item,
		RemainingTTL: remaining,
		Hit:          true,
		Expired:      false,
	}
}

// Evict는 만료된 항목을 제거한다.
func (tc *TTLCache) Evict() int {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	now := tc.now()
	evicted := 0
	for key, item := range tc.items {
		if item.IsExpired(now) {
			delete(tc.items, key)
			evicted++
		}
	}
	return evicted
}

func main() {
	fmt.Println("=== CoreDNS TTL 관리 PoC ===")
	fmt.Println()
	fmt.Println("CoreDNS cache 플러그인의 TTL 처리를 시뮬레이션합니다.")
	fmt.Println("참조: plugin/pkg/dnsutil/ttl.go, plugin/cache/cache.go")
	fmt.Println()

	config := DefaultCacheConfig()
	cache := NewTTLCache(config)

	// --- 데모 1: TTL 클램핑 ---
	fmt.Println("--- 데모 1: TTL 클램핑 (min/max 제한) ---")
	fmt.Println()
	fmt.Printf("설정: MinTTL=%v, MaxTTL=%v, FailTTL=%v\n",
		config.MinTTL, config.MaxTTL, config.FailTTL)
	fmt.Println()

	clampTests := []struct {
		name     string
		ttl      time.Duration
		respType ResponseType
	}{
		{"정상 TTL (300s)", 300 * time.Second, ResponseSuccess},
		{"너무 짧은 TTL (1s → MinTTL로 올림)", 1 * time.Second, ResponseSuccess},
		{"너무 긴 TTL (2h → MaxTTL로 내림)", 2 * time.Hour, ResponseSuccess},
		{"NXDOMAIN (600s)", 600 * time.Second, ResponseNXDOMAIN},
		{"SERVFAIL (항상 FailTTL)", 0, ResponseSERVFAIL},
	}

	for _, tc := range clampTests {
		clamped := cache.clampTTL(tc.ttl, tc.respType)
		fmt.Printf("  %-40s: %v → %v (%s)\n",
			tc.name, tc.ttl, clamped, tc.respType)
	}

	// --- 데모 2: TTL 감소 시뮬레이션 ---
	fmt.Println()
	fmt.Println("--- 데모 2: TTL 감소 (캐시 응답의 남은 TTL) ---")
	fmt.Println()

	// 시간을 제어하기 위한 가짜 시간
	currentTime := time.Now()
	cache.now = func() time.Time { return currentTime }

	// 레코드 캐시 저장 (TTL=10초)
	cache.Store("example.com", "93.184.216.34", 10*time.Second, ResponseSuccess)
	fmt.Println("캐시 저장: example.com A 93.184.216.34 (TTL=10s)")
	fmt.Println()

	// 시간 경과에 따른 TTL 감소 시뮬레이션
	checkpoints := []time.Duration{0, 2 * time.Second, 5 * time.Second, 8 * time.Second, 10 * time.Second, 12 * time.Second}

	fmt.Println("  경과시간  │ 남은TTL  │ 상태")
	fmt.Println("  ─────────┼──────────┼──────────")

	for _, elapsed := range checkpoints {
		cache.now = func() time.Time { return currentTime.Add(elapsed) }
		result := cache.Lookup("example.com")

		status := "캐시 히트"
		if result.Expired {
			status = "만료 (재조회 필요)"
		}
		if !result.Hit {
			status = "캐시 미스"
		}

		fmt.Printf("  %7v  │ %7v  │ %s\n",
			elapsed, result.RemainingTTL.Truncate(time.Millisecond), status)
	}

	// --- 데모 3: SERVFAIL 캐시 ---
	fmt.Println()
	fmt.Println("--- 데모 3: SERVFAIL 캐시 (부정적 캐싱) ---")
	fmt.Println()
	fmt.Println("SERVFAIL 응답도 짧은 TTL로 캐시하여 업스트림 부하를 줄인다.")
	fmt.Println()

	currentTime = time.Now()
	cache.now = func() time.Time { return currentTime }

	// SERVFAIL 캐시 (FailTTL=5초로 강제)
	cache.Store("broken.example.com", "SERVFAIL", 0, ResponseSERVFAIL)

	result := cache.Lookup("broken.example.com")
	fmt.Printf("  캐시 저장: broken.example.com → SERVFAIL (TTL=%v)\n", result.RemainingTTL)

	// 3초 후
	cache.now = func() time.Time { return currentTime.Add(3 * time.Second) }
	result = cache.Lookup("broken.example.com")
	fmt.Printf("  +3초 후: 남은 TTL=%v (여전히 캐시 히트 → 업스트림 쿼리 차단)\n", result.RemainingTTL)

	// 6초 후 (만료)
	cache.now = func() time.Time { return currentTime.Add(6 * time.Second) }
	result = cache.Lookup("broken.example.com")
	fmt.Printf("  +6초 후: 만료=%v (업스트림 재시도 허용)\n", result.Expired)

	// --- 데모 4: 전체 캐시 수명 주기 ---
	fmt.Println()
	fmt.Println("--- 데모 4: 전체 캐시 수명 주기 ---")
	fmt.Println()

	cache2 := NewTTLCache(config)
	currentTime = time.Now()
	cache2.now = func() time.Time { return currentTime }

	// 여러 레코드 저장
	records := []struct {
		name     string
		value    string
		ttl      time.Duration
		respType ResponseType
	}{
		{"fast.example.com", "10.0.0.1", 5 * time.Second, ResponseSuccess},
		{"medium.example.com", "10.0.0.2", 30 * time.Second, ResponseSuccess},
		{"slow.example.com", "10.0.0.3", 120 * time.Second, ResponseSuccess},
		{"missing.example.com", "NXDOMAIN", 10 * time.Second, ResponseNXDOMAIN},
	}

	for _, r := range records {
		cache2.Store(r.name, r.value, r.ttl, r.respType)
		clamped := cache2.clampTTL(r.ttl, r.respType)
		fmt.Printf("  저장: %-25s TTL=%v (클램핑 후: %v)\n", r.name, r.ttl, clamped)
	}

	fmt.Println()

	// 시간 경과에 따른 캐시 상태 확인
	timePoints := []time.Duration{0, 6 * time.Second, 15 * time.Second, 35 * time.Second}

	for _, elapsed := range timePoints {
		cache2.now = func() time.Time { return currentTime.Add(elapsed) }

		fmt.Printf("  [+%v] 캐시 상태:\n", elapsed)
		for _, r := range records {
			result := cache2.Lookup(r.name)
			if result.Hit && !result.Expired {
				fmt.Printf("    %-25s → 히트 (남은 TTL: %v)\n",
					r.name, result.RemainingTTL.Truncate(time.Millisecond))
			} else if result.Expired {
				fmt.Printf("    %-25s → 만료 (재조회 필요)\n", r.name)
			} else {
				fmt.Printf("    %-25s → 미스\n", r.name)
			}
		}

		// 만료된 항목 정리
		evicted := cache2.Evict()
		if evicted > 0 {
			fmt.Printf("    → %d개 만료 항목 정리됨\n", evicted)
		}
		fmt.Println()
	}

	// --- 데모 5: MinimalTTL 함수 시뮬레이션 ---
	fmt.Println("--- 데모 5: MinimalTTL 함수 동작 ---")
	fmt.Println()
	fmt.Println("CoreDNS MinimalTTL() (plugin/pkg/dnsutil/ttl.go):")
	fmt.Println("  DNS 응답의 모든 RR에서 최소 TTL을 찾아 캐시 만료 시간으로 사용")
	fmt.Println()

	// DNS 응답에 여러 RR이 있을 때 최소 TTL 선택
	type simRR = struct {
		name string
		ttl  uint32
	}

	testResponses := []struct {
		desc    string
		records []simRR
	}{
		{
			"Answer 섹션에 TTL이 다른 RR 3개",
			[]simRR{{"example.com A", 300}, {"example.com A", 60}, {"example.com A", 120}},
		},
		{
			"Answer + Authority 섹션",
			[]simRR{{"example.com A", 300}, {"example.com NS", 30}},
		},
		{
			"빈 응답 (MinimalDefaultTTL 적용)",
			[]simRR{},
		},
	}

	for _, tr := range testResponses {
		minTTL := findMinimalTTL(tr.records)
		fmt.Printf("  %s\n", tr.desc)
		for _, rr := range tr.records {
			fmt.Printf("    %s TTL=%d\n", rr.name, rr.ttl)
		}
		fmt.Printf("    → 최소 TTL: %v\n\n", minTTL)
	}

	fmt.Println("=== TTL 관리 PoC 완료 ===")
	fmt.Println()
	fmt.Println("핵심 정리:")
	fmt.Println("1. TTL 클램핑: MinTTL(5s) ≤ TTL ≤ MaxTTL(1h) 범위 제한")
	fmt.Println("2. TTL 감소: 캐시 응답 시 경과 시간만큼 TTL 감소")
	fmt.Println("3. SERVFAIL 캐시: 짧은 TTL(5s)로 부정적 응답 캐싱")
	fmt.Println("4. MinimalTTL: 응답 내 모든 RR의 최소 TTL을 캐시 만료 시간으로 사용")
}

// findMinimalTTL은 CoreDNS MinimalTTL() 함수를 시뮬레이션한다.
// 실제 코드 (plugin/pkg/dnsutil/ttl.go):
//
//	func MinimalTTL(m *dns.Msg, mt response.Type) time.Duration {
//	    if len(m.Answer)+len(m.Ns) == 0 { return MinimalDefaultTTL }
//	    minTTL := MaximumDefaultTTL
//	    for _, r := range m.Answer {
//	        if r.Header().Ttl < uint32(minTTL.Seconds()) {
//	            minTTL = time.Duration(r.Header().Ttl) * time.Second
//	        }
//	    }
//	    return minTTL
//	}
func findMinimalTTL(records []struct {
	name string
	ttl  uint32
}) time.Duration {
	if len(records) == 0 {
		return MinimalDefaultTTL // 빈 응답은 기본 최소 TTL
	}

	minTTL := MaximumDefaultTTL
	for _, r := range records {
		ttl := time.Duration(r.ttl) * time.Second
		if ttl < minTTL {
			minTTL = ttl
		}
	}
	return minTTL
}
