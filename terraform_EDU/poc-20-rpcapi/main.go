// Package main은 Terraform의 RPC API 서버 프레임워크를
// Go 표준 라이브러리만으로 시뮬레이션하는 PoC이다.
//
// 시뮬레이션하는 핵심 개념:
// 1. Magic Cookie 핸드셰이크 (환경변수 기반 인증)
// 2. Handle 테이블 (제네릭 정수 핸들로 서버 측 객체 참조)
// 3. go-plugin 프레임워크 패턴 (부모-자식 프로세스 통신)
// 4. 서비스 지연 초기화 (Handshake 이후 능력 협상)
// 5. Stopper 패턴 (정지 신호 전파)
// 6. gRPC 서비스 구조 (Setup, Stacks, Packages, Dependencies)
// 7. 동적 서비스 등록 (dynrpcserver 패턴)
// 8. CLI Command 진입점
// 9. 텔레메트리 통합
// 10. 에러 전파 및 정리(cleanup)
//
// 실제 소스 참조:
//   - internal/rpcapi/server.go     (ServePlugin, 핸드셰이크)
//   - internal/rpcapi/handles.go    (handleTable, 제네릭 핸들 관리)
//   - internal/rpcapi/cli.go        (CLICommandFactory)
//   - internal/rpcapi/setup.go      (setupServer, Handshake RPC)
//   - internal/rpcapi/stopper.go    (정지 신호 관리)
//   - internal/rpcapi/plugin.go     (corePlugin 구현)
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================================================
// 1. Magic Cookie 핸드셰이크 (internal/rpcapi/server.go 시뮬레이션)
// ============================================================================

// MagicCookie는 go-plugin의 핸드셰이크에 사용하는 매직 값이다.
// 환경변수에 이 값이 없으면 플러그인이 직접 실행된 것으로 간주하고 거부한다.
const (
	MagicCookieKey   = "TERRAFORM_RPCAPI_COOKIE"
	MagicCookieValue = "terraform_rpcapi_v0"
)

// ValidateHandshake는 매직 쿠키로 실행 환경을 검증한다.
func ValidateHandshake(env map[string]string) error {
	v, ok := env[MagicCookieKey]
	if !ok {
		return fmt.Errorf("이 프로그램은 직접 실행할 수 없습니다 (매직 쿠키 없음)")
	}
	if v != MagicCookieValue {
		return fmt.Errorf("매직 쿠키 불일치: %q != %q", v, MagicCookieValue)
	}
	return nil
}

// ============================================================================
// 2. Handle 테이블 (internal/rpcapi/handles.go 시뮬레이션)
// ============================================================================

// Handle은 서버 측 객체를 참조하는 정수 핸들이다.
// 실제 구현은 제네릭을 사용한다: handleTable[T any]
type Handle int64

// HandleTable은 정수 핸들로 서버 측 객체를 관리하는 테이블이다.
// 실제 구현: internal/rpcapi/handles.go의 handleTable[T any]
type HandleTable struct {
	mu      sync.Mutex
	nextID  int64
	objects map[Handle]interface{}
}

// NewHandleTable은 새 핸들 테이블을 생성한다.
func NewHandleTable() *HandleTable {
	return &HandleTable{
		nextID:  1,
		objects: make(map[Handle]interface{}),
	}
}

// Allocate는 새 핸들을 할당하고 객체를 저장한다.
func (t *HandleTable) Allocate(obj interface{}) Handle {
	t.mu.Lock()
	defer t.mu.Unlock()
	h := Handle(t.nextID)
	t.nextID++
	t.objects[h] = obj
	return h
}

// Get은 핸들로 객체를 조회한다.
func (t *HandleTable) Get(h Handle) (interface{}, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	obj, ok := t.objects[h]
	return obj, ok
}

// Close는 핸들을 해제하고 객체를 제거한다.
func (t *HandleTable) Close(h Handle) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.objects[h]
	if ok {
		delete(t.objects, h)
	}
	return ok
}

// Count는 활성 핸들 수를 반환한다.
func (t *HandleTable) Count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.objects)
}

// ============================================================================
// 3. Stopper 패턴 (internal/rpcapi/stopper.go 시뮬레이션)
// ============================================================================

// Stopper는 서버 정지 신호를 관리한다.
type Stopper struct {
	stopCh chan struct{}
	once   sync.Once
}

// NewStopper는 새 Stopper를 생성한다.
func NewStopper() *Stopper {
	return &Stopper{
		stopCh: make(chan struct{}),
	}
}

// Stop은 정지 신호를 전파한다 (한 번만 실행).
func (s *Stopper) Stop() {
	s.once.Do(func() {
		close(s.stopCh)
	})
}

// Done은 정지 신호 채널을 반환한다.
func (s *Stopper) Done() <-chan struct{} {
	return s.stopCh
}

// ============================================================================
// 4. 서비스 정의 (internal/rpcapi/ 서비스들 시뮬레이션)
// ============================================================================

// Capability는 클라이언트가 요청하는 능력을 나타낸다.
type Capability string

const (
	CapPlan         Capability = "plan"
	CapApply        Capability = "apply"
	CapStateInspect Capability = "state_inspect"
)

// HandshakeRequest는 핸드셰이크 요청을 나타낸다.
type HandshakeRequest struct {
	Capabilities []Capability `json:"capabilities"`
	Config       interface{}  `json:"config,omitempty"`
}

// HandshakeResponse는 핸드셰이크 응답을 나타낸다.
type HandshakeResponse struct {
	ServerVersion     string       `json:"server_version"`
	ActiveCapabilities []Capability `json:"active_capabilities"`
}

// StackConfig는 스택 설정을 나타낸다.
type StackConfig struct {
	Name    string `json:"name"`
	WorkDir string `json:"work_dir"`
}

// PlanResult는 Plan 결과를 나타낸다.
type PlanResult struct {
	StackHandle Handle   `json:"stack_handle"`
	Changes     []Change `json:"changes"`
}

// Change는 개별 리소스 변경을 나타낸다.
type Change struct {
	Address string `json:"address"`
	Action  string `json:"action"`
}

// ============================================================================
// 5. 동적 서비스 등록 (dynrpcserver 패턴)
// ============================================================================

// ServiceRegistrar는 서비스를 등록하는 인터페이스다.
type ServiceRegistrar interface {
	RegisterService(name string, handler http.Handler)
}

// DynServer는 동적으로 서비스를 등록/교체할 수 있는 서버다.
type DynServer struct {
	mu       sync.RWMutex
	mux      *http.ServeMux
	services map[string]http.Handler
}

// NewDynServer는 새 동적 서버를 생성한다.
func NewDynServer() *DynServer {
	return &DynServer{
		mux:      http.NewServeMux(),
		services: make(map[string]http.Handler),
	}
}

// RegisterService는 서비스를 등록한다.
func (s *DynServer) RegisterService(name string, handler http.Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.services[name] = handler
	s.mux.Handle("/"+name+"/", http.StripPrefix("/"+name, handler))
}

// ============================================================================
// 6. RPC API 서버 (internal/rpcapi/setup.go + plugin.go 시뮬레이션)
// ============================================================================

// RPCServer는 Terraform RPC API 서버를 나타낸다.
type RPCServer struct {
	stopper     *Stopper
	stacks      *HandleTable
	listener    net.Listener
	dynServer   *DynServer
	initialized atomic.Bool
	requestCount atomic.Int64

	// 서비스별 활성화 상태
	planEnabled  bool
	applyEnabled bool
	stateEnabled bool
}

// NewRPCServer는 새 RPC 서버를 생성한다.
func NewRPCServer() *RPCServer {
	return &RPCServer{
		stopper:   NewStopper(),
		stacks:    NewHandleTable(),
		dynServer: NewDynServer(),
	}
}

// Handshake는 클라이언트와 핸드셰이크를 수행한다.
// 실제 구현: internal/rpcapi/setup.go의 setupServer.Handshake
func (s *RPCServer) Handshake(req HandshakeRequest) (*HandshakeResponse, error) {
	if s.initialized.Load() {
		return nil, fmt.Errorf("이미 핸드셰이크 완료됨")
	}

	var active []Capability
	for _, cap := range req.Capabilities {
		switch cap {
		case CapPlan:
			s.planEnabled = true
			active = append(active, cap)
		case CapApply:
			s.applyEnabled = true
			active = append(active, cap)
		case CapStateInspect:
			s.stateEnabled = true
			active = append(active, cap)
		default:
			// 알 수 없는 능력은 무시
			fmt.Printf("  [경고] 알 수 없는 능력: %s\n", cap)
		}
	}

	s.initialized.Store(true)

	return &HandshakeResponse{
		ServerVersion:     "1.10.0",
		ActiveCapabilities: active,
	}, nil
}

// OpenStack은 새 스택을 열고 핸들을 반환한다.
// 실제 구현: internal/rpcapi/stacks.go
func (s *RPCServer) OpenStack(config StackConfig) (Handle, error) {
	if !s.initialized.Load() {
		return 0, fmt.Errorf("핸드셰이크가 필요합니다")
	}
	handle := s.stacks.Allocate(config)
	return handle, nil
}

// PlanStack은 스택에 대해 Plan을 실행한다.
func (s *RPCServer) PlanStack(stackHandle Handle) (*PlanResult, error) {
	if !s.planEnabled {
		return nil, fmt.Errorf("plan 능력이 활성화되지 않음")
	}

	obj, ok := s.stacks.Get(stackHandle)
	if !ok {
		return nil, fmt.Errorf("잘못된 스택 핸들: %d", stackHandle)
	}

	config := obj.(StackConfig)
	s.requestCount.Add(1)

	// Plan 시뮬레이션
	return &PlanResult{
		StackHandle: stackHandle,
		Changes: []Change{
			{Address: fmt.Sprintf("module.%s.aws_instance.web", config.Name), Action: "create"},
			{Address: fmt.Sprintf("module.%s.aws_security_group.web", config.Name), Action: "create"},
			{Address: fmt.Sprintf("module.%s.aws_subnet.main", config.Name), Action: "update"},
		},
	}, nil
}

// CloseStack은 스택 핸들을 해제한다.
func (s *RPCServer) CloseStack(handle Handle) error {
	if !s.stacks.Close(handle) {
		return fmt.Errorf("잘못된 스택 핸들: %d", handle)
	}
	return nil
}

// Stop은 서버를 정지한다.
func (s *RPCServer) Stop() {
	s.stopper.Stop()
}

// ============================================================================
// 7. CLI Command (internal/rpcapi/cli.go 시뮬레이션)
// ============================================================================

// CLICommand는 `terraform rpcapi` CLI 명령을 나타낸다.
type CLICommand struct {
	server *RPCServer
}

// Run은 RPC API 서버를 실행한다.
func (c *CLICommand) Run(env map[string]string) int {
	// 1. 매직 쿠키 검증
	if err := ValidateHandshake(env); err != nil {
		fmt.Printf("오류: %v\n", err)
		return 1
	}

	// 2. 서버 생성
	c.server = NewRPCServer()

	fmt.Println("RPC API 서버 준비 완료")
	return 0
}

// ============================================================================
// 텔레메트리 데모
// ============================================================================

// Telemetry는 RPC API 호출의 텔레메트리를 수집한다.
// 실제 구현: internal/rpcapi/telemetry.go
type Telemetry struct {
	mu     sync.Mutex
	spans  []Span
}

// Span은 단일 RPC 호출의 추적 정보다.
type Span struct {
	Name      string
	StartTime time.Time
	Duration  time.Duration
	Error     string
}

// NewTelemetry는 새 텔레메트리 수집기를 생성한다.
func NewTelemetry() *Telemetry {
	return &Telemetry{}
}

// RecordSpan은 스팬을 기록한다.
func (t *Telemetry) RecordSpan(name string, duration time.Duration, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	span := Span{
		Name:      name,
		StartTime: time.Now(),
		Duration:  duration,
	}
	if err != nil {
		span.Error = err.Error()
	}
	t.spans = append(t.spans, span)
}

// Summary는 텔레메트리 요약을 반환한다.
func (t *Telemetry) Summary() string {
	t.mu.Lock()
	defer t.mu.Unlock()

	var sb strings.Builder
	sb.WriteString("텔레메트리 요약:\n")
	for _, span := range t.spans {
		status := "OK"
		if span.Error != "" {
			status = "ERROR: " + span.Error
		}
		sb.WriteString(fmt.Sprintf("  %-25s %12s  %s\n", span.Name, span.Duration.Round(time.Microsecond), status))
	}
	return sb.String()
}

// ============================================================================
// main
// ============================================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║  Terraform RPC API 서버 프레임워크 시뮬레이션 PoC            ║")
	fmt.Println("║  실제 소스: internal/rpcapi/                                ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()

	// === 1. 매직 쿠키 핸드셰이크 데모 ===
	fmt.Println("=== 1. 매직 쿠키 핸드셰이크 ===")

	// 직접 실행 시도 (실패)
	err := ValidateHandshake(map[string]string{})
	fmt.Printf("  직접 실행: %v\n", err)

	// 잘못된 쿠키 (실패)
	err = ValidateHandshake(map[string]string{MagicCookieKey: "wrong_value"})
	fmt.Printf("  잘못된 쿠키: %v\n", err)

	// 올바른 쿠키 (성공)
	err = ValidateHandshake(map[string]string{MagicCookieKey: MagicCookieValue})
	fmt.Printf("  올바른 쿠키: 오류=%v (성공)\n", err)
	fmt.Println()

	// === 2. Handle 테이블 데모 ===
	fmt.Println("=== 2. Handle 테이블 (제네릭 핸들 관리) ===")

	ht := NewHandleTable()
	h1 := ht.Allocate("stack-prod")
	h2 := ht.Allocate("stack-staging")
	h3 := ht.Allocate("stack-dev")
	fmt.Printf("  핸들 할당: h1=%d, h2=%d, h3=%d\n", h1, h2, h3)
	fmt.Printf("  활성 핸들 수: %d\n", ht.Count())

	obj, ok := ht.Get(h1)
	fmt.Printf("  h1 조회: %v (존재=%v)\n", obj, ok)

	ht.Close(h2)
	fmt.Printf("  h2 해제 후 활성 핸들 수: %d\n", ht.Count())

	_, ok = ht.Get(h2)
	fmt.Printf("  h2 조회: 존재=%v\n", ok)
	fmt.Println()

	// === 3. Stopper 패턴 데모 ===
	fmt.Println("=== 3. Stopper 패턴 (정지 신호 전파) ===")

	stopper := NewStopper()
	var wg sync.WaitGroup

	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			select {
			case <-stopper.Done():
				fmt.Printf("  워커 %d: 정지 신호 수신\n", id)
			case <-time.After(1 * time.Second):
				fmt.Printf("  워커 %d: 타임아웃\n", id)
			}
		}(i)
	}

	time.Sleep(10 * time.Millisecond)
	fmt.Println("  Stop() 호출...")
	stopper.Stop()
	stopper.Stop() // 두 번째 호출은 무시됨 (sync.Once)
	wg.Wait()
	fmt.Println()

	// === 4. RPC 서버 전체 흐름 데모 ===
	fmt.Println("=== 4. RPC API 서버 전체 흐름 ===")

	telemetry := NewTelemetry()
	server := NewRPCServer()

	// 핸드셰이크 전 스택 열기 시도 (실패)
	_, err = server.OpenStack(StackConfig{Name: "test", WorkDir: "/tmp"})
	fmt.Printf("  핸드셰이크 전 OpenStack: %v\n", err)

	// 핸드셰이크 수행
	start := time.Now()
	resp, err := server.Handshake(HandshakeRequest{
		Capabilities: []Capability{CapPlan, CapStateInspect, "unknown_cap"},
	})
	telemetry.RecordSpan("Setup.Handshake", time.Since(start), err)

	if err != nil {
		fmt.Printf("  핸드셰이크 오류: %v\n", err)
		return
	}
	respJSON, _ := json.MarshalIndent(resp, "  ", "  ")
	fmt.Printf("  핸드셰이크 응답:\n  %s\n", string(respJSON))

	// 중복 핸드셰이크 시도 (실패)
	_, err = server.Handshake(HandshakeRequest{})
	fmt.Printf("  중복 핸드셰이크: %v\n", err)
	fmt.Println()

	// 스택 작업
	fmt.Println("=== 5. 스택 작업 ===")

	start = time.Now()
	stackHandle, err := server.OpenStack(StackConfig{
		Name:    "production",
		WorkDir: "/app/terraform/prod",
	})
	telemetry.RecordSpan("Stacks.OpenStack", time.Since(start), err)
	fmt.Printf("  스택 열기: handle=%d, err=%v\n", stackHandle, err)

	// Plan 실행
	start = time.Now()
	plan, err := server.PlanStack(stackHandle)
	telemetry.RecordSpan("Stacks.PlanStack", time.Since(start), err)
	if err != nil {
		fmt.Printf("  Plan 오류: %v\n", err)
	} else {
		fmt.Printf("  Plan 결과 (변경 %d건):\n", len(plan.Changes))
		for _, c := range plan.Changes {
			fmt.Printf("    %-8s %s\n", c.Action, c.Address)
		}
	}

	// Apply (능력 미활성화)
	fmt.Println()
	fmt.Println("=== 6. 비활성 능력 테스트 ===")
	start = time.Now()
	_, err = server.PlanStack(Handle(999))
	telemetry.RecordSpan("Stacks.PlanStack(invalid)", time.Since(start), err)
	fmt.Printf("  잘못된 핸들 Plan: %v\n", err)

	// 스택 닫기
	err = server.CloseStack(stackHandle)
	fmt.Printf("  스택 닫기: err=%v\n", err)
	fmt.Printf("  활성 스택 수: %d\n", server.stacks.Count())
	fmt.Println()

	// === 7. CLI Command 시뮬레이션 ===
	fmt.Println("=== 7. CLI Command 시뮬레이션 ===")

	cli := &CLICommand{}
	exitCode := cli.Run(map[string]string{MagicCookieKey: MagicCookieValue})
	fmt.Printf("  종료 코드: %d\n", exitCode)

	exitCode = cli.Run(map[string]string{})
	fmt.Printf("  매직 쿠키 없이 종료 코드: %d\n", exitCode)
	fmt.Println()

	// === 8. 텔레메트리 요약 ===
	fmt.Println("=== 8. 텔레메트리 ===")
	fmt.Print(telemetry.Summary())

	// 정리
	server.Stop()
	fmt.Println("\n서버 정지 완료")
}
