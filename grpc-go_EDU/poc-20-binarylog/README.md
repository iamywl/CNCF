# PoC-20: gRPC Binary Logging 시뮬레이션

## 개요

gRPC Binary Logging은 gRPC 메시지를 바이너리 형식으로 기록하여 디버깅/감사에 활용한다.
이 PoC는 로그 필터, 바이너리 직렬화, 메시지 truncation을 시뮬레이션한다.

## 시뮬레이션하는 개념

| 개념 | 실제 gRPC 코드 | 시뮬레이션 |
|------|---------------|-----------|
| BinaryLog Entry | `binarylog/binarylog.go` | 헤더/메시지/트레일러 기록 |
| Filter | GRPC_BINARY_LOG_FILTER | 메서드 패턴 매칭 |
| Sink | `binarylog/sink.go` | 메모리 싱크 |
| Truncation | 메시지 크기 제한 | 설정 크기로 잘라서 기록 |

## 실행 방법

```bash
cd grpc-go_EDU/poc-20-binarylog
go run main.go
```
