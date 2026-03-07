# PoC-03: Kafka Leader-Follower Replication with ISR

## 개요

Kafka의 Leader-Follower 복제 모델과 ISR(In-Sync Replicas) 관리를 시뮬레이션한다. 리더가 쓰기를 수락하고 팔로워가 주기적으로 fetch하며, ISR 기반으로 High Watermark를 계산한다.

## 실제 Kafka 소스 참조

| 클래스 | 경로 | 역할 |
|--------|------|------|
| `Partition.scala` | `core/src/main/scala/kafka/cluster/Partition.scala` | ISR 관리, HW 계산, 팔로워 상태 추적 |
| `ReplicaManager.scala` | `core/src/main/scala/kafka/server/ReplicaManager.scala` | 복제 관리, fetch 요청 처리 |

## 시뮬레이션하는 핵심 알고리즘

### 1. 팔로워 Fetch & 상태 업데이트
- `Partition.updateFollowerFetchState()`: 팔로워의 LEO, lastCaughtUpTime 업데이트
- 팔로워가 리더의 LEO에 도달하면 `lastCaughtUpTime` 갱신

### 2. ISR 확장 (maybeExpandIsr)
- `Partition.scala:876-894`: ISR에 없는 팔로워가 리더의 HW를 따라잡으면 ISR에 추가
- 조건: `followerLEO >= leaderHW && followerLEO >= leaderEpochStartOffset`

### 3. ISR 축소 (maybeShrinkIsr)
- `Partition.scala:1089-1127`: 지연된 팔로워를 ISR에서 제거
- `getOutOfSyncReplicas()`: `lastCaughtUpTime + replicaLagTimeMaxMs < currentTime`
- 축소 후 HW가 변경될 수 있음

### 4. High Watermark 계산
- HW = ISR 내 모든 레플리카의 LEO 중 최솟값
- HW 이하의 메시지만 컨슈머에게 노출 (데이터 안전성 보장)
- ISR 변경 시 HW 재계산

## 실행 방법

```bash
go run main.go
```

## 출력 내용

1. 정상 복제: 리더 쓰기 -> 팔로워 fetch -> HW 갱신
2. 느린 팔로워: ISR 축소 동작
3. 팔로워 복구: ISR 재진입
4. 동시 쓰기/복제 시뮬레이션 (2초 실행)
5. ISR/HW 변경 이벤트 로그

## ISR 관리 타임라인 예시

```
시간    리더 LEO    Follower-1 LEO    Follower-2 LEO    ISR            HW
t=0     0           0                 0                 {0,1,2}        0
t=1     5           5                 3                 {0,1,2}        3
t=2     10          10                3                 {0,1,2}        3
t=3     15          15                3                 {0,1}          15   <- F2 축소
t=4     20          20                20                {0,1,2}        20   <- F2 복구
```
