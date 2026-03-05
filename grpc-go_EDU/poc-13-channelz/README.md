# PoC-13: Channelz 채널 진단 시스템 시뮬레이션

## 개념

Channelz는 gRPC의 내장 진단 시스템으로, 채널·서브채널·소켓·서버의 계층 구조와 실시간 메트릭을 제공한다. 프로덕션 환경에서 연결 상태와 RPC 통계를 디버깅하는 데 사용된다.

### 엔티티 계층 구조

```
channelz 레지스트리 (ChannelMap)
├── Server #1
│   ├── Socket (리스닝: 0.0.0.0:50051)
│   └── Socket (클라이언트 연결: 10.0.0.5:43210)
│
├── Channel #2 (dns:///myservice:443)
│   ├── SubChannel #3 (10.0.0.1:443) ─── Socket #5
│   └── SubChannel #4 (10.0.0.2:443) ─── Socket #6
│
└── Channel #7 (dns:///otherservice:443)
    └── SubChannel #8 ─── Socket #9
```

### 수집하는 메트릭

| 엔티티 | 메트릭 |
|--------|--------|
| Channel | CallsStarted, CallsSucceeded, CallsFailed, LastCallStartedTime |
| SubChannel | CallsStarted, CallsSucceeded, CallsFailed, State |
| Socket | StreamsStarted/Succeeded/Failed, MessagesSent/Recv, BytesSent/Recv |
| Server | CallsStarted, CallsSucceeded, CallsFailed |

### 이벤트 트레이스

각 엔티티에 이벤트 로그가 링 버퍼로 저장된다.
- 상태 변경 (IDLE -> CONNECTING -> READY)
- RPC 실패
- 서브채널 생성/삭제

## 실행 방법

```bash
go run main.go
```

## 예상 출력

```
========================================
gRPC Channelz 시뮬레이션
========================================

[1] 채널 계층 구조 생성
────────────────────────
  서버 등록: #1 grpc-server:50051
  채널 등록: #2 grpc-channel → dns:///myservice.example.com:443
  서브채널 등록: #3 subchannel-10.0.0.1:443
  서브채널 등록: #4 subchannel-10.0.0.2:443

[3] RPC 호출 메트릭 수집
─────────────────────────
  총 RPC: started=7, succeeded=5, failed=2
  sc1 메트릭: started=5, succeeded=3, failed=2
  sc2 메트릭: started=2, succeeded=2, failed=0

[4] 채널 트리 조회
────────────────────
  [Channel #2] grpc-channel → dns:///... (state=READY)
    [SubChannel #3] subchannel-10.0.0.1:443 (state=READY)
      [Socket #5] 192.168.1.100:54321 → 10.0.0.1:443
```

## 관련 소스

| 파일 | 설명 |
|------|------|
| `internal/channelz/channel.go` | Channel 구조체, ChannelMetrics |
| `internal/channelz/subchannel.go` | SubChannel 구조체 |
| `internal/channelz/socket.go` | Socket 구조체, SocketMetrics |
| `internal/channelz/channelmap.go` | 글로벌 ChannelMap 레지스트리 |
| `channelz/service/service.go` | channelz gRPC 서비스 구현 |
