package main

import (
	"fmt"
	"strings"
)

// =============================================================================
// Grafana 라우트 레지스터 시뮬레이션
//
// Grafana는 pkg/api/api.go의 registerRoutes()에서 모든 HTTP 라우트를 등록한다.
// pkg/api/routing/routing.go의 RouteRegister 인터페이스를 통해
// 계층적 그룹, 미들웨어, 경로 파라미터를 지원한다.
// =============================================================================

// AuthLevel은 인증 레벨을 나타낸다.
type AuthLevel int

const (
	NoAuth          AuthLevel = iota // 인증 불필요
	ReqSignedIn                      // 로그인 필요
	ReqEditor                        // Editor 이상 필요
	ReqOrgAdmin                      // Org Admin 필요
	ReqGrafanaAdmin                  // Grafana Admin 필요
)

func (a AuthLevel) String() string {
	switch a {
	case NoAuth:
		return "NoAuth"
	case ReqSignedIn:
		return "ReqSignedIn"
	case ReqEditor:
		return "ReqEditor"
	case ReqOrgAdmin:
		return "ReqOrgAdmin"
	case ReqGrafanaAdmin:
		return "ReqGrafanaAdmin"
	default:
		return "Unknown"
	}
}

// MiddlewareFunc는 미들웨어 함수이다.
type MiddlewareFunc struct {
	Name  string
	Level AuthLevel
}

func (m MiddlewareFunc) String() string {
	return m.Name
}

// HandlerFunc는 라우트 핸들러 함수이다.
type HandlerFunc struct {
	Name string
}

// Route는 등록된 단일 라우트이다.
type Route struct {
	Method      string
	Pattern     string // 전체 경로 패턴 (e.g., /api/dashboards/:uid)
	Handler     HandlerFunc
	Middlewares []MiddlewareFunc
	AuthLevel   AuthLevel
}

// =============================================================================
// RouteRegister - 라우트 등록 인터페이스
// Grafana: pkg/api/routing/routing.go
// =============================================================================

// RouteRegister는 라우트를 등록하는 인터페이스이다.
type RouteRegister interface {
	Get(pattern string, handler HandlerFunc)
	Post(pattern string, handler HandlerFunc)
	Put(pattern string, handler HandlerFunc)
	Delete(pattern string, handler HandlerFunc)
	Group(prefix string, middlewares ...MiddlewareFunc) RouteRegister
	Routes() []Route
}

// routeRegisterImpl은 RouteRegister의 구현체이다.
type routeRegisterImpl struct {
	prefix      string
	middlewares []MiddlewareFunc
	routes      []Route
	groups      []*routeRegisterImpl
}

// NewRouteRegister는 새 RouteRegister를 생성한다.
func NewRouteRegister() RouteRegister {
	return &routeRegisterImpl{
		prefix:      "",
		middlewares: nil,
	}
}

func (r *routeRegisterImpl) addRoute(method, pattern string, handler HandlerFunc) {
	fullPattern := r.prefix + pattern
	route := Route{
		Method:      method,
		Pattern:     fullPattern,
		Handler:     handler,
		Middlewares: make([]MiddlewareFunc, len(r.middlewares)),
		AuthLevel:   NoAuth,
	}
	copy(route.Middlewares, r.middlewares)

	// 가장 높은 인증 레벨 결정
	for _, mw := range route.Middlewares {
		if mw.Level > route.AuthLevel {
			route.AuthLevel = mw.Level
		}
	}

	r.routes = append(r.routes, route)
}

func (r *routeRegisterImpl) Get(pattern string, handler HandlerFunc) {
	r.addRoute("GET", pattern, handler)
}

func (r *routeRegisterImpl) Post(pattern string, handler HandlerFunc) {
	r.addRoute("POST", pattern, handler)
}

func (r *routeRegisterImpl) Put(pattern string, handler HandlerFunc) {
	r.addRoute("PUT", pattern, handler)
}

func (r *routeRegisterImpl) Delete(pattern string, handler HandlerFunc) {
	r.addRoute("DELETE", pattern, handler)
}

func (r *routeRegisterImpl) Group(prefix string, middlewares ...MiddlewareFunc) RouteRegister {
	group := &routeRegisterImpl{
		prefix:      r.prefix + prefix,
		middlewares: make([]MiddlewareFunc, 0, len(r.middlewares)+len(middlewares)),
	}
	// 부모 미들웨어 상속
	group.middlewares = append(group.middlewares, r.middlewares...)
	// 그룹 미들웨어 추가
	group.middlewares = append(group.middlewares, middlewares...)

	r.groups = append(r.groups, group)
	return group
}

func (r *routeRegisterImpl) Routes() []Route {
	var allRoutes []Route
	allRoutes = append(allRoutes, r.routes...)
	for _, g := range r.groups {
		allRoutes = append(allRoutes, g.Routes()...)
	}
	return allRoutes
}

// =============================================================================
// 라우터 - 경로 매칭
// =============================================================================

// Router는 등록된 라우트에서 요청을 매칭한다.
type Router struct {
	routes []Route
}

func NewRouter(routes []Route) *Router {
	return &Router{routes: routes}
}

// MatchResult는 매칭 결과이다.
type MatchResult struct {
	Matched bool
	Route   Route
	Params  map[string]string
}

// Match는 메서드와 경로를 기반으로 라우트를 매칭한다.
func (r *Router) Match(method, path string) MatchResult {
	for _, route := range r.routes {
		if route.Method != method {
			continue
		}

		params, ok := matchPattern(route.Pattern, path)
		if ok {
			return MatchResult{
				Matched: true,
				Route:   route,
				Params:  params,
			}
		}
	}
	return MatchResult{Matched: false}
}

// matchPattern은 패턴과 경로를 매칭하고 파라미터를 추출한다.
// 패턴: /api/dashboards/:uid → 경로: /api/dashboards/abc123
// 결과: {uid: "abc123"}
func matchPattern(pattern, path string) (map[string]string, bool) {
	patternParts := strings.Split(strings.Trim(pattern, "/"), "/")
	pathParts := strings.Split(strings.Trim(path, "/"), "/")

	// 와일드카드(*) 처리
	hasWildcard := false
	for _, p := range patternParts {
		if p == "*" {
			hasWildcard = true
			break
		}
	}

	if !hasWildcard && len(patternParts) != len(pathParts) {
		return nil, false
	}

	params := make(map[string]string)

	for i, patternPart := range patternParts {
		if patternPart == "*" {
			// 나머지 경로를 와일드카드로 매칭
			params["*"] = strings.Join(pathParts[i:], "/")
			return params, true
		}

		if i >= len(pathParts) {
			return nil, false
		}

		if strings.HasPrefix(patternPart, ":") {
			// 경로 파라미터
			paramName := patternPart[1:]
			params[paramName] = pathParts[i]
		} else if patternPart != pathParts[i] {
			return nil, false
		}
	}

	return params, true
}

// =============================================================================
// 라우트 등록 (Grafana api.go 패턴)
// =============================================================================

func registerRoutes(r RouteRegister) {
	// 미들웨어 정의
	reqSignedIn := MiddlewareFunc{Name: "ReqSignedIn", Level: ReqSignedIn}
	reqEditor := MiddlewareFunc{Name: "ReqEditor", Level: ReqEditor}
	reqOrgAdmin := MiddlewareFunc{Name: "ReqOrgAdmin", Level: ReqOrgAdmin}
	reqGrafanaAdmin := MiddlewareFunc{Name: "ReqGrafanaAdmin", Level: ReqGrafanaAdmin}

	// 공개 API (인증 불필요)
	r.Get("/api/health", HandlerFunc{Name: "GetHealth"})
	r.Get("/api/frontend/settings", HandlerFunc{Name: "GetFrontendSettings"})

	// 인증 필요 API 그룹
	apiRoute := r.Group("/api", reqSignedIn)

	// 대시보드 API
	apiRoute.Get("/dashboards/uid/:uid", HandlerFunc{Name: "GetDashboardByUID"})
	apiRoute.Get("/dashboards/home", HandlerFunc{Name: "GetHomeDashboard"})
	apiRoute.Post("/dashboards/db", HandlerFunc{Name: "PostDashboard"})
	apiRoute.Post("/dashboards/import", HandlerFunc{Name: "ImportDashboard"})

	// 대시보드 편집은 Editor 이상
	dashEditRoute := apiRoute.Group("", reqEditor)
	dashEditRoute.Put("/dashboards/uid/:uid", HandlerFunc{Name: "UpdateDashboard"})
	dashEditRoute.Delete("/dashboards/uid/:uid", HandlerFunc{Name: "DeleteDashboard"})

	// 데이터소스 API
	apiRoute.Get("/datasources", HandlerFunc{Name: "GetDataSources"})
	apiRoute.Get("/datasources/uid/:uid", HandlerFunc{Name: "GetDataSourceByUID"})
	apiRoute.Post("/datasources/proxy/:uid/*", HandlerFunc{Name: "ProxyDataSourceRequest"})

	// 데이터소스 관리는 OrgAdmin 이상
	dsAdminRoute := apiRoute.Group("/datasources", reqOrgAdmin)
	dsAdminRoute.Post("", HandlerFunc{Name: "AddDataSource"})
	dsAdminRoute.Put("/uid/:uid", HandlerFunc{Name: "UpdateDataSource"})
	dsAdminRoute.Delete("/uid/:uid", HandlerFunc{Name: "DeleteDataSource"})

	// 사용자 API
	apiRoute.Get("/users/search", HandlerFunc{Name: "SearchUsers"})
	apiRoute.Get("/user", HandlerFunc{Name: "GetCurrentUser"})
	apiRoute.Put("/user", HandlerFunc{Name: "UpdateCurrentUser"})

	// 폴더 API
	apiRoute.Get("/folders", HandlerFunc{Name: "GetFolders"})
	apiRoute.Get("/folders/:uid", HandlerFunc{Name: "GetFolderByUID"})
	apiRoute.Post("/folders", HandlerFunc{Name: "CreateFolder"})

	// Grafana Admin API (서버 관리자 전용)
	adminRoute := r.Group("/api/admin", reqSignedIn, reqGrafanaAdmin)
	adminRoute.Get("/users", HandlerFunc{Name: "AdminGetUsers"})
	adminRoute.Put("/users/:id/permissions", HandlerFunc{Name: "AdminUpdateUserPermissions"})
	adminRoute.Delete("/users/:id", HandlerFunc{Name: "AdminDeleteUser"})
	adminRoute.Get("/settings", HandlerFunc{Name: "AdminGetSettings"})
	adminRoute.Get("/stats", HandlerFunc{Name: "AdminGetStats"})

	// 알림 API
	alertRoute := apiRoute.Group("/alerting", reqEditor)
	alertRoute.Get("/rules", HandlerFunc{Name: "GetAlertRules"})
	alertRoute.Post("/rules", HandlerFunc{Name: "CreateAlertRule"})
	alertRoute.Put("/rules/:uid", HandlerFunc{Name: "UpdateAlertRule"})
	alertRoute.Delete("/rules/:uid", HandlerFunc{Name: "DeleteAlertRule"})
}

// =============================================================================
// 메인
// =============================================================================

func main() {
	fmt.Println("=== Grafana 라우트 레지스터 시뮬레이션 ===")
	fmt.Println()

	// ─── 라우트 등록 ───
	register := NewRouteRegister()
	registerRoutes(register)

	routes := register.Routes()

	// ─── 등록된 라우트 테이블 ───
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("등록된 라우트 테이블")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	fmt.Printf("\n  %-8s %-40s %-30s %s\n", "Method", "Pattern", "Handler", "Auth Level")
	fmt.Println("  " + strings.Repeat("-", 105))

	for _, route := range routes {
		middlewareNames := make([]string, len(route.Middlewares))
		for i, mw := range route.Middlewares {
			middlewareNames[i] = mw.Name
		}
		fmt.Printf("  %-8s %-40s %-30s %s\n",
			route.Method, route.Pattern, route.Handler.Name, route.AuthLevel)
	}

	fmt.Printf("\n  총 %d개 라우트 등록됨\n", len(routes))

	// ─── 라우트 통계 ───
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("라우트 통계")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	methodCount := make(map[string]int)
	authCount := make(map[AuthLevel]int)
	for _, route := range routes {
		methodCount[route.Method]++
		authCount[route.AuthLevel]++
	}

	fmt.Println("\n  HTTP 메서드별:")
	for _, method := range []string{"GET", "POST", "PUT", "DELETE"} {
		if count, ok := methodCount[method]; ok {
			bar := strings.Repeat("=", count)
			fmt.Printf("    %-8s %s (%d)\n", method, bar, count)
		}
	}

	fmt.Println("\n  인증 레벨별:")
	for _, level := range []AuthLevel{NoAuth, ReqSignedIn, ReqEditor, ReqOrgAdmin, ReqGrafanaAdmin} {
		if count, ok := authCount[level]; ok {
			bar := strings.Repeat("=", count)
			fmt.Printf("    %-18s %s (%d)\n", level, bar, count)
		}
	}

	// ─── 라우트 매칭 테스트 ───
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("라우트 매칭 테스트")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	router := NewRouter(routes)

	testCases := []struct {
		method string
		path   string
		desc   string
	}{
		{"GET", "/api/health", "헬스체크 (공개)"},
		{"GET", "/api/dashboards/uid/abc123", "대시보드 조회"},
		{"POST", "/api/dashboards/db", "대시보드 저장"},
		{"DELETE", "/api/dashboards/uid/abc123", "대시보드 삭제 (Editor 필요)"},
		{"GET", "/api/datasources", "데이터소스 목록"},
		{"POST", "/api/datasources", "데이터소스 추가 (OrgAdmin 필요)"},
		{"GET", "/api/admin/users", "관리자 사용자 목록 (GrafanaAdmin 필요)"},
		{"PUT", "/api/admin/users/42/permissions", "사용자 권한 변경"},
		{"GET", "/api/alerting/rules", "알림 규칙 목록"},
		{"POST", "/api/datasources/proxy/prometheus/api/v1/query", "데이터소스 프록시"},
		{"GET", "/api/folders/folder-uid-1", "폴더 조회"},
		{"GET", "/api/unknown/path", "존재하지 않는 경로"},
		{"PATCH", "/api/dashboards/uid/abc123", "지원하지 않는 메서드"},
	}

	for _, tc := range testCases {
		result := router.Match(tc.method, tc.path)
		fmt.Printf("\n  %s %s (%s)\n", tc.method, tc.path, tc.desc)

		if result.Matched {
			fmt.Printf("    매칭됨: %s\n", result.Route.Handler.Name)
			fmt.Printf("    패턴:   %s\n", result.Route.Pattern)
			fmt.Printf("    인증:   %s\n", result.Route.AuthLevel)
			if len(result.Params) > 0 {
				fmt.Printf("    파라미터: %v\n", result.Params)
			}
			if len(result.Route.Middlewares) > 0 {
				names := make([]string, len(result.Route.Middlewares))
				for i, mw := range result.Route.Middlewares {
					names[i] = mw.Name
				}
				fmt.Printf("    미들웨어: [%s]\n", strings.Join(names, " -> "))
			}
		} else {
			fmt.Println("    매칭되지 않음 → 404 Not Found")
		}
	}

	// ─── 경로 파라미터 추출 데모 ───
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("경로 파라미터 추출 상세")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	paramTests := []struct {
		pattern string
		path    string
	}{
		{"/api/dashboards/uid/:uid", "/api/dashboards/uid/abc123"},
		{"/api/admin/users/:id/permissions", "/api/admin/users/42/permissions"},
		{"/api/alerting/rules/:uid", "/api/alerting/rules/rule-001"},
		{"/api/datasources/proxy/:uid/*", "/api/datasources/proxy/prometheus/api/v1/query_range"},
		{"/api/folders/:uid", "/api/folders/my-folder"},
	}

	for _, pt := range paramTests {
		params, ok := matchPattern(pt.pattern, pt.path)
		if ok {
			fmt.Printf("\n  패턴: %s\n", pt.pattern)
			fmt.Printf("  경로: %s\n", pt.path)
			fmt.Printf("  추출: %v\n", params)
		}
	}

	fmt.Println()
	fmt.Println("=== 라우트 레지스터 시뮬레이션 완료 ===")
}
