# containerd 시퀀스 다이어그램

## 1. 개요

이 문서는 containerd의 **주요 유즈케이스 흐름**을 Mermaid 시퀀스 다이어그램과 ASCII 다이어그램으로 설명한다.
각 흐름은 실제 소스코드에서 추적한 **호출 경로**를 기반으로 작성되었다.

### 다이어그램 목록

| 번호 | 흐름 | 핵심 컴포넌트 |
|------|------|-------------|
| 1 | 컨테이너 생성 (전체 흐름) | Client → gRPC → Snapshotter → Shim → runc |
| 2 | 이미지 Pull | Client → Transfer → Resolver → Fetcher → Content → Snapshot |
| 3 | Task 실행 (Start) | Client → gRPC → Shim(TTRPC) → runc |
| 4 | CRI RunPodSandbox | kubelet → CRI Plugin → Sandbox → Shim → CNI |
| 5 | GC (Garbage Collection) | Mutation → Scheduler → Tricolor Mark → Sweep |

---

## 2. 컨테이너 생성 흐름

### 2.1 전체 과정 개요

컨테이너 생성은 크게 4단계로 구성된다:
1. **이미지 Pull** → Content Store에 blob 저장
2. **스냅샷 준비** → Snapshotter로 CoW rootfs 생성
3. **컨테이너 메타데이터 생성** → BoltDB에 저장
4. **Task 생성/시작** → Shim → runc → 컨테이너 프로세스

### 2.2 시퀀스 다이어그램

```mermaid
sequenceDiagram
    participant Client as Go Client / ctr
    participant GRPC as containerd gRPC
    participant IMG as Image Service
    participant CS as Content Store
    participant SS as Snapshotter
    participant Meta as Metadata DB
    participant Shim as containerd-shim-runc-v2
    participant Runc as runc

    Note over Client,Runc: Phase 1 - 이미지 Pull
    Client->>GRPC: Pull(image-ref)
    GRPC->>IMG: Resolve(ref) → Descriptor
    IMG->>CS: Fetch manifest
    CS-->>IMG: manifest JSON
    IMG->>CS: Fetch config
    CS-->>IMG: config JSON
    loop 각 레이어
        IMG->>CS: Writer(ref) → Writer
        IMG->>CS: Writer.Write(data)
        IMG->>CS: Writer.Commit(size, digest)
    end
    IMG->>Meta: images.Create(name, target)
    IMG-->>Client: Image

    Note over Client,Runc: Phase 2 - 스냅샷 준비 (Unpack)
    Client->>SS: Prepare(key, "")
    Note right of SS: 빈 부모로 base 레이어
    loop 각 레이어
        SS-->>Client: []Mount
        Note right of Client: 마운트 후 레이어 적용
        Client->>SS: Commit(chainID, key)
        Client->>SS: Prepare(next-key, chainID)
    end
    Note right of SS: 최종 CoW 레이어 (Active)

    Note over Client,Runc: Phase 3 - 컨테이너 생성
    Client->>GRPC: Containers.Create(container)
    GRPC->>Meta: containers.Create(ID, Image, Runtime, Spec, SnapshotKey)
    Meta-->>Client: Container

    Note over Client,Runc: Phase 4 - Task 생성/시작
    Client->>GRPC: Tasks.Create(containerID, rootfs)
    GRPC->>SS: Mounts(snapshotKey)
    SS-->>GRPC: []Mount
    GRPC->>Shim: shim binary start (fork/exec)
    Shim-->>GRPC: BootstrapParams (ttrpc address)
    GRPC->>Shim: Create(id, bundle, rootfs, spec)
    Shim->>Runc: runc create
    Runc-->>Shim: container created
    Shim-->>GRPC: CreateResponse

    Client->>GRPC: Tasks.Start(containerID)
    GRPC->>Shim: Start(id)
    Shim->>Runc: runc start
    Runc-->>Shim: container running
    Shim-->>GRPC: StartResponse(pid)
    GRPC-->>Client: Task running (pid)
```

### 2.3 ASCII 흐름도

```
Client                containerd (gRPC)        Snapshotter          Shim              runc
  │                        │                      │                  │                  │
  │──Pull(ref)────────────>│                      │                  │                  │
  │                        │──Resolve(ref)───────>│                  │                  │
  │                        │<─Descriptor──────────│                  │                  │
  │                        │──Fetch layers──────> Content Store      │                  │
  │                        │<─layers stored───────│                  │                  │
  │<─Image─────────────────│                      │                  │                  │
  │                        │                      │                  │                  │
  │──Unpack────────────────│─Prepare(key,"")─────>│                  │                  │
  │                        │<─[]Mount─────────────│                  │                  │
  │                        │──apply layer─────────│                  │                  │
  │                        │──Commit(chainID,key)>│                  │                  │
  │                        │  (반복: 각 레이어)    │                  │                  │
  │                        │                      │                  │                  │
  │──Container.Create──────│                      │                  │                  │
  │                        │──BoltDB write───────>│                  │                  │
  │<─Container─────────────│                      │                  │                  │
  │                        │                      │                  │                  │
  │──Task.Create───────────│                      │                  │                  │
  │                        │──Mounts(key)────────>│                  │                  │
  │                        │<─[]Mount─────────────│                  │                  │
  │                        │──fork/exec──────────────────────────────│                  │
  │                        │<─BootstrapParams────────────────────────│                  │
  │                        │──TTRPC:Create(spec)─────────────────────│                  │
  │                        │                                         │──runc create────>│
  │                        │                                         │<─created─────────│
  │                        │<─CreateResponse─────────────────────────│                  │
  │                        │                                         │                  │
  │──Task.Start────────────│                                         │                  │
  │                        │──TTRPC:Start────────────────────────────│                  │
  │                        │                                         │──runc start─────>│
  │                        │                                         │<─running─────────│
  │                        │<─StartResponse(pid)─────────────────────│                  │
  │<─Task running──────────│                                         │                  │
```

---

## 3. 이미지 Pull 흐름

### 3.1 Transfer 기반 Pull (v2)

containerd v2에서는 Transfer 서비스를 통해 이미지를 Pull한다.

```mermaid
sequenceDiagram
    participant Client as Client
    participant Transfer as Transfer Service
    participant Resolver as Registry Resolver
    participant Fetcher as HTTP Fetcher
    participant CW as Content Writer
    participant CS as Content Store
    participant SS as Snapshotter
    participant Lease as Lease Manager

    Client->>Lease: Create(WithExpiration)
    Lease-->>Client: lease

    Client->>Transfer: Transfer(source, destination)

    Note over Transfer,Fetcher: Phase 1 - Resolve & Fetch Manifest
    Transfer->>Resolver: Resolve(ref)
    Resolver-->>Transfer: Descriptor (manifest digest)
    Transfer->>Fetcher: Fetch(descriptor)
    Fetcher-->>Transfer: io.ReadCloser (HTTP GET)

    Note over Transfer,CS: Phase 2 - Store Manifest
    Transfer->>CW: CS.Writer(WithRef("manifest"))
    CW-->>Transfer: Writer
    Transfer->>CW: Write(manifest bytes)
    Transfer->>CW: Commit(size, digest)
    Transfer->>Lease: AddResource(lease, content:digest)

    Note over Transfer,CS: Phase 3 - Fetch Config
    Transfer->>Fetcher: Fetch(config descriptor)
    Fetcher-->>Transfer: config JSON
    Transfer->>CW: Write + Commit(config)
    Transfer->>Lease: AddResource(lease, content:config-digest)

    Note over Transfer,SS: Phase 4 - Fetch & Unpack Layers
    loop 각 레이어 (병렬 가능)
        Transfer->>Fetcher: Fetch(layer descriptor)
        Fetcher-->>Transfer: compressed layer stream
        Transfer->>CW: Writer(WithRef("layer-N"))
        Transfer->>CW: Write(compressed data)
        Transfer->>CW: Commit(size, digest)
        Transfer->>Lease: AddResource(lease, content:layer-digest)
    end

    Note over Transfer,SS: Phase 5 - Unpack to Snapshots
    loop 각 레이어 순차 처리
        Transfer->>SS: Prepare(extract-key, parent)
        SS-->>Transfer: []Mount
        Transfer->>Transfer: Apply diff (mount + untar)
        Transfer->>SS: Commit(chainID, extract-key)
        Transfer->>Lease: AddResource(lease, snapshots/overlayfs:chainID)
    end

    Note over Transfer,CS: Phase 6 - Create Image Record
    Transfer->>CS: images.Create(name, target)
    Transfer-->>Client: Image created

    Client->>Lease: Delete(lease)
    Note right of Client: Lease 해제, GC가 미사용 리소스 정리 가능
```

### 3.2 레이어 Unpack 상세

```
이미지: nginx:latest (3개 레이어)

Content Store (digest 기반)           Snapshotter (overlay)
+---------------------------+        +---------------------------+
| sha256:aaa (manifest)     |        |                           |
| sha256:bbb (config)       |        | "" (빈 부모)              |
| sha256:ccc (layer 1)      |───────>│  │                        |
| sha256:ddd (layer 2)      |───────>│  ├─ layer-1 [Committed]   |
| sha256:eee (layer 3)      |───────>│  │   ├─ layer-2 [Committed]|
+---------------------------+        │  │   │   └─ layer-3 [Committed]
                                     │  │   │       │             |
                                     │  │   │       └─ container  |
                                     │  │   │          [Active]   |
                                     +--+---+---------------------+

ChainID 계산:
  chainID_1 = sha256(diffID_1)
  chainID_2 = sha256(chainID_1 + " " + diffID_2)
  chainID_3 = sha256(chainID_2 + " " + diffID_3)
```

---

## 4. Task 실행 흐름

### 4.1 Task Create → Start → Wait → Kill → Delete

```mermaid
sequenceDiagram
    participant Client as Client
    participant Tasks as Tasks Service
    participant SM as Shim Manager
    participant Shim as containerd-shim-runc-v2
    participant Runc as runc
    participant Events as Event Exchange

    Note over Client,Events: Task.Create
    Client->>Tasks: Create(containerID, rootfs, checkpoint)
    Tasks->>SM: Create(taskID, CreateOpts)
    SM->>SM: Find or start shim binary
    SM->>Shim: fork/exec containerd-shim-runc-v2
    Shim->>Shim: TTRPC 서버 시작
    Shim-->>SM: BootstrapParams{address, version}
    SM->>Shim: TTRPC: TaskService.Create(request)
    Shim->>Runc: runc create --bundle <path>
    Runc-->>Shim: container created (PID)
    Shim-->>SM: CreateTaskResponse{pid}
    SM->>Events: Publish("/tasks/create", {containerID, pid})
    Tasks-->>Client: CreateTaskResponse

    Note over Client,Events: Task.Start
    Client->>Tasks: Start(containerID)
    Tasks->>Shim: TTRPC: TaskService.Start(request)
    Shim->>Runc: runc start <id>
    Runc-->>Shim: container running
    Shim-->>Tasks: StartResponse{pid}
    Tasks->>Events: Publish("/tasks/start", {containerID, pid})
    Tasks-->>Client: StartResponse

    Note over Client,Events: Task.Wait (비동기)
    Client->>Tasks: Wait(containerID)
    Tasks->>Shim: TTRPC: TaskService.Wait(request)
    Note right of Shim: 프로세스 종료 대기 중...

    Note over Client,Events: Task.Kill
    Client->>Tasks: Kill(containerID, signal)
    Tasks->>Shim: TTRPC: TaskService.Kill(signal)
    Shim->>Runc: runc kill <id> <signal>
    Runc-->>Shim: signal sent

    Note over Shim,Events: 프로세스 종료
    Runc-->>Shim: process exited (status)
    Shim->>Events: Publish("/tasks/exit", {containerID, pid, exitStatus})
    Shim-->>Tasks: WaitResponse{exitStatus, exitedAt}
    Tasks-->>Client: WaitResponse

    Note over Client,Events: Task.Delete
    Client->>Tasks: Delete(containerID)
    Tasks->>Shim: TTRPC: TaskService.Delete(request)
    Shim->>Runc: runc delete <id>
    Runc-->>Shim: container deleted
    Shim->>Shim: 리소스 정리
    Shim-->>Tasks: DeleteResponse{pid, exitStatus}
    Tasks->>Events: Publish("/tasks/delete", {containerID})
    Tasks-->>Client: DeleteResponse
```

### 4.2 Exec (추가 프로세스 실행)

```mermaid
sequenceDiagram
    participant Client as Client
    participant Tasks as Tasks Service
    participant Shim as Shim
    participant Runc as runc
    participant Events as Event Exchange

    Client->>Tasks: Exec(containerID, execID, spec)
    Tasks->>Shim: TTRPC: TaskService.Exec(request)
    Shim->>Runc: runc exec --process <spec>
    Runc-->>Shim: exec process created
    Shim-->>Tasks: empty response
    Tasks->>Events: Publish("/tasks/exec-added")
    Tasks-->>Client: empty response

    Client->>Tasks: Start(containerID, execID)
    Tasks->>Shim: TTRPC: TaskService.Start(execID)
    Shim->>Runc: exec process started
    Shim-->>Tasks: StartResponse{pid}
    Tasks->>Events: Publish("/tasks/exec-started")
    Tasks-->>Client: StartResponse{pid}

    Note right of Shim: exec 프로세스 종료 시
    Shim->>Events: Publish("/tasks/exit", {containerID, execID, status})
```

---

## 5. CRI RunPodSandbox 흐름

### 5.1 Kubernetes CRI 연동

kubelet은 containerd의 CRI 플러그인을 통해 Pod을 관리한다.
RunPodSandbox는 Pod의 격리 환경(네트워크 네임스페이스, pause 컨테이너)을 생성한다.

```mermaid
sequenceDiagram
    participant Kubelet as kubelet
    participant CRI as CRI Plugin
    participant SB as Sandbox Controller
    participant SS as Snapshotter
    participant Shim as Shim (sandbox)
    participant CNI as CNI Plugin
    participant Events as Event Exchange

    Note over Kubelet,Events: RunPodSandbox
    Kubelet->>CRI: RunPodSandbox(PodSandboxConfig)
    CRI->>CRI: sandbox ID 생성
    CRI->>CRI: OCI Spec 생성 (pause 컨테이너)

    Note over CRI,SS: 이미지 준비 (pause 이미지)
    CRI->>CRI: EnsureImageExists("registry.k8s.io/pause:3.x")
    CRI->>SS: Prepare(snapshot-key, image-chainID)
    SS-->>CRI: []Mount (rootfs)

    Note over CRI,Shim: Sandbox 생성/시작
    CRI->>SB: Create(sandboxInfo, WithRootFS(mounts))
    SB->>Shim: fork/exec containerd-shim-runc-v2
    Shim-->>SB: BootstrapParams
    SB->>Shim: TTRPC: SandboxService.CreateSandbox()
    Shim-->>SB: CreateSandboxResponse

    CRI->>SB: Start(sandboxID)
    SB->>Shim: TTRPC: SandboxService.StartSandbox()
    Shim-->>SB: ControllerInstance{pid, address}

    Note over CRI,CNI: 네트워크 설정
    CRI->>CNI: Setup(sandboxID, netns)
    CNI-->>CRI: Result(IPs, Routes)

    CRI->>Events: Publish("/sandbox/create")
    CRI-->>Kubelet: RunPodSandboxResponse{sandboxID}

    Note over Kubelet,Events: CreateContainer (Pod 내 컨테이너)
    Kubelet->>CRI: CreateContainer(sandboxID, containerConfig)
    CRI->>CRI: OCI Spec 생성 (앱 컨테이너)
    CRI->>SS: Prepare(container-snapshot, image-chainID)
    SS-->>CRI: []Mount
    CRI->>CRI: containers.Create(ID, SandboxID=sandboxID)
    CRI-->>Kubelet: containerID

    Kubelet->>CRI: StartContainer(containerID)
    CRI->>Shim: TTRPC: TaskService.Create(containerID)
    CRI->>Shim: TTRPC: TaskService.Start(containerID)
    Shim-->>CRI: StartResponse{pid}
    CRI-->>Kubelet: StartContainerResponse
```

### 5.2 CRI 호출 흐름 ASCII

```
kubelet                CRI Plugin           Sandbox Controller      CNI
  │                       │                       │                  │
  │──RunPodSandbox───────>│                       │                  │
  │                       │──EnsureImage──────────│                  │
  │                       │──Prepare(rootfs)──────│                  │
  │                       │                       │                  │
  │                       │──SB.Create────────────│                  │
  │                       │──SB.Start─────────────│                  │
  │                       │                       │──shim start────>│
  │                       │                       │<─address─────────│
  │                       │                       │                  │
  │                       │──CNI.Setup────────────│──────────────────│
  │                       │<─IPs, Routes──────────│──────────────────│
  │<─sandboxID────────────│                       │                  │
  │                       │                       │                  │
  │──CreateContainer─────>│                       │                  │
  │                       │──Prepare(snapshot)────│                  │
  │                       │──containers.Create────│                  │
  │<─containerID──────────│                       │                  │
  │                       │                       │                  │
  │──StartContainer──────>│                       │                  │
  │                       │──Task.Create──────────│──TTRPC──────────>│
  │                       │──Task.Start───────────│──TTRPC──────────>│
  │<─StartResponse────────│                       │                  │
```

---

## 6. GC (Garbage Collection) 흐름

### 6.1 GC 트리거와 실행

containerd의 GC는 **Tricolor Mark-and-Sweep 알고리즘**을 사용한다.
Mutation(삭제 등)이 발생하면 GC Scheduler가 적절한 시점에 GC를 트리거한다.

```
소스 참조: plugins/gc/scheduler.go (Line 35~90) - GC config
소스 참조: core/metadata/db.go (Line 78~115) - DB의 dirty 플래그와 wlock
```

```mermaid
sequenceDiagram
    participant Client as Client
    participant Meta as Metadata DB
    participant Sched as GC Scheduler
    participant GC as GC Engine
    participant CS as Content Store
    participant SS as Snapshotter

    Note over Client,SS: Phase 1 - Mutation 감지
    Client->>Meta: Delete(resource)
    Meta->>Meta: dirty.Store(1)
    Meta->>Sched: mutationCallback(dirty=true)

    Note over Sched,SS: Phase 2 - 스케줄링
    Sched->>Sched: deletionThreshold 확인
    Sched->>Sched: scheduleDelay 대기
    Sched->>GC: trigger GC

    Note over GC,SS: Phase 3 - Mark (wlock.Lock)
    GC->>Meta: wlock.Lock() (쓰기 트랜잭션 차단)
    GC->>Meta: Read-only 트랜잭션 시작

    Note over GC,SS: Tricolor Mark
    GC->>GC: 1. 모든 리소스를 White(미방문)로 초기화
    GC->>GC: 2. GC Root 탐색 시작
    Note right of GC: GC Root = 활성 Lease + Image + Container

    loop 각 GC Root
        GC->>GC: Root를 Gray(방문 중)로 마킹
        loop Root가 참조하는 리소스
            GC->>GC: 참조 리소스를 Gray로 마킹
            GC->>GC: 모든 참조 탐색 후 Black(방문 완료)으로 변경
        end
    end

    Note right of GC: Mark 완료: Black=사용중, White=미사용

    Note over GC,SS: Phase 4 - Sweep
    loop 각 White 리소스
        GC->>CS: Content.Delete(digest) (White 콘텐츠)
        GC->>SS: Snapshot.Remove(key) (White 스냅샷)
    end

    GC->>Meta: dirty.Store(0)
    GC->>Meta: wlock.Unlock()
    GC-->>Sched: GC 완료 (통계)
```

### 6.2 Tricolor Mark 상세

```
Tricolor Mark-and-Sweep 알고리즘:

초기 상태 (모든 리소스 White):
  ┌─────────────────────────────────────────┐
  │ White (미방문)                           │
  │                                         │
  │  [image:nginx]  [content:sha256:aaa]    │
  │  [content:sha256:bbb]                   │
  │  [snap:layer-1]  [snap:layer-2]         │
  │  [content:sha256:orphan]  ← 미참조      │
  │  [snap:old-container]     ← 미참조      │
  └─────────────────────────────────────────┘

Mark 시작 (GC Root = Image:nginx):
  ┌──────────────────┐  ┌──────────────────┐  ┌──────────────────┐
  │ White (삭제 대상) │  │ Gray (탐색 중)    │  │ Black (보존)     │
  │                  │  │                  │  │                  │
  │ [content:orphan] │  │ [image:nginx]    │  │                  │
  │ [snap:old-ctr]   │  │                  │  │                  │
  └──────────────────┘  └──────────────────┘  └──────────────────┘

Mark 진행 (nginx의 참조 추적):
  ┌──────────────────┐  ┌──────────────────┐  ┌──────────────────┐
  │ White (삭제 대상) │  │ Gray (탐색 중)    │  │ Black (보존)     │
  │                  │  │                  │  │                  │
  │ [content:orphan] │  │ [content:aaa]    │  │ [image:nginx]    │
  │ [snap:old-ctr]   │  │ [content:bbb]    │  │                  │
  │                  │  │ [snap:layer-1]   │  │                  │
  │                  │  │ [snap:layer-2]   │  │                  │
  └──────────────────┘  └──────────────────┘  └──────────────────┘

Mark 완료:
  ┌──────────────────┐  ┌──────────────────┐  ┌──────────────────┐
  │ White (삭제 대상) │  │ Gray (없음)      │  │ Black (보존)     │
  │                  │  │                  │  │                  │
  │ [content:orphan] │  │                  │  │ [image:nginx]    │
  │ [snap:old-ctr]   │  │                  │  │ [content:aaa]    │
  │                  │  │                  │  │ [content:bbb]    │
  │                  │  │                  │  │ [snap:layer-1]   │
  │                  │  │                  │  │ [snap:layer-2]   │
  └──────────────────┘  └──────────────────┘  └──────────────────┘

Sweep: White 리소스 삭제
  → content:orphan 삭제
  → snap:old-ctr 삭제
```

### 6.3 GC Root 정의

| Root 타입 | 보호 대상 | 설명 |
|-----------|----------|------|
| **Image** | manifest → config → layers | 이미지가 존재하면 관련 콘텐츠/스냅샷 보호 |
| **Container** | snapshotKey → 스냅샷 체인 | 컨테이너가 존재하면 rootfs 스냅샷 보호 |
| **Lease** | lease.resources[] | 명시적으로 보호된 리소스 |
| **Active Ingestion** | ingest ref | 진행 중인 쓰기 작업의 콘텐츠 보호 |

### 6.4 GC Scheduler 설정

```
소스 참조: plugins/gc/scheduler.go (Line 35~90)
```

| 설정 | 기본값 | 설명 |
|------|--------|------|
| `pause_threshold` | 0.02 (2%) | GC 일시정지가 전체 시간의 최대 N% |
| `deletion_threshold` | 0 | N번 삭제 후 GC 스케줄링 |
| `mutation_threshold` | 100 | N번 변경 후 다음 GC 시 실행 |
| `schedule_delay` | "0ms" | 트리거 후 GC 실행까지 지연 |
| `startup_delay` | "100ms" | 서버 시작 후 첫 GC까지 지연 |

---

## 7. Shim 시작 흐름 상세

### 7.1 Shim Binary 실행

```mermaid
sequenceDiagram
    participant SM as Shim Manager
    participant OS as OS (fork/exec)
    participant Shim as containerd-shim-runc-v2
    participant TTRPC as TTRPC Server

    SM->>OS: exec("containerd-shim-runc-v2", args)
    Note right of OS: args: -namespace <ns> -id <id> -address <sock>

    OS->>Shim: 프로세스 생성
    Shim->>Shim: 자신을 subreaper로 설정
    Shim->>Shim: 새 세션 리더로 setsid()
    Shim->>TTRPC: TTRPC 서버 시작 (Unix socket)

    Shim->>SM: stdout에 BootstrapParams 출력
    Note right of SM: {"version": 2, "address": "/run/.../shim.sock"}

    SM->>SM: TTRPC 클라이언트 연결
    SM->>Shim: TTRPC 연결 확인

    Note over SM,Shim: 이제 SM ↔ Shim 간 TTRPC 통신 가능
```

### 7.2 Shim 프로세스 트리

```
containerd (PID 1000)
  │
  ├─ containerd-shim-runc-v2 (PID 2000) ← sandbox/container A
  │   └─ runc init → container PID 1 (PID 2001)
  │       ├─ app process (PID 2002)
  │       └─ sidecar (PID 2003)
  │
  └─ containerd-shim-runc-v2 (PID 3000) ← sandbox/container B
      └─ runc init → container PID 1 (PID 3001)

특징:
- 각 Shim은 독립 프로세스 (containerd 재시작 시 영향 없음)
- Shim이 subreaper로 설정되어 컨테이너 프로세스의 부모 역할
- containerd → Shim 통신은 TTRPC (경량)
```

---

## 8. 컨테이너 종료 흐름

### 8.1 Graceful Shutdown

```mermaid
sequenceDiagram
    participant Client as Client
    participant Tasks as Tasks Service
    participant Shim as Shim
    participant Runc as runc
    participant Container as Container Process
    participant Events as Event Exchange

    Client->>Tasks: Kill(containerID, SIGTERM)
    Tasks->>Shim: TTRPC: Kill(SIGTERM)
    Shim->>Runc: runc kill <id> SIGTERM
    Runc->>Container: signal(SIGTERM)

    Note right of Container: 애플리케이션 graceful shutdown...
    Container->>Container: cleanup & exit(0)

    Shim->>Shim: waitpid() → exit status 수집
    Shim->>Events: Publish("/tasks/exit", {pid, status=0})

    Client->>Tasks: Wait(containerID) 반환
    Tasks-->>Client: ExitStatus{status=0, exitedAt}

    Client->>Tasks: Delete(containerID)
    Tasks->>Shim: TTRPC: Delete(request)
    Shim->>Runc: runc delete <id>
    Shim->>Shim: 리소스 정리, Shim 프로세스 종료
    Shim-->>Tasks: DeleteResponse
    Tasks-->>Client: DeleteResponse
```

### 8.2 Force Kill (Timeout 후)

```
시간 흐름:

t=0   SIGTERM 전송
      │
      │  (graceful shutdown 대기)
      │
t=10  타임아웃 → SIGKILL 전송
      │
t=10+ 프로세스 강제 종료
      │
      └─ exit status 수집 → /tasks/exit 이벤트
```

---

## 9. 이미지 Push 흐름

```mermaid
sequenceDiagram
    participant Client as Client
    participant Transfer as Transfer Service
    participant CS as Content Store
    participant Resolver as Registry Resolver
    participant Pusher as HTTP Pusher

    Client->>Transfer: Transfer(source=local, dest=registry)

    Note over Transfer,Pusher: Phase 1 - Resolve 대상 레지스트리
    Transfer->>Resolver: Resolve(ref)
    Resolver-->>Transfer: Pusher

    Note over Transfer,Pusher: Phase 2 - 레이어 Push
    loop 각 레이어
        Transfer->>CS: ReaderAt(layer-descriptor)
        CS-->>Transfer: ReaderAt
        Transfer->>Pusher: Push(descriptor, reader)
        Note right of Pusher: HTTP PUT /v2/<name>/blobs/uploads/
        Pusher-->>Transfer: 완료
    end

    Note over Transfer,Pusher: Phase 3 - Config Push
    Transfer->>CS: ReaderAt(config-descriptor)
    Transfer->>Pusher: Push(config-descriptor, reader)

    Note over Transfer,Pusher: Phase 4 - Manifest Push
    Transfer->>CS: ReaderAt(manifest-descriptor)
    Transfer->>Pusher: Push(manifest-descriptor, reader)
    Note right of Pusher: HTTP PUT /v2/<name>/manifests/<tag>

    Transfer-->>Client: Push 완료
```

---

## 10. 전체 흐름 요약

```
┌─────────────────────────────────────────────────────────────────┐
│                    containerd 주요 흐름 요약                     │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  [이미지 Pull]                                                  │
│  Registry → Resolver → Fetcher → Content.Writer → Commit        │
│  → Snapshotter.Prepare → Apply Layer → Commit (각 레이어)       │
│  → images.Create                                                │
│                                                                 │
│  [컨테이너 생성]                                                │
│  Image → Snapshotter.Prepare(rootfs) → containers.Create(meta) │
│                                                                 │
│  [Task 실행]                                                    │
│  Task.Create → Shim(fork/exec) → TTRPC:Create → runc create    │
│  Task.Start → TTRPC:Start → runc start → 컨테이너 실행         │
│                                                                 │
│  [Task 종료]                                                    │
│  Task.Kill → TTRPC:Kill → runc kill → process exit              │
│  → /tasks/exit 이벤트 → Task.Delete → runc delete              │
│                                                                 │
│  [CRI Pod]                                                      │
│  RunPodSandbox → Sandbox.Create → Shim → CNI Setup             │
│  CreateContainer → Prepare(rootfs) → containers.Create          │
│  StartContainer → Task.Create → Task.Start                      │
│                                                                 │
│  [GC]                                                           │
│  Mutation → Scheduler → wlock.Lock → Tricolor Mark → Sweep     │
│  → Content.Delete + Snapshot.Remove → wlock.Unlock              │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```
