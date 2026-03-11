# PoC-23: gRPC ORCA 로드 리포팅 시뮬레이션

## 개요

ORCA는 백엔드 서버가 자신의 부하 상태를 클라이언트에 보고하는 표준이다.
이 PoC는 Per-Query/OOB 리포팅과 가중 라운드로빈 로드밸런싱을 시뮬레이션한다.

## 실행 방법

```bash
cd grpc-go_EDU/poc-23-orca
go run main.go
```
