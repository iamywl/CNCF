package main

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// =============================================================================
// tart CLI 커맨드 디스패치와 VM 라이프사이클 시뮬레이션
//
// tart 실제 소스코드 참조:
//   - Sources/tart/Root.swift          : @main Root, 서브커맨드 등록, SIGINT 처리, GC
//   - Sources/tart/Commands/Create.swift: VM 생성 커맨드
//   - Sources/tart/Commands/Run.swift   : VM 실행 커맨드
//   - Sources/tart/Commands/Stop.swift  : VM 중지 커맨드
//   - Sources/tart/Commands/Delete.swift: VM 삭제 커맨드
//   - Sources/tart/OTel.swift           : OpenTelemetry 스팬 생성/추적
//   - Sources/tart/Config.swift         : gc() — 임시 디렉토리 정리
// =============================================================================

// ---------------------------------------------------------------------------
// VMState: tart VMDirectory.State 시뮬레이션
//   실제 코드: Sources/tart/VMDirectory.swift — enum State { Running, Suspended, Stopped }
// ---------------------------------------------------------------------------
type VMState string

const (
	StateCreated   VMState = "created"   // 디렉토리 생성됨
	StateConfigured VMState = "configured" // config.json + disk.img + nvram.bin 작성됨
	StateRunning   VMState = "running"    // VM이 실행 중
	StateStopped   VMState = "stopped"    // VM이 중지됨
)

// ---------------------------------------------------------------------------
// VM: 가상 머신 인스턴스
//   실제 코드: Sources/tart/VM.swift — class VM: NSObject, VZVirtualMachineDelegate
// ---------------------------------------------------------------------------
type VM struct {
	Name      string
	State     VMState
	CPU       int
	MemoryMB  int
	CreatedAt time.Time
	cancel    context.CancelFunc // graceful shutdown을 위한 cancel 함수
	mu        sync.Mutex
}

// ---------------------------------------------------------------------------
// VMManager: VM 목록을 관리하는 매니저
//   실제 코드: Sources/tart/VMStorageLocal.swift — class VMStorageLocal
// ---------------------------------------------------------------------------
type VMManager struct {
	vms map[string]*VM
	mu  sync.RWMutex
}

func NewVMManager() *VMManager {
	return &VMManager{
		vms: make(map[string]*VM),
	}
}

// ---------------------------------------------------------------------------
// OTelSpan: OpenTelemetry 스팬 시뮬레이션
//   실제 코드: Sources/tart/OTel.swift — OTel.shared.tracer.spanBuilder(spanName:).startSpan()
//   Root.swift에서 각 커맨드 실행 전에 루트 스팬을 생성하고, 커맨드 인자를 속성으로 설정
// ---------------------------------------------------------------------------
type OTelSpan struct {
	Name       string
	StartTime  time.Time
	EndTime    time.Time
	Attributes map[string]string
	Status     string
}

func NewSpan(name string) *OTelSpan {
	return &OTelSpan{
		Name:       name,
		StartTime:  time.Now(),
		Attributes: make(map[string]string),
		Status:     "OK",
	}
}

func (s *OTelSpan) SetAttribute(key, value string) {
	s.Attributes[key] = value
}

func (s *OTelSpan) RecordException(err error) {
	s.Status = "ERROR"
	s.Attributes["exception.message"] = err.Error()
}

func (s *OTelSpan) End() {
	s.EndTime = time.Now()
	duration := s.EndTime.Sub(s.StartTime)
	fmt.Printf("  [OTel] 스팬 종료: name=%s, status=%s, duration=%v\n", s.Name, s.Status, duration)
	for k, v := range s.Attributes {
		fmt.Printf("         attr: %s=%s\n", k, v)
	}
}

// ---------------------------------------------------------------------------
// OTelTracer: 스팬 수집 및 flush 시뮬레이션
//   실제 코드: Sources/tart/OTel.swift — class OTel { func flush() }
//   TRACEPARENT 환경변수가 설정된 경우에만 트레이싱을 초기화
// ---------------------------------------------------------------------------
type OTelTracer struct {
	spans []*OTelSpan
	mu    sync.Mutex
}

var globalTracer = &OTelTracer{}

func (t *OTelTracer) StartSpan(name string) *OTelSpan {
	span := NewSpan(name)
	t.mu.Lock()
	t.spans = append(t.spans, span)
	t.mu.Unlock()
	return span
}

func (t *OTelTracer) Flush() {
	t.mu.Lock()
	defer t.mu.Unlock()
	fmt.Printf("\n[OTel] Flush: 총 %d개 스팬 전송 완료\n", len(t.spans))
}

// ---------------------------------------------------------------------------
// Command: 커맨드 인터페이스
//   실제 코드: ArgumentParser의 AsyncParsableCommand 프로토콜
//   Root.swift에서 parseAsRoot() → run() 호출 패턴
// ---------------------------------------------------------------------------
type Command interface {
	Name() string
	Execute(ctx context.Context, mgr *VMManager, args []string) error
}

// ---------------------------------------------------------------------------
// CreateCommand: VM 생성
//   실제 코드: Sources/tart/Commands/Create.swift
//   임시 디렉토리에 VM 생성 후 VMStorageLocal.move()로 이동
// ---------------------------------------------------------------------------
type CreateCommand struct{}

func (c *CreateCommand) Name() string { return "create" }

func (c *CreateCommand) Execute(ctx context.Context, mgr *VMManager, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("사용법: create <vm-name>")
	}
	name := args[0]

	mgr.mu.Lock()
	defer mgr.mu.Unlock()

	if _, exists := mgr.vms[name]; exists {
		return fmt.Errorf("VM '%s' 이미 존재합니다", name)
	}

	// 실제 tart: VMDirectory.temporary() → VM 초기화 → VMStorageLocal.move()
	fmt.Printf("  임시 디렉토리에 VM '%s' 생성 중...\n", name)
	vm := &VM{
		Name:      name,
		State:     StateCreated,
		CPU:       4,
		MemoryMB:  4096,
		CreatedAt: time.Now(),
	}

	// config 구성 시뮬레이션 (실제: VMConfig 생성 → config.json 저장)
	time.Sleep(50 * time.Millisecond)
	vm.State = StateConfigured
	fmt.Printf("  VM '%s' 구성 완료: CPU=%d, Memory=%dMB\n", name, vm.CPU, vm.MemoryMB)

	mgr.vms[name] = vm
	fmt.Printf("  VM '%s' 로컬 저장소로 이동 완료\n", name)
	return nil
}

// ---------------------------------------------------------------------------
// RunCommand: VM 실행
//   실제 코드: Sources/tart/Commands/Run.swift
//   VM.start() → VM.run() → sema.waitUnlessCancelled()
//   취소 시 VM.stop() → network.stop()
// ---------------------------------------------------------------------------
type RunCommand struct{}

func (r *RunCommand) Name() string { return "run" }

func (r *RunCommand) Execute(ctx context.Context, mgr *VMManager, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("사용법: run <vm-name>")
	}
	name := args[0]

	mgr.mu.RLock()
	vm, exists := mgr.vms[name]
	mgr.mu.RUnlock()

	if !exists {
		return fmt.Errorf("VM '%s'이 존재하지 않습니다", name)
	}

	vm.mu.Lock()
	if vm.State == StateRunning {
		vm.mu.Unlock()
		return fmt.Errorf("VM '%s'이 이미 실행 중입니다", name)
	}

	// 실제 tart: VM.start(recovery:, resume:) → network.run(sema) → virtualMachine.start()
	runCtx, cancel := context.WithCancel(ctx)
	vm.cancel = cancel
	vm.State = StateRunning
	vm.mu.Unlock()

	fmt.Printf("  VM '%s' 시작됨 (PID 시뮬레이션: %d)\n", name, rand.Intn(90000)+10000)

	// 실제 tart: sema.waitUnlessCancelled() — VM이 종료될 때까지 대기
	select {
	case <-runCtx.Done():
		fmt.Printf("  VM '%s' 취소 신호 수신 — graceful shutdown 진행\n", name)
	case <-time.After(2 * time.Second):
		fmt.Printf("  VM '%s' 시뮬레이션 실행 완료\n", name)
	}

	vm.mu.Lock()
	vm.State = StateStopped
	vm.mu.Unlock()
	fmt.Printf("  VM '%s' 중지됨\n", name)
	return nil
}

// ---------------------------------------------------------------------------
// StopCommand: VM 중지
//   실제 코드: Sources/tart/Commands/Stop.swift
//   ControlSocket을 통해 실행 중인 VM에 중지 신호 전송
// ---------------------------------------------------------------------------
type StopCommand struct{}

func (s *StopCommand) Name() string { return "stop" }

func (s *StopCommand) Execute(ctx context.Context, mgr *VMManager, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("사용법: stop <vm-name>")
	}
	name := args[0]

	mgr.mu.RLock()
	vm, exists := mgr.vms[name]
	mgr.mu.RUnlock()

	if !exists {
		return fmt.Errorf("VM '%s'이 존재하지 않습니다", name)
	}

	vm.mu.Lock()
	defer vm.mu.Unlock()

	if vm.State != StateRunning {
		return fmt.Errorf("VM '%s'이 실행 중이지 않습니다 (현재: %s)", name, vm.State)
	}

	// 실제 tart: cancel 함수를 호출하여 Run 커맨드의 context를 취소
	if vm.cancel != nil {
		vm.cancel()
	}
	vm.State = StateStopped
	fmt.Printf("  VM '%s' 중지 명령 전송 완료\n", name)
	return nil
}

// ---------------------------------------------------------------------------
// ListCommand: VM 목록 조회
//   실제 코드: Sources/tart/Commands/List.swift
//   VMStorageLocal.list() + VMStorageOCI.list() 결과를 포맷터로 출력
// ---------------------------------------------------------------------------
type ListCommand struct{}

func (l *ListCommand) Name() string { return "list" }

func (l *ListCommand) Execute(ctx context.Context, mgr *VMManager, args []string) error {
	mgr.mu.RLock()
	defer mgr.mu.RUnlock()

	if len(mgr.vms) == 0 {
		fmt.Println("  등록된 VM이 없습니다")
		return nil
	}

	fmt.Printf("  %-15s %-12s %-6s %-10s %s\n", "NAME", "STATE", "CPU", "MEMORY", "CREATED")
	fmt.Printf("  %s\n", strings.Repeat("-", 65))
	for _, vm := range mgr.vms {
		fmt.Printf("  %-15s %-12s %-6d %-10s %s\n",
			vm.Name, vm.State, vm.CPU,
			fmt.Sprintf("%dMB", vm.MemoryMB),
			vm.CreatedAt.Format("2006-01-02 15:04"))
	}
	return nil
}

// ---------------------------------------------------------------------------
// DeleteCommand: VM 삭제
//   실제 코드: Sources/tart/Commands/Delete.swift
//   VMStorageHelper.delete() → PIDLock 확인 → FileManager.removeItem()
// ---------------------------------------------------------------------------
type DeleteCommand struct{}

func (d *DeleteCommand) Name() string { return "delete" }

func (d *DeleteCommand) Execute(ctx context.Context, mgr *VMManager, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("사용법: delete <vm-name>")
	}
	name := args[0]

	mgr.mu.Lock()
	defer mgr.mu.Unlock()

	vm, exists := mgr.vms[name]
	if !exists {
		return fmt.Errorf("VM '%s'이 존재하지 않습니다", name)
	}

	// 실제 tart: VMDirectory.delete() — PIDLock trylock() 후 삭제
	if vm.State == StateRunning {
		return fmt.Errorf("VM '%s'이 실행 중이므로 삭제할 수 없습니다", name)
	}

	delete(mgr.vms, name)
	fmt.Printf("  VM '%s' 삭제 완료\n", name)
	return nil
}

// ---------------------------------------------------------------------------
// CloneCommand: VM 복제
//   실제 코드: Sources/tart/Commands/Clone.swift
//   VMDirectory.clone(to:, generateMAC:) — config, disk, nvram 복사
// ---------------------------------------------------------------------------
type CloneCommand struct{}

func (c *CloneCommand) Name() string { return "clone" }

func (c *CloneCommand) Execute(ctx context.Context, mgr *VMManager, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("사용법: clone <source> <destination>")
	}
	src, dst := args[0], args[1]

	mgr.mu.Lock()
	defer mgr.mu.Unlock()

	srcVM, exists := mgr.vms[src]
	if !exists {
		return fmt.Errorf("소스 VM '%s'이 존재하지 않습니다", src)
	}

	if _, exists := mgr.vms[dst]; exists {
		return fmt.Errorf("대상 VM '%s'이 이미 존재합니다", dst)
	}

	// 실제 tart: VMDirectory.clone(to:, generateMAC: true)
	cloned := &VM{
		Name:      dst,
		State:     srcVM.State,
		CPU:       srcVM.CPU,
		MemoryMB:  srcVM.MemoryMB,
		CreatedAt: time.Now(),
	}
	mgr.vms[dst] = cloned

	fmt.Printf("  VM '%s' → '%s' 복제 완료 (새 MAC 주소 생성됨)\n", src, dst)
	return nil
}

// ---------------------------------------------------------------------------
// gcTmpDir: 가비지 컬렉션 시뮬레이션
//   실제 코드: Sources/tart/Config.swift — func gc()
//   tartTmpDir 내 각 항목에 대해 FileLock.trylock() 시도,
//   잠금 가능하면(사용 중 아님) 삭제
// ---------------------------------------------------------------------------
func gcTmpDir(tmpDir string) {
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		fmt.Fprintf(os.Stderr, "GC 실패: %v\n", err)
		return
	}

	cleaned := 0
	for _, entry := range entries {
		path := filepath.Join(tmpDir, entry.Name())
		// 실제 tart: FileLock.trylock() — 잠금 가능하면 삭제
		// 여기서는 단순히 삭제 시뮬레이션
		os.RemoveAll(path)
		cleaned++
	}

	if cleaned > 0 {
		fmt.Printf("  [GC] 임시 디렉토리에서 %d개 항목 정리\n", cleaned)
	}
}

// ---------------------------------------------------------------------------
// 커맨드 디스패처: Root.swift의 main() 흐름 시뮬레이션
//   1. SIGINT 핸들러 등록 (signal(SIGINT, SIG_IGN) + DispatchSource)
//   2. 커맨드 파싱 (parseAsRoot())
//   3. OTel 루트 스팬 생성
//   4. GC 실행 (Pull/Clone 제외)
//   5. 커맨드 실행
//   6. 에러 처리 + OTel flush
// ---------------------------------------------------------------------------
func dispatch(ctx context.Context, mgr *VMManager, commands map[string]Command, input string) {
	parts := strings.Fields(input)
	if len(parts) == 0 {
		return
	}

	cmdName := parts[0]
	args := parts[1:]

	cmd, exists := commands[cmdName]
	if !exists {
		fmt.Printf("  알 수 없는 커맨드: '%s'\n", cmdName)
		fmt.Printf("  사용 가능: create, clone, run, stop, list, delete\n")
		return
	}

	// 1. 루트 스팬 생성 (실제: Root.swift line 62)
	span := globalTracer.StartSpan(cmdName)
	span.SetAttribute("command-line-arguments", input)
	defer span.End()

	// 2. GC 실행 — Pull/Clone 제외 (실제: Root.swift line 80)
	if cmdName != "clone" {
		tmpDir := filepath.Join(os.TempDir(), "tart-poc-tmp")
		os.MkdirAll(tmpDir, 0755)
		gcTmpDir(tmpDir)
	}

	// 3. 커맨드 실행 (실제: Root.swift line 89-93)
	if err := cmd.Execute(ctx, mgr, args); err != nil {
		span.RecordException(err)
		fmt.Fprintf(os.Stderr, "  에러: %v\n", err)
	}
}

// ---------------------------------------------------------------------------
// main: tart CLI 진입점 시뮬레이션
// ---------------------------------------------------------------------------
func main() {
	fmt.Println("=== tart CLI 커맨드 디스패치 & VM 라이프사이클 시뮬레이션 ===")
	fmt.Println()

	// SIGINT 처리 시뮬레이션
	// 실제 tart: signal(SIGINT, SIG_IGN) → DispatchSource.makeSignalSource(signal: SIGINT)
	//            → task.cancel()
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT)

	go func() {
		sig := <-sigCh
		fmt.Printf("\n[SIGINT] 시그널 수신: %v — 모든 작업 취소\n", sig)
		cancel()
	}()

	defer func() {
		// 실제 tart: defer { OTel.shared.flush() }
		globalTracer.Flush()
	}()

	// 커맨드 레지스트리 구성 (실제: Root.swift의 subcommands 배열)
	commands := map[string]Command{
		"create": &CreateCommand{},
		"clone":  &CloneCommand{},
		"run":    &RunCommand{},
		"stop":   &StopCommand{},
		"list":   &ListCommand{},
		"delete": &DeleteCommand{},
	}

	mgr := NewVMManager()

	// 시나리오 실행: 전체 VM 라이프사이클 데모
	scenarios := []string{
		"create macos-ventura",
		"create ubuntu-22",
		"list",
		"clone macos-ventura macos-ventura-clone",
		"list",
		"run macos-ventura",
		"list",
		"stop macos-ventura",
		"delete macos-ventura-clone",
		"list",
		"delete macos-ventura",
		"delete ubuntu-22",
		"list",
	}

	for i, scenario := range scenarios {
		select {
		case <-ctx.Done():
			fmt.Println("\n컨텍스트 취소됨 — 나머지 시나리오 건너뛰기")
			return
		default:
		}

		fmt.Printf("\n--- [%d] tart %s ---\n", i+1, scenario)
		dispatch(ctx, mgr, commands, scenario)
	}

	// 에러 시나리오 데모
	fmt.Println("\n\n=== 에러 처리 시나리오 ===")
	errorScenarios := []string{
		"create",           // 인자 부족
		"run nonexistent",  // 존재하지 않는 VM
		"unknown-cmd test", // 알 수 없는 커맨드
	}

	for _, scenario := range errorScenarios {
		fmt.Printf("\n--- tart %s ---\n", scenario)
		dispatch(ctx, mgr, commands, scenario)
	}

	fmt.Println("\n=== 시뮬레이션 완료 ===")
}
