# PoC-21: gRPC OpenTelemetry 통합 시뮬레이션

## 개요

gRPC는 OpenTelemetry와 통합하여 분산 추적과 메트릭스를 자동 수집한다.
이 PoC는 Interceptor 기반 span 생성, W3C trace context 전파, 메트릭스 수집을 시뮬레이션한다.

## 시뮬레이션하는 개념

| 개념 | 실제 gRPC 코드 | 시뮬레이션 |
|------|---------------|-----------|
| Interceptor | `stats/opentelemetry/` | Client/Server span 자동 생성 |
| Context Propagation | W3C traceparent | TraceID/SpanID 전파 |
| Span Kind | CLIENT/SERVER/INTERNAL | RPC 역할별 span |
| Metrics | OTel metrics | RPC 수/지연/바이트 수집 |

## 실행 방법

```bash
cd grpc-go_EDU/poc-21-otel-observability
go run main.go
```
