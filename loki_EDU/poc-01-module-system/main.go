package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Loki 모듈 시스템 시뮬레이션
// =============================================================================
//
// Loki는 모놀리식(single-binary)과 마이크로서비스 모드를 동시에 지원한다.
// 이를 위해 "모듈 시스템"을 사용하여 각 컴포넌트(Distributor, Ingester, Querier 등)를
// 독립적인 모듈로 정의하고, 의존성 그래프에 따라 초기화/종료한다.
//
// 핵심 원리:
//   1. 모듈 등록: 각 모듈은 이름, 초기화 함수, 종료 함수, 의존 모듈 목록을 가진다
//   2. 의존성 그래프: 모듈 간 의존 관계를 DAG(Directed Acyclic Graph)로 관리한다
//   3. 위상 정렬(Topological Sort): 의존성 순서대로 초기화한다 (Kahn's Algorithm)
//   4. 역순 종료: 초기화의 역순으로 graceful shutdown 한다
//   5. 순환 의존성 감지: DAG에 사이클이 있으면 시작 전에 에러를 발생시킨다
//
// Loki 실제 구현 참조:
//   - pkg/loki/modules.go: 모듈 등록 (initDistributor, initIngester 등)
//   - pkg/loki/loki.go: 모듈 매니저를 이용한 초기화 흐름
//   - dskit/services/manager.go: 서비스 라이프사이클 관리
// =============================================================================

// ModuleState는 모듈의 현재 상태를 나타낸다.
// Loki의 services.State와 유사한 개념이다.
type ModuleState int

const (
	StateNew      ModuleState = iota // 등록만 된 상태
	StateStarting                    // 초기화 중
	StateRunning                     // 정상 동작 중
	StateStopping                    // 종료 중
	StateStopped                     // 종료 완료
	StateFailed                      // 초기화/종료 실패
)

// String은 ModuleState를 문자열로 변환한다.
func (s ModuleState) String() string {
	switch s {
	case StateNew:
		return "NEW"
	case StateStarting:
		return "STARTING"
	case StateRunning:
		return "RUNNING"
	case StateStopping:
		return "STOPPING"
	case StateStopped:
		return "STOPPED"
	case StateFailed:
		return "FAILED"
	default:
		return "UNKNOWN"
	}
}

// Module은 Loki의 하나의 컴포넌트를 나타낸다.
// Loki에서 Distributor, Ingester, Querier 등이 각각 하나의 모듈이다.
type Module struct {
	Name         string                // 모듈 이름 (예: "ingester", "distributor")
	Dependencies []string              // 의존하는 모듈 이름 목록
	InitFn       func() (StopFn, error) // 초기화 함수 — 성공 시 종료 함수를 반환
	State        ModuleState           // 현재 상태
}

// StopFn은 모듈 종료 시 호출되는 함수이다.
type StopFn func() error

// ModuleManager는 모듈들의 등록, 의존성 관리, 초기화/종료를 담당한다.
// Loki의 pkg/loki/loki.go에서 사용하는 모듈 매니저와 동일한 역할이다.
type ModuleManager struct {
	mu       sync.Mutex
	modules  map[string]*Module  // 이름 → 모듈 매핑
	stopFns  []namedStopFn       // 종료 함수 목록 (초기화 순서대로 저장)
	initOrder []string           // 위상 정렬된 초기화 순서
}

// namedStopFn은 종료 함수에 모듈 이름을 연결한다.
type namedStopFn struct {
	name string
	fn   StopFn
}

// NewModuleManager는 새 ModuleManager를 생성한다.
func NewModuleManager() *ModuleManager {
	return &ModuleManager{
		modules: make(map[string]*Module),
	}
}

// RegisterModule은 모듈을 매니저에 등록한다.
// Loki에서는 pkg/loki/modules.go의 init 함수들에서 호출된다.
func (mm *ModuleManager) RegisterModule(name string, initFn func() (StopFn, error), deps ...string) {
	mm.mu.Lock()
	defer mm.mu.Unlock()

	mm.modules[name] = &Module{
		Name:         name,
		Dependencies: deps,
		InitFn:       initFn,
		State:        StateNew,
	}
}

// detectCycle은 DFS를 이용하여 의존성 그래프의 순환을 감지한다.
// 순환이 발견되면 순환 경로를 문자열로 반환한다.
func (mm *ModuleManager) detectCycle() error {
	// 방문 상태: 0=미방문, 1=방문 중(재귀 스택), 2=방문 완료
	visited := make(map[string]int)
	path := make([]string, 0)

	var dfs func(name string) error
	dfs = func(name string) error {
		visited[name] = 1 // 방문 중
		path = append(path, name)

		mod, ok := mm.modules[name]
		if !ok {
			return fmt.Errorf("알 수 없는 모듈: %s", name)
		}

		for _, dep := range mod.Dependencies {
			if _, exists := mm.modules[dep]; !exists {
				return fmt.Errorf("모듈 '%s'이 존재하지 않는 모듈 '%s'에 의존함", name, dep)
			}
			if visited[dep] == 1 {
				// 순환 감지: 현재 경로에서 dep가 이미 방문 중
				cycleStart := -1
				for i, p := range path {
					if p == dep {
						cycleStart = i
						break
					}
				}
				cycle := append(path[cycleStart:], dep)
				return fmt.Errorf("순환 의존성 감지: %s", strings.Join(cycle, " → "))
			}
			if visited[dep] == 0 {
				if err := dfs(dep); err != nil {
					return err
				}
			}
		}

		visited[name] = 2 // 방문 완료
		path = path[:len(path)-1]
		return nil
	}

	for name := range mm.modules {
		if visited[name] == 0 {
			if err := dfs(name); err != nil {
				return err
			}
		}
	}
	return nil
}

// topologicalSort는 Kahn's Algorithm으로 위상 정렬을 수행한다.
// 의존성이 없는 모듈부터 시작하여, 의존성이 해결된 순서대로 반환한다.
//
// 알고리즘:
//   1. 각 노드의 진입 차수(in-degree)를 계산
//   2. 진입 차수가 0인 노드를 큐에 추가
//   3. 큐에서 노드를 꺼내어 결과에 추가하고, 해당 노드에 의존하는 노드의 진입 차수를 감소
//   4. 진입 차수가 0이 된 노드를 큐에 추가
//   5. 모든 노드가 처리될 때까지 반복
func (mm *ModuleManager) topologicalSort(targets []string) ([]string, error) {
	// 타겟과 그 의존성을 모두 포함하는 서브그래프 구성
	needed := make(map[string]bool)
	var collectDeps func(name string)
	collectDeps = func(name string) {
		if needed[name] {
			return
		}
		needed[name] = true
		if mod, ok := mm.modules[name]; ok {
			for _, dep := range mod.Dependencies {
				collectDeps(dep)
			}
		}
	}
	for _, t := range targets {
		collectDeps(t)
	}

	// 진입 차수 계산 (needed 범위 내에서만)
	inDegree := make(map[string]int)
	for name := range needed {
		inDegree[name] = 0
	}
	for name := range needed {
		mod := mm.modules[name]
		for _, dep := range mod.Dependencies {
			if needed[dep] {
				// name이 dep에 의존한다 → dep → name 방향의 간선
				// name의 진입 차수를 증가시킨다
				inDegree[name]++
			}
		}
	}

	// 진입 차수가 0인 노드를 큐에 추가
	queue := make([]string, 0)
	for name, degree := range inDegree {
		if degree == 0 {
			queue = append(queue, name)
		}
	}

	// 정렬된 결과
	var sorted []string

	for len(queue) > 0 {
		// 큐에서 하나를 꺼냄 (알파벳 순서로 안정적 정렬을 위해 최소값 선택)
		minIdx := 0
		for i := 1; i < len(queue); i++ {
			if queue[i] < queue[minIdx] {
				minIdx = i
			}
		}
		current := queue[minIdx]
		queue = append(queue[:minIdx], queue[minIdx+1:]...)

		sorted = append(sorted, current)

		// current에 의존하는 노드의 진입 차수 감소
		for name := range needed {
			mod := mm.modules[name]
			for _, dep := range mod.Dependencies {
				if dep == current {
					inDegree[name]--
					if inDegree[name] == 0 {
						queue = append(queue, name)
					}
				}
			}
		}
	}

	if len(sorted) != len(needed) {
		return nil, fmt.Errorf("위상 정렬 실패: 순환 의존성 존재")
	}

	return sorted, nil
}

// InitModules는 지정된 타겟 모듈과 그 의존성을 위상 정렬 순서로 초기화한다.
// Loki에서는 -target 플래그로 지정된 모듈(예: all, read, write)을 초기화한다.
func (mm *ModuleManager) InitModules(targets ...string) error {
	mm.mu.Lock()
	defer mm.mu.Unlock()

	// 1. 순환 의존성 감지
	if err := mm.detectCycle(); err != nil {
		return fmt.Errorf("모듈 초기화 실패: %w", err)
	}

	// 2. 위상 정렬
	order, err := mm.topologicalSort(targets)
	if err != nil {
		return fmt.Errorf("위상 정렬 실패: %w", err)
	}
	mm.initOrder = order

	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║              모듈 초기화 순서 (위상 정렬 결과)                    ║")
	fmt.Println("╠══════════════════════════════════════════════════════════════╣")
	for i, name := range order {
		mod := mm.modules[name]
		deps := "(없음)"
		if len(mod.Dependencies) > 0 {
			deps = strings.Join(mod.Dependencies, ", ")
		}
		fmt.Printf("║  %d. %-15s  의존: %-30s ║\n", i+1, name, deps)
	}
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()

	// 3. 순서대로 초기화
	for _, name := range order {
		mod := mm.modules[name]
		mod.State = StateStarting
		fmt.Printf("[시작] %-15s 상태: %s → ", name, StateStarting)

		stopFn, err := mod.InitFn()
		if err != nil {
			mod.State = StateFailed
			fmt.Printf("%s (에러: %v)\n", StateFailed, err)
			return fmt.Errorf("모듈 '%s' 초기화 실패: %w", name, err)
		}

		mod.State = StateRunning
		fmt.Printf("%s\n", StateRunning)

		// 종료 함수 저장 (나중에 역순으로 호출)
		if stopFn != nil {
			mm.stopFns = append(mm.stopFns, namedStopFn{name: name, fn: stopFn})
		}
	}

	return nil
}

// StopAll은 모든 모듈을 초기화의 역순으로 종료한다.
// 이렇게 하면 의존 관계에 따라 안전하게 종료할 수 있다.
// 예: Distributor → Ingester → Storage 순으로 시작했다면
//     Storage → Ingester → Distributor 순으로 종료한다.
func (mm *ModuleManager) StopAll() {
	mm.mu.Lock()
	defer mm.mu.Unlock()

	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║              모듈 종료 (초기화 역순)                           ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")

	// 역순으로 종료
	for i := len(mm.stopFns) - 1; i >= 0; i-- {
		nsf := mm.stopFns[i]
		mod := mm.modules[nsf.name]
		mod.State = StateStopping
		fmt.Printf("[종료] %-15s 상태: %s → ", nsf.name, StateStopping)

		if err := nsf.fn(); err != nil {
			mod.State = StateFailed
			fmt.Printf("%s (에러: %v)\n", StateFailed, err)
		} else {
			mod.State = StateStopped
			fmt.Printf("%s\n", StateStopped)
		}
	}
}

// PrintDependencyGraph는 의존성 그래프를 ASCII 형태로 출력한다.
func (mm *ModuleManager) PrintDependencyGraph() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║              모듈 의존성 그래프                                ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()

	for name, mod := range mm.modules {
		if len(mod.Dependencies) == 0 {
			fmt.Printf("  [%s] (기반 모듈)\n", name)
		} else {
			fmt.Printf("  [%s]\n", name)
			for i, dep := range mod.Dependencies {
				if i == len(mod.Dependencies)-1 {
					fmt.Printf("    └── %s\n", dep)
				} else {
					fmt.Printf("    ├── %s\n", dep)
				}
			}
		}
		fmt.Println()
	}
}

// =============================================================================
// 시뮬레이션용 모듈 초기화 함수들
// Loki의 pkg/loki/modules.go에서 정의하는 initXxx 함수들을 모사한다.
// =============================================================================

// createInitFn은 시뮬레이션용 초기화/종료 함수를 생성한다.
func createInitFn(name string, initDelay, stopDelay time.Duration) func() (StopFn, error) {
	return func() (StopFn, error) {
		// 초기화 시뮬레이션 (실제로는 서버 시작, 연결 설정 등)
		time.Sleep(initDelay)

		stopFn := func() error {
			// 종료 시뮬레이션 (실제로는 연결 끊기, 플러시 등)
			time.Sleep(stopDelay)
			return nil
		}

		return stopFn, nil
	}
}

func main() {
	fmt.Println("=================================================================")
	fmt.Println("  Loki 모듈 시스템 시뮬레이션")
	fmt.Println("  - 의존성 그래프 기반 모듈 초기화/종료")
	fmt.Println("  - 위상 정렬(Topological Sort)로 초기화 순서 결정")
	fmt.Println("  - 초기화 역순으로 graceful shutdown")
	fmt.Println("=================================================================")
	fmt.Println()

	mm := NewModuleManager()

	// =========================================================================
	// 모듈 등록
	// Loki의 실제 모듈 구조를 모사한다:
	//   - server: HTTP/gRPC 서버 (기반 모듈)
	//   - ring: 일관된 해시 링 (기반 모듈)
	//   - store: 청크 스토어 (기반 모듈)
	//   - ingester: 로그 수집기 (ring, store에 의존)
	//   - distributor: 로그 분배기 (ring, ingester에 의존)
	//   - querier: 쿼리 처리기 (store, ingester에 의존)
	//   - query-frontend: 쿼리 프론트엔드 (querier에 의존)
	//   - compactor: 청크 압축기 (store에 의존)
	//   - ruler: 알림 규칙 평가기 (querier에 의존)
	// =========================================================================

	delay := 30 * time.Millisecond

	// 기반 모듈 — 의존성 없음
	mm.RegisterModule("server", createInitFn("server", delay, delay))
	mm.RegisterModule("ring", createInitFn("ring", delay, delay))
	mm.RegisterModule("store", createInitFn("store", delay, delay))

	// 중간 모듈 — 기반 모듈에 의존
	mm.RegisterModule("ingester", createInitFn("ingester", delay, delay),
		"ring", "store")
	mm.RegisterModule("distributor", createInitFn("distributor", delay, delay),
		"ring", "ingester")

	// 상위 모듈 — 중간 모듈에 의존
	mm.RegisterModule("querier", createInitFn("querier", delay, delay),
		"store", "ingester")
	mm.RegisterModule("query-frontend", createInitFn("query-frontend", delay, delay),
		"querier")
	mm.RegisterModule("compactor", createInitFn("compactor", delay, delay),
		"store")
	mm.RegisterModule("ruler", createInitFn("ruler", delay, delay),
		"querier")

	// =========================================================================
	// 시나리오 1: "all" 모드 — 모든 모듈 초기화
	// Loki의 -target=all과 동일한 동작
	// =========================================================================

	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println(" 시나리오 1: 전체 모듈 초기화 (all 모드)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	mm.PrintDependencyGraph()

	targets := []string{
		"server", "distributor", "ingester", "querier",
		"query-frontend", "compactor", "ruler",
	}

	if err := mm.InitModules(targets...); err != nil {
		fmt.Printf("초기화 에러: %v\n", err)
		return
	}

	// 정상 동작 시뮬레이션
	fmt.Println()
	fmt.Println("모든 모듈 정상 동작 중... (100ms 대기)")
	time.Sleep(100 * time.Millisecond)

	// graceful shutdown
	mm.StopAll()

	// =========================================================================
	// 시나리오 2: "read" 모드 — 읽기 경로 모듈만 초기화
	// Loki의 -target=read와 동일한 동작
	// =========================================================================

	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println(" 시나리오 2: 읽기 경로만 초기화 (read 모드)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	mm2 := NewModuleManager()
	mm2.RegisterModule("server", createInitFn("server", delay, delay))
	mm2.RegisterModule("ring", createInitFn("ring", delay, delay))
	mm2.RegisterModule("store", createInitFn("store", delay, delay))
	mm2.RegisterModule("ingester", createInitFn("ingester", delay, delay), "ring", "store")
	mm2.RegisterModule("querier", createInitFn("querier", delay, delay), "store", "ingester")
	mm2.RegisterModule("query-frontend", createInitFn("query-frontend", delay, delay), "querier")

	// read 모드: query-frontend만 타겟으로 지정하면 의존성이 자동으로 해결된다
	fmt.Println("타겟: query-frontend (의존성 자동 해결)")
	fmt.Println()

	if err := mm2.InitModules("query-frontend"); err != nil {
		fmt.Printf("초기화 에러: %v\n", err)
		return
	}

	time.Sleep(50 * time.Millisecond)
	mm2.StopAll()

	// =========================================================================
	// 시나리오 3: 순환 의존성 감지
	// =========================================================================

	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println(" 시나리오 3: 순환 의존성 감지")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	mm3 := NewModuleManager()
	// A → B → C → A 순환 구성
	mm3.RegisterModule("moduleA", createInitFn("moduleA", delay, delay), "moduleC")
	mm3.RegisterModule("moduleB", createInitFn("moduleB", delay, delay), "moduleA")
	mm3.RegisterModule("moduleC", createInitFn("moduleC", delay, delay), "moduleB")

	if err := mm3.InitModules("moduleA"); err != nil {
		fmt.Printf("예상된 에러: %v\n", err)
	}

	fmt.Println()
	fmt.Println("=================================================================")
	fmt.Println("  시뮬레이션 완료")
	fmt.Println()
	fmt.Println("  핵심 포인트:")
	fmt.Println("  1. 모듈 등록 시 의존성을 선언적으로 정의")
	fmt.Println("  2. 위상 정렬로 안전한 초기화 순서를 자동 결정")
	fmt.Println("  3. 타겟 모듈 지정 시 의존성이 자동으로 해결됨")
	fmt.Println("  4. 초기화 역순으로 graceful shutdown 보장")
	fmt.Println("  5. 순환 의존성을 사전에 감지하여 무한 루프 방지")
	fmt.Println("=================================================================")
}
