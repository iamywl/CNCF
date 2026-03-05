package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Kubernetes 가비지 컬렉션 (OwnerReference 기반) 시뮬레이션
// =============================================================================
//
// 실제 소스 참조:
//   - pkg/controller/garbagecollector/garbagecollector.go : GarbageCollector 컨트롤러
//   - pkg/controller/garbagecollector/graph.go            : node 구조체 (의존성 그래프 노드)
//   - pkg/controller/garbagecollector/graph_builder.go    : GraphBuilder (이벤트→그래프 갱신)
//   - staging/src/k8s.io/apimachinery/pkg/apis/meta/v1/types.go : OwnerReference
//
// GC 아키텍처:
//   1. GraphBuilder: informer 이벤트를 받아 의존성 그래프 구축 (단일 스레드)
//   2. attemptToDelete 큐: 삭제 가능한 노드를 enqueue
//   3. attemptToOrphan 큐: 고아 처리할 노드를 enqueue
//   4. Worker: 큐에서 꺼내 API 서버에 삭제/업데이트 요청
//
// 삭제 정책 (PropagationPolicy):
//   - Foreground: 의존 리소스 먼저 삭제 → owner에 DeletionTimestamp 설정
//     실제: owner에 foregroundDeletion finalizer 추가, dependents 삭제 후 owner 삭제
//   - Background: owner 즉시 삭제 → GC가 비동기로 dependents 삭제
//     실제: owner 즉시 삭제, GraphBuilder가 dependents를 attemptToDelete에 enqueue
//   - Orphan: owner 삭제 → dependents의 ownerReference 제거 (살아남음)
//     실제: owner에 orphan finalizer 추가, dependents의 ownerRef 제거 후 owner 삭제

// =============================================================================
// 1. 데이터 모델
// =============================================================================

// UID는 오브젝트 고유 식별자
type UID string

// OwnerReference는 소유자 참조
// 실제: staging/src/k8s.io/apimachinery/pkg/apis/meta/v1/types.go의 OwnerReference
type OwnerReference struct {
	APIVersion         string
	Kind               string
	Name               string
	UID                UID
	BlockOwnerDeletion bool // true면 foreground 삭제에서 이 dependent가 삭제될 때까지 owner 삭제 차단
}

// PropagationPolicy는 삭제 전파 정책
type PropagationPolicy string

const (
	DeletePropagationForeground PropagationPolicy = "Foreground"
	DeletePropagationBackground PropagationPolicy = "Background"
	DeletePropagationOrphan     PropagationPolicy = "Orphan"
)

// Resource는 Kubernetes 리소스를 나타냄
type Resource struct {
	APIVersion      string
	Kind            string
	Name            string
	Namespace       string
	UID             UID
	OwnerReferences []OwnerReference
	DeletionTimestamp *time.Time    // nil이면 삭제 요청 안 됨
	Finalizers      []string       // foregroundDeletion, orphan
}

func (r *Resource) String() string {
	return fmt.Sprintf("%s/%s(%s)", r.Kind, r.Name, r.UID)
}

// =============================================================================
// 2. 의존성 그래프 노드 — pkg/controller/garbagecollector/graph.go 재현
// =============================================================================

// objectReference는 그래프 노드의 식별 정보
// 실제: graph.go의 objectReference (OwnerReference + Namespace)
type objectReference struct {
	OwnerReference
	Namespace string
}

func (o objectReference) String() string {
	return fmt.Sprintf("[%s/%s, namespace: %s, name: %s, uid: %s]",
		o.APIVersion, o.Kind, o.Namespace, o.Name, o.UID)
}

// node는 의존성 그래프의 노드
// 실제: graph.go:63의 node 구조체
// 핵심 필드:
//   - identity: 노드 식별 정보
//   - dependents: 이 노드를 owner로 참조하는 노드 집합
//   - owners: 이 노드의 ownerReference 목록
//   - beingDeleted: deletionTimestamp가 설정되었는지
//   - deletingDependents: foreground 삭제 중인지
type node struct {
	mu               sync.RWMutex
	identity         objectReference
	dependents       map[*node]struct{} // 이 노드에 의존하는 노드들
	owners           []OwnerReference   // 이 노드의 소유자들
	beingDeleted     bool               // deletionTimestamp != nil
	deletingDependents bool             // foreground 삭제 진행 중
	virtual          bool               // informer 이벤트 없이 생성된 가상 노드
}

func (n *node) String() string {
	return fmt.Sprintf("%s/%s(%s)", n.identity.Kind, n.identity.Name, n.identity.UID)
}

// =============================================================================
// 3. 의존성 그래프 — GraphBuilder 역할 통합
// =============================================================================

// DependencyGraph는 오브젝트 간 소유 관계 그래프
// 실제: pkg/controller/garbagecollector/graph_builder.go의 GraphBuilder
// GraphBuilder는 informer 이벤트(Add/Update/Delete)를 받아 그래프를 갱신한다.
type DependencyGraph struct {
	mu    sync.RWMutex
	nodes map[UID]*node // UID → 그래프 노드
}

func NewDependencyGraph() *DependencyGraph {
	return &DependencyGraph{
		nodes: make(map[UID]*node),
	}
}

// AddResource는 리소스를 그래프에 추가한다
// 실제: graph_builder.go의 processGraphChanges() → addNode + addDependents
func (g *DependencyGraph) AddResource(r *Resource) {
	g.mu.Lock()
	defer g.mu.Unlock()

	n := &node{
		identity: objectReference{
			OwnerReference: OwnerReference{
				APIVersion: r.APIVersion,
				Kind:       r.Kind,
				Name:       r.Name,
				UID:        r.UID,
			},
			Namespace: r.Namespace,
		},
		dependents: make(map[*node]struct{}),
		owners:     r.OwnerReferences,
	}

	g.nodes[r.UID] = n

	// 소유자 노드에 dependent 등록
	for _, ownerRef := range r.OwnerReferences {
		if owner, ok := g.nodes[ownerRef.UID]; ok {
			owner.mu.Lock()
			owner.dependents[n] = struct{}{}
			owner.mu.Unlock()
		}
	}
}

// GetNode는 UID로 노드를 반환한다
func (g *DependencyGraph) GetNode(uid UID) *node {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.nodes[uid]
}

// RemoveNode는 노드를 그래프에서 제거한다
func (g *DependencyGraph) RemoveNode(uid UID) {
	g.mu.Lock()
	defer g.mu.Unlock()

	n, ok := g.nodes[uid]
	if !ok {
		return
	}

	// 소유자 노드의 dependents에서 제거
	for _, ownerRef := range n.owners {
		if owner, ok := g.nodes[ownerRef.UID]; ok {
			owner.mu.Lock()
			delete(owner.dependents, n)
			owner.mu.Unlock()
		}
	}

	delete(g.nodes, uid)
}

// GetDependents는 노드의 모든 의존자를 반환한다
func (g *DependencyGraph) GetDependents(uid UID) []*node {
	g.mu.RLock()
	n, ok := g.nodes[uid]
	g.mu.RUnlock()

	if !ok {
		return nil
	}

	n.mu.RLock()
	defer n.mu.RUnlock()

	deps := make([]*node, 0, len(n.dependents))
	for dep := range n.dependents {
		deps = append(deps, dep)
	}
	return deps
}

// RemoveOwnerRef는 노드에서 특정 소유자 참조를 제거한다 (orphan용)
func (g *DependencyGraph) RemoveOwnerRef(nodeUID, ownerUID UID) {
	g.mu.Lock()
	defer g.mu.Unlock()

	n, ok := g.nodes[nodeUID]
	if !ok {
		return
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	var newOwners []OwnerReference
	for _, ref := range n.owners {
		if ref.UID != ownerUID {
			newOwners = append(newOwners, ref)
		}
	}
	n.owners = newOwners

	// 소유자 노드의 dependents에서도 제거
	if owner, ok := g.nodes[ownerUID]; ok {
		owner.mu.Lock()
		delete(owner.dependents, n)
		owner.mu.Unlock()
	}
}

// PrintGraph는 전체 그래프를 출력한다
func (g *DependencyGraph) PrintGraph(indent string) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	// 루트 노드 찾기 (소유자가 없는 노드)
	var roots []*node
	for _, n := range g.nodes {
		if len(n.owners) == 0 {
			roots = append(roots, n)
		}
	}

	for _, root := range roots {
		g.printTree(root, indent, "")
	}
}

func (g *DependencyGraph) printTree(n *node, prefix, indent string) {
	status := ""
	if n.beingDeleted {
		status = " [DELETING]"
	}
	if n.deletingDependents {
		status += " [DELETING-DEPENDENTS]"
	}

	ownerInfo := ""
	if len(n.owners) > 0 {
		owners := make([]string, len(n.owners))
		for i, o := range n.owners {
			owners[i] = fmt.Sprintf("%s/%s", o.Kind, o.Name)
		}
		ownerInfo = fmt.Sprintf(" (owner: %s)", strings.Join(owners, ", "))
	}

	fmt.Printf("%s%s%s%s%s\n", prefix, indent, n.String(), ownerInfo, status)

	n.mu.RLock()
	deps := make([]*node, 0, len(n.dependents))
	for dep := range n.dependents {
		deps = append(deps, dep)
	}
	n.mu.RUnlock()

	for i, dep := range deps {
		if i == len(deps)-1 {
			g.printTree(dep, prefix+indent, "  ")
		} else {
			g.printTree(dep, prefix+indent, "  ")
		}
	}
}

// =============================================================================
// 4. GarbageCollector — 삭제 정책 구현
// =============================================================================

// GarbageCollector는 OwnerReference 기반 가비지 컬렉터
// 실제: pkg/controller/garbagecollector/garbagecollector.go의 GarbageCollector
type GarbageCollector struct {
	graph    *DependencyGraph
	resources map[UID]*Resource // 실제 저장소 역할

	// 삭제/고아 처리 큐
	// 실제: attemptToDelete, attemptToOrphan (workqueue.RateLimitingInterface)
	deleteLog []string
}

func NewGarbageCollector() *GarbageCollector {
	return &GarbageCollector{
		graph:     NewDependencyGraph(),
		resources: make(map[UID]*Resource),
	}
}

// Register는 리소스를 등록한다
func (gc *GarbageCollector) Register(r *Resource) {
	gc.resources[r.UID] = r
	gc.graph.AddResource(r)
}

// Delete는 지정된 정책으로 리소스를 삭제한다
// 실제: garbagecollector.go의 attemptToDeleteWorker → attemptToDeleteItem
func (gc *GarbageCollector) Delete(uid UID, policy PropagationPolicy) {
	r, ok := gc.resources[uid]
	if !ok {
		return
	}

	switch policy {
	case DeletePropagationForeground:
		gc.foregroundDelete(r)
	case DeletePropagationBackground:
		gc.backgroundDelete(r)
	case DeletePropagationOrphan:
		gc.orphanDelete(r)
	}
}

// foregroundDelete는 Foreground 삭제를 수행한다
// 실제 동작:
//   1. Owner에 DeletionTimestamp 설정 + foregroundDeletion finalizer 추가
//   2. Owner의 모든 dependents를 먼저 삭제 (cascading)
//   3. 모든 dependents 삭제 완료 후 owner의 finalizer 제거 → owner 삭제
//
// 이 방식은 "owner가 삭제되기 전에 dependents가 확실히 삭제됨"을 보장한다.
// 실제: garbagecollector.go의 attemptToDeleteItem()에서
//   n.isObserved() && n.isDeletingDependents() 체크
func (gc *GarbageCollector) foregroundDelete(r *Resource) {
	gc.log(fmt.Sprintf("[Foreground] %s 삭제 시작 → DeletionTimestamp 설정", r))

	// 1. Owner에 마킹
	now := time.Now()
	r.DeletionTimestamp = &now
	r.Finalizers = append(r.Finalizers, "foregroundDeletion")

	n := gc.graph.GetNode(r.UID)
	if n != nil {
		n.mu.Lock()
		n.beingDeleted = true
		n.deletingDependents = true
		n.mu.Unlock()
	}

	// 2. Dependents 삭제 (재귀적 — dependent의 dependent도 cascading)
	deps := gc.graph.GetDependents(r.UID)
	for _, dep := range deps {
		depRes := gc.resources[dep.identity.UID]
		if depRes != nil {
			// dependent의 dependent도 foreground로 삭제
			gc.foregroundDelete(depRes)
		}
	}

	// 3. 모든 dependents 삭제 후 owner 삭제
	gc.log(fmt.Sprintf("[Foreground] %s의 모든 dependents 삭제 완료 → owner 삭제", r))
	gc.graph.RemoveNode(r.UID)
	delete(gc.resources, r.UID)
}

// backgroundDelete는 Background 삭제를 수행한다
// 실제 동작:
//   1. Owner를 즉시 삭제
//   2. GraphBuilder가 그래프 변경을 감지
//   3. Dependents의 ownerReference가 가리키는 owner가 없어짐
//   4. GC worker가 "absent owner"를 감지하고 dependents를 attemptToDelete에 enqueue
//   5. Dependents 비동기 삭제
//
// 실제: garbagecollector.go의 processItem()에서
//   ownerNode 없으면 → absentOwnerCache 확인 → 삭제
func (gc *GarbageCollector) backgroundDelete(r *Resource) {
	gc.log(fmt.Sprintf("[Background] %s 즉시 삭제", r))

	// 1. Owner 즉시 삭제
	deps := gc.graph.GetDependents(r.UID)
	gc.graph.RemoveNode(r.UID)
	delete(gc.resources, r.UID)

	// 2. Dependents 비동기 삭제 (GC가 absent owner 감지)
	for _, dep := range deps {
		depRes := gc.resources[dep.identity.UID]
		if depRes != nil {
			gc.log(fmt.Sprintf("[Background] GC가 absent owner 감지 → %s 삭제", depRes))
			// dependent의 dependents도 cascading 삭제
			gc.backgroundDelete(depRes)
		}
	}
}

// orphanDelete는 Orphan 삭제를 수행한다
// 실제 동작:
//   1. Owner에 orphan finalizer 추가
//   2. 모든 dependents에서 owner의 OwnerReference 제거
//   3. Dependents는 orphan(고아)으로 살아남음
//   4. Owner의 finalizer 제거 → owner 삭제
//
// 실제: garbagecollector.go의 attemptToOrphanWorker → orphanDependents()
func (gc *GarbageCollector) orphanDelete(r *Resource) {
	gc.log(fmt.Sprintf("[Orphan] %s 삭제 시작 → dependents의 ownerRef 제거", r))

	// 1. Dependents의 ownerReference 제거
	deps := gc.graph.GetDependents(r.UID)
	for _, dep := range deps {
		depRes := gc.resources[dep.identity.UID]
		if depRes != nil {
			gc.log(fmt.Sprintf("[Orphan] %s에서 ownerRef(%s) 제거 → 고아 상태", depRes, r))
			// 리소스에서 ownerRef 제거
			var newRefs []OwnerReference
			for _, ref := range depRes.OwnerReferences {
				if ref.UID != r.UID {
					newRefs = append(newRefs, ref)
				}
			}
			depRes.OwnerReferences = newRefs
			// 그래프에서도 제거
			gc.graph.RemoveOwnerRef(dep.identity.UID, r.UID)
		}
	}

	// 2. Owner 삭제
	gc.log(fmt.Sprintf("[Orphan] %s 삭제 완료 (dependents는 고아로 존속)", r))
	gc.graph.RemoveNode(r.UID)
	delete(gc.resources, r.UID)
}

// GetAllResources는 현재 존재하는 모든 리소스를 반환한다
func (gc *GarbageCollector) GetAllResources() []*Resource {
	result := make([]*Resource, 0, len(gc.resources))
	for _, r := range gc.resources {
		result = append(result, r)
	}
	return result
}

func (gc *GarbageCollector) log(msg string) {
	gc.deleteLog = append(gc.deleteLog, msg)
	fmt.Printf("    %s\n", msg)
}

// =============================================================================
// 5. 데모 헬퍼
// =============================================================================

func printHeader(title string) {
	fmt.Println()
	fmt.Println(strings.Repeat("=", 70))
	fmt.Printf("  %s\n", title)
	fmt.Println(strings.Repeat("=", 70))
}

func printSubHeader(title string) {
	fmt.Printf("\n--- %s ---\n", title)
}

// createDeploymentHierarchy는 Deployment → ReplicaSet → Pod 계층을 생성한다
// Kubernetes에서 가장 전형적인 소유 관계:
//   Deployment → owns → ReplicaSet → owns → Pod
func createDeploymentHierarchy(gc *GarbageCollector, name string, podCount int) (UID, []UID, []UID) {
	depUID := UID(name + "-deploy")
	rsUID := UID(name + "-rs")

	// Deployment
	gc.Register(&Resource{
		APIVersion: "apps/v1", Kind: "Deployment",
		Name: name, Namespace: "default", UID: depUID,
	})

	// ReplicaSet (owned by Deployment)
	gc.Register(&Resource{
		APIVersion: "apps/v1", Kind: "ReplicaSet",
		Name: name + "-rs", Namespace: "default", UID: rsUID,
		OwnerReferences: []OwnerReference{
			{APIVersion: "apps/v1", Kind: "Deployment", Name: name, UID: depUID, BlockOwnerDeletion: true},
		},
	})

	// Pods (owned by ReplicaSet)
	var podUIDs []UID
	for i := 0; i < podCount; i++ {
		podName := fmt.Sprintf("%s-pod-%d", name, i+1)
		podUID := UID(podName)
		gc.Register(&Resource{
			APIVersion: "v1", Kind: "Pod",
			Name: podName, Namespace: "default", UID: podUID,
			OwnerReferences: []OwnerReference{
				{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: name + "-rs", UID: rsUID, BlockOwnerDeletion: true},
			},
		})
		podUIDs = append(podUIDs, podUID)
	}

	return depUID, []UID{rsUID}, podUIDs
}

// =============================================================================
// 6. 메인 — 데모
// =============================================================================

func main() {
	// =====================================================================
	// 데모 1: Foreground 삭제 (dependents 먼저 → owner 나중에)
	// =====================================================================
	printHeader("데모 1: Foreground 삭제")

	gc1 := NewGarbageCollector()
	depUID1, _, _ := createDeploymentHierarchy(gc1, "web", 3)

	printSubHeader("삭제 전 리소스 그래프")
	gc1.graph.PrintGraph("    ")

	printSubHeader("Foreground 삭제 실행 (Deployment 삭제)")
	fmt.Println("  순서: Pod 삭제 → ReplicaSet 삭제 → Deployment 삭제")
	fmt.Println()
	gc1.Delete(depUID1, DeletePropagationForeground)

	printSubHeader("삭제 후 남은 리소스")
	remaining := gc1.GetAllResources()
	if len(remaining) == 0 {
		fmt.Println("    (없음 — 모든 리소스 삭제됨)")
	}

	// =====================================================================
	// 데모 2: Background 삭제 (owner 먼저 → dependents 비동기)
	// =====================================================================
	printHeader("데모 2: Background 삭제")

	gc2 := NewGarbageCollector()
	depUID2, _, _ := createDeploymentHierarchy(gc2, "api", 2)

	printSubHeader("삭제 전 리소스 그래프")
	gc2.graph.PrintGraph("    ")

	printSubHeader("Background 삭제 실행 (Deployment 삭제)")
	fmt.Println("  순서: Deployment 즉시 삭제 → GC가 ReplicaSet 감지/삭제 → GC가 Pod 감지/삭제")
	fmt.Println()
	gc2.Delete(depUID2, DeletePropagationBackground)

	printSubHeader("삭제 후 남은 리소스")
	remaining2 := gc2.GetAllResources()
	if len(remaining2) == 0 {
		fmt.Println("    (없음 — 모든 리소스 삭제됨)")
	}

	// =====================================================================
	// 데모 3: Orphan 삭제 (owner만 삭제, dependents 존속)
	// =====================================================================
	printHeader("데모 3: Orphan 삭제 (고아 정책)")

	gc3 := NewGarbageCollector()
	depUID3, rsUIDs3, podUIDs3 := createDeploymentHierarchy(gc3, "worker", 2)

	printSubHeader("삭제 전 리소스 그래프")
	gc3.graph.PrintGraph("    ")

	printSubHeader("Orphan 삭제 실행 (Deployment 삭제)")
	fmt.Println("  순서: ReplicaSet의 ownerRef 제거 → Deployment 삭제 (Pod는 영향 없음)")
	fmt.Println()
	gc3.Delete(depUID3, DeletePropagationOrphan)

	printSubHeader("삭제 후 남은 리소스")
	remaining3 := gc3.GetAllResources()
	for _, r := range remaining3 {
		ownerInfo := ""
		if len(r.OwnerReferences) > 0 {
			owners := make([]string, len(r.OwnerReferences))
			for i, o := range r.OwnerReferences {
				owners[i] = fmt.Sprintf("%s/%s", o.Kind, o.Name)
			}
			ownerInfo = fmt.Sprintf(" (owner: %s)", strings.Join(owners, ", "))
		} else {
			ownerInfo = " (owner: 없음 — 루트 또는 고아)"
		}
		fmt.Printf("    %s%s\n", r, ownerInfo)
	}

	fmt.Printf("\n  ReplicaSet %s 존속: %v\n", rsUIDs3[0], gc3.resources[rsUIDs3[0]] != nil)
	fmt.Printf("  Pod %s 존속: %v\n", podUIDs3[0], gc3.resources[podUIDs3[0]] != nil)
	fmt.Printf("  Pod %s 존속: %v\n", podUIDs3[1], gc3.resources[podUIDs3[1]] != nil)

	// =====================================================================
	// 데모 4: 복잡한 소유 관계 (다중 owner)
	// =====================================================================
	printHeader("데모 4: 다중 소유자 시나리오")

	gc4 := NewGarbageCollector()

	// ConfigMap → 두 Deployment가 공유
	cmUID := UID("shared-config")
	gc4.Register(&Resource{
		APIVersion: "v1", Kind: "ConfigMap",
		Name: "shared-config", Namespace: "default", UID: cmUID,
	})

	// Deployment A
	depAUID := UID("dep-a")
	gc4.Register(&Resource{
		APIVersion: "apps/v1", Kind: "Deployment",
		Name: "dep-a", Namespace: "default", UID: depAUID,
	})

	// Deployment B
	depBUID := UID("dep-b")
	gc4.Register(&Resource{
		APIVersion: "apps/v1", Kind: "Deployment",
		Name: "dep-b", Namespace: "default", UID: depBUID,
	})

	// Service (owned by both Deployment A and B — 드문 케이스이지만 가능)
	svcUID := UID("shared-svc")
	gc4.Register(&Resource{
		APIVersion: "v1", Kind: "Service",
		Name: "shared-svc", Namespace: "default", UID: svcUID,
		OwnerReferences: []OwnerReference{
			{APIVersion: "apps/v1", Kind: "Deployment", Name: "dep-a", UID: depAUID},
			{APIVersion: "apps/v1", Kind: "Deployment", Name: "dep-b", UID: depBUID},
		},
	})

	printSubHeader("다중 owner 그래프")
	gc4.graph.PrintGraph("    ")

	fmt.Println("\n  Service/shared-svc는 Deployment A와 B 모두를 owner로 가진다.")
	fmt.Println("  Deployment A만 삭제하면, owner B가 남아있으므로 Service는 삭제되지 않는다.")
	fmt.Println("  (GC는 모든 ownerReference가 가리키는 owner가 없어져야 삭제한다)")

	printSubHeader("Deployment A Background 삭제")
	gc4.Delete(depAUID, DeletePropagationBackground)

	svcRes := gc4.resources[svcUID]
	if svcRes != nil {
		fmt.Printf("\n  Service/shared-svc 존속: true (owner B가 아직 존재)\n")
		fmt.Printf("  남은 ownerReferences: ")
		for _, ref := range svcRes.OwnerReferences {
			fmt.Printf("%s/%s ", ref.Kind, ref.Name)
		}
		fmt.Println()
	}

	// =====================================================================
	// 데모 5: 깊은 계층 구조의 Cascading 삭제
	// =====================================================================
	printHeader("데모 5: 깊은 계층 Cascading 삭제")

	gc5 := NewGarbageCollector()

	// CronJob → Job → Pod 계층
	cronUID := UID("backup-cron")
	gc5.Register(&Resource{
		APIVersion: "batch/v1", Kind: "CronJob",
		Name: "backup-cron", Namespace: "default", UID: cronUID,
	})

	for i := 1; i <= 3; i++ {
		jobName := fmt.Sprintf("backup-job-%d", i)
		jobUID := UID(jobName)
		gc5.Register(&Resource{
			APIVersion: "batch/v1", Kind: "Job",
			Name: jobName, Namespace: "default", UID: jobUID,
			OwnerReferences: []OwnerReference{
				{APIVersion: "batch/v1", Kind: "CronJob", Name: "backup-cron", UID: cronUID},
			},
		})

		for j := 1; j <= 2; j++ {
			podName := fmt.Sprintf("%s-pod-%d", jobName, j)
			gc5.Register(&Resource{
				APIVersion: "v1", Kind: "Pod",
				Name: podName, Namespace: "default", UID: UID(podName),
				OwnerReferences: []OwnerReference{
					{APIVersion: "batch/v1", Kind: "Job", Name: jobName, UID: jobUID},
				},
			})
		}
	}

	printSubHeader("삭제 전 리소스 그래프 (CronJob → Job → Pod)")
	gc5.graph.PrintGraph("    ")
	fmt.Printf("\n  총 리소스: %d개 (1 CronJob + 3 Job + 6 Pod)\n", len(gc5.resources))

	printSubHeader("Foreground 삭제 실행 (CronJob 삭제)")
	gc5.Delete(cronUID, DeletePropagationForeground)

	printSubHeader("삭제 후 남은 리소스")
	if len(gc5.resources) == 0 {
		fmt.Println("    (없음 — 전체 계층 삭제됨)")
	}
	fmt.Printf("  삭제 이벤트 총 %d건 발생\n", len(gc5.deleteLog))

	// =====================================================================
	// 요약
	// =====================================================================
	printHeader("요약: Kubernetes 가비지 컬렉션 정책 비교")
	fmt.Println(`
  ┌─────────────────┬──────────────────────────────────┬───────────────────────┐
  │ 정책            │ 삭제 순서                        │ Dependents 운명       │
  ├─────────────────┼──────────────────────────────────┼───────────────────────┤
  │ Foreground      │ Dependents → Owner               │ 먼저 삭제됨           │
  │ Background      │ Owner → Dependents (비동기)       │ GC가 나중에 삭제      │
  │ Orphan          │ Owner만 삭제                      │ 살아남음 (고아)        │
  └─────────────────┴──────────────────────────────────┴───────────────────────┘

  GC 아키텍처 (실제 소스):
  1. GraphBuilder (단일 스레드): informer 이벤트 → 의존성 그래프 갱신
     - graph.go: node{identity, dependents, owners, beingDeleted}
  2. attemptToDelete 큐: 삭제 가능한 노드 enqueue
  3. attemptToOrphan 큐: 고아 처리할 노드 enqueue
  4. Worker: 큐에서 꺼내 API 서버에 요청

  핵심 규칙:
  - 모든 ownerReference의 owner가 없어져야 dependent 삭제 가능
  - BlockOwnerDeletion=true면 foreground 삭제 시 dependent 완료까지 owner 차단
  - finalizer(foregroundDeletion, orphan)로 삭제 순서 보장

  실제 소스 경로:
  - GC 컨트롤러:   pkg/controller/garbagecollector/garbagecollector.go
  - 의존성 그래프:  pkg/controller/garbagecollector/graph.go
  - 그래프 빌더:   pkg/controller/garbagecollector/graph_builder.go
  - OwnerReference: staging/src/k8s.io/apimachinery/pkg/apis/meta/v1/types.go`)
}
