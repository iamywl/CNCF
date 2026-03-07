// poc-circuit-breaker: Istio 서킷 브레이커 및 아웃라이어 디텍션 시뮬레이션
//
// Istio는 DestinationRule의 TrafficPolicy를 통해 서킷 브레이커와 아웃라이어 디텍션을 설정한다.
// Pilot(istiod)은 이 설정을 Envoy 클러스터 설정(CircuitBreakers, OutlierDetection)으로 변환하여 xDS로 전달한다.
//
// 핵심 참조:
//   - pilot/pkg/networking/core/cluster_traffic_policy.go — applyConnectionPool(), applyOutlierDetection()
//   - pilot/pkg/networking/core/cluster_traffic_policy.go — getDefaultCircuitBreakerThresholds()
//
// 이 PoC는 커넥션 풀 제한(maxConnections, maxPendingRequests)과
// 아웃라이어 디텍션(consecutive5xxErrors, ejection/recovery) 동작을 시뮬레이션한다.

package main

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================================================
// 1. 서킷 브레이커 설정 (Istio DestinationRule 기반)
// ============================================================================

// ConnectionPoolSettings는 Istio의 networking.ConnectionPoolSettings를 시뮬레이션한다.
// Istio 소스: networking/v1/destination_rule.pb.go
type ConnectionPoolSettings struct {
	MaxConnections     int // TCP 최대 커넥션 수 (settings.Tcp.MaxConnections)
	MaxPendingRequests int // HTTP 최대 대기 요청 수 (settings.Http.Http1MaxPendingRequests)
	MaxRequestsPerConn int // 커넥션당 최대 요청 수 (settings.Http.MaxRequestsPerConnection)
}

// OutlierDetectionConfig는 Istio의 networking.OutlierDetection을 시뮬레이션한다.
// Istio 소스: pilot/pkg/networking/core/cluster_traffic_policy.go — applyOutlierDetection()
type OutlierDetectionConfig struct {
	Consecutive5xxErrors int           // 연속 5xx 에러 임계값 (outlier.Consecutive_5XxErrors)
	Interval             time.Duration // 분석 간격 (outlier.Interval)
	BaseEjectionTime     time.Duration // 기본 퇴출 시간 (outlier.BaseEjectionTime)
	MaxEjectionPercent   int           // 최대 퇴출 비율 (outlier.MaxEjectionPercent)
}

// ============================================================================
// 2. 서킷 브레이커 상태 머신 (Closed → Open → Half-Open → Closed)
// ============================================================================

// CircuitState는 서킷 브레이커의 상태를 나타낸다.
// Envoy는 내부적으로 이 상태 머신을 사용하여 트래픽을 제어한다.
type CircuitState int

const (
	StateClosed   CircuitState = iota // 정상 — 모든 요청 허용
	StateOpen                         // 차단 — 요청 즉시 거부
	StateHalfOpen                     // 반개방 — 제한적 요청 허용하여 복구 테스트
)

func (s CircuitState) String() string {
	switch s {
	case StateClosed:
		return "CLOSED"
	case StateOpen:
		return "OPEN"
	case StateHalfOpen:
		return "HALF-OPEN"
	default:
		return "UNKNOWN"
	}
}

// ============================================================================
// 3. 엔드포인트 (업스트림 호스트)
// ============================================================================

// Endpoint는 업스트림 서비스의 개별 인스턴스를 나타낸다.
type Endpoint struct {
	Name             string
	FailureRate      float64 // 실패 확률 (0.0~1.0)
	Consecutive5xx   int     // 현재 연속 5xx 카운트
	Ejected          bool    // 퇴출 상태
	EjectedAt        time.Time
	EjectionCount    int // 누적 퇴출 횟수 (퇴출 시간 = baseEjectionTime * ejectionCount)
	TotalRequests    int
	TotalFailures    int
	mu               sync.Mutex
}

// HandleRequest는 엔드포인트에 요청을 보내 성공/실패를 시뮬레이션한다.
func (e *Endpoint) HandleRequest() (statusCode int) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.TotalRequests++

	if rand.Float64() < e.FailureRate {
		e.TotalFailures++
		e.Consecutive5xx++
		return 503
	}

	e.Consecutive5xx = 0
	return 200
}

// ============================================================================
// 4. 커넥션 풀 (서킷 브레이커 핵심 메커니즘)
// ============================================================================

// ConnectionPool은 Istio/Envoy의 서킷 브레이커에서 커넥션 풀 제한을 시뮬레이션한다.
// Istio 소스에서 getDefaultCircuitBreakerThresholds()는 기본값을 MaxUint32로 설정하고,
// applyConnectionPool()에서 사용자 설정으로 오버라이드한다.
//
// 참조: pilot/pkg/networking/core/cluster_traffic_policy.go:
//
//	threshold.MaxConnections = &wrapperspb.UInt32Value{Value: uint32(settings.Tcp.MaxConnections)}
//	threshold.MaxPendingRequests = &wrapperspb.UInt32Value{Value: uint32(settings.Http.Http1MaxPendingRequests)}
type ConnectionPool struct {
	maxConnections     int
	maxPendingRequests int
	maxRequestsPerConn int

	activeConnections int32 // 현재 활성 커넥션 수
	pendingRequests   int32 // 현재 대기 중인 요청 수

	mu              sync.Mutex
	totalOverflows  int // 오버플로우(거부)된 요청 수
	totalProcessed  int // 처리된 요청 수
}

// NewConnectionPool은 커넥션 풀을 생성한다.
func NewConnectionPool(settings ConnectionPoolSettings) *ConnectionPool {
	return &ConnectionPool{
		maxConnections:     settings.MaxConnections,
		maxPendingRequests: settings.MaxPendingRequests,
		maxRequestsPerConn: settings.MaxRequestsPerConn,
	}
}

// TryAcquire는 커넥션을 획득하려고 시도한다.
// 커넥션 풀이 가득 차면 대기열에 넣고, 대기열도 가득 차면 오버플로우를 반환한다.
// Envoy는 CircuitBreakers.Thresholds의 MaxConnections/MaxPendingRequests로 이를 제어한다.
func (cp *ConnectionPool) TryAcquire() (acquired bool, overflow bool) {
	cp.mu.Lock()
	defer cp.mu.Unlock()

	// 활성 커넥션에 여유가 있으면 즉시 획득
	if int(cp.activeConnections) < cp.maxConnections {
		cp.activeConnections++
		cp.totalProcessed++
		return true, false
	}

	// 커넥션 가득참 → 대기열 확인
	if int(cp.pendingRequests) < cp.maxPendingRequests {
		cp.pendingRequests++
		return false, false // 대기열에 들어감
	}

	// 대기열도 가득참 → 오버플로우 (503 반환)
	cp.totalOverflows++
	return false, true
}

// Release는 커넥션을 반환한다.
func (cp *ConnectionPool) Release() {
	cp.mu.Lock()
	defer cp.mu.Unlock()

	if cp.activeConnections > 0 {
		cp.activeConnections--
	}

	// 대기 중인 요청이 있으면 하나를 활성화
	if cp.pendingRequests > 0 && int(cp.activeConnections) < cp.maxConnections {
		cp.pendingRequests--
		cp.activeConnections++
		cp.totalProcessed++
	}
}

// ReleasePending은 대기 중인 요청을 대기열에서 제거한다.
func (cp *ConnectionPool) ReleasePending() {
	cp.mu.Lock()
	defer cp.mu.Unlock()

	if cp.pendingRequests > 0 {
		cp.pendingRequests--
	}
}

// Stats는 커넥션 풀 통계를 반환한다.
func (cp *ConnectionPool) Stats() (active, pending, overflows, processed int) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return int(cp.activeConnections), int(cp.pendingRequests), cp.totalOverflows, cp.totalProcessed
}

// ============================================================================
// 5. 아웃라이어 디텍터 (엔드포인트 퇴출/복구)
// ============================================================================

// OutlierDetector는 Istio/Envoy의 아웃라이어 디텍션을 시뮬레이션한다.
// Istio 소스에서 applyOutlierDetection()은 OutlierDetection protobuf를 Envoy 설정으로 변환한다.
//
// 핵심 동작:
// 1. 연속 5xx 에러가 임계값에 도달하면 엔드포인트를 퇴출
// 2. baseEjectionTime * ejectionCount 만큼 퇴출 유지 (점진적 증가)
// 3. maxEjectionPercent 이상 퇴출하지 않음 (최소 가용성 보장)
// 4. 퇴출 시간이 지나면 자동 복구 (Half-Open → Closed 전환)
//
// 참조: pilot/pkg/networking/core/cluster_traffic_policy.go:450-517
type OutlierDetector struct {
	config    OutlierDetectionConfig
	endpoints []*Endpoint
	mu        sync.Mutex
}

// NewOutlierDetector는 아웃라이어 디텍터를 생성한다.
func NewOutlierDetector(config OutlierDetectionConfig, endpoints []*Endpoint) *OutlierDetector {
	return &OutlierDetector{
		config:    config,
		endpoints: endpoints,
	}
}

// Check는 모든 엔드포인트를 검사하여 아웃라이어를 감지하고 퇴출/복구를 수행한다.
// Envoy는 outlier.Interval 간격으로 이 검사를 수행한다.
func (od *OutlierDetector) Check() {
	od.mu.Lock()
	defer od.mu.Unlock()

	now := time.Now()
	ejectedCount := 0
	totalCount := len(od.endpoints)

	// 현재 퇴출된 엔드포인트 수 계산
	for _, ep := range od.endpoints {
		ep.mu.Lock()
		if ep.Ejected {
			ejectedCount++
		}
		ep.mu.Unlock()
	}

	for _, ep := range od.endpoints {
		ep.mu.Lock()

		// --- 복구 체크 ---
		// 퇴출된 엔드포인트의 퇴출 시간이 경과하면 복구
		// 퇴출 시간 = baseEjectionTime * ejectionCount (점진적 증가)
		if ep.Ejected {
			ejectionDuration := od.config.BaseEjectionTime * time.Duration(ep.EjectionCount)
			if now.Sub(ep.EjectedAt) >= ejectionDuration {
				ep.Ejected = false
				ep.Consecutive5xx = 0
				ejectedCount--
				fmt.Printf("  [복구] %s: 퇴출 시간 %v 경과 → 복구됨 (누적 퇴출: %d회)\n",
					ep.Name, ejectionDuration, ep.EjectionCount)
			}
			ep.mu.Unlock()
			continue
		}

		// --- 퇴출 체크 ---
		// 연속 5xx 에러가 임계값에 도달하면 퇴출
		// Istio 소스: out.Consecutive_5Xx = &wrapperspb.UInt32Value{Value: v}
		if ep.Consecutive5xx >= od.config.Consecutive5xxErrors && od.config.Consecutive5xxErrors > 0 {
			// maxEjectionPercent 검사
			// Istio 소스: out.MaxEjectionPercent = &wrapperspb.UInt32Value{Value: uint32(outlier.MaxEjectionPercent)}
			currentEjectionPercent := (ejectedCount * 100) / totalCount
			if currentEjectionPercent < od.config.MaxEjectionPercent {
				ep.Ejected = true
				ep.EjectedAt = now
				ep.EjectionCount++
				ejectedCount++
				fmt.Printf("  [퇴출] %s: 연속 5xx %d회 도달 → 퇴출 (기간: %v, 현재 퇴출률: %d%%)\n",
					ep.Name, ep.Consecutive5xx,
					od.config.BaseEjectionTime*time.Duration(ep.EjectionCount),
					(ejectedCount*100)/totalCount)
			} else {
				fmt.Printf("  [보호] %s: 퇴출 대상이지만 최대 퇴출 비율 %d%% 도달 → 퇴출 생략\n",
					ep.Name, od.config.MaxEjectionPercent)
			}
		}

		ep.mu.Unlock()
	}
}

// GetHealthyEndpoints는 현재 건강한(퇴출되지 않은) 엔드포인트 목록을 반환한다.
func (od *OutlierDetector) GetHealthyEndpoints() []*Endpoint {
	od.mu.Lock()
	defer od.mu.Unlock()

	var healthy []*Endpoint
	for _, ep := range od.endpoints {
		ep.mu.Lock()
		if !ep.Ejected {
			healthy = append(healthy, ep)
		}
		ep.mu.Unlock()
	}
	return healthy
}

// ============================================================================
// 6. 서킷 브레이커 통합 (커넥션 풀 + 아웃라이어 디텍션 + 상태 머신)
// ============================================================================

// CircuitBreaker는 Istio의 DestinationRule TrafficPolicy를 종합적으로 시뮬레이션한다.
// applyTrafficPolicy()에서 connectionPool + outlierDetection이 함께 적용되는 것을 재현한다.
//
// 참조: pilot/pkg/networking/core/cluster_traffic_policy.go:43-69
//
//	cb.applyConnectionPool(...)
//	applyOutlierDetection(...)
type CircuitBreaker struct {
	pool            *ConnectionPool
	outlierDetector *OutlierDetector
	state           CircuitState
	endpoints       []*Endpoint

	// 통계
	totalRequests  int64
	totalSuccess   int64
	totalFailures  int64
	totalRejected  int64

	mu sync.Mutex
}

// NewCircuitBreaker는 서킷 브레이커를 생성한다.
func NewCircuitBreaker(poolSettings ConnectionPoolSettings, odConfig OutlierDetectionConfig, endpoints []*Endpoint) *CircuitBreaker {
	return &CircuitBreaker{
		pool:            NewConnectionPool(poolSettings),
		outlierDetector: NewOutlierDetector(odConfig, endpoints),
		state:           StateClosed,
		endpoints:       endpoints,
	}
}

// selectEndpoint는 건강한 엔드포인트 중 하나를 라운드로빈으로 선택한다.
var rrCounter uint64

func (cb *CircuitBreaker) selectEndpoint() *Endpoint {
	healthy := cb.outlierDetector.GetHealthyEndpoints()
	if len(healthy) == 0 {
		return nil
	}
	idx := atomic.AddUint64(&rrCounter, 1) % uint64(len(healthy))
	return healthy[idx]
}

// SendRequest는 서킷 브레이커를 통해 요청을 전송한다.
// 전체 흐름: 커넥션 풀 체크 → 엔드포인트 선택 → 요청 처리 → 결과 반환
func (cb *CircuitBreaker) SendRequest(requestID int) (status int, endpoint string, detail string) {
	atomic.AddInt64(&cb.totalRequests, 1)

	// 1단계: 서킷 상태 확인
	cb.mu.Lock()
	currentState := cb.state
	cb.mu.Unlock()

	if currentState == StateOpen {
		atomic.AddInt64(&cb.totalRejected, 1)
		return 503, "N/A", "서킷 OPEN — 요청 즉시 거부"
	}

	// 2단계: 커넥션 풀 확인
	acquired, overflow := cb.pool.TryAcquire()
	if overflow {
		atomic.AddInt64(&cb.totalRejected, 1)
		return 503, "N/A", "커넥션 풀 오버플로우 — 대기열 가득참"
	}

	// 커넥션을 직접 획득하지 못했지만 대기열에 들어간 경우
	// 실제로는 대기하지만 시뮬레이션에서는 잠시 후 획득 시도
	if !acquired {
		time.Sleep(5 * time.Millisecond) // 대기 시뮬레이션
		cb.pool.ReleasePending()

		// 다시 획득 시도
		acquired, overflow = cb.pool.TryAcquire()
		if !acquired || overflow {
			atomic.AddInt64(&cb.totalRejected, 1)
			return 503, "N/A", "커넥션 풀 대기 후에도 획득 실패"
		}
	}
	defer cb.pool.Release()

	// 3단계: 엔드포인트 선택
	ep := cb.selectEndpoint()
	if ep == nil {
		atomic.AddInt64(&cb.totalRejected, 1)
		return 503, "N/A", "건강한 엔드포인트 없음 (모두 퇴출됨)"
	}

	// 4단계: 요청 처리
	// 실제 네트워크 지연 시뮬레이션
	time.Sleep(time.Duration(2+rand.Intn(8)) * time.Millisecond)
	statusCode := ep.HandleRequest()

	if statusCode == 200 {
		atomic.AddInt64(&cb.totalSuccess, 1)
		return 200, ep.Name, "성공"
	}

	atomic.AddInt64(&cb.totalFailures, 1)
	return statusCode, ep.Name, fmt.Sprintf("5xx 에러 (연속 %d회)", ep.Consecutive5xx)
}

// ============================================================================
// 7. 시뮬레이션 실행
// ============================================================================

func printHeader(title string) {
	sep := strings.Repeat("=", 80)
	fmt.Println()
	fmt.Println(sep)
	fmt.Printf("  %s\n", title)
	fmt.Println(sep)
}

func printSubHeader(title string) {
	fmt.Printf("\n--- %s ---\n", title)
}

func printEndpointStatus(endpoints []*Endpoint) {
	fmt.Println()
	fmt.Printf("  %-12s %-10s %-8s %-10s %-10s %-8s\n",
		"엔드포인트", "상태", "연속5xx", "총요청", "총실패", "퇴출횟수")
	fmt.Printf("  %s\n", strings.Repeat("-", 62))
	for _, ep := range endpoints {
		ep.mu.Lock()
		status := "정상"
		if ep.Ejected {
			status = "퇴출됨"
		}
		fmt.Printf("  %-12s %-10s %-8d %-10d %-10d %-8d\n",
			ep.Name, status, ep.Consecutive5xx, ep.TotalRequests, ep.TotalFailures, ep.EjectionCount)
		ep.mu.Unlock()
	}
}

func main() {
	rand.Seed(time.Now().UnixNano())

	printHeader("Istio 서킷 브레이커 & 아웃라이어 디텍션 PoC")
	fmt.Println()
	fmt.Println("  이 PoC는 Istio DestinationRule의 TrafficPolicy를 시뮬레이션합니다.")
	fmt.Println("  Pilot이 Envoy에 전달하는 CircuitBreakers + OutlierDetection 동작을 재현합니다.")
	fmt.Println()
	fmt.Println("  참조: pilot/pkg/networking/core/cluster_traffic_policy.go")
	fmt.Println("        - applyConnectionPool(): 커넥션 풀 제한 적용")
	fmt.Println("        - applyOutlierDetection(): 아웃라이어 디텍션 적용")
	fmt.Println("        - getDefaultCircuitBreakerThresholds(): 기본 임계값 (MaxUint32)")

	// ========================================================================
	// 시나리오 1: 커넥션 풀 오버플로우
	// ========================================================================
	printHeader("시나리오 1: 커넥션 풀 오버플로우 시뮬레이션")
	fmt.Println()
	fmt.Println("  DestinationRule 설정:")
	fmt.Println("    trafficPolicy:")
	fmt.Println("      connectionPool:")
	fmt.Println("        tcp:")
	fmt.Println("          maxConnections: 3")
	fmt.Println("        http:")
	fmt.Println("          http1MaxPendingRequests: 2")
	fmt.Println("          maxRequestsPerConnection: 5")

	poolSettings := ConnectionPoolSettings{
		MaxConnections:     3,
		MaxPendingRequests: 2,
		MaxRequestsPerConn: 5,
	}
	pool := NewConnectionPool(poolSettings)

	printSubHeader("동시 요청 10개 전송 (maxConnections=3, maxPendingRequests=2)")

	var wg sync.WaitGroup
	results := make([]string, 10)

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			acquired, overflow := pool.TryAcquire()
			if overflow {
				results[id] = fmt.Sprintf("  요청 #%02d: [거부] 커넥션 풀 오버플로우 (503)", id+1)
			} else if acquired {
				results[id] = fmt.Sprintf("  요청 #%02d: [성공] 커넥션 획득", id+1)
				time.Sleep(50 * time.Millisecond) // 요청 처리 시뮬레이션
				pool.Release()
			} else {
				results[id] = fmt.Sprintf("  요청 #%02d: [대기] 대기열 진입", id+1)
				time.Sleep(20 * time.Millisecond)
				pool.ReleasePending()
			}
		}(i)
	}
	wg.Wait()

	fmt.Println()
	for _, r := range results {
		fmt.Println(r)
	}

	active, pending, overflows, processed := pool.Stats()
	fmt.Println()
	fmt.Printf("  커넥션 풀 통계:\n")
	fmt.Printf("    활성 커넥션: %d/%d\n", active, poolSettings.MaxConnections)
	fmt.Printf("    대기 요청:   %d/%d\n", pending, poolSettings.MaxPendingRequests)
	fmt.Printf("    오버플로우:  %d건 (503 반환)\n", overflows)
	fmt.Printf("    처리 완료:   %d건\n", processed)
	fmt.Println()
	fmt.Println("  --> Istio에서 MaxConnections + MaxPendingRequests를 초과하면")
	fmt.Println("      Envoy가 즉시 503을 반환합니다. (upstream_cx_overflow 메트릭 증가)")

	// ========================================================================
	// 시나리오 2: 아웃라이어 디텍션 — 퇴출과 복구
	// ========================================================================
	printHeader("시나리오 2: 아웃라이어 디텍션 — 엔드포인트 퇴출과 복구")
	fmt.Println()
	fmt.Println("  DestinationRule 설정:")
	fmt.Println("    trafficPolicy:")
	fmt.Println("      outlierDetection:")
	fmt.Println("        consecutive5xxErrors: 3")
	fmt.Println("        interval: 500ms        (실제: 10s)")
	fmt.Println("        baseEjectionTime: 1s   (실제: 30s)")
	fmt.Println("        maxEjectionPercent: 50")

	endpoints := []*Endpoint{
		{Name: "pod-1", FailureRate: 0.0},  // 항상 성공
		{Name: "pod-2", FailureRate: 0.0},  // 항상 성공
		{Name: "pod-3", FailureRate: 1.0},  // 항상 실패 (불량 엔드포인트)
		{Name: "pod-4", FailureRate: 0.0},  // 항상 성공
	}

	odConfig := OutlierDetectionConfig{
		Consecutive5xxErrors: 3,
		Interval:             500 * time.Millisecond, // 시뮬레이션용 (실제 기본값: 10s)
		BaseEjectionTime:     1 * time.Second,        // 시뮬레이션용 (실제 기본값: 30s)
		MaxEjectionPercent:   50,
	}

	detector := NewOutlierDetector(odConfig, endpoints)

	printSubHeader("Phase 1: 불량 엔드포인트(pod-3)에 요청 전송")
	fmt.Println()

	// pod-3에 직접 요청 전송하여 연속 5xx 발생시키기
	for i := 0; i < 5; i++ {
		status := endpoints[2].HandleRequest()
		fmt.Printf("  요청 #%d → pod-3: %d (연속 5xx: %d)\n", i+1, status, endpoints[2].Consecutive5xx)
	}

	printSubHeader("Phase 2: 아웃라이어 디텍션 실행 (interval 마다)")
	fmt.Println()
	fmt.Println("  [아웃라이어 디텍션 실행]")
	detector.Check()

	printEndpointStatus(endpoints)

	printSubHeader("Phase 3: 퇴출 중 트래픽 분산 확인")
	fmt.Println()

	healthyEps := detector.GetHealthyEndpoints()
	fmt.Printf("  건강한 엔드포인트: ")
	for i, ep := range healthyEps {
		if i > 0 {
			fmt.Print(", ")
		}
		fmt.Print(ep.Name)
	}
	fmt.Println()
	fmt.Printf("  → 퇴출된 pod-3 대신 나머지 %d개 엔드포인트로 트래픽 분산\n", len(healthyEps))

	printSubHeader("Phase 4: 퇴출 시간 경과 후 복구 (baseEjectionTime * ejectionCount)")
	fmt.Println()
	fmt.Println("  1초 대기 중 (baseEjectionTime = 1s)...")
	time.Sleep(1100 * time.Millisecond) // 약간 여유를 둠

	fmt.Println()
	fmt.Println("  [아웃라이어 디텍션 재실행]")
	detector.Check()

	printEndpointStatus(endpoints)

	// ========================================================================
	// 시나리오 3: maxEjectionPercent 보호 메커니즘
	// ========================================================================
	printHeader("시나리오 3: 최대 퇴출 비율(maxEjectionPercent) 보호")
	fmt.Println()
	fmt.Println("  maxEjectionPercent=50 → 4개 중 최대 2개까지만 퇴출 가능")

	endpoints2 := []*Endpoint{
		{Name: "ep-A", FailureRate: 1.0}, // 모두 실패
		{Name: "ep-B", FailureRate: 1.0}, // 모두 실패
		{Name: "ep-C", FailureRate: 1.0}, // 모두 실패
		{Name: "ep-D", FailureRate: 0.0}, // 정상
	}

	detector2 := NewOutlierDetector(OutlierDetectionConfig{
		Consecutive5xxErrors: 2,
		Interval:             200 * time.Millisecond,
		BaseEjectionTime:     5 * time.Second,
		MaxEjectionPercent:   50, // 50% = 4개 중 최대 2개
	}, endpoints2)

	// 모든 불량 엔드포인트에 실패 유발
	fmt.Println()
	for _, ep := range endpoints2[:3] {
		for i := 0; i < 3; i++ {
			ep.HandleRequest()
		}
		fmt.Printf("  %s: 연속 5xx %d회\n", ep.Name, ep.Consecutive5xx)
	}

	fmt.Println()
	fmt.Println("  [아웃라이어 디텍션 실행]")
	detector2.Check()

	printEndpointStatus(endpoints2)

	healthy2 := detector2.GetHealthyEndpoints()
	fmt.Println()
	fmt.Printf("  결과: 3개가 실패했지만 maxEjectionPercent=50%%이므로 최대 2개만 퇴출\n")
	fmt.Printf("        건강한 엔드포인트 %d개 유지 (최소 가용성 보장)\n", len(healthy2))

	// ========================================================================
	// 시나리오 4: 통합 시뮬레이션 (커넥션 풀 + 아웃라이어 디텍션 + 상태 머신)
	// ========================================================================
	printHeader("시나리오 4: 통합 시뮬레이션 (서킷 브레이커 전체 흐름)")
	fmt.Println()
	fmt.Println("  DestinationRule 전체 설정:")
	fmt.Println("    trafficPolicy:")
	fmt.Println("      connectionPool:")
	fmt.Println("        tcp: { maxConnections: 5 }")
	fmt.Println("        http: { http1MaxPendingRequests: 3 }")
	fmt.Println("      outlierDetection:")
	fmt.Println("        consecutive5xxErrors: 3")
	fmt.Println("        interval: 300ms")
	fmt.Println("        baseEjectionTime: 800ms")
	fmt.Println("        maxEjectionPercent: 60")

	endpoints3 := []*Endpoint{
		{Name: "svc-1", FailureRate: 0.05}, // 5% 실패
		{Name: "svc-2", FailureRate: 0.05}, // 5% 실패
		{Name: "svc-3", FailureRate: 0.9},  // 90% 실패 (불안정)
		{Name: "svc-4", FailureRate: 0.05}, // 5% 실패
		{Name: "svc-5", FailureRate: 0.85}, // 85% 실패 (불안정)
	}

	cb := NewCircuitBreaker(
		ConnectionPoolSettings{
			MaxConnections:     5,
			MaxPendingRequests: 3,
			MaxRequestsPerConn: 10,
		},
		OutlierDetectionConfig{
			Consecutive5xxErrors: 3,
			Interval:             300 * time.Millisecond,
			BaseEjectionTime:     800 * time.Millisecond,
			MaxEjectionPercent:   60,
		},
		endpoints3,
	)

	// 아웃라이어 디텍션 백그라운드 실행
	stopCh := make(chan struct{})
	go func() {
		ticker := time.NewTicker(300 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				cb.outlierDetector.Check()
			case <-stopCh:
				return
			}
		}
	}()

	// 요청 전송 (3단계로 나누어 진행)
	phases := []struct {
		name     string
		count    int
		interval time.Duration
	}{
		{"초기 요청 (불량 감지 전)", 20, 20 * time.Millisecond},
		{"불량 감지 후 요청", 20, 20 * time.Millisecond},
		{"복구 후 요청", 15, 20 * time.Millisecond},
	}

	for phaseIdx, phase := range phases {
		printSubHeader(fmt.Sprintf("Phase %d: %s", phaseIdx+1, phase.name))
		fmt.Println()

		if phaseIdx == 2 {
			// 복구 대기
			fmt.Println("  (800ms 대기 — 퇴출된 엔드포인트 복구 대기)")
			time.Sleep(900 * time.Millisecond)
		}

		successCount := 0
		failCount := 0
		rejectCount := 0

		for i := 0; i < phase.count; i++ {
			status, epName, detail := cb.SendRequest(i)
			switch {
			case status == 200:
				successCount++
			case status == 503 && (detail == "커넥션 풀 오버플로우 — 대기열 가득참" ||
				detail == "서킷 OPEN — 요청 즉시 거부" ||
				detail == "건강한 엔드포인트 없음 (모두 퇴출됨)" ||
				detail == "커넥션 풀 대기 후에도 획득 실패"):
				rejectCount++
			default:
				failCount++
			}

			if i < 5 || i == phase.count-1 { // 처음 5개와 마지막만 출력
				fmt.Printf("  요청 #%02d → %-6s [%d] %s\n", i+1, epName, status, detail)
			} else if i == 5 {
				fmt.Printf("  ... (중간 요청 생략) ...\n")
			}
			time.Sleep(phase.interval)
		}

		fmt.Println()
		fmt.Printf("  Phase %d 결과: 성공=%d, 5xx실패=%d, 거부(503)=%d\n",
			phaseIdx+1, successCount, failCount, rejectCount)

		printEndpointStatus(endpoints3)

		if phaseIdx < len(phases)-1 {
			time.Sleep(400 * time.Millisecond) // 다음 phase 전 대기
		}
	}

	close(stopCh)

	// ========================================================================
	// 상태 머신 다이어그램
	// ========================================================================
	printHeader("서킷 브레이커 상태 머신")
	fmt.Println()
	fmt.Println("         요청 성공                     연속 에러 ≥ 임계값")
	fmt.Println("       ┌──────────┐                 ┌──────────────────┐")
	fmt.Println("       │          │                 │                  │")
	fmt.Println("       v          │                 │                  v")
	fmt.Println("  ┌─────────┐     │            ┌─────────┐      ┌─────────┐")
	fmt.Println("  │ CLOSED  │─────┘            │  OPEN   │      │  OPEN   │")
	fmt.Println("  │ (정상)  │───────────────── │ (차단)  │──────│ (차단)  │")
	fmt.Println("  └─────────┘  연속 에러 ≥ N   └─────────┘      └─────────┘")
	fmt.Println("       ^                            │                  │")
	fmt.Println("       │        baseEjectionTime    │                  │")
	fmt.Println("       │          경과              v                  │")
	fmt.Println("       │                      ┌───────────┐            │")
	fmt.Println("       │     시험 요청 성공    │ HALF-OPEN │            │")
	fmt.Println("       └──────────────────────│ (반개방)   │────────────┘")
	fmt.Println("                              └───────────┘  시험 요청 실패")
	fmt.Println()
	fmt.Println("  Istio/Envoy 동작:")
	fmt.Println("  - CLOSED: 모든 요청이 업스트림으로 전달됨")
	fmt.Println("  - OPEN: 엔드포인트가 퇴출되어 트래픽에서 제외됨")
	fmt.Println("  - HALF-OPEN: baseEjectionTime 경과 후 복구, 재실패 시 재퇴출")
	fmt.Println("               (퇴출 시간 = baseEjectionTime * ejectionCount 로 점진적 증가)")

	// ========================================================================
	// 요약
	// ========================================================================
	printHeader("요약: Istio 서킷 브레이커 동작 원리")
	fmt.Println()
	fmt.Println("  1. 커넥션 풀 (ConnectionPoolSettings)")
	fmt.Println("     - MaxConnections: TCP 연결 수 제한 → 초과 시 대기열로")
	fmt.Println("     - MaxPendingRequests: 대기열 크기 제한 → 초과 시 503 즉시 반환")
	fmt.Println("     - Istio 기본값: MaxUint32 (실질적으로 무제한)")
	fmt.Println("       → getDefaultCircuitBreakerThresholds()에서 설정")
	fmt.Println()
	fmt.Println("  2. 아웃라이어 디텍션 (OutlierDetection)")
	fmt.Println("     - Consecutive5xxErrors: 연속 5xx 에러 임계값")
	fmt.Println("     - Interval: 검사 주기")
	fmt.Println("     - BaseEjectionTime: 기본 퇴출 시간 (퇴출 횟수에 비례하여 증가)")
	fmt.Println("     - MaxEjectionPercent: 최대 퇴출 비율 (최소 가용성 보장)")
	fmt.Println()
	fmt.Println("  3. Pilot → Envoy 변환 흐름")
	fmt.Println("     DestinationRule YAML")
	fmt.Println("       → Pilot: applyTrafficPolicy()")
	fmt.Println("         → applyConnectionPool() → Envoy CircuitBreakers.Thresholds")
	fmt.Println("         → applyOutlierDetection() → Envoy OutlierDetection")
	fmt.Println("       → xDS Push → Envoy 클러스터 설정 적용")
	fmt.Println()
}
