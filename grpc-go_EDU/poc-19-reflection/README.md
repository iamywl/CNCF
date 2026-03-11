# PoC-19: gRPC Reflection 서비스 시뮬레이션

## 개요

gRPC Reflection은 서버가 자신의 서비스/메서드 정보를 동적으로 노출하는 기능이다.
이 PoC는 서비스 목록, 메서드 설명, 심볼 해석 등을 시뮬레이션한다.

## 시뮬레이션하는 개념

| 개념 | 실제 gRPC 코드 | 시뮬레이션 |
|------|---------------|-----------|
| ListServices | `reflection/serverreflection.go` | 등록 서비스 열거 |
| FileDescriptor | protobuf 파일 디스크립터 | 스키마 정보 저장 |
| Symbol Resolution | 심볼 → 파일 매핑 | 이름으로 서비스/메시지 조회 |

## 실행 방법

```bash
cd grpc-go_EDU/poc-19-reflection
go run main.go
```
