# PoC-01: 클라이언트-서버 기본 RPC 통신

## 개념

gRPC의 핵심 아키텍처를 TCP 기반으로 시뮬레이션한다.

```
클라이언트                              서버
┌──────────┐    TCP 연결    ┌──────────────────────┐
│ Dial()   │───────────────▶│ Accept()             │
│          │                │                      │
│ Invoke() │  RPCRequest    │ services 맵에서       │
│  ├ 직렬화 │───────────────▶│  서비스/메서드 찾기    │
│  │       │                │  → Handler 호출       │
│  └ 역직렬화│◀──────────────│  → RPCResponse 반환   │
└──────────┘   RPCResponse  └──────────────────────┘
```

## 시뮬레이션하는 gRPC 구조

| 구조체/함수 | 실제 위치 | 역할 |
|------------|----------|------|
| `ServiceDesc` | `server.go:105` | 서비스 메타데이터 (이름, 메서드 목록) |
| `MethodDesc` | `server.go:99` | 메서드 이름 + 핸들러 함수 |
| `serviceInfo` | `server.go:117` | 내부 서비스 관리 (구현체 + 메서드 맵) |
| `RegisterService` | `server.go` | 서비스 등록 |
| `Serve` | `server.go` | 연결 수락 및 RPC 처리 루프 |
| `Invoke` | `call.go` | 클라이언트 Unary RPC 호출 |

## 실행 방법

```bash
cd poc-01-architecture
go run main.go
```

## 예상 출력

```
=== gRPC 아키텍처 시뮬레이션 ===

[서버] 서비스 등록: helloworld.Greeter (메서드 2개)
[서버] 127.0.0.1:xxxxx에서 대기 중...
[클라이언트] SayHello 호출...
[클라이언트] 응답: 안녕하세요, gRPC님!
[클라이언트] SayGoodbye 호출...
[클라이언트] 응답: 안녕히 가세요, World님!
[클라이언트] 존재하지 않는 메서드 호출...
[클라이언트] 예상된 에러: RPC 에러: 메서드 'helloworld.Greeter/Unknown'를 찾을 수 없음

=== 시뮬레이션 완료 ===
```

## 핵심 포인트

1. **서비스 등록 패턴**: `ServiceDesc` → `serviceInfo`로 변환하여 메서드 맵 구성
2. **메서드 디스패치**: 서비스명 + 메서드명으로 핸들러를 찾아 호출 (실제 gRPC는 `/package.service/method` 경로 사용)
3. **핸들러 클로저**: protoc-gen-go-grpc가 생성하는 핸들러처럼 서비스 구현체를 타입 단언하여 사용
