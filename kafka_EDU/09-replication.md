# 09. Kafka 복제 시스템 Deep-Dive

## 목차

1. [개요](#1-개요)
2. [복제 아키텍처](#2-복제-아키텍처)
3. [ReplicaManager](#3-replicamanager)
4. [Partition 클래스](#4-partition-클래스)
5. [ISR 관리](#5-isr-관리)
6. [High Watermark](#6-high-watermark)
7. [Leader Epoch](#7-leader-epoch)
8. [ReplicaFetcherThread](#8-replicafetcherthread)
9. [Fetch Isolation](#9-fetch-isolation)
10. [AlterPartition 요청](#10-alterpartition-요청)
11. [Unclean Leader Election](#11-unclean-leader-election)
12. [장애 시나리오와 복구](#12-장애-시나리오와-복구)
13. [설계 결정의 이유 (Why)](#13-설계-결정의-이유-why)

---

## 1. 개요

Kafka의 복제 시스템은 **데이터 내구성**과 **고가용성**의 핵심이다. 각 파티션은
설정된 수(`replication.factor`)만큼의 복제본을 가지며, 하나가 리더, 나머지가 팔로워로
동작한다.

### 핵심 원칙

1. **단일 리더 복제**: 모든 쓰기/읽기는 리더를 통해 처리
2. **풀(Pull) 기반 복제**: 팔로워가 리더에서 데이터를 가져옴 (리더가 밀어넣지 않음)
3. **ISR(In-Sync Replicas)**: 동기화된 복제본 집합으로 내구성 보장
4. **High Watermark**: 모든 ISR에 복제 완료된 오프셋까지만 컨슈머에게 노출

### 핵심 소스 파일

| 파일 | 경로 | 역할 |
|------|------|------|
| ReplicaManager.scala | `core/src/main/scala/kafka/server/ReplicaManager.scala` | 복제 관리자 |
| Partition.scala | `core/src/main/scala/kafka/cluster/Partition.scala` | 파티션 상태 관리 |
| ReplicaFetcherThread.scala | `core/src/main/scala/kafka/server/ReplicaFetcherThread.scala` | 팔로워 페치 스레드 |
| AbstractFetcherThread.scala | `core/src/main/scala/kafka/server/AbstractFetcherThread.scala` | 페치 스레드 기반 |
| FetchIsolation.java | `server-common/src/main/java/org/apache/kafka/server/storage/log/FetchIsolation.java` | 읽기 격리 수준 |
| UnifiedLog.java | `storage/.../internals/log/UnifiedLog.java` | 로그 읽기/쓰기 |

---

## 2. 복제 아키텍처

### 2.1 Leader-Follower 모델

```
Kafka 파티션 복제 아키텍처 (replication.factor=3):

  프로듀서                                        컨슈머
    |                                               ^
    | Produce                                       | Fetch
    v                                               |
  +-----------+   Fetch    +-----------+          +-----------+
  | Broker 1  |<-----------| Broker 2  |          | Broker 1  |
  | Partition  |            | Partition  |          | (Leader)  |
  | (Leader)  |<-----------| (Follower) |          +-----------+
  +-----------+   Fetch    +-----------+
       ^
       |         Fetch    +-----------+
       +------------------| Broker 3  |
                          | Partition  |
                          | (Follower) |
                          +-----------+

  ★ 프로듀서: 리더에만 쓰기
  ★ 팔로워: 리더에서 풀(Pull)로 데이터 가져오기
  ★ 컨슈머: 리더에서 읽기 (또는 팔로워 읽기 가능, KIP-392)
```

### 2.2 복제 데이터 흐름

```
데이터 복제 흐름:

  1. 프로듀서 → 리더: Produce 요청
     ReplicaManager.appendRecords() → UnifiedLog.appendAsLeader()

  2. 팔로워 → 리더: Fetch 요청 (ReplicaFetcherThread)
     ReplicaManager.readFromLog() → UnifiedLog.read()

  3. 팔로워: 가져온 데이터를 로컬에 기록
     ReplicaFetcherThread.processPartitionData() → UnifiedLog.appendAsFollower()

  4. 리더: 팔로워의 Fetch 오프셋으로 ISR/HW 업데이트
     Partition.maybeExpandIsr() / Partition.updateFollowerFetchState()

  시간 흐름:
  t0: 프로듀서 → 리더에 offset 100 기록
  t1: 팔로워A가 리더에서 offset 100 가져감 → 로컬에 기록
  t2: 팔로워B가 리더에서 offset 100 가져감 → 로컬에 기록
  t3: 리더가 HW를 100으로 올림 (모든 ISR이 100까지 복제 완료)
  t4: 컨슈머가 offset 100까지 읽기 가능
```

---

## 3. ReplicaManager

### 3.1 구조

```scala
// ReplicaManager.scala (라인 84~)
object ReplicaManager {
    val HighWatermarkFilename = "replication-offset-checkpoint"

    private val LeaderCountMetricName = "LeaderCount"
    private val PartitionCountMetricName = "PartitionCount"
    private val OfflineReplicaCountMetricName = "OfflineReplicaCount"
    private val UnderReplicatedPartitionsMetricName = "UnderReplicatedPartitions"
    private val UnderMinIsrPartitionCountMetricName = "UnderMinIsrPartitionCount"
    private val AtMinIsrPartitionCountMetricName = "AtMinIsrPartitionCount"
    private val IsrExpandsPerSecMetricName = "IsrExpandsPerSec"
    private val IsrShrinksPerSecMetricName = "IsrShrinksPerSec"
    private val FailedIsrUpdatesPerSecMetricName = "FailedIsrUpdatesPerSec"
}
```

### 3.2 핵심 메트릭

```
ReplicaManager 메트릭:

  +----------------------------------+------------------------------------+
  | 메트릭                            | 의미                               |
  +----------------------------------+------------------------------------+
  | LeaderCount                      | 이 브로커가 리더인 파티션 수          |
  | PartitionCount                   | 이 브로커의 전체 파티션 수            |
  | UnderReplicatedPartitions        | ISR < 복제 팩터인 파티션 수          |
  | UnderMinIsrPartitionCount        | ISR < min.insync.replicas인 수     |
  | AtMinIsrPartitionCount           | ISR == min.insync.replicas인 수    |
  | OfflineReplicaCount              | 오프라인 복제본 수                   |
  | IsrExpandsPerSec                 | ISR 확장 빈도                       |
  | IsrShrinksPerSec                 | ISR 축소 빈도                       |
  | FailedIsrUpdatesPerSec           | ISR 업데이트 실패 빈도               |
  +----------------------------------+------------------------------------+
```

### 3.3 appendRecords()

프로듀서의 Produce 요청을 처리하는 핵심 메서드이다.

```scala
// ReplicaManager.scala (라인 634~)
def appendRecords(timeout: Long, requiredAcks: Short, ...)
```

```
appendRecords() 흐름:

  appendRecords(timeout, requiredAcks=-1, records)
      |
      +--- 1. 로컬 로그에 기록
      |        appendRecordsToLeader(partition, records)
      |          → partition.appendRecordsToLeader()
      |            → log.appendAsLeader(records, leaderEpoch)
      |
      +--- 2. acks에 따른 처리
      |    |
      |    +--- acks=0: 즉시 반환 (응답 없음)
      |    |
      |    +--- acks=1: 로컬 쓰기 완료 시 응답
      |    |
      |    +--- acks=-1 (all):
      |           DelayedProduce에 등록
      |           → ISR 전체가 해당 오프셋까지 복제 완료 시 응답
      |           → timeout 초과 시 타임아웃 응답
      |
      +--- 3. DelayedProduce 완료 조건 확인
               각 팔로워가 Fetch할 때마다 tryComplete() 호출
               → 모든 ISR의 LEO >= 해당 오프셋이면 완료
```

### 3.4 readFromLog()

```scala
// ReplicaManager.scala (라인 1733~)
def readFromLog(...)
```

```
readFromLog() 흐름:

  readFromLog(fetchParams, partitionData)
      |
      +--- 1. Fetch Isolation 결정
      |        fetchParams.isolation:
      |          LOG_END (복제 요청)
      |          HIGH_WATERMARK (일반 컨슈머)
      |          TXN_COMMITTED (READ_COMMITTED 컨슈머)
      |
      +--- 2. 각 파티션에서 읽기
      |        partition.readRecords(
      |            fetchOffset, maxBytes, isolation, ...)
      |          → log.read(startOffset, maxLength,
      |                     isolation, minOneMessage=true)
      |
      +--- 3. 팔로워 Fetch인 경우
               → updateFollowerFetchState(replicaId, offset)
               → maybeExpandIsr()  (ISR 확장 판단)
               → maybeIncrementLeaderHW()  (HW 상승 판단)
```

---

## 4. Partition 클래스

### 4.1 파티션 상태 관리

```
소스: core/src/main/scala/kafka/cluster/Partition.scala

Partition 클래스의 핵심 상태:

  +-----------------------------------------------+
  | Partition                                      |
  |                                                |
  | topicPartition: TopicPartition                 |
  | log: Option[UnifiedLog]       (로컬 로그)       |
  | leaderReplicaIdOpt: Option[Int]                |
  |                                                |
  | ISR 관리:                                       |
  |   inSyncReplicaIds: Set[Int]  (ISR 멤버)        |
  |   maxIdleMs: Long             (ISR 축소 임계값)  |
  |                                                |
  | 리더 상태 (리더일 때):                            |
  |   leaderEpoch: Int                             |
  |   partitionEpoch: Int                          |
  |   remoteReplicas: Map[Int, Replica]            |
  |                                                |
  | 팔로워 상태 (팔로워일 때):                         |
  |   leaderReplicaIdOpt = Some(leaderId)          |
  +-----------------------------------------------+
```

### 4.2 리더/팔로워 전환

```
Partition의 리더/팔로워 전환:

  컨트롤러에서 LeaderAndIsr 요청 도착
      |
      +--- makeLeader(partitionRegistration)
      |      1. leaderReplicaIdOpt = Some(localBrokerId)
      |      2. ISR 설정
      |      3. HW 초기화 (체크포인트에서 로드)
      |      4. ReplicaFetcher 중지 (더 이상 팔로워 아님)
      |
      +--- makeFollower(partitionRegistration)
             1. leaderReplicaIdOpt = Some(newLeaderId)
             2. HW는 변경하지 않음 (페치하면서 갱신)
             3. ReplicaFetcher 시작 (리더에서 데이터 가져오기)
```

---

## 5. ISR 관리

### 5.1 ISR (In-Sync Replicas) 개념

ISR은 리더와 "동기화된" 복제본의 집합이다. ISR에 속하려면 다음 조건을 만족해야 한다:

```
ISR 멤버 조건:
  1. 팔로워의 마지막 Fetch 시간이 replica.lag.time.max.ms 이내
  2. 팔로워의 LEO(Log End Offset)가 리더의 HW(High Watermark) 이상

  이 두 조건은 "팔로워가 충분히 빠르게 복제하고 있는가"를 판단한다.
```

### 5.2 ISR 확장 (maybeExpandIsr)

```scala
// Partition.scala (라인 876~)
private def maybeExpandIsr(followerReplica: Replica): Unit = {
    // ISR에 없는 팔로워가 충분히 따라잡았으면 ISR에 추가
}
```

```
ISR 확장 조건 (Partition.scala 라인 910~):

  followerEndOffset >= leaderLog.highWatermark
  && leaderEpochStartOffsetOpt.exists(followerEndOffset >= _)

  즉:
    1. 팔로워의 LEO >= 리더의 HW  (데이터가 충분히 복제됨)
    2. 팔로워의 LEO >= 현재 리더 에포크의 시작 오프셋 (에포크 전환 후 데이터 확인)
```

```
ISR 확장 예시:

  초기: ISR = {B1(leader), B2}  HW = 100

  B3가 복제를 시작:
  t0: B3.LEO = 50   (ISR 외부, 아직 미달)
  t1: B3.LEO = 80   (아직 미달)
  t2: B3.LEO = 100  (B3.LEO >= HW=100 ★ ISR 추가 조건 충족)

  ISR = {B1(leader), B2, B3}

  컨트롤러에 AlterPartition 요청 전송
```

### 5.3 ISR 축소 (maybeShrinkIsr)

```scala
// Partition.scala (라인 1089~)
def maybeShrinkIsr(): Unit = {
    // replica.lag.time.max.ms 이상 Fetch하지 않은 팔로워를 ISR에서 제거
}
```

```
ISR 축소 흐름:

  리더가 주기적으로 maybeShrinkIsr() 호출
      |
      +--- 각 ISR 멤버의 마지막 Fetch 시간 확인
      |
      +--- (현재시간 - lastFetchTime) > replica.lag.time.max.ms?
             |
             Yes: ISR에서 제거
             → AlterPartition 요청으로 컨트롤러에 알림
```

```
ISR 축소 예시:

  replica.lag.time.max.ms = 30000 (30초)

  t0: ISR = {B1(leader), B2, B3}
      B2: lastFetch = 현재 - 5초  (정상)
      B3: lastFetch = 현재 - 35초 (★ 초과!)

  t1: ISR = {B1(leader), B2}
      → B3 제거, HW는 B1과 B2의 LEO 기준으로 계산

  출력 로그:
    "Leader: (highWatermark: 5000, endOffset: 5100),
     Slow replicas: (B3, lagTime: 35000ms)"
```

**왜 lag 바이트가 아닌 lag 시간으로 판단하는가?**

메시지 생산 속도가 높을 때 팔로워가 수 MB 뒤처져 있더라도, 그것이 단 몇 초 전의
데이터라면 팔로워는 "동기화" 상태로 볼 수 있다. 반대로 생산이 멈춘 토픽에서
팔로워가 1 메시지만 뒤처져도, 오랫동안 Fetch하지 않았다면 문제다. 시간 기반
판단이 다양한 워크로드에서 더 정확하다.

---

## 6. High Watermark

### 6.1 HW 개념

High Watermark(HW)는 **모든 ISR 멤버에게 복제 완료된 오프셋**이다.
컨슈머는 HW까지의 메시지만 읽을 수 있다.

```
HW 계산:

  ISR = {B1(leader), B2, B3}

  B1 (leader): LEO = 105
  B2:          LEO = 103
  B3:          LEO = 101

  HW = min(ISR 멤버들의 LEO) = min(105, 103, 101) = 101

  컨슈머 가시성:
    오프셋 0~100: 읽기 가능
    오프셋 101~104: 읽기 불가 (HW 이후)
```

```
HW와 LEO의 관계:

  로그:  [0] [1] [2] ... [99] [100] [101] [102] [103] [104]
                                      ^                  ^
                                     HW                 LEO
                                      |                  |
                                 컨슈머 한계      리더의 마지막 오프셋

  |<---- 커밋된 메시지 ---->|<-- 미커밋 메시지 -->|
  |  (모든 ISR에 복제 완료)  | (일부만 복제 완료)  |
```

### 6.2 HW 상승 메커니즘

```
HW 상승 흐름:

  1. 프로듀서가 리더에 메시지 기록 → LEO 증가
  2. 팔로워A가 Fetch → 리더가 팔로워A의 LEO 업데이트
  3. 팔로워B가 Fetch → 리더가 팔로워B의 LEO 업데이트
  4. 리더가 HW = min(ISR LEO) 계산
  5. HW가 상승했으면:
     - DelayedProduce 완료 확인 (acks=-1)
     - DelayedFetch 완료 확인

  시간 순서:
  t0: LEO(leader)=100, LEO(f1)=99, LEO(f2)=98  → HW=98
  t1: f2 Fetch → LEO(f2)=99                      → HW=99
  t2: f1 Fetch → LEO(f1)=100                     → HW=99 (변화 없음, f2=99)
  t3: f2 Fetch → LEO(f2)=100                     → HW=100 (★ 상승!)
```

### 6.3 HW 체크포인트

```
HW 체크포인트:

  파일: replication-offset-checkpoint
  위치: 각 log.dirs 루트

  내용:
    0                         ← 버전
    3                         ← 파티션 수
    my-topic 0 5367851        ← 토픽 파티션 HW
    my-topic 1 2834012
    my-topic 2 7891234

  주기: replica.high.watermark.checkpoint.interval.ms (기본 5000ms)
```

**왜 HW를 주기적으로 체크포인트하는가?**

브로커가 크래시 후 재시작하면 팔로워는 HW 이후의 데이터를 잘라내야 할 수 있다
(리더가 바뀌었을 수 있으므로). 체크포인트 없이는 팔로워가 어디까지 안전한지 알 수
없다. 마지막 체크포인트의 HW가 안전한 복구 시작점이 된다.

---

## 7. Leader Epoch

### 7.1 왜 Leader Epoch가 필요한가?

HW만으로는 리더 변경 시 데이터 일관성을 완벽하게 보장할 수 없다.

```
HW만 사용할 때의 문제 (데이터 손실 시나리오):

  시나리오: B1(리더), B2(팔로워), replication.factor=2

  t0: B1에 offset 0,1 기록. B2가 offset 0까지 복제. HW=0
  t1: B2가 offset 1 Fetch → B2.LEO=2, 하지만 HW=0 (아직 B1이 HW 안 올림)
  t2: B1 크래시
  t3: B2가 새 리더로 선출
  t4: B2가 HW(0) 이후 데이터 삭제 → offset 1 손실!
  t5: 그런데 offset 1은 B1에도 B2에도 있었음 → 불필요한 데이터 손실!

  ★ 문제: HW 전파가 Fetch 응답에 1단계 지연되어 발생
```

### 7.2 Leader Epoch 메커니즘

Leader Epoch는 리더 변경마다 증가하는 카운터로, 각 에포크의 시작 오프셋을 기록한다.

```
Leader Epoch 체크포인트 예시:

  leader-epoch-checkpoint 파일:
    0          ← 버전
    3          ← 엔트리 수
    0 0        ← epoch=0, startOffset=0   (B1이 초기 리더)
    1 5000     ← epoch=1, startOffset=5000 (B2로 리더 변경)
    2 12000    ← epoch=2, startOffset=12000 (B3로 리더 변경)

  의미: epoch K의 데이터는 [startOffset_K, startOffset_{K+1}) 범위
```

### 7.3 Leader Epoch 기반 복구

```
Leader Epoch를 사용한 복구:

  B2가 팔로워로 재시작 시:
    1. 리더(B1)에게 OffsetsForLeaderEpoch 요청
       → "내 마지막 에포크 X의 끝 오프셋은?"
    2. 리더 응답: "에포크 X의 끝 오프셋은 Y"
    3. B2의 LEO > Y이면: Y 이후 데이터 잘라냄 (truncate)
    4. B2의 LEO <= Y이면: 잘라낼 필요 없음

  예시:
    B2의 상태: epoch=1, LEO=5500
    B1(리더) 응답: epoch=1의 끝은 5300

    → B2가 offset 5300 이후 데이터 삭제 (5300~5499 제거)
    → 리더에서 5300부터 다시 Fetch

    이 과정에서 HW 대신 Leader Epoch를 사용하므로
    불필요한 데이터 손실이 방지됨
```

```
Leader Epoch vs HW 비교:

  상황: 리더 B1(epoch=5), 팔로워 B2(epoch=5)
        B1.LEO=100, B2.LEO=98, HW=95

  B1 크래시 → B2가 새 리더 (epoch=6)

  HW 기반 복구:
    B2는 HW(95) 이후 잘라냄 → offset 95~97 손실
    ★ 하지만 95~97은 B2에 안전하게 있었음!

  Leader Epoch 기반 복구:
    B2는 epoch=5의 끝(=98)까지 보존
    → 데이터 손실 없음 ★
```

---

## 8. ReplicaFetcherThread

### 8.1 구조

```scala
// ReplicaFetcherThread.scala (라인 29~)
class ReplicaFetcherThread(name: String,
                           leader: LeaderEndPoint,
                           brokerConfig: KafkaConfig,
                           failedPartitions: FailedPartitions,
                           replicaMgr: ReplicaManager,
                           quota: ReplicaQuota,
                           logPrefix: String)
  extends AbstractFetcherThread(...)
```

### 8.2 페치 루프

```
ReplicaFetcherThread 실행 루프:

  while (isRunning) {
      // 1. 파티션별 Fetch 요청 구성
      fetchRequestMap = buildFetch(partitions)
        → 각 파티션: (fetchOffset, maxBytes, epoch)

      // 2. 리더에게 Fetch 요청 전송
      response = leader.fetch(fetchRequestMap)

      // 3. 응답 처리
      for ((partition, partitionData) <- response) {
          processPartitionData(partition, partitionData)
      }

      // 4. 에러 처리
      handlePartitionsWithErrors(errPartitions)
  }
```

### 8.3 processPartitionData()

```scala
// ReplicaFetcherThread.scala (라인 112~)
override def processPartitionData(
    topicPartition: TopicPartition,
    fetchOffset: Long,
    partitionData: FetchData): Option[LogAppendInfo] = {
    // ...
}
```

```
processPartitionData() 흐름:

  processPartitionData(tp, fetchOffset, data)
      |
      +--- 1. 응답의 레코드 크기 확인
      |
      +--- 2. 로컬 로그에 기록
      |        log.appendAsFollower(records)
      |        (리더가 할당한 오프셋을 그대로 사용)
      |
      +--- 3. HW 업데이트
      |        followerHighWatermark = min(localLEO, leaderHW)
      |        if (followerHighWatermark > log.highWatermark)
      |            log.updateHighWatermark(followerHighWatermark)
      |            → partitionsWithNewHighWatermark에 추가
      |
      +--- 4. 새 HW가 있으면 DelayedProduce/Fetch 확인
```

```
팔로워의 HW 업데이트:

  팔로워는 리더의 HW를 Fetch 응답에서 받음
  팔로워의 HW = min(자신의 LEO, 리더의 HW)

  예시:
    리더 HW = 100, 팔로워 LEO = 95
    → 팔로워 HW = min(95, 100) = 95

    다음 Fetch 후: 팔로워 LEO = 100
    → 팔로워 HW = min(100, 100) = 100
```

### 8.4 Tiered Storage와 ReplicaFetcher

```scala
// ReplicaFetcherThread.scala (라인 66~)
override protected[server] def shouldFetchFromLastTieredOffset(
    topicPartition: TopicPartition,
    leaderEndOffset: Long,
    replicaEndOffset: Long): Boolean = {
    val remoteStorageEnabled = replicaMgr.localLog(topicPartition).exists(_.remoteLogEnabled())
    brokerConfig.followerFetchLastTieredOffsetEnable &&
        remoteStorageEnabled &&
        !isCompactTopic &&
        replicaEndOffset == 0 &&
        leaderEndOffset != 0
}
```

새 팔로워가 처음부터 복제할 때, Tiered Storage가 활성화되어 있으면 전체 로그를
복제하는 대신 마지막 티어된 오프셋부터 시작할 수 있다.

---

## 9. Fetch Isolation

### 9.1 FetchIsolation 열거형

```java
// FetchIsolation.java
public enum FetchIsolation {
    LOG_END,         // 복제 Fetch: 리더의 전체 데이터
    HIGH_WATERMARK,  // 일반 컨슈머: HW까지만
    TXN_COMMITTED;   // READ_COMMITTED: Last Stable Offset까지만
}
```

### 9.2 격리 수준별 가시성

```
격리 수준별 가시성 경계:

  로그: [0] [1] [2] ... [90] [91] [92] [93] [94] [95] [96] [97] [98] [99]
                                ^           ^           ^              ^
                               LSO         HW         ---            LEO
                                |           |                         |
                          TXN_COMMITTED  HIGH_WATERMARK           LOG_END

  LSO (Last Stable Offset):
    진행 중인 트랜잭션의 시작 오프셋 이전까지
    → READ_COMMITTED 컨슈머의 한계

  HW (High Watermark):
    모든 ISR에 복제 완료된 오프셋
    → READ_UNCOMMITTED 컨슈머의 한계

  LEO (Log End Offset):
    리더의 마지막 오프셋
    → 팔로워 복제의 한계
```

```java
// FetchIsolation.java
public static FetchIsolation of(int replicaId, IsolationLevel isolationLevel) {
    if (!FetchRequest.isConsumer(replicaId)) {
        return LOG_END;          // 팔로워 복제 → 전체 데이터
    } else if (isolationLevel == IsolationLevel.READ_COMMITTED) {
        return TXN_COMMITTED;    // READ_COMMITTED 컨슈머 → LSO까지
    } else {
        return HIGH_WATERMARK;   // 일반 컨슈머 → HW까지
    }
}
```

### 9.3 LSO (Last Stable Offset) 계산

```
LSO 계산:

  진행 중인 트랜잭션:
    - P1: 시작 offset=90 (아직 커밋/중단 안 됨)
    - P2: 시작 offset=85, 커밋 offset=92

  LSO = min(진행 중인 모든 트랜잭션의 시작 오프셋)
      = min(90) = 90

  READ_COMMITTED 컨슈머:
    → offset 0~89까지만 읽기 가능
    → P2의 커밋된 메시지도 90 이후에 있으면 아직 안 보임
       (하지만 P2는 이미 커밋됨 → LSO 이동 후 보임)
```

**왜 LSO가 필요한가?**

READ_COMMITTED 컨슈머는 커밋된 트랜잭션 메시지만 봐야 한다. 그런데 메시지들은
오프셋 순서대로 기록되므로, 진행 중인 트랜잭션(P1)의 메시지가 커밋된 트랜잭션(P2)의
메시지 사이에 끼어 있을 수 있다. LSO는 이런 상황에서 "여기까지는 모든 트랜잭션의
결과가 확정됨"을 보장하는 경계다.

---

## 10. AlterPartition 요청

### 10.1 ISR 변경 알림

ISR이 변경되면 리더는 컨트롤러에 `AlterPartition` 요청을 보내 변경을 알린다.

```
AlterPartition 흐름:

  리더 (Broker)                        컨트롤러
       |                                   |
       | ISR 변경 감지                       |
       | (확장 또는 축소)                    |
       |                                   |
       | AlterPartition Request             |
       | {topic, partition,                 |
       |  leaderEpoch, partitionEpoch,      |
       |  newIsr=[1,2,3]}                   |
       |---------------------------------->|
       |                                   | ISR 변경 검증
       |                                   | (leaderEpoch, partitionEpoch 확인)
       |                                   | 메타데이터 업데이트
       |                                   |
       | AlterPartition Response            |
       | {error=NONE, newIsr=[1,2,3],       |
       |  newLeaderEpoch, newPartitionEpoch}|
       |<----------------------------------|
       |                                   |
       | ISR 확정, 로컬 상태 업데이트         |
```

### 10.2 파티션 에포크

```
파티션 에포크(Partition Epoch):

  ISR 변경마다 파티션 에포크가 증가한다.
  이는 ISR 변경의 원자성을 보장한다.

  t0: ISR={1,2,3}, partitionEpoch=5
  t1: B3 느림 → ISR={1,2}, partitionEpoch=6
  t2: B3 복구 → ISR={1,2,3}, partitionEpoch=7

  만약 두 개의 ISR 변경이 동시에 발생하면:
    - AlterPartition 요청에 partitionEpoch 포함
    - 컨트롤러는 현재 에포크와 일치하는 요청만 수락
    - 불일치하면 FENCED_LEADER_EPOCH 에러 반환
    - → 리더가 최신 상태로 갱신 후 재시도
```

---

## 11. Unclean Leader Election

### 11.1 개념

ISR의 모든 멤버가 오프라인인 경우, 두 가지 선택이 있다:

```
ISR 전체 오프라인 시 선택:

  옵션 A: 가용성 우선 (unclean.leader.election.enable=true)
    → ISR 외부의 복제본을 리더로 선출
    → ★ 데이터 손실 가능! (ISR 외부 복제본은 최신 데이터가 없을 수 있음)

  옵션 B: 일관성 우선 (unclean.leader.election.enable=false, 기본값)
    → ISR 멤버가 복구될 때까지 파티션 오프라인
    → ★ 데이터 손실 없음, 하지만 가용성 저하
```

### 11.2 min.insync.replicas

```
min.insync.replicas의 역할:

  설정: replication.factor=3, min.insync.replicas=2

  acks=-1(all) Produce 요청 시:
    ISR 크기 >= min.insync.replicas 일 때만 쓰기 허용

  ISR={B1,B2,B3}: 3 >= 2 → 쓰기 허용
  ISR={B1,B2}:    2 >= 2 → 쓰기 허용
  ISR={B1}:       1 < 2  → NotEnoughReplicasException 반환!

  → 최소 2개 복제본에 데이터가 보장됨
  → 1개 브로커 장애 시에도 데이터 손실 없음
```

```
min.insync.replicas = 2 with replication.factor = 3:

  정상:           B1(L) B2(F) B3(F)   ISR={B1,B2,B3}  쓰기 OK
  B3 장애:        B1(L) B2(F) X       ISR={B1,B2}     쓰기 OK
  B3+B2 장애:     B1(L) X     X       ISR={B1}        쓰기 거부!
  B1 장애 (B3복구): X    B2(L) B3(F)  ISR={B2,B3}     쓰기 OK
```

### 11.3 가용성 vs 일관성 트레이드오프

```
CAP 정리 관점에서의 Kafka 복제:

  +------------------------------------------+
  |           Kafka 복제 설정 스펙트럼         |
  |                                          |
  | 가용성 ◀━━━━━━━━━━━━━━━━━▶ 일관성        |
  |                                          |
  | acks=0         acks=1        acks=-1     |
  | unclean=true                 min.isr=2   |
  |                                          |
  | 데이터 손실     중간          데이터 안전   |
  | 매우 빠름       보통          상대적 느림   |
  +------------------------------------------+
```

---

## 12. 장애 시나리오와 복구

### 12.1 팔로워 장애

```
팔로워 장애 시나리오:

  t0: ISR = {B1(L), B2, B3}
  t1: B3 장애 (네트워크 단절 또는 프로세스 크래시)
  t2: B3의 lastFetchTime이 replica.lag.time.max.ms 초과
  t3: 리더 B1이 maybeShrinkIsr() → ISR = {B1, B2}
  t4: AlterPartition 요청으로 컨트롤러에 알림

  B3 복구 시:
  t5: B3 재시작
  t6: B3가 리더에게 OffsetsForLeaderEpoch 요청
  t7: 필요 시 로그 잘라냄 (truncate)
  t8: B3가 리더에서 Fetch 재개
  t9: B3.LEO >= HW → ISR에 재추가
```

### 12.2 리더 장애

```
리더 장애 시나리오:

  t0: ISR = {B1(L), B2, B3}
  t1: B1 장애 (갑작스러운 크래시)
  t2: 컨트롤러가 B1 장애 감지 (ZooKeeper/KRaft)
  t3: 컨트롤러가 ISR에서 새 리더 선출
      → B2 또는 B3 (ISR 멤버 중 선택)
  t4: 컨트롤러가 LeaderAndIsr 요청 전송
      → B2: makeLeader()
      → B3: makeFollower(newLeader=B2)
  t5: B3가 B2에서 Fetch 시작
  t6: 프로듀서/컨슈머가 메타데이터 갱신하여 B2에 연결

  B1 복구 시:
  t7: B1 재시작
  t8: 컨트롤러가 B1을 팔로워로 할당
  t9: B1이 OffsetsForLeaderEpoch로 잘라내기 판단
  t10: B1이 B2에서 Fetch 재개
  t11: B1.LEO >= HW → ISR에 재추가
```

### 12.3 네트워크 파티션 (Split-Brain 방지)

```
네트워크 파티션 시나리오:

  +--------+     X     +--------+
  | B1(L)  |  -----X---| B2(F)  |
  +--------+     X     +--------+
       |         X         |
       |         X         |
  +--------+     X     +--------+
  | B3(F)  |  -----X---| Ctrl   |
  +--------+     X     +--------+

  B1+B3 네트워크와 B2+Ctrl 네트워크가 분리

  KRaft 기반:
    - 컨트롤러가 B1에 접근 불가 → B1의 리더 펜싱 (fencing)
    - B2를 새 리더로 선출 (ISR에서 선택)
    - B1은 LeaderEpoch가 오래됨 → 프로듀서/컨슈머가 거부
    - B3는 B2에서 Fetch 시작 (B1과도 B2와도 통신 시도)

  → Leader Epoch + Partition Epoch으로 오래된 리더의 쓰기 방지
```

---

## 13. 설계 결정의 이유 (Why)

### 13.1 왜 풀(Pull) 기반 복제인가?

Kafka는 리더가 팔로워에게 데이터를 밀어넣는(Push) 대신, 팔로워가 리더에서
데이터를 가져오는(Pull) 방식을 사용한다.

```
Push 방식:
  리더 → 팔로워1: "새 데이터야, 받아!"
  리더 → 팔로워2: "새 데이터야, 받아!"
  → 리더가 각 팔로워의 속도를 추적해야 함
  → 느린 팔로워가 리더를 블로킹할 수 있음

Pull 방식:
  팔로워1 → 리더: "offset 100부터 데이터 줘"
  팔로워2 → 리더: "offset 98부터 데이터 줘"
  → 각 팔로워가 자신의 속도로 Fetch
  → 리더에 추가 상태 관리 없음
  → 일반 컨슈머와 동일한 코드 경로 재사용!
```

장점:
1. **단순함**: 리더는 일반 Fetch 요청을 처리할 뿐
2. **배치 최적화**: 팔로워가 한 번에 많은 데이터를 가져갈 수 있음
3. **자연스러운 배압**: 느린 팔로워가 리더 성능에 영향 없음
4. **코드 재사용**: 컨슈머 Fetch와 복제 Fetch가 같은 경로

### 13.2 왜 ISR 방식인가?

동기식 복제(synchronous replication)와 비동기식 복제(asynchronous replication)의
중간 지점인 ISR은 Kafka의 핵심 혁신이다.

```
완전 동기 복제:
  장점: 데이터 안전 보장
  단점: 모든 복제본 대기 → 가장 느린 복제본이 전체 성능 결정
        한 복제본 장애 → 전체 멈춤

완전 비동기 복제:
  장점: 빠른 쓰기
  단점: 데이터 손실 가능 (비동기로 아직 안 복제된 데이터)

ISR (동적 동기 집합):
  장점:
    - "빠른" 복제본만 동기화 대상으로 유지
    - 느린 복제본은 자동으로 ISR에서 제거 → 성능 보존
    - ISR 멤버 전체에 복제 완료된 데이터만 커밋
    - 복제본 장애 시 ISR 축소 → 나머지로 계속 운영
  단점:
    - ISR이 1개로 줄면 단일 장애점
    - min.insync.replicas로 완화
```

### 13.3 왜 HW가 Fetch 응답에 포함되는가?

리더가 별도의 HW 업데이트 메시지를 보내는 대신, Fetch 응답에 HW를 포함시킨다.

1. **추가 RPC 없음**: 팔로워는 이미 주기적으로 Fetch하므로 추가 통신 불필요
2. **자연스러운 전파**: Fetch 응답을 처리할 때 HW도 함께 업데이트
3. **지연 최소화**: 별도 채널보다 Fetch 응답이 더 빈번하게 전달됨

하지만 이 방식의 **단점**은 HW 전파에 한 단계의 지연이 있다는 것이다.
이것이 Leader Epoch가 필요한 이유이기도 하다.

### 13.4 왜 컨슈머는 HW까지만 읽는가?

```
HW 이후 데이터를 컨슈머에게 노출하면:

  t0: 리더에 offset 100 기록, HW=99
  t1: 컨슈머가 offset 100 읽음 (HW 이후 데이터)
  t2: 리더 장애, 새 리더는 offset 100이 없음
  t3: 컨슈머는 이미 읽은 offset 100이 "사라짐"
      → 데이터 일관성 위반!

  HW까지만 읽으면:
  t0: 리더에 offset 100 기록, HW=99
  t1: 컨슈머는 offset 99까지만 읽음
  t2: 리더 장애, 새 리더도 HW=99까지는 보유
  t3: 일관성 유지!
```

### 13.5 왜 replica.lag.time.max.ms 기본값이 30초인가?

```
30초의 근거:

  - 네트워크 일시적 장애: 보통 수 초 이내 복구
  - GC 정지: 수 초~수십 초 발생 가능
  - 디스크 I/O 스파이크: 수 초간 지연 가능

  30초는:
    ✓ 일시적 장애에 내성 (불필요한 ISR 축소 방지)
    ✓ 실질적 장애는 빠르게 감지 (30초 이상이면 진짜 문제)
    ✓ 운영자가 대응할 수 있는 시간

  너무 짧으면 (예: 5초):
    → GC 정지마다 ISR 변동 → AlterPartition 폭풍
    → ISR 축소/확장의 메타데이터 변경 비용

  너무 길면 (예: 300초):
    → 죽은 팔로워를 오래 ISR에 유지
    → HW 상승 지연 → 프로듀서 acks=-1 타임아웃
```

---

## 요약 테이블

| 개념 | 설명 | 관련 설정 |
|------|------|----------|
| ISR | 동기화된 복제본 집합 | replica.lag.time.max.ms |
| HW | 모든 ISR에 복제 완료된 오프셋 | - |
| LEO | 로그의 마지막 오프셋 | - |
| LSO | 모든 트랜잭션이 확정된 오프셋 | - |
| Leader Epoch | 리더 변경 추적 카운터 | - |
| Partition Epoch | ISR 변경 추적 카운터 | - |
| FetchIsolation | 읽기 격리 수준 | isolation.level |

| 설정 | 기본값 | 설명 |
|------|--------|------|
| replication.factor | 1 (토픽별) | 복제본 수 |
| min.insync.replicas | 1 | 최소 ISR 크기 |
| replica.lag.time.max.ms | 30000 | ISR 축소 임계값 |
| unclean.leader.election.enable | false | ISR 외부 리더 선출 |
| replica.fetch.max.bytes | 1048576 | 팔로워 Fetch 최대 크기 |
| replica.fetch.wait.max.ms | 500 | 팔로워 Fetch 최대 대기 |
| num.replica.fetchers | 1 | 팔로워 Fetch 스레드 수 |
| replica.high.watermark.checkpoint.interval.ms | 5000 | HW 체크포인트 주기 |

---

## 참고 소스 파일 전체 경로

```
core/src/main/scala/kafka/server/
  +-- ReplicaManager.scala          # 복제 관리자 (핵심)
  +-- ReplicaFetcherThread.scala    # 팔로워 페치 스레드
  +-- ReplicaFetcherManager.scala   # 페치 스레드 관리
  +-- AbstractFetcherThread.scala   # 페치 스레드 기반 클래스

core/src/main/scala/kafka/cluster/
  +-- Partition.scala               # 파티션 상태 (ISR, HW 관리)

server-common/src/main/java/org/apache/kafka/server/storage/log/
  +-- FetchIsolation.java           # 읽기 격리 수준

storage/src/main/java/org/apache/kafka/storage/internals/log/
  +-- UnifiedLog.java               # 로그 읽기/쓰기
  +-- LogOffsetMetadata.java        # 오프셋 메타데이터
  +-- LeaderHwChange.java           # HW 변경 이벤트

storage/src/main/java/org/apache/kafka/storage/internals/checkpoint/
  +-- OffsetCheckpointFile.java     # HW 체크포인트 파일
  +-- LazyOffsetCheckpoints.java    # 지연 로딩 체크포인트

storage/src/main/java/org/apache/kafka/storage/internals/epoch/
  +-- LeaderEpochFileCache.java     # Leader Epoch 캐시
```
