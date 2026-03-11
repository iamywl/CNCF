package main

import (
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// gRPC ORCA 로드 리포팅 시뮬레이션
// =============================================================================
//
// ORCA (Open Request Cost Aggregation)는 백엔드 서버가 자신의 부하 상태를
// 클라이언트(로드밸런서)에 보고하는 표준이다.
//
// 두 가지 리포팅 모드:
//   - Per-Query: 각 RPC 응답의 trailing metadata에 부하 포함
//   - Out-of-Band (OOB): 별도 스트림으로 주기적 부하 보고
//
// 실제 코드 참조:
//   - orca/: ORCA 서비스 구현
//   - balancer/weightedroundrobin/: 가중 라운드로빈 (ORCA 활용)
// =============================================================================

// --- ORCA Load Report ---

type OrcaLoadReport struct {
	CPUUtilization float64            `json:"cpu_utilization"`  // 0.0 ~ 1.0
	MemUtilization float64            `json:"mem_utilization"`  // 0.0 ~ 1.0
	ApplicationUtilization float64    `json:"app_utilization"`  // 앱 정의 부하
	QPS            float64            `json:"qps"`
	EPS            float64            `json:"eps"`              // errors per second
	RequestCost    map[string]float64 `json:"request_cost"`     // per-query 비용
	Utilization    map[string]float64 `json:"utilization"`      // 커스텀 메트릭
}

func (r OrcaLoadReport) String() string {
	return fmt.Sprintf("cpu=%.2f mem=%.2f app=%.2f qps=%.0f eps=%.0f",
		r.CPUUtilization, r.MemUtilization, r.ApplicationUtilization, r.QPS, r.EPS)
}

// --- Backend Server ---

type BackendServer struct {
	Address string
	Weight  float64
	mu      sync.Mutex
	report  OrcaLoadReport
	r       *rand.Rand
}

func NewBackendServer(addr string, r *rand.Rand) *BackendServer {
	return &BackendServer{
		Address: addr,
		Weight:  1.0,
		r:       r,
		report: OrcaLoadReport{
			RequestCost: make(map[string]float64),
			Utilization: make(map[string]float64),
		},
	}
}

// SimulateLoad는 서버 부하를 시뮬레이션한다.
func (s *BackendServer) SimulateLoad() {
	s.mu.Lock()
	defer s.mu.Unlock()

	// CPU/메모리는 점진적으로 변화
	s.report.CPUUtilization = clamp(s.report.CPUUtilization + (s.r.Float64()-0.5)*0.2)
	s.report.MemUtilization = clamp(s.report.MemUtilization + (s.r.Float64()-0.5)*0.1)
	s.report.ApplicationUtilization = clamp(s.report.CPUUtilization*0.6 + s.report.MemUtilization*0.4)
	s.report.QPS = float64(100 + s.r.Intn(500))
	s.report.EPS = float64(s.r.Intn(10))
	s.report.Utilization["gpu"] = clamp(s.r.Float64())
	s.report.Utilization["disk_io"] = clamp(s.r.Float64() * 0.5)
}

func (s *BackendServer) GetReport() OrcaLoadReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	// deep copy
	report := s.report
	report.RequestCost = make(map[string]float64)
	report.Utilization = make(map[string]float64)
	for k, v := range s.report.RequestCost {
		report.RequestCost[k] = v
	}
	for k, v := range s.report.Utilization {
		report.Utilization[k] = v
	}
	return report
}

// PerQueryReport는 RPC 처리 후 per-query 비용을 포함한 리포트를 반환한다.
func (s *BackendServer) PerQueryReport(method string) OrcaLoadReport {
	report := s.GetReport()
	// RPC별 비용 추가
	s.mu.Lock()
	cost := 0.01 + s.r.Float64()*0.05
	s.mu.Unlock()
	report.RequestCost[method] = cost
	return report
}

func clamp(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// --- OOB (Out-of-Band) Reporter ---

// OOBReporter는 별도 스트림으로 주기적으로 부하를 보고한다.
type OOBReporter struct {
	server   *BackendServer
	interval time.Duration
	stopCh   chan struct{}
	reports  []OrcaLoadReport
	mu       sync.Mutex
}

func NewOOBReporter(server *BackendServer, interval time.Duration) *OOBReporter {
	return &OOBReporter{
		server:   server,
		interval: interval,
		stopCh:   make(chan struct{}),
	}
}

func (r *OOBReporter) Start() {
	go func() {
		ticker := time.NewTicker(r.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				r.server.SimulateLoad()
				report := r.server.GetReport()
				r.mu.Lock()
				r.reports = append(r.reports, report)
				r.mu.Unlock()
			case <-r.stopCh:
				return
			}
		}
	}()
}

func (r *OOBReporter) Stop() {
	close(r.stopCh)
}

func (r *OOBReporter) GetReports() []OrcaLoadReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]OrcaLoadReport{}, r.reports...)
}

// --- Weighted Round Robin LB (ORCA 활용) ---

type WeightedRRBalancer struct {
	servers   []*BackendServer
	weights   []float64
	currentIdx int
}

func NewWeightedRRBalancer(servers []*BackendServer) *WeightedRRBalancer {
	weights := make([]float64, len(servers))
	for i := range weights {
		weights[i] = 1.0
	}
	return &WeightedRRBalancer{servers: servers, weights: weights}
}

// UpdateWeights는 ORCA 리포트를 기반으로 가중치를 업데이트한다.
func (b *WeightedRRBalancer) UpdateWeights() {
	for i, server := range b.servers {
		report := server.GetReport()
		// 가중치 = 1 / (cpu_util * 0.7 + mem_util * 0.3 + eps/qps)
		utilization := report.CPUUtilization*0.7 + report.MemUtilization*0.3
		errorRate := 0.0
		if report.QPS > 0 {
			errorRate = report.EPS / report.QPS
		}
		score := utilization + errorRate
		if score < 0.01 {
			score = 0.01
		}
		b.weights[i] = 1.0 / score
	}

	// 정규화
	total := 0.0
	for _, w := range b.weights {
		total += w
	}
	for i := range b.weights {
		b.weights[i] /= total
	}
}

// Pick은 가중 라운드로빈으로 서버를 선택한다.
func (b *WeightedRRBalancer) Pick() *BackendServer {
	// 가중치 기반 선택 (누적 확률)
	r := rand.Float64()
	cumulative := 0.0
	for i, w := range b.weights {
		cumulative += w
		if r <= cumulative {
			return b.servers[i]
		}
	}
	return b.servers[len(b.servers)-1]
}

func main() {
	fmt.Println("=== gRPC ORCA 로드 리포팅 시뮬레이션 ===")
	fmt.Println()

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	// --- 백엔드 서버 생성 ---
	fmt.Println("[1] 백엔드 서버 초기화")
	fmt.Println(strings.Repeat("-", 60))

	servers := []*BackendServer{
		NewBackendServer("10.0.1.1:50051", rng),
		NewBackendServer("10.0.1.2:50051", rng),
		NewBackendServer("10.0.1.3:50051", rng),
		NewBackendServer("10.0.1.4:50051", rng),
	}

	for _, s := range servers {
		fmt.Printf("  Server: %s (initial weight: %.2f)\n", s.Address, s.Weight)
	}
	fmt.Println()

	// --- Per-Query 리포팅 ---
	fmt.Println("[2] Per-Query 리포팅 (trailing metadata)")
	fmt.Println(strings.Repeat("-", 60))

	methods := []string{"/myapp.UserService/GetUser", "/myapp.OrderService/CreateOrder", "/myapp.SearchService/Search"}

	for i := 0; i < 10; i++ {
		server := servers[i%len(servers)]
		server.SimulateLoad()
		method := methods[i%len(methods)]
		report := server.PerQueryReport(method)
		fmt.Printf("  [%s] %s: %s cost=%.4f\n",
			server.Address, method, report, report.RequestCost[method])
	}
	fmt.Println()

	// --- OOB 리포팅 ---
	fmt.Println("[3] Out-of-Band 리포팅 (별도 스트림)")
	fmt.Println(strings.Repeat("-", 60))

	reporters := make([]*OOBReporter, len(servers))
	for i, server := range servers {
		reporters[i] = NewOOBReporter(server, 50*time.Millisecond)
		reporters[i].Start()
	}

	time.Sleep(300 * time.Millisecond) // 리포트 수집 대기

	for _, r := range reporters {
		r.Stop()
	}

	for i, reporter := range reporters {
		reports := reporter.GetReports()
		fmt.Printf("  Server %s: %d OOB reports\n", servers[i].Address, len(reports))
		if len(reports) > 0 {
			last := reports[len(reports)-1]
			fmt.Printf("    Latest: %s\n", last)
		}
	}
	fmt.Println()

	// --- Weighted Round Robin ---
	fmt.Println("[4] Weighted Round Robin (ORCA 기반)")
	fmt.Println(strings.Repeat("-", 60))

	balancer := NewWeightedRRBalancer(servers)
	balancer.UpdateWeights()

	fmt.Println("  가중치 업데이트 후:")
	for i, server := range servers {
		report := server.GetReport()
		fmt.Printf("    %s: weight=%.4f (cpu=%.2f, mem=%.2f)\n",
			server.Address, balancer.weights[i], report.CPUUtilization, report.MemUtilization)
	}
	fmt.Println()

	// 트래픽 분배 시뮬레이션
	fmt.Println("[5] 트래픽 분배 시뮬레이션 (1000 요청)")
	fmt.Println(strings.Repeat("-", 60))

	distribution := make(map[string]int)
	for i := 0; i < 1000; i++ {
		server := balancer.Pick()
		distribution[server.Address]++

		// 주기적으로 가중치 업데이트
		if i%100 == 99 {
			for _, s := range servers {
				s.SimulateLoad()
			}
			balancer.UpdateWeights()
		}
	}

	for _, server := range servers {
		count := distribution[server.Address]
		bar := strings.Repeat("#", int(math.Round(float64(count)/20)))
		fmt.Printf("  %s: %4d (%.1f%%) %s\n",
			server.Address, count, float64(count)/10, bar)
	}
	fmt.Println()

	// --- 최종 가중치 ---
	fmt.Println("[6] 최종 가중치")
	fmt.Println(strings.Repeat("-", 60))
	balancer.UpdateWeights()
	for i, server := range servers {
		report := server.GetReport()
		fmt.Printf("  %s: weight=%.4f\n", server.Address, balancer.weights[i])
		fmt.Printf("    cpu=%.2f mem=%.2f app=%.2f qps=%.0f eps=%.0f\n",
			report.CPUUtilization, report.MemUtilization,
			report.ApplicationUtilization, report.QPS, report.EPS)
		for k, v := range report.Utilization {
			fmt.Printf("    %s=%.2f\n", k, v)
		}
	}
	fmt.Println()

	fmt.Println("=== 시뮬레이션 완료 ===")
}
