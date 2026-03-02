# PoC 13: Cilium 관측성(Observability) 서브시스템 시뮬레이션

## 개요

이 PoC는 Cilium의 관측성 서브시스템(Hubble)의 핵심 메커니즘을 순수 Go 표준 라이브러리만으로 시뮬레이션한다. 실제 Cilium 코드베이스의 아키텍처 패턴을 충실히 재현하여, BPF 이벤트 수집부터 메트릭 노출까지의 전체 파이프라인을 이해할 수 있도록 한다.

## 실행 방법

```bash
cd EDU/poc-13-observability
go run main.go
```

외부 의존성이 없으므로 Go 표준 라이브러리만으로 바로 실행 가능하다.

## 시뮬레이션 내용

### Demo 1: Hubble Observer Pipeline

BPF 이벤트가 Observer를 거쳐 Flow로 변환되는 전체 파이프라인을 시뮬레이션한다.

**시뮬레이션 구성요소:**
- **BPFEventGenerator**: BPF 데이터패스의 `send_trace_notify()`, `send_drop_notify()` 등을 시뮬레이션
- **MonitorEvent**: `pkg/hubble/observer/types/types.go`의 MonitorEvent 재현
- **FlowParser**: `pkg/hubble/parser/parser.go`의 Parser.Decode() 재현
- **LocalObserver**: `pkg/hubble/observer/local_observer.go`의 이벤트 루프 재현
- **RingBuffer**: `pkg/hubble/container/ring.go`의 Ring Buffer 재현

**실제 코드 매핑:**
| PoC | 실제 Cilium 소스 |
|-----|----------------|
| `BPFEventGenerator.GenerateEvent()` | `pkg/monitor/agent/agent.go` - `handleEvents()` |
| `MonitorEvent` | `pkg/hubble/observer/types/types.go` - `MonitorEvent` |
| `FlowParser.Decode()` | `pkg/hubble/parser/parser.go` - `Parser.Decode()` |
| `LocalObserver.Start()` | `pkg/hubble/observer/local_observer.go` - `Start()` |
| `RingBuffer.Write()` | `pkg/hubble/container/ring.go` - `Ring.Write()` |
| `OnDecodedFlowFunc` | `pkg/hubble/observer/observeroption/option.go` - `OnDecodedFlow` |

### Demo 2: Flow Filtering

다양한 조건으로 Flow를 필터링하는 기능을 시뮬레이션한다.

**지원 필터:**
- Verdict (FORWARDED, DROPPED, AUDIT 등)
- 소스/목적지 Pod 이름
- 소스/목적지 Namespace
- Protocol (TCP, UDP, HTTP, DNS 등)
- Drop Reason

**실제 코드 매핑:**
| PoC | 실제 Cilium 소스 |
|-----|----------------|
| `FlowFilter` | `api/v1/flow/flow.proto` - `FlowFilter` |
| `matchFilters()` | `pkg/hubble/filters/filters.go` - `Apply()` |

### Demo 3: Hubble Relay - 멀티노드 집계

여러 노드의 Flow를 수집하고 타임스탬프 기준으로 정렬하는 Relay를 시뮬레이션한다.

**시뮬레이션 구성요소:**
- **HubbleRelay**: 3개 노드의 Observer에서 Flow 수집
- **PriorityQueue**: `container/heap` 기반 최소 힙으로 타임스탬프 정렬
- **FlowResponse**: 노드 이름이 포함된 응답

**실제 코드 매핑:**
| PoC | 실제 Cilium 소스 |
|-----|----------------|
| `HubbleRelay.GetFlows()` | `pkg/hubble/relay/observer/observer.go` - `retrieveFlowsFromPeer()` + `sortFlows()` |
| `PriorityQueue` | `pkg/hubble/relay/queue/priority_queue.go` - `PriorityQueue` |

### Demo 4: Prometheus Metrics

Hubble 메트릭 시스템을 시뮬레이션한다.

**구현된 메트릭 타입:**
- **Counter**: 단조 증가 카운터 (`hubble_flows_processed_total`, `hubble_drop_total` 등)
- **Gauge**: 임의 값 게이지 (`hubble_active_flows`, `hubble_connected_nodes`)
- **Histogram**: 버킷 기반 히스토그램 (`hubble_http_request_duration_seconds`)

**실제 코드 매핑:**
| PoC | 실제 Cilium 소스 |
|-----|----------------|
| `HubbleMetrics.ProcessFlow()` | `pkg/hubble/metrics/flow/handler.go` - `ProcessFlow()` |
| `Counter` | Prometheus `CounterVec` |
| `Gauge` | Prometheus `Gauge` |
| `Histogram` | Prometheus `HistogramVec` |

### Demo 5: Service Dependency Map

Flow 데이터에서 서비스 간 의존성 그래프를 구축하는 것을 시뮬레이션한다.

**시뮬레이션 구성요소:**
- **ServiceMap**: 서비스 간 통신 엣지 추적
- **ServiceEdge**: 소스-목적지 쌍의 통신 통계

### Demo 6: 이벤트 수집 경로 시각화

BPF 데이터패스에서 사용자 API까지의 전체 이벤트 수집 경로를 시각적으로 표시한다.

## 아키텍처 다이어그램

```
+--------------------------------------------------+
|              Hubble CLI / UI                      |
+--------------------------------------------------+
|                Hubble Relay                        |
|  (PriorityQueue 기반 멀티노드 Flow 집계)           |
+--------------------------------------------------+
|    Node 1 Observer  |  Node 2 Observer  |  ...    |
|    (Ring Buffer)    |  (Ring Buffer)    |         |
+--------------------------------------------------+
|              Flow Parser (L3/4, L7)               |
+--------------------------------------------------+
|           Hubble Monitor Consumer                  |
+--------------------------------------------------+
|            Monitor Agent (perf.Reader)             |
+--------------------------------------------------+
|         BPF Datapath (monitor_output map)          |
+--------------------------------------------------+
```

## 핵심 설계 패턴

### 1. 훅 기반 확장 (Observer Option Pattern)
Observer는 `OnDecodedFlow`, `OnMonitorEvent` 등의 훅을 통해 파이프라인을 확장한다. 이 PoC에서는 메트릭 수집과 서비스 맵 갱신을 훅으로 구현했다.

### 2. Lock-free Ring Buffer
실제 Cilium의 Ring Buffer는 `atomic.Uint64`와 비트 마스킹으로 lock-free 읽기를 구현한다. 이 PoC에서는 `sync.RWMutex`로 단순화했다.

### 3. PriorityQueue 기반 정렬
Relay는 `container/heap`을 사용한 최소 힙으로 여러 노드의 Flow를 타임스탬프 순으로 병합한다.

### 4. 플러그인 메트릭 시스템
각 메트릭 핸들러는 `Handler` 인터페이스를 구현하며, `ProcessFlow()` 메서드로 Flow를 처리한다.

## 관련 실제 코드 파일

| 파일 | 설명 |
|------|------|
| `pkg/hubble/observer/local_observer.go` | Observer 이벤트 루프 |
| `pkg/hubble/container/ring.go` | Ring Buffer 구현 |
| `pkg/hubble/parser/parser.go` | BPF 이벤트 파서 |
| `pkg/hubble/relay/observer/observer.go` | Relay Flow 수집/정렬 |
| `pkg/hubble/relay/queue/priority_queue.go` | PriorityQueue |
| `pkg/hubble/metrics/metrics.go` | 메트릭 초기화 |
| `pkg/hubble/metrics/flow/handler.go` | Flow 메트릭 핸들러 |
| `pkg/hubble/exporter/exporter.go` | Flow Exporter |
| `pkg/hubble/monitor/consumer.go` | Monitor-Observer 브릿지 |
| `pkg/monitor/agent/agent.go` | BPF perf reader |
| `pkg/monitor/datapath_trace.go` | TraceNotify 구조체 |
| `pkg/monitor/datapath_drop.go` | DropNotify 구조체 |
| `hubble/cmd/observe/flows.go` | CLI observe 명령 |
| `api/v1/flow/flow.pb.go` | Flow protobuf 정의 |
| `pkg/hubble/observer/observeroption/option.go` | Observer 옵션/훅 |
