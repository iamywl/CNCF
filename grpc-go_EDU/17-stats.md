# 17. gRPC-Go Stats (메트릭 시스템) 심화

## 개요

gRPC-Go의 Stats 시스템은 **RPC와 커넥션 수준의 이벤트를 수집**하는 콜백 기반 프레임워크이다.
`stats.Handler` 인터페이스를 구현하여 서버/클라이언트에 등록하면,
모든 RPC의 시작/종료, 메시지 송수신, 커넥션 이벤트를 실시간으로 수신할 수 있다.
Prometheus, OpenTelemetry 등 외부 메트릭 시스템과 연동하는 기반이 된다.

**소스코드**:
- `stats/stats.go` — 이벤트 타입 정의
- `stats/handlers.go` — Handler 인터페이스
- `experimental/stats/` — 메트릭 레지스트리 (실험적)
- `experimental/opentelemetry/` — OpenTelemetry 통합

---

## 1. Handler 인터페이스 (`stats/handlers.go`)

```go
type Handler interface {
    // TagRPC: RPC 시작 시 호출. 컨텍스트에 태그 부착.
    TagRPC(context.Context, *RPCTagInfo) context.Context

    // HandleRPC: RPC 이벤트 발생 시 호출.
    HandleRPC(context.Context, RPCStats)

    // TagConn: 새 커넥션 시 호출. 컨텍스트에 태그 부착.
    TagConn(context.Context, *ConnTagInfo) context.Context

    // HandleConn: 커넥션 이벤트 발생 시 호출.
    HandleConn(context.Context, ConnStats)
}
```

### 호출 흐름

```
커넥션 수립 ──▶ TagConn(ctx, ConnTagInfo) → ctx'
                  │
                  ├──▶ HandleConn(ctx', ConnBegin)
                  │
RPC 시작 ────────▶ TagRPC(ctx', RPCTagInfo) → ctx''
                  │
                  ├──▶ HandleRPC(ctx'', Begin)
                  ├──▶ HandleRPC(ctx'', InHeader)
                  ├──▶ HandleRPC(ctx'', InPayload)  × N
                  ├──▶ HandleRPC(ctx'', OutPayload) × M
                  ├──▶ HandleRPC(ctx'', OutHeader)  (서버)
                  ├──▶ HandleRPC(ctx'', InTrailer)  (클라이언트)
                  ├──▶ HandleRPC(ctx'', OutTrailer) (서버)
                  └──▶ HandleRPC(ctx'', End)
                  │
커넥션 종료 ──────▶ HandleConn(ctx', ConnEnd)
```

### TagInfo 구조체

```go
// RPCTagInfo
type RPCTagInfo struct {
    FullMethodName string   // "/package.Service/Method"
    FailFast       bool     // WaitForReady 비활성 여부
}

// ConnTagInfo
type ConnTagInfo struct {
    RemoteAddr net.Addr    // 원격 주소
    LocalAddr  net.Addr    // 로컬 주소
}
```

**왜 Tag와 Handle을 분리하는가?**

`TagRPC`/`TagConn`은 **컨텍스트에 사용자 정의 데이터를 부착**하는 기회이다.
예를 들어 RPC 시작 시 타이머를 시작하고 ctx에 저장한 뒤,
`HandleRPC(End)`에서 해당 타이머로 지연 시간을 계산할 수 있다.
이벤트 처리와 컨텍스트 준비를 분리하여 깔끔한 설계를 유지한다.

---

## 2. RPC 이벤트 타입 (`stats/stats.go`)

### RPCStats 인터페이스

```go
type RPCStats interface {
    isRPCStats()
    IsClient() bool   // 클라이언트 측이면 true
}
```

### 이벤트 상세

#### Begin — RPC 시작

```go
type Begin struct {
    Client                    bool      // 클라이언트 측 여부
    BeginTime                 time.Time // RPC 시작 시각
    FailFast                  bool      // WaitForReady 비활성
    IsClientStream            bool      // 클라이언트 스트리밍
    IsServerStream            bool      // 서버 스트리밍
    IsTransparentRetryAttempt bool      // 투명 재시도
}
```

#### InHeader — 헤더 수신

```go
type InHeader struct {
    Client     bool        // 클라이언트 측 여부
    WireLength int         // 와이어 상 바이트 수
    Compression string     // 압축 알고리즘
    Header     metadata.MD // 수신 헤더

    // 서버 측 전용
    FullMethod string      // "/pkg.Svc/Method"
    RemoteAddr net.Addr    // 클라이언트 주소
    LocalAddr  net.Addr    // 서버 주소
}
```

#### InPayload — 메시지 수신

```go
type InPayload struct {
    Client           bool  // 클라이언트 측 여부
    Payload          any   // 역직렬화된 메시지 (참조)
    Length           int   // 역직렬화 후 바이트 수
    CompressedLength int   // 압축 해제 전 바이트 수
    WireLength       int   // 와이어 상 바이트 수 (프레이밍 포함)
    RecvTime         time.Time // 수신 시각
}
```

#### OutPayload — 메시지 송신

```go
type OutPayload struct {
    Client           bool
    Payload          any   // 직렬화 전 메시지 (참조)
    Length           int   // 직렬화 후 바이트 수
    CompressedLength int   // 압축 후 바이트 수
    WireLength       int   // 와이어 상 바이트 수
    SentTime         time.Time // 송신 시각
}
```

#### InTrailer — 트레일러 수신 (클라이언트)

```go
type InTrailer struct {
    Client     bool
    WireLength int
    Trailer    metadata.MD
}
```

#### OutTrailer — 트레일러 송신 (서버)

```go
type OutTrailer struct {
    Client     bool
    WireLength int
    Trailer    metadata.MD
}
```

#### End — RPC 완료

```go
type End struct {
    Client    bool
    BeginTime time.Time  // RPC 시작 시각
    EndTime   time.Time  // RPC 완료 시각
    Trailer   metadata.MD
    Error     error      // nil이면 성공
}
```

---

## 3. 커넥션 이벤트

### ConnStats 인터페이스

```go
type ConnStats interface {
    isConnStats()
    IsClient() bool
}
```

### ConnBegin — 커넥션 수립

```go
type ConnBegin struct {
    Client bool
}
```

### ConnEnd — 커넥션 종료

```go
type ConnEnd struct {
    Client bool
}
```

---

## 4. 이벤트 발생 위치

### 서버 측 (`server.go`)

```
processUnaryRPC:
  ├── HandleRPC(ctx, &InHeader{...})      ← 헤더 수신 후
  ├── HandleRPC(ctx, &InPayload{...})     ← 요청 메시지 수신 후
  ├── HandleRPC(ctx, &OutPayload{...})    ← 응답 메시지 송신 후
  ├── HandleRPC(ctx, &OutTrailer{...})    ← 트레일러 송신 후
  └── HandleRPC(ctx, &End{...})           ← RPC 완료 후

processStreamingRPC:
  ├── HandleRPC(ctx, &InHeader{...})
  ├── HandleRPC(ctx, &InPayload{...})     × N (클라이언트 스트리밍)
  ├── HandleRPC(ctx, &OutPayload{...})    × M (서버 스트리밍)
  └── HandleRPC(ctx, &End{...})
```

### 클라이언트 측 (`stream.go`)

```
newClientStream:
  ├── HandleRPC(ctx, &Begin{...})         ← 스트림 생성

clientStream.RecvMsg:
  ├── HandleRPC(ctx, &InHeader{...})      ← 첫 헤더 수신
  ├── HandleRPC(ctx, &InPayload{...})     ← 각 메시지 수신
  └── HandleRPC(ctx, &InTrailer{...})     ← 트레일러 수신

clientStream.SendMsg:
  └── HandleRPC(ctx, &OutPayload{...})    ← 각 메시지 송신

clientStream.finish:
  └── HandleRPC(ctx, &End{...})           ← RPC 완료
```

---

## 5. Handler 등록

### 서버

```go
s := grpc.NewServer(
    grpc.StatsHandler(&myHandler{}),
    grpc.StatsHandler(&anotherHandler{}),  // 여러 개 가능
)
```

### 클라이언트

```go
conn, err := grpc.NewClient(target,
    grpc.WithStatsHandler(&myHandler{}),
    grpc.WithStatsHandler(&anotherHandler{}),
)
```

**여러 핸들러 등록 시**: 모든 핸들러가 순서대로 호출된다.

---

## 6. 실용 구현 예시

### Prometheus 메트릭 수집

```go
type prometheusHandler struct {
    rpcDuration  *prometheus.HistogramVec
    rpcCount     *prometheus.CounterVec
    msgSentSize  *prometheus.HistogramVec
    msgRecvSize  *prometheus.HistogramVec
    activeConns  *prometheus.GaugeVec
}

func (h *prometheusHandler) TagRPC(ctx context.Context, info *stats.RPCTagInfo) context.Context {
    return context.WithValue(ctx, rpcKey{}, &rpcData{
        method:    info.FullMethodName,
        startTime: time.Now(),
    })
}

func (h *prometheusHandler) HandleRPC(ctx context.Context, s stats.RPCStats) {
    data := ctx.Value(rpcKey{}).(*rpcData)

    switch st := s.(type) {
    case *stats.End:
        duration := st.EndTime.Sub(st.BeginTime).Seconds()
        code := status.Code(st.Error).String()
        h.rpcDuration.WithLabelValues(data.method, code).Observe(duration)
        h.rpcCount.WithLabelValues(data.method, code).Inc()

    case *stats.InPayload:
        h.msgRecvSize.WithLabelValues(data.method).Observe(float64(st.Length))

    case *stats.OutPayload:
        h.msgSentSize.WithLabelValues(data.method).Observe(float64(st.Length))
    }
}

func (h *prometheusHandler) TagConn(ctx context.Context, info *stats.ConnTagInfo) context.Context {
    return ctx
}

func (h *prometheusHandler) HandleConn(ctx context.Context, s stats.ConnStats) {
    switch s.(type) {
    case *stats.ConnBegin:
        h.activeConns.WithLabelValues("active").Inc()
    case *stats.ConnEnd:
        h.activeConns.WithLabelValues("active").Dec()
    }
}
```

### 액세스 로깅

```go
type accessLogHandler struct{}

func (h *accessLogHandler) TagRPC(ctx context.Context, info *stats.RPCTagInfo) context.Context {
    return context.WithValue(ctx, startKey{}, time.Now())
}

func (h *accessLogHandler) HandleRPC(ctx context.Context, s stats.RPCStats) {
    if end, ok := s.(*stats.End); ok {
        start := ctx.Value(startKey{}).(time.Time)
        method := "unknown"
        if md, ok := metadata.FromIncomingContext(ctx); ok {
            if vals := md.Get(":path"); len(vals) > 0 {
                method = vals[0]
            }
        }
        duration := end.EndTime.Sub(start)
        code := status.Code(end.Error)
        log.Printf("[gRPC] %s %s %v", method, code, duration)
    }
}
```

### 페이로드 크기 제한 모니터링

```go
func (h *sizeMonitor) HandleRPC(ctx context.Context, s stats.RPCStats) {
    switch st := s.(type) {
    case *stats.InPayload:
        if st.Length > 1024*1024 {  // 1MB 초과
            log.Printf("WARN: 큰 수신 메시지: %d bytes", st.Length)
        }
    case *stats.OutPayload:
        if st.Length > 1024*1024 {
            log.Printf("WARN: 큰 송신 메시지: %d bytes", st.Length)
        }
    }
}
```

---

## 7. OpenTelemetry 통합 (`experimental/opentelemetry/`)

gRPC-Go는 실험적 OpenTelemetry 통합을 제공한다.

### 사용법

```go
import "google.golang.org/grpc/stats/opentelemetry"

// 서버
s := grpc.NewServer(opentelemetry.ServerOption(opentelemetry.Options{
    MetricsOptions: opentelemetry.MetricsOptions{
        MeterProvider: provider,
    },
}))

// 클라이언트
conn, err := grpc.NewClient(target, opentelemetry.DialOption(opentelemetry.Options{
    MetricsOptions: opentelemetry.MetricsOptions{
        MeterProvider: provider,
    },
}))
```

### OTel이 수집하는 메트릭

| 메트릭 | 타입 | 설명 |
|--------|------|------|
| `grpc.client.attempt.started` | Counter | RPC 시도 시작 수 |
| `grpc.client.attempt.duration` | Histogram | RPC 시도 지연 |
| `grpc.client.attempt.sent_total_compressed_message_size` | Histogram | 송신 메시지 크기 |
| `grpc.client.attempt.rcvd_total_compressed_message_size` | Histogram | 수신 메시지 크기 |
| `grpc.client.call.duration` | Histogram | 전체 호출 지연 (재시도 포함) |
| `grpc.server.call.started` | Counter | 서버 RPC 시작 수 |
| `grpc.server.call.duration` | Histogram | 서버 처리 지연 |
| `grpc.server.call.sent_total_compressed_message_size` | Histogram | 서버 송신 크기 |
| `grpc.server.call.rcvd_total_compressed_message_size` | Histogram | 서버 수신 크기 |

---

## 8. 실험적 메트릭 레지스트리 (`experimental/stats/`)

gRPC-Go는 내부 메트릭을 위한 **실험적 레지스트리**를 제공한다.

```go
// experimental/stats/metricregistry.go
type MetricDescriptor struct {
    Name           string
    Description    string
    Unit           string
    Labels         []string
    OptionalLabels []string
    Default        bool        // 기본 활성화 여부
}

// Int64 카운터 등록
var myMetric = RegisterInt64Count(MetricDescriptor{
    Name:        "grpc.my.metric",
    Description: "My custom metric",
    Unit:        "{count}",
    Labels:      []string{"grpc.method"},
    Default:     true,
})
```

### MetricsRecorder 인터페이스

```go
type MetricsRecorder interface {
    RecordInt64Count(handle *Int64CountHandle, incr int64, labels ...string)
    RecordFloat64Count(handle *Float64CountHandle, incr float64, labels ...string)
    RecordInt64Histo(handle *Int64HistoHandle, incr int64, labels ...string)
    RecordFloat64Histo(handle *Float64HistoHandle, incr float64, labels ...string)
    RecordInt64Gauge(handle *Int64GaugeHandle, val int64, labels ...string)
}
```

---

## 9. StatsHandler vs Interceptor vs Channelz

| 특성 | StatsHandler | Interceptor | Channelz |
|------|-------------|-------------|----------|
| **수준** | 메시지/커넥션 | RPC | 채널/소켓 |
| **접근** | 읽기 전용 | 읽기/쓰기 | 읽기 전용 |
| **요청 수정** | 불가 | 가능 | 불가 |
| **응답 수정** | 불가 | 가능 | 불가 |
| **에러 핸들링** | End 이벤트로 관찰 | 에러 가로채기/수정 가능 | 집계 통계만 |
| **메시지 접근** | Payload 참조 | 가능 | 불가 |
| **바이트 크기** | 정확한 wire/compressed 크기 | 불가 | 누적만 |
| **커넥션 이벤트** | ConnBegin/ConnEnd | 불가 | Socket 통계 |
| **용도** | 메트릭, 모니터링, 로깅 | 인증, 로깅, 수정 | 실시간 진단 |

### 호출 순서

```
클라이언트 Invoke:
  1. Interceptor 체인 실행
  2. StatsHandler.TagRPC
  3. StatsHandler.HandleRPC(Begin)
  4. ... (transport 통신) ...
  5. StatsHandler.HandleRPC(End)
  6. Interceptor 체인 반환
```

**왜 둘 다 필요한가?**

인터셉터는 **비즈니스 로직** (인증, 검증, 변환)에 적합하고,
StatsHandler는 **관측** (메트릭, 로깅)에 적합하다.
StatsHandler는 와이어 레벨 크기 정보를 제공하는데, 이는 인터셉터에서는 얻을 수 없다.

---

## 10. 성능 고려사항

### HandleRPC는 동기적

```
HandleRPC는 RPC 처리 경로에서 동기적으로 호출된다.
→ 느린 HandleRPC는 RPC 지연에 직접 영향
→ 비동기 처리가 필요하면 채널에 이벤트를 넣고 별도 goroutine에서 처리

// 안티패턴: HandleRPC에서 네트워크 호출
func (h *bad) HandleRPC(ctx context.Context, s stats.RPCStats) {
    http.Post("http://metrics-server/...", ...)  // RPC 지연 증가!
}

// 권장: 비동기 처리
func (h *good) HandleRPC(ctx context.Context, s stats.RPCStats) {
    select {
    case h.eventCh <- s:  // 논블로킹 전송
    default:              // 채널 꽉 차면 드롭
    }
}
```

### 여러 Handler 등록 시

```go
// 모든 핸들러가 순서대로 호출됨
s := grpc.NewServer(
    grpc.StatsHandler(handler1),  // 첫 번째
    grpc.StatsHandler(handler2),  // 두 번째
    grpc.StatsHandler(handler3),  // 세 번째
)
// 총 호출 시간 = handler1 + handler2 + handler3
```

---

## 11. 종합 아키텍처

```
┌─────────────────────────────────────────────────┐
│                   gRPC Server/Client             │
│                                                  │
│  ┌───────────┐  ┌───────────┐  ┌──────────┐    │
│  │ Transport │  │  Stream   │  │  Server  │    │
│  │  (conn)   │  │ (RPC msg) │  │ (handler)│    │
│  └─────┬─────┘  └─────┬─────┘  └────┬─────┘    │
│        │              │              │          │
│   ConnBegin      InHeader/        Begin/       │
│   ConnEnd        InPayload/       End          │
│                  OutPayload/                    │
│                  InTrailer                      │
│        │              │              │          │
│        └──────────────┼──────────────┘          │
│                       ▼                          │
│           ┌────────────────────┐                │
│           │   stats.Handler    │ × N            │
│           │  (사용자 구현)      │                │
│           └─────────┬──────────┘                │
│                     │                            │
└─────────────────────┼────────────────────────────┘
                      │
         ┌────────────┼────────────┐
         ▼            ▼            ▼
   ┌──────────┐ ┌──────────┐ ┌──────────┐
   │Prometheus│ │OpenTelemetry│ │ Custom  │
   │ Exporter │ │  Exporter │ │ Logger  │
   └──────────┘ └──────────┘ └──────────┘
```
