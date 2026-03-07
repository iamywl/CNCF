# PoC 8: 적응형 샘플링(Adaptive Sampling) 시뮬레이션

## 개요

Jaeger의 **적응형 샘플링(Adaptive Sampling)** 알고리즘을 시뮬레이션한다. 이 알고리즘은 각 서비스/오퍼레이션의 실제 트래픽 패턴을 관찰하여 샘플링 확률을 자동으로 조정하고, `targetSamplesPerSecond`를 유지한다.

## 실제 Jaeger 소스 참조

| 파일 | 핵심 구조체/함수 | 역할 |
|------|-----------------|------|
| `adaptive/post_aggregator.go` | `PostAggregator`, `calculateProbability()` | 확률 계산 메인 로직 |
| `adaptive/post_aggregator.go` | `withinTolerance()`, `throughputToQPS()` | DeltaTolerance, QPS 변환 |
| `adaptive/options.go` | `Options`, `DefaultOptions()` | 설정값 (TargetSPS, DeltaTolerance 등) |
| `adaptive/calculationstrategy/percentage_increase_capped_calculator.go` | `Calculate()` | 증가율 50% 제한 계산기 |
| `adaptive/weightvectorcache.go` | `GetWeights()` | w(i) = i^4 가중치 |

## 핵심 알고리즘

### 확률 계산 공식
```
newProb = prevProb * (targetQPS / curQPS)
```

### 제약 조건

| 제약 | 설명 | 기본값 |
|------|------|--------|
| PercentageIncreaseCap | 확률 증가 시 최대 증가율 | 50% |
| DeltaTolerance | 목표 대비 허용 편차 범위 (조정 생략) | +/-30% |
| MinSamplingProbability | 최소 확률 하한 | 1e-5 (1/100,000) |
| 감소 즉시 적용 | 오버샘플링 방어를 위해 감소는 cap 없이 적용 | - |
| QPS=0 시 2배 | 트래픽 없을 때 확률 2배 증가 | - |

### 비대칭 설계 (핵심)
- **증가**: 50% cap으로 서서히 증가 (과도한 샘플링 방지)
- **감소**: 즉시 적용 (오버샘플링 빠르게 교정)

## 시뮬레이션 시나리오

### 시나리오 1: 일정한 고트래픽
- QPS 500 일정 → 확률이 target/QPS = 0.002로 수렴하는 과정 관찰

### 시나리오 2: 트래픽 스파이크
- QPS 10→1000→10 변화 → 급증 시 즉시 감소, 감소 시 서서히 증가

### 시나리오 3: 점진적 증가
- QPS 50→3200 지수적 증가 → 확률의 단계적 적응 관찰

### 시나리오 4: 간헐적 트래픽
- QPS 0~1 사이 변동 → QPS=0 시 2배 증가 동작 확인

### DeltaTolerance 효과 비교
- DeltaTolerance=0 vs 0.3 → 불필요한 진동 방지 효과

## 실행 방법

```bash
cd poc-adaptive-sampling
go run main.go
```

## WeightVectorCache

가중 평균 QPS 계산 시 최근 버킷에 높은 가중치를 부여:

```
가중치: w(i) = i^4, i = length..1
정규화: sum(weights) = 1.0

예: 3개 버킷 → [0.8889, 0.0988, 0.0123]
                 최신     -1       -2
```

## 핵심 학습 포인트

- 적응형 샘플링의 피드백 제어 루프 구조
- PercentageIncreaseCappedCalculator의 비대칭 설계 이유
- DeltaTolerance로 확률 진동(oscillation) 방지
- 다양한 트래픽 패턴에서의 확률 수렴 과정
- WeightVectorCache의 지수적 가중치(i^4)로 최근 데이터 우선
