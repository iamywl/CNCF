# Jenkins 데이터 모델 심층 분석

## 목차

1. [개요](#1-개요)
2. [Job 계층 구조](#2-job-계층-구조)
3. [Run 계층 구조](#3-run-계층-구조)
4. [Result 클래스](#4-result-클래스)
5. [Queue 시스템](#5-queue-시스템)
6. [Node / Computer / Executor](#6-node--computer--executor)
7. [Action / Actionable 패턴](#7-action--actionable-패턴)
8. [View 시스템](#8-view-시스템)
9. [Descriptor / Describable 패턴](#9-descriptor--describable-패턴)
10. [영속화(Persistence) 메커니즘](#10-영속화persistence-메커니즘)
11. [JENKINS_HOME 파일시스템 구조](#11-jenkins_home-파일시스템-구조)
12. [데이터 흐름: 빌드 생명주기](#12-데이터-흐름-빌드-생명주기)
13. [핵심 인터페이스와 타입 관계](#13-핵심-인터페이스와-타입-관계)

---

## 1. 개요

Jenkins의 데이터 모델은 Java의 상속 계층과 제네릭을 적극 활용하여 설계되었다. 핵심 개념은 다음 네 가지로 요약된다:

| 개념 | 클래스 | 역할 |
|------|--------|------|
| **작업(Job)** | `Job<JobT, RunT>` | 빌드 대상 정의 |
| **실행(Run)** | `Run<JobT, RunT>` | 빌드 실행 기록 |
| **큐(Queue)** | `Queue` | 빌드 요청 스케줄링 |
| **노드(Node)** | `Node` / `Computer` / `Executor` | 빌드 실행 인프라 |

이 문서에서 참조하는 모든 클래스명, 필드명, 메서드명은 Jenkins 소스코드에서 직접 확인한 것이다.

**소스코드 기준 경로**: `jenkins/core/src/main/java/hudson/model/`

---

## 2. Job 계층 구조

### 2.1 Job 클래스 정의

```
소스: core/src/main/java/hudson/model/Job.java (약 1731줄)
```

```java
public abstract class Job<JobT extends Job<JobT, RunT>, RunT extends Run<JobT, RunT>>
        extends AbstractItem implements ExtensionPoint, StaplerOverridable,
        ModelObjectWithChildren, HasWidgets
```

**재귀적 제네릭 바인딩(F-bounded polymorphism)**이 핵심이다. `JobT`와 `RunT`가 서로를 참조하면서, 하위 클래스에서 타입 안전한 API를 제공한다. 예를 들어 `FreeStyleProject`에서 `getLastBuild()`를 호출하면 `Run`이 아닌 `FreeStyleBuild`를 반환한다.

### 2.2 Job의 핵심 필드

```java
// 다음 빌드 번호 — 별도 파일(nextBuildNumber)에 저장
// config.xml과 분리하여 VCS에 설정을 넣을 수 있게 함
protected transient volatile int nextBuildNumber = 1;

// 복사된 Job은 설정이 저장될 때까지 빌드 보류
private transient volatile boolean holdOffBuildUntilSave;
private transient volatile boolean holdOffBuildUntilUserSave;

// 빌드 기록 정리 정책 (현재 BuildDiscarderProperty로 대체)
@Deprecated
private volatile BuildDiscarder logRotator;

// 프로젝트에 설정된 속성 목록
protected CopyOnWriteList<JobProperty<? super JobT>> properties = new CopyOnWriteList<>();

// 의존성 유지 여부
boolean keepDependencies;
```

**핵심 설계 결정: nextBuildNumber의 분리**

`nextBuildNumber`는 빌드마다 증가하므로 자주 변경된다. 이를 `config.xml`과 분리하여 별도 파일로 저장함으로써, Job 설정을 VCS로 관리할 수 있게 했다. `onLoad()` 메서드에서 이 값을 읽어들인다:

```java
TextFile f = getNextBuildNumberFile();
if (f.exists()) {
    synchronized (this) {
        this.nextBuildNumber = Integer.parseInt(f.readTrim());
    }
}
```

### 2.3 상속 계층 다이어그램

```
AbstractItem (이름, URL, 부모 그룹 관리)
  └── Job<JobT, RunT> (빌드 번호, 속성, 빌드 기록)
        └── AbstractProject<P, R> (SCM, 트리거, 노드 할당)
              └── Project<P, B> (Builder, Publisher, BuildWrapper)
                    └── FreeStyleProject (구체 구현)
```

### 2.4 AbstractProject

```
소스: core/src/main/java/hudson/model/AbstractProject.java (약 2163줄)
```

```java
public abstract class AbstractProject<P extends AbstractProject<P, R>,
                                       R extends AbstractBuild<P, R>>
    extends Job<P, R>
    implements BuildableItem, LazyBuildMixIn.LazyLoadingJob<P, R>,
               ParameterizedJobMixIn.ParameterizedJob<P, R>
```

Job에 SCM, 빌드 실행, 스케줄링 기능을 추가한다.

**핵심 필드:**

```java
// SCM 연동
private volatile SCM scm = new NullSCM();
private volatile SCMCheckoutStrategy scmCheckoutStrategy;

// Quiet Period — 빌드 요청 후 대기 시간 (초)
private volatile Integer quietPeriod = null;

// 빌드 실행 노드 지정 (null이면 마스터)
private String assignedNode;
private volatile boolean canRoam;         // 어느 노드에서든 실행 가능 여부

// 빌드 비활성화
protected volatile boolean disabled;

// 동시 빌드 허용
private boolean concurrentBuild;

// 업스트림/다운스트림 빌드 중 차단
protected volatile boolean blockBuildWhenDownstreamBuilding = false;
protected volatile boolean blockBuildWhenUpstreamBuilding = false;

// 트리거 목록
protected volatile DescribableList<Trigger<?>, TriggerDescriptor> triggers
    = new DescribableList<>(this);

// 임시 Action 목록 (설정 변경 시 갱신)
@CopyOnWrite
protected transient volatile List<Action> transientActions = new Vector<>();
```

### 2.5 Project

```
소스: core/src/main/java/hudson/model/Project.java
```

```java
public abstract class Project<P extends Project<P, B>, B extends Build<P, B>>
    extends AbstractProject<P, B>
    implements SCMTriggerItem, Saveable, ProjectWithMaven, BuildableItemWithBuildWrappers
```

빌드 파이프라인의 세 가지 확장점(Builder, Publisher, BuildWrapper)을 관리한다:

```java
// 빌드 단계 — 실제 빌드 작업 수행 (컴파일, 테스트 등)
private volatile DescribableList<Builder, Descriptor<Builder>> builders;

// 빌드 후 작업 — 결과 보고, 알림 등
private volatile DescribableList<Publisher, Descriptor<Publisher>> publishers;

// 빌드 래퍼 — 빌드 전/후 환경 설정 (환경 변수, 타임스탬프 등)
private volatile DescribableList<BuildWrapper, Descriptor<BuildWrapper>> buildWrappers;
```

### 2.6 FreeStyleProject

```
소스: core/src/main/java/hudson/model/FreeStyleProject.java
```

```java
public class FreeStyleProject extends Project<FreeStyleProject, FreeStyleBuild>
    implements TopLevelItem
```

Jenkins에서 가장 기본적인 프로젝트 타입이다. `Project`의 제네릭 매개변수를 구체 타입으로 바인딩한 것이 전부이며, 추가 필드는 없다. `TopLevelItem` 인터페이스를 구현하여 Jenkins 최상위에 직접 생성할 수 있는 항목임을 표시한다.

### 2.7 Job 계층 전체 클래스 관계

```
                     ┌─────────────────────┐
                     │   AbstractItem       │
                     │  ─ name: String      │
                     │  ─ parent: ItemGroup │
                     └─────────┬───────────┘
                               │ extends
                     ┌─────────▼───────────────────────┐
                     │   Job<JobT, RunT>                │
                     │  ─ nextBuildNumber: int          │
                     │  ─ logRotator: BuildDiscarder    │
                     │  ─ properties: CopyOnWriteList   │
                     │  ─ keepDependencies: boolean     │
                     └─────────┬───────────────────────┘
                               │ extends
                     ┌─────────▼───────────────────────────────┐
                     │   AbstractProject<P, R>                  │
                     │  ─ scm: SCM                              │
                     │  ─ triggers: DescribableList<Trigger>    │
                     │  ─ assignedNode: String                  │
                     │  ─ disabled: boolean                     │
                     │  ─ concurrentBuild: boolean              │
                     │  ─ quietPeriod: Integer                  │
                     │  ─ blockBuildWhenUpstreamBuilding: bool  │
                     │  ─ blockBuildWhenDownstreamBuilding: bool│
                     └─────────┬───────────────────────────────┘
                               │ extends
                     ┌─────────▼───────────────────────────────┐
                     │   Project<P, B>                          │
                     │  ─ builders: DescribableList<Builder>    │
                     │  ─ publishers: DescribableList<Publisher>│
                     │  ─ buildWrappers: DescribableList        │
                     └─────────┬───────────────────────────────┘
                               │ extends
                     ┌─────────▼───────────────┐
                     │   FreeStyleProject       │
                     │   (구체 타입 바인딩)       │
                     └─────────────────────────┘
```

---

## 3. Run 계층 구조

### 3.1 Run 클래스 정의

```
소스: core/src/main/java/hudson/model/Run.java (약 2698줄)
```

```java
public abstract class Run<JobT extends Job<JobT, RunT>, RunT extends Run<JobT, RunT>>
        extends Actionable
        implements ExtensionPoint, Comparable<RunT>, AccessControlled,
                   PersistenceRoot, DescriptorByNameOwner, OnMaster,
                   StaplerProxy, HistoricalBuild, WithConsoleUrl
```

Run은 하나의 빌드 실행 기록을 나타낸다. Job과 동일한 F-bounded polymorphism 패턴을 사용한다.

### 3.2 Run의 핵심 필드

```java
// 소유 프로젝트 참조 (역직렬화 시 복원, XML에 저장하지 않음)
protected final transient @NonNull JobT project;

// 빌드 번호 (이전 버전에서는 고유하지 않았으나, 현재는 고유하고 연속적)
public transient /*final*/ int number;

// Queue에서 할당받은 ID
private long queueId = Run.QUEUE_ID_UNKNOWN;  // -1
public static final long QUEUE_ID_UNKNOWN = -1;

// 빌드가 스케줄된 시각 (밀리초)
protected /*final*/ long timestamp;

// 빌드가 실제 시작된 시각 (0이면 기록 없음)
private long startTime;

// 빌드 결과 — 빌드 중에도 변경될 수 있으므로 volatile
protected volatile Result result;

// 빌드 설명 (HTML 마크업 가능)
@CheckForNull
protected volatile String description;

// 빌드 표시 이름 (null이면 "#번호"로 표시)
private volatile String displayName;

// 현재 상태 (transient — XML에 저장하지 않음)
private transient volatile State state;

// 빌드 소요 시간 (밀리초)
protected long duration;

// 로그 파일 인코딩
protected String charset;
```

### 3.3 State enum — 빌드 상태 머신

```java
private enum State {
    NOT_STARTED,     // 생성/큐잉됨, 아직 빌드 시작 전
    BUILDING,        // 빌드 진행 중
    POST_PRODUCTION, // 빌드 완료, 결과 확정, 로그 파일은 아직 업데이트 중
    COMPLETED        // 빌드 완료, 로그 파일 닫힘
}
```

상태 전이 다이어그램:

```
  NOT_STARTED ──▶ BUILDING ──▶ POST_PRODUCTION ──▶ COMPLETED
```

**POST_PRODUCTION 상태의 의미**: 빌드 결과는 확정되었지만 로그 파일이 아직 닫히지 않은 상태이다. 이 시점에서 Jenkins는 빌드를 "완료"로 간주한다. 다른 빌드 트리거링 등의 후속 작업은 이 단계에서 시작된다. 이 설계는 JENKINS-980 이슈 해결을 위해 도입되었다.

### 3.4 Run 상속 계층

```
Actionable
  └── Run<JobT, RunT>
        │  ─ number, timestamp, result, duration, state
        │
        └── AbstractBuild<P, R> extends Run<P, R>
              │  implements Queue.Executable, RunWithSCM<P, R>
              │  ─ SCM 체크아웃, BuildWrapper, Builder, Publisher 실행
              │
              └── Build<P, B>
                    └── FreeStyleBuild
```

### 3.5 AbstractBuild

```
소스: core/src/main/java/hudson/model/AbstractBuild.java
```

```java
public abstract class AbstractBuild<P extends AbstractProject<P, R>,
                                     R extends AbstractBuild<P, R>>
    extends Run<P, R>
    implements Queue.Executable, LazyBuildMixIn.LazyLoadingRun<P, R>,
               RunWithSCM<P, R>
```

`Run`에 SCM 체크아웃, 빌드 래퍼, 빌드 스텝 실행 기능을 추가한다. `Queue.Executable` 인터페이스를 구현하여 Queue 시스템과 연동된다.

### 3.6 영속화 — build.xml

Run은 XStream을 통해 XML로 직렬화된다:

```java
// build.xml 파일 경로
private @NonNull XmlFile getDataFile() {
    return new XmlFile(XSTREAM, new File(getRootDir(), "build.xml"));
}

// XStream 인스턴스 설정
public static final XStream XSTREAM = new XStream2();
public static final XStream2 XSTREAM2 = (XStream2) XSTREAM;

static {
    XSTREAM.alias("build", FreeStyleBuild.class);
    XSTREAM.registerConverter(Result.conv);
}
```

`transient` 키워드가 붙은 필드(`project`, `number`, `state` 등)는 XML에 저장되지 않으며, `onLoad()` 시점에 런타임에서 복원된다.

---

## 4. Result 클래스

```
소스: core/src/main/java/hudson/model/Result.java (243줄)
```

```java
public final class Result implements Serializable, CustomExportedBean
```

빌드 결과를 나타내는 불변(immutable) 싱글턴 객체이다.

### 4.1 결과 상수 정의

| 상수 | ordinal | BallColor | completeBuild | 설명 |
|------|---------|-----------|---------------|------|
| `SUCCESS` | 0 | BLUE | true | 오류 없음 |
| `UNSTABLE` | 1 | YELLOW | true | 비치명적 오류 (예: 일부 테스트 실패) |
| `FAILURE` | 2 | RED | true | 치명적 오류 |
| `NOT_BUILT` | 3 | NOTBUILT | false | 빌드되지 않음 (다단계 빌드에서 앞 단계 실패) |
| `ABORTED` | 4 | ABORTED | false | 수동 중단 |

```java
public static final @NonNull Result SUCCESS   = new Result("SUCCESS",   BallColor.BLUE,     0, true);
public static final @NonNull Result UNSTABLE  = new Result("UNSTABLE",  BallColor.YELLOW,   1, true);
public static final @NonNull Result FAILURE   = new Result("FAILURE",   BallColor.RED,      2, true);
public static final @NonNull Result NOT_BUILT = new Result("NOT_BUILT", BallColor.NOTBUILT, 3, false);
public static final @NonNull Result ABORTED   = new Result("ABORTED",   BallColor.ABORTED,  4, false);
```

### 4.2 핵심 필드

```java
private final @NonNull String name;      // 결과 이름 ("SUCCESS", "FAILURE" 등)
public final int ordinal;                 // 순서값 — 클수록 나쁨
public final @NonNull BallColor color;    // UI 아이콘 색상
public final boolean completeBuild;       // 완전한 빌드인지 여부 (ABORTED, NOT_BUILT은 false)
```

### 4.3 비교와 결합

```java
// 두 결과 중 더 나쁜 것 반환 — 파이프라인에서 전체 결과 결정에 사용
public @NonNull Result combine(@NonNull Result that) {
    if (this.ordinal < that.ordinal)
        return that;
    else
        return this;
}

// 정적 메서드 — null-safe (null은 어떤 Result보다 좋은 것으로 취급)
public static Result combine(Result r1, Result r2) {
    if (r1 == null) return r2;
    else if (r2 == null) return r1;
    else return r1.combine(r2);
}

// 비교 메서드
public boolean isWorseThan(@NonNull Result that) {
    return this.ordinal > that.ordinal;
}

public boolean isWorseOrEqualTo(@NonNull Result that) {
    return this.ordinal >= that.ordinal;
}

public boolean isBetterThan(@NonNull Result that) {
    return this.ordinal < that.ordinal;
}

public boolean isBetterOrEqualTo(@NonNull Result that) {
    return this.ordinal <= that.ordinal;
}
```

### 4.4 싱글턴 보장

역직렬화 시 동일 인스턴스를 반환하기 위해 `readResolve()`를 구현한다:

```java
private static final Result[] all = new Result[] {SUCCESS, UNSTABLE, FAILURE, NOT_BUILT, ABORTED};

private Object readResolve() {
    for (Result r : all)
        if (ordinal == r.ordinal)
            return r;
    return FAILURE;
}
```

에이전트 노드에서 역직렬화된 Result도 마스터의 싱글턴 인스턴스로 복원되므로, `==` 비교가 안전하게 동작한다.

### 4.5 결과 결합 시각화

다단계 빌드에서 각 단계의 결과가 `combine()`으로 합산된다:

```
단계 1: SUCCESS (ordinal=0)  ─┐
                               ├── combine → UNSTABLE (1)
단계 2: UNSTABLE (ordinal=1) ─┘
                                          ─┐
                                           ├── combine → FAILURE (2)
단계 3: FAILURE  (ordinal=2) ─────────────┘

최종 결과: FAILURE
```

---

## 5. Queue 시스템

```
소스: core/src/main/java/hudson/model/Queue.java (약 3252줄)
```

```java
public class Queue extends ResourceController implements Saveable
```

Queue는 Jenkins 빌드 스케줄링의 핵심이다. 빌드 요청을 받아서 실행 가능한 Executor에 할당하기까지의 전체 흐름을 관리한다.

### 5.1 큐의 4단계 자료구조

```java
// 1단계: 대기 목록 — quiet period가 지나지 않은 항목
private final Set<WaitingItem> waitingList = new TreeSet<>();

// 2단계: 차단 목록 — 실행 조건 미충족 (다른 빌드 진행 중, 리소스 부족 등)
private final ItemList<BlockedItem> blockedProjects = new ItemList<>();

// 3단계: 빌드 가능 목록 — 실행 가능하나 Executor 대기 중
private final ItemList<BuildableItem> buildables = new ItemList<>();

// 4단계: 보류 목록 — Executor에 할당됨, 아직 실행 시작 전
private final ItemList<BuildableItem> pendings = new ItemList<>();

// 완료 항목 캐시 — 5분 TTL (ID 추적용)
private final Cache<Long, LeftItem> leftItems =
    CacheBuilder.newBuilder()
        .expireAfterWrite(5 * 60, TimeUnit.SECONDS)
        .build();
```

**왜 4단계인가?**: 각 단계는 서로 다른 차단 조건을 처리한다. 시간 기반(quiet period), 리소스 기반(동시 빌드 제한), 용량 기반(Executor 가용성), 핸드오프(Executor 시작)를 분리함으로써 스케줄링 로직을 단순화한다.

### 5.2 큐 상태 전이 다이어그램

```
  schedule()
     │
     ▼
┌──────────────┐   quiet period    ┌──────────────┐   blocked?   ┌──────────────┐
│  WaitingItem │ ─── 경과 후 ───▶ │ BlockedItem  │ ──── NO ───▶│BuildableItem │
│  (waitingList)│                   │(blockedProj) │              │ (buildables) │
└──────────────┘                   └──────┬───────┘              └──────┬───────┘
                                          │ YES                         │
                                          └──── 대기 ◀────────────────┘
                                                                        │
                                                                Executor 할당
                                                                        │
                                                                        ▼
                                                               ┌──────────────┐
                                                               │BuildableItem │
                                                               │  (pendings)  │
                                                               └──────┬───────┘
                                                                      │
                                                               실행 시작
                                                                      │
                                                                      ▼
                                                               ┌──────────────┐
                                                               │   LeftItem   │
                                                               │ (leftItems)  │
                                                               │ 5분 후 제거  │
                                                               └──────────────┘
```

### 5.3 Queue.Item 계층

```java
// 추상 기본 클래스
public abstract static class Item extends Actionable implements QueueItem {
    private final long id;              // 고유 ID (마스터 전체 범위)
    @NonNull
    public final Task task;             // 빌드할 프로젝트
    private /*almost final*/ transient FutureImpl future;  // 비동기 결과
    private final long inQueueSince;    // 큐 진입 시각

    // 상태 판별 메서드
    public boolean isBlocked()   { return this instanceof BlockedItem; }
    public boolean isBuildable() { return this instanceof BuildableItem; }
}
```

**Item 계층 상속 트리:**

```
Queue.Item (abstract)
  ├── WaitingItem
  │     ─ timestamp: Calendar (실행 가능 시각)
  │     implements Comparable<WaitingItem>
  │
  └── NotWaitingItem (abstract)
        │  ─ buildableStartMilliseconds: long
        │
        ├── BlockedItem
        │     ─ causeOfBlockage: CauseOfBlockage
        │
        └── BuildableItem
              ─ isPending: boolean

LeftItem (별도)
  ─ outcome: WorkUnitContext
  ─ isCancelled: boolean
```

### 5.4 Queue.Task 인터페이스

```java
public interface Task extends FullyNamedModelObject, SubTask {
    // 빌드 차단 여부
    @Deprecated
    default boolean isBuildBlocked() {
        return getCauseOfBlockage() != null;
    }

    // 차단 원인 반환 (null이면 즉시 빌드 가능)
    @CheckForNull
    default CauseOfBlockage getCauseOfBlockage() {
        return null;
    }

    // 로드 밸런서가 사용하는 친화도 키
    default String getAffinityKey() {
        if (this instanceof FullyNamed fullyNamed) {
            return fullyNamed.getFullName();
        } else {
            return getFullDisplayName();
        }
    }

    // 중단 권한 확인
    default void checkAbortPermission() {
        if (this instanceof AccessControlled) {
            ((AccessControlled) this).checkPermission(CANCEL);
        }
    }

    String getName();
}
```

`AbstractProject`는 `BuildableItem` 인터페이스를 통해 `Queue.Task`를 구현한다.

### 5.5 Queue.Executable 인터페이스

```java
public interface Executable extends Runnable, WithConsoleUrl {
    // 이 실행을 생성한 Task
    @NonNull SubTask getParent();

    // 상위 실행 (예: 매트릭스 빌드에서 부모 Run)
    @CheckForNull
    default Queue.Executable getParentExecutable() {
        return null;
    }
}
```

`AbstractBuild`가 이 인터페이스를 구현하여, Queue에서 할당받은 작업을 Executor가 실행할 수 있게 한다.

### 5.6 Queue.JobOffer

```java
public static class JobOffer extends MappingWorksheet.ExecutorSlot {
    public final Executor executor;
    private WorkUnit workUnit;

    @Override
    protected void set(WorkUnit p) {
        assert this.workUnit == null;
        this.workUnit = p;
        assert executor.isParking();
        executor.start(workUnit);
    }

    @Override
    public Executor getExecutor() {
        return executor;
    }

    // 이 Executor가 해당 작업을 실행할 수 있는지 확인
    public @CheckForNull CauseOfBlockage getCauseOfBlockage(BuildableItem item) {
        Node node = getNode();
        if (node == null) {
            return CauseOfBlockage.fromMessage(...);
        }
        CauseOfBlockage reason = node.canTake(item);
        // QueueTaskDispatcher 체인도 확인
        for (QueueTaskDispatcher d : QueueTaskDispatcher.all()) {
            reason = d.canTake(node, item);
            if (reason != null) return reason;
        }
        return null;
    }
}
```

유휴 Executor마다 `JobOffer`가 생성된다. 스케줄러는 이 오퍼를 순회하면서 적합한 작업을 매칭한다.

### 5.7 동시성 제어

```java
private final transient ReentrantLock lock = new ReentrantLock();
private final transient Condition condition = lock.newCondition();
```

Queue는 `ReentrantLock`과 `Condition`으로 동시성을 제어한다. 모든 큐 상태 변경은 `lock.lock()` / `lock.unlock()` 블록 안에서 수행된다.

### 5.8 Snapshot — 락-프리 읽기

```java
private transient volatile Snapshot snapshot = new Snapshot(
    waitingList, blockedProjects, buildables, pendings
);
```

읽기 전용 접근은 `Snapshot` 객체를 통해 락 없이 수행할 수 있다. 쓰기 작업 후에 새 Snapshot을 생성하여 volatile 필드에 할당한다.

### 5.9 schedule2() — 빌드 스케줄링 진입점

```java
public @NonNull ScheduleResult schedule2(Task p, int quietPeriod, List<Action> actions) {
    actions = new ArrayList<>(actions);
    actions.removeIf(Objects::isNull);

    lock.lock();
    try {
        // 1. 중복 검사 — 이미 큐에 있는 동일 Task 확인
        // 2. FoldableAction 처리 — 기존 항목에 Action 병합
        // 3. WaitingItem 생성 후 waitingList에 추가
        // 4. scheduleMaintenance() 호출
    } finally {
        lock.unlock();
    }
}
```

---

## 6. Node / Computer / Executor

Jenkins의 빌드 인프라는 세 가지 계층으로 분리되어 있다:

| 계층 | 클래스 | 관심사 |
|------|--------|--------|
| **설정(Configuration)** | `Node` | 이름, 레이블, Executor 수, 워크스페이스 경로 |
| **런타임 상태(Runtime)** | `Computer` | 연결 상태, Executor 목록, 오프라인 원인 |
| **실행 스레드(Thread)** | `Executor` | 실제 빌드 실행, 작업 단위(WorkUnit) 처리 |

### 6.1 Node

```
소스: core/src/main/java/hudson/model/Node.java (약 682줄)
```

```java
public abstract class Node extends AbstractModelObject
    implements ReconfigurableDescribable<Node>, ExtensionPoint,
               AccessControlled, OnMaster, PersistenceRoot
```

Node는 설정 데이터만 보유하는 **영속 객체**이다. 설정이 변경되면 기존 인스턴스가 폐기되고 새로 생성된다.

**핵심 추상 메서드:**

```java
public abstract String getNodeName();         // 노드 이름 ("": 마스터)
public abstract String getNodeDescription();  // 노드 설명
public abstract int getNumExecutors();        // Executor 수
public abstract String getLabelString();      // 레이블 문자열
public abstract Launcher createLauncher(TaskListener listener);
public abstract @CheckForNull FilePath getWorkspaceFor(TopLevelItem item);
```

**Mode enum:**

```java
public enum Mode {
    NORMAL,     // 가능하면 이 노드에서 실행
    EXCLUSIVE   // 레이블이 일치하는 작업만 실행
}
```

**특수 사례 — Jenkins 자체도 Node이다:**

```java
// Jenkins 클래스 정의
public class Jenkins extends AbstractCIBase ... {
    // AbstractCIBase extends Node
}
```

`AbstractCIBase`가 `Node`를 상속하므로, Jenkins 마스터 자체도 하나의 Node로 취급된다.

### 6.2 Slave

```
소스: core/src/main/java/hudson/model/Slave.java
```

```java
public abstract class Slave extends Node implements Serializable
```

Node의 구체 구현이다. 원격 에이전트 노드를 나타낸다.

**핵심 필드:**

```java
protected String name;         // 에이전트 이름
private String description;    // 설명
protected final String remoteFS; // 원격 워크스페이스 루트 경로
private int numExecutors = 1;  // Executor 수
private Mode mode = Mode.NORMAL; // 작업 할당 모드
```

### 6.3 Computer

```
소스: core/src/main/java/hudson/model/Computer.java (약 1801줄)
```

```java
public /*transient*/ abstract class Computer extends Actionable
    implements AccessControlled, IComputer, ExecutorListener,
               DescriptorByNameOwner, StaplerProxy, HasWidgets
```

Computer는 Node의 **런타임 상태**를 표현한다. Node와 Computer의 생명주기는 독립적이다:
- Node를 제거해도 빌드가 진행 중이면 Computer는 남아있다
- Node 설정이 변경되면 Node 객체는 새로 생성되지만 Computer는 유지된다

**핵심 필드:**

```java
// Executor 관리
private final CopyOnWriteArrayList<Executor> executors = new CopyOnWriteArrayList<>();
private final CopyOnWriteArrayList<OneOffExecutor> oneOffExecutors = new CopyOnWriteArrayList<>();
private int numExecutors;

// 오프라인 상태
protected volatile OfflineCause offlineCause;

// 연결 시간
private long connectTime = 0;

// Node 이름 참조 (Node 객체 자체를 참조하지 않음)
protected String nodeName;

// 워크스페이스 관리
private final WorkspaceList workspaceList = new WorkspaceList();

// 임시 Action
protected transient List<Action> transientActions;

// 상태 변경 락
protected final Object statusChangeLock = new Object();
```

### 6.4 Executor

```
소스: core/src/main/java/hudson/model/Executor.java (약 992줄)
```

```java
public class Executor extends Thread implements ModelObject, IExecutor
```

Executor는 **실제 빌드를 실행하는 스레드**이다. 1.536 이후로 on-demand 방식으로 스레드를 시작한다.

**핵심 필드:**

```java
protected final @NonNull Computer owner;   // 소속 Computer
private final Queue queue;                  // Queue 참조
private final ReadWriteLock lock = new ReentrantReadWriteLock();

@GuardedBy("lock")
private long startTime;                     // 현재 실행 시작 시각
private final long creationTime = System.currentTimeMillis();

private int number;                         // Executor 번호

@GuardedBy("lock")
private Queue.Executable executable;        // 현재 실행 중인 Executable

@GuardedBy("lock")
private AsynchronousExecution asynchronousExecution;  // 비동기 실행

@GuardedBy("lock")
private WorkUnit workUnit;                  // Queue에서 할당받은 작업 단위

@GuardedBy("lock")
private boolean started;                    // 시작 여부

@GuardedBy("lock")
private Result interruptStatus;             // 인터럽트 시 결과 오버라이드

@GuardedBy("lock")
private final List<CauseOfInterruption> causes = new Vector<>();  // 인터럽트 원인
```

### 6.5 Node-Computer-Executor 관계 다이어그램

```
┌─────────────────────────────────────────────────────────┐
│  Node (설정, 영속)                                       │
│  ─ name, numExecutors, labels, mode, remoteFS           │
│                                                          │
│  ┌─────────────────────────────────────────────────┐    │
│  │  Computer (런타임 상태, 임시)                      │    │
│  │  ─ offlineCause, connectTime, workspaceList      │    │
│  │                                                   │    │
│  │  ┌────────────┐ ┌────────────┐ ┌────────────┐   │    │
│  │  │ Executor#0 │ │ Executor#1 │ │ Executor#2 │   │    │
│  │  │  (Thread)  │ │  (Thread)  │ │  (idle)    │   │    │
│  │  │ ─workUnit  │ │ ─workUnit  │ │            │   │    │
│  │  │ ─executable│ │ ─executable│ │            │   │    │
│  │  └────────────┘ └────────────┘ └────────────┘   │    │
│  └─────────────────────────────────────────────────┘    │
└─────────────────────────────────────────────────────────┘
```

### 6.6 왜 Node와 Computer를 분리했는가?

1. **생명주기 독립성**: 설정 변경 시 Node 객체는 새로 만들어지지만, Computer는 기존 빌드를 계속 실행해야 한다
2. **UI 바인딩**: Computer는 URL을 가지고 웹 UI를 제공하지만, Node는 URL 바인딩이 없다
3. **Executor 수 0**: Node가 Executor 0개로 설정되면 Computer가 생성되지 않을 수 있다 (마스터 제외)
4. **영속성 분리**: Node는 `config.xml`로 영속화되고, Computer의 런타임 상태는 영속화하지 않는다

---

## 7. Action / Actionable 패턴

### 7.1 Action 인터페이스

```
소스: core/src/main/java/hudson/model/Action.java
```

```java
public interface Action extends ModelObject {
    // 사이드바 아이콘 파일명 (null이면 숨김)
    // "symbol-" 접두사면 Jenkins Symbol 사용
    @CheckForNull String getIconFileName();

    // 표시 이름
    @NonNull String getDisplayName();

    // URL 경로 세그먼트
    @CheckForNull String getUrlName();
}
```

Action은 Jenkins에서 가장 널리 사용되는 확장 메커니즘이다. Job, Run, Computer 등 거의 모든 모델 객체에 부착하여 UI와 데이터를 확장할 수 있다.

### 7.2 Actionable 클래스

```
소스: core/src/main/java/hudson/model/Actionable.java
```

```java
public abstract class Actionable extends AbstractModelObject
    implements ModelObjectWithContextMenu
{
    // 영속 Action 목록 — XStream으로 직렬화됨
    private volatile CopyOnWriteArrayList<Action> actions;

    // 영속 Action만 반환
    @Deprecated
    public List<Action> getActions() { ... }

    // 영속 + 임시 Action 모두 반환
    @Exported(name = "actions")
    public final List<? extends Action> getAllActions() {
        List<Action> _actions = getActions();
        boolean adding = false;
        for (TransientActionFactory<?> taf :
             TransientActionFactory.factoriesFor(getClass(), Action.class)) {
            Collection<? extends Action> additions = createFor(taf);
            if (!additions.isEmpty()) {
                if (!adding) {
                    adding = true;
                    _actions = new ArrayList<>(_actions);
                }
                _actions.addAll(additions);
            }
        }
        return Collections.unmodifiableList(_actions);
    }
}
```

### 7.3 영속 Action vs 임시 Action

| 유형 | 저장 | 생성 시점 | 예시 |
|------|------|----------|------|
| **영속(Persistent)** | XML에 포함 | 명시적 `addAction()` | `CauseAction`, `ParametersAction` |
| **임시(Transient)** | 저장 안 됨 | `TransientActionFactory`에서 매번 생성 | 플러그인이 동적으로 추가하는 UI 요소 |

`CopyOnWriteArrayList`를 사용하여 영속 Action 목록의 스레드 안전성을 보장한다. 읽기 시에는 락이 필요 없고, 쓰기 시에만 내부적으로 배열을 복사한다.

### 7.4 주요 Action 구현

```
Action
  ├── InvisibleAction         — 사이드바에 표시하지 않는 Action
  ├── CauseAction             — 빌드 원인 (누가/무엇이 트리거했는지)
  ├── ParametersAction        — 빌드 매개변수
  ├── ParametersDefinitionAction — 매개변수 정의 (사용자 입력 폼)
  ├── FingerprintAction       — 아티팩트 핑거프린트
  └── InterruptedBuildAction  — 인터럽트 정보
```

---

## 8. View 시스템

### 8.1 View

```
소스: core/src/main/java/hudson/model/View.java
```

```java
public abstract class View extends AbstractModelObject
    implements AccessControlled, Describable<View>, ExtensionPoint,
               Saveable, ModelObjectWithChildren, DescriptorByNameOwner,
               HasWidgets, Badgeable
```

View는 Job을 그룹화하여 보여주는 **프레젠테이션 계층**이다. 데이터 모델 자체를 변경하지 않으며, 기존 Job의 필터링된 뷰를 제공한다.

**핵심 필드:**

```java
protected /*final*/ ViewGroup owner;   // 이 뷰를 포함하는 그룹
protected String name;                  // 뷰 이름
protected String description;           // 설명 메시지
protected boolean filterExecutors;      // 관련 Executor만 표시
protected boolean filterQueue;          // 관련 큐 항목만 표시

// 뷰 속성 목록
private volatile DescribableList<ViewProperty, ViewPropertyDescriptor> properties
    = new PropertyList(this);
```

### 8.2 ListView

```
소스: core/src/main/java/hudson/model/ListView.java
```

```java
public class ListView extends View implements DirectlyModifiableView
```

가장 기본적인 뷰 타입이다. Job 이름 목록으로 필터링한다.

**핵심 필드:**

```java
@GuardedBy("this")
/*package*/ SortedSet<String> jobNames =
    new TreeSet<>(String.CASE_INSENSITIVE_ORDER);  // 대소문자 무시

private DescribableList<ViewJobFilter, Descriptor<ViewJobFilter>> jobFilters;
private DescribableList<ListViewColumn, Descriptor<ListViewColumn>> columns;
private String includeRegex;          // 정규식 필터
private volatile boolean recurse;     // 하위 ItemGroup 재귀 탐색
private transient Pattern includePattern;  // 컴파일된 정규식
```

### 8.3 View 상속 계층

```
View (abstract)
  ├── ListView           — Job 이름 목록 기반 필터링
  ├── AllView            — 모든 Job 표시
  ├── MyView             — 현재 사용자 관련 Job 표시
  └── (플러그인 확장)     — Dashboard View, Nested View 등
```

---

## 9. Descriptor / Describable 패턴

### 9.1 개요

Jenkins의 핵심 확장 메커니즘이다. **Describable**은 설정 가능한 객체이고, **Descriptor**는 그 객체의 메타데이터(타입 정보, 폼 검증, 인스턴스 생성)를 담당한다.

```java
// Descriptor 정의
public abstract class Descriptor<T extends Describable<T>> implements Loadable, Saveable, OnMaster {
    // 이 Descriptor가 설명하는 클래스
    public final transient Class<? extends T> clazz;
}
```

### 9.2 왜 이 패턴이 필요한가?

1. **싱글턴 메타데이터**: 같은 타입의 인스턴스가 여럿이어도 Descriptor는 하나만 존재
2. **동적 폼 생성**: Descriptor가 Jelly/Groovy 뷰를 제공하여 설정 UI를 자동 생성
3. **글로벌 설정 분리**: Descriptor에 글로벌 설정을 저장하고, 각 인스턴스에 개별 설정을 저장
4. **타입 등록**: `@Extension` 어노테이션으로 Jenkins에 자동 등록

### 9.3 적용 예시

```
SCM (Describable)                     ─── SCMDescriptor (Descriptor)
  └── GitSCM                          ─── GitSCM.DescriptorImpl

Trigger (Describable)                 ─── TriggerDescriptor (Descriptor)
  └── TimerTrigger                    ─── TimerTrigger.DescriptorImpl

Builder (Describable)                 ─── BuildStepDescriptor (Descriptor)
  └── Shell                           ─── Shell.DescriptorImpl
```

### 9.4 Descriptor가 관리하는 데이터

| 데이터 | 위치 | 예시 |
|--------|------|------|
| 글로벌 설정 | `JENKINS_HOME/descriptor_name.xml` | Git 글로벌 설정 |
| 인스턴스 설정 | Job의 `config.xml` 내부 | 특정 Job의 Git 저장소 URL |
| UI 뷰 | `config.jelly` | 설정 폼 HTML |
| 검증 로직 | `doCheck*()` 메서드 | URL 유효성 검사 |

---

## 10. 영속화(Persistence) 메커니즘

### 10.1 XStream 기반 XML 직렬화

Jenkins는 데이터베이스를 사용하지 않는다. 모든 데이터는 **XStream**을 사용하여 XML 파일로 직렬화된다.

```java
// Run 클래스의 XStream 인스턴스
public static final XStream XSTREAM = new XStream2();

static {
    XSTREAM.alias("build", FreeStyleBuild.class);
    XSTREAM.registerConverter(Result.conv);
}
```

`XStream2`는 Jenkins가 XStream을 확장한 버전으로, 다음 기능을 추가한다:
- 클래스 이름 변경(마이그레이션) 지원
- 보안 강화 (허용된 타입만 역직렬화)
- 호환성 유지를 위한 필드 별칭

### 10.2 XmlFile 클래스

```java
// Run의 데이터 파일
private @NonNull XmlFile getDataFile() {
    return new XmlFile(XSTREAM, new File(getRootDir(), "build.xml"));
}
```

`XmlFile`은 XStream 인스턴스와 파일 경로를 결합하여, 원자적 읽기/쓰기를 제공한다:
- 쓰기: 임시 파일에 먼저 쓰고, 원자적으로 이름 변경
- 읽기: XML 파일을 XStream으로 역직렬화

### 10.3 Saveable 인터페이스

```java
public interface Saveable {
    void save() throws IOException;
}
```

`Queue`, `View`, `Node` 등이 이 인터페이스를 구현한다. `BulkChange`를 사용하면 여러 변경을 모아서 한 번에 저장할 수 있다.

### 10.4 transient 필드와 onLoad()

Jenkins의 영속화에서 `transient` 키워드는 중요한 역할을 한다:

| transient 필드 | 복원 방법 | 이유 |
|----------------|----------|------|
| `Run.project` | `onLoad()`에서 부모 Job 참조 설정 | 순환 참조 방지 |
| `Run.number` | 디렉토리 이름에서 추출 | 별도 관리 |
| `Run.state` | COMPLETED로 초기화 | 런타임 상태 |
| `Job.nextBuildNumber` | `nextBuildNumber` 파일에서 읽기 | 독립 관리 |
| `Computer.executors` | Computer 시작 시 생성 | 런타임 객체 |

### 10.5 volatile 필드의 의미

Jenkins 소스코드에서 `volatile`은 두 가지 목적으로 사용된다:

1. **스레드 안전성**: `Run.result`, `Run.state`, `AbstractProject.disabled` 등은 여러 스레드에서 동시에 읽고 쓸 수 있다
2. **설정 변경 반영**: `AbstractProject.scm`, `AbstractProject.triggers` 등은 관리자가 설정을 변경하면 즉시 반영되어야 한다

---

## 11. JENKINS_HOME 파일시스템 구조

```
JENKINS_HOME/
├── config.xml                      # Jenkins 메인 설정
│                                    # (시스템 URL, 보안, 뷰, 노드 목록 등)
│
├── secret.key                       # 암호화 키
├── secret.key.not-so-secret         # 초기화 토큰
├── identity.key.enc                 # 인스턴스 ID 암호화 키
│
├── queue.xml                        # 큐 상태 스냅샷
│                                    # (Jenkins 재시작 시 큐 복원)
│
├── nextBuildNumber                  # (global, 사용처에 따라 다름)
│
├── jobs/                            # 모든 Job 설정 및 빌드 기록
│   └── {job-name}/
│       ├── config.xml               # Job 설정 (XStream 직렬화)
│       ├── nextBuildNumber          # 다음 빌드 번호 (텍스트 파일)
│       └── builds/                  # 빌드 기록
│           └── {build-number}/
│               ├── build.xml        # Run 직렬화 데이터
│               ├── log              # 콘솔 출력 로그
│               ├── log.gz           # 압축된 로그 (오래된 빌드)
│               ├── changelog.xml    # SCM 변경 로그
│               └── archive/         # 아티팩트 저장소
│
├── nodes/                           # 에이전트 노드 설정
│   └── {node-name}/
│       └── config.xml               # Node 설정 (Slave 직렬화)
│
├── users/                           # 사용자 데이터
│   └── {user-id}/
│       └── config.xml               # User 설정
│
├── plugins/                         # 플러그인
│   ├── {plugin-name}.jpi            # 플러그인 JAR
│   └── {plugin-name}/              # 풀린 플러그인 디렉토리
│
├── fingerprints/                    # 아티팩트 핑거프린트 (해시 기반 디렉토리)
│   └── {aa}/{bb}/
│       └── {aabb...}.xml
│
├── logs/                            # Jenkins 자체 로그
│
├── updates/                         # 플러그인 업데이트 센터 캐시
│
├── war/                             # 풀린 WAR 파일
│
└── workspace/                       # 마스터 노드의 워크스페이스
    └── {job-name}/
        └── (소스코드 체크아웃)
```

### 11.1 파일별 데이터 모델 매핑

| 파일 경로 | 직렬화 대상 | XStream 인스턴스 |
|-----------|------------|----------------|
| `jobs/{name}/config.xml` | `Job` (+ `AbstractProject`, `Project`) | `Items.XSTREAM2` |
| `jobs/{name}/builds/{n}/build.xml` | `Run` (+ `AbstractBuild`) | `Run.XSTREAM2` |
| `nodes/{name}/config.xml` | `Slave` | `Jenkins.XSTREAM2` |
| `users/{name}/config.xml` | `User` | `User.XSTREAM` |
| `config.xml` | `Jenkins` | `Jenkins.XSTREAM2` |
| `queue.xml` | `Queue` | `Queue.XSTREAM` |
| `fingerprints/{hash}.xml` | `Fingerprint` | 자체 XStream |

### 11.2 데이터 일관성

Jenkins는 ACID 트랜잭션을 지원하지 않는다. 대신:

1. **원자적 파일 쓰기**: 임시 파일에 쓴 후 rename
2. **BulkChange**: 여러 변경을 버퍼링한 후 한 번에 저장
3. **volatile/CopyOnWrite**: 메모리 내 일관성 보장
4. **SaveableListener**: 저장 이벤트 알림

---

## 12. 데이터 흐름: 빌드 생명주기

### 12.1 전체 흐름

```
사용자/트리거
     │
     │ scheduleBuild2(quietPeriod, cause, actions)
     ▼
┌─────────────────────────────────────────────────────────────────┐
│  AbstractProject.scheduleBuild2()                                │
│    → Jenkins.get().getQueue().schedule2(this, quietPeriod, ...) │
└─────────────────────────────────┬───────────────────────────────┘
                                  │
                                  ▼
┌─────────────────────────────────────────────────────────────────┐
│  Queue.schedule2(task, quietPeriod, actions)                     │
│    1. lock.lock()                                                │
│    2. 중복 검사 (같은 Task가 이미 큐에 있는지)                       │
│    3. FoldableAction 병합 (있으면 기존 항목에 추가)                  │
│    4. WaitingItem 생성 → waitingList에 추가                       │
│    5. scheduleMaintenance() 호출                                 │
│    6. lock.unlock()                                              │
└─────────────────────────────────┬───────────────────────────────┘
                                  │
                   [Queue Maintenance Loop]
                                  │
                                  ▼
┌─────────────────────────────────────────────────────────────────┐
│  Queue.maintain()                                                │
│    1. WaitingItem의 quiet period 확인                             │
│       → 경과하면 BlockedItem 또는 BuildableItem으로 전환            │
│    2. BlockedItem의 차단 조건 확인                                 │
│       → 해소되면 BuildableItem으로 전환                            │
│    3. BuildableItem을 위한 Executor 매칭                          │
│       → JobOffer 확인, MappingWorksheet 생성                     │
│    4. 매칭 성공 시 BuildableItem → pendings으로 이동               │
└─────────────────────────────────┬───────────────────────────────┘
                                  │
                                  ▼
┌─────────────────────────────────────────────────────────────────┐
│  Executor.run()                                                  │
│    1. WorkUnit에서 Executable 생성                                │
│       → SubTask.createExecutable()                               │
│    2. Run 인스턴스 생성 (state = NOT_STARTED)                     │
│    3. state → BUILDING                                           │
│    4. SCM 체크아웃 (AbstractBuild)                                │
│    5. Builder 실행                                                │
│    6. Publisher 실행                                              │
│    7. state → POST_PRODUCTION                                    │
│    8. 로그 파일 닫기                                               │
│    9. state → COMPLETED                                          │
│   10. Run.save() — build.xml로 직렬화                             │
└─────────────────────────────────────────────────────────────────┘
```

### 12.2 Cause 시스템

빌드가 시작된 원인을 추적하기 위해 `Cause` 클래스 계층이 있다:

```java
public abstract class Cause {
    public abstract String getShortDescription();
}
```

| Cause 하위 클래스 | 트리거 원인 |
|-------------------|-----------|
| `Cause.UserIdCause` | 사용자가 수동으로 빌드 시작 |
| `Cause.UpstreamCause` | 업스트림 프로젝트 빌드 성공 후 트리거 |
| `Cause.RemoteCause` | 원격 API 호출 |
| (SCMTriggerCause) | SCM 변경 감지 (플러그인) |
| (TimerTriggerCause) | 크론 스케줄 (플러그인) |

Cause는 `CauseAction`에 담겨 `Queue.Item`의 Action 목록에 추가되고, 최종적으로 `Run`의 Action으로 저장된다.

### 12.3 빌드 번호 할당

```java
// Job.java의 onLoad()에서
TextFile f = getNextBuildNumberFile();
if (f.exists()) {
    synchronized (this) {
        this.nextBuildNumber = Integer.parseInt(f.readTrim());
    }
}

// 빌드 생성 시
synchronized (this) {
    int buildNumber = nextBuildNumber++;
    saveNextBuildNumber();
    // Run 인스턴스에 번호 할당
}
```

`synchronized` 블록으로 빌드 번호의 원자성을 보장한다. 번호 할당 후 즉시 파일에 저장하여, 비정상 종료 시에도 번호 충돌을 방지한다.

---

## 13. 핵심 인터페이스와 타입 관계

### 13.1 전체 타입 관계 다이어그램

```
                          ModelObject (interface)
                              │
                    AbstractModelObject
                         │          │
                    Actionable      View
                    │    │    │
                 Item   Run  Computer
                  │
              AbstractItem
                  │
                 Job
                  │
            AbstractProject ───implements──▶ Queue.Task
                  │
               Project
                  │
           FreeStyleProject


    Queue.Executable ◀──implements── AbstractBuild ──extends──▶ Run
         │
    Runnable (java.lang)


    Node (abstract)
      ├── AbstractCIBase ── Jenkins (마스터)
      └── Slave (에이전트)


    Descriptor<T> ◀────── 1:1 ──────▶ Describable<T>
```

### 13.2 주요 인터페이스 요약

| 인터페이스 | 패키지 | 역할 | 주요 구현체 |
|-----------|--------|------|-----------|
| `ModelObject` | `hudson.model` | 이름 제공 (`getDisplayName()`) | 거의 모든 모델 |
| `Item` | `hudson.model` | Jenkins 내 관리 항목 | `Job`, `Folder` |
| `TopLevelItem` | `hudson.model` | 최상위 생성 가능 항목 | `FreeStyleProject` |
| `Action` | `hudson.model` | UI/데이터 확장 | `CauseAction` |
| `Queue.Task` | `hudson.model` | 큐에 제출 가능한 작업 | `AbstractProject` |
| `Queue.Executable` | `hudson.model` | Executor에서 실행 가능 | `AbstractBuild` |
| `Describable<T>` | `hudson.model` | 설정 가능한 객체 | `SCM`, `Trigger` |
| `Saveable` | `hudson.model` | 저장 가능한 객체 | `Queue`, `View` |
| `PersistenceRoot` | `hudson.model` | 파일시스템 루트 보유 | `Job`, `Run` |
| `AccessControlled` | `hudson.security` | 권한 검사 | `Job`, `Computer` |
| `ExtensionPoint` | `hudson` | 플러그인 확장점 | `Job`, `Run`, `View` |

### 13.3 데이터 모델 테이블 요약

| 클래스 | 줄 수 | 핵심 필드 | 영속화 | 역할 |
|--------|-------|----------|--------|------|
| `Job` | ~1731 | nextBuildNumber, properties | config.xml | 빌드 대상 정의 |
| `AbstractProject` | ~2163 | scm, triggers, assignedNode, disabled | config.xml | SCM/빌드 실행 |
| `Project` | ~180 | builders, publishers, buildWrappers | config.xml | 빌드 파이프라인 |
| `FreeStyleProject` | ~80 | (없음) | config.xml | 기본 프로젝트 |
| `Run` | ~2698 | number, timestamp, result, duration, state | build.xml | 빌드 실행 기록 |
| `AbstractBuild` | ~500 | (SCM 연동) | build.xml | SCM 빌드 |
| `Result` | 243 | name, ordinal, color, completeBuild | (Run 내부) | 빌드 결과 |
| `Queue` | ~3252 | waitingList, blockedProjects, buildables, pendings | queue.xml | 빌드 스케줄링 |
| `Node` | ~682 | (abstract) name, numExecutors, labels | config.xml | 설정 |
| `Computer` | ~1801 | executors, offlineCause, nodeName | (없음) | 런타임 상태 |
| `Executor` | ~992 | owner, workUnit, executable | (없음) | 빌드 실행 스레드 |
| `View` | ~900 | name, description, owner | config.xml 내부 | Job 표시 뷰 |
| `ListView` | ~450 | jobNames, includeRegex, columns | config.xml 내부 | 목록 뷰 |
| `Actionable` | ~200 | actions (CopyOnWriteArrayList) | 소유자와 함께 | Action 컨테이너 |
| `Slave` | ~450 | name, remoteFS, numExecutors, mode | config.xml | 에이전트 노드 |
| `Fingerprint` | ~1200 | hash, original, usages | fingerprints/ | 아티팩트 추적 |
| `User` | ~800 | id, fullName, description | config.xml | 사용자 |

### 13.4 JobProperty 확장 메커니즘

```java
public abstract class JobProperty<J extends Job<?, ?>>
    implements ReconfigurableDescribable<JobProperty<?>>,
               BuildStep, ExtensionPoint
{
    // 소유 Job
    protected transient J owner;
}
```

`Job.properties`는 `CopyOnWriteList<JobProperty>`로 관리되며, 플러그인이 Job에 임의의 속성을 추가할 수 있게 한다. 대표적인 예:

- `BuildDiscarderProperty`: 빌드 기록 정리 정책
- `ParametersDefinitionProperty`: 빌드 매개변수 정의
- `GithubProjectProperty`: GitHub 프로젝트 연결 (플러그인)

---

## 부록: 핵심 소스 파일 목록

| 파일 | 위치 |
|------|------|
| Job.java | `core/src/main/java/hudson/model/Job.java` |
| AbstractProject.java | `core/src/main/java/hudson/model/AbstractProject.java` |
| Project.java | `core/src/main/java/hudson/model/Project.java` |
| FreeStyleProject.java | `core/src/main/java/hudson/model/FreeStyleProject.java` |
| Run.java | `core/src/main/java/hudson/model/Run.java` |
| AbstractBuild.java | `core/src/main/java/hudson/model/AbstractBuild.java` |
| Result.java | `core/src/main/java/hudson/model/Result.java` |
| Queue.java | `core/src/main/java/hudson/model/Queue.java` |
| Node.java | `core/src/main/java/hudson/model/Node.java` |
| Slave.java | `core/src/main/java/hudson/model/Slave.java` |
| Computer.java | `core/src/main/java/hudson/model/Computer.java` |
| Executor.java | `core/src/main/java/hudson/model/Executor.java` |
| Action.java | `core/src/main/java/hudson/model/Action.java` |
| Actionable.java | `core/src/main/java/hudson/model/Actionable.java` |
| View.java | `core/src/main/java/hudson/model/View.java` |
| ListView.java | `core/src/main/java/hudson/model/ListView.java` |
| Descriptor.java | `core/src/main/java/hudson/model/Descriptor.java` |
| Cause.java | `core/src/main/java/hudson/model/Cause.java` |
| JobProperty.java | `core/src/main/java/hudson/model/JobProperty.java` |
| Fingerprint.java | `core/src/main/java/hudson/model/Fingerprint.java` |
| User.java | `core/src/main/java/hudson/model/User.java` |
| Jenkins.java | `core/src/main/java/jenkins/model/Jenkins.java` |
