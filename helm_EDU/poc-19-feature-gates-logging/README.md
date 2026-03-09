# PoC-19: Feature Gates & Logging 시뮬레이션

## 개요

Helm의 Feature Gates(`pkg/gates/`)와 Logging 시스템(`internal/logging/`)의 핵심 개념을 시뮬레이션한다.

## 시뮬레이션 항목

| 개념 | 소스 참조 | 시뮬레이션 방법 |
|------|----------|----------------|
| Gate 타입 | `pkg/gates/gates.go` | string 별칭, 환경 변수 기반 활성화 |
| IsEnabled | `pkg/gates/gates.go` | os.Getenv != "" 판별 |
| Gate.Error | `pkg/gates/gates.go` | 자체 서술적 에러 메시지 |
| GateGuard | 패턴 시뮬레이션 | 게이트 활성 시에만 함수 실행 |
| DebugCheckHandler | `internal/logging/logging.go` | slog.Handler 래핑, 동적 디버그 필터링 |
| DebugEnabledFunc | `internal/logging/logging.go` | 로그 시점에 디버그 활성화 확인 |
| NewLogger | `internal/logging/logging.go` | 타임스탬프 제거, 동적 디버그 확인 |
| LogHolder | `internal/logging/logging.go` | atomic.Pointer 기반 스레드-안전 로거 |
| SetLogger/Logger | `internal/logging/logging.go` | lock-free 로거 교체 |
| WithAttrs/WithGroup | `internal/logging/logging.go` | 핸들러 래핑 체인 |

## 실행

```bash
go run main.go
```

## 핵심 출력

- Feature Gates의 환경 변수 기반 활성화/비활성화
- 값이 무엇이든 비어있지 않으면 활성화되는 동작
- 자체 서술적 에러 메시지 (활성화 방법 안내)
- GateGuard 패턴으로 기능 실행 제어
- DebugCheckHandler의 동적 디버그 레벨 on/off
- WithAttrs, WithGroup 래핑 체인
- LogHolder의 atomic.Pointer 기반 핫 스왑
- 동시성 테스트 (10 goroutine 경합 없음)
- Feature Gate + Logging 통합 시나리오
