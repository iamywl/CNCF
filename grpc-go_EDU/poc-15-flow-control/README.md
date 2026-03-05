# PoC-15: HTTP/2 윈도우 기반 흐름 제어 시뮬레이션

## 개념

HTTP/2 흐름 제어는 수신 측이 처리할 수 있는 속도로만 데이터를 전송하도록 보장한다. gRPC는 이를 연결 레벨과 스트림 레벨 두 계층에서 동시에 적용한다.

### 이중 흐름 제어

```
                 연결 레벨 윈도우 (모든 스트림이 공유)
                 ┌──────────────────────────┐
                 │    초기값: 65535 bytes     │
                 └──────────────────────────┘
                           │
            ┌──────────────┼──────────────┐
            ▼              ▼              ▼
   스트림 #1 윈도우  스트림 #3 윈도우  스트림 #5 윈도우
   ┌──────────┐   ┌──────────┐   ┌──────────┐
   │  65535   │   │  65535   │   │  65535   │
   └──────────┘   └──────────┘   └──────────┘

   전송 조건: 연결 윈도우 >= size AND 스트림 윈도우 >= size
```

### 송수신 흐름

```
송신 측                              수신 측
  │                                    │
  │── DATA (size=N) ──────────────────→│
  │   window -= N                      │  unacked += N
  │                                    │
  │                                    │  if unacked >= threshold:
  │←── WINDOW_UPDATE (delta=M) ───────│     window_update = unacked
  │   window += M                      │     unacked = 0
  │                                    │
  │   window == 0?                     │
  │   → 전송 대기                       │
  │←── WINDOW_UPDATE ─────────────────│
  │   전송 재개                         │
```

### BDP 추정

gRPC는 PING/PONG RTT 측정으로 BDP(Bandwidth Delay Product)를 추정하여 윈도우 크기를 동적으로 조절한다.

```
BDP = Bandwidth x RTT

예) 100 Mbps, RTT=10ms → BDP = 125,000 bytes
→ 윈도우를 125KB로 설정하면 파이프라인 채움
```

## 실행 방법

```bash
go run main.go
```

## 예상 출력

```
========================================
HTTP/2 흐름 제어 시뮬레이션
========================================

[1] HTTP/2 흐름 제어 기본 설정
────────────────────────────────
  초기 윈도우 크기:     65535 bytes (64.0 KB)
  기본 프레임 크기:     16384 bytes (16.0 KB)
  최대 윈도우 크기:     2147483647 bytes (2.0 GB)
  WINDOW_UPDATE 임계값: 16383 bytes

[2] 인바운드 흐름 제어 (수신 측)
─────────────────────────────────
  DATA #1 (10000 bytes): 아직 임계값 미달
  DATA #2 (10000 bytes): → WINDOW_UPDATE 20000 bytes 전송

[5] 윈도우 고갈 시나리오
─────────────────────────
  윈도우 고갈! 잔여=15 bytes, 총 전송=65520 bytes (4 프레임)
```

## 관련 소스

| 파일 | 설명 |
|------|------|
| `internal/transport/flowcontrol.go` | writeQuota, trInFlow 구현 |
| `internal/transport/bdp_estimator.go` | BDP 추정기 |
| `internal/transport/http2_client.go` | 클라이언트 흐름 제어 통합 |
| `internal/transport/http2_server.go` | 서버 흐름 제어 통합 |
