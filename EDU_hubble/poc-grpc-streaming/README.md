# PoC: gRPC Server Streaming 패턴

> **관련 문서**: [02-ARCHITECTURE.md](../02-ARCHITECTURE.md) - 통신 프로토콜, 데이터 흐름 개요

## 이 PoC가 보여주는 것

Hubble의 `GetFlows` RPC는 **Server Streaming** 방식입니다.
클라이언트가 한 번 요청하면, 서버가 Flow 이벤트를 연속적으로 보내줍니다.

```
Client (hubble observe)     Server (Hubble)
  │                           │
  │── GetFlows Request ──────>│
  │                           │
  │<── Flow #1 ──────────────│  ← eBPF 이벤트 발생
  │<── Flow #2 ──────────────│  ← eBPF 이벤트 발생
  │<── Flow #3 ──────────────│  ← eBPF 이벤트 발생
  │         ...               │
```

## 실행 방법

```bash
cd EDU/poc-grpc-streaming
go run main.go
```

## 예상 출력

```
=== PoC: gRPC Server Streaming 패턴 ===

[Server] 포트 localhost:14245에서 대기 중 (Hubble Server :4245 시뮬레이션)
[Client] 서버에 연결됨 (hubble observe --follow 시뮬레이션)
[Client] Flow 수신 시작...

  09:15:23.456 default/frontend:80 → default/backend:80 [TCP] FORWARDED
  09:15:23.557 default/api-gateway:443 ✗ default/database:3306 [TCP] DROPPED
  ...

[Client] 총 10개 Flow 수신 완료
```

## 실제 Hubble과의 대응

| 이 PoC | 실제 Hubble |
|--------|------------|
| TCP 소켓 | gRPC/TLS (HTTP/2) |
| JSON 인코딩 | Protocol Buffers |
| `handleClient()` | `Observer.GetFlows()` RPC |
| `connectAndObserve()` | `hubble observe --follow` |
| 랜덤 Flow 생성 | eBPF perf ring buffer에서 읽기 |

## 핵심 학습 포인트

1. **Server Streaming vs Unary**: 일반 REST API는 요청-응답 1:1이지만, gRPC streaming은 1:N
2. **왜 Streaming인가**: 네트워크 이벤트는 언제 발생할지 모르므로, polling보다 push가 효율적
3. **연결 유지**: `--follow` 시 연결이 유지되어 실시간 이벤트 수신
