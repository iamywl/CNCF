// poc-04-executor: Jenkins Node-Computer-Executor 3계층 시스템 시뮬레이션
//
// Jenkins의 빌드 실행 인프라의 핵심인 Node → Computer → Executor 3계층 구조를
// Go 표준 라이브러리만으로 재현한다.
//
// 실제 Jenkins 소스 참조:
//   - hudson.model.Node (Node.java)
//     - 빌드 에이전트의 설정(configuration) 계층
//     - 불변에 가까움: 설정 변경 시 객체가 재생성됨
//     - getNodeName(), getNumExecutors(), getLabelString(), getMode()
//     - toComputer(): 대응하는 Computer 객체 반환
//     - canTake(BuildableItem): 라벨 매칭 + Mode(NORMAL/EXCLUSIVE) 검사
//     - Jenkins(마스터) 자체도 Node의 구현체
//
//   - hudson.model.Computer (Computer.java)
//     - Node의 런타임 상태(running state) 계층
//     - CopyOnWriteArrayList<Executor> executors 필드로 Executor 풀 관리
//     - setNumExecutors(n): Node 설정 변경 시 호출
//       → 초과 Executor에 interrupt() 전송 (idle인 것만)
//       → 부족하면 addNewExecutorIfNecessary() 호출
//     - removeExecutor(e): Executor 완료 시 호출 → addNewExecutorIfNecessary()
//     - Node가 삭제되어도 실행 중인 빌드가 있으면 Computer는 유지됨
//
//   - hudson.model.Executor (Executor.java)
//     - Thread를 상속한 실제 빌드 실행 단위
//     - owner: Computer 참조, number: 인덱스, workUnit: 현재 작업
//     - run(): WorkUnit 수신 → Executable 생성 → 실행 → finish1 → finish2
//     - start(WorkUnit): Queue가 작업 할당 시 호출하여 스레드 시작
//     - isIdle()/isBusy(): workUnit과 executable 기반 상태 판별
//     - interrupt(Result): 빌드 중단, 인터럽트 원인 기록
//     - finish2(): owner.removeExecutor(this) → Queue.scheduleMaintenance()
//
//   - hudson.slaves.RetentionStrategy (RetentionStrategy.java)
//     - check(Computer): 주기적 호출로 에이전트 생명주기 관리
//     - RetentionStrategy.Demand: 유휴 시간 초과 시 자동 오프라인
//       → idleDelay분 유휴 → disconnect(IdleOfflineCause)
//       → inDemandDelay분 수요 → connect(false)
//     - RetentionStrategy.Always: 항상 온라인 유지
//
//   - hudson.model.Label (Label.java)
//     - contains(Node): 라벨-노드 매칭
//     - parse(String): 라벨 문자열 파싱
//
// 핵심 설계 원칙 - 설정(Node)과 런타임(Computer) 분리:
//   Node는 "사용자 설정"이고 Computer는 "실행 상태"이다.
//   Node 설정을 변경해도 진행 중인 빌드(Executor)는 즉시 영향받지 않는다.
//   Computer.setNode(node) → setNumExecutors(node.getNumExecutors())로
//   점진적으로 Executor 풀을 조정한다.
//
// 실행: go run main.go

package main

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// 1. Node: 설정(Configuration) 계층
// =============================================================================
// Jenkins의 hudson.model.Node에 대응한다.
// Node는 빌드 에이전트의 "설정 정보"만 보유하며, 런타임 상태는 Computer가 관리한다.
// Node 객체는 설정 변경 시 재생성될 수 있으며, 불변에 가깝게 취급된다.
//
// 실제 Jenkins에서:
//   - Node는 abstract class이며, Slave와 Jenkins(마스터)가 구현한다
//   - getNodeName()은 ""이면 마스터 노드를 의미한다
//   - getNumExecutors()는 실제 Executor 수와 다를 수 있다 (조정 중)
//   - Mode.NORMAL: 라벨 없는 작업도 받음, Mode.EXCLUSIVE: 라벨 매칭 작업만 받음

// NodeMode 는 Jenkins의 Node.Mode 열거형에 대응한다.
// NORMAL 모드는 라벨이 지정되지 않은 작업도 수행할 수 있고,
// EXCLUSIVE 모드는 명시적으로 이 노드를 지정한 작업만 수행한다.
type NodeMode int

const (
	// ModeNormal 은 라벨 미지정 작업도 이 노드에서 실행 가능
	// 실제: Node.Mode.NORMAL
	ModeNormal NodeMode = iota
	// ModeExclusive 는 이 노드를 명시적으로 지정한 작업만 실행
	// 실제: Node.Mode.EXCLUSIVE
	ModeExclusive
)

func (m NodeMode) String() string {
	if m == ModeExclusive {
		return "EXCLUSIVE"
	}
	return "NORMAL"
}

// Node 는 빌드 에이전트의 설정 정보를 나타낸다.
// 실제 Jenkins에서 Node는 abstract class이며 다음 필드/메서드가 핵심이다:
//   - getNodeName(): 노드 이름 ("" = 마스터)
//   - getNumExecutors(): 동시 빌드 수
//   - getLabelString(): 수동 할당된 라벨 문자열
//   - getMode(): NORMAL 또는 EXCLUSIVE
//   - toComputer(): 대응하는 Computer 반환
//   - canTake(BuildableItem): 이 노드가 작업을 받을 수 있는지 판단
type Node struct {
	Name            string   // 노드 이름. 실제: getNodeName()
	NumExecutors    int      // Executor 수. 실제: getNumExecutors()
	Labels          []string // 수동 할당 라벨. 실제: getLabelString() → parse()
	NodeDescription string   // 설명. 실제: getNodeDescription()
	Mode            NodeMode // 실행 모드. 실제: getMode()
}

// HasLabel 은 노드가 특정 라벨을 보유하는지 검사한다.
// 실제 Jenkins에서는 Label.contains(Node)가 이 역할을 수행한다.
// Label은 Node의 getAssignedLabels()를 통해 LabelAtom 집합을 얻고,
// 그 집합에 자신이 포함되어 있는지 확인한다.
func (n *Node) HasLabel(label string) bool {
	for _, l := range n.Labels {
		if l == label {
			return true
		}
	}
	// selfLabel: 노드 이름 자체도 라벨로 사용 가능
	// 실제: getSelfLabel() → LabelAtom.get(getNodeName())
	return n.Name == label
}

// CanTake 는 이 노드가 주어진 작업을 수행할 수 있는지 판단한다.
// 실제 Jenkins의 Node.canTake(Queue.BuildableItem)을 단순화한 것이다.
//
// 실제 Jenkins의 canTake() 로직 (Node.java:427-470):
//   1. 작업에 라벨이 지정되어 있고, 이 노드가 해당 라벨을 갖지 않으면 → 거부
//   2. 작업에 라벨이 없고 노드가 EXCLUSIVE 모드이면 → 거부 (FlyweightTask 예외)
//   3. 빌드 권한 검사
//   4. NodeProperty들의 canTake() 검사
//   5. isAcceptingTasks() 검사
func (n *Node) CanTake(requiredLabel string) (bool, string) {
	if requiredLabel != "" {
		// 라벨이 지정된 경우: 노드가 해당 라벨을 보유해야 함
		if !n.HasLabel(requiredLabel) {
			return false, fmt.Sprintf("라벨 '%s' 없음 (보유: [%s])",
				requiredLabel, strings.Join(n.Labels, ", "))
		}
		return true, ""
	}
	// 라벨 미지정 + EXCLUSIVE 모드: 거부
	// 실제: Node.Mode.EXCLUSIVE인 노드는 라벨 없는 작업을 받지 않음
	if n.Mode == ModeExclusive {
		return false, fmt.Sprintf("노드 '%s'는 EXCLUSIVE 모드 (라벨 지정 작업만 수행)", n.Name)
	}
	return true, ""
}

// =============================================================================
// 2. Executor: 실행(Execution) 계층
// =============================================================================
// Jenkins의 hudson.model.Executor에 대응한다.
// Executor는 Thread를 상속하며, 실제 빌드를 수행하는 단위이다.
//
// 핵심 구조 (Executor.java:93-156):
//   - owner: @NonNull Computer — 소속 Computer
//   - number: int — Executor 번호 (0부터 시작)
//   - workUnit: WorkUnit — Queue에서 받은 작업 (null이면 idle)
//   - executable: Queue.Executable — 실행 중인 빌드 (null이면 idle)
//   - started: boolean — start(WorkUnit) 호출 여부
//   - interruptStatus: Result — 인터럽트 시 설정할 결과
//
// 생명주기 (Executor.java:339-491):
//   1. Queue가 start(WorkUnit)를 호출하면 스레드 시작
//   2. run(): workUnit에서 SubTask를 꺼냄 → Executable 생성 → 실행
//   3. 완료 시 finish1(problems) → finish2()
//   4. finish2(): owner.removeExecutor(this) 호출
//   5. Computer가 addNewExecutorIfNecessary()로 새 Executor 생성

// ExecutorState 는 Executor의 상태를 나타낸다.
type ExecutorState int

const (
	// StateIdle 은 작업 대기 중. 실제: isIdle() = (workUnit == null && executable == null)
	StateIdle ExecutorState = iota
	// StateBusy 는 작업 실행 중. 실제: isBusy() = (workUnit != null || executable != null)
	StateBusy
	// StateInterrupted 는 인터럽트됨. 실제: interrupt(Result) 호출됨
	StateInterrupted
	// StateDead 는 종료됨 (풀에서 제거 대기)
	StateDead
)

func (s ExecutorState) String() string {
	switch s {
	case StateIdle:
		return "IDLE"
	case StateBusy:
		return "BUSY"
	case StateInterrupted:
		return "INTERRUPTED"
	case StateDead:
		return "DEAD"
	default:
		return "UNKNOWN"
	}
}

// WorkUnit 은 Queue에서 Executor로 전달되는 작업 단위이다.
// 실제 Jenkins의 hudson.model.queue.WorkUnit에 대응한다.
// WorkUnit은 SubTask에 대한 참조와 실행 컨텍스트를 보유한다.
//
// 실제 구조 (WorkUnit.java):
//   - work: SubTask — 실제 수행할 작업
//   - context: WorkUnitContext — 실행 컨텍스트 (다른 WorkUnit과 공유 가능)
//   - setExecutor(Executor): 어떤 Executor가 이 작업을 수행하는지 기록
type WorkUnit struct {
	JobName       string        // 작업 이름
	RequiredLabel string        // 필요한 라벨 (빈 문자열이면 아무 노드에서 실행)
	Duration      time.Duration // 시뮬레이션 실행 시간
}

// Executor 는 실제 빌드를 수행하는 goroutine 기반 실행 단위이다.
// Jenkins의 Executor는 Thread를 상속하며, run() 메서드에서 작업을 수행한다.
//
// 핵심 필드 매핑:
//   owner(Computer) → computer
//   number(int) → Number
//   workUnit(WorkUnit) → currentWork
//   executable(Executable) → 현재 작업 (currentWork에 통합)
//   started(boolean) → 별도 관리 불필요 (goroutine 시작 = started)
type Executor struct {
	Number      int            // Executor 번호. 실제: number 필드
	computer    *Computer      // 소속 Computer. 실제: owner 필드 (@NonNull)
	state       ExecutorState  // 현재 상태
	currentWork *WorkUnit      // 현재 작업. 실제: workUnit 필드
	startTime   time.Time      // 작업 시작 시간. 실제: startTime 필드
	mu          sync.RWMutex   // 상태 보호. 실제: ReadWriteLock lock 필드
	workChan    chan *WorkUnit  // 작업 수신 채널 (Thread.start()를 대체)
	stopChan    chan struct{}   // 종료 신호 (interrupt()를 대체)
	doneChan    chan struct{}   // goroutine 종료 완료 알림
}

// NewExecutor 는 Executor를 생성하고 대기 goroutine을 시작한다.
// 실제 Jenkins에서 Executor 생성자: Executor(Computer owner, int n)
// → super("Executor #" + n + " for " + owner.getDisplayName())
// → this.owner = owner; this.queue = Jenkins.get().getQueue(); this.number = n;
//
// Jenkins 1.536부터 Executor는 온디맨드로 스레드를 시작한다.
// start(WorkUnit) 호출 전까지는 isParking() = true (started == false)
func NewExecutor(comp *Computer, number int) *Executor {
	e := &Executor{
		Number:   number,
		computer: comp,
		state:    StateIdle,
		workChan: make(chan *WorkUnit, 1),
		stopChan: make(chan struct{}, 1),
		doneChan: make(chan struct{}),
	}
	go e.run()
	return e
}

// run 은 Executor의 메인 루프이다.
// 실제 Jenkins의 Executor.run() (Executor.java:339-491)을 단순화한 것이다.
//
// 실제 run() 흐름:
//   1. owner가 온라인인지 확인 → 아니면 resetWorkUnit() → return
//   2. lock 획득 → startTime 설정 → workUnit 로컬 변수로 복사
//   3. Queue.callWithLock(): workUnit.setExecutor(this) → task 추출 → Executable 생성
//   4. executable.run() 실행 (queue.execute()를 통해)
//   5. finish1(problems): 소요 시간 기록, synchronizeEnd 호출
//   6. finish2(): owner.removeExecutor(this) → queue.scheduleMaintenance()
//
// 이 시뮬레이션에서는 goroutine의 for-select 루프로 작업 대기를 구현한다.
// 실제 Jenkins에서는 start(WorkUnit) 호출 시마다 새 Thread를 시작하지만,
// 여기서는 하나의 goroutine이 반복적으로 작업을 수신한다.
func (e *Executor) run() {
	defer close(e.doneChan)

	for {
		select {
		case work := <-e.workChan:
			// 작업 수신: start(WorkUnit) 호출에 대응
			// 실제: Executor.start(WorkUnit task) (Executor.java:810-819)
			//   → this.workUnit = task → super.start() → started = true
			e.mu.Lock()
			e.state = StateBusy
			e.currentWork = work
			e.startTime = time.Now()
			e.mu.Unlock()

			computerName := e.computer.GetNodeName()
			fmt.Printf("  [Executor #%d@%s] 작업 시작: '%s' (예상 소요: %v)\n",
				e.Number, computerName, work.JobName, work.Duration)

			// 빌드 실행 시뮬레이션
			// 실제: queue.execute(executable, task) (Executor.java:456)
			// 빌드 도중 인터럽트 가능하도록 select 사용
			interrupted := false
			timer := time.NewTimer(work.Duration)
			select {
			case <-timer.C:
				// 정상 완료
			case <-e.stopChan:
				// 인터럽트: 실제 Executor.interrupt(Result) 호출에 대응
				// 실제: interruptStatus = result → super.interrupt()
				timer.Stop()
				interrupted = true
			}

			// finish1 + finish2 단계
			// 실제 finish1 (Executor.java:493-510):
			//   → 소요 시간 계산, workUnit.context.synchronizeEnd() 호출
			// 실제 finish2 (Executor.java:512-522):
			//   → owner.removeExecutor(this) → queue.scheduleMaintenance()
			e.mu.Lock()
			elapsed := time.Since(e.startTime)
			jobName := e.currentWork.JobName
			e.currentWork = nil
			if interrupted {
				e.state = StateInterrupted
				fmt.Printf("  [Executor #%d@%s] 작업 중단됨: '%s' (경과: %v)\n",
					e.Number, computerName, jobName, elapsed.Round(time.Millisecond))
				e.state = StateIdle // 중단 후 idle 복귀
			} else {
				e.state = StateIdle
				fmt.Printf("  [Executor #%d@%s] 작업 완료: '%s' (소요: %v)\n",
					e.Number, computerName, jobName, elapsed.Round(time.Millisecond))
			}
			e.mu.Unlock()

			// finish2 콜백: Computer에 완료 알림
			// 실제: owner.removeExecutor(this) 후 addNewExecutorIfNecessary()
			// 이 시뮬레이션에서는 Executor가 계속 유지되므로 생략

		case <-e.stopChan:
			// 유휴 상태에서 종료 신호 수신
			// 실제: setNumExecutors()에서 idle Executor에 interrupt() 호출
			e.mu.Lock()
			e.state = StateDead
			e.mu.Unlock()
			return
		}
	}
}

// AssignWork 는 Queue가 Executor에 작업을 할당할 때 호출한다.
// 실제: Executor.start(WorkUnit task) (Executor.java:810-819)
//   → lock.writeLock().lock()
//   → this.workUnit = task → super.start() → started = true
//   → lock.writeLock().unlock()
func (e *Executor) AssignWork(work *WorkUnit) bool {
	e.mu.RLock()
	if e.state != StateIdle {
		e.mu.RUnlock()
		return false
	}
	e.mu.RUnlock()

	e.workChan <- work
	return true
}

// Interrupt 는 실행 중인 빌드를 중단한다.
// 실제: Executor.interrupt(Result result, boolean forShutdown, CauseOfInterruption... causes)
// (Executor.java:222-248)
//   → lock.writeLock().lock()
//   → started가 false면 owner.removeExecutor(this) → return
//   → interruptStatus = result
//   → causes에 CauseOfInterruption 추가
//   → asynchronousExecution이 있으면 그것을 interrupt, 아니면 super.interrupt()
func (e *Executor) Interrupt() {
	select {
	case e.stopChan <- struct{}{}:
	default:
	}
}

// IsIdle 은 Executor가 유휴 상태인지 반환한다.
// 실제: Executor.isIdle() (Executor.java:617-624)
//   → lock.readLock().lock() → return workUnit == null && executable == null
func (e *Executor) IsIdle() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.state == StateIdle
}

// IsBusy 는 Executor가 작업 중인지 반환한다.
// 실제: Executor.isBusy() (Executor.java:629-636)
//   → return workUnit != null || executable != null
func (e *Executor) IsBusy() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.state == StateBusy
}

// GetState 는 현재 상태를 반환한다.
func (e *Executor) GetState() ExecutorState {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.state
}

// GetCurrentJobName 은 현재 실행 중인 작업 이름을 반환한다.
func (e *Executor) GetCurrentJobName() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.currentWork != nil {
		return e.currentWork.JobName
	}
	return "(없음)"
}

// WaitDone 은 Executor goroutine 종료를 기다린다.
func (e *Executor) WaitDone() {
	<-e.doneChan
}

// =============================================================================
// 3. Computer: 런타임(Runtime) 계층
// =============================================================================
// Jenkins의 hudson.model.Computer에 대응한다.
// Computer는 Node의 "실행 상태"를 관리하며, Executor 풀을 보유한다.
//
// 핵심 필드 (Computer.java:175-192):
//   - executors: CopyOnWriteArrayList<Executor> — 일반 Executor 목록
//   - oneOffExecutors: CopyOnWriteArrayList<OneOffExecutor> — 일회성 Executor
//   - numExecutors: int — 설정된 Executor 수
//   - offlineCause: OfflineCause — 오프라인 원인
//   - nodeName: String — 대응하는 Node 이름
//
// 설정-런타임 분리의 핵심:
//   - Node 설정이 변경되면 setNode(node) 호출 → setNumExecutors(node.getNumExecutors())
//   - setNumExecutors()에서 초과 Executor는 interrupt()로 제거하고,
//     부족하면 addNewExecutorIfNecessary()로 생성
//   - 진행 중인 빌드(busy Executor)는 완료될 때까지 유지됨

// Computer 는 Node의 런타임 상태를 관리한다.
type Computer struct {
	node          *Node        // 대응하는 Node 설정. 실제: getNode()
	executors     []*Executor  // Executor 풀. 실제: CopyOnWriteArrayList<Executor>
	numExecutors  int          // 설정된 Executor 수
	isOnline      bool         // 온라인 여부
	offlineCause  string       // 오프라인 원인. 실제: OfflineCause offlineCause
	connectTime   time.Time    // 연결 시간
	idleStartTime time.Time    // 유휴 시작 시간 (RetentionStrategy 판단용)
	mu            sync.RWMutex // 동시성 보호
}

// NewComputer 는 Node로부터 Computer를 생성한다.
// 실제 Jenkins에서:
//   - Node.createComputer()가 Computer 생성 (추상 메서드)
//   - Computer 생성자에서 setNode(node) 호출
//   - setNode() → setNumExecutors(node.getNumExecutors())
//   - 연쇄적으로 addNewExecutorIfNecessary() 호출되어 Executor 생성
func NewComputer(node *Node) *Computer {
	c := &Computer{
		node:          node,
		numExecutors:  node.NumExecutors,
		isOnline:      true,
		connectTime:   time.Now(),
		idleStartTime: time.Now(),
	}
	// Executor 풀 초기화
	// 실제: addNewExecutorIfNecessary() (Computer.java:884-900)
	//   → 0부터 numExecutors-1까지 사용 가능한 번호 계산
	//   → 사용 중이 아닌 번호에 대해 new Executor(this, n) 생성
	c.executors = make([]*Executor, 0, node.NumExecutors)
	for i := 0; i < node.NumExecutors; i++ {
		c.executors = append(c.executors, NewExecutor(c, i))
	}
	return c
}

// GetNodeName 은 대응하는 Node의 이름을 반환한다.
func (c *Computer) GetNodeName() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.node != nil {
		return c.node.Name
	}
	return "(unknown)"
}

// GetNode 은 대응하는 Node를 반환한다.
func (c *Computer) GetNode() *Node {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.node
}

// SetNode 은 Node 설정이 변경될 때 호출된다.
// 실제: Computer.setNode(Node node) (Computer.java:805-813)
//   → this.nodeName = node.getNodeName()
//   → setNumExecutors(node.getNumExecutors())
func (c *Computer) SetNode(node *Node) {
	c.mu.Lock()
	c.node = node
	c.mu.Unlock()

	c.SetNumExecutors(node.NumExecutors)
}

// SetNumExecutors 는 Executor 수를 조정한다.
// 실제: Computer.setNumExecutors(int n) (Computer.java:861-882)
//
// 알고리즘:
//   1. this.numExecutors = n
//   2. diff = executors.size() - n
//   3. diff > 0 (초과): Queue.withLock()에서 idle Executor에 interrupt() 전송
//      → busy Executor는 완료까지 유지 (설정-런타임 분리의 핵심)
//   4. diff < 0 (부족): addNewExecutorIfNecessary()로 신규 생성
func (c *Computer) SetNumExecutors(n int) {
	c.mu.Lock()
	oldNum := c.numExecutors
	c.numExecutors = n
	currentLen := len(c.executors)
	c.mu.Unlock()

	diff := currentLen - n

	if diff > 0 {
		// 초과: idle Executor에 interrupt 전송
		// 실제: Queue.withLock(() -> { for (Executor e : executors) {
		//     if (e.isIdle()) e.interrupt(); } });
		fmt.Printf("  [Computer:%s] Executor 수 감소: %d → %d (초과 %d개 제거 시작)\n",
			c.GetNodeName(), oldNum, n, diff)
		removed := 0
		c.mu.Lock()
		for i := len(c.executors) - 1; i >= 0 && removed < diff; i-- {
			if c.executors[i].IsIdle() {
				c.executors[i].Interrupt()
				c.executors[i].WaitDone()
				c.executors = append(c.executors[:i], c.executors[i+1:]...)
				removed++
			}
		}
		c.mu.Unlock()
		fmt.Printf("  [Computer:%s] Idle Executor %d개 제거 완료\n", c.GetNodeName(), removed)
		if removed < diff {
			fmt.Printf("  [Computer:%s] Busy Executor %d개는 완료 후 제거 예정\n",
				c.GetNodeName(), diff-removed)
		}
	} else if diff < 0 {
		// 부족: 신규 Executor 생성
		// 실제: addNewExecutorIfNecessary() (Computer.java:884-900)
		fmt.Printf("  [Computer:%s] Executor 수 증가: %d → %d (신규 %d개 생성)\n",
			c.GetNodeName(), oldNum, n, -diff)
		c.mu.Lock()
		nextNum := currentLen
		for i := 0; i < -diff; i++ {
			c.executors = append(c.executors, NewExecutor(c, nextNum+i))
		}
		c.mu.Unlock()
	}
}

// FindIdleExecutor 는 유휴 상태의 Executor를 찾아 반환한다.
func (c *Computer) FindIdleExecutor() *Executor {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, e := range c.executors {
		if e.IsIdle() {
			return e
		}
	}
	return nil
}

// CountIdle 은 유휴 Executor 수를 반환한다.
// 실제: Computer.countIdle()
func (c *Computer) CountIdle() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	count := 0
	for _, e := range c.executors {
		if e.IsIdle() {
			count++
		}
	}
	return count
}

// CountBusy 는 작업 중인 Executor 수를 반환한다.
// 실제: Computer.countBusy()
func (c *Computer) CountBusy() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	count := 0
	for _, e := range c.executors {
		if e.IsBusy() {
			count++
		}
	}
	return count
}

// IsIdle 은 모든 Executor가 유휴 상태인지 반환한다.
// 실제: Computer.isIdle()
func (c *Computer) IsIdle() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, e := range c.executors {
		if !e.IsIdle() {
			return false
		}
	}
	return true
}

// SetOffline 은 컴퓨터를 오프라인으로 전환한다.
// 실제: Computer.setTemporarilyOffline(true, cause)
func (c *Computer) SetOffline(cause string) {
	c.mu.Lock()
	c.isOnline = false
	c.offlineCause = cause
	c.mu.Unlock()
	fmt.Printf("  [Computer:%s] 오프라인 전환: %s\n", c.GetNodeName(), cause)
}

// SetOnline 은 컴퓨터를 온라인으로 전환한다.
func (c *Computer) SetOnline() {
	c.mu.Lock()
	c.isOnline = true
	c.offlineCause = ""
	c.connectTime = time.Now()
	c.mu.Unlock()
	fmt.Printf("  [Computer:%s] 온라인 전환\n", c.GetNodeName())
}

// PrintStatus 는 Computer의 현재 상태를 출력한다.
func (c *Computer) PrintStatus() {
	c.mu.RLock()
	defer c.mu.RUnlock()

	status := "ONLINE"
	if !c.isOnline {
		status = "OFFLINE"
	}
	fmt.Printf("  %-12s | %-7s | 라벨: [%s] | 모드: %-9s | Executor: %d개\n",
		c.node.Name, status, strings.Join(c.node.Labels, ", "), c.node.Mode, len(c.executors))
	for _, e := range c.executors {
		stateStr := e.GetState().String()
		jobName := e.GetCurrentJobName()
		fmt.Printf("    └─ Executor #%d: %-12s 작업: %s\n", e.Number, stateStr, jobName)
	}
}

// Shutdown 은 모든 Executor를 종료한다.
func (c *Computer) Shutdown() {
	c.mu.Lock()
	executors := make([]*Executor, len(c.executors))
	copy(executors, c.executors)
	c.mu.Unlock()

	for _, e := range executors {
		e.Interrupt()
	}
	for _, e := range executors {
		e.WaitDone()
	}
}

// =============================================================================
// 4. RetentionStrategy: 에이전트 생명주기 관리
// =============================================================================
// Jenkins의 hudson.slaves.RetentionStrategy에 대응한다.
// 주기적으로 check(Computer)를 호출하여 에이전트를 온/오프라인으로 전환한다.
//
// RetentionStrategy.Demand (RetentionStrategy.java:190-301):
//   - inDemandDelay: 이 시간(분) 동안 수요가 있으면 에이전트 시작
//   - idleDelay: 이 시간(분) 동안 유휴이면 에이전트 오프라인
//   - check(SlaveComputer c):
//     → 오프라인이고 수요 있으면: connect(false)
//     → 온라인이고 유휴이면: idleDelay 초과 시 disconnect(IdleOfflineCause)

// RetentionStrategy 는 에이전트의 생명주기를 관리하는 전략이다.
type RetentionStrategy interface {
	// Check 는 주기적으로 호출되어 Computer의 상태를 점검한다.
	// 반환값: 다음 점검까지의 대기 시간
	Check(c *Computer) time.Duration
	// Name 은 전략 이름을 반환한다.
	Name() string
}

// AlwaysRetention 은 항상 온라인을 유지하는 전략이다.
// 실제: RetentionStrategy.Always (RetentionStrategy.java:161-185)
//   → check(): 오프라인이면 tryReconnect()
type AlwaysRetention struct{}

func (a *AlwaysRetention) Check(c *Computer) time.Duration {
	c.mu.RLock()
	online := c.isOnline
	c.mu.RUnlock()
	if !online {
		fmt.Printf("  [RetentionStrategy:Always] '%s' 오프라인 감지 → 재연결 시도\n", c.GetNodeName())
		c.SetOnline()
	}
	return 1 * time.Minute
}

func (a *AlwaysRetention) Name() string { return "Always" }

// DemandRetention 은 수요 기반으로 에이전트를 관리하는 전략이다.
// 실제: RetentionStrategy.Demand (RetentionStrategy.java:190-301)
//   - inDemandDelay: 수요 대기 시간 (분)
//   - idleDelay: 유휴 허용 시간 (분), 초과 시 오프라인
type DemandRetention struct {
	InDemandDelay time.Duration // 수요 발생 후 시작까지 대기 시간
	IdleDelay     time.Duration // 유휴 허용 시간
}

// Check 는 Demand 전략의 주기적 점검을 수행한다.
// 실제 (RetentionStrategy.java:230-291):
//   1. 오프라인이고 Queue에 이 노드가 필요한 작업이 있으면:
//      → inDemandDelay 초과 시 connect(false)
//   2. 온라인이고 유휴이면:
//      → idleDelay 초과 시 disconnect(IdleOfflineCause)
func (d *DemandRetention) Check(c *Computer) time.Duration {
	c.mu.RLock()
	online := c.isOnline
	idle := true
	for _, e := range c.executors {
		if e.IsBusy() {
			idle = false
			break
		}
	}
	idleStart := c.idleStartTime
	c.mu.RUnlock()

	if online && idle {
		idleDuration := time.Since(idleStart)
		if idleDuration > d.IdleDelay {
			// 유휴 시간 초과 → 오프라인 전환
			// 실제: c.disconnect(new OfflineCause.IdleOfflineCause())
			fmt.Printf("  [RetentionStrategy:Demand] '%s' 유휴 %v 초과 (한도: %v) → 오프라인 전환\n",
				c.GetNodeName(), idleDuration.Round(time.Millisecond), d.IdleDelay)
			c.SetOffline("IdleOfflineCause: 유휴 시간 초과")
		}
	}
	return 1 * time.Minute
}

func (d *DemandRetention) Name() string { return "Demand" }

// =============================================================================
// 5. 데모 시나리오
// =============================================================================

func printSeparator(title string) {
	fmt.Println()
	fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
	fmt.Printf("  %s\n", title)
	fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
}

func printSubSection(title string) {
	fmt.Printf("\n--- %s ---\n", title)
}

func main() {
	rand.Seed(time.Now().UnixNano())

	// =====================================================================
	// 데모 1: Node-Computer-Executor 3계층 생성
	// =====================================================================
	printSeparator("데모 1: Node-Computer-Executor 3계층 생성")

	// 마스터 노드 생성
	// 실제 Jenkins에서 Jenkins(마스터)도 Node를 상속한다.
	// Jenkins.java extends Node, numExecutors=2 (기본)
	masterNode := &Node{
		Name:            "master",
		NumExecutors:    2,
		Labels:          []string{"master", "linux"},
		NodeDescription: "Built-In Node (Jenkins 마스터)",
		Mode:            ModeNormal,
	}

	// 에이전트 노드 생성
	agentNode1 := &Node{
		Name:            "agent-linux-01",
		NumExecutors:    3,
		Labels:          []string{"linux", "docker", "java"},
		NodeDescription: "Linux 빌드 에이전트",
		Mode:            ModeNormal,
	}

	agentNode2 := &Node{
		Name:            "agent-windows-01",
		NumExecutors:    2,
		Labels:          []string{"windows", "dotnet"},
		NodeDescription: "Windows 빌드 에이전트",
		Mode:            ModeExclusive, // 라벨 지정 작업만 수행
	}

	fmt.Println("\n[1] Node(설정) 생성:")
	fmt.Printf("  %-20s | executors=%d | labels=[%s] | mode=%s\n",
		masterNode.Name, masterNode.NumExecutors,
		strings.Join(masterNode.Labels, ", "), masterNode.Mode)
	fmt.Printf("  %-20s | executors=%d | labels=[%s] | mode=%s\n",
		agentNode1.Name, agentNode1.NumExecutors,
		strings.Join(agentNode1.Labels, ", "), agentNode1.Mode)
	fmt.Printf("  %-20s | executors=%d | labels=[%s] | mode=%s\n",
		agentNode2.Name, agentNode2.NumExecutors,
		strings.Join(agentNode2.Labels, ", "), agentNode2.Mode)

	// Computer(런타임) 생성 — Node.createComputer() → Computer 생성자 → setNode()
	fmt.Println("\n[2] Computer(런타임) 생성 및 Executor 풀 초기화:")
	masterComp := NewComputer(masterNode)
	agentComp1 := NewComputer(agentNode1)
	agentComp2 := NewComputer(agentNode2)

	// Executor 초기화 대기
	time.Sleep(50 * time.Millisecond)

	computers := []*Computer{masterComp, agentComp1, agentComp2}
	fmt.Println("\n[3] 전체 노드 상태:")
	for _, c := range computers {
		c.PrintStatus()
	}

	// =====================================================================
	// 데모 2: 라벨 매칭과 작업 할당
	// =====================================================================
	printSeparator("데모 2: 라벨 매칭과 작업 할당")

	// 작업 목록
	jobs := []*WorkUnit{
		{JobName: "frontend-build", RequiredLabel: "linux", Duration: 400 * time.Millisecond},
		{JobName: "dotnet-test", RequiredLabel: "dotnet", Duration: 500 * time.Millisecond},
		{JobName: "generic-deploy", RequiredLabel: "", Duration: 300 * time.Millisecond},
		{JobName: "docker-image", RequiredLabel: "docker", Duration: 600 * time.Millisecond},
		{JobName: "windows-installer", RequiredLabel: "windows", Duration: 450 * time.Millisecond},
	}

	fmt.Println("\n[1] 작업 큐:")
	for _, job := range jobs {
		label := job.RequiredLabel
		if label == "" {
			label = "(없음 — 아무 NORMAL 노드)"
		}
		fmt.Printf("  - %-20s | 라벨 요구: %-30s | 소요: %v\n",
			job.JobName, label, job.Duration)
	}

	// 라벨 매칭 및 작업 할당
	// 실제 Jenkins Queue의 작업 배분 로직 단순화:
	//   1. Queue.maintain() → 각 BuildableItem에 대해 canTake() 검사
	//   2. 매칭되는 노드 중 idle Executor를 찾아 할당
	printSubSection("라벨 매칭 및 Executor 할당")

	for _, job := range jobs {
		assigned := false
		fmt.Printf("\n  작업 '%s' (라벨: '%s') 매칭:\n", job.JobName, job.RequiredLabel)

		for _, comp := range computers {
			node := comp.GetNode()
			canTake, reason := node.CanTake(job.RequiredLabel)
			if !canTake {
				fmt.Printf("    ✗ %-20s → 거부: %s\n", node.Name, reason)
				continue
			}
			fmt.Printf("    ✓ %-20s → 매칭 성공\n", node.Name)

			// 유휴 Executor 찾기
			executor := comp.FindIdleExecutor()
			if executor == nil {
				fmt.Printf("      → 유휴 Executor 없음, 다음 노드 탐색\n")
				continue
			}

			// 작업 할당
			if executor.AssignWork(job) {
				fmt.Printf("      → Executor #%d에 할당 완료\n", executor.Number)
				assigned = true
				break
			}
		}

		if !assigned {
			fmt.Printf("    → 어떤 노드에서도 실행할 수 없음 (큐에서 대기)\n")
		}
	}

	// 빌드 실행 대기
	fmt.Println("\n[2] 빌드 실행 중...")
	time.Sleep(200 * time.Millisecond)

	fmt.Println("\n[3] 중간 상태 점검:")
	for _, c := range computers {
		c.PrintStatus()
	}

	// 모든 빌드 완료 대기
	time.Sleep(800 * time.Millisecond)

	fmt.Println("\n[4] 모든 빌드 완료 후 상태:")
	for _, c := range computers {
		c.PrintStatus()
	}

	// =====================================================================
	// 데모 3: 설정-런타임 분리 — NumExecutors 변경
	// =====================================================================
	printSeparator("데모 3: 설정-런타임 분리 — NumExecutors 동적 변경")

	fmt.Println("\n시나리오: agent-linux-01의 Executor를 3개 → 5개로 증가")
	fmt.Println("  (실제: 관리자가 노드 설정에서 '동시 빌드 수'를 변경)")

	// 먼저 작업을 일부 할당하여 busy 상태 만들기
	busyJob := &WorkUnit{
		JobName:  "long-running-test",
		Duration: 1500 * time.Millisecond,
	}
	executor := agentComp1.FindIdleExecutor()
	if executor != nil {
		executor.AssignWork(busyJob)
		time.Sleep(50 * time.Millisecond)
	}

	// Node 설정 변경: Executor 5개로 증가
	updatedNode1 := &Node{
		Name:            "agent-linux-01",
		NumExecutors:    5,
		Labels:          []string{"linux", "docker", "java"},
		NodeDescription: "Linux 빌드 에이전트 (스케일업)",
		Mode:            ModeNormal,
	}

	// setNode() 호출 — 실제: Jenkins.updateNode(node)에 의해 호출됨
	fmt.Println("\n  Node 설정 변경 적용 (setNode):")
	agentComp1.SetNode(updatedNode1)

	time.Sleep(100 * time.Millisecond)
	fmt.Println("\n  변경 후 상태:")
	agentComp1.PrintStatus()

	// Executor 수 감소 시나리오
	printSubSection("시나리오: agent-linux-01의 Executor를 5개 → 2개로 감소")
	fmt.Println("  (busy Executor는 완료까지 유지 — 설정-런타임 분리의 핵심)")

	reducedNode1 := &Node{
		Name:            "agent-linux-01",
		NumExecutors:    2,
		Labels:          []string{"linux", "docker", "java"},
		NodeDescription: "Linux 빌드 에이전트 (스케일다운)",
		Mode:            ModeNormal,
	}

	agentComp1.SetNode(reducedNode1)
	time.Sleep(100 * time.Millisecond)
	fmt.Println("\n  감소 후 상태 (busy Executor는 유지됨):")
	agentComp1.PrintStatus()

	// long-running-test 완료 대기
	time.Sleep(1500 * time.Millisecond)

	fmt.Println("\n  long-running-test 완료 후 상태:")
	agentComp1.PrintStatus()

	// =====================================================================
	// 데모 4: RetentionStrategy — 유휴 에이전트 자동 오프라인
	// =====================================================================
	printSeparator("데모 4: RetentionStrategy — 유휴 에이전트 자동 오프라인")

	fmt.Println("\n  시뮬레이션: DemandRetention (유휴 200ms 초과 시 오프라인)")
	fmt.Println("  실제 Jenkins에서는 idleDelay가 분 단위이지만, 데모를 위해 밀리초 사용")

	// 짧은 유휴 감지 전략 생성
	demandStrategy := &DemandRetention{
		InDemandDelay: 100 * time.Millisecond,
		IdleDelay:     200 * time.Millisecond,
	}
	alwaysStrategy := &AlwaysRetention{}

	// agent-windows-01에 Demand 전략 적용 (유휴 시 자동 오프라인)
	fmt.Printf("\n  agent-windows-01에 '%s' 전략 적용\n", demandStrategy.Name())
	fmt.Printf("  master에 '%s' 전략 적용\n", alwaysStrategy.Name())

	// 유휴 시간 시뮬레이션
	agentComp2.mu.Lock()
	agentComp2.idleStartTime = time.Now().Add(-300 * time.Millisecond) // 300ms 전부터 유휴
	agentComp2.mu.Unlock()

	fmt.Println("\n  [1차 점검] agent-windows-01 (300ms 유휴 상태):")
	demandStrategy.Check(agentComp2)

	// Always 전략: 마스터가 오프라인이면 자동 복구
	fmt.Println("\n  master를 수동으로 오프라인 전환:")
	masterComp.SetOffline("수동 오프라인")

	fmt.Println("\n  [1차 점검] master (Always 전략):")
	alwaysStrategy.Check(masterComp)

	time.Sleep(50 * time.Millisecond)

	fmt.Println("\n  전략 적용 후 최종 상태:")
	for _, c := range computers {
		c.PrintStatus()
	}

	// =====================================================================
	// 데모 5: Node.Mode (NORMAL vs EXCLUSIVE)
	// =====================================================================
	printSeparator("데모 5: Node.Mode (NORMAL vs EXCLUSIVE) 동작 비교")

	fmt.Println("\n  NORMAL 모드: 라벨 없는 작업도 수행 가능")
	fmt.Println("  EXCLUSIVE 모드: 명시적으로 라벨을 지정한 작업만 수행")
	fmt.Println()

	testNodes := []*Node{
		{Name: "normal-node", NumExecutors: 2, Labels: []string{"java", "linux"}, Mode: ModeNormal},
		{Name: "exclusive-node", NumExecutors: 2, Labels: []string{"java", "linux"}, Mode: ModeExclusive},
	}

	testJobs := []struct {
		name  string
		label string
	}{
		{"라벨-없는-작업", ""},
		{"java-작업", "java"},
		{"python-작업", "python"},
	}

	fmt.Printf("  %-18s | %-12s | %-15s | 결과\n", "노드", "작업", "요구 라벨", )
	fmt.Printf("  %s\n", strings.Repeat("-", 70))

	for _, node := range testNodes {
		for _, job := range testJobs {
			canTake, reason := node.CanTake(job.label)
			label := job.label
			if label == "" {
				label = "(없음)"
			}
			result := "허용"
			if !canTake {
				result = "거부: " + reason
			}
			fmt.Printf("  %-18s | %-12s | %-15s | %s\n",
				node.Name, job.name, label, result)
		}
	}

	// =====================================================================
	// 정리
	// =====================================================================
	printSeparator("정리: 모든 Executor 종료")

	for _, c := range computers {
		fmt.Printf("  %s 종료 중...\n", c.GetNodeName())
		c.Shutdown()
	}

	fmt.Println("\n  모든 노드 종료 완료")

	// =====================================================================
	// 아키텍처 요약
	// =====================================================================
	printSeparator("Jenkins Node-Computer-Executor 3계층 아키텍처 요약")

	fmt.Println(`
  ┌─────────────────────────────────────────────────────────────┐
  │                    Jenkins Master                           │
  │  ┌─────────────────────────────────────────────────────┐    │
  │  │              Queue (빌드 대기열)                      │    │
  │  │  ┌───────┐ ┌───────┐ ┌───────┐                      │    │
  │  │  │ Job A │ │ Job B │ │ Job C │ ...                   │    │
  │  │  └───┬───┘ └───┬───┘ └───┬───┘                      │    │
  │  │      └─────────┴─────────┘                           │    │
  │  │              │ 라벨 매칭 + canTake()                  │    │
  │  └──────────────┼───────────────────────────────────────┘    │
  │                 ▼                                            │
  │  ┌──────────────────────────────────────────────────────┐   │
  │  │ Node (설정 계층)          Computer (런타임 계층)        │   │
  │  │ ┌──────────────┐         ┌──────────────────────┐    │   │
  │  │ │ name         │ ──1:1── │ executors[]           │    │   │
  │  │ │ numExecutors │ setNode │ ┌────────┐┌────────┐ │    │   │
  │  │ │ labels[]     │ ──────→ │ │Exec #0 ││Exec #1 │ │    │   │
  │  │ │ mode         │         │ │(IDLE)  ││(BUSY)  │ │    │   │
  │  │ │ description  │         │ └────────┘└────────┘ │    │   │
  │  │ └──────────────┘         │ isOnline, offlineCause│   │   │
  │  │                          └──────────────────────┘    │   │
  │  │                                                      │   │
  │  │ ※ Node 변경 → Computer.setNumExecutors() 호출       │   │
  │  │   → idle Executor: 즉시 제거                         │   │
  │  │   → busy Executor: 완료까지 유지 (설정-런타임 분리)   │   │
  │  └──────────────────────────────────────────────────────┘   │
  │                                                             │
  │  RetentionStrategy                                          │
  │  ┌─────────────────────────────────────────────────────┐    │
  │  │ check(Computer) 주기적 호출                          │    │
  │  │ Always: 항상 온라인 유지 (오프라인 시 reconnect)      │    │
  │  │ Demand: 유휴 시간 초과 → 오프라인, 수요 발생 → 온라인 │    │
  │  └─────────────────────────────────────────────────────┘    │
  └─────────────────────────────────────────────────────────────┘

  핵심 원칙:
    1. 설정(Node)과 런타임(Computer)의 분리
       → Node는 사용자 설정, Computer는 실행 상태
       → 설정 변경해도 진행 중인 빌드는 안전하게 유지

    2. Executor는 Thread(goroutine)
       → Queue에서 WorkUnit을 받아 실행
       → 완료 시 finish2() → owner.removeExecutor(this)

    3. 라벨 기반 스케줄링
       → Job의 requiredLabel과 Node의 labels 매칭
       → EXCLUSIVE 모드는 라벨 없는 작업 거부

    4. RetentionStrategy로 에이전트 생명주기 관리
       → Demand: 자원 효율적 (클라우드 에이전트에 적합)
       → Always: 상시 가용 (전용 에이전트에 적합)`)

	fmt.Println()
}
