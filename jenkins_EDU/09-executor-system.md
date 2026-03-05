# 09. Executor 시스템 - 빌드 실행의 핵심 엔진

## 개요

Jenkins의 Executor 시스템은 빌드 작업을 실제로 실행하는 핵심 메커니즘이다. 이 시스템은 세 가지 주요 추상화 계층으로 구성된다:

- **Node** (설정 계층): 빌드를 수행할 수 있는 머신의 설정을 영구 저장
- **Computer** (런타임 계층): Node의 실행 중 상태를 관리하며 Executor들을 보유
- **Executor** (실행 계층): 실제로 빌드를 수행하는 Java Thread

이 설계는 "설정과 런타임 분리" 패턴의 전형적인 사례다. 설정이 변경될 때 Node 객체는 새로 생성되지만, 진행 중인 빌드를 담당하는 Computer와 Executor는 그대로 유지된다.

```
소스코드 위치:
- core/src/main/java/hudson/model/Executor.java         (992줄)
- core/src/main/java/hudson/model/Computer.java         (1801줄)
- core/src/main/java/hudson/model/Node.java             (682줄)
- core/src/main/java/hudson/slaves/SlaveComputer.java
- core/src/main/java/hudson/model/OneOffExecutor.java
- core/src/main/java/hudson/model/Slave.java
- core/src/main/java/hudson/model/queue/WorkUnit.java
- core/src/main/java/jenkins/model/queue/AsynchronousExecution.java
- core/src/main/java/hudson/slaves/RetentionStrategy.java
- core/src/main/java/hudson/slaves/ComputerLauncher.java
```

---

## 1. Node - Computer - Executor 관계도

### 1.1 전체 클래스 계층

```
                        +-----------------+
                        |   Node          |  (abstract)
                        |   설정 전용      |  XStream 직렬화
                        +---------+-------+
                                  |
                   +--------------+--------------+
                   |                             |
          +--------+--------+           +--------+--------+
          |   Jenkins       |           |   Slave         |  (abstract)
          |   (마스터 노드)  |           |   (에이전트)     |
          +-----------------+           +--------+--------+
                                                 |
                                        +--------+--------+
                                        |   DumbSlave     |
                                        +-----------------+

                        +-----------------+
                        |   Computer      |  (abstract)
                        |   런타임 상태    |  transient
                        +---------+-------+
                                  |
                   +--------------+--------------+
                   |                             |
          +--------+--------+           +--------+--------+
          | MasterComputer  |           | SlaveComputer   |
          | (Jenkins 내부)   |           | (원격 에이전트)  |
          +-----------------+           +-----------------+

                        +-----------------+
                        |   Executor      |  extends Thread
                        |   빌드 실행      |
                        +---------+-------+
                                  |
                        +--------+--------+
                        | OneOffExecutor  |
                        | (FlyweightTask) |
                        +-----------------+
```

### 1.2 소유 관계

```
  Node (설정)                    Computer (런타임)
  +-----------+                  +-------------------------------------------+
  | nodeName  |  toComputer()   | executors: CopyOnWriteArrayList<Executor> |
  | label     | <=============> | oneOffExecutors: CopyOnWriteArrayList     |
  | numExec   |  getNode()      | offlineCause: OfflineCause                |
  | mode      |                 | numExecutors: int                         |
  | remoteFS  |                 | nodeName: String                          |
  +-----------+                 +-------------------+-----------------------+
                                                    |
                                                    | owner 참조
                                                    v
                                +-------------------------------------------+
                                | Executor #0 | Executor #1 | Executor #2  |
                                |  (idle)     |  (busy)     |  (idle)      |
                                +-------------------------------------------+
```

---

## 2. Executor - 빌드를 실행하는 스레드

### 2.1 클래스 선언과 핵심 필드

`Executor`는 `Thread`를 상속하며, 각 Executor는 한 번에 하나의 빌드만 실행한다.

```
// core/src/main/java/hudson/model/Executor.java (93행)
@ExportedBean
public class Executor extends Thread implements ModelObject, IExecutor {
    protected final @NonNull Computer owner;
    private final Queue queue;
    private final ReadWriteLock lock = new ReentrantReadWriteLock();

    @GuardedBy("lock")
    private long startTime;

    private int number;

    @GuardedBy("lock")
    private Queue.Executable executable;

    private long executableEstimatedDuration = DEFAULT_ESTIMATED_DURATION;

    @GuardedBy("lock")
    private AsynchronousExecution asynchronousExecution;

    @GuardedBy("lock")
    private WorkUnit workUnit;

    @GuardedBy("lock")
    private boolean started;

    @GuardedBy("lock")
    private Result interruptStatus;

    @GuardedBy("lock")
    private final List<CauseOfInterruption> causes = new Vector<>();
}
```

**필드별 역할 정리:**

| 필드 | 타입 | 역할 |
|------|------|------|
| `owner` | `Computer` | 이 Executor를 소유한 Computer. `@NonNull`, 생성 시 고정 |
| `queue` | `Queue` | Jenkins 전역 Queue 참조. 작업 수신에 사용 |
| `lock` | `ReadWriteLock` | `executable`, `workUnit` 등 상태 필드 보호 |
| `number` | `int` | 동일 Computer 내에서 식별하는 번호 (0, 1, 2...) |
| `startTime` | `long` | 현재 빌드의 시작 시각 (epoch ms) |
| `executable` | `Queue.Executable` | 현재 실행 중인 빌드. null이면 유휴 상태 |
| `workUnit` | `WorkUnit` | Queue에서 할당받은 작업 단위 |
| `started` | `boolean` | Thread.start()가 호출되었는지 여부 |
| `asynchronousExecution` | `AsynchronousExecution` | 비동기 실행 핸들. Pipeline 등에서 사용 |
| `interruptStatus` | `Result` | 인터럽트 시 설정할 빌드 결과 (ABORTED 등) |
| `causes` | `List<CauseOfInterruption>` | 인터럽트 원인 목록 (누가, 왜 중단했는지) |

### 2.2 생성자

```java
// Executor.java (151-156행)
public Executor(@NonNull Computer owner, int n) {
    super("Executor #" + n + " for " + owner.getDisplayName());
    this.owner = owner;
    this.queue = Jenkins.get().getQueue();
    this.number = n;
}
```

Executor는 생성 시 Thread 이름을 `"Executor #N for 노드이름"` 형태로 설정한다. 생성 직후에는 `started = false` 상태이며, 실제 Thread가 시작되지 않은 "파킹(parking)" 상태다.

### 2.3 start(WorkUnit) - 작업 시작

일반적인 `Thread.start()`는 호출할 수 없다. `UnsupportedOperationException`을 던진다.

```java
// Executor.java (806-819행)
@Override
public void start() {
    throw new UnsupportedOperationException();
}

/*protected*/ void start(WorkUnit task) {
    lock.writeLock().lock();
    try {
        this.workUnit = task;
        super.start();
        started = true;
    } finally {
        lock.writeLock().unlock();
    }
}
```

`start(WorkUnit)`은 Queue 시스템이 호출하며, 작업 단위(WorkUnit)를 할당하고 나서야 실제 Thread를 시작한다. 이 설계의 핵심 이유:

1. **On-demand 스레드 생성 (1.536+)**: Executor 객체는 미리 만들어 두되, 실제 OS 스레드는 작업이 할당될 때만 생성
2. **자원 효율성**: 유휴 Executor가 스레드를 점유하지 않음
3. **원자적 할당**: writeLock으로 보호하여 workUnit 설정과 Thread 시작이 원자적으로 수행

### 2.4 run() - 메인 실행 루프

`run()` 메서드는 Executor 스레드의 전체 생명주기를 관리한다. 아래는 소스코드에서 확인한 실제 흐름이다.

```
Executor.run() 흐름도
======================

[Thread 시작]
      |
      v
  +-----------------------+
  | 노드 상태 사전 점검    |  owner.isOnline()? owner.getNode() != null?
  | (MasterComputer 제외)  |  실패 시: resetWorkUnit() -> removeExecutor() -> return
  +-----------------------+
      |
      v
  +-----------------------+
  | startTime 기록         |  lock.writeLock 내에서 설정
  +-----------------------+
      |
      v
  +-----------------------+
  | ACL.as2(SYSTEM2)      |  시스템 권한으로 전환
  +-----------------------+
      |
      v
  +------------------------------+
  | Queue.callWithLock() 블록     |  Queue 락 내에서 원자적 수행:
  |   workUnit.setExecutor(this)  |    1. Executor를 WorkUnit에 연결
  |   queue.onStartExecuting()    |    2. Queue에 실행 시작 통보
  |   _task.createExecutable()    |    3. Executable 생성 (빌드 객체)
  |   executable = _executable    |    4. Executor에 Executable 설정
  +------------------------------+
      |
      v
  +-----------------------+
  | workUnit 검증          |  resetWorkUnit 호출되었으면 bail out
  +-----------------------+
      |
      v
  +-----------------------+
  | synchronizeStart()     |  멀티 SubTask의 동기화 시작점
  +-----------------------+
      |
      v
  +-----------------------+
  | Action 복사            |  Queue Item의 Action을 Executable에 복사
  | 인증 컨텍스트 설정      |  QueueItemAuthenticator 기반 인증
  +-----------------------+
      |
      v
  +-----------------------+
  | queue.execute()        |  === 실제 빌드 실행 ===
  | (executable, task)     |  내부에서 executable.run() 호출
  +-----------------------+
      |
      +---- 정상 완료 ----> finish1(null) -> finish2()
      |
      +---- AsynchronousExecution 예외 ----> asynchronousExecution 설정
      |                                      (Thread는 종료, 빌드는 계속)
      |
      +---- 기타 예외 ----> finish1(problems) -> finish2()
```

**핵심 코드 부분 (Executor.java 339-491행):**

```java
@Override
public void run() {
    // 1. 노드 상태 사전 점검 (마스터 제외)
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
    // ... startTime 설정 ...

    try (ACLContext ctx = ACL.as2(ACL.SYSTEM2)) {
        // 2. Queue 락 내에서 Executable 생성
        task = Queue.callWithLock(() -> {
            workUnit.setExecutor(Executor.this);
            queue.onStartExecuting(Executor.this);
            Executable _executable = _task.createExecutable();
            executable = _executable;
            workUnit.setExecutable(_executable);
            return _task;
        });

        // 3. 실제 빌드 실행
        try {
            workUnit.context.synchronizeStart();
            queue.execute(executable, task);
        } catch (AsynchronousExecution x) {
            // Pipeline 같은 비동기 빌드
            x.setExecutorWithoutCompleting(this);
            this.asynchronousExecution = x;
            x.maybeComplete();
        }
    }
}
```

### 2.5 finish1() / finish2() - 완료 처리

빌드 완료 시 두 단계로 정리 작업을 수행한다.

```java
// Executor.java (493-522행)
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
        workUnit.setExecutor(null);  // 양방향 참조 해제
    }
}

private void finish2() {
    owner.removeExecutor(this);     // Computer에서 제거
    if (this instanceof OneOffExecutor) {
        owner.remove((OneOffExecutor) this);
    }
    executableEstimatedDuration = DEFAULT_ESTIMATED_DURATION;
    queue.scheduleMaintenance();    // Queue 재점검 트리거
}
```

**finish1의 역할:**
- 에러가 있으면 WorkUnitContext에 abort 전파
- synchronizeEnd()로 다른 SubTask와 종료 동기화
- workUnit과 Executor 간 양방향 참조 해제

**finish2의 역할:**
- Computer의 executors 목록에서 자신을 제거
- Computer.addNewExecutorIfNecessary()를 트리거하여 새 Executor 보충
- Queue.scheduleMaintenance()로 대기 중인 작업을 재할당

### 2.6 interrupt(Result, CauseOfInterruption) - 빌드 중단

빌드 중단은 여러 오버로드를 통해 세밀하게 제어된다.

```java
// Executor.java (159-248행)
@Override
public void interrupt() {
    if (Thread.currentThread() == this) {
        // 자기 자신이 interrupt를 복원하는 경우
        // (InterruptedException catch 후 Thread.interrupted() 호출 패턴)
        super.interrupt();
    } else {
        // 다른 스레드에서 중단 요청
        interrupt(Result.ABORTED);
    }
}

private void interrupt(Result result, boolean forShutdown,
                       CauseOfInterruption... causes) {
    lock.writeLock().lock();
    try {
        if (!started) {
            // 아직 시작 안 됨 - 단순 제거
            owner.removeExecutor(this);
            return;
        }
        interruptStatus = result;

        for (CauseOfInterruption c : causes) {
            if (!this.causes.contains(c))
                this.causes.add(c);
        }

        if (asynchronousExecution != null) {
            // 비동기 실행 중이면 AsynchronousExecution에 위임
            asynchronousExecution.interrupt(forShutdown);
        } else {
            // 일반 동기 실행이면 Thread.interrupt()
            super.interrupt();
        }
    } finally {
        lock.writeLock().unlock();
    }
}
```

**인터럽트 설계의 핵심 포인트:**

1. **자기 자신의 인터럽트 복원 vs 외부 중단**: `Thread.currentThread() == this`로 구분. JENKINS-28690 데드락 방지
2. **CauseOfInterruption 추적**: 누가(UserInterruption), 왜(시스템 종료 등) 중단했는지 기록
3. **비동기 실행 지원**: `asynchronousExecution != null`이면 `AsynchronousExecution.interrupt()` 위임
4. **ReadWriteLock**: interrupt와 상태 접근 간의 동시성 제어

### 2.7 AsynchronousExecution - 비동기 빌드

Pipeline 빌드처럼 Executor Thread를 점유하지 않고 백그라운드에서 실행되는 빌드를 지원한다.

```
// jenkins/model/queue/AsynchronousExecution.java (58행)
public abstract class AsynchronousExecution extends RuntimeException {
    private Executor executor;
    private Throwable result;

    public abstract void interrupt(boolean forShutdown);
    public abstract boolean blocksRestart();
    public abstract boolean displayCell();
}
```

**동작 원리:**

```
  Executor.run()
       |
       v
  executable.run()  <-- Pipeline 실행
       |
       | throw new AsynchronousExecution(...)
       v
  catch (AsynchronousExecution x) {
      x.setExecutorWithoutCompleting(this);
      this.asynchronousExecution = x;
  }
       |
       v
  [Executor Thread 종료 - 하지만 isActive()는 여전히 true]
       |
       :
       :  (시간 경과 - 비동기 작업 진행 중)
       :
       v
  AsynchronousExecution.completed(error)
       |
       v
  Executor.completedAsynchronous(error)
       |
       v
  finish1(error) -> finish2()  [정상 종료 경로와 동일]
```

`isActive()` 메서드가 이를 정확히 반영한다:

```java
// Executor.java (647-654행)
public boolean isActive() {
    lock.readLock().lock();
    try {
        return !started || asynchronousExecution != null || isAlive();
    } finally {
        lock.readLock().unlock();
    }
}
```

- `!started`: 아직 시작 전이면 active (작업 대기 중)
- `asynchronousExecution != null`: Thread는 죽었지만 비동기 작업 진행 중
- `isAlive()`: Thread가 살아있으면 active

### 2.8 상태 판별 메서드

```java
// Executor.java
public boolean isIdle() {
    return workUnit == null && executable == null;
}

public boolean isBusy() {
    return workUnit != null || executable != null;
}

public boolean isParking() {
    return !started;  // Thread 시작 전 (작업 대기 중)
}
```

### 2.9 진행률과 이상 감지

```java
// Executor.java (707-741행)
@Exported
public int getProgress() {
    long d = executableEstimatedDuration;
    if (d <= 0) return DEFAULT_ESTIMATED_DURATION;  // -1
    int num = (int) (getElapsedTime() * 100 / d);
    if (num >= 100) num = 99;  // 100%에 도달하면 99%로 표시
    return num;
}

@Exported
public boolean isLikelyStuck() {
    if (executable == null) return false;
    long elapsed = getElapsedTime();
    long d = executableEstimatedDuration;
    if (d >= 0) {
        // ETA의 10배 초과 -> stuck으로 판정
        return d * 10 < elapsed;
    } else {
        // ETA 없으면 24시간 초과 시 stuck
        return TimeUnit.MILLISECONDS.toHours(elapsed) > 24;
    }
}
```

### 2.10 currentExecutor()와 IMPERSONATION

```java
// Executor.java (948-952행)
public static @CheckForNull Executor currentExecutor() {
    Thread t = Thread.currentThread();
    if (t instanceof Executor) return (Executor) t;
    return IMPERSONATION.get();
}

private static final ThreadLocal<Executor> IMPERSONATION = new ThreadLocal<>();
```

`currentExecutor()`는 현재 스레드가 Executor인지, 또는 Executor를 대리(impersonation)하는 스레드인지 확인한다. 원격 채널 요청 처리 스레드가 Executor의 컨텍스트에서 동작해야 할 때 `newImpersonatingProxy()`로 IMPERSONATION ThreadLocal을 설정한다.

---

## 3. Computer - Node의 런타임 상태

### 3.1 클래스 선언과 핵심 필드

```java
// core/src/main/java/hudson/model/Computer.java (173-220행)
@ExportedBean
public abstract class Computer extends Actionable
    implements AccessControlled, IComputer, ExecutorListener,
               DescriptorByNameOwner, StaplerProxy, HasWidgets {

    private final CopyOnWriteArrayList<Executor> executors =
        new CopyOnWriteArrayList<>();
    private final CopyOnWriteArrayList<OneOffExecutor> oneOffExecutors =
        new CopyOnWriteArrayList<>();

    private int numExecutors;
    protected volatile OfflineCause offlineCause;
    private long connectTime = 0;
    protected String nodeName;

    private volatile String cachedHostName;
    private volatile EnvVars cachedEnvironment;

    private final WorkspaceList workspaceList = new WorkspaceList();
    protected final Object statusChangeLock = new Object();

    private final transient List<TerminationRequest> terminatedBy =
        Collections.synchronizedList(new ArrayList<>());
}
```

**핵심 필드 설명:**

| 필드 | 타입 | 역할 |
|------|------|------|
| `executors` | `CopyOnWriteArrayList<Executor>` | 일반 Executor 목록. Thread-safe |
| `oneOffExecutors` | `CopyOnWriteArrayList<OneOffExecutor>` | FlyweightTask 전용 일회성 Executor |
| `numExecutors` | `int` | 설정된 Executor 수 |
| `offlineCause` | `OfflineCause` | 오프라인 원인 (volatile) |
| `nodeName` | `String` | 대응 Node의 이름 (null이면 마스터) |
| `workspaceList` | `WorkspaceList` | 워크스페이스 할당 조정 |
| `terminatedBy` | `List<TerminationRequest>` | 종료 요청 이력 (디버깅용) |

**왜 CopyOnWriteArrayList를 사용하는가?**

Executor 목록은 빈번하게 읽히지만(UI 표시, 상태 확인), 수정은 드물다(Executor 추가/제거). `CopyOnWriteArrayList`는 이 읽기 위주 패턴에 최적화되어 있다:
- 읽기 시 잠금 없음
- 쓰기 시 내부 배열 복사 (쓰기가 드물므로 비용이 감수 가능)
- Iterator가 snapshot을 반환하므로 ConcurrentModificationException 없음

### 3.2 setNumExecutors() - Executor 수 동적 조정

```java
// Computer.java (861-906행)
@Restricted(NoExternalUse.class)
@GuardedBy("hudson.model.Queue.lock")
public void setNumExecutors(int n) {
    this.numExecutors = n;
    final int diff = executors.size() - n;

    if (diff > 0) {
        // Executor가 너무 많음 -> 유휴 Executor에게 interrupt 전송
        Queue.withLock(() -> {
            for (Executor e : executors) {
                if (e.isIdle()) {
                    e.interrupt();
                }
            }
        });
    }

    if (diff < 0) {
        // Executor 부족 -> 새로 추가
        addNewExecutorIfNecessary();
    }
}

private void addNewExecutorIfNecessary() {
    Set<Integer> availableNumbers = new HashSet<>();
    for (int i = 0; i < numExecutors; i++)
        availableNumbers.add(i);

    for (Executor executor : executors)
        availableNumbers.remove(executor.getNumber());

    for (Integer number : availableNumbers) {
        if (executors.size() < numExecutors) {
            Executor e = new Executor(this, number);
            executors.add(e);
        }
    }
}
```

**동적 조정의 핵심 로직:**

```
  setNumExecutors(n) 호출
       |
       v
  diff = executors.size() - n
       |
       +-- diff > 0 (줄여야 함)
       |       |
       |       v
       |   Queue.withLock 내에서:
       |     유휴 Executor에게 interrupt()
       |     -> 유휴 Executor는 removeExecutor()로 자발적 종료
       |     -> 바쁜 Executor는 작업 완료 후 자연 종료
       |
       +-- diff < 0 (늘려야 함)
       |       |
       |       v
       |   addNewExecutorIfNecessary():
       |     사용 가능한 번호 계산
       |     새 Executor 객체 생성 (아직 Thread 미시작)
       |
       +-- diff == 0 (변경 없음)
              -> 아무것도 안 함
```

Executor 수를 줄일 때 바쁜 Executor를 강제 종료하지 않는 점이 중요하다. 유휴 Executor만 interrupt하고, 바쁜 Executor는 현재 빌드가 끝나면 자연스럽게 `finish2()`에서 `removeExecutor()`를 호출하고, 이때 `addNewExecutorIfNecessary()`가 다시 불려서 적정 수를 맞춘다.

### 3.3 removeExecutor() - Executor 제거와 보충

```java
// Computer.java (1063-1083행)
protected void removeExecutor(final Executor e) {
    final Runnable task = () -> {
        synchronized (Computer.this) {
            executors.remove(e);
            oneOffExecutors.remove(e);
            addNewExecutorIfNecessary();  // 부족하면 보충
            if (!isAlive()) {
                // 모든 Executor가 비활성 -> Computer 자체 제거
                AbstractCIBase ciBase = Jenkins.getInstanceOrNull();
                if (ciBase != null) {
                    ciBase.removeComputer(Computer.this);
                }
            } else if (isIdle()) {
                // 모든 Executor 유휴 -> ComputerListener.onIdle 통보
                threadPoolForRemoting.submit(() ->
                    Listeners.notify(ComputerListener.class, false,
                                     l -> l.onIdle(this)));
            }
        }
    };
    if (!Queue.tryWithLock(task)) {
        // 락 획득 실패 시 별도 스레드로 위임 (JENKINS-28840 데드락 방지)
        threadPoolForRemoting.submit(Queue.wrapWithLock(task));
    }
}
```

### 3.4 통계 메서드

```java
// Computer.java (911-934행)
public int countIdle() {
    int n = 0;
    for (Executor e : executors) {
        if (e.isIdle()) n++;
    }
    return n;
}

public final int countBusy() {
    return countExecutors() - countIdle();
}

public final int countExecutors() {
    return executors.size();
}
```

### 3.5 FlyweightTask와 OneOffExecutor

```java
// Computer.java (1292-1300행)
/*package*/ final void startFlyWeightTask(WorkUnit p) {
    OneOffExecutor e = new OneOffExecutor(this);
    e.start(p);
    oneOffExecutors.add(e);
}

/*package*/ final void remove(OneOffExecutor e) {
    oneOffExecutors.remove(e);
}
```

OneOffExecutor는 Matrix 프로젝트의 부모 빌드처럼 "가벼운" 작업을 위한 일회성 Executor다:

```java
// core/src/main/java/hudson/model/OneOffExecutor.java (36-40행)
public class OneOffExecutor extends Executor {
    public OneOffExecutor(Computer owner) {
        super(owner, -1);  // number = -1 (일반 Executor와 구별)
    }
}
```

- 일반 Executor 슬롯을 소비하지 않음
- `number = -1`로 고정
- `oneOffExecutors` 목록에서 별도 관리
- 작업 완료 시 `finish2()`에서 `owner.remove((OneOffExecutor) this)` 호출

### 3.6 오프라인 관리

```java
// Computer.java (362-373행)
@Exported
public OfflineCause getOfflineCause() {
    var node = getNode();
    if (node != null) {
        var temporaryOfflineCause = node.getTemporaryOfflineCause();
        if (temporaryOfflineCause != null) {
            return temporaryOfflineCause;
        }
    }
    return offlineCause;
}

// Computer.java (620-626행)
public boolean isOffline() {
    return isTemporarilyOffline() || getChannel() == null;
}

public final boolean isOnline() {
    return !isOffline();
}
```

**OfflineCause 계층:**

```
  OfflineCause (abstract)
      |
      +-- SimpleOfflineCause
      |     |
      |     +-- ChannelTermination (채널 에러로 오프라인)
      |     +-- LaunchFailed (런치 실패)
      |     +-- IdleOfflineCause (유휴 상태로 인한 오프라인)
      |
      +-- UserCause (관리자가 수동으로 오프라인)
      |
      +-- ByCLI (CLI 명령으로 오프라인)
      |
      +-- LegacyOfflineCause (하위 호환)
```

### 3.7 Computer의 전체 생명주기

```
  [Node 설정 생성/변경]
        |
        v
  Jenkins.updateComputerList()
        |
        v
  +-- 새 Node? --> Computer 생성 (node.createComputer())
  |                     |
  |                     v
  |               Computer.setNode(node)
  |                     |
  |                     v
  |               setNumExecutors(node.getNumExecutors())
  |                     |
  |                     v
  |               addNewExecutorIfNecessary() --> Executor 객체 생성
  |
  +-- 기존 Node 변경? --> Computer.setNode(updatedNode)
  |                            |
  |                            v
  |                      setNumExecutors() --> Executor 수 동적 조정
  |
  +-- Node 제거? --> Computer.kill()
                         |
                         v
                   setNumExecutors(0) --> 유휴 Executor interrupt
                         |
                         v
                   [모든 Executor 종료 대기]
                         |
                         v
                   isAlive() == false
                         |
                         v
                   removeComputer() --> Computer.onRemoved()
```

---

## 4. Node - 설정 전용 계층

### 4.1 클래스 구조

```java
// core/src/main/java/hudson/model/Node.java (107행)
@ExportedBean
public abstract class Node extends AbstractModelObject
    implements ReconfigurableDescribable<Node>, ExtensionPoint,
               AccessControlled, OnMaster, PersistenceRoot {

    private static final Logger LOGGER = Logger.getLogger(Node.class.getName());

    protected transient volatile boolean holdOffLaunchUntilSave;
    private transient Nodes parent;

    // 핵심 추상 메서드들
    public abstract String getNodeName();
    public abstract String getNodeDescription();
    public abstract int getNumExecutors();
    public abstract Mode getMode();
    public abstract String getLabelString();
    public abstract FilePath getWorkspaceFor(TopLevelItem item);
    public abstract FilePath getRootPath();
    protected abstract Computer createComputer();
}
```

**Node의 Javadoc (실제 소스에서 발췌):**

```
 * Nodes are persisted objects that capture user configurations, and
 * instances get thrown away and recreated whenever the configuration
 * changes. Running state of nodes are captured by Computers.
 *
 * There is no URL binding for Node. Computer and
 * TransientComputerActionFactory must be used to associate new
 * Actions to agents.
```

### 4.2 Node의 핵심 특성

| 항목 | 설명 |
|------|------|
| 영속성 | XStream으로 XML 직렬화. 설정 변경 시 새 객체 생성 |
| transient 필드 | `holdOffLaunchUntilSave`, `parent` - 런타임에만 존재 |
| URL 바인딩 없음 | Node 자체에는 웹 URL이 없음. Computer가 UI 담당 |
| 불변에 가까움 | 이름은 사실상 immutable (setNodeName은 @Deprecated) |

### 4.3 toComputer()

```java
// Node.java (222-226행)
@CheckForNull
public final Computer toComputer() {
    AbstractCIBase ciBase = Jenkins.get();
    return ciBase.getComputer(this);
}
```

Node에서 대응하는 Computer를 찾는 메서드. Jenkins 인스턴스에 등록된 Computer 중 이 Node와 연결된 것을 반환한다. Executor가 0개이면 Computer가 없을 수 있다 (null 반환).

### 4.4 canTake(BuildableItem) - 작업 수용 가능 여부

```java
// Node.java (427-470행)
public CauseOfBlockage canTake(Queue.BuildableItem item) {
    Label l = item.getAssignedLabel();

    // 1. 레이블 매칭 확인
    if (l != null && !l.contains(this))
        return CauseOfBlockage.fromMessage(
            Messages._Node_LabelMissing(getDisplayName(), l));

    // 2. EXCLUSIVE 모드 확인
    if (l == null && getMode() == Mode.EXCLUSIVE) {
        if (!(item.task instanceof Queue.FlyweightTask && ...)) {
            return CauseOfBlockage.fromMessage(
                Messages._Node_BecauseNodeIsReserved(getDisplayName()));
        }
    }

    // 3. 빌드 권한 확인
    Authentication identity = item.authenticate2();
    if (!hasPermission2(identity, Computer.BUILD)) {
        return CauseOfBlockage.fromMessage(
            Messages._Node_LackingBuildPermission(...));
    }

    // 4. NodeProperty별 추가 검사
    for (NodeProperty prop : getNodeProperties()) {
        CauseOfBlockage c = prop.canTake(item);
        if (c != null) return c;
    }

    // 5. isAcceptingTasks() 확인
    if (!isAcceptingTasks()) {
        return new CauseOfBlockage.BecauseNodeIsNotAcceptingTasks(this);
    }

    return null;  // 수용 가능
}
```

### 4.5 Mode 열거형

```java
// Node.java (647-668행)
public enum Mode {
    NORMAL(Messages._Node_Mode_NORMAL()),      // 아무 작업이나 받음
    EXCLUSIVE(Messages._Node_Mode_EXCLUSIVE()); // 이 노드에 묶인 작업만 받음
}
```

### 4.6 Jenkins도 Node를 상속한다

Jenkins 컨트롤러(마스터 노드) 자체가 `Node`의 하위 클래스다:

```
  Jenkins extends AbstractCIBase
  AbstractCIBase extends Node
```

Jenkins 내부에 `MasterComputer`가 정의되어 있다:

```java
// jenkins/model/Jenkins.java (5392행)
public static class MasterComputer extends Computer {
    protected MasterComputer() {
        super(Jenkins.get());
    }
}
```

따라서 마스터 노드도 동일한 Node -> Computer -> Executor 구조를 따르며, 마스터에서도 빌드를 실행할 수 있다.

---

## 5. Slave와 SlaveComputer - 원격 에이전트

### 5.1 Slave 클래스

```java
// core/src/main/java/hudson/model/Slave.java (107-149행)
public abstract class Slave extends Node implements Serializable {
    protected String name;
    private String description;
    protected final String remoteFS;         // 원격 파일시스템 루트 경로
    private int numExecutors = 1;
    private Mode mode = Mode.NORMAL;
    private RetentionStrategy retentionStrategy;
    private ComputerLauncher launcher;
}
```

Slave는 Node의 구체 구현으로, 원격 에이전트의 설정을 담는다:
- `remoteFS`: 에이전트 머신의 워크스페이스 루트 경로
- `retentionStrategy`: 에이전트 생존 전략
- `launcher`: 에이전트 연결 방식

### 5.2 SlaveComputer 클래스

```java
// core/src/main/java/hudson/slaves/SlaveComputer.java (112-160행)
public class SlaveComputer extends Computer {
    private volatile Channel channel;               // 원격 통신 채널
    private transient volatile boolean acceptingTasks = true;
    private Charset defaultCharset;
    private Boolean isUnix;
    private ComputerLauncher launcher;              // 실제 런처
    private final RewindableFileOutputStream log;   // 에이전트 로그
    private final TaskListener taskListener;
    private transient int numRetryAttempt;
    private volatile Future<?> lastConnectActivity = null;
    private transient volatile String absoluteRemoteFs;
}
```

### 5.3 Channel 기반 원격 통신

SlaveComputer의 핵심은 `hudson.remoting.Channel`을 통한 원격 통신이다.

```
  [Jenkins 컨트롤러]                  [에이전트 머신]
  +------------------+               +------------------+
  |  SlaveComputer   |               |  Agent Process   |
  |  +----------+    |               |                  |
  |  | Channel  | <==|== TCP/SSH ==> |  remoting.jar    |
  |  +----------+    |               |                  |
  |  | Executor |    |               |  workspace/      |
  |  | Executor |    |               |  tools/          |
  |  +----------+    |               +------------------+
  +------------------+
```

연결 설정 과정:

```java
// SlaveComputer.java (279-328행)
@Override
protected Future<?> _connect(boolean forceReconnect) {
    if (channel != null) return Futures.precomputed(null);
    if (!forceReconnect && isConnecting())
        return lastConnectActivity;

    closeChannel();
    return lastConnectActivity = Computer.threadPoolForRemoting.submit(() -> {
        try (ACLContext ctx = ACL.as2(ACL.SYSTEM2)) {
            log.rewind();
            try {
                for (ComputerListener cl : ComputerListener.all())
                    cl.preLaunch(SlaveComputer.this, taskListener);
                offlineCause = null;
                launcher.launch(SlaveComputer.this, taskListener);
            } catch (...) {
                // 에러 처리
            } finally {
                if (channel == null && offlineCause == null) {
                    offlineCause = new OfflineCause.LaunchFailed();
                }
            }
        }
    });
}
```

### 5.4 ComputerLauncher - 연결 방식

```java
// core/src/main/java/hudson/slaves/ComputerLauncher.java (60행)
public abstract class ComputerLauncher
    implements Describable<ComputerLauncher>, ExtensionPoint {

    public boolean isLaunchSupported() { return true; }

    public void launch(SlaveComputer computer, TaskListener listener)
        throws IOException, InterruptedException { ... }
}
```

주요 구현체:

| Launcher | 설명 |
|----------|------|
| `SSHLauncher` | SSH로 에이전트에 접속하여 agent.jar 실행 |
| `JNLPLauncher` | 에이전트가 컨트롤러에 inbound 연결 (WebSocket/TCP) |
| `CommandLauncher` | 사용자 정의 명령어로 에이전트 시작 |

### 5.5 RetentionStrategy - 에이전트 생존 전략

```java
// core/src/main/java/hudson/slaves/RetentionStrategy.java (54행)
public abstract class RetentionStrategy<T extends Computer>
    implements Describable<RetentionStrategy<?>>, ExtensionPoint {

    @GuardedBy("hudson.model.Queue.lock")
    public abstract long check(@NonNull T c);

    public boolean isManualLaunchAllowed(T c) { return true; }
    public boolean isAcceptingTasks(T c) { return true; }
}
```

**기본 제공 전략:**

```java
// Always (항상 온라인 유지)
public static class Always extends RetentionStrategy<SlaveComputer> {
    @Override
    public long check(SlaveComputer c) {
        if (c.isOffline() && !c.isConnecting() && c.isLaunchSupported())
            c.tryReconnect();
        return 0;  // 즉시 재확인
    }
}

// Demand (필요할 때만 온라인)
public static class Demand extends RetentionStrategy<SlaveComputer> {
    private final long inDemandDelay;  // 요구 후 대기 시간 (분)
    private final long idleDelay;      // 유휴 후 오프라인까지 시간 (분)

    @Override
    public long check(final SlaveComputer c) {
        if (c.isOffline() && c.isLaunchSupported()) {
            // Queue에 이 노드가 필요한 작업이 있는지 확인
            // inDemandDelay 이상 기다렸으면 c.connect(false)
        } else if (c.isIdle()) {
            // idleDelay 이상 유휴이면 c.disconnect()
        }
        return 0;
    }
}
```

### 5.6 ExecutorListener 이벤트

SlaveComputer는 `ExecutorListener`를 구현하여 빌드 생명주기 이벤트를 처리한다:

```java
// SlaveComputer.java (332-377행)
@Override
public void taskAccepted(Executor executor, Queue.Task task) {
    // ComputerLauncher와 RetentionStrategy에게 통보
    if (launcher instanceof ExecutorListener) {
        ((ExecutorListener) launcher).taskAccepted(executor, task);
    }
    Slave node = getNode();
    if (node != null && node.getRetentionStrategy() instanceof ExecutorListener) {
        ((ExecutorListener) node.getRetentionStrategy()).taskAccepted(executor, task);
    }
}

@Override
public void taskStarted(Executor executor, Queue.Task task) { ... }

@Override
public void taskCompleted(Executor executor, Queue.Task task, long durationMS) { ... }

@Override
public void taskCompletedWithProblems(Executor executor, Queue.Task task,
                                       long durationMS, Throwable problems) { ... }
```

---

## 6. 설정과 런타임 분리 패턴

### 6.1 왜 Node와 Computer를 분리하는가?

Jenkins 소스코드의 주석에서 직접 밝히고 있다:

```
// Node.java (88-100행) - Javadoc
"Nodes are persisted objects that capture user configurations,
 and instances get thrown away and recreated whenever the
 configuration changes. Running state of nodes are captured
 by Computers."
```

```
// Computer.java (147-168행) - Javadoc
"This object is related to Node but they have some significant
 differences. Computer primarily works as a holder of Executors,
 so if a Node is configured (probably temporarily) with 0 executors,
 you won't have a Computer object for it."

"Also, even if you remove a Node, it takes time for the corresponding
 Computer to be removed, if some builds are already in progress on
 that node."
```

### 6.2 분리의 실질적 이점

```
  시나리오: 관리자가 에이전트의 Executor 수를 3에서 5로 변경

  [Node 레이어]                     [Computer 레이어]
  +-----------------+               +--------------------------+
  | Node(old)       |  -- 폐기 -->  | Computer (유지)           |
  | numExecutors: 3 |               | executors: [E0, E1, E2]  |
  +-----------------+               |            (E1은 빌드 중)  |
                                    +--------------------------+
  +-----------------+                         |
  | Node(new)       |  setNode() -->         v
  | numExecutors: 5 |               +--------------------------+
  +-----------------+               | Computer (갱신)           |
                                    | executors: [E0, E1, E2,  |
                                    |            E3, E4]       |
                                    | E1의 빌드는 계속 진행      |
                                    +--------------------------+
```

**핵심 이점:**

| 관심사 | Node | Computer |
|--------|------|----------|
| 영속성 | XStream 직렬화 | transient (메모리 전용) |
| 생명주기 | 설정 변경 시 재생성 | 빌드 완료까지 유지 |
| URL 바인딩 | 없음 | `/computer/NAME/` |
| 상태 | immutable에 가까움 | mutable (온/오프라인 전환) |
| 직렬화 | 파일로 저장 | 저장 안 함 |

### 6.3 코드에서 확인한 분리 패턴

```java
// Computer.java (805-813행)
protected void setNode(Node node) {
    assert node != null;
    if (node instanceof Slave)
        this.nodeName = node.getNodeName();
    else
        this.nodeName = null;
    setNumExecutors(node.getNumExecutors());
}
```

setNode()은 Node 객체에서 설정값만 가져와 Computer의 런타임 상태를 갱신한다. Computer는 Node의 참조를 직접 보관하지 않고, `nodeName`으로 간접 참조한다.

```java
// Computer.java (596-605행)
@CheckForNull
public Node getNode() {
    Jenkins j = Jenkins.getInstanceOrNull();
    if (j == null) return null;
    if (nodeName == null) return j;          // 마스터 노드
    return j.getNode(nodeName);              // 이름으로 조회
}
```

---

## 7. 작업 할당 흐름

### 7.1 전체 시퀀스

```
  [사용자]             [Queue]              [Computer]          [Executor]
     |                   |                     |                    |
     | 빌드 트리거        |                     |                    |
     |=================>|                     |                    |
     |                   |                     |                    |
     |                   | maintain() 실행      |                    |
     |                   |-- 적합 노드 탐색 ---->|                    |
     |                   |                     |                    |
     |                   | canTake(item) 확인   |                    |
     |                   |<----- null(OK) -----|                    |
     |                   |                     |                    |
     |                   | 유휴 Executor 탐색    |                    |
     |                   |-------------------->| countIdle() > 0    |
     |                   |                     |                    |
     |                   | executor.start(wu)   |                    |
     |                   |-------------------->|------------------->|
     |                   |                     |                    |
     |                   |                     |                    | Thread 시작
     |                   |                     |                    | run() 진입
     |                   |                     |                    |
     |                   | onStartExecuting()   |                    |
     |                   |<------------------------------------- ---|
     |                   |                     |                    |
     |                   |                     |                    | createExecutable()
     |                   |                     |                    | executable.run()
     |                   |                     |                    |
     |                   |                     |                    | (빌드 실행 중)
     |                   |                     |                    |
     |                   |                     |                    | finish1()
     |                   |                     |                    | finish2()
     |                   |                     |                    |   |
     |                   |                     | removeExecutor(e)  |<--+
     |                   |                     | addNewExecutor()   |
     |                   |                     |                    |
     |                   | scheduleMaintenance()|                    |
     |                   |<------------------------------------- ---|
     |                   |                     |                    |
```

### 7.2 WorkUnit의 역할

```java
// core/src/main/java/hudson/model/queue/WorkUnit.java (42-112행)
public final class WorkUnit {
    public final SubTask work;           // 실행할 작업
    public final WorkUnitContext context; // 공유 컨텍스트
    private volatile Executor executor;  // 실행 담당 Executor
    private Executable executable;       // 생성된 빌드 객체
}
```

WorkUnit은 Queue와 Executor 사이의 "손전달(hand-over)" 객체다:

```
  Queue                    WorkUnit                  Executor
  +-------+    생성    +---------------+    전달    +----------+
  | Item  | -------->  | work (SubTask)|  -------> | workUnit |
  |       |            | context       |           | executor |
  +-------+            | executor <===========>    +----------+
                       | executable    |
                       +---------------+
```

WorkUnit과 Executor는 양방향 참조를 형성한다:
- `workUnit.setExecutor(executor)` -- Executor.run() 내에서 호출
- `executor.workUnit` -- 생성자에서 설정

### 7.3 FlyweightTask 처리

일반 작업과 FlyweightTask의 할당 경로가 다르다:

```
  일반 Task:
    Queue.maintain()
      -> 적합 Computer의 유휴 Executor 탐색
      -> executor.start(workUnit)       // 기존 Executor 재사용
      -> Executor.run()

  FlyweightTask:
    Queue.maintain()
      -> Computer.startFlyWeightTask(workUnit)
      -> new OneOffExecutor(computer)   // 임시 Executor 생성
      -> oneOffExecutor.start(workUnit)
      -> OneOffExecutor.run()
      -> finish2()에서 oneOffExecutors에서 제거
```

FlyweightTask는 일반 Executor 슬롯을 차지하지 않으므로, 모든 Executor가 바빠도 실행할 수 있다. Matrix 프로젝트의 부모 빌드가 대표적인 예다.

---

## 8. 동시성 제어와 스레드 안전성

### 8.1 ReadWriteLock in Executor

Executor에서 가장 중요한 동시성 메커니즘은 `ReentrantReadWriteLock`이다.

```
  ReadLock (공유 읽기):
    - getCurrentExecutable()
    - getCurrentWorkUnit()
    - isIdle() / isBusy()
    - isActive()
    - getProgress()
    - getElapsedTime()

  WriteLock (배타적 쓰기):
    - start(WorkUnit): workUnit 설정 + Thread 시작
    - run() 내부: executable 설정
    - interrupt(): interruptStatus + causes 설정
    - abortResult(): interruptStatus 읽기 (writeLock 사용!)
```

`abortResult()`가 writeLock을 사용하는 이유:

```java
// Executor.java (250-268행)
public Result abortResult() {
    // 인터럽트 플래그를 먼저 정리
    Thread.interrupted();
    // writeLock을 사용하는 이유: 반복적 인터럽트 상황에서
    // interrupt()의 writeLock과 동일한 락이 필요 (JENKINS-28690)
    lock.writeLock().lock();
    try {
        Result r = interruptStatus;
        if (r == null) r = Result.ABORTED;
        return r;
    } finally {
        lock.writeLock().unlock();
    }
}
```

### 8.2 Queue.lock과의 상호작용

Queue 시스템과 Executor 시스템은 `Queue.lock`을 공유한다:

```java
// Computer.java (869-876행) - setNumExecutors 내부
Queue.withLock(() -> {
    for (Executor e : executors) {
        if (e.isIdle()) {
            e.interrupt();
        }
    }
});

// Computer.java (1079-1082행) - removeExecutor 내부
if (!Queue.tryWithLock(task)) {
    // 데드락 방지: 락 획득 실패 시 별도 스레드로 위임
    threadPoolForRemoting.submit(Queue.wrapWithLock(task));
}
```

`Queue.tryWithLock()`은 JENKINS-28840에서 발견된 데드락을 해결하기 위해 도입되었다. Executor가 작업 완료 후 `removeExecutor()`를 호출할 때, 이미 다른 스레드가 Queue 락을 잡고 있으면 데드락이 발생할 수 있다. 이를 방지하기 위해 비차단 시도(`tryWithLock`)를 먼저 하고, 실패하면 별도 스레드로 위임한다.

### 8.3 CopyOnWriteArrayList 사용 패턴

```java
// Computer.java (175-177행)
private final CopyOnWriteArrayList<Executor> executors = new CopyOnWriteArrayList<>();
private final CopyOnWriteArrayList<OneOffExecutor> oneOffExecutors = new CopyOnWriteArrayList<>();
```

읽기/순회가 빈번하고 수정이 드문 패턴에 최적:
- `countIdle()`, `countBusy()`, `isIdle()`, `getExecutors()` 등 빈번한 읽기
- `addNewExecutorIfNecessary()`, `removeExecutor()` 등 드문 수정

---

## 9. 에이전트 연결 과정 상세

### 9.1 SSH 에이전트 연결 시퀀스

```
  [Jenkins 컨트롤러]                        [에이전트 머신]
        |                                        |
  RetentionStrategy.check()                      |
        |                                        |
  SlaveComputer._connect()                       |
        |                                        |
  threadPoolForRemoting.submit(lambda)           |
        |                                        |
  ComputerListener.preLaunch()                   |
        |                                        |
  launcher.launch(slaveComputer, taskListener)   |
        |                                        |
        | ---- SSH 연결 시작 ------>              |
        |                                        |
        | ---- agent.jar 전송 ---->               |
        |                                        |
        | ---- java -jar agent.jar 실행 ---->     |
        |                                        |
        | <---- Channel 연결 수립 ----            |
        |                                        |
  slaveComputer.setChannel(in, out, ...)         |
        |                                        |
  channel.call(new SlaveVersion())               |
        | <---- Remoting 버전 응답 ----           |
        |                                        |
  channel.call(new DetectOS())                   |
        | <---- Unix/Windows 응답 ----            |
        |                                        |
  ComputerListener.onOnline()                    |
        |                                        |
  [에이전트 온라인 - Executor 작업 수용 가능]       |
```

### 9.2 JNLP(Inbound) 에이전트 연결

```
  [에이전트 머신]                        [Jenkins 컨트롤러]
        |                                        |
        | ---- HTTP GET /jnlpJars/agent.jar ---->|
        | <---- agent.jar 다운로드 ----           |
        |                                        |
  java -jar agent.jar -url URL -secret MAC       |
        |                                        |
        | ---- WebSocket/TCP inbound 연결 ------>|
        |                                        |
        |              JnlpAgentReceiver.handle() |
        |                                        |
        |              SlaveComputer.setChannel() |
        |                                        |
        |              ComputerListener.onOnline()|
        |                                        |
  [에이전트 온라인]                               |
```

---

## 10. 모니터링과 진단

### 10.1 Executor 상태 모니터링

Jenkins UI의 "Build Executor Status" 위젯은 다음 메서드들을 사용한다:

```java
// Computer.java (973-991행)
public List<IDisplayExecutor> getDisplayExecutors() {
    List<IDisplayExecutor> result = new ArrayList<>();
    int index = 0;
    for (Executor e : executors) {
        if (e.isDisplayCell()) {
            result.add(new DisplayExecutor(
                Integer.toString(index + 1),
                String.format("executors/%d", index), e));
        }
        index++;
    }
    for (OneOffExecutor e : oneOffExecutors) {
        if (e.isDisplayCell()) {
            result.add(new DisplayExecutor("",
                String.format("oneOffExecutors/%d", index), e));
        }
        index++;
    }
    return result;
}
```

### 10.2 Stuck 빌드 감지

```
  isLikelyStuck() 판정 기준:

  +-- ETA가 있는 경우:
  |     경과 시간 > ETA * 10  ->  stuck!
  |     예: ETA 10분인 빌드가 100분 넘게 실행 중
  |
  +-- ETA가 없는 경우:
        경과 시간 > 24시간    ->  stuck!
```

### 10.3 TerminationRequest 추적

```java
// Computer.java (230-250행)
public void recordTermination() {
    StaplerRequest2 request = Stapler.getCurrentRequest2();
    if (request != null) {
        terminatedBy.add(new TerminationRequest(
            String.format("Termination requested at %s by %s [id=%d] from HTTP request for %s",
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

이 정보는 Executor.resetWorkUnit()에서 사용되어, 작업이 할당된 후 노드가 제거된 경우의 디버깅에 활용된다.

---

## 11. 설계 결정의 이유 (Why)

### 11.1 왜 Executor는 Thread를 상속하는가?

Executor가 `Runnable`을 구현하는 대신 `Thread`를 직접 상속하는 이유:

1. **`currentExecutor()` 패턴**: `Thread.currentThread()`로 현재 Executor를 즉시 식별
2. **Thread 이름 제어**: 빌드 실행 중 Thread 이름을 동적으로 변경 (디버깅 용이)
3. **인터럽트 오버라이드**: `interrupt()` 메서드를 재정의하여 빌드 중단과 Java 인터럽트를 구분

```java
// Executor.java (443행) - Thread 이름 동적 변경
setName(getName() + " : executing " + executable);
```

### 11.2 왜 On-demand Thread 시작인가? (1.536+)

Jenkins 1.536 이전에는 Executor가 생성되자마자 Thread가 시작되었고, Thread.wait()으로 작업을 기다렸다. 이 방식의 문제:

- 많은 에이전트가 있으면 유휴 Thread가 메모리를 불필요하게 점유
- Thread dump에 유휴 Executor Thread가 가득 차 디버깅이 어려움
- Thread 수 제한이 있는 환경에서 확장성 문제

현재 방식은 Executor 객체만 미리 만들어 두고, 실제 Thread는 `start(WorkUnit)` 호출 시에만 생성한다. `isActive()`가 `isAlive()` 대신 사용해야 하는 이유도 여기에 있다.

### 11.3 왜 CauseOfInterruption을 별도로 추적하는가?

단순히 Thread.interrupt()로 빌드를 중단하면 "왜" 중단되었는지 알 수 없다. Jenkins는 여러 원인을 동시에 기록할 수 있다:

```java
// 예: 관리자 A가 중단 + 시스템 종료가 동시에 발생
interrupt(Result.ABORTED, new UserInterruption("admin-a"));
// ... 바로 뒤에 ...
interruptForShutdown();  // Result.ABORTED + 빈 causes

// recordCauseOfInterruption()에서 Build에 InterruptedBuildAction 추가
// UI에서 "Aborted by admin-a" 표시
```

### 11.4 왜 Computer는 Node의 참조 대신 nodeName을 저장하는가?

```java
// Computer.java (805-813행)
protected void setNode(Node node) {
    if (node instanceof Slave)
        this.nodeName = node.getNodeName();
    else
        this.nodeName = null;
    setNumExecutors(node.getNumExecutors());
}
```

Node 객체는 설정 변경 시 새로 생성되어 "버려진다(thrown away)." Computer가 Node에 대한 직접 참조를 유지하면:
- GC가 이전 Node 객체를 회수하지 못함 (메모리 누수)
- 이전 Node의 stale 설정을 참조할 위험
- 설정 변경 시 모든 Computer의 참조를 갱신해야 하는 복잡성

`nodeName`으로 간접 참조하면 `getNode()`가 항상 최신 Node를 조회한다.

---

## 12. 테이블 요약

### 12.1 핵심 클래스 비교

| | Node | Computer | Executor |
|---|------|----------|----------|
| **역할** | 설정 저장 | 런타임 상태 관리 | 빌드 실행 |
| **영속성** | XStream 직렬화 | transient | transient |
| **생명주기** | 설정 변경 시 재생성 | 빌드 완료까지 유지 | 빌드당 1회 |
| **상속** | `AbstractModelObject` | `Actionable` | `Thread` |
| **URL** | 없음 | `/computer/NAME/` | `/computer/NAME/executors/N` |
| **주요 상태** | 이름, 레이블, Executor 수 | 온/오프라인, Executor 목록 | 유휴/실행중/비동기 |
| **동시성** | 불변에 가까움 | CopyOnWriteArrayList | ReadWriteLock |

### 12.2 Executor 상태 전이

```
  [생성]
    |
    v
  PARKING (started=false, workUnit=null, executable=null)
    |
    | start(WorkUnit)
    v
  STARTED (started=true, workUnit!=null, executable=null)
    |
    | createExecutable()
    v
  EXECUTING (started=true, workUnit!=null, executable!=null)
    |
    +--- 정상 완료 ---------> finish1() -> finish2() -> [제거]
    |
    +--- AsynchronousExecution -> ASYNC (asynchronousExecution!=null)
    |                              |
    |                              | completed()
    |                              v
    |                        finish1() -> finish2() -> [제거]
    |
    +--- interrupt() --------> INTERRUPTED
                                |
                                v
                          finish1() -> finish2() -> [제거]
```

### 12.3 Computer 상태 전이

```
  [Node 추가]
       |
       v
  CREATED --> setNode() --> Executor 생성
       |
       v
  OFFLINE (channel == null)
       |
       | connect() -> launcher.launch()
       v
  CONNECTING (lastConnectActivity != null && !isDone())
       |
       | setChannel()
       v
  ONLINE (channel != null, !isTemporarilyOffline())
       |
       +-- setTemporaryOfflineCause(cause) --> TEMPORARILY_OFFLINE
       |                                        (채널 유지, 작업 거부)
       |
       +-- disconnect(cause) --> OFFLINE
       |
       +-- channel 에러 --> OFFLINE (ChannelTermination)
       |
       +-- kill() --> setNumExecutors(0) --> [모든 Executor 종료 후 제거]
```

---

## 13. 참고: 관련 소스 파일 목록

| 파일 | 줄 수 | 핵심 내용 |
|------|-------|----------|
| `core/src/main/java/hudson/model/Executor.java` | 992 | 빌드 실행 Thread, 인터럽트, 비동기 실행 |
| `core/src/main/java/hudson/model/Computer.java` | 1801 | Executor 관리, 오프라인 제어, UI |
| `core/src/main/java/hudson/model/Node.java` | 682 | 설정 전용, 레이블, canTake |
| `core/src/main/java/hudson/slaves/SlaveComputer.java` | ~800 | Channel 통신, 연결/해제 |
| `core/src/main/java/hudson/model/Slave.java` | ~500 | 원격 에이전트 설정 |
| `core/src/main/java/hudson/model/OneOffExecutor.java` | 40 | FlyweightTask 전용 Executor |
| `core/src/main/java/hudson/model/queue/WorkUnit.java` | 112 | Queue-Executor 간 작업 전달 |
| `core/src/main/java/jenkins/model/queue/AsynchronousExecution.java` | ~160 | Pipeline 비동기 실행 |
| `core/src/main/java/hudson/slaves/RetentionStrategy.java` | 302 | Always, Demand 전략 |
| `core/src/main/java/hudson/slaves/ComputerLauncher.java` | ~100 | SSH, JNLP 런처 추상화 |
| `core/src/main/java/hudson/slaves/OfflineCause.java` | ~100 | 오프라인 원인 계층 |
