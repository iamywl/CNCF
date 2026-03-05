// poc-04-scheduler: 쿠버네티스 스케줄링 프레임워크 시뮬레이션
//
// K8s 스케줄러의 플러그인 기반 프레임워크를 구현한다.
// 스케줄링 사이클은 다음 단계를 거친다:
//
//   SchedulingQueue → Filter(병렬) → Score → NormalizeScore → SelectHost → Bind
//
// 참조 소스:
//   - staging/src/k8s.io/kube-scheduler/framework/interface.go (Plugin interfaces)
//   - pkg/scheduler/scheduler.go (scheduleOne)
//   - pkg/scheduler/framework/runtime/framework.go (runFilterPlugins, runScorePlugins)
//   - pkg/scheduler/framework/plugins/noderesources/fit.go (NodeResourcesFit)
//   - pkg/scheduler/framework/plugins/nodeaffinity/node_affinity.go (NodeAffinity)
//   - pkg/scheduler/internal/queue/scheduling_queue.go (PriorityQueue)
//
// 실행: go run main.go
package main

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// 데이터 모델
// ============================================================================

// Resource는 CPU(밀리코어)와 Memory(Mi) 리소스를 나타낸다.
type Resource struct {
	MilliCPU int64
	MemoryMi int64
}

// Pod는 스케줄링 대상 파드이다.
type Pod struct {
	Name      string
	Namespace string
	Priority  int32             // 높을수록 먼저 스케줄링
	Labels    map[string]string
	NodeName  string // 스케줄러가 배정
	// 리소스 요청량
	Requests Resource
	// 노드 어피니티 조건
	RequiredNodeLabels  map[string]string // 필수 조건 (requiredDuringSchedulingIgnoredDuringExecution)
	PreferredNodeLabels map[string]string // 선호 조건 (preferredDuringSchedulingIgnoredDuringExecution)
}

// Node는 클러스터의 워커 노드이다.
type Node struct {
	Name        string
	Labels      map[string]string
	Capacity    Resource // 노드 전체 용량
	Allocatable Resource // 할당 가능한 용량
	Allocated   Resource // 이미 할당된 양
}

// Available은 노드에 남은 리소스를 반환한다.
func (n *Node) Available() Resource {
	return Resource{
		MilliCPU: n.Allocatable.MilliCPU - n.Allocated.MilliCPU,
		MemoryMi: n.Allocatable.MemoryMi - n.Allocated.MemoryMi,
	}
}

// ============================================================================
// 스케줄링 프레임워크 인터페이스
// 실제 소스: staging/src/k8s.io/kube-scheduler/framework/interface.go
// ============================================================================

// StatusCode는 플러그인 실행 결과 코드이다.
type StatusCode int

const (
	Success       StatusCode = iota
	Unschedulable            // 노드 부적합 (다른 노드 시도)
	Error                    // 내부 오류
)

// Status는 플러그인 실행 결과이다.
type Status struct {
	Code    StatusCode
	Message string
}

// Plugin은 스케줄링 플러그인의 기본 인터페이스이다.
type Plugin interface {
	Name() string
}

// FilterPlugin은 노드 필터링 플러그인이다.
// 부적합한 노드를 걸러낸다.
type FilterPlugin interface {
	Plugin
	Filter(pod *Pod, node *Node) *Status
}

// ScorePlugin은 노드 점수 매기기 플러그인이다.
// 적합한 노드 중에서 최적의 노드를 선택하기 위해 점수를 부여한다.
type ScorePlugin interface {
	Plugin
	Score(pod *Pod, node *Node) (int64, *Status)
	// NormalizeScore는 점수를 0-100 범위로 정규화한다.
	NormalizeScore(scores map[string]int64) *Status
	Weight() int32 // 플러그인 가중치
}

// ============================================================================
// Filter 플러그인: NodeResourcesFit
// 노드에 파드를 실행할 충분한 리소스가 있는지 확인한다.
// 실제 소스: pkg/scheduler/framework/plugins/noderesources/fit.go
// ============================================================================

type NodeResourcesFit struct{}

func (p *NodeResourcesFit) Name() string { return "NodeResourcesFit" }

func (p *NodeResourcesFit) Filter(pod *Pod, node *Node) *Status {
	avail := node.Available()

	if pod.Requests.MilliCPU > avail.MilliCPU {
		return &Status{
			Code:    Unschedulable,
			Message: fmt.Sprintf("CPU 부족: 요청=%dm, 가용=%dm", pod.Requests.MilliCPU, avail.MilliCPU),
		}
	}
	if pod.Requests.MemoryMi > avail.MemoryMi {
		return &Status{
			Code:    Unschedulable,
			Message: fmt.Sprintf("메모리 부족: 요청=%dMi, 가용=%dMi", pod.Requests.MemoryMi, avail.MemoryMi),
		}
	}

	return &Status{Code: Success}
}

// ============================================================================
// Filter 플러그인: NodeAffinity (Required)
// 파드의 nodeSelector/nodeAffinity 조건을 확인한다.
// 실제 소스: pkg/scheduler/framework/plugins/nodeaffinity/node_affinity.go
// ============================================================================

type NodeAffinityFilter struct{}

func (p *NodeAffinityFilter) Name() string { return "NodeAffinity" }

func (p *NodeAffinityFilter) Filter(pod *Pod, node *Node) *Status {
	// RequiredNodeLabels는 모두 매칭되어야 한다
	for key, val := range pod.RequiredNodeLabels {
		nodeVal, ok := node.Labels[key]
		if !ok || nodeVal != val {
			return &Status{
				Code:    Unschedulable,
				Message: fmt.Sprintf("노드 라벨 불일치: %s=%s 필요, 노드 값=%s", key, val, nodeVal),
			}
		}
	}
	return &Status{Code: Success}
}

// ============================================================================
// Score 플러그인: LeastAllocated
// 리소스 사용률이 낮은 노드에 높은 점수를 부여한다 (분산 배치).
// 실제 소스: pkg/scheduler/framework/plugins/noderesources/least_allocated.go
// ============================================================================

type LeastAllocated struct {
	weight int32
}

func (p *LeastAllocated) Name() string { return "LeastAllocated" }
func (p *LeastAllocated) Weight() int32 { return p.weight }

func (p *LeastAllocated) Score(pod *Pod, node *Node) (int64, *Status) {
	// 파드 배정 후 예상 사용률 계산
	cpuAfter := node.Allocated.MilliCPU + pod.Requests.MilliCPU
	memAfter := node.Allocated.MemoryMi + pod.Requests.MemoryMi

	// 남은 비율이 높을수록 점수가 높음
	cpuScore := int64(0)
	if node.Allocatable.MilliCPU > 0 {
		cpuScore = 100 - (cpuAfter * 100 / node.Allocatable.MilliCPU)
	}
	memScore := int64(0)
	if node.Allocatable.MemoryMi > 0 {
		memScore = 100 - (memAfter * 100 / node.Allocatable.MemoryMi)
	}

	// CPU와 메모리 점수의 평균
	score := (cpuScore + memScore) / 2
	if score < 0 {
		score = 0
	}

	return score, &Status{Code: Success}
}

func (p *LeastAllocated) NormalizeScore(scores map[string]int64) *Status {
	// 이미 0-100 범위이므로 별도 정규화 불필요
	return &Status{Code: Success}
}

// ============================================================================
// Score 플러그인: NodeAffinityScore (Preferred)
// 파드가 선호하는 라벨을 가진 노드에 보너스 점수를 준다.
// ============================================================================

type NodeAffinityScore struct {
	weight int32
}

func (p *NodeAffinityScore) Name() string { return "NodeAffinityScore" }
func (p *NodeAffinityScore) Weight() int32 { return p.weight }

func (p *NodeAffinityScore) Score(pod *Pod, node *Node) (int64, *Status) {
	if len(pod.PreferredNodeLabels) == 0 {
		return 0, &Status{Code: Success}
	}

	matched := 0
	for key, val := range pod.PreferredNodeLabels {
		if node.Labels[key] == val {
			matched++
		}
	}

	score := int64(matched * 100 / len(pod.PreferredNodeLabels))
	return score, &Status{Code: Success}
}

func (p *NodeAffinityScore) NormalizeScore(scores map[string]int64) *Status {
	return &Status{Code: Success}
}

// ============================================================================
// SchedulingQueue — 우선순위 기반 스케줄링 큐
// 실제 소스: pkg/scheduler/internal/queue/scheduling_queue.go
//
// K8s 스케줄러는 3개의 큐를 사용한다:
// - activeQ: 즉시 스케줄링 가능한 파드 (PriorityQueue)
// - backoffQ: 실패 후 백오프 대기 중인 파드
// - unschedulableQ: 현재 스케줄링 불가능한 파드
// ============================================================================

type SchedulingQueue struct {
	mu       sync.Mutex
	activeQ  []*Pod // priority 내림차순 정렬
	backoffQ []*Pod
}

func NewSchedulingQueue() *SchedulingQueue {
	return &SchedulingQueue{}
}

// Add는 파드를 우선순위에 따라 activeQ에 삽입한다.
func (q *SchedulingQueue) Add(pod *Pod) {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.activeQ = append(q.activeQ, pod)
	// 우선순위 내림차순 정렬
	sort.Slice(q.activeQ, func(i, j int) bool {
		return q.activeQ[i].Priority > q.activeQ[j].Priority
	})
}

// Pop은 가장 높은 우선순위의 파드를 꺼낸다.
func (q *SchedulingQueue) Pop() *Pod {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.activeQ) == 0 {
		return nil
	}

	pod := q.activeQ[0]
	q.activeQ = q.activeQ[1:]
	return pod
}

// AddToBackoff는 스케줄링 실패한 파드를 백오프 큐로 이동시킨다.
func (q *SchedulingQueue) AddToBackoff(pod *Pod) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.backoffQ = append(q.backoffQ, pod)
}

// Len은 activeQ의 길이를 반환한다.
func (q *SchedulingQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.activeQ)
}

// ============================================================================
// Scheduler — 스케줄링 프레임워크 구현
// 실제 소스: pkg/scheduler/scheduler.go (scheduleOne)
//            pkg/scheduler/framework/runtime/framework.go
// ============================================================================

type Scheduler struct {
	queue         *SchedulingQueue
	nodes         []*Node
	filterPlugins []FilterPlugin
	scorePlugins  []ScorePlugin
}

func NewScheduler(nodes []*Node, filters []FilterPlugin, scorers []ScorePlugin) *Scheduler {
	return &Scheduler{
		queue:         NewSchedulingQueue(),
		nodes:         nodes,
		filterPlugins: filters,
		scorePlugins:  scorers,
	}
}

// ScheduleOne은 하나의 파드를 스케줄링한다.
// 실제 소스: pkg/scheduler/scheduler.go의 scheduleOne 함수
func (s *Scheduler) ScheduleOne(pod *Pod) (string, error) {
	fmt.Printf("\n  ── 스케줄링: %s (priority=%d, cpu=%dm, mem=%dMi) ──\n",
		pod.Name, pod.Priority, pod.Requests.MilliCPU, pod.Requests.MemoryMi)

	// Phase 1: Filter — 부적합한 노드 제거 (병렬 실행)
	feasibleNodes := s.runFilterPlugins(pod)
	if len(feasibleNodes) == 0 {
		return "", fmt.Errorf("스케줄링 불가: 모든 노드가 필터에 의해 제거됨")
	}

	fmt.Printf("  [Filter 결과] 적합한 노드: %v\n", nodeNames(feasibleNodes))

	// 적합한 노드가 1개면 바로 선택
	if len(feasibleNodes) == 1 {
		selected := feasibleNodes[0].Name
		fmt.Printf("  [선택] 적합한 노드 1개 → %s 즉시 선택\n", selected)
		return selected, nil
	}

	// Phase 2: Score — 적합한 노드에 점수 부여
	nodeScores := s.runScorePlugins(pod, feasibleNodes)

	// Phase 3: SelectHost — 최고 점수 노드 선택 (동점이면 랜덤)
	selected := s.selectHost(nodeScores)
	return selected, nil
}

// runFilterPlugins는 모든 Filter 플러그인을 병렬로 실행한다.
// 실제 소스: pkg/scheduler/framework/runtime/framework.go의 RunFilterPlugins
func (s *Scheduler) runFilterPlugins(pod *Pod) []*Node {
	fmt.Println("  [Filter] 병렬 필터링 시작...")

	type filterResult struct {
		node    *Node
		pass    bool
		reasons []string
	}

	results := make([]filterResult, len(s.nodes))
	var wg sync.WaitGroup

	for i, node := range s.nodes {
		wg.Add(1)
		go func(idx int, n *Node) {
			defer wg.Done()
			result := filterResult{node: n, pass: true}

			for _, plugin := range s.filterPlugins {
				status := plugin.Filter(pod, n)
				if status.Code != Success {
					result.pass = false
					result.reasons = append(result.reasons,
						fmt.Sprintf("%s: %s", plugin.Name(), status.Message))
				}
			}
			results[idx] = result
		}(i, node)
	}

	wg.Wait()

	var feasible []*Node
	for _, r := range results {
		if r.pass {
			fmt.Printf("    [통과] %s\n", r.node.Name)
			feasible = append(feasible, r.node)
		} else {
			fmt.Printf("    [제외] %s — %s\n", r.node.Name, strings.Join(r.reasons, "; "))
		}
	}

	return feasible
}

// runScorePlugins는 Score 플러그인을 실행하고 가중치를 적용한다.
// 실제 소스: pkg/scheduler/framework/runtime/framework.go의 RunScorePlugins
func (s *Scheduler) runScorePlugins(pod *Pod, nodes []*Node) map[string]int64 {
	fmt.Println("  [Score] 점수 산출 시작...")

	// 각 플러그인별 점수 계산
	pluginScores := make([]map[string]int64, len(s.scorePlugins))
	for i, plugin := range s.scorePlugins {
		pluginScores[i] = make(map[string]int64)
		for _, node := range nodes {
			score, status := plugin.Score(pod, node)
			if status.Code == Success {
				pluginScores[i][node.Name] = score
			}
		}
		// NormalizeScore 실행
		plugin.NormalizeScore(pluginScores[i])
	}

	// 가중 합계 계산
	finalScores := make(map[string]int64)
	for _, node := range nodes {
		var total int64
		details := make([]string, 0)
		for i, plugin := range s.scorePlugins {
			weighted := pluginScores[i][node.Name] * int64(plugin.Weight())
			total += weighted
			details = append(details, fmt.Sprintf("%s=%d*%d", plugin.Name(), pluginScores[i][node.Name], plugin.Weight()))
		}
		finalScores[node.Name] = total
		fmt.Printf("    %s: 총점=%d (%s)\n", node.Name, total, strings.Join(details, ", "))
	}

	return finalScores
}

// selectHost는 최고 점수를 가진 노드를 선택한다.
// 동점인 경우 랜덤으로 선택한다.
func (s *Scheduler) selectHost(scores map[string]int64) string {
	var maxScore int64 = math.MinInt64
	var candidates []string

	for name, score := range scores {
		if score > maxScore {
			maxScore = score
			candidates = []string{name}
		} else if score == maxScore {
			candidates = append(candidates, name)
		}
	}

	selected := candidates[rand.Intn(len(candidates))]
	fmt.Printf("  [선택] 최고 점수 노드: %s (점수=%d)\n", selected, maxScore)
	return selected
}

// nodeNames는 노드 이름 목록을 반환한다.
func nodeNames(nodes []*Node) []string {
	names := make([]string, len(nodes))
	for i, n := range nodes {
		names[i] = n.Name
	}
	return names
}

// ============================================================================
// 메인
// ============================================================================

func main() {
	fmt.Println("========================================")
	fmt.Println("Kubernetes 스케줄링 프레임워크 시뮬레이션")
	fmt.Println("========================================")
	fmt.Println()
	fmt.Println("스케줄링 사이클:")
	fmt.Println("  Queue(우선순위) → Filter(병렬) → Score(가중합) → SelectHost → Bind")
	fmt.Println()

	// ── 노드 설정 ──
	nodes := []*Node{
		{
			Name:        "node-1",
			Labels:      map[string]string{"zone": "us-east-1a", "disk": "ssd", "gpu": "true"},
			Capacity:    Resource{MilliCPU: 4000, MemoryMi: 8192},
			Allocatable: Resource{MilliCPU: 3800, MemoryMi: 7680},
			Allocated:   Resource{MilliCPU: 2000, MemoryMi: 4096}, // 이미 절반 사용 중
		},
		{
			Name:        "node-2",
			Labels:      map[string]string{"zone": "us-east-1b", "disk": "ssd"},
			Capacity:    Resource{MilliCPU: 4000, MemoryMi: 8192},
			Allocatable: Resource{MilliCPU: 3800, MemoryMi: 7680},
			Allocated:   Resource{MilliCPU: 500, MemoryMi: 1024}, // 거의 비어있음
		},
		{
			Name:        "node-3",
			Labels:      map[string]string{"zone": "us-west-2a", "disk": "hdd"},
			Capacity:    Resource{MilliCPU: 2000, MemoryMi: 4096},
			Allocatable: Resource{MilliCPU: 1800, MemoryMi: 3584},
			Allocated:   Resource{MilliCPU: 1500, MemoryMi: 3000}, // 거의 가득 참
		},
		{
			Name:        "node-4",
			Labels:      map[string]string{"zone": "us-east-1a", "disk": "ssd"},
			Capacity:    Resource{MilliCPU: 8000, MemoryMi: 16384},
			Allocatable: Resource{MilliCPU: 7600, MemoryMi: 15872},
			Allocated:   Resource{MilliCPU: 1000, MemoryMi: 2048}, // 대형 노드, 거의 비어있음
		},
	}

	fmt.Println("노드 상태:")
	fmt.Println("  이름     | 라벨                              | CPU(가용/전체)    | MEM(가용/전체)")
	fmt.Println("  ---------+-----------------------------------+------------------+------------------")
	for _, n := range nodes {
		avail := n.Available()
		labelStr := ""
		for k, v := range n.Labels {
			if labelStr != "" {
				labelStr += ", "
			}
			labelStr += k + "=" + v
		}
		fmt.Printf("  %-8s | %-33s | %4dm/%4dm       | %5dMi/%5dMi\n",
			n.Name, labelStr, avail.MilliCPU, n.Allocatable.MilliCPU, avail.MemoryMi, n.Allocatable.MemoryMi)
	}

	// ── 플러그인 설정 ──
	filterPlugins := []FilterPlugin{
		&NodeResourcesFit{},
		&NodeAffinityFilter{},
	}
	scorePlugins := []ScorePlugin{
		&LeastAllocated{weight: 1},
		&NodeAffinityScore{weight: 2},
	}

	scheduler := NewScheduler(nodes, filterPlugins, scorePlugins)

	// ── 파드 큐에 추가 (우선순위 역순으로) ──
	pods := []*Pod{
		{
			Name:      "low-priority-job",
			Priority:  10,
			Requests:  Resource{MilliCPU: 100, MemoryMi: 128},
			Labels:    map[string]string{"app": "batch"},
		},
		{
			Name:     "gpu-training",
			Priority: 100,
			Requests: Resource{MilliCPU: 1000, MemoryMi: 2048},
			Labels:   map[string]string{"app": "ml"},
			// GPU 노드 필요 (node-1만 gpu=true)
			RequiredNodeLabels: map[string]string{"gpu": "true"},
		},
		{
			Name:     "web-frontend",
			Priority: 50,
			Requests: Resource{MilliCPU: 500, MemoryMi: 512},
			Labels:   map[string]string{"app": "web"},
			// SSD 선호, us-east 선호
			PreferredNodeLabels: map[string]string{"disk": "ssd", "zone": "us-east-1a"},
		},
		{
			Name:     "resource-hungry",
			Priority: 80,
			Requests: Resource{MilliCPU: 3000, MemoryMi: 8192},
			Labels:   map[string]string{"app": "bigdata"},
		},
	}

	for _, pod := range pods {
		scheduler.queue.Add(pod)
	}

	fmt.Println()
	fmt.Println("파드 큐 (우선순위 순):")
	// 큐 상태를 보여주기 위해 다시 넣기
	orderedPods := make([]*Pod, 0)
	for scheduler.queue.Len() > 0 {
		p := scheduler.queue.Pop()
		fmt.Printf("  [%d] %s (cpu=%dm, mem=%dMi)\n", p.Priority, p.Name, p.Requests.MilliCPU, p.Requests.MemoryMi)
		orderedPods = append(orderedPods, p)
	}

	// ── 스케줄링 실행 ──
	fmt.Println()
	fmt.Println("========================================")
	fmt.Println("스케줄링 실행")
	fmt.Println("========================================")

	results := make(map[string]string)
	var failures []string

	for _, pod := range orderedPods {
		selectedNode, err := scheduler.ScheduleOne(pod)
		if err != nil {
			fmt.Printf("  [실패] %s: %v\n", pod.Name, err)
			failures = append(failures, pod.Name)
		} else {
			pod.NodeName = selectedNode
			results[pod.Name] = selectedNode

			// 노드 리소스 할당 업데이트 (Assume 단계)
			for _, n := range nodes {
				if n.Name == selectedNode {
					n.Allocated.MilliCPU += pod.Requests.MilliCPU
					n.Allocated.MemoryMi += pod.Requests.MemoryMi
					break
				}
			}
			fmt.Printf("  [바인드] %s → %s\n", pod.Name, selectedNode)
		}
	}

	// ── 최종 결과 ──
	fmt.Println()
	fmt.Println("========================================")
	fmt.Println("스케줄링 결과")
	fmt.Println("========================================")
	for name, node := range results {
		fmt.Printf("  %-20s → %s\n", name, node)
	}
	for _, name := range failures {
		fmt.Printf("  %-20s → 스케줄링 실패 (Unschedulable)\n", name)
	}

	fmt.Println()
	fmt.Println("노드 최종 리소스:")
	for _, n := range nodes {
		avail := n.Available()
		cpuPct := float64(n.Allocated.MilliCPU) * 100 / float64(n.Allocatable.MilliCPU)
		memPct := float64(n.Allocated.MemoryMi) * 100 / float64(n.Allocatable.MemoryMi)
		fmt.Printf("  %s: CPU=%dm/%dm(%.0f%%), MEM=%dMi/%dMi(%.0f%%), 가용: CPU=%dm, MEM=%dMi\n",
			n.Name, n.Allocated.MilliCPU, n.Allocatable.MilliCPU, cpuPct,
			n.Allocated.MemoryMi, n.Allocatable.MemoryMi, memPct,
			avail.MilliCPU, avail.MemoryMi)
	}

	fmt.Println()
	fmt.Println("========================================")
	fmt.Println("핵심 포인트:")
	fmt.Println("========================================")
	fmt.Println("1. SchedulingQueue: 우선순위 기반 정렬 → 높은 Priority 파드 먼저 스케줄링")
	fmt.Println("2. Filter(병렬): NodeResourcesFit(리소스), NodeAffinity(라벨) → 부적합 노드 제거")
	fmt.Println("3. Score(가중합): LeastAllocated(분산배치) + NodeAffinityScore(선호노드) → 최적 노드 선택")
	fmt.Println("4. 플러그인 아키텍처: Filter/Score 인터페이스 → 확장 가능한 설계")
	fmt.Println("5. 동시성: Filter 플러그인은 goroutine으로 병렬 실행")
	fmt.Println("6. Assume: 바인드 전에 노드 리소스를 예약 → 다음 파드 스케줄링에 반영")

	_ = time.Now() // 사용하지 않는 import 방지
}
