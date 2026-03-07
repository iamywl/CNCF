# PoC-11: Kafka Transaction 2PC 프로토콜 시뮬레이션

## 개요

Kafka의 **트랜잭션(Transaction) 2-Phase Commit 프로토콜**을 시뮬레이션한다. Kafka 트랜잭션은 여러 파티션에 걸친 원자적 쓰기(atomic writes)를 보장하며, `read_committed` 격리 수준에서 어보트된 레코드를 필터링한다.

## 기반 소스코드

| 파일 | 역할 |
|------|------|
| `TransactionCoordinator.scala` | 트랜잭션 코디네이터 핵심 로직 (InitProducerId, EndTxn) |
| `TransactionState.java` | 트랜잭션 상태 열거형 및 유효한 상태 전이 정의 |
| `TransactionMetadata.scala` | 트랜잭션 메타데이터 (PID, epoch, 참여 파티션) |
| `TransactionMarkerChannelManager` | 트랜잭션 마커를 파티션에 기록 |

## 시뮬레이션 내용

### 시나리오 1: 정상 트랜잭션 커밋
- InitProducerId -> BeginTransaction -> AddPartitions -> Produce -> EndTxn(COMMIT)
- 2PC 프로토콜의 Phase 1(PrepareCommit)과 Phase 2(WriteTxnMarkers) 시각화

### 시나리오 2: 트랜잭션 어보트
- 오류 발생 시 EndTxn(ABORT)으로 모든 쓰기 취소
- 어보트 마커가 각 파티션에 기록

### 시나리오 3: read_committed vs read_uncommitted
- `read_uncommitted`: 모든 레코드 반환 (어보트된 것 포함)
- `read_committed`: 커밋된 레코드만 반환

### 시나리오 4: 상태 전이 다이어그램
- TransactionState.VALID_PREVIOUS_STATES 시각화

### 시나리오 5: __transaction_state 로그
- 트랜잭션 코디네이터의 모든 상태 변경 이력

### 시나리오 6: 프로듀서 펜싱
- epoch 증가로 좀비 프로듀서 차단

## 상태 전이

```
Empty -> Ongoing -> PrepareCommit -> CompleteCommit -> Empty
                 -> PrepareAbort  -> CompleteAbort  -> Empty
```

## 실행 방법

```bash
go run main.go
```
