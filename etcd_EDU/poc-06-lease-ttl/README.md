# PoC-06: Lease TTL 기반 키 자동 만료

## 개요

etcd의 Lease 시스템을 시뮬레이션한다.
TTL 기반 키 자동 만료, KeepAlive, Revoke 동작을 재현한다.

## 핵심 개념

| 개념 | 설명 | etcd 소스 |
|------|------|-----------|
| Lease | TTL 기반 임대 계약 (ID, TTL, 만료시각, 연결키) | `server/lease/lease.go` |
| Lessor | Lease 생명주기 관리자 | `server/lease/lessor.go` |
| Grant | 새 Lease 생성 | `lessor.Grant()` |
| Revoke | Lease 폐기 + 연결 키 삭제 | `lessor.Revoke()` |
| Renew | TTL 갱신 (KeepAlive 요청 시) | `lessor.Renew()` |
| Attach | 키를 Lease에 연결 | `lessor.Attach()` |
| 만료 감지 | 고루틴이 주기적으로 만료 Lease 탐색 | `lessor.revokeExpiredLeases()` |

## 실행

```bash
go run main.go
```

## 시뮬레이션 시나리오

1. **Lease 생성**: 2초 TTL Lease와 5초 TTL Lease 생성
2. **키 연결**: 서비스 등록 키를 Lease에 연결
3. **KeepAlive**: Lease 2만 주기적 갱신 → Lease 1은 2초 후 만료
4. **KeepAlive 중단**: 갱신 중단 → Lease 2도 만료 → 키 삭제
5. **수동 Revoke**: 장기 TTL Lease를 수동으로 폐기

## 참조 소스

- `server/lease/lease.go` - Lease 구조체, TTL/만료 관리
- `server/lease/lessor.go` - Lessor 인터페이스, 만료 감지 루프
- `server/lease/lease_queue.go` - 만료 순서 우선순위 큐
