package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Kubernetes Pod Lifecycle, QoS, Eviction, Preemption 시뮬레이션
// 실제 구현 참조:
//   - QoS: pkg/apis/core/v1/helper/qos/qos.go (ComputePodQOS)
//   - OOM Score: pkg/kubelet/qos/policy.go (GetContainerOOMScoreAdjust)
//   - Eviction: pkg/kubelet/eviction/eviction_manager.go (synchronize)
//   - Preemption: pkg/scheduler/framework/plugins/defaultpreemption/default_preemption.go
//   - Priority: pkg/apis/scheduling/types.go
// =============================================================================

// --- Pod Phase 상태 머신 ---

type PodPhase string

const (
	PodPending   PodPhase = "Pending"
	PodRunning   PodPhase = "Running"
	PodSucceeded PodPhase = "Succeeded"
	PodFailed    PodPhase = "Failed"
)

type QoSClass string

const (
	Guaranteed QoSClass = "Guaranteed"
	Burstable  QoSClass = "Burstable"
	BestEffort QoSClass = "BestEffort"
)

// --- Resource 정의 ---

type ResourceList struct {
	CPUMillis    int64 // millicores
	MemoryBytes  int64 // bytes
}

type Container struct {
	Name     string
	Requests ResourceList
	Limits   ResourceList
}

type PodSpec struct {
	Containers                    []Container
	InitContainers                []Container
	PriorityClassName             string
	Priority                      int32
	PreemptionPolicy              string // "PreemptLowerPriority" or "Never"
	TerminationGracePeriodSeconds int64
}

type PodConditionType string

const (
	PodScheduled    PodConditionType = "PodScheduled"
	PodInitialized  PodConditionType = "Initialized"
	ContainersReady PodConditionType = "ContainersReady"
	PodReady        PodConditionType = "Ready"
)

type PodCondition struct {
	Type   PodConditionType
	Status bool
}

type Pod struct {
	Name       string
	Namespace  string
	Spec       PodSpec
	Phase      PodPhase
	QoS        QoSClass
	OOMScore   int
	Conditions []PodCondition
	NodeName   string
	StartTime  time.Time
}

// --- QoS 클래스 결정 (ComputePodQOS 알고리즘) ---
// 실제 구현: pkg/apis/core/v1/helper/qos/qos.go:87-169

func ComputePodQOS(pod *Pod) QoSClass {
	allContainers := append(pod.Spec.Containers, pod.Spec.InitContainers...)
	if len(allContainers) == 0 {
		return BestEffort
	}

	requestsSet := false
	limitsSet := false
	allRequestsEqualLimits := true

	for _, c := range allContainers {
		// CPU 확인
		if c.Requests.CPUMillis > 0 {
			requestsSet = true
		}
		if c.Limits.CPUMillis > 0 {
			limitsSet = true
		}
		if c.Requests.CPUMillis != c.Limits.CPUMillis {
			allRequestsEqualLimits = false
		}

		// Memory 확인
		if c.Requests.MemoryBytes > 0 {
			requestsSet = true
		}
		if c.Limits.MemoryBytes > 0 {
			limitsSet = true
		}
		if c.Requests.MemoryBytes != c.Limits.MemoryBytes {
			allRequestsEqualLimits = false
		}
	}

	// BestEffort: requests == 0 && limits == 0
	if !requestsSet && !limitsSet {
		return BestEffort
	}

	// Guaranteed: 모든 컨테이너에서 requests == limits (CPU, Memory 둘 다)
	if allRequestsEqualLimits && requestsSet && limitsSet {
		return Guaranteed
	}

	// 나머지: Burstable
	return Burstable
}

// --- OOM Score 조정 ---
// 실제 구현: pkg/kubelet/qos/policy.go:45-120

const (
	guaranteedOOMScoreAdj = -997
	besteffortOOMScoreAdj = 1000
)

func GetContainerOOMScoreAdjust(pod *Pod, container *Container, memoryCapacity int64) int {
	qos := ComputePodQOS(pod)

	switch qos {
	case Guaranteed:
		return guaranteedOOMScoreAdj
	case BestEffort:
		return besteffortOOMScoreAdj
	default: // Burstable
		memReq := container.Requests.MemoryBytes
		if memReq == 0 || memoryCapacity == 0 {
			return besteffortOOMScoreAdj
		}
		// 1000 - (1000 * containerMemReq / memoryCapacity)
		adj := 1000 - int(1000*memReq/memoryCapacity)
		if adj < 2 {
			adj = 2 // 최소 2 (Guaranteed보다는 높게)
		}
		if adj > besteffortOOMScoreAdj {
			adj = besteffortOOMScoreAdj
		}
		return adj
	}
}

// --- Eviction Manager ---
// 실제 구현: pkg/kubelet/eviction/eviction_manager.go:248-400+

type EvictionSignal string

const (
	SignalMemoryAvailable EvictionSignal = "memory.available"
	SignalNodeFsAvailable EvictionSignal = "nodefs.available"
	SignalPIDAvailable    EvictionSignal = "pid.available"
)

type Threshold struct {
	Signal   EvictionSignal
	Value    int64 // bytes or count
	Operator string
}

type NodeCondition string

const (
	NodeMemoryPressure NodeCondition = "MemoryPressure"
	NodeDiskPressure   NodeCondition = "DiskPressure"
	NodePIDPressure    NodeCondition = "PIDPressure"
)

// Signal → Condition 매핑 (helpers.go:84-108)
var signalToCondition = map[EvictionSignal]NodeCondition{
	SignalMemoryAvailable: NodeMemoryPressure,
	SignalNodeFsAvailable: NodeDiskPressure,
	SignalPIDAvailable:    NodePIDPressure,
}

type EvictionManager struct {
	mu         sync.Mutex
	thresholds []Threshold
	pods       []*Pod
}

func NewEvictionManager(thresholds []Threshold) *EvictionManager {
	return &EvictionManager{thresholds: thresholds}
}

func (em *EvictionManager) Synchronize(currentResources map[EvictionSignal]int64) []string {
	em.mu.Lock()
	defer em.mu.Unlock()

	var evicted []string
	var conditions []NodeCondition

	// 1. 임계값 위반 확인
	for _, t := range em.thresholds {
		available, ok := currentResources[t.Signal]
		if !ok {
			continue
		}
		if available < t.Value {
			conditions = append(conditions, signalToCondition[t.Signal])
			fmt.Printf("  [Eviction] %s 압박 감지: %d < %d\n", t.Signal, available, t.Value)
		}
	}

	if len(conditions) == 0 {
		return nil
	}

	// 2. Pod 정렬 (QoS 기반: BestEffort → Burstable → Guaranteed)
	sortedPods := make([]*Pod, len(em.pods))
	copy(sortedPods, em.pods)
	sort.Slice(sortedPods, func(i, j int) bool {
		return qosPriority(sortedPods[i].QoS) < qosPriority(sortedPods[j].QoS)
	})

	// 3. 가장 낮은 QoS부터 축출
	for _, pod := range sortedPods {
		if pod.Phase != PodRunning {
			continue
		}
		pod.Phase = PodFailed
		evicted = append(evicted, pod.Name)
		fmt.Printf("  [Eviction] Pod '%s' (QoS=%s) 축출됨\n", pod.Name, pod.QoS)
		break // 하나만 축출 후 재평가
	}

	return evicted
}

func qosPriority(qos QoSClass) int {
	switch qos {
	case BestEffort:
		return 0
	case Burstable:
		return 1
	case Guaranteed:
		return 2
	default:
		return 0
	}
}

// --- Preemption (선점) ---
// 실제 구현: pkg/scheduler/framework/plugins/defaultpreemption/default_preemption.go:207-309

type PreemptionEvaluator struct {
	nodes []*Node
}

type Node struct {
	Name         string
	CPUMillis    int64
	MemoryBytes  int64
	Pods         []*Pod
	UsedCPU      int64
	UsedMemory   int64
}

func (n *Node) AvailableCPU() int64    { return n.CPUMillis - n.UsedCPU }
func (n *Node) AvailableMemory() int64 { return n.MemoryBytes - n.UsedMemory }

// SelectVictimsOnNode: 피해자 선택 알고리즘
func SelectVictimsOnNode(node *Node, preemptor *Pod) ([]*Pod, bool) {
	// 1. 선점 가능한 Pod 필터링 (우선순위가 낮은 것만)
	var candidates []*Pod
	for _, p := range node.Pods {
		if p.Spec.Priority < preemptor.Spec.Priority {
			candidates = append(candidates, p)
		}
	}

	// 2. 우선순위 오름차순 정렬 (가장 낮은 것부터 제거)
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Spec.Priority < candidates[j].Spec.Priority
	})

	// 3. 자원이 충분해질 때까지 Pod 제거
	var victims []*Pod
	freedCPU := node.AvailableCPU()
	freedMem := node.AvailableMemory()

	for _, victim := range candidates {
		if freedCPU >= preemptor.Spec.Containers[0].Requests.CPUMillis &&
			freedMem >= preemptor.Spec.Containers[0].Requests.MemoryBytes {
			break // 충분한 리소스 확보
		}
		victims = append(victims, victim)
		freedCPU += victim.Spec.Containers[0].Requests.CPUMillis
		freedMem += victim.Spec.Containers[0].Requests.MemoryBytes
	}

	schedulable := freedCPU >= preemptor.Spec.Containers[0].Requests.CPUMillis &&
		freedMem >= preemptor.Spec.Containers[0].Requests.MemoryBytes

	return victims, schedulable
}

// PodEligibleToPreemptOthers: 선점 자격 확인
func PodEligibleToPreemptOthers(pod *Pod) bool {
	if pod.Spec.PreemptionPolicy == "Never" {
		return false
	}
	return true
}

// --- Priority Sort ---
// 실제 구현: pkg/scheduler/framework/plugins/queuesort/priority_sort.go:44-48

func PrioritySort(pods []*Pod) {
	sort.Slice(pods, func(i, j int) bool {
		if pods[i].Spec.Priority != pods[j].Spec.Priority {
			return pods[i].Spec.Priority > pods[j].Spec.Priority // 내림차순
		}
		return pods[i].StartTime.Before(pods[j].StartTime) // 타임스탬프 오름차순
	})
}

// --- Graceful Termination ---

func GracefulShutdown(pod *Pod) {
	grace := pod.Spec.TerminationGracePeriodSeconds
	if grace == 0 {
		grace = 30 // 기본값
	}
	fmt.Printf("  [Termination] Pod '%s' graceful shutdown (grace=%ds)\n", pod.Name, grace)
	fmt.Printf("  [Termination] 1. Main containers → SIGTERM\n")
	fmt.Printf("  [Termination] 2. Sidecar containers → SIGTERM (main 종료 후)\n")
	fmt.Printf("  [Termination] 3. %ds 후 → SIGKILL (미종료 시)\n", grace)
	pod.Phase = PodSucceeded
}

// =============================================================================
// 데모
// =============================================================================

func main() {
	fmt.Println("=== Kubernetes Pod Lifecycle 시뮬레이션 ===")
	fmt.Println()

	// 1. QoS 클래스 결정
	demo1_QoS()

	// 2. OOM Score 조정
	demo2_OOMScore()

	// 3. Eviction Manager
	demo3_Eviction()

	// 4. Preemption (선점)
	demo4_Preemption()

	// 5. Priority Sort
	demo5_PrioritySort()

	// 6. Graceful Shutdown
	demo6_GracefulShutdown()

	// 핵심 정리
	printSummary()
}

func demo1_QoS() {
	fmt.Println("--- 1. QoS 클래스 결정 ---")

	pods := []Pod{
		{
			Name: "guaranteed-pod",
			Spec: PodSpec{Containers: []Container{
				{Name: "app", Requests: ResourceList{500, 512 * 1024 * 1024}, Limits: ResourceList{500, 512 * 1024 * 1024}},
			}},
		},
		{
			Name: "burstable-pod",
			Spec: PodSpec{Containers: []Container{
				{Name: "app", Requests: ResourceList{250, 256 * 1024 * 1024}, Limits: ResourceList{500, 512 * 1024 * 1024}},
			}},
		},
		{
			Name: "besteffort-pod",
			Spec: PodSpec{Containers: []Container{
				{Name: "app", Requests: ResourceList{0, 0}, Limits: ResourceList{0, 0}},
			}},
		},
	}

	for _, pod := range pods {
		qos := ComputePodQOS(&pod)
		fmt.Printf("  %-20s → QoS=%s\n", pod.Name, qos)
	}
	fmt.Println()
}

func demo2_OOMScore() {
	fmt.Println("--- 2. OOM Score 조정 ---")
	nodeMemory := int64(8 * 1024 * 1024 * 1024) // 8 GiB

	testCases := []struct {
		name string
		pod  Pod
	}{
		{
			name: "Guaranteed (512Mi req=limit)",
			pod: Pod{Spec: PodSpec{Containers: []Container{
				{Name: "app", Requests: ResourceList{500, 512 * 1024 * 1024}, Limits: ResourceList{500, 512 * 1024 * 1024}},
			}}},
		},
		{
			name: "Burstable (256Mi request)",
			pod: Pod{Spec: PodSpec{Containers: []Container{
				{Name: "app", Requests: ResourceList{250, 256 * 1024 * 1024}, Limits: ResourceList{500, 512 * 1024 * 1024}},
			}}},
		},
		{
			name: "BestEffort (no request)",
			pod: Pod{Spec: PodSpec{Containers: []Container{
				{Name: "app", Requests: ResourceList{0, 0}, Limits: ResourceList{0, 0}},
			}}},
		},
	}

	for _, tc := range testCases {
		score := GetContainerOOMScoreAdjust(&tc.pod, &tc.pod.Spec.Containers[0], nodeMemory)
		fmt.Printf("  %-35s → OOM Score Adj = %d\n", tc.name, score)
	}
	fmt.Println()
}

func demo3_Eviction() {
	fmt.Println("--- 3. Eviction Manager ---")

	em := NewEvictionManager([]Threshold{
		{Signal: SignalMemoryAvailable, Value: 500 * 1024 * 1024}, // 500Mi
	})

	pods := []*Pod{
		{Name: "critical-app", Phase: PodRunning, QoS: Guaranteed},
		{Name: "web-server", Phase: PodRunning, QoS: Burstable},
		{Name: "batch-job", Phase: PodRunning, QoS: BestEffort},
	}
	em.pods = pods

	// 메모리 압박 시뮬레이션
	fmt.Println("  메모리 가용량: 300Mi (임계값 500Mi)")
	evicted := em.Synchronize(map[EvictionSignal]int64{
		SignalMemoryAvailable: 300 * 1024 * 1024,
	})
	fmt.Printf("  축출된 Pod: %v\n", evicted)
	fmt.Println()
}

func demo4_Preemption() {
	fmt.Println("--- 4. Preemption (선점) ---")

	node := &Node{
		Name:        "worker-1",
		CPUMillis:   4000,
		MemoryBytes: 8 * 1024 * 1024 * 1024,
		UsedCPU:     3500,
		UsedMemory:  7 * 1024 * 1024 * 1024,
		Pods: []*Pod{
			{Name: "low-priority", Spec: PodSpec{Priority: 100, Containers: []Container{
				{Requests: ResourceList{1000, 2 * 1024 * 1024 * 1024}},
			}}, Phase: PodRunning},
			{Name: "mid-priority", Spec: PodSpec{Priority: 500, Containers: []Container{
				{Requests: ResourceList{1500, 3 * 1024 * 1024 * 1024}},
			}}, Phase: PodRunning},
			{Name: "high-priority", Spec: PodSpec{Priority: 1000, Containers: []Container{
				{Requests: ResourceList{1000, 2 * 1024 * 1024 * 1024}},
			}}, Phase: PodRunning},
		},
	}

	// 높은 우선순위 Pod 스케줄링 시도
	preemptor := &Pod{
		Name: "system-critical",
		Spec: PodSpec{Priority: 2000, Containers: []Container{
			{Requests: ResourceList{1000, 2 * 1024 * 1024 * 1024}},
		}},
	}

	fmt.Printf("  선점자: '%s' (Priority=%d, CPU=1000m, Mem=2Gi)\n",
		preemptor.Name, preemptor.Spec.Priority)
	fmt.Printf("  자격 확인: %v\n", PodEligibleToPreemptOthers(preemptor))

	victims, schedulable := SelectVictimsOnNode(node, preemptor)
	fmt.Printf("  스케줄 가능: %v\n", schedulable)
	for _, v := range victims {
		fmt.Printf("  피해자: '%s' (Priority=%d)\n", v.Name, v.Spec.Priority)
	}
	fmt.Println()
}

func demo5_PrioritySort() {
	fmt.Println("--- 5. Priority Sort (스케줄링 큐) ---")

	now := time.Now()
	pods := []*Pod{
		{Name: "pod-a", Spec: PodSpec{Priority: 100}, StartTime: now.Add(-1 * time.Minute)},
		{Name: "pod-b", Spec: PodSpec{Priority: 2000000000}, StartTime: now}, // system-critical
		{Name: "pod-c", Spec: PodSpec{Priority: 500}, StartTime: now.Add(-2 * time.Minute)},
		{Name: "pod-d", Spec: PodSpec{Priority: 500}, StartTime: now.Add(-3 * time.Minute)},
	}

	PrioritySort(pods)
	fmt.Println("  정렬 결과 (우선순위 내림차순, 동일 시 타임스탬프 오름차순):")
	for i, p := range pods {
		fmt.Printf("    %d. %-10s Priority=%-12d\n", i+1, p.Name, p.Spec.Priority)
	}
	fmt.Println()
}

func demo6_GracefulShutdown() {
	fmt.Println("--- 6. Graceful Shutdown ---")

	pod := &Pod{
		Name:  "web-app",
		Phase: PodRunning,
		Spec: PodSpec{
			TerminationGracePeriodSeconds: 60,
			Containers:                    []Container{{Name: "nginx"}},
			InitContainers:                []Container{{Name: "sidecar-proxy"}},
		},
	}
	GracefulShutdown(pod)
	fmt.Printf("  최종 Phase: %s\n", pod.Phase)
	fmt.Println()
}

func printSummary() {
	fmt.Println("=== 핵심 정리 ===")
	fmt.Println()
	_ = math.MaxInt64
	_ = strings.Join

	items := []string{
		"1. QoS 결정: requests==limits → Guaranteed, 없음 → BestEffort, 나머지 → Burstable",
		"2. OOM Score: Guaranteed=-997, BestEffort=1000, Burstable=1000-(1000*memReq/capacity)",
		"3. Eviction: BestEffort → Burstable → Guaranteed 순서로 축출 (낮은 QoS 우선)",
		"4. Preemption: 낮은 Priority Pod부터 제거하여 높은 Priority Pod 스케줄링",
		"5. Priority Sort: 우선순위 내림차순, 동일 시 대기 시간 오름차순",
		"6. Graceful Shutdown: SIGTERM → grace period → SIGKILL",
	}

	for _, item := range items {
		fmt.Printf("  %s\n", item)
	}

	fmt.Println()
	fmt.Println("소스코드 참조:")
	refs := []string{
		"  - QoS 결정:       pkg/apis/core/v1/helper/qos/qos.go",
		"  - OOM Score:      pkg/kubelet/qos/policy.go",
		"  - Eviction:       pkg/kubelet/eviction/eviction_manager.go",
		"  - Preemption:     pkg/scheduler/framework/plugins/defaultpreemption/default_preemption.go",
		"  - Priority:       pkg/apis/scheduling/types.go",
	}
	for _, r := range refs {
		fmt.Println(r)
	}
}
