package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// containerd Lease 기반 리소스 수명 관리 시뮬레이션
// =============================================================================
//
// 실제 소스 참조:
//   - core/leases/lease.go          : Lease, Resource, Manager 인터페이스, Opt
//   - core/metadata/leases.go       : leaseManager 구현 (Create, Delete, AddResource 등)
//   - core/metadata/gc.go           : scanRoots에서 Lease를 root로 사용
//   - core/metadata/buckets.go      : v1/{ns}/leases/{id}/ 버킷 구조
//
// Lease 설계 핵심:
//   1. Lease는 리소스(Content, Snapshot, Image)를 "참조"하여 GC에서 보호
//   2. 이미지 pull 중에 Lease를 만들어 중간 레이어를 보호
//   3. Pull 완료 후 Lease 삭제 → 참조 해제 → 다음 GC에서 미사용 리소스 정리
//   4. gc.expire: Lease에 만료 시간 설정, 만료 후 GC에서 root 제외
//   5. gc.flat: 직접 참조한 리소스만 보호, 자식 리소스는 보호하지 않음
//   6. GC의 scanRoots에서 Lease 버킷을 순회하며 leased 리소스를 root로 등록

// =============================================================================
// 1. 네임스페이스 컨텍스트
// =============================================================================

type contextKey string

const (
	namespaceKey contextKey = "containerd.namespace"
	leaseKey     contextKey = "containerd.lease"
)

// WithNamespace는 context에 네임스페이스를 설정한다.
func WithNamespace(ctx context.Context, ns string) context.Context {
	return context.WithValue(ctx, namespaceKey, ns)
}

// NamespaceRequired는 context에서 네임스페이스를 추출한다.
func NamespaceRequired(ctx context.Context) (string, error) {
	ns, ok := ctx.Value(namespaceKey).(string)
	if !ok || ns == "" {
		return "", fmt.Errorf("namespace is required")
	}
	return ns, nil
}

// WithLease는 context에 lease ID를 설정한다.
// 실제: core/leases/context.go — WithLease(ctx, id)
// 이 context를 사용하면 생성되는 리소스가 자동으로 lease에 등록됨
func WithLease(ctx context.Context, leaseID string) context.Context {
	return context.WithValue(ctx, leaseKey, leaseID)
}

// FromContext는 context에서 lease ID를 추출한다.
// 실제: core/leases/context.go — FromContext(ctx)
func FromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(leaseKey).(string)
	return id, ok && id != ""
}

// =============================================================================
// 2. Lease / Resource 데이터 모델
// =============================================================================

// Lease는 리소스를 GC에서 보호하는 참조 그룹이다.
// 실제: core/leases/lease.go — Lease 구조체
//
//	type Lease struct {
//	    ID        string
//	    CreatedAt time.Time
//	    Labels    map[string]string
//	}
type Lease struct {
	ID        string
	CreatedAt time.Time
	Labels    map[string]string
}

// Resource는 Lease가 참조하는 하위 리소스이다.
// 실제: core/leases/lease.go — Resource 구조체
//
//	type Resource struct {
//	    ID   string
//	    Type string
//	}
type Resource struct {
	ID   string
	Type string // "content", "snapshots/{snapshotter}", "images", "ingests"
}

func (r Resource) String() string {
	return fmt.Sprintf("%s/%s", r.Type, r.ID)
}

// GC 관련 레이블 상수
const (
	// LabelGCExpire는 Lease의 만료 시간을 지정한다.
	// 실제: core/metadata/gc.go — labelGCExpire = "containerd.io/gc.expire"
	// 만료된 Lease는 GC scanRoots에서 root 제외 → leased 리소스도 GC 대상
	LabelGCExpire = "containerd.io/gc.expire"

	// LabelGCFlat는 Lease가 직접 참조한 리소스만 보호함을 나타낸다.
	// 실제: core/metadata/gc.go — labelGCFlat = "containerd.io/gc.flat"
	// 예: Image를 flat lease로 보호 → Image의 target content는 보호하지만,
	//     content가 참조하는 다른 content는 보호하지 않음
	LabelGCFlat = "containerd.io/gc.flat"
)

// =============================================================================
// 3. Manager 인터페이스 — Lease CRUD
// =============================================================================

// Manager는 Lease의 생명주기를 관리하는 인터페이스이다.
// 실제: core/leases/lease.go — Manager interface
//
//	type Manager interface {
//	    Create(ctx, ...Opt) (Lease, error)
//	    Delete(ctx, Lease, ...DeleteOpt) error
//	    List(ctx, ...string) ([]Lease, error)
//	    AddResource(ctx, Lease, Resource) error
//	    DeleteResource(ctx, Lease, Resource) error
//	    ListResources(ctx, Lease) ([]Resource, error)
//	}
type Manager interface {
	Create(ctx context.Context, opts ...LeaseOpt) (Lease, error)
	Delete(ctx context.Context, lease Lease) error
	List(ctx context.Context) ([]Lease, error)
	AddResource(ctx context.Context, lease Lease, r Resource) error
	DeleteResource(ctx context.Context, lease Lease, r Resource) error
	ListResources(ctx context.Context, lease Lease) ([]Resource, error)
}

// LeaseOpt는 Lease 생성 옵션이다.
// 실제: core/leases/lease.go — Opt func(*Lease) error
type LeaseOpt func(*Lease) error

// WithID는 Lease ID를 설정한다.
func WithID(id string) LeaseOpt {
	return func(l *Lease) error {
		l.ID = id
		return nil
	}
}

// WithLabels는 Lease 레이블을 설정한다.
func WithLabels(labels map[string]string) LeaseOpt {
	return func(l *Lease) error {
		l.Labels = labels
		return nil
	}
}

// WithExpiration은 만료 시간을 설정한다.
func WithExpiration(d time.Duration) LeaseOpt {
	return func(l *Lease) error {
		if l.Labels == nil {
			l.Labels = make(map[string]string)
		}
		l.Labels[LabelGCExpire] = time.Now().Add(d).Format(time.RFC3339)
		return nil
	}
}

// =============================================================================
// 4. leaseManager — Manager 구현
// =============================================================================

// leaseManager는 Manager 인터페이스의 메모리 기반 구현이다.
// 실제: core/metadata/leases.go — leaseManager 구조체
//
// BoltDB 버킷 구조:
//   v1/{namespace}/leases/{lease_id}/
//     ├── createdat: <binary time>
//     ├── labels/{key}: {value}
//     ├── content/{digest}: <nil>       ← 리소스 참조
//     ├── snapshots/{snapshotter}/{key}: <nil>
//     ├── images/{name}: <nil>
//     └── ingests/{ref}: <nil>
type leaseManager struct {
	mu     sync.RWMutex
	leases map[string]map[string]*leaseData // namespace → leaseID → data
	dirty  func()                           // dirty 콜백 (삭제 시 호출)
}

type leaseData struct {
	lease     Lease
	resources []Resource
}

func newLeaseManager(dirtyFn func()) *leaseManager {
	return &leaseManager{
		leases: make(map[string]map[string]*leaseData),
		dirty:  dirtyFn,
	}
}

// Create는 새 Lease를 생성한다.
// 실제: core/metadata/leases.go — Create(ctx, ...Opt)
//
// BoltDB 작업:
//   1. v1/{ns}/leases 버킷에서 lease ID로 새 버킷 생성
//   2. createdat 저장
//   3. labels 저장
func (lm *leaseManager) Create(ctx context.Context, opts ...LeaseOpt) (Lease, error) {
	ns, err := NamespaceRequired(ctx)
	if err != nil {
		return Lease{}, err
	}

	var l Lease
	for _, opt := range opts {
		if err := opt(&l); err != nil {
			return Lease{}, err
		}
	}
	if l.ID == "" {
		return Lease{}, fmt.Errorf("lease id must be provided")
	}
	l.CreatedAt = time.Now()
	if l.Labels == nil {
		l.Labels = make(map[string]string)
	}

	lm.mu.Lock()
	defer lm.mu.Unlock()

	nsLeases, ok := lm.leases[ns]
	if !ok {
		nsLeases = make(map[string]*leaseData)
		lm.leases[ns] = nsLeases
	}

	if _, exists := nsLeases[l.ID]; exists {
		return Lease{}, fmt.Errorf("lease %q: already exists", l.ID)
	}

	nsLeases[l.ID] = &leaseData{lease: l}
	return l, nil
}

// Delete는 Lease를 삭제한다.
// 실제: core/metadata/leases.go — Delete(ctx, lease, ...DeleteOpt)
//
// BoltDB 작업:
//   1. v1/{ns}/leases/{id} 버킷 삭제
//   2. db.dirty.Add(1) — GC에 삭제 알림
func (lm *leaseManager) Delete(ctx context.Context, lease Lease) error {
	ns, err := NamespaceRequired(ctx)
	if err != nil {
		return err
	}

	lm.mu.Lock()
	defer lm.mu.Unlock()

	nsLeases, ok := lm.leases[ns]
	if !ok {
		return fmt.Errorf("lease %q: not found", lease.ID)
	}

	if _, exists := nsLeases[lease.ID]; !exists {
		return fmt.Errorf("lease %q: not found", lease.ID)
	}

	delete(nsLeases, lease.ID)

	// dirty 마킹 — GC에 삭제 알림
	// 실제: lm.db.dirty.Add(1)
	if lm.dirty != nil {
		lm.dirty()
	}

	return nil
}

// List는 네임스페이스의 모든 Lease를 나열한다.
// 실제: core/metadata/leases.go — List(ctx, ...string)
func (lm *leaseManager) List(ctx context.Context) ([]Lease, error) {
	ns, err := NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}

	lm.mu.RLock()
	defer lm.mu.RUnlock()

	nsLeases := lm.leases[ns]
	var result []Lease
	for _, ld := range nsLeases {
		result = append(result, ld.lease)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].ID < result[j].ID
	})
	return result, nil
}

// AddResource는 Lease에 리소스 참조를 추가한다.
// 실제: core/metadata/leases.go — AddResource(ctx, lease, resource)
//
// BoltDB 작업:
//   1. v1/{ns}/leases/{id}/{resourceType}/{resourceID} 키 생성
//   2. parseLeaseResource로 타입 검증
func (lm *leaseManager) AddResource(ctx context.Context, lease Lease, r Resource) error {
	ns, err := NamespaceRequired(ctx)
	if err != nil {
		return err
	}

	lm.mu.Lock()
	defer lm.mu.Unlock()

	nsLeases := lm.leases[ns]
	if nsLeases == nil {
		return fmt.Errorf("lease %q: not found", lease.ID)
	}

	ld, ok := nsLeases[lease.ID]
	if !ok {
		return fmt.Errorf("lease %q: not found", lease.ID)
	}

	// 중복 검사
	for _, existing := range ld.resources {
		if existing.ID == r.ID && existing.Type == r.Type {
			return nil // 이미 존재
		}
	}

	ld.resources = append(ld.resources, r)
	return nil
}

// DeleteResource는 Lease에서 리소스 참조를 제거한다.
// 실제: core/metadata/leases.go — DeleteResource(ctx, lease, resource)
//
// 삭제 후 db.dirty.Add(1) 호출 — GC에 알림
func (lm *leaseManager) DeleteResource(ctx context.Context, lease Lease, r Resource) error {
	ns, err := NamespaceRequired(ctx)
	if err != nil {
		return err
	}

	lm.mu.Lock()
	defer lm.mu.Unlock()

	nsLeases := lm.leases[ns]
	if nsLeases == nil {
		return fmt.Errorf("lease %q: not found", lease.ID)
	}

	ld, ok := nsLeases[lease.ID]
	if !ok {
		return fmt.Errorf("lease %q: not found", lease.ID)
	}

	for i, existing := range ld.resources {
		if existing.ID == r.ID && existing.Type == r.Type {
			ld.resources = append(ld.resources[:i], ld.resources[i+1:]...)
			// dirty 마킹
			if lm.dirty != nil {
				lm.dirty()
			}
			return nil
		}
	}

	return nil
}

// ListResources는 Lease가 참조하는 모든 리소스를 나열한다.
// 실제: core/metadata/leases.go — ListResources(ctx, lease)
func (lm *leaseManager) ListResources(ctx context.Context, lease Lease) ([]Resource, error) {
	ns, err := NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}

	lm.mu.RLock()
	defer lm.mu.RUnlock()

	nsLeases := lm.leases[ns]
	if nsLeases == nil {
		return nil, fmt.Errorf("lease %q: not found", lease.ID)
	}

	ld, ok := nsLeases[lease.ID]
	if !ok {
		return nil, fmt.Errorf("lease %q: not found", lease.ID)
	}

	result := make([]Resource, len(ld.resources))
	copy(result, ld.resources)
	return result, nil
}

// =============================================================================
// 5. GC 통합 — Lease가 root로 동작
// =============================================================================

// ResourceType은 GC 리소스 타입이다.
type ResourceType uint8

const (
	GCContent  ResourceType = iota + 1
	GCSnapshot
	GCImage
	GCLease
)

func (r ResourceType) String() string {
	switch r {
	case GCContent:
		return "Content"
	case GCSnapshot:
		return "Snapshot"
	case GCImage:
		return "Image"
	case GCLease:
		return "Lease"
	default:
		return "Unknown"
	}
}

// GCNode는 GC 그래프의 노드이다.
type GCNode struct {
	Type      ResourceType
	Namespace string
	Key       string
}

func (n GCNode) String() string {
	return fmt.Sprintf("%s/%s", n.Type, n.Key)
}

// SimulateGCWithLeases는 Lease를 root로 사용하는 GC를 시뮬레이션한다.
// 실제: core/metadata/gc.go — scanRoots에서 Lease 버킷 순회
//
// scanRoots 중 Lease 처리 (gc.go 라인 374~470):
//  1. v1/{ns}/leases 버킷 순회
//  2. 각 lease의 labels에서 gc.expire 확인 → 만료 시 skip
//  3. labels에서 gc.flat 확인 → flat이면 참조 타입에 플래그
//  4. lease 하위의 content, snapshots, images, ingests 버킷 순회
//  5. 각 리소스를 root 노드로 전달
func SimulateGCWithLeases(lm *leaseManager, allResources map[string]ResourceType) (protected, removed []string) {
	fmt.Println("  [GC] Root 수집 (Lease 기반):")

	protectedSet := make(map[string]bool)
	now := time.Now()

	lm.mu.RLock()
	defer lm.mu.RUnlock()

	for ns, nsLeases := range lm.leases {
		for _, ld := range nsLeases {
			// 만료 확인
			// 실제: scanRoots의 gc.expire 체크
			if expStr, ok := ld.lease.Labels[LabelGCExpire]; ok {
				exp, err := time.Parse(time.RFC3339, expStr)
				if err == nil && now.After(exp) {
					fmt.Printf("    Lease %q (ns=%s): 만료됨 → root 제외\n", ld.lease.ID, ns)
					continue
				}
			}

			// flat lease 확인
			flat := false
			if _, ok := ld.lease.Labels[LabelGCFlat]; ok {
				flat = true
			}

			fmt.Printf("    Lease %q (ns=%s, flat=%v): %d개 리소스 보호\n",
				ld.lease.ID, ns, flat, len(ld.resources))

			for _, r := range ld.resources {
				key := r.Type + "/" + r.ID
				protectedSet[key] = true
				if flat {
					fmt.Printf("      [flat] %s (직접 참조만 보호)\n", key)
				} else {
					fmt.Printf("      %s\n", key)
				}
			}
		}
	}

	fmt.Println("  [GC] Sweep:")
	for key, rtype := range allResources {
		if protectedSet[key] {
			protected = append(protected, key)
		} else {
			fmt.Printf("    삭제: %s (%s)\n", key, rtype)
			removed = append(removed, key)
		}
	}

	sort.Strings(protected)
	sort.Strings(removed)
	return
}

// =============================================================================
// 6. 메인 — 데모 실행
// =============================================================================

func main() {
	fmt.Println("=== containerd Lease 기반 리소스 수명 관리 시뮬레이션 ===")
	fmt.Println()

	ctx := WithNamespace(context.Background(), "default")
	dirtyCount := 0
	lm := newLeaseManager(func() {
		dirtyCount++
	})

	// --- 데모 1: Lease 기본 CRUD ---
	fmt.Println("--- 데모 1: Lease 기본 CRUD ---")

	// Lease 생성
	l1, err := lm.Create(ctx, WithID("pull-nginx"), WithLabels(map[string]string{
		"purpose": "image-pull",
	}))
	fmt.Printf("Lease 생성: id=%s, err=%v\n", l1.ID, err)

	l2, err := lm.Create(ctx, WithID("build-myapp"))
	fmt.Printf("Lease 생성: id=%s, err=%v\n", l2.ID, err)

	// 중복 생성 시도
	_, err = lm.Create(ctx, WithID("pull-nginx"))
	fmt.Printf("중복 Lease 생성: err=%v\n", err)

	// Lease 목록
	leases, _ := lm.List(ctx)
	fmt.Printf("Lease 목록: ")
	for _, l := range leases {
		fmt.Printf("%s ", l.ID)
	}
	fmt.Println()
	fmt.Println()

	// --- 데모 2: 리소스 참조 관리 ---
	fmt.Println("--- 데모 2: 리소스 참조 관리 (AddResource/ListResources) ---")

	// pull-nginx lease에 리소스 추가
	// 실제: 이미지 pull 중에 다운로드되는 콘텐트와 생성되는 스냅샷을 lease로 보호
	lm.AddResource(ctx, l1, Resource{ID: "sha256:manifest", Type: "content"})
	lm.AddResource(ctx, l1, Resource{ID: "sha256:layer1", Type: "content"})
	lm.AddResource(ctx, l1, Resource{ID: "sha256:layer2", Type: "content"})
	lm.AddResource(ctx, l1, Resource{ID: "snap-1", Type: "snapshots/overlayfs"})
	lm.AddResource(ctx, l1, Resource{ID: "nginx:latest", Type: "images"})

	resources, _ := lm.ListResources(ctx, l1)
	fmt.Printf("pull-nginx 리소스 (%d개):\n", len(resources))
	for _, r := range resources {
		fmt.Printf("  %s\n", r)
	}

	// build-myapp lease에 리소스 추가
	lm.AddResource(ctx, l2, Resource{ID: "sha256:build-layer", Type: "content"})
	lm.AddResource(ctx, l2, Resource{ID: "build-snap", Type: "snapshots/overlayfs"})

	resources2, _ := lm.ListResources(ctx, l2)
	fmt.Printf("build-myapp 리소스 (%d개):\n", len(resources2))
	for _, r := range resources2 {
		fmt.Printf("  %s\n", r)
	}
	fmt.Println()

	// --- 데모 3: Lease가 GC에서 리소스 보호 ---
	fmt.Println("--- 데모 3: Lease가 GC에서 리소스 보호 ---")

	// 시스템의 전체 리소스 (일부는 lease로 보호됨, 일부는 아님)
	allResources := map[string]ResourceType{
		"content/sha256:manifest":          GCContent,
		"content/sha256:layer1":            GCContent,
		"content/sha256:layer2":            GCContent,
		"content/sha256:build-layer":       GCContent,
		"content/sha256:orphan":            GCContent, // 누구도 참조하지 않음
		"content/sha256:old-cache":         GCContent, // 누구도 참조하지 않음
		"snapshots/overlayfs/snap-1":       GCSnapshot,
		"snapshots/overlayfs/build-snap":   GCSnapshot,
		"snapshots/overlayfs/stale-snap":   GCSnapshot, // 누구도 참조하지 않음
		"images/nginx:latest":              GCImage,
	}

	fmt.Printf("전체 리소스: %d개\n", len(allResources))
	protected, removed := SimulateGCWithLeases(lm, allResources)
	fmt.Printf("\n보호된 리소스: %d개\n", len(protected))
	for _, p := range protected {
		fmt.Printf("  %s\n", p)
	}
	fmt.Printf("삭제된 리소스: %d개\n", len(removed))
	for _, r := range removed {
		fmt.Printf("  %s\n", r)
	}
	fmt.Println()

	// --- 데모 4: Lease 삭제 후 GC ---
	fmt.Println("--- 데모 4: Lease 삭제 후 GC — 참조 해제 ---")
	fmt.Println("pull-nginx Lease 삭제:")
	err = lm.Delete(ctx, l1)
	fmt.Printf("  삭제 결과: err=%v, dirty=%d\n", err, dirtyCount)

	fmt.Println("\n남은 Lease:")
	leases, _ = lm.List(ctx)
	for _, l := range leases {
		fmt.Printf("  %s\n", l.ID)
	}

	fmt.Println("\nGC 재실행:")
	protected2, removed2 := SimulateGCWithLeases(lm, allResources)
	fmt.Printf("\n보호된 리소스: %d개\n", len(protected2))
	for _, p := range protected2 {
		fmt.Printf("  %s\n", p)
	}
	fmt.Printf("삭제된 리소스: %d개 (pull-nginx 리소스도 삭제됨)\n", len(removed2))
	for _, r := range removed2 {
		fmt.Printf("  %s\n", r)
	}
	fmt.Println()

	// --- 데모 5: Lease 만료 (gc.expire) ---
	fmt.Println("--- 데모 5: Lease 만료 (gc.expire) ---")
	lm2 := newLeaseManager(nil)
	ctx2 := WithNamespace(context.Background(), "default")

	// 이미 만료된 lease
	expiredLease, _ := lm2.Create(ctx2,
		WithID("expired-lease"),
		WithLabels(map[string]string{
			LabelGCExpire: time.Now().Add(-1 * time.Hour).Format(time.RFC3339),
		}),
	)
	lm2.AddResource(ctx2, expiredLease, Resource{ID: "sha256:expired-content", Type: "content"})

	// 아직 유효한 lease
	validLease, _ := lm2.Create(ctx2,
		WithID("valid-lease"),
		WithLabels(map[string]string{
			LabelGCExpire: time.Now().Add(24 * time.Hour).Format(time.RFC3339),
		}),
	)
	lm2.AddResource(ctx2, validLease, Resource{ID: "sha256:valid-content", Type: "content"})

	// 만료 없는 lease (영구)
	permanentLease, _ := lm2.Create(ctx2, WithID("permanent-lease"))
	lm2.AddResource(ctx2, permanentLease, Resource{ID: "sha256:permanent-content", Type: "content"})

	allResources2 := map[string]ResourceType{
		"content/sha256:expired-content":   GCContent,
		"content/sha256:valid-content":     GCContent,
		"content/sha256:permanent-content": GCContent,
	}

	fmt.Println("Lease 상태:")
	fmt.Printf("  expired-lease: gc.expire=%s (과거)\n", expiredLease.Labels[LabelGCExpire])
	fmt.Printf("  valid-lease: gc.expire=%s (미래)\n", validLease.Labels[LabelGCExpire])
	fmt.Println("  permanent-lease: gc.expire 없음 (영구)")
	fmt.Println()

	protected3, removed3 := SimulateGCWithLeases(lm2, allResources2)
	fmt.Printf("\n보호: %v\n", protected3)
	fmt.Printf("삭제: %v\n", removed3)
	fmt.Println()

	// --- 데모 6: Flat Lease ---
	fmt.Println("--- 데모 6: Flat Lease (gc.flat) ---")
	fmt.Println(`
Flat Lease 동작:
  일반 Lease: Image → Content(manifest) → Content(layer1) → Content(layer2)
              Lease가 Image를 보호하면 전체 참조 트리가 보호됨

  Flat Lease: Image → Content(manifest)  ← 이것만 보호
              Lease가 Image를 보호해도 manifest의 참조는 보호하지 않음

실제: core/metadata/gc.go — labelGCFlat = "containerd.io/gc.flat"
  if flatV := lblbkt.Get(labelGCFlat); flatV != nil {
      flat = true
  }
  ctype := ResourceContent
  if flat { ctype = resourceContentFlat }  // 0x20 비트 설정

Flat 리소스 타입은 references() 에서 참조 추적을 중단:
  case ResourceSnapshot, resourceSnapshotFlat:
      // ...
      if node.Type == resourceSnapshotFlat { return nil }  // 자식 참조 무시
`)

	lm3 := newLeaseManager(nil)
	ctx3 := WithNamespace(context.Background(), "default")

	// 일반 Lease
	normalLease, _ := lm3.Create(ctx3, WithID("normal-lease"))
	lm3.AddResource(ctx3, normalLease, Resource{ID: "sha256:manifest", Type: "content"})
	lm3.AddResource(ctx3, normalLease, Resource{ID: "sha256:layer1", Type: "content"})
	lm3.AddResource(ctx3, normalLease, Resource{ID: "sha256:layer2", Type: "content"})

	// Flat Lease — 직접 참조만
	flatLease, _ := lm3.Create(ctx3, WithID("flat-lease"), WithLabels(map[string]string{
		LabelGCFlat: "true",
	}))
	lm3.AddResource(ctx3, flatLease, Resource{ID: "sha256:flat-manifest", Type: "content"})
	// flat-manifest가 참조하는 flat-layer1, flat-layer2는 추가하지 않음

	allResources3 := map[string]ResourceType{
		"content/sha256:manifest":      GCContent,
		"content/sha256:layer1":        GCContent,
		"content/sha256:layer2":        GCContent,
		"content/sha256:flat-manifest": GCContent,
		"content/sha256:flat-layer1":   GCContent, // flat lease에서 보호 안 됨
		"content/sha256:flat-layer2":   GCContent, // flat lease에서 보호 안 됨
	}

	fmt.Println("일반 Lease: manifest + layer1 + layer2 보호")
	fmt.Println("Flat Lease: flat-manifest만 보호 (flat-layer1, flat-layer2는 미보호)")
	fmt.Println()

	protected4, removed4 := SimulateGCWithLeases(lm3, allResources3)
	fmt.Printf("\n보호: %v\n", protected4)
	fmt.Printf("삭제: %v\n", removed4)
	fmt.Println()

	// --- 데모 7: 이미지 Pull 시나리오 ---
	fmt.Println("--- 데모 7: 이미지 Pull 시나리오 (Lease 생명주기) ---")
	fmt.Println(`
이미지 Pull 흐름에서 Lease 사용:

  1. Pull 시작 → Lease 생성
     lease, _ = manager.Create(ctx, WithID("pull-xxx"))
     ctx = WithLease(ctx, lease.ID)

  2. 매니페스트 다운로드 → Lease에 리소스 추가
     // content.Writer가 자동으로 addContentLease() 호출
     manager.AddResource(ctx, lease, Resource{ID: "sha256:manifest", Type: "content"})

  3. 레이어 다운로드 → 각 레이어도 Lease에 추가
     manager.AddResource(ctx, lease, Resource{ID: "sha256:layer1", Type: "content"})
     manager.AddResource(ctx, lease, Resource{ID: "sha256:layer2", Type: "content"})

  4. 스냅샷 생성 → Lease에 추가
     // snapshotter가 자동으로 addSnapshotLease() 호출
     manager.AddResource(ctx, lease, Resource{ID: "snap-1", Type: "snapshots/overlayfs"})

  5. 이미지 등록
     // 이미지가 root가 되므로 lease가 없어도 GC에서 보호
     imageStore.Create(ctx, image)

  6. Pull 완료 → Lease 삭제
     manager.Delete(ctx, lease)
     // 이제 이미지 root → content → snapshot 참조 체인으로 보호
     // Lease 없이도 GC에서 안전

  * Lease가 중요한 이유:
    Pull 중간에 GC가 실행되면 아직 이미지에 연결되지 않은
    다운로드 중인 레이어가 삭제될 수 있음.
    Lease가 이를 방지하여 원자적 pull을 보장.
`)

	// 실제 시나리오 시뮬레이션
	lm4 := newLeaseManager(nil)
	ctx4 := WithNamespace(context.Background(), "default")

	// Step 1: Pull 시작 → Lease 생성
	pullLease, _ := lm4.Create(ctx4, WithID("pull-redis"))
	fmt.Printf("Step 1: Lease 생성: %s\n", pullLease.ID)

	// Step 2-4: 리소스 다운로드 및 Lease에 등록
	resources4 := []Resource{
		{ID: "sha256:redis-manifest", Type: "content"},
		{ID: "sha256:redis-layer1", Type: "content"},
		{ID: "sha256:redis-layer2", Type: "content"},
		{ID: "redis-snap", Type: "snapshots/overlayfs"},
	}
	for _, r := range resources4 {
		lm4.AddResource(ctx4, pullLease, r)
		fmt.Printf("Step 2-4: 리소스 추가: %s\n", r)
	}

	// GC 실행 — Lease가 보호
	fmt.Println("\n[GC 실행 — Pull 진행 중]")
	allRes4 := map[string]ResourceType{
		"content/sha256:redis-manifest":     GCContent,
		"content/sha256:redis-layer1":       GCContent,
		"content/sha256:redis-layer2":       GCContent,
		"snapshots/overlayfs/redis-snap":    GCSnapshot,
		"content/sha256:unrelated-orphan":   GCContent,
	}
	p4, r4 := SimulateGCWithLeases(lm4, allRes4)
	fmt.Printf("\n  보호: %d개, 삭제: %d개\n", len(p4), len(r4))

	// Step 5: 이미지 등록 (root가 됨)
	fmt.Println("\nStep 5: 이미지 등록 (이제 이미지 root가 보호)")

	// Step 6: Pull 완료 → Lease 삭제
	lm4.Delete(ctx4, pullLease)
	fmt.Printf("Step 6: Lease 삭제: %s\n", pullLease.ID)

	// Lease 삭제 후 GC — 이미지가 root이므로 리소스 여전히 보호
	fmt.Println("\n[Lease 삭제 후 — 이미지 root가 보호하므로 안전]")
	fmt.Println("(실제로는 Image → Content → Snapshot 참조 체인으로 보호)")

	// context에서 lease 추출
	fmt.Println()
	fmt.Println("--- 참고: context에서 Lease 자동 연결 ---")
	ctxWithLease := WithLease(ctx4, "auto-lease")
	if id, ok := FromContext(ctxWithLease); ok {
		fmt.Printf("context에서 lease ID 추출: %q\n", id)
		fmt.Println("→ content.Writer, snapshotter가 자동으로 lease에 리소스 등록")
	}

	// BoltDB 버킷 구조
	fmt.Println()
	fmt.Println("--- BoltDB Lease 버킷 구조 ---")
	printLeaseSchema()
}

func printLeaseSchema() {
	fmt.Println(`
  v1/{namespace}/leases/
  └── {lease_id}/
      ├── createdat: <binary time>
      ├── labels/
      │   ├── containerd.io/gc.expire: "2024-01-01T00:00:00Z"
      │   └── containerd.io/gc.flat: ""
      ├── content/
      │   ├── sha256:abc123: <nil>
      │   └── sha256:def456: <nil>
      ├── snapshots/
      │   └── overlayfs/
      │       └── snap-1: <nil>
      ├── images/
      │   └── nginx:latest: <nil>
      └── ingests/
          └── upload-ref-1: <nil>

  Lease 삭제 시:
    → 전체 버킷 삭제: topbkt.DeleteBucket([]byte(lease.ID))
    → db.dirty.Add(1) — GC에 알림
    → 다음 GC에서 참조되지 않는 리소스 정리
`)
	_ = strings.TrimSpace // suppress unused import
}
