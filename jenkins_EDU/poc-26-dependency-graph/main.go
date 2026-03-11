package main

import (
	"fmt"
	"strings"
)

// =============================================================================
// Jenkins DependencyGraph (양방향 + Tarjan SCC) 시뮬레이션
// =============================================================================
//
// Jenkins는 DependencyGraph로 잡 간의 의존 관계를 관리한다.
// 빌드 후 트리거(upstream/downstream), 빌드 순서 결정에 사용된다.
//
// 핵심 개념:
//   - 양방향 그래프: upstream(의존 대상)과 downstream(트리거 대상)
//   - Tarjan SCC: 순환 의존성 감지 (강결합 컴포넌트)
//   - Topological Sort: 빌드 순서 결정
//   - Transitive Closure: 간접 의존성 포함
//
// 실제 코드 참조:
//   - core/src/main/java/jenkins/model/DependencyGraph.java
// =============================================================================

// --- 그래프 노드 (Job) ---

type Job struct {
	Name string
}

// --- DependencyGraph ---

type DependencyGraph struct {
	upstream   map[string][]string // job -> upstream jobs (이 잡이 의존하는 잡)
	downstream map[string][]string // job -> downstream jobs (이 잡이 트리거하는 잡)
	jobs       map[string]*Job
}

func NewDependencyGraph() *DependencyGraph {
	return &DependencyGraph{
		upstream:   make(map[string][]string),
		downstream: make(map[string][]string),
		jobs:       make(map[string]*Job),
	}
}

func (g *DependencyGraph) AddJob(name string) {
	g.jobs[name] = &Job{Name: name}
}

// AddDependency는 from → to 의존 관계를 추가한다.
// from이 완료되면 to가 트리거된다. (from=upstream, to=downstream)
func (g *DependencyGraph) AddDependency(from, to string) {
	g.downstream[from] = append(g.downstream[from], to)
	g.upstream[to] = append(g.upstream[to], from)
}

func (g *DependencyGraph) GetUpstream(job string) []string {
	return g.upstream[job]
}

func (g *DependencyGraph) GetDownstream(job string) []string {
	return g.downstream[job]
}

// GetTransitiveDownstream은 재귀적으로 모든 하류 잡을 반환한다.
func (g *DependencyGraph) GetTransitiveDownstream(job string) []string {
	visited := make(map[string]bool)
	var result []string
	g.dfsDownstream(job, visited, &result)
	return result
}

func (g *DependencyGraph) dfsDownstream(job string, visited map[string]bool, result *[]string) {
	for _, downstream := range g.downstream[job] {
		if !visited[downstream] {
			visited[downstream] = true
			*result = append(*result, downstream)
			g.dfsDownstream(downstream, visited, result)
		}
	}
}

// GetTransitiveUpstream은 재귀적으로 모든 상류 잡을 반환한다.
func (g *DependencyGraph) GetTransitiveUpstream(job string) []string {
	visited := make(map[string]bool)
	var result []string
	g.dfsUpstream(job, visited, &result)
	return result
}

func (g *DependencyGraph) dfsUpstream(job string, visited map[string]bool, result *[]string) {
	for _, upstream := range g.upstream[job] {
		if !visited[upstream] {
			visited[upstream] = true
			*result = append(*result, upstream)
			g.dfsUpstream(upstream, visited, result)
		}
	}
}

// --- Tarjan's SCC Algorithm ---

type tarjanState struct {
	index    int
	stack    []string
	onStack  map[string]bool
	indices  map[string]int
	lowlinks map[string]int
	sccs     [][]string
}

// FindSCC는 Tarjan 알고리즘으로 강결합 컴포넌트를 찾는다.
func (g *DependencyGraph) FindSCC() [][]string {
	state := &tarjanState{
		onStack:  make(map[string]bool),
		indices:  make(map[string]int),
		lowlinks: make(map[string]int),
	}

	for name := range g.jobs {
		if _, visited := state.indices[name]; !visited {
			g.tarjanDFS(name, state)
		}
	}
	return state.sccs
}

func (g *DependencyGraph) tarjanDFS(v string, s *tarjanState) {
	s.indices[v] = s.index
	s.lowlinks[v] = s.index
	s.index++
	s.stack = append(s.stack, v)
	s.onStack[v] = true

	for _, w := range g.downstream[v] {
		if _, visited := s.indices[w]; !visited {
			g.tarjanDFS(w, s)
			if s.lowlinks[w] < s.lowlinks[v] {
				s.lowlinks[v] = s.lowlinks[w]
			}
		} else if s.onStack[w] {
			if s.indices[w] < s.lowlinks[v] {
				s.lowlinks[v] = s.indices[w]
			}
		}
	}

	// Root of SCC
	if s.lowlinks[v] == s.indices[v] {
		var scc []string
		for {
			w := s.stack[len(s.stack)-1]
			s.stack = s.stack[:len(s.stack)-1]
			s.onStack[w] = false
			scc = append(scc, w)
			if w == v {
				break
			}
		}
		s.sccs = append(s.sccs, scc)
	}
}

// --- Topological Sort (Kahn's Algorithm) ---

func (g *DependencyGraph) TopologicalSort() ([]string, bool) {
	inDegree := make(map[string]int)
	for name := range g.jobs {
		inDegree[name] = 0
	}
	for _, downstreams := range g.downstream {
		for _, d := range downstreams {
			inDegree[d]++
		}
	}

	// 진입 차수가 0인 노드로 시작
	var queue []string
	for name, deg := range inDegree {
		if deg == 0 {
			queue = append(queue, name)
		}
	}

	var sorted []string
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		sorted = append(sorted, node)

		for _, downstream := range g.downstream[node] {
			inDegree[downstream]--
			if inDegree[downstream] == 0 {
				queue = append(queue, downstream)
			}
		}
	}

	hasCycle := len(sorted) != len(g.jobs)
	return sorted, !hasCycle
}

// --- 시각화 ---

func (g *DependencyGraph) PrintASCII() {
	for name := range g.jobs {
		downstreams := g.downstream[name]
		if len(downstreams) == 0 {
			fmt.Printf("  %s (leaf)\n", name)
			continue
		}
		for _, d := range downstreams {
			fmt.Printf("  %s --> %s\n", name, d)
		}
	}
}

func main() {
	fmt.Println("=== Jenkins DependencyGraph (양방향 + Tarjan SCC) 시뮬레이션 ===")
	fmt.Println()

	// --- 의존 그래프 구축 ---
	fmt.Println("[1] 의존 관계 그래프 구축")
	fmt.Println(strings.Repeat("-", 60))

	graph := NewDependencyGraph()

	// Job 등록
	jobs := []string{
		"compile", "unit-test", "integration-test",
		"build-docker", "push-registry",
		"deploy-staging", "smoke-test",
		"deploy-production", "notify",
	}
	for _, j := range jobs {
		graph.AddJob(j)
	}

	// 의존 관계 (from → to = from 완료 시 to 트리거)
	deps := []struct{ from, to string }{
		{"compile", "unit-test"},
		{"compile", "build-docker"},
		{"unit-test", "integration-test"},
		{"build-docker", "push-registry"},
		{"integration-test", "deploy-staging"},
		{"push-registry", "deploy-staging"},
		{"deploy-staging", "smoke-test"},
		{"smoke-test", "deploy-production"},
		{"deploy-production", "notify"},
		{"smoke-test", "notify"},
	}

	for _, d := range deps {
		graph.AddDependency(d.from, d.to)
		fmt.Printf("  %s --> %s\n", d.from, d.to)
	}
	fmt.Println()

	// --- 양방향 조회 ---
	fmt.Println("[2] 양방향 관계 조회")
	fmt.Println(strings.Repeat("-", 60))

	for _, job := range []string{"deploy-staging", "compile", "notify"} {
		up := graph.GetUpstream(job)
		down := graph.GetDownstream(job)
		fmt.Printf("  %s:\n    upstream: %v\n    downstream: %v\n",
			job, up, down)
	}
	fmt.Println()

	// --- Transitive Closure ---
	fmt.Println("[3] Transitive Closure (간접 의존성)")
	fmt.Println(strings.Repeat("-", 60))

	fmt.Printf("  compile의 모든 downstream:\n    %v\n",
		graph.GetTransitiveDownstream("compile"))
	fmt.Printf("  notify의 모든 upstream:\n    %v\n",
		graph.GetTransitiveUpstream("notify"))
	fmt.Printf("  deploy-staging의 모든 upstream:\n    %v\n",
		graph.GetTransitiveUpstream("deploy-staging"))
	fmt.Println()

	// --- Topological Sort ---
	fmt.Println("[4] Topological Sort (빌드 순서)")
	fmt.Println(strings.Repeat("-", 60))

	sorted, ok := graph.TopologicalSort()
	fmt.Printf("  순환 없음: %v\n", ok)
	fmt.Printf("  빌드 순서:\n")
	for i, job := range sorted {
		fmt.Printf("    %d. %s\n", i+1, job)
	}
	fmt.Println()

	// --- Tarjan SCC (순환 감지) ---
	fmt.Println("[5] Tarjan SCC (순환 의존성 감지)")
	fmt.Println(strings.Repeat("-", 60))

	sccs := graph.FindSCC()
	hasCycle := false
	for _, scc := range sccs {
		if len(scc) > 1 {
			hasCycle = true
			fmt.Printf("  [CYCLE] %v\n", scc)
		}
	}
	if !hasCycle {
		fmt.Println("  순환 의존성 없음 (모든 SCC 크기 = 1)")
	}
	fmt.Println()

	// --- 순환 의존성 추가 ---
	fmt.Println("[6] 순환 의존성 추가 후 감지")
	fmt.Println(strings.Repeat("-", 60))

	cyclicGraph := NewDependencyGraph()
	cycleJobs := []string{"A", "B", "C", "D", "E", "F"}
	for _, j := range cycleJobs {
		cyclicGraph.AddJob(j)
	}
	cyclicGraph.AddDependency("A", "B")
	cyclicGraph.AddDependency("B", "C")
	cyclicGraph.AddDependency("C", "A") // 순환: A -> B -> C -> A
	cyclicGraph.AddDependency("D", "E")
	cyclicGraph.AddDependency("E", "F")
	cyclicGraph.AddDependency("F", "D") // 순환: D -> E -> F -> D

	fmt.Println("  그래프:")
	cyclicGraph.PrintASCII()
	fmt.Println()

	sccs2 := cyclicGraph.FindSCC()
	fmt.Println("  SCC (강결합 컴포넌트):")
	for _, scc := range sccs2 {
		label := "single"
		if len(scc) > 1 {
			label = "CYCLE"
		}
		fmt.Printf("    [%s] %v\n", label, scc)
	}

	_, ok2 := cyclicGraph.TopologicalSort()
	fmt.Printf("\n  Topological sort 가능: %v (순환 있으면 false)\n", ok2)
	fmt.Println()

	// --- 빌드 트리거 시뮬레이션 ---
	fmt.Println("[7] 빌드 트리거 시뮬레이션")
	fmt.Println(strings.Repeat("-", 60))

	fmt.Println("  'compile' 성공 시 트리거 체인:")
	triggered := make(map[string]bool)
	triggerChain(graph, "compile", triggered, 0)
	fmt.Println()

	fmt.Println("=== 시뮬레이션 완료 ===")
}

func triggerChain(g *DependencyGraph, job string, triggered map[string]bool, depth int) {
	indent := strings.Repeat("  ", depth+1)
	fmt.Printf("%s[BUILD] %s\n", indent, job)
	triggered[job] = true

	for _, downstream := range g.GetDownstream(job) {
		// 모든 upstream이 완료되었는지 확인
		allUpstreamDone := true
		for _, upstream := range g.GetUpstream(downstream) {
			if !triggered[upstream] {
				allUpstreamDone = false
				break
			}
		}
		if allUpstreamDone && !triggered[downstream] {
			triggerChain(g, downstream, triggered, depth+1)
		} else if !allUpstreamDone {
			fmt.Printf("%s  [WAIT] %s (upstream 대기 중)\n", indent, downstream)
		}
	}
}
