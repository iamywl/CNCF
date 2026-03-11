# PoC-18: Jaeger Expvar Extension 시뮬레이션

## 개요

Jaeger는 expvar를 확장하여 런타임/커스텀 메트릭스를 HTTP 엔드포인트로 노출한다.
이 PoC는 expvar 레지스트리, 런타임 메트릭스 수집, JSON 직렬화를 시뮬레이션한다.

## 실행 방법

```bash
cd jaeger_EDU/poc-18-expvar
go run main.go
```
