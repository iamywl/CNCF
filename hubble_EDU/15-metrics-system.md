# 15. 메트릭 시스템 (Metrics System)

## 개요

Hubble 메트릭 시스템은 네트워크 플로우 이벤트를 Prometheus 메트릭으로 변환하는
파이프라인이다. 플러그인 아키텍처를 채택하여 DNS, HTTP, TCP, drop, flow, ICMP,
policy, port-distribution, SCTP, flows-to-world 등 10개의 내장 메트릭 핸들러를
제공하며, 정적(Static) 및 동적(Dynamic) 프로세서를 통해 런타임 재설정을 지원한다.

이 문서에서는 메트릭 시스템의 아키텍처, Plugin/Handler 인터페이스, Registry,
FlowProcessor 구현, ConfigWatcher, 각 메트릭 핸들러의 동작을 소스코드 수준에서
분석한다.

## 아키텍처 개요

```
+-------------------+
|   Monitor Agent   |
| (eBPF perf events)|
+--------+----------+
         |
         v
+--------+----------+
|   Hubble Observer  |
|                    |
| OnDecodedFlow 훅  |
+--------+----------+
         |
         v
+--------+----------+       +-----------------------+
|  FlowProcessor    |       |  ConfigWatcher        |
|  (Static/Dynamic) |<------| (동적 메트릭 재설정)  |
+--------+----------+       +-----------------------+
         |
    +----+----+----+----+----+
    |    |    |    |    |    |
    v    v    v    v    v    v
  DNS  HTTP  TCP  drop flow policy  ...
  Handler Handler Handler  ...
    |    |    |    |    |    |
    v    v    v    v    v    v
+-----------------------------------+
|   Prometheus Registry             |
|   (Counter, Histogram, Gauge)     |
+--------+--------------------------+
         |
         v
+--------+----------+
|  /metrics HTTP     |
|  (promhttp)        |
+--------------------+
```

## 핵심 인터페이스

### FlowProcessor 인터페이스

```go
// 소스: cilium/pkg/hubble/metrics/metrics.go

type FlowProcessor interface {
    ProcessFlow(ctx context.Context, flow *pb.Flow) error
}
```

FlowProcessor는 Static과 Dynamic 두 가지 구현이 있다.

### Plugin 인터페이스

```go
// 소스: cilium/pkg/hubble/metrics/api/api.go

type Plugin interface {
    // 새 메트릭 핸들러 인스턴스 생성
    NewHandler() Handler
    // 사람이 읽을 수 있는 도움말
    HelpText() string
}

// 다른 플러그인과의 충돌 선언 (선택적)
type PluginConflicts interface {
    ConflictingPlugins() []string
}
```

### Handler 인터페이스

```go
// 소스: cilium/pkg/hubble/metrics/api/api.go

type Handler interface {
    // 메트릭 핸들러 초기화, Prometheus 레지스트리에 메트릭 등록
    Init(registry *prometheus.Registry, options *MetricConfig) error

    // 이 핸들러가 사용하는 MetricVec 목록 반환
    ListMetricVec() []*prometheus.MetricVec

    // 핸들러의 컨텍스트 옵션 반환
    Context() *ContextOptions

    // 설정 상태 문자열 반환
    Status() string

    // 런타임 설정 업데이트
    HandleConfigurationUpdate(cfg *MetricConfig) error

    // Prometheus 레지스트리에서 메트릭 해제 및 정리
    Deinit(registry *prometheus.Registry) error

    // 플로우 이벤트 처리 및 메트릭 계산
    ProcessFlow(ctx context.Context, flow *pb.Flow) error
}
```

Handler 인터페이스는 메트릭 핸들러의 전체 생명주기를 관리한다:

```
Init() -> ProcessFlow()* -> HandleConfigurationUpdate()* -> Deinit()
  |            |                      |                        |
  |         반복 호출              필요시 호출                 종료시
  |            |                      |                        |
  +-- 메트릭 등록   메트릭 업데이트    필터 재설정          메트릭 해제
```

## Registry (플러그인 레지스트리)

### 구조체

```go
// 소스: cilium/pkg/hubble/metrics/api/registry.go

type Registry struct {
    mutex    lock.Mutex
    handlers map[string]Plugin
}

type NamedHandler struct {
    Name         string
    Handler      Handler
    MetricConfig *MetricConfig
}
```

### 플러그인 등록

각 메트릭 패키지는 `init()` 함수에서 자신을 레지스트리에 등록한다:

```go
// 소스: cilium/pkg/hubble/metrics/metrics.go

import (
    _ "github.com/cilium/cilium/pkg/hubble/metrics/dns"               // invoke init
    _ "github.com/cilium/cilium/pkg/hubble/metrics/drop"              // invoke init
    _ "github.com/cilium/cilium/pkg/hubble/metrics/flow"              // invoke init
    _ "github.com/cilium/cilium/pkg/hubble/metrics/flows-to-world"    // invoke init
    _ "github.com/cilium/cilium/pkg/hubble/metrics/http"              // invoke init
    _ "github.com/cilium/cilium/pkg/hubble/metrics/icmp"              // invoke init
    _ "github.com/cilium/cilium/pkg/hubble/metrics/policy"            // invoke init
    _ "github.com/cilium/cilium/pkg/hubble/metrics/port-distribution" // invoke init
    _ "github.com/cilium/cilium/pkg/hubble/metrics/sctp"              // invoke init
    _ "github.com/cilium/cilium/pkg/hubble/metrics/tcp"               // invoke init
)
```

### 핸들러 설정 및 생성

```go
// 소스: cilium/pkg/hubble/metrics/api/registry.go

func (r *Registry) ConfigureHandlers(
    logger *slog.Logger,
    registry *prometheus.Registry,
    enabled *Config,
) (*[]NamedHandler, error) {
    r.mutex.Lock()
    defer r.mutex.Unlock()

    var enabledHandlers []NamedHandler
    metricNames := enabled.GetMetricNames()
    for _, metricsConfig := range enabled.Metrics {
        h, err := r.validateAndCreateHandlerLocked(metricsConfig, &metricNames)
        if err != nil {
            var errM *errMetricNotExist
            if errors.As(err, &errM) {
                logger.Warn("Skipping unknown hubble metric", ...)
                continue
            }
            return nil, err
        }
        enabledHandlers = append(enabledHandlers, *h)
    }

    return InitHandlers(logger, registry, &enabledHandlers)
}
```

### 플러그인 충돌 검사

```go
// 소스: cilium/pkg/hubble/metrics/api/registry.go

func (r *Registry) validateAndCreateHandlerLocked(
    metricsConfig *MetricConfig,
    metricNames *map[string]*MetricConfig,
) (*NamedHandler, error) {
    plugin, ok := r.handlers[metricsConfig.Name]
    if !ok {
        return nil, &errMetricNotExist{metricsConfig.Name}
    }

    // 충돌하는 플러그인이 이미 활성화되어 있는지 확인
    if cp, ok := plugin.(PluginConflicts); ok {
        for _, conflict := range cp.ConflictingPlugins() {
            if _, conflictExists := (*metricNames)[conflict]; conflictExists {
                return nil, fmt.Errorf(
                    "plugin %s conflicts with plugin %s",
                    metricsConfig.Name, conflict)
            }
        }
    }

    h := NamedHandler{
        Name:         metricsConfig.Name,
        Handler:      plugin.NewHandler(),
        MetricConfig: metricsConfig,
    }
    return &h, nil
}
```

## MetricConfig (설정 구조)

```go
// 소스: cilium/pkg/hubble/metrics/api/api.go

type MetricConfig struct {
    Name                 string                 `yaml:"name"`
    ContextOptionConfigs []*ContextOptionConfig `yaml:"contextOptions"`
    IncludeFilters       []*pb.FlowFilter       `yaml:"includeFilters"`
    ExcludeFilters       []*pb.FlowFilter       `yaml:"excludeFilters"`
}

type Config struct {
    Metrics []*MetricConfig `yaml:"metrics"`
}
```

### 정적 설정 파싱

CLI 플래그에서 정적 메트릭을 파싱한다:

```go
// 소스: cilium/pkg/hubble/metrics/api/api.go

func ParseStaticMetricsConfig(enabledMetrics []string) (metricConfigs *Config) {
    metricConfigs = &Config{}
    for _, metric := range enabledMetrics {
        s := strings.SplitN(metric, ":", 2)
        config := &MetricConfig{
            Name:                 s[0],
            IncludeFilters:       []*pb.FlowFilter{},
            ExcludeFilters:       []*pb.FlowFilter{},
            ContextOptionConfigs: []*ContextOptionConfig{},
        }
        if len(s) == 2 {
            config.ContextOptionConfigs = parseOptionConfigs(s[1])
        }
        metricConfigs.Metrics = append(metricConfigs.Metrics, config)
    }
    return
}
```

### 설정 문자열 형식

```
메트릭이름:옵션1=값1;옵션2=값2a|값2b

예시:
  dns:query;ignoreAAAA
  http:sourceContext=namespace|pod;exemplars=true
  drop:sourceContext=pod;destinationContext=pod
  flow:sourceContext=namespace;destinationContext=namespace
```

옵션 값 구분자:
- `;` (세미콜론): 옵션 간 구분
- `=` (등호): 키-값 구분
- `|` (파이프): 값 내 다중 선택

## StaticFlowProcessor

```go
// 소스: cilium/pkg/hubble/metrics/flow_processor.go

type StaticFlowProcessor struct {
    logger  *slog.Logger
    metrics []api.NamedHandler
}

func (p *StaticFlowProcessor) ProcessFlow(ctx context.Context, flow *flowpb.Flow) error {
    _, err := p.OnDecodedFlow(ctx, flow)
    return err
}

func (p *StaticFlowProcessor) OnDecodedFlow(
    ctx context.Context,
    flow *flowpb.Flow,
) (bool, error) {
    if len(p.metrics) == 0 {
        return false, nil
    }

    var errs error
    for _, nh := range p.metrics {
        // 하나의 핸들러가 실패해도 나머지는 계속 실행
        errs = errors.Join(errs, nh.Handler.ProcessFlow(ctx, flow))
    }
    if errs != nil {
        p.logger.Error("Failed to ProcessFlow in metrics handler", ...)
    }
    return false, nil
}
```

StaticFlowProcessor의 특징:
- 에이전트 시작 시 설정이 고정됨
- 모든 핸들러를 순차적으로 호출
- 개별 핸들러의 실패가 다른 핸들러에 영향을 주지 않음 (`errors.Join`)

## DynamicFlowProcessor

```go
// 소스: cilium/pkg/hubble/metrics/dynamic_flow_processor.go

type DynamicFlowProcessor struct {
    logger   *slog.Logger
    watcher  *metricConfigWatcher
    mutex    lock.RWMutex
    Metrics  []api.NamedHandler
    registry *prometheus.Registry
}
```

### 동적 재설정 메커니즘

```go
// 소스: cilium/pkg/hubble/metrics/dynamic_flow_processor.go

func (d *DynamicFlowProcessor) onConfigReload(
    ctx context.Context,
    hash uint64,
    config api.Config,
) {
    d.mutex.Lock()
    defer d.mutex.Unlock()

    var newHandlers []api.NamedHandler
    metricNames := config.GetMetricNames()

    curHandlerMap := make(map[string]*api.NamedHandler)
    if d.Metrics != nil {
        for _, m := range d.Metrics {
            curHandlerMap[m.Name] = &m
        }

        // 1단계: 새 설정에 없는 핸들러 해제
        for _, m := range d.Metrics {
            if _, ok := metricNames[m.Name]; !ok {
                h := curHandlerMap[m.Name]
                h.Handler.Deinit(d.registry)
                delete(curHandlerMap, m.Name)
            }
        }
    }

    // 2단계: 기존 핸들러 업데이트 또는 새 핸들러 추가
    for _, cm := range config.Metrics {
        if m, ok := curHandlerMap[cm.Name]; ok {
            if reflect.DeepEqual(*m.MetricConfig, *cm) {
                continue  // 변경 없음
            }
            m.Handler.HandleConfigurationUpdate(cm)
            m.MetricConfig = cm
        } else {
            d.addNewMetric(d.registry, cm, metricNames, &newHandlers)
        }
    }

    for _, v := range curHandlerMap {
        newHandlers = append(newHandlers, *v)
    }
    d.Metrics = newHandlers
}
```

### 재설정 흐름

```
ConfigWatcher (10초 주기)
    |
    +-- YAML 설정 파일 읽기
    +-- MD5 해시 비교 (변경 감지)
    |
    +-- 변경됨?
        |
        +-- YES: onConfigReload() 호출
        |       |
        |       +-- 1. 삭제된 메트릭: Deinit() 호출
        |       +-- 2. 변경된 메트릭: HandleConfigurationUpdate() 호출
        |       +-- 3. 새 메트릭: addNewMetric() -> Init() 호출
        |
        +-- NO: 무시
```

### RWMutex 보호

```go
// 소스: cilium/pkg/hubble/metrics/dynamic_flow_processor.go

func (d *DynamicFlowProcessor) ProcessFlow(ctx context.Context, flow *flowpb.Flow) error {
    // ...
    d.mutex.RLock()     // 읽기 잠금 (ProcessFlow 동시 실행 가능)
    defer d.mutex.RUnlock()
    // ...
}

func (d *DynamicFlowProcessor) onConfigReload(...) {
    d.mutex.Lock()      // 쓰기 잠금 (설정 변경 시 독점)
    defer d.mutex.Unlock()
    // ...
}
```

읽기-쓰기 잠금으로 ProcessFlow의 높은 동시성을 유지하면서 설정 변경의 안전성을 보장한다.

## MetricConfigWatcher

```go
// 소스: cilium/pkg/hubble/metrics/metric_config_watcher.go

type metricConfigWatcher struct {
    logger         *slog.Logger
    configFilePath string
    callback       func(ctx context.Context, hash uint64, config api.Config)
    ticker         *time.Ticker
    stop           chan bool
    currentCfgHash uint64
    cfgStore       map[string]*api.MetricConfig
    mutex          lock.RWMutex
}
```

### 해시 기반 변경 감지

```go
// 소스: cilium/pkg/hubble/metrics/metric_config_watcher.go

func calculateMetricHash(file []byte) uint64 {
    sum := md5.Sum(file)
    return binary.LittleEndian.Uint64(sum[0:16])
}
```

MD5 해시의 첫 8바이트를 uint64로 변환하여 설정 파일의 변경을 감지한다.

### 설정 검증

```go
// 소스: cilium/pkg/hubble/metrics/metric_config_watcher.go

func (c *metricConfigWatcher) validateMetricConfig(config *api.Config) error {
    metrics := make(map[string]any)
    var errs error

    for i, newMetric := range config.Metrics {
        if newMetric.Name == "" {
            errs = errors.Join(errs, fmt.Errorf(
                "metric config validation failed - missing metric name at: %d", i))
            continue
        }
        if _, ok := metrics[newMetric.Name]; ok {
            errs = errors.Join(errs, fmt.Errorf(
                "metric config validation failed - duplicate metric specified: %v",
                newMetric.Name))
        }
        metrics[newMetric.Name] = struct{}{}

        // 레이블 집합 변경 방지 (Prometheus 레지스트리 제약)
        if oldMetric, ok := c.cfgStore[newMetric.Name]; ok {
            if !reflect.DeepEqual(newMetric.ContextOptionConfigs,
                                  oldMetric.ContextOptionConfigs) {
                errs = errors.Join(errs, fmt.Errorf(
                    "label set cannot be changed without restarting Prometheus. metric: %v",
                    newMetric.Name))
            }
        }
    }
    return errs
}
```

검증 규칙:
1. 메트릭 이름이 비어있으면 안 됨
2. 중복 메트릭 이름 불가
3. 기존 메트릭의 레이블 집합(ContextOptionConfigs) 변경 불가 (Prometheus 제약)

## 메트릭 핸들러 상세

### DNS Handler

```go
// 소스: cilium/pkg/hubble/metrics/dns/handler.go

type dnsHandler struct {
    includeQuery bool
    ignoreAAAA   bool
    context      *api.ContextOptions
    AllowList    filters.FilterFuncs
    DenyList     filters.FilterFuncs

    queries       *prometheus.CounterVec   // hubble_dns_queries_total
    responses     *prometheus.CounterVec   // hubble_dns_responses_total
    responseTypes *prometheus.CounterVec   // hubble_dns_response_types_total
}
```

#### 메트릭 정의

| 메트릭 | 레이블 | 설명 |
|--------|--------|------|
| `hubble_dns_queries_total` | context + rcode, qtypes, ips_returned[, query] | DNS 쿼리 수 |
| `hubble_dns_responses_total` | context + rcode, qtypes, ips_returned[, query] | DNS 응답 수 |
| `hubble_dns_response_types_total` | context + type, qtypes[, query] | DNS 응답 유형별 수 |

#### 처리 로직

```go
// 소스: cilium/pkg/hubble/metrics/dns/handler.go

func (h *dnsHandler) ProcessFlow(ctx context.Context, flow *flowpb.Flow) error {
    if flow.GetL7() == nil { return nil }
    dns := flow.GetL7().GetDns()
    if dns == nil { return nil }

    // AAAA 레코드 무시 옵션
    if h.ignoreAAAA && len(dns.Qtypes) == 1 && dns.Qtypes[0] == "AAAA" {
        return nil
    }

    // AllowList/DenyList 필터 적용
    if !filters.Apply(h.AllowList, h.DenyList, ...) { return nil }

    switch {
    case flow.GetVerdict() == flowpb.Verdict_DROPPED:
        // rcode = "Policy denied"
        h.queries.WithLabelValues(labels...).Inc()
    case !flow.GetIsReply().GetValue():  // DNS 요청
        h.queries.WithLabelValues(labels...).Inc()
    case flow.GetIsReply().GetValue():   // DNS 응답
        h.responses.WithLabelValues(labels...).Inc()
        for _, responseType := range dns.Rrtypes {
            h.responseTypes.WithLabelValues(newLabels...).Inc()
        }
    }
    return nil
}
```

### HTTP Handler

```go
// 소스: cilium/pkg/hubble/metrics/http/handler.go

type httpHandler struct {
    requests  *prometheus.CounterVec   // hubble_http_requests_total
    responses *prometheus.CounterVec   // hubble_http_responses_total (V1만)
    duration  *prometheus.HistogramVec // hubble_http_request_duration_seconds
    context   *api.ContextOptions
    useV2     bool
    exemplars bool
    // ...
}
```

#### V1 vs V2 메트릭

| 버전 | 메트릭 | 레이블 |
|------|--------|--------|
| V1 | requests_total | context + method, protocol, reporter |
| V1 | responses_total | context + method, protocol, status, reporter |
| V1 | request_duration_seconds | context + method, reporter |
| V2 | requests_total | context + method, protocol, status, reporter |
| V2 | request_duration_seconds | context + method, reporter |

V2는 요청과 응답을 하나의 requests_total 메트릭에 합쳐 status를 포함한다.

#### Exemplar 지원

```go
// 소스: cilium/pkg/hubble/metrics/http/handler.go

func incrementCounter(c prometheus.Counter, traceID string) {
    if adder, ok := c.(prometheus.ExemplarAdder); ok && traceID != "" {
        adder.AddWithExemplar(1, prometheus.Labels{"traceID": traceID})
    } else {
        c.Inc()
    }
}
```

OpenTelemetry trace ID를 Prometheus Exemplar로 연결하여 메트릭에서 분산 추적으로
직접 이동할 수 있다.

### TCP Handler

```go
// 소스: cilium/pkg/hubble/metrics/tcp/handler.go

type tcpHandler struct {
    tcpFlags  *prometheus.CounterVec  // hubble_tcp_flags_total
    context   *api.ContextOptions
    // ...
}
```

| 메트릭 | 레이블 | 설명 |
|--------|--------|------|
| `hubble_tcp_flags_total` | flag, family + context | TCP 플래그 발생 수 |

추적하는 TCP 플래그: `SYN`, `SYN-ACK`, `FIN`, `RST`

```go
// 소스: cilium/pkg/hubble/metrics/tcp/handler.go

func (h *tcpHandler) ProcessFlow(ctx context.Context, flow *flowpb.Flow) error {
    // FORWARDED 또는 REDIRECTED만 처리
    if (flow.GetVerdict() != flowpb.Verdict_FORWARDED &&
        flow.GetVerdict() != flowpb.Verdict_REDIRECTED) || flow.GetL4() == nil {
        return nil
    }

    if tcp.Flags.SYN {
        if tcp.Flags.ACK {
            labels[0] = "SYN-ACK"
        } else {
            labels[0] = "SYN"
        }
        h.tcpFlags.WithLabelValues(labels...).Inc()
    }
    // FIN, RST도 유사하게 처리
}
```

### Drop Handler

```go
// 소스: cilium/pkg/hubble/metrics/drop/handler.go

type dropHandler struct {
    drops   *prometheus.CounterVec  // hubble_drop_total
    context *api.ContextOptions
    // ...
}
```

| 메트릭 | 레이블 | 설명 |
|--------|--------|------|
| `hubble_drop_total` | context + reason, protocol | 드롭 수 |

```go
func (h *dropHandler) ProcessFlow(ctx context.Context, flow *flowpb.Flow) error {
    if flow.GetVerdict() != flowpb.Verdict_DROPPED { return nil }

    labels := append(contextLabels,
        flow.GetDropReasonDesc().String(),
        v1.FlowProtocol(flow))
    h.drops.WithLabelValues(labels...).Inc()
    return nil
}
```

### Flow Handler

```go
// 소스: cilium/pkg/hubble/metrics/flow/handler.go

type flowHandler struct {
    flows   *prometheus.CounterVec  // hubble_flows_processed_total
    context *api.ContextOptions
    // ...
}
```

| 메트릭 | 레이블 | 설명 |
|--------|--------|------|
| `hubble_flows_processed_total` | protocol, type, subtype, verdict + context | 처리된 플로우 수 |

type/subtype 분류:
- `L7/DNS`, `L7/HTTP`, `L7/Kafka`
- `Drop`
- `Capture`
- `Trace/<observation_point>`
- `PolicyVerdict`

### Policy Handler

```go
// 소스: cilium/pkg/hubble/metrics/policy/handler.go

type policyHandler struct {
    verdicts *prometheus.CounterVec  // hubble_policy_verdicts_total
    context  *api.ContextOptions
    // ...
}
```

| 메트릭 | 레이블 | 설명 |
|--------|--------|------|
| `hubble_policy_verdicts_total` | direction, match, action + context | 정책 판정 수 |

L3/L4와 L7 정책 판정을 별도로 처리한다:

```go
// L3/L4: direction=ingress, match=l3-l4, action=forwarded
// L7:    direction=egress, match=l7/http, action=dropped
```

## 전역 메트릭

```go
// 소스: cilium/pkg/hubble/metrics/metrics.go

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

| 메트릭 | 유형 | 설명 |
|--------|------|------|
| `hubble_lost_events_total` | Counter | 이벤트 손실 수 (소스별) |
| `hubble_metrics_http_handler_requests_total` | Counter | 메트릭 HTTP 핸들러 요청 수 |
| `hubble_metrics_http_handler_request_duration_seconds` | Histogram | 메트릭 핸들러 지연 시간 |

## 초기화 흐름

```go
// 소스: cilium/pkg/hubble/metrics/metrics.go

func InitMetrics(
    logger *slog.Logger,
    reg *prometheus.Registry,
    enabled *api.Config,
    grpcMetrics *grpc_prometheus.ServerMetrics,
) error {
    e, err := InitMetricHandlers(logger, reg, enabled)
    if err != nil { return err }
    EnabledMetrics = *e

    reg.MustRegister(grpcMetrics)
    reg.MustRegister(LostEvents)
    reg.MustRegister(RequestsTotal)
    reg.MustRegister(RequestDuration)

    initEndpointDeletionHandler()
    return nil
}
```

### HTTP 서버

```go
// 소스: cilium/pkg/hubble/metrics/metrics.go

func ServerHandler(reg *prometheus.Registry, enableOpenMetrics bool) http.Handler {
    mux := http.NewServeMux()
    handler := promhttp.HandlerFor(reg, promhttp.HandlerOpts{
        EnableOpenMetrics: enableOpenMetrics,
    })
    handler = promhttp.InstrumentHandlerCounter(RequestsTotal, handler)
    handler = promhttp.InstrumentHandlerDuration(RequestDuration, handler)
    mux.Handle("/metrics", handler)
    return mux
}
```

## CiliumEndpoint 삭제 처리

```go
// 소스: cilium/pkg/hubble/metrics/metrics.go

type CiliumEndpointDeletionHandler struct {
    gracefulPeriod time.Duration
    queue          workqueue.TypedDelayingInterface[*types.CiliumEndpoint]
}
```

Pod가 삭제되면 관련 메트릭 레이블을 정리해야 한다. 즉시 삭제하지 않고
`gracefulPeriod`(기본 1분) 후에 삭제하여 마지막 스크레이핑을 보장한다.

```
Pod 삭제 이벤트
    |
    +-- ProcessCiliumEndpointDeletion()
    |       |
    |       +-- queue.AddAfter(pod, 1분)  // 1분 지연
    |
    ... (1분 후) ...
    |
    +-- queue.Get()
    |       |
    |       +-- ProcessCiliumEndpointDeletion()
    |       |       |
    |       |       +-- 각 핸들러의 ListMetricVec() 호출
    |       |       +-- 해당 Pod 관련 레이블 조합의 메트릭 삭제
```

## 메트릭 핸들러 요약

| 핸들러 | 메트릭 이름 | 유형 | 주요 레이블 |
|--------|-------------|------|-------------|
| dns | dns_queries_total | Counter | rcode, qtypes, query |
| dns | dns_responses_total | Counter | rcode, qtypes |
| dns | dns_response_types_total | Counter | type, qtypes |
| http | http_requests_total | Counter | method, protocol, status |
| http | http_responses_total | Counter | method, protocol, status |
| http | http_request_duration_seconds | Histogram | method |
| tcp | tcp_flags_total | Counter | flag, family |
| drop | drop_total | Counter | reason, protocol |
| flow | flows_processed_total | Counter | protocol, type, subtype, verdict |
| policy | policy_verdicts_total | Counter | direction, match, action |
| icmp | icmp_total | Counter | family, type |
| port-distribution | port_distribution_total | Counter | protocol, port |
| sctp | sctp_total | Counter | chunk_type |
| flows-to-world | flows_to_world_total | Counter | protocol, verdict |

## 정리

Hubble 메트릭 시스템의 핵심 설계 원칙:

1. **플러그인 아키텍처**: `init()` 기반 자동 등록으로 확장 용이
2. **이중 프로세서**: Static(고정)과 Dynamic(동적) 프로세서로 유연한 운영
3. **필터링**: 각 핸들러가 AllowList/DenyList 필터를 가져 특정 플로우만 계량
4. **컨텍스트 레이블**: ContextOptions로 소스/목적지 네임스페이스, Pod 등을 레이블에 추가
5. **안전한 정리**: Pod 삭제 시 graceful period 후 메트릭 레이블 정리
6. **Exemplar 통합**: HTTP 메트릭에서 분산 추적 trace ID 연결 지원

### 파일 참조

| 파일 | 경로 |
|------|------|
| 메트릭 초기화 | `cilium/pkg/hubble/metrics/metrics.go` |
| API 인터페이스 | `cilium/pkg/hubble/metrics/api/api.go` |
| 레지스트리 | `cilium/pkg/hubble/metrics/api/registry.go` |
| Static 프로세서 | `cilium/pkg/hubble/metrics/flow_processor.go` |
| Dynamic 프로세서 | `cilium/pkg/hubble/metrics/dynamic_flow_processor.go` |
| Config Watcher | `cilium/pkg/hubble/metrics/metric_config_watcher.go` |
| DNS 핸들러 | `cilium/pkg/hubble/metrics/dns/handler.go` |
| HTTP 핸들러 | `cilium/pkg/hubble/metrics/http/handler.go` |
| TCP 핸들러 | `cilium/pkg/hubble/metrics/tcp/handler.go` |
| Drop 핸들러 | `cilium/pkg/hubble/metrics/drop/handler.go` |
| Flow 핸들러 | `cilium/pkg/hubble/metrics/flow/handler.go` |
| Policy 핸들러 | `cilium/pkg/hubble/metrics/policy/handler.go` |
