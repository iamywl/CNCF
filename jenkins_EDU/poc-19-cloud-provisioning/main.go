// Package main은 Jenkins Cloud/Auto-Provisioning 시스템의 핵심 개념을 시뮬레이션한다.
//
// Jenkins의 클라우드 프로비저닝은 다음 핵심 메커니즘으로 동작한다:
// 1. Cloud: 클라우드 프로바이더 추상화 (canProvision, provision)
// 2. NodeProvisioner: 부하 분석 → 프로비저닝 결정
// 3. PlannedNode: Future<Node>로 비동기 프로비저닝
// 4. CloudRetentionStrategy: 유휴 에이전트 자동 종료
// 5. CloudProvisioningListener: 라이프사이클 이벤트 훅
//
// 이 PoC는 Go 표준 라이브러리만으로 이 전체 사이클을 재현한다.
package main

import (
	"fmt"
	"math/rand"
	"sync"
	"time"
)

// =============================================================================
// 1. 데이터 모델
// =============================================================================

// Label은 에이전트 레이블 (예: "linux", "docker", "java")
type Label string

// Node는 Jenkins 노드 (에이전트)
type Node struct {
	Name         string
	Labels       []Label
	NumExecutors int
	Cloud        string // 프로비저닝한 클라우드 이름
	CreatedAt    time.Time
	IdleSince    time.Time
	Busy         bool
}

func (n *Node) HasLabel(label Label) bool {
	for _, l := range n.Labels {
		if l == label {
			return true
		}
	}
	return false
}

// BuildJob은 빌드 큐의 대기 작업
type BuildJob struct {
	Name     string
	Label    Label
	Duration time.Duration
}

// CloudState는 프로비저닝 컨텍스트
// Jenkins 원본: hudson/slaves/Cloud.java의 CloudState
type CloudState struct {
	Label                   Label
	AdditionalPlannedCapacity int
}

// PlannedNode는 비동기 프로비저닝 결과
// Jenkins 원본: hudson/slaves/NodeProvisioner.java의 PlannedNode
type PlannedNode struct {
	DisplayName  string
	NumExecutors int
	Future       chan *Node // Go 채널로 Future<Node> 시뮬레이션
	Error        error
}

// CauseOfBlockage는 프로비저닝 거부 사유
type CauseOfBlockage struct {
	Reason string
}

// =============================================================================
// 2. Cloud 인터페이스 및 구현
// =============================================================================

// Cloud는 클라우드 프로바이더의 추상 인터페이스
// Jenkins 원본: hudson/slaves/Cloud.java
type Cloud interface {
	GetName() string
	CanProvision(state CloudState) bool
	Provision(state CloudState, excessWorkload int) []*PlannedNode
}

// EC2Cloud는 AWS EC2를 시뮬레이션하는 Cloud 구현
type EC2Cloud struct {
	Name            string
	Region          string
	SupportedLabels []Label
	MaxInstances    int
	RunningCount    int
	ProvisionDelay  time.Duration // VM 시작 시간 시뮬레이션
	mu              sync.Mutex
}

func (c *EC2Cloud) GetName() string { return c.Name }

func (c *EC2Cloud) CanProvision(state CloudState) bool {
	for _, l := range c.SupportedLabels {
		if l == state.Label {
			return true
		}
	}
	return false
}

func (c *EC2Cloud) Provision(state CloudState, excessWorkload int) []*PlannedNode {
	c.mu.Lock()
	defer c.mu.Unlock()

	var planned []*PlannedNode
	for i := 0; i < excessWorkload; i++ {
		if c.RunningCount >= c.MaxInstances {
			fmt.Printf("    [%s] 최대 인스턴스 제한 도달 (%d/%d)\n",
				c.Name, c.RunningCount, c.MaxInstances)
			break
		}

		c.RunningCount++
		instanceNum := c.RunningCount
		nodeName := fmt.Sprintf("%s-agent-%d", c.Name, instanceNum)

		// Future<Node>를 채널로 시뮬레이션
		future := make(chan *Node, 1)

		pn := &PlannedNode{
			DisplayName:  nodeName,
			NumExecutors: 1,
			Future:       future,
		}
		planned = append(planned, pn)

		// 비동기 프로비저닝 (VM 시작에 수 초 소요)
		go func(name string, label Label, delay time.Duration) {
			time.Sleep(delay) // VM 시작 시간 시뮬레이션
			node := &Node{
				Name:         name,
				Labels:       []Label{label},
				NumExecutors: 1,
				Cloud:        c.Name,
				CreatedAt:    time.Now(),
				IdleSince:    time.Now(),
			}
			future <- node
		}(nodeName, state.Label, c.ProvisionDelay)
	}

	return planned
}

// KubernetesCloud는 Kubernetes 클라우드를 시뮬레이션
type KubernetesCloud struct {
	Name            string
	Namespace       string
	SupportedLabels []Label
	MaxPods         int
	RunningPods     int
	ProvisionDelay  time.Duration
	mu              sync.Mutex
}

func (c *KubernetesCloud) GetName() string { return c.Name }

func (c *KubernetesCloud) CanProvision(state CloudState) bool {
	for _, l := range c.SupportedLabels {
		if l == state.Label {
			return true
		}
	}
	return false
}

func (c *KubernetesCloud) Provision(state CloudState, excessWorkload int) []*PlannedNode {
	c.mu.Lock()
	defer c.mu.Unlock()

	var planned []*PlannedNode
	for i := 0; i < excessWorkload; i++ {
		if c.RunningPods >= c.MaxPods {
			fmt.Printf("    [%s] 최대 Pod 제한 도달 (%d/%d)\n",
				c.Name, c.RunningPods, c.MaxPods)
			break
		}

		c.RunningPods++
		podNum := c.RunningPods
		podName := fmt.Sprintf("%s-pod-%d", c.Name, podNum)

		future := make(chan *Node, 1)

		pn := &PlannedNode{
			DisplayName:  podName,
			NumExecutors: 1,
			Future:       future,
		}
		planned = append(planned, pn)

		go func(name string, label Label, delay time.Duration) {
			time.Sleep(delay) // Pod 시작 시간 시뮬레이션
			node := &Node{
				Name:         name,
				Labels:       []Label{label},
				NumExecutors: 1,
				Cloud:        c.Name,
				CreatedAt:    time.Now(),
				IdleSince:    time.Now(),
			}
			future <- node
		}(podName, state.Label, c.ProvisionDelay)
	}

	return planned
}

// =============================================================================
// 3. CloudProvisioningListener
// =============================================================================

// CloudProvisioningListener는 프로비저닝 라이프사이클 이벤트를 수신
// Jenkins 원본: hudson/slaves/CloudProvisioningListener.java
type CloudProvisioningListener interface {
	CanProvision(cloud Cloud, state CloudState, numExecutors int) *CauseOfBlockage
	OnStarted(cloud Cloud, label Label, plannedNodes []*PlannedNode)
	OnComplete(plannedNode *PlannedNode, node *Node)
	OnFailure(plannedNode *PlannedNode, err error)
}

// LoggingListener는 모든 이벤트를 로깅하는 리스너
type LoggingListener struct{}

func (l *LoggingListener) CanProvision(cloud Cloud, state CloudState, n int) *CauseOfBlockage {
	fmt.Printf("  [Listener] canProvision: cloud=%s, label=%s, executors=%d → 허용\n",
		cloud.GetName(), state.Label, n)
	return nil // 허용
}

func (l *LoggingListener) OnStarted(cloud Cloud, label Label, plannedNodes []*PlannedNode) {
	names := make([]string, len(plannedNodes))
	for i, pn := range plannedNodes {
		names[i] = pn.DisplayName
	}
	fmt.Printf("  [Listener] onStarted: cloud=%s, nodes=%v\n", cloud.GetName(), names)
}

func (l *LoggingListener) OnComplete(pn *PlannedNode, node *Node) {
	fmt.Printf("  [Listener] onComplete: %s → 노드 준비 완료\n", pn.DisplayName)
}

func (l *LoggingListener) OnFailure(pn *PlannedNode, err error) {
	fmt.Printf("  [Listener] onFailure: %s → %s\n", pn.DisplayName, err)
}

// =============================================================================
// 4. CloudRetentionStrategy
// =============================================================================

// CloudRetentionStrategy는 유휴 에이전트를 자동 종료하는 전략
// Jenkins 원본: hudson/slaves/CloudRetentionStrategy.java
type CloudRetentionStrategy struct {
	IdleMinutes int
}

// Check는 에이전트의 유휴 상태를 확인하고 필요시 종료
func (s *CloudRetentionStrategy) Check(node *Node) bool {
	if !node.Busy {
		idleDuration := time.Since(node.IdleSince)
		if idleDuration > time.Duration(s.IdleMinutes)*time.Minute {
			fmt.Printf("  [RetentionStrategy] %s: 유휴 %v → 종료 요청\n",
				node.Name, idleDuration.Round(time.Second))
			return true // 종료 필요
		}
	}
	return false
}

// =============================================================================
// 5. NodeProvisioner — 부하 분석 엔진
// =============================================================================

// NodeProvisioner는 빌드 큐의 부하를 분석하여 Cloud에 프로비저닝을 요청
// Jenkins 원본: hudson/slaves/NodeProvisioner.java
type NodeProvisioner struct {
	clouds    []Cloud
	nodes     []*Node
	listeners []CloudProvisioningListener
	retention *CloudRetentionStrategy
	mu        sync.Mutex
}

func NewNodeProvisioner(clouds []Cloud, listeners []CloudProvisioningListener) *NodeProvisioner {
	return &NodeProvisioner{
		clouds:    clouds,
		nodes:     nil,
		listeners: listeners,
		retention: &CloudRetentionStrategy{IdleMinutes: 30},
	}
}

// Update는 빌드 큐를 분석하여 필요한 노드를 프로비저닝
func (np *NodeProvisioner) Update(queue []*BuildJob) {
	np.mu.Lock()
	defer np.mu.Unlock()

	// 1단계: 레이블별 수요 계산
	demand := make(map[Label]int)
	for _, job := range queue {
		demand[job.Label]++
	}

	// 2단계: 레이블별 현재 용량 계산
	capacity := make(map[Label]int)
	for _, n := range np.nodes {
		if !n.Busy {
			for _, l := range n.Labels {
				capacity[l] += n.NumExecutors
			}
		}
	}

	// 3단계: 초과 부하 계산
	for label, needed := range demand {
		available := capacity[label]
		excessWorkload := needed - available
		if excessWorkload <= 0 {
			fmt.Printf("  [NodeProvisioner] label=%s: 수요=%d, 가용=%d → 충분\n",
				label, needed, available)
			continue
		}

		fmt.Printf("  [NodeProvisioner] label=%s: 수요=%d, 가용=%d → %d개 추가 필요\n",
			label, needed, available, excessWorkload)

		// 4단계: Cloud에 프로비저닝 요청
		state := CloudState{Label: label}
		for _, cloud := range np.clouds {
			if excessWorkload <= 0 {
				break
			}

			if !cloud.CanProvision(state) {
				continue
			}

			// CloudProvisioningListener 확인
			blocked := false
			for _, listener := range np.listeners {
				if cause := listener.CanProvision(cloud, state, excessWorkload); cause != nil {
					fmt.Printf("  [NodeProvisioner] 프로비저닝 거부: %s\n", cause.Reason)
					blocked = true
					break
				}
			}
			if blocked {
				continue
			}

			// 프로비저닝 실행
			plannedNodes := cloud.Provision(state, excessWorkload)

			// onStarted 이벤트
			for _, listener := range np.listeners {
				listener.OnStarted(cloud, label, plannedNodes)
			}

			// 비동기 결과 수집
			for _, pn := range plannedNodes {
				go np.waitForNode(pn)
			}

			excessWorkload -= len(plannedNodes)
			state.AdditionalPlannedCapacity += len(plannedNodes)
		}
	}
}

// waitForNode는 PlannedNode의 Future가 완료될 때까지 기다림
func (np *NodeProvisioner) waitForNode(pn *PlannedNode) {
	select {
	case node := <-pn.Future:
		if node != nil {
			np.mu.Lock()
			np.nodes = append(np.nodes, node)
			np.mu.Unlock()

			fmt.Printf("  [NodeProvisioner] 노드 추가: %s (executors=%d)\n",
				node.Name, node.NumExecutors)

			// onComplete 이벤트
			for _, listener := range np.listeners {
				listener.OnComplete(pn, node)
			}
		}
	case <-time.After(30 * time.Second):
		err := fmt.Errorf("프로비저닝 타임아웃 (30초)")
		for _, listener := range np.listeners {
			listener.OnFailure(pn, err)
		}
	}
}

// CheckRetention은 유휴 노드를 확인하여 종료
func (np *NodeProvisioner) CheckRetention() {
	np.mu.Lock()
	defer np.mu.Unlock()

	var remaining []*Node
	for _, n := range np.nodes {
		if n.Cloud != "" && np.retention.Check(n) {
			fmt.Printf("  [NodeProvisioner] 노드 종료: %s (cloud=%s)\n",
				n.Name, n.Cloud)
		} else {
			remaining = append(remaining, n)
		}
	}
	np.nodes = remaining
}

// PrintNodes는 현재 노드 목록을 출력
func (np *NodeProvisioner) PrintNodes() {
	np.mu.Lock()
	defer np.mu.Unlock()

	if len(np.nodes) == 0 {
		fmt.Println("  (등록된 노드 없음)")
		return
	}

	fmt.Println("  ┌──────────────────────┬────────────┬──────────┬──────────────┐")
	fmt.Println("  │ 노드 이름             │ 클라우드    │ 레이블   │ 상태          │")
	fmt.Println("  ├──────────────────────┼────────────┼──────────┼──────────────┤")
	for _, n := range np.nodes {
		status := "Idle"
		if n.Busy {
			status = "Busy"
		}
		labels := ""
		for i, l := range n.Labels {
			if i > 0 {
				labels += ","
			}
			labels += string(l)
		}
		cloud := n.Cloud
		if cloud == "" {
			cloud = "(static)"
		}
		fmt.Printf("  │ %-20s │ %-10s │ %-8s │ %-12s │\n",
			n.Name, cloud, labels, status)
	}
	fmt.Println("  └──────────────────────┴────────────┴──────────┴──────────────┘")
}

// =============================================================================
// 메인 데모
// =============================================================================

func main() {
	fmt.Println("=== Jenkins Cloud/Auto-Provisioning 시스템 시뮬레이션 ===")
	fmt.Println()

	// ─────────────────────────────────────────────────
	// 클라우드 설정
	// ─────────────────────────────────────────────────
	ec2Cloud := &EC2Cloud{
		Name:            "aws-ec2",
		Region:          "us-east-1",
		SupportedLabels: []Label{"linux", "java"},
		MaxInstances:    5,
		ProvisionDelay:  500 * time.Millisecond, // 실제로는 수 분
	}

	k8sCloud := &KubernetesCloud{
		Name:            "k8s",
		Namespace:       "jenkins",
		SupportedLabels: []Label{"docker", "linux"},
		MaxPods:         10,
		ProvisionDelay:  200 * time.Millisecond, // Pod은 빠르게 시작
	}

	clouds := []Cloud{ec2Cloud, k8sCloud}
	listeners := []CloudProvisioningListener{&LoggingListener{}}

	provisioner := NewNodeProvisioner(clouds, listeners)

	// ─────────────────────────────────────────────────
	// 시나리오 1: 초기 상태 (노드 없음)
	// ─────────────────────────────────────────────────
	fmt.Println("--- 시나리오 1: 초기 상태 ---")
	provisioner.PrintNodes()
	fmt.Println()

	// ─────────────────────────────────────────────────
	// 시나리오 2: 빌드 큐에 작업 추가 → 자동 프로비저닝
	// ─────────────────────────────────────────────────
	fmt.Println("--- 시나리오 2: 빌드 요청 → 자동 프로비저닝 ---")

	queue := []*BuildJob{
		{Name: "build-app-1", Label: "linux", Duration: 5 * time.Minute},
		{Name: "build-app-2", Label: "linux", Duration: 3 * time.Minute},
		{Name: "build-docker-1", Label: "docker", Duration: 10 * time.Minute},
		{Name: "build-java-1", Label: "java", Duration: 7 * time.Minute},
	}

	fmt.Println("  빌드 큐:")
	for _, job := range queue {
		fmt.Printf("    - %s (label=%s)\n", job.Name, job.Label)
	}
	fmt.Println()

	provisioner.Update(queue)

	// 프로비저닝 완료 대기
	time.Sleep(1 * time.Second)

	fmt.Println("\n  프로비저닝 완료 후 노드 목록:")
	provisioner.PrintNodes()

	// ─────────────────────────────────────────────────
	// 시나리오 3: 추가 부하 → 스케일 업
	// ─────────────────────────────────────────────────
	fmt.Println("\n--- 시나리오 3: 추가 부하 → 스케일 업 ---")

	additionalQueue := []*BuildJob{
		{Name: "build-app-3", Label: "linux"},
		{Name: "build-app-4", Label: "linux"},
		{Name: "build-app-5", Label: "linux"},
		{Name: "build-docker-2", Label: "docker"},
		{Name: "build-docker-3", Label: "docker"},
	}

	fmt.Println("  추가 빌드 큐:")
	for _, job := range additionalQueue {
		fmt.Printf("    - %s (label=%s)\n", job.Name, job.Label)
	}
	fmt.Println()

	provisioner.Update(additionalQueue)
	time.Sleep(1 * time.Second)

	fmt.Println("\n  스케일 업 후 노드 목록:")
	provisioner.PrintNodes()

	// ─────────────────────────────────────────────────
	// 시나리오 4: 유휴 노드 해제 (CloudRetentionStrategy)
	// ─────────────────────────────────────────────────
	fmt.Println("\n--- 시나리오 4: 유휴 노드 해제 (RetentionStrategy) ---")

	// 시뮬레이션: 일부 노드의 유휴 시작 시간을 과거로 설정
	provisioner.mu.Lock()
	if len(provisioner.nodes) >= 3 {
		provisioner.nodes[0].IdleSince = time.Now().Add(-31 * time.Minute)
		provisioner.nodes[1].IdleSince = time.Now().Add(-45 * time.Minute)
		provisioner.nodes[2].Busy = true // 바쁜 노드는 유지
	}
	provisioner.mu.Unlock()

	provisioner.CheckRetention()

	fmt.Println("\n  유휴 노드 해제 후:")
	provisioner.PrintNodes()

	// ─────────────────────────────────────────────────
	// 시나리오 5: Cloud 정보 출력
	// ─────────────────────────────────────────────────
	fmt.Println("\n--- 등록된 Cloud 정보 ---")
	for _, cloud := range clouds {
		switch c := cloud.(type) {
		case *EC2Cloud:
			fmt.Printf("  %s: type=EC2, region=%s, labels=%v, running=%d/%d\n",
				c.Name, c.Region, c.SupportedLabels, c.RunningCount, c.MaxInstances)
		case *KubernetesCloud:
			fmt.Printf("  %s: type=K8s, namespace=%s, labels=%v, pods=%d/%d\n",
				c.Name, c.Namespace, c.SupportedLabels, c.RunningPods, c.MaxPods)
		}
	}

	// 난수 사용 안내
	_ = rand.Intn(1)

	fmt.Println("\n=== 시뮬레이션 완료 ===")
}
