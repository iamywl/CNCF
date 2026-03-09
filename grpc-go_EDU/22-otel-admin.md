# 22. OpenTelemetry 연동 & Admin API 심화

## 목차
1. [개요](#1-개요)
2. [OTel 플러그인 아키텍처](#2-otel-플러그인-아키텍처)
3. [클라이언트 메트릭 계측](#3-클라이언트-메트릭-계측)
4. [서버 메트릭 계측](#4-서버-메트릭-계측)
5. [분산 트레이싱 통합](#5-분산-트레이싱-통합)
6. [메트릭 레지스트리 시스템](#6-메트릭-레지스트리-시스템)
7. [CSM(Cloud Service Mesh) 통합](#7-csmcloud-service-mesh-통합)
8. [Admin API 아키텍처](#8-admin-api-아키텍처)
9. [Channelz 서비스 등록](#9-channelz-서비스-등록)
10. [CSDS 통합](#10-csds-통합)
11. [실제 활용 패턴](#11-실제-활용-패턴)
12. [설계 철학과 Why](#12-설계-철학과-why)

---

## 1. 개요

gRPC-Go의 OpenTelemetry 연동은 **메트릭**과 **트레이싱** 두 축으로 RPC를 관찰할 수 있게 해주며, Admin API는 Channelz와 CSDS를 하나의 진입점으로 통합한다.

### 핵심 소스 경로

| 컴포넌트 | 소스 경로 |
|----------|----------|
| OTel 패키지 | `stats/opentelemetry/opentelemetry.go` |
| 클라이언트 메트릭 | `stats/opentelemetry/client_metrics.go` |
| 서버 메트릭 | `stats/opentelemetry/server_metrics.go` |
| 클라이언트 트레이싱 | `stats/opentelemetry/client_tracing.go` |
| 서버 트레이싱 | `stats/opentelemetry/server_tracing.go` |
| Trace 유틸 | `stats/opentelemetry/trace.go` |
| CSM 플러그인 | `stats/opentelemetry/csm/` |
| Admin 패키지 | `admin/admin.go` |
| Internal Admin | `internal/admin/` |

---

## 2. OTel 플러그인 아키텍처

### 2.1 전체 구조

```
┌─────────────────────────────────────────────────────────────┐
│                      Application Code                        │
│                                                              │
│  grpc.NewClient(                                             │
│      opentelemetry.DialOption(Options{                       │
│          MetricsOptions: MetricsOptions{                     │
│              MeterProvider: mp,                              │
│          },                                                  │
│          TraceOptions: TraceOptions{                          │
│              TracerProvider: tp,                              │
│              TextMapPropagator: prop,                         │
│          },                                                  │
│      }),                                                     │
│  )                                                           │
│                                                              │
├──────────────────────────────────────────────────────────────┤
│                   OpenTelemetry Plugin Layer                  │
│                                                              │
│  ┌─────────────────┐  ┌──────────────────┐                  │
│  │ clientMetrics    │  │ clientTracing     │                  │
│  │ Handler          │  │ Handler           │                  │
│  │                  │  │                   │                  │
│  │ - StatsHandler   │  │ - StatsHandler    │                  │
│  │ - Interceptors   │  │ - Interceptors    │                  │
│  └────────┬─────────┘  └────────┬──────────┘                │
│           │                      │                           │
│           ▼                      ▼                           │
│  ┌─────────────────────────────────────────┐                │
│  │        OTel SDK (MeterProvider,          │                │
│  │        TracerProvider)                    │                │
│  └─────────────────────────────────────────┘                │
└──────────────────────────────────────────────────────────────┘
```

### 2.2 Options 구조

```go
// stats/opentelemetry/opentelemetry.go
type Options struct {
    MetricsOptions MetricsOptions
    TraceOptions   experimental.TraceOptions
}

type MetricsOptions struct {
    MeterProvider         otelmetric.MeterProvider
    Metrics               *stats.MetricSet
    MethodAttributeFilter func(string) bool
    OptionalLabels        []string
    pluginOption          otelinternal.PluginOption  // CSM용
}
```

### 2.3 DialOption/ServerOption 생성

```go
func DialOption(o Options) grpc.DialOption {
    var metricsOpts, tracingOpts []grpc.DialOption

    if o.isMetricsEnabled() {
        metricsHandler := &clientMetricsHandler{options: o}
        metricsHandler.initializeMetrics()
        metricsOpts = append(metricsOpts,
            grpc.WithChainUnaryInterceptor(metricsHandler.unaryInterceptor),
            grpc.WithChainStreamInterceptor(metricsHandler.streamInterceptor),
            grpc.WithStatsHandler(metricsHandler))
    }
    if o.isTracingEnabled() {
        tracingHandler := &clientTracingHandler{options: o}
        tracingHandler.initializeTraces()
        tracingOpts = append(tracingOpts,
            grpc.WithChainUnaryInterceptor(tracingHandler.unaryInterceptor),
            grpc.WithChainStreamInterceptor(tracingHandler.streamInterceptor),
            grpc.WithStatsHandler(tracingHandler))
    }
    return joinDialOptions(append(metricsOpts, tracingOpts...)...)
}
```

**왜 StatsHandler와 Interceptor를 모두 사용하는가?** StatsHandler는 RPC 생명주기 이벤트(시작, 수신, 전송 등)를 받아 메트릭을 기록하고, Interceptor는 RPC 호출을 감싸서 트레이싱 컨텍스트 주입/추출과 시간 측정을 수행한다. 두 메커니즘이 서로 보완적이다.

---

## 3. 클라이언트 메트릭 계측

### 3.1 5가지 기본 클라이언트 메트릭

| 메트릭 이름 | 타입 | 설명 |
|------------|------|------|
| `grpc.client.attempt.started` | Int64Counter | 시도 시작 수 |
| `grpc.client.attempt.duration` | Float64Histogram | 시도별 지속 시간 |
| `grpc.client.attempt.sent_total_compressed_message_size` | Int64Histogram | 압축 후 전송 크기 |
| `grpc.client.attempt.rcvd_total_compressed_message_size` | Int64Histogram | 압축 후 수신 크기 |
| `grpc.client.call.duration` | Float64Histogram | 전체 호출 지속 시간 |

### 3.2 메트릭 수집 구조

```go
type clientMetrics struct {
    attemptStarted                        otelmetric.Int64Counter
    attemptDuration                       otelmetric.Float64Histogram
    attemptSentTotalCompressedMessageSize otelmetric.Int64Histogram
    attemptRcvdTotalCompressedMessageSize otelmetric.Int64Histogram
    callDuration                          otelmetric.Float64Histogram
}
```

### 3.3 attemptInfo 컨텍스트 전파

```go
type attemptInfo struct {
    sentCompressedBytes   int64   // atomic
    recvCompressedBytes   int64   // atomic
    startTime             time.Time
    method                string
    pluginOptionLabels    map[string]string
    xdsLabels             map[string]string
    traceSpan             trace.Span
    countSentMsg          uint32
    countRecvMsg          uint32
    previousRPCAttempts   uint32
}
```

**왜 call vs attempt를 구분하는가?** gRPC 재시도 정책에서 하나의 논리적 "call"이 여러 "attempt"로 구성될 수 있다. attempt 메트릭은 개별 시도를, call 메트릭은 최종 결과를 측정한다.

---

## 4. 서버 메트릭 계측

### 4.1 4가지 기본 서버 메트릭

| 메트릭 이름 | 타입 | 설명 |
|------------|------|------|
| `grpc.server.call.started` | Int64Counter | 수신 RPC 수 |
| `grpc.server.call.duration` | Float64Histogram | RPC 처리 시간 |
| `grpc.server.call.sent_total_compressed_message_size` | Int64Histogram | 전송 크기 |
| `grpc.server.call.rcvd_total_compressed_message_size` | Int64Histogram | 수신 크기 |

### 4.2 MethodAttributeFilter

```go
type MetricsOptions struct {
    MethodAttributeFilter func(string) bool
    // ...
}
```

서버 측에서 메서드 이름을 메트릭 속성(attribute)으로 기록할지 결정하는 필터이다. 허용하지 않으면 "other"로 버킷팅된다. 이는 카디널리티 폭발을 방지하기 위한 안전장치이다.

**왜 필요한가?** 메트릭 시스템에서 레이블 카디널리티가 높으면 메모리 사용량이 급증하고 모니터링 시스템의 성능이 저하된다. gRPC 서버가 수천 개의 메서드를 노출하는 경우, 각 메서드가 별도 시계열을 생성하면 문제가 된다.

---

## 5. 분산 트레이싱 통합

### 5.1 트레이싱 아키텍처

```
Client                                     Server
┌────────────────────┐                    ┌────────────────────┐
│ clientTracingHandler│                    │ serverTracingHandler│
│                     │                    │                     │
│ unaryInterceptor:   │                    │ StatsHandler:       │
│  1. Extract context │                    │  1. Extract span    │
│  2. Start span      │  ──── RPC ─────→  │     context from    │
│  3. Inject headers  │                    │     metadata        │
│  4. Call handler     │                    │  2. Start server    │
│  5. Record status   │  ←── Response ──  │     span            │
│  6. End span        │                    │  3. Record events   │
└────────────────────┘                    │  4. End span        │
                                          └────────────────────┘
```

### 5.2 grpc-trace-bin 전파

gRPC는 표준 OpenTelemetry 전파와 별도로 `grpc-trace-bin` 바이너리 헤더를 통한 전파도 지원한다:

```go
// stats/opentelemetry/grpc_trace_bin_propagator.go
// GRPCTraceBinPropagator는 grpc-trace-bin 바이너리 헤더로 SpanContext를 전파
```

이 전파 방식은 텍스트 기반 전파보다 효율적이며, gRPC 내부 인프라와의 호환성을 위해 존재한다.

### 5.3 Span 이벤트

트레이싱에서 기록되는 이벤트:
- `Inbound compressed message sent` (메시지 수신)
- `Outbound compressed message sent` (메시지 전송)
- 이름 해석 지연 이벤트 (클라이언트만)

---

## 6. 메트릭 레지스트리 시스템

### 6.1 registryMetrics 구조

```go
type registryMetrics struct {
    intCounts           map[*estats.MetricDescriptor]otelmetric.Int64Counter
    floatCounts         map[*estats.MetricDescriptor]otelmetric.Float64Counter
    intHistos           map[*estats.MetricDescriptor]otelmetric.Int64Histogram
    floatHistos         map[*estats.MetricDescriptor]otelmetric.Float64Histogram
    intGauges           map[*estats.MetricDescriptor]otelmetric.Int64Gauge
    intUpDownCounts     map[*estats.MetricDescriptor]otelmetric.Int64UpDownCounter
    intObservableGauges map[*estats.MetricDescriptor]otelmetric.Int64ObservableGauge

    meter          otelmetric.Meter
    optionalLabels []string
}
```

### 6.2 메트릭 생성 패턴

각 메트릭 타입별 factory 함수:

```go
func createInt64Counter(setOfMetrics map[string]bool, metricName string,
    meter otelmetric.Meter, options ...otelmetric.Int64CounterOption) otelmetric.Int64Counter {
    if _, ok := setOfMetrics[metricName]; !ok {
        return noop.Int64Counter{}  // 비활성 메트릭은 noop 반환
    }
    ret, err := meter.Int64Counter(string(metricName), options...)
    if err != nil {
        return noop.Int64Counter{}
    }
    return ret
}
```

**왜 noop을 반환하는가?** 사용자가 활성화하지 않은 메트릭에 대해 nil 체크를 매번 하는 대신, noop 구현을 반환하여 호출 코드에서는 항상 안전하게 Record/Add를 호출할 수 있다. Null Object 패턴의 적용이다.

### 6.3 레이블(Label) 처리

```go
func optionFromLabels(labelKeys []string, optionalLabelKeys []string,
    optionalLabels []string, labelVals ...string) otelmetric.MeasurementOption {
    var attributes []otelattribute.KeyValue
    for i, label := range labelKeys {
        attributes = append(attributes, otelattribute.String(label, labelVals[i]))
    }
    for i, label := range optionalLabelKeys {
        for _, optLabel := range optionalLabels {
            if label == optLabel {
                attributes = append(attributes,
                    otelattribute.String(label, labelVals[i+len(labelKeys)]))
            }
        }
    }
    return otelmetric.WithAttributeSet(otelattribute.NewSet(attributes...))
}
```

### 6.4 기본 히스토그램 버킷 경계

```go
var (
    DefaultLatencyBounds = []float64{
        0, 0.00001, 0.00005, 0.0001, 0.0003, 0.0006, 0.0008,
        0.001, 0.002, 0.003, 0.004, 0.005, 0.006, 0.008,
        0.01, 0.013, 0.016, 0.02, 0.025, 0.03, 0.04, 0.05,
        0.065, 0.08, 0.1, 0.13, 0.16, 0.2, 0.25, 0.3,
        0.4, 0.5, 0.65, 0.8, 1, 2, 5, 10, 20, 50, 100,
    }
    DefaultSizeBounds = []float64{
        0, 1024, 2048, 4096, 16384, 65536, 262144, 1048576,
        4194304, 16777216, 67108864, 268435456, 1073741824, 4294967296,
    }
)
```

---

## 7. CSM(Cloud Service Mesh) 통합

### 7.1 CSM 플러그인 옵션

`stats/opentelemetry/csm/` 패키지는 Google Cloud Service Mesh 환경에서 추가 레이블을 메트릭에 첨부한다.

```go
// csm/pluginoption.go
// PluginOption은 CSM 메시 메타데이터(mesh ID, namespace 등)를
// 메트릭 레이블로 첨부하는 플러그인
```

### 7.2 CSM 레이블 흐름

```
CSM Environment Variables
    │
    ▼
PluginOption.getLabels()
    │
    ▼
attemptInfo.pluginOptionLabels
    │
    ▼
optionFromLabels() → OTel Attributes
```

---

## 8. Admin API 아키텍처

### 8.1 전체 구조

```
┌─────────────────────────────────────────┐
│           admin.Register(s)              │
│                                          │
│  ┌──────────────────────────────────┐   │
│  │     internaladmin.Register(s)     │   │
│  │                                   │   │
│  │  for _, service := range services │   │
│  │     cleanup, err := service(s)    │   │
│  │  end                              │   │
│  └──────────────────────────────────┘   │
│                                          │
│  기본 등록 서비스:                         │
│  ┌─────────────┐  ┌─────────────────┐   │
│  │  Channelz   │  │  CSDS           │   │
│  │  Service     │  │  (xDS 전용)     │   │
│  └─────────────┘  └─────────────────┘   │
└─────────────────────────────────────────┘
```

### 8.2 서비스 등록 메커니즘

```go
// admin/admin.go
func init() {
    internaladmin.AddService(func(registrar grpc.ServiceRegistrar) (func(), error) {
        channelzservice.RegisterChannelzServiceToServer(registrar)
        return nil, nil
    })
}

func Register(s grpc.ServiceRegistrar) (cleanup func(), _ error) {
    return internaladmin.Register(s)
}
```

### 8.3 AddService 패턴

```go
// internal/admin/admin.go (추정 구조)
var services []func(grpc.ServiceRegistrar) (func(), error)

func AddService(f func(grpc.ServiceRegistrar) (func(), error)) {
    services = append(services, f)
}

func Register(s grpc.ServiceRegistrar) (func(), error) {
    var cleanups []func()
    for _, svc := range services {
        cleanup, err := svc(s)
        if err != nil {
            return nil, err
        }
        if cleanup != nil {
            cleanups = append(cleanups, cleanup)
        }
    }
    return func() {
        for _, c := range cleanups {
            c()
        }
    }, nil
}
```

**왜 이런 플러그인 패턴인가?** CSDS는 xDS 패키지에 의존하므로 admin 패키지에서 직접 import하면 순환 의존이 발생한다. 대신 `AddService`로 등록하면 xDS 패키지가 자신의 init()에서 CSDS 서비스를 admin에 추가할 수 있다.

---

## 9. Channelz 서비스 등록

### 9.1 Channelz와 Admin의 관계

```
admin.Register(s)
    │
    ├─ channelzservice.RegisterChannelzServiceToServer(s)
    │   └─ Channelz gRPC 서비스를 s에 등록
    │      - GetTopChannels
    │      - GetServers
    │      - GetChannel
    │      - GetSubchannel
    │      - GetSocket
    │
    └─ (CSDS가 xds 패키지에서 AddService로 추가된 경우)
        └─ csds.RegisterClientStatusDiscoveryService(s)
```

### 9.2 Channelz가 제공하는 정보

| RPC 메서드 | 반환 정보 |
|-----------|----------|
| `GetTopChannels` | 최상위 채널 목록 (ClientConn들) |
| `GetServers` | 서버 목록 |
| `GetChannel` | 특정 채널 상세 (서브채널, 상태, 메트릭) |
| `GetSubchannel` | 서브채널 상세 (소켓, 주소) |
| `GetSocket` | 소켓 상세 (로컬/원격 주소, 스트림 수, 메시지 수) |

---

## 10. CSDS 통합

### 10.1 CSDS(Client Status Discovery Service)란?

xDS 프로토콜을 사용하는 gRPC 클라이언트의 현재 구성 상태를 질의하는 서비스이다.

```
┌───────────────────────────────────┐
│          CSDS Service              │
│                                    │
│  GetClientConfig()                 │
│    ├─ Listener 설정 상태            │
│    ├─ RouteConfiguration 상태       │
│    ├─ Cluster 상태                  │
│    └─ Endpoint 상태                 │
│                                    │
│  각 설정의 상태:                    │
│    - REQUESTED (요청됨)             │
│    - DOES_NOT_EXIST (존재하지 않음)  │
│    - ACKED (승인됨)                 │
│    - NACKED (거부됨)                │
└───────────────────────────────────┘
```

### 10.2 CSDS가 Admin에 등록되는 방식

```go
// xds 패키지의 init()에서 (추정):
func init() {
    internaladmin.AddService(func(registrar grpc.ServiceRegistrar) (func(), error) {
        // xds.GRPCServer나 grpc.Server인 경우에만 CSDS 등록
        csds.RegisterClientStatusDiscoveryService(registrar)
        return nil, nil
    })
}
```

---

## 11. 실제 활용 패턴

### 11.1 OTel 메트릭 설정

```go
import (
    "go.opentelemetry.io/otel/sdk/metric"
    otelgrpc "google.golang.org/grpc/stats/opentelemetry"
)

// MeterProvider 설정
reader := metric.NewManualReader()
mp := metric.NewMeterProvider(metric.WithReader(reader))

// gRPC 클라이언트에 OTel 연동
conn, _ := grpc.NewClient("target",
    otelgrpc.DialOption(otelgrpc.Options{
        MetricsOptions: otelgrpc.MetricsOptions{
            MeterProvider: mp,
            Metrics:       otelgrpc.DefaultMetrics(),
        },
    }),
)

// gRPC 서버에 OTel 연동
srv := grpc.NewServer(
    otelgrpc.ServerOption(otelgrpc.Options{
        MetricsOptions: otelgrpc.MetricsOptions{
            MeterProvider: mp,
        },
    }),
)
```

### 11.2 Admin 서비스 설정

```go
import "google.golang.org/grpc/admin"

// 관리 전용 서버 (별도 포트)
adminServer := grpc.NewServer()
cleanup, _ := admin.Register(adminServer)
defer cleanup()

lis, _ := net.Listen("tcp", ":9999")
go adminServer.Serve(lis)

// grpcdebug 도구로 접속
// grpcdebug localhost:9999 channelz channels
// grpcdebug localhost:9999 xds status
```

### 11.3 비동기 메트릭 리포터

```go
func (rm *registryMetrics) RegisterAsyncReporter(
    reporter estats.AsyncMetricReporter,
    metrics ...estats.AsyncMetric,
) func() {
    // Observable 계측기 수집
    observables := make([]otelmetric.Observable, 0)
    for _, m := range metrics {
        d := m.Descriptor()
        if inst, ok := rm.intObservableGauges[d]; ok {
            observables = append(observables, inst)
        }
    }
    // OTel Meter에 콜백 등록
    reg, _ := rm.meter.RegisterCallback(cbWrapper, observables...)
    return func() { reg.Unregister() }
}
```

---

## 12. 설계 철학과 Why

### 12.1 OTel 플러그인의 설계 원칙

1. **비침투적**: 기존 gRPC 코드를 변경하지 않고 DialOption/ServerOption만 추가
2. **선택적 활성화**: MeterProvider나 TracerProvider가 nil이면 해당 계측 비활성화
3. **noop 패턴**: 비활성 메트릭은 noop 구현으로 런타임 오버헤드 제거
4. **gRPC A66 준수**: [proposal A66](https://grpc.io/docs/guides/opentelemetry-metrics/)에 정의된 메트릭 이름/타입 사용

### 12.2 Admin API의 설계 원칙

1. **단일 진입점**: `admin.Register(s)` 한 번으로 모든 관리 서비스 등록
2. **순환 의존 회피**: `AddService` 플러그인 패턴으로 xDS 의존성 분리
3. **정리(cleanup) 반환**: 서버 종료 시 리소스 정리 함수 제공

### 12.3 OTel과 Admin의 시너지

```
┌─────────────────────────────────────────────────────┐
│              운영 관찰 스택                            │
│                                                      │
│  ┌─────────┐  ┌────────────┐  ┌───────────────────┐ │
│  │  OTel   │  │  Channelz  │  │     CSDS          │ │
│  │ Metrics │  │  (Admin)   │  │    (Admin+xDS)     │ │
│  │ Tracing │  │            │  │                    │ │
│  └────┬────┘  └─────┬──────┘  └────────┬──────────┘ │
│       │              │                  │             │
│  실시간 집계     런타임 상태       xDS 설정 상태      │
│  (Prometheus,   (채널, 소켓,     (LDS, RDS,          │
│   Jaeger 등)    메트릭 카운터)    CDS, EDS)           │
└─────────────────────────────────────────────────────┘
```

---

*본 문서 위치: `grpc-go_EDU/22-otel-admin.md`*
*소스코드 기준: `grpc-go/stats/opentelemetry/`, `grpc-go/admin/`*
