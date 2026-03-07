# 10. KRaft 합의 프로토콜 Deep-Dive

## 목차

1. [개요](#1-개요)
2. [Raft 프로토콜 기초](#2-raft-프로토콜-기초)
3. [상태 머신](#3-상태-머신)
4. [Pre-Vote 메커니즘](#4-pre-vote-메커니즘)
5. [투표 (Vote)](#5-투표-vote)
6. [리더 상태 (LeaderState)](#6-리더-상태-leaderstate)
7. [팔로워 상태 (FollowerState)](#7-팔로워-상태-followerstate)
8. [__cluster_metadata 토픽](#8-__cluster_metadata-토픽)
9. [메타데이터 레코드](#9-메타데이터-레코드)
10. [스냅샷과 델타](#10-스냅샷과-델타)
11. [QuorumController](#11-quorumcontroller)
12. [MetadataLoader와 MetadataPublisher](#12-metadataloader와-metadatapublisher)
13. [VoterSet: 동적 Voter 관리](#13-voterset-동적-voter-관리)
14. [설계 결정의 이유 (Why)](#14-설계-결정의-이유-why)

---

## 1. 개요

KRaft(Kafka Raft)는 Kafka의 메타데이터 합의 프로토콜로, ZooKeeper를 대체한다.
Raft 프로토콜을 기반으로 하되 Kafka의 고유한 요구사항에 맞게 확장했다.

### ZooKeeper에서 KRaft로의 전환 이유

```
ZooKeeper 기반:                    KRaft 기반:
+--------+     +----------+       +--------+
| Broker | --> | ZooKeeper|       | Broker | (컨트롤러 역할 겸함)
+--------+     | Ensemble |       +--------+
+--------+     |   (3~5)  |       +--------+
| Broker | --> |          |       | Broker | (컨트롤러 역할 겸함)
+--------+     +----------+       +--------+
+--------+                        +--------+
| Broker | ←-controller→          | Broker | (일반 브로커)
+--------+                        +--------+

문제점:                            개선:
- 별도 클러스터 운영 부담            - 단일 시스템으로 통합
- 메타데이터 불일치 가능             - 이벤트 로그 기반 일관성
- 컨트롤러 장애 시 완전한 재로드     - 증분 업데이트(델타)
- 대규모 클러스터에서 확장 한계       - 스냅샷 + 델타로 확장성
```

### 핵심 소스 파일

| 파일 | 경로 | 역할 |
|------|------|------|
| KafkaRaftClient.java | `raft/src/main/java/org/apache/kafka/raft/KafkaRaftClient.java` | Raft 클라이언트 핵심 |
| QuorumState.java | `raft/.../raft/QuorumState.java` | 상태 머신 관리 |
| LeaderState.java | `raft/.../raft/LeaderState.java` | 리더 상태 |
| FollowerState.java | `raft/.../raft/FollowerState.java` | 팔로워 상태 |
| CandidateState.java | `raft/.../raft/CandidateState.java` | 후보 상태 |
| ProspectiveState.java | `raft/.../raft/ProspectiveState.java` | Pre-Vote 상태 |
| UnattachedState.java | `raft/.../raft/UnattachedState.java` | 미연결 상태 |
| ResignedState.java | `raft/.../raft/ResignedState.java` | 사임 상태 |
| VoterSet.java | `raft/.../raft/VoterSet.java` | Voter 집합 |
| QuorumController.java | `metadata/.../controller/QuorumController.java` | 컨트롤러 상태 머신 |
| MetadataLoader.java | `metadata/.../image/loader/MetadataLoader.java` | 메타데이터 로더 |

> 소스 경로 기준: `raft/src/main/java/org/apache/kafka/raft/`

---

## 2. Raft 프로토콜 기초

### 2.1 Raft의 핵심 원칙

Raft 합의 프로토콜은 세 가지 핵심 요소로 구성된다:

```
Raft의 3가지 핵심:

  1. 리더 선출 (Leader Election)
     - 하나의 리더만 존재
     - 에포크(term) 기반 리더 변경
     - 과반수 투표로 선출

  2. 로그 복제 (Log Replication)
     - 리더가 모든 팔로워에 로그 전파
     - 과반수 복제 완료 시 커밋

  3. 안전성 (Safety)
     - 커밋된 엔트리는 절대 손실 안 됨
     - 리더는 항상 가장 최신 커밋 데이터를 보유
```

### 2.2 KRaft vs 표준 Raft

KRaft는 표준 Raft에 여러 확장을 추가했다:

```
+----------------------------+----------------------------+
| 표준 Raft                   | KRaft                      |
+----------------------------+----------------------------+
| AppendEntries RPC           | Fetch API (Kafka 기존 사용) |
| RequestVote RPC             | Vote API + PreVote         |
| 3가지 상태                   | 6가지 상태                  |
| (Leader, Follower, Candidate)| (+Resigned, Unattached,    |
|                             |   Prospective)             |
| 정적 멤버십                  | 동적 VoterSet              |
| 별도 스냅샷 메커니즘          | FetchSnapshot API          |
| InstallSnapshot RPC         | FetchSnapshot (Pull 기반)  |
+----------------------------+----------------------------+
```

---

## 3. 상태 머신

### 3.1 QuorumState: 6가지 상태

`QuorumState`는 노드의 현재 상태를 관리하며, 유효한 상태 전이만 허용한다.

```java
// QuorumState.java (라인 84~)
public class QuorumState {
    private final OptionalInt localId;
    private final Uuid localDirectoryId;
    private final Time time;
    private final QuorumStateStore store;
    private final int electionTimeoutMs;
    private final int fetchTimeoutMs;
    private volatile EpochState state;
}
```

### 3.2 상태 전이 다이어그램

```
QuorumState.java 주석에서 발췌한 상태 전이:

                      +-----------+
                      | Resigned  |
                      +-----------+
                       |         |
           새 에포크 학습 |         | 리더 발견
                       v         v
                 +-----------+  +-----------+
                 | Unattached|  | Follower  |
                 +-----------+  +-----------+
                   |   ^    |    ^   |    ^
   선거 타임아웃    |   |    |    |   |    |
                   v   |    |    |   |    |
              +-------------+    |   |    |
              | Prospective |----+   |    |
              +-------------+        |    |
                |       |            |    |
    과반수      |  선거  |   리더     |    |
    PreVote    |  패배/  |   발견     |    |
    성공       |  타임아웃|           |    |
                v       |           |    |
              +-----------+         |    |
              | Candidate |         |    |
              +-----------+         |    |
                |       |           |    |
    과반수      |  선거  |   새 에포크 |    | Fetch 타임아웃
    Vote 성공  |  패배  |   학습     |    |
                v       v           |    |
              +-----------+   +-----+    |
              |  Leader   |---+          |
              +-----------+              |
                   |                     |
               사임(종료)                 |
                   v                     |
              +-----------+              |
              | Resigned  |--------------+
              +-----------+
```

### 3.3 각 상태의 역할

```
+------------------+---------------------------------------------+
| 상태              | 역할                                         |
+------------------+---------------------------------------------+
| Resigned         | 리더가 종료(shutdown) 중, 에포크 종료 알림     |
| Unattached       | 어떤 리더도 모름, 투표 가능                    |
| Prospective      | Pre-Vote 진행 중 (불필요한 선출 방지)          |
| Candidate        | 실제 Vote 요청 중, 에포크 증가                 |
| Leader           | 로그 복제, HW 관리, 클라이언트 요청 처리        |
| Follower         | 리더에서 데이터 Fetch, 리더 감시               |
+------------------+---------------------------------------------+
```

### 3.4 EpochState 인터페이스

모든 상태 클래스는 `EpochState` 인터페이스를 구현한다:

```java
// EpochState.java
public interface EpochState {
    int epoch();
    Endpoints leaderEndpoints();
    Optional<LogOffsetMetadata> highWatermark();
    boolean canGrantVote(ReplicaKey replicaKey, boolean isLogUpToDate, boolean isPreVote);
    // ...
}
```

---

## 4. Pre-Vote 메커니즘

### 4.1 왜 Pre-Vote가 필요한가?

네트워크 파티션 후 복구된 노드가 불필요하게 선거를 일으키는 것을 방지한다.

```
Pre-Vote 없이의 문제:

  초기: Leader=B1(epoch=5), Follower=B2,B3

  t0: B3 네트워크 파티션 발생 (B3 고립)
  t1: B3 선거 타임아웃 → Candidate(epoch=6)
      → B1, B2에 Vote 요청 (도달 불가)
  t2: B3 타임아웃 → Candidate(epoch=7)
  t3: B3 타임아웃 → Candidate(epoch=8)
  ...
  tN: B3 네트워크 복구, 현재 epoch=20
      → B1, B2가 epoch=20을 학습
      → B1의 리더십 무효화! (epoch=5 < 20)
      → 불필요한 리더 재선출 ★

  Pre-Vote 있을 때:
  t0: B3 네트워크 파티션 발생
  t1: B3 선거 타임아웃 → Prospective (Pre-Vote 전송)
      → B1, B2에 PreVote 요청 (도달 불가)
      → 과반수 미달 → epoch 증가 안 함! ★
  ...
  tN: B3 네트워크 복구, epoch 여전히 5
      → B1(리더, epoch=5)에 정상적으로 Fetch 시작
      → 리더 변경 없음!
```

### 4.2 ProspectiveState

```java
// ProspectiveState.java (라인 31~)
public class ProspectiveState implements NomineeState {
    private final int localId;
    private final int epoch;
    private final OptionalInt leaderId;
    private final EpochElection epochElection;
    private final Optional<LogOffsetMetadata> highWatermark;
    private final long electionTimeoutMs;
    private final Timer electionTimer;
}
```

```
ProspectiveState 주석 발췌:

  1. Once started, it will send prevote requests and keep record
     of the received vote responses
  2. If it receives a message denoting a leader with a higher epoch,
     it will transition to follower state.
  3. If majority votes granted, it will transition to candidate state.
  4. If majority votes rejected or election times out, it will
     transition to unattached or follower state
```

### 4.3 Pre-Vote 흐름

```
Pre-Vote 전체 흐름:

  B3 (Prospective):
    |
    | PreVote(epoch=5, lastEpoch=5, endOffset=1000)
    |  (★ epoch를 증가시키지 않음)
    |
    +-------> B1 (Leader, epoch=5):
    |           → 자신이 리더이고 B3의 로그가 충분한가?
    |           → B3.endOffset >= B1.HW? → 거부 (리더가 건재)
    |           → 또는: B3의 로그가 최신인가? → 조건부 승인
    |
    +-------> B2 (Follower, epoch=5):
              → B2가 최근 리더에게서 Fetch 성공?
              → 예: 거부 (리더가 건재함)
              → 아니오: B3의 로그가 최신이면 승인

  과반수 승인:
    → Candidate(epoch=6)로 전환
    → 실제 Vote 전송

  과반수 거부/타임아웃:
    → Unattached 또는 Follower로 전환
```

---

## 5. 투표 (Vote)

### 5.1 VoteRequest/VoteResponse

```
VoteRequest:
  +-------------------+
  | CandidateId       |  후보자 ID
  | CandidateEpoch    |  후보자가 제안하는 새 에포크
  | LastEpoch         |  후보자 로그의 마지막 에포크
  | LastEndOffset     |  후보자 로그의 마지막 오프셋
  +-------------------+

VoteResponse:
  +-------------------+
  | VoteGranted       |  투표 승인 여부
  | LeaderEpoch       |  응답자가 아는 최신 에포크
  +-------------------+
```

### 5.2 canGrantVote() 로직

```java
// QuorumState.java (라인 751~)
public boolean canGrantVote(ReplicaKey replicaKey, boolean isLogUpToDate, boolean isPreVote) {
    return state.canGrantVote(replicaKey, isLogUpToDate, isPreVote);
}
```

```
canGrantVote() 판단 흐름:

  투표 요청 수신
      |
      +--- 1. 이미 다른 후보에게 투표했는가?
      |        → 예: 거부 (에포크당 1표만)
      |        → 아니오: 계속
      |
      +--- 2. 요청자의 로그가 최신인가? (isLogUpToDate)
      |        lastEpoch 비교:
      |          요청자.lastEpoch > 내.lastEpoch → 최신
      |          요청자.lastEpoch == 내.lastEpoch
      |            AND 요청자.endOffset >= 내.endOffset → 최신
      |          그 외: 최신 아님 → 거부
      |
      +--- 3. PreVote인 경우 (isPreVote):
      |        → 최근 리더에게서 Fetch 성공했으면 거부
      |           (리더가 건재하므로 불필요한 선거 방지)
      |        → Fetch 실패 상태이면 승인
      |
      +--- 4. 실제 Vote인 경우:
               → 로그가 최신이면 승인
               → 투표 기록 저장 (quorum-state 파일)
```

### 5.3 로그 비교 (isLogUpToDate)

```
로그 비교 예시:

  후보 B3: lastEpoch=5, endOffset=1000
  투표자 B2: lastEpoch=5, endOffset=950

  비교:
    epoch 같음 (5 == 5) → endOffset 비교
    1000 >= 950 → B3가 더 최신 → 투표 승인

  후보 B3: lastEpoch=4, endOffset=2000
  투표자 B2: lastEpoch=5, endOffset=500

  비교:
    B3.lastEpoch(4) < B2.lastEpoch(5) → B3가 덜 최신 → 투표 거부
    (오프셋은 많지만 에포크가 낮으면 거부!)
```

**왜 에포크를 오프셋보다 우선 비교하는가?**

더 높은 에포크는 더 최근의 리더 아래에서 기록된 데이터를 의미한다. 오프셋이 많아도
이전 에포크의 데이터는 새 리더에 의해 덮어쓰일 수 있다. 따라서 에포크가 가장
중요한 비교 기준이다. 이것이 Raft의 **선거 안전성(Election Safety)**을 보장한다.

### 5.4 CandidateState

```java
// CandidateState.java (라인 29~)
public class CandidateState implements NomineeState {
    private final int localId;
    private final Uuid localDirectoryId;
    private final int epoch;
    private final EpochElection epochElection;
    private final Optional<LogOffsetMetadata> highWatermark;
    private final int electionTimeoutMs;
    private final Timer electionTimer;
}
```

```
CandidateState 주석 발췌:

  1. Once started, it will send vote requests and keep record
     of the received vote responses.
  2. If majority votes granted, it will transition to leader state.
  3. If majority votes rejected, it will transition to prospective
     after a backoff phase.
  4. If election times out, it will transition immediately to prospective.
```

### 5.5 선거 과정 전체

```
선거 전체 흐름 (3노드 클러스터):

  B1: Follower(epoch=5)  →  Fetch 타임아웃
  B2: Follower(epoch=5)
  B3: Leader(epoch=5)    →  장애!

  1. B1: Follower → Prospective (epoch=5)
     B1 → B2: PreVote(epoch=5, lastEpoch=5, endOffset=900)
     B2: "리더 B3에서 Fetch 실패 상태, B1 로그 OK" → PreVote 승인

  2. B1: Prospective → Candidate (epoch=6)
     B1 → B2: Vote(epoch=6, lastEpoch=5, endOffset=900)
     B2: "아직 epoch=6에서 투표 안 함, B1 로그 최신" → Vote 승인

  3. B1: Candidate → Leader (epoch=6)
     → 과반수(자기 자신 + B2 = 2/3) 획득
     → BeginQuorumEpoch 전송

  4. B2: Follower(epoch=6, leader=B1)
     → B1에서 Fetch 시작

  5. B3 복구:
     → B1에서 epoch=6 학습
     → Follower(epoch=6, leader=B1)
```

---

## 6. 리더 상태 (LeaderState)

### 6.1 구조

```java
// LeaderState.java (라인 63~)
public class LeaderState<T> implements EpochState {
    static final long OBSERVER_SESSION_TIMEOUT_MS = 300_000L;
    static final double CHECK_QUORUM_TIMEOUT_FACTOR = 1.5;

    private final VoterSet.VoterNode localVoterNode;
    private final int epoch;
    private final long epochStartOffset;
    private final Set<Integer> grantingVoters;
    private final VoterSet voterSetAtEpochStart;

    private Optional<LogOffsetMetadata> highWatermark = Optional.empty();
    private Map<Integer, ReplicaState> voterStates = new HashMap<>();
    private final Map<ReplicaKey, ReplicaState> observerStates = new HashMap<>();
    private final BatchAccumulator<T> accumulator;
    private final Set<Integer> fetchedVoters = new HashSet<>();
    private final Timer checkQuorumTimer;
    private final Timer beginQuorumEpochTimer;
}
```

### 6.2 High Watermark 계산 (중앙값 방식)

KRaft의 HW 계산은 Kafka 복제의 "최소값" 방식과 다르다. **투표자 오프셋의 중앙값**을 사용한다.

```java
// LeaderState.java (라인 727~)
private boolean maybeUpdateHighWatermark() {
    ArrayList<ReplicaState> followersByDescendingFetchOffset =
        followersByDescendingFetchOffset().collect(Collectors.toCollection(ArrayList::new));

    int indexOfHw = voterStates.size() / 2;  // ★ 중앙값 인덱스
    Optional<LogOffsetMetadata> highWatermarkUpdateOpt =
        followersByDescendingFetchOffset.get(indexOfHw).endOffset;
    // ...
}
```

```
HW 중앙값 계산 예시 (5노드 클러스터):

  voterStates.size() = 5
  indexOfHw = 5 / 2 = 2  (0-indexed)

  투표자 LEO를 내림차순 정렬:
    B1(leader): 1000
    B2:          980
    B3:          960  ← indexOfHw=2 ★
    B4:          940
    B5:          900

  HW = 960 (인덱스 2의 값)

  의미: 과반수(3/5)가 960 이상 → 960까지 커밋됨

  3노드 클러스터의 경우:
    indexOfHw = 3 / 2 = 1

    B1(leader): 500
    B2:          490  ← indexOfHw=1 ★
    B3:          480

    HW = 490 (과반수 2/3이 490 이상)
```

### 6.3 에포크 시작 오프셋 조건

```java
// LeaderState.java (라인 747)
if (highWatermarkUpdateOffset > epochStartOffset) {
    // HW를 업데이트할 수 있음
}
```

**왜 epochStartOffset보다 큰 경우에만 HW를 올리는가?**

새 리더가 선출된 직후, 자신의 에포크에서 최소 1개의 레코드를 커밋해야 한다.
이것이 Raft의 **커밋 안전성(Commitment Safety)**을 보장한다. 이전 에포크의
레코드를 새 에포크의 HW로 올리면, 해당 레코드가 실제로 모든 과반수에 복제되었는지
보장할 수 없다.

```
에포크 시작 오프셋 조건 예시:

  새 리더 B1 선출 (epoch=6)
  epochStartOffset = 1000 (epoch=6의 시작)

  B1이 아직 epoch=6에서 아무것도 기록하지 않음:
    B1.LEO = 1000, B2.LEO = 999

  HW 후보 = 999
  999 <= epochStartOffset(1000) → HW 업데이트 불가!

  B1이 LeaderChangeMessage를 기록:
    B1.LEO = 1001

  B2가 Fetch → B2.LEO = 1001
  HW 후보 = 1001
  1001 > epochStartOffset(1000) → HW = 1001 ★
```

### 6.4 CheckQuorum Timer

리더는 주기적으로 과반수의 팔로워가 Fetch하고 있는지 확인한다.

```
CheckQuorum 메커니즘:

  checkQuorumTimeoutMs = fetchTimeoutMs * 1.5

  리더 B1 (3노드 클러스터):
    checkQuorum 타이머 시작 (예: 7500ms)

    각 팔로워가 Fetch할 때마다 fetchedVoters에 추가:
      B2 Fetch → fetchedVoters = {B2}
      B3 Fetch → fetchedVoters = {B3}

    타이머 만료 시:
      fetchedVoters 크기 + 1(자기 자신) >= 과반수?
        예: fetchedVoters 초기화, 타이머 리셋
        아니오: 리더 사임 (Resigned) → 고립된 리더 방지!
```

### 6.5 BeginQuorumEpoch

리더가 선출되면 모든 투표자에게 `BeginQuorumEpoch` 요청을 보낸다.

```
BeginQuorumEpoch 흐름:

  B1: Leader(epoch=6) 선출
      |
      +--- B2에 BeginQuorumEpoch(epoch=6, leaderId=B1) 전송
      |      B2: Follower(epoch=6, leader=B1) 전환
      |      B2 → B1: 응답 (acknowledged)
      |
      +--- B3에 BeginQuorumEpoch(epoch=6, leaderId=B1) 전송
             B3: Follower(epoch=6, leader=B1) 전환
             B3 → B1: 응답 (acknowledged)

  아직 응답하지 않은 투표자:
    → beginQuorumEpochTimer 만료 시 재전송
    → 응답할 때까지 반복
```

---

## 7. 팔로워 상태 (FollowerState)

### 7.1 구조

```java
// FollowerState.java (라인 32~)
public class FollowerState implements EpochState {
    private final int fetchTimeoutMs;
    private final int epoch;
    private final int leaderId;
    private final Endpoints leaderEndpoints;
    private final Optional<ReplicaKey> votedKey;
    private final Set<Integer> voters;
    private final Timer fetchTimer;

    private boolean hasFetchedFromLeader = false;
    private Optional<LogOffsetMetadata> highWatermark;
    private Optional<RawSnapshotWriter> fetchingSnapshot = Optional.empty();
}
```

### 7.2 Fetch 타임아웃으로 리더 장애 감지

```
팔로워의 리더 장애 감지:

  fetchTimeoutMs = 5000 (기본값, 실제는 quorum.fetch.timeout.ms)

  정상 상태:
    B2(Follower) → B1(Leader): Fetch 요청
    B1 → B2: Fetch 응답 + 데이터
    → fetchTimer 리셋 (5000ms부터 다시 시작)

  리더 장애:
    B2 → B1: Fetch 요청 (응답 없음)
    fetchTimer 카운트다운: 5000 → 4000 → ... → 0
    → 타임아웃!
    → B2: Follower → Prospective (Pre-Vote 시작)
```

### 7.3 hasFetchedFromLeader 플래그

```java
// FollowerState.java (라인 50~)
/* Used to track if the replica has fetched successfully from the
 * leader at least once since the transition to follower in this epoch.
 * If the replica has not yet fetched successfully, it may be able to
 * grant PreVotes.
 */
private boolean hasFetchedFromLeader = false;
```

```
hasFetchedFromLeader의 역할:

  시나리오: B2가 B1의 리더십을 방금 인정함

  t0: B2: Follower(epoch=6, leader=B1)
      hasFetchedFromLeader = false

  t1: B3 → B2: PreVote 요청
      B2: "아직 리더에서 Fetch 안 했으므로 리더 건재 확신 없음"
      → PreVote 승인 가능

  t2: B2 → B1: Fetch 성공
      hasFetchedFromLeader = true

  t3: B3 → B2: PreVote 요청
      B2: "리더에서 Fetch 성공함, 리더 건재"
      → PreVote 거부
```

---

## 8. __cluster_metadata 토픽

### 8.1 개요

KRaft는 클러스터 메타데이터를 특수 내부 토픽 `__cluster_metadata`에 저장한다.
이 토픽은 단일 파티션(파티션 0)을 가지며, KRaft 컨트롤러 쿼럼이 관리한다.

```
__cluster_metadata 토픽:

  +----------------------------------------------------+
  |  __cluster_metadata (partition 0)                   |
  |                                                     |
  |  [Record 0: RegisterBrokerRecord(B1)]               |
  |  [Record 1: RegisterBrokerRecord(B2)]               |
  |  [Record 2: RegisterBrokerRecord(B3)]               |
  |  [Record 3: TopicRecord(my-topic)]                  |
  |  [Record 4: PartitionRecord(my-topic-0, leader=B1)] |
  |  [Record 5: PartitionRecord(my-topic-1, leader=B2)] |
  |  [Record 6: PartitionChangeRecord(my-topic-0, ...)] |
  |  ...                                                |
  |  [Record N: 최신 메타데이터 변경]                      |
  +----------------------------------------------------+
```

### 8.2 이벤트 로그로서의 메타데이터

```
ZooKeeper 방식 (상태 저장):        KRaft 방식 (이벤트 로그):
  /brokers/ids/1 = {...}           Record: RegisterBroker(B1)
  /brokers/ids/2 = {...}           Record: RegisterBroker(B2)
  /topics/my-topic/0 = {...}       Record: TopicRecord(my-topic)
                                   Record: PartitionRecord(...)
  상태 읽기: 직접 조회               Record: PartitionChangeRecord(...)
  변경 감지: Watcher                 ...

  문제점:                           장점:
  - 부분 업데이트 불일치             - 이벤트 순서 보장
  - Watcher 누락 가능               - 재생으로 상태 재구축 가능
  - 스냅샷 전체 전송 필요            - 증분 델타 전파 가능
```

---

## 9. 메타데이터 레코드

### 9.1 주요 레코드 유형

```
메타데이터 레코드 유형:

  브로커 관리:
  +------------------------------+-----------------------------------+
  | RegisterBrokerRecord         | 브로커 등록 (ID, 호스트, 포트, 랙)   |
  | UnregisterBrokerRecord       | 브로커 해제                        |
  | BrokerRegistrationChangeRecord | 브로커 등록 정보 변경              |
  +------------------------------+-----------------------------------+

  토픽/파티션 관리:
  +------------------------------+-----------------------------------+
  | TopicRecord                  | 토픽 생성 (이름, UUID)              |
  | PartitionRecord              | 파티션 생성 (리더, ISR, 복제본)     |
  | PartitionChangeRecord        | 파티션 변경 (리더 변경, ISR 변경)   |
  | RemoveTopicRecord            | 토픽 삭제                          |
  +------------------------------+-----------------------------------+

  설정 관리:
  +------------------------------+-----------------------------------+
  | ConfigRecord                 | 설정 변경 (토픽/브로커 설정)        |
  +------------------------------+-----------------------------------+

  ACL/보안:
  +------------------------------+-----------------------------------+
  | AccessControlEntryRecord     | ACL 엔트리 추가                    |
  | RemoveAccessControlEntryRecord | ACL 엔트리 삭제                  |
  +------------------------------+-----------------------------------+

  기타:
  +------------------------------+-----------------------------------+
  | FeatureLevelRecord           | 기능 레벨 변경                     |
  | NoOpRecord                   | 리더 변경 시 빈 레코드              |
  +------------------------------+-----------------------------------+
```

### 9.2 레코드 적용 순서

```
토픽 생성 예시:

  AdminClient: CreateTopics(my-topic, partitions=3, rf=3)
      |
      v
  QuorumController:
    1. TopicRecord(name="my-topic", topicId=UUID)
    2. PartitionRecord(topicId=UUID, partition=0, leader=B1, isr=[B1,B2,B3])
    3. PartitionRecord(topicId=UUID, partition=1, leader=B2, isr=[B1,B2,B3])
    4. PartitionRecord(topicId=UUID, partition=2, leader=B3, isr=[B1,B2,B3])

  이 레코드들은 원자적으로 __cluster_metadata에 기록됨
  → 커밋 후 모든 브로커에 전파
```

---

## 10. 스냅샷과 델타

### 10.1 스냅샷의 필요성

```
로그가 무한히 커지는 문제:

  시간이 지남에 따라 __cluster_metadata 로그:
  [rec0] [rec1] [rec2] ... [rec100000] [rec100001] ...

  새 브로커가 조인하면 처음부터 모든 레코드를 재생해야 함
  → 시작 시간이 매우 길어짐

  스냅샷:
  [Snapshot@offset=100000]  [rec100001] [rec100002] ...
       |                         |
  전체 상태의 덤프            스냅샷 이후의 변경(델타)만

  새 브로커: 스냅샷 로드 + 델타 적용 → 빠른 시작
```

### 10.2 FetchSnapshot RPC

팔로워가 스냅샷을 가져오는 방식은 **Pull 기반**이다 (표준 Raft의 InstallSnapshot과 다름).

```
FetchSnapshot 흐름:

  팔로워 B2                              리더 B1
    |                                      |
    | FetchSnapshot(snapshotId, position)  |
    |------------------------------------->|
    |                                      | 스냅샷 청크 읽기
    |  FetchSnapshotResponse(chunk, done)  |
    |<-------------------------------------|
    |                                      |
    | (done=false이면 다음 청크 요청)       |
    | FetchSnapshot(snapshotId, position+) |
    |------------------------------------->|
    |                                      |
    |  FetchSnapshotResponse(chunk, done)  |
    |<-------------------------------------|
    |                                      |
    | ... (반복)                            |
    |                                      |
    | (done=true) 스냅샷 로드 완료          |
    | 이후 일반 Fetch로 델타 적용           |
```

**왜 Push가 아닌 Pull 기반 스냅샷인가?**

Kafka의 전체 설계 철학(Pull 기반 복제)과 일관성을 유지한다. 팔로워가 자신의
속도로 스냅샷을 가져갈 수 있고, 리더에 추가 상태 관리가 불필요하다. 또한 기존의
Fetch API와 유사한 패턴을 사용하여 구현이 자연스럽다.

### 10.3 델타 (Incremental Delta)

```
델타 적용 흐름:

  브로커의 메타데이터 상태:
    [스냅샷@offset=1000: 전체 상태]

  이후 델타 레코드 적용:
    offset 1001: PartitionChangeRecord(my-topic-0, leader=B2)
      → 내부 상태에서 my-topic-0의 리더를 B2로 변경

    offset 1002: RegisterBrokerRecord(B4)
      → 새 브로커 B4 등록

    offset 1003: ConfigRecord(my-topic, retention.ms=86400000)
      → my-topic의 보존 기간을 1일로 변경

  현재 상태 = 스냅샷@1000 + 델타[1001..1003]
```

---

## 11. QuorumController

### 11.1 개요

```
소스: metadata/src/main/java/org/apache/kafka/controller/QuorumController.java

QuorumController는 활성 컨트롤러(KRaft 리더)에서 실행되며,
모든 관리 요청을 처리하는 상태 머신이다.
```

```
QuorumController 아키텍처:

  +-----------------------------------------------+
  | QuorumController                               |
  |                                                |
  | KafkaRaftClient (Raft 합의)                     |
  |     |                                          |
  |     v                                          |
  | __cluster_metadata 로그에 레코드 기록            |
  |     |                                          |
  |     v                                          |
  | 내부 상태 머신 업데이트:                          |
  |   - TopicControlManager: 토픽/파티션 관리        |
  |   - ReplicationControlManager: 복제/ISR 관리    |
  |   - ConfigurationControlManager: 설정 관리       |
  |   - ClusterControlManager: 브로커 등록/해제      |
  |   - AclControlManager: ACL 관리                 |
  |   - FeatureControlManager: 기능 레벨 관리        |
  +-----------------------------------------------+
```

### 11.2 요청 처리 흐름

```
관리 요청 처리 흐름:

  AdminClient: CreateTopics("new-topic")
      |
      v
  브로커 (아무 브로커)
      |
      | (컨트롤러로 전달)
      v
  QuorumController (활성 컨트롤러에서만)
      |
      +--- 1. 요청 검증 (토픽 이름, 파티션 수, RF 등)
      |
      +--- 2. 메타데이터 레코드 생성
      |        TopicRecord + PartitionRecord * N
      |
      +--- 3. KafkaRaftClient를 통해 로그에 기록
      |        → __cluster_metadata에 append
      |
      +--- 4. Raft 커밋 대기 (과반수 복제)
      |
      +--- 5. 커밋 완료 → 내부 상태 머신에 적용
      |        → 토픽 생성 완료
      |
      +--- 6. 응답 반환
```

### 11.3 상태 일관성 보장

```
QuorumController의 일관성 모델:

  모든 상태 변경은 __cluster_metadata 로그를 통해서만 이루어짐:
    → 직접 상태 변경 금지
    → 레코드 기록 → 커밋 → 상태 적용 (단방향)

  이 모델의 장점:
    1. 재생 가능: 로그를 처음부터 재생하면 동일한 상태 재구축
    2. 일관성: 모든 브로커가 같은 순서로 같은 레코드를 적용
    3. 감사(audit): 모든 변경의 이력이 로그에 보존
    4. 디버깅: 문제 발생 시 로그를 분석하여 원인 파악
```

---

## 12. MetadataLoader와 MetadataPublisher

### 12.1 브로커 측 메타데이터 적용

컨트롤러가 아닌 일반 브로커는 __cluster_metadata를 Fetch하여 메타데이터를 적용한다.

```
메타데이터 적용 흐름 (일반 브로커):

  +----------------------------------------------------+
  | KafkaRaftClient                                     |
  |  → __cluster_metadata를 Fetch (또는 스냅샷)          |
  +----------------------------------------------------+
                    |
                    v
  +----------------------------------------------------+
  | MetadataLoader                                      |
  |  - 스냅샷 또는 델타 레코드를 받음                      |
  |  - MetadataImage(불변 스냅샷)로 변환                  |
  |  - MetadataDelta(변경분)를 계산                      |
  +----------------------------------------------------+
                    |
                    v
  +----------------------------------------------------+
  | MetadataPublisher(들)                               |
  |  - BrokerMetadataPublisher                          |
  |    → ReplicaManager에 파티션 변경 적용              |
  |    → ConfigRepository 업데이트                      |
  |    → GroupCoordinator 업데이트                       |
  |  - 기타 Publisher들                                 |
  +----------------------------------------------------+
```

```
소스:
  metadata/src/main/java/org/apache/kafka/image/loader/MetadataLoader.java
```

### 12.2 MetadataImage

```
MetadataImage: 특정 시점의 전체 메타데이터 스냅샷 (불변)

  MetadataImage
    +-- provenance: MetadataProvenance (오프셋, 에포크, 타임스탬프)
    +-- features: FeaturesImage (기능 레벨)
    +-- cluster: ClusterImage (브로커 등록 정보)
    +-- topics: TopicsImage (토픽/파티션 정보)
    +-- configs: ConfigsImage (설정 정보)
    +-- acls: AclsImage (ACL 정보)

  MetadataDelta: 두 MetadataImage 간의 차이
    +-- topicsDelta: TopicsDelta (변경된 토픽/파티션)
    +-- clusterDelta: ClusterDelta (변경된 브로커)
    +-- configsDelta: ConfigsDelta (변경된 설정)
```

**왜 불변(immutable) MetadataImage인가?**

메타데이터 읽기는 매우 빈번하다(모든 Produce/Fetch 요청에서 파티션 메타데이터 조회).
불변 객체는 잠금 없이 안전하게 공유할 수 있고, 이전 이미지를 참조하는 요청이
진행 중이어도 새 이미지로 교체할 수 있다.

---

## 13. VoterSet: 동적 Voter 관리

### 13.1 VoterSet 구조

```java
// VoterSet.java (라인 48~)
public final class VoterSet {
    private final Map<Integer, VoterNode> voters;
}
```

```
VoterSet의 역할:

  정적 설정 (초기):
    controller.quorum.voters = 1@broker1:9093,2@broker2:9093,3@broker3:9093

  VoterSet으로 변환:
    VoterSet {
        1 → VoterNode(id=1, directoryId=UUID, endpoints={...})
        2 → VoterNode(id=2, directoryId=UUID, endpoints={...})
        3 → VoterNode(id=3, directoryId=UUID, endpoints={...})
    }
```

### 13.2 동적 Voter 추가/제거

KRaft는 투표자 집합을 동적으로 변경할 수 있다 (kraft.version >= 1).

```
Voter 추가 흐름:

  1. AddRaftVoterRequest(newVoterId, newDirectoryId, newEndpoints)
     → 활성 리더에 전송

  2. 리더가 VotersRecord 레코드 생성
     → __cluster_metadata에 기록

  3. 커밋 후 모든 노드의 VoterSet이 업데이트

  4. 새 투표자가 로그를 따라잡으면 선거에 참여 가능

Voter 제거 흐름:

  1. RemoveRaftVoterRequest(voterId, directoryId)
     → 활성 리더에 전송

  2. 리더가 새 VotersRecord 레코드 생성 (해당 voter 제외)

  3. 커밋 후 해당 voter는 더 이상 투표에 참여하지 않음

  ★ 중요: 한 번에 하나의 voter만 추가/제거 (안전성)
```

### 13.3 멤버십 변경 안전성

```
한 번에 하나만 변경하는 이유:

  현재 과반수: {A, B, C} → 과반수 = 2

  동시에 D, E 추가:
    새 과반수: {A, B, C, D, E} → 과반수 = 3

    문제: 전환 중 일부는 {A,B,C}, 일부는 {A,B,C,D,E}를 인식
    → 두 개의 독립적인 과반수가 동시에 존재할 수 있음!
    → 두 명의 리더가 동시에 선출될 수 있음!

  한 번에 하나만 변경:
    {A, B, C} → {A, B, C, D}
    과반수 2 → 과반수 3

    {A, B, C}의 과반수(2)와 {A, B, C, D}의 과반수(3)는
    반드시 1개 이상의 공통 멤버를 가짐 → 안전!
```

---

## 14. 설계 결정의 이유 (Why)

### 14.1 왜 ZooKeeper 대신 자체 Raft 구현인가?

1. **운영 복잡도 감소**: 별도의 ZooKeeper 클러스터 운영 불필요
2. **메타데이터 일관성**: 이벤트 로그 기반으로 순서 보장
3. **확장성**: 대규모 클러스터에서 ZooKeeper의 확장 한계 극복
4. **장애 복구 속도**: 전체 메타데이터 재로드 대신 증분 적용
5. **Kafka API 재사용**: Fetch, Produce 등 기존 API 활용

### 14.2 왜 AppendEntries 대신 Fetch를 사용하는가?

표준 Raft는 리더가 팔로워에게 AppendEntries를 Push한다. KRaft는 팔로워가
리더에서 Fetch(Pull)한다.

```
AppendEntries (Push):               Fetch (Pull):
  리더 → 팔로워: "이 데이터 받아"     팔로워 → 리더: "offset X부터 줘"
  → 리더가 각 팔로워 상태 추적       → 팔로워가 자신의 진행 상태 관리
  → 새 RPC 구현 필요                → 기존 Kafka Fetch API 재사용!
```

장점:
- Kafka의 기존 네트워크 계층, Fetch API, 프로토콜을 재사용
- 팔로워가 자신의 속도로 데이터를 가져감 (자연스러운 배압)
- 코드 복잡도 감소

### 14.3 왜 6가지 상태인가?

표준 Raft의 3가지 상태(Leader, Follower, Candidate)에 3가지를 추가했다:

```
추가 상태와 이유:

  Prospective:
    → Pre-Vote를 위한 상태
    → 불필요한 에포크 증가 방지
    → 네트워크 파티션 후 복구 시 안정성

  Unattached:
    → 어떤 리더도 모르는 상태
    → Follower와 분리하여 리더 발견 전 행동 정의
    → 투표 가능 여부 명확화

  Resigned:
    → 리더의 정상적 종료(graceful shutdown)
    → EndQuorumEpoch로 팔로워에 알림
    → 빠른 리더 재선출 유도
```

### 14.4 왜 HW를 중앙값으로 계산하는가?

Kafka 복제의 HW는 ISR 최소값이지만, KRaft의 HW는 투표자 오프셋의 중앙값이다.

```
ISR 최소값 방식 (Kafka 복제):
  ISR = {B1, B2, B3}
  LEO: B1=100, B2=90, B3=80
  HW = min(100, 90, 80) = 80

  ISR이 동적으로 변하므로 min이 의미 있음
  느린 복제본은 ISR에서 제거됨

중앙값 방식 (KRaft):
  Voters = {B1, B2, B3} (고정)
  LEO: B1=100, B2=90, B3=80
  정렬: [100, 90, 80]
  indexOfHw = 3/2 = 1
  HW = 90 (과반수 2/3이 90 이상)

  Voter는 고정이므로 ISR 개념 없음
  과반수 복제를 중앙값으로 직접 계산
  → Raft의 커밋 조건과 정확히 일치
```

### 14.5 왜 에포크 시작 후 최소 1개 커밋이 필요한가?

```
에포크 시작 커밋 없이의 문제:

  epoch=5: B1(리더)이 offset 100 기록 (과반수 미복제)
  epoch=6: B2가 새 리더로 선출, epochStartOffset=100
           B2의 offset 100에는 B1의 데이터가 없을 수 있음

  만약 HW를 100으로 올리면:
    → offset 100이 커밋되었다고 표시
    → 그런데 B2에는 offset 100에 다른 데이터가 있을 수 있음!
    → 일관성 위반!

  에포크 시작 커밋 조건 있을 때:
    B2가 epoch=6에서 자신의 레코드를 기록 (offset 101)
    → HW를 101로 올리면 이 시점에서 모든 과반수가
       epoch=6의 데이터를 가지고 있음이 보장됨
    → 이전 에포크의 데이터도 자연스럽게 커밋됨
```

### 14.6 왜 Pull 기반 스냅샷인가?

```
Push 기반 스냅샷 (표준 Raft: InstallSnapshot):
  리더 → 팔로워: "이 스냅샷 받아" (리더가 능동적으로 전송)
  → 리더가 팔로워의 상태를 추적해야 함
  → 대용량 스냅샷 전송 시 리더 부하

Pull 기반 스냅샷 (KRaft: FetchSnapshot):
  팔로워 → 리더: "스냅샷의 position X부터 보내줘"
  → 팔로워가 자신의 속도로 다운로드
  → 리더는 일반 Fetch처럼 처리
  → 스냅샷 전송 중에도 일반 팔로워 Fetch에 영향 없음
```

---

## 요약 테이블

| 상태 | 클래스 | 전환 조건 |
|------|--------|----------|
| Resigned | ResignedState.java | 리더 종료 시 |
| Unattached | UnattachedState.java | 새 에포크 학습, 투표 후 |
| Prospective | ProspectiveState.java | 선거 타임아웃 (Pre-Vote) |
| Candidate | CandidateState.java | Pre-Vote 과반수 승인 |
| Leader | LeaderState.java | Vote 과반수 승인 |
| Follower | FollowerState.java | 리더 발견 |

| RPC | 용도 | 방향 |
|-----|------|------|
| Vote | 실제 투표 | Candidate → All |
| PreVote | 사전 투표 | Prospective → All |
| BeginQuorumEpoch | 리더 선출 알림 | Leader → All |
| EndQuorumEpoch | 리더 사임 알림 | Resigned → All |
| Fetch | 로그 복제 | Follower → Leader |
| FetchSnapshot | 스냅샷 다운로드 | Follower → Leader |
| AddRaftVoter | Voter 추가 | Client → Leader |
| RemoveRaftVoter | Voter 제거 | Client → Leader |

| 설정 | 기본값 | 설명 |
|------|--------|------|
| controller.quorum.voters | (필수) | 초기 Voter 목록 |
| controller.quorum.election.timeout.ms | 1000 | 선거 타임아웃 |
| controller.quorum.fetch.timeout.ms | 2000 | Fetch 타임아웃 |
| controller.quorum.election.backoff.max.ms | 1000 | 선거 백오프 최대값 |
| metadata.log.dir | (필수) | 메타데이터 로그 디렉토리 |
| metadata.log.segment.bytes | 1073741824 | 세그먼트 크기 |
| metadata.log.max.record.bytes.between.snapshots | 20971520 | 스냅샷 간격 |

---

## 참고 소스 파일 전체 경로

```
raft/src/main/java/org/apache/kafka/raft/
  +-- KafkaRaftClient.java        # Raft 클라이언트 핵심 (모든 RPC 처리)
  +-- QuorumState.java            # 상태 머신 관리
  +-- EpochState.java             # 상태 인터페이스
  +-- LeaderState.java            # 리더 상태 (HW 계산, CheckQuorum)
  +-- FollowerState.java          # 팔로워 상태 (Fetch 타이머)
  +-- CandidateState.java         # 후보 상태 (Vote 수집)
  +-- ProspectiveState.java       # Pre-Vote 상태
  +-- UnattachedState.java        # 미연결 상태
  +-- ResignedState.java          # 사임 상태
  +-- NomineeState.java           # Candidate + Prospective 공통 인터페이스
  +-- VoterSet.java               # Voter 집합 관리
  +-- ElectionState.java          # 선거 상태 영속화
  +-- FileQuorumStateStore.java   # quorum-state 파일 저장소
  +-- RaftLog.java                # Raft 로그 인터페이스
  +-- ReplicaKey.java             # 복제본 식별자 (ID + DirectoryId)

raft/src/main/java/org/apache/kafka/raft/internals/
  +-- BatchAccumulator.java       # 배치 누적기
  +-- KafkaRaftMetrics.java       # Raft 메트릭
  +-- EpochElection.java          # 에포크 선거 추적
  +-- AddVoterHandler.java        # Voter 추가 핸들러
  +-- RemoveVoterHandler.java     # Voter 제거 핸들러
  +-- KRaftControlRecordStateMachine.java # 제어 레코드 상태 머신

metadata/src/main/java/org/apache/kafka/controller/
  +-- QuorumController.java       # 컨트롤러 상태 머신

metadata/src/main/java/org/apache/kafka/image/loader/
  +-- MetadataLoader.java         # 메타데이터 로더 (브로커 측)
```
