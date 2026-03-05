# 15. gRPC-Go Keepalive 심화

## 개요

gRPC Keepalive는 **유휴 커넥션의 활성 상태를 확인**하고, **연결 수명을 관리**하는 메커니즘이다.
HTTP/2 PING 프레임을 활용하여 커넥션이 여전히 유효한지 검사하고,
너무 오래된 커넥션은 GOAWAY로 정리한다.

**소스코드**:
- `keepalive/keepalive.go` — 파라미터 정의
- `internal/transport/http2_server.go` — 서버 keepalive 루프
- `internal/transport/http2_client.go` — 클라이언트 keepalive 루프

---

## 1. 파라미터 구조체

### ClientParameters (`keepalive/keepalive.go:33`)

```go
type ClientParameters struct {
    // 활동 없을 때 핑 전송 간격.
    // 10초 미만이면 10초로 강제 설정됨.
    // 서버 EnforcementPolicy.MinTime(기본 5분)과 조율 필요.
    Time time.Duration

    // 핑 전송 후 응답 대기 시간. 초과 시 연결 종료.
    // 기본값: 20초
    Timeout time.Duration

    // true: 활성 RPC 없어도 핑 전송
    // false: 활성 RPC 없으면 Time/Timeout 무시
    PermitWithoutStream bool
}
```

### ServerParameters (`keepalive/keepalive.go:64`)

```go
type ServerParameters struct {
    // 유휴 커넥션 종료 시간 (활성 RPC 0개 이후).
    // 기본값: 무한대
    MaxConnectionIdle time.Duration

    // 커넥션 최대 수명. ±10% 지터 추가.
    // 기본값: 무한대
    MaxConnectionAge time.Duration

    // MaxConnectionAge 이후 추가 유예 기간.
    // 진행 중인 RPC 완료 대기용.
    // 기본값: 무한대
    MaxConnectionAgeGrace time.Duration

    // 활동 없을 때 핑 전송 간격.
    // 1초 미만이면 1초로 강제 설정됨.
    // 기본값: 2시간
    Time time.Duration

    // 핑 전송 후 응답 대기 시간. 초과 시 연결 종료.
    // 기본값: 20초
    Timeout time.Duration
}
```

### EnforcementPolicy (`keepalive/keepalive.go:91`)

```go
type EnforcementPolicy struct {
    // 클라이언트 핑 최소 간격.
    // 이보다 자주 핑하면 GOAWAY(ENHANCE_YOUR_CALM) 전송.
    // 기본값: 5분
    MinTime time.Duration

    // true: 활성 스트림 없어도 클라이언트 핑 허용
    // false: 스트림 없이 핑하면 GOAWAY 전송
    PermitWithoutStream bool
}
```

---

## 2. 서버 Keepalive 메커니즘

### keepalive 루프 (`internal/transport/http2_server.go`)

서버의 keepalive는 `http2Server`의 별도 goroutine에서 실행된다.

```
서버 keepalive goroutine
    │
    └── 무한 루프
        │
        ├── case: MaxConnectionIdle 타이머
        │   └── 활성 스트림 0개 + 유휴 시간 초과
        │       → GOAWAY 전송 → 연결 종료
        │
        ├── case: MaxConnectionAge 타이머
        │   └── 커넥션 수명 초과
        │       → drain = true
        │       → GOAWAY 전송
        │       → MaxConnectionAgeGrace 대기
        │       → 강제 종료
        │
        ├── case: keepalive Time 타이머
        │   └── 마지막 활동 이후 Time 경과
        │       → PING 프레임 전송
        │       → Timeout 대기
        │       → 응답 없으면 연결 종료
        │
        └── case: 클라이언트 PING 주파수 감시
            └── MinTime 미만 간격으로 핑
                → pingStrikes++
                → pingStrikes > maxPingStrikes
                → GOAWAY(ENHANCE_YOUR_CALM)
```

### MaxConnectionAge + Jitter

```go
// http2_server.go 내부
maxAge := kp.MaxConnectionAge
// ±10% 지터 추가
maxAge += time.Duration(rand.Int63n(int64(maxAge) / 5))
```

**왜 지터를 추가하는가?**

모든 클라이언트가 동시에 재연결하는 "thundering herd" 문제를 방지한다.
예를 들어 MaxConnectionAge가 30분이면, 실제 수명은 27~33분 사이에서
랜덤으로 결정되어 재연결 시점이 분산된다.

### MaxConnectionAge + Grace 흐름

```
커넥션 시작
    │
    │ ← MaxConnectionAge (30분 + 지터)
    │
    ▼
GOAWAY 전송 (drain 모드)
    │ 새 스트림 거부
    │ 기존 스트림은 계속 처리
    │
    │ ← MaxConnectionAgeGrace (5초)
    │
    ▼
강제 연결 종료
    모든 스트림에 에러 전파
```

### 핑 스트라이크 (ENHANCE_YOUR_CALM)

```
클라이언트 PING 수신
    │
    ├── 마지막 핑으로부터 MinTime 이상 경과?
    │   ├── 예 → pingStrikes = 0, 정상 처리
    │   └── 아니오 → pingStrikes++
    │
    ├── 활성 스트림 없고 PermitWithoutStream == false?
    │   └── pingStrikes++
    │
    └── pingStrikes > maxPingStrikes (2)?
        → GOAWAY(ENHANCE_YOUR_CALM) 전송
        → 연결 종료
```

---

## 3. 클라이언트 Keepalive 메커니즘

### keepalive 루프 (`internal/transport/http2_client.go`)

```
클라이언트 keepalive goroutine
    │
    └── 무한 루프
        │
        ├── Time 타이머 대기
        │
        ├── 마지막 활동 확인
        │   ├── Time 이내에 활동 있었음 → 타이머 리셋, 계속
        │   └── 활동 없음 → 핑 전송
        │
        ├── PermitWithoutStream 확인
        │   ├── 활성 스트림 있음 → 핑 진행
        │   ├── PermitWithoutStream == true → 핑 진행
        │   └── PermitWithoutStream == false → 스킵
        │
        ├── PING 프레임 전송
        │   └── 고유 데이터(8바이트) 포함
        │
        ├── Timeout 대기
        │   ├── PING ACK 수신 → 정상, 타이머 리셋
        │   └── Timeout 초과, ACK 없음
        │       → 연결 종료
        │       → 모든 스트림에 에러 전파
        │
        └── GOAWAY 수신 시
            → ENHANCE_YOUR_CALM이면 Time을 2배로 늘림
            → 자동 적응
```

### 자동 Time 적응

```
클라이언트 Time = 10초
    │
    ├── 서버가 ENHANCE_YOUR_CALM GOAWAY 전송
    │   → 핑 간격이 서버 MinTime보다 짧다는 의미
    │
    ├── 클라이언트 자동 조정
    │   → Time = 10초 × 2 = 20초
    │
    ├── 다시 ENHANCE_YOUR_CALM 수신 시
    │   → Time = 20초 × 2 = 40초
    │
    └── 서버가 정상 응답할 때까지 반복
```

**왜 자동 적응인가?**

클라이언트와 서버의 keepalive 설정이 불일치할 때, 클라이언트가 자동으로
서버의 정책에 적응하여 연결이 반복적으로 끊기는 것을 방지한다.
(`keepalive/keepalive.go:44` 주석 참조)

---

## 4. PING 프레임 구조

```
HTTP/2 PING 프레임 (RFC 7540 §6.7):
┌────────────────────────────────────────┐
│ Length (3 bytes): 8                    │
│ Type (1 byte):   0x6 (PING)           │
│ Flags (1 byte):  0x0 (요청) / 0x1 (ACK)│
│ Stream ID:       0x0 (커넥션 레벨)     │
├────────────────────────────────────────┤
│ Opaque Data (8 bytes):                │
│   고유 식별자 (요청/응답 매칭용)        │
└────────────────────────────────────────┘
```

### BDP 추정과의 관계

gRPC-Go는 keepalive PING 외에도 **BDP(Bandwidth-Delay Product) 추정**을 위해
PING을 사용한다. BDP PING은 데이터 전송 중에 발생하며, 라운드트립 시간을
측정하여 최적의 윈도우 크기를 계산한다.

```
keepalive PING:
  목적: 커넥션 활성 확인
  발생: 유휴 시 주기적
  간격: keepalive.Time (기본 2시간)

BDP PING:
  목적: 대역폭 추정
  발생: 데이터 전송 중
  간격: 일정량 데이터 전송 후
```

---

## 5. 설정 조합 가이드

### 기본 권장 설정

```go
// 서버
grpc.KeepaliveParams(keepalive.ServerParameters{
    MaxConnectionIdle:     15 * time.Minute,
    MaxConnectionAge:      30 * time.Minute,
    MaxConnectionAgeGrace: 5 * time.Second,
    Time:                  2 * time.Hour,      // 기본값
    Timeout:               20 * time.Second,   // 기본값
}),
grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
    MinTime:             5 * time.Minute,      // 기본값
    PermitWithoutStream: false,                // 기본값
}),

// 클라이언트
grpc.WithKeepaliveParams(keepalive.ClientParameters{
    Time:                10 * time.Minute,     // > 서버 MinTime
    Timeout:             20 * time.Second,
    PermitWithoutStream: false,
}),
```

### 빠른 장애 감지 (로컬 네트워크)

```go
// 서버
keepalive.ServerParameters{
    Time:    10 * time.Second,
    Timeout: 3 * time.Second,
},
keepalive.EnforcementPolicy{
    MinTime:             5 * time.Second,
    PermitWithoutStream: true,
},

// 클라이언트
keepalive.ClientParameters{
    Time:                10 * time.Second,
    Timeout:             3 * time.Second,
    PermitWithoutStream: true,
},
```

### DNS 기반 LB 호환 (커넥션 갱신)

```go
// 서버: 주기적 커넥션 갱신으로 DNS 변경 반영
keepalive.ServerParameters{
    MaxConnectionAge:      5 * time.Minute,   // 짧은 수명
    MaxConnectionAgeGrace: 10 * time.Second,
},
```

### 프록시 환경 (NAT 타임아웃 방지)

```go
// 클라이언트: NAT 테이블 유지를 위한 주기적 핑
keepalive.ClientParameters{
    Time:                30 * time.Second,    // NAT 타임아웃보다 짧게
    Timeout:             10 * time.Second,
    PermitWithoutStream: true,                // 유휴 시에도 핑
},

// 서버: 클라이언트 핑 허용
keepalive.EnforcementPolicy{
    MinTime:             10 * time.Second,
    PermitWithoutStream: true,
},
```

---

## 6. 파라미터 불일치 문제

### 문제: 클라이언트 Time < 서버 MinTime

```
클라이언트 Time = 10초
서버 MinTime = 5분

→ 클라이언트가 10초마다 핑
→ 서버가 pingStrikes 누적
→ GOAWAY(ENHANCE_YOUR_CALM) 전송
→ 연결 종료
→ 클라이언트 재연결 후 반복

자동 적응:
→ 클라이언트 Time이 2배씩 증가 (10초 → 20초 → 40초 → ...)
→ 서버 MinTime 이상이 되면 안정화
→ 하지만 여러 번의 연결 끊김 발생
```

**해결**: 클라이언트 Time을 서버 MinTime 이상으로 설정

### 문제: PermitWithoutStream 불일치

```
클라이언트 PermitWithoutStream = true
서버 PermitWithoutStream = false

→ 유휴 커넥션에서 클라이언트가 핑 전송
→ 서버가 pingStrikes 누적
→ GOAWAY(ENHANCE_YOUR_CALM)
```

**해결**: 양쪽 PermitWithoutStream 일치시키기

---

## 7. 타이머 상호작용

```
서버 타이머 (http2_server.go):
┌─────────────────────────────────────────────┐
│                                              │
│  MaxConnectionIdle Timer                     │
│  ├── 시작: 마지막 스트림 종료 시              │
│  ├── 리셋: 새 스트림 시작 시                  │
│  └── 만료: GOAWAY → Close                    │
│                                              │
│  MaxConnectionAge Timer                      │
│  ├── 시작: 커넥션 생성 시                     │
│  ├── 리셋: 없음 (절대 시간)                   │
│  └── 만료: GOAWAY → Grace → Close            │
│                                              │
│  Keepalive Timer                             │
│  ├── 시작: 마지막 활동 이후                   │
│  ├── 리셋: 데이터 수신 시                     │
│  └── 만료: PING → Timeout → Close            │
│                                              │
│  Ping Strike Counter                         │
│  ├── 증가: 클라이언트 PING < MinTime          │
│  ├── 리셋: 정상 간격 PING 수신 시             │
│  └── 초과: GOAWAY(ENHANCE_YOUR_CALM)          │
│                                              │
└─────────────────────────────────────────────┘

클라이언트 타이머 (http2_client.go):
┌─────────────────────────────────────────────┐
│                                              │
│  Keepalive Timer                             │
│  ├── 시작: 마지막 활동 이후                   │
│  ├── 리셋: 데이터 수신/전송 시                │
│  └── 만료: PING → Timeout → Close            │
│                                              │
│  ENHANCE_YOUR_CALM 수신 시                   │
│  └── Time = Time × 2 (자동 백오프)           │
│                                              │
└─────────────────────────────────────────────┘
```

---

## 8. GOAWAY 프레임

```
HTTP/2 GOAWAY 프레임:
┌────────────────────────────────────────────────┐
│ Type: 0x7 (GOAWAY)                             │
│ Stream ID: 0x0                                 │
├────────────────────────────────────────────────┤
│ Last-Stream-ID (4 bytes)                       │
│   → 이 ID 이하의 스트림은 처리 완료 보장       │
│                                                │
│ Error Code (4 bytes)                           │
│   → 0x0: NO_ERROR (정상 종료)                  │
│   → 0xB: ENHANCE_YOUR_CALM (핑 과다)          │
│                                                │
│ Debug Data (가변)                              │
│   → 사람이 읽을 수 있는 디버그 정보            │
└────────────────────────────────────────────────┘
```

### GOAWAY 발생 원인

| 원인 | Error Code | gRPC 동작 |
|------|-----------|----------|
| MaxConnectionAge | NO_ERROR | 정상 종료, Grace 대기 |
| MaxConnectionIdle | NO_ERROR | 유휴 종료 |
| GracefulStop() | NO_ERROR | 서버 종료 |
| 핑 과다 | ENHANCE_YOUR_CALM | 즉시 종료 |
| 프로토콜 에러 | PROTOCOL_ERROR | 즉시 종료 |

---

## 9. 모니터링

### Channelz로 keepalive 상태 확인

```bash
# keepalive로 인한 연결 종료 확인
grpcdebug localhost:8080 channelz sockets

# 소켓별 핑 통계 확인
grpcdebug localhost:8080 channelz socket <id>
```

### 로그로 확인

```bash
export GRPC_GO_LOG_SEVERITY_LEVEL=info
export GRPC_GO_LOG_VERBOSITY_LEVEL=2

# 예상 로그:
# transport: closing server transport due to maximum connection age
# transport: closing server transport due to idleness
# transport: received GOAWAY from server
```

---

## 10. 트러블슈팅

### "ENHANCE_YOUR_CALM" GOAWAY

```
에러: code = Unavailable desc = transport: received GOAWAY with ENHANCE_YOUR_CALM

원인: 클라이언트 핑 간격이 서버 MinTime보다 짧음

해결:
1. 클라이언트 Time을 서버 MinTime 이상으로 설정
2. 서버 MinTime을 낮추기
3. PermitWithoutStream 일치시키기
```

### 유휴 커넥션 갑자기 종료

```
원인: 서버 MaxConnectionIdle 설정

해결:
1. MaxConnectionIdle 값 늘리기
2. 클라이언트에서 PermitWithoutStream: true로 핑 유지
3. 서버에서 PermitWithoutStream: true로 핑 허용
```

### NAT/프록시 뒤에서 커넥션 끊김

```
원인: NAT 테이블 타임아웃 (보통 5분)으로 매핑 제거

해결:
1. 클라이언트 Time을 NAT 타임아웃보다 짧게 (예: 30초)
2. PermitWithoutStream: true
3. 서버 MinTime을 클라이언트 Time 이하로
4. 서버 PermitWithoutStream: true
```
