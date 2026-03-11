# PoC-21: Jaeger → OTel 환경변수 마이그레이션 시뮬레이션

## 개요

Jaeger v2의 OTel Collector 기반 전환에 따라 JAEGER_* 환경변수를 OTEL_*로 마이그레이션한다.
이 PoC는 매핑 규칙, 값 변환, 폐기 감지를 시뮬레이션한다.

## 실행 방법

```bash
cd jaeger_EDU/poc-21-env2otel
go run main.go
```
