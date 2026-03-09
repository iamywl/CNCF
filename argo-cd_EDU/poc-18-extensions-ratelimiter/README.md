# PoC: Argo CD Extensions & Rate Limiter 시뮬레이션

## 개요

Argo CD의 서드파티 UI 확장 프록시(Server Extensions)와
Application Controller 작업 큐 속도 제한(Rate Limiter)을 시뮬레이션한다.

## 대응하는 Argo CD 소스코드

| 이 PoC | Argo CD 소스 | 설명 |
|--------|-------------|------|
| `ExtensionManager` | `server/extension/extension.go` | 확장 관리자 |
| `HandleProxy()` | `extension.go` 내 프록시 핸들러 | RBAC + 프록시 |
| `filterSecurityHeaders()` | `extension.go` 내 헤더 필터 | Authorization/Cookie 제거 |
| `TokenBucket` | `util/ratelimiter/ratelimiter.go` | 토큰 버킷 |
| `ExponentialBackoff` | workqueue의 백오프 래퍼 | 지수 백오프 + 자동 리셋 |
| `AppControllerRateLimiter` | `AppControllerRateLimiterConfig` | 복합 Rate Limiter |

## 구현 내용

### 1. Server Extensions
- 확장 등록, 프록시 라우팅
- RBAC 기반 접근 제어 (프로젝트 단위)
- 보안 헤더 필터링 (Authorization/Cookie 제거)

### 2. Token Bucket
- 초당 토큰 생성률(QPS)과 최대 버킷 크기
- 경과 시간 기반 토큰 보충

### 3. Exponential Backoff
- 실패 시 지수적 대기 시간 증가
- 자동 리셋 (일정 시간 실패 없으면 카운트 초기화)

### 4. 복합 Rate Limiter
- Token Bucket + Exponential Backoff 조합
- 성공 시 토큰 소비, 실패 시 백오프 대기

## 실행 방법

```bash
go run main.go
```

## 핵심 포인트

- Extensions는 RBAC과 인증을 Argo CD가 처리하므로 확장 서비스는 비즈니스 로직에만 집중할 수 있다
- Token Bucket은 burst를 허용하면서 평균 QPS를 제어한다
- 복합 Rate Limiter는 실패하는 앱에 점진적으로 더 긴 대기를 부과하여 리소스 경쟁을 방지한다
