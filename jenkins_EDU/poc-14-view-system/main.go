// poc-14-view-system: Jenkins View 시스템 시뮬레이션
//
// Jenkins 소스코드 참조:
//   - hudson.model.View (abstract class, ExtensionPoint)
//     필드: name, description, owner(ViewGroup), filterExecutors, filterQueue
//     메서드: getItems(), contains(), getComputers(), getQueueItems()
//   - hudson.model.AllView (모든 작업을 보여주는 기본 뷰)
//     getItems() → owner.getItemGroup().getItems() (전체 반환)
//     contains() → 항상 true
//   - hudson.model.ListView (명시적으로 선택한 작업만 보여주는 뷰)
//     필드: jobNames(TreeSet), includeRegex, includePattern, jobFilters, recurse
//     getItems() → jobNames + includePattern 매칭 후 jobFilters 체인 적용
//     contains() → getItems().contains(item)
//   - hudson.model.ViewGroup (interface)
//     getViews(), getView(name), getPrimaryView(), canDelete(), deleteView()
//     getItemGroup() → TopLevelItem 컨테이너
//   - hudson.views.ViewJobFilter (abstract class)
//     filter(added, all, filteringView) → 필터 체인에서 항목 추가/제거
//   - hudson.views.StatusFilter (ViewJobFilter 구현)
//     statusFilter=true → 활성 작업만, false → 비활성 작업만
//   - hudson.model.HealthReport
//     score(0~100), iconClassName, iconUrl, description
//     점수 구간: 0~20(폭풍), 21~40(비), 41~60(구름), 61~80(구름해), 81~100(해)
//   - hudson.model.Job.getBuildStabilityHealthReport()
//     최근 5개 빌드 중 실패 비율로 건강 점수 계산
//     score = 100 * (totalCount - failCount) / totalCount
//
// 핵심 원리:
//   1) View는 추상 클래스로, 작업(Job) 목록을 보여주는 UI 컴포넌트
//   2) AllView는 ViewGroup의 모든 작업을 무조건 표시
//   3) ListView는 jobNames(명시적 선택) + includeRegex(패턴 매칭) + jobFilters(필터 체인)으로 작업 선별
//   4) ViewGroup(보통 Jenkins 인스턴스)이 View의 컨테이너 역할
//   5) filterExecutors/filterQueue로 뷰에 포함된 작업의 실행자/큐만 필터링
//   6) HealthReport는 빌드 성공률 기반의 0~100 점수와 5단계 아이콘
//
// 실행: go run main.go

package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// =============================================================================
// 1. BuildResult & BuildRecord: 빌드 결과
// =============================================================================
// Jenkins: hudson.model.Result
// - SUCCESS, UNSTABLE, FAILURE, NOT_BUILT, ABORTED
// Jenkins: hudson.model.Run
// - 각 빌드 실행의 기록 (번호, 결과, 시간 등)

// BuildResult는 빌드 결과 상태를 나타낸다.
type BuildResult int

const (
	SUCCESS  BuildResult = iota // 성공 (파란색)
	UNSTABLE                    // 불안정 (노란색) - 테스트 실패 등
	FAILURE                     // 실패 (빨간색)
	ABORTED                     // 중단됨
)

// String은 빌드 결과를 문자열로 반환한다.
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

// Icon은 Jenkins 아이콘 색상을 반환한다.
// Jenkins: hudson.model.BallColor - BLUE(성공), YELLOW(불안정), RED(실패)
func (r BuildResult) Icon() string {
	switch r {
	case SUCCESS:
		return "blue"    // Jenkins에서 성공은 파란 구슬
	case UNSTABLE:
		return "yellow"  // 테스트 실패 등은 노란 구슬
	case FAILURE:
		return "red"     // 빌드 실패는 빨간 구슬
	case ABORTED:
		return "aborted" // 중단은 회색
	default:
		return "grey"
	}
}

// BuildRecord는 개별 빌드 실행 기록이다.
// Jenkins: hudson.model.Run - number, result, timestamp, duration 등
type BuildRecord struct {
	Number    int           // 빌드 번호
	Result    BuildResult   // 빌드 결과
	Timestamp time.Time     // 빌드 시작 시간
	Duration  time.Duration // 빌드 소요 시간
}

// =============================================================================
// 2. HealthReport: 건강 보고서
// =============================================================================
// Jenkins: hudson.model.HealthReport
// - score: 0~100 퍼센트
// - iconClassName: 아이콘 CSS 클래스
// - iconUrl: 아이콘 이미지 경로
// - description: 툴팁 설명
//
// 점수 구간 및 아이콘 (HealthReport 생성자에서 결정):
//   0~20  → icon-health-00to19 (health-00to19.png) - 폭풍
//   21~40 → icon-health-20to39 (health-20to39.png) - 비
//   41~60 → icon-health-40to59 (health-40to59.png) - 구름
//   61~80 → icon-health-60to79 (health-60to79.png) - 구름+해
//   81~100→ icon-health-80plus  (health-80plus.png) - 해

// HealthReport는 프로젝트의 건강 상태를 나타낸다.
type HealthReport struct {
	Score         int    // 0~100 건강 점수
	IconClassName string // CSS 아이콘 클래스
	IconUrl       string // 아이콘 이미지 경로
	Description   string // 설명 (툴팁)
}

// NewHealthReport는 점수를 기반으로 HealthReport를 생성한다.
// Jenkins: HealthReport(int score, String iconUrl, Localizable description) 생성자 참조.
// 점수에 따라 자동으로 적절한 아이콘이 할당된다.
func NewHealthReport(score int, description string) *HealthReport {
	hr := &HealthReport{
		Score:       score,
		Description: description,
	}

	// Jenkins HealthReport 생성자의 아이콘 결정 로직 재현
	switch {
	case score <= 20:
		hr.IconClassName = "icon-health-00to19"
		hr.IconUrl = "health-00to19.png"
	case score <= 40:
		hr.IconClassName = "icon-health-20to39"
		hr.IconUrl = "health-20to39.png"
	case score <= 60:
		hr.IconClassName = "icon-health-40to59"
		hr.IconUrl = "health-40to59.png"
	case score <= 80:
		hr.IconClassName = "icon-health-60to79"
		hr.IconUrl = "health-60to79.png"
	default:
		hr.IconClassName = "icon-health-80plus"
		hr.IconUrl = "health-80plus.png"
	}

	return hr
}

// WeatherEmoji는 건강 점수에 대응하는 날씨 아이콘을 반환한다.
func (hr *HealthReport) WeatherEmoji() string {
	switch {
	case hr.Score <= 20:
		return "[STORM]" // 폭풍 - 매우 불건강
	case hr.Score <= 40:
		return "[RAIN]"  // 비 - 불건강
	case hr.Score <= 60:
		return "[CLOUD]" // 구름 - 보통
	case hr.Score <= 80:
		return "[CLOUDY]" // 구름+해 - 양호
	default:
		return "[SUNNY]" // 해 - 건강
	}
}

// =============================================================================
// 3. TopLevelItem / Job: 최상위 항목 (작업)
// =============================================================================
// Jenkins: hudson.model.TopLevelItem (interface)
//   - Jenkins 루트에 직접 포함되는 항목
//   - Job, Folder 등이 구현
// Jenkins: hudson.model.Job (abstract class)
//   - builds: RunMap, healthReports 등
//   - getBuildStabilityHealthReport(): 최근 5개 빌드 기반 건강 점수

// TopLevelItem은 Jenkins의 최상위 항목 인터페이스이다.
type TopLevelItem interface {
	GetName() string
	GetFullName() string
	GetDisplayName() string
}

// Job은 빌드 가능한 프로젝트를 나타낸다.
// Jenkins: hudson.model.Job<JobT, RunT>
type Job struct {
	Name        string         // 작업 이름
	FullName    string         // 전체 경로 (폴더 포함)
	Description string         // 설명
	Disabled    bool           // 비활성화 여부
	Builds      []BuildRecord  // 빌드 이력 (최신순)
	Building    bool           // 현재 빌드 중 여부
}

func (j *Job) GetName() string        { return j.Name }
func (j *Job) GetFullName() string    { return j.FullName }
func (j *Job) GetDisplayName() string { return j.Name }

// GetBuildStabilityHealthReport는 최근 5개 빌드를 기반으로 건강 점수를 계산한다.
// Jenkins: hudson.model.Job.getBuildStabilityHealthReport()
// - 최근 5개 빌드 중 실패 개수로 점수 계산
// - score = 100 * (totalCount - failCount) / totalCount
// - 현재 빌드 중이면 이전 빌드부터 카운트 (Jenkins 소스 1283행)
func (j *Job) GetBuildStabilityHealthReport() *HealthReport {
	if len(j.Builds) == 0 {
		return NewHealthReport(100, "빌드 이력 없음")
	}

	failCount := 0
	totalCount := 0
	startIdx := 0

	// Jenkins: if (lastBuild != null && lastBuild.isBuilding())
	//   lastBuild = lastBuild.getPreviousBuild();
	if j.Building && len(j.Builds) > 0 {
		startIdx = 1 // 현재 빌드 중이면 이전 빌드부터
	}

	// 최근 5개 빌드를 검사
	for i := startIdx; i < len(j.Builds) && totalCount < 5; i++ {
		build := j.Builds[i]
		switch build.Result {
		case SUCCESS, UNSTABLE:
			// BLUE, YELLOW → 성공으로 간주 (Jenkins: case BLUE: case YELLOW:)
			totalCount++
		case FAILURE:
			// RED → 실패
			failCount++
			totalCount++
		default:
			// ABORTED 등은 건너뜀 (Jenkins: default: break)
		}
	}

	if totalCount == 0 {
		return nil
	}

	score := int(100.0 * float64(totalCount-failCount) / float64(totalCount))

	// 설명 생성 (Jenkins: Messages._Job_NOfMFailed, _Job_NoRecentBuildFailed 등)
	var description string
	if failCount == 0 {
		description = "최근 빌드 실패 없음"
	} else if totalCount == failCount {
		description = "모든 최근 빌드 실패"
	} else {
		description = fmt.Sprintf("최근 %d회 빌드 중 %d회 실패", totalCount, failCount)
	}

	return NewHealthReport(score, fmt.Sprintf("빌드 안정성: %s", description))
}

// =============================================================================
// 4. Executor & QueueItem: 실행자와 큐 항목
// =============================================================================
// Jenkins: hudson.model.Executor - 빌드를 실행하는 스레드
// Jenkins: hudson.model.Queue.Item - 빌드 대기열 항목
// View.getComputers()와 View.getQueueItems()에서 필터링에 사용

// Executor는 빌드를 실행하는 단위이다.
type Executor struct {
	NodeName string // 노드 이름
	Number   int    // 실행자 번호
	JobName  string // 현재 실행 중인 작업 (빈 문자열이면 유휴)
	Idle     bool   // 유휴 상태
}

// QueueItem은 빌드 대기열의 항목이다.
type QueueItem struct {
	ID       int       // 큐 아이템 ID
	JobName  string    // 대기 중인 작업 이름
	EnqueueTime time.Time // 대기열 진입 시간
	Why      string    // 대기 이유
}

// =============================================================================
// 5. ViewJobFilter: 뷰 작업 필터 인터페이스
// =============================================================================
// Jenkins: hudson.views.ViewJobFilter (abstract class)
// - filter(added, all, filteringView) → List<TopLevelItem>
// - 각 필터가 체인으로 연결되어 순차적으로 적용
// - ListView.getItems()에서 jobFilters가 비어있지 않으면 체인 실행

// ViewJobFilter는 뷰의 작업 목록을 필터링하는 인터페이스이다.
type ViewJobFilter interface {
	// Filter는 현재까지 추가된 항목(added)과 전체 후보(all)를 받아
	// 필터링된 결과를 반환한다.
	// Jenkins: ViewJobFilter.filter(List<TopLevelItem> added, List<TopLevelItem> all, View filteringView)
	Filter(added []TopLevelItem, all []TopLevelItem) []TopLevelItem
	GetName() string
}

// StatusFilter는 작업의 활성/비활성 상태로 필터링한다.
// Jenkins: hudson.views.StatusFilter
// - statusFilter=true: 활성 작업만 표시
// - statusFilter=false: 비활성 작업만 표시
type StatusFilter struct {
	StatusFilter bool // true=활성만, false=비활성만
}

func (f *StatusFilter) GetName() string {
	if f.StatusFilter {
		return "StatusFilter(활성만)"
	}
	return "StatusFilter(비활성만)"
}

// Filter는 StatusFilter의 필터링 로직을 수행한다.
// Jenkins: StatusFilter.filter() 참조
// - ParameterizedJob.isDisabled() ^ statusFilter 로 필터링
// - 즉 statusFilter=true이면 isDisabled()=false인 것만 통과
func (f *StatusFilter) Filter(added []TopLevelItem, all []TopLevelItem) []TopLevelItem {
	var filtered []TopLevelItem
	for _, item := range added {
		if job, ok := item.(*Job); ok {
			// Jenkins: ((ParameterizedJob) item).isDisabled() ^ statusFilter
			// XOR: disabled=false, statusFilter=true → false^true=true → 통과
			//       disabled=true, statusFilter=true → true^true=false → 제외
			if job.Disabled != f.StatusFilter {
				filtered = append(filtered, item)
			}
		} else {
			// Job이 아닌 항목은 그대로 포함 (Jenkins 원본 동작)
			filtered = append(filtered, item)
		}
	}
	return filtered
}

// HealthFilter는 건강 점수 기준으로 필터링하는 커스텀 필터이다.
// Jenkins에는 내장되어 있지 않지만, ViewJobFilter 확장 패턴을 보여주기 위해 포함.
type HealthFilter struct {
	MinScore int // 최소 건강 점수
}

func (f *HealthFilter) GetName() string {
	return fmt.Sprintf("HealthFilter(점수>=%d)", f.MinScore)
}

// Filter는 건강 점수가 MinScore 이상인 작업만 남긴다.
func (f *HealthFilter) Filter(added []TopLevelItem, all []TopLevelItem) []TopLevelItem {
	var filtered []TopLevelItem
	for _, item := range added {
		if job, ok := item.(*Job); ok {
			hr := job.GetBuildStabilityHealthReport()
			if hr != nil && hr.Score >= f.MinScore {
				filtered = append(filtered, item)
			}
		}
	}
	return filtered
}

// =============================================================================
// 6. View: 뷰 추상 인터페이스
// =============================================================================
// Jenkins: hudson.model.View (abstract class)
// 필드:
//   - owner: ViewGroup (컨테이너)
//   - name: 뷰 이름
//   - description: 설명
//   - filterExecutors: 관련 실행자만 표시
//   - filterQueue: 관련 큐 항목만 표시
// 추상 메서드:
//   - getItems() → Collection<TopLevelItem>
//   - contains(TopLevelItem) → boolean

// View는 작업 목록을 보여주는 UI 컴포넌트 인터페이스이다.
type View interface {
	GetName() string
	GetDescription() string
	GetItems() []TopLevelItem
	Contains(item TopLevelItem) bool
	IsFilterExecutors() bool
	IsFilterQueue() bool
	SetOwner(owner ViewGroup)
	GetOwner() ViewGroup
}

// BaseView는 View의 공통 필드를 제공한다.
// Jenkins: View 추상 클래스의 필드들에 대응.
type BaseView struct {
	Name            string    // Jenkins: View.name
	Description     string    // Jenkins: View.description
	FilterExecutors bool      // Jenkins: View.filterExecutors
	FilterQueue     bool      // Jenkins: View.filterQueue
	Owner           ViewGroup // Jenkins: View.owner
}

func (v *BaseView) GetName() string        { return v.Name }
func (v *BaseView) GetDescription() string { return v.Description }
func (v *BaseView) IsFilterExecutors() bool { return v.FilterExecutors }
func (v *BaseView) IsFilterQueue() bool     { return v.FilterQueue }
func (v *BaseView) SetOwner(owner ViewGroup) { v.Owner = owner }
func (v *BaseView) GetOwner() ViewGroup      { return v.Owner }

// GetComputers는 뷰에 관련된 노드의 실행자 목록을 반환한다.
// Jenkins: View.getComputers()
// - filterExecutors=false: 모든 컴퓨터 반환
// - filterExecutors=true: 뷰 항목을 실행 중인 컴퓨터만 반환
func GetComputers(v View, allExecutors []Executor) []Executor {
	if !v.IsFilterExecutors() {
		return allExecutors
	}

	// 뷰에 포함된 작업 이름 수집
	itemNames := make(map[string]bool)
	for _, item := range v.GetItems() {
		itemNames[item.GetName()] = true
	}

	// 뷰 항목을 실행 중인 실행자만 필터링
	var filtered []Executor
	for _, exec := range allExecutors {
		if exec.Idle || itemNames[exec.JobName] {
			filtered = append(filtered, exec)
		}
	}
	return filtered
}

// GetQueueItems는 뷰에 관련된 큐 항목을 반환한다.
// Jenkins: View.getQueueItems() → filterQueue(Arrays.asList(Jenkins.get().getQueue().getItems()))
// Jenkins: View.filterQueue()
// - filterQueue=false: 전체 큐 반환
// - filterQueue=true: 뷰 항목에 해당하는 큐만 반환
func GetQueueItems(v View, allQueueItems []QueueItem) []QueueItem {
	if !v.IsFilterQueue() {
		return allQueueItems
	}

	itemNames := make(map[string]bool)
	for _, item := range v.GetItems() {
		itemNames[item.GetName()] = true
	}

	var filtered []QueueItem
	for _, qi := range allQueueItems {
		if itemNames[qi.JobName] {
			filtered = append(filtered, qi)
		}
	}
	return filtered
}

// =============================================================================
// 7. AllView: 모든 작업을 보여주는 뷰
// =============================================================================
// Jenkins: hudson.model.AllView
// - getItems() → owner.getItemGroup().getItems() (전체 반환)
// - contains(item) → true (항상)
// - isEditable() → false (설정 변경 불가)
// - DEFAULT_VIEW_NAME = "all"

// AllView는 ViewGroup의 모든 작업을 표시하는 뷰이다.
type AllView struct {
	BaseView
}

const DefaultViewName = "all" // Jenkins: AllView.DEFAULT_VIEW_NAME

// NewAllView는 새 AllView를 생성한다.
// Jenkins: AllView(String name, ViewGroup owner)
func NewAllView(name string, owner ViewGroup) *AllView {
	return &AllView{
		BaseView: BaseView{
			Name:        name,
			Description: "모든 작업을 표시합니다",
			Owner:       owner,
		},
	}
}

// GetItems는 소유자의 모든 항목을 반환한다.
// Jenkins: AllView.getItems() → (Collection) getOwner().getItemGroup().getItems()
func (v *AllView) GetItems() []TopLevelItem {
	if v.Owner == nil {
		return nil
	}
	return v.Owner.GetAllItems()
}

// Contains는 항상 true를 반환한다.
// Jenkins: AllView.contains(TopLevelItem item) → return true
func (v *AllView) Contains(item TopLevelItem) bool {
	return true
}

// =============================================================================
// 8. ListView: 선택된 작업만 보여주는 뷰
// =============================================================================
// Jenkins: hudson.model.ListView
// 필드:
//   - jobNames: SortedSet<String> (TreeSet, CASE_INSENSITIVE_ORDER)
//   - includeRegex: String (정규식 문자열)
//   - includePattern: Pattern (컴파일된 정규식, transient)
//   - jobFilters: DescribableList<ViewJobFilter>
//   - recurse: boolean (ItemGroup 재귀 탐색)
//
// getItems() 로직 (ListView.java 218행~):
//   1. jobNames에 포함된 이름의 작업을 수집
//   2. includePattern이 있으면 패턴 매칭되는 작업도 추가
//   3. jobFilters가 있으면 체인으로 순차 필터링
//   4. LinkedHashSet으로 중복 제거

// ListView는 명시적으로 선택한 작업과 패턴 매칭된 작업을 보여주는 뷰이다.
type ListView struct {
	BaseView
	JobNames       map[string]bool   // Jenkins: jobNames (TreeSet, case-insensitive)
	IncludeRegex   string            // Jenkins: includeRegex
	includePattern *regexp.Regexp    // Jenkins: includePattern (transient, 컴파일된 정규식)
	JobFilters     []ViewJobFilter   // Jenkins: jobFilters (DescribableList<ViewJobFilter>)
	Recurse        bool              // Jenkins: recurse
}

// NewListView는 새 ListView를 생성한다.
// Jenkins: ListView(String name, ViewGroup owner)
func NewListView(name string, owner ViewGroup) *ListView {
	return &ListView{
		BaseView: BaseView{
			Name:  name,
			Owner: owner,
		},
		JobNames:   make(map[string]bool),
		JobFilters: nil,
	}
}

// Add는 작업을 뷰에 추가한다.
// Jenkins: ListView.add(TopLevelItem item)
// - jobNames.add(item.getRelativeNameFrom(getOwner().getItemGroup()))
func (v *ListView) Add(item TopLevelItem) {
	v.JobNames[item.GetName()] = true
}

// Remove는 작업을 뷰에서 제거한다.
// Jenkins: ListView.remove(TopLevelItem item)
func (v *ListView) Remove(item TopLevelItem) bool {
	name := item.GetName()
	if _, ok := v.JobNames[name]; ok {
		delete(v.JobNames, name)
		return true
	}
	return false
}

// SetIncludeRegex는 포함 정규식을 설정한다.
// Jenkins: ListView.setIncludeRegex(String includeRegex)
// - Util.nullify()로 빈 문자열을 nil 처리
// - Pattern.compile()로 정규식 컴파일
func (v *ListView) SetIncludeRegex(regex string) error {
	if regex == "" {
		v.IncludeRegex = ""
		v.includePattern = nil
		return nil
	}
	pattern, err := regexp.Compile(regex)
	if err != nil {
		return fmt.Errorf("잘못된 정규식: %v", err)
	}
	v.IncludeRegex = regex
	v.includePattern = pattern
	return nil
}

// AddJobFilter는 ViewJobFilter를 추가한다.
func (v *ListView) AddJobFilter(filter ViewJobFilter) {
	v.JobFilters = append(v.JobFilters, filter)
}

// GetItems는 뷰에 포함된 작업 목록을 반환한다.
// Jenkins: ListView.getItems(boolean recurse) (218행~272행)
//
// 알고리즘:
//   1단계: jobNames에 있는 이름으로 작업 수집
//   2단계: includePattern이 있으면 패턴 매칭 추가
//   3단계: jobFilters가 있으면 체인으로 필터링
//   4단계: LinkedHashSet으로 중복 제거
func (v *ListView) GetItems() []TopLevelItem {
	if v.Owner == nil {
		return nil
	}

	allItems := v.Owner.GetAllItems()
	var items []TopLevelItem

	// 1단계: jobNames에 있는 작업 수집
	// Jenkins: for (String name : names) { TopLevelItem i = parent.getItem(name); ... }
	for _, item := range allItems {
		if v.JobNames[item.GetName()] {
			items = append(items, item)
		}
	}

	// 2단계: includePattern으로 매칭되는 작업 추가
	// Jenkins: if (includePattern != null) { items.addAll(parent.getItems(item -> includePattern.matcher(itemName).matches())); }
	if v.includePattern != nil {
		for _, item := range allItems {
			if v.includePattern.MatchString(item.GetName()) {
				items = append(items, item)
			}
		}
	}

	// 3단계: jobFilters 체인 적용
	// Jenkins: for (ViewJobFilter jobFilter : jobFilters) { items = jobFilter.filter(items, candidates, this); }
	if len(v.JobFilters) > 0 {
		for _, filter := range v.JobFilters {
			items = filter.Filter(items, allItems)
		}
	}

	// 4단계: 중복 제거 (Jenkins: items = new ArrayList<>(new LinkedHashSet<>(items)))
	seen := make(map[string]bool)
	var unique []TopLevelItem
	for _, item := range items {
		if !seen[item.GetName()] {
			seen[item.GetName()] = true
			unique = append(unique, item)
		}
	}

	return unique
}

// Contains는 특정 항목이 뷰에 포함되는지 확인한다.
// Jenkins: ListView.contains(TopLevelItem item) → getItems().contains(item)
func (v *ListView) Contains(item TopLevelItem) bool {
	for _, i := range v.GetItems() {
		if i.GetName() == item.GetName() {
			return true
		}
	}
	return false
}

// =============================================================================
// 9. ViewGroup: 뷰의 컨테이너
// =============================================================================
// Jenkins: hudson.model.ViewGroup (interface)
// - getViews() → Collection<View>
// - getView(name) → View
// - getPrimaryView() → View
// - canDelete(view) → boolean
// - deleteView(view)
// - getItemGroup() → ItemGroup<? extends TopLevelItem>
//
// Jenkins 인스턴스 자체가 ViewGroup을 구현한다.
// ViewGroup.getAllViews()는 중첩 뷰를 재귀적으로 수집한다.

// ViewGroup은 View의 컨테이너 인터페이스이다.
type ViewGroup interface {
	GetViews() []View
	GetView(name string) View
	GetPrimaryView() View
	CanDelete(view View) bool
	DeleteView(view View) error
	AddView(view View)
	GetAllItems() []TopLevelItem
}

// JenkinsInstance는 Jenkins의 루트 ViewGroup을 시뮬레이션한다.
// Jenkins 클래스 자체가 ViewGroup을 구현하며, 모든 작업과 뷰를 포함한다.
type JenkinsInstance struct {
	Name        string
	Views       []View
	PrimaryView string           // 기본 뷰 이름
	Items       []TopLevelItem   // 모든 작업
	Executors   []Executor       // 모든 실행자
	QueueItems  []QueueItem      // 대기열
}

// NewJenkinsInstance는 새 Jenkins 인스턴스를 생성한다.
func NewJenkinsInstance(name string) *JenkinsInstance {
	return &JenkinsInstance{
		Name: name,
	}
}

func (j *JenkinsInstance) GetViews() []View       { return j.Views }
func (j *JenkinsInstance) GetPrimaryView() View {
	for _, v := range j.Views {
		if v.GetName() == j.PrimaryView {
			return v
		}
	}
	if len(j.Views) > 0 {
		return j.Views[0]
	}
	return nil
}

func (j *JenkinsInstance) GetView(name string) View {
	for _, v := range j.Views {
		if v.GetName() == name {
			return v
		}
	}
	return nil
}

func (j *JenkinsInstance) CanDelete(view View) bool {
	// 기본 뷰는 삭제 불가
	return view.GetName() != j.PrimaryView
}

func (j *JenkinsInstance) DeleteView(view View) error {
	if !j.CanDelete(view) {
		return fmt.Errorf("기본 뷰 '%s'는 삭제할 수 없습니다", view.GetName())
	}
	for i, v := range j.Views {
		if v.GetName() == view.GetName() {
			j.Views = append(j.Views[:i], j.Views[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("뷰 '%s'를 찾을 수 없습니다", view.GetName())
}

func (j *JenkinsInstance) AddView(view View) {
	view.SetOwner(j)
	j.Views = append(j.Views, view)
	if j.PrimaryView == "" {
		j.PrimaryView = view.GetName()
	}
}

func (j *JenkinsInstance) GetAllItems() []TopLevelItem {
	return j.Items
}

// AddItem은 작업을 추가한다.
func (j *JenkinsInstance) AddItem(item TopLevelItem) {
	j.Items = append(j.Items, item)
}

// =============================================================================
// 10. 출력 헬퍼
// =============================================================================

func printSeparator(title string) {
	fmt.Println()
	fmt.Printf("========== %s ==========\n", title)
}

func printSubSeparator(title string) {
	fmt.Printf("\n--- %s ---\n", title)
}

// printViewItems는 뷰의 항목을 테이블 형식으로 출력한다.
func printViewItems(v View) {
	items := v.GetItems()
	if len(items) == 0 {
		fmt.Println("  (항목 없음)")
		return
	}

	fmt.Printf("  %-20s %-10s %-8s %-8s %s\n", "이름", "상태", "최근결과", "건강", "설명")
	fmt.Printf("  %-20s %-10s %-8s %-8s %s\n",
		strings.Repeat("-", 20), strings.Repeat("-", 10),
		strings.Repeat("-", 8), strings.Repeat("-", 8), strings.Repeat("-", 20))

	for _, item := range items {
		if job, ok := item.(*Job); ok {
			status := "활성"
			if job.Disabled {
				status = "비활성"
			}

			lastResult := "-"
			if len(job.Builds) > 0 {
				lastResult = job.Builds[0].Result.String()
			}

			hr := job.GetBuildStabilityHealthReport()
			healthStr := "-"
			if hr != nil {
				healthStr = fmt.Sprintf("%s %d%%", hr.WeatherEmoji(), hr.Score)
			}

			fmt.Printf("  %-20s %-10s %-8s %-16s %s\n",
				job.Name, status, lastResult, healthStr, job.Description)
		}
	}
}

// =============================================================================
// 11. 메인: 데모 시나리오
// =============================================================================

func main() {
	fmt.Println("=======================================================")
	fmt.Println(" Jenkins View 시스템 시뮬레이션")
	fmt.Println(" (poc-14-view-system)")
	fmt.Println("=======================================================")
	fmt.Println()
	fmt.Println("참조: core/src/main/java/hudson/model/View.java")
	fmt.Println("      core/src/main/java/hudson/model/AllView.java")
	fmt.Println("      core/src/main/java/hudson/model/ListView.java")
	fmt.Println("      core/src/main/java/hudson/model/ViewGroup.java")
	fmt.Println("      core/src/main/java/hudson/model/HealthReport.java")

	// -------------------------------------------------------------------------
	// 데모 1: Jenkins 인스턴스 및 작업 생성
	// -------------------------------------------------------------------------
	printSeparator("1. Jenkins 인스턴스 및 작업(Job) 생성")

	jenkins := NewJenkinsInstance("Jenkins")

	// 다양한 상태의 작업 생성
	now := time.Now()
	jobs := []*Job{
		{
			Name: "frontend-build", FullName: "frontend-build",
			Description: "프론트엔드 빌드", Disabled: false,
			Builds: []BuildRecord{
				{Number: 5, Result: SUCCESS, Timestamp: now.Add(-1 * time.Hour), Duration: 3 * time.Minute},
				{Number: 4, Result: SUCCESS, Timestamp: now.Add(-5 * time.Hour), Duration: 4 * time.Minute},
				{Number: 3, Result: SUCCESS, Timestamp: now.Add(-10 * time.Hour), Duration: 3 * time.Minute},
				{Number: 2, Result: SUCCESS, Timestamp: now.Add(-24 * time.Hour), Duration: 5 * time.Minute},
				{Number: 1, Result: SUCCESS, Timestamp: now.Add(-48 * time.Hour), Duration: 4 * time.Minute},
			},
		},
		{
			Name: "backend-build", FullName: "backend-build",
			Description: "백엔드 빌드", Disabled: false,
			Builds: []BuildRecord{
				{Number: 10, Result: SUCCESS, Timestamp: now.Add(-2 * time.Hour), Duration: 5 * time.Minute},
				{Number: 9, Result: FAILURE, Timestamp: now.Add(-6 * time.Hour), Duration: 2 * time.Minute},
				{Number: 8, Result: SUCCESS, Timestamp: now.Add(-12 * time.Hour), Duration: 5 * time.Minute},
				{Number: 7, Result: SUCCESS, Timestamp: now.Add(-24 * time.Hour), Duration: 6 * time.Minute},
				{Number: 6, Result: FAILURE, Timestamp: now.Add(-36 * time.Hour), Duration: 1 * time.Minute},
			},
		},
		{
			Name: "api-test", FullName: "api-test",
			Description: "API 통합 테스트", Disabled: false,
			Builds: []BuildRecord{
				{Number: 20, Result: FAILURE, Timestamp: now.Add(-30 * time.Minute), Duration: 10 * time.Minute},
				{Number: 19, Result: FAILURE, Timestamp: now.Add(-3 * time.Hour), Duration: 8 * time.Minute},
				{Number: 18, Result: FAILURE, Timestamp: now.Add(-8 * time.Hour), Duration: 9 * time.Minute},
				{Number: 17, Result: SUCCESS, Timestamp: now.Add(-16 * time.Hour), Duration: 10 * time.Minute},
				{Number: 16, Result: FAILURE, Timestamp: now.Add(-24 * time.Hour), Duration: 7 * time.Minute},
			},
		},
		{
			Name: "deploy-staging", FullName: "deploy-staging",
			Description: "스테이징 배포", Disabled: false,
			Builds: []BuildRecord{
				{Number: 3, Result: SUCCESS, Timestamp: now.Add(-4 * time.Hour), Duration: 2 * time.Minute},
				{Number: 2, Result: UNSTABLE, Timestamp: now.Add(-12 * time.Hour), Duration: 3 * time.Minute},
				{Number: 1, Result: SUCCESS, Timestamp: now.Add(-36 * time.Hour), Duration: 2 * time.Minute},
			},
		},
		{
			Name: "deploy-prod", FullName: "deploy-prod",
			Description: "프로덕션 배포", Disabled: false,
			Builds: []BuildRecord{
				{Number: 2, Result: SUCCESS, Timestamp: now.Add(-24 * time.Hour), Duration: 3 * time.Minute},
				{Number: 1, Result: SUCCESS, Timestamp: now.Add(-72 * time.Hour), Duration: 4 * time.Minute},
			},
		},
		{
			Name: "nightly-backup", FullName: "nightly-backup",
			Description: "야간 백업", Disabled: false,
			Builds: []BuildRecord{
				{Number: 30, Result: SUCCESS, Timestamp: now.Add(-6 * time.Hour), Duration: 15 * time.Minute},
				{Number: 29, Result: SUCCESS, Timestamp: now.Add(-30 * time.Hour), Duration: 14 * time.Minute},
			},
		},
		{
			Name: "legacy-build", FullName: "legacy-build",
			Description: "레거시 빌드 (중단됨)", Disabled: true,
			Builds: []BuildRecord{
				{Number: 50, Result: FAILURE, Timestamp: now.Add(-720 * time.Hour), Duration: 20 * time.Minute},
			},
		},
		{
			Name: "experimental-feature", FullName: "experimental-feature",
			Description: "실험적 기능 (비활성)", Disabled: true,
			Builds: nil, // 빌드 이력 없음
		},
	}

	// Jenkins 인스턴스에 작업 등록
	for _, job := range jobs {
		jenkins.AddItem(job)
	}

	fmt.Printf("Jenkins 인스턴스: %s\n", jenkins.Name)
	fmt.Printf("등록된 작업 수: %d\n", len(jenkins.Items))
	fmt.Println()

	// 작업 목록 출력
	fmt.Printf("%-22s %-8s %-5s %-8s %s\n", "작업 이름", "상태", "빌드수", "최근결과", "설명")
	fmt.Printf("%-22s %-8s %-5s %-8s %s\n",
		strings.Repeat("-", 22), strings.Repeat("-", 8),
		strings.Repeat("-", 5), strings.Repeat("-", 8), strings.Repeat("-", 16))
	for _, job := range jobs {
		status := "활성"
		if job.Disabled {
			status = "비활성"
		}
		lastResult := "-"
		if len(job.Builds) > 0 {
			lastResult = job.Builds[0].Result.String()
		}
		fmt.Printf("%-22s %-8s %-5d %-8s %s\n",
			job.Name, status, len(job.Builds), lastResult, job.Description)
	}

	// -------------------------------------------------------------------------
	// 데모 2: AllView - 모든 작업 표시
	// -------------------------------------------------------------------------
	printSeparator("2. AllView - 모든 작업 표시")
	fmt.Println()
	fmt.Println("AllView는 ViewGroup의 모든 작업을 무조건 표시하는 기본 뷰이다.")
	fmt.Println("Jenkins: AllView.getItems() -> owner.getItemGroup().getItems()")
	fmt.Println("Jenkins: AllView.contains(item) -> return true (항상)")
	fmt.Println()

	allView := NewAllView(DefaultViewName, jenkins)
	jenkins.AddView(allView)

	fmt.Printf("뷰 이름: %s\n", allView.GetName())
	fmt.Printf("설명:    %s\n", allView.GetDescription())
	fmt.Printf("항목 수: %d\n", len(allView.GetItems()))
	fmt.Println()
	printViewItems(allView)

	// AllView.contains() 테스트
	printSubSeparator("AllView.contains() 테스트")
	fmt.Printf("  contains(frontend-build) = %v  (항상 true)\n", allView.Contains(jobs[0]))
	fmt.Printf("  contains(legacy-build)   = %v  (비활성이어도 true)\n", allView.Contains(jobs[6]))

	// -------------------------------------------------------------------------
	// 데모 3: ListView - 명시적 작업 선택
	// -------------------------------------------------------------------------
	printSeparator("3. ListView - 명시적 작업 선택 (jobNames)")
	fmt.Println()
	fmt.Println("ListView는 jobNames Set에 명시적으로 추가한 작업만 표시한다.")
	fmt.Println("Jenkins: ListView.jobNames (TreeSet, CASE_INSENSITIVE_ORDER)")
	fmt.Println("Jenkins: ListView.add(item) -> jobNames.add(item.getRelativeNameFrom(...))")
	fmt.Println()

	buildView := NewListView("Build Jobs", jenkins)
	jenkins.AddView(buildView)
	buildView.Description = "빌드 관련 작업만 모아놓은 뷰"

	// 빌드 관련 작업만 명시적으로 추가
	buildView.Add(jobs[0]) // frontend-build
	buildView.Add(jobs[1]) // backend-build

	fmt.Printf("뷰 이름: %s\n", buildView.GetName())
	fmt.Printf("설명:    %s\n", buildView.Description)
	fmt.Printf("jobNames: %v\n", getSortedKeys(buildView.JobNames))
	fmt.Printf("항목 수: %d\n", len(buildView.GetItems()))
	fmt.Println()
	printViewItems(buildView)

	// contains() 테스트
	printSubSeparator("ListView.contains() 테스트")
	fmt.Printf("  contains(frontend-build) = %v  (jobNames에 포함)\n", buildView.Contains(jobs[0]))
	fmt.Printf("  contains(api-test)       = %v  (jobNames에 미포함)\n", buildView.Contains(jobs[2]))

	// -------------------------------------------------------------------------
	// 데모 4: ListView - includeRegex 패턴 매칭
	// -------------------------------------------------------------------------
	printSeparator("4. ListView - includeRegex 정규식 패턴 매칭")
	fmt.Println()
	fmt.Println("includeRegex를 설정하면 이름이 패턴에 매칭되는 작업도 자동 포함된다.")
	fmt.Println("Jenkins: ListView.includeRegex / includePattern")
	fmt.Println("Jenkins: if (includePattern != null) { includePattern.matcher(itemName).matches() }")
	fmt.Println()

	deployView := NewListView("Deploy Jobs", jenkins)
	jenkins.AddView(deployView)
	deployView.Description = "배포 관련 작업 (정규식: deploy-.*)"

	// 정규식으로 deploy- 접두사 작업을 자동 포함
	err := deployView.SetIncludeRegex("deploy-.*")
	if err != nil {
		fmt.Printf("정규식 오류: %v\n", err)
		return
	}

	fmt.Printf("뷰 이름:      %s\n", deployView.GetName())
	fmt.Printf("includeRegex: %s\n", deployView.IncludeRegex)
	fmt.Printf("jobNames:     %v (명시적 선택 없음)\n", getSortedKeys(deployView.JobNames))
	fmt.Printf("매칭된 항목:  %d개\n", len(deployView.GetItems()))
	fmt.Println()
	printViewItems(deployView)

	// jobNames + includeRegex 조합
	printSubSeparator("jobNames + includeRegex 조합")
	fmt.Println("  jobNames와 includeRegex를 동시에 사용하면 합집합(OR)으로 동작한다.")

	mixedView := NewListView("Mixed View", jenkins)
	mixedView.Add(jobs[0]) // frontend-build (명시적)
	_ = mixedView.SetIncludeRegex(".*-test$") // api-test (패턴)

	fmt.Printf("  jobNames: %v\n", getSortedKeys(mixedView.JobNames))
	fmt.Printf("  includeRegex: %s\n", mixedView.IncludeRegex)
	fmt.Printf("  결과 항목: ")
	for i, item := range mixedView.GetItems() {
		if i > 0 {
			fmt.Print(", ")
		}
		fmt.Print(item.GetName())
	}
	fmt.Println()

	// -------------------------------------------------------------------------
	// 데모 5: ViewJobFilter 체인
	// -------------------------------------------------------------------------
	printSeparator("5. ViewJobFilter 체인")
	fmt.Println()
	fmt.Println("ListView에 ViewJobFilter를 추가하면 체인으로 순차 적용된다.")
	fmt.Println("Jenkins: for (ViewJobFilter jobFilter : jobFilters) {")
	fmt.Println("             items = jobFilter.filter(items, candidates, this);")
	fmt.Println("         }")
	fmt.Println()

	// 5-1: StatusFilter - 활성 작업만
	printSubSeparator("5-1. StatusFilter(활성만)")
	fmt.Println("  Jenkins: hudson.views.StatusFilter")
	fmt.Println("  statusFilter=true → isDisabled()=false인 작업만 통과")

	activeView := NewListView("Active Only", jenkins)
	// 모든 작업을 포함시킨 뒤 StatusFilter로 필터링
	for _, job := range jobs {
		activeView.Add(job)
	}
	activeView.AddJobFilter(&StatusFilter{StatusFilter: true})

	fmt.Printf("  jobNames: 전체 %d개\n", len(activeView.JobNames))
	fmt.Printf("  필터: %s\n", activeView.JobFilters[0].GetName())
	fmt.Printf("  결과 항목: %d개 (비활성 제외)\n", len(activeView.GetItems()))
	fmt.Println()
	printViewItems(activeView)

	// 5-2: StatusFilter - 비활성 작업만
	printSubSeparator("5-2. StatusFilter(비활성만)")

	disabledView := NewListView("Disabled Only", jenkins)
	for _, job := range jobs {
		disabledView.Add(job)
	}
	disabledView.AddJobFilter(&StatusFilter{StatusFilter: false})

	fmt.Printf("  필터: %s\n", disabledView.JobFilters[0].GetName())
	fmt.Printf("  결과 항목: %d개 (활성 제외)\n", len(disabledView.GetItems()))
	fmt.Println()
	printViewItems(disabledView)

	// 5-3: 복합 필터 체인 (StatusFilter + HealthFilter)
	printSubSeparator("5-3. 복합 필터 체인 (StatusFilter + HealthFilter)")
	fmt.Println("  필터 체인은 순차적으로 적용된다.")
	fmt.Println("  StatusFilter(활성만) -> HealthFilter(점수>=60) 순서로 적용")

	healthyActiveView := NewListView("Healthy Active", jenkins)
	for _, job := range jobs {
		healthyActiveView.Add(job)
	}
	healthyActiveView.AddJobFilter(&StatusFilter{StatusFilter: true})  // 활성만
	healthyActiveView.AddJobFilter(&HealthFilter{MinScore: 60})         // 건강 점수 60 이상만

	fmt.Printf("  필터 체인:\n")
	for i, f := range healthyActiveView.JobFilters {
		fmt.Printf("    [%d] %s\n", i+1, f.GetName())
	}
	fmt.Printf("  결과 항목: %d개\n", len(healthyActiveView.GetItems()))
	fmt.Println()
	printViewItems(healthyActiveView)

	// -------------------------------------------------------------------------
	// 데모 6: HealthReport 건강 점수 시각화
	// -------------------------------------------------------------------------
	printSeparator("6. HealthReport - 건강 점수 시각화")
	fmt.Println()
	fmt.Println("Jenkins: hudson.model.HealthReport")
	fmt.Println("Jenkins: Job.getBuildStabilityHealthReport()")
	fmt.Println("  score = 100 * (totalCount - failCount) / totalCount")
	fmt.Println()
	fmt.Println("점수 구간 및 아이콘:")
	fmt.Println("  81~100 [SUNNY]  : icon-health-80plus  (health-80plus.png)")
	fmt.Println("  61~80  [CLOUDY] : icon-health-60to79  (health-60to79.png)")
	fmt.Println("  41~60  [CLOUD]  : icon-health-40to59  (health-40to59.png)")
	fmt.Println("  21~40  [RAIN]   : icon-health-20to39  (health-20to39.png)")
	fmt.Println("   0~20  [STORM]  : icon-health-00to19  (health-00to19.png)")

	printSubSeparator("각 작업의 건강 보고서")
	fmt.Printf("  %-22s %-10s %-7s %-18s %-22s %s\n",
		"작업", "빌드(최근5)", "점수", "아이콘", "CSS클래스", "설명")
	fmt.Printf("  %-22s %-10s %-7s %-18s %-22s %s\n",
		strings.Repeat("-", 22), strings.Repeat("-", 10),
		strings.Repeat("-", 7), strings.Repeat("-", 18),
		strings.Repeat("-", 22), strings.Repeat("-", 20))

	for _, job := range jobs {
		hr := job.GetBuildStabilityHealthReport()
		if hr == nil {
			continue
		}

		// 최근 5개 빌드 결과 요약
		buildSummary := ""
		count := 0
		for i := 0; i < len(job.Builds) && count < 5; i++ {
			b := job.Builds[i]
			switch b.Result {
			case SUCCESS:
				buildSummary += "O"
			case UNSTABLE:
				buildSummary += "U"
			case FAILURE:
				buildSummary += "X"
			case ABORTED:
				buildSummary += "-"
			}
			count++
		}
		if buildSummary == "" {
			buildSummary = "(없음)"
		}

		fmt.Printf("  %-22s %-10s %s %-3d%%  %-18s %-22s %s\n",
			job.Name, buildSummary, hr.WeatherEmoji(), hr.Score,
			hr.IconUrl, hr.IconClassName, hr.Description)
	}

	// -------------------------------------------------------------------------
	// 데모 7: filterExecutors / filterQueue
	// -------------------------------------------------------------------------
	printSeparator("7. filterExecutors / filterQueue 필터링")
	fmt.Println()
	fmt.Println("View의 filterExecutors/filterQueue 옵션으로")
	fmt.Println("뷰에 포함된 작업의 실행자/큐 항목만 보여줄 수 있다.")
	fmt.Println()
	fmt.Println("Jenkins: View.filterExecutors / View.filterQueue")
	fmt.Println("Jenkins: View.getComputers() - filterExecutors=true이면 관련 노드만")
	fmt.Println("Jenkins: View.filterQueue()   - filterQueue=true이면 관련 큐만")

	// 실행자 시뮬레이션
	executors := []Executor{
		{NodeName: "master", Number: 0, JobName: "frontend-build", Idle: false},
		{NodeName: "master", Number: 1, JobName: "", Idle: true},
		{NodeName: "agent-1", Number: 0, JobName: "api-test", Idle: false},
		{NodeName: "agent-1", Number: 1, JobName: "nightly-backup", Idle: false},
		{NodeName: "agent-2", Number: 0, JobName: "", Idle: true},
		{NodeName: "agent-2", Number: 1, JobName: "deploy-staging", Idle: false},
	}

	// 큐 시뮬레이션
	queueItems := []QueueItem{
		{ID: 1, JobName: "backend-build", EnqueueTime: now, Why: "이전 빌드 완료 대기"},
		{ID: 2, JobName: "api-test", EnqueueTime: now, Why: "실행자 대기"},
		{ID: 3, JobName: "deploy-prod", EnqueueTime: now, Why: "수동 승인 대기"},
		{ID: 4, JobName: "nightly-backup", EnqueueTime: now, Why: "스케줄 대기"},
	}

	// 7-1: filterExecutors=false (기본값)
	printSubSeparator("7-1. Build Jobs 뷰 - filterExecutors=false (기본)")
	fmt.Printf("  뷰 항목: %v\n", getItemNames(buildView))
	allExecs := GetComputers(buildView, executors)
	fmt.Printf("  실행자 표시: %d개 (전체)\n", len(allExecs))
	for _, e := range allExecs {
		status := "유휴"
		if !e.Idle {
			status = fmt.Sprintf("실행중: %s", e.JobName)
		}
		fmt.Printf("    %s#%d: %s\n", e.NodeName, e.Number, status)
	}

	// 7-2: filterExecutors=true
	printSubSeparator("7-2. Build Jobs 뷰 - filterExecutors=true")
	buildView.FilterExecutors = true
	filteredExecs := GetComputers(buildView, executors)
	fmt.Printf("  뷰 항목: %v\n", getItemNames(buildView))
	fmt.Printf("  실행자 표시: %d개 (뷰 관련만 + 유휴)\n", len(filteredExecs))
	for _, e := range filteredExecs {
		status := "유휴"
		if !e.Idle {
			status = fmt.Sprintf("실행중: %s", e.JobName)
		}
		fmt.Printf("    %s#%d: %s\n", e.NodeName, e.Number, status)
	}
	buildView.FilterExecutors = false // 원복

	// 7-3: filterQueue 필터링
	printSubSeparator("7-3. Build Jobs 뷰 - filterQueue 필터링")
	fmt.Printf("  전체 큐: %d개\n", len(queueItems))
	for _, qi := range queueItems {
		fmt.Printf("    큐#%d: %s (%s)\n", qi.ID, qi.JobName, qi.Why)
	}

	buildView.FilterQueue = true
	filteredQueue := GetQueueItems(buildView, queueItems)
	fmt.Printf("\n  filterQueue=true 적용 후: %d개 (뷰 관련만)\n", len(filteredQueue))
	for _, qi := range filteredQueue {
		fmt.Printf("    큐#%d: %s (%s)\n", qi.ID, qi.JobName, qi.Why)
	}
	buildView.FilterQueue = false

	// -------------------------------------------------------------------------
	// 데모 8: ViewGroup 관리
	// -------------------------------------------------------------------------
	printSeparator("8. ViewGroup - 뷰 관리")
	fmt.Println()
	fmt.Println("Jenkins: hudson.model.ViewGroup (interface)")
	fmt.Println("  - getViews(), getView(name), getPrimaryView()")
	fmt.Println("  - canDelete(view), deleteView(view)")
	fmt.Println("  - Jenkins 인스턴스 자체가 ViewGroup 구현")
	fmt.Println()

	fmt.Printf("등록된 뷰 목록 (%d개):\n", len(jenkins.GetViews()))
	for i, v := range jenkins.GetViews() {
		primary := ""
		if v.GetName() == jenkins.PrimaryView {
			primary = " [기본 뷰]"
		}
		itemCount := len(v.GetItems())
		fmt.Printf("  [%d] %-20s (%d개 항목)%s\n", i+1, v.GetName(), itemCount, primary)
	}

	printSubSeparator("getPrimaryView()")
	pv := jenkins.GetPrimaryView()
	fmt.Printf("  기본 뷰: %s\n", pv.GetName())

	printSubSeparator("getView(name)")
	found := jenkins.GetView("Deploy Jobs")
	if found != nil {
		fmt.Printf("  getView(\"Deploy Jobs\") = %s (%d개 항목)\n",
			found.GetName(), len(found.GetItems()))
	}
	notFound := jenkins.GetView("NonExistent")
	fmt.Printf("  getView(\"NonExistent\") = %v\n", notFound)

	printSubSeparator("canDelete() / deleteView()")
	for _, v := range jenkins.GetViews() {
		fmt.Printf("  canDelete(%s) = %v\n", v.GetName(), jenkins.CanDelete(v))
	}

	// 뷰 삭제 시도
	err = jenkins.DeleteView(allView)
	if err != nil {
		fmt.Printf("  deleteView(%s) 실패: %s\n", allView.GetName(), err)
	}
	// 기본 뷰가 아닌 뷰 삭제
	deployViewRef := jenkins.GetView("Deploy Jobs")
	if deployViewRef != nil {
		err = jenkins.DeleteView(deployViewRef)
		if err != nil {
			fmt.Printf("  deleteView(%s) 실패: %s\n", deployViewRef.GetName(), err)
		} else {
			fmt.Printf("  deleteView(%s) 성공\n", "Deploy Jobs")
		}
	}
	fmt.Printf("\n  삭제 후 뷰 수: %d개\n", len(jenkins.GetViews()))

	// -------------------------------------------------------------------------
	// 데모 9: 전체 구조 시각화
	// -------------------------------------------------------------------------
	printSeparator("9. View 시스템 구조 다이어그램")
	fmt.Println()
	fmt.Println(`
  Jenkins View 시스템 아키텍처
  ============================

  +--------------------------------------------------+
  |  JenkinsInstance (ViewGroup 구현)                 |
  |                                                  |
  |  items: [Job, Job, Job, ...]                     |
  |  views: [View, View, View, ...]                  |
  |  primaryView: "all"                              |
  |                                                  |
  |  +--------------------------------------------+  |
  |  |  AllView ("all")                           |  |
  |  |  - getItems() -> owner의 모든 항목         |  |
  |  |  - contains() -> 항상 true                 |  |
  |  +--------------------------------------------+  |
  |                                                  |
  |  +--------------------------------------------+  |
  |  |  ListView ("Build Jobs")                   |  |
  |  |  - jobNames: {frontend-build, backend-build}| |
  |  |  - includeRegex: null                       | |
  |  |  - jobFilters: []                           | |
  |  |  - filterExecutors: false                   | |
  |  |  - filterQueue: false                       | |
  |  |                                             | |
  |  |  getItems() 알고리즘:                       | |
  |  |    1. jobNames 매칭                         | |
  |  |    2. includeRegex 패턴 매칭                | |
  |  |    3. jobFilters 체인 적용                  | |
  |  |    4. 중복 제거                             | |
  |  +--------------------------------------------+  |
  +--------------------------------------------------+

  ViewJobFilter 체인:
  +----------+     +--------------+     +--------------+
  | 초기 목록 | --> | StatusFilter | --> | HealthFilter | --> 최종 목록
  +----------+     +--------------+     +--------------+

  HealthReport 점수 범위:
  |  0%           20%          40%          60%          80%         100% |
  |  [STORM]      |  [RAIN]     |  [CLOUD]    |  [CLOUDY]   |  [SUNNY]  |
  |  health-00to19|  health-20to39  health-40to59  health-60to79  80plus |
`)

	// -------------------------------------------------------------------------
	// 데모 10: 요약
	// -------------------------------------------------------------------------
	printSeparator("10. 요약")
	fmt.Println()
	fmt.Println("Jenkins View 시스템의 핵심:")
	fmt.Println()
	fmt.Println("1. View는 작업(Job) 목록을 보여주는 UI 컴포넌트의 추상 클래스")
	fmt.Println("   - 확장 포인트(ExtensionPoint)로 플러그인에서 커스텀 뷰 가능")
	fmt.Println()
	fmt.Println("2. AllView: owner의 모든 작업을 무조건 표시")
	fmt.Println("   - 설정 변경 불가 (isEditable=false)")
	fmt.Println("   - Jenkins 기본 뷰로 'all' 이름 사용")
	fmt.Println()
	fmt.Println("3. ListView: 3단계로 작업을 선별")
	fmt.Println("   - jobNames: 명시적 이름 지정 (TreeSet, 대소문자 무시)")
	fmt.Println("   - includeRegex: 정규식 패턴 매칭")
	fmt.Println("   - jobFilters: ViewJobFilter 체인으로 필터링")
	fmt.Println()
	fmt.Println("4. ViewGroup: View의 컨테이너 인터페이스")
	fmt.Println("   - Jenkins 인스턴스가 루트 ViewGroup")
	fmt.Println("   - 뷰 추가/삭제/조회 관리")
	fmt.Println()
	fmt.Println("5. HealthReport: 빌드 안정성 기반 0~100 건강 점수")
	fmt.Println("   - 최근 5개 빌드의 성공/실패 비율로 계산")
	fmt.Println("   - 5단계 날씨 아이콘으로 시각화")
	fmt.Println()
	fmt.Println("6. filterExecutors/filterQueue: 뷰 범위 제한")
	fmt.Println("   - 뷰에 포함된 작업의 실행자/큐만 표시")
}

// =============================================================================
// 유틸리티 함수
// =============================================================================

// getSortedKeys는 map의 키를 정렬하여 반환한다.
func getSortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// getItemNames는 뷰의 항목 이름 목록을 반환한다.
func getItemNames(v View) []string {
	var names []string
	for _, item := range v.GetItems() {
		names = append(names, item.GetName())
	}
	sort.Strings(names)
	return names
}
