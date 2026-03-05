# 12. 태스크 실행 시스템 Deep-Dive

## 목차

1. [개요](#1-개요)
2. [핵심 인터페이스 계층](#2-핵심-인터페이스-계층)
3. [프로세스 상태 머신](#3-프로세스-상태-머신)
4. [2-Layer 서비스 아키텍처](#4-2-layer-서비스-아키텍처)
5. [TaskManager (런타임 계층)](#5-taskmanager-런타임-계층)
6. [Tasks Service (API 계층)](#6-tasks-service-api-계층)
7. [shimTask - 런타임 브릿지](#7-shimtask---런타임-브릿지)
8. [Exec - 컨테이너 내 추가 프로세스](#8-exec---컨테이너-내-추가-프로세스)
9. [Wait와 프로세스 종료 처리](#9-wait와-프로세스-종료-처리)
10. [Checkpoint/Restore](#10-checkpointrestore)
11. [모니터링과 메트릭](#11-모니터링과-메트릭)
12. [설계 철학과 핵심 교훈](#12-설계-철학과-핵심-교훈)

---

## 1. 개요

containerd에서 **Task**는 **실행 중인 컨테이너의 상태**를 나타낸다. Container(메타데이터)와
Task(실행 상태)는 명확히 분리된 개념이다:

```
Container (메타데이터)                Task (실행 상태)
┌─────────────────────┐              ┌─────────────────────┐
│ ID: "my-container"  │              │ ID: "my-container"  │
│ Image: "nginx"      │  ──Create──► │ PID: 12345          │
│ Spec: OCI runtime   │              │ Status: Running     │
│ Runtime: "runc.v2"  │              │ IO: stdin/out/err   │
│ Snapshotter: "ovl"  │              │ Processes: [exec1]  │
└─────────────────────┘              └─────────────────────┘
     정적, 영구 저장                      동적, 프로세스 생명주기
```

**왜 분리하는가?**

1. **생명주기 독립**: Container는 영구 메타데이터, Task는 일시적 실행 상태
2. **재시작 가능**: Container가 존재하면 Task를 여러 번 생성/삭제 가능
3. **관심사 분리**: 메타데이터 관리와 프로세스 관리를 독립적으로 진화 가능
4. **하나의 Container = 최대 하나의 Task**: 동시에 여러 Task 불가

### 태스크 실행의 전체 계층

```
┌─────────────────────────────────────────────────────┐
│                  gRPC/TTRPC API                     │  ← 외부 클라이언트
├─────────────────────────────────────────────────────┤
│             Tasks Service (local)                   │  ← API 계층
│   plugins/services/tasks/local.go                   │     (인증, 직렬화, 이벤트)
├─────────────────────────────────────────────────────┤
│            TaskManager (v2 runtime)                 │  ← 런타임 계층
│   core/runtime/v2/task_manager.go                   │     (Bundle, Shim 관리)
├─────────────────────────────────────────────────────┤
│              ShimManager                            │  ← Shim 관리
│   core/runtime/v2/shim_manager.go                   │     (프로세스 시작/중지)
├─────────────────────────────────────────────────────┤
│         shimTask (runtime.Task 구현)                │  ← 프로토콜 브릿지
│   core/runtime/v2/shim.go                           │     (TTRPC/gRPC ↔ Go 인터페이스)
├─────────────────────────────────────────────────────┤
│      containerd-shim-runc-v2 (별도 프로세스)        │  ← 실제 컨테이너 관리
│   cmd/containerd-shim-runc-v2/                      │     (runc 호출, 프로세스 관리)
└─────────────────────────────────────────────────────┘
```

### 핵심 소스 파일

| 파일 | 역할 |
|------|------|
| `core/runtime/runtime.go` | PlatformRuntime, CreateOpts, Exit, IO |
| `core/runtime/task.go` | Process, ExecProcess, Task 인터페이스, Status 열거형 |
| `core/runtime/v2/task_manager.go` | TaskManager (PlatformRuntime 구현) |
| `core/runtime/v2/shim_manager.go` | ShimManager (shim 프로세스 관리) |
| `core/runtime/v2/shim.go` | shimTask (runtime.Task 구현), shim 연결 |
| `core/runtime/v2/process.go` | process (exec 프로세스 구현) |
| `plugins/services/tasks/local.go` | Tasks API 서비스 (local 구현) |

---

## 2. 핵심 인터페이스 계층

### 2.1 Process 인터페이스

모든 실행 단위의 기본 인터페이스:

```go
// 소스: core/runtime/task.go

type Process interface {
    ID() string
    State(ctx context.Context) (State, error)
    Kill(ctx context.Context, signal uint32, all bool) error
    ResizePty(ctx context.Context, size ConsoleSize) error
    CloseIO(ctx context.Context) error
    Start(ctx context.Context) error
    Wait(ctx context.Context) (*Exit, error)
}
```

Process는 "실행 가능한 것"의 최소 인터페이스이다. Task의 init process든 exec로 추가된
process든 동일한 인터페이스를 구현한다.

### 2.2 ExecProcess 인터페이스

```go
// 소스: core/runtime/task.go

type ExecProcess interface {
    Process
    Delete(ctx context.Context) (*Exit, error)
}
```

ExecProcess는 Process에 `Delete()` 를 추가한다.

**왜 Process와 ExecProcess를 분리하는가?**

- init process (Task 자체): Task 전체의 삭제가 필요 → TaskManager.Delete()를 통해
- exec process: 자기 자신만 삭제 가능 → `ExecProcess.Delete()` 직접 호출

이 분리는 삭제 경로의 복잡도 차이를 반영한다:
- exec 삭제: 단순히 프로세스 정보만 정리
- task 삭제: shim 프로세스 종료, bundle 정리, mount 해제 등

### 2.3 Task 인터페이스

```go
// 소스: core/runtime/task.go

type Task interface {
    Process                                    // 기본 프로세스 기능

    PID(ctx context.Context) (uint32, error)   // 컨테이너의 init PID
    Namespace() string                         // 네임스페이스
    Pause(ctx context.Context) error           // cgroup freezer로 일시 정지
    Resume(ctx context.Context) error          // 일시 정지 해제
    Exec(ctx context.Context, id string, opts ExecOpts) (ExecProcess, error)  // exec 추가
    Pids(ctx context.Context) ([]ProcessInfo, error)   // 모든 PID 목록
    Checkpoint(ctx context.Context, path string, opts *types.Any) error  // 체크포인트
    Update(ctx context.Context, resources *types.Any, annotations map[string]string) error  // 리소스 업데이트
    Process(ctx context.Context, id string) (ExecProcess, error)  // exec 프로세스 조회
    Stats(ctx context.Context) (*types.Any, error)   // 메트릭 수집
}
```

인터페이스 상속 관계:

```
Process
├── ID(), State(), Kill(), ResizePty(), CloseIO(), Start(), Wait()
│
├── ExecProcess (extends Process)
│   └── Delete()
│
└── Task (extends Process)
    └── PID(), Namespace(), Pause(), Resume(), Exec(),
        Pids(), Checkpoint(), Update(), Process(), Stats()
```

### 2.4 PlatformRuntime 인터페이스

```go
// 소스: core/runtime/runtime.go

type PlatformRuntime interface {
    ID() string
    Create(ctx context.Context, taskID string, opts CreateOpts) (Task, error)
    Get(ctx context.Context, taskID string) (Task, error)
    Tasks(ctx context.Context, all bool) ([]Task, error)
    Delete(ctx context.Context, taskID string) (*Exit, error)
}
```

PlatformRuntime은 **Task의 생명주기 관리자**이다. containerd에서는 `TaskManager`가
이 인터페이스를 구현한다.

### 2.5 주요 데이터 구조

```go
// 소스: core/runtime/runtime.go

type CreateOpts struct {
    Spec            typeurl.Any    // OCI 런타임 스펙
    Rootfs          []mount.Mount  // 루트 파일시스템 마운트
    IO              IO             // stdin/stdout/stderr
    Checkpoint      string         // 체크포인트 경로
    RestoreFromPath bool           // 로컬 체크포인트 복원 여부
    RuntimeOptions  typeurl.Any    // 런타임별 옵션
    TaskOptions     typeurl.Any    // 태스크별 옵션
    Runtime         string         // 런타임 이름 (io.containerd.runc.v2)
    SandboxID       string         // 샌드박스 ID (Pod 그룹핑)
    Address         string         // Task API 서버 주소
    Version         uint32         // Task API 버전
}

type IO struct {
    Stdin    string   // FIFO 경로
    Stdout   string   // FIFO 경로
    Stderr   string   // FIFO 경로
    Terminal bool     // PTY 사용 여부
}

type Exit struct {
    Pid       uint32
    Status    uint32
    Timestamp time.Time
}
```

---

## 3. 프로세스 상태 머신

### 3.1 Status 열거형

```go
// 소스: core/runtime/task.go

type Status int

const (
    CreatedStatus  Status = iota + 1  // 1: 생성됨 (아직 Start 안 됨)
    RunningStatus                     // 2: 실행 중
    StoppedStatus                     // 3: 종료됨
    DeletedStatus                     // 4: 삭제됨
    PausedStatus                      // 5: 일시 정지됨
    PausingStatus                     // 6: 일시 정지 중 (전환 중)
)
```

### 3.2 상태 전이 다이어그램

```
                              ┌─────────┐
                    Create()  │         │
               ┌─────────────►│ Created │
               │              │         │
               │              └────┬────┘
               │                   │ Start()
               │                   ▼
               │              ┌─────────┐         ┌──────────┐
               │              │         │ Pause() │          │
               │              │ Running ├────────►│ Pausing  │
               │              │         │         │          │
               │              └────┬────┘         └────┬─────┘
               │                   │                   │
               │                   │               (완료)
               │                   │                   │
               │                   │              ┌────▼─────┐
               │                   │              │          │
               │                   │              │  Paused  │
               │                   │              │          │
               │                   │              └────┬─────┘
               │                   │                   │ Resume()
               │                   │◄──────────────────┘
               │                   │
               │              Kill() / 프로세스 종료
               │                   │
               │              ┌────▼─────┐
               │              │          │
               │              │ Stopped  │
               │              │          │
               │              └────┬─────┘
               │                   │ Delete()
               │              ┌────▼─────┐
               │              │          │
               └──────────────│ Deleted  │
                              │          │
                              └──────────┘
```

### 3.3 State 구조체

```go
// 소스: core/runtime/task.go

type State struct {
    Status     Status       // 현재 상태
    Pid        uint32       // 메인 프로세스 PID
    ExitStatus uint32       // 종료 코드 (Stopped일 때만 유효)
    ExitedAt   time.Time    // 종료 시간 (Stopped일 때만 유효)
    Stdin      string       // IO 경로
    Stdout     string
    Stderr     string
    Terminal   bool
}
```

### 3.4 상태 변환 매핑

Tasks Service에서 내부 Status를 gRPC 프로토콜의 Status로 변환:

```go
// 소스: plugins/services/tasks/local.go

func getProcessState(ctx context.Context, p runtime.Process) (*task.Process, error) {
    ctx, cancel := timeout.WithContext(ctx, stateTimeout)  // 2초 타임아웃
    defer cancel()

    state, err := p.State(ctx)
    // ...
    status := task.Status_UNKNOWN
    switch state.Status {
    case runtime.CreatedStatus:  status = task.Status_CREATED
    case runtime.RunningStatus:  status = task.Status_RUNNING
    case runtime.StoppedStatus:  status = task.Status_STOPPED
    case runtime.PausedStatus:   status = task.Status_PAUSED
    case runtime.PausingStatus:  status = task.Status_PAUSING
    default:
        log.G(ctx).WithField("status", state.Status).Warn("unknown status")
    }
    // ...
}
```

`stateTimeout`은 2초로 설정된다. shim이 응답하지 않는 경우 무한 대기를 방지한다.
에러 처리에서 `IsNotFound`, `IsUnavailable`, `IsDeadlineExceeded`는 특별히 처리된다:
- shim이 이미 종료된 경우 (NotFound)
- shim과의 연결이 끊긴 경우 (Unavailable)
- 타임아웃 (DeadlineExceeded)

---

## 4. 2-Layer 서비스 아키텍처

containerd의 Task 처리는 **2개의 서비스 계층**으로 구성된다.

### 4.1 전체 구조

```
┌───────────────────────────────────────────────────────────┐
│                     gRPC/TTRPC API                        │
│              (TasksClient 인터페이스)                      │
└──────────────────────────┬────────────────────────────────┘
                           │
┌──────────────────────────▼────────────────────────────────┐
│              Tasks Service (API 계층)                     │
│                                                           │
│  local struct {                                           │
│    containers  containers.Store    // 컨테이너 메타데이터  │
│    store       content.Store       // 체크포인트 저장      │
│    publisher   events.Publisher    // 이벤트 발행          │
│    monitor     runtime.TaskMonitor // 상태 모니터링        │
│    v2Runtime   runtime.PlatformRuntime  // 런타임         │
│  }                                                        │
│                                                           │
│  역할:                                                    │
│  - 컨테이너 메타데이터 조회                                │
│  - gRPC 요청/응답 변환                                    │
│  - 에러 변환 (errgrpc.ToGRPC)                             │
│  - Task Monitor 등록/해제                                 │
│  - 체크포인트 이미지 저장                                  │
└──────────────────────────┬────────────────────────────────┘
                           │
┌──────────────────────────▼────────────────────────────────┐
│             TaskManager (런타임 계층)                      │
│                                                           │
│  TaskManager struct {                                     │
│    root    string          // 영구 데이터 경로             │
│    state   string          // 임시 상태 경로               │
│    manager *ShimManager    // Shim 프로세스 관리           │
│    mounts  mount.Manager   // 마운트 관리                  │
│  }                                                        │
│                                                           │
│  역할:                                                    │
│  - Bundle 생성/관리                                       │
│  - Shim 프로세스 시작/종료                                 │
│  - Rootfs 마운트 활성화/비활성화                           │
│  - 런타임 기능 검증                                       │
└───────────────────────────────────────────────────────────┘
```

### 4.2 왜 2-Layer인가?

| 관심사 | API 계층 (local) | 런타임 계층 (TaskManager) |
|--------|------------------|-------------------------|
| 프로토콜 | gRPC/protobuf 변환 | 없음 (Go 인터페이스) |
| 메타데이터 | Container Store 조회 | Bundle 관리 |
| 이벤트 | events.Publisher | 없음 |
| 모니터링 | TaskMonitor 등록 | 없음 |
| Shim 관리 | 없음 | ShimManager 위임 |
| 에러 변환 | errgrpc.ToGRPC | 원시 Go error |

이 분리 덕분에:
- 런타임 계층을 gRPC 없이 테스트 가능
- 새로운 API 프로토콜 추가 시 런타임 계층 변경 불필요
- 런타임 구현을 교체해도 API 계층 변경 불필요

---

## 5. TaskManager (런타임 계층)

### 5.1 플러그인 등록

```go
// 소스: core/runtime/v2/task_manager.go

func init() {
    registry.Register(&plugin.Registration{
        Type: plugins.RuntimePluginV2,
        ID:   "task",
        Requires: []plugin.Type{
            plugins.ShimPlugin,
            plugins.MountManagerPlugin,
            plugins.WarningPlugin,
        },
        Config: &TaskConfig{
            Platforms: defaultPlatforms(),
        },
        InitFn: func(ic *plugin.InitContext) (interface{}, error) {
            // ...
            shimManager := shimManagerI.(*ShimManager)
            shimManager.LoadExistingShims(ic.Context, state, root)

            return &TaskManager{
                root:    root,
                state:   state,
                manager: shimManager,
                mounts:  mounts,
            }, nil
        },
    })
}
```

TaskManager는 `RuntimePluginV2` 타입, ID `"task"`로 등록된다.
초기화 시 `LoadExistingShims()`로 이전 실행에서 살아남은 shim 프로세스를 복원한다.

### 5.2 TaskManager 구조체

```go
// 소스: core/runtime/v2/task_manager.go

type TaskManager struct {
    root    string           // 영구 저장 경로 (번들의 spec 등)
    state   string           // 임시 상태 경로 (shim 소켓, PID 등)
    manager *ShimManager     // shim 프로세스 관리
    mounts  mount.Manager    // 마운트 관리
}
```

### 5.3 Create 흐름

```go
// 소스: core/runtime/v2/task_manager.go

func (m *TaskManager) Create(ctx context.Context, taskID string, opts runtime.CreateOpts) (_ runtime.Task, retErr error) {
    // 1. Bundle 생성
    bundle, err := NewBundle(ctx, m.root, m.state, taskID, opts.Spec)
    defer func() {
        if retErr != nil { bundle.Delete() }
    }()

    // 2. Rootfs 마운트 활성화
    ai, err := m.mounts.Activate(ctx, taskID, opts.Rootfs, activateOpts...)
    if err == nil {
        opts.Rootfs = ai.System
        defer func() {
            if retErr != nil { m.mounts.Deactivate(dctx, taskID) }
        }()
    }

    // 3. Shim 시작
    shim, err := m.manager.Start(ctx, taskID, bundle, opts)

    // 4. shimTask 생성 및 Create 호출
    shimTask, err := newShimTask(shim)
    t, err := shimTask.Create(ctx, opts)

    return t, nil
}
```

상세 흐름:

```
TaskManager.Create()
│
├─ 1. NewBundle()
│    ├─ root/{ns}/{id}/   → 영구 데이터 (spec.json)
│    └─ state/{ns}/{id}/  → 임시 데이터 (shim 상태)
│
├─ 2. mounts.Activate()
│    └─ Rootfs 마운트 준비 (overlayfs 등)
│
├─ 3. manager.Start()  (ShimManager)
│    ├─ sandboxID 있으면 → 기존 shim 재사용 시도
│    │    └─ loadShim() → bootstrap.json으로 연결
│    └─ 없으면 → startShim()
│         ├─ resolveRuntimePath() → 바이너리 경로
│         ├─ shimBinary() → binary 구조체 생성
│         └─ binary.Start() → shim 프로세스 시작
│              ├─ cmd.Start() → 프로세스 fork
│              ├─ stdout 읽기 → BootstrapParams
│              └─ makeConnection() → TTRPC/gRPC 연결
│
├─ 4. newShimTask(shim)
│    └─ ShimInstance → shimTask (runtime.Task 구현)
│
├─ 5. shimTask.Create(ctx, opts)
│    └─ TTRPC: task.Create → shim 프로세스에 컨테이너 생성 요청
│
└─ 6. 실패 시 정리
     ├─ shims.Delete()
     ├─ shimTask.delete()
     ├─ shimTask.Shutdown()
     └─ shimTask.Close()
```

### 5.4 Delete 흐름

```go
// 소스: core/runtime/v2/task_manager.go

func (m *TaskManager) Delete(ctx context.Context, taskID string) (*runtime.Exit, error) {
    shim, err := m.manager.shims.Get(ctx, taskID)
    container, err := m.manager.containers.Get(ctx, taskID)
    shimTask, err := newShimTask(shim)

    sandboxed := container.SandboxID != ""
    exit, err := shimTask.delete(ctx, sandboxed, func(ctx context.Context, id string) {
        m.manager.shims.Delete(ctx, id)  // shim 목록에서 제거
    })

    m.mounts.Deactivate(ctx, taskID)  // 마운트 해제

    return exit, nil
}
```

### 5.5 shimTask.delete() 상세

```go
// 소스: core/runtime/v2/shim.go

func (s *shimTask) delete(ctx context.Context, sandboxed bool,
    removeTask func(ctx context.Context, id string)) (*runtime.Exit, error) {

    // 1. shim에 Delete 요청
    response, shimErr := s.task.Delete(ctx, &task.DeleteRequest{ID: s.ID()})

    // 2. 성공하면 task 목록에서 제거
    if shimErr == nil {
        removeTask(ctx, s.ID())
    }

    // 3. 샌드박스가 아니면 shim 종료
    if !sandboxed {
        s.waitShutdown(ctx)  // Shutdown 요청 + 3초 타임아웃
    }

    // 4. 연결 종료 + bundle 삭제
    s.ShimInstance.Delete(ctx)

    // 5. 안전을 위해 한 번 더 제거
    removeTask(ctx, s.ID())

    return &runtime.Exit{
        Status:    response.ExitStatus,
        Timestamp: protobuf.FromTimestamp(response.ExitedAt),
        Pid:       response.Pid,
    }, nil
}
```

**sandboxed 여부가 중요한 이유**: Pod 환경에서 하나의 shim이 여러 컨테이너를 관리하므로,
개별 컨테이너 삭제 시 shim을 종료하면 안 된다. sandbox controller가 최종적으로
shim 종료를 결정한다.

### 5.6 Get과 Tasks

```go
// 소스: core/runtime/v2/task_manager.go

func (m *TaskManager) Get(ctx context.Context, id string) (runtime.Task, error) {
    shim, err := m.manager.shims.Get(ctx, id)
    return newShimTask(shim)
}

func (m *TaskManager) Tasks(ctx context.Context, all bool) ([]runtime.Task, error) {
    shims, err := m.manager.shims.GetAll(ctx, all)
    out := make([]runtime.Task, len(shims))
    for i := range shims {
        out[i], _ = newShimTask(shims[i])
    }
    return out, nil
}
```

`GetAll(ctx, all)`: `all=false`이면 현재 네임스페이스의 task만, `all=true`이면 전체.

---

## 6. Tasks Service (API 계층)

### 6.1 플러그인 등록과 의존성

```go
// 소스: plugins/services/tasks/local.go

var tasksServiceRequires = []plugin.Type{
    plugins.EventPlugin,
    plugins.RuntimePluginV2,
    plugins.MetadataPlugin,
    plugins.TaskMonitorPlugin,
}

func init() {
    registry.Register(&plugin.Registration{
        Type:     plugins.ServicePlugin,
        ID:       services.TasksService,
        Requires: tasksServiceRequires,
        Config:   &Config{},
        InitFn:   initFunc,
    })
    timeout.Set(stateTimeout, 2*time.Second)
}
```

### 6.2 initFunc와 초기화

```go
// 소스: plugins/services/tasks/local.go

func initFunc(ic *plugin.InitContext) (interface{}, error) {
    v2r, err := ic.GetByID(plugins.RuntimePluginV2, "task")
    m, err := ic.GetSingle(plugins.MetadataPlugin)
    ep, err := ic.GetSingle(plugins.EventPlugin)

    monitor, err := ic.GetSingle(plugins.TaskMonitorPlugin)
    if err != nil {
        if !errors.Is(err, plugin.ErrPluginNotFound) {
            return nil, err
        }
        monitor = runtime.NewNoopMonitor()  // 모니터 없으면 no-op
    }

    db := m.(*metadata.DB)
    l := &local{
        containers: metadata.NewContainerStore(db),
        store:      db.ContentStore(),
        publisher:  ep.(events.Publisher),
        monitor:    monitor.(runtime.TaskMonitor),
        v2Runtime:  v2r.(runtime.PlatformRuntime),
    }

    // 기존 실행 중인 태스크를 모니터에 등록
    v2Tasks, err := l.v2Runtime.Tasks(ic.Context, true)
    for _, t := range v2Tasks {
        l.monitor.Monitor(t, nil)
    }

    // BlockIO, RDT 설정 (Linux 전용)
    blockio.SetConfig(config.BlockIOConfigFile)
    rdt.SetConfig(config.RdtConfigFile)

    return l, nil
}
```

**모니터 복원이 중요한 이유**: containerd가 재시작되면 기존 Task의 모니터링이 사라진다.
initFunc에서 기존 Task를 모니터에 재등록하여 종료 이벤트를 놓치지 않도록 한다.

### 6.3 local 구조체

```go
// 소스: plugins/services/tasks/local.go

type local struct {
    containers containers.Store        // 컨테이너 메타데이터
    store      content.Store           // 체크포인트 저장용
    publisher  events.Publisher        // 이벤트 발행
    monitor    runtime.TaskMonitor     // 상태 모니터링
    v2Runtime  runtime.PlatformRuntime // TaskManager
}
```

`local`은 `api.TasksClient` 인터페이스를 구현한다:

```go
var _ = (api.TasksClient)(&local{})
```

### 6.4 Create API

```go
// 소스: plugins/services/tasks/local.go

func (l *local) Create(ctx context.Context, r *api.CreateTaskRequest, _ ...grpc.CallOption) (*api.CreateTaskResponse, error) {
    // 1. 컨테이너 메타데이터 조회
    container, err := l.getContainer(ctx, r.ContainerID)

    // 2. 체크포인트 처리
    if r.Options != nil {
        taskOptions, err := formatOptions(container.Runtime.Name, r.Options)
        checkpointPath = taskOptions.CriuImagePath
        taskAPIAddress = taskOptions.TaskApiAddress
        taskAPIVersion = taskOptions.TaskApiVersion
    }

    // 3. 체크포인트 이미지에서 복원 경로 추출
    if checkpointPath == "" && r.Checkpoint != nil {
        // Content Store에서 체크포인트 아카이브 추출
        reader, _ := l.store.ReaderAt(ctx, checkpointDesc)
        archive.Apply(ctx, checkpointPath, content.NewReader(reader))
    }

    // 4. CreateOpts 조립
    opts := runtime.CreateOpts{
        Spec:       container.Spec,
        IO:         runtime.IO{Stdin: r.Stdin, Stdout: r.Stdout, ...},
        Checkpoint: checkpointPath,
        Runtime:    container.Runtime.Name,
        SandboxID:  container.SandboxID,
        // ...
    }

    // 5. 중복 검사
    _, err = rtime.Get(ctx, r.ContainerID)
    if err == nil {
        return nil, errgrpc.ToGRPC(errdefs.ErrAlreadyExists)
    }

    // 6. 런타임에 Create 위임
    c, err := rtime.Create(ctx, r.ContainerID, opts)

    // 7. 모니터 등록
    l.monitor.Monitor(c, map[string]string{"runtime": container.Runtime.Name})

    pid, _ := c.PID(ctx)
    return &api.CreateTaskResponse{ContainerID: r.ContainerID, Pid: pid}, nil
}
```

API 계층의 역할이 명확하게 드러난다:
1. 메타데이터 조회 (Container Store)
2. 프로토콜 변환 (protobuf → Go 구조체)
3. 중복 검사
4. 런타임에 위임
5. 모니터 등록
6. 에러 변환 (Go error → gRPC error)

### 6.5 Start API

```go
// 소스: plugins/services/tasks/local.go

func (l *local) Start(ctx context.Context, r *api.StartRequest, _ ...grpc.CallOption) (*api.StartResponse, error) {
    t, err := l.getTask(ctx, r.ContainerID)

    p := runtime.Process(t)          // 기본: init process
    if r.ExecID != "" {              // ExecID가 있으면: exec process
        p, _ = t.Process(ctx, r.ExecID)
    }

    p.Start(ctx)

    state, _ := p.State(ctx)
    return &api.StartResponse{Pid: state.Pid}, nil
}
```

**init process vs exec process의 통합 처리**: ExecID가 비어있으면 init process(Task 자체),
ExecID가 있으면 해당 exec process를 Start한다. 동일한 코드 경로로 두 케이스를 처리한다.

### 6.6 Delete API

```go
// 소스: plugins/services/tasks/local.go

func (l *local) Delete(ctx context.Context, r *api.DeleteTaskRequest, _ ...grpc.CallOption) (*api.DeleteResponse, error) {
    container, err := l.getContainer(ctx, r.ContainerID)
    t, err := l.v2Runtime.Get(ctx, container.ID)

    // 모니터 중지 (먼저!)
    l.monitor.Stop(t)

    // 런타임에 삭제 위임
    exit, err := l.v2Runtime.Delete(ctx, r.ContainerID)

    return &api.DeleteResponse{
        ExitStatus: exit.Status,
        ExitedAt:   protobuf.ToTimestamp(exit.Timestamp),
        Pid:        exit.Pid,
    }, nil
}
```

**모니터를 먼저 중지하는 이유**: 삭제 과정에서 발생하는 이벤트가 모니터에 의해
중복 처리되는 것을 방지한다.

---

## 7. shimTask - 런타임 브릿지

### 7.1 shimTask 구조

```go
// 소스: core/runtime/v2/shim.go

type shimTask struct {
    ShimInstance                    // shim 프로세스 정보
    task TaskServiceClient          // TTRPC/gRPC 클라이언트
}
```

shimTask는 `runtime.Task` 인터페이스를 구현한다:

```go
var _ runtime.Task = &shimTask{}
```

### 7.2 newShimTask

```go
// 소스: core/runtime/v2/shim.go

func newShimTask(shim ShimInstance) (*shimTask, error) {
    _, version := shim.Endpoint()
    taskClient, err := NewTaskClient(shim.Client(), version)
    return &shimTask{
        ShimInstance: shim,
        task:         taskClient,
    }, nil
}
```

ShimInstance의 Client()로 원시 연결 객체를 가져오고, version에 따라 적절한
TaskServiceClient를 생성한다. version 2는 TTRPC v2 프로토콜, version 3은
streaming IO를 지원하는 v3 프로토콜을 사용한다.

### 7.3 shimTask의 주요 메서드들

모든 메서드는 동일한 패턴을 따른다: **Go 인터페이스 호출 → TTRPC/gRPC 요청 → 응답 변환**

```go
// 소스: core/runtime/v2/shim.go

func (s *shimTask) Start(ctx context.Context) error {
    _, err := s.task.Start(ctx, &task.StartRequest{ID: s.ID()})
    return errgrpc.ToNative(err)
}

func (s *shimTask) Kill(ctx context.Context, signal uint32, all bool) error {
    _, err := s.task.Kill(ctx, &task.KillRequest{
        ID: s.ID(), Signal: signal, All: all,
    })
    return errgrpc.ToNative(err)
}

func (s *shimTask) Pause(ctx context.Context) error {
    _, err := s.task.Pause(ctx, &task.PauseRequest{ID: s.ID()})
    return errgrpc.ToNative(err)
}

func (s *shimTask) Resume(ctx context.Context) error {
    _, err := s.task.Resume(ctx, &task.ResumeRequest{ID: s.ID()})
    return errgrpc.ToNative(err)
}
```

### 7.4 State 조회

```go
// 소스: core/runtime/v2/shim.go

func (s *shimTask) State(ctx context.Context) (runtime.State, error) {
    response, err := s.task.State(ctx, &task.StateRequest{ID: s.ID()})
    if err != nil {
        if errdefs.IsDeadlineExceeded(err) {
            return runtime.State{}, err          // 타임아웃 전파
        }
        if !errors.Is(err, ttrpc.ErrClosed) {
            return runtime.State{}, errgrpc.ToNative(err)
        }
        return runtime.State{}, errdefs.ErrNotFound  // 연결 끊김 → NotFound
    }
    return runtime.State{
        Pid:        response.Pid,
        Status:     statusFromProto(response.Status),
        Stdin:      response.Stdin,
        Stdout:     response.Stdout,
        Stderr:     response.Stderr,
        Terminal:   response.Terminal,
        ExitStatus: response.ExitStatus,
        ExitedAt:   protobuf.FromTimestamp(response.ExitedAt),
    }, nil
}
```

**ttrpc.ErrClosed 처리**: shim이 비정상 종료되면 TTRPC 연결이 끊기고 `ttrpc.ErrClosed`가
발생한다. 이 경우 NotFound로 변환하여 "해당 task가 더 이상 존재하지 않음"을 표현한다.

### 7.5 PID 조회

```go
// 소스: core/runtime/v2/shim.go

func (s *shimTask) PID(ctx context.Context) (uint32, error) {
    response, err := s.task.Connect(ctx, &task.ConnectRequest{ID: s.ID()})
    return response.TaskPid, errgrpc.ToNative(err)
}
```

PID는 `Connect` RPC를 통해 조회한다. Connect는 shim과의 연결을 확인하는 동시에
init process의 PID를 반환하는 경량 RPC이다.

### 7.6 statusFromProto 변환

```go
// 소스: core/runtime/v2/process.go

func statusFromProto(from tasktypes.Status) runtime.Status {
    switch from {
    case tasktypes.Status_CREATED:  return runtime.CreatedStatus
    case tasktypes.Status_RUNNING:  return runtime.RunningStatus
    case tasktypes.Status_STOPPED:  return runtime.StoppedStatus
    case tasktypes.Status_PAUSED:   return runtime.PausedStatus
    case tasktypes.Status_PAUSING:  return runtime.PausingStatus
    }
    return 0  // unknown
}
```

---

## 8. Exec - 컨테이너 내 추가 프로세스

### 8.1 Exec 개요

`docker exec`나 `ctr task exec`에 해당하는 기능이다. 실행 중인 컨테이너에 추가 프로세스를
생성한다.

```
Task (init process)
├── PID: 1 (컨테이너의 init)
│
├── Exec "exec-1" (PID: 42)
│   └── 별도의 Process 인터페이스
│
└── Exec "exec-2" (PID: 55)
    └── 별도의 Process 인터페이스
```

### 8.2 API 계층의 Exec

```go
// 소스: plugins/services/tasks/local.go

func (l *local) Exec(ctx context.Context, r *api.ExecProcessRequest, _ ...grpc.CallOption) (*ptypes.Empty, error) {
    if r.ExecID == "" {
        return nil, status.Errorf(codes.InvalidArgument, "exec id cannot be empty")
    }
    t, err := l.getTask(ctx, r.ContainerID)

    t.Exec(ctx, r.ExecID, runtime.ExecOpts{
        Spec: r.Spec,
        IO: runtime.IO{
            Stdin:    r.Stdin,
            Stdout:   r.Stdout,
            Stderr:   r.Stderr,
            Terminal: r.Terminal,
        },
    })

    return empty, nil
}
```

### 8.3 shimTask의 Exec

```go
// 소스: core/runtime/v2/shim.go

func (s *shimTask) Exec(ctx context.Context, id string, opts runtime.ExecOpts) (runtime.ExecProcess, error) {
    if err := identifiers.Validate(id); err != nil {
        return nil, fmt.Errorf("invalid exec id %s: %w", id, err)
    }
    request := &task.ExecProcessRequest{
        ID:       s.ID(),      // Task ID
        ExecID:   id,          // Exec process ID
        Stdin:    opts.IO.Stdin,
        Stdout:   opts.IO.Stdout,
        Stderr:   opts.IO.Stderr,
        Terminal: opts.IO.Terminal,
        Spec:     opts.Spec,   // 프로세스 스펙 (명령어, 환경 등)
    }
    s.task.Exec(ctx, request)

    return &process{
        id:   id,
        shim: s,
    }, nil
}
```

Exec 후 `process` 구조체를 반환한다. 이 구조체가 ExecProcess 인터페이스를 구현한다.

### 8.4 process 구조체 (exec 프로세스)

```go
// 소스: core/runtime/v2/process.go

type process struct {
    id   string      // exec ID
    shim *shimTask   // 부모 shimTask 참조
}
```

process의 모든 메서드는 shimTask의 TTRPC 클라이언트를 사용하되, `ExecID`를 추가로 전달한다:

```go
// 소스: core/runtime/v2/process.go

func (p *process) Kill(ctx context.Context, signal uint32, _ bool) error {
    _, err := p.shim.task.Kill(ctx, &task.KillRequest{
        Signal: signal,
        ID:     p.shim.ID(),   // Task ID
        ExecID: p.id,          // Exec process ID ← 핵심 차이
    })
    return errgrpc.ToNative(err)
}

func (p *process) Start(ctx context.Context) error {
    _, err := p.shim.task.Start(ctx, &task.StartRequest{
        ID:     p.shim.ID(),
        ExecID: p.id,
    })
    return errgrpc.ToNative(err)
}

func (p *process) Wait(ctx context.Context) (*runtime.Exit, error) {
    response, err := p.shim.task.Wait(ctx, &task.WaitRequest{
        ID:     p.shim.ID(),
        ExecID: p.id,
    })
    return &runtime.Exit{
        Timestamp: protobuf.FromTimestamp(response.ExitedAt),
        Status:    response.ExitStatus,
    }, nil
}

func (p *process) Delete(ctx context.Context) (*runtime.Exit, error) {
    response, err := p.shim.task.Delete(ctx, &task.DeleteRequest{
        ID:     p.shim.ID(),
        ExecID: p.id,
    })
    return &runtime.Exit{
        Status:    response.ExitStatus,
        Timestamp: protobuf.FromTimestamp(response.ExitedAt),
        Pid:       response.Pid,
    }, nil
}
```

### 8.5 DeleteProcess API

```go
// 소스: plugins/services/tasks/local.go

func (l *local) DeleteProcess(ctx context.Context, r *api.DeleteProcessRequest, _ ...grpc.CallOption) (*api.DeleteResponse, error) {
    t, err := l.getTask(ctx, r.ContainerID)
    process, err := t.Process(ctx, r.ExecID)
    exit, err := process.Delete(ctx)
    return &api.DeleteResponse{
        ID:         r.ExecID,
        ExitStatus: exit.Status,
        ExitedAt:   protobuf.ToTimestamp(exit.Timestamp),
        Pid:        exit.Pid,
    }, nil
}
```

Exec process의 생명주기:

```
Exec() → Created → Start() → Running → Kill()/종료 → Stopped → Delete()
```

---

## 9. Wait와 프로세스 종료 처리

### 9.1 Wait API

```go
// 소스: plugins/services/tasks/local.go

func (l *local) Wait(ctx context.Context, r *api.WaitRequest, _ ...grpc.CallOption) (*api.WaitResponse, error) {
    t, err := l.getTask(ctx, r.ContainerID)
    p := runtime.Process(t)
    if r.ExecID != "" {
        p, _ = t.Process(ctx, r.ExecID)
    }
    exit, err := p.Wait(ctx)
    return &api.WaitResponse{
        ExitStatus: exit.Status,
        ExitedAt:   protobuf.ToTimestamp(exit.Timestamp),
    }, nil
}
```

### 9.2 shimTask.Wait()

```go
// 소스: core/runtime/v2/shim.go

func (s *shimTask) Wait(ctx context.Context) (*runtime.Exit, error) {
    taskPid, err := s.PID(ctx)  // 먼저 PID 확인
    response, err := s.task.Wait(ctx, &task.WaitRequest{ID: s.ID()})
    return &runtime.Exit{
        Pid:       taskPid,
        Timestamp: protobuf.FromTimestamp(response.ExitedAt),
        Status:    response.ExitStatus,
    }, nil
}
```

Wait는 **블로킹 호출**이다. 프로세스가 종료될 때까지 응답하지 않는다.
shim 측에서 프로세스의 waitpid()를 호출하고, 종료 시 Wait RPC 응답을 보낸다.

### 9.3 Shim 비정상 종료 시 처리

```go
// 소스: core/runtime/v2/shim.go

func cleanupAfterDeadShim(ctx context.Context, id string, rt *runtime.NSMap[ShimInstance],
    events *exchange.Exchange, binaryCall *binary) {
    ctx, cancel := timeout.WithContext(ctx, cleanupTimeout)  // 5초 타임아웃
    defer cancel()

    // 1. shim binary의 Delete 호출 (정리 시도)
    response, err := binaryCall.Delete(ctx)

    // 2. Task가 여전히 존재하면 이벤트 발행
    if _, err := rt.Get(ctx, id); err != nil {
        return  // 이미 삭제됨
    }

    // 3. TaskExit 이벤트 발행
    events.Publish(ctx, runtime.TaskExitEventTopic, &eventstypes.TaskExit{
        ContainerID: id,
        ExitStatus:  exitStatus,  // 응답 없으면 255
        ExitedAt:    exitedAt,    // 응답 없으면 현재 시간
    })

    // 4. TaskDelete 이벤트 발행
    events.Publish(ctx, runtime.TaskDeleteEventTopic, &eventstypes.TaskDelete{...})
}
```

Shim이 비정상 종료되면 TTRPC 연결의 `onClose` 콜백이 호출된다:

```go
// 소스: core/runtime/v2/shim_manager.go (startShim 내부)

shim, err := b.Start(ctx, typeurl.MarshalProto(topts), func() {
    log.G(ctx).WithField("id", id).Info("shim disconnected")
    cleanupAfterDeadShim(context.WithoutCancel(ctx), id, m.shims, m.events, b)
    m.shims.Delete(ctx, id)
})
```

이 콜백 체인:
1. Shim 프로세스 종료 → TTRPC 연결 끊김
2. `onClose` 콜백 호출
3. `cleanupAfterDeadShim()` 실행
4. exit status 255로 TaskExit 이벤트 발행
5. shim 목록에서 제거

### 9.4 버전 다운그레이드

```go
// 소스: core/runtime/v2/shim.go

type clientVersionDowngrader interface {
    Downgrade() error
}

func (s *shim) Downgrade() error {
    if s.version >= CurrentShimVersion {
        s.version--
        return nil
    }
    return fmt.Errorf("unable to downgrade...")
}
```

```go
// 소스: core/runtime/v2/task_manager.go (Create 내부)

t, err := shimTask.Create(ctx, opts)
if err != nil && errdefs.IsNotImplemented(err) {
    downgrader, ok := shim.(clientVersionDowngrader)
    if ok {
        if derr := downgrader.Downgrade(); derr == nil {
            shimTask, _ = newShimTask(shim)  // 낮은 버전으로 재생성
            return shimTask.Create(ctx, opts)
        }
    }
}
```

containerd v2.x에서 v1.7.x 시대의 shim과 통신할 때:
1. v3 프로토콜로 Create 시도
2. `NotImplemented` 에러 발생
3. 버전을 v2로 다운그레이드
4. v2 프로토콜로 재시도

이 메커니즘은 containerd 업그레이드 시 기존 shim과의 호환성을 보장한다.

---

## 10. Checkpoint/Restore

### 10.1 Checkpoint 개요

Checkpoint는 실행 중인 컨테이너의 메모리 상태를 디스크에 저장하여 나중에 복원할 수 있게 한다.
CRIU(Checkpoint/Restore In Userspace)를 사용한다.

### 10.2 Checkpoint API

```go
// 소스: plugins/services/tasks/local.go

func (l *local) Checkpoint(ctx context.Context, r *api.CheckpointTaskRequest, _ ...grpc.CallOption) (*api.CheckpointTaskResponse, error) {
    container, err := l.getContainer(ctx, r.ContainerID)
    t, err := l.getTaskFromContainer(ctx, container)

    // 1. 체크포인트 경로 결정
    image, err := getCheckpointPath(container.Runtime.Name, r.Options)
    checkpointImageExists := false
    if image == "" {
        checkpointImageExists = true
        image, _ = os.MkdirTemp(os.Getenv("XDG_RUNTIME_DIR"), "ctrd-checkpoint")
        defer os.RemoveAll(image)
    }

    // 2. shim에 체크포인트 요청
    t.Checkpoint(ctx, image, r.Options)

    // 3. Content Store에 저장 (경로 미지정 시)
    if checkpointImageExists {
        tar := archive.Diff(ctx, "", image)
        cp, _ := l.writeContent(ctx, images.MediaTypeContainerd1Checkpoint, image, tar)

        // Config도 저장
        data, _ := proto.Marshal(typeurl.MarshalProto(container.Spec))
        spec := bytes.NewReader(data)
        specD, _ := l.writeContent(ctx, images.MediaTypeContainerd1CheckpointConfig, ..., spec)

        return &api.CheckpointTaskResponse{
            Descriptors: []*types.Descriptor{cp, specD},
        }, nil
    }
}
```

체크포인트 저장 구조:

```
Content Store
├── sha256:aaa... (MediaTypeContainerd1Checkpoint)
│   └── CRIU 체크포인트 tar 아카이브
│       ├── criu 이미지 파일들 (pages, fdinfo 등)
│       └── 프로세스 상태 덤프
│
└── sha256:bbb... (MediaTypeContainerd1CheckpointConfig)
    └── 컨테이너 OCI 런타임 스펙 (protobuf)
```

### 10.3 Create에서의 Restore

```go
// 소스: plugins/services/tasks/local.go (Create 내부)

// CRI를 통한 로컬 체크포인트 복원
if r.Checkpoint != nil && r.Checkpoint.Annotations != nil {
    ann, ok := r.Checkpoint.Annotations["RestoreFromPath"]
    if ok {
        checkpointPath = ann
        restoreFromPath = true
    }
}

// 체크포인트 이미지에서 복원
if checkpointPath == "" && r.Checkpoint != nil {
    checkpointPath, _ = os.MkdirTemp(...)
    reader, _ := l.store.ReaderAt(ctx, checkpointDesc)
    archive.Apply(ctx, checkpointPath, content.NewReader(reader))
}
```

```go
// 소스: core/runtime/v2/shim.go (shimTask.Create 내부)

if opts.RestoreFromPath {
    // rootfs-diff.tar가 있으면 언팩
    rootfsDiff := filepath.Join(opts.Checkpoint, "..", crmetadata.RootFsDiffTar)
    if _, err = os.Stat(rootfsDiff); err == nil {
        decompressed, _ := compression.DecompressStream(rootfsDiffTar)
        archive.Apply(ctx, filepath.Join(s.Bundle(), "rootfs"), decompressed)
    }
    // Create 후 바로 Start
    s.Start(ctx)
}
```

복원 시 Create → Start가 연속으로 호출되어 체크포인트 시점의 상태로 프로세스가 재개된다.

### 10.4 shimTask.Checkpoint()

```go
// 소스: core/runtime/v2/shim.go

func (s *shimTask) Checkpoint(ctx context.Context, path string, options *ptypes.Any) error {
    request := &task.CheckpointTaskRequest{
        ID:      s.ID(),
        Path:    path,
        Options: options,
    }
    _, err := s.task.Checkpoint(ctx, request)
    return errgrpc.ToNative(err)
}
```

실제 CRIU 호출은 shim 프로세스(containerd-shim-runc-v2) 내부에서 수행된다.

---

## 11. 모니터링과 메트릭

### 11.1 TaskMonitor

```go
// 소스: plugins/services/tasks/local.go (initFunc 내부)

monitor, err := ic.GetSingle(plugins.TaskMonitorPlugin)
if err != nil {
    if !errors.Is(err, plugin.ErrPluginNotFound) {
        return nil, err
    }
    monitor = runtime.NewNoopMonitor()
}
```

TaskMonitor는 Task의 상태를 추적한다. 모니터 플러그인이 없으면 NoopMonitor를 사용한다.

모니터 등록/해제:

```go
// Create 시 등록
l.monitor.Monitor(c, map[string]string{"runtime": container.Runtime.Name})

// Delete 시 해제
l.monitor.Stop(t)
```

### 11.2 Metrics API

```go
// 소스: plugins/services/tasks/local.go

func (l *local) Metrics(ctx context.Context, r *api.MetricsRequest, _ ...grpc.CallOption) (*api.MetricsResponse, error) {
    filter, err := filters.ParseAll(r.Filters...)
    var resp api.MetricsResponse
    tasks, err := l.v2Runtime.Tasks(ctx, false)
    getTasksMetrics(ctx, filter, tasks, &resp)
    return &resp, nil
}

func getTasksMetrics(ctx context.Context, filter filters.Filter, tasks []runtime.Task, r *api.MetricsResponse) {
    for _, tk := range tasks {
        if !filter.Match(filters.AdapterFunc(func(fieldpath []string) (string, bool) {
            switch fieldpath[0] {
            case "id":        return t.ID(), true
            case "namespace": return t.Namespace(), true
            }
            return "", false
        })) {
            continue
        }

        collected := time.Now()
        stats, err := tk.Stats(ctx)
        r.Metrics = append(r.Metrics, &types.Metric{
            Timestamp: protobuf.ToTimestamp(collected),
            ID:        tk.ID(),
            Data:      stats,
        })
    }
}
```

메트릭 수집 흐름:

```
Metrics API 요청
    │
    ▼
Tasks(ctx, false)         ← 현재 네임스페이스의 모든 Task
    │
    ▼
filter.Match()            ← ID/namespace 기반 필터링
    │
    ▼
tk.Stats(ctx)             ← shimTask.Stats() → TTRPC
    │
    ▼
shim에서 cgroup 통계 수집  ← cpu, memory, io, pids
    │
    ▼
protobuf Any로 반환       ← 플랫폼별 포맷
```

### 11.3 shimTask.Stats()

```go
// 소스: core/runtime/v2/shim.go

func (s *shimTask) Stats(ctx context.Context) (*ptypes.Any, error) {
    response, err := s.task.Stats(ctx, &task.StatsRequest{ID: s.ID()})
    return response.Stats, errgrpc.ToNative(err)
}
```

Stats는 `*types.Any`를 반환한다. 실제 내용은 플랫폼에 따라 다르다:
- Linux: cgroup v1/v2 통계 (CPU, 메모리, IO, PID 수)
- Windows: HCS 통계

### 11.4 Update API

```go
// 소스: plugins/services/tasks/local.go

func (l *local) Update(ctx context.Context, r *api.UpdateTaskRequest, _ ...grpc.CallOption) (*ptypes.Empty, error) {
    t, err := l.getTask(ctx, r.ContainerID)
    t.Update(ctx, r.Resources, r.Annotations)
    return empty, nil
}
```

```go
// 소스: core/runtime/v2/shim.go

func (s *shimTask) Update(ctx context.Context, resources *ptypes.Any, annotations map[string]string) error {
    _, err := s.task.Update(ctx, &task.UpdateTaskRequest{
        ID:          s.ID(),
        Resources:   resources,
        Annotations: annotations,
    })
    return errgrpc.ToNative(err)
}
```

Update로 실행 중인 컨테이너의 리소스(CPU, 메모리 제한 등)를 동적으로 변경할 수 있다.

### 11.5 Pause/Resume

```go
// 소스: plugins/services/tasks/local.go

func (l *local) Pause(ctx context.Context, r *api.PauseTaskRequest, _ ...grpc.CallOption) (*ptypes.Empty, error) {
    t, err := l.getTask(ctx, r.ContainerID)
    t.Pause(ctx)
    return empty, nil
}

func (l *local) Resume(ctx context.Context, r *api.ResumeTaskRequest, _ ...grpc.CallOption) (*ptypes.Empty, error) {
    t, err := l.getTask(ctx, r.ContainerID)
    t.Resume(ctx)
    return empty, nil
}
```

Pause/Resume은 Linux cgroup freezer를 사용하여 컨테이너의 모든 프로세스를 일시 정지/재개한다.

### 11.6 Kill API

```go
// 소스: plugins/services/tasks/local.go

func (l *local) Kill(ctx context.Context, r *api.KillRequest, _ ...grpc.CallOption) (*ptypes.Empty, error) {
    t, err := l.getTask(ctx, r.ContainerID)
    p := runtime.Process(t)
    if r.ExecID != "" {
        p, _ = t.Process(ctx, r.ExecID)
    }
    p.Kill(ctx, r.Signal, r.All)
    return empty, nil
}
```

Kill의 `All` 플래그: `true`이면 컨테이너 내 모든 프로세스에 시그널 전송,
`false`이면 대상 프로세스만.

### 11.7 ListPids API

```go
// 소스: plugins/services/tasks/local.go

func (l *local) ListPids(ctx context.Context, r *api.ListPidsRequest, _ ...grpc.CallOption) (*api.ListPidsResponse, error) {
    t, err := l.getTask(ctx, r.ContainerID)
    processList, err := t.Pids(ctx)
    var processes []*task.ProcessInfo
    for _, p := range processList {
        pInfo := task.ProcessInfo{Pid: p.Pid}
        if p.Info != nil {
            a, _ := typeurl.MarshalAnyToProto(p.Info)
            pInfo.Info = a
        }
        processes = append(processes, &pInfo)
    }
    return &api.ListPidsResponse{Processes: processes}, nil
}
```

ListPids는 컨테이너 내 모든 프로세스의 PID를 반환한다.
`Info` 필드는 플랫폼별 추가 정보(예: Linux에서는 cgroup 정보)를 포함할 수 있다.

---

## 12. 설계 철학과 핵심 교훈

### 12.1 Container와 Task의 분리

이것이 containerd 태스크 시스템의 가장 근본적인 설계 결정이다:

```
전통적 접근:           containerd 접근:
┌─────────┐           ┌─────────┐     ┌──────┐
│Container│           │Container│────►│ Task │
│ = 실행  │           │ = 설정  │     │ = 실행│
└─────────┘           └─────────┘     └──────┘
                       영구 저장        임시 상태
```

이 분리의 구체적 이점:
1. 컨테이너 재시작: Container 메타데이터 유지, Task만 재생성
2. 이미지 업데이트: Container.Spec 변경 → 새 Task 생성
3. 상태 관찰: Container 존재 여부와 Task 실행 여부를 독립적으로 확인
4. 정리(Cleanup): Task가 없어도 Container 메타데이터로 리소스 추적 가능

### 12.2 인터페이스 상속의 의도적 설계

```
Process (7 methods)
    ↓
ExecProcess (+1: Delete)
    ↓
Task (+10: PID, Namespace, Pause, Resume, Exec, Pids, Checkpoint, Update, Process, Stats)
```

이 계층은 **Liskov Substitution Principle**을 따른다:
- Process가 필요한 곳에 ExecProcess나 Task를 사용 가능
- API 계층에서 init process와 exec process를 동일하게 처리 가능

```go
// 실제 사용 예: Kill API
p := runtime.Process(t)     // Task를 Process로 취급
if r.ExecID != "" {
    p, _ = t.Process(ctx, r.ExecID)  // ExecProcess도 Process
}
p.Kill(ctx, r.Signal, r.All)  // 동일한 Kill() 호출
```

### 12.3 2-Layer 아키텍처의 관심사 분리

```
API 계층 (local)           런타임 계층 (TaskManager)
─────────────────          ──────────────────────────
프로토콜 변환              번들/shim 관리
메타데이터 조회            마운트 활성화/비활성화
이벤트 발행               런타임 기능 검증
모니터 등록/해제          shim 프로세스 생명주기
에러 변환                 연결 관리
```

각 계층을 독립적으로 교체할 수 있다:
- API 계층: gRPC 대신 다른 프로토콜 사용 가능
- 런타임 계층: runc 대신 다른 OCI 런타임 사용 가능

### 12.4 shimTask = Proxy 패턴

shimTask는 전형적인 **Proxy 패턴**이다:

```
runtime.Task 인터페이스
       ↑
    shimTask
       │
       │ TTRPC/gRPC
       ↓
containerd-shim-runc-v2 (별도 프로세스)
       │
       │ exec
       ↓
    runc (컨테이너 런타임)
```

shimTask의 모든 메서드는 동일한 패턴을 따른다:
1. Go 인터페이스 호출 받음
2. protobuf Request 구조체 생성
3. TTRPC/gRPC로 shim에 전송
4. Response를 Go 구조체로 변환하여 반환
5. 에러를 `errgrpc.ToNative()`로 변환

이 일관성 덕분에 새로운 RPC 메서드를 추가하는 것이 기계적인 작업이 된다.

### 12.5 에러 처리의 세밀함

Tasks Service의 에러 처리는 세 가지 계층에서 이루어진다:

```
1. shim 측 에러      → errgrpc.ToNative() → Go 표준 에러
2. 연결 에러         → ttrpc.ErrClosed → errdefs.ErrNotFound
3. 타임아웃          → DeadlineExceeded 그대로 전파
4. API 응답 에러     → errgrpc.ToGRPC() → gRPC status 코드
```

특히 `ttrpc.ErrClosed`를 `ErrNotFound`로 변환하는 것은 의미론적으로 정확하다:
"연결이 끊겼다" = "해당 엔티티가 더 이상 존재하지 않는다"

### 12.6 버전 호환성 전략

containerd는 shim API 버전 호환성을 위해 **graceful degradation** 전략을 사용한다:

```
v3 시도 → NotImplemented? → v2로 다운그레이드 → 재시도
```

이 전략의 장점:
- 신규 기능은 v3로 점진적 도입
- 기존 shim은 업그레이드 없이 계속 동작
- 완전한 하위 호환성 유지

### 12.7 정리

containerd의 태스크 실행 시스템은 **Container(정적 설정)와 Task(동적 실행)의 분리**라는
핵심 원칙 위에 구축된다. Process → ExecProcess → Task의 인터페이스 계층, 2-Layer 서비스
아키텍처(API/런타임), shimTask Proxy 패턴이 결합되어 유연하고 확장 가능한 컨테이너 실행
환경을 제공한다. 특히 버전 다운그레이드 메커니즘, 세밀한 에러 분류, 모니터 복원 패턴은
프로덕션 환경에서의 안정성을 위한 실전적인 설계 결정이다.
