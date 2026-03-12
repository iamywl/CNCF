# PoC-13: BatchTx (배치 트랜잭션)

## 개요

etcd의 BatchTx 메커니즘을 시뮬레이션한다. etcd는 BoltDB 위에 배치 트랜잭션 레이어를 두어 여러 쓰기를 버퍼링하고, 일정 조건에서 한 번에 커밋함으로써 I/O 성능을 최적화한다.

## etcd 소스 참조

- `server/storage/backend/batch_tx.go` - BatchTx 인터페이스 및 batchTx 구현
- `server/storage/backend/backend.go` - batchLimit(10000), batchInterval(100ms) 상수

## 핵심 개념

### 배치 커밋 트리거 조건
1. **batchLimit 초과**: `Unlock()` 시 pending >= batchLimit이면 자동 커밋
2. **batchInterval 타이머**: 100ms마다 주기적으로 강제 커밋
3. **명시적 Commit()**: ForceCommit 등

### ReadTx와의 관계
- ReadTx는 커밋된 데이터(저장소)와 미커밋 데이터(버퍼/캐시) 모두 읽기 가능
- etcd의 ConcurrentReadTx는 txReadBuffer와 readBuffer를 merge

## 실행 방법

```bash
go run main.go
```

## 데모 내용

1. 배치 커밋 vs 즉시 커밋 성능 비교 (1000개 쓰기)
2. batchLimit 초과 시 자동 커밋
3. ReadTx로 미커밋 데이터 읽기
4. batchInterval 주기적 커밋
5. 파일 영속화 및 복원
6. 동시 쓰기와 배치 커밋
