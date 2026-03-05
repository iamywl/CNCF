# 10. containerd Runtime Shim Deep-Dive

## 목차

1. [개요](#1-개요)
2. [왜 별도 프로세스인가: Shim 아키텍처의 이유](#2-왜-별도-프로세스인가-shim-아키텍처의-이유)
3. [Shim v2 아키텍처](#3-shim-v2-아키텍처)
4. [Manager 인터페이스와 BootstrapParams](#4-manager-인터페이스와-bootstrapparams)
5. [Shim Manager (runc v2) 구현](#5-shim-manager-runc-v2-구현)
6. [binary.Start(): containerd 측 Shim 시작](#6-binarystart-containerd-측-shim-시작)
7. [Shim 진입점과 Run 함수](#7-shim-진입점과-run-함수)
8. [TTRPC 통신: containerd와 Shim 사이](#8-ttrpc-통신-containerd와-shim-사이)
9. [TaskService 구현](#9-taskservice-구현)
10. [프로세스 생명주기 관리](#10-프로세스-생명주기-관리)
11. [processExits와 Reaping](#11-processexits와-reaping)
12. [forward() goroutine과 이벤트 전파](#12-forward-goroutine과-이벤트-전파)
13. [OOM 감시](#13-oom-감시)
14. [Shim 그룹핑과 Pod 지원](#14-shim-그룹핑과-pod-지원)
15. [Shim 전체 생명주기](#15-shim-전체-생명주기)

---

## 1. 개요

containerd의 Runtime Shim은 containerd 데몬과 실제 컨테이너 프로세스 사이에 위치하는 **경량 중간 프로세스**이다. 각 컨테이너(또는 Pod)마다 독립된 shim 프로세스가 실행되어, containerd가 재시작되어도 컨테이너가 계속 동작할 수 있게 한다.

```
소스 위치:
  pkg/shim/shim.go                                      -- Run, Manager, BootstrapParams
  cmd/containerd-shim-runc-v2/main.go                   -- shim 진입점
  cmd/containerd-shim-runc-v2/manager/manager_linux.go  -- Shim Manager 구현
  cmd/containerd-shim-runc-v2/task/service.go           -- TaskService 구현
  core/runtime/v2/binary.go                             -- binary.Start() (containerd 측)
```

---

## 2. 왜 별도 프로세스인가: Shim 아키텍처의 이유

### 문제: containerd 재시작

```
containerd 재시작 없이:                 containerd 재시작 시:

  containerd (PID 100)                    containerd (PID 100) ← 종료
    |                                       |
    +-- container (PID 200)                 +-- container (PID 200) ← 고아 프로세스!
    +-- container (PID 300)                 +-- container (PID 300) ← 고아 프로세스!
```

containerd가 컨테이너의 직접 부모이면, containerd 재시작 시 모든 컨테이너가 고아가 된다.

### 해결: Shim 중간 계층

```
  containerd (PID 100)
    |
    +-- containerd-shim (PID 150) ← 독립 프로세스 그룹
    |     |
    |     +-- container (PID 200)
    |
    +-- containerd-shim (PID 160)
          |
          +-- container (PID 300)

containerd 재시작해도:
  - shim은 계속 실행
  - container는 shim이 관리
  - containerd는 shim에 재연결
```

### Shim이 제공하는 가치

| 기능 | 설명 |
|------|------|
| **프로세스 격리** | containerd 재시작/크래시에 컨테이너 영향 없음 |
| **Reaping** | 컨테이너 프로세스의 zombie 처리 |
| **Exit 보고** | 컨테이너 종료 시 exit status 보존 |
| **IO 관리** | stdin/stdout/stderr 관리 |
| **OOM 감시** | cgroup OOM 이벤트 감지 |
| **경량성** | GOMAXPROCS=2, GC 40%로 메모리 최소화 |

---

## 3. Shim v2 아키텍처

```
+--------------------+          ttrpc           +-------------------+
|                    |  (unix domain socket)     |                   |
|    containerd      |<========================>| containerd-shim   |
|    daemon          |                          | -runc-v2          |
|                    |  Tasks API:              |                   |
|  [RuntimePlugin]   |  Create, Start, Kill     |  [TaskService]    |
|  [binary.Start()]  |  Delete, Wait, Exec      |  [Manager]        |
|                    |  Pause, Resume, Stats     |  [reaper]         |
|                    |                          |  [oom watcher]    |
+--------------------+                          +---+---------------+
                                                    |
                                                    | fork/exec (runc)
                                                    v
                                              +------------------+
                                              | container process|
                                              | (PID 1 in ns)    |
                                              +------------------+
```

### TTRPC vs gRPC

Shim은 gRPC가 아닌 **ttrpc**를 사용한다:
- **더 낮은 메모리**: HTTP/2 스택 없음
- **더 빠른 시작**: 핸드셰이크 오버헤드 없음
- **파일 디스크립터 전달**: Unix 소켓의 SCM_RIGHTS 지원
- **수백 개 shim**: 각 shim의 메모리 절약이 전체 시스템에 큰 영향

---

## 4. Manager 인터페이스와 BootstrapParams

### Manager 인터페이스

```go
// 소스: pkg/shim/shim.go:78-83

type Manager interface {
    Name() string
    Start(ctx context.Context, id string, opts StartOpts) (BootstrapParams, error)
    Stop(ctx context.Context, id string) (StopStatus, error)
    Info(ctx context.Context, optionsR io.Reader) (*types.RuntimeInfo, error)
}
```

Manager는 shim 프로세스의 **생명주기 관리자**이다. containerd가 shim 바이너리를 실행할 때 서브커맨드로 Manager의 메서드를 호출한다.

| 메서드 | 서브커맨드 | 역할 |
|--------|----------|------|
| Start | `shim start` | 새 shim 프로세스 시작, 소켓 주소 반환 |
| Stop | `shim delete` | shim 프로세스 정리 |
| Info | `shim --info` | 런타임 정보 반환 |

### BootstrapParams

```go
// 소스: pkg/shim/shim.go:62-69

type BootstrapParams struct {
    Version  int    `json:"version"`   // shim 프로토콜 버전 (2 또는 3)
    Address  string `json:"address"`   // 연결할 소켓 주소
    Protocol string `json:"protocol"`  // "ttrpc" 또는 "grpc"
}
```

shim이 시작되면 BootstrapParams를 JSON으로 stdout에 출력한다. containerd는 이를 읽어 shim에 연결한다.

### StartOpts

```go
// 소스: pkg/shim/shim.go:55-59

type StartOpts struct {
    Address      string  // containerd gRPC 주소
    TTRPCAddress string  // containerd ttrpc 주소
    Debug        bool    // 디버그 모드
}
```

### StopStatus

```go
// 소스: pkg/shim/shim.go:71-75

type StopStatus struct {
    Pid        int
    ExitStatus int
    ExitedAt   time.Time
}
```

---

## 5. Shim Manager (runc v2) 구현

### manager 구조체

```go
// 소스: cmd/containerd-shim-runc-v2/manager/manager_linux.go:54-60

func NewShimManager(name string) shim.Manager {
    return &manager{name: name}
}

type manager struct {
    name string
}
```

### Start: 새 Shim 프로세스 시작

```go
// 소스: cmd/containerd-shim-runc-v2/manager/manager_linux.go:184-283

func (manager) Start(ctx context.Context, id string, opts shim.StartOpts) (_ shim.BootstrapParams, retErr error) {
    var params shim.BootstrapParams
    params.Version = 3
    params.Protocol = "ttrpc"

    // 1. shim 명령 구성
    cmd, _ := newCommand(ctx, id, opts.Address, opts.TTRPCAddress, opts.Debug)

    // 2. Pod 그룹핑 확인
    grouping := id
    spec, _ := readSpec()
    for _, group := range groupLabels {
        if groupID, ok := spec.Annotations[group]; ok {
            grouping = groupID
            break
        }
    }

    // 3. Unix 소켓 생성
    s, err := newShimSocket(ctx, opts.Address, grouping, false)
    if err != nil {
        if errdefs.IsAlreadyExists(err) {
            // 같은 그룹의 shim이 이미 실행 중 → 기존 주소 반환
            params.Address = s.addr
            return params, nil
        }
        return params, err
    }
    cmd.ExtraFiles = append(cmd.ExtraFiles, s.f)  // FD 3으로 소켓 전달

    // 4. shim 프로세스 시작
    cmd.Start()
    go cmd.Wait()  // 별도 goroutine에서 대기

    // 5. cgroup 설정 (선택적)
    if opts, _ := shim.ReadRuntimeOptions[*options.Options](os.Stdin); opts != nil {
        if opts.ShimCgroup != "" {
            // shim을 지정된 cgroup에 추가
        }
    }

    // 6. OOM 점수 조정
    shim.AdjustOOMScore(cmd.Process.Pid)

    params.Address = sockets[0].addr
    return params, nil
}
```

### Start 흐름도

```
containerd                          Shim Manager (Start)
  |                                       |
  |  exec("containerd-shim-runc-v2",     |
  |        "-id", "ctr1", "start")        |
  |-------------------------------------->|
  |                                       |
  |                                       |  1. newCommand() -- 자기 자신을 재실행
  |                                       |  2. readSpec() -- config.json 읽기
  |                                       |  3. 그룹핑 결정 (Pod sandbox ID)
  |                                       |  4. newShimSocket() -- Unix 소켓 생성
  |                                       |  5. cmd.Start() -- 새 프로세스 시작
  |                                       |  6. OOM 점수 조정
  |                                       |
  |  stdout: {"version":3,                |
  |           "address":"...",             |
  |           "protocol":"ttrpc"}          |
  |<--------------------------------------|
  |                                       |
  |  ttrpc 연결                            |
  |-------------------------------------->|  (새 shim 프로세스가 서비스)
```

### newCommand: 자기 재실행

```go
// 소스: cmd/containerd-shim-runc-v2/manager/manager_linux.go:82-111

func newCommand(ctx context.Context, id, containerdAddress, containerdTTRPCAddress string, debug bool) (*exec.Cmd, error) {
    self, _ := os.Executable()     // 자기 자신의 경로
    args := []string{
        "-namespace", ns,
        "-id", id,
        "-address", containerdAddress,
    }
    cmd := exec.Command(self, args...)  // 같은 바이너리를 재실행
    cmd.SysProcAttr = &syscall.SysProcAttr{
        Setpgid: true,  // 새 프로세스 그룹
    }
    return cmd, nil
}
```

**왜 자기 자신을 재실행하는가:**
`"start"` 서브커맨드로 호출된 프로세스는 소켓을 만들고 새 프로세스를 시작한 뒤 종료된다. 새 프로세스는 서브커맨드 없이 실행되어 실제 shim 서비스를 제공한다. 이 2단계 시작은 containerd가 shim의 소켓 주소를 동기적으로 받을 수 있게 한다.

### Stop: Shim 정리

```go
// 소스: cmd/containerd-shim-runc-v2/manager/manager_linux.go:285-327

func (manager) Stop(ctx context.Context, id string) (shim.StopStatus, error) {
    // 1. runc 컨테이너 강제 삭제
    r := process.NewRunc(root, path, ns, runtime, false)
    r.Delete(ctx, id, &runcC.DeleteOpts{Force: true})

    // 2. rootfs 언마운트
    mount.UnmountRecursive(filepath.Join(path, "rootfs"), 0)

    // 3. init PID 읽기
    pid, _ := runcC.ReadPidFile(filepath.Join(path, process.InitPidFile))

    return shim.StopStatus{
        ExitedAt:   time.Now(),
        ExitStatus: 128 + int(unix.SIGKILL),
        Pid:        pid,
    }, nil
}
```

Stop은 `"delete"` 서브커맨드로 호출된다. 잔존하는 runc 컨테이너와 마운트를 강제 정리한다.

---

## 6. binary.Start(): containerd 측 Shim 시작

### binary 구조체

```go
// 소스: core/runtime/v2/binary.go:38-61

type shimBinaryConfig struct {
    runtime      string
    address      string
    ttrpcAddress string
    env          []string
}

type binary struct {
    runtime                string
    containerdAddress      string
    containerdTTRPCAddress string
    bundle                 *Bundle
    env                    []string
}
```

### Start 흐름

```go
// 소스: core/runtime/v2/binary.go:63-154

func (b *binary) Start(ctx context.Context, opts *types.Any, onClose func()) (_ *shim, err error) {
    // 1. 명령 인자 구성
    args := []string{"-id", b.bundle.ID}
    args = append(args, "start")

    // 2. shim 명령 생성 및 실행
    cmd, _ := client.Command(ctx, &client.CommandConfig{
        Runtime:      b.runtime,
        Address:      b.containerdAddress,
        TTRPCAddress: b.containerdTTRPCAddress,
        Path:         b.bundle.Path,
        Args:         args,
    })

    // 3. shim 로그 파이프 열기
    f, _ := openShimLog(shimCtx, b.bundle, client.AnonDialer)
    go func() {
        io.Copy(os.Stderr, f)  // shim 로그를 containerd stderr로 복사
    }()

    // 4. shim 실행 및 결과 읽기
    out, _ := cmd.CombinedOutput()
    response := bytes.TrimSpace(out)

    // 5. 부트스트랩 파라미터 파싱
    params, _ := parseStartResponse(response)

    // 6. shim에 연결
    conn, _ := makeConnection(ctx, b.bundle.ID, params, onCloseWithShimLog)

    // 7. 부트스트랩 정보 저장 (재시작 복구용)
    writeBootstrapParams(filepath.Join(b.bundle.Path, "bootstrap.json"), params)

    // 8. shim 바이너리 경로 저장
    os.WriteFile(filepath.Join(b.bundle.Path, "shim-binary-path"), []byte(b.runtime), 0600)

    return &shim{
        bundle:  b.bundle,
        client:  conn,
        address: fmt.Sprintf("%s+%s", params.Protocol, params.Address),
        version: params.Version,
    }, nil
}
```

### 재시작 복구 데이터

```
{bundle-path}/
  +-- bootstrap.json     (shim 주소/프로토콜)
  +-- shim-binary-path   (shim 바이너리 경로)
```

containerd가 재시작되면 이 파일들을 읽어 기존 shim에 재연결한다.

---

## 7. Shim 진입점과 Run 함수

### main.go

```go
// 소스: cmd/containerd-shim-runc-v2/main.go:29-31

func main() {
    shim.Run(context.Background(), manager.NewShimManager("io.containerd.runc.v2"))
}
```

매우 간결한 진입점이다. `shim.Run()`이 모든 로직을 처리한다.

### Run 함수 흐름

```go
// 소스: pkg/shim/shim.go:195-207

func Run(ctx context.Context, manager Manager, opts ...BinaryOpts) {
    var config Config
    for _, o := range opts {
        o(&config)
    }
    if err := run(ctx, manager, config); err != nil {
        os.Exit(1)
    }
}
```

### run 함수 내부

```go
// 소스: pkg/shim/shim.go:222-444 (핵심 흐름)

func run(ctx context.Context, manager Manager, config Config) error {
    parseFlags()

    // 서브커맨드 처리
    switch action {
    case "delete":
        // Manager.Stop() 호출, 결과를 stdout에 출력
        ss, _ := manager.Stop(ctx, id)
        data, _ := proto.Marshal(&shimapi.DeleteResponse{...})
        os.Stdout.Write(data)
        return nil

    case "start":
        // Manager.Start() 호출, BootstrapParams를 stdout에 JSON 출력
        params, _ := manager.Start(ctx, id, opts)
        data, _ := json.Marshal(&params)
        os.Stdout.Write(data)
        return nil
    }

    // 서브커맨드 없음 → 실제 shim 서비스 실행
    setRuntime()        // GOMAXPROCS=2, GC=40%
    subreaper()         // child subreaper 설정
    setupSignals()      // 시그널 핸들러

    // 플러그인 로딩 (shutdown, publisher, task service 등)
    for _, p := range registry.Graph(...) {
        result := p.Init(initContext)
        initialized.Add(result)
        if src, ok := instance.(TTRPCService); ok {
            ttrpcServices = append(ttrpcServices, src)
        }
    }

    // TTRPC 서버 시작
    server, _ := newServer(...)
    for _, srv := range ttrpcServices {
        srv.RegisterTTRPC(server)
    }

    serve(ctx, server, signals, sd.Shutdown, pprofHandler)
}
```

### setRuntime: 메모리 최적화

```go
// 소스: pkg/shim/shim.go:166-178

func setRuntime() {
    debug.SetGCPercent(40)      // GC 임계값을 기본 100에서 40으로 낮춤
    go func() {
        for range time.Tick(30 * time.Second) {
            debug.FreeOSMemory()  // 30초마다 OS에 메모리 반환
        }
    }()
    if os.Getenv("GOMAXPROCS") == "" {
        runtime.GOMAXPROCS(2)   // goroutine 스케줄러 스레드 수 제한
    }
}
```

**왜 이렇게 공격적으로 최적화하는가:**
노드에 수백 개의 shim이 실행될 수 있다. 각 shim이 10MB를 절약하면 1000개의 shim에서 10GB를 절약한다.

---

## 8. TTRPC 통신: containerd와 Shim 사이

### 통신 경로

```
containerd                              shim
  |                                       |
  |  ttrpc.Client                         |  ttrpc.Server
  |  TaskService_CreateTask()             |  service.Create()
  |-----(unix socket)-------------------->|
  |                                       |
  |  TaskService_StartTask()              |  service.Start()
  |-----(unix socket)-------------------->|
  |                                       |
  |  <--- 이벤트 (별도 publisher) ---      |  service.forward()
  |                                       |
```

### 소켓 전달

```go
// manager.Start() 내부
cmd.ExtraFiles = append(cmd.ExtraFiles, s.f)  // FD 3
```

Unix 소켓 파일 디스크립터를 ExtraFiles로 전달한다. 자식 프로세스에서 FD 3으로 접근 가능하다.

```go
// serve() 내부
func serveListener(socketFlag string, fd int) {
    // socketFlag가 비어있으면 fd를 사용
    l, _ = net.FileListener(os.NewFile(uintptr(fd), "socket"))
}
```

---

## 9. TaskService 구현

### service 구조체

```go
// 소스: cmd/containerd-shim-runc-v2/task/service.go:111-146

type service struct {
    mu sync.Mutex

    context  context.Context
    events   chan interface{}            // 이벤트 버퍼 (128 크기)
    platform stdio.Platform
    ec       chan runcC.Exit             // reaper 구독
    cg1oom   oom.Watcher                // cgroup v1 OOM
    cg2oom   oomv2.Interface            // cgroup v2 OOM

    publisher events.Publisher

    containers map[string]*runc.Container  // ID → Container 맵

    lifecycleMu  sync.Mutex
    running      map[int][]containerProcess    // PID → 실행 중 프로세스
    runningExecs map[*runc.Container]int       // Container → 실행 중 exec 수
    execCountSubscribers map[*runc.Container]chan<- int
    containerInitExit    map[*runc.Container]runcC.Exit

    exitSubscribers map[*map[int][]runcC.Exit]struct{}

    shutdown shutdown.Service
}
```

### NewTaskService

```go
// 소스: cmd/containerd-shim-runc-v2/task/service.go:64-108

func NewTaskService(ctx context.Context, publisher shim.Publisher, sd shutdown.Service) (taskAPI.TTRPCTaskService, error) {
    s := &service{
        context:    ctx,
        events:     make(chan interface{}, 128),  // 128개 버퍼
        ec:         reaper.Default.Subscribe(),   // exit 이벤트 구독
        publisher:  publisher,
        shutdown:   sd,
        containers: make(map[string]*runc.Container),
        running:    make(map[int][]containerProcess),
        // ...
    }

    go s.processExits()      // exit 이벤트 처리 goroutine
    s.initPlatform()         // epoll 기반 console 관리
    go s.forward(ctx, publisher)  // 이벤트 발행 goroutine

    return s, nil
}
```

### Create: 컨테이너 생성

```go
// 소스: cmd/containerd-shim-runc-v2/task/service.go:227-293

func (s *service) Create(ctx context.Context, r *taskAPI.CreateTaskRequest) (*taskAPI.CreateTaskResponse, error) {
    s.mu.Lock()
    defer s.mu.Unlock()

    // 1. 조기 종료 감지 준비
    s.lifecycleMu.Lock()
    handleStarted, cleanup := s.preStart(nil)
    s.lifecycleMu.Unlock()
    defer cleanup()

    // 2. runc 컨테이너 생성
    container, _ := runc.NewContainer(ctx, s.platform, r)
    s.containers[r.ID] = container

    // 3. TaskCreate 이벤트 발행
    s.send(&eventstypes.TaskCreate{
        ContainerID: r.ID,
        Bundle:      r.Bundle,
        Pid:         uint32(container.Pid()),
    })

    // 4. OOM 감시 시작
    switch cg := container.Cgroup().(type) {
    case cgroup1.Cgroup:
        s.cg1oom.Add(container.ID, cg)
    case *cgroupsv2.Manager:
        s.cg2oom.Add(container.ID, container.Pid(), s.oomEvent)
    }

    // 5. 시작 완료 처리
    proc, _ := container.Process("")
    handleStarted(container, proc)

    return &taskAPI.CreateTaskResponse{
        Pid: uint32(container.Pid()),
    }, nil
}
```

### Start: 프로세스 시작

```go
// 소스: cmd/containerd-shim-runc-v2/task/service.go:301-356

func (s *service) Start(ctx context.Context, r *taskAPI.StartRequest) (*taskAPI.StartResponse, error) {
    container, _ := s.getContainer(r.ID)

    // init process 또는 exec process 시작
    s.lifecycleMu.Lock()
    if r.ExecID == "" {
        cinit = container  // init process 재시작
    } else {
        // exec: init이 아직 실행 중인지 확인
        if _, initExited := s.containerInitExit[container]; initExited {
            return nil, errdefs.ErrFailedPrecondition
        }
        s.runningExecs[container]++
    }
    handleStarted, cleanup := s.preStart(cinit)
    s.lifecycleMu.Unlock()
    defer cleanup()

    // 실제 시작
    p, _ := container.Start(ctx, r)

    // 이벤트 발행
    if r.ExecID == "" {
        s.send(&eventstypes.TaskStart{...})
    } else {
        s.send(&eventstypes.TaskExecStarted{...})
    }

    handleStarted(container, p)
    return &taskAPI.StartResponse{Pid: uint32(p.Pid())}, nil
}
```

---

## 10. 프로세스 생명주기 관리

### 프로세스 상태 전이

```
  Create()
    |
    v
[Created] --- runc create (paused)
    |
  Start()
    |
    v
[Running] --- runc start
    |
    +--- Kill(SIGTERM)
    |      |
    |      v
    |   [Stopped]
    |
    +--- Pause()
    |      |
    |      v
    |   [Paused] --- Resume() ---> [Running]
    |
    +--- (프로세스 자연 종료)
           |
           v
        [Stopped]
           |
         Delete()
           |
           v
        (삭제됨)
```

### Exec: 추가 프로세스

```go
// 소스: cmd/containerd-shim-runc-v2/task/service.go:391-411

func (s *service) Exec(ctx context.Context, r *taskAPI.ExecProcessRequest) (*ptypes.Empty, error) {
    container, _ := s.getContainer(r.ID)

    // exec ID 예약 (중복 방지)
    ok, cancel := container.ReserveProcess(r.ExecID)
    if !ok {
        return nil, errdefs.ErrAlreadyExists
    }

    // exec 프로세스 생성
    process, err := container.Exec(ctx, r)
    if err != nil {
        cancel()  // 실패 시 예약 취소
        return nil, err
    }

    s.send(&eventstypes.TaskExecAdded{
        ContainerID: container.ID,
        ExecID:      process.ID(),
    })
    return empty, nil
}
```

---

## 11. processExits와 Reaping

### 프로세스 Reaping이란

Linux에서 자식 프로세스가 종료되면 부모 프로세스가 `wait()`을 호출하여 종료 상태를 수거해야 한다. 그렇지 않으면 zombie 프로세스가 된다. Shim은 **subreaper**로 설정되어 컨테이너 프로세스의 종료를 수거한다.

### processExits goroutine

```go
// 소스: cmd/containerd-shim-runc-v2/task/service.go:664-701

func (s *service) processExits() {
    for e := range s.ec {  // reaper에서 exit 이벤트 수신
        s.lifecycleMu.Lock()

        // 1. 동시 Start() 호출에 exit 알림
        for subscriber := range s.exitSubscribers {
            (*subscriber)[e.Pid] = append((*subscriber)[e.Pid], e)
        }

        // 2. running 맵에서 해당 PID의 프로세스 조회
        var cps []containerProcess
        for _, cp := range s.running[e.Pid] {
            _, init := cp.Process.(*process.Init)
            if init {
                s.containerInitExit[cp.Container] = e
            }
            cps = append(cps, cp)
        }
        delete(s.running, e.Pid)

        s.lifecycleMu.Unlock()

        // 3. exit 처리
        for _, cp := range cps {
            if ip, ok := cp.Process.(*process.Init); ok {
                s.handleInitExit(e, cp.Container, ip)
            } else {
                s.handleProcessExit(e, cp.Container, cp.Process)
            }
        }
    }
}
```

### preStart: 조기 종료 경쟁 조건 해결

```go
// 소스: cmd/containerd-shim-runc-v2/task/service.go:168-224

func (s *service) preStart(c *runc.Container) (handleStarted func(*runc.Container, process.Process), cleanup func()) {
    // exit 이벤트 구독
    exits := make(map[int][]runcC.Exit)
    s.exitSubscribers[&exits] = struct{}{}

    handleStarted = func(c *runc.Container, p process.Process) {
        pid := p.Pid()
        s.lifecycleMu.Lock()

        ees, exited := exits[pid]
        delete(s.exitSubscribers, &exits)

        if exited {
            // 이미 종료됨: 즉시 exit 처리
            s.lifecycleMu.Unlock()
            for _, ee := range ees {
                s.handleProcessExit(ee, c, p)
            }
        } else {
            // 아직 실행 중: running 맵에 추가
            s.running[pid] = append(s.running[pid], containerProcess{
                Container: c,
                Process:   p,
            })
            s.lifecycleMu.Unlock()
        }
    }

    return handleStarted, cleanup
}
```

**왜 이 복잡한 패턴이 필요한가:**
프로세스 시작과 exit 감지 사이에 경쟁 조건이 있다. 프로세스가 시작 직후 매우 빠르게 종료되면, `processExits()`가 running 맵에서 해당 PID를 찾지 못할 수 있다. `preStart`는 시작 전에 exit 이벤트를 구독하여 이 경쟁 조건을 해결한다.

---

## 12. forward() goroutine과 이벤트 전파

```go
// 소스: cmd/containerd-shim-runc-v2/task/service.go:813-823

func (s *service) forward(ctx context.Context, publisher shim.Publisher) {
    ns, _ := namespaces.Namespace(ctx)
    ctx = namespaces.WithNamespace(context.Background(), ns)
    for e := range s.events {
        err := publisher.Publish(ctx, runtime.GetTopic(e), e)
        if err != nil {
            log.G(ctx).WithError(err).Error("post event")
        }
    }
    publisher.Close()
}
```

### 이벤트 흐름

```
TaskService 메서드                events 채널              forward()
  |                                  |                        |
  | s.send(TaskCreate{...})         |                        |
  |-----> events <- TaskCreate ---->|-----> publisher.Publish |
  |                                  |       (ttrpc → containerd)
  | s.send(TaskStart{...})          |                        |
  |-----> events <- TaskStart  ---->|-----> publisher.Publish |
  |                                  |                        |
  | s.send(TaskExit{...})           |                        |
  |-----> events <- TaskExit   ---->|-----> publisher.Publish |
```

`events` 채널은 128개 버퍼를 가진다. 이는 containerd에 이벤트를 전달하는 publisher가 일시적으로 느려도 shim의 주요 로직이 블록되지 않도록 하기 위함이다.

### send 메서드

```go
// 소스: cmd/containerd-shim-runc-v2/task/service.go:712-714

func (s *service) send(evt interface{}) {
    s.events <- evt
}
```

---

## 13. OOM 감시

### cgroup v1 OOM

```go
// NewTaskService 내부
if cgroups.Mode() != cgroups.Unified {
    ep, _ = oomv1.New(publisher)
    go ep.Run(ctx)
}
```

cgroup v1에서는 `memory.oom_control`의 eventfd를 감시한다.

### cgroup v2 OOM

```go
// Create 내부
s.cg2oom.Add(container.ID, container.Pid(), s.oomEvent)
```

cgroup v2에서는 `memory.events`를 감시한다.

### OOM 이벤트 핸들러

```go
// 소스: cmd/containerd-shim-runc-v2/task/service.go:703-710

func (s *service) oomEvent(id string) {
    err := s.publisher.Publish(s.context, runtime.TaskOOMEventTopic, &eventstypes.TaskOOM{
        ContainerID: id,
    })
}
```

OOM 이벤트는 containerd에 전파되어, 모니터링 시스템이나 CRI를 통해 kubelet에 보고된다.

---

## 14. Shim 그룹핑과 Pod 지원

### 그룹 라벨

```go
// 소스: cmd/containerd-shim-runc-v2/manager/manager_linux.go:65-68

var groupLabels = []string{
    "io.containerd.runc.v2.group",
    "io.kubernetes.cri.sandbox-id",
}
```

### 그룹핑 로직

```go
// Start() 내부
grouping := id
spec, _ := readSpec()
for _, group := range groupLabels {
    if groupID, ok := spec.Annotations[group]; ok {
        grouping = groupID  // sandbox ID로 그룹핑
        break
    }
}

s, err := newShimSocket(ctx, opts.Address, grouping, false)
if errdefs.IsAlreadyExists(err) {
    // 이미 같은 그룹의 shim이 실행 중 → 기존 shim 재사용
    params.Address = s.addr
    return params, nil
}
```

### Pod 시나리오

```
Pod (sandbox-id: "pod-abc123")
  |
  +-- pause container (sandbox)
  |     → shim 생성 (socket: /run/.../pod-abc123)
  |
  +-- app container
  |     → grouping = "pod-abc123"
  |     → 기존 shim 재사용! (socket: /run/.../pod-abc123)
  |
  +-- sidecar container
        → grouping = "pod-abc123"
        → 기존 shim 재사용!
```

같은 Pod의 모든 컨테이너가 **하나의 shim 프로세스**를 공유한다. 이로써:
- 메모리 절약 (shim 프로세스 수 감소)
- 같은 Pod 내 컨테이너 간 일관된 이벤트 처리
- kubelet과의 효율적 통신

---

## 15. Shim 전체 생명주기

```
Phase 1: 시작
  containerd ──exec──> shim binary ("start" 서브커맨드)
                         |
                         +── socket 생성
                         +── 새 shim 프로세스 fork
                         +── BootstrapParams stdout 출력
                         +── 종료
                              |
                              v
                         새 shim 프로세스 (서비스 모드)
                         +── subreaper 설정
                         +── 플러그인 로딩
                         +── ttrpc 서버 시작
                         +── processExits goroutine
                         +── forward goroutine

Phase 2: 동작
  containerd ──ttrpc──> shim
                         |
  Create ──────────────> runc create (paused container)
  Start ───────────────> runc start
  Exec ────────────────> runc exec
  Kill ────────────────> runc kill
  Wait ────────────────> 블로킹 (exit까지)

Phase 3: 종료
  프로세스 종료 ──> reaper가 감지
                    |
                    +── processExits()
                    +── handleProcessExit()
                    +── TaskExit 이벤트
                    |
  containerd ──> Delete ──> shim
                              |
                              +── runc delete
                              +── containers 맵에서 제거
                              +── containers가 비면 Shutdown
                              +── ttrpc 서버 종료
                              +── shim 프로세스 종료

Phase 4: 정리 (비정상 종료 시)
  containerd ──exec──> shim binary ("delete" 서브커맨드)
                         |
                         +── Manager.Stop()
                         +── runc delete --force
                         +── rootfs unmount
                         +── StopStatus stdout 출력
                         +── 종료
```

이 생명주기는 containerd가 재시작되더라도 Phase 2에서 Phase 3로의 전이를 shim이 독립적으로 관리할 수 있음을 보장한다. containerd는 재시작 후 `bootstrap.json`을 읽어 기존 shim에 재연결하여, 마치 재시작이 없었던 것처럼 동작을 계속한다.
