# Apache Kafka 데이터 모델

## 1. 개요

Kafka의 데이터 모델은 **레코드(Record)**를 기본 단위로, 레코드가 **배치(RecordBatch)**로 묶이고, 배치가 **세그먼트(Segment)**에 저장되며, 세그먼트가 **파티션(Partition)**을 구성하는 계층 구조다.

```
Topic
  └─ Partition (순서 보장 단위)
       └─ LogSegment (물리 파일)
            └─ RecordBatch (네트워크/디스크 I/O 단위)
                 └─ Record (개별 메시지)
```

## 2. Record (개별 레코드)

### 2.1 Record 인터페이스

**소스**: `clients/src/main/java/org/apache/kafka/common/record/internal/Record.java`

```
Record:
  ├─ offset()     : long      ← 파티션 내 고유 오프셋
  ├─ sequence()   : int       ← 프로듀서 시퀀스 번호
  ├─ timestamp()  : long      ← 레코드 타임스탬프
  ├─ key()        : ByteBuffer ← 키 (nullable)
  ├─ value()      : ByteBuffer ← 값 (nullable)
  └─ headers()    : Header[]   ← 헤더 배열 (v2+)
```

### 2.2 DefaultRecord 바이너리 포맷 (Magic v2)

**소스**: `clients/src/main/java/org/apache/kafka/common/record/internal/DefaultRecord.java`

```
Record Wire Format:
  ┌──────────────────────────────────────────────┐
  │ Length            (Varint)                    │  가변 길이 정수
  │ Attributes        (Int8)                     │  현재 미사용
  │ TimestampDelta    (Varlong)                  │  배치 기준 시간 차이
  │ OffsetDelta       (Varint)                   │  배치 기준 오프셋 차이
  │ KeyLength         (Varint)                   │  키 길이 (-1이면 null)
  │ Key               (Bytes)                    │  키 데이터
  │ ValueLength       (Varint)                   │  값 길이 (-1이면 null)
  │ Value             (Bytes)                    │  값 데이터
  │ HeadersCount      (Varint)                   │  헤더 개수
  │ Headers           [{key, value}]             │  헤더 배열
  └──────────────────────────────────────────────┘
```

**설계 이유**:
- **Varint 인코딩**: 작은 값은 1바이트, 큰 값도 최대 5바이트 — 공간 절약
- **Delta 인코딩**: 타임스탬프와 오프셋을 배치 기준값 대비 차이로 저장 — 압축 효율 극대화
- **MAX_RECORD_OVERHEAD**: 21바이트 (키/값/헤더 제외한 최대 오버헤드)

## 3. RecordBatch (레코드 배치)

### 3.1 배치 구조

**소스**: `clients/src/main/java/org/apache/kafka/common/record/internal/DefaultRecordBatch.java`

```
RecordBatch Wire Format (RECORD_BATCH_OVERHEAD = 61 bytes):
  ┌─────────────────────────────────────────────────┐
  │ Offset 0-7:   BaseOffset          (Int64)       │  배치 시작 오프셋
  │ Offset 8-11:  Length              (Int32)       │  배치 전체 길이
  │ Offset 12-15: PartitionLeaderEpoch (Int32)      │  리더 에포크
  │ Offset 16:    Magic               (Int8)        │  포맷 버전 (현재 2)
  │ Offset 17-20: CRC                 (Uint32)      │  CRC32-C 체크섬
  │ Offset 21-22: Attributes          (Int16)       │  압축, 타임스탬프 등
  │ Offset 23-26: LastOffsetDelta     (Int32)       │  마지막 레코드 오프셋 차이
  │ Offset 27-34: BaseTimestamp       (Int64)       │  기준 타임스탬프
  │ Offset 35-42: MaxTimestamp        (Int64)       │  최대 타임스탬프
  │ Offset 43-50: ProducerId          (Int64)       │  프로듀서 ID
  │ Offset 51-52: ProducerEpoch       (Int16)       │  프로듀서 에포크
  │ Offset 53-56: BaseSequence        (Int32)       │  기준 시퀀스 번호
  │ Offset 57-60: RecordsCount        (Int32)       │  레코드 개수
  │ Offset 61+:   Records             [Record...]   │  레코드 배열 (압축 가능)
  └─────────────────────────────────────────────────┘
```

### 3.2 Attributes 비트마스크

```
Bit 0-2:  압축 타입
            000 = NONE
            001 = GZIP
            010 = SNAPPY
            011 = LZ4
            100 = ZSTD

Bit 3:    타임스탬프 타입
            0 = CREATE_TIME (프로듀서 지정)
            1 = LOG_APPEND_TIME (브로커 지정)

Bit 4:    트랜잭션 플래그 (1 = 트랜잭션 메시지)
Bit 5:    컨트롤 플래그 (1 = 컨트롤 레코드)
Bit 6:    삭제 호라이즌 플래그
Bit 7-15: 미사용
```

### 3.3 압축 타입

**소스**: `clients/src/main/java/org/apache/kafka/common/record/internal/CompressionType.java`

| 타입 | 코드 | 특징 |
|------|------|------|
| NONE | 0 | 압축 없음, 최소 CPU |
| GZIP | 1 | 높은 압축률, 높은 CPU (기본 레벨 6) |
| SNAPPY | 2 | 빠른 압축/해제, 중간 압축률 |
| LZ4 | 3 | 매우 빠른 해제, 적절한 압축률 (기본 레벨 9) |
| ZSTD | 4 | 최고 압축률, 빠른 해제 (기본 레벨 3) |

**배치 단위 압축**: 레코드 배열 전체가 하나의 압축 단위. 개별 레코드가 아닌 배치를 압축하므로 사전(dictionary) 효과로 압축률이 높다.

## 4. MemoryRecords (인메모리 배치 컨테이너)

**소스**: `clients/src/main/java/org/apache/kafka/common/record/internal/MemoryRecords.java`

```java
public class MemoryRecords extends AbstractRecords {
    private final ByteBuffer buffer;     // 모든 배치를 담는 버퍼
}
```

| 메서드 | 용도 |
|--------|------|
| `sizeInBytes()` | 전체 배치의 바이트 크기 |
| `writeTo(TransferableChannel)` | 네트워크 전송 (zero-copy sendfile) |
| `batchIterator()` | 배치 순회 |
| `validBytes()` | 유효한 바이트 수 (불완전 배치 제외) |

## 5. Topic과 Partition 모델

### 5.1 TopicPartition

**소스**: `clients/src/main/java/org/apache/kafka/common/TopicPartition.java`

```java
public class TopicPartition {
    private final int partition;    // 파티션 번호
    private final String topic;     // 토픽 이름
}
// toString(): "orders-0"
```

### 5.2 TopicIdPartition

**소스**: `clients/src/main/java/org/apache/kafka/common/TopicIdPartition.java`

```java
public class TopicIdPartition {
    private final Uuid topicId;                  // 고유 UUID
    private final TopicPartition topicPartition; // (토픽명, 파티션)
}
```

**왜 UUID가 필요한가?** 토픽을 삭제하고 같은 이름으로 재생성하면 이전 토픽과 구분해야 한다. UUID로 구별하면 오래된 데이터가 새 토픽에 혼입되는 것을 방지할 수 있다.

### 5.3 PartitionRegistration (메타데이터)

**소스**: `metadata/src/main/java/org/apache/kafka/metadata/PartitionRegistration.java`

```
PartitionRegistration:
  ├─ replicas[]              ← 전체 복제본 브로커 ID 목록
  ├─ directories[]           ← 각 복제본의 디렉토리 UUID
  ├─ isr[]                   ← ISR (In-Sync Replicas) 목록
  ├─ removingReplicas[]      ← 제거 진행 중인 복제본
  ├─ addingReplicas[]        ← 추가 진행 중인 복제본
  ├─ elr[]                   ← 선출 가능 리더 복제본
  ├─ lastKnownElr[]          ← 마지막 알려진 ELR (복구용)
  ├─ leader                  ← 리더 브로커 ID
  ├─ leaderRecoveryState     ← RECOVERED | RECOVERING
  ├─ leaderEpoch             ← 리더 에포크 (단조 증가)
  └─ partitionEpoch          ← 파티션 에포크 (KRaft)
```

### 5.4 LeaderAndIsr

**소스**: `metadata/src/main/java/org/apache/kafka/metadata/LeaderAndIsr.java`

```java
public class LeaderAndIsr {
    private final int leader;
    private final int leaderEpoch;
    private final LeaderRecoveryState leaderRecoveryState;
    private final List<BrokerState> isrWithBrokerEpoch;
    private final int partitionEpoch;

    public static final int INITIAL_LEADER_EPOCH = 0;
    public static final int NO_LEADER = -1;
}
```

## 6. 로그 세그먼트 모델

### 6.1 LogSegment

**소스**: `storage/src/main/java/org/apache/kafka/storage/internals/log/LogSegment.java`

```
LogSegment:
  ├─ FileRecords log           ← 실제 데이터 (.log 파일)
  ├─ OffsetIndex offsetIndex   ← 오프셋 인덱스 (.index)
  ├─ TimeIndex timeIndex       ← 타임스탬프 인덱스 (.timeindex)
  ├─ TransactionIndex txnIndex ← 트랜잭션 인덱스 (.txnindex)
  ├─ long baseOffset           ← 세그먼트 시작 오프셋
  └─ int indexIntervalBytes    ← 인덱스 엔트리 간격 (기본 4KB)
```

### 6.2 파일 구조

```
/var/kafka-logs/orders-0/
  ├─ 00000000000000000000.log         ← 첫 번째 세그먼트 데이터
  ├─ 00000000000000000000.index       ← 오프셋 → 파일 위치 매핑
  ├─ 00000000000000000000.timeindex   ← 타임스탬프 → 오프셋 매핑
  ├─ 00000000000000000000.txnindex    ← 중단된 트랜잭션 목록
  ├─ 00000000000000001000.log         ← 두 번째 세그먼트
  ├─ 00000000000000001000.index
  ├─ 00000000000000001000.timeindex
  ├─ 00000000000000001000.txnindex
  └─ leader-epoch-checkpoint          ← 리더 에포크 체크포인트
```

**파일명 규칙**: 20자리 제로 패딩된 base offset (예: `00000000000000001000`)

### 6.3 인덱스 구조

**OffsetIndex** (8바이트/엔트리):
```
[4 bytes: 상대 오프셋] [4 bytes: 파일 내 절대 위치]
```

**TimeIndex** (12바이트/엔트리):
```
[8 bytes: 타임스탬프] [4 bytes: 상대 오프셋]
```

**TransactionIndex** (32바이트/엔트리):
```
[8 bytes: producerId] [8 bytes: firstOffset]
[8 bytes: lastOffset]  [8 bytes: lastStableOffset]
```

### 6.4 UnifiedLog

**소스**: `storage/src/main/java/org/apache/kafka/storage/internals/log/UnifiedLog.java`

로컬 세그먼트와 원격(Tiered) 세그먼트의 통합 뷰:

```
UnifiedLog:
  ├─ [Remote segments]     ← 원격 저장소 (선택적)
  ├─ [Overlap region]      ← 원격+로컬 중복 구간
  └─ [Local segments]      ← 로컬 디스크 (활성 세그먼트 포함)
       └─ LocalLog
            └─ LogSegments (ConcurrentNavigableMap<Long, LogSegment>)
```

## 7. 프로토콜 메시지 정의

### 7.1 메시지 스키마 시스템

**소스**: `clients/src/main/resources/common/message/`

Kafka의 모든 요청/응답은 JSON 스키마로 정의되며, 빌드 시 자동 생성된다.

```json
// ProduceRequest.json 예시
{
  "apiKey": 0,
  "type": "request",
  "name": "ProduceRequest",
  "validVersions": "3-13",
  "flexibleVersions": "9+",
  "fields": [
    { "name": "TransactionalId", "type": "string", "versions": "3+", "nullableVersions": "3+" },
    { "name": "Acks", "type": "int16", "versions": "3+" },
    { "name": "TimeoutMs", "type": "int32", "versions": "3+" },
    { "name": "TopicData", "type": "[]TopicProduceData", "versions": "3+",
      "fields": [
        { "name": "Name", "type": "string", "versions": "3-12" },
        { "name": "TopicId", "type": "uuid", "versions": "13+" },
        { "name": "PartitionData", "type": "[]PartitionProduceData",
          "fields": [
            { "name": "Index", "type": "int32" },
            { "name": "Records", "type": "records" }
          ]
        }
      ]
    }
  ]
}
```

### 7.2 주요 API 키

**소스**: `clients/src/main/java/org/apache/kafka/common/protocol/ApiKeys.java`

| API Key | 이름 | 용도 |
|---------|------|------|
| 0 | PRODUCE | 메시지 발행 |
| 1 | FETCH | 메시지 가져오기 |
| 2 | LIST_OFFSETS | 오프셋 조회 |
| 3 | METADATA | 토픽/브로커 메타데이터 |
| 8 | OFFSET_COMMIT | 오프셋 커밋 |
| 9 | OFFSET_FETCH | 커밋된 오프셋 조회 |
| 10 | FIND_COORDINATOR | 코디네이터 브로커 찾기 |
| 11 | JOIN_GROUP | 컨슈머 그룹 참여 |
| 14 | SYNC_GROUP | 그룹 할당 동기화 |
| 22 | INIT_PRODUCER_ID | 프로듀서 ID 초기화 |
| 26 | END_TXN | 트랜잭션 종료 |
| 52 | VOTE | KRaft 투표 |
| 56 | ALTER_PARTITION | ISR 변경 |
| 68 | CONSUMER_GROUP_HEARTBEAT | 새 프로토콜 하트비트 |

## 8. 컨슈머 그룹 메타데이터

### 8.1 __consumer_offsets 토픽

오프셋과 그룹 메타데이터를 저장하는 내부 토픽:

**오프셋 커밋 키/값**:
```
Key: OffsetCommitKey
  ├─ group: string        ← 컨슈머 그룹 ID
  ├─ topic: string        ← 토픽 이름
  └─ partition: int32     ← 파티션 번호

Value: OffsetCommitValue
  ├─ offset: int64        ← 커밋된 오프셋
  ├─ leaderEpoch: int32   ← 리더 에포크
  ├─ metadata: string     ← 클라이언트 메타데이터
  ├─ commitTimestamp: int64 ← 커밋 시각
  └─ topicId: uuid        ← 토픽 UUID (v4+)
```

### 8.2 그룹 유형

**소스**: `group-coordinator/src/main/java/org/apache/kafka/coordinator/group/`

| 그룹 유형 | 설명 | 프로토콜 |
|-----------|------|---------|
| `CLASSIC` | 기존 JoinGroup/SyncGroup 기반 | JoinGroup + SyncGroup + Heartbeat |
| `CONSUMER` | KIP-848 새 프로토콜 | ConsumerGroupHeartbeat |
| `SHARE` | KIP-932 공유 소비 모델 | ShareGroupHeartbeat |
| `STREAMS` | Kafka Streams 전용 | StreamsGroupHeartbeat |

## 9. 데이터 관계 다이어그램

```
                    TopicPartition
                    ("orders", 0)
                         │
                 TopicIdPartition
            (uuid="abc-123", "orders", 0)
                         │
                 PartitionRegistration
                 (메타데이터: 리더, ISR, 에포크)
                         │
                      UnifiedLog
                    (orders-0 파티션)
                         │
           ┌─────────────┼─────────────┐
           │             │             │
       LogSegment    LogSegment    LogSegment
       (base=0)     (base=1000)   (base=5000, 활성)
           │
    ┌──────┼──────┐
    │      │      │
  .log  .index .timeindex
    │
 RecordBatch[]
    │
 ┌──┼──┐
 │  │  │
 R0 R1 R2  (DefaultRecord)
```

## 10. 체크포인트 파일

### recovery-point-offset-checkpoint

각 로그 디렉토리에 위치하며, 복구 시 시작점을 기록:

```
0                          ← 버전
2                          ← 엔트리 수
orders 0 1000              ← 토픽 파티션 오프셋
orders 1 2000
```

### log-start-offset-checkpoint

각 파티션의 시작 오프셋 (로그 잘림/삭제 후):

```
0
2
orders 0 500
orders 1 800
```

**소스**: `storage/src/main/java/org/apache/kafka/storage/internals/checkpoint/OffsetCheckpointFile.java`
