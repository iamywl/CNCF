# 23. 성능 벤치마크 시스템

> Kafka의 내장 성능 측정 도구 — ProducerPerformance, ConsumerPerformance, EndToEndLatency

---

## 1. 개요

Kafka는 처리량(throughput)과 지연시간(latency)이 핵심 성능 지표이다.
이를 체계적으로 측정하기 위한 내장 벤치마크 도구를 제공한다.

| 도구 | 소스 위치 | 측정 대상 |
|------|----------|----------|
| ProducerPerformance | `tools/.../ProducerPerformance.java` | 프로듀서 처리량, 지연시간, 백분위 |
| ConsumerPerformance | `tools/.../ConsumerPerformance.java` | 컨슈머 처리량, 메시지/초 |
| EndToEndLatency | `tools/.../EndToEndLatency.java` | 프로듀서→컨슈머 전구간 지연시간 |

### 왜 내장 벤치마크가 필요한가?

1. **일관된 측정**: 동일한 메트릭 정의로 버전 간 비교 가능
2. **프로덕션 검증**: 실제 Kafka 프로토콜을 사용하여 실제 성능 측정
3. **튜닝 가이드**: 설정 변경의 효과를 정량적으로 확인
4. **CI/CD 통합**: 자동화된 성능 회귀 테스트

---

## 2. ProducerPerformance 아키텍처

### 2.1 전체 구조

**소스 위치**: `tools/src/main/java/org/apache/kafka/tools/ProducerPerformance.java`

```
┌───────────────────────────────────────────────────────────┐
│                    ProducerPerformance                      │
│                                                            │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────────┐ │
│  │ConfigPost    │  │ KafkaProducer│  │ ThroughputThrottler│ │
│  │ Processor    │──▶│ <byte[],    │  │ (처리량 제한)     │ │
│  │(CLI 파싱/    │  │  byte[]>     │  └──────────────────┘ │
│  │ 유효성검증)  │  └──────┬───────┘                       │
│  └──────────────┘         │                               │
│                           │ send()                         │
│                    ┌──────▼───────┐                        │
│                    │ PerfCallback  │                        │
│                    │ (지연시간 측정)│                        │
│                    └──────┬───────┘                        │
│                           │ record()                       │
│                    ┌──────▼───────┐                        │
│                    │    Stats     │                        │
│                    │ (통계 수집/  │                        │
│                    │  백분위 계산) │                        │
│                    └─────────────┘                         │
└───────────────────────────────────────────────────────────┘
```

### 2.2 핵심 실행 루프

```java
void start(String[] args) throws IOException {
    ConfigPostProcessor config = new ConfigPostProcessor(parser, args);
    KafkaProducer<byte[], byte[]> producer = createKafkaProducer(config.producerProps);

    if (config.transactionsEnabled)
        producer.initTransactions();

    byte[] payload = null;
    if (config.recordSize != null) {
        payload = new byte[config.recordSize];
    }
    SplittableRandom random = new SplittableRandom(0);

    stats = new Stats(config.numRecords, config.reportingInterval, false);
    long startMs = System.currentTimeMillis();
    ThroughputThrottler throttler = new ThroughputThrottler(config.throughput, startMs);

    for (long i = 0; i < config.numRecords; i++) {
        payload = generateRandomPayload(config.recordSize,
            config.payloadByteList, payload, random,
            config.payloadMonotonic, i);

        // 트랜잭션 관리
        if (config.transactionsEnabled && currentTransactionSize == 0) {
            producer.beginTransaction();
            transactionStartTime = System.currentTimeMillis();
        }

        record = new ProducerRecord<>(config.topicName, payload);
        long sendStartMs = System.currentTimeMillis();
        cb = new PerfCallback(sendStartMs, payload.length, stats, steadyStateStats);
        producer.send(record, cb);

        // 트랜잭션 커밋 (시간 기반)
        if (config.transactionsEnabled &&
            config.transactionDurationMs <= (sendStartMs - transactionStartTime)) {
            producer.commitTransaction();
            currentTransactionSize = 0;
        }

        // 스로틀링
        if (throttler.shouldThrottle(i, sendStartMs)) {
            throttler.throttle();
        }
    }

    stats.printTotal();
}
```

### 2.3 Stats 클래스 — 통계 수집 엔진

```java
static class Stats {
    private final long start;
    private final int[] latencies;   // 지연시간 샘플링 배열
    private final long sampling;     // 샘플링 비율
    private long count;              // 전체 레코드 수
    private long bytes;              // 전체 바이트 수
    private int maxLatency;          // 최대 지연시간
    private long totalLatency;       // 총 지연시간 합
    private long windowCount;        // 현재 윈도우 레코드 수
    private int windowMaxLatency;    // 현재 윈도우 최대 지연시간
    private long windowTotalLatency; // 현재 윈도우 총 지연시간
    private long windowBytes;        // 현재 윈도우 바이트 수
    private long windowStart;        // 현재 윈도우 시작 시간

    public Stats(long numRecords, long reportingInterval, boolean isSteadyState) {
        this.start = System.currentTimeMillis();
        this.sampling = numRecords / Math.min(numRecords, 500000);
        this.latencies = new int[(int)(numRecords / this.sampling) + 1];
        this.reportingInterval = reportingInterval;
    }
}
```

**왜 샘플링을 사용하는가?**

수백만 개의 레코드를 전송할 때 모든 지연시간을 저장하면 메모리가 부족해진다.
`sampling = numRecords / min(numRecords, 500000)`으로 최대 50만 개까지만
샘플링하여 메모리 사용을 제한하면서도 정확한 백분위를 계산한다.

### 2.4 지연시간 기록 메커니즘

```java
public void record(int latency, int bytes, long time) {
    this.count++;
    this.bytes += bytes;
    this.totalLatency += latency;
    this.maxLatency = Math.max(this.maxLatency, latency);

    // 윈도우 통계
    this.windowCount++;
    this.windowBytes += bytes;
    this.windowTotalLatency += latency;
    this.windowMaxLatency = Math.max(windowMaxLatency, latency);

    // 샘플링된 지연시간 저장
    if (this.iteration % this.sampling == 0) {
        this.latencies[index] = latency;
        this.index++;
    }

    // 주기적 보고
    if (time - windowStart >= reportingInterval) {
        printWindow();
        newWindow();
    }
    this.iteration++;
}
```

### 2.5 백분위 계산

```java
private static int[] percentiles(int[] latencies, int count,
                                  double... percentiles) {
    int size = Math.min(count, latencies.length);
    Arrays.sort(latencies, 0, size);  // 정렬
    int[] values = new int[percentiles.length];
    for (int i = 0; i < percentiles.length; i++) {
        int index = (int)(percentiles[i] * size);
        values[i] = latencies[index];
    }
    return values;
}
```

출력 형식:
```
1000000 records sent, 500000.000000 records/sec (47.68 MB/sec),
  2.50 ms avg latency, 350.00 ms max latency,
  1 ms 50th, 5 ms 95th, 20 ms 99th, 100 ms 99.9th.
```

### 2.6 PerfCallback — 비동기 지연시간 측정

```java
static final class PerfCallback implements Callback {
    private final long start;
    private final int bytes;
    private final Stats stats;

    public void onCompletion(RecordMetadata metadata, Exception exception) {
        long now = System.currentTimeMillis();
        int latency = (int)(now - start);
        if (exception == null) {
            this.stats.record(latency, bytes, now);
        }
        if (exception != null)
            exception.printStackTrace();
    }
}
```

**왜 Callback에서 지연시간을 측정하는가?**

`producer.send()`는 비동기이다. 실제 지연시간은 브로커가 ACK를 보내는
시점에 측정해야 정확하다. Callback의 `onCompletion`은 ACK를 받은 후
호출되므로 **end-to-end 전송 지연시간**을 정확히 측정한다.

---

## 3. 페이로드 생성 전략

### 3.1 세 가지 모드

```java
static byte[] generateRandomPayload(Integer recordSize,
        List<byte[]> payloadByteList, byte[] payload,
        SplittableRandom random, boolean payloadMonotonic,
        long recordValue) {

    if (!payloadByteList.isEmpty()) {
        // 1. 파일 기반: 실제 데이터 패턴 재현
        payload = payloadByteList.get(random.nextInt(payloadByteList.size()));
    } else if (recordSize != null) {
        // 2. 고정 크기 랜덤: 처리량 테스트
        for (int j = 0; j < payload.length; ++j)
            payload[j] = (byte)(random.nextInt(26) + 65);
    } else if (payloadMonotonic) {
        // 3. 단조 증가: 순서 검증
        payload = Long.toString(recordValue).getBytes(StandardCharsets.UTF_8);
    }
    return payload;
}
```

| 모드 | 옵션 | 용도 |
|------|------|------|
| 고정 크기 랜덤 | `--record-size 100` | 처리량 벤치마크 (일반적) |
| 파일 기반 | `--payload-file data.txt` | 실제 데이터 패턴 시뮬레이션 |
| 단조 증가 | `--payload-monotonic` | 순서 보존 검증 |

**왜 SplittableRandom을 사용하는가?**

`java.util.Random`은 스레드 안전하지만 경합(contention)이 발생한다.
`SplittableRandom`은 스레드 안전하지 않지만 단일 스레드에서 훨씬 빠르다.
벤치마크 도구에서 랜덤 생성 자체가 병목이 되면 안 되기 때문이다.

---

## 4. ThroughputThrottler

### 4.1 처리량 제한 메커니즘

```java
// server/src/main/java/org/apache/kafka/server/util/ThroughputThrottler.java
public class ThroughputThrottler {
    private final long startMs;
    private final double targetThroughput;  // 목표 records/sec

    public boolean shouldThrottle(long currentCount, long sendStartMs) {
        if (targetThroughput < 0) return false;  // -1이면 무제한
        float elapsedMs = sendStartMs - startMs;
        return (currentCount / elapsedMs) > (targetThroughput / 1000.0);
    }

    public void throttle() {
        // Thread.sleep()으로 전송 속도 조절
    }
}
```

**왜 스로틀링이 필요한가?**

1. **공정한 비교**: 고정 처리량에서의 지연시간 비교가 의미 있음
2. **프로덕션 시뮬레이션**: 실제 워크로드는 무제한 전송이 아님
3. **브로커 과부하 방지**: 벤치마크가 클러스터를 망가뜨리면 안 됨

---

## 5. 트랜잭션 벤치마크

### 5.1 트랜잭션 활성화

```java
// ConfigPostProcessor에서 트랜잭션 설정
this.transactionsEnabled = transactionDurationMsArg != null
    || transactionIdArg != null
    || producerProps.containsKey(ProducerConfig.TRANSACTIONAL_ID_CONFIG);

if (transactionsEnabled) {
    String transactionId = Optional.ofNullable(transactionIdArg)
        .orElse(txIdInProps.orElse(
            DEFAULT_TRANSACTION_ID_PREFIX + Uuid.randomUuid().toString()));
    producerProps.put(ProducerConfig.TRANSACTIONAL_ID_CONFIG, transactionId);
}
```

### 5.2 시간 기반 트랜잭션 커밋

```java
// 기본 3초마다 커밋
if (config.transactionsEnabled &&
    config.transactionDurationMs <= (sendStartMs - transactionStartTime)) {
    producer.commitTransaction();
    currentTransactionSize = 0;
}
```

**왜 시간 기반 커밋인가?**

레코드 수 기반보다 시간 기반이 더 현실적이다. 프로덕션에서 트랜잭션은
보통 "마이크로배치 윈도우"로 관리되며, 이는 시간 기반이다.

---

## 6. Warmup과 Steady-State 측정

### 6.1 워밍업 기간

```java
if (config.warmupRecords > 0) {
    System.out.println("Warmup first " + config.warmupRecords + " records.");
}

// 워밍업 기간이 끝나면 별도의 Stats 인스턴스 생성
if ((isSteadyState = config.warmupRecords > 0) && i == config.warmupRecords) {
    steadyStateStats = new Stats(
        config.numRecords - config.warmupRecords,
        config.reportingInterval, isSteadyState);
    stats.suppressPrinting();  // 워밍업 통계 출력 중단
}
```

**왜 워밍업이 필요한가?**

1. **JVM JIT 컴파일**: 초기 인터프리터 모드는 느림
2. **커넥션 설정**: 첫 요청에 TCP 핸드셰이크 비용
3. **배칭 안정화**: RecordAccumulator가 안정 상태에 도달하기까지 시간 필요
4. **GC 안정화**: 초기 힙 확장과 GC 패턴 안정화

---

## 7. 윈도우 기반 보고

### 7.1 주기적 통계 출력

```java
public void printWindow() {
    long elapsed = System.currentTimeMillis() - windowStart;
    double recsPerSec = 1000.0 * windowCount / (double)elapsed;
    double mbPerSec = 1000.0 * windowBytes / (double)elapsed / (1024.0 * 1024.0);
    System.out.printf(
        "%d records sent, %.1f records/sec (%.2f MB/sec), " +
        "%.1f ms avg latency, %.1f ms max latency.%n",
        windowCount, recsPerSec, mbPerSec,
        windowTotalLatency / (double)windowCount,
        (double)windowMaxLatency);
}
```

출력 예시:
```
50000 records sent, 50000.0 records/sec (47.68 MB/sec), 2.5 ms avg latency, 50.0 ms max latency.
50000 records sent, 52000.0 records/sec (49.59 MB/sec), 2.3 ms avg latency, 45.0 ms max latency.
```

### 7.2 최종 통계 출력

```java
public void printTotal() {
    long elapsed = System.currentTimeMillis() - start;
    double recsPerSec = 1000.0 * count / (double)elapsed;
    double mbPerSec = 1000.0 * bytes / (double)elapsed / (1024.0 * 1024.0);
    int[] percs = percentiles(latencies, index, 0.5, 0.95, 0.99, 0.999);
    System.out.printf(
        "%d records sent, %f records/sec (%.2f MB/sec), " +
        "%.2f ms avg latency, %.2f ms max latency, " +
        "%d ms 50th, %d ms 95th, %d ms 99th, %d ms 99.9th.%n",
        count, recsPerSec, mbPerSec,
        totalLatency / (double)count, (double)maxLatency,
        percs[0], percs[1], percs[2], percs[3]);
}
```

---

## 8. ConsumerPerformance

### 8.1 컨슈머 벤치마크

```java
// tools/src/main/java/org/apache/kafka/tools/ConsumerPerformance.java
// 핵심 메트릭:
// - data.consumed.in.MB: 소비한 데이터 총량 (MB)
// - MB.sec: 초당 소비 처리량 (MB/s)
// - data.consumed.in.nMsg: 소비한 메시지 수
// - nMsg.sec: 초당 메시지 소비 수
```

### 8.2 EndToEndLatency

```java
// tools/src/main/java/org/apache/kafka/tools/EndToEndLatency.java
// 프로듀서 → 브로커 → 컨슈머 전체 경로의 지연시간 측정
// 동기식으로 1건씩 전송하여 정확한 지연시간 측정
```

---

## 9. 벤치마크 실행 가이드

### 9.1 기본 처리량 테스트

```bash
# 100만 개 레코드, 100바이트, 무제한 처리량
kafka-producer-perf-test.sh \
  --topic test-topic \
  --num-records 1000000 \
  --record-size 100 \
  --throughput -1 \
  --producer-props bootstrap.servers=localhost:9092
```

### 9.2 지연시간 프로파일링

```bash
# 10만 개, 1KB, 5000 records/sec으로 제한
kafka-producer-perf-test.sh \
  --topic test-topic \
  --num-records 100000 \
  --record-size 1024 \
  --throughput 5000 \
  --producer-props bootstrap.servers=localhost:9092 \
    acks=all linger.ms=0
```

### 9.3 트랜잭션 성능 테스트

```bash
kafka-producer-perf-test.sh \
  --topic test-topic \
  --num-records 100000 \
  --record-size 100 \
  --throughput -1 \
  --transactional-id perf-test-tx \
  --transaction-duration-ms 1000 \
  --producer-props bootstrap.servers=localhost:9092
```

### 9.4 튜닝 파라미터별 효과

| 파라미터 | 처리량 효과 | 지연시간 효과 |
|----------|-----------|-------------|
| `batch.size` ↑ | ↑ 증가 | ↑ 증가 |
| `linger.ms` ↑ | ↑ 증가 | ↑ 증가 |
| `acks=0` | ↑↑ 최대 | ↓↓ 최소 |
| `acks=1` | ↑ 양호 | ↓ 낮음 |
| `acks=all` | → 보통 | ↑ 높음 |
| `compression.type=lz4` | ↑ 네트워크 절약 | ↑ CPU 사용 |
| `buffer.memory` ↑ | ↑ 안정성 | → 변화 없음 |

---

## 10. 정리

### 벤치마크 시스템의 핵심 설계

| 설계 | 이유 |
|------|------|
| 지연시간 샘플링 (50만 상한) | 메모리 효율성 |
| Callback 기반 측정 | 비동기 전송의 정확한 지연시간 |
| 윈도우 기반 보고 | 시간에 따른 성능 변화 관찰 |
| 워밍업 분리 | JIT/GC 안정화 후 순수 성능 측정 |
| SplittableRandom | 벤치마크 도구 자체의 오버헤드 최소화 |
| 시간 기반 트랜잭션 | 현실적인 트랜잭션 패턴 시뮬레이션 |

---

*소스 참조: tools/src/main/java/org/apache/kafka/tools/ProducerPerformance.java, ConsumerPerformance.java, EndToEndLatency.java*
