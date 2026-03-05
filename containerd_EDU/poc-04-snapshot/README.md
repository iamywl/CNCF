# PoC-04: Overlay 스냅샷 시뮬레이션

## 목적

containerd의 Overlay Snapshotter를 시뮬레이션한다. Snapshotter는 이미지 레이어를 CoW(Copy-on-Write) 파일시스템으로 관리하며, 컨테이너에 읽기/쓰기 가능한 루트 파일시스템을 제공한다.

1. **Snapshotter 인터페이스** — Prepare(rw), View(ro), Commit(seal), Remove, Mounts
2. **Kind 전이** — Active(rw) -> Committed(sealed), View(ro, commit 불가)
3. **Parent-Child CoW 레이어** — 이미지 레이어(Committed) 위에 컨테이너 레이어(Active)
4. **Overlay mount 구성** — lowerdir(parent 체인) + upperdir(현재) + workdir(작업용)
5. **이미지 레이어 언팩** — Prepare -> mount -> 압축해제 -> Commit 반복

## 핵심 개념

### Snapshot Kind 상태 전이

```
Prepare(key, parent) ──→ Active (rw)  ──→ Commit(name, key) ──→ Committed
View(key, parent)    ──→ View (ro)    ──→ (commit 불가, Remove만 가능)
```

- **Active**: 읽기/쓰기 가능. `Prepare()`로 생성. upperdir + workdir 보유.
- **Committed**: 불변. `Commit()`으로 전환. 다른 스냅샷의 parent가 될 수 있음.
- **View**: 읽기 전용. `View()`로 생성. Commit 불가, 검사 용도.

### Overlay 마운트 결정 로직

| 조건 | 마운트 타입 | 설명 |
|------|-----------|------|
| 부모 없음 + Active | bind (rw) | 단일 레이어, overlay 불필요 |
| 부모 없음 + View | bind (ro) | 단일 레이어, 읽기 전용 |
| 부모 있음 + Active | overlay | upperdir + workdir + lowerdir |
| 부모 1개 + View | bind (ro) | 부모의 fs를 직접 bind |
| 부모 다수 + View | overlay | lowerdir만 (upperdir/workdir 없음) |

### 이미지 레이어 언팩 흐름

```
Layer 0: Prepare("extract-0", "") → mount → unpack → Commit(chainID-0, "extract-0")
Layer 1: Prepare("extract-1", chainID-0) → mount → unpack → Commit(chainID-1, "extract-1")
Layer 2: Prepare("extract-2", chainID-1) → mount → unpack → Commit(chainID-2, "extract-2")

컨테이너: Prepare("container-key", chainID-2) → overlay mount 반환
```

### 파일시스템 구조

```
root/
└── snapshots/
    ├── 1/              ← Layer 0 (Committed)
    │   └── fs/         ← 이 레이어의 파일들
    ├── 2/              ← Layer 1 (Committed)
    │   └── fs/
    ├── 3/              ← Layer 2 (Committed)
    │   └── fs/
    └── 4/              ← Container (Active)
        ├── fs/         ← upperdir (변경사항, CoW)
        └── work/       ← overlay workdir (커널 사용)
```

## 실제 소스 참조

| PoC 구현 | 실제 소스 경로 | 설명 |
|----------|---------------|------|
| `Kind` 상수 | `core/snapshots/snapshotter.go:44` | KindUnknown(0), KindView(1), KindActive(2), KindCommitted(3) |
| `Info` 구조체 | `core/snapshots/snapshotter.go:102` | Kind, Name, Parent, Labels, Created, Updated |
| `Snapshotter` 인터페이스 | `core/snapshots/snapshotter.go:255` | Prepare, View, Commit, Remove, Mounts, Walk, Stat |
| `Mount` 구조체 | `core/mount/mount.go:34` | Type, Source, Target, Options |
| `snapshotter` 구조체 | `plugins/snapshots/overlay/overlay.go:108` | root, ms, asyncRemove, upperdirLabel, options |
| `NewSnapshotter()` | `plugins/snapshots/overlay/overlay.go:121` | root 생성, DType 확인, MetaStore 생성, snapshots/ 디렉토리 |
| `Prepare()` | `plugins/snapshots/overlay/overlay.go:265` | `createSnapshot(KindActive, key, parent, opts)` |
| `View()` | `plugins/snapshots/overlay/overlay.go:269` | `createSnapshot(KindView, key, parent, opts)` |
| `createSnapshot()` | `plugins/snapshots/overlay/overlay.go:428` | temp dir -> fs/ + work/ -> storage.CreateSnapshot -> Rename |
| `prepareDirectory()` | `plugins/snapshots/overlay/overlay.go:533` | MkdirTemp -> Mkdir(fs/) + Active이면 Mkdir(work/) |
| `Commit()` | `plugins/snapshots/overlay/overlay.go:297` | GetInfo -> DiskUsage -> CommitActive(key, name, usage) |
| `mounts()` | `plugins/snapshots/overlay/overlay.go:552` | ParentIDs 수에 따라 bind/overlay 마운트 결정 |
| `upperPath()` | `plugins/snapshots/overlay/overlay.go:617` | `root/snapshots/{id}/fs` |
| `workPath()` | `plugins/snapshots/overlay/overlay.go:621` | `root/snapshots/{id}/work` |
| `Remove()` | `plugins/snapshots/overlay/overlay.go:320` | storage.Remove + os.RemoveAll |
| `Walk()` | `plugins/snapshots/overlay/overlay.go:351` | storage.WalkInfo |

## 실행 방법

```bash
cd containerd_EDU/poc-04-snapshot
go run main.go
```

## 예상 출력

```
======================================================================
containerd Overlay Snapshot 시뮬레이션
======================================================================

[1] Snapshotter 인터페이스 개요
  Snapshotter: Prepare, View, Commit, Remove, Mounts, Walk ...

[2] 이미지 레이어 언팩 시뮬레이션
  Layer 0: sha256:aaaaaa11111111...
    Prepare: OK (mount=bind [rw,rbind])
    Unpack:  OK
    Commit:  OK
  Layer 1: sha256:bbbbbb22222222...
    Prepare: OK (mount=overlay [...])
    ...

[3] 컨테이너 스냅샷 생성 (Prepare)
  Type:    overlay
  Options: workdir=.../4/work
           upperdir=.../4/fs
           lowerdir=.../3/fs:.../2/fs:.../1/fs

[4] View 스냅샷 (읽기 전용)
  → View는 upperdir/workdir 없이 lowerdir만

[5] 컨테이너 파일 쓰기 (CoW 시뮬레이션)
  container-created.txt, modified-index.html

[6] 컨테이너 스냅샷을 새 이미지로 Commit
  Active → Committed 전환

[9] 마운트 타입 비교: bind vs overlay
  단일 레이어 → bind mount
  다중 레이어 → overlay mount

[10] 실제 파일시스템 구조
  snapshots/1/ → [fs/]
  snapshots/2/ → [fs/]
  ...
```
