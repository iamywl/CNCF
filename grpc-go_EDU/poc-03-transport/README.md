# PoC-03: HTTP/2 프레임 송수신 시뮬레이션

## 개념

gRPC의 전송 계층인 HTTP/2 프레임 처리를 시뮬레이션한다.

```
핸들러 고루틴들          controlBuffer           loopyWriter          네트워크
┌──────────┐          ┌─────────────┐         ┌──────────┐        ┌──────┐
│ 핸들러 A  │──put()──▶│ wakeupCh    │         │          │        │      │
│ 핸들러 B  │──put()──▶│ itemList    │──get()─▶│ encode() │──write─▶│ conn │
│ keepalive │──put()──▶│ (linked list)│         │ flush()  │        │      │
└──────────┘          └─────────────┘         └──────────┘        └──────┘

HTTP/2 프레임 구조 (9바이트 헤더):
┌────────────┬──────┬───────┬──────────┬─────────────┐
│ Length (3B) │Type  │Flags  │Stream ID │  Payload    │
│            │(1B)  │(1B)   │(4B)      │  (Length B) │
└────────────┴──────┴───────┴──────────┴─────────────┘
```

## 시뮬레이션하는 gRPC 구조

| 구조체/함수 | 실제 위치 | 역할 |
|------------|----------|------|
| `controlBuffer` | `controlbuf.go:307` | 프레임 큐잉 버퍼 (put/get) |
| `loopyWriter` | `controlbuf.go:542` | 비동기 프레임 발신 루프 |
| `itemList` | `controlbuf.go` | 링크드 리스트 기반 큐 |
| Frame 타입 | RFC 7540 | DATA, HEADERS, SETTINGS, PING, GOAWAY, WINDOW_UPDATE |

## 실행 방법

```bash
cd poc-03-transport
go run main.go
```

## 예상 출력

```
=== HTTP/2 프레임 송수신 시뮬레이션 ===

── 1. 프레임 인코딩/디코딩 ──
  인코딩: SETTINGS        stream=0   flags=0x00 payload=6 bytes → 총 15 bytes
  인코딩: HEADERS         stream=1   flags=0x04 payload=21 bytes → 총 30 bytes
  ...
  디코딩: SETTINGS        stream=0   flags=0x00 payload=6 bytes
  ...

── 2. controlBuffer + loopyWriter 패턴 ──
[클라이언트/loopy] 전송: SETTINGS (stream=0, payload=6 bytes)
[클라이언트/loopy] 전송: HEADERS (stream=1, payload=47 bytes)
[클라이언트/loopy] 전송: DATA (stream=1, payload=7 bytes)
...

=== 시뮬레이션 완료 ===
```

## 핵심 포인트

1. **9바이트 프레임 헤더**: HTTP/2 프레임은 3(길이)+1(타입)+1(플래그)+4(스트림ID) 고정 헤더를 가짐
2. **controlBuffer**: 여러 고루틴의 프레임을 안전하게 큐잉하고, loopyWriter가 순차적으로 소비
3. **loopyWriter 패턴**: 단일 고루틴이 네트워크 쓰기를 담당하여 경합을 방지
4. **wakeupCh**: 소비자가 대기 중일 때만 채널로 깨움 (불필요한 깨우기 방지)
