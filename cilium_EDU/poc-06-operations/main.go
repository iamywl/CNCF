package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Cilium 운영 시뮬레이션 — 설정 로드, 헬스 체크, 메트릭 수집
// =============================================================================
//
// 이 PoC는 Cilium 에이전트의 운영 관련 핵심 메커니즘을 시뮬레이션한다:
//   1. YAML 설정 파일 로딩 및 검증 (DaemonConfig)
//   2. 헬스 체크 엔드포인트 (cilium-health)
//   3. Prometheus 메트릭 수집 패턴
//
// 실제 소스 코드 참조:
//
// 1. 설정 관리
//   - pkg/option/config.go            → DaemonConfig 구조체 (1400+ 필드)
//   - pkg/option/config.go:2186       → Validate() — 설정값 검증
//   - pkg/option/config.go:3318       → ValidateUnchanged() — SHA256 체크섬 비교
//   - pkg/option/config.go:1143       → HiveConfig{StartTimeout, StopTimeout, LogThreshold}
//   - pkg/option/constants.go         → 상수 정의 (기본값, 열거형)
//
// 2. 헬스 체크
//   - pkg/health/health_manager.go    → CiliumHealthManager, Cell, controllerInterval(60s)
//   - pkg/health/server/server.go     → Server 구조체, Config
//   - pkg/health/server/prober.go     → prober, 주기적 ICMP/HTTP 프로빙
//   - pkg/health/server/status.go     → GetStatusHandler, PutStatusProbeHandler
//
// 3. 메트릭 수집
//   - pkg/maps/metricsmap/metricsmap.go → metricsmapCollector, Prometheus Collector
//   - pkg/metrics/                      → 메트릭 레지스트리, 네임스페이스 관리
// =============================================================================

// ─────────────────────────────────────────────────────────────────────────────
// 1. 설정 로드 및 검증 (pkg/option/config.go)
// ─────────────────────────────────────────────────────────────────────────────

// DaemonConfig는 Cilium 에이전트의 설정이다.
// 실제 코드: pkg/option/config.go:1187 — DaemonConfig 구조체
// 실제로는 1400개 이상의 필드를 가지며, viper를 통해 YAML/환경변수/CLI에서 로드된다.
type DaemonConfig struct {
	// 기본 설정
	Debug       bool   `json:"debug"`
	LogLevel    string `json:"log-level"`
	ClusterName string `json:"cluster-name"`
	ClusterID   int    `json:"cluster-id"`

	// 네트워킹
	TunnelMode   string `json:"tunnel"`           // vxlan, geneve, disabled
	RoutingMode  string `json:"routing-mode"`     // tunnel, native
	EnableIPv4   bool   `json:"enable-ipv4"`
	EnableIPv6   bool   `json:"enable-ipv6"`
	IPAM         string `json:"ipam"`             // kubernetes, cluster-pool, ...
	MTU          int    `json:"mtu"`

	// BPF
	BPFRoot            string `json:"bpf-root"`
	CTMapEntriesTCP    int    `json:"bpf-ct-global-tcp-max"`
	CTMapEntriesAny    int    `json:"bpf-ct-global-any-max"`
	PolicyMapMaxEntries int   `json:"bpf-policy-map-max"`

	// 보안
	EnablePolicy   string `json:"enable-policy"`   // default, always, never
	EnableL7Proxy  bool   `json:"enable-l7-proxy"`
	EncryptNode    bool   `json:"encrypt-node"`

	// Hive (pkg/option/config.go:1143)
	HiveStartTimeout time.Duration `json:"hive-start-timeout"`
	HiveStopTimeout  time.Duration `json:"hive-stop-timeout"`

	// 헬스 체크
	ClusterHealthPort int `json:"cluster-health-port"`

	// 내부 상태 (설정 변경 감지용)
	shaSum [32]byte
}

// DefaultConfig는 기본 설정을 반환한다.
// 실제 코드: pkg/defaults/ 패키지에서 각종 기본값이 정의됨
func DefaultConfig() *DaemonConfig {
	return &DaemonConfig{
		Debug:               false,
		LogLevel:            "info",
		ClusterName:         "default",
		ClusterID:           0,
		TunnelMode:          "vxlan",
		RoutingMode:         "tunnel",
		EnableIPv4:          true,
		EnableIPv6:          false,
		IPAM:                "cluster-pool",
		MTU:                 0, // 자동 감지
		BPFRoot:             "/sys/fs/bpf",
		CTMapEntriesTCP:     524288,
		CTMapEntriesAny:     262144,
		PolicyMapMaxEntries: 16384,
		EnablePolicy:        "default",
		EnableL7Proxy:       true,
		EncryptNode:         false,
		HiveStartTimeout:    5 * time.Minute,
		HiveStopTimeout:     time.Minute,
		ClusterHealthPort:   4240,
	}
}

// Validate는 설정값을 검증한다.
// 실제 코드: pkg/option/config.go:2186 — DaemonConfig.Validate()
// IPv6 CIDR 파싱, 터널 모드 검증, IPAM 호환성 등을 확인한다.
func (c *DaemonConfig) Validate() []error {
	var errs []error

	// 클러스터 이름 검증
	if c.ClusterName == "" {
		errs = append(errs, fmt.Errorf("cluster-name은 비어있을 수 없음"))
	}

	// 클러스터 ID 범위 검증 (0-255)
	if c.ClusterID < 0 || c.ClusterID > 255 {
		errs = append(errs, fmt.Errorf("cluster-id는 0-255 범위여야 함: %d", c.ClusterID))
	}

	// 터널 모드 검증
	validTunnelModes := map[string]bool{"vxlan": true, "geneve": true, "disabled": true}
	if !validTunnelModes[c.TunnelMode] {
		errs = append(errs, fmt.Errorf("잘못된 tunnel 모드: %s (허용: vxlan, geneve, disabled)", c.TunnelMode))
	}

	// 라우팅 모드 검증
	validRoutingModes := map[string]bool{"tunnel": true, "native": true}
	if !validRoutingModes[c.RoutingMode] {
		errs = append(errs, fmt.Errorf("잘못된 routing-mode: %s (허용: tunnel, native)", c.RoutingMode))
	}

	// 정책 모드 검증
	validPolicyModes := map[string]bool{"default": true, "always": true, "never": true}
	if !validPolicyModes[c.EnablePolicy] {
		errs = append(errs, fmt.Errorf("잘못된 enable-policy: %s (허용: default, always, never)", c.EnablePolicy))
	}

	// BPF 맵 크기 검증
	if c.CTMapEntriesTCP < 1024 {
		errs = append(errs, fmt.Errorf("bpf-ct-global-tcp-max가 너무 작음: %d (최소: 1024)", c.CTMapEntriesTCP))
	}

	// MTU 검증
	if c.MTU != 0 && (c.MTU < 1280 || c.MTU > 9000) {
		errs = append(errs, fmt.Errorf("MTU 범위 오류: %d (허용: 0(자동), 1280-9000)", c.MTU))
	}

	// 라우팅-터널 호환성 검증
	if c.RoutingMode == "native" && c.TunnelMode != "disabled" {
		errs = append(errs, fmt.Errorf("native 라우팅에서는 tunnel=disabled여야 함"))
	}

	return errs
}

// LoadConfigFromYAML은 간단한 YAML 파서로 설정을 로드한다.
// 실제 코드: viper 라이브러리를 통한 YAML/환경변수/CLI 통합 로드
// 여기서는 표준 라이브러리만 사용하므로 간이 파서 구현
func LoadConfigFromYAML(filename string) (*DaemonConfig, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("설정 파일 읽기 실패: %w", err)
	}

	config := DefaultConfig()
	lines := strings.Split(string(data), "\n")

	for lineNo, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		value = strings.Trim(value, "\"'") // 따옴표 제거

		switch key {
		case "debug":
			config.Debug = (value == "true")
		case "log-level":
			config.LogLevel = value
		case "cluster-name":
			config.ClusterName = value
		case "cluster-id":
			fmt.Sscanf(value, "%d", &config.ClusterID)
		case "tunnel":
			config.TunnelMode = value
		case "routing-mode":
			config.RoutingMode = value
		case "enable-ipv4":
			config.EnableIPv4 = (value == "true")
		case "enable-ipv6":
			config.EnableIPv6 = (value == "true")
		case "ipam":
			config.IPAM = value
		case "mtu":
			fmt.Sscanf(value, "%d", &config.MTU)
		case "bpf-ct-global-tcp-max":
			fmt.Sscanf(value, "%d", &config.CTMapEntriesTCP)
		case "bpf-ct-global-any-max":
			fmt.Sscanf(value, "%d", &config.CTMapEntriesAny)
		case "bpf-policy-map-max":
			fmt.Sscanf(value, "%d", &config.PolicyMapMaxEntries)
		case "enable-policy":
			config.EnablePolicy = value
		case "enable-l7-proxy":
			config.EnableL7Proxy = (value == "true")
		case "encrypt-node":
			config.EncryptNode = (value == "true")
		case "cluster-health-port":
			fmt.Sscanf(value, "%d", &config.ClusterHealthPort)
		default:
			fmt.Printf("    [config] 경고: 알 수 없는 설정 키 (줄 %d): %s\n", lineNo+1, key)
		}
	}

	return config, nil
}

// PrintConfig는 설정을 출력한다.
func (c *DaemonConfig) PrintConfig() {
	fmt.Printf("    cluster-name:           %s\n", c.ClusterName)
	fmt.Printf("    cluster-id:             %d\n", c.ClusterID)
	fmt.Printf("    debug:                  %v\n", c.Debug)
	fmt.Printf("    log-level:              %s\n", c.LogLevel)
	fmt.Printf("    tunnel:                 %s\n", c.TunnelMode)
	fmt.Printf("    routing-mode:           %s\n", c.RoutingMode)
	fmt.Printf("    enable-ipv4:            %v\n", c.EnableIPv4)
	fmt.Printf("    enable-ipv6:            %v\n", c.EnableIPv6)
	fmt.Printf("    ipam:                   %s\n", c.IPAM)
	fmt.Printf("    mtu:                    %d (0=auto)\n", c.MTU)
	fmt.Printf("    enable-policy:          %s\n", c.EnablePolicy)
	fmt.Printf("    enable-l7-proxy:        %v\n", c.EnableL7Proxy)
	fmt.Printf("    bpf-ct-global-tcp-max:  %d\n", c.CTMapEntriesTCP)
	fmt.Printf("    cluster-health-port:    %d\n", c.ClusterHealthPort)
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. 헬스 체크 (pkg/health/server/, pkg/health/health_manager.go)
// ─────────────────────────────────────────────────────────────────────────────

// ComponentStatus는 컴포넌트의 상태이다.
type ComponentStatus struct {
	Name    string `json:"name"`
	Status  string `json:"status"`  // ok, warning, error, degraded
	Message string `json:"message"`
	LastOK  time.Time `json:"lastOK"`
}

// NodeHealth는 노드의 헬스 상태이다.
// 실제 코드: api/v1/health/models/NodeStatus
type NodeHealth struct {
	Name        string         `json:"name"`
	PrimaryAddr string         `json:"primary-address"`
	ICMP        *ProbeResult   `json:"icmp"`
	HTTP        *ProbeResult   `json:"http"`
}

// ProbeResult는 프로브 결과이다.
// 실제 코드: api/v1/health/models/ConnectivityStatus
type ProbeResult struct {
	Status  string        `json:"status"` // ok, error
	Latency time.Duration `json:"latency"`
	Error   string        `json:"error,omitempty"`
}

// HealthServer는 cilium-health 서버이다.
// 실제 코드: pkg/health/server/server.go — Server 구조체
// ICMP/HTTP 프로빙을 주기적으로 수행하고 결과를 API로 제공한다.
type HealthServer struct {
	mu          sync.RWMutex
	config      *DaemonConfig
	startTime   time.Time
	components  map[string]*ComponentStatus
	nodes       map[string]*NodeHealth

	// 프로브 설정 (실제 코드: prober.go — probeInterval, probeRateLimiter)
	probeInterval time.Duration
}

func NewHealthServer(config *DaemonConfig) *HealthServer {
	return &HealthServer{
		config:     config,
		startTime:  time.Now(),
		components: make(map[string]*ComponentStatus),
		nodes:      make(map[string]*NodeHealth),
		probeInterval: 60 * time.Second, // controllerInterval (health_manager.go:39)
	}
}

// RegisterComponent는 컴포넌트를 등록한다.
func (s *HealthServer) RegisterComponent(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.components[name] = &ComponentStatus{
		Name:   name,
		Status: "unknown",
	}
}

// UpdateComponentStatus는 컴포넌트 상태를 갱신한다.
func (s *HealthServer) UpdateComponentStatus(name, status, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if comp, ok := s.components[name]; ok {
		comp.Status = status
		comp.Message = message
		if status == "ok" {
			comp.LastOK = time.Now()
		}
	}
}

// ProbeNode는 노드를 프로빙한다.
// 실제 코드: pkg/health/server/prober.go — prober.getNodes(), probeNodes()
// ICMP ping과 HTTP GET을 수행하여 연결성을 확인한다.
func (s *HealthServer) ProbeNode(name, addr string) *NodeHealth {
	health := &NodeHealth{
		Name:        name,
		PrimaryAddr: addr,
	}

	// ICMP 프로브 시뮬레이션
	icmpLatency := time.Duration(rand.Intn(10)+1) * time.Millisecond
	if rand.Float64() < 0.9 { // 90% 성공률
		health.ICMP = &ProbeResult{
			Status:  "ok",
			Latency: icmpLatency,
		}
	} else {
		health.ICMP = &ProbeResult{
			Status: "error",
			Error:  "request timeout",
		}
	}

	// HTTP 프로브 시뮬레이션
	httpLatency := time.Duration(rand.Intn(50)+5) * time.Millisecond
	if rand.Float64() < 0.85 { // 85% 성공률
		health.HTTP = &ProbeResult{
			Status:  "ok",
			Latency: httpLatency,
		}
	} else {
		health.HTTP = &ProbeResult{
			Status: "error",
			Error:  "connection refused",
		}
	}

	s.mu.Lock()
	s.nodes[name] = health
	s.mu.Unlock()

	return health
}

// GetOverallStatus는 전체 상태를 반환한다.
// 실제 코드: pkg/health/server/status.go — GetStatusResponse()
func (s *HealthServer) GetOverallStatus() map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	overall := "ok"
	degradedCount := 0
	errorCount := 0

	for _, comp := range s.components {
		switch comp.Status {
		case "error":
			errorCount++
			overall = "error"
		case "degraded", "warning":
			degradedCount++
			if overall == "ok" {
				overall = "degraded"
			}
		}
	}

	return map[string]interface{}{
		"status":   overall,
		"uptime":   time.Since(s.startTime).String(),
		"components": len(s.components),
		"errors":    errorCount,
		"degraded":  degradedCount,
	}
}

// GetStatusJSON는 JSON 형태로 상태를 반환한다.
func (s *HealthServer) GetStatusJSON() ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	status := struct {
		Overall    string                      `json:"overall"`
		Uptime     string                      `json:"uptime"`
		Components map[string]*ComponentStatus `json:"components"`
		Nodes      map[string]*NodeHealth      `json:"nodes"`
	}{
		Uptime:     time.Since(s.startTime).String(),
		Components: s.components,
		Nodes:      s.nodes,
	}

	// 전체 상태 계산
	status.Overall = "ok"
	for _, comp := range s.components {
		if comp.Status == "error" {
			status.Overall = "error"
			break
		}
		if comp.Status == "degraded" || comp.Status == "warning" {
			status.Overall = "degraded"
		}
	}

	return json.MarshalIndent(status, "", "  ")
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. Prometheus 메트릭 수집 패턴
// ─────────────────────────────────────────────────────────────────────────────

// MetricType은 메트릭의 종류이다.
type MetricType int

const (
	CounterMetric MetricType = iota
	GaugeMetric
	HistogramMetric
)

func (t MetricType) String() string {
	switch t {
	case CounterMetric:
		return "counter"
	case GaugeMetric:
		return "gauge"
	case HistogramMetric:
		return "histogram"
	default:
		return "unknown"
	}
}

// Metric은 하나의 Prometheus 메트릭이다.
type Metric struct {
	Name   string
	Help   string
	Type   MetricType
	Labels map[string]string
	Value  float64
}

// MetricRegistry는 메트릭 레지스트리이다.
// 실제 코드: pkg/metrics/ 패키지의 레지스트리
type MetricRegistry struct {
	mu      sync.RWMutex
	metrics map[string]*Metric
	order   []string // 등록 순서 유지
}

func NewMetricRegistry() *MetricRegistry {
	return &MetricRegistry{
		metrics: make(map[string]*Metric),
	}
}

// Register는 메트릭을 등록한다.
func (r *MetricRegistry) Register(name, help string, mtype MetricType, labels map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := metricKey(name, labels)
	r.metrics[key] = &Metric{
		Name:   name,
		Help:   help,
		Type:   mtype,
		Labels: labels,
		Value:  0,
	}
	r.order = append(r.order, key)
}

// Inc는 카운터를 증가시킨다.
func (r *MetricRegistry) Inc(name string, labels map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := metricKey(name, labels)
	if m, ok := r.metrics[key]; ok {
		m.Value++
	}
}

// Add는 카운터에 값을 더한다.
func (r *MetricRegistry) Add(name string, labels map[string]string, value float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := metricKey(name, labels)
	if m, ok := r.metrics[key]; ok {
		m.Value += value
	}
}

// Set은 게이지를 설정한다.
func (r *MetricRegistry) Set(name string, labels map[string]string, value float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := metricKey(name, labels)
	if m, ok := r.metrics[key]; ok {
		m.Value = value
	}
}

// Exposition은 Prometheus exposition format으로 메트릭을 출력한다.
func (r *MetricRegistry) Exposition() string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var sb strings.Builder
	printed := make(map[string]bool) // HELP/TYPE은 한 번만 출력

	for _, key := range r.order {
		m := r.metrics[key]

		// HELP과 TYPE은 메트릭 이름당 한 번만 출력
		if !printed[m.Name] {
			sb.WriteString(fmt.Sprintf("# HELP %s %s\n", m.Name, m.Help))
			sb.WriteString(fmt.Sprintf("# TYPE %s %s\n", m.Name, m.Type))
			printed[m.Name] = true
		}

		// 레이블 포맷팅
		if len(m.Labels) > 0 {
			labels := make([]string, 0, len(m.Labels))
			keys := make([]string, 0, len(m.Labels))
			for k := range m.Labels {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				labels = append(labels, fmt.Sprintf("%s=\"%s\"", k, m.Labels[k]))
			}
			sb.WriteString(fmt.Sprintf("%s{%s} %.0f\n",
				m.Name, strings.Join(labels, ","), m.Value))
		} else {
			sb.WriteString(fmt.Sprintf("%s %.0f\n", m.Name, m.Value))
		}
	}

	return sb.String()
}

func metricKey(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%s", k, labels[k])
	}
	return name + "{" + strings.Join(parts, ",") + "}"
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. HTTP 서버 (헬스 체크 + 메트릭 엔드포인트)
// ─────────────────────────────────────────────────────────────────────────────

// startHTTPServer는 헬스 체크와 메트릭 엔드포인트를 제공하는 HTTP 서버를 시작한다.
func startHTTPServer(health *HealthServer, metrics *MetricRegistry, port int) *http.Server {
	mux := http.NewServeMux()

	// /healthz — 헬스 체크 엔드포인트
	// 실제 코드: pkg/health/server/status.go — GetStatusHandler
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		status := health.GetOverallStatus()
		w.Header().Set("Content-Type", "application/json")
		if status["status"] != "ok" {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		json.NewEncoder(w).Encode(status)
	})

	// /status — 상세 상태
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		data, _ := health.GetStatusJSON()
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	})

	// /metrics — Prometheus 메트릭
	// 실제 코드: pkg/metrics/ — Prometheus HTTP handler
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		w.Write([]byte(metrics.Exposition()))
	})

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
	}

	go func() {
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			fmt.Printf("    [http] 서버 오류: %v\n", err)
		}
	}()

	return server
}

// ─────────────────────────────────────────────────────────────────────────────
// 5. main — 시나리오 실행
// ─────────────────────────────────────────────────────────────────────────────

func main() {
	rand.Seed(time.Now().UnixNano())

	fmt.Println("================================================================")
	fmt.Println("  Cilium 운영 시뮬레이션")
	fmt.Println("  설정 로드, 헬스 체크, 메트릭 수집")
	fmt.Println("================================================================")

	// ==========================================
	// 시나리오 1: 설정 파일 로드 및 검증
	// ==========================================
	fmt.Println("\n" + strings.Repeat("-", 60))
	fmt.Println("시나리오 1: YAML 설정 파일 로드 및 검증")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println()
	fmt.Println("  Cilium 에이전트는 DaemonConfig로 설정을 관리한다.")
	fmt.Println("  - 설정 소스: YAML 파일, 환경변수(CILIUM_ prefix), CLI 플래그")
	fmt.Println("  - 검증: Validate()로 값 범위, 호환성, 필수 필드 확인")
	fmt.Println("  - 변경 감지: ValidateUnchanged()로 SHA256 체크섬 비교")
	fmt.Println()

	// sample-config.yaml 파일 생성
	sampleConfig := `# Cilium 에이전트 설정 파일 (시뮬레이션)
# 실제 파일: /etc/cilium/cilium-config/config
debug: true
log-level: debug
cluster-name: production-cluster
cluster-id: 1
tunnel: vxlan
routing-mode: tunnel
enable-ipv4: true
enable-ipv6: false
ipam: cluster-pool
mtu: 1500
bpf-ct-global-tcp-max: 524288
bpf-ct-global-any-max: 262144
bpf-policy-map-max: 16384
enable-policy: always
enable-l7-proxy: true
cluster-health-port: 4240
`

	configFile := "sample-config.yaml"
	os.WriteFile(configFile, []byte(sampleConfig), 0644)
	defer os.Remove(configFile)

	fmt.Printf("  [config] 설정 파일 로드: %s\n", configFile)
	config, err := LoadConfigFromYAML(configFile)
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
		return
	}

	fmt.Println("\n  [config] 로드된 설정:")
	config.PrintConfig()

	fmt.Println("\n  [config] 설정 검증:")
	if errs := config.Validate(); len(errs) > 0 {
		for _, e := range errs {
			fmt.Printf("    오류: %v\n", e)
		}
	} else {
		fmt.Println("    모든 검증 통과")
	}

	// 잘못된 설정 검증 테스트
	fmt.Println("\n  [config] 잘못된 설정 검증 테스트:")
	badConfig := DefaultConfig()
	badConfig.ClusterName = ""
	badConfig.ClusterID = 300
	badConfig.TunnelMode = "wireguard"
	badConfig.RoutingMode = "native"
	badConfig.MTU = 500

	if errs := badConfig.Validate(); len(errs) > 0 {
		for _, e := range errs {
			fmt.Printf("    오류: %v\n", e)
		}
	}

	// ==========================================
	// 시나리오 2: 헬스 체크
	// ==========================================
	fmt.Println("\n" + strings.Repeat("-", 60))
	fmt.Println("시나리오 2: 헬스 체크 엔드포인트")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println()
	fmt.Println("  cilium-health는 ICMP/HTTP 프로빙으로 노드 간 연결성을 확인한다.")
	fmt.Println("  - 프로브 간격: 60초 (controllerInterval)")
	fmt.Println("  - 프로브 방식: ICMP ping + HTTP GET")
	fmt.Println("  - 장애 임계: 5분간 성공 없으면 재시작 (successfulPingTimeout)")
	fmt.Println()

	healthServer := NewHealthServer(config)

	// 컴포넌트 등록
	components := []string{
		"cilium-agent",
		"cilium-health",
		"k8s-connectivity",
		"kvstore",
		"endpoint-manager",
		"policy-engine",
		"datapath",
	}

	for _, comp := range components {
		healthServer.RegisterComponent(comp)
	}

	// 컴포넌트 상태 시뮬레이션
	fmt.Println("  [health] 컴포넌트 상태 갱신:")
	healthServer.UpdateComponentStatus("cilium-agent", "ok", "Running")
	fmt.Println("    cilium-agent: ok")
	healthServer.UpdateComponentStatus("cilium-health", "ok", "Healthy")
	fmt.Println("    cilium-health: ok")
	healthServer.UpdateComponentStatus("k8s-connectivity", "ok", "Connected to apiserver")
	fmt.Println("    k8s-connectivity: ok")
	healthServer.UpdateComponentStatus("kvstore", "ok", "Connected to etcd")
	fmt.Println("    kvstore: ok")
	healthServer.UpdateComponentStatus("endpoint-manager", "ok", "15 endpoints managed")
	fmt.Println("    endpoint-manager: ok")
	healthServer.UpdateComponentStatus("policy-engine", "warning", "Policy computation slow (2.1s)")
	fmt.Println("    policy-engine: warning")
	healthServer.UpdateComponentStatus("datapath", "ok", "BPF programs loaded")
	fmt.Println("    datapath: ok")

	// 노드 프로빙 시뮬레이션
	fmt.Println("\n  [health] 노드 프로빙:")
	nodes := []struct {
		name string
		addr string
	}{
		{"node-01", "10.0.0.1"},
		{"node-02", "10.0.0.2"},
		{"node-03", "10.0.0.3"},
		{"node-04", "10.0.0.4"},
	}

	for _, n := range nodes {
		health := healthServer.ProbeNode(n.name, n.addr)
		icmpStr := fmt.Sprintf("ICMP: %s", health.ICMP.Status)
		if health.ICMP.Status == "ok" {
			icmpStr += fmt.Sprintf(" (%v)", health.ICMP.Latency)
		} else {
			icmpStr += fmt.Sprintf(" (%s)", health.ICMP.Error)
		}
		httpStr := fmt.Sprintf("HTTP: %s", health.HTTP.Status)
		if health.HTTP.Status == "ok" {
			httpStr += fmt.Sprintf(" (%v)", health.HTTP.Latency)
		} else {
			httpStr += fmt.Sprintf(" (%s)", health.HTTP.Error)
		}
		fmt.Printf("    %s (%s): %s, %s\n", n.name, n.addr, icmpStr, httpStr)
	}

	// 전체 상태 출력
	fmt.Println("\n  [health] 전체 상태:")
	overallStatus := healthServer.GetOverallStatus()
	for k, v := range overallStatus {
		fmt.Printf("    %s: %v\n", k, v)
	}

	// ==========================================
	// 시나리오 3: Prometheus 메트릭 수집
	// ==========================================
	fmt.Println("\n" + strings.Repeat("-", 60))
	fmt.Println("시나리오 3: Prometheus 메트릭 수집")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println()
	fmt.Println("  Cilium은 Prometheus 형식으로 메트릭을 노출한다.")
	fmt.Println("  - 네임스페이스: cilium_")
	fmt.Println("  - 엔드포인트: :9962/metrics (기본)")
	fmt.Println("  - 수집기: metricsmapCollector (BPF 맵 기반)")
	fmt.Println()

	registry := NewMetricRegistry()

	// Cilium 에이전트 메트릭 등록
	// 포워딩/드롭 카운터
	for _, dir := range []string{"ingress", "egress"} {
		registry.Register("cilium_forward_count_total",
			"Total forwarded packets",
			CounterMetric,
			map[string]string{"direction": dir})
		registry.Register("cilium_forward_bytes_total",
			"Total forwarded bytes",
			CounterMetric,
			map[string]string{"direction": dir})
	}

	// 드롭 카운터
	for _, reason := range []string{"POLICY_DENIED", "INVALID_SOURCE_MAC"} {
		registry.Register("cilium_drop_count_total",
			"Total dropped packets",
			CounterMetric,
			map[string]string{"reason": reason, "direction": "ingress"})
	}

	// 엔드포인트 게이지
	registry.Register("cilium_endpoint_count",
		"Number of endpoints managed by this agent",
		GaugeMetric,
		nil)

	// 정책 관련
	registry.Register("cilium_policy_count",
		"Number of policies currently loaded",
		GaugeMetric,
		nil)
	registry.Register("cilium_policy_import_errors_total",
		"Number of times a policy import has failed",
		CounterMetric,
		nil)

	// 메트릭 값 시뮬레이션
	fmt.Println("  [metrics] 메트릭 값 시뮬레이션:")

	registry.Add("cilium_forward_count_total",
		map[string]string{"direction": "ingress"}, 1523456)
	registry.Add("cilium_forward_bytes_total",
		map[string]string{"direction": "ingress"}, 2147483648)
	registry.Add("cilium_forward_count_total",
		map[string]string{"direction": "egress"}, 1234567)
	registry.Add("cilium_forward_bytes_total",
		map[string]string{"direction": "egress"}, 1073741824)

	registry.Add("cilium_drop_count_total",
		map[string]string{"reason": "POLICY_DENIED", "direction": "ingress"}, 42)
	registry.Add("cilium_drop_count_total",
		map[string]string{"reason": "INVALID_SOURCE_MAC", "direction": "ingress"}, 3)

	registry.Set("cilium_endpoint_count", nil, 15)
	registry.Set("cilium_policy_count", nil, 28)
	registry.Inc("cilium_policy_import_errors_total", nil)

	// Prometheus exposition format 출력
	fmt.Println("\n  [metrics] Prometheus exposition format:")
	exposition := registry.Exposition()
	for _, line := range strings.Split(exposition, "\n") {
		if line != "" {
			fmt.Printf("    %s\n", line)
		}
	}

	// ==========================================
	// 시나리오 4: HTTP 서버 (실제 엔드포인트 시연)
	// ==========================================
	fmt.Println("\n" + strings.Repeat("-", 60))
	fmt.Println("시나리오 4: HTTP 엔드포인트 시연")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println()
	fmt.Println("  실제 Cilium 에이전트가 제공하는 API 엔드포인트:")
	fmt.Println("  - /healthz  → 헬스 체크 (200 OK / 503 Service Unavailable)")
	fmt.Println("  - /status   → 상세 상태 (컴포넌트 + 노드 프로브 결과)")
	fmt.Println("  - /metrics  → Prometheus 메트릭")
	fmt.Println()

	port := 19876 + rand.Intn(100) // 충돌 방지를 위한 랜덤 포트
	server := startHTTPServer(healthServer, registry, port)
	defer server.Close()

	// 약간 대기 후 HTTP 요청
	time.Sleep(100 * time.Millisecond)

	// /healthz 호출
	fmt.Printf("  [http] GET http://localhost:%d/healthz\n", port)
	resp, err := http.Get(fmt.Sprintf("http://localhost:%d/healthz", port))
	if err != nil {
		fmt.Printf("    오류: %v\n", err)
	} else {
		var result map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()
		fmt.Printf("    상태 코드: %d\n", resp.StatusCode)
		data, _ := json.MarshalIndent(result, "    ", "  ")
		fmt.Printf("    응답: %s\n", string(data))
	}

	// /metrics 호출
	fmt.Printf("\n  [http] GET http://localhost:%d/metrics (처음 3줄)\n", port)
	resp2, err := http.Get(fmt.Sprintf("http://localhost:%d/metrics", port))
	if err != nil {
		fmt.Printf("    오류: %v\n", err)
	} else {
		var body strings.Builder
		buf := make([]byte, 4096)
		n, _ := resp2.Body.Read(buf)
		body.Write(buf[:n])
		resp2.Body.Close()
		lines := strings.Split(body.String(), "\n")
		count := 0
		for _, line := range lines {
			if line != "" && count < 6 {
				fmt.Printf("    %s\n", line)
				count++
			}
		}
		fmt.Println("    ...")
	}

	// ==========================================
	// 시나리오 5: 운영 아키텍처 요약
	// ==========================================
	fmt.Println("\n" + strings.Repeat("-", 60))
	fmt.Println("시나리오 5: Cilium 운영 아키텍처 요약")
	fmt.Println(strings.Repeat("-", 60))

	fmt.Println(`
  Cilium 에이전트 운영 구조:

  +-------------------------------------------------------------------+
  |                        cilium-agent                                |
  |                                                                   |
  |  +------------------+  +------------------+  +-----------------+  |
  |  | DaemonConfig     |  | HealthManager    |  | MetricRegistry  |  |
  |  | (pkg/option/)    |  | (pkg/health/)    |  | (pkg/metrics/)  |  |
  |  |                  |  |                  |  |                 |  |
  |  | YAML+Env+CLI     |  | ICMP/HTTP Probe  |  | Prometheus      |  |
  |  | -> Validate()    |  | -> 60s interval  |  | -> /metrics     |  |
  |  | -> checksum()    |  | -> node health   |  | -> Counter      |  |
  |  +--------+---------+  +--------+---------+  | -> Gauge        |  |
  |           |                     |             | -> Histogram    |  |
  |           v                     v             +---------+-------+  |
  |  +--------+---------+  +-------+----------+            |          |
  |  | viper            |  | cilium-health    |            |          |
  |  | ConfigMap watch  |  | :4240/healthz    |    :9962/metrics      |
  |  +------------------+  +------------------+                       |
  +-------------------------------------------------------------------+

  설정 로드 흐름:
  ConfigMap (K8s) --+
  환경변수         --+--> viper --> DaemonConfig --> Validate()
  CLI 플래그       --+                                |
                                                      v
                                              각 서브시스템에 전달

  헬스 체크 흐름:
  +--------+     +----------+     +----------+
  | Timer  |---->| 노드 목록|---->| ICMP/HTTP|---> 결과 캐시
  | (60초) |     | 조회     |     | 프로빙   |---> /healthz API
  +--------+     +----------+     +----------+`)

	fmt.Println("\n\n  운영 시뮬레이션 완료.")
}
