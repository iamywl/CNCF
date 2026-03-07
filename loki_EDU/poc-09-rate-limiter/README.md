# PoC #09: 레이트 리미터 - 테넌트별 수집 속도 제한

## 개요

Loki의 Distributor는 테넌트별로 수집 속도를 제한하여 하나의 테넌트가 전체 시스템 리소스를 독점하는 것을 방지한다. 이 PoC는 Token Bucket 알고리즘과 Local/Global 전략을 구현한다.

## 실제 Loki 코드와의 관계

| 이 PoC | Loki 실제 코드 |
|--------|---------------|
| `TokenBucket` | `golang.org/x/time/rate.Limiter` 기반 래퍼 |
| `DistributorRateLimiter` | `pkg/distributor/distributor.go` |
| `TenantLimits` | `pkg/validation/limits.go` |
| Local/Global Strategy | `pkg/distributor/rate_strategy.go` |

## 핵심 알고리즘

### Token Bucket
```
시간 →
버킷: [████████░░] (8/10 토큰)
      ↓ 3개 소비
버킷: [█████░░░░░] (5/10 토큰)
      ↓ 시간 경과 (보충)
버킷: [███████░░░] (7/10 토큰)
      ↓ 9개 소비 시도
      → 거부! (7 < 9)
```

### Local vs Global Strategy
```
설정: 한도 10MB/s, Distributor 5대

LOCAL:  각 Distributor가 10MB/s 허용 → 실제 총 50MB/s 가능 (과다)
GLOBAL: 각 Distributor가 2MB/s 허용  → 실제 총 10MB/s 가능 (정확)
```

## 실행 방법

```bash
go run main.go
```

## 시연 내용

1. **Token Bucket 기본 동작**: 토큰 소비, 보충, 버스트 처리
2. **테넌트별 제한**: Premium/Basic 테넌트에 다른 제한 적용
3. **Local vs Global 비교**: 분산 환경에서의 전략 차이
4. **버스트 흡수 패턴**: 간헐적 대량 전송 시나리오
5. **동시 다중 테넌트**: 5개 테넌트 동시 시뮬레이션
6. **Token Bucket 시각화**: ASCII 바 차트로 토큰 상태 표시

## 핵심 설계 포인트

- **시간 기반 보충**: `elapsed * rate`로 경과 시간에 비례하여 토큰 보충
- **버스트 지원**: `capacity`까지 순간 처리 가능, 이후 `rate`로 제한
- **테넌트 격리**: 각 테넌트마다 독립적인 Token Bucket
- **Global 전략**: `rate / numDistributors`로 분산 환경에서 정확한 한도 유지
