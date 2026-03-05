package main

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// containerd Pod 샌드박스 컨트롤러 시뮬레이션
// =============================================================================
//
// containerd v2에서 Sandbox는 Pod의 격리 환경을 표현하는 핵심 개념이다.
// Kubernetes CRI의 RunPodSandbox를 containerd 내부에서 처리할 때,
// Sandbox Controller가 생성 → 시작 → 정지 → 정리의 생명주기를 관리한다.
//
// 핵심 구조:
//   - Sandbox (메타데이터): ID, Runtime, Spec, Sandboxer, Labels, Extensions
//   - Controller: Create → Start → Stop → Wait → Shutdown → Status → Metrics
//   - ControllerInstance: 실행 중인 샌드박스의 PID, Address 등
//   - Store: 메타데이터 CRUD (BoltDB 기반)
//
// 실제 코드 참조:
//   - core/sandbox/controller.go  — Controller 인터페이스, ControllerInstance
//   - core/sandbox/store.go       — Sandbox 구조체, Store 인터페이스
//   - core/sandbox/bridge.go      — gRPC/tTRPC 브릿지
//   - core/sandbox/helpers.go     — CreateOpt, StopOpt 옵션 패턴
//   - core/metadata/sandbox.go    — BoltDB 기반 메타데이터 저장
// =============================================================================

// --- Sandbox 메타데이터 ---
// 실제 코드: core/sandbox/store.go - Sandbox struct
// 메타데이터 DB에 저장되는 샌드박스 정보이다.

type Sandbox struct {
	ID        string            // 고유 식별자
	Labels    map[string]string // 라벨 (필터링에 사용)
	Runtime   RuntimeOpts       // 런타임 설정 (runc, kata 등)
	Spec      []byte            // OCI 런타임 스펙 (직렬화된 형태)
	Sandboxer string            // 샌드박스 컨트롤러 이름
	CreatedAt time.Time
	UpdatedAt time.Time
	// Extensions는 클라이언트 지정 메타데이터이다.
	// 실제 코드: core/sandbox/store.go 라인 45 — Extensions map[string]typeurl.Any
	Extensions map[string]string
}

// RuntimeOpts는 런타임 정보를 담는다.
// 실제 코드: core/sandbox/store.go - RuntimeOpts struct
type RuntimeOpts struct {
	Name    string // 런타임 이름 (e.g., "io.containerd.runc.v2")
	Options string // 런타임별 옵션 (직렬화된 형태)
}

// AddLabel은 샌드박스에 라벨을 추가한다.
// 실제 코드: core/sandbox/store.go - Sandbox.AddLabel
func (s *Sandbox) AddLabel(name, value string) {
	if s.Labels == nil {
		s.Labels = map[string]string{}
	}
	s.Labels[name] = value
}

// GetLabel은 라벨 값을 조회한다.
// 실제 코드: core/sandbox/store.go - Sandbox.GetLabel
func (s *Sandbox) GetLabel(name string) (string, error) {
	v, ok := s.Labels[name]
	if !ok {
		return "", fmt.Errorf("label %q not found", name)
	}
	return v, nil
}

// --- ControllerInstance ---
// 실제 코드: core/sandbox/controller.go - ControllerInstance struct
// Start() 후 반환되는 실행 중인 샌드박스 인스턴스 정보이다.

type ControllerInstance struct {
	SandboxID string
	Pid       uint32    // 샌드박스 프로세스 PID
	CreatedAt time.Time // 생성 시각
	Address   string    // 샌드박스 접근 주소 (shim socket path)
	Version   uint32    // API 버전
	Labels    map[string]string
}

// --- ExitStatus ---
// 실제 코드: core/sandbox/controller.go - ExitStatus struct

type ExitStatus struct {
	ExitStatus uint32
	ExitedAt   time.Time
}

// --- ControllerStatus ---
// 실제 코드: core/sandbox/controller.go - ControllerStatus struct
// Status() 호출 결과. Ping보다 무거운 호출이며, 상세 상태를 반환한다.

type ControllerStatus struct {
	SandboxID string
	Pid       uint32
	State     string // "created", "running", "stopped"
	CreatedAt time.Time
	ExitedAt  time.Time
	Address   string
	Info      map[string]string
}

// --- Controller 인터페이스 ---
// 실제 코드: core/sandbox/controller.go - Controller interface
// 샌드박스의 전체 생명주기를 관리하는 인터페이스이다.
// CRI의 RunPodSandbox → Create + Start,
// StopPodSandbox → Stop,
// RemovePodSandbox → Shutdown으로 매핑된다.

type Controller interface {
	Create(ctx context.Context, sandbox Sandbox, opts ...CreateOpt) error
	Start(ctx context.Context, sandboxID string) (ControllerInstance, error)
	Stop(ctx context.Context, sandboxID string, opts ...StopOpt) error
	Wait(ctx context.Context, sandboxID string) (ExitStatus, error)
	Status(ctx context.Context, sandboxID string, verbose bool) (ControllerStatus, error)
	Shutdown(ctx context.Context, sandboxID string) error
}

// --- CreateOpt / StopOpt 옵션 패턴 ---
// 실제 코드: core/sandbox/controller.go - CreateOptions, CreateOpt

type CreateOptions struct {
	NetNSPath   string
	Annotations map[string]string
}

type CreateOpt func(*CreateOptions) error

// WithNetNSPath는 네트워크 네임스페이스 경로를 지정한다.
// 실제 코드: core/sandbox/controller.go - WithNetNSPath
func WithNetNSPath(path string) CreateOpt {
	return func(o *CreateOptions) error {
		o.NetNSPath = path
		return nil
	}
}

// WithAnnotations는 어노테이션을 설정한다.
// 실제 코드: core/sandbox/controller.go - WithAnnotations
func WithAnnotations(annotations map[string]string) CreateOpt {
	return func(o *CreateOptions) error {
		o.Annotations = annotations
		return nil
	}
}

type StopOptions struct {
	Timeout *time.Duration
}

type StopOpt func(*StopOptions)

// WithTimeout은 정지 타임아웃을 설정한다.
// 실제 코드: core/sandbox/controller.go - WithTimeout
func WithTimeout(timeout time.Duration) StopOpt {
	return func(o *StopOptions) {
		o.Timeout = &timeout
	}
}

// --- Sandbox Store ---
// 실제 코드: core/sandbox/store.go - Store interface
// 메타데이터 DB(BoltDB)에서 샌드박스 CRUD를 수행한다.
// 실제 BoltDB 경로: v1/<namespace>/sandboxes/<sandbox-id>

type Store interface {
	Create(ctx context.Context, sandbox Sandbox) (Sandbox, error)
	Update(ctx context.Context, sandbox Sandbox, fieldpaths ...string) (Sandbox, error)
	Get(ctx context.Context, id string) (Sandbox, error)
	List(ctx context.Context, filters ...string) ([]Sandbox, error)
	Delete(ctx context.Context, id string) error
}

// --- inMemoryStore 구현 ---
// BoltDB 기반 Store를 메모리 맵으로 시뮬레이션한다.

type inMemoryStore struct {
	mu        sync.RWMutex
	sandboxes map[string]Sandbox
}

func NewInMemoryStore() Store {
	return &inMemoryStore{sandboxes: make(map[string]Sandbox)}
}

func (s *inMemoryStore) Create(ctx context.Context, sandbox Sandbox) (Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.sandboxes[sandbox.ID]; exists {
		return Sandbox{}, fmt.Errorf("sandbox %q already exists", sandbox.ID)
	}

	now := time.Now()
	sandbox.CreatedAt = now
	sandbox.UpdatedAt = now
	s.sandboxes[sandbox.ID] = sandbox
	return sandbox, nil
}

func (s *inMemoryStore) Update(ctx context.Context, sandbox Sandbox, fieldpaths ...string) (Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.sandboxes[sandbox.ID]
	if !ok {
		return Sandbox{}, fmt.Errorf("sandbox %q not found", sandbox.ID)
	}

	// fieldpaths가 지정되면 해당 필드만 업데이트
	// 실제 코드: core/metadata/sandbox.go 에서 fieldpaths에 따라 부분 업데이트
	if len(fieldpaths) == 0 {
		sandbox.CreatedAt = existing.CreatedAt
		sandbox.UpdatedAt = time.Now()
		s.sandboxes[sandbox.ID] = sandbox
	} else {
		for _, fp := range fieldpaths {
			switch fp {
			case "labels":
				existing.Labels = sandbox.Labels
			case "extensions":
				existing.Extensions = sandbox.Extensions
			}
		}
		existing.UpdatedAt = time.Now()
		s.sandboxes[sandbox.ID] = existing
	}

	return s.sandboxes[sandbox.ID], nil
}

func (s *inMemoryStore) Get(ctx context.Context, id string) (Sandbox, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sb, ok := s.sandboxes[id]
	if !ok {
		return Sandbox{}, fmt.Errorf("sandbox %q not found", id)
	}
	return sb, nil
}

func (s *inMemoryStore) List(ctx context.Context, filters ...string) ([]Sandbox, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []Sandbox
	for _, sb := range s.sandboxes {
		// 간단한 라벨 필터 구현
		if len(filters) > 0 {
			match := true
			for _, f := range filters {
				parts := strings.SplitN(f, "=", 2)
				if len(parts) == 2 {
					if v, ok := sb.Labels[parts[0]]; !ok || v != parts[1] {
						match = false
						break
					}
				}
			}
			if !match {
				continue
			}
		}
		result = append(result, sb)
	}
	return result, nil
}

func (s *inMemoryStore) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.sandboxes[id]; !ok {
		return fmt.Errorf("sandbox %q not found", id)
	}
	delete(s.sandboxes, id)
	return nil
}

// --- shimController 구현 ---
// 실제 Sandbox Controller는 shim 프로세스를 관리한다.
// 여기서는 goroutine으로 shim을 시뮬레이션한다.

type shimController struct {
	store     Store
	mu        sync.RWMutex
	instances map[string]*shimInstance
}

type shimInstance struct {
	sandbox  Sandbox
	pid      uint32
	address  string
	state    string // "created", "running", "stopped"
	startAt  time.Time
	exitAt   time.Time
	exitCode uint32
	waitCh   chan struct{} // Wait()에서 사용하는 알림 채널
}

func NewShimController(store Store) Controller {
	return &shimController{
		store:     store,
		instances: make(map[string]*shimInstance),
	}
}

// Create는 샌드박스 환경을 초기화한다 (아직 시작하지 않음).
// 실제 코드: core/sandbox/controller.go - Controller.Create
// CRI에서 RunPodSandbox 호출 시 먼저 Create를 호출한다.
func (sc *shimController) Create(ctx context.Context, sandbox Sandbox, opts ...CreateOpt) error {
	options := &CreateOptions{}
	for _, opt := range opts {
		if err := opt(options); err != nil {
			return err
		}
	}

	// 메타데이터 Store에 저장
	saved, err := sc.store.Create(ctx, sandbox)
	if err != nil {
		return fmt.Errorf("failed to create sandbox metadata: %w", err)
	}

	sc.mu.Lock()
	defer sc.mu.Unlock()

	// shim 인스턴스 생성 (아직 시작되지 않은 상태)
	sc.instances[sandbox.ID] = &shimInstance{
		sandbox: saved,
		state:   "created",
		waitCh:  make(chan struct{}),
	}

	fmt.Printf("    [Controller] 샌드박스 생성: ID=%s, Runtime=%s, Sandboxer=%s\n",
		sandbox.ID, sandbox.Runtime.Name, sandbox.Sandboxer)
	if options.NetNSPath != "" {
		fmt.Printf("    [Controller]   NetNS: %s\n", options.NetNSPath)
	}

	return nil
}

// Start는 이전에 생성된 샌드박스를 시작한다.
// 실제 코드: core/sandbox/controller.go - Controller.Start
// 반환값: ControllerInstance (PID, Address 등)
func (sc *shimController) Start(ctx context.Context, sandboxID string) (ControllerInstance, error) {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	inst, ok := sc.instances[sandboxID]
	if !ok {
		return ControllerInstance{}, fmt.Errorf("sandbox %q not found", sandboxID)
	}

	if inst.state != "created" {
		return ControllerInstance{}, fmt.Errorf("sandbox %q is in %s state, expected created", sandboxID, inst.state)
	}

	// shim 프로세스 시작 시뮬레이션
	inst.pid = uint32(rand.Intn(50000) + 10000)
	inst.address = fmt.Sprintf("/run/containerd/s/%s.sock", sandboxID[:8])
	inst.state = "running"
	inst.startAt = time.Now()

	fmt.Printf("    [Controller] 샌드박스 시작: ID=%s, PID=%d, Address=%s\n",
		sandboxID, inst.pid, inst.address)

	return ControllerInstance{
		SandboxID: sandboxID,
		Pid:       inst.pid,
		CreatedAt: inst.sandbox.CreatedAt,
		Address:   inst.address,
		Version:   2,
		Labels:    inst.sandbox.Labels,
	}, nil
}

// Stop은 실행 중인 샌드박스를 정지한다.
// 실제 코드: core/sandbox/controller.go - Controller.Stop
func (sc *shimController) Stop(ctx context.Context, sandboxID string, opts ...StopOpt) error {
	options := &StopOptions{}
	for _, opt := range opts {
		opt(options)
	}

	sc.mu.Lock()
	defer sc.mu.Unlock()

	inst, ok := sc.instances[sandboxID]
	if !ok {
		return fmt.Errorf("sandbox %q not found", sandboxID)
	}

	if inst.state != "running" {
		return fmt.Errorf("sandbox %q is not running (state=%s)", sandboxID, inst.state)
	}

	timeout := 10 * time.Second
	if options.Timeout != nil {
		timeout = *options.Timeout
	}

	// 정지 시뮬레이션 (graceful shutdown)
	fmt.Printf("    [Controller] 샌드박스 정지 중: ID=%s (timeout=%v)\n", sandboxID, timeout)
	time.Sleep(50 * time.Millisecond)

	inst.state = "stopped"
	inst.exitAt = time.Now()
	inst.exitCode = 0

	// Wait() 대기자에게 알림
	close(inst.waitCh)

	fmt.Printf("    [Controller] 샌드박스 정지 완료: ID=%s, ExitCode=%d\n", sandboxID, inst.exitCode)
	return nil
}

// Wait는 샌드박스 프로세스가 종료될 때까지 블로킹한다.
// 실제 코드: core/sandbox/controller.go - Controller.Wait
func (sc *shimController) Wait(ctx context.Context, sandboxID string) (ExitStatus, error) {
	sc.mu.RLock()
	inst, ok := sc.instances[sandboxID]
	sc.mu.RUnlock()

	if !ok {
		return ExitStatus{}, fmt.Errorf("sandbox %q not found", sandboxID)
	}

	select {
	case <-inst.waitCh:
		return ExitStatus{
			ExitStatus: inst.exitCode,
			ExitedAt:   inst.exitAt,
		}, nil
	case <-ctx.Done():
		return ExitStatus{}, ctx.Err()
	}
}

// Status는 샌드박스의 상세 상태를 반환한다.
// 실제 코드: core/sandbox/controller.go - Controller.Status
// Ping보다 무거운 호출이며, 리소스 사용량 등 상세 정보를 수집한다.
func (sc *shimController) Status(ctx context.Context, sandboxID string, verbose bool) (ControllerStatus, error) {
	sc.mu.RLock()
	defer sc.mu.RUnlock()

	inst, ok := sc.instances[sandboxID]
	if !ok {
		return ControllerStatus{}, fmt.Errorf("sandbox %q not found", sandboxID)
	}

	status := ControllerStatus{
		SandboxID: sandboxID,
		Pid:       inst.pid,
		State:     inst.state,
		CreatedAt: inst.sandbox.CreatedAt,
		ExitedAt:  inst.exitAt,
		Address:   inst.address,
	}

	if verbose {
		status.Info = map[string]string{
			"runtime":   inst.sandbox.Runtime.Name,
			"sandboxer": inst.sandbox.Sandboxer,
			"uptime":    time.Since(inst.startAt).String(),
		}
	}

	return status, nil
}

// Shutdown은 샌드박스와 관련 리소스를 완전히 정리한다.
// 실제 코드: core/sandbox/controller.go - Controller.Shutdown
// Stop과 달리 shim 프로세스와 모든 태스크를 강제 정리한다.
func (sc *shimController) Shutdown(ctx context.Context, sandboxID string) error {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	inst, ok := sc.instances[sandboxID]
	if !ok {
		return fmt.Errorf("sandbox %q not found", sandboxID)
	}

	// 아직 running이면 먼저 정지
	if inst.state == "running" {
		inst.state = "stopped"
		inst.exitAt = time.Now()
		close(inst.waitCh)
	}

	// 메타데이터 삭제
	if err := sc.store.Delete(ctx, sandboxID); err != nil {
		fmt.Printf("    [Controller] 메타데이터 삭제 실패 (무시): %v\n", err)
	}

	delete(sc.instances, sandboxID)
	fmt.Printf("    [Controller] 샌드박스 정리 완료: ID=%s\n", sandboxID)

	return nil
}

// =============================================================================
// 메인 함수 — Pod 샌드박스 생명주기 시뮬레이션
// =============================================================================

func main() {
	fmt.Println("=== containerd Pod 샌드박스 컨트롤러 시뮬레이션 ===")
	fmt.Println()

	ctx := context.Background()
	store := NewInMemoryStore()
	controller := NewShimController(store)

	// --- 1. CRI RunPodSandbox 시뮬레이션 ---
	// Kubernetes kubelet이 CRI를 통해 Pod 샌드박스를 요청한다.
	// CRI에서 containerd로의 매핑:
	//   RunPodSandbox  → Controller.Create + Controller.Start
	//   StopPodSandbox → Controller.Stop
	//   RemovePodSandbox → Controller.Shutdown

	fmt.Println("[1] CRI RunPodSandbox 시뮬레이션")
	fmt.Println(strings.Repeat("-", 60))

	sandbox1 := Sandbox{
		ID: "sb-pod-nginx-abc123",
		Labels: map[string]string{
			"io.kubernetes.pod.name":      "nginx-deployment-7fb96c846b-x4k2n",
			"io.kubernetes.pod.namespace": "default",
			"io.kubernetes.pod.uid":       "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
		},
		Runtime: RuntimeOpts{
			Name: "io.containerd.runc.v2",
		},
		Sandboxer: "podsandbox",
		Spec:      []byte(`{"linux":{"namespaces":[{"type":"pid"},{"type":"ipc"},{"type":"uts"}]}}`),
	}

	// Create (초기화)
	fmt.Println("  [Create]")
	err := controller.Create(ctx, sandbox1,
		WithNetNSPath("/var/run/netns/cni-abc123"),
		WithAnnotations(map[string]string{
			"io.kubernetes.cri.container-type": "sandbox",
		}),
	)
	if err != nil {
		fmt.Printf("  생성 실패: %v\n", err)
		return
	}

	// Start (실행)
	fmt.Println("  [Start]")
	instance, err := controller.Start(ctx, sandbox1.ID)
	if err != nil {
		fmt.Printf("  시작 실패: %v\n", err)
		return
	}
	fmt.Printf("    반환값: SandboxID=%s, PID=%d, Address=%s\n",
		instance.SandboxID, instance.Pid, instance.Address)
	fmt.Println()

	// --- 2. 컨테이너 실행 (Pod 안에서) ---
	fmt.Println("[2] Pod 내 컨테이너 실행 시뮬레이션")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println("  (실제로는 Task API를 통해 컨테이너를 생성/시작한다)")
	fmt.Println("  Container: nginx (image: docker.io/library/nginx:1.25)")
	fmt.Println("  Container: log-collector (image: fluent/fluentd:v1.16)")
	fmt.Println()

	// --- 3. Sandbox Status 조회 ---
	fmt.Println("[3] Sandbox Status 조회")
	fmt.Println(strings.Repeat("-", 60))

	status, err := controller.Status(ctx, sandbox1.ID, true)
	if err != nil {
		fmt.Printf("  상태 조회 실패: %v\n", err)
		return
	}
	fmt.Printf("  SandboxID: %s\n", status.SandboxID)
	fmt.Printf("  PID:       %d\n", status.Pid)
	fmt.Printf("  State:     %s\n", status.State)
	fmt.Printf("  Address:   %s\n", status.Address)
	fmt.Printf("  CreatedAt: %s\n", status.CreatedAt.Format(time.RFC3339))
	if status.Info != nil {
		fmt.Printf("  Info:\n")
		for k, v := range status.Info {
			fmt.Printf("    %s: %s\n", k, v)
		}
	}
	fmt.Println()

	// --- 4. Store 조회 ---
	fmt.Println("[4] Sandbox Store 조회")
	fmt.Println(strings.Repeat("-", 60))

	// 두 번째 샌드박스 생성 (다른 Pod)
	sandbox2 := Sandbox{
		ID: "sb-pod-redis-def456",
		Labels: map[string]string{
			"io.kubernetes.pod.name":      "redis-master-0",
			"io.kubernetes.pod.namespace": "cache",
		},
		Runtime:   RuntimeOpts{Name: "io.containerd.runc.v2"},
		Sandboxer: "podsandbox",
	}
	_ = controller.Create(ctx, sandbox2)
	_, _ = controller.Start(ctx, sandbox2.ID)
	fmt.Println()

	// List all
	allSandboxes, _ := store.List(ctx)
	fmt.Printf("  전체 샌드박스 수: %d\n", len(allSandboxes))
	for _, sb := range allSandboxes {
		podName, _ := sb.GetLabel("io.kubernetes.pod.name")
		podNs, _ := sb.GetLabel("io.kubernetes.pod.namespace")
		fmt.Printf("    ID=%s  Pod=%s/%s  Runtime=%s\n",
			sb.ID, podNs, podName, sb.Runtime.Name)
	}
	fmt.Println()

	// Filter by namespace
	fmt.Println("  필터: io.kubernetes.pod.namespace=cache")
	filtered, _ := store.List(ctx, "io.kubernetes.pod.namespace=cache")
	for _, sb := range filtered {
		podName, _ := sb.GetLabel("io.kubernetes.pod.name")
		fmt.Printf("    ID=%s  Pod=%s\n", sb.ID, podName)
	}
	fmt.Println()

	// --- 5. Wait 시뮬레이션 (비동기) ---
	fmt.Println("[5] Wait 시뮬레이션 (비동기 종료 대기)")
	fmt.Println(strings.Repeat("-", 60))

	waitDone := make(chan struct{})
	go func() {
		exitStatus, err := controller.Wait(ctx, sandbox1.ID)
		if err != nil {
			fmt.Printf("  Wait 에러: %v\n", err)
		} else {
			fmt.Printf("    [Wait] 샌드박스 종료 감지: ExitCode=%d, ExitedAt=%s\n",
				exitStatus.ExitStatus, exitStatus.ExitedAt.Format(time.RFC3339))
		}
		close(waitDone)
	}()

	// 잠시 후 Stop 호출
	time.Sleep(100 * time.Millisecond)

	// --- 6. CRI StopPodSandbox ---
	fmt.Println("[6] CRI StopPodSandbox 시뮬레이션")
	fmt.Println(strings.Repeat("-", 60))

	err = controller.Stop(ctx, sandbox1.ID, WithTimeout(30*time.Second))
	if err != nil {
		fmt.Printf("  정지 실패: %v\n", err)
	}

	// Wait 고루틴 완료 대기
	<-waitDone
	fmt.Println()

	// --- 7. CRI RemovePodSandbox ---
	fmt.Println("[7] CRI RemovePodSandbox 시뮬레이션")
	fmt.Println(strings.Repeat("-", 60))

	err = controller.Shutdown(ctx, sandbox1.ID)
	if err != nil {
		fmt.Printf("  정리 실패: %v\n", err)
	}

	// 정리 후 확인
	remaining, _ := store.List(ctx)
	fmt.Printf("  남은 샌드박스 수: %d\n", len(remaining))
	for _, sb := range remaining {
		fmt.Printf("    ID=%s\n", sb.ID)
	}
	fmt.Println()

	// --- 8. 두 번째 샌드박스도 정리 ---
	fmt.Println("[8] 두 번째 샌드박스 정리")
	fmt.Println(strings.Repeat("-", 60))
	_ = controller.Stop(ctx, sandbox2.ID)
	_ = controller.Shutdown(ctx, sandbox2.ID)

	remaining2, _ := store.List(ctx)
	fmt.Printf("  남은 샌드박스 수: %d\n", len(remaining2))
	fmt.Println()

	// --- CRI ↔ Sandbox 매핑 설명 ---
	fmt.Println("[CRI ↔ Sandbox Controller 매핑]")
	fmt.Println()
	fmt.Println("  CRI API              →  Sandbox Controller")
	fmt.Println("  ─────────────────────────────────────────────")
	fmt.Println("  RunPodSandbox        →  Create + Start")
	fmt.Println("  StopPodSandbox       →  Stop")
	fmt.Println("  RemovePodSandbox     →  Shutdown")
	fmt.Println("  PodSandboxStatus     →  Status")
	fmt.Println("  ListPodSandbox       →  Store.List")
	fmt.Println()

	fmt.Println("=== 시뮬레이션 완료 ===")
}
