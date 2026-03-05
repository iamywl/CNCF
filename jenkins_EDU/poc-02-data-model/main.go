// poc-02-data-model: Jenkins 핵심 데이터 모델 시뮬레이션
//
// Jenkins의 핵심 데이터 모델을 Go로 재현한다:
// 1. Job/Run 계층 구조: F-bounded polymorphism을 Go 인터페이스+컴포지션으로 시뮬레이션
// 2. 빌드 라이프사이클 상태 전이: NOT_STARTED → BUILDING → POST_PRODUCTION → COMPLETED
// 3. Action/Actionable 패턴: Run에 동적으로 데이터를 첨부하는 확장 패턴
// 4. Node/Computer/Executor 관계: 설정(Node) → 런타임(Computer) → 실행(Executor) 3계층
// 5. 빌드 번호 관리: nextBuildNumber 자동 증가, 빌드 히스토리 조회
//
// 실제 Jenkins 소스 참조:
//   - core/src/main/java/hudson/model/Job.java
//     → public abstract class Job<JobT extends Job<JobT, RunT>, RunT extends Run<JobT, RunT>>
//           extends AbstractItem implements ExtensionPoint, StaplerOverridable, ...
//     → 필드: nextBuildNumber, properties, keepDependencies
//     → 메서드: assignBuildNumber(), getLastBuild(), getBuildByNumber()
//
//   - core/src/main/java/hudson/model/Run.java
//     → public abstract class Run<JobT extends Job<JobT, RunT>, RunT extends Run<JobT, RunT>>
//           extends Actionable implements ExtensionPoint, Comparable<RunT>, ...
//     → 필드: number, timestamp, startTime, duration, result, state
//     → private enum State { NOT_STARTED, BUILDING, POST_PRODUCTION, COMPLETED }
//
//   - core/src/main/java/hudson/model/Result.java
//     → public final class Result implements Serializable, CustomExportedBean
//     → SUCCESS(0), UNSTABLE(1), FAILURE(2), NOT_BUILT(3), ABORTED(4)
//
//   - core/src/main/java/hudson/model/Actionable.java
//     → public abstract class Actionable extends AbstractModelObject
//     → private volatile CopyOnWriteArrayList<Action> actions
//
//   - core/src/main/java/hudson/model/Node.java
//     → public abstract class Node extends AbstractModelObject
//           implements ReconfigurableDescribable<Node>, ExtensionPoint, ...
//     → abstract int getNumExecutors(), abstract Mode getMode()
//
//   - core/src/main/java/hudson/model/Computer.java
//     → public abstract class Computer extends Actionable implements AccessControlled, ...
//     → CopyOnWriteArrayList<Executor> executors, int numExecutors
//
//   - core/src/main/java/hudson/model/Executor.java
//     → public class Executor extends Thread implements ModelObject, IExecutor
//     → protected final Computer owner; int number; Queue.Executable executable
//
//   - 상속 계층:
//     Job 쪽:  FreeStyleProject → Project → AbstractProject → Job → AbstractItem → Actionable
//     Run 쪽:  FreeStyleBuild → Build → AbstractBuild → Run → Actionable
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
// Result: 빌드 결과
// 실제: core/src/main/java/hudson/model/Result.java
// public final class Result implements Serializable, CustomExportedBean {
//     public static final Result SUCCESS  = new Result("SUCCESS",  BallColor.BLUE,     0, true);
//     public static final Result UNSTABLE = new Result("UNSTABLE", BallColor.YELLOW,   1, true);
//     public static final Result FAILURE  = new Result("FAILURE",  BallColor.RED,      2, true);
//     public static final Result NOT_BUILT= new Result("NOT_BUILT",BallColor.NOTBUILT, 3, false);
//     public static final Result ABORTED  = new Result("ABORTED",  BallColor.ABORTED,  4, false);
//     private final String name;
//     private final int ordinal;            // 심각도 순서 (낮을수록 양호)
//     private final boolean completeBuild;  // 빌드 완료 여부
// }
// =============================================================================

// Result는 Jenkins의 빌드 결과를 나타낸다.
// ordinal 값이 낮을수록 양호한 결과이며, isWorseThan()으로 비교한다.
type Result struct {
	Name          string // 결과 이름
	Ordinal       int    // 심각도 순서 (0=SUCCESS, 4=ABORTED)
	CompleteBuild bool   // 빌드가 완료된 것인지 여부
	Color         string // Ball 색상 (Jenkins UI에서 사용)
}

// Jenkins 표준 Result 인스턴스들
// 실제: Result.java의 static final 필드들과 동일한 ordinal 값
var (
	ResultSUCCESS  = &Result{"SUCCESS", 0, true, "blue"}
	ResultUNSTABLE = &Result{"UNSTABLE", 1, true, "yellow"}
	ResultFAILURE  = &Result{"FAILURE", 2, true, "red"}
	ResultNOT_BUILT = &Result{"NOT_BUILT", 3, false, "notbuilt"}
	ResultABORTED  = &Result{"ABORTED", 4, false, "aborted"}
)

// IsWorseThan은 현재 결과가 other보다 나쁜지 판단한다.
// 실제: Result.java → public boolean isWorseThan(@NonNull Result that) { return ordinal > that.ordinal; }
func (r *Result) IsWorseThan(other *Result) bool {
	return r.Ordinal > other.Ordinal
}

// IsWorseOrEqualTo는 현재 결과가 other보다 같거나 나쁜지 판단한다.
// 실제: Result.java → public boolean isWorseOrEqualTo(@NonNull Result that) { return ordinal >= that.ordinal; }
func (r *Result) IsWorseOrEqualTo(other *Result) bool {
	return r.Ordinal >= other.Ordinal
}

// CombineWith는 두 결과 중 더 나쁜 것을 반환한다.
// 실제: Result.java → public Result combine(Result that) { ... ordinal < that.ordinal ? that : this; }
func (r *Result) CombineWith(other *Result) *Result {
	if r.Ordinal < other.Ordinal {
		return other
	}
	return r
}

func (r *Result) String() string {
	return r.Name
}

// =============================================================================
// State: 빌드 상태 (라이프사이클)
// 실제: core/src/main/java/hudson/model/Run.java 내부 private enum
// private enum State {
//     NOT_STARTED,    // 빌드 생성/큐잉됨, 아직 시작 안 함
//     BUILDING,       // 빌드 진행 중
//     POST_PRODUCTION,// 빌드 완료, 결과 확정, 로그 파일 아직 업데이트 중
//     COMPLETED       // 빌드 완료, 로그 파일 닫힘
// }
// =============================================================================

type State int

const (
	// NOT_STARTED: 빌드가 생성/큐잉되었지만 아직 실행을 시작하지 않은 상태
	StateNOT_STARTED State = iota
	// BUILDING: 빌드가 실행 중인 상태. result 값이 이 상태에서 변경될 수 있음
	StateBUILDING
	// POST_PRODUCTION: 빌드 완료, 결과 확정. 로그 파일은 아직 업데이트 중.
	// Jenkins는 이 상태부터 빌드를 "완료"로 간주하여 후속 빌드 트리거 등을 수행.
	// See JENKINS-980.
	StatePOST_PRODUCTION
	// COMPLETED: 빌드 완전 종료. 로그 파일도 닫힘.
	StateCOMPLETED
)

var stateNames = map[State]string{
	StateNOT_STARTED:    "NOT_STARTED",
	StateBUILDING:       "BUILDING",
	StatePOST_PRODUCTION: "POST_PRODUCTION",
	StateCOMPLETED:      "COMPLETED",
}

func (s State) String() string {
	return stateNames[s]
}

// =============================================================================
// Action / Actionable 패턴
// 실제: core/src/main/java/hudson/model/Actionable.java
// public abstract class Actionable extends AbstractModelObject implements ModelObjectWithContextMenu {
//     private volatile CopyOnWriteArrayList<Action> actions;
//     public List<Action> getActions() { ... }  // 영속 액션만
//     public final List<? extends Action> getAllActions() { ... } // 영속 + 일시 액션
//     public void addAction(Action a) { ... }
//     public <T extends Action> T getAction(Class<T> type) { ... }
// }
//
// Action은 ModelObject를 확장하는 인터페이스로, Run/Job/Computer 등에 동적으로 첨부된다.
// 예: TestResultAction, CauseAction, ParametersAction 등이 빌드에 첨부됨.
// =============================================================================

// Action은 Actionable 객체에 첨부할 수 있는 동적 데이터 인터페이스.
// 실제: core/src/main/java/hudson/model/Action.java
// public interface Action extends ModelObject { ... }
type Action interface {
	GetDisplayName() string // 실제: ModelObject.getDisplayName()
	GetIconFileName() string // 실제: Action.getIconFileName()
	GetUrlName() string      // 실제: Action.getUrlName()
}

// Actionable은 Action 리스트를 가진 객체의 기반 구조체.
// 실제: Actionable.java의 CopyOnWriteArrayList<Action> actions 필드를 시뮬레이션.
// CopyOnWriteArrayList는 쓰기 시 복사하여 읽기 성능을 보장하는 스레드-안전 리스트.
type Actionable struct {
	actions []Action   // 실제: private volatile CopyOnWriteArrayList<Action> actions
	mu      sync.RWMutex // Go에서 CopyOnWriteArrayList 대신 RWMutex 사용
}

// AddAction은 Action을 추가한다.
// 실제: Actionable.java → public void addAction(@NonNull Action a) { ... getActions().add(a); }
func (a *Actionable) AddAction(action Action) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.actions = append(a.actions, action)
}

// GetAllActions는 모든 Action을 반환한다.
// 실제: Actionable.java → public final List<? extends Action> getAllActions()
func (a *Actionable) GetAllActions() []Action {
	a.mu.RLock()
	defer a.mu.RUnlock()
	result := make([]Action, len(a.actions))
	copy(result, a.actions)
	return result
}

// GetAction은 특정 타입의 Action을 찾아 반환한다.
// 실제: Actionable.java → public <T extends Action> T getAction(Class<T> type)
// Java의 제네릭 타입 매칭을 Go에서는 타입 이름 문자열 비교로 시뮬레이션
func (a *Actionable) GetAction(typeName string) Action {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, action := range a.actions {
		if action.GetDisplayName() == typeName {
			return action
		}
	}
	return nil
}

// =============================================================================
// 구체적인 Action 구현체들
// =============================================================================

// CauseAction은 빌드의 원인(Cause)을 기록하는 Action.
// 실제: core/src/main/java/hudson/model/CauseAction.java
// public class CauseAction implements FoldableAction, RunAction2 {
//     private final transient List<Cause> causes = new ArrayList<>();
// }
type CauseAction struct {
	Description string // 빌드 원인 설명 (예: "Started by user admin")
}

func (c *CauseAction) GetDisplayName() string  { return "CauseAction" }
func (c *CauseAction) GetIconFileName() string { return "" }
func (c *CauseAction) GetUrlName() string      { return "" }

// TestResultAction은 테스트 결과를 기록하는 Action.
// 실제: 플러그인 junit에서 제공 (hudson.tasks.junit.TestResultAction)
type TestResultAction struct {
	TotalCount  int // 전체 테스트 수
	FailCount   int // 실패한 테스트 수
	SkipCount   int // 스킵한 테스트 수
}

func (t *TestResultAction) GetDisplayName() string  { return "TestResultAction" }
func (t *TestResultAction) GetIconFileName() string { return "clipboard.png" }
func (t *TestResultAction) GetUrlName() string      { return "testReport" }

// ParametersAction은 빌드 파라미터를 기록하는 Action.
// 실제: core/src/main/java/hudson/model/ParametersAction.java
// public class ParametersAction implements RunAction2, Iterable<ParameterValue>, QueueAction {
//     private final List<ParameterValue> parameters;
// }
type ParametersAction struct {
	Parameters map[string]string // 파라미터 이름-값 쌍
}

func (p *ParametersAction) GetDisplayName() string  { return "ParametersAction" }
func (p *ParametersAction) GetIconFileName() string { return "" }
func (p *ParametersAction) GetUrlName() string      { return "" }

// =============================================================================
// Run: 빌드 실행 기록
// 실제: core/src/main/java/hudson/model/Run.java
// public abstract class Run<JobT extends Job<JobT, RunT>, RunT extends Run<JobT, RunT>>
//     extends Actionable implements ExtensionPoint, Comparable<RunT>, ...
//
// 핵심 필드:
//   protected final transient JobT project;    // 소속 Job
//   public transient int number;               // 빌드 번호
//   protected long timestamp;                  // 빌드 스케줄 시각
//   private long startTime;                    // 빌드 시작 시각
//   protected volatile Result result;          // 빌드 결과
//   private transient volatile State state;    // 빌드 상태
//   protected long duration;                   // 소요 시간(ms)
//   private long queueId;                      // Queue.Item.getId()
//   private boolean keepLog;                   // 빌드 보존 여부
// =============================================================================

// Run은 하나의 빌드 실행을 나타낸다.
type Run struct {
	Actionable                  // Actionable 임베딩 (Action/Actionable 패턴)
	Number      int             // 실제: public transient int number — 빌드 번호
	QueueId     int64           // 실제: private long queueId — 큐 아이템 ID
	Timestamp   time.Time       // 실제: protected long timestamp — 빌드 스케줄 시각
	StartTime   time.Time       // 실제: private long startTime — 빌드 실행 시작 시각
	Duration    time.Duration   // 실제: protected long duration — 빌드 소요 시간
	Result      *Result         // 실제: protected volatile Result result — 빌드 결과
	State       State           // 실제: private transient volatile State state
	KeepLog     bool            // 실제: private boolean keepLog — 빌드 보존 여부
	DisplayName string          // 실제: private volatile String displayName
	Description string          // 실제: protected volatile String description
	JobName     string          // 소속 Job 이름 (Go에서는 포인터 대신 이름으로 참조)
}

// IsBuilding은 빌드가 진행 중인지 반환한다.
// 실제: Run.java → public boolean isBuilding() { return state.compareTo(State.POST_PRODUCTION) < 0; }
func (r *Run) IsBuilding() bool {
	return r.State < StatePOST_PRODUCTION
}

// IsLogUpdated는 로그가 아직 업데이트 중인지 반환한다.
// 실제: Run.java → public boolean isLogUpdated() { return state.compareTo(State.COMPLETED) < 0; }
func (r *Run) IsLogUpdated() bool {
	return r.State < StateCOMPLETED
}

// GetDurationString은 소요 시간을 문자열로 반환한다.
// 실제: Run.java → public String getDurationString() { ... Util.getTimeSpanString(duration) ... }
func (r *Run) GetDurationString() string {
	if r.Duration == 0 {
		return "N/A"
	}
	return r.Duration.String()
}

// GetFullDisplayName은 "JobName #Number" 형태의 전체 표시 이름을 반환한다.
// 실제: Run.java → public String getFullDisplayName() { return project.getFullDisplayName() + " #" + number; }
func (r *Run) GetFullDisplayName() string {
	display := r.DisplayName
	if display == "" {
		display = fmt.Sprintf("#%d", r.Number)
	}
	return fmt.Sprintf("%s %s", r.JobName, display)
}

// =============================================================================
// Job: 빌드 대상 (작업) 정의
// 실제: core/src/main/java/hudson/model/Job.java
// public abstract class Job<JobT extends Job<JobT, RunT>, RunT extends Run<JobT, RunT>>
//     extends AbstractItem implements ExtensionPoint, StaplerOverridable, ...
//
// 핵심 필드:
//   protected transient volatile int nextBuildNumber = 1; // 다음 빌드 번호
//   boolean keepDependencies;                              // 의존성 보존 여부
//   protected CopyOnWriteList<JobProperty<? super JobT>> properties; // Job 속성 목록
//
// 상속 계층:
//   FreeStyleProject → Project → AbstractProject → Job → AbstractItem → Actionable
//
// AbstractItem (core/src/main/java/hudson/model/AbstractItem.java):
//   public abstract class AbstractItem extends Actionable implements Loadable, Item, ...
//   → name 필드, getFullName(), getUrl() 등 제공
//
// AbstractProject (core/src/main/java/hudson/model/AbstractProject.java):
//   public abstract class AbstractProject<P, R> extends Job<P, R> implements BuildableItem, ...
//   → SCM scm, List<Trigger<?>> triggers, BuildAuthorizationToken authToken 등
//
// Project (core/src/main/java/hudson/model/Project.java):
//   public abstract class Project<P, B> extends AbstractProject<P, B> implements SCMTriggerItem, ...
//   → DescribableList<Builder,Descriptor<Builder>> builders
//   → DescribableList<BuildWrapper,Descriptor<BuildWrapper>> buildWrappers
//   → DescribableList<Publisher,Descriptor<Publisher>> publishers
// =============================================================================

// Job은 빌드 대상(작업)을 나타낸다.
type Job struct {
	Actionable                         // Actionable 임베딩
	Name             string            // 실제: AbstractItem.name — Job 이름
	FullName         string            // 실제: AbstractItem.getFullName() — 전체 경로
	NextBuildNumber  int               // 실제: Job.nextBuildNumber — 다음 빌드 번호
	KeepDependencies bool              // 실제: Job.keepDependencies
	Description      string            // 실제: AbstractItem.description
	Disabled         bool              // 실제: AbstractProject.disabled — 비활성화 여부
	Builds           []*Run            // 빌드 히스토리 (실제: SortedMap<Integer,RunT> 기반의 RunMap)
	Properties       map[string]string // 실제: Job.properties — Job 속성 목록 (간소화)
	mu               sync.Mutex        // 빌드 번호 할당 동기화
}

// AssignBuildNumber는 새 빌드 번호를 할당하고 nextBuildNumber를 증가시킨다.
// 실제: Job.java → public int assignBuildNumber() throws IOException {
//     return ExtensionList.lookupFirst(BuildNumberAssigner.class).assignBuildNumber(this, ...);
// }
// 기본 구현 (DefaultBuildNumberAssigner):
//     synchronized (job) { int r = job.nextBuildNumber++; saveNextBuildNumber.call(); return r; }
func (j *Job) AssignBuildNumber() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	num := j.NextBuildNumber
	j.NextBuildNumber++
	return num
}

// GetLastBuild는 가장 최근 빌드를 반환한다.
// 실제: Job.java → public RunT getLastBuild()
func (j *Job) GetLastBuild() *Run {
	if len(j.Builds) == 0 {
		return nil
	}
	return j.Builds[len(j.Builds)-1]
}

// GetLastSuccessfulBuild는 가장 최근 성공 빌드를 반환한다.
// 실제: Job.java → public RunT getLastSuccessfulBuild()
func (j *Job) GetLastSuccessfulBuild() *Run {
	for i := len(j.Builds) - 1; i >= 0; i-- {
		if j.Builds[i].Result != nil && j.Builds[i].Result == ResultSUCCESS {
			return j.Builds[i]
		}
	}
	return nil
}

// GetLastFailedBuild는 가장 최근 실패 빌드를 반환한다.
// 실제: Job.java → public RunT getLastFailedBuild()
func (j *Job) GetLastFailedBuild() *Run {
	for i := len(j.Builds) - 1; i >= 0; i-- {
		if j.Builds[i].Result != nil && j.Builds[i].Result == ResultFAILURE {
			return j.Builds[i]
		}
	}
	return nil
}

// GetBuildByNumber는 빌드 번호로 빌드를 찾는다.
// 실제: Job.java → public RunT getBuildByNumber(int n)
func (j *Job) GetBuildByNumber(number int) *Run {
	for _, build := range j.Builds {
		if build.Number == number {
			return build
		}
	}
	return nil
}

// ScheduleBuild는 새 빌드를 생성하고 실행하는 전체 파이프라인을 시뮬레이션한다.
// 실제 Jenkins에서는 Queue → Executor → Run.execute() 순서로 진행되지만,
// 여기서는 데이터 모델 중심으로 상태 전이를 시뮬레이션한다.
func (j *Job) ScheduleBuild(cause string, params map[string]string) *Run {
	buildNum := j.AssignBuildNumber()

	run := &Run{
		Number:    buildNum,
		QueueId:   int64(buildNum * 100), // 실제: Queue.Item.getId()에서 할당
		Timestamp: time.Now(),
		State:     StateNOT_STARTED,
		JobName:   j.Name,
	}

	// CauseAction 첨부 — 빌드 원인 기록
	// 실제: Run 생성 시 CauseAction이 addAction()으로 첨부됨
	run.AddAction(&CauseAction{Description: cause})

	// ParametersAction 첨부 (파라미터가 있는 경우)
	// 실제: ParameterizedJobMixIn에서 빌드 파라미터를 ParametersAction으로 첨부
	if len(params) > 0 {
		run.AddAction(&ParametersAction{Parameters: params})
	}

	j.Builds = append(j.Builds, run)
	return run
}

// =============================================================================
// Node / Computer / Executor: 빌드 인프라 3계층
//
// Node (설정 계층):
//   실제: core/src/main/java/hudson/model/Node.java
//   "Nodes are persisted objects that capture user configurations,
//    and instances get thrown away and recreated whenever the configuration changes."
//   → 사용자 설정을 담는 영속 객체. 설정 변경 시 재생성됨.
//   → abstract int getNumExecutors()
//   → abstract Mode getMode() — NORMAL/EXCLUSIVE
//
// Computer (런타임 계층):
//   실제: core/src/main/java/hudson/model/Computer.java
//   "Running state of nodes are captured by Computers."
//   → Node의 런타임 상태를 관리. executors 리스트, 온라인/오프라인 상태.
//   → CopyOnWriteArrayList<Executor> executors
//   → int numExecutors
//
// Executor (실행 계층):
//   실제: core/src/main/java/hudson/model/Executor.java
//   "Thread that executes builds."
//   → Computer에 속한 스레드. Queue에서 할당된 작업을 실행.
//   → protected final Computer owner
//   → int number (Executor 번호)
//   → Queue.Executable executable (현재 실행 중인 작업, null이면 유휴)
// =============================================================================

// ExecutorState는 Executor의 현재 상태를 나타낸다.
type ExecutorState int

const (
	ExecutorIDLE ExecutorState = iota // 유휴 상태 — executable == null
	ExecutorBUSY                      // 작업 실행 중 — executable != null
)

var executorStateNames = map[ExecutorState]string{
	ExecutorIDLE: "IDLE",
	ExecutorBUSY: "BUSY",
}

func (s ExecutorState) String() string {
	return executorStateNames[s]
}

// Executor는 빌드를 실행하는 스레드를 나타낸다.
// 실제: Executor extends Thread
// → 생성자에서 Computer.owner를 받고, Queue에서 WorkUnit을 할당받으면 start()
type Executor struct {
	Number       int            // 실제: private int number — Executor 번호 (Computer 내에서 고유)
	State        ExecutorState  // IDLE 또는 BUSY
	CurrentBuild *Run           // 실제: private Queue.Executable executable
	StartTime    time.Time      // 실제: private long startTime — 현재 작업 시작 시각
	OwnerName    string         // 소속 Computer 이름
}

// Computer는 Node의 런타임 상태를 관리한다.
// 실제: Computer extends Actionable
// → Node 하나에 Computer 하나가 대응
// → Executor 목록을 관리하며, 온라인/오프라인 상태를 추적
type Computer struct {
	Actionable                     // Actionable 임베딩
	Name           string          // 실제: Computer.getName() — 표시 이름
	NodeName       string          // 실제: Computer.getNode().getNodeName() — 소속 Node 이름
	Executors      []*Executor     // 실제: CopyOnWriteArrayList<Executor> executors
	NumExecutors   int             // 실제: private int numExecutors
	IsOnline       bool            // 실제: isOffline()의 반대
	IsAcceptingTasks bool          // 실제: isAcceptingTasks()
}

// GetIdleExecutor는 유휴 상태인 Executor를 반환한다.
func (c *Computer) GetIdleExecutor() *Executor {
	for _, e := range c.Executors {
		if e.State == ExecutorIDLE {
			return e
		}
	}
	return nil
}

// GetBusyExecutors는 실행 중인 Executor 수를 반환한다.
// 실제: Computer.java → public int countBusy()
func (c *Computer) GetBusyExecutors() int {
	count := 0
	for _, e := range c.Executors {
		if e.State == ExecutorBUSY {
			count++
		}
	}
	return count
}

// Node는 빌드 에이전트의 설정을 담는 영속 객체이다.
// 실제: Node extends AbstractModelObject
// → Jenkins 마스터(Jenkins 클래스)도 Node를 확장한다.
// → Slave는 원격 에이전트를 나타내는 Node 구현체.
type Node struct {
	Name          string    // 실제: Node.getNodeName()
	Description   string    // 실제: Node.getNodeDescription()
	NumExecutors  int       // 실제: abstract int getNumExecutors()
	Labels        []string  // 실제: Node.getLabelString() — 라벨 목록
	Mode          string    // 실제: abstract Mode getMode() — "NORMAL" 또는 "EXCLUSIVE"
	Computer      *Computer // 런타임 상태 (Node → Computer 1:1 매핑)
}

// CreateComputer는 Node에 대응하는 Computer를 생성한다.
// 실제: Node.java → public abstract Computer createComputer()
// Slave 구현에서: new SlaveComputer(this)
func (n *Node) CreateComputer() *Computer {
	executors := make([]*Executor, n.NumExecutors)
	for i := 0; i < n.NumExecutors; i++ {
		executors[i] = &Executor{
			Number:    i,
			State:     ExecutorIDLE,
			OwnerName: n.Name,
		}
	}

	computer := &Computer{
		Name:             n.Name,
		NodeName:         n.Name,
		Executors:        executors,
		NumExecutors:     n.NumExecutors,
		IsOnline:         true,
		IsAcceptingTasks: true,
	}

	n.Computer = computer
	return computer
}

// =============================================================================
// 빌드 라이프사이클 실행 엔진
// 실제 Jenkins의 빌드 실행 흐름:
//   1. Queue.schedule() → WaitingItem 생성
//   2. Queue.maintain() → BuildableItem으로 전환
//   3. Executor.run() → Queue에서 WorkUnit 할당, executable 설정
//   4. Run.execute() → 상태 전이 실행
//      → state = BUILDING, result = null 초기화
//      → run() 메서드 호출 (실제 빌드 수행)
//      → state = POST_PRODUCTION (결과 확정, 후속 빌드 트리거 가능)
//      → 로그 닫기, 알림 전송
//      → state = COMPLETED
//
// 실제: Run.java → public final synchronized void execute(@NonNull RunExecution job)
// =============================================================================

// ExecuteBuild는 빌드의 상태 전이를 시뮬레이션한다.
// 실제 Jenkins의 Run.execute() 메서드의 상태 전이 순서를 재현.
func ExecuteBuild(run *Run, executor *Executor) {
	fmt.Printf("\n  --- 빌드 실행: %s ---\n", run.GetFullDisplayName())

	// 1단계: NOT_STARTED → BUILDING
	// 실제: Run.execute() → state = State.BUILDING
	run.State = StateBUILDING
	run.StartTime = time.Now()
	executor.State = ExecutorBUSY
	executor.CurrentBuild = run
	executor.StartTime = run.StartTime
	fmt.Printf("  [%s] 상태: %s → %s (Executor #%d에서 실행)\n",
		run.GetFullDisplayName(), StateNOT_STARTED, StateBUILDING, executor.Number)

	// 빌드 실행 시뮬레이션 (랜덤 결과)
	buildDuration := time.Duration(100+rand.Intn(400)) * time.Millisecond
	time.Sleep(50 * time.Millisecond) // 약간의 지연으로 시뮬레이션

	// 빌드 결과 결정 (랜덤)
	outcomes := []*Result{ResultSUCCESS, ResultSUCCESS, ResultSUCCESS, ResultUNSTABLE, ResultFAILURE}
	run.Result = outcomes[rand.Intn(len(outcomes))]
	run.Duration = buildDuration

	// 테스트 결과 Action 첨부
	totalTests := 10 + rand.Intn(90)
	failTests := 0
	skipTests := rand.Intn(5)
	if run.Result == ResultUNSTABLE {
		failTests = 1 + rand.Intn(5)
	} else if run.Result == ResultFAILURE {
		failTests = 5 + rand.Intn(10)
	}
	run.AddAction(&TestResultAction{
		TotalCount: totalTests,
		FailCount:  failTests,
		SkipCount:  skipTests,
	})

	// 2단계: BUILDING → POST_PRODUCTION
	// 실제: Run.execute() 에서 runner.post(listener) 호출 후
	//       state = State.POST_PRODUCTION
	// "Jenkins will now see this build as completed" — 후속 빌드 트리거 가능
	run.State = StatePOST_PRODUCTION
	fmt.Printf("  [%s] 상태: %s → %s (결과: %s, 소요: %s)\n",
		run.GetFullDisplayName(), StateBUILDING, StatePOST_PRODUCTION,
		run.Result, run.GetDurationString())

	// 3단계: POST_PRODUCTION → COMPLETED
	// 실제: Run.execute() 마지막에서 state = State.COMPLETED, 로그 파일 닫기
	run.State = StateCOMPLETED
	executor.State = ExecutorIDLE
	executor.CurrentBuild = nil
	fmt.Printf("  [%s] 상태: %s → %s\n",
		run.GetFullDisplayName(), StatePOST_PRODUCTION, StateCOMPLETED)
}

// =============================================================================
// 데모 시나리오 헬퍼 함수들
// =============================================================================

func printSeparator(title string) {
	fmt.Printf("\n%s\n", strings.Repeat("=", 80))
	fmt.Printf("  %s\n", title)
	fmt.Printf("%s\n", strings.Repeat("=", 80))
}

func printSubSeparator(title string) {
	fmt.Printf("\n  --- %s ---\n", title)
}

// =============================================================================
// 메인 데모
// =============================================================================

func main() {
	rand.Seed(time.Now().UnixNano())

	fmt.Println("╔══════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║       Jenkins 핵심 데이터 모델 시뮬레이션 (PoC-02)                  ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════════╝")

	// =========================================================================
	// 1. Job/Run 상속 계층 시각화
	// =========================================================================
	printSeparator("1. Job/Run 상속 계층 구조")

	fmt.Println(`
  ┌─────────────────────────────────────────────────────────────────────────┐
  │ Job 계층 (실제 Java 상속 체인)                                         │
  │                                                                         │
  │   Actionable                                        ← Action 리스트     │
  │     └─ AbstractItem                                 ← name, fullName    │
  │         └─ Job<JobT, RunT>                          ← nextBuildNumber   │
  │             └─ AbstractProject<P, R>                ← scm, triggers     │
  │                 └─ Project<P, B>                    ← builders, publs.  │
  │                     └─ FreeStyleProject             ← 구체적 잡 타입    │
  │                                                                         │
  │ Run 계층 (실제 Java 상속 체인)                                         │
  │                                                                         │
  │   Actionable                                        ← Action 리스트     │
  │     └─ Run<JobT, RunT>                              ← number, result    │
  │         └─ AbstractBuild<P, R>                      ← builtOn, wkspace  │
  │             └─ Build<P, B>                          ← SCM checkout      │
  │                 └─ FreeStyleBuild                   ← 구체적 빌드 타입  │
  └─────────────────────────────────────────────────────────────────────────┘

  제네릭 바인딩 (F-bounded polymorphism):
  ┌─────────────────────────────────────────────────────────────────────────┐
  │ Job<JobT extends Job<JobT, RunT>, RunT extends Run<JobT, RunT>>       │
  │ Run<JobT extends Job<JobT, RunT>, RunT extends Run<JobT, RunT>>       │
  │                                                                         │
  │ 예: FreeStyleProject extends Project<FreeStyleProject, FreeStyleBuild>│
  │     FreeStyleBuild   extends Build<FreeStyleProject, FreeStyleBuild>  │
  │                                                                         │
  │ → getLastBuild()가 Run이 아닌 FreeStyleBuild를 반환 (타입 안전)       │
  └─────────────────────────────────────────────────────────────────────────┘`)

	// =========================================================================
	// 2. Result 비교 시뮬레이션
	// =========================================================================
	printSeparator("2. Result 비교 (심각도 순서)")

	allResults := []*Result{ResultSUCCESS, ResultUNSTABLE, ResultFAILURE, ResultNOT_BUILT, ResultABORTED}
	fmt.Println("\n  Result 인스턴스 목록:")
	fmt.Println("  ┌──────────────┬─────────┬───────────────┬────────┐")
	fmt.Println("  │ Name         │ Ordinal │ CompleteBuild │ Color  │")
	fmt.Println("  ├──────────────┼─────────┼───────────────┼────────┤")
	for _, r := range allResults {
		fmt.Printf("  │ %-12s │    %d    │ %-13v │ %-6s │\n",
			r.Name, r.Ordinal, r.CompleteBuild, r.Color)
	}
	fmt.Println("  └──────────────┴─────────┴───────────────┴────────┘")

	fmt.Println("\n  Result.isWorseThan() 비교 매트릭스:")
	fmt.Print("  ")
	fmt.Printf("%-12s", "isWorseThan")
	for _, r := range allResults {
		fmt.Printf("  %-10s", r.Name)
	}
	fmt.Println()
	fmt.Print("  ")
	fmt.Println(strings.Repeat("-", 62))
	for _, r1 := range allResults {
		fmt.Printf("  %-12s", r1.Name)
		for _, r2 := range allResults {
			if r1.IsWorseThan(r2) {
				fmt.Printf("  %-10s", "true")
			} else {
				fmt.Printf("  %-10s", "false")
			}
		}
		fmt.Println()
	}

	fmt.Println("\n  Result.combine() 예시:")
	r1 := ResultSUCCESS
	r2 := ResultUNSTABLE
	fmt.Printf("  SUCCESS.combine(UNSTABLE) = %s (더 나쁜 쪽을 취함)\n", r1.CombineWith(r2))
	r3 := ResultFAILURE
	fmt.Printf("  UNSTABLE.combine(FAILURE) = %s\n", r2.CombineWith(r3))

	// =========================================================================
	// 3. Node / Computer / Executor 계층 시뮬레이션
	// =========================================================================
	printSeparator("3. Node / Computer / Executor 3계층")

	fmt.Println(`
  ┌───────────────────────────────────────────────────────────────────────┐
  │ 설정 계층 (영속)         런타임 계층             실행 계층            │
  │                                                                       │
  │   Node                    Computer               Executor             │
  │   ├─ name                 ├─ executors[]          ├─ number            │
  │   ├─ numExecutors         ├─ isOnline             ├─ state (IDLE/BUSY) │
  │   ├─ labels               ├─ isAcceptingTasks     ├─ executable        │
  │   └─ mode (NORMAL/EXCL)   └─ numExecutors         └─ startTime        │
  │                                                                       │
  │   "설정 변경 시 Node는     "런타임 상태를 관리.    "Thread 기반으로     │
  │    재생성됨"                Online/Offline 추적"    빌드를 실행"        │
  │                                                                       │
  │   1:1 매핑 ──────────────►                                            │
  │              Node.createComputer()                                    │
  │                                1:N 매핑 ─────────►                    │
  │                                 numExecutors개 생성                   │
  └───────────────────────────────────────────────────────────────────────┘`)

	// 마스터 노드 생성
	masterNode := &Node{
		Name:         "(built-in)",
		Description:  "the Jenkins controller's built-in node",
		NumExecutors: 2,
		Labels:       []string{"master", "built-in"},
		Mode:         "NORMAL",
	}
	masterComputer := masterNode.CreateComputer()

	// 에이전트 노드 생성
	agentNode := &Node{
		Name:         "agent-linux-01",
		Description:  "Linux build agent",
		NumExecutors: 4,
		Labels:       []string{"linux", "docker", "java"},
		Mode:         "EXCLUSIVE",
	}
	agentComputer := agentNode.CreateComputer()

	nodes := []*Node{masterNode, agentNode}
	computers := []*Computer{masterComputer, agentComputer}

	printSubSeparator("Node 설정 정보")
	fmt.Println("  ┌──────────────────┬──────────┬────────────────────────┬───────────┐")
	fmt.Println("  │ Name             │ #Exec    │ Labels                 │ Mode      │")
	fmt.Println("  ├──────────────────┼──────────┼────────────────────────┼───────────┤")
	for _, n := range nodes {
		fmt.Printf("  │ %-16s │    %d     │ %-22s │ %-9s │\n",
			n.Name, n.NumExecutors, strings.Join(n.Labels, ","), n.Mode)
	}
	fmt.Println("  └──────────────────┴──────────┴────────────────────────┴───────────┘")

	printSubSeparator("Computer 런타임 상태")
	for _, c := range computers {
		fmt.Printf("  Computer[%s]: online=%v, accepting=%v, executors=%d\n",
			c.Name, c.IsOnline, c.IsAcceptingTasks, c.NumExecutors)
		for _, e := range c.Executors {
			fmt.Printf("    Executor #%d: %s\n", e.Number, e.State)
		}
	}

	// =========================================================================
	// 4. Job 생성 + 빌드 실행 (상태 전이 데모)
	// =========================================================================
	printSeparator("4. Job 생성 → 빌드 실행 → 상태 전이")

	// FreeStyleProject 시뮬레이션
	// 실제: FreeStyleProject extends Project<FreeStyleProject, FreeStyleBuild>
	job := &Job{
		Name:            "my-webapp",
		FullName:        "my-webapp",
		NextBuildNumber: 1,
		Description:     "웹 애플리케이션 빌드 잡",
		Properties: map[string]string{
			"GIT_URL":   "https://github.com/example/webapp.git",
			"GIT_BRANCH": "main",
		},
	}

	fmt.Printf("\n  Job 생성: name=%s, nextBuildNumber=%d\n", job.Name, job.NextBuildNumber)
	fmt.Printf("  설명: %s\n", job.Description)

	// 빌드 3회 실행
	printSubSeparator("빌드 3회 실행")

	for i := 0; i < 3; i++ {
		cause := fmt.Sprintf("Started by user admin (빌드 #%d)", i+1)
		params := map[string]string{
			"DEPLOY_ENV": []string{"dev", "staging", "prod"}[i],
		}

		// 빌드 스케줄 (실제: Queue.schedule() → Run 생성)
		run := job.ScheduleBuild(cause, params)
		fmt.Printf("\n  빌드 스케줄: %s (nextBuildNumber=%d)\n",
			run.GetFullDisplayName(), job.NextBuildNumber)

		// Executor 할당 (실제: Queue → Executor.run() → 작업 할당)
		executor := masterComputer.GetIdleExecutor()
		if executor == nil {
			executor = agentComputer.GetIdleExecutor()
		}
		if executor == nil {
			fmt.Printf("  [경고] 사용 가능한 Executor 없음 — 큐에서 대기\n")
			continue
		}

		// 빌드 실행 (상태 전이)
		ExecuteBuild(run, executor)
	}

	// =========================================================================
	// 5. Action/Actionable 패턴 데모
	// =========================================================================
	printSeparator("5. Action/Actionable 패턴")

	fmt.Println(`
  ┌───────────────────────────────────────────────────────────────────────┐
  │ Actionable 패턴의 핵심:                                              │
  │                                                                       │
  │  Run/Job/Computer 등은 모두 Actionable을 확장                        │
  │  → 어떤 객체든 Action을 동적으로 첨부/조회할 수 있음                 │
  │  → 플러그인이 기존 클래스를 수정하지 않고 데이터를 추가하는 패턴     │
  │                                                                       │
  │  대표적인 Action 구현체:                                              │
  │  ├─ CauseAction        — 빌드 원인 기록                              │
  │  ├─ ParametersAction   — 빌드 파라미터                               │
  │  ├─ TestResultAction   — 테스트 결과 (junit 플러그인)                │
  │  ├─ ChangeLogSet       — SCM 변경 이력                               │
  │  └─ BadgeAction        — UI 배지                                     │
  └───────────────────────────────────────────────────────────────────────┘`)

	if lastBuild := job.GetLastBuild(); lastBuild != nil {
		fmt.Printf("\n  마지막 빌드 [%s]에 첨부된 Action 목록:\n", lastBuild.GetFullDisplayName())
		fmt.Println("  ┌────┬────────────────────┬──────────────────────────────────────────┐")
		fmt.Println("  │ #  │ Type               │ Details                                  │")
		fmt.Println("  ├────┼────────────────────┼──────────────────────────────────────────┤")
		for i, action := range lastBuild.GetAllActions() {
			detail := ""
			switch a := action.(type) {
			case *CauseAction:
				detail = fmt.Sprintf("cause=%q", a.Description)
			case *ParametersAction:
				pairs := []string{}
				for k, v := range a.Parameters {
					pairs = append(pairs, fmt.Sprintf("%s=%s", k, v))
				}
				detail = strings.Join(pairs, ", ")
			case *TestResultAction:
				detail = fmt.Sprintf("total=%d, fail=%d, skip=%d",
					a.TotalCount, a.FailCount, a.SkipCount)
			}
			fmt.Printf("  │ %2d │ %-18s │ %-40s │\n", i+1, action.GetDisplayName(), detail)
		}
		fmt.Println("  └────┴────────────────────┴──────────────────────────────────────────┘")
	}

	// =========================================================================
	// 6. 빌드 히스토리 조회
	// =========================================================================
	printSeparator("6. 빌드 히스토리 조회")

	fmt.Printf("\n  Job [%s] 빌드 히스토리:\n", job.Name)
	fmt.Println("  ┌───────┬──────────────┬────────────┬──────────────┬──────────────────────┐")
	fmt.Println("  │ Build │ Result       │ Duration   │ State        │ Cause                │")
	fmt.Println("  ├───────┼──────────────┼────────────┼──────────────┼──────────────────────┤")
	for _, build := range job.Builds {
		resultStr := "N/A"
		if build.Result != nil {
			resultStr = build.Result.String()
		}
		causeStr := ""
		for _, action := range build.GetAllActions() {
			if ca, ok := action.(*CauseAction); ok {
				causeStr = ca.Description
				if len(causeStr) > 20 {
					causeStr = causeStr[:17] + "..."
				}
			}
		}
		fmt.Printf("  │ #%-4d │ %-12s │ %-10s │ %-12s │ %-20s │\n",
			build.Number, resultStr, build.GetDurationString(), build.State, causeStr)
	}
	fmt.Println("  └───────┴──────────────┴────────────┴──────────────┴──────────────────────┘")

	// 빌드 조회 메서드 데모
	printSubSeparator("빌드 조회 메서드")
	if lb := job.GetLastBuild(); lb != nil {
		fmt.Printf("  getLastBuild()           → %s (result=%s)\n", lb.GetFullDisplayName(), lb.Result)
	}
	if lsb := job.GetLastSuccessfulBuild(); lsb != nil {
		fmt.Printf("  getLastSuccessfulBuild() → %s\n", lsb.GetFullDisplayName())
	} else {
		fmt.Println("  getLastSuccessfulBuild() → (없음)")
	}
	if lfb := job.GetLastFailedBuild(); lfb != nil {
		fmt.Printf("  getLastFailedBuild()     → %s\n", lfb.GetFullDisplayName())
	} else {
		fmt.Println("  getLastFailedBuild()     → (없음)")
	}
	if b := job.GetBuildByNumber(1); b != nil {
		fmt.Printf("  getBuildByNumber(1)      → %s (result=%s)\n", b.GetFullDisplayName(), b.Result)
	}

	// =========================================================================
	// 7. 빌드 번호 관리 메커니즘
	// =========================================================================
	printSeparator("7. 빌드 번호 관리 (nextBuildNumber)")

	fmt.Println(`
  ┌───────────────────────────────────────────────────────────────────────┐
  │ nextBuildNumber 메커니즘:                                             │
  │                                                                       │
  │  Job.java:                                                            │
  │    protected transient volatile int nextBuildNumber = 1;              │
  │                                                                       │
  │  별도 파일에 저장: JENKINS_HOME/jobs/{name}/nextBuildNumber            │
  │  → VCS에서 config.xml만 관리해도 빌드 번호는 독립적으로 유지          │
  │                                                                       │
  │  assignBuildNumber() 흐름:                                            │
  │    synchronized(job) {                                                │
  │      int r = job.nextBuildNumber++;                                   │
  │      saveNextBuildNumber.call();  // 파일에 저장                      │
  │      return r;                                                        │
  │    }                                                                  │
  │                                                                       │
  │  onLoad() 시:                                                         │
  │    nextBuildNumberFile.readTrim()으로 복원                             │
  │    파일이 깨진 경우 lastBuild.number + 1로 복구                       │
  └───────────────────────────────────────────────────────────────────────┘`)

	fmt.Printf("\n  현재 nextBuildNumber: %d\n", job.NextBuildNumber)
	fmt.Println("  동시 빌드 번호 할당 시뮬레이션 (goroutine 10개):")

	// 동시 빌드 번호 할당 시뮬레이션
	// 실제 Jenkins에서는 synchronized(job) { ... }로 보호
	concurrentJob := &Job{
		Name:            "concurrent-test",
		NextBuildNumber: 1,
	}

	var wg sync.WaitGroup
	assignedNumbers := make([]int, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			num := concurrentJob.AssignBuildNumber()
			assignedNumbers[idx] = num
		}(i)
	}
	wg.Wait()

	fmt.Printf("  할당된 번호: %v\n", assignedNumbers)
	fmt.Printf("  최종 nextBuildNumber: %d\n", concurrentJob.NextBuildNumber)

	// 중복 검사
	seen := make(map[int]bool)
	duplicates := false
	for _, n := range assignedNumbers {
		if seen[n] {
			duplicates = true
			break
		}
		seen[n] = true
	}
	if duplicates {
		fmt.Println("  [경고] 중복 발견! (동기화 문제)")
	} else {
		fmt.Println("  [확인] 중복 없음 — synchronized(Mutex) 보호 정상 동작")
	}

	// =========================================================================
	// 8. 빌드 상태 전이 다이어그램
	// =========================================================================
	printSeparator("8. 빌드 상태 전이 다이어그램 (Run.State)")

	fmt.Println(`
  ┌─────────────────────────────────────────────────────────────────────────┐
  │                     빌드 상태 전이 (Run.State)                         │
  │                                                                         │
  │   ┌──────────────┐    Run.execute()     ┌──────────┐                   │
  │   │ NOT_STARTED  │ ──────────────────► │ BUILDING │                   │
  │   │              │    state=BUILDING    │          │                   │
  │   │ 빌드 생성/   │    startTime 기록    │ 빌드 실행│                   │
  │   │ 큐에서 대기  │                      │ result   │                   │
  │   └──────────────┘                      │ 변경 가능│                   │
  │                                          └────┬─────┘                   │
  │                                               │                         │
  │                                    runner.post(listener)                │
  │                                    결과 확정                            │
  │                                               │                         │
  │                                          ┌────▼──────────┐              │
  │                                          │POST_PRODUCTION│              │
  │                                          │               │              │
  │                                          │ 빌드 "완료"   │              │
  │                                          │ 후속 트리거   │              │
  │                                          │ 로그 업데이트 │              │
  │                                          └────┬──────────┘              │
  │                                               │                         │
  │                                    로그 닫기, 알림 전송                 │
  │                                               │                         │
  │                                          ┌────▼─────┐                   │
  │                                          │COMPLETED │                   │
  │                                          │          │                   │
  │                                          │ 모든 작업│                   │
  │                                          │ 종료     │                   │
  │                                          └──────────┘                   │
  │                                                                         │
  │  isBuilding()  = state < POST_PRODUCTION  (NOT_STARTED, BUILDING)      │
  │  isLogUpdated()= state < COMPLETED        (NOT_STARTED..POST_PROD)     │
  └─────────────────────────────────────────────────────────────────────────┘`)

	// =========================================================================
	// 9. Executor 상태 변화 추적
	// =========================================================================
	printSeparator("9. Executor 상태 변화 추적")

	fmt.Println("\n  전체 Executor 현황:")
	fmt.Println("  ┌──────────────────┬────────────┬───────────┬──────────────────────┐")
	fmt.Println("  │ Computer         │ Executor # │ State     │ Current Build        │")
	fmt.Println("  ├──────────────────┼────────────┼───────────┼──────────────────────┤")
	for _, c := range computers {
		for _, e := range c.Executors {
			buildStr := "(none)"
			if e.CurrentBuild != nil {
				buildStr = e.CurrentBuild.GetFullDisplayName()
			}
			fmt.Printf("  │ %-16s │     #%-5d │ %-9s │ %-20s │\n",
				c.Name, e.Number, e.State, buildStr)
		}
	}
	fmt.Println("  └──────────────────┴────────────┴───────────┴──────────────────────┘")

	for _, c := range computers {
		fmt.Printf("  Computer[%s]: busy=%d, idle=%d, total=%d\n",
			c.Name, c.GetBusyExecutors(), c.NumExecutors-c.GetBusyExecutors(), c.NumExecutors)
	}

	// =========================================================================
	// 10. 데이터 모델 관계 요약
	// =========================================================================
	printSeparator("10. 데이터 모델 관계 요약")

	fmt.Println(`
  ┌───────────────────────────────────────────────────────────────────────────┐
  │                    Jenkins 핵심 데이터 모델 관계                          │
  │                                                                           │
  │   Jenkins (싱글톤)                                                       │
  │   ├─ jobs: Map<String, TopLevelItem>                                     │
  │   │   └─ Job<JobT, RunT>                                                 │
  │   │       ├─ nextBuildNumber: int          ← 자동 증가, 별도 파일 저장   │
  │   │       ├─ properties: List<JobProperty> ← Job 설정 속성               │
  │   │       └─ builds: SortedMap<Integer, RunT>                            │
  │   │           └─ Run<JobT, RunT>                                         │
  │   │               ├─ number, timestamp, duration, result, state          │
  │   │               └─ actions: List<Action> ← 동적 데이터 첨부            │
  │   │                   ├─ CauseAction       (빌드 원인)                   │
  │   │                   ├─ ParametersAction   (빌드 파라미터)              │
  │   │                   └─ TestResultAction   (테스트 결과)                │
  │   │                                                                       │
  │   ├─ nodes: List<Node>                     ← 영속 설정                   │
  │   │   └─ Node                                                             │
  │   │       ├─ numExecutors, labels, mode                                  │
  │   │       └─ createComputer() ──────┐                                    │
  │   │                                  ▼                                    │
  │   └─ computers: Map<Node, Computer>  ← 런타임 상태                       │
  │       └─ Computer                                                         │
  │           ├─ isOnline, numExecutors                                       │
  │           └─ executors: List<Executor>                                    │
  │               └─ Executor (Thread)                                        │
  │                   ├─ number, state (IDLE/BUSY)                            │
  │                   └─ executable → Run 실행                               │
  └───────────────────────────────────────────────────────────────────────────┘

  핵심 설계 원칙:
  1. F-bounded polymorphism  → Job<J,R>과 Run<J,R>이 서로 참조하여 타입 안전 보장
  2. Action/Actionable       → 플러그인이 클래스 수정 없이 동적 데이터 첨부
  3. Node/Computer 분리      → 설정(영속)과 런타임(일시) 관심사 분리
  4. nextBuildNumber 파일    → config.xml과 독립적으로 빌드 번호 관리`)

	fmt.Println()
}
