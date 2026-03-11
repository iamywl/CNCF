# PoC-22: Jaeger ES Mapping Template 생성 시뮬레이션

## 개요

Jaeger는 ES에 span/서비스 데이터를 저장할 때 인덱스 매핑 템플릿을 생성한다.
이 PoC는 span/service/dependency 매핑, ILM 정책 생성을 시뮬레이션한다.

## 실행 방법

```bash
cd jaeger_EDU/poc-22-es-mapping
go run main.go
```
