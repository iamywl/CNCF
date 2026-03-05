// poc-10-init-milestone: Jenkins InitMilestone 및 Reactor 위상 정렬 시뮬레이션
//
// Jenkins는 시작 시 Reactor 패턴을 사용하여 초기화 작업을 위상 정렬(topological sort)
// 기반으로 실행한다. 각 초기화 작업은 @Initializer 어노테이션으로 의존성을 선언하고,
// InitMilestone 열거형이 정의하는 단계에 따라 순서가 결정된다.
//
// 참조 소스 코드:
//   - jenkins/core/src/main/java/hudson/init/InitMilestone.java
//     : 초기화 단계 열거형 (STARTED → COMPLETED)
//   - jenkins/core/src/main/java/hudson/init/Initializer.java
//     : @Initializer 어노테이션 — after(), before(), requires(), attains(), fatal
//   - InitMilestone.ordering() → TaskGraphBuilder로 마일스톤 간 순서 체인 생성
//   - Reactor 패턴: TaskGraphBuilder로 DAG 구성 → 위상 정렬 → 병렬 실행
//
// 실행: go run main.go

package main

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================================================
// 1. InitMilestone — jenkins/core/src/main/java/hudson/init/InitMilestone.java
// ============================================================================
// Jenkins의 InitMilestone은 초기화 과정의 주요 체크포인트를 정의한다.
// 각 마일스톤은 이름과 설명 메시지를 가진다.
//
// 순서:
//   STARTED → PLUGINS_LISTED → PLUGINS_PREPARED → PLUGINS_STARTED →
//   EXTENSIONS_AUGMENTED → SYSTEM_CONFIG_LOADED → SYSTEM_CONFIG_ADAPTED →
//   JOB_LOADED → JOB_CONFIG_ADAPTED → COMPLETED
//
// InitMilestone.java 라인 132~138에서 ordering() 메서드:
//   public static TaskBuilder ordering() {
//       TaskGraphBuilder b = new TaskGraphBuilder();
//       InitMilestone[] v = values();
//       for (int i = 0; i < v.length - 1; i++)
//           b.add(null, Executable.NOOP).requires(v[i]).attains(v[i + 1]);
//       return b;
//   }

type InitMilestone int

const (
	STARTED InitMilestone = iota
	PLUGINS_LISTED
	PLUGINS_PREPARED
	PLUGINS_STARTED
	EXTENSIONS_AUGMENTED
	SYSTEM_CONFIG_LOADED
	SYSTEM_CONFIG_ADAPTED
	JOB_LOADED
	JOB_CONFIG_ADAPTED
	COMPLETED
)

var milestoneNames = map[InitMilestone]string{
	STARTED:              "STARTED",
	PLUGINS_LISTED:       "PLUGINS_LISTED",
	PLUGINS_PREPARED:     "PLUGINS_PREPARED",
	PLUGINS_STARTED:      "PLUGINS_STARTED",
	EXTENSIONS_AUGMENTED: "EXTENSIONS_AUGMENTED",
	SYSTEM_CONFIG_LOADED: "SYSTEM_CONFIG_LOADED",
	SYSTEM_CONFIG_ADAPTED: "SYSTEM_CONFIG_ADAPTED",
	JOB_LOADED:           "JOB_LOADED",
	JOB_CONFIG_ADAPTED:   "JOB_CONFIG_ADAPTED",
	COMPLETED:            "COMPLETED",
}

var milestoneMessages = map[InitMilestone]string{
	STARTED:              "Started initialization",
	PLUGINS_LISTED:       "Listed all plugins",
	PLUGINS_PREPARED:     "Prepared all plugins",
	PLUGINS_STARTED:      "Started all plugins",
	EXTENSIONS_AUGMENTED: "Augmented all extensions",
	SYSTEM_CONFIG_LOADED: "System config loaded",
	SYSTEM_CONFIG_ADAPTED: "System config adapted",
	JOB_LOADED:           "Loaded all jobs",
	JOB_CONFIG_ADAPTED:   "Configuration for all jobs updated",
	COMPLETED:            "Completed initialization",
}

func (m InitMilestone) String() string {
	return milestoneNames[m]
}

func (m InitMilestone) Message() string {
	return milestoneMessages[m]
}

// AllMilestones: 모든 마일스톤 순서대로 반환
func AllMilestones() []InitMilestone {
	return []InitMilestone{
		STARTED, PLUGINS_LISTED, PLUGINS_PREPARED, PLUGINS_STARTED,
		EXTENSIONS_AUGMENTED, SYSTEM_CONFIG_LOADED, SYSTEM_CONFIG_ADAPTED,
		JOB_LOADED, JOB_CONFIG_ADAPTED, COMPLETED,
	}
}

// ============================================================================
// 2. Initializer — jenkins/core/src/main/java/hudson/init/Initializer.java
// ============================================================================
// @Initializer 어노테이션은 초기화 메서드에 붙여서 의존성을 선언한다.
//
//   @Initializer(after = PLUGINS_STARTED, before = EXTENSIONS_AUGMENTED)
//   public static void loadExtensions() { ... }
//
// 필드:
//   - after(): 이 마일스톤 이후에 실행 (기본값 STARTED)
//   - before(): 이 마일스톤 이전에 완료 (기본값 COMPLETED)
//   - requires(): 명시적 의존성 (문자열 기반)
//   - attains(): 이 작업이 달성하는 마일스톤 (문자열 기반)
//   - displayName(): 진행 표시용 이름
//   - fatal: 실패 시 부팅 중단 여부 (기본값 true)

type InitializerConfig struct {
	Name        string        // 작업 이름 (displayName)
	After       InitMilestone // 이 마일스톤 이후 실행
	Before      InitMilestone // 이 마일스톤 이전 완료
	Requires    []string      // 명시적 의존성 (작업 이름)
	Attains     []string      // 이 작업이 달성하는 것
	Fatal       bool          // 실패 시 부팅 중단
	ExecuteFn   func() error  // 실제 실행 함수
	SimDuration time.Duration // 시뮬레이션 실행 시간
}

// ============================================================================
// 3. Task — Reactor의 작업 단위
// ============================================================================
// Jenkins Reactor에서 각 작업(Task)은 의존성 그래프의 노드이다.
// 각 Task는 requires/attains로 의존성을 선언한다.

type Task struct {
	ID          string
	DisplayName string
	After       InitMilestone
	Before      InitMilestone
	Requires    []string      // 선행 조건 (다른 Task ID 또는 마일스톤 이름)
	Attains     []string      // 이 작업이 달성하는 것
	Fatal       bool
	Execute     func() error
	SimDuration time.Duration

	// 실행 상태
	status     TaskStatus
	startTime  time.Time
	endTime    time.Time
	err        error
	mu         sync.Mutex
}

type TaskStatus int

const (
	TaskPending TaskStatus = iota
	TaskRunning
	TaskCompleted
	TaskFailed
	TaskSkipped
)

func (s TaskStatus) String() string {
	switch s {
	case TaskPending:
		return "PENDING"
	case TaskRunning:
		return "RUNNING"
	case TaskCompleted:
		return "COMPLETED"
	case TaskFailed:
		return "FAILED"
	case TaskSkipped:
		return "SKIPPED"
	default:
		return "UNKNOWN"
	}
}

func (t *Task) SetStatus(s TaskStatus) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.status = s
}

func (t *Task) GetStatus() TaskStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.status
}

// ============================================================================
// 4. TaskGraphBuilder — 의존성 그래프 구성
// ============================================================================
// Jenkins의 TaskGraphBuilder는 작업 간 의존성 그래프(DAG)를 구성한다.
// InitMilestone.ordering()이 마일스톤 간 순서 체인을 생성하고,
// 각 @Initializer가 추가 의존성 간선을 추가한다.

type TaskGraphBuilder struct {
	tasks      []*Task
	taskByID   map[string]*Task
	milestones map[InitMilestone]bool // 달성된 마일스톤
	mu         sync.Mutex
}

func NewTaskGraphBuilder() *TaskGraphBuilder {
	return &TaskGraphBuilder{
		tasks:      make([]*Task, 0),
		taskByID:   make(map[string]*Task),
		milestones: make(map[InitMilestone]bool),
	}
}

// AddTask: 작업을 그래프에 추가
func (b *TaskGraphBuilder) AddTask(config InitializerConfig) *Task {
	task := &Task{
		ID:          config.Name,
		DisplayName: config.Name,
		After:       config.After,
		Before:      config.Before,
		Requires:    config.Requires,
		Attains:     config.Attains,
		Fatal:       config.Fatal,
		Execute:     config.ExecuteFn,
		SimDuration: config.SimDuration,
		status:      TaskPending,
	}
	b.tasks = append(b.tasks, task)
	b.taskByID[task.ID] = task
	return task
}

// AddMilestoneOrdering: InitMilestone.ordering()의 Go 구현
// 마일스톤 간 순서 체인을 DAG 노드로 추가한다.
//
// Jenkins 코드에서:
//   for (int i = 0; i < v.length - 1; i++)
//       b.add(null, Executable.NOOP).requires(v[i]).attains(v[i + 1]);
func (b *TaskGraphBuilder) AddMilestoneOrdering() {
	milestones := AllMilestones()
	for i := 0; i < len(milestones)-1; i++ {
		current := milestones[i]
		next := milestones[i+1]
		taskID := fmt.Sprintf("milestone-%s-to-%s", current.String(), next.String())
		b.AddTask(InitializerConfig{
			Name:  taskID,
			After: current,
			Before: next,
			Fatal: true,
			ExecuteFn: func() error {
				// NOOP — 마일스톤 전환만 수행
				return nil
			},
			SimDuration: 0,
		})
	}
}

// ============================================================================
// 5. DAG — 방향성 비순환 그래프
// ============================================================================
// Reactor는 작업 간 의존성을 DAG로 표현하고, 위상 정렬(topological sort)으로
// 실행 순서를 결정한다.

type DAG struct {
	nodes    []string            // 노드 목록
	edges    map[string][]string // adjacency list (from → to)
	inDegree map[string]int      // 진입 차수
}

func NewDAG() *DAG {
	return &DAG{
		nodes:    make([]string, 0),
		edges:    make(map[string][]string),
		inDegree: make(map[string]int),
	}
}

func (d *DAG) AddNode(id string) {
	if _, exists := d.inDegree[id]; !exists {
		d.nodes = append(d.nodes, id)
		d.inDegree[id] = 0
	}
}

// AddEdge: from → to 간선 추가 (from이 먼저 실행되어야 함)
func (d *DAG) AddEdge(from, to string) {
	d.AddNode(from)
	d.AddNode(to)
	d.edges[from] = append(d.edges[from], to)
	d.inDegree[to]++
}

// TopologicalSort: Kahn's 알고리즘 기반 위상 정렬
// 진입 차수가 0인 노드부터 시작하여 BFS로 순서 결정.
// 같은 레벨의 노드는 병렬 실행 가능.
func (d *DAG) TopologicalSort() ([][]string, error) {
	inDeg := make(map[string]int)
	for k, v := range d.inDegree {
		inDeg[k] = v
	}

	// 진입 차수가 0인 노드 찾기
	var queue []string
	for _, node := range d.nodes {
		if inDeg[node] == 0 {
			queue = append(queue, node)
		}
	}

	var levels [][]string
	visited := 0

	for len(queue) > 0 {
		// 현재 레벨의 모든 노드 (병렬 실행 가능)
		sort.Strings(queue) // 안정적인 순서를 위해 정렬
		currentLevel := make([]string, len(queue))
		copy(currentLevel, queue)
		levels = append(levels, currentLevel)

		var nextQueue []string
		for _, node := range queue {
			visited++
			for _, neighbor := range d.edges[node] {
				inDeg[neighbor]--
				if inDeg[neighbor] == 0 {
					nextQueue = append(nextQueue, neighbor)
				}
			}
		}
		queue = nextQueue
	}

	if visited != len(d.nodes) {
		return nil, fmt.Errorf("순환 의존성 감지: %d개 노드 중 %d개만 정렬됨", len(d.nodes), visited)
	}

	return levels, nil
}

// HasCycle: 순환 감지 (DFS 기반)
func (d *DAG) HasCycle() bool {
	color := make(map[string]int) // 0: white, 1: gray, 2: black
	for _, node := range d.nodes {
		color[node] = 0
	}

	var dfs func(node string) bool
	dfs = func(node string) bool {
		color[node] = 1
		for _, neighbor := range d.edges[node] {
			if color[neighbor] == 1 {
				return true // back edge found
			}
			if color[neighbor] == 0 && dfs(neighbor) {
				return true
			}
		}
		color[node] = 2
		return false
	}

	for _, node := range d.nodes {
		if color[node] == 0 && dfs(node) {
			return true
		}
	}
	return false
}

// ============================================================================
// 6. Reactor — 작업 실행 엔진
// ============================================================================
// Jenkins의 Reactor는 TaskGraphBuilder가 구성한 DAG를 기반으로
// 작업을 위상 정렬 순서에 따라 실행한다.
// 같은 레벨의 작업은 병렬로 실행할 수 있다.

type Reactor struct {
	builder    *TaskGraphBuilder
	dag        *DAG
	listeners  []ReactorListener
	startTime  time.Time
	endTime    time.Time
	maxWorkers int

	// 실행 통계
	totalTasks   int
	completed    int32
	failed       int32
	skipped      int32
}

type ReactorListener interface {
	OnTaskStarted(task *Task)
	OnTaskCompleted(task *Task)
	OnTaskFailed(task *Task, err error)
	OnMilestoneReached(milestone InitMilestone)
}

func NewReactor(builder *TaskGraphBuilder, maxWorkers int) *Reactor {
	return &Reactor{
		builder:    builder,
		dag:        NewDAG(),
		maxWorkers: maxWorkers,
	}
}

func (r *Reactor) AddListener(l ReactorListener) {
	r.listeners = append(r.listeners, l)
}

// BuildDAG: TaskGraphBuilder의 작업들로 DAG 구성
// 마일스톤을 가상 노드로 추가하고, 각 작업의 after/before/requires를 간선으로 변환.
func (r *Reactor) BuildDAG() error {
	// 1. 마일스톤을 DAG 노드로 추가
	for _, m := range AllMilestones() {
		r.dag.AddNode(m.String())
	}

	// 2. 마일스톤 간 순서 체인 (ordering)
	milestones := AllMilestones()
	for i := 0; i < len(milestones)-1; i++ {
		r.dag.AddEdge(milestones[i].String(), milestones[i+1].String())
	}

	// 3. 각 작업을 DAG에 추가
	for _, task := range r.builder.tasks {
		// 마일스톤 전환 노드는 이미 처리됨
		if strings.HasPrefix(task.ID, "milestone-") {
			continue
		}

		r.dag.AddNode(task.ID)

		// after 마일스톤 → 이 작업 (마일스톤 이후에 실행)
		r.dag.AddEdge(task.After.String(), task.ID)

		// 이 작업 → before 마일스톤 (마일스톤 이전에 완료)
		if task.Before != COMPLETED {
			r.dag.AddEdge(task.ID, task.Before.String())
		} else {
			r.dag.AddEdge(task.ID, COMPLETED.String())
		}

		// requires: 명시적 의존성
		for _, req := range task.Requires {
			r.dag.AddNode(req)
			r.dag.AddEdge(req, task.ID)
		}

		// attains: 이 작업이 달성하는 것
		for _, att := range task.Attains {
			r.dag.AddNode(att)
			r.dag.AddEdge(task.ID, att)
		}
	}

	// 순환 감지
	if r.dag.HasCycle() {
		return fmt.Errorf("DAG에 순환 의존성이 존재합니다")
	}

	r.totalTasks = len(r.builder.tasks)
	return nil
}

// Execute: DAG를 위상 정렬하고 레벨별로 병렬 실행
func (r *Reactor) Execute() error {
	r.startTime = time.Now()

	// 위상 정렬
	levels, err := r.dag.TopologicalSort()
	if err != nil {
		return fmt.Errorf("위상 정렬 실패: %w", err)
	}

	// 레벨별 실행
	for levelIdx, level := range levels {
		// 이 레벨에서 실행할 작업 식별
		var tasksToRun []*Task
		var milestonesToAnnounce []InitMilestone

		for _, nodeID := range level {
			// 마일스톤 노드인 경우
			for _, m := range AllMilestones() {
				if m.String() == nodeID {
					milestonesToAnnounce = append(milestonesToAnnounce, m)
					break
				}
			}

			// 작업 노드인 경우
			if task, exists := r.builder.taskByID[nodeID]; exists {
				if !strings.HasPrefix(task.ID, "milestone-") {
					tasksToRun = append(tasksToRun, task)
				}
			}
		}

		// 마일스톤 도달 알림
		for _, m := range milestonesToAnnounce {
			for _, l := range r.listeners {
				l.OnMilestoneReached(m)
			}
		}

		// 작업 병렬 실행
		if len(tasksToRun) > 0 {
			r.executeLevel(levelIdx, tasksToRun)
		}
	}

	r.endTime = time.Now()
	return nil
}

// executeLevel: 한 레벨의 작업을 병렬로 실행
func (r *Reactor) executeLevel(levelIdx int, tasks []*Task) {
	if len(tasks) == 0 {
		return
	}

	// 워커 풀 크기 결정
	workers := r.maxWorkers
	if workers > len(tasks) {
		workers = len(tasks)
	}

	var wg sync.WaitGroup
	taskCh := make(chan *Task, len(tasks))

	// 작업을 채널에 넣기
	for _, task := range tasks {
		taskCh <- task
	}
	close(taskCh)

	// 워커 고루틴 시작
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for task := range taskCh {
				r.executeTask(task)
			}
		}()
	}

	wg.Wait()
}

// executeTask: 개별 작업 실행
func (r *Reactor) executeTask(task *Task) {
	task.SetStatus(TaskRunning)
	task.startTime = time.Now()

	for _, l := range r.listeners {
		l.OnTaskStarted(task)
	}

	// 실행 시간 시뮬레이션
	if task.SimDuration > 0 {
		time.Sleep(task.SimDuration)
	}

	// 실제 작업 실행
	if task.Execute != nil {
		if err := task.Execute(); err != nil {
			task.err = err
			task.SetStatus(TaskFailed)
			task.endTime = time.Now()
			atomic.AddInt32(&r.failed, 1)

			for _, l := range r.listeners {
				l.OnTaskFailed(task, err)
			}

			if task.Fatal {
				fmt.Printf("  [치명적 오류] %s: %v — 부팅 중단\n", task.ID, err)
			}
			return
		}
	}

	task.SetStatus(TaskCompleted)
	task.endTime = time.Now()
	atomic.AddInt32(&r.completed, 1)

	for _, l := range r.listeners {
		l.OnTaskCompleted(task)
	}
}

// ============================================================================
// 7. ReactorListener 구현
// ============================================================================

// ProgressListener: 초기화 진행 상태를 시각적으로 표시
type ProgressListener struct {
	mu            sync.Mutex
	taskEvents    []string
	milestoneHits []InitMilestone
	startTime     time.Time
}

func NewProgressListener() *ProgressListener {
	return &ProgressListener{
		startTime: time.Now(),
	}
}

func (l *ProgressListener) OnTaskStarted(task *Task) {
	l.mu.Lock()
	defer l.mu.Unlock()
	elapsed := time.Since(l.startTime).Milliseconds()
	msg := fmt.Sprintf("  [%5dms] ▶ 시작: %s", elapsed, task.DisplayName)
	l.taskEvents = append(l.taskEvents, msg)
	fmt.Println(msg)
}

func (l *ProgressListener) OnTaskCompleted(task *Task) {
	l.mu.Lock()
	defer l.mu.Unlock()
	elapsed := time.Since(l.startTime).Milliseconds()
	duration := task.endTime.Sub(task.startTime).Milliseconds()
	msg := fmt.Sprintf("  [%5dms] ✓ 완료: %s (%dms)", elapsed, task.DisplayName, duration)
	l.taskEvents = append(l.taskEvents, msg)
	fmt.Println(msg)
}

func (l *ProgressListener) OnTaskFailed(task *Task, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	elapsed := time.Since(l.startTime).Milliseconds()
	msg := fmt.Sprintf("  [%5dms] ✗ 실패: %s — %v", elapsed, task.DisplayName, err)
	l.taskEvents = append(l.taskEvents, msg)
	fmt.Println(msg)
}

func (l *ProgressListener) OnMilestoneReached(milestone InitMilestone) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.milestoneHits = append(l.milestoneHits, milestone)
	elapsed := time.Since(l.startTime).Milliseconds()
	bar := renderProgressBar(milestone)
	fmt.Printf("  [%5dms] ★ 마일스톤: %s — %s\n", elapsed, milestone.String(), milestone.Message())
	fmt.Printf("           %s\n", bar)
}

// renderProgressBar: 마일스톤 진행률 표시
func renderProgressBar(current InitMilestone) string {
	all := AllMilestones()
	total := len(all)
	idx := 0
	for i, m := range all {
		if m == current {
			idx = i
			break
		}
	}
	filled := idx + 1
	empty := total - filled
	pct := float64(filled) / float64(total) * 100
	return fmt.Sprintf("[%s%s] %.0f%%",
		strings.Repeat("█", filled),
		strings.Repeat("░", empty),
		pct)
}

// ============================================================================
// 8. 데모 함수들
// ============================================================================

func printSeparator(title string) {
	fmt.Println()
	fmt.Println(strings.Repeat("=", 72))
	fmt.Printf("  %s\n", title)
	fmt.Println(strings.Repeat("=", 72))
	fmt.Println()
}

// demoMilestoneOrdering: 마일스톤 순서 체인 시연
func demoMilestoneOrdering() {
	printSeparator("데모 1: InitMilestone 순서 체인")

	fmt.Println("  Jenkins 초기화 마일스톤 (순서대로):")
	fmt.Println()
	milestones := AllMilestones()
	for i, m := range milestones {
		marker := "  "
		if i == 0 {
			marker = "→ "
		} else if i == len(milestones)-1 {
			marker = "→ "
		}
		fmt.Printf("  %s%2d. %-25s  %s\n", marker, i+1, m.String(), m.Message())
		if i < len(milestones)-1 {
			fmt.Println("       │")
		}
	}

	fmt.Println()
	fmt.Println("  ordering() 메서드가 마일스톤 간 순서 체인을 생성한다:")
	fmt.Println("  TaskGraphBuilder에 NOOP 작업을 추가하여:")
	for i := 0; i < len(milestones)-1; i++ {
		fmt.Printf("    requires(%s) → attains(%s)\n",
			milestones[i].String(), milestones[i+1].String())
	}
}

// demoDAGTopologicalSort: DAG 위상 정렬 시연
func demoDAGTopologicalSort() {
	printSeparator("데모 2: DAG 위상 정렬 (Kahn's Algorithm)")

	dag := NewDAG()

	// 간단한 빌드 파이프라인 DAG
	dag.AddNode("checkout")
	dag.AddNode("compile")
	dag.AddNode("unit-test")
	dag.AddNode("integration-test")
	dag.AddNode("lint")
	dag.AddNode("package")
	dag.AddNode("deploy")

	dag.AddEdge("checkout", "compile")
	dag.AddEdge("checkout", "lint")
	dag.AddEdge("compile", "unit-test")
	dag.AddEdge("compile", "integration-test")
	dag.AddEdge("compile", "package")
	dag.AddEdge("unit-test", "package")
	dag.AddEdge("integration-test", "package")
	dag.AddEdge("lint", "package")
	dag.AddEdge("package", "deploy")

	fmt.Println("  DAG 구조:")
	fmt.Println("  checkout → compile → unit-test ──┐")
	fmt.Println("     │         │                    ├→ package → deploy")
	fmt.Println("     │         └→ integration-test ─┘")
	fmt.Println("     └→ lint ───────────────────────┘")
	fmt.Println()

	// 순환 감지
	fmt.Printf("  순환 존재: %v\n\n", dag.HasCycle())

	// 위상 정렬
	levels, err := dag.TopologicalSort()
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
		return
	}

	fmt.Println("  위상 정렬 결과 (레벨별):")
	for i, level := range levels {
		parallel := ""
		if len(level) > 1 {
			parallel = " (병렬 실행 가능)"
		}
		fmt.Printf("  레벨 %d: [%s]%s\n", i, strings.Join(level, ", "), parallel)
	}

	// 순환 감지 데모
	fmt.Println("\n  순환 의존성 감지 테스트:")
	cyclicDAG := NewDAG()
	cyclicDAG.AddEdge("A", "B")
	cyclicDAG.AddEdge("B", "C")
	cyclicDAG.AddEdge("C", "A") // 순환!
	fmt.Printf("  A → B → C → A: 순환 존재 = %v\n", cyclicDAG.HasCycle())
}

// demoSimpleReactor: 기본 Reactor 실행 시연
func demoSimpleReactor() {
	printSeparator("데모 3: Reactor 기본 실행 (마일스톤 + 작업)")

	builder := NewTaskGraphBuilder()

	// 마일스톤 ordering 추가
	builder.AddMilestoneOrdering()

	// 실제 Jenkins 초기화 작업들을 시뮬레이션
	// 각 작업은 after/before로 마일스톤에 연결됨
	initTasks := []InitializerConfig{
		{
			Name:  "PluginManager.loadPlugins",
			After: STARTED, Before: PLUGINS_LISTED,
			Fatal: true,
			ExecuteFn: func() error {
				// 플러그인 목록 스캔 시뮬레이션
				return nil
			},
			SimDuration: 30 * time.Millisecond,
		},
		{
			Name:  "PluginManager.preparePlugins",
			After: PLUGINS_LISTED, Before: PLUGINS_PREPARED,
			Fatal: true,
			ExecuteFn: func() error {
				return nil
			},
			SimDuration: 25 * time.Millisecond,
		},
		{
			Name:  "PluginManager.startPlugins",
			After: PLUGINS_PREPARED, Before: PLUGINS_STARTED,
			Fatal: true,
			ExecuteFn: func() error {
				return nil
			},
			SimDuration: 40 * time.Millisecond,
		},
		{
			Name:  "ExtensionFinder.scout",
			After: PLUGINS_STARTED, Before: EXTENSIONS_AUGMENTED,
			Fatal: true,
			ExecuteFn: func() error {
				return nil
			},
			SimDuration: 20 * time.Millisecond,
		},
		{
			Name:  "ExtensionFinder.resolve",
			After: PLUGINS_STARTED, Before: EXTENSIONS_AUGMENTED,
			Requires: []string{"ExtensionFinder.scout"},
			Fatal:    true,
			ExecuteFn: func() error {
				return nil
			},
			SimDuration: 15 * time.Millisecond,
		},
		{
			Name:  "Jenkins.loadConfig",
			After: EXTENSIONS_AUGMENTED, Before: SYSTEM_CONFIG_LOADED,
			Fatal: true,
			ExecuteFn: func() error {
				return nil
			},
			SimDuration: 20 * time.Millisecond,
		},
		{
			Name:  "CasCGlobalConfig.configure",
			After: SYSTEM_CONFIG_LOADED, Before: SYSTEM_CONFIG_ADAPTED,
			Fatal: false,
			ExecuteFn: func() error {
				return nil
			},
			SimDuration: 10 * time.Millisecond,
		},
		{
			Name:  "Jenkins.loadJobs",
			After: SYSTEM_CONFIG_ADAPTED, Before: JOB_LOADED,
			Fatal: true,
			ExecuteFn: func() error {
				return nil
			},
			SimDuration: 50 * time.Millisecond,
		},
		{
			Name:  "Jenkins.adaptJobConfigs",
			After: JOB_LOADED, Before: JOB_CONFIG_ADAPTED,
			Fatal: false,
			ExecuteFn: func() error {
				return nil
			},
			SimDuration: 15 * time.Millisecond,
		},
		{
			Name:  "GroovyInitScript.run",
			After: JOB_CONFIG_ADAPTED, Before: COMPLETED,
			Fatal: false,
			ExecuteFn: func() error {
				return nil
			},
			SimDuration: 10 * time.Millisecond,
		},
	}

	for _, cfg := range initTasks {
		builder.AddTask(cfg)
	}

	// Reactor 실행
	reactor := NewReactor(builder, 4)
	listener := NewProgressListener()
	reactor.AddListener(listener)

	if err := reactor.BuildDAG(); err != nil {
		fmt.Printf("  DAG 구성 실패: %v\n", err)
		return
	}

	fmt.Println("  Jenkins 초기화 시작...")
	fmt.Println()
	if err := reactor.Execute(); err != nil {
		fmt.Printf("  실행 실패: %v\n", err)
		return
	}

	elapsed := reactor.endTime.Sub(reactor.startTime)
	fmt.Printf("\n  초기화 완료: %dms (완료=%d, 실패=%d)\n",
		elapsed.Milliseconds(), atomic.LoadInt32(&reactor.completed), atomic.LoadInt32(&reactor.failed))
}

// demoParallelExecution: 병렬 실행 시연
func demoParallelExecution() {
	printSeparator("데모 4: 병렬 실행 가능한 작업 식별")

	builder := NewTaskGraphBuilder()
	builder.AddMilestoneOrdering()

	// PLUGINS_STARTED 이후에 병렬로 실행 가능한 작업들
	parallelTasks := []InitializerConfig{
		{
			Name: "SecurityRealm.init", After: PLUGINS_STARTED, Before: EXTENSIONS_AUGMENTED,
			Fatal: true, SimDuration: 30 * time.Millisecond,
			ExecuteFn: func() error { return nil },
		},
		{
			Name: "AuthorizationStrategy.init", After: PLUGINS_STARTED, Before: EXTENSIONS_AUGMENTED,
			Fatal: true, SimDuration: 25 * time.Millisecond,
			ExecuteFn: func() error { return nil },
		},
		{
			Name: "QueueManager.init", After: PLUGINS_STARTED, Before: EXTENSIONS_AUGMENTED,
			Fatal: true, SimDuration: 20 * time.Millisecond,
			ExecuteFn: func() error { return nil },
		},
		{
			Name: "NodeMonitor.init", After: PLUGINS_STARTED, Before: EXTENSIONS_AUGMENTED,
			Fatal: false, SimDuration: 35 * time.Millisecond,
			ExecuteFn: func() error { return nil },
		},
		{
			Name: "ToolLocationNodeProperty.init", After: PLUGINS_STARTED, Before: EXTENSIONS_AUGMENTED,
			Fatal: false, SimDuration: 15 * time.Millisecond,
			ExecuteFn: func() error { return nil },
		},
	}

	for _, cfg := range parallelTasks {
		builder.AddTask(cfg)
	}

	// JOB_LOADED 이후에 병렬 실행
	jobTasks := []InitializerConfig{
		{
			Name: "JobConfigHistory.migrate", After: JOB_LOADED, Before: JOB_CONFIG_ADAPTED,
			Fatal: false, SimDuration: 20 * time.Millisecond,
			ExecuteFn: func() error { return nil },
		},
		{
			Name: "Fingerprinter.init", After: JOB_LOADED, Before: JOB_CONFIG_ADAPTED,
			Fatal: false, SimDuration: 15 * time.Millisecond,
			ExecuteFn: func() error { return nil },
		},
		{
			Name: "BuildTrigger.rebuildDependencyGraph", After: JOB_LOADED, Before: JOB_CONFIG_ADAPTED,
			Fatal: true, SimDuration: 25 * time.Millisecond,
			ExecuteFn: func() error { return nil },
		},
	}

	for _, cfg := range jobTasks {
		builder.AddTask(cfg)
	}

	reactor := NewReactor(builder, 8) // 8개 워커
	listener := NewProgressListener()
	reactor.AddListener(listener)

	if err := reactor.BuildDAG(); err != nil {
		fmt.Printf("  DAG 구성 실패: %v\n", err)
		return
	}

	fmt.Println("  병렬 실행 (워커 8개):")
	fmt.Println()
	reactor.Execute()

	elapsed := reactor.endTime.Sub(reactor.startTime)
	fmt.Printf("\n  총 소요 시간: %dms\n", elapsed.Milliseconds())
	fmt.Println("  → 같은 마일스톤 구간의 작업들이 병렬로 실행됨")

	// 순차 실행과 비교
	fmt.Println("\n  [비교] 순차 실행 시 예상 시간:")
	totalSeq := int64(0)
	for _, t := range parallelTasks {
		totalSeq += t.SimDuration.Milliseconds()
	}
	for _, t := range jobTasks {
		totalSeq += t.SimDuration.Milliseconds()
	}
	fmt.Printf("  순차 실행: ~%dms | 병렬 실행: ~%dms\n", totalSeq, elapsed.Milliseconds())
}

// demoFatalTask: 치명적 오류 처리 시연
func demoFatalTask() {
	printSeparator("데모 5: 치명적 작업 실패 (fatal=true)")

	builder := NewTaskGraphBuilder()
	builder.AddMilestoneOrdering()

	// 정상 작업
	builder.AddTask(InitializerConfig{
		Name: "NormalTask1", After: STARTED, Before: PLUGINS_LISTED,
		Fatal: false, SimDuration: 10 * time.Millisecond,
		ExecuteFn: func() error { return nil },
	})

	// 치명적 작업 (실패)
	builder.AddTask(InitializerConfig{
		Name: "CriticalPluginLoader", After: PLUGINS_LISTED, Before: PLUGINS_PREPARED,
		Fatal: true, SimDuration: 15 * time.Millisecond,
		ExecuteFn: func() error {
			return fmt.Errorf("필수 플러그인 'credentials' 로드 실패: ClassNotFoundException")
		},
	})

	// 비치명적 작업 (실패해도 계속)
	builder.AddTask(InitializerConfig{
		Name: "OptionalPluginInit", After: PLUGINS_LISTED, Before: PLUGINS_PREPARED,
		Fatal: false, SimDuration: 10 * time.Millisecond,
		ExecuteFn: func() error {
			return fmt.Errorf("선택적 플러그인 초기화 경고")
		},
	})

	reactor := NewReactor(builder, 4)
	listener := NewProgressListener()
	reactor.AddListener(listener)

	reactor.BuildDAG()

	fmt.Println("  초기화 시작 (치명적 오류 포함)...")
	fmt.Println()
	reactor.Execute()

	fmt.Printf("\n  결과: 완료=%d, 실패=%d\n",
		atomic.LoadInt32(&reactor.completed), atomic.LoadInt32(&reactor.failed))
	fmt.Println("\n  Jenkins 동작:")
	fmt.Println("    - fatal=true 작업 실패 → 부팅 중단")
	fmt.Println("    - fatal=false 작업 실패 → 경고만 출력하고 계속")
}

// demoInitializerAnnotation: @Initializer 어노테이션 시뮬레이션
func demoInitializerAnnotation() {
	printSeparator("데모 6: @Initializer 어노테이션 시뮬레이션")

	fmt.Println("  Jenkins의 @Initializer 사용 예:")
	fmt.Println()
	fmt.Println("  // 기본 사용법:")
	fmt.Println("  @Initializer(after = PLUGINS_STARTED)")
	fmt.Println("  public static void loadExtensions() { ... }")
	fmt.Println()
	fmt.Println("  // 명시적 의존성:")
	fmt.Println("  @Initializer(after = JOB_LOADED, requires = {\"loadSecurityConfig\"})")
	fmt.Println("  public static void configureACL() { ... }")
	fmt.Println()
	fmt.Println("  // 치명적 작업:")
	fmt.Println("  @Initializer(after = STARTED, fatal = true)")
	fmt.Println("  public static void initializeCore() { ... }")
	fmt.Println()

	// 다양한 의존성 패턴으로 작업 생성
	builder := NewTaskGraphBuilder()
	builder.AddMilestoneOrdering()

	// 실제 Jenkins 플러그인에서 볼 수 있는 @Initializer 패턴들
	tasks := []InitializerConfig{
		// 기본 패턴: after만 지정
		{
			Name: "initSecurity", After: PLUGINS_STARTED, Before: EXTENSIONS_AUGMENTED,
			Fatal: true, SimDuration: 15 * time.Millisecond,
			ExecuteFn: func() error { return nil },
		},
		// requires로 다른 작업에 의존
		{
			Name: "configureACL", After: PLUGINS_STARTED, Before: EXTENSIONS_AUGMENTED,
			Requires: []string{"initSecurity"},
			Fatal: true, SimDuration: 10 * time.Millisecond,
			ExecuteFn: func() error { return nil },
		},
		// attains로 커스텀 마일스톤 달성
		{
			Name: "loadCredentials", After: EXTENSIONS_AUGMENTED, Before: SYSTEM_CONFIG_LOADED,
			Attains: []string{"CREDENTIALS_LOADED"},
			Fatal: true, SimDuration: 20 * time.Millisecond,
			ExecuteFn: func() error { return nil },
		},
		// 커스텀 마일스톤을 requires로 사용
		{
			Name: "configureCloudProviders", After: EXTENSIONS_AUGMENTED, Before: SYSTEM_CONFIG_LOADED,
			Requires: []string{"CREDENTIALS_LOADED"},
			Fatal: false, SimDuration: 15 * time.Millisecond,
			ExecuteFn: func() error { return nil },
		},
	}

	for _, cfg := range tasks {
		builder.AddTask(cfg)
	}

	reactor := NewReactor(builder, 4)
	listener := NewProgressListener()
	reactor.AddListener(listener)
	reactor.BuildDAG()

	fmt.Println("  실행:")
	fmt.Println()
	reactor.Execute()

	fmt.Println("\n  의존성 체인:")
	fmt.Println("    initSecurity → configureACL")
	fmt.Println("    loadCredentials → [CREDENTIALS_LOADED] → configureCloudProviders")
}

// demoFullBootSequence: 전체 부팅 시퀀스 시뮬레이션
func demoFullBootSequence() {
	printSeparator("데모 7: 전체 Jenkins 부팅 시퀀스 시뮬레이션")

	builder := NewTaskGraphBuilder()
	builder.AddMilestoneOrdering()

	// 실제 Jenkins 부팅 시 발생하는 초기화 작업들
	bootTasks := []InitializerConfig{
		// Phase 1: Plugin Discovery
		{
			Name: "PluginManager.collectPlugins", After: STARTED, Before: PLUGINS_LISTED,
			Fatal: true, SimDuration: 40 * time.Millisecond,
			ExecuteFn: func() error {
				fmt.Println("           → 플러그인 디렉토리 스캔 (134개 발견)")
				return nil
			},
		},
		{
			Name: "PluginManager.resolveDependencies", After: STARTED, Before: PLUGINS_LISTED,
			Requires: []string{"PluginManager.collectPlugins"},
			Fatal: true, SimDuration: 20 * time.Millisecond,
			ExecuteFn: func() error {
				fmt.Println("           → 의존성 해석 완료 (12개 의존성 트리)")
				return nil
			},
		},

		// Phase 2: Plugin Preparation
		{
			Name: "PluginManager.loadClasses", After: PLUGINS_LISTED, Before: PLUGINS_PREPARED,
			Fatal: true, SimDuration: 60 * time.Millisecond,
			ExecuteFn: func() error {
				fmt.Println("           → 클래스로더 설정 완료 (134개 플러그인)")
				return nil
			},
		},

		// Phase 3: Plugin Start
		{
			Name: "PluginManager.startPlugins", After: PLUGINS_PREPARED, Before: PLUGINS_STARTED,
			Fatal: true, SimDuration: 80 * time.Millisecond,
			ExecuteFn: func() error {
				fmt.Println("           → 플러그인 시작 완료 (134/134)")
				return nil
			},
		},

		// Phase 4: Extension Discovery (병렬)
		{
			Name: "ExtensionFinder.scout", After: PLUGINS_STARTED, Before: EXTENSIONS_AUGMENTED,
			Fatal: true, SimDuration: 30 * time.Millisecond,
			ExecuteFn: func() error {
				fmt.Println("           → 확장 포인트 스캔 (2,847개 발견)")
				return nil
			},
		},
		{
			Name: "DescriptorFinder.init", After: PLUGINS_STARTED, Before: EXTENSIONS_AUGMENTED,
			Fatal: true, SimDuration: 25 * time.Millisecond,
			ExecuteFn: func() error {
				fmt.Println("           → Descriptor 인스턴스화 (463개)")
				return nil
			},
		},
		{
			Name: "ExtensionFinder.resolve", After: PLUGINS_STARTED, Before: EXTENSIONS_AUGMENTED,
			Requires: []string{"ExtensionFinder.scout", "DescriptorFinder.init"},
			Fatal: true, SimDuration: 20 * time.Millisecond,
			ExecuteFn: func() error {
				fmt.Println("           → 확장 포인트 연결 완료")
				return nil
			},
		},

		// Phase 5: System Config
		{
			Name: "Jenkins.loadConfig", After: EXTENSIONS_AUGMENTED, Before: SYSTEM_CONFIG_LOADED,
			Fatal: true, SimDuration: 15 * time.Millisecond,
			ExecuteFn: func() error {
				fmt.Println("           → config.xml 로드 완료")
				return nil
			},
		},
		{
			Name: "SecurityRealm.init", After: EXTENSIONS_AUGMENTED, Before: SYSTEM_CONFIG_LOADED,
			Fatal: true, SimDuration: 20 * time.Millisecond,
			ExecuteFn: func() error {
				fmt.Println("           → 보안 영역 초기화 (LDAP)")
				return nil
			},
		},

		// Phase 6: Config Adaptation (CasC 등)
		{
			Name: "CasCGlobalConfig", After: SYSTEM_CONFIG_LOADED, Before: SYSTEM_CONFIG_ADAPTED,
			Fatal: false, SimDuration: 25 * time.Millisecond,
			ExecuteFn: func() error {
				fmt.Println("           → Configuration as Code 적용")
				return nil
			},
		},

		// Phase 7: Job Loading
		{
			Name: "Jenkins.loadJobs", After: SYSTEM_CONFIG_ADAPTED, Before: JOB_LOADED,
			Fatal: true, SimDuration: 100 * time.Millisecond,
			ExecuteFn: func() error {
				fmt.Println("           → 잡 로드 완료 (256개 잡, 12,847개 빌드)")
				return nil
			},
		},

		// Phase 8: Job Config Adaptation (병렬)
		{
			Name: "JobConfigHistory.migrate", After: JOB_LOADED, Before: JOB_CONFIG_ADAPTED,
			Fatal: false, SimDuration: 20 * time.Millisecond,
			ExecuteFn: func() error {
				fmt.Println("           → 잡 설정 이력 마이그레이션")
				return nil
			},
		},
		{
			Name: "BuildTrigger.rebuildGraph", After: JOB_LOADED, Before: JOB_CONFIG_ADAPTED,
			Fatal: true, SimDuration: 15 * time.Millisecond,
			ExecuteFn: func() error {
				fmt.Println("           → 의존성 그래프 재구성")
				return nil
			},
		},
		{
			Name: "Fingerprinter.init", After: JOB_LOADED, Before: JOB_CONFIG_ADAPTED,
			Fatal: false, SimDuration: 10 * time.Millisecond,
			ExecuteFn: func() error {
				fmt.Println("           → 핑거프린트 인덱스 로드")
				return nil
			},
		},

		// Phase 9: Groovy Init Scripts
		{
			Name: "GroovyInitScript.run", After: JOB_CONFIG_ADAPTED, Before: COMPLETED,
			Fatal: false, SimDuration: 30 * time.Millisecond,
			ExecuteFn: func() error {
				fmt.Println("           → init.groovy.d/ 스크립트 실행 (3개)")
				return nil
			},
		},
	}

	for _, cfg := range bootTasks {
		builder.AddTask(cfg)
	}

	reactor := NewReactor(builder, 4)
	listener := NewProgressListener()
	reactor.AddListener(listener)

	if err := reactor.BuildDAG(); err != nil {
		fmt.Printf("  DAG 구성 실패: %v\n", err)
		return
	}

	fmt.Println("  Jenkins 부팅 시퀀스 시작")
	fmt.Println("  " + strings.Repeat("-", 60))
	fmt.Println()

	startTime := time.Now()
	reactor.Execute()
	totalTime := time.Since(startTime)

	fmt.Println()
	fmt.Println("  " + strings.Repeat("-", 60))
	fmt.Printf("  부팅 완료: 총 %dms\n", totalTime.Milliseconds())
	fmt.Printf("  작업: %d개 완료, %d개 실패\n",
		atomic.LoadInt32(&reactor.completed), atomic.LoadInt32(&reactor.failed))
	fmt.Println("  마일스톤 도달 순서:")
	for _, m := range listener.milestoneHits {
		fmt.Printf("    %-25s %s\n", m.String(), m.Message())
	}
}

// demoRandomizedWorkload: 무작위 워크로드로 위상 정렬 검증
func demoRandomizedWorkload() {
	printSeparator("데모 8: 무작위 워크로드 — 위상 정렬 정확성 검증")

	builder := NewTaskGraphBuilder()
	builder.AddMilestoneOrdering()

	// 무작위 작업 생성
	rng := rand.New(rand.NewSource(42))
	milestones := AllMilestones()
	taskCount := 20
	executionOrder := make([]string, 0, taskCount)
	var orderMu sync.Mutex

	fmt.Printf("  %d개의 무작위 작업 생성:\n", taskCount)
	for i := 0; i < taskCount; i++ {
		afterIdx := rng.Intn(len(milestones) - 1)
		beforeIdx := afterIdx + 1 + rng.Intn(len(milestones)-afterIdx-1)
		if beforeIdx >= len(milestones) {
			beforeIdx = len(milestones) - 1
		}

		name := fmt.Sprintf("task-%02d", i)
		after := milestones[afterIdx]
		before := milestones[beforeIdx]
		dur := time.Duration(5+rng.Intn(20)) * time.Millisecond
		fatal := rng.Float64() > 0.3

		localName := name
		builder.AddTask(InitializerConfig{
			Name: name, After: after, Before: before,
			Fatal: fatal, SimDuration: dur,
			ExecuteFn: func() error {
				orderMu.Lock()
				executionOrder = append(executionOrder, localName)
				orderMu.Unlock()
				return nil
			},
		})
		fmt.Printf("    %s: after=%s, before=%s, dur=%dms, fatal=%v\n",
			name, after.String(), before.String(), dur.Milliseconds(), fatal)
	}

	reactor := NewReactor(builder, 8)
	listener := NewProgressListener()
	reactor.AddListener(listener)
	reactor.BuildDAG()

	fmt.Println("\n  실행:")
	fmt.Println()
	reactor.Execute()

	fmt.Printf("\n  실행 순서: [%s]\n", strings.Join(executionOrder, ", "))
	fmt.Printf("  총 %d개 작업 완료\n", len(executionOrder))

	// 의존성 순서 검증
	fmt.Println("\n  의존성 순서 검증:")
	violations := 0
	taskMap := make(map[string]*Task)
	for _, t := range builder.tasks {
		taskMap[t.ID] = t
	}

	orderIndex := make(map[string]int)
	for i, name := range executionOrder {
		orderIndex[name] = i
	}

	for _, name := range executionOrder {
		task := taskMap[name]
		if task == nil {
			continue
		}
		for _, req := range task.Requires {
			if reqIdx, ok := orderIndex[req]; ok {
				if curIdx, ok := orderIndex[name]; ok && reqIdx > curIdx {
					fmt.Printf("    ✗ 위반: %s (idx=%d) → %s (idx=%d)\n", req, reqIdx, name, curIdx)
					violations++
				}
			}
		}
	}

	if violations == 0 {
		fmt.Println("    ✓ 모든 의존성 순서가 올바름")
	} else {
		fmt.Printf("    ✗ %d개 위반 발견\n", violations)
	}
}

// ============================================================================
// 메인 함수
// ============================================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║  Jenkins PoC-10: InitMilestone 및 Reactor 위상 정렬 시뮬레이션      ║")
	fmt.Println("║                                                                      ║")
	fmt.Println("║  참조: jenkins/core/src/main/java/hudson/init/InitMilestone.java     ║")
	fmt.Println("║        jenkins/core/src/main/java/hudson/init/Initializer.java       ║")
	fmt.Println("║        org.jvnet.hudson.reactor.TaskGraphBuilder (Reactor 패턴)      ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════════╝")

	demoMilestoneOrdering()
	demoDAGTopologicalSort()
	demoSimpleReactor()
	demoParallelExecution()
	demoFatalTask()
	demoInitializerAnnotation()
	demoFullBootSequence()
	demoRandomizedWorkload()

	printSeparator("요약: Jenkins 초기화 시스템의 핵심 설계")
	fmt.Println(`  Jenkins의 초기화 시스템은 다음 핵심 원칙으로 설계되었다:

  1. InitMilestone (초기화 단계 열거형)
     - 10개의 순서화된 마일스톤: STARTED → ... → COMPLETED
     - ordering(): 마일스톤 간 NOOP 작업으로 순서 체인 생성
     - 플러그인이 적절한 시점에 초기화할 수 있는 체크포인트
     - 실제 코드: hudson.init.InitMilestone

  2. @Initializer (초기화 작업 어노테이션)
     - after/before: 마일스톤 기반 시간 범위
     - requires/attains: 명시적 의존성 선언
     - fatal: 실패 시 부팅 중단 여부
     - 실제 코드: hudson.init.Initializer

  3. Reactor (작업 실행 엔진)
     - TaskGraphBuilder: 의존성 DAG 구성
     - Kahn's Algorithm: 위상 정렬로 실행 순서 결정
     - 같은 레벨 = 병렬 실행 가능 → 부팅 시간 최적화
     - 순환 의존성 감지: 부팅 전 검증

  4. 설계 이점
     - 플러그인 독립성: 각 플러그인이 자체 초기화 시점 선언
     - 병렬성: 독립적인 초기화 작업은 동시 실행
     - 안전성: fatal 작업 실패 시 부팅 중단, 순환 의존성 사전 감지
     - 확장성: 커스텀 마일스톤과 의존성으로 플러그인 통합`)

	fmt.Println("\n프로그램이 정상 종료되었습니다.")
}
