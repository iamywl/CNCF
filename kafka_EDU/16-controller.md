# 16. KRaft 컨트롤러 Deep-Dive

## 목차

1. [개요](#1-개요)
2. [ControllerServer 초기화](#2-controllerserver-초기화)
3. [QuorumController: 핵심 상태 머신](#3-quorumcontroller-핵심-상태-머신)
4. [토픽 관리](#4-토픽-관리)
5. [ISR 변경: AlterPartition](#5-isr-변경-alterpartition)
6. [브로커 등록과 생명주기](#6-브로커-등록과-생명주기)
7. [BrokerLifecycleManager](#7-brokerlifecyclemanager)
8. [동적 설정 관리](#8-동적-설정-관리)
9. [기능 버전 관리](#9-기능-버전-관리)
10. [메타데이터 이미지와 스냅샷](#10-메타데이터-이미지와-스냅샷)
11. [리더 밸런싱](#11-리더-밸런싱)
12. [SharedServer와 결합 모드](#12-sharedserver와-결합-모드)
13. [왜(Why) 이렇게 설계했는가](#13-왜why-이렇게-설계했는가)

---

## 1. 개요

KRaft(Kafka Raft)는 ZooKeeper를 대체하는 Kafka의 자체 메타데이터 관리 시스템이다.
컨트롤러는 클러스터의 모든 메타데이터를 관리하는 중앙 두뇌로, Raft 합의 프로토콜을 기반으로
리더 선출과 메타데이터 복제를 수행한다.

```
소스 파일 위치:
  metadata/src/main/java/org/apache/kafka/controller/   -- 컨트롤러 핵심 로직
  core/src/main/scala/kafka/server/ControllerServer.scala -- 컨트롤러 서버 진입점
  server/src/main/java/org/apache/kafka/server/         -- 브로커 생명주기 관리
  raft/                                                  -- Raft 합의 구현
```

### ZooKeeper vs KRaft

| 항목 | ZooKeeper 모드 | KRaft 모드 |
|------|---------------|-----------|
| 메타데이터 저장 | ZooKeeper znodes | Raft 메타데이터 로그 |
| 리더 선출 | ZK ephemeral nodes | Raft 프로토콜 |
| 컨트롤러 수 | 1 (active) | 1 active + N standby |
| 장애 복구 시간 | ZK 세션 타임아웃 (수초) | Raft 리더 선출 (밀리초) |
| 스냅샷 | ZK 스냅샷 | Kafka 스냅샷 |
| 외부 의존성 | ZooKeeper 클러스터 | 없음 (자체 내장) |

### KRaft 아키텍처 개요

```
+------------------------------------------------------------------+
|                       KRaft 컨트롤러 쿼럼                          |
|                                                                   |
|  +------------------+  +------------------+  +------------------+ |
|  |  Controller #1   |  |  Controller #2   |  |  Controller #3   | |
|  |  (Active Leader) |  |  (Standby/Voter) |  |  (Standby/Voter) | |
|  |                  |  |                  |  |                  | |
|  | QuorumController |  | QuorumController |  | QuorumController | |
|  | (이벤트 처리중)   |  | (팔로워 모드)    |  | (팔로워 모드)    | |
|  +--------+---------+  +--------+---------+  +--------+---------+ |
|           |                     |                     |           |
+-----------|---------------------|---------------------|----------+
            |                     |                     |
            +----------+----------+----------+----------+
                       |                     |
                       v                     v
            +--------------------+  +--------------------+
            |  __cluster_metadata |  |  __cluster_metadata |
            |  (Raft 메타데이터   |  |  (복제본)           |
            |   로그 파티션)      |  |                    |
            +--------------------+  +--------------------+
                       |
            +----------+----------+
            |          |          |
            v          v          v
        +--------+ +--------+ +--------+
        |Broker 1| |Broker 2| |Broker 3|
        |(팔로워) | |(팔로워) | |(팔로워) |
        +--------+ +--------+ +--------+
```

---

## 2. ControllerServer 초기화

**소스 파일**: `core/src/main/scala/kafka/server/ControllerServer.scala`

```scala
class ControllerServer(
  val sharedServer: SharedServer,
  val configSchema: KafkaConfigSchema,
  val bootstrapMetadata: BootstrapMetadata
) extends Logging {

  val config = sharedServer.controllerConfig
  def raftManager: KafkaRaftManager[ApiMessageAndVersion] = sharedServer.raftManager

  // 핵심 컴포넌트
  var tokenCache: DelegationTokenCache = _
  var credentialProvider: CredentialProvider = _
  var socketServer: SocketServer = _
  var controller: Controller = _
}
```

### startup() 시퀀스

```
ControllerServer.startup()
    |
    +---> 1. LinuxIoMetricsCollector 초기화 (Linux IO 메트릭)
    |
    +---> 2. Authorizer 설정 (선택적)
    |         +---> authorizer.configure()
    |         +---> ClusterMetadataAuthorizer 초기화
    |
    +---> 3. DelegationTokenCache + CredentialProvider 초기화
    |
    +---> 4. CreateTopicPolicy / AlterConfigPolicy 로드 (선택적)
    |
    +---> 5. QuorumController 빌드
    |         +---> QuorumController.Builder
    |         +---> .setNodeId(config.nodeId)
    |         +---> .setRaftClient(raftManager.client)
    |         +---> .setReplicaPlacer(StripedReplicaPlacer)
    |         +---> .build()
    |
    +---> 6. QuorumControllerMetrics 등록
    |
    +---> 7. MetadataPublisher 체인 등록
    |         +---> AclPublisher
    |         +---> DelegationTokenPublisher
    |         +---> ScramPublisher
    |         +---> DynamicClientQuotaPublisher
    |         +---> FeaturesPublisher
    |         +---> ControllerMetadataMetricsPublisher
    |
    +---> 8. SocketServer 시작
    |         +---> 컨트롤러 리스너만 바인드
    |
    +---> 9. Controller 활성화
              +---> controller 시작 → Raft 리더 선출 참여
```

---

## 3. QuorumController: 핵심 상태 머신

**소스 파일**: `metadata/src/main/java/org/apache/kafka/controller/QuorumController.java`

```java
public final class QuorumController implements Controller {
    private static final int DEFAULT_MAX_RECORDS_PER_BATCH = 10000;
    static final int MAX_RECORDS_PER_USER_OP = DEFAULT_MAX_RECORDS_PER_BATCH;
}
```

### QuorumController의 내부 구조

```
+------------------------------------------------------------------+
|                       QuorumController                            |
|                                                                   |
|  +---------------------+  +-------------------------+            |
|  | ReplicationControl   |  | ClusterControlManager   |            |
|  | Manager              |  |                         |            |
|  | - 토픽/파티션 관리    |  | - 브로커 등록/펜싱       |            |
|  | - ISR 변경           |  | - 하트비트 처리          |            |
|  | - 리더 선출          |  | - 컨트롤러 등록          |            |
|  +---------------------+  +-------------------------+            |
|                                                                   |
|  +---------------------+  +-------------------------+            |
|  | ConfigurationControl |  | FeatureControlManager   |            |
|  | Manager              |  |                         |            |
|  | - 동적 설정 관리     |  | - 기능 버전 관리         |            |
|  | - ConfigRecord       |  | - FeatureLevelRecord    |            |
|  +---------------------+  +-------------------------+            |
|                                                                   |
|  +---------------------+  +-------------------------+            |
|  | AclControlManager   |  | ProducerIdControl       |            |
|  |                      |  | Manager                 |            |
|  | - ACL 레코드 관리    |  | - 프로듀서 ID 할당       |            |
|  +---------------------+  +-------------------------+            |
|                                                                   |
|  +---------------------+  +-------------------------+            |
|  | ClientQuotaControl  |  | DelegationTokenControl  |            |
|  | Manager              |  | Manager                 |            |
|  | - 클라이언트 쿼터    |  | - 위임 토큰 관리         |            |
|  +---------------------+  +-------------------------+            |
|                                                                   |
|  +---------------------+  +-------------------------+            |
|  | ScramControlManager  |  | OffsetControlManager    |            |
|  |                      |  |                         |            |
|  | - SCRAM 자격증명     |  | - 오프셋 관리            |            |
|  +---------------------+  +-------------------------+            |
|                                                                   |
|  +------------------------------------------------------+        |
|  | KafkaEventQueue (단일 스레드 이벤트 큐)                |        |
|  | - 모든 상태 변경을 직렬화하여 처리                      |        |
|  +------------------------------------------------------+        |
+------------------------------------------------------------------+
```

### 이벤트 처리 모델

QuorumController는 **단일 스레드 이벤트 큐**(KafkaEventQueue)를 사용하여 모든 상태 변경을
직렬화한다. 이는 동시성 문제를 원천적으로 방지한다.

```
외부 요청 (CreateTopics, AlterPartition, ...)
    |
    v
+---------------------------------------------------+
| KafkaEventQueue                                    |
|                                                    |
| +--------+  +--------+  +--------+  +--------+    |
| |Event #1|->|Event #2|->|Event #3|->|Event #4|    |
| +--------+  +--------+  +--------+  +--------+    |
|                                                    |
| 단일 스레드가 순서대로 처리                          |
+---------------------------------------------------+
    |
    v
이벤트 핸들러 실행
    |
    +---> ControllerResult<T> 반환
    |         |
    |         +---> List<ApiMessageAndVersion> records  -- 메타데이터 레코드들
    |         +---> T response                          -- 응답 데이터
    |
    +---> Raft 로그에 레코드 쓰기
    |
    +---> 레코드가 커밋되면 응답 완료
```

### 메타데이터 레코드 타입

QuorumController가 Raft 로그에 기록하는 레코드 타입들:

| 레코드 | 설명 |
|--------|------|
| RegisterBrokerRecord | 브로커 등록 |
| UnregisterBrokerRecord | 브로커 등록 해제 |
| FenceBrokerRecord | 브로커 펜싱 (격리) |
| UnfenceBrokerRecord | 브로커 펜싱 해제 |
| BrokerRegistrationChangeRecord | 브로커 등록 정보 변경 |
| TopicRecord | 토픽 생성 |
| PartitionRecord | 파티션 생성 |
| PartitionChangeRecord | 파티션 변경 (ISR, 리더 등) |
| RemoveTopicRecord | 토픽 삭제 |
| ConfigRecord | 설정 변경 |
| FeatureLevelRecord | 기능 버전 변경 |
| AccessControlEntryRecord | ACL 추가 |
| RemoveAccessControlEntryRecord | ACL 삭제 |
| ProducerIdsRecord | 프로듀서 ID 할당 |
| ClientQuotaRecord | 클라이언트 쿼터 |
| DelegationTokenRecord | 위임 토큰 |
| UserScramCredentialRecord | SCRAM 자격증명 |
| RegisterControllerRecord | 컨트롤러 등록 |
| NoOpRecord | 무연산 레코드 (동기화용) |
| BeginTransactionRecord | 트랜잭션 시작 (KRaft 트랜잭션) |
| EndTransactionRecord | 트랜잭션 종료 |
| AbortTransactionRecord | 트랜잭션 중단 |

---

## 4. 토픽 관리

### ReplicationControlManager

**소스 파일**: `metadata/src/main/java/org/apache/kafka/controller/ReplicationControlManager.java`

```java
public class ReplicationControlManager {
    // 토픽, 파티션, ISR 등 복제 관련 모든 상태를 관리
    // CreateTopics, DeleteTopics, AlterPartitionReassignments 등 처리
}
```

### CreateTopics 처리 흐름

```
클라이언트: CreateTopics 요청
    |
    v
QuorumController.createTopics(request)
    |
    +---> KafkaEventQueue에 이벤트 등록
    |
    v
ReplicationControlManager.createTopics(request)
    |
    +---> 1. 토픽 이름 유효성 검증
    |         - 정규식 검사, 중복 검사, 예약 이름 검사
    |
    +---> 2. 파티션 수/복제 팩터 결정
    |         - 명시적 지정 또는 기본값 (default.replication.factor)
    |
    +---> 3. 레플리카 배치 (ReplicaPlacer)
    |         +---> StripedReplicaPlacer
    |         |     - 랙(rack) 인식 배치
    |         |     - 브로커 간 균등 분배
    |         |     - 동일 랙에 모든 레플리카 배치 방지
    |
    +---> 4. CreateTopicPolicy 검사 (설정된 경우)
    |
    +---> 5. 레코드 생성
    |         +---> TopicRecord { name, topicId (UUID) }
    |         +---> PartitionRecord { topicId, partitionId, replicas, isr, leader }
    |         +---> ConfigRecord (토픽별 설정, 있는 경우)
    |
    +---> 6. ControllerResult 반환
              +---> records: [TopicRecord, PartitionRecord*N, ConfigRecord*M]
              +---> response: CreateTopicsResponseData
```

### DeleteTopics 처리 흐름

```
DeleteTopics 요청
    |
    v
ReplicationControlManager.deleteTopics(topicIds)
    |
    +---> 토픽 존재 여부 확인
    +---> RemoveTopicRecord 생성
    +---> 관련 파티션 상태 정리
    +---> 관련 설정 정리 (ConfigRecord 삭제)
```

### 파티션 재할당 (AlterPartitionReassignments)

```
기존 상태:                    목표 상태:
  파티션 P0                     파티션 P0
  replicas: [1, 2, 3]          replicas: [4, 5, 6]
  ISR: [1, 2, 3]               ISR: [4, 5, 6]
  leader: 1                    leader: 4

재할당 과정:
  1. replicas를 [1, 2, 3, 4, 5, 6]으로 확장
  2. 새 레플리카(4, 5, 6)가 데이터를 따라잡을 때까지 대기
  3. 새 레플리카가 ISR에 합류
  4. 기존 레플리카(1, 2, 3) 제거
  5. replicas를 [4, 5, 6]으로 축소
  6. 리더를 4로 변경

각 단계마다 PartitionChangeRecord가 생성됨
```

---

## 5. ISR 변경: AlterPartition

### AlterPartition 요청 처리

브로커가 ISR(In-Sync Replicas) 변경을 요청할 때 AlterPartition API를 사용한다.

```
브로커 (파티션 리더)
    |
    +---> ISR에서 느린 레플리카 제거 필요
    |     또는
    +---> 따라잡은 레플리카를 ISR에 추가 필요
    |
    v
AlterPartition 요청 → 컨트롤러
    |
    v
ReplicationControlManager.alterPartition(request)
    |
    +---> 1. 요청한 브로커가 실제 리더인지 검증
    +---> 2. 브로커 에포크(epoch) 검증
    +---> 3. 파티션 에포크 검증
    +---> 4. 새 ISR 유효성 검사
    |         - 모든 ISR 멤버가 유효한 레플리카인가?
    |         - 리더가 ISR에 포함되어 있는가?
    |
    +---> 5. PartitionChangeRecord 생성
    |         {
    |           topicId: ...,
    |           partitionId: ...,
    |           isr: [새 ISR 목록],
    |           leader: (변경 시),
    |           leaderEpoch: (변경 시 증가)
    |         }
    |
    +---> 6. 응답: AlterPartitionResponseData
```

### PartitionChangeBuilder

**소스 파일**: `metadata/src/main/java/org/apache/kafka/controller/PartitionChangeBuilder.java`

PartitionChangeBuilder는 파티션 상태 변경을 안전하게 계산하는 헬퍼 클래스다.

```
PartitionChangeBuilder
    |
    +---> setTargetIsr(newIsr)           -- ISR 변경
    +---> setTargetReplicas(newReplicas) -- 레플리카 변경
    +---> electLeader()                  -- 리더 선출
    |
    +---> build()
              |
              +---> 현재 상태와 비교
              +---> 변경이 있으면 PartitionChangeRecord 생성
              +---> 변경이 없으면 Optional.empty() 반환
```

---

## 6. 브로커 등록과 생명주기

### ClusterControlManager

**소스 파일**: `metadata/src/main/java/org/apache/kafka/controller/ClusterControlManager.java`

```java
public class ClusterControlManager {
    // 브로커 등록, 펜싱, 언펜싱, 하트비트 관리
    // TimelineHashMap을 사용한 스냅샷 가능한 상태 관리
}
```

### 브로커 등록 흐름

```
브로커 시작
    |
    v
BrokerRegistrationRequest → 컨트롤러
    |
    v
ClusterControlManager.registerBroker(request)
    |
    +---> 1. 클러스터 ID 검증
    |         - 브로커의 클러스터 ID == 컨트롤러의 클러스터 ID
    |
    +---> 2. 중복 등록 검사
    |         - 같은 brokerId가 이미 등록되어 있고 에포크가 다르면?
    |         - 기존 등록을 펜싱하고 새 등록 수행
    |
    +---> 3. RegisterBrokerRecord 생성
    |         {
    |           brokerId: 1,
    |           brokerEpoch: 12345,   -- 고유 에포크
    |           incarnationId: UUID,
    |           listeners: [...],
    |           features: [...],
    |           rack: "rack-a",
    |           fenced: true,         -- 초기 상태는 펜싱
    |           inControlledShutdown: false
    |         }
    |
    +---> 4. 응답: BrokerRegistrationReply { epoch, brokerEpoch }
```

### 브로커 상태 전이

```
                    RegisterBroker
                         |
                         v
+----------+      +----------+       +-----------+
|  미등록   | ---> |  펜싱됨   | ----> |  활성화   |
| (Unknown) |      | (Fenced) |       | (Unfenced)|
+----------+      +----+-----+       +-----+-----+
                       ^  |                 |
                       |  |   UnfenceBroker |
                       |  |   (하트비트 성공)|
                       |  |                 |
                       |  |   FenceBroker   |
                       |  +--- (하트비트    -+
                       |       타임아웃)
                       |                    |
                       |                    v
                       |            +--------------+
                       |            | Controlled   |
                       |            | Shutdown     |
                       +----------- | (정상 종료중) |
                    (종료 완료)      +--------------+
```

### 펜싱(Fencing)의 의미

펜싱된 브로커는:

1. **리더 파티션을 가질 수 없음**: ISR에서 제거되고 리더십이 이전
2. **새 파티션 할당 대상에서 제외**: ReplicaPlacer가 무시
3. **데이터 서빙 가능**: 팔로워로서 복제는 계속 수행

```
Register → Fenced → (heartbeat 성공) → Unfenced → (heartbeat 실패) → Fenced
                                            |
                                    (controlled shutdown)
                                            |
                                            v
                                    InControlledShutdown → Unregister
```

### 브로커 등록 레코드 타입

| 레코드 | 상태 변경 | 설명 |
|--------|----------|------|
| RegisterBrokerRecord | 미등록 → 펜싱됨 | 최초 등록, 항상 펜싱 상태로 시작 |
| UnfenceBrokerRecord | 펜싱됨 → 활성화 | 하트비트 성공 후 활성화 |
| FenceBrokerRecord | 활성화 → 펜싱됨 | 하트비트 타임아웃 |
| BrokerRegistrationChangeRecord | 다양 | 메타데이터 변경, controlled shutdown 진입 |
| UnregisterBrokerRecord | 활성화 → 미등록 | 완전 제거 (관리자 명령) |

---

## 7. BrokerLifecycleManager

**소스 파일**: `server/src/main/java/org/apache/kafka/server/BrokerLifecycleManager.java`

```java
public class BrokerLifecycleManager {
    private final Logger logger;
    private final KafkaEventQueue eventQueue;
    private final AbstractKafkaConfig config;
    private final Time time;
    private final Set<Uuid> logDirs;
}
```

BrokerLifecycleManager는 브로커 측에서 컨트롤러와의 상호작용을 관리한다.

### 브로커 → 컨트롤러 통신

```
BrokerLifecycleManager
    |
    +---> 1. BrokerRegistrationRequest 전송
    |         - 브로커 시작 시 1회
    |         - listeners, features, rack, logDirs 정보 포함
    |
    +---> 2. BrokerHeartbeatRequest 주기적 전송
    |         - 기본 간격: broker.heartbeat.interval.ms
    |         - 포함 정보:
    |           - wantFence: 펜싱 요청 여부
    |           - wantShutDown: 종료 요청 여부
    |           - currentMetadataOffset: 현재 메타데이터 오프셋
    |
    +---> 3. 응답 처리
              - isCaughtUp: 메타데이터를 충분히 따라잡았는가
              - isFenced: 현재 펜싱 상태인가
              - shouldShutDown: 종료해도 되는가
```

### 하트비트 처리 (컨트롤러 측)

**소스 파일**: `metadata/src/main/java/org/apache/kafka/controller/BrokerHeartbeatManager.java`

```
BrokerHeartbeatRequest 도착
    |
    v
ClusterControlManager.heartbeat(request)
    |
    +---> 브로커 에포크 검증
    |
    +---> BrokerHeartbeatManager.touch(brokerId)
    |         - 마지막 하트비트 시각 갱신
    |
    +---> 브로커가 Fenced 상태이고, 메타데이터를 충분히 따라잡았으면:
    |         - UnfenceBrokerRecord 생성 → 활성화
    |
    +---> 브로커가 wantShutDown == true이면:
    |         - Controlled Shutdown 절차 시작
    |         - 리더 파티션을 다른 브로커로 이전
    |
    +---> 응답: BrokerHeartbeatReply
              { isCaughtUp, isFenced, shouldShutDown }
```

### 하트비트 타임아웃과 자동 펜싱

```
BrokerHeartbeatManager에서 주기적으로 체크:
    |
    +---> 마지막 하트비트로부터 broker.session.timeout.ms 초과?
    |         |
    |    Yes  +---> FenceBrokerRecord 생성
    |         |     - 해당 브로커를 펜싱
    |         |     - 리더 파티션을 ISR의 다른 브로커로 이전
    |         |     - 클라이언트는 잠시 중단 후 새 리더 발견
    |         |
    |    No   +---> 아무것도 안함
```

---

## 8. 동적 설정 관리

### ConfigurationControlManager

**소스 파일**: `metadata/src/main/java/org/apache/kafka/controller/ConfigurationControlManager.java`

```java
public class ConfigurationControlManager {
    public static final ConfigResource DEFAULT_NODE =
        new ConfigResource(Type.BROKER, "");

    private final KafkaConfigSchema configSchema;
    private final Optional<AlterConfigPolicy> alterConfigPolicy;
    private final ConfigurationValidator validator;

    // 스냅샷 가능한 설정 데이터
    private final TimelineHashMap<ConfigResource, TimelineHashMap<String, String>> configData;
    private final TimelineHashSet<Integer> brokersWithConfigs;
}
```

### 설정 리소스 타입

```
ConfigResource.Type:
    BROKER          -- 브로커 설정 (예: "0" → 브로커 0, "" → 클러스터 기본)
    TOPIC           -- 토픽 설정 (예: "my-topic")
    BROKER_LOGGER   -- 브로커 로거 레벨
    CLIENT_METRICS  -- 클라이언트 메트릭 구독
```

### 설정 변경 흐름

```
AlterConfigs / IncrementalAlterConfigs 요청
    |
    v
ConfigurationControlManager.incrementalAlterConfigs(alterations)
    |
    +---> 1. 리소스 존재 여부 검증 (토픽이면 토픽 존재 확인)
    |
    +---> 2. 설정 키/값 유효성 검사
    |         - configSchema에서 정의된 키인가?
    |         - 값의 타입이 올바른가?
    |
    +---> 3. AlterConfigPolicy 검사 (설정된 경우)
    |
    +---> 4. OpType에 따른 처리
    |         - SET: 값 설정
    |         - DELETE: 값 삭제 (기본값으로 복원)
    |         - APPEND: 리스트에 값 추가
    |         - SUBTRACT: 리스트에서 값 제거
    |
    +---> 5. ConfigRecord 생성
              {
                resourceType: TOPIC,
                resourceName: "my-topic",
                name: "retention.ms",
                value: "86400000"
              }
```

### TimelineHashMap의 역할

ConfigurationControlManager는 `TimelineHashMap`을 사용한다. 이것은:

1. **스냅샷 지원**: 특정 시점의 상태를 저장하고 복원
2. **Raft 로그와 동기화**: 레코드 적용 시 상태 변경, 롤백 가능
3. **메모리 효율성**: 변경된 부분만 새로 저장 (copy-on-write 아님, 버전 체인)

```
Timeline:
  offset 100: {"retention.ms": "604800000"}     -- 7일
  offset 150: {"retention.ms": "86400000"}      -- 1일
  offset 200: {"retention.ms": "86400000",
               "max.message.bytes": "1048576"}

스냅샷 at offset 100 → 7일 retention 반환
스냅샷 at offset 200 → 1일 retention + 1MB max message 반환
```

---

## 9. 기능 버전 관리

### FeatureControlManager

**소스 파일**: `metadata/src/main/java/org/apache/kafka/controller/FeatureControlManager.java`

```java
public class FeatureControlManager {
    // 기능 버전(Feature Level)을 관리
    // 클러스터의 모든 브로커가 지원하는 최소 버전을 추적
    // 롤링 업그레이드 시 안전하게 기능 활성화
}
```

### 기능 버전의 역할

```
+-------------------------------------------------------------------+
|  브로커 클러스터                                                    |
|                                                                   |
|  Broker 1: MetadataVersion 3.8-IV0, KRaft v1                     |
|  Broker 2: MetadataVersion 3.8-IV0, KRaft v1                     |
|  Broker 3: MetadataVersion 3.7-IV4, KRaft v0  <-- 구버전          |
|                                                                   |
|  Finalized Features:                                              |
|    MetadataVersion = 3.7-IV4  (가장 낮은 버전으로 합의)             |
|    KRaft = v0                                                      |
+-------------------------------------------------------------------+
```

### UpdateFeatures 처리 흐름

```
UpdateFeatures 요청
    |
    v
FeatureControlManager.updateFeatures(updates)
    |
    +---> 1. 요청된 기능이 유효한가?
    |
    +---> 2. 모든 브로커가 요청된 버전을 지원하는가?
    |         +---> ClusterFeatureSupportDescriber 조회
    |         +---> 각 브로커의 지원 범위 확인
    |
    +---> 3. 다운그레이드인가? (현재 버전 > 요청 버전)
    |         - UPGRADE_BROKER_NOT_ALLOWED → 다운그레이드 거부
    |         - SAFE_DOWNGRADE → 안전한 다운그레이드 허용
    |         - UNSAFE_DOWNGRADE → 데이터 손실 가능, 명시적 요청 필요
    |
    +---> 4. FeatureLevelRecord 생성
              {
                name: "metadata.version",
                featureLevel: 17  -- MetadataVersion 3.8-IV0
              }
```

### 롤링 업그레이드 절차

```
1단계: 모든 브로커를 새 버전으로 업그레이드 (바이너리만)
    Broker 1: 3.7 → 3.8 (재시작)
    Broker 2: 3.7 → 3.8 (재시작)
    Broker 3: 3.7 → 3.8 (재시작)
    -- 이 시점에서 metadata.version은 여전히 3.7

2단계: 기능 버전 업그레이드 요청
    kafka-features.sh --upgrade --metadata 3.8-IV0
    -- 컨트롤러가 모든 브로커 지원 확인 후 FeatureLevelRecord 기록
    -- metadata.version이 3.8-IV0으로 업그레이드

3단계: 새 기능 사용 가능
    -- 3.8에서 추가된 새 메타데이터 레코드 타입 사용 가능
```

---

## 10. 메타데이터 이미지와 스냅샷

### MetadataImage

메타데이터 이미지는 특정 시점의 클러스터 전체 메타데이터 상태를 나타내는 불변 스냅샷이다.

```
MetadataImage (at offset N)
    |
    +---> TopicsImage          -- 모든 토픽/파티션 상태
    +---> ClusterImage         -- 모든 브로커 등록 정보
    +---> ConfigurationsImage  -- 모든 동적 설정
    +---> AclsImage            -- 모든 ACL
    +---> ClientQuotasImage    -- 클라이언트 쿼터
    +---> FeaturesImage        -- 기능 버전
    +---> ProducerIdsImage     -- 프로듀서 ID 상태
    +---> ScramImage           -- SCRAM 자격증명
    +---> DelegationTokenImage -- 위임 토큰
```

### Raft 스냅샷

Raft 로그가 무한히 커지는 것을 방지하기 위해 주기적으로 스냅샷을 생성한다.

```
Raft 메타데이터 로그:
  +----+----+----+----+----+----+----+----+----+
  | R1 | R2 | R3 | R4 | R5 | R6 | R7 | R8 | R9 |
  +----+----+----+----+----+----+----+----+----+
                     |
                     v
              스냅샷 생성 (at offset 5)
              +------------------+
              | 전체 메타데이터    |
              | 상태를 직렬화     |
              +------------------+
                     |
                     v
  이전 로그 삭제 가능:
  [삭제] [삭제] [삭제] [삭제] [삭제] | R6 | R7 | R8 | R9 |
                                     ^
                                     스냅샷 이후부터만 유지
```

### 팔로워의 따라잡기 (Catch-up)

```
새 브로커가 클러스터에 합류:
    |
    +---> 1. 컨트롤러에서 최신 스냅샷 전송 (대량 전송)
    |         - __cluster_metadata 토픽의 스냅샷 세그먼트
    |
    +---> 2. 스냅샷 적용하여 MetadataImage 구축
    |
    +---> 3. 스냅샷 이후의 레코드를 순차 적용
    |         - Raft 로그의 delta 레코드들
    |
    +---> 4. 최신 상태에 도달 → 브로커 활성화 가능
```

---

## 11. 리더 밸런싱

### Preferred Leader Election

각 파티션에는 "preferred leader"가 있다. 이는 레플리카 목록의 첫 번째 브로커다.
리더 밸런싱은 preferred leader로 리더십을 복원하여 부하를 고르게 분배한다.

```
파티션 P0: replicas=[1, 2, 3], leader=2  -- 브로커 1이 preferred
파티션 P1: replicas=[2, 3, 1], leader=3  -- 브로커 2가 preferred
파티션 P2: replicas=[3, 1, 2], leader=1  -- 브로커 3이 preferred

리더 밸런싱 후:
파티션 P0: replicas=[1, 2, 3], leader=1  -- preferred로 복원
파티션 P1: replicas=[2, 3, 1], leader=2  -- preferred로 복원
파티션 P2: replicas=[3, 1, 2], leader=3  -- 이미 preferred
```

### 자동 리더 밸런싱

QuorumController는 주기적으로 리더 불균형을 체크한다:

```
leaderImbalanceCheckIntervalNs (기본: 5분)마다:
    |
    +---> 각 브로커의 리더 비율 계산
    |         - 실제 리더 수 / preferred 리더 수
    |
    +---> 불균형 비율 > leader.imbalance.per.broker.percentage (기본: 10%)
    |         |
    |    Yes  +---> preferred leader로 선출 시도
    |         |     - 조건: preferred leader가 ISR에 있어야 함
    |         |     - PartitionChangeRecord 생성
    |         |
    |    No   +---> 아무것도 안함
```

### ElectLeaders 요청

```
ElectLeadersRequest:
  electionType: PREFERRED  또는  UNCLEAN

PREFERRED 선출:
    +---> ISR에서 preferred replica를 리더로 선출
    +---> preferred replica가 ISR에 없으면 실패

UNCLEAN 선출:
    +---> ISR이 비어있을 때, ISR 밖의 레플리카를 리더로 선출
    +---> 데이터 손실 가능! (비동기 레플리카가 리더가 됨)
    +---> unclean.leader.election.enable = true 필요
```

---

## 12. SharedServer와 결합 모드

### SharedServer

Kafka 4.0부터 브로커와 컨트롤러를 같은 프로세스에서 실행할 수 있다 (결합 모드).

```
결합 모드 (Combined Mode):
+------------------------------------------+
|           SharedServer                    |
|                                          |
|  +------------------+  +---------------+ |
|  | ControllerServer |  | BrokerServer  | |
|  |                  |  |               | |
|  | QuorumController |  | ReplicaManager| |
|  | Raft Voter       |  | GroupCoord.   | |
|  +------------------+  +---------------+ |
|                                          |
|  공유 리소스:                              |
|  - KafkaRaftManager (Raft 클라이언트)      |
|  - MetadataLoader (메타데이터 로더)        |
|  - Metrics (메트릭)                       |
|  - Time (시간)                            |
+------------------------------------------+

분리 모드 (Separated Mode):
+------------------+     +------------------+
| Controller Node  |     | Broker Node      |
|                  |     |                  |
| ControllerServer |     | BrokerServer     |
| SharedServer     |     | SharedServer     |
| (Raft Voter)     |     | (Raft Observer)  |
+------------------+     +------------------+
```

### 왜 결합 모드를 지원하는가

1. **개발/테스트 편의성**: 단일 프로세스로 전체 Kafka 실행
2. **소규모 배포**: 3노드로 브로커+컨트롤러 모두 운영
3. **자원 효율성**: 별도 컨트롤러 노드 불필요

### MetadataLoader와 MetadataPublisher

```
Raft 메타데이터 로그
    |
    v
MetadataLoader
    |
    +---> 레코드를 읽어서 MetadataImage 구축
    |
    +---> MetadataPublisher 체인에 이미지 게시
              |
              +---> AclPublisher           -- Authorizer에 ACL 전달
              +---> ScramPublisher         -- CredentialProvider에 전달
              +---> FeaturesPublisher      -- 기능 버전 게시
              +---> DynamicConfigPublisher -- 동적 설정 적용
              +---> KRaftMetadataCachePublisher -- 메타데이터 캐시 갱신
```

---

## 13. 왜(Why) 이렇게 설계했는가

### Q: 왜 ZooKeeper를 제거하고 KRaft를 만들었는가?

1. **운영 복잡성**: ZooKeeper는 별도 클러스터 운영이 필요하고, 모니터링/업그레이드가 이중 부담
2. **성능 한계**: ZK의 znode 모델은 대규모 메타데이터(수만 파티션)에서 느림
3. **장애 복구 시간**: ZK 세션 타임아웃(수초~수십초) vs Raft 리더 선출(밀리초)
4. **확장성**: ZK의 읽기/쓰기 병목 vs Raft의 로그 기반 복제

### Q: 왜 단일 스레드 이벤트 큐를 사용하는가?

QuorumController의 KafkaEventQueue는 모든 상태 변경을 직렬화한다:

1. **동시성 문제 원천 방지**: 락, 데드락, 경쟁 조건이 발생할 수 없음
2. **추론 용이성**: 상태 전이가 결정적이고 디버깅이 쉬움
3. **Raft와의 자연스러운 매핑**: Raft 로그 자체가 직렬화된 이벤트 시퀀스

```
단점: 처리량 제한?
  → 메타데이터 변경은 데이터 플레인 대비 매우 드물다
  → CreateTopics는 초당 수십 건이면 충분
  → 읽기(메타데이터 조회)는 스냅샷에서 락 없이 수행
```

### Q: 왜 브로커를 처음에 펜싱 상태로 등록하는가?

1. **안전성**: 메타데이터를 충분히 따라잡지 못한 브로커가 리더가 되면 오래된 데이터 서빙
2. **일관성**: 브로커가 최신 ISR 정보를 모르면 잘못된 복제 수행 가능
3. **점진적 활성화**: 하트비트로 건강함을 증명한 후에만 활성화

### Q: 왜 TimelineHashMap을 사용하는가?

TimelineHashMap은 SnapshotRegistry와 통합된 버전 관리 자료구조다:

1. **스냅샷 효율성**: 스냅샷 생성 시 전체 복사 불필요 (버전 체인으로 관리)
2. **롤백 지원**: Raft 로그 잘림(truncation) 시 이전 상태로 되돌리기 가능
3. **읽기 일관성**: 특정 오프셋 기준의 일관된 뷰 제공

### Q: 왜 메타데이터를 __cluster_metadata 토픽에 저장하는가?

1. **Raft 로그 자체가 Kafka 로그**: 기존 로그 세그먼트 관리 재사용
2. **스냅샷**: Kafka의 로그 세그먼트 형식으로 스냅샷 저장
3. **복제**: Kafka의 복제 프로토콜 대신 Raft 프로토콜 사용하지만, 저장 형식은 동일

---

## 부록: 주요 소스 파일 색인

| 파일 | 경로 | 설명 |
|------|------|------|
| ControllerServer.scala | core/src/main/scala/kafka/server/ControllerServer.scala | 컨트롤러 서버 진입점 |
| QuorumController.java | metadata/.../controller/QuorumController.java | 핵심 상태 머신 |
| ReplicationControlManager.java | metadata/.../controller/ReplicationControlManager.java | 토픽/파티션/ISR 관리 |
| ClusterControlManager.java | metadata/.../controller/ClusterControlManager.java | 브로커 등록/펜싱 |
| ConfigurationControlManager.java | metadata/.../controller/ConfigurationControlManager.java | 동적 설정 |
| FeatureControlManager.java | metadata/.../controller/FeatureControlManager.java | 기능 버전 |
| AclControlManager.java | metadata/.../controller/AclControlManager.java | ACL 관리 |
| BrokerHeartbeatManager.java | metadata/.../controller/BrokerHeartbeatManager.java | 하트비트 추적 |
| PartitionChangeBuilder.java | metadata/.../controller/PartitionChangeBuilder.java | 파티션 변경 계산 |
| BrokerLifecycleManager.java | server/.../server/BrokerLifecycleManager.java | 브로커 측 생명주기 |
| ProducerIdControlManager.java | metadata/.../controller/ProducerIdControlManager.java | 프로듀서 ID |
| DelegationTokenControlManager.java | metadata/.../controller/DelegationTokenControlManager.java | 위임 토큰 |
| ScramControlManager.java | metadata/.../controller/ScramControlManager.java | SCRAM 자격증명 |
| ClientQuotaControlManager.java | metadata/.../controller/ClientQuotaControlManager.java | 클라이언트 쿼터 |
