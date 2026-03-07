# PoC-09: Producer RecordAccumulator 배칭 시뮬레이션

## 개요

Kafka 프로듀서의 핵심 메커니즘인 **RecordAccumulator 배칭**을 시뮬레이션한다. Kafka 프로듀서는 레코드를 즉시 전송하지 않고, 파티션별 배치에 축적(accumulate)한 뒤 배치 크기 임계치 도달 또는 linger.ms 타임아웃 중 먼저 충족되는 조건에 의해 전송한다.

## 기반 소스코드

| 파일 | 역할 |
|------|------|
| `RecordAccumulator.java` | 파티션별 `Deque<ProducerBatch>` 관리, append/ready/drain 로직 |
| `Sender.java` | 백그라운드 스레드에서 ready -> drain -> send 루프 실행 |
| `BufferPool.java` | `buffer.memory` 설정에 따른 메모리 풀 관리 |
| `ProducerBatch.java` | 단일 배치 내 레코드 축적 |

## 시뮬레이션 내용

### 시나리오 1: batch.size 트리거
- 큰 레코드를 빠르게 전송하여 배치 크기가 먼저 충족되는 경우
- `linger.ms`를 기다리지 않고 즉시 전송

### 시나리오 2: linger.ms 트리거
- 작은 레코드를 소량 전송하여 배치가 차지 않는 경우
- `linger.ms` 타임아웃 후 전송

### 시나리오 3: BufferPool 메모리 제한
- `buffer.memory` 한도에 도달하면 `producer.send()`가 블로킹
- 기존 배치 전송 완료 후 메모리 반환 시 진행

### 시나리오 4: 처리량 vs 지연 트레이드오프
- `linger.ms=0, 10, 100`으로 동일 레코드를 전송하여 배치 크기, 지연, 처리량 비교

## 핵심 알고리즘

```
append(partition, record):
  1. Deque<ProducerBatch>[partition]에서 마지막 배치 확인
  2. tryAppend() 시도 -> 성공하면 반환
  3. 실패하면 BufferPool.allocate() -> 새 배치 생성

Sender 루프:
  1. ready(): 각 파티션의 첫 배치 검사
     - batchFull || dequeSize > 1 -> 즉시 전송
     - waitedTime >= lingerMs    -> 즉시 전송
  2. drain(): ready 파티션에서 배치 꺼내기
  3. send(): 브로커에 전송 후 BufferPool 반환
```

## 실행 방법

```bash
go run main.go
```
