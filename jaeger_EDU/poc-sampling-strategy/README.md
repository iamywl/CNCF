# PoC 7: 샘플링 전략(Sampling Strategy) 시뮬레이션

## 개요

Jaeger의 **샘플링 전략(Sampling Strategy)** 시스템을 시뮬레이션한다. 대규모 분산 시스템에서 모든 트레이스를 저장하면 비용이 과다해지므로, 샘플링을 통해 의미 있는 트레이스만 선별적으로 수집한다.

## 실제 Jaeger 소스 참조

| 파일 | 핵심 구조체 | 역할 |
|------|-----------|------|
| `internal/sampling/samplingstrategy/file/strategy.go` | `strategy`, `operationStrategy`, `serviceStrategy` | 샘플링 전략 정의 |
| `internal/sampling/samplingstrategy/file/provider.go` | `Provider` | 파일 기반 전략 제공자 |
| `internal/sampling/samplingstrategy/adaptive/` | `PostAggregator` | 적응형 샘플링 (PoC 8에서 다룸) |

## 세 가지 기본 전략

### 1. Always-On (const=1)
모든 트레이스를 샘플링. 개발/테스트 환경에 적합.

### 2. Probabilistic (확률적)
TraceID를 경계값과 비교하여 결정론적으로 샘플링:
```
samplingBoundary = MaxUint64 * samplingRate
sampled = (traceID <= samplingBoundary)
```

**결정론적 특성**: 동일한 traceID는 항상 같은 결과. 분산 환경에서 모든 서비스가 독립적으로 동일한 샘플링 결정 가능.

### 3. Rate-Limiting (토큰 버킷)
초당 최대 N개 트레이스만 허용. 토큰 버킷(Token Bucket) 알고리즘 사용:
```
- 초당 N개 토큰 보충
- 요청 시 토큰 1개 소비 → 샘플링
- 토큰 없으면 드롭
```

### 4. Per-Operation 전략
오퍼레이션별로 다른 샘플링률 적용:
```json
{
  "service": "api-gateway",
  "default": "probabilistic(0.01)",
  "operations": {
    "GET /health": "probabilistic(0.001)",
    "POST /orders": "probabilistic(0.50)"
  }
}
```

## 시뮬레이션 내용

1. 세 가지 기본 전략으로 100,000개 트레이스 처리 비교
2. 토큰 버킷의 버스트 대응 동작 확인
3. 결정론적 샘플링의 일관성 검증
4. Per-Operation 전략으로 오퍼레이션별 차등 샘플링
5. 비용 절감 효과 시각화

## 실행 방법

```bash
cd poc-sampling-strategy
go run main.go
```

## 핵심 학습 포인트

- 확률적 샘플러의 결정론적 특성이 분산 환경에서 중요한 이유
- 토큰 버킷 알고리즘의 버스트 트래픽 대응 메커니즘
- 오퍼레이션별 차등 샘플링으로 중요한 트레이스 우선 수집
- 샘플링률에 따른 비용 절감 효과 (100% → 0.1%로 약 1000배 절감)
