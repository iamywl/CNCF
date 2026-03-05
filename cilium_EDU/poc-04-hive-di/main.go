package main

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Cilium Hive DI (의존성 주입) 프레임워크 시뮬레이션
// =============================================================================
//
// Cilium의 Hive는 uber/dig 기반의 DI 프레임워크이다.
// 이 PoC는 핵심 메커니즘을 표준 라이브러리만으로 재현한다.
//
// 실제 소스 코드 참조:
//   - vendor/github.com/cilium/hive/hive.go         → Hive 컨테이너, Start/Stop/Populate
//   - vendor/github.com/cilium/hive/cell/cell.go    → Cell 인터페이스, container 인터페이스
//   - vendor/github.com/cilium/hive/cell/module.go  → Module, ModuleID, FullModuleID, 스코프 생성
//   - vendor/github.com/cilium/hive/cell/provide.go → Provide 셀, 생성자 등록
//   - vendor/github.com/cilium/hive/cell/invoke.go  → Invoke 셀, 지연된 함수 호출
//   - vendor/github.com/cilium/hive/cell/group.go   → Group 셀, 스코프 없는 그룹
//   - vendor/github.com/cilium/hive/cell/lifecycle.go → DefaultLifecycle, Hook, HookInterface
//   - pkg/hive/hive.go                              → Cilium 전용 Hive 래퍼 (module decorator 등)
//
// 핵심 설계 원리:
//   1. reflect.Type 기반 의존성 해석 — 생성자의 매개변수/반환값 타입으로 그래프 구축
//   2. Cell 트리 구조 — Module은 dig.Scope를 생성, Group은 스코프 없이 묶음
//   3. Lifecycle 역순 종료 — Start 역순으로 Stop하여 의존성 안전성 보장
//   4. Invoke 지연 실행 — Apply 시점에는 등록만, Populate 시점에 실행
//   5. ModuleDecorator — 각 모듈에 스코프된 객체(로거 등) 자동 제공
// =============================================================================

// ─────────────────────────────────────────────────────────────────────────────
// 1. 타입 시스템 (reflect.Type 기반)
// ─────────────────────────────────────────────────────────────────────────────

// TypeKey는 DI 컨테이너에서 타입을 식별하는 키이다.
// 실제 Hive에서는 reflect.Type이 이 역할을 한다.
type TypeKey = reflect.Type

// typeKeyOf는 값의 reflect.Type을 반환한다.
func typeKeyOf(v interface{}) TypeKey {
	return reflect.TypeOf(v)
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. Cell 인터페이스 (vendor/github.com/cilium/hive/cell/cell.go)
// ─────────────────────────────────────────────────────────────────────────────

// Cell은 Hive의 모듈 단위이다.
// 실제 Cell 인터페이스: Apply(container, rootContainer) error + Info(container) Info
type Cell interface {
	// Apply는 셀을 DI 컨테이너에 적용한다.
	Apply(c *Container) error
	// Info는 셀의 구조적 요약을 반환한다.
	Info(indent int) string
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. Container — dig.Container/dig.Scope 시뮬레이션
// ─────────────────────────────────────────────────────────────────────────────

// Container는 DI 컨테이너이다.
// 실제로는 uber/dig의 dig.Container 또는 dig.Scope가 이 역할을 한다.
// dig.Container.Provide(ctor)로 생성자를 등록하고,
// dig.Container.Invoke(fn)로 의존성을 주입받아 함수를 호출한다.
type Container struct {
	mu          sync.RWMutex
	name        string               // 스코프 이름 (모듈 ID)
	parent      *Container           // 부모 스코프 (nil이면 루트)
	providers   map[TypeKey]*ctor    // 타입별 생성자
	instances   map[TypeKey]reflect.Value // 이미 생성된 인스턴스
	children    []*Container         // 하위 스코프
	private     map[TypeKey]bool     // private 타입 (스코프 내부에서만 접근)
}

// ctor는 하나의 생성자 함수를 래핑한다.
type ctor struct {
	fn       interface{}    // 생성자 함수
	fnType   reflect.Type   // 함수 타입
	fnValue  reflect.Value  // 함수 값
	inTypes  []TypeKey      // 입력(매개변수) 타입 목록
	outTypes []TypeKey      // 출력(반환값) 타입 목록 — error 제외
	hasError bool           // 마지막 반환값이 error인지
	name     string         // 디버깅용 이름
}

func NewContainer(name string) *Container {
	return &Container{
		name:      name,
		providers: make(map[TypeKey]*ctor),
		instances: make(map[TypeKey]reflect.Value),
		private:   make(map[TypeKey]bool),
	}
}

// Scope는 하위 스코프를 생성한다.
// 실제 코드: dig.Container.Scope(name) *dig.Scope
// Module.Apply에서 c.Scope(m.id)로 모듈별 스코프를 생성한다.
func (c *Container) Scope(name string) *Container {
	child := &Container{
		name:      name,
		parent:    c,
		providers: make(map[TypeKey]*ctor),
		instances: make(map[TypeKey]reflect.Value),
		private:   make(map[TypeKey]bool),
	}
	c.children = append(c.children, child)
	return child
}

// Provide는 생성자를 등록한다.
// 실제 코드: dig.Container.Provide(ctor, opts...) error
// 생성자의 매개변수 타입은 의존성, 반환값 타입은 제공하는 객체가 된다.
func (c *Container) Provide(fn interface{}, export bool) error {
	ct, err := newCtor(fn)
	if err != nil {
		return err
	}

	for _, outType := range ct.outTypes {
		c.mu.Lock()
		if existing, ok := c.providers[outType]; ok {
			c.mu.Unlock()
			return fmt.Errorf("타입 %v 중복 등록: '%s'와 '%s'", outType, existing.name, ct.name)
		}
		c.providers[outType] = ct
		if !export {
			c.private[outType] = true
		}
		c.mu.Unlock()
	}
	return nil
}

// Resolve는 타입을 해석하여 인스턴스를 반환한다.
// 현재 스코프 → 부모 스코프 순서로 탐색한다 (스코프 체인).
// 실제 dig에서는 Scope가 부모 Container의 타입에 접근 가능하다.
func (c *Container) Resolve(t TypeKey) (reflect.Value, error) {
	c.mu.RLock()
	// 이미 생성된 인스턴스가 있으면 반환
	if inst, ok := c.instances[t]; ok {
		c.mu.RUnlock()
		return inst, nil
	}
	c.mu.RUnlock()

	// 현재 스코프에서 생성자 찾기
	c.mu.RLock()
	ct, found := c.providers[t]
	c.mu.RUnlock()

	if found {
		return c.construct(ct, t)
	}

	// 부모 스코프에서 찾기 (private 타입은 부모에서 접근 불가)
	if c.parent != nil {
		c.parent.mu.RLock()
		isPrivate := c.parent.private[t]
		c.parent.mu.RUnlock()
		if !isPrivate {
			return c.parent.Resolve(t)
		}
	}

	return reflect.Value{}, fmt.Errorf("타입 %v를 제공하는 생성자가 없음 (스코프: %s)", t, c.name)
}

// construct는 생성자를 호출하여 인스턴스를 생성한다.
func (c *Container) construct(ct *ctor, targetType TypeKey) (reflect.Value, error) {
	// 의존성 해석
	args := make([]reflect.Value, len(ct.inTypes))
	for i, inType := range ct.inTypes {
		resolved, err := c.Resolve(inType)
		if err != nil {
			return reflect.Value{}, fmt.Errorf("'%s'의 의존성 해석 실패: %w", ct.name, err)
		}
		args[i] = resolved
	}

	// 생성자 호출
	results := ct.fnValue.Call(args)

	// 에러 처리
	if ct.hasError {
		errVal := results[len(results)-1]
		if !errVal.IsNil() {
			return reflect.Value{}, fmt.Errorf("'%s' 생성 실패: %v", ct.name, errVal.Interface())
		}
		results = results[:len(results)-1]
	}

	// 결과 저장
	c.mu.Lock()
	for i, outType := range ct.outTypes {
		c.instances[outType] = results[i]
	}
	c.mu.Unlock()

	return c.instances[targetType], nil
}

// newCtor는 함수를 분석하여 ctor를 생성한다.
func newCtor(fn interface{}) (*ctor, error) {
	fnType := reflect.TypeOf(fn)
	if fnType.Kind() != reflect.Func {
		return nil, fmt.Errorf("생성자는 함수여야 합니다: %T", fn)
	}

	// 입력 타입 수집
	inTypes := make([]TypeKey, fnType.NumIn())
	for i := 0; i < fnType.NumIn(); i++ {
		inTypes[i] = fnType.In(i)
	}

	// 출력 타입 수집 (error 제외)
	hasError := false
	numOut := fnType.NumOut()
	if numOut > 0 && fnType.Out(numOut-1).Implements(reflect.TypeOf((*error)(nil)).Elem()) {
		hasError = true
		numOut--
	}
	outTypes := make([]TypeKey, numOut)
	for i := 0; i < numOut; i++ {
		outTypes[i] = fnType.Out(i)
	}

	return &ctor{
		fn:       fn,
		fnType:   fnType,
		fnValue:  reflect.ValueOf(fn),
		inTypes:  inTypes,
		outTypes: outTypes,
		hasError: hasError,
		name:     fmt.Sprintf("%v", fnType),
	}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. Lifecycle (vendor/github.com/cilium/hive/cell/lifecycle.go)
// ─────────────────────────────────────────────────────────────────────────────

// HookContext는 라이프사이클 훅에 전달되는 컨텍스트이다.
// 타임아웃 시 취소된다. 실제 코드: type HookContext context.Context
type HookContext = context.Context

// HookInterface는 Start/Stop 메서드를 가진 인터페이스이다.
// 실제 코드: vendor/github.com/cilium/hive/cell/lifecycle.go:26
type HookInterface interface {
	Start(HookContext) error
	Stop(HookContext) error
}

// Hook은 OnStart/OnStop 콜백 쌍이다.
// 실제 코드: vendor/github.com/cilium/hive/cell/lifecycle.go:42
type Hook struct {
	OnStart func(HookContext) error
	OnStop  func(HookContext) error
}

func (h Hook) Start(ctx HookContext) error {
	if h.OnStart == nil {
		return nil
	}
	return h.OnStart(ctx)
}

func (h Hook) Stop(ctx HookContext) error {
	if h.OnStop == nil {
		return nil
	}
	return h.OnStop(ctx)
}

// augmentedHook은 모듈 ID와 함께 훅을 추적한다.
// 실제 코드: vendor/github.com/cilium/hive/cell/lifecycle.go:82
type augmentedHook struct {
	HookInterface
	moduleID FullModuleID
}

// Lifecycle은 훅들을 관리한다.
// 실제 코드: DefaultLifecycle (vendor/github.com/cilium/hive/cell/lifecycle.go:74)
// Start는 순서대로, Stop은 역순으로 실행한다.
type Lifecycle struct {
	mu           sync.Mutex
	hooks        []augmentedHook
	numStarted   int
	logThreshold time.Duration
}

func NewLifecycle() *Lifecycle {
	return &Lifecycle{
		logThreshold: time.Millisecond * 100,
	}
}

// Append는 훅을 추가한다.
// 실제 코드: DefaultLifecycle.Append (lifecycle.go:100)
func (lc *Lifecycle) Append(hook HookInterface) {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	lc.hooks = append(lc.hooks, augmentedHook{hook, nil})
}

// AppendWithModule은 모듈 ID와 함께 훅을 추가한다.
func (lc *Lifecycle) AppendWithModule(hook HookInterface, moduleID FullModuleID) {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	lc.hooks = append(lc.hooks, augmentedHook{hook, moduleID})
}

// Start는 모든 훅의 Start를 순서대로 실행한다.
// 실제 코드: DefaultLifecycle.Start (lifecycle.go:107)
// 실패 시 이미 시작된 훅들의 Stop을 역순으로 실행해야 한다.
func (lc *Lifecycle) Start(ctx context.Context) error {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	for i, hook := range lc.hooks[lc.numStarted:] {
		moduleStr := ""
		if hook.moduleID != nil {
			moduleStr = fmt.Sprintf(" [%s]", hook.moduleID.String())
		}

		t0 := time.Now()
		fmt.Printf("    [lifecycle] 시작 훅 실행%s (#%d)\n", moduleStr, lc.numStarted+i+1)

		if err := hook.Start(ctx); err != nil {
			fmt.Printf("    [lifecycle] 시작 훅 실패%s: %v\n", moduleStr, err)
			return err
		}

		d := time.Since(t0)
		if d > lc.logThreshold {
			fmt.Printf("    [lifecycle] 시작 훅 완료%s (소요: %v, 느림!)\n", moduleStr, d)
		}
		lc.numStarted++
	}
	return nil
}

// Stop은 모든 훅의 Stop을 역순으로 실행한다.
// 실제 코드: DefaultLifecycle.Stop (lifecycle.go:156)
// 하나가 실패해도 나머지를 계속 실행한다 (errors.Join).
func (lc *Lifecycle) Stop(ctx context.Context) error {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	var lastErr error
	for ; lc.numStarted > 0; lc.numStarted-- {
		hook := lc.hooks[lc.numStarted-1]
		moduleStr := ""
		if hook.moduleID != nil {
			moduleStr = fmt.Sprintf(" [%s]", hook.moduleID.String())
		}

		fmt.Printf("    [lifecycle] 종료 훅 실행%s (#%d)\n", moduleStr, lc.numStarted)
		if err := hook.Stop(ctx); err != nil {
			fmt.Printf("    [lifecycle] 종료 훅 실패%s: %v (계속 진행)\n", moduleStr, err)
			lastErr = err
		}
	}
	return lastErr
}

// PrintHooks는 등록된 훅을 출력한다.
func (lc *Lifecycle) PrintHooks() {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	fmt.Println("    등록된 시작 훅:")
	for i, hook := range lc.hooks {
		moduleStr := ""
		if hook.moduleID != nil {
			moduleStr = fmt.Sprintf(" (%s)", hook.moduleID.String())
		}
		fmt.Printf("      %d. %T%s\n", i+1, hook.HookInterface, moduleStr)
	}

	fmt.Println("    등록된 종료 훅 (역순):")
	for i := len(lc.hooks) - 1; i >= 0; i-- {
		hook := lc.hooks[i]
		moduleStr := ""
		if hook.moduleID != nil {
			moduleStr = fmt.Sprintf(" (%s)", hook.moduleID.String())
		}
		fmt.Printf("      %d. %T%s\n", len(lc.hooks)-i, hook.HookInterface, moduleStr)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 5. ModuleID / FullModuleID (vendor/github.com/cilium/hive/cell/module.go)
// ─────────────────────────────────────────────────────────────────────────────

// ModuleID는 모듈 식별자이다.
// 실제 코드: type ModuleID string (module.go:36)
type ModuleID string

// FullModuleID는 중첩 모듈의 완전한 식별자이다.
// 예: "agent.controlplane.endpoint-manager"
// 실제 코드: type FullModuleID []string (module.go:41)
type FullModuleID []string

func (f FullModuleID) String() string {
	return strings.Join(f, ".")
}

func (f FullModuleID) append(m ModuleID) FullModuleID {
	result := make(FullModuleID, len(f))
	copy(result, f)
	return append(result, string(m))
}

// ─────────────────────────────────────────────────────────────────────────────
// 6. Cell 구현체들: Provide, Invoke, Module, Group
// ─────────────────────────────────────────────────────────────────────────────

// --- ProvideCell (vendor/github.com/cilium/hive/cell/provide.go) ---

// ProvideCell은 생성자를 DI 컨테이너에 등록하는 셀이다.
// 실제 코드: type provider struct { ctors []any; export bool }
// cell.Provide(ctors...any) Cell은 export=true로 생성
// cell.ProvidePrivate(ctors...any) Cell은 export=false로 생성
type ProvideCell struct {
	ctors  []interface{} // 생성자 함수 목록
	export bool          // true: 스코프 밖에서도 접근 가능, false: 스코프 내부만
}

func Provide(ctors ...interface{}) Cell {
	return &ProvideCell{ctors: ctors, export: true}
}

func ProvidePrivate(ctors ...interface{}) Cell {
	return &ProvideCell{ctors: ctors, export: false}
}

func (p *ProvideCell) Apply(c *Container) error {
	for _, ctor := range p.ctors {
		if err := c.Provide(ctor, p.export); err != nil {
			return fmt.Errorf("Provide 적용 실패: %w", err)
		}
	}
	return nil
}

func (p *ProvideCell) Info(indent int) string {
	prefix := strings.Repeat("  ", indent)
	var sb strings.Builder
	for _, ctor := range p.ctors {
		exportStr := ""
		if !p.export {
			exportStr = " [private]"
		}
		sb.WriteString(fmt.Sprintf("%s  Provide: %v%s\n", prefix, reflect.TypeOf(ctor), exportStr))
	}
	return sb.String()
}

// --- InvokeCell (vendor/github.com/cilium/hive/cell/invoke.go) ---

// InvokeCell은 Populate 시점에 실행될 함수를 등록하는 셀이다.
// 실제 코드: type invoker struct { funcs []namedFunc }
// Apply 시점에는 InvokerList에 등록만 하고, Populate 시점에 실행한다.
// 이렇게 지연하는 이유: 설정 플래그가 먼저 등록/파싱되어야 하기 때문.
type InvokeCell struct {
	fns []interface{} // 실행할 함수 목록
}

func Invoke(fns ...interface{}) Cell {
	return &InvokeCell{fns: fns}
}

func (inv *InvokeCell) Apply(c *Container) error {
	// 실제 Hive에서는 Apply 시점에 InvokerList에 추가만 한다.
	// 여기서는 Hive.invokes에 저장하고, Start 시점에 실행한다.
	// Container에 직접 저장할 수 없으므로 Hive에서 처리한다.
	return nil
}

func (inv *InvokeCell) Info(indent int) string {
	prefix := strings.Repeat("  ", indent)
	var sb strings.Builder
	for _, fn := range inv.fns {
		sb.WriteString(fmt.Sprintf("%s  Invoke: %v\n", prefix, reflect.TypeOf(fn)))
	}
	return sb.String()
}

// --- ModuleCell (vendor/github.com/cilium/hive/cell/module.go) ---

// ModuleCell은 이름 있는 스코프를 생성하는 셀이다.
// 실제 코드: type module struct { id string; description string; cells []Cell }
// Module.Apply는 dig.Scope를 생성하고 ModuleID/FullModuleID를 스코프에 제공한다.
// ProvidePrivate로 등록된 생성자는 이 스코프와 하위 스코프에서만 접근 가능하다.
type ModuleCell struct {
	id          string
	description string
	cells       []Cell
}

func Module(id, description string, cells ...Cell) Cell {
	// 실제 코드에서는 ID 형식을 정규식으로 검증한다: ^[a-z][a-z0-9_-]{1,30}$
	return &ModuleCell{id: id, description: description, cells: cells}
}

func (m *ModuleCell) Apply(c *Container) error {
	// 스코프 생성 — 실제 코드: scope := c.Scope(m.id)
	scope := c.Scope(m.id)
	fmt.Printf("    [module] 스코프 생성: %s (%s)\n", m.id, m.description)

	// 하위 셀 적용
	for _, cell := range m.cells {
		if err := cell.Apply(scope); err != nil {
			return fmt.Errorf("모듈 '%s'에서 셀 적용 실패: %w", m.id, err)
		}
	}
	return nil
}

func (m *ModuleCell) Info(indent int) string {
	prefix := strings.Repeat("  ", indent)
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%sModule: %s (%s)\n", prefix, m.id, m.description))
	for _, cell := range m.cells {
		sb.WriteString(cell.Info(indent + 1))
	}
	return sb.String()
}

// --- GroupCell (vendor/github.com/cilium/hive/cell/group.go) ---

// GroupCell은 스코프 없이 셀들을 묶는다.
// 실제 코드: type group []Cell
// Module과 달리 새 스코프를 생성하지 않는다.
type GroupCell struct {
	cells []Cell
}

func Group(cells ...Cell) Cell {
	return &GroupCell{cells: cells}
}

func (g *GroupCell) Apply(c *Container) error {
	for _, cell := range g.cells {
		if err := cell.Apply(c); err != nil {
			return err
		}
	}
	return nil
}

func (g *GroupCell) Info(indent int) string {
	var sb strings.Builder
	for _, cell := range g.cells {
		sb.WriteString(cell.Info(indent))
	}
	return sb.String()
}

// ─────────────────────────────────────────────────────────────────────────────
// 7. Hive 컨테이너 (vendor/github.com/cilium/hive/hive.go)
// ─────────────────────────────────────────────────────────────────────────────

// Hive는 모듈러 애플리케이션을 구축하는 프레임워크이다.
// 실제 코드: type Hive struct { container *dig.Container; cells []Cell; lifecycle Lifecycle; ... }
type Hive struct {
	container *Container
	cells     []Cell
	lifecycle *Lifecycle
	invokes   []invokeEntry // Invoke 셀에서 수집한 함수들
	populated bool
	shutdown  chan error
}

type invokeEntry struct {
	fn        interface{}
	container *Container // 어떤 스코프에서 실행할지
}

func NewHive(cells ...Cell) *Hive {
	h := &Hive{
		container: NewContainer("root"),
		cells:     cells,
		lifecycle: NewLifecycle(),
		shutdown:  make(chan error, 1),
	}

	// Lifecycle을 컨테이너에 기본 제공
	// 실제 코드: hive.provideDefaults()에서 Lifecycle, Shutdowner 등을 제공한다
	h.container.mu.Lock()
	lcType := reflect.TypeOf((*Lifecycle)(nil))
	h.container.instances[lcType] = reflect.ValueOf(h.lifecycle)
	h.container.mu.Unlock()

	// 셀 적용 — 생성자와 Invoke 등록
	for _, cell := range cells {
		if err := h.applyCell(cell, h.container); err != nil {
			panic(fmt.Sprintf("셀 적용 실패: %v", err))
		}
	}

	return h
}

// applyCell은 셀을 재귀적으로 적용하면서 InvokeCell의 함수를 수집한다.
func (h *Hive) applyCell(cell Cell, c *Container) error {
	switch v := cell.(type) {
	case *InvokeCell:
		// Invoke 함수를 수집 (나중에 Populate에서 실행)
		for _, fn := range v.fns {
			h.invokes = append(h.invokes, invokeEntry{fn: fn, container: c})
		}
		return nil
	case *ModuleCell:
		// 모듈은 스코프를 생성하고 하위 셀을 재귀 적용
		scope := c.Scope(v.id)
		fmt.Printf("  [hive] 모듈 스코프 생성: %s (%s)\n", v.id, v.description)

		// Lifecycle을 스코프에도 제공 (모듈별 augmented lifecycle 시뮬레이션)
		scope.mu.Lock()
		lcType := reflect.TypeOf((*Lifecycle)(nil))
		scope.instances[lcType] = reflect.ValueOf(h.lifecycle)
		scope.mu.Unlock()

		for _, child := range v.cells {
			if err := h.applyCell(child, scope); err != nil {
				return err
			}
		}
		return nil
	case *GroupCell:
		for _, child := range v.cells {
			if err := h.applyCell(child, c); err != nil {
				return err
			}
		}
		return nil
	default:
		return cell.Apply(c)
	}
}

// Populate는 객체 그래프를 인스턴스화한다.
// 실제 코드: Hive.Populate(log) error
// 설정 파싱 후 Invoke 함수들을 실행하여 객체를 구축한다.
func (h *Hive) Populate() error {
	if h.populated {
		return nil
	}
	h.populated = true

	fmt.Println("  [hive] Invoke 함수 실행 중...")
	for _, entry := range h.invokes {
		ct, err := newCtor(entry.fn)
		if err != nil {
			return fmt.Errorf("Invoke 함수 분석 실패: %w", err)
		}

		// 의존성 해석
		args := make([]reflect.Value, len(ct.inTypes))
		for i, inType := range ct.inTypes {
			resolved, resolveErr := entry.container.Resolve(inType)
			if resolveErr != nil {
				return fmt.Errorf("Invoke 의존성 해석 실패: %w", resolveErr)
			}
			args[i] = resolved
		}

		// 함수 호출
		results := ct.fnValue.Call(args)

		// 에러 체크
		if ct.hasError {
			errVal := results[len(results)-1]
			if !errVal.IsNil() {
				return fmt.Errorf("Invoke 실행 실패: %v", errVal.Interface())
			}
		}
	}
	return nil
}

// Start는 Hive를 시작한다.
// 실제 코드: Hive.Start(log, ctx) error
// 1) Populate (Invoke 실행) → 2) Lifecycle.Start
func (h *Hive) Start() error {
	fmt.Println("\n  [hive] === Populate 단계 ===")
	if err := h.Populate(); err != nil {
		return fmt.Errorf("Populate 실패: %w", err)
	}

	fmt.Println("\n  [hive] === Lifecycle Start 단계 ===")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return h.lifecycle.Start(ctx)
}

// Stop은 Hive를 중지한다.
// 실제 코드: Hive.Stop(log, ctx) error
func (h *Hive) Stop() error {
	fmt.Println("\n  [hive] === Lifecycle Stop 단계 ===")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return h.lifecycle.Stop(ctx)
}

// PrintObjects는 등록된 셀 구조를 출력한다.
// 실제 코드: Hive.PrintObjects(w, log) error
func (h *Hive) PrintObjects() {
	fmt.Println("  셀 구조:")
	for _, cell := range h.cells {
		fmt.Print(cell.Info(2))
	}
	h.lifecycle.PrintHooks()
}

// ─────────────────────────────────────────────────────────────────────────────
// 8. 의존성 그래프 분석 유틸리티
// ─────────────────────────────────────────────────────────────────────────────

// DepGraphAnalyzer는 컨테이너의 의존성 그래프를 분석한다.
type DepGraphAnalyzer struct {
	container *Container
}

// AnalyzeDependencies는 의존성 관계를 분석하여 출력한다.
func (a *DepGraphAnalyzer) AnalyzeDependencies() {
	a.analyzeContainer(a.container, 0)
}

func (a *DepGraphAnalyzer) analyzeContainer(c *Container, depth int) {
	prefix := strings.Repeat("  ", depth+2)
	fmt.Printf("%s스코프 '%s':\n", prefix, c.name)

	// 제공되는 타입 목록
	types := make([]string, 0, len(c.providers))
	for t, ct := range c.providers {
		privateStr := ""
		if c.private[t] {
			privateStr = " [private]"
		}
		deps := make([]string, len(ct.inTypes))
		for i, in := range ct.inTypes {
			deps[i] = in.String()
		}
		depStr := ""
		if len(deps) > 0 {
			depStr = fmt.Sprintf(" ← (%s)", strings.Join(deps, ", "))
		}
		types = append(types, fmt.Sprintf("%s  - %v%s%s", prefix, t, privateStr, depStr))
	}
	sort.Strings(types)
	for _, line := range types {
		fmt.Println(line)
	}

	// 하위 스코프
	for _, child := range c.children {
		a.analyzeContainer(child, depth+1)
	}
}

// DetectCycles는 순환 의존성을 감지한다. DFS 기반.
// 실제 Hive에서는 dig가 순환 감지를 수행한다.
func (a *DepGraphAnalyzer) DetectCycles() ([]string, bool) {
	visited := make(map[TypeKey]bool)
	inStack := make(map[TypeKey]bool)

	var cycle []string
	var dfs func(t TypeKey, c *Container) bool

	dfs = func(t TypeKey, c *Container) bool {
		visited[t] = true
		inStack[t] = true

		ct, found := c.providers[t]
		if !found && c.parent != nil {
			ct, found = c.parent.providers[t]
		}
		if found {
			for _, dep := range ct.inTypes {
				if !visited[dep] {
					if dfs(dep, c) {
						cycle = append([]string{t.String()}, cycle...)
						return true
					}
				} else if inStack[dep] {
					cycle = append(cycle, dep.String(), t.String())
					return true
				}
			}
		}

		inStack[t] = false
		return false
	}

	a.detectInContainer(a.container, dfs, visited)
	if len(cycle) > 0 {
		return cycle, true
	}
	return nil, false
}

func (a *DepGraphAnalyzer) detectInContainer(c *Container, dfs func(TypeKey, *Container) bool, visited map[TypeKey]bool) {
	for t := range c.providers {
		if !visited[t] {
			if dfs(t, c) {
				return
			}
		}
	}
	for _, child := range c.children {
		a.detectInContainer(child, dfs, visited)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 9. 시뮬레이션용 서비스 타입들 (Cilium 에이전트 컴포넌트 모방)
// ─────────────────────────────────────────────────────────────────────────────

// Config는 Cilium 에이전트 설정이다. (pkg/option/config.go의 DaemonConfig 모방)
type Config struct {
	Debug       bool
	ClusterName string
	TunnelMode  string
}

// Datastore는 상태 저장소이다. (StateDB 모방)
type Datastore struct {
	Name string
	Data map[string]string
}

// EndpointManager는 엔드포인트 관리자이다.
type EndpointManager struct {
	Store  *Datastore
	Config *Config
	Count  int
}

// PolicyEngine은 정책 엔진이다.
type PolicyEngine struct {
	EPManager *EndpointManager
	Config    *Config
	Rules     int
}

// ─────────────────────────────────────────────────────────────────────────────
// 10. main — 시나리오 실행
// ─────────────────────────────────────────────────────────────────────────────

func main() {
	fmt.Println("================================================================")
	fmt.Println("  Cilium Hive DI 프레임워크 시뮬레이션")
	fmt.Println("  reflect 기반 의존성 해석, Cell/Module/Lifecycle")
	fmt.Println("================================================================")

	// ==========================================
	// 시나리오 1: 기본 Provide/Invoke/Lifecycle
	// ==========================================
	fmt.Println("\n" + strings.Repeat("─", 60))
	fmt.Println("시나리오 1: 기본 Provide + Invoke + Lifecycle")
	fmt.Println(strings.Repeat("─", 60))
	fmt.Println()
	fmt.Println("  Cilium Hive에서 가장 기본적인 패턴:")
	fmt.Println("  1. cell.Provide(NewConfig) — 생성자 등록")
	fmt.Println("  2. cell.Invoke(func(cfg *Config, lc *Lifecycle) { ... }) — 초기화 함수")
	fmt.Println("  3. Lifecycle에 Hook을 등록하여 Start/Stop 관리")

	hive1 := NewHive(
		// Config 제공 (의존성 없음)
		Provide(func() *Config {
			fmt.Println("    [ctor] Config 생성")
			return &Config{
				Debug:       true,
				ClusterName: "cilium-cluster",
				TunnelMode:  "vxlan",
			}
		}),

		// Datastore 제공 (의존성 없음)
		Provide(func() *Datastore {
			fmt.Println("    [ctor] Datastore 생성")
			return &Datastore{
				Name: "statedb",
				Data: make(map[string]string),
			}
		}),

		// EndpointManager 제공 (Config + Datastore에 의존)
		// reflect가 매개변수 타입을 분석하여 의존성을 자동 해석한다
		Provide(func(cfg *Config, ds *Datastore) *EndpointManager {
			fmt.Printf("    [ctor] EndpointManager 생성 (클러스터: %s, 저장소: %s)\n",
				cfg.ClusterName, ds.Name)
			return &EndpointManager{
				Store:  ds,
				Config: cfg,
				Count:  0,
			}
		}),

		// Invoke: 객체 생성 + Lifecycle Hook 등록
		// 실제 Cilium에서 대부분의 모듈이 이 패턴을 사용한다
		Invoke(func(em *EndpointManager, lc *Lifecycle) {
			fmt.Println("    [invoke] EndpointManager 초기화 + Lifecycle Hook 등록")
			lc.Append(Hook{
				OnStart: func(ctx HookContext) error {
					em.Count = 5
					fmt.Printf("    [hook] EndpointManager 시작됨: %d개 엔드포인트 로드\n", em.Count)
					return nil
				},
				OnStop: func(ctx HookContext) error {
					fmt.Printf("    [hook] EndpointManager 종료: %d개 엔드포인트 정리\n", em.Count)
					em.Count = 0
					return nil
				},
			})
		}),
	)

	if err := hive1.Start(); err != nil {
		fmt.Printf("  오류: %v\n", err)
	} else {
		if err := hive1.Stop(); err != nil {
			fmt.Printf("  종료 오류: %v\n", err)
		}
	}

	// ==========================================
	// 시나리오 2: Module + 중첩 스코프 + ProvidePrivate
	// ==========================================
	fmt.Println("\n" + strings.Repeat("─", 60))
	fmt.Println("시나리오 2: Module 중첩 스코프 + ProvidePrivate")
	fmt.Println(strings.Repeat("─", 60))
	fmt.Println()
	fmt.Println("  실제 Cilium 에이전트 구조:")
	fmt.Println("  - Module(\"agent\") 안에 하위 Module들이 중첩")
	fmt.Println("  - 각 Module은 dig.Scope를 생성하여 이름 공간 분리")
	fmt.Println("  - ProvidePrivate는 해당 모듈 내부에서만 접근 가능")
	fmt.Println()

	hive2 := NewHive(
		// 루트에서 Config 제공 (모든 모듈에서 접근 가능)
		Provide(func() *Config {
			fmt.Println("    [ctor] Config 생성 (루트 스코프)")
			return &Config{Debug: false, ClusterName: "prod-cluster", TunnelMode: "geneve"}
		}),

		// controlplane 모듈
		Module("controlplane", "Control plane components",
			// Datastore는 controlplane 모듈 내부에서만 접근 (ProvidePrivate)
			ProvidePrivate(func() *Datastore {
				fmt.Println("    [ctor] Datastore 생성 (controlplane 스코프, private)")
				return &Datastore{Name: "controlplane-db", Data: make(map[string]string)}
			}),

			// EndpointManager는 외부에서도 접근 가능 (Provide)
			Provide(func(cfg *Config, ds *Datastore) *EndpointManager {
				fmt.Printf("    [ctor] EndpointManager 생성 (controlplane 스코프)\n")
				return &EndpointManager{Store: ds, Config: cfg}
			}),

			// endpoint-manager 하위 모듈
			Module("endpoint-manager", "Manages endpoints lifecycle",
				Invoke(func(em *EndpointManager, lc *Lifecycle) {
					fmt.Println("    [invoke] endpoint-manager 모듈 초기화")
					lc.AppendWithModule(Hook{
						OnStart: func(ctx HookContext) error {
							em.Count = 10
							fmt.Printf("    [hook] endpoint-manager 시작: %d개 엔드포인트\n", em.Count)
							return nil
						},
						OnStop: func(ctx HookContext) error {
							fmt.Printf("    [hook] endpoint-manager 종료\n")
							return nil
						},
					}, FullModuleID{"controlplane", "endpoint-manager"})
				}),
			),
		),

		// datapath 모듈
		Module("datapath", "Datapath management",
			Provide(func(cfg *Config) *PolicyEngine {
				fmt.Printf("    [ctor] PolicyEngine 생성 (datapath 스코프)\n")
				return &PolicyEngine{Config: cfg, Rules: 42}
			}),

			Invoke(func(pe *PolicyEngine, lc *Lifecycle) {
				fmt.Println("    [invoke] datapath 모듈 초기화")
				lc.AppendWithModule(Hook{
					OnStart: func(ctx HookContext) error {
						fmt.Printf("    [hook] datapath 시작: %d개 정책 규칙 로드\n", pe.Rules)
						return nil
					},
					OnStop: func(ctx HookContext) error {
						fmt.Printf("    [hook] datapath 종료\n")
						return nil
					},
				}, FullModuleID{"datapath"})
			}),
		),
	)

	if err := hive2.Start(); err != nil {
		fmt.Printf("  오류: %v\n", err)
	} else {
		fmt.Println("\n  등록된 훅 상태:")
		hive2.PrintObjects()
		if err := hive2.Stop(); err != nil {
			fmt.Printf("  종료 오류: %v\n", err)
		}
	}

	// ==========================================
	// 시나리오 3: 누락된 의존성 감지
	// ==========================================
	fmt.Println("\n" + strings.Repeat("─", 60))
	fmt.Println("시나리오 3: 누락된 의존성 감지")
	fmt.Println(strings.Repeat("─", 60))
	fmt.Println()
	fmt.Println("  생성자가 존재하지 않는 타입을 요구하면 시작 시 즉시 오류를 반환한다.")
	fmt.Println("  실제 Hive에서는 ErrPopulate{} 타입으로 감싸서 반환한다.")
	fmt.Println()

	hive3 := NewHive(
		// PolicyEngine은 *EndpointManager에 의존하지만, 제공하는 곳이 없음
		Provide(func(em *EndpointManager) *PolicyEngine {
			return &PolicyEngine{EPManager: em}
		}),

		Invoke(func(pe *PolicyEngine) {
			fmt.Println("이 코드는 실행되지 않아야 한다")
		}),
	)

	if err := hive3.Start(); err != nil {
		fmt.Printf("  예상된 오류: %v\n", err)
	}

	// ==========================================
	// 시나리오 4: Lifecycle 역순 종료 보장
	// ==========================================
	fmt.Println("\n" + strings.Repeat("─", 60))
	fmt.Println("시나리오 4: Lifecycle 역순 종료 보장")
	fmt.Println(strings.Repeat("─", 60))
	fmt.Println()
	fmt.Println("  Hive의 핵심 설계: Start는 등록 순서대로, Stop은 역순으로 실행.")
	fmt.Println("  이유: 나중에 시작된 컴포넌트가 먼저 종료되어야")
	fmt.Println("  의존하는 컴포넌트가 아직 살아있는 상태에서 정리할 수 있다.")
	fmt.Println()

	lc := NewLifecycle()
	services := []string{"DB", "Cache", "API-Server", "Health-Checker"}

	for _, svc := range services {
		name := svc // 클로저용 복사
		lc.Append(Hook{
			OnStart: func(ctx HookContext) error {
				fmt.Printf("    시작: %s\n", name)
				return nil
			},
			OnStop: func(ctx HookContext) error {
				fmt.Printf("    종료: %s\n", name)
				return nil
			},
		})
	}

	fmt.Println("  Start 순서:")
	ctx := context.Background()
	lc.Start(ctx)
	fmt.Println("\n  Stop 순서 (역순):")
	lc.Stop(ctx)

	// ==========================================
	// 시나리오 5: 의존성 그래프 시각화
	// ==========================================
	fmt.Println("\n" + strings.Repeat("─", 60))
	fmt.Println("시나리오 5: 의존성 그래프 시각화")
	fmt.Println(strings.Repeat("─", 60))

	fmt.Println(`
  실제 Cilium 에이전트의 Hive 구조 (단순화):

  Hive (root container)
  ├── Provide: *Config (의존성 없음)
  │
  ├── Module: "controlplane"
  │   ├── ProvidePrivate: *Datastore (스코프 내부만)
  │   ├── Provide: *EndpointManager ← (*Config, *Datastore)
  │   └── Module: "endpoint-manager"
  │       └── Invoke: 초기화 ← (*EndpointManager, *Lifecycle)
  │
  └── Module: "datapath"
      ├── Provide: *PolicyEngine ← (*Config)
      └── Invoke: 초기화 ← (*PolicyEngine, *Lifecycle)

  의존성 해석 순서:
  ┌────────┐
  │ Config │ (루트 스코프, 모든 모듈에서 접근 가능)
  └───┬────┘
      │
      ├──────────────────────────────────┐
      ▼                                  ▼
  ┌──────────┐                    ┌──────────────┐
  │Datastore │ (controlplane     │ PolicyEngine  │ (datapath 스코프)
  │(private) │  스코프에서만)     │               │
  └────┬─────┘                    └──────────────┘
       │
       ▼
  ┌─────────────────┐
  │EndpointManager  │ ← Config + Datastore
  └─────────────────┘

  Lifecycle Hook 실행 순서:
  Start: endpoint-manager → datapath (등록 순)
  Stop:  datapath → endpoint-manager (역순)`)

	fmt.Println("\n\n  Hive DI 시뮬레이션 완료.")
}
