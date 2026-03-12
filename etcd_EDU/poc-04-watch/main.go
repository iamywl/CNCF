// poc-04-watch: etcd Watch 메커니즘 시뮬레이션
//
// etcd의 Watch는 키의 변경 이벤트를 실시간으로 감시하는 메커니즘이다.
// 핵심 설계는 synced/unsynced 워처 그룹 분리와 효율적인 이벤트 배치 처리이다.
//
// 실제 구현 참조:
//   - server/storage/mvcc/watchable_store.go: watchableStore, watcher, synced/unsynced
//   - server/storage/mvcc/watcher_group.go: watcherGroup, watcherBatch, eventBatch
//
// 이 PoC는 Watch의 핵심 원리를 재현한다:
//   - synced 워처: 현재 리비전과 동기화됨 → 새 이벤트 즉시 수신
//   - unsynced 워처: 과거 이벤트를 따라잡아야 함 → syncWatchers()로 동기화
package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// ============================================================
// 기본 타입 정의
// ============================================================

type Revision struct {
	Main int64
	Sub  int64
}

func (r Revision) String() string {
	return fmt.Sprintf("%d.%d", r.Main, r.Sub)
}

// EventType: 이벤트 종류
// etcd 소스: api/v3/mvccpb/kv.proto → enum Event_EventType { PUT, DELETE }
type EventType int

const (
	EventPut    EventType = 0
	EventDelete EventType = 1
)

func (t EventType) String() string {
	if t == EventPut {
		return "PUT"
	}
	return "DELETE"
}

// Event: Watch 이벤트
// etcd 소스: api/v3/mvccpb/kv.proto → message Event
type Event struct {
	Type  EventType
	Key   string
	Value string
	Rev   int64 // ModRevision
}

func (e Event) String() string {
	if e.Type == EventDelete {
		return fmt.Sprintf("[DELETE] key=%q rev=%d", e.Key, e.Rev)
	}
	return fmt.Sprintf("[PUT] key=%q value=%q rev=%d", e.Key, e.Value, e.Rev)
}

// WatchResponse: Watch 채널로 전달되는 응답
// etcd 소스: server/storage/mvcc/watchable_store.go → type WatchResponse struct
type WatchResponse struct {
	WatchID  int64
	Events   []Event
	Revision int64 // 현재 저장소 리비전
}

// ============================================================
// Watcher: 하나의 Watch 등록
// etcd 소스: watchable_store.go → type watcher struct
//
// 핵심 필드:
//   - key: 감시 대상 키
//   - startRev: Watch 시작 리비전
//   - minRev: 최소 수신 리비전 (synced 시 currentRev+1)
//   - ch: 이벤트를 전달할 채널
// ============================================================

type Watcher struct {
	id       int64
	key      string              // 감시 대상 키 (etcd: wa.key)
	startRev int64               // Watch 시작 리비전 (etcd: wa.startRev)
	minRev   int64               // 최소 수신 리비전 (etcd: wa.minRev)
	ch       chan<- WatchResponse // 이벤트 채널 (etcd: wa.ch)
}

// ============================================================
// WatcherGroup: 워처 집합 관리
// etcd 소스: watcher_group.go → type watcherGroup struct
//
// 실제 etcd는 keyWatchers(단일 키) + ranges(범위, IntervalTree)를 구분하지만,
// 이 PoC에서는 키 기반 맵으로 단순화한다.
// ============================================================

type WatcherGroup struct {
	// keyWatchers: 키별 워처 맵
	// etcd 소스: watcherSetByKey map[string]watcherSet
	keyWatchers map[string]map[int64]*Watcher
	// watchers: 전체 워처 집합
	watchers map[int64]*Watcher
}

func NewWatcherGroup() *WatcherGroup {
	return &WatcherGroup{
		keyWatchers: make(map[string]map[int64]*Watcher),
		watchers:    make(map[int64]*Watcher),
	}
}

// add: 워처를 그룹에 추가
// etcd 소스: func (wg *watcherGroup) add(wa *watcher)
func (wg *WatcherGroup) add(w *Watcher) {
	wg.watchers[w.id] = w
	if _, ok := wg.keyWatchers[w.key]; !ok {
		wg.keyWatchers[w.key] = make(map[int64]*Watcher)
	}
	wg.keyWatchers[w.key][w.id] = w
}

// delete: 워처를 그룹에서 제거
// etcd 소스: func (wg *watcherGroup) delete(wa *watcher) bool
func (wg *WatcherGroup) delete(w *Watcher) bool {
	if _, ok := wg.watchers[w.id]; !ok {
		return false
	}
	delete(wg.watchers, w.id)
	if set, ok := wg.keyWatchers[w.key]; ok {
		delete(set, w.id)
		if len(set) == 0 {
			delete(wg.keyWatchers, w.key)
		}
	}
	return true
}

// size: 워처 수
func (wg *WatcherGroup) size() int {
	return len(wg.watchers)
}

// watcherSetByKey: 특정 키를 감시하는 워처 집합
// etcd 소스: func (wg *watcherGroup) watcherSetByKey(key string) watcherSet
func (wg *WatcherGroup) watcherSetByKey(key string) map[int64]*Watcher {
	return wg.keyWatchers[key]
}

// ============================================================
// WatchableStore: Watch 기능을 포함한 MVCC 저장소
// etcd 소스: watchable_store.go → type watchableStore struct
//
// 핵심 설계:
//   - synced 그룹: currentRev과 동기화된 워처 → 새 이벤트 즉시 수신
//   - unsynced 그룹: 과거 리비전부터 따라잡아야 하는 워처
//   - syncWatchers(): 백그라운드에서 unsynced 워처를 동기화
//   - notify(): 새 이벤트 발생 시 synced 워처에 전달
// ============================================================

type WatchableStore struct {
	mu sync.RWMutex

	// 저장소 상태
	currentRev int64
	events     []Event            // 모든 이벤트 이력 (리비전 순서)
	kvstore    map[string]string   // 현재 KV 상태
	revToKey   map[int64][]string  // 리비전 → 변경된 키 목록

	// Watch 상태
	// etcd 소스: watchableStore의 synced, unsynced watcherGroup
	synced   *WatcherGroup // 동기화된 워처 (etcd: synced watcherGroup)
	unsynced *WatcherGroup // 미동기화 워처 (etcd: unsynced watcherGroup)

	nextWatchID int64
	stopc       chan struct{}
	wg          sync.WaitGroup
}

// NewWatchableStore: Watch 기능을 포함한 저장소 생성
// etcd 소스: func New(lg *zap.Logger, b backend.Backend, le lease.Lessor, cfg StoreConfig) WatchableKV
func NewWatchableStore() *WatchableStore {
	s := &WatchableStore{
		currentRev:  1,
		events:      make([]Event, 0),
		kvstore:     make(map[string]string),
		revToKey:    make(map[int64][]string),
		synced:      NewWatcherGroup(),
		unsynced:    NewWatcherGroup(),
		nextWatchID: 1,
		stopc:       make(chan struct{}),
	}

	// 백그라운드 동기화 루프 시작
	// etcd 소스: go s.syncWatchersLoop()
	s.wg.Add(1)
	go s.syncWatchersLoop()

	return s
}

// Put: 키-값 저장 + synced 워처에 이벤트 알림
// etcd: watchable_store_txn.go → End() → notify()
func (s *WatchableStore) Put(key, value string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	rev := s.currentRev
	s.currentRev++

	s.kvstore[key] = value

	ev := Event{
		Type:  EventPut,
		Key:   key,
		Value: value,
		Rev:   rev,
	}
	s.events = append(s.events, ev)
	s.revToKey[rev] = append(s.revToKey[rev], key)

	// synced 워처에 즉시 알림
	// etcd 소스: func (s *watchableStore) notify(rev int64, evs []mvccpb.Event)
	s.notify(rev, []Event{ev})

	return rev
}

// Delete: 키 삭제 + synced 워처에 이벤트 알림
func (s *WatchableStore) Delete(key string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.kvstore[key]; !ok {
		return -1
	}

	rev := s.currentRev
	s.currentRev++

	delete(s.kvstore, key)

	ev := Event{
		Type: EventDelete,
		Key:  key,
		Rev:  rev,
	}
	s.events = append(s.events, ev)
	s.revToKey[rev] = append(s.revToKey[rev], key)

	s.notify(rev, []Event{ev})

	return rev
}

// Watch: 새 워처를 등록한다.
// etcd 소스: func (s *watchableStore) watch(key, end []byte, startRev int64, ...) (*watcher, cancelFunc)
//
// 핵심 분류 로직:
//   synced := startRev > s.store.currentRev || startRev == 0
//   if synced → synced 그룹에 추가 (즉시 이벤트 수신 가능)
//   else      → unsynced 그룹에 추가 (과거 이벤트 따라잡기 필요)
func (s *WatchableStore) Watch(key string, startRev int64) (int64, <-chan WatchResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ch := make(chan WatchResponse, 128) // etcd: chanBufLen = 128

	id := s.nextWatchID
	s.nextWatchID++

	w := &Watcher{
		id:       id,
		key:      key,
		startRev: startRev,
		minRev:   startRev,
		ch:       ch,
	}

	// synced vs unsynced 분류
	// etcd 소스: synced := startRev > s.store.currentRev || startRev == 0
	synced := startRev == 0 || startRev >= s.currentRev
	if synced {
		// synced: currentRev+1부터 이벤트 수신
		w.minRev = s.currentRev
		if startRev > w.minRev {
			w.minRev = startRev
		}
		s.synced.add(w)
		fmt.Printf("  [Watch #%d] 키=%q startRev=%d → synced 그룹 (minRev=%d)\n",
			id, key, startRev, w.minRev)
	} else {
		// unsynced: 과거 이벤트를 따라잡아야 함
		s.unsynced.add(w)
		fmt.Printf("  [Watch #%d] 키=%q startRev=%d → unsynced 그룹 (과거 이벤트 따라잡기 필요)\n",
			id, key, startRev)
	}

	return id, ch
}

// notify: 새 이벤트를 synced 워처에 전달한다.
// etcd 소스: func (s *watchableStore) notify(rev int64, evs []mvccpb.Event)
//
// 핵심 로직:
// 1. synced 그룹에서 이벤트의 키를 감시하는 워처 찾기
// 2. 워처의 minRev 이상인 이벤트만 전달
// 3. 전달 후 minRev를 rev+1로 업데이트
func (s *WatchableStore) notify(rev int64, evs []Event) {
	for _, ev := range evs {
		watchers := s.synced.watcherSetByKey(ev.Key)
		for _, w := range watchers {
			if ev.Rev >= w.minRev {
				resp := WatchResponse{
					WatchID:  w.id,
					Events:   []Event{ev},
					Revision: rev,
				}
				// 비동기 전송 (채널이 가득 차면 스킵)
				// etcd: func (w *watcher) send(wr WatchResponse) bool
				select {
				case w.ch <- resp:
					// 전달 성공
				default:
					// 채널 가득 참 → etcd에서는 victim 처리
					fmt.Printf("  [경고] Watch #%d 채널 가득 참\n", w.id)
				}
				// minRev 업데이트
				// etcd: w.minRev = rev + 1
				w.minRev = rev + 1
			}
		}
	}
}

// syncWatchersLoop: 백그라운드에서 unsynced 워처를 주기적으로 동기화한다.
// etcd 소스: func (s *watchableStore) syncWatchersLoop()
//   - 100ms 간격으로 실행 (etcd: watchResyncPeriod = 100ms)
func (s *WatchableStore) syncWatchersLoop() {
	defer s.wg.Done()

	// etcd: delayTicker := time.NewTicker(watchResyncPeriod)
	ticker := time.NewTicker(50 * time.Millisecond) // 데모용으로 짧게
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.syncWatchers()
		case <-s.stopc:
			return
		}
	}
}

// syncWatchers: unsynced 워처의 과거 이벤트를 동기화한다.
// etcd 소스: func (s *watchableStore) syncWatchers() int
//
// 핵심 알고리즘:
// 1. unsynced 워처들 중 최소 minRev 찾기
// 2. minRev부터 currentRev까지의 이벤트를 조회
// 3. 각 워처에 해당하는 이벤트를 배치로 전달
// 4. 완료된 워처를 synced 그룹으로 이동
func (s *WatchableStore) syncWatchers() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.unsynced.size() == 0 {
		return
	}

	curRev := s.currentRev

	// 1. 최소 minRev 찾기
	// etcd: wg, minRev := s.unsynced.choose(maxWatchersPerSync, curRev, compactionRev)
	minRev := int64(1<<63 - 1)
	for _, w := range s.unsynced.watchers {
		if w.minRev < minRev {
			minRev = w.minRev
		}
	}

	// 2. minRev부터 currentRev까지의 이벤트 조회
	// etcd: evs := rangeEvents(s.store.lg, s.store.b, minRev, curRev+1, wg)
	var relevantEvents []Event
	for _, ev := range s.events {
		if ev.Rev >= minRev && ev.Rev < curRev {
			relevantEvents = append(relevantEvents, ev)
		}
	}

	// 3. 각 워처에 이벤트 배치 전달
	// etcd: wb := newWatcherBatch(wg, evs)
	toSync := make([]*Watcher, 0)
	for _, w := range s.unsynced.watchers {
		var watcherEvents []Event
		for _, ev := range relevantEvents {
			if ev.Key == w.key && ev.Rev >= w.minRev {
				watcherEvents = append(watcherEvents, ev)
			}
		}

		if len(watcherEvents) > 0 {
			resp := WatchResponse{
				WatchID:  w.id,
				Events:   watcherEvents,
				Revision: curRev - 1,
			}
			select {
			case w.ch <- resp:
			default:
			}
		}

		// 4. synced 그룹으로 이동 준비
		// etcd: w.minRev = curRev + 1 → s.synced.add(w)
		w.minRev = curRev
		toSync = append(toSync, w)
	}

	// unsynced → synced 이동
	for _, w := range toSync {
		s.unsynced.delete(w)
		s.synced.add(w)
	}

	if len(toSync) > 0 {
		fmt.Printf("  [syncWatchers] %d개 워처를 unsynced → synced로 이동\n", len(toSync))
	}
}

// Close: 저장소를 닫는다.
func (s *WatchableStore) Close() {
	close(s.stopc)
	s.wg.Wait()
}

// ============================================================
// 메인: Watch 메커니즘의 핵심 동작을 시연
// ============================================================

func main() {
	fmt.Println("=== etcd Watch 메커니즘 시뮬레이션 ===")
	fmt.Println()

	store := NewWatchableStore()
	defer store.Close()

	// ----------------------------------------
	// 시나리오 1: 먼저 데이터를 넣고 Watch 등록
	// ----------------------------------------
	fmt.Println("--- 시나리오 1: 사전 데이터 + synced Watch ---")
	rev1 := store.Put("config/db_host", "localhost")
	rev2 := store.Put("config/db_port", "5432")
	rev3 := store.Put("config/db_host", "10.0.0.1")
	fmt.Printf("  사전 데이터: rev=%d, %d, %d\n", rev1, rev2, rev3)
	fmt.Println()

	// startRev=0 → synced 워처 (최신 이벤트부터)
	fmt.Println("Watch 등록 (startRev=0, 최신부터):")
	watchID1, ch1 := store.Watch("config/db_host", 0)
	fmt.Println()

	// 새 이벤트 발생 → synced 워처가 즉시 수신
	fmt.Println("새 이벤트 발생:")
	store.Put("config/db_host", "192.168.1.1")

	// 이벤트 수신 확인
	time.Sleep(10 * time.Millisecond)
	select {
	case resp := <-ch1:
		fmt.Printf("  Watch #%d 수신: ", watchID1)
		for _, ev := range resp.Events {
			fmt.Printf("%s ", ev)
		}
		fmt.Println()
	default:
		fmt.Println("  (이벤트 없음)")
	}
	fmt.Println()

	// ----------------------------------------
	// 시나리오 2: 과거 리비전부터 Watch (unsynced)
	// ----------------------------------------
	fmt.Println("--- 시나리오 2: 과거 리비전 Watch (unsynced → synced) ---")
	fmt.Println("Watch 등록 (startRev=1, 과거부터):")
	watchID2, ch2 := store.Watch("config/db_host", 1)
	fmt.Println()

	// syncWatchers가 과거 이벤트를 전달할 때까지 대기
	fmt.Println("syncWatchers가 과거 이벤트를 전달하는 중...")
	time.Sleep(200 * time.Millisecond)

	// 수신된 과거 이벤트 확인
	fmt.Printf("Watch #%d 수신 (과거 이벤트):\n", watchID2)
	drainChannel(ch2)
	fmt.Println()

	// 이제 synced 상태이므로 새 이벤트도 수신
	fmt.Println("synced 상태에서 새 이벤트:")
	store.Put("config/db_host", "10.0.0.99")
	time.Sleep(10 * time.Millisecond)

	select {
	case resp := <-ch2:
		fmt.Printf("  Watch #%d 수신: ", watchID2)
		for _, ev := range resp.Events {
			fmt.Printf("%s ", ev)
		}
		fmt.Println()
	default:
		fmt.Println("  (이벤트 없음)")
	}
	fmt.Println()

	// ----------------------------------------
	// 시나리오 3: 여러 키에 대한 Watch
	// ----------------------------------------
	fmt.Println("--- 시나리오 3: 다중 키 Watch ---")
	_, chA := store.Watch("server/addr", 0)
	_, chB := store.Watch("server/port", 0)
	fmt.Println()

	store.Put("server/addr", "10.0.0.1")
	store.Put("server/port", "8080")
	store.Put("server/name", "web01") // 이 키는 Watch하지 않음
	store.Put("server/addr", "10.0.0.2")

	time.Sleep(10 * time.Millisecond)

	fmt.Println("server/addr Watch 수신:")
	drainChannel(chA)
	fmt.Println("server/port Watch 수신:")
	drainChannel(chB)
	fmt.Println()

	// ----------------------------------------
	// 시나리오 4: DELETE 이벤트
	// ----------------------------------------
	fmt.Println("--- 시나리오 4: DELETE 이벤트 ---")
	_, chDel := store.Watch("temp/key", 0)
	fmt.Println()

	store.Put("temp/key", "temporary_value")
	time.Sleep(10 * time.Millisecond)
	drainChannel(chDel)

	store.Delete("temp/key")
	time.Sleep(10 * time.Millisecond)
	fmt.Println("DELETE 이벤트:")
	drainChannel(chDel)
	fmt.Println()

	// ----------------------------------------
	// 시나리오 5: synced/unsynced 상태 요약
	// ----------------------------------------
	fmt.Println("--- 시나리오 5: synced/unsynced 상태 ---")
	store.mu.RLock()
	fmt.Printf("  synced 워처 수: %d\n", store.synced.size())
	fmt.Printf("  unsynced 워처 수: %d\n", store.unsynced.size())
	fmt.Printf("  현재 리비전: %d\n", store.currentRev)
	fmt.Printf("  전체 이벤트 수: %d\n", len(store.events))
	store.mu.RUnlock()

	fmt.Println()
	fmt.Println("=== Watch 메커니즘 핵심 원리 ===")
	fmt.Println("1. synced 워처: currentRev과 동기화됨 → notify()로 즉시 이벤트 수신")
	fmt.Println("2. unsynced 워처: startRev < currentRev → 과거 이벤트를 따라잡아야 함")
	fmt.Println("3. syncWatchers(): 주기적으로 unsynced 워처의 과거 이벤트 전달")
	fmt.Println("4. 동기화 완료 후 unsynced → synced 이동")
	fmt.Println("5. 채널이 가득 차면 victim 처리 (이 PoC에서는 스킵)")
	fmt.Println()
	fmt.Println("실제 etcd 추가 기능:")
	fmt.Println("  - 범위 Watch (key+end): IntervalTree로 관리")
	fmt.Println("  - victim 워처: 채널 블로킹 시 별도 큐에서 재전송")
	fmt.Println("  - WatchStream: 여러 Watch를 하나의 gRPC 스트림으로 멀티플렉싱")
	fmt.Println("  - Progress 알림: 이벤트 없을 때도 현재 리비전 전달")
}

// drainChannel: 채널에서 모든 대기 중인 응답을 읽어 출력한다.
func drainChannel(ch <-chan WatchResponse) {
	for {
		select {
		case resp := <-ch:
			evStrs := make([]string, len(resp.Events))
			for i, ev := range resp.Events {
				evStrs[i] = ev.String()
			}
			fmt.Printf("  WatchID=%d, rev=%d: %s\n",
				resp.WatchID, resp.Revision, strings.Join(evStrs, ", "))
		default:
			return
		}
	}
}
