# 21. Kafka Admin API (관리 API) Deep-Dive

## 목차

1. [개요](#1-개요)
2. [왜(Why) Admin API가 필요한가](#2-왜why-admin-api가-필요한가)
3. [Admin API 아키텍처](#3-admin-api-아키텍처)
4. [ControllerApis - 요청 핸들러](#4-controllerapis---요청-핸들러)
5. [토픽 생성 (CreateTopics)](#5-토픽-생성-createtopics)
6. [토픽 삭제 (DeleteTopics)](#6-토픽-삭제-deletetopics)
7. [파티션 생성 (CreatePartitions)](#7-파티션-생성-createpartitions)
8. [파티션 재배치 (AlterPartitionReassignments)](#8-파티션-재배치-alterpartitionreassignments)
9. [ACL 관리 (AclControlManager)](#9-acl-관리-aclcontrolmanager)
10. [ReplicationControlManager - 토픽/파티션 컨트롤러](#10-replicationcontrolmanager---토픽파티션-컨트롤러)
11. [ControllerMutationQuota와 Admin API](#11-controllermutationquota와-admin-api)
12. [KafkaAdminClient - 클라이언트 라이브러리](#12-kafkaadminclient---클라이언트-라이브러리)
13. [왜(Why) 이렇게 설계했는가](#13-왜why-이렇게-설계했는가)

---

## 1. 개요

Kafka Admin API는 **토픽, 파티션, ACL, 설정** 등 클러스터 리소스를 관리하는 API 집합이다. KRaft 모드에서는 `ControllerApis`가 관리 요청을 처리하고, `ReplicationControlManager`와 `AclControlManager`가 실제 상태 변경을 수행한다.

```
소스 파일 위치:
  core/src/main/scala/kafka/server/ControllerApis.scala                         -- 컨트롤러 API 핸들러
  metadata/src/main/java/org/apache/kafka/controller/ReplicationControlManager.java -- 토픽/파티션 관리
  metadata/src/main/java/org/apache/kafka/controller/AclControlManager.java      -- ACL 관리
  metadata/src/main/java/org/apache/kafka/controller/ClientQuotaControlManager.java -- 쿼터 관리
  metadata/src/main/java/org/apache/kafka/controller/ConfigurationControlManager.java -- 설정 관리
  clients/src/main/java/org/apache/kafka/clients/admin/KafkaAdminClient.java     -- 클라이언트 SDK
  clients/src/main/java/org/apache/kafka/common/requests/CreateTopicsRequest.java
  clients/src/main/java/org/apache/kafka/common/requests/DeleteTopicsRequest.java
  clients/src/main/java/org/apache/kafka/common/requests/CreatePartitionsRequest.java
  clients/src/main/java/org/apache/kafka/common/requests/AlterPartitionReassignmentsRequest.java
  server/src/main/java/org/apache/kafka/server/quota/ControllerMutationQuotaManager.java -- 변경 쿼터
  core/src/main/scala/kafka/server/ConfigAdminManager.scala                     -- 설정 관리 도우미
  core/src/main/java/kafka/server/builders/KafkaApisBuilder.java                -- API 핸들러 빌더
```

### Admin API 전체 구조

```
┌─────────────────────────────────────────────────────────────┐
│                     클라이언트 (KafkaAdminClient)              │
│                                                             │
│  createTopics() / deleteTopics() / createPartitions()       │
│  createAcls() / deleteAcls() / alterConfigs()               │
│  alterPartitionReassignments() / alterClientQuotas()        │
└─────────────────────────────────────────────────────────────┘
                          │
                          │ Kafka Protocol (TCP)
                          v
┌─────────────────────────────────────────────────────────────┐
│                   ControllerApis (컨트롤러 노드)              │
│                                                             │
│  1. 요청 파싱 및 인증/인가 검사                                │
│  2. ControllerMutationQuota 검사                             │
│  3. Controller 인터페이스 호출                                │
└─────────────────────────────────────────────────────────────┘
                          │
                          v
┌─────────────────────────────────────────────────────────────┐
│                  QuorumController                            │
│                                                             │
│  ┌─────────────────┐  ┌──────────────┐  ┌───────────────┐  │
│  │ Replication     │  │ Acl          │  │ ClientQuota   │  │
│  │ ControlManager  │  │ ControlMgr   │  │ ControlMgr    │  │
│  └─────────────────┘  └──────────────┘  └───────────────┘  │
│                                                             │
│  ┌─────────────────┐  ┌──────────────┐                     │
│  │ Configuration   │  │ Feature      │                     │
│  │ ControlManager  │  │ ControlMgr   │                     │
│  └─────────────────┘  └──────────────┘                     │
└─────────────────────────────────────────────────────────────┘
                          │
                          │ 메타데이터 레코드 생성
                          v
┌─────────────────────────────────────────────────────────────┐
│                __cluster_metadata 토픽                       │
│                                                             │
│  TopicRecord / PartitionRecord / RemoveTopicRecord          │
│  AccessControlEntryRecord / RemoveAccessControlEntryRecord  │
│  ClientQuotaRecord / ConfigRecord                           │
└─────────────────────────────────────────────────────────────┘
```

---

## 2. 왜(Why) Admin API가 필요한가

### 문제 상황

분산 스트리밍 플랫폼에서는 리소스 관리가 복잡하다:

1. **토픽 관리**: 파티션 수, 복제 팩터, 설정 변경을 안전하게 수행해야 함
2. **파티션 재배치**: 브로커 추가/제거 시 데이터를 안전하게 이동해야 함
3. **보안 관리**: ACL을 통한 접근 제어가 필요
4. **설정 관리**: 브로커, 토픽, 사용자 설정을 동적으로 변경해야 함
5. **쿼터 관리**: 클라이언트별 사용량 제한이 필요

### 설계 원칙

| 원칙 | 구현 |
|------|------|
| 단일 진입점 | ControllerApis가 모든 관리 요청을 처리 |
| 멱등성 | 토픽 생성 시 이미 존재하면 에러가 아닌 정보 반환 |
| 원자성 | 각 요청 내 개별 리소스별 성공/실패 분리 (부분 실패 허용) |
| 인증/인가 | 모든 관리 작업에 ACL 기반 접근 제어 적용 |
| 쿼터 보호 | ControllerMutationQuota로 과도한 관리 요청 차단 |
| 비동기 처리 | CompletableFuture 기반 비동기 파이프라인 |

---

## 3. Admin API 아키텍처

### 요청 처리 파이프라인

```
KafkaAdminClient.createTopics(topics)
    │
    │ CreateTopicsRequest 생성
    │ 컨트롤러 노드로 전송
    v
ControllerApis.handle(request)
    │
    │ ApiKeys 매칭
    │ case ApiKeys.CREATE_TOPICS => handleCreateTopics(request)
    v
handleCreateTopics(request)
    │
    ├── 1. 요청 역직렬화
    │     val createTopicsRequest = request.body[CreateTopicsRequest]
    │
    ├── 2. ControllerMutationQuota 검사
    │     val controllerMutationQuota = quotas.controllerMutation.newQuotaFor(
    │         request.session, request.header, strictSinceVersion=6)
    │
    ├── 3. 인가 검사
    │     authHelper.authorize(request.context, CREATE, CLUSTER, CLUSTER_NAME)
    │     authHelper.filterByAuthorized(request.context, CREATE, TOPIC, names)
    │
    ├── 4. Controller.createTopics() 호출
    │     controller.createTopics(context, effectiveRequest, describableTopicNames)
    │
    ├── 5. 결과 처리
    │     중복/인가 실패 토픽에 대한 에러 응답 추가
    │
    └── 6. 쓰로틀 적용 후 응답 전송
          requestHelper.sendResponseMaybeThrottleWithControllerQuota(
              controllerMutationQuota, request, response)
```

### ControllerApis에서 지원하는 Admin API

```
소스: core/src/main/scala/kafka/server/ControllerApis.scala
```

```scala
override def handle(request: RequestChannel.Request, requestLocal: RequestLocal): Unit = {
    val handlerFuture = request.header.apiKey match {
        case ApiKeys.CREATE_TOPICS  => handleCreateTopics(request)
        case ApiKeys.DELETE_TOPICS  => handleDeleteTopics(request)
        case ApiKeys.CREATE_PARTITIONS => handleCreatePartitions(request)
        case ApiKeys.ALTER_PARTITION_REASSIGNMENTS => handleAlterPartitionReassignments(request)
        case ApiKeys.CREATE_ACLS    => aclApis.handleCreateAcls(request)
        case ApiKeys.DELETE_ACLS    => aclApis.handleDeleteAcls(request)
        case ApiKeys.DESCRIBE_ACLS  => aclApis.handleDescribeAcls(request)
        case ApiKeys.ALTER_CONFIGS  => handleAlterConfigs(request)
        case ApiKeys.INCREMENTAL_ALTER_CONFIGS => handleIncrementalAlterConfigs(request)
        case ApiKeys.DESCRIBE_CONFIGS => handleDescribeConfigsRequest(request)
        case ApiKeys.ALTER_CLIENT_QUOTAS => handleAlterClientQuotas(request)
        case ApiKeys.ELECT_LEADERS  => handleElectLeaders(request)
        // ... 기타 API
    }
}
```

---

## 4. ControllerApis - 요청 핸들러

```
소스: core/src/main/scala/kafka/server/ControllerApis.scala
```

### 클래스 구조

```scala
class ControllerApis(
    val requestChannel: RequestChannel,           // 네트워크 요청 채널
    val authorizerPlugin: Option[Plugin[Authorizer]], // 인가 플러그인
    val quotas: QuotaManagers,                    // 쿼터 매니저들
    val time: Time,
    val controller: Controller,                   // QuorumController 참조
    val raftManager: RaftManager[ApiMessageAndVersion], // Raft 매니저
    val config: KafkaConfig,
    val clusterId: String,
    val registrationsPublisher: ControllerRegistrationsPublisher,
    val apiVersionManager: ApiVersionManager,
    val metadataCache: KRaftMetadataCache
) extends ApiRequestHandler with Logging {

    val authHelper = new AuthHelper(authorizerPlugin)
    val configHelper = new ConfigHelper(metadataCache, config, metadataCache)
    val requestHelper = new RequestHandlerHelper(requestChannel, quotas, time)
    private val aclApis = new AclApis(authHelper, authorizerPlugin, requestHelper,
                                       ProcessRole.ControllerRole, config)
}
```

### 인증/인가 흐름

모든 관리 요청은 인가 검사를 거친다:

```
┌─────────────────────────────────────────────────────────────┐
│                      인가 검사 흐름                           │
│                                                             │
│  1. 클러스터 수준 권한 검사                                   │
│     authHelper.authorize(context, CREATE, CLUSTER, ...)     │
│     → true: 모든 토픽에 대한 작업 가능                        │
│     → false: 토픽별 개별 검사                                │
│                                                             │
│  2. 토픽 수준 권한 검사                                      │
│     authHelper.filterByAuthorized(context, CREATE, TOPIC,   │
│         topicNames)                                         │
│     → 권한 있는 토픽만 반환                                   │
│                                                             │
│  3. 권한 없는 토픽에 대한 에러 응답                            │
│     TOPIC_AUTHORIZATION_FAILED                              │
└─────────────────────────────────────────────────────────────┘
```

---

## 5. 토픽 생성 (CreateTopics)

### ControllerApis 레이어

```scala
// 소스: core/src/main/scala/kafka/server/ControllerApis.scala

private def handleCreateTopics(request: RequestChannel.Request): CompletableFuture[Unit] = {
    val createTopicsRequest = request.body[CreateTopicsRequest]

    // ControllerMutationQuota: API 버전 6 이상이면 Strict 모드
    val controllerMutationQuota = quotas.controllerMutation.newQuotaFor(
        request.session, request.header, 6)

    val context = new ControllerRequestContext(
        request.context.header.data, request.context.principal,
        requestTimeoutMsToDeadlineNs(time, createTopicsRequest.data.timeoutMs),
        controllerMutationQuotaRecorderFor(controllerMutationQuota))

    val future = createTopics(context, createTopicsRequest.data,
        authHelper.authorize(request.context, CREATE, CLUSTER, CLUSTER_NAME, logIfDenied = false),
        names => authHelper.filterByAuthorized(request.context, CREATE, TOPIC, names)(identity),
        names => authHelper.filterByAuthorized(request.context, DESCRIBE_CONFIGS, TOPIC, names)(identity))

    future.handle[Unit] { (result, exception) =>
        val response = if (exception != null)
            createTopicsRequest.getErrorResponse(exception)
        else
            new CreateTopicsResponse(result)
        requestHelper.sendResponseMaybeThrottleWithControllerQuota(
            controllerMutationQuota, request, response)
    }
}
```

### createTopics 로직 상세

```scala
def createTopics(context: ControllerRequestContext,
                  request: CreateTopicsRequestData,
                  hasClusterAuth: Boolean,
                  getCreatableTopics: Iterable[String] => Set[String],
                  getDescribableTopics: Iterable[String] => Set[String]
): CompletableFuture[CreateTopicsResponseData] = {

    // 1. 중복 토픽 이름 검출
    val topicNames = new util.HashSet[String]()
    val duplicateTopicNames = new util.HashSet[String]()
    request.topics().forEach { topicData =>
        if (!topicNames.add(topicData.name())) {
            duplicateTopicNames.add(topicData.name())
        }
    }

    // 2. 내부 토픽 (__cluster_metadata) 생성 거부
    val allowedTopicNames = topicNames.diff(Set(Topic.CLUSTER_METADATA_TOPIC_NAME))

    // 3. 인가된 토픽만 필터링
    val authorizedTopicNames = if (hasClusterAuth) allowedTopicNames
                                else getCreatableTopics(allowedTopicNames)

    // 4. 권한 없는 토픽 제거
    val iterator = effectiveRequest.topics().iterator()
    while (iterator.hasNext) {
        val creatableTopic = iterator.next()
        if (duplicateTopicNames.contains(creatableTopic.name()) ||
            !authorizedTopicNames.contains(creatableTopic.name())) {
            iterator.remove()
        }
    }

    // 5. Controller에 생성 위임
    controller.createTopics(context, effectiveRequest, describableTopicNames)
}
```

### ReplicationControlManager 레이어

```
소스: metadata/src/main/java/org/apache/kafka/controller/ReplicationControlManager.java
```

`ReplicationControlManager`는 토픽/파티션의 **실제 상태를 관리**한다:

```java
static class TopicControlInfo {
    private final String name;
    private final Uuid id;
    private final TimelineHashMap<Integer, PartitionRegistration> parts;

    TopicControlInfo(String name, SnapshotRegistry snapshotRegistry, Uuid id) {
        this.name = name;
        this.id = id;
        this.parts = new TimelineHashMap<>(snapshotRegistry, 0);
    }
}
```

토픽 생성 시 생성되는 메타데이터 레코드:

```
TopicRecord {
    name: "my-topic"
    topicId: UUID
}

PartitionRecord {    (파티션 수만큼 반복)
    partitionId: 0
    topicId: UUID
    replicas: [1, 2, 3]
    isr: [1, 2, 3]
    leader: 1
    leaderEpoch: 0
    partitionEpoch: 0
}
```

### 파티션 배치 (Placement)

```
토픽 생성 시 파티션 배치 전략:

1. 사용자 지정 배치 (replicaAssignment 명시)
   → 사용자가 각 파티션의 복제본 위치를 직접 지정

2. 자동 배치 (기본)
   → ClusterDescriber를 통해 사용 가능한 브로커 조회
   → 랙(rack) 인식 배치 알고리즘 적용
   → 복제본이 서로 다른 랙에 분산되도록 배치

설정:
   defaultReplicationFactor = 3  (기본 복제 팩터)
   defaultNumPartitions = 1      (기본 파티션 수)
```

---

## 6. 토픽 삭제 (DeleteTopics)

```scala
// 소스: core/src/main/scala/kafka/server/ControllerApis.scala

private def handleDeleteTopics(request: RequestChannel.Request): CompletableFuture[Unit] = {
    val deleteTopicsRequest = request.body[DeleteTopicsRequest]
    val controllerMutationQuota = quotas.controllerMutation.newQuotaFor(
        request.session, request.header, 5)

    // 토픽 삭제 비활성화 검사
    if (!config.deleteTopicEnable) {
        return CompletableFuture.failedFuture(new TopicDeletionDisabledException())
    }

    // 토픽은 이름 또는 ID로 참조 가능
    // 1. 이름으로 제공된 토픽 수집
    // 2. ID로 제공된 토픽 → 이름 해석 필요
    // 3. 중복 검사
    // 4. 인가 검사 (DELETE 권한)
    // 5. controller.deleteTopics() 호출

    controller.findTopicNames(context, providedIds).thenCompose { topicNames =>
        // ID → 이름 매핑
        val deletableTopicNames = getDeletableTopics(toAuthenticate)
        controller.deleteTopics(context, deletableIds)
    }
}
```

### 토픽 삭제 흐름

```
DeleteTopics 요청
    │
    ├── 1. delete.topic.enable 검사
    │     → false이면 TopicDeletionDisabledException
    │
    ├── 2. 토픽 이름/ID 해석
    │     ├── 이름으로 참조: 그대로 사용
    │     └── ID로 참조: controller.findTopicNames() 호출
    │
    ├── 3. 중복 검사
    │     → "Duplicate topic name" / "Duplicate topic id" 에러
    │
    ├── 4. 인가 검사
    │     ├── DESCRIBE 권한: 토픽 존재 여부 확인 가능
    │     └── DELETE 권한: 토픽 삭제 가능
    │
    ├── 5. controller.deleteTopics(deletableIds) 호출
    │
    └── 6. __cluster_metadata에 RemoveTopicRecord 기록
          RemoveTopicRecord { topicId: UUID }
```

---

## 7. 파티션 생성 (CreatePartitions)

```scala
// 소스: core/src/main/scala/kafka/server/ControllerApis.scala

private def handleCreatePartitions(request: RequestChannel.Request): CompletableFuture[Unit] = {
    val createPartitionsRequest = request.body[CreatePartitionsRequest]
    val controllerMutationQuota = quotas.controllerMutation.newQuotaFor(
        request.session, request.header, 3)

    // ALTER 권한 검사
    val authorizedTopicNames = filterAlterAuthorizedTopics(topicNames)

    // Controller에 파티션 생성 위임
    controller.createPartitions(context, topics, request.validateOnly)
}
```

### 파티션 추가 검증

```
CreatePartitions 요청 검증:

1. 토픽 존재 여부 확인
   → UNKNOWN_TOPIC_OR_PARTITION

2. 파티션 수 감소 시도 검사
   → INVALID_PARTITIONS (파티션은 추가만 가능, 삭제 불가)

3. 새 파티션 수가 현재보다 큰지 확인
   → INVALID_PARTITIONS

4. 복제본 배치 검증 (사용자 지정 시)
   → INVALID_REPLICA_ASSIGNMENT

5. validateOnly=true이면 검증만 수행, 실제 생성 안 함
```

---

## 8. 파티션 재배치 (AlterPartitionReassignments)

```scala
// 소스: core/src/main/scala/kafka/server/ControllerApis.scala

def handleAlterPartitionReassignments(request: RequestChannel.Request): CompletableFuture[Unit] = {
    val alterRequest = request.body[AlterPartitionReassignmentsRequest]

    // CLUSTER ALTER 권한 필요 (토픽별 아닌 클러스터 수준)
    authHelper.authorizeClusterOperation(request, ALTER)

    // Controller에 재배치 위임
    controller.alterPartitionReassignments(context, alterRequest.data)
}
```

### 파티션 재배치 흐름

```
AlterPartitionReassignments 요청
    │
    ├── 재배치 시작 (replicas 지정)
    │   ├── 현재 복제본: [1, 2, 3]
    │   └── 목표 복제본: [4, 5, 6]
    │
    ├── ReplicationControlManager에서 처리
    │   ├── 1. ISR에 새 복제본 추가 (catch-up 시작)
    │   ├── 2. 새 복제본이 ISR에 합류할 때까지 대기
    │   ├── 3. 리더 변경 (필요 시)
    │   └── 4. 이전 복제본 제거
    │
    └── 재배치 취소 (replicas=null)
        └── 진행 중인 재배치를 중단하고 원래 상태로 복원

메타데이터 레코드:
  PartitionChangeRecord {
      topicId: UUID
      partitionId: 0
      replicas: [1, 2, 3, 4, 5, 6]   // 일시적으로 양쪽 모두 포함
      isr: [1, 2, 3]
      addingReplicas: [4, 5, 6]
      removingReplicas: [1, 2, 3]
  }
```

---

## 9. ACL 관리 (AclControlManager)

```
소스: metadata/src/main/java/org/apache/kafka/controller/AclControlManager.java
```

### 데이터 구조

```java
public class AclControlManager {
    private final TimelineHashMap<Uuid, StandardAcl> idToAcl;     // UUID → ACL 매핑
    private final TimelineHashSet<StandardAcl> existingAcls;       // 중복 검사용 세트
}
```

### ACL 생성

```java
ControllerResult<List<AclCreateResult>> createAcls(List<AclBinding> acls) {
    Set<StandardAcl> aclsToCreate = new HashSet<>(acls.size());
    List<AclCreateResult> results = new ArrayList<>(acls.size());
    List<ApiMessageAndVersion> records = BoundedList.newArrayBacked(MAX_RECORDS_PER_USER_OP);

    for (AclBinding acl : acls) {
        // 1. ACL 유효성 검증
        validateNewAcl(acl);

        // 2. StandardAcl로 변환
        StandardAcl standardAcl = StandardAcl.fromAclBinding(acl);

        // 3. 이미 존재하는지 확인
        if (!existingAcls.contains(standardAcl)) {
            if (aclsToCreate.add(standardAcl)) {
                // 4. 메타데이터 레코드 생성
                StandardAclWithId standardAclWithId = new StandardAclWithId(newAclId(), standardAcl);
                records.add(new ApiMessageAndVersion(standardAclWithId.toRecord(), (short) 0));
            }
        }
        results.add(AclCreateResult.SUCCESS);
    }
    return new ControllerResult<>(records, results, true);
}
```

### ACL 검증 규칙

```java
static void validateNewAcl(AclBinding binding) {
    // 리소스 타입 검증: UNKNOWN, ANY 불허
    switch (binding.pattern().resourceType()) {
        case UNKNOWN: case ANY:
            throw new InvalidRequestException("Invalid resourceType");
    }

    // 패턴 타입 검증: LITERAL, PREFIXED만 허용
    switch (binding.pattern().patternType()) {
        case LITERAL: case PREFIXED: break;
        default: throw new InvalidRequestException("Invalid patternType");
    }

    // 작업 검증: UNKNOWN, ANY 불허
    switch (binding.entry().operation()) {
        case UNKNOWN: case ANY:
            throw new InvalidRequestException("Invalid operation");
    }

    // 권한 타입 검증: DENY, ALLOW만 허용
    switch (binding.entry().permissionType()) {
        case DENY: case ALLOW: break;
        default: throw new InvalidRequestException("Invalid permissionType");
    }

    // 리소스 이름 빈 값 검증
    if (binding.pattern().name() == null || binding.pattern().name().isEmpty()) {
        throw new InvalidRequestException("Resource name should not be empty");
    }

    // Principal 형식 검증 ("Type:Name" 형식)
    int colonIndex = binding.entry().principal().indexOf(":");
    if (colonIndex == -1) {
        throw new InvalidRequestException("Could not parse principal");
    }
}
```

### ACL 삭제

```java
ControllerResult<List<AclDeleteResult>> deleteAcls(List<AclBindingFilter> filters) {
    List<AclDeleteResult> results = new ArrayList<>();
    Set<ApiMessageAndVersion> records = new HashSet<>();

    for (AclBindingFilter filter : filters) {
        validateFilter(filter);
        AclDeleteResult result = deleteAclsForFilter(filter, records);
        results.add(result);
    }
    return ControllerResult.atomicOf(new ArrayList<>(records), results);
}

AclDeleteResult deleteAclsForFilter(AclBindingFilter filter, Set<ApiMessageAndVersion> records) {
    List<AclBindingDeleteResult> deleted = new ArrayList<>();
    for (Entry<Uuid, StandardAcl> entry : idToAcl.entrySet()) {
        AclBinding binding = entry.getValue().toBinding();
        if (filter.matches(binding)) {
            // 삭제 상한 검사 (MAX_RECORDS_PER_USER_OP)
            if (records.size() >= MAX_RECORDS_PER_USER_OP) {
                throw new BoundedListTooLongException("Cannot remove more than " +
                    MAX_RECORDS_PER_USER_OP + " acls in a single delete operation.");
            }
            deleted.add(new AclBindingDeleteResult(binding));
            records.add(new ApiMessageAndVersion(
                new RemoveAccessControlEntryRecord().setId(entry.getKey()), (short) 0));
        }
    }
    return new AclDeleteResult(deleted);
}
```

### ACL 메타데이터 레코드

```
생성: AccessControlEntryRecord {
    id: UUID
    resourceType: TOPIC
    resourceName: "my-topic"
    patternType: LITERAL
    principal: "User:alice"
    host: "*"
    operation: READ
    permissionType: ALLOW
}

삭제: RemoveAccessControlEntryRecord {
    id: UUID
}
```

---

## 10. ReplicationControlManager - 토픽/파티션 컨트롤러

```
소스: metadata/src/main/java/org/apache/kafka/controller/ReplicationControlManager.java
```

### 핵심 상태

```java
public class ReplicationControlManager {
    static final int MAX_ELECTIONS_PER_IMBALANCE = 1_000;
    static final int MAX_PARTITIONS_PER_BATCH = 10_000;

    private final SnapshotRegistry snapshotRegistry;
    private final short defaultReplicationFactor;        // 기본 복제 팩터 (3)
    private final int defaultNumPartitions;              // 기본 파티션 수 (1)
    private final int maxElectionsPerImbalance;          // 불균형 시 최대 리더 선출 수
    private final ConfigurationControlManager configurationControl;
    private final ClusterControlManager clusterControl;
    private final Optional<CreateTopicPolicy> createTopicPolicy;
    private final FeatureControlManager featureControl;

    // 토픽 상태 저장
    // topicsByName: name → TopicControlInfo
    // topicsById: UUID → TopicControlInfo
}
```

### TopicControlInfo

```java
static class TopicControlInfo {
    private final String name;
    private final Uuid id;
    private final TimelineHashMap<Integer, PartitionRegistration> parts;
    // 각 파티션의 복제본, ISR, 리더, 에포크 정보를 관리
}
```

### 파티션 배치 알고리즘

```
파티션 배치 시 고려 사항:

1. 랙(Rack) 분산
   ┌─────────┐   ┌─────────┐   ┌─────────┐
   │ Rack-A  │   │ Rack-B  │   │ Rack-C  │
   │ Broker1 │   │ Broker2 │   │ Broker3 │
   │ Broker4 │   │ Broker5 │   │ Broker6 │
   └─────────┘   └─────────┘   └─────────┘

   토픽 T (3 파티션, 복제팩터 3):
     P0: [Broker1(A), Broker2(B), Broker3(C)]  ← 모든 랙에 분산
     P1: [Broker5(B), Broker6(C), Broker4(A)]  ← 리더 분산
     P2: [Broker3(C), Broker1(A), Broker5(B)]  ← 리더 분산

2. 리더 분산
   각 파티션의 리더(첫 번째 복제본)가 다른 브로커에 위치

3. 디렉토리 인식 (KRaft)
   featureControl.metadataVersionOrThrow().isDirectoryAssignmentSupported()
   → 브로커 내 로그 디렉토리까지 고려한 배치
```

---

## 11. ControllerMutationQuota와 Admin API

```
소스: server/src/main/java/org/apache/kafka/server/quota/ControllerMutationQuotaManager.java
```

### 쿼터 적용 방식

Admin API 요청은 `ControllerMutationQuota`로 보호된다:

```scala
// ControllerApis에서 각 Admin API 호출 시:
val controllerMutationQuota = quotas.controllerMutation.newQuotaFor(
    request.session, request.header, strictSinceVersion)
```

`strictSinceVersion`은 API별로 다르다:

| API | strictSinceVersion | 의미 |
|-----|-------------------|------|
| CreateTopics | 6 | API v6 이상에서 Strict (즉시 거부) |
| DeleteTopics | 5 | API v5 이상에서 Strict |
| CreatePartitions | 3 | API v3 이상에서 Strict |

### Strict vs Permissive 동작

```
Strict 모드 (최신 API 버전):
  쿼터 초과 → THROTTLING_QUOTA_EXCEEDED 에러 즉시 반환
  → 요청이 처리되지 않음

Permissive 모드 (이전 API 버전):
  쿼터 초과 → 요청은 처리하되 응답에 throttleTimeMs 포함
  → 클라이언트가 알아서 대기

Unbounded 모드 (쿼터 비활성화):
  → 제한 없이 처리
```

### 쿼터 소비 기록

```scala
// 토픽 생성 시 파티션 수만큼 쿼터 소비
val context = new ControllerRequestContext(
    request.context.header.data,
    request.context.principal,
    requestTimeoutMsToDeadlineNs(time, createTopicsRequest.data.timeoutMs),
    controllerMutationQuotaRecorderFor(controllerMutationQuota)
)

private def controllerMutationQuotaRecorderFor(quota: ControllerMutationQuota) = {
    new Consumer[lang.Integer]() {
        override def accept(permits: lang.Integer): Unit = {
            quota.record(permits.doubleValue())  // 파티션 수를 permits로 기록
        }
    }
}
```

예: 토픽 3개(각 10파티션) 생성 요청 → 쿼터 소비 = 30 permits

---

## 12. KafkaAdminClient - 클라이언트 라이브러리

```
소스: clients/src/main/java/org/apache/kafka/clients/admin/KafkaAdminClient.java
```

### 사용 예시

```java
// Admin 클라이언트 생성
Properties props = new Properties();
props.put(AdminClientConfig.BOOTSTRAP_SERVERS_CONFIG, "localhost:9092");
AdminClient admin = AdminClient.create(props);

// 토픽 생성
NewTopic topic = new NewTopic("my-topic", 3, (short) 3);
admin.createTopics(List.of(topic)).all().get();

// 토픽 삭제
admin.deleteTopics(List.of("my-topic")).all().get();

// 파티션 추가
admin.createPartitions(Map.of("my-topic", NewPartitions.increaseTo(6))).all().get();

// ACL 생성
AclBinding binding = new AclBinding(
    new ResourcePattern(ResourceType.TOPIC, "my-topic", PatternType.LITERAL),
    new AccessControlEntry("User:alice", "*", AclOperation.READ, AclPermissionType.ALLOW));
admin.createAcls(List.of(binding)).all().get();

// ACL 삭제
AclBindingFilter filter = new AclBindingFilter(
    new ResourcePatternFilter(ResourceType.TOPIC, "my-topic", PatternType.LITERAL),
    AccessControlEntryFilter.ANY);
admin.deleteAcls(List.of(filter)).all().get();

// 파티션 재배치
Map<TopicPartition, Optional<NewPartitionReassignment>> reassignments = Map.of(
    new TopicPartition("my-topic", 0),
    Optional.of(new NewPartitionReassignment(List.of(4, 5, 6))));
admin.alterPartitionReassignments(reassignments).all().get();
```

### 요청 전송 흐름

```
KafkaAdminClient.createTopics(topics)
    │
    ├── 1. CreateTopicsRequest 생성
    │     CreateTopicsRequestData {
    │         topics: [
    │             CreatableTopic { name: "my-topic", numPartitions: 3,
    │                              replicationFactor: 3, configs: [...] }
    │         ]
    │         timeoutMs: 30000
    │         validateOnly: false
    │     }
    │
    ├── 2. 컨트롤러 노드 탐색
    │     metadata.fetch() → controller node 확인
    │
    ├── 3. 네트워크 전송
    │     NetworkClient.send(controllerNode, request)
    │
    ├── 4. 응답 수신
    │     CreateTopicsResponseData {
    │         topics: [
    │             CreatableTopicResult { name: "my-topic", errorCode: 0,
    │                                   topicId: UUID, numPartitions: 3,
    │                                   replicationFactor: 3 }
    │         ]
    │         throttleTimeMs: 0
    │     }
    │
    └── 5. KafkaFuture 완료
          CreateTopicsResult.all().get()  → 성공 반환
```

---

## 13. 왜(Why) 이렇게 설계했는가

### Q1: 왜 Admin API 요청을 컨트롤러에서만 처리하는가?

토픽, 파티션, ACL 같은 **클러스터 전체 상태**를 변경하는 작업은 **단일 조정자(single coordinator)**에서 처리해야 일관성이 보장된다.

- **ZooKeeper 시절**: 여러 브로커가 ZK에 동시 쓰기 → 분산 잠금 필요, 복잡한 장애 처리
- **KRaft 시절**: 컨트롤러가 Raft 로그에 순차 기록 → 단일 스레드 처리, 충돌 없음

모든 상태 변경이 `__cluster_metadata` 토픽을 통해 **순서화(linearized)**되므로, 동시 요청 간 충돌이 원천적으로 방지된다.

### Q2: 왜 CompletableFuture 기반 비동기 처리인가?

Admin API 요청은 Raft 커밋을 기다려야 하므로 **blocking I/O가 필연적**이다. 요청 핸들러 스레드를 블로킹하면 다른 요청 처리가 지연된다.

`CompletableFuture` 기반으로:
- 요청 핸들러 스레드가 즉시 반환
- Raft 커밋 완료 시 콜백으로 응답 전송
- 더 많은 동시 Admin 요청 처리 가능

### Q3: 왜 ControllerMutationQuota에 Strict/Permissive 모드가 있는가?

이전 버전 클라이언트와의 호환성 때문이다:

- **Strict (최신)**: 쿼터 초과 시 `THROTTLING_QUOTA_EXCEEDED` 에러 반환 → 클라이언트가 에러를 처리할 수 있어야 함
- **Permissive (이전)**: 요청은 처리하되 응답에 `throttleTimeMs`를 포함 → 이전 버전 클라이언트도 쓰로틀 시간을 이해할 수 있음

`strictSinceVersion`을 API별로 다르게 설정하여, API 버전 업그레이드 시 점진적으로 Strict 모드를 적용한다.

### Q4: 왜 ACL에 BoundedList(MAX_RECORDS_PER_USER_OP) 제한이 있는가?

```java
if (records.size() >= MAX_RECORDS_PER_USER_OP) {
    throw new BoundedListTooLongException("Cannot remove more than " +
        MAX_RECORDS_PER_USER_OP + " acls in a single delete operation.");
}
```

Raft 로그에 한 번에 너무 많은 레코드를 쓰면:
1. **메모리 압박**: 수만 개의 ACL 삭제 → 수만 개의 레코드가 메모리에 버퍼링
2. **커밋 지연**: 대량 레코드 복제 시 팔로워 지연 증가
3. **원자성 실패 영향**: 한 번에 처리하는 레코드가 많을수록 실패 시 영향 범위 확대

상한을 두어 **한 번의 사용자 작업이 시스템에 미치는 영향을 제한**한다.

### Q5: 왜 토픽 삭제 시 이름과 ID 모두 지원하는가?

- **이름 기반**: 사람이 읽기 쉽고, 기존 도구와 호환
- **ID 기반**: 토픽 이름이 재사용될 수 있으므로, 정확한 토픽을 식별하려면 UUID 필요

예: 토픽 "orders"를 삭제하고 같은 이름으로 재생성하면, 이전 토픽의 잔여 데이터와 새 토픽이 혼동될 수 있다. UUID로 식별하면 이런 문제가 방지된다.

---

## 참고 자료

- KIP-500: Apache Kafka Without ZooKeeper (KRaft)
- KIP-599: Throttle Create Topic, Create Partition and Delete Topic Operations
- KIP-664: Provide tooling to detect and abort stuck partition reassignments
- 소스코드: `core/src/main/scala/kafka/server/ControllerApis.scala`
- 소스코드: `metadata/src/main/java/org/apache/kafka/controller/`
