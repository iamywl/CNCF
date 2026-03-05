# PoC-14: 메트릭 수집 (stats.Handler) 시뮬레이션

## 개념

gRPC의 `stats.Handler` 인터페이스는 RPC 및 연결 수준의 이벤트를 후킹하여 메트릭을 수집하는 표준 방법이다. OpenTelemetry, Prometheus 등과 연동하여 관측성(observability)을 제공한다.

### Handler 인터페이스

```go
type Handler interface {
    TagRPC(ctx, *RPCTagInfo) ctx     // RPC 시작 시 태그 부착
    HandleRPC(ctx, RPCStats)         // RPC 이벤트 처리
    TagConn(ctx, *ConnTagInfo) ctx   // 연결 시작 시 태그 부착
    HandleConn(ctx, ConnStats)       // 연결 이벤트 처리
}
```

### 이벤트 호출 순서

```
연결 수립:
  TagConn → ConnBegin

RPC 호출 (Unary, 클라이언트 측):
  TagRPC → Begin → OutPayload → InPayload → End

RPC 호출 (Unary, 서버 측):
  TagRPC → Begin → InPayload → OutPayload → End

연결 종료:
  ConnEnd
```

### Context를 통한 상관관계

```
TagConn: ConnTag{ConnID, RemoteAddr} → context에 저장
   ↓
TagRPC: RPCTag{Method, StartTime, ConnID} → context에 저장
   ↓                                          (ConnTag에서 ConnID 복사)
HandleRPC(Begin/InPayload/OutPayload/End):
   context에서 RPCTag 추출 → method, connID로 메트릭 연관
```

## 실행 방법

```bash
go run main.go
```

## 예상 출력

```
========================================
gRPC Stats Handler 시뮬레이션
========================================

[3] RPC 이벤트 시뮬레이션
──────────────────────────
  RPC 호출 시뮬레이션 중...

[4] 전체 이벤트 로그
─────────────────────
    TagConn: remote=10.0.0.1:443, id=conn-xxxx
    HandleConn[Begin]: id=conn-xxxx
    TagRPC: method=/myservice.UserService/GetUser
    HandleRPC[Begin]: /myservice.UserService/GetUser (conn=conn-xxxx)
    HandleRPC[OutPayload]: len=342 wire=347
    HandleRPC[InPayload]: len=567 wire=572
    HandleRPC[End]: OK latency=3ms
    ...

[5] 메트릭 요약
────────────────
  총 RPC 수: 8 (에러: 2)
  총 전송 바이트: ...
  활성 연결: 1
  지연 시간: min=1ms, avg=3ms, max=5ms
```

## 관련 소스

| 파일 | 설명 |
|------|------|
| `stats/handlers.go` | Handler 인터페이스 정의 |
| `stats/stats.go` | RPCStats, ConnStats, Begin, End, InPayload, OutPayload |
| `internal/transport/http2_client.go` | 클라이언트에서 stats 이벤트 발생 |
| `internal/transport/http2_server.go` | 서버에서 stats 이벤트 발생 |
