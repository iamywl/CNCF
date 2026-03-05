package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Kubernetes etcd Watch + ResourceVersion 메커니즘 시뮬레이션
//
// 실제 구현 참조:
//   - staging/src/k8s.io/apiserver/pkg/storage/etcd3/store.go (store, GuaranteedUpdate)
//   - staging/src/k8s.io/apiserver/pkg/storage/cacher/watch_cache.go (watchCache, watchCacheEvent)
//
// 핵심 개념:
//   1. 모든 쓰기 연산은 전역 revision을 증가시킨다 (etcd의 MVCC revision)
//   2. 각 키는 자체 ModRevision(=ResourceVersion)을 가진다
//   3. Watch는 특정 revision 이후의 변경 이벤트를 스트리밍한다
//   4. GuaranteedUpdate는 compare-and-swap 루프로 낙관적 동시성을 보장한다
//   5. OptimisticPut은 키가 없을 때만 생성한다 (Create semantics)
// =============================================================================

// --- 이벤트 타입 ---

// WatchEventType은 Watch 이벤트의 종류를 나타낸다.
// 실제 Kubernetes: k8s.io/apimachinery/pkg/watch.EventType
type WatchEventType string

const (
	EventAdded    WatchEventType = "ADDED"
	EventModified WatchEventType = "MODIFIED"
	EventDeleted  WatchEventType = "DELETED"
)

// WatchEvent는 하나의 변경 이벤트를 나타낸다.
// 실제 watchCacheEvent(watch_cache.go:71)에 대응하며,
// ResourceVersion 필드로 이벤트 순서를 보장한다.
type WatchEvent struct {
	Type            WatchEventType
	Key             string
	Value           string
	ResourceVersion int64 // 이 이벤트가 발생한 시점의 글로벌 revision
}

// --- 키-값 항목 ---

// KeyValue는 etcd에 저장된 하나의 키-값 쌍이다.
// 실제 etcd3/store.go의 objState(line 108)에 대응한다.
type KeyValue struct {
	Key             string
	Value           string
	ModRevision     int64 // 마지막 수정된 시점의 revision (= ResourceVersion)
	CreateRevision  int64 // 최초 생성된 시점의 revision
}

// --- Watcher ---

// Watcher는 특정 prefix의 변경사항을 구독하는 클라이언트이다.
// 실제 watch_cache.go의 cacheWatcher에 대응한다.
type Watcher struct {
	id       int
	prefix   string          // 감시 대상 키 prefix
	startRev int64           // 이 revision 이후의 이벤트만 수신
	ch       chan WatchEvent  // 이벤트 수신 채널
	done     chan struct{}    // 종료 시그널
}

// ResultChan은 이벤트 수신 채널을 반환한다.
func (w *Watcher) ResultChan() <-chan WatchEvent {
	return w.ch
}

// Stop은 Watcher를 종료한다.
func (w *Watcher) Stop() {
	select {
	case <-w.done:
		// 이미 종료됨
	default:
		close(w.done)
	}
}

// --- EtcdStore ---

// EtcdStore는 etcd의 MVCC 스토리지를 시뮬레이션한다.
// 실제 etcd3/store.go의 store struct(line 80)에 대응한다.
//
// 핵심 설계:
//   - revision: 전역 단조 증가 카운터 (etcd의 global revision)
//   - eventLog: 과거 이벤트를 보관하는 슬라이딩 윈도우 (watchCache의 cache 배열에 대응)
//   - watchers: 활성 Watch 구독자 목록
type EtcdStore struct {
	mu       sync.RWMutex
	data     map[string]*KeyValue // 현재 상태
	revision int64                // 전역 revision (단조 증가)
	eventLog []WatchEvent         // 이벤트 히스토리 (watchCache의 sliding window)

	watcherMu   sync.Mutex
	watchers    []*Watcher
	nextWatchID int
}

// NewEtcdStore는 새 스토어를 생성한다.
func NewEtcdStore() *EtcdStore {
	return &EtcdStore{
		data:     make(map[string]*KeyValue),
		revision: 0,
		eventLog: make([]WatchEvent, 0, 1000),
	}
}

// CurrentRevision은 현재 전역 revision을 반환한다.
func (s *EtcdStore) CurrentRevision() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.revision
}

// Get은 키의 현재 값을 반환한다.
func (s *EtcdStore) Get(key string) (*KeyValue, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	kv, ok := s.data[key]
	if !ok {
		return nil, false
	}
	// 복사본 반환 (실제 etcd는 snapshot isolation)
	cp := *kv
	return &cp, true
}

// Put은 키-값을 저장하고 이벤트를 발행한다.
// revision이 증가하며, 새 키면 ADDED, 기존 키면 MODIFIED 이벤트가 발생한다.
func (s *EtcdStore) Put(key, value string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.revision++
	rev := s.revision

	existing, exists := s.data[key]
	eventType := EventAdded
	if exists {
		eventType = EventModified
		existing.Value = value
		existing.ModRevision = rev
	} else {
		s.data[key] = &KeyValue{
			Key:            key,
			Value:          value,
			ModRevision:    rev,
			CreateRevision: rev,
		}
	}

	event := WatchEvent{
		Type:            eventType,
		Key:             key,
		Value:           value,
		ResourceVersion: rev,
	}

	s.eventLog = append(s.eventLog, event)
	s.notifyWatchers(event)
	return rev
}

// OptimisticPut은 키가 존재하지 않을 때만 생성한다.
// etcd의 Create semantics에 대응한다.
// 실제 Kubernetes에서 리소스 생성 시 동일 이름이 이미 존재하면 AlreadyExists 오류를 반환하는 원리.
func (s *EtcdStore) OptimisticPut(key, value string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.data[key]; exists {
		return 0, fmt.Errorf("key %q already exists (AlreadyExists)", key)
	}

	s.revision++
	rev := s.revision

	s.data[key] = &KeyValue{
		Key:            key,
		Value:          value,
		ModRevision:    rev,
		CreateRevision: rev,
	}

	event := WatchEvent{
		Type:            EventAdded,
		Key:             key,
		Value:           value,
		ResourceVersion: rev,
	}

	s.eventLog = append(s.eventLog, event)
	s.notifyWatchers(event)
	return rev, nil
}

// GuaranteedUpdate는 compare-and-swap 루프로 업데이트를 보장한다.
// 실제 etcd3/store.go의 GuaranteedUpdate에 대응한다.
//
// 동작 원리:
//   1. 현재 값을 읽는다 (Get)
//   2. updateFunc로 새 값을 계산한다
//   3. 읽은 시점의 ModRevision과 현재 ModRevision이 같은지 확인한다 (CAS)
//   4. 다르면 (다른 goroutine이 중간에 수정) 1부터 재시도한다
//   5. 같으면 업데이트를 적용한다
//
// maxRetries: 최대 재시도 횟수 (무한 루프 방지)
func (s *EtcdStore) GuaranteedUpdate(key string, maxRetries int, updateFunc func(current string) string) (int64, error) {
	for attempt := 0; attempt <= maxRetries; attempt++ {
		// 1단계: 현재 값 읽기
		s.mu.RLock()
		kv, exists := s.data[key]
		var currentValue string
		var currentRev int64
		if exists {
			currentValue = kv.Value
			currentRev = kv.ModRevision
		}
		s.mu.RUnlock()

		// 2단계: 새 값 계산 (락 밖에서 - 비용이 큰 연산 가능)
		newValue := updateFunc(currentValue)

		// 3단계: CAS (Compare-And-Swap)
		s.mu.Lock()
		kv2, exists2 := s.data[key]

		if !exists && !exists2 {
			// 키가 아직 없음 - 새로 생성
			s.revision++
			rev := s.revision
			s.data[key] = &KeyValue{
				Key:            key,
				Value:          newValue,
				ModRevision:    rev,
				CreateRevision: rev,
			}
			event := WatchEvent{
				Type:            EventAdded,
				Key:             key,
				Value:           newValue,
				ResourceVersion: rev,
			}
			s.eventLog = append(s.eventLog, event)
			s.notifyWatchers(event)
			s.mu.Unlock()
			return rev, nil
		}

		if exists2 && kv2.ModRevision == currentRev {
			// ModRevision이 변하지 않음 - 안전하게 업데이트
			s.revision++
			rev := s.revision
			kv2.Value = newValue
			kv2.ModRevision = rev
			event := WatchEvent{
				Type:            EventModified,
				Key:             key,
				Value:           newValue,
				ResourceVersion: rev,
			}
			s.eventLog = append(s.eventLog, event)
			s.notifyWatchers(event)
			s.mu.Unlock()
			return rev, nil
		}

		// ModRevision이 변경됨 - 재시도 필요 (conflict)
		s.mu.Unlock()
		fmt.Printf("  [GuaranteedUpdate] CAS 충돌 감지 (attempt %d): expected rev=%d, actual rev=%d\n",
			attempt+1, currentRev, kv2.ModRevision)
	}

	return 0, fmt.Errorf("GuaranteedUpdate 실패: %d회 재시도 후에도 CAS 충돌", maxRetries+1)
}

// Delete는 키를 삭제하고 DELETED 이벤트를 발행한다.
func (s *EtcdStore) Delete(key string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	kv, exists := s.data[key]
	if !exists {
		return 0, fmt.Errorf("key %q not found", key)
	}

	s.revision++
	rev := s.revision

	event := WatchEvent{
		Type:            EventDeleted,
		Key:             key,
		Value:           kv.Value,
		ResourceVersion: rev,
	}

	delete(s.data, key)
	s.eventLog = append(s.eventLog, event)
	s.notifyWatchers(event)
	return rev, nil
}

// Watch는 지정된 prefix와 startRevision 이후의 이벤트를 스트리밍하는 Watcher를 생성한다.
// 실제 watchCache의 Watch() 메서드에 대응한다.
//
// 동작 원리:
//   1. startRev 이후의 과거 이벤트를 eventLog에서 찾아 즉시 전송 (히스토리 재생)
//   2. 이후 새로운 이벤트는 실시간으로 전송
//   3. prefix가 ""이면 모든 키를 감시, 아니면 해당 prefix로 시작하는 키만 감시
func (s *EtcdStore) Watch(prefix string, startRev int64) *Watcher {
	s.watcherMu.Lock()
	s.nextWatchID++
	w := &Watcher{
		id:       s.nextWatchID,
		prefix:   prefix,
		startRev: startRev,
		ch:       make(chan WatchEvent, 100),
		done:     make(chan struct{}),
	}
	s.watchers = append(s.watchers, w)
	s.watcherMu.Unlock()

	// 과거 이벤트 히스토리 재생 (watch_cache.go의 getAllEventsSinceLocked에 대응)
	s.mu.RLock()
	for _, event := range s.eventLog {
		if event.ResourceVersion > startRev && matchesPrefix(event.Key, prefix) {
			select {
			case w.ch <- event:
			case <-w.done:
				s.mu.RUnlock()
				return w
			}
		}
	}
	s.mu.RUnlock()

	return w
}

// notifyWatchers는 모든 활성 Watcher에게 이벤트를 전달한다.
// 호출자가 s.mu.Lock()을 보유하고 있어야 한다.
func (s *EtcdStore) notifyWatchers(event WatchEvent) {
	s.watcherMu.Lock()
	defer s.watcherMu.Unlock()

	alive := make([]*Watcher, 0, len(s.watchers))
	for _, w := range s.watchers {
		select {
		case <-w.done:
			close(w.ch)
			continue
		default:
		}

		if event.ResourceVersion > w.startRev && matchesPrefix(event.Key, w.prefix) {
			select {
			case w.ch <- event:
			default:
				// 채널이 가득 차면 이벤트 드롭 (실제로는 에러 처리)
				fmt.Printf("  [WARN] Watcher %d: 이벤트 드롭 (채널 가득 참)\n", w.id)
			}
		}
		alive = append(alive, w)
	}
	s.watchers = alive
}

// matchesPrefix는 키가 지정된 prefix로 시작하는지 확인한다.
func matchesPrefix(key, prefix string) bool {
	if prefix == "" {
		return true
	}
	return strings.HasPrefix(key, prefix)
}

// --- 데모 실행 ---

func main() {
	fmt.Println("=== Kubernetes etcd Watch + ResourceVersion 시뮬레이션 ===")
	fmt.Println()

	store := NewEtcdStore()

	// -----------------------------------------------
	// 1. 기본 CRUD + ResourceVersion 추적
	// -----------------------------------------------
	fmt.Println("--- 1. 기본 CRUD + ResourceVersion 추적 ---")

	rev := store.Put("/registry/pods/default/nginx", `{"name":"nginx","image":"nginx:1.19"}`)
	fmt.Printf("  PUT nginx → revision=%d\n", rev)

	rev = store.Put("/registry/pods/default/redis", `{"name":"redis","image":"redis:6"}`)
	fmt.Printf("  PUT redis → revision=%d\n", rev)

	rev = store.Put("/registry/pods/default/nginx", `{"name":"nginx","image":"nginx:1.21"}`)
	fmt.Printf("  PUT nginx (update) → revision=%d\n", rev)

	kv, ok := store.Get("/registry/pods/default/nginx")
	if ok {
		fmt.Printf("  GET nginx: value=%s, ModRevision=%d, CreateRevision=%d\n",
			kv.Value, kv.ModRevision, kv.CreateRevision)
	}

	fmt.Printf("  현재 글로벌 revision: %d\n", store.CurrentRevision())
	fmt.Println()

	// -----------------------------------------------
	// 2. OptimisticPut (Create-only semantics)
	// -----------------------------------------------
	fmt.Println("--- 2. OptimisticPut (키가 없을 때만 생성) ---")

	rev, err := store.OptimisticPut("/registry/pods/default/memcached", `{"name":"memcached"}`)
	if err != nil {
		fmt.Printf("  OptimisticPut memcached 실패: %v\n", err)
	} else {
		fmt.Printf("  OptimisticPut memcached 성공 → revision=%d\n", rev)
	}

	// 같은 키로 다시 생성 시도 → 실패해야 함
	rev, err = store.OptimisticPut("/registry/pods/default/memcached", `{"name":"memcached-v2"}`)
	if err != nil {
		fmt.Printf("  OptimisticPut memcached 재시도: %v (정상 - 이미 존재)\n", err)
	}
	fmt.Println()

	// -----------------------------------------------
	// 3. Watch 테스트 (히스토리 재생 + 실시간 이벤트)
	// -----------------------------------------------
	fmt.Println("--- 3. Watch (히스토리 재생 + 실시간 이벤트) ---")

	// revision 0부터 Watch → 과거 모든 이벤트를 히스토리에서 재생
	watcher := store.Watch("/registry/pods/", 0)
	fmt.Println("  Watcher 생성: prefix=/registry/pods/, startRev=0")

	// 과거 이벤트 수신 (히스토리)
	fmt.Println("  [히스토리 재생]")
	drainTimeout := time.After(100 * time.Millisecond)
historyLoop:
	for {
		select {
		case event := <-watcher.ResultChan():
			fmt.Printf("    %s %s (rev=%d)\n", event.Type, event.Key, event.ResourceVersion)
		case <-drainTimeout:
			break historyLoop
		}
	}

	// 실시간 이벤트 수신 테스트
	fmt.Println("  [실시간 이벤트]")
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		count := 0
		for count < 3 {
			select {
			case event := <-watcher.ResultChan():
				fmt.Printf("    실시간: %s %s = %s (rev=%d)\n",
					event.Type, event.Key, event.Value, event.ResourceVersion)
				count++
			case <-time.After(1 * time.Second):
				return
			}
		}
	}()

	time.Sleep(50 * time.Millisecond) // Watcher가 준비될 시간

	store.Put("/registry/pods/default/postgres", `{"name":"postgres"}`)
	store.Put("/registry/pods/kube-system/coredns", `{"name":"coredns"}`)
	store.Delete("/registry/pods/default/redis")

	wg.Wait()
	watcher.Stop()
	fmt.Println()

	// -----------------------------------------------
	// 4. 특정 revision부터 Watch (부분 히스토리)
	// -----------------------------------------------
	fmt.Println("--- 4. 특정 revision부터 Watch ---")

	currentRev := store.CurrentRevision()
	fmt.Printf("  현재 revision: %d\n", currentRev)

	// 중간 revision부터 Watch
	midRev := currentRev - 3
	watcher2 := store.Watch("/registry/pods/", midRev)
	fmt.Printf("  Watcher 생성: startRev=%d (revision %d 이후 이벤트만)\n", midRev, midRev)

	drainTimeout2 := time.After(100 * time.Millisecond)
	fmt.Println("  수신된 이벤트:")
historyLoop2:
	for {
		select {
		case event := <-watcher2.ResultChan():
			fmt.Printf("    %s %s (rev=%d)\n", event.Type, event.Key, event.ResourceVersion)
		case <-drainTimeout2:
			break historyLoop2
		}
	}
	watcher2.Stop()
	fmt.Println()

	// -----------------------------------------------
	// 5. GuaranteedUpdate (CAS 루프)
	// -----------------------------------------------
	fmt.Println("--- 5. GuaranteedUpdate (Compare-And-Swap 루프) ---")

	store.Put("/registry/configmaps/default/config", `{"data":{"count":"0"}}`)
	fmt.Println("  초기값: count=0")

	// 동시에 여러 goroutine이 같은 키를 업데이트
	var updateWg sync.WaitGroup
	for i := 0; i < 5; i++ {
		updateWg.Add(1)
		go func(id int) {
			defer updateWg.Done()
			rev, err := store.GuaranteedUpdate("/registry/configmaps/default/config", 10,
				func(current string) string {
					// 현재 값에서 count를 추출하여 +1
					// 실제로는 JSON 파싱하지만, 여기서는 단순화
					return fmt.Sprintf(`{"data":{"count":"%d","updater":"%d"}}`, id+1, id)
				})
			if err != nil {
				fmt.Printf("  goroutine %d: 업데이트 실패: %v\n", id, err)
			} else {
				fmt.Printf("  goroutine %d: 업데이트 성공 → revision=%d\n", id, rev)
			}
		}(i)
	}
	updateWg.Wait()

	finalKV, _ := store.Get("/registry/configmaps/default/config")
	fmt.Printf("  최종 값: %s (revision=%d)\n", finalKV.Value, finalKV.ModRevision)
	fmt.Println()

	// -----------------------------------------------
	// 6. ResourceVersion 일관성 검증
	// -----------------------------------------------
	fmt.Println("--- 6. ResourceVersion 일관성 검증 ---")

	// 모든 이벤트의 ResourceVersion이 단조 증가하는지 확인
	store2 := NewEtcdStore()
	for i := 0; i < 10; i++ {
		store2.Put(fmt.Sprintf("/key/%d", i), fmt.Sprintf("value-%d", i))
	}
	store2.Put("/key/3", "value-3-updated")
	store2.Delete("/key/5")

	watcher3 := store2.Watch("/key/", 0)
	var lastRev int64
	monotonic := true
	count := 0
	timeout3 := time.After(100 * time.Millisecond)
rvLoop:
	for {
		select {
		case event := <-watcher3.ResultChan():
			if event.ResourceVersion <= lastRev {
				fmt.Printf("  ResourceVersion 역전 감지: prev=%d, curr=%d\n", lastRev, event.ResourceVersion)
				monotonic = false
			}
			lastRev = event.ResourceVersion
			count++
		case <-timeout3:
			break rvLoop
		}
	}
	watcher3.Stop()

	if monotonic {
		fmt.Printf("  총 %d개 이벤트, 모든 ResourceVersion이 단조 증가함 (1 → %d)\n", count, lastRev)
	} else {
		fmt.Println("  ResourceVersion 일관성 위반 감지!")
	}

	fmt.Println()
	fmt.Println("=== 시뮬레이션 완료 ===")
	fmt.Println()
	fmt.Println("핵심 요약:")
	fmt.Println("  1. 전역 revision은 모든 쓰기 연산에서 단조 증가한다 (etcd MVCC)")
	fmt.Println("  2. 각 키의 ModRevision이 Kubernetes의 ResourceVersion이 된다")
	fmt.Println("  3. Watch는 startRev 이후의 히스토리를 재생한 뒤 실시간 이벤트를 스트리밍한다")
	fmt.Println("  4. OptimisticPut은 키 부재를 전제로 한 원자적 생성이다")
	fmt.Println("  5. GuaranteedUpdate는 CAS 루프로 낙관적 동시성을 처리한다")
}
