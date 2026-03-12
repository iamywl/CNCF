# PoC-07: 원자적 비교-교환 트랜잭션

## 개요

etcd의 트랜잭션 시스템을 시뮬레이션한다.
Compare 조건 평가, Success/Failure 분기 실행, 분산 잠금 패턴을 재현한다.

## 핵심 개념

| 개념 | 설명 | etcd 소스 |
|------|------|-----------|
| Compare | 키의 value/version/create_rev/mod_rev 조건 | `txn.compareKV()` |
| TxnRequest | IF compare THEN success ELSE failure | `pb.TxnRequest` |
| applyCompares | 모든 조건을 AND로 평가 | `txn.applyCompares()` |
| executeTxn | 조건 결과에 따라 연산 실행 | `txn.executeTxn()` |
| 원자성 | 락 기반으로 비교+실행을 원자적으로 수행 | `kv.Write(trace)` |

## 실행

```bash
go run main.go
```

## 시뮬레이션 시나리오

1. **CAS 성공**: version == "1.0" → "2.0"으로 업데이트
2. **CAS 실패**: version이 이미 "2.0"이므로 "1.0" 조건 불만족
3. **분산 잠금 획득**: create_rev == 0 (키 미존재) → 잠금 생성
4. **분산 잠금 충돌**: 이미 잠금 존재 → 획득 실패
5. **다중 조건**: 여러 Compare AND 조건 + 여러 연산 실행
6. **동시성 카운터**: CAS 재시도 패턴으로 안전한 카운터 증가

## 참조 소스

- `server/etcdserver/txn/txn.go` - 트랜잭션 비교/실행 로직
- `api/etcdserverpb/rpc.proto` - Txn RPC 메시지 정의
