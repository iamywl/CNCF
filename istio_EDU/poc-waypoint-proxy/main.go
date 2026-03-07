package main

import (
	"fmt"
	"math/rand"
	"strings"
	"time"
)

// =============================================================================
// Istio Waypoint 프록시 L7 라우팅 시뮬레이션
//
// 실제 Istio 소스 참조:
//   - pilot/pkg/networking/core/waypoint.go: findWaypointResources(), ConnectTerminate, ConnectOriginate
//   - pilot/pkg/networking/core/route/retry/retry.go: ConvertPolicy(), DefaultPolicy(), parseRetryOn()
//   - pilot/pkg/networking/core/route/route.go: VirtualService 라우트 빌더
//   - pilot/pkg/xds/endpoints/endpoint_builder.go: findServiceWaypoint(), waypoint endpoint 라우팅
//
// Ambient Mesh에서 Waypoint 프록시의 역할:
// 1. ztunnel은 L4 트래픽을 처리 (HBONE 터널)
// 2. Waypoint는 L7 정책(HTTP 라우팅, 인가, 관측성) 적용
// 3. 요청 흐름: 클라이언트 -> ztunnel -> HBONE -> waypoint -> L7 처리 -> 엔드포인트
//
// Waypoint의 내부 리스너 구조 (실제 Istio):
// - connect_terminate: HBONE CONNECT 터널 종료
// - main_internal:     HTTP 라우팅 및 정책 적용
// - connect_originate: 엔드포인트로의 HBONE 터널 생성
// =============================================================================

// --- 요청 ---
type Request struct {
	ID         string
	SourceIP   string
	DestIP     string // 서비스 VIP
	Host       string
	Path       string
	Method     string
	Headers    map[string]string
	RetryCount int // 현재 재시도 횟수
}

func (r Request) String() string {
	return fmt.Sprintf("[%s] %s %s%s (from=%s, dest=%s)", r.ID, r.Method, r.Host, r.Path, r.SourceIP, r.DestIP)
}

// --- 응답 ---
type Response struct {
	StatusCode int
	Body       string
	Endpoint   string
	Latency    time.Duration
}

// --- 엔드포인트 ---
type Endpoint struct {
	Address    string
	Port       int
	Weight     int
	Healthy    bool
	FailRate   float64 // 실패 확률 (테스트용)
}

func (e Endpoint) String() string {
	return fmt.Sprintf("%s:%d", e.Address, e.Port)
}

// --- L7 라우트 매칭 규칙 ---
// 실제 Istio의 VirtualService HTTPRoute에 대응
type RouteMatch struct {
	PathPrefix string
	PathExact  string
	Headers    map[string]string
	Methods    []string
}

// --- Fault Injection ---
// 실제 Istio의 networking.v1alpha3.HTTPFaultInjection에 대응
type FaultInjection struct {
	Delay      *FaultDelay
	Abort      *FaultAbort
	Percentage float64 // 0.0 ~ 100.0
}

type FaultDelay struct {
	Duration time.Duration
}

type FaultAbort struct {
	StatusCode int
}

// --- Retry Policy ---
// 실제 Istio의 retry.go 참조
// DefaultPolicy(): NumRetries=2, RetryOn="connect-failure,refused-stream,unavailable,cancelled,retriable-status-codes"
type RetryPolicy struct {
	Attempts       int
	PerTryTimeout  time.Duration
	RetryOn        string // 재시도 조건
	RetriableCodes []int  // 재시도할 HTTP 상태 코드
}

// DefaultRetryPolicy는 Istio 기본 재시도 정책 반환
// 실제: retry.go의 cachedDefaultPolicy
func DefaultRetryPolicy() *RetryPolicy {
	return &RetryPolicy{
		Attempts:       2,
		PerTryTimeout:  0, // 0이면 전체 타임아웃 사용
		RetryOn:        "connect-failure,refused-stream,unavailable,cancelled,retriable-status-codes",
		RetriableCodes: []int{503},
	}
}

// --- L7 라우트 ---
type Route struct {
	Name       string
	Match      RouteMatch
	Endpoints  []Endpoint
	Fault      *FaultInjection
	Retry      *RetryPolicy
	Timeout    time.Duration
}

// =============================================================================
// ztunnel 시뮬레이션
// =============================================================================

// Ztunnel은 L4 트래픽을 처리하는 노드 프록시
// 서비스에 waypoint가 있으면 트래픽을 waypoint로 전달
type Ztunnel struct {
	NodeName  string
	waypoints map[string]*WaypointProxy // 서비스 VIP -> waypoint
}

func NewZtunnel(nodeName string) *Ztunnel {
	return &Ztunnel{
		NodeName:  nodeName,
		waypoints: make(map[string]*WaypointProxy),
	}
}

func (z *Ztunnel) RegisterWaypoint(serviceVIP string, wp *WaypointProxy) {
	z.waypoints[serviceVIP] = wp
}

// HandleRequest는 요청을 처리
// 서비스에 waypoint가 있으면 HBONE 터널을 통해 waypoint로 전달
func (z *Ztunnel) HandleRequest(req Request) (*Response, []string) {
	var log []string
	log = append(log, fmt.Sprintf("[ztunnel@%s] 요청 수신: %s", z.NodeName, req))

	// 서비스 VIP에 대한 waypoint 확인
	wp, hasWaypoint := z.waypoints[req.DestIP]
	if hasWaypoint {
		log = append(log, fmt.Sprintf("[ztunnel@%s] 서비스 %s에 waypoint 발견 -> HBONE 터널로 waypoint 전달", z.NodeName, req.DestIP))
		log = append(log, fmt.Sprintf("[ztunnel@%s] HBONE CONNECT: %s -> waypoint(%s)", z.NodeName, req.SourceIP, wp.Address))

		// waypoint로 전달 (L7 처리)
		resp, wpLog := wp.HandleRequest(req)
		log = append(log, wpLog...)
		return resp, log
	}

	// waypoint 없으면 직접 L4 전달 (시뮬레이션에서는 미구현)
	log = append(log, fmt.Sprintf("[ztunnel@%s] waypoint 없음 -> L4 직접 전달", z.NodeName))
	return &Response{StatusCode: 200, Body: "L4 direct"}, log
}

// =============================================================================
// Waypoint 프록시 시뮬레이션
// =============================================================================

// WaypointProxy는 Ambient Mesh의 L7 프록시
// 실제 Istio에서는 Envoy 기반이며, connect_terminate/main_internal/connect_originate
// 3개 내부 리스너로 구성
type WaypointProxy struct {
	Name      string
	Address   string
	Routes    []Route
	rng       *rand.Rand
}

func NewWaypointProxy(name, address string) *WaypointProxy {
	return &WaypointProxy{
		Name:    name,
		Address: address,
		Routes:  make([]Route, 0),
		rng:     rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (wp *WaypointProxy) AddRoute(route Route) {
	wp.Routes = append(wp.Routes, route)
}

// HandleRequest는 L7 요청 처리의 전체 흐름을 시뮬레이션
// 실제 Waypoint의 리스너 체인:
// 1. connect_terminate: HBONE 터널 종료
// 2. main_internal: HTTP 라우팅, 인가, Fault Injection, Retry
// 3. connect_originate: 선택된 엔드포인트로 HBONE 터널 생성
func (wp *WaypointProxy) HandleRequest(req Request) (*Response, []string) {
	var log []string

	// --- Phase 1: HBONE CONNECT 종료 (connect_terminate) ---
	log = append(log, fmt.Sprintf("  [waypoint/%s] Phase 1: connect_terminate - HBONE 터널 종료", wp.Name))

	// --- Phase 2: L7 라우팅 (main_internal) ---
	log = append(log, fmt.Sprintf("  [waypoint/%s] Phase 2: main_internal - L7 HTTP 라우팅 시작", wp.Name))

	// 라우트 매칭
	route := wp.matchRoute(req)
	if route == nil {
		log = append(log, fmt.Sprintf("  [waypoint/%s] 매칭되는 라우트 없음 -> 404", wp.Name))
		return &Response{StatusCode: 404, Body: "no route matched"}, log
	}
	log = append(log, fmt.Sprintf("  [waypoint/%s] 라우트 매칭: '%s'", wp.Name, route.Name))

	// Fault Injection 처리
	if route.Fault != nil {
		faultResp, faultLog, applied := wp.applyFaultInjection(req, route.Fault)
		log = append(log, faultLog...)
		if applied && faultResp != nil {
			return faultResp, log
		}
	}

	// Retry 정책 결정
	retryPolicy := route.Retry
	if retryPolicy == nil {
		retryPolicy = DefaultRetryPolicy()
	}

	// 엔드포인트 선택 및 요청 전달 (with retry)
	resp, retryLog := wp.sendWithRetry(req, route, retryPolicy)
	log = append(log, retryLog...)

	return resp, log
}

// matchRoute는 요청에 매칭되는 라우트를 찾음
// 실제 Istio의 route.go에서 VirtualService의 HTTPRoute 매칭
func (wp *WaypointProxy) matchRoute(req Request) *Route {
	for i := range wp.Routes {
		route := &wp.Routes[i]
		match := route.Match

		// Path 매칭 (prefix 또는 exact)
		if match.PathPrefix != "" {
			if !strings.HasPrefix(req.Path, match.PathPrefix) {
				continue
			}
		}
		if match.PathExact != "" {
			if req.Path != match.PathExact {
				continue
			}
		}

		// Method 매칭
		if len(match.Methods) > 0 {
			methodMatched := false
			for _, m := range match.Methods {
				if m == req.Method {
					methodMatched = true
					break
				}
			}
			if !methodMatched {
				continue
			}
		}

		// Header 매칭
		if len(match.Headers) > 0 {
			headerMatched := true
			for k, v := range match.Headers {
				if req.Headers[k] != v {
					headerMatched = false
					break
				}
			}
			if !headerMatched {
				continue
			}
		}

		return route
	}
	return nil
}

// applyFaultInjection은 Fault Injection 적용
// 실제 Istio의 networking.v1alpha3.HTTPFaultInjection
func (wp *WaypointProxy) applyFaultInjection(req Request, fault *FaultInjection) (*Response, []string, bool) {
	var log []string

	// 확률 체크
	if wp.rng.Float64()*100 >= fault.Percentage {
		log = append(log, fmt.Sprintf("  [waypoint/%s] Fault Injection: 확률 미적용 (%.0f%%)", wp.Name, fault.Percentage))
		return nil, log, false
	}

	// Delay 적용
	if fault.Delay != nil {
		log = append(log, fmt.Sprintf("  [waypoint/%s] Fault Injection: 지연 %v 적용 (%.0f%%)",
			wp.Name, fault.Delay.Duration, fault.Percentage))
		// 실제로는 time.Sleep을 하지만 시뮬레이션에서는 로그만 남김
	}

	// Abort 적용
	if fault.Abort != nil {
		log = append(log, fmt.Sprintf("  [waypoint/%s] Fault Injection: 중단 %d 적용 (%.0f%%)",
			wp.Name, fault.Abort.StatusCode, fault.Percentage))
		return &Response{
			StatusCode: fault.Abort.StatusCode,
			Body:       "fault injection abort",
		}, log, true
	}

	return nil, log, false
}

// sendWithRetry는 재시도 로직을 포함한 요청 전달
// 실제: retry.go의 DefaultPolicy() + ConvertPolicy()
// NumRetries=2, RetryOn="connect-failure,refused-stream,unavailable,cancelled,retriable-status-codes"
func (wp *WaypointProxy) sendWithRetry(req Request, route *Route, policy *RetryPolicy) (*Response, []string) {
	var log []string
	maxAttempts := policy.Attempts + 1 // 첫 시도 + 재시도 횟수
	var lastResp *Response
	usedEndpoints := make(map[string]bool) // RetryHostPredicate: 이전 호스트 재사용 방지

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// 엔드포인트 선택 (이전에 사용한 호스트 피하기)
		// 실제: RetryHostPredicate = [envoy.retry_host_predicates.previous_hosts]
		ep := wp.selectEndpoint(route.Endpoints, usedEndpoints)
		if ep == nil {
			log = append(log, fmt.Sprintf("  [waypoint/%s] 가용 엔드포인트 없음", wp.Name))
			return &Response{StatusCode: 503, Body: "no healthy upstream"}, log
		}
		usedEndpoints[ep.String()] = true

		if attempt == 1 {
			log = append(log, fmt.Sprintf("  [waypoint/%s] Phase 3: connect_originate - 엔드포인트 %s로 HBONE 터널 생성", wp.Name, ep))
		} else {
			log = append(log, fmt.Sprintf("  [waypoint/%s] 재시도 #%d: 엔드포인트 %s (이전 호스트 회피)", wp.Name, attempt-1, ep))
		}

		// 엔드포인트에 요청 전달 시뮬레이션
		resp := wp.simulateUpstreamCall(ep)
		lastResp = resp

		log = append(log, fmt.Sprintf("  [waypoint/%s]   -> 응답: %d (endpoint=%s)", wp.Name, resp.StatusCode, ep))

		// 재시도 필요 여부 판단
		if !wp.shouldRetry(resp, policy) {
			return resp, log
		}

		if attempt < maxAttempts {
			log = append(log, fmt.Sprintf("  [waypoint/%s]   -> 재시도 조건 충족 (retryOn=%s)", wp.Name, policy.RetryOn))
		}
	}

	log = append(log, fmt.Sprintf("  [waypoint/%s] 최대 재시도 횟수(%d) 초과", wp.Name, policy.Attempts))
	return lastResp, log
}

// selectEndpoint는 가중치 기반 엔드포인트 선택 (healthy만, 이전 호스트 회피)
func (wp *WaypointProxy) selectEndpoint(endpoints []Endpoint, used map[string]bool) *Endpoint {
	// 1차: healthy이고 이전에 사용하지 않은 엔드포인트
	var candidates []Endpoint
	var totalWeight int
	for _, ep := range endpoints {
		if ep.Healthy && !used[ep.String()] {
			candidates = append(candidates, ep)
			totalWeight += ep.Weight
		}
	}

	// 이전 호스트 회피 불가 시 HostSelectionRetryMaxAttempts(5) 후 아무거나 선택
	if len(candidates) == 0 {
		for _, ep := range endpoints {
			if ep.Healthy {
				candidates = append(candidates, ep)
				totalWeight += ep.Weight
			}
		}
	}

	if len(candidates) == 0 {
		return nil
	}

	// 가중치 기반 랜덤 선택
	r := wp.rng.Intn(totalWeight)
	cumulative := 0
	for i := range candidates {
		cumulative += candidates[i].Weight
		if r < cumulative {
			return &candidates[i]
		}
	}
	return &candidates[len(candidates)-1]
}

// simulateUpstreamCall은 엔드포인트 호출 시뮬레이션
func (wp *WaypointProxy) simulateUpstreamCall(ep *Endpoint) *Response {
	// 실패 확률에 따른 503 반환
	if wp.rng.Float64() < ep.FailRate {
		return &Response{
			StatusCode: 503,
			Body:       "upstream unavailable",
			Endpoint:   ep.String(),
			Latency:    time.Millisecond * time.Duration(50+wp.rng.Intn(200)),
		}
	}
	return &Response{
		StatusCode: 200,
		Body:       "OK",
		Endpoint:   ep.String(),
		Latency:    time.Millisecond * time.Duration(5+wp.rng.Intn(50)),
	}
}

// shouldRetry는 재시도 여부를 판단
// 실제: retry.go의 RetryOn 파싱 및 Envoy의 retry 조건 체크
func (wp *WaypointProxy) shouldRetry(resp *Response, policy *RetryPolicy) bool {
	retryConditions := strings.Split(policy.RetryOn, ",")
	for _, cond := range retryConditions {
		cond = strings.TrimSpace(cond)
		switch cond {
		case "retriable-status-codes":
			for _, code := range policy.RetriableCodes {
				if resp.StatusCode == code {
					return true
				}
			}
		case "connect-failure":
			if resp.StatusCode == 503 {
				return true
			}
		case "unavailable":
			if resp.StatusCode == 503 {
				return true
			}
		case "refused-stream":
			if resp.StatusCode == 503 {
				return true
			}
		case "5xx":
			if resp.StatusCode >= 500 && resp.StatusCode < 600 {
				return true
			}
		}
	}
	return false
}

// =============================================================================
// 시뮬레이션 실행
// =============================================================================

func main() {
	fmt.Println("=" + strings.Repeat("=", 79))
	fmt.Println("Istio Waypoint 프록시 L7 라우팅 시뮬레이션")
	fmt.Println("=" + strings.Repeat("=", 79))

	// -----------------------------------------------------------------------
	// 환경 구성
	// -----------------------------------------------------------------------
	fmt.Println("\n[1] Ambient Mesh 환경 구성")
	fmt.Println(strings.Repeat("-", 60))

	// Waypoint 프록시 생성
	waypoint := NewWaypointProxy("reviews-waypoint", "10.96.0.100")

	// 엔드포인트 정의
	healthyEndpoints := []Endpoint{
		{Address: "10.1.1.1", Port: 9080, Weight: 3, Healthy: true, FailRate: 0.0},
		{Address: "10.1.1.2", Port: 9080, Weight: 2, Healthy: true, FailRate: 0.0},
		{Address: "10.1.1.3", Port: 9080, Weight: 1, Healthy: true, FailRate: 0.0},
	}

	// 일부 실패 가능한 엔드포인트
	unstableEndpoints := []Endpoint{
		{Address: "10.1.2.1", Port: 9080, Weight: 1, Healthy: true, FailRate: 0.7}, // 70% 실패율
		{Address: "10.1.2.2", Port: 9080, Weight: 1, Healthy: true, FailRate: 0.7},
		{Address: "10.1.2.3", Port: 9080, Weight: 1, Healthy: true, FailRate: 0.0}, // 안정
	}

	// 라우트 설정 (VirtualService에 해당)
	waypoint.AddRoute(Route{
		Name: "reviews-v2-canary",
		Match: RouteMatch{
			PathPrefix: "/api/reviews",
			Headers:    map[string]string{"x-canary": "true"},
		},
		Endpoints: []Endpoint{
			{Address: "10.1.3.1", Port: 9080, Weight: 1, Healthy: true, FailRate: 0.0},
		},
		Retry: &RetryPolicy{
			Attempts:       3,
			RetryOn:        "5xx",
			RetriableCodes: []int{503},
		},
	})

	waypoint.AddRoute(Route{
		Name: "reviews-with-fault",
		Match: RouteMatch{
			PathPrefix: "/api/fault",
		},
		Endpoints: healthyEndpoints,
		Fault: &FaultInjection{
			Abort:      &FaultAbort{StatusCode: 503},
			Percentage: 100.0,
		},
	})

	waypoint.AddRoute(Route{
		Name: "reviews-with-delay",
		Match: RouteMatch{
			PathPrefix: "/api/delay",
		},
		Endpoints: healthyEndpoints,
		Fault: &FaultInjection{
			Delay:      &FaultDelay{Duration: 5 * time.Second},
			Percentage: 50.0,
		},
	})

	waypoint.AddRoute(Route{
		Name: "reviews-unstable",
		Match: RouteMatch{
			PathPrefix: "/api/unstable",
		},
		Endpoints: unstableEndpoints,
		Retry: &RetryPolicy{
			Attempts:       3,
			RetryOn:        "connect-failure,refused-stream,unavailable,retriable-status-codes",
			RetriableCodes: []int{503},
		},
	})

	waypoint.AddRoute(Route{
		Name: "reviews-default",
		Match: RouteMatch{
			PathPrefix: "/",
		},
		Endpoints: healthyEndpoints,
	})

	// ztunnel 생성 및 waypoint 등록
	ztunnel := NewZtunnel("node-1")
	ztunnel.RegisterWaypoint("10.96.1.100", waypoint)

	fmt.Println("  Waypoint 프록시 구성:")
	fmt.Printf("    이름: %s\n", waypoint.Name)
	fmt.Printf("    주소: %s\n", waypoint.Address)
	fmt.Printf("    등록된 라우트:\n")
	for _, r := range waypoint.Routes {
		retryInfo := "기본(2회)"
		if r.Retry != nil {
			retryInfo = fmt.Sprintf("커스텀(%d회, on=%s)", r.Retry.Attempts, r.Retry.RetryOn)
		}
		faultInfo := "없음"
		if r.Fault != nil {
			if r.Fault.Abort != nil {
				faultInfo = fmt.Sprintf("abort(%d, %.0f%%)", r.Fault.Abort.StatusCode, r.Fault.Percentage)
			}
			if r.Fault.Delay != nil {
				faultInfo = fmt.Sprintf("delay(%v, %.0f%%)", r.Fault.Delay.Duration, r.Fault.Percentage)
			}
		}
		match := ""
		if r.Match.PathPrefix != "" {
			match += "prefix=" + r.Match.PathPrefix
		}
		if len(r.Match.Headers) > 0 {
			match += fmt.Sprintf(" headers=%v", r.Match.Headers)
		}
		fmt.Printf("      [%s] match={%s} endpoints=%d retry=%s fault=%s\n",
			r.Name, match, len(r.Endpoints), retryInfo, faultInfo)
	}

	fmt.Println("\n  내부 리스너 체인 (실제 Envoy 구조):")
	fmt.Println("    ┌─────────────────┐    ┌────────────────┐    ┌──────────────────┐")
	fmt.Println("    │connect_terminate│ -> │ main_internal  │ -> │connect_originate │")
	fmt.Println("    │ (HBONE 터널 종료) │    │ (L7 HTTP 라우팅) │    │ (HBONE 터널 생성)  │")
	fmt.Println("    └─────────────────┘    └────────────────┘    └──────────────────┘")

	// -----------------------------------------------------------------------
	// 테스트 시나리오
	// -----------------------------------------------------------------------

	// 시나리오 1: 기본 요청 흐름
	fmt.Println("\n" + strings.Repeat("=", 80))
	fmt.Println("[2] 시나리오 1: 기본 요청 흐름 (ztunnel -> waypoint -> endpoint)")
	fmt.Println(strings.Repeat("-", 60))

	req1 := Request{
		ID: "req-001", SourceIP: "10.1.0.5", DestIP: "10.96.1.100",
		Host: "reviews.default.svc.cluster.local", Path: "/api/reviews/1",
		Method: "GET", Headers: map[string]string{},
	}
	resp1, log1 := ztunnel.HandleRequest(req1)
	for _, l := range log1 {
		fmt.Println("  " + l)
	}
	fmt.Printf("  최종 결과: HTTP %d\n", resp1.StatusCode)

	// 시나리오 2: 헤더 기반 카나리 라우팅
	fmt.Println("\n" + strings.Repeat("=", 80))
	fmt.Println("[3] 시나리오 2: 헤더 기반 카나리 라우팅 (x-canary: true)")
	fmt.Println(strings.Repeat("-", 60))

	req2 := Request{
		ID: "req-002", SourceIP: "10.1.0.5", DestIP: "10.96.1.100",
		Host: "reviews.default.svc.cluster.local", Path: "/api/reviews/2",
		Method: "GET", Headers: map[string]string{"x-canary": "true"},
	}
	resp2, log2 := ztunnel.HandleRequest(req2)
	for _, l := range log2 {
		fmt.Println("  " + l)
	}
	fmt.Printf("  최종 결과: HTTP %d (카나리 엔드포인트로 라우팅됨)\n", resp2.StatusCode)

	// 시나리오 3: Fault Injection - Abort
	fmt.Println("\n" + strings.Repeat("=", 80))
	fmt.Println("[4] 시나리오 3: Fault Injection - Abort (503, 100%)")
	fmt.Println(strings.Repeat("-", 60))

	req3 := Request{
		ID: "req-003", SourceIP: "10.1.0.5", DestIP: "10.96.1.100",
		Host: "reviews.default.svc.cluster.local", Path: "/api/fault/test",
		Method: "GET", Headers: map[string]string{},
	}
	resp3, log3 := ztunnel.HandleRequest(req3)
	for _, l := range log3 {
		fmt.Println("  " + l)
	}
	fmt.Printf("  최종 결과: HTTP %d (fault injection abort)\n", resp3.StatusCode)

	// 시나리오 4: Fault Injection - Delay (50% 확률)
	fmt.Println("\n" + strings.Repeat("=", 80))
	fmt.Println("[5] 시나리오 4: Fault Injection - Delay (5초, 50% 확률)")
	fmt.Println(strings.Repeat("-", 60))

	for i := 0; i < 3; i++ {
		req4 := Request{
			ID: fmt.Sprintf("req-delay-%d", i+1), SourceIP: "10.1.0.5", DestIP: "10.96.1.100",
			Host: "reviews.default.svc.cluster.local", Path: "/api/delay/test",
			Method: "GET", Headers: map[string]string{},
		}
		resp4, log4 := ztunnel.HandleRequest(req4)
		for _, l := range log4 {
			fmt.Println("  " + l)
		}
		fmt.Printf("  최종 결과: HTTP %d\n\n", resp4.StatusCode)
	}

	// 시나리오 5: Retry 동작 (불안정한 엔드포인트)
	fmt.Println(strings.Repeat("=", 80))
	fmt.Println("[6] 시나리오 5: Retry 동작 (불안정한 엔드포인트, 3회 재시도)")
	fmt.Println(strings.Repeat("-", 60))

	successCount := 0
	retryTotal := 10
	for i := 0; i < retryTotal; i++ {
		req5 := Request{
			ID: fmt.Sprintf("req-retry-%d", i+1), SourceIP: "10.1.0.5", DestIP: "10.96.1.100",
			Host: "reviews.default.svc.cluster.local", Path: "/api/unstable/data",
			Method: "GET", Headers: map[string]string{},
		}
		resp5, log5 := ztunnel.HandleRequest(req5)
		for _, l := range log5 {
			fmt.Println("  " + l)
		}
		fmt.Printf("  최종 결과: HTTP %d\n\n", resp5.StatusCode)
		if resp5.StatusCode == 200 {
			successCount++
		}
	}
	fmt.Printf("  Retry 통계: %d/%d 성공 (%.0f%%)\n", successCount, retryTotal, float64(successCount)/float64(retryTotal)*100)
	fmt.Println("  => 재시도 덕분에 70% 실패율의 엔드포인트도 높은 성공률 달성")

	// -----------------------------------------------------------------------
	// 요청 흐름 다이어그램
	// -----------------------------------------------------------------------
	fmt.Println("\n" + strings.Repeat("=", 80))
	fmt.Println("[7] Ambient Mesh 요청 흐름 다이어그램")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println(`
  클라이언트 Pod        ztunnel (L4)           Waypoint (L7)          서버 Pod
  ┌──────────┐    ┌────────────────┐    ┌──────────────────┐    ┌──────────┐
  │ app      │    │ HBONE 터널 생성  │    │ connect_terminate│    │ app      │
  │ 컨테이너  │ -> │ 서비스 VIP 확인  │ -> │ main_internal    │ -> │ 컨테이너  │
  │          │    │ waypoint 확인   │    │  - 라우트 매칭    │    │          │
  └──────────┘    │ CONNECT 터널    │    │  - AuthZ 정책    │    └──────────┘
                  └────────────────┘    │  - Fault Inject  │
                                       │  - Retry/Timeout │
                                       │ connect_originate│
                                       └──────────────────┘

  ztunnel 결정 로직:
  ┌─────────────────────────────────┐
  │ 서비스에 waypoint이 있는가?      │
  │   YES -> waypoint으로 HBONE 전달 │
  │   NO  -> 직접 L4 포워딩          │
  └─────────────────────────────────┘

  Waypoint 내부 처리:
  ┌─────────────────────────────────────────────────┐
  │ 1. HBONE CONNECT 종료 (connect_terminate)        │
  │ 2. HTTP 라우트 매칭 (VirtualService 규칙)         │
  │ 3. Fault Injection 적용 (있으면)                  │
  │ 4. Authorization Policy 평가                     │
  │ 5. 엔드포인트 선택 (가중치 기반 LB)               │
  │ 6. 요청 전달 (retry 포함)                         │
  │    - 실패 시 다른 호스트로 재시도                   │
  │    - RetryHostPredicate로 이전 호스트 회피         │
  │ 7. HBONE 터널 생성 (connect_originate)            │
  └─────────────────────────────────────────────────┘
`)

	fmt.Println(strings.Repeat("=", 80))
	fmt.Println("시뮬레이션 완료!")
	fmt.Println(strings.Repeat("=", 80))
}
