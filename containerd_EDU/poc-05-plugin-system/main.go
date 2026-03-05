// containerd 플러그인 의존성 그래프 시뮬레이션
//
// containerd의 플러그인 시스템은 Registration 구조체 기반으로 동작하며,
// Graph() 함수가 DFS 알고리즘으로 의존성 순서를 계산하여 초기화 순서를 결정한다.
//
// 참조 소스코드:
//   - vendor/github.com/containerd/plugin/plugin.go   (Registration, Registry, Graph, children, DisableFilter)
//   - vendor/github.com/containerd/plugin/context.go   (InitContext, Meta, Plugin, Set, GetSingle, GetByType, GetByID)

package main

import (
	"errors"
	"fmt"
	"strings"
)

// ============================================================================
// 1. 핵심 타입 정의
// 참조: vendor/github.com/containerd/plugin/plugin.go (Type, Registration, Registry)
// ============================================================================

// Type은 플러그인의 타입을 나타낸다.
// containerd에서는 "io.containerd.content.v1", "io.containerd.snapshotter.v1" 등의 형태로 사용.
type Type string

func (t Type) String() string { return string(t) }

// 플러그인 타입 상수
// 실제 containerd에서는 plugins/ 패키지에 정의됨
const (
	ContentPlugin    Type = "io.containerd.content.v1"
	SnapshotPlugin   Type = "io.containerd.snapshotter.v1"
	MetadataPlugin   Type = "io.containerd.metadata.v1"
	RuntimePlugin    Type = "io.containerd.runtime.v2"
	ServicePlugin    Type = "io.containerd.service.v1"
	GCPlugin         Type = "io.containerd.gc.v1"
	EventPlugin      Type = "io.containerd.event.v1"
	LeasePlugin      Type = "io.containerd.lease.v1"
	WildcardDep      Type = "*" // 와일드카드: 모든 플러그인에 의존
)

// Meta는 플러그인이 초기화 시 채우는 메타데이터.
// 참조: vendor/github.com/containerd/plugin/context.go - Meta struct
type Meta struct {
	Exports      map[string]string // 플러그인이 노출하는 값
	Capabilities []string          // 플러그인의 기능 스위치
}

// Registration은 플러그인 등록 정보를 담는 구조체.
// 참조: vendor/github.com/containerd/plugin/plugin.go - Registration struct
type Registration struct {
	Type     Type
	ID       string
	Config   interface{}
	Requires []Type // 이 플러그인이 필요로 하는 다른 플러그인 타입 목록

	// InitFn은 플러그인 초기화 시 호출되는 함수.
	// InitContext를 받아서 플러그인 인스턴스를 반환한다.
	InitFn func(*InitContext) (interface{}, error)
}

// URI는 "Type.ID" 형태의 고유 식별자를 반환.
// 참조: plugin.go - func (r *Registration) URI() string
func (r *Registration) URI() string {
	return r.Type.String() + "." + r.ID
}

// Init은 등록된 플러그인을 초기화한다.
// 참조: plugin.go - func (r Registration) Init(ic *InitContext) *Plugin
func (r Registration) Init(ic *InitContext) *Plugin {
	p, err := r.InitFn(ic)
	return &Plugin{
		Registration: r,
		Config:       ic.Config,
		Meta:         *ic.Meta,
		instance:     p,
		err:          err,
	}
}

// ============================================================================
// 2. DisableFilter: 특정 플러그인 비활성화
// 참조: plugin.go - type DisableFilter func(r *Registration) bool
// ============================================================================

// DisableFilter는 비활성화할 플러그인을 판별하는 함수 타입.
// true를 반환하면 해당 플러그인은 Graph에서 제외된다.
type DisableFilter func(r *Registration) bool

// ============================================================================
// 3. Registry와 Graph (DFS 의존성 정렬)
// 참조: plugin.go - Registry, Graph(), children(), Register(), checkUnique()
// ============================================================================

// Registry는 등록된 플러그인 목록이다.
// containerd에서는 불변(immutable)이며, Register 시 새 슬라이스를 반환한다.
type Registry []*Registration

var (
	ErrNoType       = errors.New("plugin: no type")
	ErrNoPluginID   = errors.New("plugin: no id")
	ErrIDRegistered = errors.New("plugin: id already registered")
	ErrInvalidReq   = errors.New("invalid requires")
)

// Register는 새 플러그인을 Registry에 추가한다.
// 참조: plugin.go - func (registry Registry) Register(r *Registration) Registry
func (registry Registry) Register(r *Registration) Registry {
	if r.Type == "" {
		panic(ErrNoType)
	}
	if r.ID == "" {
		panic(ErrNoPluginID)
	}
	// 중복 URI 검사
	for _, registered := range registry {
		if r.URI() == registered.URI() {
			panic(fmt.Errorf("%s: %w", r.URI(), ErrIDRegistered))
		}
	}
	// 와일드카드("*")는 단독으로만 사용 가능
	for _, requires := range r.Requires {
		if requires == "*" && len(r.Requires) != 1 {
			panic(ErrInvalidReq)
		}
	}
	return append(registry, r)
}

// Graph는 DFS 기반으로 의존성 순서에 따른 초기화 순서 목록을 반환한다.
// filter에 의해 비활성화된 플러그인은 제외된다.
// 참조: plugin.go - func (registry Registry) Graph(filter DisableFilter) []Registration
func (registry Registry) Graph(filter DisableFilter) []Registration {
	disabled := map[*Registration]bool{}
	for _, r := range registry {
		if filter(r) {
			disabled[r] = true
		}
	}

	ordered := make([]Registration, 0, len(registry)-len(disabled))
	added := map[*Registration]bool{}
	for _, r := range registry {
		if disabled[r] {
			continue
		}
		// DFS: 의존성을 먼저 추가
		children(r, registry, added, disabled, &ordered)
		if !added[r] {
			ordered = append(ordered, *r)
			added[r] = true
		}
	}
	return ordered
}

// children은 재귀적으로 의존성 플러그인을 ordered에 추가하는 DFS 핵심 함수.
// 참조: plugin.go - func children(reg *Registration, registry []*Registration, ...)
func children(reg *Registration, registry []*Registration, added, disabled map[*Registration]bool, ordered *[]Registration) {
	for _, t := range reg.Requires {
		for _, r := range registry {
			// 비활성화되지 않았고, 자기 자신이 아니며, 타입이 일치하거나 와일드카드("*")인 경우
			if !disabled[r] && r.URI() != reg.URI() && (t == "*" || r.Type == t) {
				children(r, registry, added, disabled, ordered)
				if !added[r] {
					*ordered = append(*ordered, *r)
					added[r] = true
				}
			}
		}
	}
}

// ============================================================================
// 4. Plugin, Set, InitContext (초기화 컨텍스트)
// 참조: context.go - Plugin, Set, InitContext, GetSingle, GetByType, GetByID
// ============================================================================

// Plugin은 초기화된 플러그인을 나타낸다.
type Plugin struct {
	Registration Registration
	Config       interface{}
	Meta         Meta
	instance     interface{}
	err          error
}

func (p *Plugin) Instance() (interface{}, error) {
	return p.instance, p.err
}

func (p *Plugin) Err() error {
	return p.err
}

// Set은 초기화된 플러그인의 순서가 보장된 컬렉션이다.
// 참조: context.go - Set struct
type Set struct {
	ordered     []*Plugin
	byTypeAndID map[Type]map[string]*Plugin
}

func NewPluginSet() *Set {
	return &Set{
		byTypeAndID: make(map[Type]map[string]*Plugin),
	}
}

// Add는 초기화된 플러그인을 Set에 추가한다.
func (ps *Set) Add(p *Plugin) error {
	if byID, ok := ps.byTypeAndID[p.Registration.Type]; !ok {
		ps.byTypeAndID[p.Registration.Type] = map[string]*Plugin{
			p.Registration.ID: p,
		}
	} else if _, exists := byID[p.Registration.ID]; !exists {
		byID[p.Registration.ID] = p
	} else {
		return fmt.Errorf("plugin %s already initialized", p.Registration.URI())
	}
	ps.ordered = append(ps.ordered, p)
	return nil
}

func (ps *Set) Get(t Type, id string) *Plugin {
	if byID, ok := ps.byTypeAndID[t]; ok {
		return byID[id]
	}
	return nil
}

// InitContext는 플러그인 초기화 시 전달되는 컨텍스트.
// GetSingle, GetByType, GetByID 메서드를 통해 이미 초기화된 다른 플러그인을 조회할 수 있다.
// 참조: context.go - InitContext struct
type InitContext struct {
	Config     interface{}
	Meta       *Meta
	Properties map[string]string
	plugins    *Set
}

func NewContext(plugins *Set, properties map[string]string) *InitContext {
	if properties == nil {
		properties = map[string]string{}
	}
	return &InitContext{
		Properties: properties,
		Meta: &Meta{
			Exports:      map[string]string{},
			Capabilities: nil,
		},
		plugins: plugins,
	}
}

// GetSingle은 주어진 타입의 플러그인 인스턴스가 하나만 있을 때 반환한다.
// 참조: context.go - func (i *InitContext) GetSingle(t Type) (interface{}, error)
func (ic *InitContext) GetSingle(t Type) (interface{}, error) {
	var (
		found    bool
		instance interface{}
	)
	for _, v := range ic.plugins.byTypeAndID[t] {
		i, err := v.Instance()
		if err != nil {
			return nil, err
		}
		if found {
			return nil, fmt.Errorf("multiple plugins registered for %s", t)
		}
		instance = i
		found = true
	}
	if !found {
		return nil, fmt.Errorf("no plugins registered for %s", t)
	}
	return instance, nil
}

// GetByID는 주어진 타입과 ID로 특정 플러그인 인스턴스를 반환한다.
// 참조: context.go - func (i *InitContext) GetByID(t Type, id string) (interface{}, error)
func (ic *InitContext) GetByID(t Type, id string) (interface{}, error) {
	p := ic.plugins.Get(t, id)
	if p == nil {
		return nil, fmt.Errorf("no plugins registered for %s.%s", t, id)
	}
	return p.Instance()
}

// GetByType은 주어진 타입의 모든 플러그인 인스턴스를 반환한다.
// 참조: context.go - func (i *InitContext) GetByType(t Type) (map[string]interface{}, error)
func (ic *InitContext) GetByType(t Type) (map[string]interface{}, error) {
	result := map[string]interface{}{}
	for id, p := range ic.plugins.byTypeAndID[t] {
		i, err := p.Instance()
		if err != nil {
			return nil, err
		}
		result[id] = i
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("no plugins registered for %s", t)
	}
	return result, nil
}

// ============================================================================
// 5. 시뮬레이션 플러그인 인스턴스
// ============================================================================

type ContentStore struct{ Name string }
type Snapshotter struct{ Name string }
type MetadataDB struct{ Name string }
type RuntimeV2 struct{ Name string }
type TaskService struct{ Name string }
type GarbageCollector struct{ Name string }
type EventExchange struct{ Name string }
type LeaseManager struct{ Name string }

// ============================================================================
// 6. main: 전체 시나리오 실행
// ============================================================================

func main() {
	fmt.Println("========================================")
	fmt.Println("containerd 플러그인 의존성 그래프 시뮬레이션")
	fmt.Println("========================================")
	fmt.Println()

	// ----- 시나리오 1: 플러그인 등록 및 의존성 정렬 -----
	fmt.Println("--- 시나리오 1: 플러그인 등록 및 Graph() 의존성 정렬 ---")
	fmt.Println()

	var reg Registry

	// Content 플러그인: 의존성 없음 (가장 기본)
	reg = reg.Register(&Registration{
		Type:     ContentPlugin,
		ID:       "content",
		Requires: nil,
		InitFn: func(ic *InitContext) (interface{}, error) {
			ic.Meta.Exports["root"] = "/var/lib/containerd/io.containerd.content.v1"
			return &ContentStore{Name: "content-store"}, nil
		},
	})

	// Event 플러그인: 의존성 없음
	reg = reg.Register(&Registration{
		Type:     EventPlugin,
		ID:       "exchange",
		Requires: nil,
		InitFn: func(ic *InitContext) (interface{}, error) {
			return &EventExchange{Name: "event-exchange"}, nil
		},
	})

	// Snapshot 플러그인: Content에 의존
	reg = reg.Register(&Registration{
		Type:     SnapshotPlugin,
		ID:       "overlayfs",
		Requires: []Type{ContentPlugin},
		InitFn: func(ic *InitContext) (interface{}, error) {
			// GetSingle로 Content 플러그인 조회
			cs, err := ic.GetSingle(ContentPlugin)
			if err != nil {
				return nil, fmt.Errorf("snapshotter requires content store: %w", err)
			}
			ic.Meta.Capabilities = append(ic.Meta.Capabilities, "overlay", "hardlink")
			fmt.Printf("    [overlayfs] Content store 참조: %v\n", cs)
			return &Snapshotter{Name: "overlayfs"}, nil
		},
	})

	// Lease 플러그인: Content에 의존
	reg = reg.Register(&Registration{
		Type:     LeasePlugin,
		ID:       "manager",
		Requires: []Type{ContentPlugin},
		InitFn: func(ic *InitContext) (interface{}, error) {
			return &LeaseManager{Name: "lease-manager"}, nil
		},
	})

	// Metadata 플러그인: Content, Snapshot에 의존
	reg = reg.Register(&Registration{
		Type:     MetadataPlugin,
		ID:       "bolt",
		Requires: []Type{ContentPlugin, SnapshotPlugin},
		InitFn: func(ic *InitContext) (interface{}, error) {
			cs, _ := ic.GetSingle(ContentPlugin)
			snaps, _ := ic.GetByType(SnapshotPlugin)
			fmt.Printf("    [bolt] Content: %v, Snapshotters: %v\n", cs, snaps)
			return &MetadataDB{Name: "bolt-db"}, nil
		},
	})

	// GC 플러그인: Metadata에 의존
	reg = reg.Register(&Registration{
		Type:     GCPlugin,
		ID:       "scheduler",
		Requires: []Type{MetadataPlugin},
		InitFn: func(ic *InitContext) (interface{}, error) {
			return &GarbageCollector{Name: "gc-scheduler"}, nil
		},
	})

	// Runtime 플러그인: Metadata, Event에 의존
	reg = reg.Register(&Registration{
		Type:     RuntimePlugin,
		ID:       "task",
		Requires: []Type{MetadataPlugin, EventPlugin},
		InitFn: func(ic *InitContext) (interface{}, error) {
			meta, _ := ic.GetSingle(MetadataPlugin)
			evt, _ := ic.GetSingle(EventPlugin)
			fmt.Printf("    [runtime] Metadata: %v, Event: %v\n", meta, evt)
			return &RuntimeV2{Name: "runtime-v2"}, nil
		},
	})

	// Tasks Service 플러그인: Runtime, Event, Metadata에 의존
	reg = reg.Register(&Registration{
		Type:     ServicePlugin,
		ID:       "tasks-service",
		Requires: []Type{RuntimePlugin, EventPlugin, MetadataPlugin},
		InitFn: func(ic *InitContext) (interface{}, error) {
			rt, _ := ic.GetByID(RuntimePlugin, "task")
			fmt.Printf("    [tasks-service] Runtime: %v\n", rt)
			return &TaskService{Name: "tasks-service"}, nil
		},
	})

	fmt.Printf("  등록된 플러그인 수: %d\n\n", len(reg))

	// 모든 플러그인 활성화 (필터 없음)
	noFilter := func(r *Registration) bool { return false }
	ordered := reg.Graph(noFilter)

	fmt.Println("  Graph() DFS 의존성 정렬 결과 (초기화 순서):")
	for i, r := range ordered {
		deps := "없음"
		if len(r.Requires) > 0 {
			depStrs := make([]string, len(r.Requires))
			for j, t := range r.Requires {
				depStrs[j] = string(t)
			}
			deps = strings.Join(depStrs, ", ")
		}
		fmt.Printf("    %2d. %-55s [의존: %s]\n", i+1, r.URI(), deps)
	}

	// ----- 실제 초기화 수행 -----
	fmt.Println()
	fmt.Println("  순서대로 플러그인 초기화 실행:")
	initialized := NewPluginSet()
	for i, r := range ordered {
		ic := NewContext(initialized, map[string]string{
			"root":  "/var/lib/containerd",
			"state": "/run/containerd",
		})
		result := r.Init(ic)
		if err := initialized.Add(result); err != nil {
			fmt.Printf("    %2d. [실패] %s: %v\n", i+1, r.URI(), err)
			continue
		}
		if result.Err() != nil {
			fmt.Printf("    %2d. [에러] %s: %v\n", i+1, r.URI(), result.Err())
		} else {
			inst, _ := result.Instance()
			capStr := ""
			if len(result.Meta.Capabilities) > 0 {
				capStr = fmt.Sprintf(" (capabilities: %v)", result.Meta.Capabilities)
			}
			expStr := ""
			if len(result.Meta.Exports) > 0 {
				expStr = fmt.Sprintf(" (exports: %v)", result.Meta.Exports)
			}
			fmt.Printf("    %2d. [성공] %-50s → %T%s%s\n", i+1, r.URI(), inst, capStr, expStr)
		}
	}

	// ----- 시나리오 2: DisableFilter로 특정 플러그인 비활성화 -----
	fmt.Println()
	fmt.Println("--- 시나리오 2: DisableFilter로 GC, Lease 플러그인 비활성화 ---")
	fmt.Println()

	disableFilter := func(r *Registration) bool {
		return r.Type == GCPlugin || r.Type == LeasePlugin
	}
	filtered := reg.Graph(disableFilter)

	fmt.Println("  비활성화 후 초기화 순서:")
	for i, r := range filtered {
		fmt.Printf("    %2d. %s\n", i+1, r.URI())
	}
	fmt.Printf("  원래 %d개 → 필터 후 %d개\n", len(ordered), len(filtered))

	// ----- 시나리오 3: 와일드카드 의존성 ("*") -----
	fmt.Println()
	fmt.Println("--- 시나리오 3: 와일드카드 의존성 (\"*\") ---")
	fmt.Println()

	var wcReg Registry
	wcReg = wcReg.Register(&Registration{
		Type: "base.v1", ID: "alpha",
		InitFn: func(ic *InitContext) (interface{}, error) { return "alpha", nil },
	})
	wcReg = wcReg.Register(&Registration{
		Type: "base.v1", ID: "beta",
		InitFn: func(ic *InitContext) (interface{}, error) { return "beta", nil },
	})
	wcReg = wcReg.Register(&Registration{
		Type: "ext.v1", ID: "gamma",
		InitFn: func(ic *InitContext) (interface{}, error) { return "gamma", nil },
	})
	// 와일드카드("*"): 모든 다른 플러그인에 의존 → 가장 마지막에 초기화
	wcReg = wcReg.Register(&Registration{
		Type: "monitor.v1", ID: "wildcard-dep",
		Requires: []Type{"*"},
		InitFn: func(ic *InitContext) (interface{}, error) {
			return "wildcard-monitor", nil
		},
	})

	wcOrdered := wcReg.Graph(func(r *Registration) bool { return false })
	fmt.Println("  와일드카드 의존성으로 인해 모든 플러그인 뒤에 배치:")
	for i, r := range wcOrdered {
		deps := "없음"
		if len(r.Requires) > 0 {
			depStrs := make([]string, len(r.Requires))
			for j, t := range r.Requires {
				depStrs[j] = string(t)
			}
			deps = strings.Join(depStrs, ", ")
		}
		fmt.Printf("    %d. %-30s [의존: %s]\n", i+1, r.URI(), deps)
	}

	// ----- 시나리오 4: GetSingle, GetByType, GetByID 조회 -----
	fmt.Println()
	fmt.Println("--- 시나리오 4: InitContext 조회 메서드 ---")
	fmt.Println()

	ic := NewContext(initialized, nil)

	// GetSingle: 단일 인스턴스 조회
	if cs, err := ic.GetSingle(ContentPlugin); err == nil {
		fmt.Printf("  GetSingle(Content):  %v\n", cs)
	}

	// GetByID: 특정 플러그인 조회
	if rt, err := ic.GetByID(RuntimePlugin, "task"); err == nil {
		fmt.Printf("  GetByID(Runtime, \"task\"): %v\n", rt)
	}

	// GetByType: 타입별 모든 인스턴스 조회
	if snaps, err := ic.GetByType(SnapshotPlugin); err == nil {
		fmt.Printf("  GetByType(Snapshot): %v\n", snaps)
	}

	// 존재하지 않는 플러그인 조회 시 에러
	if _, err := ic.GetByID(ContentPlugin, "nonexistent"); err != nil {
		fmt.Printf("  GetByID(Content, \"nonexistent\"): 에러 → %v\n", err)
	}

	fmt.Println()
	fmt.Println("========================================")
	fmt.Println("시뮬레이션 완료")
	fmt.Println("========================================")
}
