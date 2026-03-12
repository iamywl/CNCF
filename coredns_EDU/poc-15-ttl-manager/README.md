# PoC 15: TTL 관리

## 개요

CoreDNS cache 플러그인의 TTL(Time To Live) 처리 메커니즘을 시뮬레이션한다.
TTL 클램핑, TTL 감소, SERVFAIL 캐싱, MinimalTTL 함수의 동작을 구현한다.

## 실제 CoreDNS 코드 참조

| 파일 | 역할 |
|------|------|
| `plugin/pkg/dnsutil/ttl.go` | `MinimalTTL()`, `MinimalDefaultTTL`, `MaximumDefaultTTL` 상수 |
| `plugin/cache/cache.go` | Cache 구조체 (`pttl`, `minpttl`, `failttl` 등) |
| `plugin/cache/handler.go` | TTL 감소 로직, 캐시 히트/미스 처리 |

## 핵심 동작

1. **TTL 클램핑**: `MinTTL(5s) ≤ TTL ≤ MaxTTL(1h)` 범위로 제한
2. **TTL 감소**: 캐시 응답 시 `남은 TTL = 원래 TTL - 경과 시간`
3. **SERVFAIL 캐시**: 짧은 TTL(5s)로 부정적 응답을 캐싱하여 업스트림 부하 감소
4. **MinimalTTL**: 응답 내 모든 RR의 최소 TTL을 캐시 만료 시간으로 사용

## 실행

```bash
go run main.go
```

## 데모 시나리오

1. **TTL 클램핑**: 너무 짧은/긴 TTL이 min/max 범위로 조정되는 과정
2. **TTL 감소**: 10초 TTL 레코드가 시간 경과에 따라 감소 → 만료
3. **SERVFAIL 캐시**: 실패 응답의 짧은 TTL 캐싱과 만료
4. **캐시 수명 주기**: 다양한 TTL의 레코드가 순서대로 만료되는 과정
5. **MinimalTTL 함수**: 여러 RR에서 최소 TTL을 찾는 로직
