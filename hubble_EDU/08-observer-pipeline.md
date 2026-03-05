# 08. Observer Pipeline Deep-Dive

## 개요

Hubble Observer Pipeline은 Cilium 에이전트 내부에서 네트워크 이벤트가 수집되어 최종 사용자에게
전달되기까지의 전체 데이터 처리 경로를 정의한다. 이 파이프라인의 핵심은 `LocalObserverServer`로,
BPF perf ring buffer에서 원시 이벤트를 수신하여 파싱, 훅 체인 실행, 링 버퍼 저장, 그리고
gRPC API를 통한 클라이언트 응답까지를 담당한다.

이 문서에서는 Observer Pipeline의 아키텍처, 이벤트 루프, 훅 시스템, RPC 구현,
네임스페이스 관리, Lost Event 처리 등을 소스코드 기반으로 심층 분석한다.

> **소스 경로**: `cilium/pkg/hubble/observer/`

---

## 1. 전체 파이프라인 아키텍처

### 1.1 데이터 흐름 개요

```
  BPF Perf Ring Buffer
         │
         ▼
  ┌──────────────────┐
  │  MonitorEvent     │  (UUID, Timestamp, NodeName, Payload)
  │  events channel   │  buffered channel (MonitorBuffer=1024)
  └────────┬─────────┘
           │
           ▼
  ┌──────────────────────────────────────────────────┐
  │           LocalObserverServer.Start()             │
  │                                                    │
  │  1. OnMonitorEvent hooks (pre-decode)              │
  │     ├── hook1(ctx, monitorEvent) → stop?           │
  │     └── hook2(ctx, monitorEvent) → stop?           │
  │                                                    │
  │  2. payloadParser.Decode(monitorEvent)             │
  │     ├── PerfEvent → L3/L4 or L7 Flow              │
  │     ├── AgentEvent → AgentEvent                    │
  │     └── LostEvent → LostEvent                     │
  │                                                    │
  │  3. trackNamespaces(flow)                          │
  │     └── nsManager.AddNamespace(src/dst)            │
  │                                                    │
  │  4. OnDecodedFlow hooks (post-decode, flow only)   │
  │     ├── hook1(ctx, flow) → stop?                   │
  │     └── hook2(ctx, flow) → stop?                   │
  │                                                    │
  │  5. numObservedFlows.Add(1)                        │
  │                                                    │
  │  6. OnDecodedEvent hooks (all event types)         │
  │     ├── hook1(ctx, event) → stop?                  │
  │     └── hook2(ctx, event) → stop?                  │
  │                                                    │
  │  7. ring.Write(event)                              │
  └──────────────────────────────────────────────────┘
           │
           ▼
  ┌──────────────────┐
  │  Ring Buffer      │  (MaxFlows=4095, lock-free)
  └────────┬─────────┘
           │
           ▼
  ┌──────────────────────────────────────────────────┐
  │         GetFlows / GetAgentEvents / etc.          │
  │                                                    │
  │  1. OnGetFlows hooks                               │
  │  2. BuildFilterList(whitelist, blacklist)           │
  │  3. newRingReader → eventsReader                   │
  │  4. eventsReader.Next() loop                       │
  │     ├── time range filter (since/until)            │
  │     ├── whitelist/blacklist Apply()                │
  │     └── LostEvent bypass (no filter)               │
  │  5. OnFlowDelivery hooks                           │
  │  6. FieldMask 적용                                 │
  │  7. server.Send(response)                          │
  └──────────────────────────────────────────────────┘
```

### 1.2 왜 이런 파이프라인 구조인가

1. **관심사 분리**: 수집(events channel) → 처리(decode + hooks) → 저장(ring) → 조회(RPC)가
   명확히 분리되어 각 단계를 독립적으로 테스트하고 확장할 수 있다.

2. **훅 기반 확장**: 파이프라인의 각 단계에 훅을 삽입하여, 코어 코드를 수정하지 않고도
   메트릭 수집, 필터링, 로깅 등을 추가할 수 있다.

3. **단일 이벤트 루프**: `Start()`의 `for range events` 루프가 모든 이벤트를 순차 처리하여
   동시성 문제를 회피하면서도 높은 처리량을 보장한다.

4. **Lock-free 링 버퍼**: 쓰기(Start 루프)와 읽기(RPC 핸들러)가 동시에 발생하지만
   링 버퍼의 atomic 기반 설계로 락 없이 동작한다.

---

## 2. LocalObserverServer 구조체

### 2.1 구조체 정의

```go
// 소스: cilium/pkg/hubble/observer/local_observer.go

type LocalObserverServer struct {
    // ring buffer that contains the references of all flows
    ring *container.Ring

    // events is the channel used by the writer(s) to send the flow data
    // into the observer server.
    events chan *observerTypes.MonitorEvent

    // stopped is mostly used in unit tests to signalize when the events
    // channel is empty, once it's closed.
    stopped chan struct{}

    log *slog.Logger

    // payloadParser decodes flowpb.Payload into flowpb.Flow
    payloadParser parser.Decoder

    opts observeroption.Options

    // startTime is the time when this instance was started
    startTime time.Time

    // numObservedFlows counts how many flows have been observed
    numObservedFlows atomic.Uint64

    nsManager namespace.Manager
}
```

### 2.2 각 필드의 역할

| 필드 | 타입 | 역할 |
|------|------|------|
| `ring` | `*container.Ring` | 디코딩된 이벤트를 저장하는 링 버퍼 (최대 4095개) |
| `events` | `chan *MonitorEvent` | BPF에서 수집된 원시 이벤트를 수신하는 버퍼 채널 (1024) |
| `stopped` | `chan struct{}` | events 채널이 닫힌 후 처리 완료를 알리는 신호 채널 |
| `log` | `*slog.Logger` | 구조화된 로거 |
| `payloadParser` | `parser.Decoder` | MonitorEvent를 Flow/AgentEvent/DebugEvent로 변환 |
| `opts` | `Options` | 훅 체인, 버퍼 크기 등 모든 설정 값 |
| `startTime` | `time.Time` | 서버 시작 시간 (uptime 계산용) |
| `numObservedFlows` | `atomic.Uint64` | 관측된 총 Flow 수 (lock-free 카운터) |
| `nsManager` | `namespace.Manager` | Flow에서 추출한 네임스페이스를 추적 관리 |

### 2.3 왜 atomic.Uint64를 사용하는가

`numObservedFlows`는 이벤트 루프(단일 고루틴)에서 증가하고, `ServerStatus` RPC(별도 고루틴)에서
읽히므로 data race를 방지해야 한다. `sync.Mutex` 대신 `atomic.Uint64`를 사용하는 이유는:

- 단순 카운터에 뮤텍스는 과도한 오버헤드
- `Add(1)`과 `Load()`만 필요하므로 atomic이 최적
- 고빈도 업데이트(모든 Flow마다) 시 성능 차이가 유의미

---

## 3. 초기화: NewLocalServer

### 3.1 생성 흐름

```go
// 소스: cilium/pkg/hubble/observer/local_observer.go

func NewLocalServer(
    payloadParser parser.Decoder,
    nsManager namespace.Manager,
    logger *slog.Logger,
    options ...observeroption.Option,
) (*LocalObserverServer, error) {
    opts := observeroption.Default // start with defaults
    options = append(options, DefaultOptions...)
    for _, opt := range options {
        if err := opt(&opts); err != nil {
            return nil, fmt.Errorf("failed to apply option: %w", err)
        }
    }
    // ...
    s := &LocalObserverServer{
        ring:          container.NewRing(opts.MaxFlows),
        events:        make(chan *observerTypes.MonitorEvent, opts.MonitorBuffer),
        // ...
    }
    for _, f := range s.opts.OnServerInit {
        err := f.OnServerInit(s)
        // ...
    }
    return s, nil
}
```

### 3.2 초기화 단계 상세

```
NewLocalServer(payloadParser, nsManager, logger, options...)
    │
    ├── 1. Default 옵션으로 시작
    │       opts := observeroption.Default
    │       ├── MaxFlows: 4095 (Capacity4095)
    │       └── MonitorBuffer: 1024
    │
    ├── 2. DefaultOptions 추가 (패키지 수준 변수)
    │       options = append(options, DefaultOptions...)
    │       └── 다른 패키지가 init()에서 추가 가능
    │
    ├── 3. 모든 Option 함수 순차 적용
    │       for _, opt := range options {
    │           opt(&opts)  // Options 구조체 수정
    │       }
    │
    ├── 4. 핵심 자원 생성
    │       ├── container.NewRing(4095)   → 링 버퍼
    │       ├── make(chan *MonitorEvent, 1024) → 이벤트 채널
    │       └── make(chan struct{})        → 정지 신호
    │
    └── 5. OnServerInit 훅 실행
            for _, f := range s.opts.OnServerInit {
                f.OnServerInit(s)  // 서버 참조 전달
            }
```

### 3.3 Functional Options 패턴

`Option`은 `func(o *Options) error` 타입으로, Go의 관용적인 Functional Options 패턴을 따른다:

```go
// 소스: cilium/pkg/hubble/observer/observeroption/option.go

type Option func(o *Options) error

func WithMaxFlows(capacity container.Capacity) Option {
    return func(o *Options) error {
        o.MaxFlows = capacity
        return nil
    }
}

func WithMonitorBuffer(size int) Option {
    return func(o *Options) error {
        o.MonitorBuffer = size
        return nil
    }
}
```

이 패턴의 장점:
- 기본값이 있으므로 호출자는 변경하고 싶은 옵션만 지정
- 검증 로직을 Option 내부에 포함 가능 (에러 반환)
- 새 옵션 추가 시 기존 코드 변경 불필요

---

## 4. 이벤트 루프: Start()

### 4.1 전체 코드 분석

`Start()`는 LocalObserverServer의 핵심으로, events 채널에서 이벤트를 읽어 처리하는
무한 루프를 실행한다:

```go
// 소스: cilium/pkg/hubble/observer/local_observer.go

func (s *LocalObserverServer) Start() {
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

nextEvent:
    for monitorEvent := range s.GetEventsChannel() {
        // 1. OnMonitorEvent 훅 체인
        for _, f := range s.opts.OnMonitorEvent {
            stop, err := f.OnMonitorEvent(ctx, monitorEvent)
            if err != nil {
                s.log.Info("failed in OnMonitorEvent", ...)
            }
            if stop {
                continue nextEvent
            }
        }

        // 2. 디코딩
        ev, err := s.payloadParser.Decode(monitorEvent)
        if err != nil {
            // 에러 처리 (unknown/skipped 이벤트는 무시)
            continue
        }

        // 3. Flow인 경우: 네임스페이스 추적 + OnDecodedFlow 훅
        if flow, ok := ev.Event.(*flowpb.Flow); ok {
            s.trackNamespaces(flow)
            for _, f := range s.opts.OnDecodedFlow {
                stop, err := f.OnDecodedFlow(ctx, flow)
                if stop { continue nextEvent }
            }
            s.numObservedFlows.Add(1)
        }

        // 4. OnDecodedEvent 훅 (모든 이벤트 타입)
        for _, f := range s.opts.OnDecodedEvent {
            stop, err := f.OnDecodedEvent(ctx, ev)
            if stop { continue nextEvent }
        }

        // 5. 링 버퍼에 저장
        s.GetRingBuffer().Write(ev)
    }
    close(s.GetStopped())
}
```

### 4.2 이벤트 루프 상태 머신

```
                    ┌─────────────────────┐
                    │  events 채널 대기     │
                    └──────────┬──────────┘
                               │ monitorEvent 수신
                               ▼
                    ┌─────────────────────┐
                    │ OnMonitorEvent 훅    │──── stop=true ──→ continue nextEvent
                    └──────────┬──────────┘
                               │ stop=false
                               ▼
                    ┌─────────────────────┐
                    │ payloadParser.Decode │──── error ──→ continue (skip)
                    └──────────┬──────────┘
                               │ success
                               ▼
                    ┌─────────────────────┐
                    │ ev.Event 타입 확인   │
                    └──────┬──────────────┘
                           │
              ┌────────────┼──────────────┐
              │ *Flow      │ *AgentEvent  │ *DebugEvent, etc.
              ▼            │              │
   ┌──────────────────┐   │              │
   │ trackNamespaces  │   │              │
   │ OnDecodedFlow    │   │              │
   │ numObservedFlows │   │              │
   └────────┬─────────┘   │              │
            │              │              │
            └──────────────┼──────────────┘
                           │
                           ▼
                    ┌─────────────────────┐
                    │ OnDecodedEvent 훅    │──── stop=true ──→ continue nextEvent
                    └──────────┬──────────┘
                               │
                               ▼
                    ┌─────────────────────┐
                    │ ring.Write(ev)       │
                    └──────────┬──────────┘
                               │
                               ▼
                    ┌─────────────────────┐
                    │  events 채널 대기     │ (루프 반복)
                    └─────────────────────┘
```

### 4.3 Context 취소 메커니즘

```go
ctx, cancel := context.WithCancel(context.Background())
defer cancel()
```

이 패턴의 의미:
1. `Start()`가 시작될 때 취소 가능한 context를 생성
2. 훅에서 스폰한 고루틴이 이 context를 참조
3. events 채널이 닫히면 `for range` 루프가 종료
4. `defer cancel()`이 실행되어 모든 하위 고루틴에 취소 신호 전달
5. `close(s.GetStopped())`로 외부에 종료 알림

### 4.4 에러 처리 전략

디코딩 에러 시 세 가지 카테고리로 구분한다:

```go
switch {
case
    errors.Is(err, parserErrors.ErrUnknownEventType),
    errors.Is(err, parserErrors.ErrEventSkipped),
    parserErrors.IsErrInvalidType(err):
    // 조용히 무시 — 알 수 없는 이벤트 타입이나 의도적 스킵
default:
    s.log.Debug("failed to decode payload", ...)
}
```

| 에러 유형 | 처리 | 이유 |
|-----------|------|------|
| `ErrUnknownEventType` | 무시 | Hubble이 처리하지 않는 BPF 이벤트 타입 |
| `ErrEventSkipped` | 무시 | 파서가 의도적으로 건너뛴 이벤트 |
| `IsErrInvalidType` | 무시 | perf ring buffer의 알 수 없는 타입 |
| 기타 에러 | Debug 로그 | 예상치 못한 파싱 실패 |

**왜 Debug 레벨인가**: 프로덕션 환경에서 일부 파싱 실패는 정상적(예: 새로운 BPF 프로그램
버전이 아직 지원되지 않는 이벤트를 보내는 경우)이므로 Info/Warn으로 올리면 로그 노이즈가 된다.

---

## 5. MonitorEvent 타입 시스템

### 5.1 MonitorEvent 구조체

```go
// 소스: cilium/pkg/hubble/observer/types/types.go

type MonitorEvent struct {
    UUID      uuid.UUID
    Timestamp time.Time
    NodeName  string
    Payload   any     // AgentEvent | PerfEvent | LostEvent
}
```

`Payload`는 `any` (= `interface{}`) 타입으로 세 가지 구체 타입 중 하나를 담는다.

### 5.2 Payload 타입 계층

```
MonitorEvent.Payload
    │
    ├── PerfEvent
    │   ├── Data []byte    ← BPF perf ring buffer의 원시 바이트
    │   └── CPU  int       ← 이벤트가 발생한 CPU 번호
    │
    ├── AgentEvent
    │   ├── Type int       ← monitorAPI.MessageType* 값
    │   └── Message any    ← accesslog.LogRecord, AgentNotifyMessage 등
    │
    └── LostEvent
        ├── Source int         ← 이벤트 손실 지점
        ├── NumLostEvents uint64  ← 손실된 이벤트 수
        ├── CPU int            ← CPU 번호 (perf ring buffer 손실 시)
        ├── First time.Time    ← 첫 손실 시간
        └── Last time.Time     ← 마지막 손실 시간
```

### 5.3 LostEvent 소스 상수

```go
// 소스: cilium/pkg/hubble/observer/types/types.go

const (
    LostEventSourceUnspec           = iota  // 0: 알 수 없는 소스
    LostEventSourcePerfRingBuffer           // 1: BPF perf ring buffer 오버플로우
    LostEventSourceEventsQueue              // 2: events 채널 가득 참
    LostEventSourceHubbleRingBuffer         // 3: Hubble 링 버퍼 오버라이트
)
```

### 5.4 이벤트 소스별 특성

| 소스 | 발생 조건 | 심각도 | 대응 |
|------|----------|--------|------|
| PerfRingBuffer | BPF perf 버퍼를 읽기 전에 덮어씀 | 높음 | perf 버퍼 크기 증가 |
| EventsQueue | events 채널(1024)이 가득 참 | 중간 | MonitorBuffer 증가 |
| HubbleRingBuffer | 링 버퍼에서 읽기 전에 덮어씀 | 낮음 | MaxFlows 증가 |

---

## 6. 훅(Hook) 체인 시스템

### 6.1 훅 타입 개요

Options 구조체에 정의된 7가지 훅 포인트:

```go
// 소스: cilium/pkg/hubble/observer/observeroption/option.go

type Options struct {
    // ...
    OnServerInit   []OnServerInit          // 서버 초기화 완료 시
    OnMonitorEvent []OnMonitorEvent        // 디코딩 전 원시 이벤트
    OnDecodedFlow  []OnDecodedFlow         // Flow 디코딩 후
    OnDecodedEvent []OnDecodedEvent        // 모든 이벤트 디코딩 후
    OnBuildFilter  []filters.OnBuildFilter // 필터 빌드 시
    OnFlowDelivery []OnFlowDelivery        // API 응답 전송 전
    OnGetFlows     []OnGetFlows            // GetFlows RPC 호출 시
}
```

### 6.2 훅 실행 순서 다이어그램

```
이벤트 수신
    │
    ▼
┌───────────────────────┐
│ OnMonitorEvent [0..N] │  ← 원시 이벤트 필터링/메트릭
│   stop=true → SKIP    │
└───────────┬───────────┘
            │
            ▼ Decode
            │
    ┌───────┴───────┐
    │ Flow?         │
    │   YES         │ NO
    ▼               ▼
┌────────────┐  ┌───────────────────────┐
│OnDecodedFlow│  │ (skip OnDecodedFlow)  │
│  [0..N]    │  └───────────┬───────────┘
│ stop→SKIP  │              │
└─────┬──────┘              │
      │                     │
      └──────────┬──────────┘
                 │
                 ▼
    ┌───────────────────────┐
    │ OnDecodedEvent [0..N] │  ← 모든 이벤트 타입에 적용
    │   stop=true → SKIP    │
    └───────────┬───────────┘
                │
                ▼
           ring.Write(ev)


GetFlows RPC 호출 시:

    ┌───────────────────────┐
    │ OnGetFlows [0..N]     │  ← context 수정 가능
    └───────────┬───────────┘
                │
                ▼
    ┌───────────────────────┐
    │ OnBuildFilter [0..N]  │  ← 커스텀 필터 추가
    └───────────┬───────────┘
                │
                ▼
         eventsReader.Next() loop
                │
                ▼
    ┌───────────────────────┐
    │ OnFlowDelivery [0..N] │  ← 전송 전 최종 필터/변환
    │   stop=true → SKIP    │
    └───────────┬───────────┘
                │
                ▼
           server.Send(resp)
```

### 6.3 훅 인터페이스와 함수 어댑터

각 훅은 인터페이스와 함수 어댑터 쌍으로 정의된다:

```go
// 소스: cilium/pkg/hubble/observer/observeroption/option.go

// 인터페이스 정의
type OnMonitorEvent interface {
    OnMonitorEvent(context.Context, *observerTypes.MonitorEvent) (stop, error)
}

// 함수 어댑터
type OnMonitorEventFunc func(context.Context, *observerTypes.MonitorEvent) (stop, error)

func (f OnMonitorEventFunc) OnMonitorEvent(
    ctx context.Context, event *observerTypes.MonitorEvent,
) (stop, error) {
    return f(ctx, event)
}
```

이 패턴은 Go 표준 라이브러리의 `http.HandlerFunc` 패턴과 동일하다:

```
인터페이스 구현체 ─┐
                   ├── 둘 다 Options에 등록 가능
함수 어댑터     ───┘

// 복잡한 상태를 가진 구조체는 인터페이스 직접 구현
type MetricsHook struct { registry *prometheus.Registry }
func (h *MetricsHook) OnDecodedFlow(ctx context.Context, flow *pb.Flow) (bool, error) { ... }

// 단순한 로직은 함수로 직접 등록
WithOnDecodedFlowFunc(func(ctx context.Context, flow *pb.Flow) (bool, error) {
    log.Info("flow observed", "src", flow.GetSource())
    return false, nil
})
```

### 6.4 stop 시맨틱

```go
type stop = bool  // 타입 별칭
```

| stop 값 | 의미 | 동작 |
|---------|------|------|
| `false` | 정상 진행 | 다음 훅 또는 다음 단계 실행 |
| `true` | 이벤트 건너뛰기 | `continue nextEvent`로 현재 이벤트 처리 중단 |

**중요**: `stop=true`와 `error != nil`은 독립적이다. 에러가 있어도 처리를 계속할 수 있고,
에러 없이도 이벤트를 건너뛸 수 있다.

```go
// 에러가 있어도 stop=false이면 계속 진행
stop, err := f.OnMonitorEvent(ctx, monitorEvent)
if err != nil {
    s.log.Info("failed in OnMonitorEvent", ...)
}
if stop {
    continue nextEvent  // 에러와 무관하게 stop만 확인
}
```

### 6.5 With* 옵션 함수들

각 훅에 대해 두 가지 등록 방법을 제공한다:

```go
// 인터페이스 기반 등록
func WithOnMonitorEvent(f OnMonitorEvent) Option {
    return func(o *Options) error {
        o.OnMonitorEvent = append(o.OnMonitorEvent, f)
        return nil
    }
}

// 함수 기반 등록 (편의 함수)
func WithOnMonitorEventFunc(
    f func(context.Context, *observerTypes.MonitorEvent) (stop, error),
) Option {
    return WithOnMonitorEvent(OnMonitorEventFunc(f))
}
```

| 훅 | With 함수 | WithFunc 함수 |
|----|-----------|--------------|
| OnServerInit | `WithOnServerInit` | `WithOnServerInitFunc` |
| OnMonitorEvent | `WithOnMonitorEvent` | `WithOnMonitorEventFunc` |
| OnDecodedFlow | `WithOnDecodedFlow` | `WithOnDecodedFlowFunc` |
| OnDecodedEvent | `WithOnDecodedEvent` | `WithOnDecodedEventFunc` |
| OnBuildFilter | `WithOnBuildFilter` | `WithOnBuildFilterFunc` |
| OnFlowDelivery | `WithOnFlowDelivery` | `WithOnFlowDeliveryFunc` |
| OnGetFlows | `WithOnGetFlows` | `WithOnGetFlowsFunc` |

---

## 7. RPC 구현: GetFlows

### 7.1 GetFlows 전체 흐름

`GetFlows`는 가장 핵심적인 gRPC 서버 스트리밍 RPC로, 클라이언트의 필터 조건에 맞는
Flow를 연속적으로 전송한다:

```go
// 소스: cilium/pkg/hubble/observer/local_observer.go

func (s *LocalObserverServer) GetFlows(
    req *observerpb.GetFlowsRequest,
    server observerpb.Observer_GetFlowsServer,
) (err error) {
    // 1. 요청 검증
    if err := validateRequest(req); err != nil { return err }

    ctx, cancel := context.WithCancel(server.Context())
    defer cancel()

    // 2. OnGetFlows 훅 실행
    for _, f := range s.opts.OnGetFlows {
        ctx, err = f.OnGetFlows(ctx, req)
        if err != nil { return err }
    }

    // 3. 필터 빌드
    log := s.GetLogger()
    filterList := append(filters.DefaultFilters(log), s.opts.OnBuildFilter...)
    whitelist, err := filters.BuildFilterList(ctx, req.Whitelist, filterList)
    blacklist, err := filters.BuildFilterList(ctx, req.Blacklist, filterList)

    // 4. RingReader 생성 (시작 위치 결정)
    ring := s.GetRingBuffer()
    ringReader, err := newRingReader(ring, req, whitelist, blacklist)

    // 5. EventsReader 생성 (필터링 + 시간 범위)
    eventsReader, err := newEventsReader(ringReader, req, log, whitelist, blacklist)

    // 6. FieldMask 설정
    mask, err := fieldmask.New(req.GetFieldMask())

    // 7. Lost Event Rate Limiter 설정
    lostEventCounter := counter.NewIntervalRangeCounter(s.opts.LostEventSendInterval)

    // 8. 이벤트 읽기 루프
    for {
        // 8a. Rate-limited lost event 전송
        if lostEventCounter.IsElapsed(now) { ... }

        // 8b. 다음 이벤트 읽기
        e, err := eventsReader.Next(ctx)

        // 8c. Flow 이벤트 처리
        switch ev := e.Event.(type) {
        case *flowpb.Flow:
            // OnFlowDelivery 훅
            // FieldMask 적용
            // 응답 생성 및 전송
        case *flowpb.LostEvent:
            // HubbleRingBuffer 소스면 rate-limit
            // 아니면 즉시 전송
        }

        server.Send(resp)
    }
}
```

### 7.2 요청 검증

```go
func validateRequest(req genericRequest) error {
    if req.GetFirst() && req.GetFollow() {
        return status.Errorf(codes.InvalidArgument,
            "first cannot be specified with follow")
    }
    return nil
}
```

`first`와 `follow`를 동시에 지정할 수 없는 이유:
- `first`: 링 버퍼의 가장 오래된 이벤트부터 읽기
- `follow`: 새 이벤트를 실시간으로 추적
- 두 옵션의 의미가 상충: "처음부터" + "끝에서 따라가기"

### 7.3 genericRequest 인터페이스

세 가지 RPC 요청을 추상화하는 인터페이스:

```go
// 소스: cilium/pkg/hubble/observer/local_observer.go

type genericRequest interface {
    GetNumber() uint64
    GetFollow() bool
    GetSince()  *timestamppb.Timestamp
    GetUntil()  *timestamppb.Timestamp
    GetFirst()  bool
}

var (
    _ genericRequest = (*observerpb.GetFlowsRequest)(nil)
    _ genericRequest = (*observerpb.GetAgentEventsRequest)(nil)
    _ genericRequest = (*observerpb.GetDebugEventsRequest)(nil)
)
```

이 추상화 덕분에 `newRingReader`, `newEventsReader`, `validateRequest` 등의 함수를
세 RPC에서 공유할 수 있다.

### 7.4 RingReader 시작 위치 결정

```go
func newRingReader(ring *container.Ring, req genericRequest,
    whitelist, blacklist filters.FilterFuncs,
) (*container.RingReader, error) {
    since := req.GetSince()

    // Case 1: --first (since 없이)
    if req.GetFirst() && since == nil {
        return container.NewRingReader(ring, ring.OldestWrite()), nil
    }

    // Case 2: --follow (number=0, since 없이)
    if req.GetFollow() && req.GetNumber() == 0 && since == nil {
        return container.NewRingReader(ring, ring.LastWriteParallel()), nil
    }

    // Case 3: 일반 조회 — 뒤에서부터 역방향 탐색
    idx := ring.LastWriteParallel()
    reader := container.NewRingReader(ring, idx)
    for i := ring.Len(); i > 0; i, idx = i-1, idx-1 {
        e, err := reader.Previous()
        // LostEvent(HubbleRingBuffer) 만나면 중단
        // since 이전이면 중단
        // eventCount == Number이면 중단
    }
    return container.NewRingReader(ring, idx), nil
}
```

시작 위치 결정 로직:

```
입력: --first, --follow, --since, --last N
      │
      ├── --first (since 없음)
      │   └── ring.OldestWrite() ← 가장 오래된 위치
      │
      ├── --follow (N=0, since 없음)
      │   └── ring.LastWriteParallel() ← 최신 위치 (새 이벤트만)
      │
      └── 그 외 (--last N, --since T)
          └── 역방향 스캔
              ├── LostEvent(HubbleRingBuffer) 만남 → 중단
              ├── timestamp < since → 중단 (1칸 앞으로)
              └── 필터 매칭 수 == N → 중단
```

**왜 역방향 스캔인가**: 링 버퍼의 최신 위치에서 과거로 거슬러 올라가면서 조건에 맞는
시작 지점을 찾는다. 이렇게 하면 실제로 이벤트를 버퍼링하지 않고도 정확한 시작 위치를
결정할 수 있다.

---

## 8. eventsReader: 필터링된 이벤트 읽기

### 8.1 구조체 정의

```go
// 소스: cilium/pkg/hubble/observer/local_observer.go

type eventsReader struct {
    ringReader           *container.RingReader
    whitelist, blacklist filters.FilterFuncs
    maxEvents            uint64
    follow, timeRange    bool
    since, until         *time.Time
    eventCount           uint64  // 호출자가 업데이트
}
```

### 8.2 Next() 메서드 분석

```go
func (r *eventsReader) Next(ctx context.Context) (*v1.Event, error) {
    for {
        // 컨텍스트 취소 확인
        select {
        case <-ctx.Done():
            return nil, ctx.Err()
        default:
        }

        var e *v1.Event
        if r.follow {
            e = r.ringReader.NextFollow(ctx)  // 블로킹: 새 이벤트 대기
        } else {
            if r.maxEvents > 0 && r.eventCount >= r.maxEvents {
                return nil, io.EOF  // 최대 이벤트 수 도달
            }
            e, err = r.ringReader.Next()  // 논블로킹: 다음 이벤트 읽기
        }

        if e == nil { return nil, io.EOF }

        // LostEvent는 필터/시간 범위 무시
        _, isLostEvent := e.Event.(*flowpb.LostEvent)
        if !isLostEvent {
            // 시간 범위 필터
            if r.timeRange {
                ts := e.Timestamp.AsTime()
                if r.until != nil && ts.After(*r.until) { return nil, io.EOF }
                if r.since != nil && ts.Before(*r.since) { continue }
            }
            // 화이트리스트/블랙리스트 필터
            if !filters.Apply(r.whitelist, r.blacklist, e) { continue }
        }

        return e, nil
    }
}
```

### 8.3 follow 모드 vs 일반 모드

```
일반 모드 (follow=false):
┌──────────────────────────────────────────┐
│ Ring Buffer [oldest ... newest]           │
│   ▲ start                    ▲ end       │
│   │ (newRingReader 결정)     │ (EOF)     │
│   └── Next() → Next() → ... → EOF       │
└──────────────────────────────────────────┘

follow 모드 (follow=true):
┌──────────────────────────────────────────┐
│ Ring Buffer [oldest ... newest → 새 이벤트]│
│                       ▲ start             │
│                       │ (LastWriteParallel)│
│                       └── NextFollow(ctx) │
│                           (블로킹 대기)    │
│                           ← ctx.Done()    │
└──────────────────────────────────────────┘
```

### 8.4 LostEvent 특수 처리

eventsReader에서 LostEvent를 특별 취급하는 이유:

1. **필터 우회**: LostEvent는 사용자가 명시적으로 요청하지 않는다. 어떤 필터 조건에서도
   LostEvent 정보를 전달해야 데이터 손실을 인지할 수 있다.

2. **시간 범위 우회**: LostEvent의 타임스탬프는 손실이 "감지된" 시점이지 실제 이벤트
   시점이 아니다. 따라서 "링 버퍼 타임스탬프가 단조 증가한다"는 가정이 LostEvent에는
   적용되지 않는다.

### 8.5 eventCount의 외부 관리

`eventsReader.eventCount`는 eventsReader가 아닌 **호출자(GetFlows)** 가 직접 증가시킨다:

```go
case *flowpb.Flow:
    eventsReader.eventCount++  // 호출자가 증가
```

이유: eventsReader는 반환하는 이벤트의 구체 타입을 알지 못한다. `Next()`가 반환하는
`v1.Event`는 Flow일 수도, AgentEvent일 수도 있다. "Flow 20개를 보내라"는 요청에서
LostEvent나 AgentEvent는 카운트에 포함하면 안 되므로, 카운트 책임을 호출자에게 위임한다.

---

## 9. GetAgentEvents / GetDebugEvents

### 9.1 GetAgentEvents

```go
// 소스: cilium/pkg/hubble/observer/local_observer.go

func (s *LocalObserverServer) GetAgentEvents(
    req *observerpb.GetAgentEventsRequest,
    server observerpb.Observer_GetAgentEventsServer,
) (err error) {
    // 1. validateRequest
    // 2. newRingReader (필터 없음)
    // 3. newEventsReader (whitelist/blacklist 없음)
    // 4. 이벤트 루프
    for {
        e, err := eventsReader.Next(ctx)
        switch ev := e.Event.(type) {
        case *flowpb.AgentEvent:
            eventsReader.eventCount++
            resp := &observerpb.GetAgentEventsResponse{
                Time:       e.Timestamp,
                NodeName:   nodeTypes.GetAbsoluteNodeName(),
                AgentEvent: ev,
            }
            server.Send(resp)
        }
    }
}
```

### 9.2 세 RPC의 비교

| 항목 | GetFlows | GetAgentEvents | GetDebugEvents |
|------|----------|---------------|----------------|
| 필터 | whitelist + blacklist | 없음 | 없음 |
| OnGetFlows 훅 | O | X | X |
| OnFlowDelivery 훅 | O | X | X |
| FieldMask | O | X | X |
| LostEvent 처리 | Rate-limited 전송 | 무시 | 무시 |
| 이벤트 타입 | `*flowpb.Flow` | `*flowpb.AgentEvent` | `*flowpb.DebugEvent` |
| 사용 빈도 | 매우 높음 | 낮음 (디버깅) | 낮음 (디버깅) |

**왜 GetAgentEvents/GetDebugEvents에는 필터가 없는가**: 이 이벤트들은 주로 디버깅 목적으로
사용되며, 이벤트 볼륨이 Flow에 비해 훨씬 적다. 복잡한 필터링보다는 전체 이벤트를 보는 것이
디버깅에 더 유용하다.

---

## 10. ServerStatus RPC

### 10.1 구현

```go
// 소스: cilium/pkg/hubble/observer/local_observer.go

func (s *LocalObserverServer) ServerStatus(
    ctx context.Context, req *observerpb.ServerStatusRequest,
) (*observerpb.ServerStatusResponse, error) {
    rate, err := getFlowRate(s.GetRingBuffer(), time.Now())
    if err != nil {
        s.log.Warn("Failed to get flow rate", logfields.Error, err)
    }
    return &observerpb.ServerStatusResponse{
        Version:   build.ServerVersion.String(),
        MaxFlows:  s.GetRingBuffer().Cap(),
        NumFlows:  s.GetRingBuffer().Len(),
        SeenFlows: s.numObservedFlows.Load(),
        UptimeNs:  uint64(time.Since(s.startTime).Nanoseconds()),
        FlowsRate: rate,
    }, nil
}
```

### 10.2 응답 필드 설명

| 필드 | 의미 | 데이터 소스 |
|------|------|------------|
| `Version` | Hubble 서버 버전 | `build.ServerVersion` |
| `MaxFlows` | 링 버퍼 최대 용량 | `ring.Cap()` = 4095 |
| `NumFlows` | 현재 저장된 이벤트 수 | `ring.Len()` |
| `SeenFlows` | 누적 관측 Flow 수 | `numObservedFlows.Load()` (atomic) |
| `UptimeNs` | 서버 가동 시간 (나노초) | `time.Since(startTime)` |
| `FlowsRate` | 초당 Flow 처리율 | `getFlowRate()` |

### 10.3 Flow Rate 계산 알고리즘

```go
func getFlowRate(ring *container.Ring, at time.Time) (float64, error) {
    reader := container.NewRingReader(ring, ring.LastWriteParallel())
    count := 0
    since := at.Add(-1 * time.Minute)  // 최근 1분

    for {
        e, err := reader.Previous()   // 과거로 스캔
        lost := e.GetLostEvent()

        if lost != nil && lost.Source == flowpb.LostEventSource_HUBBLE_RING_BUFFER {
            // 링 버퍼 전체를 읽음 → 사용 가능한 마지막 이벤트 시간 기준
            if lastSeenEvent != nil {
                since = lastSeenEvent.Timestamp.AsTime()
            }
            break
        }

        if _, isFlowEvent := e.Event.(*flowpb.Flow); !isFlowEvent {
            continue  // Flow가 아닌 이벤트 무시
        }

        ts := e.Timestamp.AsTime()
        if ts.Before(since) {
            break  // 1분 범위를 벗어남
        }
        lastSeenEvent = e
        count++
    }
    return float64(count) / at.Sub(since).Seconds(), nil
}
```

계산 방식:
1. 최신 이벤트부터 과거로 역방향 스캔
2. 최근 1분 또는 링 버퍼 전체 (더 짧은 쪽) 범위에서 Flow 수 집계
3. `Flow 수 / 시간(초)` = 초당 Flow 비율

**왜 1분 기준인가**: 너무 짧은 기간(1초)은 순간 변동이 크고, 너무 긴 기간(1시간)은
현재 상태를 반영하지 못한다. 1분은 적절한 스무딩 윈도우를 제공한다.

---

## 11. Lost Event Rate Limiting

### 11.1 문제

링 버퍼 오버플로우가 발생하면 매 이벤트마다 LostEvent가 생성될 수 있다. 이를
그대로 클라이언트에 전송하면:

1. 네트워크 대역폭 낭비
2. 클라이언트 측 로그/UI 오버플로우
3. 실제 Flow 데이터 전송 지연

### 11.2 해결: IntervalRangeCounter

```go
// GetFlows 내부
lostEventCounter := counter.NewIntervalRangeCounter(s.opts.LostEventSendInterval)

// 이벤트 루프
for {
    now := time.Now()

    // 주기적으로 누적된 lost event 전송
    if lostEventCounter.IsElapsed(now) {
        count := lostEventCounter.Clear()
        resp := &observerpb.GetFlowsResponse{
            Time: timestamppb.New(now),
            ResponseTypes: &observerpb.GetFlowsResponse_LostEvents{
                LostEvents: &flowpb.LostEvent{
                    Source:        flowpb.LostEventSource_HUBBLE_RING_BUFFER,
                    NumEventsLost: count.Count,
                    First:         timestamppb.New(count.First),
                    Last:          timestamppb.New(count.Last),
                },
            },
        }
        server.Send(resp)
    }

    e, err := eventsReader.Next(ctx)

    switch ev := e.Event.(type) {
    case *flowpb.LostEvent:
        switch ev.Source {
        case flowpb.LostEventSource_HUBBLE_RING_BUFFER:
            lostEventCounter.Increment(now)  // 누적만
        default:
            // 다른 소스는 즉시 전송
            resp = &observerpb.GetFlowsResponse{...}
        }
    }
}
```

### 11.3 Rate Limiting 전략

```
시간 →
t0        t1        t2        t3        t4
│ lost    │ lost    │         │ lost    │
│ lost    │ lost    │         │         │
│ lost    │         │         │         │
│         │         │         │         │
├─────────┤         ├─────────┤         │
│ Interval│         │ Interval│         │
│ 경과    │         │ 경과    │         │
│         │         │         │         │
│ Send:   │         │ Send:   │         │
│ count=5 │         │ count=1 │         │
│ first=t0│         │ first=t3│         │
│ last=t1 │         │ last=t3 │         │
└─────────┘         └─────────┘
```

- HubbleRingBuffer 소스의 LostEvent만 rate-limit
- 다른 소스(PerfRingBuffer, EventsQueue)는 즉시 전송 (더 가까운 소스에서 rate-limit 해야 함)
- 누적 정보에 Count, First, Last 시간을 포함하여 손실 규모와 기간을 전달

---

## 12. FieldMask 최적화

### 12.1 목적

클라이언트가 Flow의 일부 필드만 필요한 경우, 전체 Flow 대신 요청된 필드만 복사하여
네트워크 대역폭과 직렬화 비용을 절약한다.

### 12.2 구현

```go
// GetFlows 내부

fm := req.GetFieldMask()
mask, err := fieldmask.New(fm)

var flow *flowpb.Flow
if mask.Active() {
    flow = new(flowpb.Flow)
    mask.Alloc(flow.ProtoReflect())  // 필요한 필드만 미리 할당
}

// 이벤트 루프 내부
case *flowpb.Flow:
    if mask.Active() {
        mask.Copy(flow.ProtoReflect(), ev.ProtoReflect())
        ev = flow  // 마스크 적용된 Flow 사용
    }
```

### 12.3 FieldMask 동작

```
클라이언트 요청: field_mask = ["source.namespace", "destination.namespace", "verdict"]

원본 Flow:                        마스크 적용 후:
┌─────────────────────┐          ┌─────────────────────┐
│ Time: ...           │          │ Time: (zero)        │
│ Source:             │          │ Source:             │
│   Namespace: "ns-a" │    →     │   Namespace: "ns-a" │
│   Labels: [...]     │          │   Labels: (nil)     │
│   PodName: "pod-1"  │          │   PodName: ""       │
│ Destination:        │          │ Destination:        │
│   Namespace: "ns-b" │          │   Namespace: "ns-b" │
│   Labels: [...]     │          │   Labels: (nil)     │
│ Verdict: FORWARDED  │          │ Verdict: FORWARDED  │
│ Type: L3_L4         │          │ Type: (zero)        │
│ Summary: "..."      │          │ Summary: ""         │
└─────────────────────┘          └─────────────────────┘
```

**왜 Flow를 미리 할당하는가**: `flow = new(flowpb.Flow)`와 `mask.Alloc()`은 이벤트 루프
밖에서 한 번만 실행된다. 루프 내부에서는 같은 `flow` 객체에 `mask.Copy()`로 덮어쓰기만
하므로 GC 압력을 최소화한다.

---

## 13. 네임스페이스 관리

### 13.1 namespaceManager 구조

```go
// 소스: cilium/pkg/hubble/observer/namespace/manager.go

type Manager interface {
    GetNamespaces() []*observerpb.Namespace
    AddNamespace(*observerpb.Namespace)
}

type namespaceManager struct {
    mu         lock.RWMutex
    namespaces map[string]namespaceRecord   // key: "cluster/namespace"
    nowFunc    func() time.Time
}

type namespaceRecord struct {
    namespace *observerpb.Namespace
    added     time.Time
}
```

### 13.2 Flow에서 네임스페이스 추출

```go
// 소스: cilium/pkg/hubble/observer/local_observer.go

func (s *LocalObserverServer) trackNamespaces(flow *flowpb.Flow) {
    if srcNs := flow.GetSource().GetNamespace(); srcNs != "" {
        s.nsManager.AddNamespace(&observerpb.Namespace{
            Namespace: srcNs,
            Cluster:   nodeTypes.GetClusterName(),
        })
    }
    if dstNs := flow.GetDestination().GetNamespace(); dstNs != "" {
        s.nsManager.AddNamespace(&observerpb.Namespace{
            Namespace: dstNs,
            Cluster:   nodeTypes.GetClusterName(),
        })
    }
}
```

이벤트 루프의 OnDecodedFlow 훅 실행 전에 `trackNamespaces`를 호출한다. 이는 훅이
네임스페이스 정보를 활용할 수 있도록 보장한다.

### 13.3 TTL 기반 정리

```go
const namespaceTTL = 1 * time.Hour

func (m *namespaceManager) cleanupNamespaces() {
    m.mu.Lock()
    defer m.mu.Unlock()
    for key, record := range m.namespaces {
        if record.added.Add(namespaceTTL).Before(m.nowFunc()) {
            delete(m.namespaces, key)
        }
    }
}
```

- **TTL = 1시간**: 네임스페이스가 삭제되었거나 더 이상 트래픽이 없으면 1시간 후 자동 제거
- **AddNamespace로 갱신**: 같은 네임스페이스의 Flow가 관측될 때마다 `added` 타임스탬프가
  갱신되므로, 활성 네임스페이스는 영구적으로 유지

### 13.4 정렬된 네임스페이스 반환

```go
func (m *namespaceManager) GetNamespaces() []*observerpb.Namespace {
    m.mu.RLock()
    namespaces := make([]*observerpb.Namespace, 0, len(m.namespaces))
    for _, ns := range m.namespaces {
        namespaces = append(namespaces, ns.namespace)
    }
    m.mu.RUnlock()

    sort.Slice(namespaces, func(i, j int) bool {
        a := namespaces[i]
        b := namespaces[j]
        if a.Cluster != b.Cluster {
            return a.Cluster < b.Cluster  // 클러스터 우선 정렬
        }
        return a.Namespace < b.Namespace  // 네임스페이스 이름 정렬
    })
    return namespaces
}
```

**왜 정렬하는가**:
1. `GetNamespaces` API 응답의 일관성 보장 (같은 상태에서 항상 같은 순서)
2. 멀티클러스터 환경에서 클러스터별 그룹핑
3. CLI(`hubble list namespaces`)에서 사용자에게 깔끔한 출력 제공

### 13.5 키 구조

```go
func (m *namespaceManager) AddNamespace(ns *observerpb.Namespace) {
    key := ns.GetCluster() + "/" + ns.GetNamespace()
    m.namespaces[key] = namespaceRecord{namespace: ns, added: m.nowFunc()}
}
```

`"cluster/namespace"` 형태의 복합 키를 사용하여:
- 같은 이름의 네임스페이스가 다른 클러스터에 존재할 수 있음
- 단일 클러스터에서는 `"local-cluster/default"` 같은 형태

---

## 14. Default 옵션과 설정

### 14.1 기본값

```go
// 소스: cilium/pkg/hubble/observer/observeroption/defaults.go

var Default = Options{
    MaxFlows:      container.Capacity4095,  // 4095
    MonitorBuffer: 1024,
}
```

### 14.2 왜 이 기본값인가

| 설정 | 기본값 | 이유 |
|------|--------|------|
| MaxFlows | 4095 | 2^12-1, 링 버퍼는 2^n-1 크기만 허용 (mask 연산 최적화) |
| MonitorBuffer | 1024 | BPF 이벤트 버스트를 흡수할 수 있는 충분한 버퍼 크기 |

### 14.3 DefaultOptions 패턴

```go
// 소스: cilium/pkg/hubble/observer/local_observer.go

var DefaultOptions []observeroption.Option

func NewLocalServer(..., options ...observeroption.Option) {
    opts := observeroption.Default
    options = append(options, DefaultOptions...)
    for _, opt := range options {
        opt(&opts)
    }
}
```

`DefaultOptions`는 패키지 수준 변수로, 다른 패키지가 `init()` 함수에서 기본 옵션을
추가할 수 있다. 이를 통해:

1. Cilium 메트릭 패키지가 메트릭 훅을 기본 등록
2. Policy 패키지가 정책 적용 훅을 기본 등록
3. 코어 observer 코드는 이런 의존성을 알 필요 없음

```
패키지 초기화 순서:
metrics/init() → DefaultOptions = append(DefaultOptions, WithOnDecodedFlow(metricsHook))
policy/init()  → DefaultOptions = append(DefaultOptions, WithOnMonitorEvent(policyHook))
                        │
                        ▼
observer.NewLocalServer() → options = append(options, DefaultOptions...)
```

---

## 15. 동시성 모델

### 15.1 고루틴 구조

```
┌─────────────────────────────────────────────────────┐
│                   Cilium Agent                       │
│                                                      │
│  Goroutine 1: BPF Event Producer                    │
│    monitorEvent → events channel (buffered 1024)    │
│                                                      │
│  Goroutine 2: LocalObserverServer.Start()           │
│    events channel → decode → hooks → ring.Write()   │
│    (단일 소비자: 순차 처리)                            │
│                                                      │
│  Goroutine 3..N: GetFlows RPC Handlers              │
│    ring.Read() → filter → server.Send()             │
│    (다수의 동시 읽기 가능)                              │
│                                                      │
│  Goroutine N+1: ServerStatus RPC                    │
│    ring.Cap(), ring.Len(), numObservedFlows.Load()  │
│    (읽기 전용, lock-free)                             │
└─────────────────────────────────────────────────────┘
```

### 15.2 동기화 메커니즘

| 자원 | 쓰기 | 읽기 | 동기화 방식 |
|------|------|------|------------|
| events channel | BPF producer | Start() 루프 | buffered channel |
| ring buffer | Start() 루프 | GetFlows handlers | atomic + cycle detection |
| numObservedFlows | Start() 루프 | ServerStatus | atomic.Uint64 |
| nsManager | trackNamespaces | GetNamespaces | RWMutex |

### 15.3 단일 소비자 패턴의 장점

이벤트 루프(Start)는 단일 고루틴에서 실행된다:

1. **훅 실행 순서 보장**: OnMonitorEvent → Decode → OnDecodedFlow → OnDecodedEvent → Write
   순서가 항상 보장된다.

2. **상태 공유 불필요**: 훅들이 동시에 실행되지 않으므로 훅 내부에서 뮤텍스가 필요 없다.

3. **링 버퍼 쓰기 직렬화**: ring.Write()가 단일 고루틴에서만 호출되므로 쓰기 경쟁이
   없다 (ring은 내부적으로 atomic을 사용하지만 단일 쓰기자 가정).

4. **배압(Backpressure)**: events 채널이 가득 차면 BPF producer가 블록되어 자연스러운
   배압이 형성된다.

---

## 16. 성능 고려사항

### 16.1 버퍼 크기 튜닝

```
BPF perf ring → events channel(1024) → Start() → ring(4095) → GetFlows
     ↑                  ↑                              ↑
     │                  │                              │
     │     MonitorBuffer 증가 시:           MaxFlows 증가 시:
     │     - 메모리 사용 증가                - 더 오래된 이벤트 보존
     │     - 버스트 흡수 향상                - 메모리 사용 증가
     │     - LostEvent(EventsQueue) 감소     - 조회 범위 확대
     │
     BPF perf buffer 증가 시:
     - 커널 메모리 사용 증가
     - LostEvent(PerfRingBuffer) 감소
```

### 16.2 핫 패스(Hot Path) 최적화

이벤트 루프의 각 이벤트 처리는 핫 패스로, 다음 최적화가 적용되어 있다:

1. **atomic 카운터**: `numObservedFlows`에 mutex 대신 atomic 사용
2. **에러 타입 분기**: `switch/case`로 알려진 에러만 빠르게 처리
3. **조건부 로깅**: `log.Enabled(slog.LevelDebug)` 체크 후 로그 구성
4. **FieldMask 사전 할당**: 이벤트 루프 밖에서 한 번만 할당
5. **LostEvent Rate Limiting**: 모든 Lost Event를 개별 전송하지 않고 집계

### 16.3 메모리 관리

```go
// FieldMask Flow 재사용
var flow *flowpb.Flow
if mask.Active() {
    flow = new(flowpb.Flow)       // 루프 밖에서 1회 할당
    mask.Alloc(flow.ProtoReflect())
}

// 루프 내부
mask.Copy(flow.ProtoReflect(), ev.ProtoReflect())  // 기존 객체에 복사
ev = flow  // 새 할당 없이 포인터만 교체
```

이 패턴으로 GetFlows가 수백만 개의 Flow를 스트리밍해도 추가 할당이 거의 없다.

---

## 17. 확장 가능성과 플러그인 패턴

### 17.1 메트릭 훅 예시

Cilium에서 Hubble 메트릭은 OnDecodedFlow 훅으로 구현된다:

```go
// 개념적 구현 (실제 코드는 cilium/pkg/hubble/metrics/)

type MetricsHandler struct {
    flowsProcessed prometheus.Counter
    flowDuration   prometheus.Histogram
}

func (h *MetricsHandler) OnDecodedFlow(
    ctx context.Context, flow *pb.Flow,
) (bool, error) {
    h.flowsProcessed.Inc()
    // duration, verdict, protocol 등의 레이블로 메트릭 기록
    return false, nil  // stop=false: 다음 훅 실행 계속
}
```

### 17.2 정책 적용 훅 예시

```go
// 개념적 구현

type PolicyHook struct{}

func (h *PolicyHook) OnMonitorEvent(
    ctx context.Context, ev *types.MonitorEvent,
) (bool, error) {
    // 특정 조건의 이벤트를 사전 필터링
    if shouldSkip(ev) {
        return true, nil  // stop=true: 이 이벤트 건너뛰기
    }
    return false, nil
}
```

### 17.3 커스텀 필터 훅

```go
// OnBuildFilter를 통한 서버 측 커스텀 필터 추가
type CustomFilter struct{}

func (f *CustomFilter) OnBuildFilter(
    ctx context.Context, ff *flowpb.FlowFilter,
) ([]filters.FilterFunc, error) {
    // ff에 커스텀 필드가 있으면 해당 필터 함수 반환
    return nil, nil
}
```

---

## 18. 요약

Observer Pipeline은 Hubble의 핵심 데이터 처리 경로로, 다음과 같은 설계 원칙을 따른다:

| 원칙 | 구현 |
|------|------|
| 단일 책임 | 수집 → 디코딩 → 훅 → 저장 → 조회가 명확히 분리 |
| 개방-폐쇄 | 7개 훅 포인트로 코어 코드 수정 없이 확장 가능 |
| Lock-free | Ring Buffer + atomic으로 쓰기/읽기 동시성 확보 |
| 배압 관리 | buffered channel + Rate Limiting으로 과부하 방지 |
| 메모리 효율 | FieldMask 사전 할당 + LostEvent 집계로 GC 압력 최소화 |

핵심 흐름:
```
BPF → events(1024) → [OnMonitorEvent] → Decode → [OnDecodedFlow]
                                                → [OnDecodedEvent]
                                                → ring.Write()
                                                          │
                                                          ▼
GetFlows ← [OnFlowDelivery] ← [filter] ← eventsReader ← ring
```

이 파이프라인 구조 덕분에 Hubble은 Cilium 에이전트 내부에서 최소한의 오버헤드로
초당 수천~수만 개의 네트워크 이벤트를 처리하고, 실시간 스트리밍 또는 히스토리 조회를
동시에 지원할 수 있다.
