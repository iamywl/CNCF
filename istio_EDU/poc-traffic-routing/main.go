// poc-traffic-routing: VirtualService/DestinationRule 라우팅 시뮬레이션
//
// Istio의 VirtualService와 DestinationRule이 트래픽 라우팅을 결정하는 과정을 시뮬레이션한다.
// - VirtualService: host 매칭, URI prefix 매칭, header 기반 라우팅, 가중치 기반 라우팅
// - DestinationRule: subset(label selector), 로드밸런싱 정책, fault injection
// - Route Resolution: 요청 → 라우트 매칭 → subset 선택 → 엔드포인트 선택
//
// 참조: pilot/pkg/model/virtualservice.go, pilot/pkg/networking/core/route/route.go

package main

import (
	"fmt"
	"math/rand"
	"net/http"
	"strings"
	"time"
)

// ============================================================================
// 1. 데이터 모델: Istio의 VirtualService / DestinationRule 모델링
//    참조: networking/v1/virtual_service.pb.go
//    참조: pilot/pkg/model/virtualservice.go
// ============================================================================

// StringMatch는 Istio의 문자열 매칭 조건을 나타낸다.
// Istio에서는 exact, prefix, regex 3가지 매칭을 지원한다.
// 참조: networking/v1alpha3 의 StringMatch protobuf
type StringMatch struct {
	Exact  string // 정확히 일치
	Prefix string // 접두사 일치
	Regex  string // 정규식 일치 (이 PoC에서는 간단한 패턴만 지원)
}

// Match는 주어진 값이 이 StringMatch 조건에 부합하는지 검사한다.
func (sm StringMatch) Match(value string) bool {
	if sm.Exact != "" {
		return value == sm.Exact
	}
	if sm.Prefix != "" {
		return strings.HasPrefix(value, sm.Prefix)
	}
	if sm.Regex != "" {
		// 간단한 와일드카드 패턴만 지원 (* = 모든 문자열)
		if sm.Regex == "*" {
			return true
		}
		return value == sm.Regex
	}
	return true // 조건 없으면 항상 매칭
}

// IsEmpty는 매칭 조건이 비어 있는지 검사한다.
func (sm StringMatch) IsEmpty() bool {
	return sm.Exact == "" && sm.Prefix == "" && sm.Regex == ""
}

// String은 StringMatch를 문자열로 표현한다.
func (sm StringMatch) String() string {
	if sm.Exact != "" {
		return fmt.Sprintf("exact:%s", sm.Exact)
	}
	if sm.Prefix != "" {
		return fmt.Sprintf("prefix:%s", sm.Prefix)
	}
	if sm.Regex != "" {
		return fmt.Sprintf("regex:%s", sm.Regex)
	}
	return "<any>"
}

// HTTPMatchRequest는 HTTP 요청 매칭 조건이다.
// 참조: pilot/pkg/model/virtualservice.go 의 HTTPMatchRequest 처리 로직
// 참조: networking/v1alpha3 의 HTTPMatchRequest protobuf
type HTTPMatchRequest struct {
	Name    string                 // 매칭 규칙 이름
	URI     StringMatch            // URI 매칭 (예: prefix:/api/v1)
	Method  StringMatch            // HTTP 메서드 매칭 (예: exact:GET)
	Headers map[string]StringMatch // 헤더 매칭 (예: x-version: exact:v2)
	// Istio는 sourceLabels, sourceNamespace, port, gateways도 지원하지만 여기서는 생략
}

// Matches는 HTTP 요청이 이 매칭 조건에 부합하는지 검사한다.
// 모든 조건이 AND로 결합된다.
// 참조: pilot/pkg/networking/core/route/route.go 의 매칭 로직
func (m HTTPMatchRequest) Matches(req *HTTPRequest) bool {
	// URI 매칭
	if !m.URI.IsEmpty() && !m.URI.Match(req.URI) {
		return false
	}
	// 메서드 매칭
	if !m.Method.IsEmpty() && !m.Method.Match(req.Method) {
		return false
	}
	// 헤더 매칭 (모든 헤더 조건이 충족되어야 함)
	for key, matcher := range m.Headers {
		headerVal := req.Headers[key]
		if !matcher.Match(headerVal) {
			return false
		}
	}
	return true
}

// HTTPRouteDestination은 라우팅 목적지와 가중치를 정의한다.
// 참조: networking/v1alpha3 의 HTTPRouteDestination
type HTTPRouteDestination struct {
	Host   string // 대상 서비스 호스트명
	Subset string // DestinationRule에서 정의한 subset 이름
	Port   int    // 대상 포트
	Weight int    // 가중치 (0-100, 전체 합이 100이어야 함)
}

// FaultInjection은 장애 주입 설정이다.
// 참조: networking/v1alpha3 의 HTTPFaultInjection
type FaultInjection struct {
	DelayPercent float64       // 지연 주입 비율 (0-100)
	DelayDuration time.Duration // 지연 시간
	AbortPercent float64       // 중단 주입 비율 (0-100)
	AbortCode    int           // HTTP 상태 코드 (예: 503)
}

// HTTPRoute는 하나의 HTTP 라우팅 규칙이다.
// 참조: networking/v1alpha3 의 HTTPRoute
type HTTPRoute struct {
	Name     string                 // 라우트 이름
	Match    []HTTPMatchRequest     // 매칭 조건 목록 (OR 결합)
	Route    []HTTPRouteDestination // 라우팅 목적지 목록 (가중치 기반)
	Timeout  time.Duration          // 요청 타임아웃
	Retries  *RetryPolicy           // 재시도 정책
	Fault    *FaultInjection        // 장애 주입
	// Istio는 rewrite, mirror, corsPolicy, headers도 지원
}

// RetryPolicy는 재시도 정책이다.
type RetryPolicy struct {
	Attempts int
	PerTryTimeout time.Duration
	RetryOn string // 재시도 조건 (예: "5xx,reset,connect-failure")
}

// VirtualService는 Istio의 VirtualService 리소스를 모델링한다.
// 참조: pilot/pkg/model/virtualservice.go
// 참조: networking/v1alpha3 의 VirtualService protobuf
type VirtualService struct {
	Name      string      // VirtualService 이름
	Namespace string      // 네임스페이스
	Hosts     []string    // 이 VS가 적용될 호스트 목록
	HTTP      []HTTPRoute // HTTP 라우팅 규칙 (순서대로 평가)
}

// ============================================================================
// 2. DestinationRule 모델
//    참조: networking/v1alpha3 의 DestinationRule protobuf
// ============================================================================

// LoadBalancerType은 로드밸런싱 알고리즘을 나타낸다.
type LoadBalancerType int

const (
	LBRoundRobin LoadBalancerType = iota
	LBRandom
	LBLeastConn
	LBConsistentHash
)

func (lb LoadBalancerType) String() string {
	switch lb {
	case LBRoundRobin:
		return "ROUND_ROBIN"
	case LBRandom:
		return "RANDOM"
	case LBLeastConn:
		return "LEAST_CONN"
	case LBConsistentHash:
		return "CONSISTENT_HASH"
	default:
		return "UNKNOWN"
	}
}

// TrafficPolicy는 트래픽 정책이다.
type TrafficPolicy struct {
	LoadBalancer     LoadBalancerType
	ConnectionPool   *ConnectionPoolSettings
	OutlierDetection *OutlierDetection
}

// ConnectionPoolSettings는 연결 풀 설정이다.
type ConnectionPoolSettings struct {
	MaxConnections     int
	ConnectTimeout     time.Duration
	MaxRequestsPerConn int
}

// OutlierDetection은 이상 감지 설정이다.
type OutlierDetection struct {
	ConsecutiveErrors int
	Interval          time.Duration
	BaseEjectionTime  time.Duration
}

// Subset은 DestinationRule의 서브셋이다.
// 레이블 셀렉터로 엔드포인트 그룹을 정의한다.
type Subset struct {
	Name          string            // 서브셋 이름 (예: "v1", "v2")
	Labels        map[string]string // 레이블 셀렉터 (예: version: v1)
	TrafficPolicy *TrafficPolicy    // 서브셋별 트래픽 정책
}

// DestinationRule은 Istio의 DestinationRule 리소스를 모델링한다.
type DestinationRule struct {
	Name          string         // DR 이름
	Namespace     string         // 네임스페이스
	Host          string         // 대상 서비스 호스트명
	TrafficPolicy *TrafficPolicy // 전역 트래픽 정책
	Subsets       []Subset       // 서브셋 목록
}

// ============================================================================
// 3. 서비스 레지스트리 (Service Endpoint)
// ============================================================================

// Endpoint는 서비스의 실제 인스턴스를 나타낸다.
type Endpoint struct {
	Address string            // IP:Port
	Labels  map[string]string // 파드 레이블
	Weight  int               // 엔드포인트 가중치
	Healthy bool              // 헬스 상태
}

// ServiceRegistry는 서비스 레지스트리이다.
type ServiceRegistry struct {
	services map[string][]Endpoint // host -> endpoints
}

// NewServiceRegistry는 새로운 서비스 레지스트리를 생성한다.
func NewServiceRegistry() *ServiceRegistry {
	return &ServiceRegistry{
		services: make(map[string][]Endpoint),
	}
}

// RegisterEndpoints는 서비스에 엔드포인트를 등록한다.
func (sr *ServiceRegistry) RegisterEndpoints(host string, endpoints []Endpoint) {
	sr.services[host] = endpoints
}

// GetEndpoints는 서비스의 엔드포인트를 반환한다.
func (sr *ServiceRegistry) GetEndpoints(host string) []Endpoint {
	return sr.services[host]
}

// FilterByLabels는 레이블 셀렉터에 맞는 엔드포인트를 필터링한다.
func (sr *ServiceRegistry) FilterByLabels(host string, labels map[string]string) []Endpoint {
	all := sr.services[host]
	if len(labels) == 0 {
		return all
	}

	var filtered []Endpoint
	for _, ep := range all {
		if matchLabels(ep.Labels, labels) && ep.Healthy {
			filtered = append(filtered, ep)
		}
	}
	return filtered
}

// matchLabels는 엔드포인트 레이블이 셀렉터에 부합하는지 검사한다.
func matchLabels(epLabels, selector map[string]string) bool {
	for k, v := range selector {
		if epLabels[k] != v {
			return false
		}
	}
	return true
}

// ============================================================================
// 4. Route Resolution Engine (라우트 해석 엔진)
//    참조: pilot/pkg/networking/core/route/route.go - BuildSidecarVirtualHostWrapper()
// ============================================================================

// HTTPRequest는 들어오는 HTTP 요청을 나타낸다.
type HTTPRequest struct {
	Host    string            // Host 헤더
	URI     string            // 요청 URI
	Method  string            // HTTP 메서드
	Headers map[string]string // 요청 헤더
}

// RouteResult는 라우팅 결과이다.
type RouteResult struct {
	Matched          bool
	RouteName        string
	MatchedRule      string
	DestinationHost  string
	DestinationSubset string
	SelectedEndpoint *Endpoint
	FaultInjected    *FaultResult
}

// FaultResult는 장애 주입 결과이다.
type FaultResult struct {
	Delayed bool
	Delay   time.Duration
	Aborted bool
	AbortCode int
}

// RouteEngine은 VirtualService/DestinationRule 기반 라우팅 엔진이다.
// Istio의 Pilot이 envoy에 전달하는 라우팅 설정을 런타임에 평가하는 것을 시뮬레이션한다.
type RouteEngine struct {
	virtualServices  []VirtualService
	destinationRules map[string]*DestinationRule // host -> DestinationRule
	registry         *ServiceRegistry
	rng              *rand.Rand
	// 로드밸런서 상태 (round robin용)
	rrCounters map[string]int // subset key -> counter
}

// NewRouteEngine은 새로운 라우팅 엔진을 생성한다.
func NewRouteEngine(registry *ServiceRegistry) *RouteEngine {
	return &RouteEngine{
		destinationRules: make(map[string]*DestinationRule),
		registry:         registry,
		rng:              rand.New(rand.NewSource(time.Now().UnixNano())),
		rrCounters:       make(map[string]int),
	}
}

// AddVirtualService는 VirtualService를 등록한다.
func (re *RouteEngine) AddVirtualService(vs VirtualService) {
	re.virtualServices = append(re.virtualServices, vs)
}

// AddDestinationRule은 DestinationRule을 등록한다.
func (re *RouteEngine) AddDestinationRule(dr DestinationRule) {
	re.destinationRules[dr.Host] = &dr
}

// Resolve는 HTTP 요청에 대한 라우팅을 해석한다.
//
// Istio의 실제 라우트 해석 흐름:
// 1. Host 매칭: VirtualService의 hosts 목록에서 요청 Host와 일치하는 VS 찾기
// 2. Route 매칭: VS의 HTTP 라우트 규칙을 순서대로 평가 (첫 번째 매칭 사용)
// 3. 가중치 기반 목적지 선택: 매칭된 라우트의 destination 중 가중치에 따라 선택
// 4. Subset 해석: DestinationRule에서 subset의 label selector 조회
// 5. 엔드포인트 선택: label이 일치하는 엔드포인트에서 LB 알고리즘으로 선택
//
// 참조: pilot/pkg/networking/core/route/route.go:104-155
func (re *RouteEngine) Resolve(req *HTTPRequest) RouteResult {
	// 1단계: Host 매칭으로 적합한 VirtualService 찾기
	// 참조: pilot/pkg/model/virtualservice.go:37-121 (SelectVirtualServices)
	vs := re.findVirtualService(req.Host)
	if vs == nil {
		return RouteResult{Matched: false, MatchedRule: "no matching VirtualService"}
	}

	// 2단계: HTTP 라우트 규칙 순서대로 평가
	// Istio는 HTTPRoute를 순서대로 평가하고 첫 번째 매칭을 사용한다.
	for _, route := range vs.HTTP {
		if matchHTTPRoute(route, req) {
			// 3단계: 장애 주입 확인
			var faultResult *FaultResult
			if route.Fault != nil {
				faultResult = re.evaluateFault(route.Fault)
				if faultResult.Aborted {
					return RouteResult{
						Matched:       true,
						RouteName:     route.Name,
						MatchedRule:   describeMatch(route, req),
						FaultInjected: faultResult,
					}
				}
			}

			// 4단계: 가중치 기반 목적지 선택
			dest := re.selectDestination(route.Route)

			// 5단계: DestinationRule에서 subset 레이블 조회
			labels := re.getSubsetLabels(dest.Host, dest.Subset)

			// 6단계: 엔드포인트 필터링 및 로드밸런싱
			endpoint := re.selectEndpoint(dest.Host, dest.Subset, labels)

			return RouteResult{
				Matched:           true,
				RouteName:         route.Name,
				MatchedRule:       describeMatch(route, req),
				DestinationHost:   dest.Host,
				DestinationSubset: dest.Subset,
				SelectedEndpoint:  endpoint,
				FaultInjected:     faultResult,
			}
		}
	}

	return RouteResult{Matched: false, MatchedRule: "no matching HTTP route"}
}

// findVirtualService는 Host가 일치하는 VirtualService를 찾는다.
// Istio에서는 가장 구체적인 호스트가 우선한다 (e.g., reviews.default.svc > *.default.svc > *)
// 참조: pilot/pkg/model/virtualservice.go:37-121
func (re *RouteEngine) findVirtualService(host string) *VirtualService {
	// 정확한 호스트 매칭 우선
	for i := range re.virtualServices {
		for _, vsHost := range re.virtualServices[i].Hosts {
			if vsHost == host {
				return &re.virtualServices[i]
			}
		}
	}
	// 와일드카드 매칭
	for i := range re.virtualServices {
		for _, vsHost := range re.virtualServices[i].Hosts {
			if vsHost == "*" || matchWildcardHost(vsHost, host) {
				return &re.virtualServices[i]
			}
		}
	}
	return nil
}

// matchWildcardHost는 와일드카드 호스트 매칭을 수행한다.
// 예: *.example.com은 foo.example.com과 매칭
func matchWildcardHost(pattern, host string) bool {
	if !strings.HasPrefix(pattern, "*.") {
		return false
	}
	suffix := pattern[1:] // ".example.com"
	return strings.HasSuffix(host, suffix)
}

// matchHTTPRoute는 HTTP 요청이 라우트의 매칭 조건에 부합하는지 검사한다.
// 매칭 조건이 없으면 모든 요청에 매칭된다.
// 매칭 조건이 여러 개이면 OR 결합이다.
func matchHTTPRoute(route HTTPRoute, req *HTTPRequest) bool {
	if len(route.Match) == 0 {
		return true // 매칭 조건 없으면 모든 요청에 매칭
	}
	for _, match := range route.Match {
		if match.Matches(req) {
			return true // OR: 하나라도 매칭되면 성공
		}
	}
	return false
}

// selectDestination은 가중치 기반으로 목적지를 선택한다.
// 참조: envoy의 weighted_clusters 구현
func (re *RouteEngine) selectDestination(destinations []HTTPRouteDestination) HTTPRouteDestination {
	if len(destinations) == 1 {
		return destinations[0]
	}

	// 가중치 합 계산
	totalWeight := 0
	for _, d := range destinations {
		totalWeight += d.Weight
	}

	// 랜덤 가중치 선택
	r := re.rng.Intn(totalWeight)
	cumulative := 0
	for _, d := range destinations {
		cumulative += d.Weight
		if r < cumulative {
			return d
		}
	}
	return destinations[len(destinations)-1]
}

// getSubsetLabels는 DestinationRule에서 subset의 레이블 셀렉터를 조회한다.
func (re *RouteEngine) getSubsetLabels(host, subset string) map[string]string {
	dr, ok := re.destinationRules[host]
	if !ok {
		return nil
	}
	for _, s := range dr.Subsets {
		if s.Name == subset {
			return s.Labels
		}
	}
	return nil
}

// selectEndpoint는 레이블이 일치하는 엔드포인트에서 로드밸런서로 하나를 선택한다.
func (re *RouteEngine) selectEndpoint(host, subset string, labels map[string]string) *Endpoint {
	endpoints := re.registry.FilterByLabels(host, labels)
	if len(endpoints) == 0 {
		return nil
	}

	// DestinationRule의 LB 정책 조회
	lbType := LBRoundRobin
	dr, ok := re.destinationRules[host]
	if ok {
		// subset 레벨의 LB 정책 우선
		for _, s := range dr.Subsets {
			if s.Name == subset && s.TrafficPolicy != nil {
				lbType = s.TrafficPolicy.LoadBalancer
				break
			}
		}
		// subset에 없으면 전역 정책 사용
		if lbType == LBRoundRobin && dr.TrafficPolicy != nil {
			lbType = dr.TrafficPolicy.LoadBalancer
		}
	}

	// 로드밸런싱 수행
	switch lbType {
	case LBRandom:
		idx := re.rng.Intn(len(endpoints))
		return &endpoints[idx]
	case LBRoundRobin:
		key := host + "/" + subset
		idx := re.rrCounters[key] % len(endpoints)
		re.rrCounters[key] = idx + 1
		return &endpoints[idx]
	default:
		return &endpoints[0]
	}
}

// evaluateFault는 장애 주입을 평가한다.
func (re *RouteEngine) evaluateFault(fault *FaultInjection) *FaultResult {
	result := &FaultResult{}

	// 지연 주입
	if fault.DelayPercent > 0 {
		if re.rng.Float64()*100 < fault.DelayPercent {
			result.Delayed = true
			result.Delay = fault.DelayDuration
		}
	}

	// 중단 주입
	if fault.AbortPercent > 0 {
		if re.rng.Float64()*100 < fault.AbortPercent {
			result.Aborted = true
			result.AbortCode = fault.AbortCode
		}
	}

	return result
}

// describeMatch는 매칭 조건을 설명하는 문자열을 반환한다.
func describeMatch(route HTTPRoute, req *HTTPRequest) string {
	if len(route.Match) == 0 {
		return "default (no match conditions)"
	}
	for _, m := range route.Match {
		if m.Matches(req) {
			parts := []string{}
			if !m.URI.IsEmpty() {
				parts = append(parts, fmt.Sprintf("uri=%s", m.URI))
			}
			if !m.Method.IsEmpty() {
				parts = append(parts, fmt.Sprintf("method=%s", m.Method))
			}
			for k, v := range m.Headers {
				parts = append(parts, fmt.Sprintf("header[%s]=%s", k, v))
			}
			if m.Name != "" {
				return fmt.Sprintf("%s (%s)", m.Name, strings.Join(parts, ", "))
			}
			return strings.Join(parts, ", ")
		}
	}
	return "unknown"
}

// ============================================================================
// 5. 시뮬레이션 헬퍼
// ============================================================================

// printRouteResult는 라우팅 결과를 출력한다.
func printRouteResult(req *HTTPRequest, result RouteResult) {
	fmt.Printf("  요청: %s %s (Host: %s", req.Method, req.URI, req.Host)
	if len(req.Headers) > 0 {
		fmt.Printf(", Headers: {")
		first := true
		for k, v := range req.Headers {
			if !first {
				fmt.Printf(", ")
			}
			fmt.Printf("%s: %s", k, v)
			first = false
		}
		fmt.Printf("}")
	}
	fmt.Println(")")

	if !result.Matched {
		fmt.Printf("    -> 매칭 실패: %s\n", result.MatchedRule)
		return
	}

	fmt.Printf("    -> 라우트: %s\n", result.RouteName)
	fmt.Printf("    -> 매칭 규칙: %s\n", result.MatchedRule)

	if result.FaultInjected != nil {
		if result.FaultInjected.Aborted {
			fmt.Printf("    -> [장애 주입] HTTP %d 중단\n", result.FaultInjected.AbortCode)
			fmt.Println()
			return
		}
		if result.FaultInjected.Delayed {
			fmt.Printf("    -> [장애 주입] %v 지연\n", result.FaultInjected.Delay)
		}
	}

	fmt.Printf("    -> 목적지: %s (subset: %s)\n", result.DestinationHost, result.DestinationSubset)

	if result.SelectedEndpoint != nil {
		fmt.Printf("    -> 엔드포인트: %s (labels: %v)\n",
			result.SelectedEndpoint.Address, result.SelectedEndpoint.Labels)
	} else {
		fmt.Printf("    -> 엔드포인트: <없음>\n")
	}
	fmt.Println()
}

// ============================================================================
// 6. 데모 시나리오 구성
// ============================================================================

func setupBookinfoDemo() (*RouteEngine, *ServiceRegistry) {
	registry := NewServiceRegistry()

	// reviews 서비스 엔드포인트 (v1, v2, v3)
	registry.RegisterEndpoints("reviews.default.svc.cluster.local", []Endpoint{
		{Address: "10.0.1.1:9080", Labels: map[string]string{"app": "reviews", "version": "v1"}, Healthy: true},
		{Address: "10.0.1.2:9080", Labels: map[string]string{"app": "reviews", "version": "v1"}, Healthy: true},
		{Address: "10.0.2.1:9080", Labels: map[string]string{"app": "reviews", "version": "v2"}, Healthy: true},
		{Address: "10.0.2.2:9080", Labels: map[string]string{"app": "reviews", "version": "v2"}, Healthy: true},
		{Address: "10.0.3.1:9080", Labels: map[string]string{"app": "reviews", "version": "v3"}, Healthy: true},
	})

	// ratings 서비스 엔드포인트
	registry.RegisterEndpoints("ratings.default.svc.cluster.local", []Endpoint{
		{Address: "10.0.4.1:9080", Labels: map[string]string{"app": "ratings", "version": "v1"}, Healthy: true},
		{Address: "10.0.4.2:9080", Labels: map[string]string{"app": "ratings", "version": "v1"}, Healthy: true},
	})

	// productpage 서비스 엔드포인트
	registry.RegisterEndpoints("productpage.default.svc.cluster.local", []Endpoint{
		{Address: "10.0.5.1:9080", Labels: map[string]string{"app": "productpage", "version": "v1"}, Healthy: true},
	})

	engine := NewRouteEngine(registry)
	return engine, registry
}

// ============================================================================
// main
// ============================================================================

func main() {
	fmt.Println("==========================================================")
	fmt.Println(" Istio VirtualService/DestinationRule 라우팅 시뮬레이션")
	fmt.Println("==========================================================")
	fmt.Println()

	// --------------------------------------------------------
	// 시나리오 1: 가중치 기반 카나리 라우팅
	// --------------------------------------------------------
	fmt.Println("[시나리오 1] 가중치 기반 카나리 라우팅")
	fmt.Println("----------------------------------------------------------")
	fmt.Println("  reviews 서비스에 대해 v1에 80%, v2에 20% 트래픽 분배.")
	fmt.Println("  이는 Istio에서 가장 흔한 카나리 배포 패턴이다.")
	fmt.Println()
	fmt.Println("  VirtualService 설정:")
	fmt.Println("    hosts: [reviews.default.svc.cluster.local]")
	fmt.Println("    http:")
	fmt.Println("    - route:")
	fmt.Println("      - destination: {host: reviews, subset: v1}")
	fmt.Println("        weight: 80")
	fmt.Println("      - destination: {host: reviews, subset: v2}")
	fmt.Println("        weight: 20")
	fmt.Println()

	engine, _ := setupBookinfoDemo()

	// DestinationRule 등록
	engine.AddDestinationRule(DestinationRule{
		Name:      "reviews-dr",
		Namespace: "default",
		Host:      "reviews.default.svc.cluster.local",
		TrafficPolicy: &TrafficPolicy{
			LoadBalancer: LBRoundRobin,
		},
		Subsets: []Subset{
			{
				Name:   "v1",
				Labels: map[string]string{"app": "reviews", "version": "v1"},
			},
			{
				Name:   "v2",
				Labels: map[string]string{"app": "reviews", "version": "v2"},
			},
			{
				Name:   "v3",
				Labels: map[string]string{"app": "reviews", "version": "v3"},
			},
		},
	})

	// VirtualService: 가중치 기반 카나리
	engine.AddVirtualService(VirtualService{
		Name:      "reviews-canary",
		Namespace: "default",
		Hosts:     []string{"reviews.default.svc.cluster.local"},
		HTTP: []HTTPRoute{
			{
				Name: "canary-route",
				Route: []HTTPRouteDestination{
					{Host: "reviews.default.svc.cluster.local", Subset: "v1", Weight: 80},
					{Host: "reviews.default.svc.cluster.local", Subset: "v2", Weight: 20},
				},
			},
		},
	})

	// 요청 시뮬레이션 (10회)
	v1Count, v2Count := 0, 0
	for i := 0; i < 10; i++ {
		req := &HTTPRequest{
			Host:   "reviews.default.svc.cluster.local",
			URI:    "/api/reviews",
			Method: http.MethodGet,
		}
		result := engine.Resolve(req)
		if result.DestinationSubset == "v1" {
			v1Count++
		} else if result.DestinationSubset == "v2" {
			v2Count++
		}
		printRouteResult(req, result)
	}
	fmt.Printf("  [통계] 10회 요청 결과: v1=%d회, v2=%d회\n", v1Count, v2Count)
	fmt.Printf("  (가중치 80:20이므로 v1이 대략 8회, v2가 2회에 가까울 것으로 예상)\n\n")

	// --------------------------------------------------------
	// 시나리오 2: 헤더 기반 라우팅 (A/B 테스트)
	// --------------------------------------------------------
	fmt.Println("==========================================================")
	fmt.Println("[시나리오 2] 헤더 기반 라우팅 (A/B 테스트)")
	fmt.Println("----------------------------------------------------------")
	fmt.Println("  특정 사용자(end-user: jason)의 요청을 v2로 라우팅하고,")
	fmt.Println("  나머지는 v1으로 라우팅한다.")
	fmt.Println()
	fmt.Println("  VirtualService 설정:")
	fmt.Println("    http:")
	fmt.Println("    - match:")
	fmt.Println("      - headers: {end-user: {exact: jason}}")
	fmt.Println("      route:")
	fmt.Println("      - destination: {host: reviews, subset: v2}")
	fmt.Println("    - route:  # default")
	fmt.Println("      - destination: {host: reviews, subset: v1}")
	fmt.Println()

	engine2, _ := setupBookinfoDemo()

	engine2.AddDestinationRule(DestinationRule{
		Name: "reviews-dr",
		Host: "reviews.default.svc.cluster.local",
		Subsets: []Subset{
			{Name: "v1", Labels: map[string]string{"app": "reviews", "version": "v1"}},
			{Name: "v2", Labels: map[string]string{"app": "reviews", "version": "v2"}},
		},
	})

	engine2.AddVirtualService(VirtualService{
		Name:  "reviews-header-routing",
		Hosts: []string{"reviews.default.svc.cluster.local"},
		HTTP: []HTTPRoute{
			{
				Name: "jason-route",
				Match: []HTTPMatchRequest{
					{
						Name: "jason-match",
						Headers: map[string]StringMatch{
							"end-user": {Exact: "jason"},
						},
					},
				},
				Route: []HTTPRouteDestination{
					{Host: "reviews.default.svc.cluster.local", Subset: "v2", Weight: 100},
				},
			},
			{
				Name: "default-route",
				// 매칭 조건 없으면 모든 요청에 매칭 (catch-all)
				Route: []HTTPRouteDestination{
					{Host: "reviews.default.svc.cluster.local", Subset: "v1", Weight: 100},
				},
			},
		},
	})

	// jason 사용자의 요청
	jasonReq := &HTTPRequest{
		Host:    "reviews.default.svc.cluster.local",
		URI:     "/api/reviews",
		Method:  http.MethodGet,
		Headers: map[string]string{"end-user": "jason"},
	}
	printRouteResult(jasonReq, engine2.Resolve(jasonReq))

	// 일반 사용자의 요청
	normalReq := &HTTPRequest{
		Host:    "reviews.default.svc.cluster.local",
		URI:     "/api/reviews",
		Method:  http.MethodGet,
		Headers: map[string]string{"end-user": "alice"},
	}
	printRouteResult(normalReq, engine2.Resolve(normalReq))

	// 헤더 없는 요청
	noHeaderReq := &HTTPRequest{
		Host:   "reviews.default.svc.cluster.local",
		URI:    "/api/reviews",
		Method: http.MethodGet,
	}
	printRouteResult(noHeaderReq, engine2.Resolve(noHeaderReq))

	// --------------------------------------------------------
	// 시나리오 3: URI prefix 기반 라우팅
	// --------------------------------------------------------
	fmt.Println("==========================================================")
	fmt.Println("[시나리오 3] URI Prefix 기반 라우팅")
	fmt.Println("----------------------------------------------------------")
	fmt.Println("  URI 경로에 따라 다른 서비스로 라우팅한다.")
	fmt.Println("  /api/v1/* -> reviews v1, /api/v2/* -> reviews v2")
	fmt.Println()

	engine3, _ := setupBookinfoDemo()

	engine3.AddDestinationRule(DestinationRule{
		Name: "reviews-dr",
		Host: "reviews.default.svc.cluster.local",
		Subsets: []Subset{
			{Name: "v1", Labels: map[string]string{"app": "reviews", "version": "v1"}},
			{Name: "v2", Labels: map[string]string{"app": "reviews", "version": "v2"}},
			{Name: "v3", Labels: map[string]string{"app": "reviews", "version": "v3"}},
		},
	})

	engine3.AddVirtualService(VirtualService{
		Name:  "reviews-uri-routing",
		Hosts: []string{"reviews.default.svc.cluster.local"},
		HTTP: []HTTPRoute{
			{
				Name: "v1-api-route",
				Match: []HTTPMatchRequest{
					{Name: "v1-prefix", URI: StringMatch{Prefix: "/api/v1/"}},
				},
				Route: []HTTPRouteDestination{
					{Host: "reviews.default.svc.cluster.local", Subset: "v1", Weight: 100},
				},
			},
			{
				Name: "v2-api-route",
				Match: []HTTPMatchRequest{
					{Name: "v2-prefix", URI: StringMatch{Prefix: "/api/v2/"}},
				},
				Route: []HTTPRouteDestination{
					{Host: "reviews.default.svc.cluster.local", Subset: "v2", Weight: 100},
				},
			},
			{
				Name: "v3-exact-route",
				Match: []HTTPMatchRequest{
					{Name: "v3-exact", URI: StringMatch{Exact: "/api/v3/reviews"}},
				},
				Route: []HTTPRouteDestination{
					{Host: "reviews.default.svc.cluster.local", Subset: "v3", Weight: 100},
				},
			},
			{
				Name: "default-route",
				Route: []HTTPRouteDestination{
					{Host: "reviews.default.svc.cluster.local", Subset: "v1", Weight: 100},
				},
			},
		},
	})

	uriTests := []*HTTPRequest{
		{Host: "reviews.default.svc.cluster.local", URI: "/api/v1/reviews", Method: http.MethodGet},
		{Host: "reviews.default.svc.cluster.local", URI: "/api/v1/ratings", Method: http.MethodGet},
		{Host: "reviews.default.svc.cluster.local", URI: "/api/v2/reviews", Method: http.MethodGet},
		{Host: "reviews.default.svc.cluster.local", URI: "/api/v3/reviews", Method: http.MethodGet},
		{Host: "reviews.default.svc.cluster.local", URI: "/api/v3/other", Method: http.MethodGet},
		{Host: "reviews.default.svc.cluster.local", URI: "/health", Method: http.MethodGet},
	}

	for _, req := range uriTests {
		printRouteResult(req, engine3.Resolve(req))
	}

	// --------------------------------------------------------
	// 시나리오 4: 복합 매칭 (URI + Header + Method)
	// --------------------------------------------------------
	fmt.Println("==========================================================")
	fmt.Println("[시나리오 4] 복합 매칭 (URI + Header + Method)")
	fmt.Println("----------------------------------------------------------")
	fmt.Println("  여러 조건을 AND로 결합한 복합 매칭 규칙 시뮬레이션.")
	fmt.Println("  POST /api/reviews + x-test: canary -> v3")
	fmt.Println()

	engine4, _ := setupBookinfoDemo()

	engine4.AddDestinationRule(DestinationRule{
		Name: "reviews-dr",
		Host: "reviews.default.svc.cluster.local",
		Subsets: []Subset{
			{Name: "v1", Labels: map[string]string{"app": "reviews", "version": "v1"}},
			{Name: "v2", Labels: map[string]string{"app": "reviews", "version": "v2"}},
			{Name: "v3", Labels: map[string]string{"app": "reviews", "version": "v3"}},
		},
	})

	engine4.AddVirtualService(VirtualService{
		Name:  "reviews-complex",
		Hosts: []string{"reviews.default.svc.cluster.local"},
		HTTP: []HTTPRoute{
			{
				Name: "canary-test-route",
				Match: []HTTPMatchRequest{
					{
						Name:   "canary-complex",
						URI:    StringMatch{Prefix: "/api/"},
						Method: StringMatch{Exact: http.MethodPost},
						Headers: map[string]StringMatch{
							"x-test": {Exact: "canary"},
						},
					},
				},
				Route: []HTTPRouteDestination{
					{Host: "reviews.default.svc.cluster.local", Subset: "v3", Weight: 100},
				},
			},
			{
				Name: "default-route",
				Route: []HTTPRouteDestination{
					{Host: "reviews.default.svc.cluster.local", Subset: "v1", Weight: 100},
				},
			},
		},
	})

	complexTests := []*HTTPRequest{
		// 모든 조건 충족: POST + /api/ + x-test: canary -> v3
		{
			Host: "reviews.default.svc.cluster.local", URI: "/api/reviews", Method: http.MethodPost,
			Headers: map[string]string{"x-test": "canary"},
		},
		// URI만 충족: GET /api/reviews (Method 불일치) -> v1 (default)
		{
			Host: "reviews.default.svc.cluster.local", URI: "/api/reviews", Method: http.MethodGet,
			Headers: map[string]string{"x-test": "canary"},
		},
		// URI + Method 충족하지만 헤더 불일치 -> v1 (default)
		{
			Host: "reviews.default.svc.cluster.local", URI: "/api/reviews", Method: http.MethodPost,
			Headers: map[string]string{"x-test": "stable"},
		},
		// 모든 조건 불일치 -> v1 (default)
		{
			Host: "reviews.default.svc.cluster.local", URI: "/health", Method: http.MethodGet,
		},
	}

	for _, req := range complexTests {
		printRouteResult(req, engine4.Resolve(req))
	}

	// --------------------------------------------------------
	// 시나리오 5: 장애 주입 (Fault Injection)
	// --------------------------------------------------------
	fmt.Println("==========================================================")
	fmt.Println("[시나리오 5] 장애 주입 (Fault Injection)")
	fmt.Println("----------------------------------------------------------")
	fmt.Println("  ratings 서비스에 100% 확률로 HTTP 503 중단을 주입한다.")
	fmt.Println("  Istio의 HTTPFaultInjection 기능을 시뮬레이션한다.")
	fmt.Println()

	engine5, _ := setupBookinfoDemo()

	engine5.AddDestinationRule(DestinationRule{
		Name: "ratings-dr",
		Host: "ratings.default.svc.cluster.local",
		Subsets: []Subset{
			{Name: "v1", Labels: map[string]string{"app": "ratings", "version": "v1"}},
		},
	})

	engine5.AddVirtualService(VirtualService{
		Name:  "ratings-fault",
		Hosts: []string{"ratings.default.svc.cluster.local"},
		HTTP: []HTTPRoute{
			{
				Name: "fault-route",
				Match: []HTTPMatchRequest{
					{
						Name: "jason-fault",
						Headers: map[string]StringMatch{
							"end-user": {Exact: "jason"},
						},
					},
				},
				Fault: &FaultInjection{
					AbortPercent: 100,
					AbortCode:    http.StatusServiceUnavailable,
				},
				Route: []HTTPRouteDestination{
					{Host: "ratings.default.svc.cluster.local", Subset: "v1", Weight: 100},
				},
			},
			{
				Name: "delay-route",
				Match: []HTTPMatchRequest{
					{
						Name: "tester-delay",
						Headers: map[string]StringMatch{
							"end-user": {Exact: "tester"},
						},
					},
				},
				Fault: &FaultInjection{
					DelayPercent:  100,
					DelayDuration: 5 * time.Second,
				},
				Route: []HTTPRouteDestination{
					{Host: "ratings.default.svc.cluster.local", Subset: "v1", Weight: 100},
				},
			},
			{
				Name: "default-route",
				Route: []HTTPRouteDestination{
					{Host: "ratings.default.svc.cluster.local", Subset: "v1", Weight: 100},
				},
			},
		},
	})

	faultTests := []*HTTPRequest{
		// jason 사용자 -> 503 중단
		{
			Host: "ratings.default.svc.cluster.local", URI: "/api/ratings", Method: http.MethodGet,
			Headers: map[string]string{"end-user": "jason"},
		},
		// tester 사용자 -> 5초 지연
		{
			Host: "ratings.default.svc.cluster.local", URI: "/api/ratings", Method: http.MethodGet,
			Headers: map[string]string{"end-user": "tester"},
		},
		// 일반 사용자 -> 정상
		{
			Host: "ratings.default.svc.cluster.local", URI: "/api/ratings", Method: http.MethodGet,
			Headers: map[string]string{"end-user": "alice"},
		},
	}

	for _, req := range faultTests {
		printRouteResult(req, engine5.Resolve(req))
	}

	// --------------------------------------------------------
	// 시나리오 6: DestinationRule 상세 - 로드밸런싱 정책
	// --------------------------------------------------------
	fmt.Println("==========================================================")
	fmt.Println("[시나리오 6] DestinationRule 로드밸런싱 정책")
	fmt.Println("----------------------------------------------------------")
	fmt.Println("  subset별로 다른 로드밸런싱 정책을 적용한다.")
	fmt.Println("  v1: ROUND_ROBIN, v2: RANDOM")
	fmt.Println()

	engine6, _ := setupBookinfoDemo()

	engine6.AddDestinationRule(DestinationRule{
		Name: "reviews-dr-lb",
		Host: "reviews.default.svc.cluster.local",
		TrafficPolicy: &TrafficPolicy{
			LoadBalancer: LBRoundRobin,
			ConnectionPool: &ConnectionPoolSettings{
				MaxConnections:     100,
				ConnectTimeout:     5 * time.Second,
				MaxRequestsPerConn: 10,
			},
			OutlierDetection: &OutlierDetection{
				ConsecutiveErrors: 5,
				Interval:          10 * time.Second,
				BaseEjectionTime:  30 * time.Second,
			},
		},
		Subsets: []Subset{
			{
				Name:   "v1",
				Labels: map[string]string{"app": "reviews", "version": "v1"},
				TrafficPolicy: &TrafficPolicy{
					LoadBalancer: LBRoundRobin,
				},
			},
			{
				Name:   "v2",
				Labels: map[string]string{"app": "reviews", "version": "v2"},
				TrafficPolicy: &TrafficPolicy{
					LoadBalancer: LBRandom,
				},
			},
		},
	})

	engine6.AddVirtualService(VirtualService{
		Name:  "reviews-lb-test",
		Hosts: []string{"reviews.default.svc.cluster.local"},
		HTTP: []HTTPRoute{
			{
				Name: "v1-route",
				Match: []HTTPMatchRequest{
					{URI: StringMatch{Prefix: "/v1/"}},
				},
				Route: []HTTPRouteDestination{
					{Host: "reviews.default.svc.cluster.local", Subset: "v1", Weight: 100},
				},
			},
			{
				Name: "v2-route",
				Match: []HTTPMatchRequest{
					{URI: StringMatch{Prefix: "/v2/"}},
				},
				Route: []HTTPRouteDestination{
					{Host: "reviews.default.svc.cluster.local", Subset: "v2", Weight: 100},
				},
			},
		},
	})

	fmt.Println("  v1 subset (ROUND_ROBIN) - 6회 요청:")
	for i := 0; i < 6; i++ {
		req := &HTTPRequest{
			Host: "reviews.default.svc.cluster.local", URI: "/v1/reviews", Method: http.MethodGet,
		}
		result := engine6.Resolve(req)
		if result.SelectedEndpoint != nil {
			fmt.Printf("    요청 %d -> %s\n", i+1, result.SelectedEndpoint.Address)
		}
	}
	fmt.Println()

	fmt.Println("  v2 subset (RANDOM) - 6회 요청:")
	for i := 0; i < 6; i++ {
		req := &HTTPRequest{
			Host: "reviews.default.svc.cluster.local", URI: "/v2/reviews", Method: http.MethodGet,
		}
		result := engine6.Resolve(req)
		if result.SelectedEndpoint != nil {
			fmt.Printf("    요청 %d -> %s\n", i+1, result.SelectedEndpoint.Address)
		}
	}
	fmt.Println()

	// --------------------------------------------------------
	// 시나리오 7: 호스트 매칭 실패 케이스
	// --------------------------------------------------------
	fmt.Println("==========================================================")
	fmt.Println("[시나리오 7] 호스트 매칭 실패 및 경계 케이스")
	fmt.Println("----------------------------------------------------------")
	fmt.Println()

	// 등록되지 않은 호스트에 대한 요청
	unknownReq := &HTTPRequest{
		Host: "unknown-service.default.svc.cluster.local", URI: "/api/test", Method: http.MethodGet,
	}
	printRouteResult(unknownReq, engine6.Resolve(unknownReq))

	// --------------------------------------------------------
	// 요약
	// --------------------------------------------------------
	fmt.Println("==========================================================")
	fmt.Println(" 요약: Istio 트래픽 라우팅 핵심 메커니즘")
	fmt.Println("==========================================================")
	fmt.Println()
	fmt.Println("  1. VirtualService (라우팅 규칙)")
	fmt.Println("     - hosts: 어떤 서비스에 적용할지 결정")
	fmt.Println("     - HTTP route: 순서대로 매칭 (첫 번째 일치 사용)")
	fmt.Println("     - match: URI prefix, exact, header, method 등 조건")
	fmt.Println("     - route: 가중치 기반 목적지 분배")
	fmt.Println("     - fault: 장애 주입 (지연/중단)")
	fmt.Println()
	fmt.Println("  2. DestinationRule (대상 정책)")
	fmt.Println("     - subsets: 레이블 셀렉터로 엔드포인트 그룹 정의")
	fmt.Println("     - trafficPolicy: LB 알고리즘, 연결 풀, 이상 감지")
	fmt.Println("     - 전역 정책 + subset 레벨 정책 (subset 우선)")
	fmt.Println()
	fmt.Println("  3. 라우트 해석 흐름")
	fmt.Println("     요청 -> Host 매칭 -> HTTP Route 매칭 ->")
	fmt.Println("     가중치 목적지 선택 -> Subset 레이블 조회 ->")
	fmt.Println("     엔드포인트 필터링 -> LB 알고리즘 선택")
	fmt.Println()
	fmt.Println("  4. Pilot -> Envoy 변환")
	fmt.Println("     Pilot(istiod)이 VS/DR을 Envoy의")
	fmt.Println("     Route/Cluster/Endpoint 설정으로 변환하여 xDS로 전달")
	fmt.Println()
}
