// poc-01-architecture: Jenkins WAR 디스패처 + 초기화 시퀀스 시뮬레이션
//
// Jenkins의 핵심 아키텍처 패턴을 Go로 재현한다:
// 1. 단일 WAR 구조: 하나의 바이너리에 웹 서버, 플러그인 관리, 빌드 엔진 등 여러 컴포넌트 포함
// 2. ServletContextListener → 초기화 스레드: contextInitialized()에서 별도 goroutine으로 부팅
// 3. InitMilestone 순서: DAG 의존성에 따른 위상정렬로 초기화 단계 실행
// 4. Reactor 패턴: 작업(Task)에 requires/attains 관계를 선언하고 위상정렬로 실행
//
// 실제 Jenkins 소스 참조:
//   - core/src/main/java/hudson/init/InitMilestone.java
//     → enum InitMilestone { STARTED, PLUGINS_LISTED, ..., COMPLETED }
//     → ordering() 메서드: TaskGraphBuilder로 마일스톤 간 순서 강제
//   - core/src/main/java/jenkins/model/Jenkins.java
//     → Jenkins 클래스: AbstractCIBase 확장, 싱글톤 인스턴스
//   - Reactor: org.jvnet.hudson.reactor.Reactor (외부 라이브러리)
//     → DAG 기반 작업 실행 엔진
//
// 실행: go run main.go

package main

import (
	"fmt"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// InitMilestone: Jenkins 초기화 마일스톤
// 실제: core/src/main/java/hudson/init/InitMilestone.java
// enum InitMilestone implements Milestone {
//     STARTED, PLUGINS_LISTED, PLUGINS_PREPARED, PLUGINS_STARTED,
//     EXTENSIONS_AUGMENTED, SYSTEM_CONFIG_LOADED, SYSTEM_CONFIG_ADAPTED,
//     JOB_LOADED, JOB_CONFIG_ADAPTED, COMPLETED
// }
// =============================================================================

type InitMilestone int

const (
	STARTED              InitMilestone = iota // 초기화 시작 - 아무 작업 없이 달성되는 첫 마일스톤
	PLUGINS_LISTED                            // 모든 플러그인 메타데이터 검사, 의존성 파악 완료
	PLUGINS_PREPARED                          // 모든 플러그인 메타데이터 로드, classloader 설정 완료
	PLUGINS_STARTED                           // 모든 플러그인 실행 시작, 확장점 로드, 디스크립터 인스턴스화
	EXTENSIONS_AUGMENTED                      // 프로그래밍적으로 구성된 확장점 구현체 추가 완료
	SYSTEM_CONFIG_LOADED                      // 파일 시스템에서 모든 시스템 설정 로드 완료
	SYSTEM_CONFIG_ADAPTED                     // 시스템 설정 적응 완료 (CasC 등)
	JOB_LOADED                                // 모든 Job과 빌드 기록이 디스크에서 로드됨
	JOB_CONFIG_ADAPTED                        // Job 설정 적응/업데이트 완료
	COMPLETED                                 // 최종 마일스톤 - 모든 실행 완료
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
	if name, ok := milestoneNames[m]; ok {
		return name
	}
	return fmt.Sprintf("UNKNOWN(%d)", m)
}

func (m InitMilestone) Message() string {
	if msg, ok := milestoneMessages[m]; ok {
		return msg
	}
	return "Unknown milestone"
}

// =============================================================================
// Reactor: DAG 기반 작업 실행 엔진
// 실제: org.jvnet.hudson.reactor.Reactor (별도 라이브러리)
// Jenkins는 Reactor를 사용해 초기화 작업의 의존성을 DAG로 관리하고
// 위상정렬(topological sort)로 올바른 순서로 실행한다.
//
// 핵심 인터페이스:
//   Milestone: 달성해야 할 이정표
//   Task: requires(Milestone), attains(Milestone), run()
//   TaskGraphBuilder: 작업 그래프를 선언적으로 구성
// =============================================================================

// ReactorTask: Reactor에서 실행하는 개별 작업
// 실제: org.jvnet.hudson.reactor.TaskGraphBuilder.TaskDef
type ReactorTask struct {
	Name     string        // 작업 이름
	Requires InitMilestone // 이 마일스톤 달성 후에 실행 가능
	Attains  InitMilestone // 이 작업 완료 시 달성되는 마일스톤
	Execute  func() error  // 실제 실행 로직
}

// Reactor: 작업 의존성 그래프를 관리하고 위상정렬로 실행
type Reactor struct {
	tasks     []*ReactorTask
	achieved  map[InitMilestone]bool
	mu        sync.Mutex
	listeners []ReactorListener
}

// ReactorListener: 마일스톤 달성 이벤트 수신
type ReactorListener interface {
	OnMilestoneAttained(milestone InitMilestone)
	OnTaskStarted(task *ReactorTask)
	OnTaskCompleted(task *ReactorTask, err error)
}

func NewReactor() *Reactor {
	return &Reactor{
		tasks:    make([]*ReactorTask, 0),
		achieved: make(map[InitMilestone]bool),
	}
}

func (r *Reactor) AddListener(l ReactorListener) {
	r.listeners = append(r.listeners, l)
}

func (r *Reactor) AddTask(task *ReactorTask) {
	r.tasks = append(r.tasks, task)
}

// AddOrderingTasks: InitMilestone.ordering() 시뮬레이션
// 실제: InitMilestone.java의 ordering() 메서드
//   public static TaskBuilder ordering() {
//       TaskGraphBuilder b = new TaskGraphBuilder();
//       InitMilestone[] v = values();
//       for (int i = 0; i < v.length - 1; i++)
//           b.add(null, Executable.NOOP).requires(v[i]).attains(v[i + 1]);
//       return b;
//   }
// 연속된 마일스톤 사이에 NOOP 작업을 삽입하여 순서를 강제한다.
func (r *Reactor) AddOrderingTasks() {
	milestones := []InitMilestone{
		STARTED, PLUGINS_LISTED, PLUGINS_PREPARED, PLUGINS_STARTED,
		EXTENSIONS_AUGMENTED, SYSTEM_CONFIG_LOADED, SYSTEM_CONFIG_ADAPTED,
		JOB_LOADED, JOB_CONFIG_ADAPTED, COMPLETED,
	}
	for i := 0; i < len(milestones)-1; i++ {
		from := milestones[i]
		to := milestones[i+1]
		r.AddTask(&ReactorTask{
			Name:     fmt.Sprintf("ordering: %s → %s", from, to),
			Requires: from,
			Attains:  to,
			Execute:  func() error { return nil }, // NOOP - 순서 강제용
		})
	}
}

// Execute: 위상정렬 기반 작업 실행
// 실제 Reactor는 멀티스레드로 병렬 실행하지만, 여기서는 순차 실행으로 단순화
func (r *Reactor) Execute() error {
	// STARTED는 무조건 달성
	r.achieve(STARTED)

	// 위상정렬: requires가 이미 달성된 작업부터 실행
	executed := make(map[*ReactorTask]bool)
	maxIterations := len(r.tasks) * 2 // 무한 루프 방지

	for iteration := 0; iteration < maxIterations; iteration++ {
		progress := false

		// 실행 가능한 작업을 requires 순서로 정렬하여 실행
		runnable := r.findRunnableTasks(executed)
		if len(runnable) == 0 {
			break
		}

		// requires 값이 작은 것(=더 앞선 마일스톤)부터 실행
		sort.Slice(runnable, func(i, j int) bool {
			return runnable[i].Requires < runnable[j].Requires
		})

		for _, task := range runnable {
			if executed[task] {
				continue
			}

			// 리스너 알림: 작업 시작
			for _, l := range r.listeners {
				l.OnTaskStarted(task)
			}

			// 작업 실행
			err := task.Execute()
			executed[task] = true
			progress = true

			// 리스너 알림: 작업 완료
			for _, l := range r.listeners {
				l.OnTaskCompleted(task, err)
			}

			if err != nil {
				return fmt.Errorf("작업 '%s' 실행 실패: %w", task.Name, err)
			}

			// 마일스톤 달성
			r.achieve(task.Attains)
		}

		if !progress {
			return fmt.Errorf("순환 의존성 감지: 더 이상 실행 가능한 작업 없음")
		}
	}

	// 미실행 작업 확인
	for _, task := range r.tasks {
		if !executed[task] {
			return fmt.Errorf("작업 '%s' 실행 불가: requires=%s 미달성", task.Name, task.Requires)
		}
	}

	return nil
}

func (r *Reactor) findRunnableTasks(executed map[*ReactorTask]bool) []*ReactorTask {
	var result []*ReactorTask
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, task := range r.tasks {
		if !executed[task] && r.achieved[task.Requires] {
			result = append(result, task)
		}
	}
	return result
}

func (r *Reactor) achieve(milestone InitMilestone) {
	r.mu.Lock()
	alreadyAchieved := r.achieved[milestone]
	r.achieved[milestone] = true
	r.mu.Unlock()

	if !alreadyAchieved {
		for _, l := range r.listeners {
			l.OnMilestoneAttained(milestone)
		}
	}
}

func (r *Reactor) IsAchieved(milestone InitMilestone) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.achieved[milestone]
}

// =============================================================================
// InitReactorRunner: 초기화 리액터 실행자
// 실제: core/src/main/java/hudson/init/InitReactorRunner.java
// Jenkins 부팅 시 리액터를 실행하면서 진행 상황을 표시한다.
// =============================================================================

type InitReactorRunner struct {
	startTime time.Time
}

func (irr *InitReactorRunner) OnMilestoneAttained(milestone InitMilestone) {
	elapsed := time.Since(irr.startTime)
	fmt.Printf("  [%6.1fs] ★ 마일스톤 달성: %-25s (%s)\n",
		elapsed.Seconds(), milestone, milestone.Message())
}

func (irr *InitReactorRunner) OnTaskStarted(task *ReactorTask) {
	if strings.HasPrefix(task.Name, "ordering:") {
		return // ordering 작업은 로그 생략
	}
	elapsed := time.Since(irr.startTime)
	fmt.Printf("  [%6.1fs]   ▶ 작업 시작: %s\n", elapsed.Seconds(), task.Name)
}

func (irr *InitReactorRunner) OnTaskCompleted(task *ReactorTask, err error) {
	if strings.HasPrefix(task.Name, "ordering:") {
		return
	}
	elapsed := time.Since(irr.startTime)
	if err != nil {
		fmt.Printf("  [%6.1fs]   ✗ 작업 실패: %s - %v\n", elapsed.Seconds(), task.Name, err)
	} else {
		fmt.Printf("  [%6.1fs]   ✓ 작업 완료: %s\n", elapsed.Seconds(), task.Name)
	}
}

// =============================================================================
// PluginManager: 플러그인 관리자
// 실제: core/src/main/java/hudson/PluginManager.java
// - 플러그인 디렉토리 스캔
// - 의존성 해석 (위상정렬)
// - classloader 계층 구성
// - 플러그인 시작
// =============================================================================

type PluginWrapper struct {
	ShortName    string   // 플러그인 짧은 이름
	Version      string   // 버전
	Dependencies []string // 의존하는 플러그인 이름
	Active       bool     // 활성화 여부
	Loaded       bool     // 로드 완료 여부
	Started      bool     // 시작 완료 여부
}

type PluginManager struct {
	plugins []*PluginWrapper
	mu      sync.RWMutex
}

func NewPluginManager() *PluginManager {
	return &PluginManager{
		plugins: make([]*PluginWrapper, 0),
	}
}

// discoverPlugins: 플러그인 디렉토리 스캔 시뮬레이션
// 실제: PluginManager.loadDetachedPlugins(), loadBundledPlugins()
func (pm *PluginManager) discoverPlugins() {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	// 시뮬레이션: 가상의 플러그인 목록
	pm.plugins = []*PluginWrapper{
		{ShortName: "credentials", Version: "2.6.1", Dependencies: nil, Active: true},
		{ShortName: "ssh-credentials", Version: "1.19", Dependencies: []string{"credentials"}, Active: true},
		{ShortName: "git-client", Version: "3.12.1", Dependencies: []string{"credentials", "ssh-credentials"}, Active: true},
		{ShortName: "git", Version: "4.14.3", Dependencies: []string{"git-client", "credentials"}, Active: true},
		{ShortName: "workflow-step-api", Version: "2.24", Dependencies: nil, Active: true},
		{ShortName: "workflow-api", Version: "2.47", Dependencies: []string{"workflow-step-api"}, Active: true},
		{ShortName: "pipeline-model-definition", Version: "2.2114.3", Dependencies: []string{"workflow-api", "workflow-step-api", "credentials"}, Active: true},
		{ShortName: "matrix-auth", Version: "3.1.5", Dependencies: nil, Active: true},
		{ShortName: "junit", Version: "1.62", Dependencies: nil, Active: true},
		{ShortName: "mailer", Version: "1.34", Dependencies: nil, Active: true},
	}
}

// resolvePluginDependencies: 플러그인 의존성을 위상정렬하여 로드 순서 결정
// 실제: PluginManager에서 CyclicGraphDetector를 사용하여 순환 의존성 탐지
func (pm *PluginManager) resolvePluginDependencies() ([]*PluginWrapper, error) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	// 이름 → 플러그인 매핑
	byName := make(map[string]*PluginWrapper)
	for _, p := range pm.plugins {
		byName[p.ShortName] = p
	}

	// 위상정렬 (Kahn's algorithm)
	inDegree := make(map[string]int)
	for _, p := range pm.plugins {
		if _, exists := inDegree[p.ShortName]; !exists {
			inDegree[p.ShortName] = 0
		}
		for _, dep := range p.Dependencies {
			if _, exists := byName[dep]; exists {
				inDegree[p.ShortName]++
			}
		}
	}

	// 진입 차수가 0인 노드부터 시작
	var queue []string
	for name, degree := range inDegree {
		if degree == 0 {
			queue = append(queue, name)
		}
	}
	sort.Strings(queue) // 결정적 순서를 위해 정렬

	var sorted []*PluginWrapper
	visited := make(map[string]bool)

	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]

		if visited[name] {
			continue
		}
		visited[name] = true

		p := byName[name]
		sorted = append(sorted, p)

		// 이 플러그인에 의존하는 다른 플러그인의 진입 차수 감소
		for _, other := range pm.plugins {
			for _, dep := range other.Dependencies {
				if dep == name {
					inDegree[other.ShortName]--
					if inDegree[other.ShortName] == 0 {
						queue = append(queue, other.ShortName)
						sort.Strings(queue) // 결정적 순서 유지
					}
				}
			}
		}
	}

	// 순환 의존성 확인
	if len(sorted) != len(pm.plugins) {
		return nil, fmt.Errorf("순환 의존성 감지: %d개 플러그인 중 %d개만 정렬됨",
			len(pm.plugins), len(sorted))
	}

	return sorted, nil
}

func (pm *PluginManager) preparePlugins(sorted []*PluginWrapper) {
	for _, p := range sorted {
		p.Loaded = true
		simulateWork(5, 15) // classloader 설정 시뮬레이션
	}
}

func (pm *PluginManager) startPlugins(sorted []*PluginWrapper) {
	for _, p := range sorted {
		if p.Active && p.Loaded {
			p.Started = true
			simulateWork(3, 10)
		}
	}
}

func (pm *PluginManager) GetPlugins() []*PluginWrapper {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	result := make([]*PluginWrapper, len(pm.plugins))
	copy(result, pm.plugins)
	return result
}

// =============================================================================
// ExtensionList: 확장점 레지스트리
// 실제: core/src/main/java/hudson/ExtensionList.java
// Jenkins의 확장점(Extension Point) 시스템 — 플러그인이 기능을 추가하는 메커니즘.
// @Extension 어노테이션이 붙은 클래스를 자동 발견하여 등록한다.
// =============================================================================

type Extension struct {
	Name           string
	ExtensionPoint string // 구현하는 확장점 이름
	PluginName     string // 제공하는 플러그인
	Ordinal        int    // 우선순위 (낮을수록 높은 우선순위)
}

type ExtensionRegistry struct {
	extensions map[string][]*Extension // extensionPoint → []Extension
	mu         sync.RWMutex
}

func NewExtensionRegistry() *ExtensionRegistry {
	return &ExtensionRegistry{
		extensions: make(map[string][]*Extension),
	}
}

func (er *ExtensionRegistry) Register(ext *Extension) {
	er.mu.Lock()
	defer er.mu.Unlock()
	er.extensions[ext.ExtensionPoint] = append(er.extensions[ext.ExtensionPoint], ext)
}

func (er *ExtensionRegistry) GetExtensions(extensionPoint string) []*Extension {
	er.mu.RLock()
	defer er.mu.RUnlock()
	result := make([]*Extension, len(er.extensions[extensionPoint]))
	copy(result, er.extensions[extensionPoint])

	// ordinal로 정렬
	sort.Slice(result, func(i, j int) bool {
		return result[i].Ordinal < result[j].Ordinal
	})
	return result
}

func (er *ExtensionRegistry) AllExtensionPoints() []string {
	er.mu.RLock()
	defer er.mu.RUnlock()
	var points []string
	for ep := range er.extensions {
		points = append(points, ep)
	}
	sort.Strings(points)
	return points
}

func (er *ExtensionRegistry) TotalCount() int {
	er.mu.RLock()
	defer er.mu.RUnlock()
	count := 0
	for _, exts := range er.extensions {
		count += len(exts)
	}
	return count
}

// =============================================================================
// Servlet 시뮬레이션: Jenkins의 WAR 기반 웹 구조
// 실제: Jenkins는 Winstone/Jetty 내장 서블릿 컨테이너로 실행
// - WebAppMain implements ServletContextListener
// - contextInitialized() → initThread 시작 → Jenkins 싱글톤 생성
// =============================================================================

// ServletContext: 서블릿 컨텍스트 시뮬레이션
type ServletContext struct {
	Attributes map[string]interface{}
	InitParams map[string]string
	mu         sync.RWMutex
}

func NewServletContext() *ServletContext {
	return &ServletContext{
		Attributes: make(map[string]interface{}),
		InitParams: make(map[string]string),
	}
}

func (sc *ServletContext) SetAttribute(name string, value interface{}) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.Attributes[name] = value
}

func (sc *ServletContext) GetAttribute(name string) interface{} {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	return sc.Attributes[name]
}

// StaplerDispatcher: Stapler 프레임워크 디스패처 시뮬레이션
// 실제: Stapler 프레임워크 — URL을 Java 객체 트리에 매핑
// URL /job/my-project/42/console → Jenkins.getItem("my-project").getBuild(42).doConsole()
type StaplerDispatcher struct {
	jenkins *Jenkins
	mux     *http.ServeMux
}

func NewStaplerDispatcher(jenkins *Jenkins) *StaplerDispatcher {
	sd := &StaplerDispatcher{
		jenkins: jenkins,
		mux:     http.NewServeMux(),
	}
	sd.registerRoutes()
	return sd
}

func (sd *StaplerDispatcher) registerRoutes() {
	// Stapler는 URL 경로를 객체 트리로 매핑
	// / → Jenkins 인스턴스
	// /api/json → Jenkins.doApi()
	// /job/{name} → Jenkins.getItem(name)
	// /computer/{name} → Jenkins.getComputer(name)

	sd.mux.HandleFunc("/", sd.handleRoot)
	sd.mux.HandleFunc("/api/json", sd.handleAPI)
	sd.mux.HandleFunc("/manage", sd.handleManage)
	sd.mux.HandleFunc("/job/", sd.handleJob)
	sd.mux.HandleFunc("/computer/", sd.handleComputer)
	sd.mux.HandleFunc("/queue/api/json", sd.handleQueueAPI)
	sd.mux.HandleFunc("/crumbIssuer/api/json", sd.handleCrumb)
}

func (sd *StaplerDispatcher) handleRoot(w http.ResponseWriter, r *http.Request) {
	if !sd.jenkins.isReady() {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, "Jenkins가 아직 초기화 중입니다... (현재: %s)\n",
			sd.jenkins.initLevel.Message())
		return
	}
	fmt.Fprintf(w, "Jenkins %s\n상태: 실행 중\n플러그인: %d개 활성\n",
		sd.jenkins.version, len(sd.jenkins.pluginManager.GetPlugins()))
}

func (sd *StaplerDispatcher) handleAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"mode":"NORMAL","nodeDescription":"Built-In Node","numExecutors":%d,"version":"%s"}`,
		sd.jenkins.numExecutors, sd.jenkins.version)
}

func (sd *StaplerDispatcher) handleManage(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "Jenkins 관리 페이지\n- 시스템 설정\n- 플러그인 관리\n- 노드 관리\n")
}

func (sd *StaplerDispatcher) handleJob(w http.ResponseWriter, r *http.Request) {
	jobName := strings.TrimPrefix(r.URL.Path, "/job/")
	jobName = strings.TrimSuffix(jobName, "/")
	fmt.Fprintf(w, "Job: %s\n상태: 대기 중\n", jobName)
}

func (sd *StaplerDispatcher) handleComputer(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "컴퓨터(노드) 목록\n- Built-In Node (executors: %d)\n",
		sd.jenkins.numExecutors)
}

func (sd *StaplerDispatcher) handleQueueAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"items":[]}`)
}

func (sd *StaplerDispatcher) handleCrumb(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"crumb":"simulated-crumb","crumbRequestField":"Jenkins-Crumb"}`)
}

// =============================================================================
// Jenkins: 메인 싱글톤 클래스
// 실제: core/src/main/java/jenkins/model/Jenkins.java
// public class Jenkins extends AbstractCIBase implements
//     DirectlyModifiableTopLevelItemGroup, StaplerProxy, ...
//
// Jenkins 인스턴스는 전체 시스템의 루트 객체:
// - 모든 Job, Node, Plugin을 관리
// - Queue(빌드 큐) 소유
// - 초기화 시 Reactor를 통해 순차적으로 부팅
// =============================================================================

type Jenkins struct {
	version        string
	jenkinsHome    string
	numExecutors   int
	initLevel      InitMilestone
	pluginManager  *PluginManager
	extensionReg   *ExtensionRegistry
	servletContext *ServletContext
	dispatcher     *StaplerDispatcher
	reactor        *Reactor
	bootStartTime  time.Time
	mu             sync.RWMutex
}

func NewJenkins(home string) *Jenkins {
	j := &Jenkins{
		version:       "2.450",
		jenkinsHome:   home,
		numExecutors:  2,
		initLevel:     STARTED,
		pluginManager: NewPluginManager(),
		extensionReg:  NewExtensionRegistry(),
		reactor:       NewReactor(),
	}
	j.servletContext = NewServletContext()
	j.servletContext.SetAttribute("app", j)
	return j
}

func (j *Jenkins) isReady() bool {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.initLevel == COMPLETED
}

func (j *Jenkins) setInitLevel(level InitMilestone) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.initLevel = level
}

func (j *Jenkins) getInitLevel() InitMilestone {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.initLevel
}

// =============================================================================
// WebAppMain: ServletContextListener 구현
// 실제: core/src/main/java/hudson/WebAppMain.java
// public class WebAppMain implements ServletContextListener {
//     public void contextInitialized(ServletContextEvent event) {
//         ...
//         initThread = new Thread("Jenkins initialization thread") {
//             public void run() {
//                 // Jenkins 싱글톤 생성 및 초기화
//             }
//         };
//         initThread.start();
//     }
// }
// =============================================================================

type WebAppMain struct {
	jenkins  *Jenkins
	initDone chan struct{}
}

func NewWebAppMain(jenkinsHome string) *WebAppMain {
	return &WebAppMain{
		initDone: make(chan struct{}),
	}
}

// contextInitialized: 서블릿 컨텍스트 초기화 시 호출
// 실제: WebAppMain.contextInitialized(ServletContextEvent)
func (wam *WebAppMain) contextInitialized(jenkinsHome string) {
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("WebAppMain.contextInitialized() 호출")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	// Jenkins 인스턴스 생성
	wam.jenkins = NewJenkins(jenkinsHome)
	wam.jenkins.bootStartTime = time.Now()

	fmt.Printf("  JENKINS_HOME: %s\n", jenkinsHome)
	fmt.Printf("  Jenkins 버전: %s\n", wam.jenkins.version)
	fmt.Println()

	// 초기화 스레드 시작 (실제 Jenkins에서는 별도 Thread)
	// 실제: new Thread("Jenkins initialization thread") { ... }.start()
	fmt.Println("초기화 스레드 시작 (goroutine)...")
	go wam.initializationThread()
}

func (wam *WebAppMain) initializationThread() {
	defer close(wam.initDone)

	j := wam.jenkins
	reactor := j.reactor
	runner := &InitReactorRunner{startTime: j.bootStartTime}
	reactor.AddListener(runner)

	// 마일스톤 순서 강제를 위한 ordering 작업 추가
	// 실제: InitMilestone.ordering()
	reactor.AddOrderingTasks()

	// 초기화 작업 등록 (실제 Jenkins에서는 @Initializer 어노테이션으로 등록)
	wam.registerInitializationTasks(reactor)

	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("Reactor 실행 시작")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	err := reactor.Execute()
	if err != nil {
		fmt.Printf("\n[ERROR] 초기화 실패: %v\n", err)
		return
	}

	j.setInitLevel(COMPLETED)

	elapsed := time.Since(j.bootStartTime)
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Printf("Jenkins 초기화 완료! (소요 시간: %.2f초)\n", elapsed.Seconds())
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
}

// registerInitializationTasks: @Initializer 어노테이션이 붙은 메서드들을 등록
// 실제 Jenkins에서는 리플렉션으로 @Initializer를 스캔하여 자동 등록
func (wam *WebAppMain) registerInitializationTasks(reactor *Reactor) {
	j := wam.jenkins

	// ─────── PLUGINS_LISTED 단계 ───────
	// 플러그인 디렉토리 스캔
	reactor.AddTask(&ReactorTask{
		Name:     "플러그인 디렉토리 스캔",
		Requires: STARTED,
		Attains:  PLUGINS_LISTED,
		Execute: func() error {
			j.pluginManager.discoverPlugins()
			plugins := j.pluginManager.GetPlugins()
			fmt.Printf("           → %d개 플러그인 발견\n", len(plugins))
			for _, p := range plugins {
				deps := "없음"
				if len(p.Dependencies) > 0 {
					deps = strings.Join(p.Dependencies, ", ")
				}
				fmt.Printf("             ├─ %s (%s) [의존성: %s]\n", p.ShortName, p.Version, deps)
			}
			return nil
		},
	})

	// ─────── PLUGINS_PREPARED 단계 ───────
	// 플러그인 의존성 해석 및 classloader 설정
	reactor.AddTask(&ReactorTask{
		Name:     "플러그인 의존성 해석 (위상정렬)",
		Requires: PLUGINS_LISTED,
		Attains:  PLUGINS_PREPARED,
		Execute: func() error {
			sorted, err := j.pluginManager.resolvePluginDependencies()
			if err != nil {
				return err
			}
			fmt.Printf("           → 위상정렬 결과 (로드 순서):\n")
			for i, p := range sorted {
				fmt.Printf("             %2d. %s\n", i+1, p.ShortName)
			}
			j.pluginManager.preparePlugins(sorted)
			fmt.Printf("           → %d개 플러그인 classloader 설정 완료\n", len(sorted))
			return nil
		},
	})

	// ─────── PLUGINS_STARTED 단계 ───────
	// 플러그인 시작
	reactor.AddTask(&ReactorTask{
		Name:     "플러그인 시작 및 확장점 로드",
		Requires: PLUGINS_PREPARED,
		Attains:  PLUGINS_STARTED,
		Execute: func() error {
			sorted, _ := j.pluginManager.resolvePluginDependencies()
			j.pluginManager.startPlugins(sorted)

			startedCount := 0
			for _, p := range j.pluginManager.GetPlugins() {
				if p.Started {
					startedCount++
				}
			}
			fmt.Printf("           → %d개 플러그인 시작됨\n", startedCount)
			return nil
		},
	})

	// ─────── EXTENSIONS_AUGMENTED 단계 ───────
	// 확장점 등록
	reactor.AddTask(&ReactorTask{
		Name:     "확장점(Extension Point) 등록",
		Requires: PLUGINS_STARTED,
		Attains:  EXTENSIONS_AUGMENTED,
		Execute: func() error {
			wam.registerExtensions()
			points := j.extensionReg.AllExtensionPoints()
			fmt.Printf("           → %d개 확장점, 총 %d개 구현체 등록\n",
				len(points), j.extensionReg.TotalCount())
			for _, ep := range points {
				exts := j.extensionReg.GetExtensions(ep)
				fmt.Printf("             ├─ %s (%d개)\n", ep, len(exts))
				for _, ext := range exts {
					fmt.Printf("             │  └─ %s (from: %s)\n", ext.Name, ext.PluginName)
				}
			}
			return nil
		},
	})

	// ─────── SYSTEM_CONFIG_LOADED 단계 ───────
	reactor.AddTask(&ReactorTask{
		Name:     "시스템 설정 로드 (config.xml)",
		Requires: EXTENSIONS_AUGMENTED,
		Attains:  SYSTEM_CONFIG_LOADED,
		Execute: func() error {
			simulateWork(10, 30)
			fmt.Printf("           → JENKINS_HOME/config.xml 로드 완료\n")
			fmt.Printf("           → numExecutors=%d, mode=NORMAL\n", j.numExecutors)
			return nil
		},
	})

	// ─────── SYSTEM_CONFIG_ADAPTED 단계 ───────
	reactor.AddTask(&ReactorTask{
		Name:     "시스템 설정 적응 (CasC 등)",
		Requires: SYSTEM_CONFIG_LOADED,
		Attains:  SYSTEM_CONFIG_ADAPTED,
		Execute: func() error {
			simulateWork(5, 15)
			fmt.Printf("           → Configuration as Code 처리 완료\n")
			return nil
		},
	})

	// ─────── JOB_LOADED 단계 ───────
	reactor.AddTask(&ReactorTask{
		Name:     "Job 로드 (JENKINS_HOME/jobs/)",
		Requires: SYSTEM_CONFIG_ADAPTED,
		Attains:  JOB_LOADED,
		Execute: func() error {
			simulateWork(20, 50)
			jobs := []string{"my-web-app", "backend-api", "integration-tests", "nightly-build", "deploy-prod"}
			fmt.Printf("           → %d개 Job 로드:\n", len(jobs))
			for _, job := range jobs {
				fmt.Printf("             ├─ %s/\n", job)
				fmt.Printf("             │  ├─ config.xml\n")
				fmt.Printf("             │  └─ builds/ (nextBuildNumber 관리)\n")
			}
			return nil
		},
	})

	// ─────── JOB_CONFIG_ADAPTED 단계 ───────
	reactor.AddTask(&ReactorTask{
		Name:     "Job 설정 적응/업데이트",
		Requires: JOB_LOADED,
		Attains:  JOB_CONFIG_ADAPTED,
		Execute: func() error {
			simulateWork(5, 10)
			fmt.Printf("           → Job 설정 호환성 업데이트 완료\n")
			return nil
		},
	})

	// ─────── COMPLETED 단계로 가는 최종 작업 ───────
	reactor.AddTask(&ReactorTask{
		Name:     "Groovy 초기화 스크립트 실행",
		Requires: JOB_CONFIG_ADAPTED,
		Attains:  COMPLETED,
		Execute: func() error {
			simulateWork(5, 10)
			fmt.Printf("           → init.groovy.d/ 스크립트 실행 완료\n")
			return nil
		},
	})
}

// registerExtensions: 확장점 구현체 등록 시뮬레이션
// 실제: @Extension 어노테이션 스캔 → ExtensionList에 등록
func (wam *WebAppMain) registerExtensions() {
	er := wam.jenkins.extensionReg

	// SCM 확장점
	er.Register(&Extension{Name: "GitSCM", ExtensionPoint: "SCM", PluginName: "git", Ordinal: 0})

	// Builder 확장점
	er.Register(&Extension{Name: "Shell", ExtensionPoint: "Builder", PluginName: "core", Ordinal: 0})
	er.Register(&Extension{Name: "BatchFile", ExtensionPoint: "Builder", PluginName: "core", Ordinal: 1})

	// Publisher 확장점
	er.Register(&Extension{Name: "JUnitResultArchiver", ExtensionPoint: "Publisher", PluginName: "junit", Ordinal: 0})
	er.Register(&Extension{Name: "Mailer", ExtensionPoint: "Publisher", PluginName: "mailer", Ordinal: 1})

	// AuthorizationStrategy 확장점
	er.Register(&Extension{Name: "GlobalMatrixAuthorizationStrategy", ExtensionPoint: "AuthorizationStrategy", PluginName: "matrix-auth", Ordinal: 0})

	// CredentialsProvider 확장점
	er.Register(&Extension{Name: "SystemCredentialsProvider", ExtensionPoint: "CredentialsProvider", PluginName: "credentials", Ordinal: 0})

	// QueueTaskDispatcher 확장점
	er.Register(&Extension{Name: "DefaultQueueTaskDispatcher", ExtensionPoint: "QueueTaskDispatcher", PluginName: "core", Ordinal: 100})
}

// =============================================================================
// DAG 의존성 시각화
// =============================================================================

func printDAGVisualization() {
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("InitMilestone DAG (의존성 그래프)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()
	fmt.Println("  STARTED")
	fmt.Println("    │")
	fmt.Println("    ▼")
	fmt.Println("  PLUGINS_LISTED          ← 플러그인 메타데이터 검사, 의존성 해석")
	fmt.Println("    │")
	fmt.Println("    ▼")
	fmt.Println("  PLUGINS_PREPARED        ← 플러그인 classloader 설정")
	fmt.Println("    │")
	fmt.Println("    ▼")
	fmt.Println("  PLUGINS_STARTED         ← 플러그인 실행, 확장점 로드")
	fmt.Println("    │")
	fmt.Println("    ▼")
	fmt.Println("  EXTENSIONS_AUGMENTED    ← 확장점 구현체 추가")
	fmt.Println("    │")
	fmt.Println("    ▼")
	fmt.Println("  SYSTEM_CONFIG_LOADED    ← config.xml 로드")
	fmt.Println("    │")
	fmt.Println("    ▼")
	fmt.Println("  SYSTEM_CONFIG_ADAPTED   ← CasC 등 설정 적응")
	fmt.Println("    │")
	fmt.Println("    ▼")
	fmt.Println("  JOB_LOADED              ← jobs/ 디렉토리에서 Job 로드")
	fmt.Println("    │")
	fmt.Println("    ▼")
	fmt.Println("  JOB_CONFIG_ADAPTED      ← Job 설정 호환성 업데이트")
	fmt.Println("    │")
	fmt.Println("    ▼")
	fmt.Println("  COMPLETED               ← 초기화 완료, Groovy 스크립트 실행 후")
	fmt.Println()
	fmt.Println("  Reactor는 이 DAG를 위상정렬(topological sort)하여")
	fmt.Println("  각 마일스톤의 선행 조건이 충족된 후에만 해당 작업을 실행한다.")
	fmt.Println("  플러그인은 @Initializer(after=X, before=Y)로 자신의 작업을")
	fmt.Println("  특정 마일스톤 사이에 삽입할 수 있다.")
	fmt.Println()
}

// =============================================================================
// WAR 구조 시각화
// =============================================================================

func printWARStructure() {
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("Jenkins WAR 파일 내부 구조")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()
	fmt.Println("  jenkins.war")
	fmt.Println("  ├── META-INF/")
	fmt.Println("  │   └── MANIFEST.MF          # Main-Class: Main (Winstone 실행)")
	fmt.Println("  ├── WEB-INF/")
	fmt.Println("  │   ├── web.xml               # ServletContextListener 등록")
	fmt.Println("  │   │                          # → hudson.WebAppMain")
	fmt.Println("  │   ├── lib/                   # 핵심 라이브러리")
	fmt.Println("  │   │   ├── jenkins-core-*.jar  # Jenkins 핵심 로직")
	fmt.Println("  │   │   ├── remoting-*.jar      # 에이전트 통신")
	fmt.Println("  │   │   ├── stapler-*.jar        # URL→객체 매핑 프레임워크")
	fmt.Println("  │   │   └── xstream-*.jar        # XML 직렬화")
	fmt.Println("  │   └── classes/                # 컴파일된 클래스")
	fmt.Println("  ├── executable/                 # Winstone (내장 Jetty)")
	fmt.Println("  │   └── winstone.jar")
	fmt.Println("  ├── scripts/                    # Jelly/Groovy 뷰")
	fmt.Println("  └── css/, images/, help/        # 정적 리소스")
	fmt.Println()
	fmt.Println("  실행 방식:")
	fmt.Println("  $ java -jar jenkins.war --httpPort=8080")
	fmt.Println("  1. Main 클래스가 Winstone(내장 Jetty) 서버를 시작")
	fmt.Println("  2. web.xml의 <listener> → WebAppMain.contextInitialized() 호출")
	fmt.Println("  3. 초기화 스레드에서 Jenkins 싱글톤 생성, Reactor 실행")
	fmt.Println("  4. Reactor가 InitMilestone 순서대로 초기화 수행")
	fmt.Println("  5. COMPLETED 달성 후 HTTP 요청 수신 시작")
	fmt.Println()
}

// =============================================================================
// JENKINS_HOME 구조 시각화
// =============================================================================

func printJenkinsHomeStructure() {
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("JENKINS_HOME 파일시스템 구조")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()
	fmt.Println("  $JENKINS_HOME/")
	fmt.Println("  ├── config.xml              # 전역 설정 (numExecutors, mode 등)")
	fmt.Println("  ├── credentials.xml         # 자격 증명 저장소")
	fmt.Println("  ├── secret.key              # 암호화 키")
	fmt.Println("  ├── secrets/                # 비밀 관리")
	fmt.Println("  │   ├── master.key")
	fmt.Println("  │   └── hudson.util.Secret")
	fmt.Println("  ├── plugins/                # 설치된 플러그인")
	fmt.Println("  │   ├── git.jpi             # .jpi/.hpi 파일 (JAR 형태)")
	fmt.Println("  │   ├── git/                # 압축 해제된 플러그인")
	fmt.Println("  │   │   ├── META-INF/MANIFEST.MF")
	fmt.Println("  │   │   └── WEB-INF/")
	fmt.Println("  │   │       ├── classes/")
	fmt.Println("  │   │       └── lib/")
	fmt.Println("  │   └── credentials.jpi")
	fmt.Println("  ├── jobs/                   # Job 정의")
	fmt.Println("  │   ├── my-web-app/")
	fmt.Println("  │   │   ├── config.xml      # Job 설정")
	fmt.Println("  │   │   ├── nextBuildNumber  # 다음 빌드 번호")
	fmt.Println("  │   │   └── builds/         # 빌드 이력")
	fmt.Println("  │   │       ├── 1/          # 빌드 #1")
	fmt.Println("  │   │       │   ├── build.xml")
	fmt.Println("  │   │       │   ├── log")
	fmt.Println("  │   │       │   └── changelog.xml")
	fmt.Println("  │   │       └── 2/")
	fmt.Println("  │   └── backend-api/")
	fmt.Println("  ├── nodes/                  # 에이전트 노드 설정")
	fmt.Println("  │   └── agent-1/")
	fmt.Println("  │       └── config.xml")
	fmt.Println("  ├── users/                  # 사용자 설정")
	fmt.Println("  │   └── admin_*/")
	fmt.Println("  │       └── config.xml")
	fmt.Println("  ├── logs/                   # 에이전트 로그")
	fmt.Println("  ├── workspace/              # 빌드 워크스페이스")
	fmt.Println("  ├── war/                    # WAR 압축 해제 캐시")
	fmt.Println("  ├── updates/                # 업데이트 센터 캐시")
	fmt.Println("  └── init.groovy.d/          # 초기화 Groovy 스크립트")
	fmt.Println()
}

// =============================================================================
// Stapler URL 라우팅 데모
// =============================================================================

func printStaplerRouting() {
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("Stapler URL→객체 매핑 데모")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()
	fmt.Println("  Stapler는 URL 경로를 Java 객체 트리에 매핑하는 프레임워크:")
	fmt.Println()
	fmt.Println("  URL                          →  메서드 호출 체인")
	fmt.Println("  ─────────────────────────────────────────────────────")
	fmt.Println("  /                             →  Jenkins.doIndex()")
	fmt.Println("  /api/json                     →  Jenkins.getApi().doJson()")
	fmt.Println("  /job/my-app                   →  Jenkins.getItem('my-app')")
	fmt.Println("  /job/my-app/42                →  ...getItem('my-app').getBuild(42)")
	fmt.Println("  /job/my-app/42/console        →  ...getBuild(42).doConsole()")
	fmt.Println("  /job/my-app/build             →  ...getItem('my-app').doBuild()")
	fmt.Println("  /computer                     →  Jenkins.getComputer()")
	fmt.Println("  /computer/agent-1             →  Jenkins.getComputer('agent-1')")
	fmt.Println("  /manage                       →  Jenkins.getManage()")
	fmt.Println("  /queue/api/json               →  Jenkins.getQueue().getApi().doJson()")
	fmt.Println()
	fmt.Println("  규칙:")
	fmt.Println("  - URL 세그먼트가 getXxx() 또는 getDynamic(name)으로 매핑")
	fmt.Println("  - 마지막 세그먼트가 doXxx() 또는 index.jelly로 매핑")
	fmt.Println("  - @WebMethod 어노테이션으로 커스텀 매핑 가능")
	fmt.Println()
}

// =============================================================================
// HTTP 서버 데모 (Stapler 시뮬레이션)
// =============================================================================

func runHTTPServerDemo(jenkins *Jenkins) {
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("HTTP 서버 디스패처 데모 (Stapler 시뮬레이션)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	dispatcher := NewStaplerDispatcher(jenkins)

	// 가상 HTTP 요청 시뮬레이션
	routes := []struct {
		method string
		path   string
		desc   string
	}{
		{"GET", "/", "메인 페이지"},
		{"GET", "/api/json", "Jenkins API"},
		{"GET", "/job/my-web-app/", "Job 페이지"},
		{"GET", "/computer/", "노드 목록"},
		{"GET", "/queue/api/json", "빌드 큐 API"},
		{"GET", "/crumbIssuer/api/json", "CRUMB 발급"},
	}

	for _, route := range routes {
		fmt.Printf("  요청: %s %s (%s)\n", route.method, route.path, route.desc)

		// http.ResponseWriter 시뮬레이션
		recorder := &responseRecorder{headers: make(http.Header), body: &strings.Builder{}}
		req, _ := http.NewRequest(route.method, route.path, nil)

		dispatcher.mux.ServeHTTP(recorder, req)

		fmt.Printf("  응답 (status=%d):\n", recorder.statusCode)
		body := recorder.body.String()
		for _, line := range strings.Split(body, "\n") {
			if line != "" {
				fmt.Printf("    %s\n", line)
			}
		}
		fmt.Println()
	}
}

// responseRecorder: http.ResponseWriter 구현 (테스트용)
type responseRecorder struct {
	statusCode int
	headers    http.Header
	body       *strings.Builder
}

func (rr *responseRecorder) Header() http.Header          { return rr.headers }
func (rr *responseRecorder) WriteHeader(statusCode int)    { rr.statusCode = statusCode }
func (rr *responseRecorder) Write(b []byte) (int, error) { return rr.body.Write(b) }

// =============================================================================
// 다중 컴포넌트 통합 데모
// 단일 WAR 바이너리에 포함된 여러 서브시스템의 협력을 시연한다.
// =============================================================================

type SubsystemStatus struct {
	Name      string
	Status    string
	StartTime time.Time
	Ready     bool
}

func printSubsystemArchitecture() {
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("Jenkins 서브시스템 아키텍처")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()
	fmt.Println("  ┌─────────────────────────────────────────────────────────┐")
	fmt.Println("  │                    jenkins.war                          │")
	fmt.Println("  │                                                         │")
	fmt.Println("  │  ┌──────────┐  ┌──────────┐  ┌──────────────────────┐  │")
	fmt.Println("  │  │ Winstone │  │ Stapler  │  │   Jenkins Core       │  │")
	fmt.Println("  │  │ (Jetty)  │  │ (URL     │  │   ┌───────────────┐  │  │")
	fmt.Println("  │  │          │──│  routing) │──│   │ Queue         │  │  │")
	fmt.Println("  │  │ HTTP     │  │          │  │   │ (빌드 스케줄러)│  │  │")
	fmt.Println("  │  │ Server   │  │ GET /job/ │  │   ├───────────────┤  │  │")
	fmt.Println("  │  │          │  │ → Jenkins │  │   │ Executor Pool │  │  │")
	fmt.Println("  │  │ :8080    │  │  .getItem │  │   │ (빌드 실행)   │  │  │")
	fmt.Println("  │  └──────────┘  │  (name)   │  │   ├───────────────┤  │  │")
	fmt.Println("  │                └──────────┘  │   │ SCM / Pipeline│  │  │")
	fmt.Println("  │                              │   │ (소스 관리)   │  │  │")
	fmt.Println("  │  ┌─────────────────────────┐ │   └───────────────┘  │  │")
	fmt.Println("  │  │    Plugin Manager       │ │                      │  │")
	fmt.Println("  │  │  ┌─────┐ ┌─────┐ ┌───┐ │ │  ┌────────────────┐  │  │")
	fmt.Println("  │  │  │ git │ │cred │ │...│ │ │  │ XStream        │  │  │")
	fmt.Println("  │  │  └─────┘ └─────┘ └───┘ │ │  │ (XML 직렬화)  │  │  │")
	fmt.Println("  │  └─────────────────────────┘ │  └────────────────┘  │  │")
	fmt.Println("  │                              └──────────────────────┘  │")
	fmt.Println("  └─────────────────────────────────────────────────────────┘")
	fmt.Println()
	fmt.Println("  모든 컴포넌트가 하나의 WAR 파일에 패키징되어")
	fmt.Println("  java -jar jenkins.war 한 줄로 전체 시스템이 실행된다.")
	fmt.Println()
}

// =============================================================================
// 부팅 시퀀스 전체 데모
// =============================================================================

func demonstrateBootSequence() {
	fmt.Println()
	fmt.Println("╔═════════════════════════════════════════════════════════╗")
	fmt.Println("║  Jenkins 부팅 시퀀스 시뮬레이션                        ║")
	fmt.Println("║  WebAppMain → InitThread → Reactor → 마일스톤 순회    ║")
	fmt.Println("╚═════════════════════════════════════════════════════════╝")
	fmt.Println()

	// 1단계: WebAppMain.contextInitialized() 호출
	wam := NewWebAppMain("/var/jenkins_home")
	wam.contextInitialized("/var/jenkins_home")

	// 초기화 완료 대기
	<-wam.initDone

	fmt.Println()

	// HTTP 서버 데모
	if wam.jenkins != nil && wam.jenkins.isReady() {
		runHTTPServerDemo(wam.jenkins)
	}
}

// =============================================================================
// Reactor 위상정렬 알고리즘 데모
// 마일스톤 외에 커스텀 의존성을 추가하여 위상정렬 동작을 시연
// =============================================================================

func demonstrateTopologicalSort() {
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("Reactor 위상정렬 알고리즘 데모")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	// DAG 정의 (Kahn's algorithm 시연)
	type Node struct {
		Name string
		Deps []string
	}

	nodes := []Node{
		{Name: "F", Deps: []string{"D", "E"}},
		{Name: "D", Deps: []string{"B", "C"}},
		{Name: "E", Deps: []string{"C"}},
		{Name: "C", Deps: []string{"A"}},
		{Name: "B", Deps: []string{"A"}},
		{Name: "A", Deps: nil},
	}

	fmt.Println("  입력 그래프:")
	fmt.Println("       A")
	fmt.Println("      / \\")
	fmt.Println("     B   C")
	fmt.Println("      \\ / \\")
	fmt.Println("       D   E")
	fmt.Println("        \\ /")
	fmt.Println("         F")
	fmt.Println()

	// 위상정렬 실행
	inDegree := make(map[string]int)
	nameSet := make(map[string]bool)
	depMap := make(map[string][]string)
	reverseMap := make(map[string][]string) // node → 이 노드에 의존하는 노드들

	for _, n := range nodes {
		nameSet[n.Name] = true
		depMap[n.Name] = n.Deps
		if _, exists := inDegree[n.Name]; !exists {
			inDegree[n.Name] = 0
		}
		for _, dep := range n.Deps {
			inDegree[n.Name]++
			reverseMap[dep] = append(reverseMap[dep], n.Name)
		}
	}

	var queue []string
	for name := range nameSet {
		if inDegree[name] == 0 {
			queue = append(queue, name)
		}
	}
	sort.Strings(queue)

	var sorted []string
	step := 1

	fmt.Println("  위상정렬 과정 (Kahn's Algorithm):")
	fmt.Printf("  초기 진입차수: ")
	for _, n := range nodes {
		fmt.Printf("%s=%d ", n.Name, inDegree[n.Name])
	}
	fmt.Println()
	fmt.Println()

	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		sorted = append(sorted, name)

		fmt.Printf("  Step %d: %s 처리 (진입차수=0)\n", step, name)

		// 이 노드에 의존하는 노드들의 진입차수 감소
		for _, dependent := range reverseMap[name] {
			inDegree[dependent]--
			fmt.Printf("          → %s 진입차수: %d→%d\n", dependent, inDegree[dependent]+1, inDegree[dependent])
			if inDegree[dependent] == 0 {
				queue = append(queue, dependent)
				sort.Strings(queue)
			}
		}
		step++
	}

	fmt.Println()
	fmt.Printf("  위상정렬 결과: %s\n", strings.Join(sorted, " → "))
	fmt.Println()
	fmt.Println("  Jenkins의 Reactor도 동일한 원리로 초기화 작업의 실행 순서를 결정한다.")
	fmt.Println("  @Initializer(after=PLUGINS_STARTED, before=EXTENSIONS_AUGMENTED)와 같이")
	fmt.Println("  선언된 의존성을 DAG로 구성하고 위상정렬로 실행한다.")
	fmt.Println()
}

// =============================================================================
// 플러그인 classloader 계층 시각화
// =============================================================================

func printClassLoaderHierarchy() {
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("Jenkins ClassLoader 계층 구조")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()
	fmt.Println("  Bootstrap ClassLoader (JRE)")
	fmt.Println("    │")
	fmt.Println("    ├── System ClassLoader (classpath)")
	fmt.Println("    │     │")
	fmt.Println("    │     ├── Winstone ClassLoader (winstone.jar)")
	fmt.Println("    │     │     │")
	fmt.Println("    │     │     └── WebApp ClassLoader (WEB-INF/lib/)")
	fmt.Println("    │     │           │")
	fmt.Println("    │     │           ├── jenkins-core-*.jar")
	fmt.Println("    │     │           ├── stapler-*.jar")
	fmt.Println("    │     │           ├── remoting-*.jar")
	fmt.Println("    │     │           │")
	fmt.Println("    │     │           ├── PluginClassLoader (credentials)")
	fmt.Println("    │     │           │     │")
	fmt.Println("    │     │           │     └── PluginClassLoader (ssh-credentials)")
	fmt.Println("    │     │           │           │")
	fmt.Println("    │     │           │           └── PluginClassLoader (git-client)")
	fmt.Println("    │     │           │                 │")
	fmt.Println("    │     │           │                 └── PluginClassLoader (git)")
	fmt.Println("    │     │           │")
	fmt.Println("    │     │           ├── PluginClassLoader (workflow-step-api)")
	fmt.Println("    │     │           │     │")
	fmt.Println("    │     │           │     └── PluginClassLoader (workflow-api)")
	fmt.Println("    │     │           │           │")
	fmt.Println("    │     │           │           └── PluginClassLoader (pipeline-model-definition)")
	fmt.Println("    │     │           │")
	fmt.Println("    │     │           └── PluginClassLoader (junit, mailer, ...)")
	fmt.Println()
	fmt.Println("  각 플러그인은 독립적인 ClassLoader를 가지며,")
	fmt.Println("  의존하는 플러그인의 ClassLoader를 parent로 참조한다.")
	fmt.Println("  이를 통해 플러그인 간 클래스 격리와 의존성 공유를 동시에 달성한다.")
	fmt.Println()
}

// =============================================================================
// 유틸리티
// =============================================================================

func simulateWork(minMs, maxMs int) {
	duration := time.Duration(minMs+rand.Intn(maxMs-minMs+1)) * time.Millisecond
	time.Sleep(duration)
}

// =============================================================================
// main: 모든 데모를 순차적으로 실행
// =============================================================================

func main() {
	fmt.Println("╔═════════════════════════════════════════════════════════╗")
	fmt.Println("║  poc-01-architecture: Jenkins WAR 디스패처 +           ║")
	fmt.Println("║  초기화 시퀀스 시뮬레이션                              ║")
	fmt.Println("║                                                         ║")
	fmt.Println("║  Jenkins 소스 참조:                                     ║")
	fmt.Println("║  - hudson/init/InitMilestone.java                       ║")
	fmt.Println("║  - hudson/WebAppMain.java                               ║")
	fmt.Println("║  - jenkins/model/Jenkins.java                           ║")
	fmt.Println("║  - hudson/PluginManager.java                            ║")
	fmt.Println("╚═════════════════════════════════════════════════════════╝")
	fmt.Println()

	// 1. WAR 구조 설명
	printWARStructure()

	// 2. JENKINS_HOME 구조
	printJenkinsHomeStructure()

	// 3. InitMilestone DAG 시각화
	printDAGVisualization()

	// 4. 서브시스템 아키텍처
	printSubsystemArchitecture()

	// 5. ClassLoader 계층
	printClassLoaderHierarchy()

	// 6. 위상정렬 알고리즘 데모
	demonstrateTopologicalSort()

	// 7. Stapler URL 라우팅 설명
	printStaplerRouting()

	// 8. 부팅 시퀀스 실행 (핵심 데모)
	demonstrateBootSequence()

	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("데모 완료")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
}
