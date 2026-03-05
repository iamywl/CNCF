# containerd 교육 자료 (EDU)

## 프로젝트 개요

**containerd**는 산업 표준 컨테이너 런타임으로, 단순성, 견고성, 이식성에 중점을 둔 CNCF Graduated 프로젝트이다.
Linux와 Windows에서 데몬으로 동작하며, 호스트 시스템의 전체 컨테이너 라이프사이클을 관리한다.

containerd는 Docker, Kubernetes(CRI), AWS ECS, Google GKE 등 상위 시스템에 임베디드되어 사용되도록 설계되었으며,
직접 사용하기보다는 상위 플랫폼의 컨테이너 런타임 백엔드로 동작한다.

```
+----------------------------------------------------------+
|  상위 시스템 (Docker, Kubernetes, BuildKit, ...)          |
+----------------------------------------------------------+
       |              |              |              |
       v              v              v              v
+----------------------------------------------------------+
|                    containerd 데몬                        |
|  +--------+ +--------+ +----------+ +--------+ +------+  |
|  | Image  | |Content | |Snapshot  | |Runtime | |Event |  |
|  | Store  | | Store  | |  Store   | | (Shim) | |  Bus |  |
|  +--------+ +--------+ +----------+ +--------+ +------+  |
+----------------------------------------------------------+
       |                      |                |
       v                      v                v
+-------------+    +------------------+   +---------+
|  Registry   |    | 파일시스템 (CoW) |   |  runc   |
| (OCI Dist.) |    | overlay/btrfs    |   | (OCI)   |
+-------------+    +------------------+   +---------+
```

### 핵심 기능

| 기능 | 설명 |
|------|------|
| **이미지 전송/저장** | OCI 호환 레지스트리에서 이미지를 Pull/Push, Content Store에 digest 기반 저장 |
| **컨테이너 실행/감시** | Shim v2를 통한 컨테이너 생성/시작/종료, 프로세스 수명 관리 |
| **저수준 저장소** | Snapshotter를 통한 CoW 파일시스템 레이어 관리 (overlay, btrfs, zfs 등) |
| **네트워크 연결** | CNI를 통한 컨테이너 네트워크 설정 (CRI 플러그인에서 활용) |
| **플러그인 아키텍처** | 30여 종의 플러그인 타입, DFS 의존성 해석을 통한 자동 초기화 |
| **CRI 지원** | Kubernetes Container Runtime Interface 네이티브 구현 |
| **네임스페이스 격리** | 단일 데몬에서 여러 클라이언트(Docker, K8s 등)의 리소스를 격리 |
| **가비지 컬렉션** | Tricolor Mark-and-Sweep GC, Lease 기반 리소스 수명 관리 |

### 소스코드 정보

| 항목 | 값 |
|------|-----|
| 소스 경로 | `containerd/` |
| 언어 | Go |
| 모듈 | `github.com/containerd/containerd/v2` |
| 라이선스 | Apache 2.0 |
| CNCF 상태 | Graduated |

---

## 문서 목차

### 기본 문서 (01~06)

| 번호 | 문서 | 내용 |
|------|------|------|
| 01 | [01-architecture.md](./01-architecture.md) | 전체 아키텍처, gRPC 클라이언트-서버, 플러그인 시스템, Shim v2, 초기화 흐름 |
| 02 | [02-data-model.md](./02-data-model.md) | Container, Image, Content, Snapshot, Runtime, Sandbox, Lease 데이터 모델 |
| 03 | [03-sequence-diagrams.md](./03-sequence-diagrams.md) | 컨테이너 생성, 이미지 Pull, Task 실행, CRI, GC 시퀀스 다이어그램 |
| 04 | [04-code-structure.md](./04-code-structure.md) | 디렉토리 구조, 빌드 시스템, 바이너리, Proto 파일 구조 |
| 05 | [05-core-components.md](./05-core-components.md) | Plugin Registry, Content Store, Snapshotter, Runtime/Shim, Metadata DB, Event, GC |
| 06 | [06-operations.md](./06-operations.md) | 설치, 설정, CRI 연동, 모니터링, 트러블슈팅, 마이그레이션 |

### 심화 문서 (07~20)

| 번호 | 문서 | 내용 |
|------|------|------|
| 07 | [07-plugin-system.md](./07-plugin-system.md) | 플러그인 등록/초기화/DFS 그래프, Registration, InitContext |
| 08 | [08-content-store.md](./08-content-store.md) | Content Store CAS 구현, digest 기반 저장, 무결성 검증, Writer |
| 09 | [09-snapshot-system.md](./09-snapshot-system.md) | Snapshotter 인터페이스, overlay/btrfs 구현, CoW, Kind 상태 |
| 10 | [10-runtime-shim.md](./10-runtime-shim.md) | Shim v2 아키텍처, TTRPC, ShimManager, 프로세스 관리 |
| 11 | [11-image-management.md](./11-image-management.md) | OCI Image Spec, Handler/Walk 패턴, Unpack, GC 라벨 |
| 12 | [12-task-execution.md](./12-task-execution.md) | Task/Process 인터페이스, 2-Layer 서비스, shimTask Proxy, Exec |
| 13 | [13-cri-plugin.md](./13-cri-plugin.md) | CRI gRPC 플러그인, criService, RunPodSandbox, EventMonitor |
| 14 | [14-metadata-store.md](./14-metadata-store.md) | BoltDB 아키텍처, 버킷 계층, 네임스페이스 격리, 트랜잭션, 마이그레이션 |
| 15 | [15-events-system.md](./15-events-system.md) | Envelope, Publisher/Subscriber, Exchange/Broadcaster, backoff retry |
| 16 | [16-gc-system.md](./16-gc-system.md) | Tricolor Mark-and-Sweep, 리소스 타입, 라벨 기반 참조, GC 스케줄러 |
| 17 | [17-transfer-service.md](./17-transfer-service.md) | Transferrer 인터페이스, Pull/Push, Import/Export, ProgressTracker |
| 18 | [18-sandbox-system.md](./18-sandbox-system.md) | Sandbox Controller/Store, gRPC/ttrpc Bridge, CRI PodSandbox |
| 19 | [19-lease-system.md](./19-lease-system.md) | Lease 기반 리소스 수명 관리, GC 보호, Context 전파, Flat Lease |
| 20 | [20-namespace-isolation.md](./20-namespace-isolation.md) | Context 기반 네임스페이스, BoltDB 스코핑, 콘텐츠 공유 정책 |

### PoC 코드 (poc-01~16)

| 번호 | PoC | 핵심 개념 |
|------|-----|----------|
| 01 | [poc-01-architecture](./poc-01-architecture/) | 서버 초기화, 플러그인 DFS 정렬, gRPC/TTRPC/Metrics 리스너 |
| 02 | [poc-02-data-model](./poc-02-data-model/) | Descriptor, Image, Container, Task 상태머신 |
| 03 | [poc-03-content-store](./poc-03-content-store/) | CAS 쓰기/읽기, 중복제거, digest/크기 검증 |
| 04 | [poc-04-snapshot](./poc-04-snapshot/) | Overlay 레이어 언팩, Prepare/Commit/View, CoW |
| 05 | [poc-05-plugin-system](./poc-05-plugin-system/) | 플러그인 의존성 그래프, DFS 정렬, DisableFilter |
| 06 | [poc-06-runtime-shim](./poc-06-runtime-shim/) | Shim 프로세스 생명주기, TTRPC TaskService |
| 07 | [poc-07-task-execution](./poc-07-task-execution/) | Task/Process 생명주기, Exec, Wait, 다중 Task |
| 08 | [poc-08-cri](./poc-08-cri/) | RunPodSandbox, Create/Start/Stop/RemoveContainer |
| 09 | [poc-09-metadata](./poc-09-metadata/) | BoltDB 스키마, 네임스페이스 격리, GC wlock |
| 10 | [poc-10-events](./poc-10-events/) | Publisher/Subscriber, 토픽 필터링, Forward |
| 11 | [poc-11-gc](./poc-11-gc/) | Tricolor Mark-Sweep, gc.expire, gc.root |
| 12 | [poc-12-lease](./poc-12-lease/) | Lease CRUD, GC 보호, 만료, Flat Lease |
| 13 | [poc-13-transfer](./poc-13-transfer/) | Transfer Service Pull, 중복 스킵, 타입 매칭 |
| 14 | [poc-14-sandbox](./poc-14-sandbox/) | Sandbox Controller, Create/Start/Stop/Shutdown |
| 15 | [poc-15-namespace](./poc-15-namespace/) | 네임스페이스 격리, BoltDB 스키마, 삭제 보호 |
| 16 | [poc-16-image-unpack](./poc-16-image-unpack/) | Image Index 파싱, ChainID 계산, 레이어 언팩 |

---

## 실행 방법

### PoC 실행 (각 poc 디렉토리에서)

```bash
cd containerd_EDU/poc-XX-<name>/
go run main.go
```

모든 PoC는 Go 표준 라이브러리만 사용하므로 별도 의존성 설치가 필요 없다.

### containerd 소스 빌드

```bash
cd containerd/
make                    # containerd, ctr, shim 등 전체 빌드
make bin/containerd     # containerd 데몬만 빌드
make bin/ctr            # ctr CLI만 빌드
```

---

## 참고 자료

- [containerd 공식 사이트](https://containerd.io/)
- [containerd GitHub](https://github.com/containerd/containerd)
- [containerd v2.0 문서](https://github.com/containerd/containerd/blob/main/docs/containerd-2.0.md)
- [CRI Plugin 가이드](https://github.com/containerd/containerd/blob/main/docs/cri/config.md)
- [OCI Runtime Spec](https://github.com/opencontainers/runtime-spec)
- [OCI Image Spec](https://github.com/opencontainers/image-spec)
