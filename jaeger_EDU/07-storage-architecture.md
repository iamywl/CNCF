# 07. Jaeger 스토리지 아키텍처 Deep Dive

## 목차

1. [스토리지 설계 철학](#1-스토리지-설계-철학)
2. [Factory 인터페이스 체계](#2-factory-인터페이스-체계)
3. [Reader/Writer 인터페이스 상세](#3-readerwriter-인터페이스-상세)
4. [스토리지 확장(Extension) 구현](#4-스토리지-확장extension-구현)
5. [스토리지 설정 체계](#5-스토리지-설정-체계)
6. [팩토리 디스패치](#6-팩토리-디스패치)
7. [V1 <-> V2 어댑터](#7-v1--v2-어댑터)
8. [지연 초기화 ADR](#8-지연-초기화-adr)
9. [메트릭 데코레이터](#9-메트릭-데코레이터)
10. [백엔드별 팩토리 구현 비교](#10-백엔드별-팩토리-구현-비교)

---

## 1. 스토리지 설계 철학

### 1.1 왜 다중 스토리지 백엔드를 지원하는가

Jaeger는 분산 트레이싱 시스템으로, 다양한 운영 환경에서 사용된다. 소규모 개발 환경부터 대규모 프로덕션 환경까지, 각 환경이 요구하는 스토리지 특성은 크게 다르다.

| 환경 | 요구사항 | 적합 백엔드 |
|------|---------|------------|
| 로컬 개발/테스트 | 무설정, 빠른 시작 | Memory |
| 소규모 프로덕션 | 로컬 영속성, 낮은 운영 부담 | Badger |
| 중규모 프로덕션 | 검색 성능, 스케일 아웃 | Elasticsearch/OpenSearch |
| 대규모 프로덕션 | 고쓰루풋, 분산 쓰기 | Cassandra, ClickHouse |
| 마이크로서비스 분리 | 스토리지 서비스 독립 배포 | gRPC (Remote Storage) |

이러한 다양성을 지원하기 위해 Jaeger는 **Factory 패턴** 기반의 추상화 계층을 설계했다. 핵심 원칙은 다음과 같다:

1. **코어 로직과 스토리지의 분리** - 쿼리 서비스, 수집기 등 핵심 컴포넌트는 구체적 스토리지를 모른다
2. **인터페이스 기반 계약** - `tracestore.Factory`, `tracestore.Reader`, `tracestore.Writer` 인터페이스만 알면 된다
3. **설정 기반 선택** - YAML 설정 하나로 백엔드를 교체할 수 있다

### 1.2 Factory 패턴 추상화

Jaeger의 스토리지 아키텍처는 **Abstract Factory 패턴**을 채택한다. Factory가 Reader와 Writer를 생성하고, 각 백엔드가 자신만의 Factory 구현을 제공하는 구조이다.

```
                    ┌─────────────────────────────────┐
                    │      jaeger_storage Extension    │
                    │  (OTel Collector Extension)      │
                    └────────┬────────────────────────┘
                             │
                    ┌────────▼────────────────────────┐
                    │   storageconfig.CreateTrace...   │
                    │   (switch on backend type)       │
                    └────────┬────────────────────────┘
                             │
           ┌─────────────────┼──────────────────┐
           │                 │                  │
     ┌─────▼────┐    ┌──────▼─────┐    ┌───────▼──────┐
     │ memory   │    │elasticsearch│    │  clickhouse  │  ...
     │ Factory  │    │  Factory    │    │   Factory    │
     └────┬─────┘    └─────┬──────┘    └──────┬───────┘
          │                │                  │
          ▼                ▼                  ▼
    Reader/Writer    Reader/Writer      Reader/Writer
```

### 1.3 v1 (단일 Span I/O) vs v2 (배치 ptrace.Traces) API 진화

Jaeger의 스토리지 API는 두 세대(v1, v2)에 걸쳐 진화했다. 이 진화는 OpenTelemetry(OTel)와의 통합을 핵심 동기로 한다.

**v1 API (Jaeger-native 모델)**

v1 API는 Jaeger 자체 데이터 모델(`model.Span`, `model.Trace`)을 기반으로 한다.

파일: `internal/storage/v1/api/spanstore/interface.go`

```go
// v1: 개별 span 단위 쓰기
type Writer interface {
    WriteSpan(ctx context.Context, span *model.Span) error
}

// v1: trace 단위 읽기, 결과가 슬라이스로 반환
type Reader interface {
    GetTrace(ctx context.Context, query GetTraceParameters) (*model.Trace, error)
    FindTraces(ctx context.Context, query *TraceQueryParameters) ([]*model.Trace, error)
    FindTraceIDs(ctx context.Context, query *TraceQueryParameters) ([]model.TraceID, error)
    GetServices(ctx context.Context) ([]string, error)
    GetOperations(ctx context.Context, query OperationQueryParameters) ([]Operation, error)
}
```

**v2 API (OTel-native 모델)**

v2 API는 OTel의 `ptrace.Traces` 데이터 모델과 Go 1.23의 `iter.Seq2` 이터레이터를 기반으로 한다.

파일: `internal/storage/v2/api/tracestore/writer.go`

```go
// v2: 배치 단위 쓰기 (OTLP Exporter API 호환)
type Writer interface {
    WriteTraces(ctx context.Context, td ptrace.Traces) error
}
```

파일: `internal/storage/v2/api/tracestore/reader.go`

```go
// v2: 이터레이터 기반 읽기 (스트리밍, 메모리 효율적)
type Reader interface {
    GetTraces(ctx context.Context, traceIDs ...GetTraceParams) iter.Seq2[[]ptrace.Traces, error]
    FindTraces(ctx context.Context, query TraceQueryParams) iter.Seq2[[]ptrace.Traces, error]
    FindTraceIDs(ctx context.Context, query TraceQueryParams) iter.Seq2[[]FoundTraceID, error]
    GetServices(ctx context.Context) ([]string, error)
    GetOperations(ctx context.Context, query OperationQueryParams) ([]Operation, error)
}
```

**v1 vs v2 핵심 차이점 비교**

| 측면 | v1 API | v2 API |
|------|--------|--------|
| 데이터 모델 | `model.Span`, `model.Trace` (Jaeger 고유) | `ptrace.Traces` (OTel 표준) |
| 쓰기 단위 | 개별 Span (`WriteSpan`) | 배치 Traces (`WriteTraces`) |
| 읽기 반환 | 슬라이스 (`[]*model.Trace`) | 이터레이터 (`iter.Seq2`) |
| 스트리밍 | 불가 (전체 결과 메모리 적재) | 가능 (청크 단위 yield) |
| OTel 호환성 | 변환 필요 | 네이티브 호환 |
| Go 버전 요구 | 제한 없음 | Go 1.23+ (iter 패키지) |

v2 API 도입의 핵심 이유는 **OTLP Exporter API와의 호환성**이다. `WriteTraces(ctx, ptrace.Traces)`는 OTel Collector의 Exporter 인터페이스와 동일한 시그니처를 가지므로, Jaeger를 OTel Collector의 파이프라인에 자연스럽게 통합할 수 있다.

---

## 2. Factory 인터페이스 체계

### 2.1 tracestore.Factory

파일: `internal/storage/v2/api/tracestore/factory.go`

```go
package tracestore

type Factory interface {
    CreateTraceReader() (Reader, error)
    CreateTraceWriter() (Writer, error)
}
```

이 인터페이스는 Jaeger 스토리지 체계의 **최상위 진입점**이다. 모든 백엔드 구현은 이 인터페이스를 반드시 구현해야 한다. 설계 특징은 다음과 같다:

- **최소 인터페이스 원칙(ISP)**: Reader와 Writer 생성만 요구한다. 의존성 저장소, 샘플링 저장소 등은 별도 인터페이스로 분리되어 있다
- **에러 반환**: 팩토리 생성 시 에러 발생 가능성을 명시한다 (DB 연결 실패 등)
- **지연 생성**: 팩토리 자체는 연결을 설정하지만, Reader/Writer는 필요할 때 생성된다

### 2.2 depstore.Factory

파일: `internal/storage/v2/api/depstore/factory.go`

```go
package depstore

type Factory interface {
    CreateDependencyReader() (Reader, error)
}
```

서비스 간 의존성 데이터를 읽는 Reader를 생성한다. 대부분의 백엔드 팩토리가 `tracestore.Factory`와 `depstore.Factory`를 동시에 구현한다.

파일: `internal/storage/v2/api/depstore/reader.go`

```go
type Reader interface {
    GetDependencies(ctx context.Context, query QueryParameters) ([]model.DependencyLink, error)
}

type QueryParameters struct {
    StartTime time.Time
    EndTime   time.Time
}
```

### 2.3 SamplingStoreFactory

파일: `internal/storage/v1/factory.go`

```go
type SamplingStoreFactory interface {
    CreateLock() (distributedlock.Lock, error)
    CreateSamplingStore(maxBuckets int) (samplingstore.Store, error)
}
```

적응형 샘플링(Adaptive Sampling)을 위한 팩토리이다. 두 가지 구성 요소를 생성한다:

1. **distributedlock.Lock**: 분산 환경에서 샘플링 계산 리더 선출에 사용
2. **samplingstore.Store**: 샘플링 확률 데이터를 저장/조회

### 2.4 Purger 인터페이스

파일: `internal/storage/v1/factory.go`

```go
type Purger interface {
    Purge(context.Context) error
}
```

통합 테스트에서만 사용되는 인터페이스이다. 스토리지의 모든 데이터를 삭제한다. Memory, Badger, Cassandra, Elasticsearch, ClickHouse 백엔드가 이를 구현한다.

### 2.5 distributedlock.Lock 인터페이스

파일: `internal/distributedlock/interface.go`

```go
type Lock interface {
    Acquire(resource string, ttl time.Duration) (acquired bool, err error)
    Forfeit(resource string) (forfeited bool, err error)
}
```

분산 락은 적응형 샘플링에서 리더 선출(leader election)에 사용된다. 여러 Collector 인스턴스 중 하나만 샘플링 확률을 계산하도록 보장한다.

### 2.6 인터페이스 계층 구조 다이어그램

```
                 ┌────────────────────┐
                 │ tracestore.Factory │  ← 필수 (모든 백엔드)
                 │  CreateTraceReader │
                 │  CreateTraceWriter │
                 └──────┬─────────────┘
                        │
        ┌───────────────┼───────────────────┐
        │               │                   │
  ┌─────▼──────┐  ┌─────▼───────────┐  ┌───▼─────────────┐
  │ depstore   │  │ SamplingStore   │  │    Purger        │
  │  .Factory  │  │   Factory       │  │                  │
  │ (optional) │  │  (optional)     │  │  (test only)     │
  └────────────┘  └─────────────────┘  └──────────────────┘

  백엔드별 구현 현황:
  ┌─────────────────┬────────┬─────────┬──────────┬────────┐
  │ Backend         │depstore│Sampling │  Purger  │  Lock  │
  ├─────────────────┼────────┼─────────┼──────────┼────────┤
  │ Memory          │   O    │    O    │    O     │   O    │
  │ Badger          │   O    │    O    │    O     │   O    │
  │ Cassandra       │   O    │    O    │    O     │   O    │
  │ Elasticsearch   │   O    │    O    │    O     │   X    │
  │ ClickHouse      │   O    │    X    │    O     │   X    │
  │ gRPC (Remote)   │   O    │    X    │    X     │   X    │
  └─────────────────┴────────┴─────────┴──────────┴────────┘
```

---

## 3. Reader/Writer 인터페이스 상세

### 3.1 Writer 인터페이스

파일: `internal/storage/v2/api/tracestore/writer.go`

```go
type Writer interface {
    WriteTraces(ctx context.Context, td ptrace.Traces) error
}
```

**설계 원칙:**

1. **멱등성(Idempotent)**: 동일한 span을 여러 번 써도 안전해야 한다
2. **비원자적(Non-atomic)**: 배치 내 일부 span만 성공할 수 있다. 실패 시 에러 반환
3. **OTLP Exporter 호환**: OTel Collector의 `exporter.Traces` 인터페이스와 동일한 시그니처

```
WriteTraces 호출 흐름:

ptrace.Traces ──────────────────────────────────────┐
│                                                    │
│  ResourceSpans[0]                                  │
│  ├─ Resource: {service.name: "frontend"}           │
│  └─ ScopeSpans[0]                                  │
│     └─ Spans[0]: {name: "GET /api", ...}           │
│                                                    │
│  ResourceSpans[1]                                  │
│  ├─ Resource: {service.name: "backend"}            │
│  └─ ScopeSpans[0]                                  │
│     └─ Spans[0]: {name: "SELECT ...", ...}         │
│                                                    │
└────────────────────┬───────────────────────────────┘
                     │
                     ▼
           WriteTraces(ctx, td)
                     │
                     ▼
            백엔드별 저장 로직
```

### 3.2 Reader 인터페이스

파일: `internal/storage/v2/api/tracestore/reader.go`

Reader 인터페이스는 5가지 메서드를 정의한다:

#### 3.2.1 GetTraces

```go
GetTraces(ctx context.Context, traceIDs ...GetTraceParams) iter.Seq2[[]ptrace.Traces, error]
```

**가변 인자 설계**: 여러 trace ID를 한 번에 요청할 수 있다. `GetTraceParams`는 trace ID와 선택적 시간 범위를 포함한다.

```go
type GetTraceParams struct {
    TraceID pcommon.TraceID    // 필수
    Start   time.Time          // 선택적 최적화 힌트
    End     time.Time          // 선택적 최적화 힌트
}
```

**시간 범위가 선택적인 이유**: Tempo 같은 백엔드는 시간 범위를 알면 훨씬 효율적으로 조회할 수 있다. 하지만 모든 백엔드가 이를 필요로 하지는 않으므로 선택적 필드이다.

#### 3.2.2 FindTraces / FindTraceIDs

```go
FindTraces(ctx context.Context, query TraceQueryParams) iter.Seq2[[]ptrace.Traces, error]
FindTraceIDs(ctx context.Context, query TraceQueryParams) iter.Seq2[[]FoundTraceID, error]
```

`TraceQueryParams`는 서비스명, 오퍼레이션명, 속성 필터, 시간 범위, 지속 시간 범위, 검색 깊이를 포함한다:

```go
type TraceQueryParams struct {
    ServiceName   string
    OperationName string
    Attributes    pcommon.Map      // pcommon.NewMap()으로 초기화 필요
    StartTimeMin  time.Time
    StartTimeMax  time.Time
    DurationMin   time.Duration
    DurationMax   time.Duration
    SearchDepth   int
}
```

`FindTraceIDs`는 `FindTraces`와 동일한 검색을 수행하되, 전체 trace 대신 ID만 반환한다. 배치 작업에서 먼저 ID 목록을 얻고 나중에 전체 trace를 로드하는 패턴에 유용하다.

```go
type FoundTraceID struct {
    TraceID pcommon.TraceID
    Start   time.Time    // 최적화 힌트 (정확한 시간이 아닐 수 있음)
    End     time.Time    // 최적화 힌트
}
```

#### 3.2.3 GetServices / GetOperations

```go
GetServices(ctx context.Context) ([]string, error)
GetOperations(ctx context.Context, query OperationQueryParams) ([]Operation, error)
```

UI에서 서비스 드롭다운과 오퍼레이션 드롭다운을 채우는 데 사용된다. 이 메서드들은 이터레이터가 아닌 슬라이스를 반환한다 (결과 크기가 작으므로).

### 3.3 이터레이터 계약 (Iterator Contract)

v2 API의 핵심 설계 특징은 **이터레이터 기반 반환**이다. `iter.Seq2[[]ptrace.Traces, error]` 타입은 Go 1.23의 표준 이터레이터 패턴을 사용한다.

**이터레이터 규칙:**

```
┌─────────────────────────────────────────────────────────────────┐
│                    이터레이터 계약 (Contract)                     │
├─────────────────────────────────────────────────────────────────┤
│                                                                  │
│  1. 일회성(Single-use):                                          │
│     한 번 소비하면 재사용 불가                                     │
│                                                                  │
│  2. 청킹 규칙:                                                   │
│     - 하나의 ptrace.Traces 청크에 여러 trace의 span 혼재 금지      │
│     - 큰 trace는 여러 연속 ptrace.Traces 청크로 분할 가능           │
│     - 각 ptrace.Traces 객체는 비어있으면 안 됨                      │
│                                                                  │
│  3. 에지 케이스:                                                  │
│     - 찾지 못한 trace ID는 무시 (에러 아님)                        │
│     - 모든 ID를 찾지 못하면 빈 이터레이터 반환                      │
│     - 에러 발생 시 이터레이터가 에러를 반환하고 중단                 │
│                                                                  │
└─────────────────────────────────────────────────────────────────┘
```

**청킹 다이어그램:**

```
  GetTraces(traceA, traceB, traceC)

  이터레이터 반환 순서:
  ┌──────────────────────┐
  │ yield #1             │
  │ []ptrace.Traces{     │
  │   Traces(traceA)     │  ← traceA 전체 (작은 trace)
  │ }                    │
  └──────────┬───────────┘
             ▼
  ┌──────────────────────┐
  │ yield #2             │
  │ []ptrace.Traces{     │
  │   Traces(traceB-1/2) │  ← traceB 청크 1 (큰 trace, 분할)
  │ }                    │
  └──────────┬───────────┘
             ▼
  ┌──────────────────────┐
  │ yield #3             │
  │ []ptrace.Traces{     │
  │   Traces(traceB-2/2) │  ← traceB 청크 2 (나머지)
  │ }                    │
  └──────────┬───────────┘
             ▼
  (traceC not found → 무시)
             ▼
  이터레이터 종료
```

### 3.4 v1과 v2 Reader 시그니처 대비표

| 메서드 | v1 시그니처 | v2 시그니처 |
|--------|-----------|-----------|
| 트레이스 조회 | `GetTrace(ctx, query) (*Trace, error)` | `GetTraces(ctx, ...GetTraceParams) iter.Seq2[[]ptrace.Traces, error]` |
| 트레이스 검색 | `FindTraces(ctx, *QueryParams) ([]*Trace, error)` | `FindTraces(ctx, QueryParams) iter.Seq2[[]ptrace.Traces, error]` |
| 트레이스 ID 검색 | `FindTraceIDs(ctx, *QueryParams) ([]TraceID, error)` | `FindTraceIDs(ctx, QueryParams) iter.Seq2[[]FoundTraceID, error]` |
| 서비스 목록 | `GetServices(ctx) ([]string, error)` | `GetServices(ctx) ([]string, error)` (동일) |
| 오퍼레이션 목록 | `GetOperations(ctx, query) ([]Operation, error)` | `GetOperations(ctx, query) ([]Operation, error)` (동일) |

---

## 4. 스토리지 확장(Extension) 구현

### 4.1 Extension 인터페이스

파일: `cmd/jaeger/internal/extension/jaegerstorage/extension.go`

Jaeger v2는 OTel Collector의 확장(Extension) 메커니즘을 활용하여 스토리지를 관리한다. 이 설계는 OTel Collector 파이프라인과의 자연스러운 통합을 가능하게 한다.

```go
type Extension interface {
    extension.Extension
    TraceStorageFactory(name string) (tracestore.Factory, error)
    MetricStorageFactory(name string) (storage.MetricStoreFactory, error)
}
```

`extension.Extension`은 OTel Collector의 표준 확장 인터페이스로, `Start()`와 `Shutdown()` 라이프사이클 메서드를 포함한다.

### 4.2 storageExt 구조체 - 지연 초기화 (Lazy Initialization)

```go
type storageExt struct {
    config           *Config
    telset           telemetry.Settings
    factories        map[string]tracestore.Factory      // trace 스토리지 팩토리 캐시
    metricsFactories map[string]storage.MetricStoreFactory // 메트릭 스토리지 팩토리 캐시
    factoryMu        sync.Mutex                          // 동시성 보호
}
```

핵심 설계는 **지연 초기화(Lazy Initialization)**이다. 팩토리는 최초 요청 시에만 생성되고, 이후에는 캐시된 인스턴스를 반환한다.

### 4.3 Start() - 설정 검증만 수행

```go
func (s *storageExt) Start(_ context.Context, host component.Host) error {
    s.telset.Host = host
    s.telset.Metrics = otelmetrics.NewFactory(s.telset.MeterProvider).
        Namespace(metrics.NSOptions{Name: "jaeger"})

    // 설정만 검증, 팩토리 생성은 하지 않음
    for name, cfg := range s.config.TraceBackends {
        if err := cfg.Validate(); err != nil {
            return fmt.Errorf("invalid configuration for trace storage '%s': %w", name, err)
        }
    }
    for name, cfg := range s.config.MetricBackends {
        if err := cfg.Validate(); err != nil {
            return fmt.Errorf("invalid configuration for metric storage '%s': %w", name, err)
        }
    }
    return nil
}
```

**왜 Start()에서 팩토리를 생성하지 않는가?** ADR-003(8장에서 상세 설명)에 따라, 사용되지 않는 백엔드가 리소스를 낭비하거나 시작 실패를 유발하는 문제를 해결하기 위해 지연 초기화를 채택했다.

### 4.4 TraceStorageFactory() - 지연 생성 + 캐싱

```go
func (s *storageExt) TraceStorageFactory(name string) (tracestore.Factory, error) {
    s.factoryMu.Lock()
    defer s.factoryMu.Unlock()

    // 1. 캐시 확인
    if f, ok := s.factories[name]; ok {
        return f, nil
    }

    // 2. 설정 존재 확인
    cfg, ok := s.config.TraceBackends[name]
    if !ok {
        return nil, fmt.Errorf(
            "storage '%s' not declared in '%s' extension configuration",
            name, componentType,
        )
    }

    // 3. 최초 요청 시 팩토리 생성
    factory, err := storageconfig.CreateTraceStorageFactory(
        context.Background(), name, cfg, s.telset,
        func(authCfg config.Authentication, backendType, backendName string) (extensionauth.HTTPClient, error) {
            return s.resolveAuthenticator(s.telset.Host, authCfg, backendType, backendName)
        },
    )
    if err != nil {
        return nil, fmt.Errorf("failed to initialize storage '%s': %w", name, err)
    }

    // 4. 캐시에 저장
    s.factories[name] = factory
    return factory, nil
}
```

**동시성 보호**: `sync.Mutex`로 보호되어 여러 고루틴이 동시에 같은 팩토리를 요청해도 하나만 생성된다.

### 4.5 Shutdown() - 생성된 팩토리만 정리

```go
func (s *storageExt) Shutdown(context.Context) error {
    var errs []error
    for _, factory := range s.factories {
        if closer, ok := factory.(io.Closer); ok {
            err := closer.Close()
            if err != nil {
                errs = append(errs, err)
            }
        }
    }
    for _, metricfactory := range s.metricsFactories {
        if closer, ok := metricfactory.(io.Closer); ok {
            if err := closer.Close(); err != nil {
                errs = append(errs, err)
            }
        }
    }
    return errors.Join(errs...)
}
```

`io.Closer` 인터페이스를 구현하는 팩토리만 정리한다. 지연 초기화 덕분에 실제로 사용된 팩토리만 `s.factories`에 존재하므로, 불필요한 정리가 발생하지 않는다.

### 4.6 글로벌 헬퍼 함수

Extension에서 팩토리를 꺼내는 편의 함수들이 제공된다:

```go
// 트레이스 스토리지 팩토리 조회
func GetTraceStoreFactory(name string, host component.Host) (tracestore.Factory, error)

// 샘플링 스토리지 팩토리 조회 (타입 단언 포함)
func GetSamplingStoreFactory(name string, host component.Host) (storage.SamplingStoreFactory, error)

// Purger 조회 (타입 단언 포함)
func GetPurger(name string, host component.Host) (storage.Purger, error)

// 메트릭 스토리지 팩토리 조회
func GetMetricStorageFactory(name string, host component.Host) (storage.MetricStoreFactory, error)
```

이 함수들의 공통 패턴은 다음과 같다:

```
GetSamplingStoreFactory("my_store", host)
    │
    ├─ 1. findExtension(host) → Extension 찾기
    │
    ├─ 2. ext.TraceStorageFactory("my_store") → Factory 획득
    │
    └─ 3. f.(storage.SamplingStoreFactory) → 타입 단언
         │
         ├─ 성공 → SamplingStoreFactory 반환
         └─ 실패 → "storage 'my_store' does not support sampling store" 에러
```

이 패턴을 통해 **선택적 기능(Optional Capability)**을 타입 시스템으로 표현한다. 모든 백엔드가 샘플링이나 퍼지를 지원할 필요는 없으며, 지원하는 백엔드만 해당 인터페이스를 구현한다.

### 4.7 인증 해석기 (Authentication Resolver)

Elasticsearch/OpenSearch 백엔드는 외부 인증 확장과 연동할 수 있다:

```go
func (s *storageExt) resolveAuthenticator(
    host component.Host,
    authCfg config.Authentication,
    backendType, backendName string,
) (extensionauth.HTTPClient, error) {
    if authCfg.AuthenticatorID.String() == "" {
        return nil, nil  // 인증 미설정 → nil 반환 (인증 없음)
    }
    httpAuth, err := s.getAuthenticator(host, authCfg.AuthenticatorID.String())
    if err != nil {
        return nil, fmt.Errorf("failed to get HTTP authenticator for %s backend '%s': %w",
            backendType, backendName, err)
    }
    return httpAuth, nil
}
```

OTel Collector의 확장 메커니즘을 통해 인증 확장(예: Bearer Token, OAuth2)을 스토리지 백엔드에 주입할 수 있다.

---

## 5. 스토리지 설정 체계

### 5.1 Config 구조

파일: `cmd/internal/storageconfig/config.go`

```go
type Config struct {
    TraceBackends  map[string]TraceBackend  `mapstructure:"backends"`
    MetricBackends map[string]MetricBackend `mapstructure:"metric_backends"`
}
```

설정은 **이름 기반 맵(map)** 구조이다. 여러 백엔드를 서로 다른 이름으로 선언하고, 파이프라인 컴포넌트가 이름으로 참조한다.

### 5.2 TraceBackend 구조

```go
type TraceBackend struct {
    Memory        *memory.Configuration     `mapstructure:"memory"`
    Badger        *badger.Config            `mapstructure:"badger"`
    GRPC          *grpc.Config              `mapstructure:"grpc"`
    Cassandra     *cassandra.Options        `mapstructure:"cassandra"`
    Elasticsearch *escfg.Configuration      `mapstructure:"elasticsearch"`
    Opensearch    *escfg.Configuration      `mapstructure:"opensearch"`
    ClickHouse    *clickhouse.Configuration `mapstructure:"clickhouse"`
}
```

**설계 원칙: 정확히 하나의 백엔드만 설정**

모든 필드가 포인터 타입이다. `nil`이면 해당 백엔드가 설정되지 않은 것이고, 정확히 하나의 필드만 비-nil이어야 한다. 이를 `Validate()` 메서드가 강제한다.

### 5.3 Validate() - 단일 백엔드 강제

```go
func (cfg *TraceBackend) Validate() error {
    var backends []string
    if cfg.Memory != nil {
        backends = append(backends, "memory")
    }
    if cfg.Badger != nil {
        backends = append(backends, "badger")
    }
    if cfg.GRPC != nil {
        backends = append(backends, "grpc")
    }
    if cfg.Cassandra != nil {
        backends = append(backends, "cassandra")
    }
    if cfg.Elasticsearch != nil {
        backends = append(backends, "elasticsearch")
    }
    if cfg.Opensearch != nil {
        backends = append(backends, "opensearch")
    }
    if cfg.ClickHouse != nil {
        backends = append(backends, "clickhouse")
    }
    if len(backends) == 0 {
        return errors.New("empty configuration")
    }
    if len(backends) > 1 {
        return fmt.Errorf("multiple backend types found for trace storage: %v", backends)
    }
    return nil
}
```

**왜 하나의 TraceBackend에 하나의 백엔드만 허용하는가?**

1. **명확한 소유권**: 각 이름이 정확히 하나의 스토리지 구현에 매핑된다
2. **단순한 팩토리 디스패치**: switch 문 하나로 팩토리를 생성할 수 있다
3. **다중 백엔드 필요 시**: 별도의 이름으로 선언하면 된다

### 5.4 Unmarshal() - 백엔드별 기본값 적용

```go
func (cfg *TraceBackend) Unmarshal(conf *confmap.Conf) error {
    // 백엔드별 기본값 설정
    if conf.IsSet("memory") {
        cfg.Memory = &memory.Configuration{
            MaxTraces: 1_000_000,   // 기본 100만 trace
        }
    }
    if conf.IsSet("badger") {
        v := badger.DefaultConfig()
        cfg.Badger = v
    }
    if conf.IsSet("grpc") {
        v := grpc.DefaultConfig()
        cfg.GRPC = &v
    }
    if conf.IsSet("cassandra") {
        cfg.Cassandra = &cassandra.Options{
            Configuration:          cascfg.DefaultConfiguration(),
            SpanStoreWriteCacheTTL: 12 * time.Hour,
            Index: cassandra.IndexConfig{
                Tags:        true,
                ProcessTags: true,
                Logs:        true,
            },
            ArchiveEnabled: false,
        }
    }
    if conf.IsSet("elasticsearch") {
        v := es.DefaultConfig()
        cfg.Elasticsearch = &v
    }
    if conf.IsSet("opensearch") {
        v := es.DefaultConfig()
        cfg.Opensearch = &v
    }
    if conf.IsSet("clickhouse") {
        cfg.ClickHouse = &clickhouse.Configuration{}
    }
    return conf.Unmarshal(cfg)
}
```

`confmap.Unmarshaler` 인터페이스를 구현하여 **2단계 역직렬화**를 수행한다:
1. 먼저 설정된 백엔드의 기본값을 채운다
2. 이후 사용자 설정으로 덮어쓴다

이 패턴을 통해 사용자는 최소한의 설정만 작성해도 합리적인 기본값이 적용된다.

### 5.5 YAML 설정 예시

```yaml
extensions:
  jaeger_storage:
    backends:
      # 주 스토리지: Elasticsearch
      primary_es:
        elasticsearch:
          server_urls: http://elasticsearch:9200
          index_prefix: jaeger

      # 아카이브 스토리지: 인메모리 (개발용)
      archive_mem:
        memory:
          max_traces: 10000

    metric_backends:
      prometheus_metrics:
        prometheus:
          endpoint: http://prometheus:9090

service:
  extensions: [jaeger_storage]

  pipelines:
    traces:
      receivers: [otlp]
      exporters: [jaeger_storage_exporter]

  # Query는 'primary_es' 스토리지를 사용
  jaeger_query:
    storage:
      traces: primary_es
      traces_archive: archive_mem
```

---

## 6. 팩토리 디스패치

### 6.1 CreateTraceStorageFactory()

파일: `cmd/internal/storageconfig/factory.go`

```go
func CreateTraceStorageFactory(
    ctx context.Context,
    name string,
    backend TraceBackend,
    telset telemetry.Settings,
    authResolver AuthResolver,
) (tracestore.Factory, error) {
    telset.Logger.Sugar().Infof("Initializing storage '%s'", name)

    // 스코프된 메트릭 팩토리 생성
    telset.Metrics = telset.Metrics.Namespace(metrics.NSOptions{
        Name: "storage",
        Tags: map[string]string{
            "name": name,
            "role": "tracestore",
        },
    })

    var factory tracestore.Factory
    var err error

    switch {
    case backend.Memory != nil:
        factory, err = memory.NewFactory(*backend.Memory, telset)
    case backend.Badger != nil:
        factory, err = badger.NewFactory(*backend.Badger, telset)
    case backend.GRPC != nil:
        factory, err = grpc.NewFactory(ctx, *backend.GRPC, telset)
    case backend.Cassandra != nil:
        factory, err = cassandra.NewFactory(*backend.Cassandra, telset)
    case backend.Elasticsearch != nil:
        var httpAuth extensionauth.HTTPClient
        if authResolver != nil {
            httpAuth, err = authResolver(
                backend.Elasticsearch.Authentication, "elasticsearch", name)
            if err != nil {
                return nil, err
            }
        }
        factory, err = es.NewFactory(ctx, *backend.Elasticsearch, telset, httpAuth)
    case backend.Opensearch != nil:
        var httpAuth extensionauth.HTTPClient
        if authResolver != nil {
            httpAuth, err = authResolver(
                backend.Opensearch.Authentication, "opensearch", name)
            if err != nil {
                return nil, err
            }
        }
        factory, err = es.NewFactory(ctx, *backend.Opensearch, telset, httpAuth)
    case backend.ClickHouse != nil:
        factory, err = clickhouse.NewFactory(ctx, *backend.ClickHouse, telset)
    default:
        err = errors.New("empty configuration")
    }

    if err != nil {
        return nil, fmt.Errorf("failed to initialize storage '%s': %w", name, err)
    }
    return factory, nil
}
```

**설계 포인트:**

1. **switch 기반 디스패치**: `TraceBackend.Validate()`가 정확히 하나의 필드만 non-nil임을 보장하므로, switch 문의 case가 정확히 하나만 매칭된다
2. **메트릭 네임스페이싱**: 각 팩토리에 `storage.{name}.tracestore` 네임스페이스의 메트릭을 주입한다
3. **인증 해석**: Elasticsearch/OpenSearch만 인증이 필요하므로, 해당 case에서만 `authResolver`를 호출한다
4. **AuthResolver 분리**: 인증 해석 로직을 함수 타입으로 주입받아 jaegerstorage와 remote-storage 간 코드 재사용이 가능하다

```go
type AuthResolver func(
    authCfg escfg.Authentication,
    backendType, backendName string,
) (extensionauth.HTTPClient, error)
```

### 6.2 팩토리 디스패치 흐름도

```
YAML Config
    │
    ▼
┌───────────────────────────────┐
│ storageExt.TraceStorageFactory│
│    ("primary_es")             │
└───────┬───────────────────────┘
        │
        ▼
┌───────────────────────────────────────────────────┐
│ storageconfig.CreateTraceStorageFactory()          │
│                                                    │
│   backend.Elasticsearch != nil  → switch 매칭      │
│                                                    │
│   1. authResolver() → httpAuth 획득                │
│   2. es.NewFactory(ctx, cfg, telset, httpAuth)     │
│   3. return factory                                │
└───────┬───────────────────────────────────────────┘
        │
        ▼
┌───────────────────────────────────────────────────┐
│ elasticsearch.Factory                              │
│                                                    │
│   ├─ CreateTraceReader() → ES TraceReader          │
│   │     (+ tracestoremetrics.NewReaderDecorator)   │
│   ├─ CreateTraceWriter() → ES TraceWriter          │
│   └─ CreateDependencyReader() → ES DepReader       │
└───────────────────────────────────────────────────┘
```

### 6.3 백엔드별 NewFactory 초기화 비교

| 백엔드 | NewFactory 시점 동작 | 외부 연결 | 스키마 생성 |
|--------|---------------------|----------|-----------|
| Memory | 인메모리 Store 할당 | 없음 | 없음 |
| Badger | DB 파일 열기, 백그라운드 GC 시작 | 로컬 파일 | 없음 |
| gRPC | gRPC 커넥션 2개 수립 (reader/writer) | 리모트 서버 | 없음 |
| Cassandra | 세션 생성, 클러스터 연결, 설정 검증 | Cassandra 클러스터 | 선택적 |
| Elasticsearch | HTTP 클라이언트 생성, 커넥션 풀 | ES 클러스터 | 없음 |
| OpenSearch | HTTP 클라이언트 생성, 커넥션 풀 | OS 클러스터 | 없음 |
| ClickHouse | 커넥션 열기, Ping, 스키마 생성 | ClickHouse 서버 | 선택적 |

---

## 7. V1 <-> V2 어댑터

### 7.1 어댑터 패턴의 필요성

Jaeger는 v1에서 v2로 점진적으로 마이그레이션 중이다. 일부 백엔드(Cassandra, Badger)는 아직 v1 인터페이스를 기반으로 구현되어 있으며, 이를 v2 인터페이스로 노출하기 위해 **어댑터(Adapter)**가 필요하다.

파일: `internal/storage/v2/v1adapter/` 디렉토리

```
v1adapter/
├── tracereader.go      # v1 SpanReader → v2 tracestore.Reader
├── tracewriter.go      # v1 SpanWriter → v2 tracestore.Writer
├── spanreader.go       # v2 tracestore.Reader → v1 SpanReader (역방향)
├── spanwriter.go       # v2 tracestore.Writer → v1 SpanWriter (역방향)
├── depreader.go        # v1 dependencystore.Reader → v2 depstore.Reader
├── translator.go       # v1 Batch <-> ptrace.Traces 변환
└── otelids.go          # v1 TraceID/SpanID <-> OTel TraceID/SpanID 변환
```

### 7.2 TraceWriter: v1 SpanWriter를 v2 Writer로 감싸기

파일: `internal/storage/v2/v1adapter/tracewriter.go`

```go
type TraceWriter struct {
    spanWriter spanstore.Writer
}

func NewTraceWriter(spanWriter spanstore.Writer) *TraceWriter {
    return &TraceWriter{
        spanWriter: spanWriter,
    }
}

func (t *TraceWriter) WriteTraces(ctx context.Context, td ptrace.Traces) error {
    batches := V1BatchesFromTraces(td)           // ptrace.Traces → []*model.Batch
    var errs []error
    for _, batch := range batches {
        for _, span := range batch.Spans {
            if span.Process == nil {
                span.Process = batch.Process      // Process 정보 전파
            }
            err := t.spanWriter.WriteSpan(ctx, span)  // 개별 span 쓰기
            if err != nil {
                errs = append(errs, err)
            }
        }
    }
    return errors.Join(errs...)
}
```

**변환 흐름:**

```
ptrace.Traces (OTel 배치)
    │
    │  V1BatchesFromTraces()
    ▼
[]*model.Batch (Jaeger v1 배치)
    │
    │  for each batch:
    │    for each span:
    ▼
spanWriter.WriteSpan(span)  ← v1 API 호출
```

**주의사항**: v1 API는 개별 span 단위로 쓰므로, 배치 내 N개 span에 대해 N번의 `WriteSpan` 호출이 발생한다. 이는 성능 오버헤드가 있지만, v1 백엔드를 수정하지 않고도 v2 인터페이스를 제공할 수 있다.

### 7.3 TraceReader: v1 SpanReader를 v2 Reader로 감싸기

파일: `internal/storage/v2/v1adapter/tracereader.go`

```go
type TraceReader struct {
    spanReader spanstore.Reader
}

func NewTraceReader(spanReader spanstore.Reader) *TraceReader {
    return &TraceReader{
        spanReader: spanReader,
    }
}

func (tr *TraceReader) GetTraces(
    ctx context.Context,
    traceIDs ...tracestore.GetTraceParams,
) iter.Seq2[[]ptrace.Traces, error] {
    return func(yield func([]ptrace.Traces, error) bool) {
        for _, idParams := range traceIDs {
            query := spanstore.GetTraceParameters{
                TraceID:   ToV1TraceID(idParams.TraceID),
                StartTime: idParams.Start,
                EndTime:   idParams.End,
            }
            t, err := tr.spanReader.GetTrace(ctx, query)
            if err != nil {
                if errors.Is(err, spanstore.ErrTraceNotFound) {
                    continue   // 찾지 못한 ID는 무시 (v2 계약 준수)
                }
                yield(nil, err)
                return
            }
            batch := &model.Batch{Spans: t.GetSpans()}
            tr := V1BatchesToTraces([]*model.Batch{batch})
            if !yield([]ptrace.Traces{tr}, nil) {
                return  // caller가 중단 요청
            }
        }
    }
}
```

**이터레이터 변환**: v1의 동기적 `GetTrace()`를 v2의 이터레이터 `GetTraces()`로 변환한다. 각 trace ID에 대해 v1 API를 호출하고, 결과를 `ptrace.Traces`로 변환하여 yield한다.

```
v2 GetTraces(traceA, traceB, traceC)
    │
    ├─ v1.GetTrace(traceA) → *model.Trace → V1BatchesToTraces → yield
    │
    ├─ v1.GetTrace(traceB) → ErrTraceNotFound → continue (무시)
    │
    └─ v1.GetTrace(traceC) → *model.Trace → V1BatchesToTraces → yield
```

`FindTraces`와 `FindTraceIDs`도 유사한 패턴으로 어댑팅된다:

```go
func (tr *TraceReader) FindTraces(
    ctx context.Context,
    query tracestore.TraceQueryParams,
) iter.Seq2[[]ptrace.Traces, error] {
    return func(yield func([]ptrace.Traces, error) bool) {
        traces, err := tr.spanReader.FindTraces(ctx, query.ToSpanStoreQueryParameters())
        if err != nil {
            yield(nil, err)
            return
        }
        for _, trace := range traces {
            batch := &model.Batch{Spans: trace.GetSpans()}
            otelTrace := V1BatchesToTraces([]*model.Batch{batch})
            if !yield([]ptrace.Traces{otelTrace}, nil) {
                return
            }
        }
    }
}
```

### 7.4 역방향 어댑터: v2 Reader를 v1 SpanReader로 다운그레이드

파일: `internal/storage/v2/v1adapter/spanreader.go`

v2 인터페이스를 v1 인터페이스로 되돌리는 역방향 어댑터도 존재한다. 아직 v1 API를 사용하는 컴포넌트를 위한 것이다.

```go
type SpanReader struct {
    traceReader tracestore.Reader
}

func GetV1Reader(reader tracestore.Reader) spanstore.Reader {
    // 이미 v1 어댑터이면 내부 v1 reader를 직접 반환 (이중 어댑팅 방지)
    if tr, ok := reader.(*TraceReader); ok {
        return tr.spanReader
    }
    return &SpanReader{
        traceReader: reader,
    }
}
```

**이중 어댑팅 방지**: `GetV1Reader`와 `GetV1Writer`는 전달받은 인스턴스가 이미 v1adapter의 래퍼인지 확인한다. 만약 그렇다면 내부의 원본 v1 구현을 직접 반환하여 불필요한 변환을 피한다.

```
GetV1Reader(reader) 흐름:

Case 1: reader가 v1adapter.TraceReader인 경우
  ┌─────────────────────────────────────┐
  │ *TraceReader{spanReader: v1Reader}  │
  └─────────┬───────────────────────────┘
            │ unwrap
            ▼
       v1Reader  ← 원본 반환 (변환 0회)

Case 2: reader가 네이티브 v2 구현인 경우
  ┌──────────────────────────────┐
  │ native v2 tracestore.Reader  │
  └─────────┬────────────────────┘
            │ wrap
            ▼
  ┌──────────────────────────────┐
  │ SpanReader{traceReader: ...} │  ← 어댑터 생성 (변환 필요)
  └──────────────────────────────┘
```

### 7.5 SpanWriter 역방향 어댑터

파일: `internal/storage/v2/v1adapter/spanwriter.go`

```go
type SpanWriter struct {
    traceWriter tracestore.Writer
}

func (sw *SpanWriter) WriteSpan(ctx context.Context, span *model.Span) error {
    traces := V1BatchesToTraces([]*model.Batch{{Spans: []*model.Span{span}}})
    return sw.traceWriter.WriteTraces(ctx, traces)
}
```

개별 `WriteSpan` 호출을 `WriteTraces` 호출로 변환한다. 단일 span을 배치로 감싸서 v2 Writer에 전달한다.

### 7.6 변환 함수 (Translator)

파일: `internal/storage/v2/v1adapter/translator.go`

두 가지 핵심 변환 함수가 존재한다:

```go
// OTel traces → Jaeger v1 batches
func V1BatchesFromTraces(traces ptrace.Traces) []*model.Batch {
    batches := jaegertranslator.ProtoFromTraces(traces)
    spanMap := createSpanMapFromBatches(batches)
    transferWarningsToModelSpans(traces, spanMap)
    return batches
}

// Jaeger v1 batches → OTel traces
func V1BatchesToTraces(batches []*model.Batch) ptrace.Traces {
    traces, _ := jaegertranslator.ProtoToTraces(batches)  // 에러 안 남
    spanMap := jptrace.SpanMap(traces, func(s ptrace.Span) pcommon.SpanID {
        return s.SpanID()
    })
    transferWarningsToOTLPSpans(batches, spanMap)
    return traces
}
```

**주목할 점**: 변환 과정에서 **warnings 전파**를 별도로 처리한다. OTel 표준에는 "warnings" 개념이 없으므로, Jaeger는 span 속성(attribute)으로 warnings를 저장한다. 변환 시 이를 적절히 옮겨야 한다.

```
V1BatchesFromTraces 변환 상세 흐름:

ptrace.Traces
    │
    │  jaegertranslator.ProtoFromTraces()
    │  (OTel Collector Contrib 라이브러리)
    ▼
[]*model.Batch
    │
    │  createSpanMapFromBatches()
    │  → map[SpanID]*model.Span
    │
    │  transferWarningsToModelSpans()
    │  → OTel span의 warning 속성을 model.Span.Warnings로 이동
    ▼
[]*model.Batch (warnings 포함)
```

### 7.7 DependencyReader 어댑터

파일: `internal/storage/v2/v1adapter/depreader.go`

```go
type DependencyReader struct {
    reader dependencystore.Reader
}

func (dr *DependencyReader) GetDependencies(
    ctx context.Context,
    query depstore.QueryParameters,
) ([]model.DependencyLink, error) {
    return dr.reader.GetDependencies(ctx, query.EndTime, query.EndTime.Sub(query.StartTime))
}
```

v1의 `(endTs, lookback)` 시그니처를 v2의 `QueryParameters{StartTime, EndTime}` 구조체로 변환한다. 역방향 어댑터 `DowngradedDependencyReader`도 제공된다.

### 7.8 어댑터 사용 현황

현재 소스코드에서 v1 어댑터를 사용하는 백엔드와 네이티브 v2 구현을 사용하는 백엔드:

| 백엔드 | Writer | Reader | 어댑터 사용 |
|--------|--------|--------|-----------|
| Memory | 네이티브 v2 | 네이티브 v2 | X |
| Badger | v1adapter.NewTraceWriter | v1adapter.NewTraceReader | O |
| Cassandra | v1adapter.NewTraceWriter | 네이티브 v2 (ctracestore) | 부분적 |
| Elasticsearch | 네이티브 v2 | 네이티브 v2 | X |
| ClickHouse | 네이티브 v2 | 네이티브 v2 | X |
| gRPC | 네이티브 v2 | 네이티브 v2 | X |

이 표에서 알 수 있듯이, 최신 백엔드(ClickHouse, gRPC의 v2 버전)는 처음부터 v2 인터페이스로 구현되었고, 기존 백엔드(Badger)는 v1 어댑터를 통해 v2를 지원한다. Cassandra는 Writer만 아직 v1 어댑터를 사용하고, Reader는 네이티브 v2로 마이그레이션되었다.

---

## 8. 지연 초기화 ADR

### 8.1 문제 정의

파일: `docs/adr/003-lazy-storage-factory-initialization.md`

기존 구현에서 `jaegerstorage` Extension의 `Start()`는 설정된 **모든** 스토리지 백엔드를 즉시 초기화했다:

```go
// 기존 방식 (Start()에서 모든 팩토리 초기화)
func (s *storageExt) Start(ctx context.Context, host component.Host) error {
    for storageName, cfg := range s.config.TraceBackends {
        factory, err := storageconfig.CreateTraceStorageFactory(...)
        s.factories[storageName] = factory
    }
}
```

이로 인해 발생하는 문제:

| 문제 | 설명 |
|------|------|
| **자원 낭비** | 설정만 되고 사용되지 않는 백엔드가 연결, 메모리, 백그라운드 고루틴을 소비 |
| **불필요한 시작 실패** | 사용하지 않는 백엔드의 서버가 다운되면 전체 Jaeger 시작 실패 |
| **유연성 부족** | 공유 설정 파일에서 환경별로 다른 백엔드를 선택적으로 사용할 수 없음 |

### 8.2 실제 시나리오

ADR에서 제시한 구체적 시나리오:

```yaml
extensions:
  jaeger_storage:
    trace_backends:
      primary_es:
        elasticsearch: { ... }
      archive_cassandra:        # 실제로는 사용되지 않음
        cassandra: { ... }
      debug_memory:
        memory: { max_traces: 10000 }

  jaeger_query:
    storage:
      traces: primary_es
      traces_archive: debug_memory  # archive_cassandra가 아닌 debug_memory 사용
```

이 설정에서 `archive_cassandra`는 선언만 되고 어떤 파이프라인 컴포넌트도 참조하지 않는다. 그러나 기존 방식에서는 Cassandra 클러스터에 연결을 시도하고, 클러스터가 다운되어 있으면 Jaeger 전체가 시작되지 않았다.

### 8.3 검토된 두 가지 옵션

**Option 1: Two-Phase Factory Framework (Configure + Initialize)**

팩토리 인터페이스 자체를 변경하여 설정 검증(Configure)과 초기화(Initialize)를 분리한다:

```go
type ConfigurableFactory interface {
    Factory
    Configure(ctx context.Context) error       // 설정 검증만
}

type InitializableFactory interface {
    Factory
    Initialize(ctx context.Context) error      // 실제 연결
    IsInitialized() bool
}
```

장점: 설정 에러를 시작 시점에 모두 잡을 수 있다.
단점: 모든 백엔드 팩토리(6개 이상)를 수정해야 하고, 인터페이스 변경으로 하위 호환성이 깨진다.

**Option 2: Simple Lazy Initialization (선택됨)**

팩토리 인터페이스를 변경하지 않고, Extension의 `Start()`에서 초기화를 제거하고 `TraceStorageFactory()`에서 지연 생성한다.

장점: 코드 변경 최소화, 빠른 구현, 하위 호환성 유지.
단점: 사용되지 않는 백엔드의 설정 에러를 시작 시점에 잡지 못한다.

### 8.4 선택된 결정: Option 2 + Validate() 완화 조치

**완화 조치**: 팩토리 생성은 지연하되, `Start()`에서 `Validate()` 호출로 기본적인 설정 검증은 수행한다.

```go
func (s *storageExt) Start(_ context.Context, host component.Host) error {
    s.telset.Host = host
    // 설정 검증 (연결은 하지 않음)
    for name, cfg := range s.config.TraceBackends {
        if err := cfg.Validate(); err != nil {
            return fmt.Errorf("invalid configuration for trace storage '%s': %w", name, err)
        }
    }
    return nil
}
```

### 8.5 결과(Consequences)

| 구분 | 결과 |
|------|------|
| **긍정적** | 사용되지 않는 백엔드가 리소스를 소비하지 않음 |
| **긍정적** | 사용되지 않는 백엔드가 다운되어도 시작 성공 |
| **긍정적** | 최소한의 코드 변경으로 회귀 위험 감소 |
| **부정적** | 설정 에러가 최초 접근 시점까지 지연 (Validate로 부분 완화) |
| **부정적** | 시작 후 백엔드가 다운되면 첫 번째 접근 시 실패 |
| **중립적** | 초기화 로그 메시지가 시작 시점에서 첫 접근 시점으로 이동 |
| **중립적** | Shutdown 로직이 부분 초기화된 팩토리 맵을 처리해야 함 |

### 8.6 지연 초기화 시퀀스 다이어그램

```
┌──────────┐     ┌────────────┐     ┌──────────────┐     ┌──────────┐
│  Config  │     │ storageExt │     │ storageconfig│     │ Backend  │
│  (YAML)  │     │            │     │              │     │ (ES/CAS) │
└────┬─────┘     └─────┬──────┘     └──────┬───────┘     └────┬─────┘
     │                 │                   │                  │
     │  Start()        │                   │                  │
     │────────────────>│                   │                  │
     │                 │                   │                  │
     │                 │ Validate() only   │                  │
     │                 │──────────┐        │                  │
     │                 │          │        │                  │
     │                 │<─────────┘        │                  │
     │                 │                   │                  │
     │  (시간 경과)     │                   │                  │
     │                 │                   │                  │
     │  TraceStorageFactory("primary_es")  │                  │
     │────────────────>│                   │                  │
     │                 │                   │                  │
     │                 │ Lock + Check Cache│                  │
     │                 │──────────┐        │                  │
     │                 │          │ (miss) │                  │
     │                 │<─────────┘        │                  │
     │                 │                   │                  │
     │                 │ CreateTraceStorageFactory()           │
     │                 │──────────────────>│                  │
     │                 │                   │                  │
     │                 │                   │ NewFactory()     │
     │                 │                   │─────────────────>│
     │                 │                   │                  │
     │                 │                   │  Factory         │
     │                 │                   │<─────────────────│
     │                 │                   │                  │
     │                 │  Factory          │                  │
     │                 │<──────────────────│                  │
     │                 │                   │                  │
     │                 │ Cache + Return    │                  │
     │  Factory        │──────────┐        │                  │
     │<────────────────│          │        │                  │
     │                 │<─────────┘        │                  │
     │                 │                   │                  │
```

---

## 9. 메트릭 데코레이터

### 9.1 ReadMetricsDecorator

파일: `internal/storage/v2/api/tracestore/tracestoremetrics/reader_metrics.go`

Jaeger는 **Decorator 패턴**을 사용하여 스토리지 Reader에 메트릭 수집 기능을 투명하게 추가한다. 핵심 읽기 로직을 수정하지 않고 관측 가능성(observability)을 확보하는 설계이다.

```go
type ReadMetricsDecorator struct {
    traceReader          tracestore.Reader      // 원본 Reader
    findTracesMetrics    *queryMetrics
    findTraceIDsMetrics  *queryMetrics
    getTraceMetrics      *queryMetrics
    getServicesMetrics   *queryMetrics
    getOperationsMetrics *queryMetrics
}
```

### 9.2 queryMetrics 구조체

각 오퍼레이션별로 다음 메트릭을 수집한다:

```go
type queryMetrics struct {
    Errors     metrics.Counter `metric:"requests" tags:"result=err"`
    Successes  metrics.Counter `metric:"requests" tags:"result=ok"`
    Responses  metrics.Counter `metric:"responses"`
    ErrLatency metrics.Timer   `metric:"latency" tags:"result=err"`
    OKLatency  metrics.Timer   `metric:"latency" tags:"result=ok"`
}
```

| 메트릭 | 타입 | 태그 | 설명 |
|--------|------|------|------|
| `requests` | Counter | `result=ok` | 성공한 요청 수 |
| `requests` | Counter | `result=err` | 실패한 요청 수 |
| `responses` | Counter | - | 반환된 결과 수 |
| `latency` | Timer | `result=ok` | 성공 요청 지연 시간 |
| `latency` | Timer | `result=err` | 실패 요청 지연 시간 |

### 9.3 NewReaderDecorator 팩토리

```go
func NewReaderDecorator(
    traceReader tracestore.Reader,
    metricsFactory metrics.Factory,
) *ReadMetricsDecorator {
    return &ReadMetricsDecorator{
        traceReader:          traceReader,
        findTracesMetrics:    buildQueryMetrics("find_traces", metricsFactory),
        findTraceIDsMetrics:  buildQueryMetrics("find_trace_ids", metricsFactory),
        getTraceMetrics:      buildQueryMetrics("get_trace", metricsFactory),
        getServicesMetrics:   buildQueryMetrics("get_services", metricsFactory),
        getOperationsMetrics: buildQueryMetrics("get_operations", metricsFactory),
    }
}

func buildQueryMetrics(operation string, metricsFactory metrics.Factory) *queryMetrics {
    qMetrics := &queryMetrics{}
    scoped := metricsFactory.Namespace(metrics.NSOptions{
        Name: "",
        Tags: map[string]string{"operation": operation},
    })
    metrics.Init(qMetrics, scoped, nil)   // 구조체 태그 기반 자동 초기화
    return qMetrics
}
```

`metrics.Init()`은 Go 구조체 태그를 리플렉션으로 읽어서 메트릭 인스턴스를 자동 생성한다. `metric:"requests"` 태그가 Counter를, `metric:"latency"` 태그가 Timer를 생성한다.

### 9.4 이터레이터 메트릭 래핑

이터레이터 기반 메서드(`GetTraces`, `FindTraces`, `FindTraceIDs`)의 메트릭 수집은 특별한 처리가 필요하다. 이터레이터는 지연 실행(lazy evaluation)되므로, 메트릭은 이터레이터가 **실제로 소비될 때** 수집된다.

```go
func (m *ReadMetricsDecorator) GetTraces(
    ctx context.Context,
    traceIDs ...tracestore.GetTraceParams,
) iter.Seq2[[]ptrace.Traces, error] {
    return func(yield func([]ptrace.Traces, error) bool) {
        start := time.Now()                    // 타이머 시작
        var err error
        length := 0
        defer func() {
            m.getTraceMetrics.emit(err, time.Since(start), length)  // 종료 시 메트릭 방출
        }()
        getTraceIter := m.traceReader.GetTraces(ctx, traceIDs...)
        for traces, iterErr := range getTraceIter {
            err = iterErr
            length += len(traces)              // 결과 수 누적
            if !yield(traces, iterErr) {
                return                         // caller 중단 시 종료
            }
        }
    }
}
```

**핵심 패턴**: `defer`를 사용하여 이터레이터 소비가 완료(또는 중단)된 후에 메트릭을 방출한다. 이를 통해 전체 소비 시간과 결과 수를 정확히 측정한다.

```
메트릭 데코레이터 실행 흐름:

caller → GetTraces()
           │
           │ 이터레이터 반환 (아직 실행 안 됨)
           │
           ▼
caller → for traces, err := range iter {  ← 이터레이터 실행 시작
           │                                  start = time.Now()
           │
           │  yield #1: traces (len=1)
           │  length += 1
           │
           │  yield #2: traces (len=1)
           │  length += 1
           │
           }  ← 이터레이터 종료
              │
              │  defer 실행
              │  emit(err=nil, latency=120ms, responses=2)
              ▼
```

### 9.5 비-이터레이터 메서드의 메트릭

`GetServices`와 `GetOperations`는 슬라이스를 반환하므로 더 단순하다:

```go
func (m *ReadMetricsDecorator) GetServices(ctx context.Context) ([]string, error) {
    start := time.Now()
    retMe, err := m.traceReader.GetServices(ctx)
    m.getServicesMetrics.emit(err, time.Since(start), len(retMe))
    return retMe, err
}
```

### 9.6 백엔드에서의 데코레이터 사용

각 백엔드 팩토리의 `CreateTraceReader()`에서 데코레이터를 적용한다:

```go
// Memory 팩토리
func (f *Factory) CreateTraceReader() (tracestore.Reader, error) {
    return tracestoremetrics.NewReaderDecorator(f.store, f.metricsFactory), nil
}

// Elasticsearch 팩토리
func (f *Factory) CreateTraceReader() (tracestore.Reader, error) {
    params := f.coreFactory.GetSpanReaderParams()
    return tracestoremetrics.NewReaderDecorator(
        v2tracestore.NewTraceReader(params), f.metricsFactory), nil
}

// Cassandra 팩토리
func (f *Factory) CreateTraceReader() (tracestore.Reader, error) {
    corereader, err := cspanstore.NewSpanReader(...)
    if err != nil {
        return nil, err
    }
    return tracestoremetrics.NewReaderDecorator(
        ctracestore.NewTraceReader(corereader), f.metricsFactory), nil
}
```

**주목할 점**: Writer에는 메트릭 데코레이터가 적용되지 않는다. 이는 Writer가 OTel Collector 파이프라인의 Exporter로 동작하며, Collector 자체가 Exporter 메트릭을 수집하기 때문이다. Reader는 Jaeger Query 서비스가 직접 호출하므로 별도의 메트릭 수집이 필요하다.

### 9.7 메트릭 네임스페이스 구조

```
jaeger.                              ← 최상위 네임스페이스
  storage.                           ← storageconfig에서 추가
    {name}.                          ← 백엔드 이름 (예: primary_es)
      tracestore.                    ← role 태그
        {operation}.                 ← 오퍼레이션 태그
          requests{result=ok}        ← 성공 카운터
          requests{result=err}       ← 실패 카운터
          responses                  ← 결과 카운터
          latency{result=ok}         ← 성공 지연
          latency{result=err}        ← 실패 지연

예시 메트릭 이름:
  jaeger.storage.primary_es.tracestore.get_trace.requests{result=ok}
  jaeger.storage.primary_es.tracestore.find_traces.latency{result=err}
```

---

## 10. 백엔드별 팩토리 구현 비교

### 10.1 구현 전략 분류

백엔드 팩토리는 구현 전략에 따라 세 가지로 분류할 수 있다:

**Type A: 네이티브 v2 구현**

처음부터 v2 인터페이스(`tracestore.Reader`, `tracestore.Writer`)로 구현되었다.

```
Memory, Elasticsearch, ClickHouse, gRPC
```

**Type B: v1 어댑터 사용**

v1 구현(`spanstore.Reader`, `spanstore.Writer`)을 v1adapter로 감싸서 v2 인터페이스를 제공한다.

```
Badger
```

**Type C: 하이브리드 (부분 네이티브)**

일부는 네이티브 v2, 일부는 v1 어댑터를 사용한다.

```
Cassandra (Reader: 네이티브 v2, Writer: v1 어댑터)
```

### 10.2 팩토리 구현 상세 비교

```
┌─────────────────────────────────────────────────────────────────┐
│                    Memory Factory                                │
├─────────────────────────────────────────────────────────────────┤
│ 구현: internal/storage/v2/memory/factory.go                      │
│ 타입: 네이티브 v2                                                │
│ 특징:                                                            │
│  - Store 객체가 Reader와 Writer를 동시에 구현                     │
│  - SamplingStoreFactory, Purger, depstore.Factory 모두 구현      │
│  - Reader에 tracestoremetrics.NewReaderDecorator 적용            │
│  - Lock: 인메모리 Lock 구현 (단일 프로세스)                       │
│  - 가장 간단한 구현 (59줄)                                       │
└─────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────┐
│                    Badger Factory                                 │
├─────────────────────────────────────────────────────────────────┤
│ 구현: internal/storage/v2/badger/factory.go                      │
│ 타입: v1 어댑터 사용                                             │
│ 특징:                                                            │
│  - v1 badger.Factory를 내부에 보유                               │
│  - 모든 Create* 메서드가 v1adapter로 래핑                        │
│  - SamplingStoreFactory, Purger, Lock 지원                       │
│  - DB 파일 열기, 백그라운드 GC 등 v1 Factory가 처리               │
└─────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────┐
│                    Cassandra Factory                              │
├─────────────────────────────────────────────────────────────────┤
│ 구현: internal/storage/v2/cassandra/factory.go                   │
│ 타입: 하이브리드                                                 │
│ 특징:                                                            │
│  - Reader: 네이티브 v2 (ctracestore.NewTraceReader)              │
│  - Writer: v1adapter.NewTraceWriter                              │
│  - 메트릭 데코레이터 적용 (Reader)                                │
│  - SamplingStoreFactory, Purger, Lock 모두 지원                  │
│  - 가장 많은 기능 지원 백엔드 중 하나                              │
└─────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────┐
│                    Elasticsearch Factory                          │
├─────────────────────────────────────────────────────────────────┤
│ 구현: internal/storage/v2/elasticsearch/factory.go               │
│ 타입: 네이티브 v2                                                │
│ 특징:                                                            │
│  - coreFactory (v1 FactoryBase) 내부 보유                        │
│  - Reader/Writer: 네이티브 v2 (v2tracestore.NewTraceReader 등)  │
│  - SamplingStore 지원 (v1 기반)                                  │
│  - Purger 지원 (통합 테스트용)                                    │
│  - ensureRequiredFields: span.kind, error 태그 자동 추가         │
│  - HTTP Auth 지원 (extensionauth.HTTPClient)                     │
└─────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────┐
│                    ClickHouse Factory                             │
├─────────────────────────────────────────────────────────────────┤
│ 구현: internal/storage/v2/clickhouse/factory.go                  │
│ 타입: 네이티브 v2                                                │
│ 특징:                                                            │
│  - 초기화 시 Ping으로 연결 확인                                   │
│  - 선택적 스키마 자동 생성 (11개 테이블/뷰)                       │
│  - Purger 지원 (5개 테이블 TRUNCATE)                             │
│  - Sampling/Lock 미지원                                          │
│  - 가장 최근에 추가된 백엔드                                      │
└─────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────┐
│                    gRPC (Remote) Factory                          │
├─────────────────────────────────────────────────────────────────┤
│ 구현: internal/storage/v2/grpc/factory.go                        │
│ 타입: 네이티브 v2                                                │
│ 특징:                                                            │
│  - Reader/Writer 별도 gRPC 커넥션                                │
│  - Reader: 트레이싱 활성화 (재귀 트레이스 생성 안전)               │
│  - Writer: 트레이싱 비활성화 (재귀 방지)                          │
│  - 베어러 토큰 인터셉터, 멀티테넌시 인터셉터 지원                  │
│  - Sampling/Purger/Lock 미지원                                   │
└─────────────────────────────────────────────────────────────────┘
```

### 10.3 gRPC Factory의 Reader/Writer 커넥션 분리

gRPC Factory는 Reader와 Writer에 별도의 gRPC 커넥션을 사용하는 독특한 설계를 채택한다:

```go
type Factory struct {
    // readerConn: 트레이싱 활성화 (읽기 요청 추적 가능)
    readerConn *grpc.ClientConn
    // writerConn: 트레이싱 비활성화 (재귀 트레이스 방지)
    writerConn *grpc.ClientConn
}
```

**왜 Writer에서 트레이싱을 비활성화하는가?**

Jaeger Collector가 span을 받아서 스토리지에 쓸 때, 이 쓰기 동작 자체가 새로운 span을 생성하면 무한 루프가 발생한다:

```
span 수신 → WriteTraces() → gRPC 호출 → 새 span 생성 → WriteTraces() → ...
```

Writer의 gRPC 커넥션에 `noop.NewTracerProvider()`를 사용하여 이 문제를 방지한다.

### 10.4 전체 아키텍처 종합 다이어그램

```
┌────────────────────────────────────────────────────────────────────────┐
│                        Jaeger v2 Storage Architecture                  │
├────────────────────────────────────────────────────────────────────────┤
│                                                                        │
│  ┌───────────────────────────────────────────────────────────┐         │
│  │              OTel Collector Pipeline                       │         │
│  │                                                           │         │
│  │  Receiver → Processor → Exporter                          │         │
│  │              ↓                    ↓                        │         │
│  │         Query Service       WriteTraces()                  │         │
│  └──────────┬──────────────────────┬─────────────────────────┘         │
│             │                      │                                   │
│  ┌──────────▼──────────────────────▼──────────────────────┐           │
│  │           jaeger_storage Extension                      │           │
│  │                                                         │           │
│  │  TraceStorageFactory(name) → sync.Mutex → Lazy Init     │           │
│  │                                                         │           │
│  │  ┌─────────────────┐  ┌──────────────────┐             │           │
│  │  │ Config (YAML)   │  │ Factory Cache    │             │           │
│  │  │ TraceBackends   │→ │ map[name]Factory │             │           │
│  │  │ MetricBackends  │  │                  │             │           │
│  │  └─────────────────┘  └──────┬───────────┘             │           │
│  └──────────────────────────────┼─────────────────────────┘           │
│                                 │                                      │
│  ┌──────────────────────────────▼────────────────────────────────┐    │
│  │        storageconfig.CreateTraceStorageFactory()               │    │
│  │        switch on backend type                                  │    │
│  └──────┬──────┬──────┬──────┬──────┬──────┬──────┬─────────────┘    │
│         │      │      │      │      │      │      │                   │
│     ┌───▼──┐┌──▼──┐┌──▼──┐┌──▼──┐┌──▼──┐┌──▼──┐┌──▼────┐            │
│     │Memory││Badgr││ gRPC ││Cass.││  ES  ││  OS  ││Click │            │
│     │      ││     ││      ││     ││      ││      ││House │            │
│     └──┬───┘└──┬──┘└──┬──┘└──┬──┘└──┬──┘└──┬──┘└──┬────┘            │
│        │       │      │      │      │      │      │                   │
│     ┌──▼───────▼──────▼──────▼──────▼──────▼──────▼────┐             │
│     │           tracestore.Factory                      │             │
│     │  CreateTraceReader() → Reader (+ MetricsDecorator)│             │
│     │  CreateTraceWriter() → Writer                     │             │
│     └──────────────────┬────────────────────────────────┘             │
│                        │                                              │
│     ┌──────────────────▼────────────────────────────────┐             │
│     │  Optional Interfaces (타입 단언으로 확인)           │             │
│     │                                                    │             │
│     │  depstore.Factory      → CreateDependencyReader()  │             │
│     │  SamplingStoreFactory  → CreateSamplingStore()     │             │
│     │  Purger                → Purge()                   │             │
│     │  io.Closer             → Close()                   │             │
│     └────────────────────────────────────────────────────┘             │
│                                                                        │
└────────────────────────────────────────────────────────────────────────┘
```

---

## 요약

Jaeger의 스토리지 아키텍처는 다음과 같은 핵심 설계 원칙으로 구성된다:

1. **Abstract Factory 패턴**: `tracestore.Factory` 인터페이스를 통해 백엔드를 추상화하고, 설정 기반으로 구체적 팩토리를 디스패치한다

2. **v1/v2 이중 API**: OTel-네이티브 v2 API(`ptrace.Traces`, `iter.Seq2`)와 레거시 v1 API(`model.Span`) 간의 어댑터 레이어를 통해 점진적 마이그레이션을 지원한다

3. **지연 초기화**: ADR-003에 따라 팩토리 생성을 최초 접근 시점까지 지연하여, 사용되지 않는 백엔드의 리소스 낭비와 시작 실패를 방지한다

4. **Decorator 패턴 메트릭**: `ReadMetricsDecorator`가 Reader를 투명하게 감싸서, 핵심 로직 변경 없이 요청 수, 지연 시간, 결과 수 메트릭을 수집한다

5. **선택적 기능 인터페이스**: `depstore.Factory`, `SamplingStoreFactory`, `Purger` 등을 별도 인터페이스로 분리하고, 타입 단언으로 백엔드의 기능 지원 여부를 확인한다

6. **OTel Collector 통합**: OTel Collector의 Extension 메커니즘을 활용하여 스토리지 라이프사이클을 관리하고, Exporter 인터페이스와 호환되는 Writer API를 제공한다

이 아키텍처를 통해 Jaeger는 인메모리 개발 환경부터 대규모 분산 프로덕션 환경까지, 단일 코드베이스로 7가지 이상의 스토리지 백엔드를 지원한다.
