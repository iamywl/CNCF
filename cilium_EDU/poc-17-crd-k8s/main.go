package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Cilium K8s CRD 리소스 프레임워크 시뮬레이션
// =============================================================================
// 실제 소스: pkg/k8s/resource/resource.go, event.go, store.go, key.go
//
// Cilium의 Resource[T] 제네릭 프레임워크 핵심:
//
// 1. Resource[T]: Kubernetes 리소스에 대한 타입 안전한 접근을 제공
//    - Events() 채널로 이벤트 스트림 수신
//    - Store()로 읽기 전용 캐시 접근
//    - "lazy" 시작: Events() 또는 Store() 호출 시 Informer 시작
//
// 2. Event[T]: 리소스 변경 이벤트 (Upsert/Delete/Sync)
//    - Done(error) 콜백 필수 호출 (미호출 시 finalizer panic)
//    - Done(nil)로 성공, Done(err)로 실패 → ErrorHandler에 따라 재큐
//
// 3. Store[T]: 읽기 전용 타입 캐시
//    - List, Get, GetByKey, ByIndex 등
//    - Indexer로 커스텀 인덱스 지원
//
// 4. subscriber: 각 구독자는 독립적 큐를 가짐
//    - keyWorkItem/syncWorkItem을 workqueue에 넣어 처리
//    - RateLimiter로 실패 재시도 속도 제어
//    - ErrorHandler: AlwaysRetry(기본), ErrorActionStop, ErrorActionIgnore
//
// 실행: go run main.go
// =============================================================================

// ============================================================================
// Key (실제: pkg/k8s/resource/key.go)
// ============================================================================

// Key는 리소스의 네임스페이스/이름 키이다.
// 실제 구현에서는 cache.MetaNamespaceKeyFunc로 생성된다.
type Key struct {
	Namespace string
	Name      string
}

func (k Key) String() string {
	if k.Namespace == "" {
		return k.Name
	}
	return k.Namespace + "/" + k.Name
}

// ============================================================================
// EventKind / Event (실제: pkg/k8s/resource/event.go)
// ============================================================================

// EventKind는 이벤트 유형이다.
type EventKind string

const (
	Sync   EventKind = "sync"   // 스토어 동기화 완료
	Upsert EventKind = "upsert" // 생성 또는 업데이트
	Delete EventKind = "delete" // 삭제
)

// Event는 리소스 변경 이벤트이다.
// 실제 구현(event.go)에서 Done 콜백은 필수 호출이며,
// 미호출 시 runtime.SetFinalizer를 통해 panic이 발생한다.
type Event[T any] struct {
	Kind   EventKind
	Key    Key
	Object T

	// Done은 이벤트 처리 완료를 표시한다.
	// err가 nil이면 성공, non-nil이면 ErrorHandler에 따라 재큐 또는 무시.
	// 실제: Event[T].Done(error) - 호출하지 않으면 finalizer에서 panic
	Done func(err error)
}

// ============================================================================
// ErrorHandler (실제: pkg/k8s/resource/error.go)
// ============================================================================

type ErrorAction int

const (
	ErrorActionRetry  ErrorAction = iota // 재큐 (기본값)
	ErrorActionStop                       // 구독 종료
	ErrorActionIgnore                     // 무시
)

type ErrorHandler func(key Key, numRequeues int, err error) ErrorAction

// AlwaysRetry는 기본 에러 핸들러로, 항상 재큐한다.
// 실제: resource.AlwaysRetry
var AlwaysRetry ErrorHandler = func(key Key, numRequeues int, err error) ErrorAction {
	return ErrorActionRetry
}

// ============================================================================
// Store (실제: pkg/k8s/resource/store.go)
// ============================================================================

// Store는 읽기 전용 타입 캐시이다.
// 실제 구현(store.go)에서는 cache.Indexer를 래핑하여 타입 안전한 접근을 제공.
type Store[T any] struct {
	mu       sync.RWMutex
	items    map[string]T          // key.String() -> T
	indexers map[string]IndexFunc[T] // 인덱서 이름 -> 함수
	indices  map[string]map[string][]string // indexName -> indexValue -> []keyString
}

type IndexFunc[T any] func(obj T) []string

func NewStore[T any]() *Store[T] {
	return &Store[T]{
		items:    make(map[string]T),
		indexers: make(map[string]IndexFunc[T]),
		indices:  make(map[string]map[string][]string),
	}
}

func (s *Store[T]) AddIndexer(name string, fn IndexFunc[T]) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.indexers[name] = fn
	s.indices[name] = make(map[string][]string)
}

// List는 모든 항목을 반환한다. (실제: typedStore.List)
func (s *Store[T]) List() []T {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]T, 0, len(s.items))
	for _, item := range s.items {
		result = append(result, item)
	}
	return result
}

// GetByKey는 키로 항목을 조회한다. (실제: typedStore.GetByKey)
func (s *Store[T]) GetByKey(key Key) (T, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	item, ok := s.items[key.String()]
	return item, ok
}

// ByIndex는 인덱스로 항목을 조회한다. (실제: typedStore.ByIndex)
func (s *Store[T]) ByIndex(indexName, indexValue string) []T {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys, ok := s.indices[indexName][indexValue]
	if !ok {
		return nil
	}
	result := make([]T, 0, len(keys))
	for _, k := range keys {
		if item, exists := s.items[k]; exists {
			result = append(result, item)
		}
	}
	return result
}

// IterKeys는 키 반복자를 반환한다. (실제: typedStore.IterKeys)
func (s *Store[T]) IterKeys() []Key {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]Key, 0, len(s.items))
	for k := range s.items {
		parts := strings.SplitN(k, "/", 2)
		if len(parts) == 2 {
			keys = append(keys, Key{Namespace: parts[0], Name: parts[1]})
		} else {
			keys = append(keys, Key{Name: k})
		}
	}
	return keys
}

func (s *Store[T]) upsert(key Key, obj T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keyStr := key.String()
	s.items[keyStr] = obj
	// 인덱스 업데이트
	for indexName, fn := range s.indexers {
		// 기존 인덱스 제거
		s.removeFromIndex(indexName, keyStr)
		// 새 인덱스 추가
		for _, val := range fn(obj) {
			s.indices[indexName][val] = append(s.indices[indexName][val], keyStr)
		}
	}
}

func (s *Store[T]) delete(key Key) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keyStr := key.String()
	for indexName := range s.indexers {
		s.removeFromIndex(indexName, keyStr)
	}
	delete(s.items, keyStr)
}

func (s *Store[T]) removeFromIndex(indexName, keyStr string) {
	idx := s.indices[indexName]
	for val, keys := range idx {
		newKeys := make([]string, 0, len(keys))
		for _, k := range keys {
			if k != keyStr {
				newKeys = append(newKeys, k)
			}
		}
		if len(newKeys) == 0 {
			delete(idx, val)
		} else {
			idx[val] = newKeys
		}
	}
}

// ============================================================================
// WorkItem (실제: pkg/k8s/resource/resource.go)
// ============================================================================

// WorkItem은 subscriber 큐의 작업 항목이다.
// 실제 구현에서는 syncWorkItem과 keyWorkItem 두 종류가 있다.
type WorkItem interface {
	isWorkItem()
}

type syncWorkItem struct{}
func (syncWorkItem) isWorkItem() {}

type keyWorkItem struct{ key Key }
func (keyWorkItem) isWorkItem() {}

// ============================================================================
// Resource (실제: pkg/k8s/resource/resource.go)
// ============================================================================

// Resource는 K8s 리소스에 대한 타입 안전한 접근을 제공한다.
// 실제 구현의 핵심 특성:
// - "lazy" 시작: Events()/Store() 호출 시에만 Informer 시작 (markNeeded)
// - 각 subscriber는 독립적 workqueue를 가짐
// - subscriber 등록 시 초기 키를 큐에 넣고, 동기화 완료 시 syncWorkItem 큐
// - processLoop에서 키를 꺼내 store에서 최신 객체를 가져와 이벤트 생성
type Resource[T any] struct {
	mu           sync.RWMutex
	store        *Store[T]
	subscribers  []*subscriber[T]
	synchronized bool

	// API 서버 시뮬레이션
	apiStore map[string]T
	apiMu    sync.RWMutex
	watchers []chan resourceDelta[T]
}

type resourceDelta[T any] struct {
	deltaType string // "Added", "Updated", "Deleted"
	key       Key
	obj       T
}

func NewResource[T any](store *Store[T]) *Resource[T] {
	return &Resource[T]{
		store:    store,
		apiStore: make(map[string]T),
	}
}

// Events는 이벤트 채널을 반환한다. (실제: resource.Events)
// 실제 구현에서는:
// 1. markNeeded()로 Informer 시작 (lazy)
// 2. subscriber 생성 (독립적 workqueue, RateLimiter)
// 3. 초기 키를 큐에 넣음 (store.IterKeys)
// 4. 동기화 완료 시 syncWorkItem 큐
// 5. processLoop: 키 꺼내기 -> store 조회 -> Event 생성 -> 채널 전송
func (r *Resource[T]) Events(ctx context.Context) <-chan Event[T] {
	out := make(chan Event[T], 16)

	sub := &subscriber[T]{
		resource: r,
		queue:    make([]WorkItem, 0),
		outCh:    out,
	}

	r.mu.Lock()
	r.subscribers = append(r.subscribers, sub)

	// 초기 키를 큐에 넣음 (실제: store.IterKeys 후 sub.enqueueKey)
	for _, key := range r.store.IterKeys() {
		sub.queue = append(sub.queue, keyWorkItem{key: key})
	}

	// 이미 동기화되었으면 sync 큐
	if r.synchronized {
		sub.queue = append(sub.queue, syncWorkItem{})
	}
	r.mu.Unlock()

	// 큐 처리 고루틴 시작 (실제: sub.processLoop)
	go sub.processLoop(ctx)

	return out
}

// SimulateAPIChange는 API 서버에서의 변경을 시뮬레이션한다.
func (r *Resource[T]) SimulateAPIChange(deltaType string, key Key, obj T) {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch deltaType {
	case "Added", "Updated":
		r.store.upsert(key, obj)
	case "Deleted":
		r.store.delete(key)
	}

	// 모든 subscriber에게 키 전달 (실제: Informer delta 처리에서 sub.enqueueKey)
	for _, sub := range r.subscribers {
		sub.enqueueKey(key)
	}
}

// MarkSynced는 초기 동기화를 완료한다.
// 실제 구현에서는 cache.WaitForCacheSync 후 synchronized=true 설정,
// 모든 subscriber에게 syncWorkItem 큐.
func (r *Resource[T]) MarkSynced() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.synchronized = true
	for _, sub := range r.subscribers {
		sub.enqueueSync()
	}
}

// ============================================================================
// subscriber (실제: pkg/k8s/resource/resource.go)
// ============================================================================

// subscriber는 Resource의 각 구독자를 나타낸다.
// 실제 구현에서 각 subscriber는 독립적 workqueue.TypedRateLimitingInterface를
// 가지며, 키 단위로 이벤트를 처리한다.
type subscriber[T any] struct {
	resource *Resource[T]
	queue    []WorkItem
	queueMu  sync.Mutex
	queueCh  chan struct{}
	outCh    chan Event[T]

	// lastKnown은 구독자가 본 마지막 객체 상태를 추적한다.
	// 실제 구현(lastKnownObjects)에서 Delete 이벤트에 마지막 상태를 포함하기 위해 사용.
	lastKnown   map[string]T
	lastKnownMu sync.Mutex
}

func (s *subscriber[T]) enqueueKey(key Key) {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	s.queue = append(s.queue, keyWorkItem{key: key})
	s.notifyQueue()
}

func (s *subscriber[T]) enqueueSync() {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	s.queue = append(s.queue, syncWorkItem{})
	s.notifyQueue()
}

func (s *subscriber[T]) notifyQueue() {
	if s.queueCh != nil {
		select {
		case s.queueCh <- struct{}{}:
		default:
		}
	}
}

// processLoop는 큐에서 작업을 꺼내 이벤트를 생성한다.
// 실제 구현(resource.go:processLoop):
// - workqueue에서 항목을 Get
// - syncWorkItem이면 Sync 이벤트
// - keyWorkItem이면 store.GetByKey로 최신 객체 조회
//   - 존재하면 Upsert, 없으면 lastKnownObjects에서 가져와 Delete
//   - 구독자가 본 적 없는 객체의 Delete는 무시
// - Done 콜백: 성공 시 wq.Forget, 실패 시 ErrorHandler → Retry/Stop/Ignore
func (s *subscriber[T]) processLoop(ctx context.Context) {
	defer close(s.outCh)
	s.lastKnown = make(map[string]T)
	s.queueCh = make(chan struct{}, 1)

	// 초기 큐 처리
	s.processQueuedItems(ctx)

	// 이후 이벤트 대기
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.queueCh:
			s.processQueuedItems(ctx)
		}
	}
}

func (s *subscriber[T]) processQueuedItems(ctx context.Context) {
	for {
		s.queueMu.Lock()
		if len(s.queue) == 0 {
			s.queueMu.Unlock()
			return
		}
		item := s.queue[0]
		s.queue = s.queue[1:]
		s.queueMu.Unlock()

		var event Event[T]
		switch wi := item.(type) {
		case syncWorkItem:
			event.Kind = Sync
		case keyWorkItem:
			obj, exists := s.resource.store.GetByKey(wi.key)
			if !exists {
				// 삭제: lastKnown에서 마지막 상태를 가져옴
				s.lastKnownMu.Lock()
				lastObj, seen := s.lastKnown[wi.key.String()]
				s.lastKnownMu.Unlock()
				if !seen {
					// 구독자가 본 적 없는 객체 → 무시 (실제 동작과 동일)
					continue
				}
				event.Kind = Delete
				event.Key = wi.key
				event.Object = lastObj
			} else {
				s.lastKnownMu.Lock()
				s.lastKnown[wi.key.String()] = obj
				s.lastKnownMu.Unlock()
				event.Kind = Upsert
				event.Key = wi.key
				event.Object = obj
			}
		}

		done := make(chan struct{})
		event.Done = func(err error) {
			if err == nil && event.Kind == Delete {
				s.lastKnownMu.Lock()
				delete(s.lastKnown, event.Key.String())
				s.lastKnownMu.Unlock()
			}
			close(done)
		}

		select {
		case s.outCh <- event:
			// Done 호출 대기
			select {
			case <-done:
			case <-ctx.Done():
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// ============================================================================
// CiliumNetworkPolicy (CRD 리소스 예제)
// ============================================================================

type CiliumNetworkPolicy struct {
	Namespace       string
	Name            string
	ResourceVersion int64
	Labels          map[string]string
	Ingress         []IngressRule
	Egress          []EgressRule
	EndpointSelector map[string]string
}

type IngressRule struct {
	FromLabels map[string]string
	Port       int
	Protocol   string
}

type EgressRule struct {
	ToLabels map[string]string
	Port     int
	Protocol string
}

func cnpKey(cnp *CiliumNetworkPolicy) Key {
	return Key{Namespace: cnp.Namespace, Name: cnp.Name}
}

// ============================================================================
// 시뮬레이션 실행
// ============================================================================

func main() {
	fmt.Println("=== Cilium K8s Resource[T] 프레임워크 시뮬레이션 ===")
	fmt.Println("소스: pkg/k8s/resource/resource.go, event.go, store.go")
	fmt.Println()

	// ─── 1. Store 및 Indexer 설정 ───────────────────────────────
	fmt.Println("[1] Store[T] 및 Indexer 설정")

	store := NewStore[*CiliumNetworkPolicy]()

	// 인덱서: 네임스페이스별 (실제: cache.Indexers 사용)
	store.AddIndexer("namespace", func(cnp *CiliumNetworkPolicy) []string {
		return []string{cnp.Namespace}
	})

	// 인덱서: 레이블(tier)별
	store.AddIndexer("tier", func(cnp *CiliumNetworkPolicy) []string {
		if tier, ok := cnp.Labels["tier"]; ok {
			return []string{tier}
		}
		return nil
	})
	fmt.Println("  인덱서 등록: namespace, tier")
	fmt.Println()

	// ─── 2. Resource 생성 및 초기 데이터 로드 ───────────────────
	fmt.Println("[2] Resource[T] 생성 및 초기 데이터 (List)")

	resource := NewResource[*CiliumNetworkPolicy](store)

	// 초기 데이터 (실제: Informer의 초기 List에서 가져옴)
	initialPolicies := []*CiliumNetworkPolicy{
		{
			Namespace: "default", Name: "allow-frontend", ResourceVersion: 1,
			Labels:           map[string]string{"tier": "frontend"},
			EndpointSelector: map[string]string{"app": "frontend"},
			Ingress:          []IngressRule{{FromLabels: map[string]string{"app": "gateway"}, Port: 80, Protocol: "TCP"}},
		},
		{
			Namespace: "default", Name: "allow-backend", ResourceVersion: 2,
			Labels:           map[string]string{"tier": "backend"},
			EndpointSelector: map[string]string{"app": "api-server"},
			Ingress:          []IngressRule{{FromLabels: map[string]string{"app": "frontend"}, Port: 8080, Protocol: "TCP"}},
			Egress:           []EgressRule{{ToLabels: map[string]string{"app": "database"}, Port: 5432, Protocol: "TCP"}},
		},
		{
			Namespace: "monitoring", Name: "metrics-access", ResourceVersion: 3,
			Labels:           map[string]string{"tier": "infra"},
			EndpointSelector: map[string]string{"app": "prometheus"},
			Ingress:          []IngressRule{{FromLabels: map[string]string{"app": "grafana"}, Port: 9090, Protocol: "TCP"}},
		},
	}

	for _, cnp := range initialPolicies {
		store.upsert(cnpKey(cnp), cnp)
		fmt.Printf("  Loaded: %s/%s (rv=%d)\n", cnp.Namespace, cnp.Name, cnp.ResourceVersion)
	}
	fmt.Println()

	// ─── 3. Events() 구독 ───────────────────────────────────────
	fmt.Println("[3] Events() 구독 (subscriber 시작)")
	fmt.Println("  실제: resource.Events(ctx) - 초기 키 replay 후 Sync 이벤트")
	fmt.Println()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events := resource.Events(ctx)

	// 동기화 완료 마킹 (실제: Informer HasSynced 후 자동)
	resource.MarkSynced()

	// 이벤트 수집
	var received []string
	var syncReceived bool

	// 초기 이벤트 수집 (Upsert replay + Sync)
	fmt.Println("  --- 초기 이벤트 (replay + sync) ---")
	timeout := time.After(500 * time.Millisecond)
collectInitial:
	for {
		select {
		case ev := <-events:
			switch ev.Kind {
			case Upsert:
				msg := fmt.Sprintf("  Upsert: %s (rv=%d)", ev.Key, ev.Object.ResourceVersion)
				fmt.Println(msg)
				received = append(received, msg)
				ev.Done(nil) // 성공으로 처리
			case Sync:
				fmt.Println("  Sync: 스토어 동기화 완료")
				syncReceived = true
				ev.Done(nil)
			case Delete:
				msg := fmt.Sprintf("  Delete: %s", ev.Key)
				fmt.Println(msg)
				received = append(received, msg)
				ev.Done(nil)
			}
		case <-timeout:
			break collectInitial
		}
	}
	fmt.Printf("  초기 Upsert: %d개, Sync: %v\n", len(received), syncReceived)
	fmt.Println()

	// ─── 4. 리소스 변경 Watch ───────────────────────────────────
	fmt.Println("[4] 리소스 변경 (Watch를 통한 증분 업데이트)")

	// 정책 추가
	newPolicy := &CiliumNetworkPolicy{
		Namespace: "default", Name: "allow-cache", ResourceVersion: 4,
		Labels:           map[string]string{"tier": "backend"},
		EndpointSelector: map[string]string{"app": "redis"},
		Ingress:          []IngressRule{{FromLabels: map[string]string{"app": "api-server"}, Port: 6379, Protocol: "TCP"}},
	}
	fmt.Printf("  API 변경: Added %s/%s\n", newPolicy.Namespace, newPolicy.Name)
	resource.SimulateAPIChange("Added", cnpKey(newPolicy), newPolicy)

	// 정책 업데이트
	updatedPolicy := &CiliumNetworkPolicy{
		Namespace: "default", Name: "allow-frontend", ResourceVersion: 5,
		Labels:           map[string]string{"tier": "frontend"},
		EndpointSelector: map[string]string{"app": "frontend"},
		Ingress: []IngressRule{
			{FromLabels: map[string]string{"app": "gateway"}, Port: 80, Protocol: "TCP"},
			{FromLabels: map[string]string{"app": "mobile-gw"}, Port: 443, Protocol: "TCP"}, // 규칙 추가
		},
	}
	fmt.Printf("  API 변경: Updated %s/%s (rv=%d)\n", updatedPolicy.Namespace, updatedPolicy.Name, updatedPolicy.ResourceVersion)
	resource.SimulateAPIChange("Updated", cnpKey(updatedPolicy), updatedPolicy)

	// 정책 삭제
	fmt.Printf("  API 변경: Deleted monitoring/metrics-access\n")
	resource.SimulateAPIChange("Deleted", Key{Namespace: "monitoring", Name: "metrics-access"}, nil)

	// 이벤트 수집
	time.Sleep(100 * time.Millisecond)
	fmt.Println()
	fmt.Println("  --- 증분 이벤트 ---")
	timeout2 := time.After(500 * time.Millisecond)
collectIncremental:
	for {
		select {
		case ev := <-events:
			switch ev.Kind {
			case Upsert:
				fmt.Printf("  Upsert: %s (rv=%d)\n", ev.Key, ev.Object.ResourceVersion)
				ev.Done(nil)
			case Delete:
				fmt.Printf("  Delete: %s (rv=%d)\n", ev.Key, ev.Object.ResourceVersion)
				ev.Done(nil)
			case Sync:
				ev.Done(nil)
			}
		case <-timeout2:
			break collectIncremental
		}
	}
	fmt.Println()

	// ─── 5. Store 조회 ──────────────────────────────────────────
	fmt.Println("[5] Store[T] 조회")

	fmt.Printf("  전체 항목: %d개\n", len(store.List()))
	fmt.Println()

	// GetByKey
	cnp, exists := store.GetByKey(Key{Namespace: "default", Name: "allow-frontend"})
	if exists {
		fmt.Printf("  GetByKey(default/allow-frontend): rv=%d, ingress=%d개 규칙\n",
			cnp.ResourceVersion, len(cnp.Ingress))
	}

	// ByIndex
	fmt.Println()
	fmt.Println("  --- ByIndex(namespace, default) ---")
	defaultPolicies := store.ByIndex("namespace", "default")
	for _, p := range defaultPolicies {
		fmt.Printf("    %s/%s (rv=%d)\n", p.Namespace, p.Name, p.ResourceVersion)
	}

	fmt.Println()
	fmt.Println("  --- ByIndex(tier, backend) ---")
	backendPolicies := store.ByIndex("tier", "backend")
	for _, p := range backendPolicies {
		fmt.Printf("    %s/%s (rv=%d)\n", p.Namespace, p.Name, p.ResourceVersion)
	}
	fmt.Println()

	// ─── 6. Done 에러 핸들링 ────────────────────────────────────
	fmt.Println("[6] Done(err) 에러 핸들링 시뮬레이션")
	fmt.Println("  실제: ErrorHandler에 따라 Retry/Stop/Ignore 결정")
	fmt.Println()

	errorHandler := func(key Key, numRequeues int, err error) ErrorAction {
		if numRequeues >= 3 {
			return ErrorActionIgnore
		}
		return ErrorActionRetry
	}

	// 에러 핸들러 테스트
	for i := 0; i < 4; i++ {
		action := errorHandler(Key{Namespace: "default", Name: "test"}, i, fmt.Errorf("처리 실패"))
		actionStr := "Retry"
		if action == ErrorActionIgnore {
			actionStr = "Ignore"
		}
		fmt.Printf("  numRequeues=%d → %s\n", i, actionStr)
	}
	fmt.Println()

	// ─── 7. 최종 Store 상태 ─────────────────────────────────────
	fmt.Println("[7] 최종 Store 상태")

	keys := store.IterKeys()
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })

	fmt.Printf("  %-30s %-5s %-15s %-10s\n", "Key", "RV", "Tier", "Rules")
	fmt.Println("  " + strings.Repeat("-", 65))
	for _, k := range keys {
		p, _ := store.GetByKey(k)
		tier := p.Labels["tier"]
		rules := len(p.Ingress) + len(p.Egress)
		fmt.Printf("  %-30s %-5d %-15s %-10d\n", k.String(), p.ResourceVersion, tier, rules)
	}

	// ─── 구조 요약 ─────────────────────────────────────────────
	fmt.Println()
	fmt.Println("=== Resource[T] 프레임워크 구조 ===")
	fmt.Println()
	fmt.Println("  Resource[T] (lazy start)")
	fmt.Println("  ├── Events(ctx) → <-chan Event[T]")
	fmt.Println("  │   ├── subscriber 생성 (독립 workqueue)")
	fmt.Println("  │   ├── 초기 키 replay (store.IterKeys)")
	fmt.Println("  │   ├── Upsert, Upsert, ..., Sync (동기화 완료)")
	fmt.Println("  │   └── 이후 증분: Upsert, Delete, ...")
	fmt.Println("  │")
	fmt.Println("  ├── Store(ctx) → Store[T]")
	fmt.Println("  │   ├── List()       : 전체 항목")
	fmt.Println("  │   ├── GetByKey()   : 키로 조회")
	fmt.Println("  │   ├── ByIndex()    : 인덱스 조회")
	fmt.Println("  │   └── IterKeys()   : 키 반복")
	fmt.Println("  │")
	fmt.Println("  └── Event[T].Done(err)")
	fmt.Println("      ├── nil  → 성공 (wq.Forget)")
	fmt.Println("      └── err  → ErrorHandler")
	fmt.Println("          ├── AlwaysRetry → 재큐 (RateLimited)")
	fmt.Println("          ├── ErrorActionStop → 구독 종료")
	fmt.Println("          └── ErrorActionIgnore → 무시")
	fmt.Println()
	fmt.Println("  핵심: 각 subscriber는 독립적 큐를 가져 병렬 처리 가능")
	fmt.Println("  핵심: Done() 미호출 시 finalizer를 통한 panic (leak 방지)")
	fmt.Println()
	fmt.Println("=== 시뮬레이션 완료 ===")
}
