# PoC 5: 인증서 자동 갱신 (Certificate Rotation)

## 개요

Istio의 `SecretManagerClient`가 워크로드 인증서를 자동으로 갱신하는 메커니즘을 시뮬레이션합니다.
실제 코드 경로: `istio/security/pkg/nodeagent/cache/secretcache.go`

## 핵심 알고리즘

### rotateTime() - 갱신 시점 계산

```
jitter = (rand() * graceRatioJitter) * (-1 또는 +1)
jitterGraceRatio = graceRatio + jitter  (범위: [0, 1])
secretLifeTime = expireTime - createdTime
gracePeriod = jitterGraceRatio * secretLifeTime
delay = expireTime - gracePeriod - now
```

- **gracePeriodRatio (기본 0.5)**: 인증서 수명의 50%를 grace period로 사용
- **gracePeriodRatioJitter (기본 0.01)**: ±1%의 지터로 대규모 플릿의 동시 갱신 방지

### registerSecret() - 갱신 스케줄링

1. `rotateTime()`으로 대기 시간 계산
2. 인증서를 `secretCache`에 저장
3. `DelayedQueue`에 갱신 작업 등록
4. 갱신 시: 캐시 초기화 → `OnSecretUpdate()` → SDS push

## 시뮬레이션 내용

1. **rotateTime 알고리즘 분석**: 지터에 의한 갱신 시점 변동 확인
2. **단일 워크로드 갱신 사이클**: 인증서 발급 → 갱신 → SDS push 전체 흐름
3. **다중 워크로드 독립 갱신**: 서로 다른 TTL의 워크로드들이 독립적으로 갱신
4. **타임라인 요약**: 전체 갱신 프로세스 시각화

## 실행

```bash
go run main.go
```

## 주요 참조 코드

| 함수 | 파일 위치 | 설명 |
|------|----------|------|
| `SecretManagerClient` | `security/pkg/nodeagent/cache/secretcache.go:83` | 인증서 관리 클라이언트 구조체 |
| `rotateTime()` | `security/pkg/nodeagent/cache/secretcache.go:858` | 갱신 시점 계산 핵심 알고리즘 |
| `registerSecret()` | `security/pkg/nodeagent/cache/secretcache.go:877` | 캐시 저장 및 갱신 스케줄링 |
| `GenerateSecret()` | `security/pkg/nodeagent/cache/secretcache.go:248` | SDS 인증서 요청 처리 |
| `OnSecretUpdate()` | `security/pkg/nodeagent/cache/secretcache.go:210` | SDS push 콜백 트리거 |
