# 14. Kafka Streams Deep-Dive

## 목차

1. [개요](#1-개요)
2. [Topology: DAG 기반 처리 그래프](#2-topology-dag-기반-처리-그래프)
3. [StreamsBuilder vs Topology (DSL vs Processor API)](#3-streamsbuilder-vs-topology-dsl-vs-processor-api)
4. [KafkaStreams: 메인 진입점](#4-kafkastreams-메인-진입점)
5. [StreamThread: 처리 스레드](#5-streamthread-처리-스레드)
6. [Task: 파티션별 처리 단위](#6-task-파티션별-처리-단위)
7. [StateStore: 상태 저장소](#7-statestore-상태-저장소)
8. [Changelog 토픽](#8-changelog-토픽)
9. [윈도우 연산](#9-윈도우-연산)
10. [DSL 연산자](#10-dsl-연산자)
11. [KTable vs KStream: 이중성](#11-ktable-vs-kstream-이중성)
12. [Exactly-Once: processing.guarantee](#12-exactly-once-processingguarantee)

---

## 1. 개요

**Kafka Streams**는 Kafka 위에서 동작하는 **스트림 처리 라이브러리**이다. 별도의 클러스터
없이 일반 Java 애플리케이션에 내장되어 실행된다. 입력 토픽에서 데이터를 읽고, 처리하고,
출력 토픽에 쓰는 파이프라인을 선언적으로 정의할 수 있다.

### 핵심 특징

```
Kafka Streams의 차별점:

1. 라이브러리 (프레임워크가 아님):
   - 별도 클러스터 불필요 (Flink, Spark와 다름)
   - jar 의존성만 추가하면 됨
   - 기존 애플리케이션에 내장 가능

2. Kafka 네이티브:
   - 입력/출력이 Kafka 토픽
   - Consumer Group으로 파티셔닝/병렬화
   - Kafka 복제로 상태 내구성 보장

3. 정확히 한 번(Exactly-Once):
   - Kafka 트랜잭션 기반 EOS
   - read-process-write 원자성 보장
```

### 소스 위치

| 컴포넌트 | 소스 경로 |
|----------|----------|
| KafkaStreams | `streams/src/main/java/org/apache/kafka/streams/KafkaStreams.java` |
| StreamsBuilder | `streams/src/main/java/org/apache/kafka/streams/StreamsBuilder.java` |
| Topology | `streams/src/main/java/org/apache/kafka/streams/Topology.java` |
| StreamThread | `streams/src/main/java/org/apache/kafka/streams/processor/internals/StreamThread.java` |
| StreamTask | `streams/src/main/java/org/apache/kafka/streams/processor/internals/StreamTask.java` |
| TaskManager | `streams/src/main/java/org/apache/kafka/streams/processor/internals/TaskManager.java` |
| InternalTopologyBuilder | `streams/src/main/java/org/apache/kafka/streams/processor/internals/InternalTopologyBuilder.java` |
| KeyValueStore | `streams/src/main/java/org/apache/kafka/streams/state/KeyValueStore.java` |
| RocksDBStore | `streams/src/main/java/org/apache/kafka/streams/state/internals/` |
| StreamsPartitionAssignor | `streams/src/main/java/org/apache/kafka/streams/processor/internals/StreamsPartitionAssignor.java` |

---

## 2. Topology: DAG 기반 처리 그래프

### 2.1 Topology 개념

Topology는 **방향성 비순환 그래프(DAG)**로 데이터 처리 파이프라인을 표현한다.

```
Topology DAG 예시:

입력 토픽                 프로세서 노드               출력 토픽
+---------+           +-------------+           +---------+
| orders  |--Source-->| FilterNode  |--Sink---->| valid-  |
|         |           | (금액>0)    |           | orders  |
+---------+           +------+------+           +---------+
                             |
                     +-------+-------+
                     | EnrichNode    |
                     | (고객정보 조인)|
                     +-------+-------+
                             |
                        +----+----+
                        | SinkNode|----------->+---------+
                        +---------+            | enriched|
                                               | orders  |
                                               +---------+

노드 유형:
  SourceNode: Kafka 토픽에서 레코드 읽기
  Processor:  비즈니스 로직 처리
  SinkNode:   Kafka 토픽에 레코드 쓰기
```

### 2.2 Topology 클래스

```java
// Topology.java (라인 60-74)
public class Topology {
    protected final InternalTopologyBuilder internalTopologyBuilder;

    public Topology() {
        this(new InternalTopologyBuilder());
    }

    // 소스 노드 추가
    public Topology addSource(String name, String... topics) { ... }

    // 프로세서 노드 추가
    public Topology addProcessor(String name,
        ProcessorSupplier<?, ?, ?, ?> supplier,
        String... parentNames) { ... }

    // 싱크 노드 추가
    public Topology addSink(String name, String topic,
        String... parentNames) { ... }

    // 상태 저장소 추가
    public Topology addStateStore(StoreBuilder<?> storeBuilder,
        String... processorNames) { ... }
}
```

### 2.3 InternalTopologyBuilder

```
InternalTopologyBuilder 내부 구조:

+----------------------------------------------------------+
| InternalTopologyBuilder                                   |
|                                                           |
|  nodeFactories: Map<String, NodeFactory>                  |
|  +------------------------------------------------------+|
|  | "SOURCE-orders" -> SourceNodeFactory(topics=["orders"])||
|  | "FILTER"        -> ProcessorNodeFactory(...)          ||
|  | "SINK-output"   -> SinkNodeFactory(topic="output")    ||
|  +------------------------------------------------------+|
|                                                           |
|  nodeGrouper: NodeGrouper                                 |
|  |-- 노드들을 서브토폴로지(SubTopology)로 그룹화         |
|  |-- 연결된 노드들은 같은 서브토폴로지                    |
|                                                           |
|  stateStoreNameToSourceTopics:                            |
|    Map<String, List<String>>                              |
|  |-- 상태 저장소와 소스 토픽 매핑                         |
|                                                           |
|  topicToPatterns: Map<String, Pattern>                    |
|  |-- 정규식 기반 토픽 구독                                |
+----------------------------------------------------------+
```

---

## 3. StreamsBuilder vs Topology (DSL vs Processor API)

### 3.1 두 가지 API

```
DSL (High-Level API) - StreamsBuilder:
+--------------------------------------------------+
| StreamsBuilder builder = new StreamsBuilder();    |
|                                                   |
| KStream<String, String> stream =                  |
|     builder.stream("input-topic");                |
|                                                   |
| stream.filter((k, v) -> v.length() > 5)          |
|       .mapValues(v -> v.toUpperCase())            |
|       .to("output-topic");                        |
|                                                   |
| Topology topology = builder.build();              |
+--------------------------------------------------+
=> 선언적, 함수형 스타일
=> filter, map, join, aggregate 등 내장 연산자

Processor API (Low-Level API) - Topology:
+--------------------------------------------------+
| Topology topology = new Topology();               |
|                                                   |
| topology.addSource("source", "input-topic")       |
|   .addProcessor("process",                        |
|     () -> new MyProcessor(), "source")            |
|   .addSink("sink", "output-topic", "process");    |
+--------------------------------------------------+
=> 명시적, 세밀한 제어
=> 커스텀 Processor 구현 필요
```

### 3.2 StreamsBuilder 내부

```java
// StreamsBuilder.java (라인 65-79)
public class StreamsBuilder {
    protected final Topology topology;
    protected final InternalTopologyBuilder internalTopologyBuilder;
    protected final InternalStreamsBuilder internalStreamsBuilder;

    public StreamsBuilder() {
        topology = new Topology();
        internalTopologyBuilder = topology.internalTopologyBuilder;
        internalStreamsBuilder =
            new InternalStreamsBuilder(internalTopologyBuilder, false);
    }
}
```

### 3.3 DSL이 Processor API로 변환되는 과정

```
DSL -> Processor API 변환:

builder.stream("orders")        -> SourceNode("KSTREAM-SOURCE-0")
  .filter((k, v) -> ...)        -> ProcessorNode("KSTREAM-FILTER-1")
  .mapValues(v -> ...)           -> ProcessorNode("KSTREAM-MAPVALUES-2")
  .to("output")                 -> SinkNode("KSTREAM-SINK-3")

내부 토폴로지:
  KSTREAM-SOURCE-0 --> KSTREAM-FILTER-1 --> KSTREAM-MAPVALUES-2
                                                     |
                                              KSTREAM-SINK-3

각 DSL 연산자는 내부적으로 하나 이상의 ProcessorNode를 생성한다.
```

**왜 두 가지 API인가?** DSL은 대부분의 스트림 처리 요구사항을 간결하게 표현하지만,
복잡한 상태 관리나 외부 시스템 연동이 필요한 경우 Processor API가 더 유연하다.
DSL은 내부적으로 Processor API 위에 구축되어 있으므로, 필요시 혼합 사용이 가능하다.

---

## 4. KafkaStreams: 메인 진입점

### 4.1 KafkaStreams 상태 머신

`KafkaStreams.java` (라인 254-264):

```java
public enum State {
    CREATED(1, 3),          // 0: 생성됨
    REBALANCING(2, 3, 5),   // 1: 리밸런싱 중
    RUNNING(1, 2, 3, 5),    // 2: 실행 중
    PENDING_SHUTDOWN(4),    // 3: 셧다운 대기
    NOT_RUNNING,            // 4: 정지됨 (정상)
    PENDING_ERROR(6),       // 5: 에러 처리 대기
    ERROR;                  // 6: 에러 (비정상)
}
```

```
KafkaStreams 상태 머신:

            start()
  CREATED ---------> REBALANCING <-----+
     |                   |     |       |
     |           할당 완료|     |  리밸런스
     |                   v     |  트리거
     |               RUNNING --+
     |                   |
     |    close()   close()  에러
     |       |          |      |
     +-------+----------+  PENDING_ERROR
             |                 |
             v                 v
       PENDING_SHUTDOWN     ERROR
             |
             v
        NOT_RUNNING
```

### 4.2 KafkaStreams 초기화

```
KafkaStreams 생성 시 주요 컴포넌트:

KafkaStreams(Topology topology, Properties config)
    |
    |-- 1. StreamsConfig 파싱
    |   |-- application.id (필수)
    |   |-- bootstrap.servers (필수)
    |   |-- num.stream.threads (기본 1)
    |
    |-- 2. StreamThread 생성 (num.stream.threads만큼)
    |   |-- 각 스레드가 독립적인 KafkaConsumer 보유
    |   |-- 각 스레드가 독립적인 KafkaProducer 보유
    |
    |-- 3. GlobalStreamThread 생성 (GlobalKTable 사용 시)
    |   |-- 모든 파티션 구독
    |   |-- 글로벌 상태 저장소 유지
    |
    |-- 4. StateDirectory 초기화
    |   |-- /tmp/kafka-streams/{application.id}/{task.id}/
    |   |-- RocksDB 등 상태 저장소 파일 위치
    |
    |-- 5. Metrics 설정
```

### 4.3 start() 메서드

```
KafkaStreams.start():
    |
    |-- 상태: CREATED -> REBALANCING
    |
    |-- 각 StreamThread.start()
    |   |-- Consumer.subscribe(토픽들)
    |   |-- StreamsPartitionAssignor 등록
    |   |-- 메인 처리 루프 시작
    |
    |-- GlobalStreamThread.start() (있는 경우)
    |   |-- 글로벌 상태 복원
    |   |-- 글로벌 토픽 구독
    |
    |-- 리밸런스 -> Task 할당 -> RUNNING
```

---

## 5. StreamThread: 처리 스레드

### 5.1 StreamThread 상태 머신

`StreamThread.java` (라인 181-206):

```java
public enum State implements ThreadStateTransitionValidator {
    CREATED(1, 5),                    // 0
    STARTING(2, 3, 5),                // 1
    PARTITIONS_REVOKED(2, 3, 5),      // 2
    PARTITIONS_ASSIGNED(2, 3, 4, 5),  // 3
    RUNNING(2, 3, 4, 5),              // 4
    PENDING_SHUTDOWN(6),              // 5
    DEAD;                             // 6
}
```

```
StreamThread 상태 머신:

  CREATED --> STARTING --> PARTITIONS_ASSIGNED --> RUNNING
                 ^               |    ^              |
                 |               |    |              |
                 |   리밸런스    v    |   리밸런스   |
                 |   트리거    PARTITIONS_REVOKED    |
                 |               |                   |
                 +---------------+                   |
                                                     v
                                              PENDING_SHUTDOWN
                                                     |
                                                     v
                                                   DEAD
```

### 5.2 메인 처리 루프

```
StreamThread.runLoop() 핵심:

while (isRunning()) {
    // 1. Consumer로 레코드 가져오기
    ConsumerRecords<byte[], byte[]> records = consumer.poll(pollTime);

    // 2. 레코드를 Task별로 분배
    for (ConsumerRecord record : records) {
        TaskId taskId = taskForPartition(record.partition());
        tasks.get(taskId).addRecords(record.partition(), record);
    }

    // 3. 각 Task 처리
    for (StreamTask task : activeTasks) {
        // 레코드 처리 (process)
        if (task.process(wallClockTime)) {
            processedCount++;
        }
    }

    // 4. Punctuation 실행
    for (StreamTask task : activeTasks) {
        task.maybePunctuateStreamTime();
        task.maybePunctuateWallClockTime();
    }

    // 5. 커밋 (주기적)
    if (shouldCommit) {
        commitAll();
    }
}
```

```
StreamThread 처리 루프 상세:

+------------------------------------------------------+
|                  poll(pollTime)                        |
|  Consumer에서 레코드 가져오기                          |
|  pollTime = 나머지 처리에 걸린 시간에 따라 동적 조정  |
+----------------------------+-------------------------+
                             |
                             v
+------------------------------------------------------+
|              addRecordsToTasks()                       |
|  ConsumerRecord -> PartitionGroup -> Task에 분배      |
|  각 파티션은 정확히 하나의 Task에 매핑                 |
+----------------------------+-------------------------+
                             |
                             v
+------------------------------------------------------+
|              process() [반복]                          |
|  각 Task에서 max.task.idle.ms까지 레코드 처리         |
|  SourceNode -> ProcessorNode -> ... -> SinkNode       |
|  StateStore 읽기/쓰기 포함                            |
+----------------------------+-------------------------+
                             |
                             v
+------------------------------------------------------+
|              punctuate()                              |
|  STREAM_TIME: 이벤트 시간 기반 주기적 호출            |
|  WALL_CLOCK_TIME: 벽시계 시간 기반 주기적 호출        |
+----------------------------+-------------------------+
                             |
                             v
+------------------------------------------------------+
|              commit()                                 |
|  오프셋 커밋 + StateStore flush + Producer flush      |
|  commit.interval.ms (기본 30초) 주기                  |
+------------------------------------------------------+
```

### 5.3 poll-process-commit 루프

**왜 poll과 process를 분리하는가?** Consumer.poll()은 네트워크 I/O를 포함하므로
블로킹된다. poll에서 가져온 레코드를 즉시 처리하지 않고 Task의 버퍼에 넣으면,
여러 파티션의 레코드를 이벤트 시간 순서로 정렬하여 처리할 수 있다
(PartitionGroup의 타임스탬프 기반 우선순위 큐).

---

## 6. Task: 파티션별 처리 단위

### 6.1 Task 개념

```
Task와 파티션 매핑:

서브토폴로지 0: Source("orders") -> Filter -> Sink("valid-orders")
서브토폴로지 1: Source("payments") -> Join -> Sink("receipts")

토픽 "orders": 3 파티션, "payments": 3 파티션

Task 생성:
  서브토폴로지 0:
    Task 0_0: orders-P0 -> Filter -> valid-orders-P0
    Task 0_1: orders-P1 -> Filter -> valid-orders-P1
    Task 0_2: orders-P2 -> Filter -> valid-orders-P2

  서브토폴로지 1:
    Task 1_0: payments-P0 -> Join -> receipts-P0
    Task 1_1: payments-P1 -> Join -> receipts-P1
    Task 1_2: payments-P2 -> Join -> receipts-P2

총 6개 Task, 2개 StreamThread에 분배:
  Thread-0: [Task 0_0, Task 0_1, Task 1_0]
  Thread-1: [Task 0_2, Task 1_1, Task 1_2]
```

### 6.2 Task 유형

```
Task 유형:

1. StreamTask (Active Task):
   +----------------------------------------+
   | 입력 파티션에서 읽기                     |
   | -> 프로세서 토폴로지로 처리              |
   | -> 출력 토픽에 쓰기                      |
   | -> 로컬 StateStore 관리                  |
   | -> Changelog 토픽에 변경 기록            |
   +----------------------------------------+

2. StandbyTask:
   +----------------------------------------+
   | Changelog 토픽에서 읽기                  |
   | -> 로컬 StateStore에 복제              |
   | -> 장애 복구 시 빠른 상태 복원           |
   | -> 데이터 처리 없음, 상태만 유지         |
   +----------------------------------------+
```

### 6.3 Task 상태 전이

```
Task 라이프사이클:

  CREATED           Task 생성됨
     |
     | initializeIfNeeded()
     v
  RESTORING          상태 저장소 복원 중
     |               (Changelog 토픽에서 재생)
     | stateRestored()
     v
  RUNNING            정상 처리 중
     |               (poll -> process -> commit)
     | suspend()
     v
  SUSPENDED          일시 중지 (리밸런스 중)
     |
     +-- resume() --> RUNNING  (같은 Task 재할당)
     |
     +-- closeDirty() --> CLOSED  (다른 인스턴스로 이동)
     |
     +-- closeClean() --> CLOSED  (정상 종료)
```

### 6.4 Task 처리 상세

```
StreamTask.process() 내부:

1. PartitionGroup에서 다음 레코드 선택
   (가장 작은 타임스탬프의 레코드)

2. SourceNode.process(record)
   |-- 타임스탬프 추출
   |-- 다음 노드로 전달

3. ProcessorNode.process(record)
   |-- 사용자 로직 실행
   |-- context.forward(key, value) -> 다음 노드
   |-- StateStore 읽기/쓰기

4. SinkNode.process(record)
   |-- 출력 토픽으로 전송
   |-- Producer.send()

5. 오프셋 업데이트
   |-- 처리 완료된 레코드의 오프셋 기록
```

---

## 7. StateStore: 상태 저장소

### 7.1 StateStore 인터페이스

```java
// KeyValueStore.java - 핵심 인터페이스
public interface KeyValueStore<K, V> extends StateStore {
    void put(K key, V value);
    V get(K key);
    V delete(K key);
    KeyValueIterator<K, V> range(K from, K to);
    KeyValueIterator<K, V> all();
    long approximateNumEntries();
}
```

### 7.2 StateStore 계층

```
StateStore 구현 계층:

KeyValueStore (인터페이스)
    |
    +-- InMemoryKeyValueStore
    |   |-- ConcurrentNavigableMap 기반
    |   |-- 빠르지만 메모리 제한
    |
    +-- RocksDBStore
    |   |-- Facebook RocksDB 기반 (JNI)
    |   |-- 디스크 기반, 대용량 지원
    |   |-- LSM-Tree 구조
    |
    +-- ChangeloggingKeyValueBytesStore (래퍼)
    |   |-- 쓰기 시 Changelog 토픽에 기록
    |   |-- 실제 저장은 위임 (RocksDB 등)
    |
    +-- CachingKeyValueStore (래퍼)
    |   |-- 인메모리 캐시 레이어
    |   |-- 중복 키 병합으로 Changelog 트래픽 감소
    |
    +-- MeteredKeyValueStore (래퍼)
        |-- 메트릭 수집
        |-- 지연 시간, 처리량 측정
```

### 7.3 RocksDB 통합

```
RocksDB StateStore 구조:

디렉토리 구조:
/tmp/kafka-streams/{app.id}/{task.id}/
    rocksdb/
        {store-name}/
            000001.sst     (SSTable 파일)
            000002.sst
            CURRENT
            MANIFEST-000003
            OPTIONS-000004
            LOG

RocksDB 설정 (기본):
+------------------------------------------+
| block.cache.size: 50MB                   |
| write.buffer.size: 16MB                  |
| max.write.buffer.number: 3              |
| compaction.style: LEVEL                  |
+------------------------------------------+
```

**왜 RocksDB인가?**
1. **디스크 기반**: 메모리보다 큰 상태를 처리 가능
2. **임베디드**: 별도 프로세스 불필요, JNI로 직접 호출
3. **LSM-Tree**: 쓰기 최적화, 스트림 처리에 적합
4. **스냅샷**: 일관된 읽기 보장
5. **압축**: 디스크 사용량 최소화

### 7.4 캐싱 레이어

```
CachingKeyValueStore 동작:

쓰기: put("A", 1) -> put("A", 2) -> put("A", 3)

[캐시 없이]
  Changelog: [A=1], [A=2], [A=3]  (3개 레코드)
  RocksDB:   3번 쓰기

[캐시 있음 (cache.max.bytes.buffering)]
  캐시: A -> 3 (최신 값만 유지)
  flush 시:
    Changelog: [A=3]  (1개 레코드)
    RocksDB:   1번 쓰기

효과:
  - Changelog 토픽 트래픽 67% 감소
  - RocksDB 쓰기 67% 감소
  - 네트워크/디스크 I/O 절약
```

---

## 8. Changelog 토픽

### 8.1 Changelog 개념

```
Changelog 토픽 = State Store의 Write-Ahead Log

StateStore 쓰기 시:
    put("order-123", { status: "confirmed" })
        |
        v
    ChangeloggingKeyValueBytesStore
        |
        +-- 1. 실제 저장소(RocksDB)에 쓰기
        |
        +-- 2. Changelog 토픽에 전송
            토픽: {app.id}-{store-name}-changelog
            Key:   "order-123"
            Value: { status: "confirmed" }

Changelog 토픽 설정:
  - cleanup.policy: compact
  - 파티션 수: 소스 토픽과 동일
  - 복제 인수: 설정 가능 (기본 1, 운영 시 3 권장)
```

### 8.2 상태 복원

```
상태 복원 시나리오:

1. Task 재할당 (리밸런스 후):
   Thread-A에서 Thread-B로 Task 0_1 이동
   |
   Thread-B:
   |-- Changelog 토픽 처음부터 읽기
   |-- 각 레코드를 RocksDB에 replay
   |-- 복원 완료 -> Task RUNNING 전환

2. 인스턴스 재시작:
   로컬 RocksDB 디렉토리가 남아있으면:
   |-- 기존 데이터 로드
   |-- Changelog에서 마지막 체크포인트 이후만 replay
   => 빠른 복구 (Full restore 불필요)

3. StandbyTask로 빠른 복구:
   num.standby.replicas > 0 설정 시:
   |-- StandbyTask가 Changelog를 지속적으로 소비
   |-- Task 재할당 시 standby -> active 전환
   |-- 최소한의 복원만 필요
   => 가장 빠른 복구
```

```
복원 시간 비교:

                     복원 시간
Cold Start           ████████████████████  (전체 Changelog 재생)
(RocksDB 없음)

Warm Start           ██████               (체크포인트 이후만)
(RocksDB 남아있음)

Standby Replica      ██                   (마지막 동기화 이후만)
(StandbyTask)
```

### 8.3 Changelog 토픽 이름 규칙

```
Changelog 토픽 이름:

{application.id}-{store.name}-changelog

예:
  application.id = "order-processor"
  store.name = "orders-store"
  -> "order-processor-orders-store-changelog"

Repartition 토픽:
  {application.id}-{internal-node}-repartition
  예: "order-processor-KSTREAM-AGGREGATE-0001-repartition"
```

---

## 9. 윈도우 연산

### 9.1 윈도우 유형

```
1. Tumbling Window (고정 윈도우):
   +----------+----------+----------+
   |  0-10s   | 10-20s   | 20-30s   |
   |  [A,B,C] | [D,E]    | [F,G,H]  |
   +----------+----------+----------+
   - 겹침 없음, 간격 없음
   - 사용: 1분/1시간/1일 단위 집계

2. Hopping Window (슬라이딩 윈도우 - 고정 간격):
   +----------+
   |  0-10s   | [A,B,C]
   +----+-----+
        +----------+
        |  5-15s   | [C,D,E]
        +----+-----+
             +----------+
             | 10-20s   | [D,E,F]
             +----------+
   - 크기 10초, 간격 5초
   - 윈도우 간 겹침 존재
   - 사용: 이동 평균 계산

3. Sliding Window (슬라이딩 윈도우 - 이벤트 기반):
   이벤트 A(t=3), B(t=7), C(t=12), 윈도우=10s
   |-- 윈도우 [3, 13): [A, B, C]
   |-- 윈도우 [7, 17): [B, C]
   |-- 윈도우 [2, 12): [A, B]
   - 각 이벤트가 새 윈도우의 시작/끝
   - 사용: 정밀한 시간 기반 조인

4. Session Window (세션 윈도우):
   이벤트: A(t=0), B(t=5), [gap=30s], C(t=40), D(t=43)
   +--------+              +--------+
   | 세션 1 |              | 세션 2 |
   | [A, B] |   비활성 30s  | [C, D] |
   +--------+              +--------+
   - 비활성 간격(inactivity gap)으로 구분
   - 사용: 사용자 세션 분석
```

### 9.2 윈도우 상태 저장소

```
WindowStore 구조:

인터페이스: WindowStore<K, V>
  put(K key, V value, long windowStartTimestamp)
  fetch(K key, long timeFrom, long timeTo)

내부 저장: KeySchema
  실제 키 = [key] + [windowStartTimestamp]

RocksDB에 저장:
  Key: "user-123" + 1709100000000 (window start)
  Value: { count: 42 }

  Key: "user-123" + 1709100060000 (다음 window)
  Value: { count: 17 }

윈도우 보존:
  windowStore.retention(Duration.ofDays(1))
  -> 1일 이상 된 윈도우 자동 삭제
  -> RocksDB 컴팩션 시 정리
```

### 9.3 Late-Arriving 레코드

```
늦게 도착하는 레코드 처리:

grace period 설정:
  TimeWindows.ofSizeWithNoGrace(Duration.ofMinutes(5))
    -> 윈도우 종료 후 즉시 닫음 (늦은 레코드 무시)

  TimeWindows.ofSizeAndGrace(
    Duration.ofMinutes(5),   // 윈도우 크기
    Duration.ofMinutes(1))   // 유예 기간
    -> 윈도우 종료 후 1분간 늦은 레코드 수용

타임라인:
  |---윈도우 5분---|--유예 1분--|--폐쇄--|
  t=0            t=5          t=6
                  ^             ^
              윈도우 종료    최종 닫힘
              (아직 수용)   (이후 무시)
```

**왜 grace period가 필요한가?** 분산 시스템에서 이벤트는 항상 순서대로 도착하지
않는다. 네트워크 지연, 파티션 간 시간 차이 등으로 인해 윈도우 종료 후에도 해당
윈도우에 속하는 레코드가 도착할 수 있다. grace period는 이러한 상황에 대한
트레이드오프(정확성 vs 지연)를 제공한다.

---

## 10. DSL 연산자

### 10.1 Stateless 연산자

```
Stateless 연산자 (상태 없음, 1:1 또는 1:N 변환):

filter(Predicate):
  [A, B, C, D] -> filter(x > B) -> [C, D]

map(KeyValueMapper):
  [k1:v1, k2:v2] -> map(k,v -> (k.upper(), v+1))
                  -> [K1:v1+1, K2:v2+1]

flatMap(KeyValueMapper):
  [k:v] -> flatMap(k,v -> [(k,"a"), (k,"b")])
         -> [k:"a", k:"b"]

mapValues(ValueMapper):
  [k:v] -> mapValues(v -> v.length()) -> [k:3]
  키 변경 없음 -> repartition 불필요!

branch(Predicate...):
  [1,2,3,4,5] -> branch(x<3, x>=3)
              -> 스트림1: [1,2]
              -> 스트림2: [3,4,5]

selectKey(KeyValueMapper):
  [k:v] -> selectKey((k,v) -> v.userId)
        -> [userId:v]  (repartition 필요!)

peek(ForeachAction):
  [A,B,C] -> peek(x -> log(x)) -> [A,B,C]
  사이드 이펙트용, 스트림 변경 없음
```

### 10.2 Stateful 연산자

```
Stateful 연산자 (StateStore 필요):

groupByKey() + count():
  [A:1, B:1, A:1, C:1, A:1]
  -> groupByKey()
  -> count()
  -> KTable: {A:3, B:1, C:1}

groupByKey() + reduce():
  [user1:100, user1:200, user2:50]
  -> groupByKey()
  -> reduce((v1, v2) -> v1 + v2)
  -> KTable: {user1:300, user2:50}

groupByKey() + aggregate():
  [order1:{item:A, qty:2}, order1:{item:B, qty:1}]
  -> groupByKey()
  -> aggregate(
       () -> new Cart(),                    // 초기값
       (key, order, cart) -> cart.add(order) // 집계
     )
  -> KTable: {order1: Cart{A:2, B:1}}
```

### 10.3 Join 연산자

```
Join 유형:

1. KStream-KStream Join (윈도우 필수):
   orders: [orderId:order]
   payments: [orderId:payment]

   orders.join(payments,
     (order, payment) -> new Receipt(order, payment),
     JoinWindows.ofTimeDifferenceWithNoGrace(Duration.ofMinutes(5))
   )
   -> 5분 이내의 같은 키 레코드를 조인

2. KStream-KTable Join:
   clickStream: [userId:click]
   userTable: [userId:userProfile]

   clickStream.join(userTable,
     (click, profile) -> enrichedClick(click, profile)
   )
   -> 클릭 시점의 최신 사용자 프로필로 보강

3. KTable-KTable Join:
   ordersTable: [orderId:orderDetails]
   shipmentsTable: [orderId:shipmentStatus]

   ordersTable.join(shipmentsTable,
     (order, shipment) -> new OrderWithShipment(order, shipment)
   )
   -> 양쪽 테이블이 변경될 때마다 조인 결과 업데이트
```

```
Join 내부 구현:

KStream-KTable Join:

  KStream 레코드 도착:
    1. KTable의 StateStore에서 같은 키 조회
    2. 값이 있으면 ValueJoiner 호출
    3. 결과를 다음 노드로 전달

  KTable 레코드 도착:
    1. StateStore 업데이트
    2. 스트림 측에서 조인하므로 여기서는 전달 안 함

  => KStream 레코드가 도착할 때만 조인 발생
  => KTable은 항상 최신 상태 유지
```

---

## 11. KTable vs KStream: 이중성

### 11.1 Stream-Table Duality

```
스트림과 테이블의 이중성:

[KStream] = 이벤트의 연속
  시간 ->
  [A:1] [B:2] [A:3] [C:4] [A:5]

  각 레코드가 독립적인 이벤트
  A의 값: 1, 3, 5 (모두 유효)

[KTable] = 키별 최신 상태
  A -> 5  (최신 값)
  B -> 2
  C -> 4

  같은 키의 새 레코드가 이전 값을 대체
```

```
변환 관계:

Stream -> Table:
  stream.groupByKey().reduce((v1, v2) -> v2)
  = stream.toTable()

  스트림의 모든 이벤트를 재생하면 테이블이 됨
  (키별 마지막 값 = 현재 상태)

Table -> Stream:
  table.toStream()

  테이블의 각 변경이 스트림의 이벤트가 됨
  (CDC: Change Data Capture)
```

### 11.2 KTable의 내부 동작

```
KTable 내부 구조:

                   입력 토픽 (compact)
                        |
                        v
                   SourceNode
                        |
                        v
              +----+----+----+----+
              | StateStore         |
              | (Materialized)     |
              |                    |
              | A -> 5             |
              | B -> 2             |
              | C -> 4             |
              +----+----+----+----+
                        |
                        v
                   변경 전파
              (downstream에 변경 알림)

새 레코드 [A:7] 도착:
  1. StateStore: A -> 7 업데이트
  2. 변경 레코드 전파: (A, oldValue=5, newValue=7)
  3. Changelog 토픽에 기록
```

### 11.3 GlobalKTable

```
KTable vs GlobalKTable:

[KTable]
  - 파티션별: Task 0은 P0 데이터만 보유
  - co-partitioning 필요 (조인 시 같은 키가 같은 파티션)
  - 확장성: 파티션 수만큼 병렬화

[GlobalKTable]
  - 전체 복제: 모든 인스턴스가 모든 데이터 보유
  - co-partitioning 불필요
  - 작은 참조 데이터에 적합 (설정, 코드 테이블)
  - 주의: 데이터가 크면 메모리 부족

예:
  // 모든 인스턴스에 전체 country 테이블 복제
  GlobalKTable<String, String> countries =
      builder.globalTable("countries");

  // 어떤 파티션의 order든 country 조인 가능
  orders.join(countries,
      (orderId, order) -> order.countryCode(),
      (order, country) -> order.withCountry(country)
  );
```

---

## 12. Exactly-Once: processing.guarantee

### 12.1 Exactly-Once Semantics (EOS) 개요

```
processing.guarantee 설정:

1. "at_least_once" (기본값):
   +----------------------------------------------+
   | 장애 시 중복 처리 가능                        |
   | 오프셋 커밋과 출력 전송이 별도                |
   | 빠르지만 중복 허용                            |
   +----------------------------------------------+

2. "exactly_once_v2" (Kafka 2.5+):
   +----------------------------------------------+
   | 읽기-처리-쓰기가 트랜잭션으로 원자적          |
   | 오프셋 커밋 + 출력 전송 + StateStore 변경     |
   | 모두 하나의 트랜잭션으로 묶임                 |
   +----------------------------------------------+
```

### 12.2 At-Least-Once의 문제

```
At-Least-Once 중복 시나리오:

  1. poll(): 레코드 [A:1, B:2, C:3] 가져옴
  2. process(): A:1 처리, 출력 토픽에 전송
  3. process(): B:2 처리, 출력 토픽에 전송
  4. <<< 장애 발생 >>>
  5. 오프셋 커밋 안 됨!
  6. 재시작 후 마지막 커밋된 오프셋부터 다시 읽기
  7. poll(): 레코드 [A:1, B:2, C:3] 다시 가져옴
  8. process(): A:1 다시 처리 -> 출력에 중복!
```

### 12.3 Exactly-Once V2 동작

```
Exactly-Once V2 동작:

StreamThread별 하나의 트랜잭셔널 프로듀서 사용
(transactional.id = {app.id}-{thread.id})

처리 사이클:
  1. producer.beginTransaction()
  2. poll(): 레코드 가져옴
  3. process(): 처리 + 출력 전송
     (트랜잭션 내에서 producer.send())
  4. StateStore 변경 -> Changelog 토픽 전송
     (트랜잭션 내에서)
  5. 오프셋 커밋
     producer.sendOffsetsToTransaction()
     (트랜잭션 내에서)
  6. producer.commitTransaction()

모든 것이 원자적:
  - 출력 레코드 전송
  - StateStore changelog 기록
  - 입력 오프셋 커밋
  -> 전부 성공 또는 전부 실패
```

```
Exactly-Once V2 장애 복구:

장애 시나리오:
  1. beginTransaction()
  2. process() -> 출력 전송
  3. <<< 장애 >>>
  4. commitTransaction() 호출 안 됨

복구:
  1. 새 인스턴스가 같은 transactional.id로 시작
  2. InitProducerId() -> 이전 미커밋 트랜잭션 자동 ABORT
  3. ABORT된 출력 레코드는 Read Committed 컨슈머에게 안 보임
  4. 오프셋 커밋 안 됨 -> 마지막 커밋 오프셋부터 다시 처리
  5. 중복 없이 정확히 한 번 처리!
```

### 12.4 EOS V1 vs V2

```
V1 (exactly_once, 폐기됨) vs V2 (exactly_once_v2):

[V1] Task별 프로듀서:
  - Task 0_0: transactional.id = "app-0_0"
  - Task 0_1: transactional.id = "app-0_1"
  - Task 0_2: transactional.id = "app-0_2"
  => N개 Task = N개 프로듀서 = N개 트랜잭션
  => 브로커 부하 증가, 리소스 낭비

[V2] Thread별 프로듀서:
  - Thread-0: transactional.id = "app-Thread-0"
  - Thread-1: transactional.id = "app-Thread-1"
  => N개 Thread = N개 프로듀서
  => 스레드 수는 Task 수보다 훨씬 적음
  => 브로커 부하 대폭 감소
```

**왜 V2가 가능해졌는가?** Kafka 2.5에서 도입된 "Consumer Group Metadata"를
활용하여, 하나의 프로듀서가 여러 Task의 오프셋을 동일 트랜잭션에서 커밋할 수
있게 되었다. 이전에는 Task별로 독립적인 트랜잭션이 필요했지만, 그룹 메타데이터에
generationId를 포함시켜 리밸런스 시 좀비 Task를 안전하게 펜싱할 수 있게 되었다.

---

## 요약

```
Kafka Streams 아키텍처 요약:

API 계층:
  StreamsBuilder (DSL) -> Topology (Processor API)
  -> InternalTopologyBuilder -> ProcessorNode 그래프

실행 계층:
  KafkaStreams -> StreamThread[] -> TaskManager -> StreamTask[]
  각 StreamThread: Consumer + Producer + StateStore

처리 모델:
  poll -> addRecordsToTasks -> process -> punctuate -> commit
  파티션별 Task, 이벤트 시간 순서 처리

상태 관리:
  StateStore (RocksDB) -> Changelog 토픽 (내구성)
  캐싱 -> 배치 쓰기 (효율성)
  StandbyTask -> 빠른 복구

Exactly-Once:
  Kafka 트랜잭션으로 read-process-write 원자성
  V2: Thread별 프로듀서로 효율적 구현

핵심 설계 원칙:
1. 라이브러리 아키텍처: 클러스터 불필요, 어디서든 실행
2. Consumer Group 기반 병렬화: 자연스러운 확장
3. Changelog 기반 상태 내구성: Kafka 복제 활용
4. 이벤트 시간 처리: 순서 보장, 윈도우 정확성
5. Stream-Table 이중성: 유연한 데이터 모델링
```
