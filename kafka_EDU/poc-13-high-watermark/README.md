# PoC-13: Kafka High Watermark (HW) 메커니즘

## 개요

Kafka의 High Watermark 메커니즘을 시뮬레이션한다. HW는 ISR(In-Sync Replica) 내 모든 레플리카가 복제를 완료한 최소 오프셋으로, 컨슈머가 읽을 수 있는 데이터의 상한선을 결정한다.

## 참조 소스코드

| 파일 | 핵심 로직 |
|------|----------|
| `core/src/main/scala/kafka/cluster/Partition.scala` | `maybeIncrementLeaderHW()` - ISR 내 최소 LEO로 HW 계산 |
| `raft/src/main/java/org/apache/kafka/raft/LeaderState.java` | KRaft 리더 상태 관리, 레플리카 상태 추적 |

## 핵심 알고리즘

```
HW = min(LEO of all replicas in ISR)

maybeIncrementLeaderHW():
  newHW = leaderLEO
  for each replica in ISR:
    if replica.LEO < newHW:
      newHW = replica.LEO
  if newHW > currentHW:
    currentHW = newHW
```

## 시뮬레이션 시나리오

| 시나리오 | 설명 |
|---------|------|
| 1. HW 진행 | 팔로워가 순차적으로 fetch하여 HW가 올라가는 과정 |
| 2. 컨슈머 읽기 제한 | 리더 LEO > HW일 때 컨슈머가 HW까지만 읽을 수 있음 |
| 3. ISR 축소 | 지연된 팔로워가 ISR에서 제거되어 HW가 올라감 |
| 4. ISR 확장 | 복귀한 팔로워가 리더를 따라잡아 ISR에 재추가 |
| 5. 점진적 시각화 | 팔로워 속도 차이에 따른 HW 진행 과정 ASCII 시각화 |

## 실행

```bash
go run main.go
```

## 핵심 개념

- **LEO (Log End Offset)**: 각 레플리카의 로그 끝 오프셋 (다음에 쓸 위치)
- **HW (High Watermark)**: ISR 내 모든 LEO의 최솟값, 컨슈머 읽기 상한
- **ISR (In-Sync Replicas)**: 리더와 동기화된 레플리카 집합
- **ISR 축소**: `replica.lag.time.max.ms` 초과 시 팔로워 제거
- **ISR 확장**: 팔로워 LEO >= 리더 LEO이면 재추가
