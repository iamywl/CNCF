# PoC-07: Task/Process 생명주기

## 목적

containerd의 Task와 Process 생명주기를 시뮬레이션한다. Task는 컨테이너 내부의 실행 단위이며, PlatformRuntime이 Task의 CRUD를 관리한다. Task는 Process 인터페이스를 내장하며, Exec으로 실행 중인 Task에 추가 프로세스를 생성할 수 있다.

## 핵심 개념

### 1. PlatformRuntime 인터페이스

PlatformRuntime은 플랫폼별 Task 관리를 담당하는 최상위 인터페이스이다. containerd v2에서는 `RuntimePluginV2`로 등록된 shim 기반 런타임이 이를 구현한다.

```go
type PlatformRuntime interface {
    ID() string
    Create(ctx, taskID, opts) (Task, error)
    Get(ctx, taskID) (Task, error)
    Tasks(ctx, all) ([]Task, error)
    Delete(ctx, taskID) (*Exit, error)
}
```

### 2. Task와 Process 인터페이스 계층

```
Process (기본 프로세스 인터페이스)
├── ID() string
├── State(ctx) (State, error)
├── Kill(ctx, signal, all) error
├── Start(ctx) error
└── Wait(ctx) (*Exit, error)

Task (= Process + 컨테이너 관리)
├── Process (내장)
├── PID(ctx) (uint32, error)
├── Namespace() string
├── Pause(ctx) / Resume(ctx)
├── Exec(ctx, id, opts) (ExecProcess, error)
├── Process(ctx, id) (ExecProcess, error)
└── ...

ExecProcess (= Process + Delete)
├── Process (내장)
└── Delete(ctx) (*Exit, error)
```

### 3. 상태 전이

Task와 Process 모두 동일한 상태 전이를 따른다:

```
Created ──Start()──> Running ──Kill()──> Stopped ──Delete()──> (삭제)
                       │                    ↑
                  Pause() │           Resume()
                       ↓                    │
                     Paused ────────────────┘
```

각 상태의 의미:
- **Created**: 프로세스가 생성되었지만 아직 실행되지 않음 (runc create)
- **Running**: 프로세스가 실행 중 (runc start)
- **Stopped**: 프로세스가 종료됨 (exit status 확인 가능)
- **Paused**: 컨테이너가 일시정지됨 (cgroup freezer)
- **Deleted**: Task가 삭제됨

### 4. Exec (추가 프로세스)

실행 중인 Task에 `Exec`으로 추가 프로세스를 생성할 수 있다. 이는 `kubectl exec`이나 `docker exec`의 기반이 된다.

```
Task.Exec(id, opts) → ExecProcess (Created)
ExecProcess.Start() → Running
ExecProcess.Kill()  → Stopped
ExecProcess.Delete() → 삭제
```

### 5. Wait (종료 대기)

`Wait`는 프로세스가 종료될 때까지 블로킹하며, 종료 시 `Exit` 구조체를 반환한다. goroutine에서 호출하여 비동기 종료 감지에 사용된다.

### 6. local 서비스 (Task API)

`plugins/services/tasks/local.go`의 `local` 구조체는 containerd의 Task gRPC 서비스를 구현한다. PlatformRuntime을 래핑하여 컨테이너 메타데이터 조회, 모니터링 등의 추가 기능을 제공한다.

## 소스 참조

| 파일 | 설명 |
|------|------|
| `core/runtime/runtime.go` | PlatformRuntime 인터페이스, CreateOpts, Exit, IO |
| `core/runtime/task.go` | Task, Process, ExecProcess 인터페이스, State, Status 상수 |
| `plugins/services/tasks/local.go` | local 서비스 (Create, Start, Delete, Exec, Wait 구현) |

## 실행 방법

```bash
go run main.go
```

## 예상 출력

```
========================================
containerd Task/Process 생명주기 시뮬레이션
========================================

--- 시나리오 1: Task 생명주기 ---
    상태 전이: Created → Running → Stopped → (Delete)

  [1] Create Task:
    [생성 후] Task=nginx-container, PID=..., Status=CREATED
  [2] Start Task:
    [시작 후] Task=nginx-container, PID=..., Status=RUNNING
  [3] Pause / Resume:
    [Pause 후] Status=PAUSED
    [Resume 후] Status=RUNNING
  [4] Kill Task (SIGTERM):
    [Kill 후] Status=STOPPED
  [5] Delete Task:
    삭제 완료

--- 시나리오 2: Exec (추가 프로세스 실행) ---
  exec-shell 프로세스 Create → Start → Kill → Delete

--- 시나리오 3: Wait으로 Exit 코드 수집 ---
  goroutine에서 Wait 호출 → Kill 후 종료 감지

--- 시나리오 4: 여러 Task 동시 관리 ---
  redis-task, postgres-task, memcached-task 동시 실행

--- Task/Process 상태 전이 다이어그램 ---
  ASCII 아트로 상태 전이 시각화
```
