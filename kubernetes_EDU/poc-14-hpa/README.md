# PoC-14: Horizontal Pod Autoscaler (HPA) 시뮬레이션

## 개요

Kubernetes HPA의 핵심 알고리즘인 레플리카 계산, tolerance, stabilization window, scaleUp limit를 시뮬레이션한다.

## 시뮬레이션 대상

| 컴포넌트 | 실제 소스 | 시뮬레이션 |
|----------|----------|-----------|
| ReplicaCalculator | `pkg/controller/podautoscaler/replica_calculator.go` | usageRatio 기반 ceil 계산, tolerance 적용 |
| HorizontalController | `pkg/controller/podautoscaler/horizontal.go` | reconcileAutoscaler 루프, stabilization, scaleUp limit |
| Tolerances | `replica_calculator.go:56` isWithin() | |usageRatio-1.0| ≤ tolerance 판정 |
| Stabilization Window | `horizontal.go` stabilizeRecommendation() | scaleDown: 최근 5분 중 최대값 |
| ScaleUp Limit | `horizontal.go:62-63` scaleUpLimitFactor=2.0 | max(2*current, 4) 상한 |

## 핵심 공식

```
usageRatio = currentMetricValue / targetMetricValue
desiredReplicas = ceil(usageRatio * readyPodCount)

Tolerance (기본 0.1):
  0.9 ≤ usageRatio ≤ 1.1 → 변경 없음 (플래핑 방지)

ScaleUp Limit:
  max(2 * currentReplicas, 4)  → 한 번에 최대 2배

ScaleDown Stabilization (기본 5분):
  추천값 히스토리 중 최대값 선택 → 급격한 축소 방지
```

## 실행

```bash
go run main.go
```

## 데모 항목

1. **기본 스케일업**: CPU 80% → 목표 50% → ceil(1.6*3)=5
2. **Tolerance**: CPU 52% → usageRatio 1.04 → tolerance 이내 → 변경 없음
3. **Stabilization Window**: CPU 급락 시 5분 동안 최대 추천값 유지
4. **ScaleUp Limit**: CPU 250% 과부하 → max(2*2, 4)=4로 제한
5. **Min/Max Bounds**: minReplicas=3, maxReplicas=8 바운드 적용
6. **트래픽 패턴 시뮬레이션**: 급증→감소 시나리오에서 HPA 동작 추적
