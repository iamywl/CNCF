# PoC-17: gRPC Health Check 시뮬레이션

## 개요

gRPC Health Checking Protocol의 핵심 메커니즘을 Go 표준 라이브러리만으로 시뮬레이션한다.

## 구현 개념

| 개념 | 실제 코드 | 시뮬레이션 |
|------|----------|-----------|
| Health Server | `health/server.go` | HealthServer 구조체 (statusMap + updates) |
| Check RPC | `server.go:60-70` | 동기식 상태 조회 |
| Watch RPC | `server.go:89-132` | 스트리밍 상태 구독 (Fan-out) |
| SetServingStatus | `server.go:136-159` | 상태 변경 + 채널 기반 알림 전파 |
| Shutdown/Resume | `server.go:166-187` | 그레이스풀 생명주기 관리 |
| Client Health Check | `client.go:59-117` | 상태 기반 연결 상태 전이 |

## 핵심 메커니즘

### 버퍼=1 채널 + 논블로킹 전파

```
SetServingStatus() 호출 시:
  select { case <-ch: default: }  // 이전 미소비 상태 폐기
  ch <- newStatus                  // 최신 상태 전송
```

- 서버가 절대 블로킹되지 않음
- 채널에는 항상 최신 상태만 존재
- 클라이언트가 느려도 서버 성능에 영향 없음

### Watch vs Check

- **Check**: 일회성 조회, K8s probe용
- **Watch**: 이벤트 기반 구독, 로드 밸런서 통합용

## 실행

```bash
go run main.go
```

## 시뮬레이션 시나리오

1. **Check RPC** — 서비스별 동기식 상태 조회
2. **Watch RPC** — 2개 클라이언트 동시 구독, Fan-out 확인
3. **SERVICE_UNKNOWN** — 미등록 서비스 Watch (스트림 유지)
4. **Shutdown/Resume** — 그레이스풀 종료 및 복구
5. **로드 밸런서 통합** — 3개 서버, 장애 감지 → 트래픽 분배 → 복구
