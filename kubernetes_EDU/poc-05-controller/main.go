// poc-05-controller: 쿠버네티스 컨트롤러 패턴 (Informer + WorkQueue + Reconcile)
//
// K8s 컨트롤러의 핵심 메커니즘을 구현한다:
// - Fake API Server: LIST + WATCH 지원
// - Informer: LIST로 전체 상태를 가져온 뒤 WATCH로 변경사항 추적
// - DeltaFIFO: 이벤트를 중복 제거하며 순서대로 처리
// - WorkQueue: 재시도(rate limiting) 기능이 있는 작업 큐
// - Reconciler: ReplicaSet 컨트롤러 유사 — desired vs actual 조정
//
// 참조 소스:
//   - staging/src/k8s.io/client-go/tools/cache/delta_fifo.go (DeltaFIFO)
//   - staging/src/k8s.io/client-go/tools/cache/reflector.go (Reflector = LIST+WATCH)
//   - staging/src/k8s.io/client-go/util/workqueue/rate_limiting_queue.go
//   - pkg/controller/replicaset/replica_set.go (ReplicaSetController)
//
// 실행: go run main.go
package main

import (
	"fmt"
	"math/rand"
	"sync"
	"time"
)

// ============================================================================
// 데이터 모델
// ============================================================================

// Pod는 간소화된 파드 리소스이다.
type Pod struct {
	Name            string
	Namespace       string
	Labels          map[string]string
	OwnerName       string // 소유 ReplicaSet 이름
	ResourceVersion int64
	Phase           string // Pending, Running, Terminated
}

// ReplicaSet은 파드 복제 관리 리소스이다.
type ReplicaSet struct {
	Name            string
	Namespace       string
	Replicas        int // 원하는 복제본 수
	Selector        map[string]string
	ResourceVersion int64
}

// ============================================================================
// Watch 이벤트 — API Server가 전달하는 변경 이벤트
// ============================================================================

// EventType은 이벤트 종류이다.
type EventType string

const (
	Added    EventType = "ADDED"
	Modified EventType = "MODIFIED"
	Deleted  EventType = "DELETED"
)

// WatchEvent는 API Server의 watch 스트림 이벤트이다.
type WatchEvent struct {
	Type EventType
	Pod  Pod
}

// ============================================================================
// Fake API Server — LIST + WATCH 지원
// 실제 소스: staging/src/k8s.io/apiserver/pkg/registry/generic/registry/store.go
// ============================================================================

// FakeAPIServer는 간소화된 API Server이다.
type FakeAPIServer struct {
	mu              sync.RWMutex
	pods            map[string]*Pod // namespace/name → Pod
	replicaSets     map[string]*ReplicaSet
	resourceVersion int64
	watchers        []chan WatchEvent
	watcherMu       sync.Mutex
}

func NewFakeAPIServer() *FakeAPIServer {
	return &FakeAPIServer{
		pods:        make(map[string]*Pod),
		replicaSets: make(map[string]*ReplicaSet),
	}
}

func (s *FakeAPIServer) ListPods() []Pod {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]Pod, 0, len(s.pods))
	for _, p := range s.pods {
		result = append(result, *p)
	}
	return result
}

func (s *FakeAPIServer) CreatePod(pod *Pod) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := pod.Namespace + "/" + pod.Name
	if _, exists := s.pods[key]; exists {
		return fmt.Errorf("이미 존재: %s", key)
	}
	s.resourceVersion++
	pod.ResourceVersion = s.resourceVersion
	stored := *pod
	s.pods[key] = &stored
	s.notifyWatchers(WatchEvent{Type: Added, Pod: stored})
	return nil
}

func (s *FakeAPIServer) UpdatePod(pod *Pod) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := pod.Namespace + "/" + pod.Name
	if _, exists := s.pods[key]; !exists {
		return fmt.Errorf("없음: %s", key)
	}
	s.resourceVersion++
	pod.ResourceVersion = s.resourceVersion
	stored := *pod
	s.pods[key] = &stored
	s.notifyWatchers(WatchEvent{Type: Modified, Pod: stored})
	return nil
}

func (s *FakeAPIServer) DeletePod(namespace, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := namespace + "/" + name
	pod, exists := s.pods[key]
	if !exists {
		return fmt.Errorf("없음: %s", key)
	}
	deleted := *pod
	delete(s.pods, key)
	s.resourceVersion++
	s.notifyWatchers(WatchEvent{Type: Deleted, Pod: deleted})
	return nil
}

func (s *FakeAPIServer) GetReplicaSet(namespace, name string) (*ReplicaSet, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rs, ok := s.replicaSets[namespace+"/"+name]
	if ok {
		cp := *rs
		return &cp, true
	}
	return nil, false
}

func (s *FakeAPIServer) CreateReplicaSet(rs *ReplicaSet) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resourceVersion++
	rs.ResourceVersion = s.resourceVersion
	stored := *rs
	s.replicaSets[rs.Namespace+"/"+rs.Name] = &stored
}

func (s *FakeAPIServer) Watch() <-chan WatchEvent {
	ch := make(chan WatchEvent, 100)
	s.watcherMu.Lock()
	s.watchers = append(s.watchers, ch)
	s.watcherMu.Unlock()
	return ch
}

func (s *FakeAPIServer) notifyWatchers(event WatchEvent) {
	s.watcherMu.Lock()
	defer s.watcherMu.Unlock()
	for _, ch := range s.watchers {
		select {
		case ch <- event:
		default:
		}
	}
}

// ============================================================================
// DeltaFIFO — 이벤트 중복 제거 + FIFO 순서 보장
// 실제 소스: staging/src/k8s.io/client-go/tools/cache/delta_fifo.go
//
// DeltaFIFO는 두 가지 핵심 자료구조를 사용한다:
// - items: 키 → Delta 목록 (같은 오브젝트의 연속 이벤트를 합침)
// - queue: FIFO 순서의 키 목록 (중복 없음)
// ============================================================================

// DeltaType은 Delta의 종류이다.
type DeltaType string

const (
	DeltaAdded    DeltaType = "Added"
	DeltaUpdated  DeltaType = "Updated"
	DeltaDeleted  DeltaType = "Deleted"
	DeltaSync     DeltaType = "Sync" // 주기적 재동기화
)

// Delta는 하나의 변경 이벤트이다.
type Delta struct {
	Type DeltaType
	Pod  Pod
}

// DeltaFIFO는 이벤트를 키 기반으로 중복 제거하며 FIFO 순서로 제공한다.
type DeltaFIFO struct {
	mu    sync.Mutex
	cond  *sync.Cond
	items map[string][]Delta // 키 → Delta 슬라이스
	queue []string           // 처리 대기 중인 키 (FIFO, 중복 없음)
}

func NewDeltaFIFO() *DeltaFIFO {
	df := &DeltaFIFO{
		items: make(map[string][]Delta),
	}
	df.cond = sync.NewCond(&df.mu)
	return df
}

// podKey는 파드의 고유 키를 생성한다.
func podKey(p Pod) string {
	return p.Namespace + "/" + p.Name
}

// Add는 Delta를 FIFO에 추가한다.
// 같은 키에 대한 Delta는 합쳐진다 (중복 제거).
func (df *DeltaFIFO) Add(deltaType DeltaType, pod Pod) {
	df.mu.Lock()
	defer df.mu.Unlock()

	key := podKey(pod)

	// items에 Delta 추가
	df.items[key] = append(df.items[key], Delta{Type: deltaType, Pod: pod})

	// queue에 키가 없으면 추가 (중복 방지)
	found := false
	for _, k := range df.queue {
		if k == key {
			found = true
			break
		}
	}
	if !found {
		df.queue = append(df.queue, key)
	}

	df.cond.Signal() // 대기 중인 Pop에게 알림
}

// Pop은 가장 오래된 키의 Delta 목록을 꺼낸다. 비어있으면 블로킹 대기한다.
func (df *DeltaFIFO) Pop() (string, []Delta) {
	df.mu.Lock()
	defer df.mu.Unlock()

	for len(df.queue) == 0 {
		df.cond.Wait()
	}

	key := df.queue[0]
	df.queue = df.queue[1:]
	deltas := df.items[key]
	delete(df.items, key)

	return key, deltas
}

// ============================================================================
// Informer — LIST + WATCH로 로컬 캐시를 유지한다.
// 실제 소스: staging/src/k8s.io/client-go/tools/cache/reflector.go
//
// Informer는 두 단계로 동작한다:
// 1. LIST: 전체 오브젝트 목록을 가져와서 DeltaFIFO에 Sync로 넣음
// 2. WATCH: 변경사항을 실시간으로 DeltaFIFO에 Add/Update/Delete로 넣음
// ============================================================================

// Informer는 API Server의 상태를 로컬에 캐시한다.
type Informer struct {
	apiServer *FakeAPIServer
	deltaFIFO *DeltaFIFO
	store     map[string]Pod // 로컬 캐시 (Indexer 역할)
	storeMu   sync.RWMutex
	handlers  []ResourceEventHandler // 이벤트 핸들러
}

// ResourceEventHandler는 Informer의 이벤트 콜백이다.
// 실제 소스: staging/src/k8s.io/client-go/tools/cache/controller.go
type ResourceEventHandler interface {
	OnAdd(pod Pod)
	OnUpdate(oldPod, newPod Pod)
	OnDelete(pod Pod)
}

func NewInformer(api *FakeAPIServer) *Informer {
	return &Informer{
		apiServer: api,
		deltaFIFO: NewDeltaFIFO(),
		store:     make(map[string]Pod),
	}
}

// AddEventHandler는 이벤트 핸들러를 등록한다.
func (inf *Informer) AddEventHandler(handler ResourceEventHandler) {
	inf.handlers = append(inf.handlers, handler)
}

// Run은 Informer를 시작한다.
func (inf *Informer) Run(stopCh <-chan struct{}) {
	// Phase 1: LIST — 전체 상태를 가져온다
	fmt.Println("[Informer] LIST 단계 — 전체 파드 목록 동기화")
	pods := inf.apiServer.ListPods()
	for _, pod := range pods {
		inf.deltaFIFO.Add(DeltaSync, pod)
	}
	fmt.Printf("[Informer] LIST 완료 — %d개 파드 동기화\n", len(pods))

	// Phase 2: WATCH — 변경사항을 실시간으로 추적
	fmt.Println("[Informer] WATCH 단계 — 변경사항 추적 시작")
	watchCh := inf.apiServer.Watch()
	go func() {
		for {
			select {
			case event := <-watchCh:
				switch event.Type {
				case Added:
					inf.deltaFIFO.Add(DeltaAdded, event.Pod)
				case Modified:
					inf.deltaFIFO.Add(DeltaUpdated, event.Pod)
				case Deleted:
					inf.deltaFIFO.Add(DeltaDeleted, event.Pod)
				}
			case <-stopCh:
				return
			}
		}
	}()

	// DeltaFIFO 처리 루프 — Pop된 Delta를 로컬 캐시에 반영하고 핸들러 호출
	go func() {
		for {
			select {
			case <-stopCh:
				return
			default:
			}

			key, deltas := inf.deltaFIFO.Pop()
			inf.processDeltas(key, deltas)
		}
	}()
}

// processDeltas는 Delta 목록을 처리하여 로컬 캐시를 업데이트하고 핸들러를 호출한다.
func (inf *Informer) processDeltas(key string, deltas []Delta) {
	inf.storeMu.Lock()
	defer inf.storeMu.Unlock()

	for _, d := range deltas {
		switch d.Type {
		case DeltaAdded, DeltaSync:
			old, exists := inf.store[key]
			inf.store[key] = d.Pod
			if exists {
				for _, h := range inf.handlers {
					h.OnUpdate(old, d.Pod)
				}
			} else {
				for _, h := range inf.handlers {
					h.OnAdd(d.Pod)
				}
			}
		case DeltaUpdated:
			old := inf.store[key]
			inf.store[key] = d.Pod
			for _, h := range inf.handlers {
				h.OnUpdate(old, d.Pod)
			}
		case DeltaDeleted:
			old := inf.store[key]
			delete(inf.store, key)
			for _, h := range inf.handlers {
				h.OnDelete(old)
			}
		}
	}
}

// GetStore는 로컬 캐시의 복사본을 반환한다.
func (inf *Informer) GetStore() map[string]Pod {
	inf.storeMu.RLock()
	defer inf.storeMu.RUnlock()
	cp := make(map[string]Pod)
	for k, v := range inf.store {
		cp[k] = v
	}
	return cp
}

// ============================================================================
// WorkQueue — 재시도 가능한 작업 큐 (Rate Limiting)
// 실제 소스: staging/src/k8s.io/client-go/util/workqueue/rate_limiting_queue.go
//
// WorkQueue는 다음 기능을 제공한다:
// - 중복 방지: 같은 키가 큐에 여러 번 들어가지 않음
// - 재시도: 실패 시 지수 백오프로 재시도
// - 순서 보장: FIFO 순서
// ============================================================================

// WorkQueue는 rate limiting 기능이 있는 작업 큐이다.
type WorkQueue struct {
	mu         sync.Mutex
	cond       *sync.Cond
	queue      []string          // 처리 대기 키 목록 (FIFO)
	dirty      map[string]bool   // 큐에 있는 키 집합 (중복 방지)
	processing map[string]bool   // 현재 처리 중인 키 집합
	retries    map[string]int    // 키별 재시도 횟수
	maxRetries int
}

func NewWorkQueue(maxRetries int) *WorkQueue {
	wq := &WorkQueue{
		dirty:      make(map[string]bool),
		processing: make(map[string]bool),
		retries:    make(map[string]int),
		maxRetries: maxRetries,
	}
	wq.cond = sync.NewCond(&wq.mu)
	return wq
}

// Add는 키를 큐에 추가한다. 이미 있으면 무시한다 (중복 방지).
// 현재 처리 중인 키도 추가 가능하다 (처리 완료 후 재처리됨).
func (wq *WorkQueue) Add(key string) {
	wq.mu.Lock()
	defer wq.mu.Unlock()

	if wq.dirty[key] {
		return // 이미 큐에 있음
	}

	wq.dirty[key] = true
	wq.queue = append(wq.queue, key)
	wq.cond.Signal()
}

// Get은 큐에서 키를 꺼낸다. 비어있으면 블로킹 대기한다.
func (wq *WorkQueue) Get() string {
	wq.mu.Lock()
	defer wq.mu.Unlock()

	for len(wq.queue) == 0 {
		wq.cond.Wait()
	}

	key := wq.queue[0]
	wq.queue = wq.queue[1:]
	delete(wq.dirty, key)
	wq.processing[key] = true

	return key
}

// Done은 키 처리가 완료되었음을 알린다.
func (wq *WorkQueue) Done(key string) {
	wq.mu.Lock()
	defer wq.mu.Unlock()
	delete(wq.processing, key)
	// 처리 중에 같은 키가 다시 들어왔을 수 있으므로 dirty 확인
}

// Retry는 키를 재시도 큐에 추가한다. 최대 재시도 횟수 초과 시 false 반환.
func (wq *WorkQueue) Retry(key string) bool {
	wq.mu.Lock()
	defer wq.mu.Unlock()

	wq.retries[key]++
	if wq.retries[key] > wq.maxRetries {
		fmt.Printf("    [WorkQueue] 키 '%s' 최대 재시도 초과 (%d회) → 드롭\n", key, wq.maxRetries)
		delete(wq.retries, key)
		return false
	}

	// 지수 백오프 시뮬레이션 (실제로는 time.After를 사용)
	backoff := time.Duration(wq.retries[key]*100) * time.Millisecond
	fmt.Printf("    [WorkQueue] 키 '%s' 재시도 #%d (백오프: %v)\n", key, wq.retries[key], backoff)

	delete(wq.processing, key)
	go func() {
		time.Sleep(backoff)
		wq.Add(key)
	}()
	return true
}

// ============================================================================
// ReplicaSet Controller — Informer + WorkQueue + Reconcile 조합
// 실제 소스: pkg/controller/replicaset/replica_set.go
// ============================================================================

// RSController는 ReplicaSet을 위한 컨트롤러이다.
type RSController struct {
	apiServer *FakeAPIServer
	informer  *Informer
	workQueue *WorkQueue
}

func NewRSController(api *FakeAPIServer) *RSController {
	ctrl := &RSController{
		apiServer: api,
		informer:  NewInformer(api),
		workQueue: NewWorkQueue(3),
	}

	// Informer에 이벤트 핸들러 등록
	// 실제 K8s에서는 AddFunc/UpdateFunc/DeleteFunc으로 등록한다
	ctrl.informer.AddEventHandler(ctrl)

	return ctrl
}

// OnAdd — Informer에서 파드 추가 이벤트 수신
func (c *RSController) OnAdd(pod Pod) {
	if pod.OwnerName != "" {
		key := pod.Namespace + "/" + pod.OwnerName
		fmt.Printf("  [핸들러] OnAdd: 파드 %s → WorkQueue에 RS키 '%s' 추가\n", pod.Name, key)
		c.workQueue.Add(key)
	}
}

// OnUpdate — Informer에서 파드 업데이트 이벤트 수신
func (c *RSController) OnUpdate(oldPod, newPod Pod) {
	if newPod.OwnerName != "" {
		key := newPod.Namespace + "/" + newPod.OwnerName
		fmt.Printf("  [핸들러] OnUpdate: 파드 %s (Phase: %s→%s) → WorkQueue에 RS키 '%s' 추가\n",
			newPod.Name, oldPod.Phase, newPod.Phase, key)
		c.workQueue.Add(key)
	}
}

// OnDelete — Informer에서 파드 삭제 이벤트 수신
func (c *RSController) OnDelete(pod Pod) {
	if pod.OwnerName != "" {
		key := pod.Namespace + "/" + pod.OwnerName
		fmt.Printf("  [핸들러] OnDelete: 파드 %s → WorkQueue에 RS키 '%s' 추가\n", pod.Name, key)
		c.workQueue.Add(key)
	}
}

// Run은 컨트롤러를 시작한다.
func (c *RSController) Run(stopCh <-chan struct{}) {
	// Informer 시작
	c.informer.Run(stopCh)

	// Reconcile 워커 시작
	go c.worker(stopCh)
}

// worker는 WorkQueue에서 키를 꺼내 Reconcile을 실행하는 루프이다.
func (c *RSController) worker(stopCh <-chan struct{}) {
	for {
		select {
		case <-stopCh:
			return
		default:
		}

		key := c.workQueue.Get()
		fmt.Printf("\n  [워커] WorkQueue에서 키 '%s' 처리 시작\n", key)

		err := c.reconcile(key)
		if err != nil {
			fmt.Printf("  [워커] Reconcile 실패: %v\n", err)
			c.workQueue.Retry(key)
		} else {
			c.workQueue.Done(key)
		}
	}
}

// reconcile은 ReplicaSet의 원하는 상태와 현재 상태를 비교하여 조정한다.
// 실제 소스: pkg/controller/replicaset/replica_set.go의 syncReplicaSet
func (c *RSController) reconcile(key string) error {
	parts := splitKey(key)
	if len(parts) != 2 {
		return fmt.Errorf("잘못된 키: %s", key)
	}
	namespace, name := parts[0], parts[1]

	// ReplicaSet 조회
	rs, exists := c.apiServer.GetReplicaSet(namespace, name)
	if !exists {
		fmt.Printf("  [Reconcile] RS '%s' 삭제됨 → 건너뜀\n", key)
		return nil
	}

	// 현재 파드 상태 확인 (Informer 로컬 캐시에서)
	store := c.informer.GetStore()
	var activePods []Pod
	for _, pod := range store {
		if pod.OwnerName == rs.Name && pod.Namespace == rs.Namespace && pod.Phase != "Terminated" {
			activePods = append(activePods, pod)
		}
	}

	current := len(activePods)
	desired := rs.Replicas
	diff := desired - current

	fmt.Printf("  [Reconcile] RS '%s': 원하는=%d, 현재=%d, 차이=%d\n", key, desired, current, diff)

	if diff > 0 {
		// 파드 부족 → 생성
		for i := 0; i < diff; i++ {
			pod := &Pod{
				Name:      fmt.Sprintf("%s-%s", name, randomSuffix()),
				Namespace: namespace,
				Labels:    rs.Selector,
				OwnerName: rs.Name,
				Phase:     "Pending",
			}
			err := c.apiServer.CreatePod(pod)
			if err != nil {
				return fmt.Errorf("파드 생성 실패: %v", err)
			}
			fmt.Printf("  [Reconcile] 파드 생성: %s\n", pod.Name)
		}
	} else if diff < 0 {
		// 파드 초과 → 삭제
		toDelete := -diff
		for i := 0; i < toDelete && i < len(activePods); i++ {
			err := c.apiServer.DeletePod(activePods[i].Namespace, activePods[i].Name)
			if err != nil {
				return fmt.Errorf("파드 삭제 실패: %v", err)
			}
			fmt.Printf("  [Reconcile] 파드 삭제: %s\n", activePods[i].Name)
		}
	} else {
		fmt.Printf("  [Reconcile] 이미 원하는 상태 달성 — 변경 없음\n")
	}

	return nil
}

// ============================================================================
// 유틸리티
// ============================================================================

func randomSuffix() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 5)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return string(b)
}

func splitKey(key string) []string {
	for i, c := range key {
		if c == '/' {
			return []string{key[:i], key[i+1:]}
		}
	}
	return []string{key}
}

// ============================================================================
// 메인
// ============================================================================

func main() {
	fmt.Println("========================================")
	fmt.Println("Kubernetes 컨트롤러 패턴 시뮬레이션")
	fmt.Println("Informer + WorkQueue + Reconcile")
	fmt.Println("========================================")
	fmt.Println()
	fmt.Println("데이터 흐름:")
	fmt.Println("  API Server ──WATCH──→ Informer ──→ DeltaFIFO ──→ 로컬캐시 + 핸들러")
	fmt.Println("                                                       │")
	fmt.Println("                                                  WorkQueue")
	fmt.Println("                                                       │")
	fmt.Println("  API Server ←──CRUD──── Reconciler ←──────── Worker Loop")
	fmt.Println()

	apiServer := NewFakeAPIServer()
	stopCh := make(chan struct{})

	// ── 1단계: ReplicaSet 생성 ──
	fmt.Println("── 1단계: ReplicaSet 생성 (replicas=3) ──")
	rs := &ReplicaSet{
		Name:      "web-rs",
		Namespace: "default",
		Replicas:  3,
		Selector:  map[string]string{"app": "web"},
	}
	apiServer.CreateReplicaSet(rs)
	fmt.Printf("  ReplicaSet '%s' 생성 완료 (replicas=%d)\n", rs.Name, rs.Replicas)

	// ── 2단계: 기존 파드 1개 생성 (LIST에서 발견될 파드) ──
	fmt.Println()
	fmt.Println("── 2단계: 기존 파드 1개 미리 생성 (LIST로 발견될 파드) ──")
	existingPod := &Pod{
		Name:      "web-rs-existing",
		Namespace: "default",
		Labels:    map[string]string{"app": "web"},
		OwnerName: "web-rs",
		Phase:     "Running",
	}
	apiServer.CreatePod(existingPod)
	fmt.Printf("  파드 '%s' 생성 완료 (Phase=%s)\n", existingPod.Name, existingPod.Phase)

	// ── 3단계: 컨트롤러 시작 ──
	fmt.Println()
	fmt.Println("── 3단계: 컨트롤러 시작 ──")
	controller := NewRSController(apiServer)
	controller.Run(stopCh)

	// 초기 조정 대기 (LIST → 로컬캐시 → 핸들러 → WorkQueue → Reconcile → 파드 생성)
	time.Sleep(1 * time.Second)

	// ── 4단계: 외부에서 파드 삭제 (장애 시뮬레이션) ──
	fmt.Println()
	fmt.Println("── 4단계: 외부에서 파드 삭제 (장애 시뮬레이션) ──")
	pods := apiServer.ListPods()
	if len(pods) > 0 {
		victimPod := pods[0]
		fmt.Printf("  [시뮬레이션] 파드 '%s' 삭제 (노드 장애 시뮬레이션)\n", victimPod.Name)
		apiServer.DeletePod(victimPod.Namespace, victimPod.Name)
	}

	// 복구 대기
	time.Sleep(1 * time.Second)

	// ── 5단계: 최종 상태 확인 ──
	fmt.Println()
	fmt.Println("── 5단계: 최종 상태 확인 ──")
	finalPods := apiServer.ListPods()
	fmt.Printf("  API Server의 파드 목록 (%d개):\n", len(finalPods))
	for _, p := range finalPods {
		fmt.Printf("    - %s (Phase=%s, Owner=%s, rv=%d)\n",
			p.Name, p.Phase, p.OwnerName, p.ResourceVersion)
	}

	fmt.Println()
	fmt.Println("  Informer 로컬 캐시:")
	store := controller.informer.GetStore()
	for key, p := range store {
		fmt.Printf("    - [%s] Phase=%s, Owner=%s\n", key, p.Phase, p.OwnerName)
	}

	close(stopCh)
	time.Sleep(100 * time.Millisecond)

	fmt.Println()
	fmt.Println("========================================")
	fmt.Println("핵심 포인트:")
	fmt.Println("========================================")
	fmt.Println("1. Informer: LIST로 전체 동기화 → WATCH로 변경사항 추적 (레벨 트리거)")
	fmt.Println("2. DeltaFIFO: 같은 오브젝트의 연속 이벤트를 합침 (중복 제거)")
	fmt.Println("3. 로컬 캐시(Store): API Server 부하 감소 — 읽기는 캐시에서")
	fmt.Println("4. WorkQueue: 중복 키 방지 + 지수 백오프 재시도")
	fmt.Println("5. Reconcile: 원하는 상태(RS.Replicas) vs 현재 상태(활성 파드 수) 비교")
	fmt.Println("6. 자가 치유: 파드 삭제 → WATCH 이벤트 → Reconcile → 파드 재생성")
}
