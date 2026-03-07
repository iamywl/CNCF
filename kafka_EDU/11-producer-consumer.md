# 11. 프로듀서와 컨슈머 Deep-Dive

## 목차

1. [개요](#1-개요)
2. [KafkaProducer: send()에서 브로커까지](#2-kafkaproducer-send에서-브로커까지)
3. [RecordAccumulator: 배치 관리 엔진](#3-recordaccumulator-배치-관리-엔진)
4. [BuiltInPartitioner: Sticky Partitioning](#4-builtinpartitioner-sticky-partitioning)
5. [Sender: 네트워크 I/O 스레드](#5-sender-네트워크-io-스레드)
6. [KafkaConsumer: poll() 흐름](#6-kafkaconsumer-poll-흐름)
7. [ClassicKafkaConsumer: 레거시 프로토콜](#7-classickafkaconsumer-레거시-프로토콜)
8. [Fetcher: 데이터 가져오기](#8-fetcher-데이터-가져오기)
9. [ConsumerCoordinator: 그룹 관리](#9-consumercoordinator-그룹-관리)
10. [Idempotent Producer: 중복 방지](#10-idempotent-producer-중복-방지)
11. [스레드 모델](#11-스레드-모델)
12. [설정 최적화](#12-설정-최적화)

---

## 1. 개요

Kafka 클라이언트는 크게 **프로듀서(Producer)**와 **컨슈머(Consumer)**로 나뉜다. 프로듀서는 레코드를
브로커에 전송하고, 컨슈머는 브로커에서 레코드를 가져온다. 이 두 클라이언트는 단순해 보이지만 내부적으로
**배치 처리**, **비동기 I/O**, **멱등성**, **트랜잭션** 등 정교한 메커니즘을 포함한다.

### 소스 위치

| 컴포넌트 | 소스 경로 |
|----------|----------|
| KafkaProducer | `clients/src/main/java/org/apache/kafka/clients/producer/KafkaProducer.java` |
| RecordAccumulator | `clients/src/main/java/org/apache/kafka/clients/producer/internals/RecordAccumulator.java` |
| BuiltInPartitioner | `clients/src/main/java/org/apache/kafka/clients/producer/internals/BuiltInPartitioner.java` |
| Sender | `clients/src/main/java/org/apache/kafka/clients/producer/internals/Sender.java` |
| BufferPool | `clients/src/main/java/org/apache/kafka/clients/producer/internals/BufferPool.java` |
| ProducerBatch | `clients/src/main/java/org/apache/kafka/clients/producer/internals/ProducerBatch.java` |
| TransactionManager | `clients/src/main/java/org/apache/kafka/clients/producer/internals/TransactionManager.java` |
| KafkaConsumer | `clients/src/main/java/org/apache/kafka/clients/consumer/KafkaConsumer.java` |
| ClassicKafkaConsumer | `clients/src/main/java/org/apache/kafka/clients/consumer/internals/ClassicKafkaConsumer.java` |
| AsyncKafkaConsumer | `clients/src/main/java/org/apache/kafka/clients/consumer/internals/AsyncKafkaConsumer.java` |
| Fetcher | `clients/src/main/java/org/apache/kafka/clients/consumer/internals/Fetcher.java` |
| FetchCollector | `clients/src/main/java/org/apache/kafka/clients/consumer/internals/FetchCollector.java` |
| ConsumerCoordinator | `clients/src/main/java/org/apache/kafka/clients/consumer/internals/ConsumerCoordinator.java` |

---

## 2. KafkaProducer: send()에서 브로커까지

### 2.1 전체 흐름

```
사용자 코드                        KafkaProducer                  브로커
    |                                  |                            |
    |-- send(ProducerRecord) --------->|                            |
    |                                  |-- interceptors.onSend() -->|
    |                                  |-- doSend()                 |
    |                                  |   |-- waitOnMetadata()     |
    |                                  |   |-- serialize(key)       |
    |                                  |   |-- serialize(value)     |
    |                                  |   |-- partition()          |
    |                                  |   |-- accumulator.append() |
    |                                  |   |-- sender.wakeup()      |
    |<-- Future<RecordMetadata> -------|                            |
    |                                  |                            |
    |                  [Sender Thread] |-- runOnce()                |
    |                                  |   |-- sendProducerData()   |
    |                                  |   |   |-- ready()          |
    |                                  |   |   |-- drain()          |
    |                                  |   |   |-- sendProduceReqs()|
    |                                  |   |-- client.poll()  ----->|
    |                                  |                     <------|
    |                                  |-- callback.onCompletion()  |
```

### 2.2 send() 메서드

`KafkaProducer.send()`는 두 단계로 나뉜다.

```java
// KafkaProducer.java (라인 1055-1063)
public Future<RecordMetadata> send(ProducerRecord<K, V> record, Callback callback) {
    ProducerRecord<K, V> interceptedRecord = this.interceptors.onSend(record);
    return doSend(interceptedRecord, callback);
}
```

**왜 인터셉터를 먼저 호출하는가?** 인터셉터는 레코드가 직렬화되기 전에 실행되어야 한다.
사용자가 레코드를 수정하거나 메트릭을 수집하는 용도로 사용하기 때문이다.

### 2.3 doSend() 상세 분석

`doSend()`는 실제 전송 로직의 핵심이다 (`KafkaProducer.java` 라인 1093).

```
doSend() 실행 단계:
+--------------------------------------------------------------------+
| 1. throwIfProducerClosed()        - 프로듀서 상태 확인              |
| 2. waitOnMetadata(topic)          - 토픽 메타데이터 대기            |
| 3. keySerializer.serialize()      - 키 직렬화                      |
| 4. valueSerializer.serialize()    - 값 직렬화                      |
| 5. partition()                    - 파티션 결정                     |
| 6. ensureValidRecordSize()        - 레코드 크기 검증               |
| 7. accumulator.append()           - 어큐뮬레이터에 추가            |
| 8. transactionManager.maybeAdd()  - 트랜잭션 파티션 등록           |
| 9. sender.wakeup()                - Sender 스레드 깨우기           |
+--------------------------------------------------------------------+
```

핵심 코드 (라인 1116-1163):

```java
// 키/값 직렬화
byte[] serializedKey = keySerializerPlugin.get().serialize(
    record.topic(), record.headers(), record.key());
byte[] serializedValue = valueSerializerPlugin.get().serialize(
    record.topic(), record.headers(), record.value());

// 파티션 결정
int partition = partition(record, serializedKey, serializedValue, cluster);

// 어큐뮬레이터에 추가
RecordAccumulator.RecordAppendResult result = accumulator.append(
    record.topic(), partition, timestamp,
    serializedKey, serializedValue, headers,
    appendCallbacks, remainingWaitMs, nowMs, cluster);

// 배치가 가득 찼거나 새 배치가 생성되면 Sender 깨우기
if (result.batchIsFull || result.newBatchCreated) {
    this.sender.wakeup();
}
```

### 2.4 partition() 메서드

파티션 결정은 3단계 우선순위를 따른다 (`KafkaProducer.java` 라인 1588):

```
파티션 결정 우선순위:
+--------------------------------------------------+
| 1. record.partition() != null                     |
|    -> 사용자 지정 파티션 사용                      |
+--------------------------------------------------+
| 2. partitionerPlugin.get() != null                |
|    -> 커스텀 Partitioner 사용                      |
+--------------------------------------------------+
| 3. serializedKey != null && !partitionerIgnoreKeys|
|    -> BuiltInPartitioner.partitionForKey()         |
|    -> Utils.toPositive(murmur2(key)) % numParts   |
+--------------------------------------------------+
| 4. 그 외                                          |
|    -> UNKNOWN_PARTITION 반환                       |
|    -> RecordAccumulator에서 Sticky Partitioning    |
+--------------------------------------------------+
```

**왜 UNKNOWN_PARTITION을 반환하는가?** 키가 없는 메시지의 경우, 파티션 결정을
RecordAccumulator로 미루는 것이 더 효율적이다. 어큐뮬레이터는 각 파티션의 배치 상태를
알고 있으므로, 현재 열려있는 배치에 메시지를 추가하여 네트워크 효율을 높일 수 있다.

---

## 3. RecordAccumulator: 배치 관리 엔진

### 3.1 내부 구조

RecordAccumulator는 프로듀서의 심장이다. 레코드를 파티션별 배치로 묶어 관리한다.

```
RecordAccumulator 내부 구조:
+------------------------------------------------------------+
| RecordAccumulator                                           |
|                                                             |
|  topicInfoMap: ConcurrentMap<String, TopicInfo>             |
|  +-------------------------------------------------------+ |
|  | "my-topic" -> TopicInfo                                | |
|  |   builtInPartitioner: BuiltInPartitioner               | |
|  |   batches: CopyOnWriteMap<Integer, Deque<ProducerBatch>>||
|  |   +--------------------------------------------------+| |
|  |   | partition-0 -> Deque [batch1, batch2, ...]        || |
|  |   | partition-1 -> Deque [batch3, ...]                || |
|  |   | partition-2 -> Deque [batch4, batch5, ...]        || |
|  |   +--------------------------------------------------+| |
|  +-------------------------------------------------------+ |
|                                                             |
|  free: BufferPool (buffer.memory 크기의 ByteBuffer 풀)      |
|  muted: Set<TopicPartition> (순서 보장 시 뮤트된 파티션)     |
|  incomplete: IncompleteBatches (전송 중인 배치 추적)         |
+------------------------------------------------------------+
```

### 3.2 핵심 필드

```java
// RecordAccumulator.java 주요 필드 (라인 68-91)
public class RecordAccumulator {
    private final int batchSize;              // batch.size 설정값
    private final Compression compression;     // 압축 코덱
    private final int lingerMs;               // linger.ms 설정값
    private final int deliveryTimeoutMs;       // delivery.timeout.ms
    private final boolean enableAdaptivePartitioning;  // 적응형 파티셔닝
    private final BufferPool free;             // ByteBuffer 풀
    private final ConcurrentMap<String, TopicInfo> topicInfoMap;  // 토픽별 정보
    private final Set<TopicPartition> muted;   // 순서 보장용 뮤트 파티션
    private final TransactionManager transactionManager;
}
```

### 3.3 append() 흐름

레코드가 어큐뮬레이터에 추가되는 과정 (`RecordAccumulator.java` 라인 275):

```
append() 실행 흐름:

  1. TopicInfo 조회 또는 생성
     |
  2. partition == UNKNOWN_PARTITION?
     |-- YES --> BuiltInPartitioner에서 sticky partition 선택
     |-- NO  --> 지정된 파티션 사용
     |
  3. 해당 파티션의 Deque<ProducerBatch> 가져오기
     |
  4. synchronized(deque) {
     |   마지막 배치에 레코드 추가 시도
     |   |-- 성공 --> 결과 반환
     |   |-- 실패 (배치 가득참) --> 계속
     }
     |
  5. BufferPool에서 새 ByteBuffer 할당
     |-- 메모리 부족 시 max.block.ms까지 대기
     |
  6. synchronized(deque) {
     |   다시 마지막 배치에 추가 시도 (다른 스레드가 새 배치 생성했을 수 있음)
     |   |-- 성공 --> 새 buffer 해제, 결과 반환
     |   |-- 실패 --> 새 ProducerBatch 생성, deque에 추가
     }
     |
  7. RecordAppendResult 반환
     (batchIsFull, newBatchCreated 플래그 포함)
```

**왜 두 번 시도하는가(double-check)?** 멀티스레드 환경에서 첫 번째 시도가 실패한 후
ByteBuffer를 할당하는 동안 다른 스레드가 이미 새 배치를 생성했을 수 있다.
이 패턴은 불필요한 메모리 할당을 방지하는 최적화이다.

### 3.4 BufferPool

BufferPool은 고정 크기(batch.size)의 ByteBuffer를 재사용하는 풀이다.

```
BufferPool 구조:
+-----------------------------------------------+
| BufferPool (total = buffer.memory)             |
|                                                |
|  free: Deque<ByteBuffer>  [buf1][buf2][buf3]   |
|  ^-- 재사용 가능한 batch.size 크기 버퍼        |
|                                                |
|  nonPooledAvailableMemory: long                |
|  ^-- 풀링되지 않은 가용 메모리                  |
|                                                |
|  waiters: Deque<Condition>                     |
|  ^-- 메모리 대기 중인 스레드들                  |
+-----------------------------------------------+
|                                                |
| 할당 규칙:                                      |
| - 요청 크기 == batch.size -> free에서 가져옴    |
| - 요청 크기 != batch.size -> 새로 할당          |
| - 메모리 부족 -> waiters에 등록, max.block.ms   |
|   까지 대기                                     |
+-----------------------------------------------+
```

**왜 batch.size 크기만 풀링하는가?** 대부분의 배치가 동일한 크기(batch.size)로 생성되므로,
이 크기의 버퍼만 풀링하면 GC 압력을 크게 줄일 수 있다. 비정상 크기의 버퍼(예: 단일
대형 레코드)는 사용 후 GC에 맡긴다.

### 3.5 ProducerBatch

각 배치는 `ProducerBatch` 객체로 관리된다.

```
ProducerBatch 구조:
+---------------------------------------------+
| ProducerBatch                                |
|  topicPartition: TopicPartition              |
|  recordsBuilder: MemoryRecordsBuilder        |
|  createdMs: long                             |
|  lastAppendTime: long                        |
|  produceFuture: ProduceRequestResult         |
|  lastAttemptMs: long                         |
|  attemptsLeft: int                           |
|  inRetry: boolean                            |
|  producerId: long   (멱등성 프로듀서용)       |
|  producerEpoch: short                        |
|  baseSequence: int                           |
+---------------------------------------------+
```

---

## 4. BuiltInPartitioner: Sticky Partitioning

### 4.1 KIP-794: Sticky Partitioning 개선

Kafka 3.3 이전에는 키가 없는 메시지를 라운드로빈으로 파티션에 분배했다. 이 방식은 각
레코드가 다른 파티션의 배치에 들어가 작은 배치가 많이 생성되는 문제가 있었다.

KIP-794는 **Sticky Partitioning**을 도입했다. 하나의 파티션에 batch.size만큼 데이터를
모은 후 다른 파티션으로 전환한다.

```
라운드로빈 vs Sticky Partitioning:

[라운드로빈 - 비효율적]
Record 1 -> Partition 0  [batch: |R1|........]
Record 2 -> Partition 1  [batch: |R2|........]
Record 3 -> Partition 2  [batch: |R3|........]
Record 4 -> Partition 0  [batch: |R1|R4|.....]
=> 작은 배치 3개 동시 전송

[Sticky Partitioning - 효율적]
Record 1 -> Partition 0  [batch: |R1|R2|R3|R4|]  <- 가득 참!
Record 2 -> Partition 0
Record 3 -> Partition 0
Record 4 -> Partition 0
Record 5 -> Partition 1  [batch: |R5|R6|R7|...]  <- 전환
=> 큰 배치 1개 전송, 압축률 향상
```

### 4.2 적응형 파티셔닝 (Adaptive Partitioning)

`BuiltInPartitioner.java`의 `nextPartition()` 메서드 (라인 66)는 단순 랜덤이 아닌
**가중 랜덤(weighted random)**을 사용한다.

```java
// BuiltInPartitioner.java (라인 66-112)
private int nextPartition(Cluster cluster) {
    int random = randomPartition();
    PartitionLoadStats partitionLoadStats = this.partitionLoadStats;

    if (partitionLoadStats == null) {
        // 통계 없음 -> 균등 분배
        List<PartitionInfo> availablePartitions =
            cluster.availablePartitionsForTopic(topic);
        partition = availablePartitions.get(
            random % availablePartitions.size()).partition();
    } else {
        // 누적 빈도 테이블 기반 가중 랜덤 선택
        int[] cumulativeFrequencyTable =
            partitionLoadStats.cumulativeFrequencyTable;
        int weightedRandom = random %
            cumulativeFrequencyTable[partitionLoadStats.length - 1];
        int searchResult = Arrays.binarySearch(
            cumulativeFrequencyTable, 0,
            partitionLoadStats.length, weightedRandom);
        int partitionIndex = Math.abs(searchResult + 1);
        partition = partitionLoadStats.partitionIds[partitionIndex];
    }
    return partition;
}
```

```
가중 랜덤 선택 예시:

파티션별 큐 크기 (작은 것이 좋음):
  P0: 큐 100KB,  P1: 큐 50KB,  P2: 큐 200KB

역수 가중치 계산:
  P0: 1/100 = 0.01,  P1: 1/50 = 0.02,  P2: 1/200 = 0.005

정규화:
  P0: 28.6%,  P1: 57.1%,  P2: 14.3%

누적 빈도 테이블:
  [286, 857, 1000]

random = 500 -> binarySearch -> P1 선택 (큐가 가장 작은 파티션)
```

**왜 가중 랜덤인가?** 브로커별 처리 속도가 다를 수 있다. 느린 브로커의 파티션에는
데이터가 더 적게 전송되어, 전체 처리량이 균형을 이룬다. 이것이 "적응형(adaptive)"
파티셔닝이라 불리는 이유다.

### 4.3 파티션 전환 시점

```
Sticky Partition 전환 조건:
+--------------------------------------------------+
| stickyBatchSize 바이트만큼 현재 파티션에 전송 완료 |
| OR                                                |
| 현재 배치가 가득 참 (batch.size 도달)              |
| OR                                                |
| 현재 파티션이 더 이상 사용 불가                    |
+--------------------------------------------------+
        |
        v
  nextPartition() 호출 -> 새 파티션으로 전환
```

---

## 5. Sender: 네트워크 I/O 스레드

### 5.1 Sender 스레드 개요

Sender는 별도의 I/O 스레드에서 실행되며, 어큐뮬레이터의 배치를 브로커에 전송한다.

```java
// KafkaProducer.java (라인 470-472) - Sender 스레드 시작
this.sender = newSender(logContext, kafkaClient, this.metadata);
String ioThreadName = NETWORK_THREAD_PREFIX + " | " + clientId;
this.ioThread = new Sender.SenderThread(ioThreadName, this.sender, true);
this.ioThread.start();
```

### 5.2 run() 메인 루프

```java
// Sender.java (라인 241-304)
@Override
public void run() {
    // 메인 루프 - close 호출까지 실행
    while (running) {
        try { runOnce(); }
        catch (Exception e) { log.error("Uncaught error", e); }
    }
    // 셧다운: 남은 배치/요청 완료 대기
    while (!forceClose && (accumulator.hasUndrained() ||
           client.inFlightRequestCount() > 0 ||
           hasPendingTransactionalRequests())) {
        try { runOnce(); }
        catch (Exception e) { log.error("Uncaught error", e); }
    }
    // 강제 종료 시 미완료 배치 중단
    if (forceClose) {
        if (transactionManager != null) transactionManager.close();
        accumulator.abortIncompleteBatches();
    }
    client.close();
}
```

### 5.3 runOnce() 상세

```java
// Sender.java (라인 310-346)
void runOnce() {
    // 1. 트랜잭션 관련 처리
    if (transactionManager != null) {
        transactionManager.maybeResolveSequences();
        if (transactionManager.hasFatalError()) {
            maybeAbortBatches(lastError);
            client.poll(retryBackoffMs, time.milliseconds());
            return;
        }
        transactionManager.bumpIdempotentEpochAndResetIdIfNeeded();
        if (maybeSendAndPollTransactionalRequest()) return;
    }
    // 2. 프로듀스 데이터 전송
    long currentTimeMs = time.milliseconds();
    long pollTimeout = sendProducerData(currentTimeMs);
    // 3. 네트워크 I/O 폴링
    client.poll(pollTimeout, currentTimeMs);
}
```

### 5.4 sendProducerData() 핵심 로직

```
sendProducerData() 실행 단계:
+-----------------------------------------------------------+
| 1. accumulator.ready()                                     |
|    -> 전송 준비된 노드 목록 + 리더 모르는 토픽 목록         |
|                                                            |
| 2. 리더 모르는 토픽이 있으면 메타데이터 업데이트 요청       |
|                                                            |
| 3. 각 ready 노드에 대해 client.ready() 확인                |
|    -> 연결 안 됨 -> 목록에서 제거                           |
|    -> 연결됨 -> latency 통계 업데이트                       |
|                                                            |
| 4. accumulator.drain()                                     |
|    -> 노드별 전송할 배치 목록 생성                          |
|    -> maxRequestSize 이내로 배치 묶기                       |
|                                                            |
| 5. addToInflightBatches()                                  |
|    -> in-flight 배치 추적 시작                              |
|                                                            |
| 6. guaranteeMessageOrder이면 파티션 뮤트                    |
|    -> max.in.flight.requests.per.connection == 1           |
|                                                            |
| 7. 만료된 배치 처리 (delivery.timeout.ms 초과)              |
|                                                            |
| 8. sendProduceRequests()                                   |
|    -> 노드별 ProduceRequest 생성 및 전송                    |
+-----------------------------------------------------------+
```

핵심 코드 (`Sender.java` 라인 380-455):

```java
private long sendProducerData(long now) {
    MetadataSnapshot metadataSnapshot = metadata.fetchMetadataSnapshot();

    // 1. 전송 준비된 노드 확인
    RecordAccumulator.ReadyCheckResult result =
        this.accumulator.ready(metadataSnapshot, now);

    // 2. 리더 모르는 토픽 -> 메타데이터 갱신
    if (!result.unknownLeaderTopics.isEmpty()) {
        for (String topic : result.unknownLeaderTopics)
            this.metadata.add(topic, now);
        this.metadata.requestUpdate(false);
    }

    // 3. 연결 안 된 노드 제거
    Iterator<Node> iter = result.readyNodes.iterator();
    while (iter.hasNext()) {
        Node node = iter.next();
        if (!this.client.ready(node, now)) {
            iter.remove();
        }
    }

    // 4. 배치 drain
    Map<Integer, List<ProducerBatch>> batches =
        this.accumulator.drain(metadataSnapshot,
            result.readyNodes, this.maxRequestSize, now);
    addToInflightBatches(batches);

    // 5. 순서 보장 모드 -> 뮤트
    if (guaranteeMessageOrder) {
        for (List<ProducerBatch> batchList : batches.values())
            for (ProducerBatch batch : batchList)
                this.accumulator.mutePartition(batch.topicPartition);
    }

    // 6. 만료 처리
    List<ProducerBatch> expiredBatches = this.accumulator.expiredBatches(now);
    failExpiredBatches(expiredBatches, now, true);

    // 7. 실제 전송
    sendProduceRequests(batches, now);
    return pollTimeout;
}
```

### 5.5 drain() 동작

```
drain() - 노드별 배치 수집:

Node 0 (broker-0):
  topic-A/partition-0: [batch1, batch2]  -> drain batch1
  topic-B/partition-1: [batch3]          -> drain batch3
  => Node 0: [batch1, batch3] (maxRequestSize 이내)

Node 1 (broker-1):
  topic-A/partition-1: [batch4]          -> drain batch4
  topic-A/partition-2: [batch5, batch6]  -> drain batch5
  => Node 1: [batch4, batch5]

각 노드의 drain은 라운드로빈으로 파티션을 순회하여
특정 파티션이 독점하지 않도록 한다.
(nodesDrainIndex로 시작 위치 기억)
```

---

## 6. KafkaConsumer: poll() 흐름

### 6.1 아키텍처 개요

KafkaConsumer는 **위임 패턴(Delegation Pattern)**을 사용한다.

```
KafkaConsumer (파사드)
    |
    |-- delegate: ConsumerDelegate
        |
        +-- ClassicKafkaConsumer  (Classic 프로토콜)
        |   |-- ConsumerCoordinator
        |   |-- Fetcher
        |   |-- ConsumerNetworkClient
        |
        +-- AsyncKafkaConsumer    (Consumer 프로토콜, KIP-848)
            |-- ConsumerHeartbeatRequestManager
            |-- FetchRequestManager
            |-- ConsumerNetworkThread
```

**왜 위임 패턴인가?** Kafka는 두 가지 컨슈머 프로토콜을 지원한다:
1. **Classic 프로토콜**: JoinGroup/SyncGroup 기반, 클라이언트 측 리밸런싱
2. **Consumer 프로토콜 (KIP-848)**: ConsumerGroupHeartbeat 기반, 서버 측 리밸런싱

같은 KafkaConsumer API로 두 프로토콜을 투명하게 전환하기 위해 위임 패턴을 사용한다.

### 6.2 poll() 전체 흐름

```
consumer.poll(Duration.ofMillis(1000))
    |
    v
+-- ConsumerDelegate.poll(timeout) ----+
|                                       |
|  1. updateAssignmentMetadataIfNeeded()|
|     |-- coordinator.poll()            |
|     |   |-- 그룹 참여 확인            |
|     |   |-- 하트비트 전송             |
|     |   |-- 리밸런스 처리             |
|     |-- updateFetchPositions()        |
|     |   |-- 리셋 필요한 오프셋 처리   |
|     |   |-- committed 오프셋 조회     |
|                                       |
|  2. pollForFetches(timeout)           |
|     |-- fetcher.sendFetches()         |
|     |   |-- 파티션별 FetchRequest 생성|
|     |   |-- 브로커에 비동기 전송      |
|     |-- client.poll(timeout)          |
|     |   |-- 네트워크 I/O              |
|     |   |-- 응답 처리                 |
|     |-- fetcher.collectFetch()        |
|     |   |-- 완료된 fetch 수집         |
|     |   |-- 역직렬화                  |
|     |   |-- ConsumerRecords 생성      |
|                                       |
|  3. interceptors.onConsume(records)   |
|                                       |
|  4. return ConsumerRecords            |
+---------------------------------------+
```

---

## 7. ClassicKafkaConsumer: 레거시 프로토콜

### 7.1 핵심 구성 요소

ClassicKafkaConsumer (`ClassicKafkaConsumer.java`)는 다음 컴포넌트를 조합한다:

```
ClassicKafkaConsumer 내부 구조:
+----------------------------------------------------+
| ClassicKafkaConsumer                                |
|                                                     |
|  coordinator: ConsumerCoordinator                   |
|  |-- 그룹 참여/탈퇴, 리밸런스, 오프셋 커밋          |
|                                                     |
|  fetcher: Fetcher<K, V>                             |
|  |-- FetchRequest 생성/전송, 응답 수집              |
|                                                     |
|  client: ConsumerNetworkClient                      |
|  |-- 네트워크 I/O, 요청/응답 처리                   |
|                                                     |
|  subscriptions: SubscriptionState                   |
|  |-- 구독 토픽/파티션, 오프셋 위치 관리             |
|                                                     |
|  metadata: ConsumerMetadata                         |
|  |-- 토픽/파티션/브로커 메타데이터                   |
+----------------------------------------------------+
```

### 7.2 pollForFetches()

```
pollForFetches() 상세:

  1. fetcher.collectFetch() 호출
     |-- 이전 poll에서 받은 응답이 있는지 확인
     |-- 있으면 바로 반환 (네트워크 대기 불필요)
     |
  2. 결과 없으면:
     |-- fetcher.sendFetches()
     |   |-- 각 파티션의 리더 노드에 FetchRequest 생성
     |   |-- 비동기 전송
     |-- client.poll(timeout)
     |   |-- 네트워크 I/O 대기
     |   |-- FetchResponse 처리
     |-- fetcher.collectFetch()
     |   |-- CompletedFetch에서 레코드 추출
     |   |-- 역직렬화, ConsumerRecords 생성
     |
  3. 반환
```

**왜 두 번 collectFetch를 호출하는가?** 첫 번째 호출은 이전 poll() 사이클에서 이미
도착했지만 아직 처리하지 않은 응답을 확인한다. 네트워크 왕복을 줄이는 최적화이다.

---

## 8. Fetcher: 데이터 가져오기

### 8.1 Fetcher 클래스 계층

```
AbstractFetch
  |-- fetchBuffer: FetchBuffer
  |-- metricsManager: FetchMetricsManager
  |-- subscriptions: SubscriptionState
  |
  +-- Fetcher<K, V>  (Classic 프로토콜용)
  |   |-- client: ConsumerNetworkClient
  |   |-- fetchCollector: FetchCollector<K, V>
  |
  +-- FetchRequestManager  (Consumer 프로토콜용)
      |-- networkClientDelegate: NetworkClientDelegate
      |-- fetchCollector: FetchCollector<K, V>
```

### 8.2 sendFetches()

Fetcher가 FetchRequest를 생성하는 과정:

```
sendFetches() 프로세스:
+----------------------------------------------------------+
| 1. prepareFetchRequests()                                 |
|    |-- 각 fetchable 파티션에 대해:                         |
|    |   |-- 리더 노드 확인                                  |
|    |   |-- 이미 in-flight 요청이 있는 노드 제외            |
|    |   |-- FetchSessionHandler로 세션 기반 요청 구성       |
|    |                                                      |
| 2. 노드별 FetchRequest.Builder 생성                       |
|    |-- maxWaitMs: fetch.max.wait.ms                       |
|    |-- minBytes: fetch.min.bytes                          |
|    |-- maxBytes: fetch.max.bytes                          |
|    |-- 파티션별: fetchOffset, maxBytes, logStartOffset    |
|                                                           |
| 3. 각 노드에 비동기 전송                                   |
|    |-- client.send(node, request)                         |
|    |-- 응답 콜백 등록 -> FetchBuffer에 결과 저장           |
+----------------------------------------------------------+
```

### 8.3 FetchSession

```
FetchSession 최적화:

[첫 번째 요청 - Full Fetch]
Client -> Broker: {파티션: [P0, P1, P2, P3], offset: [100, 200, 300, 400]}
Broker -> Client: {sessionId: 42, data: [...], 파티션: [P0, P1, P2, P3]}

[두 번째 요청 - Incremental Fetch]
Client -> Broker: {sessionId: 42, 변경된 파티션만: [P1: offset 250]}
Broker -> Client: {sessionId: 42, data: [...], 변경된 파티션만}

=> 요청/응답 크기 대폭 감소 (수백 파티션 구독 시 효과적)
```

**왜 FetchSession을 사용하는가?** 컨슈머가 수백 개의 파티션을 구독하면 매번 모든
파티션 정보를 보내야 한다. FetchSession은 첫 번째 full fetch 이후 변경된 파티션만
보내는 incremental fetch를 지원하여 네트워크 대역폭을 절약한다.

### 8.4 collectFetch()와 FetchCollector

```
FetchCollector 처리 흐름:

FetchBuffer (응답 큐)
    |
    v
CompletedFetch
    |-- partition: TopicPartition
    |-- records: MemoryRecords
    |-- fetchOffset: long
    |
    v
FetchCollector.collectFetch()
    |
    |-- 1. position 검증 (현재 위치와 fetch 오프셋 일치 확인)
    |-- 2. 각 레코드 역직렬화
    |   |-- keyDeserializer.deserialize()
    |   |-- valueDeserializer.deserialize()
    |-- 3. ConsumerRecord 생성
    |-- 4. max.poll.records 제한 적용
    |-- 5. position 업데이트
    |
    v
ConsumerRecords<K, V> (사용자에게 반환)
```

---

## 9. ConsumerCoordinator: 그룹 관리

### 9.1 역할

ConsumerCoordinator는 클라이언트 측에서 컨슈머 그룹 프로토콜을 관리한다.

```
ConsumerCoordinator 주요 책임:
+----------------------------------------------+
| 1. 그룹 코디네이터 발견                        |
|    FindCoordinator 요청으로 담당 브로커 확인    |
|                                                |
| 2. 그룹 참여 (JoinGroup)                       |
|    멤버 ID 할당, 리더 선출                      |
|                                                |
| 3. 동기화 (SyncGroup)                          |
|    리더: 파티션 할당 결과 전송                   |
|    팔로워: 할당 결과 수신                       |
|                                                |
| 4. 하트비트                                    |
|    그룹 멤버십 유지, 리밸런스 감지              |
|                                                |
| 5. 오프셋 커밋                                 |
|    auto.commit 또는 수동 커밋                   |
|                                                |
| 6. 리밸런스 리스너 호출                         |
|    onPartitionsRevoked/Assigned                |
+----------------------------------------------+
```

### 9.2 JoinGroup/SyncGroup 흐름

```
Consumer A (리더)          Coordinator           Consumer B (팔로워)
     |                         |                       |
     |-- JoinGroup ----------->|<---------- JoinGroup--|
     |                         |                       |
     |                   (리더 선출)                    |
     |                         |                       |
     |<-- JoinResponse --------|-------- JoinResponse->|
     |    (리더 플래그,          |     (멤버 목록 없음)  |
     |     전체 멤버 목록)       |                      |
     |                         |                       |
     |  [파티션 할당 계산]       |                      |
     |                         |                       |
     |-- SyncGroup ----------->|<--------- SyncGroup --|
     |   (할당 결과 포함)       |    (빈 할당)          |
     |                         |                       |
     |<-- SyncResponse --------|------- SyncResponse ->|
     |   (내 파티션 목록)       |   (내 파티션 목록)    |
     |                         |                       |
     |-- Heartbeat ----------->|<--------- Heartbeat --|
     |   (주기적)              |       (주기적)        |
```

### 9.3 오프셋 커밋

```
오프셋 커밋 흐름:

Consumer
    |-- commitSync() / commitAsync()
    |   또는 auto.commit.interval.ms마다 자동
    |
    v
OffsetCommitRequest {
    groupId: "my-group",
    generationId: 3,
    memberId: "consumer-1-xxx",
    topics: [{
        name: "my-topic",
        partitions: [{
            partitionIndex: 0,
            committedOffset: 1500,
            committedMetadata: ""
        }]
    }]
}
    |
    v
Coordinator Broker
    |-- __consumer_offsets 토픽에 기록
    |-- OffsetCommitResponse 반환
```

---

## 10. Idempotent Producer: 중복 방지

### 10.1 멱등성의 필요성

```
네트워크 실패 시나리오 (멱등성 없음):

Producer                    Broker
   |-- Produce(msg-A) ------->|
   |                          |-- 로그에 msg-A 기록 (offset 100)
   |    X 응답 유실 X         |
   |                          |
   |-- [타임아웃, 재전송]      |
   |-- Produce(msg-A) ------->|
   |                          |-- 로그에 msg-A 중복 기록 (offset 101)
   |<-- Success (101) --------|

결과: msg-A가 2번 기록됨 (offset 100, 101)
```

### 10.2 ProducerId + Epoch + Sequence 트리플

```
멱등성 메커니즘:

Producer                         Broker
   |                               |
   |-- InitProducerId() ---------> |
   |<-- PID=42, Epoch=0 ---------- |
   |                               |
   |-- Produce(PID=42, Ep=0,       |
   |   Seq=0, msg-A) -----------> |
   |                               |-- Seq 0 처리, 기록
   |   X 응답 유실 X               |
   |                               |
   |-- Produce(PID=42, Ep=0,       |
   |   Seq=0, msg-A) -----------> |
   |                               |-- Seq 0 이미 처리됨!
   |                               |-- 중복 감지, 이전 결과 반환
   |<-- Success (offset 100) ----- |

결과: msg-A는 1번만 기록됨 (offset 100)
```

### 10.3 시퀀스 번호 관리

```
파티션별 시퀀스 번호 추적:

TransactionManager (클라이언트):
+----------------------------------+
| partition-0: nextSequence = 0    |
| partition-1: nextSequence = 0    |
+----------------------------------+

Batch 1 (partition-0, 3 records):
  baseSequence = 0, lastSequence = 2
  nextSequence = 3

Batch 2 (partition-0, 2 records):
  baseSequence = 3, lastSequence = 4
  nextSequence = 5

브로커 측:
+----------------------------------+
| partition-0:                     |
|   PID=42, Epoch=0               |
|   lastSequence = 4              |
|   -> 다음 Seq 5 기대            |
|   -> Seq < 5: 중복 (DUP)        |
|   -> Seq > 5: 순서 오류         |
+----------------------------------+
```

### 10.4 에포크(Epoch)의 역할

```
에포크 범프 시나리오:

Old Producer (PID=42, Epoch=0)
    |-- 장애 발생

New Producer (같은 transactional.id)
    |-- InitProducerId()
    |-- PID=42, Epoch=1   <- 에포크 증가

Old Producer 복구 시도:
    |-- Produce(PID=42, Epoch=0, ...)
    |-- 브로커: Epoch 0 < 현재 1 -> PRODUCER_FENCED 에러
    |-- Old Producer 차단됨 (Zombie Fencing)
```

**왜 에포크가 필요한가?** 프로듀서 장애 복구 시, 이전 인스턴스("좀비")가 여전히
살아있을 수 있다. 에포크 메커니즘은 항상 최신 인스턴스만 쓸 수 있도록 보장하여
데이터 일관성을 유지한다.

---

## 11. 스레드 모델

### 11.1 프로듀서 스레드 모델

```
KafkaProducer 스레드 모델:

[사용자 스레드 (N개)]           [Sender 스레드 (1개)]
     |                              |
     |-- send()                     |-- runOnce() 루프
     |   |-- serialize              |   |-- sendProducerData()
     |   |-- partition              |   |   |-- ready()
     |   |-- accumulator.append()   |   |   |-- drain()
     |   |   (synchronized)         |   |   |-- sendProduceRequests()
     |   |-- sender.wakeup()        |   |-- client.poll()
     |                              |   |   |-- 응답 처리
     |                              |   |   |-- 콜백 실행

동기화 지점:
- RecordAccumulator.append(): deque 동기화
- BufferPool.allocate(): 메모리 할당 동기화
- IncompleteBatches: ConcurrentHashMap

스레드 안전:
- KafkaProducer는 thread-safe (여러 스레드에서 공유 가능)
- send()는 여러 스레드에서 동시 호출 가능
- 하나의 Sender 스레드가 모든 네트워크 I/O 처리
```

### 11.2 컨슈머 스레드 모델

```
KafkaConsumer 스레드 모델:

[Classic 프로토콜]
[사용자 스레드 (1개)]
     |
     |-- poll()
     |   |-- coordinator.poll()
     |   |   |-- heartbeat
     |   |   |-- joinGroup/syncGroup
     |   |-- fetcher.sendFetches()
     |   |-- client.poll()
     |   |-- fetcher.collectFetch()
     |
     주의: KafkaConsumer는 NOT thread-safe
     ConcurrentModificationException 발생 가능

[Consumer 프로토콜 (KIP-848)]
[사용자 스레드]              [NetworkThread]
     |                           |
     |-- poll()                  |-- 이벤트 루프
     |   |-- 이벤트 전송 ------->|   |-- HeartbeatRequestManager
     |   |-- 결과 대기           |   |-- FetchRequestManager
     |   |<-- 레코드 반환 ------|   |-- CommitRequestManager
     |                           |   |-- client.poll()

=> Consumer 프로토콜은 백그라운드 스레드가 네트워크 처리
   하트비트가 poll()과 독립적으로 실행됨
```

**왜 Consumer 프로토콜에서 스레드를 분리했는가?** Classic 프로토콜에서는 poll()
호출이 늦으면 하트비트도 늦어져 불필요한 리밸런스가 발생했다.
Consumer 프로토콜은 네트워크 스레드를 분리하여, 사용자의 레코드 처리 시간이
길어도 하트비트가 정상적으로 전송되도록 한다.

---

## 12. 설정 최적화

### 12.1 프로듀서 핵심 설정

```
+--------------------+----------+------------------------------------------------+
| 설정               | 기본값   | 설명 및 튜닝 가이드                             |
+--------------------+----------+------------------------------------------------+
| batch.size         | 16384    | 배치 크기 (바이트). 클수록 처리량 증가,          |
|                    |          | 지연 시간 증가. 64KB~1MB 권장 (고처리량)        |
+--------------------+----------+------------------------------------------------+
| linger.ms          | 0        | 배치 대기 시간. 0이면 즉시 전송.                |
|                    |          | 5~100ms 설정 시 배치 효율 크게 향상             |
+--------------------+----------+------------------------------------------------+
| buffer.memory      | 33554432 | 전체 버퍼 메모리 (32MB).                        |
|                    |          | 프로듀서 처리량에 따라 조정                      |
+--------------------+----------+------------------------------------------------+
| acks               | all      | all(-1): ISR 전체 확인. 가장 안전.              |
|                    |          | 1: 리더만 확인. 0: 확인 없음(최고 속도)         |
+--------------------+----------+------------------------------------------------+
| compression.type   | none     | 압축. lz4/snappy: 빠름. zstd: 높은 압축률       |
|                    |          | 네트워크/디스크 절약 vs CPU 사용                 |
+--------------------+----------+------------------------------------------------+
| max.in.flight.req  | 5        | 연결당 최대 in-flight 요청.                     |
|                    |per.conn  | 1: 순서 보장 (성능 저하)                        |
|                    |          | 5: 기본값, 멱등성 사용 시 순서 보장 가능         |
+--------------------+----------+------------------------------------------------+
| delivery.timeout.ms| 120000   | 전송 타임아웃 (2분).                            |
|                    |          | >= linger.ms + request.timeout.ms               |
+--------------------+----------+------------------------------------------------+
| enable.idempotence | true     | 멱등성 활성화. Kafka 3.0+에서 기본 활성          |
+--------------------+----------+------------------------------------------------+
```

### 12.2 프로듀서 설정 상호작용

```
설정 간 관계:

delivery.timeout.ms >= linger.ms + request.timeout.ms
       |                   |              |
       |                   |              +-- 단일 요청 타임아웃
       |                   +-- 배치 대기 시간
       +-- 전체 전송 타임아웃 (재시도 포함)

batch.size + linger.ms:
  - batch.size 크고 linger.ms 작음: 빠른 전송, 작은 배치
  - batch.size 크고 linger.ms 큼:  느린 전송, 큰 배치 (고처리량)
  - batch.size 작고 linger.ms 큼:  느린 전송, 작은 배치 (비효율)

max.in.flight.requests.per.connection:
  - 멱등성 OFF + >1: 순서 보장 없음 (재전송 시 역전 가능)
  - 멱등성 ON  + <=5: 순서 보장 (시퀀스 번호로 검증)
  - 멱등성 ON  + >5: 지원 안됨 (예외 발생)
```

### 12.3 컨슈머 핵심 설정

```
+------------------------+----------+-------------------------------------------+
| 설정                   | 기본값   | 설명 및 튜닝 가이드                        |
+------------------------+----------+-------------------------------------------+
| fetch.min.bytes        | 1        | 최소 fetch 크기. 클수록 대기 시간 증가,     |
|                        |          | 배치 효율 향상. 1KB~1MB                    |
+------------------------+----------+-------------------------------------------+
| fetch.max.bytes        | 52428800 | 최대 fetch 크기 (50MB).                    |
|                        |          | 대용량 메시지 시 증가                       |
+------------------------+----------+-------------------------------------------+
| fetch.max.wait.ms      | 500      | 최대 fetch 대기 (500ms).                   |
|                        |          | fetch.min.bytes와 함께 작용                 |
+------------------------+----------+-------------------------------------------+
| max.poll.records       | 500      | poll() 당 최대 레코드 수.                   |
|                        |          | 처리 시간 고려하여 조정                     |
+------------------------+----------+-------------------------------------------+
| max.poll.interval.ms   | 300000   | poll() 간 최대 간격 (5분).                  |
|                        |          | 초과 시 그룹에서 제거됨                     |
+------------------------+----------+-------------------------------------------+
| session.timeout.ms     | 45000    | 세션 타임아웃 (45초).                       |
|                        |          | 하트비트 실패 시 그룹에서 제거              |
+------------------------+----------+-------------------------------------------+
| heartbeat.interval.ms  | 3000     | 하트비트 간격 (3초).                        |
|                        |          | session.timeout.ms의 1/3 이하 권장          |
+------------------------+----------+-------------------------------------------+
| auto.offset.reset      | latest   | 오프셋 없을 때: latest/earliest/none        |
+------------------------+----------+-------------------------------------------+
| enable.auto.commit     | true     | 자동 오프셋 커밋.                           |
|                        |          | false 설정 시 수동 커밋 필요                |
+------------------------+----------+-------------------------------------------+
| auto.commit.interval.ms| 5000     | 자동 커밋 간격 (5초).                       |
+------------------------+----------+-------------------------------------------+
```

### 12.4 컨슈머 설정 상호작용

```
설정 간 관계:

session.timeout.ms > heartbeat.interval.ms * 3 (권장)
       |                    |
       |                    +-- 하트비트 전송 빈도
       +-- 멤버 탈퇴 감지 시간

max.poll.interval.ms:
  - 레코드 처리 시간이 이 값을 초과하면 리밸런스 발생
  - max.poll.records를 줄이면 처리 시간 감소
  - 무거운 처리: max.poll.records 줄이거나
    max.poll.interval.ms 늘리기

fetch.min.bytes + fetch.max.wait.ms:
  - 지연 시간 최소화: fetch.min.bytes=1, fetch.max.wait.ms=100
  - 처리량 최대화: fetch.min.bytes=1MB, fetch.max.wait.ms=500
  - fetch.min.bytes 충족되면 fetch.max.wait.ms 무시
```

### 12.5 고처리량 설정 프로필

```
// 프로듀서 고처리량 설정
Properties props = new Properties();
props.put("batch.size", 65536);            // 64KB 배치
props.put("linger.ms", 20);               // 20ms 대기
props.put("buffer.memory", 67108864);      // 64MB 버퍼
props.put("compression.type", "lz4");      // LZ4 압축
props.put("acks", "all");                  // 안전한 기본값 유지

// 컨슈머 고처리량 설정
props.put("fetch.min.bytes", 1048576);     // 1MB 최소 fetch
props.put("fetch.max.wait.ms", 500);       // 500ms 최대 대기
props.put("max.poll.records", 1000);       // poll당 1000 레코드
```

### 12.6 저지연 설정 프로필

```
// 프로듀서 저지연 설정
Properties props = new Properties();
props.put("batch.size", 16384);            // 기본 16KB
props.put("linger.ms", 0);                // 즉시 전송
props.put("acks", "1");                   // 리더만 확인
props.put("compression.type", "none");     // 압축 없음

// 컨슈머 저지연 설정
props.put("fetch.min.bytes", 1);           // 즉시 fetch
props.put("fetch.max.wait.ms", 100);       // 100ms 최대 대기
props.put("max.poll.records", 100);        // 빠른 처리
```

---

## 요약

```
Kafka 클라이언트 아키텍처 요약:

[프로듀서]
User Thread --> send() --> doSend()
  |-- serialize --> partition --> accumulator.append()
  |
Sender Thread --> runOnce()
  |-- sendProducerData() --> drain() --> sendProduceRequests()
  |-- client.poll() --> 응답 처리 --> 콜백 실행

핵심 설계 원칙:
1. 비동기 전송: send()는 즉시 반환, Sender가 배경에서 전송
2. 배치 처리: RecordAccumulator로 레코드를 배치로 묶어 효율 증대
3. Sticky Partitioning: 하나의 파티션에 집중하여 배치 크기 극대화
4. 멱등성: PID + Epoch + Sequence로 중복 방지

[컨슈머]
User Thread --> poll()
  |-- coordinator.poll() --> 그룹 관리
  |-- fetcher.sendFetches() --> 데이터 요청
  |-- client.poll() --> 네트워크 I/O
  |-- fetcher.collectFetch() --> 레코드 수집

핵심 설계 원칙:
1. Pull 모델: 컨슈머가 자신의 속도에 맞춰 데이터 가져옴
2. 그룹 코디네이션: JoinGroup/SyncGroup으로 파티션 할당
3. FetchSession: 반복 요청 시 incremental fetch로 효율 증대
4. 오프셋 관리: __consumer_offsets에 진행 상황 저장
```
