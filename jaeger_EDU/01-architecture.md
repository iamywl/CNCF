# Jaeger 아키텍처

## 1. 개요

Jaeger는 **분산 트레이싱 플랫폼**으로, 마이크로서비스 아키텍처에서 요청의 end-to-end 흐름을 추적한다. Uber Technologies에서 개발하여 CNCF에 기증했으며, 2019년 Graduated 프로젝트로 승격되었다.

Jaeger v2는 **OpenTelemetry Collector** 위에 구축된 완전히 새로운 아키텍처를 채택했다. 기존 v1의 독립 컴포넌트(Agent, Collector, Query, Ingester)가 OTel Collector의 Extension, Receiver, Processor, Exporter 패턴으로 통합되었다.

## 2. 전체 아키텍처

```
┌─────────────────────────────────────────────────────────────────────────┐
│                        Jaeger v2 (OTel Collector 기반)                    │
│                                                                         │
│  ┌─────────────────┐    ┌───────────────┐    ┌─────────────────────┐   │
│  │   Receivers      │    │  Processors   │    │    Exporters        │   │
│  │                  │    │               │    │                     │   │
│  │  ┌────────────┐  │    │ ┌───────────┐ │    │ ┌─────────────────┐ │   │
│  │  │ OTLP       │──┼────┤ │ Batch     │─┼────┤ │ Storage Exporter│ │   │
│  │  │ (gRPC/HTTP)│  │    │ └───────────┘ │    │ │ (jaeger_storage │ │   │
│  │  └────────────┘  │    │ ┌───────────┐ │    │ │  _exporter)     │ │   │
│  │  ┌────────────┐  │    │ │ Adaptive  │ │    │ └────────┬────────┘ │   │
│  │  │ Jaeger     │──┼────┤ │ Sampling  │ │    │          │          │   │
│  │  │ (Thrift)   │  │    │ └───────────┘ │    └──────────┼──────────┘   │
│  │  └────────────┘  │    └───────────────┘               │              │
│  │  ┌────────────┐  │                                    │              │
│  │  │ Zipkin     │──┘                                    │              │
│  │  └────────────┘                                       │              │
│  └─────────────────┘                                     │              │
│                                                          │              │
│  ┌───────────────────────────────────────────────────────┼────────────┐ │
│  │                    Extensions                         │            │ │
│  │                                                       ▼            │ │
│  │  ┌─────────────────┐  ┌────────────────┐  ┌────────────────────┐  │ │
│  │  │ jaeger_query    │  │ jaeger_storage  │  │ remote_sampling    │  │ │
│  │  │                 │  │                 │  │                    │  │ │
│  │  │ - HTTP API      │  │ - Memory        │  │ - File-based       │  │ │
│  │  │ - gRPC API      │  │ - Badger        │  │ - Adaptive         │  │ │
│  │  │ - UI 서빙       │  │ - Cassandra     │  │ - HTTP/gRPC 제공    │  │ │
│  │  │ - 트레이스 조회  │  │ - Elasticsearch │  └────────────────────┘  │ │
│  │  └────────┬────────┘  │ - ClickHouse    │  ┌────────────────────┐  │ │
│  │           │           │ - gRPC (Remote) │  │ jaeger_mcp         │  │ │
│  │           │           └────────┬────────┘  │ (LLM 통합)         │  │ │
│  │           └────────────────────┘           └────────────────────┘  │ │
│  └───────────────────────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────────────────────┘
                              │
                              ▼
                    ┌──────────────────┐
                    │   Storage        │
                    │                  │
                    │  Memory/Badger   │
                    │  Cassandra       │
                    │  Elasticsearch   │
                    │  ClickHouse      │
                    └──────────────────┘
```

## 3. OTel Collector 기반 설계

Jaeger v2의 핵심 설계 결정은 **OpenTelemetry Collector를 런타임으로 채택**한 것이다.

### 3.1 왜 OTel Collector인가?

1. **표준 프로토콜**: OTLP(OpenTelemetry Protocol)을 네이티브로 수신
2. **파이프라인 모델**: Receiver → Processor → Exporter 파이프라인으로 데이터 처리 표준화
3. **확장성**: Extension 메커니즘으로 Query/Storage/Sampling 등 부가 기능 구현
4. **설정 관리**: YAML 기반 통합 설정, 환경 변수 치환 지원
5. **생태계 활용**: OTel Collector의 기존 컴포넌트(kafkaexporter, batchprocessor 등) 재사용

### 3.2 컴포넌트 매핑

Jaeger의 모든 기능은 OTel Collector의 4가지 컴포넌트 타입으로 구현된다:

| OTel 컴포넌트 | Jaeger 기능 | 소스 위치 |
|--------------|------------|----------|
| **Extension** | jaeger_storage (스토리지 관리) | `cmd/jaeger/internal/extension/jaegerstorage/` |
| **Extension** | jaeger_query (쿼리 서비스) | `cmd/jaeger/internal/extension/jaegerquery/` |
| **Extension** | remote_sampling (샘플링) | `cmd/jaeger/internal/extension/remotesampling/` |
| **Extension** | jaeger_mcp (MCP 서버) | `cmd/jaeger/internal/extension/jaegermcp/` |
| **Exporter** | jaeger_storage_exporter | `cmd/jaeger/internal/exporters/storageexporter/` |
| **Processor** | adaptive_sampling | `cmd/jaeger/internal/processors/adaptivesampling/` |
| **Receiver** | otlp, jaeger, zipkin | OTel Collector contrib |

### 3.3 컴포넌트 등록

`cmd/jaeger/internal/components.go`에서 모든 팩토리를 등록한다:

```go
// components.go:68-154
func (b builders) build() (otelcol.Factories, error) {
    factories.Extensions = [
        jaegerquery.NewFactory(),
        jaegerstorage.NewFactory(),
        remotesampling.NewFactory(),
        jaegermcp.NewFactory(),
        // + healthcheckv2, pprof, zpages, basicauth 등
    ]
    factories.Receivers = [otlpreceiver, jaegerreceiver, zipkinreceiver, kafkareceiver]
    factories.Exporters = [storageexporter, kafkaexporter, prometheusexporter]
    factories.Processors = [batchprocessor, adaptivesampling, tailsamplingprocessor]
    factories.Connectors = [forwardconnector, spanmetricsconnector]
}
```

## 4. 진입점 및 초기화 흐름

### 4.1 main() 함수

```
cmd/jaeger/main.go
    │
    ├─ viper.New()                    // 설정 관리
    ├─ internal.Command()             // OTel Collector 커맨드 생성
    │   ├─ otelcol.CollectorSettings  // 빌드 정보 + 팩토리 등록
    │   └─ otelcol.NewCommand()       // cobra 커맨드 생성
    ├─ version.Command()              // 버전 출력 서브커맨드
    ├─ docs.Command()                 // 문서 생성 서브커맨드
    ├─ mappings.Command()             // ES 매핑 출력 서브커맨드
    └─ command.Execute()              // 실행
```

### 4.2 All-in-One 기본 설정

설정 파일 없이 실행하면 임베디드 `all-in-one.yaml`이 사용된다:

```go
// command.go:57-84
cmd.RunE = func(cmd *cobra.Command, args []string) error {
    return checkConfigAndRun(cmd, args, yamlAllInOne.ReadFile, otelRunE)
}

func checkConfigAndRun(...) error {
    if !configFlag.Changed {
        // 설정 파일 미지정 → 임베디드 all-in-one.yaml 사용
        data, _ := getCfg("all-in-one.yaml")
        configFlag.Value.Set("yaml:" + string(data))
    }
    return runE(cmd, args)
}
```

### 4.3 All-in-One 파이프라인 구성

```yaml
# all-in-one.yaml
service:
  extensions: [jaeger_storage, jaeger_query, jaeger_mcp, remote_sampling,
               healthcheckv2, expvar, zpages]
  pipelines:
    traces:
      receivers: [otlp, jaeger, zipkin]    # 3가지 프로토콜 수신
      processors: [batch]                   # 배치 처리
      exporters: [jaeger_storage_exporter]  # 스토리지에 저장

extensions:
  jaeger_storage:
    backends:
      some_storage:
        memory:
          max_traces: 100000    # 인메모리, 최대 10만 트레이스

  jaeger_query:
    storage:
      traces: some_storage     # 위에서 정의한 스토리지 참조

receivers:
  otlp:
    protocols:
      grpc: { endpoint: "localhost:4317" }
      http: { endpoint: "localhost:4318" }
```

## 5. 핵심 확장(Extension) 관계

```
┌──────────────────┐
│  jaeger_storage   │ ←── 스토리지 백엔드 팩토리 관리
│  (확장)           │     TraceStorageFactory(name) → Factory
└────────┬─────────┘
         │ 의존
    ┌────┴────┐
    │         │
    ▼         ▼
┌────────┐ ┌──────────────┐
│ query  │ │ storage      │
│ (확장) │ │ _exporter    │
│        │ │ (내보내기)    │
│ HTTP   │ │              │
│ gRPC   │ │ WriteTraces  │
│ UI     │ │ (ptrace)     │
└────┬───┘ └──────────────┘
     │
     ▼
┌──────────────┐
│  jaeger_mcp   │ ←── Query 서비스에 의존
│  (MCP 서버)   │     LLM 도구 제공
└──────────────┘
```

### 5.1 의존성 순서

1. **jaeger_storage** 가장 먼저 시작 (스토리지 팩토리 초기화)
2. **jaeger_storage_exporter** → jaeger_storage에서 Writer 획득
3. **jaeger_query** → jaeger_storage에서 Reader 획득, HTTP/gRPC 서버 시작
4. **jaeger_mcp** → jaeger_query에서 QueryService 획득
5. **remote_sampling** → jaeger_storage에서 SamplingStore 획득 (적응형 모드)

## 6. 포트 맵

```
┌────────────────────────────────────────────────────────┐
│ 포트          │ 프로토콜   │ 용도                       │
├───────────────┼───────────┼───────────────────────────│
│ 4317          │ gRPC      │ OTLP 트레이스 수신          │
│ 4318          │ HTTP      │ OTLP 트레이스 수신          │
│ 14250         │ gRPC      │ Jaeger 레거시 수신          │
│ 14268         │ HTTP      │ Jaeger Thrift 수신          │
│ 6831/6832     │ UDP       │ Jaeger Thrift Compact/Binary│
│ 9411          │ HTTP      │ Zipkin 호환 수신            │
│ 16685         │ gRPC      │ Query API (gRPC)           │
│ 16686         │ HTTP      │ Query API + UI             │
│ 16687         │ HTTP      │ MCP 서버 (LLM 통합)        │
│ 5778          │ HTTP      │ 샘플링 전략 제공            │
│ 5779          │ gRPC      │ 샘플링 전략 제공            │
│ 8888          │ HTTP      │ Prometheus 메트릭           │
│ 13133         │ HTTP      │ 헬스체크                    │
│ 27777         │ HTTP      │ expvar 모니터링             │
│ 27778         │ HTTP      │ zpages 디버깅              │
└────────────────────────────────────────────────────────┘
```
> 소스: `ports/ports.go:10-28`, `all-in-one.yaml`

## 7. 배포 모델

### 7.1 All-in-One (단일 바이너리)

모든 컴포넌트가 하나의 프로세스에서 실행된다. 개발/테스트용.

```bash
docker run --rm jaegertracing/jaeger:latest
# 또는 --config 없이 실행하면 인메모리 all-in-one 기본 설정 사용
```

### 7.2 프로덕션 분리 배포

```
┌────────────────┐     ┌──────────────────┐     ┌────────────────┐
│  Collector      │     │  Query Service   │     │  Remote Storage│
│                 │     │                  │     │                │
│ receivers:      │     │ extensions:      │     │ gRPC 서버로    │
│   otlp          │     │   jaeger_query   │     │ 스토리지 공유   │
│ exporters:      │     │   jaeger_storage │     │                │
│   storage_exp   │     │                  │     │                │
│   kafka_exp     │     │ UI + HTTP API    │     │                │
└────────┬────────┘     └────────┬─────────┘     └────────────────┘
         │                       │
         └───────────┬───────────┘
                     ▼
              ┌──────────────┐
              │  Cassandra   │
              │  / ES / CH   │
              └──────────────┘
```

### 7.3 Kafka 파이프라인

고처리량 환경에서 Kafka를 버퍼로 사용:

```
SDK → Collector (kafkaexporter) → Kafka → Ingester (kafkareceiver) → Storage
```

## 8. v1 → v2 마이그레이션

| 항목 | v1 | v2 |
|------|----|----|
| 런타임 | 독자 구현 | OTel Collector |
| 설정 | CLI 플래그 | YAML 파일 |
| 프로토콜 | Thrift 중심 | OTLP 네이티브 |
| 스토리지 API | v1 (단일 Span I/O) | v2 (배치 ptrace.Traces) |
| Agent | 별도 프로세스 | 불필요 (SDK 직접 전송) |
| 바이너리 | 5개 (agent, collector, query, ingester, all-in-one) | 1개 (jaeger) |

## 9. 소스코드 참조

| 파일 | 설명 |
|------|------|
| `cmd/jaeger/main.go` | 프로그램 진입점 |
| `cmd/jaeger/internal/command.go` | OTel Collector 커맨드 생성, all-in-one 기본 설정 |
| `cmd/jaeger/internal/components.go` | 모든 팩토리 등록 |
| `cmd/jaeger/internal/all-in-one.yaml` | All-in-One 기본 YAML 설정 |
| `ports/ports.go` | 포트 상수 정의 |
