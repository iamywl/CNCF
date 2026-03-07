# 13. 트랜잭션 시스템 Deep-Dive

## 목차

1. [개요](#1-개요)
2. [트랜잭션 프로토콜 전체 흐름](#2-트랜잭션-프로토콜-전체-흐름)
3. [TransactionCoordinator](#3-transactioncoordinator)
4. [TransactionMetadata와 상태 머신](#4-transactionmetadata와-상태-머신)
5. [2PC: Two-Phase Commit](#5-2pc-two-phase-commit)
6. [EndTransactionMarker](#6-endtransactionmarker)
7. [Idempotent Producer 메커니즘](#7-idempotent-producer-메커니즘)
8. [ProducerIdManager](#8-produceridmanager)
9. [프로듀서 펜싱](#9-프로듀서-펜싱)
10. [Read Committed 격리 수준](#10-read-committed-격리-수준)
11. [__transaction_state 토픽](#11-__transaction_state-토픽)
12. [설계 결정: Why?](#12-설계-결정-why)

---

## 1. 개요

Kafka의 트랜잭션 시스템은 **Exactly-Once Semantics (EOS)**를 구현하는 핵심 인프라이다.
프로듀서가 여러 토픽/파티션에 원자적(atomic)으로 메시지를 전송할 수 있게 해준다.
트랜잭션이 커밋되면 모든 메시지가 보이고, 중단(abort)되면 모든 메시지가 보이지 않는다.

### 소스 위치

| 컴포넌트 | 소스 경로 |
|----------|----------|
| TransactionCoordinator (Scala) | `core/src/main/scala/kafka/coordinator/transaction/TransactionCoordinator.scala` |
| TransactionStateManager | `core/src/main/scala/kafka/coordinator/transaction/TransactionStateManager.scala` |
| TransactionMarkerChannelManager | `core/src/main/scala/kafka/coordinator/transaction/TransactionMarkerChannelManager.scala` |
| TransactionMetadata (Java) | `transaction-coordinator/src/main/java/org/apache/kafka/coordinator/transaction/TransactionMetadata.java` |
| TransactionState (Java) | `transaction-coordinator/src/main/java/org/apache/kafka/coordinator/transaction/TransactionState.java` |
| TransactionLog (Java) | `transaction-coordinator/src/main/java/org/apache/kafka/coordinator/transaction/TransactionLog.java` |
| ProducerIdManager (Java) | `transaction-coordinator/src/main/java/org/apache/kafka/coordinator/transaction/ProducerIdManager.java` |
| RPCProducerIdManager | `transaction-coordinator/src/main/java/org/apache/kafka/coordinator/transaction/RPCProducerIdManager.java` |
| TransactionManager (클라이언트) | `clients/src/main/java/org/apache/kafka/clients/producer/internals/TransactionManager.java` |

---

## 2. 트랜잭션 프로토콜 전체 흐름

### 2.1 전체 시퀀스

```
트랜잭션 전체 흐름:

Producer                 TransactionCoordinator        Data Brokers
   |                            |                          |
   |  1. InitProducerId         |                          |
   |-- InitProducerIdReq ------>|                          |
   |   (transactionalId)        |-- __transaction_state    |
   |                            |   에 EMPTY 상태 기록     |
   |<-- PID, Epoch -------------|                          |
   |                            |                          |
   |  2. BeginTransaction       |                          |
   |   (클라이언트 내부)         |                          |
   |                            |                          |
   |  3. AddPartitionsToTxn     |                          |
   |-- AddPartitionsToTxnReq -->|                          |
   |   (txnId, PID, [P0, P1])  |-- __transaction_state    |
   |                            |   에 ONGOING + 파티션 기록|
   |<-- OK --------------------|                          |
   |                            |                          |
   |  4. Produce                |                          |
   |-- ProduceReq (PID, Epoch, Seq) ------------------>   |
   |   데이터 브로커에 직접 전송  |                   기록   |
   |<-- OK -------------------------------------------|   |
   |                            |                          |
   |  5. EndTxn (COMMIT)        |                          |
   |-- EndTxnReq (COMMIT) ----->|                          |
   |                            |-- Phase 1: PREPARE_COMMIT|
   |                            |   __transaction_state 기록|
   |                            |                          |
   |                            |-- Phase 2: WriteTxnMarkers
   |                            |-- WriteTxnMarkersReq --->|
   |                            |   COMMIT 마커 기록        |
   |                            |<-- OK ------------------|
   |                            |                          |
   |                            |-- COMPLETE_COMMIT 기록   |
   |<-- OK --------------------|                          |
```

### 2.2 코드 레벨 흐름

```java
// 프로듀서 사용 예시 (KafkaProducer.java 라인 198-220)
Properties props = new Properties();
props.put("bootstrap.servers", "localhost:9092");
props.put("transactional.id", "my-transactional-id");
Producer<String, String> producer = new KafkaProducer<>(props, ...);

producer.initTransactions();      // 1. InitProducerId

try {
    producer.beginTransaction();  // 2. 클라이언트 상태 변경
    for (int i = 0; i < 100; i++)
        producer.send(new ProducerRecord<>("topic", ...));
                                  // 3. AddPartitionsToTxn + Produce
    producer.commitTransaction(); // 5. EndTxn(COMMIT)
} catch (ProducerFencedException | OutOfOrderSequenceException e) {
    producer.close();             // 복구 불가능한 에러
} catch (KafkaException e) {
    producer.abortTransaction();  // 5. EndTxn(ABORT)
}
```

---

## 3. TransactionCoordinator

### 3.1 역할

`TransactionCoordinator.scala` (라인 84-98)는 트랜잭션의 2PC 코디네이터이다.

```
TransactionCoordinator 구성:
+-----------------------------------------------+
| TransactionCoordinator                         |
|                                                |
|  txnConfig: TransactionConfig                  |
|  |-- transactionalIdExpirationMs: 604800000    |
|  |-- transactionMaxTimeoutMs: 900000           |
|  |-- transactionTopicPartitions: 50            |
|                                                |
|  txnManager: TransactionStateManager           |
|  |-- 트랜잭션 상태 저장소                       |
|  |-- __transaction_state 토픽 관리              |
|                                                |
|  txnMarkerChannelManager:                      |
|    TransactionMarkerChannelManager             |
|  |-- 데이터 브로커에 트랜잭션 마커 전송         |
|                                                |
|  producerIdManager: ProducerIdManager          |
|  |-- 고유한 ProducerId 할당                    |
+-----------------------------------------------+
```

### 3.2 핵심 메서드

```
TransactionCoordinator 핵심 메서드:

handleInitProducerId(transactionalId, timeout, ...)
  |-- 새 트랜잭션: PID 할당, EMPTY 상태 생성
  |-- 기존 트랜잭션: 에포크 범프, 이전 트랜잭션 정리
  |-- 진행 중 트랜잭션: PREPARE_EPOCH_FENCE로 전환

handleAddPartitionsToTxn(transactionalId, PID, epoch, partitions)
  |-- ONGOING 상태 확인
  |-- 새 파티션을 topicPartitions에 추가
  |-- __transaction_state에 기록

handleEndTransaction(transactionalId, PID, epoch, COMMIT/ABORT)
  |-- Phase 1: PREPARE_COMMIT 또는 PREPARE_ABORT
  |-- Phase 2: WriteTxnMarkers 전송
  |-- 완료: COMPLETE_COMMIT 또는 COMPLETE_ABORT
```

### 3.3 handleInitProducerId 상세

`TransactionCoordinator.scala` (라인 114-222):

```
handleInitProducerId() 처리 흐름:

입력: transactionalId, transactionTimeoutMs

1. transactionalId == null?
   |-- YES -> PID만 할당 (멱등성 전용)
   |-- NO  -> 계속

2. transactionalId == ""?
   |-- YES -> INVALID_REQUEST 에러

3. 기존 TransactionMetadata 조회
   |-- NONE -> 새 메타데이터 생성
   |   |-- PID 할당 (ProducerIdManager)
   |   |-- 상태: EMPTY
   |   |-- __transaction_state에 기록
   |
   |-- EXISTS -> 상태에 따라 처리
       |
       |-- PREPARE_ABORT, PREPARE_COMMIT:
       |   -> CONCURRENT_TRANSACTIONS (재시도 필요)
       |
       |-- COMPLETE_ABORT, COMPLETE_COMMIT, EMPTY:
       |   -> 에포크 범프
       |   -> 에포크 소진 시 새 PID 할당
       |   -> __transaction_state에 기록
       |
       |-- ONGOING:
       |   -> PREPARE_EPOCH_FENCE
       |   -> 기존 트랜잭션 abort
       |   -> CONCURRENT_TRANSACTIONS 반환
       |   -> 클라이언트 재시도 시 정상 처리
       |
       |-- DEAD, PREPARE_EPOCH_FENCE:
       |   -> IllegalStateException (발생하면 안됨)
```

---

## 4. TransactionMetadata와 상태 머신

### 4.1 TransactionMetadata 구조

```java
// TransactionMetadata.java (라인 37-62)
public class TransactionMetadata {
    private final String transactionalId;
    private long producerId;           // 현재 PID
    private long prevProducerId;       // 이전 PID (커밋 완료된)
    private long nextProducerId;       // 다음 PID (에포크 소진 시)
    private short producerEpoch;       // 현재 에포크
    private short lastProducerEpoch;   // 이전 에포크
    private int txnTimeoutMs;          // 트랜잭션 타임아웃
    private TransactionState state;    // 현재 상태
    private HashSet<TopicPartition> topicPartitions;  // 참여 파티션
    private volatile long txnStartTimestamp;           // 시작 시간
    private volatile long txnLastUpdateTimestamp;      // 최근 업데이트
    private TransactionVersion clientTransactionVersion;

    private Optional<TransactionState> pendingState;   // 전이 중 상태
    private boolean hasFailedEpochFence;               // 에포크 펜스 실패 여부
    private final ReentrantLock lock;                  // 동시성 제어
}
```

### 4.2 상태 머신

`TransactionState.java` (라인 31-103)에 정의된 상태:

```
트랜잭션 상태 머신:

+----------+     AddPartitions     +----------+
|          | ------------------->  |          |
|  EMPTY   |     또는 AddOffsets   | ONGOING  |
|  (0)     | <-------------------  |  (1)     |
+----------+                       +----+-----+
     ^  ^                               |   |
     |  |                    EndTxn     |   |
     |  |                   (commit)    |   | EndTxn
     |  |                      |        |   | (abort)
     |  |                      v        |   v
     |  |              +-------+---+    |  +------+------+
     |  |              | PREPARE_  |    |  | PREPARE_    |
     |  |              | COMMIT    |    |  | ABORT       |
     |  |              |   (2)     |    |  |   (3)       |
     |  |              +-----+-----+    |  +------+------+
     |  |                    |          |         |
     |  |  acks 수신         |          |  acks 수신
     |  |                    v          |         v
     |  |           +--------+---+     |  +------+------+
     |  +-----------|  COMPLETE_ |     |  | COMPLETE_   |
     |              |  COMMIT(4) |     |  | ABORT (5)   |
     |              +------------+     |  +------+------+
     |                                 |         |
     +--<-----------------------------<+---------+

+------------------------------------------+
|  DEAD (6)    - 만료된 transactionalId    |
|  PREPARE_EPOCH_FENCE (7) - 에포크 펜싱 중 |
+------------------------------------------+
```

### 4.3 상태 전이 규칙

```java
// TransactionState.java (라인 94-103)
public static final Map<TransactionState, Set<TransactionState>>
    VALID_PREVIOUS_STATES = Map.of(
    EMPTY,              Set.of(EMPTY, COMPLETE_COMMIT, COMPLETE_ABORT),
    ONGOING,            Set.of(ONGOING, EMPTY, COMPLETE_COMMIT, COMPLETE_ABORT),
    PREPARE_COMMIT,     Set.of(ONGOING),
    PREPARE_ABORT,      Set.of(ONGOING, PREPARE_EPOCH_FENCE,
                               EMPTY, COMPLETE_COMMIT, COMPLETE_ABORT),
    COMPLETE_COMMIT,    Set.of(PREPARE_COMMIT),
    COMPLETE_ABORT,     Set.of(PREPARE_ABORT),
    DEAD,               Set.of(EMPTY, COMPLETE_ABORT, COMPLETE_COMMIT),
    PREPARE_EPOCH_FENCE,Set.of(ONGOING)
);
```

```
상태 전이 테이블:

현재 상태           | 가능한 다음 상태
--------------------|----------------------------------------
EMPTY               | ONGOING, DEAD, PREPARE_ABORT(TV2)
ONGOING             | PREPARE_COMMIT, PREPARE_ABORT,
                    | PREPARE_EPOCH_FENCE, ONGOING(파티션 추가)
PREPARE_COMMIT      | COMPLETE_COMMIT
PREPARE_ABORT       | COMPLETE_ABORT
COMPLETE_COMMIT     | EMPTY, ONGOING, DEAD
COMPLETE_ABORT      | EMPTY, ONGOING, DEAD
DEAD                | (최종 상태)
PREPARE_EPOCH_FENCE | PREPARE_ABORT
```

### 4.4 pendingState 메커니즘

```
pendingState의 역할:

상태 전이는 두 단계로 진행:
1. pendingState 설정 (전이 시작)
2. pendingState를 state로 확정 (레코드 기록 완료)

예: ONGOING -> PREPARE_COMMIT

단계 1:
  state = ONGOING
  pendingState = Optional.of(PREPARE_COMMIT)
  -> 이 시점에서 다른 상태 전이 시도 차단

단계 2 (기록 성공):
  state = PREPARE_COMMIT
  pendingState = Optional.empty()

단계 2 (기록 실패):
  state = ONGOING  (원래 상태 유지)
  pendingState = Optional.empty()

이 메커니즘은 __transaction_state 기록이 완료되기 전에
다른 요청이 상태를 변경하는 것을 방지한다.
```

---

## 5. 2PC: Two-Phase Commit

### 5.1 Phase 1: Prepare

```
Phase 1 - Prepare:

endTransaction(COMMIT) 요청 수신
    |
    v
TransactionCoordinator.endTransaction()
    |
    v
1. 현재 상태 확인: ONGOING이어야 함
    |
    v
2. prepareCommitOrAbort()
    |-- ONGOING -> PREPARE_COMMIT
    |-- TransitMetadata 생성
    |
    v
3. __transaction_state에 PREPARE_COMMIT 기록
    |
    +-- 레코드:
    |   Key: transactionalId
    |   Value: {
    |     PID, epoch, state: PREPARE_COMMIT,
    |     topicPartitions: [t1-P0, t1-P1, t2-P0],
    |     txnStartTimestamp, txnLastUpdateTimestamp
    |   }
    |
    v
4. 기록 성공 -> Phase 2 시작
   기록 실패 -> 상태 롤백, 에러 반환
```

**왜 Phase 1이 필요한가?** PREPARE_COMMIT을 __transaction_state에 기록하면,
코디네이터가 장애 후 복구되더라도 트랜잭션을 커밋해야 한다는 결정이 영구적으로
보존된다. 이는 2PC의 "결정 지점(decision point)"이다.

### 5.2 Phase 2: WriteTxnMarkers

```
Phase 2 - WriteTxnMarkers:

TransactionCoordinator
    |
    v
TransactionMarkerChannelManager
    |
    |-- 각 파티션의 리더 브로커에 WriteTxnMarkersRequest 전송
    |
    +-- Broker-0 (리더: t1-P0, t1-P1)
    |   WriteTxnMarkersRequest {
    |     PID, epoch,
    |     coordinatorEpoch,
    |     result: COMMIT,
    |     topics: [t1: [P0, P1]]
    |   }
    |   -> t1-P0에 COMMIT 컨트롤 레코드 기록
    |   -> t1-P1에 COMMIT 컨트롤 레코드 기록
    |
    +-- Broker-1 (리더: t2-P0)
    |   WriteTxnMarkersRequest {
    |     PID, epoch,
    |     result: COMMIT,
    |     topics: [t2: [P0]]
    |   }
    |   -> t2-P0에 COMMIT 컨트롤 레코드 기록
    |
    +-- GroupCoordinator (오프셋이 포함된 경우)
        WriteTxnMarkersRequest {
          PID, epoch,
          result: COMMIT,
          topics: [__consumer_offsets: [P12]]
        }
        -> 트랜잭셔널 오프셋 확정
```

```
Phase 2 완료 후:

모든 마커 기록 성공
    |
    v
__transaction_state에 COMPLETE_COMMIT 기록
    |
    v
메타데이터 정리:
  - topicPartitions 초기화
  - txnStartTimestamp 리셋
  - state = COMPLETE_COMMIT
```

### 5.3 장애 복구

```
2PC 장애 복구 시나리오:

Case 1: Phase 1 전 장애 (ONGOING)
  -> 코디네이터 복구 시 txnTimeout 초과 여부 확인
  -> 초과: 자동 ABORT
  -> 미초과: 클라이언트가 다시 EndTxn 전송

Case 2: Phase 1 후, Phase 2 전 장애 (PREPARE_COMMIT)
  -> 코디네이터 복구 시 __transaction_state에서 로드
  -> PREPARE_COMMIT 확인 -> Phase 2 재개
  -> 모든 마커 전송 완료 -> COMPLETE_COMMIT

Case 3: Phase 2 부분 완료 후 장애
  -> 일부 마커만 기록된 상태
  -> 코디네이터 복구 후 나머지 마커 재전송
  -> WriteTxnMarkers는 멱등적 (이미 기록된 마커 무시)

Case 4: Phase 1 후, Phase 2 전 장애 (PREPARE_ABORT)
  -> PREPARE_COMMIT과 동일하게 처리
  -> ABORT 마커 전송 -> COMPLETE_ABORT
```

**왜 2PC인가?** 단일 데이터 소스가 아닌 여러 파티션에 원자적으로 쓰기를 보장하려면
분산 합의가 필요하다. Kafka의 2PC는 전통적인 2PC와 다르게 코디네이터가
__transaction_state 토픽의 복제를 활용하여 단일 장애점을 제거한다.

---

## 6. EndTransactionMarker

### 6.1 컨트롤 레코드

트랜잭션 마커는 데이터 레코드와 함께 파티션 로그에 기록되는 **컨트롤 레코드(Control Record)**이다.

```
파티션 로그 내 컨트롤 레코드:

Partition 0의 로그:
+--------+--------+--------+--------+--------+--------+--------+
| offset | 0      | 1      | 2      | 3      | 4      | 5      |
|--------|--------|--------|--------|--------|--------|--------|
| type   | DATA   | DATA   | DATA   | CTRL   | DATA   | DATA   |
| PID    | 42     | 42     | 42     | 42     | 99     | 99     |
| epoch  | 0      | 0      | 0      | 0      | 1      | 1      |
| data   | msg-A  | msg-B  | msg-C  | COMMIT | msg-D  | msg-E  |
+--------+--------+--------+--------+--------+--------+--------+
                                ^
                                |
                         트랜잭션 마커
                         (PID=42의 트랜잭션 커밋)
```

### 6.2 마커 유형

```
EndTransactionMarker 유형:

1. COMMIT 마커:
   - 해당 PID/epoch의 모든 메시지가 유효
   - Read Committed 컨슈머에게 보임

2. ABORT 마커:
   - 해당 PID/epoch의 모든 메시지가 무효
   - Read Committed 컨슈머에게 숨김
   - 메시지 자체는 로그에 남음 (공간 차지)
   - 로그 압축/만료 시 정리

마커 레코드 구조:
  ControlRecordType: COMMIT(0) 또는 ABORT(1)
  CoordinatorEpoch: 트랜잭션 코디네이터의 에포크
```

### 6.3 마커와 오프셋의 관계

```
ABORT된 트랜잭션의 메시지 처리:

Partition 0:
  offset 0: [PID=42] msg-A  (TXN 시작)
  offset 1: [PID=42] msg-B
  offset 2: [PID=99] msg-C  (다른 프로듀서, 비트랜잭션)
  offset 3: [PID=42] msg-D
  offset 4: [PID=42] ABORT  (TXN 중단)
  offset 5: [PID=99] msg-E

Read Committed 컨슈머가 보는 것:
  offset 2: msg-C  (비트랜잭션, 항상 보임)
  offset 5: msg-E  (비트랜잭션, 항상 보임)
  => PID=42의 msg-A, B, D는 숨겨짐

Read Uncommitted 컨슈머가 보는 것:
  offset 0: msg-A
  offset 1: msg-B
  offset 2: msg-C
  offset 3: msg-D
  offset 5: msg-E
  => 모든 데이터 메시지 보임 (마커 제외)
```

---

## 7. Idempotent Producer 메커니즘

### 7.1 PID + Epoch + Sequence 트리플

멱등성 프로듀서는 트랜잭션의 기초이다. 모든 트랜잭셔널 프로듀서는 멱등성을 포함한다.

```
멱등성 메커니즘 구조:

ProducerId (PID): 64비트 정수
  - 전역적으로 고유한 프로듀서 식별자
  - 컨트롤러에서 블록 단위로 할당

ProducerEpoch: 16비트 정수 (0 ~ 32767)
  - PID 내에서 "세대"를 구분
  - 같은 PID로 여러 세션 구분

SequenceNumber: 32비트 정수 (파티션별)
  - 파티션 내에서 배치 단위 증가
  - 0부터 시작, 각 배치마다 +레코드수

+--------------------------------------------------+
| 유니크 식별:                                      |
|   (PID=42, Epoch=0, Partition=3, Seq=15)         |
|   -> 이 조합은 전체 클러스터에서 유일             |
|   -> 같은 조합의 두 번째 전송 = 중복              |
+--------------------------------------------------+
```

### 7.2 브로커 측 중복 감지

```
브로커의 ProducerStateManager:

파티션 P0의 프로듀서 상태:
+--------------------------------------------+
| PID=42, Epoch=0:                           |
|   lastSequence: 14                         |
|   lastOffset: 1234                         |
|   batches: [                               |
|     {seq:10, offset:1230, timestamp:...},  |
|     {seq:12, offset:1232, timestamp:...},  |
|     {seq:14, offset:1234, timestamp:...}   |
|   ]                                        |
+--------------------------------------------+
| PID=99, Epoch=1:                           |
|   lastSequence: 3                          |
|   lastOffset: 1235                         |
|   ...                                      |
+--------------------------------------------+

수신된 배치 검증:
  - seq == lastSequence + 1: 정상 (연속)
  - seq <= lastSequence: DuplicateSequenceException
  - seq > lastSequence + 1: OutOfOrderSequenceException
```

### 7.3 시퀀스 래핑

```
시퀀스 번호 래핑:

시퀀스는 32비트 정수 (int):
  0, 1, 2, ..., 2147483647, 0, 1, 2, ...
  (Integer.MAX_VALUE 이후 0으로 순환)

브로커는 래핑을 인식:
  lastSequence = 2147483646
  newSequence = 2147483647 -> 정상 (연속)
  nextSequence = 0 -> 정상 (래핑)
  nextSequence = 5 -> OutOfOrderSequence (건너뜀)
```

---

## 8. ProducerIdManager

### 8.1 PID 할당 메커니즘

```java
// ProducerIdManager.java (라인 32-42)
public interface ProducerIdManager {
    long generateProducerId() throws Exception;

    static ProducerIdManager rpc(int brokerId,
            Time time,
            Supplier<Long> brokerEpochSupplier,
            NodeToControllerChannelManager controllerChannel) {
        return new RPCProducerIdManager(
            brokerId, time, brokerEpochSupplier, controllerChannel);
    }
}
```

### 8.2 블록 할당 방식

```
PID 블록 할당:

Controller
+---------------------------------------------------+
| 전역 PID 카운터: nextProducerId = 0               |
|                                                   |
| Broker-0 요청: 1000개 블록                        |
| -> [0, 999] 할당, nextProducerId = 1000           |
|                                                   |
| Broker-1 요청: 1000개 블록                        |
| -> [1000, 1999] 할당, nextProducerId = 2000       |
|                                                   |
| Broker-0 블록 소진, 재요청:                       |
| -> [2000, 2999] 할당, nextProducerId = 3000       |
+---------------------------------------------------+

RPCProducerIdManager (각 브로커):
+---------------------------------------------------+
| currentBlock: [2000, 2999]                        |
| nextId: 2042                                      |
|                                                   |
| generateProducerId():                              |
|   if (nextId >= blockEnd) 새 블록 요청             |
|   return nextId++                                 |
+---------------------------------------------------+
```

**왜 블록 할당인가?** 매 PID 할당마다 컨트롤러에 RPC를 보내면 병목이 된다.
블록 단위로 미리 할당받으면, 대부분의 PID 할당이 로컬에서 처리된다.
블록 크기(기본 1000)는 브로커 재시작 시 약간의 PID 낭비를 감수한다.

### 8.3 에포크 소진과 PID 회전

```
에포크 소진 시나리오:

PID=42, Epoch:
  0, 1, 2, ..., 32766, 32767 (Short.MAX_VALUE)
  -> 에포크 소진! 더 이상 범프 불가

해결: PID 회전 (Rotation)
  1. 새 PID 할당 (예: PID=99)
  2. 이전 PID 기록: prevProducerId = 42
  3. 새 PID로 에포크 0부터 시작

코드 (TransactionMetadata.java):
  isEpochExhausted(epoch) = epoch >= Short.MAX_VALUE - 1

TransactionCoordinator.scala (라인 261-265):
  if (txnMetadata.isProducerEpochExhausted)
    txnMetadata.prepareProducerIdRotation(
      producerIdManager.generateProducerId(), ...)
  else
    txnMetadata.prepareIncrementProducerEpoch(...)
```

---

## 9. 프로듀서 펜싱

### 9.1 좀비 프로듀서 문제

```
좀비 프로듀서 시나리오:

시간 T1: Producer-A (txnId="txn-1", PID=42, Epoch=0) 정상 운영
         |-- 트랜잭션 시작
         |-- 메시지 전송 (P0, P1)
         |-- 네트워크 단절! (좀비 상태)

시간 T2: Producer-B (txnId="txn-1") 시작
         |-- InitProducerId()
         |-- PID=42, Epoch=1 할당
         |-- 새 트랜잭션 시작

시간 T3: Producer-A 네트워크 복구, 전송 재시도
         |-- Produce(PID=42, Epoch=0, ...)
         |-- 브로커: Epoch=0 < 현재 Epoch=1
         |-- -> PRODUCER_FENCED 에러
         |-- Producer-A 차단됨!
```

### 9.2 PREPARE_EPOCH_FENCE

```
에포크 펜싱 프로세스:

Producer-B가 InitProducerId(txnId="txn-1") 요청
    |
    v
TransactionCoordinator:
  기존 메타데이터 조회: PID=42, Epoch=0, State=ONGOING
    |
    v
  prepareFenceProducerEpoch():
    |-- state: ONGOING -> PREPARE_EPOCH_FENCE
    |-- epoch: 0 -> 1 (범프)
    |-- __transaction_state에 기록
    |
    v
  기존 트랜잭션 abort 시작:
    |-- endTransaction(txnId, PID=42, Epoch=1, ABORT)
    |-- Phase 1: PREPARE_ABORT
    |-- Phase 2: WriteTxnMarkers(ABORT)
    |-- COMPLETE_ABORT
    |
    v
  Producer-B에게 응답:
    |-- CONCURRENT_TRANSACTIONS (재시도 필요)
    |
  Producer-B 재시도 -> 이번에는 상태가 COMPLETE_ABORT
    |-- 에포크 범프: PID=42, Epoch=2
    |-- 정상 응답: PID=42, Epoch=2
```

### 9.3 펜싱이 적용되는 지점

```
펜싱 검증 지점:

1. Produce 요청 시 (데이터 브로커):
   +----------------------------------------------+
   | 수신된 Epoch < 브로커가 아는 Epoch            |
   | -> PRODUCER_FENCED (또는 INVALID_PRODUCER_EPOCH)|
   +----------------------------------------------+

2. AddPartitionsToTxn 요청 시:
   +----------------------------------------------+
   | 수신된 Epoch < TransactionMetadata의 Epoch    |
   | -> PRODUCER_FENCED                            |
   +----------------------------------------------+

3. EndTxn 요청 시:
   +----------------------------------------------+
   | 수신된 Epoch < TransactionMetadata의 Epoch    |
   | -> PRODUCER_FENCED                            |
   +----------------------------------------------+

4. TxnOffsetCommit 요청 시:
   +----------------------------------------------+
   | 수신된 Epoch < 그룹 코디네이터가 아는 Epoch    |
   | -> PRODUCER_FENCED                            |
   +----------------------------------------------+
```

---

## 10. Read Committed 격리 수준

### 10.1 Last Stable Offset (LSO)

```
Last Stable Offset 개념:

파티션 로그:
offset:  0   1   2   3   4   5   6   7   8   9
PID:    42  42  99  42  99  42  99  42  99  99
txn:    T1  T1  --  T1  --  T1  --  T1  --  --
marker:                              COMMIT

LSO = 가장 오래된 진행 중 트랜잭션의 첫 오프셋
    = 모든 트랜잭션이 완료된 경우: HW (High Watermark)

예시: PID=42 트랜잭션이 커밋 전이면:
  LSO = 0 (PID=42의 첫 오프셋)
  -> Read Committed 컨슈머는 offset 0 이후 읽지 못함

예시: PID=42 트랜잭션 커밋 후:
  LSO = HW (진행 중 트랜잭션 없음)
  -> Read Committed 컨슈머는 모든 커밋된 오프셋 읽기 가능
```

### 10.2 Read Committed 동작

```
Read Committed 컨슈머의 동작:

1. FetchRequest에 isolation_level=READ_COMMITTED 설정

2. 브로커 응답에 포함되는 정보:
   - 데이터 레코드 (LSO까지만)
   - abortedTransactions: [{PID, firstOffset}]
     (ABORT된 트랜잭션 목록)

3. 컨슈머의 필터링:
   for each record in response:
     if record.PID in abortedTransactions
       and record.offset >= abortedTxn.firstOffset
       and record.offset <= ABORT_marker.offset:
         -> 건너뜀 (ABORT된 메시지)
     else:
         -> 사용자에게 전달

+------------------------------------------------+
| 결과:                                           |
| - COMMIT된 트랜잭션의 메시지: 전달              |
| - ABORT된 트랜잭션의 메시지: 숨김               |
| - 비트랜잭션 메시지: 항상 전달                   |
| - 진행 중 트랜잭션의 메시지: LSO로 차단          |
+------------------------------------------------+
```

### 10.3 LSO와 Consumer Lag

```
LSO의 Consumer Lag 영향:

시나리오: 느린 트랜잭션

시간 T1: PID=42 트랜잭션 시작 (offset 100)
시간 T2: 다른 프로듀서들이 offset 100000까지 기록
시간 T3: PID=42 아직 진행 중

LSO = 100 (PID=42의 첫 오프셋)
HW  = 100000

Read Committed 컨슈머:
  -> offset 100까지만 읽을 수 있음
  -> 99900 레코드의 lag 발생

해결: transaction.max.timeout.ms (기본 15분)
  -> 타임아웃 초과 시 코디네이터가 자동 ABORT
  -> LSO 전진

교훈: 트랜잭션은 짧게 유지해야 함!
```

---

## 11. __transaction_state 토픽

### 11.1 토픽 구조

```
__transaction_state 토픽:
+--------------------------------------------------+
| 기본 설정:                                        |
|   파티션 수: 50                                   |
|   복제 인수: 3                                    |
|   정리 정책: compact                              |
|   min.insync.replicas: 2                          |
|   segment.bytes: 100MB                            |
+--------------------------------------------------+

파티셔닝:
  hash(transactionalId) % 50 = 파티션 번호
  -> 해당 파티션의 리더 = TransactionCoordinator

예:
  "txn-orders" -> hash % 50 = 17
  -> __transaction_state-17의 리더가 코디네이터
```

### 11.2 레코드 포맷

```
TransactionLog 레코드:

Key:
  - transactionalId (String)

Value (TransactionMetadata 직렬화):
  - producerId: long (8 bytes)
  - producerEpoch: short (2 bytes)
  - transactionTimeoutMs: int (4 bytes)
  - transactionStatus: byte (1 byte)
    0: EMPTY
    1: ONGOING
    2: PREPARE_COMMIT
    3: PREPARE_ABORT
    4: COMPLETE_COMMIT
    5: COMPLETE_ABORT
    6: DEAD
    7: PREPARE_EPOCH_FENCE
  - topicPartitions: List<TopicPartition>
  - txnStartTimestamp: long (8 bytes)
  - txnLastUpdateTimestamp: long (8 bytes)
```

### 11.3 상태 복구

```
코디네이터 복구 (파티션 리더 전환):

__transaction_state-17 리더 전환
    |
    v
TransactionStateManager.loadTransactionsForTxnTopicPartition()
    |
    |-- __transaction_state-17의 모든 레코드 읽기
    |
    |-- 각 레코드를 TransactionMetadata로 변환
    |   Key: transactionalId
    |   Value: {PID, epoch, state, partitions, ...}
    |
    |-- 인메모리 캐시에 로딩
    |
    |-- 미완료 트랜잭션 처리:
    |   PREPARE_COMMIT -> Phase 2 재개 (마커 전송)
    |   PREPARE_ABORT  -> Phase 2 재개 (마커 전송)
    |   ONGOING (타임아웃) -> 자동 ABORT
    |
    v
코디네이터 활성화, 요청 처리 시작
```

---

## 12. 설계 결정: Why?

### 12.1 왜 별도의 __transaction_state 토픽인가?

```
대안 비교:

1. __consumer_offsets에 함께 저장:
   - 문제: 관심사 분리 위반
   - 문제: 컴팩션 주기/설정이 다름
   - 문제: 트랜잭션 부하가 오프셋 관리에 영향

2. 외부 스토어 (ZK, DB):
   - 문제: 추가 인프라 의존성
   - 문제: Kafka 복제의 내구성 활용 불가

3. 별도 내부 토픽 (현재 방식):
   + 독립적인 파티셔닝/복제 설정
   + Kafka 자체 복제로 내구성 보장
   + 컴팩션으로 자동 정리
   + 추가 인프라 불필요
```

### 12.2 왜 마커를 데이터 파티션에 기록하는가?

```
대안: 마커를 별도 토픽에 저장

문제:
- 컨슈머가 데이터 토픽과 마커 토픽을 동시에 읽어야 함
- 두 토픽 간 동기화 문제
- 복잡한 컨슈머 로직

현재 방식: 마커를 데이터 파티션에 인라인 기록

장점:
- 컨슈머는 하나의 파티션만 읽으면 됨
- 마커와 데이터의 순서가 자연스럽게 보장
- LSO 계산이 단순 (같은 로그에서 처리)
- 기존 로그 정리 메커니즘으로 마커도 정리
```

### 12.3 왜 PID + Epoch + Sequence인가?

```
더 단순한 대안 vs 현재 방식:

[대안 1: 메시지 UUID]
  각 메시지에 UUID 부여, 브로커가 중복 확인
  -> 문제: 브로커가 모든 UUID를 기억해야 함 (무한 저장)
  -> 문제: 메모리/디스크 사용량 폭증

[대안 2: 시퀀스만 사용]
  파티션별 연속 번호
  -> 문제: 프로듀서 재시작 시 시퀀스 리셋
  -> 문제: 여러 프로듀서 구분 불가

[현재: PID + Epoch + Sequence]
  + PID: 프로듀서 식별
  + Epoch: 같은 PID의 세대 구분 (좀비 감지)
  + Sequence: 파티션 내 순서 + 중복 감지
  + 브로커는 PID별 최근 5개 배치만 기억
    (ProducerStateManager)
  + 메모리 효율적
```

### 12.4 왜 트랜잭션 타임아웃이 필요한가?

```
타임아웃이 없는 경우의 위험:

1. 프로듀서가 트랜잭션 시작 후 장애
   -> ONGOING 상태 영구 지속
   -> LSO가 영구히 정지
   -> Read Committed 컨슈머 완전 차단

2. 프로듀서 버그로 EndTxn 호출 누락
   -> 동일한 문제 발생

해결: transaction.max.timeout.ms (기본 15분)
  -> 타임아웃 초과 시 코디네이터가 자동 ABORT
  -> 주기적 체크: transactionAbortTimedOutTransactionCleanupIntervalMs

클라이언트 설정:
  transaction.timeout.ms (기본 60초)
  -> 서버의 max 이하여야 함
```

### 12.5 Exactly-Once의 한계와 범위

```
Exactly-Once가 보장되는 범위:

+----------------------------------------------+
| 보장됨:                                      |
| - Kafka -> Kafka (Consume-Transform-Produce) |
|   producer.sendOffsetsToTransaction() 사용   |
|   오프셋 커밋과 메시지 전송이 원자적          |
|                                               |
| - 단일 프로듀서의 파티션 내 중복 방지         |
|   (PID + Epoch + Sequence)                    |
|                                               |
| - 다수 파티션에 대한 원자적 쓰기              |
|   (트랜잭션)                                  |
+----------------------------------------------+
| 보장되지 않음:                                |
| - Kafka -> 외부 시스템 (DB, API 등)           |
|   외부 시스템의 멱등성은 별도 구현 필요       |
|                                               |
| - 애플리케이션 레벨 재전송                    |
|   producer.send()를 직접 재호출하면 중복 발생 |
|   (Kafka 재시도만 멱등, 앱 재시도는 아님)     |
|                                               |
| - 다른 프로듀서 인스턴스의 중복               |
|   (같은 transactionalId를 사용하지 않으면)    |
+----------------------------------------------+
```

---

## 요약

```
Kafka 트랜잭션 시스템 요약:

프로토콜 흐름:
  InitProducerId -> BeginTxn -> Produce -> EndTxn

상태 머신:
  EMPTY -> ONGOING -> PREPARE_COMMIT -> COMPLETE_COMMIT
                   -> PREPARE_ABORT  -> COMPLETE_ABORT

2PC:
  Phase 1: __transaction_state에 PREPARE 기록 (결정 영구화)
  Phase 2: 데이터 파티션에 마커 기록 (결정 전파)

멱등성:
  PID + Epoch + Sequence로 중복 방지
  파티션별 시퀀스, 브로커가 최근 배치 기억

펜싱:
  에포크 비교로 좀비 프로듀서 차단
  PREPARE_EPOCH_FENCE로 기존 트랜잭션 abort

Read Committed:
  LSO(Last Stable Offset)로 미완료 트랜잭션 차단
  abortedTransactions 목록으로 ABORT된 메시지 필터링

핵심 설계 원칙:
1. __transaction_state 토픽으로 코디네이터 상태 영구 저장
2. 인라인 컨트롤 레코드로 마커와 데이터의 순서 보장
3. 블록 단위 PID 할당으로 컨트롤러 부하 분산
4. 타임아웃으로 미완료 트랜잭션 자동 정리
5. 에포크 기반 펜싱으로 좀비 프로듀서 차단
```
