package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// =============================================================================
// Loki PoC #16: 멀티테넌트 - 테넌트 격리, 리소스 제한, 데이터 분리
// =============================================================================
//
// Loki는 멀티테넌트 아키텍처를 기반으로 설계되어 있다.
// 각 테넌트는 완전히 격리된 데이터 공간과 리소스 제한을 갖는다.
//
// 핵심 개념:
// 1. 테넌트 ID: X-Scope-OrgID 헤더로 식별
// 2. per-tenant 인스턴스: 테넌트별 독립적인 스트림 맵
// 3. per-tenant 제한: 수집 속도, 스트림 수 제한
// 4. 런타임 설정 오버라이드: 테넌트별 설정 동적 변경
// 5. 테넌트 범위 쿼리 격리: 쿼리가 자신의 데이터만 접근
//
// 참조: pkg/ingester/instance.go, pkg/validation/limits.go

// =============================================================================
// LogStream: 로그 스트림
// =============================================================================
// Loki 실제 코드: pkg/ingester/stream.go
// 하나의 레이블 조합에 대응하는 로그 시퀀스

type LogStream struct {
	Labels    map[string]string
	Entries   []LogEntry
	CreatedAt time.Time
	BytesUsed int64
}

type LogEntry struct {
	Timestamp time.Time
	Line      string
}

func (s *LogStream) LabelString() string {
	keys := make([]string, 0, len(s.Labels))
	for k := range s.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%q", k, s.Labels[k])
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func (s *LogStream) Push(entry LogEntry) {
	s.Entries = append(s.Entries, entry)
	s.BytesUsed += int64(len(entry.Line))
}

// =============================================================================
// TenantLimits: 테넌트별 리소스 제한
// =============================================================================
// Loki 실제 코드: pkg/validation/limits.go
//
// Loki는 테넌트별로 다양한 제한을 설정할 수 있다:
// - 수집 속도 제한 (ingestion rate, burst size)
// - 스트림 수 제한 (max streams per user)
// - 쿼리 제한 (max query length, max query lookback)
// - 보존 기간 등

type TenantLimits struct {
	MaxStreamsPerTenant  int           // 테넌트당 최대 스트림 수
	IngestionRateLimit  float64       // 초당 바이트 수집 제한
	IngestionBurstSize  int           // 버스트 허용 크기
	MaxLabelNameLength  int           // 레이블 이름 최대 길이
	MaxLabelValueLength int           // 레이블 값 최대 길이
	MaxLabelCount       int           // 레이블 최대 개수
	RetentionPeriod     time.Duration // 보존 기간
}

// DefaultLimits 는 기본 제한값을 반환한다
func DefaultLimits() *TenantLimits {
	return &TenantLimits{
		MaxStreamsPerTenant:  100,
		IngestionRateLimit:  10000, // 10KB/s
		IngestionBurstSize:  50000, // 50KB
		MaxLabelNameLength:  1024,
		MaxLabelValueLength: 2048,
		MaxLabelCount:       30,
		RetentionPeriod:     7 * 24 * time.Hour, // 7일
	}
}

// =============================================================================
// RuntimeConfig: 런타임 설정 오버라이드
// =============================================================================
// Loki 실제 코드: pkg/runtime/config.go
//
// 런타임 설정은 재시작 없이 테넌트별 제한을 동적으로 변경할 수 있다.
// 일반적으로 파일 또는 ConfigMap을 통해 주기적으로 리로드된다.

type RuntimeConfig struct {
	mu             sync.RWMutex
	defaultLimits  *TenantLimits
	tenantOverrides map[string]*TenantLimits
}

func NewRuntimeConfig(defaults *TenantLimits) *RuntimeConfig {
	return &RuntimeConfig{
		defaultLimits:   defaults,
		tenantOverrides: make(map[string]*TenantLimits),
	}
}

// SetOverride 는 특정 테넌트의 제한을 오버라이드한다
func (rc *RuntimeConfig) SetOverride(tenantID string, limits *TenantLimits) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.tenantOverrides[tenantID] = limits
}

// GetLimits 는 테넌트의 실제 제한을 반환한다 (오버라이드 우선)
func (rc *RuntimeConfig) GetLimits(tenantID string) *TenantLimits {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	if override, ok := rc.tenantOverrides[tenantID]; ok {
		return override
	}
	return rc.defaultLimits
}

// =============================================================================
// RateLimiter: 테넌트별 수집 속도 제한기
// =============================================================================
// Loki 실제 코드: pkg/ingester/limiter.go
// 토큰 버킷 알고리즘 기반 속도 제한

type RateLimiter struct {
	mu         sync.Mutex
	rate       float64   // 초당 토큰 생성량 (바이트/초)
	burst      int       // 최대 버스트 크기
	tokens     float64   // 현재 토큰 수
	lastRefill time.Time // 마지막 토큰 보충 시간
}

func NewRateLimiter(rate float64, burst int) *RateLimiter {
	return &RateLimiter{
		rate:       rate,
		burst:      burst,
		tokens:     float64(burst),
		lastRefill: time.Now(),
	}
}

// Allow 는 주어진 바이트 수를 허용할 수 있는지 확인한다
func (rl *RateLimiter) Allow(bytes int) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(rl.lastRefill).Seconds()
	rl.tokens += elapsed * rl.rate
	if rl.tokens > float64(rl.burst) {
		rl.tokens = float64(rl.burst)
	}
	rl.lastRefill = now

	if rl.tokens >= float64(bytes) {
		rl.tokens -= float64(bytes)
		return true
	}
	return false
}

// =============================================================================
// TenantInstance: 테넌트별 인스턴스
// =============================================================================
// Loki 실제 코드: pkg/ingester/instance.go
//
// 각 테넌트는 독립적인 instance를 갖는다:
// - streams: 테넌트의 모든 스트림
// - index: 테넌트의 인덱스
// - limiter: 테넌트의 리소스 제한기

type TenantInstance struct {
	mu          sync.RWMutex
	TenantID    string
	streams     map[string]*LogStream // 레이블 문자열 → 스트림
	rateLimiter *RateLimiter
	limits      *TenantLimits

	// 통계
	totalEntries    int64
	totalBytes      int64
	rejectedEntries int64
	rejectedReason  map[string]int64
}

func NewTenantInstance(tenantID string, limits *TenantLimits) *TenantInstance {
	return &TenantInstance{
		TenantID:       tenantID,
		streams:        make(map[string]*LogStream),
		rateLimiter:    NewRateLimiter(limits.IngestionRateLimit, limits.IngestionBurstSize),
		limits:         limits,
		rejectedReason: make(map[string]int64),
	}
}

// Push 는 로그 엔트리를 테넌트 인스턴스에 추가한다
// Loki 실제 코드: instance.Push() → 검증 → 스트림 조회/생성 → 엔트리 추가
func (ti *TenantInstance) Push(labels map[string]string, entry LogEntry) error {
	ti.mu.Lock()
	defer ti.mu.Unlock()

	// 1. 레이블 검증
	// Loki 실제 코드: validation.ValidateLabels()
	if err := ti.validateLabels(labels); err != nil {
		ti.rejectedEntries++
		ti.rejectedReason["label_validation"] = ti.rejectedReason["label_validation"] + 1
		return err
	}

	// 2. 속도 제한 확인
	// Loki 실제 코드: Limiter.AllowN()
	if !ti.rateLimiter.Allow(len(entry.Line)) {
		ti.rejectedEntries++
		ti.rejectedReason["rate_limit"] = ti.rejectedReason["rate_limit"] + 1
		return fmt.Errorf("테넌트 '%s' 수집 속도 제한 초과 (%.0f bytes/s)",
			ti.TenantID, ti.limits.IngestionRateLimit)
	}

	// 3. 스트림 조회 또는 생성
	stream := ti.getOrCreateStream(labels)
	if stream == nil {
		ti.rejectedEntries++
		ti.rejectedReason["max_streams"] = ti.rejectedReason["max_streams"] + 1
		return fmt.Errorf("테넌트 '%s' 최대 스트림 수 초과 (%d)",
			ti.TenantID, ti.limits.MaxStreamsPerTenant)
	}

	// 4. 엔트리 추가
	stream.Push(entry)
	atomic.AddInt64(&ti.totalEntries, 1)
	atomic.AddInt64(&ti.totalBytes, int64(len(entry.Line)))

	return nil
}

// validateLabels 는 레이블을 검증한다
func (ti *TenantInstance) validateLabels(labels map[string]string) error {
	if len(labels) > ti.limits.MaxLabelCount {
		return fmt.Errorf("레이블 개수 초과 (%d > %d)", len(labels), ti.limits.MaxLabelCount)
	}
	for k, v := range labels {
		if len(k) > ti.limits.MaxLabelNameLength {
			return fmt.Errorf("레이블 이름 길이 초과: %s", k)
		}
		if len(v) > ti.limits.MaxLabelValueLength {
			return fmt.Errorf("레이블 값 길이 초과: %s=%s", k, v)
		}
	}
	return nil
}

// getOrCreateStream 은 레이블에 해당하는 스트림을 조회하거나 생성한다
// Loki 실제 코드: instance.getOrCreateStream()
func (ti *TenantInstance) getOrCreateStream(labels map[string]string) *LogStream {
	// 임시 스트림으로 레이블 문자열 생성
	tmpStream := &LogStream{Labels: labels}
	key := tmpStream.LabelString()

	if stream, ok := ti.streams[key]; ok {
		return stream
	}

	// 스트림 수 제한 확인
	// Loki 실제 코드: streamCountLimiter.AssertNewStreamAllowed()
	if len(ti.streams) >= ti.limits.MaxStreamsPerTenant {
		return nil
	}

	stream := &LogStream{
		Labels:    labels,
		CreatedAt: time.Now(),
	}
	ti.streams[key] = stream
	return stream
}

// Query 는 레이블 셀렉터에 매칭되는 로그를 반환한다
// 테넌트 범위 쿼리 격리: 해당 테넌트의 데이터만 접근 가능
func (ti *TenantInstance) Query(labelSelector map[string]string, from, through time.Time) []LogEntry {
	ti.mu.RLock()
	defer ti.mu.RUnlock()

	var results []LogEntry
	for _, stream := range ti.streams {
		if matchLabels(stream.Labels, labelSelector) {
			for _, entry := range stream.Entries {
				if !entry.Timestamp.Before(from) && entry.Timestamp.Before(through) {
					results = append(results, entry)
				}
			}
		}
	}

	// 시간순 정렬
	sort.Slice(results, func(i, j int) bool {
		return results[i].Timestamp.Before(results[j].Timestamp)
	})
	return results
}

// matchLabels 는 스트림 레이블이 셀렉터에 매칭되는지 확인한다
func matchLabels(streamLabels, selector map[string]string) bool {
	for k, v := range selector {
		if streamLabels[k] != v {
			return false
		}
	}
	return true
}

// StreamCount 는 테넌트의 스트림 수를 반환한다
func (ti *TenantInstance) StreamCount() int {
	ti.mu.RLock()
	defer ti.mu.RUnlock()
	return len(ti.streams)
}

// =============================================================================
// Ingester: 멀티테넌트 Ingester
// =============================================================================
// Loki 실제 코드: pkg/ingester/ingester.go
//
// Ingester는 테넌트 ID별로 독립적인 instance를 관리한다.
// X-Scope-OrgID 헤더에서 테넌트 ID를 추출하여 해당 instance로 라우팅한다.

type Ingester struct {
	mu        sync.RWMutex
	instances map[string]*TenantInstance // tenantID → instance
	config    *RuntimeConfig
}

func NewIngester(config *RuntimeConfig) *Ingester {
	return &Ingester{
		instances: make(map[string]*TenantInstance),
		config:    config,
	}
}

// GetOrCreateInstance 는 테넌트의 인스턴스를 조회하거나 생성한다
// Loki 실제 코드: Ingester.getOrCreateInstance()
func (ing *Ingester) GetOrCreateInstance(tenantID string) *TenantInstance {
	ing.mu.Lock()
	defer ing.mu.Unlock()

	if inst, ok := ing.instances[tenantID]; ok {
		return inst
	}

	limits := ing.config.GetLimits(tenantID)
	inst := NewTenantInstance(tenantID, limits)
	ing.instances[tenantID] = inst
	return inst
}

// Push 는 X-Scope-OrgID 헤더에서 테넌트 ID를 추출하여 해당 인스턴스에 전달한다
// Loki 실제 코드: Ingester.Push() → tenant.TenantID(ctx) → instance.Push()
func (ing *Ingester) Push(tenantID string, labels map[string]string, entry LogEntry) error {
	instance := ing.GetOrCreateInstance(tenantID)
	return instance.Push(labels, entry)
}

// Query 는 해당 테넌트의 데이터만 검색한다
// 테넌트 격리: 다른 테넌트의 데이터에 접근 불가
func (ing *Ingester) Query(tenantID string, labelSelector map[string]string, from, through time.Time) ([]LogEntry, error) {
	ing.mu.RLock()
	instance, ok := ing.instances[tenantID]
	ing.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("테넌트 '%s'의 인스턴스가 없습니다", tenantID)
	}

	return instance.Query(labelSelector, from, through), nil
}

// TenantStats 는 모든 테넌트의 통계를 반환한다
func (ing *Ingester) TenantStats() map[string]map[string]int64 {
	ing.mu.RLock()
	defer ing.mu.RUnlock()

	stats := make(map[string]map[string]int64)
	for tenantID, inst := range ing.instances {
		stats[tenantID] = map[string]int64{
			"streams":          int64(inst.StreamCount()),
			"total_entries":    atomic.LoadInt64(&inst.totalEntries),
			"total_bytes":      atomic.LoadInt64(&inst.totalBytes),
			"rejected_entries": atomic.LoadInt64(&inst.rejectedEntries),
		}
	}
	return stats
}

// =============================================================================
// HTTP 요청 시뮬레이션
// =============================================================================

// SimulatedRequest 는 HTTP 요청을 시뮬레이션한다
type SimulatedRequest struct {
	Headers map[string]string
	Labels  map[string]string
	Entry   LogEntry
}

// ExtractTenantID 는 X-Scope-OrgID 헤더에서 테넌트 ID를 추출한다
// Loki 실제 코드: dskit/tenant.TenantID(ctx)
func ExtractTenantID(headers map[string]string) (string, error) {
	tenantID, ok := headers["X-Scope-OrgID"]
	if !ok || tenantID == "" {
		return "", fmt.Errorf("X-Scope-OrgID 헤더가 없습니다")
	}
	return tenantID, nil
}

// =============================================================================
// 메인 함수
// =============================================================================

func main() {
	fmt.Println("=== Loki 멀티테넌트 격리 시뮬레이션 ===")
	fmt.Println()

	// ─────────────────────────────────────────────────────────────
	// 1단계: 런타임 설정 및 테넌트별 제한
	// ─────────────────────────────────────────────────────────────
	fmt.Println("--- [1] 테넌트별 리소스 제한 설정 ---")
	fmt.Println()

	defaults := DefaultLimits()
	runtimeConfig := NewRuntimeConfig(defaults)

	// 테넌트별 오버라이드 설정
	// Loki 실제 코드: runtime_config.yaml에서 테넌트별 제한 설정
	runtimeConfig.SetOverride("premium-tenant", &TenantLimits{
		MaxStreamsPerTenant:  500,
		IngestionRateLimit:  100000, // 100KB/s
		IngestionBurstSize:  500000,
		MaxLabelNameLength:  1024,
		MaxLabelValueLength: 2048,
		MaxLabelCount:       50,
		RetentionPeriod:     30 * 24 * time.Hour,
	})

	runtimeConfig.SetOverride("free-tenant", &TenantLimits{
		MaxStreamsPerTenant:  5,        // 매우 제한적
		IngestionRateLimit:  1000,     // 1KB/s
		IngestionBurstSize:  5000,
		MaxLabelNameLength:  256,
		MaxLabelValueLength: 512,
		MaxLabelCount:       10,
		RetentionPeriod:     24 * time.Hour, // 1일
	})

	fmt.Println("  ┌────────────────────┬──────────┬─────────────┬────────────────┐")
	fmt.Println("  │ 테넌트             │ 스트림   │ 속도 (B/s)  │ 보존 기간      │")
	fmt.Println("  ├────────────────────┼──────────┼─────────────┼────────────────┤")

	tenantList := []string{"(기본값)", "premium-tenant", "free-tenant"}
	for _, tid := range tenantList {
		var limits *TenantLimits
		if tid == "(기본값)" {
			limits = defaults
		} else {
			limits = runtimeConfig.GetLimits(tid)
		}
		fmt.Printf("  │ %-18s │ %8d │ %11.0f │ %-14s │\n",
			tid, limits.MaxStreamsPerTenant, limits.IngestionRateLimit, limits.RetentionPeriod)
	}
	fmt.Println("  └────────────────────┴──────────┴─────────────┴────────────────┘")
	fmt.Println()

	// ─────────────────────────────────────────────────────────────
	// 2단계: 멀티테넌트 Ingester 생성
	// ─────────────────────────────────────────────────────────────
	fmt.Println("--- [2] 멀티테넌트 Ingester ---")
	fmt.Println()

	ingester := NewIngester(runtimeConfig)

	// 다양한 테넌트의 로그 수집 시뮬레이션
	baseTime := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	// X-Scope-OrgID 헤더 기반 테넌트 식별
	requests := []SimulatedRequest{
		// premium-tenant의 요청들
		{
			Headers: map[string]string{"X-Scope-OrgID": "premium-tenant"},
			Labels:  map[string]string{"service": "api", "env": "prod", "region": "us-east"},
			Entry:   LogEntry{Timestamp: baseTime, Line: "Request processed successfully"},
		},
		{
			Headers: map[string]string{"X-Scope-OrgID": "premium-tenant"},
			Labels:  map[string]string{"service": "api", "env": "prod", "region": "us-west"},
			Entry:   LogEntry{Timestamp: baseTime.Add(1 * time.Second), Line: "Cache hit for user profile"},
		},
		{
			Headers: map[string]string{"X-Scope-OrgID": "premium-tenant"},
			Labels:  map[string]string{"service": "worker", "env": "prod", "region": "us-east"},
			Entry:   LogEntry{Timestamp: baseTime.Add(2 * time.Second), Line: "Background job completed"},
		},
		// free-tenant의 요청들
		{
			Headers: map[string]string{"X-Scope-OrgID": "free-tenant"},
			Labels:  map[string]string{"app": "demo"},
			Entry:   LogEntry{Timestamp: baseTime, Line: "Demo app started"},
		},
		{
			Headers: map[string]string{"X-Scope-OrgID": "free-tenant"},
			Labels:  map[string]string{"app": "demo", "version": "v1"},
			Entry:   LogEntry{Timestamp: baseTime.Add(1 * time.Second), Line: "User logged in"},
		},
		// standard-tenant (기본 제한 적용)
		{
			Headers: map[string]string{"X-Scope-OrgID": "standard-tenant"},
			Labels:  map[string]string{"service": "auth"},
			Entry:   LogEntry{Timestamp: baseTime, Line: "Authentication successful"},
		},
		// 헤더 없는 요청 (거부됨)
		{
			Headers: map[string]string{},
			Labels:  map[string]string{"app": "unknown"},
			Entry:   LogEntry{Timestamp: baseTime, Line: "No tenant ID"},
		},
	}

	fmt.Println("  요청 처리:")
	for i, req := range requests {
		tenantID, err := ExtractTenantID(req.Headers)
		if err != nil {
			fmt.Printf("    [%d] 거부: %s\n", i+1, err)
			continue
		}

		err = ingester.Push(tenantID, req.Labels, req.Entry)
		if err != nil {
			fmt.Printf("    [%d] 거부: 테넌트=%s, %s\n", i+1, tenantID, err)
		} else {
			fmt.Printf("    [%d] 수집: 테넌트=%s, 레이블=%v\n", i+1, tenantID, req.Labels)
		}
	}
	fmt.Println()

	// ─────────────────────────────────────────────────────────────
	// 3단계: 스트림 수 제한 테스트
	// ─────────────────────────────────────────────────────────────
	fmt.Println("--- [3] 스트림 수 제한 테스트 ---")
	fmt.Println()

	// free-tenant는 최대 5개 스트림만 허용
	fmt.Printf("  free-tenant 최대 스트림 수: %d\n", runtimeConfig.GetLimits("free-tenant").MaxStreamsPerTenant)
	fmt.Println()

	for i := 0; i < 8; i++ {
		labels := map[string]string{
			"app":      "demo",
			"instance": fmt.Sprintf("instance-%d", i),
		}
		entry := LogEntry{
			Timestamp: baseTime.Add(time.Duration(i) * time.Second),
			Line:      fmt.Sprintf("Log from instance %d", i),
		}
		err := ingester.Push("free-tenant", labels, entry)
		if err != nil {
			fmt.Printf("    [스트림 %d] 거부: %s\n", i+1, err)
		} else {
			fmt.Printf("    [스트림 %d] 생성 성공 (총 스트림: %d)\n",
				i+1, ingester.GetOrCreateInstance("free-tenant").StreamCount())
		}
	}
	fmt.Println()

	// ─────────────────────────────────────────────────────────────
	// 4단계: 테넌트 범위 쿼리 격리
	// ─────────────────────────────────────────────────────────────
	fmt.Println("--- [4] 테넌트 범위 쿼리 격리 ---")
	fmt.Println()

	from := baseTime.Add(-1 * time.Hour)
	through := baseTime.Add(1 * time.Hour)

	// premium-tenant 쿼리
	results, err := ingester.Query("premium-tenant", map[string]string{"env": "prod"}, from, through)
	if err != nil {
		fmt.Printf("  쿼리 오류: %s\n", err)
	} else {
		fmt.Printf("  premium-tenant 쿼리 {env=\"prod\"}: %d 결과\n", len(results))
		for _, r := range results {
			fmt.Printf("    [%s] %s\n", r.Timestamp.Format("15:04:05"), r.Line)
		}
	}
	fmt.Println()

	// free-tenant 쿼리 — premium-tenant의 데이터에 접근 불가
	results, err = ingester.Query("free-tenant", map[string]string{"env": "prod"}, from, through)
	if err != nil {
		fmt.Printf("  free-tenant 쿼리 {env=\"prod\"}: 오류 - %s\n", err)
	} else {
		fmt.Printf("  free-tenant 쿼리 {env=\"prod\"}: %d 결과 (premium-tenant 데이터 접근 불가)\n", len(results))
	}
	fmt.Println()

	// free-tenant 자신의 데이터 쿼리
	results, err = ingester.Query("free-tenant", map[string]string{"app": "demo"}, from, through)
	if err != nil {
		fmt.Printf("  free-tenant 쿼리 {app=\"demo\"}: 오류 - %s\n", err)
	} else {
		fmt.Printf("  free-tenant 쿼리 {app=\"demo\"}: %d 결과\n", len(results))
		for _, r := range results {
			fmt.Printf("    [%s] %s\n", r.Timestamp.Format("15:04:05"), r.Line)
		}
	}
	fmt.Println()

	// 존재하지 않는 테넌트 쿼리
	_, err = ingester.Query("nonexistent-tenant", map[string]string{}, from, through)
	if err != nil {
		fmt.Printf("  nonexistent-tenant 쿼리: %s\n", err)
	}
	fmt.Println()

	// ─────────────────────────────────────────────────────────────
	// 5단계: 런타임 설정 오버라이드
	// ─────────────────────────────────────────────────────────────
	fmt.Println("--- [5] 런타임 설정 오버라이드 ---")
	fmt.Println()

	fmt.Println("  [이벤트] free-tenant의 스트림 제한을 5 → 20으로 증가")

	// 런타임 설정 변경 (재시작 불필요)
	newLimits := *runtimeConfig.GetLimits("free-tenant") // 복사
	newLimits.MaxStreamsPerTenant = 20
	runtimeConfig.SetOverride("free-tenant", &newLimits)

	// 새 인스턴스 생성 시 변경된 제한 적용
	// (실제 Loki에서는 기존 인스턴스도 런타임 설정을 참조)
	fmt.Printf("  변경 후 free-tenant 제한: MaxStreams=%d\n",
		runtimeConfig.GetLimits("free-tenant").MaxStreamsPerTenant)
	fmt.Println()

	// ─────────────────────────────────────────────────────────────
	// 6단계: 테넌트별 통계
	// ─────────────────────────────────────────────────────────────
	fmt.Println("--- [6] 테넌트별 통계 ---")
	fmt.Println()

	stats := ingester.TenantStats()
	sortedTenants := make([]string, 0, len(stats))
	for t := range stats {
		sortedTenants = append(sortedTenants, t)
	}
	sort.Strings(sortedTenants)

	fmt.Println("  ┌────────────────────┬─────────┬────────┬────────┬──────────┐")
	fmt.Println("  │ 테넌트             │ 스트림  │ 엔트리 │ 바이트 │ 거부     │")
	fmt.Println("  ├────────────────────┼─────────┼────────┼────────┼──────────┤")
	for _, tid := range sortedTenants {
		s := stats[tid]
		fmt.Printf("  │ %-18s │ %7d │ %6d │ %6d │ %8d │\n",
			tid, s["streams"], s["total_entries"], s["total_bytes"], s["rejected_entries"])
	}
	fmt.Println("  └────────────────────┴─────────┴────────┴────────┴──────────┘")
	fmt.Println()

	// ─────────────────────────────────────────────────────────────
	// 동작 원리 요약
	// ─────────────────────────────────────────────────────────────
	fmt.Println("=== 멀티테넌트 동작 원리 요약 ===")
	fmt.Println()
	fmt.Println("  1. 테넌트 식별: X-Scope-OrgID 헤더에서 테넌트 ID 추출")
	fmt.Println("  2. 인스턴스 격리: 테넌트별 독립적인 스트림 맵, 인덱스, 리소스")
	fmt.Println("  3. 리소스 제한:")
	fmt.Println("     → 스트림 수 제한 (MaxStreamsPerTenant)")
	fmt.Println("     → 수집 속도 제한 (IngestionRateLimit)")
	fmt.Println("     → 레이블 검증 (크기, 개수)")
	fmt.Println("  4. 쿼리 격리: 테넌트는 자신의 데이터만 접근 가능")
	fmt.Println("  5. 런타임 오버라이드: 재시작 없이 테넌트별 제한 변경")
	fmt.Println()
	fmt.Println("  Loki 핵심 코드 경로:")
	fmt.Println("  - pkg/ingester/instance.go    → TenantInstance (스트림 맵, Push, Query)")
	fmt.Println("  - pkg/ingester/limiter.go     → 속도 제한, 스트림 수 제한")
	fmt.Println("  - pkg/validation/limits.go    → TenantLimits 정의")
	fmt.Println("  - pkg/runtime/config.go       → 런타임 설정 오버라이드")
}
