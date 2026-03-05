// poc-03-build-queue: Jenkins 빌드 큐 4단계 상태 머신 시뮬레이션
//
// Jenkins의 빌드 큐(Queue.java, 약 3,252줄)가 작업을 접수하고 Executor에 할당하기까지의
// 전체 상태 머신을 Go 표준 라이브러리만으로 재현한다.
//
// 핵심 시뮬레이션:
//   1. 4개 컬렉션 기반 상태 머신: waitingList → blockedProjects / buildables → pendings → left
//   2. maintain() 루프: 주기적으로 큐 상태를 갱신하는 5단계 유지보수
//   3. LoadBalancer: 빌드 가능 아이템을 적합한 Executor에 배치
//   4. Snapshot 패턴: 잠금 없이 일관된 큐 상태 읽기
//   5. QueueSorter: 빌드 우선순위 정렬
//
// 실제 Jenkins 소스 참조:
//   - core/src/main/java/hudson/model/Queue.java
//     → 4개 컬렉션: waitingList(TreeSet<WaitingItem>), blockedProjects(ItemList<BlockedItem>),
//       buildables(ItemList<BuildableItem>), pendings(ItemList<BuildableItem>)
//     → leftItems: Guava Cache (5분 TTL)
//     → maintain(): Phase 1~5로 상태 전이 실행
//     → Snapshot 클래스: volatile 참조로 잠금 없는 읽기
//   - core/src/main/java/hudson/model/LoadBalancer.java
//     → map(Task, MappingWorksheet): Executor 매핑 전략
//     → CONSISTENT_HASH: ConsistentHash 기반 기본 구현
//   - core/src/main/java/hudson/model/queue/QueueSorter.java
//     → sortBuildableItems(): 빌드 가능 항목 우선순위 정렬
//     → DEFAULT_BLOCKED_ITEM_COMPARATOR: inQueueSince 기준 정렬
//   - core/src/main/java/hudson/model/queue/MappingWorksheet.java
//     → 작업-Executor 매핑 문제 정의
//
// 실행: go run main.go

package main

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// =============================================================================
// 1. Queue.Item 계층 구조
// =============================================================================
// 실제: Queue.java 2260~2937줄
// Actionable → Queue.Item(abstract) → WaitingItem / NotWaitingItem(abstract) → BlockedItem / BuildableItem
//                                    → LeftItem

// ItemState 는 큐 아이템의 상태를 나타낸다.
// 실제 Jenkins에서는 별도 enum이 아니라, 아이템이 어떤 컬렉션에 속하는지로 상태를 구분한다.
type ItemState int

const (
	StateWaiting  ItemState = iota // waitingList에 속함 (quiet period 대기)
	StateBlocked                   // blockedProjects에 속함 (리소스 부족 등)
	StateBuildable                 // buildables에 속함 (즉시 빌드 가능, Executor 대기)
	StatePending                   // pendings에 속함 (Executor에 할당됨, 실행 시작 전)
	StateLeft                      // leftItems에 속함 (큐를 떠남 - 실행 시작 또는 취소)
)

var stateNames = map[ItemState]string{
	StateWaiting:   "WAITING",
	StateBlocked:   "BLOCKED",
	StateBuildable: "BUILDABLE",
	StatePending:   "PENDING",
	StateLeft:      "LEFT",
}

func (s ItemState) String() string {
	if name, ok := stateNames[s]; ok {
		return name
	}
	return fmt.Sprintf("UNKNOWN(%d)", s)
}

// CauseOfBlockage 는 작업이 차단된 이유를 나타낸다.
// 실제: Queue.java의 CauseOfBlockage 추상 클래스
// - Task.getCauseOfBlockage(): 태스크 자체 차단
// - ResourceActivity: 리소스 점유
// - QueueTaskDispatcher.canRun(): 플러그인 거부권
type CauseOfBlockage struct {
	Reason string
	Fatal  bool // isFatal()이 true이면 취소됨
}

// QueueItem 은 큐에 들어가는 작업 아이템이다.
// 실제: Queue.Item (abstract class extending Actionable)
// - id: 전체 Jenkins 인스턴스에서 고유한 ID (상태 이동해도 유지)
// - task: 빌드할 프로젝트 (Task 인터페이스)
// - inQueueSince: 큐 진입 시각
// - future: FutureImpl (비동기 완료 추적)
type QueueItem struct {
	ID           int64
	TaskName     string     // 실제: Task 인터페이스 (equals()로 중복 검사)
	State        ItemState
	Priority     int        // QueueSorter가 사용하는 우선순위 (낮을수록 높은 우선순위)
	QuietUntil   time.Time  // WaitingItem.timestamp: 이 시각 이후에 실행 가능
	InQueueSince time.Time  // Queue.Item.inQueueSince
	BlockCause   *CauseOfBlockage
	AssignedNode string     // 할당된 노드 이름
	Cancelled    bool       // LeftItem.isCancelled()
	Labels       []string   // 작업이 요구하는 레이블 (노드 매칭용)

	// 실제: BuildableItem.buildableStartMilliseconds (NotWaitingItem에서 상속)
	// waiting 상태를 벗어난 시점 기록
	BuildableStartTime time.Time
}

func (item *QueueItem) String() string {
	label := ""
	if len(item.Labels) > 0 {
		label = fmt.Sprintf(" [labels: %s]", strings.Join(item.Labels, ","))
	}
	return fmt.Sprintf("Item#%d(%s, priority=%d%s)", item.ID, item.TaskName, item.Priority, label)
}

// =============================================================================
// 2. Executor: 빌드 실행 슬롯
// =============================================================================
// 실제: hudson/model/Executor.java
// - Executor는 Thread를 확장한 클래스
// - 각 Computer(노드)마다 numExecutors개의 Executor 보유
// - isParking(): 유휴 상태 (JobOffer 생성 대상)

// Executor 는 빌드를 실행하는 슬롯이다.
type Executor struct {
	ID       int
	NodeName string   // 소속 노드
	Labels   []string // 노드가 가진 레이블
	Busy     bool
	Current  *QueueItem // 현재 실행 중인 작업
}

func (e *Executor) String() string {
	status := "idle"
	if e.Busy {
		status = fmt.Sprintf("running %s", e.Current.TaskName)
	}
	return fmt.Sprintf("Executor#%d@%s(%s)", e.ID, e.NodeName, status)
}

// =============================================================================
// 3. QueueListener: 큐 이벤트 리스너
// =============================================================================
// 실제: hudson/model/queue/QueueListener.java
// - onEnterWaiting, onLeaveWaiting, onEnterBlocked, onLeaveBlocked,
//   onEnterBuildable, onLeaveBuildable, onLeft

// QueueListener 는 큐 상태 전이 이벤트를 수신하는 리스너이다.
type QueueListener interface {
	OnEnterWaiting(item *QueueItem)
	OnLeaveWaiting(item *QueueItem)
	OnEnterBlocked(item *QueueItem)
	OnLeaveBlocked(item *QueueItem)
	OnEnterBuildable(item *QueueItem)
	OnLeaveBuildable(item *QueueItem)
	OnLeft(item *QueueItem)
}

// LoggingListener 는 모든 이벤트를 로그로 출력하는 리스너이다.
type LoggingListener struct{}

func (l *LoggingListener) OnEnterWaiting(item *QueueItem) {
	fmt.Printf("    [QueueListener] onEnterWaiting: %s (quiet until %s)\n",
		item, item.QuietUntil.Format("15:04:05.000"))
}
func (l *LoggingListener) OnLeaveWaiting(item *QueueItem)   {
	fmt.Printf("    [QueueListener] onLeaveWaiting: %s\n", item)
}
func (l *LoggingListener) OnEnterBlocked(item *QueueItem)   {
	fmt.Printf("    [QueueListener] onEnterBlocked: %s (cause: %s)\n", item, item.BlockCause.Reason)
}
func (l *LoggingListener) OnLeaveBlocked(item *QueueItem)   {
	fmt.Printf("    [QueueListener] onLeaveBlocked: %s\n", item)
}
func (l *LoggingListener) OnEnterBuildable(item *QueueItem) {
	fmt.Printf("    [QueueListener] onEnterBuildable: %s\n", item)
}
func (l *LoggingListener) OnLeaveBuildable(item *QueueItem) {
	fmt.Printf("    [QueueListener] onLeaveBuildable: %s\n", item)
}
func (l *LoggingListener) OnLeft(item *QueueItem) {
	result := "EXECUTED"
	if item.Cancelled {
		result = "CANCELLED"
	}
	fmt.Printf("    [QueueListener] onLeft: %s (%s)\n", item, result)
}

// =============================================================================
// 4. QueueSorter: 빌드 우선순위 정렬
// =============================================================================
// 실제: hudson/model/queue/QueueSorter.java
// - sortBuildableItems(List<BuildableItem>): 빌드 가능 항목 정렬
// - sortBlockedItems(List<BlockedItem>): 차단 항목 정렬
// - DEFAULT_BLOCKED_ITEM_COMPARATOR: inQueueSince 기준 정렬

// QueueSorter 는 큐 아이템을 우선순위에 따라 정렬하는 인터페이스이다.
type QueueSorter interface {
	SortBuildableItems(items []*QueueItem) []*QueueItem
	SortBlockedItems(items []*QueueItem) []*QueueItem
}

// PriorityQueueSorter 는 우선순위 기반 정렬기이다.
// 실제 Jenkins에서는 플러그인(Priority Sorter Plugin 등)이 QueueSorter를 구현한다.
type PriorityQueueSorter struct{}

func (s *PriorityQueueSorter) SortBuildableItems(items []*QueueItem) []*QueueItem {
	sorted := make([]*QueueItem, len(items))
	copy(sorted, items)
	sort.Slice(sorted, func(i, j int) bool {
		// 1차: 우선순위 (낮을수록 높은 우선순위)
		if sorted[i].Priority != sorted[j].Priority {
			return sorted[i].Priority < sorted[j].Priority
		}
		// 2차: 큐 진입 시간 (FIFO)
		return sorted[i].InQueueSince.Before(sorted[j].InQueueSince)
	})
	return sorted
}

func (s *PriorityQueueSorter) SortBlockedItems(items []*QueueItem) []*QueueItem {
	// 실제: DEFAULT_BLOCKED_ITEM_COMPARATOR = Comparator.comparingLong(Queue.Item::getInQueueSince)
	sorted := make([]*QueueItem, len(items))
	copy(sorted, items)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].InQueueSince.Before(sorted[j].InQueueSince)
	})
	return sorted
}

// =============================================================================
// 5. LoadBalancer: Executor 배치 전략
// =============================================================================
// 실제: hudson/model/LoadBalancer.java
// - map(Task, MappingWorksheet) → Mapping 또는 null
// - CONSISTENT_HASH: ConsistentHash 기반 기본 구현
//   → 각 WorkChunk에 대해 ConsistentHash 구성
//   → 가중치 = Executor 수 * 100
//   → assignGreedily()로 탐욕적 할당

// LoadBalancer 는 빌드 가능 아이템을 어떤 Executor에서 실행할지 결정하는 전략이다.
type LoadBalancer interface {
	// Map 은 아이템에 적합한 Executor를 반환한다.
	// nil을 반환하면 "지금은 실행할 수 없음"을 의미한다.
	Map(item *QueueItem, candidates []*Executor) *Executor
}

// ConsistentHashLoadBalancer 는 Jenkins의 기본 LoadBalancer(CONSISTENT_HASH)를 시뮬레이션한다.
// 실제: LoadBalancer.java 82~140줄
// - Task의 affinityKey를 해시하여 일관된 노드에 할당
// - 같은 작업은 가능하면 같은 노드에서 실행 (빌드 캐시 활용)
type ConsistentHashLoadBalancer struct{}

func (lb *ConsistentHashLoadBalancer) Map(item *QueueItem, candidates []*Executor) *Executor {
	if len(candidates) == 0 {
		return nil
	}

	// 레이블 매칭: 작업이 요구하는 레이블을 가진 Executor만 후보
	var matched []*Executor
	for _, exec := range candidates {
		if matchLabels(item.Labels, exec.Labels) {
			matched = append(matched, exec)
		}
	}
	if len(matched) == 0 {
		return nil
	}

	// 실제: ConsistentHash<ExecutorChunk>로 task.getAffinityKey() 해싱
	// 여기서는 간단히 task 이름의 해시로 일관된 노드 선택
	hash := simpleHash(item.TaskName)
	idx := hash % len(matched)
	return matched[idx]
}

// matchLabels 는 작업이 요구하는 레이블이 Executor의 레이블에 포함되는지 확인한다.
func matchLabels(required, available []string) bool {
	if len(required) == 0 {
		return true // 레이블 요구사항 없음
	}
	avail := make(map[string]bool)
	for _, l := range available {
		avail[l] = true
	}
	for _, r := range required {
		if !avail[r] {
			return false
		}
	}
	return true
}

func simpleHash(s string) int {
	h := 0
	for _, c := range s {
		h = h*31 + int(c)
	}
	if h < 0 {
		h = -h
	}
	return h
}

// =============================================================================
// 6. Snapshot: 불변 큐 스냅샷
// =============================================================================
// 실제: Queue.java 3085~3103줄
// private static class Snapshot {
//     private final Set<WaitingItem> waitingList;
//     private final List<BlockedItem> blockedProjects;
//     private final List<BuildableItem> buildables;
//     private final List<BuildableItem> pendings;
//     Snapshot(...) { // 방어적 복사 }
// }
// - volatile Snapshot snapshot: 잠금 없이 일관된 읽기 제공
// - updateSnapshot(): lock 안에서 호출, 새 Snapshot 생성 후 volatile 쓰기

// QueueSnapshot 은 큐의 4개 컬렉션에 대한 불변 스냅샷이다.
type QueueSnapshot struct {
	WaitingList     []*QueueItem
	BlockedProjects []*QueueItem
	Buildables      []*QueueItem
	Pendings        []*QueueItem
	CreatedAt       time.Time
}

func (s *QueueSnapshot) String() string {
	return fmt.Sprintf("Snapshot{waiting=%d, blocked=%d, buildable=%d, pending=%d}",
		len(s.WaitingList), len(s.BlockedProjects), len(s.Buildables), len(s.Pendings))
}

// TotalItems 는 스냅샷의 전체 아이템 수를 반환한다.
func (s *QueueSnapshot) TotalItems() int {
	return len(s.WaitingList) + len(s.BlockedProjects) + len(s.Buildables) + len(s.Pendings)
}

// =============================================================================
// 7. ScheduleResult: 스케줄링 결과
// =============================================================================
// 실제: hudson/model/queue/ScheduleResult.java
// - Created: 새 WaitingItem 생성됨
// - Existing: 이미 큐에 동일 Task 존재, FoldableAction으로 병합
// - Refused: QueueDecisionHandler가 거부

type ScheduleResultType int

const (
	ResultCreated  ScheduleResultType = iota // 새로 생성됨
	ResultExisting                            // 기존 항목에 병합
	ResultRefused                             // 거부됨
)

type ScheduleResult struct {
	Type ScheduleResultType
	Item *QueueItem
}

// =============================================================================
// 8. Queue: 빌드 큐 핵심
// =============================================================================
// 실제: hudson/model/Queue.java (3,252줄)
// - 4개 컬렉션 + leftItems(Guava Cache)
// - ReentrantLock + Condition으로 동시성 제어
// - maintain()으로 상태 전이

// Queue 는 Jenkins 빌드 큐의 핵심 구현이다.
type Queue struct {
	mu sync.Mutex // 실제: ReentrantLock (Queue.java 349줄)

	// --- 4개 핵심 컬렉션 (Queue.java 193~215줄) ---

	// waitingList: quiet period이 지나지 않은 항목
	// 실제: TreeSet<WaitingItem> (timestamp 순 자동 정렬)
	waitingList []*QueueItem

	// blockedProjects: 차단 조건이 존재하는 항목
	// 실제: ItemList<BlockedItem> (ArrayList 확장, Task 기반 검색)
	blockedProjects []*QueueItem

	// buildables: 즉시 빌드 가능, Executor 대기 중
	// 실제: ItemList<BuildableItem> (QueueSorter에 의한 순서 변경)
	buildables []*QueueItem

	// pendings: Executor에 할당됨, 실행 시작 전
	// 실제: ItemList<BuildableItem> (isPending = true)
	pendings []*QueueItem

	// leftItems: 큐를 떠난 항목 (5분 TTL)
	// 실제: Cache<Long, LeftItem> (Guava CacheBuilder)
	leftItems []*QueueItem

	// --- 부속 컴포넌트 ---
	executors    []*Executor
	loadBalancer LoadBalancer
	sorter       QueueSorter
	listeners    []QueueListener

	// snapshot: volatile 참조로 잠금 없는 읽기 제공
	// 실제: private transient volatile Snapshot snapshot
	snapshot atomic.Value // *QueueSnapshot

	// nextID: 아이템 ID 생성기
	// 실제: QueueIdStrategy.get().generateIdFor()
	nextID int64

	// blockConditions: 태스크별 차단 조건 (시뮬레이션용)
	// 실제: Task.getCauseOfBlockage(), QueueTaskDispatcher.canRun() 등
	blockConditions map[string]*CauseOfBlockage
}

// NewQueue 는 새 빌드 큐를 생성한다.
func NewQueue(executors []*Executor, lb LoadBalancer, sorter QueueSorter) *Queue {
	q := &Queue{
		executors:       executors,
		loadBalancer:    lb,
		sorter:          sorter,
		listeners:       []QueueListener{&LoggingListener{}},
		blockConditions: make(map[string]*CauseOfBlockage),
	}
	q.updateSnapshot()
	return q
}

// =============================================================================
// 8.1 schedule2(): 작업 스케줄링
// =============================================================================
// 실제: Queue.java 581~596줄
// public ScheduleResult schedule2(Task p, int quietPeriod, List<Action> actions) {
//     lock.lock();
//     try { try {
//         for (QueueDecisionHandler h : QueueDecisionHandler.all())
//             if (!h.shouldSchedule(p, actions))
//                 return ScheduleResult.refused();
//         return scheduleInternal(p, quietPeriod, actions);
//     } finally { updateSnapshot(); } } finally { lock.unlock(); }
// }

// Schedule 은 작업을 큐에 등록한다.
// quietPeriod: 작업이 즉시 실행되지 않고 대기하는 시간 (quiet period)
func (q *Queue) Schedule(taskName string, priority int, quietPeriod time.Duration, labels []string) ScheduleResult {
	q.mu.Lock()
	defer q.mu.Unlock()
	defer q.updateSnapshot() // 실제: finally { updateSnapshot(); }

	// --- 중복 검사 (scheduleInternal, Queue.java 611~676줄) ---
	// 실제: liveGetItems(p)로 동일 Task를 가진 모든 Item을 찾고,
	//       QueueAction.shouldSchedule()로 새 스케줄이 필요한지 확인
	for _, item := range q.allLiveItems() {
		if item.TaskName == taskName && !item.Cancelled {
			fmt.Printf("  [schedule] 중복 감지: %s 이미 큐에 존재 (상태: %s)\n",
				taskName, item.State)
			// 실제: FoldableAction.foldIntoExisting()으로 정보 병합
			// WaitingItem이면 timestamp를 더 이른 시각으로 갱신
			return ScheduleResult{Type: ResultExisting, Item: item}
		}
	}

	// --- 새 WaitingItem 생성 (Queue.java 538~541줄) ---
	q.nextID++
	now := time.Now()
	item := &QueueItem{
		ID:           q.nextID,
		TaskName:     taskName,
		State:        StateWaiting,
		Priority:     priority,
		QuietUntil:   now.Add(quietPeriod),
		InQueueSince: now,
		Labels:       labels,
	}

	// enter(Queue q) → waitingList에 추가
	q.waitingList = append(q.waitingList, item)
	// 실제: TreeSet이므로 timestamp 순 자동 정렬
	sort.Slice(q.waitingList, func(i, j int) bool {
		if q.waitingList[i].QuietUntil.Equal(q.waitingList[j].QuietUntil) {
			return q.waitingList[i].ID < q.waitingList[j].ID
		}
		return q.waitingList[i].QuietUntil.Before(q.waitingList[j].QuietUntil)
	})

	for _, l := range q.listeners {
		l.OnEnterWaiting(item)
	}

	fmt.Printf("  [schedule] 새 아이템 생성: %s (quiet period: %v)\n", item, quietPeriod)
	return ScheduleResult{Type: ResultCreated, Item: item}
}

// =============================================================================
// 8.2 maintain(): 큐 유지보수 루프 (Queue의 심장)
// =============================================================================
// 실제: Queue.java 1593~1829줄
// maintain()은 5개 Phase로 구성:
//   Phase 1: parked Executor 수집, lost pending 복구
//   Phase 2: blocked → buildable 전이
//   Phase 3: waitingList → buildable/blocked 전이
//   Phase 4: QueueSorter 적용
//   Phase 5: buildables → Executor 할당

// Maintain 은 큐의 모든 상태 전이를 한 번 실행한다.
func (q *Queue) Maintain() {
	q.mu.Lock()
	defer q.mu.Unlock()
	defer q.updateSnapshot()

	now := time.Now()

	// === Phase 1: parked Executor 수집 & lost pendings 처리 ===
	// 실제: Queue.java 1604~1637줄
	// 유휴(parking) Executor에서 JobOffer를 생성하고,
	// 할당된 Executor가 사라진 pending 항목을 buildable로 복귀
	var idleExecutors []*Executor
	for _, exec := range q.executors {
		if !exec.Busy {
			idleExecutors = append(idleExecutors, exec)
		}
	}

	// lost pendings 확인: pending인데 Executor가 없는 경우
	// 실제: lostPendings 리스트에서 WorkUnit.context.item을 제거하며 확인
	var lostPendings []*QueueItem
	for _, item := range q.pendings {
		found := false
		for _, exec := range q.executors {
			if exec.Current != nil && exec.Current.ID == item.ID {
				found = true
				break
			}
		}
		if !found {
			lostPendings = append(lostPendings, item)
		}
	}
	for _, item := range lostPendings {
		fmt.Printf("  [maintain/Phase1] lost pending 복구: %s → BUILDABLE\n", item)
		q.removeFromPendings(item)
		item.State = StateBuildable
		q.buildables = append(q.buildables, item)
		for _, l := range q.listeners {
			l.OnEnterBuildable(item)
		}
	}

	// === Phase 2: blocked → buildable 전이 ===
	// 실제: Queue.java 1640~1664줄
	// 차단 조건이 해제된 blockedProjects를 buildable로 이동
	// isFatal() 차단이면 취소
	if q.sorter != nil {
		q.blockedProjects = q.sorter.SortBlockedItems(q.blockedProjects)
	}
	var remainingBlocked []*QueueItem
	for _, item := range q.blockedProjects {
		cause := q.getCauseOfBlockage(item)
		if cause == nil {
			// 차단 해제 → buildable로 이동
			fmt.Printf("  [maintain/Phase2] 차단 해제: %s → BUILDABLE\n", item)
			for _, l := range q.listeners {
				l.OnLeaveBlocked(item)
			}
			item.State = StateBuildable
			item.BlockCause = nil
			item.BuildableStartTime = now
			q.buildables = append(q.buildables, item)
			for _, l := range q.listeners {
				l.OnEnterBuildable(item)
			}
		} else if cause.Fatal {
			// fatal 차단 → 취소
			fmt.Printf("  [maintain/Phase2] fatal 차단으로 취소: %s (cause: %s)\n", item, cause.Reason)
			for _, l := range q.listeners {
				l.OnLeaveBlocked(item)
			}
			item.State = StateLeft
			item.Cancelled = true
			q.leftItems = append(q.leftItems, item)
			for _, l := range q.listeners {
				l.OnLeft(item)
			}
		} else {
			// 여전히 차단 → 유지
			// 실제: p.leave(this); new BlockedItem(p, causeOfBlockage).enter(this);
			item.BlockCause = cause
			remainingBlocked = append(remainingBlocked, item)
		}
	}
	q.blockedProjects = remainingBlocked

	// === Phase 3: waitingList → buildable/blocked 전이 ===
	// 실제: Queue.java 1667~1690줄
	// timestamp가 현재 시각을 지난 WaitingItem을 처리
	// waitingList는 TreeSet이므로 peek()로 가장 이른 항목부터 확인
	var remainingWaiting []*QueueItem
	for _, item := range q.waitingList {
		if item.QuietUntil.After(now) {
			// 아직 quiet period 진행 중
			remainingWaiting = append(remainingWaiting, item)
			continue
		}

		// quiet period 만료
		for _, l := range q.listeners {
			l.OnLeaveWaiting(item)
		}

		// 차단 조건 확인
		cause := q.getCauseOfBlockage(item)
		if cause == nil {
			// 차단 없음 → buildable
			fmt.Printf("  [maintain/Phase3] quiet period 만료: %s → BUILDABLE\n", item)
			item.State = StateBuildable
			item.BuildableStartTime = now
			q.buildables = append(q.buildables, item)
			for _, l := range q.listeners {
				l.OnEnterBuildable(item)
			}
		} else {
			// 차단 있음 → blocked
			fmt.Printf("  [maintain/Phase3] quiet period 만료, 차단됨: %s → BLOCKED (cause: %s)\n",
				item, cause.Reason)
			item.State = StateBlocked
			item.BlockCause = cause
			item.BuildableStartTime = now
			q.blockedProjects = append(q.blockedProjects, item)
			for _, l := range q.listeners {
				l.OnEnterBlocked(item)
			}
		}
	}
	q.waitingList = remainingWaiting

	// === Phase 4: QueueSorter 적용 ===
	// 실제: Queue.java 1692~1695줄
	// if (sorter != null) { sorter.sortBuildableItems(buildables); }
	if q.sorter != nil && len(q.buildables) > 0 {
		q.buildables = q.sorter.SortBuildableItems(q.buildables)
		fmt.Printf("  [maintain/Phase4] QueueSorter 적용: buildables %d개 정렬 완료\n", len(q.buildables))
	}

	// === Phase 5: buildables → Executor 할당 ===
	// 실제: Queue.java 1698~1749줄
	// - 마지막 차단 확인 (buildable → blocked 전환 가능)
	// - FlyweightTask: OneOffExecutor로 직접 실행
	// - 일반 태스크: LoadBalancer.map() → Mapping.execute()
	var remainingBuildables []*QueueItem
	for _, item := range q.buildables {
		// 마지막 차단 확인
		cause := q.getCauseOfBlockage(item)
		if cause != nil {
			// buildable → blocked 전환 (마지막 순간에 차단 조건 재발견)
			fmt.Printf("  [maintain/Phase5] 마지막 차단 확인: %s → BLOCKED (cause: %s)\n",
				item, cause.Reason)
			for _, l := range q.listeners {
				l.OnLeaveBuildable(item)
			}
			item.State = StateBlocked
			item.BlockCause = cause
			q.blockedProjects = append(q.blockedProjects, item)
			for _, l := range q.listeners {
				l.OnEnterBlocked(item)
			}
			continue
		}

		// LoadBalancer로 Executor 매핑
		// 실제: MappingWorksheet ws = new MappingWorksheet(p, candidates);
		//       Mapping m = loadBalancer.map(p.task, ws);
		exec := q.loadBalancer.Map(item, idleExecutors)
		if exec == nil {
			// 매핑 실패 → buildables에 유지
			// 실제: p.transientCausesOfBlockage에 원인 목록 기록
			fmt.Printf("  [maintain/Phase5] Executor 매핑 실패: %s (적합한 Executor 없음)\n", item)
			remainingBuildables = append(remainingBuildables, item)
			continue
		}

		// 매핑 성공 → pending 전이
		// 실제: WorkUnitContext wuc = new WorkUnitContext(p);
		//       m.execute(wuc);  → Executor에 작업 할당
		//       makePending(p);  → buildable → pending
		fmt.Printf("  [maintain/Phase5] Executor 할당: %s → %s → PENDING\n", item, exec)
		for _, l := range q.listeners {
			l.OnLeaveBuildable(item)
		}
		item.State = StatePending
		item.AssignedNode = exec.NodeName
		q.pendings = append(q.pendings, item)

		// Executor를 busy 상태로 변경
		exec.Busy = true
		exec.Current = item
		// idle 목록에서 제거
		for i, e := range idleExecutors {
			if e.ID == exec.ID && e.NodeName == exec.NodeName {
				idleExecutors = append(idleExecutors[:i], idleExecutors[i+1:]...)
				break
			}
		}
	}
	q.buildables = remainingBuildables
}

// =============================================================================
// 8.3 onStartExecuting(): 실행 시작 (pending → left)
// =============================================================================
// 실제: Queue.java 1175~1186줄
// Executor 스레드가 Executable.run()을 호출하기 직전에 호출
// pendings에서 제거하고 leftItems에 등록

// StartExecuting 은 pending 아이템의 실행을 시작한다 (pending → left 전이).
func (q *Queue) StartExecuting(item *QueueItem) {
	q.mu.Lock()
	defer q.mu.Unlock()
	defer q.updateSnapshot()

	q.removeFromPendings(item)
	item.State = StateLeft
	q.leftItems = append(q.leftItems, item)
	for _, l := range q.listeners {
		l.OnLeft(item)
	}
	fmt.Printf("  [onStartExecuting] 실행 시작: %s @ %s\n", item, item.AssignedNode)
}

// =============================================================================
// 8.4 cancel(): 작업 취소
// =============================================================================
// 실제: Queue.java 721~735줄
// waitingList, blockedProjects, buildables에서 동일 Task 검색 후 취소
// 비트 OR(|)로 short-circuit 방지 (양쪽 모두 취소 보장)

// Cancel 은 지정된 태스크를 큐에서 취소한다.
func (q *Queue) Cancel(taskName string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	defer q.updateSnapshot()

	cancelled := false

	// waitingList에서 검색
	for i, item := range q.waitingList {
		if item.TaskName == taskName {
			q.waitingList = append(q.waitingList[:i], q.waitingList[i+1:]...)
			for _, l := range q.listeners {
				l.OnLeaveWaiting(item)
			}
			item.State = StateLeft
			item.Cancelled = true
			q.leftItems = append(q.leftItems, item)
			for _, l := range q.listeners {
				l.OnLeft(item)
			}
			cancelled = true
			break
		}
	}

	// 실제: blockedProjects.cancel(p) != null | buildables.cancel(p) != null
	// 비트 OR(|)로 양쪽 모두 평가 (short-circuit 방지)
	for i, item := range q.blockedProjects {
		if item.TaskName == taskName {
			q.blockedProjects = append(q.blockedProjects[:i], q.blockedProjects[i+1:]...)
			for _, l := range q.listeners {
				l.OnLeaveBlocked(item)
			}
			item.State = StateLeft
			item.Cancelled = true
			q.leftItems = append(q.leftItems, item)
			for _, l := range q.listeners {
				l.OnLeft(item)
			}
			cancelled = true
			break
		}
	}

	for i, item := range q.buildables {
		if item.TaskName == taskName {
			q.buildables = append(q.buildables[:i], q.buildables[i+1:]...)
			for _, l := range q.listeners {
				l.OnLeaveBuildable(item)
			}
			item.State = StateLeft
			item.Cancelled = true
			q.leftItems = append(q.leftItems, item)
			for _, l := range q.listeners {
				l.OnLeft(item)
			}
			cancelled = true
			break
		}
	}

	return cancelled
}

// =============================================================================
// 8.5 SetBlockCondition / RemoveBlockCondition: 차단 조건 관리
// =============================================================================

// SetBlockCondition 은 태스크에 차단 조건을 설정한다.
func (q *Queue) SetBlockCondition(taskName string, cause *CauseOfBlockage) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.blockConditions[taskName] = cause
}

// RemoveBlockCondition 은 태스크의 차단 조건을 제거한다.
func (q *Queue) RemoveBlockCondition(taskName string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.blockConditions, taskName)
}

// =============================================================================
// 8.6 GetSnapshot(): 잠금 없는 읽기
// =============================================================================
// 실제: Queue.java 793~812줄
// public Item[] getItems() {
//     Snapshot s = this.snapshot; // volatile 읽기
//     // s에서 안전하게 조회
// }

// GetSnapshot 은 큐의 현재 상태 스냅샷을 반환한다 (잠금 불필요).
func (q *Queue) GetSnapshot() *QueueSnapshot {
	val := q.snapshot.Load()
	if val == nil {
		return &QueueSnapshot{}
	}
	return val.(*QueueSnapshot)
}

// --- 내부 헬퍼 메서드 ---

// getCauseOfBlockage 는 아이템의 차단 원인을 확인한다.
// 실제: Queue.getCauseOfBlockageForItem(Item)
// - Task.getCauseOfBlockage()
// - ResourceActivity 리소스 점유 확인
// - QueueTaskDispatcher.canRun(Item) 플러그인 거부권
func (q *Queue) getCauseOfBlockage(item *QueueItem) *CauseOfBlockage {
	if cause, ok := q.blockConditions[item.TaskName]; ok {
		return cause
	}
	return nil
}

// updateSnapshot 은 새 스냅샷을 생성하여 volatile 참조를 갱신한다.
// 실제: Queue.java 737~743줄
// private void updateSnapshot() {
//     Snapshot revised = new Snapshot(waitingList, blockedProjects, buildables, pendings);
//     snapshot = revised; // volatile 쓰기
// }
func (q *Queue) updateSnapshot() {
	snap := &QueueSnapshot{
		WaitingList:     copySlice(q.waitingList),
		BlockedProjects: copySlice(q.blockedProjects),
		Buildables:      copySlice(q.buildables),
		Pendings:        copySlice(q.pendings),
		CreatedAt:       time.Now(),
	}
	q.snapshot.Store(snap)
}

func copySlice(src []*QueueItem) []*QueueItem {
	if len(src) == 0 {
		return nil
	}
	dst := make([]*QueueItem, len(src))
	copy(dst, src)
	return dst
}

func (q *Queue) removeFromPendings(item *QueueItem) {
	for i, p := range q.pendings {
		if p.ID == item.ID {
			q.pendings = append(q.pendings[:i], q.pendings[i+1:]...)
			return
		}
	}
}

func (q *Queue) allLiveItems() []*QueueItem {
	var items []*QueueItem
	items = append(items, q.waitingList...)
	items = append(items, q.blockedProjects...)
	items = append(items, q.buildables...)
	items = append(items, q.pendings...)
	return items
}

// =============================================================================
// 9. 시각화 헬퍼
// =============================================================================

func printQueueState(q *Queue) {
	snap := q.GetSnapshot()
	fmt.Println()
	fmt.Println("  ╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("  ║                    큐 상태 스냅샷                             ║")
	fmt.Println("  ╠══════════════════════════════════════════════════════════════╣")

	printCollection("waitingList    (TreeSet<WaitingItem>)", snap.WaitingList)
	printCollection("blockedProjects(ItemList<BlockedItem>)", snap.BlockedProjects)
	printCollection("buildables     (ItemList<BuildableItem>)", snap.Buildables)
	printCollection("pendings       (ItemList<BuildableItem>)", snap.Pendings)

	fmt.Println("  ╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()
}

func printCollection(name string, items []*QueueItem) {
	fmt.Printf("  ║ %-40s [%d개]         ║\n", name, len(items))
	for _, item := range items {
		extra := ""
		if item.BlockCause != nil {
			extra = fmt.Sprintf(" (차단: %s)", item.BlockCause.Reason)
		}
		if item.AssignedNode != "" {
			extra = fmt.Sprintf(" (노드: %s)", item.AssignedNode)
		}
		fmt.Printf("  ║   - %-52s   ║\n", fmt.Sprintf("%s%s", item, extra))
	}
}

func printExecutorState(executors []*Executor) {
	fmt.Println("  ┌──────────────────────────────────────────────────────────────┐")
	fmt.Println("  │                    Executor 상태                              │")
	fmt.Println("  ├──────────────────────────────────────────────────────────────┤")
	for _, exec := range executors {
		status := "  IDLE"
		if exec.Busy {
			status = fmt.Sprintf("  BUSY → %s", exec.Current.TaskName)
		}
		labels := ""
		if len(exec.Labels) > 0 {
			labels = fmt.Sprintf(" [%s]", strings.Join(exec.Labels, ","))
		}
		fmt.Printf("  │  Executor#%d@%-10s%-6s%s\n", exec.ID, exec.NodeName, status, labels)
	}
	fmt.Println("  └──────────────────────────────────────────────────────────────┘")
}

func printStateDiagram() {
	fmt.Println(`
  Jenkins 빌드 큐 상태 머신 (Queue.java 155~161줄의 Javadoc)
  ═══════════════════════════════════════════════════════════

                    ┌──────────────┐
                    │    enter     │  schedule2() 호출
                    └──────┬───────┘
                           │
                           v
                    ┌──────────────┐
                    │ waitingList  │  TreeSet<WaitingItem>
                    │ (quiet period│  timestamp 순 자동 정렬
                    │   대기)      │  compareTo(): timestamp → id
                    └──────┬───────┘
                           │ quiet period 만료
              ┌────────────┴────────────┐
              │                         │
              v                         v
       ┌──────────────┐         ┌──────────────┐
       │ blockedProjects│       │  buildables   │
       │ (차단 조건     │<─────>│ (즉시 빌드    │
       │   존재)       │ 양방향 │   가능)       │
       └──────────────┘  전이   └──────┬───────┘
       ItemList<BlockedItem>            │ LoadBalancer.map()
       CauseOfBlockage:                 │ → Mapping.execute()
       - Task 자체 차단                  v
       - 리소스 점유          ┌──────────────┐
       - 플러그인 거부권       │   pendings   │  ItemList<BuildableItem>
       - 동시빌드 불가         │ (Executor    │  isPending = true
                              │   할당됨)    │
                              └──────┬───────┘
                                     │ onStartExecuting()
                                     v
                              ┌──────────────┐
                              │  leftItems   │  Cache<Long, LeftItem>
                              │ (완료/취소)   │  5분 TTL (Guava Cache)
                              └──────────────┘
`)
}

func printMaintainPhases() {
	fmt.Println(`
  maintain() 5단계 (Queue.java 1593~1829줄)
  ═══════════════════════════════════════════

  ┌─────────────────────────────────────────────────────┐
  │ Phase 1: parked Executor 수집 & lost pending 복구    │
  │  - 유휴(parking) Executor → JobOffer 생성            │
  │  - Executor 소실된 pending → buildable로 복귀         │
  ├─────────────────────────────────────────────────────┤
  │ Phase 2: blocked → buildable 전이                    │
  │  - getCauseOfBlockageForItem() == null → buildable   │
  │  - isFatal() → cancel()                             │
  │  - 각 전이 후 updateSnapshot() (JENKINS-28926)       │
  ├─────────────────────────────────────────────────────┤
  │ Phase 3: waitingList → buildable/blocked 전이         │
  │  - timestamp <= now → quiet period 만료              │
  │  - 차단 없음 → buildable                             │
  │  - 차단 있음 → blocked                               │
  ├─────────────────────────────────────────────────────┤
  │ Phase 4: QueueSorter 적용                            │
  │  - sorter.sortBuildableItems(buildables)             │
  │  - 우선순위에 따라 실행 순서 결정                       │
  ├─────────────────────────────────────────────────────┤
  │ Phase 5: buildables → Executor 할당                  │
  │  - 마지막 차단 확인 (buildable → blocked 가능)         │
  │  - FlyweightTask → OneOffExecutor 직접 실행           │
  │  - 일반: LoadBalancer.map() → Mapping.execute()      │
  │  - 매핑 성공 → pending / 매핑 실패 → buildables 유지   │
  └─────────────────────────────────────────────────────┘
`)
}

// =============================================================================
// 10. 메인: 시나리오 실행
// =============================================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════════╗")
	fmt.Println("║  Jenkins 빌드 큐 상태 머신 시뮬레이션                              ║")
	fmt.Println("║  실제: core/src/main/java/hudson/model/Queue.java (3,252줄)      ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════╝")

	// =========================================================================
	// 시나리오 1: 상태 머신 다이어그램 출력
	// =========================================================================
	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("시나리오 1: 상태 머신 다이어그램")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	printStateDiagram()
	printMaintainPhases()

	// =========================================================================
	// 시나리오 2: 기본 상태 전이 (waiting → buildable → pending → left)
	// =========================================================================
	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("시나리오 2: 기본 상태 전이 흐름")
	fmt.Println("  schedule() → waitingList → (quiet period 만료) → buildables")
	fmt.Println("  → (LoadBalancer) → pendings → (onStartExecuting) → left")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	executors := []*Executor{
		{ID: 1, NodeName: "master", Labels: []string{"linux", "java"}},
		{ID: 2, NodeName: "master", Labels: []string{"linux", "java"}},
		{ID: 1, NodeName: "agent-1", Labels: []string{"linux", "docker"}},
		{ID: 1, NodeName: "agent-2", Labels: []string{"windows", "dotnet"}},
	}

	queue := NewQueue(executors, &ConsistentHashLoadBalancer{}, &PriorityQueueSorter{})

	fmt.Println("\n[Step 1] 작업 3개 스케줄링 (quiet period: 0~200ms)")
	queue.Schedule("my-app-build", 2, 0, nil)
	queue.Schedule("unit-tests", 1, 100*time.Millisecond, nil)
	queue.Schedule("deploy-staging", 3, 200*time.Millisecond, nil)

	printQueueState(queue)

	fmt.Println("[Step 2] 첫 번째 maintain() - quiet period 만료된 항목 처리")
	time.Sleep(50 * time.Millisecond)
	queue.Maintain()
	printQueueState(queue)
	printExecutorState(executors)

	fmt.Println("[Step 3] 두 번째 maintain() - 나머지 항목 처리")
	time.Sleep(200 * time.Millisecond)
	queue.Maintain()
	printQueueState(queue)
	printExecutorState(executors)

	fmt.Println("[Step 4] onStartExecuting() - pending → left 전이")
	snap := queue.GetSnapshot()
	for _, item := range snap.Pendings {
		queue.StartExecuting(item)
	}
	printQueueState(queue)

	// Executor 리셋 (다음 시나리오를 위해)
	for _, exec := range executors {
		exec.Busy = false
		exec.Current = nil
	}

	// =========================================================================
	// 시나리오 3: 차단과 해제 (waiting → blocked → buildable)
	// =========================================================================
	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("시나리오 3: 차단과 해제")
	fmt.Println("  CauseOfBlockage로 작업이 blocked 상태에 머무르다가,")
	fmt.Println("  차단 조건 해제 후 buildable로 전이되는 흐름")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	queue2 := NewQueue(executors, &ConsistentHashLoadBalancer{}, &PriorityQueueSorter{})

	// 차단 조건 미리 설정
	// 실제: Task.getCauseOfBlockage(), QueueTaskDispatcher.canRun() 등에서 차단
	queue2.SetBlockCondition("integration-test", &CauseOfBlockage{
		Reason: "Build is blocked by upstream project: my-app-build",
		Fatal:  false,
	})
	queue2.SetBlockCondition("release", &CauseOfBlockage{
		Reason: "Jenkins is about to shut down",
		Fatal:  true, // fatal → 취소됨
	})

	fmt.Println("\n[Step 1] 차단 조건이 있는 작업 스케줄링")
	queue2.Schedule("integration-test", 1, 0, nil)
	queue2.Schedule("code-scan", 2, 0, nil)
	queue2.Schedule("release", 3, 0, nil)

	fmt.Println("\n[Step 2] maintain() - quiet period 만료 → 차단/빌드가능 분류")
	queue2.Maintain()
	printQueueState(queue2)
	printExecutorState(executors)

	fmt.Println("[Step 3] 차단 조건 해제 후 maintain()")
	queue2.RemoveBlockCondition("integration-test")
	fmt.Println("  → 'integration-test' 차단 조건 제거됨")
	queue2.Maintain()
	printQueueState(queue2)
	printExecutorState(executors)

	// Executor 리셋
	for _, exec := range executors {
		exec.Busy = false
		exec.Current = nil
	}

	// =========================================================================
	// 시나리오 4: QueueSorter 우선순위 정렬
	// =========================================================================
	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("시나리오 4: QueueSorter 우선순위 정렬")
	fmt.Println("  QueueSorter.sortBuildableItems()로 우선순위 기반 실행 순서 결정")
	fmt.Println("  실제: Queue.java Phase 4 (sorter.sortBuildableItems(buildables))")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	queue3 := NewQueue(executors, &ConsistentHashLoadBalancer{}, &PriorityQueueSorter{})

	fmt.Println("\n[Step 1] 다양한 우선순위로 작업 5개 스케줄링")
	queue3.Schedule("low-priority-scan", 5, 0, nil)
	queue3.Schedule("critical-hotfix", 1, 0, nil)
	queue3.Schedule("nightly-build", 4, 0, nil)
	queue3.Schedule("pr-check", 2, 0, nil)
	queue3.Schedule("security-scan", 3, 0, nil)

	fmt.Println("\n[Step 2] maintain() - 우선순위 순으로 Executor 할당")
	fmt.Println("  (Executor 4개이므로 4개만 할당, 1개는 buildables에 남음)")
	queue3.Maintain()
	printQueueState(queue3)
	printExecutorState(executors)

	// Executor 리셋
	for _, exec := range executors {
		exec.Busy = false
		exec.Current = nil
	}

	// =========================================================================
	// 시나리오 5: 레이블 매칭 (Label-based LoadBalancer)
	// =========================================================================
	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("시나리오 5: 레이블 기반 Executor 매칭")
	fmt.Println("  작업이 요구하는 레이블과 노드 레이블을 매칭하여 Executor 할당")
	fmt.Println("  실제: MappingWorksheet에서 applicableExecutorChunks() 필터링")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	queue4 := NewQueue(executors, &ConsistentHashLoadBalancer{}, &PriorityQueueSorter{})

	fmt.Println("\n  Executor 레이블:")
	for _, exec := range executors {
		fmt.Printf("    %s: %v\n", exec, exec.Labels)
	}

	fmt.Println("\n[Step 1] 레이블 요구사항이 있는 작업 스케줄링")
	queue4.Schedule("docker-build", 1, 0, []string{"docker"})
	queue4.Schedule("dotnet-build", 1, 0, []string{"windows", "dotnet"})
	queue4.Schedule("java-build", 1, 0, []string{"java"})
	queue4.Schedule("gpu-train", 1, 0, []string{"gpu"}) // 매칭 불가

	fmt.Println("\n[Step 2] maintain() - 레이블 매칭으로 Executor 할당")
	queue4.Maintain()
	printQueueState(queue4)
	printExecutorState(executors)

	// Executor 리셋
	for _, exec := range executors {
		exec.Busy = false
		exec.Current = nil
	}

	// =========================================================================
	// 시나리오 6: 중복 스케줄링 & 취소
	// =========================================================================
	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("시나리오 6: 중복 스케줄링 감지 & 취소")
	fmt.Println("  실제: scheduleInternal()의 중복 검사 (QueueAction.shouldSchedule())")
	fmt.Println("  실제: cancel()의 비트 OR(|) 패턴 (short-circuit 방지)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	queue5 := NewQueue(executors, &ConsistentHashLoadBalancer{}, &PriorityQueueSorter{})

	fmt.Println("\n[Step 1] 동일 작업 중복 스케줄링 시도")
	r1 := queue5.Schedule("my-app", 1, 100*time.Millisecond, nil)
	fmt.Printf("  결과: %v (Type=%d, Created=0/Existing=1/Refused=2)\n", r1.Item, r1.Type)
	r2 := queue5.Schedule("my-app", 1, 50*time.Millisecond, nil) // 중복
	fmt.Printf("  결과: %v (Type=%d) → 기존 항목에 병합\n", r2.Item, r2.Type)

	fmt.Println("\n[Step 2] 작업 취소")
	queue5.Schedule("to-cancel", 2, 50*time.Millisecond, nil)
	printQueueState(queue5)
	cancelled := queue5.Cancel("to-cancel")
	fmt.Printf("  'to-cancel' 취소 결과: %v\n", cancelled)
	printQueueState(queue5)

	// Executor 리셋
	for _, exec := range executors {
		exec.Busy = false
		exec.Current = nil
	}

	// =========================================================================
	// 시나리오 7: Snapshot 패턴 - 잠금 없는 읽기
	// =========================================================================
	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("시나리오 7: Snapshot 패턴 - 잠금 없는 동시 읽기")
	fmt.Println("  실제: volatile Snapshot snapshot으로 잠금 없이 일관된 읽기")
	fmt.Println("  쓰기 스레드: lock 안에서 updateSnapshot() 호출")
	fmt.Println("  읽기 스레드: this.snapshot (volatile 읽기) → 일관된 조회")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	queue6 := NewQueue(executors, &ConsistentHashLoadBalancer{}, &PriorityQueueSorter{})

	fmt.Println("\n[Step 1] 작업 스케줄링 (쓰기)")
	for i := 1; i <= 5; i++ {
		queue6.Schedule(fmt.Sprintf("concurrent-job-%d", i), rand.Intn(5)+1, 0, nil)
	}

	fmt.Println("\n[Step 2] 여러 goroutine에서 동시에 스냅샷 읽기 (잠금 없음)")
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(readerID int) {
			defer wg.Done()
			snap := queue6.GetSnapshot()
			fmt.Printf("  [Reader %d] %s (total=%d items)\n",
				readerID, snap, snap.TotalItems())
		}(i + 1)
	}
	wg.Wait()

	fmt.Println("\n[Step 3] maintain() 실행 → 스냅샷 자동 갱신")
	queue6.Maintain()
	snap2 := queue6.GetSnapshot()
	fmt.Printf("  갱신된 스냅샷: %s\n", snap2)
	printQueueState(queue6)
	printExecutorState(executors)

	// Executor 리셋
	for _, exec := range executors {
		exec.Busy = false
		exec.Current = nil
	}

	// =========================================================================
	// 시나리오 8: 전체 라이프사이클 시뮬레이션
	// =========================================================================
	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("시나리오 8: 전체 라이프사이클 (maintain 루프 + 빌드 완료)")
	fmt.Println("  실제: MaintainTask (5초 간격 주기적 호출)")
	fmt.Println("  실제: AtmostOneTaskExecutor (동시 maintain 방지)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	queue7 := NewQueue(executors, &ConsistentHashLoadBalancer{}, &PriorityQueueSorter{})

	// 차단 조건 설정
	queue7.SetBlockCondition("deploy-prod", &CauseOfBlockage{
		Reason: "Waiting for approval",
		Fatal:  false,
	})

	fmt.Println("\n[Step 1] 작업 6개 스케줄링 (다양한 quiet period, 차단 조건)")
	queue7.Schedule("build-frontend", 1, 0, nil)
	queue7.Schedule("build-backend", 1, 50*time.Millisecond, nil)
	queue7.Schedule("run-e2e-tests", 2, 100*time.Millisecond, []string{"docker"})
	queue7.Schedule("deploy-staging", 3, 150*time.Millisecond, nil)
	queue7.Schedule("deploy-prod", 4, 0, nil)            // 차단됨
	queue7.Schedule("notify-slack", 5, 200*time.Millisecond, nil)

	printQueueState(queue7)

	// maintain 루프 시뮬레이션
	// 실제: MaintainTask가 5초 간격으로 maintain() 호출
	// 실제: AtmostOneTaskExecutor<Void>로 동시 실행 방지
	for round := 1; round <= 4; round++ {
		time.Sleep(80 * time.Millisecond)
		fmt.Printf("\n[maintain 라운드 %d] (%.0fms 경과)\n", round, float64(round)*80)
		queue7.Maintain()
		printQueueState(queue7)

		// pending 아이템의 실행 시작 시뮬레이션
		snap := queue7.GetSnapshot()
		for _, item := range snap.Pendings {
			queue7.StartExecuting(item)
		}

		// 빌드 완료 시뮬레이션: Executor 해제
		for _, exec := range executors {
			if exec.Busy {
				fmt.Printf("  [빌드 완료] %s @ %s 완료\n", exec.Current.TaskName, exec.NodeName)
				exec.Busy = false
				exec.Current = nil
			}
		}

		// 라운드 2에서 차단 해제
		if round == 2 {
			fmt.Println("\n  >>> 'deploy-prod' 승인 완료 → 차단 해제")
			queue7.RemoveBlockCondition("deploy-prod")
		}
	}

	fmt.Println("\n[최종 상태]")
	printQueueState(queue7)
	printExecutorState(executors)

	// =========================================================================
	// 요약
	// =========================================================================
	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("요약: Jenkins 빌드 큐 핵심 메커니즘")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()
	fmt.Println("  1. 4개 컬렉션 상태 머신:")
	fmt.Println("     waitingList(TreeSet) → blockedProjects(ItemList) / buildables(ItemList)")
	fmt.Println("     → pendings(ItemList) → leftItems(Cache, 5분 TTL)")
	fmt.Println()
	fmt.Println("  2. maintain() 5단계:")
	fmt.Println("     Phase 1: parked Executor 수집, lost pending 복구")
	fmt.Println("     Phase 2: blocked → buildable (차단 해제 확인)")
	fmt.Println("     Phase 3: waiting → buildable/blocked (quiet period 확인)")
	fmt.Println("     Phase 4: QueueSorter 적용 (우선순위 정렬)")
	fmt.Println("     Phase 5: buildables → Executor 할당 (LoadBalancer)")
	fmt.Println()
	fmt.Println("  3. 동시성 패턴:")
	fmt.Println("     - ReentrantLock + Condition (synchronized 대신)")
	fmt.Println("     - volatile Snapshot (잠금 없는 읽기)")
	fmt.Println("     - AtmostOneTaskExecutor (maintain 동시 실행 방지)")
	fmt.Println("     - try { try { } finally { updateSnapshot(); } } finally { lock.unlock(); }")
	fmt.Println()
	fmt.Println("  4. LoadBalancer:")
	fmt.Println("     - CONSISTENT_HASH: affinityKey 해싱으로 일관된 노드 할당")
	fmt.Println("     - MappingWorksheet: 작업-Executor 매핑 문제 정의")
	fmt.Println("     - assignGreedily(): 탐욕적 할당 알고리즘")
	fmt.Println()
	fmt.Println("  5. QueueSorter:")
	fmt.Println("     - sortBuildableItems(): 빌드 가능 항목 우선순위 정렬")
	fmt.Println("     - DEFAULT_BLOCKED_ITEM_COMPARATOR: inQueueSince 기준")
	fmt.Println()
	fmt.Println("  6. Snapshot 패턴:")
	fmt.Println("     - 4개 컬렉션의 방어적 복사 (LinkedHashSet, ArrayList)")
	fmt.Println("     - volatile 참조 → 모든 스레드에 즉시 가시")
	fmt.Println("     - getItems() 등 읽기 메서드는 잠금 불필요")
}
