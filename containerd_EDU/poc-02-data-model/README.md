# PoC-02: 핵심 데이터 구조

## 목적

containerd의 5대 핵심 데이터 모델을 시뮬레이션한다:

1. **Content Descriptor** — OCI 이미지 콘텐츠 참조 단위 (MediaType, Digest, Size)
2. **Image** — 이미지 메타데이터 (Name + Target Descriptor)
3. **Container** — 실행 템플릿 (ID, Image, Runtime, Spec, SnapshotKey, Snapshotter, SandboxID)
4. **Task** — 실행 상태 머신 (Created -> Running -> Paused -> Stopped -> Deleted)
5. **Snapshot** — CoW 파일시스템 레이어 (Kind: Active/Committed/View, Parent-Child 체인)

## 핵심 개념

### Container vs Task 분리

containerd의 가장 중요한 설계 결정 중 하나는 Container와 Task의 분리이다:

- **Container**: "어떻게 실행할 것인가"를 정의하는 메타데이터 (이미지, 런타임, OCI 스펙, 스냅샷)
- **Task**: "실제 실행 중인 프로세스"를 나타내는 런타임 상태 (PID, 상태 전이, exec 프로세스)

이 분리를 통해:
- Container 메타데이터는 Task 없이도 존재 가능 (생성만 하고 실행하지 않는 경우)
- 하나의 Container에서 여러 번 Task를 생성/삭제 가능
- CRI(Kubernetes)의 CreateContainer/StartContainer 분리에 자연스럽게 매핑

### Snapshot Kind 상태 전이

```
Prepare(key, parent) → Active (writable)
Commit(name, key)    → Committed (immutable, 새 parent 가능)
View(key, parent)    → View (readonly, commit 불가)
```

### Content Descriptor 체인

```
Image.Target (Manifest Descriptor)
  └→ Manifest.Config (Config Descriptor)
  └→ Manifest.Layers[] (Layer Descriptors)
       └→ Content Store에서 digest로 조회
```

## 실제 소스 참조

| PoC 구현 | 실제 소스 경로 | 핵심 필드/인터페이스 |
|----------|---------------|---------------------|
| `Descriptor` | OCI image-spec `specs-go/v1` | MediaType, Digest, Size, Platform |
| `Image` | `core/images/image.go:35` | Name, Labels, Target(Descriptor) |
| `Container` | `core/containers/containers.go:30` | ID, Image, Runtime, Spec, SnapshotKey, Snapshotter, SandboxID |
| `RuntimeInfo` | `core/containers/containers.go:87` | Name, Options |
| `Task` 인터페이스 | `core/runtime/task.go:63` | Process + PID, Namespace, Pause, Resume, Exec |
| `Process` 인터페이스 | `core/runtime/task.go:35` | ID, State, Kill, Start, Wait |
| `TaskStatus` | `core/runtime/task.go:101` | Created(1), Running(2), Stopped(3), Deleted(4), Paused(5), Pausing(6) |
| `State` | `core/runtime/task.go:119` | Status, Pid, ExitStatus, ExitedAt |
| `ContentInfo` | `core/content/content.go:90` | Digest, Size, CreatedAt, UpdatedAt, Labels |
| `ContentStatus` | `core/content/content.go:99` | Ref, Offset, Total, Expected, StartedAt |
| `Writer` 인터페이스 | `core/content/content.go:146` | WriteCloser + Digest, Commit, Status, Truncate |
| `SnapshotKind` | `core/snapshots/snapshotter.go:44` | KindUnknown(0), KindView(1), KindActive(2), KindCommitted(3) |
| `SnapshotInfo` | `core/snapshots/snapshotter.go:102` | Kind, Name, Parent, Labels |
| `Snapshotter` 인터페이스 | `core/snapshots/snapshotter.go:255` | Prepare, View, Commit, Remove, Mounts, Walk |
| `Mount` | `core/mount/mount.go:34` | Type, Source, Target, Options |
| `PlatformRuntime` | `core/runtime/runtime.go:70` | ID, Create, Get, Tasks, Delete |

## 실행 방법

```bash
cd containerd_EDU/poc-02-data-model
go run main.go
```

## 예상 출력

```
======================================================================
containerd 핵심 데이터 구조 시뮬레이션
======================================================================

[1] Content Descriptor (OCI Image Spec)
  이미지 콘텐츠 그래프 (Image → Manifest → Config + Layers)
  ┌─────────────────────────────────────────────┐
  │ Image Target (Manifest)                     │
  │   mediaType: application/vnd.oci.image...   │
  │   digest:    sha256:...                     │
  ...

[2] Image (core/images/image.go)
  Image: { "name": "docker.io/library/nginx:1.25", ... }

[3] Container (core/containers/containers.go)
  Container: { "id": "nginx-web-001", ... }

[4] Task 상태 머신 (core/runtime/task.go)
  → Created (pid=12345)
  → Running
  → Pausing → Paused
  → Running (resumed)
  → Stopped (exit=0)
  → Deleted

[5] Content Info
  blob 목록 테이블 ...

[6] Snapshot Info
  스냅샷 레이어 스택 ...

[7] 전체 데이터 흐름
  이미지 Pull → 컨테이너 실행 전체 흐름 ...
```
