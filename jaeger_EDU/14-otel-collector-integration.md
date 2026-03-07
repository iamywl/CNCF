# OpenTelemetry Collector 통합

## 목차

1. [개요](#1-개요)
2. [왜 OTel Collector를 런타임으로 선택했는가](#2-왜-otel-collector를-런타임으로-선택했는가)
3. [파이프라인 모델](#3-파이프라인-모델)
4. [Jaeger v2 컴포넌트 등록](#4-jaeger-v2-컴포넌트-등록)
5. [Receiver 상세](#5-receiver-상세)
6. [Processor 상세](#6-processor-상세)
7. [Exporter 상세](#7-exporter-상세)
8. [Extension 상세](#8-extension-상세)
9. [Connector 상세](#9-connector-상세)
10. [설정 시스템](#10-설정-시스템)
11. [All-in-one 기본 설정](#11-all-in-one-기본-설정)
12. [진입점과 실행 흐름](#12-진입점과-실행-흐름)
13. [Jaeger 전용 컴포넌트 심화](#13-jaeger-전용-컴포넌트-심화)
14. [정리](#14-정리)

---

## 1. 개요

Jaeger v2는 OpenTelemetry (OTel) Collector를 **런타임 프레임워크**로 채택했다. Jaeger만의 고유한 기능(스토리지 연동, 쿼리 서비스, 적응형 샘플링, MCP 서버 등)을 OTel Collector의 컴포넌트로 구현하여, 표준화된 파이프라인 모델 위에서 동작한다.

```
Jaeger v1                          Jaeger v2
┌──────────────────┐               ┌──────────────────────────────┐
│  자체 HTTP/gRPC  │               │  OTel Collector 런타임       │
│  서버 구현       │               │  ┌──────────────────────┐    │
│                  │     ──→       │  │ 표준 Receiver        │    │
│  자체 파이프라인 │               │  │ 표준 Processor       │    │
│                  │               │  │ 표준 Exporter        │    │
│  자체 스토리지   │               │  │ Jaeger Extension     │    │
│  추상화         │               │  └──────────────────────┘    │
└──────────────────┘               └──────────────────────────────┘
```

핵심 소스 파일:

```
cmd/jaeger/
├── main.go                                    # 진입점
└── internal/
    ├── command.go                              # OTel Collector 설정 및 실행
    ├── components.go                           # 컴포넌트 팩토리 등록
    ├── all-in-one.yaml                         # 기본 설정 (go:embed)
    ├── exporters/
    │   └── storageexporter/                    # jaeger_storage_exporter
    ├── processors/
    │   └── adaptivesampling/                   # adaptive_sampling processor
    └── extension/
        ├── jaegerstorage/                      # jaeger_storage extension
        ├── jaegerquery/                        # jaeger_query extension
        ├── jaegermcp/                          # jaeger_mcp extension
        ├── remotesampling/                     # remote_sampling extension
        ├── expvar/                             # expvar extension
        └── remotestorage/                      # remotestorage extension
```

---

## 2. 왜 OTel Collector를 런타임으로 선택했는가

### 2.1 설계 동기

Jaeger v1은 Agent, Collector, Query, Ingester 등 여러 독립 바이너리로 구성되었다. 각각이 자체 서버, 설정 시스템, 라이프사이클 관리를 갖고 있어 유지보수 부담이 컸다.

OTel Collector를 런타임으로 채택한 이유:

| 이유 | 설명 |
|------|------|
| **표준화** | OTLP가 사실상 표준이 되면서, Jaeger 고유 프로토콜의 필요성 감소 |
| **인프라 재사용** | HTTP/gRPC 서버, TLS, 인증, 메트릭, 헬스체크 등을 직접 구현할 필요 없음 |
| **파이프라인 모델** | Receiver → Processor → Exporter 파이프라인이 텔레메트리 데이터 처리에 이상적 |
| **확장성** | Extension 메커니즘으로 Jaeger 고유 기능(쿼리, MCP 등)을 깔끔하게 추가 |
| **생태계** | OTel Collector 기여(contrib) 프로젝트의 수백 개 컴포넌트 활용 가능 |
| **단일 바이너리** | v1의 4개 바이너리를 하나로 통합 |

### 2.2 아키텍처적 결과

```
┌─────────────────────────────────────────────────────────────────┐
│                    OTel Collector 런타임                         │
│                                                                 │
│  ┌──────────┐    ┌──────────┐    ┌──────────────────────────┐   │
│  │Receivers │───▶│Processors│───▶│      Exporters           │   │
│  │          │    │          │    │                          │   │
│  │ OTLP     │    │ batch    │    │ jaeger_storage_exporter  │   │
│  │ Jaeger   │    │ memlimit │    │ (→ Jaeger Storage)      │   │
│  │ Zipkin   │    │ adaptive │    │                          │   │
│  │ Kafka    │    │ sampling │    │ kafka, prometheus, etc.  │   │
│  └──────────┘    └──────────┘    └──────────────────────────┘   │
│                                                                 │
│  ┌──────────────────────────────────────────────────────────┐   │
│  │                     Extensions                           │   │
│  │                                                          │   │
│  │  jaeger_storage  : 백엔드 스토리지 관리                    │   │
│  │  jaeger_query    : 쿼리 API (gRPC + HTTP UI)             │   │
│  │  jaeger_mcp      : MCP 서버 (LLM 통합)                   │   │
│  │  remote_sampling : 원격 샘플링 전략 제공                   │   │
│  │  healthcheckv2   : 헬스 체크                              │   │
│  │  pprof, zpages   : 디버깅/프로파일링                      │   │
│  └──────────────────────────────────────────────────────────┘   │
│                                                                 │
│  ┌──────────────────────────────────────────────────────────┐   │
│  │                     Connectors                           │   │
│  │  forward       : 파이프라인 간 데이터 전달                  │   │
│  │  spanmetrics   : 스팬에서 RED 메트릭 추출                   │   │
│  └──────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────┘
```

### 2.3 Extension 라이프사이클

OTel Collector의 Extension은 다음 라이프사이클 메서드를 구현한다:

```go
type Extension interface {
    component.Component  // Start(ctx, host), Shutdown(ctx)
}
```

Jaeger Extension들은 추가로 `extensioncapabilities.Dependent`를 구현하여 시작 순서를 제어한다:

```go
type Dependent interface {
    Dependencies() []component.ID
}
```

```
Extension 시작 순서 (의존성 기반):
  1. jaeger_storage     ← 의존성 없음 (가장 먼저 시작)
  2. remote_sampling    ← jaeger_storage에 의존
  3. jaeger_query       ← jaeger_storage에 의존
  4. jaeger_mcp         ← jaeger_query에 의존
  5. healthcheckv2      ← 의존성 없음 (순서 무관)
  6. expvar, zpages     ← 의존성 없음 (순서 무관)
```

---

## 3. 파이프라인 모델

### 3.1 OTel Collector의 파이프라인

OTel Collector는 텔레메트리 데이터를 **파이프라인(pipeline)**으로 처리한다. 파이프라인은 하나 이상의 Receiver, 0개 이상의 Processor, 하나 이상의 Exporter로 구성된다.

```
Pipeline: traces
  ┌────────────┐    ┌────────────┐    ┌─────────────────────┐
  │ Receivers  │───▶│ Processors │───▶│     Exporters       │
  │            │    │            │    │                     │
  │ ┌────────┐ │    │ ┌────────┐ │    │ ┌─────────────────┐ │
  │ │ otlp   │ │    │ │ batch  │ │    │ │jaeger_storage   │ │
  │ │ jaeger │ │    │ └────────┘ │    │ │_exporter        │ │
  │ │ zipkin │ │    │            │    │ └─────────────────┘ │
  │ └────────┘ │    │            │    │                     │
  └────────────┘    └────────────┘    └─────────────────────┘
```

YAML 설정에서:

```yaml
service:
  pipelines:
    traces:
      receivers: [otlp, jaeger, zipkin]
      processors: [batch]
      exporters: [jaeger_storage_exporter]
```

### 3.2 데이터 흐름

```
외부 애플리케이션
    │
    │ OTLP/Jaeger/Zipkin 프로토콜
    ▼
┌─ Receiver ──────────────────────────────────────────┐
│  - 프로토콜별 서버 구동 (gRPC, HTTP, Thrift 등)      │
│  - 수신 데이터를 pdata.Traces로 변환                 │
│  - Processor 체인으로 전달                           │
└─────────────────────────────────────────────────────┘
    │
    │ pdata.Traces (OTLP 내부 표현)
    ▼
┌─ Processor ─────────────────────────────────────────┐
│  - 데이터 변환/필터링/배칭                            │
│  - 체인 방식으로 순서대로 실행                         │
│  - 입력과 동일한 pdata.Traces 출력                    │
└─────────────────────────────────────────────────────┘
    │
    │ pdata.Traces
    ▼
┌─ Exporter ──────────────────────────────────────────┐
│  - 외부 시스템으로 데이터 전송                         │
│  - jaeger_storage_exporter: Jaeger 스토리지에 저장    │
│  - 재시도, 큐잉, 타임아웃 등 내장                     │
└─────────────────────────────────────────────────────┘
```

### 3.3 Connector

Connector는 하나의 파이프라인의 출력을 다른 파이프라인의 입력으로 연결한다. Exporter와 Receiver의 역할을 동시에 수행한다.

```
Pipeline: traces              Pipeline: metrics
  [receivers] → [processors]     [receivers] → [processors] → [exporters]
       → [connectors/spanmetrics] ─────┘
```

`spanmetrics` Connector는 트레이스 데이터에서 RED(Rate, Error, Duration) 메트릭을 추출한다.

---

## 4. Jaeger v2 컴포넌트 등록

### 4.1 components.go

**파일 경로**: `cmd/jaeger/internal/components.go`

Jaeger v2가 사용하는 모든 OTel Collector 컴포넌트는 `components.go`에서 등록된다.

```go
type builders struct {
    extension func(factories ...extension.Factory) (map[component.Type]extension.Factory, error)
    receiver  func(factories ...receiver.Factory) (map[component.Type]receiver.Factory, error)
    exporter  func(factories ...exporter.Factory) (map[component.Type]exporter.Factory, error)
    processor func(factories ...processor.Factory) (map[component.Type]processor.Factory, error)
    connector func(factories ...connector.Factory) (map[component.Type]connector.Factory, error)
}
```

`builders` 구조체는 각 컴포넌트 타입별 팩토리 맵 생성 함수를 보유한다. `otelcol.MakeFactoryMap`을 사용하여 팩토리를 등록한다.

### 4.2 build() 메서드

`build()` 메서드가 모든 팩토리 맵을 생성한다:

```go
func (b builders) build() (otelcol.Factories, error) {
    var err error
    factories := otelcol.Factories{
        Telemetry: otelconftelemetry.NewFactory(),
    }

    factories.Extensions, err = b.extension(
        // 표준
        healthcheckv2extension.NewFactory(),
        pprofextension.NewFactory(),
        zpagesextension.NewFactory(),
        // 인증
        basicauthextension.NewFactory(),
        sigv4authextension.NewFactory(),
        // Jaeger 전용
        jaegermcp.NewFactory(),
        jaegerquery.NewFactory(),
        jaegerstorage.NewFactory(),
        remotesampling.NewFactory(),
        expvar.NewFactory(),
        storagecleaner.NewFactory(),
        remotestorage.NewFactory(),
    )
    // ...

    factories.Receivers, err = b.receiver(
        otlpreceiver.NewFactory(),
        nopreceiver.NewFactory(),
        jaegerreceiver.NewFactory(),
        kafkareceiver.NewFactory(),
        zipkinreceiver.NewFactory(),
    )
    // ...

    factories.Exporters, err = b.exporter(
        debugexporter.NewFactory(),
        otlpexporter.NewFactory(),
        otlphttpexporter.NewFactory(),
        nopexporter.NewFactory(),
        storageexporter.NewFactory(),
        kafkaexporter.NewFactory(),
        prometheusexporter.NewFactory(),
    )
    // ...

    factories.Processors, err = b.processor(
        batchprocessor.NewFactory(),
        memorylimiterprocessor.NewFactory(),
        tailsamplingprocessor.NewFactory(),
        attributesprocessor.NewFactory(),
        filterprocessor.NewFactory(),
        adaptivesampling.NewFactory(),
    )
    // ...

    factories.Connectors, err = b.connector(
        forwardconnector.NewFactory(),
        spanmetricsconnector.NewFactory(),
    )
    // ...

    return factories, nil
}
```

### 4.3 Components() 함수

```go
func Components() (otelcol.Factories, error) {
    return defaultBuilders().build()
}
```

이 함수는 OTel Collector 런타임에 전달되어 사용 가능한 모든 컴포넌트를 알려준다.

### 4.4 컴포넌트 분류 테이블

아래 테이블은 Jaeger v2에 등록된 모든 컴포넌트를 정리한 것이다.

#### Receivers

| 컴포넌트 | 소스 | 프로토콜 | 설명 |
|----------|------|---------|------|
| `otlp` | OTel Core | gRPC (4317), HTTP (4318) | OTLP 표준 수신 |
| `nop` | OTel Core | - | 테스트용 무동작 수신기 |
| `jaeger` | OTel Contrib | gRPC (14250), Thrift HTTP (14268), Thrift Binary (6832), Thrift Compact (6831) | Jaeger 레거시 프로토콜 |
| `kafka` | OTel Contrib | Kafka | Kafka에서 트레이스 소비 |
| `zipkin` | OTel Contrib | HTTP (9411) | Zipkin 포맷 수신 |

#### Processors

| 컴포넌트 | 소스 | 설명 |
|----------|------|------|
| `batch` | OTel Core | 스팬을 배치로 묶어 전송 효율 향상 |
| `memorylimiter` | OTel Core | 메모리 사용량 제한, 과부하 방지 |
| `tailsampling` | OTel Contrib | 전체 트레이스를 보고 샘플링 결정 (tail-based) |
| `attributes` | OTel Contrib | 스팬 속성 추가/수정/삭제 |
| `filter` | OTel Contrib | 조건에 따라 스팬 필터링 |
| `adaptive_sampling` | Jaeger | 적응형 샘플링용 처리량 수집 |

#### Exporters

| 컴포넌트 | 소스 | 설명 |
|----------|------|------|
| `debug` | OTel Core | 콘솔에 디버그 출력 |
| `otlp` | OTel Core | OTLP gRPC로 전송 |
| `otlphttp` | OTel Core | OTLP HTTP로 전송 |
| `nop` | OTel Core | 테스트용 무동작 전송기 |
| `jaeger_storage_exporter` | Jaeger | Jaeger 스토리지 백엔드에 저장 |
| `kafka` | OTel Contrib | Kafka로 전송 |
| `prometheus` | OTel Contrib | Prometheus 메트릭 노출 |

#### Extensions

| 컴포넌트 | 소스 | 설명 |
|----------|------|------|
| `healthcheckv2` | OTel Contrib | HTTP/gRPC 헬스 체크 |
| `pprof` | OTel Contrib | Go pprof 프로파일링 |
| `zpages` | OTel Core | zPages 디버깅 UI |
| `basicauth` | OTel Contrib | HTTP Basic 인증 |
| `sigv4auth` | OTel Contrib | AWS SigV4 인증 |
| `jaeger_mcp` | Jaeger | MCP 서버 (LLM 통합) |
| `jaeger_query` | Jaeger | 쿼리 API + Jaeger UI |
| `jaeger_storage` | Jaeger | 스토리지 백엔드 관리 |
| `remote_sampling` | Jaeger | 원격 샘플링 전략 |
| `expvar` | Jaeger | Go expvar 메트릭 노출 |
| `storagecleaner` | Jaeger | E2E 테스트용 스토리지 정리 |
| `remotestorage` | Jaeger | 원격 스토리지 서비스 |

#### Connectors

| 컴포넌트 | 소스 | 설명 |
|----------|------|------|
| `forward` | OTel Core | 파이프라인 간 데이터 전달 |
| `spanmetrics` | OTel Contrib | 스팬에서 RED 메트릭 추출 |

---

## 5. Receiver 상세

### 5.1 OTLP Receiver

Jaeger v2의 주력 수신기. gRPC와 HTTP 프로토콜을 모두 지원한다.

```yaml
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: "localhost:4317"
      http:
        endpoint: "localhost:4318"
```

OTLP Receiver는 수신한 데이터를 **변환 없이** `pdata.Traces`로 전달한다. OTLP가 OTel Collector의 네이티브 형식이기 때문이다.

### 5.2 Jaeger Receiver

레거시 Jaeger 클라이언트와의 호환을 위해 제공된다.

```yaml
receivers:
  jaeger:
    protocols:
      grpc:
        endpoint: "localhost:14250"
      thrift_http:
        endpoint: "localhost:14268"
      thrift_binary:
        endpoint: "localhost:6832"
      thrift_compact:
        endpoint: "localhost:6831"
```

4개의 프로토콜을 동시에 지원한다:

| 프로토콜 | 포트 | 전송 방식 | 사용 환경 |
|---------|------|---------|----------|
| gRPC | 14250 | gRPC | 프로덕션 권장 |
| Thrift HTTP | 14268 | HTTP POST | 방화벽 친화적 |
| Thrift Binary | 6832 | UDP | 고성능, Agent 사이드카 |
| Thrift Compact | 6831 | UDP | 대역폭 최적화 |

### 5.3 Zipkin Receiver

```yaml
receivers:
  zipkin:
    endpoint: "localhost:9411"
```

Zipkin 형식의 트레이스를 수신하여 OTLP로 변환한다. Zipkin에서 Jaeger로 마이그레이션할 때 유용하다.

### 5.4 Kafka Receiver

```yaml
receivers:
  kafka:
    brokers:
      - kafka:9092
    topic: jaeger-spans
    encoding: otlp_proto
```

Kafka 토픽에서 트레이스를 소비한다. 높은 처리량 환경에서 Collector 앞에 Kafka를 버퍼로 두는 패턴에 사용된다.

---

## 6. Processor 상세

### 6.1 Batch Processor

```yaml
processors:
  batch:
    send_batch_size: 10000
    timeout: 10s
```

개별 스팬을 배치로 묶어 Exporter에 전달한다. 네트워크 오버헤드를 줄이고 스토리지 쓰기 효율을 높인다.

```
개별 스팬:       배치 후:
  span1 ──┐
  span2 ──┼──→  [span1, span2, ..., span10000] ──→ Exporter
  span3 ──┤
  ...     ┘
```

### 6.2 Memory Limiter Processor

```yaml
processors:
  memorylimiter:
    check_interval: 5s
    limit_mib: 4096
    spike_limit_mib: 512
```

Collector의 메모리 사용량을 모니터링하고, 한계에 도달하면 데이터를 드롭하여 OOM을 방지한다.

```
메모리 사용량 모니터링:
  ┌──────────────────────────────────────────────┐
  │ limit_mib = 4096 MB                          │
  │ ────────────────────────────── (하드 리밋)    │
  │                                              │
  │ limit - spike_limit = 3584 MB                │
  │ ─────────────────────────── (소프트 리밋)     │
  │                                              │
  │ ▓▓▓▓▓▓▓▓▓▓▓ 현재 사용량                      │
  │                                              │
  │ 소프트 리밋 초과 → GC 강제 실행               │
  │ 하드 리밋 초과 → 데이터 드롭                  │
  └──────────────────────────────────────────────┘
```

### 6.3 Adaptive Sampling Processor

**파일 경로**: `cmd/jaeger/internal/processors/adaptivesampling/processor.go`

Jaeger 전용 프로세서로, 트레이스 데이터에서 적응형 샘플링에 필요한 처리량 정보를 수집한다.

```go
type traceProcessor struct {
    config     *Config
    aggregator samplingstrategy.Aggregator
    telset     component.TelemetrySettings
}

func (tp *traceProcessor) processTraces(_ context.Context, td ptrace.Traces) (ptrace.Traces, error) {
    batches := v1adapter.V1BatchesFromTraces(td)
    for _, batch := range batches {
        for _, span := range batch.Spans {
            if span.Process == nil {
                span.Process = batch.Process
            }
            tp.aggregator.HandleRootSpan(span)
        }
    }
    return td, nil  // 데이터를 수정하지 않고 그대로 전달
}
```

**왜 프로세서로 구현했는가?**

프로세서는 파이프라인 내에서 모든 트레이스 데이터를 관찰할 수 있다. 적응형 샘플링은 루트 스팬의 처리량을 관찰해야 하므로, Exporter가 아닌 Processor가 적합하다. 데이터 자체는 수정하지 않고(`return td, nil`) 관찰만 한다.

시작 시 `remote_sampling` Extension에서 `AdaptiveSamplingComponents`를 가져온다:

```go
func (tp *traceProcessor) start(_ context.Context, host component.Host) error {
    parts, err := remotesampling.GetAdaptiveSamplingComponents(host)
    if err != nil {
        return fmt.Errorf("cannot load adaptive sampling components: %w", err)
    }

    agg, err := adaptive.NewAggregator(
        *parts.Options,
        tp.telset.Logger,
        otelmetrics.NewFactory(tp.telset.MeterProvider),
        parts.DistLock,
        parts.SamplingStore,
    )
    // ...
    agg.Start()
    tp.aggregator = agg
    return nil
}
```

### 6.4 Tail Sampling Processor

```yaml
processors:
  tailsampling:
    decision_wait: 10s
    policies:
      - name: errors
        type: status_code
        status_code: { status_codes: [ERROR] }
      - name: slow
        type: latency
        latency: { threshold_ms: 5000 }
```

Head-based 샘플링과 달리, 전체 트레이스가 수집된 후에 샘플링 결정을 내린다. 에러 트레이스나 느린 트레이스만 선택적으로 보관할 수 있다.

```
Head-based (Jaeger 적응형):
  루트 스팬 생성 시 → 즉시 결정 → 이후 스팬도 같은 결정

Tail-based:
  모든 스팬 수집 → 대기 (decision_wait) → 전체 트레이스 분석 → 결정
                                           ↑
                                    에러? 느림? 특정 속성?
```

### 6.5 Attributes Processor

```yaml
processors:
  attributes:
    actions:
      - key: environment
        value: production
        action: insert
      - key: internal.id
        action: delete
```

스팬 속성을 추가, 수정, 삭제할 수 있다. 환경 정보 주입이나 민감 데이터 제거에 활용된다.

### 6.6 Filter Processor

```yaml
processors:
  filter:
    traces:
      span:
        - 'attributes["http.route"] == "/health"'
        - 'name == "readiness-check"'
```

조건에 맞는 스팬을 필터링(드롭)한다. 헬스체크와 같은 노이즈를 제거하는 데 유용하다.

---

## 7. Exporter 상세

### 7.1 jaeger_storage_exporter

**파일 경로**: `cmd/jaeger/internal/exporters/storageexporter/`

Jaeger v2의 핵심 Exporter. 파이프라인의 트레이스 데이터를 Jaeger 스토리지 백엔드에 저장한다.

```yaml
exporters:
  jaeger_storage_exporter:
    trace_storage: some_storage
```

#### 팩토리

```go
// factory.go
var componentType = component.MustNewType("jaeger_storage_exporter")

func NewFactory() exporter.Factory {
    return exporter.NewFactory(
        componentType,
        createDefaultConfig,
        exporter.WithTraces(createTracesExporter, component.StabilityLevelDevelopment),
    )
}
```

#### Exporter 구현

```go
// exporter.go
type storageExporter struct {
    config      *Config
    logger      *zap.Logger
    traceWriter tracestore.Writer
    sanitizer   sanitizer.Func
}

func (exp *storageExporter) start(_ context.Context, host component.Host) error {
    // jaeger_storage Extension에서 스토리지 팩토리 획득
    f, err := jaegerstorage.GetTraceStoreFactory(exp.config.TraceStorage, host)
    if err != nil {
        return fmt.Errorf("cannot find storage factory: %w", err)
    }
    // TraceWriter 생성
    if exp.traceWriter, err = f.CreateTraceWriter(); err != nil {
        return fmt.Errorf("cannot create trace writer: %w", err)
    }
    return nil
}

func (exp *storageExporter) pushTraces(ctx context.Context, td ptrace.Traces) error {
    return exp.traceWriter.WriteTraces(ctx, exp.sanitizer(td))
}
```

**설계 포인트**:

1. `start()`에서 `jaeger_storage` Extension의 팩토리를 획득한다. OTel Collector의 `host.GetExtensions()`를 통해 다른 Extension에 접근한다.
2. `pushTraces()`에서 `sanitizer.Sanitize`를 적용한 후 스토리지에 쓴다. sanitizer는 잘못된 스팬 데이터를 정리한다.
3. `MutatesData: false`로 설정하여 입력 데이터를 수정하지 않음을 선언한다.

#### exporterhelper 통합

```go
func createTracesExporter(ctx context.Context, set exporter.Settings,
    config component.Config) (exporter.Traces, error) {
    cfg := config.(*Config)
    ex := newExporter(cfg, set.TelemetrySettings)
    return exporterhelper.NewTraces(ctx, set, cfg,
        ex.pushTraces,
        exporterhelper.WithCapabilities(consumer.Capabilities{MutatesData: false}),
        exporterhelper.WithTimeout(exporterhelper.TimeoutConfig{Timeout: 0}),  // 타임아웃 비활성
        exporterhelper.WithRetry(cfg.RetryConfig),                              // 재시도 설정
        exporterhelper.WithQueue(cfg.QueueConfig),                              // 큐 설정
        exporterhelper.WithStart(ex.start),
        exporterhelper.WithShutdown(ex.close),
    )
}
```

`exporterhelper`는 재시도, 큐잉, 타임아웃 등의 공통 기능을 제공한다. 기본 설정에서 재시도는 비활성화(`Enabled: false`)되어 있다.

### 7.2 기타 Exporter

| Exporter | 용도 |
|----------|------|
| `debug` | 디버깅용 콘솔 출력 |
| `otlp` | 다른 OTel Collector로 gRPC 전송 |
| `otlphttp` | 다른 OTel Collector로 HTTP 전송 |
| `kafka` | Kafka 토픽에 전송 (버퍼링, 분산) |
| `prometheus` | Prometheus 메트릭 엔드포인트 노출 |
| `nop` | 테스트용 무동작 |

---

## 8. Extension 상세

### 8.1 jaeger_storage

Jaeger의 스토리지 백엔드를 관리하는 핵심 Extension. 다른 컴포넌트(Exporter, Query, Sampling)가 이 Extension을 통해 스토리지에 접근한다.

```yaml
extensions:
  jaeger_storage:
    backends:
      some_storage:
        memory:
          max_traces: 100000
      # 또는
      prod_storage:
        elasticsearch:
          server_urls: ["http://es:9200"]
      # 또는
      cassandra_storage:
        cassandra:
          servers: ["cassandra:9042"]
```

**의존성 그래프**:

```
jaeger_storage
  │
  ├── jaeger_storage_exporter  (TraceWriter 획득)
  ├── jaeger_query             (TraceReader 획득)
  ├── remote_sampling          (SamplingStore 획득)
  └── storagecleaner           (테스트용)
```

### 8.2 jaeger_query

쿼리 API 서비스를 제공하는 Extension. gRPC API와 Jaeger UI(정적 파일)를 서빙한다.

```yaml
extensions:
  jaeger_query:
    storage:
      traces: some_storage
    http:
      endpoint: "localhost:16686"
    grpc:
      endpoint: "localhost:16685"
```

### 8.3 jaeger_mcp

LLM 통합을 위한 MCP(Model Context Protocol) 서버 Extension. 자세한 내용은 15장에서 다룬다.

```yaml
extensions:
  jaeger_mcp:
    http:
      endpoint: "localhost:16687"
```

### 8.4 remote_sampling

원격 샘플링 전략을 SDK에 제공하는 Extension. 자세한 내용은 13장에서 다룬다.

```yaml
extensions:
  remote_sampling:
    file:
      path: /etc/jaeger/sampling.json
      default_sampling_probability: 0.01
      reload_interval: 30s
    http:
      endpoint: "localhost:5778"
    grpc:
      endpoint: "localhost:5779"
```

### 8.5 healthcheckv2

HTTP 및 gRPC 헬스 체크 엔드포인트를 제공한다.

```yaml
extensions:
  healthcheckv2:
    use_v2: true
    http:
      endpoint: "localhost:13133"
    grpc:
```

### 8.6 pprof, zpages

디버깅/프로파일링 도구:

```yaml
extensions:
  pprof:
    endpoint: "localhost:1777"
  zpages:
    endpoint: "localhost:27778"
```

- `pprof`: Go의 `net/http/pprof`를 통한 CPU/메모리 프로파일링
- `zpages`: 파이프라인 상태, 트레이스 디버깅 정보 제공

### 8.7 expvar

Go의 `expvar` 패키지를 통한 내부 변수 노출.

```yaml
extensions:
  expvar:
    endpoint: "localhost:27777"
```

---

## 9. Connector 상세

### 9.1 Forward Connector

가장 간단한 Connector. 한 파이프라인의 출력을 다른 파이프라인의 입력으로 전달한다.

```yaml
service:
  pipelines:
    traces/source:
      receivers: [otlp]
      processors: [batch]
      exporters: [forward]
    traces/destination:
      receivers: [forward]
      processors: [filter]
      exporters: [jaeger_storage_exporter]
```

### 9.2 Span Metrics Connector

트레이스 데이터에서 자동으로 RED 메트릭을 추출한다.

```yaml
connectors:
  spanmetrics:
    histogram:
      explicit:
        buckets: [2ms, 4ms, 6ms, 8ms, 10ms, 50ms, 100ms, 200ms, 400ms, 800ms, 1s, 1400ms, 2s, 5s, 10s, 15s]
    dimensions:
      - name: http.method
      - name: http.status_code
    metrics_flush_interval: 15s

service:
  pipelines:
    traces:
      receivers: [otlp]
      exporters: [spanmetrics, jaeger_storage_exporter]
    metrics:
      receivers: [spanmetrics]
      exporters: [prometheus]
```

추출되는 메트릭:

| 메트릭 | 타입 | 설명 |
|--------|------|------|
| `traces_span_metrics_calls_total` | Counter | 스팬 호출 수 (Rate) |
| `traces_span_metrics_duration_bucket` | Histogram | 스팬 지속 시간 분포 (Duration) |
| `traces_span_metrics_calls_total{status_code="ERROR"}` | Counter | 에러 스팬 수 (Error) |

---

## 10. 설정 시스템

### 10.1 Config Provider

OTel Collector는 여러 설정 소스를 지원한다.

**파일 경로**: `cmd/jaeger/internal/command.go`

```go
settings := otelcol.CollectorSettings{
    // ...
    ConfigProviderSettings: otelcol.ConfigProviderSettings{
        ResolverSettings: confmap.ResolverSettings{
            ProviderFactories: []confmap.ProviderFactory{
                envprovider.NewFactory(),     // ${env:VAR_NAME}
                fileprovider.NewFactory(),    // file:/path/to/config.yaml
                httpprovider.NewFactory(),    // http://host/config
                httpsprovider.NewFactory(),   // https://host/config
                yamlprovider.NewFactory(),    // yaml:inline_config
            },
        },
    },
}
```

| Provider | URI 형식 | 설명 |
|----------|---------|------|
| `env` | `${env:VAR_NAME}` | 환경 변수 치환 |
| `file` | `file:/path/config.yaml` | 로컬 파일 |
| `http` | `http://host/config` | HTTP로 설정 다운로드 |
| `https` | `https://host/config` | HTTPS로 설정 다운로드 |
| `yaml` | `yaml:inline_yaml` | 인라인 YAML |

### 10.2 환경 변수 치환

설정 파일에서 `${env:VAR_NAME:-default}` 구문으로 환경 변수를 사용할 수 있다:

```yaml
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: "${env:JAEGER_LISTEN_HOST:-localhost}:4317"
```

- `JAEGER_LISTEN_HOST`가 설정되어 있으면 해당 값 사용
- 없으면 기본값 `localhost` 사용

### 10.3 YAML 설정 구조

```yaml
# 서비스 정의: 어떤 Extension과 파이프라인을 사용할지
service:
  extensions: [jaeger_storage, jaeger_query, ...]
  pipelines:
    traces:
      receivers: [otlp, jaeger, zipkin]
      processors: [batch]
      exporters: [jaeger_storage_exporter]
  telemetry:
    metrics: { ... }
    logs: { ... }

# Extension 설정
extensions:
  jaeger_storage: { ... }
  jaeger_query: { ... }
  # ...

# Receiver 설정
receivers:
  otlp: { ... }
  jaeger: { ... }
  # ...

# Processor 설정
processors:
  batch: { ... }
  # ...

# Exporter 설정
exporters:
  jaeger_storage_exporter: { ... }
  # ...

# Connector 설정 (선택)
connectors:
  spanmetrics: { ... }
  # ...
```

### 10.4 Collector Settings

OTel Collector 런타임에 전달되는 설정:

```go
// command.go
settings := otelcol.CollectorSettings{
    BuildInfo: info,                    // 버전, 명령어 정보
    Factories: Components,              // 사용 가능한 컴포넌트 팩토리
    ConfigProviderSettings: ...,        // 설정 소스
}
```

```
otelcol.CollectorSettings
├── BuildInfo
│   ├── Command: "jaeger"
│   ├── Description: "Jaeger backend v2"
│   └── Version: version.Get().GitVersion
├── Factories: Components()
│   ├── Extensions: map[type]Factory
│   ├── Receivers: map[type]Factory
│   ├── Processors: map[type]Factory
│   ├── Exporters: map[type]Factory
│   └── Connectors: map[type]Factory
└── ConfigProviderSettings
    └── ResolverSettings
        └── ProviderFactories: [env, file, http, https, yaml]
```

---

## 11. All-in-one 기본 설정

### 11.1 임베딩

**파일 경로**: `cmd/jaeger/internal/command.go`

Jaeger v2는 `--config` 플래그 없이 실행하면 내장된 all-in-one 설정을 사용한다.

```go
//go:embed all-in-one.yaml
var yamlAllInOne embed.FS
```

```go
func checkConfigAndRun(
    cmd *cobra.Command, args []string,
    getCfg func(name string) ([]byte, error),
    runE func(cmd *cobra.Command, args []string) error,
) error {
    configFlag := cmd.Flag("config")
    if !configFlag.Changed {
        log.Print("No '--config' flags detected, using default All-in-One configuration " +
            "with memory storage.")
        data, err := getCfg("all-in-one.yaml")
        if err != nil {
            return fmt.Errorf("cannot read embedded all-in-one configuration: %w", err)
        }
        configFlag.Value.Set("yaml:" + string(data))
    }
    return runE(cmd, args)
}
```

**왜 이렇게 구현했는가?**

OTel Collector는 `--config` 플래그를 필수로 요구한다. Jaeger v1은 설정 없이도 즉시 실행 가능한 all-in-one 모드를 제공했다. 이 호환성을 유지하기 위해, `--config` 플래그가 없으면 내장된 YAML을 `yaml:` 프로바이더를 통해 주입한다.

### 11.2 기본 설정 내용

**파일 경로**: `cmd/jaeger/internal/all-in-one.yaml`

```yaml
service:
  extensions: [jaeger_storage, jaeger_query, jaeger_mcp, remote_sampling,
               healthcheckv2, expvar, zpages]
  pipelines:
    traces:
      receivers: [otlp, jaeger, zipkin]
      processors: [batch]
      exporters: [jaeger_storage_exporter]
  telemetry:
    resource:
      service.name: jaeger
    metrics:
      level: detailed
      readers:
        - pull:
            exporter:
              prometheus:
                host: "${env:JAEGER_LISTEN_HOST:-localhost}"
                port: 8888
    logs:
      level: info
```

```yaml
extensions:
  jaeger_query:
    storage:
      traces: some_storage

  jaeger_mcp:
    http:
      endpoint: "${env:JAEGER_LISTEN_HOST:-localhost}:16687"

  jaeger_storage:
    backends:
      some_storage:
        memory:
          max_traces: 100000

  remote_sampling:
    file:
      path:                              # 비어 있음 (기본 전략 사용)
      default_sampling_probability: 1    # 100% 샘플링
      reload_interval: 1s
    http:
      endpoint: "${env:JAEGER_LISTEN_HOST:-localhost}:5778"
    grpc:
      endpoint: "${env:JAEGER_LISTEN_HOST:-localhost}:5779"

  healthcheckv2:
    use_v2: true
    http:
      endpoint: "${env:JAEGER_LISTEN_HOST:-localhost}:13133"

  expvar:
    endpoint: "${env:JAEGER_LISTEN_HOST:-localhost}:27777"

  zpages:
    endpoint: "${env:JAEGER_LISTEN_HOST:-localhost}:27778"
```

```yaml
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: "${env:JAEGER_LISTEN_HOST:-localhost}:4317"
      http:
        endpoint: "${env:JAEGER_LISTEN_HOST:-localhost}:4318"

  jaeger:
    protocols:
      grpc:
        endpoint: "${env:JAEGER_LISTEN_HOST:-localhost}:14250"
      thrift_http:
        endpoint: "${env:JAEGER_LISTEN_HOST:-localhost}:14268"
      thrift_binary:
        endpoint: "${env:JAEGER_LISTEN_HOST:-localhost}:6832"
      thrift_compact:
        endpoint: "${env:JAEGER_LISTEN_HOST:-localhost}:6831"

  zipkin:
    endpoint: "${env:JAEGER_LISTEN_HOST:-localhost}:9411"

processors:
  batch:

exporters:
  jaeger_storage_exporter:
    trace_storage: some_storage
```

### 11.3 기본 파이프라인 흐름

```
                         all-in-one 기본 파이프라인

  ┌──────────────────┐    ┌─────────┐    ┌────────────────────────┐
  │   Receivers      │    │Processor│    │       Exporter         │
  │                  │    │         │    │                        │
  │  otlp (4317/18)  │───▶│  batch  │───▶│ jaeger_storage_exporter│
  │  jaeger (14250+) │    │         │    │   → memory storage    │
  │  zipkin (9411)   │    │         │    │   (max 100,000 traces)│
  └──────────────────┘    └─────────┘    └────────────────────────┘

  Extensions:
  ┌────────────────┐ ┌────────────────┐ ┌─────────────────┐
  │ jaeger_storage │ │ jaeger_query   │ │ jaeger_mcp      │
  │ (memory)       │ │ (16686)        │ │ (16687)         │
  └────────────────┘ └────────────────┘ └─────────────────┘
  ┌────────────────┐ ┌────────────────┐ ┌─────────────────┐
  │remote_sampling │ │ healthcheckv2  │ │ expvar/zpages   │
  │ (5778/5779)    │ │ (13133)        │ │ (27777/27778)   │
  └────────────────┘ └────────────────┘ └─────────────────┘
```

### 11.4 포트 요약

| 포트 | 프로토콜 | 컴포넌트 | 용도 |
|------|---------|---------|------|
| 4317 | gRPC | otlp receiver | OTLP 수신 (gRPC) |
| 4318 | HTTP | otlp receiver | OTLP 수신 (HTTP) |
| 5778 | HTTP | remote_sampling | 샘플링 전략 (HTTP) |
| 5779 | gRPC | remote_sampling | 샘플링 전략 (gRPC) |
| 6831 | UDP | jaeger receiver | Thrift Compact |
| 6832 | UDP | jaeger receiver | Thrift Binary |
| 8888 | HTTP | telemetry | Prometheus 메트릭 |
| 9411 | HTTP | zipkin receiver | Zipkin 수신 |
| 13133 | HTTP | healthcheckv2 | 헬스 체크 |
| 14250 | gRPC | jaeger receiver | Jaeger gRPC |
| 14268 | HTTP | jaeger receiver | Thrift HTTP |
| 16686 | HTTP | jaeger_query | Jaeger UI + API |
| 16687 | HTTP | jaeger_mcp | MCP 서버 |
| 27777 | HTTP | expvar | Go expvar |
| 27778 | HTTP | zpages | zPages |

---

## 12. 진입점과 실행 흐름

### 12.1 main.go

**파일 경로**: `cmd/jaeger/main.go`

```go
func main() {
    v := viper.New()
    command := internal.Command()
    command.AddCommand(version.Command())
    command.AddCommand(docs.Command(v))
    command.AddCommand(mappings.Command())
    config.AddFlags(v, command)

    if err := command.Execute(); err != nil {
        log.Fatal(err)
    }
}
```

### 12.2 Command() 함수

**파일 경로**: `cmd/jaeger/internal/command.go`

```go
func Command() *cobra.Command {
    info := component.BuildInfo{
        Command:     "jaeger",
        Description: description,
        Version:     version.Get().GitVersion,
    }

    settings := otelcol.CollectorSettings{
        BuildInfo: info,
        Factories: Components,
        ConfigProviderSettings: otelcol.ConfigProviderSettings{
            ResolverSettings: confmap.ResolverSettings{
                ProviderFactories: []confmap.ProviderFactory{
                    envprovider.NewFactory(),
                    fileprovider.NewFactory(),
                    httpprovider.NewFactory(),
                    httpsprovider.NewFactory(),
                    yamlprovider.NewFactory(),
                },
            },
        },
    }
    cmd := otelcol.NewCommand(settings)

    // --config 플래그 없으면 all-in-one 설정 주입
    otelRunE := cmd.RunE
    cmd.RunE = func(cmd *cobra.Command, args []string) error {
        return checkConfigAndRun(cmd, args, yamlAllInOne.ReadFile, otelRunE)
    }

    return cmd
}
```

### 12.3 전체 실행 흐름

```
main()
  │
  ├─ 1. internal.Command() 생성
  │     ├─ BuildInfo 설정 (command, version)
  │     ├─ Components() 등록 (모든 팩토리)
  │     ├─ ConfigProvider 설정 (env, file, http, https, yaml)
  │     └─ otelcol.NewCommand() → Cobra 커맨드 생성
  │
  ├─ 2. 서브커맨드 추가 (version, docs, mappings)
  │
  └─ 3. command.Execute()
       │
       ├─ --config 있으면 → 해당 설정 사용
       └─ --config 없으면 → all-in-one.yaml 임베딩 → yaml: 프로바이더로 주입
            │
            ▼
       OTel Collector 런타임 시작
            │
            ├─ 설정 파싱 (YAML → confmap)
            ├─ 컴포넌트 인스턴스화 (팩토리에서 생성)
            ├─ Extension 시작 (의존성 순서)
            ├─ 파이프라인 구축 (Receiver → Processor → Exporter)
            ├─ 파이프라인 시작
            └─ 시그널 대기 (SIGTERM/SIGINT)
                 │
                 └─ Graceful Shutdown
                      ├─ 파이프라인 중지
                      ├─ Extension 중지
                      └─ 종료
```

---

## 13. Jaeger 전용 컴포넌트 심화

### 13.1 컴포넌트 간 의존성 패턴

Jaeger의 OTel Collector 통합에서 가장 중요한 패턴은 **Extension 간 의존성 주입**이다.

```
패턴: host.GetExtensions()를 통한 컴포넌트 간 통신

┌────────────────────┐
│  jaeger_storage     │ ← 스토리지 팩토리 보유
│  (Extension)        │
│                    │
│  GetTraceStoreFactory(name, host)  ← 헬퍼 함수
│  GetSamplingStoreFactory(name, host)
└────────────────────┘
  ▲         ▲         ▲
  │         │         │
  │   ┌─────┘   ┌─────┘
  │   │         │
┌─┴───┴──┐  ┌──┴────────┐  ┌──────────────┐
│storage  │  │remote     │  │jaeger_query  │
│exporter │  │sampling   │  │(Extension)   │
│(Exporter)│ │(Extension)│  │              │
└─────────┘  └───────────┘  └──────┬───────┘
                                   ▲
                                   │
                            ┌──────┴───────┐
                            │ jaeger_mcp   │
                            │ (Extension)  │
                            └──────────────┘
```

예시 - `storageexporter`가 `jaeger_storage` Extension에서 Writer를 획득:

```go
func (exp *storageExporter) start(_ context.Context, host component.Host) error {
    f, err := jaegerstorage.GetTraceStoreFactory(exp.config.TraceStorage, host)
    // host.GetExtensions()를 내부적으로 사용하여 jaeger_storage Extension을 찾음
    // ...
}
```

예시 - `jaeger_mcp`가 `jaeger_query` Extension에서 QueryService를 획득:

```go
func (s *server) Start(ctx context.Context, host component.Host) error {
    queryExt, err := jaegerquery.GetExtension(host)
    s.queryAPI = queryExt.QueryService()
    // ...
}
```

### 13.2 Dependencies() 메서드로 시작 순서 보장

```go
// storageexporter는 Extension이 아니므로 Dependencies()가 없다.
// 대신 start()에서 동적으로 Extension을 찾는다.

// jaeger_mcp Extension:
func (*server) Dependencies() []component.ID {
    return []component.ID{jaegerquery.ID}  // jaeger_query 이후에 시작
}

// remote_sampling Extension:
func (*rsExtension) Dependencies() []component.ID {
    return []component.ID{jaegerstorage.ID}  // jaeger_storage 이후에 시작
}
```

### 13.3 Exporter vs Processor vs Extension 역할 분담

Jaeger v2에서 각 컴포넌트 타입의 역할:

```
┌──────────────────────────────────────────────────────────┐
│ Extension: 파이프라인 외부의 독립적인 서비스              │
│   - jaeger_storage: 스토리지 팩토리 관리                  │
│   - jaeger_query: 쿼리 API + UI 서빙                     │
│   - jaeger_mcp: MCP 프로토콜 서빙                        │
│   - remote_sampling: 샘플링 전략 HTTP/gRPC 서빙          │
├──────────────────────────────────────────────────────────┤
│ Processor: 파이프라인 내에서 데이터 관찰/변환             │
│   - adaptive_sampling: 처리량 관찰 (데이터 수정 안 함)    │
│   - batch: 데이터 배칭                                   │
│   - filter: 데이터 필터링                                │
├──────────────────────────────────────────────────────────┤
│ Exporter: 파이프라인의 최종 목적지                       │
│   - jaeger_storage_exporter: Jaeger 스토리지에 저장       │
│   - kafka: Kafka 토픽에 전송                             │
│   - prometheus: 메트릭 노출                              │
├──────────────────────────────────────────────────────────┤
│ Receiver: 파이프라인의 입력 소스                          │
│   - otlp: OTLP 프로토콜 수신                             │
│   - jaeger: Jaeger 레거시 프로토콜 수신                   │
│   - kafka: Kafka 토픽에서 소비                            │
├──────────────────────────────────────────────────────────┤
│ Connector: 파이프라인 간 다리                            │
│   - spanmetrics: 트레이스 → 메트릭 변환                   │
│   - forward: 파이프라인 체이닝                            │
└──────────────────────────────────────────────────────────┘
```

### 13.4 프로덕션 설정 예시

```yaml
service:
  extensions: [jaeger_storage, jaeger_query, remote_sampling, healthcheckv2]
  pipelines:
    traces:
      receivers: [otlp]
      processors: [memorylimiter, batch, adaptive_sampling]
      exporters: [jaeger_storage_exporter]
    traces/kafka:
      receivers: [kafka]
      processors: [batch]
      exporters: [jaeger_storage_exporter]
    metrics:
      receivers: [spanmetrics]
      exporters: [prometheus]

extensions:
  jaeger_storage:
    backends:
      primary:
        elasticsearch:
          server_urls: ["http://es-1:9200", "http://es-2:9200"]
          index_prefix: jaeger
          num_shards: 5
          num_replicas: 1

  jaeger_query:
    storage:
      traces: primary
    http:
      endpoint: "0.0.0.0:16686"

  remote_sampling:
    adaptive:
      sampling_store: adaptive_store
      target_samples_per_second: 2.0
    http:
      endpoint: "0.0.0.0:5778"

  healthcheckv2:
    use_v2: true
    http:
      endpoint: "0.0.0.0:13133"

receivers:
  otlp:
    protocols:
      grpc:
        endpoint: "0.0.0.0:4317"
      http:
        endpoint: "0.0.0.0:4318"

  kafka:
    brokers: ["kafka-1:9092", "kafka-2:9092"]
    topic: jaeger-spans
    encoding: otlp_proto

connectors:
  spanmetrics:
    histogram:
      explicit:
        buckets: [2ms, 10ms, 50ms, 100ms, 500ms, 1s, 5s, 10s]

processors:
  memorylimiter:
    check_interval: 5s
    limit_mib: 8192
    spike_limit_mib: 1024

  batch:
    send_batch_size: 10000
    timeout: 5s

  adaptive_sampling:

exporters:
  jaeger_storage_exporter:
    trace_storage: primary

  prometheus:
    endpoint: "0.0.0.0:8889"
```

---

## 14. 정리

### 14.1 핵심 설계 원칙

| 원칙 | 구현 방식 |
|------|----------|
| **표준 준수** | OTLP를 기본 프로토콜로, OTel Collector를 런타임으로 채택 |
| **단일 바이너리** | v1의 Agent+Collector+Query+Ingester를 하나로 통합 |
| **컴포넌트 재사용** | OTel Contrib의 수백 개 컴포넌트 활용 가능 |
| **관심사 분리** | Extension(서비스), Processor(관찰), Exporter(저장) 역할 구분 |
| **의존성 주입** | Extension 간 host.GetExtensions()를 통한 느슨한 결합 |
| **설정 유연성** | 환경 변수, 파일, HTTP 등 다양한 설정 소스 지원 |
| **하위 호환** | Jaeger/Zipkin 레거시 프로토콜 계속 지원 |

### 14.2 v1에서 v2로의 매핑

| Jaeger v1 컴포넌트 | Jaeger v2 컴포넌트 |
|-------------------|-------------------|
| Jaeger Agent | 제거 (SDK가 직접 Collector로 전송) |
| Jaeger Collector | OTel Collector 런타임 + jaeger_storage_exporter |
| Jaeger Query | jaeger_query Extension |
| Jaeger Ingester | kafka Receiver + jaeger_storage_exporter |
| 자체 HTTP/gRPC 서버 | OTel Collector 서버 인프라 |
| 자체 설정 시스템 | OTel Collector YAML + confmap |
| 자체 헬스체크 | healthcheckv2 Extension |

### 14.3 관련 소스 파일 전체 목록

```
cmd/jaeger/
├── main.go                                             # 진입점
└── internal/
    ├── command.go                                       # OTel Collector 설정/실행
    ├── components.go                                    # 컴포넌트 팩토리 등록
    ├── all-in-one.yaml                                  # 기본 설정 (go:embed)
    ├── exporters/
    │   └── storageexporter/
    │       ├── config.go                                # Exporter 설정
    │       ├── exporter.go                              # Exporter 구현
    │       └── factory.go                               # Exporter 팩토리
    ├── processors/
    │   └── adaptivesampling/
    │       ├── config.go                                # Processor 설정
    │       ├── factory.go                               # Processor 팩토리
    │       └── processor.go                             # Processor 구현
    └── extension/
        ├── jaegerstorage/                               # 스토리지 관리 Extension
        ├── jaegerquery/                                 # 쿼리 API Extension
        ├── jaegermcp/                                   # MCP 서버 Extension
        ├── remotesampling/                              # 원격 샘플링 Extension
        ├── expvar/                                      # expvar Extension
        ├── remotestorage/                               # 원격 스토리지 Extension
        └── storagecleaner/                              # 테스트용 Extension
```
