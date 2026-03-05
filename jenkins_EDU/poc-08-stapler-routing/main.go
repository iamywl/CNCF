// poc-08-stapler-routing: Jenkins Stapler 웹 프레임워크 URL-to-Object 라우팅 시뮬레이션
//
// Jenkins의 Stapler 프레임워크는 URL 경로의 각 세그먼트를 Java 객체 트리의
// getter 메서드 호출로 변환하여 객체 그래프를 탐색하는 독특한 라우팅 방식을 사용한다.
// 이 PoC는 그 핵심 메커니즘을 Go 표준 라이브러리만으로 재현한다.
//
// 실제 Jenkins 소스 참조:
//   - jenkins.model.Jenkins (Jenkins.java:355)
//     루트 객체. StaplerProxy, StaplerFallback 구현.
//     getItem(name) → TopLevelItem, getView(name) → View,
//     getComputer() → ComputerSet, getDynamic(token) → Action
//   - hudson.model.Job (Job.java:900)
//     getDynamic(token, req, rsp): 토큰을 빌드 번호로 파싱 시도,
//     실패하면 Widget/Permalink 검색, 최종적으로 super.getDynamic 호출
//   - hudson.model.Run (Run.java:2606)
//     getDynamic(token, req, rsp): Action 검색 후 없으면 RedirectUp 반환
//   - hudson.model.View (View.java:605)
//     getDynamic(token): Action URL 이름 매칭
//   - hudson.model.Actionable (Actionable.java:348)
//     getDynamic(token, req, rsp): getAllActions()에서 urlName 매칭
//   - jenkins.security.stapler.DoActionFilter (DoActionFilter.java:50)
//     DO_METHOD_REGEX = "^do[^a-z].*" → do로 시작하는 웹 메서드 필터
//   - jenkins.security.stapler.WebMethodConstants (WebMethodConstants.java:53)
//     WEB_METHOD_PARAMETERS: StaplerRequest, HttpServletRequest 등
//
// Stapler URL 라우팅 우선순위:
//   1. getXxx()        - 파라미터 없는 getter (예: getPluginManager)
//   2. getXxx(String)  - 문자열 파라미터 getter (예: getItem("my-app"))
//   3. getXxx(int)     - 숫자 파라미터 getter (예: getBuildByNumber(42))
//   4. getDynamic(String, ...) - 동적 라우트 (최후 수단 객체 탐색)
//   5. doXxx()         - 액션 메서드 (POST 처리, 예: doBuild, doConfigSubmit)
//   6. 뷰 파일         - Jelly/Groovy 템플릿 (index.jelly, configure.groovy)
//   7. public 필드     - 직접 필드 접근
//
// 실행: go run main.go

package main

import (
	"fmt"
	"strconv"
	"strings"
)

// =============================================================================
// 1. Stapler 라우팅의 핵심 인터페이스
// =============================================================================

// StaplerObject 는 Stapler가 URL 세그먼트를 매핑할 수 있는 객체를 나타낸다.
// Jenkins의 모든 URL-라우팅 가능 객체는 이 패턴을 따른다.
//
// 실제 Jenkins에서는 Java 리플렉션으로 get{Name}(), do{Action}() 메서드를 찾지만,
// Go에서는 인터페이스를 통해 명시적으로 라우팅 동작을 정의한다.
type StaplerObject interface {
	// GetDisplayName 은 이 객체의 표시 이름을 반환한다.
	GetDisplayName() string

	// GetTypeName 은 이 객체의 타입 이름을 반환한다 (Jenkins 클래스명에 대응).
	GetTypeName() string
}

// StaplerGetterNoArg 는 파라미터 없는 getter를 지원하는 객체이다.
// URL 세그먼트가 정확히 getter 이름에 매칭될 때 사용된다.
// 예: /pluginManager → getPluginManager()
//
// 참조: Jenkins.java에서 getPluginManager(), getSecurityRealm() 등
type StaplerGetterNoArg interface {
	// GetChildByName 은 고정 이름으로 자식 객체를 반환한다.
	// 반환값: (자식 객체, 존재 여부)
	GetChildByName(name string) (StaplerObject, bool)

	// ListGetterNames 는 사용 가능한 파라미터 없는 getter 이름 목록을 반환한다.
	ListGetterNames() []string
}

// StaplerGetterString 은 문자열 파라미터 getter를 지원하는 객체이다.
// URL 세그먼트 이름 다음의 토큰을 문자열로 전달한다.
// 예: /job/my-app → getItem("my-app")
//
// 참조: Jenkins.java:3021 getItem(String name)
type StaplerGetterString interface {
	// GetChildByString 은 이름으로 자식 객체를 조회한다.
	// prefix: getter 접두사 (예: "job", "view"), token: 실제 값
	// 반환값: (자식 객체, 존재 여부)
	GetChildByString(prefix string, token string) (StaplerObject, bool)

	// ListStringGetterPrefixes 는 문자열 getter 접두사 목록을 반환한다.
	ListStringGetterPrefixes() []string
}

// StaplerGetterInt 는 숫자 파라미터 getter를 지원하는 객체이다.
// URL 세그먼트를 정수로 파싱하여 전달한다.
// 예: /42 → getBuildByNumber(42)
//
// 참조: Job.java:837 getBuildByNumber(int n)
type StaplerGetterInt interface {
	// GetChildByInt 는 숫자 인덱스로 자식 객체를 조회한다.
	// 반환값: (자식 객체, 존재 여부)
	GetChildByInt(index int) (StaplerObject, bool)
}

// StaplerDynamic 은 getDynamic(String, ...) 메서드를 지원하는 객체이다.
// 위의 getter들이 모두 실패했을 때 최후 수단으로 호출된다.
//
// 참조:
//   - Jenkins.java:4010 getDynamic(String token) → Action urlName 매칭
//   - Job.java:900 getDynamic() → 빌드 번호 파싱, Widget, Permalink 검색
//   - Run.java:2606 getDynamic() → transient Action 검색
//   - View.java:605 getDynamic(String token) → Action 매칭
//   - Actionable.java:364 getDynamic() → getAllActions()에서 urlName 매칭
type StaplerDynamic interface {
	// GetDynamic 은 동적으로 자식 객체를 찾는다.
	// 반환값: (자식 객체, 존재 여부)
	GetDynamic(token string) (StaplerObject, bool)
}

// StaplerAction 은 do{Action}() 메서드를 지원하는 객체이다.
// URL의 마지막 세그먼트가 do{Action} 패턴과 매칭될 때 호출된다.
//
// 참조:
//   - DoActionFilter.java:50 DO_METHOD_REGEX = "^do[^a-z].*"
//   - Jenkins.java:4033 doConfigSubmit(StaplerRequest2 req, StaplerResponse2 rsp)
//   - Job.java:1399 doConfigSubmit(StaplerRequest2 req, StaplerResponse2 rsp)
//   - Run.java:2300 doDoDelete(StaplerRequest2 req, StaplerResponse2 rsp)
type StaplerAction interface {
	// DoAction 은 액션 메서드를 실행한다.
	// actionName: 액션 이름 (do 접두사 제외, 예: "Build", "ConfigSubmit")
	// 반환값: (응답 메시지, 지원 여부)
	DoAction(actionName string) (string, bool)

	// ListActions 는 사용 가능한 do{Action} 이름 목록을 반환한다.
	ListActions() []string
}

// StaplerViewable 은 뷰 렌더링을 지원하는 객체이다.
// Jelly/Groovy 템플릿 탐색을 시뮬레이션한다.
//
// 참조: Jenkins은 클래스 계층을 따라가며 {ClassName}/{viewName}.jelly 파일을 검색한다.
// 예: Run 클래스의 console 뷰 → hudson/model/Run/console.jelly
type StaplerViewable interface {
	// RenderView 는 뷰를 렌더링한다.
	// viewName: 뷰 이름 (예: "index", "configure", "console")
	// 반환값: (렌더링 결과, 존재 여부)
	RenderView(viewName string) (string, bool)

	// ListViews 는 사용 가능한 뷰 이름 목록을 반환한다.
	ListViews() []string
}

// StaplerProxy 는 요청 처리 전 권한 확인을 위한 프록시 패턴이다.
// getTarget()이 null을 반환하면 403 Forbidden이 된다.
//
// 참조: Run.java:2647 getTarget() → SKIP_PERMISSION_CHECK가 아니면 권한 확인
//       Jenkins.java:355 implements StaplerProxy
type StaplerProxy interface {
	// GetTarget 은 실제 처리 대상 객체를 반환한다.
	// 권한 확인 후 자기 자신 또는 nil을 반환한다.
	GetTarget() StaplerObject
}

// StaplerFallback 은 URL 매핑 실패 시 대체 객체를 반환하는 인터페이스이다.
// 모든 getter/action/view가 실패했을 때 마지막으로 시도된다.
//
// 참조: Jenkins.java:5287 getStaplerFallback() → getPrimaryView()
type StaplerFallback interface {
	// GetStaplerFallback 은 대체 객체를 반환한다.
	GetStaplerFallback() StaplerObject
}

// =============================================================================
// 2. 라우팅 결과 타입
// =============================================================================

// RouteResultType 은 라우팅 결과의 종류를 나타낸다.
type RouteResultType int

const (
	RouteObject  RouteResultType = iota // 객체 탐색 성공
	RouteAction                         // do{Action} 메서드 호출
	RouteView                           // 뷰 렌더링
	RouteFail                           // 라우팅 실패 (404)
)

func (r RouteResultType) String() string {
	switch r {
	case RouteObject:
		return "OBJECT"
	case RouteAction:
		return "ACTION"
	case RouteView:
		return "VIEW"
	case RouteFail:
		return "404"
	}
	return "UNKNOWN"
}

// RouteStep 은 라우팅 과정의 한 단계를 기록한다.
type RouteStep struct {
	Segment    string          // URL 세그먼트
	Method     string          // 호출된 메서드 (예: "getItem(String)", "getDynamic")
	Object     StaplerObject   // 결과 객체
	ResultType RouteResultType // 결과 종류
	Message    string          // 추가 메시지
}

// RouteResult 는 전체 라우팅 결과를 나타낸다.
type RouteResult struct {
	URL     string      // 원래 URL
	Steps   []RouteStep // 각 단계별 기록
	Final   StaplerObject
	Success bool
}

// =============================================================================
// 3. Stapler 라우터 (핵심 라우팅 엔진)
// =============================================================================

// StaplerRouter 는 Stapler의 URL-to-Object 라우팅 엔진을 시뮬레이션한다.
//
// 실제 Stapler에서는 org.kohsuke.stapler.Stapler 클래스가 이 역할을 담당한다.
// tryInvoke() 메서드에서 리플렉션으로 getter/action/view를 탐색한다.
// 이 PoC에서는 인터페이스 타입 단언으로 동일한 로직을 구현한다.
type StaplerRouter struct {
	root StaplerObject // Jenkins 인스턴스 (루트 객체)
}

// NewStaplerRouter 는 새로운 라우터를 생성한다.
func NewStaplerRouter(root StaplerObject) *StaplerRouter {
	return &StaplerRouter{root: root}
}

// Route 는 URL 경로를 객체 그래프에서 탐색한다.
//
// Stapler의 실제 라우팅 알고리즘:
//  1. URL을 '/' 기준으로 세그먼트 분할
//  2. 루트 객체(Jenkins)에서 시작
//  3. 각 세그먼트에 대해 우선순위대로 매핑 시도:
//     a. StaplerProxy → getTarget()으로 권한 확인
//     b. getXxx() → 파라미터 없는 getter
//     c. getXxx(String) → 현재 세그먼트가 prefix, 다음 세그먼트가 argument
//     d. getXxx(int) → 세그먼트를 정수로 파싱
//     e. getDynamic(String) → 동적 라우트
//     f. doXxx() → 액션 메서드 (마지막 세그먼트에서만)
//     g. 뷰 파일 탐색 (마지막 세그먼트에서만)
//  4. 매핑된 객체로 이동, 다음 세그먼트 처리
//  5. 모든 세그먼트 소진 → 최종 객체의 index 뷰 렌더링
func (r *StaplerRouter) Route(url string) *RouteResult {
	result := &RouteResult{
		URL:   url,
		Steps: make([]RouteStep, 0),
	}

	// URL 파싱: 빈 세그먼트 제거
	segments := splitURL(url)
	if len(segments) == 0 {
		// 루트 URL → StaplerFallback으로 primaryView 반환
		result.Steps = append(result.Steps, RouteStep{
			Segment:    "/",
			Method:     "root",
			Object:     r.root,
			ResultType: RouteObject,
			Message:    fmt.Sprintf("루트 객체: %s (%s)", r.root.GetDisplayName(), r.root.GetTypeName()),
		})
		result.Final = r.root
		result.Success = true
		return result
	}

	current := r.root
	result.Steps = append(result.Steps, RouteStep{
		Segment:    "/",
		Method:     "root",
		Object:     current,
		ResultType: RouteObject,
		Message:    fmt.Sprintf("루트 객체: %s (%s)", current.GetDisplayName(), current.GetTypeName()),
	})

	i := 0
	for i < len(segments) {
		segment := segments[i]

		// --- StaplerProxy 확인 ---
		// 참조: Run.java:2647 getTarget()
		if proxy, ok := current.(StaplerProxy); ok {
			target := proxy.GetTarget()
			if target == nil {
				result.Steps = append(result.Steps, RouteStep{
					Segment:    segment,
					Method:     "StaplerProxy.getTarget()",
					ResultType: RouteFail,
					Message:    "권한 거부 (getTarget() → null, 403 Forbidden)",
				})
				result.Success = false
				return result
			}
			// 프록시를 통과했으면 target으로 대체 (보통 자기 자신)
			if target != current {
				current = target
			}
		}

		found := false

		// --- 우선순위 1: getXxx() 파라미터 없는 getter ---
		// 예: /pluginManager → getPluginManager()
		if getter, ok := current.(StaplerGetterNoArg); ok {
			if child, exists := getter.GetChildByName(segment); exists {
				result.Steps = append(result.Steps, RouteStep{
					Segment:    segment,
					Method:     fmt.Sprintf("get%s()", capitalize(segment)),
					Object:     child,
					ResultType: RouteObject,
					Message:    fmt.Sprintf("→ %s (%s)", child.GetDisplayName(), child.GetTypeName()),
				})
				current = child
				i++
				found = true
			}
		}

		// --- 우선순위 2: getXxx(String) 문자열 파라미터 getter ---
		// 예: /job/my-app → getItem("my-app") (다음 세그먼트를 인자로 소비)
		// 참조: Jenkins.java:3021 getItem(String name)
		if !found {
			if getter, ok := current.(StaplerGetterString); ok {
				prefixes := getter.ListStringGetterPrefixes()
				for _, prefix := range prefixes {
					if segment == prefix && i+1 < len(segments) {
						nextToken := segments[i+1]
						if child, exists := getter.GetChildByString(prefix, nextToken); exists {
							result.Steps = append(result.Steps, RouteStep{
								Segment:    segment + "/" + nextToken,
								Method:     fmt.Sprintf("get%s(\"%s\")", capitalize(prefix), nextToken),
								Object:     child,
								ResultType: RouteObject,
								Message:    fmt.Sprintf("→ %s (%s)", child.GetDisplayName(), child.GetTypeName()),
							})
							current = child
							i += 2 // prefix + argument 두 세그먼트 소비
							found = true
							break
						}
					}
				}
			}
		}

		// --- 우선순위 3: getXxx(int) 숫자 파라미터 getter ---
		// 예: /42 → getBuildByNumber(42)
		// 참조: Job.java:837 getBuildByNumber(int n)
		// Job.getDynamic에서 먼저 Integer.parseInt(token)을 시도한다 (Job.java:904)
		if !found {
			if num, err := strconv.Atoi(segment); err == nil {
				if getter, ok := current.(StaplerGetterInt); ok {
					if child, exists := getter.GetChildByInt(num); exists {
						result.Steps = append(result.Steps, RouteStep{
							Segment:    segment,
							Method:     fmt.Sprintf("getBuildByNumber(%d)", num),
							Object:     child,
							ResultType: RouteObject,
							Message:    fmt.Sprintf("→ %s (%s)", child.GetDisplayName(), child.GetTypeName()),
						})
						current = child
						i++
						found = true
					}
				}
			}
		}

		// --- 우선순위 4: getDynamic(String, ...) 동적 라우트 ---
		// 참조:
		//   Jenkins.java:4010 → Action urlName 매칭 및 ManagementLink 검색
		//   Actionable.java:364 → getAllActions()에서 urlName 매칭
		//   Job.java:900 → 빌드 번호 시도 → Widget → Permalink → super.getDynamic
		//   Run.java:2625 → transient Action 검색 → RedirectUp
		if !found {
			if dyn, ok := current.(StaplerDynamic); ok {
				if child, exists := dyn.GetDynamic(segment); exists {
					result.Steps = append(result.Steps, RouteStep{
						Segment:    segment,
						Method:     fmt.Sprintf("getDynamic(\"%s\")", segment),
						Object:     child,
						ResultType: RouteObject,
						Message:    fmt.Sprintf("→ %s (%s)", child.GetDisplayName(), child.GetTypeName()),
					})
					current = child
					i++
					found = true
				}
			}
		}

		// --- 우선순위 5: do{Action}() 액션 메서드 (마지막 세그먼트에서만) ---
		// 참조: DoActionFilter.java:50 → DO_METHOD_REGEX = "^do[^a-z].*"
		// 액션 메서드는 반드시 "do"로 시작하고 그 다음이 대문자여야 한다.
		if !found {
			actionName := extractActionName(segment)
			if actionName != "" {
				if action, ok := current.(StaplerAction); ok {
					if msg, supported := action.DoAction(actionName); supported {
						result.Steps = append(result.Steps, RouteStep{
							Segment:    segment,
							Method:     fmt.Sprintf("do%s(req, rsp)", actionName),
							ResultType: RouteAction,
							Message:    msg,
						})
						result.Final = current
						result.Success = true
						return result
					}
				}
			}
		}

		// --- 우선순위 6: 뷰 파일 탐색 (마지막 세그먼트에서만) ---
		// 참조: {ClassName}/{viewName}.jelly 패턴으로 뷰 파일 검색
		// 클래스 계층을 따라가며 뷰를 찾는다.
		if !found {
			if viewable, ok := current.(StaplerViewable); ok {
				if rendered, exists := viewable.RenderView(segment); exists {
					result.Steps = append(result.Steps, RouteStep{
						Segment:    segment,
						Method:     fmt.Sprintf("%s/%s.jelly", current.GetTypeName(), segment),
						ResultType: RouteView,
						Message:    rendered,
					})
					result.Final = current
					result.Success = true
					return result
				}
			}
		}

		// --- 최종 실패: StaplerFallback 시도 ---
		// 참조: Jenkins.java:5287 getStaplerFallback() → getPrimaryView()
		if !found {
			if fb, ok := current.(StaplerFallback); ok {
				fallback := fb.GetStaplerFallback()
				if fallback != nil {
					result.Steps = append(result.Steps, RouteStep{
						Segment:    segment,
						Method:     "StaplerFallback.getStaplerFallback()",
						Object:     fallback,
						ResultType: RouteObject,
						Message:    fmt.Sprintf("폴백 → %s (%s)", fallback.GetDisplayName(), fallback.GetTypeName()),
					})
					current = fallback
					// 세그먼트를 소비하지 않고 다시 시도 (폴백 객체에서 같은 세그먼트를 처리)
					continue
				}
			}
		}

		if !found {
			result.Steps = append(result.Steps, RouteStep{
				Segment:    segment,
				Method:     "N/A",
				ResultType: RouteFail,
				Message:    fmt.Sprintf("'%s' 세그먼트를 처리할 수 없음 (404 Not Found)", segment),
			})
			result.Success = false
			return result
		}
	}

	// 모든 세그먼트 소진 → 최종 객체의 index 뷰 렌더링 시도
	result.Final = current
	result.Success = true
	return result
}

// splitURL 은 URL을 세그먼트로 분할한다.
func splitURL(url string) []string {
	parts := strings.Split(strings.Trim(url, "/"), "/")
	segments := make([]string, 0)
	for _, p := range parts {
		if p != "" {
			segments = append(segments, p)
		}
	}
	return segments
}

// capitalize 은 문자열의 첫 글자를 대문자로 변환한다.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// extractActionName 은 세그먼트에서 do{Action} 패턴의 액션 이름을 추출한다.
// DoActionFilter.java:50의 DO_METHOD_REGEX = "^do[^a-z].*" 패턴을 따른다.
// "build" → "Build", "configSubmit" → "ConfigSubmit"
// "do"만 단독으로 사용되면 웹 메서드로 간주하지 않는다.
func extractActionName(segment string) string {
	// Jenkins의 URL 컨벤션: URL에서는 소문자로 시작
	// 예: /build → doBuild(), /configSubmit → doConfigSubmit()
	if len(segment) == 0 {
		return ""
	}
	return capitalize(segment)
}

// =============================================================================
// 4. Jenkins 객체 그래프 구현
// =============================================================================

// --- Artifact (빌드 아티팩트) ---
// 참조: Run.java:1078 getArtifacts()

// Artifact 는 빌드 아티팩트를 나타낸다.
type Artifact struct {
	Name string
	Path string
	Size int64
}

func (a *Artifact) GetDisplayName() string { return a.Name }
func (a *Artifact) GetTypeName() string    { return "Artifact" }

// --- Executor (실행자) ---
// 참조: Computer.java:941 getExecutors()

// Executor 는 빌드 실행자를 나타낸다.
type Executor struct {
	Number    int
	Busy      bool
	BuildName string
}

func (e *Executor) GetDisplayName() string { return fmt.Sprintf("Executor #%d", e.Number) }
func (e *Executor) GetTypeName() string    { return "Executor" }

func (e *Executor) RenderView(viewName string) (string, bool) {
	views := map[string]string{
		"index": fmt.Sprintf("[Executor #%d 뷰] 상태: busy=%v, 빌드: %s", e.Number, e.Busy, e.BuildName),
	}
	if v, ok := views[viewName]; ok {
		return v, true
	}
	return "", false
}

func (e *Executor) ListViews() []string {
	return []string{"index"}
}

// --- Computer (노드/에이전트) ---
// 참조: Computer.java:173

// Computer 는 Jenkins 노드(에이전트)를 나타낸다.
type Computer struct {
	NodeName  string
	Executors []*Executor
	Online    bool
	Actions   map[string]StaplerObject // Action urlName → Action 객체
}

func (c *Computer) GetDisplayName() string { return c.NodeName }
func (c *Computer) GetTypeName() string    { return "Computer" }

func (c *Computer) GetTarget() StaplerObject {
	// 참조: Computer는 StaplerProxy를 구현한다 (Computer.java:173)
	// 접근 권한 확인을 시뮬레이션 (여기서는 항상 통과)
	return c
}

func (c *Computer) GetChildByInt(index int) (StaplerObject, bool) {
	for _, e := range c.Executors {
		if e.Number == index {
			return e, true
		}
	}
	return nil, false
}

func (c *Computer) GetDynamic(token string) (StaplerObject, bool) {
	if a, ok := c.Actions[token]; ok {
		return a, true
	}
	return nil, false
}

func (c *Computer) DoAction(actionName string) (string, bool) {
	actions := map[string]string{
		"DoDelete": fmt.Sprintf("[ACTION] Computer '%s' 삭제 처리", c.NodeName),
	}
	if msg, ok := actions[actionName]; ok {
		return msg, true
	}
	return "", false
}

func (c *Computer) ListActions() []string {
	return []string{"DoDelete"}
}

func (c *Computer) RenderView(viewName string) (string, bool) {
	views := map[string]string{
		"index":     fmt.Sprintf("[Computer 뷰] %s - 온라인: %v, Executor 수: %d", c.NodeName, c.Online, len(c.Executors)),
		"configure": fmt.Sprintf("[Computer 설정 뷰] %s 노드 설정 폼", c.NodeName),
	}
	if v, ok := views[viewName]; ok {
		return v, true
	}
	return "", false
}

func (c *Computer) ListViews() []string {
	return []string{"index", "configure"}
}

// --- Run (빌드 실행) ---
// 참조: Run.java:170

// Run 은 빌드 실행 인스턴스를 나타낸다.
type Run struct {
	Number    int
	JobName   string
	Result    string // SUCCESS, FAILURE, UNSTABLE, ABORTED
	Artifacts []*Artifact
	Actions   map[string]StaplerObject // Action urlName → Action
}

func (r *Run) GetDisplayName() string { return fmt.Sprintf("%s #%d", r.JobName, r.Number) }
func (r *Run) GetTypeName() string    { return "Run" }

func (r *Run) GetTarget() StaplerObject {
	// 참조: Run.java:2647 getTarget()
	// SKIP_PERMISSION_CHECK가 아니면 Job.READ 권한 확인
	return r
}

// GetChildByName: 아티팩트 접근 등
func (r *Run) GetChildByName(name string) (StaplerObject, bool) {
	if name == "artifact" && len(r.Artifacts) > 0 {
		// 참조: Run.java:1078 getArtifacts() → ArtifactList 반환
		return &ArtifactList{Artifacts: r.Artifacts, BuildName: r.GetDisplayName()}, true
	}
	return nil, false
}

func (r *Run) ListGetterNames() []string {
	return []string{"artifact"}
}

// GetDynamic: Action 검색
// 참조: Run.java:2606 getDynamic() → transient Action 검색
//       Run.java:2625 getDynamicImpl() → 없으면 RedirectUp 반환
func (r *Run) GetDynamic(token string) (StaplerObject, bool) {
	if a, ok := r.Actions[token]; ok {
		return a, true
	}
	return nil, false
}

// DoAction: 빌드 관련 액션
// 참조: Run.java:2300 doDoDelete(), Run.java:2221 doConsoleText()
func (r *Run) DoAction(actionName string) (string, bool) {
	actions := map[string]string{
		"DoDelete":    fmt.Sprintf("[ACTION] 빌드 %s #%d 삭제 처리", r.JobName, r.Number),
		"ConsoleText": fmt.Sprintf("[ACTION] 빌드 %s #%d 콘솔 로그 출력 (text/plain)", r.JobName, r.Number),
	}
	if msg, ok := actions[actionName]; ok {
		return msg, true
	}
	return "", false
}

func (r *Run) ListActions() []string {
	return []string{"DoDelete", "ConsoleText"}
}

// RenderView: 빌드 뷰 렌더링
// 참조: {Run 클래스 계층}/{viewName}.jelly
func (r *Run) RenderView(viewName string) (string, bool) {
	views := map[string]string{
		"index":   fmt.Sprintf("[Run 뷰] %s #%d - 결과: %s", r.JobName, r.Number, r.Result),
		"console": fmt.Sprintf("[Console 뷰] %s #%d 빌드 콘솔 출력\n  > Building...\n  > Tests passed\n  > BUILD %s", r.JobName, r.Number, r.Result),
		"changes": fmt.Sprintf("[Changes 뷰] %s #%d 변경사항 목록", r.JobName, r.Number),
	}
	if v, ok := views[viewName]; ok {
		return v, true
	}
	return "", false
}

func (r *Run) ListViews() []string {
	return []string{"index", "console", "changes"}
}

// --- ArtifactList (아티팩트 목록) ---

// ArtifactList 는 빌드 아티팩트 목록을 나타낸다.
type ArtifactList struct {
	Artifacts []*Artifact
	BuildName string
}

func (al *ArtifactList) GetDisplayName() string { return "Artifacts" }
func (al *ArtifactList) GetTypeName() string    { return "ArtifactList" }

func (al *ArtifactList) GetDynamic(token string) (StaplerObject, bool) {
	for _, a := range al.Artifacts {
		if a.Name == token || a.Path == token {
			return a, true
		}
	}
	return nil, false
}

func (al *ArtifactList) RenderView(viewName string) (string, bool) {
	if viewName == "index" {
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("[Artifacts 뷰] %s 의 아티팩트 목록:\n", al.BuildName))
		for _, a := range al.Artifacts {
			sb.WriteString(fmt.Sprintf("  - %s (%s, %d bytes)\n", a.Name, a.Path, a.Size))
		}
		return sb.String(), true
	}
	return "", false
}

func (al *ArtifactList) ListViews() []string {
	return []string{"index"}
}

// --- Job (프로젝트/잡) ---
// 참조: Job.java

// Job 은 Jenkins 프로젝트(잡)를 나타낸다.
type Job struct {
	Name   string
	Builds map[int]*Run    // 빌드 번호 → Run
	Actions map[string]StaplerObject
}

func (j *Job) GetDisplayName() string { return j.Name }
func (j *Job) GetTypeName() string    { return "Job" }

// GetChildByInt: 빌드 번호로 조회
// 참조: Job.java:837 getBuildByNumber(int n)
func (j *Job) GetChildByInt(index int) (StaplerObject, bool) {
	if build, ok := j.Builds[index]; ok {
		return build, true
	}
	return nil, false
}

// GetDynamic: 빌드 번호 파싱, Widget, Permalink 검색
// 참조: Job.java:900-918
// 실제 코드:
//   try {
//       return getBuildByNumber(Integer.parseInt(token));
//   } catch (NumberFormatException e) {
//       for (Widget w : getWidgets()) { ... }
//       for (Permalink p : getPermalinks()) { ... }
//       return super.getDynamic(token, req, rsp);
//   }
func (j *Job) GetDynamic(token string) (StaplerObject, bool) {
	// 1. 빌드 번호로 파싱 시도 (Job.java:904)
	if num, err := strconv.Atoi(token); err == nil {
		if build, ok := j.Builds[num]; ok {
			return build, true
		}
	}
	// 2. Permalink 검색 (lastSuccessfulBuild, lastFailedBuild 등)
	permalinks := map[string]func() *Run{
		"lastBuild": func() *Run {
			var maxNum int
			var lastBuild *Run
			for num, build := range j.Builds {
				if num > maxNum {
					maxNum = num
					lastBuild = build
				}
			}
			return lastBuild
		},
		"lastSuccessfulBuild": func() *Run {
			var maxNum int
			var last *Run
			for num, build := range j.Builds {
				if num > maxNum && build.Result == "SUCCESS" {
					maxNum = num
					last = build
				}
			}
			return last
		},
	}
	if resolver, ok := permalinks[token]; ok {
		if build := resolver(); build != nil {
			return build, true
		}
	}
	// 3. Action 검색 (Actionable.java:364)
	if a, ok := j.Actions[token]; ok {
		return a, true
	}
	return nil, false
}

// DoAction: Job 관련 액션
// 참조: Job.java:1399 doConfigSubmit()
//       AbstractProject.java:1734 doBuild()
func (j *Job) DoAction(actionName string) (string, bool) {
	actions := map[string]string{
		"Build":        fmt.Sprintf("[ACTION] Job '%s' 빌드 트리거 → 큐에 추가됨", j.Name),
		"ConfigSubmit": fmt.Sprintf("[ACTION] Job '%s' 설정 저장 (BulkChange 시작 → 저장 → 커밋)", j.Name),
		"DoDelete":     fmt.Sprintf("[ACTION] Job '%s' 삭제 처리", j.Name),
		"Disable":      fmt.Sprintf("[ACTION] Job '%s' 비활성화", j.Name),
		"Enable":       fmt.Sprintf("[ACTION] Job '%s' 활성화", j.Name),
	}
	if msg, ok := actions[actionName]; ok {
		return msg, true
	}
	return "", false
}

func (j *Job) ListActions() []string {
	return []string{"Build", "ConfigSubmit", "DoDelete", "Disable", "Enable"}
}

// RenderView: Job 뷰 렌더링
func (j *Job) RenderView(viewName string) (string, bool) {
	views := map[string]string{
		"index":     fmt.Sprintf("[Job 뷰] %s - 빌드 수: %d", j.Name, len(j.Builds)),
		"configure": fmt.Sprintf("[Job 설정 뷰] %s 프로젝트 설정 폼\n  - SCM, 트리거, 빌드 스텝, 후처리 설정", j.Name),
		"changes":   fmt.Sprintf("[Changes 뷰] %s 최근 변경사항", j.Name),
	}
	if v, ok := views[viewName]; ok {
		return v, true
	}
	return "", false
}

func (j *Job) ListViews() []string {
	return []string{"index", "configure", "changes"}
}

// --- View (뷰) ---
// 참조: View.java:148

// View 는 Jenkins 뷰를 나타낸다.
type View struct {
	Name  string
	Items []string // TopLevelItem 이름 목록
}

func (v *View) GetDisplayName() string { return v.Name }
func (v *View) GetTypeName() string    { return "View" }

// GetDynamic: Action URL 매칭
// 참조: View.java:605 getDynamic(String token)
func (v *View) GetDynamic(token string) (StaplerObject, bool) {
	return nil, false
}

func (v *View) DoAction(actionName string) (string, bool) {
	actions := map[string]string{
		"DoDelete":     fmt.Sprintf("[ACTION] View '%s' 삭제", v.Name),
		"ConfigSubmit": fmt.Sprintf("[ACTION] View '%s' 설정 저장", v.Name),
	}
	if msg, ok := actions[actionName]; ok {
		return msg, true
	}
	return "", false
}

func (v *View) ListActions() []string {
	return []string{"DoDelete", "ConfigSubmit"}
}

func (v *View) RenderView(viewName string) (string, bool) {
	views := map[string]string{
		"index": fmt.Sprintf("[View 뷰] %s - 아이템: %v", v.Name, v.Items),
	}
	if rendered, ok := views[viewName]; ok {
		return rendered, true
	}
	return "", false
}

func (v *View) ListViews() []string {
	return []string{"index"}
}

// --- ActionObject (일반 Action 객체) ---
// Action은 Jenkins의 Actionable에 붙는 확장 포인트이다.
// 참조: hudson.model.Action → getUrlName(), getDisplayName(), getIconFileName()

// ActionObject 는 Jenkins Action을 나타낸다.
type ActionObject struct {
	URLName     string
	DisplayName string
}

func (a *ActionObject) GetDisplayName() string { return a.DisplayName }
func (a *ActionObject) GetTypeName() string    { return "Action" }

func (a *ActionObject) RenderView(viewName string) (string, bool) {
	if viewName == "index" {
		return fmt.Sprintf("[Action 뷰] %s", a.DisplayName), true
	}
	return "", false
}

func (a *ActionObject) ListViews() []string {
	return []string{"index"}
}

// --- PluginManager ---

// PluginManager 는 플러그인 관리자를 나타낸다.
type PluginManager struct {
	Plugins []string
}

func (pm *PluginManager) GetDisplayName() string { return "Plugin Manager" }
func (pm *PluginManager) GetTypeName() string    { return "PluginManager" }

func (pm *PluginManager) RenderView(viewName string) (string, bool) {
	views := map[string]string{
		"index":     fmt.Sprintf("[PluginManager 뷰] 설치된 플러그인: %v", pm.Plugins),
		"installed": fmt.Sprintf("[PluginManager 설치목록] %d개 플러그인 설치됨", len(pm.Plugins)),
		"available": "[PluginManager 사용가능] 업데이트 센터에서 플러그인 검색",
	}
	if v, ok := views[viewName]; ok {
		return v, true
	}
	return "", false
}

func (pm *PluginManager) ListViews() []string {
	return []string{"index", "installed", "available"}
}

// --- ComputerSet ---
// 참조: Jenkins.java:1483 getComputer() → new ComputerSet()

// ComputerSet 은 컴퓨터(노드) 집합을 나타낸다.
type ComputerSet struct {
	Computers map[string]*Computer
}

func (cs *ComputerSet) GetDisplayName() string { return "Nodes" }
func (cs *ComputerSet) GetTypeName() string    { return "ComputerSet" }

func (cs *ComputerSet) GetChildByString(prefix string, token string) (StaplerObject, bool) {
	// /computer/{name} 에서 {name}으로 Computer를 찾음
	// prefix는 사용하지 않음 (ComputerSet 자체가 이미 /computer 아래)
	if c, ok := cs.Computers[token]; ok {
		return c, true
	}
	return nil, false
}

func (cs *ComputerSet) ListStringGetterPrefixes() []string {
	return []string{} // ComputerSet은 직접 자식을 이름으로 찾음
}

// GetDynamic: 이름으로 Computer 검색
func (cs *ComputerSet) GetDynamic(token string) (StaplerObject, bool) {
	if c, ok := cs.Computers[token]; ok {
		return c, true
	}
	return nil, false
}

func (cs *ComputerSet) RenderView(viewName string) (string, bool) {
	if viewName == "index" {
		names := make([]string, 0, len(cs.Computers))
		for name := range cs.Computers {
			names = append(names, name)
		}
		return fmt.Sprintf("[ComputerSet 뷰] 노드 목록: %v", names), true
	}
	return "", false
}

func (cs *ComputerSet) ListViews() []string {
	return []string{"index"}
}

// --- Jenkins (루트 객체) ---
// 참조: Jenkins.java:355

// JenkinsRoot 는 Jenkins 싱글턴 루트 객체를 나타낸다.
type JenkinsRoot struct {
	Items         map[string]*Job        // name → Job (TopLevelItem)
	Views         map[string]*View       // name → View
	ComputerSet   *ComputerSet           // /computer
	PluginManager *PluginManager         // /pluginManager
	PrimaryView   *View                  // StaplerFallback 대상
	Actions       map[string]StaplerObject // Action urlName → Action
}

func (j *JenkinsRoot) GetDisplayName() string { return "Jenkins" }
func (j *JenkinsRoot) GetTypeName() string    { return "Jenkins" }

func (j *JenkinsRoot) GetTarget() StaplerObject {
	// 참조: Jenkins.java에서 StaplerProxy 구현
	// 보안 확인을 시뮬레이션 (여기서는 항상 통과)
	return j
}

// GetChildByName: 파라미터 없는 getter
// 참조: Jenkins.java:1483 getComputer() → ComputerSet
//       Jenkins.java의 각종 getXxx() 메서드
func (j *JenkinsRoot) GetChildByName(name string) (StaplerObject, bool) {
	switch name {
	case "computer":
		return j.ComputerSet, true
	case "pluginManager":
		return j.PluginManager, true
	}
	return nil, false
}

func (j *JenkinsRoot) ListGetterNames() []string {
	return []string{"computer", "pluginManager"}
}

// GetChildByString: 문자열 파라미터 getter
// 참조: Jenkins.java:3021 getItem(String name)
//       Jenkins.java:1866 getView(String name)
func (j *JenkinsRoot) GetChildByString(prefix string, token string) (StaplerObject, bool) {
	switch prefix {
	case "job":
		if item, ok := j.Items[token]; ok {
			return item, true
		}
	case "view":
		if view, ok := j.Views[token]; ok {
			return view, true
		}
	}
	return nil, false
}

func (j *JenkinsRoot) ListStringGetterPrefixes() []string {
	return []string{"job", "view"}
}

// GetDynamic: Action 및 ManagementLink 검색
// 참조: Jenkins.java:4010-4021
//   for (Action a : getActions()) {
//       String url = a.getUrlName();
//       if (url.equals(token) || url.equals('/' + token))
//           return a;
//   }
//   for (Action a : getManagementLinks())
//       if (Objects.equals(a.getUrlName(), token))
//           return a;
func (j *JenkinsRoot) GetDynamic(token string) (StaplerObject, bool) {
	if a, ok := j.Actions[token]; ok {
		return a, true
	}
	return nil, false
}

// DoAction: Jenkins 전역 액션
// 참조: Jenkins.java:4033 doConfigSubmit()
func (j *JenkinsRoot) DoAction(actionName string) (string, bool) {
	actions := map[string]string{
		"ConfigSubmit": "[ACTION] Jenkins 전역 설정 저장 (시스템 메시지, 실행자 수 등)",
		"Reload":       "[ACTION] Jenkins 설정 디스크에서 다시 로드",
		"Restart":      "[ACTION] Jenkins 안전 재시작 스케줄링",
		"QuietDown":    "[ACTION] 새 빌드 수락 중지 (Quiet Down 모드)",
	}
	if msg, ok := actions[actionName]; ok {
		return msg, true
	}
	return "", false
}

func (j *JenkinsRoot) ListActions() []string {
	return []string{"ConfigSubmit", "Reload", "Restart", "QuietDown"}
}

// GetStaplerFallback: URL 매핑 실패 시 primaryView 반환
// 참조: Jenkins.java:5287 getStaplerFallback() → getPrimaryView()
func (j *JenkinsRoot) GetStaplerFallback() StaplerObject {
	if j.PrimaryView != nil {
		return j.PrimaryView
	}
	return nil
}

// RenderView: Jenkins 뷰 렌더링
func (j *JenkinsRoot) RenderView(viewName string) (string, bool) {
	views := map[string]string{
		"index":     fmt.Sprintf("[Jenkins 뷰] 대시보드 - Job 수: %d, View 수: %d", len(j.Items), len(j.Views)),
		"configure": "[Jenkins 전역 설정 뷰] 시스템 설정 폼",
		"manage":    "[Jenkins 관리 뷰] 시스템 관리 메뉴",
	}
	if v, ok := views[viewName]; ok {
		return v, true
	}
	return "", false
}

func (j *JenkinsRoot) ListViews() []string {
	return []string{"index", "configure", "manage"}
}

// =============================================================================
// 5. 객체 그래프 구축 (데모용)
// =============================================================================

// buildJenkinsObjectGraph 는 Jenkins 객체 그래프를 구축한다.
// 실제 Jenkins 인스턴스의 계층 구조를 시뮬레이션한다.
//
// 객체 그래프:
//   Jenkins (루트)
//     ├── job/
//     │   ├── my-app (FreeStyleProject)
//     │   │   ├── 41 (Run, FAILURE)
//     │   │   ├── 42 (Run, SUCCESS)
//     │   │   │   ├── artifact/ (ArtifactList)
//     │   │   │   │   └── app.jar (Artifact)
//     │   │   │   └── testReport (Action)
//     │   │   └── 43 (Run, SUCCESS)
//     │   └── backend-api (FreeStyleProject)
//     │       └── 1 (Run, SUCCESS)
//     ├── view/
//     │   ├── all (View)
//     │   └── frontend (View)
//     ├── computer (ComputerSet)
//     │   ├── master (Computer)
//     │   │   ├── Executor #0
//     │   │   └── Executor #1
//     │   └── agent-01 (Computer)
//     │       └── Executor #0
//     ├── pluginManager (PluginManager)
//     └── manage (ManagementLink Action)
func buildJenkinsObjectGraph() *JenkinsRoot {
	// 아티팩트 생성
	appJar := &Artifact{Name: "app.jar", Path: "target/app.jar", Size: 15728640}
	readme := &Artifact{Name: "README.md", Path: "README.md", Size: 2048}

	// Run(빌드) 생성
	run41 := &Run{
		Number:  41,
		JobName: "my-app",
		Result:  "FAILURE",
		Actions: map[string]StaplerObject{
			"testReport": &ActionObject{URLName: "testReport", DisplayName: "Test Report"},
		},
	}
	run42 := &Run{
		Number:    42,
		JobName:   "my-app",
		Result:    "SUCCESS",
		Artifacts: []*Artifact{appJar, readme},
		Actions: map[string]StaplerObject{
			"testReport": &ActionObject{URLName: "testReport", DisplayName: "Test Report"},
			"changes":    &ActionObject{URLName: "changes", DisplayName: "Changes"},
		},
	}
	run43 := &Run{
		Number:  43,
		JobName: "my-app",
		Result:  "SUCCESS",
		Actions: map[string]StaplerObject{
			"testReport": &ActionObject{URLName: "testReport", DisplayName: "Test Report"},
		},
	}
	run1 := &Run{
		Number:  1,
		JobName: "backend-api",
		Result:  "SUCCESS",
		Actions: map[string]StaplerObject{},
	}

	// Job(프로젝트) 생성
	myApp := &Job{
		Name:   "my-app",
		Builds: map[int]*Run{41: run41, 42: run42, 43: run43},
		Actions: map[string]StaplerObject{
			"ws": &ActionObject{URLName: "ws", DisplayName: "Workspace"},
		},
	}
	backendAPI := &Job{
		Name:    "backend-api",
		Builds:  map[int]*Run{1: run1},
		Actions: map[string]StaplerObject{},
	}

	// View 생성
	allView := &View{
		Name:  "all",
		Items: []string{"my-app", "backend-api"},
	}
	frontendView := &View{
		Name:  "frontend",
		Items: []string{"my-app"},
	}

	// Computer(노드) 생성
	masterComp := &Computer{
		NodeName: "master",
		Executors: []*Executor{
			{Number: 0, Busy: true, BuildName: "my-app #43"},
			{Number: 1, Busy: false, BuildName: ""},
		},
		Online:  true,
		Actions: map[string]StaplerObject{},
	}
	agentComp := &Computer{
		NodeName: "agent-01",
		Executors: []*Executor{
			{Number: 0, Busy: false, BuildName: ""},
		},
		Online:  true,
		Actions: map[string]StaplerObject{},
	}

	// ComputerSet 생성
	computerSet := &ComputerSet{
		Computers: map[string]*Computer{
			"master":   masterComp,
			"agent-01": agentComp,
		},
	}

	// PluginManager 생성
	pluginMgr := &PluginManager{
		Plugins: []string{"git", "pipeline", "credentials", "matrix-auth"},
	}

	// Jenkins 루트 생성
	jenkins := &JenkinsRoot{
		Items: map[string]*Job{
			"my-app":      myApp,
			"backend-api": backendAPI,
		},
		Views: map[string]*View{
			"all":      allView,
			"frontend": frontendView,
		},
		ComputerSet:   computerSet,
		PluginManager: pluginMgr,
		PrimaryView:   allView,
		Actions: map[string]StaplerObject{
			"manage": &ActionObject{URLName: "manage", DisplayName: "Manage Jenkins"},
		},
	}

	return jenkins
}

// =============================================================================
// 6. 출력 유틸리티
// =============================================================================

const (
	colorReset  = "\033[0m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorRed    = "\033[31m"
	colorCyan   = "\033[36m"
	colorBold   = "\033[1m"
	colorDim    = "\033[2m"
)

// printRouteResult 는 라우팅 결과를 시각적으로 출력한다.
func printRouteResult(result *RouteResult) {
	fmt.Printf("\n%s%s[URL] %s%s\n", colorBold, colorCyan, result.URL, colorReset)
	fmt.Println(strings.Repeat("-", 70))

	for idx, step := range result.Steps {
		var statusColor string
		var statusIcon string
		switch step.ResultType {
		case RouteObject:
			statusColor = colorGreen
			statusIcon = "O"
		case RouteAction:
			statusColor = colorYellow
			statusIcon = "A"
		case RouteView:
			statusColor = colorCyan
			statusIcon = "V"
		case RouteFail:
			statusColor = colorRed
			statusIcon = "X"
		}

		prefix := "  "
		if idx > 0 {
			prefix = "  " + strings.Repeat("  ", idx-1) + "-> "
		}

		fmt.Printf("%s%s[%s]%s %-20s  %s%-30s%s  %s\n",
			prefix,
			statusColor, statusIcon, colorReset,
			step.Segment,
			colorDim, step.Method, colorReset,
			step.Message,
		)
	}

	if result.Success {
		fmt.Printf("\n  %s=> 결과: 라우팅 성공%s", colorGreen, colorReset)
		if result.Final != nil {
			fmt.Printf(" → %s%s%s (%s)\n",
				colorBold, result.Final.GetDisplayName(), colorReset,
				result.Final.GetTypeName())
		} else {
			fmt.Println()
		}
	} else {
		fmt.Printf("\n  %s=> 결과: 라우팅 실패 (404 Not Found)%s\n", colorRed, colorReset)
	}
}

// printObjectGraph 는 Jenkins 객체 그래프를 시각적으로 출력한다.
func printObjectGraph(jenkins *JenkinsRoot) {
	fmt.Printf("\n%s%s=== Jenkins 객체 그래프 ===%s\n", colorBold, colorCyan, colorReset)
	fmt.Println("실제 Jenkins의 계층적 객체 트리를 시뮬레이션한 구조:")
	fmt.Println()
	fmt.Println("Jenkins (루트, StaplerProxy + StaplerFallback)")
	fmt.Println("  |")

	// Jobs
	fmt.Println("  +-- /job/{name}  [getItem(String)]")
	for name, job := range jenkins.Items {
		fmt.Printf("  |   +-- %s  (Job)\n", name)
		for num, build := range job.Builds {
			artCount := len(build.Artifacts)
			artStr := ""
			if artCount > 0 {
				artStr = fmt.Sprintf(" [%d artifacts]", artCount)
			}
			fmt.Printf("  |   |   +-- #%d  (%s)%s\n", num, build.Result, artStr)
		}
	}
	fmt.Println("  |")

	// Views
	fmt.Println("  +-- /view/{name}  [getView(String)]")
	for name, view := range jenkins.Views {
		fmt.Printf("  |   +-- %s  (아이템: %v)\n", name, view.Items)
	}
	fmt.Println("  |")

	// Computer
	fmt.Println("  +-- /computer  [getComputer()]")
	for name, comp := range jenkins.ComputerSet.Computers {
		fmt.Printf("  |   +-- %s  (온라인: %v, Executor: %d)\n",
			name, comp.Online, len(comp.Executors))
	}
	fmt.Println("  |")

	// PluginManager
	fmt.Printf("  +-- /pluginManager  [getPluginManager()] → %v\n", jenkins.PluginManager.Plugins)
	fmt.Println("  |")

	// Fallback
	fmt.Printf("  +-- StaplerFallback → primaryView: %s\n", jenkins.PrimaryView.Name)
	fmt.Println()
}

// printRoutingRules 는 Stapler 라우팅 우선순위 규칙을 시각적으로 출력한다.
func printRoutingRules() {
	fmt.Printf("\n%s%s=== Stapler URL 라우팅 우선순위 규칙 ===%s\n", colorBold, colorCyan, colorReset)
	fmt.Println("참조: Stapler 프레임워크의 tryInvoke() 메서드")
	fmt.Println()
	fmt.Println("  URL 세그먼트 수신")
	fmt.Println("         |")
	fmt.Println("         v")
	fmt.Println("  +------+------+")
	fmt.Println("  | StaplerProxy |  getTarget() → 권한 확인")
	fmt.Println("  | (Run, Jenkins)|  null이면 403 Forbidden")
	fmt.Println("  +------+------+")
	fmt.Println("         |")
	fmt.Println("         v")
	fmt.Println("  [1] getXxx()           파라미터 없는 getter")
	fmt.Println("      예: /pluginManager  → getPluginManager()")
	fmt.Println("         |")
	fmt.Println("         v (실패)")
	fmt.Println("  [2] getXxx(String)     문자열 파라미터 getter")
	fmt.Println("      예: /job/my-app    → getItem(\"my-app\")")
	fmt.Println("         |")
	fmt.Println("         v (실패)")
	fmt.Println("  [3] getXxx(int)        숫자 파라미터 getter")
	fmt.Println("      예: /42            → getBuildByNumber(42)")
	fmt.Println("         |")
	fmt.Println("         v (실패)")
	fmt.Println("  [4] getDynamic(String)  동적 라우트")
	fmt.Println("      예: Action URL 매칭, Permalink 해석")
	fmt.Println("         |")
	fmt.Println("         v (실패)")
	fmt.Println("  [5] do{Action}()       액션 메서드")
	fmt.Println("      예: /build → doBuild(req, rsp)")
	fmt.Println("      패턴: ^do[^a-z].*  (DoActionFilter)")
	fmt.Println("         |")
	fmt.Println("         v (실패)")
	fmt.Println("  [6] 뷰 파일 탐색       Jelly/Groovy 템플릿")
	fmt.Println("      예: /console → {Run}/console.jelly")
	fmt.Println("      클래스 계층을 따라가며 검색")
	fmt.Println("         |")
	fmt.Println("         v (실패)")
	fmt.Println("  [7] StaplerFallback    대체 객체")
	fmt.Println("      예: Jenkins → getPrimaryView()")
	fmt.Println("         |")
	fmt.Println("         v (실패)")
	fmt.Println("      404 Not Found")
	fmt.Println()
}

// =============================================================================
// 7. 메인: 데모 실행
// =============================================================================

func main() {
	fmt.Println("================================================================")
	fmt.Println("  Jenkins Stapler URL-to-Object 라우팅 시뮬레이션")
	fmt.Println("  참조: jenkins.model.Jenkins, org.kohsuke.stapler.Stapler")
	fmt.Println("================================================================")

	// --- 라우팅 규칙 시각화 ---
	printRoutingRules()

	// --- 객체 그래프 구축 ---
	jenkins := buildJenkinsObjectGraph()
	printObjectGraph(jenkins)

	// --- 라우터 생성 ---
	router := NewStaplerRouter(jenkins)

	// --- 테스트 URL 목록 ---
	fmt.Printf("%s%s=== URL 라우팅 테스트 ===%s\n", colorBold, colorCyan, colorReset)

	testCases := []struct {
		url         string
		description string
	}{
		// 시나리오 1: 기본 객체 탐색
		{"/job/my-app", "Job 조회 (getItem(String))"},
		{"/job/my-app/42", "빌드 조회 (getItem → getBuildByNumber(int))"},
		{"/job/my-app/42/console", "빌드 콘솔 뷰 (getItem → getBuildByNumber → console.jelly)"},

		// 시나리오 2: 깊은 객체 그래프 탐색
		{"/job/my-app/42/artifact", "아티팩트 목록 (Run.getArtifacts())"},
		{"/job/my-app/42/testReport", "테스트 리포트 Action (getDynamic)"},

		// 시나리오 3: View/Computer 경로
		{"/view/all", "뷰 조회 (getView(String))"},
		{"/computer", "컴퓨터셋 (getComputer(), 파라미터 없는 getter)"},
		{"/computer/master", "마스터 노드 (getComputer → getDynamic)"},
		{"/computer/master/0", "마스터 노드 Executor #0 (Computer → getChildByInt(0))"},

		// 시나리오 4: 파라미터 없는 getter
		{"/pluginManager", "플러그인 매니저 (getPluginManager())"},
		{"/pluginManager/installed", "플러그인 목록 뷰 (installed.jelly)"},

		// 시나리오 5: 액션 메서드 (do{Action})
		{"/job/my-app/build", "빌드 트리거 (doBuild())"},
		{"/job/my-app/configSubmit", "설정 저장 (doConfigSubmit())"},
		{"/job/my-app/42/doDelete", "빌드 삭제 (doDoDelete())"},
		{"/job/my-app/42/consoleText", "콘솔 텍스트 (doConsoleText())"},

		// 시나리오 6: Permalink (getDynamic)
		{"/job/my-app/lastBuild", "최신 빌드 Permalink (getDynamic → Permalink 해석)"},
		{"/job/my-app/lastSuccessfulBuild", "최근 성공 빌드 Permalink"},

		// 시나리오 7: getDynamic으로 Action 찾기
		{"/manage", "Jenkins 관리 (getDynamic → ManagementLink Action)"},
		{"/job/my-app/ws", "워크스페이스 Action (getDynamic → Action urlName)"},

		// 시나리오 8: 뷰 렌더링
		{"/job/my-app/configure", "Job 설정 뷰 (configure.jelly)"},
		{"/job/my-app/42/changes", "빌드 변경사항 뷰 (changes.jelly)"},

		// 시나리오 9: 실패 케이스
		{"/job/nonexistent", "존재하지 않는 Job (404)"},
		{"/job/my-app/999", "존재하지 않는 빌드 번호 (404)"},
		{"/unknown/path", "알 수 없는 경로 (404)"},
	}

	for _, tc := range testCases {
		fmt.Printf("\n%s--- %s ---%s", colorDim, tc.description, colorReset)
		result := router.Route(tc.url)
		printRouteResult(result)
	}

	// --- 실제 Jenkins URL 매핑 요약 ---
	fmt.Printf("\n\n%s%s=== 실제 Jenkins URL 매핑 주요 경로 요약 ===%s\n", colorBold, colorCyan, colorReset)
	fmt.Println()
	fmt.Println("  URL 패턴                              매핑 메서드/객체")
	fmt.Println("  ─────────────────────────────────────  ──────────────────────────────────")
	fmt.Println("  /                                      Jenkins (루트, → StaplerFallback → primaryView)")
	fmt.Println("  /job/{name}                            Jenkins.getItem(name) → TopLevelItem")
	fmt.Println("  /job/{name}/{number}                   Job.getDynamic(number) → Run")
	fmt.Println("  /job/{name}/{number}/console            Run의 console.jelly 뷰")
	fmt.Println("  /job/{name}/{number}/artifact           Run.getArtifacts() → ArtifactList")
	fmt.Println("  /job/{name}/{number}/testReport         Run.getDynamic(\"testReport\") → Action")
	fmt.Println("  /job/{name}/lastBuild                   Job.getDynamic(\"lastBuild\") → Permalink → Run")
	fmt.Println("  /job/{name}/build                       Job.doBuild(req, rsp) → 빌드 트리거")
	fmt.Println("  /job/{name}/configSubmit                Job.doConfigSubmit(req, rsp) → 설정 저장")
	fmt.Println("  /view/{name}                            Jenkins.getView(name) → View")
	fmt.Println("  /computer                               Jenkins.getComputer() → ComputerSet")
	fmt.Println("  /computer/{name}                        ComputerSet에서 Computer 조회")
	fmt.Println("  /computer/{name}/{executorNum}           Computer.getExecutor(num) → Executor")
	fmt.Println("  /pluginManager                          Jenkins.getPluginManager() → PluginManager")
	fmt.Println("  /manage                                 Jenkins.getDynamic(\"manage\") → ManagementLink")
	fmt.Println()

	// --- 핵심 설계 원리 ---
	fmt.Printf("%s%s=== Stapler 설계 핵심 원리 ===%s\n", colorBold, colorCyan, colorReset)
	fmt.Println()
	fmt.Println("  1. URL = 객체 그래프 경로")
	fmt.Println("     - URL의 각 '/'가 getter 메서드 호출에 대응")
	fmt.Println("     - /job/my-app/42/console = Jenkins.getItem(\"my-app\").getBuild(42).console.jelly")
	fmt.Println()
	fmt.Println("  2. Convention over Configuration")
	fmt.Println("     - get{Name}()/do{Action}() 네이밍 규칙만으로 자동 라우팅")
	fmt.Println("     - 별도의 라우트 등록/어노테이션 불필요")
	fmt.Println()
	fmt.Println("  3. 보안이 객체 레벨에 내장")
	fmt.Println("     - StaplerProxy.getTarget()으로 접근 전 권한 확인")
	fmt.Println("     - getter 내부에서 Item.READ/DISCOVER 권한 체크")
	fmt.Println("     - 참조: Jenkins.java:3021 getItem()의 hasPermission() 호출")
	fmt.Println()
	fmt.Println("  4. 확장성: 새 URL = 새 getter 추가")
	fmt.Println("     - 플러그인이 Action을 추가하면 자동으로 URL 노출")
	fmt.Println("     - getDynamic()이 Action의 urlName으로 매칭")
	fmt.Println()
	fmt.Println("  5. StaplerFallback: 우아한 폴백")
	fmt.Println("     - Jenkins 루트 URL(/)에서 매핑 실패 → primaryView로 폴백")
	fmt.Println("     - 참조: Jenkins.java:5287 getStaplerFallback() → getPrimaryView()")
	fmt.Println()
}
