# PoC-20: Jaeger ES Metric Store (SPM) 시뮬레이션

## 개요

Jaeger SPM은 ES에 저장된 span에서 RED 메트릭(Rate, Error, Duration)을 집계한다.
이 PoC는 span 인덱싱, 시간 버킷 집계, percentile 계산, ES DSL 생성을 시뮬레이션한다.

## 실행 방법

```bash
cd jaeger_EDU/poc-20-es-metrics
go run main.go
```
