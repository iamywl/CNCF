// Package main은 Argo CD의 Server Extensions와 Rate Limiter 서브시스템을
// Go 표준 라이브러리만으로 시뮬레이션하는 PoC이다.
//
// 시뮬레이션하는 핵심 개념:
// 1. Extension Manager (확장 프록시 관리)
// 2. 확장 등록 및 프록시 라우팅
// 3. RBAC 기반 접근 제어
// 4. 보안 헤더 필터링 (Authorization/Cookie 제거)
// 5. 클러스터 기반 라우팅
// 6. Token Bucket 알고리즘
// 7. 지수 백오프 (Auto Reset)
// 8. 복합 Rate Limiter (Token Bucket + Backoff)
// 9. 작업 큐 통합
// 10. 설정 기반 Rate Limiter 생성
//
// 실제 소스 참조:
//   - server/extension/extension.go   (Extension Manager, 프록시)
//   - util/ratelimiter/ratelimiter.go (Rate Limiter)
package main

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// 1. Server Extensions (server/extension/extension.go 시뮬레이션)
// ============================================================================

// ExtensionConfig는 확장 설정이다.
type ExtensionConfig struct {
	Name      string
	BackendURL string
	Headers   map[string]string // 프록시 시 추가할 헤더
}

// Extension은 등록된 확장이다.
type Extension struct {
	Config  ExtensionConfig
	Enabled bool
}

// RBACPolicy는 간단한 RBAC 정책이다.
type RBACPolicy struct {
	AllowedExtensions map[string][]string // 확장이름 → 허용 프로젝트 목록
}

// ExtensionManager는 확장을 관리하는 중앙 관리자다.
// 실제 구현: server/extension/extension.go의 Manager
type ExtensionManager struct {
	mu         sync.RWMutex
	extensions map[string]*Extension
	rbac       *RBACPolicy
}

// NewExtensionManager는 새 ExtensionManager를 생성한다.
func NewExtensionManager(rbac *RBACPolicy) *ExtensionManager {
	return &ExtensionManager{
		extensions: make(map[string]*Extension),
		rbac:       rbac,
	}
}

// RegisterExtension은 확장을 등록한다.
func (m *ExtensionManager) RegisterExtension(config ExtensionConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.extensions[config.Name] = &Extension{
		Config:  config,
		Enabled: true,
	}
}

// ProxyRequest는 확장으로 요청을 프록시한다.
// 실제 구현에서는 httputil.ReverseProxy를 사용한다.
type ProxyRequest struct {
	ExtensionName string
	Path          string
	Method        string
	Headers       map[string]string
	Project       string // 앱이 속한 프로젝트
	ClusterName   string
}

// ProxyResponse는 프록시 응답이다.
type ProxyResponse struct {
	StatusCode int
	Body       string
	Headers    map[string]string
}

// HandleProxy는 확장 프록시 요청을 처리한다.
func (m *ExtensionManager) HandleProxy(req ProxyRequest) ProxyResponse {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// 1. 확장 존재 확인
	ext, ok := m.extensions[req.ExtensionName]
	if !ok {
		return ProxyResponse{
			StatusCode: 404,
			Body:       fmt.Sprintf("extension %q not found", req.ExtensionName),
		}
	}

	if !ext.Enabled {
		return ProxyResponse{
			StatusCode: 503,
			Body:       fmt.Sprintf("extension %q is disabled", req.ExtensionName),
		}
	}

	// 2. RBAC 검사
	if !m.checkRBAC(req.ExtensionName, req.Project) {
		return ProxyResponse{
			StatusCode: 403,
			Body:       fmt.Sprintf("access denied for project %q on extension %q", req.Project, req.ExtensionName),
		}
	}

	// 3. 보안 헤더 필터링
	// Authorization, Cookie 헤더는 제거 (백엔드로 전달하지 않음)
	safeHeaders := filterSecurityHeaders(req.Headers)

	// 4. 프록시 요청 구성
	targetURL := ext.Config.BackendURL + req.Path
	for k, v := range ext.Config.Headers {
		safeHeaders[k] = v
	}

	// 클러스터 이름을 헤더로 전달
	if req.ClusterName != "" {
		safeHeaders["Argocd-Application-Name"] = req.ClusterName
	}

	return ProxyResponse{
		StatusCode: 200,
		Body: fmt.Sprintf("프록시 응답: %s %s → %s (headers=%v)",
			req.Method, req.Path, targetURL, safeHeaders),
		Headers: map[string]string{
			"X-Argocd-Extension-Name": req.ExtensionName,
		},
	}
}

func (m *ExtensionManager) checkRBAC(extensionName, project string) bool {
	if m.rbac == nil {
		return true
	}
	allowed, ok := m.rbac.AllowedExtensions[extensionName]
	if !ok {
		return true // 정책이 없으면 허용
	}
	for _, p := range allowed {
		if p == "*" || p == project {
			return true
		}
	}
	return false
}

// filterSecurityHeaders는 보안에 민감한 헤더를 제거한다.
func filterSecurityHeaders(headers map[string]string) map[string]string {
	safe := make(map[string]string)
	for k, v := range headers {
		lower := strings.ToLower(k)
		if lower == "authorization" || lower == "cookie" {
			continue // 보안 헤더 제거
		}
		safe[k] = v
	}
	return safe
}

// ============================================================================
// 2. Token Bucket Rate Limiter
//    (util/ratelimiter/ratelimiter.go 시뮬레이션)
// ============================================================================

// TokenBucket은 토큰 버킷 알고리즘을 구현한다.
type TokenBucket struct {
	mu       sync.Mutex
	tokens   float64
	maxTokens float64
	rate      float64 // 초당 토큰 생성 수 (QPS)
	lastTime time.Time
}

// NewTokenBucket은 새 토큰 버킷을 생성한다.
func NewTokenBucket(qps float64, bucketSize int) *TokenBucket {
	return &TokenBucket{
		tokens:    float64(bucketSize),
		maxTokens: float64(bucketSize),
		rate:      qps,
		lastTime:  time.Now(),
	}
}

// Allow는 토큰을 소비하고 허용 여부를 반환한다.
func (tb *TokenBucket) Allow() bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(tb.lastTime).Seconds()
	tb.lastTime = now

	// 경과 시간만큼 토큰 보충
	tb.tokens += elapsed * tb.rate
	if tb.tokens > tb.maxTokens {
		tb.tokens = tb.maxTokens
	}

	if tb.tokens >= 1 {
		tb.tokens--
		return true
	}
	return false
}

// Tokens는 현재 토큰 수를 반환한다.
func (tb *TokenBucket) Tokens() float64 {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	return tb.tokens
}

// ============================================================================
// 3. Exponential Backoff with Auto Reset
// ============================================================================

// ExponentialBackoff는 실패 시 지수적으로 증가하는 대기 시간을 관리한다.
// 성공이 연속되면 자동으로 리셋된다.
type ExponentialBackoff struct {
	mu           sync.Mutex
	baseDelay    time.Duration
	maxDelay     time.Duration
	failures     int
	lastFailure  time.Time
	resetAfter   time.Duration // 이 시간 동안 실패가 없으면 자동 리셋
}

// NewExponentialBackoff는 새 지수 백오프를 생성한다.
func NewExponentialBackoff(baseDelay, maxDelay, resetAfter time.Duration) *ExponentialBackoff {
	return &ExponentialBackoff{
		baseDelay:  baseDelay,
		maxDelay:   maxDelay,
		resetAfter: resetAfter,
	}
}

// RecordFailure는 실패를 기록하고 다음 대기 시간을 반환한다.
func (eb *ExponentialBackoff) RecordFailure() time.Duration {
	eb.mu.Lock()
	defer eb.mu.Unlock()

	now := time.Now()

	// 자동 리셋: 마지막 실패 이후 resetAfter가 지났으면 카운트 리셋
	if !eb.lastFailure.IsZero() && now.Sub(eb.lastFailure) > eb.resetAfter {
		eb.failures = 0
	}

	eb.failures++
	eb.lastFailure = now

	delay := float64(eb.baseDelay) * math.Pow(2, float64(eb.failures-1))
	if delay > float64(eb.maxDelay) {
		delay = float64(eb.maxDelay)
	}
	return time.Duration(delay)
}

// Reset은 백오프를 수동으로 리셋한다.
func (eb *ExponentialBackoff) Reset() {
	eb.mu.Lock()
	defer eb.mu.Unlock()
	eb.failures = 0
}

// Failures는 현재 연속 실패 횟수를 반환한다.
func (eb *ExponentialBackoff) Failures() int {
	eb.mu.Lock()
	defer eb.mu.Unlock()
	return eb.failures
}

// ============================================================================
// 4. 복합 Rate Limiter (Token Bucket + Backoff)
//    실제 구현에서는 AppControllerRateLimiter 함수로 조합한다.
// ============================================================================

// AppControllerRateLimiter는 Application Controller용 복합 Rate Limiter다.
type AppControllerRateLimiter struct {
	bucket  *TokenBucket
	backoff *ExponentialBackoff
}

// NewAppControllerRateLimiter는 복합 Rate Limiter를 생성한다.
func NewAppControllerRateLimiter(bucketQPS float64, bucketSize int,
	baseDelay, maxDelay time.Duration) *AppControllerRateLimiter {
	return &AppControllerRateLimiter{
		bucket:  NewTokenBucket(bucketQPS, bucketSize),
		backoff: NewExponentialBackoff(baseDelay, maxDelay, maxDelay*2),
	}
}

// ShouldProcess는 작업 큐 항목을 처리할 수 있는지 판단한다.
type ProcessDecision struct {
	Allowed   bool
	Delay     time.Duration
	Reason    string
}

// Decide는 처리 여부를 결정한다.
func (r *AppControllerRateLimiter) Decide(key string, failed bool) ProcessDecision {
	if failed {
		delay := r.backoff.RecordFailure()
		return ProcessDecision{
			Allowed: false,
			Delay:   delay,
			Reason:  fmt.Sprintf("백오프 대기: %v (연속 실패 %d회)", delay, r.backoff.Failures()),
		}
	}

	r.backoff.Reset()
	if r.bucket.Allow() {
		return ProcessDecision{
			Allowed: true,
			Reason:  "토큰 소비 성공",
		}
	}

	return ProcessDecision{
		Allowed: false,
		Delay:   time.Second, // 1초 후 재시도
		Reason:  "토큰 버킷 소진, 대기 필요",
	}
}

// ============================================================================
// main
// ============================================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║  Argo CD Extensions & Rate Limiter 시뮬레이션 PoC           ║")
	fmt.Println("║  실제 소스: server/extension/, util/ratelimiter/            ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()

	// === 1. Extension Manager ===
	fmt.Println("=== 1. Extension Manager ===")

	rbac := &RBACPolicy{
		AllowedExtensions: map[string][]string{
			"rollout-dashboard": {"team-a", "team-b"},
			"cost-analyzer":    {"*"}, // 모든 프로젝트 허용
		},
	}

	mgr := NewExtensionManager(rbac)

	// 확장 등록
	mgr.RegisterExtension(ExtensionConfig{
		Name:       "rollout-dashboard",
		BackendURL: "http://argo-rollouts-dashboard:3100",
		Headers:    map[string]string{"X-Extension-Token": "secret123"},
	})
	mgr.RegisterExtension(ExtensionConfig{
		Name:       "cost-analyzer",
		BackendURL: "http://kubecost:9090",
	})

	fmt.Println("  확장 등록 완료: rollout-dashboard, cost-analyzer")
	fmt.Println()

	// 프록시 요청 시뮬레이션
	requests := []ProxyRequest{
		{
			ExtensionName: "rollout-dashboard",
			Path:          "/api/v1/rollouts/canary-deploy",
			Method:        "GET",
			Headers:       map[string]string{"Authorization": "Bearer xxx", "Content-Type": "application/json"},
			Project:       "team-a",
			ClusterName:   "production",
		},
		{
			ExtensionName: "rollout-dashboard",
			Path:          "/api/v1/rollouts/status",
			Method:        "GET",
			Headers:       map[string]string{"Cookie": "session=abc123"},
			Project:       "team-c", // RBAC 거부
		},
		{
			ExtensionName: "nonexistent",
			Path:          "/test",
			Method:        "GET",
			Headers:       map[string]string{},
			Project:       "default",
		},
		{
			ExtensionName: "cost-analyzer",
			Path:          "/api/costs/namespace/production",
			Method:        "GET",
			Headers:       map[string]string{"Accept": "application/json"},
			Project:       "any-project",
		},
	}

	for i, req := range requests {
		resp := mgr.HandleProxy(req)
		fmt.Printf("  요청 %d: %s %s /extensions/%s%s (project=%s)\n",
			i+1, req.Method, req.ExtensionName, req.ExtensionName, req.Path, req.Project)
		fmt.Printf("    응답: %d — %s\n", resp.StatusCode, resp.Body)
		fmt.Println()
	}

	// === 2. 보안 헤더 필터링 ===
	fmt.Println("=== 2. 보안 헤더 필터링 ===")
	original := map[string]string{
		"Authorization": "Bearer token123",
		"Cookie":        "session=abc",
		"Content-Type":  "application/json",
		"X-Custom":      "value",
	}
	filtered := filterSecurityHeaders(original)
	fmt.Printf("  원본 헤더:  %v\n", original)
	fmt.Printf("  필터 후:    %v\n", filtered)
	fmt.Println()

	// === 3. Token Bucket ===
	fmt.Println("=== 3. Token Bucket Rate Limiter ===")
	bucket := NewTokenBucket(10, 5) // 10 QPS, 버킷 크기 5

	fmt.Printf("  설정: QPS=10, BucketSize=5\n")
	fmt.Printf("  초기 토큰: %.0f\n", bucket.Tokens())

	// 빠른 연속 요청
	for i := 1; i <= 8; i++ {
		allowed := bucket.Allow()
		fmt.Printf("  요청 %d: 허용=%v (남은 토큰=%.1f)\n", i, allowed, bucket.Tokens())
	}

	// 100ms 대기 후 토큰 보충 확인
	time.Sleep(100 * time.Millisecond)
	fmt.Printf("  100ms 대기 후 토큰: %.1f\n", bucket.Tokens())
	fmt.Println()

	// === 4. Exponential Backoff ===
	fmt.Println("=== 4. Exponential Backoff with Auto Reset ===")
	backoff := NewExponentialBackoff(
		100*time.Millisecond,  // baseDelay
		10*time.Second,        // maxDelay
		5*time.Second,         // resetAfter
	)

	for i := 1; i <= 6; i++ {
		delay := backoff.RecordFailure()
		fmt.Printf("  실패 %d: 대기 시간=%v\n", i, delay.Round(time.Millisecond))
	}

	backoff.Reset()
	fmt.Printf("  리셋 후 실패 횟수: %d\n", backoff.Failures())
	delay := backoff.RecordFailure()
	fmt.Printf("  리셋 후 첫 실패: 대기 시간=%v (baseDelay로 복귀)\n", delay.Round(time.Millisecond))
	fmt.Println()

	// === 5. 복합 Rate Limiter ===
	fmt.Println("=== 5. Application Controller Rate Limiter ===")
	limiter := NewAppControllerRateLimiter(
		10,                  // bucketQPS
		5,                   // bucketSize
		250*time.Millisecond, // baseDelay
		5*time.Second,        // maxDelay
	)

	// 정상 처리
	for i := 1; i <= 3; i++ {
		decision := limiter.Decide(fmt.Sprintf("app-%d", i), false)
		fmt.Printf("  app-%d (성공): 허용=%v — %s\n", i, decision.Allowed, decision.Reason)
	}

	// 실패 시나리오
	for i := 1; i <= 4; i++ {
		decision := limiter.Decide("app-failing", true)
		fmt.Printf("  app-failing 실패 %d: 허용=%v, 대기=%v — %s\n",
			i, decision.Allowed, decision.Delay.Round(time.Millisecond), decision.Reason)
	}

	// 성공으로 복구
	decision := limiter.Decide("app-failing", false)
	fmt.Printf("  app-failing 복구: 허용=%v — %s\n", decision.Allowed, decision.Reason)
}
