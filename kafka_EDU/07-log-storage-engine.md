# 07. Kafka 로그 스토리지 엔진 Deep-Dive

## 목차

1. [개요](#1-개요)
2. [파일 레이아웃](#2-파일-레이아웃)
3. [LogSegment 구조](#3-logsegment-구조)
4. [인덱스 시스템](#4-인덱스-시스템)
5. [쓰기 경로 (Write Path)](#5-쓰기-경로-write-path)
6. [읽기 경로 (Read Path)](#6-읽기-경로-read-path)
7. [세그먼트 롤링](#7-세그먼트-롤링)
8. [로그 보존 정책](#8-로그-보존-정책)
9. [로그 컴팩션](#9-로그-컴팩션)
10. [Tiered Storage](#10-tiered-storage)
11. [체크포인트 메커니즘](#11-체크포인트-메커니즘)
12. [장애 복구](#12-장애-복구)
13. [설계 결정의 이유 (Why)](#13-설계-결정의-이유-why)

---

## 1. 개요

Kafka의 핵심은 **추가 전용(append-only) 로그 스토리지 엔진**이다. 모든 메시지는 디스크의
순차적 파일에 기록되며, 이 단순한 구조가 Kafka의 높은 처리량과 내구성의 근본이다.

### 왜 추가 전용 로그인가?

전통적인 메시지 브로커(RabbitMQ, ActiveMQ 등)는 메시지 소비 후 삭제하는 모델을 사용한다.
Kafka는 이와 달리 모든 메시지를 보존하고, 컨슈머가 자신의 오프셋을 관리하도록 설계했다.

```
전통적 브로커:                    Kafka:
+---------+                      +---------+
| Message | --consume--> 삭제     | Message | --consume--> 오프셋 이동
+---------+                      +---------+
                                 (메시지는 보존)
```

이 설계의 핵심 이점:
- **순차 I/O**: 디스크의 순차 쓰기는 랜덤 쓰기보다 수백 배 빠르다
- **다중 컨슈머**: 같은 데이터를 여러 컨슈머가 독립적으로 소비 가능
- **시간 여행**: 과거의 특정 오프셋부터 재소비 가능
- **단순한 복제**: 팔로워가 리더의 로그를 순차적으로 복제

### 핵심 소스 파일

| 파일 | 경로 | 역할 |
|------|------|------|
| UnifiedLog.java | `storage/.../internals/log/UnifiedLog.java` | 로컬+원격 통합 뷰 |
| LocalLog.java | `storage/.../internals/log/LocalLog.java` | 로컬 세그먼트 관리 |
| LogSegment.java | `storage/.../internals/log/LogSegment.java` | 단일 세그먼트 |
| OffsetIndex.java | `storage/.../internals/log/OffsetIndex.java` | 오프셋 인덱스 |
| TimeIndex.java | `storage/.../internals/log/TimeIndex.java` | 타임스탬프 인덱스 |
| TransactionIndex.java | `storage/.../internals/log/TransactionIndex.java` | 트랜잭션 인덱스 |
| LogFileUtils.java | `storage/.../internals/log/LogFileUtils.java` | 파일 유틸리티 |
| LogManager.java | `storage/.../internals/log/LogManager.java` | 로그 관리자 |
| Cleaner.java | `storage/.../internals/log/Cleaner.java` | 로그 컴팩션 |
| SkimpyOffsetMap.java | `storage/.../internals/log/SkimpyOffsetMap.java` | 컴팩션용 해시맵 |

> 소스 경로 기준: `storage/src/main/java/org/apache/kafka/storage/internals/log/`

---

## 2. 파일 레이아웃

### 2.1 디렉토리 구조

Kafka의 각 파티션은 독립적인 디렉토리에 저장된다.

```
/kafka-logs/                              # log.dirs 설정
  +-- my-topic-0/                         # {토픽명}-{파티션번호}
  |     +-- 00000000000000000000.log      # 첫 번째 세그먼트 (데이터)
  |     +-- 00000000000000000000.index    # 오프셋 인덱스
  |     +-- 00000000000000000000.timeindex # 타임스탬프 인덱스
  |     +-- 00000000000000000000.txnindex  # 트랜잭션 인덱스
  |     +-- 00000000000005367851.log      # 두 번째 세그먼트
  |     +-- 00000000000005367851.index
  |     +-- 00000000000005367851.timeindex
  |     +-- 00000000000005367851.txnindex
  |     +-- 00000000000010735702.log      # 세 번째 세그먼트 (활성 세그먼트)
  |     +-- 00000000000010735702.index
  |     +-- 00000000000010735702.timeindex
  |     +-- 00000000000010735702.txnindex
  |     +-- leader-epoch-checkpoint         # 리더 에포크 체크포인트
  |     +-- partition.metadata              # 파티션 메타데이터 (토픽 ID)
  |     +-- 00000000000005367851.snapshot  # 프로듀서 상태 스냅샷
  |
  +-- my-topic-1/
  |     +-- ...
  |
  +-- recovery-point-offset-checkpoint     # 복구 포인트 체크포인트
  +-- log-start-offset-checkpoint          # 로그 시작 오프셋 체크포인트
  +-- replication-offset-checkpoint        # High Watermark 체크포인트
  +-- cleaner-offset-checkpoint            # 컴팩션 진행 체크포인트
  +-- meta.properties                      # 브로커 메타데이터
```

### 2.2 20자리 오프셋 패딩

파일 이름은 해당 세그먼트의 **베이스 오프셋**을 20자리로 제로 패딩한 값이다.

```
LogFileUtils.java에서:

public static long offsetFromFileName(String fileName) {
    return Long.parseLong(fileName.substring(0, fileName.indexOf('.')));
}
```

20자리를 사용하는 이유: `Long.MAX_VALUE = 9223372036854775807` (19자리)이므로
20자리면 모든 가능한 오프셋 값을 표현할 수 있다.

```
파일 이름 생성 규칙:
  오프셋 0       → 00000000000000000000.log
  오프셋 5367851 → 00000000000005367851.log
  최대 오프셋     → 09223372036854775807.log
                   ^^^^^^^^^^^^^^^^^^^^
                   20자리 제로 패딩
```

### 2.3 파일 확장자와 역할

`LogFileUtils.java`에서 정의된 파일 확장자:

```java
// LogFileUtils.java
public static final String LOG_FILE_SUFFIX = ".log";               // 레코드 데이터
public static final String INDEX_FILE_SUFFIX = ".index";           // 오프셋→물리위치 인덱스
public static final String TIME_INDEX_FILE_SUFFIX = ".timeindex";  // 타임스탬프→오프셋 인덱스
public static final String TXN_INDEX_FILE_SUFFIX = ".txnindex";    // 트랜잭션 경계
public static final String PRODUCER_SNAPSHOT_FILE_SUFFIX = ".snapshot"; // 프로듀서 상태
public static final String CLEANED_FILE_SUFFIX = ".cleaned";       // 컴팩션 중간 결과
public static final String SWAP_FILE_SUFFIX = ".swap";             // 컴팩션 교체 중
public static final String DELETED_FILE_SUFFIX = ".deleted";       // 삭제 예정
```

### 2.4 특수 디렉토리 접미사

```java
// LogFileUtils.java
public static final String DELETE_DIR_SUFFIX = "-delete";   // 삭제 예정 디렉토리
public static final String FUTURE_DIR_SUFFIX = "-future";   // 로그 디렉토리 이동 중
public static final String STRAY_DIR_SUFFIX = "-stray";     // 미할당 파티션
```

---

## 3. LogSegment 구조

### 3.1 세그먼트의 4가지 구성 요소

`LogSegment.java`는 4가지 핵심 컴포넌트로 구성된다:

```java
// LogSegment.java
public class LogSegment implements Closeable {
    private final FileRecords log;                          // .log 파일 (실제 레코드)
    private final LazyIndex<OffsetIndex> lazyOffsetIndex;   // .index 파일
    private final LazyIndex<TimeIndex> lazyTimeIndex;       // .timeindex 파일
    private final TransactionIndex txnIndex;                // .txnindex 파일
    private final long baseOffset;                          // 세그먼트 기준 오프셋
    private final int indexIntervalBytes;                   // 인덱스 엔트리 간격
    private final long rollJitterMs;                        // 롤링 지터
    private final Time time;
}
```

```
+------------------------------------------------------------------+
|                         LogSegment                                |
|                                                                   |
|  baseOffset = 5367851                                            |
|                                                                   |
|  +----------------------+  +-------------------+                  |
|  |    FileRecords       |  |   OffsetIndex     |                  |
|  |  (.log 파일)          |  |  (.index 파일)     |                  |
|  |                      |  |                   |                  |
|  |  [Batch1][Batch2]... |  |  offset -> pos    |                  |
|  |  순차적 레코드 배치     |  |  이진 탐색 가능      |                  |
|  +----------------------+  +-------------------+                  |
|                                                                   |
|  +----------------------+  +-------------------+                  |
|  |    TimeIndex         |  | TransactionIndex  |                  |
|  |  (.timeindex 파일)    |  |  (.txnindex 파일)  |                  |
|  |                      |  |                   |                  |
|  |  timestamp -> offset |  |  txn 경계 추적      |                  |
|  +----------------------+  +-------------------+                  |
+------------------------------------------------------------------+
```

### 3.2 LazyIndex

인덱스는 `LazyIndex` 래퍼를 통해 **지연 로딩**된다. 이는 브로커 시작 시 모든 인덱스를
메모리 맵핑하는 비용을 줄여준다. 실제로 접근이 필요할 때만 mmap이 생성된다.

### 3.3 FileRecords

`FileRecords`는 `.log` 파일에 대한 추상화로, `java.nio.FileChannel`을 사용하여
디스크에서 직접 레코드를 읽고 쓴다. 핵심 메서드:

- `append(MemoryRecords)`: 레코드를 파일 끝에 추가
- `slice(startPosition, maxSize)`: 특정 위치에서 레코드 슬라이스 읽기
- `writeTo(GatheringByteChannel, position, length)`: Zero-copy 전송

### 3.4 주요 상태 변수

```java
// LogSegment.java
// 인덱스 엔트리 추가 이후 누적된 바이트 수
private int bytesSinceLastIndexEntry = 0;

// 이 세그먼트에서 관찰된 최대 타임스탬프
private volatile TimestampOffset maxTimestampAndOffsetSoFar = TimestampOffset.UNKNOWN;

// 시간 기반 롤링에 사용되는 기준 타임스탬프
private volatile OptionalLong rollingBasedTimestamp = OptionalLong.empty();
```

---

## 4. 인덱스 시스템

### 4.1 AbstractIndex 기반 클래스

모든 인덱스는 `AbstractIndex`를 상속하며, `MappedByteBuffer`(mmap)를 통해
메모리 매핑된 파일로 구현된다.

```java
// AbstractIndex.java
public abstract class AbstractIndex implements Closeable {
    private final ReentrantLock lock = new ReentrantLock();
    private final ReentrantReadWriteLock remapLock = new ReentrantReadWriteLock();

    private final long baseOffset;
    private final int maxIndexSize;
    private volatile MappedByteBuffer mmap;    // 메모리 매핑된 버퍼
    private volatile int maxEntries;           // 최대 엔트리 수
    private volatile int entries;              // 현재 엔트리 수
}
```

**왜 mmap을 사용하는가?**

메모리 매핑 파일은 OS 페이지 캐시를 직접 활용한다. 별도의 사용자 공간 버퍼링 없이도
효율적으로 인덱스를 읽을 수 있고, 동시 읽기가 자연스럽게 지원된다. 크래시 시에도 OS가
데이터를 디스크에 플러시해주므로 내구성이 보장된다.

### 4.2 OffsetIndex (.index)

오프셋에서 물리적 파일 위치로의 매핑을 제공한다.

```java
// OffsetIndex.java
public final class OffsetIndex extends AbstractIndex {
    private static final int ENTRY_SIZE = 8;  // 4바이트 상대오프셋 + 4바이트 위치
    private volatile long lastOffset;
}
```

```
OffsetIndex 파일 포맷:
+-------------------+-------------------+-------------------+
| Entry 0           | Entry 1           | Entry 2           | ...
+--------+----------+--------+----------+--------+----------+
| RelOff | Position | RelOff | Position | RelOff | Position |
| 4 byte | 4 byte   | 4 byte | 4 byte   | 4 byte | 4 byte   |
+--------+----------+--------+----------+--------+----------+

RelOff = 실제오프셋 - baseOffset (상대 오프셋)
Position = .log 파일 내 물리적 바이트 위치

예시 (baseOffset = 5367851):
  Entry: (relOff=0, pos=0)       → 오프셋 5367851이 파일 위치 0에
  Entry: (relOff=142, pos=32768) → 오프셋 5367993이 파일 위치 32768에
  Entry: (relOff=287, pos=65536) → 오프셋 5368138이 파일 위치 65536에
```

**왜 상대 오프셋을 사용하는가?**

절대 오프셋은 8바이트(`long`)가 필요하지만, 상대 오프셋은 4바이트(`int`)로 충분하다.
이는 인덱스 파일 크기를 절반으로 줄이고, 같은 mmap 크기에 두 배의 엔트리를 저장할 수
있게 한다. 하나의 세그먼트가 `Integer.MAX_VALUE`(약 21억) 이상의 오프셋 범위를
가지는 경우는 실무에서 거의 없다.

### 4.3 이진 탐색 (Binary Search)

```java
// OffsetIndex.java
public OffsetPosition lookup(long targetOffset) {
    return inRemapReadLock(() -> {
        ByteBuffer idx = mmap().duplicate();
        int slot = largestLowerBoundSlotFor(idx, targetOffset, IndexSearchType.KEY);
        // slot이 찾아지면 해당 엔트리를 반환
        // 못 찾으면 (baseOffset, 0) 반환
    });
}
```

```
이진 탐색 예시: targetOffset = 5368100, baseOffset = 5367851

인덱스 엔트리: [0, 142, 287, 430, 575, 720]  (상대 오프셋)
대상 상대 오프셋: 5368100 - 5367851 = 249

이진 탐색:
  low=0, high=5
  mid=2 → 287 > 249 → high=1
  mid=0 → 0 < 249 → low=1
  mid=1 → 142 < 249 → low=2
  low > high → slot=1 (142)

결과: 오프셋 5367993의 물리적 위치 반환
→ 이후 순차 스캔으로 정확한 오프셋 찾기
```

### 4.4 TimeIndex (.timeindex)

타임스탬프에서 오프셋으로의 매핑을 제공한다.

```java
// TimeIndex.java
public class TimeIndex extends AbstractIndex {
    private static final int ENTRY_SIZE = 12;  // 8바이트 타임스탬프 + 4바이트 상대오프셋
    private volatile TimestampOffset lastEntry;
}
```

```
TimeIndex 파일 포맷:
+---------------------------+---------------------------+
| Entry 0                   | Entry 1                   | ...
+-----------+---------------+-----------+---------------+
| Timestamp | RelativeOffset| Timestamp | RelativeOffset|
| 8 bytes   | 4 bytes       | 8 bytes   | 4 bytes       |
+-----------+---------------+-----------+---------------+

의미: "이 타임스탬프 이전에는 해당 오프셋 이전의 메시지만 존재"
```

TimeIndex의 타임스탬프는 **단조 증가**가 보장된다. 새 엔트리는 이전 엔트리의
타임스탬프보다 크거나 같을 때만 추가된다.

### 4.5 TransactionIndex (.txnindex)

중단된 트랜잭션의 경계를 추적한다.

```java
// TransactionIndex.java
// AbortedTxn 레코드를 순차적으로 저장
// 각 엔트리: producerId(8B) + firstOffset(8B) + lastOffset(8B) + lastStableOffset(8B)
```

```
TransactionIndex 용도:
  READ_COMMITTED 격리 수준의 컨슈머가 중단된 트랜잭션의 메시지를 건너뛰기 위해 사용

  프로듀서 P1이 트랜잭션을 시작했다가 중단:
    offset 100: P1 시작
    offset 105: P1 데이터
    offset 110: P1 중단(abort)

  TransactionIndex에 기록:
    AbortedTxn(producerId=P1, firstOffset=100, lastOffset=110, lastStableOffset=95)
```

---

## 5. 쓰기 경로 (Write Path)

### 5.1 전체 쓰기 흐름

```
프로듀서 요청
    |
    v
+-------------------------------------------+
| UnifiedLog.appendAsLeader()               |
|  - analyzeAndValidateRecords()            |
|  - trimInvalidBytes()                     |
|  - synchronized(lock)                     |
|    |                                      |
|    v                                      |
|  validateAndAssignOffsets()               |
|  maybeRoll() → 필요 시 새 세그먼트          |
|  analyzeAndValidateProducerState()        |
|    |                                      |
|    v                                      |
|  localLog.append(lastOffset, records)     |
|  updateHighWatermarkWithLogEndOffset()    |
|  producerStateManager.update()            |
+-------------------------------------------+
            |
            v
+-------------------------------------------+
| LocalLog.append()                         |
|  - segments.activeSegment.append(...)     |
|  - nextOffsetMetadata 업데이트              |
+-------------------------------------------+
            |
            v
+-------------------------------------------+
| LogSegment.append()                       |
|  - log.append(records)  [FileRecords]     |
|  - 인덱스 업데이트 (간격 조건 충족 시)       |
|    - offsetIndex.append()                 |
|    - timeIndex.maybeAppend()              |
+-------------------------------------------+
            |
            v
+-------------------------------------------+
| FileRecords.append()                      |
|  - channel.write(records.buffer())        |
|  [FileChannel을 통한 디스크 쓰기]           |
+-------------------------------------------+
```

### 5.2 UnifiedLog.appendAsLeader()

리더 복제본에서 프로듀서 요청을 처리하는 진입점이다.

```java
// UnifiedLog.java (라인 999~)
public LogAppendInfo appendAsLeader(MemoryRecords records, int leaderEpoch) {
    return appendAsLeader(records, leaderEpoch, AppendOrigin.CLIENT,
        RequestLocal.noCaching(), VerificationGuard.SENTINEL,
        TransactionVersion.TV_UNKNOWN);
}
```

오버로드된 버전에서 실제 `append()` 메서드를 호출한다:

```java
// UnifiedLog.java (라인 1028~)
public LogAppendInfo appendAsLeader(MemoryRecords records, int leaderEpoch,
        AppendOrigin origin, RequestLocal requestLocal,
        VerificationGuard verificationGuard, short transactionVersion) {
    boolean validateAndAssignOffsets = true;  // 리더는 오프셋을 할당
    return append(records, origin, validateAndAssignOffsets, leaderEpoch, ...);
}
```

### 5.3 UnifiedLog.append() 내부 (핵심)

```java
// UnifiedLog.java (라인 1094~)
private LogAppendInfo append(MemoryRecords records, ...) {
    maybeFlushMetadataFile();  // 파티션 메타데이터 먼저 기록

    LogAppendInfo appendInfo = analyzeAndValidateRecords(records, ...);

    if (appendInfo.validBytes() <= 0) return appendInfo;

    final MemoryRecords trimmedRecords = trimInvalidBytes(records, appendInfo);

    synchronized (lock) {  // ★ 파티션 단위 잠금
        // 1. 오프셋 할당
        PrimitiveRef.LongRef offset = PrimitiveRef.ofLong(localLog.logEndOffset());
        appendInfo.setFirstOffset(offset.value);

        // 2. 메시지 검증 및 오프셋 할당
        LogValidator validator = new LogValidator(validRecords, ...);
        ValidationResult result = validator.validateMessagesAndAssignOffsets(offset, ...);

        // 3. 세그먼트 롤링 판단
        LogSegment segment = maybeRoll(validRecords.sizeInBytes(), appendInfo);

        // 4. 프로듀서 상태 검증 (멱등성/트랜잭션)
        AnalyzeAndValidateProducerStateResult result =
            analyzeAndValidateProducerState(logOffsetMetadata, validRecords, ...);

        // 5. 실제 로그에 기록
        localLog.append(appendInfo.lastOffset(), validRecords);
        updateHighWatermarkWithLogEndOffset();

        // 6. 프로듀서 상태 업데이트
        result.updatedProducers.values().forEach(producerStateManager::update);

        // 7. 트랜잭션 인덱스 업데이트
        for (CompletedTxn completedTxn : result.completedTxns) {
            long lastStableOffset = producerStateManager.lastStableOffset(completedTxn);
            segment.updateTxnIndex(completedTxn, lastStableOffset);
        }
    }
}
```

**왜 파티션 단위 잠금인가?**

Kafka는 파티션 수준에서 순서를 보장한다. 하나의 파티션 내에서 오프셋은 단조 증가해야
하므로, 동시에 여러 스레드가 같은 파티션에 쓸 수 없다. 그러나 서로 다른 파티션은
독립적으로 잠금 없이 병렬 쓰기가 가능하다.

### 5.4 LogSegment.append()

```java
// LogSegment.java (라인 250~)
public void append(long largestOffset, MemoryRecords records) throws IOException {
    if (records.sizeInBytes() > 0) {
        int physicalPosition = log.sizeInBytes();  // 현재 파일 끝 위치

        ensureOffsetInRange(largestOffset);  // 상대 오프셋 범위 확인

        // 1. FileRecords에 레코드 추가
        long appendedBytes = log.append(records);

        // 2. 배치별로 인덱스 업데이트
        for (RecordBatch batch : records.batches()) {
            // 최대 타임스탬프 갱신
            if (batchMaxTimestamp > maxTimestampSoFar()) {
                maxTimestampAndOffsetSoFar = new TimestampOffset(batchMaxTimestamp, batchLastOffset);
            }

            // indexIntervalBytes 이상 누적되면 인덱스 엔트리 추가
            if (bytesSinceLastIndexEntry > indexIntervalBytes) {
                offsetIndex().append(batchLastOffset, physicalPosition);
                timeIndex().maybeAppend(maxTimestampSoFar(), shallowOffsetOfMaxTimestampSoFar());
                bytesSinceLastIndexEntry = 0;  // 카운터 리셋
            }

            physicalPosition += batch.sizeInBytes();
            bytesSinceLastIndexEntry += batch.sizeInBytes();
        }
    }
}
```

### 5.5 인덱스 간격 (indexIntervalBytes)

```
indexIntervalBytes = 4096 (기본값, log.index.interval.bytes)

데이터 쓰기:
  [Batch 1: 512B]  bytesSince=512   → 인덱스 추가 안 함
  [Batch 2: 1024B] bytesSince=1536  → 인덱스 추가 안 함
  [Batch 3: 2048B] bytesSince=3584  → 인덱스 추가 안 함
  [Batch 4: 768B]  bytesSince=4352  → ★ 인덱스 엔트리 추가! (4352 > 4096)
                                        bytesSince 리셋 → 0
  [Batch 5: 512B]  bytesSince=512   → 인덱스 추가 안 함
  ...
```

**왜 희소 인덱스(sparse index)인가?**

모든 레코드의 위치를 인덱스에 저장하면 인덱스 파일이 데이터 파일만큼 커진다.
희소 인덱스는 mmap 메모리를 절약하면서도, 인덱스 조회 후 짧은 순차 스캔만으로
원하는 레코드를 찾을 수 있다. 순차 스캔 범위는 `indexIntervalBytes` 이내이므로
성능 영향이 미미하다.

---

## 6. 읽기 경로 (Read Path)

### 6.1 오프셋 기반 읽기 흐름

```
읽기 요청 (targetOffset)
    |
    v
+-------------------------------------------+
| LocalLog.read()                           |
|  - segments.floorSegment(targetOffset)    |
|    (NavigableMap에서 적절한 세그먼트 찾기)   |
+-------------------------------------------+
            |
            v
+-------------------------------------------+
| LogSegment.read()                         |
|  1. offsetIndex.lookup(targetOffset)      |
|     → OffsetPosition(relOffset, position) |
|                                           |
|  2. log.searchForOffsetWithSize(          |
|         targetOffset, position)           |
|     → LogOffsetPosition(offset, pos, sz)  |
|                                           |
|  3. log.slice(startPosition, length)      |
|     → FileRecords 슬라이스                  |
+-------------------------------------------+
```

### 6.2 2단계 탐색

```
예시: targetOffset = 5368100

단계 1: OffsetIndex 이진 탐색
  +----------------------------------+
  | 인덱스 엔트리들:                    |
  | (0, 0)                           |
  | (142, 32768)   ← hit!           |
  | (287, 65536)                     |
  +----------------------------------+
  결과: position = 32768

단계 2: .log 파일에서 순차 스캔
  +----------------------------------+
  | position 32768부터 배치 순회:       |
  |                                  |
  | Batch@32768: offset 5367993~5368020 |
  |   → 계속                          |
  | Batch@33792: offset 5368021~5368050 |
  |   → 계속                          |
  | ...                              |
  | Batch@36864: offset 5368090~5368120 |
  |   → ★ 목표 오프셋 포함!             |
  +----------------------------------+
  결과: startPosition = 36864

단계 3: FileRecords.slice()
  → position 36864부터 maxBytes까지의 파일 슬라이스 반환
```

### 6.3 타임스탬프 기반 읽기

타임스탬프로 오프셋을 찾는 경우, TimeIndex를 먼저 조회한다:

```
타임스탬프 검색 흐름:
  1. TimeIndex.lookup(targetTimestamp)
     → TimestampOffset(timestamp, offset)

  2. 찾은 offset으로 OffsetIndex.lookup(offset)
     → 물리적 위치 확인

  3. .log 파일에서 순차 스캔하며 정확한 타임스탬프 매칭
```

---

## 7. 세그먼트 롤링

### 7.1 롤링 조건

`LogSegment.shouldRoll()`은 다음 조건 중 하나라도 참이면 새 세그먼트를 생성한다:

```java
// LogSegment.java (라인 167~)
public boolean shouldRoll(RollParams rollParams) throws IOException {
    boolean reachedRollMs = timeWaitedForRoll(rollParams.now(),
        rollParams.maxTimestampInMessages()) > rollParams.maxSegmentMs() - rollJitterMs;
    int size = size();
    return size > rollParams.maxSegmentBytes() - rollParams.messagesSize()   // 1. 크기 초과
        || (size > 0 && reachedRollMs)                                       // 2. 시간 초과
        || offsetIndex().isFull()                                            // 3. 인덱스 꽉 참
        || timeIndex().isFull()                                              // 4. 시간인덱스 꽉 참
        || !canConvertToRelativeOffset(rollParams.maxOffsetInMessages());    // 5. 상대오프셋 오버플로
}
```

```
세그먼트 롤링 결정 다이어그램:

                        새 메시지 도착
                            |
            +---------------+---------------+
            |               |               |
            v               v               v
    크기 > maxSegmentBytes?  시간 > maxSegmentMs?  인덱스 꽉 찼는가?
            |               |               |
            +-------+-------+-------+-------+
                    |               |
                 Yes (any)         No (all)
                    |               |
                    v               v
             새 세그먼트 생성     기존 세그먼트에 추가
```

### 7.2 롤링 지터 (Roll Jitter)

`rollJitterMs`는 여러 파티션이 동시에 세그먼트를 롤링하는 것을 방지한다.

**왜 지터가 필요한가?**

모든 파티션이 정확히 같은 시간 간격으로 세그먼트를 롤링하면, 특정 시점에 모든 파티션이
동시에 새 파일을 생성하면서 I/O 스파이크가 발생한다. 지터를 추가하면 이 작업이 시간적으로
분산되어 시스템 부하가 균등해진다.

### 7.3 롤링 과정

```
세그먼트 롤링 과정:

  현재 상태:
    segments = {0: seg0, 5367851: seg1(active)}

  롤링 후:
    1. seg1의 인덱스를 trimToValidSize() - 남은 공간 제거
    2. seg1을 읽기 전용으로 변환
    3. 새 세그먼트 seg2 생성 (baseOffset = nextOffset)
    4. segments에 seg2 추가

    segments = {0: seg0, 5367851: seg1, 10735702: seg2(active)}
```

---

## 8. 로그 보존 정책

### 8.1 시간 기반 삭제

```
log.retention.ms (기본값: 168시간 = 7일)

시간 기반 삭제 흐름:
  1. LogManager가 주기적으로 cleanupLogs() 실행
  2. 각 파티션의 가장 오래된 세그먼트부터 검사
  3. 세그먼트의 최대 타임스탬프가 보존 기간보다 오래되었으면 삭제 대상
  4. 삭제 대상 세그먼트의 파일명에 .deleted 접미사 추가
  5. 스케줄러가 일정 시간 후 .deleted 파일을 물리적으로 삭제

  타임라인:
  |---seg0---|---seg1---|---seg2---|---seg3(active)---|
  |<-- 10일 전 -->|<-- 5일 전 -->|<-- 2일 전 -->| 현재 |

  retention.ms = 7일이면:
  seg0 삭제 대상 (10일 > 7일)
  seg1 보존 (5일 < 7일)
```

### 8.2 크기 기반 삭제

```
log.retention.bytes (기본값: -1, 제한 없음)

크기 기반 삭제 흐름:
  1. 파티션의 총 크기 계산
  2. 총 크기가 retention.bytes를 초과하면 가장 오래된 세그먼트부터 삭제
  3. 삭제 후에도 초과하면 다음 세그먼트 삭제 반복
```

### 8.3 삭제 프로세스

```
세그먼트 삭제 과정:

  원래 파일:
    00000000000000000000.log
    00000000000000000000.index
    00000000000000000000.timeindex
    00000000000000000000.txnindex

  1단계: .deleted 접미사 추가 (비동기)
    00000000000000000000.log.deleted
    00000000000000000000.index.deleted
    00000000000000000000.timeindex.deleted
    00000000000000000000.txnindex.deleted

  2단계: 스케줄러가 일정 시간(file.delete.delay.ms) 후 물리적 삭제
```

**왜 즉시 삭제하지 않는가?**

진행 중인 읽기 요청이 해당 세그먼트의 파일 디스크립터를 참조하고 있을 수 있다.
2단계 삭제는 이러한 레이스 컨디션을 방지한다. `.deleted` 접미사가 붙은 파일은
새로운 읽기 요청에서는 보이지 않지만, 기존 파일 디스크립터는 유효하다.

---

## 9. 로그 컴팩션

### 9.1 컴팩션 개요

`cleanup.policy=compact`로 설정된 토픽은 시간/크기 기반 삭제 대신 **키 기반 컴팩션**을
수행한다. 같은 키의 최신 값만 보존하고 이전 값을 제거한다.

```
컴팩션 전:                          컴팩션 후:
+-----+-------+                    +-----+-------+
| Key | Value |                    | Key | Value |
+-----+-------+                    +-----+-------+
| A   | v1    | offset=0           | C   | v1    | offset=2
| B   | v1    | offset=1           | A   | v2    | offset=3
| C   | v1    | offset=2           | B   | v2    | offset=4
| A   | v2    | offset=3           | D   | v1    | offset=5
| B   | v2    | offset=4           +-----+-------+
| D   | v1    | offset=5           (오프셋은 변경되지 않음!)
+-----+-------+
```

### 9.2 Cleaner 아키텍처

```java
// Cleaner.java
public class Cleaner {
    private final OffsetMap offsetMap;      // SkimpyOffsetMap (키 → 오프셋 매핑)
    private final int ioBufferSize;        // I/O 버퍼 크기
    private ByteBuffer readBuffer;         // 읽기 버퍼
    private ByteBuffer writeBuffer;        // 쓰기 버퍼
    private final Throttler throttler;     // I/O 제한
}
```

```
컴팩션 흐름:

  +-----------------------------------------+
  | LogCleaner (스레드 관리)                  |
  |  +-----------------------------------+  |
  |  | LogCleanerManager                 |  |
  |  | - 컴팩션 대상 파티션 선정            |  |
  |  | - 더티/클린 비율 계산               |  |
  |  +-----------------------------------+  |
  |  +-----------------------------------+  |
  |  | Cleaner 스레드 (N개)               |  |
  |  | - 실제 컴팩션 로직 수행             |  |
  |  +-----------------------------------+  |
  +-----------------------------------------+
```

### 9.3 더티 영역과 클린 영역

```
로그 세그먼트 구조:

  |<------- 클린 영역 ------->|<------- 더티 영역 ------->|
  +--------+--------+--------+--------+--------+--------+
  | seg0   | seg1   | seg2   | seg3   | seg4   | seg5   |
  | (컴팩션 | (컴팩션 | (컴팩션 |        |        |(active)|
  |  완료)  |  완료)  |  완료)  |        |        |        |
  +--------+--------+--------+--------+--------+--------+
                              ^
                      firstDirtyOffset
                      (cleaner-offset-checkpoint에 기록)

  더티 비율 = 더티 영역 크기 / 전체 로그 크기
  더티 비율이 min.cleanable.dirty.ratio 이상이면 컴팩션 트리거
```

### 9.4 SkimpyOffsetMap (MD5 해시 기반)

```java
// SkimpyOffsetMap.java
public class SkimpyOffsetMap implements OffsetMap {
    public final int bytesPerEntry;       // hashSize + 8 (오프셋)
    private final ByteBuffer bytes;       // 해시 테이블 버퍼
    private final MessageDigest digest;   // MD5 해시 인스턴스
    private final int hashSize;           // 16 (MD5)
    private final int slots;              // memory / bytesPerEntry
}
```

**왜 키 자체가 아닌 MD5 해시를 사용하는가?**

키의 크기는 가변적이고 매우 클 수 있다. MD5 해시(16바이트)를 키의 프록시로 사용하면:

1. **고정 크기**: 엔트리당 24바이트(16바이트 해시 + 8바이트 오프셋)로 고정
2. **메모리 효율**: 128MB 버퍼로 약 560만 개의 키를 추적 가능
3. **해시 충돌**: MD5의 충돌 확률은 매우 낮고, 충돌 시 최악의 경우 이전 값이 보존될
   뿐이므로 데이터 손실은 없다(다음 컴팩션 사이클에서 처리됨)

```
SkimpyOffsetMap 내부 구조:

  +----+-------+------+----+-------+------+----+-------+------+
  |Hash| Offset| (빈) |Hash| Offset| (빈) |Hash| Offset| (빈) |
  |16B |  8B   |      |16B |  8B   |      |16B |  8B   |      |
  +----+-------+------+----+-------+------+----+-------+------+
  |<-- slot 0 -->|      |<-- slot 1 -->|      |<-- slot 2 -->|

  put(key, offset):
    1. hash = MD5(key)
    2. slot = (hash의 처음 4바이트) % slots
    3. 선형 탐사(linear probing)로 빈 슬롯 또는 같은 해시 슬롯 찾기
    4. 해시와 오프셋 저장

  get(key):
    1. hash = MD5(key)
    2. slot 찾기 (선형 탐사)
    3. 해시 일치하면 오프셋 반환
    4. 빈 슬롯 만나면 -1 반환
```

### 9.5 컴팩션 프로세스

```
컴팩션 단계별 흐름:

단계 1: OffsetMap 구축 (더티 영역 스캔)
  더티 세그먼트를 역순으로 읽으며 키 → 최신오프셋 매핑 구축

  seg3: key=A@offset=1000, key=B@offset=1001
  seg4: key=A@offset=2000, key=C@offset=2001

  OffsetMap: {A→2000, B→1001, C→2001}

단계 2: 클린 세그먼트 재작성
  클린 세그먼트를 읽으며, OffsetMap에 있는 키의 메시지는 건너뛰기
  (더 최신 버전이 더티 영역에 존재하므로)

  .cleaned 접미사의 임시 파일에 결과 기록

단계 3: 더티 세그먼트 재작성
  더티 세그먼트를 읽으며, OffsetMap의 오프셋과 일치하는 레코드만 유지

  key=A@offset=1000 → 건너뛰기 (최신은 2000)
  key=A@offset=2000 → 유지

단계 4: 파일 교체
  .cleaned → .swap → 원본 교체
  1. 새 파일: 00000000000000000000.log.cleaned
  2. 원자적 이름 변경: .cleaned → .swap
  3. 원본 삭제
  4. .swap → 최종 이름
```

### 9.6 톰스톤 (Tombstone)

키에 대해 `value=null`인 메시지를 **톰스톤**이라 한다.

```
톰스톤의 수명:
  1. 톰스톤 메시지 생성 (key=A, value=null)
  2. 첫 번째 컴팩션: 톰스톤 유지 (delete.retention.ms 이내)
     - 하류 컨슈머가 삭제 이벤트를 볼 수 있도록
  3. 두 번째 컴팩션: delete.retention.ms 초과 시 톰스톤 제거
```

---

## 10. Tiered Storage

### 10.1 개요

Tiered Storage는 오래된 로그 세그먼트를 원격 스토리지(S3, HDFS 등)로 오프로드하는
기능이다. `UnifiedLog`는 로컬과 원격 세그먼트를 통합된 뷰로 제공한다.

```java
// UnifiedLog.java (클래스 주석)
/**
 * A log which presents a unified view of local and tiered log segments.
 *
 * The log consists of tiered and local segments with the tiered portion
 * of the log being optional. There could be an overlap between the tiered
 * and local segments. The active segment is always guaranteed to be local.
 */
```

```
Tiered Storage 아키텍처:

  +-------+-------+-------+-------+-------+-------+
  | seg0  | seg1  | seg2  | seg3  | seg4  | seg5  |
  |       |       |       |       |       |(active)|
  +-------+-------+-------+-------+-------+-------+
  |<-- 원격 스토리지 -->|<-- 겹침 -->|<-- 로컬 -->|
          (S3/HDFS)       (양쪽 존재)     (디스크)

  시간이 지남에 따라:
  - 오래된 세그먼트는 원격으로 복사
  - 복사 완료된 로컬 세그먼트는 삭제 가능
  - 활성 세그먼트는 항상 로컬
```

### 10.2 RemoteLogManager

```
소스 파일: storage/src/main/java/org/apache/kafka/server/log/remote/storage/RemoteLogManager.java

RemoteLogManager의 역할:
  1. 완료된 로컬 세그먼트를 원격 스토리지에 복사
  2. 원격 세그먼트의 메타데이터 관리
  3. 원격 세그먼트에서 데이터 읽기 지원
  4. 원격 세그먼트 보존/삭제 관리
```

### 10.3 읽기 통합

```
오프셋 기반 읽기 시 세그먼트 결정:

  요청된 오프셋이 로컬 세그먼트 범위 내?
    → 예: 로컬에서 직접 읽기
    → 아니오: RemoteLogManager를 통해 원격에서 읽기

  원격 읽기:
    1. RemoteLogMetadataManager에서 세그먼트 메타데이터 조회
    2. RemoteStorageManager에서 해당 세그먼트 데이터 가져오기
    3. RemoteIndexCache에서 인덱스 캐시 활용
```

---

## 11. 체크포인트 메커니즘

### 11.1 체크포인트 파일 종류

Kafka는 여러 종류의 체크포인트 파일을 사용하여 브로커 재시작 시 빠른 복구를 지원한다.

| 파일 | 기록 주기 | 용도 |
|------|----------|------|
| `recovery-point-offset-checkpoint` | 주기적 플러시 | 플러시 완료된 오프셋 |
| `log-start-offset-checkpoint` | 로그 시작 변경 시 | 각 파티션의 시작 오프셋 |
| `replication-offset-checkpoint` | 주기적 | High Watermark |
| `cleaner-offset-checkpoint` | 컴팩션 후 | 컴팩션 진행 위치 |

### 11.2 recovery-point-offset-checkpoint

```
역할: fsync가 완료된 마지막 오프셋을 기록

브로커 시작 시:
  1. 체크포인트에서 각 파티션의 recoveryPoint 읽기
  2. recoveryPoint 이후의 세그먼트만 복구 필요
  3. 복구: 세그먼트의 레코드를 읽으며 인덱스 재구축

  |------- 플러시 완료 -------|--- 복구 필요 ---|
  +--------+--------+--------+--------+--------+
  | seg0   | seg1   | seg2   | seg3   | seg4   |
  +--------+--------+--------+--------+--------+
                              ^
                       recoveryPoint
```

**왜 매 쓰기마다 fsync하지 않는가?**

`fsync`는 비용이 높은 연산이다. Kafka는 복제를 통해 내구성을 보장하므로, 모든 쓰기에
대해 `fsync`를 수행할 필요가 없다. 대신 OS 페이지 캐시에 의존하고, 주기적으로
(`log.flush.interval.messages` 또는 `log.flush.interval.ms`)에만 플러시한다.

### 11.3 log-start-offset-checkpoint

```
역할: 각 파티션에서 유효한 데이터의 시작 오프셋

용도:
  - 시간/크기 기반 삭제 후 logStartOffset 갱신
  - 컨슈머가 이 오프셋 이전의 데이터를 요청하면 OffsetOutOfRangeException
  - AdminClient.deleteRecords()로 수동 삭제 시 갱신

파일 포맷:
  0                    ← 파일 버전
  3                    ← 파티션 수
  my-topic 0 5367851   ← 토픽명 파티션번호 시작오프셋
  my-topic 1 2834012
  my-topic 2 7891234
```

---

## 12. 장애 복구

### 12.1 브로커 재시작 시 복구 흐름

```
브로커 시작
    |
    v
LogManager 초기화
    |
    v
각 log.dirs에 대해:
    |
    v
+-----------------------------------------+
| 체크포인트 파일 읽기                       |
|  - recovery-point-offset-checkpoint     |
|  - log-start-offset-checkpoint          |
+-----------------------------------------+
    |
    v
각 파티션 디렉토리에 대해:
    |
    v
+-----------------------------------------+
| LogLoader.load()                        |
|  1. .swap 파일 존재? → 컴팩션 완료 처리    |
|  2. .cleaned 파일 존재? → 삭제 (미완료)   |
|  3. .deleted 파일 존재? → 삭제            |
|  4. 세그먼트 파일 로드                     |
|  5. recoveryPoint 이후 세그먼트 복구      |
|     - 레코드 검증 (CRC 체크)              |
|     - 인덱스 재구축                       |
|  6. 누락된 인덱스 파일 재생성              |
+-----------------------------------------+
```

### 12.2 손상된 세그먼트 처리

```
복구 중 손상 감지:
  1. RecordBatch의 CRC32 검증 실패
  2. 오프셋 순서 위반 감지
  3. 매직 바이트 불일치

  → 손상된 위치 이후의 데이터는 잘린다(truncate)
  → 팔로워가 리더에서 정상 데이터를 다시 가져온다
```

### 12.3 .swap 파일 복구

```
컴팩션 중 크래시가 발생한 경우:

  시나리오 1: .cleaned 파일만 존재
    → .cleaned 파일 삭제 (컴팩션 미완료, 원본 유지)

  시나리오 2: .swap 파일 존재
    → .swap 파일을 최종 파일로 교체 완료
    → 원본 세그먼트 삭제
```

---

## 13. 설계 결정의 이유 (Why)

### 13.1 왜 파일 시스템에 직접 쓰는가?

Kafka 초기 설계에서 가장 중요한 결정 중 하나는 데이터베이스나 키-밸류 스토어 대신
**OS 파일 시스템과 페이지 캐시에 직접 의존**하는 것이었다.

1. **이중 버퍼링 회피**: 애플리케이션 레벨 캐시 + OS 페이지 캐시의 이중 복사를 피함
2. **GC 압력 감소**: JVM 힙 대신 OS 메모리를 활용하므로 GC 정지 시간이 짧음
3. **재시작 시 웜업 불필요**: OS 페이지 캐시는 프로세스 재시작 후에도 유효
4. **순차 I/O 최적화**: OS의 read-ahead, write-behind 최적화를 자연스럽게 활용

### 13.2 왜 세그먼트로 분할하는가?

하나의 거대한 파일 대신 세그먼트로 분할하는 이유:

1. **효율적 삭제**: 세그먼트 단위로 파일을 삭제하므로 O(1) 연산
2. **mmap 크기 관리**: 인덱스의 mmap 크기를 세그먼트 단위로 제한
3. **병렬 복사**: 세그먼트 단위로 Tiered Storage에 독립적으로 복사
4. **복구 범위 제한**: 크래시 복구 시 마지막 세그먼트만 재검증

### 13.3 왜 인덱스와 데이터를 분리하는가?

인덱스를 데이터 파일에 인라인으로 저장하지 않는 이유:

1. **읽기 효율**: 인덱스만 mmap하면 데이터 전체를 메모리에 올리지 않아도 됨
2. **재구축 가능**: 인덱스가 손상되어도 데이터 파일에서 재구축 가능
3. **독립적 크기 관리**: 인덱스 파일의 최대 크기를 별도로 제한 가능
4. **Zero-copy 지원**: 데이터 파일을 인덱스 없이 직접 네트워크로 전송 가능

### 13.4 왜 프로듀서에서 오프셋을 할당하지 않는가?

리더 브로커가 오프셋을 할당하는 이유:

1. **전역 순서 보장**: 하나의 파티션에 쓰는 모든 프로듀서의 메시지에 대해 일관된 순서
2. **원자적 할당**: `synchronized(lock)` 블록 내에서 순차적으로 오프셋 할당
3. **갭 없는 오프셋**: 모든 오프셋이 연속적 (컴팩션 후 갭 발생 가능하지만 할당 시점에는 없음)

### 13.5 왜 컴팩션에 별도 스레드를 사용하는가?

컴팩션은 I/O 집약적 작업이므로 쓰기/읽기 경로와 분리한다:

1. **쓰기 경로 비간섭**: 컴팩션이 진행 중이어도 프로듀서 쓰기는 영향 없음
2. **스로틀링**: `Throttler`를 통해 컴팩션 I/O를 제한하여 정상 트래픽 보호
3. **중단 가능**: `checkDone` 콜백으로 파티션 변경 시 컴팩션 중단 가능
4. **메모리 격리**: 컴팩션용 OffsetMap 메모리를 별도로 관리

### 13.6 왜 MD5 해시인가?

`SkimpyOffsetMap`이 SHA-256 등의 더 안전한 해시 대신 MD5를 사용하는 이유:

```java
// SkimpyOffsetMap.java
public SkimpyOffsetMap(int memory) throws NoSuchAlgorithmException {
    this(memory, "MD5");  // 기본값: MD5
}
```

1. **속도**: MD5는 SHA-256보다 약 2배 빠름, 컴팩션은 수백만 키를 처리
2. **크기**: MD5 해시(16바이트) < SHA-256(32바이트), 메모리 효율 2배
3. **보안 불필요**: 컴팩션은 보안이 아닌 중복 제거가 목적
4. **충돌 허용**: 충돌 시 이전 값이 보존될 뿐, 데이터 손실 없음

---

## 요약 테이블

| 구성 요소 | 파일 확장자 | 엔트리 크기 | 역할 |
|-----------|-----------|------------|------|
| FileRecords | .log | 가변 (배치) | 실제 레코드 데이터 |
| OffsetIndex | .index | 8바이트 | 오프셋 → 물리적 위치 |
| TimeIndex | .timeindex | 12바이트 | 타임스탬프 → 오프셋 |
| TransactionIndex | .txnindex | 32바이트 | 중단된 트랜잭션 경계 |
| ProducerSnapshot | .snapshot | 가변 | 프로듀서 상태 (멱등성) |

| 설정 | 기본값 | 설명 |
|------|--------|------|
| log.segment.bytes | 1GB | 세그먼트 최대 크기 |
| log.index.interval.bytes | 4096 | 인덱스 엔트리 간격 |
| log.index.size.max.bytes | 10MB | 인덱스 파일 최대 크기 |
| log.retention.hours | 168 (7일) | 시간 기반 보존 기간 |
| log.retention.bytes | -1 | 크기 기반 보존 (무제한) |
| log.cleaner.min.cleanable.ratio | 0.5 | 컴팩션 트리거 더티 비율 |
| log.cleaner.dedupe.buffer.size | 128MB | 컴팩션 OffsetMap 크기 |
| log.roll.ms | null | 시간 기반 세그먼트 롤링 |
| log.roll.jitter.ms | 0 | 롤링 시간 분산 지터 |

---

## 참고 소스 파일 전체 경로

```
storage/src/main/java/org/apache/kafka/storage/internals/log/
  +-- UnifiedLog.java          # 통합 로그 (로컬+원격)
  +-- LocalLog.java            # 로컬 세그먼트 관리
  +-- LogSegment.java          # 단일 세그먼트
  +-- LogSegments.java         # 세그먼트 컬렉션 (NavigableMap)
  +-- OffsetIndex.java         # 오프셋 인덱스
  +-- TimeIndex.java           # 타임스탬프 인덱스
  +-- TransactionIndex.java    # 트랜잭션 인덱스
  +-- AbstractIndex.java       # 인덱스 기반 클래스
  +-- LogFileUtils.java        # 파일명/확장자 유틸리티
  +-- LogManager.java          # 로그 관리자 (보존/삭제)
  +-- LogLoader.java           # 브로커 시작 시 로그 로딩
  +-- LogCleaner.java          # 컴팩션 스레드 관리
  +-- LogCleanerManager.java   # 컴팩션 대상 선정
  +-- Cleaner.java             # 실제 컴팩션 로직
  +-- SkimpyOffsetMap.java     # MD5 기반 OffsetMap
  +-- RollParams.java          # 세그먼트 롤링 파라미터
  +-- LogConfig.java           # 로그 설정
  +-- ProducerStateManager.java # 프로듀서 상태 (멱등성/트랜잭션)
  +-- RemoteIndexCache.java    # 원격 인덱스 캐시

storage/src/main/java/org/apache/kafka/server/log/remote/storage/
  +-- RemoteLogManager.java    # 원격 로그 관리
  +-- RemoteLogManagerConfig.java # 원격 로그 설정
```
