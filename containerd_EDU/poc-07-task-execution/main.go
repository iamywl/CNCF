// containerd Task/Process 생명주기 시뮬레이션
//
// containerd에서 Task는 컨테이너 내부의 실행 단위이며,
// PlatformRuntime이 Task의 생성/조회/삭제를 관리한다.
// Task는 Process 인터페이스를 내장하며, Exec으로 추가 프로세스를 생성할 수 있다.
//
// 참조 소스코드:
//   - core/runtime/runtime.go  (PlatformRuntime, CreateOpts, Exit)
//   - core/runtime/task.go     (Task, Process, ExecProcess, State, Status)
//   - plugins/services/tasks/local.go (local 서비스 - Create, Start, Delete, Exec, Wait)

package main

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"
)

// ============================================================================
// 1. 프로세스 상태 정의
// 참조: core/runtime/task.go - Status 상수, State struct
// ============================================================================

// Status는 Task/Process의 런타임 상태를 나타낸다.
type Status int

const (
	CreatedStatus Status = iota + 1 // Created: 프로세스 생성됨 (아직 시작 안 됨)
	RunningStatus                   // Running: 프로세스 실행 중
	StoppedStatus                   // Stopped: 프로세스 종료됨
	DeletedStatus                   // Deleted: 프로세스 삭제됨
	PausedStatus                    // Paused: 프로세스 일시정지
	PausingStatus                   // Pausing: 일시정지 진행 중
)

func (s Status) String() string {
	switch s {
	case CreatedStatus:
		return "CREATED"
	case RunningStatus:
		return "RUNNING"
	case StoppedStatus:
		return "STOPPED"
	case DeletedStatus:
		return "DELETED"
	case PausedStatus:
		return "PAUSED"
	case PausingStatus:
		return "PAUSING"
	default:
		return "UNKNOWN"
	}
}

// State는 프로세스의 런타임 상태 정보.
// 참조: core/runtime/task.go - State struct
type State struct {
	Status     Status
	Pid        uint32
	ExitStatus uint32
	ExitedAt   time.Time
	Stdin      string
	Stdout     string
	Stderr     string
	Terminal   bool
}

// Exit은 프로세스 종료 정보.
// 참조: core/runtime/runtime.go - Exit struct
type Exit struct {
	Pid       uint32
	Status    uint32
	Timestamp time.Time
}

// IO는 프로세스의 I/O 정보.
// 참조: core/runtime/runtime.go - IO struct
type IO struct {
	Stdin    string
	Stdout   string
	Stderr   string
	Terminal bool
}

// ============================================================================
// 2. Process 인터페이스
// 참조: core/runtime/task.go - Process interface
// ============================================================================

// Process는 컨테이너 내 실행 중인 프로세스를 나타내는 인터페이스.
type Process interface {
	ID() string
	State(ctx context.Context) (State, error)
	Kill(ctx context.Context, signal uint32, all bool) error
	Start(ctx context.Context) error
	Wait(ctx context.Context) (*Exit, error)
}

// ExecProcess는 Task.Exec으로 생성된 추가 프로세스.
// 참조: core/runtime/task.go - ExecProcess interface
type ExecProcess interface {
	Process
	Delete(ctx context.Context) (*Exit, error)
}

// ============================================================================
// 3. Task 인터페이스
// 참조: core/runtime/task.go - Task interface
// ============================================================================

// Task는 컨테이너의 실행 단위이며 Process를 내장한다.
type Task interface {
	Process
	PID(ctx context.Context) (uint32, error)
	Namespace() string
	Pause(ctx context.Context) error
	Resume(ctx context.Context) error
	Exec(ctx context.Context, id string, opts ExecOpts) (ExecProcess, error)
	Process2(ctx context.Context, id string) (ExecProcess, error)
}

type ExecOpts struct {
	Spec []byte
	IO   IO
}

// ============================================================================
// 4. simProcess: Process 구현 (exec 프로세스)
// ============================================================================

type simProcess struct {
	id         string
	pid        uint32
	status     Status
	exitStatus uint32
	exitedAt   time.Time
	io         IO
	waitCh     chan struct{} // Wait 대기용 채널
	mu         sync.Mutex
}

func newSimProcess(id string, io IO) *simProcess {
	return &simProcess{
		id:     id,
		pid:    uint32(rand.Intn(90000) + 10000),
		status: CreatedStatus,
		io:     io,
		waitCh: make(chan struct{}),
	}
}

func (p *simProcess) ID() string { return p.id }

func (p *simProcess) State(_ context.Context) (State, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return State{
		Status:     p.status,
		Pid:        p.pid,
		ExitStatus: p.exitStatus,
		ExitedAt:   p.exitedAt,
		Stdin:      p.io.Stdin,
		Stdout:     p.io.Stdout,
		Stderr:     p.io.Stderr,
		Terminal:   p.io.Terminal,
	}, nil
}

func (p *simProcess) Kill(_ context.Context, signal uint32, _ bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.status != RunningStatus {
		return fmt.Errorf("process %s is not running (status=%s)", p.id, p.status)
	}
	fmt.Printf("      [프로세스] %s (pid=%d) 에 signal=%d 전송\n", p.id, p.pid, signal)
	// 비동기적으로 프로세스 종료 시뮬레이션
	go func() {
		time.Sleep(30 * time.Millisecond)
		p.mu.Lock()
		p.status = StoppedStatus
		p.exitStatus = 0
		p.exitedAt = time.Now()
		p.mu.Unlock()
		close(p.waitCh)
	}()
	return nil
}

func (p *simProcess) Start(_ context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.status != CreatedStatus {
		return fmt.Errorf("process %s cannot start from status %s", p.id, p.status)
	}
	p.status = RunningStatus
	return nil
}

// Wait는 프로세스 종료까지 블로킹한다.
// 참조: core/runtime/task.go - Process.Wait()
func (p *simProcess) Wait(_ context.Context) (*Exit, error) {
	<-p.waitCh
	p.mu.Lock()
	defer p.mu.Unlock()
	return &Exit{
		Pid:       p.pid,
		Status:    p.exitStatus,
		Timestamp: p.exitedAt,
	}, nil
}

// Delete는 exec 프로세스를 삭제한다.
func (p *simProcess) Delete(_ context.Context) (*Exit, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.status == RunningStatus {
		return nil, fmt.Errorf("process %s is still running", p.id)
	}
	return &Exit{
		Pid:       p.pid,
		Status:    p.exitStatus,
		Timestamp: p.exitedAt,
	}, nil
}

// ============================================================================
// 5. simTask: Task 구현 (컨테이너의 init 프로세스)
// ============================================================================

type simTask struct {
	simProcess // init 프로세스 내장
	namespace  string
	execs      map[string]*simProcess
	mu2        sync.Mutex
}

func newSimTask(id, namespace string, io IO) *simTask {
	return &simTask{
		simProcess: *newSimProcess(id, io),
		namespace:  namespace,
		execs:      make(map[string]*simProcess),
	}
}

func (t *simTask) PID(_ context.Context) (uint32, error) {
	return t.pid, nil
}

func (t *simTask) Namespace() string {
	return t.namespace
}

// Pause는 컨테이너를 일시정지한다.
func (t *simTask) Pause(_ context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.status != RunningStatus {
		return fmt.Errorf("task %s is not running", t.id)
	}
	t.status = PausedStatus
	return nil
}

// Resume는 일시정지된 컨테이너를 재개한다.
func (t *simTask) Resume(_ context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.status != PausedStatus {
		return fmt.Errorf("task %s is not paused", t.id)
	}
	t.status = RunningStatus
	return nil
}

// Exec는 실행 중인 Task에 추가 프로세스를 생성한다.
// 참조: core/runtime/task.go - Task.Exec()
// 참조: plugins/services/tasks/local.go - func (l *local) Exec(...)
func (t *simTask) Exec(_ context.Context, id string, opts ExecOpts) (ExecProcess, error) {
	t.mu2.Lock()
	defer t.mu2.Unlock()
	t.mu.Lock()
	status := t.status
	t.mu.Unlock()

	if status != RunningStatus {
		return nil, fmt.Errorf("task %s is not running, cannot exec", t.id)
	}
	if _, exists := t.execs[id]; exists {
		return nil, fmt.Errorf("exec %s already exists", id)
	}

	p := newSimProcess(id, opts.IO)
	t.execs[id] = p
	return p, nil
}

// Process2는 exec ID로 프로세스를 조회한다.
// 참조: core/runtime/task.go - Task.Process()
func (t *simTask) Process2(_ context.Context, id string) (ExecProcess, error) {
	t.mu2.Lock()
	defer t.mu2.Unlock()
	p, ok := t.execs[id]
	if !ok {
		return nil, fmt.Errorf("exec process %s not found", id)
	}
	return p, nil
}

// ============================================================================
// 6. PlatformRuntime: Task 생성/관리 인터페이스 구현
// 참조: core/runtime/runtime.go - PlatformRuntime interface
// 참조: plugins/services/tasks/local.go - local struct
// ============================================================================

// CreateOpts는 Task 생성 옵션.
// 참조: core/runtime/runtime.go - CreateOpts struct
type CreateOpts struct {
	Spec      []byte
	IO        IO
	Runtime   string
	SandboxID string
}

// PlatformRuntime은 Task의 CRUD를 담당하는 런타임 인터페이스.
type PlatformRuntime struct {
	id    string
	mu    sync.Mutex
	tasks map[string]*simTask
}

func NewPlatformRuntime(id string) *PlatformRuntime {
	return &PlatformRuntime{
		id:    id,
		tasks: make(map[string]*simTask),
	}
}

func (r *PlatformRuntime) ID() string { return r.id }

// Create는 새 Task를 생성한다.
// 참조: core/runtime/runtime.go - PlatformRuntime.Create()
// 참조: plugins/services/tasks/local.go - func (l *local) Create(...)
func (r *PlatformRuntime) Create(ctx context.Context, taskID string, opts CreateOpts) (Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.tasks[taskID]; exists {
		return nil, fmt.Errorf("task %s already exists", taskID)
	}

	task := newSimTask(taskID, "default", opts.IO)
	r.tasks[taskID] = task
	return task, nil
}

// Get은 taskID로 Task를 조회한다.
// 참조: core/runtime/runtime.go - PlatformRuntime.Get()
func (r *PlatformRuntime) Get(_ context.Context, taskID string) (Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	t, ok := r.tasks[taskID]
	if !ok {
		return nil, fmt.Errorf("task %s not found", taskID)
	}
	return t, nil
}

// Tasks는 모든 Task를 반환한다.
// 참조: core/runtime/runtime.go - PlatformRuntime.Tasks()
func (r *PlatformRuntime) Tasks(_ context.Context) ([]Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	tasks := make([]Task, 0, len(r.tasks))
	for _, t := range r.tasks {
		tasks = append(tasks, t)
	}
	return tasks, nil
}

// Delete는 Task를 삭제한다.
// 참조: core/runtime/runtime.go - PlatformRuntime.Delete()
// 참조: plugins/services/tasks/local.go - func (l *local) Delete(...)
func (r *PlatformRuntime) Delete(_ context.Context, taskID string) (*Exit, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	t, ok := r.tasks[taskID]
	if !ok {
		return nil, fmt.Errorf("task %s not found", taskID)
	}
	t.mu.Lock()
	if t.status == RunningStatus {
		t.mu.Unlock()
		return nil, fmt.Errorf("task %s is still running", taskID)
	}
	exit := &Exit{
		Pid:       t.pid,
		Status:    t.exitStatus,
		Timestamp: t.exitedAt,
	}
	t.mu.Unlock()

	delete(r.tasks, taskID)
	return exit, nil
}

// ============================================================================
// 7. 상태 출력 헬퍼
// ============================================================================

func printTaskState(ctx context.Context, label string, t Task) {
	state, err := t.State(ctx)
	if err != nil {
		fmt.Printf("    [%s] 에러: %v\n", label, err)
		return
	}
	pid, _ := t.PID(ctx)
	fmt.Printf("    [%s] Task=%s, PID=%d, Status=%s, ExitStatus=%d\n",
		label, t.ID(), pid, state.Status, state.ExitStatus)
}

func printProcessState(ctx context.Context, label string, p Process) {
	state, err := p.State(ctx)
	if err != nil {
		fmt.Printf("    [%s] 에러: %v\n", label, err)
		return
	}
	fmt.Printf("    [%s] Process=%s, PID=%d, Status=%s, ExitStatus=%d\n",
		label, p.ID(), state.Pid, state.Status, state.ExitStatus)
}

// ============================================================================
// 8. main: 전체 시나리오 실행
// ============================================================================

func main() {
	rand.Seed(time.Now().UnixNano())
	ctx := context.Background()

	fmt.Println("========================================")
	fmt.Println("containerd Task/Process 생명주기 시뮬레이션")
	fmt.Println("========================================")
	fmt.Println()

	runtime := NewPlatformRuntime("io.containerd.runtime.v2.task")
	fmt.Printf("  Runtime ID: %s\n\n", runtime.ID())

	// ----- 시나리오 1: Task 생명주기 (Create → Start → Kill → Delete) -----
	fmt.Println("--- 시나리오 1: Task 생명주기 ---")
	fmt.Println("    상태 전이: Created → Running → Stopped → (Delete)")
	fmt.Println()

	// Create: Task 생성 (init 프로세스 Created 상태)
	fmt.Println("  [1] Create Task:")
	task1, err := runtime.Create(ctx, "nginx-container", CreateOpts{
		IO: IO{
			Stdin:  "/proc/self/fd/0",
			Stdout: "/run/containerd/fifo/nginx-stdout",
			Stderr: "/run/containerd/fifo/nginx-stderr",
		},
		Runtime:   "io.containerd.runc.v2",
		SandboxID: "sandbox-001",
	})
	if err != nil {
		fmt.Printf("    에러: %v\n", err)
		return
	}
	printTaskState(ctx, "생성 후", task1)

	// Start: init 프로세스 실행 (Created → Running)
	fmt.Println()
	fmt.Println("  [2] Start Task:")
	if err := task1.Start(ctx); err != nil {
		fmt.Printf("    에러: %v\n", err)
		return
	}
	printTaskState(ctx, "시작 후", task1)

	// Pause/Resume
	fmt.Println()
	fmt.Println("  [3] Pause / Resume:")
	if err := task1.Pause(ctx); err != nil {
		fmt.Printf("    Pause 에러: %v\n", err)
	}
	printTaskState(ctx, "Pause 후", task1)

	if err := task1.Resume(ctx); err != nil {
		fmt.Printf("    Resume 에러: %v\n", err)
	}
	printTaskState(ctx, "Resume 후", task1)

	// Kill: 시그널 전송 (Running → Stopped)
	fmt.Println()
	fmt.Println("  [4] Kill Task (SIGTERM):")
	if err := task1.Kill(ctx, 15, false); err != nil {
		fmt.Printf("    에러: %v\n", err)
		return
	}
	time.Sleep(100 * time.Millisecond) // 종료 대기
	printTaskState(ctx, "Kill 후", task1)

	// Delete: Task 삭제
	fmt.Println()
	fmt.Println("  [5] Delete Task:")
	exit, err := runtime.Delete(ctx, "nginx-container")
	if err != nil {
		fmt.Printf("    에러: %v\n", err)
		return
	}
	fmt.Printf("    삭제 완료: pid=%d, exitStatus=%d\n", exit.Pid, exit.Status)

	// Tasks 조회
	tasks, _ := runtime.Tasks(ctx)
	fmt.Printf("    현재 Task 수: %d\n", len(tasks))

	// ----- 시나리오 2: Exec - 실행 중인 Task에 추가 프로세스 -----
	fmt.Println()
	fmt.Println("--- 시나리오 2: Exec (추가 프로세스 실행) ---")
	fmt.Println()

	// 새 Task 생성 및 시작
	task2, _ := runtime.Create(ctx, "app-server", CreateOpts{
		IO: IO{Stdout: "/run/containerd/fifo/app-stdout"},
	})
	task2.Start(ctx)
	printTaskState(ctx, "Task 시작", task2)

	// Exec: 추가 프로세스 생성 (Created 상태)
	fmt.Println()
	fmt.Println("  [1] Exec - shell 프로세스 추가:")
	execProc, err := task2.Exec(ctx, "exec-shell", ExecOpts{
		IO: IO{
			Stdin:  "/proc/self/fd/0",
			Stdout: "/run/containerd/fifo/exec-stdout",
			Stderr: "/run/containerd/fifo/exec-stderr",
		},
	})
	if err != nil {
		fmt.Printf("    에러: %v\n", err)
		return
	}
	printProcessState(ctx, "Exec 생성 후", execProc)

	// Exec Start
	fmt.Println()
	fmt.Println("  [2] Exec Start:")
	if err := execProc.Start(ctx); err != nil {
		fmt.Printf("    에러: %v\n", err)
		return
	}
	printProcessState(ctx, "Exec 시작 후", execProc)

	// Exec Kill
	fmt.Println()
	fmt.Println("  [3] Exec Kill:")
	if err := execProc.Kill(ctx, 9, false); err != nil {
		fmt.Printf("    에러: %v\n", err)
		return
	}
	time.Sleep(100 * time.Millisecond)
	printProcessState(ctx, "Exec Kill 후", execProc)

	// Exec Delete
	fmt.Println()
	fmt.Println("  [4] Exec Delete:")
	execExit, err := execProc.Delete(ctx)
	if err != nil {
		fmt.Printf("    에러: %v\n", err)
		return
	}
	fmt.Printf("    Exec 삭제: pid=%d, exitStatus=%d\n", execExit.Pid, execExit.Status)

	// Process2로 조회
	fmt.Println()
	fmt.Println("  [5] Process2로 exec 조회:")
	p2, err := task2.Process2(ctx, "exec-shell")
	if err != nil {
		fmt.Printf("    에러: %v\n", err)
	} else {
		printProcessState(ctx, "조회 결과", p2)
	}

	// ----- 시나리오 3: Wait로 Exit 코드 수집 -----
	fmt.Println()
	fmt.Println("--- 시나리오 3: Wait으로 Exit 코드 수집 ---")
	fmt.Println()

	task3, _ := runtime.Create(ctx, "worker-task", CreateOpts{
		IO: IO{Stdout: "/run/containerd/fifo/worker-stdout"},
	})
	task3.Start(ctx)
	printTaskState(ctx, "시작", task3)

	// goroutine으로 Wait 호출 (블로킹)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		fmt.Println("    [Wait] 프로세스 종료 대기 중...")
		exit, err := task3.Wait(ctx)
		if err != nil {
			fmt.Printf("    [Wait] 에러: %v\n", err)
			return
		}
		fmt.Printf("    [Wait] 종료 감지: pid=%d, exitStatus=%d, exitedAt=%s\n",
			exit.Pid, exit.Status, exit.Timestamp.Format("15:04:05.000"))
	}()

	// 잠시 후 Kill
	time.Sleep(100 * time.Millisecond)
	fmt.Println("    [Main] 1초 후 Kill 전송...")
	task3.Kill(ctx, 15, false)

	// Wait 완료 대기
	wg.Wait()

	// ----- 시나리오 4: 여러 Task 동시 관리 -----
	fmt.Println()
	fmt.Println("--- 시나리오 4: 여러 Task 동시 관리 ---")
	fmt.Println()

	taskIDs := []string{"redis-task", "postgres-task", "memcached-task"}
	for _, id := range taskIDs {
		t, _ := runtime.Create(ctx, id, CreateOpts{
			IO: IO{Stdout: fmt.Sprintf("/run/containerd/fifo/%s-stdout", id)},
		})
		t.Start(ctx)
	}

	allTasks, _ := runtime.Tasks(ctx)
	fmt.Printf("  현재 실행 중인 Task 목록 (%d개):\n", len(allTasks))
	for _, t := range allTasks {
		state, _ := t.State(ctx)
		pid, _ := t.PID(ctx)
		fmt.Printf("    - %-20s PID=%-6d Status=%s  Namespace=%s\n",
			t.ID(), pid, state.Status, t.Namespace())
	}

	// ----- 상태 전이 다이어그램 -----
	fmt.Println()
	fmt.Println("--- Task/Process 상태 전이 다이어그램 ---")
	fmt.Println()
	fmt.Println("  ┌─────────┐  Start()  ┌─────────┐  Kill()   ┌─────────┐")
	fmt.Println("  │ CREATED │ ────────► │ RUNNING │ ────────► │ STOPPED │")
	fmt.Println("  └─────────┘           └────┬────┘           └─────────┘")
	fmt.Println("                              │                     │")
	fmt.Println("                     Pause()  │  Resume()           │ Delete()")
	fmt.Println("                              ▼                     ▼")
	fmt.Println("                         ┌────────┐           ┌─────────┐")
	fmt.Println("                         │ PAUSED │           │ DELETED │")
	fmt.Println("                         └────────┘           └─────────┘")
	fmt.Println()
	fmt.Println("  Task = init 프로세스 (컨테이너당 1개)")
	fmt.Println("  Exec = 추가 프로세스 (Task.Exec으로 생성, 여러 개 가능)")
	fmt.Println("  Wait = 프로세스 종료까지 블로킹, Exit 코드 반환")

	fmt.Println()
	fmt.Println("========================================")
	fmt.Println("시뮬레이션 완료")
	fmt.Println("========================================")
}
