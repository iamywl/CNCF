package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Jaeger 멀티테넌트 트레이스 격리 시뮬레이션
//
// 실제 소스코드 참조:
//   - internal/tenancy/manager.go: Manager, guard, tenantList
//   - internal/tenancy/context.go: WithTenant, GetTenant
//   - internal/tenancy/http.go: ExtractTenantHTTPHandler
//   - internal/tenancy/grpc.go: GetValidTenant, NewGuardingUnaryInterceptor
//   - internal/storage/v2/memory/tenant.go: Tenant (per-tenant storage)
//
// 핵심 설계:
//   1. HTTP 헤더(x-tenant)에서 테넌트 추출
//   2. 화이트리스트 기반 테넌트 검증 (guard)
//   3. context.Context를 통한 테넌트 전파
//   4. 테넌트별 독립된 스토리지 격리
//   5. gRPC metadata를 통한 테넌트 전파
// =============================================================================

// ---------------------------------------------------------------------------
// 테넌트 컨텍스트 전파 (internal/tenancy/context.go)
// ---------------------------------------------------------------------------

// tenantKeyType은 context.Context 관례에 따른 커스텀 키 타입
// 실제 코드: tenantKeyType string, tenantKey = tenantKeyType("tenant")
type tenantKeyType string

const tenantKey = tenantKeyType("tenant")

// WithTenant은 context에 테넌트를 연결
// 실제 코드: context.go L17-L19
func WithTenant(ctx context.Context, tenant string) context.Context {
	return context.WithValue(ctx, tenantKey, tenant)
}

// GetTenant은 context에서 테넌트를 추출
// 실제 코드: context.go L22-L32
func GetTenant(ctx context.Context) string {
	tenant := ctx.Value(tenantKey)
	if tenant == nil {
		return ""
	}
	if s, ok := tenant.(string); ok {
		return s
	}
	return ""
}

// ---------------------------------------------------------------------------
// 테넌트 가드 인터페이스 (internal/tenancy/manager.go)
// ---------------------------------------------------------------------------

// Guard는 테넌트가 유효한지 검증하는 인터페이스
// 실제 코드: manager.go L22-L24
type Guard interface {
	Valid(candidate string) bool
}

// tenantDontCare는 테넌트 검증을 하지 않는 가드 (테넌시 비활성화 또는 화이트리스트 없음)
// 실제 코드: manager.go L43-L47
type tenantDontCare bool

func (tenantDontCare) Valid(string) bool {
	return true
}

// tenantList는 허용된 테넌트 목록으로 검증하는 가드
// 실제 코드: manager.go L49-L56
type tenantList struct {
	tenants map[string]bool
}

func (tl *tenantList) Valid(candidate string) bool {
	_, ok := tl.tenants[candidate]
	return ok
}

func newTenantList(tenants []string) *tenantList {
	tenantMap := make(map[string]bool)
	for _, tenant := range tenants {
		tenantMap[tenant] = true
	}
	return &tenantList{tenants: tenantMap}
}

// ---------------------------------------------------------------------------
// 테넌트 매니저 (internal/tenancy/manager.go)
// ---------------------------------------------------------------------------

// Options는 멀티테넌시 설정
// 실제 코드: manager.go L7-L11
type Options struct {
	Enabled bool
	Header  string
	Tenants []string
}

// Manager는 테넌시를 관리하는 핵심 구조체
// 실제 코드: manager.go L14-L18
type Manager struct {
	Enabled bool
	Header  string
	guard   Guard
}

// NewManager는 테넌시 매니저를 생성
// 실제 코드: manager.go L26-L36
func NewManager(options *Options) *Manager {
	header := options.Header
	if header == "" && options.Enabled {
		header = "x-tenant" // 기본 헤더명
	}

	var g Guard
	// tenancyGuardFactory 로직 (manager.go L69-L80)
	if !options.Enabled || len(options.Tenants) == 0 {
		g = tenantDontCare(true)
	} else {
		g = newTenantList(options.Tenants)
	}

	return &Manager{
		Enabled: options.Enabled,
		Header:  header,
		guard:   g,
	}
}

func (m *Manager) Valid(tenant string) bool {
	return m.guard.Valid(tenant)
}

// ---------------------------------------------------------------------------
// HTTP 미들웨어: 테넌트 추출 (internal/tenancy/http.go)
// ---------------------------------------------------------------------------

// ExtractTenantHTTPHandler는 HTTP 요청에서 테넌트 헤더를 추출하여 context에 전파
// 실제 코드: http.go L16-L38
func ExtractTenantHTTPHandler(tc *Manager, h http.Handler) http.Handler {
	if !tc.Enabled {
		return h
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant := r.Header.Get(tc.Header)
		if tenant == "" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte("missing tenant header"))
			return
		}

		if !tc.Valid(tenant) {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte("unknown tenant"))
			return
		}

		ctx := WithTenant(r.Context(), tenant)
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ---------------------------------------------------------------------------
// gRPC Metadata 전파 시뮬레이션 (internal/tenancy/grpc.go)
// ---------------------------------------------------------------------------

// Metadata는 gRPC metadata를 시뮬레이션
type Metadata map[string][]string

func NewMetadata(pairs ...string) Metadata {
	md := make(Metadata)
	for i := 0; i+1 < len(pairs); i += 2 {
		md[pairs[i]] = append(md[pairs[i]], pairs[i+1])
	}
	return md
}

func (md Metadata) Get(key string) []string {
	return md[strings.ToLower(key)]
}

// metadataKeyType은 context에서 gRPC metadata를 저장하기 위한 키 타입
type metadataKeyType string

const metadataKey = metadataKeyType("grpc-metadata")

func ContextWithMetadata(ctx context.Context, md Metadata) context.Context {
	return context.WithValue(ctx, metadataKey, md)
}

func MetadataFromContext(ctx context.Context) (Metadata, bool) {
	md, ok := ctx.Value(metadataKey).(Metadata)
	return md, ok
}

// GetValidTenant은 다양한 소스에서 테넌트를 추출하고 검증
// 실제 코드: grpc.go L26-L37
func GetValidTenant(ctx context.Context, tm *Manager) (string, error) {
	// 1. context에 직접 연결된 테넌트 확인
	if tenant := GetTenant(ctx); tenant != "" {
		if !tm.Valid(tenant) {
			return "", fmt.Errorf("unknown tenant: %s", tenant)
		}
		return tenant, nil
	}

	// 2. gRPC metadata에서 테넌트 추출 (실제 코드: grpc.go L40-L57)
	md, ok := MetadataFromContext(ctx)
	if !ok {
		return "", fmt.Errorf("missing tenant header")
	}

	tenants := md.Get(tm.Header)
	switch len(tenants) {
	case 0:
		return "", fmt.Errorf("missing tenant header")
	case 1:
		if !tm.Valid(tenants[0]) {
			return "", fmt.Errorf("unknown tenant: %s", tenants[0])
		}
		return tenants[0], nil
	default:
		return "", fmt.Errorf("extra tenant header")
	}
}

// NewGuardingUnaryInterceptor는 gRPC 유나리 인터셉터 시뮬레이션
// 실제 코드: grpc.go L104-L117
func NewGuardingUnaryInterceptor(tc *Manager) func(ctx context.Context, req interface{}) (context.Context, error) {
	return func(ctx context.Context, _ interface{}) (context.Context, error) {
		tenant, err := GetValidTenant(ctx, tc)
		if err != nil {
			return ctx, err
		}

		// "upgrade" 테넌트를 context에 직접 연결
		if GetTenant(ctx) != "" {
			return ctx, nil
		}
		return WithTenant(ctx, tenant), nil
	}
}

// ---------------------------------------------------------------------------
// 테넌트별 격리 스토리지 (internal/storage/v2/memory/tenant.go)
// ---------------------------------------------------------------------------

// Trace는 간단한 트레이스 데이터
type Trace struct {
	TraceID     string
	ServiceName string
	Operation   string
	Duration    time.Duration
	StartTime   time.Time
	Tags        map[string]string
}

// TenantStore는 단일 테넌트의 스토리지
// 실제 코드: memory/tenant.go의 Tenant 구조체 참조
type TenantStore struct {
	mu     sync.RWMutex
	traces map[string]*Trace // traceID → Trace
}

func NewTenantStore() *TenantStore {
	return &TenantStore{
		traces: make(map[string]*Trace),
	}
}

func (ts *TenantStore) WriteTrace(trace *Trace) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.traces[trace.TraceID] = trace
}

func (ts *TenantStore) GetTrace(traceID string) (*Trace, bool) {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	t, ok := ts.traces[traceID]
	return t, ok
}

func (ts *TenantStore) ListTraces() []*Trace {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	result := make([]*Trace, 0, len(ts.traces))
	for _, t := range ts.traces {
		result = append(result, t)
	}
	return result
}

// MultiTenantStorage는 테넌트별 격리된 스토리지 관리
type MultiTenantStorage struct {
	mu      sync.RWMutex
	tenants map[string]*TenantStore
}

func NewMultiTenantStorage() *MultiTenantStorage {
	return &MultiTenantStorage{
		tenants: make(map[string]*TenantStore),
	}
}

func (ms *MultiTenantStorage) GetOrCreateTenantStore(tenant string) *TenantStore {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if store, ok := ms.tenants[tenant]; ok {
		return store
	}
	store := NewTenantStore()
	ms.tenants[tenant] = store
	return store
}

func (ms *MultiTenantStorage) GetTenantStore(tenant string) (*TenantStore, bool) {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	store, ok := ms.tenants[tenant]
	return store, ok
}

// WriteTrace는 context에서 테넌트를 추출하여 해당 테넌트의 스토리지에 기록
func (ms *MultiTenantStorage) WriteTrace(ctx context.Context, trace *Trace) error {
	tenant := GetTenant(ctx)
	if tenant == "" {
		return fmt.Errorf("tenant not found in context")
	}
	store := ms.GetOrCreateTenantStore(tenant)
	store.WriteTrace(trace)
	return nil
}

// GetTrace는 context에서 테넌트를 추출하여 해당 테넌트의 스토리지에서 조회
func (ms *MultiTenantStorage) GetTrace(ctx context.Context, traceID string) (*Trace, error) {
	tenant := GetTenant(ctx)
	if tenant == "" {
		return nil, fmt.Errorf("tenant not found in context")
	}
	store, ok := ms.GetTenantStore(tenant)
	if !ok {
		return nil, fmt.Errorf("no data for tenant: %s", tenant)
	}
	trace, ok := store.GetTrace(traceID)
	if !ok {
		return nil, fmt.Errorf("trace not found: %s", traceID)
	}
	return trace, nil
}

// ---------------------------------------------------------------------------
// 시각화 헬퍼
// ---------------------------------------------------------------------------

func printSeparator(title string) {
	fmt.Println()
	fmt.Println(strings.Repeat("=", 80))
	fmt.Printf(" %s\n", title)
	fmt.Println(strings.Repeat("=", 80))
}

func printSubSeparator(title string) {
	fmt.Printf("\n--- %s ---\n", title)
}

// ---------------------------------------------------------------------------
// 메인: 시뮬레이션 실행
// ---------------------------------------------------------------------------

func main() {
	fmt.Println("Jaeger 멀티테넌트 트레이스 격리 시뮬레이션")
	fmt.Println("참조: internal/tenancy/manager.go, context.go, http.go, grpc.go")
	fmt.Println("참조: internal/storage/v2/memory/tenant.go")

	// =========================================================================
	// 1단계: 테넌트 매니저 설정
	// =========================================================================
	printSeparator("1단계: 테넌트 매니저 설정 및 검증")

	manager := NewManager(&Options{
		Enabled: true,
		Header:  "x-tenant",
		Tenants: []string{"team-alpha", "team-beta", "team-gamma"},
	})

	fmt.Printf("테넌시 활성화: %v\n", manager.Enabled)
	fmt.Printf("테넌트 헤더: %s\n", manager.Header)
	fmt.Println()

	// 화이트리스트 검증 테스트
	testTenants := []string{"team-alpha", "team-beta", "team-gamma", "team-unknown", ""}
	fmt.Printf("%-15s  유효 여부\n", "테넌트")
	fmt.Println(strings.Repeat("-", 30))
	for _, tenant := range testTenants {
		name := tenant
		if name == "" {
			name = "(빈 문자열)"
		}
		fmt.Printf("%-15s  %v\n", name, manager.Valid(tenant))
	}

	// 테넌시 비활성화 시 동작
	printSubSeparator("테넌시 비활성화 시 동작")
	disabledManager := NewManager(&Options{
		Enabled: false,
	})
	fmt.Printf("비활성화 매니저 - 임의 테넌트 검증: %v (tenantDontCare)\n",
		disabledManager.Valid("anyone"))

	// =========================================================================
	// 2단계: HTTP 미들웨어를 통한 테넌트 추출
	// =========================================================================
	printSeparator("2단계: HTTP 미들웨어 테넌트 추출")

	storage := NewMultiTenantStorage()

	// 비즈니스 로직 핸들러
	traceHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant := GetTenant(r.Context())
		traceID := r.URL.Query().Get("trace_id")
		action := r.URL.Query().Get("action")

		if action == "write" {
			trace := &Trace{
				TraceID:     traceID,
				ServiceName: r.URL.Query().Get("service"),
				Operation:   r.URL.Query().Get("operation"),
				Duration:    150 * time.Millisecond,
				StartTime:   time.Now(),
			}
			if err := storage.WriteTrace(r.Context(), trace); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(err.Error()))
				return
			}
			fmt.Fprintf(w, "tenant=%s: trace %s written", tenant, traceID)
		} else {
			trace, err := storage.GetTrace(r.Context(), traceID)
			if err != nil {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(err.Error()))
				return
			}
			fmt.Fprintf(w, "tenant=%s: trace %s, service=%s", tenant, trace.TraceID, trace.ServiceName)
		}
	})

	// 테넌트 미들웨어 적용
	handler := ExtractTenantHTTPHandler(manager, traceHandler)

	// 테스트 케이스
	type httpTestCase struct {
		name       string
		tenant     string
		action     string
		traceID    string
		service    string
		operation  string
		expectCode int
	}

	testCases := []httpTestCase{
		{"헤더 누락", "", "write", "t1", "svc", "op", 401},
		{"허용되지 않은 테넌트", "team-unknown", "write", "t1", "svc", "op", 401},
		{"team-alpha 쓰기", "team-alpha", "write", "trace-001", "api-gateway", "GET /users", 200},
		{"team-alpha 쓰기2", "team-alpha", "write", "trace-002", "user-service", "GetUser", 200},
		{"team-beta 쓰기", "team-beta", "write", "trace-003", "order-service", "CreateOrder", 200},
		{"team-beta 쓰기2", "team-beta", "write", "trace-001", "payment-service", "ProcessPayment", 200},
	}

	fmt.Printf("%-25s %-15s %-12s  응답코드  결과\n", "테스트", "테넌트", "액션")
	fmt.Println(strings.Repeat("-", 80))

	for _, tc := range testCases {
		reqURL := fmt.Sprintf("/api/traces?action=%s&trace_id=%s&service=%s&operation=%s",
			url.QueryEscape(tc.action), url.QueryEscape(tc.traceID),
			url.QueryEscape(tc.service), url.QueryEscape(tc.operation))
		req := httptest.NewRequest("POST", reqURL, nil)
		if tc.tenant != "" {
			req.Header.Set("x-tenant", tc.tenant)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		tenantDisplay := tc.tenant
		if tenantDisplay == "" {
			tenantDisplay = "(없음)"
		}
		fmt.Printf("%-25s %-15s %-12s  %d       %s\n",
			tc.name, tenantDisplay, tc.action, rec.Code, rec.Body.String())
	}

	// =========================================================================
	// 3단계: 테넌트 격리 검증
	// =========================================================================
	printSeparator("3단계: 테넌트 간 데이터 격리 검증")

	fmt.Println()
	fmt.Println("각 테넌트는 독립된 스토리지를 가집니다:")
	fmt.Println("(internal/storage/v2/memory/tenant.go의 Tenant 구조체)")
	fmt.Println()

	// team-alpha의 데이터 조회
	printSubSeparator("team-alpha 스토리지")
	alphaStore, _ := storage.GetTenantStore("team-alpha")
	for _, trace := range alphaStore.ListTraces() {
		fmt.Printf("  TraceID=%s, Service=%s, Operation=%s\n",
			trace.TraceID, trace.ServiceName, trace.Operation)
	}

	// team-beta의 데이터 조회
	printSubSeparator("team-beta 스토리지")
	betaStore, _ := storage.GetTenantStore("team-beta")
	for _, trace := range betaStore.ListTraces() {
		fmt.Printf("  TraceID=%s, Service=%s, Operation=%s\n",
			trace.TraceID, trace.ServiceName, trace.Operation)
	}

	// 교차 접근 시도
	printSubSeparator("교차 접근 시도: team-alpha가 team-beta의 trace-003 조회")
	alphaCtx := WithTenant(context.Background(), "team-alpha")
	_, err := storage.GetTrace(alphaCtx, "trace-003")
	if err != nil {
		fmt.Printf("  오류: %s\n", err)
		fmt.Println("  → 테넌트 격리가 올바르게 동작합니다!")
	}

	// 같은 traceID가 다른 테넌트에 존재
	printSubSeparator("같은 TraceID(trace-001)가 서로 다른 테넌트에 독립 존재")
	alphaTrace, _ := storage.GetTrace(alphaCtx, "trace-001")
	betaCtx := WithTenant(context.Background(), "team-beta")
	betaTrace, _ := storage.GetTrace(betaCtx, "trace-001")
	fmt.Printf("  team-alpha의 trace-001: service=%s\n", alphaTrace.ServiceName)
	fmt.Printf("  team-beta의 trace-001:  service=%s\n", betaTrace.ServiceName)
	fmt.Println("  → 같은 TraceID라도 테넌트별로 완전히 독립된 데이터입니다!")

	// =========================================================================
	// 4단계: gRPC Metadata 전파 시뮬레이션
	// =========================================================================
	printSeparator("4단계: gRPC Metadata 전파 시뮬레이션")

	fmt.Println()
	fmt.Println("Jaeger는 gRPC metadata를 통해서도 테넌트를 전파합니다.")
	fmt.Println("실제 코드: grpc.go의 NewGuardingUnaryInterceptor, NewClientUnaryInterceptor")
	fmt.Println()

	interceptor := NewGuardingUnaryInterceptor(manager)

	// 케이스 1: metadata에 유효한 테넌트
	printSubSeparator("케이스 1: gRPC metadata에 유효한 테넌트")
	md := NewMetadata("x-tenant", "team-gamma")
	ctx1 := ContextWithMetadata(context.Background(), md)
	enrichedCtx, err := interceptor(ctx1, nil)
	if err != nil {
		fmt.Printf("  오류: %s\n", err)
	} else {
		fmt.Printf("  추출된 테넌트: %s\n", GetTenant(enrichedCtx))
		fmt.Println("  → metadata에서 추출되어 context에 직접 연결됨 (upgrade)")
	}

	// 케이스 2: metadata에 허용되지 않은 테넌트
	printSubSeparator("케이스 2: gRPC metadata에 허용되지 않은 테넌트")
	md2 := NewMetadata("x-tenant", "team-hacker")
	ctx2 := ContextWithMetadata(context.Background(), md2)
	_, err = interceptor(ctx2, nil)
	if err != nil {
		fmt.Printf("  오류: %s\n", err)
	}

	// 케이스 3: 이미 context에 직접 연결된 테넌트
	printSubSeparator("케이스 3: context에 직접 연결된 테넌트 (이미 upgrade됨)")
	ctx3 := WithTenant(context.Background(), "team-alpha")
	enrichedCtx3, err := interceptor(ctx3, nil)
	if err != nil {
		fmt.Printf("  오류: %s\n", err)
	} else {
		fmt.Printf("  테넌트: %s (context에서 직접 확인, metadata 검사 건너뜀)\n", GetTenant(enrichedCtx3))
	}

	// 케이스 4: metadata에 중복 테넌트 헤더
	printSubSeparator("케이스 4: gRPC metadata에 중복 테넌트 헤더")
	md4 := Metadata{"x-tenant": {"team-alpha", "team-beta"}}
	ctx4 := ContextWithMetadata(context.Background(), md4)
	_, err = interceptor(ctx4, nil)
	if err != nil {
		fmt.Printf("  오류: %s\n", err)
		fmt.Println("  → 테넌트 헤더는 정확히 1개만 허용됩니다")
	}

	// =========================================================================
	// 5단계: 동시 접근 격리 시연
	// =========================================================================
	printSeparator("5단계: 동시 접근 시 테넌트 격리 시연")

	fmt.Println()
	fmt.Println("여러 테넌트가 동시에 데이터를 쓰고 읽어도 격리가 유지되는지 확인합니다.")
	fmt.Println()

	concurrentStorage := NewMultiTenantStorage()
	var wg sync.WaitGroup
	tenants := []string{"team-alpha", "team-beta", "team-gamma"}

	// 각 테넌트가 동시에 10개씩 트레이스 기록
	for _, tenant := range tenants {
		wg.Add(1)
		go func(t string) {
			defer wg.Done()
			ctx := WithTenant(context.Background(), t)
			for i := 0; i < 10; i++ {
				trace := &Trace{
					TraceID:     fmt.Sprintf("%s-trace-%03d", t, i),
					ServiceName: fmt.Sprintf("%s-service", t),
					Operation:   "Process",
					Duration:    time.Duration(100+i*10) * time.Millisecond,
					StartTime:   time.Now(),
				}
				concurrentStorage.WriteTrace(ctx, trace)
			}
		}(tenant)
	}
	wg.Wait()

	// 각 테넌트 스토리지 확인
	for _, tenant := range tenants {
		store, _ := concurrentStorage.GetTenantStore(tenant)
		traces := store.ListTraces()
		fmt.Printf("%-12s: %d개 트레이스 저장됨\n", tenant, len(traces))

		// 다른 테넌트의 데이터가 섞이지 않았는지 확인
		otherTenantData := false
		for _, trace := range traces {
			if !strings.HasPrefix(trace.TraceID, tenant) {
				otherTenantData = true
				break
			}
		}
		if otherTenantData {
			fmt.Printf("  경고: 다른 테넌트의 데이터가 섞여 있습니다!\n")
		} else {
			fmt.Printf("  격리 확인: 해당 테넌트의 데이터만 존재합니다\n")
		}
	}

	// =========================================================================
	// 아키텍처 다이어그램
	// =========================================================================
	printSeparator("아키텍처: 테넌트 전파 흐름")

	fmt.Println(`
    HTTP 요청                          gRPC 요청
    +-----------+                      +-----------+
    | x-tenant: |                      | metadata: |
    | team-alpha|                      | x-tenant: |
    +-----+-----+                      | team-alpha|
          |                            +-----+-----+
          v                                  |
  ExtractTenantHTTPHandler              v
  (http.go L16-L38)              NewGuardingUnaryInterceptor
          |                      (grpc.go L104-L117)
          v                                  |
  +-------+--------+                        v
  | Manager.Valid() |           GetValidTenant()
  | (guard 검증)    |           (grpc.go L26-L37)
  +-------+--------+                        |
          |                                  |
          v                                  v
  WithTenant(ctx, tenant)        WithTenant(ctx, tenant)
  (context.go L17-L19)          (context.go L17-L19)
          |                                  |
          +----------+  +---------+----------+
                     |  |
                     v  v
            +--------+--------+
            |  context.Context |
            |  tenant="team-  |
            |   alpha"        |
            +--------+--------+
                     |
                     v
         GetTenant(ctx) → "team-alpha"
                     |
                     v
         +----------+-----------+
         | MultiTenantStorage   |
         |  tenants:            |
         |    team-alpha: {...} |  ← 독립 스토리지
         |    team-beta:  {...} |  ← 독립 스토리지
         |    team-gamma: {...} |  ← 독립 스토리지
         +----------------------+
`)
}
