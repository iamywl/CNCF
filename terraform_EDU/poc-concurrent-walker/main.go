package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Terraform 동시성 그래프 워커(Concurrent Graph Walker) 시뮬레이션
// =============================================================================
// Terraform은 리소스 의존성 DAG를 동시에 워킹하여 병렬로 리소스를 처리합니다.
// 세마포어로 동시 실행 수를 제한하고, 의존성이 모두 완료된 노드만 실행합니다.
//
// 실제 코드: internal/dag/walk.go
//
// 핵심 원리:
// 1. DAG의 각 노드는 모든 의존성(부모)이 완료되어야 실행 가능
// 2. 세마포어로 동시 실행되는 goroutine 수 제한
// 3. 에러 발생 시 해당 노드의 모든 의존 노드(자식)에 전파
// 4. 실행 타이밍 추적으로 병렬성 시각화
// =============================================================================

// ─────────────────────────────────────────────────────────────────────────────
// DAG (방향 비순환 그래프)
// ─────────────────────────────────────────────────────────────────────────────

// Node는 그래프의 노드입니다.
type Node struct {
	Name     string
	Duration time.Duration // 시뮬레이션 실행 시간
	Error    error         // 시뮬레이션 에러 (nil이면 성공)
}

// DAG는 방향 비순환 그래프입니다.
type DAG struct {
	nodes map[string]*Node
	edges map[string]map[string]bool // from → {to → true} (from이 to에 의존)
}

func NewDAG() *DAG {
	return &DAG{
		nodes: make(map[string]*Node),
		edges: make(map[string]map[string]bool),
	}
}

func (g *DAG) AddNode(name string, duration time.Duration, err error) {
	g.nodes[name] = &Node{
		Name:     name,
		Duration: duration,
		Error:    err,
	}
	if g.edges[name] == nil {
		g.edges[name] = make(map[string]bool)
	}
}

// AddDependency는 의존성을 추가합니다 (node는 dep에 의존).
func (g *DAG) AddDependency(node, dep string) {
	if g.edges[node] == nil {
		g.edges[node] = make(map[string]bool)
	}
	g.edges[node][dep] = true
}

// Dependencies는 노드의 의존성(부모) 목록을 반환합니다.
func (g *DAG) Dependencies(name string) []string {
	var deps []string
	for dep := range g.edges[name] {
		deps = append(deps, dep)
	}
	sort.Strings(deps)
	return deps
}

// Dependents는 노드에 의존하는(자식) 노드 목록을 반환합니다.
func (g *DAG) Dependents(name string) []string {
	var deps []string
	for node, edges := range g.edges {
		if edges[name] {
			deps = append(deps, node)
		}
	}
	sort.Strings(deps)
	return deps
}

// RootNodes는 의존성이 없는 루트 노드를 반환합니다.
func (g *DAG) RootNodes() []string {
	var roots []string
	for name := range g.nodes {
		if len(g.edges[name]) == 0 {
			roots = append(roots, name)
		}
	}
	sort.Strings(roots)
	return roots
}

// NodeNames는 모든 노드 이름을 반환합니다.
func (g *DAG) NodeNames() []string {
	var names []string
	for name := range g.nodes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ─────────────────────────────────────────────────────────────────────────────
// ExecutionRecord: 실행 기록
// ─────────────────────────────────────────────────────────────────────────────

// ExecutionRecord는 노드 실행 기록입니다.
type ExecutionRecord struct {
	Node      string
	StartTime time.Duration // 시작 시간 (워커 시작 기준)
	EndTime   time.Duration // 종료 시간
	Duration  time.Duration
	Error     error
	Skipped   bool   // 의존성 에러로 건너뜀
	SkipReason string
}

// ─────────────────────────────────────────────────────────────────────────────
// Walker: 동시성 그래프 워커
// ─────────────────────────────────────────────────────────────────────────────

// Walker는 DAG를 동시에 워킹합니다.
// 실제: internal/dag/walk.go
type Walker struct {
	dag         *DAG
	parallelism int // 동시 실행 제한 (세마포어 크기)

	mu       sync.Mutex
	records  []ExecutionRecord
	errors   map[string]error  // 노드별 에러 기록
	done     map[string]bool   // 완료된 노드
	baseTime time.Time         // 워커 시작 시간
}

func NewWalker(dag *DAG, parallelism int) *Walker {
	return &Walker{
		dag:         dag,
		parallelism: parallelism,
		errors:      make(map[string]error),
		done:        make(map[string]bool),
	}
}

// Walk는 DAG를 워킹합니다.
func (w *Walker) Walk() []ExecutionRecord {
	w.baseTime = time.Now()

	// 세마포어 (동시 실행 제한)
	sem := make(chan struct{}, w.parallelism)

	// 각 노드의 완료를 알리는 채널
	nodeDone := make(map[string]chan struct{})
	for name := range w.dag.nodes {
		nodeDone[name] = make(chan struct{})
	}

	var wg sync.WaitGroup

	// 모든 노드에 대해 goroutine 시작
	for _, name := range w.dag.NodeNames() {
		wg.Add(1)
		go func(nodeName string) {
			defer wg.Done()
			defer close(nodeDone[nodeName])

			// 1. 모든 의존성 완료 대기
			deps := w.dag.Dependencies(nodeName)
			for _, dep := range deps {
				<-nodeDone[dep]
			}

			// 2. 의존성 에러 확인
			w.mu.Lock()
			for _, dep := range deps {
				if err, ok := w.errors[dep]; ok && err != nil {
					// 의존성에서 에러 발생 → 이 노드 건너뛰기
					record := ExecutionRecord{
						Node:       nodeName,
						StartTime:  time.Since(w.baseTime),
						EndTime:    time.Since(w.baseTime),
						Duration:   0,
						Skipped:    true,
						SkipReason: fmt.Sprintf("의존성 '%s' 에러: %v", dep, err),
					}
					w.records = append(w.records, record)
					w.errors[nodeName] = fmt.Errorf("의존성 에러로 건너뜀")
					w.done[nodeName] = true
					w.mu.Unlock()
					return
				}
			}
			w.mu.Unlock()

			// 3. 세마포어 획득 (동시 실행 제한)
			sem <- struct{}{}

			startTime := time.Since(w.baseTime)
			node := w.dag.nodes[nodeName]

			// 4. 노드 실행 (시뮬레이션)
			time.Sleep(node.Duration)

			endTime := time.Since(w.baseTime)

			// 5. 결과 기록
			w.mu.Lock()
			record := ExecutionRecord{
				Node:      nodeName,
				StartTime: startTime,
				EndTime:   endTime,
				Duration:  node.Duration,
				Error:     node.Error,
			}
			w.records = append(w.records, record)
			if node.Error != nil {
				w.errors[nodeName] = node.Error
			}
			w.done[nodeName] = true
			w.mu.Unlock()

			// 6. 세마포어 해제
			<-sem

		}(name)
	}

	wg.Wait()

	// 시작 시간순 정렬
	sort.Slice(w.records, func(i, j int) bool {
		if w.records[i].StartTime == w.records[j].StartTime {
			return w.records[i].Node < w.records[j].Node
		}
		return w.records[i].StartTime < w.records[j].StartTime
	})

	return w.records
}

// ─────────────────────────────────────────────────────────────────────────────
// 타임라인 시각화
// ─────────────────────────────────────────────────────────────────────────────

// PrintTimeline은 실행 타임라인을 ASCII로 시각화합니다.
func PrintTimeline(records []ExecutionRecord, parallelism int) {
	if len(records) == 0 {
		return
	}

	// 최대 종료 시간 계산
	var maxEnd time.Duration
	for _, r := range records {
		if r.EndTime > maxEnd {
			maxEnd = r.EndTime
		}
	}

	// 타임라인 너비 (문자 수)
	const width = 50
	scale := float64(width) / float64(maxEnd)

	fmt.Printf("  동시 실행 제한: %d\n", parallelism)
	fmt.Printf("  전체 실행 시간: %v\n\n", maxEnd.Truncate(time.Millisecond))

	fmt.Printf("  %-20s ", "노드")
	fmt.Print("|")
	for i := 0; i < width; i++ {
		if i%(width/5) == 0 {
			t := time.Duration(float64(i) / scale)
			label := fmt.Sprintf("%dms", t.Milliseconds())
			fmt.Print(label)
			i += len(label) - 1
		} else {
			fmt.Print("-")
		}
	}
	fmt.Println("|")

	fmt.Printf("  %-20s ", "")
	fmt.Print("|")
	fmt.Print(strings.Repeat("-", width))
	fmt.Println("|")

	for _, r := range records {
		name := r.Node
		if len(name) > 18 {
			name = name[:18]
		}

		startPos := int(float64(r.StartTime) * scale)
		endPos := int(float64(r.EndTime) * scale)
		if endPos <= startPos {
			endPos = startPos + 1
		}
		if endPos > width {
			endPos = width
		}

		fmt.Printf("  %-20s |", name)

		for i := 0; i < width; i++ {
			if i >= startPos && i < endPos {
				if r.Skipped {
					fmt.Print("X")
				} else if r.Error != nil {
					fmt.Print("!")
				} else {
					fmt.Print("#")
				}
			} else {
				fmt.Print(" ")
			}
		}

		status := "OK"
		if r.Skipped {
			status = "SKIP"
		} else if r.Error != nil {
			status = "ERR"
		}
		fmt.Printf("| [%s] %v\n", status, r.Duration)
	}

	fmt.Printf("  %-20s ", "")
	fmt.Print("|")
	fmt.Print(strings.Repeat("-", width))
	fmt.Println("|")

	fmt.Println()
	fmt.Println("  범례: # = 실행 중, ! = 에러, X = 건너뜀")
}

// PrintExecutionSummary는 실행 요약을 출력합니다.
func PrintExecutionSummary(records []ExecutionRecord) {
	totalDuration := time.Duration(0)
	successCount := 0
	errorCount := 0
	skipCount := 0

	var maxEnd time.Duration

	for _, r := range records {
		if r.EndTime > maxEnd {
			maxEnd = r.EndTime
		}
		if r.Skipped {
			skipCount++
		} else if r.Error != nil {
			errorCount++
			totalDuration += r.Duration
		} else {
			successCount++
			totalDuration += r.Duration
		}
	}

	fmt.Println("  실행 요약:")
	fmt.Printf("    총 노드:         %d\n", len(records))
	fmt.Printf("    성공:            %d\n", successCount)
	fmt.Printf("    에러:            %d\n", errorCount)
	fmt.Printf("    건너뜀:          %d\n", skipCount)
	fmt.Printf("    순차 실행 시간:  %v (모든 작업 합계)\n", totalDuration)
	fmt.Printf("    실제 실행 시간:  %v (병렬 실행)\n", maxEnd.Truncate(time.Millisecond))
	if totalDuration > 0 {
		speedup := float64(totalDuration) / float64(maxEnd)
		fmt.Printf("    속도 향상:       %.1fx\n", speedup)
	}
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
	fmt.Println("║          Terraform 동시성 그래프 워커 시뮬레이션                    ║")
	fmt.Println("║                                                                      ║")
	fmt.Println("║  DAG 기반 병렬 실행 + 세마포어 제어 + 에러 전파                    ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════════╝")

	// ─── 예제 1: 간단한 DAG (병렬성 확인) ───
	printSeparator("1. 간단한 DAG (parallelism=4)")
	fmt.Println("  그래프 구조:")
	fmt.Println("    provider → vpc → subnet_a → instance_a → output")
	fmt.Println("                   → subnet_b → instance_b ─↗")
	fmt.Println()

	dag1 := NewDAG()
	dag1.AddNode("provider", 30*time.Millisecond, nil)
	dag1.AddNode("vpc", 50*time.Millisecond, nil)
	dag1.AddNode("subnet_a", 40*time.Millisecond, nil)
	dag1.AddNode("subnet_b", 40*time.Millisecond, nil)
	dag1.AddNode("instance_a", 60*time.Millisecond, nil)
	dag1.AddNode("instance_b", 60*time.Millisecond, nil)
	dag1.AddNode("output", 10*time.Millisecond, nil)

	dag1.AddDependency("vpc", "provider")
	dag1.AddDependency("subnet_a", "vpc")
	dag1.AddDependency("subnet_b", "vpc")
	dag1.AddDependency("instance_a", "subnet_a")
	dag1.AddDependency("instance_b", "subnet_b")
	dag1.AddDependency("output", "instance_a")
	dag1.AddDependency("output", "instance_b")

	walker1 := NewWalker(dag1, 4)
	records1 := walker1.Walk()
	PrintTimeline(records1, 4)
	PrintExecutionSummary(records1)

	// ─── 예제 2: 병렬성 비교 (parallelism=1 vs 4) ───
	printSeparator("2. 병렬성 비교: parallelism=1 (순차)")

	dag2a := NewDAG()
	dag2a.AddNode("A", 30*time.Millisecond, nil)
	dag2a.AddNode("B", 30*time.Millisecond, nil)
	dag2a.AddNode("C", 30*time.Millisecond, nil)
	dag2a.AddNode("D", 30*time.Millisecond, nil)
	dag2a.AddNode("E", 30*time.Millisecond, nil)

	// A와 B는 독립, C는 A에 의존, D는 B에 의존, E는 C+D에 의존
	dag2a.AddDependency("C", "A")
	dag2a.AddDependency("D", "B")
	dag2a.AddDependency("E", "C")
	dag2a.AddDependency("E", "D")

	walker2a := NewWalker(dag2a, 1)
	records2a := walker2a.Walk()
	PrintTimeline(records2a, 1)
	PrintExecutionSummary(records2a)

	printSeparator("2b. 병렬성 비교: parallelism=4 (병렬)")

	dag2b := NewDAG()
	dag2b.AddNode("A", 30*time.Millisecond, nil)
	dag2b.AddNode("B", 30*time.Millisecond, nil)
	dag2b.AddNode("C", 30*time.Millisecond, nil)
	dag2b.AddNode("D", 30*time.Millisecond, nil)
	dag2b.AddNode("E", 30*time.Millisecond, nil)

	dag2b.AddDependency("C", "A")
	dag2b.AddDependency("D", "B")
	dag2b.AddDependency("E", "C")
	dag2b.AddDependency("E", "D")

	walker2b := NewWalker(dag2b, 4)
	records2b := walker2b.Walk()
	PrintTimeline(records2b, 4)
	PrintExecutionSummary(records2b)

	// ─── 예제 3: 에러 전파 ───
	printSeparator("3. 에러 전파 (subnet_a 실패 → instance_a, output 건너뜀)")

	dag3 := NewDAG()
	dag3.AddNode("provider", 20*time.Millisecond, nil)
	dag3.AddNode("vpc", 30*time.Millisecond, nil)
	dag3.AddNode("subnet_a", 25*time.Millisecond, fmt.Errorf("API 에러: VPC quota 초과"))
	dag3.AddNode("subnet_b", 25*time.Millisecond, nil)
	dag3.AddNode("instance_a", 40*time.Millisecond, nil) // subnet_a 에러로 건너뜀
	dag3.AddNode("instance_b", 40*time.Millisecond, nil)
	dag3.AddNode("output", 10*time.Millisecond, nil) // instance_a 건너뜀으로 건너뜀

	dag3.AddDependency("vpc", "provider")
	dag3.AddDependency("subnet_a", "vpc")
	dag3.AddDependency("subnet_b", "vpc")
	dag3.AddDependency("instance_a", "subnet_a")
	dag3.AddDependency("instance_b", "subnet_b")
	dag3.AddDependency("output", "instance_a")
	dag3.AddDependency("output", "instance_b")

	walker3 := NewWalker(dag3, 4)
	records3 := walker3.Walk()
	PrintTimeline(records3, 4)

	fmt.Println("  에러 전파 상세:")
	for _, r := range records3 {
		if r.Error != nil {
			fmt.Printf("    [ERR]  %-20s 에러: %v\n", r.Node, r.Error)
		}
		if r.Skipped {
			fmt.Printf("    [SKIP] %-20s 이유: %s\n", r.Node, r.SkipReason)
		}
	}
	fmt.Println()
	PrintExecutionSummary(records3)

	// ─── 예제 4: 대규모 그래프 (20 노드) ───
	printSeparator("4. 대규모 인프라 그래프 (20 노드, parallelism=4)")

	dag4 := NewDAG()

	// 레이어 1: 프로바이더 (1개)
	dag4.AddNode("provider_aws", 20*time.Millisecond, nil)

	// 레이어 2: 네트워크 기반 (3개)
	dag4.AddNode("vpc_main", 40*time.Millisecond, nil)
	dag4.AddNode("vpc_staging", 40*time.Millisecond, nil)
	dag4.AddNode("iam_role", 30*time.Millisecond, nil)

	// 레이어 3: 서브넷/보안그룹 (5개)
	dag4.AddNode("subnet_pub_1", 25*time.Millisecond, nil)
	dag4.AddNode("subnet_pub_2", 25*time.Millisecond, nil)
	dag4.AddNode("subnet_priv_1", 25*time.Millisecond, nil)
	dag4.AddNode("sg_web", 20*time.Millisecond, nil)
	dag4.AddNode("sg_db", 20*time.Millisecond, nil)

	// 레이어 4: 인스턴스/DB (5개)
	dag4.AddNode("web_1", 50*time.Millisecond, nil)
	dag4.AddNode("web_2", 50*time.Millisecond, nil)
	dag4.AddNode("api_server", 45*time.Millisecond, nil)
	dag4.AddNode("rds_primary", 80*time.Millisecond, nil)
	dag4.AddNode("rds_replica", 60*time.Millisecond, nil)

	// 레이어 5: 로드밸런서/캐시 (3개)
	dag4.AddNode("alb_web", 35*time.Millisecond, nil)
	dag4.AddNode("alb_api", 35*time.Millisecond, nil)
	dag4.AddNode("elasticache", 40*time.Millisecond, nil)

	// 레이어 6: DNS/출력 (3개)
	dag4.AddNode("route53_web", 15*time.Millisecond, nil)
	dag4.AddNode("route53_api", 15*time.Millisecond, nil)
	dag4.AddNode("outputs", 5*time.Millisecond, nil)

	// 의존성 설정
	dag4.AddDependency("vpc_main", "provider_aws")
	dag4.AddDependency("vpc_staging", "provider_aws")
	dag4.AddDependency("iam_role", "provider_aws")

	dag4.AddDependency("subnet_pub_1", "vpc_main")
	dag4.AddDependency("subnet_pub_2", "vpc_main")
	dag4.AddDependency("subnet_priv_1", "vpc_main")
	dag4.AddDependency("sg_web", "vpc_main")
	dag4.AddDependency("sg_db", "vpc_main")

	dag4.AddDependency("web_1", "subnet_pub_1")
	dag4.AddDependency("web_1", "sg_web")
	dag4.AddDependency("web_1", "iam_role")
	dag4.AddDependency("web_2", "subnet_pub_2")
	dag4.AddDependency("web_2", "sg_web")
	dag4.AddDependency("web_2", "iam_role")
	dag4.AddDependency("api_server", "subnet_priv_1")
	dag4.AddDependency("api_server", "sg_web")
	dag4.AddDependency("api_server", "iam_role")
	dag4.AddDependency("rds_primary", "subnet_priv_1")
	dag4.AddDependency("rds_primary", "sg_db")
	dag4.AddDependency("rds_replica", "rds_primary")

	dag4.AddDependency("alb_web", "web_1")
	dag4.AddDependency("alb_web", "web_2")
	dag4.AddDependency("alb_api", "api_server")
	dag4.AddDependency("elasticache", "subnet_priv_1")
	dag4.AddDependency("elasticache", "sg_db")

	dag4.AddDependency("route53_web", "alb_web")
	dag4.AddDependency("route53_api", "alb_api")
	dag4.AddDependency("outputs", "route53_web")
	dag4.AddDependency("outputs", "route53_api")
	dag4.AddDependency("outputs", "rds_replica")
	dag4.AddDependency("outputs", "elasticache")

	fmt.Println("  그래프 정보:")
	fmt.Printf("    노드 수: %d\n", len(dag4.nodes))
	fmt.Printf("    루트 노드: %v\n", dag4.RootNodes())
	fmt.Println()

	walker4 := NewWalker(dag4, 4)
	records4 := walker4.Walk()
	PrintTimeline(records4, 4)
	PrintExecutionSummary(records4)

	// ─── 예제 5: 다른 병렬성 설정으로 동일 그래프 비교 ───
	printSeparator("5. 병렬성 설정에 따른 성능 비교")

	for _, p := range []int{1, 2, 4, 8} {
		// 같은 그래프 재구성
		dag5 := NewDAG()
		dag5.AddNode("A", 30*time.Millisecond, nil)
		dag5.AddNode("B", 30*time.Millisecond, nil)
		dag5.AddNode("C", 30*time.Millisecond, nil)
		dag5.AddNode("D", 30*time.Millisecond, nil)
		dag5.AddNode("E", 30*time.Millisecond, nil)
		dag5.AddNode("F", 30*time.Millisecond, nil)
		dag5.AddNode("G", 30*time.Millisecond, nil)
		dag5.AddNode("H", 30*time.Millisecond, nil)

		// A,B,C,D는 독립적
		// E는 A에 의존
		// F는 B에 의존
		// G는 C,D에 의존
		// H는 E,F,G에 의존
		dag5.AddDependency("E", "A")
		dag5.AddDependency("F", "B")
		dag5.AddDependency("G", "C")
		dag5.AddDependency("G", "D")
		dag5.AddDependency("H", "E")
		dag5.AddDependency("H", "F")
		dag5.AddDependency("H", "G")

		walker5 := NewWalker(dag5, p)
		records5 := walker5.Walk()

		var maxEnd time.Duration
		totalWork := time.Duration(0)
		for _, r := range records5 {
			if r.EndTime > maxEnd {
				maxEnd = r.EndTime
			}
			if !r.Skipped {
				totalWork += r.Duration
			}
		}

		speedup := float64(totalWork) / float64(maxEnd)
		fmt.Printf("  parallelism=%-2d  실행시간: %-8v  순차합계: %-8v  속도향상: %.1fx\n",
			p, maxEnd.Truncate(time.Millisecond), totalWork, speedup)
	}

	// ─── 아키텍처 요약 ───
	printSeparator("동시성 그래프 워커 아키텍처 요약")
	fmt.Print(`
  Terraform Graph Walker 동작 원리:

  ┌──────────────────────────────────────────────────────┐
  │                    DAG (의존성 그래프)                 │
  │                                                       │
  │    provider → vpc → subnet_a → instance_a ─┐         │
  │                   → subnet_b → instance_b ──→ output  │
  └──────────────────────────────┬─────────────────────────┘
                                 │
                                 ▼
  ┌──────────────────────────────────────────────────────┐
  │                  Walker.Walk()                        │
  │                                                       │
  │  1. 모든 노드에 goroutine 할당                       │
  │  2. 각 goroutine은 의존성 완료 대기                   │
  │  3. 세마포어로 동시 실행 수 제한                       │
  │  4. 에러 발생 시 의존 노드에 전파                      │
  └──────────────────────────────┬─────────────────────────┘
                                 │
                                 ▼
  ┌──────────────────────────────────────────────────────┐
  │               세마포어 (Semaphore)                     │
  │                                                       │
  │  parallelism = 4                                      │
  │  ┌───┐ ┌───┐ ┌───┐ ┌───┐                            │
  │  │ 1 │ │ 2 │ │ 3 │ │ 4 │  ← 동시 실행 슬롯          │
  │  └───┘ └───┘ └───┘ └───┘                            │
  │                                                       │
  │  → 슬롯이 모두 차면 대기                               │
  │  → 작업 완료 시 슬롯 반환                              │
  └──────────────────────────────────────────────────────┘

  에러 전파:

    provider ✓ → vpc ✓ → subnet_a ✗ → instance_a [SKIP]
                       → subnet_b ✓ → instance_b ✓ → output [SKIP]
                                                      (instance_a 건너뜀)

  실제 코드:
    internal/dag/walk.go       Walker 구현체
    internal/terraform/graph_walk_operation.go  Operation 워커
`)
}
