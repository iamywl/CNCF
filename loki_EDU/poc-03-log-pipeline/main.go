package main

import (
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// =============================================================================
// Loki 로그 파이프라인 시뮬레이션
// =============================================================================
//
// Loki의 Distributor가 로그를 수집하여 Ingester로 분배하는 전체 파이프라인을
// 시뮬레이션한다. 실제 Loki에서 Push API를 통해 로그가 유입되면 다음 과정을 거친다:
//
//   1. 유효성 검사: 타임스탬프, 라인 크기, 레이블 수 등 확인
//   2. 속도 제한: 테넌트별 token bucket 기반 rate limiting
//   3. 해시 계산: 레이블 세트를 해시하여 스트림 키 생성
//   4. 링 라우팅: Consistent Hash Ring으로 담당 Ingester 결정
//   5. 복제 전송: 복제 인자만큼 여러 Ingester에 전송
//   6. 멀티 테넌트 격리: 테넌트 간 데이터/자원 격리
//
// Loki 실제 구현 참조:
//   - pkg/distributor/distributor.go: Push(), validateEntry()
//   - pkg/validation/validate.go: 유효성 검사 규칙
//   - pkg/distributor/ratestore.go: Rate limiting
//   - dskit/ring/ring.go: Hash ring 기반 라우팅
// =============================================================================

// LogEntry는 하나의 로그 엔트리를 나타낸다.
// Loki의 logproto.Entry와 동일한 구조이다.
type LogEntry struct {
	Timestamp time.Time         // 타임스탬프
	Line      string            // 로그 라인
	Labels    map[string]string // 레이블 세트 (예: app=nginx, level=error)
	TenantID  string            // 테넌트 ID (멀티 테넌시)
}

// StreamKey는 레이블 세트로 결정되는 스트림의 고유 키이다.
func (e *LogEntry) StreamKey() string {
	// 레이블을 정렬하여 일관된 키 생성
	keys := make([]string, 0, len(e.Labels))
	for k := range e.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%q", k, e.Labels[k]))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// =============================================================================
// 유효성 검사기 (Validator)
// Loki의 pkg/validation/validate.go에 해당
// =============================================================================

// ValidationLimits는 유효성 검사 제한을 정의한다.
// Loki에서는 per-tenant limits로 관리된다.
type ValidationLimits struct {
	MaxLineSize      int           // 최대 라인 크기 (바이트)
	MaxLabelCount    int           // 최대 레이블 수
	MaxLabelNameLen  int           // 최대 레이블 이름 길이
	MaxLabelValueLen int           // 최대 레이블 값 길이
	RejectOldSamples bool          // 오래된 샘플 거부 여부
	MaxAge           time.Duration // 최대 허용 나이 (현재 시간 기준)
	RejectFuture     bool          // 미래 타임스탬프 거부 여부
	MaxFutureGrace   time.Duration // 미래 허용 여유 시간
}

// DefaultLimits는 기본 유효성 검사 제한이다.
func DefaultLimits() ValidationLimits {
	return ValidationLimits{
		MaxLineSize:      256 * 1024, // 256KB
		MaxLabelCount:    30,
		MaxLabelNameLen:  1024,
		MaxLabelValueLen: 2048,
		RejectOldSamples: true,
		MaxAge:           1 * time.Hour,
		RejectFuture:     true,
		MaxFutureGrace:   10 * time.Minute,
	}
}

// ValidationError는 유효성 검사 실패를 나타낸다.
type ValidationError struct {
	Reason string
	Entry  *LogEntry
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("유효성 검사 실패 [%s]: tenant=%s, stream=%s",
		e.Reason, e.Entry.TenantID, e.Entry.StreamKey())
}

// Validator는 로그 엔트리의 유효성을 검사한다.
type Validator struct {
	limits ValidationLimits
}

// NewValidator는 새 Validator를 생성한다.
func NewValidator(limits ValidationLimits) *Validator {
	return &Validator{limits: limits}
}

// Validate는 로그 엔트리의 유효성을 검사한다.
// Loki의 distributor.validateEntry()와 동일한 검사를 수행한다.
func (v *Validator) Validate(entry *LogEntry) error {
	now := time.Now()

	// 1. 라인 크기 검사
	if len(entry.Line) > v.limits.MaxLineSize {
		return &ValidationError{
			Reason: fmt.Sprintf("라인 크기 초과 (%d > %d 바이트)", len(entry.Line), v.limits.MaxLineSize),
			Entry:  entry,
		}
	}

	// 2. 레이블 수 검사
	if len(entry.Labels) > v.limits.MaxLabelCount {
		return &ValidationError{
			Reason: fmt.Sprintf("레이블 수 초과 (%d > %d)", len(entry.Labels), v.limits.MaxLabelCount),
			Entry:  entry,
		}
	}

	// 3. 레이블 이름/값 길이 검사
	for name, value := range entry.Labels {
		if len(name) > v.limits.MaxLabelNameLen {
			return &ValidationError{
				Reason: fmt.Sprintf("레이블 이름 길이 초과 (%s: %d > %d)", name, len(name), v.limits.MaxLabelNameLen),
				Entry:  entry,
			}
		}
		if len(value) > v.limits.MaxLabelValueLen {
			return &ValidationError{
				Reason: fmt.Sprintf("레이블 값 길이 초과 (%s: %d > %d)", name, len(value), v.limits.MaxLabelValueLen),
				Entry:  entry,
			}
		}
	}

	// 4. 타임스탬프 검사 — 너무 오래된 로그 거부
	if v.limits.RejectOldSamples {
		oldest := now.Add(-v.limits.MaxAge)
		if entry.Timestamp.Before(oldest) {
			return &ValidationError{
				Reason: fmt.Sprintf("타임스탬프 너무 오래됨 (max_age=%v)", v.limits.MaxAge),
				Entry:  entry,
			}
		}
	}

	// 5. 타임스탬프 검사 — 미래 타임스탬프 거부
	if v.limits.RejectFuture {
		maxFuture := now.Add(v.limits.MaxFutureGrace)
		if entry.Timestamp.After(maxFuture) {
			return &ValidationError{
				Reason: fmt.Sprintf("미래 타임스탬프 (grace=%v)", v.limits.MaxFutureGrace),
				Entry:  entry,
			}
		}
	}

	return nil
}

// =============================================================================
// Token Bucket Rate Limiter
// Loki의 pkg/distributor/ratestore.go에 해당
// =============================================================================

// RateLimiter는 테넌트별 속도 제한을 구현한다.
// Token Bucket 알고리즘을 사용한다.
type RateLimiter struct {
	mu         sync.Mutex
	rate       float64   // 초당 토큰 생성 속도 (바이트/초)
	burst      float64   // 버스트 허용량 (최대 토큰 수)
	tokens     float64   // 현재 토큰 수
	lastRefill time.Time // 마지막 토큰 리필 시간
}

// NewRateLimiter는 새 RateLimiter를 생성한다.
func NewRateLimiter(rate, burst float64) *RateLimiter {
	return &RateLimiter{
		rate:       rate,
		burst:      burst,
		tokens:     burst, // 초기에 버스트만큼 토큰 보유
		lastRefill: time.Now(),
	}
}

// Allow는 주어진 크기의 요청이 허용되는지 확인한다.
// 허용되면 토큰을 소비하고 true를 반환한다.
func (rl *RateLimiter) Allow(size float64) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	// 토큰 리필 (경과 시간에 비례)
	now := time.Now()
	elapsed := now.Sub(rl.lastRefill).Seconds()
	rl.tokens += elapsed * rl.rate
	if rl.tokens > rl.burst {
		rl.tokens = rl.burst
	}
	rl.lastRefill = now

	// 토큰 소비
	if rl.tokens >= size {
		rl.tokens -= size
		return true
	}
	return false
}

// =============================================================================
// 간단한 Hash Ring (PoC-02에서 가져온 핵심 로직)
// =============================================================================

type ringToken struct {
	hash   uint32
	nodeID string
}

type SimpleRing struct {
	tokens          []ringToken
	replicationFactor int
}

func NewSimpleRing(replicationFactor int) *SimpleRing {
	return &SimpleRing{replicationFactor: replicationFactor}
}

func hashString(s string) uint32 {
	h := md5.Sum([]byte(s))
	return binary.BigEndian.Uint32(h[:4])
}

func (r *SimpleRing) AddNode(id string, numTokens int) {
	for i := 0; i < numTokens; i++ {
		key := fmt.Sprintf("%s-vnode-%d", id, i)
		r.tokens = append(r.tokens, ringToken{hash: hashString(key), nodeID: id})
	}
	sort.Slice(r.tokens, func(i, j int) bool {
		return r.tokens[i].hash < r.tokens[j].hash
	})
}

func (r *SimpleRing) GetReplicaNodes(key string) []string {
	if len(r.tokens) == 0 {
		return nil
	}
	hash := hashString(key)
	startIdx := sort.Search(len(r.tokens), func(i int) bool {
		return r.tokens[i].hash >= hash
	})
	if startIdx >= len(r.tokens) {
		startIdx = 0
	}

	seen := make(map[string]bool)
	var result []string
	for i := 0; i < len(r.tokens) && len(result) < r.replicationFactor; i++ {
		idx := (startIdx + i) % len(r.tokens)
		nodeID := r.tokens[idx].nodeID
		if !seen[nodeID] {
			seen[nodeID] = true
			result = append(result, nodeID)
		}
	}
	return result
}

// =============================================================================
// Ingester 시뮬레이션
// =============================================================================

// Ingester는 로그를 수신하여 저장하는 컴포넌트이다.
type Ingester struct {
	ID       string
	mu       sync.Mutex
	streams  map[string][]LogEntry // 스트림 키 → 로그 엔트리 목록
	received int32                 // 수신한 총 엔트리 수 (atomic)
}

func NewIngester(id string) *Ingester {
	return &Ingester{
		ID:      id,
		streams: make(map[string][]LogEntry),
	}
}

func (ing *Ingester) Push(entry LogEntry) {
	ing.mu.Lock()
	defer ing.mu.Unlock()
	key := entry.StreamKey()
	ing.streams[key] = append(ing.streams[key], entry)
	atomic.AddInt32(&ing.received, 1)
}

func (ing *Ingester) Stats() (int, int) {
	ing.mu.Lock()
	defer ing.mu.Unlock()
	return len(ing.streams), int(atomic.LoadInt32(&ing.received))
}

// =============================================================================
// Distributor — 로그 파이프라인의 핵심
// =============================================================================

// PipelineStats는 파이프라인 처리 통계이다.
type PipelineStats struct {
	Total         int32
	Validated     int32
	RateLimited   int32
	ValidationErr int32
	Distributed   int32
}

// Distributor는 로그 수집 → 검증 → 분배 파이프라인을 구현한다.
type Distributor struct {
	validator    *Validator
	rateLimiters map[string]*RateLimiter // 테넌트별 rate limiter
	ring         *SimpleRing
	ingesters    map[string]*Ingester
	stats        PipelineStats
	mu           sync.Mutex
	ratePerTenant float64 // 테넌트당 초당 허용 바이트
	burstPerTenant float64 // 테넌트당 버스트 허용량
}

// NewDistributor는 새 Distributor를 생성한다.
func NewDistributor(limits ValidationLimits, ring *SimpleRing, ingesters map[string]*Ingester, ratePerTenant, burstPerTenant float64) *Distributor {
	return &Distributor{
		validator:      NewValidator(limits),
		rateLimiters:   make(map[string]*RateLimiter),
		ring:           ring,
		ingesters:      ingesters,
		ratePerTenant:  ratePerTenant,
		burstPerTenant: burstPerTenant,
	}
}

// getRateLimiter는 테넌트별 rate limiter를 반환한다 (없으면 생성).
func (d *Distributor) getRateLimiter(tenantID string) *RateLimiter {
	d.mu.Lock()
	defer d.mu.Unlock()

	rl, ok := d.rateLimiters[tenantID]
	if !ok {
		rl = NewRateLimiter(d.ratePerTenant, d.burstPerTenant)
		d.rateLimiters[tenantID] = rl
	}
	return rl
}

// Push는 로그 엔트리를 파이프라인에 투입한다.
// Loki의 distributor.Push()와 동일한 흐름이다.
func (d *Distributor) Push(entry LogEntry) error {
	atomic.AddInt32(&d.stats.Total, 1)

	// ── 단계 1: 유효성 검사 ──
	if err := d.validator.Validate(&entry); err != nil {
		atomic.AddInt32(&d.stats.ValidationErr, 1)
		return err
	}
	atomic.AddInt32(&d.stats.Validated, 1)

	// ── 단계 2: 속도 제한 (테넌트별) ──
	rl := d.getRateLimiter(entry.TenantID)
	if !rl.Allow(float64(len(entry.Line))) {
		atomic.AddInt32(&d.stats.RateLimited, 1)
		return fmt.Errorf("속도 제한 초과: tenant=%s", entry.TenantID)
	}

	// ── 단계 3: 링 기반 라우팅 ──
	streamKey := entry.StreamKey()
	replicaNodes := d.ring.GetReplicaNodes(streamKey)

	// ── 단계 4: 복제 전송 ──
	for _, nodeID := range replicaNodes {
		if ing, ok := d.ingesters[nodeID]; ok {
			ing.Push(entry)
		}
	}

	atomic.AddInt32(&d.stats.Distributed, 1)
	return nil
}

// GetStats는 파이프라인 통계를 반환한다.
func (d *Distributor) GetStats() PipelineStats {
	return PipelineStats{
		Total:         atomic.LoadInt32(&d.stats.Total),
		Validated:     atomic.LoadInt32(&d.stats.Validated),
		RateLimited:   atomic.LoadInt32(&d.stats.RateLimited),
		ValidationErr: atomic.LoadInt32(&d.stats.ValidationErr),
		Distributed:   atomic.LoadInt32(&d.stats.Distributed),
	}
}

func main() {
	fmt.Println("=================================================================")
	fmt.Println("  Loki 로그 파이프라인 시뮬레이션")
	fmt.Println("  - 유효성 검사 → 속도 제한 → 링 라우팅 → 복제 전송")
	fmt.Println("  - 멀티 테넌트 격리")
	fmt.Println("=================================================================")
	fmt.Println()

	// =========================================================================
	// 인프라 구성
	// =========================================================================

	// Hash Ring 구성 (3 Ingester, 복제 인자 3)
	ring := NewSimpleRing(3)
	ingesters := make(map[string]*Ingester)
	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("ingester-%d", i)
		ring.AddNode(id, 64)
		ingesters[id] = NewIngester(id)
	}

	// Distributor 구성
	limits := DefaultLimits()
	// 테스트를 위해 제한을 줄임
	limits.MaxLineSize = 100     // 100바이트
	limits.MaxLabelCount = 5     // 최대 5개 레이블
	limits.MaxAge = 30 * time.Minute

	dist := NewDistributor(limits, ring, ingesters,
		5000,  // 테넌트당 5000 바이트/초
		10000, // 버스트 10000 바이트
	)

	// =========================================================================
	// 시나리오 1: 정상 로그 처리
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println(" 시나리오 1: 정상 로그 처리")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	now := time.Now()
	normalEntries := []LogEntry{
		{
			Timestamp: now,
			Line:      `level=info msg="GET /api/users 200 12ms"`,
			Labels:    map[string]string{"app": "api-server", "level": "info"},
			TenantID:  "team-alpha",
		},
		{
			Timestamp: now.Add(-1 * time.Second),
			Line:      `level=error msg="connection refused" host="db-primary"`,
			Labels:    map[string]string{"app": "api-server", "level": "error"},
			TenantID:  "team-alpha",
		},
		{
			Timestamp: now.Add(-2 * time.Second),
			Line:      `level=warn msg="high latency detected" p99=450ms`,
			Labels:    map[string]string{"app": "worker", "level": "warn"},
			TenantID:  "team-beta",
		},
		{
			Timestamp: now.Add(-5 * time.Second),
			Line:      `level=info msg="batch processed" items=1500 duration=2.3s`,
			Labels:    map[string]string{"app": "batch-job", "level": "info"},
			TenantID:  "team-beta",
		},
	}

	for _, entry := range normalEntries {
		if err := dist.Push(entry); err != nil {
			fmt.Printf("  [실패] %v\n", err)
		} else {
			streamKey := entry.StreamKey()
			replicas := ring.GetReplicaNodes(streamKey)
			fmt.Printf("  [성공] tenant=%-12s 스트림=%-45s → [%s]\n",
				entry.TenantID, streamKey, strings.Join(replicas, ", "))
		}
	}

	fmt.Println()

	// =========================================================================
	// 시나리오 2: 유효성 검사 실패 케이스
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println(" 시나리오 2: 유효성 검사 실패 케이스")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	invalidEntries := []struct {
		name  string
		entry LogEntry
	}{
		{
			name: "라인 크기 초과",
			entry: LogEntry{
				Timestamp: now,
				Line:      strings.Repeat("x", 200), // 200 > 100 바이트 제한
				Labels:    map[string]string{"app": "test"},
				TenantID:  "team-alpha",
			},
		},
		{
			name: "레이블 수 초과",
			entry: LogEntry{
				Timestamp: now,
				Line:      "test log",
				Labels: map[string]string{
					"label1": "v1", "label2": "v2", "label3": "v3",
					"label4": "v4", "label5": "v5", "label6": "v6", // 6 > 5 제한
				},
				TenantID: "team-alpha",
			},
		},
		{
			name: "타임스탬프 너무 오래됨",
			entry: LogEntry{
				Timestamp: now.Add(-2 * time.Hour), // 2시간 전 > 30분 제한
				Line:      "old log line",
				Labels:    map[string]string{"app": "test"},
				TenantID:  "team-alpha",
			},
		},
		{
			name: "미래 타임스탬프",
			entry: LogEntry{
				Timestamp: now.Add(1 * time.Hour), // 1시간 후 > 10분 유예
				Line:      "future log line",
				Labels:    map[string]string{"app": "test"},
				TenantID:  "team-alpha",
			},
		},
	}

	for _, tc := range invalidEntries {
		err := dist.Push(tc.entry)
		if err != nil {
			fmt.Printf("  [거부] %-20s → %v\n", tc.name, err)
		} else {
			fmt.Printf("  [통과] %-20s (예상치 못한 통과)\n", tc.name)
		}
	}
	fmt.Println()

	// =========================================================================
	// 시나리오 3: 테넌트별 속도 제한
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println(" 시나리오 3: 테넌트별 속도 제한 (Token Bucket)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	// 새 Distributor (낮은 rate limit으로 테스트)
	dist2 := NewDistributor(DefaultLimits(), ring, ingesters,
		100,  // 100 바이트/초
		500,  // 버스트 500 바이트
	)

	tenantA := "rate-test-tenant"
	accepted, rejected := 0, 0
	lineSize := 80 // 각 라인 80바이트

	// 빠르게 20개 요청을 보냄 (80 * 20 = 1600 바이트, 버스트 500 초과)
	for i := 0; i < 20; i++ {
		entry := LogEntry{
			Timestamp: now,
			Line:      fmt.Sprintf("log line %02d: %s", i, strings.Repeat(".", lineSize-20)),
			Labels:    map[string]string{"app": "rate-test"},
			TenantID:  tenantA,
		}
		if err := dist2.Push(entry); err != nil {
			rejected++
		} else {
			accepted++
		}
	}

	fmt.Printf("  테넌트: %s\n", tenantA)
	fmt.Printf("  설정: rate=100 B/s, burst=500 B, 라인크기=%d B\n", lineSize)
	fmt.Printf("  요청: 20개 (총 %d 바이트)\n", lineSize*20)
	fmt.Printf("  결과: 허용=%d, 거부=%d\n", accepted, rejected)
	fmt.Println()

	// 다른 테넌트는 독립적으로 처리됨을 보여줌
	tenantB := "rate-test-tenant-2"
	accepted2 := 0
	for i := 0; i < 5; i++ {
		entry := LogEntry{
			Timestamp: now,
			Line:      fmt.Sprintf("tenant-B log %d", i),
			Labels:    map[string]string{"app": "other-app"},
			TenantID:  tenantB,
		}
		if err := dist2.Push(entry); err == nil {
			accepted2++
		}
	}
	fmt.Printf("  다른 테넌트(%s)는 독립 처리: 5개 중 %d개 허용\n", tenantB, accepted2)
	fmt.Println()

	// =========================================================================
	// 시나리오 4: 대량 로그 처리 (멀티 테넌트)
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println(" 시나리오 4: 대량 로그 처리 (멀티 테넌트 시뮬레이션)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	// 새 Distributor (높은 rate limit)
	freshIngesters := make(map[string]*Ingester)
	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("ingester-%d", i)
		freshIngesters[id] = NewIngester(id)
	}
	dist3 := NewDistributor(DefaultLimits(), ring, freshIngesters,
		1000000, // 1MB/초
		5000000, // 5MB 버스트
	)

	tenants := []string{"org-frontend", "org-backend", "org-infra"}
	apps := []string{"nginx", "api", "worker", "db", "cache"}
	levels := []string{"info", "warn", "error", "debug"}

	rng := rand.New(rand.NewSource(42))
	totalEntries := 1000
	var errors int32

	for i := 0; i < totalEntries; i++ {
		tenant := tenants[rng.Intn(len(tenants))]
		app := apps[rng.Intn(len(apps))]
		level := levels[rng.Intn(len(levels))]

		entry := LogEntry{
			Timestamp: now.Add(-time.Duration(rng.Intn(600)) * time.Second),
			Line:      fmt.Sprintf(`msg="request processed" request_id=%d status=200`, rng.Intn(100000)),
			Labels:    map[string]string{"app": app, "level": level},
			TenantID:  tenant,
		}

		if err := dist3.Push(entry); err != nil {
			atomic.AddInt32(&errors, 1)
		}
	}

	stats := dist3.GetStats()
	fmt.Printf("  파이프라인 통계:\n")
	fmt.Printf("  ┌──────────────────┬──────────┐\n")
	fmt.Printf("  │ 항목             │ 수량     │\n")
	fmt.Printf("  ├──────────────────┼──────────┤\n")
	fmt.Printf("  │ 총 수신          │ %8d │\n", stats.Total)
	fmt.Printf("  │ 유효성 통과      │ %8d │\n", stats.Validated)
	fmt.Printf("  │ 유효성 실패      │ %8d │\n", stats.ValidationErr)
	fmt.Printf("  │ 속도 제한        │ %8d │\n", stats.RateLimited)
	fmt.Printf("  │ 분배 완료        │ %8d │\n", stats.Distributed)
	fmt.Printf("  └──────────────────┴──────────┘\n")
	fmt.Println()

	// Ingester별 통계
	fmt.Println("  Ingester별 수신 통계 (복제 인자=3):")
	for id, ing := range freshIngesters {
		streams, entries := ing.Stats()
		fmt.Printf("    %-12s: 스트림 %3d개, 엔트리 %5d개\n", id, streams, entries)
	}

	// 테넌트별 격리 확인
	fmt.Println()
	fmt.Println("  테넌트별 격리:")
	tenantCounts := make(map[string]int)
	for _, ing := range freshIngesters {
		ing.mu.Lock()
		for _, entries := range ing.streams {
			for _, e := range entries {
				tenantCounts[e.TenantID]++
			}
		}
		ing.mu.Unlock()
	}
	for tenant, count := range tenantCounts {
		pct := float64(count) / float64(stats.Distributed*3) * 100 // 복제 인자 3
		fmt.Printf("    %-15s: %5d 엔트리 (%.1f%%)\n", tenant, count/3, pct)
	}

	fmt.Println()
	fmt.Println("=================================================================")
	fmt.Println("  시뮬레이션 완료")
	fmt.Println()
	fmt.Println("  Loki 로그 파이프라인 흐름:")
	fmt.Println("  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌──────────┐")
	fmt.Println("  │ Push API │→│ Validate │→│  Rate   │→│  Route   │")
	fmt.Println("  │          │  │          │  │  Limit  │  │ (Ring)   │")
	fmt.Println("  └──────────┘  └──────────┘  └──────────┘  └────┬─────┘")
	fmt.Println("                                                  │")
	fmt.Println("                              ┌──────────┬────────┴────────┐")
	fmt.Println("                              ▼          ▼                 ▼")
	fmt.Println("                         Ingester-1  Ingester-2      Ingester-3")
	fmt.Println("                         (replica)   (replica)       (replica)")
	fmt.Println()
	fmt.Println("  핵심 포인트:")
	fmt.Println("  1. 유효성 검사로 잘못된 로그를 빠르게 거부")
	fmt.Println("  2. Token Bucket으로 테넌트별 독립적 속도 제한")
	fmt.Println("  3. Consistent Hash Ring으로 안정적인 스트림 라우팅")
	fmt.Println("  4. 복제 인자만큼 여러 Ingester에 복제하여 내구성 확보")
	fmt.Println("  5. 멀티 테넌시로 테넌트 간 완전한 격리")
	fmt.Println("=================================================================")
}
