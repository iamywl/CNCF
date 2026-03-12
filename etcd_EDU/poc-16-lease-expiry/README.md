# PoC-16: Lease Expiry (키 만료 알림)

## 개요

etcd의 힙 기반 Lease 만료 스케줄러를 시뮬레이션한다. `container/heap` 최소 힙으로 Lease 만료 시간을 관리하고, 주기적으로 만료된 Lease를 감지하여 알림을 발생시킨다.

## etcd 소스 참조

- `server/lease/lease_queue.go` - LeaseWithTime, LeaseQueue(최소 힙), LeaseExpiredNotifier
- `server/lease/lessor.go` - Lessor 인터페이스, Grant/Renew/Revoke

## 핵심 개념

### LeaseExpiredNotifier
- `LeaseQueue`: `container/heap` 기반 최소 힙 (만료 시간 기준)
- `RegisterOrUpdate()`: 새 Lease는 `heap.Push`, 기존 Lease는 `heap.Fix`
- `Unregister()`: `heap.Pop`으로 만료 Lease 제거
- `Peek()`: O(1)으로 가장 빨리 만료되는 Lease 확인

### 만료 감지 루프
- 주기적으로 `Peek()` → 만료 확인 → `Unregister()` → 콜백
- etcd의 `revokeExpiredLeases()`와 동일한 패턴

### TTL 갱신
- `Renew()` 시 만료 시간 변경 후 `heap.Fix`로 힙 재정렬
- KeepAlive 패턴으로 세션/임시 키 유지

## 실행 방법

```bash
go run main.go
```

## 데모 내용

1. LeaseExpiredNotifier 기본 동작 (힙 정렬)
2. TTL 갱신 시 힙 재정렬
3. Lessor 통합 데모 (생성 → 만료 → 알림)
4. TTL 갱신 (Lease KeepAlive 패턴)
5. 대량 Lease 만료 순서 검증
