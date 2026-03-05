# containerd 데이터 모델

## 1. 개요

containerd의 데이터 모델은 **OCI(Open Container Initiative) 표준**을 기반으로 설계되었다.
컨테이너 라이프사이클에 필요한 모든 엔티티가 명확한 구조체와 인터페이스로 정의되어 있으며,
BoltDB 메타데이터 저장소에 **네임스페이스별로 격리**되어 저장된다.

### 핵심 엔티티 관계

```
+------------------------------------------------------------------+
|                    containerd 엔티티 관계도                        |
+------------------------------------------------------------------+
|                                                                    |
|   Image ─────references────> Descriptor (Content)                  |
|     │                           │                                  |
|     │ uses                      │ digest                           |
|     v                           v                                  |
|   Container ──snapshot──> Snapshotter ──layers──> Content Store    |
|     │                        │                                     |
|     │ creates                │ CoW                                 |
|     v                        v                                     |
|   Task ───────shim──────> Process (runc)                           |
|     │                                                              |
|     │ belongs to                                                   |
|     v                                                              |
|   Sandbox ──controller──> Shim (TTRPC)                             |
|                                                                    |
|   Lease ──protects──> Content, Snapshot, Container                 |
|                                                                    |
+------------------------------------------------------------------+
```

---

## 2. Container

컨테이너는 containerd의 핵심 엔티티로, **실행에 필요한 모든 메타데이터**를 보유한다.
컨테이너 자체는 실행 상태가 아니며, Task를 생성해야 실제 프로세스가 실행된다.

```
소스 참조: core/containers/containers.go (Line 30~84)
```

### 2.1 Container 구조체

```go
type Container struct {
    // ID는 네임스페이스 내에서 컨테이너를 고유하게 식별한다.
    // 생성 후 변경 불가 (required, immutable)
    ID string

    // Labels는 메타데이터 확장을 위한 키-값 쌍이다.
    // 선택적이며 완전히 변경 가능 (optional, mutable)
    Labels map[string]string

    // Image는 컨테이너에 사용된 이미지 참조이다.
    // 선택적이며 변경 가능 (optional, mutable)
    Image string

    // Runtime은 컨테이너 태스크 실행 시 사용할 런타임을 지정한다.
    // 필수이며 변경 불가 (required, immutable)
    Runtime RuntimeInfo

    // Spec은 컨테이너 구현을 위한 런타임 사양(OCI Spec)이다.
    // 필수이며 변경 가능 (required, mutable)
    Spec typeurl.Any

    // SnapshotKey는 컨테이너 rootfs에 사용할 스냅샷 키이다.
    // 태스크 생성 시 이 키로 스냅샷 서비스에서 마운트를 조회한다.
    // 선택적이며 변경 가능 (optional, mutable)
    SnapshotKey string

    // Snapshotter는 rootfs에 사용되는 스냅샷터 이름이다.
    // 선택적이며 변경 불가 (optional, immutable)
    Snapshotter string

    // CreatedAt은 컨테이너 생성 시각이다.
    CreatedAt time.Time

    // UpdatedAt은 컨테이너 최종 수정 시각이다.
    UpdatedAt time.Time

    // Extensions는 클라이언트 지정 메타데이터를 저장한다.
    Extensions map[string]typeurl.Any

    // SandboxID는 이 컨테이너가 속한 샌드박스의 식별자이다.
    // 선택적이며 생성 후 변경 불가 (optional, immutable)
    SandboxID string
}
```

### 2.2 RuntimeInfo

```go
// RuntimeInfo는 런타임 관련 정보를 보유한다
type RuntimeInfo struct {
    Name    string        // 런타임 이름 (예: "io.containerd.runc.v2")
    Options typeurl.Any   // 런타임별 옵션 (protobuf Any)
}
```

### 2.3 Container Store 인터페이스

```go
type Store interface {
    Get(ctx context.Context, id string) (Container, error)
    List(ctx context.Context, filters ...string) ([]Container, error)
    Create(ctx context.Context, container Container) (Container, error)
    Update(ctx context.Context, container Container, fieldpaths ...string) (Container, error)
    Delete(ctx context.Context, id string) error
}
```

### 2.4 Container 필드 특성 요약

| 필드 | 필수 | 변경 가능 | 설명 |
|------|------|----------|------|
| `ID` | O | X | 네임스페이스 내 고유 식별자 |
| `Labels` | X | O | 메타데이터 키-값 쌍 |
| `Image` | X | O | 이미지 참조 (docker.io/library/nginx:latest 등) |
| `Runtime` | O | X | 런타임 이름 + 옵션 |
| `Spec` | O | O | OCI Runtime Spec (protobuf Any) |
| `SnapshotKey` | X | O | rootfs 스냅샷 키 |
| `Snapshotter` | X | X | 스냅샷터 이름 (overlay, native 등) |
| `SandboxID` | X | X | 소속 샌드박스 ID |
| `Extensions` | X | O | 클라이언트 확장 데이터 |

---

## 3. Image

이미지는 **OCI Image Spec 기반의 컨테이너 이미지 메타데이터**를 나타낸다.
실제 콘텐츠(매니페스트, 설정, 레이어)는 Content Store에 저장되며,
Image 레코드는 해당 콘텐츠의 **루트 Descriptor**를 참조한다.

```
소스 참조: core/images/image.go (Line 35~56)
```

### 3.1 Image 구조체

```go
type Image struct {
    // Name은 이미지 참조 이름이다.
    // Pull하려면 resolver와 호환되는 참조여야 한다.
    // 필수 (required)
    Name string

    // Labels는 이미지 레코드에 대한 런타임 데코레이션이다.
    // 선택적 (optional)
    Labels map[string]string

    // Target은 이미지의 루트 콘텐츠를 설명하는 OCI Descriptor이다.
    // 일반적으로 manifest, index 또는 manifest list를 가리킨다.
    Target ocispec.Descriptor

    // 생성/수정 시각
    CreatedAt, UpdatedAt time.Time
}
```

### 3.2 OCI Descriptor

Image의 `Target` 필드는 OCI Image Spec의 `Descriptor` 타입이다.
Descriptor는 **콘텐츠 주소 지정(Content Addressable)**의 핵심이다.

```go
// opencontainers/image-spec/specs-go/v1
type Descriptor struct {
    MediaType   string            // MIME 타입 (application/vnd.oci.image.manifest.v1+json 등)
    Digest      digest.Digest     // SHA-256 해시 (sha256:abc123...)
    Size        int64             // 바이트 크기
    URLs        []string          // 대체 다운로드 URL
    Annotations map[string]string // 어노테이션
    Platform    *Platform         // 대상 플랫폼 (os, arch)
}
```

### 3.3 이미지 콘텐츠 구조

```
Image.Target (Descriptor)
  │
  ├─ MediaType: manifest 또는 index
  │
  ├─[Index인 경우]──> OCI Index
  │   └─ Manifests[]
  │       ├─ Descriptor (linux/amd64)
  │       ├─ Descriptor (linux/arm64)
  │       └─ Descriptor (windows/amd64)
  │
  └─[Manifest인 경우]──> OCI Manifest
      ├─ Config: Descriptor → 이미지 설정 (JSON)
      │   └─ { "os": "linux", "arch": "amd64", "rootfs": {...}, "config": {...} }
      │
      └─ Layers[]: Descriptor[]
          ├─ Descriptor → 레이어 1 (base)
          ├─ Descriptor → 레이어 2
          └─ Descriptor → 레이어 3 (top)
```

### 3.4 Image Store 인터페이스

```go
type Store interface {
    Get(ctx context.Context, name string) (Image, error)
    List(ctx context.Context, filters ...string) ([]Image, error)
    Create(ctx context.Context, image Image) (Image, error)
    Update(ctx context.Context, image Image, fieldpaths ...string) (Image, error)
    Delete(ctx context.Context, name string, opts ...DeleteOpt) error
}
```

### 3.5 Image 유틸리티 메서드

| 메서드 | 설명 |
|--------|------|
| `image.Config(ctx, provider, platform)` | 이미지 설정 Descriptor 해석 |
| `image.RootFS(ctx, provider, platform)` | DiffID 목록 (레이어 해시) 반환 |
| `image.Size(ctx, provider, platform)` | 전체 packed 리소스 크기 계산 |
| `Manifest(ctx, provider, image, platform)` | 플랫폼별 매니페스트 해석 |
| `Platforms(ctx, provider, image)` | 지원 플랫폼 목록 |
| `Check(ctx, provider, image, platform)` | 이미지 컴포넌트 가용성 확인 |
| `Children(ctx, provider, desc)` | 디스크립터의 하위 콘텐츠 |

---

## 4. Content

Content는 containerd의 **Content Addressable Storage**를 구현한다.
모든 blob(매니페스트, 설정, 레이어 등)이 **digest(SHA-256 해시)로 식별**되어 저장된다.

```
소스 참조: core/content/content.go (Line 30~218)
```

### 4.1 Content Store 인터페이스 계층

```
                    Store
                   (통합)
           ┌────────┼────────────┐
           │        │            │
        Manager  Provider  IngestManager
           │        │            │
           │        │         Ingester
           │        │            │
     InfoProvider   │         Writer
                    │
                 ReaderAt
```

```go
// Store는 콘텐츠 지향 인터페이스를 통합한 완전한 구현이다
type Store interface {
    Manager        // 콘텐츠 관리 (Info, Update, Walk, Delete)
    Provider       // 콘텐츠 읽기 (ReaderAt)
    IngestManager  // 진행 중 쓰기 관리 (Status, ListStatuses, Abort)
    Ingester       // 콘텐츠 쓰기 (Writer)
}
```

### 4.2 핵심 인터페이스

**Provider** - 콘텐츠 읽기

```go
type Provider interface {
    // ReaderAt은 desc.Digest만 설정하면 된다.
    // 다른 필드는 내부적으로 데이터 위치 해석에 사용될 수 있다.
    ReaderAt(ctx context.Context, desc ocispec.Descriptor) (ReaderAt, error)
}
```

**Ingester** - 콘텐츠 쓰기

```go
type Ingester interface {
    // Writer는 쓰기 작업(ingestion)을 시작한다.
    // ref로 고유하게 식별되며, 동일 ref로 재개 가능하다.
    // 모든 데이터가 쓰여지면 Writer.Commit()으로 완료한다.
    Writer(ctx context.Context, opts ...WriterOpt) (Writer, error)
}
```

**Writer** - 콘텐츠 쓰기 핸들러

```go
type Writer interface {
    io.WriteCloser
    Digest() digest.Digest
    Commit(ctx context.Context, size int64, expected digest.Digest, opts ...Opt) error
    Status() (Status, error)
    Truncate(size int64) error
}
```

**Manager** - 콘텐츠 관리

```go
type Manager interface {
    InfoProvider
    Update(ctx context.Context, info Info, fieldpaths ...string) (Info, error)
    Walk(ctx context.Context, fn WalkFunc, filters ...string) error
    Delete(ctx context.Context, dgst digest.Digest) error
}
```

### 4.3 Content Info

```go
type Info struct {
    Digest    digest.Digest        // SHA-256 해시 (콘텐츠 주소)
    Size      int64                // 바이트 크기
    CreatedAt time.Time            // 생성 시각
    UpdatedAt time.Time            // 수정 시각
    Labels    map[string]string    // 레이블 (GC 참조 등)
}
```

### 4.4 Ingestion Status

```go
type Status struct {
    Ref       string          // 쓰기 작업 참조 ID
    Offset    int64           // 현재까지 쓰여진 바이트
    Total     int64           // 전체 크기 (알 수 있는 경우)
    Expected  digest.Digest   // 기대하는 최종 digest
    StartedAt time.Time       // 시작 시각
    UpdatedAt time.Time       // 마지막 쓰기 시각
}
```

### 4.5 콘텐츠 라이프사이클

```
콘텐츠 전체 수명:

1. Ingestion 시작 (Ingester.Writer)
   ┌─────────────────────┐
   │  Ingest 상태         │
   │  ref 로 식별         │
   │  IngestManager로 관리│
   └──────────┬──────────┘
              │ Writer.Commit()
              v
2. Committed 콘텐츠 (Provider/Manager)
   ┌─────────────────────┐
   │  Content 상태        │
   │  digest 로 식별      │
   │  Manager로 관리      │
   └──────────┬──────────┘
              │ Manager.Delete() 또는 GC
              v
3. 삭제됨
```

---

## 5. Snapshot

Snapshot은 containerd의 **Copy-on-Write 파일시스템 레이어**를 관리한다.
이미지의 각 레이어는 Committed 스냅샷이 되고, 컨테이너 실행 시 Active 스냅샷이 생성된다.

```
소스 참조: core/snapshots/snapshotter.go (Line 43~406)
```

### 5.1 Snapshot Kind

```go
type Kind uint8

const (
    KindUnknown   Kind = iota
    KindView                    // 읽기 전용 스냅샷
    KindActive                  // 읽기-쓰기 가능한 활성 스냅샷
    KindCommitted               // 커밋된 불변 스냅샷
)
```

| Kind | 설명 | 생성 방법 | 부모 가능 | 쓰기 가능 |
|------|------|----------|----------|----------|
| `View` | 읽기 전용 뷰 | `Snapshotter.View()` | X | X |
| `Active` | 활성 (쓰기 가능) | `Snapshotter.Prepare()` | X | O |
| `Committed` | 커밋됨 (불변) | `Snapshotter.Commit()` | O | X |

### 5.2 Snapshot Info

```go
type Info struct {
    Kind    Kind              // 스냅샷 종류 (Active/Committed/View)
    Name    string            // 이름 또는 키
    Parent  string            // 부모 스냅샷 이름 (없으면 "")
    Labels  map[string]string // 레이블
    Created time.Time         // 생성 시각
    Updated time.Time         // 수정 시각
}
```

### 5.3 Snapshotter 인터페이스

```go
type Snapshotter interface {
    Stat(ctx context.Context, key string) (Info, error)
    Update(ctx context.Context, info Info, fieldpaths ...string) (Info, error)
    Usage(ctx context.Context, key string) (Usage, error)
    Mounts(ctx context.Context, key string) ([]mount.Mount, error)
    Prepare(ctx context.Context, key, parent string, opts ...Opt) ([]mount.Mount, error)
    View(ctx context.Context, key, parent string, opts ...Opt) ([]mount.Mount, error)
    Commit(ctx context.Context, name, key string, opts ...Opt) error
    Remove(ctx context.Context, key string) error
    Walk(ctx context.Context, fn WalkFunc, filters ...string) error
    Close() error
}
```

### 5.4 스냅샷 수명 주기

```
이미지 레이어 언팩:

                    ""(빈 부모)
                     │
    Prepare("extract-1", "")
                     │ 레이어 1 적용
    Commit("layer-1-chainid", "extract-1")
                     │
                  layer-1  [Committed]
                     │
    Prepare("extract-2", "layer-1")
                     │ 레이어 2 적용
    Commit("layer-2-chainid", "extract-2")
                     │
                  layer-2  [Committed]
                     │
                    ...

컨테이너 실행:

                  layer-N  [Committed] (이미지 최상위 레이어)
                     │
    Prepare("container-rootfs", "layer-N")
                     │
              container-rootfs [Active] ← 컨테이너가 여기에 쓰기
                     │
                   (컨테이너 종료 시)
    Remove("container-rootfs")

이미지 비교용:

                  layer-N  [Committed]
                     │
    View("readonly-view", "layer-N")
                     │
              readonly-view [View] ← 읽기만 가능
```

### 5.5 Usage

```go
type Usage struct {
    Inodes int64   // 사용 중인 inode 수
    Size   int64   // 스냅샷이 사용하는 바이트 크기 (부모 제외)
}
```

### 5.6 지원되는 Snapshotter 구현

| Snapshotter | 파일시스템 | 특징 |
|-------------|-----------|------|
| **overlayfs** | OverlayFS | 기본 snapshotter, 커널 4.x 이상 |
| **native** | 일반 FS | 하드 링크 + 디렉토리 복사 |
| **btrfs** | Btrfs | Btrfs 서브볼륨 기반 스냅샷 |
| **zfs** | ZFS | ZFS 스냅샷/클론 활용 |
| **devmapper** | Device Mapper | 블록 레벨 씬 프로비저닝 |

---

## 6. Runtime / Task / Process

Runtime은 **컨테이너 프로세스의 생성과 관리**를 담당한다.

```
소스 참조: core/runtime/runtime.go (Line 28~83)
```

### 6.1 Runtime 인터페이스

```go
// PlatformRuntime은 플랫폼별 태스크와 프로세스의 생성/관리를 담당한다
type PlatformRuntime interface {
    ID() string
    Create(ctx context.Context, taskID string, opts CreateOpts) (Task, error)
    Get(ctx context.Context, taskID string) (Task, error)
    Tasks(ctx context.Context, all bool) ([]Task, error)
    Delete(ctx context.Context, taskID string) (*Exit, error)
}
```

### 6.2 CreateOpts

```go
type CreateOpts struct {
    Spec            typeurl.Any     // OCI Runtime Spec
    Rootfs          []mount.Mount   // rootfs 마운트 목록
    IO              IO              // stdin/stdout/stderr
    Checkpoint      string          // 체크포인트 digest (복원용)
    RestoreFromPath bool            // 로컬 체크포인트 복원 여부
    RuntimeOptions  typeurl.Any     // 런타임 옵션
    TaskOptions     typeurl.Any     // 태스크 옵션
    Runtime         string          // 런타임 이름
    SandboxID       string          // 소속 샌드박스 ID
    Address         string          // Task API 서버 주소
    Version         uint32          // Task API 버전
}
```

### 6.3 IO 구조체

```go
type IO struct {
    Stdin    string   // stdin FIFO 경로
    Stdout   string   // stdout FIFO 경로
    Stderr   string   // stderr FIFO 경로
    Terminal bool     // TTY 모드 여부
}
```

### 6.4 Exit 정보

```go
type Exit struct {
    Pid       uint32      // 프로세스 ID
    Status    uint32      // 종료 코드
    Timestamp time.Time   // 종료 시각
}
```

### 6.5 Container → Task → Process 관계

```
Container (메타데이터만)
    │
    │ Task.Create()
    v
Task (실행 중인 컨테이너)
    │
    ├─ 메인 프로세스 (PID 1)
    │   └─ stdin/stdout/stderr
    │
    └─ Exec 프로세스 (추가 프로세스)
        └─ Task.Exec() 으로 생성

관계 규칙:
- Container : Task = 1 : 0..1 (한 번에 하나의 Task만)
- Task : Process = 1 : 1..N (메인 + exec)
- Task가 없으면 Container는 "정지" 상태
```

---

## 7. Sandbox

Sandbox는 **Pod 수준의 격리 환경**을 나타낸다.
Kubernetes CRI에서 Pod는 Sandbox로 모델링되며, 여러 컨테이너가 하나의 Sandbox를 공유한다.

```
소스 참조: core/sandbox/store.go (Line 29~118)
```

### 7.1 Sandbox 구조체

```go
type Sandbox struct {
    // ID는 네임스페이스 내에서 샌드박스를 고유하게 식별한다
    ID string

    // Labels는 메타데이터 확장을 위한 키-값 쌍
    Labels map[string]string

    // Runtime은 이 샌드박스에 사용할 Shim 런타임
    Runtime RuntimeOpts

    // Spec은 샌드박스 구현을 위한 런타임 사양
    Spec typeurl.Any

    // Sandboxer는 이 샌드박스를 관리하는 컨트롤러 이름
    Sandboxer string

    // 생성/수정 시각
    CreatedAt time.Time
    UpdatedAt time.Time

    // Extensions는 클라이언트 지정 메타데이터
    Extensions map[string]typeurl.Any
}
```

### 7.2 RuntimeOpts

```go
type RuntimeOpts struct {
    Name    string        // 런타임 이름 (예: "io.containerd.runc.v2")
    Options typeurl.Any   // 런타임별 옵션
}
```

### 7.3 Sandbox Controller 인터페이스

```
소스 참조: core/sandbox/controller.go (Line 95~117)
```

```go
type Controller interface {
    Create(ctx context.Context, sandboxInfo Sandbox, opts ...CreateOpt) error
    Start(ctx context.Context, sandboxID string) (ControllerInstance, error)
    Platform(ctx context.Context, sandboxID string) (imagespec.Platform, error)
    Stop(ctx context.Context, sandboxID string, opts ...StopOpt) error
    Wait(ctx context.Context, sandboxID string) (ExitStatus, error)
    Status(ctx context.Context, sandboxID string, verbose bool) (ControllerStatus, error)
    Shutdown(ctx context.Context, sandboxID string) error
    Metrics(ctx context.Context, sandboxID string) (*types.Metric, error)
    Update(ctx context.Context, sandboxID string, sandbox Sandbox, fields ...string) error
}
```

### 7.4 ControllerInstance

```go
type ControllerInstance struct {
    SandboxID string
    Pid       uint32
    CreatedAt time.Time
    Address   string            // Shim TTRPC 주소
    Version   uint32            // Task API 버전
    Labels    map[string]string
    Spec      typeurl.Any
}
```

### 7.5 Sandbox Store 인터페이스

```go
type Store interface {
    Create(ctx context.Context, sandbox Sandbox) (Sandbox, error)
    Update(ctx context.Context, sandbox Sandbox, fieldpaths ...string) (Sandbox, error)
    Get(ctx context.Context, id string) (Sandbox, error)
    List(ctx context.Context, filters ...string) ([]Sandbox, error)
    Delete(ctx context.Context, id string) error
}
```

### 7.6 Sandbox ↔ Container 관계 (CRI)

```
Kubernetes Pod
    │
    └─ CRI RunPodSandbox
        │
        ├─ Sandbox 생성 (pause 컨테이너)
        │   ├─ ID: "sandbox-abc123"
        │   ├─ Runtime: "io.containerd.runc.v2"
        │   └─ Spec: Pod 네트워크/IPC/PID 네임스페이스 설정
        │
        ├─ Container A (SandboxID: "sandbox-abc123")
        │   └─ 애플리케이션 컨테이너
        │
        └─ Container B (SandboxID: "sandbox-abc123")
            └─ 사이드카 컨테이너
```

---

## 8. Lease

Lease는 **리소스를 GC(가비지 컬렉션)로부터 보호**하는 메커니즘이다.
이미지 Pull이나 컨테이너 생성 같은 장기 작업 중에 리소스가 삭제되지 않도록 보장한다.

```
소스 참조: core/leases/lease.go (Line 30~105)
```

### 8.1 Lease 구조체

```go
type Lease struct {
    ID        string             // 리스 고유 ID
    CreatedAt time.Time          // 생성 시각
    Labels    map[string]string  // 레이블 (만료 시간 등)
}
```

### 8.2 Resource

```go
type Resource struct {
    ID   string   // 리소스 식별자 (digest, snapshot key 등)
    Type string   // 리소스 타입 ("content", "snapshots/<name>" 등)
}
```

### 8.3 Lease Manager 인터페이스

```go
type Manager interface {
    Create(context.Context, ...Opt) (Lease, error)
    Delete(context.Context, Lease, ...DeleteOpt) error
    List(context.Context, ...string) ([]Lease, error)
    AddResource(context.Context, Lease, Resource) error
    DeleteResource(context.Context, Lease, Resource) error
    ListResources(context.Context, Lease) ([]Resource, error)
}
```

### 8.4 Lease 옵션

| 옵션 | 설명 |
|------|------|
| `WithLabel(key, value)` | 레이블 설정 |
| `WithLabels(map)` | 여러 레이블 일괄 설정 |
| `WithExpiration(duration)` | 만료 시간 설정 (`containerd.io/gc.expire` 레이블) |
| `SynchronousDelete` | 삭제 시 참조되지 않는 리소스 즉시 정리 |

### 8.5 Lease 사용 패턴

```
이미지 Pull 시:

1. Lease 생성 (만료: 24시간)
   lease, _ := leaseManager.Create(ctx, WithExpiration(24*time.Hour))

2. 콘텐츠 다운로드 시 리소스 등록
   leaseManager.AddResource(ctx, lease, Resource{
       ID:   "sha256:abc123",     // 매니페스트 digest
       Type: "content",
   })
   leaseManager.AddResource(ctx, lease, Resource{
       ID:   "sha256:def456",     // 레이어 digest
       Type: "content",
   })

3. 스냅샷 언팩 시 리소스 등록
   leaseManager.AddResource(ctx, lease, Resource{
       ID:   "layer-chainid-1",
       Type: "snapshots/overlayfs",
   })

4. 작업 완료 후 Lease 삭제
   leaseManager.Delete(ctx, lease)
   → 이제 GC가 미참조 리소스를 정리할 수 있음
```

---

## 9. Event

Event는 containerd 내부의 **비동기 이벤트 시스템**을 구현한다.

```
소스 참조: core/events/events.go (Line 26~81)
```

### 9.1 Envelope

```go
type Envelope struct {
    Timestamp time.Time      // 이벤트 발생 시각
    Namespace string         // 네임스페이스
    Topic     string         // 이벤트 토픽 ("/tasks/exit", "/images/create" 등)
    Event     typeurl.Any    // 이벤트 페이로드 (protobuf Any)
}
```

### 9.2 Publisher / Subscriber 인터페이스

```go
// Publisher는 이벤트를 발행한다
type Publisher interface {
    Publish(ctx context.Context, topic string, event Event) error
}

// Forwarder는 이벤트를 이벤트 버스에 전달한다
type Forwarder interface {
    Forward(ctx context.Context, envelope *Envelope) error
}

// Subscriber는 이벤트를 구독한다
type Subscriber interface {
    Subscribe(ctx context.Context, filters ...string) (ch <-chan *Envelope, errs <-chan error)
}
```

### 9.3 이벤트 토픽 예시

| 토픽 | 발생 시점 |
|------|----------|
| `/tasks/create` | 태스크 생성 |
| `/tasks/start` | 태스크 시작 |
| `/tasks/exit` | 태스크 종료 |
| `/tasks/oom` | OOM(Out of Memory) 발생 |
| `/tasks/exec-added` | exec 프로세스 추가 |
| `/tasks/exec-started` | exec 프로세스 시작 |
| `/tasks/paused` | 태스크 일시 정지 |
| `/tasks/resumed` | 태스크 재개 |
| `/images/create` | 이미지 생성 |
| `/images/update` | 이미지 수정 |
| `/images/delete` | 이미지 삭제 |
| `/containers/create` | 컨테이너 생성 |
| `/containers/update` | 컨테이너 수정 |
| `/containers/delete` | 컨테이너 삭제 |
| `/namespaces/create` | 네임스페이스 생성 |
| `/namespaces/update` | 네임스페이스 수정 |
| `/namespaces/delete` | 네임스페이스 삭제 |
| `/snapshots/prepare` | 스냅샷 준비 |
| `/snapshots/commit` | 스냅샷 커밋 |
| `/snapshots/remove` | 스냅샷 삭제 |

---

## 10. Metadata DB 스키마

모든 엔티티의 메타데이터는 **BoltDB**에 저장된다.

```
소스 참조: core/metadata/db.go (Line 40~54, 78~115)
```

### 10.1 DB 구조체

```go
type DB struct {
    db  Transactor                       // BoltDB 트랜잭터
    ss  map[string]*snapshotter          // 네임스페이스별 스냅샷터 프록시
    cs  *contentStore                    // 콘텐츠 스토어 프록시

    wlock sync.RWMutex                   // GC 중 쓰기 잠금
    dirty atomic.Uint32                  // GC 필요 플래그

    dirtySS          map[string]struct{} // 삭제된 스냅샷터 추적
    dirtyCS          bool                // 삭제된 콘텐츠 추적
    mutationCallbacks []func(bool)       // 변경 콜백
    collectors       map[gc.ResourceType]Collector // GC 수집기

    dbopts dbOptions
}
```

### 10.2 BoltDB 버킷 구조

```
v1/                              ← 스키마 버전
├── {namespace}/                 ← 네임스페이스별 격리
│   ├── containers/              ← 컨테이너 메타데이터
│   │   └── {container-id}/
│   │       ├── labels
│   │       ├── image
│   │       ├── runtime
│   │       ├── spec
│   │       ├── snapshotKey
│   │       ├── snapshotter
│   │       ├── createdAt
│   │       ├── updatedAt
│   │       ├── extensions
│   │       └── sandboxID
│   │
│   ├── images/                  ← 이미지 메타데이터
│   │   └── {image-name}/
│   │       ├── labels
│   │       ├── target (descriptor)
│   │       ├── createdAt
│   │       └── updatedAt
│   │
│   ├── content/                 ← 콘텐츠 레이블 (실제 blob은 파일시스템)
│   │   └── {digest}/
│   │       └── labels
│   │
│   ├── snapshots/               ← 스냅샷 메타데이터 (실제 데이터는 snapshotter)
│   │   └── {snapshotter}/
│   │       └── {name}/
│   │
│   ├── sandboxes/               ← 샌드박스 메타데이터
│   │   └── {sandbox-id}/
│   │
│   └── leases/                  ← 리스 메타데이터
│       └── {lease-id}/
│           ├── labels
│           └── resources/
│               └── {type}/{id}
│
└── ...
```

---

## 11. 데이터 모델 요약

### 11.1 엔티티 비교 테이블

| 엔티티 | 식별자 | 저장소 | 주요 필드 | 관계 |
|--------|--------|--------|----------|------|
| **Container** | `ID` | BoltDB | Runtime, Spec, SnapshotKey, SandboxID | Image, Snapshot, Sandbox에 참조 |
| **Image** | `Name` | BoltDB | Target(Descriptor), Labels | Content 참조 (digest) |
| **Content** | `Digest` | 파일시스템 + BoltDB Labels | Size, Labels | Image에서 참조됨 |
| **Snapshot** | `Name/Key` | Snapshotter + BoltDB | Kind, Parent, Labels | Container rootfs, Content 레이어 |
| **Task** | Container `ID` | Shim 프로세스 | PID, Status, IO | Container에서 1:0..1 |
| **Sandbox** | `ID` | BoltDB | Runtime, Spec, Sandboxer | Container에서 참조 (SandboxID) |
| **Lease** | `ID` | BoltDB | Labels, Resources | Content, Snapshot 보호 |
| **Event** | 없음 (스트림) | 메모리 | Topic, Namespace, Payload | 모든 엔티티 변경 시 발행 |

### 11.2 typeurl.Any 패턴

containerd는 Google Protobuf의 `Any` 타입을 확장한 **typeurl.Any**를 광범위하게 사용한다.
이를 통해 **런타임 사양(OCI Spec), 런타임 옵션, 확장 데이터** 등을 타입 안전하게 직렬화/역직렬화한다.

```go
// Container.Spec, Container.Runtime.Options, Sandbox.Spec 등에서 사용
type Any interface {
    GetTypeUrl() string  // 타입 URL (예: "types.containerd.io/opencontainers/runtime-spec/1/Spec")
    GetValue() []byte    // 직렬화된 바이트
}
```

### 11.3 데이터 흐름 요약

```
Registry ──Pull──> Content Store ──Unpack──> Snapshotter
                       │                         │
                       │ digest                   │ snapshot key
                       v                         v
                   BoltDB Metadata (namespace-isolated)
                       │                         │
                       │ image name               │ container.SnapshotKey
                       v                         v
                    Image ──────────────────> Container
                                                 │
                                                 │ Task.Create()
                                                 v
                                               Task ──> Shim ──> runc ──> Process
```
