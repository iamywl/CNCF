# 20. Kafka 메트릭/모니터링 시스템 Deep-Dive

## 목차

1. [개요](#1-개요)
2. [왜(Why) 자체 메트릭 시스템을 구축했는가](#2-왜why-자체-메트릭-시스템을-구축했는가)
3. [메트릭 아키텍처 전체 구조](#3-메트릭-아키텍처-전체-구조)
4. [Metrics 레지스트리](#4-metrics-레지스트리)
5. [Sensor 시스템](#5-sensor-시스템)
6. [통계 타입 (Stats)](#6-통계-타입-stats)
7. [KafkaMetric과 MetricConfig](#7-kafkametric과-metricconfig)
8. [JmxReporter - JMX 통합](#8-jmxreporter---jmx-통합)
9. [MetricsReporter 인터페이스](#9-metricsreporter-인터페이스)
10. [브로커 토픽 메트릭 (BrokerTopicStats)](#10-브로커-토픽-메트릭-brokerTopicStats)
11. [Recording Level과 성능 최적화](#11-recording-level과-성능-최적화)
12. [센서 만료와 생명주기](#12-센서-만료와-생명주기)
13. [주요 메트릭 카테고리와 모니터링 가이드](#13-주요-메트릭-카테고리와-모니터링-가이드)
14. [왜(Why) 이렇게 설계했는가](#14-왜why-이렇게-설계했는가)

---

## 1. 개요

Kafka는 Yammer Metrics 라이브러리에서 출발하여 **자체 메트릭 프레임워크**(`org.apache.kafka.common.metrics`)로 진화한 측정/모니터링 시스템을 갖추고 있다. 모든 메트릭은 JMX(Java Management Extensions)를 통해 노출되며, `MetricsReporter` 플러그인을 통해 외부 모니터링 시스템으로 내보낼 수 있다.

```
소스 파일 위치:
  clients/src/main/java/org/apache/kafka/common/metrics/Metrics.java         -- 메트릭 레지스트리
  clients/src/main/java/org/apache/kafka/common/metrics/Sensor.java          -- 센서 (측정 핸들)
  clients/src/main/java/org/apache/kafka/common/metrics/KafkaMetric.java     -- 개별 메트릭
  clients/src/main/java/org/apache/kafka/common/metrics/MetricConfig.java    -- 메트릭 설정
  clients/src/main/java/org/apache/kafka/common/metrics/MetricsReporter.java -- 리포터 인터페이스
  clients/src/main/java/org/apache/kafka/common/metrics/JmxReporter.java     -- JMX 리포터
  clients/src/main/java/org/apache/kafka/common/metrics/stats/Rate.java      -- Rate 통계
  clients/src/main/java/org/apache/kafka/common/metrics/stats/Avg.java       -- 평균 통계
  clients/src/main/java/org/apache/kafka/common/metrics/stats/Max.java       -- 최대값 통계
  clients/src/main/java/org/apache/kafka/common/metrics/stats/Min.java       -- 최소값 통계
  clients/src/main/java/org/apache/kafka/common/metrics/stats/TokenBucket.java -- 토큰 버킷
  clients/src/main/java/org/apache/kafka/common/metrics/stats/Percentiles.java -- 백분위수
  clients/src/main/java/org/apache/kafka/common/metrics/stats/WindowedSum.java -- 윈도우 합계
  storage/src/main/java/org/apache/kafka/storage/log/metrics/BrokerTopicStats.java  -- 브로커 토픽 통계
  storage/src/main/java/org/apache/kafka/storage/log/metrics/BrokerTopicMetrics.java -- 브로커 토픽 메트릭
  server-common/src/main/java/org/apache/kafka/server/metrics/FilteringJmxReporter.java -- 필터링 JMX 리포터
```

### 메트릭 시스템 개념도

```
+------------------+
|   브로커/클라이언트  |
|   코드에서 측정     |
+------------------+
        |
        | sensor.record(value)
        v
+------------------+     +------------------+     +------------------+
|     Sensor       |────→|     Stat          |────→|   KafkaMetric    |
| (측정 핸들)       |     | (Rate, Avg, ...)  |     | (이름+값 쌍)      |
+------------------+     +------------------+     +------------------+
        |                                                |
        | 부모 센서 전파                                    |
        v                                                v
+------------------+                        +------------------+
|  Parent Sensor   |                        |  MetricsReporter |
+------------------+                        |  (JmxReporter 등)|
                                            +------------------+
                                                    |
                                            +-------+-------+
                                            |               |
                                            v               v
                                    +----------+     +-----------+
                                    | JMX MBean|     | Prometheus|
                                    | (기본)    |     | (플러그인)  |
                                    +----------+     +-----------+
```

---

## 2. 왜(Why) 자체 메트릭 시스템을 구축했는가

Kafka 초기에는 Yammer Metrics (Coda Hale Metrics)를 사용했다. 그러나 다음과 같은 이유로 자체 메트릭 프레임워크를 개발했다:

| 문제 | 자체 프레임워크의 해결책 |
|------|----------------------|
| Yammer Metrics 의존성 크기가 큰 클라이언트에 부담 | `clients` 모듈에 포함되는 경량 프레임워크 |
| 쿼터 시스템과의 통합 어려움 | `Sensor.checkQuotas()`로 측정과 쿼터 검사를 통합 |
| 시간 윈도우 기반 Rate 계산 필요 | `SampledStat` 기반의 정밀한 윈도우 관리 |
| 메트릭의 동적 생성/삭제 필요 | `Sensor` 만료 메커니즘으로 자동 정리 |
| Recording Level 제어 | `INFO`, `DEBUG`, `TRACE` 레벨로 오버헤드 조절 |

> Kafka의 메트릭 시스템은 **메트릭 수집**과 **쿼터 시스템**이 같은 `Sensor`/`Metrics` 인프라를 공유하는 것이 핵심 설계 결정이다.

---

## 3. 메트릭 아키텍처 전체 구조

```
+-----------------------------------------------------------------------+
|                         Metrics (레지스트리)                             |
|                                                                       |
|  ┌──────────────────────────────────────────────────────────┐          |
|  │ sensors: ConcurrentMap<String, Sensor>                  │          |
|  │                                                         │          |
|  │  "bytes-sent" ─→ Sensor ─→ [Rate] ─→ KafkaMetric       │          |
|  │                         ─→ [Avg]  ─→ KafkaMetric       │          |
|  │                                                         │          |
|  │  "request-time" ─→ Sensor ─→ [Avg]  ─→ KafkaMetric     │          |
|  │                           ─→ [Max]  ─→ KafkaMetric     │          |
|  │                           ─→ [Percentiles] ─→ KafkaMetric × N │   |
|  └──────────────────────────────────────────────────────────┘          |
|                                                                       |
|  ┌──────────────────────────────────────────────────────────┐          |
|  │ metrics: ConcurrentMap<MetricName, KafkaMetric>         │          |
|  │                                                         │          |
|  │  MetricName("byte-rate", "Produce", {user=..})          │          |
|  │    ─→ KafkaMetric(Rate stat, config with Quota)         │          |
|  └──────────────────────────────────────────────────────────┘          |
|                                                                       |
|  ┌──────────────────────────────────────────────────────────┐          |
|  │ reporters: List<MetricsReporter>                        │          |
|  │                                                         │          |
|  │  [JmxReporter, ...]                                     │          |
|  └──────────────────────────────────────────────────────────┘          |
+-----------------------------------------------------------------------+
```

---

## 4. Metrics 레지스트리

```
소스: clients/src/main/java/org/apache/kafka/common/metrics/Metrics.java
```

`Metrics` 클래스는 센서와 메트릭의 **글로벌 레지스트리**이다. 브로커, 프로듀서, 컨슈머 각각 하나의 `Metrics` 인스턴스를 가진다.

### 핵심 필드

```java
public final class Metrics implements Closeable {
    private final MetricConfig config;                          // 기본 메트릭 설정
    private final ConcurrentMap<MetricName, KafkaMetric> metrics; // 메트릭 이름 → 메트릭 맵
    private final ConcurrentMap<String, Sensor> sensors;         // 센서 이름 → 센서 맵
    private final ConcurrentMap<Sensor, List<Sensor>> childrenSensors; // 부모→자식 센서 관계
    private final List<MetricsReporter> reporters;               // 메트릭 리포터 목록
    private final Time time;
    private final ScheduledThreadPoolExecutor metricsScheduler;  // 센서 만료 스케줄러
}
```

### 생성자

```java
public Metrics(MetricConfig defaultConfig, List<MetricsReporter> reporters,
               Time time, boolean enableExpiration, MetricsContext metricsContext) {
    this.config = defaultConfig;
    this.sensors = new ConcurrentHashMap<>();
    this.metrics = new ConcurrentHashMap<>();
    this.childrenSensors = new ConcurrentHashMap<>();
    this.reporters = Objects.requireNonNull(reporters);
    this.time = time;

    // 리포터 초기화
    for (MetricsReporter reporter : reporters) {
        reporter.contextChange(metricsContext);
        reporter.init(new ArrayList<>());
    }

    // 센서 만료 활성화 시 30초마다 만료된 센서 정리
    if (enableExpiration) {
        this.metricsScheduler = new ScheduledThreadPoolExecutor(1);
        this.metricsScheduler.setThreadFactory(
            runnable -> KafkaThread.daemon("SensorExpiryThread", runnable));
        this.metricsScheduler.scheduleAtFixedRate(
            new ExpireSensorTask(), 30, 30, TimeUnit.SECONDS);
    }

    // 메타 메트릭: 등록된 총 메트릭 수
    addMetric(metricName("count", "kafka-metrics-count", "total number of registered metrics"),
        (config, now) -> metrics.size());
}
```

### MetricName 구조

```java
// MetricName은 (name, group, description, tags)로 구성된다
public MetricName metricName(String name, String group, String description, Map<String, String> tags) {
    Map<String, String> combinedTag = new LinkedHashMap<>(config.tags());
    combinedTag.putAll(tags);
    return new MetricName(name, group, description, combinedTag);
}
```

예시:
```
MetricName("byte-rate", "Produce", "Tracking byte-rate per user/client-id",
           {user="alice", client-id="producer-1"})
```

### 센서 생성

```java
public synchronized Sensor sensor(String name, MetricConfig config,
                                   long inactiveSensorExpirationTimeSeconds,
                                   Sensor.RecordingLevel recordingLevel,
                                   Sensor... parents) {
    Sensor s = getSensor(name);
    if (s == null) {
        s = new Sensor(this, name, parents, config == null ? this.config : config,
                       time, inactiveSensorExpirationTimeSeconds, recordingLevel);
        this.sensors.put(name, s);
        if (parents != null) {
            for (Sensor parent : parents) {
                childrenSensors.computeIfAbsent(parent, k -> new ArrayList<>()).add(s);
            }
        }
    }
    return s;
}
```

---

## 5. Sensor 시스템

```
소스: clients/src/main/java/org/apache/kafka/common/metrics/Sensor.java
```

`Sensor`는 연속적인 수치 값을 수신하여 연관된 `Stat`(통계)에 전달하는 **측정 핸들**이다.

### 핵심 필드

```java
public final class Sensor {
    private final Metrics registry;         // 소속 레지스트리
    private final String name;              // 센서 고유 이름
    private final Sensor[] parents;         // 부모 센서 (값 전파)
    private final List<StatAndConfig> stats; // 등록된 통계 목록
    private final Map<MetricName, KafkaMetric> metrics; // 연관 메트릭
    private final MetricConfig config;      // 기본 설정
    private final Time time;
    private volatile long lastRecordTime;   // 마지막 기록 시각
    private final long inactiveSensorExpirationTimeMs; // 비활성 만료 시간
    private final RecordingLevel recordingLevel; // 기록 레벨
    private final Object metricLock;        // 메트릭 동기화 잠금
}
```

### record() - 값 기록

```java
private void recordInternal(double value, long timeMs, boolean checkQuotas) {
    this.lastRecordTime = timeMs;
    synchronized (this) {
        synchronized (metricLock()) {
            // 모든 등록된 stat에 값 전달
            for (StatAndConfig statAndConfig : this.stats) {
                statAndConfig.stat.record(statAndConfig.config(), value, timeMs);
            }
        }
        // 쿼터 검사 (선택적)
        if (checkQuotas)
            checkQuotas(timeMs);
    }
    // 부모 센서에 값 전파
    for (Sensor parent : parents)
        parent.record(value, timeMs, checkQuotas);
}
```

### checkQuotas() - 쿼터 위반 검사

```java
public void checkQuotas(long timeMs) {
    for (KafkaMetric metric : this.metrics.values()) {
        MetricConfig config = metric.config();
        if (config != null) {
            Quota quota = config.quota();
            if (quota != null) {
                double value = metric.measurableValue(timeMs);
                if (metric.measurable() instanceof TokenBucket) {
                    if (value < 0) {  // TokenBucket: 잔여 토큰 < 0
                        throw new QuotaViolationException(metric, value, quota.bound());
                    }
                } else {
                    if (!quota.acceptable(value)) {  // Rate: 값 > 상한
                        throw new QuotaViolationException(metric, value, quota.bound());
                    }
                }
            }
        }
    }
}
```

### RecordingLevel - 기록 레벨

```java
public enum RecordingLevel {
    INFO(0, "INFO"),    // 기본 메트릭 (항상 기록)
    DEBUG(1, "DEBUG"),  // 디버그 메트릭
    TRACE(2, "TRACE");  // 가장 상세한 메트릭

    public boolean shouldRecord(final int configId) {
        if (configId == INFO.id) return this.id == INFO.id;
        else if (configId == DEBUG.id) return this.id == INFO.id || this.id == DEBUG.id;
        else if (configId == TRACE.id) return true;
    }
}
```

### 부모-자식 센서 관계

```
예: 네트워크 바이트 전송 메트릭

   Sensor("node-bytes-sent")  ← 노드별 메트릭
          |
          | record(value) 호출 시 부모에게도 전파
          v
   Sensor("bytes-sent")       ← 전체 합계 메트릭
```

부모 센서를 통해 **개별 노드 메트릭을 기록하면 자동으로 전체 합계도 업데이트**된다. 별도로 합계를 관리할 필요가 없다.

---

## 6. 통계 타입 (Stats)

```
소스: clients/src/main/java/org/apache/kafka/common/metrics/stats/
```

Kafka는 다양한 통계 구현체를 제공한다:

### SampledStat 기반 (시간 윈도우)

| 클래스 | 설명 | 용도 |
|--------|------|------|
| `Rate` | 시간 윈도우 기반 초당 속도 | byte-rate, request-rate |
| `SimpleRate` | 단순화된 Rate | 복제 쿼터 |
| `Avg` | 시간 윈도우 기반 평균 | request-latency-avg |
| `Max` | 시간 윈도우 내 최대값 | request-latency-max |
| `Min` | 시간 윈도우 내 최소값 | 최소 대기 시간 |
| `WindowedSum` | 시간 윈도우 합계 | 윈도우 내 총 바이트 |
| `WindowedCount` | 시간 윈도우 카운트 | 윈도우 내 요청 수 |
| `Percentiles` | 백분위수 (히스토그램) | p99 지연시간 |

### 누적(Cumulative) 통계

| 클래스 | 설명 | 용도 |
|--------|------|------|
| `CumulativeSum` | 전체 누적 합계 | total-bytes-sent |
| `CumulativeCount` | 전체 누적 카운트 | total-requests |

### 특수 통계

| 클래스 | 설명 | 용도 |
|--------|------|------|
| `TokenBucket` | 토큰 버킷 알고리즘 | 컨트롤러 변경 쿼터 |
| `Frequencies` | 빈도 분포 | 에러 코드 분포 |

### Rate 계산 상세

```
Rate stat의 시간 윈도우 구조:

  ┌────────┬────────┬────────┬────────┬────────┬────────┐
  │ 샘플0  │ 샘플1  │ 샘플2  │ ...    │ 샘플9  │ 현재   │
  │(만료)  │        │        │        │        │ 샘플   │
  └────────┴────────┴────────┴────────┴────────┴────────┘
  ←─────── 10개 완료 윈도우 (10초) ─────────→ ←현재(1초)→

  Rate = 전체합 / 전체시간
       = (샘플1~현재 합) / (현재시간 - 가장 오래된 샘플의 시작시간)

  설정:
    quota.window.num = 11 (10개 완료 + 1개 현재)
    quota.window.size.seconds = 1 (각 윈도우 1초)
```

---

## 7. KafkaMetric과 MetricConfig

### KafkaMetric

```
소스: clients/src/main/java/org/apache/kafka/common/metrics/KafkaMetric.java
```

```java
public final class KafkaMetric implements Metric {
    private final MetricName metricName;           // 메트릭 이름
    private final Object lock;                     // 동기화 잠금
    private final Time time;
    private final MetricValueProvider<?> metricValueProvider; // 값 제공자 (Stat)
    private volatile MetricConfig config;          // 설정 (동적 변경 가능)

    @Override
    public Object metricValue() {
        long now = time.milliseconds();
        synchronized (this.lock) {
            return metricValueProvider.value(config, now);  // 현재 값 계산
        }
    }
}
```

`metricValue()` 호출 시 현재 시간 기준으로 값을 계산한다. Rate의 경우 현재 시간 윈도우의 합계를 시간으로 나누어 계산한다.

### MetricConfig

```java
public class MetricConfig {
    private Quota quota;         // 쿼터 한계값 (null이면 쿼터 없음)
    private int samples;         // 유지할 샘플 수
    private long timeWindowMs;   // 각 샘플의 시간 간격
    private Map<String, String> tags; // 기본 태그
    private Sensor.RecordingLevel recordLevel; // 기록 레벨
}
```

쿼터와 메트릭이 같은 `MetricConfig`를 사용하는 것이 핵심이다. `Sensor.record()` 시 Rate를 계산하고, 같은 설정의 Quota와 비교하여 위반 여부를 판단한다.

---

## 8. JmxReporter - JMX 통합

```
소스: clients/src/main/java/org/apache/kafka/common/metrics/JmxReporter.java
```

`JmxReporter`는 모든 Kafka 메트릭을 **JMX MBean**으로 노출한다.

### MBean 이름 생성 규칙

```java
static String getMBeanName(String prefix, MetricName metricName) {
    StringBuilder mBeanName = new StringBuilder();
    mBeanName.append(prefix);
    mBeanName.append(":type=");
    mBeanName.append(metricName.group());
    for (Map.Entry<String, String> entry : metricName.tags().entrySet()) {
        if (entry.getKey().isEmpty() || entry.getValue().isEmpty()) continue;
        mBeanName.append(",");
        mBeanName.append(entry.getKey());
        mBeanName.append("=");
        mBeanName.append(Sanitizer.jmxSanitize(entry.getValue()));
    }
    return mBeanName.toString();
}
```

생성 예시:
```
MetricName("byte-rate", "Produce", {user="alice", client-id="producer-1"})
→ MBean: "kafka.server:type=Produce,user=alice,client-id=producer-1"
→ Attribute: "byte-rate"
```

### KafkaMbean - DynamicMBean 구현

```java
private static class KafkaMbean implements DynamicMBean {
    private final ObjectName objectName;
    private final Map<String, KafkaMetric> metrics;

    @Override
    public Object getAttribute(String name) throws AttributeNotFoundException {
        if (this.metrics.containsKey(name))
            return this.metrics.get(name).metricValue();  // 실시간 값 반환
        else
            throw new AttributeNotFoundException("Could not find attribute " + name);
    }

    @Override
    public MBeanInfo getMBeanInfo() {
        MBeanAttributeInfo[] attrs = new MBeanAttributeInfo[metrics.size()];
        int i = 0;
        for (Map.Entry<String, KafkaMetric> entry : this.metrics.entrySet()) {
            attrs[i] = new MBeanAttributeInfo(
                entry.getKey(),
                double.class.getName(),
                entry.getValue().metricName().description(),
                true, false, false);  // 읽기 전용
            i++;
        }
        return new MBeanInfo(this.getClass().getName(), "", attrs, null, null, null);
    }
}
```

### 메트릭 변경 이벤트 처리

```java
@Override
public void metricChange(KafkaMetric metric) {
    synchronized (LOCK) {
        String mbeanName = addAttribute(metric);  // MBean에 속성 추가
        if (mbeanName != null && mbeanPredicate.test(mbeanName)) {
            reregister(mbeans.get(mbeanName));     // MBeanServer에 재등록
        }
    }
}

@Override
public void metricRemoval(KafkaMetric metric) {
    synchronized (LOCK) {
        KafkaMbean mbean = removeAttribute(metric, mBeanName);
        if (mbean != null) {
            if (mbean.metrics.isEmpty()) {
                unregister(mbean);           // 속성이 없으면 MBean 삭제
                mbeans.remove(mBeanName);
            } else if (mbeanPredicate.test(mBeanName)) {
                reregister(mbean);           // 속성 변경 반영
            }
        }
    }
}
```

### JMX 필터링

```java
// 런타임에 include/exclude 패턴으로 노출할 MBean 필터링
public static Predicate<String> compilePredicate(Map<String, ?> configs) {
    String include = (String) configs.get(INCLUDE_CONFIG);  // 기본: ".*"
    String exclude = (String) configs.get(EXCLUDE_CONFIG);  // 기본: ""

    Pattern includePattern = Pattern.compile(include);
    Pattern excludePattern = Pattern.compile(exclude);

    return s -> includePattern.matcher(s).matches()
                && !excludePattern.matcher(s).matches();
}
```

설정 예:
```properties
# 특정 메트릭만 JMX에 노출
metrics.jmx.include=kafka.server:type=BrokerTopicMetrics.*
metrics.jmx.exclude=.*FailedProduceRequests.*
```

---

## 9. MetricsReporter 인터페이스

```
소스: clients/src/main/java/org/apache/kafka/common/metrics/MetricsReporter.java
```

```java
public interface MetricsReporter extends Reconfigurable, AutoCloseable {
    // 초기화 시 기존 메트릭 전달
    void init(List<KafkaMetric> metrics);

    // 메트릭이 추가/변경될 때 호출
    void metricChange(KafkaMetric metric);

    // 메트릭이 제거될 때 호출
    void metricRemoval(KafkaMetric metric);

    // 레지스트리 닫힐 때 호출
    void close();

    // 동적 재설정 가능한 설정 키
    default Set<String> reconfigurableConfigs() { return Collections.emptySet(); }

    // 컨텍스트 라벨 설정
    default void contextChange(MetricsContext metricsContext) { }
}
```

### 리포터 등록 흐름

```
Metrics 생성
    │
    ├── reporters.forEach(reporter -> {
    │       reporter.contextChange(metricsContext);
    │       reporter.init(existingMetrics);
    │   })
    │
    └── 이후 메트릭 변경 시:
        ├── addMetric() → reporters.forEach(r -> r.metricChange(metric))
        └── removeMetric() → reporters.forEach(r -> r.metricRemoval(metric))
```

### 커스텀 MetricsReporter 구현 예

```java
public class PrometheusReporter implements MetricsReporter {
    @Override
    public void metricChange(KafkaMetric metric) {
        // Prometheus 형식으로 변환하여 /metrics 엔드포인트에 노출
        String prometheusName = convertToPrometheusName(metric.metricName());
        prometheusRegistry.register(prometheusName, metric::metricValue);
    }
}
```

브로커 설정:
```properties
metric.reporters=com.example.PrometheusReporter
```

---

## 10. 브로커 토픽 메트릭 (BrokerTopicStats)

```
소스: storage/src/main/java/org/apache/kafka/storage/log/metrics/BrokerTopicStats.java
```

`BrokerTopicStats`는 **토픽별 및 전체(all-topics) 메트릭**을 관리한다.

### 구조

```java
public class BrokerTopicStats implements AutoCloseable {
    private final BrokerTopicMetrics allTopicsStats;  // 전체 토픽 합계
    private final ConcurrentMap<String, BrokerTopicMetrics> stats;  // 토픽별 메트릭

    public BrokerTopicMetrics topicStats(String topic) {
        return stats.computeIfAbsent(topic,
            k -> new BrokerTopicMetrics(k, remoteStorageEnabled));
    }
}
```

### 주요 메트릭 (BrokerTopicMetrics)

```
소스: storage/src/main/java/org/apache/kafka/storage/log/metrics/BrokerTopicMetrics.java
```

| 메트릭 이름 | 타입 | 설명 |
|------------|------|------|
| `MessagesInPerSec` | Rate | 초당 수신 메시지 수 |
| `BytesInPerSec` | Rate | 초당 수신 바이트 |
| `BytesOutPerSec` | Rate | 초당 송신 바이트 |
| `BytesRejectedPerSec` | Rate | 초당 거부된 바이트 |
| `FailedProduceRequestsPerSec` | Rate | 초당 실패한 프로듀스 요청 |
| `FailedFetchRequestsPerSec` | Rate | 초당 실패한 페치 요청 |
| `TotalProduceRequestsPerSec` | Rate | 초당 총 프로듀스 요청 |
| `TotalFetchRequestsPerSec` | Rate | 초당 총 페치 요청 |
| `ReplicationBytesInPerSec` | Rate | 초당 복제 수신 바이트 |
| `ReplicationBytesOutPerSec` | Rate | 초당 복제 송신 바이트 |
| `ReassignmentBytesInPerSec` | Rate | 초당 재배치 수신 바이트 |
| `ReassignmentBytesOutPerSec` | Rate | 초당 재배치 송신 바이트 |

### JMX MBean 경로

```
kafka.server:type=BrokerTopicMetrics,name=MessagesInPerSec
kafka.server:type=BrokerTopicMetrics,name=BytesInPerSec,topic=my-topic
```

---

## 11. Recording Level과 성능 최적화

### Recording Level 계층

```
TRACE (모든 메트릭 기록)
  │
  ├── DEBUG (INFO + DEBUG 메트릭)
  │     │
  │     └── INFO (기본 메트릭만)
  │
  └── 설정: metrics.recording.level=INFO
```

### 성능 최적화 기법

1. **조건부 기록**: `shouldRecord()` 검사로 불필요한 계산 회피

```java
public void record(double value) {
    if (shouldRecord()) {          // RecordingLevel 검사
        recordInternal(value, time.milliseconds(), true);
    }
}
```

2. **ConcurrentHashMap 기반 센서 조회**: 센서 이름으로 O(1) 조회
3. **SampledStat의 윈도우 재사용**: 시간 윈도우가 만료되면 가장 오래된 윈도우를 재사용
4. **volatile lastRecordTime**: 센서 만료 검사에만 사용, 잠금 없이 읽기

---

## 12. 센서 만료와 생명주기

### 센서 생명주기

```
1. 생성 (Metrics.sensor())
   └── sensors 맵에 등록
   └── 부모-자식 관계 설정
   └── MetricsReporter에 알림

2. 사용 (Sensor.record())
   └── lastRecordTime 갱신
   └── Stat에 값 전달
   └── 쿼터 검사

3. 만료 (ExpireSensorTask)
   └── 30초마다 실행
   └── lastRecordTime + expirationTime < now 이면 제거

4. 제거 (Metrics.removeSensor())
   └── 자식 센서 먼저 제거 (재귀)
   └── 연관 메트릭 모두 제거
   └── MetricsReporter에 알림
```

### ExpireSensorTask

```java
// Metrics 클래스 내부
class ExpireSensorTask implements Runnable {
    @Override
    public void run() {
        for (Map.Entry<String, Sensor> entry : sensors.entrySet()) {
            Sensor sensor = entry.getValue();
            if (sensor.hasExpired()) {
                removeSensor(entry.getKey());
            }
        }
    }
}
```

### 쿼터 센서의 만료 시간

```java
// ClientQuotaManager에서 쿼터 센서는 1시간 후 만료
private static final int INACTIVE_SENSOR_EXPIRATION_TIME_SECONDS = 3600;

// exempt 센서는 만료되지 않음 (항상 존재)
private static final long DEFAULT_INACTIVE_EXEMPT_SENSOR_EXPIRATION_TIME_SECONDS = Long.MAX_VALUE;
```

---

## 13. 주요 메트릭 카테고리와 모니터링 가이드

### 브로커 핵심 메트릭

| 카테고리 | JMX 경로 | 주요 속성 | 모니터링 의미 |
|----------|---------|----------|-------------|
| 요청 처리 | `kafka.network:type=RequestMetrics` | `RequestsPerSec`, `TotalTimeMs` | API 처리 성능 |
| 네트워크 | `kafka.network:type=SocketServer` | `NetworkProcessorAvgIdlePercent` | 네트워크 스레드 포화도 |
| 요청 핸들러 | `kafka.server:type=KafkaRequestHandlerPool` | `RequestHandlerAvgIdlePercent` | 요청 핸들러 포화도 |
| 토픽 | `kafka.server:type=BrokerTopicMetrics` | `BytesInPerSec`, `BytesOutPerSec` | 처리량 |
| 복제 | `kafka.server:type=ReplicaManager` | `UnderReplicatedPartitions` | 복제 지연 |
| 컨트롤러 | `kafka.controller:type=KafkaController` | `ActiveControllerCount` | 컨트롤러 상태 |
| 로그 | `kafka.log:type=LogManager` | `LogDirectoryOffline` | 디스크 장애 |

### 클라이언트 핵심 메트릭

| 카테고리 | JMX 경로 | 주요 속성 |
|----------|---------|----------|
| 프로듀서 | `kafka.producer:type=producer-metrics` | `record-send-rate`, `record-error-rate` |
| 컨슈머 | `kafka.consumer:type=consumer-fetch-manager-metrics` | `records-consumed-rate`, `fetch-latency-avg` |
| 컨슈머 그룹 | `kafka.consumer:type=consumer-coordinator-metrics` | `commit-latency-avg`, `rebalance-latency-avg` |

### 알림 설정 가이드

```
중요(Critical) 알림:
  - UnderReplicatedPartitions > 0    ← 복제 지연, 데이터 손실 위험
  - ActiveControllerCount != 1       ← 컨트롤러 문제
  - OfflinePartitionsCount > 0       ← 파티션 오프라인

경고(Warning) 알림:
  - RequestHandlerAvgIdlePercent < 0.3  ← 요청 핸들러 과부하
  - NetworkProcessorAvgIdlePercent < 0.3 ← 네트워크 스레드 과부하
  - BytesInPerSec 급증                   ← 트래픽 폭증

참고(Info):
  - MessagesInPerSec                 ← 처리량 트렌드
  - throttle-time (avg)              ← 쿼터 위반 빈도
```

---

## 14. 왜(Why) 이렇게 설계했는가

### Q1: 왜 메트릭과 쿼터가 같은 Sensor를 공유하는가?

메트릭 수집과 쿼터 검사를 분리하면:
- 동일한 값을 두 번 기록해야 함 (오버헤드)
- 두 시스템 간 시간 차이로 정확도 저하
- 코드 중복 증가

`Sensor.record()` 한 번의 호출로 **통계 업데이트 + 쿼터 검사**를 동시에 수행하여, 정확하고 효율적인 제어가 가능하다.

### Q2: 왜 DynamicMBean을 사용하는가?

JMX에 메트릭을 노출할 때 Standard MBean 대신 `DynamicMBean`을 사용하는 이유:
- **메트릭이 런타임에 동적으로 추가/제거됨** → 인터페이스를 미리 정의할 수 없음
- 같은 MBean ObjectName 아래에 여러 속성을 동적으로 관리 가능
- `getMBeanInfo()`가 현재 등록된 메트릭을 반영하여 속성 목록을 동적으로 생성

### Q3: 왜 SampledStat의 시간 윈도우를 사용하는가?

전체 기간의 평균(moving average)을 사용하면:
- 과거의 오래된 데이터가 현재 값에 과도하게 영향
- 최근 버스트를 감지하기 어려움

시간 윈도우 기반 샘플링은:
- 최근 10초(기본) 이내의 데이터만으로 Rate를 계산
- 버스트를 빠르게 감지하여 쿼터 위반 판단
- 오래된 데이터는 자동으로 윈도우에서 제외

### Q4: 왜 센서에 부모-자식 관계가 있는가?

네트워크 메트릭을 예로 들면:
- 노드별 센서: `node-{id}-bytes-sent` → 개별 브로커로 보낸 바이트
- 전체 센서: `bytes-sent` → 모든 브로커로 보낸 총 바이트

부모-자식 관계를 사용하면 노드별 센서에 값을 기록할 때 **자동으로 전체 센서에도 전파**된다. 별도의 합산 로직이 필요 없다.

### Q5: 왜 MetricsReporter를 플러그인으로 설계했는가?

모니터링 인프라는 조직마다 다르다:
- Prometheus, Datadog, InfluxDB, CloudWatch 등
- JMX만으로는 모든 환경을 지원할 수 없음
- `MetricsReporter` 플러그인으로 **메트릭 생산(Kafka)과 소비(모니터링)를 분리**

Kafka는 JMX를 기본으로 제공하되, 플러그인 아키텍처로 어떤 모니터링 시스템이든 통합 가능하게 했다.

---

## 참고 자료

- KIP-45: Metrics Improvements
- KIP-714: Client metrics and observability
- 소스코드: `clients/src/main/java/org/apache/kafka/common/metrics/`
- Kafka 공식 문서: Monitoring 섹션
