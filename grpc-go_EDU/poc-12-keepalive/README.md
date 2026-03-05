# PoC-12: Keepalive 핑/퐁 및 유휴 관리 시뮬레이션

## 개념

gRPC keepalive는 장시간 유지되는 HTTP/2 연결의 상태를 확인하는 메커니즘이다. 중간 로드밸런서나 프록시가 유휴 연결을 끊는 문제를 방지하고, 죽은 연결을 빠르게 감지한다.

### 클라이언트 Keepalive 흐름

```
클라이언트                    서버
    │                          │
    │ (Time 경과, 활동 없음)    │
    │─── PING ────────────────→│
    │                          │
    │←── PONG ────────────────│
    │  (연결 정상)             │
    │                          │
    │ (Time 경과, 활동 없음)    │
    │─── PING ────────────────→│
    │                          │
    │   (Timeout 경과, 응답 없음)
    │   연결 종료               │
```

### 서버 Keepalive 정책

```
연결 생성 ──────────────────────────────────→ 시간
    │                                         │
    │←── MaxConnectionIdle ──→│ GOAWAY (유휴) │
    │                         │               │
    │←──── MaxConnectionAge ────→│ GOAWAY     │
    │                            │←Grace→│ 강제종료
    │                                         │
    │  PING 간격 < MinTime? → GOAWAY (위반)   │
```

### EnforcementPolicy

서버는 클라이언트의 과도한 PING을 감지하여 GOAWAY로 연결을 끊는다.

| 조건 | 결과 |
|------|------|
| PING 간격 < MinTime | GOAWAY 전송 |
| 스트림 없이 PING (PermitWithoutStream=false) | GOAWAY 전송 |

## 실행 방법

```bash
go run main.go
```

## 예상 출력

```
========================================
gRPC Keepalive 시뮬레이션
========================================

[1] 클라이언트 Keepalive — 정상 동작
──────────────────────────────────────
  [conn-1 +0s] 클라이언트 keepalive 시작 (Time=50ms, Timeout=30ms)
  [conn-1 +50ms] → PING 전송
  [conn-1 +55ms] ← PONG 수신 — 연결 정상
  [conn-1 +100ms] → PING 전송
  [conn-1 +105ms] ← PONG 수신 — 연결 정상

[2] 클라이언트 Keepalive — PONG 타임아웃
──────────────────────────────────────────
  [conn-2 +50ms] → PING 전송
  [conn-2 +70ms] PONG 타임아웃 (20ms) — 연결 종료

[4] 서버 MaxConnectionIdle
──────────────────────────
  [conn-4 +80ms] 유휴 시간 초과 — GOAWAY 전송
```

## 관련 소스

| 파일 | 설명 |
|------|------|
| `keepalive/keepalive.go` | ClientParameters, ServerParameters, EnforcementPolicy |
| `internal/transport/http2_client.go` | 클라이언트 keepalive() 루프 |
| `internal/transport/http2_server.go` | 서버 keepalive() + handlePing() |
