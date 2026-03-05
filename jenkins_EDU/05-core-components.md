# 05. Jenkins 핵심 컴포넌트

Jenkins의 핵심 컴포넌트 7개를 소스코드 기반으로 분석한다.
각 컴포넌트의 내부 구조, 동시성 모델, 그리고 **왜 이런 설계를 선택했는지**를 중심으로 설명한다.

> **소스 경로 기준**: `jenkins/core/src/main/java/`

---

## 목차

1. [Jenkins 싱글턴](#1-jenkins-싱글턴)
2. [빌드 큐 (Queue)](#2-빌드-큐-queue)
3. [Executor](#3-executor)
4. [Computer](#4-computer)
5. [Node](#5-node)
6. [PluginManager](#6-pluginmanager)
7. [ExtensionList](#7-extensionlist)
8. [컴포넌트 관계 다이어그램](#8-컴포넌트-관계-다이어그램)

---

## 1. Jenkins 싱글턴

**파일**: `jenkins/model/Jenkins.java` (~5990줄)

### 1.1 클래스 선언과 상속 계층

```
Jenkins extends AbstractCIBase
    implements DirectlyModifiableTopLevelItemGroup,
               StaplerProxy, StaplerFallback,
               ModifiableViewGroup, AccessControlled,
               DescriptorByNameOwner, ModelObjectWithContextMenu,
               ModelObjectWithChildren, OnMaster, Loadable
```

상속 계층을 풀어보면 다음과 같다:

```
Object
  └─ AbstractModelObject
       └─ Node                          ← Jenkins 자체가 Node (마스터 노드)
            └─ AbstractCIBase           ← ItemGroup<TopLevelItem>, ViewGroup
                 └─ Jenkins             ← 전체 시스템의 루트 객체
```

**왜 Jenkins가 Node를 상속하는가?**

Jenkins 마스터 자체도 빌드를 실행할 수 있는 노드다.
별도의 "마스터 노드" 클래스를 만들지 않고, Jenkins 자체가 Node를 상속함으로써
마스터와 에이전트를 동일한 추상화로 다룰 수 있다.
이 설계 덕분에 `Jenkins.get().getComputers()`가 마스터 컴퓨터를 포함한
모든 컴퓨터 목록을 일관되게 반환한다.

### 1.2 핵심 필드

소스코드에서 직접 확인한 Jenkins의 주요 필드:

```java
// jenkins/model/Jenkins.java

// --- 빌드 큐 ---
private final transient Queue queue;                          // 라인 358

// --- 실행자 설정 ---
private int numExecutors = 2;                                 // 라인 399
private Mode mode = Mode.NORMAL;                              // 라인 404

// --- 보안 ---
private volatile AuthorizationStrategy authorizationStrategy  // 라인 424
    = AuthorizationStrategy.UNSECURED;
private volatile SecurityRealm securityRealm                  // 라인 441
    = SecurityRealm.NO_AUTHENTICATION;

// --- 아이템(Job) 관리 ---
/*package*/ final transient Map<String, TopLevelItem> items    // 라인 489
    = new CopyOnWriteMap.Tree<>(String.CASE_INSENSITIVE_ORDER);

// --- 싱글턴 ---
private static Jenkins theInstance;                           // 라인 494

// --- 초기화 상태 ---
private transient volatile InitMilestone initLevel            // 라인 484
    = InitMilestone.STARTED;

// --- 노드/컴퓨터 관리 ---
protected final transient ConcurrentMap<Node, Computer>       // 라인 546
    computers = new ConcurrentHashMap<>();
private final transient Nodes nodes = new Nodes(this);        // 라인 592

// --- 확장 포인트 ---
private final transient Map<Class, ExtensionList>             // 라인 535
    extensionLists = new ConcurrentHashMap<>();

// --- 기타 ---
public final transient File root;                             // 라인 479
private final transient UpdateCenter updateCenter;            // 라인 858
public final Hudson.CloudList clouds;                         // 라인 551
```

**왜 items에 CopyOnWriteMap을 사용하는가?**

Job 목록은 읽기가 쓰기보다 압도적으로 많은 패턴이다.
웹 UI에서 매 요청마다 Job 목록을 조회하지만, Job 생성/삭제는 드문 이벤트다.
`CopyOnWriteMap`은 쓰기 시 전체 복사 비용이 있지만, 읽기에 대해서는
동기화 없이 일관된 스냅샷을 제공한다. 또한 `String.CASE_INSENSITIVE_ORDER`를
사용해 Windows 파일시스템에서의 대소문자 비구분과 일관성을 맞춘다.

### 1.3 싱글턴 패턴: getInstance / get

```java
// jenkins/model/Jenkins.java 라인 803~809
public static Jenkins get() throws IllegalStateException {
    Jenkins instance = getInstanceOrNull();
    if (instance == null) {
        throw new IllegalStateException(
            "Jenkins.instance is missing. Read the documentation of "
            + "Jenkins.getInstanceOrNull to see what you are doing wrong.");
    }
    return instance;
}

// 라인 838~840
public static Jenkins getInstanceOrNull() {
    return HOLDER.getInstance();
}
```

| 메서드 | 반환 | null 가능 | 용도 |
|--------|------|-----------|------|
| `get()` | `@NonNull Jenkins` | 불가 (예외 발생) | 일반적인 코드에서 사용 |
| `getInstanceOrNull()` | `@Nullable Jenkins` | 가능 | 초기화 중/종료 중 안전한 접근 |
| `getInstance()` | `@Nullable Jenkins` | 가능 | **@Deprecated** - getInstanceOrNull의 별칭 |
| `getActiveInstance()` | `@NonNull Jenkins` | 불가 (예외 발생) | **@Deprecated** - get()의 별칭 |

**왜 두 가지 접근 방식을 제공하는가?**

Jenkins는 서블릿 컨테이너 위에서 동작하며, 초기화(startup)와 종료(shutdown) 중에는
싱글턴이 null일 수 있다. 플러그인 코드가 이 시점에 접근하면 NullPointerException이
발생한다. `getInstanceOrNull()`은 이런 edge case에서 안전하게 null을 반환하고,
`get()`은 명확한 오류 메시지와 함께 예외를 던져 디버깅을 돕는다.

### 1.4 Stapler 루트: 모든 URL의 시작점

Jenkins는 Kohsuke Kawaguchi가 만든 Stapler 프레임워크를 사용해 URL을 Java 객체 트리에 매핑한다.

```
URL 요청                          객체 탐색
──────────────────────────────    ──────────────────────────────
/                            →   Jenkins (루트)
/job/my-project              →   Jenkins.getItem("my-project")
/computer/agent-1            →   Jenkins.getComputer("agent-1")
/queue                       →   Jenkins.getQueue()
/pluginManager               →   Jenkins.getPluginManager()
/manage                      →   ManageJenkinsAction
```

Jenkins 클래스가 `StaplerProxy`와 `StaplerFallback`을 구현하는 이유:

- **StaplerProxy**: 요청이 Jenkins 객체에 도달하기 전에 보안 검사를 수행
- **StaplerFallback**: URL 경로가 매칭되지 않을 때 대체 객체로 위임

### 1.5 역할 요약

Jenkins 싱글턴은 다음 여섯 가지 핵심 역할을 수행한다:

```
┌─────────────────────────────────────────────────────────┐
│                    Jenkins 싱글턴                         │
├─────────────────────────────────────────────────────────┤
│ 1. Job/Item 관리                                        │
│    - items 맵으로 TopLevelItem 관리                      │
│    - 생성/삭제/이동/복사 API                              │
│                                                         │
│ 2. 플러그인 관리                                         │
│    - PluginManager 소유                                  │
│    - ExtensionList 레지스트리                             │
│                                                         │
│ 3. 보안                                                 │
│    - SecurityRealm (인증)                                │
│    - AuthorizationStrategy (인가)                        │
│                                                         │
│ 4. 큐 관리                                              │
│    - Queue 인스턴스 소유                                  │
│    - 스케줄링 트리거                                      │
│                                                         │
│ 5. 노드/컴퓨터 관리                                      │
│    - computers 맵으로 런타임 상태 추적                    │
│    - Nodes 객체로 설정 관리                               │
│                                                         │
│ 6. 웹 UI 루트                                           │
│    - Stapler URL 라우팅의 시작점                          │
│    - View 관리                                           │
└─────────────────────────────────────────────────────────┘
```

---

## 2. 빌드 큐 (Queue)

**파일**: `hudson/model/Queue.java` (~3252줄)

### 2.1 클래스 선언

```java
// hudson/model/Queue.java 라인 183
public class Queue extends ResourceController implements Saveable {
```

`ResourceController`를 상속하는 이유는 빌드가 외부 리소스(예: 라이선스 서버,
하드웨어 장비)를 배타적으로 사용해야 할 때 리소스 충돌을 방지하기 위해서다.
`Saveable`을 구현하여 큐 상태를 XML로 직렬화하고, Jenkins 재시작 후에도
대기 중인 빌드를 복원한다.

### 2.2 4단계 상태 머신

Queue는 빌드 요청이 실행되기까지 거치는 4단계 상태를 내부 자료구조로 관리한다.

```java
// hudson/model/Queue.java

// 라인 193 - 정숙 기간(quiet period)이 끝나지 않은 항목
private final Set<WaitingItem> waitingList = new TreeSet<>();

// 라인 202 - 다른 빌드가 진행 중이거나 리소스가 없어 차단된 항목
private final ItemList<BlockedItem> blockedProjects = new ItemList<>();

// 라인 209 - 즉시 실행 가능하지만 Executor를 기다리는 항목
private final ItemList<BuildableItem> buildables = new ItemList<>();

// 라인 215 - Executor에 할당되었지만 아직 실행 시작 전인 항목
private final ItemList<BuildableItem> pendings = new ItemList<>();
```

상태 전이를 다이어그램으로 표현하면:

```
                        ┌──────────────────────┐
                        │                      │
   schedule()           v                      │
  ─────────→  [WaitingItem]                    │
                  │                            │
                  │ quiet period 만료           │
                  │                            │
                  ├───→ [BlockedItem] ←────────┘
                  │         │                 (노드 소실 시
                  │         │ 차단 해제         pending에서
                  │         │                  복귀)
                  v         v
              [BuildableItem]
                  │
                  │ LoadBalancer가
                  │ Executor에 매핑
                  v
              [Pending]
                  │
                  │ Executor.start()
                  v
              [LeftItem] ──→ (5분 캐시 후 제거)
```

소스코드의 원본 주석(라인 154~164)에 있는 ASCII 다이어그램:

```
(enter) --> waitingList --+--> blockedProjects
                          |        ^
                          |        |
                          |        v
                          +--> buildables ---> pending ---> left
                                   ^              |
                                   |              |
                                   +---(rarely)---+
```

**왜 4단계로 나누었는가?**

단순히 "대기"와 "실행 중" 두 상태만 있으면 충분하지 않은 이유가 있다:

1. **WaitingItem**: 사용자가 코드를 커밋하면 짧은 시간 내에 여러 커밋이 올 수 있다.
   정숙 기간(quiet period)을 두어 여러 변경을 하나의 빌드로 묶는다.

2. **BlockedItem**: 동일 Job의 이전 빌드가 아직 실행 중이거나,
   `QueueTaskDispatcher`가 차단을 결정한 경우. 빌드 가능 목록에서
   분리해야 LoadBalancer가 불필요한 매핑 시도를 하지 않는다.

3. **BuildableItem**: Executor가 사용 가능해지기를 기다리는 순수한 대기 상태.
   `QueueSorter`로 우선순위를 조정할 수 있는 지점이다.

4. **Pending**: Executor에 할당되었지만 `Executor.run()`이 아직 시작되지 않은
   짧은 순간. 노드가 이 사이에 제거되면 다시 buildable로 복귀해야 하므로
   별도 상태가 필요하다.

### 2.3 Queue.maintain(): 주기적 유지보수

```java
// hudson/model/Queue.java 라인 1593~1829
public void maintain() {
    Jenkins jenkins = Jenkins.getInstanceOrNull();
    if (jenkins == null) {
        return;
    }
    lock.lock();
    try { try {
        // 1. parked executor 수집 및 lost pending 복구
        Map<Executor, JobOffer> parked = new HashMap<>();
        // ... executor 순회, isParking() 확인 ...

        // 2. blocked → buildable 전이
        for (BlockedItem p : blockedItems) {
            CauseOfBlockage causeOfBlockage = getCauseOfBlockageForItem(p);
            if (causeOfBlockage == null) {
                Runnable r = makeBuildable(new BuildableItem(p));
                // ...
            }
        }

        // 3. waitingList → buildable/blocked 전이
        while (!waitingList.isEmpty()) {
            WaitingItem top = peek();
            if (top.timestamp > now) break;
            // ...
        }

        // 4. buildable → executor 할당
        for (BuildableItem p : new ArrayList<>(buildables)) {
            if (p.task instanceof FlyweightTask) {
                // 경량 태스크 처리
            } else {
                // LoadBalancer로 매핑
                MappingWorksheet ws = new MappingWorksheet(p, candidates);
                Mapping m = loadBalancer.map(p.task, ws);
                if (m != null) {
                    WorkUnitContext wuc = new WorkUnitContext(p);
                    m.execute(wuc);
                    p.leave(this);
                    makePending(p);
                }
            }
        }
    } finally { updateSnapshot(); } } finally {
        lock.unlock();
    }
}
```

`maintain()` 메서드의 처리 순서가 중요하다:

| 순서 | 단계 | 설명 |
|------|------|------|
| 1 | parked executor 수집 | 유휴 executor를 `JobOffer`로 포장 |
| 2 | lost pending 복구 | 할당된 executor가 사라진 pending 항목을 buildable로 복귀 |
| 3 | blocked → buildable | 차단 조건이 해소된 항목 전이 |
| 4 | waiting → buildable/blocked | 정숙 기간이 만료된 항목 전이 |
| 5 | buildable → pending | LoadBalancer로 executor에 매핑, pending으로 전이 |

**왜 이 순서인가?**

blocked 항목을 먼저 처리한 후 waiting 항목을 처리하는 이유:
이미 한 번 buildable이었다가 blocked된 항목은 더 오래 기다렸으므로
공정성(fairness) 관점에서 우선 처리한다. waiting 항목은 방금 정숙 기간이
끝난 신규 항목이다.

### 2.4 maintainerThread: AtmostOneTaskExecutor

```java
// hudson/model/Queue.java 라인 336~347
private final transient AtmostOneTaskExecutor<Void> maintainerThread =
    new AtmostOneTaskExecutor<>(new Callable<>() {
        @Override
        public Void call() throws Exception {
            maintain();
            return null;
        }
    });
```

`maintain()`은 `AtmostOneTaskExecutor`를 통해 실행된다.
이 패턴은 동시에 최대 하나의 maintain() 호출만 실행되도록 보장한다.
여러 스레드에서 `scheduleMaintenance()`를 호출하더라도 중복 실행을 방지한다.

또한 별도 타이머 스레드(`MaintainTask`)가 주기적으로 호출한다:

```java
// 라인 357
new MaintainTask(this).periodic();
```

**왜 별도 스레드가 필요한가?**

모든 executor가 바쁘게 작업 중이면 어떤 executor도 큐를 확인하지 않는다.
이때 새 executor가 가용해져도 큐에서 작업을 꺼내줄 주체가 없다.
별도 타이머 스레드가 주기적으로 maintain()을 호출하여 이 교착 상태를 방지한다.

### 2.5 동시성 제어: ReentrantLock + Condition

```java
// hudson/model/Queue.java 라인 349~351
private final transient ReentrantLock lock = new ReentrantLock();
private final transient Condition condition = lock.newCondition();
```

**왜 synchronized가 아닌 ReentrantLock인가?**

1. **tryLock()**: 큐 락을 획득하지 못하면 다른 작업을 수행할 수 있다
2. **Condition**: 특정 조건(executor가 사용 가능해짐)을 기다리는 정밀한 대기/통지
3. **공정성(fairness)**: 필요시 공정 모드로 전환 가능
4. **락 진단**: `lock.getHoldCount()`, `lock.isHeldByCurrentThread()` 등으로
   데드락 진단이 용이

### 2.6 Snapshot: 불변 스냅샷

```java
// hudson/model/Queue.java 라인 217
private transient volatile Snapshot snapshot =
    new Snapshot(waitingList, blockedProjects, buildables, pendings);

// 라인 3085~3103
private static class Snapshot {
    private final Set<WaitingItem> waitingList;
    private final List<BlockedItem> blockedProjects;
    private final List<BuildableItem> buildables;
    private final List<BuildableItem> pendings;

    Snapshot(Set<WaitingItem> waitingList,
             List<BlockedItem> blockedProjects,
             List<BuildableItem> buildables,
             List<BuildableItem> pendings) {
        this.waitingList = new LinkedHashSet<>(waitingList);
        this.blockedProjects = new ArrayList<>(blockedProjects);
        this.buildables = new ArrayList<>(buildables);
        this.pendings = new ArrayList<>(pendings);
    }
}
```

**왜 Snapshot인가?**

외부에서 큐 상태를 조회할 때마다 락을 잡으면 성능이 떨어진다.
Snapshot은 현재 큐 상태의 불변 복사본이다. `volatile` 참조를 통해
락 없이 읽기가 가능하다. 쓰기(상태 전이) 시에만 락을 잡고
`updateSnapshot()`을 호출하여 새 Snapshot을 생성한다.

이 패턴은 CRWP(Copy-on-write Read-Write Pattern)의 변형으로,
읽기 빈도가 쓰기 빈도보다 훨씬 높은 큐 상태 조회에 최적화되어 있다.

### 2.7 LoadBalancer: 빌드-노드 매핑

```java
// hudson/model/LoadBalancer.java 라인 54
public abstract class LoadBalancer implements ExtensionPoint {
    /**
     * Chooses the executor(s) to carry out the build for the given task.
     */
    public abstract Mapping map(Task task, MappingWorksheet worksheet);
}
```

`Queue.maintain()`에서 buildable 항목을 executor에 매핑할 때 사용한다:

```java
// Queue.java 라인 1787~1788
MappingWorksheet ws = new MappingWorksheet(p, candidates);
Mapping m = loadBalancer.map(p.task, ws);
```

LoadBalancer는 ExtensionPoint이므로 플러그인으로 교체 가능하다.
기본 구현은 `ConsistentHash` 기반으로, 같은 Job은 가능하면 같은 노드에서
실행되도록 하여 워크스페이스 캐시를 재활용한다.

### 2.8 JobOffer: Executor와 Queue의 인터페이스

```java
// hudson/model/Queue.java 라인 235~330
public static class JobOffer extends MappingWorksheet.ExecutorSlot {
    public final Executor executor;
    private WorkUnit workUnit;

    @Override
    protected void set(WorkUnit p) {
        assert this.workUnit == null;
        this.workUnit = p;
        assert executor.isParking();
        executor.start(workUnit);       // ← Executor 스레드 시작
    }

    public @CheckForNull CauseOfBlockage getCauseOfBlockage(BuildableItem item) {
        Node node = getNode();
        if (node == null) {
            return CauseOfBlockage.fromMessage(...);
        }
        CauseOfBlockage reason = node.canTake(item);
        if (reason != null) return reason;

        for (QueueTaskDispatcher d : QueueTaskDispatcher.all()) {
            reason = d.canTake(node, item);
            if (reason != null) return reason;
        }
        // ... isAvailable 확인 ...
        return null;
    }
}
```

**왜 JobOffer라는 중간 객체가 필요한가?**

Executor와 Queue 사이의 결합도를 낮추기 위해서다. Queue는 Executor에 대해
직접 알지 않고, JobOffer를 통해 "이 executor가 이 작업을 받을 수 있는지"를
확인한다. JobOffer의 `getCauseOfBlockage()`는 Node 수준, QueueTaskDispatcher
수준의 차단 이유를 모두 확인하는 통합 검증 지점이다.

---

## 3. Executor

**파일**: `hudson/model/Executor.java` (~992줄)

### 3.1 클래스 선언과 핵심 필드

```java
// hudson/model/Executor.java 라인 93
public class Executor extends Thread implements ModelObject, IExecutor {

    protected final @NonNull Computer owner;     // 라인 94
    private final Queue queue;                    // 라인 95
    private final ReadWriteLock lock              // 라인 96
        = new ReentrantReadWriteLock();

    @GuardedBy("lock")
    private long startTime;                       // 라인 100
    private int number;                           // 라인 109
    @GuardedBy("lock")
    private Queue.Executable executable;          // 라인 114
    @GuardedBy("lock")
    private AsynchronousExecution asynchronousExecution; // 라인 126
    @GuardedBy("lock")
    private WorkUnit workUnit;                    // 라인 133
    @GuardedBy("lock")
    private boolean started;                      // 라인 136
    @GuardedBy("lock")
    private Result interruptStatus;               // 라인 143
    @GuardedBy("lock")
    private final List<CauseOfInterruption> causes // 라인 149
        = new Vector<>();
}
```

**왜 Executor가 Thread를 직접 상속하는가?**

역사적 이유가 크다. Jenkins(당시 Hudson) 초기 설계에서 Executor는
Thread를 상속하여 빌드별로 하나의 스레드를 할당했다. 현대적 관점에서는
ThreadPool + Runnable이 더 나은 설계이지만, 기존 플러그인들이
`Thread.currentThread() instanceof Executor` 패턴에 의존하기 때문에
변경할 수 없다.

1.536 이후 Executor 스레드는 온디맨드(on-demand)로 시작된다.
`isParking()` 상태에서는 스레드가 시작되지 않고, Queue가 작업을
할당하면 그때 `start()`를 호출한다.

### 3.2 start(WorkUnit): 작업 할당

```java
// hudson/model/Executor.java 라인 810~819
/*protected*/ void start(WorkUnit task) {
    lock.writeLock().lock();
    try {
        this.workUnit = task;
        super.start();      // Thread.start() — 스레드 시작
        started = true;
    } finally {
        lock.writeLock().unlock();
    }
}
```

Queue의 `JobOffer.set()`에서 호출된다. write lock으로 보호되며,
workUnit 할당과 스레드 시작을 원자적으로 수행한다.

### 3.3 isParking(): 유휴 상태 확인

```java
// hudson/model/Executor.java 라인 688~695
public boolean isParking() {
    lock.readLock().lock();
    try {
        return !started;
    } finally {
        lock.readLock().unlock();
    }
}
```

Queue.maintain()에서 유휴 executor를 찾을 때 사용한다:

```java
// Queue.java 라인 1620~1622
if (e.isParking()) {
    parked.put(e, new JobOffer(e));
}
```

### 3.4 run(): 메인 실행 루프

Executor.run()의 전체 흐름을 단계별로 분석한다:

```java
// hudson/model/Executor.java 라인 338~491

@Override
public void run() {
    // [1] 사전 검사: 노드가 온라인인지 확인
    if (!(owner instanceof Jenkins.MasterComputer)) {
        if (!owner.isOnline()) {
            resetWorkUnit("went off-line before the task's worker thread started");
            owner.removeExecutor(this);
            queue.scheduleMaintenance();
            return;
        }
        if (owner.getNode() == null) {
            resetWorkUnit("was removed before the task's worker thread started");
            owner.removeExecutor(this);
            queue.scheduleMaintenance();
            return;
        }
    }

    // [2] startTime 기록, workUnit 참조 획득
    lock.writeLock().lock();
    try {
        startTime = System.currentTimeMillis();
        workUnit = this.workUnit;
    } finally {
        lock.writeLock().unlock();
    }

    try (ACLContext ctx = ACL.as2(ACL.SYSTEM2)) {
        // [3] Queue 락 내에서 Executable 생성 (원자적)
        SubTask task = Queue.callWithLock(() -> {
            // ... 노드 온라인 재확인 ...
            workUnit.setExecutor(Executor.this);
            queue.onStartExecuting(Executor.this);

            SubTask _task = workUnit.work;
            Executable _executable = _task.createExecutable();
            // ... executable 설정 ...
            workUnit.setExecutable(_executable);
            return _task;
        });

        // [4] 동기화 후 빌드 실행
        workUnit.context.synchronizeStart();
        // ... Actionable 처리 ...

        Authentication auth = workUnit.context.item.authenticate2();
        try (ACLContext context = ACL.as2(auth)) {
            queue.execute(executable, task);  // ← 실제 빌드 실행
        }

    } catch (AsynchronousExecution x) {
        // [5] 비동기 실행 처리 (Pipeline 등)
        lock.writeLock().lock();
        try {
            x.setExecutorWithoutCompleting(this);
            this.asynchronousExecution = x;
        } finally {
            lock.writeLock().unlock();
        }
        x.maybeComplete();
    } finally {
        // [6] 정리
        if (asynchronousExecution == null) {
            finish2();  // executor 제거 + queue 유지보수 스케줄
        }
    }
}
```

실행 흐름을 표로 정리:

| 단계 | 설명 | 실패 시 |
|------|------|---------|
| 1. 사전 검사 | 노드 온라인/존재 확인 | resetWorkUnit → 큐로 복귀 |
| 2. 상태 초기화 | startTime, workUnit 설정 | - |
| 3. Executable 생성 | Queue 락 내에서 원자적으로 | null이면 빌드 스킵 |
| 4. 빌드 실행 | queue.execute() 호출 | 예외 처리 후 finish1/finish2 |
| 5. 비동기 분기 | Pipeline의 AsynchronousExecution | 나중에 completedAsynchronous() |
| 6. 정리 | executor 제거, 큐 유지보수 | - |

**왜 Queue 락 내에서 Executable을 생성하는가? (단계 3)**

`Queue.callWithLock()` 안에서 `createExecutable()`을 호출하는 이유:
Executor에 작업이 할당된 후(pending 상태), 노드가 제거되면 작업을
큐로 돌려보내야 한다. 하지만 Executable이 이미 생성된 후에는 되돌릴 수 없다.
Queue 락 안에서 "노드 확인 → Executable 생성"을 원자적으로 수행하여
이 경쟁 조건을 방지한다.

### 3.5 interrupt(): 빌드 중단

```java
// hudson/model/Executor.java 라인 158~188
@Override
public void interrupt() {
    if (Thread.currentThread() == this) {
        // 자기 자신이 interrupt → 표준 처리 (InterruptedException 복원)
        super.interrupt();
    } else {
        // 다른 스레드가 interrupt → 빌드 중단
        interrupt(Result.ABORTED);
    }
}

// 라인 222~248
private void interrupt(Result result, boolean forShutdown,
                       CauseOfInterruption... causes) {
    lock.writeLock().lock();
    try {
        if (!started) {
            owner.removeExecutor(this);  // 아직 시작 안 됨 → 제거
            return;
        }
        interruptStatus = result;
        for (CauseOfInterruption c : causes) {
            if (!this.causes.contains(c))
                this.causes.add(c);
        }
        if (asynchronousExecution != null) {
            asynchronousExecution.interrupt(forShutdown);
        } else {
            super.interrupt();  // Thread.interrupt()
        }
    } finally {
        lock.writeLock().unlock();
    }
}
```

**왜 Thread.currentThread() == this를 확인하는가?**

Java의 `Thread.interrupt()`는 InterruptedException을 발생시키는 메커니즘이다.
정상적인 코드는 InterruptedException을 캐치한 후 인터럽트 플래그를 복원한다:

```java
try {
    Thread.sleep(1000);
} catch (InterruptedException e) {
    Thread.currentThread().interrupt(); // 플래그 복원
}
```

이때 `Thread.currentThread().interrupt()`는 자기 자신에 대한 호출이므로
빌드 중단이 아닌 플래그 복원이다. Jenkins는 이 두 경우를 구분하여
자기 호출은 표준 처리, 외부 호출은 빌드 중단으로 처리한다.
이 로직은 JENKINS-28690 데드락 문제를 해결하기 위해 도입되었다.

### 3.6 finish1/finish2: 정리 단계

```java
// hudson/model/Executor.java 라인 493~522
private void finish1(@CheckForNull Throwable problems) {
    if (problems != null) {
        LOGGER.log(Level.SEVERE, "Executor threw an exception", problems);
        workUnit.context.abort(problems);
    }
    long time = System.currentTimeMillis() - startTime;
    LOGGER.log(FINE, "{0} completed {1} in {2}ms",
        new Object[]{getName(), executable, time});
    try {
        workUnit.context.synchronizeEnd(this, executable, problems, time);
    } catch (InterruptedException e) {
        workUnit.context.abort(e);
    } finally {
        workUnit.setExecutor(null);
    }
}

private void finish2() {
    owner.removeExecutor(this);
    if (this instanceof OneOffExecutor) {
        owner.remove((OneOffExecutor) this);
    }
    executableEstimatedDuration = DEFAULT_ESTIMATED_DURATION;
    queue.scheduleMaintenance();  // ← 큐에 유지보수 요청
}
```

**왜 finish를 두 단계로 나누었는가?**

`finish1`은 빌드 결과 정리(동기화, 에러 처리)를, `finish2`는 executor 자체의
정리(Computer에서 제거, 큐 유지보수 트리거)를 담당한다.
비동기 실행(`AsynchronousExecution`)의 경우 `finish1`은 나중에 호출되지만
`finish2`는 항상 마지막에 호출되어야 하므로 분리되어 있다.

### 3.7 ReadWriteLock 사용 패턴

```
┌─────────────────────────────────────────────────────┐
│ Executor의 ReadWriteLock 사용                        │
├─────────────────────────────────────────────────────┤
│                                                     │
│ Read Lock (동시 접근 허용)                            │
│   - isParking()                                     │
│   - getCurrentExecutable()                          │
│   - isActive()                                      │
│   - getCurrentWorkUnit()                            │
│                                                     │
│ Write Lock (배타적 접근)                              │
│   - start(WorkUnit)                                 │
│   - interrupt(Result, ...)                           │
│   - run() 내부의 상태 변경                            │
│   - resetWorkUnit()                                 │
│                                                     │
└─────────────────────────────────────────────────────┘
```

**왜 synchronized가 아닌 ReadWriteLock인가?**

Executor의 상태(executable, workUnit, started)는 웹 UI에서 매우 빈번하게
읽힌다(빌드 진행률, 큐 상태 표시 등). 쓰기(시작, 중단)는 상대적으로 드물다.
ReadWriteLock은 읽기 동시성을 보장하면서 쓰기의 배타성을 유지한다.

---

## 4. Computer

**파일**: `hudson/model/Computer.java` (~1801줄)

### 4.1 클래스 선언

```java
// hudson/model/Computer.java 라인 173
public /*transient*/ abstract class Computer
    extends Actionable
    implements AccessControlled, IComputer, ExecutorListener,
               DescriptorByNameOwner, StaplerProxy, HasWidgets {
```

주석의 `/*transient*/`는 Computer가 직렬화 대상이 아님을 강조한다.
Computer는 런타임 상태이며, 재시작 시 Node로부터 다시 생성된다.

### 4.2 핵심 필드

```java
// hudson/model/Computer.java

private final CopyOnWriteArrayList<Executor> executors      // 라인 175
    = new CopyOnWriteArrayList<>();
private final CopyOnWriteArrayList<OneOffExecutor>           // 라인 177
    oneOffExecutors = new CopyOnWriteArrayList<>();

private int numExecutors;                                    // 라인 179
protected volatile OfflineCause offlineCause;                // 라인 184
protected String nodeName;                                   // 라인 192
private final WorkspaceList workspaceList                    // 라인 206
    = new WorkspaceList();
protected final Object statusChangeLock = new Object();      // 라인 210
```

**왜 executors에 CopyOnWriteArrayList를 사용하는가?**

Executor 목록은 웹 UI에서 매 요청마다 순회(iterate)되지만,
executor 추가/제거는 빌드 시작/종료 시에만 발생한다.
CopyOnWriteArrayList는 이런 "읽기 >>> 쓰기" 패턴에 최적화되어 있으며,
순회 중 `ConcurrentModificationException`이 발생하지 않는다.

### 4.3 Node와 Computer의 관계

```
┌─────────────┐         ┌─────────────┐
│    Node      │────────→│  Computer    │
│ (설정/영속)   │ creates │ (런타임/임시) │
├─────────────┤         ├─────────────┤
│ name         │         │ executors[]  │
│ numExecutors │         │ offlineCause │
│ mode         │         │ channel      │
│ label        │         │ workspaceList│
│ properties   │         │ nodeName     │
└─────────────┘         └─────────────┘
      │                       │
      │ 설정 변경 시            │ setNode(node)으로
      │ 새 Node 생성           │ 기존 Computer 업데이트
      v                       v
```

```java
// hudson/model/Computer.java 라인 805~813
protected void setNode(Node node) {
    assert node != null;
    if (node instanceof Slave)
        this.nodeName = node.getNodeName();
    else
        this.nodeName = null;
    setNumExecutors(node.getNumExecutors());
}
```

**왜 Node와 Computer를 분리했는가?**

1. **수명 차이**: Node는 사용자 설정이므로 영속적이다. Computer는 런타임
   상태이므로 연결/해제에 따라 생성/소멸된다. 설정이 변경되면 Node 객체는
   교체되지만, Computer는 그대로 유지될 수 있다.

2. **실행자 관리**: Node는 "몇 개의 executor를 가져야 하는가"를 정의하고,
   Computer는 "현재 실행 중인 executor들의 상태"를 관리한다.

3. **URL 바인딩**: Node에는 URL이 없고, Computer에 URL이 바인딩된다.
   (`/computer/agent-1/`) 이를 통해 에이전트의 웹 UI를 제공한다.

### 4.4 isOnline / isOffline

```java
// hudson/model/Computer.java 라인 624~626
public final boolean isOnline() {
    return !isOffline();
}
```

`isOffline()`은 하위 클래스에서 구현한다. 주요 판단 기준:

```
온라인 조건:
  ┌─ 채널(Channel)이 연결됨 (SlaveComputer)
  └─ offlineCause가 null

오프라인 사유 (OfflineCause):
  ├─ ByCLI: CLI 명령으로 오프라인 처리
  ├─ ByUser: 사용자가 UI에서 오프라인 처리
  ├─ ChannelTermination: 연결 끊김
  ├─ LaunchFailed: 에이전트 시작 실패
  └─ IdleOffline: 유휴 상태에서 RetentionStrategy에 의해 종료
```

### 4.5 SlaveComputer: 원격 에이전트 연결

```java
// hudson/slaves/SlaveComputer.java 라인 112~116
public class SlaveComputer extends Computer {
    private volatile Channel channel;
    private transient volatile boolean acceptingTasks = true;
    private Charset defaultCharset;
    private Boolean isUnix;
}
```

`Channel`은 Jenkins Remoting 라이브러리의 핵심 클래스로,
마스터와 에이전트 사이의 양방향 RPC 통신을 제공한다.

```
┌──────────────────┐     Channel (TCP)    ┌──────────────────┐
│   Master JVM     │ ←────────────────→   │   Agent JVM      │
│                  │                      │                  │
│ SlaveComputer    │  Callable 전송       │ remoting.jar     │
│   channel ───────│──────────────────→   │   Callable 실행  │
│                  │  결과 반환            │                  │
│                  │ ←────────────────    │                  │
└──────────────────┘                      └──────────────────┘
```

**왜 Channel 기반인가?**

Jenkins의 분산 빌드 아키텍처에서 에이전트는 별도 JVM 프로세스다.
Channel은 Java 객체를 직렬화하여 네트워크로 전송하고, 원격 JVM에서
실행한 결과를 다시 직렬화하여 반환한다. 이 RPC 메커니즘 덕분에
마스터에서 `FilePath`, `Launcher` 등의 API를 로컬과 동일하게 사용할 수 있다.

### 4.6 statusChangeLock

```java
// hudson/model/Computer.java 라인 210
protected final Object statusChangeLock = new Object();

// 라인 1597~1603
public void waitUntilOnline() throws InterruptedException {
    synchronized (statusChangeLock) {
        while (!isOnline())
            statusChangeLock.wait(1000);
    }
}
```

**왜 별도 객체를 락으로 사용하는가?**

`synchronized(this)`를 사용하면 Computer 객체의 모든 동기화 블록이
동일한 모니터를 공유하여 불필요한 경합이 발생한다. 상태 변경 관련
대기/통지만을 위한 전용 락 객체를 사용하여 세밀한 동시성 제어를 한다.

### 4.7 TerminationRequest: 디버깅 지원

```java
// hudson/model/Computer.java 라인 220~250
private final transient List<TerminationRequest> terminatedBy
    = Collections.synchronizedList(new ArrayList<>());

public void recordTermination() {
    StaplerRequest2 request = Stapler.getCurrentRequest2();
    if (request != null) {
        terminatedBy.add(new TerminationRequest(
            String.format("Termination requested at %s by %s [id=%d] "
                + "from HTTP request for %s",
                new Date(), Thread.currentThread(),
                Thread.currentThread().getId(),
                request.getRequestURL())));
    } else {
        terminatedBy.add(new TerminationRequest(
            String.format("Termination requested at %s by %s [id=%d]",
                new Date(), Thread.currentThread(),
                Thread.currentThread().getId())));
    }
}
```

Executor의 `resetWorkUnit()`에서 이 정보를 활용한다:

```java
// Executor.java 라인 311~317
if (owner.getTerminatedBy().isEmpty()) {
    pw.print("No termination trace available.");
} else {
    pw.println("Termination trace follows:");
    for (Computer.TerminationRequest request : owner.getTerminatedBy()) {
        Functions.printStackTrace(request, pw);
    }
}
```

**왜 스택 트레이스를 기록하는가?**

노드 제거는 여러 경로(UI, CLI, API, Cloud 관리)에서 발생할 수 있다.
Executor가 작업을 할당받은 후 노드가 사라지면 원인 추적이 어렵다.
TerminationRequest에 스택 트레이스를 기록하여 "누가, 언제, 어떤 경로로"
노드를 제거했는지 사후에 진단할 수 있게 한다.

---

## 5. Node

**파일**: `hudson/model/Node.java` (~682줄)

### 5.1 클래스 선언

```java
// hudson/model/Node.java 라인 107
public abstract class Node extends AbstractModelObject
    implements ReconfigurableDescribable<Node>, ExtensionPoint,
               AccessControlled, OnMaster, PersistenceRoot {
```

핵심 인터페이스의 역할:

| 인터페이스 | 역할 |
|-----------|------|
| `ReconfigurableDescribable` | 웹 UI에서 재설정 가능한 설명자(Descriptor) 패턴 |
| `ExtensionPoint` | 플러그인으로 새로운 Node 타입 추가 가능 |
| `AccessControlled` | 노드별 접근 제어 |
| `OnMaster` | 마스터 JVM에서만 존재하는 객체 (에이전트로 직렬화되지 않음) |
| `PersistenceRoot` | XML 직렬화의 루트 (config.xml) |

### 5.2 설정만 보유 (런타임 상태 없음)

Node의 핵심 원칙은 **설정만 보유**한다는 것이다:

```java
// Node.java - Javadoc (라인 95~96)
// "Nodes are persisted objects that capture user configurations,
//  and instances get thrown away and recreated whenever
//  the configuration changes."
```

Node가 보유하는 것:

```
Node (설정 = 영속적)
├─ nodeName          : 노드 이름
├─ nodeDescription   : 설명
├─ numExecutors      : executor 수
├─ mode              : NORMAL / EXCLUSIVE
├─ labelString       : 레이블 (예: "linux docker")
├─ nodeProperties    : 속성 목록
└─ temporaryOfflineCause : 일시적 오프라인 사유
```

Node가 보유하지 않는 것:

```
Computer (런타임 = 임시)
├─ executors[]       : 실행 중인 executor 목록
├─ channel           : 원격 연결
├─ offlineCause      : 오프라인 사유
├─ cachedHostName    : 호스트명
└─ cachedEnvironment : 환경변수
```

### 5.3 toComputer(): 대응 Computer 반환

```java
// hudson/model/Node.java 라인 222~226
@CheckForNull
public final Computer toComputer() {
    AbstractCIBase ciBase = Jenkins.get();
    return ciBase.getComputer(this);
}
```

`Jenkins.computers` 맵(ConcurrentHashMap<Node, Computer>)에서
현재 Node에 대응하는 Computer를 조회한다. 반환값이 null일 수 있는 경우:

- Node의 executor 수가 0인 경우 (Computer가 생성되지 않음)
- Node가 방금 추가되어 Computer가 아직 생성되지 않은 경우

### 5.4 createComputer(): Computer 팩토리

```java
// hudson/model/Node.java 라인 247~248
@CheckForNull
@Restricted(ProtectedExternally.class)
protected abstract Computer createComputer();
```

`Jenkins.updateComputerList()`에서만 호출되어야 한다.
Node의 하위 클래스가 자신에게 맞는 Computer를 생성한다:

| Node 하위 클래스 | Computer 하위 클래스 |
|-----------------|---------------------|
| Jenkins (마스터) | Jenkins.MasterComputer |
| Slave | SlaveComputer |
| DumbSlave | SlaveComputer |
| 플러그인별 Node | 플러그인별 Computer |

### 5.5 getWorkspaceFor(): 워크스페이스 경로

```java
// hudson/model/Node.java 라인 483
public abstract @CheckForNull FilePath getWorkspaceFor(TopLevelItem item);
```

Node마다 워크스페이스 위치가 다르다:

```
마스터: $JENKINS_HOME/workspace/{job-name}
에이전트: {agent-root}/workspace/{job-name}
```

`FilePath`는 로컬/원격 파일시스템을 투명하게 추상화하는 Jenkins의 핵심 클래스다.
마스터에서는 직접 파일 접근, 에이전트에서는 Channel을 통한 원격 파일 접근을 한다.

### 5.6 canTake(): 작업 수용 가능 여부

```java
// Node.java (Slave.java에서 구현)
public CauseOfBlockage canTake(BuildableItem item) {
    // Label 매칭 확인
    // Mode 확인 (EXCLUSIVE이면 라벨 일치 필수)
    // NodeProperty의 canTake() 확인
}
```

Queue.maintain()에서 JobOffer.getCauseOfBlockage()를 통해 호출된다.
Node 수준에서 작업을 거부할 수 있는 이유:

```
거부 사유 (CauseOfBlockage):
├─ BecauseLabelIsOffline: 요구 라벨의 노드가 모두 오프라인
├─ BecauseLabelIsBusy: 요구 라벨의 노드가 모두 사용 중
├─ BecauseNodeIsOffline: 지정된 노드가 오프라인
├─ BecauseNodeIsBusy: 지정된 노드가 사용 중
└─ BecauseNodeIsNotAcceptingTasks: RetentionStrategy에 의해 거부
```

### 5.7 Jenkins 마스터도 Node

```java
// Node.java Javadoc (라인 92~93)
// "As a special case, Jenkins extends from here."
```

Jenkins 클래스의 상속 계층:

```
Node
  └─ AbstractCIBase extends Node
       └─ Jenkins extends AbstractCIBase
```

이 설계의 결과:

```java
// Jenkins도 Node의 메서드를 가진다
Jenkins.get().getNumExecutors();    // 마스터의 executor 수
Jenkins.get().getMode();            // NORMAL or EXCLUSIVE
Jenkins.get().toComputer();         // MasterComputer 반환
Jenkins.get().getSelfLabel();       // "built-in" 라벨
```

### 5.8 Mode 열거형

```java
public enum Mode {
    NORMAL,     // 라벨이 일치하지 않아도 실행 가능
    EXCLUSIVE   // 라벨이 명시적으로 일치할 때만 실행
}
```

EXCLUSIVE 모드는 특수 용도의 에이전트(예: GPU 노드, macOS 빌드 노드)를
지정된 Job만 사용하도록 제한한다. NORMAL 모드는 라벨이 일치하지 않아도
빈 executor가 있으면 빌드를 수용한다.

---

## 6. PluginManager

**파일**: `hudson/PluginManager.java` (~2697줄)

### 6.1 클래스 선언

```java
// hudson/PluginManager.java 라인 205
public abstract class PluginManager extends AbstractModelObject
    implements OnMaster, StaplerOverridable, StaplerProxy {
```

### 6.2 핵심 필드

```java
// hudson/PluginManager.java

// 발견된 모든 플러그인
protected final List<PluginWrapper> plugins                   // 라인 325
    = new CopyOnWriteArrayList<>();

// 활성 플러그인 (위상 정렬: Y가 X에 의존하면 Y가 X 뒤에 위치)
protected final List<PluginWrapper> activePlugins             // 라인 330
    = new CopyOnWriteArrayList<>();

// 로드 실패한 플러그인
protected final List<FailedPlugin> failedPlugins              // 라인 332
    = new ArrayList<>();

// 플러그인 디렉토리
public final File rootDir;                                    // 라인 337

// 통합 클래스로더
public final ClassLoader uberClassLoader                      // 라인 367
    = new UberClassLoader(activePlugins);

// 플러그인 전략
private final PluginStrategy strategy;                        // 라인 388
```

### 6.3 플러그인 로딩 프로세스

PluginManager의 `initTasks()` 메서드(라인 445~)가 반환하는 `TaskBuilder`는
Jenkins의 Reactor 프레임워크를 통해 단계별로 실행된다:

```
초기화 단계 (InitMilestone 기준)
═══════════════════════════════════════════════

[PLUGINS_LISTED]
  1. loadBundledPlugins()
     └─ WAR 내장 플러그인을 plugins/ 디렉토리에 추출

  2. listPluginArchives()
     └─ plugins/ 디렉토리에서 *.hpi, *.jpi 파일 목록 수집

  3. Inspecting plugins (병렬)
     └─ 각 아카이브의 매니페스트를 읽어 PluginWrapper 생성
     └─ 중복 검사 (같은 shortName의 플러그인이 여러 버전)

  4. Checking cyclic dependencies
     └─ CyclicGraphDetector로 순환 의존성 감지
     └─ 위상 정렬(topological sort)로 activePlugins 구성

[PLUGINS_PREPARED]
  5. ClassLoader 준비
     └─ 각 플러그인에 독립 ClassLoader 생성
     └─ 의존성에 따른 ClassLoader 체인 구성

[PLUGINS_STARTED]
  6. Plugin.start() 호출
     └─ 각 플러그인의 초기화 로직 실행
```

코드에서 위상 정렬 부분:

```java
// PluginManager.java 라인 508~549
g.followedBy().attains(PLUGINS_LISTED)
    .add("Checking cyclic dependencies", new Executable() {
        @Override
        public void run(Reactor reactor) throws Exception {
            CyclicGraphDetector<PluginWrapper> cgd = new CyclicGraphDetector<>() {
                @Override
                protected List<PluginWrapper> getEdges(PluginWrapper p) {
                    List<PluginWrapper> next = new ArrayList<>();
                    addTo(p.getDependencies(), next);
                    addTo(p.getOptionalDependencies(), next);
                    return next;
                }

                @Override
                protected void reactOnCycle(PluginWrapper q,
                                           List<PluginWrapper> cycle) {
                    LOGGER.log(Level.SEVERE,
                        "found cycle in plugin dependencies: ...");
                    for (PluginWrapper pw : cycle) {
                        pw.setHasCycleDependency(true);
                        failedPlugins.add(new FailedPlugin(pw, ...));
                    }
                }
            };
            cgd.run(getPlugins());

            // 위상 정렬 결과를 activePlugins에 저장
            for (PluginWrapper p : cgd.getSorted()) {
                if (p.isActive()) {
                    activePlugins.add(p);
                }
            }
        }
    });
```

**왜 위상 정렬이 필요한가?**

플러그인 A가 플러그인 B에 의존하면, B의 ClassLoader가 먼저 준비되어야
A의 클래스를 로드할 수 있다. 위상 정렬은 이 의존성 순서를 보장한다.
순환 의존성이 감지되면 관련 플러그인 모두를 비활성화하여 시스템 안정성을
보호한다.

### 6.4 UberClassLoader: 통합 클래스로더

```java
// PluginManager.java 라인 367
public final ClassLoader uberClassLoader = new UberClassLoader(activePlugins);
```

UberClassLoader는 모든 활성 플러그인의 클래스를 하나의 인터페이스로 접근할 수 있게 한다:

```
UberClassLoader (통합)
├─ Jenkins Core ClassLoader
├─ Plugin A ClassLoader
│   └─ 의존: Jenkins Core
├─ Plugin B ClassLoader
│   └─ 의존: Jenkins Core, Plugin A
└─ Plugin C ClassLoader
    └─ 의존: Jenkins Core
```

클래스 로딩 순서:

```
1. UberClassLoader.loadClass("com.example.MyClass")
2. Jenkins Core ClassLoader에서 검색
3. 각 활성 플러그인의 ClassLoader에서 검색 (순서는 위상 정렬 기반)
4. 찾지 못하면 ClassNotFoundException
```

**왜 단일 UberClassLoader가 필요한가?**

XStream(XML 직렬화), Jelly(UI 렌더링), Stapler(URL 라우팅) 등의
프레임워크가 클래스를 이름으로 로드할 때, 어느 플러그인에 속한 클래스인지
미리 알 수 없다. UberClassLoader가 모든 플러그인 클래스에 대한
단일 진입점을 제공하여 이 문제를 해결한다.

### 6.5 플러그인 격리: ClassLoader 계층

각 플러그인은 독립된 ClassLoader를 가진다:

```
                    ┌─────────────────┐
                    │  Java Platform   │
                    │  ClassLoader     │
                    └────────┬────────┘
                             │
                    ┌────────┴────────┐
                    │  Jenkins Core    │
                    │  ClassLoader     │
                    └────────┬────────┘
                             │
              ┌──────────────┼──────────────┐
              │              │              │
     ┌────────┴───┐  ┌──────┴─────┐  ┌─────┴──────┐
     │ Plugin A   │  │ Plugin B   │  │ Plugin C   │
     │ ClassLoader│  │ ClassLoader│  │ ClassLoader│
     └────────────┘  └────────────┘  └────────────┘
           │                │
           │  B가 A에 의존   │
           └────────────────┘
           A의 ClassLoader를 parent로 참조
```

**왜 플러그인별 ClassLoader 격리가 필요한가?**

1. **버전 충돌 방지**: Plugin A가 Guava 30을, Plugin B가 Guava 31을
   사용하더라도 각자의 ClassLoader에서 다른 버전을 로드할 수 있다.

2. **동적 로딩/언로딩**: 플러그인을 런타임에 추가/제거할 때
   해당 ClassLoader만 GC하면 된다.

3. **보안**: 플러그인이 서로의 내부 클래스에 직접 접근하지 못하도록 제한한다.

### 6.6 createDefault(): 커스텀 PluginManager

```java
// PluginManager.java 라인 299~320
public static @NonNull PluginManager createDefault(@NonNull Jenkins jenkins) {
    String pmClassName = SystemProperties.getString(CUSTOM_PLUGIN_MANAGER);
    if (pmClassName != null && !pmClassName.isBlank()) {
        // 시스템 속성으로 지정된 커스텀 PluginManager 클래스 로드
        final Class<? extends PluginManager> klass =
            Class.forName(pmClassName).asSubclass(PluginManager.class);
        for (PMConstructor c : PMConstructor.values()) {
            PluginManager pm = c.create(klass, jenkins);
            if (pm != null) return pm;
        }
    }
    return new LocalPluginManager(jenkins);
}
```

생성자 탐색 순서:

| 우선순위 | 생성자 시그니처 | 용도 |
|---------|---------------|------|
| 1 | `(Jenkins)` | Jenkins 인스턴스 접근 |
| 2 | `(ServletContext, File)` | 서블릿 컨텍스트 + 홈 디렉토리 |
| 3 | `(File)` | 홈 디렉토리만 필요 |

**왜 커스텀 PluginManager를 지원하는가?**

테스트 환경에서 플러그인 로딩을 모킹하거나, 특수한 배포 환경(컨테이너,
읽기 전용 파일시스템)에서 플러그인 관리 방식을 변경하기 위해서다.
시스템 속성으로 지정하므로 플러그인보다 먼저 로드되어야 한다.

---

## 7. ExtensionList

**파일**: `hudson/ExtensionList.java` (~512줄)

### 7.1 클래스 선언

```java
// hudson/ExtensionList.java 라인 64
public class ExtensionList<T> extends AbstractList<T> implements OnMaster {
```

`AbstractList<T>`를 상속하여 표준 Java List 인터페이스를 제공한다.
외부에서는 일반 List처럼 사용하되, 내부에서는 확장 발견과 캐싱을 수행한다.

### 7.2 핵심 필드

```java
// hudson/ExtensionList.java

public final @CheckForNull Jenkins jenkins;               // 라인 71
public final Class<T> extensionType;                      // 라인 72

// 발견된 확장 목록 (지연 로딩)
@CopyOnWrite
private volatile List<ExtensionComponent<T>> extensions;  // 라인 78

// 리스너
private final List<ExtensionListListener> listeners       // 라인 80
    = new CopyOnWriteArrayList<>();

// 수동 등록 인스턴스 (레거시)
private final CopyOnWriteArrayList<ExtensionComponent<T>> // 라인 86
    legacyInstances;
```

### 7.3 지연 로딩: ensureLoaded()

```java
// hudson/ExtensionList.java 라인 300~314
private List<ExtensionComponent<T>> ensureLoaded() {
    if (extensions != null)
        return extensions;  // 이미 로드됨

    if (jenkins == null ||
        jenkins.getInitLevel().compareTo(InitMilestone.PLUGINS_PREPARED) < 0)
        return legacyInstances;  // 플러그인 준비 안 됨 → 레거시만 반환

    synchronized (getLoadLock()) {
        if (extensions == null) {
            List<ExtensionComponent<T>> r = load();
            r.addAll(legacyInstances);
            extensions = sort(r);
        }
        return extensions;
    }
}
```

**왜 지연 로딩인가?**

Jenkins 초기화 과정에서 ExtensionList가 생성되지만, 이 시점에 모든 플러그인이
준비되지 않았을 수 있다. `ensureLoaded()`는 첫 접근 시에 확장을 발견하며,
`PLUGINS_PREPARED` 마일스톤 이전에는 수동 등록된 레거시 인스턴스만 반환한다.

### 7.4 load(): 확장 발견

```java
protected List<ExtensionComponent<T>> load() {
    // ExtensionFinder를 통해 @Extension 어노테이션이 붙은
    // extensionType의 모든 구현체를 발견
}
```

발견 메커니즘 체인:

```
ExtensionList.load()
  └─ ExtensionFinder.find(extensionType, Jenkins)
       ├─ GuiceFinder (Guice DI 컨테이너에서 발견)
       │   └─ @Extension 어노테이션 스캔
       └─ 기타 커스텀 ExtensionFinder
```

### 7.5 ExtensionFinder: @Extension 구현 발견

```java
// hudson/ExtensionFinder.java 라인 87
public abstract class ExtensionFinder implements ExtensionPoint {
    public abstract <T> Collection<ExtensionComponent<T>>
        find(Class<T> type, Hudson hudson);
}
```

`ExtensionFinder` 자체가 `ExtensionPoint`다. 이것은 "확장을 발견하는 방법"
자체도 확장 가능하다는 의미다.

```
@Extension 어노테이션 예시:

@Extension
public class GitSCM extends SCM {
    // Git 소스 관리 구현
}

→ ExtensionList<SCM>에 자동 등록됨
```

### 7.6 CopyOnWrite 패턴

```java
// ExtensionList.java 라인 78
@CopyOnWrite
private volatile List<ExtensionComponent<T>> extensions;
```

`extensions` 필드는 `volatile`이며 참조 교체(CopyOnWrite) 방식으로 업데이트된다:

```java
// 라인 228~235 (removeSync)
private synchronized boolean removeSync(Object o) {
    boolean removed = removeComponent(legacyInstances, o);
    if (extensions != null) {
        List<ExtensionComponent<T>> r = new ArrayList<>(extensions); // 복사
        removed |= removeComponent(r, o);                           // 수정
        extensions = sort(r);                                        // 교체
    }
    return removed;
}
```

**왜 CopyOnWrite인가?**

ExtensionList는 거의 변경되지 않지만(플러그인 동적 로드 시에만) 매우 빈번하게
읽힌다(Descriptor 조회, 확장 포인트 순회 등). CopyOnWrite는 읽기에 대해
동기화 없이 안전한 접근을 제공하고, 쓰기 시에만 새 리스트를 생성하여 교체한다.

### 7.7 refresh(): 동적 플러그인 로드

```java
// hudson/ExtensionList.java 라인 329~349
public boolean refresh(ExtensionComponentSet delta) {
    synchronized (getLoadLock()) {
        if (extensions == null)
            return false;

        Collection<ExtensionComponent<T>> newComponents = load(delta);
        if (!newComponents.isEmpty()) {
            List<ExtensionComponent<T>> components = new ArrayList<>(extensions);
            Set<T> instances = Collections.newSetFromMap(new IdentityHashMap<>());
            for (ExtensionComponent<T> component : components) {
                instances.add(component.getInstance());
            }
            boolean fireListeners = false;
            for (ExtensionComponent<T> newComponent : newComponents) {
                // 중복 확인 후 추가
            }
        }
    }
}
```

Jenkins 런타임 중 새 플러그인이 설치되면 `Jenkins.refreshExtensions()`가 호출되고,
각 ExtensionList의 `refresh()`가 새로 발견된 확장을 기존 리스트에 추가한다.

**왜 중복 검사가 필요한가?**

동적 로드 시 플러그인 A의 @Extension 클래스가 생성자에서 다른 ExtensionList를
조회하면, 해당 리스트의 refresh가 먼저 실행될 수 있다. 이때 같은 확장이
두 번 등록되는 것을 `IdentityHashMap` 기반의 중복 검사로 방지한다.

### 7.8 lookup 정적 메서드

ExtensionList 사용을 위한 편의 메서드:

```java
// 특정 타입의 ExtensionList 획득
ExtensionList<SCM> scmList = ExtensionList.lookup(SCM.class);

// 싱글턴 확장 조회
GitSCM git = ExtensionList.lookupSingleton(GitSCM.class);

// 첫 번째 구현 조회
SCM first = ExtensionList.lookupFirst(SCM.class);
```

이 패턴은 서비스 로케이터(Service Locator) 패턴의 Jenkins 구현이다.

---

## 8. 컴포넌트 관계 다이어그램

### 8.1 소유 관계 (Ownership)

```
┌──────────────────────────────────────────────────────────────┐
│                        Jenkins 싱글턴                         │
│                    (시스템 루트 객체)                          │
│                                                              │
│  ┌─────────┐  ┌──────────────┐  ┌──────────────────┐        │
│  │  Queue   │  │ PluginManager│  │  SecurityRealm    │        │
│  │ (빌드 큐)│  │ (플러그인)   │  │ (인증)             │        │
│  └────┬────┘  └──────┬───────┘  └──────────────────┘        │
│       │              │                                       │
│  ┌────┴────────┐  ┌──┴─────────────┐                        │
│  │ LoadBalancer│  │ ExtensionList[] │                        │
│  │ QueueSorter│  │ (확장 레지스트리)│                        │
│  └────────────┘  └─────────────────┘                        │
│                                                              │
│  ┌──────────────────────┐  ┌────────────────────────┐       │
│  │ Nodes (설정 관리)     │  │ computers (런타임 맵)   │       │
│  │ ConcurrentMap         │  │ ConcurrentHashMap       │       │
│  │ <String, Node>        │  │ <Node, Computer>        │       │
│  └──────────────────────┘  └────────────────────────┘       │
│                                                              │
│  ┌──────────────────────────────────────────┐               │
│  │ AuthorizationStrategy  │ UpdateCenter     │               │
│  │ (인가)                 │ (업데이트 센터)   │               │
│  └──────────────────────────────────────────┘               │
└──────────────────────────────────────────────────────────────┘
```

### 8.2 큐-실행 파이프라인

```
사용자가 빌드 트리거
        │
        v
┌───────────────────────────────────────────────────────┐
│                      Queue                             │
│                                                       │
│  [WaitingItem] ──→ [BlockedItem] ──→ [BuildableItem]  │
│       │                                    │          │
│       └──── quiet period ────┘             │          │
│                                            │          │
│            LoadBalancer.map()              │          │
│                   │                        │          │
│                   v                        │          │
│            [Pending] ─── JobOffer.set() ──→│          │
│                              │                        │
└──────────────────────────────│────────────────────────┘
                               │
                               v
                     Executor.start(WorkUnit)
                               │
                               v
                     Executor.run()
                       │
                       ├─ [사전 검사] 노드 온라인?
                       │
                       ├─ [Executable 생성] Queue 락 내
                       │
                       ├─ [빌드 실행] queue.execute()
                       │
                       ├─ [finish1] 결과 정리
                       │
                       └─ [finish2] executor 제거
                              │
                              v
                     queue.scheduleMaintenance()
                     (다음 빌드 시작 트리거)
```

### 8.3 Node-Computer-Executor 계층

```
┌──────────────────────────────────────────────────────────┐
│                                                          │
│  Node (설정)           Computer (런타임)    Executor[]    │
│  ═══════════          ═════════════════    ═══════════    │
│                                                          │
│  ┌──────────┐         ┌──────────────┐    ┌──────────┐  │
│  │ Jenkins  │────────→│MasterComputer│───→│Executor 0│  │
│  │ (마스터) │ creates │              │    │Executor 1│  │
│  └──────────┘         └──────────────┘    └──────────┘  │
│                                                          │
│  ┌──────────┐         ┌──────────────┐    ┌──────────┐  │
│  │DumbSlave │────────→│SlaveComputer │───→│Executor 0│  │
│  │"agent-1" │ creates │  channel ────│──→ │Executor 1│  │
│  └──────────┘         │  offlineCause│    │Executor 2│  │
│                       └──────────────┘    └──────────┘  │
│                                                          │
│  ┌──────────┐         ┌──────────────┐    ┌──────────┐  │
│  │CloudSlave│────────→│SlaveComputer │───→│Executor 0│  │
│  │"cloud-2" │ creates │  channel ────│──→ └──────────┘  │
│  └──────────┘         └──────────────┘                   │
│                                                          │
└──────────────────────────────────────────────────────────┘
```

### 8.4 플러그인-확장 계층

```
┌─────────────────────────────────────────────────────────┐
│                    PluginManager                         │
│                                                         │
│  plugins/                                               │
│  ├─ git.jpi ──→ PluginWrapper ──→ ClassLoader           │
│  │                                    │                 │
│  │              ┌─────────────────────┘                 │
│  │              │                                       │
│  │              v                                       │
│  │         @Extension                                   │
│  │         GitSCM ──→ ExtensionList<SCM>에 등록          │
│  │                                                      │
│  ├─ workflow-api.jpi ──→ PluginWrapper ──→ ClassLoader  │
│  │                                                      │
│  └─ ...                                                 │
│                                                         │
│  UberClassLoader (모든 ClassLoader 통합)                  │
│                                                         │
│  ExtensionFinder                                        │
│  └─ @Extension 스캔 ──→ ExtensionList<T> 등록            │
│                                                         │
└─────────────────────────────────────────────────────────┘
```

### 8.5 빌드 생명주기 전체 흐름

```
[사용자/SCM 트리거]
        │
        v
[1] Jenkins.getQueue().schedule(task, quietPeriod)
        │
        v
[2] WaitingItem 생성, waitingList에 추가
        │ (quiet period 경과)
        v
[3] maintain(): waiting → blocked 또는 buildable
        │
        │ (차단 조건 없음)
        v
[4] maintain(): BuildableItem, LoadBalancer.map()
        │
        │ (적합한 executor 발견)
        v
[5] JobOffer.set(WorkUnit) → Executor.start(WorkUnit)
        │
        v
[6] Executor.run()
        │
        ├─ Queue.callWithLock: createExecutable()
        │
        ├─ synchronizeStart()
        │
        ├─ queue.execute(executable, task)
        │     │
        │     └─ Run.execute() → Build 수행
        │
        ├─ finish1(): synchronizeEnd()
        │
        └─ finish2(): removeExecutor(), scheduleMaintenance()
                │
                v
        [다음 빌드를 위해 Queue.maintain() 재실행]
```

---

## 핵심 설계 원칙 요약

| 원칙 | 적용 사례 | 이유 |
|------|----------|------|
| **싱글턴** | Jenkins 클래스 | 시스템의 유일한 루트 객체, 전역 접근 필요 |
| **설정과 런타임 분리** | Node vs Computer | 수명 주기가 다른 관심사를 분리 |
| **CopyOnWrite** | items, executors, extensions | 읽기 >>> 쓰기 패턴에 최적화 |
| **Snapshot** | Queue.Snapshot | 락 없는 읽기로 성능 보장 |
| **지연 로딩** | ExtensionList.ensureLoaded() | 초기화 순서 의존성 해결 |
| **확장 포인트** | ExtensionPoint + @Extension | 플러그인으로 모든 동작 교체 가능 |
| **ClassLoader 격리** | 플러그인별 ClassLoader | 버전 충돌 방지, 동적 로딩 |
| **ReentrantLock** | Queue, Executor | synchronized 대비 정밀한 제어 |
| **ReadWriteLock** | Executor | 읽기 동시성 보장 |
| **위상 정렬** | 플러그인 의존성 | 로딩 순서 보장, 순환 감지 |
| **Thread 상속** | Executor extends Thread | 역사적 호환성 (플러그인 의존) |
| **Stapler 루트** | Jenkins → URL 라우팅 | 객체 트리 = URL 트리 |

이 7개 컴포넌트가 Jenkins의 핵심 골격이다. Queue가 빌드를 스케줄링하고,
LoadBalancer가 노드를 선택하고, Executor가 빌드를 실행하고,
Computer가 런타임 상태를 관리하고, PluginManager가 확장을 로드하고,
ExtensionList가 확장을 발견하며, Jenkins 싱글턴이 이 모든 것을 소유하고 조율한다.
