package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Terraform DAG(Directed Acyclic Graph) 엔진 시뮬레이션
// =============================================================================
//
// Terraform는 인프라 리소스 간의 의존 관계를 DAG로 표현하고,
// 위상 정렬(Topological Sort)을 통해 올바른 순서로 리소스를 처리한다.
//
// 실제 Terraform 소스:
//   - internal/dag/dag.go: DAG 구현 (AcyclicGraph)
//   - internal/dag/walk.go: 병렬 워커 (Walker)
//   - internal/dag/tarjan.go: 강연결 컴포넌트 (순환 탐지)
//
// 이 PoC에서 구현하는 핵심 개념:
//   1. 인접 리스트 기반 그래프 (정점 + 간선)
//   2. DFS 기반 위상 정렬
//   3. 순환(Cycle) 탐지
//   4. 병렬 워커 (dag.Walker 패턴)

// =============================================================================
// 1. Vertex(정점) 인터페이스
// =============================================================================

// Vertex는 그래프의 정점을 나타낸다.
// Terraform에서는 dag.Vertex 인터페이스로 정의되며,
// 리소스, 데이터소스, 프로바이더 등 다양한 노드가 구현한다.
type Vertex interface {
	Name() string
}

// ResourceVertex는 인프라 리소스를 나타내는 정점이다.
type ResourceVertex struct {
	ResourceType string // 예: aws_vpc, aws_subnet
	ResourceName string // 예: main, web
	Action       string // 예: create, update, delete
}

func (v *ResourceVertex) Name() string {
	return fmt.Sprintf("%s.%s", v.ResourceType, v.ResourceName)
}

func (v *ResourceVertex) String() string {
	return fmt.Sprintf("[%s] %s", v.Action, v.Name())
}

// =============================================================================
// 2. Edge(간선) 구조체
// =============================================================================

// Edge는 두 정점 간의 의존 관계를 나타낸다.
// Source → Target은 "Source가 Target에 의존한다"는 의미이다.
// 즉 Target이 먼저 생성되어야 한다.
type Edge struct {
	Source Vertex
	Target Vertex
}

// =============================================================================
// 3. DAG(Directed Acyclic Graph) 구현
// =============================================================================

// DAG는 방향 비순환 그래프를 구현한다.
// Terraform의 dag.AcyclicGraph에 대응한다.
//
// 실제 Terraform 구현:
//   - vertices: Set (map[Vertex]struct{})
//   - edges: Set (map[Edge]struct{})
//   - downEdges/upEdges: 인접 리스트 캐시
type DAG struct {
	vertices  []Vertex
	edges     []Edge
	downEdges map[string][]string // 정점 → 의존하는 정점들 (정방향)
	upEdges   map[string][]string // 정점 → 이 정점에 의존하는 정점들 (역방향)
	vertexMap map[string]Vertex   // 이름으로 정점 조회
}

// NewDAG는 새로운 DAG를 생성한다.
func NewDAG() *DAG {
	return &DAG{
		vertices:  make([]Vertex, 0),
		edges:     make([]Edge, 0),
		downEdges: make(map[string][]string),
		upEdges:   make(map[string][]string),
		vertexMap: make(map[string]Vertex),
	}
}

// AddVertex는 그래프에 정점을 추가한다.
func (g *DAG) AddVertex(v Vertex) {
	name := v.Name()
	if _, exists := g.vertexMap[name]; exists {
		return
	}
	g.vertices = append(g.vertices, v)
	g.vertexMap[name] = v
	g.downEdges[name] = make([]string, 0)
	g.upEdges[name] = make([]string, 0)
}

// AddEdge는 두 정점 사이에 간선을 추가한다.
// source가 target에 의존함을 나타낸다 (target → source 순서로 처리).
func (g *DAG) AddEdge(source, target Vertex) error {
	sourceName := source.Name()
	targetName := target.Name()

	// 정점 존재 확인
	if _, exists := g.vertexMap[sourceName]; !exists {
		return fmt.Errorf("소스 정점 %q 을 찾을 수 없습니다", sourceName)
	}
	if _, exists := g.vertexMap[targetName]; !exists {
		return fmt.Errorf("타겟 정점 %q 을 찾을 수 없습니다", targetName)
	}

	g.edges = append(g.edges, Edge{Source: source, Target: target})
	g.downEdges[sourceName] = append(g.downEdges[sourceName], targetName)
	g.upEdges[targetName] = append(g.upEdges[targetName], sourceName)
	return nil
}

// =============================================================================
// 4. 위상 정렬 (Topological Sort)
// =============================================================================

// TopologicalSort는 DFS 기반 위상 정렬을 수행한다.
// 의존 관계를 존중하여 정점의 처리 순서를 결정한다.
//
// Terraform에서는 dag.AcyclicGraph.TopologicalOrder()로 구현된다.
// 실제로는 깊이 우선 탐색(DFS)의 후위 순회(post-order)를 역순으로 사용한다.
func (g *DAG) TopologicalSort() ([]Vertex, error) {
	// 먼저 순환을 검사한다
	cycles := g.DetectCycles()
	if len(cycles) > 0 {
		return nil, fmt.Errorf("그래프에 순환이 발견되었습니다: %v", cycles)
	}

	visited := make(map[string]bool)
	result := make([]Vertex, 0, len(g.vertices))

	// DFS 후위 순회
	var dfs func(name string)
	dfs = func(name string) {
		if visited[name] {
			return
		}
		visited[name] = true

		// 의존하는 정점(하위 정점)을 먼저 방문
		for _, dep := range g.downEdges[name] {
			dfs(dep)
		}

		// 후위 순회: 모든 의존성을 처리한 후 현재 정점 추가
		result = append(result, g.vertexMap[name])
	}

	// 모든 정점에서 DFS 시작
	for _, v := range g.vertices {
		dfs(v.Name())
	}

	return result, nil
}

// =============================================================================
// 5. 순환 탐지 (Cycle Detection)
// =============================================================================

// DetectCycles는 그래프에서 순환을 탐지한다.
// Terraform은 Tarjan의 강연결 컴포넌트(SCC) 알고리즘을 사용한다.
// (internal/dag/tarjan.go 참고)
//
// 여기서는 DFS 기반 3-color 알고리즘으로 구현한다:
//   - white(0): 미방문
//   - gray(1): 현재 경로에서 방문 중
//   - black(2): 완전히 처리됨
func (g *DAG) DetectCycles() [][]string {
	const (
		white = 0 // 미방문
		gray  = 1 // 방문 중 (현재 DFS 경로에 있음)
		black = 2 // 완료
	)

	color := make(map[string]int)
	parent := make(map[string]string)
	var cycles [][]string

	var dfs func(name string)
	dfs = func(name string) {
		color[name] = gray

		for _, dep := range g.downEdges[name] {
			if color[dep] == gray {
				// 순환 발견! 경로를 추적한다.
				cycle := []string{dep, name}
				curr := name
				for curr != dep {
					curr = parent[curr]
					if curr == dep {
						break
					}
					cycle = append(cycle, curr)
				}
				cycles = append(cycles, cycle)
			} else if color[dep] == white {
				parent[dep] = name
				dfs(dep)
			}
		}

		color[name] = black
	}

	for _, v := range g.vertices {
		name := v.Name()
		if color[name] == white {
			dfs(name)
		}
	}

	return cycles
}

// =============================================================================
// 6. 병렬 워커 (Parallel Walker)
// =============================================================================

// WalkFunc는 각 정점을 처리하는 콜백 함수이다.
type WalkFunc func(v Vertex) error

// ParallelWalk는 DAG를 병렬로 순회하며 각 정점에 대해 콜백을 실행한다.
// Terraform의 dag.Walker에 대응한다.
//
// 핵심 알고리즘:
//   1. 의존성이 없는(in-degree = 0) 정점부터 시작
//   2. 정점 처리 완료 시 해당 정점에 의존하는 정점들의 의존성 카운트 감소
//   3. 의존성 카운트가 0이 된 정점은 즉시 병렬 실행
//
// Terraform의 실제 Walker는 더 복잡하다:
//   - Update()로 그래프 동적 변경 가능
//   - Wait()로 완료 대기
//   - 에러 전파 및 정점별 에러 추적
func (g *DAG) ParallelWalk(callback WalkFunc) []error {
	var (
		mu       sync.Mutex
		errors   []error
		wg       sync.WaitGroup
		inDegree = make(map[string]int)
		done     = make(map[string]bool)
	)

	// 각 정점의 진입 차수(in-degree) 계산
	for _, v := range g.vertices {
		name := v.Name()
		inDegree[name] = len(g.downEdges[name])
	}

	// 정점 처리 함수
	var processVertex func(v Vertex)
	processVertex = func(v Vertex) {
		defer wg.Done()

		name := v.Name()

		// 콜백 실행
		if err := callback(v); err != nil {
			mu.Lock()
			errors = append(errors, fmt.Errorf("%s: %w", name, err))
			mu.Unlock()
			return
		}

		mu.Lock()
		done[name] = true
		mu.Unlock()

		// 이 정점에 의존하는 정점들의 의존성 카운트 감소
		mu.Lock()
		dependents := make([]Vertex, 0)
		for _, depName := range g.upEdges[name] {
			inDegree[depName]--
			if inDegree[depName] == 0 && !done[depName] {
				dependents = append(dependents, g.vertexMap[depName])
			}
		}
		mu.Unlock()

		// 의존성이 충족된 정점들을 병렬 실행
		for _, dep := range dependents {
			wg.Add(1)
			go processVertex(dep)
		}
	}

	// 의존성이 없는 정점(루트 노드)부터 시작
	for _, v := range g.vertices {
		name := v.Name()
		if inDegree[name] == 0 {
			wg.Add(1)
			go processVertex(v)
		}
	}

	wg.Wait()
	return errors
}

// =============================================================================
// 7. 그래프 시각화
// =============================================================================

// Visualize는 그래프를 ASCII로 출력한다.
func (g *DAG) Visualize() string {
	var sb strings.Builder
	sb.WriteString("┌─────────────────────────────────────────────┐\n")
	sb.WriteString("│           DAG 그래프 시각화                    │\n")
	sb.WriteString("├─────────────────────────────────────────────┤\n")

	sb.WriteString("│ 정점 (Vertices):                             │\n")
	for _, v := range g.vertices {
		sb.WriteString(fmt.Sprintf("│   ● %s\n", v.Name()))
	}

	sb.WriteString("│                                             │\n")
	sb.WriteString("│ 간선 (Edges): source → target (의존 관계)      │\n")
	for _, e := range g.edges {
		sb.WriteString(fmt.Sprintf("│   %s ──depends──▶ %s\n",
			e.Source.Name(), e.Target.Name()))
	}
	sb.WriteString("└─────────────────────────────────────────────┘\n")
	return sb.String()
}

// =============================================================================
// 메인 함수
// =============================================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════╗")
	fmt.Println("║   Terraform DAG 엔진 시뮬레이션                           ║")
	fmt.Println("║   실제 코드: internal/dag/dag.go, walk.go, tarjan.go     ║")
	fmt.Println("╚══════════════════════════════════════════════════════════╝")
	fmt.Println()

	// =========================================================================
	// 데모 1: 인프라 리소스 의존 그래프 구축
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  데모 1: 인프라 리소스 의존 그래프 구축")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	// 의존 관계:
	//   aws_eip.web ──▶ aws_instance.web ──▶ aws_subnet.main ──▶ aws_vpc.main
	//                   aws_instance.web ──▶ aws_security_group.web ──▶ aws_vpc.main
	//
	// 처리 순서: VPC → (Subnet, SG 병렬) → EC2 → EIP

	g := NewDAG()

	// 정점 추가
	vpc := &ResourceVertex{ResourceType: "aws_vpc", ResourceName: "main", Action: "create"}
	subnet := &ResourceVertex{ResourceType: "aws_subnet", ResourceName: "main", Action: "create"}
	sg := &ResourceVertex{ResourceType: "aws_security_group", ResourceName: "web", Action: "create"}
	ec2 := &ResourceVertex{ResourceType: "aws_instance", ResourceName: "web", Action: "create"}
	eip := &ResourceVertex{ResourceType: "aws_eip", ResourceName: "web", Action: "create"}

	g.AddVertex(vpc)
	g.AddVertex(subnet)
	g.AddVertex(sg)
	g.AddVertex(ec2)
	g.AddVertex(eip)

	// 간선 추가 (의존 관계)
	g.AddEdge(subnet, vpc)  // subnet은 vpc에 의존
	g.AddEdge(sg, vpc)      // security_group은 vpc에 의존
	g.AddEdge(ec2, subnet)  // instance는 subnet에 의존
	g.AddEdge(ec2, sg)      // instance는 security_group에 의존
	g.AddEdge(eip, ec2)     // eip는 instance에 의존

	// 그래프 시각화
	fmt.Println(g.Visualize())

	// 의존 관계 다이어그램
	fmt.Println("  의존 관계 다이어그램:")
	fmt.Println()
	fmt.Println("                 aws_eip.web")
	fmt.Println("                     │")
	fmt.Println("                     ▼")
	fmt.Println("               aws_instance.web")
	fmt.Println("                /           \\")
	fmt.Println("               ▼             ▼")
	fmt.Println("     aws_subnet.main   aws_security_group.web")
	fmt.Println("               \\             /")
	fmt.Println("                ▼           ▼")
	fmt.Println("               aws_vpc.main")
	fmt.Println()

	// =========================================================================
	// 데모 2: 위상 정렬
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  데모 2: 위상 정렬 (Topological Sort)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	sorted, err := g.TopologicalSort()
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
		return
	}

	fmt.Println("  위상 정렬 결과 (의존성 순서):")
	for i, v := range sorted {
		rv := v.(*ResourceVertex)
		deps := g.downEdges[v.Name()]
		depStr := "(의존성 없음 - 루트)"
		if len(deps) > 0 {
			depStr = fmt.Sprintf("← 의존: %s", strings.Join(deps, ", "))
		}
		fmt.Printf("    %d. %-30s %s\n", i+1, rv.String(), depStr)
	}
	fmt.Println()

	// =========================================================================
	// 데모 3: 순환 탐지
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  데모 3: 순환 탐지 (Cycle Detection)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	// 정상 그래프 검사
	cycles := g.DetectCycles()
	if len(cycles) == 0 {
		fmt.Println("  ✓ 정상 그래프: 순환 없음")
	}

	// 순환이 있는 그래프 생성
	fmt.Println()
	fmt.Println("  순환 그래프 테스트:")
	fmt.Println("    A → B → C → A (순환)")
	fmt.Println()

	cycleGraph := NewDAG()
	a := &ResourceVertex{ResourceType: "resource", ResourceName: "A", Action: "create"}
	b := &ResourceVertex{ResourceType: "resource", ResourceName: "B", Action: "create"}
	c := &ResourceVertex{ResourceType: "resource", ResourceName: "C", Action: "create"}
	cycleGraph.AddVertex(a)
	cycleGraph.AddVertex(b)
	cycleGraph.AddVertex(c)
	cycleGraph.AddEdge(a, b)
	cycleGraph.AddEdge(b, c)
	cycleGraph.AddEdge(c, a) // 순환 생성!

	cycles = cycleGraph.DetectCycles()
	if len(cycles) > 0 {
		fmt.Printf("  ✗ 순환 발견! %d개의 순환:\n", len(cycles))
		for i, cycle := range cycles {
			fmt.Printf("    순환 %d: %s\n", i+1, strings.Join(cycle, " → "))
		}
	}

	// 위상 정렬 시도 (실패해야 함)
	_, err = cycleGraph.TopologicalSort()
	if err != nil {
		fmt.Printf("  ✗ 위상 정렬 실패: %v\n", err)
	}
	fmt.Println()

	// =========================================================================
	// 데모 4: 병렬 워커
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  데모 4: 병렬 워커 (Parallel Walk)")
	fmt.Println("  (dag.Walker 패턴 - 의존성 충족된 정점을 동시 처리)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	startTime := time.Now()
	var walkMu sync.Mutex
	walkLog := make([]string, 0)

	errs := g.ParallelWalk(func(v Vertex) error {
		rv := v.(*ResourceVertex)
		elapsed := time.Since(startTime).Milliseconds()

		// 리소스 생성 시뮬레이션 (100ms 소요)
		entry := fmt.Sprintf("  [%4dms] ▶ 시작: %-35s (goroutine에서 실행 중)",
			elapsed, rv.String())
		walkMu.Lock()
		walkLog = append(walkLog, entry)
		walkMu.Unlock()

		time.Sleep(100 * time.Millisecond) // 리소스 생성 시뮬레이션

		elapsed = time.Since(startTime).Milliseconds()
		entry = fmt.Sprintf("  [%4dms] ✓ 완료: %s", elapsed, rv.String())
		walkMu.Lock()
		walkLog = append(walkLog, entry)
		walkMu.Unlock()

		return nil
	})

	// 실행 로그 출력
	for _, log := range walkLog {
		fmt.Println(log)
	}
	fmt.Println()

	totalTime := time.Since(startTime).Milliseconds()
	fmt.Printf("  총 소요 시간: %dms\n", totalTime)
	fmt.Println("  (순차 실행 시 500ms 소요, 병렬 실행으로 단축됨)")
	fmt.Println("  병렬 처리: VPC → (Subnet + SG 동시) → EC2 → EIP")

	if len(errs) > 0 {
		fmt.Println("\n  오류:")
		for _, e := range errs {
			fmt.Printf("    - %v\n", e)
		}
	}

	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  핵심 포인트 정리")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()
	fmt.Println("  1. Terraform은 모든 리소스 관계를 DAG로 모델링한다")
	fmt.Println("  2. 위상 정렬로 올바른 생성/삭제 순서를 결정한다")
	fmt.Println("  3. 순환 의존성이 있으면 plan/apply가 실패한다")
	fmt.Println("  4. dag.Walker는 의존성이 충족된 리소스를 병렬로 처리한다")
	fmt.Println("  5. 병렬 처리로 대규모 인프라 배포 시간을 크게 단축한다")
}
