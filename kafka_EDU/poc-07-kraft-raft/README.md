# PoC-07: KRaft (Kafka Raft) 합의 프로토콜

## 개요

KRaft는 Kafka의 ZooKeeper 대체를 위한 Raft 합의 프로토콜 구현이다. 이 PoC는 3노드 클러스터에서의 리더 선출, 로그 복제, 하이 워터마크 계산을 시뮬레이션한다.

## 실행 방법

```bash
go run main.go
```

## Kafka 소스코드 참조

| 컴포넌트 | 원본 파일 | 설명 |
|----------|----------|------|
| KafkaRaftClient | `raft/src/main/java/.../raft/KafkaRaftClient.java` | Raft 프로토콜 메인 클라이언트 |
| LeaderState | `raft/src/main/java/.../raft/LeaderState.java` | 리더 상태 관리, 하이 워터마크 계산 |
| CandidateState | `raft/src/main/java/.../raft/CandidateState.java` | 후보 상태, 투표 추적 |
| EpochElection | `raft/src/main/java/.../raft/internals/EpochElection.java` | 에포크별 투표 관리 |

## 시뮬레이션하는 핵심 개념

### 1. 리더 선출 (VoteRequest/VoteResponse)

```
CandidateState 생성자:
  1. epoch 증가
  2. epochElection = new EpochElection(voters.voterKeys())
  3. epochElection.recordVote(localId, true)  // 자기 투표
  4. 다른 투표자에게 VoteRequest 전송

투표 승인 조건 (handleVoteRequest):
  - 후보 에포크 >= 내 에포크
  - 아직 투표하지 않았거나 같은 후보에게 투표
  - 후보의 로그가 최신 (lastLogEpoch, lastLogOffset 비교)
```

### 2. 에포크 기반 리더 추적

```
모든 Raft 메시지에 epoch가 포함된다.
더 높은 epoch를 가진 메시지를 받으면 즉시 팔로워로 전환한다.
이를 통해 네트워크 분할 후 재합류 시 자동으로 정상화된다.
```

### 3. 하이 워터마크 계산 (LeaderState.java:727)

```java
// maybeUpdateHighWatermark()
ArrayList<ReplicaState> followersByDescendingFetchOffset =
    followersByDescendingFetchOffset().collect(Collectors.toCollection(ArrayList::new));

int indexOfHw = voterStates.size() / 2;
Optional<LogOffsetMetadata> highWatermarkUpdateOpt =
    followersByDescendingFetchOffset.get(indexOfHw).endOffset;
```

3노드 예시:
```
오프셋: [10, 7, 5] (내림차순)
indexOfHw = 3/2 = 1
하이 워터마크 = 7 (과반수인 2노드가 7 이상 복제)
```

### 4. 선거 타임아웃 랜덤화

```
electionTimeoutMs = baseTimeout + random(0, baseTimeout)
이를 통해 여러 노드가 동시에 선거를 시작하는 것(split vote)을 방지한다.
```

### 5. 로그 복제

```
Kafka Raft에서는 표준 Raft와 달리 팔로워가 Fetch 요청으로 로그를 Pull한다.
리더는 Fetch 응답에 로그 엔트리와 하이 워터마크를 포함한다.
```

## 상태 전이 다이어그램

```
                  선거 타임아웃
    ┌──────────────────────────────┐
    │                              ▼
 Follower ──────────────────→ Candidate
    ▲                           │  │
    │  더 높은 epoch 발견       │  │ 과반수 투표 획득
    │  또는 새 리더 발견        │  ▼
    │                         Leader
    │                           │
    └───────────────────────────┘
         더 높은 epoch 발견
```

## Kafka Raft vs 표준 Raft

| 항목 | 표준 Raft | KRaft |
|------|----------|-------|
| 리더 선출 | RequestVote RPC | VoteRequest (거의 동일) |
| 로그 복제 | AppendEntries (push) | Fetch (pull) |
| 로그 조정 | nextIndex 감소 | Kafka 로그 재조정 프로토콜 |
| 리더십 통보 | 첫 heartbeat | BeginQuorumEpoch (별도 API) |
| 멤버십 변경 | Configuration change | AddRaftVoter/RemoveRaftVoter |
