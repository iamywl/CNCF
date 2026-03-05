# containerd 코드 구조

## 1. 개요

containerd는 **Go 모듈 기반의 모노레포**로 구성되며, `github.com/containerd/containerd/v2` 모듈 경로를 사용한다.
코드는 역할에 따라 `cmd/`, `core/`, `plugins/`, `client/`, `api/`, `internal/`, `pkg/`로 명확하게 분리되어 있다.

---

## 2. 최상위 디렉토리 구조

```
containerd/
├── api/                    # Protobuf API 정의 (별도 모듈: containerd/containerd/api)
├── client/                 # Go 클라이언트 라이브러리
├── cluster/                # 클러스터 테스트 설정
├── cmd/                    # 실행 바이너리 (4개 메인 + 3개 유틸리티)
├── contrib/                # 부가 도구 (Ansible, Dockerfile, autocomplete 등)
├── core/                   # 핵심 인터페이스 및 데이터 타입 (17개 서브시스템)
├── defaults/               # 기본값 상수 (경로, 포트 등)
├── docs/                   # 문서
├── integration/            # 통합 테스트
├── internal/               # 내부 전용 패키지 (외부 import 불가)
├── pkg/                    # 외부 사용 가능 유틸리티 패키지
├── plugins/                # 플러그인 구현체
├── releases/               # 릴리스 노트
├── script/                 # 빌드/CI 스크립트
├── test/                   # 테스트 유틸리티
├── vendor/                 # 벤더링된 의존성
├── version/                # 버전 정보
├── go.mod                  # Go 모듈 정의
├── go.sum                  # 의존성 체크섬
├── Makefile                # 메인 빌드 시스템
├── Makefile.linux           # Linux 빌드 타겟
├── Makefile.darwin          # macOS 빌드 타겟
├── Makefile.windows         # Windows 빌드 타겟
├── Makefile.freebsd         # FreeBSD 빌드 타겟
├── containerd.service      # systemd 서비스 파일
└── Vagrantfile             # Vagrant 개발 환경
```

---

## 3. cmd/ - 실행 바이너리

```
소스 참조: Makefile (Line 87)
  COMMANDS=ctr containerd containerd-stress
```

```
cmd/
├── containerd/                     # containerd 데몬 (메인 바이너리)
│   ├── main.go                     # 진입점: command.App() 호출
│   ├── command/                    # CLI 명령어 정의
│   │   ├── main.go                 # App() 함수: 설정 로드, 서버 시작
│   │   ├── config.go               # config 서브커맨드
│   │   ├── publish.go              # publish 서브커맨드
│   │   └── oci-hook.go             # OCI hook 서브커맨드
│   ├── server/                     # 서버 구현
│   │   ├── server.go               # Server 구조체, New(), 플러그인 로딩
│   │   └── config/                 # 설정 구조체 및 로더
│   │       └── config.go           # Config, LoadConfig, MigrateConfig
│   └── builtins/                   # 내장 플러그인 등록 (blank import)
│       ├── builtins.go             # 공통 플러그인
│       ├── builtins_linux.go       # Linux 전용 (overlayfs 등)
│       ├── builtins_unix.go        # Unix 공통
│       ├── builtins_windows.go     # Windows 전용
│       ├── cri.go                  # CRI 플러그인
│       └── tracing.go             # OpenTelemetry 트레이싱
│
├── containerd-shim-runc-v2/        # Shim v2 바이너리
│   └── main.go                     # shim 진입점
│
├── containerd-stress/              # 스트레스 테스트 도구
│   └── main.go
│
├── ctr/                            # ctr CLI 도구
│   └── main.go                     # containerd 관리 CLI
│
├── gen-manpages/                   # man 페이지 생성기
├── go-buildtag/                    # 빌드 태그 유틸리티
└── protoc-gen-go-fieldpath/        # Protobuf fieldpath 생성기
```

### 3.1 바이너리 요약

| 바이너리 | 경로 | 역할 |
|---------|------|------|
| `containerd` | `cmd/containerd/` | 메인 데몬, gRPC/TTRPC API 서버 |
| `ctr` | `cmd/ctr/` | containerd 관리 CLI (디버깅용) |
| `containerd-shim-runc-v2` | `cmd/containerd-shim-runc-v2/` | 컨테이너 프로세스 관리 Shim |
| `containerd-stress` | `cmd/containerd-stress/` | 성능/스트레스 테스트 도구 |

### 3.2 진입점 흐름

```
cmd/containerd/main.go
  │
  ├── import _ "cmd/containerd/builtins"     ← 모든 내장 플러그인 등록
  │
  └── main()
        └── command.App()                     ← CLI 앱 생성
              └── server.New(ctx, config)     ← 서버 초기화
                    └── LoadPlugins()          ← 플러그인 DFS 정렬
                          └── registry.Graph() ← 토폴로지 정렬된 플러그인 목록
```

---

## 4. core/ - 핵심 인터페이스

`core/`는 containerd의 **핵심 데이터 타입과 인터페이스**를 정의한다.
실제 구현은 `plugins/`에 있으며, `core/`는 계약(contract)만 정의한다.

```
core/
├── containers/             # Container 구조체 및 Store 인터페이스
│   └── containers.go       # Container{ID, Image, Runtime, Spec, ...}
│
├── content/                # Content Store 인터페이스
│   ├── content.go          # Store, Provider, Ingester, Manager, Writer
│   ├── helpers.go          # ReadBlob, WriteBlob 등 유틸리티
│   ├── adaptor.go          # 인터페이스 어댑터
│   └── proxy/              # gRPC 프록시 클라이언트
│
├── diff/                   # Diff 서비스 인터페이스
│   └── proxy/
│
├── events/                 # Event 시스템 인터페이스
│   ├── events.go           # Publisher, Subscriber, Forwarder, Envelope
│   └── exchange/           # Exchange 구현 (in-memory)
│       └── exchange.go
│
├── images/                 # Image 구조체 및 Store 인터페이스
│   └── image.go            # Image{Name, Labels, Target}, Manifest(), Children()
│
├── introspection/          # 내부 상태 조회
│
├── leases/                 # Lease 관리 인터페이스
│   ├── lease.go            # Lease{ID, Labels}, Resource{ID, Type}, Manager
│   ├── context.go          # 컨텍스트에서 Lease 추출
│   └── grpc.go             # gRPC 메타데이터에서 Lease 추출
│
├── metadata/               # BoltDB 메타데이터 저장소 구현
│   ├── db.go               # DB 구조체, NewDB, Init, GC
│   ├── containers.go       # Container Store 구현
│   ├── images.go           # Image Store 구현
│   ├── content.go          # Content Store 메타데이터 래퍼
│   ├── snapshot.go         # Snapshotter 메타데이터 래퍼
│   ├── leases.go           # Lease Manager 구현
│   ├── namespaces.go       # Namespace Store
│   ├── sandbox.go          # Sandbox Store 구현
│   ├── gc.go               # GC 구현 (Mark/Sweep 로직)
│   ├── buckets.go          # BoltDB 버킷 키 정의
│   ├── migrations.go       # 스키마 마이그레이션
│   └── boltutil/           # BoltDB 유틸리티 (레이블, 타임스탬프 등)
│
├── metrics/                # 메트릭 정의
│
├── mount/                  # 마운트 유틸리티
│
├── remotes/                # 원격 레지스트리 리졸버
│
├── runtime/                # Runtime 인터페이스
│   ├── runtime.go          # PlatformRuntime, Task, Process, CreateOpts, Exit
│   └── v2/                 # Runtime v2 구현 (Shim Manager)
│       ├── shim.go         # Shim 관리
│       ├── shim_manager.go # ShimManager: shim 시작/관리
│       ├── task_manager.go # TaskManager: task 생성/관리
│       ├── bundle.go       # OCI 번들 관리
│       ├── binary.go       # shim 바이너리 실행
│       ├── process.go      # 프로세스 관리
│       └── bridge.go       # TTRPC/gRPC 브릿지
│
├── sandbox/                # Sandbox 인터페이스
│   ├── store.go            # Sandbox{ID, Labels, Runtime, Spec}, Store
│   ├── controller.go       # Controller 인터페이스 (Create, Start, Stop 등)
│   ├── bridge.go           # Sandbox Controller 브릿지
│   └── helpers.go          # 유틸리티
│
├── snapshots/              # Snapshotter 인터페이스
│   ├── snapshotter.go      # Snapshotter 인터페이스, Info, Kind, Usage
│   ├── storage/            # Snapshotter용 메타데이터 저장소
│   └── proxy/              # gRPC 프록시 클라이언트
│
├── streaming/              # 스트리밍 인터페이스
│
├── transfer/               # 전송 서비스 인터페이스
│   ├── transfer.go         # Transfer 인터페이스
│   ├── registry/           # 레지스트리 전송
│   ├── image/              # 이미지 전송
│   ├── archive/            # 아카이브 전송
│   ├── local/              # 로컬 전송
│   ├── streaming/          # 스트리밍 전송
│   └── plugins/            # 전송 플러그인
│
└── unpack/                 # 이미지 언팩 로직
```

### 4.1 core/ 서브시스템 요약

| 서브시스템 | 패키지 | 핵심 타입/인터페이스 |
|-----------|--------|-------------------|
| **Container** | `core/containers` | `Container`, `Store` |
| **Image** | `core/images` | `Image`, `Store`, `Manifest()`, `Children()` |
| **Content** | `core/content` | `Store`, `Provider`, `Ingester`, `Writer` |
| **Snapshot** | `core/snapshots` | `Snapshotter`, `Info`, `Kind` |
| **Runtime** | `core/runtime` | `PlatformRuntime`, `Task`, `CreateOpts` |
| **Sandbox** | `core/sandbox` | `Sandbox`, `Store`, `Controller` |
| **Lease** | `core/leases` | `Lease`, `Resource`, `Manager` |
| **Event** | `core/events` | `Publisher`, `Subscriber`, `Envelope` |
| **Metadata** | `core/metadata` | `DB`, `NewDB()`, GC 로직 |
| **Transfer** | `core/transfer` | `Transfer` 인터페이스, Registry/Archive 전송 |
| **Mount** | `core/mount` | 마운트/언마운트 유틸리티 |
| **Diff** | `core/diff` | Diff 계산, Apply |
| **Metrics** | `core/metrics` | Prometheus 메트릭 |
| **Streaming** | `core/streaming` | 스트리밍 인터페이스 |
| **Unpack** | `core/unpack` | 이미지 레이어 언팩 |
| **Remotes** | `core/remotes` | 레지스트리 리졸버 |
| **Introspection** | `core/introspection` | 서버 상태 조회 |

---

## 5. plugins/ - 플러그인 구현체

`plugins/`는 `core/`에 정의된 인터페이스의 **실제 구현**을 담는다.
각 플러그인은 `init()`에서 `registry.Register()`를 호출하여 자동 등록된다.

```
plugins/
├── types.go                # 플러그인 타입 상수 정의 (30+ 타입)
│
├── content/                # Content Store 구현
│   └── local/              # 파일시스템 기반 로컬 Content Store
│       └── plugin/
│
├── cri/                    # CRI (Container Runtime Interface) 플러그인
│
├── diff/                   # Diff 서비스 구현
│
├── events/                 # Event Exchange 구현
│
├── gc/                     # GC Scheduler 구현
│   ├── scheduler.go        # GC 스케줄러 (threshold, delay 설정)
│   ├── scheduler_test.go
│   └── metrics.go          # GC 메트릭
│
├── imageverifier/          # 이미지 검증 플러그인
│
├── leases/                 # Lease Manager 구현
│
├── metadata/               # Metadata Plugin (BoltDB 초기화)
│
├── mount/                  # Mount Manager 구현
│
├── nri/                    # NRI (Node Resource Interface)
│
├── restart/                # 컨테이너 자동 재시작 플러그인
│
├── sandbox/                # Sandbox Controller 구현
│
├── services/               # gRPC/TTRPC 서비스 플러그인
│   ├── containers/         # Containers gRPC 서비스
│   ├── content/            # Content gRPC 서비스
│   ├── diff/               # Diff gRPC 서비스
│   ├── events/             # Events gRPC 서비스
│   ├── healthcheck/        # Health Check 서비스
│   ├── images/             # Images gRPC 서비스
│   ├── introspection/      # Introspection gRPC 서비스
│   ├── leases/             # Leases gRPC 서비스
│   ├── mounts/             # Mounts gRPC 서비스
│   ├── namespaces/         # Namespaces gRPC 서비스
│   ├── opt/                # Opt (내부) 서비스
│   ├── sandbox/            # Sandbox gRPC 서비스
│   ├── snapshots/          # Snapshots gRPC 서비스
│   ├── streaming/          # Streaming gRPC 서비스
│   ├── tasks/              # Tasks gRPC 서비스
│   ├── transfer/           # Transfer gRPC 서비스
│   ├── version/            # Version gRPC 서비스
│   └── warning/            # Warning 서비스
│
├── snapshots/              # Snapshotter 구현체
│   └── (overlay, native, btrfs, zfs, devmapper 등)
│
├── streaming/              # Streaming 플러그인
│
└── transfer/               # Transfer 플러그인
```

### 5.1 플러그인 등록 패턴

각 플러그인 패키지의 `init()` 함수에서 등록:

```go
// plugins/gc/scheduler.go 예시
func init() {
    registry.Register(&plugin.Registration{
        Type: plugins.GCPlugin,
        ID:   "scheduler",
        Requires: []plugin.Type{
            plugins.MetadataPlugin,
        },
        Config: &config{
            PauseThreshold:    0.02,
            DeletionThreshold: 0,
            MutationThreshold: 100,
            ScheduleDelay:     tomlext.Duration(0),
            StartupDelay:      tomlext.Duration(100 * time.Millisecond),
        },
        InitFn: func(ic *plugin.InitContext) (interface{}, error) {
            // 플러그인 초기화 로직
        },
    })
}
```

---

## 6. api/ - Protobuf API 정의

API 정의는 **별도 Go 모듈** (`github.com/containerd/containerd/api`)로 관리된다.

```
api/
├── events/                         # 이벤트 타입 Proto
│   ├── container.proto             # ContainerCreate, ContainerUpdate, ContainerDelete
│   ├── content.proto               # ContentDelete
│   ├── image.proto                 # ImageCreate, ImageUpdate, ImageDelete
│   ├── namespace.proto             # NamespaceCreate, NamespaceUpdate, NamespaceDelete
│   ├── sandbox.proto               # SandboxCreate, SandboxStart, SandboxExit
│   ├── snapshot.proto              # SnapshotPrepare, SnapshotCommit, SnapshotRemove
│   └── task.proto                  # TaskCreate, TaskStart, TaskExit, TaskOOM, TaskExecAdded
│
├── runtime/                        # 런타임 API
│   ├── sandbox/v1/                 # Sandbox Shim API
│   │   └── sandbox.proto           # SandboxService (CreateSandbox, StartSandbox, ...)
│   └── task/
│       ├── v2/shim.proto           # Task Shim API v2
│       └── v3/shim.proto           # Task Shim API v3
│
├── services/                       # gRPC 서비스 정의
│   ├── containers/v1/containers.proto
│   ├── content/v1/content.proto
│   ├── diff/v1/diff.proto
│   ├── events/v1/events.proto
│   ├── images/v1/images.proto
│   ├── introspection/v1/introspection.proto
│   ├── leases/v1/leases.proto
│   ├── mounts/v1/mounts.proto
│   ├── namespaces/v1/namespace.proto
│   ├── sandbox/v1/sandbox.proto
│   ├── snapshots/v1/snapshots.proto
│   ├── streaming/v1/streaming.proto
│   ├── tasks/v1/tasks.proto
│   ├── transfer/v1/transfer.proto
│   ├── ttrpc/events/v1/events.proto    # TTRPC 이벤트 서비스
│   └── version/v1/version.proto
│
├── types/                          # 공통 타입 정의
│   ├── descriptor.proto            # Descriptor
│   ├── event.proto                 # Envelope
│   ├── fieldpath.proto             # FieldPath
│   ├── introspection.proto         # PluginInfo, ServerInfo
│   ├── metrics.proto               # Metric
│   ├── mount.proto                 # Mount
│   ├── platform.proto              # Platform
│   ├── sandbox.proto               # Sandbox
│   ├── task/task.proto             # Task 상태
│   ├── runc/options/oci.proto      # runc OCI 옵션
│   ├── runtimeoptions/v1/api.proto # 런타임 옵션
│   └── transfer/                   # 전송 관련 타입
│       ├── imagestore.proto
│       ├── importexport.proto
│       ├── progress.proto
│       ├── registry.proto
│       └── streaming.proto
│
└── releases/                       # 릴리스 태그
```

### 6.1 Proto 서비스 패턴

각 서비스 Proto 파일은 다음 패턴을 따른다:

```
api/services/{service}/v1/{service}.proto
  ↓ protoc 생성
api/services/{service}/v1/{service}.pb.go        # 타입 정의
api/services/{service}/v1/{service}_grpc.pb.go   # gRPC 서버/클라이언트
```

---

## 7. client/ - Go 클라이언트 라이브러리

```
client/
├── client.go               # Client 구조체, New(), gRPC 연결
├── container.go            # Container 객체 (메타데이터 + Task 생성)
├── image.go                # Image 객체 (Pull, Unpack, Config)
├── task.go                 # Task 객체 (Start, Kill, Wait, Delete)
├── pull.go                 # Pull 동작
├── push.go                 # Push 동작
├── leases.go               # Lease 관리
├── content.go              # Content Store 접근
├── snapshots.go            # Snapshot 접근
├── namespace.go            # Namespace 접근
└── ...
```

---

## 8. internal/ - 내부 전용 패키지

Go의 `internal` 패키지 규칙에 의해 **외부 모듈에서 import 불가**한 패키지이다.

```
internal/
├── cri/                    # CRI 플러그인 내부 구현 (가장 큰 패키지)
│   ├── server/             # CRI gRPC 서비스 구현
│   ├── store/              # CRI 내부 저장소
│   └── ...
├── cleanup/                # 정리 유틸리티
├── erofsutils/             # EROFS 유틸리티
├── eventq/                 # 이벤트 큐
├── failpoint/              # 테스트용 장애 주입
├── fsmount/                # 파일시스템 마운트
├── fsverity/               # fs-verity 지원
├── kmutex/                 # 키 기반 뮤텍스
├── lazyregexp/             # 지연 초기화 정규식
├── nri/                    # NRI 내부 구현
├── oom/                    # OOM 모니터
├── pprof/                  # pprof 래퍼
├── randutil/               # 랜덤 유틸리티
├── registrar/              # 레지스트라
├── tomlext/                # TOML 확장 (Duration 등)
├── truncindex/             # 트렁케이트 인덱스
├── userns/                 # 사용자 네임스페이스
└── wintls/                 # Windows TLS
```

---

## 9. pkg/ - 외부 사용 가능 유틸리티

`pkg/`는 containerd와 외부 프로젝트 모두에서 사용 가능한 유틸리티 패키지이다.

```
pkg/
├── gc/                     # GC 알고리즘 (Tricolor Mark-and-Sweep)
├── shim/                   # Shim 유틸리티 (외부 shim 구현용)
├── cio/                    # 컨테이너 I/O (FIFO 관리)
├── oci/                    # OCI Spec 생성 유틸리티
├── namespaces/             # 네임스페이스 유틸리티
├── filters/                # 필터 파싱 (fieldpath 기반)
├── archive/                # tar 아카이브 유틸리티
├── reference/              # 이미지 참조 파싱
├── identifiers/            # ID 검증
├── labels/                 # 레이블 검증
├── rootfs/                 # rootfs 관리
├── snapshotters/           # Snapshotter 유틸리티
├── dialer/                 # gRPC 다이얼러
├── timeout/                # 타임아웃 관리
├── progress/               # 진행률 표시
├── display/                # 출력 포매팅
├── sys/                    # 시스템 호출 래퍼
├── apparmor/               # AppArmor 프로필
├── seccomp/                # Seccomp 프로필
├── cap/                    # Linux Capabilities
├── cdi/                    # CDI (Container Device Interface)
├── blockio/                # Block I/O 설정
├── rdt/                    # RDT (Resource Director Technology)
├── netns/                  # 네트워크 네임스페이스
├── epoch/                  # SOURCE_DATE_EPOCH
├── ioutil/                 # I/O 유틸리티
├── atomicfile/             # 원자적 파일 쓰기
├── protobuf/               # Protobuf 유틸리티
├── deprecation/            # 더이상 사용되지 않는 기능 추적
├── imageverifier/          # 이미지 검증
├── tracing/                # OpenTelemetry 트레이싱
├── ttrpcutil/              # TTRPC 유틸리티
├── fifosync/               # FIFO 동기화
├── shutdown/               # 셧다운 시그널
├── stdio/                  # 표준 I/O
├── httpdbg/                # HTTP 디버그
├── schedcore/              # 스케줄러 코어
├── oom/                    # OOM 관리
├── os/                     # OS 추상화
├── kernelversion/          # 커널 버전 확인
└── testutil/               # 테스트 유틸리티
```

---

## 10. 빌드 시스템

### 10.1 Go 모듈

```
소스 참조: go.mod (Line 1~2)
  module github.com/containerd/containerd/v2
  go 1.24.6
```

containerd는 **두 개의 Go 모듈**로 구성된다:

| 모듈 | 경로 | 용도 |
|------|------|------|
| `github.com/containerd/containerd/v2` | `go.mod` | 메인 모듈 (데몬, 클라이언트, 플러그인) |
| `github.com/containerd/containerd/api` | `api/go.mod` | API 정의 (Proto 생성 코드) |

### 10.2 Makefile 타겟

```
소스 참조: Makefile (Line 87~88, 159, 165~178)
```

| 타겟 | 설명 |
|------|------|
| `make` / `make all` | 모든 바이너리 빌드 (`binaries` 타겟) |
| `make binaries` | ctr, containerd, containerd-stress 빌드 |
| `make bin/containerd` | containerd 데몬만 빌드 |
| `make bin/ctr` | ctr CLI만 빌드 |
| `make bin/containerd-shim-runc-v2` | Shim v2만 빌드 |
| `make install` | 바이너리 설치 (`/usr/local/bin/`) |
| `make test` | 단위 테스트 실행 |
| `make integration` | 통합 테스트 실행 |
| `make check` | 린터 실행 (golangci-lint) |
| `make protos` | Protobuf 코드 생성 |
| `make generate` | 코드 생성 (protos + go generate) |
| `make release` | 릴리스 아카이브 생성 |
| `make static-release` | 정적 빌드 릴리스 |
| `make coverage` | 커버리지 리포트 생성 |
| `make ci` | CI 파이프라인 (check + binaries + check-protos + coverage) |
| `make vendor` | 벤더링 업데이트 |
| `make mandir` | man 페이지 생성 |

### 10.3 빌드 플래그

```
소스 참조: Makefile (Line 37~38, 100~111)
```

| 변수 | 기본값 | 설명 |
|------|--------|------|
| `GO` | `go` | Go 명령어 |
| `PREFIX` | `/usr/local` | 설치 경로 |
| `VERSION` | git describe | 버전 (예: v2.0.0) |
| `REVISION` | git rev-parse | 커밋 해시 |
| `PACKAGE` | `github.com/containerd/containerd/v2` | 패키지 경로 |
| `STATIC` | (미설정) | 설정 시 정적 바이너리 |
| `GODEBUG` | (미설정) | 설정 시 디버그 빌드 (-N -l) |
| `SHIM_CGO_ENABLED` | 0 | Shim CGO 사용 여부 |
| `GO_BUILDTAGS` | `urfave_cli_no_docs` | 빌드 태그 |

### 10.4 플랫폼별 빌드

```
Makefile.linux      → Linux 전용 타겟 (snapshotter, apparmor, seccomp 등)
Makefile.darwin     → macOS 전용 타겟
Makefile.windows    → Windows 전용 타겟 (runhcs, hcsshim)
Makefile.freebsd    → FreeBSD 전용 타겟
```

---

## 11. 주요 의존성

```
소스 참조: go.mod (Line 5~30)
```

| 의존성 | 용도 |
|--------|------|
| `github.com/containerd/ttrpc` | TTRPC 프로토콜 (Shim 통신) |
| `github.com/containerd/plugin` | 플러그인 프레임워크 |
| `github.com/containerd/go-runc` | runc 바인딩 |
| `github.com/containerd/go-cni` | CNI 네트워크 |
| `github.com/containerd/errdefs` | 에러 타입 정의 |
| `github.com/containerd/log` | 구조화 로깅 (logrus 래퍼) |
| `github.com/containerd/continuity` | 파일시스템 연속성 |
| `github.com/containerd/platforms` | 플랫폼 매칭 |
| `github.com/containerd/typeurl/v2` | protobuf Any 확장 |
| `github.com/containerd/cgroups/v3` | cgroup 관리 |
| `github.com/containerd/nri` | NRI (Node Resource Interface) |
| `go.etcd.io/bbolt` | BoltDB (메타데이터 저장) |
| `google.golang.org/grpc` | gRPC 프레임워크 |
| `google.golang.org/protobuf` | Protocol Buffers |
| `github.com/opencontainers/runtime-spec` | OCI Runtime Spec |
| `github.com/opencontainers/image-spec` | OCI Image Spec |
| `github.com/opencontainers/go-digest` | 콘텐츠 해시 |
| `github.com/urfave/cli/v2` | CLI 프레임워크 |
| `github.com/pelletier/go-toml/v2` | TOML 설정 파서 |
| `github.com/docker/go-metrics` | Prometheus 메트릭 |

---

## 12. 코드 네비게이션 가이드

### 12.1 "이 기능은 어디에 구현되어 있는가?"

| 찾고자 하는 것 | 인터페이스 | 구현 |
|---------------|-----------|------|
| 컨테이너 CRUD | `core/containers/containers.go` | `core/metadata/containers.go` |
| 이미지 CRUD | `core/images/image.go` | `core/metadata/images.go` |
| 콘텐츠 저장 | `core/content/content.go` | `plugins/content/local/` |
| 스냅샷 관리 | `core/snapshots/snapshotter.go` | `plugins/snapshots/` |
| 태스크 관리 | `core/runtime/runtime.go` | `core/runtime/v2/` |
| 샌드박스 관리 | `core/sandbox/controller.go` | `plugins/sandbox/` |
| 이벤트 시스템 | `core/events/events.go` | `core/events/exchange/` |
| GC | `pkg/gc/` | `plugins/gc/scheduler.go` + `core/metadata/gc.go` |
| CRI 서비스 | gRPC CRI | `internal/cri/` |
| gRPC 서비스 | `api/services/*/v1/*.proto` | `plugins/services/*/` |

### 12.2 "이 요청은 어떤 경로로 처리되는가?"

```
클라이언트 → gRPC 인터셉터 (네임스페이스 주입)
  → plugins/services/{service}/ (gRPC 서비스 핸들러)
    → core/{subsystem}/ (인터페이스 호출)
      → core/metadata/ (BoltDB 메타데이터) 또는
        plugins/{implementation}/ (실제 구현)
```

### 12.3 파일 명명 규칙

| 패턴 | 설명 |
|------|------|
| `*_linux.go` | Linux 전용 코드 |
| `*_unix.go` | Unix 공통 (Linux + macOS + FreeBSD) |
| `*_windows.go` | Windows 전용 코드 |
| `*_freebsd.go` | FreeBSD 전용 코드 |
| `*_test.go` | 테스트 코드 |
| `proxy/` | gRPC 프록시 클라이언트 |
| `testsuite/` | 인터페이스 적합성 테스트 |
