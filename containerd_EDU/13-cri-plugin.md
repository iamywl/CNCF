# 13. CRI 플러그인 (Container Runtime Interface)

## 목차

1. [개요](#1-개요)
2. [CRI란 무엇인가](#2-cri란-무엇인가)
3. [CRI 플러그인 아키텍처](#3-cri-플러그인-아키텍처)
4. [CRI 플러그인 등록 및 초기화](#4-cri-플러그인-등록-및-초기화)
5. [criService 구조체 분석](#5-criservice-구조체-분석)
6. [RunPodSandbox 흐름](#6-runpodsandbox-흐름)
7. [CreateContainer 흐름](#7-createcontainer-흐름)
8. [StartContainer 흐름](#8-startcontainer-흐름)
9. [StopContainer 흐름](#9-stopcontainer-흐름)
10. [RemoveContainer 흐름](#10-removecontainer-흐름)
11. [Pod와 Container의 관계](#11-pod와-container의-관계)
12. [EventMonitor와 상태 동기화](#12-eventmonitor와-상태-동기화)
13. [CRI 서비스 생명주기](#13-cri-서비스-생명주기)
14. [왜 이렇게 설계했는가](#14-왜-이렇게-설계했는가)

---

## 1. 개요

containerd의 CRI(Container Runtime Interface) 플러그인은 Kubernetes의 kubelet이 containerd를 컨테이너 런타임으로 사용할 수 있게 해주는 핵심 구성 요소다. CRI 플러그인은 gRPC 서버로 동작하며, Kubernetes의 RuntimeService와 ImageService API를 구현한다.

**핵심 소스 파일:**

| 파일 | 역할 |
|------|------|
| `plugins/cri/cri.go` | CRI gRPC 플러그인 등록, criGRPCServer |
| `internal/cri/server/service.go` | criService 구현체, NewCRIService |
| `internal/cri/server/sandbox_run.go` | RunPodSandbox 구현 |
| `internal/cri/server/container_create.go` | CreateContainer 구현 |
| `internal/cri/server/container_start.go` | StartContainer 구현 |
| `internal/cri/server/container_stop.go` | StopContainer 구현 |
| `internal/cri/server/container_remove.go` | RemoveContainer 구현 |
| `internal/cri/server/events/events.go` | EventMonitor, backOff |

---

## 2. CRI란 무엇인가

CRI(Container Runtime Interface)는 Kubernetes가 다양한 컨테이너 런타임을 일관된 방식으로 사용할 수 있도록 정의한 gRPC 인터페이스다.

### CRI v1 API 구성

```
CRI v1 API
├── RuntimeService          # 컨테이너/샌드박스 생명주기 관리
│   ├── RunPodSandbox       # Pod 샌드박스 생성 및 시작
│   ├── StopPodSandbox      # Pod 샌드박스 중지
│   ├── RemovePodSandbox    # Pod 샌드박스 제거
│   ├── PodSandboxStatus    # Pod 샌드박스 상태 조회
│   ├── ListPodSandbox      # Pod 샌드박스 목록 조회
│   ├── CreateContainer     # 컨테이너 생성
│   ├── StartContainer      # 컨테이너 시작
│   ├── StopContainer       # 컨테이너 중지
│   ├── RemoveContainer     # 컨테이너 제거
│   ├── ListContainers      # 컨테이너 목록 조회
│   ├── ContainerStatus     # 컨테이너 상태 조회
│   ├── ExecSync            # 동기 명령 실행
│   ├── Exec                # 비동기 명령 실행 (스트리밍)
│   ├── Attach              # 컨테이너 연결 (스트리밍)
│   └── PortForward         # 포트 포워딩 (스트리밍)
│
└── ImageService            # 이미지 관리
    ├── ListImages          # 이미지 목록
    ├── ImageStatus         # 이미지 상태
    ├── PullImage           # 이미지 풀
    ├── RemoveImage         # 이미지 삭제
    └── ImageFsInfo         # 이미지 파일시스템 정보
```

### 왜 CRI가 필요한가

Kubernetes 초기에는 Docker가 유일한 런타임이었다. 하지만 containerd, CRI-O 등 다양한 런타임이 등장하면서, kubelet이 특정 런타임에 종속되지 않도록 추상화 계층이 필요했다. CRI는 이 추상화를 gRPC 인터페이스로 정의하여, 어떤 런타임이든 CRI를 구현하면 Kubernetes와 통합할 수 있게 했다.

---

## 3. CRI 플러그인 아키텍처

```
+-------------------------------------------------------------------+
|                         kubelet                                    |
|                           |                                        |
|                    CRI gRPC 호출                                   |
+-------------------------------------------------------------------+
                            |
                            v
+-------------------------------------------------------------------+
|                    containerd CRI Plugin                           |
|                                                                    |
|  +--------------------+    +-------------------+                   |
|  | criGRPCServer      |    | instrument.Service|  <-- 메트릭 계측  |
|  | ├ RuntimeService   |--->| (래퍼)            |                   |
|  | └ ImageService     |    +-------------------+                   |
|  +--------------------+              |                             |
|                                      v                             |
|  +-----------------------------------------------------------+    |
|  |                     criService                             |    |
|  |                                                            |    |
|  |  +-----------+  +-------------+  +-------------+           |    |
|  |  | sandbox   |  | container   |  | netPlugin   |           |    |
|  |  | Store     |  | Store       |  | (CNI)       |           |    |
|  |  +-----------+  +-------------+  +-------------+           |    |
|  |                                                            |    |
|  |  +-----------+  +-------------+  +-------------+           |    |
|  |  | sandbox   |  | event       |  | stream      |           |    |
|  |  | Service   |  | Monitor     |  | Server      |           |    |
|  |  +-----------+  +-------------+  +-------------+           |    |
|  +-----------------------------------------------------------+    |
|                            |                                       |
+-------------------------------------------------------------------+
                            |
                            v
+-------------------------------------------------------------------+
|                    containerd Core                                  |
|  (Content Store, Snapshotter, Metadata DB, Task/Runtime)           |
+-------------------------------------------------------------------+
```

---

## 4. CRI 플러그인 등록 및 초기화

### 플러그인 등록

CRI 플러그인은 `plugins/cri/cri.go`의 `init()` 함수에서 등록된다.

```go
// 소스: plugins/cri/cri.go (47-66행)
func init() {
    defaultConfig := criconfig.DefaultServerConfig()
    registry.Register(&plugin.Registration{
        Type: plugins.GRPCPlugin,
        ID:   "cri",
        Requires: []plugin.Type{
            plugins.CRIServicePlugin,
            plugins.PodSandboxPlugin,
            plugins.SandboxControllerPlugin,
            plugins.NRIApiPlugin,
            plugins.EventPlugin,
            plugins.ServicePlugin,
            plugins.LeasePlugin,
            plugins.SandboxStorePlugin,
            plugins.TransferPlugin,
            plugins.WarningPlugin,
        },
        Config:          &defaultConfig,
        ConfigMigration: configMigration,
        InitFn:          initCRIService,
    })
}
```

### 의존성 목록

| 플러그인 타입 | 역할 |
|-------------|------|
| CRIServicePlugin | Runtime/Image 서비스 (런타임 구성, 이미지 관리) |
| PodSandboxPlugin | Pod 샌드박스 컨트롤러 (pause 컨테이너 관리) |
| SandboxControllerPlugin | 샌드박스 생명주기 관리 |
| NRIApiPlugin | Node Resource Interface (NRI) 훅 |
| EventPlugin | containerd 이벤트 교환기 |
| ServicePlugin | containerd 서비스 접근 |
| LeasePlugin | Lease 관리 (GC 보호) |
| SandboxStorePlugin | 샌드박스 메타데이터 저장소 |
| TransferPlugin | 이미지 전송 서비스 |
| WarningPlugin | 경고 메시지 발행 |

### 초기화 흐름 (initCRIService)

```
initCRIService(ic *plugin.InitContext)
    |
    +-- (1) CRI Runtime 플러그인 로드 → criRuntimePlugin
    |
    +-- (2) CRI Image 플러그인 로드 → criImagePlugin
    |
    +-- (3) 런타임별 Snapshotter를 이미지 서비스에 전파
    |
    +-- (4) ServerConfig 유효성 검증
    |
    +-- (5) containerd 클라이언트 생성
    |       containerd.New("", WithDefaultNamespace("k8s.io"),
    |                         WithInMemoryServices(ic))
    |
    +-- (6) Sandbox 컨트롤러 수집 (getSandboxControllers)
    |       ├── SandboxControllerPlugin 타입 수집
    |       └── PodSandboxPlugin 타입 수집
    |
    +-- (7) 스트리밍 설정 (exec/attach/portforward)
    |
    +-- (8) CRIServiceOptions 구성
    |
    +-- (9) server.NewCRIService(options) 호출
    |
    +-- (10) RegisterReadiness() → goroutine에서 s.Run(ready) 실행
    |
    +-- (11) criGRPCServer 생성 반환
```

### criGRPCServer 구조체

```go
// 소스: plugins/cri/cri.go (180-185행)
type criGRPCServer struct {
    runtime.RuntimeServiceServer
    runtime.ImageServiceServer
    io.Closer
    initializer
}
```

criGRPCServer는 RuntimeService와 ImageService를 모두 포함하며, gRPC 서버에 등록할 때 `instrument.NewService(c)`로 감싸서 메트릭 계측을 추가한다.

```go
// 소스: plugins/cri/cri.go (187-192행)
func (c *criGRPCServer) register(s *grpc.Server) error {
    instrumented := instrument.NewService(c)
    runtime.RegisterRuntimeServiceServer(s, instrumented)
    runtime.RegisterImageServiceServer(s, instrumented)
    return nil
}
```

---

## 5. criService 구조체 분석

`criService`는 CRI의 실제 비즈니스 로직을 담당하는 핵심 구조체다.

```go
// 소스: internal/cri/server/service.go (119-171행)
type criService struct {
    runtime.UnimplementedRuntimeServiceServer
    runtime.UnimplementedImageServiceServer

    RuntimeService                              // 런타임 설정 접근
    ImageService                                // 이미지 서비스

    config           criconfig.Config           // 전체 설정
    imageFSPaths     map[string]string          // 스냅샷터별 이미지 FS 경로
    os               osinterface.OS             // OS 추상화
    sandboxStore     *sandboxstore.Store         // 샌드박스 메타데이터
    sandboxNameIndex *registrar.Registrar        // 샌드박스 이름 유일성 보장
    containerStore   *containerstore.Store       // 컨테이너 메타데이터
    containerNameIndex *registrar.Registrar      // 컨테이너 이름 유일성 보장
    netPlugin        map[string]cni.CNI          // CNI 네트워크 플러그인
    client           *containerd.Client          // containerd 클라이언트
    streamServer     streaming.Server            // exec/attach 스트리밍
    eventMonitor     *events.EventMonitor        // containerd 이벤트 모니터
    initialized      atomic.Bool                 // 초기화 완료 플래그
    cniNetConfMonitor map[string]*cniNetConfSyncer // CNI 설정 변경 감시
    containerEventsQ eventq.EventQueue           // 컨테이너 이벤트 큐
    nri              *nri.API                    // NRI 인터페이스
    sandboxService   sandboxService              // 샌드박스 CRUD
    runtimeHandlers  map[string]*runtime.RuntimeHandler
    runtimeFeatures  *runtime.RuntimeFeatures
    statsCollector   *StatsCollector             // CPU 사용량 수집기
}
```

### criService 의존성 관계도

```
criService
├── RuntimeService (설정)
│   └── Config() criconfig.Config
├── ImageService (이미지)
│   ├── PullImage()
│   ├── GetImage()
│   └── RuntimeSnapshotter()
├── sandboxStore (인메모리)
│   └── Get/Add/Delete sandbox.Sandbox
├── containerStore (인메모리)
│   └── Get/Add/Delete container.Container
├── sandboxService (샌드박스 컨트롤러)
│   ├── CreateSandbox()
│   ├── StartSandbox()
│   ├── StopSandbox()
│   └── WaitSandbox()
├── client (*containerd.Client)
│   ├── SandboxStore()    → 영속 메타데이터
│   ├── LeasesService()   → GC 보호
│   └── TaskService()     → 태스크 관리
├── netPlugin (CNI)
│   ├── Setup()           → 네트워크 설정
│   └── Remove()          → 네트워크 해제
├── eventMonitor
│   ├── Subscribe()       → 이벤트 구독
│   └── Start()           → 이벤트 처리 루프
├── streamServer
│   └── Start()           → exec/attach 스트리밍
└── nri (NRI)
    ├── RunPodSandbox()
    ├── StartContainer()
    └── StopContainer()
```

---

## 6. RunPodSandbox 흐름

`RunPodSandbox`는 Kubernetes Pod의 인프라 컨테이너(샌드박스)를 생성하고 시작하는 과정이다.

### 전체 흐름 다이어그램

```
kubelet → RunPodSandbox(config, runtimeHandler)
    |
    +-- (1) ID 생성: util.GenerateID()
    |
    +-- (2) 이름 예약: sandboxNameIndex.Reserve(name, id)
    |       └── 동일 이름 동시 생성 방지
    |
    +-- (3) Lease 생성: leaseSvc.Create(ctx, leases.WithID(id))
    |       └── GC로부터 리소스 보호
    |
    +-- (4) 런타임 결정: config.GetSandboxRuntime(config, runtimeHandler)
    |       └── sandboxInfo.Runtime.Name, sandboxInfo.Sandboxer 설정
    |
    +-- (5) Sandbox 메타데이터 생성
    |       ├── sandboxstore.NewSandbox(Metadata, Status)
    |       └── client.SandboxStore().Create(ctx, sandboxInfo)
    |
    +-- (6) 네트워크 네임스페이스 생성 (non-host 네트워크)
    |       ├── netns.NewNetNS(netnsMountDir)
    |       └── sandbox.NetNSPath 설정
    |
    +-- (7) CNI 네트워크 설정: setupPodNetwork(ctx, &sandbox)
    |       ├── netPlugin.Setup(ctx, id, path, opts...)
    |       └── sandbox.IP, sandbox.AdditionalIPs 설정
    |
    +-- (8) 샌드박스 컨트롤러 생성:
    |       sandboxService.CreateSandbox(ctx, sandboxInfo, opts...)
    |
    +-- (9) Pause 이미지 확보: ensurePauseImageExists()
    |
    +-- (10) 샌드박스 시작:
    |        sandboxService.StartSandbox(ctx, sandboxer, id)
    |        └── ctrl.Pid, ctrl.Address, ctrl.Labels 반환
    |
    +-- (11) 상태 업데이트: status.State = StateReady
    |
    +-- (12) 인메모리 저장: sandboxStore.Add(sandbox)
    |
    +-- (13) 이벤트 발행: CONTAINER_CREATED_EVENT, CONTAINER_STARTED_EVENT
    |
    +-- (14) 종료 모니터: startSandboxExitMonitor()
    |
    └── return RunPodSandboxResponse{PodSandboxId: id}
```

### 오류 시 정리 (defer 체인)

RunPodSandbox는 여러 개의 defer를 사용하여, 중간에 실패하면 이전 단계에서 할당한 리소스를 역순으로 정리한다.

```
defer 순서 (LIFO):
  1. sandboxNameIndex.ReleaseByName(name)
  2. leaseSvc.Delete(ctx, ls)
  3. client.SandboxStore().Delete(ctx, id)
  4. sandboxStore.Add(sandbox) [cleanupErr 있을 때만]
  5. sandbox.NetNS.Remove()
  6. teardownPodNetwork(ctx, sandbox)
  7. nri.RemovePodSandbox(ctx, &sandbox)
```

`cleanupErr` 변수가 핵심이다. 정리 중 오류가 발생하면 cleanupErr에 기록하고, 이후 defer는 cleanupErr가 nil인 경우에만 실행된다. cleanupErr가 nil이 아니면, 샌드박스를 StateUnknown 상태로 인메모리 스토어에 추가하여 나중에 kubelet의 syncPod가 정리하도록 남겨둔다.

---

## 7. CreateContainer 흐름

`CreateContainer`는 기존 Pod 샌드박스 안에 새 컨테이너를 생성한다.

```go
// 소스: internal/cri/server/container_create.go (60행)
func (c *criService) CreateContainer(ctx context.Context,
    r *runtime.CreateContainerRequest) (_ *runtime.CreateContainerResponse, retErr error)
```

### 주요 단계

```
kubelet → CreateContainer(podSandboxId, config, sandboxConfig)
    |
    +-- (1) 샌드박스 조회: sandboxStore.Get(podSandboxId)
    |
    +-- (2) 샌드박스 상태 확인: sandboxService.SandboxStatus()
    |
    +-- (3) ID/이름 생성 및 예약
    |       ├── id = util.GenerateID()
    |       └── containerNameIndex.Reserve(name, id)
    |
    +-- (4) 체크포인트 이미지 확인 (CRIU 복원 경로)
    |
    +-- (5) 이미지 해석: LocalResolve(config.Image)
    |
    +-- (6) OCI 스펙 생성
    |       ├── 기본 스펙 로드 (BaseRuntimeSpec)
    |       ├── 마운트 설정 (볼륨, hostPath)
    |       ├── Linux 네임스페이스 설정
    |       ├── 장치 설정 (devices)
    |       ├── 리소스 제한 (cgroups)
    |       ├── SELinux 레이블
    |       └── Seccomp/AppArmor 프로필
    |
    +-- (7) 스냅샷 준비
    |       ├── Snapshotter.Prepare() → 이미지 레이어 위에 RW 레이어 생성
    |       └── 스냅샷 키 생성
    |
    +-- (8) containerd 컨테이너 생성
    |       container, err = client.NewContainer(ctx, id, opts...)
    |
    +-- (9) I/O 설정 (stdout/stderr FIFO)
    |
    +-- (10) 메타데이터 저장
    |        ├── containerStore.Add(cntr)
    |        └── CONTAINER_CREATED_EVENT 발행
    |
    └── return CreateContainerResponse{ContainerId: id}
```

### OCI 스펙 생성 세부사항

containerd CRI 플러그인은 Kubernetes 컨테이너 설정을 OCI 런타임 스펙으로 변환한다:

```
CRI ContainerConfig → OCI Spec 변환
├── Image.Config         → Process.Env, Process.Args, Process.Cwd
├── SecurityContext      → Process.User (UID/GID)
│   ├── RunAsUser        → Process.User.UID
│   ├── RunAsGroup       → Process.User.GID
│   ├── Capabilities     → Process.Capabilities
│   ├── SELinuxOptions   → Process.SelinuxLabel
│   └── Seccomp          → Linux.Seccomp
├── Mounts               → Mounts[]
│   ├── HostPath         → Source
│   ├── ContainerPath    → Destination
│   └── Readonly         → Options
├── Linux.Resources      → Linux.Resources
│   ├── CpuPeriod/Quota  → CPU
│   ├── MemoryLimit      → Memory.Limit
│   └── OomScoreAdj      → Process.OOMScoreAdj
└── Envs                 → Process.Env (추가)
```

---

## 8. StartContainer 흐름

```go
// 소스: internal/cri/server/container_start.go (45행)
func (c *criService) StartContainer(ctx context.Context,
    r *runtime.StartContainerRequest) (retRes *runtime.StartContainerResponse, retErr error)
```

### 주요 단계

```
kubelet → StartContainer(containerId)
    |
    +-- (1) 컨테이너 조회: containerStore.Get(containerId)
    |
    +-- (2) 시작 상태 설정: setContainerStarting(cntr)
    |       └── CONTAINER_CREATED 상태에서만 허용
    |       └── Starting = true (동시 시작/제거 방지)
    |
    +-- (3) 샌드박스 상태 확인: sandbox.Status == StateReady
    |
    +-- (4) I/O 생성: createContainerLoggers(logPath, tty)
    |       └── CRI 로그 형식으로 stdout/stderr 기록
    |
    +-- (5) CRIU 복원 확인 (cntr.Status.Get().Restore)
    |       └── 체크포인트에서 복원하는 경우 별도 경로
    |
    +-- (6) containerd Task 생성
    |       task, err = container.NewTask(ctx, ioCreation, taskOpts...)
    |       └── shim endpoint 사용 (sandbox.Endpoint)
    |
    +-- (7) Exit 채널 대기: task.Wait(ctx) → exitCh
    |
    +-- (8) NRI 알림: nri.StartContainer(ctx, &sandbox, &cntr)
    |
    +-- (9) Task 시작: task.Start(ctx)
    |
    +-- (10) 상태 업데이트: Pid, StartedAt
    |
    +-- (11) 종료 모니터: startContainerExitMonitor(ctx, id, pid, exitCh)
    |
    +-- (12) 이벤트 발행: CONTAINER_STARTED_EVENT
    |
    +-- (13) NRI 후처리: nri.PostStartContainer()
    |
    └── return StartContainerResponse{}
```

### 왜 Task 생성과 시작이 분리되어 있는가

containerd에서 Task 생성(NewTask)과 시작(Start)은 별도 단계다:
- **NewTask**: shim 프로세스를 시작하고 OCI 번들을 준비한다. I/O 파이프도 이 단계에서 설정된다.
- **Start**: 실제 컨테이너 프로세스를 실행한다.

이 분리 덕분에 NRI 훅을 Start 전에 호출하여 리소스 조정을 할 수 있다.

---

## 9. StopContainer 흐름

```go
// 소스: internal/cri/server/container_stop.go (39행)
func (c *criService) StopContainer(ctx context.Context,
    r *runtime.StopContainerRequest) (*runtime.StopContainerResponse, error)
```

### 2단계 중지 전략

```
kubelet → StopContainer(containerId, timeout)
    |
    +-- timeout > 0인 경우:
    |   |
    |   +-- (1) StopSignal 결정 (SIGTERM 기본)
    |   |       └── 이미지 Config의 StopSignal 또는 container.StopSignal
    |   |
    |   +-- (2) SIGTERM 전송: task.Kill(ctx, sig)
    |   |       └── IsStopSignaledWithTimeout로 중복 전송 방지
    |   |
    |   +-- (3) timeout 대기: waitContainerStop(sigTermCtx, container)
    |   |       └── container.Stopped() 채널 또는 타임아웃
    |   |
    |   +-- (4) 타임아웃 시 SIGKILL 전송
    |   |       task.Kill(ctx, syscall.SIGKILL)
    |   |
    |   └-- (5) 최종 대기: waitContainerStop(ctx, container)
    |
    +-- timeout == 0인 경우:
    |   └── 즉시 SIGKILL 전송
    |
    └── NRI 알림: nri.StopContainer()
```

### 재시도 메커니즘

```go
// 소스: internal/cri/server/container_stop.go (87-108행)
func (c *criService) stopContainerRetryOnConnectionClosed(
    ctx context.Context, container containerstore.Container, timeout time.Duration) error {
    const maxRetries = 3
    for i := 1; i <= maxRetries; i++ {
        err = c.stopContainer(ctx, container, timeout)
        if err == nil {
            return nil
        }
        if !ctrdutil.IsShimTTRPCClosed(err) {
            return err
        }
        // ttrpc 연결이 닫힌 경우 = 컨테이너가 자체 종료됨
        retryAfter := time.Duration(100*i*i) * time.Millisecond
        time.Sleep(retryAfter)
    }
    return err
}
```

shim의 ttrpc 연결이 닫혔다는 것은 컨테이너가 이미 자체 종료되었을 가능성이 높다. 이 경우 최대 3회까지 지수적 백오프(100ms, 400ms, 900ms)로 재시도한다.

---

## 10. RemoveContainer 흐름

```go
// 소스: internal/cri/server/container_remove.go (34행)
func (c *criService) RemoveContainer(ctx context.Context,
    r *runtime.RemoveContainerRequest) (_ *runtime.RemoveContainerResponse, retErr error)
```

### 제거 단계

```
kubelet → RemoveContainer(containerId)
    |
    +-- (1) 컨테이너 조회 (없으면 성공 반환 - 멱등성)
    |
    +-- (2) 실행 중이면 강제 중지: stopContainer(ctx, container, 0)
    |       └── timeout=0 → 즉시 SIGKILL
    |
    +-- (3) Removing 상태 설정: setContainerRemoving(container)
    |       └── RUNNING/UNKNOWN이면 거부
    |       └── Starting이면 거부
    |
    +-- (4) NRI 알림: nri.RemoveContainer()
    |
    +-- (5) containerd 컨테이너 삭제
    |       container.Container.Delete(ctx, WithSnapshotCleanup)
    |       └── 스냅샷도 함께 정리
    |
    +-- (6) 체크포인트 삭제: container.Delete()
    |
    +-- (7) 루트 디렉토리 삭제
    |       ├── containerRootDir 삭제
    |       └── volatileContainerRootDir 삭제
    |
    +-- (8) 인메모리 정리
    |       ├── containerStore.Delete(id)
    |       └── containerNameIndex.ReleaseByKey(id)
    |
    +-- (9) 이벤트 발행: CONTAINER_DELETED_EVENT
    |
    └── return RemoveContainerResponse{}
```

---

## 11. Pod와 Container의 관계

### Kubernetes Pod 모델과 containerd Sandbox

```
+-------------------------------------------------------+
|                    Kubernetes Pod                       |
|                                                        |
|  +-----------+  +-----------+  +-----------+           |
|  | Container |  | Container |  | Container |           |
|  | (app1)    |  | (app2)    |  | (sidecar) |           |
|  +-----------+  +-----------+  +-----------+           |
|                                                        |
|  +--------------------------------------------------+  |
|  |             Sandbox (Pause Container)             |  |
|  |  ┌─────────────────────────────────────────────┐  |  |
|  |  │ Network Namespace (shared)                  │  |  |
|  |  │ IPC Namespace (shared)                      │  |  |
|  |  │ PID Namespace (optional, shared)            │  |  |
|  |  └─────────────────────────────────────────────┘  |  |
|  +--------------------------------------------------+  |
+-------------------------------------------------------+
```

### 데이터 모델 매핑

| Kubernetes 개념 | containerd 구현 |
|----------------|----------------|
| Pod | Sandbox (sandbox.Sandbox) |
| Pod 인프라 | Pause 컨테이너 (PodSandbox Controller) |
| 앱 컨테이너 | containers.Container + Task |
| Pod 네트워크 | NetNS + CNI 설정 |
| Pod 상태 | sandboxstore.Status |
| 컨테이너 상태 | containerstore.Status |

### 관계 구조

```
sandboxStore
  └── Sandbox
      ├── ID: "abc123"
      ├── Metadata
      │   ├── Name: "k8s_POD_my-app_default_uid_0"
      │   ├── Config: *PodSandboxConfig
      │   └── RuntimeHandler: "runc"
      ├── Status: StateReady
      ├── Sandboxer: "podsandbox"
      ├── NetNSPath: "/var/run/netns/cni-xxx"
      ├── IP: "10.244.1.5"
      └── Endpoint: {Address, Version}

containerStore
  └── Container
      ├── ID: "def456"
      ├── Metadata
      │   ├── Name: "k8s_nginx_my-app_default_uid_0"
      │   ├── SandboxID: "abc123"  ← 샌드박스 참조
      │   ├── Config: *ContainerConfig
      │   └── ImageRef: "sha256:..."
      ├── Status: CONTAINER_RUNNING
      ├── Container: containerd.Container  ← containerd 핸들
      └── StopSignal: "SIGTERM"
```

---

## 12. EventMonitor와 상태 동기화

### EventMonitor 아키텍처

```go
// 소스: internal/cri/server/events/events.go (46-53행)
type EventMonitor struct {
    ch           <-chan *events.Envelope   // 이벤트 수신 채널
    errCh        <-chan error              // 에러 채널
    ctx          context.Context
    cancel       context.CancelFunc
    backOff      *backOff                  // 백오프 재시도 큐
    eventHandler EventHandler              // 이벤트 처리 인터페이스
}
```

### 이벤트 처리 흐름

```
containerd Events
    |
    +-- Subscribe(filters: topic=="/tasks/oom", topic~="/images/")
    |
    v
EventMonitor.Start() goroutine
    |
    +-- 이벤트 수신 ←── em.ch
    |   |
    |   +-- namespace != "k8s.io" → 무시
    |   |
    |   +-- convertEvent(e.Event) → id, evt
    |   |   ├── TaskOOM → ContainerID
    |   |   ├── SandboxExit → SandboxID
    |   |   ├── ImageCreate/Update/Delete → Name
    |   |   └── TaskExit → ContainerID
    |   |
    |   +-- backOff 중? → enBackOff(id, evt) → 큐에 저장
    |   |
    |   +-- eventHandler.HandleEvent(evt) 호출
    |   |   └── 실패 시 → enBackOff(id, evt)
    |   |
    +-- 백오프 만료 체크 ←── backOffCheckCh (1초마다)
    |   |
    |   +-- 만료된 ID들 조회
    |   +-- 큐에서 이벤트 꺼내어 순서대로 재처리
    |   └── 실패 시 → reBackOff(id, events, duration*2)
    |
    +-- 에러 수신 ←── em.errCh
    |   └── 스트림 오류 → errCh로 전파
    |
    └── 컨텍스트 취소 ←── em.ctx.Done()
```

### 백오프 메커니즘

```go
// 소스: internal/cri/server/events/events.go (36-38행)
const (
    backOffInitDuration        = 1 * time.Second     // 초기 대기
    backOffMaxDuration         = 5 * time.Minute     // 최대 대기
    backOffExpireCheckDuration = 1 * time.Second     // 체크 주기
)
```

백오프는 이벤트 처리 실패 시 기하급수적으로 대기 시간을 늘린다:
- 1회 실패: 1초 대기
- 2회 실패: 2초 대기
- 3회 실패: 4초 대기
- ...
- 최대: 5분 대기

같은 ID에 대한 이벤트는 큐에 순서대로 쌓이며, 백오프가 만료되면 큐의 이벤트를 순서대로 처리한다.

---

## 13. CRI 서비스 생명주기

### Run() 시작 흐름

```go
// 소스: internal/cri/server/service.go (279-378행)
func (c *criService) Run(ready func()) error
```

```
Run(ready)
    |
    +-- (1) eventMonitor.Subscribe(client, filters)
    |       └── topic=="/tasks/oom", topic~="/images/"
    |
    +-- (2) statsCollector.Start()
    |       └── 백그라운드 CPU 사용량 수집
    |
    +-- (3) recover(ctx) — 상태 복구
    |       └── containerd에 남아있는 컨테이너/샌드박스 동기화
    |
    +-- (4) eventMonitor.Start() → eventMonitorErrCh
    |
    +-- (5) CNI 설정 동기화 시작
    |       └── 각 네트워크 플러그인의 syncLoop() goroutine
    |
    +-- (6) streamServer.Start(true) → streamServerErrCh
    |       └── exec/attach/portforward HTTP 서버
    |
    +-- (7) nri.Register()
    |
    +-- (8) initialized.Store(true)
    |       └── 이 시점부터 gRPC 요청 처리 가능
    |
    +-- (9) ready() 콜백 호출
    |
    +-- (10) select {} — 크리티컬 서비스 중 하나가 종료될 때까지 대기
    |       ├── eventMonitorErrCh
    |       ├── streamServerErrCh
    |       └── cniNetConfMonitorErrCh
    |
    └-- (11) Close() → 모든 서비스 정리
```

### Close() 정리 순서

```go
// 소스: internal/cri/server/service.go (382-397행)
func (c *criService) Close() error {
    // 1. CNI 네트워크 설정 모니터 중지
    for name, h := range c.cniNetConfMonitor {
        h.stop()
    }
    // 2. 이벤트 모니터 중지
    c.eventMonitor.Stop()
    // 3. 통계 수집기 중지
    c.statsCollector.Stop()
    // 4. 스트리밍 서버 중지
    c.streamServer.Stop()
    return nil
}
```

---

## 14. 왜 이렇게 설계했는가

### Q1: 왜 CRI 플러그인이 containerd 프로세스 내부에 내장되어 있는가?

초기 containerd-shim-runc-v1 시절에는 CRI 플러그인이 별도 프로세스(cri-containerd)로 동작했다. 하지만 이 구조는 불필요한 gRPC 호출 오버헤드와 프로세스 관리 복잡성을 초래했다. containerd 1.1부터 CRI를 내장 플러그인으로 전환하여 성능과 운영 편의성을 개선했다.

### Q2: 왜 RuntimeService와 ImageService가 분리되어 있는가?

CRI 플러그인에서 RuntimeService와 ImageService는 별도의 CRIServicePlugin으로 분리되어 있다:

```go
// plugins/cri/cri.go (73-82행)
criRuntimePlugin, err := ic.GetByID(plugins.CRIServicePlugin, "runtime")
criImagePlugin, err := ic.GetByID(plugins.CRIServicePlugin, "images")
```

이유:
- **독립적 설정**: 이미지 서비스는 레지스트리/미러/인증 설정을, 런타임 서비스는 OCI 런타임 설정을 독립적으로 관리
- **스냅샷터 전파**: 런타임별 스냅샷터 설정을 이미지 서비스에 전파 가능
- **책임 분리**: 이미지 풀/관리와 컨테이너 생명주기 관리의 코드 분리

### Q3: 왜 sandboxStore와 containerStore가 인메모리인가?

containerd의 메타데이터 DB(BoltDB)와 별도로 CRI 플러그인이 인메모리 스토어를 유지하는 이유:

1. **성능**: 모든 List/Get 호출이 BoltDB 트랜잭션 없이 처리됨
2. **CRI 전용 메타데이터**: CRI의 Status(Starting, Removing 플래그), CNI 결과, NRI 메타데이터 등 containerd 코어에 없는 정보 저장
3. **상태 추적**: Container/Sandbox의 상태 전이를 원자적으로 추적 (atomic Bool, sync.Mutex)
4. **복구**: 시작 시 `recover()`에서 containerd 상태와 동기화

### Q4: 왜 defer 체인으로 정리하는가?

RunPodSandbox의 복잡한 defer 체인은 Go의 LIFO defer 특성을 활용한 정리 패턴이다:

```
할당 순서: A → B → C → D
정리 순서: D → C → B → A (역순)
```

각 리소스 할당 직후 defer를 등록하면, 어느 단계에서 실패하든 이미 할당된 리소스만 역순으로 정리된다. `cleanupErr`는 정리 자체가 실패한 경우를 추적하여, 추후 kubelet이 재정리할 수 있도록 한다.

### Q5: 왜 EventMonitor가 필요한가?

컨테이너가 자체 종료(OOM, 프로세스 종료 등)되면 CRI 레이어가 이를 인지해야 한다. EventMonitor는 containerd의 TaskExit, TaskOOM 이벤트를 구독하여:

1. 컨테이너 상태를 EXITED로 업데이트
2. Task 리소스를 정리
3. kubelet에 상태 변경을 알림

백오프 메커니즘은 일시적 오류(shim 연결 끊김 등)에 대한 복원력을 제공한다.

---

## 요약

CRI 플러그인은 containerd의 가장 복잡한 플러그인으로, Kubernetes의 Pod/Container 모델을 containerd의 Sandbox/Container/Task 모델로 변환하는 브리지 역할을 한다. 핵심 설계 원칙은:

1. **플러그인 아키텍처**: 다른 containerd 플러그인과 동일한 방식으로 등록/초기화
2. **계층적 추상화**: CRI API → criService → containerd Client → shim
3. **안전한 생명주기**: defer 체인, 상태 플래그(Starting/Removing), 멱등성
4. **비동기 상태 동기화**: EventMonitor + 백오프로 자체 종료 이벤트 처리
5. **NRI 통합**: 각 생명주기 단계에 NRI 훅을 삽입하여 리소스 관리 확장

```
소스 경로 요약:
  plugins/cri/cri.go                        # 플러그인 등록
  internal/cri/server/service.go            # criService 핵심
  internal/cri/server/sandbox_run.go        # RunPodSandbox
  internal/cri/server/container_create.go   # CreateContainer
  internal/cri/server/container_start.go    # StartContainer
  internal/cri/server/container_stop.go     # StopContainer
  internal/cri/server/container_remove.go   # RemoveContainer
  internal/cri/server/events/events.go      # EventMonitor
```
