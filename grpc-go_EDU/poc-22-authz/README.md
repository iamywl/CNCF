# PoC-22: gRPC Authorization 정책 엔진 시뮬레이션

## 개요

gRPC authz는 RBAC 기반 인가 정책을 인터셉터로 적용한다.
이 PoC는 DENY/ALLOW 규칙 평가, Principal/Permission 매칭을 시뮬레이션한다.

## 실행 방법

```bash
cd grpc-go_EDU/poc-22-authz
go run main.go
```
