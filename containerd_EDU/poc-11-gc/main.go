package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// =============================================================================
// containerd Tricolor GC 알고리즘 시뮬레이션
// =============================================================================
//
// 실제 소스 참조:
//   - pkg/gc/gc.go                : Tricolor(), ConcurrentMark(), Sweep(), Node, ResourceType
//   - core/metadata/gc.go         : ResourceType 상수, scanRoots(), references(), scanAll(), remove()
//   - core/metadata/db.go         : GarbageCollect() — wlock, getMarked, sweep, dirty 리셋
//   - plugins/gc/scheduler.go     : gcScheduler — mutation 기반 GC 스케줄링
//
// containerd GC 설계 핵심:
//   1. Tricolor Mark-Sweep: 위키피디아 tri-color marking 알고리즘 구현
//   2. Root 수집: Lease, Image(비만료), Container가 root 노드
//   3. 참조 추적: containerd.io/gc.ref.* 레이블로 리소스 간 참조 표현
//   4. BFS/DFS 탐색: gray 스택에서 하나씩 꺼내며 참조 추적 (depth-first)
//   5. GC 스케줄러: mutation count, deletion threshold 기반 자동 스케줄

// =============================================================================
// 1. 리소스 타입 정의
// =============================================================================

// ResourceType은 GC 대상 리소스의 타입을 나타낸다.
// 실제: core/metadata/gc.go — ResourceType 상수 (iota)
type ResourceType uint8

const (
	ResourceUnknown   ResourceType = iota
	ResourceContent                // 콘텐트 블롭 (OCI 레이어, 매니페스트 등)
	ResourceSnapshot               // 스냅샷 (파일시스템 레이어)
	ResourceContainer              // 컨테이너
	ResourceTask                   // 태스크 (실행 중인 프로세스)
	ResourceImage                  // 이미지
	ResourceLease                  // 리스
	ResourceIngest                 // 콘텐트 인제스트 (진행 중인 다운로드)
)

func (r ResourceType) String() string {
	switch r {
	case ResourceContent:
		return "Content"
	case ResourceSnapshot:
		return "Snapshot"
	case ResourceContainer:
		return "Container"
	case ResourceTask:
		return "Task"
	case ResourceImage:
		return "Image"
	case ResourceLease:
		return "Lease"
	case ResourceIngest:
		return "Ingest"
	default:
		return "Unknown"
	}
}

// =============================================================================
// 2. Node — GC 그래프의 노드
// =============================================================================

// Node는 GC 그래프에서 하나의 리소스를 나타낸다.
// 실제: pkg/gc/gc.go — Node 구조체
//
//	type Node struct {
//	    Type      ResourceType
//	    Namespace string
//	    Key       string
//	}
type Node struct {
	Type      ResourceType
	Namespace string
	Key       string
}

func (n Node) String() string {
	return fmt.Sprintf("%s/%s/%s", n.Namespace, n.Type, n.Key)
}

// =============================================================================
// 3. Resource — 메타데이터 스토어의 리소스
// =============================================================================

// Resource는 메타데이터 스토어에 저장된 리소스이다.
// Labels에 GC 참조 정보가 포함된다.
type Resource struct {
	Node   Node
	Labels map[string]string
}

// GC 참조 레이블 상수
// 실제: core/metadata/gc.go에서 정의
const (
	// labelGCRoot는 루트 객체를 표시한다.
	// 이 레이블이 있으면 다른 루트가 참조하지 않아도 GC 대상에서 제외
	LabelGCRoot = "containerd.io/gc.root"

	// labelGCRef 접두사 — 다른 리소스를 참조
	// 실제: labelGCRef = []byte("containerd.io/gc.ref.")
	LabelGCRefContent  = "containerd.io/gc.ref.content"    // 콘텐트 참조
	LabelGCRefSnapshot = "containerd.io/gc.ref.snapshot."  // 스냅샷 참조 (접두사+snapshotter)
	LabelGCRefImage    = "containerd.io/gc.ref.image"      // 이미지 참조

	// labelGCExpire — 만료 시간 (RFC3339)
	// 이미지에 설정 시 만료 후 GC 대상이 됨
	LabelGCExpire = "containerd.io/gc.expire"

	// labelGCFlat — 직접 참조만 보호 (자식 참조 무시)
	LabelGCFlat = "containerd.io/gc.flat"
)

// =============================================================================
// 4. MetadataStore — 메타데이터 스토어 시뮬레이션
// =============================================================================

// MetadataStore는 모든 리소스를 관리하는 메타데이터 스토어이다.
type MetadataStore struct {
	resources map[Node]*Resource
}

func NewMetadataStore() *MetadataStore {
	return &MetadataStore{
		resources: make(map[Node]*Resource),
	}
}

// Add는 리소스를 스토어에 추가한다.
func (s *MetadataStore) Add(r Resource) {
	s.resources[r.Node] = &r
}

// Remove는 리소스를 스토어에서 삭제한다.
func (s *MetadataStore) Remove(n Node) {
	delete(s.resources, n)
}

// Get은 리소스를 조회한다.
func (s *MetadataStore) Get(n Node) (*Resource, bool) {
	r, ok := s.resources[n]
	return r, ok
}

// All은 모든 리소스 노드를 반환한다.
func (s *MetadataStore) All() []Node {
	var nodes []Node
	for n := range s.resources {
		nodes = append(nodes, n)
	}
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].String() < nodes[j].String()
	})
	return nodes
}

// =============================================================================
// 5. Tricolor Mark-Sweep — 핵심 GC 알고리즘
// =============================================================================

// Tricolor는 tri-color mark-sweep GC를 구현한다.
// 실제: pkg/gc/gc.go — Tricolor(roots, refs)
//
// 알고리즘 (위키피디아 tri-color marking):
//   1. 모든 root를 gray 스택에 넣고 seen에 추가
//   2. gray 스택에서 노드를 꺼내 (depth-first):
//      a. 해당 노드의 참조를 조회 (refs 함수)
//      b. 아직 안 본 참조를 gray에 추가
//      c. 현재 노드를 black(reachable)으로 이동
//   3. gray 스택이 빌 때까지 반복
//   4. reachable에 없는 노드 = white = 수거 대상
//
// 핵심 코드 (pkg/gc/gc.go):
//
//	grays = append(grays, roots...)
//	for len(grays) > 0 {
//	    id := grays[len(grays)-1]          // depth-first
//	    grays = grays[:len(grays)-1]
//	    rs, err := refs(id)
//	    for _, target := range rs {
//	        if _, ok := seen[target]; !ok {
//	            grays = append(grays, target)
//	            seen[target] = struct{}{}
//	        }
//	    }
//	    reachable[id] = struct{}{}           // mark as black
//	}
func Tricolor(roots []Node, refs func(Node) ([]Node, error)) (map[Node]struct{}, error) {
	var (
		grays     []Node                // gray 스택 (탐색 대기)
		seen      = map[Node]struct{}{} // 발견된 노드 (non-white)
		reachable = map[Node]struct{}{} // black 노드 (도달 가능)
	)

	// 1. 모든 root를 gray에 추가
	grays = append(grays, roots...)
	for _, root := range roots {
		seen[root] = struct{}{}
	}

	// 2. gray 스택이 빌 때까지 탐색
	for len(grays) > 0 {
		// depth-first: 스택의 마지막 원소
		id := grays[len(grays)-1]
		grays = grays[:len(grays)-1]

		// 참조 조회
		rs, err := refs(id)
		if err != nil {
			return nil, err
		}

		// 아직 안 본 참조를 gray에 추가
		for _, target := range rs {
			if _, ok := seen[target]; !ok {
				grays = append(grays, target)
				seen[target] = struct{}{}
			}
		}

		// black으로 이동 (도달 가능)
		reachable[id] = struct{}{}
	}

	return reachable, nil
}

// Sweep은 reachable에 없는 노드를 제거한다.
// 실제: pkg/gc/gc.go — Sweep(reachable, all, remove)
func Sweep(reachable map[Node]struct{}, all []Node, remove func(Node) error) error {
	for _, node := range all {
		if _, ok := reachable[node]; !ok {
			if err := remove(node); err != nil {
				return err
			}
		}
	}
	return nil
}

// =============================================================================
// 6. Root 수집 및 참조 추적
// =============================================================================

// scanRoots는 GC root 노드를 수집한다.
// 실제: core/metadata/gc.go — scanRoots(ctx, tx, nc)
//
// Root가 되는 리소스:
//   - Image: 만료되지 않은 이미지 (gc.expire 레이블이 없거나 미만료)
//   - Container: 모든 컨테이너
//   - Lease: 모든 리스 (만료되지 않은)
//   - Content/Snapshot: gc.root 레이블이 있는 경우
func scanRoots(store *MetadataStore) []Node {
	var roots []Node
	now := time.Now()

	for _, r := range store.resources {
		switch r.Node.Type {
		case ResourceImage:
			// 이미지는 기본적으로 root
			// gc.expire 레이블이 있으면 만료 확인
			if expStr, ok := r.Labels[LabelGCExpire]; ok {
				exp, err := time.Parse(time.RFC3339, expStr)
				if err == nil && now.After(exp) {
					continue // 만료된 이미지는 root에서 제외
				}
			}
			roots = append(roots, r.Node)

		case ResourceContainer:
			// 컨테이너는 항상 root
			roots = append(roots, r.Node)

		case ResourceLease:
			// 리스는 항상 root (만료 체크는 생략)
			roots = append(roots, r.Node)

		default:
			// Content, Snapshot 등: gc.root 레이블이 있으면 root
			if _, ok := r.Labels[LabelGCRoot]; ok {
				roots = append(roots, r.Node)
			}
		}
	}

	sort.Slice(roots, func(i, j int) bool {
		return roots[i].String() < roots[j].String()
	})
	return roots
}

// references는 노드가 참조하는 다른 노드를 반환한다.
// 실제: core/metadata/gc.go — references(ctx, tx, node, fn)
//
// 참조 규칙:
//   - Image → target content (digest)
//   - Container → snapshot (snapshotter/key)
//   - Content → gc.ref.content, gc.ref.snapshot.* 레이블
//   - Snapshot → parent snapshot
//   - Lease → leased resources (content, snapshot, image)
func references(store *MetadataStore, node Node) ([]Node, error) {
	r, ok := store.Get(node)
	if !ok {
		return nil, nil
	}

	var refs []Node

	// gc.ref.content 레이블 → Content 참조
	for k, v := range r.Labels {
		if k == LabelGCRefContent || strings.HasPrefix(k, LabelGCRefContent+".") ||
			strings.HasPrefix(k, LabelGCRefContent+"/") {
			refs = append(refs, Node{Type: ResourceContent, Namespace: node.Namespace, Key: v})
		}
		if strings.HasPrefix(k, LabelGCRefSnapshot) {
			// gc.ref.snapshot.{snapshotter} → Snapshot 참조
			snapshotter := strings.TrimPrefix(k, LabelGCRefSnapshot)
			if idx := strings.IndexByte(snapshotter, '/'); idx >= 0 {
				snapshotter = snapshotter[:idx]
			}
			refs = append(refs, Node{Type: ResourceSnapshot, Namespace: node.Namespace, Key: snapshotter + "/" + v})
		}
		if k == LabelGCRefImage || strings.HasPrefix(k, LabelGCRefImage+".") {
			refs = append(refs, Node{Type: ResourceImage, Namespace: node.Namespace, Key: v})
		}
	}

	// 타입별 고유 참조
	switch node.Type {
	case ResourceImage:
		// Image → target content
		if target, ok := r.Labels["target.digest"]; ok {
			refs = append(refs, Node{Type: ResourceContent, Namespace: node.Namespace, Key: target})
		}
	case ResourceContainer:
		// Container → snapshot
		if ss, ok := r.Labels["snapshotter"]; ok {
			if sk, ok2 := r.Labels["snapshotKey"]; ok2 {
				refs = append(refs, Node{Type: ResourceSnapshot, Namespace: node.Namespace, Key: ss + "/" + sk})
			}
		}
	case ResourceSnapshot:
		// Snapshot → parent snapshot
		if parent, ok := r.Labels["parent"]; ok {
			parts := strings.SplitN(node.Key, "/", 2)
			if len(parts) == 2 {
				refs = append(refs, Node{Type: ResourceSnapshot, Namespace: node.Namespace, Key: parts[0] + "/" + parent})
			}
		}
	case ResourceLease:
		// Lease → leased resources
		for k, v := range r.Labels {
			if strings.HasPrefix(k, "lease.resource.content/") {
				refs = append(refs, Node{Type: ResourceContent, Namespace: node.Namespace, Key: v})
			}
			if strings.HasPrefix(k, "lease.resource.snapshot/") {
				refs = append(refs, Node{Type: ResourceSnapshot, Namespace: node.Namespace, Key: v})
			}
			if strings.HasPrefix(k, "lease.resource.image/") {
				refs = append(refs, Node{Type: ResourceImage, Namespace: node.Namespace, Key: v})
			}
		}
	}

	return refs, nil
}

// =============================================================================
// 7. GC 스케줄러 — mutation 기반 트리거
// =============================================================================

// GCScheduler는 mutation count 기반으로 GC를 스케줄한다.
// 실제: plugins/gc/scheduler.go — gcScheduler
//
// 설정값:
//   - MutationThreshold: 100 (기본값) — 이 횟수 이상 mutation 시 다음 GC 실행
//   - DeletionThreshold: 0 (기본값) — 삭제 기반 즉시 스케줄 임계값
//   - PauseThreshold: 0.02 — GC 일시정지 비율 (전체 시간의 2%)
//   - ScheduleDelay: 0ms — 스케줄 후 실제 GC까지 지연
type GCScheduler struct {
	mutationThreshold int
	deletionThreshold int

	mutations atomic.Int32
	deletions atomic.Int32

	gcFunc func() // GC 실행 함수
}

func NewGCScheduler(mutationThreshold, deletionThreshold int, gcFunc func()) *GCScheduler {
	return &GCScheduler{
		mutationThreshold: mutationThreshold,
		deletionThreshold: deletionThreshold,
		gcFunc:            gcFunc,
	}
}

// MutationCallback은 DB.RegisterMutationCallback에 등록되는 콜백이다.
// 실제: plugins/gc/scheduler.go — mutationCallback(dirty bool)
//
//	func (s *gcScheduler) mutationCallback(dirty bool) {
//	    e := mutationEvent{ts: time.Now(), mutation: true, dirty: dirty}
//	    go func() { s.eventC <- e }()
//	}
func (s *GCScheduler) MutationCallback(dirty bool) {
	s.mutations.Add(1)
	if dirty {
		s.deletions.Add(1)
	}

	// 임계값 확인 — 초과 시 GC 트리거
	// 실제: run() 루프에서 mutationThreshold, deletionThreshold 확인
	shouldGC := false
	if s.deletionThreshold > 0 && int(s.deletions.Load()) >= s.deletionThreshold {
		shouldGC = true
	}
	if s.mutationThreshold > 0 && int(s.mutations.Load()) >= s.mutationThreshold {
		shouldGC = true
	}

	if shouldGC {
		fmt.Printf("  [스케줄러] GC 트리거 (mutations=%d, deletions=%d)\n",
			s.mutations.Load(), s.deletions.Load())
		s.gcFunc()
		s.mutations.Store(0)
		s.deletions.Store(0)
	}
}

// =============================================================================
// 8. GarbageCollect — 전체 GC 프로세스
// =============================================================================

// GarbageCollect는 containerd의 전체 GC 프로세스를 시뮬레이션한다.
// 실제: core/metadata/db.go — GarbageCollect(ctx)
//
// 프로세스:
//   1. wlock.Lock() — 쓰기 차단
//   2. Mark: getMarked() → scanRoots() + Tricolor() 으로 도달 가능 노드 수집
//   3. Sweep: scanAll() + 미마크 노드 삭제
//   4. dirty 리셋
//   5. wlock.Unlock()
func GarbageCollect(store *MetadataStore, wlock *sync.RWMutex) (marked map[Node]struct{}, removed []Node) {
	fmt.Println("\n  [GC] === GC 시작 ===")
	t1 := time.Now()

	// 1. wlock.Lock() — 쓰기 차단
	wlock.Lock()
	defer wlock.Unlock()
	fmt.Println("  [GC] wlock.Lock() 획득")

	// 2. Mark 단계 — root에서 도달 가능한 노드 수집
	fmt.Println("  [GC] Mark 단계:")
	roots := scanRoots(store)
	fmt.Printf("  [GC]   Root 노드 %d개:\n", len(roots))
	for _, r := range roots {
		fmt.Printf("  [GC]     %s\n", r)
	}

	refsFn := func(n Node) ([]Node, error) {
		return references(store, n)
	}
	marked, err := Tricolor(roots, refsFn)
	if err != nil {
		fmt.Printf("  [GC]   Mark 에러: %v\n", err)
		return nil, nil
	}
	fmt.Printf("  [GC]   도달 가능 노드 %d개\n", len(marked))

	// 3. Sweep 단계 — 미마크 노드 삭제
	fmt.Println("  [GC] Sweep 단계:")
	all := store.All()

	Sweep(marked, all, func(n Node) error {
		fmt.Printf("  [GC]   삭제: %s\n", n)
		store.Remove(n)
		removed = append(removed, n)
		return nil
	})

	elapsed := time.Since(t1)
	fmt.Printf("  [GC] === GC 완료: %d개 삭제, 소요시간=%v ===\n", len(removed), elapsed)

	return marked, removed
}

// =============================================================================
// 9. 메인 — 데모 실행
// =============================================================================

func main() {
	fmt.Println("=== containerd Tricolor GC 알고리즘 시뮬레이션 ===")
	fmt.Println()

	var wlock sync.RWMutex

	// --- 데모 1: 기본 Tricolor GC ---
	fmt.Println("--- 데모 1: 기본 Tricolor Mark-Sweep ---")
	store := NewMetadataStore()

	// 리소스 그래프 구축:
	// Image:nginx → Content:sha256:manifest → Content:sha256:layer1
	//                                       → Content:sha256:layer2
	// Image:nginx → Snapshot:overlayfs/snap-1 → Snapshot:overlayfs/snap-0 (parent)
	// Container:c1 → Snapshot:overlayfs/snap-1
	// Content:sha256:orphan (누구도 참조하지 않음 → GC 대상)
	// Snapshot:overlayfs/snap-orphan (누구도 참조하지 않음 → GC 대상)

	ns := "default"

	store.Add(Resource{
		Node: Node{Type: ResourceImage, Namespace: ns, Key: "docker.io/nginx:latest"},
		Labels: map[string]string{
			"target.digest":                    "sha256:manifest",
			"containerd.io/gc.ref.snapshot.overlayfs": "snap-1",
		},
	})
	store.Add(Resource{
		Node: Node{Type: ResourceContent, Namespace: ns, Key: "sha256:manifest"},
		Labels: map[string]string{
			"containerd.io/gc.ref.content.0": "sha256:layer1",
			"containerd.io/gc.ref.content.1": "sha256:layer2",
		},
	})
	store.Add(Resource{
		Node:   Node{Type: ResourceContent, Namespace: ns, Key: "sha256:layer1"},
		Labels: map[string]string{},
	})
	store.Add(Resource{
		Node:   Node{Type: ResourceContent, Namespace: ns, Key: "sha256:layer2"},
		Labels: map[string]string{},
	})
	store.Add(Resource{
		Node:   Node{Type: ResourceSnapshot, Namespace: ns, Key: "overlayfs/snap-1"},
		Labels: map[string]string{"parent": "snap-0"},
	})
	store.Add(Resource{
		Node:   Node{Type: ResourceSnapshot, Namespace: ns, Key: "overlayfs/snap-0"},
		Labels: map[string]string{},
	})
	store.Add(Resource{
		Node: Node{Type: ResourceContainer, Namespace: ns, Key: "c1"},
		Labels: map[string]string{
			"snapshotter": "overlayfs",
			"snapshotKey": "snap-1",
		},
	})

	// 고아 리소스 (참조되지 않음)
	store.Add(Resource{
		Node:   Node{Type: ResourceContent, Namespace: ns, Key: "sha256:orphan"},
		Labels: map[string]string{},
	})
	store.Add(Resource{
		Node:   Node{Type: ResourceSnapshot, Namespace: ns, Key: "overlayfs/snap-orphan"},
		Labels: map[string]string{},
	})

	fmt.Println("리소스 그래프:")
	fmt.Println("  Image:nginx → Content:manifest → Content:layer1, Content:layer2")
	fmt.Println("  Image:nginx → Snapshot:overlayfs/snap-1 → Snapshot:overlayfs/snap-0")
	fmt.Println("  Container:c1 → Snapshot:overlayfs/snap-1")
	fmt.Println("  Content:sha256:orphan (고아)")
	fmt.Println("  Snapshot:overlayfs/snap-orphan (고아)")
	fmt.Printf("  전체 리소스: %d개\n", len(store.resources))

	marked, removed := GarbageCollect(store, &wlock)
	fmt.Printf("\n  도달 가능: %d개, 삭제: %d개, 남은 리소스: %d개\n",
		len(marked), len(removed), len(store.resources))
	fmt.Println()

	// --- 데모 2: 이미지 만료 GC ---
	fmt.Println("--- 데모 2: 이미지 만료 (gc.expire) ---")
	store2 := NewMetadataStore()

	// 만료된 이미지 — root에서 제외됨
	expiredTime := time.Now().Add(-1 * time.Hour).Format(time.RFC3339)
	store2.Add(Resource{
		Node: Node{Type: ResourceImage, Namespace: ns, Key: "expired-image"},
		Labels: map[string]string{
			LabelGCExpire:   expiredTime,
			"target.digest": "sha256:expired-content",
		},
	})
	store2.Add(Resource{
		Node:   Node{Type: ResourceContent, Namespace: ns, Key: "sha256:expired-content"},
		Labels: map[string]string{},
	})

	// 유효한 이미지
	futureTime := time.Now().Add(24 * time.Hour).Format(time.RFC3339)
	store2.Add(Resource{
		Node: Node{Type: ResourceImage, Namespace: ns, Key: "valid-image"},
		Labels: map[string]string{
			LabelGCExpire:   futureTime,
			"target.digest": "sha256:valid-content",
		},
	})
	store2.Add(Resource{
		Node:   Node{Type: ResourceContent, Namespace: ns, Key: "sha256:valid-content"},
		Labels: map[string]string{},
	})

	// 만료 없는 이미지 (영구)
	store2.Add(Resource{
		Node: Node{Type: ResourceImage, Namespace: ns, Key: "permanent-image"},
		Labels: map[string]string{
			"target.digest": "sha256:permanent-content",
		},
	})
	store2.Add(Resource{
		Node:   Node{Type: ResourceContent, Namespace: ns, Key: "sha256:permanent-content"},
		Labels: map[string]string{},
	})

	fmt.Println("이미지 상태:")
	fmt.Printf("  expired-image: gc.expire=%s (과거 → root 제외)\n", expiredTime)
	fmt.Printf("  valid-image: gc.expire=%s (미래 → root 유지)\n", futureTime)
	fmt.Println("  permanent-image: gc.expire 없음 (영구 → root 유지)")

	marked2, removed2 := GarbageCollect(store2, &wlock)
	fmt.Printf("\n  도달 가능: %d개, 삭제: %d개\n", len(marked2), len(removed2))
	fmt.Println()

	// --- 데모 3: gc.root 레이블 ---
	fmt.Println("--- 데모 3: gc.root 레이블로 콘텐트 보호 ---")
	store3 := NewMetadataStore()

	// gc.root 레이블이 있는 콘텐트 → root로 취급
	store3.Add(Resource{
		Node: Node{Type: ResourceContent, Namespace: ns, Key: "sha256:protected"},
		Labels: map[string]string{
			LabelGCRoot: "true",
			"containerd.io/gc.ref.content": "sha256:protected-child",
		},
	})
	store3.Add(Resource{
		Node:   Node{Type: ResourceContent, Namespace: ns, Key: "sha256:protected-child"},
		Labels: map[string]string{},
	})

	// gc.root가 없는 콘텐트 → GC 대상
	store3.Add(Resource{
		Node:   Node{Type: ResourceContent, Namespace: ns, Key: "sha256:unprotected"},
		Labels: map[string]string{},
	})

	fmt.Println("리소스:")
	fmt.Println("  Content:protected (gc.root=true) → Content:protected-child")
	fmt.Println("  Content:unprotected (gc.root 없음)")

	marked3, removed3 := GarbageCollect(store3, &wlock)
	fmt.Printf("\n  도달 가능: %d개, 삭제: %d개\n", len(marked3), len(removed3))
	fmt.Println()

	// --- 데모 4: GC 스케줄러 ---
	fmt.Println("--- 데모 4: GC 스케줄러 (mutation 기반 트리거) ---")
	store4 := NewMetadataStore()
	store4.Add(Resource{
		Node: Node{Type: ResourceImage, Namespace: ns, Key: "img1"},
		Labels: map[string]string{"target.digest": "sha256:c1"},
	})
	store4.Add(Resource{
		Node:   Node{Type: ResourceContent, Namespace: ns, Key: "sha256:c1"},
		Labels: map[string]string{},
	})

	gcCount := 0
	scheduler := NewGCScheduler(5, 3, func() {
		gcCount++
		fmt.Printf("    >>> GC 실행 #%d\n", gcCount)
		GarbageCollect(store4, &wlock)
	})

	fmt.Println("mutationThreshold=5, deletionThreshold=3")
	fmt.Println("mutation 이벤트 전송 (dirty=false):")
	for i := 0; i < 6; i++ {
		fmt.Printf("  mutation %d: ", i+1)
		scheduler.MutationCallback(false)
		if i < 4 {
			fmt.Println("(임계값 미달)")
		}
	}

	fmt.Println("\n삭제 이벤트 전송 (dirty=true):")
	scheduler.mutations.Store(0)
	scheduler.deletions.Store(0)
	// 고아 콘텐트 추가
	store4.Add(Resource{
		Node:   Node{Type: ResourceContent, Namespace: ns, Key: "sha256:orphan-sched"},
		Labels: map[string]string{},
	})
	for i := 0; i < 4; i++ {
		fmt.Printf("  deletion %d: ", i+1)
		scheduler.MutationCallback(true)
		if i < 2 {
			fmt.Println("(임계값 미달)")
		}
	}
	fmt.Println()

	// --- 데모 5: Tricolor 색상 추적 ---
	fmt.Println("--- 데모 5: Tricolor 색상 변화 추적 ---")
	store5 := NewMetadataStore()

	store5.Add(Resource{
		Node: Node{Type: ResourceImage, Namespace: ns, Key: "root"},
		Labels: map[string]string{
			"target.digest":                 "sha256:A",
			"containerd.io/gc.ref.content":  "sha256:B",
		},
	})
	store5.Add(Resource{
		Node: Node{Type: ResourceContent, Namespace: ns, Key: "sha256:A"},
		Labels: map[string]string{
			"containerd.io/gc.ref.content": "sha256:C",
		},
	})
	store5.Add(Resource{
		Node:   Node{Type: ResourceContent, Namespace: ns, Key: "sha256:B"},
		Labels: map[string]string{},
	})
	store5.Add(Resource{
		Node:   Node{Type: ResourceContent, Namespace: ns, Key: "sha256:C"},
		Labels: map[string]string{},
	})
	store5.Add(Resource{
		Node:   Node{Type: ResourceContent, Namespace: ns, Key: "sha256:D"},
		Labels: map[string]string{}, // 참조되지 않음
	})

	fmt.Println("그래프: Image:root → Content:A → Content:C")
	fmt.Println("                   → Content:B")
	fmt.Println("        Content:D (고아)")
	fmt.Println()

	// 수동 Tricolor 추적
	roots := scanRoots(store5)
	fmt.Printf("초기 상태:\n")
	fmt.Printf("  White (전체): {root, A, B, C, D}\n")
	fmt.Printf("  Gray  (root): {%s}\n", formatNodes(roots))
	fmt.Printf("  Black: {}\n\n")

	step := 0
	refsFn := func(n Node) ([]Node, error) {
		rs, err := references(store5, n)
		step++
		fmt.Printf("  Step %d: %s를 Gray에서 꺼냄 → 참조: %s → Black으로 이동\n",
			step, n, formatNodes(rs))
		return rs, err
	}

	reachable, _ := Tricolor(roots, refsFn)
	fmt.Printf("\n최종 상태:\n")
	fmt.Printf("  Black (도달 가능): %s\n", formatNodeSet(reachable))

	white := []string{}
	for _, n := range store5.All() {
		if _, ok := reachable[n]; !ok {
			white = append(white, n.Key)
		}
	}
	fmt.Printf("  White (수거 대상): {%s}\n", strings.Join(white, ", "))
}

// =============================================================================
// 헬퍼 함수
// =============================================================================

func formatNodes(nodes []Node) string {
	if len(nodes) == 0 {
		return "(없음)"
	}
	var parts []string
	for _, n := range nodes {
		parts = append(parts, n.Key)
	}
	return strings.Join(parts, ", ")
}

func formatNodeSet(set map[Node]struct{}) string {
	var parts []string
	for n := range set {
		parts = append(parts, n.Key)
	}
	sort.Strings(parts)
	return "{" + strings.Join(parts, ", ") + "}"
}
