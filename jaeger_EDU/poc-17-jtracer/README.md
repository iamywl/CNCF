# PoC-17: Jaeger Self-Tracing (jtracer) 시뮬레이션

## 개요

Jaeger는 자기 자신도 추적하여 내부 동작을 관측한다. 이 PoC는 OTel SDK 부트스트랩,
self-tracing span 생성, 배치 익스포트를 시뮬레이션한다.

## 실행 방법

```bash
cd jaeger_EDU/poc-17-jtracer
go run main.go
```
