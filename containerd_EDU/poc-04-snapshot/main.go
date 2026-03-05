// containerd PoC-04: Overlay 스냅샷 시뮬레이션
//
// 실제 소스 참조:
//   - core/snapshots/snapshotter.go                : Snapshotter 인터페이스, Info, Kind, Usage, Opt
//   - core/snapshots/snapshotter.go:44             : Kind = uint8 (KindUnknown=0, KindView=1, KindActive=2, KindCommitted=3)
//   - core/snapshots/snapshotter.go:102            : Info{Kind, Name, Parent, Labels, Created, Updated}
//   - core/snapshots/snapshotter.go:255            : Snapshotter — Prepare, View, Commit, Remove, Mounts, Walk
//   - core/mount/mount.go:34                       : Mount{Type, Source, Target, Options}
//   - plugins/snapshots/overlay/overlay.go         : NewSnapshotter(root, opts) → snapshotter
//   - plugins/snapshots/overlay/overlay.go:108     : snapshotter{root, ms, asyncRemove, upperdirLabel, options}
//   - plugins/snapshots/overlay/overlay.go:265     : Prepare(ctx, key, parent) → createSnapshot(KindActive)
//   - plugins/snapshots/overlay/overlay.go:269     : View(ctx, key, parent) → createSnapshot(KindView)
//   - plugins/snapshots/overlay/overlay.go:297     : Commit(ctx, name, key) → CommitActive 메타데이터 전환
//   - plugins/snapshots/overlay/overlay.go:428     : createSnapshot — temp dir 생성 → fs/ + work/ → rename
//   - plugins/snapshots/overlay/overlay.go:552     : mounts(s) — parentIDs에 따라 bind/overlay 마운트 결정
//   - plugins/snapshots/overlay/overlay.go:617     : upperPath(id) → root/snapshots/{id}/fs
//   - plugins/snapshots/overlay/overlay.go:621     : workPath(id) → root/snapshots/{id}/work
//
// 핵심 개념:
//   1. Snapshotter 인터페이스: Prepare(rw), View(ro), Commit(seal), Remove(delete)
//   2. Kind 전이: Prepare→Active, View→View(ro), Active→Commit→Committed
//   3. Parent-Child CoW 레이어: 이미지 레이어(Committed) → 컨테이너 레이어(Active)
//   4. Overlay mount: lowerdir(parent 체인) + upperdir(현재 fs/) + workdir(작업용 work/)
//   5. 단일 레이어(parent 없음)이면 bind mount, 복수 레이어면 overlay mount
//
// 실행: go run main.go

package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// =============================================================================
// 1. 기본 타입 정의 (core/snapshots/snapshotter.go 참조)
// =============================================================================

// Kind는 스냅샷의 종류
// 실제: snapshots.Kind = uint8
const (
	KindUnknown   uint8 = iota
	KindView            // 읽기 전용 스냅샷
	KindActive          // 읽기/쓰기 가능 (Prepare로 생성)
	KindCommitted       // 커밋된 불변 스냅샷 (parent가 될 수 있음)
)

func kindString(k uint8) string {
	switch k {
	case KindView:
		return "View"
	case KindActive:
		return "Active"
	case KindCommitted:
		return "Committed"
	default:
		return "Unknown"
	}
}

// Info는 스냅샷 메타데이터
// 실제: snapshots.Info{Kind, Name, Parent, Labels, Created, Updated}
type Info struct {
	Kind    uint8
	Name    string            // Active: key, Committed: name
	Parent  string            // 부모 스냅샷 이름
	Labels  map[string]string
	Created time.Time
	Updated time.Time
}

// Usage는 디스크 사용량
// 실제: snapshots.Usage{Inodes, Size}
type Usage struct {
	Inodes int64
	Size   int64
}

// Mount는 마운트 정보
// 실제: mount.Mount{Type, Source, Target, Options}
type Mount struct {
	Type    string
	Source  string
	Target  string
	Options []string
}

// =============================================================================
// 2. 내부 스냅샷 메타데이터 (storage.Snapshot 시뮬레이션)
// =============================================================================

// snapshot은 내부 메타데이터 (실제: storage.Snapshot{Kind, ID, ParentIDs})
type snapshot struct {
	Kind      uint8
	ID        string   // 숫자 ID (디렉토리 이름)
	ParentIDs []string // 부모 체인의 ID 목록 (가장 가까운 것이 [0])
}

// =============================================================================
// 3. Overlay Snapshotter (plugins/snapshots/overlay/overlay.go 참조)
// =============================================================================

// Snapshotter는 Overlay 기반 스냅샷 관리자
// 실제: overlay.snapshotter{root, ms, asyncRemove, upperdirLabel, options, remapIDs, slowChown}
type Snapshotter struct {
	root    string
	options []string // overlay 마운트 옵션 (userxattr, index=off 등)

	mu        sync.Mutex
	idCounter atomic.Int64
	snapshots map[string]*snapshotRecord // key/name → 레코드
}

// snapshotRecord는 스냅샷 메타데이터 레코드 (실제: MetaStore 기반)
type snapshotRecord struct {
	info     Info
	snap     snapshot
	usage    Usage
}

// NewSnapshotter는 Overlay Snapshotter를 생성
// 실제: overlay.NewSnapshotter(root, opts)
//   - os.MkdirAll(root, 0700)
//   - fs.SupportsDType(root) 확인
//   - storage.NewMetaStore(metadata.db) 생성
//   - os.Mkdir(root/snapshots, 0700)
//   - userxattr, index=off 옵션 자동 감지
func NewSnapshotter(root string) (*Snapshotter, error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, fmt.Errorf("root 디렉토리 생성 실패: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "snapshots"), 0700); err != nil {
		return nil, fmt.Errorf("snapshots 디렉토리 생성 실패: %w", err)
	}

	return &Snapshotter{
		root:      root,
		options:   []string{"index=off"}, // 실제: supportsIndex() 검사 후 추가
		snapshots: make(map[string]*snapshotRecord),
	}, nil
}

// upperPath는 스냅샷의 fs (upper) 디렉토리 경로
// 실제: overlay.go:617 — root/snapshots/{id}/fs
func (o *Snapshotter) upperPath(id string) string {
	return filepath.Join(o.root, "snapshots", id, "fs")
}

// workPath는 스냅샷의 work 디렉토리 경로
// 실제: overlay.go:621 — root/snapshots/{id}/work
func (o *Snapshotter) workPath(id string) string {
	return filepath.Join(o.root, "snapshots", id, "work")
}

// nextID는 새 스냅샷 ID를 생성
func (o *Snapshotter) nextID() string {
	return fmt.Sprintf("%d", o.idCounter.Add(1))
}

// =============================================================================
// 4. Prepare — Active 스냅샷 생성
// =============================================================================

// Prepare는 쓰기 가능한 Active 스냅샷을 생성
// 실제: overlay.go:265 Prepare(ctx, key, parent, opts) → createSnapshot(KindActive)
//
// 흐름:
//   1. temp 디렉토리 생성 (new-XXXXX)
//   2. fs/ 디렉토리 생성 (upperdir)
//   3. Active이면 work/ 디렉토리도 생성 (workdir)
//   4. storage.CreateSnapshot(ctx, KindActive, key, parent, opts)
//   5. os.Rename(temp, snapshots/{id})
//   6. mounts(snapshot) 반환
func (o *Snapshotter) Prepare(key, parent string) ([]Mount, error) {
	return o.createSnapshot(KindActive, key, parent)
}

// =============================================================================
// 5. View — 읽기 전용 스냅샷 생성
// =============================================================================

// View는 읽기 전용 스냅샷을 생성
// 실제: overlay.go:269 View(ctx, key, parent, opts) → createSnapshot(KindView)
// Prepare와 동일하지만 work/ 디렉토리를 생성하지 않고, mounts에 "ro" 옵션이 추가됨
func (o *Snapshotter) View(key, parent string) ([]Mount, error) {
	return o.createSnapshot(KindView, key, parent)
}

// createSnapshot은 스냅샷 생성의 핵심 로직
// 실제: overlay.go:428 createSnapshot(ctx, kind, key, parent, opts)
func (o *Snapshotter) createSnapshot(kind uint8, key, parent string) ([]Mount, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	// 중복 키 체크
	if _, exists := o.snapshots[key]; exists {
		return nil, fmt.Errorf("snapshot %q already exists", key)
	}

	// 부모 확인
	var parentIDs []string
	if parent != "" {
		parentRec, ok := o.snapshots[parent]
		if !ok {
			return nil, fmt.Errorf("parent %q not found", parent)
		}
		if parentRec.info.Kind != KindCommitted {
			return nil, fmt.Errorf("parent %q is not committed (kind=%s)", parent, kindString(parentRec.info.Kind))
		}
		// 부모 체인 구성: [부모ID, 부모의부모ID, ...]
		parentIDs = append([]string{parentRec.snap.ID}, parentRec.snap.ParentIDs...)
	}

	// 새 ID 할당
	id := o.nextID()

	// 디렉토리 생성
	// 실제: prepareDirectory(ctx, snapshotDir, kind) → MkdirTemp → Mkdir(fs/) + Mkdir(work/)
	snapshotPath := filepath.Join(o.root, "snapshots", id)
	if err := os.MkdirAll(snapshotPath, 0755); err != nil {
		return nil, err
	}

	// fs/ (upperdir) — 항상 생성
	if err := os.Mkdir(filepath.Join(snapshotPath, "fs"), 0755); err != nil {
		return nil, err
	}

	// work/ — Active일 때만 생성
	// 실제: overlay.go:543-546 — if kind == KindActive { os.Mkdir("work", 0711) }
	if kind == KindActive {
		if err := os.Mkdir(filepath.Join(snapshotPath, "work"), 0711); err != nil {
			return nil, err
		}
	}

	snap := snapshot{
		Kind:      kind,
		ID:        id,
		ParentIDs: parentIDs,
	}

	now := time.Now()
	rec := &snapshotRecord{
		info: Info{
			Kind:    kind,
			Name:    key,
			Parent:  parent,
			Created: now,
			Updated: now,
		},
		snap: snap,
	}
	o.snapshots[key] = rec

	return o.mounts(snap), nil
}

// =============================================================================
// 6. Commit — Active → Committed 전환
// =============================================================================

// Commit은 Active 스냅샷을 Committed로 전환
// 실제: overlay.go:297 Commit(ctx, name, key, opts)
//   1. GetInfo(ctx, key) → 기존 스냅샷 ID 획득
//   2. fs.DiskUsage(ctx, upperPath(id)) → 사용량 계산
//   3. storage.CommitActive(ctx, key, name, usage) → 메타데이터 전환
//
// 핵심: Active 스냅샷의 key가 사라지고 Committed 스냅샷의 name이 생긴다
// 디렉토리 자체는 이동하지 않고, 메타데이터만 변경된다
func (o *Snapshotter) Commit(name, key string) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	rec, ok := o.snapshots[key]
	if !ok {
		return fmt.Errorf("snapshot %q not found", key)
	}
	if rec.info.Kind != KindActive {
		return fmt.Errorf("snapshot %q is not active (kind=%s)", key, kindString(rec.info.Kind))
	}
	if _, exists := o.snapshots[name]; exists {
		return fmt.Errorf("name %q already exists", name)
	}

	// 사용량 계산 (시뮬레이션)
	// 실제: fs.DiskUsage(ctx, upperPath(id))
	usage := Usage{Inodes: 10, Size: 4096}

	// 메타데이터 전환: key 삭제 → name으로 재등록
	delete(o.snapshots, key)

	rec.info.Kind = KindCommitted
	rec.info.Name = name
	rec.info.Updated = time.Now()
	rec.snap.Kind = KindCommitted
	rec.usage = usage

	o.snapshots[name] = rec
	return nil
}

// =============================================================================
// 7. Mounts — 마운트 정보 반환
// =============================================================================

// Mounts는 Active 스냅샷의 마운트 정보를 반환
// 실제: overlay.go:277 Mounts(ctx, key) → storage.GetSnapshot(ctx, key) → mounts(s)
func (o *Snapshotter) Mounts(key string) ([]Mount, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	rec, ok := o.snapshots[key]
	if !ok {
		return nil, fmt.Errorf("snapshot %q not found", key)
	}
	return o.mounts(rec.snap), nil
}

// mounts는 스냅샷에 대한 마운트 옵션을 결정
// 실제: overlay.go:552 mounts(s storage.Snapshot, info snapshots.Info) []mount.Mount
//
// 마운트 결정 로직:
//   1. ParentIDs 없음 (단일 레이어) → bind mount
//      - Active: bind + rw
//      - View: bind + ro
//   2. ParentIDs 있음 + Active → overlay mount
//      - upperdir = snapshots/{id}/fs
//      - workdir  = snapshots/{id}/work
//      - lowerdir = 부모들의 fs 경로 (: 구분)
//   3. ParentIDs 1개 + View → bind mount (ro)
//   4. ParentIDs 여러개 + View → overlay mount (lowerdir만, upperdir/workdir 없음)
func (o *Snapshotter) mounts(s snapshot) []Mount {
	var options []string

	if len(s.ParentIDs) == 0 {
		// 부모 없음 → bind mount
		// 실제: overlay.go:564-581
		roFlag := "rw"
		if s.Kind == KindView {
			roFlag = "ro"
		}
		return []Mount{
			{
				Source:  o.upperPath(s.ID),
				Type:    "bind",
				Options: []string{roFlag, "rbind"},
			},
		}
	}

	if s.Kind == KindActive {
		// Active + 부모 있음 → overlay mount
		// 실제: overlay.go:583-587
		options = append(options,
			fmt.Sprintf("workdir=%s", o.workPath(s.ID)),
			fmt.Sprintf("upperdir=%s", o.upperPath(s.ID)),
		)
	} else if len(s.ParentIDs) == 1 {
		// View + 부모 1개 → 부모의 fs를 bind mount (ro)
		// 실제: overlay.go:588-598
		return []Mount{
			{
				Source:  o.upperPath(s.ParentIDs[0]),
				Type:    "bind",
				Options: []string{"ro", "rbind"},
			},
		}
	}

	// lowerdir 구성: 부모들의 fs 경로를 : 로 연결
	// 실제: overlay.go:601-605
	parentPaths := make([]string, len(s.ParentIDs))
	for i := range s.ParentIDs {
		parentPaths[i] = o.upperPath(s.ParentIDs[i])
	}
	options = append(options, fmt.Sprintf("lowerdir=%s", strings.Join(parentPaths, ":")))
	options = append(options, o.options...)

	return []Mount{
		{
			Type:    "overlay",
			Source:  "overlay",
			Options: options,
		},
	}
}

// =============================================================================
// 8. Remove — 스냅샷 삭제
// =============================================================================

// Remove는 스냅샷을 삭제
// 실제: overlay.go:320 Remove(ctx, key) → storage.Remove(ctx, key) + os.RemoveAll
func (o *Snapshotter) Remove(key string) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	rec, ok := o.snapshots[key]
	if !ok {
		return fmt.Errorf("snapshot %q not found", key)
	}

	// 자식이 있는지 확인 (부모로 사용 중이면 삭제 불가)
	for _, other := range o.snapshots {
		if other.info.Parent == key {
			return fmt.Errorf("snapshot %q has children, remove them first", key)
		}
	}

	// 디렉토리 삭제
	snapshotPath := filepath.Join(o.root, "snapshots", rec.snap.ID)
	os.RemoveAll(snapshotPath)

	delete(o.snapshots, key)
	return nil
}

// =============================================================================
// 9. Stat, Walk — 조회
// =============================================================================

// Stat은 스냅샷 정보를 반환
// 실제: overlay.go:194 Stat(ctx, key) → storage.GetInfo(ctx, key)
func (o *Snapshotter) Stat(key string) (Info, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	rec, ok := o.snapshots[key]
	if !ok {
		return Info{}, fmt.Errorf("snapshot %q not found", key)
	}
	return rec.info, nil
}

// Walk는 모든 스냅샷을 순회
// 실제: overlay.go:351 Walk(ctx, fn, filters) → storage.WalkInfo(ctx, fn, filters)
func (o *Snapshotter) Walk(fn func(Info) error) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	for _, rec := range o.snapshots {
		if err := fn(rec.info); err != nil {
			return err
		}
	}
	return nil
}

// =============================================================================
// 10. 이미지 레이어 언팩 시뮬레이션
// =============================================================================

// simulateUnpack은 이미지 레이어를 스냅샷으로 언팩하는 과정을 시뮬레이션
// 실제 흐름 (image unpack):
//   1. 각 layer에 대해:
//      a. Prepare(extractKey, parent) → Active 스냅샷 + mounts
//      b. mount.All(mounts, tempDir) → 마운트
//      c. layer 압축해제 → tempDir에 풀기
//      d. Commit(chainID, extractKey) → Committed로 전환
//   2. 다음 layer의 parent = 이전 layer의 chainID
func simulateUnpack(sn *Snapshotter, layers []string) (string, error) {
	parent := "" // 첫 레이어는 빈 부모

	for i, layerDigest := range layers {
		extractKey := fmt.Sprintf("extract-%d %s", i, layerDigest)
		chainID := computeChainID(layers[:i+1])

		fmt.Printf("\n    Layer %d: %s\n", i, layerDigest[:20]+"...")
		fmt.Printf("      extractKey: %s\n", extractKey)
		fmt.Printf("      chainID:    %s\n", chainID[:30]+"...")
		fmt.Printf("      parent:     %s\n", parentOrNone(parent))

		// 1. Prepare — Active 스냅샷 생성
		mounts, err := sn.Prepare(extractKey, parent)
		if err != nil {
			return "", fmt.Errorf("prepare layer %d: %w", i, err)
		}
		fmt.Printf("      Prepare:    OK (mount=%s)\n", mountSummary(mounts[0]))

		// 2. 시뮬레이션: 마운트 후 레이어 압축해제
		// 실제: mount.All(mounts, tmpDir) → unpackLayer(tmpDir, layer)
		// 여기서는 fs/ 디렉토리에 파일 생성으로 대체
		rec := sn.snapshots[extractKey]
		fsDir := sn.upperPath(rec.snap.ID)
		os.WriteFile(filepath.Join(fsDir, fmt.Sprintf("file-from-layer-%d.txt", i)),
			[]byte(fmt.Sprintf("content from layer %d: %s", i, layerDigest)), 0644)
		fmt.Printf("      Unpack:     OK (파일 생성: file-from-layer-%d.txt)\n", i)

		// 3. Commit — Active → Committed
		if err := sn.Commit(chainID, extractKey); err != nil {
			return "", fmt.Errorf("commit layer %d: %w", i, err)
		}
		fmt.Printf("      Commit:     OK (%s → %s)\n", extractKey, chainID[:30]+"...")

		parent = chainID
	}

	return parent, nil
}

// =============================================================================
// 도우미 함수들
// =============================================================================

func computeChainID(layers []string) string {
	// 실제: ChainID = SHA256(chainID(parent) + " " + diffID)
	combined := strings.Join(layers, " ")
	h := sha256.Sum256([]byte(combined))
	return fmt.Sprintf("sha256:%x", h)
}

func parentOrNone(s string) string {
	if s == "" {
		return "(none)"
	}
	if len(s) > 30 {
		return s[:30] + "..."
	}
	return s
}

func mountSummary(m Mount) string {
	if m.Type == "bind" {
		return fmt.Sprintf("bind [%s]", strings.Join(m.Options, ","))
	}
	return fmt.Sprintf("overlay [%s]", strings.Join(m.Options, ", ")[:60]+"...")
}

func printMounts(mounts []Mount) {
	for _, m := range mounts {
		fmt.Printf("      Type:    %s\n", m.Type)
		fmt.Printf("      Source:  %s\n", m.Source)
		for _, opt := range m.Options {
			fmt.Printf("      Option:  %s\n", opt)
		}
	}
}

// =============================================================================
// main
// =============================================================================

func main() {
	fmt.Println("=" + strings.Repeat("=", 69))
	fmt.Println("containerd Overlay Snapshot 시뮬레이션")
	fmt.Println("=" + strings.Repeat("=", 69))

	// 임시 디렉토리 생성
	tmpDir, err := os.MkdirTemp("", "containerd-snapshot-*")
	if err != nil {
		fmt.Printf("임시 디렉토리 생성 실패: %v\n", err)
		return
	}
	defer os.RemoveAll(tmpDir)

	sn, err := NewSnapshotter(tmpDir)
	if err != nil {
		fmt.Printf("Snapshotter 생성 실패: %v\n", err)
		return
	}

	// =====================================================================
	// 1. Snapshotter 인터페이스 개요
	// =====================================================================
	fmt.Println("\n[1] Snapshotter 인터페이스 개요")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println(`
  실제 인터페이스 (core/snapshots/snapshotter.go:255):
  ┌──────────────────────────────────────────────────────────┐
  │ Snapshotter                                              │
  ├──────────────────────────────────────────────────────────┤
  │ Stat(ctx, key) → (Info, error)                          │
  │ Update(ctx, info, fieldpaths) → (Info, error)           │
  │ Usage(ctx, key) → (Usage, error)                        │
  │ Mounts(ctx, key) → ([]mount.Mount, error)               │
  │ Prepare(ctx, key, parent, opts) → ([]mount.Mount, error)│ ← Active
  │ View(ctx, key, parent, opts) → ([]mount.Mount, error)   │ ← View(ro)
  │ Commit(ctx, name, key, opts) → error                    │ ← Committed
  │ Remove(ctx, key) → error                                │
  │ Walk(ctx, fn, filters) → error                          │
  │ Close() → error                                         │
  └──────────────────────────────────────────────────────────┘

  Kind 전이:
    Prepare(key, parent) ──→ Active (rw)  ──→ Commit(name, key) ──→ Committed
    View(key, parent)    ──→ View (ro)    ──→ (commit 불가, Remove만 가능)
  `)

	// =====================================================================
	// 2. 이미지 레이어 언팩 시뮬레이션
	// =====================================================================
	fmt.Println("[2] 이미지 레이어 언팩 시뮬레이션")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println("  nginx:1.25 이미지의 3개 레이어를 순차 언팩:")

	layers := []string{
		"sha256:aaaaaa1111111111111111111111111111111111111111111111111111111111",
		"sha256:bbbbbb2222222222222222222222222222222222222222222222222222222222",
		"sha256:cccccc3333333333333333333333333333333333333333333333333333333333",
	}

	imageChainID, err := simulateUnpack(sn, layers)
	if err != nil {
		fmt.Printf("  언팩 실패: %v\n", err)
		return
	}
	fmt.Printf("\n  이미지 RootFS ChainID: %s\n", imageChainID[:40]+"...")

	// =====================================================================
	// 3. 컨테이너 스냅샷 생성 (Prepare)
	// =====================================================================
	fmt.Println("\n[3] 컨테이너 스냅샷 생성 (Prepare)")
	fmt.Println(strings.Repeat("-", 60))

	containerKey := "container-nginx-001"
	fmt.Printf("  Prepare(key=%q, parent=%q)\n", containerKey, imageChainID[:30]+"...")

	mounts, err := sn.Prepare(containerKey, imageChainID)
	if err != nil {
		fmt.Printf("  Prepare 실패: %v\n", err)
		return
	}

	fmt.Println("  반환된 Mount:")
	printMounts(mounts)

	fmt.Println("\n  Overlay 마운트 다이어그램:")
	fmt.Println("  ┌───────────────────────────────────────────────┐")
	fmt.Println("  │           Container Layer (Active)            │")
	fmt.Println("  │  upperdir = snapshots/4/fs  (rw, CoW)        │")
	fmt.Println("  │  workdir  = snapshots/4/work                 │")
	fmt.Println("  ├───────────────────────────────────────────────┤")
	fmt.Println("  │           Layer 2 (Committed, lowerdir)       │")
	fmt.Println("  │  snapshots/3/fs                               │")
	fmt.Println("  ├───────────────────────────────────────────────┤")
	fmt.Println("  │           Layer 1 (Committed, lowerdir)       │")
	fmt.Println("  │  snapshots/2/fs                               │")
	fmt.Println("  ├───────────────────────────────────────────────┤")
	fmt.Println("  │           Layer 0 (Committed, lowerdir)       │")
	fmt.Println("  │  snapshots/1/fs                               │")
	fmt.Println("  └───────────────────────────────────────────────┘")

	// =====================================================================
	// 4. View 스냅샷 (읽기 전용)
	// =====================================================================
	fmt.Println("\n[4] View 스냅샷 (읽기 전용)")
	fmt.Println(strings.Repeat("-", 60))

	viewKey := "inspect-nginx-view"
	fmt.Printf("  View(key=%q, parent=%q)\n", viewKey, imageChainID[:30]+"...")

	viewMounts, err := sn.View(viewKey, imageChainID)
	if err != nil {
		fmt.Printf("  View 실패: %v\n", err)
		return
	}

	fmt.Println("  반환된 Mount:")
	printMounts(viewMounts)
	fmt.Println("  → View는 upperdir/workdir 없이 lowerdir만 (읽기 전용)")

	// View에서 Commit 불가 확인
	// 실제에서는 Commit 시 View를 거부하는 로직이 있음
	fmt.Println("\n  View→Commit 시도:")
	viewInfo, _ := sn.Stat(viewKey)
	if viewInfo.Kind == KindView {
		fmt.Println("  → View 스냅샷은 Commit 불가 (설계 원칙)")
		fmt.Println("  → View는 Remove만 가능")
	}

	// =====================================================================
	// 5. 컨테이너에 파일 쓰기 (CoW 시뮬레이션)
	// =====================================================================
	fmt.Println("\n[5] 컨테이너 파일 쓰기 (CoW 시뮬레이션)")
	fmt.Println(strings.Repeat("-", 60))

	// Active 스냅샷의 upperdir에 파일 작성
	rec := sn.snapshots[containerKey]
	upperDir := sn.upperPath(rec.snap.ID)

	// 새 파일 생성 (overlay upperdir에 저장됨)
	os.WriteFile(filepath.Join(upperDir, "container-created.txt"),
		[]byte("Created by running container"), 0644)
	// 수정 파일 (overlay에서 CoW: lowerdir 파일을 upperdir로 복사 후 수정)
	os.WriteFile(filepath.Join(upperDir, "modified-index.html"),
		[]byte("Modified nginx index.html"), 0644)

	fmt.Println("  Container upperdir에 파일 생성:")
	entries, _ := os.ReadDir(upperDir)
	for _, e := range entries {
		fi, _ := e.Info()
		fmt.Printf("    %s  %d bytes\n", e.Name(), fi.Size())
	}

	fmt.Println("\n  CoW (Copy-on-Write) 동작 원리:")
	fmt.Println("  ┌─────────────────────────────────────────────────┐")
	fmt.Println("  │ Read:  lowerdir에서 파일을 직접 읽음             │")
	fmt.Println("  │ Write: lowerdir 파일을 upperdir로 복사 후 수정   │")
	fmt.Println("  │ New:   upperdir에 새 파일 생성                   │")
	fmt.Println("  │ Del:   upperdir에 whiteout 파일 생성             │")
	fmt.Println("  │        (lowerdir 원본은 변경되지 않음!)           │")
	fmt.Println("  └─────────────────────────────────────────────────┘")

	// =====================================================================
	// 6. 컨테이너 스냅샷을 새 이미지로 Commit
	// =====================================================================
	fmt.Println("\n[6] 컨테이너 스냅샷을 새 이미지로 Commit")
	fmt.Println(strings.Repeat("-", 60))

	newImageName := "sha256:new-image-layer-from-container"
	fmt.Printf("  Commit(name=%q, key=%q)\n", newImageName, containerKey)

	if err := sn.Commit(newImageName, containerKey); err != nil {
		fmt.Printf("  Commit 실패: %v\n", err)
	} else {
		fmt.Println("  → Active 스냅샷이 Committed로 전환됨")
		fmt.Println("  → 이제 이 스냅샷을 parent로 새 컨테이너 생성 가능")

		// Committed 스냅샷으로 새 컨테이너 생성
		newContainerKey := "container-from-committed"
		newMounts, err := sn.Prepare(newContainerKey, newImageName)
		if err == nil {
			fmt.Printf("\n  새 컨테이너 Prepare: %s\n", newContainerKey)
			fmt.Println("  반환된 Mount:")
			printMounts(newMounts)
		}
	}

	// =====================================================================
	// 7. Stat으로 스냅샷 정보 조회
	// =====================================================================
	fmt.Println("\n[7] Stat으로 스냅샷 정보 조회")
	fmt.Println(strings.Repeat("-", 60))

	// Walk로 모든 스냅샷 출력
	fmt.Println("  ┌────┬────────────┬────────────────────────────┬────────────────────────────┐")
	fmt.Println("  │ #  │ Kind       │ Name                       │ Parent                     │")
	fmt.Println("  ├────┼────────────┼────────────────────────────┼────────────────────────────┤")
	idx := 0
	sn.Walk(func(info Info) error {
		idx++
		name := info.Name
		if len(name) > 26 {
			name = name[:26] + ".."
		}
		parent := info.Parent
		if parent == "" {
			parent = "(none)"
		} else if len(parent) > 26 {
			parent = parent[:26] + ".."
		}
		fmt.Printf("  │ %d  │ %-10s │ %-26s │ %-26s │\n", idx, kindString(info.Kind), name, parent)
		return nil
	})
	fmt.Println("  └────┴────────────┴────────────────────────────┴────────────────────────────┘")

	// =====================================================================
	// 8. Remove — 스냅샷 삭제
	// =====================================================================
	fmt.Println("\n[8] Remove — 스냅샷 삭제")
	fmt.Println(strings.Repeat("-", 60))

	// View 삭제
	fmt.Printf("  Remove(%q): ", viewKey)
	if err := sn.Remove(viewKey); err != nil {
		fmt.Printf("실패: %v\n", err)
	} else {
		fmt.Println("성공")
	}

	// 새 컨테이너 삭제
	fmt.Printf("  Remove(%q): ", "container-from-committed")
	if err := sn.Remove("container-from-committed"); err != nil {
		fmt.Printf("실패: %v\n", err)
	} else {
		fmt.Println("성공")
	}

	// 부모가 있는 스냅샷 삭제 시도 (실패 예상)
	chainID1 := computeChainID(layers[:1])
	fmt.Printf("  Remove(%q) [자식 있는 부모]: ", chainID1[:30]+"...")
	if err := sn.Remove(chainID1); err != nil {
		fmt.Printf("실패: %v\n", err)
	}

	// =====================================================================
	// 9. 단일 레이어 (bind mount) vs 다중 레이어 (overlay mount)
	// =====================================================================
	fmt.Println("\n[9] 마운트 타입 비교: bind vs overlay")
	fmt.Println(strings.Repeat("-", 60))

	// 단일 레이어: 빈 부모에서 Prepare → bind mount
	singleKey := "single-layer-active"
	singleMounts, _ := sn.Prepare(singleKey, "")
	fmt.Println("  [단일 레이어] Prepare(key, parent=\"\")")
	fmt.Printf("    → Mount Type: %s\n", singleMounts[0].Type)
	fmt.Printf("    → Options: %v\n", singleMounts[0].Options)
	fmt.Println("    → 부모 없으면 overlay가 아닌 bind mount 사용")
	fmt.Println("    → 실제: overlay.go:564 'if len(s.ParentIDs) == 0'")

	// 다중 레이어: 이미지 위에 Prepare → overlay mount
	multiKey := "multi-layer-active"
	multiMounts, _ := sn.Prepare(multiKey, newImageName)
	fmt.Println("\n  [다중 레이어] Prepare(key, parent=imageChainID)")
	fmt.Printf("    → Mount Type: %s\n", multiMounts[0].Type)
	fmt.Println("    → Options:")
	for _, opt := range multiMounts[0].Options {
		// 경로를 축약
		short := opt
		if len(short) > 70 {
			short = short[:70] + "..."
		}
		fmt.Printf("      %s\n", short)
	}

	// =====================================================================
	// 10. 파일시스템 구조 확인
	// =====================================================================
	fmt.Println("\n[10] 실제 파일시스템 구조")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Printf("  root: %s\n", tmpDir)

	snapshotsDir := filepath.Join(tmpDir, "snapshots")
	entries2, _ := os.ReadDir(snapshotsDir)
	for _, e := range entries2 {
		snID := e.Name()
		snPath := filepath.Join(snapshotsDir, snID)
		subEntries, _ := os.ReadDir(snPath)

		var subNames []string
		for _, se := range subEntries {
			subNames = append(subNames, se.Name()+"/")
		}
		fmt.Printf("  snapshots/%s/ → [%s]\n", snID, strings.Join(subNames, " "))

		// fs/ 디렉토리의 파일 목록
		fsPath := filepath.Join(snPath, "fs")
		fsEntries, _ := os.ReadDir(fsPath)
		for _, fe := range fsEntries {
			fi, _ := fe.Info()
			fmt.Printf("    fs/%s (%d bytes)\n", fe.Name(), fi.Size())
		}
	}

	// =====================================================================
	// 11. 전체 동작 요약
	// =====================================================================
	fmt.Println("\n" + strings.Repeat("=", 70))
	fmt.Println("Overlay Snapshotter 동작 요약")
	fmt.Println(strings.Repeat("=", 70))
	fmt.Println(`
  이미지 레이어 언팩 → 컨테이너 실행 흐름:

  1. 이미지 언팩 (각 layer에 대해):
     Prepare(extractKey, parent) → mount → 압축해제 → Commit(chainID, key)

  2. 컨테이너 생성:
     Prepare(containerKey, imageChainID) → overlay mount 반환
     ┌─────────────────┐
     │ upperdir (rw)   │ ← 컨테이너 변경사항
     ├─────────────────┤
     │ lowerdir (ro)   │ ← 이미지 레이어 체인
     │  layer 2        │
     │  layer 1        │
     │  layer 0        │
     └─────────────────┘

  3. 컨테이너 실행:
     mount.All(mounts, rootfs) → 프로세스 실행
     - 읽기: lowerdir에서 파일 읽기 (zero-copy)
     - 쓰기: CoW로 upperdir에 복사 후 수정
     - 삭제: whiteout 파일 생성

  4. 새 이미지 생성 (선택):
     Commit(newName, containerKey) → Active → Committed
     → 새 이미지의 레이어로 사용 가능

  5. 컨테이너 종료:
     Remove(containerKey) → upperdir 삭제 (변경사항 폐기)

  디렉토리 구조:
  root/
  └── snapshots/
      ├── 1/                ← Layer 0 (Committed)
      │   └── fs/           ← 이 레이어의 파일들
      ├── 2/                ← Layer 1 (Committed)
      │   ├── fs/
      │   └── (work/ 없음 — Committed는 work 불필요)
      ├── 3/                ← Layer 2 (Committed)
      │   └── fs/
      └── 4/                ← Container (Active)
          ├── fs/           ← upperdir (변경사항)
          └── work/         ← overlay workdir`)

	fmt.Println("\n" + strings.Repeat("=", 70))
	fmt.Println("containerd Overlay Snapshot PoC 완료")
	fmt.Println(strings.Repeat("=", 70))
}
