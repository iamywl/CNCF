# PoC-02: Kafka Partitioning Strategies

## 개요

Kafka의 프로듀서 파티셔닝 전략을 시뮬레이션한다. 키가 있는 레코드는 Murmur2 해시로 파티션을 결정하고, 키가 없는 레코드는 Sticky 파티셔닝(KIP-794)을 사용한다.

## 실제 Kafka 소스 참조

| 클래스 | 경로 | 역할 |
|--------|------|------|
| `BuiltInPartitioner.java` | `clients/src/main/java/.../producer/internals/BuiltInPartitioner.java` | Sticky 파티셔닝, CFT 기반 적응형 분배 |
| `Utils.java` | `clients/src/main/java/.../common/utils/Utils.java` | `murmur2()` 해시 함수, `toPositive()` |

## 시뮬레이션하는 핵심 알고리즘

### 1. Murmur2 해시 파티셔닝
- `Utils.murmur2(key)`: seed=0x9747b28c, m=0x5bd1e995, r=24
- `BuiltInPartitioner.partitionForKey()`: `toPositive(murmur2(key)) % numPartitions`
- 동일 키는 항상 동일 파티션으로 -> 키 기반 메시지 순서 보장

### 2. Round-Robin (Kafka 2.3 이전)
- 키가 없을 때 순차적으로 파티션 분배
- 완벽한 균등 분배이지만 매 레코드마다 파티션 전환 -> 배치 비효율

### 3. Sticky 파티셔닝 (KIP-794)
- `BuiltInPartitioner.updatePartitionInfo()`: `stickyBatchSize`만큼 같은 파티션에 전송
- 배치가 가득 차면 다음 파티션으로 전환 -> 네트워크 배치 효율 극대화
- 전환 조건: `producedBytes >= stickyBatchSize && enableSwitch || producedBytes >= stickyBatchSize * 2`

### 4. Adaptive Sticky (큐 로드 기반)
- `BuiltInPartitioner.updatePartitionLoadStats()`: 파티션 큐 크기에 기반한 가중치 계산
- CFT(Cumulative Frequency Table) 알고리즘:
  1. 큐 크기를 역전 (작은 큐 -> 높은 빈도)
  2. 누적합으로 변환
  3. 균일 분포 난수로 이진 탐색 -> 가중 확률 파티션 선택

## 실행 방법

```bash
go run main.go
```

## 출력 내용

1. Murmur2 해시 파티셔닝: 10,000개 키의 분포 + 동일 키 일관성 확인
2. Round-Robin 파티셔닝: 균등 분배 확인
3. Sticky 파티셔닝: 배치 효율 (전환 횟수 비교)
4. Adaptive Sticky: CFT 구축 과정 + 큐 부하 반영 분포

## CFT 알고리즘 예시

```
큐 크기:   [10, 10, 1, 1, 1, 1]
역전:      [1,  1,  10, 10, 10, 10]
누적합:    [1,  2,  12, 22, 32, 42]

-> random % 42:
   0      -> P0 (확률 1/42 = 2.4%)
   1      -> P1 (확률 1/42 = 2.4%)
   2~11   -> P2 (확률 10/42 = 23.8%)
   12~21  -> P3 (확률 10/42 = 23.8%)
   22~31  -> P4 (확률 10/42 = 23.8%)
   32~41  -> P5 (확률 10/42 = 23.8%)
```
