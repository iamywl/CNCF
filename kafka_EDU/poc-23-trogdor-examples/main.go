package main

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// =============================================================================
// Kafka Trogdor 테스트 프레임워크 + Examples 시뮬레이션
//
// 이 PoC는 Trogdor의 Coordinator-Agent 아키텍처와 공식 예제 패턴을 시뮬레이션한다:
//   1. TaskSpec 다형성 (워크로드/장애 TaskSpec → TaskController + TaskWorker)
//   2. TaskManager 상태 머신 (PENDING → RUNNING → STOPPING → DONE)
//   3. WorkerManager + ShutdownManager (참조 카운팅 기반 안전 종료)
//   4. Coordinator → Agent REST 통신 시뮬레이션
//   5. ProduceBench / ConsumeBench 워크로드 실행
//   6. NetworkPartitionFault 장애 주입
//   7. Examples: Producer-Consumer CountDownLatch 패턴
//
// 참조 소스:
//   trogdor/src/main/java/.../coordinator/TaskManager.java
//   trogdor/src/main/java/.../agent/WorkerManager.java
//   trogdor/src/main/java/.../task/TaskSpec.java
//   trogdor/src/main/java/.../workload/ProduceBenchSpec.java
//   trogdor/src/main/java/.../fault/NetworkPartitionFaultSpec.java
//   examples/src/main/java/kafka/examples/KafkaConsumerProducerDemo.java
// =============================================================================

// --- TaskStateType: 태스크 상태 ---
// 실제: trogdor/src/main/java/.../rest/TaskStateType.java

type TaskStateType int

const (
	TaskPending  TaskStateType = iota // 시작 시간 대기
	TaskRunning                       // 실행 중
	TaskStopping                      // 종료 진행 중
	TaskDone                          // 완료
)

func (t TaskStateType) String() string {
	switch t {
	case TaskPending:
		return "PENDING"
	case TaskRunning:
		return "RUNNING"
	case TaskStopping:
		return "STOPPING"
	case TaskDone:
		return "DONE"
	default:
		return "UNKNOWN"
	}
}

// --- WorkerState: 워커 상태 ---
// 실제: WorkerManager.State enum (STARTING, CANCELLING, RUNNING, STOPPING, DONE)

type WorkerStateType int

const (
	WorkerStarting   WorkerStateType = iota
	WorkerCancelling
	WorkerRunning
	WorkerStopping
	WorkerDone
)

func (w WorkerStateType) String() string {
	switch w {
	case WorkerStarting:
		return "STARTING"
	case WorkerCancelling:
		return "CANCELLING"
	case WorkerRunning:
		return "RUNNING"
	case WorkerStopping:
		return "STOPPING"
	case WorkerDone:
		return "DONE"
	default:
		return "UNKNOWN"
	}
}

// --- TaskSpec: 추상 태스크 사양 ---
// 실제: trogdor/src/main/java/.../task/TaskSpec.java
// @JsonTypeInfo(use = JsonTypeInfo.Id.CLASS) → 다형성 직렬화

type TaskSpec interface {
	StartMs() int64
	DurationMs() int64
	EndMs() int64
	NewController(id string) TaskController
	NewTaskWorker(id string) TaskWorker
	TypeName() string
}

// --- TaskController: Coordinator 측 태스크 제어 ---
// 실제: trogdor/src/main/java/.../task/TaskController.java

type TaskController interface {
	TargetNodes() []string
}

// --- TaskWorker: Agent 측 태스크 실행 ---
// 실제: trogdor/src/main/java/.../task/TaskWorker.java

type TaskWorker interface {
	Start(statusTracker StatusTracker) error
	Stop() error
}

// --- StatusTracker: 워커 상태 보고 ---

type StatusTracker struct {
	mu     sync.Mutex
	status map[string]interface{}
}

func NewStatusTracker() *StatusTracker {
	return &StatusTracker{status: make(map[string]interface{})}
}

func (st *StatusTracker) Update(key string, value interface{}) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.status[key] = value
}

func (st *StatusTracker) Get() map[string]interface{} {
	st.mu.Lock()
	defer st.mu.Unlock()
	result := make(map[string]interface{})
	for k, v := range st.status {
		result[k] = v
	}
	return result
}

// --- ProduceBenchSpec: Producer 벤치마크 사양 ---
// 실제: trogdor/src/main/java/.../workload/ProduceBenchSpec.java

type ProduceBenchSpec struct {
	startMs       int64
	durationMs    int64
	ProducerNode  string
	TargetTopic   string
	MaxMessages   int
	MessageBytes  int
	TargetMsgSec  int
}

func (s *ProduceBenchSpec) StartMs() int64    { return s.startMs }
func (s *ProduceBenchSpec) DurationMs() int64 { return s.durationMs }
func (s *ProduceBenchSpec) EndMs() int64      { return s.startMs + s.durationMs }
func (s *ProduceBenchSpec) TypeName() string  { return "ProduceBenchSpec" }

func (s *ProduceBenchSpec) NewController(id string) TaskController {
	return &ProduceBenchController{node: s.ProducerNode}
}

func (s *ProduceBenchSpec) NewTaskWorker(id string) TaskWorker {
	return &ProduceBenchWorker{
		id:           id,
		topic:        s.TargetTopic,
		maxMessages:  s.MaxMessages,
		messageBytes: s.MessageBytes,
		targetRate:   s.TargetMsgSec,
	}
}

type ProduceBenchController struct {
	node string
}

func (c *ProduceBenchController) TargetNodes() []string {
	return []string{c.node}
}

type ProduceBenchWorker struct {
	id           string
	topic        string
	maxMessages  int
	messageBytes int
	targetRate   int
}

func (w *ProduceBenchWorker) Start(tracker StatusTracker) error {
	produced := 0
	startTime := time.Now()
	for produced < w.maxMessages {
		// 처리량 제한 시뮬레이션
		if w.targetRate > 0 {
			elapsed := time.Since(startTime).Seconds()
			expected := int(elapsed * float64(w.targetRate))
			if produced >= expected && elapsed > 0 {
				time.Sleep(time.Millisecond)
				continue
			}
		}
		produced++
		// 주기적으로 상태 보고
		if produced%100 == 0 {
			tracker.Update("produced", produced)
			tracker.Update("throughput_rps", float64(produced)/time.Since(startTime).Seconds())
		}
	}
	tracker.Update("produced", produced)
	tracker.Update("totalBytes", produced*w.messageBytes)
	tracker.Update("throughput_rps", float64(produced)/time.Since(startTime).Seconds())
	return nil
}

func (w *ProduceBenchWorker) Stop() error { return nil }

// --- ConsumeBenchSpec: Consumer 벤치마크 사양 ---

type ConsumeBenchSpec struct {
	startMs      int64
	durationMs   int64
	ConsumerNode string
	TargetTopic  string
	MaxMessages  int
}

func (s *ConsumeBenchSpec) StartMs() int64    { return s.startMs }
func (s *ConsumeBenchSpec) DurationMs() int64 { return s.durationMs }
func (s *ConsumeBenchSpec) EndMs() int64      { return s.startMs + s.durationMs }
func (s *ConsumeBenchSpec) TypeName() string  { return "ConsumeBenchSpec" }

func (s *ConsumeBenchSpec) NewController(id string) TaskController {
	return &ConsumeBenchController{node: s.ConsumerNode}
}

func (s *ConsumeBenchSpec) NewTaskWorker(id string) TaskWorker {
	return &ConsumeBenchWorker{
		id:          id,
		topic:       s.TargetTopic,
		maxMessages: s.MaxMessages,
	}
}

type ConsumeBenchController struct{ node string }

func (c *ConsumeBenchController) TargetNodes() []string { return []string{c.node} }

type ConsumeBenchWorker struct {
	id          string
	topic       string
	maxMessages int
}

func (w *ConsumeBenchWorker) Start(tracker StatusTracker) error {
	consumed := 0
	start := time.Now()
	for consumed < w.maxMessages {
		// 소비 시뮬레이션 (실제: KafkaConsumer.poll())
		batch := rand.Intn(50) + 1
		if consumed+batch > w.maxMessages {
			batch = w.maxMessages - consumed
		}
		consumed += batch
		time.Sleep(time.Microsecond * 100)
		if consumed%100 == 0 {
			tracker.Update("consumed", consumed)
		}
	}
	tracker.Update("consumed", consumed)
	tracker.Update("throughput_rps", float64(consumed)/time.Since(start).Seconds())
	return nil
}

func (w *ConsumeBenchWorker) Stop() error { return nil }

// --- NetworkPartitionFaultSpec: 네트워크 파티션 장애 ---
// 실제: trogdor/src/main/java/.../fault/NetworkPartitionFaultSpec.java

type NetworkPartitionFaultSpec struct {
	startMs    int64
	durationMs int64
	Partitions [][]string // 파티션 그룹 (그룹 간 네트워크 차단)
}

func (s *NetworkPartitionFaultSpec) StartMs() int64    { return s.startMs }
func (s *NetworkPartitionFaultSpec) DurationMs() int64 { return s.durationMs }
func (s *NetworkPartitionFaultSpec) EndMs() int64      { return s.startMs + s.durationMs }
func (s *NetworkPartitionFaultSpec) TypeName() string  { return "NetworkPartitionFaultSpec" }

func (s *NetworkPartitionFaultSpec) NewController(id string) TaskController {
	// 모든 파티션의 노드를 대상으로 함
	nodes := make([]string, 0)
	seen := make(map[string]bool)
	for _, group := range s.Partitions {
		for _, node := range group {
			if !seen[node] {
				nodes = append(nodes, node)
				seen[node] = true
			}
		}
	}
	return &FaultController{nodes: nodes}
}

func (s *NetworkPartitionFaultSpec) NewTaskWorker(id string) TaskWorker {
	return &NetworkPartitionWorker{partitions: s.Partitions}
}

// Validate는 동일 노드 중복 검증 (실제 소스의 validateUnique())
func (s *NetworkPartitionFaultSpec) Validate() error {
	seen := make(map[string]bool)
	for _, group := range s.Partitions {
		for _, node := range group {
			if seen[node] {
				return fmt.Errorf("node %s appears in multiple partition groups", node)
			}
			seen[node] = true
		}
	}
	return nil
}

type FaultController struct{ nodes []string }

func (c *FaultController) TargetNodes() []string { return c.nodes }

type NetworkPartitionWorker struct {
	partitions [][]string
	active     bool
}

func (w *NetworkPartitionWorker) Start(tracker StatusTracker) error {
	w.active = true
	// iptables 규칙 추가 시뮬레이션
	for i, group1 := range w.partitions {
		for j, group2 := range w.partitions {
			if i >= j {
				continue
			}
			for _, n1 := range group1 {
				for _, n2 := range group2 {
					tracker.Update(fmt.Sprintf("block_%s_%s", n1, n2), true)
				}
			}
		}
	}
	tracker.Update("state", "ACTIVE")
	return nil
}

func (w *NetworkPartitionWorker) Stop() error {
	w.active = false
	return nil
}

// --- ShutdownManager: 참조 카운팅 기반 안전 종료 ---
// 실제: WorkerManager.ShutdownManager 내부 클래스

type ShutdownManager struct {
	mu       sync.Mutex
	shutdown bool
	refCount int64
	done     chan struct{}
}

func NewShutdownManager() *ShutdownManager {
	return &ShutdownManager{done: make(chan struct{})}
}

type ShutdownRef struct {
	manager *ShutdownManager
	closed  int32 // atomic
}

func (sm *ShutdownManager) TakeReference() (*ShutdownRef, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if sm.shutdown {
		return nil, fmt.Errorf("WorkerManager is shut down")
	}
	sm.refCount++
	return &ShutdownRef{manager: sm}, nil
}

func (ref *ShutdownRef) Close() {
	if atomic.CompareAndSwapInt32(&ref.closed, 0, 1) {
		ref.manager.mu.Lock()
		ref.manager.refCount--
		shouldNotify := ref.manager.shutdown && ref.manager.refCount == 0
		ref.manager.mu.Unlock()
		if shouldNotify {
			close(ref.manager.done)
		}
	}
}

func (sm *ShutdownManager) Shutdown() bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if sm.shutdown {
		return false
	}
	sm.shutdown = true
	if sm.refCount == 0 {
		close(sm.done)
	}
	return true
}

func (sm *ShutdownManager) WaitForQuiescence(timeout time.Duration) bool {
	select {
	case <-sm.done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// --- ManagedTask: Coordinator의 태스크 관리 ---
// 실제: TaskManager.ManagedTask 내부 클래스

type ManagedTask struct {
	ID         string
	Spec       TaskSpec
	Controller TaskController
	State      TaskStateType
	StartedMs  int64
	DoneMs     int64
	Cancelled  bool
	Error      string
	WorkerIDs  map[string]int64 // nodeName → workerID
}

// --- ManagedWorker: Agent의 워커 관리 ---

type ManagedWorker struct {
	WorkerID   int64
	TaskID     string
	Spec       TaskSpec
	TaskWorker TaskWorker
	State      WorkerStateType
	StartedMs  int64
	DoneMs     int64
	Error      string
	Status     *StatusTracker
	Reference  *ShutdownRef
}

// --- TaskManager: 단일 스레드 태스크 관리 ---
// 실제: trogdor/src/main/java/.../coordinator/TaskManager.java
// single-threaded executor → 락 불필요

type TaskManager struct {
	mu            sync.Mutex
	tasks         map[string]*ManagedTask
	nextWorkerID  int64
	events        chan func()
	nodeManagers  map[string]*NodeManager
	workerStates  map[int64]string // workerID → state
}

func NewTaskManager(nodes []string) *TaskManager {
	tm := &TaskManager{
		tasks:        make(map[string]*ManagedTask),
		events:       make(chan func(), 100),
		nodeManagers: make(map[string]*NodeManager),
		workerStates: make(map[int64]string),
	}
	for _, node := range nodes {
		tm.nodeManagers[node] = &NodeManager{Name: node}
	}
	return tm
}

type NodeManager struct {
	Name string
}

// Run은 단일 스레드 이벤트 루프 (실제: single-threaded executor)
func (tm *TaskManager) Run(done <-chan struct{}) {
	for {
		select {
		case fn := <-tm.events:
			fn()
		case <-done:
			return
		}
	}
}

// CreateTask는 태스크 생성을 이벤트 큐에 제출
func (tm *TaskManager) CreateTask(id string, spec TaskSpec) error {
	resultCh := make(chan error, 1)
	tm.events <- func() {
		// 중복 검사
		if existing, ok := tm.tasks[id]; ok {
			if existing.Spec.TypeName() == spec.TypeName() {
				resultCh <- nil // 동일한 spec이면 성공
				return
			}
			resultCh <- fmt.Errorf("conflict: task %s already exists with different spec", id)
			return
		}

		controller := spec.NewController(id)
		now := time.Now().UnixMilli()

		// 시작 시간 조정 (실제: adjustedSpec)
		task := &ManagedTask{
			ID:         id,
			Spec:       spec,
			Controller: controller,
			State:      TaskPending,
			WorkerIDs:  make(map[string]int64),
		}
		tm.tasks[id] = task

		// 시작 시간이 지났으면 즉시 실행 예약
		if spec.StartMs() <= now {
			tm.scheduleRun(task)
		} else {
			fmt.Printf("    [TaskManager] 태스크 %s: PENDING (시작까지 %dms)\n",
				id, spec.StartMs()-now)
		}
		resultCh <- nil
	}
	return <-resultCh
}

// scheduleRun은 태스크를 RUNNING 상태로 전환
func (tm *TaskManager) scheduleRun(task *ManagedTask) {
	task.State = TaskRunning
	task.StartedMs = time.Now().UnixMilli()

	targetNodes := task.Controller.TargetNodes()
	for _, nodeName := range targetNodes {
		if _, ok := tm.nodeManagers[nodeName]; !ok {
			task.Error = fmt.Sprintf("unknown node: %s", nodeName)
			task.State = TaskDone
			task.DoneMs = time.Now().UnixMilli()
			return
		}
		workerID := tm.nextWorkerID
		tm.nextWorkerID++
		task.WorkerIDs[nodeName] = workerID
		tm.workerStates[workerID] = "RUNNING"
	}
	fmt.Printf("    [TaskManager] 태스크 %s: PENDING → RUNNING (workers: %v)\n",
		task.ID, task.WorkerIDs)
}

// StopTask는 태스크 중지 요청
func (tm *TaskManager) StopTask(id string) error {
	resultCh := make(chan error, 1)
	tm.events <- func() {
		task, ok := tm.tasks[id]
		if !ok {
			resultCh <- fmt.Errorf("task %s not found", id)
			return
		}
		switch task.State {
		case TaskPending:
			task.State = TaskDone
			task.DoneMs = time.Now().UnixMilli()
			task.Cancelled = true
			fmt.Printf("    [TaskManager] 태스크 %s: PENDING → DONE (취소)\n", id)
		case TaskRunning:
			task.State = TaskStopping
			task.Cancelled = true
			fmt.Printf("    [TaskManager] 태스크 %s: RUNNING → STOPPING\n", id)
			// 워커 정지 후 DONE 전환
			task.State = TaskDone
			task.DoneMs = time.Now().UnixMilli()
			fmt.Printf("    [TaskManager] 태스크 %s: STOPPING → DONE\n", id)
		case TaskStopping:
			fmt.Printf("    [TaskManager] 태스크 %s: 이미 STOPPING 상태\n", id)
		case TaskDone:
			fmt.Printf("    [TaskManager] 태스크 %s: 이미 DONE 상태\n", id)
		}
		resultCh <- nil
	}
	return <-resultCh
}

// DestroyTask는 태스크 레코드 삭제
func (tm *TaskManager) DestroyTask(id string) error {
	resultCh := make(chan error, 1)
	tm.events <- func() {
		task, ok := tm.tasks[id]
		if !ok {
			resultCh <- fmt.Errorf("task %s not found", id)
			return
		}
		if task.State != TaskDone {
			resultCh <- fmt.Errorf("task %s is not DONE (current: %s)", id, task.State)
			return
		}
		delete(tm.tasks, id)
		fmt.Printf("    [TaskManager] 태스크 %s: 삭제됨\n", id)
		resultCh <- nil
	}
	return <-resultCh
}

// GetTasks는 현재 태스크 목록 조회
func (tm *TaskManager) GetTasks() map[string]*ManagedTask {
	resultCh := make(chan map[string]*ManagedTask, 1)
	tm.events <- func() {
		result := make(map[string]*ManagedTask)
		for k, v := range tm.tasks {
			result[k] = v
		}
		resultCh <- result
	}
	return <-resultCh
}

// --- WorkerManager: Agent 측 워커 관리 ---
// 실제: trogdor/src/main/java/.../agent/WorkerManager.java

type WorkerManager struct {
	mu              sync.Mutex
	workers         map[int64]*ManagedWorker
	shutdownManager *ShutdownManager
	nodeName        string
}

func NewWorkerManager(nodeName string) *WorkerManager {
	return &WorkerManager{
		workers:         make(map[int64]*ManagedWorker),
		shutdownManager: NewShutdownManager(),
		nodeName:        nodeName,
	}
}

func (wm *WorkerManager) CreateWorker(workerID int64, taskID string, spec TaskSpec) error {
	wm.mu.Lock()
	defer wm.mu.Unlock()

	if _, exists := wm.workers[workerID]; exists {
		return fmt.Errorf("worker %d already exists", workerID)
	}

	ref, err := wm.shutdownManager.TakeReference()
	if err != nil {
		return err
	}

	worker := &ManagedWorker{
		WorkerID:   workerID,
		TaskID:     taskID,
		Spec:       spec,
		TaskWorker: spec.NewTaskWorker(taskID),
		State:      WorkerStarting,
		StartedMs:  time.Now().UnixMilli(),
		Status:     NewStatusTracker(),
		Reference:  ref,
	}
	wm.workers[workerID] = worker

	fmt.Printf("    [WorkerManager:%s] 워커 %d 생성: STARTING\n", wm.nodeName, workerID)

	// 비동기 시작
	go func() {
		// STARTING → RUNNING
		wm.mu.Lock()
		worker.State = WorkerRunning
		wm.mu.Unlock()
		fmt.Printf("    [WorkerManager:%s] 워커 %d: STARTING → RUNNING\n", wm.nodeName, workerID)

		err := worker.TaskWorker.Start(*worker.Status)
		// RUNNING → DONE
		wm.mu.Lock()
		worker.State = WorkerDone
		worker.DoneMs = time.Now().UnixMilli()
		if err != nil {
			worker.Error = err.Error()
		}
		wm.mu.Unlock()
		worker.Reference.Close()
		fmt.Printf("    [WorkerManager:%s] 워커 %d: RUNNING → DONE\n", wm.nodeName, workerID)
	}()

	return nil
}

func (wm *WorkerManager) StopWorker(workerID int64) error {
	wm.mu.Lock()
	defer wm.mu.Unlock()

	worker, ok := wm.workers[workerID]
	if !ok {
		return fmt.Errorf("worker %d not found", workerID)
	}

	if worker.State == WorkerRunning {
		worker.State = WorkerStopping
		fmt.Printf("    [WorkerManager:%s] 워커 %d: RUNNING → STOPPING\n", wm.nodeName, workerID)
		go func() {
			worker.TaskWorker.Stop()
			wm.mu.Lock()
			worker.State = WorkerDone
			worker.DoneMs = time.Now().UnixMilli()
			wm.mu.Unlock()
			worker.Reference.Close()
		}()
	}
	return nil
}

func (wm *WorkerManager) GetWorkerStates() map[int64]WorkerStateType {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	result := make(map[int64]WorkerStateType)
	for id, w := range wm.workers {
		result[id] = w.State
	}
	return result
}

// --- Producer-Consumer Example 패턴 ---
// 실제: examples/src/main/java/kafka/examples/KafkaConsumerProducerDemo.java
// CountDownLatch 패턴: Producer 완료 후 Consumer 종료

type ProducerConsumerDemo struct {
	topic       string
	numMessages int
	latch       sync.WaitGroup
	messages    []string
	mu          sync.Mutex
}

func NewProducerConsumerDemo(topic string, numMessages int) *ProducerConsumerDemo {
	return &ProducerConsumerDemo{
		topic:       topic,
		numMessages: numMessages,
	}
}

func (d *ProducerConsumerDemo) Run() {
	// CountDownLatch(1) → Producer 완료 시 Consumer에 알림
	d.latch.Add(1)
	var producerDone int32

	// Producer 스레드
	go func() {
		for i := 0; i < d.numMessages; i++ {
			msg := fmt.Sprintf("msg-%d", i)
			d.mu.Lock()
			d.messages = append(d.messages, msg)
			d.mu.Unlock()
			time.Sleep(time.Microsecond * 50)
		}
		atomic.StoreInt32(&producerDone, 1)
		d.latch.Done() // countDown()
	}()

	// Consumer 스레드
	consumed := 0
	var consumeWg sync.WaitGroup
	consumeWg.Add(1)
	go func() {
		defer consumeWg.Done()
		offset := 0
		for {
			d.mu.Lock()
			available := len(d.messages)
			d.mu.Unlock()

			if offset < available {
				offset = available
				consumed = offset
			}

			// Producer가 완료되고 모든 메시지를 소비했으면 종료
			if atomic.LoadInt32(&producerDone) == 1 && consumed >= d.numMessages {
				break
			}
			time.Sleep(time.Microsecond * 100)
		}
	}()

	// CountDownLatch.await() → Producer 완료 대기
	d.latch.Wait()
	consumeWg.Wait()

	fmt.Printf("    Producer: %d 메시지 발행 완료\n", d.numMessages)
	fmt.Printf("    Consumer: %d 메시지 소비 완료\n", consumed)
}

// --- 메인 함수 ---

func main() {
	fmt.Println("=== Trogdor 테스트 프레임워크 + Examples 시뮬레이션 ===")
	fmt.Println()

	// 1. TaskSpec 다형성
	fmt.Println("--- 1단계: TaskSpec 다형성 ---")
	specs := []TaskSpec{
		&ProduceBenchSpec{
			startMs: 0, durationMs: 5000,
			ProducerNode: "node0", TargetTopic: "bench-topic",
			MaxMessages: 500, MessageBytes: 1024, TargetMsgSec: 10000,
		},
		&ConsumeBenchSpec{
			startMs: 0, durationMs: 5000,
			ConsumerNode: "node1", TargetTopic: "bench-topic",
			MaxMessages: 500,
		},
		&NetworkPartitionFaultSpec{
			startMs: 0, durationMs: 3000,
			Partitions: [][]string{{"node0", "node1"}, {"node2"}},
		},
	}

	for _, spec := range specs {
		controller := spec.NewController("task-1")
		worker := spec.NewTaskWorker("task-1")
		fmt.Printf("  %s: targets=%v, worker=%T\n",
			spec.TypeName(), controller.TargetNodes(), worker)
	}

	// NetworkPartitionFault 유효성 검증
	validFault := &NetworkPartitionFaultSpec{
		Partitions: [][]string{{"a", "b"}, {"c", "d"}},
	}
	invalidFault := &NetworkPartitionFaultSpec{
		Partitions: [][]string{{"a", "b"}, {"b", "c"}}, // b 중복
	}
	fmt.Printf("  유효한 파티션: %v\n", validFault.Validate())
	fmt.Printf("  중복 노드 파티션: %v\n", invalidFault.Validate())

	// 2. TaskManager 상태 머신
	fmt.Println()
	fmt.Println("--- 2단계: TaskManager 상태 머신 ---")
	done := make(chan struct{})
	tm := NewTaskManager([]string{"node0", "node1", "node2"})
	go tm.Run(done)

	// 태스크 생성
	tm.CreateTask("produce-bench-1", &ProduceBenchSpec{
		startMs: 0, durationMs: 5000,
		ProducerNode: "node0", TargetTopic: "test",
		MaxMessages: 100, MessageBytes: 512, TargetMsgSec: 1000,
	})

	tm.CreateTask("net-partition-1", &NetworkPartitionFaultSpec{
		startMs: 0, durationMs: 3000,
		Partitions: [][]string{{"node0"}, {"node1", "node2"}},
	})

	// 중복 생성 테스트
	err := tm.CreateTask("produce-bench-1", &ProduceBenchSpec{
		startMs: 0, durationMs: 5000,
		ProducerNode: "node0", TargetTopic: "test",
		MaxMessages: 100, MessageBytes: 512, TargetMsgSec: 1000,
	})
	fmt.Printf("    중복 생성 (동일 spec): err=%v\n", err)

	// 태스크 조회
	tasks := tm.GetTasks()
	fmt.Printf("    현재 태스크 수: %d\n", len(tasks))
	for id, task := range tasks {
		fmt.Printf("    - %s: state=%s, workers=%d\n",
			id, task.State, len(task.WorkerIDs))
	}

	// 태스크 중지
	tm.StopTask("produce-bench-1")
	tm.StopTask("net-partition-1")

	// 태스크 삭제
	tm.DestroyTask("produce-bench-1")

	tasks = tm.GetTasks()
	fmt.Printf("    삭제 후 태스크 수: %d\n", len(tasks))
	close(done)

	// 3. ShutdownManager 참조 카운팅
	fmt.Println()
	fmt.Println("--- 3단계: ShutdownManager 참조 카운팅 ---")
	sm := NewShutdownManager()

	ref1, _ := sm.TakeReference()
	ref2, _ := sm.TakeReference()
	fmt.Printf("    참조 2개 획득\n")

	// 종료 시도 (참조가 남아있으므로 대기)
	sm.Shutdown()
	fmt.Printf("    Shutdown 요청됨 (참조 카운트: 2)\n")

	// 새 참조 획득 시도 (실패)
	_, err = sm.TakeReference()
	fmt.Printf("    종료 후 참조 획득: err=%v\n", err)

	// 참조 해제
	ref1.Close()
	fmt.Printf("    ref1 해제 (참조 카운트: 1)\n")

	// 중복 Close 안전성
	ref1.Close()
	fmt.Printf("    ref1 중복 해제 (무시됨)\n")

	ref2.Close()
	fmt.Printf("    ref2 해제 (참조 카운트: 0)\n")

	quiesced := sm.WaitForQuiescence(time.Second)
	fmt.Printf("    Quiescence 도달: %v\n", quiesced)

	// 4. WorkerManager 워커 생명주기
	fmt.Println()
	fmt.Println("--- 4단계: WorkerManager 워커 생명주기 ---")
	wm := NewWorkerManager("agent-0")

	// 워커 생성 (ProduceBench)
	wm.CreateWorker(1, "produce-bench", &ProduceBenchSpec{
		ProducerNode: "agent-0", TargetTopic: "perf",
		MaxMessages: 200, MessageBytes: 256, TargetMsgSec: 5000,
	})

	// 워커 생성 (ConsumeBench)
	wm.CreateWorker(2, "consume-bench", &ConsumeBenchSpec{
		ConsumerNode: "agent-0", TargetTopic: "perf",
		MaxMessages: 200,
	})

	// 워커 실행 대기
	time.Sleep(200 * time.Millisecond)

	// 워커 상태 조회
	states := wm.GetWorkerStates()
	for id, state := range states {
		fmt.Printf("    워커 %d: %s\n", id, state)
	}

	// 워커 완료 대기
	time.Sleep(500 * time.Millisecond)
	states = wm.GetWorkerStates()
	for id, state := range states {
		fmt.Printf("    워커 %d (최종): %s\n", id, state)
	}

	// 5. Coordinator-Agent REST 통신 시뮬레이션
	fmt.Println()
	fmt.Println("--- 5단계: Coordinator → Agent REST 통신 ---")
	agents := map[string]*WorkerManager{
		"node0": NewWorkerManager("node0"),
		"node1": NewWorkerManager("node1"),
	}

	// Coordinator가 Agent에 워커 생성 요청
	fmt.Printf("    Coordinator → Agent node0: CreateWorker(10, produce-bench)\n")
	agents["node0"].CreateWorker(10, "produce-bench", &ProduceBenchSpec{
		ProducerNode: "node0", TargetTopic: "dist-test",
		MaxMessages: 100, MessageBytes: 128, TargetMsgSec: 2000,
	})

	fmt.Printf("    Coordinator → Agent node1: CreateWorker(11, consume-bench)\n")
	agents["node1"].CreateWorker(11, "consume-bench", &ConsumeBenchSpec{
		ConsumerNode: "node1", TargetTopic: "dist-test",
		MaxMessages: 100,
	})

	// Agent 상태 폴링 (실제: Coordinator의 NodeManager가 주기적 폴링)
	time.Sleep(300 * time.Millisecond)
	for agentName, agent := range agents {
		agentStates := agent.GetWorkerStates()
		for wid, ws := range agentStates {
			fmt.Printf("    Agent %s → Coordinator: 워커 %d 상태=%s\n",
				agentName, wid, ws)
		}
	}

	// 6. NetworkPartitionFault 실행
	fmt.Println()
	fmt.Println("--- 6단계: NetworkPartitionFault 장애 주입 ---")
	faultSpec := &NetworkPartitionFaultSpec{
		startMs: 0, durationMs: 2000,
		Partitions: [][]string{{"broker-0", "broker-1"}, {"broker-2"}},
	}

	faultWorker := faultSpec.NewTaskWorker("fault-1")
	faultStatus := NewStatusTracker()
	faultWorker.Start(*faultStatus)

	statusMap := faultStatus.Get()
	fmt.Printf("    장애 상태: state=%v\n", statusMap["state"])
	for k, v := range statusMap {
		if strings.HasPrefix(k, "block_") {
			fmt.Printf("    - %s = %v\n", k, v)
		}
	}

	faultWorker.Stop()
	fmt.Printf("    장애 해제됨\n")

	// 7. Examples: Producer-Consumer CountDownLatch 패턴
	fmt.Println()
	fmt.Println("--- 7단계: Examples - Producer-Consumer 패턴 ---")
	demo := NewProducerConsumerDemo("demo-topic", 100)
	demo.Run()

	// 완료 대기
	time.Sleep(200 * time.Millisecond)

	// 요약
	fmt.Println()
	fmt.Println("=== 요약 ===")
	fmt.Println("  - TaskSpec 다형성: @JsonTypeInfo → ProduceBenchSpec, NetworkPartitionFaultSpec 등")
	fmt.Println("  - TaskManager: 단일 스레드 이벤트 루프 (PENDING→RUNNING→STOPPING→DONE)")
	fmt.Println("  - WorkerManager: Worker 생명주기 (STARTING→RUNNING→STOPPING→DONE)")
	fmt.Println("  - ShutdownManager: 참조 카운팅 기반 안전한 graceful shutdown")
	fmt.Println("  - Coordinator→Agent: REST API 기반 분산 태스크 배포/모니터링")
	fmt.Println("  - NetworkPartitionFault: iptables 기반 네트워크 파티션 장애 주입")
	fmt.Println("  - Examples: CountDownLatch(WaitGroup) 기반 Producer-Consumer 동기화")
}
