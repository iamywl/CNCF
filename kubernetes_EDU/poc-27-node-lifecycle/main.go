// poc-27-node-lifecycle: Node Lifecycle Controller 및 Taint/Toleration 시뮬레이션
//
// 이 프로그램은 Kubernetes Node Lifecycle Controller의 핵심 알고리즘을 표준 라이브러리만으로 구현한다:
//   1. 노드 건강 모니터링 (tryUpdateNodeHealth 로직)
//   2. Taint/Toleration 매칭 알고리즘
//   3. Zone 상태 계산 (Normal/PartialDisruption/FullDisruption)
//   4. Rate-Limited Eviction Queue
//   5. TolerationSeconds 기반 Pod eviction 스케줄링
//
// 실행: go run main.go
package main

import (
	"container/heap"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

// ===========================================================================
// 1. 핵심 데이터 구조 (staging/src/k8s.io/api/core/v1/types.go 기반)
// ===========================================================================

// TaintEffect는 Taint의 효과를 정의한다.
// 실제 소스: staging/src/k8s.io/api/core/v1/types.go line 4047-4068
type TaintEffect string

const (
	TaintEffectNoSchedule       TaintEffect = "NoSchedule"
	TaintEffectPreferNoSchedule TaintEffect = "PreferNoSchedule"
	TaintEffectNoExecute        TaintEffect = "NoExecute"
)

// TolerationOperator는 Toleration 매칭 연산자를 정의한다.
// 실제 소스: staging/src/k8s.io/api/core/v1/types.go line 4102-4109
type TolerationOperator string

const (
	TolerationOpExists TolerationOperator = "Exists"
	TolerationOpEqual  TolerationOperator = "Equal"
)

// Taint는 노드에 부여되는 오염 표시이다.
// 실제 소스: staging/src/k8s.io/api/core/v1/types.go line 4031-4044
type Taint struct {
	Key       string
	Value     string
	Effect    TaintEffect
	TimeAdded time.Time
}

// Toleration은 Pod가 견딜 수 있는 Taint를 선언한다.
// 실제 소스: staging/src/k8s.io/api/core/v1/types.go line 4072-4098
type Toleration struct {
	Key               string
	Operator          TolerationOperator
	Value             string
	Effect            TaintEffect
	TolerationSeconds *int64 // nil이면 무한 허용
}

// ConditionStatus는 노드 조건의 상태를 나타낸다.
type ConditionStatus string

const (
	ConditionTrue    ConditionStatus = "True"
	ConditionFalse   ConditionStatus = "False"
	ConditionUnknown ConditionStatus = "Unknown"
)

// NodeCondition은 노드의 특정 조건 상태를 나타낸다.
type NodeCondition struct {
	Type              string
	Status            ConditionStatus
	LastHeartbeatTime time.Time
}

// NodeHealthData는 컨트롤러가 관리하는 노드별 건강 데이터이다.
// 실제 소스: pkg/controller/nodelifecycle/node_lifecycle_controller.go line 168-173
type NodeHealthData struct {
	ProbeTimestamp           time.Time
	ReadyTransitionTimestamp time.Time
	ReadyConditionStatus     ConditionStatus
}

// ===========================================================================
// 2. Node 및 Pod 구조체
// ===========================================================================

// Node는 클러스터의 워커 노드를 나타낸다.
type Node struct {
	Name       string
	Zone       string
	Taints     []Taint
	Conditions []NodeCondition
	CreatedAt  time.Time
}

// Pod는 클러스터에서 실행 중인 워크로드를 나타낸다.
type Pod struct {
	Name        string
	Namespace   string
	NodeName    string
	Tolerations []Toleration
}

// ===========================================================================
// 3. ZoneState - Zone 상태 관리
// 실제 소스: pkg/controller/nodelifecycle/node_lifecycle_controller.go line 116-124
// ===========================================================================

type ZoneState string

const (
	StateInitial           ZoneState = "Initial"
	StateNormal            ZoneState = "Normal"
	StateFullDisruption    ZoneState = "FullDisruption"
	StatePartialDisruption ZoneState = "PartialDisruption"
)

// ===========================================================================
// 4. Taint/Toleration 매칭 알고리즘
// 실제 소스: staging/src/k8s.io/component-helpers/scheduling/corev1/helpers.go
// ===========================================================================

// TolerationMatchesTaint는 Toleration이 특정 Taint와 매칭되는지 확인한다.
func TolerationMatchesTaint(toleration Toleration, taint Taint) bool {
	// Effect 비교: 빈 Effect는 모든 Effect에 매칭
	if len(toleration.Effect) > 0 && toleration.Effect != taint.Effect {
		return false
	}

	// Key 비교: 빈 Key + Exists이면 모든 Key에 매칭
	if len(toleration.Key) > 0 && toleration.Key != taint.Key {
		return false
	}

	// Operator에 따른 Value 비교
	switch toleration.Operator {
	case TolerationOpExists:
		// Key만 존재하면 매칭 (Value 무시)
		return true
	case TolerationOpEqual, "": // 기본값은 Equal
		return toleration.Value == taint.Value
	default:
		return false
	}
}

// FindUntoleratedTaint는 Pod가 견디지 못하는 Taint를 찾는다.
// 실제 소스의 FindMatchingUntoleratedTaint()에 해당
func FindUntoleratedTaint(taints []Taint, tolerations []Toleration, filter func(Taint) bool) (Taint, bool) {
	for _, taint := range taints {
		if filter != nil && !filter(taint) {
			continue
		}
		tolerated := false
		for _, toleration := range tolerations {
			if TolerationMatchesTaint(toleration, taint) {
				tolerated = true
				break
			}
		}
		if !tolerated {
			return taint, true
		}
	}
	return Taint{}, false
}

// GetMatchingTolerations는 모든 Taint에 대한 매칭 Toleration을 반환한다.
func GetMatchingTolerations(taints []Taint, tolerations []Toleration) (bool, []Toleration) {
	var usedTolerations []Toleration
	for _, taint := range taints {
		matched := false
		for _, toleration := range tolerations {
			if TolerationMatchesTaint(toleration, taint) {
				matched = true
				usedTolerations = append(usedTolerations, toleration)
				break
			}
		}
		if !matched {
			return false, nil
		}
	}
	return true, usedTolerations
}

// GetMinTolerationTime는 사용된 Toleration 중 최소 허용 시간을 반환한다.
// 반환값 < 0이면 무한 허용 (TolerationSeconds 미설정)
// 실제 소스: pkg/controller/tainteviction/taint_eviction.go line 160-182
func GetMinTolerationTime(tolerations []Toleration) time.Duration {
	if len(tolerations) == 0 {
		return 0
	}
	minTime := int64(math.MaxInt64)
	for _, t := range tolerations {
		if t.TolerationSeconds != nil {
			seconds := *t.TolerationSeconds
			if seconds <= 0 {
				return 0 // 즉시 eviction
			}
			if seconds < minTime {
				minTime = seconds
			}
		}
	}
	if minTime == int64(math.MaxInt64) {
		return -1 // 무한 허용
	}
	return time.Duration(minTime) * time.Second
}

// ===========================================================================
// 5. Rate-Limited Timed Queue
// 실제 소스: pkg/controller/nodelifecycle/scheduler/rate_limited_queue.go
// ===========================================================================

// TimedValue는 특정 시간에 처리되어야 하는 값이다.
// 실제 소스: rate_limited_queue.go line 42-48
type TimedValue struct {
	Value     string
	AddedAt   time.Time
	ProcessAt time.Time
}

// TimedQueue는 ProcessAt 기준 min-heap이다.
type TimedQueue []*TimedValue

func (h TimedQueue) Len() int            { return len(h) }
func (h TimedQueue) Less(i, j int) bool  { return h[i].ProcessAt.Before(h[j].ProcessAt) }
func (h TimedQueue) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *TimedQueue) Push(x interface{}) { *h = append(*h, x.(*TimedValue)) }
func (h *TimedQueue) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[0 : n-1]
	return x
}

// RateLimitedTimedQueue는 속도 제한이 적용된 우선순위 큐이다.
// 실제 소스: rate_limited_queue.go line 198-214
type RateLimitedTimedQueue struct {
	mu          sync.Mutex
	queue       TimedQueue
	known       map[string]bool
	qps         float32 // 초당 허용 처리 수
	lastAccept  time.Time
	tokenBucket float32
}

// NewRateLimitedTimedQueue는 새 큐를 생성한다.
func NewRateLimitedTimedQueue(qps float32) *RateLimitedTimedQueue {
	return &RateLimitedTimedQueue{
		queue:       TimedQueue{},
		known:       make(map[string]bool),
		qps:         qps,
		lastAccept:  time.Now(),
		tokenBucket: 1, // burst=1
	}
}

// Add는 큐에 항목을 추가한다. 중복은 무시한다.
func (q *RateLimitedTimedQueue) Add(value string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.known[value] {
		return false
	}
	now := time.Now()
	item := &TimedValue{Value: value, AddedAt: now, ProcessAt: now}
	heap.Push(&q.queue, item)
	q.known[value] = true
	return true
}

// Remove는 큐에서 항목을 제거한다.
func (q *RateLimitedTimedQueue) Remove(value string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.known[value] {
		return false
	}
	delete(q.known, value)
	for i, v := range q.queue {
		if v.Value == value {
			heap.Remove(&q.queue, i)
			return true
		}
	}
	return true
}

// tryAccept는 Token Bucket 알고리즘으로 속도를 제한한다.
func (q *RateLimitedTimedQueue) tryAccept(now time.Time) bool {
	elapsed := now.Sub(q.lastAccept).Seconds()
	q.tokenBucket += float32(elapsed) * q.qps
	if q.tokenBucket > 1 {
		q.tokenBucket = 1 // burst = 1
	}
	q.lastAccept = now
	if q.tokenBucket >= 1 {
		q.tokenBucket -= 1
		return true
	}
	return false
}

// SwapLimiter는 Rate Limiter의 QPS를 변경한다.
func (q *RateLimitedTimedQueue) SwapLimiter(newQPS float32) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.qps = newQPS
}

// ActionFunc는 큐 항목을 처리하는 함수이다. false 반환 시 재시도한다.
type ActionFunc func(TimedValue) (bool, time.Duration)

// Try는 큐의 항목을 처리한다. Rate Limiter에 의해 제한될 수 있다.
// 실제 소스: rate_limited_queue.go line 231-256
func (q *RateLimitedTimedQueue) Try(fn ActionFunc) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	processed := 0
	for q.queue.Len() > 0 {
		val := q.queue[0]
		if !q.tryAccept(time.Now()) {
			break // Rate Limit 도달
		}
		now := time.Now()
		if now.Before(val.ProcessAt) {
			break // 아직 처리 시간이 안 됨
		}
		if ok, wait := fn(*val); !ok {
			val.ProcessAt = now.Add(wait)
			heap.Fix(&q.queue, 0)
		} else {
			heap.Pop(&q.queue)
			processed++
		}
	}
	return processed
}

// ===========================================================================
// 6. ComputeZoneState - Zone 상태 계산
// 실제 소스: pkg/controller/nodelifecycle/node_lifecycle_controller.go line 1264-1282
// ===========================================================================

const (
	DefaultUnhealthyZoneThreshold float32 = 0.55
	DefaultEvictionLimiterQPS     float32 = 0.1
	DefaultSecondaryEvictionQPS   float32 = 0.01
	DefaultLargeClusterThreshold  int32   = 50
)

// ComputeZoneState는 Zone 내 노드 상태를 기반으로 Zone 상태를 결정한다.
func ComputeZoneState(conditions []ConditionStatus, threshold float32) (int, ZoneState) {
	readyNodes := 0
	notReadyNodes := 0
	for _, status := range conditions {
		if status == ConditionTrue {
			readyNodes++
		} else {
			notReadyNodes++
		}
	}
	switch {
	case readyNodes == 0 && notReadyNodes > 0:
		return notReadyNodes, StateFullDisruption
	case notReadyNodes > 2 && float32(notReadyNodes)/float32(notReadyNodes+readyNodes) >= threshold:
		return notReadyNodes, StatePartialDisruption
	default:
		return notReadyNodes, StateNormal
	}
}

// ReducedQPSFunc는 부분 장애 시의 eviction QPS를 결정한다.
// 실제 소스: node_lifecycle_controller.go line 1197-1204
func ReducedQPSFunc(nodeNum int, largeClusterThreshold int32, secondaryQPS float32) float32 {
	if int32(nodeNum) > largeClusterThreshold {
		return secondaryQPS
	}
	return 0 // 소규모 클러스터에서는 eviction 중단
}

// ===========================================================================
// 7. NodeLifecycleController 시뮬레이터
// ===========================================================================

// NodeLifecycleController는 Node Lifecycle Controller의 핵심 로직을 시뮬레이션한다.
type NodeLifecycleController struct {
	nodes          map[string]*Node
	pods           map[string]*Pod
	nodeHealth     map[string]*NodeHealthData
	zoneStates     map[string]ZoneState
	zoneTainters   map[string]*RateLimitedTimedQueue
	evictionLimiterQPS          float32
	secondaryEvictionLimiterQPS float32
	unhealthyZoneThreshold      float32
	largeClusterThreshold       int32
	gracePeriod    time.Duration
	evictedPods    []string // 퇴거된 Pod 목록
}

// NewNodeLifecycleController는 새 컨트롤러를 생성한다.
func NewNodeLifecycleController() *NodeLifecycleController {
	return &NodeLifecycleController{
		nodes:          make(map[string]*Node),
		pods:           make(map[string]*Pod),
		nodeHealth:     make(map[string]*NodeHealthData),
		zoneStates:     make(map[string]ZoneState),
		zoneTainters:   make(map[string]*RateLimitedTimedQueue),
		evictionLimiterQPS:          DefaultEvictionLimiterQPS,
		secondaryEvictionLimiterQPS: DefaultSecondaryEvictionQPS,
		unhealthyZoneThreshold:      DefaultUnhealthyZoneThreshold,
		largeClusterThreshold:       DefaultLargeClusterThreshold,
		gracePeriod:    2 * time.Second, // 시뮬레이션용 짧은 grace period
	}
}

// AddNode는 노드를 클러스터에 추가한다.
func (c *NodeLifecycleController) AddNode(node *Node) {
	c.nodes[node.Name] = node
	c.nodeHealth[node.Name] = &NodeHealthData{
		ProbeTimestamp:           time.Now(),
		ReadyTransitionTimestamp: time.Now(),
		ReadyConditionStatus:     ConditionTrue,
	}
	// Zone용 Tainter 생성
	if _, ok := c.zoneTainters[node.Zone]; !ok {
		c.zoneTainters[node.Zone] = NewRateLimitedTimedQueue(c.evictionLimiterQPS)
		c.zoneStates[node.Zone] = StateInitial
	}
}

// AddPod는 Pod를 특정 노드에 배치한다.
func (c *NodeLifecycleController) AddPod(pod *Pod) {
	c.pods[pod.Name] = pod
}

// SimulateNodeFailure는 노드 장애를 시뮬레이션한다.
func (c *NodeLifecycleController) SimulateNodeFailure(nodeName string, status ConditionStatus) {
	node, ok := c.nodes[nodeName]
	if !ok {
		return
	}
	// 노드의 Ready 조건 변경
	found := false
	for i := range node.Conditions {
		if node.Conditions[i].Type == "Ready" {
			node.Conditions[i].Status = status
			found = true
			break
		}
	}
	if !found {
		node.Conditions = append(node.Conditions, NodeCondition{
			Type:              "Ready",
			Status:            status,
			LastHeartbeatTime: time.Now(),
		})
	}
}

// TryUpdateNodeHealth는 노드 건강 상태를 업데이트한다.
// 실제 소스: node_lifecycle_controller.go line 813-978 의 핵심 로직 시뮬레이션
func (c *NodeLifecycleController) TryUpdateNodeHealth(nodeName string) (ConditionStatus, ConditionStatus) {
	node, ok := c.nodes[nodeName]
	if !ok {
		return ConditionUnknown, ConditionUnknown
	}
	health, ok := c.nodeHealth[nodeName]
	if !ok {
		return ConditionUnknown, ConditionUnknown
	}

	// 현재 Ready 조건 찾기
	var currentStatus ConditionStatus = ConditionUnknown
	for _, cond := range node.Conditions {
		if cond.Type == "Ready" {
			currentStatus = cond.Status
			break
		}
	}

	previousStatus := health.ReadyConditionStatus

	// Grace Period 초과 확인 (heartbeat 없음 시뮬레이션)
	if time.Since(health.ProbeTimestamp) > c.gracePeriod && currentStatus != ConditionTrue {
		currentStatus = ConditionUnknown
	}

	// 상태 전환 기록
	if currentStatus != previousStatus {
		health.ReadyTransitionTimestamp = time.Now()
	}
	health.ReadyConditionStatus = currentStatus

	return previousStatus, currentStatus
}

// ProcessTaintBaseEviction은 노드 상태에 따라 Taint를 적용한다.
// 실제 소스: node_lifecycle_controller.go line 764-798 의 핵심 로직 시뮬레이션
func (c *NodeLifecycleController) ProcessTaintBaseEviction(nodeName string) {
	node, ok := c.nodes[nodeName]
	if !ok {
		return
	}
	health := c.nodeHealth[nodeName]

	switch health.ReadyConditionStatus {
	case ConditionFalse:
		// NotReady Taint 추가 (NoExecute)
		c.addTaintIfNotExists(node, Taint{
			Key:       "node.kubernetes.io/not-ready",
			Effect:    TaintEffectNoExecute,
			TimeAdded: time.Now(),
		})
		c.removeTaint(node, "node.kubernetes.io/unreachable", TaintEffectNoExecute)
		// Rate Limited Queue에 추가
		c.zoneTainters[node.Zone].Add(node.Name)

	case ConditionUnknown:
		// Unreachable Taint 추가 (NoExecute)
		c.addTaintIfNotExists(node, Taint{
			Key:       "node.kubernetes.io/unreachable",
			Effect:    TaintEffectNoExecute,
			TimeAdded: time.Now(),
		})
		c.removeTaint(node, "node.kubernetes.io/not-ready", TaintEffectNoExecute)
		c.zoneTainters[node.Zone].Add(node.Name)

	case ConditionTrue:
		// 정상 복구: 모든 관련 Taint 제거
		c.removeTaint(node, "node.kubernetes.io/not-ready", TaintEffectNoExecute)
		c.removeTaint(node, "node.kubernetes.io/unreachable", TaintEffectNoExecute)
		c.zoneTainters[node.Zone].Remove(node.Name)
	}
}

// DoNoScheduleTaintingPass는 NoSchedule Taint를 적용한다.
// 실제 소스: node_lifecycle_controller.go line 523-576
func (c *NodeLifecycleController) DoNoScheduleTaintingPass(nodeName string) {
	node, ok := c.nodes[nodeName]
	if !ok {
		return
	}
	health := c.nodeHealth[nodeName]

	switch health.ReadyConditionStatus {
	case ConditionFalse:
		c.addTaintIfNotExists(node, Taint{
			Key:    "node.kubernetes.io/not-ready",
			Effect: TaintEffectNoSchedule,
		})
	case ConditionUnknown:
		c.addTaintIfNotExists(node, Taint{
			Key:    "node.kubernetes.io/unreachable",
			Effect: TaintEffectNoSchedule,
		})
	case ConditionTrue:
		c.removeTaint(node, "node.kubernetes.io/not-ready", TaintEffectNoSchedule)
		c.removeTaint(node, "node.kubernetes.io/unreachable", TaintEffectNoSchedule)
	}
}

// HandleDisruption은 Zone 상태를 계산하고 Rate Limiter를 조정한다.
// 실제 소스: node_lifecycle_controller.go line 979-1068
func (c *NodeLifecycleController) HandleDisruption() map[string]ZoneState {
	// Zone별 노드 상태 수집
	zoneConditions := make(map[string][]ConditionStatus)
	for _, node := range c.nodes {
		status := ConditionTrue
		health := c.nodeHealth[node.Name]
		if health != nil {
			status = health.ReadyConditionStatus
		}
		zoneConditions[node.Zone] = append(zoneConditions[node.Zone], status)
	}

	newZoneStates := make(map[string]ZoneState)
	allFullyDisrupted := true

	for zone, conditions := range zoneConditions {
		_, state := ComputeZoneState(conditions, c.unhealthyZoneThreshold)
		newZoneStates[zone] = state
		if state != StateFullDisruption {
			allFullyDisrupted = false
		}
	}

	// 모든 Zone이 FullDisruption인 경우 -> eviction 전체 중단
	if allFullyDisrupted && len(newZoneStates) > 0 {
		for zone := range c.zoneTainters {
			c.zoneTainters[zone].SwapLimiter(0) // eviction 중단
		}
	} else {
		for zone, state := range newZoneStates {
			switch state {
			case StateNormal:
				c.zoneTainters[zone].SwapLimiter(c.evictionLimiterQPS)
			case StatePartialDisruption:
				nodeCount := len(zoneConditions[zone])
				qps := ReducedQPSFunc(nodeCount, c.largeClusterThreshold, c.secondaryEvictionLimiterQPS)
				c.zoneTainters[zone].SwapLimiter(qps)
			case StateFullDisruption:
				c.zoneTainters[zone].SwapLimiter(c.evictionLimiterQPS)
			}
		}
	}

	c.zoneStates = newZoneStates
	return newZoneStates
}

// ProcessPodEvictions는 NoExecute Taint가 있는 노드의 Pod를 처리한다.
// 실제 소스: pkg/controller/tainteviction/taint_eviction.go line 451-490
func (c *NodeLifecycleController) ProcessPodEvictions() []string {
	var evicted []string

	for _, pod := range c.pods {
		node, ok := c.nodes[pod.NodeName]
		if !ok {
			continue
		}

		// NoExecute Taint만 필터링
		var noExecuteTaints []Taint
		for _, t := range node.Taints {
			if t.Effect == TaintEffectNoExecute {
				noExecuteTaints = append(noExecuteTaints, t)
			}
		}

		if len(noExecuteTaints) == 0 {
			continue
		}

		// Toleration 매칭 확인
		allTolerated, usedTolerations := GetMatchingTolerations(noExecuteTaints, pod.Tolerations)

		if !allTolerated {
			// Tolerate하지 못하는 Taint 존재 -> 즉시 eviction
			evicted = append(evicted, fmt.Sprintf("%s (즉시: toleration 없음)", pod.Name))
			continue
		}

		// 최소 허용 시간 계산
		minTime := GetMinTolerationTime(usedTolerations)
		if minTime < 0 {
			// 무한 허용 -> eviction 안 함
			continue
		}
		if minTime == 0 {
			evicted = append(evicted, fmt.Sprintf("%s (즉시: tolerationSeconds=0)", pod.Name))
		} else {
			evicted = append(evicted, fmt.Sprintf("%s (%v 후 예약됨)", pod.Name, minTime))
		}
	}

	c.evictedPods = append(c.evictedPods, evicted...)
	return evicted
}

// addTaintIfNotExists는 Taint가 없으면 추가한다.
func (c *NodeLifecycleController) addTaintIfNotExists(node *Node, taint Taint) {
	for _, t := range node.Taints {
		if t.Key == taint.Key && t.Effect == taint.Effect {
			return
		}
	}
	node.Taints = append(node.Taints, taint)
}

// removeTaint는 특정 Taint를 제거한다.
func (c *NodeLifecycleController) removeTaint(node *Node, key string, effect TaintEffect) {
	var filtered []Taint
	for _, t := range node.Taints {
		if t.Key == key && t.Effect == effect {
			continue
		}
		filtered = append(filtered, t)
	}
	node.Taints = filtered
}

// ===========================================================================
// 8. 메인 시뮬레이션
// ===========================================================================

func printHeader(title string) {
	fmt.Println()
	fmt.Println(strings.Repeat("=", 70))
	fmt.Printf(" %s\n", title)
	fmt.Println(strings.Repeat("=", 70))
}

func printSubHeader(title string) {
	fmt.Printf("\n--- %s ---\n", title)
}

func printNodeStatus(nodes map[string]*Node, health map[string]*NodeHealthData) {
	fmt.Println()
	fmt.Printf("  %-12s %-10s %-14s %s\n", "노드", "Zone", "상태", "Taints")
	fmt.Println("  " + strings.Repeat("-", 65))

	// 정렬된 출력을 위해 이름순으로 순회
	names := make([]string, 0, len(nodes))
	for name := range nodes {
		names = append(names, name)
	}
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[i] > names[j] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}

	for _, name := range names {
		node := nodes[name]
		h := health[name]
		status := "Unknown"
		if h != nil {
			status = string(h.ReadyConditionStatus)
		}
		taintStrs := []string{}
		for _, t := range node.Taints {
			taintStrs = append(taintStrs, fmt.Sprintf("%s:%s",
				strings.TrimPrefix(t.Key, "node.kubernetes.io/"), t.Effect))
		}
		taintDisplay := "(없음)"
		if len(taintStrs) > 0 {
			taintDisplay = strings.Join(taintStrs, ", ")
		}
		fmt.Printf("  %-12s %-10s %-14s %s\n", node.Name, node.Zone, status, taintDisplay)
	}
}

func main() {
	// ================================================================
	// 데모 1: Taint/Toleration 매칭 알고리즘
	// ================================================================
	printHeader("데모 1: Taint/Toleration 매칭 알고리즘")

	taints := []Taint{
		{Key: "node.kubernetes.io/not-ready", Effect: TaintEffectNoExecute},
		{Key: "nvidia.com/gpu", Value: "true", Effect: TaintEffectNoSchedule},
		{Key: "team", Value: "ml-platform", Effect: TaintEffectNoSchedule},
	}

	testCases := []struct {
		name        string
		tolerations []Toleration
	}{
		{
			name:        "Pod-A (Toleration 없음)",
			tolerations: []Toleration{},
		},
		{
			name: "Pod-B (NotReady Exists 허용, 300초)",
			tolerations: []Toleration{
				{Key: "node.kubernetes.io/not-ready", Operator: TolerationOpExists, Effect: TaintEffectNoExecute, TolerationSeconds: int64Ptr(300)},
			},
		},
		{
			name: "Pod-C (모든 Taint 허용)",
			tolerations: []Toleration{
				{Key: "node.kubernetes.io/not-ready", Operator: TolerationOpExists, Effect: TaintEffectNoExecute},
				{Key: "nvidia.com/gpu", Operator: TolerationOpEqual, Value: "true", Effect: TaintEffectNoSchedule},
				{Key: "team", Operator: TolerationOpEqual, Value: "ml-platform", Effect: TaintEffectNoSchedule},
			},
		},
		{
			name: "Pod-D (빈 Key + Exists = 모든 Taint 허용)",
			tolerations: []Toleration{
				{Key: "", Operator: TolerationOpExists},
			},
		},
	}

	fmt.Println("\n  노드 Taints:")
	for _, t := range taints {
		fmt.Printf("    - %s=%s:%s\n", t.Key, t.Value, t.Effect)
	}

	for _, tc := range testCases {
		fmt.Printf("\n  %s:\n", tc.name)
		noScheduleFilter := func(t Taint) bool {
			return t.Effect == TaintEffectNoSchedule || t.Effect == TaintEffectNoExecute
		}
		taint, untolerated := FindUntoleratedTaint(taints, tc.tolerations, noScheduleFilter)
		if untolerated {
			fmt.Printf("    결과: 거부 (견디지 못하는 Taint: %s:%s)\n", taint.Key, taint.Effect)
		} else {
			fmt.Printf("    결과: 허용 (모든 Taint를 Tolerate)\n")
		}

		allTolerated, usedTolerations := GetMatchingTolerations(taints, tc.tolerations)
		if allTolerated {
			minTime := GetMinTolerationTime(usedTolerations)
			if minTime < 0 {
				fmt.Printf("    NoExecute 허용 시간: 무한 (영구 허용)\n")
			} else if minTime == 0 {
				fmt.Printf("    NoExecute 허용 시간: 즉시 eviction\n")
			} else {
				fmt.Printf("    NoExecute 허용 시간: %v\n", minTime)
			}
		}
	}

	// ================================================================
	// 데모 2: Zone 상태 계산
	// ================================================================
	printHeader("데모 2: Zone 상태 계산 (ComputeZoneState)")

	zoneScenarios := []struct {
		name       string
		conditions []ConditionStatus
	}{
		{
			name:       "시나리오 A: 모두 정상 (5/5 Ready)",
			conditions: []ConditionStatus{ConditionTrue, ConditionTrue, ConditionTrue, ConditionTrue, ConditionTrue},
		},
		{
			name:       "시나리오 B: 1개 장애 (4/5 Ready, 20%)",
			conditions: []ConditionStatus{ConditionTrue, ConditionTrue, ConditionTrue, ConditionTrue, ConditionFalse},
		},
		{
			name:       "시나리오 C: 2개 장애 (3/5 Ready, 40%)",
			conditions: []ConditionStatus{ConditionTrue, ConditionTrue, ConditionTrue, ConditionFalse, ConditionFalse},
		},
		{
			name:       "시나리오 D: 3개 장애 (2/5 Ready, 60%)",
			conditions: []ConditionStatus{ConditionTrue, ConditionTrue, ConditionFalse, ConditionFalse, ConditionFalse},
		},
		{
			name:       "시나리오 E: 4개 장애 (1/5 Ready, 80%)",
			conditions: []ConditionStatus{ConditionTrue, ConditionFalse, ConditionFalse, ConditionFalse, ConditionFalse},
		},
		{
			name:       "시나리오 F: 모두 장애 (0/5 Ready)",
			conditions: []ConditionStatus{ConditionFalse, ConditionFalse, ConditionFalse, ConditionFalse, ConditionFalse},
		},
	}

	fmt.Printf("\n  임계값: unhealthyZoneThreshold = %.0f%%\n", DefaultUnhealthyZoneThreshold*100)
	fmt.Printf("  조건: notReady > 2 AND 비율 >= 55%% -> PartialDisruption\n")
	fmt.Printf("  조건: readyNodes == 0 -> FullDisruption\n\n")
	fmt.Printf("  %-45s %-8s %s\n", "시나리오", "불량수", "Zone 상태")
	fmt.Println("  " + strings.Repeat("-", 70))

	for _, s := range zoneScenarios {
		unhealthy, state := ComputeZoneState(s.conditions, DefaultUnhealthyZoneThreshold)
		fmt.Printf("  %-45s %-8d %s\n", s.name, unhealthy, state)
	}

	// ================================================================
	// 데모 3: Rate-Limited Eviction Queue
	// ================================================================
	printHeader("데모 3: Rate-Limited Eviction Queue")

	queue := NewRateLimitedTimedQueue(10.0) // 시뮬레이션용 높은 QPS

	// 노드 5개 추가
	for i := 1; i <= 5; i++ {
		name := fmt.Sprintf("node-%d", i)
		added := queue.Add(name)
		fmt.Printf("  큐에 %s 추가: %v\n", name, added)
	}

	// 중복 추가 시도
	added := queue.Add("node-1")
	fmt.Printf("  큐에 node-1 재추가 시도: %v (중복 방지)\n", added)

	// 처리
	fmt.Println()
	fmt.Println("  큐 처리 시작 (QPS=10.0):")
	processed := queue.Try(func(val TimedValue) (bool, time.Duration) {
		fmt.Printf("    처리됨: %s\n", val.Value)
		return true, 0
	})
	fmt.Printf("  총 처리 항목: %d\n", processed)

	// Rate Limiter 변경 데모
	printSubHeader("Rate Limiter 변경 시뮬레이션")
	queue2 := NewRateLimitedTimedQueue(0.0) // QPS=0 (eviction 중단)
	queue2.Add("node-x")
	queue2.Add("node-y")
	processed = queue2.Try(func(val TimedValue) (bool, time.Duration) {
		fmt.Printf("    처리됨: %s\n", val.Value)
		return true, 0
	})
	fmt.Printf("  QPS=0 상태에서 처리 항목: %d (eviction 중단됨)\n", processed)

	queue2.SwapLimiter(10.0) // QPS 복원
	processed = queue2.Try(func(val TimedValue) (bool, time.Duration) {
		fmt.Printf("    처리됨: %s\n", val.Value)
		return true, 0
	})
	fmt.Printf("  QPS=10 복원 후 처리 항목: %d\n", processed)

	// ================================================================
	// 데모 4: 전체 Node Lifecycle 시뮬레이션
	// ================================================================
	printHeader("데모 4: Node Lifecycle Controller 전체 시뮬레이션")

	ctrl := NewNodeLifecycleController()

	// Zone-A에 노드 3개, Zone-B에 노드 2개 구성
	simNodes := []*Node{
		{Name: "node-a1", Zone: "zone-a", Conditions: []NodeCondition{{Type: "Ready", Status: ConditionTrue}}, CreatedAt: time.Now()},
		{Name: "node-a2", Zone: "zone-a", Conditions: []NodeCondition{{Type: "Ready", Status: ConditionTrue}}, CreatedAt: time.Now()},
		{Name: "node-a3", Zone: "zone-a", Conditions: []NodeCondition{{Type: "Ready", Status: ConditionTrue}}, CreatedAt: time.Now()},
		{Name: "node-b1", Zone: "zone-b", Conditions: []NodeCondition{{Type: "Ready", Status: ConditionTrue}}, CreatedAt: time.Now()},
		{Name: "node-b2", Zone: "zone-b", Conditions: []NodeCondition{{Type: "Ready", Status: ConditionTrue}}, CreatedAt: time.Now()},
	}
	for _, n := range simNodes {
		ctrl.AddNode(n)
	}

	// Pod 배치
	seconds300 := int64(300)
	seconds60 := int64(60)
	simPods := []*Pod{
		{Name: "web-1", Namespace: "default", NodeName: "node-a1", Tolerations: []Toleration{
			{Key: "node.kubernetes.io/not-ready", Operator: TolerationOpExists, Effect: TaintEffectNoExecute, TolerationSeconds: &seconds300},
			{Key: "node.kubernetes.io/unreachable", Operator: TolerationOpExists, Effect: TaintEffectNoExecute, TolerationSeconds: &seconds300},
		}},
		{Name: "web-2", Namespace: "default", NodeName: "node-a1", Tolerations: []Toleration{
			{Key: "node.kubernetes.io/not-ready", Operator: TolerationOpExists, Effect: TaintEffectNoExecute, TolerationSeconds: &seconds60},
			{Key: "node.kubernetes.io/unreachable", Operator: TolerationOpExists, Effect: TaintEffectNoExecute, TolerationSeconds: &seconds60},
		}},
		{Name: "critical-1", Namespace: "kube-system", NodeName: "node-a1", Tolerations: []Toleration{
			{Key: "node.kubernetes.io/not-ready", Operator: TolerationOpExists, Effect: TaintEffectNoExecute},
			{Key: "node.kubernetes.io/unreachable", Operator: TolerationOpExists, Effect: TaintEffectNoExecute},
		}},
		{Name: "batch-1", Namespace: "default", NodeName: "node-a1"}, // Toleration 없음
		{Name: "web-3", Namespace: "default", NodeName: "node-b1", Tolerations: []Toleration{
			{Key: "node.kubernetes.io/not-ready", Operator: TolerationOpExists, Effect: TaintEffectNoExecute, TolerationSeconds: &seconds300},
			{Key: "node.kubernetes.io/unreachable", Operator: TolerationOpExists, Effect: TaintEffectNoExecute, TolerationSeconds: &seconds300},
		}},
	}
	for _, p := range simPods {
		ctrl.AddPod(p)
	}

	// 단계 1: 정상 상태
	printSubHeader("단계 1: 정상 상태")
	zoneStates := ctrl.HandleDisruption()
	printNodeStatus(ctrl.nodes, ctrl.nodeHealth)
	fmt.Println("\n  Zone 상태:")
	for zone, state := range zoneStates {
		fmt.Printf("    %s: %s\n", zone, state)
	}

	// 단계 2: node-a1 장애 발생
	printSubHeader("단계 2: node-a1 장애 (NotReady)")
	ctrl.SimulateNodeFailure("node-a1", ConditionFalse)
	// 건강 상태 갱신
	prev, curr := ctrl.TryUpdateNodeHealth("node-a1")
	fmt.Printf("\n  node-a1 상태 전환: %s -> %s\n", prev, curr)

	// NoSchedule + NoExecute Taint 적용
	ctrl.DoNoScheduleTaintingPass("node-a1")
	ctrl.ProcessTaintBaseEviction("node-a1")

	zoneStates = ctrl.HandleDisruption()
	printNodeStatus(ctrl.nodes, ctrl.nodeHealth)
	fmt.Println("\n  Zone 상태:")
	for zone, state := range zoneStates {
		fmt.Printf("    %s: %s\n", zone, state)
	}

	// Pod eviction 처리
	fmt.Println("\n  Pod Eviction 결과 (node-a1의 Pod):")
	evicted := ctrl.ProcessPodEvictions()
	if len(evicted) == 0 {
		fmt.Println("    (eviction 대상 없음)")
	}
	for _, e := range evicted {
		fmt.Printf("    - %s\n", e)
	}

	// 단계 3: 대규모 장애 (Zone-A의 모든 노드 다운)
	printSubHeader("단계 3: Zone-A 전체 장애 (FullDisruption)")
	ctrl.evictedPods = nil // 리셋
	ctrl.SimulateNodeFailure("node-a2", ConditionUnknown)
	ctrl.SimulateNodeFailure("node-a3", ConditionUnknown)
	ctrl.TryUpdateNodeHealth("node-a2")
	ctrl.TryUpdateNodeHealth("node-a3")
	ctrl.DoNoScheduleTaintingPass("node-a2")
	ctrl.DoNoScheduleTaintingPass("node-a3")
	ctrl.ProcessTaintBaseEviction("node-a2")
	ctrl.ProcessTaintBaseEviction("node-a3")

	zoneStates = ctrl.HandleDisruption()
	printNodeStatus(ctrl.nodes, ctrl.nodeHealth)
	fmt.Println("\n  Zone 상태:")
	for zone, state := range zoneStates {
		fmt.Printf("    %s: %s\n", zone, state)
	}

	// 단계 4: 모든 노드 장애 (Master Disruption)
	printSubHeader("단계 4: 모든 Zone 장애 (Master Disruption Mode)")
	ctrl.SimulateNodeFailure("node-b1", ConditionUnknown)
	ctrl.SimulateNodeFailure("node-b2", ConditionUnknown)
	ctrl.TryUpdateNodeHealth("node-b1")
	ctrl.TryUpdateNodeHealth("node-b2")
	ctrl.ProcessTaintBaseEviction("node-b1")
	ctrl.ProcessTaintBaseEviction("node-b2")

	zoneStates = ctrl.HandleDisruption()
	printNodeStatus(ctrl.nodes, ctrl.nodeHealth)
	fmt.Println("\n  Zone 상태:")
	for zone, state := range zoneStates {
		fmt.Printf("    %s: %s\n", zone, state)
	}
	fmt.Println("\n  [Master Disruption Mode]: 모든 Zone이 FullDisruption")
	fmt.Println("  -> Eviction Rate = 0 (전체 eviction 중단)")
	fmt.Println("  -> 마스터/네트워크 장애로 판단하여 Pod 퇴거를 보류")

	// ================================================================
	// 데모 5: getMinTolerationTime 알고리즘
	// ================================================================
	printHeader("데모 5: getMinTolerationTime 알고리즘")

	tolerationCases := []struct {
		name        string
		tolerations []Toleration
	}{
		{
			name:        "Toleration 없음",
			tolerations: []Toleration{},
		},
		{
			name: "TolerationSeconds=300",
			tolerations: []Toleration{
				{TolerationSeconds: int64Ptr(300)},
			},
		},
		{
			name: "TolerationSeconds=60, 300 (최소 선택)",
			tolerations: []Toleration{
				{TolerationSeconds: int64Ptr(300)},
				{TolerationSeconds: int64Ptr(60)},
			},
		},
		{
			name: "TolerationSeconds 미설정 (무한 허용)",
			tolerations: []Toleration{
				{Key: "any"},
			},
		},
		{
			name: "TolerationSeconds=0 (즉시 eviction)",
			tolerations: []Toleration{
				{TolerationSeconds: int64Ptr(0)},
			},
		},
		{
			name: "혼합: 300초 + 무한 (최소=300초)",
			tolerations: []Toleration{
				{TolerationSeconds: int64Ptr(300)},
				{Key: "other"},
			},
		},
	}

	fmt.Printf("\n  %-45s %s\n", "시나리오", "최소 허용 시간")
	fmt.Println("  " + strings.Repeat("-", 60))
	for _, tc := range tolerationCases {
		result := GetMinTolerationTime(tc.tolerations)
		var display string
		if result < 0 {
			display = "무한 (eviction 없음)"
		} else if result == 0 {
			display = "0 (즉시 eviction)"
		} else {
			display = fmt.Sprintf("%v", result)
		}
		fmt.Printf("  %-45s %s\n", tc.name, display)
	}

	// ================================================================
	// 요약
	// ================================================================
	printHeader("시뮬레이션 완료")
	fmt.Println(`
  이 PoC는 다음 Kubernetes 핵심 알고리즘을 시뮬레이션했습니다:

  1. Taint/Toleration 매칭: Key, Operator, Value, Effect 기반 매칭
  2. Zone 상태 계산: Normal, PartialDisruption, FullDisruption
  3. Rate-Limited Queue: Token Bucket + Priority Heap 기반 속도 제어
  4. Node Health 모니터링: Grace Period 기반 노드 상태 판단
  5. Pod Eviction 스케줄링: TolerationSeconds 기반 지연 eviction

  실제 Kubernetes 소스:
  - pkg/controller/nodelifecycle/node_lifecycle_controller.go
  - pkg/controller/tainteviction/taint_eviction.go
  - pkg/controller/nodelifecycle/scheduler/rate_limited_queue.go
  - pkg/scheduler/framework/plugins/tainttoleration/taint_toleration.go`)
}

func int64Ptr(v int64) *int64 {
	return &v
}
