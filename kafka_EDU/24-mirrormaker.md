# 24. MirrorMaker 2.0 (MM2) — 크로스 클러스터 복제

> Kafka Connect 기반의 멀티 클러스터 복제 프레임워크

---

## 1. 개요

MirrorMaker 2.0(MM2)은 Kafka 클러스터 간 데이터, 설정, ACL, 컨슈머 그룹 상태를
양방향으로 복제하는 프레임워크이다.

**소스 위치**: `connect/mirror/src/main/java/org/apache/kafka/connect/mirror/`

### 왜 MM2가 필요한가?

| 요구사항 | 설명 |
|----------|------|
| 재해 복구(DR) | 주 클러스터 장애 시 백업 클러스터로 페일오버 |
| 지리적 분산 | 글로벌 데이터센터 간 데이터 동기화 |
| 클러스터 마이그레이션 | 새 클러스터로 무중단 이관 |
| 데이터 집계 | 여러 로컬 클러스터의 데이터를 중앙 집계 |

### MM1 vs MM2

| 항목 | MM1 (레거시) | MM2 |
|------|-------------|-----|
| 아키텍처 | 단순 컨슈머→프로듀서 | Kafka Connect 기반 |
| 오프셋 동기화 | 불가 | 자동 (checkpoint) |
| ACL 동기화 | 불가 | 자동 |
| 설정 동기화 | 불가 | 자동 |
| 양방향 복제 | 수동 구성 | 네이티브 지원 |
| Exactly-Once | 불가 | 지원 (READ_COMMITTED) |

---

## 2. 아키텍처

### 2.1 전체 구조

```
┌─────────────────────────────────────────────────────────┐
│                     MirrorMaker                          │
│                                                          │
│  ┌─────────────────────────────────────────────────────┐ │
│  │              MirrorMakerConfig                       │ │
│  │  clusters = primary, backup                          │ │
│  │  primary.bootstrap.servers = vip1:9092               │ │
│  │  backup.bootstrap.servers = vip2:9092                │ │
│  │  primary->backup.enabled = true                      │ │
│  │  backup->primary.enabled = true                      │ │
│  └─────────────────────────────────────────────────────┘ │
│                                                          │
│  ┌─────────────────┐  ┌─────────────────┐               │
│  │ Herder           │  │ Herder           │              │
│  │ (primary→backup) │  │ (backup→primary) │              │
│  │                  │  │                  │              │
│  │ ┌──────────────┐│  │ ┌──────────────┐│               │
│  │ │MirrorSource  ││  │ │MirrorSource  ││               │
│  │ │ Connector    ││  │ │ Connector    ││               │
│  │ └──────────────┘│  │ └──────────────┘│               │
│  │ ┌──────────────┐│  │ ┌──────────────┐│               │
│  │ │MirrorCheckpt ││  │ │MirrorCheckpt ││               │
│  │ │ Connector    ││  │ │ Connector    ││               │
│  │ └──────────────┘│  │ └──────────────┘│               │
│  │ ┌──────────────┐│  │ ┌──────────────┐│               │
│  │ │MirrorHeart   ││  │ │MirrorHeart   ││               │
│  │ │ beat Connctr ││  │ │ beat Connctr ││               │
│  │ └──────────────┘│  │ └──────────────┘│               │
│  └─────────────────┘  └─────────────────┘               │
└─────────────────────────────────────────────────────────┘
```

### 2.2 세 가지 커넥터

| 커넥터 | 역할 | 복제 대상 |
|--------|------|----------|
| MirrorSourceConnector | 데이터 복제 | 토픽 데이터, 설정, ACL |
| MirrorCheckpointConnector | 오프셋 동기화 | 컨슈머 그룹 오프셋 |
| MirrorHeartbeatConnector | 연결 상태 감시 | heartbeat 토픽 |

```java
public static final List<Class<?>> CONNECTOR_CLASSES = List.of(
    MirrorSourceConnector.class,
    MirrorHeartbeatConnector.class,
    MirrorCheckpointConnector.class
);
```

---

## 3. MirrorMaker 진입점

### 3.1 초기화 흐름

```java
public MirrorMaker(MirrorMakerConfig config, List<String> clusters, Time time) {
    this.config = config;

    // REST 서버 초기화 (내부 통신용)
    if (config.enableInternalRest()) {
        this.restClient = new RestClient(config);
        internalServer = new MirrorRestServer(config.originals(), restClient);
        internalServer.initializeServer();
        this.advertisedUrl = internalServer.advertisedUrl();
    }

    // 대상 클러스터 필터링
    if (clusters != null && !clusters.isEmpty()) {
        this.clusters = new HashSet<>(clusters);
    } else {
        this.clusters = config.clusters();
    }

    // 소스→타겟 페어별 Herder 생성
    Set<SourceAndTarget> herderPairs = config.clusterPairs().stream()
        .filter(x -> this.clusters.contains(x.target()))
        .collect(Collectors.toSet());
    herderPairs.forEach(this::addHerder);
}
```

**왜 Herder를 페어별로 생성하는가?**

각 `source→target` 복제 흐름은 독립적인 Connect 런타임이 필요하다.
Herder는 Connect의 코디네이터 역할로, 커넥터 배포와 태스크 할당을 관리한다.
페어별로 격리하면 한 복제 흐름의 장애가 다른 흐름에 영향을 주지 않는다.

### 3.2 addHerder — Herder 생성의 복잡성

```java
private void addHerder(SourceAndTarget sourceAndTarget) {
    Map<String, String> workerProps = config.workerConfig(sourceAndTarget);
    Plugins plugins = new Plugins(workerProps);
    plugins.compareAndSwapWithDelegatingLoader();

    DistributedConfig distributedConfig = new DistributedConfig(workerProps);
    String kafkaClusterId = distributedConfig.kafkaClusterId();

    // 공유 AdminClient
    SharedTopicAdmin sharedAdmin = new SharedTopicAdmin(adminProps);

    // 오프셋/상태/설정 백킹 스토어
    KafkaOffsetBackingStore offsetBackingStore = ...;
    StatusBackingStore statusBackingStore = ...;
    ConfigBackingStore configBackingStore = ...;

    // Worker 생성
    Worker worker = new Worker(workerId, time, plugins,
        distributedConfig, offsetBackingStore, clientConfigOverridePolicy);

    // MirrorHerder 생성 (DistributedHerder의 특화 버전)
    Herder herder = new MirrorHerder(config, sourceAndTarget,
        distributedConfig, time, worker, kafkaClusterId,
        statusBackingStore, configBackingStore,
        advertisedUrl.toString(), restClient,
        clientConfigOverridePolicy, restNamespace, sharedAdmin);

    herders.put(sourceAndTarget, herder);
}
```

---

## 4. MirrorSourceConnector — 데이터 복제의 핵심

### 4.1 토픽 파티션 발견

```java
List<TopicPartition> findSourceTopicPartitions()
        throws InterruptedException, ExecutionException {
    Set<String> topics = listTopics(sourceAdminClient).stream()
        .filter(this::shouldReplicateTopic)
        .collect(Collectors.toSet());
    return describeTopics(sourceAdminClient, topics).stream()
        .flatMap(MirrorSourceConnector::expandTopicDescription)
        .collect(Collectors.toList());
}
```

### 4.2 토픽 필터링 로직

```java
boolean shouldReplicateTopic(String topic) {
    return (topicFilter.shouldReplicateTopic(topic)
            || (heartbeatsReplicationEnabled
                && replicationPolicy.isHeartbeatsTopic(topic)))
        && !replicationPolicy.isInternalTopic(topic)
        && !isCycle(topic);  // 순환 복제 방지
}
```

### 4.3 순환 감지 (Cycle Detection)

```java
boolean isCycle(String topic) {
    String source = replicationPolicy.topicSource(topic);
    if (source == null) {
        return false;  // 원본 토픽
    } else if (source.equals(sourceAndTarget.target())) {
        return true;   // 타겟에서 온 토픽 → 순환!
    } else {
        String upstreamTopic = replicationPolicy.upstreamTopic(topic);
        if (upstreamTopic == null || upstreamTopic.equals(topic)) {
            return false;
        }
        return isCycle(upstreamTopic);  // 재귀적 상위 추적
    }
}
```

**왜 순환 감지가 필요한가?**

양방향 복제(A↔B)에서 A의 토픽이 B에 복제되면 `B.topicName`이 된다.
이 복제본이 다시 A로 복제되면 무한 루프가 발생한다.
`isCycle`은 토픽 이름에서 소스 클러스터를 추출하여 재귀적으로 순환을 감지한다.

### 4.4 라운드로빈 태스크 분배

```java
@Override
public List<Map<String, String>> taskConfigs(int maxTasks) {
    int numTasks = Math.min(maxTasks, knownSourceTopicPartitions.size());
    List<List<TopicPartition>> roundRobinByTask = new ArrayList<>(numTasks);
    for (int i = 0; i < numTasks; i++) {
        roundRobinByTask.add(new ArrayList<>());
    }

    int count = 0;
    for (TopicPartition partition : knownSourceTopicPartitions) {
        int index = count % numTasks;
        roundRobinByTask.get(index).add(partition);
        count++;
    }
    // ...
}
```

**왜 라운드로빈인가?**

토픽마다 트래픽과 파티션 수가 다르다. 단순히 토픽 단위로 태스크를 할당하면
불균형이 발생한다. 라운드로빈으로 파티션 수준에서 분배하면 태스크 간 부하가
균등해지고, Connect 워커 간 분산도 개선된다.

### 4.5 토픽 설정 동기화

```java
void syncTopicConfigs()
        throws InterruptedException, ExecutionException {
    Map<String, Config> sourceConfigs =
        describeTopicConfigs(topicsBeingReplicated());
    Map<String, Config> targetConfigs = sourceConfigs.entrySet().stream()
        .collect(Collectors.toMap(
            x -> formatRemoteTopic(x.getKey()),
            x -> targetConfig(x.getValue(), true)));
    incrementalAlterConfigs(targetConfigs);
}
```

### 4.6 ACL 동기화

```java
void syncTopicAcls()
        throws InterruptedException, ExecutionException {
    Optional<Collection<AclBinding>> rawBindings = listTopicAclBindings();
    List<AclBinding> filteredBindings = rawBindings.get().stream()
        .filter(x -> x.pattern().resourceType() == ResourceType.TOPIC)
        .filter(x -> x.pattern().patternType() == PatternType.LITERAL)
        .filter(this::shouldReplicateAcl)
        .filter(x -> shouldReplicateTopic(x.pattern().name()))
        .map(this::targetAclBinding)
        .collect(Collectors.toList());
    updateTopicAcls(filteredBindings);
}
```

**왜 WRITE ACL은 복제하지 않는가?**

```java
boolean shouldReplicateAcl(AclBinding aclBinding) {
    return !(aclBinding.entry().permissionType() == AclPermissionType.ALLOW
        && aclBinding.entry().operation() == AclOperation.WRITE);
}
```

미러링된 토픽에 쓰기 권한을 복제하면, 사용자가 실수로 미러 토픽에 직접 쓸 수 있다.
이는 데이터 불일치를 유발한다. READ 권한만 복제하여 미러는 읽기 전용으로 유지한다.

### 4.7 Exactly-Once 지원

```java
@Override
public ExactlyOnceSupport exactlyOnceSupport(Map<String, String> props) {
    return consumerUsesReadCommitted(props)
            ? ExactlyOnceSupport.SUPPORTED
            : ExactlyOnceSupport.UNSUPPORTED;
}
```

소스 컨슈머가 `isolation.level=read_committed`일 때만 EOS를 지원한다.
그렇지 않으면 중단된 트랜잭션의 레코드가 타겟으로 복제될 수 있다.

---

## 5. 토픽 이름 변환 (Replication Policy)

### 5.1 기본 정책

```
소스 토픽: orders
타겟 토픽: primary.orders (source-cluster-alias.topic-name)
```

### 5.2 Remote Topic 포맷

```java
String formatRemoteTopic(String topic) {
    return replicationPolicy.formatRemoteTopic(
        sourceAndTarget.source(), topic);
}
```

### 5.3 커스텀 정책

`ReplicationPolicy` 인터페이스를 구현하여 토픽 이름 변환 규칙을 커스터마이즈할 수 있다.
`IdentityReplicationPolicy`는 원본 토픽 이름을 그대로 사용하지만,
순환 감지가 작동하지 않을 수 있어 주의가 필요하다.

---

## 6. MirrorCheckpointConnector — 오프셋 동기화

### 6.1 OffsetSync

```java
// OffsetSync.java
// 소스 클러스터의 오프셋과 타겟 클러스터의 오프셋 간 매핑
// 소스 offset 1000 → 타겟 offset 1000 (일반적으로 1:1이지만 항상은 아님)
```

### 6.2 OffsetSyncStore

```java
// OffsetSyncStore.java
// offset-syncs 토픽에서 매핑을 읽어 캐싱
// 컨슈머 그룹 페일오버 시 타겟 클러스터에서의 오프셋을 계산
```

---

## 7. MirrorHeartbeatConnector — 연결 감시

하트비트 커넥터는 주기적으로 하트비트 토픽에 메시지를 전송하여
복제 파이프라인의 연결 상태와 지연시간을 모니터링한다.

```
heartbeats 토픽: heartbeats (소스), primary.heartbeats (타겟)
메시지 내용: 소스 클러스터 이름, 타임스탬프
```

---

## 8. Scheduler — 주기적 작업 관리

```java
@Override
public void start(Map<String, String> props) {
    scheduler = new Scheduler(getClass(), config.entityLabel(),
                              config.adminTimeout());

    // 초기화 작업
    scheduler.execute(this::createOffsetSyncsTopic, "creating offset-syncs topic");
    scheduler.execute(this::loadTopicPartitions, "loading topic-partitions");
    scheduler.execute(this::computeAndCreateTopicPartitions, "creating partitions");

    // 주기적 작업
    scheduler.scheduleRepeating(this::syncTopicAcls,
        config.syncTopicAclsInterval(), "syncing topic ACLs");
    scheduler.scheduleRepeating(this::syncTopicConfigs,
        config.syncTopicConfigsInterval(), "syncing topic configs");
    scheduler.scheduleRepeatingDelayed(this::refreshTopicPartitions,
        config.refreshTopicsInterval(), "refreshing topics");
}
```

---

## 9. 설정 예시

### 9.1 mm2.properties

```properties
# 클러스터 정의
clusters = primary, backup
primary.bootstrap.servers = primary-broker1:9092,primary-broker2:9092
backup.bootstrap.servers = backup-broker1:9092,backup-broker2:9092

# 복제 흐름 활성화
primary->backup.enabled = true
backup->primary.enabled = true

# 토픽 필터
primary->backup.topics = orders, payments, users
primary->backup.topics.exclude = .*internal.*

# 동기화 주기
refresh.topics.interval.seconds = 30
sync.topic.configs.interval.seconds = 60
sync.topic.acls.interval.seconds = 60

# 복제 팩터
replication.factor = 3
```

---

## 10. 정리

### MM2의 핵심 설계 원칙

| 원칙 | 설명 |
|------|------|
| Connect 기반 | 기존 Connect 인프라 재활용 |
| 3-커넥터 분리 | 데이터/오프셋/하트비트 독립 관리 |
| 순환 감지 | 재귀적 토픽 소스 추적 |
| 라운드로빈 분배 | 파티션 수준 부하 균등 분배 |
| 증분 동기화 | IncrementalAlterConfigs API 활용 |
| WRITE ACL 필터 | 미러 토픽 읽기 전용 보장 |

---

## 11. MirrorHerder — 특화된 DistributedHerder

### 11.1 Herder 생성의 복잡성

```java
// MirrorMaker.java:addHerder()
private void addHerder(SourceAndTarget sourceAndTarget) {
    Map<String, String> workerProps = config.workerConfig(sourceAndTarget);
    Plugins plugins = new Plugins(workerProps);
    plugins.compareAndSwapWithDelegatingLoader();

    DistributedConfig distributedConfig = new DistributedConfig(workerProps);

    // 백킹 스토어 생성
    KafkaOffsetBackingStore offsetBackingStore = ...;
    StatusBackingStore statusBackingStore = ...;
    ConfigBackingStore configBackingStore = ...;

    // Worker + Herder 생성
    Worker worker = new Worker(workerId, time, plugins, distributedConfig, ...);
    Herder herder = new MirrorHerder(config, sourceAndTarget, ...);
    herders.put(sourceAndTarget, herder);
}
```

**왜 각 복제 페어마다 별도의 백킹 스토어를 생성하는가?**

| 컴포넌트 | 역할 | 격리 이유 |
|----------|------|----------|
| OffsetBackingStore | 커넥터 오프셋 저장 | 페어별 독립적인 오프셋 추적 |
| StatusBackingStore | 태스크 상태 저장 | 한 페어의 장애가 다른 페어에 영향 없음 |
| ConfigBackingStore | 커넥터 설정 저장 | 페어별 독립적인 설정 관리 |

### 11.2 REST 서버와 내부 통신

```java
// MirrorMaker 초기화
if (config.enableInternalRest()) {
    this.restClient = new RestClient(config);
    internalServer = new MirrorRestServer(config.originals(), restClient);
    internalServer.initializeServer();
    this.advertisedUrl = internalServer.advertisedUrl();
}
```

**왜 내부 REST 서버가 필요한가?** DistributedHerder는 Connect 클러스터 내에서 리더 선출과 태스크 재배분을 위해 REST API로 통신한다. MM2의 각 Herder가 별도의 Connect 런타임이므로, 내부 REST 서버를 통해 코디네이션한다.

## 12. OffsetSyncStore 상세

### 12.1 오프셋 매핑 원리

```
소스 클러스터:  offset 1000 → 레코드 A
                offset 1001 → 레코드 B (트랜잭션 abort)
                offset 1002 → 레코드 C

타겟 클러스터:  offset 1000 → 레코드 A
                              (B는 복제되지 않음 - abort)
                offset 1001 → 레코드 C

매핑: source offset 1000 → target offset 1000
      source offset 1002 → target offset 1001

차이 이유: abort된 트랜잭션 레코드는 READ_COMMITTED에서 건너뜀
```

### 12.2 OffsetSync 토픽

```
mm2-offset-syncs.<target>.internal
  Key: topic-partition (예: orders-0)
  Value: {source_offset, target_offset}

이 토픽은 MirrorSourceTask가 레코드를 복제할 때 주기적으로 업데이트한다.
CheckpointConnector가 이 데이터를 읽어 컨슈머 그룹 오프셋을 변환한다.
```

### 12.3 컨슈머 그룹 페일오버 흐름

```
정상 상태:
  소스 클러스터: consumer-group-1, topic=orders, offset=1000

페일오버:
  1. CheckpointConnector가 offset-syncs 토픽에서
     source offset 1000 → target offset 998 매핑 확인
  2. 타겟 클러스터의 checkpoints 토픽에 기록:
     consumer-group-1, topic=primary.orders, offset=998
  3. 컨슈머가 타겟 클러스터에서 offset 998부터 소비 재개

결과: 최소한의 데이터 중복으로 페일오버 완료
```

## 13. 에러 처리와 복원력

### 13.1 Scheduler 에러 처리

```java
// Scheduler에서 주기적 작업 실패 시
scheduler.scheduleRepeating(this::syncTopicAcls,
    config.syncTopicAclsInterval(), "syncing topic ACLs");

// syncTopicAcls() 내부에서 예외 발생 시:
//   → Scheduler가 예외 로깅
//   → 다음 주기에 재시도
//   → MM2 프로세스는 계속 실행
```

**왜 작업 실패가 프로세스를 중단시키지 않는가?** ACL 동기화나 설정 동기화는 보조 기능이다. 이 기능이 일시적으로 실패해도 데이터 복제(MirrorSourceTask)는 독립적으로 계속 동작해야 한다.

### 13.2 MirrorSourceTask 에러 복원

```
데이터 복제 에러 시나리오:

1. 소스 클러스터 네트워크 단절
   → 컨슈머 poll() 타임아웃
   → Connect 프레임워크가 자동 재시도
   → 복구 후 마지막 오프셋부터 재개

2. 타겟 클러스터 쓰기 실패
   → 프로듀서 재시도 (Kafka 내장)
   → max.retries 초과 시 → 태스크 실패 → 재시작

3. 토픽 자동 생성 실패
   → replication.factor 불만족 등
   → 에러 로깅, 해당 토픽 복제 건너뜀
   → 다음 refresh 주기에 재시도
```

## 14. 순환 감지(Cycle Detection) 상세

### 14.1 isCycle 재귀 추적

```java
boolean isCycle(String topic) {
    // 1. 토픽 이름에서 소스 클러스터 추출
    String source = replicationPolicy.topicSource(topic);
    if (source == null) {
        return false;  // 원본 토픽 (prefix 없음)
    }

    // 2. 소스가 현재 타겟과 같으면 순환
    if (source.equals(sourceAndTarget.target())) {
        return true;   // 예: backup.orders를 backup→primary에서 복제하려 함
    }

    // 3. 상위 토픽으로 재귀 추적
    String upstreamTopic = replicationPolicy.upstreamTopic(topic);
    if (upstreamTopic == null || upstreamTopic.equals(topic)) {
        return false;  // 더 이상 추적 불가
    }
    return isCycle(upstreamTopic);
}
```

### 14.2 다중 클러스터 순환 예시

```
3-클러스터 환경: A, B, C
  A→B: orders → B.orders
  B→C: orders, B.orders → C.orders, C.B.orders
  C→A: ???

C→A 복제 시:
  - orders → A.orders (새 토픽)
  - C.B.orders:
    isCycle("C.B.orders")
      source = "C" (타겟은 A, C≠A)
      upstream = "B.orders"
      isCycle("B.orders")
        source = "B" (타겟은 A, B≠A)
        upstream = "orders"
        isCycle("orders")
          source = null → false
    → false (순환 아님, 정상 복제)

  - A.orders (이미 A에서 온 토픽):
    isCycle("A.orders")
      source = "A" (타겟은 A, A==A)
      → true! (순환 감지)
    → 복제 건너뜀
```

## 15. 성능 고려사항

### 15.1 태스크 수와 처리량

```
파티션 수와 태스크 수의 관계:

100개 파티션, maxTasks=10:
  → 각 태스크가 10개 파티션 담당
  → 라운드로빈으로 균등 분배

처리량 최적화:
  - tasks.max 증가 → 병렬성 향상
  - 단, 각 태스크는 독립적인 컨슈머+프로듀서 → 리소스 비례 증가
  - 최적값: 파티션 수 이하, Connect 워커의 CPU/메모리에 맞춤
```

### 15.2 동기화 간격 튜닝

```properties
# 토픽 발견 주기 (기본 30초)
refresh.topics.interval.seconds = 30
# 값이 작으면: 새 토픽 빠르게 발견, AdminClient 부하 증가
# 값이 크면: 새 토픽 발견 지연

# 설정 동기화 주기 (기본 60초)
sync.topic.configs.interval.seconds = 60
# 값이 작으면: 설정 변경 빠르게 반영
# 값이 크면: AdminClient 부하 감소

# ACL 동기화 주기 (기본 60초)
sync.topic.acls.interval.seconds = 60
```

### 15.3 Exactly-Once 성능 영향

```
Exactly-Once 활성화 시:
  - 프로듀서 트랜잭션 오버헤드: ~5-10% 처리량 감소
  - 컨슈머 READ_COMMITTED: 트랜잭션 커밋 대기
  - 소규모 배치 시 상대적 오버헤드 증가

권장:
  - 데이터 정확성 중요 → Exactly-Once 활성화
  - 높은 처리량 필요, 약간의 중복 허용 → At-Least-Once
```

## 16. 운영 가이드

### 16.1 MM2 모니터링 메트릭

```
Connect JMX 메트릭:
  - source-record-poll-total: 소스에서 읽은 레코드 수
  - source-record-write-total: 타겟에 쓴 레코드 수
  - replication-latency-ms: 복제 지연시간

MM2 커스텀 메트릭:
  - record-count: 복제된 레코드 수
  - byte-count: 복제된 바이트 수
  - replication-latency-ms-avg: 평균 복제 지연
```

### 16.2 페일오버 절차

```
DR 페일오버:
  1. 소스 클러스터 장애 확인
  2. CheckpointConnector가 마지막 체크포인트 확인
  3. 컨슈머를 타겟 클러스터로 전환
  4. 체크포인트 기반 오프셋으로 소비 재개
  5. 일부 레코드 중복 가능 (체크포인트 간격만큼)

페일백:
  1. 소스 클러스터 복구
  2. 타겟→소스 방향 복제 확인
  3. 컨슈머를 소스 클러스터로 전환
```

---

*소스 참조: connect/mirror/src/main/java/org/apache/kafka/connect/mirror/*
