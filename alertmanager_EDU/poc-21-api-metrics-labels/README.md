# PoC: Alertmanager API Metrics, Labels, Types 시뮬레이션

## 개요

Alertmanager의 API 메트릭 수집, pkg/labels 레이블 매칭 라이브러리,
핵심 타입 시스템(Alert, AlertStatus, Marker)을 시뮬레이션한다.

## 대응하는 Alertmanager 소스코드

| 이 PoC | Alertmanager 소스 | 설명 |
|--------|------------------|------|
| `AlertMetrics` | `api/metrics/metrics.go` | 알림 수신 카운터 |
| `Matcher` | `pkg/labels/matcher.go` | 레이블 매처 (=, !=, =~, !~) |
| `ParseMatcher()` | `pkg/labels/parse.go` | 매처 문자열 파싱 |
| `Alert` | `types/types.go` | 알림 핵심 타입 |
| `AlertStatus` | `types/types.go` | 알림 처리 상태 |
| `Marker` | `alert/alert.go` | 알림 상태 추적 인터페이스 |

## 실행 방법

```bash
go run main.go
```

## 핵심 포인트

- Matcher는 4가지 연산자(=, !=, =~, !~)로 레이블 값을 필터링한다
- 여러 Matcher를 AND 조합하여 복합 필터를 구성한다
- Marker는 알림의 Active/Inhibited/Silenced 상태를 추적한다
