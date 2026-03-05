package main

import (
	"fmt"
	"reflect"
	"strings"
)

// ---------------------------------------------------------------------------
// 1. 서비스 인터페이스 정의 (Grafana 내부 서비스를 추상화)
// ---------------------------------------------------------------------------

// Logger 인터페이스 — Grafana의 log.Logger에 해당
type Logger interface {
	Info(msg string)
	Error(msg string)
}

// Store 인터페이스 — Grafana의 sqlstore.SQLStore에 해당
type Store interface {
	Get(key string) (string, bool)
	Set(key, value string)
}

// HTTPServer 인터페이스 — Grafana의 api.HTTPServer에 해당
type HTTPServer interface {
	Start() error
	Stop() error
}

// ---------------------------------------------------------------------------
// 2. 구체 구현체
// ---------------------------------------------------------------------------

type consoleLogger struct {
	prefix string
}

func (l *consoleLogger) Info(msg string)  { fmt.Printf("[INFO][%s] %s\n", l.prefix, msg) }
func (l *consoleLogger) Error(msg string) { fmt.Printf("[ERROR][%s] %s\n", l.prefix, msg) }

type memoryStore struct {
	data   map[string]string
	logger Logger
}

func (s *memoryStore) Get(key string) (string, bool) {
	v, ok := s.data[key]
	if ok {
		s.logger.Info(fmt.Sprintf("Store.Get(%s) = %s", key, v))
	}
	return v, ok
}

func (s *memoryStore) Set(key, value string) {
	s.data[key] = value
	s.logger.Info(fmt.Sprintf("Store.Set(%s, %s)", key, value))
}

type simpleHTTPServer struct {
	store  Store
	logger Logger
	addr   string
}

func (h *simpleHTTPServer) Start() error {
	h.logger.Info(fmt.Sprintf("HTTPServer starting on %s", h.addr))
	return nil
}

func (h *simpleHTTPServer) Stop() error {
	h.logger.Info("HTTPServer stopping")
	return nil
}

// ---------------------------------------------------------------------------
// 3. Provider 함수 — Wire에서 사용하는 생성자 패턴
// ---------------------------------------------------------------------------

// ProvideLogger — 의존성 없음
func ProvideLogger() Logger {
	return &consoleLogger{prefix: "grafana"}
}

// ProvideStore — Logger에 의존
func ProvideStore(logger Logger) Store {
	return &memoryStore{
		data:   make(map[string]string),
		logger: logger,
	}
}

// ProvideHTTPServer — Store, Logger에 의존
func ProvideHTTPServer(store Store, logger Logger) HTTPServer {
	return &simpleHTTPServer{
		store:  store,
		logger: logger,
		addr:   ":3000",
	}
}

// ---------------------------------------------------------------------------
// 4. Wire DI Container 시뮬레이션
// ---------------------------------------------------------------------------

// Provider는 하나의 의존성 제공자를 표현한다.
type Provider struct {
	Name       string       // 제공하는 타입 이름
	Deps       []string     // 의존하는 타입 이름들
	OutputType reflect.Type // 반환 타입
	Fn         interface{}  // Provider 함수
}

// Binding은 인터페이스 → 구체 타입 바인딩이다 (wire.Bind 시뮬레이션).
type Binding struct {
	Interface string
	Concrete  string
}

// Container는 Wire의 Injector를 시뮬레이션한다.
type Container struct {
	providers  map[string]*Provider
	bindings   map[string]string        // interface → concrete
	instances  map[string]reflect.Value  // 생성된 인스턴스 캐시
	buildOrder []string                 // 위상 정렬 결과
}

// NewContainer — 새 DI 컨테이너 생성
func NewContainer() *Container {
	return &Container{
		providers: make(map[string]*Provider),
		bindings:  make(map[string]string),
		instances: make(map[string]reflect.Value),
	}
}

// Provide — Provider 함수 등록 (Wire의 wire.Build에 전달하는 Provider에 해당)
func (c *Container) Provide(name string, deps []string, fn interface{}) {
	p := &Provider{
		Name:       name,
		Deps:       deps,
		OutputType: reflect.TypeOf(fn),
		Fn:         fn,
	}
	c.providers[name] = p

	if len(deps) == 0 {
		fmt.Printf("[Provider 등록] %s provider 등록됨\n", name)
	} else {
		fmt.Printf("[Provider 등록] %s provider 등록됨 (의존: %s)\n", name, strings.Join(deps, ", "))
	}
}

// Bind — 인터페이스를 구체 타입에 바인딩 (wire.Bind 시뮬레이션)
func (c *Container) Bind(iface, concrete string) {
	c.bindings[iface] = concrete
	fmt.Printf("[Bind] %s → %s 바인딩됨\n", iface, concrete)
}

// detectCycle — DFS 기반 순환 의존성 감지
func (c *Container) detectCycle() (bool, []string) {
	// 0: unvisited, 1: in-progress, 2: done
	state := make(map[string]int)
	path := []string{}

	var dfs func(node string) bool
	dfs = func(node string) bool {
		state[node] = 1 // in-progress
		path = append(path, node)

		p, ok := c.providers[node]
		if !ok {
			return false
		}
		for _, dep := range p.Deps {
			if state[dep] == 1 {
				// 사이클 발견 — 경로에서 사이클 부분 추출
				cycleStart := -1
				for i, n := range path {
					if n == dep {
						cycleStart = i
						break
					}
				}
				if cycleStart >= 0 {
					cycle := append(path[cycleStart:], dep)
					return true
					_ = cycle
				}
				return true
			}
			if state[dep] == 0 {
				if dfs(dep) {
					return true
				}
			}
		}

		state[node] = 2 // done
		path = path[:len(path)-1]
		return false
	}

	for name := range c.providers {
		if state[name] == 0 {
			if dfs(name) {
				return true, path
			}
		}
	}
	return false, nil
}

// topologicalSort — Kahn's algorithm으로 초기화 순서 결정
func (c *Container) topologicalSort() ([]string, error) {
	// 진입 차수 계산
	inDegree := make(map[string]int)
	for name := range c.providers {
		if _, ok := inDegree[name]; !ok {
			inDegree[name] = 0
		}
		for _, dep := range c.providers[name].Deps {
			inDegree[dep] += 0 // dep도 맵에 존재하도록
		}
	}
	for _, p := range c.providers {
		for _, dep := range p.Deps {
			_ = dep
			inDegree[p.Name]++
		}
	}

	// 진입 차수 0인 노드를 큐에 추가
	queue := []string{}
	for name, deg := range inDegree {
		if deg == 0 {
			if _, isProvider := c.providers[name]; isProvider {
				queue = append(queue, name)
			}
		}
	}

	result := []string{}
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		result = append(result, node)

		// node에 의존하는 모든 Provider의 진입 차수 감소
		for name, p := range c.providers {
			for _, dep := range p.Deps {
				if dep == node {
					inDegree[name]--
					if inDegree[name] == 0 {
						queue = append(queue, name)
					}
				}
			}
		}
	}

	if len(result) != len(c.providers) {
		return nil, fmt.Errorf("순환 의존성 감지: 위상 정렬 불가")
	}
	return result, nil
}

// Build — 모든 의존성을 해결하고 인스턴스를 생성 (wire.Build 시뮬레이션)
func (c *Container) Build() error {
	fmt.Println("\n=== Build 시작 ===")

	// 1. 순환 의존성 검사
	fmt.Println("\n[1단계] 순환 의존성 검사...")
	hasCycle, cyclePath := c.detectCycle()
	if hasCycle {
		return fmt.Errorf("순환 의존성 발견: %s", strings.Join(cyclePath, " → "))
	}
	fmt.Println("  순환 의존성 없음 ✓")

	// 2. 위상 정렬
	fmt.Println("\n[2단계] 위상 정렬 (초기화 순서 결정)...")
	order, err := c.topologicalSort()
	if err != nil {
		return err
	}
	c.buildOrder = order
	fmt.Printf("  초기화 순서: %s\n", strings.Join(order, " → "))

	// 3. 순서대로 인스턴스 생성
	fmt.Println("\n[3단계] 인스턴스 생성...")
	for _, name := range order {
		p := c.providers[name]

		// 의존성 인스턴스들을 인자로 수집
		args := []reflect.Value{}
		for _, dep := range p.Deps {
			instance, ok := c.instances[dep]
			if !ok {
				return fmt.Errorf("%s의 의존성 %s가 아직 초기화되지 않음", name, dep)
			}
			args = append(args, instance)
		}

		// Provider 함수 호출
		fn := reflect.ValueOf(p.Fn)
		results := fn.Call(args)
		if len(results) > 0 {
			c.instances[name] = results[0]
			fmt.Printf("  [생성] %s 인스턴스 생성됨 (타입: %s)\n", name, results[0].Type())
		}
	}

	fmt.Println("\n[빌드 완료] 모든 서비스 초기화 성공")
	return nil
}

// Get — 이름으로 인스턴스를 가져온다.
func (c *Container) Get(name string) (interface{}, bool) {
	// 바인딩 확인
	if concrete, ok := c.bindings[name]; ok {
		name = concrete
	}
	v, ok := c.instances[name]
	if !ok {
		return nil, false
	}
	return v.Interface(), true
}

// ---------------------------------------------------------------------------
// 5. 순환 의존성 테스트용 Provider
// ---------------------------------------------------------------------------

type ServiceA struct{}
type ServiceB struct{}
type ServiceC struct{}

func ProvideServiceA(_ *ServiceB) *ServiceA { return &ServiceA{} }
func ProvideServiceB(_ *ServiceC) *ServiceB { return &ServiceB{} }
func ProvideServiceC(_ *ServiceA) *ServiceC { return &ServiceC{} } // A→B→C→A 순환

// ---------------------------------------------------------------------------
// 6. 메인 — Wire DI 시뮬레이션 실행
// ---------------------------------------------------------------------------

func main() {
	fmt.Println("╔══════════════════════════════════════════════╗")
	fmt.Println("║   Grafana Wire DI Container Simulation       ║")
	fmt.Println("╚══════════════════════════════════════════════╝")

	// ---------------------------------------------------------------
	// 정상 케이스: Logger → Store → HTTPServer
	// ---------------------------------------------------------------
	fmt.Println("\n━━━ 1. 정상 의존성 그래프 ━━━")
	container := NewContainer()

	container.Provide("Logger", []string{}, ProvideLogger)
	container.Provide("Store", []string{"Logger"}, ProvideStore)
	container.Provide("HTTPServer", []string{"Store", "Logger"}, ProvideHTTPServer)

	// 인터페이스 바인딩 시뮬레이션
	container.Bind("log.Logger", "Logger")
	container.Bind("sqlstore.Store", "Store")

	if err := container.Build(); err != nil {
		fmt.Printf("빌드 실패: %v\n", err)
		return
	}

	// 생성된 서비스 사용
	fmt.Println("\n━━━ 서비스 사용 테스트 ━━━")

	if logger, ok := container.Get("Logger"); ok {
		logger.(Logger).Info("DI 컨테이너에서 가져온 Logger 사용")
	}

	if store, ok := container.Get("Store"); ok {
		store.(Store).Set("dashboard:1", "My Dashboard")
		store.(Store).Get("dashboard:1")
	}

	if server, ok := container.Get("HTTPServer"); ok {
		server.(HTTPServer).Start()
		server.(HTTPServer).Stop()
	}

	// 바인딩으로 가져오기
	if logger, ok := container.Get("log.Logger"); ok {
		logger.(Logger).Info("바인딩(log.Logger → Logger)으로 가져온 Logger")
	}

	// ---------------------------------------------------------------
	// 순환 의존성 케이스: A → B → C → A
	// ---------------------------------------------------------------
	fmt.Println("\n━━━ 2. 순환 의존성 감지 테스트 ━━━")
	cycleContainer := NewContainer()

	cycleContainer.Provide("ServiceA", []string{"ServiceB"}, ProvideServiceA)
	cycleContainer.Provide("ServiceB", []string{"ServiceC"}, ProvideServiceB)
	cycleContainer.Provide("ServiceC", []string{"ServiceA"}, ProvideServiceC)

	if err := cycleContainer.Build(); err != nil {
		fmt.Printf("\n[순환 의존성 감지됨] %v\n", err)
	}

	// ---------------------------------------------------------------
	// 의존성 그래프 시각화
	// ---------------------------------------------------------------
	fmt.Println("\n━━━ 3. 의존성 그래프 (Grafana wire.go 구조) ━━━")
	fmt.Println(`
  Wire DI 의존성 그래프:

  ┌─────────────────────────────────────────────────┐
  │                  wire.Build()                    │
  │                                                  │
  │   ┌──────────┐                                   │
  │   │  Logger   │ ← 의존성 없음 (먼저 생성)          │
  │   └────┬─────┘                                   │
  │        │                                         │
  │        ├──────────────────┐                      │
  │        ▼                  ▼                      │
  │   ┌──────────┐     ┌─────────────┐              │
  │   │  Store   │     │ HTTPServer  │              │
  │   │(Logger)  │────▶│(Store,Logger)│              │
  │   └──────────┘     └─────────────┘              │
  │                                                  │
  │   위상 정렬 순서: Logger → Store → HTTPServer     │
  └─────────────────────────────────────────────────┘

  순환 의존성 (에러):

  ┌──────────┐     ┌──────────┐     ┌──────────┐
  │ ServiceA │────▶│ ServiceB │────▶│ ServiceC │
  └──────────┘     └──────────┘     └────┬─────┘
       ▲                                  │
       └──────────────────────────────────┘
                 (사이클 감지!)
`)

	// ---------------------------------------------------------------
	// Grafana 실제 Provider 구조 설명
	// ---------------------------------------------------------------
	fmt.Println("━━━ 4. Grafana 실제 Wire Provider 목록 (일부) ━━━")
	grafanaProviders := []struct {
		Name string
		Deps string
		File string
	}{
		{"*setting.Cfg", "(없음)", "pkg/setting/setting.go"},
		{"*sqlstore.SQLStore", "Cfg, Bus", "pkg/services/sqlstore/sqlstore.go"},
		{"*api.HTTPServer", "Cfg, RouteRegister, Bus, ...", "pkg/api/http_server.go"},
		{"*login.AuthenticatorService", "Cfg, SQLStore, UserService", "pkg/services/login/authn.go"},
		{"*dashboards.DashboardService", "Cfg, SQLStore, FolderService", "pkg/services/dashboards/service.go"},
		{"*alerting.AlertEngine", "Cfg, SQLStore, DashboardService", "pkg/services/alerting/engine.go"},
	}

	fmt.Printf("  %-35s %-30s %s\n", "Provider", "의존성", "파일")
	fmt.Println("  " + strings.Repeat("-", 95))
	for _, p := range grafanaProviders {
		fmt.Printf("  %-35s %-30s %s\n", p.Name, p.Deps, p.File)
	}
}
