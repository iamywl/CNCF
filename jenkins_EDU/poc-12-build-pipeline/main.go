// poc-12-build-pipeline: Jenkins Freestyle 빌드 파이프라인 시뮬레이션
//
// Jenkins 소스코드 참조:
//   - hudson.tasks.BuildStep (인터페이스: prebuild/perform/getRequiredMonitorService)
//   - hudson.tasks.BuildStepCompatibilityLayer (하위 호환성 레이어)
//   - hudson.tasks.Builder (BuildStep 구현 - 실제 빌드 수행)
//   - hudson.tasks.Publisher (BuildStep 구현 - 빌드 후 처리)
//   - hudson.tasks.Recorder (Publisher 하위 - 결과 기록, 빌드 결과 변경 가능)
//   - hudson.tasks.Notifier (Publisher 하위 - 외부 알림, Recorder 이후 실행)
//   - hudson.tasks.BuildWrapper (빌드 환경 setUp/tearDown)
//   - hudson.tasks.BuildStepMonitor (NONE/STEP/BUILD 동시성 제어)
//   - hudson.model.CheckPoint (동시 빌드 간 세밀한 동기화)
//   - hudson.triggers.Trigger (cron 기반 빌드 트리거)
//   - hudson.model.Build.BuildExecution.doRun() (빌드 실행 오케스트레이션)
//   - hudson.model.AbstractBuild.AbstractRunner.performAllBuildSteps() (Publisher 실행)
//
// 핵심 원리:
//   1) Freestyle 빌드는 Trigger → BuildWrapper.setUp → Builder(순차)
//      → Publisher(순차, Recorder 먼저 → Notifier) → BuildWrapper.tearDown 순서로 실행
//   2) BuildStep.perform()은 BuildStepMonitor를 통해 동시 빌드 동기화 수준을 결정
//   3) BuildStepMonitor.BUILD: 이전 빌드 완전 완료 후 실행 (가장 보수적)
//      BuildStepMonitor.STEP: 이전 빌드의 같은 스텝 완료 후 실행
//      BuildStepMonitor.NONE: 동기화 없이 독립 실행 (권장)
//   4) CheckPoint.report()/block()으로 세밀한 Barrier 동기화 가능
//   5) Publisher 내에서 Recorder → Notifier 순서가 보장됨
//      (Publisher.DescriptorExtensionListImpl의 classify 메서드)
//   6) Builder.perform() 실패 시 즉시 중단, Publisher는 모두 실행 (하나 실패해도 계속)
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
// 1. BuildResult: 빌드 결과
// =============================================================================
// Jenkins: hudson.model.Result
// - SUCCESS, UNSTABLE, FAILURE, NOT_BUILT, ABORTED
// - 결과는 "악화"만 가능 (SUCCESS → UNSTABLE은 가능, FAILURE → SUCCESS는 불가)

// BuildResult는 빌드 실행 결과를 나타낸다.
type BuildResult int

const (
	SUCCESS  BuildResult = iota // 빌드 성공
	UNSTABLE                    // 불안정 (테스트 실패 등)
	FAILURE                     // 빌드 실패
	ABORTED                     // 빌드 중단
)

func (r BuildResult) String() string {
	switch r {
	case SUCCESS:
		return "SUCCESS"
	case UNSTABLE:
		return "UNSTABLE"
	case FAILURE:
		return "FAILURE"
	case ABORTED:
		return "ABORTED"
	default:
		return "UNKNOWN"
	}
}

// Combine은 두 결과 중 더 나쁜 쪽을 선택한다.
// Jenkins: Result.combine() — 결과는 악화 방향으로만 합산
func Combine(a, b BuildResult) BuildResult {
	if a > b {
		return a
	}
	return b
}

// =============================================================================
// 2. BuildStepMonitor: 동시 빌드 동기화 수준
// =============================================================================
// Jenkins: hudson.tasks.BuildStepMonitor (enum)
//   - NONE: 독립 실행 (동기화 없음, 권장)
//   - STEP: 이전 빌드의 같은 스텝 완료 대기 (CheckPoint 사용)
//   - BUILD: 이전 빌드 완전 완료 대기 (CheckPoint.COMPLETED.block())
//
// 실제 코드 (BuildStepMonitor.java):
//   STEP.perform() {
//       CheckPoint cp = new CheckPoint(bs.getClass().getName(), bs.getClass());
//       cp.block(listener, displayName);
//       try { return bs.perform(...); }
//       finally { cp.report(); }
//   }

// MonitorLevel은 동시 빌드 동기화 수준을 정의한다.
type MonitorLevel int

const (
	MonitorNone  MonitorLevel = iota // 동기화 없음 (권장)
	MonitorStep                      // 이전 빌드의 같은 스텝 완료 대기
	MonitorBuild                     // 이전 빌드 전체 완료 대기
)

func (m MonitorLevel) String() string {
	switch m {
	case MonitorNone:
		return "NONE"
	case MonitorStep:
		return "STEP"
	case MonitorBuild:
		return "BUILD"
	default:
		return "UNKNOWN"
	}
}

// =============================================================================
// 3. CheckPoint: 동시 빌드 간 세밀한 동기화 지점
// =============================================================================
// Jenkins: hudson.model.CheckPoint
//   - report(): 체크포인트 도달을 알림 (후속 빌드 대기 해제)
//   - block(): 이전 빌드의 같은 체크포인트 도달까지 대기
//   - 미리 정의된 상수:
//     COMPLETED: 빌드 완료 (AbstractBuild.isBuilding()==false)
//     MAIN_COMPLETED: Builder 단계 완료, Publisher 진입 시
//     CULPRITS_DETERMINED: 빌드 원인 분석 완료
//
// Barrier 패턴: report()가 호출되면 block()에서 대기 중인 스레드가 풀림

// CheckPoint는 동시 빌드 간 동기화 지점이다.
type CheckPoint struct {
	name     string
	identity string
	mu       sync.Mutex
	reported bool
	waitCh   chan struct{}
}

// NewCheckPoint는 새 체크포인트를 생성한다.
func NewCheckPoint(name string) *CheckPoint {
	return &CheckPoint{
		name:   name,
		waitCh: make(chan struct{}),
	}
}

// Report는 이 체크포인트에 도달했음을 알린다.
// Jenkins: CheckPoint.report() → Run.reportCheckpoint(this)
func (cp *CheckPoint) Report() {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if !cp.reported {
		cp.reported = true
		close(cp.waitCh)
	}
}

// Block은 이 체크포인트 도달까지 대기한다.
// Jenkins: CheckPoint.block() → Run.waitForCheckpoint(this, null, null)
func (cp *CheckPoint) Block() {
	<-cp.waitCh
}

// 미리 정의된 체크포인트 (실제 Jenkins에서도 static final로 선언)
var (
	// CheckPointCompleted는 빌드 완료 시점.
	// Jenkins: CheckPoint.COMPLETED = new CheckPoint("COMPLETED")
	CheckPointCompleted = NewCheckPoint("COMPLETED")

	// CheckPointMainCompleted는 Builder 단계 완료, Publisher 진입 시점.
	// Jenkins: CheckPoint.MAIN_COMPLETED = new CheckPoint("MAIN_COMPLETED")
	CheckPointMainCompleted = NewCheckPoint("MAIN_COMPLETED")
)

// =============================================================================
// 4. BuildListener: 빌드 출력 수집기
// =============================================================================
// Jenkins: hudson.model.BuildListener (interface)
// - getLogger(): PrintStream 반환
// - 빌드 중 모든 로그는 이 리스너를 통해 출력

// BuildListener는 빌드 로그를 수집하는 리스너이다.
type BuildListener struct {
	prefix string
	logs   []string
}

func NewBuildListener(prefix string) *BuildListener {
	return &BuildListener{prefix: prefix}
}

func (bl *BuildListener) Log(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	full := fmt.Sprintf("[%s] %s", bl.prefix, msg)
	bl.logs = append(bl.logs, full)
	fmt.Println(full)
}

// =============================================================================
// 5. Build: 빌드 실행 컨텍스트
// =============================================================================
// Jenkins: hudson.model.AbstractBuild
//   - 빌드 번호, 결과, 환경변수, 워크스페이스 등을 보유
//   - buildEnvironments: BuildWrapper.setUp()이 반환한 Environment 리스트

// Build는 하나의 빌드 실행 컨텍스트이다.
type Build struct {
	Number      int
	ProjectName string
	Result      BuildResult
	EnvVars     map[string]string // 환경변수
	StartTime   time.Time
	EndTime     time.Time
	Listener    *BuildListener
}

func NewBuild(number int, projectName string) *Build {
	return &Build{
		Number:      number,
		ProjectName: projectName,
		Result:      SUCCESS,
		EnvVars:     make(map[string]string),
		Listener:    NewBuildListener(fmt.Sprintf("Build#%d", number)),
	}
}

// SetResult는 빌드 결과를 악화 방향으로만 설정한다.
// Jenkins: AbstractBuild.setResult(Result) — 이미 더 나쁜 결과가 있으면 무시
func (b *Build) SetResult(result BuildResult) {
	b.Result = Combine(b.Result, result)
}

// =============================================================================
// 6. BuildStep 인터페이스: 빌드의 한 단계
// =============================================================================
// Jenkins: hudson.tasks.BuildStep (interface)
//   - prebuild(AbstractBuild, BuildListener): 빌드 전 검증
//   - perform(AbstractBuild, Launcher, BuildListener): 실제 실행
//   - getRequiredMonitorService(): 동시성 동기화 수준 반환
//
// BuildStepCompatibilityLayer는 < 1.150 플러그인과의 호환성을 위한 추상 클래스.
// SimpleBuildStep은 현대적 API (Run + FilePath 기반).

// BuildStep은 빌드의 한 단계를 나타내는 인터페이스이다.
type BuildStep interface {
	// Name은 이 빌드 스텝의 표시 이름을 반환한다.
	Name() string

	// Prebuild는 빌드 시작 전 검증을 수행한다.
	// false를 반환하면 빌드가 중단된다.
	Prebuild(build *Build) bool

	// Perform은 빌드 스텝을 실행한다.
	// false를 반환하면 실패로 간주한다.
	Perform(build *Build) bool

	// GetRequiredMonitorService는 동시 빌드 동기화 수준을 반환한다.
	GetRequiredMonitorService() MonitorLevel
}

// =============================================================================
// 7. Builder: 실제 빌드 작업 수행
// =============================================================================
// Jenkins: hudson.tasks.Builder extends BuildStepCompatibilityLayer
//   - getRequiredMonitorService(): 기본값 NONE (Builder는 보통 이전 빌드에 의존하지 않음)
//   - 예시 구현: Shell (셸 명령 실행), BatchFile (배치 파일 실행)
//   - Build.BuildExecution.build()에서 순차 실행
//   - 하나라도 false 반환 시 즉시 중단 (나머지 Builder 스킵)

// ShellBuilder는 셸 명령을 실행하는 Builder이다.
// Jenkins: hudson.tasks.Shell extends CommandInterpreter extends Builder
type ShellBuilder struct {
	name    string
	command string
	// 시뮬레이션: 실패 확률
	failRate float64
}

func NewShellBuilder(name, command string, failRate float64) *ShellBuilder {
	return &ShellBuilder{name: name, command: command, failRate: failRate}
}

func (s *ShellBuilder) Name() string { return s.name }

func (s *ShellBuilder) Prebuild(build *Build) bool {
	build.Listener.Log("  [Builder] %s: prebuild 검증 통과", s.name)
	return true
}

func (s *ShellBuilder) Perform(build *Build) bool {
	build.Listener.Log("  [Builder] %s: 실행 — %s", s.name, s.command)
	// 시뮬레이션: 빌드 시간 소요
	time.Sleep(time.Duration(50+rand.Intn(100)) * time.Millisecond)

	if rand.Float64() < s.failRate {
		build.Listener.Log("  [Builder] %s: 실패!", s.name)
		return false
	}
	build.Listener.Log("  [Builder] %s: 성공", s.name)
	return true
}

// Builder는 기본적으로 MonitorNone을 반환한다.
// Jenkins: Builder.getRequiredMonitorService() returns BuildStepMonitor.NONE
func (s *ShellBuilder) GetRequiredMonitorService() MonitorLevel {
	return MonitorNone
}

// =============================================================================
// 8. Publisher / Recorder / Notifier: 빌드 후 처리
// =============================================================================
// Jenkins 계층:
//   Publisher (abstract) ← Recorder (abstract) ← 구체 플러그인
//   Publisher (abstract) ← Notifier (abstract) ← 구체 플러그인
//
// Publisher.needsToRunAfterFinalized():
//   - false(기본): post2() 단계에서 실행 (빌드 결과 변경 가능)
//   - true: cleanUp() 단계에서 실행 (빌드 결과 변경 불가)
//
// Publisher 정렬 (Publisher.ExtensionComponentComparator):
//   Recorder(0) → 미분류(1) → Notifier(2)
//   이를 통해 Recorder가 먼저 실행되어 결과를 확정한 뒤 Notifier가 알림
//
// performAllBuildSteps()에서 Publisher는 하나 실패해도 나머지 계속 실행
//   (Builder와 다른 점: Builder는 하나 실패 시 즉시 중단)

// PublisherKind는 Publisher의 종류를 나타낸다.
type PublisherKind int

const (
	KindRecorder PublisherKind = iota // 결과 기록 (빌드 결과 변경 가능)
	KindNotifier                      // 외부 알림 (Recorder 이후 실행)
)

// Publisher는 빌드 후 처리를 수행하는 BuildStep이다.
type Publisher struct {
	name                   string
	kind                   PublisherKind
	needsToRunAfterFinalize bool
	monitorLevel           MonitorLevel
	action                 func(build *Build) bool
}

func NewRecorder(name string, action func(build *Build) bool) *Publisher {
	return &Publisher{
		name:         name,
		kind:         KindRecorder,
		monitorLevel: MonitorBuild, // Publisher 기본값: BUILD
		action:       action,
	}
}

func NewNotifier(name string, action func(build *Build) bool) *Publisher {
	return &Publisher{
		name:         name,
		kind:         KindNotifier,
		monitorLevel: MonitorBuild,
		action:       action,
	}
}

func (p *Publisher) Name() string { return p.name }

func (p *Publisher) Prebuild(build *Build) bool {
	build.Listener.Log("  [Publisher] %s: prebuild 검증 통과", p.name)
	return true
}

func (p *Publisher) Perform(build *Build) bool {
	kindStr := "Recorder"
	if p.kind == KindNotifier {
		kindStr = "Notifier"
	}
	build.Listener.Log("  [Publisher/%s] %s: 실행 중...", kindStr, p.name)
	time.Sleep(time.Duration(30+rand.Intn(50)) * time.Millisecond)

	result := p.action(build)
	if result {
		build.Listener.Log("  [Publisher/%s] %s: 완료", kindStr, p.name)
	} else {
		build.Listener.Log("  [Publisher/%s] %s: 실패", kindStr, p.name)
	}
	return result
}

// Publisher의 기본 MonitorLevel은 BUILD이다.
// Jenkins: BuildStep 인터페이스의 default는 BUILD (레거시 호환)
func (p *Publisher) GetRequiredMonitorService() MonitorLevel {
	return p.monitorLevel
}

// NeedsToRunAfterFinalized는 빌드 완료 후 실행해야 하는지 반환한다.
// Jenkins: Publisher.needsToRunAfterFinalized()
//   true 반환 시 cleanUp 단계에서 실행 (빌드 결과 변경 불가)
func (p *Publisher) NeedsToRunAfterFinalized() bool {
	return p.needsToRunAfterFinalize
}

// =============================================================================
// 9. BuildWrapper: 빌드 환경 래핑
// =============================================================================
// Jenkins: hudson.tasks.BuildWrapper (abstract class)
//   - setUp(AbstractBuild, Launcher, BuildListener) → Environment
//   - Environment.tearDown(AbstractBuild, BuildListener) → boolean
//   - decorateLauncher(): 런처 장식 (preCheckout 이전에 호출)
//   - preCheckout(): SCM checkout 이전에 호출
//   - makeBuildVariables(): 빌드 변수 추가
//
// 빌드 실행 순서에서의 위치:
//   1. decorateLauncher() — 가장 먼저
//   2. preCheckout() — SCM checkout 전
//   3. setUp() — SCM checkout 후, Builder 실행 전
//   4. (Builder 실행)
//   5. (Publisher 실행)
//   6. tearDown() — 가장 마지막, 빌드 실패해도 반드시 호출
//
// tearDown은 역순으로 실행됨 (스택처럼 LIFO)
// Jenkins: AbstractBuildExecution.tearDownBuildEnvironments()
//   for (int i = buildEnvironments.size() - 1; i >= 0; i--) ...

// WrapperEnvironment는 BuildWrapper.setUp()이 반환하는 환경 객체이다.
type WrapperEnvironment struct {
	name    string
	tearFn  func(build *Build) bool
	envVars map[string]string
}

// BuildWrapper는 빌드 환경을 설정하고 정리하는 래퍼이다.
type BuildWrapper struct {
	name  string
	setUp func(build *Build) (*WrapperEnvironment, error)
}

func NewBuildWrapper(name string, setUp func(build *Build) (*WrapperEnvironment, error)) *BuildWrapper {
	return &BuildWrapper{name: name, setUp: setUp}
}

// =============================================================================
// 10. Trigger: 빌드 시작 조건
// =============================================================================
// Jenkins: hudson.triggers.Trigger<J extends Item>
//   - start(J project, boolean newInstance): 트리거 시작
//   - run(): 주기적 호출 (cron 표현식 매칭 시)
//   - stop(): 트리거 중지
//   - spec: cron 탭 스펙 ("H/5 * * * *" 등)
//   - tabs: CronTabList (파싱된 cron 표현식)
//
// 하위 구현:
//   - SCMTrigger: SCM 폴링으로 변경 감지 시 빌드
//   - TimerTrigger: cron 스케줄에 따라 무조건 빌드
//
// Trigger.Cron (PeriodicWork 확장):
//   - 매 분마다 실행되어 모든 트리거의 cron 표현식을 체크
//   - tabs.check(cal)이 true이면 trigger.run() 호출

// TriggerType은 트리거 종류이다.
type TriggerType int

const (
	TriggerTimer TriggerType = iota // 시간 기반 (cron)
	TriggerSCM                      // SCM 변경 감지
)

// Trigger는 빌드를 시작하는 조건이다.
type Trigger struct {
	name        string
	triggerType TriggerType
	cronSpec    string
	shouldFire  func() bool // 트리거 발동 여부 판단 함수
}

func NewTimerTrigger(cronSpec string) *Trigger {
	return &Trigger{
		name:        "TimerTrigger",
		triggerType: TriggerTimer,
		cronSpec:    cronSpec,
		shouldFire:  func() bool { return true }, // 시간이 되면 항상 발동
	}
}

func NewSCMTrigger(cronSpec string, hasChanges func() bool) *Trigger {
	return &Trigger{
		name:        "SCMTrigger",
		triggerType: TriggerSCM,
		cronSpec:    cronSpec,
		shouldFire:  hasChanges,
	}
}

// Run은 트리거의 주기적 실행을 시뮬레이션한다.
// Jenkins: Trigger.run() — cron 매칭 시 호출됨
func (t *Trigger) Run() bool {
	return t.shouldFire()
}

// =============================================================================
// 11. FreestyleProject: Freestyle 프로젝트 정의
// =============================================================================
// Jenkins: hudson.model.FreeStyleProject extends Project
//   - getBuildWrappers(): BuildWrapper 목록
//   - getBuilders(): Builder 목록
//   - getPublishersList(): Publisher 목록
//   - getTriggers(): Trigger 맵

// FreestyleProject는 Jenkins Freestyle 프로젝트를 나타낸다.
type FreestyleProject struct {
	Name       string
	Triggers   []*Trigger
	Wrappers   []*BuildWrapper
	Builders   []BuildStep
	Publishers []*Publisher
	BuildCount int
}

func NewFreestyleProject(name string) *FreestyleProject {
	return &FreestyleProject{Name: name}
}

// =============================================================================
// 12. BuildExecution: 빌드 실행 오케스트레이터
// =============================================================================
// Jenkins: hudson.model.Build.BuildExecution extends AbstractRunner
//
// doRun(BuildListener listener):
//   1. preBuild(listener, project.getBuilders()) — Builder들의 prebuild
//   2. preBuild(listener, project.getPublishersList()) — Publisher들의 prebuild
//   3. for (BuildWrapper w : wrappers) { e = w.setUp(...); buildEnvironments.add(e); }
//   4. build(listener, project.getBuilders()) — Builder 순차 실행 (하나 실패 시 중단)
//
// post2(BuildListener listener):
//   5. performAllBuildSteps(listener, project.getPublishersList(), true)
//      — needsToRunAfterFinalized()==false인 Publisher 실행 (Recorder → Notifier 순)
//
// cleanUp(BuildListener listener):
//   6. performAllBuildSteps(listener, project.getPublishersList(), false)
//      — needsToRunAfterFinalized()==true인 Publisher 실행
//   7. tearDownBuildEnvironments() — Environment 역순 tearDown
//      for (int i = buildEnvironments.size() - 1; i >= 0; i--)
//          environment.tearDown(build, listener)

// BuildExecution은 빌드 실행을 오케스트레이션한다.
type BuildExecution struct {
	project      *FreestyleProject
	build        *Build
	environments []*WrapperEnvironment
}

func NewBuildExecution(project *FreestyleProject, build *Build) *BuildExecution {
	return &BuildExecution{
		project: project,
		build:   build,
	}
}

// Execute는 전체 빌드 파이프라인을 실행한다.
func (be *BuildExecution) Execute() BuildResult {
	b := be.build
	b.StartTime = time.Now()

	b.Listener.Log("========================================")
	b.Listener.Log("프로젝트 '%s' 빌드 #%d 시작", be.project.Name, b.Number)
	b.Listener.Log("========================================")

	// ── Phase 1: Prebuild ──────────────────────────────────────────
	// Jenkins: Build.BuildExecution.doRun()
	//   if (!preBuild(listener, project.getBuilders())) return FAILURE;
	//   if (!preBuild(listener, project.getPublishersList())) return FAILURE;
	b.Listener.Log("")
	b.Listener.Log("── Phase 1: Prebuild 검증 ──")
	if !be.preBuildAll() {
		b.SetResult(FAILURE)
		be.tearDownAll()
		be.finalize()
		return b.Result
	}

	// ── Phase 2: BuildWrapper.setUp ────────────────────────────────
	// Jenkins: for (BuildWrapper w : wrappers) {
	//     Environment e = w.setUp((AbstractBuild)build, launcher, listener);
	//     if (e == null) return FAILURE;
	//     buildEnvironments.add(e);
	// }
	b.Listener.Log("")
	b.Listener.Log("── Phase 2: BuildWrapper setUp ──")
	if !be.setUpWrappers() {
		b.SetResult(FAILURE)
		be.tearDownAll()
		be.finalize()
		return b.Result
	}

	// ── Phase 3: Builder 순차 실행 ──────────────────────────────────
	// Jenkins: Build.BuildExecution.build(listener, project.getBuilders())
	//   for (BuildStep bs : steps) {
	//     if (!perform(bs, listener)) return false;  // 하나 실패 시 즉시 중단
	//     if (executor.isInterrupted()) throw new InterruptedException();
	//   }
	b.Listener.Log("")
	b.Listener.Log("── Phase 3: Builder 실행 (순차, 실패 시 중단) ──")
	if !be.executeBuilders() {
		b.SetResult(FAILURE)
	}

	// Builder 완료 → MAIN_COMPLETED 체크포인트 보고
	// Jenkins: CheckPoint.MAIN_COMPLETED
	b.Listener.Log("")
	b.Listener.Log("  >>> CheckPoint.MAIN_COMPLETED 도달")

	// ── Phase 4: Publisher 실행 (post2) ─────────────────────────────
	// Jenkins: Build.BuildExecution.post2(listener)
	//   performAllBuildSteps(listener, project.getPublishersList(), true)
	//   — phase=true: needsToRunAfterFinalized() XOR true
	//   — 즉 needsToRunAfterFinalized()==false인 Publisher만 실행
	//
	// Publisher 실행 순서: Recorder(0) → 미분류(1) → Notifier(2)
	// Publisher는 하나 실패해도 나머지 계속 실행 (Builder와의 핵심 차이)
	b.Listener.Log("")
	b.Listener.Log("── Phase 4: Publisher 실행 (Recorder → Notifier, 실패해도 계속) ──")
	be.executePublishers(false) // needsToRunAfterFinalized == false

	// ── Phase 5: cleanUp (후처리 Publisher + tearDown) ──────────────
	// Jenkins: Build.BuildExecution.cleanUp(listener)
	//   performAllBuildSteps(listener, project.getPublishersList(), false)
	//   — phase=false: needsToRunAfterFinalized()==true인 Publisher 실행
	//   super.cleanUp(listener) → tearDownBuildEnvironments()
	b.Listener.Log("")
	b.Listener.Log("── Phase 5: CleanUp (후처리 Publisher + Environment tearDown) ──")
	be.executePublishers(true) // needsToRunAfterFinalized == true
	be.tearDownAll()

	// 완료
	be.finalize()
	return b.Result
}

// preBuildAll은 모든 BuildStep의 prebuild를 호출한다.
func (be *BuildExecution) preBuildAll() bool {
	// Builder prebuild
	for _, bs := range be.project.Builders {
		if !bs.Prebuild(be.build) {
			be.build.Listener.Log("  [Prebuild] %s: prebuild 실패 — 빌드 중단", bs.Name())
			return false
		}
	}
	// Publisher prebuild
	for _, pub := range be.project.Publishers {
		if !pub.Prebuild(be.build) {
			be.build.Listener.Log("  [Prebuild] %s: prebuild 실패 — 빌드 중단", pub.Name())
			return false
		}
	}
	return true
}

// setUpWrappers는 BuildWrapper.setUp을 순차 실행한다.
func (be *BuildExecution) setUpWrappers() bool {
	for _, w := range be.project.Wrappers {
		be.build.Listener.Log("  [Wrapper] %s: setUp 시작", w.name)
		env, err := w.setUp(be.build)
		if err != nil || env == nil {
			be.build.Listener.Log("  [Wrapper] %s: setUp 실패 — %v", w.name, err)
			return false
		}
		// 환경 변수 적용
		for k, v := range env.envVars {
			be.build.EnvVars[k] = v
			be.build.Listener.Log("  [Wrapper] %s: 환경변수 설정 %s=%s", w.name, k, v)
		}
		be.environments = append(be.environments, env)
		be.build.Listener.Log("  [Wrapper] %s: setUp 완료", w.name)
	}
	return true
}

// executeBuilders는 Builder를 순차 실행한다.
// Jenkins: Build.BuildExecution.build() — 하나 실패 시 즉시 중단
func (be *BuildExecution) executeBuilders() bool {
	for _, bs := range be.project.Builders {
		// BuildStepMonitor에 따른 동기화 적용
		mon := bs.GetRequiredMonitorService()
		be.build.Listener.Log("  [Monitor] %s: 동기화 수준 = %s", bs.Name(), mon)

		// Jenkins: AbstractRunner.perform(BuildStep, BuildListener)
		//   BuildStepMonitor mon = bs.getRequiredMonitorService();
		//   canContinue = mon.perform(bs, build, launcher, listener);
		if !be.performWithMonitor(bs, mon) {
			be.build.Listener.Log("  [Builder] %s: 실패 → 빌드 중단", bs.Name())
			return false
		}
	}
	return true
}

// executePublishers는 Publisher를 실행한다.
// Jenkins: AbstractRunner.performAllBuildSteps(listener, publishers, phase)
//   phase와 needsToRunAfterFinalized()의 XOR로 실행 대상 결정:
//   - phase=true, needsToRunAfterFinalized=false → 실행 (post2 단계)
//   - phase=false, needsToRunAfterFinalized=true → 실행 (cleanUp 단계)
//
// Publisher 정렬: Recorder(0) → 미분류(1) → Notifier(2)
func (be *BuildExecution) executePublishers(afterFinalized bool) {
	// Jenkins와 동일하게 Recorder → Notifier 순서로 정렬하여 실행
	// Publisher.DescriptorExtensionListImpl.sort()의 classify:
	//   Recorder → 0, 미분류 → 1, Notifier → 2
	var sorted []*Publisher

	// Recorder 먼저
	for _, pub := range be.project.Publishers {
		if pub.kind == KindRecorder && pub.NeedsToRunAfterFinalized() == afterFinalized {
			sorted = append(sorted, pub)
		}
	}
	// 그다음 Notifier
	for _, pub := range be.project.Publishers {
		if pub.kind == KindNotifier && pub.NeedsToRunAfterFinalized() == afterFinalized {
			sorted = append(sorted, pub)
		}
	}

	if len(sorted) == 0 {
		be.build.Listener.Log("  (해당 단계의 Publisher 없음)")
		return
	}

	allSuccess := true
	for _, pub := range sorted {
		mon := pub.GetRequiredMonitorService()
		be.build.Listener.Log("  [Monitor] %s: 동기화 수준 = %s", pub.Name(), mon)

		// Jenkins: performAllBuildSteps — 실패해도 계속 실행
		if !be.performWithMonitor(pub, mon) {
			allSuccess = false
			be.build.Listener.Log("  [Publisher] %s: 실패 → 결과 FAILURE (나머지 계속 실행)", pub.Name())
			be.build.SetResult(FAILURE)
		}
	}
	if !allSuccess {
		be.build.Listener.Log("  [Publisher] 일부 Publisher 실패 — 빌드 결과: %s", be.build.Result)
	}
}

// performWithMonitor는 BuildStepMonitor를 적용하여 BuildStep을 실행한다.
// Jenkins: BuildStepMonitor.perform(BuildStep, AbstractBuild, Launcher, BuildListener)
func (be *BuildExecution) performWithMonitor(bs BuildStep, mon MonitorLevel) bool {
	switch mon {
	case MonitorBuild:
		// Jenkins: BUILD.perform()
		//   CheckPoint.COMPLETED.block(listener, displayName);
		//   return bs.perform(build, launcher, listener);
		be.build.Listener.Log("    → [BUILD 동기화] 이전 빌드 완료 대기 (시뮬레이션)")
		return bs.Perform(be.build)

	case MonitorStep:
		// Jenkins: STEP.perform()
		//   CheckPoint cp = new CheckPoint(bs.getClass().getName(), bs.getClass());
		//   cp.block(listener, displayName);
		//   try { return bs.perform(...); }
		//   finally { cp.report(); }
		be.build.Listener.Log("    → [STEP 동기화] 이전 빌드의 같은 스텝 완료 대기 (시뮬레이션)")
		result := bs.Perform(be.build)
		be.build.Listener.Log("    → [STEP 동기화] 체크포인트 보고")
		return result

	case MonitorNone:
		// Jenkins: NONE.perform()
		//   return bs.perform(build, launcher, listener);
		return bs.Perform(be.build)

	default:
		return bs.Perform(be.build)
	}
}

// tearDownAll은 모든 WrapperEnvironment를 역순으로 tearDown한다.
// Jenkins: AbstractBuildExecution.tearDownBuildEnvironments()
//   for (int i = buildEnvironments.size() - 1; i >= 0; i--) {
//       environment.tearDown(build, listener);
//   }
func (be *BuildExecution) tearDownAll() {
	if len(be.environments) == 0 {
		return
	}
	be.build.Listener.Log("  [TearDown] BuildWrapper 환경 역순 정리 (LIFO)")
	// 역순 tearDown (Jenkins 동작과 동일)
	for i := len(be.environments) - 1; i >= 0; i-- {
		env := be.environments[i]
		be.build.Listener.Log("  [TearDown] %s: tearDown 시작", env.name)
		if env.tearFn != nil {
			if !env.tearFn(be.build) {
				be.build.Listener.Log("  [TearDown] %s: tearDown 실패", env.name)
				be.build.SetResult(FAILURE)
			} else {
				be.build.Listener.Log("  [TearDown] %s: tearDown 완료", env.name)
			}
		}
	}
}

func (be *BuildExecution) finalize() {
	be.build.EndTime = time.Now()
	duration := be.build.EndTime.Sub(be.build.StartTime)
	be.build.Listener.Log("")
	be.build.Listener.Log("========================================")
	be.build.Listener.Log("빌드 #%d 완료: %s (소요시간: %v)", be.build.Number, be.build.Result, duration.Round(time.Millisecond))
	be.build.Listener.Log("========================================")
}

// =============================================================================
// 13. CheckPointDemo: 동시 빌드에서의 CheckPoint 동기화 데모
// =============================================================================

func demoCheckPointBarrier() {
	fmt.Println("\n" + strings.Repeat("=", 70))
	fmt.Println("데모 2: CheckPoint Barrier 동기화 (동시 빌드)")
	fmt.Println(strings.Repeat("=", 70))
	fmt.Println()
	fmt.Println("시나리오: 빌드 #1과 #2가 동시 실행")
	fmt.Println("빌드 #2는 빌드 #1의 MAIN_COMPLETED 체크포인트를 대기한다.")
	fmt.Println()
	fmt.Println("Jenkins 소스 참조:")
	fmt.Println("  - hudson.model.CheckPoint.report() / block()")
	fmt.Println("  - hudson.tasks.BuildStepMonitor.STEP → cp.block(); bs.perform(); cp.report();")
	fmt.Println()

	cp := NewCheckPoint("MAIN_COMPLETED")
	var wg sync.WaitGroup
	wg.Add(2)

	// 빌드 #1: 작업 후 체크포인트 보고
	go func() {
		defer wg.Done()
		fmt.Println("[빌드 #1] Builder 실행 시작...")
		time.Sleep(200 * time.Millisecond)
		fmt.Println("[빌드 #1] Builder 완료 — CheckPoint.MAIN_COMPLETED.report()")
		cp.Report()
		fmt.Println("[빌드 #1] Publisher 실행 중...")
		time.Sleep(100 * time.Millisecond)
		fmt.Println("[빌드 #1] Publisher 완료")
	}()

	// 빌드 #2: 체크포인트 대기 후 실행
	go func() {
		defer wg.Done()
		fmt.Println("[빌드 #2] Builder 실행 시작...")
		time.Sleep(50 * time.Millisecond)
		fmt.Println("[빌드 #2] Builder 완료 — Publisher 전에 CheckPoint 대기...")
		fmt.Println("[빌드 #2] CheckPoint.MAIN_COMPLETED.block() — 빌드 #1 대기")
		cp.Block()
		fmt.Println("[빌드 #2] CheckPoint 통과 — Publisher 실행 시작")
		time.Sleep(100 * time.Millisecond)
		fmt.Println("[빌드 #2] Publisher 완료")
	}()

	wg.Wait()
	fmt.Println()
	fmt.Println("결과: 빌드 #2의 Publisher는 빌드 #1의 MAIN_COMPLETED 이후에 실행됨")
}

// =============================================================================
// 14. main: 전체 빌드 파이프라인 데모
// =============================================================================

func main() {
	fmt.Println(strings.Repeat("=", 70))
	fmt.Println("Jenkins 빌드 파이프라인 시뮬레이션")
	fmt.Println("(Freestyle 프로젝트의 빌드 단계 실행 순서)")
	fmt.Println(strings.Repeat("=", 70))
	fmt.Println()
	fmt.Println("실행 순서:")
	fmt.Println("  Trigger.run() → Queue.schedule()")
	fmt.Println("  → BuildWrapper.setUp()")
	fmt.Println("  → Builder.perform() (순차, 실패 시 중단)")
	fmt.Println("  → Publisher.perform() (순차, Recorder→Notifier, 실패해도 계속)")
	fmt.Println("  → BuildWrapper.tearDown() (역순)")
	fmt.Println()

	// ── Freestyle 프로젝트 구성 ──────────────────────────────────────

	project := NewFreestyleProject("my-java-app")

	// Trigger 설정
	// Jenkins: TimerTrigger — cron 표현식 기반
	// Jenkins: SCMTrigger — SCM 폴링 결과에 따라 빌드
	scmChanged := true
	project.Triggers = []*Trigger{
		NewTimerTrigger("H/15 * * * *"),
		NewSCMTrigger("H/5 * * * *", func() bool { return scmChanged }),
	}

	// BuildWrapper 설정
	// Jenkins: BuildWrapper — 빌드 환경 래핑 (setUp/tearDown)
	project.Wrappers = []*BuildWrapper{
		// 타임스탬프 래퍼: 빌드 로그에 타임스탬프 추가
		NewBuildWrapper("TimestamperBuildWrapper", func(build *Build) (*WrapperEnvironment, error) {
			return &WrapperEnvironment{
				name: "TimestamperBuildWrapper",
				envVars: map[string]string{
					"BUILD_TIMESTAMP": time.Now().Format("2006-01-02T15:04:05"),
				},
				tearFn: func(build *Build) bool {
					build.Listener.Log("    타임스탬프 래퍼 정리 완료")
					return true
				},
			}, nil
		}),
		// 크레덴셜 바인딩 래퍼: 자격증명을 환경변수에 주입
		NewBuildWrapper("CredentialBindingWrapper", func(build *Build) (*WrapperEnvironment, error) {
			return &WrapperEnvironment{
				name: "CredentialBindingWrapper",
				envVars: map[string]string{
					"DOCKER_USER": "deploy-bot",
					"DOCKER_PASS": "****",
				},
				tearFn: func(build *Build) bool {
					build.Listener.Log("    크레덴셜 바인딩 정리 (민감 데이터 삭제)")
					return true
				},
			}, nil
		}),
	}

	// Builder 설정 (순차 실행, 하나 실패 시 중단)
	project.Builders = []BuildStep{
		NewShellBuilder("Checkout", "git checkout main", 0.0),
		NewShellBuilder("Compile", "mvn compile -q", 0.0),
		NewShellBuilder("UnitTest", "mvn test -q", 0.0),
		NewShellBuilder("Package", "mvn package -DskipTests", 0.0),
	}

	// Publisher 설정 (Recorder → Notifier 순, 하나 실패해도 계속)
	project.Publishers = []*Publisher{
		// Recorder: 결과 기록 (빌드 결과 변경 가능)
		NewRecorder("JUnitResultArchiver", func(build *Build) bool {
			build.Listener.Log("    테스트 결과 수집: 42 tests, 0 failures")
			return true
		}),
		NewRecorder("JacocoPublisher", func(build *Build) bool {
			build.Listener.Log("    코드 커버리지: 78.5%% (임계값: 70%%)")
			return true
		}),
		NewRecorder("ArtifactArchiver", func(build *Build) bool {
			build.Listener.Log("    아티팩트 보관: target/my-java-app-1.0.jar")
			return true
		}),
		// Notifier: 외부 알림 (Recorder 이후 실행)
		NewNotifier("EmailNotifier", func(build *Build) bool {
			build.Listener.Log("    이메일 발송: 빌드 #%d 결과 = %s", build.Number, build.Result)
			return true
		}),
		NewNotifier("SlackNotifier", func(build *Build) bool {
			build.Listener.Log("    Slack 알림: #%s 빌드 #%d — %s",
				build.ProjectName, build.Number, build.Result)
			return true
		}),
	}

	// ── 데모 1: Trigger → 빌드 실행 ──────────────────────────────────

	fmt.Println(strings.Repeat("=", 70))
	fmt.Println("데모 1: Freestyle 빌드 파이프라인 전체 실행")
	fmt.Println(strings.Repeat("=", 70))
	fmt.Println()

	// Trigger 확인
	fmt.Println("── Trigger 확인 ──")
	triggered := false
	for _, t := range project.Triggers {
		fired := t.Run()
		fmt.Printf("  %s (spec: %s): %v\n", t.name, t.cronSpec, map[bool]string{true: "발동", false: "미발동"}[fired])
		if fired {
			triggered = true
		}
	}

	if !triggered {
		fmt.Println("  트리거 발동 없음 — 빌드 스킵")
		return
	}

	fmt.Println("  → 빌드 큐에 스케줄링")
	fmt.Println()

	// 빌드 실행
	project.BuildCount++
	build := NewBuild(project.BuildCount, project.Name)
	execution := NewBuildExecution(project, build)
	result := execution.Execute()

	// ── 데모 2: CheckPoint Barrier ──────────────────────────────────
	demoCheckPointBarrier()

	// ── 데모 3: Builder 실패 시나리오 ────────────────────────────────
	fmt.Println("\n" + strings.Repeat("=", 70))
	fmt.Println("데모 3: Builder 실패 시 파이프라인 동작")
	fmt.Println(strings.Repeat("=", 70))
	fmt.Println()
	fmt.Println("시나리오: 'Compile' 단계에서 실패")
	fmt.Println("기대: Builder 즉시 중단, Publisher는 모두 실행, tearDown 보장")
	fmt.Println()

	failProject := NewFreestyleProject("failing-build")
	failProject.Wrappers = []*BuildWrapper{
		NewBuildWrapper("EnvWrapper", func(build *Build) (*WrapperEnvironment, error) {
			return &WrapperEnvironment{
				name:    "EnvWrapper",
				envVars: map[string]string{"ENV": "test"},
				tearFn: func(build *Build) bool {
					build.Listener.Log("    EnvWrapper tearDown — 빌드 실패해도 반드시 실행됨!")
					return true
				},
			}, nil
		}),
	}
	failProject.Builders = []BuildStep{
		NewShellBuilder("Checkout", "git checkout main", 0.0),
		NewShellBuilder("Compile", "mvn compile -q", 1.0), // 100% 실패
		NewShellBuilder("Test", "mvn test -q", 0.0),        // 실행되지 않음
	}
	failProject.Publishers = []*Publisher{
		NewRecorder("JUnitArchiver", func(build *Build) bool {
			build.Listener.Log("    (실패 빌드에서도 Recorder 실행됨)")
			return true
		}),
		NewNotifier("EmailNotifier", func(build *Build) bool {
			build.Listener.Log("    실패 알림 발송: 빌드 #%d FAILURE", build.Number)
			return true
		}),
	}

	failProject.BuildCount++
	failBuild := NewBuild(failProject.BuildCount, failProject.Name)
	failExecution := NewBuildExecution(failProject, failBuild)
	failExecution.Execute()

	// ── 데모 4: Publisher 정렬 순서 확인 ─────────────────────────────
	fmt.Println("\n" + strings.Repeat("=", 70))
	fmt.Println("데모 4: Publisher 정렬 순서 (Recorder → Notifier)")
	fmt.Println(strings.Repeat("=", 70))
	fmt.Println()
	fmt.Println("Jenkins 소스 참조:")
	fmt.Println("  - Publisher.ExtensionComponentComparator.classify():")
	fmt.Println("    Recorder → 0, 미분류 → 1, Notifier → 2")
	fmt.Println("  - 이 순서로 정렬하여 Recorder가 먼저 결과를 확정한 후 Notifier가 알림")
	fmt.Println()

	orderProject := NewFreestyleProject("publisher-order-demo")
	orderProject.Builders = []BuildStep{
		NewShellBuilder("Build", "echo ok", 0.0),
	}
	// 의도적으로 Notifier를 먼저 추가하여 정렬이 올바르게 동작하는지 확인
	orderProject.Publishers = []*Publisher{
		NewNotifier("Notifier-A (Slack)", func(build *Build) bool {
			build.Listener.Log("    Slack: 빌드 결과 = %s", build.Result)
			return true
		}),
		NewRecorder("Recorder-A (테스트 결과)", func(build *Build) bool {
			// Recorder가 빌드를 UNSTABLE로 변경
			build.Listener.Log("    테스트 결과 분석: 3 failures → UNSTABLE로 변경")
			build.SetResult(UNSTABLE)
			return true
		}),
		NewNotifier("Notifier-B (이메일)", func(build *Build) bool {
			build.Listener.Log("    이메일: 빌드 결과 = %s", build.Result)
			return true
		}),
		NewRecorder("Recorder-B (커버리지)", func(build *Build) bool {
			build.Listener.Log("    커버리지: 65%% (임계값 미달)")
			return true
		}),
	}

	orderProject.BuildCount++
	orderBuild := NewBuild(orderProject.BuildCount, orderProject.Name)
	orderExecution := NewBuildExecution(orderProject, orderBuild)
	orderExecution.Execute()

	fmt.Println()
	fmt.Println("확인: Recorder가 Notifier보다 먼저 실행되어 결과를 UNSTABLE로 변경한 뒤,")
	fmt.Println("      Notifier가 확정된 결과(UNSTABLE)를 알림")

	// ── 전체 요약 ─────────────────────────────────────────────────────
	fmt.Println("\n" + strings.Repeat("=", 70))
	fmt.Println("요약: Jenkins Freestyle 빌드 파이프라인 핵심 설계")
	fmt.Println(strings.Repeat("=", 70))
	fmt.Println()
	fmt.Println("1. 실행 순서:")
	fmt.Println("   Trigger.run() → Queue.schedule()")
	fmt.Println("   → preBuild(Builders) → preBuild(Publishers)")
	fmt.Println("   → BuildWrapper.setUp() (순차)")
	fmt.Println("   → Builder.perform() (순차, 실패 시 즉시 중단)")
	fmt.Println("   → Publisher.perform() (Recorder→Notifier 순, 실패해도 계속)")
	fmt.Println("   → BuildWrapper.tearDown() (역순, 실패해도 반드시 실행)")
	fmt.Println()
	fmt.Println("2. 핵심 차이점:")
	fmt.Println("   - Builder: 하나 실패 → 나머지 Builder 스킵")
	fmt.Println("   - Publisher: 하나 실패 → 나머지 Publisher 계속 실행")
	fmt.Println("   - tearDown: 빌드 실패해도 반드시 실행 (리소스 정리 보장)")
	fmt.Println()
	fmt.Println("3. BuildStepMonitor 동시성 제어:")
	fmt.Println("   - NONE: 독립 실행 (Builder 기본값, 권장)")
	fmt.Println("   - STEP: 이전 빌드의 같은 스텝 완료 대기 (CheckPoint 기반)")
	fmt.Println("   - BUILD: 이전 빌드 전체 완료 대기 (Publisher/레거시 기본값)")
	fmt.Println()
	fmt.Println("4. Publisher 정렬:")
	fmt.Println("   Recorder(0) → 미분류(1) → Notifier(2)")
	fmt.Println("   → Recorder가 먼저 결과 확정 → Notifier가 확정된 결과로 알림")
	fmt.Println()

	_ = result
}
