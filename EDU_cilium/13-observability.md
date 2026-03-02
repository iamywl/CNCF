# 13. Cilium 관측성(Observability) 서브시스템 심층 분석

## 목차

1. [개요](#1-개요)
2. [Hubble 아키텍처: Observer - Relay - UI/CLI](#2-hubble-아키텍처-observer---relay---uicli)
3. [Hubble Observer: BPF perf/ring buffer에서 Flow 이벤트 파싱](#3-hubble-observer-bpf-perfring-buffer에서-flow-이벤트-파싱)
4. [Hubble Relay: 멀티노드 Flow 집계 (gRPC 스트리밍)](#4-hubble-relay-멀티노드-flow-집계-grpc-스트리밍)
5. [Hubble CLI: observe 명령으로 실시간 Flow 모니터링](#5-hubble-cli-observe-명령으로-실시간-flow-모니터링)
6. [Hubble UI: 서비스 맵 시각화](#6-hubble-ui-서비스-맵-시각화)
7. [Prometheus 메트릭: pkg/metrics/, 커스텀 메트릭 등록](#7-prometheus-메트릭-pkgmetrics-커스텀-메트릭-등록)
8. [OpenTelemetry 통합: OTLP 내보내기](#8-opentelemetry-통합-otlp-내보내기)
9. [Flow 데이터 구조: 소스/목적지, verdict, drop reason, L7 정보](#9-flow-데이터-구조-소스목적지-verdict-drop-reason-l7-정보)
10. [BPF 이벤트 수집 경로: monitor_output map에서 Hubble까지](#10-bpf-이벤트-수집-경로-monitor_output-map에서-hubble까지)
11. [디버깅 도구: gops, pprof, BPF 패킷 트레이스](#11-디버깅-도구-gops-pprof-bpf-패킷-트레이스)

---

## 1. 개요

Cilium의 관측성(Observability) 서브시스템은 eBPF 기반 네트워킹 스택에서 발생하는 모든 네트워크 이벤트를 가시화하는 핵심 인프라이다. **Hubble**은 이 관측성의 중심축으로, 커널 수준의 BPF perf 이벤트를 사용자 친화적인 Flow 데이터로 변환하여 실시간 네트워크 모니터링, 서비스 맵 시각화, 문제 진단을 가능하게 한다.

### 관측성 스택 전체 구조

```
+--------------------------------------------------+
|                   Hubble UI                       |  브라우저 기반 서비스 맵
+--------------------------------------------------+
|                  Hubble CLI                       |  hubble observe 명령
+--------------------------------------------------+
|                 Hubble Relay                      |  멀티노드 gRPC 집계
+--------------------------------------------------+
|    Hubble Observer  |  Prometheus  |  Exporter    |  노드별 로컬 처리
+--------------------------------------------------+
|              Parser (L3/4, L7, Debug)             |  이벤트 디코딩
+--------------------------------------------------+
|           Monitor Consumer (Ring Buffer)          |  BPF->사용자공간 브릿지
+--------------------------------------------------+
|          Monitor Agent (perf.Reader)              |  perf ring buffer 읽기
+--------------------------------------------------+
|        BPF Datapath (monitor_output map)          |  커널 이벤트 생성
+--------------------------------------------------+
```

### 핵심 소스코드 위치

| 컴포넌트 | 경로 |
|---------|------|
| Hubble Observer | `pkg/hubble/observer/local_observer.go` |
| Hubble Relay | `pkg/hubble/relay/` |
| Hubble Parser | `pkg/hubble/parser/` |
| Hubble Metrics | `pkg/hubble/metrics/` |
| Hubble Exporter | `pkg/hubble/exporter/` |
| Hubble CLI | `hubble/cmd/observe/` |
| Monitor Agent | `pkg/monitor/agent/agent.go` |
| Monitor Consumer | `pkg/hubble/monitor/consumer.go` |
| Datapath Events | `pkg/monitor/datapath_trace.go`, `datapath_drop.go` |
| Cilium Metrics | `pkg/metrics/metrics.go` |
| Ring Buffer | `pkg/hubble/container/ring.go` |
| Flow Proto | `api/v1/flow/flow.pb.go` |

---

## 2. Hubble 아키텍처: Observer - Relay - UI/CLI

### 2.1 세 계층 아키텍처

Hubble은 **Observer - Relay - Client** 세 계층으로 구성된다.

```
+---------+    +---------+    +---------+
| Node 1  |    | Node 2  |    | Node 3  |
| Observer |    | Observer |    | Observer |
+----+----+    +----+----+    +----+----+
     |              |              |
     +------+-------+------+------+
            |              |
     +------v------+       |
     | Hubble Relay|<------+       gRPC 스트리밍으로
     | (집계/정렬)  |               멀티노드 Flow 수집
     +------+------+
            |
     +------v------+
     |  Client     |
     | (CLI / UI)  |
     +-------------+
```

#### Observer (노드 로컬)
- 각 Cilium Agent 내부에 임베디드 실행
- BPF perf ring buffer에서 이벤트를 읽어 파싱
- 파싱된 Flow를 인메모리 Ring Buffer에 저장
- 소스: `pkg/hubble/observer/local_observer.go`

```go
// LocalObserverServer는 Cilium 프로세스 내부에서 실행되는 Observer 구현
type LocalObserverServer struct {
    ring          *container.Ring                    // Flow 참조를 담는 Ring Buffer
    events        chan *observerTypes.MonitorEvent    // 모니터 이벤트 수신 채널
    payloadParser parser.Decoder                     // BPF 페이로드 디코더
    opts          observeroption.Options              // 설정 옵션
}
```

#### Relay (클러스터 전역)
- 별도 Pod으로 배포 (hubble-relay)
- 각 노드의 Observer gRPC 서버에 연결
- 여러 노드의 Flow를 타임스탬프 기준으로 정렬/병합
- 소스: `pkg/hubble/relay/server/server.go`

#### Client (CLI / UI)
- **Hubble CLI**: `hubble observe` 명령으로 실시간 Flow 모니터링
- **Hubble UI**: 브라우저 기반 서비스 의존성 맵 시각화

### 2.2 Observer 옵션 체계와 훅 시스템

Observer는 유연한 훅(hook) 시스템을 제공하여 이벤트 처리 파이프라인을 확장할 수 있다.

소스: `pkg/hubble/observer/observeroption/option.go`

```go
type Options struct {
    MaxFlows       container.Capacity       // Ring Buffer 최대 용량
    MonitorBuffer  int                      // 모니터 이벤트 버퍼 크기
    OnServerInit   []OnServerInit           // 서버 초기화 시 호출
    OnMonitorEvent []OnMonitorEvent         // 이벤트 디코딩 전 호출
    OnDecodedFlow  []OnDecodedFlow          // Flow 디코딩 후 호출
    OnDecodedEvent []OnDecodedEvent         // 이벤트 디코딩 후 호출
    OnBuildFilter  []filters.OnBuildFilter  // 필터 빌드 시 호출
    OnFlowDelivery []OnFlowDelivery         // API 전달 전 호출
    OnGetFlows     []OnGetFlows             // GetFlows API 호출 시
}
```

이 훅 시스템이 Hubble의 핵심 확장 메커니즘으로, 메트릭 수집, Flow 내보내기, 필터링 등이 모두 이 훅을 통해 구현된다.

---

## 3. Hubble Observer: BPF perf/ring buffer에서 Flow 이벤트 파싱

### 3.1 이벤트 처리 루프

Observer의 `Start()` 메서드가 핵심 이벤트 루프를 실행한다.

소스: `pkg/hubble/observer/local_observer.go` (116-197행)

```go
func (s *LocalObserverServer) Start() {
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

nextEvent:
    for monitorEvent := range s.GetEventsChannel() {
        // 1단계: OnMonitorEvent 훅 실행 (디코딩 전)
        for _, f := range s.opts.OnMonitorEvent {
            stop, err := f.OnMonitorEvent(ctx, monitorEvent)
            if stop { continue nextEvent }
        }

        // 2단계: 페이로드 파싱 (BPF 바이트 -> Flow 구조체)
        ev, err := s.payloadParser.Decode(monitorEvent)
        if err != nil {
            // 알 수 없는 이벤트는 조용히 무시
            continue
        }

        // 3단계: Flow 디코딩 후 처리
        if flow, ok := ev.Event.(*flowpb.Flow); ok {
            s.trackNamespaces(flow)         // 네임스페이스 추적
            for _, f := range s.opts.OnDecodedFlow {
                stop, err := f.OnDecodedFlow(ctx, flow)
                if stop { continue nextEvent }
            }
            s.numObservedFlows.Add(1)       // 관측 카운터 증가
        }

        // 4단계: OnDecodedEvent 훅 실행
        for _, f := range s.opts.OnDecodedEvent {
            stop, err := f.OnDecodedEvent(ctx, ev)
            if stop { continue nextEvent }
        }

        // 5단계: Ring Buffer에 기록
        s.GetRingBuffer().Write(ev)
    }
}
```

### 3.2 Ring Buffer 구현

Hubble의 Ring Buffer는 lock-free 읽기와 최소한의 잠금으로 구현된 고성능 순환 버퍼이다.

소스: `pkg/hubble/container/ring.go`

```go
type Ring struct {
    mask      uint64            // 인덱스 마스크 (dataLen - 1)
    write     atomic.Uint64     // 쓰기 위치 (원자적)
    cycleExp  uint8             // 사이클 지수 (2^x)
    dataLen   uint64            // 내부 버퍼 길이
    data      []*v1.Event       // 이벤트 저장 슬라이스
    notifyMu  lock.Mutex        // 알림용 뮤텍스
    notifyCh  chan struct{}     // 대기 중인 리더에게 알림
}
```

핵심 설계:
- **용량 제한**: 2^i - 1 형태 (1, 3, 7, 15, ..., 65535)
- **원자적 쓰기**: `atomic.Uint64`로 쓰기 위치 관리
- **사이클 감지**: write 카운터의 상위 비트로 오버랩 감지
- **알림 메커니즘**: `notifyCh` 채널을 close하여 대기 중인 리더에게 알림

```go
func (r *Ring) Write(entry *v1.Event) {
    r.notifyMu.Lock()

    write := r.write.Add(1)
    writeIdx := (write - 1) & r.mask
    r.dataStoreAtomic(writeIdx, entry)

    // 대기 중인 리더에게 알림
    if r.notifyCh != nil {
        close(r.notifyCh)
        r.notifyCh = nil
    }

    r.notifyMu.Unlock()
}
```

### 3.3 GetFlows gRPC 서비스

`GetFlows`는 클라이언트 요청에 따라 Ring Buffer에서 Flow를 읽어 스트리밍하는 gRPC 서비스이다.

소스: `pkg/hubble/observer/local_observer.go` (260-428행)

처리 과정:
1. **필터 구축**: whitelist/blacklist 기반 필터 함수 생성
2. **RingReader 생성**: 요청 조건(since, number, first)에 맞는 시작 위치 결정
3. **EventsReader 생성**: 필터, 시간 범위, 최대 이벤트 수 적용
4. **FieldMask 적용**: 요청된 필드만 복사 (대역폭 최적화)
5. **스트리밍 응답**: Flow를 `GetFlowsResponse`로 래핑하여 전송

---

## 4. Hubble Relay: 멀티노드 Flow 집계 (gRPC 스트리밍)

### 4.1 Relay 서버 구조

Relay는 클러스터 내 모든 노드의 Observer에서 Flow를 수집하여 단일 스트림으로 제공한다.

소스: `pkg/hubble/relay/server/server.go`

```go
type Server struct {
    server           *grpc.Server          // gRPC 서버
    pm               *pool.PeerManager     // 피어(노드) 관리자
    healthServer     *healthServer         // 헬스 체크
    metricsServer    *http.Server          // Prometheus 메트릭
}
```

### 4.2 Flow 수집 파이프라인

소스: `pkg/hubble/relay/observer/observer.go`

```
+--------+   +--------+   +--------+
| Node A |   | Node B |   | Node C |    각 노드에서 gRPC로 Flow 수집
+---+----+   +---+----+   +---+----+
    |            |             |
    v            v             v
  +------------------------------+
  |   flows channel (병합)        |      모든 Flow를 하나의 채널로
  +-------------+----------------+
                |
                v
  +-------------+----------------+
  |   PriorityQueue (정렬)        |      타임스탬프 기준 정렬
  +-------------+----------------+
                |
                v
  +-------------+----------------+
  |   Error Aggregation (집계)    |      오류 메시지 집계
  +-------------+----------------+
                |
                v
  +-------------+----------------+
  |   gRPC Stream (클라이언트)     |      정렬된 Flow 스트리밍
  +------------------------------+
```

#### Flow 수집 (`retrieveFlowsFromPeer`)

```go
func retrieveFlowsFromPeer(
    ctx context.Context,
    client observerpb.ObserverClient,
    req *observerpb.GetFlowsRequest,
    flows chan<- *observerpb.GetFlowsResponse,
) error {
    c, err := client.GetFlows(ctx, req)
    for {
        flow, err := c.Recv()
        if err != nil { return err }
        select {
        case flows <- flow:
        case <-ctx.Done(): return nil
        }
    }
}
```

#### 타임스탬프 정렬 (`sortFlows`)

소스: `pkg/hubble/relay/observer/observer.go` (67-119행)

```go
func sortFlows(ctx context.Context, flows <-chan *observerpb.GetFlowsResponse,
    qlen int, bufferDrainTimeout time.Duration) <-chan *observerpb.GetFlowsResponse {

    pq := queue.NewPriorityQueue(qlen)
    sortedFlows := make(chan *observerpb.GetFlowsResponse, qlen)

    go func() {
        for flow := range flows {
            if pq.Len() == qlen {
                f := pq.Pop()               // 가장 오래된 항목 내보냄
                sortedFlows <- f
            }
            pq.Push(flow)                    // 새 항목 삽입
        }
        // 잔여 항목 모두 내보냄
        for f := pq.Pop(); f != nil; f = pq.Pop() {
            sortedFlows <- f
        }
    }()
    return sortedFlows
}
```

### 4.3 PriorityQueue

소스: `pkg/hubble/relay/queue/priority_queue.go`

`container/heap`을 사용한 최소 힙(min-heap) 구현으로, 타임스탬프가 가장 오래된 Flow가 먼저 나온다.

```go
type PriorityQueue struct {
    h minHeap    // []*observerpb.GetFlowsResponse
}

func (h minHeap) Less(i, j int) bool {
    if h[i].GetTime().GetSeconds() == h[j].GetTime().GetSeconds() {
        return h[i].GetTime().GetNanos() < h[j].GetTime().GetNanos()
    }
    return h[i].GetTime().GetSeconds() < h[j].GetTime().GetSeconds()
}
```

---

## 5. Hubble CLI: observe 명령으로 실시간 Flow 모니터링

### 5.1 명령 구조

소스: `hubble/cmd/observe/flows.go`

Hubble CLI의 `observe` 명령은 Relay(또는 로컬 Observer)에 gRPC로 연결하여 실시간 Flow를 표시한다.

```
hubble observe [flags]
    --follow              실시간 Flow 추적
    --last N              최근 N개 Flow
    --since "5m ago"      시간 기반 필터
    --from-pod "ns/pod"   소스 Pod 필터
    --to-pod "ns/pod"     목적지 Pod 필터
    --verdict FORWARDED   판정 필터
    --protocol tcp        프로토콜 필터
    --http-status 200     HTTP 상태 코드 필터
    -o json|compact|table 출력 형식
```

### 5.2 지원 필터 유형

| 필터 카테고리 | 플래그 예시 | 설명 |
|-------------|-----------|------|
| **엔드포인트** | `--from-pod`, `--to-pod`, `--pod` | Pod 기반 필터 |
| **네임스페이스** | `--from-namespace`, `--to-namespace`, `-n` | Kubernetes 네임스페이스 필터 |
| **레이블** | `--from-label`, `--to-label`, `-l` | Cilium 레이블 기반 필터 |
| **네트워크** | `--from-ip`, `--to-ip`, `--from-port`, `--to-port` | IP/포트 필터 |
| **프로토콜** | `--protocol tcp/udp/http/dns` | L4/L7 프로토콜 필터 |
| **판정** | `--verdict FORWARDED/DROPPED` | 트래픽 판정 필터 |
| **서비스** | `--from-service`, `--to-service` | Kubernetes 서비스 필터 |
| **HTTP** | `--http-method`, `--http-status`, `--http-path` | HTTP 요청 필터 |
| **ID** | `--from-identity`, `--to-identity` | 보안 ID 필터 |
| **트래픽 방향** | `--traffic-direction ingress/egress` | 인그레스/이그레스 필터 |
| **부정** | `--not` | 다음 필터의 반전 |

### 5.3 GetFlows 요청 구성

```go
req := &observerpb.GetFlowsRequest{
    Number:    number,           // 최대 Flow 수
    Follow:    selectorOpts.follow,  // 실시간 추적 여부
    Whitelist: wl,               // 허용 필터 목록
    Blacklist: bl,               // 차단 필터 목록
    Since:     since,            // 시작 시간
    Until:     until,            // 종료 시간
    First:     first,            // 첫 N개 (오래된 것부터)
    FieldMask: fm,               // 필드 마스크 (대역폭 최적화)
}
```

### 5.4 출력 형식

- **compact**: 한 줄 요약 (기본)
- **dict**: 사전 형태
- **json/jsonpb**: JSON 형식
- **table**: 표 형식 (follow 모드 비호환)

---

## 6. Hubble UI: 서비스 맵 시각화

### 6.1 서비스 맵 개요

Hubble UI는 클러스터 내 서비스 간 통신을 시각적으로 표현하는 웹 인터페이스이다. Flow 데이터에서 소스/목적지 서비스를 추출하여 방향 그래프(directed graph)를 구성한다.

```
서비스 맵 구성 요소:
- 노드: Pod, Service, 외부 엔드포인트
- 엣지: Flow 데이터 기반 연결 관계
- 색상: 판정 (FORWARDED=녹색, DROPPED=빨간색)
- 두께: 트래픽 볼륨
```

### 6.2 Flow에서 서비스 맵 생성

```
Flow 데이터:
{
  source: {namespace: "default", labels: ["app=frontend"]},
  destination: {namespace: "default", labels: ["app=backend"]},
  source_service: {name: "frontend", namespace: "default"},
  destination_service: {name: "backend", namespace: "default"},
  verdict: FORWARDED,
  l4: {TCP: {source_port: 54321, destination_port: 8080}}
}

         [frontend] --TCP:8080--> [backend]    (FORWARDED)
```

### 6.3 Helm 배포

```yaml
# values.yaml
hubble:
  enabled: true
  relay:
    enabled: true
  ui:
    enabled: true
```

---

## 7. Prometheus 메트릭: pkg/metrics/, 커스텀 메트릭 등록

### 7.1 Cilium Agent 메트릭 (pkg/metrics/)

소스: `pkg/metrics/metrics.go`

Cilium Agent는 Prometheus 네임스페이스 `cilium`으로 수백 개의 메트릭을 노출한다.

```go
const (
    CiliumAgentNamespace = "cilium"
    SubsystemBPF         = "bpf"
    SubsystemDatapath    = "datapath"
    SubsystemAgent       = "agent"
    SubsystemK8s         = "k8s"
)
```

주요 서브시스템별 메트릭:
- **bpf**: BPF 시스콜 통계 (`cilium_bpf_*`)
- **datapath**: 데이터패스 관리 (`cilium_datapath_*`)
- **agent**: 에이전트 내부 상태 (`cilium_agent_*`)
- **k8s**: Kubernetes 클라이언트 통계 (`cilium_k8s_*`)
- **ipcache**: IP 캐시 상태 (`cilium_ipcache_*`)

### 7.2 Hubble 메트릭 (pkg/hubble/metrics/)

소스: `pkg/hubble/metrics/metrics.go`

Hubble 메트릭은 Flow 데이터를 기반으로 Prometheus 네임스페이스 `hubble`로 노출된다.

```go
const DefaultPrometheusNamespace = "hubble"
```

#### 내장 메트릭 플러그인

| 플러그인 | 소스 경로 | 설명 |
|---------|---------|------|
| `flow` | `pkg/hubble/metrics/flow/` | `hubble_flows_processed_total` |
| `dns` | `pkg/hubble/metrics/dns/` | DNS 요청/응답 메트릭 |
| `drop` | `pkg/hubble/metrics/drop/` | 패킷 드롭 메트릭 |
| `http` | `pkg/hubble/metrics/http/` | HTTP 요청 메트릭 |
| `tcp` | `pkg/hubble/metrics/tcp/` | TCP 연결 메트릭 |
| `icmp` | `pkg/hubble/metrics/icmp/` | ICMP 메트릭 |
| `kafka` | `pkg/hubble/metrics/kafka/` | Kafka 요청 메트릭 |
| `policy` | `pkg/hubble/metrics/policy/` | 정책 판정 메트릭 |
| `port-distribution` | `pkg/hubble/metrics/port-distribution/` | 포트 분포 메트릭 |
| `flows-to-world` | `pkg/hubble/metrics/flows-to-world/` | 외부 통신 메트릭 |
| `sctp` | `pkg/hubble/metrics/sctp/` | SCTP 메트릭 |

### 7.3 메트릭 플러그인 시스템

소스: `pkg/hubble/metrics/api/api.go`

```go
type Plugin interface {
    NewHandler() Handler
    HelpText() string
}

type Handler interface {
    Init(registry *prometheus.Registry, options *MetricConfig) error
    ProcessFlow(ctx context.Context, flow *pb.Flow) error
    ListMetricVec() []*prometheus.MetricVec
    Context() *ContextOptions
    Deinit(registry *prometheus.Registry) error
}
```

#### Flow 메트릭 핸들러 예시

소스: `pkg/hubble/metrics/flow/handler.go`

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
        Namespace: "hubble",
        Name:      "flows_processed_total",
        Help:      "Total number of flows processed",
    }, labels)

    registry.MustRegister(h.flows)
    return nil
}

func (h *flowHandler) ProcessFlow(ctx context.Context, flow *flowpb.Flow) error {
    labels := []string{FlowProtocol(flow), typeName, subType, flow.GetVerdict().String()}
    h.flows.WithLabelValues(labels...).Inc()
    return nil
}
```

### 7.4 메트릭 설정

Hubble 메트릭은 두 가지 방식으로 설정할 수 있다:

**정적 설정** (Helm):
```yaml
hubble:
  metrics:
    enabled:
      - dns:query
      - drop
      - tcp
      - flow
      - http
      - icmp
      - port-distribution
```

**동적 설정** (ConfigMap):
```yaml
metrics:
  - name: flow
    contextOptions:
      - name: sourceContext
        values: ["namespace"]
    includeFilters:
      - source_pod: ["default/"]
```

### 7.5 추가 내부 메트릭

```go
var (
    LostEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
        Namespace: "hubble",
        Name:      "lost_events_total",
        Help:      "Number of lost events",
    }, []string{"source"})

    RequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
        Namespace: "hubble",
        Name:      "metrics_http_handler_requests_total",
    }, []string{"code"})

    RequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
        Namespace: "hubble",
        Name:      "metrics_http_handler_request_duration_seconds",
    }, []string{"code"})
)
```

---

## 8. OpenTelemetry 통합: OTLP 내보내기

### 8.1 트레이스 컨텍스트 추출

Hubble은 OpenTelemetry의 W3C Trace Context 표준을 지원하여 HTTP 요청의 trace ID를 Flow에 포함시킨다.

소스: `pkg/hubble/parser/seven/tracing.go`

```go
const traceparentHeader = "traceparent"

func extractTraceContext(record *accesslog.LogRecord) *flowpb.TraceContext {
    if record.HTTP != nil {
        traceID := traceIDFromHTTPHeader(record.HTTP.Headers)
        if traceID == "" { return nil }
        return &flowpb.TraceContext{
            Parent: &flowpb.TraceParent{
                TraceId: traceID,
            },
        }
    }
    return nil
}

func traceIDFromHTTPHeader(h http.Header) string {
    // W3C TraceContext 전파기를 사용하여 traceparent 헤더에서 trace ID 추출
    tc := propagation.TraceContext{}
    sp := trace.SpanContextFromContext(
        tc.Extract(context.Background(), propagation.HeaderCarrier(h)),
    )
    if sp.HasTraceID() {
        return sp.TraceID().String()
    }
    return ""
}
```

### 8.2 Flow Exporter

Hubble Exporter는 Flow 데이터를 외부 시스템으로 내보내는 메커니즘을 제공한다.

소스: `pkg/hubble/exporter/exporter.go`

```go
type FlowLogExporter interface {
    Export(ctx context.Context, ev *v1.Event) error
    Stop() error
}

type exporter struct {
    logger     *slog.Logger
    encoder    Encoder        // JSON, protobuf 등
    writer     io.WriteCloser // 파일, stdout 등
    aggregator *AggregatorRunner  // Flow 집계기
    opts       Options
}
```

#### Exporter 처리 흐름

```go
func (e *exporter) Export(ctx context.Context, ev *v1.Event) error {
    // 1. 필터 적용 (allowlist/denylist)
    if !filters.Apply(e.opts.AllowFilters(), e.opts.DenyFilters(), ev) {
        return nil
    }

    // 2. OnExportEvent 훅 실행
    for _, f := range e.opts.OnExportEvent {
        stop, err := f.OnExportEvent(ctx, ev, e.encoder)
        if stop { return nil }
    }

    // 3. 집계기가 활성화된 경우 Flow를 집계기에 전달
    if e.aggregator != nil {
        if _, ok := ev.Event.(*flowpb.Flow); ok {
            e.aggregator.Add(ev)
            return nil
        }
    }

    // 4. ExportEvent로 변환하여 인코딩
    res := e.eventToExportEvent(ev)
    return e.encoder.Encode(res)
}
```

### 8.3 동적 Exporter 설정

소스: `pkg/hubble/exporter/config.go`

```go
type FlowLogConfig struct {
    Name                string         // Exporter 이름
    FilePath            string         // 출력 파일 경로
    FieldMask           FieldMask      // 포함할 필드 목록
    FieldAggregate      FieldAggregate // 집계 기준 필드
    AggregationInterval Duration       // 집계 주기
    IncludeFilters      FlowFilters    // 포함 필터
    ExcludeFilters      FlowFilters    // 제외 필터
    FileMaxSizeMB       int            // 파일 최대 크기
    FileMaxBackups      int            // 최대 백업 파일 수
}
```

YAML 설정 예시:
```yaml
flowLogs:
  - name: "all-flows"
    filePath: "/var/run/cilium/hubble/events.log"
    fieldMask:
      - time
      - source
      - destination
      - verdict
    includeFilters:
      - source_pod: ["kube-system/"]
    fileMaxSizeMb: 10
    fileMaxBackups: 5
```

### 8.4 OpenTelemetry OTLP 내보내기 구현 전략

Cilium은 다음과 같은 방식으로 OTLP와 통합할 수 있다:

1. **OpenTelemetry Collector Sidecar**: Hubble Exporter의 출력을 OTLP Collector가 수집
2. **Trace ID 연결**: HTTP Flow의 W3C trace ID를 통해 분산 트레이싱 시스템과 연결
3. **Prometheus Remote Write**: Hubble 메트릭을 OTLP 메트릭 백엔드로 전달

---

## 9. Flow 데이터 구조: 소스/목적지, verdict, drop reason, L7 정보

### 9.1 Flow 프로토콜 버퍼 구조

소스: `api/v1/flow/flow.pb.go`

```protobuf
message Flow {
    // 시간/식별
    Timestamp time = 1;
    string uuid = 34;
    Emitter emitter = 41;

    // 판정
    Verdict verdict = 2;           // FORWARDED, DROPPED, AUDIT, REDIRECTED, ERROR
    uint32 drop_reason = 3;        // (deprecated) 드롭 사유 코드
    AuthType auth_type = 35;       // 인증 타입

    // 네트워크 계층
    Ethernet ethernet = 4;         // L2 정보
    IP IP = 5;                     // L3 정보 (소스/목적지 IP)
    Layer4 l4 = 6;                 // L4 정보 (TCP/UDP/ICMP/SCTP)
    Tunnel tunnel = 39;            // 터널 정보

    // 엔드포인트
    Endpoint source = 8;           // 소스 엔드포인트
    Endpoint destination = 9;      // 목적지 엔드포인트

    // 서비스
    Service source_service = 20;
    Service destination_service = 21;

    // L7 정보
    Layer7 l7 = 15;                // HTTP, DNS, Kafka 등

    // 이벤트 메타데이터
    CiliumEventType event_type = 19;
    TrafficDirection traffic_direction = 22;
    TraceObservationPoint trace_observation_point = 24;
    DropReason drop_reason_desc = 29;  // 상세 드롭 사유

    // 정책
    uint32 policy_match_type = 23;
    repeated Policy egress_allowed_by = 31;
    repeated Policy ingress_allowed_by = 32;
    repeated Policy egress_denied_by = 33;
    repeated Policy ingress_denied_by = 34;

    // 트레이싱
    TraceContext trace_context = 28;
    uint64 ip_trace_id = 40;
}
```

### 9.2 Endpoint 구조

```protobuf
message Endpoint {
    uint32 ID = 1;              // Cilium 엔드포인트 ID
    uint32 identity = 2;        // 보안 ID
    string namespace = 3;       // Kubernetes 네임스페이스
    repeated string labels = 4; // Cilium 레이블
    string pod_name = 5;        // Pod 이름
    repeated Workload workloads = 6; // 워크로드 정보
}
```

### 9.3 Verdict (판정)

```go
// Flow 판정 결과
var verdicts = []string{
    "FORWARDED",    // 전달됨
    "DROPPED",      // 드롭됨
    "AUDIT",        // 감사 모드
    "REDIRECTED",   // 프록시로 리다이렉트됨
    "ERROR",        // 오류 발생
    "TRACED",       // 추적됨
    "TRANSLATED",   // NAT 변환됨
}
```

### 9.4 Drop Reason

드롭 사유는 BPF 데이터패스에서 발생하는 다양한 이유를 포함한다:

- `POLICY_DENIED`: 정책에 의해 차단
- `UNSUPPORTED_L3_PROTOCOL`: 지원하지 않는 L3 프로토콜
- `CT_TRUNCATED_OR_INVALID`: 연결 추적 항목 손상
- `INVALID_SOURCE_IP`: 잘못된 소스 IP
- 기타 수십 가지 사유

### 9.5 L7 정보

```protobuf
message Layer7 {
    FlowType type = 1;          // REQUEST, RESPONSE, SAMPLE
    uint32 latency_ns = 2;      // 지연 시간 (나노초)
    oneof record {
        DNS dns = 100;           // DNS 쿼리/응답
        HTTP http = 101;         // HTTP 요청/응답
        Kafka kafka = 102;       // Kafka 요청/응답
    }
}

message HTTP {
    uint32 code = 1;             // HTTP 상태 코드
    string method = 2;           // GET, POST 등
    string url = 3;              // 요청 URL
    string protocol = 4;         // HTTP/1.1, HTTP/2
    repeated HTTPHeader headers = 5;
}

message DNS {
    string query = 1;            // DNS 쿼리 이름
    repeated string ips = 2;     // 응답 IP 목록
    uint32 ttl = 3;              // TTL
    repeated string cnames = 4;  // CNAME 목록
    uint32 rcode = 6;            // 응답 코드
    repeated string qtypes = 7;  // 쿼리 타입
    repeated string rrtypes = 8; // 응답 레코드 타입
}
```

---

## 10. BPF 이벤트 수집 경로: monitor_output map에서 Hubble까지

### 10.1 전체 이벤트 흐름

```
[BPF 데이터패스]
      |
      | send_trace_notify() / send_drop_notify() 등
      v
[monitor_output map]  <-- BPF perf event array (CPU별)
      |
      | perf.Reader.Read()
      v
[Monitor Agent]  (pkg/monitor/agent/agent.go)
      |
      | NotifyPerfEvent(data, cpu)
      v
[Hubble Consumer]  (pkg/hubble/monitor/consumer.go)
      |
      | MonitorEvent{PerfEvent{Data, CPU}}
      v
[Observer events channel]
      |
      | payloadParser.Decode(monitorEvent)
      v
[Parser]  (pkg/hubble/parser/parser.go)
      |
      | L3/4 Parser 또는 L7 Parser
      v
[Flow protobuf]
      |
      | Ring Buffer에 저장, 메트릭 처리
      v
[Ring Buffer]  +  [Prometheus Metrics]  +  [Exporter]
```

### 10.2 Monitor Agent: perf ring buffer 읽기

소스: `pkg/monitor/agent/agent.go` (324-397행)

```go
func (a *agent) handleEvents(stopCtx context.Context) {
    // 1. perf reader 생성
    bufferSize := int(a.Pagesize * a.Npages)
    monitorEvents, err := perf.NewReader(a.events, bufferSize)

    // 2. 이벤트 루프
    for !isCtxDone(stopCtx) {
        record, err := monitorEvents.Read()
        // record 처리
        a.processPerfRecord(record)
    }
}

func (a *agent) processPerfRecord(record perf.Record) {
    if record.LostSamples > 0 {
        // 손실된 이벤트 알림
        a.notifyPerfEventLostLocked(record.LostSamples, record.CPU)
    } else {
        // 정상 이벤트를 모든 소비자에게 전달
        a.notifyPerfEventLocked(record.RawSample, record.CPU)
    }
}
```

### 10.3 Hubble Consumer: Monitor와 Observer 브릿지

소스: `pkg/hubble/monitor/consumer.go`

```go
type consumer struct {
    uuider   *bufuuid.Generator    // UUID 생성기
    observer Observer               // Hubble Observer

    lostEventCounter *counter.IntervalRangeCounter
    metricLostPerfEvents     prometheus.Counter
    metricLostObserverEvents prometheus.Counter
}

// BPF perf 이벤트를 MonitorEvent로 변환
func (c *consumer) NotifyPerfEvent(data []byte, cpu int) {
    c.sendEvent(func() any {
        return &observerTypes.PerfEvent{
            Data: data,
            CPU:  cpu,
        }
    })
}

func (c *consumer) sendEvent(payloader func() any) {
    // Observer의 이벤트 채널에 전송
    select {
    case c.observer.GetEventsChannel() <- c.newEvent(now, payloader):
        // 성공
    default:
        // 채널이 가득 찬 경우 손실 이벤트 카운터 증가
        c.incrementLostEventLocked(now)
    }
}
```

### 10.4 MonitorEvent 타입 체계

소스: `pkg/hubble/observer/types/types.go`

```go
type MonitorEvent struct {
    UUID      uuid.UUID    // 고유 식별자
    Timestamp time.Time    // 수신 시각
    NodeName  string       // 노드 이름
    Payload   any          // AgentEvent | PerfEvent | LostEvent
}

type PerfEvent struct {
    Data []byte  // BPF perf 이벤트 원시 데이터
    CPU  int     // CPU 번호
}

type AgentEvent struct {
    Type    int  // monitorAPI.MessageType* 값
    Message any  // accesslog.LogRecord 또는 AgentNotifyMessage
}

type LostEvent struct {
    Source        int      // 손실 발생 위치
    NumLostEvents uint64   // 손실 이벤트 수
    CPU           int      // CPU 번호
}
```

### 10.5 파서(Parser) 체계

소스: `pkg/hubble/parser/parser.go`

```go
type Parser struct {
    l34  *threefour.Parser   // L3/L4 파서 (Trace, Drop, PolicyVerdict)
    l7   *seven.Parser       // L7 파서 (HTTP, DNS, Kafka)
    dbg  *debug.Parser       // 디버그 이벤트 파서
    sock *sock.Parser        // 소켓 트레이스 파서
}

func (p *Parser) Decode(monitorEvent *observerTypes.MonitorEvent) (*v1.Event, error) {
    switch payload := monitorEvent.Payload.(type) {
    case *observerTypes.PerfEvent:
        switch payload.Data[0] {
        case monitorAPI.MessageTypeDebug:
            return p.dbg.Decode(payload.Data, payload.CPU)
        case monitorAPI.MessageTypeTraceSock:
            return p.sock.Decode(payload.Data, flow)
        default:
            return p.l34.Decode(payload.Data, flow)   // Trace, Drop, PolicyVerdict 등
        }
    case *observerTypes.AgentEvent:
        switch payload.Type {
        case monitorAPI.MessageTypeAccessLog:
            return p.l7.Decode(&logrecord, flow)       // L7 로그 (HTTP, DNS)
        case monitorAPI.MessageTypeAgent:
            return agent.NotifyMessageToProto(msg)     // 에이전트 알림
        }
    case *observerTypes.LostEvent:
        return lostEvent                                // 손실 이벤트
    }
}
```

### 10.6 BPF 이벤트 구조체

#### TraceNotify (패킷 추적)

소스: `pkg/monitor/datapath_trace.go`

```go
type TraceNotify struct {
    Type       uint8    // 메시지 타입 (MessageTypeTrace)
    ObsPoint   uint8    // 관측 지점 (FromLxc, ToNetwork 등)
    Source     uint16   // 소스 엔드포인트 ID
    Hash       uint32   // 플로우 해시
    OrigLen    uint32   // 원본 패킷 길이
    CapLen     uint16   // 캡처된 길이
    SrcLabel   uint32   // 소스 보안 ID
    DstLabel   uint32   // 목적지 보안 ID
    DstID      uint16   // 목적지 엔드포인트 ID
    Reason     uint8    // 추적 사유 (Policy, CtEstablished 등)
    Flags      uint8    // IPv6, L3Device, VXLAN 플래그
    Ifindex    uint32   // 네트워크 인터페이스 인덱스
    OrigIP     [16]byte // 원본 IP (NAT 전)
    IPTraceID  uint64   // IP 트레이스 ID
}
```

#### DropNotify (패킷 드롭)

소스: `pkg/monitor/datapath_drop.go`

```go
type DropNotify struct {
    Type       uint8    // 메시지 타입 (MessageTypeDrop)
    SubType    uint8    // 드롭 사유 코드
    Source     uint16   // 소스 엔드포인트 ID
    Hash       uint32   // 플로우 해시
    SrcLabel   uint32   // 소스 보안 ID
    DstLabel   uint32   // 목적지 보안 ID
    DstID      uint32   // 목적지 엔드포인트 ID
    Line       uint16   // BPF 소스 라인
    File       uint8    // BPF 소스 파일
    ExtError   int8     // 확장 오류 코드
    Ifindex    uint32   // 네트워크 인터페이스 인덱스
    IPTraceID  uint64   // IP 트레이스 ID
}
```

---

## 11. 디버깅 도구: gops, pprof, BPF 패킷 트레이스

### 11.1 hubble observe를 이용한 실시간 디버깅

```bash
# 특정 Pod의 모든 Flow 추적
hubble observe --from-pod default/my-pod --follow

# 드롭된 Flow만 확인
hubble observe --verdict DROPPED --follow

# 특정 드롭 사유 필터
hubble observe --drop-reason-desc POLICY_DENIED

# DNS 쿼리 모니터링
hubble observe --protocol dns --follow

# HTTP 요청 모니터링 (4xx/5xx 오류)
hubble observe --protocol http --http-status "4+" --http-status "5+"

# 특정 서비스 간 통신 추적
hubble observe --from-service default/frontend --to-service default/backend

# 전체 네임스페이스 모니터링
hubble observe --namespace kube-system --follow -o json

# IP 기반 추적
hubble observe --from-ip 10.0.0.1 --to-port 80

# 트래픽 방향 필터
hubble observe --traffic-direction ingress --to-pod default/nginx
```

### 11.2 hubble status

```bash
# Hubble 서버 상태 확인
hubble status
# 출력:
# Healthcheck (via unix:///var/run/cilium/hubble.sock): Ok
# Current/Max Flows: 8190/8190 (100.00%)
# Flows/s: 45.89
# Connected Nodes: 3/3
```

### 11.3 gops (Go 프로세스 진단)

```bash
# Cilium Agent의 gops 엔드포인트
gops stats <cilium-agent-pid>

# 고루틴 스택
gops stack <cilium-agent-pid>

# 메모리 통계
gops memstats <cilium-agent-pid>
```

### 11.4 pprof (성능 프로파일링)

```bash
# CPU 프로파일 (30초)
curl -s http://localhost:6060/debug/pprof/profile?seconds=30 > cpu.prof
go tool pprof cpu.prof

# 힙 프로파일
curl -s http://localhost:6060/debug/pprof/heap > heap.prof
go tool pprof heap.prof

# 고루틴 프로파일
curl -s http://localhost:6060/debug/pprof/goroutine > goroutine.prof
go tool pprof goroutine.prof

# 실시간 웹 UI
go tool pprof -http=:8080 cpu.prof
```

### 11.5 BPF 패킷 트레이스

```bash
# cilium monitor로 BPF 이벤트 직접 확인
cilium-dbg monitor --type trace

# 특정 엔드포인트의 모든 이벤트
cilium-dbg monitor --from <endpoint-id>

# 드롭 이벤트만 확인
cilium-dbg monitor --type drop

# 상세 모드
cilium-dbg monitor -v

# JSON 출력
cilium-dbg monitor -o json
```

### 11.6 Cilium 상태 확인 명령

```bash
# 전체 상태
cilium-dbg status --verbose

# 엔드포인트 목록
cilium-dbg endpoint list

# 특정 엔드포인트의 BPF 정책
cilium-dbg bpf policy get <endpoint-id>

# BPF 맵 내용 확인
cilium-dbg bpf ct list global
cilium-dbg bpf nat list

# 데이터패스 디버그 정보
cilium-dbg debuginfo
```

### 11.7 Prometheus 메트릭 기반 모니터링

```bash
# Hubble 메트릭 확인
curl -s http://localhost:9965/metrics | grep hubble_

# 주요 메트릭:
# hubble_flows_processed_total          - 처리된 Flow 총 수
# hubble_lost_events_total              - 손실된 이벤트 수
# hubble_dns_queries_total              - DNS 쿼리 수
# hubble_drop_total                     - 드롭된 패킷 수
# hubble_tcp_flags_total                - TCP 플래그별 통계
# hubble_http_requests_total            - HTTP 요청 수
# hubble_http_request_duration_seconds  - HTTP 응답 시간

# Cilium Agent 메트릭 확인
curl -s http://localhost:9962/metrics | grep cilium_

# 주요 메트릭:
# cilium_datapath_errors_total          - 데이터패스 오류
# cilium_bpf_map_ops_total              - BPF 맵 연산
# cilium_policy_import_errors_total     - 정책 가져오기 오류
# cilium_endpoint_count                 - 엔드포인트 수
```

---

## 요약

Cilium의 관측성 서브시스템은 **BPF 데이터패스**에서 시작하여 **Hubble Observer**, **Relay**, **CLI/UI**를 거쳐 사용자에게 도달하는 완전한 파이프라인을 구성한다.

핵심 설계 원칙:
1. **제로 오버헤드 수집**: BPF perf ring buffer를 통한 효율적인 이벤트 전달
2. **플러그인 아키텍처**: 훅 시스템과 메트릭 플러그인을 통한 확장성
3. **선언적 필터링**: 다양한 기준의 조합 가능한 필터 체계
4. **타임스탬프 정렬**: PriorityQueue를 이용한 멀티노드 이벤트 병합
5. **표준 통합**: Prometheus, OpenTelemetry 등 표준 관측성 도구와의 통합
