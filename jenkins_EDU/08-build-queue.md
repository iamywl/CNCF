# 08. Jenkins 빌드 큐(Build Queue) 심화 분석

## 개요

Jenkins의 빌드 큐(`Queue`)는 사용자가 요청한 빌드 작업을 접수하고, 적절한 시점에 적절한 노드의 Executor에 할당하여 실행시키는 핵심 스케줄링 엔진이다. 이 문서는 `core/src/main/java/hudson/model/Queue.java` (약 3,252줄)를 중심으로, 큐의 상태 머신, 아이템 계층 구조, 핵심 메서드, 동시성 제어, 로드밸런싱, 영속화 메커니즘을 심층 분석한다.

**핵심 소스 파일:**

| 파일 | 경로 | 역할 |
|------|------|------|
| Queue.java | `core/src/main/java/hudson/model/Queue.java` | 빌드 큐 핵심 (3,252줄) |
| LoadBalancer.java | `core/src/main/java/hudson/model/LoadBalancer.java` | Executor 배치 전략 |
| MappingWorksheet.java | `core/src/main/java/hudson/model/queue/MappingWorksheet.java` | 작업-Executor 매핑 문제 정의 |
| WorkUnit.java | `core/src/main/java/hudson/model/queue/WorkUnit.java` | Executor 핸드오버 단위 |
| WorkUnitContext.java | `core/src/main/java/hudson/model/queue/WorkUnitContext.java` | WorkUnit 간 공유 컨텍스트 |
| QueueListener.java | `core/src/main/java/hudson/model/queue/QueueListener.java` | 큐 이벤트 리스너 |
| QueueSorter.java | `core/src/main/java/hudson/model/queue/QueueSorter.java` | 빌드 가능 항목 정렬 |
| QueueTaskDispatcher.java | `core/src/main/java/hudson/model/queue/QueueTaskDispatcher.java` | 실행 거부권(Veto) |
| ScheduleResult.java | `core/src/main/java/hudson/model/queue/ScheduleResult.java` | 스케줄링 결과 |
| FutureImpl.java | `core/src/main/java/hudson/model/queue/FutureImpl.java` | 비동기 완료 추적 |

---

## 1. 큐 상태 머신 (Queue State Machine)

### 1.1 상태 전이 다이어그램

Jenkins Queue의 아이템은 다섯 단계를 거쳐 처리된다. `Queue.java`의 클래스 Javadoc에 명시된 상태 전이 다이어그램은 다음과 같다:

```
                 +------------------+
                 |      enter       |
                 +--------+---------+
                          |
                          v
                 +------------------+
                 |   waitingList    |  TreeSet<WaitingItem>
                 |  (quietPeriod)   |  조용한 기간 대기
                 +--------+---------+
                          |
           +--------------+--------------+
           |                             |
           v                             v
  +------------------+          +------------------+
  | blockedProjects  |          |    buildables    |  ItemList<BuildableItem>
  | (차단 조건 존재)  |<-------->|  (빌드 가능 대기)  |  Executor 대기
  +------------------+          +--------+---------+
  ItemList<BlockedItem>                  |
                                         v
                                +------------------+
                                |     pendings     |  ItemList<BuildableItem>
                                | (Executor 할당됨) |  isPending = true
                                +--------+---------+
                                         |
                                         v
                                +------------------+
                                |    leftItems     |  Cache<Long, LeftItem>
                                |  (완료, 5분 TTL)  |  Guava Cache
                                +------------------+
```

**핵심 전이 규칙:**

- `waitingList -> blockedProjects`: 조용한 기간이 지났으나 차단 조건(`CauseOfBlockage`)이 존재
- `waitingList -> buildables`: 조용한 기간이 지났고 차단 조건이 없음
- `blockedProjects -> buildables`: 차단 조건이 해제됨
- `buildables -> blockedProjects`: 마지막 순간에 차단 조건이 재발견됨
- `buildables -> pendings`: `LoadBalancer`가 Executor 매핑에 성공하여 `Mapping.execute()` 호출
- `pendings -> leftItems`: `Executor`가 실제 실행을 시작 (`onStartExecuting()`)
- `pendings -> buildables`: 할당된 Executor가 사라진 경우 (드물게 발생)

### 1.2 각 상태 컬렉션의 구현

Queue.java의 193~224줄에서 정의된 다섯 컬렉션:

```java
// Queue.java 193줄
private final Set<WaitingItem> waitingList = new TreeSet<>();

// Queue.java 202줄
private final ItemList<BlockedItem> blockedProjects = new ItemList<>();

// Queue.java 209줄
private final ItemList<BuildableItem> buildables = new ItemList<>();

// Queue.java 215줄
private final ItemList<BuildableItem> pendings = new ItemList<>();

// Queue.java 224줄
private final Cache<Long, LeftItem> leftItems =
    CacheBuilder.newBuilder().expireAfterWrite(5 * 60, TimeUnit.SECONDS).build();
```

**왜 이런 자료구조를 선택했는가:**

| 컬렉션 | 타입 | 선택 이유 |
|--------|------|----------|
| `waitingList` | `TreeSet<WaitingItem>` | `WaitingItem`이 `Comparable`을 구현하여 `timestamp` 순서로 자동 정렬. `peek()`으로 가장 빠른 항목을 O(1)에 접근 가능 |
| `blockedProjects` | `ItemList<BlockedItem>` | `ArrayList` 확장. Task 기반 검색(`get(Task)`, `getAll(Task)`)과 일괄 취소(`cancelAll()`)를 지원 |
| `buildables` | `ItemList<BuildableItem>` | `QueueSorter`에 의한 순서 변경이 가능한 리스트 형태. LoadBalancer 매핑 시 순회 필요 |
| `pendings` | `ItemList<BuildableItem>` | buildables와 동일한 타입(`BuildableItem`)을 사용하되, `isPending = true`로 구분 |
| `leftItems` | `Cache<Long, LeftItem>` | Guava의 시간 기반 만료 캐시. 5분 TTL로 완료된 항목을 일시적으로 추적. ID로 조회 가능 |

### 1.3 ItemList 내부 클래스

```java
// Queue.java 3042~3083줄
private class ItemList<T extends Item> extends ArrayList<T> {
    public T get(Task task) {
        for (T item : this) {
            if (item.task.equals(task)) {
                return item;
            }
        }
        return null;
    }

    public List<T> getAll(Task task) {
        List<T> result = new ArrayList<>();
        for (T item : this) {
            if (item.task.equals(task)) {
                result.add(item);
            }
        }
        return result;
    }

    public boolean containsKey(Task task) {
        return get(task) != null;
    }

    public T cancel(Task p) {
        T x = get(p);
        if (x != null) x.cancel(Queue.this);
        return x;
    }

    public void cancelAll() {
        for (T t : new ArrayList<>(this))
            t.cancel(Queue.this);
        clear();
    }
}
```

`ItemList`는 `ArrayList`를 확장하면서 `Task` 기반의 조회/취소 메서드를 추가한 편의 클래스다. `get(Task)`는 O(N) 선형 탐색이지만, 큐에 동시에 존재하는 항목 수가 일반적으로 적기 때문에 실용적인 선택이다.

---

## 2. Queue.Item 계층 구조

### 2.1 클래스 계층도

```
                    Actionable
                        |
              Queue.Item (abstract)
              |    id: long
              |    task: Task
              |    future: FutureImpl
              |    inQueueSince: long
              |
    +---------+----------+------------------+
    |                    |                   |
WaitingItem       NotWaitingItem (abstract)  LeftItem
|  timestamp      |  buildableStartMillis    |  outcome: WorkUnitContext
|  Comparable     |                          |
                  +----------+
                  |          |
            BlockedItem   BuildableItem
            |  causeOfBlockage   |  isPending: boolean
                                 |  transientCausesOfBlockage
```

### 2.2 Item 기본 클래스

```java
// Queue.java 2260~2574줄
public abstract static class Item extends Actionable implements QueueItem {
    private final long id;
    @Exported @NonNull
    public final Task task;
    private /*almost final*/ transient FutureImpl future;
    private final long inQueueSince;

    // 생성자 (Queue.java 2413줄)
    protected Item(@NonNull Task task, @NonNull List<Action> actions,
                   long id, FutureImpl future) {
        this(task, actions, id, future, System.currentTimeMillis());
    }

    // 복사 생성자 (Queue.java 2426줄) - 상태 전이 시 사용
    protected Item(Item item) {
        this(item.task, new ArrayList<>(item.getActions()),
             item.id, item.future, item.inQueueSince);
    }

    // 추상 메서드 - 각 서브클래스가 구현
    /*package*/ abstract void enter(Queue q);    // 해당 큐에 진입
    /*package*/ abstract boolean leave(Queue q); // 해당 큐에서 이탈

    // 취소 (Queue.java 2564줄)
    /*package*/ boolean cancel(Queue q) {
        boolean r = leave(q);
        if (r) {
            future.setAsCancelled();
            LeftItem li = new LeftItem(this);
            li.enter(q);
        }
        return r;
    }
}
```

**핵심 필드 설명:**

| 필드 | 타입 | 설명 |
|------|------|------|
| `id` | `long` | 전체 Jenkins 인스턴스에서 고유한 ID. `QueueIdStrategy.get().generateIdFor()`로 생성. Task가 상태를 이동해도 동일한 ID 유지 |
| `task` | `Task` | 빌드할 프로젝트. `equals()`로 중복 검사에 사용 |
| `future` | `FutureImpl` | 호출자가 비동기적으로 완료를 추적. `start`(실행 시작)과 자체(실행 완료) 두 단계 추적 |
| `inQueueSince` | `long` | 큐에 진입한 Unix 타임스탬프 (밀리초) |

**`enter()`/`leave()` 패턴:**

모든 상태 전이는 이전 컬렉션에서 `leave()` 후 새 컬렉션으로 `enter()`하는 방식으로 이루어진다. 각 `enter()`/`leave()` 메서드는 `QueueListener`에 이벤트를 통보한다.

### 2.3 WaitingItem

```java
// Queue.java 2662~2708줄
public static final class WaitingItem extends Item implements Comparable<WaitingItem> {
    @Exported
    public Calendar timestamp;  // 이 시각 이후에 실행 가능

    public WaitingItem(Calendar timestamp, Task project, List<Action> actions) {
        super(project, actions,
              QueueIdStrategy.get().generateIdFor(project, actions),
              new FutureImpl(project));
        this.timestamp = timestamp;
    }

    @Override
    public int compareTo(WaitingItem that) {
        int r = this.timestamp.getTime().compareTo(that.timestamp.getTime());
        if (r != 0) return r;
        return Long.compare(this.getId(), that.getId());
    }

    @Override
    public CauseOfBlockage getCauseOfBlockage() {
        long diff = timestamp.getTimeInMillis() - System.currentTimeMillis();
        if (diff >= 0)
            return CauseOfBlockage.fromMessage(
                Messages._Queue_InQuietPeriod(Util.getTimeSpanString(diff)));
        else
            return CauseOfBlockage.fromMessage(Messages._Queue_FinishedWaiting());
    }

    @Override
    void enter(Queue q) {
        if (q.waitingList.add(this)) {
            Listeners.notify(QueueListener.class, true,
                l -> l.onEnterWaiting(this));
        }
    }

    @Override
    boolean leave(Queue q) {
        boolean r = q.waitingList.remove(this);
        if (r) {
            Listeners.notify(QueueListener.class, true,
                l -> l.onLeaveWaiting(this));
        }
        return r;
    }
}
```

**왜 `Comparable`을 구현하는가:** `waitingList`가 `TreeSet`이므로 `timestamp` 기준 정렬이 필수다. 같은 시각인 경우 `id`로 2차 정렬하여 안정적인 순서를 보장한다. 이렇게 하면 `maintain()` 메서드에서 `peek()`으로 가장 이른 항목을 O(1)에 꺼낼 수 있다.

### 2.4 NotWaitingItem (추상)

```java
// Queue.java 2713~2729줄
public abstract static class NotWaitingItem extends Item {
    @Exported
    public final long buildableStartMilliseconds;

    protected NotWaitingItem(WaitingItem wi) {
        super(wi);
        buildableStartMilliseconds = System.currentTimeMillis();
    }

    protected NotWaitingItem(NotWaitingItem ni) {
        super(ni);
        buildableStartMilliseconds = ni.buildableStartMilliseconds;
    }
}
```

`NotWaitingItem`은 `BlockedItem`과 `BuildableItem`의 공통 부모다. `buildableStartMilliseconds`는 대기(waiting) 단계를 벗어난 시점을 기록한다. 이 값은 `BuildableItem.isStuck()` 메서드에서 "작업이 얼마나 오래 빌드 가능 상태에 머물렀는가"를 계산하는 데 사용된다.

### 2.5 BlockedItem

```java
// Queue.java 2734~2782줄
public final class BlockedItem extends NotWaitingItem {
    private final transient CauseOfBlockage causeOfBlockage;

    BlockedItem(WaitingItem wi, CauseOfBlockage causeOfBlockage) {
        super(wi);
        this.causeOfBlockage = causeOfBlockage;
    }

    BlockedItem(NotWaitingItem ni, CauseOfBlockage causeOfBlockage) {
        super(ni);
        this.causeOfBlockage = causeOfBlockage;
    }

    @Override
    public CauseOfBlockage getCauseOfBlockage() {
        if (causeOfBlockage != null) {
            return causeOfBlockage;
        }
        // 하위 호환성을 위한 폴백
        return getCauseOfBlockageForItem(this);
    }

    @Override
    void enter(Queue q) {
        LOGGER.log(Level.FINE, "{0} is blocked", this);
        blockedProjects.add(this);
        Listeners.notify(QueueListener.class, true,
            l -> l.onEnterBlocked(this));
    }

    @Override
    boolean leave(Queue q) {
        boolean r = blockedProjects.remove(this);
        if (r) {
            LOGGER.log(Level.FINE, "{0} no longer blocked", this);
            Listeners.notify(QueueListener.class, true,
                l -> l.onLeaveBlocked(this));
        }
        return r;
    }
}
```

**`BlockedItem`이 내부(inner) 클래스인 이유:** `blockedProjects` 필드에 직접 접근해야 하므로 `Queue`의 inner class로 정의되었다. 반면 `WaitingItem`은 `static`이다 -- `waitingList`에 대한 접근은 `enter(Queue q)` 파라미터를 통해 이루어진다.

**차단 원인 (`CauseOfBlockage`) 소스:**
1. `Task.getCauseOfBlockage()` -- 태스크 자체가 차단 이유를 보고
2. `ResourceActivity` -- 필요한 리소스가 다른 빌드에 의해 점유됨
3. `QueueTaskDispatcher.canRun(Item)` -- 플러그인이 거부권 행사
4. 동시 빌드 불허 시 이미 buildables/pendings에 동일 Task 존재

### 2.6 BuildableItem

```java
// Queue.java 2787~2877줄
public static final class BuildableItem extends NotWaitingItem {
    private boolean isPending;
    private transient volatile @CheckForNull List<CauseOfBlockage>
        transientCausesOfBlockage;

    @Override
    public CauseOfBlockage getCauseOfBlockage() {
        Jenkins jenkins = Jenkins.get();
        if (isBlockedByShutdown(task))
            return CauseOfBlockage.fromMessage(
                Messages._Queue_HudsonIsAboutToShutDown());

        List<CauseOfBlockage> causesOfBlockage = transientCausesOfBlockage;
        Label label = getAssignedLabel();
        // ... 레이블 기반 차단 원인 결정
    }

    @Override
    public boolean isStuck() {
        Label label = getAssignedLabel();
        if (label != null && label.isOffline())
            return true;  // 실행 가능한 노드가 없음

        long d = task.getEstimatedDuration();
        long elapsed = System.currentTimeMillis() - buildableStartMilliseconds;
        if (d >= 0) {
            // 다른 곳에서 실행했다면 10회 빌드했을 시간
            return elapsed > Math.max(d, 60000L) * 10;
        } else {
            // 24시간 이상 대기
            return TimeUnit.MILLISECONDS.toHours(elapsed) > 24;
        }
    }

    @Override
    void enter(Queue q) {
        q.buildables.add(this);
        Listeners.notify(QueueListener.class, true,
            l -> l.onEnterBuildable(this));
    }

    @Override
    boolean leave(Queue q) {
        boolean r = q.buildables.remove(this);
        if (r) {
            Listeners.notify(QueueListener.class, true,
                l -> l.onLeaveBuildable(this));
        }
        return r;
    }
}
```

**`isStuck()` 판단 로직:**

```
stuck 판단 기준:
  1. 할당된 레이블의 모든 노드가 오프라인인 경우 → 즉시 stuck
  2. 예상 빌드 시간(d) >= 0:
     → 대기 시간 > max(d, 60초) * 10 이면 stuck
  3. 예상 빌드 시간을 알 수 없는 경우:
     → 24시간 이상 대기하면 stuck
```

**`transientCausesOfBlockage`:**
`maintain()` 루프에서 `LoadBalancer.map()`이 null을 반환할 때(매핑 실패), 그 원인 목록을 여기에 기록한다. UI에서 "왜 빌드가 시작되지 않는지" 표시할 때 참조된다.

### 2.7 LeftItem

```java
// Queue.java 2885~2937줄
public static final class LeftItem extends Item {
    public final WorkUnitContext outcome;

    // 실행이 시작된 경우
    public LeftItem(WorkUnitContext wuc) {
        super(wuc.item);
        this.outcome = wuc;
    }

    // 취소된 경우
    public LeftItem(Item cancelled) {
        super(cancelled);
        this.outcome = null;  // null이면 취소를 의미
    }

    @Exported
    public @CheckForNull Executable getExecutable() {
        return outcome != null ? outcome.getPrimaryWorkUnit().getExecutable() : null;
    }

    @Exported
    public boolean isCancelled() {
        return outcome == null;
    }

    @Override
    void enter(Queue q) {
        q.leftItems.put(getId(), this);
        Listeners.notify(QueueListener.class, true, l -> l.onLeft(this));
    }

    @Override
    boolean leave(Queue q) {
        return false;  // LeftItem은 leave 불가 (Cache TTL에 의해 자동 만료)
    }
}
```

**`LeftItem`의 두 가지 생성 경로:**

1. **실행 시작**: `onStartExecuting(Executor)` -> `new LeftItem(WorkUnitContext)` -- `outcome != null`
2. **취소**: `Item.cancel(Queue)` -> `new LeftItem(this)` -- `outcome == null`

`leftItems`는 Guava `Cache`로 5분 후 자동 만료된다. 이 기간 동안 `getItem(long id)`로 조회 가능하여, API 소비자가 큐를 떠난 항목의 상태를 확인할 수 있다.

---

## 3. 핵심 메서드 분석

### 3.1 schedule2() -- 작업 스케줄링

작업을 큐에 등록하는 공개 진입점이다:

```java
// Queue.java 581~596줄
public @NonNull ScheduleResult schedule2(Task p, int quietPeriod,
                                          List<Action> actions) {
    actions = new ArrayList<>(actions);
    actions.removeIf(Objects::isNull);  // null 액션 제거

    lock.lock();
    try { try {
        for (QueueDecisionHandler h : QueueDecisionHandler.all())
            if (!h.shouldSchedule(p, actions))
                return ScheduleResult.refused();    // 거부권

        return scheduleInternal(p, quietPeriod, actions);
    } finally { updateSnapshot(); } } finally {
        lock.unlock();
    }
}
```

**중첩 try-finally 패턴:** Jenkins Queue는 특이한 `try { try { ... } finally { updateSnapshot(); } } finally { lock.unlock(); }` 패턴을 사용한다. 이는 잠금 해제 전에 반드시 스냅샷을 갱신하여, 외부 스레드가 읽는 스냅샷이 항상 최신 상태를 반영하도록 보장한다.

### 3.2 scheduleInternal() -- 중복 검사 및 실제 등록

```java
// Queue.java 611~676줄
private @NonNull ScheduleResult scheduleInternal(Task p, int quietPeriod,
                                                  List<Action> actions) {
    lock.lock();
    try { try {
        Calendar due = new GregorianCalendar();
        due.add(Calendar.SECOND, quietPeriod);

        // 1. 큐에 이미 동일한 Task가 있는지 확인
        List<Item> duplicatesInQueue = new ArrayList<>();
        for (Item item : liveGetItems(p)) {
            boolean shouldScheduleItem = false;
            for (QueueAction action : item.getActions(QueueAction.class)) {
                shouldScheduleItem |= action.shouldSchedule(actions);
            }
            for (QueueAction action : Util.filter(actions, QueueAction.class)) {
                shouldScheduleItem |= action.shouldSchedule(
                    new ArrayList<>(item.getAllActions()));
            }
            if (!shouldScheduleItem) {
                duplicatesInQueue.add(item);
            }
        }

        if (duplicatesInQueue.isEmpty()) {
            // 2. 중복 없음 → 새 WaitingItem 생성
            WaitingItem added = new WaitingItem(due, p, actions);
            added.enter(this);
            scheduleMaintenance();
            return ScheduleResult.created(added);
        }

        // 3. 중복 존재 → FoldableAction으로 기존 항목에 정보 병합
        for (Item item : duplicatesInQueue) {
            for (FoldableAction a : Util.filter(actions, FoldableAction.class)) {
                a.foldIntoExisting(item, p, actions);
            }
        }

        // 4. 기존 WaitingItem의 timestamp를 더 이른 시각으로 갱신
        boolean queueUpdated = false;
        for (WaitingItem wi : Util.filter(duplicatesInQueue, WaitingItem.class)) {
            if (wi.timestamp.before(due))
                continue;  // 이미 더 이른 시각이면 유지
            wi.leave(this);
            wi.timestamp = due;
            wi.enter(this);  // TreeSet 재정렬을 위해 재삽입
            queueUpdated = true;
        }

        if (queueUpdated) scheduleMaintenance();
        return ScheduleResult.existing(duplicatesInQueue.getFirst());
    } finally { updateSnapshot(); } } finally {
        lock.unlock();
    }
}
```

**스케줄링 흐름 요약:**

```
schedule2(Task, quietPeriod, actions)
    |
    v
QueueDecisionHandler.shouldSchedule() 거부 확인
    |
    v
scheduleInternal()
    |
    +-- 중복 검사 (QueueAction.shouldSchedule())
    |
    +-- [중복 없음] → WaitingItem 생성 → waitingList에 enter
    |
    +-- [중복 있음] → FoldableAction.foldIntoExisting()
    |                → WaitingItem.timestamp 갱신 (더 이른 시각으로)
    |
    v
scheduleMaintenance() → maintain() 트리거
```

**`ScheduleResult` 반환 타입:**

| 타입 | 설명 |
|------|------|
| `ScheduleResult.Created` | 새 WaitingItem이 생성됨. `getCreateItem()`으로 접근 |
| `ScheduleResult.Existing` | 이미 큐에 동일 Task 존재. 기존 Item 반환 |
| `ScheduleResult.Refused` | `QueueDecisionHandler`가 거부. `isRefused() == true` |

### 3.3 maintain() -- 주기적 유지보수

`maintain()`은 Queue의 심장이다. 모든 상태 전이 로직이 이 메서드에서 실행된다:

```java
// Queue.java 1593~1829줄
public void maintain() {
    Jenkins jenkins = Jenkins.getInstanceOrNull();
    if (jenkins == null) return;

    lock.lock();
    try { try {
        // === Phase 1: parked Executor 수집 & lost pendings 처리 ===
        Map<Executor, JobOffer> parked = new HashMap<>();
        List<BuildableItem> lostPendings = new ArrayList<>(pendings);

        for (Computer c : jenkins.getComputers()) {
            for (Executor e : c.getAllExecutors()) {
                if (e.isInterrupted()) {
                    lostPendings.clear(); // 인터럽트된 Executor가 있으면 안전하게 건너뜀
                    continue;
                }
                if (e.isParking()) {
                    parked.put(e, new JobOffer(e));  // 유휴 Executor → JobOffer 생성
                }
                final WorkUnit workUnit = e.getCurrentWorkUnit();
                if (workUnit != null) {
                    lostPendings.remove(workUnit.context.item);
                }
            }
        }
        // pending -> buildable (Executor 소실)
        for (BuildableItem p : lostPendings) {
            p.isPending = false;
            pendings.remove(p);
            var r = makeBuildable(p);
            if (r != null) r.run();
        }

        // === Phase 2: blocked -> buildable ===
        List<BlockedItem> blockedItems = new ArrayList<>(blockedProjects);
        if (sorter != null) {
            sorter.sortBlockedItems(blockedItems);
        } else {
            blockedItems.sort(QueueSorter.DEFAULT_BLOCKED_ITEM_COMPARATOR);
        }
        for (BlockedItem p : blockedItems) {
            CauseOfBlockage causeOfBlockage = getCauseOfBlockageForItem(p);
            if (causeOfBlockage == null) {
                Runnable r = makeBuildable(new BuildableItem(p));
                if (r != null) {
                    p.leave(this);
                    r.run();
                    updateSnapshot();  // JENKINS-28926
                }
            } else {
                if (causeOfBlockage.isFatal()) {
                    cancel(p);
                } else {
                    p.leave(this);
                    new BlockedItem(p, causeOfBlockage).enter(this);
                    updateSnapshot();
                }
            }
        }

        // === Phase 3: waitingList -> buildable/blocked ===
        while (!waitingList.isEmpty()) {
            WaitingItem top = peek();
            if (top.timestamp.compareTo(new GregorianCalendar()) > 0)
                break;  // 아직 시간이 안 된 항목 → 종료

            CauseOfBlockage causeOfBlockage = getCauseOfBlockageForItem(top);
            if (causeOfBlockage == null) {
                top.leave(this);
                Runnable r = makeBuildable(new BuildableItem(top));
                if (r != null) {
                    r.run();
                } else {
                    new BlockedItem(top, CauseOfBlockage.fromMessage(
                        Messages._Queue_HudsonIsAboutToShutDown())).enter(this);
                }
            } else {
                if (causeOfBlockage.isFatal()) {
                    cancel(top);
                } else {
                    top.leave(this);
                    new BlockedItem(top, causeOfBlockage).enter(this);
                }
            }
        }

        // === Phase 4: QueueSorter 적용 ===
        if (sorter != null) {
            sorter.sortBuildableItems(buildables);
        }
        updateSnapshot();

        // === Phase 5: buildables -> Executor 할당 ===
        for (BuildableItem p : new ArrayList<>(buildables)) {
            // 마지막 차단 확인
            CauseOfBlockage causeOfBlockage = getCauseOfBlockageForItem(p);
            if (causeOfBlockage != null) {
                // buildable -> blocked 전환
                p.leave(this);
                new BlockedItem(p, causeOfBlockage).enter(this);
                updateSnapshot();
                continue;
            }

            if (p.task instanceof FlyweightTask) {
                // FlyweightTask: OneOffExecutor로 직접 실행
                Runnable r = makeFlyWeightTaskBuildable(new BuildableItem(p));
                if (r != null) {
                    p.leave(this);
                    r.run();
                    updateSnapshot();
                }
            } else {
                // 일반 태스크: LoadBalancer로 Executor 매핑
                List<JobOffer> candidates = new ArrayList<>();
                Map<Node, CauseOfBlockage> reasonMap = new HashMap<>();
                for (JobOffer j : parked.values()) {
                    CauseOfBlockage reason = j.getCauseOfBlockage(p);
                    if (reason == null) {
                        candidates.add(j);
                    }
                    reasonMap.put(j.getNode(), reason);
                }

                MappingWorksheet ws = new MappingWorksheet(p, candidates);
                Mapping m = loadBalancer.map(p.task, ws);
                if (m == null) {
                    // 매핑 실패 → buildables에 유지
                    p.transientCausesOfBlockage = reasonMap.values()
                        .stream().filter(Objects::nonNull)
                        .collect(Collectors.toList());
                    continue;
                }

                // 매핑 성공 → 실행
                WorkUnitContext wuc = new WorkUnitContext(p);
                m.execute(wuc);
                p.leave(this);
                if (!wuc.getWorkUnits().isEmpty()) {
                    makePending(p);  // buildable -> pending
                }
                updateSnapshot();
            }
        }
    } finally { updateSnapshot(); } } finally {
        lock.unlock();
    }
}
```

**maintain() 5단계 요약:**

```
maintain()
  |
  |-- Phase 1: parked Executor 수집, lost pending 복구
  |     - 유휴(parking) Executor에서 JobOffer 생성
  |     - 할당된 Executor가 사라진 pending 항목 → buildable로 복귀
  |
  |-- Phase 2: blocked -> buildable 전이
  |     - 차단 조건이 해제된 항목을 buildable로 이동
  |     - isFatal() 차단 → 취소
  |     - JENKINS-28926: 각 전이 후 스냅샷 갱신
  |
  |-- Phase 3: waitingList -> buildable/blocked 전이
  |     - timestamp가 현재 시각을 지난 WaitingItem 처리
  |     - 차단 없음 → buildable, 차단 있음 → blocked
  |
  |-- Phase 4: QueueSorter로 buildables 정렬
  |     - 우선순위에 따라 실행 순서 결정
  |
  |-- Phase 5: buildables -> Executor 할당
  |     - FlyweightTask: 직접 실행 (OneOffExecutor)
  |     - 일반 태스크: LoadBalancer.map() → Mapping.execute()
  |     - 매핑 성공 → pending, 매핑 실패 → buildables 유지
```

### 3.4 maintain() 트리거

`maintain()`은 두 가지 방식으로 호출된다:

```java
// Queue.java 336~347줄 - AtmostOneTaskExecutor
private final transient AtmostOneTaskExecutor<Void> maintainerThread =
    new AtmostOneTaskExecutor<>(new Callable<>() {
        @Override
        public Void call() throws Exception {
            maintain();
            return null;
        }
    });

// Queue.java 1197~1200줄 - scheduleMaintenance()
public Future<?> scheduleMaintenance() {
    return maintainerThread.submit();
}

// Queue.java 3017~3037줄 - MaintainTask (주기적)
private static class MaintainTask extends SafeTimerTask {
    private final WeakReference<Queue> queue;

    private void periodic() {
        long interval = 5000;  // 5초 간격
        Timer.get().scheduleWithFixedDelay(this, interval, interval,
            TimeUnit.MILLISECONDS);
    }

    @Override
    protected void doRun() {
        Queue q = queue.get();
        if (q != null) q.maintain();
        else cancel();  // Queue가 GC되면 자동 해제
    }
}
```

**`AtmostOneTaskExecutor`의 역할:** 여러 곳에서 동시에 `scheduleMaintenance()`를 호출해도, 실제 `maintain()` 실행은 최대 하나만 진행된다. 이전 실행이 끝나기 전에 새로운 요청이 들어오면 다음 실행으로 병합(coalesce)된다.

**`MaintainTask`:** 5초 간격으로 주기적으로 `maintain()`을 호출하는 안전장치다. `WeakReference`로 Queue를 참조하여, Queue 객체가 GC되면 타이머도 자동 취소된다.

### 3.5 cancel() -- 작업 취소

```java
// Queue.java 721~735줄
public boolean cancel(Task p) {
    lock.lock();
    try { try {
        LOGGER.log(Level.FINE, "Cancelling {0}", p);
        for (WaitingItem item : waitingList) {
            if (item.task.equals(p)) {
                return item.cancel(this);
            }
        }
        // 비트 OR(|)을 사용하여 양쪽 모두 평가
        return blockedProjects.cancel(p) != null | buildables.cancel(p) != null;
    } finally { updateSnapshot(); } } finally {
        lock.unlock();
    }
}

// Queue.java 745~753줄 - Item 기반 취소
public boolean cancel(Item item) {
    LOGGER.log(Level.FINE, "Cancelling {0} item#{1}",
        new Object[] {item.task, item.id});
    lock.lock();
    try { try {
        return item.cancel(this);
    } finally { updateSnapshot(); } } finally {
        lock.unlock();
    }
}
```

**비트 OR(`|`) 사용 이유:** `cancel(Task p)`에서 `blockedProjects.cancel(p) != null | buildables.cancel(p) != null`은 논리 OR(`||`)이 아닌 비트 OR(`|`)을 사용한다. 이는 short-circuit 평가를 방지하여, 첫 번째 조건이 true여도 두 번째 취소를 반드시 실행하도록 보장한다. 동일 Task가 blockedProjects와 buildables 양쪽에 존재할 수 있는 엣지 케이스를 처리하기 위함이다.

### 3.6 onStartExecuting() -- 실행 시작

```java
// Queue.java 1175~1186줄
/*package*/ void onStartExecuting(Executor exec) throws InterruptedException {
    lock.lock();
    try { try {
        final WorkUnit wu = exec.getCurrentWorkUnit();
        pendings.remove(wu.context.item);

        LeftItem li = new LeftItem(wu.context);
        li.enter(this);
    } finally { updateSnapshot(); } } finally {
        lock.unlock();
    }
}
```

`Executor` 스레드가 실제로 `Executable.run()`을 호출하기 직전에 이 메서드가 호출된다. `pendings`에서 제거하고 `leftItems`에 등록하는 최종 전이를 수행한다.

---

## 4. 동시성 제어

### 4.1 ReentrantLock + Condition

```java
// Queue.java 349~351줄
private final transient ReentrantLock lock = new ReentrantLock();
private final transient Condition condition = lock.newCondition();
```

Jenkins Queue는 `synchronized` 대신 `ReentrantLock`을 사용한다:

**왜 `ReentrantLock`인가:**

| 특성 | synchronized | ReentrantLock |
|------|-------------|---------------|
| 타임아웃 | 불가 | `tryLock(timeout)` 지원 |
| 인터럽트 | 불가 | `lockInterruptibly()` 지원 |
| Condition | `wait()`/`notify()` 하나 | 여러 Condition 생성 가능 |
| 재진입 | 가능 | 가능 |
| 공정성 | 보장 안 됨 | 선택 가능 (현재 사용 안 함) |

### 4.2 잠금 래퍼 메서드

외부에서 Queue 잠금이 필요한 경우를 위한 정적 편의 메서드:

```java
// Queue.java 1277~1286줄
public static <T extends Throwable> void runWithLock(
        ThrowingRunnable<T> runnable) throws T {
    final Jenkins jenkins = Jenkins.getInstanceOrNull();
    final Queue queue = jenkins == null ? null : jenkins.getQueue();
    if (queue == null) {
        runnable.run();
    } else {
        queue._runWithLock(runnable);
    }
}

// Queue.java 1362~1372줄 - 비차단 잠금 시도
public static boolean tryWithLock(Runnable runnable) {
    final Queue queue = ...;
    return queue._tryWithLock(runnable);
}

// Queue.java 1381~1391줄 - 타임아웃 지정 잠금 시도
public static boolean tryWithLock(Runnable runnable, Duration timeout)
        throws InterruptedException {
    final Queue queue = ...;
    return queue._tryWithLock(runnable, timeout);
}
```

**`tryWithLock()`의 존재 이유:** UI 스레드 등에서 Queue 잠금을 기다리다가 응답이 늦어지는 것을 방지한다. 잠금을 즉시 획득할 수 없으면 `false`를 반환하여, 호출자가 대안 경로를 선택할 수 있다.

### 4.3 Snapshot -- 불변 스냅샷

```java
// Queue.java 3085~3103줄
private static class Snapshot {
    private final Set<WaitingItem> waitingList;
    private final List<BlockedItem> blockedProjects;
    private final List<BuildableItem> buildables;
    private final List<BuildableItem> pendings;

    Snapshot(Set<WaitingItem> waitingList, List<BlockedItem> blockedProjects,
             List<BuildableItem> buildables, List<BuildableItem> pendings) {
        this.waitingList = new LinkedHashSet<>(waitingList);  // 방어적 복사
        this.blockedProjects = new ArrayList<>(blockedProjects);
        this.buildables = new ArrayList<>(buildables);
        this.pendings = new ArrayList<>(pendings);
    }
}

// Queue.java 217줄
private transient volatile Snapshot snapshot = new Snapshot(...);

// Queue.java 737~743줄
private void updateSnapshot() {
    Snapshot revised = new Snapshot(waitingList, blockedProjects,
                                    buildables, pendings);
    snapshot = revised;  // volatile 쓰기 → 모든 스레드에 즉시 가시
}
```

**Snapshot 패턴이 해결하는 문제:**

Queue의 내부 상태는 `lock`으로 보호되지만, 읽기 전용 조회(`getItems()`, `getItem(long id)` 등)는 잠금 없이도 안전하게 수행되어야 한다. `volatile Snapshot` 참조를 통해:

1. 쓰기 스레드: `lock` 안에서 상태 변경 후 `updateSnapshot()` 호출
2. 읽기 스레드: `this.snapshot` 읽기 → 원자적 참조 읽기 → 일관된 스냅샷 조회

```java
// Queue.java 793~812줄 - 잠금 없이 안전하게 읽기
@Exported(inline = true)
public Item[] getItems() {
    Snapshot s = this.snapshot;  // volatile 읽기: 시점 고정
    List<Item> r = new ArrayList<>();
    for (WaitingItem p : s.waitingList) { ... }
    for (BlockedItem p : s.blockedProjects) { ... }
    for (BuildableItem p : reverse(s.buildables)) { ... }
    for (BuildableItem p : reverse(s.pendings)) { ... }
    return r.toArray(new Item[0]);
}
```

---

## 5. LoadBalancer

### 5.1 역할과 인터페이스

```java
// LoadBalancer.java 54~77줄
public abstract class LoadBalancer implements ExtensionPoint {
    @CheckForNull
    public abstract Mapping map(@NonNull Task task, MappingWorksheet worksheet);
}
```

`LoadBalancer`는 `buildables`에 있는 항목을 어떤 노드의 어떤 Executor에서 실행할지 결정하는 전략 패턴이다. `map()` 메서드가 null을 반환하면 "지금은 실행할 수 없음"을 의미하고, `Mapping` 객체를 반환하면 "이 매핑대로 실행하라"를 의미한다.

### 5.2 기본 구현: CONSISTENT_HASH

```java
// LoadBalancer.java 82~140줄
public static final LoadBalancer CONSISTENT_HASH = new LoadBalancer() {
    @Override
    public Mapping map(@NonNull Task task, MappingWorksheet ws) {
        // 각 WorkChunk에 대해 ConsistentHash 구성
        List<ConsistentHash<ExecutorChunk>> hashes = new ArrayList<>(ws.works.size());
        for (int i = 0; i < ws.works.size(); i++) {
            ConsistentHash<ExecutorChunk> hash =
                new ConsistentHash<>(ExecutorChunk::getName);

            List<ExecutorChunk> chunks = ws.works(i).applicableExecutorChunks();
            Map<ExecutorChunk, Integer> toAdd =
                Maps.newHashMapWithExpectedSize(chunks.size());
            for (ExecutorChunk ec : chunks) {
                toAdd.put(ec, ec.size() * 100);  // 가중치 = Executor 수 * 100
            }
            hash.addAll(toAdd);
            hashes.add(hash);
        }

        // 탐욕적 할당 (Greedy Assignment)
        Mapping m = ws.new Mapping();
        if (assignGreedily(m, task, hashes, 0)) {
            return m;
        }
        return null;
    }

    private boolean assignGreedily(Mapping m, Task task,
            List<ConsistentHash<ExecutorChunk>> hashes, int i) {
        if (i == hashes.size()) return true;  // 모든 WorkChunk 할당 완료

        String key = task.getAffinityKey();
        key += i > 0 ? String.valueOf(i) : "";

        for (ExecutorChunk ec : hashes.get(i).list(key)) {
            m.assign(i, ec);
            if (m.isPartiallyValid() && assignGreedily(m, task, hashes, i + 1))
                return true;
            // 실패 → 다음 후보 시도
        }

        m.assign(i, null);
        return false;
    }
};
```

**ConsistentHash를 사용하는 이유:**

1. **노드 친화성(Affinity):** `task.getAffinityKey()`를 해시 키로 사용하여, 동일한 작업이 이전과 같은 노드에 할당될 확률을 높인다. 이는 workspace 캐시 활용, 빌드 성능 향상에 기여한다.
2. **가중치:** 각 노드의 Executor 수에 비례하는 가중치(`ec.size() * 100`)를 부여하여, Executor가 많은 노드에 더 많은 작업이 분배된다.
3. **탐욕적 할당:** 재귀적으로 각 WorkChunk에 대해 ConsistentHash 순서대로 할당을 시도한다. 부분 유효성(`isPartiallyValid()`)을 체크하면서 백트래킹한다.

### 5.3 sanitize() 래퍼

```java
// LoadBalancer.java 156~178줄
protected LoadBalancer sanitize() {
    final LoadBalancer base = this;
    return new LoadBalancer() {
        @Override
        public Mapping map(@NonNull Task task, MappingWorksheet worksheet) {
            if (Queue.isBlockedByShutdown(task)) {
                return null;  // 종료 중이면 새 작업 할당 안 함
            }
            return base.map(task, worksheet);
        }

        @Override
        protected LoadBalancer sanitize() {
            return this;  // 이중 래핑 방지
        }
    };
}
```

Queue 생성자에서 `this.loadBalancer = loadBalancer.sanitize()`로 호출된다 (Queue.java 354줄). Jenkins가 종료 중(`isQuietingDown()`)일 때 새로운 작업이 시작되지 않도록 보장하는 안전장치다.

---

## 6. MappingWorksheet -- 매핑 문제 정의

### 6.1 개념

```
MappingWorksheet
  |
  |-- executors: List<ExecutorChunk>
  |     = 같은 노드의 유휴 Executor를 그룹화
  |
  |-- works: List<WorkChunk>
  |     = 같은 노드에서 실행되어야 하는 SubTask를 그룹화
  |
  |-- item: BuildableItem
  |     = 매핑 대상 아이템
```

`MappingWorksheet`는 "어디서 이 작업을 실행할 것인가?"라는 배치 문제를 형식화한 것이다:

- **WorkChunk:** 동일 노드 제약(`getSameNodeConstraint()`)을 공유하는 `SubTask`들의 그룹
- **ExecutorChunk:** 동일 `Computer`(노드)의 유휴 `Executor`들의 그룹
- **목표:** 모든 WorkChunk를 적절한 ExecutorChunk에 매핑

### 6.2 ExecutorChunk

```java
// MappingWorksheet.java 121~180줄
public final class ExecutorChunk extends ReadOnlyList<ExecutorSlot>
        implements Named {
    public final int index;
    public final Computer computer;
    public final Node node;
    public final ACL nodeAcl;

    public boolean canAccept(WorkChunk c) {
        if (this.size() < c.size())
            return false;   // 용량 부족
        if (c.assignedLabel != null && !c.assignedLabel.contains(node))
            return false;   // 레이블 불일치
        if (!nodeAcl.hasPermission2(item.authenticate2(), Computer.BUILD))
            return false;   // 권한 부족
        return true;
    }

    private void execute(WorkChunk wc, WorkUnitContext wuc) {
        assert capacity() >= wc.size();
        int e = 0;
        for (SubTask s : wc) {
            while (!get(e).isAvailable()) e++;
            get(e++).set(wuc.createWorkUnit(s));
            // ExecutorSlot.set() → Executor.start(workUnit)
        }
    }
}
```

`execute()` 메서드는 매핑이 확정된 후 실제로 WorkUnit을 Executor에 할당하는 핵심 로직이다. 각 SubTask에 대해 `WorkUnitContext.createWorkUnit()`으로 WorkUnit을 생성하고, `ExecutorSlot.set()`으로 Executor에 전달한다.

### 6.3 Mapping

```java
// MappingWorksheet.java 245~325줄
public final class Mapping {
    private final ExecutorChunk[] mapping = new ExecutorChunk[works.size()];

    public ExecutorChunk assign(int index, ExecutorChunk element) {
        ExecutorChunk o = mapping[index];
        mapping[index] = element;
        return o;
    }

    public boolean isPartiallyValid() {
        int[] used = new int[executors.size()];
        for (int i = 0; i < mapping.length; i++) {
            ExecutorChunk ec = mapping[i];
            if (ec == null) continue;
            if (!ec.canAccept(works(i))) return false;
            if ((used[ec.index] += works(i).size()) > ec.capacity())
                return false;  // 용량 초과
        }
        return true;
    }

    public boolean isCompletelyValid() {
        for (ExecutorChunk ec : mapping)
            if (ec == null) return false;  // 미할당 존재
        return isPartiallyValid();
    }

    public void execute(WorkUnitContext wuc) {
        if (!isCompletelyValid())
            throw new IllegalStateException();
        for (int i = 0; i < size(); i++)
            assigned(i).execute(get(i), wuc);
    }
}
```

**`isPartiallyValid()`가 필요한 이유:** 탐욕적 할당 중 백트래킹을 위해, 현재까지의 부분 할당이 유효한지 빠르게 판단해야 한다. 이미 할당된 ExecutorChunk의 용량이 초과되었는지, 레이블/권한 제약을 위반했는지 확인한다.

---

## 7. Queue.Task 인터페이스

### 7.1 인터페이스 정의

```java
// Queue.java 2003~2186줄
public interface Task extends FullyNamedModelObject, SubTask {

    // 차단 여부 (deprecated, getCauseOfBlockage() 사용)
    @Deprecated
    default boolean isBuildBlocked() {
        return getCauseOfBlockage() != null;
    }

    // 차단 원인 반환 (null이면 실행 가능)
    @CheckForNull
    default CauseOfBlockage getCauseOfBlockage() {
        return null;
    }

    // LoadBalancer가 사용하는 노드 친화성 키
    default String getAffinityKey() {
        if (this instanceof FullyNamed fullyNamed) {
            return fullyNamed.getFullName();
        } else {
            return getFullDisplayName();
        }
    }

    // 동시 빌드 허용 여부
    default boolean isConcurrentBuild() {
        return false;
    }

    // SubTask 목록 (최소 1개, 자기 자신 포함)
    default Collection<? extends SubTask> getSubTasks() {
        return Set.of(this);
    }

    // 실행 시 사용할 인증 정보
    default @NonNull Authentication getDefaultAuthentication2() {
        return ACL.SYSTEM2;
    }
}
```

### 7.2 마커 인터페이스

```java
// Queue.java 1963줄
public interface TransientTask extends Task {}    // 영속화 대상 제외

// Queue.java 1970줄
public interface FlyweightTask extends Task {}    // Executor를 소비하지 않음

// Queue.java 1978줄
public interface NonBlockingTask extends Task {}  // 종료 중에도 실행 가능
```

**FlyweightTask:** Pipeline의 FlowExecution 같은 경량 태스크는 일반 Executor를 점유하지 않고 `OneOffExecutor`에서 실행된다. `maintain()` 메서드에서 별도 경로(`makeFlyWeightTaskBuildable()`)로 처리된다.

**NonBlockingTask:** Jenkins 종료 중(`isQuietingDown()`)에도 실행이 허용되는 태스크. 다른 태스크의 실행을 유지하는 데 필요한 관리성 태스크에 사용된다.

### 7.3 getCauseOfBlockage() 평가 체인

```java
// Queue.java 1209~1242줄
private CauseOfBlockage getCauseOfBlockageForItem(Item i) {
    // 1. Task 자체의 차단 원인
    CauseOfBlockage causeOfBlockage = getCauseOfBlockageForTask(i.task);
    if (causeOfBlockage != null) return causeOfBlockage;

    // 2. QueueTaskDispatcher의 거부권
    for (QueueTaskDispatcher d : QueueTaskDispatcher.all()) {
        causeOfBlockage = d.canRun(i);
        if (causeOfBlockage != null) return causeOfBlockage;
    }

    // 3. 동시 빌드 불허 시 중복 체크
    if (!(i instanceof BuildableItem)) {
        if (!i.task.isConcurrentBuild() &&
            (buildables.containsKey(i.task) || pendings.containsKey(i.task))) {
            return CauseOfBlockage.fromMessage(Messages._Queue_InProgress());
        }
    }

    return null;
}

// Queue.java 1252~1268줄
private CauseOfBlockage getCauseOfBlockageForTask(Task task) {
    // Task 자체가 보고하는 차단 원인
    CauseOfBlockage causeOfBlockage = task.getCauseOfBlockage();
    if (causeOfBlockage != null) return causeOfBlockage;

    // 리소스 충돌 확인
    if (!canRun(task.getResourceList())) {
        ResourceActivity r = getBlockingActivity(task);
        if (r != null) {
            if (r == task) // 자기 자신에 의해 차단 = 다른 빌드 진행 중
                return CauseOfBlockage.fromMessage(Messages._Queue_InProgress());
            return CauseOfBlockage.fromMessage(
                Messages._Queue_BlockedBy(r.getDisplayName()));
        }
    }
    return null;
}
```

**차단 원인 평가 순서:**

```
getCauseOfBlockageForItem(Item)
  |
  |-- 1. task.getCauseOfBlockage()
  |     └── Task 구현체가 직접 보고 (예: 비활성화된 Job)
  |
  |-- 2. canRun(task.getResourceList())
  |     └── 필요한 리소스가 다른 활동에 의해 점유됨
  |
  |-- 3. QueueTaskDispatcher.canRun(item)
  |     └── 플러그인이 거부권 행사 (확장 포인트)
  |
  |-- 4. isConcurrentBuild() 체크
  |     └── 동시 빌드 불허 시, 이미 실행/대기 중인 동일 Task 존재
  |
  v
  null (차단 없음) 또는 CauseOfBlockage (차단 존재)
```

---

## 8. Queue.JobOffer

### 8.1 Executor에서 작업 제공

```java
// Queue.java 235~330줄
public static class JobOffer extends MappingWorksheet.ExecutorSlot {
    public final Executor executor;
    private WorkUnit workUnit;

    private JobOffer(Executor executor) {
        this.executor = executor;
    }

    @Override
    protected void set(WorkUnit p) {
        assert this.workUnit == null;
        this.workUnit = p;
        assert executor.isParking();
        executor.start(workUnit);  // Executor 스레드 시작!
    }

    @Override
    public boolean isAvailable() {
        return workUnit == null
            && !executor.getOwner().isOffline()
            && executor.getOwner().isAcceptingTasks();
    }

    public @CheckForNull CauseOfBlockage getCauseOfBlockage(BuildableItem item) {
        Node node = getNode();
        if (node == null) {
            return CauseOfBlockage.fromMessage(
                Messages._Queue_node_has_been_removed_from_configuration(...));
        }

        // 1. Node 자체의 canTake 확인
        CauseOfBlockage reason = node.canTake(item);
        if (reason != null) return reason;

        // 2. QueueTaskDispatcher의 거부권
        for (QueueTaskDispatcher d : QueueTaskDispatcher.all()) {
            reason = d.canTake(node, item);
            if (reason != null) return reason;
        }

        // 3. 이미 사용 중인 슬롯
        if (workUnit != null) {
            return CauseOfBlockage.fromMessage(
                Messages._Queue_executor_slot_already_in_use());
        }

        // 4. 오프라인 또는 작업 미수락 상태
        if (executor.getOwner().isOffline()) {
            return new BecauseNodeIsOffline(node);
        }
        if (!executor.getOwner().isAcceptingTasks()) {
            return new BecauseNodeIsNotAcceptingTasks(node);
        }

        return null;  // 수락 가능
    }
}
```

**`set(WorkUnit)` 호출 경로:**

```
maintain()
  → LoadBalancer.map() → Mapping
  → Mapping.execute(WorkUnitContext)
  → ExecutorChunk.execute(WorkChunk, WorkUnitContext)
  → ExecutorSlot.set(WorkUnit)    [= JobOffer.set()]
  → Executor.start(WorkUnit)      // Executor 스레드가 실행 시작
```

**`getCauseOfBlockage(BuildableItem)` 체크 항목:**

| 순서 | 체크 | 설명 |
|------|------|------|
| 1 | `node == null` | 노드가 설정에서 제거됨 |
| 2 | `node.canTake(item)` | 노드의 내장 수락 조건 (레이블, 모드) |
| 3 | `QueueTaskDispatcher.canTake(node, item)` | 플러그인 거부권 |
| 4 | `workUnit != null` | 이미 다른 작업이 할당됨 |
| 5 | `executor.getOwner().isOffline()` | 노드가 오프라인 |
| 6 | `!isAcceptingTasks()` | 노드가 작업 수락 거부 (RetentionStrategy) |

---

## 9. FlyweightTask 처리

FlyweightTask는 일반 Executor 슬롯을 소비하지 않는 경량 태스크다. Pipeline의 `FlowExecution`이 대표적이다.

### 9.1 makeFlyWeightTaskBuildable()

```java
// Queue.java 1869~1918줄
private Runnable makeFlyWeightTaskBuildable(final BuildableItem p) {
    if (p.task instanceof FlyweightTask) {
        Jenkins h = Jenkins.get();
        Label lbl = p.getAssignedLabel();

        // 1. master에 바인딩된 경우
        Computer masterComputer = h.toComputer();
        if (lbl != null && lbl.equals(h.getSelfLabel()) && masterComputer != null) {
            if (h.canTake(p) == null) {
                return createFlyWeightTaskRunnable(p, masterComputer);
            }
            return null;
        }

        // 2. 레이블 없음 + master에서 실행 가능
        if (lbl == null && h.canTake(p) == null
            && masterComputer != null
            && masterComputer.isOnline()
            && masterComputer.isAcceptingTasks()) {
            return createFlyWeightTaskRunnable(p, masterComputer);
        }

        // 3. ConsistentHash로 에이전트 노드 선택
        Map<Node, Integer> hashSource = new HashMap<>(h.getNodes().size());
        for (Node n : h.getNodes()) {
            hashSource.put(n, n.getNumExecutors() * 100);
        }
        ConsistentHash<Node> hash = new ConsistentHash<>(NODE_HASH);
        hash.addAll(hashSource);

        String fullDisplayName = p.task.getFullDisplayName();
        for (Node n : hash.list(fullDisplayName)) {
            final Computer c = n.toComputer();
            if (c == null || c.isOffline()) continue;
            if (lbl != null && !lbl.contains(n)) continue;
            if (n.canTake(p) != null) continue;
            return createFlyWeightTaskRunnable(p, c);
        }
    }
    return null;
}

// Queue.java 1920~1929줄
private Runnable createFlyWeightTaskRunnable(final BuildableItem p,
        final @NonNull Computer c) {
    return () -> {
        c.startFlyWeightTask(new WorkUnitContext(p).createWorkUnit(p.task));
        makePending(p);
    };
}
```

**FlyweightTask 배치 우선순위:**

1. **master 우선:** 레이블이 지정되지 않은 경우, 에이전트 연결 불안정으로 인한 영향을 줄이기 위해 master에서 실행을 시도한다.
2. **ConsistentHash:** master에서 실행할 수 없으면, 에이전트 노드들 사이에서 ConsistentHash로 선택한다. `task.getFullDisplayName()`을 키로 사용하여 동일 작업이 같은 노드에 배치되는 경향성을 유지한다.

---

## 10. WorkUnit과 WorkUnitContext

### 10.1 WorkUnit -- Executor 핸드오버 단위

```java
// WorkUnit.java 42~112줄
public final class WorkUnit {
    public final SubTask work;       // 실행할 서브태스크
    public final WorkUnitContext context;  // 공유 컨텍스트
    private volatile Executor executor;   // 실행 중인 Executor
    private Executable executable;        // 생성된 실행체

    public void setExecutor(@CheckForNull Executor e) {
        executor = e;
        if (e != null) {
            context.future.addExecutor(e);
        }
    }

    public void setExecutable(Executable executable) {
        this.executable = executable;
        if (executable instanceof Run) {
            ((Run) executable).setQueueId(context.item.getId());
        }
    }

    public boolean isMainWork() {
        return context.task == work;
    }
}
```

### 10.2 WorkUnitContext -- 공유 컨텍스트

```java
// WorkUnitContext.java 48~215줄
public final class WorkUnitContext {
    public final BuildableItem item;
    public final Task task;
    public final FutureImpl future;
    public final List<Action> actions;
    private final Latch startLatch, endLatch;
    private List<WorkUnit> workUnits = new ArrayList<>();
    private volatile Throwable aborted;

    public WorkUnitContext(BuildableItem item) {
        this.item = item;
        this.task = item.task;
        this.future = (FutureImpl) item.getFuture();
        this.actions = new ArrayList<>(item.getActions());
        int workUnitSize = task.getSubTasks().size();
        startLatch = new Latch(workUnitSize) { ... };
        endLatch = new Latch(workUnitSize);
    }

    public WorkUnit createWorkUnit(SubTask execUnit) {
        WorkUnit wu = new WorkUnit(this, execUnit);
        workUnits.add(wu);
        return wu;
    }

    // 모든 Executor가 시작 동기화
    public void synchronizeStart() throws InterruptedException {
        try {
            startLatch.synchronize();
        } finally {
            Executor e = Executor.currentExecutor();
            WorkUnit wu = e.getCurrentWorkUnit();
            if (wu.isMainWork()) {
                future.start.set(e.getCurrentExecutable());
            }
        }
    }

    // 모든 Executor가 종료 동기화
    public void synchronizeEnd(Executor e, Executable executable,
            Throwable problems, long duration) throws InterruptedException {
        try {
            endLatch.synchronize();
        } finally {
            WorkUnit wu = e.getCurrentWorkUnit();
            if (wu.isMainWork()) {
                if (problems == null) {
                    future.set(executable);      // 정상 완료
                } else {
                    future.set(problems);         // 오류 발생
                }
                future.finished();
            }
        }
    }
}
```

**Latch 동기화가 필요한 이유:**

하나의 Task가 여러 SubTask로 구성될 수 있고, 각 SubTask는 다른 Executor에서 실행된다. `startLatch`는 모든 Executor가 실행을 시작할 준비가 될 때까지 대기하고, `endLatch`는 모든 Executor가 완료될 때까지 대기한다. 이를 통해:

1. 모든 SubTask가 동시에 시작됨을 보장
2. 메인 WorkUnit이 전체 결과를 `FutureImpl`에 설정하기 전에 모든 SubTask 완료를 대기

```
Task (예: 4개 SubTask)
  |
  +-- WorkUnit[0] (main) --> Executor A -- startLatch.synchronize() --+
  +-- WorkUnit[1]        --> Executor B -- startLatch.synchronize() --+-- 모두 동기화 후 실행
  +-- WorkUnit[2]        --> Executor C -- startLatch.synchronize() --+
  +-- WorkUnit[3]        --> Executor D -- startLatch.synchronize() --+
  |
  +-- (각각 실행) --+
  |                 |
  +-- endLatch.synchronize() -- 모든 Executor 완료 대기
  |
  v
  future.set(executable)  // main WorkUnit이 결과 설정
```

---

## 11. QueueListener -- 이벤트 리스너

```java
// QueueListener.java 29~83줄
public abstract class QueueListener implements ExtensionPoint {
    public void onEnterWaiting(WaitingItem wi) {}
    public void onLeaveWaiting(WaitingItem wi) {}
    public void onEnterBlocked(BlockedItem bi) {}
    public void onLeaveBlocked(BlockedItem bi) {}
    public void onEnterBuildable(BuildableItem bi) {}
    public void onLeaveBuildable(BuildableItem bi) {}
    public void onLeft(LeftItem li) {}
}
```

**콜백 호출 시점과 주의사항:**

| 콜백 | 호출 시점 | 호출 위치 |
|------|----------|----------|
| `onEnterWaiting` | `WaitingItem.enter()` | `scheduleInternal()` |
| `onLeaveWaiting` | `WaitingItem.leave()` | `maintain()` Phase 3 |
| `onEnterBlocked` | `BlockedItem.enter()` | `maintain()` Phase 2, 3 |
| `onLeaveBlocked` | `BlockedItem.leave()` | `maintain()` Phase 2 |
| `onEnterBuildable` | `BuildableItem.enter()` | `maintain()` Phase 2, 3, 5 |
| `onLeaveBuildable` | `BuildableItem.leave()` | `maintain()` Phase 5 |
| `onLeft` | `LeftItem.enter()` | `onStartExecuting()`, `Item.cancel()` |

**경고:** 이 콜백은 Queue의 잠금을 잡은 상태에서 동기적으로 호출된다. 콜백 내에서 무거운 작업을 수행하면 데드락이나 전체 큐 처리 지연을 유발할 수 있다. 비동기 처리가 권장된다.

### 11.1 Queue.Saver -- 영속화 트리거

```java
// Queue.java 3196~3251줄
@Extension
public static final class Saver extends QueueListener implements Runnable {
    static /*final*/ int DELAY_SECONDS =
        SystemProperties.getInteger(
            "hudson.model.Queue.Saver.DELAY_SECONDS", 60);

    private final Object lock = new Object();
    @GuardedBy("lock")
    private Future<?> nextSave;

    @Override
    public void onEnterWaiting(WaitingItem wi) { push(); }

    @Override
    public void onLeft(Queue.LeftItem li) { push(); }

    private void push() {
        if (DELAY_SECONDS < 0) return;
        synchronized (lock) {
            if (nextSave != null
                && !(nextSave.isDone() || nextSave.isCancelled()))
                return;  // 이미 예약된 저장이 있음
            nextSave = Timer.get().schedule(this, DELAY_SECONDS,
                TimeUnit.SECONDS);
        }
    }

    @Override
    public void run() {
        try {
            Jenkins j = Jenkins.getInstanceOrNull();
            if (j != null) j.getQueue().save();
        } finally {
            synchronized (lock) { nextSave = null; }
        }
    }
}
```

**지연 저장(Delayed Save)의 이유:** Queue는 빈번하게 상태가 변경된다. 매번 즉시 저장하면 I/O 부하가 커진다. 대신:

1. `onEnterWaiting` 또는 `onLeft` 이벤트 발생 시 60초 후 저장을 예약
2. 이미 예약된 저장이 있으면 추가 예약하지 않음
3. 60초 내에 여러 변경이 발생해도 하나의 저장으로 합쳐짐(coalesce)
4. `blocked`/`buildable` 상태 변경은 저장을 트리거하지 않음 -- 이 상태는 `maintain()`에 의해 자동 복구되므로

---

## 12. 영속화 (Persistence)

### 12.1 save() -- 큐 상태 저장

```java
// Queue.java 462~490줄
@Override
public void save() {
    if (BulkChange.contains(this)) return;
    if (Jenkins.getInstanceOrNull() == null) return;

    XmlFile queueFile = new XmlFile(XSTREAM, getXMLQueueFile());
    lock.lock();
    try {
        State state = new State();
        QueueIdStrategy.get().persist(state);
        for (Item item : getItems()) {
            if (item.task instanceof TransientTask) continue;
            state.items.add(item);
        }
        try {
            queueFile.write(state);
        } catch (IOException e) {
            LOGGER.log(
                e instanceof ClosedByInterruptException
                    ? Level.FINE : Level.WARNING,
                "Failed to write out the queue file " + getXMLQueueFile(), e);
        }
    } finally {
        lock.unlock();
    }
    SaveableListener.fireOnChange(this, queueFile);
}
```

**저장 형식:**

```java
// Queue.java 378~393줄
public static final class State {
    public List<Item> items = new ArrayList<>();
    public Map<String, Object> properties = new HashMap<>();
}
```

저장 파일: `$JENKINS_HOME/queue.xml` (또는 `$JENKINS_HOME/queue/{id}.xml`)

```xml
<!-- queue.xml 예시 (XStream 직렬화) -->
<queue>
  <items>
    <hudson.model.Queue_-WaitingItem>
      <id>42</id>
      <task class="hudson.model.FreeStyleProject" reference="../../..."/>
      <timestamp>2024-01-15 10:30:00</timestamp>
      <actions>...</actions>
    </hudson.model.Queue_-WaitingItem>
    <hudson.model.Queue_-BlockedItem>
      ...
    </hudson.model.Queue_-BlockedItem>
  </items>
  <properties>
    <entry>
      <string>next-id</string>
      <long>43</long>
    </entry>
  </properties>
</queue>
```

**`TransientTask` 제외:** `TransientTask` 마커 인터페이스를 구현한 Task는 영속화 대상에서 제외된다. 이는 일시적인 작업(예: 플러그인이 생성한 임시 태스크)이 재시작 후 불필요하게 복원되는 것을 방지한다.

### 12.2 load() -- 큐 상태 복원

```java
// Queue.java 398~457줄
public void load() {
    lock.lock();
    try { try {
        // 기존 큐 초기화
        waitingList.clear();
        blockedProjects.clear();
        buildables.clear();
        pendings.clear();

        File queueFile = getXMLQueueFile();
        if (Files.exists(queueFile.toPath())) {
            Object unmarshaledObj =
                new XmlFile(XSTREAM, queueFile).read();

            State state;
            if (unmarshaledObj instanceof State) {
                state = (State) unmarshaledObj;
            } else {
                // 하위 호환성 - 구버전 List 형식
                List items = (List) unmarshaledObj;
                state = new State();
                state.items.addAll(items);
            }
            QueueIdStrategy.get().load(state);

            for (Object o : state.items) {
                if (o instanceof Task) {
                    schedule((Task) o, 0);  // 하위 호환성
                } else if (o instanceof Item item) {
                    if (item.task == null) continue;  // 손상된 항목 무시
                    switch (item) {
                        case WaitingItem wi  -> item.enter(this);
                        case BlockedItem bi  -> item.enter(this);
                        case BuildableItem bdi -> item.enter(this);
                        default -> throw new IllegalStateException();
                    }
                }
            }

            // 원본 파일을 .bak으로 이동 (진단용 보존)
            File bk = new File(queueFile.getPath() + ".bak");
            Files.move(queueFile.toPath(), bk.toPath(),
                StandardCopyOption.REPLACE_EXISTING);
        }
    } catch (IOException | InvalidPathException e) {
        LOGGER.log(Level.WARNING,
            "Failed to load the queue file " + getXMLQueueFile(), e);
    } finally { updateSnapshot(); } } finally {
        lock.unlock();
    }
}
```

**복원 후 `.bak` 이동 이유:** 역직렬화 중 발생할 수 있는 문제를 진단하기 위해 원본 `queue.xml`을 `queue.xml.bak`으로 보존한다. Jenkins 코드 주석에 따르면, `MatrixConfiguration` 객체가 제대로 역직렬화되지 않아 Executor가 죽는 사건을 경험한 후 이 보호 조치가 추가되었다.

### 12.3 XStream 직렬화 설정

```java
// Queue.java 2942~3010줄
public static final XStream XSTREAM = new XStream2();

static {
    // hudson.model.Item → fullName 문자열로 직렬화
    XSTREAM.registerConverter(new AbstractSingleValueConverter() {
        @Override
        public boolean canConvert(Class klazz) {
            return hudson.model.Item.class.isAssignableFrom(klazz);
        }
        @Override
        public Object fromString(String string) {
            return Jenkins.get().getItemByFullName(string);
        }
        @Override
        public String toString(Object item) {
            return ((hudson.model.Item) item).getFullName();
        }
    });

    // Run → "project-name#build-number" 문자열로 직렬화
    XSTREAM.registerConverter(new AbstractSingleValueConverter() {
        @Override
        public boolean canConvert(Class klazz) {
            return Run.class.isAssignableFrom(klazz);
        }
        @Override
        public String toString(Object object) {
            Run<?, ?> run = (Run<?, ?>) object;
            return run.getParent().getFullName() + "#" + run.getNumber();
        }
    });

    // Queue 자체 → 싱글톤 참조로 직렬화
    XSTREAM.registerConverter(new AbstractSingleValueConverter() {
        @Override
        public boolean canConvert(Class klazz) {
            return Queue.class.isAssignableFrom(klazz);
        }
        @Override
        public Object fromString(String string) {
            return Jenkins.get().getQueue();
        }
        @Override
        public String toString(Object item) {
            return "queue";
        }
    });
}
```

**커스텀 직렬화의 이유:** Jenkins의 Job/Run 객체는 복잡한 객체 그래프를 가지므로, 전체를 직렬화하면 파일이 거대해지고 역직렬화가 불안정해진다. 대신 fullName(경로)이나 "project#number" 같은 간결한 참조로 저장하고, 복원 시 Jenkins 인스턴스에서 실제 객체를 찾아온다.

---

## 13. QueueSorter -- 빌드 순서 결정

```java
// QueueSorter.java 21~79줄
public abstract class QueueSorter implements ExtensionPoint {
    public static final Comparator<Queue.BlockedItem> DEFAULT_BLOCKED_ITEM_COMPARATOR =
        Comparator.comparingLong(Queue.Item::getInQueueSince);

    public abstract void sortBuildableItems(List<BuildableItem> buildables);

    public void sortBlockedItems(List<Queue.BlockedItem> blockedItems) {
        blockedItems.sort(DEFAULT_BLOCKED_ITEM_COMPARATOR);
    }

    @Initializer(after = JOB_CONFIG_ADAPTED)
    public static void installDefaultQueueSorter() {
        ExtensionList<QueueSorter> all = all();
        if (all.isEmpty()) return;

        Queue q = Jenkins.get().getQueue();
        if (q.getSorter() != null) return;  // 이미 설치됨
        q.setSorter(all.getFirst());

        if (all.size() > 1)
            LOGGER.warning("Multiple QueueSorters are registered. " +
                "Only the first one is used.");
    }
}
```

**기본 정렬:** QueueSorter가 등록되지 않은 경우, blocked 항목은 `inQueueSince` 기준으로 정렬된다 (오래된 것 먼저). buildables는 정렬되지 않고 입력 순서를 유지한다.

**QueueSorter의 영향 범위:**

```
maintain()
  |
  +-- Phase 2: blocked 항목 정렬
  |     → sorter.sortBlockedItems() 또는
  |       DEFAULT_BLOCKED_ITEM_COMPARATOR (inQueueSince 기준)
  |
  +-- Phase 4: buildable 항목 정렬
  |     → sorter.sortBuildableItems()
  |
  v
  buildables의 앞쪽 항목이 먼저 Executor 할당 기회를 얻음
```

---

## 14. QueueTaskDispatcher -- 실행 거부권

```java
// QueueTaskDispatcher.java 46~142줄
public abstract class QueueTaskDispatcher implements ExtensionPoint {
    // "이 노드에서 이 작업을 실행할 수 있는가?"
    public @CheckForNull CauseOfBlockage canTake(Node node,
            BuildableItem item) {
        return canTake(node, item.task);
    }

    // "이 작업을 지금 실행해도 되는가?" (노드 무관)
    public @CheckForNull CauseOfBlockage canRun(Queue.Item item) {
        return null;
    }
}
```

**`canTake()` vs `canRun()` 차이:**

| 메서드 | 호출 시점 | 의미 | 클라우드 프로비저닝 |
|--------|----------|------|-------------------|
| `canRun(Item)` | `getCauseOfBlockageForItem()` | "이 작업을 지금 실행할 수 없음" (시간/조건 문제) | 프로비저닝 안 함 (blocked) |
| `canTake(Node, BuildableItem)` | `JobOffer.getCauseOfBlockage()` | "이 노드에서 실행할 수 없음" (노드 문제) | 프로비저닝 시도 (buildable) |

**이 구분이 중요한 이유:** `canRun()`이 차단하면 항목이 `blockedProjects`에 들어가고, Jenkins는 새 노드를 프로비저닝하지 않는다. `canTake()`가 모든 노드에서 차단하면 항목은 `buildables`에 남아있고, Cloud 플러그인이 새 노드를 프로비저닝할 수 있다.

---

## 15. QueueDecisionHandler -- 스케줄링 결정

```java
// Queue.java 2641~2657줄
public abstract static class QueueDecisionHandler implements ExtensionPoint {
    public abstract boolean shouldSchedule(Task p, List<Action> actions);

    public static ExtensionList<QueueDecisionHandler> all() {
        return ExtensionList.lookup(QueueDecisionHandler.class);
    }
}
```

`schedule2()` 메서드의 최초 단계에서 호출된다. 하나라도 `false`를 반환하면 작업이 큐에 추가되지 않고 `ScheduleResult.refused()`가 반환된다.

**QueueAction.shouldSchedule()과의 차이:**

| 메커니즘 | 호출 시점 | 역할 |
|---------|----------|------|
| `QueueDecisionHandler.shouldSchedule()` | 큐 진입 전 | 전역적 거부 (예: 시스템 정책) |
| `QueueAction.shouldSchedule()` | 중복 검사 시 | "이 액션이 다르므로 별도 실행 필요" 판단 |
| `FoldableAction.foldIntoExisting()` | 중복으로 판정 후 | 기존 항목에 액션 정보 병합 |

---

## 16. 동시 빌드와 중복 검사

### 16.1 중복 방지 로직

```java
// scheduleInternal() 내부 (Queue.java 618~630줄)
List<Item> duplicatesInQueue = new ArrayList<>();
for (Item item : liveGetItems(p)) {
    boolean shouldScheduleItem = false;
    for (QueueAction action : item.getActions(QueueAction.class)) {
        shouldScheduleItem |= action.shouldSchedule(actions);
    }
    for (QueueAction action : Util.filter(actions, QueueAction.class)) {
        shouldScheduleItem |= action.shouldSchedule(
            new ArrayList<>(item.getAllActions()));
    }
    if (!shouldScheduleItem) {
        duplicatesInQueue.add(item);
    }
}
```

**`liveGetItems(Task)`:** 현재 잠금을 잡은 상태에서 실제 컬렉션을 직접 조회한다 (스냅샷이 아닌 라이브 데이터). 주의점으로, `pendings`는 제외된다 -- 이미 `WorkUnitContext.actions`가 확정되어 변경이 불가하기 때문이다.

```java
// Queue.java 1108~1130줄
private List<Item> liveGetItems(Task t) {
    lock.lock();
    try {
        List<Item> result = new ArrayList<>();
        result.addAll(blockedProjects.getAll(t));
        result.addAll(buildables.getAll(t));
        // pendings는 제외 -- actions가 이미 확정됨
        for (Item item : waitingList) {
            if (item.task.equals(t)) result.add(item);
        }
        return result;
    } finally {
        lock.unlock();
    }
}
```

### 16.2 동시 빌드 제어

```java
// getCauseOfBlockageForItem() 내부 (Queue.java 1228~1239줄)
if (!(i instanceof BuildableItem)) {
    if (!i.task.isConcurrentBuild()
        && (buildables.containsKey(i.task) || pendings.containsKey(i.task))) {
        return CauseOfBlockage.fromMessage(Messages._Queue_InProgress());
    }
}
```

`isConcurrentBuild()` 기본값은 `false`이므로, 동일 Task의 동시 실행은 기본적으로 차단된다. `BuildableItem` 자체에 대해서는 이 체크를 건너뛰는데, 이미 buildables에 있는 항목이 자기 자신을 차단하는 것을 방지하기 위함이다.

---

## 17. 전체 아키텍처 다이어그램

```
                    사용자/트리거
                        |
                   schedule2()
                        |
              QueueDecisionHandler
              (shouldSchedule 확인)
                        |
                scheduleInternal()
              (중복 검사 + FoldableAction)
                        |
                        v
    +-------------------------------------------+
    |              Queue (빌드 큐)                |
    |                                           |
    |  waitingList ──── maintain() ──┐          |
    |  (TreeSet)         Phase 3     |          |
    |                                v          |
    |  blockedProjects ← maintain() → buildables|
    |  (ItemList)      Phase 2      (ItemList)  |
    |                                |          |
    |                       LoadBalancer.map()  |
    |                       MappingWorksheet    |
    |                                |          |
    |                                v          |
    |                            pendings       |
    |                           (ItemList)      |
    |                                |          |
    |                     onStartExecuting()    |
    |                                |          |
    |                                v          |
    |                            leftItems      |
    |                        (Cache, 5분 TTL)    |
    +-------------------------------------------+
              |                          |
         QueueListener              save()/load()
         (이벤트 통지)              (queue.xml, XStream)
              |
    +--------------------+
    | Saver              |
    | (60초 지연 저장)    |
    +--------------------+
```

---

## 18. 주요 Jenkins JIRA 이슈

Queue.java 코드에는 수많은 JIRA 이슈 참조가 있다. 주요 이슈:

| 이슈 | 내용 | 코드 위치 |
|------|------|----------|
| JENKINS-14813 | 취소 시 "too late" 케이스 처리 | `doCancelItem()` (773줄) |
| JENKINS-27708 | blocked 태스크가 라이브 상태를 반영하지 못하는 문제 | `maintain()` Phase 5 (1733, 1813~1823줄) |
| JENKINS-27871 | 동일 이슈의 변종 | `maintain()` Phase 5 |
| JENKINS-28840 | 인터럽트된 Executor에서 데드락 발생 | `maintain()` Phase 1 (1611줄) |
| JENKINS-28926 | blocked 해제 후 스냅샷 미갱신으로 인한 충돌 | `maintain()` Phase 2, 5 (1675, 1749줄) |
| JENKINS-30084 | FlyweightTask가 노드 프로비저닝을 트리거하지 못하는 문제 | `makeBuildable()` (1850줄) |
| JENKINS-51584 | TransientActionFactory 액션이 영속화되는 문제 | `Item` 복사 생성자 (2426줄), `WorkUnitContext` (79줄) |
| JENKINS-8882 | 예측 부하가 가용 Executor를 초과하는 문제 | `MappingWorksheet` 생성자 |

---

## 19. 성능 고려사항

### 19.1 잠금 경합 (Lock Contention)

Queue의 모든 상태 변경은 단일 `ReentrantLock`으로 직렬화된다. 이는 단순하지만, 대규모 Jenkins에서 병목이 될 수 있다:

- **`maintain()` 호출 빈도:** 5초 주기 + 이벤트 기반 트리거
- **`maintain()` 실행 시간:** O(W + B + P) * O(D) -- W(waiting), B(blocked), P(buildable) 항목 수와 D(dispatcher) 수에 비례
- **`Snapshot` 패턴:** 읽기 전용 접근은 잠금 없이 O(1)

### 19.2 최적화 전략

1. **`AtmostOneTaskExecutor`:** 여러 `scheduleMaintenance()` 호출을 하나로 합침
2. **`Snapshot`:** volatile 참조로 UI 스레드의 잠금 대기 제거
3. **`tryWithLock()`:** 비차단 잠금으로 UI 응답성 보장
4. **`reasonMap` 캐싱:** `maintain()` Phase 5에서 동일 노드에 대한 `getCauseOfBlockage()` 결과를 캐싱하여 반복 계산 방지

```java
// Queue.java 1767~1776줄
Map<Node, CauseOfBlockage> reasonMap = new HashMap<>();
for (JobOffer j : parked.values()) {
    Node offerNode = j.getNode();
    CauseOfBlockage reason;
    if (reasonMap.containsKey(offerNode)) {
        reason = reasonMap.get(offerNode);  // 캐시 히트
    } else {
        reason = j.getCauseOfBlockage(p);
        reasonMap.put(offerNode, reason);   // 캐시 저장
    }
}
```

---

## 20. 확장 포인트 요약

Queue 시스템은 다수의 확장 포인트를 제공하여 플러그인이 동작을 커스터마이즈할 수 있다:

| 확장 포인트 | 인터페이스 | 역할 |
|------------|-----------|------|
| `QueueDecisionHandler` | `shouldSchedule(Task, List<Action>)` | 큐 진입 거부 |
| `QueueTaskDispatcher` | `canRun(Item)`, `canTake(Node, BuildableItem)` | 실행/배치 거부 |
| `QueueSorter` | `sortBuildableItems()`, `sortBlockedItems()` | 우선순위 정렬 |
| `QueueListener` | `onEnterWaiting()`, `onLeft()` 등 | 상태 변화 통지 |
| `LoadBalancer` | `map(Task, MappingWorksheet)` | Executor 배치 전략 |
| `QueueAction` | `shouldSchedule(List<Action>)` | 중복 판단 |
| `FoldableAction` | `foldIntoExisting(Item, Task, List<Action>)` | 중복 시 정보 병합 |
| `QueueItemAuthenticator` | `authenticate2(Item)` | 실행 시 인증 정보 결정 |
| `QueueIdStrategy` | `generateIdFor()`, `persist()`, `load()` | ID 생성 및 영속화 전략 |

---

## 요약

Jenkins의 빌드 큐는 단순한 FIFO 대기열이 아니라, 5단계 상태 머신 + 다중 확장 포인트 + 동시성 제어가 결합된 정교한 스케줄링 엔진이다.

**핵심 설계 결정과 그 이유:**

1. **5단계 상태 분리:** 각 단계가 명확한 의미를 가지므로, UI에서 "왜 빌드가 시작되지 않는지" 정확하게 표시할 수 있다
2. **Snapshot 패턴:** 단일 잠금의 단순성을 유지하면서, 읽기 전용 접근의 성능을 보장한다
3. **ConsistentHash 로드밸런싱:** 작업의 노드 친화성을 유지하여 workspace 캐시 활용률을 높인다
4. **지연 영속화:** 빈번한 상태 변경에도 I/O 부하를 최소화하면서, 크래시 후 복구 가능성을 보장한다
5. **풍부한 확장 포인트:** 플러그인이 스케줄링의 모든 단계에 개입할 수 있어, Jenkins의 유연한 확장성을 실현한다
