# PoC: 서킷 브레이커와 아웃라이어 디텍션

## 개요

Istio는 `DestinationRule`의 `TrafficPolicy`를 통해 서킷 브레이커와 아웃라이어 디텍션을 설정한다. Pilot(istiod)은 이 설정을 Envoy가 이해하는 `CircuitBreakers`와 `OutlierDetection` 클러스터 설정으로 변환하여 xDS로 사이드카에 전달한다.

이 PoC는 그 핵심 동작을 Go 표준 라이브러리만으로 시뮬레이션한다.

## 시뮬레이션 대상

### 1. 커넥션 풀 (ConnectionPoolSettings)

Istio 소스의 `applyConnectionPool()` 함수가 Envoy `CircuitBreakers.Thresholds`로 변환하는 과정을 재현한다.

| 설정 | Istio 필드 | Envoy 필드 | 설명 |
|------|-----------|-----------|------|
| TCP 최대 커넥션 | `tcp.maxConnections` | `MaxConnections` | 업스트림으로의 최대 TCP 연결 수 |
| HTTP 최대 대기 요청 | `http.http1MaxPendingRequests` | `MaxPendingRequests` | 커넥션 풀 대기열 크기 |
| 커넥션당 최대 요청 | `http.maxRequestsPerConnection` | `MaxRequestsPerConnection` | 하나의 커넥션에서 처리할 최대 요청 수 |

**기본값**: Istio는 `getDefaultCircuitBreakerThresholds()`에서 모든 값을 `MaxUint32`로 설정한다 (실질적으로 무제한).

### 2. 아웃라이어 디텍션 (OutlierDetection)

Istio 소스의 `applyOutlierDetection()` 함수가 Envoy `OutlierDetection` 설정으로 변환하는 과정을 재현한다.

| 설정 | 설명 |
|------|------|
| `consecutive5xxErrors` | 연속 5xx 에러 임계값 (이 횟수 도달 시 퇴출) |
| `interval` | 아웃라이어 분석 주기 |
| `baseEjectionTime` | 기본 퇴출 시간 (퇴출 횟수에 비례하여 증가) |
| `maxEjectionPercent` | 최대 퇴출 비율 (최소 가용성 보장) |

### 3. 상태 머신

```
CLOSED (정상) ──연속 에러 ≥ N──→ OPEN (차단)
   ^                                │
   │                    baseEjectionTime 경과
   │                                │
   │                                v
   └───시험 요청 성공──── HALF-OPEN (반개방)
                            │
                            └──시험 요청 실패──→ OPEN
```

- **퇴출 시간 점진적 증가**: `baseEjectionTime * ejectionCount`
- **최대 퇴출 비율 보호**: `maxEjectionPercent` 이상 퇴출하지 않아 최소 가용성 보장

## 실행 방법

```bash
cd istio_EDU/poc-circuit-breaker
go run main.go
```

## 예상 출력

4개 시나리오가 순차적으로 실행된다:

1. **커넥션 풀 오버플로우**: 동시 요청 10개 중 `maxConnections(3) + maxPendingRequests(2) = 5`개만 수용, 나머지 503 반환
2. **아웃라이어 디텍션**: 연속 5xx 3회 도달한 pod-3이 퇴출되고, `baseEjectionTime(1s)` 후 복구
3. **최대 퇴출 비율 보호**: 3개 불량 엔드포인트 중 `maxEjectionPercent=50%`로 최대 2개만 퇴출
4. **통합 시뮬레이션**: 커넥션 풀 + 아웃라이어 디텍션이 함께 동작하며 불량 엔드포인트 자동 퇴출/복구

## Istio 소스 참조

| 파일 | 함수/구조체 | 역할 |
|------|-----------|------|
| `pilot/pkg/networking/core/cluster_traffic_policy.go` | `applyTrafficPolicy()` | TrafficPolicy를 Envoy 클러스터에 적용 |
| `pilot/pkg/networking/core/cluster_traffic_policy.go` | `applyConnectionPool()` | ConnectionPoolSettings → Envoy CircuitBreakers |
| `pilot/pkg/networking/core/cluster_traffic_policy.go` | `applyOutlierDetection()` | OutlierDetection → Envoy OutlierDetection |
| `pilot/pkg/networking/core/cluster_traffic_policy.go` | `getDefaultCircuitBreakerThresholds()` | 기본 임계값 (MaxUint32) |
| `pilot/pkg/networking/core/cluster_traffic_policy.go` | `selectTrafficPolicyComponents()` | TrafficPolicy에서 각 컴포넌트 추출 |

## DestinationRule YAML 예시

```yaml
apiVersion: networking.istio.io/v1beta1
kind: DestinationRule
metadata:
  name: reviews-circuit-breaker
spec:
  host: reviews.default.svc.cluster.local
  trafficPolicy:
    connectionPool:
      tcp:
        maxConnections: 100
      http:
        http1MaxPendingRequests: 50
        maxRequestsPerConnection: 10
    outlierDetection:
      consecutive5xxErrors: 5
      interval: 10s
      baseEjectionTime: 30s
      maxEjectionPercent: 50
```
