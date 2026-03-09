# PoC: Alertmanager Feature Control & Distributed Tracing 시뮬레이션

## 개요

Alertmanager의 기능 플래그 시스템(Feature Control)과
OpenTelemetry 기반 분산 추적(Distributed Tracing)을 시뮬레이션한다.

## 대응하는 Alertmanager 소스코드

| 이 PoC | Alertmanager 소스 | 설명 |
|--------|------------------|------|
| `Flagger` 인터페이스 | `featurecontrol/featurecontrol.go` | 기능 플래그 조회 |
| `Flags` 구조체 | `featurecontrol/featurecontrol.go` | 실제 플래그 구현 |
| `NewFlags()` | `featurecontrol/featurecontrol.go` | 팩토리 |
| `NoopFlagger` | `featurecontrol/featurecontrol.go` | 기본 비활성 |
| `Tracer` | `tracing/tracing.go` | OTel 트레이서 |
| `TracingTransport` | `tracing/http.go` | HTTP 트랜스포트 래핑 |

## 실행 방법

```bash
go run main.go
```

## 핵심 포인트

- --enable-feature 옵션으로 실험적 기능을 런타임에 활성화한다
- 상호 배타적 플래그(classic-mode vs utf8-strict-mode)를 검증한다
- HTTP 트랜스포트를 래핑하여 모든 알림 전송에 자동으로 트레이싱 스팬을 생성한다
