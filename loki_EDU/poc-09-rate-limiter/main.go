package main

import (
	"fmt"
	"math"
	"sync"
	"time"
)

// =============================================================================
// Loki PoC #09: 레이트 리미터 - 테넌트별 수집 속도 제한
// =============================================================================
//
// Loki의 Distributor는 테넌트별로 수집 속도를 제한하여 하나의 테넌트가
// 전체 시스템 리소스를 독점하는 것을 방지한다.
// 핵심 메커니즘:
//   1. Token Bucket 알고리즘: 일정 속도로 토큰이 채워지고, 요청 시 소비
//   2. Per-Tenant Limits: 테넌트마다 독립적인 제한 설정
//   3. Local vs Global Strategy: 분산 환경에서의 속도 제한 전략
//      - Local: 각 Distributor가 전체 한도를 적용
//      - Global: 전체 한도를 Distributor 수로 나누어 적용
//
// 실제 Loki 코드: pkg/distributor/distributor.go, pkg/validation/limits.go
//
// 실행: go run main.go

// =============================================================================
// 1. Token Bucket 알고리즘
// =============================================================================
// Token Bucket은 네트워크 트래픽 제어에서 널리 사용되는 알고리즘이다.
//
// 동작 원리:
//   - 버킷에 일정 용량(capacity)의 토큰이 들어갈 수 있다
//   - 토큰은 일정 속도(rate)로 채워진다
//   - 요청 시 필요한 토큰을 소비한다
//   - 토큰이 부족하면 요청이 거부된다
//   - 버스트(burst): 버킷이 가득 차면 순간적으로 capacity만큼 처리 가능
//
//   시간 →
//   토큰: ████████░░ (8/10)
//   요청:    ▼3개       → 성공 (5/10 남음)
//   시간 경과 → 토큰 보충 → ███████░░░ (7/10)
//   요청:       ▼9개    → 실패 (7 < 9)

// TokenBucket은 토큰 버킷 레이트 리미터이다.
type TokenBucket struct {
	mu       sync.Mutex
	tokens   float64   // 현재 토큰 수
	capacity float64   // 최대 토큰 수 (버스트 크기)
	rate     float64   // 초당 토큰 보충 속도
	lastTime time.Time // 마지막 토큰 보충 시간
}

// NewTokenBucket은 새로운 토큰 버킷을 생성한다.
func NewTokenBucket(rate, capacity float64) *TokenBucket {
	return &TokenBucket{
		tokens:   capacity, // 처음에는 가득 채움
		capacity: capacity,
		rate:     rate,
		lastTime: time.Now(),
	}
}

// refill은 경과 시간에 따라 토큰을 보충한다.
// 이것이 Token Bucket의 핵심: 시간 기반으로 토큰이 자동 보충된다.
func (tb *TokenBucket) refill() {
	now := time.Now()
	elapsed := now.Sub(tb.lastTime).Seconds()
	tb.lastTime = now

	// 경과 시간 * 초당 보충 속도 = 보충할 토큰 수
	tb.tokens += elapsed * tb.rate

	// 최대 용량을 초과하지 않도록 제한
	if tb.tokens > tb.capacity {
		tb.tokens = tb.capacity
	}
}

// Allow는 n개의 토큰을 소비할 수 있는지 확인하고, 가능하면 소비한다.
func (tb *TokenBucket) Allow(n float64) bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	tb.refill()

	if tb.tokens >= n {
		tb.tokens -= n
		return true
	}
	return false
}

// AllowN은 n개의 토큰을 소비할 수 있는지 확인하고,
// 결과와 함께 현재 토큰 상태를 반환한다.
func (tb *TokenBucket) AllowN(n float64) (allowed bool, remaining float64) {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	tb.refill()

	if tb.tokens >= n {
		tb.tokens -= n
		return true, tb.tokens
	}
	return false, tb.tokens
}

// Status는 현재 토큰 상태를 반환한다.
func (tb *TokenBucket) Status() (tokens, capacity, rate float64) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	tb.refill()
	return tb.tokens, tb.capacity, tb.rate
}

// =============================================================================
// 2. 테넌트별 제한 설정
// =============================================================================
// Loki는 테넌트별로 다른 제한을 설정할 수 있다.
// 실제 코드: pkg/validation/limits.go

// TenantLimits는 테넌트별 제한 설정이다.
type TenantLimits struct {
	IngestionRate      float64 // 초당 허용 바이트 수 (bytes/sec)
	IngestionBurstSize float64 // 최대 버스트 크기 (bytes)
	MaxStreamsPerUser   int     // 테넌트당 최대 스트림 수
	MaxLabelNames      int     // 스트림당 최대 레이블 수
	MaxLineSize        int     // 최대 로그 라인 크기 (bytes)
}

// DefaultLimits는 기본 제한 설정을 반환한다.
func DefaultLimits() TenantLimits {
	return TenantLimits{
		IngestionRate:      4 * 1024 * 1024, // 4MB/s
		IngestionBurstSize: 6 * 1024 * 1024, // 6MB 버스트
		MaxStreamsPerUser:   10000,
		MaxLabelNames:      15,
		MaxLineSize:        256 * 1024, // 256KB
	}
}

// =============================================================================
// 3. 레이트 리미팅 전략 (Local vs Global)
// =============================================================================
// Loki는 두 가지 레이트 리미팅 전략을 제공한다:
//
// Local Strategy:
//   - 각 Distributor가 전체 한도를 적용
//   - 장점: 간단, 다른 인스턴스와 통신 불필요
//   - 단점: 실제 총 한도 = 한도 × Distributor 수 (과다 허용)
//
// Global Strategy:
//   - 전체 한도를 Distributor 수로 나누어 적용
//   - 장점: 전체 시스템 기준으로 정확한 제한
//   - 단점: Distributor 수를 알아야 함 (ring 기반 discovery)
//
//   예: 한도 10MB/s, Distributor 5대
//   Local:  각 Distributor가 10MB/s 허용 → 실제 총 50MB/s 가능
//   Global: 각 Distributor가 2MB/s 허용  → 실제 총 10MB/s 가능

// RateLimitStrategy는 레이트 리미팅 전략이다.
type RateLimitStrategy int

const (
	LocalStrategy  RateLimitStrategy = iota // 로컬 전략
	GlobalStrategy                          // 글로벌 전략
)

func (s RateLimitStrategy) String() string {
	if s == LocalStrategy {
		return "LOCAL"
	}
	return "GLOBAL"
}

// =============================================================================
// 4. Distributor 레이트 리미터
// =============================================================================
// 실제 Loki의 Distributor는 테넌트별 레이트 리미터를 관리한다.

// DistributorRateLimiter는 분산 환경의 테넌트별 레이트 리미터이다.
type DistributorRateLimiter struct {
	mu              sync.Mutex
	strategy        RateLimitStrategy
	numDistributors int                        // 전체 Distributor 수 (Global 전략에 사용)
	limiters        map[string]*TokenBucket    // 테넌트별 레이트 리미터
	tenantLimits    map[string]TenantLimits    // 테넌트별 제한 설정
	defaultLimits   TenantLimits               // 기본 제한 설정
	stats           map[string]*TenantStats    // 테넌트별 통계
}

// TenantStats는 테넌트별 통계이다.
type TenantStats struct {
	TotalRequests    int
	AllowedRequests  int
	RejectedRequests int
	TotalBytes       float64
	AllowedBytes     float64
	RejectedBytes    float64
}

// NewDistributorRateLimiter는 새로운 Distributor 레이트 리미터를 생성한다.
func NewDistributorRateLimiter(strategy RateLimitStrategy, numDistributors int) *DistributorRateLimiter {
	return &DistributorRateLimiter{
		strategy:        strategy,
		numDistributors: numDistributors,
		limiters:        make(map[string]*TokenBucket),
		tenantLimits:    make(map[string]TenantLimits),
		defaultLimits:   DefaultLimits(),
		stats:           make(map[string]*TenantStats),
	}
}

// SetTenantLimits는 특정 테넌트의 제한을 설정한다.
func (d *DistributorRateLimiter) SetTenantLimits(tenant string, limits TenantLimits) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.tenantLimits[tenant] = limits
	// 기존 리미터 삭제 (새 설정으로 재생성)
	delete(d.limiters, tenant)
}

// getOrCreateLimiter는 테넌트의 레이트 리미터를 가져오거나 생성한다.
func (d *DistributorRateLimiter) getOrCreateLimiter(tenant string) *TokenBucket {
	limiter, ok := d.limiters[tenant]
	if ok {
		return limiter
	}

	// 테넌트별 제한 가져오기
	limits, ok := d.tenantLimits[tenant]
	if !ok {
		limits = d.defaultLimits
	}

	// 전략에 따라 실제 적용 속도 계산
	effectiveRate := limits.IngestionRate
	effectiveBurst := limits.IngestionBurstSize

	if d.strategy == GlobalStrategy && d.numDistributors > 0 {
		// Global 전략: 전체 한도를 Distributor 수로 나눔
		effectiveRate = limits.IngestionRate / float64(d.numDistributors)
		effectiveBurst = limits.IngestionBurstSize / float64(d.numDistributors)
	}

	limiter = NewTokenBucket(effectiveRate, effectiveBurst)
	d.limiters[tenant] = limiter
	return limiter
}

// getOrCreateStats는 테넌트의 통계를 가져오거나 생성한다.
func (d *DistributorRateLimiter) getOrCreateStats(tenant string) *TenantStats {
	stats, ok := d.stats[tenant]
	if !ok {
		stats = &TenantStats{}
		d.stats[tenant] = stats
	}
	return stats
}

// AllowRequest는 수집 요청을 허용할지 결정한다.
func (d *DistributorRateLimiter) AllowRequest(tenant string, sizeBytes float64) (allowed bool, reason string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	stats := d.getOrCreateStats(tenant)
	stats.TotalRequests++
	stats.TotalBytes += sizeBytes

	// 1. 레이트 리밋 확인
	limiter := d.getOrCreateLimiter(tenant)
	if !limiter.Allow(sizeBytes) {
		stats.RejectedRequests++
		stats.RejectedBytes += sizeBytes
		tokens, capacity, rate := limiter.Status()
		return false, fmt.Sprintf(
			"레이트 초과 (요청: %.0f bytes, 남은 토큰: %.0f/%.0f, 속도: %.0f bytes/s)",
			sizeBytes, tokens, capacity, rate)
	}

	stats.AllowedRequests++
	stats.AllowedBytes += sizeBytes
	return true, "허용"
}

// GetStats는 테넌트의 통계를 반환한다.
func (d *DistributorRateLimiter) GetStats(tenant string) *TenantStats {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.getOrCreateStats(tenant)
}

// =============================================================================
// 5. 유틸리티 함수
// =============================================================================

// formatBytes는 바이트 수를 사람이 읽기 쉬운 형태로 변환한다.
func formatBytes(bytes float64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%.0f B", bytes)
	} else if bytes < 1024*1024 {
		return fmt.Sprintf("%.1f KB", bytes/1024)
	} else if bytes < 1024*1024*1024 {
		return fmt.Sprintf("%.1f MB", bytes/(1024*1024))
	}
	return fmt.Sprintf("%.1f GB", bytes/(1024*1024*1024))
}

// =============================================================================
// 6. 메인 함수 - 레이트 리미터 시연
// =============================================================================

func main() {
	fmt.Println("=== Loki PoC #09: 레이트 리미터 - 테넌트별 수집 속도 제한 ===")
	fmt.Println()

	// =========================================================================
	// 시연 1: Token Bucket 기본 동작
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("[시연 1] Token Bucket 기본 동작")
	fmt.Println()

	// 초당 10개 토큰, 최대 20개 버스트
	bucket := NewTokenBucket(10, 20)

	fmt.Println("  설정: rate=10 tokens/sec, capacity=20 tokens (burst)")
	fmt.Println()

	// 버스트 테스트: 한 번에 많은 토큰 소비
	fmt.Println("  [버스트 테스트] 연속 요청 (각 5 토큰):")
	for i := 0; i < 6; i++ {
		allowed, remaining := bucket.AllowN(5)
		status := "허용"
		if !allowed {
			status = "거부"
		}
		fmt.Printf("    요청 %d: 5 토큰 → %s (남은 토큰: %.1f)\n", i+1, status, remaining)
	}

	// 토큰 보충 대기
	fmt.Println()
	fmt.Println("  [보충 테스트] 500ms 대기 후 재시도...")
	time.Sleep(500 * time.Millisecond) // 10 * 0.5 = 5 토큰 보충

	allowed, remaining := bucket.AllowN(5)
	status := "허용"
	if !allowed {
		status = "거부"
	}
	fmt.Printf("    500ms 후 요청: 5 토큰 → %s (남은 토큰: %.1f)\n", status, remaining)
	fmt.Println()

	// =========================================================================
	// 시연 2: 테넌트별 제한 (Local Strategy)
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("[시연 2] 테넌트별 제한 (Local Strategy)")
	fmt.Println()

	localLimiter := NewDistributorRateLimiter(LocalStrategy, 3)

	// 테넌트별 다른 제한 설정
	localLimiter.SetTenantLimits("tenant-premium", TenantLimits{
		IngestionRate:      10 * 1024, // 10KB/s
		IngestionBurstSize: 20 * 1024, // 20KB 버스트
	})
	localLimiter.SetTenantLimits("tenant-basic", TenantLimits{
		IngestionRate:      2 * 1024, // 2KB/s
		IngestionBurstSize: 4 * 1024, // 4KB 버스트
	})

	fmt.Println("  설정:")
	fmt.Println("    tenant-premium: rate=10KB/s, burst=20KB")
	fmt.Println("    tenant-basic:   rate=2KB/s,  burst=4KB")
	fmt.Println("    전략: LOCAL (각 Distributor가 전체 한도 적용)")
	fmt.Println()

	// 각 테넌트에 대해 요청 시뮬레이션
	tenants := []string{"tenant-premium", "tenant-basic"}
	for _, tenant := range tenants {
		fmt.Printf("  [%s] 연속 요청 (각 3KB):\n", tenant)
		for i := 0; i < 8; i++ {
			allowed, reason := localLimiter.AllowRequest(tenant, 3*1024)
			if allowed {
				fmt.Printf("    요청 %d: 3KB → 허용\n", i+1)
			} else {
				fmt.Printf("    요청 %d: 3KB → 거부 (%s)\n", i+1, reason)
			}
		}
		fmt.Println()
	}

	// 통계 출력
	for _, tenant := range tenants {
		stats := localLimiter.GetStats(tenant)
		fmt.Printf("  [%s] 통계: 총 %d 요청, 허용 %d, 거부 %d (거부율: %.1f%%)\n",
			tenant, stats.TotalRequests, stats.AllowedRequests, stats.RejectedRequests,
			float64(stats.RejectedRequests)/float64(stats.TotalRequests)*100)
	}
	fmt.Println()

	// =========================================================================
	// 시연 3: Local vs Global Strategy 비교
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("[시연 3] Local vs Global Strategy 비교")
	fmt.Println()

	numDistributors := 5
	tenantLimit := TenantLimits{
		IngestionRate:      10 * 1024, // 10KB/s
		IngestionBurstSize: 20 * 1024, // 20KB 버스트
	}

	fmt.Printf("  설정: tenant 한도=10KB/s, Distributor=%d대\n", numDistributors)
	fmt.Println()

	strategies := []struct {
		name     string
		strategy RateLimitStrategy
	}{
		{"LOCAL", LocalStrategy},
		{"GLOBAL", GlobalStrategy},
	}

	for _, s := range strategies {
		limiter := NewDistributorRateLimiter(s.strategy, numDistributors)
		limiter.SetTenantLimits("test-tenant", tenantLimit)

		// 각 Distributor에서 동시에 요청을 보내는 시뮬레이션
		allowedTotal := 0
		rejectedTotal := 0
		requestSize := 3.0 * 1024 // 3KB

		fmt.Printf("  [%s 전략] %d개 Distributor에서 각 3KB 요청 7회:\n", s.name, numDistributors)

		for d := 0; d < numDistributors; d++ {
			allowed := 0
			rejected := 0
			for i := 0; i < 7; i++ {
				ok, _ := limiter.AllowRequest("test-tenant", requestSize)
				if ok {
					allowed++
					allowedTotal++
				} else {
					rejected++
					rejectedTotal++
				}
			}
			fmt.Printf("    Distributor-%d: 허용=%d, 거부=%d\n", d+1, allowed, rejected)
		}

		effectiveRate := tenantLimit.IngestionRate
		if s.strategy == GlobalStrategy {
			effectiveRate = tenantLimit.IngestionRate / float64(numDistributors)
		}
		fmt.Printf("    → 총 허용=%d, 거부=%d, 각 Distributor 실효 속도=%s/s\n",
			allowedTotal, rejectedTotal, formatBytes(effectiveRate))
		fmt.Printf("    → 시스템 전체 실효 속도: %s/s\n",
			formatBytes(effectiveRate*float64(numDistributors)))
		fmt.Println()
	}

	fmt.Println("  비교 분석:")
	fmt.Println("    LOCAL:  각 Distributor가 전체 한도 적용")
	fmt.Printf("            실제 총 한도 = %s/s × %d = %s/s (의도보다 과다)\n",
		formatBytes(tenantLimit.IngestionRate), numDistributors,
		formatBytes(tenantLimit.IngestionRate*float64(numDistributors)))
	fmt.Println("    GLOBAL: 전체 한도를 Distributor 수로 나누어 적용")
	fmt.Printf("            실제 총 한도 = %s/s ÷ %d × %d = %s/s (정확한 제한)\n",
		formatBytes(tenantLimit.IngestionRate), numDistributors, numDistributors,
		formatBytes(tenantLimit.IngestionRate))
	fmt.Println()

	// =========================================================================
	// 시연 4: 버스트 흡수 패턴
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("[시연 4] 버스트 흡수 패턴")
	fmt.Println()

	// 낮은 속도 + 높은 버스트: 간헐적 대량 전송에 적합
	burstBucket := NewTokenBucket(100, 1000) // 100/s, 버스트 1000
	fmt.Println("  설정: rate=100 tokens/s, burst=1000 tokens")
	fmt.Println()

	// 시나리오: 1초 대기 → 대량 전송 → 대기 → 대량 전송
	fmt.Println("  시나리오: 대량 전송 → 대기 → 대량 전송")
	fmt.Println()

	// 첫 번째 버스트: 1000 토큰 (최대 버스트 크기)
	ok, rem := burstBucket.AllowN(800)
	fmt.Printf("    1차 버스트 (800 토큰): %v (남은: %.0f)\n", ok, rem)

	ok, rem = burstBucket.AllowN(300)
	fmt.Printf("    추가 요청 (300 토큰): %v (남은: %.0f) ← 버스트 초과\n", ok, rem)

	// 2초 대기 후 보충
	fmt.Println("    ... 2초 대기 (200 토큰 보충 예상) ...")
	time.Sleep(2 * time.Second)

	ok, rem = burstBucket.AllowN(150)
	fmt.Printf("    2차 요청 (150 토큰): %v (남은: %.0f)\n", ok, rem)

	ok, rem = burstBucket.AllowN(100)
	fmt.Printf("    3차 요청 (100 토큰): %v (남은: %.0f)\n", ok, rem)
	fmt.Println()

	// =========================================================================
	// 시연 5: 동시 다중 테넌트 시뮬레이션
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("[시연 5] 동시 다중 테넌트 시뮬레이션")
	fmt.Println()

	multiLimiter := NewDistributorRateLimiter(GlobalStrategy, 3)

	// 5개 테넌트, 각각 다른 제한
	multiTenants := []struct {
		name   string
		rate   float64 // bytes/sec
		burst  float64
		load   float64 // 초당 전송 시도량
	}{
		{"tenant-A", 10000, 20000, 8000},   // 한도 내 사용
		{"tenant-B", 10000, 20000, 15000},  // 한도 초과
		{"tenant-C", 5000, 10000, 3000},    // 한도 내 사용
		{"tenant-D", 5000, 10000, 12000},   // 한도 대폭 초과
		{"tenant-E", 20000, 40000, 25000},  // 한도 약간 초과
	}

	for _, t := range multiTenants {
		multiLimiter.SetTenantLimits(t.name, TenantLimits{
			IngestionRate:      t.rate,
			IngestionBurstSize: t.burst,
		})
	}

	fmt.Println("  설정 (Global 전략, 3대 Distributor):")
	for _, t := range multiTenants {
		effectiveRate := t.rate / 3.0
		overRatio := t.load / t.rate * 100
		fmt.Printf("    %s: 한도=%s/s (실효=%s/s), 시도=%s/s (%.0f%% 사용)\n",
			t.name, formatBytes(t.rate), formatBytes(effectiveRate),
			formatBytes(t.load), overRatio)
	}
	fmt.Println()

	// 각 테넌트에 대해 100회 요청 시뮬레이션
	fmt.Println("  시뮬레이션 결과 (각 100회 요청):")
	for _, t := range multiTenants {
		requestSize := t.load / 100.0 // 100회로 나눠서 전송
		for i := 0; i < 100; i++ {
			multiLimiter.AllowRequest(t.name, requestSize)
		}

		stats := multiLimiter.GetStats(t.name)
		rejectRate := float64(stats.RejectedRequests) / float64(stats.TotalRequests) * 100
		fmt.Printf("    %s: 허용=%3d, 거부=%3d (거부율: %5.1f%%), 허용량=%s\n",
			t.name, stats.AllowedRequests, stats.RejectedRequests,
			rejectRate, formatBytes(stats.AllowedBytes))
	}
	fmt.Println()

	// =========================================================================
	// 시연 6: Token Bucket 시각화
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("[시연 6] Token Bucket 시각화")
	fmt.Println()

	vizBucket := NewTokenBucket(5, 20) // 5/s, 최대 20
	fmt.Println("  설정: rate=5 tokens/s, capacity=20")
	fmt.Println()

	// 시각적으로 토큰 상태 표시
	vizActions := []struct {
		consume float64
		wait    time.Duration
		desc    string
	}{
		{0, 0, "초기 상태"},
		{8, 0, "8 토큰 소비"},
		{5, 0, "5 토큰 소비"},
		{0, 400 * time.Millisecond, "400ms 대기"},
		{3, 0, "3 토큰 소비"},
		{0, 1 * time.Second, "1초 대기"},
		{10, 0, "10 토큰 소비 시도"},
		{0, 2 * time.Second, "2초 대기"},
		{5, 0, "5 토큰 소비"},
	}

	for _, action := range vizActions {
		if action.wait > 0 {
			time.Sleep(action.wait)
			tokens, capacity, _ := vizBucket.Status()
			bar := renderBar(tokens, capacity)
			fmt.Printf("  %-20s %s  [%.1f/%.0f]\n", action.desc, bar, tokens, capacity)
		} else if action.consume > 0 {
			ok := vizBucket.Allow(action.consume)
			result := "성공"
			if !ok {
				result = "실패"
			}
			tokens, capacity, _ := vizBucket.Status()
			bar := renderBar(tokens, capacity)
			fmt.Printf("  %-20s %s  [%.1f/%.0f] (%s)\n",
				action.desc, bar, tokens, capacity, result)
		} else {
			tokens, capacity, _ := vizBucket.Status()
			bar := renderBar(tokens, capacity)
			fmt.Printf("  %-20s %s  [%.1f/%.0f]\n", action.desc, bar, tokens, capacity)
		}
	}

	// =========================================================================
	// 구조 요약
	// =========================================================================
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("=== 레이트 리미터 구조 요약 ===")
	fmt.Println()
	fmt.Println("  Loki 레이트 리미팅 아키텍처:")
	fmt.Println()
	fmt.Println("  ┌─────────────────────────────────────────────┐")
	fmt.Println("  │ Push Request (tenant-id: X, size: N bytes)  │")
	fmt.Println("  └────────────────────┬────────────────────────┘")
	fmt.Println("                       │")
	fmt.Println("                       ▼")
	fmt.Println("  ┌─────────────────────────────────────────────┐")
	fmt.Println("  │ Distributor                                 │")
	fmt.Println("  │  ┌───────────────────────────────────────┐  │")
	fmt.Println("  │  │ 1. 테넌트 식별 (X-Scope-OrgID 헤더)   │  │")
	fmt.Println("  │  │ 2. 테넌트별 제한 조회                  │  │")
	fmt.Println("  │  │ 3. Token Bucket 확인                  │  │")
	fmt.Println("  │  │    - Local: 전체 한도 적용             │  │")
	fmt.Println("  │  │    - Global: 한도 ÷ Distributor 수     │  │")
	fmt.Println("  │  │ 4. 허용/거부 결정                     │  │")
	fmt.Println("  │  └───────────────────────────────────────┘  │")
	fmt.Println("  └─────────────────────────────────────────────┘")
	fmt.Println()
	fmt.Println("  핵심 포인트:")
	fmt.Println("    - Token Bucket: 일정 속도로 토큰 보충, 요청 시 소비")
	fmt.Println("    - 버스트 지원: capacity까지 순간 처리 가능")
	fmt.Println("    - 테넌트 격리: 각 테넌트 독립적인 레이트 리미터")
	fmt.Println("    - Global 전략: 분산 환경에서 정확한 전체 한도 유지")
	fmt.Println("    - Loki 실제 코드: pkg/distributor/distributor.go")
}

// renderBar는 토큰 상태를 시각적 바로 렌더링한다.
func renderBar(tokens, capacity float64) string {
	barWidth := 30
	filled := int(math.Round(tokens / capacity * float64(barWidth)))
	if filled < 0 {
		filled = 0
	}
	if filled > barWidth {
		filled = barWidth
	}
	empty := barWidth - filled

	bar := "["
	for i := 0; i < filled; i++ {
		bar += "#"
	}
	for i := 0; i < empty; i++ {
		bar += "."
	}
	bar += "]"
	return bar
}
