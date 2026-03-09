# 19. Kafka 쿼터(Quota) 관리 시스템 Deep-Dive

## 목차

1. [개요](#1-개요)
2. [왜(Why) 쿼터 시스템이 필요한가](#2-왜why-쿼터-시스템이-필요한가)
3. [쿼터 타입과 계층 구조](#3-쿼터-타입과-계층-구조)
4. [핵심 클래스 구조](#4-핵심-클래스-구조)
5. [ClientQuotaManager 동작 원리](#5-clientquotamanager-동작-원리)
6. [쿼터 매칭 우선순위](#6-쿼터-매칭-우선순위)
7. [쓰로틀링 메커니즘](#7-쓰로틀링-메커니즘)
8. [Rate 계산과 시간 윈도우](#8-rate-계산과-시간-윈도우)
9. [ReplicationQuotaManager](#9-replicationquotamanager)
10. [ControllerMutationQuotaManager](#10-controllermutationquotamanager)
11. [쿼터 설정과 동적 변경](#11-쿼터-설정과-동적-변경)
12. [KRaft 모드에서의 쿼터 관리](#12-kraft-모드에서의-쿼터-관리)
13. [왜(Why) 이렇게 설계했는가](#13-왜why-이렇게-설계했는가)

---

## 1. 개요

Kafka의 쿼터 시스템은 **멀티테넌트(multi-tenant) 환경**에서 특정 클라이언트가 브로커 자원을 독점하지 못하도록 제한하는 메커니즘이다. 프로듀서의 쓰기 속도, 컨슈머의 읽기 속도, 요청 처리 시간, 컨트롤러 변경 빈도를 제어한다.

```
소스 파일 위치:
  server/src/main/java/org/apache/kafka/server/quota/ClientQuotaManager.java      -- 클라이언트 쿼터 핵심
  server/src/main/java/org/apache/kafka/server/quota/ControllerMutationQuotaManager.java -- 컨트롤러 변경 쿼터
  server/src/main/java/org/apache/kafka/server/quota/ThrottledChannel.java         -- 쓰로틀 채널
  server/src/main/java/org/apache/kafka/server/quota/QuotaType.java               -- 쿼터 타입 열거
  server-common/src/main/java/org/apache/kafka/server/quota/QuotaUtils.java        -- 쓰로틀 시간 계산
  server-common/src/main/java/org/apache/kafka/server/config/QuotaConfig.java      -- 쿼터 설정
  core/src/main/java/kafka/server/QuotaFactory.java                                -- 쿼터 매니저 팩토리
  core/src/main/java/kafka/server/ClientRequestQuotaManager.java                   -- 요청 쿼터 매니저
  core/src/main/java/kafka/server/ReplicationQuotaManager.java                     -- 복제 쿼터 매니저
  metadata/src/main/java/org/apache/kafka/controller/ClientQuotaControlManager.java -- KRaft 쿼터 컨트롤러
  clients/src/main/java/org/apache/kafka/common/metrics/Sensor.java                -- 메트릭 센서
  clients/src/main/java/org/apache/kafka/common/metrics/Quota.java                 -- 쿼터 한계값
```

### 쿼터 시스템 전체 구조

```
+----------------------------------------------------------+
|                   클라이언트 요청                            |
|         (Produce / Fetch / Admin / Request)               |
+----------------------------------------------------------+
                         |
                         v
+----------------------------------------------------------+
|              KafkaApis / ControllerApis                   |
|   요청 처리 시 쿼터 매니저에 사용량 기록                       |
+----------------------------------------------------------+
          |              |              |              |
          v              v              v              v
+------------+  +------------+  +------------+  +-------------+
| Produce    |  | Fetch      |  | Request    |  | Controller  |
| QuotaMgr   |  | QuotaMgr   |  | QuotaMgr   |  | Mutation    |
|            |  |            |  |            |  | QuotaMgr    |
| byte-rate  |  | byte-rate  |  | request-%  |  | token-bucket|
+------------+  +------------+  +------------+  +-------------+
     |               |              |                |
     v               v              v                v
+----------------------------------------------------------+
|                   Sensor + Rate                          |
|           시간 윈도우 기반 사용량 추적                       |
+----------------------------------------------------------+
     |
     v  (Quota Violation 발생 시)
+----------------------------------------------------------+
|            ThrottledChannel + DelayQueue                  |
|         쓰로틀 시간 계산 → 채널 뮤트 → 자동 해제             |
+----------------------------------------------------------+
```

---

## 2. 왜(Why) 쿼터 시스템이 필요한가

### 문제 상황

멀티테넌트 Kafka 클러스터에서는 다음과 같은 상황이 발생할 수 있다:

1. **하나의 프로듀서가 네트워크 대역폭 독점**: 대량 배치 전송으로 다른 클라이언트의 쓰기 지연
2. **특정 컨슈머가 디스크 I/O 독점**: 대량 페치로 브로커의 페이지 캐시 오염
3. **관리 작업의 과도한 요청**: 토픽 대량 생성/삭제로 컨트롤러 과부하
4. **요청 처리 시간 독점**: 복잡한 요청이 브로커 스레드를 오래 점유

### 설계 목표

| 목표 | 구현 방식 |
|------|----------|
| 공정한 자원 분배 | user/client-id별 독립 쿼터 |
| 유연한 정책 적용 | 계층적 쿼터 매칭 (user > client-id > default) |
| 동적 변경 | 브로커 재시작 없이 쿼터 변경 가능 |
| 저오버헤드 | 시간 윈도우 기반 샘플링으로 효율적 추적 |
| 확장성 | 커스텀 쿼터 콜백 지원 |

---

## 3. 쿼터 타입과 계층 구조

### QuotaType 열거형

```
소스: server-common/src/main/java/org/apache/kafka/server/quota/QuotaType.java
```

```java
public enum QuotaType {
    FETCH("Fetch"),                              // 컨슈머 읽기 속도 (bytes/sec)
    PRODUCE("Produce"),                          // 프로듀서 쓰기 속도 (bytes/sec)
    REQUEST("Request"),                          // 요청 처리 시간 (%)
    CONTROLLER_MUTATION("ControllerMutation"),    // 컨트롤러 변경 속도 (mutations/sec)
    LEADER_REPLICATION("LeaderReplication"),      // 리더 복제 속도 (bytes/sec)
    FOLLOWER_REPLICATION("FollowerReplication"),  // 팔로워 복제 속도 (bytes/sec)
    ALTER_LOG_DIRS_REPLICATION("AlterLogDirsReplication"), // 로그 디렉토리 변경 속도
    RLM_COPY("RLMCopy"),                         // 원격 로그 복사 속도
    RLM_FETCH("RLMFetch");                       // 원격 로그 페치 속도
}
```

### 쿼터 타입 분류

```
+--------------------+     +---------------------+     +----------------------+
|  클라이언트 쿼터      |     |  복제 쿼터            |     |  관리 쿼터             |
+--------------------+     +---------------------+     +----------------------+
| PRODUCE            |     | LEADER_REPLICATION  |     | CONTROLLER_MUTATION  |
| FETCH              |     | FOLLOWER_REPLICATION|     |                      |
| REQUEST            |     | ALTER_LOG_DIRS_REP  |     |                      |
+--------------------+     +---------------------+     +----------------------+
        |                          |                            |
        v                          v                            v
  ClientQuotaManager      ReplicationQuotaManager     ControllerMutationQuotaManager
  (Rate 기반)              (Rate 기반)                (TokenBucket 기반)
```

### 쿼터 설정 키 (QuotaConfig)

```
소스: server-common/src/main/java/org/apache/kafka/server/config/QuotaConfig.java
```

| 설정 키 | 설명 | 기본값 |
|---------|------|--------|
| `producer_byte_rate` | 프로듀서 바이트 전송 속도 상한 (bytes/sec) | Long.MAX_VALUE |
| `consumer_byte_rate` | 컨슈머 바이트 수신 속도 상한 (bytes/sec) | Long.MAX_VALUE |
| `request_percentage` | 요청 처리 시간 상한 (%) | Integer.MAX_VALUE |
| `controller_mutation_rate` | 컨트롤러 변경 속도 상한 (mutations/sec) | Integer.MAX_VALUE |
| `connection_creation_rate` | IP별 연결 생성 속도 상한 | Integer.MAX_VALUE |

---

## 4. 핵심 클래스 구조

### 클래스 상속 및 구성 관계

```
                  ClientQuotaManager
                  /          \
                 /            \
   ClientRequestQuotaManager  ControllerMutationQuotaManager
   (REQUEST 쿼터)              (CONTROLLER_MUTATION 쿼터)


   ReplicationQuotaManager (독립 클래스 - ReplicaQuota 인터페이스)


   QuotaFactory.QuotaManagers (모든 쿼터 매니저를 묶는 레코드)
     ├── fetch: ClientQuotaManager        (FETCH)
     ├── produce: ClientQuotaManager      (PRODUCE)
     ├── request: ClientRequestQuotaManager (REQUEST)
     ├── controllerMutation: ControllerMutationQuotaManager
     ├── leader: ReplicationQuotaManager  (LEADER_REPLICATION)
     ├── follower: ReplicationQuotaManager (FOLLOWER_REPLICATION)
     └── alterLogDirs: ReplicationQuotaManager (ALTER_LOG_DIRS)
```

### QuotaFactory - 쿼터 매니저 생성

```
소스: core/src/main/java/kafka/server/QuotaFactory.java
```

`QuotaFactory.instantiate()` 메서드에서 모든 쿼터 매니저를 한 번에 생성한다:

```java
public static QuotaManagers instantiate(KafkaConfig cfg, Metrics metrics,
                                         Time time, String threadNamePrefix, String role) {
    Optional<Plugin<ClientQuotaCallback>> callbackPlugin = createClientQuotaCallback(cfg, metrics, role);
    return new QuotaManagers(
        new ClientQuotaManager(clientConfig(cfg), metrics, QuotaType.FETCH, time, threadNamePrefix, callbackPlugin),
        new ClientQuotaManager(clientConfig(cfg), metrics, QuotaType.PRODUCE, time, threadNamePrefix, callbackPlugin),
        new ClientRequestQuotaManager(clientConfig(cfg), metrics, time, threadNamePrefix, callbackPlugin),
        new ControllerMutationQuotaManager(clientControllerMutationConfig(cfg), metrics, time, threadNamePrefix, callbackPlugin),
        new ReplicationQuotaManager(replicationConfig(cfg), metrics, QuotaType.LEADER_REPLICATION, time),
        new ReplicationQuotaManager(replicationConfig(cfg), metrics, QuotaType.FOLLOWER_REPLICATION, time),
        new ReplicationQuotaManager(alterLogDirsReplicationConfig(cfg), metrics, QuotaType.ALTER_LOG_DIRS_REPLICATION, time),
        callbackPlugin
    );
}
```

### 핵심 엔티티 모델

```
소스: server/src/main/java/org/apache/kafka/server/quota/ClientQuotaManager.java
```

`ClientQuotaManager` 내부에서 쿼터 대상(엔티티)을 다음과 같이 모델링한다:

```java
// 사용자 엔티티
public record UserEntity(String sanitizedUser) implements ClientQuotaEntity.ConfigEntity { ... }

// 클라이언트 ID 엔티티
public record ClientIdEntity(String clientId) implements ClientQuotaEntity.ConfigEntity { ... }

// 복합 쿼터 엔티티 (user + client-id)
public record KafkaQuotaEntity(ClientQuotaEntity.ConfigEntity userEntity,
                                ClientQuotaEntity.ConfigEntity clientIdEntity)
                                implements ClientQuotaEntity { ... }
```

---

## 5. ClientQuotaManager 동작 원리

### 초기화

```
소스: server/src/main/java/org/apache/kafka/server/quota/ClientQuotaManager.java
```

`ClientQuotaManager` 생성자에서 다음을 초기화한다:

1. **Metrics 인스턴스**: 메트릭 레지스트리 참조
2. **SensorAccess**: 센서 생성/조회를 위한 스레드 안전 래퍼
3. **DelayQueue**: 쓰로틀된 채널을 관리하는 지연 큐
4. **ThrottledChannelReaper**: 쓰로틀 만료된 채널의 뮤트를 해제하는 데몬 스레드
5. **DefaultQuotaCallback 또는 CustomQuotaCallback**: 쿼터 값 해석 전략

```java
public ClientQuotaManager(ClientQuotaManagerConfig config, Metrics metrics,
                           QuotaType quotaType, Time time, String threadNamePrefix,
                           Optional<Plugin<ClientQuotaCallback>> clientQuotaCallbackPlugin) {
    this.config = config;
    this.metrics = metrics;
    this.quotaType = quotaType;
    this.time = time;
    this.sensorAccessor = new SensorAccess(lock, metrics);
    this.clientQuotaType = QuotaType.toClientQuotaType(quotaType);
    this.quotaTypesEnabled = clientQuotaCallbackPlugin.isPresent() ? CUSTOM_QUOTAS : NO_QUOTAS;
    this.delayQueueSensor = metrics.sensor(quotaType + "-delayQueue");
    this.throttledChannelReaper = new ThrottledChannelReaper(delayQueue, threadNamePrefix);
    this.quotaCallback = clientQuotaCallbackPlugin.map(Plugin::get).orElse(new DefaultQuotaCallback());
    start();  // ThrottledChannelReaper 스레드 시작
}
```

### 사용량 기록과 쓰로틀 시간 계산

요청 처리 시 `recordAndGetThrottleTimeMs()` 메서드가 호출된다:

```java
public int recordAndGetThrottleTimeMs(Session session, String clientId, double value, long timeMs) {
    var clientSensors = getOrCreateQuotaSensors(session, clientId);
    try {
        clientSensors.quotaSensor().record(value, timeMs, true);  // checkQuotas=true
        return 0;  // 쿼터 이내 → 쓰로틀 없음
    } catch (QuotaViolationException e) {
        var throttleTimeMs = (int) throttleTime(e, timeMs);
        return throttleTimeMs;  // 쿼터 초과 → 쓰로틀 시간 반환
    }
}
```

**핵심 흐름:**

```
recordAndGetThrottleTimeMs()
    │
    ├── getOrCreateQuotaSensors(session, clientId)
    │       │
    │       ├── quotaCallback.quotaMetricTags()  ← 메트릭 태그 결정
    │       ├── sensorAccessor.getOrCreate()     ← 쿼터 센서 생성/조회
    │       └── sensorAccessor.getOrCreate()     ← 쓰로틀 시간 센서
    │
    ├── quotaSensor.record(value, timeMs, checkQuotas=true)
    │       │
    │       ├── stat.record()  ← Rate 통계에 값 기록
    │       └── checkQuotas()  ← Quota.acceptable(value) 검사
    │              │
    │              └── QuotaViolationException 발생 (value > bound)
    │
    └── throttleTime(e, timeMs)  ← 쓰로틀 시간 계산
            │
            └── QuotaUtils.throttleTime()
```

---

## 6. 쿼터 매칭 우선순위

Kafka는 계층적 쿼터 시스템을 제공한다. 하나의 클라이언트 연결에 대해 **가장 구체적인(most specific)** 쿼터가 적용된다.

### 우선순위 (높은 것부터)

```
1. /config/users/<user>/clients/<client-id>    ← 가장 구체적
2. /config/users/<user>/clients/<default>
3. /config/users/<user>
4. /config/users/<default>/clients/<client-id>
5. /config/users/<default>/clients/<default>
6. /config/users/<default>
7. /config/clients/<client-id>
8. /config/clients/<default>                    ← 가장 일반적
```

### DefaultQuotaCallback의 쿼터 조회 로직

```
소스: server/src/main/java/org/apache/kafka/server/quota/ClientQuotaManager.java (내부 클래스 DefaultQuotaCallback)
```

```java
private Quota findQuota(String sanitizedUser, String clientId,
                         UserEntity userEntity, ClientIdEntity clientIdEntity) {
    if (!sanitizedUser.isEmpty() && !clientId.isEmpty()) {
        return findUserClientQuota(userEntity, clientIdEntity);  // user+client-id 쿼터 검색
    }
    if (!sanitizedUser.isEmpty()) {
        return findUserQuota(userEntity);  // user 쿼터 검색
    }
    if (!clientId.isEmpty()) {
        return findClientQuota(clientIdEntity);  // client-id 쿼터 검색
    }
    return null;
}
```

`findUserClientQuota`는 4단계 폴백으로 쿼터를 검색한다:

```java
private Quota findUserClientQuota(UserEntity userEntity, ClientIdEntity clientIdEntity) {
    // 1단계: /config/users/<user>/clients/<client-id>
    var quota = overriddenQuotas.get(new KafkaQuotaEntity(userEntity, clientIdEntity));
    if (quota != null) return quota;

    // 2단계: /config/users/<user>/clients/<default>
    quota = overriddenQuotas.get(new KafkaQuotaEntity(userEntity, DEFAULT_USER_CLIENT_ID));
    if (quota != null) return quota;

    // 3단계: /config/users/<default>/clients/<client-id>
    quota = overriddenQuotas.get(new KafkaQuotaEntity(DEFAULT_USER_ENTITY, clientIdEntity));
    if (quota != null) return quota;

    // 4단계: /config/users/<default>/clients/<default>
    return overriddenQuotas.get(DEFAULT_USER_CLIENT_ID_QUOTA_ENTITY);
}
```

### 쿼터 타입 활성화 비트마스크

`ClientQuotaManager`는 현재 어떤 수준의 쿼터가 활성화되어 있는지를 비트마스크로 추적한다:

```java
public static final int NO_QUOTAS = 0;
public static final int CLIENT_ID_QUOTA_ENABLED = 1;
public static final int USER_QUOTA_ENABLED = 2;
public static final int USER_CLIENT_ID_QUOTA_ENABLED = 4;
public static final int CUSTOM_QUOTAS = 8;
```

단일 수준의 쿼터만 활성화된 경우, 메트릭 태그 결정을 최적화할 수 있다:

```
quotaTypesEnabled = 1 (CLIENT_ID만): userTag="", clientIdTag=clientId
quotaTypesEnabled = 2 (USER만):       userTag=sanitizedUser, clientIdTag=""
quotaTypesEnabled = 4 (USER+CLIENT):  userTag=sanitizedUser, clientIdTag=clientId
기타 복합 조합:                        overriddenQuotas 맵을 순회하며 검색
```

---

## 7. 쓰로틀링 메커니즘

### ThrottledChannel

```
소스: server/src/main/java/org/apache/kafka/server/quota/ThrottledChannel.java
```

쿼터 위반 시 채널을 일정 시간 동안 **뮤트(mute)**한다:

```java
public class ThrottledChannel implements Delayed {
    private final Time time;
    private final int throttleTimeMs;
    private final ThrottleCallback callback;
    private final long endTimeNanos;

    public ThrottledChannel(Time time, int throttleTimeMs, ThrottleCallback callback) {
        this.time = time;
        this.throttleTimeMs = throttleTimeMs;
        this.callback = callback;
        this.endTimeNanos = time.nanoseconds() + TimeUnit.MILLISECONDS.toNanos(throttleTimeMs);
        callback.startThrottling();  // 쓰로틀 시작 → 채널 뮤트
    }

    public void notifyThrottlingDone() {
        callback.endThrottling();  // 쓰로틀 종료 → 채널 언뮤트
    }

    @Override
    public long getDelay(TimeUnit unit) {
        return unit.convert(endTimeNanos - time.nanoseconds(), TimeUnit.NANOSECONDS);
    }
}
```

### ThrottledChannelReaper

`ClientQuotaManager` 내부 스레드로, `DelayQueue`에서 만료된 `ThrottledChannel`을 꺼내 언뮤트한다:

```java
public class ThrottledChannelReaper extends ShutdownableThread {
    @Override
    public void doWork() {
        ThrottledChannel throttledChannel;
        try {
            throttledChannel = delayQueue.poll(1, TimeUnit.SECONDS);
            if (throttledChannel != null) {
                delayQueueSensor.record(-1);
                throttledChannel.notifyThrottlingDone();  // 채널 언뮤트
            }
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
        }
    }
}
```

### 쓰로틀 흐름 다이어그램

```
┌─────────┐    요청    ┌──────────┐    사용량 기록    ┌──────────────────┐
│ Client  │──────────→│ KafkaApis │─────────────────→│ ClientQuotaManager│
└─────────┘           └──────────┘                   └──────────────────┘
                           │                                │
                           │         throttleTimeMs > 0     │
                           │←───────────────────────────────┘
                           │
                           ├── 1. throttle() 호출
                           │     ├── ThrottledChannel 생성
                           │     ├── callback.startThrottling()  ← 채널 뮤트
                           │     └── delayQueue.add(throttledChannel)
                           │
                           ├── 2. 응답에 throttleTimeMs 포함하여 전송
                           │
                           └── 3. 클라이언트는 throttleTimeMs만큼 대기 후 재시도

                     (백그라운드 스레드)
┌──────────────────────────┐
│ ThrottledChannelReaper   │
│   delayQueue.poll()      │──→ 만료 시 callback.endThrottling()  ← 채널 언뮤트
└──────────────────────────┘
```

---

## 8. Rate 계산과 시간 윈도우

### 쓰로틀 시간 계산 공식

```
소스: server-common/src/main/java/org/apache/kafka/server/quota/QuotaUtils.java
```

관측된 Rate(O)가 목표 Rate(T)를 초과했을 때, 윈도우 크기(W) 내에서 Rate를 정상으로 되돌리기 위한 지연 시간(X)을 계산한다:

```
O * W / (W + X) = T
→ X = (O - T) / T * W
```

Java 구현:

```java
public static long throttleTime(QuotaViolationException e, long timeMs) {
    double difference = e.value() - e.bound();
    double throttleTimeMs = difference / e.bound() * windowSize(e.metric(), timeMs);
    return Math.round(throttleTimeMs);
}
```

### 시간 윈도우 설정

```
소스: server-common/src/main/java/org/apache/kafka/server/config/QuotaConfig.java
```

| 설정 | 기본값 | 설명 |
|------|--------|------|
| `quota.window.num` | 11 | 메모리에 유지할 샘플 수 (10개 완료 + 1개 현재) |
| `quota.window.size.seconds` | 1 | 각 샘플의 시간 간격 (초) |
| `replication.quota.window.num` | 11 | 복제 쿼터 샘플 수 |
| `replication.quota.window.size.seconds` | 1 | 복제 쿼터 샘플 시간 간격 |
| `controller.quota.window.num` | 11 | 컨트롤러 쿼터 샘플 수 |
| `controller.quota.window.size.seconds` | 1 | 컨트롤러 쿼터 샘플 시간 간격 |

### Rate 통계와 MetricConfig

쿼터 센서가 생성될 때 `MetricConfig`에 쿼터 한계값과 시간 윈도우를 설정한다:

```java
private MetricConfig getQuotaMetricConfig(double quotaLimit) {
    return new MetricConfig()
            .timeWindow(config.quotaWindowSizeSeconds(), TimeUnit.SECONDS)
            .samples(config.numQuotaSamples())
            .quota(new Quota(quotaLimit, true));  // isUpperBound=true
}
```

`Sensor.record()` 호출 시:
1. `Rate` stat이 현재 시간 윈도우에 값을 누적
2. `checkQuotas()`에서 현재 Rate를 계산
3. Rate > quota.bound() 이면 `QuotaViolationException` 발생

### maxValueInQuotaWindow

한 번의 요청에서 쓰로틀 없이 전송 가능한 최대 바이트 수:

```java
public double maxValueInQuotaWindow(Session session, String clientId) {
    if (!quotasEnabled()) return Double.MAX_VALUE;
    var clientSensors = getOrCreateQuotaSensors(session, clientId);
    var limit = quotaCallback.quotaLimit(clientQuotaType, clientSensors.metricTags());
    if (limit != null)
        return limit * (config.numQuotaSamples() - 1) * config.quotaWindowSizeSeconds();
    return Double.MAX_VALUE;
}
```

이 값은 **페치 요청의 최대 바이트를 결정**하는 데 사용된다. 쿼터가 1MB/s이고 윈도우가 10초면, 한 번에 최대 10MB까지 페치 가능하다.

---

## 9. ReplicationQuotaManager

```
소스: core/src/main/java/kafka/server/ReplicationQuotaManager.java
```

복제 쿼터는 클라이언트 쿼터와 달리 **토픽 파티션 단위**로 쓰로틀을 적용한다. 파티션 재배치(reassignment) 시 네트워크 대역폭을 제한하는 데 주로 사용된다.

### 핵심 메서드

```java
public class ReplicationQuotaManager implements ReplicaQuota {
    private final ConcurrentHashMap<String, List<Integer>> throttledPartitions;

    // 쿼터 초과 여부 확인
    @Override
    public boolean isQuotaExceeded() {
        try {
            sensor().checkQuotas();
            return false;
        } catch (QuotaViolationException qve) {
            return true;
        }
    }

    // 특정 파티션이 쓰로틀 대상인지 확인
    @Override
    public boolean isThrottled(TopicPartition topicPartition) {
        List<Integer> partitions = throttledPartitions.get(topicPartition.topic());
        return partitions != null &&
               (partitions.equals(ALL_REPLICAS) || partitions.contains(topicPartition.partition()));
    }

    // 사용량 기록 (쿼터 무시)
    @Override
    public void record(long value) {
        sensor().record((double) value, time.milliseconds(), false);
    }
}
```

### 클라이언트 쿼터와의 차이

| 항목 | ClientQuotaManager | ReplicationQuotaManager |
|------|-------------------|------------------------|
| 쓰로틀 단위 | user/client-id | 토픽 파티션 |
| 쓰로틀 방식 | 채널 뮤트 + 지연 응답 | Fetch 크기 제한 |
| 쿼터 초과 시 | ThrottledChannel 생성 | isQuotaExceeded()=true → 페치 중단 |
| 동적 설정 | 8단계 계층적 매칭 | 브로커/토픽 수준 설정 |

---

## 10. ControllerMutationQuotaManager

```
소스: server/src/main/java/org/apache/kafka/server/quota/ControllerMutationQuotaManager.java
```

컨트롤러 변경 쿼터는 **TokenBucket 알고리즘**을 사용한다. Rate 기반 쿼터와 달리, 버스트를 허용하되 장기적으로 설정된 속도를 초과하지 못하게 한다.

### TokenBucket vs Rate

```
Rate 기반 (Produce/Fetch/Request):
  ┌─────────────────────────────────┐
  │ 시간 윈도우 내 평균 속도를 계산     │
  │ 초과 시 쓰로틀 시간 계산 후 지연    │
  └─────────────────────────────────┘

TokenBucket 기반 (ControllerMutation):
  ┌─────────────────────────────────┐
  │ 토큰이 일정 속도로 충전됨          │
  │ 요청 시 토큰 소비                  │
  │ 토큰 < 0 이면 즉시 거부           │
  │ 복구까지 대기 시간 = -value/rate   │
  └─────────────────────────────────┘
```

### Strict vs Permissive 쿼터

```java
public ControllerMutationQuota newQuotaFor(Session session, RequestHeader header,
                                            short strictSinceVersion) {
    if (header.apiVersion() >= strictSinceVersion)
        return newStrictQuotaFor(session, header);  // 즉시 거부
    return newPermissiveQuotaFor(session, header.clientId());  // 쓰로틀만 적용
}
```

- **StrictControllerMutationQuota**: 쿼터 초과 시 `THROTTLING_QUOTA_EXCEEDED` 에러 반환
- **PermissiveControllerMutationQuota**: 쿼터 초과 시 요청은 처리하되 응답에 쓰로틀 시간 포함

### 쓰로틀 시간 계산 (TokenBucket)

```java
public static long throttleTimeMs(QuotaViolationException e) {
    if (e.metric().measurable() instanceof TokenBucket) {
        return Math.round(-e.value() / e.bound() * 1000);
        // value < 0 이므로 결과는 양수
        // 예: value=-5, bound=10 → throttleTime = 500ms
    }
    throw new IllegalArgumentException("Not a TokenBucket metric");
}
```

---

## 11. 쿼터 설정과 동적 변경

### 쿼터 업데이트 흐름

```java
// ClientQuotaManager.updateQuota()
public void updateQuota(Optional<ConfigEntity> userEntity,
                        Optional<ConfigEntity> clientEntity,
                        Optional<Quota> quota) {
    lock.writeLock().lock();  // 동시성 보호
    try {
        var quotaEntity = new KafkaQuotaEntity(userEntity.orElse(null), clientEntity.orElse(null));

        if (quota.isPresent()) {
            updateQuotaTypes(quotaEntity, true);  // 비트마스크 업데이트
            quotaCallback.updateQuota(clientQuotaType, quotaEntity, quota.get().bound());
        } else {
            updateQuotaTypes(quotaEntity, false);
            quotaCallback.removeQuota(clientQuotaType, quotaEntity);
        }

        updateQuotaMetricConfigs(updatedEntity);  // 기존 메트릭에 새 쿼터 적용
    } finally {
        lock.writeLock().unlock();
    }
}
```

### 메트릭 설정 업데이트 최적화

단일 수준의 쿼터만 활성화된 경우, 영향받는 메트릭 하나만 업데이트한다 (O(1)):

```java
public void updateQuotaMetricConfigs(Optional<KafkaQuotaEntity> updatedQuotaEntity) {
    var singleUpdate = switch (quotaTypesEnabled) {
        case NO_QUOTAS, CLIENT_ID_QUOTA_ENABLED, USER_QUOTA_ENABLED,
             USER_CLIENT_ID_QUOTA_ENABLED -> updatedQuotaEntity.isPresent();
        default -> false;
    };

    if (singleUpdate) {
        // 단일 메트릭만 업데이트 (최적화 경로)
        var metric = allMetrics.get(quotaMetricName);
        if (metric != null) metric.config(getQuotaMetricConfig(newQuota));
    } else {
        // 모든 메트릭 순회하며 업데이트 (O(n))
        allMetrics.forEach((metricName, metric) -> { ... });
    }
}
```

### CLI를 통한 쿼터 설정

```bash
# user별 프로듀서 쿼터 설정 (10MB/s)
kafka-configs.sh --alter --entity-type users --entity-name user1 \
  --add-config 'producer_byte_rate=10485760'

# client-id별 컨슈머 쿼터 설정 (5MB/s)
kafka-configs.sh --alter --entity-type clients --entity-name my-consumer \
  --add-config 'consumer_byte_rate=5242880'

# user+client-id 조합 요청 쿼터 설정 (50%)
kafka-configs.sh --alter --entity-type users --entity-name user1 \
  --entity-type clients --entity-name my-producer \
  --add-config 'request_percentage=50'

# 기본 쿼터 설정 (모든 사용자)
kafka-configs.sh --alter --entity-type users --entity-default \
  --add-config 'producer_byte_rate=1048576'
```

---

## 12. KRaft 모드에서의 쿼터 관리

```
소스: metadata/src/main/java/org/apache/kafka/controller/ClientQuotaControlManager.java
```

KRaft 모드에서는 쿼터 설정이 `__cluster_metadata` 토픽에 저장된다. `ClientQuotaControlManager`가 쿼터 변경 요청을 검증하고 레코드를 생성한다.

### 쿼터 변경 처리 흐름

```
클라이언트 AlterClientQuotas 요청
    │
    v
ControllerApis.handleAlterClientQuotas()
    │
    v
ClientQuotaControlManager.alterClientQuotas()
    │
    ├── 1. 엔티티 유효성 검증 (user/client-id/ip)
    ├── 2. 쿼터 키 유효성 검증 (producer_byte_rate 등)
    ├── 3. 값 범위 검증
    ├── 4. ClientQuotaRecord 생성
    │
    v
__cluster_metadata 토픽에 레코드 기록
    │
    v
DynamicClientQuotaPublisher → BrokerServer
    │
    v
각 브로커의 ClientQuotaManager.updateQuota() 호출
```

### 쿼터 데이터 저장 구조

```java
// Timeline 기반 데이터 구조 (KRaft 스냅샷 지원)
final TimelineHashMap<ClientQuotaEntity, TimelineHashMap<String, Double>> clientQuotaData;

// 예시 데이터:
// {user=user1} → {producer_byte_rate=10485760.0, consumer_byte_rate=5242880.0}
// {client-id=my-producer} → {request_percentage=50.0}
// {user=<default>} → {producer_byte_rate=1048576.0}
```

---

## 13. 왜(Why) 이렇게 설계했는가

### Q1: 왜 Rate 기반과 TokenBucket 기반을 구분했는가?

**Produce/Fetch 쿼터 (Rate 기반):**
- 네트워크 대역폭 제한이 목적
- 초과 시 지연만 발생, 요청 자체는 처리됨
- 일시적 버스트를 어느 정도 허용

**ControllerMutation 쿼터 (TokenBucket 기반):**
- 컨트롤러 부하 보호가 목적
- 토픽 100개 동시 생성 같은 버스트를 즉시 차단해야 함
- Strict 모드에서는 `THROTTLING_QUOTA_EXCEEDED` 에러로 즉시 거부

### Q2: 왜 8단계 계층적 쿼터 매칭인가?

멀티테넌트 환경에서 다양한 수준의 정책이 필요하기 때문이다:

```
                              세분화 높음
/config/users/alice/clients/producer-1  ← 특정 사용자의 특정 앱에만 적용
/config/users/alice/clients/<default>   ← 특정 사용자의 모든 앱에 적용
/config/users/alice                     ← 특정 사용자에 적용
/config/users/<default>/clients/ingester ← 모든 사용자의 특정 앱에 적용
/config/users/<default>/clients/<default> ← 모든 사용자+앱의 기본값
/config/users/<default>                 ← 모든 사용자의 기본값
/config/clients/producer-1              ← 특정 앱에 적용
/config/clients/<default>               ← 모든 앱의 기본값
                              세분화 낮음
```

### Q3: 왜 센서(Sensor)를 1시간 후 만료시키는가?

```java
private static final int INACTIVE_SENSOR_EXPIRATION_TIME_SECONDS = 3600;
```

- 수천 개의 클라이언트가 연결/해제를 반복하면 센서가 무한히 증가
- 1시간 이상 비활성인 센서는 자동 정리하여 메모리 누수 방지
- 재연결 시 새 센서가 생성되므로 기능적 영향 없음

### Q4: 왜 unrecordQuotaSensor가 필요한가?

```java
public void unrecordQuotaSensor(Session session, String clientId, double value, long timeMs) {
    var clientSensors = getOrCreateQuotaSensors(session, clientId);
    clientSensors.quotaSensor().record(value * -1, timeMs, false);
}
```

Fetch 쿼터에서 쓰로틀이 발생하면 브로커는 빈 응답을 반환한다. 이미 기록된 값을 되돌려야(unrecord) 실제 전송하지 않은 바이트가 쿼터에 반영되지 않는다. Rate stat은 시간 윈도우별 합계를 유지하므로, 음수 값을 기록하면 원래 값으로 돌아간다.

### Q5: 왜 쓰로틀 시 채널 뮤트를 사용하는가?

단순히 응답을 지연시키는 것만으로는 클라이언트가 새 요청을 계속 보낼 수 있다. 채널을 뮤트하면:

1. 셀렉터가 해당 채널의 READ 이벤트를 무시
2. 쓰로틀 기간 동안 새 요청을 읽지 않음
3. ThrottledChannelReaper가 만료 시 언뮤트하여 정상 처리 재개

이 방식으로 브로커가 과부하된 클라이언트의 요청을 효과적으로 차단할 수 있다.

---

## 참고 자료

- KIP-13: Quota Management
- KIP-124: Request rate quotas
- KIP-599: ControllerMutation quotas
- 소스코드: `server/src/main/java/org/apache/kafka/server/quota/`
