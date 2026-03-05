# 13. Observability (Hubble) 서브시스템

## 목차

1. [개요](#1-개요)
2. [이벤트 파이프라인 전체 흐름](#2-이벤트-파이프라인-전체-흐름)
3. [BPF 이벤트 발생](#3-bpf-이벤트-발생)
4. [Monitor Agent](#4-monitor-agent)
5. [Monitor Consumer](#5-monitor-consumer)
6. [Hubble Observer](#6-hubble-observer)
7. [Ring Buffer](#7-ring-buffer)
8. [이벤트 파서](#8-이벤트-파서)
9. [gRPC 서버](#9-grpc-서버)
10. [Hubble Relay](#10-hubble-relay)
11. [메트릭 시스템](#11-메트릭-시스템)
12. [Hook 시스템](#12-hook-시스템)
13. [왜 이 아키텍처인가?](#13-왜-이-아키텍처인가)
14. [참고 파일 목록](#14-참고-파일-목록)

---

## 1. 개요

Cilium의 Observability 서브시스템은 **Hubble**이라는 이름으로 구현되어 있다.
Hubble은 BPF 데이터패스에서 발생하는 패킷 이벤트를 커널 공간에서 사용자 공간으로 전달하고,
이를 구조화된 Flow 데이터로 파싱한 뒤, gRPC API를 통해 외부 클라이언트에 제공한다.

핵심 설계 원칙은 다음과 같다:

| 원칙 | 설명 |
|------|------|
| **제로 카피에 가까운 전달** | BPF perf event array를 통해 커널에서 직접 사용자 공간으로 이벤트 전달 |
| **Lock-free 링 버퍼** | atomic 연산 기반의 사용자 공간 링 버퍼로 여러 gRPC 스트림이 동시 읽기 가능 |
| **플러그인 기반 메트릭** | DNS, HTTP, TCP, Drop 등 프로토콜별 Prometheus 메트릭 핸들러 |
| **Hook 체인** | OnMonitorEvent/OnDecodedFlow/OnDecodedEvent 등 확장 가능한 이벤트 처리 체인 |
| **멀티 노드 집계** | Hubble Relay가 클러스터 전체 노드의 흐름을 우선순위 큐로 정렬하여 제공 |

---

## 2. 이벤트 파이프라인 전체 흐름

BPF 프로그램에서 생성된 이벤트가 최종 gRPC 클라이언트에 도달하기까지의 전체 파이프라인이다.

```
+--------------------------------------------------------------+
|                     BPF Datapath (커널 공간)                    |
|                                                              |
|  send_trace_notify()   send_drop_notify()                    |
|         |                      |                             |
|         v                      v                             |
|  +--------------------------------------------------+        |
|  |          cilium_events (BPF_MAP_TYPE_PERF_EVENT)  |        |
|  |          - Per-CPU 링 버퍼                          |        |
|  |          - Rate limiting (토큰 버킷)                 |        |
|  +--------------------------------------------------+        |
+-------------------------------|------------------------------ +
                                |
                    perf.Reader.Read()
                                |
                                v
+--------------------------------------------------------------+
|                  Monitor Agent (사용자 공간)                     |
|                                                              |
|  agent.handleEvents() --- 루프 ---+                           |
|       |                          |                           |
|       v                          v                           |
|  processPerfRecord()        processPerfRecord()              |
|       |                                                      |
|       +----> notifyPerfEventLocked() -- consumers에 전달      |
|       +----> sendToListenersLocked() -- listeners에 전달      |
+-------------------------------|------------------------------ +
                                |
                    NotifyPerfEvent(data, cpu)
                                |
                                v
+--------------------------------------------------------------+
|                  Monitor Consumer                             |
|                                                              |
|  consumer.sendEvent()                                        |
|       |                                                      |
|       +---> observer.GetEventsChannel() <- MonitorEvent       |
|       |     (채널이 가득 차면 lost event 카운터 증가)              |
+-------------------------------|------------------------------ +
                                |
                    chan *MonitorEvent
                                |
                                v
+--------------------------------------------------------------+
|              LocalObserverServer.Start()                      |
|                                                              |
|  for monitorEvent := range events {                          |
|      OnMonitorEvent hooks  ----+                             |
|      payloadParser.Decode()    |                             |
|      OnDecodedFlow hooks   ----+                             |
|      OnDecodedEvent hooks  ----+                             |
|      ring.Write(ev)                                          |
|  }                                                           |
+-------------------------------|------------------------------ +
                                |
                         Ring Buffer
                        (atomic 기반)
                                |
              +-----------------+-----------------+
              |                 |                 |
              v                 v                 v
        GetFlows()       GetAgentEvents()  GetDebugEvents()
        (gRPC 스트림)     (gRPC 스트림)      (gRPC 스트림)
              |
              v
     +-------------------+
     |   Hubble Relay     |
     | (멀티 노드 집계)     |
     | - PeerManager       |
     | - Priority Queue    |
     +-------------------+
              |
              v
     hubble CLI / UI / Grafana
```

이벤트가 커널에서 최종 클라이언트까지 도달하는 과정에서 3곳에서 유실이 발생할 수 있다:

| 유실 지점 | 원인 | 소스 상수 |
|-----------|------|----------|
| BPF perf ring buffer | 커널 버퍼가 가득 차서 덮어쓰기 | `LostEventSourcePerfRingBuffer` (1) |
| Monitor events queue | 채널 버퍼가 가득 참 | `LostEventSourceEventsQueue` (2) |
| Hubble ring buffer | 읽기 전에 쓰기가 추월 | `LostEventSourceHubbleRingBuffer` (3) |

이 상수들은 `pkg/hubble/observer/types/types.go`에 정의되어 있다:

```go
// pkg/hubble/observer/types/types.go (lines 12-26)
const (
    LostEventSourceUnspec           = iota
    LostEventSourcePerfRingBuffer        // perf event ring buffer 덮어쓰기
    LostEventSourceEventsQueue           // events queue 가득 참
    LostEventSourceHubbleRingBuffer      // hubble ring buffer 읽기 실패
)
```

---

## 3. BPF 이벤트 발생

### 3.1 관찰 지점 (Observation Points)

BPF 데이터패스는 패킷이 Cilium 내부를 이동하는 동안 14개의 관찰 지점에서 이벤트를 발생시킨다.
이 열거형은 `bpf/lib/notify.h` (lines 50-66)에 정의되어 있다:

```c
// bpf/lib/notify.h (lines 50-66)
enum trace_point {
    TRACE_POINT_UNKNOWN = -1,
    TRACE_TO_LXC,          // 0  - 엔드포인트(컨테이너)로 전달
    TRACE_TO_PROXY,        // 1  - L7 프록시로 전달
    TRACE_TO_HOST,         // 2  - 호스트 네트워크 스택으로 전달
    TRACE_TO_STACK,        // 3  - 커널 네트워크 스택으로 전달
    TRACE_TO_OVERLAY,      // 4  - 오버레이(VXLAN/Geneve)로 전달
    TRACE_FROM_LXC,        // 5  - 엔드포인트에서 수신
    TRACE_FROM_PROXY,      // 6  - L7 프록시에서 수신
    TRACE_FROM_HOST,       // 7  - 호스트에서 수신
    TRACE_FROM_STACK,      // 8  - 커널 스택에서 수신
    TRACE_FROM_OVERLAY,    // 9  - 오버레이에서 수신
    TRACE_FROM_NETWORK,    // 10 - 외부 네트워크에서 수신
    TRACE_TO_NETWORK,      // 11 - 외부 네트워크로 전달
    TRACE_FROM_CRYPTO,     // 12 - WireGuard 복호화 후
    TRACE_TO_CRYPTO,       // 13 - WireGuard 암호화 전
} __packed;
```

이 관찰 지점들을 패킷 흐름 다이어그램으로 표현하면:

```
                          TRACE_FROM_NETWORK
                                |
                                v
+-------+    TRACE_FROM_LXC   +---------+   TRACE_TO_HOST    +------+
|  Pod  | -----------------> | Cilium  | -----------------> | Host |
| (LXC) | <----------------- | BPF     | <----------------- |      |
+-------+    TRACE_TO_LXC     |Programs |   TRACE_FROM_HOST  +------+
                               |         |
                               |         |   TRACE_TO_OVERLAY  +----------+
                               |         | ------------------> | Overlay  |
                               |         | <------------------ | (VXLAN/  |
                               |         |  TRACE_FROM_OVERLAY | Geneve)  |
                               |         |                     +----------+
                               |         |
                               |         |   TRACE_TO_PROXY    +-------+
                               |         | ------------------> | L7    |
                               |         | <------------------ | Proxy |
                               |         |  TRACE_FROM_PROXY   +-------+
                               |         |
                               |         |   TRACE_TO_CRYPTO   +-----------+
                               |         | ------------------> | WireGuard |
                               |         | <------------------ | cilium_wg0|
                               +---------+  TRACE_FROM_CRYPTO  +-----------+
                                    |
                                    | TRACE_TO_NETWORK
                                    v
                              외부 네트워크
```

### 3.2 이벤트 타입

BPF에서 발생하는 이벤트 타입은 `pkg/monitor/api/types.go` (lines 20-55)에 정의되어 있다:

| 상수 | 값 | 설명 | BPF 소스 |
|------|----|------|----------|
| `MessageTypeDrop` | 1 | 패킷 드롭 | `bpf/lib/drop.h` |
| `MessageTypeDebug` | 2 | 디버그 메시지 | `bpf/lib/dbg.h` |
| `MessageTypeCapture` | 3 | 패킷 캡처 | `bpf/lib/dbg.h` |
| `MessageTypeTrace` | 4 | 패킷 추적 (관찰 지점 통과) | `bpf/lib/trace.h` |
| `MessageTypePolicyVerdict` | 5 | 정책 판정 결과 | `bpf/lib/policy_log.h` |
| `MessageTypeTraceSock` | 7 | 소켓 레벨 추적 | `bpf/lib/trace_sock.h` |
| `MessageTypeAccessLog` | 129 | L7 프록시 접근 로그 | 에이전트 레벨 |
| `MessageTypeAgent` | 130 | 에이전트 알림 | 에이전트 레벨 |

```go
// pkg/monitor/api/types.go (lines 20-55)
const (
    MessageTypeUnspec = iota        // 0
    MessageTypeDrop                 // 1 - BPF datapath
    MessageTypeDebug                // 2 - BPF datapath
    MessageTypeCapture              // 3 - BPF datapath
    MessageTypeTrace                // 4 - BPF datapath
    MessageTypePolicyVerdict        // 5 - BPF datapath

    MessageTypeTraceSock     = 7    // BPF datapath

    // 129-255: agent level events
    MessageTypeAccessLog     = 129  // L7 proxy log
    MessageTypeAgent         = 130  // agent notification
)
```

0~128은 BPF 데이터패스 이벤트용으로 예약되고, 129~255는 에이전트 레벨 이벤트용이다.
이 분리를 통해 커널 공간 이벤트와 사용자 공간 이벤트를 동일한 파이프라인으로 처리하면서도
타입으로 구분할 수 있다.

### 3.3 cilium_events perf event array

모든 BPF 이벤트의 출구는 단일 perf event array이다.
`bpf/lib/events.h` (lines 8-13)에 정의되어 있다:

```c
// bpf/lib/events.h (lines 8-13)
struct {
    __uint(type, BPF_MAP_TYPE_PERF_EVENT_ARRAY);
    __uint(key_size, sizeof(__u32));
    __uint(value_size, sizeof(__u32));
    __uint(pinning, LIBBPF_PIN_BY_NAME);
} cilium_events __section_maps_btf;
```

`BPF_MAP_TYPE_PERF_EVENT_ARRAY`는 CPU별로 독립된 링 버퍼를 제공한다.
`pinning = LIBBPF_PIN_BY_NAME`이므로 BPF 파일시스템에 고정되어
사용자 공간에서 이름으로 찾아 열 수 있다.

### 3.4 Trace Notify 구조체

패킷 추적 이벤트의 실제 데이터 구조는 `pkg/monitor/datapath_trace.go` (lines 50-68)에 정의된다:

```go
// pkg/monitor/datapath_trace.go (lines 50-68)
type TraceNotify struct {
    Type       uint8                    // 이벤트 타입 (CILIUM_NOTIFY_TRACE = 4)
    ObsPoint   uint8                    // 관찰 지점 (TRACE_TO_LXC 등)
    Source     uint16                   // 소스 엔드포인트 ID
    Hash       uint32                   // 패킷 해시 (flow 식별용)
    OrigLen    uint32                   // 원본 패킷 길이
    CapLen     uint16                   // 캡처된 길이
    Version    uint8                    // 알림 버전 (0, 1, 2)
    ExtVersion uint8                    // 확장 버전
    SrcLabel   identity.NumericIdentity // 소스 보안 아이덴티티
    DstLabel   identity.NumericIdentity // 목적지 보안 아이덴티티
    DstID      uint16                   // 목적지 엔드포인트 ID
    Reason     uint8                    // 전달 사유 (CT 상태 등)
    Flags      uint8                    // IPv6/L3Device/VXLAN/Geneve 플래그
    Ifindex    uint32                   // 네트워크 인터페이스 인덱스
    OrigIP     types.IPv6               // NAT 전 원본 IP
    IPTraceID  uint64                   // IP 트레이스 ID (v2)
}
```

버전별로 크기가 다르다:

| 버전 | 크기 | 추가된 필드 |
|------|------|-----------|
| v0 | 32바이트 | 기본 필드만 |
| v1 | 48바이트 | OrigIP (NAT 전 원본 IP) |
| v2 | 56바이트 | IPTraceID (엔드투엔드 추적 ID) |

이 구조체의 `Reason` 필드에는 Connection Tracking 상태가 인코딩된다:

```go
// pkg/monitor/datapath_trace.go (lines 206-218)
const (
    TraceReasonPolicy             = iota // 새 연결 (정책에 의해)
    TraceReasonCtEstablished             // 기존 연결
    TraceReasonCtReply                   // 응답 패킷
    TraceReasonCtRelated                 // 관련 연결
    TraceReasonCtDeprecatedReopened      // (사용 중단)
    TraceReasonUnknown                   // 알 수 없음
    TraceReasonSRv6Encap                 // SRv6 캡슐화
    TraceReasonSRv6Decap                 // SRv6 역캡슐화
    TraceReasonEncryptMask = uint8(0x80) // 암호화 비트마스크
)
```

### 3.5 Drop Notify 구조체

패킷 드롭 이벤트의 데이터 구조는 `pkg/monitor/datapath_drop.go` (lines 66-86)에 정의된다:

```go
// pkg/monitor/datapath_drop.go (lines 66-86)
type DropNotify struct {
    Type       uint8                    // CILIUM_NOTIFY_DROP = 1
    SubType    uint8                    // 드롭 사유 코드
    Source     uint16                   // 소스 엔드포인트 ID
    Hash       uint32                   // 패킷 해시
    OrigLen    uint32                   // 원본 패킷 길이
    CapLen     uint16                   // 캡처된 길이
    Version    uint8                    // 알림 버전
    ExtVersion uint8                    // 확장 버전
    SrcLabel   identity.NumericIdentity // 소스 아이덴티티
    DstLabel   identity.NumericIdentity // 목적지 아이덴티티
    DstID      uint32                   // 목적지 엔드포인트 ID
    Line       uint16                   // 드롭 발생 소스 라인 번호
    File       uint8                    // 드롭 발생 소스 파일 번호
    ExtError   int8                     // 확장 에러 코드
    Ifindex    uint32                   // 네트워크 인터페이스 인덱스
    Flags      uint8                    // 플래그
    IPTraceID  uint64                   // IP 트레이스 ID (v3)
}
```

`Line`과 `File` 필드가 특징적이다. 드롭이 발생한 BPF C 소스코드의 정확한 위치를
기록하여 디버깅을 돕는다. `__MAGIC_LINE__`과 `__MAGIC_FILE__` 매크로가 이를 자동 삽입한다.

### 3.6 Rate Limiting

BPF 이벤트 발생 시 토큰 버킷 기반의 레이트 리미팅이 적용된다.
`bpf/lib/trace.h` (lines 237-242)에서 확인할 수 있다:

```c
// bpf/lib/trace.h (lines 237-242)
if (EVENTS_MAP_RATE_LIMIT > 0) {
    settings.bucket_size = EVENTS_MAP_BURST_LIMIT;
    settings.tokens_per_topup = EVENTS_MAP_RATE_LIMIT;
    if (!ratelimit_check_and_take(&rkey, &settings))
        return;
}
```

토큰이 소진되면 이벤트는 조용히 버려진다. 이는 높은 트래픽 상황에서 perf 버퍼 오버플로우를
방지하는 첫 번째 방어선이다. 또한 Monitor Aggregation 레벨에 따라 수신 방향 이벤트를
필터링할 수 있다:

```c
// bpf/lib/trace.h (lines 51-56)
enum {
    TRACE_AGGREGATE_NONE = 0,      // 모든 패킷 추적
    TRACE_AGGREGATE_RX = 1,        // 수신 추적 숨기기
    TRACE_AGGREGATE_ACTIVE_CT = 3, // 활성 연결 추적 제한
};
```

---

## 4. Monitor Agent

Monitor Agent는 BPF perf event array에서 이벤트를 읽어
등록된 consumers와 listeners에 분배하는 중앙 허브 역할을 한다.

### 4.1 Agent 인터페이스

`pkg/monitor/agent/agent.go` (lines 44-52)에 정의된 인터페이스:

```go
// pkg/monitor/agent/agent.go (lines 44-52)
type Agent interface {
    AttachToEventsMap(nPages int) error              // perf 맵 연결
    SendEvent(typ int, event any) error              // 사용자 공간 이벤트 주입
    RegisterNewListener(newListener MonitorListener) // 외부 모니터 클라이언트
    RemoveListener(ml MonitorListener)
    RegisterNewConsumer(newConsumer MonitorConsumer)  // 내부 소비자 (Hubble)
    RemoveConsumer(mc MonitorConsumer)
    State() *models.MonitorStatus                    // 현재 상태 조회
}
```

두 종류의 구독자를 지원한다:

| 구독자 타입 | 용도 | 데이터 형식 |
|------------|------|-----------|
| **Listener** | 외부 `cilium monitor` 클라이언트 | gob 인코딩된 payload |
| **Consumer** | 내부 Hubble Observer | 디코딩된 메시지 (구조체) |

### 4.2 agent 구조체

```go
// pkg/monitor/agent/agent.go (lines 64-81)
type agent struct {
    logger *slog.Logger
    lock.Mutex
    models.MonitorStatus

    ctx              context.Context
    perfReaderCancel context.CancelFunc

    listeners map[listener.MonitorListener]struct{} // 외부 클라이언트
    consumers map[consumer.MonitorConsumer]struct{} // 내부 소비자

    events        *ebpf.Map      // cilium_events perf map
    monitorEvents *perf.Reader   // perf reader 인스턴스
}
```

### 4.3 Perf Reader 생명 주기

Perf reader는 **구독자가 있을 때만** 실행되는 lazy start 패턴을 사용한다:

```
구독자 없음          첫 구독자 등록           마지막 구독자 제거
    |                    |                       |
    v                    v                       v
  [IDLE]  ------->  startPerfReaderLocked()  --> perfReaderCancel()
                    go handleEvents()            perf reader 정지
                         |
                         v
                   monitorEvents.Read() 루프
```

이 설계의 이유: perf reader가 동작하지 않으면 커널은 해당 perf 버퍼에 이벤트를
쓰지 않으므로, 아무도 이벤트를 소비하지 않을 때 불필요한 커널-사용자 공간
데이터 전달 오버헤드를 제거한다.

### 4.4 handleEvents 루프

`pkg/monitor/agent/agent.go` (lines 324-372):

```go
func (a *agent) handleEvents(stopCtx context.Context) {
    bufferSize := int(a.Pagesize * a.Npages)
    monitorEvents, err := perf.NewReader(a.events, bufferSize)
    // ...
    for !isCtxDone(stopCtx) {
        record, err := monitorEvents.Read()  // 블로킹 읽기
        switch {
        case isCtxDone(stopCtx):
            return
        case err != nil:
            // 알 수 없는 이벤트 타입 → Unknown 카운터 증가
            // EBADFD → reader 종료
            continue
        }
        a.processPerfRecord(record)
    }
}
```

### 4.5 processPerfRecord

`pkg/monitor/agent/agent.go` (lines 376-397):

```go
func (a *agent) processPerfRecord(record perf.Record) {
    a.Lock()
    defer a.Unlock()

    if record.LostSamples > 0 {
        // perf 버퍼 오버플로우 → lost event 전파
        a.MonitorStatus.Lost += int64(record.LostSamples)
        a.notifyPerfEventLostLocked(record.LostSamples, record.CPU)
        a.sendToListenersLocked(&payload.Payload{
            CPU:  record.CPU,
            Lost: record.LostSamples,
            Type: payload.RecordLost,
        })
    } else {
        // 정상 이벤트 → consumers + listeners에 전달
        a.notifyPerfEventLocked(record.RawSample, record.CPU)
        a.sendToListenersLocked(&payload.Payload{
            Data: record.RawSample,
            CPU:  record.CPU,
            Type: payload.EventSample,
        })
    }
}
```

처리 흐름:

```
perf.Record
    |
    +-- LostSamples > 0 ?
    |       |
    |       YES --> MonitorStatus.Lost += N
    |       |       notifyPerfEventLostLocked()  --> 모든 consumer에 전달
    |       |       sendToListenersLocked()       --> 모든 listener에 전달
    |       |
    |       NO  --> notifyPerfEventLocked()       --> 모든 consumer에 전달
    |               sendToListenersLocked()       --> 모든 listener에 전달
```

---

## 5. Monitor Consumer

Monitor Consumer는 Monitor Agent와 Hubble Observer 사이의 브릿지 역할을 한다.
perf event를 `MonitorEvent`로 래핑하고, observer의 events 채널에 전달한다.

### 5.1 consumer 구조체

`pkg/hubble/monitor/consumer.go` (lines 33-44):

```go
// pkg/hubble/monitor/consumer.go (lines 33-44)
type consumer struct {
    uuider   *bufuuid.Generator  // 이벤트 UUID 생성기
    observer Observer             // Hubble Observer 참조

    lostLock         lock.Mutex
    lostEventCounter *counter.IntervalRangeCounter  // lost event 합산 카운터
    logLimiter       logging.Limiter                // 로그 제한기

    metricLostPerfEvents     prometheus.Counter      // perf 유실 메트릭
    metricLostObserverEvents prometheus.Counter      // 큐 유실 메트릭
}
```

### 5.2 NotifyPerfEvent

`pkg/hubble/monitor/consumer.go` (lines 73-80):

```go
func (c *consumer) NotifyPerfEvent(data []byte, cpu int) {
    c.sendEvent(func() any {
        return &observerTypes.PerfEvent{
            Data: data,
            CPU:  cpu,
        }
    })
}
```

### 5.3 sendEvent와 backpressure 처리

`pkg/hubble/monitor/consumer.go` (lines 97-109):

```go
func (c *consumer) sendEvent(payloader func() any) {
    c.lostLock.Lock()
    defer c.lostLock.Unlock()

    now := time.Now()
    c.trySendLostEventLocked(now)  // 이전에 누적된 lost event 전송 시도

    select {
    case c.observer.GetEventsChannel() <- c.newEvent(now, payloader):
        // 성공: 이벤트 전달
    default:
        // 채널 가득 참: lost event 카운터 증가
        c.incrementLostEventLocked(now)
    }
}
```

이 패턴이 중요한 이유:

1. **비블로킹 전송**: `select` + `default` 패턴으로 채널이 가득 차면 즉시 반환한다.
   Monitor Agent의 perf reader 루프를 블로킹하지 않는다.

2. **Lost event 합산**: 개별 유실마다 이벤트를 보내면 오히려 혼잡이 가중되므로,
   `IntervalRangeCounter`를 사용하여 설정된 간격(기본 1초)마다 합산된 유실 카운터를 한 번에 전송한다.

3. **우선순위 전송**: `trySendLostEventLocked()`가 새 이벤트보다 먼저 호출되어,
   이전에 누적된 유실 정보가 새 데이터보다 우선 전달된다.

```
                  sendEvent() 호출
                       |
                       v
              trySendLostEventLocked()
              lost event 누적분이 있고
              전송 간격이 지났으면 전송 시도
                       |
                       v
              select {
              case ch <- newEvent:
                  // 성공
              default:
                  // 채널 가득 참
                  incrementLostEventLocked()
                  // 카운터 증가 + Prometheus 메트릭 기록
              }
```

### 5.4 MonitorEvent 타입

consumer가 생성하는 MonitorEvent는 `pkg/hubble/observer/types/types.go` (lines 28-68)에 정의되어 있다:

```go
// MonitorEvent는 observer가 소비하는 최상위 이벤트 타입
type MonitorEvent struct {
    UUID      uuid.UUID   // 고유 식별자
    Timestamp time.Time   // 수신 시각
    NodeName  string      // 발생 노드명
    Payload   any         // AgentEvent | PerfEvent | LostEvent
}

type PerfEvent struct {   // BPF perf 링 버퍼에서 온 원시 데이터
    Data []byte
    CPU  int
}

type AgentEvent struct {  // 에이전트 레벨 이벤트
    Type    int           // MessageType* 값
    Message any           // accesslog.LogRecord, AgentNotifyMessage 등
}

type LostEvent struct {   // 유실 알림
    Source        int     // 유실 발생 위치
    NumLostEvents uint64
    CPU           int
    First         time.Time  // 첫 유실 시각
    Last          time.Time  // 마지막 유실 시각
}
```

---

## 6. Hubble Observer

LocalObserverServer는 Hubble의 핵심 엔진으로, MonitorEvent를 수신하여 파싱하고,
링 버퍼에 저장한 뒤, gRPC API로 제공한다.

### 6.1 LocalObserverServer 구조체

`pkg/hubble/observer/local_observer.go` (lines 44-70):

```go
type LocalObserverServer struct {
    ring          *container.Ring                    // 링 버퍼
    events        chan *observerTypes.MonitorEvent    // 입력 채널
    stopped       chan struct{}                       // 종료 시그널
    log           *slog.Logger
    payloadParser parser.Decoder                     // 이벤트 파서
    opts          observeroption.Options             // Hook 옵션들
    startTime     time.Time
    numObservedFlows atomic.Uint64                   // 관찰된 flow 수
    nsManager     namespace.Manager                  // 네임스페이스 관리
}
```

### 6.2 Start() - 이벤트 처리 루프

`pkg/hubble/observer/local_observer.go` (lines 116-197)의 Start()는 Observer의 핵심 루프이다:

```go
func (s *LocalObserverServer) Start() {
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

nextEvent:
    for monitorEvent := range s.GetEventsChannel() {
        // 1단계: OnMonitorEvent hooks 실행
        for _, f := range s.opts.OnMonitorEvent {
            stop, err := f.OnMonitorEvent(ctx, monitorEvent)
            if stop { continue nextEvent }
        }

        // 2단계: 이벤트 디코딩
        ev, err := s.payloadParser.Decode(monitorEvent)
        if err != nil {
            // ErrUnknownEventType, ErrEventSkipped 등은 무시
            continue
        }

        // 3단계: Flow인 경우 OnDecodedFlow hooks 실행
        if flow, ok := ev.Event.(*flowpb.Flow); ok {
            s.trackNamespaces(flow)
            for _, f := range s.opts.OnDecodedFlow {
                stop, err := f.OnDecodedFlow(ctx, flow)
                if stop { continue nextEvent }
            }
            s.numObservedFlows.Add(1)
        }

        // 4단계: OnDecodedEvent hooks 실행
        for _, f := range s.opts.OnDecodedEvent {
            stop, err := f.OnDecodedEvent(ctx, ev)
            if stop { continue nextEvent }
        }

        // 5단계: 링 버퍼에 기록
        s.GetRingBuffer().Write(ev)
    }
    close(s.GetStopped())
}
```

이벤트 처리 파이프라인을 시각화하면:

```
MonitorEvent
    |
    v
[OnMonitorEvent hooks]  -- 필터링/로깅/전처리
    | (stop? --> skip)
    v
[payloadParser.Decode()] -- 원시 데이터 -> v1.Event 변환
    | (error? --> skip)
    v
[Flow인가?]
    |         |
   YES       NO
    |         |
    v         |
[trackNamespaces]   |
    |               |
    v               |
[OnDecodedFlow hooks]   -- 메트릭/드롭이벤트/로컬노드정보
    | (stop? --> skip)  |
    v                   |
    +-------------------+
    |
    v
[OnDecodedEvent hooks]  -- 내보내기(export)
    | (stop? --> skip)
    v
[ring.Write(ev)]  -- 링 버퍼에 기록
```

### 6.3 GetFlows() - gRPC 스트리밍 API

`pkg/hubble/observer/local_observer.go` (lines 260-428)에 구현된 GetFlows()는
클라이언트가 gRPC 스트림으로 Flow를 요청할 때 호출된다:

```go
func (s *LocalObserverServer) GetFlows(
    req *observerpb.GetFlowsRequest,
    server observerpb.Observer_GetFlowsServer,
) error {
    // 1. 필터 빌드 (whitelist + blacklist)
    whitelist, _ := filters.BuildFilterList(ctx, req.Whitelist, ...)
    blacklist, _ := filters.BuildFilterList(ctx, req.Blacklist, ...)

    // 2. 링 리더 생성 (시작 위치 계산)
    ringReader, _ := newRingReader(ring, req, whitelist, blacklist)

    // 3. 이벤트 리더 생성 (since/until/number/follow 처리)
    eventsReader, _ := newEventsReader(ringReader, req, ...)

    // 4. FieldMask 설정 (필요한 필드만 전송)
    mask, _ := fieldmask.New(req.GetFieldMask())

    // 5. 이벤트 읽기 루프
    for {
        e, err := eventsReader.Next(ctx)
        // ...

        switch ev := e.Event.(type) {
        case *flowpb.Flow:
            // OnFlowDelivery hooks 실행
            // FieldMask 적용
            // gRPC 응답 전송
        case *flowpb.LostEvent:
            // Hubble ring buffer 유실은 rate-limit하여 전송
            // 다른 소스의 유실은 즉시 전송
        }
    }
}
```

GetFlows()의 주요 기능:

| 기능 | 설명 |
|------|------|
| **Whitelist/Blacklist** | FlowFilter로 IP, Pod, Label, Verdict 등으로 필터링 |
| **Since/Until** | 시간 범위 지정 |
| **Number** | 최대 반환 이벤트 수 |
| **Follow** | 실시간 스트리밍 (tail -f 유사) |
| **First** | 링 버퍼 시작부터 읽기 |
| **FieldMask** | protobuf 필드 마스크로 필요한 필드만 전송 |
| **Lost event rate-limiting** | Hubble 링 버퍼 유실을 합산하여 전송 |

---

## 7. Ring Buffer

Hubble의 링 버퍼는 lock-free 설계로 단일 writer와 다수 reader를 지원한다.

### 7.1 Ring 구조체

`pkg/hubble/container/ring.go` (lines 86-110):

```go
type Ring struct {
    mask      uint64           // 인덱스 계산용 비트마스크
    write     atomic.Uint64    // 마지막 쓰기 위치 (atomic)
    cycleExp  uint8            // 2^x 승수 (사이클 계산용)
    cycleMask uint64           // 사이클 마스크
    halfCycle uint64           // 총 사이클의 절반
    dataLen   uint64           // 내부 버퍼 길이
    data      []*v1.Event      // 실제 데이터 배열

    notifyMu lock.Mutex        // 알림용 뮤텍스
    notifyCh chan struct{}      // 대기 중인 reader 깨우기
}
```

### 7.2 용량 설계: 2^n - 1

링 버퍼의 용량은 반드시 `2^n - 1` 형태여야 한다 (1, 3, 7, 15, ..., 65535).
`pkg/hubble/container/ring.go` (lines 47-64)에서 가능한 용량이 열거되어 있다:

```go
const (
    Capacity1     capacity = 1<<(iota+1) - 1  // 1
    Capacity3                                  // 3
    Capacity7                                  // 7
    // ...
    Capacity4095                               // 4095
    // ...
    Capacity65535                              // 65535
)
```

**왜 2^n - 1인가?**

실제 내부 버퍼 크기는 `capacity + 1 = 2^n`이다. 1 슬롯은 쓰기 전용으로 예약되어
writer와 reader의 충돌을 방지한다. 이렇게 하면:

- `mask = capacity` (모든 비트가 1)
- `index = write & mask` 연산으로 O(1) 인덱스 계산
- 나눗셈 없이 비트 시프트만으로 사이클 번호 계산: `cycle = write >> cycleExp`

```
dataLen = 8 (2^3), capacity = 7 (2^3 - 1), mask = 0x7, cycleExp = 3

write:  0  1  2  3  4  5  6  7  8  9  10 11 12 13 14 15 ...
index:  0  1  2  3  4  5  6  7  0  1   2  3  4  5  6  7 ...
cycle:  0  0  0  0  0  0  0  0  1  1   1  1  1  1  1  1 ...
```

### 7.3 Write() - 쓰기 연산

`pkg/hubble/container/ring.go` (lines 168-190):

```go
func (r *Ring) Write(entry *v1.Event) {
    r.notifyMu.Lock()

    write := r.write.Add(1)          // atomic 증가
    writeIdx := (write - 1) & r.mask // 비트마스크로 인덱스 계산
    r.dataStoreAtomic(writeIdx, entry)

    // 대기 중인 reader 깨우기
    if r.notifyCh != nil {
        close(r.notifyCh)    // 채널 닫기로 모든 대기자에게 알림
        r.notifyCh = nil
    }

    r.notifyMu.Unlock()
}
```

`notifyMu` 뮤텍스는 쓰기 자체를 보호하는 것이 아니라(쓰기는 atomic),
**reader가 잠들기 직전의 경쟁 조건**을 방지한다:

```
문제 시나리오 (뮤텍스 없이):
  Reader: lastWrite 확인 → 새 데이터 없음 → (여기서 writer가 쓰기) → 알림 놓침 → sleep
  Writer: write → notify → (reader가 아직 sleep 안 했으므로 알림 무효)

해결:
  Reader: notifyMu.Lock() → lastWrite 확인 → notifyCh 획득 → Unlock() → select(notifyCh)
  Writer: notifyMu.Lock() → write → close(notifyCh) → Unlock()
```

### 7.4 readFrom() - 읽기 연산과 사이클 감지

`pkg/hubble/container/ring.go` (lines 297-398)에 구현된 readFrom()은
reader가 writer를 따라잡거나 뒤처지는 4가지 상황을 처리한다:

```
              +----------------유효한 읽기 범위-----------+  +현재 쓰기 중
              |                                          |  |  +다음 쓰기 위치
              V                                          V  V  V
write: f0 f1 f2 f3 f4 f5 f6 f7 f8 f9 fa fb fc fd fe ff  0  1  2  3 ...
index:  0  1  2  3  4  5  6  7  8  9  a  b  c  d  e  f  0  1  2  3 ...
cycle: 1f 1f 1f 1f 1f 1f 1f 1f 1f 1f 1f 1f 1f 1f 1f 1f  0  0  0  0 ...
```

4가지 케이스:

| 케이스 | 조건 | 동작 |
|--------|------|------|
| **이전 사이클, 유효** | readCycle == writeCycle-1, readIdx > lastWriteIdx | 정상 읽기 |
| **현재 사이클, 유효** | readCycle == writeCycle, readIdx < lastWriteIdx | 정상 읽기 |
| **Reader가 Writer 따라잡음** | readCycle >= writeCycle, 근처 | 새 쓰기 대기 (notifyCh) |
| **Reader가 뒤처짐** | Writer가 Reader를 추월 | LostEvent 발생 |

reader가 writer를 따라잡은 경우의 대기 메커니즘:

```go
// reader가 writer를 따라잡은 경우
r.notifyMu.Lock()
if lastWrite != r.write.Load()-1 {
    // 쓰기가 발생 → 재시도
    r.notifyMu.Unlock()
    read--
    continue
}
if r.notifyCh == nil {
    r.notifyCh = make(chan struct{})
}
notifyCh := r.notifyCh
r.notifyMu.Unlock()

select {
case <-notifyCh:     // Writer가 close(notifyCh)로 깨움
    read--
    continue
case <-ctx.Done():   // 컨텍스트 취소
    return
}
```

---

## 8. 이벤트 파서

Parser는 원시 MonitorEvent를 구조화된 v1.Event(protobuf Flow)로 변환한다.

### 8.1 Parser 구조체

`pkg/hubble/parser/parser.go` (lines 37-42):

```go
type Parser struct {
    l34  *threefour.Parser  // L3/L4 파서 (gopacket 사용)
    l7   *seven.Parser      // L7 파서 (DNS/HTTP/Kafka)
    dbg  *debug.Parser      // 디버그 이벤트 파서
    sock *sock.Parser       // 소켓 이벤트 파서
}
```

### 8.2 Decode() - 이벤트 타입별 분기

`pkg/hubble/parser/parser.go` (lines 100-204)의 Decode() 메서드:

```go
func (p *Parser) Decode(monitorEvent *observerTypes.MonitorEvent) (*v1.Event, error) {
    ev := &v1.Event{
        Timestamp: timestamppb.New(monitorEvent.Timestamp),
    }

    switch payload := monitorEvent.Payload.(type) {
    case *observerTypes.PerfEvent:
        // BPF perf 이벤트 → 첫 바이트로 타입 결정
        switch payload.Data[0] {
        case monitorAPI.MessageTypeDebug:
            dbg, _ := p.dbg.Decode(payload.Data, payload.CPU)
            ev.Event = dbg
        case monitorAPI.MessageTypeTraceSock:
            p.sock.Decode(payload.Data, flow)
        default:
            // Trace, Drop, Capture, PolicyVerdict → L3/L4 파서
            p.l34.Decode(payload.Data, flow)
        }

    case *observerTypes.AgentEvent:
        switch payload.Type {
        case monitorAPI.MessageTypeAccessLog:
            // L7 프록시 로그 → L7 파서
            logrecord := payload.Message.(accesslog.LogRecord)
            p.l7.Decode(&logrecord, flow)
        case monitorAPI.MessageTypeAgent:
            // 에이전트 알림 → AgentEvent protobuf 변환
            ev.Event = agent.NotifyMessageToProto(agentNotifyMessage)
        }

    case *observerTypes.LostEvent:
        // 유실 이벤트 → LostEvent protobuf 변환
        ev.Event = &pb.LostEvent{
            Source:        lostEventSourceToProto(payload.Source),
            NumEventsLost: payload.NumLostEvents,
        }
    }
    return ev, nil
}
```

이벤트 타입과 파서의 매핑:

```
MonitorEvent.Payload
    |
    +-- PerfEvent (BPF 원시 데이터)
    |       |
    |       +-- data[0] == 2 (Debug)       --> debug.Parser
    |       +-- data[0] == 7 (TraceSock)   --> sock.Parser
    |       +-- data[0] == 1 (Drop)        --> threefour.Parser (L3/L4)
    |       +-- data[0] == 4 (Trace)       --> threefour.Parser (L3/L4)
    |       +-- data[0] == 3 (Capture)     --> threefour.Parser (L3/L4)
    |       +-- data[0] == 5 (PolicyVerdict) --> threefour.Parser (L3/L4)
    |
    +-- AgentEvent (사용자 공간 이벤트)
    |       |
    |       +-- type == 129 (AccessLog)    --> seven.Parser (L7)
    |       +-- type == 130 (Agent)        --> agent.NotifyMessageToProto
    |
    +-- LostEvent (유실 알림)
            |
            +--> LostEvent protobuf 직접 생성
```

### 8.3 L3/L4 Parser (threefour)

`pkg/hubble/parser/threefour/parser.go` (lines 31-48):

```go
type Parser struct {
    log            *slog.Logger
    endpointGetter getters.EndpointGetter    // 엔드포인트 정보 조회
    identityGetter getters.IdentityGetter    // 보안 아이덴티티 조회
    dnsGetter      getters.DNSGetter         // DNS 이름 역방향 조회
    ipGetter       getters.IPGetter          // IP -> 메타데이터 조회
    serviceGetter  getters.ServiceGetter     // 서비스 정보 조회
    linkGetter     getters.LinkGetter        // 네트워크 인터페이스 이름 조회

    dropNotifyDecoder          options.DropNotifyDecoderFunc
    traceNotifyDecoder         options.TraceNotifyDecoderFunc
    policyVerdictNotifyDecoder options.PolicyVerdictNotifyDecoderFunc
    debugCaptureDecoder        options.DebugCaptureDecoderFunc
    packetDecoder              options.L34PacketDecoder

    epResolver          *common.EndpointResolver
    correlateL3L4Policy bool
}
```

gopacket을 사용하여 실제 패킷 헤더를 디코딩한다. 지원하는 프로토콜 레이어:

- Ethernet, IPv4, IPv6
- TCP, UDP, SCTP
- ICMPv4, ICMPv6
- VRRPv2, IGMPv1or2
- VXLAN, Geneve (오버레이)

getter 인터페이스를 통해 IP 주소를 Pod 이름, 서비스 이름, DNS 이름 등
풍부한 메타데이터로 변환한다. 이것이 Hubble이 단순한 패킷 캡처가 아닌
**Kubernetes-aware 관찰 도구**인 이유이다.

### 8.4 L7 Parser (seven)

`pkg/hubble/parser/seven/parser.go` (lines 31-40):

```go
type Parser struct {
    log               *slog.Logger
    timestampCache    *lru.Cache[string, time.Time]       // 타임스탬프 캐시
    traceContextCache *lru.Cache[string, *flowpb.TraceContext]  // 트레이스 컨텍스트 캐시
    dnsGetter         getters.DNSGetter
    ipGetter          getters.IPGetter
    serviceGetter     getters.ServiceGetter
    endpointGetter    getters.EndpointGetter
    opts              *options.Options
}
```

L7 파서는 프록시에서 전달된 `accesslog.LogRecord`를 파싱하여 다음 프로토콜을 지원한다:

| 프로토콜 | 정보 |
|----------|------|
| **DNS** | 쿼리 이름, 응답 코드, 응답 IP |
| **HTTP** | 메서드, URL, 상태 코드, 헤더 |
| **Kafka** | 토픽, 파티션, 오프셋 |

LRU 캐시를 사용하여 동일 연결의 요청/응답 상관관계(correlation)를 추적한다.

---

## 9. gRPC 서버

Hubble은 두 개의 gRPC 서버를 실행한다.

### 9.1 서버 구성

`pkg/hubble/cell/hubbleintegration.go` (lines 208-377)의 `launch()` 함수에서
두 서버가 설정된다:

```
+------------------+     +------------------+
| UNIX Domain Socket|     | TCP Server       |
| unix:///var/run/  |     | :4244            |
| cilium/hubble.sock|     | (Relay가 연결)     |
+------------------+     +------------------+
| - InsecureLocal   |     | - TLS/mTLS 지원   |
| - cilium Pod 내부 |     | - 외부 접근 가능    |
| - 디버깅/트러블슈팅 |     | - Relay가 사용     |
+------------------+     +------------------+
        |                         |
        +----------+--------------+
                   |
                   v
        LocalObserverServer
         (공통 Observer)
```

**UNIX 소켓 서버** (lines 289-315):

```go
sockPath := "unix://" + h.config.SocketPath  // unix:///var/run/cilium/hubble.sock
localSrvOpts = append(localSrvOpts,
    serveroption.WithUnixSocketListener(h.log, sockPath),
    serveroption.WithHealthService(),
    serveroption.WithObserverService(hubbleObserver),
    serveroption.WithPeerService(h.peerService),
    serveroption.WithInsecure(),  // 로컬이므로 TLS 불필요
    // ...
)
```

**TCP 서버** (lines 323-374):

```go
address := h.config.ListenAddress  // 기본 :4244
options := []serveroption.Option{
    serveroption.WithTCPListener(address),
    serveroption.WithObserverService(hubbleObserver),
    // TLS 설정 ...
}
```

### 9.2 Monitor Consumer 등록

`launch()` (line 282)에서 consumer를 Monitor Agent에 등록하여
이벤트 파이프라인을 연결한다:

```go
go hubbleObserver.Start()
h.monitorAgent.RegisterNewConsumer(
    monitor.NewConsumer(hubbleObserver, h.config.LostEventSendInterval),
)
```

이 한 줄이 전체 파이프라인의 연결 고리이다:

```
monitorAgent ---RegisterNewConsumer---> consumer ---events chan---> hubbleObserver
```

### 9.3 gRPC API

Observer 서비스가 제공하는 주요 RPC:

| RPC | 설명 | 스트리밍 |
|-----|------|----------|
| `GetFlows` | Flow 이벤트 조회/구독 | Server streaming |
| `GetAgentEvents` | 에이전트 이벤트 조회 | Server streaming |
| `GetDebugEvents` | 디버그 이벤트 조회 | Server streaming |
| `GetNodes` | 노드 목록 조회 | Unary |
| `GetNamespaces` | 네임스페이스 목록 조회 | Unary |
| `ServerStatus` | 서버 상태 조회 | Unary |

---

## 10. Hubble Relay

Hubble Relay는 클러스터의 모든 노드에서 실행되는 Hubble 서버에 연결하여
클러스터 전체의 Flow를 집계한다.

### 10.1 Relay Server 구조체

`pkg/hubble/relay/server/server.go` (lines 52-59):

```go
type Server struct {
    server           *grpc.Server
    grpcHealthServer *grpc.Server
    pm               *pool.PeerManager    // 피어 연결 관리
    healthServer     *healthServer
    metricsServer    *http.Server
    opts             options
}
```

### 10.2 PeerManager

PeerManager는 Cilium Peer Service를 통해 클러스터의 모든 Hubble 노드를 발견하고
gRPC 연결 풀을 관리한다:

```
+------------------+
|  Hubble Relay     |
|                  |
|  PeerManager     |
|    |             |
|    +-- Node A ---> gRPC conn ---> Hubble Server (Node A)
|    +-- Node B ---> gRPC conn ---> Hubble Server (Node B)
|    +-- Node C ---> gRPC conn ---> Hubble Server (Node C)
|    +-- Node D ---> gRPC conn ---> Hubble Server (Node D)
|                  |
+------------------+
```

### 10.3 retrieveFlowsFromPeer

`pkg/hubble/relay/observer/observer.go` (lines 37-65):

```go
func retrieveFlowsFromPeer(
    ctx context.Context,
    client observerpb.ObserverClient,
    req *observerpb.GetFlowsRequest,
    flows chan<- *observerpb.GetFlowsResponse,
) error {
    c, err := client.GetFlows(ctx, req)
    // ...
    for {
        flow, err := c.Recv()
        // ...
        select {
        case flows <- flow:
        case <-ctx.Done():
            return nil
        }
    }
}
```

각 피어 노드에 대해 goroutine을 생성하여 병렬로 Flow를 수집한다.
수집된 Flow는 공유 채널로 전달된다.

### 10.4 sortFlows - 우선순위 큐 기반 정렬

`pkg/hubble/relay/observer/observer.go` (lines 67-119):

```go
func sortFlows(
    ctx context.Context,
    flows <-chan *observerpb.GetFlowsResponse,
    qlen int,
    bufferDrainTimeout time.Duration,
) <-chan *observerpb.GetFlowsResponse {
    pq := queue.NewPriorityQueue(qlen)
    sortedFlows := make(chan *observerpb.GetFlowsResponse, qlen)

    go func() {
        defer close(sortedFlows)
        for {
            select {
            case flow, ok := <-flows:
                if !ok { break }
                if pq.Len() == qlen {
                    f := pq.Pop()         // 가장 오래된 flow 출력
                    sortedFlows <- f
                }
                pq.Push(flow)             // 새 flow 삽입
            case t := <-time.After(bufferDrainTimeout):
                // 타임아웃 시 오래된 flow 드레인
                for _, f := range pq.PopOlderThan(t.Add(-bufferDrainTimeout)) {
                    sortedFlows <- f
                }
            }
        }
        // 큐 드레인
        for f := pq.Pop(); f != nil; f = pq.Pop() {
            sortedFlows <- f
        }
    }()
    return sortedFlows
}
```

정렬 메커니즘:

```
Node A: [t=1] [t=4] [t=7] ...
Node B: [t=2] [t=5] [t=8] ...    --+
Node C: [t=3] [t=6] [t=9] ...      |
                                     v
                            +-----------------+
                            | Priority Queue  |
                            | (타임스탬프 정렬)  |
                            +-----------------+
                                     |
                                     v
                            정렬된 출력:
                            [t=1] [t=2] [t=3] [t=4] [t=5] ...
```

**왜 우선순위 큐인가?**

각 노드의 Hubble 서버가 보내는 Flow는 노드 내에서는 시간순이지만,
노드 간에는 순서가 보장되지 않는다. 네트워크 지연, 처리 속도 차이 등으로
Node B의 t=2 이벤트가 Node A의 t=4 이벤트보다 늦게 도착할 수 있다.

우선순위 큐는 `qlen` 크기의 정렬 윈도우를 제공하여, 이 윈도우 내에서
타임스탬프 기준 정렬을 수행한다. `bufferDrainTimeout`은 새 이벤트가 없을 때
큐에 잔류하는 오래된 이벤트를 강제로 출력하는 역할을 한다.

---

## 11. 메트릭 시스템

Hubble은 플러그인 기반의 Prometheus 메트릭 시스템을 제공한다.

### 11.1 StaticFlowProcessor

`pkg/hubble/metrics/flow_processor.go` (lines 18-52):

```go
type StaticFlowProcessor struct {
    logger  *slog.Logger
    metrics []api.NamedHandler  // 등록된 메트릭 핸들러 목록
}

func (p *StaticFlowProcessor) OnDecodedFlow(ctx context.Context, flow *flowpb.Flow) (bool, error) {
    var errs error
    for _, nh := range p.metrics {
        errs = errors.Join(errs, nh.Handler.ProcessFlow(ctx, flow))
    }
    return false, nil  // stop = false: 다음 hook도 실행
}
```

StaticFlowProcessor는 `OnDecodedFlow` hook으로 등록되어,
모든 디코딩된 Flow에 대해 등록된 메트릭 핸들러를 순서대로 실행한다.
하나의 핸들러 실패가 다른 핸들러에 영향을 주지 않도록 `errors.Join`으로 에러를 합산한다.

### 11.2 메트릭 플러그인

사용 가능한 메트릭 플러그인들:

| 플러그인 | 디렉토리 | 주요 메트릭 |
|----------|----------|-----------|
| **flow** | `metrics/flow/` | `hubble_flows_processed_total` |
| **dns** | `metrics/dns/` | DNS 쿼리/응답 메트릭 |
| **drop** | `metrics/drop/` | 패킷 드롭 사유별 메트릭 |
| **http** | `metrics/http/` | HTTP 요청 지연/상태코드 메트릭 |
| **tcp** | `metrics/tcp/` | TCP 연결 상태/플래그 메트릭 |
| **icmp** | `metrics/icmp/` | ICMP 메시지 타입 메트릭 |
| **sctp** | `metrics/sctp/` | SCTP 패킷 메트릭 |
| **policy** | `metrics/policy/` | 정책 판정 메트릭 |
| **port-distribution** | `metrics/port-distribution/` | 포트 분포 메트릭 |
| **flows-to-world** | `metrics/flows-to-world/` | 외부 트래픽 메트릭 |

### 11.3 Flow Handler 예시

`pkg/hubble/metrics/flow/handler.go` (lines 22-108)의 flowHandler:

```go
type flowHandler struct {
    flows     *prometheus.CounterVec
    context   *api.ContextOptions
    AllowList filters.FilterFuncs
    DenyList  filters.FilterFuncs
}

func (h *flowHandler) Init(registry *prometheus.Registry, options *api.MetricConfig) error {
    labels := []string{"protocol", "type", "subtype", "verdict"}
    labels = append(labels, h.context.GetLabelNames()...)

    h.flows = prometheus.NewCounterVec(prometheus.CounterOpts{
        Namespace: api.DefaultPrometheusNamespace,  // "hubble"
        Name:      "flows_processed_total",
        Help:      "Total number of flows processed",
    }, labels)

    registry.MustRegister(h.flows)
    return nil
}

func (h *flowHandler) ProcessFlow(ctx context.Context, flow *flowpb.Flow) error {
    // 이벤트 타입별 분류
    switch flow.GetEventType().GetType() {
    case monitorAPI.MessageTypeAccessLog:
        typeName = "L7"
        // DNS/HTTP/Kafka 세부 분류
    case monitorAPI.MessageTypeDrop:
        typeName = "Drop"
    case monitorAPI.MessageTypeTrace:
        typeName = "Trace"
        subType = monitorAPI.TraceObservationPoints[...]
    case monitorAPI.MessageTypePolicyVerdict:
        typeName = "PolicyVerdict"
    }

    h.flows.WithLabelValues(protocol, typeName, subType, verdict).Inc()
    return nil
}
```

결과 Prometheus 메트릭 예시:

```
hubble_flows_processed_total{protocol="TCP",type="Trace",subtype="to-endpoint",verdict="FORWARDED"} 42
hubble_flows_processed_total{protocol="UDP",type="L7",subtype="DNS",verdict="FORWARDED"} 15
hubble_flows_processed_total{protocol="TCP",type="Drop",subtype="",verdict="DROPPED"} 3
```

### 11.4 메트릭 파이프라인 통합

`pkg/hubble/cell/hubbleintegration.go` (lines 262-266)에서 메트릭 프로세서가
Observer hook으로 등록된다:

```go
if h.metricsFlowProcessor != nil {
    observerOpts = append(observerOpts,
        observeroption.WithOnDecodedFlowFunc(func(ctx context.Context, f *flowpb.Flow) (bool, error) {
            return false, h.metricsFlowProcessor.ProcessFlow(ctx, f)
        }),
    )
}
```

---

## 12. Hook 시스템

Hubble Observer의 확장성의 핵심은 Hook 시스템이다.

### 12.1 Hook 타입

`pkg/hubble/observer/observeroption/option.go` (lines 27-39)에 정의된 Options:

```go
type Options struct {
    MaxFlows              container.Capacity
    MonitorBuffer         int
    LostEventSendInterval time.Duration

    OnServerInit   []OnServerInit          // 서버 초기화 시
    OnMonitorEvent []OnMonitorEvent        // 이벤트 디코딩 전
    OnDecodedFlow  []OnDecodedFlow         // Flow 디코딩 후
    OnDecodedEvent []OnDecodedEvent        // 이벤트 디코딩 후
    OnBuildFilter  []filters.OnBuildFilter // 필터 빌드 시
    OnFlowDelivery []OnFlowDelivery        // Flow API 전달 시
    OnGetFlows     []OnGetFlows            // GetFlows 호출 시
}
```

### 12.2 Hook 인터페이스와 실행 순서

각 Hook 인터페이스는 `(stop bool, err error)` 패턴을 따른다:

```go
// OnMonitorEvent - 디코딩 전 이벤트 필터링/전처리
type OnMonitorEvent interface {
    OnMonitorEvent(context.Context, *observerTypes.MonitorEvent) (stop, error)
}

// OnDecodedFlow - 디코딩된 Flow 후처리
type OnDecodedFlow interface {
    OnDecodedFlow(context.Context, *pb.Flow) (stop, error)
}

// OnDecodedEvent - 디코딩된 모든 이벤트 후처리
type OnDecodedEvent interface {
    OnDecodedEvent(context.Context, *v1.Event) (stop, error)
}
```

`stop = true`를 반환하면 **체인 실행이 즉시 중단**되고 해당 이벤트가 버려진다.
이를 통해 필터링 hook이 불필요한 이벤트를 일찍 제거할 수 있다.

### 12.3 Hook 등록 순서와 의도

`pkg/hubble/cell/hubbleintegration.go`의 `launch()` (lines 208-377)에서
hook 등록 순서는 의도적이다:

```go
// 1. MonitorFilter (OnMonitorEvent) - 이벤트 타입 필터링
observerOpts = append(observerOpts, observeroption.WithOnMonitorEvent(monitorFilter))

// 2. DropEventEmitter (OnDecodedFlow) - K8s 드롭 이벤트 발행
observerOpts = append(observerOpts, observeroption.WithOnDecodedFlowFunc(...))

// 3. LocalNodeWatcher (OnDecodedFlow) - 로컬 노드 정보 채우기
observerOpts = append(observerOpts, observeroption.WithOnDecodedFlow(localNodeWatcher))

// 4. Exporters (OnDecodedEvent) - Flow 로그 내보내기
observerOpts = append(observerOpts, observeroption.WithOnDecodedEventFunc(...))

// 5. Metrics FlowProcessor (OnDecodedFlow) - Prometheus 메트릭
observerOpts = append(observerOpts, observeroption.WithOnDecodedFlowFunc(...))

// 6. 외부 주입 옵션 (마지막)
observerOpts = append(observerOpts, h.observerOptions...)
```

**왜 이 순서인가?**

1. MonitorFilter가 가장 먼저: 불필요한 이벤트를 디코딩 전에 제거하여 CPU 절약
2. DropEventEmitter가 LocalNodeWatcher 전: 드롭 이벤트 생성에 노드 정보 불필요
3. LocalNodeWatcher가 메트릭 전: 메트릭이 올바른 노드 정보를 포함하도록
4. Exporters와 Metrics가 가장 나중: 완전히 처리된 Flow를 내보내기/측정

### 12.4 Hook 실행 흐름도

```
MonitorEvent 도착
    |
    v
+--[OnMonitorEvent #1]--+  (예: MonitorFilter - 이벤트 타입 필터)
|  stop=true? --> SKIP   |
+------------------------+
    |
    v
+--[OnMonitorEvent #N]--+
+------------------------+
    |
    v
payloadParser.Decode()
    |
    v
+--[OnDecodedFlow #1]---+  (예: DropEventEmitter)
|  stop=true? --> SKIP   |
+------------------------+
    |
    v
+--[OnDecodedFlow #2]---+  (예: LocalNodeWatcher)
+------------------------+
    |
    v
+--[OnDecodedFlow #3]---+  (예: Metrics FlowProcessor)
+------------------------+
    |
    v
+--[OnDecodedEvent #1]--+  (예: Exporter)
|  stop=true? --> SKIP   |
+------------------------+
    |
    v
ring.Write(ev)
```

---

## 13. 왜 이 아키텍처인가?

### 13.1 왜 perf event array인가? (BPF 맵이 아닌?)

BPF 프로그램에서 사용자 공간으로 데이터를 전달하는 방법은 여러 가지가 있다:

| 방법 | 장점 | 단점 |
|------|------|------|
| BPF 맵 (Hash/Array) | 구조화된 조회 가능 | 폴링 필요, 이벤트 순서 보장 불가 |
| BPF perf event array | 이벤트 기반, CPU별 버퍼 | 오버플로우 시 유실 |
| BPF ring buffer | 최신 커널에서 성능 우수 | 호환성 문제 |

Cilium이 perf event array를 선택한 이유:
1. **이벤트 기반 전달**: 패킷 처리 경로에서 즉시 이벤트를 푸시하므로 폴링 지연 없음
2. **CPU별 격리**: Per-CPU 버퍼로 CPU 간 경합 없음
3. **넓은 커널 호환성**: BPF ring buffer보다 오래된 커널 지원
4. **패킷 데이터 첨부**: 이벤트에 실제 패킷 헤더를 함께 전달 가능

### 13.2 왜 사용자 공간 링 버퍼인가? (채널이 아닌?)

Go의 채널은 FIFO 큐이지만, Hubble의 요구사항은 더 복잡하다:

| 요구사항 | Go 채널 | Hubble 링 버퍼 |
|----------|---------|---------------|
| 다수 reader 동시 접근 | 불가 (한 reader만 수신) | 가능 (독립적 읽기 위치) |
| 과거 이벤트 조회 | 불가 (소비되면 사라짐) | 가능 (덮어쓰기 전까지 보존) |
| 시간 범위 필터링 | 불가 | 가능 (RingReader + rewind) |
| backpressure 없는 쓰기 | 블로킹 또는 드롭 | 항상 논블로킹 (덮어쓰기) |
| Follow 모드 | 어려움 | 내장 (notifyCh 기반) |

링 버퍼는 **Writer가 절대 블로킹되지 않는다**는 보장이 핵심이다.
Observer의 Start() 루프가 gRPC 클라이언트의 느린 소비 때문에 멈추면
전체 이벤트 파이프라인이 정체되기 때문이다.

### 13.3 왜 단일 events 채널을 거치는가?

BPF perf → Monitor Agent → consumer → events 채널 → Observer 파이프라인에서
`events` 채널은 **buffered channel**이다. 이 채널의 역할:

1. **속도 차이 흡수**: BPF 이벤트 발생 속도와 파싱 속도의 차이를 버퍼링
2. **backpressure 지점**: 채널이 가득 차면 consumer가 lost event를 기록하고
   Monitor Agent는 즉시 다음 perf 이벤트로 진행
3. **디커플링**: Monitor Agent는 Hubble Observer의 내부 구현을 알 필요 없음

```
BPF perf     Monitor Agent     consumer      events chan     Observer
(매우 빠름)   (단순 분배)       (비블로킹)    (버퍼 역할)    (파싱, 느림)
   |              |               |              |              |
   +--event------>+--dispatch---->+--send------->+--dequeue---->+
   |              |               |              |              |
   +--event------>+--dispatch---->+--send--X (full)             |
   |              |               | lost++       |              |
```

### 13.4 왜 Hook 체인인가? (하드코딩이 아닌?)

Hook 시스템은 Observer 코어를 수정하지 않고 기능을 확장할 수 있게 한다:

1. **메트릭**: `OnDecodedFlow` hook으로 Prometheus 메트릭 수집
2. **내보내기**: `OnDecodedEvent` hook으로 외부 시스템(Elasticsearch, S3 등)으로 전송
3. **필터링**: `OnMonitorEvent` hook으로 불필요한 이벤트 조기 제거
4. **이벤트 강화**: `OnDecodedFlow` hook으로 노드 정보 추가
5. **드롭 알림**: `OnDecodedFlow` hook으로 K8s Event 생성

`stop` 반환값은 **단락 평가(short-circuit evaluation)** 패턴으로,
비용이 낮은 필터링을 먼저 실행하고, 이벤트가 필터링되면 비용이 높은
메트릭 계산이나 내보내기를 건너뛸 수 있다.

### 13.5 왜 Relay는 우선순위 큐를 사용하는가?

분산 시스템에서 전역 시간 순서를 보장하는 것은 근본적으로 어렵다.
각 노드의 시계가 정확히 동기화되어 있다고 가정하더라도, 네트워크 지연으로 인해
도착 순서와 발생 순서가 다를 수 있다.

Relay의 우선순위 큐는 **제한된 정렬 윈도우**를 제공하여 실용적인 해결책을 제시한다:

- 큐 크기(`qlen`)가 정렬 윈도우의 크기를 결정
- `bufferDrainTimeout`이 최대 지연을 제한
- 완벽한 전역 순서 대신 "충분히 좋은" 순서를 보장
- 실시간 스트리밍 지연을 최소화

### 13.6 Rate Limiting 전략의 계층화

Cilium의 observability는 3단계의 rate limiting을 통해 고부하 상황에서도
시스템 안정성을 유지한다:

```
[1단계] BPF 레벨 - 토큰 버킷
        EVENTS_MAP_RATE_LIMIT으로 이벤트 생성 자체를 제한
        + Monitor Aggregation으로 수신 이벤트 억제
            |
            v
[2단계] Consumer 레벨 - 비블로킹 채널
        events 채널이 가득 차면 이벤트를 버리고 카운터 증가
        주기적으로 합산된 lost event를 전송
            |
            v
[3단계] gRPC 레벨 - Lost event rate limiting
        GetFlows()에서 Hubble 링 버퍼 유실을
        IntervalRangeCounter로 합산하여 전송
```

각 단계에서 유실이 발생해도 **유실 사실 자체는 보존**하여
운영자가 관찰 품질을 모니터링할 수 있다.

---

## 14. 참고 파일 목록

### BPF 데이터패스

| 파일 | 내용 |
|------|------|
| `bpf/lib/notify.h` | trace_point enum (14개 관찰 지점), NOTIFY_CAPTURE_HDR |
| `bpf/lib/trace.h` | send_trace_notify(), trace_reason, Monitor Aggregation |
| `bpf/lib/drop.h` | send_drop_notify(), drop_notify 구조체 |
| `bpf/lib/events.h` | cilium_events perf event array 정의 |

### Monitor Agent

| 파일 | 내용 |
|------|------|
| `pkg/monitor/agent/agent.go` | Agent 인터페이스, agent 구조체, handleEvents(), processPerfRecord() |
| `pkg/monitor/api/types.go` | MessageType 상수, TraceObservationPoints, AgentNotification |
| `pkg/monitor/datapath_trace.go` | TraceNotify 구조체, 버전별 디코딩, trace_reason |
| `pkg/monitor/datapath_drop.go` | DropNotify 구조체, 버전별 디코딩 |

### Hubble Core

| 파일 | 내용 |
|------|------|
| `pkg/hubble/observer/types/types.go` | MonitorEvent, PerfEvent, AgentEvent, LostEvent |
| `pkg/hubble/monitor/consumer.go` | consumer 구조체, sendEvent() backpressure |
| `pkg/hubble/container/ring.go` | Ring 구조체, Write(), readFrom(), 사이클 감지 |
| `pkg/hubble/observer/local_observer.go` | LocalObserverServer, Start(), GetFlows() |
| `pkg/hubble/observer/observeroption/option.go` | Hook 인터페이스 (OnMonitorEvent/OnDecodedFlow/OnDecodedEvent) |

### Parser

| 파일 | 내용 |
|------|------|
| `pkg/hubble/parser/parser.go` | Parser 구조체, Decode() 이벤트 타입 분기 |
| `pkg/hubble/parser/threefour/parser.go` | L3/L4 Parser (gopacket, getter 인터페이스) |
| `pkg/hubble/parser/seven/parser.go` | L7 Parser (DNS/HTTP/Kafka, LRU 캐시) |

### gRPC 서버 및 Relay

| 파일 | 내용 |
|------|------|
| `pkg/hubble/cell/hubbleintegration.go` | launch() - UNIX/TCP 서버, hook 등록 순서 |
| `pkg/hubble/relay/server/server.go` | Relay Server, PeerManager |
| `pkg/hubble/relay/observer/observer.go` | retrieveFlowsFromPeer(), sortFlows() 우선순위 큐 |

### 메트릭

| 파일 | 내용 |
|------|------|
| `pkg/hubble/metrics/flow_processor.go` | StaticFlowProcessor |
| `pkg/hubble/metrics/flow/handler.go` | hubble_flows_processed_total 메트릭 |
| `pkg/hubble/metrics/dns/handler.go` | DNS 메트릭 핸들러 |
| `pkg/hubble/metrics/drop/handler.go` | Drop 메트릭 핸들러 |
| `pkg/hubble/metrics/http/handler.go` | HTTP 메트릭 핸들러 |
| `pkg/hubble/metrics/tcp/handler.go` | TCP 메트릭 핸들러 |
| `pkg/hubble/metrics/policy/handler.go` | Policy 메트릭 핸들러 |
