package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// =============================================================================
// Cilium Hive Cell 아키텍처 시뮬레이션
// =============================================================================
//
// Cilium은 Hive라는 자체 DI 프레임워크를 사용하여 컴포넌트를 관리한다.
// 핵심 개념:
//   - Cell: 독립적인 기능 단위 (모듈). Start/Stop 라이프사이클을 가짐
//   - Module: 여러 Cell을 그룹화하는 상위 단위
//   - Hive: 전체 Cell을 관리하는 최상위 컨테이너
//   - Lifecycle Hook: Start/Stop 시점에 실행되는 콜백
//   - Config: 각 Cell이 필요로 하는 설정값 바인딩
//
// 실제 Cilium의 Cell 계층:
//   Infrastructure (K8s client, API server)
//     → ControlPlane (EndpointManager, PolicyEngine, IPCache)
//       → Datapath (BPF loader, map manager)
// =============================================================================

// --- 라이프사이클 인터페이스 ---

// HookInterface는 Cilium의 cell.HookInterface를 시뮬레이션한다.
// 실제 코드: pkg/hive/lifecycle.go
type HookInterface interface {
	Start(HookContext) error
	Stop(HookContext) error
}

// HookContext는 라이프사이클 훅에 전달되는 컨텍스트이다.
// 실제 Cilium에서는 context.Context를 감싸서 타임아웃/취소를 관리한다.
type HookContext struct {
	Timeout time.Duration
}

// --- 설정(Config) 바인딩 ---

// Config는 Cell에 주입되는 설정값을 표현한다.
// 실제 Cilium에서는 cell.Config[T]로 타입 안전한 설정 바인딩을 한다.
type Config struct {
	values map[string]string
}

func NewConfig() *Config {
	return &Config{values: make(map[string]string)}
}

func (c *Config) Set(key, value string) {
	c.values[key] = value
}

func (c *Config) Get(key string) string {
	return c.values[key]
}

func (c *Config) GetWithDefault(key, defaultVal string) string {
	if v, ok := c.values[key]; ok {
		return v
	}
	return defaultVal
}

// --- Cell 인터페이스 ---

// Cell은 Cilium Hive의 최소 기능 단위이다.
// 실제 코드: pkg/hive/cell/cell.go
// 각 Cell은 이름, 의존성 목록, 라이프사이클 훅을 가진다.
type Cell interface {
	Name() string
	Level() CellLevel
	Dependencies() []string
	Start(HookContext) error
	Stop(HookContext) error
}

// CellLevel은 Cell의 계층을 나타낸다.
// Infrastructure → ControlPlane → Datapath 순서로 시작된다.
type CellLevel int

const (
	LevelInfrastructure CellLevel = iota // K8s 클라이언트, API 서버 등 기반 인프라
	LevelControlPlane                    // 엔드포인트 관리, 정책 엔진 등 제어부
	LevelDatapath                        // BPF 프로그램 로딩, 맵 관리 등 데이터 경로
)

func (l CellLevel) String() string {
	switch l {
	case LevelInfrastructure:
		return "Infrastructure"
	case LevelControlPlane:
		return "ControlPlane"
	case LevelDatapath:
		return "Datapath"
	}
	return "Unknown"
}

// --- Infrastructure Cell 구현 ---

// K8sClientCell은 Kubernetes API 클라이언트를 시뮬레이션한다.
// 실제 코드: pkg/k8s/client/cell.go
type K8sClientCell struct {
	config    *Config
	connected bool
}

func NewK8sClientCell(cfg *Config) *K8sClientCell {
	return &K8sClientCell{config: cfg}
}

func (c *K8sClientCell) Name() string          { return "k8s-client" }
func (c *K8sClientCell) Level() CellLevel      { return LevelInfrastructure }
func (c *K8sClientCell) Dependencies() []string { return nil } // 최하위 레벨, 의존성 없음

func (c *K8sClientCell) Start(ctx HookContext) error {
	endpoint := c.config.GetWithDefault("k8s-api-server", "https://10.96.0.1:443")
	fmt.Printf("  [k8s-client] Kubernetes API 서버 연결: %s\n", endpoint)
	c.connected = true
	return nil
}

func (c *K8sClientCell) Stop(ctx HookContext) error {
	fmt.Println("  [k8s-client] Kubernetes API 서버 연결 해제")
	c.connected = false
	return nil
}

func (c *K8sClientCell) IsConnected() bool { return c.connected }

// APIServerCell은 Cilium의 내부 API 서버를 시뮬레이션한다.
// 실제 코드: pkg/api/cell.go — Unix domain socket으로 cilium CLI와 통신
type APIServerCell struct {
	config  *Config
	running bool
}

func NewAPIServerCell(cfg *Config) *APIServerCell {
	return &APIServerCell{config: cfg}
}

func (c *APIServerCell) Name() string          { return "api-server" }
func (c *APIServerCell) Level() CellLevel      { return LevelInfrastructure }
func (c *APIServerCell) Dependencies() []string { return nil }

func (c *APIServerCell) Start(ctx HookContext) error {
	socketPath := c.config.GetWithDefault("api-socket", "/var/run/cilium/cilium.sock")
	fmt.Printf("  [api-server] Unix 소켓에서 대기: %s\n", socketPath)
	c.running = true
	return nil
}

func (c *APIServerCell) Stop(ctx HookContext) error {
	fmt.Println("  [api-server] API 서버 종료")
	c.running = false
	return nil
}

// --- ControlPlane Cell 구현 ---

// EndpointManagerCell은 엔드포인트 관리자를 시뮬레이션한다.
// 실제 코드: pkg/endpoint/cell.go
type EndpointManagerCell struct {
	config    *Config
	endpoints map[uint16]string // ID → Pod 이름
}

func NewEndpointManagerCell(cfg *Config) *EndpointManagerCell {
	return &EndpointManagerCell{config: cfg, endpoints: make(map[uint16]string)}
}

func (c *EndpointManagerCell) Name() string          { return "endpoint-manager" }
func (c *EndpointManagerCell) Level() CellLevel      { return LevelControlPlane }
func (c *EndpointManagerCell) Dependencies() []string { return []string{"k8s-client"} }

func (c *EndpointManagerCell) Start(ctx HookContext) error {
	fmt.Println("  [endpoint-manager] 엔드포인트 관리자 초기화, K8s 워치 시작")
	// 시뮬레이션: 기존 엔드포인트 복원
	c.endpoints[1001] = "pod-frontend-abc"
	c.endpoints[1002] = "pod-backend-xyz"
	fmt.Printf("  [endpoint-manager] 기존 엔드포인트 %d개 복원됨\n", len(c.endpoints))
	return nil
}

func (c *EndpointManagerCell) Stop(ctx HookContext) error {
	fmt.Printf("  [endpoint-manager] %d개 엔드포인트 정리 중\n", len(c.endpoints))
	c.endpoints = make(map[uint16]string)
	return nil
}

// PolicyEngineCell은 네트워크 정책 엔진을 시뮬레이션한다.
// 실제 코드: pkg/policy/cell.go
type PolicyEngineCell struct {
	config *Config
	rules  int
}

func NewPolicyEngineCell(cfg *Config) *PolicyEngineCell {
	return &PolicyEngineCell{config: cfg}
}

func (c *PolicyEngineCell) Name() string          { return "policy-engine" }
func (c *PolicyEngineCell) Level() CellLevel      { return LevelControlPlane }
func (c *PolicyEngineCell) Dependencies() []string { return []string{"k8s-client", "endpoint-manager"} }

func (c *PolicyEngineCell) Start(ctx HookContext) error {
	mode := c.config.GetWithDefault("policy-enforcement", "default")
	fmt.Printf("  [policy-engine] 정책 엔진 시작 (모드: %s)\n", mode)
	c.rules = 5
	fmt.Printf("  [policy-engine] %d개 정책 규칙 로드됨\n", c.rules)
	return nil
}

func (c *PolicyEngineCell) Stop(ctx HookContext) error {
	fmt.Println("  [policy-engine] 정책 엔진 종료")
	return nil
}

// --- Datapath Cell 구현 ---

// BPFLoaderCell은 BPF 프로그램 로더를 시뮬레이션한다.
// 실제 코드: pkg/datapath/loader/cell.go
type BPFLoaderCell struct {
	config         *Config
	loadedPrograms []string
}

func NewBPFLoaderCell(cfg *Config) *BPFLoaderCell {
	return &BPFLoaderCell{config: cfg}
}

func (c *BPFLoaderCell) Name() string     { return "bpf-loader" }
func (c *BPFLoaderCell) Level() CellLevel { return LevelDatapath }
func (c *BPFLoaderCell) Dependencies() []string {
	return []string{"endpoint-manager", "policy-engine"}
}

func (c *BPFLoaderCell) Start(ctx HookContext) error {
	fmt.Println("  [bpf-loader] BPF 프로그램 컴파일 및 로딩 시작")
	// Cilium이 실제로 로딩하는 BPF 프로그램 목록 시뮬레이션
	c.loadedPrograms = []string{
		"bpf_lxc.o",    // 엔드포인트 TC 프로그램 (from_container/to_container)
		"bpf_host.o",   // 호스트 네트워킹 프로그램
		"bpf_overlay.o", // VXLAN/Geneve 오버레이 프로그램
		"bpf_network.o", // 네트워크 정책 프로그램
	}
	for _, p := range c.loadedPrograms {
		fmt.Printf("  [bpf-loader] 로딩 완료: %s\n", p)
	}
	return nil
}

func (c *BPFLoaderCell) Stop(ctx HookContext) error {
	fmt.Printf("  [bpf-loader] %d개 BPF 프로그램 언로드\n", len(c.loadedPrograms))
	c.loadedPrograms = nil
	return nil
}

// MapManagerCell은 BPF 맵 관리자를 시뮬레이션한다.
// 실제 코드: pkg/maps/cell.go
type MapManagerCell struct {
	config *Config
	maps   map[string]int // 맵 이름 → 엔트리 수
}

func NewMapManagerCell(cfg *Config) *MapManagerCell {
	return &MapManagerCell{config: cfg, maps: make(map[string]int)}
}

func (c *MapManagerCell) Name() string          { return "map-manager" }
func (c *MapManagerCell) Level() CellLevel      { return LevelDatapath }
func (c *MapManagerCell) Dependencies() []string { return []string{"bpf-loader"} }

func (c *MapManagerCell) Start(ctx HookContext) error {
	fmt.Println("  [map-manager] BPF 맵 초기화")
	// 실제 Cilium BPF 맵 이름과 용도를 반영
	c.maps["cilium_ct4_global"] = 524288   // Connection Tracking (IPv4)
	c.maps["cilium_ct6_global"] = 524288   // Connection Tracking (IPv6)
	c.maps["cilium_ipcache"] = 512000      // IP → Identity 매핑
	c.maps["cilium_policy"] = 16384        // 정책 맵
	c.maps["cilium_lxc"] = 65535           // 엔드포인트 맵
	c.maps["cilium_lb4_services"] = 65536  // L4 서비스 맵
	for name, size := range c.maps {
		fmt.Printf("  [map-manager] 맵 생성: %-28s (max entries: %d)\n", name, size)
	}
	return nil
}

func (c *MapManagerCell) Stop(ctx HookContext) error {
	fmt.Printf("  [map-manager] %d개 BPF 맵 정리\n", len(c.maps))
	return nil
}

// --- Hive (최상위 컨테이너) ---

// Hive는 모든 Cell을 관리하는 최상위 컨테이너이다.
// 실제 코드: pkg/hive/hive.go
// Cell을 레벨 순서(Infrastructure → ControlPlane → Datapath)로 시작하고,
// 역순으로 정지한다.
type Hive struct {
	cells    []Cell
	config   *Config
	mu       sync.Mutex
	started  bool
	stopCh   chan struct{}
}

func NewHive(cfg *Config) *Hive {
	return &Hive{
		config: cfg,
		stopCh: make(chan struct{}),
	}
}

// RegisterCell은 Hive에 Cell을 등록한다.
func (h *Hive) RegisterCell(cell Cell) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cells = append(h.cells, cell)
}

// validateDependencies는 모든 Cell의 의존성이 충족되는지 확인한다.
func (h *Hive) validateDependencies() error {
	cellNames := make(map[string]bool)
	for _, c := range h.cells {
		cellNames[c.Name()] = true
	}
	for _, c := range h.cells {
		for _, dep := range c.Dependencies() {
			if !cellNames[dep] {
				return fmt.Errorf("Cell '%s'의 의존성 '%s'를 찾을 수 없음", c.Name(), dep)
			}
		}
	}
	return nil
}

// sortCells는 Cell을 레벨 순서로 정렬한다.
// Infrastructure → ControlPlane → Datapath
func (h *Hive) sortCells() {
	// 안정 정렬: 같은 레벨 내에서는 등록 순서 유지
	n := len(h.cells)
	for i := 0; i < n-1; i++ {
		for j := 0; j < n-i-1; j++ {
			if h.cells[j].Level() > h.cells[j+1].Level() {
				h.cells[j], h.cells[j+1] = h.cells[j+1], h.cells[j]
			}
		}
	}
}

// Start는 모든 Cell을 순서대로 시작한다.
func (h *Hive) Start() error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.started {
		return fmt.Errorf("Hive가 이미 시작됨")
	}

	// 1. 의존성 검증
	if err := h.validateDependencies(); err != nil {
		return fmt.Errorf("의존성 검증 실패: %w", err)
	}

	// 2. Cell 정렬 (레벨 순서)
	h.sortCells()

	// 3. 시작 순서 출력
	fmt.Println("\n=== Hive 시작 순서 ===")
	for i, c := range h.cells {
		deps := c.Dependencies()
		depStr := "(없음)"
		if len(deps) > 0 {
			depStr = strings.Join(deps, ", ")
		}
		fmt.Printf("  %d. [%s] %s (의존: %s)\n", i+1, c.Level(), c.Name(), depStr)
	}

	// 4. 순서대로 시작
	ctx := HookContext{Timeout: 10 * time.Second}
	fmt.Println("\n=== Cell 시작 ===")

	currentLevel := CellLevel(-1)
	for _, c := range h.cells {
		if c.Level() != currentLevel {
			currentLevel = c.Level()
			fmt.Printf("\n--- %s 레이어 시작 ---\n", currentLevel)
		}
		fmt.Printf("▶ Cell 시작: %s\n", c.Name())
		if err := c.Start(ctx); err != nil {
			return fmt.Errorf("Cell '%s' 시작 실패: %w", c.Name(), err)
		}
	}

	h.started = true
	fmt.Println("\n=== Hive 시작 완료 ===")
	return nil
}

// Stop은 모든 Cell을 역순으로 정지한다.
// Datapath → ControlPlane → Infrastructure 순서
func (h *Hive) Stop() error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if !h.started {
		return fmt.Errorf("Hive가 시작되지 않음")
	}

	ctx := HookContext{Timeout: 10 * time.Second}
	fmt.Println("\n=== Cell 정지 (역순) ===")

	// 역순으로 정지
	for i := len(h.cells) - 1; i >= 0; i-- {
		c := h.cells[i]
		fmt.Printf("■ Cell 정지: %s\n", c.Name())
		if err := c.Stop(ctx); err != nil {
			log.Printf("경고: Cell '%s' 정지 실패: %v", c.Name(), err)
		}
	}

	h.started = false
	fmt.Println("\n=== Hive 정지 완료 ===")
	return nil
}

// --- 메인 ---

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════╗")
	fmt.Println("║  Cilium Hive Cell 아키텍처 시뮬레이션               ║")
	fmt.Println("║  Infrastructure → ControlPlane → Datapath           ║")
	fmt.Println("╚══════════════════════════════════════════════════════╝")

	// 설정 생성 — 실제 Cilium의 cilium-config ConfigMap에 해당
	cfg := NewConfig()
	cfg.Set("k8s-api-server", "https://10.96.0.1:443")
	cfg.Set("api-socket", "/var/run/cilium/cilium.sock")
	cfg.Set("policy-enforcement", "default")
	cfg.Set("tunnel-protocol", "vxlan")
	cfg.Set("enable-ipv6", "true")

	fmt.Println("\n=== 설정 ===")
	fmt.Printf("  k8s-api-server:    %s\n", cfg.Get("k8s-api-server"))
	fmt.Printf("  policy-enforcement: %s\n", cfg.Get("policy-enforcement"))
	fmt.Printf("  tunnel-protocol:   %s\n", cfg.Get("tunnel-protocol"))

	// Hive 생성 및 Cell 등록
	hive := NewHive(cfg)

	// Infrastructure 레이어
	hive.RegisterCell(NewK8sClientCell(cfg))
	hive.RegisterCell(NewAPIServerCell(cfg))

	// ControlPlane 레이어
	hive.RegisterCell(NewEndpointManagerCell(cfg))
	hive.RegisterCell(NewPolicyEngineCell(cfg))

	// Datapath 레이어
	hive.RegisterCell(NewBPFLoaderCell(cfg))
	hive.RegisterCell(NewMapManagerCell(cfg))

	// Hive 시작
	if err := hive.Start(); err != nil {
		log.Fatalf("Hive 시작 실패: %v", err)
	}

	// 시그널 대기 시뮬레이션
	fmt.Println("\n=== Cilium Agent 실행 중 (Ctrl+C 또는 2초 후 자동 종료) ===")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		fmt.Printf("\n시그널 수신: %v\n", sig)
	case <-time.After(2 * time.Second):
		fmt.Println("\n자동 종료 타이머 만료")
	}

	// Graceful Shutdown
	if err := hive.Stop(); err != nil {
		log.Printf("Hive 정지 오류: %v", err)
	}

	fmt.Println("\nCilium Agent가 정상적으로 종료되었습니다.")
}
