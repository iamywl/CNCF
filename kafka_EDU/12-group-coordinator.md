# 12. 그룹 코디네이터 Deep-Dive

## 목차

1. [개요](#1-개요)
2. [아키텍처: Service -> Shard -> Manager](#2-아키텍처-service---shard---manager)
3. [Classic 그룹 상태 머신](#3-classic-그룹-상태-머신)
4. [JoinGroup 흐름](#4-joingroup-흐름)
5. [SyncGroup 흐름](#5-syncgroup-흐름)
6. [Consumer 그룹 (KIP-848)](#6-consumer-그룹-kip-848)
7. [__consumer_offsets 토픽](#7-__consumer_offsets-토픽)
8. [파티션 할당 전략](#8-파티션-할당-전략)
9. [Share 그룹 (KIP-932)](#9-share-그룹-kip-932)
10. [OffsetMetadataManager](#10-offsetmetadatamanager)
11. [CoordinatorRuntime 패턴](#11-coordinatorruntime-패턴)
12. [설계 결정: Why?](#12-설계-결정-why)

---

## 1. 개요

**그룹 코디네이터(Group Coordinator)**는 Kafka의 컨슈머 그룹 관리를 담당하는 핵심
서버 측 컴포넌트이다. 컨슈머들이 토픽의 파티션을 분배받아 병렬로 소비하려면 "누가 어떤
파티션을 처리할지" 합의하는 메커니즘이 필요한데, 이 역할을 그룹 코디네이터가 수행한다.

### 소스 위치

| 컴포넌트 | 소스 경로 |
|----------|----------|
| GroupCoordinatorService | `group-coordinator/src/main/java/org/apache/kafka/coordinator/group/GroupCoordinatorService.java` |
| GroupCoordinatorShard | `group-coordinator/src/main/java/org/apache/kafka/coordinator/group/GroupCoordinatorShard.java` |
| GroupMetadataManager | `group-coordinator/src/main/java/org/apache/kafka/coordinator/group/GroupMetadataManager.java` |
| OffsetMetadataManager | `group-coordinator/src/main/java/org/apache/kafka/coordinator/group/OffsetMetadataManager.java` |
| ClassicGroup | `group-coordinator/src/main/java/org/apache/kafka/coordinator/group/classic/ClassicGroup.java` |
| ClassicGroupState | `group-coordinator/src/main/java/org/apache/kafka/coordinator/group/classic/ClassicGroupState.java` |
| ConsumerGroup | `group-coordinator/src/main/java/org/apache/kafka/coordinator/group/modern/consumer/ConsumerGroup.java` |
| CoordinatorRuntime | `coordinator-common/src/main/java/org/apache/kafka/coordinator/common/runtime/CoordinatorRuntime.java` |
| TargetAssignmentBuilder | `group-coordinator/src/main/java/org/apache/kafka/coordinator/group/modern/TargetAssignmentBuilder.java` |

---

## 2. 아키텍처: Service -> Shard -> Manager

### 2.1 3계층 구조

그룹 코디네이터는 3계층으로 나뉜다.

```
요청 흐름:

KafkaApis (네트워크 계층)
    |
    v
GroupCoordinatorService (서비스 계층)
    |-- CoordinatorRuntime을 통해 파티션별 라우팅
    |
    v
GroupCoordinatorShard (샤드 계층)
    |-- __consumer_offsets 파티션 하나에 대응
    |-- 레코드 직렬화/역직렬화
    |
    v
GroupMetadataManager (그룹 관리)  +  OffsetMetadataManager (오프셋 관리)
    |-- 그룹 상태 머신                |-- 오프셋 커밋/조회
    |-- JoinGroup/SyncGroup 처리     |-- 오프셋 만료
    |-- 하트비트 처리                 |-- 트랜잭셔널 오프셋
```

### 2.2 GroupCoordinatorService

`GroupCoordinatorService`는 외부 요청의 진입점이다. CoordinatorRuntime을 사용하여
요청을 올바른 샤드로 라우팅한다.

```
GroupCoordinatorService 핵심 메서드:
+-------------------------------------------------------+
| consumerGroupHeartbeat()  - KIP-848 하트비트           |
| joinGroup()               - Classic JoinGroup          |
| syncGroup()               - Classic SyncGroup          |
| heartbeat()               - Classic 하트비트           |
| leaveGroup()              - 그룹 탈퇴                  |
| commitOffsets()            - 오프셋 커밋               |
| fetchOffsets()             - 오프셋 조회               |
| deleteGroups()             - 그룹 삭제                 |
| listGroups()               - 그룹 목록 조회            |
| shareGroupHeartbeat()      - KIP-932 하트비트          |
+-------------------------------------------------------+
```

### 2.3 GroupCoordinatorShard

각 샤드는 `__consumer_offsets`의 하나의 파티션에 대응하며, 해당 파티션에 속하는
모든 그룹의 상태를 관리한다.

```java
// GroupCoordinatorShard.java - 주요 멤버
public class GroupCoordinatorShard
    implements CoordinatorShard<CoordinatorRecord> {

    private final GroupMetadataManager groupMetadataManager;
    private final OffsetMetadataManager offsetMetadataManager;
    private final GroupConfigManager groupConfigManager;
    private final CoordinatorMetricsShard metricsShard;
}
```

```
그룹 -> 샤드 매핑:

__consumer_offsets 토픽 (기본 50 파티션):

그룹 "group-A" -> hash("group-A") % 50 = 12
                   -> __consumer_offsets-12
                   -> Shard 12

그룹 "group-B" -> hash("group-B") % 50 = 37
                   -> __consumer_offsets-37
                   -> Shard 37

각 Shard는 해당 파티션의 리더 브로커에서 실행
```

### 2.4 GroupMetadataManager

GroupMetadataManager는 실제 그룹 상태를 관리하는 핵심 클래스이다.
모든 그룹 유형(Classic, Consumer, Share, Streams)을 처리한다.

```java
// GroupMetadataManager.java - 그룹 저장소
// groups: 모든 그룹의 in-memory 상태
// classicGroupJoin(): Classic 프로토콜 JoinGroup 처리
// consumerGroupHeartbeat(): Consumer 프로토콜 하트비트 처리
// shareGroupHeartbeat(): Share 프로토콜 하트비트 처리
```

---

## 3. Classic 그룹 상태 머신

### 3.1 상태 정의

Classic 그룹은 5개 상태를 갖는다 (`ClassicGroupState.java`):

```
Classic 그룹 상태 머신:

                  +-------------------+
                  |      EMPTY        |
                  | (멤버 없음,       |
                  |  오프셋만 존재)    |
                  +---+-------+-------+
                      |       ^
        join group    |       | 모든 멤버 탈퇴
        from new      |       |
        member        v       |
                  +---+-------+-------+
            +---->| PREPARING_REBALANCE|<----+
            |     | (멤버 수집 중,     |     |
            |     |  JoinGroup 대기)   |     |
            |     +---+-------+-------+     |
            |         |                     |
            |  타임아웃 또는    새 멤버 참여  |
            |  모든 멤버 참여   또는 멤버 탈퇴|
            |         |                     |
            |         v                     |
            |     +---+-------+-------+     |
            |     | COMPLETING_REBALANCE    |
            |     | (리더의 SyncGroup       |
            +-----+  대기 중)          +----+
                  +---+-------+-------+
                      |
              리더의 SyncGroup
              수신 (할당 포함)
                      |
                      v
                  +---+-------+-------+
                  |      STABLE       |
                  | (정상 운영 중,    |
                  |  하트비트 수신)    |
                  +---+-------+-------+
                      |       |
        멤버 실패     |       | 멤버 탈퇴/
        (하트비트 X)  |       | 메타데이터 변경
                      v       v
                  PREPARING_REBALANCE

                  +-------------------+
                  |      DEAD         |
                  | (그룹 메타데이터  |
                  |  제거 대기)       |
                  +-------------------+
```

### 3.2 상태 전이 규칙

`ClassicGroupState.java` (라인 109-114)에 정의된 전이 규칙:

```java
static {
    EMPTY.addValidPreviousStates(PREPARING_REBALANCE);
    PREPARING_REBALANCE.addValidPreviousStates(
        STABLE, COMPLETING_REBALANCE, EMPTY);
    COMPLETING_REBALANCE.addValidPreviousStates(PREPARING_REBALANCE);
    STABLE.addValidPreviousStates(COMPLETING_REBALANCE);
    DEAD.addValidPreviousStates(
        STABLE, PREPARING_REBALANCE, COMPLETING_REBALANCE, EMPTY, DEAD);
}
```

```
상태 전이 테이블:

현재 상태              -> 가능한 다음 상태
-------------------------------------------------
EMPTY                  -> PREPARING_REBALANCE, DEAD
PREPARING_REBALANCE    -> COMPLETING_REBALANCE, EMPTY, DEAD
COMPLETING_REBALANCE   -> STABLE, PREPARING_REBALANCE, DEAD
STABLE                 -> PREPARING_REBALANCE, DEAD
DEAD                   -> (없음, 최종 상태)
```

### 3.3 각 상태에서의 요청 처리

```
+----------------------+--------------------------------------------------+
| 상태                 | 요청 처리 방식                                    |
+----------------------+--------------------------------------------------+
| EMPTY                | JoinGroup: 새 멤버 등록, PREPARING_REBALANCE 전이  |
|                      | SyncGroup: UNKNOWN_MEMBER_ID 에러                |
|                      | Heartbeat: UNKNOWN_MEMBER_ID 에러                |
|                      | OffsetCommit: UNKNOWN_MEMBER_ID 에러             |
|                      | OffsetFetch: 정상 처리                            |
+----------------------+--------------------------------------------------+
| PREPARING_REBALANCE  | JoinGroup: 멤버 등록, 응답 보류(park)             |
|                      | SyncGroup: REBALANCE_IN_PROGRESS 에러            |
|                      | Heartbeat: REBALANCE_IN_PROGRESS 에러            |
|                      | OffsetCommit: 이전 세대 허용                     |
|                      | LeaveGroup: 멤버 제거                            |
+----------------------+--------------------------------------------------+
| COMPLETING_REBALANCE | JoinGroup: 새 멤버 -> PREPARING_REBALANCE 재전이  |
|                      | SyncGroup: 리더는 할당 전송, 팔로워는 대기       |
|                      | Heartbeat: REBALANCE_IN_PROGRESS 에러            |
|                      | OffsetCommit: REBALANCE_IN_PROGRESS 에러         |
+----------------------+--------------------------------------------------+
| STABLE               | JoinGroup: 리더 -> 리밸런스, 팔로워 -> 확인      |
|                      | SyncGroup: 현재 할당 반환                        |
|                      | Heartbeat: 정상 응답                             |
|                      | OffsetCommit: 현재 세대 확인 후 처리             |
+----------------------+--------------------------------------------------+
| DEAD                 | 모든 요청: UNKNOWN_MEMBER_ID 에러                |
+----------------------+--------------------------------------------------+
```

---

## 4. JoinGroup 흐름

### 4.1 classicGroupJoin() 진입점

`GroupMetadataManager.java` 라인 6188의 `classicGroupJoin()`은 JoinGroup 요청의
진입점이다.

```java
// GroupMetadataManager.java (라인 6188-6216)
public CoordinatorResult<Void, CoordinatorRecord> classicGroupJoin(
    AuthorizableRequestContext context,
    JoinGroupRequestData request,
    CompletableFuture<JoinGroupResponseData> responseFuture) {

    Group group = groups.get(request.groupId(), Long.MAX_VALUE);

    if (group != null && group.type() == CONSUMER && !group.isEmpty()) {
        // 비어있지 않은 Consumer 그룹 -> Consumer 프로토콜로 전환
        return classicGroupJoinToConsumerGroup(
            (ConsumerGroup) group, context, request, responseFuture);
    } else if (group == null || group.type() == CLASSIC ||
               group.type() == CONSUMER || ...) {
        // Classic 그룹 또는 새 그룹
        return classicGroupJoinToClassicGroup(
            context, request, responseFuture);
    }
}
```

### 4.2 JoinGroup 세부 흐름

```
JoinGroup 처리 흐름:

classicGroupJoin()
    |
    +-- 그룹 존재?
        |-- NO -> classicGroupJoinToClassicGroup()
        |         |-- 새 ClassicGroup 생성
        |         |-- 첫 번째 멤버를 리더로 설정
        |
        |-- YES -> 기존 멤버 or 신규 멤버?
            |
            |-- 신규 멤버 (memberId == "")
            |   |-- classicGroupJoinNewMember()
            |   |   |-- 고유 memberId 생성
            |   |   |   (clientId + "-" + UUID)
            |   |   |-- Static 멤버?
            |   |   |   |-- YES -> classicGroupJoinNewStaticMember()
            |   |   |   |-- NO  -> classicGroupJoinNewDynamicMember()
            |   |   |-- 그룹에 멤버 추가
            |   |   |-- 리밸런스 트리거
            |
            |-- 기존 멤버 (memberId != "")
                |-- classicGroupJoinExistingMember()
                |   |-- memberId 검증
                |   |-- 프로토콜 메타데이터 업데이트
                |   |-- 필요시 리밸런스 트리거
```

### 4.3 리더 선출

```
리더 선출 규칙:

1. 첫 번째 JoinGroup 요청을 보낸 멤버가 리더
2. 리밸런스 시, 기존 리더가 다시 참여하면 리더 유지
3. 기존 리더가 참여하지 않으면 첫 번째 참여 멤버가 새 리더

리더의 특권:
- JoinGroup 응답에 전체 멤버 목록 포함
- SyncGroup에서 파티션 할당 결과를 제출해야 함

팔로워의 JoinGroup 응답:
- 멤버 목록 비어있음
- 리더 ID만 포함
```

### 4.4 타이머 관리

```
JoinGroup 관련 타이머:

1. rebalanceTimeout (rebalance.timeout.ms):
   +-----------------------------------------------+
   | PREPARING_REBALANCE 진입 시 타이머 시작        |
   | -> 타임아웃: 현재까지 참여한 멤버만으로 진행   |
   | -> 미참여 dynamic 멤버 제거                    |
   +-----------------------------------------------+

2. initialRebalanceDelay (group.initial.rebalance.delay.ms):
   +-----------------------------------------------+
   | 새 그룹의 첫 리밸런스에 지연 추가              |
   | -> 여러 멤버가 거의 동시에 참여할 때           |
   |    불필요한 다중 리밸런스 방지                  |
   | -> 기본값: 3초                                 |
   +-----------------------------------------------+
```

**왜 initialRebalanceDelay가 필요한가?** 애플리케이션을 배포하면 여러 인스턴스가
거의 동시에 시작한다. 지연 없이는 첫 번째 인스턴스가 참여하자마자 리밸런스가 시작되고,
두 번째 인스턴스가 참여하면 또 리밸런스가 시작되는 "리밸런스 폭풍"이 발생한다.
초기 지연은 이 문제를 완화한다.

---

## 5. SyncGroup 흐름

### 5.1 SyncGroup 처리

```
SyncGroup 흐름:

Consumer A (리더)             Coordinator          Consumer B (팔로워)
     |                            |                      |
     |-- SyncGroup(assignment) -->|                      |
     |   { P0->A, P1->B }        |<-- SyncGroup(empty) -|
     |                            |                      |
     |                  [할당 결과를 __consumer_offsets에 기록]
     |                            |                      |
     |                  [그룹 상태: COMPLETING_REBALANCE -> STABLE]
     |                            |                      |
     |<-- SyncResponse -----------|---------- SyncResponse -->|
     |   assignment: [P0]         |         assignment: [P1]  |
     |                            |                      |
```

### 5.2 __consumer_offsets 기록

SyncGroup 시 다음 레코드가 `__consumer_offsets`에 기록된다:

```
GroupMetadata 레코드 구조:

Key: GroupMetadataKey {
    groupId: "my-group"
}

Value: GroupMetadataValue {
    protocolType: "consumer",
    generation: 3,
    protocol: "range",
    leader: "consumer-1-xxx",
    currentStateTimestamp: 1709123456789,
    members: [
        {
            memberId: "consumer-1-xxx",
            groupInstanceId: null,
            clientId: "consumer-1",
            clientHost: "/192.168.1.10",
            rebalanceTimeout: 300000,
            sessionTimeout: 45000,
            subscription: <bytes>,    // 구독 토픽 목록
            assignment: <bytes>       // 할당된 파티션 목록
        },
        {
            memberId: "consumer-2-yyy",
            ...
        }
    ]
}
```

### 5.3 하트비트

```
하트비트 처리:

Consumer                      Coordinator
    |                              |
    |-- Heartbeat(memberId,        |
    |   generationId) ------------>|
    |                              |-- memberId 검증
    |                              |-- generationId 검증
    |                              |-- 세션 타이머 리셋
    |                              |
    |                              |-- 리밸런스 진행 중?
    |<-- HeartbeatResponse --------|
         errorCode:
         - NONE (정상)
         - REBALANCE_IN_PROGRESS
           (리밸런스 필요, rejoin 해야 함)
         - UNKNOWN_MEMBER_ID
           (세션 만료됨)
         - ILLEGAL_GENERATION
           (세대 불일치)
```

---

## 6. Consumer 그룹 (KIP-848)

### 6.1 새 프로토콜의 동기

Classic 프로토콜의 문제점:

```
Classic 프로토콜 문제점:

1. Stop-the-World 리밸런스:
   +----------------------------------------------+
   | 리밸런스 시작 -> 모든 컨슈머 파티션 해제       |
   | -> 모든 컨슈머 정지 -> 새 할당 -> 재시작      |
   | => 전체 그룹이 일시 정지 (수십 초~분)          |
   +----------------------------------------------+

2. 클라이언트 측 할당:
   +----------------------------------------------+
   | 리더 컨슈머가 할당 계산                        |
   | -> 리더 장애 시 할당 실패                      |
   | -> 비표준 할당기 버전 불일치 위험              |
   +----------------------------------------------+

3. poll() 의존적 하트비트:
   +----------------------------------------------+
   | poll() 호출 지연 -> 하트비트 지연             |
   | -> 불필요한 리밸런스 발생                      |
   +----------------------------------------------+
```

### 6.2 Consumer 그룹 상태 머신

Consumer 그룹 (`ConsumerGroup.java` 라인 84-89)의 상태:

```
Consumer 그룹 상태 머신 (KIP-848):

    +-------------------+
    |      EMPTY        |
    | (멤버 없음)       |
    +---+-------+-------+
        |       ^
   하트비트     | 모든 멤버 탈퇴
   (첫 멤버)    |
        v       |
    +---+-------+-------+
    |    ASSIGNING       |
    | (서버가 파티션     |
    |  할당 계산 중)     |
    +---+-------+-------+
        |       ^
   할당 완료    | 멤버 변경
   (일부 멤버에  | (새 멤버 참여,
   할당 전달)    |  구독 변경)
        v       |
    +---+-------+-------+
    |   RECONCILING      |
    | (멤버들이 새 할당  |
    |  적용 중)          |
    +---+-------+-------+
        |
   모든 멤버가
   새 할당 확인
        |
        v
    +---+-------+-------+
    |     STABLE         |
    | (정상 운영 중)     |
    +---+-------+-------+

    +---+-------+-------+
    |      DEAD          |
    | (그룹 제거 대기)   |
    +-------------------+
```

### 6.3 ConsumerGroupHeartbeat 프로토콜

```
서버 측 할당 흐름:

Consumer                          Coordinator
    |                                  |
    |-- ConsumerGroupHeartbeat ------->|
    |   {                              |
    |     groupId: "my-group",         |
    |     memberId: "xxx",             |
    |     memberEpoch: 0,              |  [1] 멤버 등록/업데이트
    |     subscribedTopics: ["t1"],    |  [2] 파티션 할당 계산
    |     topicPartitions: [...]       |  [3] 타겟 할당 결정
    |   }                              |
    |                                  |
    |<-- HeartbeatResponse ------------|
    |   {                              |
    |     memberId: "xxx",             |
    |     memberEpoch: 1,              |
    |     assignment: {                |
    |       topicPartitions: [         |
    |         {topicId: ...,           |
    |          partitions: [0, 1]}     |
    |       ]                          |
    |     }                            |
    |   }                              |
```

### 6.4 Reconciliation (점진적 할당)

```
Cooperative 리밸런스 (Stop-the-World 없음):

단계 1: 현재 상태
  Consumer A: [P0, P1, P2]
  Consumer B: [P3, P4]

단계 2: Consumer C 참여, 새 할당 계산
  Target: A:[P0, P1], B:[P3, P4], C:[P2]

단계 3: Reconciliation
  A에게 Heartbeat 응답: "P2를 해제하세요"
  A: P2 해제 -> Heartbeat으로 확인
       (P0, P1은 계속 처리)

단계 4: P2가 해제된 후
  C에게 Heartbeat 응답: "P2를 가져가세요"
  C: P2 소비 시작

=> A, B는 자신의 파티션을 계속 처리
   정지 시간 = P2 전환 시간만 (밀리초 단위)
```

**왜 서버 측 할당인가?** 서버가 할당을 직접 계산하면:
1. 리더 선출이 불필요하다 (단일 장애점 제거)
2. 전체 클러스터 메타데이터를 활용한 최적 할당이 가능하다
3. 클라이언트 간 할당기 버전 불일치 문제가 없다
4. 점진적(incremental) 할당이 자연스럽다

---

## 7. __consumer_offsets 토픽

### 7.1 토픽 구조

```
__consumer_offsets 토픽:
+----------------------------------------------------------+
| 기본 설정:                                                |
|   파티션 수: 50 (offsets.topic.num.partitions)            |
|   복제 인수: 3 (offsets.topic.replication.factor)         |
|   정리 정책: compact                                      |
|   세그먼트 크기: 100MB                                    |
+----------------------------------------------------------+

저장되는 레코드 종류:
+----------------------------------------------------------+
| 1. OffsetCommit                                           |
|    Key: {groupId, topic, partition}                       |
|    Value: {offset, leaderEpoch, metadata, commitTimestamp}|
|                                                           |
| 2. GroupMetadata                                          |
|    Key: {groupId}                                         |
|    Value: {protocol, generation, leader, members[]}       |
|                                                           |
| 3. ConsumerGroup 메타데이터 (KIP-848)                     |
|    ConsumerGroupMetadataKey/Value                         |
|    ConsumerGroupMemberMetadataKey/Value                   |
|    ConsumerGroupTargetAssignmentMemberKey/Value           |
|    ConsumerGroupCurrentMemberAssignmentKey/Value          |
|    ConsumerGroupPartitionMetadataKey/Value                |
+----------------------------------------------------------+
```

### 7.2 레코드 키 유형

`GroupCoordinatorShard.java`에서 가져온 레코드 타입들:

```
__consumer_offsets 레코드 키 유형:

[Legacy 레코드]
OffsetCommitKey          - 오프셋 커밋 (레거시)
GroupMetadataKey          - 그룹 메타데이터 (레거시)

[KIP-848 Consumer 그룹 레코드]
ConsumerGroupMetadataKey                  - 그룹 메타데이터
ConsumerGroupMemberMetadataKey            - 멤버 메타데이터
ConsumerGroupPartitionMetadataKey         - 파티션 메타데이터
ConsumerGroupTargetAssignmentMetadataKey  - 타겟 할당 메타데이터
ConsumerGroupTargetAssignmentMemberKey    - 멤버별 타겟 할당
ConsumerGroupCurrentMemberAssignmentKey   - 멤버별 현재 할당
ConsumerGroupRegularExpressionKey         - 정규식 구독

[KIP-932 Share 그룹 레코드]
ShareGroupMetadataKey                     - Share 그룹 메타데이터
ShareGroupMemberMetadataKey               - Share 멤버 메타데이터
ShareGroupCurrentMemberAssignmentKey      - Share 현재 할당

[새로운 오프셋 레코드]
OffsetCommitKey (new)    - 오프셋 커밋 (신규 포맷)
```

### 7.3 컴팩션과 오프셋 관리

```
__consumer_offsets 컴팩션:

시간 T1:
  [group-A, topic-1, P0] -> offset: 100
  [group-A, topic-1, P0] -> offset: 200
  [group-A, topic-1, P0] -> offset: 300

컴팩션 후:
  [group-A, topic-1, P0] -> offset: 300  (최신만 유지)

오프셋 만료:
  offsets.retention.minutes (기본 7일)
  -> 7일 이상 커밋 없는 오프셋은 tombstone 기록
  -> 컴팩션 시 제거

Tombstone 레코드:
  Key: {groupId, topic, partition}
  Value: null  <- 삭제 마커
```

---

## 8. 파티션 할당 전략

### 8.1 Classic 프로토콜 할당기

Classic 프로토콜에서는 **클라이언트(리더)**가 할당을 계산한다.

```
RangeAssignor (기본):
+---------------------------------------------+
| 각 토픽별로 파티션을 범위로 분배              |
|                                              |
| topic-1: [P0, P1, P2, P3, P4, P5]           |
| 컨슈머: [C0, C1, C2]                         |
|                                              |
| 6 / 3 = 2개씩, 나머지 0                     |
| C0: [P0, P1]                                |
| C1: [P2, P3]                                |
| C2: [P4, P5]                                |
+---------------------------------------------+

문제: 토픽이 여러 개일 때 불균형
| topic-1: C0:[P0,P1], C1:[P2], C2:[P3]       |
| topic-2: C0:[P0,P1], C1:[P2], C2:[P3]       |
| => C0이 항상 파티션을 더 많이 받음            |
```

```
RoundRobinAssignor:
+---------------------------------------------+
| 모든 토픽의 파티션을 라운드로빈 분배          |
|                                              |
| 전체 파티션: [t1-P0, t1-P1, t2-P0, t2-P1]   |
| C0: [t1-P0, t2-P0]                          |
| C1: [t1-P1, t2-P1]                          |
|                                              |
| 장점: 균등 분배                               |
| 단점: 구독 토픽이 다르면 비효율               |
+---------------------------------------------+
```

```
CooperativeStickyAssignor:
+---------------------------------------------+
| 기존 할당을 최대한 유지하면서 균형 조정       |
|                                              |
| 이전 할당: C0:[P0,P1,P2], C1:[P3,P4]        |
| C2 참여 후:                                  |
| C0:[P0,P1], C1:[P3,P4], C2:[P2]             |
|                                              |
| 장점: 최소한의 파티션 이동                    |
|       Cooperative 리밸런스 지원               |
+---------------------------------------------+
```

### 8.2 Consumer 프로토콜 할당기 (서버 측)

```
서버 측 할당기:
+-----------------------------------------------+
| group-coordinator/src/main/java/org/apache/    |
| kafka/coordinator/group/assignor/              |
|                                                |
| UniformAssignor:                               |
|   - 서버 측 기본 할당기                         |
|   - 파티션을 균등하게 분배                      |
|   - 기존 할당을 최대한 유지 (sticky)            |
|   - 구독 유형에 따라 최적화                     |
|     - Homogeneous: 모든 멤버 같은 구독          |
|     - Heterogeneous: 멤버별 다른 구독           |
|                                                |
| SimpleAssignor:                                |
|   - Share 그룹용 할당기                         |
|   - 모든 파티션을 모든 멤버에게 할당            |
+-----------------------------------------------+
```

### 8.3 TargetAssignmentBuilder

서버 측 할당의 핵심 클래스:

```
TargetAssignmentBuilder 흐름:

1. 현재 상태 수집:
   - 현재 멤버 목록과 구독
   - 현재 할당 상태
   - 사용 가능한 토픽/파티션

2. 할당기 호출:
   - ConsumerGroupPartitionAssignor.assign()
   - 입력: 멤버 구독, 토픽 파티션, 기존 할당
   - 출력: 멤버별 새 할당

3. 변경 레코드 생성:
   - 변경된 멤버의 TargetAssignment만 레코드로 생성
   - __consumer_offsets에 기록

4. 에포크 업데이트:
   - 그룹 에포크 증가
   - 변경된 멤버의 할당 에포크 증가
```

---

## 9. Share 그룹 (KIP-932)

### 9.1 Share 그룹 개념

```
기존 Consumer 그룹 vs Share 그룹:

[Consumer 그룹 - 파티션 독점]
Topic Partition 0 --> Consumer A (독점)
Topic Partition 1 --> Consumer B (독점)
Topic Partition 2 --> Consumer C (독점)

=> 파티션 수 = 최대 병렬도
=> 3개 파티션이면 최대 3개 컨슈머

[Share 그룹 - 파티션 공유]
Topic Partition 0 --> Consumer A, B, C (공유)
Topic Partition 1 --> Consumer A, B, C (공유)

=> 파티션 수와 무관한 병렬도
=> 2개 파티션이어도 N개 컨슈머 가능
```

### 9.2 Share 그룹의 동작

```
Share 그룹 레코드 처리:

Partition 0의 레코드들:
[R0] [R1] [R2] [R3] [R4] [R5] [R6] [R7] ...

Consumer A가 fetch:    [R0, R1, R2]  -> 처리 중
Consumer B가 fetch:    [R3, R4, R5]  -> 처리 중
Consumer C가 fetch:    [R6, R7]      -> 처리 중

각 컨슈머는 Acknowledge로 처리 결과 보고:
- ACCEPT: 처리 완료
- REJECT: 처리 실패, 다른 컨슈머에게 재분배
- RELEASE: 처리 포기, 다시 사용 가능
```

### 9.3 Share 그룹 상태

```
Share 그룹도 Consumer 그룹과 유사한 상태 머신을 따르지만,
파티션 할당이 아닌 파티션 공유 기반으로 동작한다.

상태: EMPTY -> STABLE (참여 즉시)
      STABLE -> DEAD

Share 그룹에서는 리밸런스 개념이 다르다:
- 모든 멤버가 모든 파티션에 접근 가능
- 멤버 참여/탈퇴 시 할당 변경 불필요
- 서버가 레코드 단위로 분배 관리
```

**왜 Share 그룹이 필요한가?** 전통적인 메시지 큐(RabbitMQ, ActiveMQ)처럼 레코드
단위의 부하 분산이 필요한 경우가 있다. Consumer 그룹은 파티션 단위 분배만 가능하여,
파티션 수보다 많은 컨슈머를 활용할 수 없다. Share 그룹은 이 제약을 해소한다.

---

## 10. OffsetMetadataManager

### 10.1 역할

`OffsetMetadataManager.java` (라인 72-79)는 모든 그룹의 오프셋을 관리한다.

```
OffsetMetadataManager 구조:
+--------------------------------------------------+
| OffsetMetadataManager                             |
|                                                   |
|  offsetsByGroup: TimelineHashMap<                  |
|    String(groupId),                               |
|    TimelineHashMap<                               |
|      String(topic),                               |
|      TimelineHashMap<                             |
|        Integer(partition),                        |
|        OffsetAndMetadata                          |
|      >                                            |
|    >                                              |
|  >                                                |
|                                                   |
|  openTransactionsByGroup: TimelineHashMap          |
|  ^-- 진행 중인 트랜잭셔널 오프셋 커밋             |
+--------------------------------------------------+
```

### 10.2 오프셋 커밋 처리

```
오프셋 커밋 흐름:

commitOffsets(request)
    |
    +-- 1. 그룹 존재/상태 확인
    |      Classic 그룹: STABLE 상태, 세대 일치
    |      Consumer 그룹: 멤버 에포크 확인
    |
    +-- 2. 각 파티션별 오프셋 검증
    |      - 메타데이터 크기 제한
    |      - 유효한 오프셋 값
    |
    +-- 3. CoordinatorRecord 생성
    |      OffsetCommitKey + OffsetCommitValue
    |
    +-- 4. __consumer_offsets에 기록
    |
    +-- 5. 인메모리 상태 업데이트 (replay)
```

### 10.3 트랜잭셔널 오프셋 커밋

```
트랜잭셔널 오프셋 커밋:

Producer가 트랜잭션 내에서 오프셋 커밋:
  producer.sendOffsetsToTransaction(offsets, "consumer-group")

흐름:
1. Producer -> TransactionCoordinator:
   AddOffsetsToTxn(txnId, groupId)

2. TransactionCoordinator:
   그룹의 __consumer_offsets 파티션을 트랜잭션에 추가

3. Producer -> GroupCoordinator:
   TxnOffsetCommit(txnId, producerId, epoch, offsets)

4. GroupCoordinator:
   pendingTransactionalOffsets에 저장 (아직 미확정)

5. 트랜잭션 커밋 시:
   TransactionCoordinator -> GroupCoordinator:
   WriteTxnMarkers(COMMIT)
   -> pending 오프셋을 확정 오프셋으로 이동
```

### 10.4 오프셋 만료

```
오프셋 만료 처리:

조건:
- offsets.retention.minutes (기본 7일) 경과
- 그룹이 EMPTY 상태이거나
- 해당 파티션을 더 이상 구독하지 않음

처리:
1. 만료 타이머가 주기적으로 실행
2. 만료된 오프셋 탐색
3. Tombstone 레코드 생성 (Value = null)
4. __consumer_offsets에 기록
5. 인메모리에서 제거
6. 컴팩션이 tombstone 이전 레코드 정리
```

---

## 11. CoordinatorRuntime 패턴

### 11.1 개요

`coordinator-common` 모듈의 CoordinatorRuntime은 그룹 코디네이터와 트랜잭션
코디네이터가 공유하는 런타임 프레임워크이다.

```
CoordinatorRuntime 아키텍처:

+----------------------------------------------+
| CoordinatorRuntime<S, U>                     |
|                                               |
|  S: CoordinatorShard (비즈니스 로직)           |
|  U: CoordinatorRecord (레코드 타입)            |
|                                               |
|  +------------------------------------------+|
|  | CoordinatorEventProcessor                ||
|  | (이벤트 처리 스레드 풀)                    ||
|  |                                          ||
|  | 이벤트 큐 -> 단일 스레드로 순서 보장      ||
|  +------------------------------------------+|
|                                               |
|  +------------------------------------------+|
|  | PartitionWriter                           ||
|  | (__consumer_offsets 파티션에 레코드 기록)  ||
|  +------------------------------------------+|
|                                               |
|  +------------------------------------------+|
|  | CoordinatorLoader                         ||
|  | (파티션 로드 시 기존 레코드 재생)          ||
|  +------------------------------------------+|
+----------------------------------------------+
```

### 11.2 이벤트 기반 처리

```
요청 처리 흐름:

외부 요청 (예: JoinGroup)
    |
    v
CoordinatorRuntime.scheduleWriteOperation()
    |
    v
CoordinatorEventProcessor 스레드
    |-- 이벤트 큐에서 꺼내기
    |-- Shard의 비즈니스 로직 실행
    |   (예: GroupCoordinatorShard.joinGroup())
    |-- CoordinatorResult<Response, Records> 반환
    |
    v
PartitionWriter
    |-- Records를 __consumer_offsets에 기록
    |-- 기록 성공 시 응답 전송
    |-- 기록 실패 시 에러 응답
```

### 11.3 파티션 로딩

```
파티션 리더 전환 시 로딩:

__consumer_offsets-12의 리더가 Broker-A로 전환
    |
    v
CoordinatorLoader.load()
    |-- __consumer_offsets-12의 모든 레코드 읽기
    |-- 각 레코드를 Shard.replay()로 재생
    |   |-- GroupMetadata -> groups 맵에 추가
    |   |-- OffsetCommit -> offsets 맵에 추가
    |   |-- ConsumerGroupMember -> member 추가
    |-- 로딩 완료 -> Shard 활성화
    |
    v
Shard-12 준비 완료, 요청 처리 가능
```

**왜 이벤트 기반 아키텍처인가?** 단일 __consumer_offsets 파티션에 대한 모든 작업을
하나의 이벤트 스레드에서 순차 처리하면, 복잡한 동기화 없이도 일관성을 보장할 수 있다.
이는 Actor 모델과 유사한 접근 방식이다.

---

## 12. 설계 결정: Why?

### 12.1 왜 __consumer_offsets가 일반 토픽인가?

```
설계 선택: 오프셋을 Kafka 토픽에 저장

대안:
1. ZooKeeper에 저장 (Kafka 0.8 이전)
   - 문제: ZK 병목, 쓰기 성능 한계
   - 수천 오프셋 커밋/초 불가

2. 외부 DB (MySQL, Redis 등)
   - 문제: 외부 의존성, 운영 복잡성
   - Kafka 클러스터와 별도 관리 필요

3. Kafka 토픽 (현재 방식)
   - 장점: Kafka 자체 복제/내구성 활용
   - 장점: 높은 쓰기 처리량
   - 장점: 컴팩션으로 자동 정리
   - 장점: 추가 인프라 불필요
```

### 12.2 왜 그룹별 코디네이터를 지정하는가?

```
코디네이터 분산:

[중앙집중식]
모든 그룹 -> 하나의 코디네이터
=> 병목, 단일 장애점

[분산식 (현재)]
그룹 -> hash(groupId) % 50 -> __consumer_offsets 파티션
     -> 해당 파티션의 리더 브로커 = 코디네이터

장점:
- 부하 분산: 50개 파티션으로 그룹 분산
- 고가용성: 파티션 리더 장애 시 자동 페일오버
- 확장성: 브로커 추가 시 자연스럽게 부하 재분배
```

### 12.3 왜 Classic에서 Consumer 프로토콜로 진화하는가?

```
진화의 핵심 이유:

1. 리밸런스 비용 감소
   Classic: O(전체 파티션) - 모두 재할당
   Consumer: O(변경 파티션) - 변경분만 재할당

2. 운영 단순화
   Classic: 클라이언트 할당기 버전 관리 필요
   Consumer: 서버 측 할당, 일관된 동작

3. 안정성 향상
   Classic: poll() 지연 -> 하트비트 실패 -> 리밸런스
   Consumer: 독립 하트비트 -> poll() 지연과 무관

4. 관찰가능성
   Classic: 할당 상태가 클라이언트에만 존재
   Consumer: 할당 상태가 서버에 영구 저장
            -> 관리자 도구로 조회 가능

5. 확장성
   Classic: 리더 컨슈머의 메모리에 모든 멤버 정보 필요
   Consumer: 서버에서 관리, 클라이언트 부담 없음
```

### 12.4 왜 Shard 패턴을 사용하는가?

```
Shard 패턴의 이점:

1. 격리: 한 파티션의 문제가 다른 파티션에 영향 없음
2. 병렬성: 서로 다른 파티션의 요청은 병렬 처리
3. 재사용: GroupCoordinator와 TransactionCoordinator가
   같은 CoordinatorRuntime 공유
4. 테스트: Shard 단위 단위 테스트 용이
5. 장애 복구: 파티션 단위 로딩/언로딩
```

---

## 요약

```
그룹 코디네이터 아키텍처 요약:

3계층 구조:
  GroupCoordinatorService (요청 라우팅)
    -> GroupCoordinatorShard (파티션별 격리)
      -> GroupMetadataManager (그룹 상태 관리)
      -> OffsetMetadataManager (오프셋 관리)

그룹 프로토콜 진화:
  Classic 그룹 (JoinGroup/SyncGroup)
    -> Consumer 그룹 (KIP-848, 서버 측 할당)
    -> Share 그룹 (KIP-932, 레코드 단위 분배)

핵심 설계 원칙:
1. __consumer_offsets 토픽으로 상태 영구 저장
2. 파티션 해싱으로 코디네이터 분산
3. 이벤트 기반 단일 스레드로 일관성 보장
4. Shard 패턴으로 격리와 병렬성 확보
5. 점진적 리밸런스로 서비스 중단 최소화
```
