# PoC 03: 로그 파이프라인 (Log Pipeline)

## 개요

Loki Distributor의 로그 수집 → 전처리 → 분배 → 저장 파이프라인을 시뮬레이션한다.
Push API를 통해 로그가 유입되면 유효성 검사, 속도 제한, 링 라우팅, 복제 전송 과정을 거친다.

## 시뮬레이션하는 Loki 컴포넌트

| 컴포넌트 | Loki 실제 위치 | 설명 |
|----------|---------------|------|
| Distributor.Push() | `pkg/distributor/distributor.go` | 로그 수집 진입점 |
| validateEntry() | `pkg/validation/validate.go` | 유효성 검사 |
| RateLimiter | `pkg/distributor/ratestore.go` | 테넌트별 속도 제한 |
| Ring routing | `dskit/ring/ring.go` | Hash Ring 기반 라우팅 |
| Ingester | `pkg/ingester/ingester.go` | 로그 수신/저장 |

## 파이프라인 흐름

```
Push API → Validate → Rate Limit → Hash Ring Route → Replicate
  │           │            │              │              │
  │      라인 크기     Token Bucket    스트림 키 해시    복제 인자만큼
  │      레이블 수     테넌트별 독립    시계 방향 탐색    다른 노드 전송
  │      타임스탬프    바이트/초 제한
  │      미래/과거
  ▼
Ingester-1, Ingester-2, Ingester-3 (replicas)
```

## 시나리오

1. **정상 로그 처리**: 4개의 정상 로그 엔트리가 파이프라인을 통과
2. **유효성 검사 실패**: 라인 초과, 레이블 초과, 오래된 타임스탬프, 미래 타임스탬프
3. **속도 제한**: Token Bucket으로 테넌트별 독립 제한, 버스트 초과 시 거부
4. **대량 처리**: 1000개 로그를 3개 테넌트에 분산, 통계 출력

## 실행 방법

```bash
go run main.go
```

## 학습 포인트

- Loki가 잘못된 로그를 파이프라인 초기에 거부하여 자원을 절약하는 방법
- Token Bucket 알고리즘의 동작 원리 (rate + burst)
- 테넌트별 독립 Rate Limiting의 중요성 (noisy neighbor 방지)
- Consistent Hash Ring을 통한 안정적 스트림 라우팅
- 복제(Replication)를 통한 데이터 내구성 확보
