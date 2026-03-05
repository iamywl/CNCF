# 16. gRPC-Go Channelz 심화

## 개요

Channelz는 gRPC의 **런타임 진단 시스템**이다. 채널, 서브채널, 소켓, 서버의
실시간 상태와 통계를 조회할 수 있으며, 프로덕션 환경에서 gRPC 연결 문제를
디버깅하는 핵심 도구이다.

**소스코드**:
- `internal/channelz/` — 내부 구현
  - `channel.go` — Channel/SubChannel 메트릭
  - `server.go` — Server 메트릭
  - `socket.go` — Socket 통계
  - `trace.go` — 이벤트 트레이스
- `channelz/service/` — gRPC 서비스 구현
- `channelz/grpc_channelz_v1/` — Protobuf 정의

---

## 1. Channelz 계층 구조

```
                    ┌─────────────────┐
                    │   Channelz DB   │
                    │  (in-memory)    │
                    └────────┬────────┘
                             │
            ┌────────────────┼────────────────┐
            │                │                │
     ┌──────▼──────┐  ┌─────▼──────┐  ┌─────▼──────┐
     │  Top-Level  │  │   Server   │  │   Server   │
     │  Channel    │  │     #1     │  │     #2     │
     │  (ClientConn)│ │            │  │            │
     └──────┬──────┘  └─────┬──────┘  └────────────┘
            │               │
     ┌──────▼──────┐  ┌─────▼──────┐
     │ SubChannel  │  │  Listen    │
     │  (addrConn) │  │  Socket    │
     └──────┬──────┘  └─────┬──────┘
            │               │
     ┌──────▼──────┐  ┌─────▼──────┐
     │   Socket    │  │  Normal    │
     │ (TCP conn)  │  │  Socket    │
     └─────────────┘  └────────────┘
```

### 엔티티 설명

| 엔티티 | gRPC 매핑 | 설명 |
|--------|----------|------|
| Top-Level Channel | `ClientConn` | 클라이언트 채널 (하나의 타겟) |
| SubChannel | `addrConn` | 서브채널 (하나의 주소에 대한 연결) |
| Socket | `net.Conn` | 실제 TCP 소켓 |
| Server | `Server` | gRPC 서버 인스턴스 |
| Listen Socket | `net.Listener` | 리스닝 소켓 |
| Normal Socket | `transport.ServerTransport` | 서버 측 클라이언트 연결 |

---

## 2. Channel 메트릭 (`internal/channelz/channel.go`)

### Channel 구조체

```go
type Channel struct {
    Entity
    // nested: 자식 채널 (xDS 등에서 중첩 채널 사용)
    NestedChans map[int64]string
    SubChans    map[int64]string

    // 트레이스 이벤트
    trace *ChannelTrace

    // 메트릭
    ChannelMetrics
}
```

### ChannelMetrics

```go
type ChannelMetrics struct {
    // 연결 상태
    State           atomic.Int64    // connectivity.State
    Target          string          // 타겟 주소

    // RPC 통계
    CallsStarted    atomic.Int64    // 시작된 RPC 수
    CallsSucceeded  atomic.Int64    // 성공한 RPC 수
    CallsFailed     atomic.Int64    // 실패한 RPC 수
    LastCallStartedTimestamp atomic.Int64  // 마지막 RPC 시작 시각
}
```

### 제공 정보

| 필드 | 의미 | 활용 |
|------|------|------|
| State | Idle/Connecting/Ready/TransientFailure/Shutdown | 연결 상태 확인 |
| Target | `dns:///my-service:8080` | 대상 서비스 확인 |
| CallsStarted | 누적 RPC 시작 수 | 처리량 측정 |
| CallsSucceeded | 누적 성공 수 | 성공률 계산 |
| CallsFailed | 누적 실패 수 | 에러율 계산 |
| LastCallStartedTimestamp | 마지막 RPC 시각 | 활성도 확인 |

---

## 3. SubChannel 메트릭

SubChannel은 Channel과 동일한 메트릭 구조를 공유한다.
하나의 주소(address)에 대한 연결을 나타내며, 연결 상태와 RPC 통계를 제공한다.

```
Channel (ClientConn)
├── SubChannel #1 (10.0.0.1:8080)
│   ├── State: Ready
│   ├── CallsStarted: 1500
│   ├── CallsSucceeded: 1495
│   ├── CallsFailed: 5
│   └── Socket #1 (TCP conn)
│
└── SubChannel #2 (10.0.0.2:8080)
    ├── State: TransientFailure
    ├── CallsStarted: 1000
    ├── CallsSucceeded: 990
    ├── CallsFailed: 10
    └── Socket #2 (TCP conn) — 연결 실패 상태
```

---

## 4. Socket 메트릭 (`internal/channelz/socket.go`)

### SocketMetrics

```go
type SocketMetrics struct {
    // 스트림 통계
    StreamsStarted   atomic.Int64
    StreamsSucceeded atomic.Int64
    StreamsFailed    atomic.Int64

    // 메시지 통계
    MessagesSent     atomic.Int64
    MessagesReceived atomic.Int64

    // 핑 통계
    KeepAlivesSent   atomic.Int64

    // 마지막 이벤트 시각
    LastLocalStreamCreatedTimestamp  atomic.Int64
    LastRemoteStreamCreatedTimestamp atomic.Int64
    LastMessageSentTimestamp         atomic.Int64
    LastMessageReceivedTimestamp     atomic.Int64
}
```

### Socket 추가 정보

| 필드 | 설명 |
|------|------|
| LocalAddr | 로컬 IP:포트 |
| RemoteAddr | 원격 IP:포트 |
| Security | TLS 정보 (프로토콜, 인증서) |
| SocketOptions | TCP 소켓 옵션 |

---

## 5. Server 메트릭 (`internal/channelz/server.go`)

### Server 구조체

```go
type Server struct {
    Entity

    // 리스닝 소켓 (net.Listener)
    ListenSockets map[int64]string

    // 서버 메트릭
    ServerMetrics
}
```

### ServerMetrics

```go
type ServerMetrics struct {
    CallsStarted    atomic.Int64
    CallsSucceeded  atomic.Int64
    CallsFailed     atomic.Int64
    LastCallStartedTimestamp atomic.Int64
}
```

---

## 6. 이벤트 트레이스 (`internal/channelz/trace.go`)

Channelz는 각 채널/서브채널의 중요 이벤트를 시간순으로 기록한다.

### 트레이스 이벤트

```go
type TraceEvent struct {
    Desc     string          // 이벤트 설명
    Severity Severity        // CtINFO, CtWarning, CtError
    Timestamp time.Time      // 발생 시각
    RefID    int64           // 관련 엔티티 ID
    RefType  RefChannelType  // 관련 엔티티 타입
}
```

### 기록되는 이벤트 예시

| Severity | 이벤트 | 설명 |
|----------|--------|------|
| INFO | Channel created | 채널 생성 |
| INFO | Resolver state updated | 리졸버 상태 갱신 |
| INFO | SubChannel created | 서브채널 생성 |
| INFO | SubChannel connectivity change: READY | 연결 상태 변경 |
| WARNING | SubChannel connectivity change: TRANSIENT_FAILURE | 연결 실패 |
| INFO | Channel authority set to "server:8080" | 권한 설정 |
| INFO | parsed dial target is: ... | 타겟 파싱 |

### 트레이스 활용

```
Channel #1 (dns:///my-service:8080) 트레이스:
    14:00:01 [INFO] Channel created
    14:00:01 [INFO] Channel authority set to "my-service:8080"
    14:00:01 [INFO] parsed dial target is: {Scheme:dns Authority: URL:{...}}
    14:00:01 [INFO] Resolver state updated: {Addresses:[10.0.0.1:8080, 10.0.0.2:8080]}
    14:00:01 [INFO] SubChannel #2 created
    14:00:01 [INFO] SubChannel #3 created
    14:00:02 [INFO] SubChannel #2 connectivity change: CONNECTING
    14:00:02 [INFO] SubChannel #3 connectivity change: CONNECTING
    14:00:02 [INFO] SubChannel #2 connectivity change: READY
    14:00:02 [INFO] SubChannel #3 connectivity change: READY
    14:05:00 [WARNING] SubChannel #3 connectivity change: TRANSIENT_FAILURE
    14:05:01 [INFO] SubChannel #3 connectivity change: CONNECTING
    14:05:02 [INFO] SubChannel #3 connectivity change: READY
```

---

## 7. 등록 메커니즘

### 자동 등록

gRPC-Go의 핵심 컴포넌트는 생성/삭제 시 자동으로 Channelz에 등록/해제된다.

```
ClientConn 생성:
  NewClient() → channelzRegistration()
    → channelz.RegisterChannel(cc, parentID) → ID 할당

addrConn 생성:
  cc.newAddrConnLocked() → channelz.RegisterSubChannel(ac, ccID) → ID 할당

Server 생성:
  NewServer() → channelz.RegisterServer(s) → ID 할당

트랜스포트 생성:
  newHTTP2Server() → channelz.RegisterSocket(socket, serverID) → ID 할당
```

### ID 할당

```go
// internal/channelz/ 내부
var idGen atomic.Int64

func RegisterChannel(c *Channel, pid int64) int64 {
    id := idGen.Add(1)
    c.ID = id
    c.Parent = pid
    db.addChannel(id, c)
    return id
}
```

모든 Channelz 엔티티는 **전역 고유 ID**를 가진다. 이 ID로 부모-자식 관계를 추적한다.

---

## 8. Channelz gRPC 서비스

### 서비스 등록

```go
import "google.golang.org/grpc/channelz/service"

s := grpc.NewServer()
channelz.RegisterChannelzServiceToServer(s)
```

### 제공 RPC

| RPC | 설명 |
|-----|------|
| `GetTopChannels` | 최상위 채널 목록 |
| `GetServers` | 서버 목록 |
| `GetChannel` | 특정 채널 상세 |
| `GetSubchannel` | 특정 서브채널 상세 |
| `GetSocket` | 특정 소켓 상세 |
| `GetServerSockets` | 서버의 소켓 목록 |

### 조회 예시 (grpcurl)

```bash
# 최상위 채널 목록
grpcurl -plaintext localhost:8080 \
  grpc.channelz.v1.Channelz/GetTopChannels

# 특정 채널 상세
grpcurl -plaintext -d '{"channel_id": 1}' localhost:8080 \
  grpc.channelz.v1.Channelz/GetChannel

# 서버 목록
grpcurl -plaintext localhost:8080 \
  grpc.channelz.v1.Channelz/GetServers

# 소켓 상세
grpcurl -plaintext -d '{"socket_id": 5}' localhost:8080 \
  grpc.channelz.v1.Channelz/GetSocket
```

### grpcdebug 도구

```bash
# 채널 목록
grpcdebug localhost:8080 channelz channels

# 채널 상세 (자식 포함)
grpcdebug localhost:8080 channelz channel 1

# 서브채널 상세
grpcdebug localhost:8080 channelz subchannel 2

# 소켓 상세
grpcdebug localhost:8080 channelz socket 5

# 서버 목록
grpcdebug localhost:8080 channelz servers

# 서버의 소켓 목록
grpcdebug localhost:8080 channelz server 3
```

---

## 9. 실제 디버깅 시나리오

### 시나리오 1: 연결이 안 되는 문제

```bash
# 1. 채널 목록 확인
grpcdebug localhost:8080 channelz channels
# → Channel #1: state=TRANSIENT_FAILURE, target=dns:///backend:8080

# 2. 채널 상세 확인
grpcdebug localhost:8080 channelz channel 1
# → SubChannels: [#2, #3]
# → Trace:
#   - Resolver state updated: {Addresses: [10.0.0.1:8080]}
#   - SubChannel #2: CONNECTING → TRANSIENT_FAILURE

# 3. 서브채널 확인
grpcdebug localhost:8080 channelz subchannel 2
# → State: TRANSIENT_FAILURE
# → Trace:
#   - Connection failed: dial tcp 10.0.0.1:8080: connection refused

# 결론: 백엔드 서버가 다운됨
```

### 시나리오 2: RPC 실패율 급증

```bash
# 1. 채널 통계 확인
grpcdebug localhost:8080 channelz channel 1
# → CallsStarted: 10000
# → CallsSucceeded: 8500
# → CallsFailed: 1500 (15% 실패!)

# 2. 서브채널별 확인
grpcdebug localhost:8080 channelz subchannel 2
# → CallsSucceeded: 4250, CallsFailed: 0   (정상)

grpcdebug localhost:8080 channelz subchannel 3
# → CallsSucceeded: 4250, CallsFailed: 1500 (이 서브채널에 집중)
# → State: READY (연결은 되어 있지만 에러 발생)

# 3. 소켓 확인
grpcdebug localhost:8080 channelz socket 5
# → RemoteAddr: 10.0.0.2:8080
# → MessagesReceived: 5750, MessagesSent: 5750

# 결론: 10.0.0.2 서버에 문제 있음 (애플리케이션 레벨 에러)
```

### 시나리오 3: Keepalive로 연결 끊김

```bash
# 1. 소켓 확인
grpcdebug localhost:8080 channelz socket 5
# → KeepAlivesSent: 150 (많은 핑 전송)
# → State: closed

# 2. 채널 트레이스 확인
# → "received GOAWAY with ENHANCE_YOUR_CALM"

# 결론: 클라이언트 핑 간격이 서버 EnforcementPolicy.MinTime보다 짧음
```

---

## 10. Channelz와 다른 관측성 도구 비교

| 도구 | 범위 | 접근 방식 | 용도 |
|------|------|----------|------|
| **Channelz** | gRPC 내부 | gRPC 서비스 조회 | 연결/채널 실시간 디버깅 |
| **StatsHandler** | RPC 이벤트 | 콜백 기반 | 메트릭 수집 (Prometheus, OTel) |
| **Binary Logging** | 전체 메시지 | 파일/네트워크 | 메시지 레벨 디버깅 |
| **gRPC Logging** | 프레임워크 로그 | 환경 변수 | 일반 디버깅 |

### 언제 Channelz를 사용하는가?

```
✓ 연결 상태가 이상할 때 (TRANSIENT_FAILURE 지속)
✓ 특정 서브채널/소켓의 문제를 좁혀야 할 때
✓ RPC 성공/실패율을 실시간으로 확인할 때
✓ Keepalive/GOAWAY 관련 문제 디버깅
✓ DNS 해석 결과 확인
✓ 밸런서가 SubConn을 올바르게 관리하는지 확인

✗ 상세 메시지 내용 확인 → Binary Logging
✗ 집계 메트릭/대시보드 → StatsHandler + Prometheus
✗ 분산 트레이싱 → OpenTelemetry
```

---

## 11. 보안 고려사항

Channelz는 내부 정보를 노출하므로, **프로덕션에서 접근 제어가 필수**이다.

```
노출되는 정보:
├── 서버 주소 (IP:포트)
├── 클라이언트 주소
├── TLS 인증서 정보
├── RPC 통계 (처리량, 에러율)
├── 타겟 서비스 이름
└── 연결 상태

보호 방법:
├── Channelz 서비스에 인증 인터셉터 적용
├── 별도 관리자 포트에서만 노출
├── 네트워크 정책으로 접근 제한
└── 프로덕션에서 불필요하면 비활성화
```

```go
// 관리자 포트에서만 Channelz 노출
adminServer := grpc.NewServer(grpc.Creds(adminCreds))
channelz.RegisterChannelzServiceToServer(adminServer)

adminLis, _ := net.Listen("tcp", ":9090")  // 관리자 포트
go adminServer.Serve(adminLis)

// 서비스 포트는 Channelz 없이
serviceServer := grpc.NewServer(grpc.Creds(serviceCreds))
pb.RegisterMyServiceServer(serviceServer, &myService{})
serviceLis, _ := net.Listen("tcp", ":8080")
serviceServer.Serve(serviceLis)
```

---

## 12. 종합 아키텍처

```
┌─────────────────────────────────────────────────────┐
│                    gRPC 프로세스                      │
│                                                      │
│  ┌───────────┐  ┌───────────┐  ┌──────────────┐    │
│  │ ClientConn│  │ ClientConn│  │    Server    │    │
│  │  #1       │  │  #2       │  │    #3        │    │
│  └─────┬─────┘  └─────┬─────┘  └──────┬───────┘    │
│        │              │               │             │
│        │    자동등록    │    자동등록    │   자동등록   │
│        ▼              ▼               ▼             │
│  ┌─────────────────────────────────────────────┐    │
│  │              Channelz DB (in-memory)         │    │
│  │  ┌─────────────────────────────────────┐    │    │
│  │  │ Channels: {1: Ch#1, 2: Ch#2}       │    │    │
│  │  │ SubChannels: {4: SC#4, 5: SC#5}    │    │    │
│  │  │ Sockets: {6: Sk#6, 7: Sk#7}        │    │    │
│  │  │ Servers: {3: Srv#3}                 │    │    │
│  │  └─────────────────────────────────────┘    │    │
│  └──────────────────────┬──────────────────────┘    │
│                         │                            │
│                         ▼                            │
│  ┌──────────────────────────────────────────┐       │
│  │   Channelz gRPC Service                   │       │
│  │   (grpc.channelz.v1.Channelz)            │       │
│  │                                           │       │
│  │   GetTopChannels() → Ch#1, Ch#2          │       │
│  │   GetChannel(1) → Ch#1 상세              │       │
│  │   GetServers() → Srv#3                   │       │
│  │   GetSocket(6) → Sk#6 상세               │       │
│  └──────────────────────────────────────────┘       │
│                         │                            │
└─────────────────────────┼────────────────────────────┘
                          │
                          ▼
                  ┌───────────────┐
                  │  grpcdebug    │
                  │  grpcurl      │
                  │  커스텀 도구   │
                  └───────────────┘
```
