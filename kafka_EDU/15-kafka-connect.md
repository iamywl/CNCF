# 15. Kafka Connect 프레임워크 Deep-Dive

## 목차

1. [개요](#1-개요)
2. [Connect 아키텍처](#2-connect-아키텍처)
3. [Connector 인터페이스](#3-connector-인터페이스)
4. [SourceTask와 SinkTask](#4-sourcetask와-sinktask)
5. [Worker: 태스크 실행 엔진](#5-worker-태스크-실행-엔진)
6. [WorkerSourceTask와 WorkerSinkTask](#6-workersourcetask와-workersinktask)
7. [TransformationChain: SMT 파이프라인](#7-transformationchain-smt-파이프라인)
8. [DistributedHerder: 분산 모드 코디네이션](#8-distributedherder-분산-모드-코디네이션)
9. [오프셋 저장소](#9-오프셋-저장소)
10. [설정과 상태 저장](#10-설정과-상태-저장)
11. [REST API](#11-rest-api)
12. [에러 처리와 Dead Letter Queue](#12-에러-처리와-dead-letter-queue)
13. [왜(Why) 이렇게 설계했는가](#13-왜why-이렇게-설계했는가)

---

## 1. 개요

Kafka Connect는 Kafka와 외부 시스템 간의 데이터 이동을 표준화하는 프레임워크다. 데이터베이스, 파일 시스템,
검색 인덱스, 메시지 큐 등 다양한 시스템과 Kafka를 연결하기 위한 **플러그인 기반 아키텍처**를 제공한다.

```
소스 파일 위치:
  connect/api/src/main/java/org/apache/kafka/connect/     -- 공개 API (커넥터 개발용)
  connect/runtime/src/main/java/org/apache/kafka/connect/  -- 런타임 엔진
  connect/runtime/.../runtime/distributed/                 -- 분산 모드
  connect/runtime/.../storage/                             -- 오프셋/설정/상태 저장소
  connect/runtime/.../rest/resources/                      -- REST API
```

### Kafka Connect가 해결하는 문제

| 문제 | Connect의 해결 방식 |
|------|---------------------|
| 반복적인 프로듀서/컨슈머 코드 | 선언적 설정만으로 파이프라인 구성 |
| 오프셋 관리 | 프레임워크가 자동으로 오프셋 추적 |
| 장애 복구 | 분산 모드에서 자동 리밸런싱 |
| 스키마 진화 | Converter + Schema Registry 통합 |
| 확장성 | 태스크 수 동적 조절 |

---

## 2. Connect 아키텍처

### 전체 구조

```
+------------------------------------------------------------------+
|                      Kafka Connect Cluster                        |
|                                                                   |
|  +------------------+  +------------------+  +------------------+ |
|  |    Worker #1     |  |    Worker #2     |  |    Worker #3     | |
|  |                  |  |                  |  |                  | |
|  | +------+ +-----+ |  | +------+ +-----+ |  | +------+        | |
|  | |Source| |Sink | |  | |Source| |Sink | |  | |Sink  |        | |
|  | |Task-0| |Task-0| |  | |Task-1| |Task-1| |  | |Task-2|        | |
|  | +--+---+ +--+--+ |  | +--+---+ +--+--+ |  | +--+---+        | |
|  |    |        |     |  |    |        |     |  |    |            | |
|  +----+--------+-----+  +----+--------+-----+  +----+------------+ |
|       |        |              |        |              |             |
+-------+--------+--------------+--------+--------------+-------------+
        |        |              |        |              |
        v        v              v        v              v
  +-----------+    +-------------------------------------------+
  | External  |    |              Kafka Cluster                |
  | System    |    |  connect-configs  connect-offsets          |
  | (DB, S3..) |    |  connect-status   user-topics             |
  +-----------+    +-------------------------------------------+
```

### 핵심 컴포넌트 계층

```
+-------------------------------------------------------------------+
|                        Herder (코디네이터)                          |
|  - StandaloneHerder (단독 모드)                                    |
|  - DistributedHerder (분산 모드) -- 컨슈머 그룹 기반 코디네이션       |
+-------------------------------------------------------------------+
        |
        v
+-------------------------------------------------------------------+
|                        Worker (실행 엔진)                           |
|  - 커넥터/태스크 생명주기 관리                                       |
|  - Converter 설정 및 관리                                           |
|  - 플러그인 격리 (ClassLoader)                                      |
+-------------------------------------------------------------------+
        |
        +---> WorkerConnector (커넥터 래퍼)
        |         |
        |         +---> Connector.start() / taskConfigs() / stop()
        |
        +---> WorkerSourceTask / WorkerSinkTask (태스크 래퍼)
                  |
                  +---> SourceTask.poll() / SinkTask.put()
                  +---> TransformationChain (SMT 적용)
                  +---> Converter (직렬화/역직렬화)
```

**소스 파일**: `connect/runtime/src/main/java/org/apache/kafka/connect/runtime/Worker.java`

```java
// Worker.java (line 140)
public final class Worker {
    Herder herder;
    private final ExecutorService executor;
    private final Plugins plugins;
    private final WorkerConfig config;
    private final Converter internalKeyConverter;
    private final Converter internalValueConverter;
    private final OffsetBackingStore globalOffsetBackingStore;

    private final ConcurrentMap<String, WorkerConnector> connectors = new ConcurrentHashMap<>();
    private final ConcurrentMap<ConnectorTaskId, WorkerTask<?, ?>> tasks = new ConcurrentHashMap<>();
}
```

---

## 3. Connector 인터페이스

### 기본 Connector 추상 클래스

Connector는 외부 시스템과의 연결을 관리하고, Task 설정을 생성하는 역할을 한다.

**소스 파일**: `connect/api/src/main/java/org/apache/kafka/connect/connector/Connector.java`

```java
public abstract class Connector implements Versioned {
    protected ConnectorContext context;

    // 커넥터 초기화
    public void initialize(ConnectorContext ctx) {
        context = ctx;
    }

    // 설정 파싱 및 외부 시스템 연결
    public abstract void start(Map<String, String> props);

    // Task 구현 클래스 반환
    public abstract Class<? extends Task> taskClass();

    // 최대 maxTasks개의 Task 설정 생성
    public abstract List<Map<String, String>> taskConfigs(int maxTasks);

    // 커넥터 중지
    public abstract void stop();

    // 설정 검증
    public Config validate(Map<String, String> connectorConfigs) { ... }

    // 설정 정의
    public abstract ConfigDef config();
}
```

### SourceConnector와 SinkConnector

```
                    Connector (추상)
                        |
            +-----------+-----------+
            |                       |
     SourceConnector          SinkConnector
     (외부 → Kafka)           (Kafka → 외부)
            |                       |
     SourceTask               SinkTask
```

**소스 파일**: `connect/api/src/main/java/org/apache/kafka/connect/source/SourceConnector.java`
**소스 파일**: `connect/api/src/main/java/org/apache/kafka/connect/sink/SinkConnector.java`

### Connector의 두 가지 역할

| 역할 | 설명 |
|------|------|
| **Task 설정 분배** | 작업을 여러 Task에 분할 (예: DB 테이블별, 파일별) |
| **변경 감지** | 외부 시스템 변화를 감지하고 ConnectorContext로 런타임에 알림 |

```java
// ConnectorContext를 통해 재설정 요청
context.requestTaskReconfiguration();
```

### 왜 Connector와 Task를 분리했는가

Connector는 "무엇을 할 것인가"를 결정하고, Task는 "실제로 데이터를 옮기는" 작업자다.
이 분리 덕분에:

1. **병렬성**: 하나의 Connector가 여러 Task를 생성하여 병렬 처리
2. **재사용성**: Task 설정만 바꾸면 다른 테이블/토픽을 처리
3. **분산**: Task는 서로 다른 Worker에서 실행 가능

---

## 4. SourceTask와 SinkTask

### SourceTask: 외부 시스템 → Kafka

**소스 파일**: `connect/api/src/main/java/org/apache/kafka/connect/source/SourceTask.java`

```java
public abstract class SourceTask implements Task {
    protected SourceTaskContext context;

    // 설정 파싱 및 초기화
    public abstract void start(Map<String, String> props);

    // 외부 시스템에서 레코드 폴링 -- 핵심 메서드
    // 데이터가 없으면 블로킹, 주기적으로 null 반환하여 PAUSE 전환 허용
    public abstract List<SourceRecord> poll() throws InterruptedException;

    // 오프셋 커밋 시 호출 (선택적 구현)
    public void commit() throws InterruptedException { }

    // 개별 레코드 커밋 확인 콜백 (선택적 구현)
    public void commitRecord(SourceRecord record, RecordMetadata metadata)
            throws InterruptedException { }

    // 태스크 중지 -- 다른 스레드에서 호출됨
    public abstract void stop();
}
```

### SourceTask 실행 흐름

```
+-------------------+     poll()      +------------------+
|   WorkerSource    | <-------------- |    SourceTask    |
|      Task         |                 |   (사용자 구현)   |
+--------+----------+                 +------------------+
         |
         | List<SourceRecord>
         v
+-------------------+
| TransformationChain|  <-- SMT 적용
+--------+----------+
         |
         v
+-------------------+
|    Converter      |  <-- 직렬화 (JSON, Avro, Protobuf)
+--------+----------+
         |
         v
+-------------------+
|  KafkaProducer    |  <-- Kafka 토픽에 전송
+--------+----------+
         |
         v
+-------------------+
| OffsetStorage     |  <-- 오프셋 저장 (connect-offsets 토픽)
+-------------------+
```

### SourceRecord 구조

```java
// SourceRecord의 핵심 필드
public class SourceRecord extends ConnectRecord<SourceRecord> {
    Map<String, ?> sourcePartition;  // 소스 시스템의 파티션 식별자
    Map<String, ?> sourceOffset;     // 소스 시스템 내 오프셋
    String topic;                     // 대상 Kafka 토픽
    Integer kafkaPartition;           // 대상 Kafka 파티션 (선택)
    Schema keySchema;                 // 키 스키마
    Object key;                       // 키 데이터
    Schema valueSchema;               // 값 스키마
    Object value;                     // 값 데이터
}
```

### 트랜잭션 경계 (Exactly-Once)

**소스 파일**: `connect/api/src/main/java/org/apache/kafka/connect/source/SourceTask.java`

```java
public enum TransactionBoundary {
    POLL,       // poll() 호출 단위로 트랜잭션 (기본값)
    INTERVAL,   // 시간 간격 기반 트랜잭션
    CONNECTOR;  // 커넥터가 TransactionContext로 직접 제어
}
```

### SinkTask: Kafka → 외부 시스템

**소스 파일**: `connect/api/src/main/java/org/apache/kafka/connect/sink/SinkTask.java`

```java
public abstract class SinkTask implements Task {
    protected SinkTaskContext context;

    // 설정 파싱 및 초기화
    public abstract void start(Map<String, String> props);

    // 레코드 배치를 외부 시스템에 쓰기 -- 핵심 메서드
    public abstract void put(Collection<SinkRecord> records);

    // 배치 플러시 (오프셋 커밋 전)
    public void flush(Map<TopicPartition, OffsetAndMetadata> currentOffsets) { }

    // 커밋 전 훅 -- 기본 구현은 flush() 호출
    public Map<TopicPartition, OffsetAndMetadata> preCommit(
        Map<TopicPartition, OffsetAndMetadata> currentOffsets) {
        flush(currentOffsets);
        return currentOffsets;
    }

    // 파티션 할당 시 호출 (리밸런스 후)
    public void open(Collection<TopicPartition> partitions) { }

    // 파티션 해제 시 호출 (리밸런스 전)
    public void close(Collection<TopicPartition> partitions) { }

    // 태스크 중지
    public abstract void stop();
}
```

### SinkTask 생명주기

```
+----------+     +----------+     +---------+     +---------+     +------+
|initialize| --> |  start   | --> |  open   | --> |  put    | --> | flush|
+----------+     +----------+     +---------+     +---------+     +------+
                                      ^               |               |
                                      |               v               v
                                  +---------+     +---------+     +------+
                                  |  open   | <-- |  close  | <-- |commit|
                                  |(new pts)|     |(old pts)|     +------+
                                  +---------+     +---------+
                                                      |
                                                      v
                                                  +--------+
                                                  |  stop  |
                                                  +--------+
```

### SinkTask의 open()/close()가 필요한 이유

컨슈머 그룹 리밸런싱이 발생하면 파티션 할당이 바뀐다. SinkTask는 파티션별로 외부 시스템에
대한 writer를 유지할 수 있으므로:

- `close()`: 기존 파티션의 writer를 정리
- `open()`: 새 파티션에 대한 writer를 초기화

예를 들어 HDFS SinkTask는 파티션별로 파일 핸들을 관리한다.

---

## 5. Worker: 태스크 실행 엔진

Worker는 커넥터와 태스크의 생명주기를 관리하는 실행 엔진이다.

**소스 파일**: `connect/runtime/src/main/java/org/apache/kafka/connect/runtime/Worker.java`

### Worker의 핵심 필드

```java
public final class Worker {
    public static final long CONNECTOR_GRACEFUL_SHUTDOWN_TIMEOUT_MS =
        TimeUnit.SECONDS.toMillis(5);

    Herder herder;
    private final ExecutorService executor;           // 태스크 실행용 스레드 풀
    private final Plugins plugins;                    // 플러그인 관리
    private final WorkerConfig config;                // Worker 설정
    private final Converter internalKeyConverter;     // 내부 키 컨버터 (JSON)
    private final Converter internalValueConverter;   // 내부 값 컨버터 (JSON)
    private final OffsetBackingStore globalOffsetBackingStore;  // 글로벌 오프셋 저장소

    // 활성 커넥터/태스크 맵
    private final ConcurrentMap<String, WorkerConnector> connectors;
    private final ConcurrentMap<ConnectorTaskId, WorkerTask<?, ?>> tasks;
}
```

### Worker의 내부 컨버터

Worker는 내부적으로 JSON 컨버터를 사용한다. 이것은 오프셋 키/값을 직렬화하는 데 쓰인다.

```java
// Worker 생성자 내부
Map<String, String> internalConverterConfig =
    Map.of(JsonConverterConfig.SCHEMAS_ENABLE_CONFIG, "false");
this.internalKeyConverter = plugins.newInternalConverter(
    true, JsonConverter.class.getName(), internalConverterConfig);
this.internalValueConverter = plugins.newInternalConverter(
    false, JsonConverter.class.getName(), internalConverterConfig);
```

### Worker의 태스크 시작 과정

```
Worker.startConnector(connName, config, ctx, statusListener)
    |
    +---> plugins.newConnector(connectorClass)     -- 커넥터 인스턴스 생성
    +---> new WorkerConnector(...)                 -- 래퍼 생성
    +---> executor.submit(workerConnector)          -- 스레드에서 실행
              |
              +---> connector.initialize(ctx)
              +---> connector.start(config)
              +---> 상태를 STARTED로 전환

Worker.startSourceTask(taskId, config, ...)
    |
    +---> plugins.newTask(taskClass)               -- 태스크 인스턴스 생성
    +---> keyConverter / valueConverter 설정
    +---> TransformationChain 구성
    +---> new WorkerSourceTask(...)                -- 래퍼 생성
    +---> executor.submit(workerSourceTask)        -- 스레드에서 실행
```

### 플러그인 격리 (ClassLoader)

Worker는 각 커넥터/태스크에 대해 별도의 ClassLoader를 사용한다.
이는 서로 다른 커넥터가 같은 라이브러리의 다른 버전을 사용할 수 있게 한다.

```
+-------------------------------------------------+
|  Parent ClassLoader (Kafka Connect Runtime)      |
+-------------------------------------------------+
      |                    |                    |
      v                    v                    v
+------------+     +------------+     +------------+
| Plugin CL  |     | Plugin CL  |     | Plugin CL  |
| Connector A|     | Connector B|     | Connector C|
| (v1.0 lib) |     | (v2.0 lib) |     | (v3.0 lib) |
+------------+     +------------+     +------------+
```

**소스 파일**: `connect/runtime/src/main/java/org/apache/kafka/connect/runtime/isolation/Plugins.java`

---

## 6. WorkerSourceTask와 WorkerSinkTask

### WorkerSourceTask

**소스 파일**: `connect/runtime/src/main/java/org/apache/kafka/connect/runtime/WorkerSourceTask.java`

WorkerSourceTask는 AbstractWorkerSourceTask를 상속하며, SourceTask의 래퍼 역할을 한다.

```java
class WorkerSourceTask extends AbstractWorkerSourceTask {
    private volatile CommittableOffsets committableOffsets;
    final SubmittedRecords submittedRecords;
    private final AtomicReference<Exception> producerSendException;

    // 생성자에서 주입받는 핵심 의존성
    // - SourceTask task
    // - Converter keyConverter, valueConverter
    // - TransformationChain transformationChain
    // - Producer<byte[], byte[]> producer
    // - OffsetStorageWriter offsetWriter
    // - ConnectorOffsetBackingStore offsetStore
}
```

### WorkerSourceTask의 메인 루프

```
while (isRunning()) {
    +---> task.poll()                    -- 소스 시스템에서 레코드 폴링
    |         |
    |         v
    |     List<SourceRecord> records
    |         |
    +---> for each record:
    |         |
    |         +---> transformationChain.apply(record)  -- SMT 적용
    |         |
    |         +---> keyConverter.fromConnectData(...)   -- 키 직렬화
    |         +---> valueConverter.fromConnectData(...)  -- 값 직렬화
    |         |
    |         +---> producer.send(producerRecord)       -- Kafka에 전송
    |         |
    |         +---> submittedRecords.submit(record)     -- 오프셋 추적
    |
    +---> 주기적으로 commitOffsets()     -- 오프셋 커밋
}
```

### WorkerSinkTask

**소스 파일**: `connect/runtime/src/main/java/org/apache/kafka/connect/runtime/WorkerSinkTask.java`

```java
class WorkerSinkTask extends WorkerTask<ConsumerRecord<byte[], byte[]>, SinkRecord> {
    private final SinkTask task;
    private final Consumer<byte[], byte[]> consumer;        // Kafka 컨슈머
    private final List<SinkRecord> messageBatch;             // 배치 버퍼
    private final Map<TopicPartition, OffsetAndMetadata> currentOffsets;
    private final Map<TopicPartition, OffsetAndMetadata> lastCommittedOffsets;
}
```

### WorkerSinkTask의 메인 루프

```
while (isRunning()) {
    +---> consumer.poll(timeout)         -- Kafka에서 레코드 폴링
    |         |
    |         v
    |     ConsumerRecords<byte[], byte[]>
    |         |
    +---> for each record:
    |         |
    |         +---> keyConverter.toConnectData(...)   -- 키 역직렬화
    |         +---> valueConverter.toConnectData(...)  -- 값 역직렬화
    |         |
    |         +---> transformationChain.apply(...)    -- SMT 적용
    |         |
    |         +---> messageBatch.add(sinkRecord)     -- 배치에 추가
    |
    +---> task.put(messageBatch)          -- SinkTask에 배치 전달
    |
    +---> 주기적으로:
              task.preCommit(currentOffsets)
              consumer.commitSync(offsets)  -- 컨슈머 오프셋 커밋
}
```

### 컨슈머 리밸런스 처리

WorkerSinkTask는 ConsumerRebalanceListener를 구현하여 파티션 변경을 처리한다:

```
리밸런스 발생
    |
    +---> onPartitionsRevoked(oldPartitions)
    |         +---> task.close(oldPartitions)    -- 기존 파티션 정리
    |         +---> task.flush(currentOffsets)    -- 버퍼 플러시
    |         +---> consumer.commitSync(offsets)  -- 오프셋 커밋
    |
    +---> onPartitionsAssigned(newPartitions)
              +---> task.open(newPartitions)      -- 새 파티션 초기화
```

---

## 7. TransformationChain: SMT 파이프라인

SMT(Single Message Transforms)는 레코드가 커넥터와 Kafka 사이를 이동할 때 변환을 적용하는
경량 파이프라인이다.

**소스 파일**: `connect/runtime/src/main/java/org/apache/kafka/connect/runtime/TransformationChain.java`

```java
public class TransformationChain<T, R extends ConnectRecord<R>> implements AutoCloseable {
    private final List<TransformationStage<R>> transformationStages;
    private final RetryWithToleranceOperator<T> retryWithToleranceOperator;

    public R apply(ProcessingContext<T> context, R record) {
        if (transformationStages.isEmpty()) return record;

        for (final TransformationStage<R> transformationStage : transformationStages) {
            final R current = record;
            // 에러 허용 정책에 따라 실행
            record = retryWithToleranceOperator.execute(
                context,
                () -> transformationStage.apply(current),
                Stage.TRANSFORMATION,
                transformationStage.transformClass()
            );
            if (record == null) break;  // 필터링: null 반환 시 레코드 드롭
        }
        return record;
    }
}
```

### SMT 체인 동작

```
SourceRecord/SinkRecord
    |
    v
+-------------------+
| Transform #1      |  예: ExtractField
| (ValueToKey)      |
+--------+----------+
         |
         v
+-------------------+
| Transform #2      |  예: ReplaceField
| (mask/rename)     |
+--------+----------+
         |
         v
+-------------------+
| Transform #3      |  예: TimestampRouter
| (topic routing)   |
+--------+----------+
         |
         v
   변환된 레코드 (또는 null로 드롭)
```

### null 반환으로 필터링하는 이유

SMT에서 null을 반환하면 해당 레코드가 드롭된다. 이 설계는:

1. **단순성**: 별도의 필터링 API 없이 기존 Transformation 인터페이스로 처리
2. **체인 단절**: null이 반환되면 이후 변환을 건너뛰어 성능 최적화
3. **유연성**: Filter SMT와 조합하여 조건부 필터링 구현

### 내장 SMT 목록

| SMT | 설명 |
|-----|------|
| InsertField | 정적/동적 필드 삽입 |
| ReplaceField | 필드 이름 변경/제거 |
| MaskField | 필드 값 마스킹 |
| ValueToKey | 값의 필드를 키로 복사 |
| ExtractField | 구조체에서 단일 필드 추출 |
| SetSchemaMetadata | 스키마 이름/버전 변경 |
| TimestampRouter | 타임스탬프 기반 토픽 라우팅 |
| RegexRouter | 정규식 기반 토픽 이름 변경 |
| Flatten | 중첩 구조 평탄화 |
| Cast | 필드 타입 변환 |
| HeaderFrom | 레코드 필드를 헤더로 복사 |
| InsertHeader | 정적 헤더 삽입 |
| DropHeaders | 헤더 제거 |
| Filter | 조건부 레코드 필터링 |

---

## 8. DistributedHerder: 분산 모드 코디네이션

### Herder 인터페이스

**소스 파일**: `connect/runtime/src/main/java/org/apache/kafka/connect/runtime/Herder.java`

```java
public interface Herder {
    void start();
    void stop();
    boolean isReady();

    // 커넥터 관리
    void putConnectorConfig(String connName, Map<String, String> config, ...);
    void deleteConnectorConfig(String connName, Callback<Herder.Created<ConnectorInfo>> cb);

    // 상태 조회
    ConnectorStateInfo connectorStatus(String connName);

    // 태스크 관리
    void restartTask(ConnectorTaskId taskId);
    void restartConnectorAndTasks(RestartRequest request, ...);
}
```

### DistributedHerder 구조

**소스 파일**: `connect/runtime/src/main/java/org/apache/kafka/connect/runtime/distributed/DistributedHerder.java`

```java
package org.apache.kafka.connect.runtime.distributed;

public class DistributedHerder extends AbstractHerder implements Runnable {
    // 핵심 의존성
    private final Worker worker;
    private final WorkerGroupMember member;           // 컨슈머 그룹 멤버
    private final ConfigBackingStore configBackingStore;  // 설정 저장소
    private final StatusBackingStore statusBackingStore;  // 상태 저장소
}
```

### 분산 모드의 컨슈머 그룹 기반 코디네이션

DistributedHerder는 Kafka의 컨슈머 그룹 프로토콜을 활용하여 Worker 간 조율을 수행한다.

```
+-------------------------------------------------------------------+
|                    Kafka Cluster                                   |
|                                                                   |
|  __consumer_offsets 토픽 (그룹 코디네이션)                          |
|  connect-configs 토픽 (커넥터 설정 공유)                            |
|  connect-status 토픽 (상태 공유)                                   |
|  connect-offsets 토픽 (오프셋 공유)                                 |
+-------------------------------------------------------------------+
         |              |              |
         v              v              v
   +-----------+  +-----------+  +-----------+
   | Worker #1 |  | Worker #2 |  | Worker #3 |
   | (Leader)  |  | (Follower)|  | (Follower)|
   +-----------+  +-----------+  +-----------+
```

### 리밸런싱 프로토콜

```
그룹 코디네이터 (Kafka 브로커)
    |
    +---> JoinGroup 요청 수신
    |
    +---> Leader Worker 선출
    |
    +---> Leader가 할당 계산
    |         |
    |         +---> IncrementalCooperativeAssignor  (증분 협력적)
    |         |     또는
    |         +---> EagerAssignor                   (전체 재할당)
    |
    +---> SyncGroup으로 할당 배포
    |
    +---> 각 Worker가 할당된 커넥터/태스크 시작
```

**소스 파일**: `connect/runtime/src/main/java/org/apache/kafka/connect/runtime/distributed/IncrementalCooperativeAssignor.java`

### 왜 컨슈머 그룹 프로토콜을 재사용하는가

1. **이미 검증된 메커니즘**: Kafka의 컨슈머 그룹 프로토콜은 수년간 프로덕션에서 검증됨
2. **자동 장애 감지**: 하트비트 기반으로 Worker 장애를 자동 감지
3. **리밸런싱**: 새 Worker 추가/제거 시 자동으로 재분배
4. **추가 인프라 불필요**: ZooKeeper나 별도 코디네이션 서비스 없이 Kafka만으로 동작

### 증분 협력적 리밸런싱 (Incremental Cooperative Rebalancing)

기존 Eager 리밸런싱은 모든 태스크를 중지하고 재분배했다. 이는 큰 클러스터에서 중단 시간을
만든다. 증분 협력적 리밸런싱은 이를 개선한다:

```
Eager 리밸런싱:                    Incremental Cooperative:
+---+---+---+---+---+---+        +---+---+---+---+---+---+
|T1 |T2 |T3 |T4 |T5 |T6 |        |T1 |T2 |T3 |T4 |T5 |T6 |
+---+---+---+---+---+---+        +---+---+---+---+---+---+
|  Worker A  |  Worker B |        |  Worker A  |  Worker B |
+------------+-----------+        +------------+-----------+
                                        |
  리밸런스 시: 모든 태스크 중지            리밸런스 시: T3만 이동
  -> T1~T6 모두 일시 중단                -> T1,T2,T4,T5,T6 계속 실행
```

---

## 9. 오프셋 저장소

### KafkaOffsetBackingStore

**소스 파일**: `connect/runtime/src/main/java/org/apache/kafka/connect/storage/KafkaOffsetBackingStore.java`

```java
public class KafkaOffsetBackingStore extends KafkaTopicBasedBackingStore
    implements OffsetBackingStore {
    // connect-offsets 토픽을 사용하여 오프셋 저장
    // KafkaBasedLog를 내부적으로 사용 (프로듀서 + 컨슈머)
}
```

### 오프셋 저장 흐름

```
SourceTask.poll()
    |
    +---> SourceRecord { sourcePartition, sourceOffset }
    |
    v
OffsetStorageWriter
    |
    +---> 키: {connector: "my-conn", partition: sourcePartition}
    +---> 값: sourceOffset
    |
    v
KafkaProducer.send()
    |
    v
connect-offsets 토픽
    [key: {"connector":"my-conn","partition":{...}}]
    [value: {"offset": 12345}]
```

### 오프셋 복구 시 읽기

```
SourceTask 시작 시:
    |
    +---> OffsetStorageReader.offset(sourcePartition)
    |         |
    |         v
    |     connect-offsets 토픽 전체 소비 (컴팩션된 최신 값)
    |         |
    |         v
    |     인메모리 맵에서 오프셋 조회
    |
    +---> task.start(config)
    +---> task.poll() -- 저장된 오프셋부터 폴링 재개
```

### ConnectorOffsetBackingStore (KIP-618)

**소스 파일**: `connect/runtime/src/main/java/org/apache/kafka/connect/storage/ConnectorOffsetBackingStore.java`

```java
public class ConnectorOffsetBackingStore implements OffsetBackingStore {
    // 커넥터별 전용 오프셋 토픽 지원
    // Worker 글로벌 오프셋 스토어 + 커넥터별 오프셋 스토어 조합
}
```

KIP-618 이전에는 모든 커넥터가 하나의 `connect-offsets` 토픽을 공유했다. 이는:

1. **격리 부족**: 커넥터 삭제 시 오프셋이 남아있음
2. **보안**: 한 커넥터가 다른 커넥터의 오프셋에 접근 가능
3. **성능**: 많은 커넥터가 하나의 토픽을 공유하면 컴팩션 부하

ConnectorOffsetBackingStore는 커넥터별 전용 토픽을 지원하여 이 문제를 해결한다:

```
+---------------------------------------------+
|      ConnectorOffsetBackingStore             |
|                                              |
|  +-------------------+  +------------------+ |
|  | Worker Global     |  | Connector-Specific| |
|  | OffsetStore       |  | OffsetStore       | |
|  | (connect-offsets) |  | (my-conn-offsets) | |
|  +-------------------+  +------------------+ |
|                                              |
|  읽기: 커넥터별 토픽 우선 -> 글로벌 폴백       |
|  쓰기: 커넥터별 토픽에만 기록                   |
+---------------------------------------------+
```

---

## 10. 설정과 상태 저장

### 내부 토픽 구조

```
+-------------------------------------------------------------------+
|                     Kafka 내부 토픽                                 |
|                                                                   |
|  connect-configs (compact)                                        |
|    - 커넥터 설정, 태스크 설정, 커밋 레코드                           |
|    - key: connector-<name>, task-<name>-<id>                      |
|                                                                   |
|  connect-offsets (compact)                                        |
|    - SourceTask 오프셋                                             |
|    - key: ["connector","partition-info"]                           |
|                                                                   |
|  connect-status (compact)                                         |
|    - 커넥터/태스크 상태 (RUNNING, PAUSED, FAILED 등)                |
|    - key: status-connector-<name>, status-task-<name>-<id>        |
+-------------------------------------------------------------------+
```

### KafkaConfigBackingStore

**소스 파일**: `connect/runtime/src/main/java/org/apache/kafka/connect/storage/KafkaConfigBackingStore.java`

```
connect-configs 토픽의 레코드 유형:

  +--------------------------------------------------+
  | connector-<name>     | 커넥터 설정 (JSON)          |
  | task-<name>-<id>     | 태스크 설정 (JSON)          |
  | commit-<name>        | 태스크 설정 커밋 마커         |
  | target-state-<name>  | 목표 상태 (STARTED/PAUSED)  |
  | session-key          | 분산 모드 세션 키             |
  +--------------------------------------------------+
```

### KafkaStatusBackingStore

**소스 파일**: `connect/runtime/src/main/java/org/apache/kafka/connect/storage/KafkaStatusBackingStore.java`

```
connect-status 토픽의 레코드:

  키: status-connector-<name>
  값: {
    "state": "RUNNING",
    "worker_id": "worker1:8083",
    "generation": 3
  }

  키: status-task-<name>-<id>
  값: {
    "state": "RUNNING",
    "worker_id": "worker2:8083",
    "generation": 3,
    "trace": null
  }
```

### 왜 Kafka 토픽을 내부 저장소로 사용하는가

1. **자체 의존성 최소화**: 외부 DB 없이 Kafka만으로 동작
2. **복제와 내구성**: Kafka의 복제 메커니즘을 그대로 활용
3. **컴팩션**: Log compaction으로 최신 설정만 유지
4. **분산 공유**: 모든 Worker가 같은 토픽을 소비하여 상태 동기화

---

## 11. REST API

**소스 파일**: `connect/runtime/src/main/java/org/apache/kafka/connect/runtime/rest/resources/ConnectorsResource.java`

### ConnectorsResource 엔드포인트

```java
@Path("/connectors")
@Produces(MediaType.APPLICATION_JSON)
@Consumes(MediaType.APPLICATION_JSON)
public class ConnectorsResource {
    private final Herder herder;
    // ...
}
```

### REST API 목록

| 메서드 | 경로 | 설명 |
|--------|------|------|
| GET | /connectors | 모든 커넥터 목록 |
| POST | /connectors | 커넥터 생성 |
| GET | /connectors/{name} | 커넥터 정보 |
| GET | /connectors/{name}/config | 커넥터 설정 |
| PUT | /connectors/{name}/config | 커넥터 설정 변경/생성 |
| PATCH | /connectors/{name}/config | 커넥터 설정 부분 변경 |
| DELETE | /connectors/{name} | 커넥터 삭제 |
| GET | /connectors/{name}/status | 커넥터 상태 |
| POST | /connectors/{name}/restart | 커넥터 재시작 |
| GET | /connectors/{name}/tasks | 태스크 목록 |
| GET | /connectors/{name}/tasks/{id}/status | 태스크 상태 |
| POST | /connectors/{name}/tasks/{id}/restart | 태스크 재시작 |
| PUT | /connectors/{name}/pause | 커넥터 일시정지 |
| PUT | /connectors/{name}/resume | 커넥터 재개 |
| PUT | /connectors/{name}/stop | 커넥터 중지 |
| GET | /connectors/{name}/offsets | 오프셋 조회 |
| PATCH | /connectors/{name}/offsets | 오프셋 변경 |
| DELETE | /connectors/{name}/offsets | 오프셋 삭제 |

### REST 요청 흐름 (분산 모드)

```
클라이언트 → Worker REST API
    |
    +---> ConnectorsResource
    |         |
    |         v
    +---> DistributedHerder
              |
              +---> 요청이 리더 Worker용인가?
              |         |
              |    Yes  +---> 직접 처리
              |    No   +---> RestClient로 리더에 포워딩
              |
              +---> configBackingStore에 설정 기록 (connect-configs)
              |
              +---> 리밸런싱 트리거 (필요 시)
```

### REST API 포워딩이 필요한 이유

분산 모드에서 커넥터 설정 변경은 리더 Worker만 처리할 수 있다. 클라이언트가 팔로워 Worker에
요청을 보내면, 해당 Worker는 투명하게 리더로 포워딩한다. 이는:

1. **일관성**: 설정 변경은 리더가 직렬화하여 처리
2. **편의성**: 클라이언트는 아무 Worker에나 요청 가능
3. **가용성**: 로드 밸런서 뒤에 Worker들을 배치 가능

---

## 12. 에러 처리와 Dead Letter Queue

### RetryWithToleranceOperator

**소스 파일**: `connect/runtime/src/main/java/org/apache/kafka/connect/runtime/errors/RetryWithToleranceOperator.java`

```
에러 허용 모드:
  errors.tolerance = none     -- 에러 발생 시 태스크 즉시 실패 (기본값)
  errors.tolerance = all      -- 에러를 로깅하고 레코드 스킵

에러 재시도:
  errors.retry.timeout = 0    -- 재시도 없음 (기본값)
  errors.retry.delay.max.ms   -- 최대 재시도 지연
```

### Dead Letter Queue (DLQ)

```
정상 레코드 흐름:
  SourceTask → Transform → Converter → KafkaProducer → output-topic

에러 레코드 흐름 (errors.tolerance = all):
  SourceTask → Transform → Converter → [에러 발생!]
                                            |
                                            v
                                    +-------------------+
                                    | DeadLetterQueue   |
                                    | Reporter          |
                                    +-------------------+
                                            |
                                            v
                                    errors.deadletterqueue.topic.name
                                    (에러 레코드 + 에러 메타데이터 헤더)
```

### 에러 리포터 체인

```java
// Worker에서 에러 리포터 구성
List<ErrorReporter<SourceRecord>> errorReporters = new ArrayList<>();
errorReporters.add(new LogReporter(...));           // 항상 로깅
errorReporters.add(new DeadLetterQueueReporter(...)); // DLQ 설정 시
```

**소스 파일**: `connect/runtime/src/main/java/org/apache/kafka/connect/runtime/errors/DeadLetterQueueReporter.java`
**소스 파일**: `connect/runtime/src/main/java/org/apache/kafka/connect/runtime/errors/LogReporter.java`

---

## 13. 왜(Why) 이렇게 설계했는가

### Q: 왜 Connect는 Kafka의 일부인가? 별도 프로젝트가 아닌 이유는?

Connect는 Kafka 에코시스템의 핵심 통합 계층이다. Kafka에 내장됨으로써:

1. **프로토콜 호환성**: Kafka 버전과 항상 동기화
2. **동일한 설정 패턴**: Kafka 클라이언트 설정을 그대로 재사용
3. **배포 단순화**: Kafka 설치에 포함, 별도 설치 불필요

### Q: 왜 Converter를 런타임에서 분리했는가?

Converter는 데이터 포맷(JSON, Avro, Protobuf)과 커넥터 로직을 분리한다:

```
커넥터 개발자: ConnectRecord (구조화된 데이터)만 다룸
   |
   v
Converter: 직렬화/역직렬화 담당 (플러그인으로 교체 가능)
   |
   v
Kafka: byte[] 저장
```

이 분리 덕분에 하나의 JDBC 커넥터가 JSON으로도, Avro로도 동작한다.

### Q: 왜 오프셋을 Kafka 토픽에 저장하는가?

1. **At-least-once 보장**: 레코드 전송과 오프셋 저장이 같은 Kafka 인프라를 사용
2. **장애 복구**: Worker가 죽어도 다른 Worker가 오프셋을 읽어 이어서 처리
3. **컴팩션**: 오래된 오프셋은 자동 삭제, 최신 값만 유지

### Q: 왜 Worker당 하나의 스레드가 아니라 태스크당 하나의 스레드인가?

태스크당 스레드를 할당하면:

1. **격리**: 하나의 느린 태스크가 다른 태스크에 영향 없음
2. **병렬성**: CPU 코어를 최대한 활용
3. **단순성**: 각 태스크가 독립적인 상태 머신으로 동작

```
Worker 프로세스
    |
    +---> Thread-1: WorkerConnector-A
    +---> Thread-2: WorkerSourceTask-A-0
    +---> Thread-3: WorkerSourceTask-A-1
    +---> Thread-4: WorkerSinkTask-B-0
    +---> Thread-5: WorkerSinkTask-B-1
```

### Q: 왜 DistributedHerder의 설정 변경은 리더만 처리하는가?

Kafka Connect의 설정은 `connect-configs` 토픽에 저장된다. 여러 Worker가 동시에 설정을
기록하면 경쟁 조건이 발생한다. 리더가 직렬화하여:

1. **순서 보장**: 설정 변경이 순서대로 적용
2. **충돌 방지**: 동시 수정으로 인한 비일관성 방지
3. **원자성**: 커넥터 생성과 태스크 설정이 함께 커밋

---

## 부록: 주요 소스 파일 색인

| 파일 | 경로 | 설명 |
|------|------|------|
| Connector.java | connect/api/.../connector/Connector.java | 커넥터 기본 인터페이스 |
| SourceTask.java | connect/api/.../source/SourceTask.java | 소스 태스크 인터페이스 |
| SinkTask.java | connect/api/.../sink/SinkTask.java | 싱크 태스크 인터페이스 |
| Worker.java | connect/runtime/.../runtime/Worker.java | 태스크 실행 엔진 |
| WorkerConnector.java | connect/runtime/.../runtime/WorkerConnector.java | 커넥터 래퍼 |
| WorkerSourceTask.java | connect/runtime/.../runtime/WorkerSourceTask.java | 소스 태스크 래퍼 |
| WorkerSinkTask.java | connect/runtime/.../runtime/WorkerSinkTask.java | 싱크 태스크 래퍼 |
| TransformationChain.java | connect/runtime/.../runtime/TransformationChain.java | SMT 파이프라인 |
| DistributedHerder.java | connect/runtime/.../distributed/DistributedHerder.java | 분산 코디네이터 |
| KafkaOffsetBackingStore.java | connect/runtime/.../storage/KafkaOffsetBackingStore.java | 오프셋 저장소 |
| ConnectorOffsetBackingStore.java | connect/runtime/.../storage/ConnectorOffsetBackingStore.java | 커넥터별 오프셋 |
| KafkaConfigBackingStore.java | connect/runtime/.../storage/KafkaConfigBackingStore.java | 설정 저장소 |
| KafkaStatusBackingStore.java | connect/runtime/.../storage/KafkaStatusBackingStore.java | 상태 저장소 |
| ConnectorsResource.java | connect/runtime/.../rest/resources/ConnectorsResource.java | REST API |
| IncrementalCooperativeAssignor.java | connect/runtime/.../distributed/IncrementalCooperativeAssignor.java | 증분 리밸런싱 |
| RetryWithToleranceOperator.java | connect/runtime/.../errors/RetryWithToleranceOperator.java | 에러 허용 처리 |
| DeadLetterQueueReporter.java | connect/runtime/.../errors/DeadLetterQueueReporter.java | DLQ 리포터 |
