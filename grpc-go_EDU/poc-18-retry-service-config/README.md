# PoC-18: gRPC Retry 및 Service Config 시뮬레이션

## 개요

gRPC의 Retry 메커니즘과 Service Config 파싱/적용 로직을 Go 표준 라이브러리만으로 시뮬레이션한다.

## 구현 개념

| 개념 | 실제 코드 | 시뮬레이션 |
|------|----------|-----------|
| Service Config 파싱 | `service_config.go:172-269` | JSON → ServiceConfig 파싱 + 검증 |
| MethodConfig 매칭 | `service_config.go:1095-1107` | 정확 매칭 → 서비스 매칭 → 전역 매칭 |
| RetryPolicy | `internal/serviceconfig/serviceconfig.go:157-180` | MaxAttempts, 백오프, 재시도 코드 |
| shouldRetry | `stream.go:685-779` | 9단계 체크 파이프라인 |
| retryThrottler | `clientconn.go:1710-1745` | 토큰 버킷 기반 스로틀링 |
| 지수 백오프 + 지터 | `stream.go:760-766` | Thundering Herd 방지 |
| 서버 푸시백 | `stream.go:718-731` | grpc-retry-pushback-ms 처리 |
| 투명 재시도 | `stream.go:692-703` | 스트림 미생성 시 안전한 재시도 |

## 핵심 메커니즘

### 9단계 재시도 판단 파이프라인

```
1. 기본 체크 (finished/committed/drop)
2. 투명 재시도 (transportStream == nil)
3. 미처리 확인 (firstAttempt && unprocessed)
4. 재시도 비활성화 확인
5. 서버 푸시백 확인
6. 상태 코드 매칭
7. 스로틀 확인
8. 최대 시도 확인
9. 백오프 계산 + 대기
```

### 토큰 버킷 스로틀링

```
실패: tokens -= 1
성공: tokens += tokenRatio
tokens ≤ thresh → 재시도 차단
```

## 실행

```bash
go run main.go
```

## 시뮬레이션 시나리오

1. **Service Config 파싱** — JSON 파싱 + 메서드별 매칭
2. **기본 Retry** — UNAVAILABLE 후 성공
3. **최대 시도 초과** — maxAttempts 도달
4. **비재시도 코드** — INVALID_ARGUMENT는 재시도 불가
5. **Retry Throttling** — 토큰 소진 시 차단
6. **서버 푸시백** — grpc-retry-pushback-ms 처리
7. **지수 백오프 + 지터** — 분포 시각화
8. **투명 재시도** — RetryPolicy 없이도 가능
9. **검증 실패 사례** — 잘못된 Config 검출
