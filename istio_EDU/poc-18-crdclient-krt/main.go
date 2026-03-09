package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Istio CRD Client + krt 프레임워크 시뮬레이션
//
// 실제 소스 참조:
//   - pilot/pkg/config/kube/crdclient/client.go → CRD CRUD + Watch
//   - pkg/kube/krt/collection.go                → krt.Collection, NewCollection
//   - pkg/kube/krt/index.go                     → krt.Index
//   - pkg/kube/krt/join.go                      → krt.JoinCollection
//   - pkg/kube/krt/filter.go                    → krt.FilterXxx
//
// 핵심 알고리즘:
//   1. ConfigStore: CRD → config.Config 변환 + Watch 이벤트
//   2. krt.Collection: 선언적 데이터 변환 파이프라인 (map/filter)
//   3. krt.Index: 특정 키로 O(1) 조회
//   4. 변경 전파: 입력 변경 → 파생 컬렉션 자동 갱신 → 핸들러 호출
// =============================================================================

// --- Config 모델 (Istio 내부 설정 표현) ---

type GroupVersionKind struct {
	Group   string
	Version string
	Kind    string
}

func (g GroupVersionKind) String() string {
	return fmt.Sprintf("%s/%s/%s", g.Group, g.Version, g.Kind)
}

var (
	GVKVirtualService   = GroupVersionKind{"networking.istio.io", "v1alpha3", "VirtualService"}
	GVKDestinationRule  = GroupVersionKind{"networking.istio.io", "v1alpha3", "DestinationRule"}
	GVKGateway          = GroupVersionKind{"networking.istio.io", "v1alpha3", "Gateway"}
)

type Config struct {
	GVK             GroupVersionKind
	Name            string
	Namespace       string
	ResourceVersion string
	Generation      int64
	Labels          map[string]string
	Annotations     map[string]string
	Spec            map[string]interface{}
}

func (c Config) Key() string {
	return fmt.Sprintf("%s/%s/%s", c.GVK.Kind, c.Namespace, c.Name)
}

// --- ConfigStore: CRD Client 시뮬레이션 ---
// 실제: pilot/pkg/config/kube/crdclient/client.go

type EventType int

const (
	EventAdd EventType = iota
	EventUpdate
	EventDelete
)

func (e EventType) String() string {
	switch e {
	case EventAdd:
		return "ADD"
	case EventUpdate:
		return "UPDATE"
	case EventDelete:
		return "DELETE"
	}
	return "UNKNOWN"
}

type ConfigEvent struct {
	Type   EventType
	Config Config
	Old    *Config // UPDATE 시 이전 값
}

type EventHandler func(ConfigEvent)

type ConfigStore struct {
	mu       sync.RWMutex
	configs  map[string]Config // key → Config
	handlers []EventHandler
	revCount int64
}

func NewConfigStore() *ConfigStore {
	return &ConfigStore{
		configs: make(map[string]Config),
	}
}

func (cs *ConfigStore) RegisterHandler(handler EventHandler) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.handlers = append(cs.handlers, handler)
}

func (cs *ConfigStore) Create(cfg Config) error {
	cs.mu.Lock()
	key := cfg.Key()
	if _, exists := cs.configs[key]; exists {
		cs.mu.Unlock()
		return fmt.Errorf("already exists: %s", key)
	}
	cs.revCount++
	cfg.ResourceVersion = fmt.Sprintf("%d", cs.revCount)
	cs.configs[key] = cfg
	handlers := append([]EventHandler{}, cs.handlers...)
	cs.mu.Unlock()

	event := ConfigEvent{Type: EventAdd, Config: cfg}
	for _, h := range handlers {
		h(event)
	}
	return nil
}

func (cs *ConfigStore) Update(cfg Config) error {
	cs.mu.Lock()
	key := cfg.Key()
	old, exists := cs.configs[key]
	if !exists {
		cs.mu.Unlock()
		return fmt.Errorf("not found: %s", key)
	}
	cs.revCount++
	cfg.ResourceVersion = fmt.Sprintf("%d", cs.revCount)
	cfg.Generation = old.Generation + 1
	cs.configs[key] = cfg
	handlers := append([]EventHandler{}, cs.handlers...)
	cs.mu.Unlock()

	event := ConfigEvent{Type: EventUpdate, Config: cfg, Old: &old}
	for _, h := range handlers {
		h(event)
	}
	return nil
}

func (cs *ConfigStore) Delete(gvk GroupVersionKind, name, namespace string) error {
	cs.mu.Lock()
	key := fmt.Sprintf("%s/%s/%s", gvk.Kind, namespace, name)
	cfg, exists := cs.configs[key]
	if !exists {
		cs.mu.Unlock()
		return fmt.Errorf("not found: %s", key)
	}
	delete(cs.configs, key)
	handlers := append([]EventHandler{}, cs.handlers...)
	cs.mu.Unlock()

	event := ConfigEvent{Type: EventDelete, Config: cfg}
	for _, h := range handlers {
		h(event)
	}
	return nil
}

func (cs *ConfigStore) Get(gvk GroupVersionKind, name, namespace string) *Config {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	key := fmt.Sprintf("%s/%s/%s", gvk.Kind, namespace, name)
	if cfg, ok := cs.configs[key]; ok {
		return &cfg
	}
	return nil
}

func (cs *ConfigStore) List(gvk GroupVersionKind, namespace string) []Config {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	var result []Config
	for _, cfg := range cs.configs {
		if cfg.GVK != gvk {
			continue
		}
		if namespace != "" && cfg.Namespace != namespace {
			continue
		}
		result = append(result, cfg)
	}
	return result
}

// --- krt.Collection 시뮬레이션 ---
// 실제: pkg/kube/krt/collection.go

// Collection은 선언적 데이터 변환 파이프라인의 기본 단위이다.
// 입력 컬렉션이 변경되면 변환 함수를 자동으로 재실행하여 출력을 갱신한다.
type Collection[T any] struct {
	mu       sync.RWMutex
	name     string
	items    map[string]T
	keyFunc  func(T) string
	handlers []func(EventType, T)
}

func NewCollection[T any](name string, keyFunc func(T) string) *Collection[T] {
	return &Collection[T]{
		name:    name,
		items:   make(map[string]T),
		keyFunc: keyFunc,
	}
}

func (c *Collection[T]) Set(item T) {
	c.mu.Lock()
	key := c.keyFunc(item)
	_, exists := c.items[key]
	c.items[key] = item
	handlers := append([]func(EventType, T){}, c.handlers...)
	c.mu.Unlock()

	eventType := EventAdd
	if exists {
		eventType = EventUpdate
	}
	for _, h := range handlers {
		h(eventType, item)
	}
}

func (c *Collection[T]) Delete(key string) {
	c.mu.Lock()
	item, exists := c.items[key]
	if !exists {
		c.mu.Unlock()
		return
	}
	delete(c.items, key)
	handlers := append([]func(EventType, T){}, c.handlers...)
	c.mu.Unlock()

	for _, h := range handlers {
		h(EventDelete, item)
	}
}

func (c *Collection[T]) List() []T {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make([]T, 0, len(c.items))
	for _, item := range c.items {
		result = append(result, item)
	}
	return result
}

func (c *Collection[T]) Get(key string) (T, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	item, ok := c.items[key]
	return item, ok
}

func (c *Collection[T]) RegisterHandler(handler func(EventType, T)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handlers = append(c.handlers, handler)
}

// --- krt.MapCollection: 변환 파이프라인 ---
// 실제: krt.NewCollection(input, transformFunc)

// MapCollection은 입력 컬렉션의 각 요소를 변환하여 파생 컬렉션을 생성한다.
func MapCollection[In any, Out any](
	name string,
	input *Collection[In],
	transform func(In) *Out,
	keyFunc func(Out) string,
) *Collection[Out] {
	output := NewCollection[Out](name, keyFunc)

	// 기존 항목 변환
	for _, item := range input.List() {
		if result := transform(item); result != nil {
			output.Set(*result)
		}
	}

	// 변경 감지 핸들러 등록
	input.RegisterHandler(func(eventType EventType, item In) {
		if eventType == EventDelete {
			// 삭제 시 출력에서도 제거 (간단한 구현)
			return
		}
		if result := transform(item); result != nil {
			output.Set(*result)
		}
	})

	return output
}

// --- krt.Index: 인덱스 기반 조회 ---
// 실제: pkg/kube/krt/index.go

type Index[T any] struct {
	mu         sync.RWMutex
	name       string
	collection *Collection[T]
	indexFunc  func(T) []string
	index      map[string][]T
}

func NewIndex[T any](name string, collection *Collection[T], indexFunc func(T) []string) *Index[T] {
	idx := &Index[T]{
		name:       name,
		collection: collection,
		indexFunc:  indexFunc,
		index:      make(map[string][]T),
	}

	// 기존 항목 인덱싱
	for _, item := range collection.List() {
		for _, key := range indexFunc(item) {
			idx.index[key] = append(idx.index[key], item)
		}
	}

	// 변경 감지
	collection.RegisterHandler(func(eventType EventType, item T) {
		idx.mu.Lock()
		defer idx.mu.Unlock()
		keys := indexFunc(item)
		for _, key := range keys {
			if eventType == EventDelete {
				// 간단한 구현: rebuild
				idx.index[key] = nil
			} else {
				idx.index[key] = append(idx.index[key], item)
			}
		}
	})

	return idx
}

func (idx *Index[T]) Lookup(key string) []T {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.index[key]
}

// --- 시뮬레이션 모델 ---

type VirtualServiceSpec struct {
	Hosts    []string
	Gateways []string
	Routes   []string
}

type RouteEntry struct {
	Host     string
	Namespace string
	Gateway  string
	Routes   []string
}

func (r RouteEntry) Key() string {
	return fmt.Sprintf("%s/%s", r.Namespace, r.Host)
}

// --- 메인 함수 ---

func main() {
	fmt.Println("=== Istio CRD Client + krt 프레임워크 시뮬레이션 ===")
	fmt.Println()

	// 1. ConfigStore 생성 (CRD Client 시뮬레이션)
	fmt.Println("--- 1단계: ConfigStore (CRD Client) CRUD ---")
	store := NewConfigStore()

	// 이벤트 핸들러 등록
	store.RegisterHandler(func(event ConfigEvent) {
		fmt.Printf("  [Watch] %s: %s\n", event.Type, event.Config.Key())
	})

	// VirtualService 생성
	store.Create(Config{
		GVK: GVKVirtualService, Name: "reviews-route", Namespace: "default",
		Spec: map[string]interface{}{
			"hosts":    []string{"reviews"},
			"gateways": []string{"mesh"},
		},
	})

	store.Create(Config{
		GVK: GVKVirtualService, Name: "ratings-route", Namespace: "default",
		Spec: map[string]interface{}{
			"hosts":    []string{"ratings"},
			"gateways": []string{"mesh"},
		},
	})

	store.Create(Config{
		GVK: GVKDestinationRule, Name: "reviews-dr", Namespace: "default",
		Spec: map[string]interface{}{
			"host": "reviews",
		},
	})

	fmt.Printf("  VirtualService 목록: %d개\n", len(store.List(GVKVirtualService, "")))
	fmt.Printf("  DestinationRule 목록: %d개\n", len(store.List(GVKDestinationRule, "")))

	// 2. krt.Collection으로 변환 파이프라인 구성
	fmt.Println()
	fmt.Println("--- 2단계: krt.Collection 변환 파이프라인 ---")

	// 입력 컬렉션: Config를 직접 담는 컬렉션
	configCollection := NewCollection[Config]("configs", func(c Config) string {
		return c.Key()
	})

	// ConfigStore에서 krt Collection으로 동기화
	for _, cfg := range store.List(GVKVirtualService, "") {
		configCollection.Set(cfg)
	}

	// 변환: Config → RouteEntry (VirtualService → 라우팅 엔트리)
	routeCollection := MapCollection[Config, RouteEntry](
		"routes",
		configCollection,
		func(cfg Config) *RouteEntry {
			if cfg.GVK != GVKVirtualService {
				return nil
			}
			hosts, _ := cfg.Spec["hosts"].([]string)
			gateways, _ := cfg.Spec["gateways"].([]string)
			if len(hosts) == 0 {
				return nil
			}
			return &RouteEntry{
				Host:      hosts[0],
				Namespace: cfg.Namespace,
				Gateway:   strings.Join(gateways, ","),
				Routes:    []string{fmt.Sprintf("→ %s.%s.svc.cluster.local", hosts[0], cfg.Namespace)},
			}
		},
		func(r RouteEntry) string { return r.Key() },
	)

	fmt.Printf("  파생 RouteEntry 수: %d\n", len(routeCollection.List()))
	for _, route := range routeCollection.List() {
		fmt.Printf("    %s: gateway=%s, routes=%v\n", route.Key(), route.Gateway, route.Routes)
	}

	// 3. 인덱스 생성 및 조회
	fmt.Println()
	fmt.Println("--- 3단계: krt.Index 인덱스 기반 조회 ---")

	gatewayIndex := NewIndex[RouteEntry]("by-gateway", routeCollection,
		func(r RouteEntry) []string {
			return []string{r.Gateway}
		},
	)

	meshRoutes := gatewayIndex.Lookup("mesh")
	fmt.Printf("  'mesh' 게이트웨이 라우트 수: %d\n", len(meshRoutes))
	for _, r := range meshRoutes {
		fmt.Printf("    %s\n", r.Host)
	}

	// 4. 변경 전파 시뮬레이션
	fmt.Println()
	fmt.Println("--- 4단계: 변경 전파 (Reactive Update) ---")

	// 핸들러: 파생 컬렉션 변경 감지
	routeCollection.RegisterHandler(func(eventType EventType, route RouteEntry) {
		fmt.Printf("  [RouteUpdate] %s: %s (gateway=%s)\n", eventType, route.Key(), route.Gateway)
	})

	// 새 VirtualService 추가 → 자동으로 RouteEntry 생성
	fmt.Println("  새 VirtualService 추가: productpage-route")
	configCollection.Set(Config{
		GVK: GVKVirtualService, Name: "productpage-route", Namespace: "default",
		Spec: map[string]interface{}{
			"hosts":    []string{"productpage"},
			"gateways": []string{"bookinfo-gateway"},
		},
	})

	time.Sleep(10 * time.Millisecond)
	fmt.Printf("  파생 RouteEntry 수: %d\n", len(routeCollection.List()))

	// 5. ConfigStore Update → Watch 이벤트 → 전파
	fmt.Println()
	fmt.Println("--- 5단계: ConfigStore Update 전파 ---")

	// ConfigStore 변경 → configCollection 동기화 → routeCollection 갱신
	store.RegisterHandler(func(event ConfigEvent) {
		if event.Config.GVK == GVKVirtualService {
			if event.Type == EventDelete {
				configCollection.Delete(event.Config.Key())
			} else {
				configCollection.Set(event.Config)
			}
		}
	})

	// 기존 VirtualService 업데이트
	store.Update(Config{
		GVK: GVKVirtualService, Name: "reviews-route", Namespace: "default",
		Spec: map[string]interface{}{
			"hosts":    []string{"reviews"},
			"gateways": []string{"bookinfo-gateway"}, // mesh → bookinfo-gateway 변경
		},
	})

	time.Sleep(10 * time.Millisecond)

	// 6. Delete 전파
	fmt.Println()
	fmt.Println("--- 6단계: Delete 전파 ---")
	store.Delete(GVKVirtualService, "ratings-route", "default")

	time.Sleep(10 * time.Millisecond)
	fmt.Printf("  ConfigStore VirtualService 수: %d\n", len(store.List(GVKVirtualService, "")))
	fmt.Printf("  파생 RouteEntry 수: %d\n", len(routeCollection.List()))

	// 요약
	fmt.Println()
	fmt.Println("=== 요약 ===")
	fmt.Println("  - ConfigStore: CRD CRUD + Watch 이벤트 시스템")
	fmt.Println("  - krt.Collection: 선언적 변환 (Config → RouteEntry)")
	fmt.Println("  - krt.Index: 특정 키로 O(1) 조회 (게이트웨이별 라우트)")
	fmt.Println("  - 변경 전파: 입력 변경 → 파생 컬렉션 자동 갱신 → 핸들러 호출")
}
