# 09. containerd Snapshot 시스템 Deep-Dive

## 목차

1. [개요](#1-개요)
2. [Snapshotter가 필요한 이유](#2-snapshotter가-필요한-이유)
3. [Snapshotter 인터페이스 상세](#3-snapshotter-인터페이스-상세)
4. [Kind: 스냅샷의 세 가지 종류](#4-kind-스냅샷의-세-가지-종류)
5. [Info와 Usage: 스냅샷 메타데이터](#5-info와-usage-스냅샷-메타데이터)
6. [Mount 구조체: 마운트의 통화(Lingua Franca)](#6-mount-구조체-마운트의-통화lingua-franca)
7. [Parent-Child CoW 관계](#7-parent-child-cow-관계)
8. [Overlay Snapshotter 구현](#8-overlay-snapshotter-구현)
9. [createSnapshot: 스냅샷 생성 흐름](#9-createsnapshot-스냅샷-생성-흐름)
10. [mounts(): 마운트 옵션 생성 로직](#10-mounts-마운트-옵션-생성-로직)
11. [Commit과 Remove](#11-commit과-remove)
12. [AsyncRemove 패턴](#12-asyncremove-패턴)
13. [이미지 레이어 언팩과 Snapshot 관계](#13-이미지-레이어-언팩과-snapshot-관계)
14. [Cleaner 인터페이스와 Cleanup](#14-cleaner-인터페이스와-cleanup)
15. [ID 매핑(User Namespace) 지원](#15-id-매핑user-namespace-지원)
16. [설계 철학과 Native 비교](#16-설계-철학과-native-비교)

---

## 1. 개요

Snapshot 시스템은 containerd에서 **컨테이너 파일시스템을 관리**하는 핵심 서브시스템이다. 이미지의 각 레이어를 Copy-on-Write(CoW) 방식으로 적층하고, 컨테이너의 쓰기 가능한 최상위 레이어를 제공한다.

```
소스 위치:
  core/snapshots/snapshotter.go                  -- Snapshotter 인터페이스
  core/mount/mount.go                            -- Mount 구조체
  plugins/snapshots/overlay/overlay.go           -- overlay 구현체
  plugins/snapshots/overlay/plugin/plugin.go     -- 플러그인 등록
```

---

## 2. Snapshotter가 필요한 이유

### 컨테이너 파일시스템의 문제

컨테이너 이미지는 여러 레이어로 구성된다:

```
이미지 "nginx:latest"
  Layer 4: nginx 설정 파일 (+5MB)
  Layer 3: nginx 바이너리 (+20MB)
  Layer 2: apt 패키지들 (+100MB)
  Layer 1: Ubuntu base (+70MB)
  ─────────────────────────────
  총: 195MB
```

같은 base 이미지를 사용하는 컨테이너가 100개 있다면?
- **복사 방식**: 195MB x 100 = 19.5GB
- **CoW 방식**: 195MB (공유) + 컨테이너별 차이분만

### Snapshotter의 역할

```
Content Store (블롭)         Snapshotter (파일시스템)
  |                              |
  | Layer tar.gz (sha256:aaa)    | Committed: "layer-1"
  | Layer tar.gz (sha256:bbb)    | Committed: "layer-2" (parent: layer-1)
  |                              |
  |                              | Active: "container-1" (parent: layer-2)
  |                              | Active: "container-2" (parent: layer-2)
  |                              |
  | (압축된 아카이브)             | (마운트 가능한 파일시스템)
```

Content Store는 블롭(tar.gz)을 저장하고, Snapshotter는 이를 **마운트 가능한 파일시스템**으로 관리한다.

---

## 3. Snapshotter 인터페이스 상세

```go
// 소스: core/snapshots/snapshotter.go:255-351

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

### 메서드별 역할

| 메서드 | 입력 | 출력 | 설명 |
|--------|------|------|------|
| Stat | key | Info | 스냅샷 정보 조회 |
| Update | Info, fieldpaths | Info | 라벨 등 가변 정보 업데이트 |
| Usage | key | Usage | 디스크 사용량 조회 |
| Mounts | key(Active) | []Mount | Active 스냅샷의 마운트 정보 |
| Prepare | key, parent | []Mount | 읽기/쓰기 Active 스냅샷 생성 |
| View | key, parent | []Mount | 읽기 전용 Active 스냅샷 생성 |
| Commit | name, key(Active) | - | Active를 Committed로 변환 |
| Remove | key | - | 스냅샷 삭제 |
| Walk | fn, filters | - | 전체 스냅샷 순회 |
| Close | - | - | 리소스 정리 |

### Prepare vs View

```
Prepare("container-rw", "image-layer-3")
  → 결과: 읽기/쓰기 가능한 마운트
  → 용도: 컨테이너 rootfs, 레이어 언팩

View("container-ro", "image-layer-3")
  → 결과: 읽기 전용 마운트
  → 용도: 이미지 검사, diff 계산
```

두 메서드 모두 Active 스냅샷을 생성하지만:
- **Prepare**: upperdir + workdir 생성 → overlay rw 마운트 반환
- **View**: upperdir만 생성 → bind 또는 overlay ro 마운트 반환

---

## 4. Kind: 스냅샷의 세 가지 종류

```go
// 소스: core/snapshots/snapshotter.go:44-52

type Kind uint8

const (
    KindUnknown   Kind = iota
    KindView                    // 읽기 전용 임시
    KindActive                  // 읽기/쓰기 임시
    KindCommitted               // 읽기 전용 영구
)
```

### 상태 전이 다이어그램

```
                 Prepare(key, parent)
                      |
                      v
  View(key, parent)  [Active]
       |              |
       v              | Commit(name, key)
    [View]            v
       |         [Committed]
       |              |
       |              | (다른 스냅샷의 parent로 사용)
       |              v
       |         [Committed] ←── parent ── [Active] ...
       |
   Remove()      Remove()
       |              |
       v              v
   (삭제됨)       (삭제됨, 자식이 없어야 함)
```

### 각 Kind의 특성

| 속성 | Active | View | Committed |
|------|:------:|:----:|:---------:|
| 읽기 가능 | O | O | O |
| 쓰기 가능 | O | X | X |
| 마운트 가능 | O | O | 직접 불가 |
| parent 역할 | X | X | O |
| Commit 가능 | O | X | - |
| key 용어 | key | key | name |
| 생성 방법 | Prepare | View | Commit |

### Key vs Name

Snapshotter 인터페이스에서:
- **key**: Active/View 스냅샷을 식별하는 임시 식별자
- **name**: Committed 스냅샷을 식별하는 영구 식별자
- **parent**: 부모 Committed 스냅샷의 name

모든 식별자는 **같은 네임스페이스**를 공유한다. Active와 Committed가 같은 이름을 가질 수 없다.

---

## 5. Info와 Usage: 스냅샷 메타데이터

### Info 구조체

```go
// 소스: core/snapshots/snapshotter.go:103-115

type Info struct {
    Kind    Kind              // Active, View, 또는 Committed
    Name    string            // 스냅샷 이름/키
    Parent  string            // 부모 Committed 스냅샷 이름
    Labels  map[string]string // 가변 라벨
    Created time.Time
    Updated time.Time
}
```

### 상속되는 라벨

```go
// 소스: core/snapshots/snapshotter.go:34-35

const (
    inheritedLabelsPrefix = "containerd.io/snapshot/"
    labelSnapshotRef      = "containerd.io/snapshot.ref"
)
```

`containerd.io/snapshot/` 접두어가 붙은 라벨은 Prepare/View/Commit 시 **자동 상속**된다. 이는 UID/GID 매핑 등 스냅샷 체인 전체에 적용되어야 하는 설정에 사용된다.

```go
// 소스: core/snapshots/snapshotter.go:37-41

const (
    LabelSnapshotUIDMapping = "containerd.io/snapshot/uidmapping"
    LabelSnapshotGIDMapping = "containerd.io/snapshot/gidmapping"
)
```

### Usage 구조체

```go
// 소스: core/snapshots/snapshotter.go:121-124

type Usage struct {
    Inodes int64  // 사용 중인 inode 수
    Size   int64  // 바이트 단위 디스크 사용량
}
```

Usage는 해당 스냅샷 **자체**의 리소스만 포함하며, 부모의 사용량은 포함하지 않는다.

---

## 6. Mount 구조체: 마운트의 통화(Lingua Franca)

```go
// 소스: core/mount/mount.go:34-46

type Mount struct {
    Type    string    // "overlay", "bind" 등
    Source  string    // 마운트 소스 경로 또는 디바이스
    Target  string    // 마운트 포인트 (서브디렉토리)
    Options []string  // fstab 스타일 옵션
}
```

Mount 구조체는 containerd의 "공용어(lingua franca)"라고 불린다. 소스 코드 주석에서도 이를 명시한다. Snapshotter, Runtime, Differ 등 모든 컴포넌트가 Mount를 통해 파일시스템을 주고받는다.

### Mount.All과 UnmountMounts

```go
// 소스: core/mount/mount.go:50-57

func All(mounts []Mount, target string) error {
    for _, m := range mounts {
        if err := m.Mount(target); err != nil {
            return err
        }
    }
    return nil
}

// 소스: core/mount/mount.go:61-75
func UnmountMounts(mounts []Mount, target string, flags int) error {
    for i := len(mounts) - 1; i >= 0; i-- {
        // 역순으로 언마운트
    }
    return nil
}
```

### readonlyOverlay: 읽기 전용 변환

```go
// 소스: core/mount/mount.go:134-152

func readonlyOverlay(opt []string) []string {
    out := make([]string, 0, len(opt))
    upper := ""
    for _, o := range opt {
        if strings.HasPrefix(o, "upperdir=") {
            upper = strings.TrimPrefix(o, "upperdir=")
        } else if !isSkippedReadonlyOption(o) {
            out = append(out, o)
        }
    }
    if upper != "" {
        for i, o := range out {
            if strings.HasPrefix(o, "lowerdir=") {
                out[i] = "lowerdir=" + upper + ":" + strings.TrimPrefix(o, "lowerdir=")
            }
        }
    }
    return out
}
```

overlay 마운트를 읽기 전용으로 변환할 때, upperdir를 lowerdir 앞에 추가하고 workdir를 제거한다. 이렇게 하면 upperdir의 변경 사항도 보이면서 더 이상 쓸 수 없는 상태가 된다.

---

## 7. Parent-Child CoW 관계

### Copy-on-Write 스택

```
컨테이너 rootfs (overlay mount):

  +-----------------------------------+
  | upperdir (container's writable)   |  Active 스냅샷
  +-----------------------------------+
  | lowerdir[0] (layer 3)             |  Committed 스냅샷
  +-----------------------------------+
  | lowerdir[1] (layer 2)             |  Committed 스냅샷
  +-----------------------------------+
  | lowerdir[2] (layer 1)             |  Committed 스냅샷
  +-----------------------------------+

  읽기: upperdir → lowerdir[0] → lowerdir[1] → lowerdir[2] 순서로 검색
  쓰기: upperdir에만 기록 (Copy-on-Write)
```

### Parent 체인

```
"layer-1" (Committed, parent: "")
    |
    +-- "layer-2" (Committed, parent: "layer-1")
         |
         +-- "layer-3" (Committed, parent: "layer-2")
              |
              +-- "container-A" (Active, parent: "layer-3")
              +-- "container-B" (Active, parent: "layer-3")
```

Committed 스냅샷은 **여러 Active 스냅샷의 부모**가 될 수 있다. 이것이 CoW의 핵심이다. 100개의 컨테이너가 같은 이미지를 사용해도 레이어는 한 벌만 디스크에 존재한다.

---

## 8. Overlay Snapshotter 구현

### snapshotter 구조체

```go
// 소스: plugins/snapshots/overlay/overlay.go:108-116

type snapshotter struct {
    root          string        // 루트 디렉토리
    ms            MetaStore     // 메타데이터 DB (BoltDB)
    asyncRemove   bool          // 비동기 삭제 활성화
    upperdirLabel bool          // upperdir 라벨 노출
    options       []string      // 마운트 옵션 (userxattr 등)
    remapIDs      bool          // ID 매핑 지원
    slowChown     bool          // 느린 chown 허용
}
```

### 파일시스템 레이아웃

```
{root}/
  |
  +-- metadata.db          (BoltDB: 스냅샷 메타데이터)
  |
  +-- snapshots/
        |
        +-- 1/              (스냅샷 ID = 1)
        |   +-- fs/          (upperdir / 마운트 소스)
        |   +-- work/        (overlayfs workdir, Active만)
        |
        +-- 2/
        |   +-- fs/
        |   +-- work/
        |
        +-- 3/
            +-- fs/
```

### NewSnapshotter: 초기화 흐름

```go
// 소스: plugins/snapshots/overlay/overlay.go:121-174

func NewSnapshotter(root string, opts ...Opt) (snapshots.Snapshotter, error) {
    // 1. 옵션 적용
    var config SnapshotterConfig
    for _, opt := range opts {
        opt(&config)
    }

    // 2. 디렉토리 생성 및 d_type 지원 확인
    os.MkdirAll(root, 0700)
    supportsDType, _ := fs.SupportsDType(root)
    if !supportsDType {
        return nil, fmt.Errorf("%s does not support d_type", root)
    }

    // 3. 메타스토어 초기화 (BoltDB)
    if config.ms == nil {
        config.ms, _ = storage.NewMetaStore(filepath.Join(root, "metadata.db"))
    }

    // 4. snapshots 디렉토리 생성
    os.Mkdir(filepath.Join(root, "snapshots"), 0700)

    // 5. userxattr 필요 여부 감지
    if userxattr, _ := overlayutils.NeedsUserXAttr(root); userxattr {
        config.mountOptions = append(config.mountOptions, "userxattr")
    }

    // 6. index=off 지원 시 추가
    if supportsIndex() {
        config.mountOptions = append(config.mountOptions, "index=off")
    }

    return &snapshotter{...}, nil
}
```

**d_type 확인**: overlayfs는 파일시스템이 d_type(directory entry type)을 지원해야 한다. XFS의 경우 `ftype=1`로 포맷해야 한다. 이를 확인하지 않으면 런타임에 파일 삭제 실패 등 미묘한 버그가 발생한다.

---

## 9. createSnapshot: 스냅샷 생성 흐름

```go
// 소스: plugins/snapshots/overlay/overlay.go:428-531

func (o *snapshotter) createSnapshot(ctx context.Context, kind snapshots.Kind, key, parent string, opts []snapshots.Opt) (_ []mount.Mount, err error) {
    var (
        s        storage.Snapshot
        td, path string
        info     snapshots.Info
    )

    // 실패 시 정리
    defer func() {
        if err != nil {
            if td != "" { os.RemoveAll(td) }
            if path != "" { os.RemoveAll(path) }
        }
    }()

    err = o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        // 1. 임시 디렉토리 생성
        snapshotDir := filepath.Join(o.root, "snapshots")
        td, _ = o.prepareDirectory(ctx, snapshotDir, kind)

        // 2. 메타데이터에 스냅샷 레코드 생성
        s, _ = storage.CreateSnapshot(ctx, kind, key, parent, opts...)

        // 3. 스냅샷 정보 조회
        _, info, _, _ = storage.GetInfo(ctx, key)

        // 4. UID/GID 매핑 처리
        // ...

        // 5. 임시 → 최종 경로 이동 (원자적)
        path = filepath.Join(snapshotDir, s.ID)
        os.Rename(td, path)
        td = ""

        return nil
    })

    return o.mounts(s, info), nil
}
```

### prepareDirectory: 디렉토리 구조 생성

```go
// 소스: plugins/snapshots/overlay/overlay.go:533-550

func (o *snapshotter) prepareDirectory(ctx context.Context, snapshotDir string, kind snapshots.Kind) (string, error) {
    td, _ := os.MkdirTemp(snapshotDir, "new-")

    // fs/ 디렉토리 생성 (모든 종류)
    os.Mkdir(filepath.Join(td, "fs"), 0755)

    // work/ 디렉토리 생성 (Active만)
    if kind == snapshots.KindActive {
        os.Mkdir(filepath.Join(td, "work"), 0711)
    }

    return td, nil
}
```

**왜 임시 디렉토리를 먼저 만드는가:**
`os.MkdirTemp`로 임시 디렉토리를 생성한 후 `os.Rename`으로 최종 위치로 이동한다. 이는 디렉토리 생성 실패 시 불완전한 스냅샷이 남지 않도록 하기 위한 원자적 패턴이다.

---

## 10. mounts(): 마운트 옵션 생성 로직

```go
// 소스: plugins/snapshots/overlay/overlay.go:552-615

func (o *snapshotter) mounts(s storage.Snapshot, info snapshots.Info) []mount.Mount {
    var options []string

    // 1. ID 매핑 옵션 (지원 시)
    if o.remapIDs {
        if v, ok := info.Labels[snapshots.LabelSnapshotUIDMapping]; ok {
            options = append(options, fmt.Sprintf("uidmap=%s", v))
        }
    }

    // 2. 부모 없음 → bind 마운트
    if len(s.ParentIDs) == 0 {
        roFlag := "rw"
        if s.Kind == snapshots.KindView {
            roFlag = "ro"
        }
        return []mount.Mount{{
            Source:  o.upperPath(s.ID),
            Type:    "bind",
            Options: append(options, roFlag, "rbind"),
        }}
    }

    // 3. Active → overlay (upperdir + workdir + lowerdir)
    if s.Kind == snapshots.KindActive {
        options = append(options,
            fmt.Sprintf("workdir=%s", o.workPath(s.ID)),
            fmt.Sprintf("upperdir=%s", o.upperPath(s.ID)),
        )
    } else if len(s.ParentIDs) == 1 {
        // 4. View + 부모 1개 → 부모를 직접 bind 마운트
        return []mount.Mount{{
            Source:  o.upperPath(s.ParentIDs[0]),
            Type:    "bind",
            Options: append(options, "ro", "rbind"),
        }}
    }

    // 5. lowerdir 구성 (부모 체인)
    parentPaths := make([]string, len(s.ParentIDs))
    for i := range s.ParentIDs {
        parentPaths[i] = o.upperPath(s.ParentIDs[i])
    }
    options = append(options, fmt.Sprintf("lowerdir=%s", strings.Join(parentPaths, ":")))
    options = append(options, o.options...)  // userxattr, index=off 등

    return []mount.Mount{{
        Type:    "overlay",
        Source:  "overlay",
        Options: options,
    }}
}
```

### 마운트 유형 결정 로직

```
ParentIDs 수 = 0?
  |
  Yes → bind mount (rw 또는 ro)
  |
  No → Kind = Active?
         |
         Yes → overlay mount (upperdir + workdir + lowerdir)
         |
         No (View/Committed) → ParentIDs 수 = 1?
                                  |
                                  Yes → bind mount (ro)
                                  |
                                  No → overlay mount (lowerdir만)
```

**왜 부모가 없으면 bind mount인가:**
overlayfs는 최소 하나의 lowerdir이 필요하다. 부모가 없는 스냅샷(기본 레이어)은 단순히 fs/ 디렉토리를 bind mount하는 것으로 충분하다.

### 경로 헬퍼

```go
// 소스: plugins/snapshots/overlay/overlay.go:617-623

func (o *snapshotter) upperPath(id string) string {
    return filepath.Join(o.root, "snapshots", id, "fs")
}

func (o *snapshotter) workPath(id string) string {
    return filepath.Join(o.root, "snapshots", id, "work")
}
```

---

## 11. Commit과 Remove

### Commit: Active → Committed

```go
// 소스: plugins/snapshots/overlay/overlay.go:297-315

func (o *snapshotter) Commit(ctx context.Context, name, key string, opts ...snapshots.Opt) error {
    return o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        // 1. 기존 Active의 ID 조회
        id, _, _, _ := storage.GetInfo(ctx, key)

        // 2. upperdir의 디스크 사용량 계산
        usage, _ := fs.DiskUsage(ctx, o.upperPath(id))

        // 3. 메타데이터에서 Active → Committed 전환
        _, err = storage.CommitActive(ctx, key, name, snapshots.Usage(usage), opts...)
        return err
    })
}
```

Commit 후에는:
- key(Active)로 더 이상 접근 불가
- name(Committed)으로 접근 가능
- 다른 스냅샷의 parent로 사용 가능
- **파일시스템의 실제 데이터는 이동하지 않음** (메타데이터만 변경)

### Remove: 스냅샷 삭제

```go
// 소스: plugins/snapshots/overlay/overlay.go:320-348

func (o *snapshotter) Remove(ctx context.Context, key string) (err error) {
    var removals []string

    // 트랜잭션 완료 후 디렉토리 삭제
    defer func() {
        if err == nil {
            for _, dir := range removals {
                os.RemoveAll(dir)
            }
        }
    }()

    return o.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
        // 1. 메타데이터에서 제거
        _, _, err = storage.Remove(ctx, key)

        // 2. 동기 삭제 모드면 정리 대상 수집
        if !o.asyncRemove {
            removals, _ = o.getCleanupDirectories(ctx)
        }
        return nil
    })
}
```

**왜 defer로 삭제하는가:**
메타데이터 트랜잭션이 **커밋된 후**에 디렉토리를 삭제한다. 트랜잭션 내에서 삭제하면, 디렉토리 삭제에 실패했을 때 트랜잭션 롤백이 필요하지만, 이미 메타데이터에서 키가 제거되었으므로 사용자에게는 삭제된 것으로 보인다. 디렉토리 삭제 실패는 나중에 Cleanup에서 처리한다.

---

## 12. AsyncRemove 패턴

### 비동기 삭제의 필요성

대규모 컨테이너 삭제 시 수 GB의 파일시스템 데이터를 동기적으로 삭제하면 API 응답이 매우 느려진다. AsyncRemove 패턴은 이를 2단계로 분리한다:

```
동기 삭제 (SyncRemove = true):

  Remove(key) ──→ 메타데이터 삭제 ──→ 디렉토리 삭제 ──→ 반환
                                      (느릴 수 있음)

비동기 삭제 (asyncRemove = true):

  Remove(key) ──→ 메타데이터 삭제 ──→ 반환 (빠름!)
                                      |
                    나중에...          v
  Cleanup() ──→ 고아 디렉토리 탐색 ──→ 삭제
```

### Cleanup 구현

```go
// 소스: plugins/snapshots/overlay/overlay.go:371-384

func (o *snapshotter) Cleanup(ctx context.Context) error {
    cleanup, _ := o.cleanupDirectories(ctx)
    for _, dir := range cleanup {
        os.RemoveAll(dir)
    }
    return nil
}
```

### getCleanupDirectories: 고아 디렉토리 탐색

```go
// 소스: plugins/snapshots/overlay/overlay.go:399-426

func (o *snapshotter) getCleanupDirectories(ctx context.Context) ([]string, error) {
    // 1. 메타데이터에서 알려진 스냅샷 ID 목록 가져옴
    ids, _ := storage.IDMap(ctx)

    // 2. 파일시스템의 실제 디렉토리 목록
    snapshotDir := filepath.Join(o.root, "snapshots")
    fd, _ := os.Open(snapshotDir)
    dirs, _ := fd.Readdirnames(0)

    // 3. 차집합 = 고아 디렉토리
    cleanup := []string{}
    for _, d := range dirs {
        if _, ok := ids[d]; ok {
            continue  // 메타데이터에 존재하는 디렉토리는 유지
        }
        cleanup = append(cleanup, filepath.Join(snapshotDir, d))
    }
    return cleanup, nil
}
```

메타데이터에는 없지만 파일시스템에는 존재하는 디렉토리가 "고아 디렉토리"이다. 이는 Remove() 후 아직 정리되지 않은 데이터이거나, 크래시로 인해 남은 잔여물이다.

---

## 13. 이미지 레이어 언팩과 Snapshot 관계

### 언팩 흐름

```
이미지 Pull 완료 후:

Content Store:
  sha256:aaa (layer1.tar.gz)
  sha256:bbb (layer2.tar.gz)
  sha256:ccc (layer3.tar.gz)

Unpack 과정:

1. snapshotter.Prepare("extract-layer1", "")  → mounts
2. mount.All(mounts, tmpDir)
3. 압축 해제: layer1.tar.gz → tmpDir
4. unmount
5. snapshotter.Commit("layer1-chainid", "extract-layer1")

6. snapshotter.Prepare("extract-layer2", "layer1-chainid")  → mounts
7. mount.All(mounts, tmpDir)
8. 압축 해제: layer2.tar.gz → tmpDir
9. unmount
10. snapshotter.Commit("layer2-chainid", "extract-layer2")

11. snapshotter.Prepare("extract-layer3", "layer2-chainid")  → mounts
12. mount.All(mounts, tmpDir)
13. 압축 해제: layer3.tar.gz → tmpDir
14. unmount
15. snapshotter.Commit("layer3-chainid", "extract-layer3")
```

### 컨테이너 생성 시

```
snapshotter.Prepare("container-1-rootfs", "layer3-chainid")
  → overlay mount:
      upperdir = snapshots/{new-id}/fs
      workdir  = snapshots/{new-id}/work
      lowerdir = snapshots/{layer3-id}/fs:snapshots/{layer2-id}/fs:snapshots/{layer1-id}/fs
```

### UnpackKeyFormat

```go
// 소스: core/snapshots/snapshotter.go:29-33

const (
    UnpackKeyPrefix   = "extract"
    UnpackKeyFormat   = UnpackKeyPrefix + "-%s %s"
)
```

언팩 중인 Active 스냅샷의 키는 `extract-{random} {chainid}` 형태이다. 이 접두어로 현재 진행 중인 언팩 작업을 식별하고, 중복 언팩을 방지한다.

---

## 14. Cleaner 인터페이스와 Cleanup

```go
// 소스: core/snapshots/snapshotter.go:353-362

type Cleaner interface {
    Cleanup(ctx context.Context) error
}
```

Cleaner는 Snapshotter의 **선택적(optional)** 인터페이스이다. 비동기 삭제를 지원하는 snapshotter가 구현하며, GC 사이클에서 호출된다.

```
GC 사이클:

1. 도달 가능한 스냅샷 마킹
2. 도달 불가능한 스냅샷 Remove() 호출
3. snapshotter가 Cleaner를 구현하면 Cleanup() 호출
4. Cleanup()이 고아 디렉토리 정리
```

---

## 15. ID 매핑(User Namespace) 지원

### UID/GID 매핑의 필요성

User namespace를 사용하는 컨테이너에서는 호스트와 컨테이너의 UID/GID가 다르다. 이미지 레이어의 파일 소유권을 컨테이너의 UID 범위로 변환해야 한다.

### 구현

```go
// 소스: plugins/snapshots/overlay/overlay.go:468-518 (createSnapshot 내부)

// UID/GID 매핑 라벨 확인
if v, ok := info.Labels[snapshots.LabelSnapshotUIDMapping]; ok {
    uidmapLabel = v
    needsRemap = true
}
if v, ok := info.Labels[snapshots.LabelSnapshotGIDMapping]; ok {
    gidmapLabel = v
    needsRemap = true
}

if needsRemap {
    var idMap userns.IDMap
    idMap.Unmarshal(uidmapLabel, gidmapLabel)
    root, _ := idMap.RootPair()
    mappedUID, mappedGID = int(root.Uid), int(root.Gid)
}

// fs/ 디렉토리의 소유자를 매핑된 UID/GID로 변경
if mappedUID != -1 && mappedGID != -1 {
    os.Lchown(filepath.Join(td, "fs"), mappedUID, mappedGID)
}
```

### 마운트 옵션에서의 ID 매핑

```go
// mounts() 내부
if o.remapIDs {
    if v, ok := info.Labels[snapshots.LabelSnapshotUIDMapping]; ok {
        options = append(options, fmt.Sprintf("uidmap=%s", v))
    }
    if v, ok := info.Labels[snapshots.LabelSnapshotGIDMapping]; ok {
        options = append(options, fmt.Sprintf("gidmap=%s", v))
    }
}
```

Linux 커널 5.12+에서 지원하는 ID-mapped 마운트를 활용한다. 이 방식은 파일 소유권을 실제로 변경하지 않고 마운트 시점에 매핑하므로, 같은 레이어를 다른 UID 범위의 컨테이너들이 공유할 수 있다.

---

## 16. 설계 철학과 Native 비교

### Overlay vs Native Snapshotter

| 항목 | Overlay | Native |
|------|---------|--------|
| CoW 방식 | overlayfs (커널) | 전체 복사 |
| 디스크 효율 | 높음 | 낮음 |
| 성능 | 읽기 빠름 | 읽기/쓰기 동일 |
| 커널 요구 | overlayfs 지원 | 없음 |
| 플랫폼 | Linux | 모든 플랫폼 |
| 레이어 깊이 제한 | 128 (커널 제한) | 없음 |

### 핵심 설계 원칙

1. **인터페이스 중심** -- Snapshotter 인터페이스만 만족하면 어떤 구현이든 가능
2. **Mount 기반 추상화** -- 마운트 시스템콜의 직렬화로 플랫폼 독립성 확보
3. **트랜잭션 기반** -- BoltDB 트랜잭션으로 메타데이터 일관성 보장
4. **CoW 분리** -- CoW 구현은 Snapshotter에, CoW 위의 diff 계산은 Differ에 위임
5. **지연 정리** -- 삭제는 빠르게, 실제 정리는 나중에 (AsyncRemove + Cleanup)

이 설계 덕분에 containerd는 overlayfs, devmapper, ZFS, Windows 등 다양한 저장소 백엔드를 일관된 API로 지원할 수 있다.
