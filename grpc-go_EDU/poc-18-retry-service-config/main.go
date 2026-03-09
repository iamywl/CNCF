// gRPC Retry 및 Service Config 시뮬레이션
//
// 이 PoC는 gRPC의 재시도 메커니즘과 Service Config의 핵심을 시뮬레이션한다:
//   1. Service Config 파싱 — JSON → MethodConfig + RetryPolicy
//   2. shouldRetry 판단 — 9단계 체크 파이프라인
//   3. 지수 백오프 + 지터 — Thundering Herd 방지
//   4. retryThrottler — 토큰 버킷 기반 서버 보호
//   5. 투명 재시도 — 스트림 미생성 시 안전한 재시도
//   6. 서버 푸시백 — 서버 지시 대기 시간
//
// 실제 코드 참조:
//   - service_config.go           — JSON 파싱, ServiceConfig
//   - internal/serviceconfig/     — MethodConfig, RetryPolicy
//   - stream.go:685-779           — shouldRetry() 9단계 판단
//   - clientconn.go:1710-1745     — retryThrottler
//
// 실행: go run main.go

package main

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────
// 1. gRPC 상태 코드 (간략)
// ─────────────────────────────────────────────────

type Code int

const (
	OK                 Code = 0
	Canceled           Code = 1
	Unknown            Code = 2
	InvalidArgument    Code = 3
	DeadlineExceeded   Code = 4
	NotFound           Code = 5
	AlreadyExists      Code = 6
	PermissionDenied   Code = 7
	ResourceExhausted  Code = 8
	Aborted            Code = 10
	Unavailable        Code = 14
	Internal           Code = 13
	DataLoss           Code = 15
	Unauthenticated    Code = 16
)

var codeNames = map[Code]string{
	OK: "OK", Canceled: "CANCELLED", Unknown: "UNKNOWN",
	InvalidArgument: "INVALID_ARGUMENT", DeadlineExceeded: "DEADLINE_EXCEEDED",
	NotFound: "NOT_FOUND", AlreadyExists: "ALREADY_EXISTS",
	PermissionDenied: "PERMISSION_DENIED", ResourceExhausted: "RESOURCE_EXHAUSTED",
	Aborted: "ABORTED", Unavailable: "UNAVAILABLE", Internal: "INTERNAL",
	DataLoss: "DATA_LOSS", Unauthenticated: "UNAUTHENTICATED",
}

func (c Code) String() string {
	if name, ok := codeNames[c]; ok {
		return name
	}
	return fmt.Sprintf("CODE(%d)", c)
}

func codeFromName(name string) Code {
	for code, n := range codeNames {
		if n == name {
			return code
		}
	}
	return Unknown
}

// ─────────────────────────────────────────────────
// 2. Service Config 데이터 구조
// ─────────────────────────────────────────────────

// RetryPolicy는 재시도 정책을 정의한다.
// 실제 코드: internal/serviceconfig/serviceconfig.go:157-180
type RetryPolicy struct {
	MaxAttempts          int                `json:"maxAttempts"`
	InitialBackoff       string             `json:"initialBackoff"`
	MaxBackoff           string             `json:"maxBackoff"`
	BackoffMultiplier    float64            `json:"backoffMultiplier"`
	RetryableStatusCodes []string           `json:"retryableStatusCodes"`
	parsedInitialBackoff time.Duration
	parsedMaxBackoff     time.Duration
	retryableCodes       map[Code]bool
}

// MethodName은 Service Config에서 메서드를 식별한다.
type MethodName struct {
	Service string `json:"service"`
	Method  string `json:"method"`
}

// MethodConfig는 메서드별 RPC 설정이다.
// 실제 코드: internal/serviceconfig/serviceconfig.go:130-152
type MethodConfig struct {
	Name         []MethodName `json:"name"`
	WaitForReady *bool        `json:"waitForReady,omitempty"`
	Timeout      string       `json:"timeout,omitempty"`
	RetryPolicy  *RetryPolicy `json:"retryPolicy,omitempty"`
}

// RetryThrottling은 전역 재시도 스로틀링 정책이다.
// 실제 코드: service_config.go:109-121
type RetryThrottling struct {
	MaxTokens  float64 `json:"maxTokens"`
	TokenRatio float64 `json:"tokenRatio"`
}

// ServiceConfig는 gRPC Service Config의 최상위 구조체이다.
// 실제 코드: service_config.go:158-164
type ServiceConfig struct {
	MethodConfig     []MethodConfig   `json:"methodConfig"`
	RetryThrottling  *RetryThrottling `json:"retryThrottling,omitempty"`
	methods          map[string]*MethodConfig
}

// ─────────────────────────────────────────────────
// 3. Service Config 파싱
// ─────────────────────────────────────────────────

func parseDuration(s string) time.Duration {
	s = strings.TrimSuffix(s, "s")
	val := 0.0
	fmt.Sscanf(s, "%f", &val)
	return time.Duration(val * float64(time.Second))
}

// ParseServiceConfig는 JSON을 파싱하여 ServiceConfig를 생성한다.
// 실제 코드: service_config.go:172-269
func ParseServiceConfig(jsonStr string) (*ServiceConfig, error) {
	var sc ServiceConfig
	if err := json.Unmarshal([]byte(jsonStr), &sc); err != nil {
		return nil, fmt.Errorf("JSON 파싱 오류: %v", err)
	}

	sc.methods = make(map[string]*MethodConfig)

	for i := range sc.MethodConfig {
		mc := &sc.MethodConfig[i]

		// RetryPolicy 검증 및 파싱
		if mc.RetryPolicy != nil {
			rp := mc.RetryPolicy
			if rp.MaxAttempts < 2 {
				return nil, fmt.Errorf("maxAttempts는 2 이상이어야 함 (현재: %d)", rp.MaxAttempts)
			}
			rp.parsedInitialBackoff = parseDuration(rp.InitialBackoff)
			rp.parsedMaxBackoff = parseDuration(rp.MaxBackoff)
			if rp.parsedInitialBackoff <= 0 {
				return nil, fmt.Errorf("initialBackoff는 양수여야 함")
			}
			if rp.BackoffMultiplier <= 0 {
				return nil, fmt.Errorf("backoffMultiplier는 양수여야 함")
			}
			if len(rp.RetryableStatusCodes) == 0 {
				return nil, fmt.Errorf("retryableStatusCodes는 비어있지 않아야 함")
			}
			// map으로 변환 → O(1) 검색
			rp.retryableCodes = make(map[Code]bool)
			for _, name := range rp.RetryableStatusCodes {
				rp.retryableCodes[codeFromName(name)] = true
			}
		}

		// 메서드 매핑
		for _, name := range mc.Name {
			key := "/" + name.Service + "/" + name.Method
			if name.Service == "" && name.Method == "" {
				key = "" // 전역 기본값
			} else if name.Method == "" {
				key = "/" + name.Service + "/" // 서비스 레벨
			}
			sc.methods[key] = mc
		}
	}

	// retryThrottling 검증
	if sc.RetryThrottling != nil {
		if sc.RetryThrottling.MaxTokens <= 0 || sc.RetryThrottling.MaxTokens > 1000 {
			return nil, fmt.Errorf("maxTokens는 (0, 1000] 범위여야 함")
		}
		if sc.RetryThrottling.TokenRatio <= 0 {
			return nil, fmt.Errorf("tokenRatio는 양수여야 함")
		}
	}

	return &sc, nil
}

// getMethodConfig는 메서드에 대한 설정을 반환한다.
// 실제 코드: service_config.go:1095-1107
func (sc *ServiceConfig) getMethodConfig(method string) *MethodConfig {
	// 1. 정확 매칭: /service/method
	if mc, ok := sc.methods[method]; ok {
		return mc
	}
	// 2. 서비스 레벨: /service/
	if i := strings.LastIndex(method, "/"); i >= 0 {
		if mc, ok := sc.methods[method[:i+1]]; ok {
			return mc
		}
	}
	// 3. 전역 기본값: ""
	if mc, ok := sc.methods[""]; ok {
		return mc
	}
	return nil
}

// ─────────────────────────────────────────────────
// 4. Retry Throttler (토큰 버킷)
// ─────────────────────────────────────────────────

// retryThrottler는 토큰 버킷 기반 재시도 스로틀러이다.
// 실제 코드: clientconn.go:1710-1745
type retryThrottler struct {
	max    float64
	thresh float64 // max / 2
	ratio  float64
	mu     sync.Mutex
	tokens float64
}

func newRetryThrottler(maxTokens, tokenRatio float64) *retryThrottler {
	return &retryThrottler{
		max:    maxTokens,
		thresh: maxTokens / 2,
		ratio:  tokenRatio,
		tokens: maxTokens,
	}
}

// throttle은 재시도를 차단해야 하는지 판단한다.
// 실제 코드: clientconn.go:1722-1733
func (rt *retryThrottler) throttle() bool {
	if rt == nil {
		return false
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.tokens--
	if rt.tokens < 0 {
		rt.tokens = 0
	}
	return rt.tokens <= rt.thresh
}

// successfulRPC는 성공한 RPC를 기록한다.
// 실제 코드: clientconn.go:1735-1745
func (rt *retryThrottler) successfulRPC() {
	if rt == nil {
		return
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.tokens += rt.ratio
	if rt.tokens > rt.max {
		rt.tokens = rt.max
	}
}

func (rt *retryThrottler) getTokens() float64 {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.tokens
}

// ─────────────────────────────────────────────────
// 5. shouldRetry — 9단계 재시도 판단
// ─────────────────────────────────────────────────

type retryDecision struct {
	shouldRetry bool
	backoff     time.Duration
	reason      string
}

// shouldRetry는 9단계 체크 파이프라인으로 재시도 여부를 판단한다.
// 실제 코드: stream.go:685-779
func shouldRetry(
	rp *RetryPolicy,
	throttler *retryThrottler,
	code Code,
	numRetries int,
	numRetriesSincePushback int,
	isTransparentRetry bool,
	pushbackMs int,
	hasPushback bool,
) retryDecision {
	// [2단계] 투명 재시도 (스트림 미생성)
	if isTransparentRetry {
		return retryDecision{true, 0, "투명 재시도 (요청 미전송)"}
	}

	// [6단계] 상태 코드 매칭
	if rp == nil || !rp.retryableCodes[code] {
		return retryDecision{false, 0, fmt.Sprintf("%s는 재시도 불가 코드", code)}
	}

	// [7단계] 스로틀 확인
	if throttler != nil && throttler.throttle() {
		return retryDecision{false, 0, fmt.Sprintf("스로틀 차단 (tokens=%.1f ≤ thresh=%.1f)",
			throttler.getTokens(), throttler.thresh)}
	}

	// [8단계] 최대 시도 확인
	if numRetries+1 >= rp.MaxAttempts {
		return retryDecision{false, 0, fmt.Sprintf("최대 시도 도달 (%d/%d)", numRetries+1, rp.MaxAttempts)}
	}

	// [9단계] 백오프 계산
	var dur time.Duration
	if hasPushback {
		dur = time.Millisecond * time.Duration(pushbackMs)
	} else {
		fact := math.Pow(rp.BackoffMultiplier, float64(numRetriesSincePushback))
		cur := math.Min(float64(rp.parsedInitialBackoff)*fact, float64(rp.parsedMaxBackoff))
		// 지터: ±20%
		cur *= 0.8 + 0.4*rand.Float64()
		dur = time.Duration(int64(cur))
	}

	return retryDecision{true, dur, fmt.Sprintf("백오프 %v 후 재시도", dur.Round(time.Millisecond))}
}

// ─────────────────────────────────────────────────
// 6. RPC 시뮬레이션
// ─────────────────────────────────────────────────

type RPCResult struct {
	Code       Code
	PushbackMs int
	HasPushback bool
}

// simulateRPC는 서버 RPC를 시뮬레이션한다.
func simulateRPC(results []RPCResult, attempt int) RPCResult {
	if attempt < len(results) {
		return results[attempt]
	}
	return RPCResult{Code: OK}
}

// executeWithRetry는 재시도 로직을 포함한 RPC 실행을 시뮬레이션한다.
func executeWithRetry(
	method string,
	rp *RetryPolicy,
	throttler *retryThrottler,
	results []RPCResult,
) (Code, int) {
	numRetries := 0
	numRetriesSincePushback := 0

	for attempt := 0; ; attempt++ {
		result := simulateRPC(results, attempt)

		if result.Code == OK {
			if throttler != nil {
				throttler.successfulRPC()
			}
			fmt.Printf("    시도 %d: %s ✓\n", attempt+1, result.Code)
			return OK, attempt + 1
		}

		fmt.Printf("    시도 %d: %s", attempt+1, result.Code)

		decision := shouldRetry(
			rp, throttler, result.Code,
			numRetries, numRetriesSincePushback,
			false, result.PushbackMs, result.HasPushback,
		)

		if !decision.shouldRetry {
			fmt.Printf(" → 재시도 불가: %s\n", decision.reason)
			return result.Code, attempt + 1
		}

		fmt.Printf(" → %s\n", decision.reason)

		if result.HasPushback {
			numRetriesSincePushback = 0
		} else {
			numRetriesSincePushback++
		}
		numRetries++

		// 실제로는 백오프 대기하지만, 시뮬레이션에서는 생략
	}
}

// ─────────────────────────────────────────────────
// 7. 시뮬레이션
// ─────────────────────────────────────────────────

func main() {
	fmt.Println("========================================")
	fmt.Println("gRPC Retry & Service Config 시뮬레이션")
	fmt.Println("========================================")

	// ── 1. Service Config 파싱 ──
	fmt.Println("\n[1] Service Config 파싱")
	fmt.Println("───────────────────────")

	configJSON := `{
		"methodConfig": [
			{
				"name": [{"service": "helloworld.Greeter", "method": "SayHello"}],
				"waitForReady": true,
				"timeout": "10s",
				"retryPolicy": {
					"maxAttempts": 4,
					"initialBackoff": "0.1s",
					"maxBackoff": "1s",
					"backoffMultiplier": 2.0,
					"retryableStatusCodes": ["UNAVAILABLE", "RESOURCE_EXHAUSTED"]
				}
			},
			{
				"name": [{"service": "helloworld.Greeter"}],
				"retryPolicy": {
					"maxAttempts": 3,
					"initialBackoff": "0.05s",
					"maxBackoff": "0.5s",
					"backoffMultiplier": 1.5,
					"retryableStatusCodes": ["UNAVAILABLE"]
				}
			},
			{
				"name": [{}],
				"waitForReady": true
			}
		],
		"retryThrottling": {
			"maxTokens": 10,
			"tokenRatio": 0.1
		}
	}`

	sc, err := ParseServiceConfig(configJSON)
	if err != nil {
		fmt.Printf("  파싱 오류: %v\n", err)
		return
	}
	fmt.Println("  파싱 성공!")

	// 메서드별 Config 매칭
	fmt.Println("\n  메서드별 Config 매칭:")
	testMethods := []string{
		"/helloworld.Greeter/SayHello",
		"/helloworld.Greeter/SayGoodbye",
		"/other.Service/DoSomething",
	}
	for _, method := range testMethods {
		mc := sc.getMethodConfig(method)
		if mc != nil && mc.RetryPolicy != nil {
			fmt.Printf("    %s → maxAttempts=%d, retryableCodes=%v\n",
				method, mc.RetryPolicy.MaxAttempts, mc.RetryPolicy.RetryableStatusCodes)
		} else if mc != nil {
			fmt.Printf("    %s → waitForReady (재시도 없음)\n", method)
		} else {
			fmt.Printf("    %s → 설정 없음\n", method)
		}
	}

	// ── 2. 기본 Retry 시나리오 ──
	fmt.Println("\n[2] 기본 Retry — UNAVAILABLE 후 성공")
	fmt.Println("────────────────────────────────────")

	mc := sc.getMethodConfig("/helloworld.Greeter/SayHello")
	rp := mc.RetryPolicy

	code, attempts := executeWithRetry(
		"/helloworld.Greeter/SayHello",
		rp, nil,
		[]RPCResult{
			{Code: Unavailable},
			{Code: Unavailable},
			{Code: OK},
		},
	)
	fmt.Printf("  결과: %s (총 %d회 시도)\n", code, attempts)

	// ── 3. 최대 시도 초과 ──
	fmt.Println("\n[3] 최대 시도 초과 (maxAttempts=4)")
	fmt.Println("──────────────────────────────────")

	code, attempts = executeWithRetry(
		"/helloworld.Greeter/SayHello",
		rp, nil,
		[]RPCResult{
			{Code: Unavailable},
			{Code: Unavailable},
			{Code: Unavailable},
			{Code: Unavailable}, // 4번째 시도에서 중단
		},
	)
	fmt.Printf("  결과: %s (총 %d회 시도)\n", code, attempts)

	// ── 4. 비재시도 코드 ──
	fmt.Println("\n[4] 비재시도 코드 (INVALID_ARGUMENT)")
	fmt.Println("────────────────────────────────────")

	code, attempts = executeWithRetry(
		"/helloworld.Greeter/SayHello",
		rp, nil,
		[]RPCResult{
			{Code: InvalidArgument}, // 재시도 불가 코드
		},
	)
	fmt.Printf("  결과: %s (총 %d회 시도)\n", code, attempts)

	// ── 5. Retry Throttling ──
	fmt.Println("\n[5] Retry Throttling — 토큰 소진 시 차단")
	fmt.Println("──────────────────────────────────────────")

	throttler := newRetryThrottler(10, 0.1)
	fmt.Printf("  초기 토큰: %.1f (thresh: %.1f)\n", throttler.getTokens(), throttler.thresh)

	// 연속 실패로 토큰 소진
	fmt.Println("  연속 실패 시뮬레이션:")
	for i := 0; i < 8; i++ {
		blocked := throttler.throttle()
		fmt.Printf("    실패 %d: tokens=%.1f, 차단=%v\n", i+1, throttler.getTokens(), blocked)
	}

	fmt.Println("  스로틀 상태에서 재시도 시도:")
	code, attempts = executeWithRetry(
		"/helloworld.Greeter/SayHello",
		rp, throttler,
		[]RPCResult{
			{Code: Unavailable}, // 스로틀에 의해 차단됨
		},
	)
	fmt.Printf("  결과: %s (총 %d회 시도)\n", code, attempts)

	// 성공으로 토큰 복구
	fmt.Println("\n  성공 RPC로 토큰 복구:")
	for i := 0; i < 30; i++ {
		throttler.successfulRPC()
	}
	fmt.Printf("  30회 성공 후 토큰: %.1f\n", throttler.getTokens())

	// ── 6. 서버 푸시백 ──
	fmt.Println("\n[6] 서버 푸시백 — grpc-retry-pushback-ms")
	fmt.Println("──────────────────────────────────────────")

	code, attempts = executeWithRetry(
		"/helloworld.Greeter/SayHello",
		rp, nil,
		[]RPCResult{
			{Code: Unavailable, PushbackMs: 500, HasPushback: true}, // 서버: "500ms 후 재시도"
			{Code: OK},
		},
	)
	fmt.Printf("  결과: %s (총 %d회 시도)\n", code, attempts)

	// ── 7. 지수 백오프 + 지터 시각화 ──
	fmt.Println("\n[7] 지수 백오프 + 지터 분포")
	fmt.Println("─────────────────────────")
	fmt.Println("  설정: initial=0.1s, max=1s, multiplier=2.0, 지터=±20%")
	fmt.Println()

	for retry := 0; retry < 6; retry++ {
		fact := math.Pow(2.0, float64(retry))
		base := math.Min(0.1*fact, 1.0)
		minD := base * 0.8
		maxD := base * 1.2

		// 10개 샘플
		samples := make([]time.Duration, 10)
		for i := range samples {
			cur := base * (0.8 + 0.4*rand.Float64())
			samples[i] = time.Duration(cur * float64(time.Second))
		}

		bar := strings.Repeat("█", int(base*50))
		fmt.Printf("  재시도 %d: base=%.3fs [%.3f~%.3f] %s\n",
			retry+1, base, minD, maxD, bar)
	}

	// ── 8. 투명 재시도 ──
	fmt.Println("\n[8] 투명 재시도 — 스트림 미생성 시")
	fmt.Println("───────────────────────────────────")

	decision := shouldRetry(nil, nil, Unavailable, 0, 0, true, 0, false)
	fmt.Printf("  transportStream=nil, allowTransparentRetry=true\n")
	fmt.Printf("  결과: shouldRetry=%v, reason=%s\n", decision.shouldRetry, decision.reason)
	fmt.Println("  → RetryPolicy 없이도 재시도 가능 (요청이 서버에 도달하지 않았으므로)")

	// ── 9. Service Config 검증 실패 사례 ──
	fmt.Println("\n[9] Service Config 검증 실패 사례")
	fmt.Println("──────────────────────────────────")

	invalidConfigs := []struct {
		name string
		json string
	}{
		{
			"maxAttempts < 2",
			`{"methodConfig":[{"name":[{}],"retryPolicy":{"maxAttempts":1,"initialBackoff":"0.1s","maxBackoff":"1s","backoffMultiplier":2.0,"retryableStatusCodes":["UNAVAILABLE"]}}]}`,
		},
		{
			"retryableStatusCodes 비어있음",
			`{"methodConfig":[{"name":[{}],"retryPolicy":{"maxAttempts":3,"initialBackoff":"0.1s","maxBackoff":"1s","backoffMultiplier":2.0,"retryableStatusCodes":[]}}]}`,
		},
		{
			"maxTokens > 1000",
			`{"methodConfig":[],"retryThrottling":{"maxTokens":1001,"tokenRatio":0.1}}`,
		},
	}

	for _, tc := range invalidConfigs {
		_, err := ParseServiceConfig(tc.json)
		if err != nil {
			fmt.Printf("  ✗ %s: %v\n", tc.name, err)
		} else {
			fmt.Printf("  ✓ %s: 예상과 달리 파싱 성공\n", tc.name)
		}
	}

	fmt.Println("\n========================================")
	fmt.Println("시뮬레이션 완료")
	fmt.Println("========================================")
}
