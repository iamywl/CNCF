# 26. Trogdor 테스트 프레임워크 + Examples Deep-Dive

> Kafka 분산 테스트 프레임워크 Trogdor의 Coordinator-Agent 아키텍처와 공식 예제 코드 분석

---

## 1. 개요

Kafka 프로젝트는 분산 환경에서의 통합 테스트와 장애 주입을 위해 **Trogdor** 프레임워크를 자체 개발했다. Trogdor는 다음 두 가지 핵심 기능을 제공한다:

1. **워크로드 생성 (`trogdor/src/main/java/.../workload/`)**: ProduceBench, ConsumeBench, RoundTripWorkload 등의 벤치마크 태스크를 분산 에이전트에서 실행하여 처리량, 레이턴시 등을 측정한다.

2. **장애 주입 (`trogdor/src/main/java/.../fault/`)**: NetworkPartitionFault, ProcessStopFault, DegradedNetworkFault 등의 장애를 시뮬레이션하여 Kafka 클러스터의 복원력을 검증한다.

또한 `examples/src/` 디렉토리에는 Producer-Consumer, EOS(Exactly Once Semantics), Transactional 패턴의 공식 참조 구현이 있다.

```
┌────────────────────────────────────────────────────────────────┐
│                     Trogdor 아키텍처                            │
│                                                                │
│  ┌──────────────────┐         ┌──────────────────┐            │
│  │   Coordinator    │ ←REST─→ │   CLI / Client   │            │
│  │  (TaskManager)   │         └──────────────────┘            │
│  │  :8889           │                                          │
│  └────────┬─────────┘                                          │
│           │ REST API                                           │
│     ┌─────┴──────┬─────────────────┐                          │
│     ▼            ▼                 ▼                          │
│  ┌──────┐    ┌──────┐         ┌──────┐                       │
│  │Agent │    │Agent │   ...   │Agent │                       │
│  │:8888 │    │:8888 │         │:8888 │                       │
│  ├──────┤    ├──────┤         ├──────┤                       │
│  │Worker│    │Worker│         │Worker│                       │
│  │Mgr   │    │Mgr   │         │Mgr   │                       │
│  └──────┘    └──────┘         └──────┘                       │
│   node0       node1            node2                          │
│                                                                │
│  Kafka Cluster ──────────────────────────────────             │
│  ┌─────┐  ┌─────┐  ┌─────┐                                  │
│  │Br-0 │  │Br-1 │  │Br-2 │                                  │
│  └─────┘  └─────┘  └─────┘                                  │
└────────────────────────────────────────────────────────────────┘
```

---

## 2. Coordinator 아키텍처

### 2.1 Coordinator 클래스

```java
// trogdor/src/main/java/.../coordinator/Coordinator.java

public final class Coordinator {
    public static final int DEFAULT_PORT = 8889;

    private final long startTimeMs;
    private final TaskManager taskManager;
    private final JsonRestServer restServer;
    private final Time time;

    public Coordinator(Platform platform, Scheduler scheduler,
                       JsonRestServer restServer,
                       CoordinatorRestResource resource,
                       long firstWorkerId) {
        this.time = scheduler.time();
        this.startTimeMs = time.milliseconds();
        this.taskManager = new TaskManager(platform, scheduler, firstWorkerId);
        this.restServer = restServer;
        resource.setCoordinator(this);
    }
}
```

**설계 결정**: Coordinator는 `Platform` 인터페이스를 통해 토폴로지를 인식하고, `TaskManager`에 실제 태스크 관리를 위임한다. REST API는 `JsonRestServer` + JAX-RS Resource 패턴으로 구현된다.

### 2.2 TaskManager: 태스크 상태 머신

```java
// trogdor/src/main/java/.../coordinator/TaskManager.java

public final class TaskManager {
    private final Platform platform;
    private final Scheduler scheduler;
    private final Map<String, ManagedTask> tasks;
    private final ScheduledExecutorService executor;  // 단일 스레드
    private final Map<String, NodeManager> nodeManagers;
    private final Map<Long, WorkerState> workerStates;
    private long nextWorkerId;
}
```

**왜 단일 스레드 실행자인가?** TaskManager의 모든 상태 변경은 단일 스레드 `executor`를 통해 직렬화된다. 이 설계로 인해 **락이 필요 없으며**, 상태 머신의 전이가 원자적으로 보장된다.

### 2.3 태스크 상태 머신

```
┌─────────┐    시작 시간 도달    ┌─────────┐    완료/에러     ┌──────┐
│ PENDING │ ──────────────────→ │ RUNNING │ ──────────────→ │ DONE │
└─────────┘                     └─────────┘                 └──────┘
     │                               │                          ▲
     │ 취소 (stopTask)               │ 취소 (stopTask)          │
     └───────────────────────────────┤                          │
                                     ▼                          │
                                ┌──────────┐  워커 전부 완료    │
                                │ STOPPING │ ───────────────→ ──┘
                                └──────────┘
```

```java
// TaskManager.ManagedTask

class ManagedTask {
    private final String id;
    private final TaskSpec originalSpec;
    private final TaskSpec spec;
    private final TaskController controller;
    private TaskStateType state;  // PENDING, RUNNING, STOPPING, DONE
    private long startedMs = -1;
    private long doneMs = -1;
    boolean cancelled = false;
    private Future<?> startFuture = null;
    public TreeMap<String, Long> workerIds = new TreeMap<>();
    private String error = "";
}
```

### 2.4 태스크 생성 흐름

```java
// TaskManager.CreateTask (Callable, executor에서 실행)

class CreateTask implements Callable<Void> {
    @Override
    public Void call() throws Exception {
        // 1. 중복 검사: 동일 ID + 동일 Spec이면 무시
        ManagedTask task = tasks.get(id);
        if (task != null) {
            if (!task.originalSpec.equals(originalSpec)) {
                throw new RequestConflictException("different spec");
            }
            return null;  // 멱등성
        }

        // 2. TaskController 생성 (Spec에서)
        TaskController controller = spec.newController(id);

        // 3. 상태 PENDING으로 시작
        task = new ManagedTask(id, originalSpec, spec, controller, PENDING);
        tasks.put(id, task);

        // 4. 시작 시간에 맞춰 스케줄링
        long delayMs = task.startDelayMs(time.milliseconds());
        task.startFuture = scheduler.schedule(executor, new RunTask(task), delayMs);
        return null;
    }
}
```

### 2.5 태스크 실행 흐름

```java
// TaskManager.RunTask (Callable)

class RunTask implements Callable<Void> {
    @Override
    public Void call() throws Exception {
        task.clearStartFuture();
        if (task.state != PENDING) return null;

        // 대상 노드 결정
        TreeSet<String> nodeNames = task.findNodeNames();

        task.state = RUNNING;
        task.startedMs = time.milliseconds();

        // 각 노드의 Agent에 Worker 생성 요청
        for (String workerName : nodeNames) {
            long workerId = nextWorkerId++;
            task.workerIds.put(workerName, workerId);
            workerStates.put(workerId, new WorkerReceiving(task.id, task.spec));
            nodeManagers.get(workerName).createWorker(workerId, task.id, task.spec);
        }
        return null;
    }
}
```

| 단계 | 동작 | 관련 클래스 |
|------|------|-----------|
| 1. 생성 | REST API → CreateTask | CoordinatorRestResource |
| 2. 스케줄 | startMs까지 대기 | Scheduler |
| 3. 실행 | 대상 노드 결정 → Worker 생성 | TaskController, NodeManager |
| 4. 모니터링 | Worker 상태 업데이트 수신 | UpdateWorkerState |
| 5. 완료 | 모든 Worker 완료 → DONE 전이 | handleWorkerCompletion |

---

## 3. Agent 아키텍처

### 3.1 Agent 클래스

```java
// trogdor/src/main/java/.../agent/Agent.java

public final class Agent {
    public static final int DEFAULT_PORT = 8888;
    private final Platform platform;
    private final long serverStartMs;
    private final WorkerManager workerManager;
    private final JsonRestServer restServer;
}
```

### 3.2 WorkerManager: 워커 생명주기

```java
// trogdor/src/main/java/.../agent/WorkerManager.java

public final class WorkerManager {
    private final Map<Long, Worker> workers;
    private final ScheduledExecutorService stateChangeExecutor;  // 단일 스레드
    private final ExecutorService workerCleanupExecutor;          // 다중 스레드
    private final ShutdownManager shutdownManager;
}
```

**ShutdownManager 패턴**:

```java
// WorkerManager.ShutdownManager - 참조 카운팅 기반 안전 종료

static class ShutdownManager {
    private boolean shutdown = false;
    private long refCount = 0;

    class Reference implements AutoCloseable {
        @Override public void close() {
            synchronized (ShutdownManager.this) {
                refCount--;
                if (shutdown && refCount == 0) {
                    ShutdownManager.this.notifyAll();
                }
            }
        }
    }

    synchronized Reference takeReference() {
        if (shutdown) throw new KafkaException("shut down");
        refCount++;
        return new Reference();
    }

    synchronized void waitForQuiescence() throws InterruptedException {
        while (!shutdown || refCount > 0) wait();
    }
}
```

**왜 참조 카운팅인가?** RPC 처리 중 종료되면 데이터 손실이 발생할 수 있다. ShutdownManager는 모든 진행 중인 RPC와 Worker가 완료된 후에만 종료를 허용한다.

### 3.3 Worker 상태 머신

```
┌──────────┐   start() 성공    ┌─────────┐   endMs 도달     ┌──────────┐
│ STARTING │ ───────────────→  │ RUNNING │ ──────────────→  │ STOPPING │
└──────────┘                   └─────────┘                  └──────────┘
     │                              │                            │
     │ start() 실패                 │ stop() 호출                │ stop() 완료
     │                              │                            │
     ▼                              ▼                            ▼
┌──────────┐                  ┌──────────┐                  ┌──────┐
│CANCELLING│ ──────────────→  │ STOPPING │ ──────────────→  │ DONE │
└──────────┘                  └──────────┘                  └──────┘
```

```java
// Worker 상태 전이

void transitionToRunning() {
    state = State.RUNNING;
    // endMs에 자동 종료 타이머 설정
    timeoutFuture = scheduler.schedule(stateChangeExecutor,
        new StopWorker(workerId, false),
        Math.max(0, spec.endMs() - time.milliseconds()));
}

Future<Void> transitionToStopping() {
    state = State.STOPPING;
    timeoutFuture.cancel(false);  // 타이머 취소
    return workerCleanupExecutor.submit(new HaltWorker(this));
}

void transitionToDone() {
    state = State.DONE;
    doneMs = time.milliseconds();
    reference.close();  // ShutdownManager 참조 해제
    doneFuture.complete(error);
}
```

---

## 4. TaskSpec 다형성 시스템

### 4.1 TaskSpec 추상 클래스

```java
// trogdor/src/main/java/.../task/TaskSpec.java

@JsonTypeInfo(use = JsonTypeInfo.Id.CLASS, property = "class")
public abstract class TaskSpec {
    public static final long MAX_TASK_DURATION_MS = 1000000000000000L;

    private final long startMs;
    private final long durationMs;

    public abstract TaskController newController(String id);
    public abstract TaskWorker newTaskWorker(String id);
}
```

**핵심 패턴**: `@JsonTypeInfo(use = JsonTypeInfo.Id.CLASS)` 어노테이션을 통해 JSON의 `"class"` 필드에 따라 적절한 하위 클래스로 역직렬화된다. 이로 인해 새 태스크 유형을 추가할 때 코드 변경 없이 JSON 스펙만 작성하면 된다.

### 4.2 주요 TaskSpec 구현

| TaskSpec 클래스 | 용도 | 대상 노드 |
|----------------|------|---------|
| `ProduceBenchSpec` | Producer 처리량 벤치마크 | producerNode |
| `ConsumeBenchSpec` | Consumer 처리량 벤치마크 | consumerNode |
| `RoundTripWorkloadSpec` | 생산-소비 왕복 레이턴시 | clientNode |
| `ConnectionStressSpec` | 연결 스트레스 테스트 | clientNode |
| `SustainedConnectionSpec` | 장시간 연결 유지 | clientNode |
| `NetworkPartitionFaultSpec` | 네트워크 파티션 장애 | partitions 노드 |
| `ProcessStopFaultSpec` | 프로세스 강제 종료 | taskNodes |
| `DegradedNetworkFaultSpec` | 네트워크 품질 저하 | node |
| `ExternalCommandSpec` | 외부 명령 실행 | 지정 노드 |
| `NoOpTaskSpec` | 아무것도 하지 않음 (테스트용) | 모든 노드 |

### 4.3 ProduceBenchSpec 상세

```java
// trogdor/src/main/java/.../workload/ProduceBenchSpec.java

public final class ProduceBenchSpec extends TaskSpec {
    private final String producerNode;
    private final String bootstrapServers;
    private final int targetMessagesPerSec;
    private final long maxMessages;
    private final PayloadGenerator keyGenerator;
    private final PayloadGenerator valueGenerator;
    private final Optional<TransactionGenerator> transactionGenerator;
    private final Map<String, String> producerConf;
    private final TopicsSpec activeTopics;
    private final TopicsSpec inactiveTopics;

    @Override
    public TaskController newController(String id) {
        return topology -> Set.of(producerNode);  // 단일 노드
    }

    @Override
    public TaskWorker newTaskWorker(String id) {
        return new ProduceBenchWorker(id, this);
    }
}
```

**JSON 예시**:

```json
{
    "class": "org.apache.kafka.trogdor.workload.ProduceBenchSpec",
    "durationMs": 10000000,
    "producerNode": "node0",
    "bootstrapServers": "localhost:9092",
    "targetMessagesPerSec": 10,
    "maxMessages": 100,
    "activeTopics": {
        "foo[1-3]": {
            "numPartitions": 3,
            "replicationFactor": 1
        }
    }
}
```

### 4.4 NetworkPartitionFaultSpec

```java
// trogdor/src/main/java/.../fault/NetworkPartitionFaultSpec.java

public class NetworkPartitionFaultSpec extends TaskSpec {
    private final List<List<String>> partitions;

    @Override
    public TaskController newController(String id) {
        return new NetworkPartitionFaultController(partitionSets());
    }

    @Override
    public TaskWorker newTaskWorker(String id) {
        return new NetworkPartitionFaultWorker(id, partitionSets());
    }

    // 파티션 유효성 검사: 노드 중복 불가
    private List<Set<String>> partitionSets() {
        HashSet<String> prevNodes = new HashSet<>();
        for (List<String> partition : partitions) {
            for (String nodeName : partition) {
                if (prevNodes.contains(nodeName)) {
                    throw new RuntimeException("Node " + nodeName +
                        " appears in more than one partition.");
                }
                prevNodes.add(nodeName);
            }
        }
        // ...
    }
}
```

---

## 5. Throttle (처리량 제어)

### 5.1 슬라이딩 윈도우 기반 스로틀

```java
// trogdor/src/main/java/.../workload/Throttle.java

public class Throttle {
    private final int maxPerPeriod;  // 주기당 최대 수
    private final int periodMs;       // 주기 (밀리초)
    private int count;
    private long prevPeriod;

    public synchronized boolean increment() throws InterruptedException {
        boolean throttled = false;
        while (true) {
            if (count < maxPerPeriod) {
                count++;
                return throttled;
            }
            long curPeriod = time().milliseconds() / periodMs;
            if (curPeriod <= prevPeriod) {
                // 같은 주기: 다음 주기까지 대기
                long nextPeriodMs = (curPeriod + 1) * periodMs;
                delay(nextPeriodMs - time().milliseconds());
                throttled = true;
            } else {
                // 새 주기: 카운터 리셋
                prevPeriod = curPeriod;
                count = 0;
            }
        }
    }
}
```

**왜 이 설계인가?** Token Bucket 대신 시간 주기 기반 접근을 사용하면 구현이 단순하면서도 정확한 처리량 제어가 가능하다. `Object.wait()`을 사용하여 불필요한 CPU 사용을 방지한다.

### 5.2 ThroughputGenerator 계층 구조

| 클래스 | 동작 |
|--------|------|
| `ConstantThroughputGenerator` | 고정 처리량 유지 |
| `GaussianThroughputGenerator` | 정규 분포 기반 가변 처리량 |

---

## 6. PayloadGenerator 시스템

### 6.1 다형성 페이로드 생성

```
PayloadGenerator (abstract)
├── ConstantPayloadGenerator        // 고정 페이로드
├── SequentialPayloadGenerator      // 순차 증가 페이로드
├── UniformRandomPayloadGenerator   // 균일 분포 랜덤
├── NullPayloadGenerator            // null 페이로드
├── TimestampConstantPayloadGenerator
├── TimestampRandomPayloadGenerator
├── GaussianTimestampConstantPayloadGenerator
├── GaussianTimestampRandomPayloadGenerator
└── RandomComponentPayloadGenerator // 복합 랜덤 컴포넌트
```

```java
// SequentialPayloadGenerator: 4바이트 순차 증가

public class SequentialPayloadGenerator implements PayloadGenerator {
    private final int size;
    private final long startOffset;

    @Override
    public byte[] generate(long position) {
        byte[] result = new byte[size];
        long value = startOffset + position;
        // big-endian 인코딩
        for (int i = 0; i < Math.min(size, 8); i++) {
            result[size - 1 - i] = (byte) (value & 0xFF);
            value >>= 8;
        }
        return result;
    }
}
```

---

## 7. NodeManager: Coordinator-Agent 통신

### 7.1 하트비트 기반 상태 동기화

```java
// trogdor/src/main/java/.../coordinator/NodeManager.java

// NodeManager는 개별 Agent와의 통신을 관리한다.
// 주기적으로 Agent의 상태를 폴링하여 TaskManager에 보고한다.

// Coordinator → Agent 통신:
//   POST /agent/worker/create  (CreateWorkerRequest)
//   PUT  /agent/worker/stop    (StopWorkerRequest)
//   PUT  /agent/worker/destroy (DestroyWorkerRequest)
//   GET  /agent/status         (AgentStatusResponse)
```

| REST 엔드포인트 | 메서드 | 용도 |
|----------------|--------|------|
| `/coordinator/task` | POST | 태스크 생성 |
| `/coordinator/task` | DELETE | 태스크 삭제 |
| `/coordinator/tasks` | GET | 태스크 목록 조회 |
| `/coordinator/task/{id}` | GET | 태스크 상세 조회 |
| `/coordinator/status` | GET | Coordinator 상태 |
| `/agent/worker/create` | POST | Worker 생성 |
| `/agent/worker/stop` | PUT | Worker 중지 |
| `/agent/status` | GET | Agent 상태 |

---

## 8. Kafka Examples 디렉토리

### 8.1 디렉토리 구조

```
examples/src/main/java/kafka/examples/
├── KafkaConsumerProducerDemo.java   // 기본 Producer-Consumer 데모
├── KafkaExactlyOnceDemo.java        // EOS (Exactly Once Semantics)
├── TransactionalClientDemo.java     // 트랜잭션 클라이언트
├── ExactlyOnceMessageProcessor.java // EOS 메시지 프로세서
├── Producer.java                    // Producer 구현
├── Consumer.java                    // Consumer 구현
├── KafkaProperties.java             // 공통 설정
└── Utils.java                       // 유틸리티
```

### 8.2 KafkaConsumerProducerDemo

```java
// examples/src/main/java/kafka/examples/KafkaConsumerProducerDemo.java

public class KafkaConsumerProducerDemo {
    public static final String TOPIC_NAME = "my-topic";
    public static final String GROUP_NAME = "my-group";

    public static void main(String[] args) {
        int numRecords = Integer.parseInt(args[0]);
        boolean isAsync = args.length == 1 || !args[1].equalsIgnoreCase("sync");

        // 1단계: 토픽 초기화
        Utils.recreateTopics(BOOTSTRAP_SERVERS, -1, TOPIC_NAME);
        CountDownLatch latch = new CountDownLatch(2);

        // 2단계: Producer 스레드 시작
        Producer producerThread = new Producer("producer", BOOTSTRAP_SERVERS,
            TOPIC_NAME, isAsync, null, false, numRecords, -1, latch);
        producerThread.start();

        // 3단계: Consumer 스레드 시작
        Consumer consumerThread = new Consumer("consumer", BOOTSTRAP_SERVERS,
            TOPIC_NAME, GROUP_NAME, Optional.empty(), false, numRecords, latch);
        consumerThread.start();

        latch.await(5, TimeUnit.MINUTES);
    }
}
```

**패턴**: `CountDownLatch`를 사용하여 Producer와 Consumer가 모두 완료될 때까지 대기. 5분 타임아웃으로 행(hang) 방지.

### 8.3 Exactly Once Semantics (EOS) 패턴

```java
// examples/src/main/java/kafka/examples/ExactlyOnceMessageProcessor.java

// 핵심 알고리즘:
// 1. Consumer에서 메시지 읽기
// 2. Producer 트랜잭션 시작
// 3. 처리 결과 출력 토픽에 쓰기
// 4. Consumer 오프셋을 트랜잭션에 포함
// 5. 트랜잭션 커밋 (원자적)

// producer.initTransactions();
// while (true) {
//     records = consumer.poll(Duration.ofMillis(200));
//     producer.beginTransaction();
//     for (record : records) {
//         producer.send(new ProducerRecord(outputTopic, ...));
//     }
//     producer.sendOffsetsToTransaction(offsets, groupMeta);
//     producer.commitTransaction();
// }
```

---

## 9. Platform과 Topology

### 9.1 Platform 설정

```java
// trogdor/src/main/java/.../common/Platform.java

public interface Platform {
    String name();
    Node curNode();
    Topology topology();
    Scheduler scheduler();
    String runCommand(String[] command) throws IOException;
}
```

### 9.2 설정 파일 포맷

```json
{
    "platform": "org.apache.kafka.trogdor.basic.BasicPlatform",
    "nodes": {
        "node0": {
            "hostname": "host0.example.com",
            "trogdor.agent.port": 8888,
            "trogdor.coordinator.port": 8889
        },
        "node1": {
            "hostname": "host1.example.com",
            "trogdor.agent.port": 8888
        },
        "node2": {
            "hostname": "host2.example.com",
            "trogdor.agent.port": 8888
        }
    }
}
```

| 필드 | 설명 |
|------|------|
| `platform` | Platform 구현체 클래스명 |
| `nodes.*.hostname` | 노드 호스트명 |
| `nodes.*.trogdor.agent.port` | Agent 포트 (기본 8888) |
| `nodes.*.trogdor.coordinator.port` | Coordinator 포트 (기본 8889) |

---

## 10. Histogram: 레이턴시 측정

```java
// trogdor/src/main/java/.../workload/Histogram.java

// P50, P95, P99 등의 백분위수를 계산한다.
// 벤치마크 Worker들이 레이턴시 분포를 추적하는 데 사용한다.
```

| 지표 | 설명 |
|------|------|
| p50 | 중앙값 레이턴시 |
| p95 | 95번째 백분위수 |
| p99 | 99번째 백분위수 |
| average | 평균 레이턴시 |
| count | 총 샘플 수 |

---

## 11. 장애 주입 메커니즘

### 11.1 NetworkPartitionFault

```
정상 상태:
  node0 ←→ node1 ←→ node2

파티션 후:
  [node0, node1] ←→ X ←→ [node2]
  partition 1          partition 2
```

iptables 규칙을 사용하여 실제 네트워크 파티션을 구현:

```bash
# Worker가 실행하는 명령 (개념)
iptables -A INPUT -s <other_partition_ips> -j DROP
iptables -A OUTPUT -d <other_partition_ips> -j DROP
```

### 11.2 ProcessStopFault

```java
// 특정 프로세스를 SIGSTOP/SIGCONT로 제어
// kill -STOP <pid>  // 프로세스 일시 중지
// kill -CONT <pid>  // 프로세스 재개
```

### 11.3 DegradedNetworkFault

```bash
# tc (traffic control)로 네트워크 품질 저하
tc qdisc add dev eth0 root netem delay 100ms loss 1%
```

---

## 12. Exec 모드

Agent는 `--exec` 플래그로 단일 태스크를 직접 실행하고 종료할 수 있다:

```java
// Agent.exec()
boolean exec(TaskSpec spec, PrintStream out) throws Exception {
    TaskController controller = spec.newController(EXEC_TASK_ID);
    Set<String> nodes = controller.targetNodes(platform.topology());
    if (!nodes.contains(platform.curNode().name())) {
        out.println("This task is not configured to run on this node.");
        return false;
    }
    KafkaFuture<String> future = workerManager.createWorker(
        EXEC_WORKER_ID, EXEC_TASK_ID, spec);
    String error = future.get();
    return error == null || error.isEmpty();
}
```

사용 예시:
```bash
bin/trogdor-agent.sh --agent.config config.json \
  --node-name node0 \
  --exec '{"class":"org.apache.kafka.trogdor.workload.ProduceBenchSpec",...}'
```

---

## 13. StringExpander: 토픽 이름 확장

```java
// trogdor/src/main/java/.../common/StringExpander.java

// "foo[1-3]" → ["foo1", "foo2", "foo3"]
// "bar[a-c]" → ["bara", "barb", "barc"]
// TopicsSpec에서 사용하여 다수의 토픽을 한 줄로 지정
```

| 패턴 | 확장 결과 |
|------|---------|
| `test-topic[0-2]` | `test-topic0`, `test-topic1`, `test-topic2` |
| `perf-[a-c]` | `perf-a`, `perf-b`, `perf-c` |
| `topic[1-3]-part[0-1]` | 6개 조합 |

---

## 14. 운영 가이드

### 14.1 Coordinator 시작

```bash
bin/trogdor-coordinator.sh \
  --coordinator.config /path/to/trogdor.conf \
  --node-name node0
```

### 14.2 Agent 시작

```bash
bin/trogdor-agent.sh \
  --agent.config /path/to/trogdor.conf \
  --node-name node1
```

### 14.3 태스크 제출 (CoordinatorClient CLI)

```bash
# 태스크 생성
curl -X POST http://coordinator:8889/coordinator/task \
  -H "Content-Type: application/json" \
  -d '{
    "id": "produce-bench-1",
    "spec": {
      "class": "org.apache.kafka.trogdor.workload.ProduceBenchSpec",
      "startMs": 0,
      "durationMs": 60000,
      "producerNode": "node1",
      "bootstrapServers": "broker:9092",
      "targetMessagesPerSec": 1000,
      "activeTopics": {
        "test-topic[0-2]": {
          "numPartitions": 6,
          "replicationFactor": 3
        }
      }
    }
  }'

# 태스크 상태 조회
curl http://coordinator:8889/coordinator/tasks

# 태스크 중지
curl -X PUT http://coordinator:8889/coordinator/task/stop \
  -d '{"id": "produce-bench-1"}'
```

---

## 15. 설계 결정 분석

### 15.1 왜 자체 테스트 프레임워크인가?

| 기준 | JUnit/TestContainers | Trogdor |
|------|---------------------|---------|
| 분산 실행 | 단일 JVM | 다중 노드 |
| 네트워크 장애 | 시뮬레이션 한계 | 실제 iptables |
| 장기 실행 | 테스트 타임아웃 | 무제한 duration |
| 결과 수집 | 개별 노드 | 중앙 수집 |
| 재사용성 | 코드 기반 | JSON 스펙 기반 |

### 15.2 왜 단일 스레드 실행자인가?

TaskManager와 WorkerManager 모두 `ScheduledExecutorService.newSingleThreadScheduledExecutor()`를 사용한다:
- **락 불필요**: 모든 상태 변경이 단일 스레드에서 실행
- **순서 보장**: 이벤트 처리 순서가 보장됨
- **디버깅 용이**: 상태 전이가 예측 가능

### 15.3 왜 JSON 기반 TaskSpec인가?

`@JsonTypeInfo`를 통한 다형성 역직렬화로:
- 새 태스크 유형 추가 시 **코드 변경 최소화** (클래스 추가만)
- CLI에서 직접 JSON 스펙 전달 가능 (`--exec` 모드)
- 태스크 스펙의 **버전 관리** 및 **재현성** 보장

---

## 참조 소스 파일

| 파일 | 역할 |
|------|------|
| `trogdor/src/main/java/.../coordinator/Coordinator.java` | Coordinator 메인 |
| `trogdor/src/main/java/.../coordinator/TaskManager.java` | 태스크 상태 머신 |
| `trogdor/src/main/java/.../coordinator/NodeManager.java` | Agent 통신 관리 |
| `trogdor/src/main/java/.../agent/Agent.java` | Agent 메인 |
| `trogdor/src/main/java/.../agent/WorkerManager.java` | Worker 생명주기 |
| `trogdor/src/main/java/.../task/TaskSpec.java` | 태스크 스펙 추상 클래스 |
| `trogdor/src/main/java/.../workload/ProduceBenchSpec.java` | Producer 벤치마크 |
| `trogdor/src/main/java/.../fault/NetworkPartitionFaultSpec.java` | 네트워크 파티션 |
| `trogdor/src/main/java/.../workload/Throttle.java` | 처리량 제어 |
| `examples/src/main/java/kafka/examples/KafkaConsumerProducerDemo.java` | 기본 데모 |
| `examples/src/main/java/kafka/examples/ExactlyOnceMessageProcessor.java` | EOS 데모 |
