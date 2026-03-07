# PoC 15: Waypoint 프록시 L7 라우팅 시뮬레이션

## 개요

Istio Ambient Mesh에서 Waypoint 프록시가 ztunnel로부터 트래픽을 넘겨받아 L7 HTTP 라우팅, Retry, Fault Injection을 수행하는 과정을 시뮬레이션한다.

## Istio 소스코드 참조

| 파일 | 역할 |
|------|------|
| `pilot/pkg/networking/core/waypoint.go` | findWaypointResources(), 내부 리스너 상수 정의 |
| `pilot/pkg/networking/core/route/retry/retry.go` | ConvertPolicy(), DefaultPolicy(), parseRetryOn() |
| `pilot/pkg/networking/core/route/route.go` | VirtualService 라우트 빌드 |
| `pilot/pkg/xds/endpoints/endpoint_builder.go` | findServiceWaypoint(), waypoint 엔드포인트 라우팅 |

## Waypoint 프록시 아키텍처

### 내부 리스너 체인
```
connect_terminate -> main_internal -> connect_originate
  (HBONE 종료)        (L7 처리)        (HBONE 생성)
```

### ztunnel과 Waypoint의 역할 분담
- **ztunnel** (L4): 서비스에 waypoint이 있으면 HBONE 터널로 waypoint에 전달
- **waypoint** (L7): HTTP 라우팅, 인가 정책, fault injection, retry/timeout 처리

### 기본 Retry 정책 (Istio 기본값)
```
NumRetries: 2
RetryOn: "connect-failure,refused-stream,unavailable,cancelled,retriable-status-codes"
HostSelectionRetryMaxAttempts: 5
RetryHostPredicate: [previous_hosts]  (이전 실패 호스트 회피)
```

## 시뮬레이션 시나리오

1. **기본 요청 흐름**: ztunnel -> HBONE -> waypoint -> 라우트 매칭 -> 엔드포인트
2. **헤더 기반 카나리 라우팅**: `x-canary: true` 헤더로 카나리 엔드포인트 선택
3. **Fault Injection - Abort**: 100% 확률로 503 abort 주입
4. **Fault Injection - Delay**: 50% 확률로 5초 지연 주입
5. **Retry 동작**: 70% 실패율 엔드포인트에 3회 재시도, 이전 호스트 회피

## 실행

```bash
go run main.go
```
