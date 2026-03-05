package main

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Grafana HTTP 미들웨어 체인 시뮬레이션
//
// Grafana는 모든 HTTP 요청에 대해 미들웨어 체인을 통과시킨다.
// pkg/middleware/ 의 각 미들웨어가 순서대로 실행되며,
// 요청 전처리 → 핸들러 실행 → 응답 후처리 순으로 동작한다.
// =============================================================================

// Handler는 요청을 처리하는 함수 타입이다.
// Grafana의 web.Handler에 해당한다.
type Handler func(ctx *Context)

// Middleware는 Handler를 감싸서 전/후 처리를 추가하는 함수 타입이다.
// func(next Handler) Handler 시그니처로 체인을 구성한다.
type Middleware func(next Handler) Handler

// Context는 요청 처리에 필요한 모든 상태를 담는 구조체이다.
// Grafana의 models.ReqContext에 해당한다.
type Context struct {
	// 요청 정보
	Method string
	Path   string
	Headers map[string]string

	// 응답 정보
	StatusCode int
	Body       string

	// 인증 정보
	User      *User
	OrgID     int64
	IsSignedIn bool

	// 메타데이터
	RequestID string
	TraceID   string
	StartTime time.Time

	// 실행 추적
	Logger     *RequestLogger
	Aborted    bool
	PanicValue interface{}

	// 미들웨어 실행 순서 추적
	executionTrace []string
}

// User는 인증된 사용자 정보를 담는 구조체이다.
type User struct {
	ID       int64
	Login    string
	Role     string // Viewer, Editor, Admin, GrafanaAdmin
	OrgID    int64
	Teams    []int64
}

// RequestLogger는 요청별 로거이다.
type RequestLogger struct {
	mu      sync.Mutex
	entries []string
}

func (l *RequestLogger) Info(msg string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := fmt.Sprintf("[INFO] "+msg, args...)
	l.entries = append(l.entries, entry)
}

func (l *RequestLogger) Warn(msg string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := fmt.Sprintf("[WARN] "+msg, args...)
	l.entries = append(l.entries, entry)
}

func (l *RequestLogger) Error(msg string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := fmt.Sprintf("[ERROR] "+msg, args...)
	l.entries = append(l.entries, entry)
}

func (l *RequestLogger) Entries() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	result := make([]string, len(l.entries))
	copy(result, l.entries)
	return result
}

// =============================================================================
// 미들웨어 구현
// =============================================================================

// MetricsCollector는 요청 메트릭을 수집한다.
type MetricsCollector struct {
	mu       sync.Mutex
	requests []RequestMetric
}

type RequestMetric struct {
	Method     string
	Path       string
	StatusCode int
	Duration   time.Duration
	RequestID  string
}

func (m *MetricsCollector) Record(metric RequestMetric) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, metric)
}

func (m *MetricsCollector) Summary() []RequestMetric {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]RequestMetric, len(m.requests))
	copy(result, m.requests)
	return result
}

// 1. RequestMetadata 미들웨어 - 요청 ID와 시작 시간을 설정한다.
// Grafana: pkg/middleware/request_metadata.go
func RequestMetadata() Middleware {
	return func(next Handler) Handler {
		return func(ctx *Context) {
			// 요청 ID 생성
			ctx.RequestID = fmt.Sprintf("req-%06x", rand.Intn(0xFFFFFF))
			ctx.StartTime = time.Now()
			ctx.Logger = &RequestLogger{}
			ctx.executionTrace = append(ctx.executionTrace, "-> RequestMetadata (enter)")

			ctx.Logger.Info("Request started: %s %s [%s]", ctx.Method, ctx.Path, ctx.RequestID)

			next(ctx)

			ctx.executionTrace = append(ctx.executionTrace, "<- RequestMetadata (exit)")
		}
	}
}

// 2. Tracing 미들웨어 - 분산 트레이싱 스팬을 생성한다.
// Grafana: pkg/middleware/request_tracing.go (OpenTelemetry 연동)
func Tracing() Middleware {
	return func(next Handler) Handler {
		return func(ctx *Context) {
			ctx.TraceID = fmt.Sprintf("trace-%012x", rand.Int63n(0xFFFFFFFFFFFF))
			ctx.executionTrace = append(ctx.executionTrace, "-> Tracing (enter): span="+ctx.TraceID)

			ctx.Logger.Info("Trace span created: %s", ctx.TraceID)

			next(ctx)

			duration := time.Since(ctx.StartTime)
			ctx.Logger.Info("Trace span ended: %s duration=%v status=%d", ctx.TraceID, duration, ctx.StatusCode)
			ctx.executionTrace = append(ctx.executionTrace, "<- Tracing (exit)")
		}
	}
}

// 3. Metrics 미들웨어 - Prometheus 메트릭을 수집한다.
// Grafana: pkg/middleware/request_metrics.go
func Metrics(collector *MetricsCollector) Middleware {
	return func(next Handler) Handler {
		return func(ctx *Context) {
			ctx.executionTrace = append(ctx.executionTrace, "-> Metrics (enter)")

			next(ctx)

			duration := time.Since(ctx.StartTime)
			collector.Record(RequestMetric{
				Method:     ctx.Method,
				Path:       ctx.Path,
				StatusCode: ctx.StatusCode,
				Duration:   duration,
				RequestID:  ctx.RequestID,
			})
			ctx.Logger.Info("Metric recorded: %s %s -> %d (%v)", ctx.Method, ctx.Path, ctx.StatusCode, duration)
			ctx.executionTrace = append(ctx.executionTrace, "<- Metrics (exit)")
		}
	}
}

// 4. Logger 미들웨어 - 요청/응답을 로깅한다.
// Grafana: pkg/middleware/logger.go
func Logger() Middleware {
	return func(next Handler) Handler {
		return func(ctx *Context) {
			ctx.executionTrace = append(ctx.executionTrace, "-> Logger (enter)")
			ctx.Logger.Info(">>> %s %s", ctx.Method, ctx.Path)

			next(ctx)

			status := ctx.StatusCode
			level := "INFO"
			if status >= 400 && status < 500 {
				level = "WARN"
			} else if status >= 500 {
				level = "ERROR"
			}
			ctx.Logger.Info("[%s] <<< %s %s -> %d", level, ctx.Method, ctx.Path, status)
			ctx.executionTrace = append(ctx.executionTrace, "<- Logger (exit)")
		}
	}
}

// 5. Recovery 미들웨어 - 패닉을 복구하고 500 에러를 반환한다.
// Grafana: pkg/middleware/recovery.go
func Recovery() Middleware {
	return func(next Handler) Handler {
		return func(ctx *Context) {
			ctx.executionTrace = append(ctx.executionTrace, "-> Recovery (enter)")

			defer func() {
				if r := recover(); r != nil {
					ctx.PanicValue = r
					ctx.StatusCode = 500
					ctx.Body = fmt.Sprintf(`{"error":"Internal Server Error","message":"panic recovered: %v"}`, r)
					ctx.Logger.Error("PANIC RECOVERED: %v", r)
					ctx.executionTrace = append(ctx.executionTrace, "!! Recovery: panic caught")
				}
				ctx.executionTrace = append(ctx.executionTrace, "<- Recovery (exit)")
			}()

			next(ctx)
		}
	}
}

// 6. Auth 미들웨어 - 인증을 확인한다.
// Grafana: pkg/middleware/auth.go
func Auth(requiredAuth string) Middleware {
	// 시뮬레이션용 토큰 DB
	tokenDB := map[string]*User{
		"token-admin": {
			ID: 1, Login: "admin", Role: "Admin", OrgID: 1,
			Teams: []int64{1, 2},
		},
		"token-viewer": {
			ID: 2, Login: "viewer", Role: "Viewer", OrgID: 1,
			Teams: []int64{2},
		},
		"token-editor": {
			ID: 3, Login: "editor", Role: "Editor", OrgID: 1,
			Teams: []int64{1},
		},
	}

	return func(next Handler) Handler {
		return func(ctx *Context) {
			ctx.executionTrace = append(ctx.executionTrace, fmt.Sprintf("-> Auth (enter): required=%s", requiredAuth))

			if requiredAuth == "NoAuth" {
				ctx.Logger.Info("Auth: no authentication required")
				next(ctx)
				ctx.executionTrace = append(ctx.executionTrace, "<- Auth (exit)")
				return
			}

			// Authorization 헤더에서 토큰 추출
			token := ctx.Headers["Authorization"]
			if token == "" {
				ctx.StatusCode = 401
				ctx.Body = `{"error":"Unauthorized","message":"missing authentication token"}`
				ctx.Logger.Warn("Auth: no token provided")
				ctx.Aborted = true
				ctx.executionTrace = append(ctx.executionTrace, "<- Auth (exit): DENIED - no token")
				return
			}

			// Bearer 접두사 제거
			token = strings.TrimPrefix(token, "Bearer ")

			user, ok := tokenDB[token]
			if !ok {
				ctx.StatusCode = 401
				ctx.Body = `{"error":"Unauthorized","message":"invalid token"}`
				ctx.Logger.Warn("Auth: invalid token: %s", token)
				ctx.Aborted = true
				ctx.executionTrace = append(ctx.executionTrace, "<- Auth (exit): DENIED - invalid token")
				return
			}

			// GrafanaAdmin 체크
			if requiredAuth == "ReqGrafanaAdmin" && user.Role != "GrafanaAdmin" {
				ctx.StatusCode = 403
				ctx.Body = `{"error":"Forbidden","message":"Grafana admin required"}`
				ctx.Logger.Warn("Auth: GrafanaAdmin required, got %s", user.Role)
				ctx.Aborted = true
				ctx.executionTrace = append(ctx.executionTrace, "<- Auth (exit): DENIED - not GrafanaAdmin")
				return
			}

			ctx.User = user
			ctx.OrgID = user.OrgID
			ctx.IsSignedIn = true
			ctx.Logger.Info("Auth: authenticated as %s (role=%s, org=%d)", user.Login, user.Role, user.OrgID)
			ctx.executionTrace = append(ctx.executionTrace, fmt.Sprintf("   Auth: user=%s role=%s", user.Login, user.Role))

			next(ctx)
			ctx.executionTrace = append(ctx.executionTrace, "<- Auth (exit)")
		}
	}
}

// 7. RBAC 미들웨어 - 역할 기반 접근 제어를 확인한다.
// Grafana: pkg/middleware/authorize.go
func RBAC(action string, scope string) Middleware {
	// 역할별 권한 매핑
	rolePermissions := map[string][]string{
		"Viewer":       {"dashboards:read", "datasources:read", "folders:read"},
		"Editor":       {"dashboards:read", "dashboards:write", "dashboards:create", "datasources:read", "folders:read", "folders:write"},
		"Admin":        {"dashboards:read", "dashboards:write", "dashboards:create", "dashboards:delete", "datasources:read", "datasources:write", "datasources:create", "folders:read", "folders:write", "folders:create", "users:read"},
		"GrafanaAdmin": {"*"},
	}

	return func(next Handler) Handler {
		return func(ctx *Context) {
			ctx.executionTrace = append(ctx.executionTrace, fmt.Sprintf("-> RBAC (enter): action=%s scope=%s", action, scope))

			if !ctx.IsSignedIn || ctx.User == nil {
				ctx.StatusCode = 403
				ctx.Body = `{"error":"Forbidden","message":"not authenticated"}`
				ctx.Aborted = true
				ctx.executionTrace = append(ctx.executionTrace, "<- RBAC (exit): DENIED - not signed in")
				return
			}

			perms, ok := rolePermissions[ctx.User.Role]
			if !ok {
				ctx.StatusCode = 403
				ctx.Body = `{"error":"Forbidden","message":"unknown role"}`
				ctx.Aborted = true
				ctx.executionTrace = append(ctx.executionTrace, "<- RBAC (exit): DENIED - unknown role")
				return
			}

			// 권한 확인
			hasPermission := false
			for _, p := range perms {
				if p == "*" || p == action {
					hasPermission = true
					break
				}
				// 와일드카드 매칭: "dashboards:*" matches "dashboards:read"
				if strings.HasSuffix(p, ":*") {
					prefix := strings.TrimSuffix(p, "*")
					if strings.HasPrefix(action, prefix) {
						hasPermission = true
						break
					}
				}
			}

			if !hasPermission {
				ctx.StatusCode = 403
				ctx.Body = fmt.Sprintf(`{"error":"Forbidden","message":"user %s (role=%s) lacks permission: %s"}`, ctx.User.Login, ctx.User.Role, action)
				ctx.Logger.Warn("RBAC: denied %s for user %s (role=%s)", action, ctx.User.Login, ctx.User.Role)
				ctx.Aborted = true
				ctx.executionTrace = append(ctx.executionTrace, fmt.Sprintf("<- RBAC (exit): DENIED - no %s permission", action))
				return
			}

			ctx.Logger.Info("RBAC: allowed %s on %s for %s", action, scope, ctx.User.Login)
			ctx.executionTrace = append(ctx.executionTrace, fmt.Sprintf("   RBAC: ALLOWED %s on %s", action, scope))

			next(ctx)
			ctx.executionTrace = append(ctx.executionTrace, "<- RBAC (exit)")
		}
	}
}

// =============================================================================
// 체인 빌더
// =============================================================================

// Build는 핸들러에 미들웨어를 역순으로 감싸서 체인을 구성한다.
// Build(handler, m1, m2, m3) → m1(m2(m3(handler)))
// 실행 순서: m1 → m2 → m3 → handler → m3 → m2 → m1
func Build(handler Handler, middlewares ...Middleware) Handler {
	for i := len(middlewares) - 1; i >= 0; i-- {
		handler = middlewares[i](handler)
	}
	return handler
}

// =============================================================================
// 메인
// =============================================================================

func main() {
	fmt.Println("=== Grafana HTTP 미들웨어 체인 시뮬레이션 ===")
	fmt.Println()

	collector := &MetricsCollector{}

	// ─── 시나리오 1: 정상 요청 (Admin 사용자, 대시보드 읽기) ───
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("시나리오 1: Admin 사용자 - 대시보드 읽기 (정상)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	dashboardHandler := func(ctx *Context) {
		ctx.executionTrace = append(ctx.executionTrace, "** Handler: getDashboard")
		ctx.StatusCode = 200
		ctx.Body = `{"dashboard":{"uid":"abc123","title":"System Overview"}}`
		ctx.Logger.Info("Handler: returned dashboard abc123")
	}

	chain1 := Build(dashboardHandler,
		RequestMetadata(),
		Tracing(),
		Metrics(collector),
		Logger(),
		Recovery(),
		Auth("ReqSignedIn"),
		RBAC("dashboards:read", "dashboards:uid:abc123"),
	)

	ctx1 := &Context{
		Method:  "GET",
		Path:    "/api/dashboards/uid/abc123",
		Headers: map[string]string{"Authorization": "Bearer token-admin"},
	}
	chain1(ctx1)
	printResult(ctx1)

	// ─── 시나리오 2: 인증 실패 (토큰 없음) ───
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("시나리오 2: 인증 실패 - 토큰 없음")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	chain2 := Build(dashboardHandler,
		RequestMetadata(),
		Tracing(),
		Metrics(collector),
		Logger(),
		Recovery(),
		Auth("ReqSignedIn"),
		RBAC("dashboards:read", "dashboards:*"),
	)

	ctx2 := &Context{
		Method:  "GET",
		Path:    "/api/dashboards/uid/xyz",
		Headers: map[string]string{},
	}
	chain2(ctx2)
	printResult(ctx2)

	// ─── 시나리오 3: 권한 부족 (Viewer가 대시보드 삭제 시도) ───
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("시나리오 3: 권한 부족 - Viewer가 대시보드 삭제 시도")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	deleteHandler := func(ctx *Context) {
		ctx.executionTrace = append(ctx.executionTrace, "** Handler: deleteDashboard")
		ctx.StatusCode = 200
		ctx.Body = `{"message":"Dashboard deleted"}`
	}

	chain3 := Build(deleteHandler,
		RequestMetadata(),
		Tracing(),
		Metrics(collector),
		Logger(),
		Recovery(),
		Auth("ReqSignedIn"),
		RBAC("dashboards:delete", "dashboards:uid:abc123"),
	)

	ctx3 := &Context{
		Method:  "DELETE",
		Path:    "/api/dashboards/uid/abc123",
		Headers: map[string]string{"Authorization": "Bearer token-viewer"},
	}
	chain3(ctx3)
	printResult(ctx3)

	// ─── 시나리오 4: 패닉 복구 ───
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("시나리오 4: 패닉 복구 - 핸들러에서 패닉 발생")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	panicHandler := func(ctx *Context) {
		ctx.executionTrace = append(ctx.executionTrace, "** Handler: about to panic!")
		// 의도적인 패닉 - nil pointer dereference 시뮬레이션
		panic("nil pointer dereference in dashboard service")
	}

	chain4 := Build(panicHandler,
		RequestMetadata(),
		Tracing(),
		Metrics(collector),
		Logger(),
		Recovery(),
		Auth("ReqSignedIn"),
		RBAC("dashboards:read", "dashboards:*"),
	)

	ctx4 := &Context{
		Method:  "GET",
		Path:    "/api/dashboards/uid/broken",
		Headers: map[string]string{"Authorization": "Bearer token-admin"},
	}
	chain4(ctx4)
	printResult(ctx4)

	// ─── 시나리오 5: 인증 불필요 (공개 API) ───
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("시나리오 5: 인증 불필요 - 헬스체크 엔드포인트")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	healthHandler := func(ctx *Context) {
		ctx.executionTrace = append(ctx.executionTrace, "** Handler: healthCheck")
		ctx.StatusCode = 200
		ctx.Body = `{"status":"ok","database":"ok","version":"10.0.0"}`
	}

	// 공개 API는 Auth("NoAuth")를 사용하고 RBAC를 생략한다
	chain5 := Build(healthHandler,
		RequestMetadata(),
		Tracing(),
		Metrics(collector),
		Logger(),
		Recovery(),
		Auth("NoAuth"),
	)

	ctx5 := &Context{
		Method:  "GET",
		Path:    "/api/health",
		Headers: map[string]string{},
	}
	chain5(ctx5)
	printResult(ctx5)

	// ─── 메트릭 요약 ───
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("수집된 요청 메트릭 요약")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Printf("%-8s %-35s %-8s %-15s %s\n", "Method", "Path", "Status", "Duration", "RequestID")
	fmt.Println(strings.Repeat("-", 90))
	for _, m := range collector.Summary() {
		fmt.Printf("%-8s %-35s %-8d %-15v %s\n", m.Method, m.Path, m.StatusCode, m.Duration, m.RequestID)
	}

	fmt.Println()
	fmt.Println("=== 미들웨어 체인 실행 순서 다이어그램 ===")
	fmt.Println()
	fmt.Println("  요청 →  RequestMetadata → Tracing → Metrics → Logger → Recovery → Auth → RBAC → Handler")
	fmt.Println("  응답 ←  RequestMetadata ← Tracing ← Metrics ← Logger ← Recovery ← Auth ← RBAC ← Handler")
	fmt.Println()
	fmt.Println("  각 미들웨어는 next(ctx)를 호출하기 전에 전처리를,")
	fmt.Println("  next(ctx) 반환 후에 후처리를 수행한다.")
	fmt.Println("  Recovery는 defer/recover로 패닉을 잡아 500 에러로 변환한다.")
}

func printResult(ctx *Context) {
	fmt.Printf("\n  응답 코드: %d\n", ctx.StatusCode)
	fmt.Printf("  응답 본문: %s\n", ctx.Body)

	if ctx.PanicValue != nil {
		fmt.Printf("  패닉 복구: %v\n", ctx.PanicValue)
	}

	fmt.Println("\n  [실행 순서 추적]")
	for _, trace := range ctx.executionTrace {
		fmt.Printf("    %s\n", trace)
	}

	fmt.Println("\n  [로그 엔트리]")
	for _, entry := range ctx.Logger.Entries() {
		fmt.Printf("    %s\n", entry)
	}
}
