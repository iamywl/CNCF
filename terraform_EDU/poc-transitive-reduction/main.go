package main

import (
	"fmt"
	"sort"
	"strings"
)

// =============================================================================
// Terraform DAG 이행적 축소(Transitive Reduction) 시뮬레이션
// =============================================================================
// Terraform은 리소스 의존성 그래프(DAG)에서 이행적 축소를 수행합니다.
// 이행적 축소는 도달 가능성(reachability)을 유지하면서 불필요한 간선을
// 제거하는 그래프 최적화 알고리즘입니다.
//
// 실제 코드: internal/dag/dag.go의 TransitiveReduction() 메서드
//
// 예시: A → B → C, A → C 일 때
//   A → C 는 A → B → C를 통해 도달 가능하므로 제거할 수 있습니다.
//   이를 통해 그래프가 단순해지고 실행 순서가 명확해집니다.
// =============================================================================

// ─────────────────────────────────────────────────────────────────────────────
// Graph (방향 그래프)
// ─────────────────────────────────────────────────────────────────────────────

// Graph는 방향 그래프입니다.
type Graph struct {
	vertices []string
	edges    map[string]map[string]bool // from → {to → true}
}

// NewGraph는 새 그래프를 생성합니다.
func NewGraph() *Graph {
	return &Graph{
		edges: make(map[string]map[string]bool),
	}
}

// AddVertex는 정점을 추가합니다.
func (g *Graph) AddVertex(v string) {
	for _, existing := range g.vertices {
		if existing == v {
			return
		}
	}
	g.vertices = append(g.vertices, v)
	if g.edges[v] == nil {
		g.edges[v] = make(map[string]bool)
	}
}

// AddEdge는 간선을 추가합니다 (from → to).
func (g *Graph) AddEdge(from, to string) {
	g.AddVertex(from)
	g.AddVertex(to)
	g.edges[from][to] = true
}

// RemoveEdge는 간선을 제거합니다.
func (g *Graph) RemoveEdge(from, to string) {
	if g.edges[from] != nil {
		delete(g.edges[from], to)
	}
}

// HasEdge는 간선이 존재하는지 확인합니다.
func (g *Graph) HasEdge(from, to string) bool {
	if g.edges[from] == nil {
		return false
	}
	return g.edges[from][to]
}

// Successors는 정점의 직접 후속 정점들을 반환합니다.
func (g *Graph) Successors(v string) []string {
	var result []string
	if g.edges[v] == nil {
		return result
	}
	for to := range g.edges[v] {
		result = append(result, to)
	}
	sort.Strings(result)
	return result
}

// EdgeCount는 간선 수를 반환합니다.
func (g *Graph) EdgeCount() int {
	count := 0
	for _, targets := range g.edges {
		count += len(targets)
	}
	return count
}

// Clone은 그래프를 복사합니다.
func (g *Graph) Clone() *Graph {
	clone := NewGraph()
	for _, v := range g.vertices {
		clone.AddVertex(v)
	}
	for from, targets := range g.edges {
		for to := range targets {
			clone.AddEdge(from, to)
		}
	}
	return clone
}

// ─────────────────────────────────────────────────────────────────────────────
// 이행적 축소 알고리즘
// ─────────────────────────────────────────────────────────────────────────────

// TransitiveReduction은 이행적 축소를 수행합니다.
// 실제: internal/dag/dag.go
//
// 알고리즘:
// 각 정점 u에 대해:
//   각 직접 후속 정점 v에 대해:
//     v에서 도달 가능한 모든 정점 w에 대해:
//       u → w 간선이 있으면 제거 (u → v → ... → w 경로로 이미 도달 가능)
func TransitiveReduction(g *Graph) (removed []Edge) {
	// 정렬된 정점 목록 (결정적 순서)
	vertices := make([]string, len(g.vertices))
	copy(vertices, g.vertices)
	sort.Strings(vertices)

	for _, u := range vertices {
		successors := g.Successors(u)

		for _, v := range successors {
			// v에서 도달 가능한 모든 정점 찾기 (v 자체 제외)
			reachable := findReachable(g, v)

			// u의 다른 후속 정점 중 v를 통해 도달 가능한 것들의 간선 제거
			for _, w := range successors {
				if w == v {
					continue
				}
				if reachable[w] {
					removed = append(removed, Edge{From: u, To: w})
					g.RemoveEdge(u, w)
				}
			}
		}
	}

	return removed
}

// Edge는 간선을 나타냅니다.
type Edge struct {
	From string
	To   string
}

func (e Edge) String() string {
	return fmt.Sprintf("%s -> %s", e.From, e.To)
}

// findReachable은 시작 정점에서 도달 가능한 모든 정점을 BFS로 찾습니다.
func findReachable(g *Graph, start string) map[string]bool {
	reachable := make(map[string]bool)
	queue := []string{start}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		for _, next := range g.Successors(current) {
			if !reachable[next] {
				reachable[next] = true
				queue = append(queue, next)
			}
		}
	}

	return reachable
}

// ─────────────────────────────────────────────────────────────────────────────
// 도달 가능성 검증
// ─────────────────────────────────────────────────────────────────────────────

// VerifyReachability는 축소 전후의 도달 가능성이 동일한지 검증합니다.
func VerifyReachability(before, after *Graph) (bool, string) {
	vertices := before.vertices

	for _, v := range vertices {
		beforeReach := findReachable(before, v)
		afterReach := findReachable(after, v)

		// before에서 도달 가능한 모든 정점이 after에서도 도달 가능해야 함
		for target := range beforeReach {
			if !afterReach[target] {
				return false, fmt.Sprintf("%s에서 %s로의 도달 가능성이 사라졌습니다", v, target)
			}
		}

		// after에서 새로운 도달 가능성이 생기면 안됨
		for target := range afterReach {
			if !beforeReach[target] {
				return false, fmt.Sprintf("%s에서 %s로의 새로운 도달 가능성이 생겼습니다", v, target)
			}
		}
	}

	return true, "도달 가능성이 완전히 보존되었습니다"
}

// ─────────────────────────────────────────────────────────────────────────────
// ASCII 시각화
// ─────────────────────────────────────────────────────────────────────────────

// PrintGraph는 그래프를 ASCII로 시각화합니다.
func PrintGraph(g *Graph, title string) {
	fmt.Printf("  [%s] (정점: %d, 간선: %d)\n\n", title, len(g.vertices), g.EdgeCount())

	// 인접 리스트 형태로 출력
	vertices := make([]string, len(g.vertices))
	copy(vertices, g.vertices)
	sort.Strings(vertices)

	for _, v := range vertices {
		successors := g.Successors(v)
		if len(successors) > 0 {
			arrows := make([]string, len(successors))
			for i, s := range successors {
				arrows[i] = s
			}
			fmt.Printf("    %-12s --> %s\n", v, strings.Join(arrows, ", "))
		} else {
			fmt.Printf("    %-12s (종단)\n", v)
		}
	}
	fmt.Println()
}

// PrintGraphVisual은 그래프를 더 시각적으로 표현합니다.
func PrintGraphVisual(g *Graph) {
	vertices := make([]string, len(g.vertices))
	copy(vertices, g.vertices)
	sort.Strings(vertices)

	// 레벨 계산 (위상 정렬 기반)
	levels := topologicalLevels(g)

	// 레벨별로 그룹화
	maxLevel := 0
	levelGroups := make(map[int][]string)
	for v, level := range levels {
		levelGroups[level] = append(levelGroups[level], v)
		if level > maxLevel {
			maxLevel = level
		}
	}

	fmt.Println("    레벨별 배치:")
	for level := 0; level <= maxLevel; level++ {
		nodes := levelGroups[level]
		sort.Strings(nodes)
		fmt.Printf("    L%d: %s\n", level, strings.Join(nodes, "  "))
	}
	fmt.Println()
}

// topologicalLevels는 각 정점의 위상 정렬 레벨을 계산합니다.
func topologicalLevels(g *Graph) map[string]int {
	// 진입 차수 계산
	inDegree := make(map[string]int)
	for _, v := range g.vertices {
		inDegree[v] = 0
	}
	for _, targets := range g.edges {
		for to := range targets {
			inDegree[to]++
		}
	}

	// BFS로 레벨 계산
	levels := make(map[string]int)
	queue := []string{}

	for v, degree := range inDegree {
		if degree == 0 {
			queue = append(queue, v)
			levels[v] = 0
		}
	}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		for _, next := range g.Successors(current) {
			inDegree[next]--
			newLevel := levels[current] + 1
			if newLevel > levels[next] {
				levels[next] = newLevel
			}
			if inDegree[next] == 0 {
				queue = append(queue, next)
			}
		}
	}

	return levels
}

// ─────────────────────────────────────────────────────────────────────────────
// 헬퍼 함수
// ─────────────────────────────────────────────────────────────────────────────

func printSeparator(title string) {
	fmt.Println()
	fmt.Println(strings.Repeat("=", 70))
	fmt.Printf("  %s\n", title)
	fmt.Println(strings.Repeat("=", 70))
	fmt.Println()
}

// ─────────────────────────────────────────────────────────────────────────────
// 메인 함수
// ─────────────────────────────────────────────────────────────────────────────

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║          Terraform DAG 이행적 축소 시뮬레이션                       ║")
	fmt.Println("║                                                                      ║")
	fmt.Println("║  도달 가능성을 유지하면서 불필요한 간선을 제거하는 최적화           ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════════╝")

	// ─── 예제 1: 간단한 삼각형 ───
	printSeparator("예제 1: 간단한 삼각형 (A→B→C, A→C)")

	g1 := NewGraph()
	g1.AddEdge("A", "B")
	g1.AddEdge("B", "C")
	g1.AddEdge("A", "C") // 이행적 간선 (A→B→C로 도달 가능)

	before1 := g1.Clone()
	PrintGraph(g1, "축소 전")

	fmt.Println("  A→C는 A→B→C를 통해 도달 가능하므로 제거 가능")
	fmt.Println()

	removed1 := TransitiveReduction(g1)
	PrintGraph(g1, "축소 후")

	fmt.Printf("  제거된 간선: %d개\n", len(removed1))
	for _, e := range removed1 {
		fmt.Printf("    - %s (이행적 간선)\n", e.String())
	}

	ok, msg := VerifyReachability(before1, g1)
	fmt.Printf("  도달 가능성 검증: %v - %s\n", ok, msg)

	// ─── 예제 2: 다이아몬드 패턴 ───
	printSeparator("예제 2: 다이아몬드 패턴 + 이행적 간선")

	g2 := NewGraph()
	g2.AddEdge("A", "B")
	g2.AddEdge("A", "C")
	g2.AddEdge("B", "D")
	g2.AddEdge("C", "D")
	g2.AddEdge("A", "D") // 이행적 (A→B→D 또는 A→C→D)

	before2 := g2.Clone()
	PrintGraph(g2, "축소 전")

	fmt.Print(`
    축소 전:        축소 후:
       A               A
      /|\             / \
     / | \           /   \
    B  |  C    →    B     C
     \ | /           \  /
      \|/             \/
       D               D
`)

	removed2 := TransitiveReduction(g2)
	PrintGraph(g2, "축소 후")

	fmt.Printf("  제거된 간선: %d개\n", len(removed2))
	for _, e := range removed2 {
		fmt.Printf("    - %s\n", e.String())
	}

	ok2, msg2 := VerifyReachability(before2, g2)
	fmt.Printf("  도달 가능성 검증: %v - %s\n", ok2, msg2)

	// ─── 예제 3: 체인 + 이행적 간선들 ───
	printSeparator("예제 3: 긴 체인 + 다수의 이행적 간선")

	g3 := NewGraph()
	// 메인 체인: A → B → C → D → E
	g3.AddEdge("A", "B")
	g3.AddEdge("B", "C")
	g3.AddEdge("C", "D")
	g3.AddEdge("D", "E")
	// 이행적 간선들
	g3.AddEdge("A", "C") // A→B→C
	g3.AddEdge("A", "D") // A→B→C→D
	g3.AddEdge("A", "E") // A→B→C→D→E
	g3.AddEdge("B", "D") // B→C→D
	g3.AddEdge("B", "E") // B→C→D→E
	g3.AddEdge("C", "E") // C→D→E

	before3 := g3.Clone()
	PrintGraph(g3, "축소 전")

	removed3 := TransitiveReduction(g3)
	PrintGraph(g3, "축소 후")

	fmt.Printf("  제거된 간선: %d개 (원래 %d개 → %d개)\n",
		len(removed3), before3.EdgeCount(), g3.EdgeCount())
	for _, e := range removed3 {
		fmt.Printf("    - %s\n", e.String())
	}

	ok3, msg3 := VerifyReachability(before3, g3)
	fmt.Printf("  도달 가능성 검증: %v - %s\n", ok3, msg3)

	// ─── 예제 4: Terraform 인프라 그래프 시뮬레이션 ───
	printSeparator("예제 4: Terraform 인프라 의존성 그래프")

	g4 := NewGraph()

	// 실제 Terraform 인프라 의존성
	// Provider → VPC → Subnets → Security Groups → Instances → Outputs
	g4.AddEdge("provider", "vpc")
	g4.AddEdge("vpc", "subnet_a")
	g4.AddEdge("vpc", "subnet_b")
	g4.AddEdge("vpc", "sg_web")
	g4.AddEdge("vpc", "sg_db")
	g4.AddEdge("subnet_a", "instance_web")
	g4.AddEdge("subnet_b", "instance_db")
	g4.AddEdge("sg_web", "instance_web")
	g4.AddEdge("sg_db", "instance_db")
	g4.AddEdge("instance_web", "output_ip")
	g4.AddEdge("instance_db", "output_db_host")

	// 이행적 간선 (Terraform이 의존성 분석에서 추가하는 간접 의존성)
	g4.AddEdge("provider", "subnet_a")     // provider→vpc→subnet_a
	g4.AddEdge("provider", "subnet_b")     // provider→vpc→subnet_b
	g4.AddEdge("provider", "sg_web")       // provider→vpc→sg_web
	g4.AddEdge("provider", "instance_web") // provider→vpc→subnet_a→instance_web
	g4.AddEdge("provider", "instance_db")  // provider→vpc→subnet_b→instance_db
	g4.AddEdge("vpc", "instance_web")      // vpc→subnet_a→instance_web
	g4.AddEdge("vpc", "instance_db")       // vpc→subnet_b→instance_db
	g4.AddEdge("provider", "output_ip")    // 긴 이행적 경로

	before4 := g4.Clone()
	PrintGraph(g4, "축소 전")
	PrintGraphVisual(g4)

	removed4 := TransitiveReduction(g4)
	PrintGraph(g4, "축소 후")
	PrintGraphVisual(g4)

	fmt.Printf("  제거된 간선: %d개 (원래 %d개 → %d개)\n",
		len(removed4), before4.EdgeCount(), g4.EdgeCount())
	fmt.Println("  제거된 이행적 간선 목록:")
	for _, e := range removed4 {
		fmt.Printf("    - %-30s (다른 경로로 도달 가능)\n", e.String())
	}

	ok4, msg4 := VerifyReachability(before4, g4)
	fmt.Printf("\n  도달 가능성 검증: %v - %s\n", ok4, msg4)

	// ─── 예제 5: 대규모 그래프 ───
	printSeparator("예제 5: 대규모 그래프 (20 노드)")

	g5 := NewGraph()

	// 20개 노드의 계층적 그래프
	nodes := []string{
		"root",
		"net_1", "net_2", "net_3",
		"sub_1a", "sub_1b", "sub_2a", "sub_2b", "sub_3a",
		"sg_1", "sg_2", "sg_3",
		"inst_1", "inst_2", "inst_3", "inst_4",
		"lb_1", "lb_2",
		"dns_1",
		"out_1",
	}

	for _, n := range nodes {
		g5.AddVertex(n)
	}

	// 필수 간선 (계층 구조)
	g5.AddEdge("root", "net_1")
	g5.AddEdge("root", "net_2")
	g5.AddEdge("root", "net_3")
	g5.AddEdge("net_1", "sub_1a")
	g5.AddEdge("net_1", "sub_1b")
	g5.AddEdge("net_2", "sub_2a")
	g5.AddEdge("net_2", "sub_2b")
	g5.AddEdge("net_3", "sub_3a")
	g5.AddEdge("net_1", "sg_1")
	g5.AddEdge("net_2", "sg_2")
	g5.AddEdge("net_3", "sg_3")
	g5.AddEdge("sub_1a", "inst_1")
	g5.AddEdge("sg_1", "inst_1")
	g5.AddEdge("sub_1b", "inst_2")
	g5.AddEdge("sg_1", "inst_2")
	g5.AddEdge("sub_2a", "inst_3")
	g5.AddEdge("sg_2", "inst_3")
	g5.AddEdge("sub_2b", "inst_4")
	g5.AddEdge("sg_2", "inst_4")
	g5.AddEdge("inst_1", "lb_1")
	g5.AddEdge("inst_2", "lb_1")
	g5.AddEdge("inst_3", "lb_2")
	g5.AddEdge("inst_4", "lb_2")
	g5.AddEdge("lb_1", "dns_1")
	g5.AddEdge("lb_2", "dns_1")
	g5.AddEdge("dns_1", "out_1")

	// 이행적 간선 추가 (많은 양)
	g5.AddEdge("root", "sub_1a")
	g5.AddEdge("root", "sub_1b")
	g5.AddEdge("root", "sub_2a")
	g5.AddEdge("root", "sub_2b")
	g5.AddEdge("root", "sg_1")
	g5.AddEdge("root", "sg_2")
	g5.AddEdge("root", "inst_1")
	g5.AddEdge("root", "inst_2")
	g5.AddEdge("root", "inst_3")
	g5.AddEdge("root", "lb_1")
	g5.AddEdge("root", "dns_1")
	g5.AddEdge("root", "out_1")
	g5.AddEdge("net_1", "inst_1")
	g5.AddEdge("net_1", "inst_2")
	g5.AddEdge("net_2", "inst_3")
	g5.AddEdge("net_2", "inst_4")
	g5.AddEdge("net_1", "lb_1")
	g5.AddEdge("net_2", "lb_2")
	g5.AddEdge("sub_1a", "lb_1")
	g5.AddEdge("sub_2a", "lb_2")

	before5 := g5.Clone()

	fmt.Printf("  축소 전: 정점 %d개, 간선 %d개\n", len(g5.vertices), g5.EdgeCount())

	removed5 := TransitiveReduction(g5)

	fmt.Printf("  축소 후: 정점 %d개, 간선 %d개\n", len(g5.vertices), g5.EdgeCount())
	fmt.Printf("  제거된 간선: %d개 (%.1f%% 감소)\n",
		len(removed5),
		float64(len(removed5))/float64(before5.EdgeCount())*100)

	PrintGraph(g5, "축소 후 그래프")
	PrintGraphVisual(g5)

	ok5, msg5 := VerifyReachability(before5, g5)
	fmt.Printf("  도달 가능성 검증: %v - %s\n", ok5, msg5)

	// ─── 아키텍처 요약 ───
	printSeparator("이행적 축소 아키텍처 요약")
	fmt.Print(`
  이행적 축소 (Transitive Reduction)란?

  정의: 도달 가능성(reachability)을 유지하면서 간선 수를 최소화하는 변환

  예시:
    축소 전:           축소 후:
    A ──→ B            A ──→ B
    │     │                  │
    │     ▼                  ▼
    └───→ C            (없음) C

    A→C는 A→B→C를 통해 도달 가능하므로 불필요

  Terraform에서의 용도:

  1. 의존성 그래프 최적화
     - 리소스 간 직접 의존성만 유지
     - 간접 의존성 (이행적 간선) 제거
     - 실행 계획(plan)의 가독성 향상

  2. 병렬 실행 최적화
     - 불필요한 대기(wait) 관계 제거
     - 더 많은 리소스를 병렬로 처리 가능

  알고리즘 (O(V * E)):
    for each vertex u:
      for each successor v of u:
        for each vertex w reachable from v:
          if edge(u, w) exists:
            remove edge(u, w)

  실제 코드: internal/dag/dag.go TransitiveReduction()
`)
}
