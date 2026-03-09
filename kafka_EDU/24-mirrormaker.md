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

*소스 참조: connect/mirror/src/main/java/org/apache/kafka/connect/mirror/*
