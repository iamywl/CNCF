# Query 서비스

## 개요

Jaeger Query 서비스는 분산 트레이싱 데이터를 조회하고 시각화하기 위한 핵심 컴포넌트이다.
스토리지 백엔드(Elasticsearch, ClickHouse 등)에 저장된 트레이스 데이터를 읽어
HTTP API와 gRPC API를 통해 클라이언트(Jaeger UI, CLI, 외부 시스템)에 제공한다.

Query 서비스의 핵심 역할:
- **트레이스 조회**: Trace ID 기반 단건 조회, 조건 기반 검색
- **서비스/오퍼레이션 메타데이터**: 서비스 목록, 오퍼레이션 목록 제공
- **의존성 그래프**: 서비스 간 호출 관계 분석
- **트레이스 보정**: 클록 스큐 보정, 스팬 ID 중복 제거 등 후처리
- **아카이브**: 트레이스를 아카이브 스토리지로 이동
- **UI 서빙**: Jaeger UI 정적 자산 + 설정 주입

---

## 1. 전체 아키텍처

### 1.1 OTel Collector Extension 구조

Jaeger v2에서 Query 서비스는 OpenTelemetry Collector의 **Extension**으로 구현된다.
`jaegerquery` 패키지가 이 extension을 정의하며, collector 파이프라인의 일부로 동작한다.

```
소스 경로: cmd/jaeger/internal/extension/jaegerquery/
```

```
┌─────────────────────────────────────────────────────────┐
│                   OTel Collector                         │
│                                                          │
│  ┌──────────────────────────────────────────────────┐   │
│  │            jaegerquery Extension                   │   │
│  │                                                    │   │
│  │  ┌──────────────┐    ┌─────────────────────────┐  │   │
│  │  │  QueryService │    │       Server             │  │   │
│  │  │  (querysvc)   │    │                          │  │   │
│  │  │               │    │  ┌──────┐  ┌──────────┐ │  │   │
│  │  │  traceReader  │◄───│  │ HTTP │  │   gRPC   │ │  │   │
│  │  │  depReader    │    │  │:16686│  │  :16685  │ │  │   │
│  │  │  adjuster     │    │  └──────┘  └──────────┘ │  │   │
│  │  │  archive R/W  │    │                          │  │   │
│  │  └──────────────┘    └─────────────────────────┘  │   │
│  └──────────────────────────────────────────────────┘   │
│                           │                              │
│                           ▼                              │
│  ┌──────────────────────────────────────────────────┐   │
│  │          jaegerstorage Extension                   │   │
│  │  (Elasticsearch / ClickHouse / Memory / ...)       │   │
│  └──────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────┘
```

### 1.2 Extension 인터페이스

```go
// 소스: cmd/jaeger/internal/extension/jaegerquery/extension.go

type Extension interface {
    extension.Extension
    QueryService() *querysvc.QueryService
}
```

다른 extension에서 Query 서비스에 접근할 때 `GetExtension(host)`를 사용한다.
이 함수는 host에 등록된 extension 중 `componentType`이 일치하는 것을 찾아 반환한다:

```go
func GetExtension(host component.Host) (Extension, error) {
    var id component.ID
    var comp component.Component
    for i, ext := range host.GetExtensions() {
        if i.Type() == componentType {
            id, comp = i, ext
            break
        }
    }
    // ...
    ext, ok := comp.(Extension)
    // ...
    return ext, nil
}
```

### 1.3 의존성 순서

`server` 구조체는 `extensioncapabilities.Dependent` 인터페이스를 구현하여
`jaegerstorage` extension이 먼저 시작되도록 보장한다:

```go
// 소스: cmd/jaeger/internal/extension/jaegerquery/server.go

func (*server) Dependencies() []component.ID {
    return []component.ID{jaegerstorage.ID}
}
```

---

## 2. 설정 (Configuration)

### 2.1 Config 구조체

```go
// 소스: cmd/jaeger/internal/extension/jaegerquery/config.go

type Config struct {
    queryapp.QueryOptions `mapstructure:",squash"`
    Storage Storage       `mapstructure:"storage"`
}

type Storage struct {
    TracesPrimary string `mapstructure:"traces" valid:"required"`
    TracesArchive string `mapstructure:"traces_archive" valid:"optional"`
    Metrics       string `mapstructure:"metrics" valid:"optional"`
}
```

`Config`는 두 가지 주요 부분으로 구성된다:
- **QueryOptions**: HTTP/gRPC 서버 설정, UI 설정, 보안 설정 등
- **Storage**: 스토리지 백엔드 이름 참조 (jaegerstorage extension에서 정의)

### 2.2 QueryOptions 상세

```go
// 소스: cmd/jaeger/internal/extension/jaegerquery/internal/flags.go

type QueryOptions struct {
    BasePath               string                   `mapstructure:"base_path"`
    UIConfig               UIConfig                 `mapstructure:"ui"`
    BearerTokenPropagation bool                     `mapstructure:"bearer_token_propagation"`
    Tenancy                tenancy.Options           `mapstructure:"multi_tenancy"`
    MaxClockSkewAdjust     time.Duration            `mapstructure:"max_clock_skew_adjust"`
    MaxTraceSize           int                      `mapstructure:"max_trace_size"`
    EnableTracing          bool                     `mapstructure:"enable_tracing"`
    HTTP                   confighttp.ServerConfig  `mapstructure:"http"`
    GRPC                   configgrpc.ServerConfig  `mapstructure:"grpc"`
}
```

### 2.3 기본값

```go
func DefaultQueryOptions() QueryOptions {
    return QueryOptions{
        MaxClockSkewAdjust: 0, // 기본값: 비활성화
        HTTP: confighttp.ServerConfig{
            NetAddr: confignet.AddrConfig{
                Endpoint:  ports.PortToHostPort(ports.QueryHTTP),  // :16686
                Transport: confignet.TransportTypeTCP,
            },
        },
        GRPC: configgrpc.ServerConfig{
            NetAddr: confignet.AddrConfig{
                Endpoint:  ports.PortToHostPort(ports.QueryGRPC),  // :16685
                Transport: confignet.TransportTypeTCP,
            },
        },
    }
}
```

| 설정 항목 | 기본값 | 설명 |
|-----------|--------|------|
| HTTP 포트 | 16686 | UI + HTTP API 서빙 |
| gRPC 포트 | 16685 | gRPC API 서빙 |
| MaxClockSkewAdjust | 0 (비활성화) | 클록 스큐 보정 최대 허용 시간 |
| MaxTraceSize | 0 (무제한) | 트레이스당 최대 스팬 수 |
| BasePath | "" | HTTP 라우트 기본 경로 |
| EnableTracing | false | Query 서비스 자체 트레이싱 |

### 2.4 UIConfig

```go
type UIConfig struct {
    ConfigFile string `mapstructure:"config_file" valid:"optional"`
    AssetsPath string `mapstructure:"assets_path" valid:"optional"`
    LogAccess  bool   `mapstructure:"log_access" valid:"optional"`
}
```

- `ConfigFile`: UI 설정 파일 경로 (.json 또는 .js)
- `AssetsPath`: 커스텀 UI 자산 경로 (비어 있으면 임베디드 자산 사용)
- `LogAccess`: 정적 자산 접근 로깅 활성화

---

## 3. 서버 시작 흐름

### 3.1 Start() 메서드

`server.Start()`는 extension이 활성화될 때 호출되며, 전체 Query 서비스를 초기화한다:

```go
// 소스: cmd/jaeger/internal/extension/jaegerquery/server.go

func (s *server) Start(ctx context.Context, host component.Host) error {
    // 1. TracerProvider 초기화 (선택적)
    var tp trace.TracerProvider = nooptrace.NewTracerProvider()
    if s.config.EnableTracing {
        tracerProvider, tracerCloser, err := jtracer.NewProvider(ctx, "jaeger")
        // ...
        tp = tracerProvider
    }

    // 2. 텔레메트리 설정
    telset := telemetry.FromOtelComponent(s.telset, host)
    telset.TracerProvider = tp
    telset.Metrics = telset.Metrics.
        Namespace(metrics.NSOptions{Name: "jaeger"}).
        Namespace(metrics.NSOptions{Name: "query"})

    // 3. 스토리지 팩토리에서 Reader 생성
    tf, err := jaegerstorage.GetTraceStoreFactory(
        s.config.Storage.TracesPrimary, host)
    traceReader, err := tf.CreateTraceReader()

    // 4. 의존성 Reader 생성
    df, ok := tf.(depstore.Factory)
    depReader, err := df.CreateDependencyReader()

    // 5. QueryService 생성
    opts := querysvc.QueryServiceOptions{
        MaxClockSkewAdjust: s.config.MaxClockSkewAdjust,
        MaxTraceSize:       s.config.MaxTraceSize,
    }
    s.addArchiveStorage(&opts, host)  // 아카이브 스토리지 추가
    qs := querysvc.NewQueryService(traceReader, depReader, opts)
    s.qs = qs

    // 6. 메트릭 Reader 생성
    mqs, err := s.createMetricReader(host)

    // 7. 테넌시 매니저 생성
    tm := tenancy.NewManager(&s.config.Tenancy)

    // 8. HTTP/gRPC 서버 생성 및 시작
    s.server, err = queryapp.NewServer(
        ctx, qs, mqs, &s.config.QueryOptions, tm, telset)
    s.server.Start(ctx)

    return nil
}
```

### 3.2 시작 흐름 시퀀스 다이어그램

```
Collector              server              jaegerstorage         QueryService          Server
   │                     │                      │                     │                  │
   │── Start(ctx,host) ──►                      │                     │                  │
   │                     │── GetTraceStore ─────►                     │                  │
   │                     │◄── traceReader ──────│                     │                  │
   │                     │── GetDepStore ───────►                     │                  │
   │                     │◄── depReader ────────│                     │                  │
   │                     │── addArchiveStorage ─►                     │                  │
   │                     │◄── archiveR/W ───────│                     │                  │
   │                     │                      │                     │                  │
   │                     │── NewQueryService(reader, depReader, opts) ►                  │
   │                     │◄── qs ──────────────────────────────────────│                  │
   │                     │                      │                     │                  │
   │                     │── NewServer(ctx, qs, mqs, opts, tm, telset) ──────────────────►
   │                     │                      │                     │                  │
   │                     │── server.Start(ctx) ─────────────────────────────────────────►
   │                     │                      │                     │    ┌──HTTP:16686 │
   │                     │                      │                     │    │  gRPC:16685 │
   │◄── nil ─────────────│                      │                     │    └─────────────│
```

### 3.3 아카이브 스토리지 초기화

```go
func (s *server) addArchiveStorage(
    opts *querysvc.QueryServiceOptions,
    host component.Host,
) error {
    if s.config.Storage.TracesArchive == "" {
        s.telset.Logger.Info("Archive storage not configured")
        return nil
    }
    f, err := jaegerstorage.GetTraceStoreFactory(
        s.config.Storage.TracesArchive, host)
    // ...
    traceReader, traceWriter := s.initArchiveStorage(f)
    if traceReader == nil || traceWriter == nil {
        return nil
    }
    opts.ArchiveTraceReader = traceReader
    opts.ArchiveTraceWriter = traceWriter
    return nil
}
```

아카이브 스토리지가 설정되면 `QueryServiceOptions`에 Reader와 Writer가 추가된다.
이를 통해 `GetTraces`에서 Primary에 없는 트레이스를 Archive에서 폴백 조회할 수 있다.

---

## 4. QueryService 핵심 로직

### 4.1 구조체와 생성

```go
// 소스: cmd/jaeger/internal/extension/jaegerquery/querysvc/service.go

type QueryService struct {
    traceReader      tracestore.Reader
    dependencyReader depstore.Reader
    adjuster         adjuster.Adjuster
    options          QueryServiceOptions
}

func NewQueryService(
    traceReader tracestore.Reader,
    dependencyReader depstore.Reader,
    options QueryServiceOptions,
) *QueryService {
    qsvc := &QueryService{
        traceReader:      traceReader,
        dependencyReader: dependencyReader,
        adjuster: adjuster.Sequence(
            adjuster.StandardAdjusters(options.MaxClockSkewAdjust)...,
        ),
        options: options,
    }
    return qsvc
}
```

`QueryService`는 생성 시 `StandardAdjusters` 파이프라인을 구성한다.
이 파이프라인은 `adjuster.Sequence`로 래핑되어 모든 adjuster가 순서대로 실행된다.

### 4.2 GetTraces - 핵심 조회 메서드

`GetTraces`는 Query 서비스의 가장 중요한 메서드로, Trace ID 기반 조회를 수행한다:

```go
func (qs QueryService) GetTraces(
    ctx context.Context,
    params GetTraceParams,
) iter.Seq2[[]ptrace.Traces, error] {
    getTracesIter := qs.traceReader.GetTraces(ctx, params.TraceIDs...)
    return func(yield func([]ptrace.Traces, error) bool) {
        // 1. Primary 스토리지에서 조회
        foundTraceIDs, proceed := qs.receiveTraces(
            getTracesIter, yield, params.RawTraces)
        // 2. 못 찾은 Trace ID가 있고 Archive가 설정되어 있으면 Archive 폴백
        if proceed && qs.options.ArchiveTraceReader != nil {
            var missingTraceIDs []tracestore.GetTraceParams
            for _, id := range params.TraceIDs {
                if _, found := foundTraceIDs[id.TraceID]; !found {
                    missingTraceIDs = append(missingTraceIDs, id)
                }
            }
            if len(missingTraceIDs) > 0 {
                getArchiveTracesIter := qs.options.ArchiveTraceReader.GetTraces(
                    ctx, missingTraceIDs...)
                qs.receiveTraces(getArchiveTracesIter, yield, params.RawTraces)
            }
        }
    }
}
```

**핵심 동작**:
1. Primary 스토리지에서 트레이스를 조회한다
2. 조회 결과에서 어떤 TraceID가 발견되었는지 추적한다
3. 찾지 못한 TraceID가 있고 Archive Reader가 설정되어 있으면, Archive에서 다시 조회한다
4. 이터레이터 기반으로 결과를 스트리밍 반환한다

### 4.3 receiveTraces - 트레이스 수신 및 보정

```go
func (qs QueryService) receiveTraces(
    seq iter.Seq2[[]ptrace.Traces, error],
    yield func([]ptrace.Traces, error) bool,
    rawTraces bool,
) (map[pcommon.TraceID]struct{}, bool) {
    foundTraceIDs := make(map[pcommon.TraceID]struct{})
    proceed := true

    processTraces := func(traces []ptrace.Traces, err error) bool {
        if err != nil {
            proceed = yield(nil, err)
            return proceed
        }
        for _, trace := range traces {
            if !rawTraces {
                qs.adjuster.Adjust(trace)  // adjuster 파이프라인 적용
            }
            jptrace.SpanIter(trace)(func(_ jptrace.SpanIterPos, span ptrace.Span) bool {
                foundTraceIDs[span.TraceID()] = struct{}{}
                return true
            })
        }
        proceed = yield(traces, nil)
        return proceed
    }

    if rawTraces {
        seq(processTraces)     // 원본 그대로 전달
    } else {
        // AggregateTracesWithLimit로 분산된 청크를 하나의 트레이스로 집계
        jptrace.AggregateTracesWithLimit(seq, qs.options.MaxTraceSize)(
            func(trace ptrace.Traces, err error) bool {
                return processTraces([]ptrace.Traces{trace}, err)
            },
        )
    }

    return foundTraceIDs, proceed
}
```

**RawTraces vs 보정된 트레이스**:

| 모드 | rawTraces=true | rawTraces=false (기본) |
|------|---------------|----------------------|
| 집계 | 청크 그대로 반환 | AggregateTracesWithLimit로 하나의 트레이스로 합침 |
| Adjuster | 적용하지 않음 | StandardAdjusters 전체 적용 |
| MaxTraceSize | 적용하지 않음 | 스팬 수 제한 적용 |
| 용도 | 디버깅, 원본 데이터 확인 | UI 표시, 일반 사용 |

### 4.4 FindTraces - 검색 기반 조회

```go
func (qs QueryService) FindTraces(
    ctx context.Context,
    query TraceQueryParams,
) iter.Seq2[[]ptrace.Traces, error] {
    return func(yield func([]ptrace.Traces, error) bool) {
        tracesIter := qs.traceReader.FindTraces(ctx, query.TraceQueryParams)
        qs.receiveTraces(tracesIter, yield, query.RawTraces)
    }
}
```

`FindTraces`는 `GetTraces`와 달리 Archive 폴백이 없다. 검색 조건(서비스, 오퍼레이션, 시간 범위, 태그 등)에 맞는 트레이스를 스토리지에서 찾아 반환한다.

### 4.5 ArchiveTrace - 아카이브 저장

```go
func (qs QueryService) ArchiveTrace(
    ctx context.Context,
    query tracestore.GetTraceParams,
) error {
    if qs.options.ArchiveTraceWriter == nil {
        return errNoArchiveSpanStorage
    }
    // 먼저 GetTraces로 트레이스를 조회
    getTracesIter := qs.GetTraces(ctx, GetTraceParams{
        TraceIDs: []tracestore.GetTraceParams{query},
    })
    var found bool
    var archiveErr error
    getTracesIter(func(traces []ptrace.Traces, err error) bool {
        // ...
        for _, trace := range traces {
            found = true
            err = qs.options.ArchiveTraceWriter.WriteTraces(ctx, trace)
            if err != nil {
                archiveErr = errors.Join(archiveErr, err)
            }
        }
        return true
    })
    if archiveErr == nil && !found {
        return spanstore.ErrTraceNotFound
    }
    return archiveErr
}
```

아카이브 동작:
1. Archive Writer가 설정되어 있는지 확인
2. `GetTraces`로 트레이스를 조회 (Primary + Archive 폴백 포함)
3. 조회된 트레이스를 Archive Writer로 기록
4. 찾지 못하면 `ErrTraceNotFound` 반환

### 4.6 기타 메서드

```go
func (qs QueryService) GetServices(ctx context.Context) ([]string, error) {
    services, err := qs.traceReader.GetServices(ctx)
    if services == nil {
        services = []string{}  // nil 대신 빈 슬라이스 반환
    }
    return services, err
}

func (qs QueryService) GetOperations(
    ctx context.Context,
    query tracestore.OperationQueryParams,
) ([]tracestore.Operation, error) {
    return qs.traceReader.GetOperations(ctx, query)
}

func (qs QueryService) GetDependencies(
    ctx context.Context,
    endTs time.Time,
    lookback time.Duration,
) ([]model.DependencyLink, error) {
    return qs.dependencyReader.GetDependencies(ctx, depstore.QueryParameters{
        StartTime: endTs.Add(-lookback),
        EndTime:   endTs,
    })
}

func (qs QueryService) GetCapabilities() StorageCapabilities {
    return StorageCapabilities{
        ArchiveStorage: qs.options.hasArchiveStorage(),
    }
}
```

`GetCapabilities`는 UI에서 아카이브 기능 버튼 표시 여부를 결정하는 데 사용된다.
`hasArchiveStorage()`는 ArchiveTraceReader와 ArchiveTraceWriter가 모두 설정되어 있으면 true를 반환한다.

---

## 5. Adjuster 파이프라인

### 5.1 Adjuster 인터페이스

```go
// 소스: cmd/jaeger/internal/extension/jaegerquery/internal/adjuster/adjuster.go

type Adjuster interface {
    Adjust(ptrace.Traces)
}

type Func func(traces ptrace.Traces)

func (f Func) Adjust(traces ptrace.Traces) {
    f(traces)
}
```

`Adjuster`는 `ptrace.Traces`를 인자로 받아 in-place로 수정한다.
`Func` 타입으로 함수를 Adjuster 인터페이스로 래핑할 수 있다.

### 5.2 Sequence 컴포지터

```go
func Sequence(adjusters ...Adjuster) Adjuster {
    return sequence{adjusters: adjusters}
}

type sequence struct {
    adjusters []Adjuster
}

func (c sequence) Adjust(traces ptrace.Traces) {
    for _, adjuster := range c.adjusters {
        adjuster.Adjust(traces)
    }
}
```

`Sequence`는 여러 Adjuster를 순서대로 실행하는 컴포지트 패턴이다.
한 adjuster의 에러가 다음 adjuster의 실행을 중단시키지 않는다.

### 5.3 StandardAdjusters

```go
// 소스: cmd/jaeger/internal/extension/jaegerquery/internal/adjuster/standard.go

func StandardAdjusters(maxClockSkewAdjust time.Duration) []Adjuster {
    return []Adjuster{
        DeduplicateClientServerSpanIDs(),  // 1. Zipkin 스타일 스팬 ID 중복 제거
        SortCollections(),                  // 2. 컬렉션 정렬
        DeduplicateSpans(),                 // 3. 중복 스팬 제거 (SortCollections 이후)
        CorrectClockSkew(maxClockSkewAdjust), // 4. 클록 스큐 보정
        NormalizeIPAttributes(),            // 5. IP 속성 정규화
        MoveLibraryAttributes(),            // 6. 라이브러리 속성 이동
        RemoveEmptySpanLinks(),             // 7. 빈 스팬 링크 제거
    }
}
```

7개의 adjuster가 정해진 순서로 실행된다. 순서가 중요한 이유:
- `DeduplicateSpans`는 `SortCollections`가 먼저 실행되어야 정확히 동작한다
- `CorrectClockSkew`는 스팬 ID가 고유해야 하므로 `DeduplicateClientServerSpanIDs` 이후에 실행

### 5.4 DeduplicateClientServerSpanIDs (Zipkin 호환)

Zipkin 스타일의 클라이언트-서버 스팬은 동일한 Span ID를 공유한다.
Jaeger UI는 모든 스팬이 고유한 ID를 가져야 하므로, 서버 스팬에 새 ID를 부여한다.

```go
// 소스: cmd/jaeger/internal/extension/jaegerquery/internal/adjuster/spaniduniquifier.go

func DeduplicateClientServerSpanIDs() Adjuster {
    return Func(func(traces ptrace.Traces) {
        adjuster := spanIDDeduper{
            spansByID: make(map[pcommon.SpanID][]ptrace.Span),
            maxUsedID: pcommon.NewSpanIDEmpty(),
        }
        adjuster.adjust(traces)
    })
}
```

동작 과정:

```
1. groupSpansByID: 스팬을 ID별로 그룹화
   SpanID=A → [clientSpan, serverSpan]

2. uniquifyServerSpanIDs: 서버 스팬에 새 ID 부여
   - 같은 ID를 공유하는 client+server 쌍 발견
   - 서버 스팬에 새 고유 ID 할당 (incrementSpanID)
   - 서버 스팬의 ParentSpanID를 이전 공유 ID로 설정

   Before:                    After:
   clientSpan(id=A)           clientSpan(id=A)
   serverSpan(id=A)           serverSpan(id=B, parentId=A)

3. swapParentIDs: 자식 스팬들의 ParentSpanID 업데이트
   - 이전 ID를 부모로 참조하던 스팬들의 참조를 새 ID로 변경
```

```go
func (d *spanIDDeduper) uniquifyServerSpanIDs(traces ptrace.Traces) {
    oldToNewSpanIDs := make(map[pcommon.SpanID]pcommon.SpanID)
    // ... 모든 스팬을 순회하면서
    if span.Kind() == ptrace.SpanKindServer &&
       d.isSharedWithClientSpan(span.SpanID()) {
        newID, err := d.makeUniqueSpanID()
        // ...
        oldToNewSpanIDs[span.SpanID()] = newID
        span.SetParentSpanID(span.SpanID())  // 이전 공유 ID가 부모
        span.SetSpanID(newID)
    }
    d.swapParentIDs(traces, oldToNewSpanIDs)
}
```

### 5.5 CorrectClockSkew (클록 스큐 보정)

분산 시스템에서 호스트 간 시계 차이로 인해 자식 스팬이 부모보다 먼저 시작되거나
부모보다 늦게 끝나는 문제가 발생한다. 이 adjuster가 이를 보정한다.

```go
// 소스: cmd/jaeger/internal/extension/jaegerquery/internal/adjuster/clockskew.go

func CorrectClockSkew(maxDelta time.Duration) Adjuster {
    return Func(func(traces ptrace.Traces) {
        adjuster := &clockSkewAdjuster{
            traces:   traces,
            maxDelta: maxDelta,
        }
        adjuster.buildNodesMap()     // 스팬 ID -> 노드 매핑
        adjuster.buildSubGraphs()    // 부모-자식 트리 구성
        for _, root := range adjuster.roots {
            skew := clockSkew{hostKey: root.hostKey}
            adjuster.adjustNode(root, nil, skew)  // 트리 순회하며 보정
        }
    })
}
```

**호스트 식별**:

```go
func hostKey(resource ptrace.ResourceSpans) string {
    // 우선순위: host.id > host.ip > host.name
    if attr, ok := resource.Resource().Attributes().Get(
        string(otelsemconv.HostIDKey)); ok {
        return attr.Str()
    }
    if attr, ok := resource.Resource().Attributes().Get(
        string(otelsemconv.HostIPKey)); ok {
        // str 또는 slice 타입 처리
    }
    if attr, ok := resource.Resource().Attributes().Get(
        string(otelsemconv.HostNameKey)); ok {
        return attr.Str()
    }
    return ""
}
```

같은 hostKey를 가진 스팬은 같은 호스트에서 생성된 것으로 간주하여
클록 스큐 보정을 적용하지 않는다.

**스큐 계산 알고리즘**:

```go
func (*clockSkewAdjuster) calculateSkew(child *node, parent *node) time.Duration {
    parentStartTime := parent.span.StartTimestamp().AsTime()
    childStartTime := child.span.StartTimestamp().AsTime()
    parentEndTime := parent.span.EndTimestamp().AsTime()
    childEndTime := child.span.EndTimestamp().AsTime()
    parentDuration := parentEndTime.Sub(parentStartTime)
    childDuration := childEndTime.Sub(childStartTime)

    if childDuration > parentDuration {
        // 자식이 부모보다 오래 걸린 경우 (비동기 또는 타임아웃)
        // 자식이 부모 시작 전에 시작하지 않도록만 보정
        if childStartTime.Before(parentStartTime) {
            return parentStartTime.Sub(childStartTime)
        }
        return 0
    }
    if !childStartTime.Before(parentStartTime) &&
       !childEndTime.After(parentEndTime) {
        // 자식이 이미 부모 범위 내에 있으면 보정 불필요
        return 0
    }
    // 네트워크 지연을 요청/응답 균등 분배로 가정
    latency := (parentDuration - childDuration) / 2
    return parentStartTime.Add(latency).Sub(childStartTime)
}
```

시각화:

```
보정 전:
Parent: |=========================|
Child:       |==============|           (네트워크 지연 고려)

보정 계산:
latency = (parentDuration - childDuration) / 2
delta = parentStart + latency - childStart

보정 후:
Parent: |=========================|
Child:     |==============|             (부모 범위 안으로 조정)
```

**보정 제한**:

```go
func (a *clockSkewAdjuster) adjustTimestamps(n *node, skew clockSkew) {
    if skew.delta == 0 {
        return
    }
    if absDuration(skew.delta) > a.maxDelta {
        if a.maxDelta == 0 {
            // maxDelta=0이면 보정 비활성화
            jptrace.AddWarnings(n.span,
                fmt.Sprintf(warningSkewAdjustDisabled, skew.delta))
            return
        }
        // 최대 허용 delta 초과 시 경고만 남기고 적용하지 않음
        jptrace.AddWarnings(n.span,
            fmt.Sprintf(warningMaxDeltaExceeded, a.maxDelta, skew.delta))
        return
    }
    // 스팬 시작 시각과 이벤트 타임스탬프 모두 보정
    n.span.SetStartTimestamp(pcommon.NewTimestampFromTime(
        n.span.StartTimestamp().AsTime().Add(skew.delta)))
    for i := 0; i < n.span.Events().Len(); i++ {
        event := n.span.Events().At(i)
        event.SetTimestamp(pcommon.NewTimestampFromTime(
            event.Timestamp().AsTime().Add(skew.delta)))
    }
}
```

---

## 6. HTTP 서버와 API v2

### 6.1 서버 생성

```go
// 소스: cmd/jaeger/internal/extension/jaegerquery/internal/server.go

type Server struct {
    queryOptions *QueryOptions
    grpcConn     net.Listener
    httpConn     net.Listener
    grpcServer   *grpc.Server
    httpServer   *httpServer
    bgFinished   sync.WaitGroup
    telset       telemetry.Settings
}
```

`Server`는 HTTP와 gRPC를 동시에 서빙한다. `bgFinished`는 Go 1.22+의 `sync.WaitGroup.Go()`를 사용하여 백그라운드 고루틴을 관리한다:

```go
func (s *Server) Start(ctx context.Context) error {
    err := s.initListener(ctx)
    // ...
    s.bgFinished.Go(func() {
        // HTTP 서버 시작
        err := s.httpServer.Serve(s.httpConn)
        // ...
    })
    s.bgFinished.Go(func() {
        // gRPC 서버 시작
        err := s.grpcServer.Serve(s.grpcConn)
        // ...
    })
    return nil
}
```

### 6.2 HTTP 라우터 초기화 (initRouter)

`initRouter`는 HTTP 핸들러 체인의 핵심 구성 함수이다:

```go
func initRouter(
    querySvc *querysvc.QueryService,
    metricsQuerySvc metricstore.Reader,
    queryOpts *QueryOptions,
    tenancyMgr *tenancy.Manager,
    telset telemetry.Settings,
) (http.Handler, io.Closer) {
    // 1. API v2 핸들러 생성
    apiHandler := NewAPIHandler(querySvc, apiHandlerOptions...)

    // 2. ServeMux 생성
    r := http.NewServeMux()

    // 3. API v3 라우트 등록 (먼저)
    (&apiv3.HTTPGateway{
        QueryService: querySvc,
        Logger:       telset.Logger,
        Tracer:       telset.TracerProvider,
        BasePath:     queryOpts.BasePath,
    }).RegisterRoutes(r)

    // 4. API v2 라우트 등록
    apiHandler.RegisterRoutes(r)

    // 5. API 404 핸들러 (정적 핸들러보다 먼저)
    r.HandleFunc(apiNotFoundPattern, func(w http.ResponseWriter, _ *http.Request) {
        http.Error(w, "404 page not found", http.StatusNotFound)
    })

    // 6. 정적 자산 핸들러 (SPA 라우팅 포함)
    staticHandlerCloser := RegisterStaticHandler(
        r, telset.Logger, queryOpts, querySvc.GetCapabilities())

    // 7. 미들웨어 스택
    var handler http.Handler = r
    if queryOpts.BearerTokenPropagation {
        handler = bearertoken.PropagationHandler(telset.Logger, handler)
    }
    if tenancyMgr.Enabled {
        handler = tenancy.ExtractTenantHTTPHandler(tenancyMgr, handler)
    }
    handler = traceResponseHandler(handler)

    return handler, staticHandlerCloser
}
```

**라우트 등록 순서가 중요한 이유**:
1. API v3 라우트가 먼저 등록된다 (더 구체적인 경로)
2. API v2 라우트가 그 다음
3. `/api/` 패턴의 404 핸들러가 존재하지 않는 API 경로 처리
4. 정적 핸들러가 최후 폴백 (SPA 라우팅으로 index.html 반환)

### 6.3 미들웨어 스택

```
요청 들어옴
    │
    ▼
┌─────────────────────┐
│ traceResponseHandler│  ← Trace ID를 응답 헤더에 추가
├─────────────────────┤
│ tenancy.Extract...  │  ← 멀티 테넌시 헤더 추출 (선택)
├─────────────────────┤
│ bearertoken.Prop... │  ← Bearer 토큰 전파 (선택)
├─────────────────────┤
│ recoveryHandler     │  ← 패닉 복구
├─────────────────────┤
│ otelhttp            │  ← OTel HTTP 계측 (static 경로 제외)
├─────────────────────┤
│     ServeMux        │  ← 라우팅
│  ┌───────────────┐  │
│  │ API v3 routes │  │
│  │ API v2 routes │  │
│  │ API 404       │  │
│  │ Static/SPA    │  │
│  └───────────────┘  │
└─────────────────────┘
```

### 6.4 API v2 라우트 정의

```go
// 소스: cmd/jaeger/internal/extension/jaegerquery/internal/http_handler.go

func (aH *APIHandler) RegisterRoutes(router *http.ServeMux) {
    // 트레이스 조회
    aH.handleFunc(router, aH.getTrace,    GET,  "/traces/{traceID}")
    aH.handleFunc(router, aH.archiveTrace,POST, "/archive/{traceID}")
    aH.handleFunc(router, aH.search,      GET,  "/traces")

    // 메타데이터
    aH.handleFunc(router, aH.getServices,   GET, "/services")
    aH.handleFunc(router, aH.getOperations, GET, "/operations")
    aH.handleFunc(router, aH.getOperationsLegacy, GET,
                  "/services/{service}/operations")

    // OTLP 변환
    aH.handleFunc(router, aH.transformOTLP, POST, "/transform")

    // 의존성
    aH.handleFunc(router, aH.dependencies,     GET, "/dependencies")
    aH.handleFunc(router, aH.deepDependencies, GET, "/deep-dependencies")

    // 메트릭
    aH.handleFunc(router, aH.latencies, GET, "/metrics/latencies")
    aH.handleFunc(router, aH.calls,     GET, "/metrics/calls")
    aH.handleFunc(router, aH.errors,    GET, "/metrics/errors")
    aH.handleFunc(router, aH.minStep,   GET, "/metrics/minstep")

    // 품질 메트릭
    aH.handleFunc(router, aH.getQualityMetrics, GET, "/quality-metrics")
}
```

전체 라우트 맵:

| HTTP 메서드 | 경로 | 핸들러 | 설명 |
|------------|------|--------|------|
| GET | /api/traces/{traceID} | getTrace | 단일 트레이스 조회 |
| POST | /api/archive/{traceID} | archiveTrace | 트레이스 아카이브 |
| GET | /api/traces | search | 트레이스 검색 |
| GET | /api/services | getServices | 서비스 목록 |
| GET | /api/operations | getOperations | 오퍼레이션 목록 |
| GET | /api/services/{service}/operations | getOperationsLegacy | 레거시 오퍼레이션 |
| POST | /api/transform | transformOTLP | OTLP -> Jaeger 변환 |
| GET | /api/dependencies | dependencies | 서비스 의존성 |
| GET | /api/deep-dependencies | deepDependencies | 심층 의존성 |
| GET | /api/metrics/latencies | latencies | 레이턴시 메트릭 |
| GET | /api/metrics/calls | calls | 호출 수 메트릭 |
| GET | /api/metrics/errors | errors | 에러율 메트릭 |
| GET | /api/metrics/minstep | minStep | 최소 스텝 |
| GET | /api/quality-metrics | getQualityMetrics | 품질 메트릭 |

### 6.5 응답 형식

API v2는 일관된 JSON 응답 구조를 사용한다:

```go
type structuredResponse struct {
    Data   any               `json:"data"`
    Total  int               `json:"total"`
    Limit  int               `json:"limit"`
    Offset int               `json:"offset"`
    Errors []structuredError `json:"errors"`
}

type structuredError struct {
    Code    int        `json:"code,omitempty"`
    Msg     string     `json:"msg"`
    TraceID ui.TraceID `json:"traceID,omitempty"`
}
```

예시 응답:
```json
{
  "data": [...],
  "total": 5,
  "limit": 0,
  "offset": 0,
  "errors": []
}
```

### 6.6 getTrace 핸들러 상세

```go
func (aH *APIHandler) getTrace(w http.ResponseWriter, r *http.Request) {
    query, ok := aH.parseGetTraceParameters(w, r)
    if !ok {
        return
    }
    getTracesIter := aH.queryService.GetTraces(r.Context(), query)
    traces, err := v1adapter.V1TracesFromSeq2(getTracesIter)
    if errors.Is(err, spanstore.ErrTraceNotFound) ||
       (err == nil && len(traces) == 0) {
        aH.handleError(w, spanstore.ErrTraceNotFound, http.StatusNotFound)
        return
    }
    // ...
    structuredRes := aH.tracesToResponse(traces, uiErrors)
    aH.writeJSON(w, r, structuredRes)
}
```

`v1adapter.V1TracesFromSeq2`는 v2의 `iter.Seq2[[]ptrace.Traces, error]`를
v1의 `[]*model.Trace`로 변환한다. API v2는 아직 Jaeger 모델 v1 형식으로 응답하기 때문이다.

### 6.7 search 핸들러

```go
func (aH *APIHandler) search(w http.ResponseWriter, r *http.Request) {
    tQuery, err := aH.queryParser.parseTraceQueryParams(r)
    // ...
    if len(tQuery.TraceIDs) > 0 {
        // TraceID 기반 조회
        tracesFromStorage, uiErrors, err = aH.tracesByIDs(r.Context(), tQuery)
    } else {
        // 조건 기반 검색
        queryParams := querysvc.TraceQueryParams{
            TraceQueryParams: tQuery.TraceQueryParams,
            RawTraces:        tQuery.RawTraces,
        }
        findTracesIter := aH.queryService.FindTraces(r.Context(), queryParams)
        tracesFromStorage, err = v1adapter.V1TracesFromSeq2(findTracesIter)
    }
    structuredRes := aH.tracesToResponse(tracesFromStorage, uiErrors)
    aH.writeJSON(w, r, structuredRes)
}
```

`/api/traces` 엔드포인트는 두 가지 모드를 지원한다:
1. `traceID` 파라미터가 있으면 해당 ID들의 트레이스를 직접 조회
2. 없으면 `service`, `operation`, `start`, `end` 등의 파라미터로 검색

---

## 7. 쿼리 파서 (Query Parser)

### 7.1 트레이스 쿼리 파라미터

```go
// 소스: cmd/jaeger/internal/extension/jaegerquery/internal/query_parser.go

type queryParser struct {
    traceQueryLookbackDuration time.Duration
    timeNow                    func() time.Time
}
```

### 7.2 parseTraceQueryParams

```go
func (p *queryParser) parseTraceQueryParams(r *http.Request) (
    *traceQueryParameters, error) {
    service := r.FormValue(serviceParam)       // 서비스 이름
    operation := r.FormValue(operationParam)   // 오퍼레이션 이름

    // 시간 파라미터 (마이크로초 단위)
    startTime, err := p.parseTime(r, startTimeParam, time.Microsecond)
    endTime, err := p.parseTime(r, endTimeParam, time.Microsecond)

    // 태그 필터
    tags, err := p.parseTags(r.Form[tagParam], r.Form[tagsParam])

    // 결과 제한 (기본값: 100)
    limit := defaultQueryLimit  // 100
    if limitParam != "" {
        limit = int(limitParsed)
    }

    // 기간 필터
    minDuration, err := parseDuration(r, minDurationParam, parser, 0)
    maxDuration, err := parseDuration(r, maxDurationParam, parser, 0)

    // Trace ID 직접 지정
    var traceIDs []model.TraceID
    for _, id := range r.Form[traceIDParam] {
        traceID, err := model.TraceIDFromString(id)
        traceIDs = append(traceIDs, traceID)
    }

    // raw 모드
    raw, err := parseBool(r, rawParam)

    traceQuery := &traceQueryParameters{
        TraceQueryParams: tracestore.TraceQueryParams{
            ServiceName:   service,
            OperationName: operation,
            StartTimeMin:  startTime,
            StartTimeMax:  endTime,
            Attributes:    convertTagsToAttributes(tags),
            SearchDepth:   limit,
            DurationMin:   minDuration,
            DurationMax:   maxDuration,
        },
        RawTraces: raw,
        TraceIDs:  traceIDs,
    }
    // 유효성 검증
    p.validateQuery(traceQuery)
    return traceQuery, nil
}
```

**쿼리 문법** (소스 주석에서 발췌):

```
query     ::= param | param '&' query
param     ::= service | operation | limit | start | end |
              minDuration | maxDuration | tag | tags
service   ::= 'service=' strValue
operation ::= 'operation=' strValue
limit     ::= 'limit=' intValue
start     ::= 'start=' intValue (unix microseconds)
end       ::= 'end=' intValue (unix microseconds)
minDuration ::= 'minDuration=' strValue ("1ms", "500us", "2s" 등)
maxDuration ::= 'maxDuration=' strValue
tag       ::= 'tag=' key ':' value
tags      ::= 'tags=' jsonMap
```

**시간 단위 설계 결정**:
- `start/end`: 마이크로초 (Zipkin 전통에서 유래, 스팬 레이턴시가 마이크로초 단위)
- `minDuration/maxDuration`: Go duration 문자열 ("1ms", "500us" 등)
- 메트릭 API: 밀리초 (Prometheus 최소 스텝이 1ms)

### 7.3 태그 파싱

```go
func (*queryParser) parseTags(simpleTags []string, jsonTags []string) (
    map[string]string, error) {
    retMe := make(map[string]string)
    // 단순 태그: "key:value" 형식
    for _, tag := range simpleTags {
        keyAndValue := strings.Split(tag, ":")
        if l := len(keyAndValue); l <= 1 {
            return nil, fmt.Errorf(
                "malformed 'tag' parameter, expecting key:value, received: %s", tag)
        }
        retMe[keyAndValue[0]] = strings.Join(keyAndValue[1:], ":")
    }
    // JSON 태그: {"key":"value"} 형식
    for _, tags := range jsonTags {
        var fromJSON map[string]string
        json.Unmarshal([]byte(tags), &fromJSON)
        maps.Copy(retMe, fromJSON)
    }
    return retMe, nil
}
```

두 가지 태그 파라미터 형식을 지원한다:
- `tag=http.status_code:200` - 단순 키:값 쌍
- `tags={"http.status_code":"200","error":"true"}` - JSON 맵

### 7.4 쿼리 유효성 검증

```go
func (*queryParser) validateQuery(traceQuery *traceQueryParameters) error {
    // TraceID가 없으면 service 파라미터 필수
    if len(traceQuery.TraceIDs) == 0 && traceQuery.ServiceName == "" {
        return errServiceParameterRequired
    }
    // maxDuration이 minDuration보다 커야 함
    if traceQuery.DurationMin != 0 && traceQuery.DurationMax != 0 {
        if traceQuery.DurationMax < traceQuery.DurationMin {
            return errMaxDurationGreaterThanMin
        }
    }
    return nil
}
```

---

## 8. API v3 (모던 API)

### 8.1 HTTP Gateway

API v3는 OTLP 네이티브 형식을 사용하며, API v2와 별도의 라우트를 가진다:

```go
// 소스: cmd/jaeger/internal/extension/jaegerquery/internal/apiv3/http_gateway.go

const (
    routeGetTrace      = "/api/v3/traces/{trace_id}"
    routeFindTraces    = "/api/v3/traces"
    routeGetServices   = "/api/v3/services"
    routeGetOperations = "/api/v3/operations"
)

type HTTPGateway struct {
    QueryService *querysvc.QueryService
    Logger       *zap.Logger
    Tracer       trace.TracerProvider
    BasePath     string
}

func (h *HTTPGateway) RegisterRoutes(router *http.ServeMux) {
    h.addRoute(router, h.getTrace,      routeGetTrace,      http.MethodGet)
    h.addRoute(router, h.findTraces,    routeFindTraces,    http.MethodGet)
    h.addRoute(router, h.getServices,   routeGetServices,   http.MethodGet)
    h.addRoute(router, h.getOperations, routeGetOperations, http.MethodGet)
}
```

API v3 라우트:

| HTTP 메서드 | 경로 | 설명 |
|------------|------|------|
| GET | /api/v3/traces/{trace_id} | Trace ID로 조회 |
| GET | /api/v3/traces | 조건 기반 검색 |
| GET | /api/v3/services | 서비스 목록 |
| GET | /api/v3/operations | 오퍼레이션 목록 |

### 8.2 API v3 vs v2 차이점

| 특성 | API v2 | API v3 |
|------|--------|--------|
| 데이터 형식 | Jaeger 모델 v1 (JSON) | OTLP (protobuf JSON) |
| 시간 파라미터 | 마이크로초 (Unix epoch) | RFC3339Nano 문자열 |
| 검색 쿼리 | `service`, `operation` | `query.service_name`, `query.operation_name` |
| 응답 래퍼 | structuredResponse | GRPCGatewayWrapper |
| 검색 제한 | `limit` | `query.num_traces` |
| 시간 범위 | `start`, `end` | `query.start_time_min`, `query.start_time_max` |

### 8.3 getTrace (v3)

```go
func (h *HTTPGateway) getTrace(w http.ResponseWriter, r *http.Request) {
    traceIDVar := r.PathValue(paramTraceID)
    traceID, err := model.TraceIDFromString(traceIDVar)
    // ...
    request := querysvc.GetTraceParams{
        TraceIDs: []tracestore.GetTraceParams{
            {TraceID: v1adapter.FromV1TraceID(traceID)},
        },
    }
    // RFC3339Nano 형식 시간 파라미터 파싱
    startTime := http_query.Get(paramStartTime)
    if startTime != "" {
        timeParsed, err := time.Parse(time.RFC3339Nano, startTime)
        request.TraceIDs[0].Start = timeParsed.UTC()
    }
    // ...
    getTracesIter := h.QueryService.GetTraces(r.Context(), request)
    trc, err := jiter.FlattenWithErrors(getTracesIter)
    h.returnTraces(trc, err, w)
}
```

### 8.4 findTraces (v3) 쿼리 파싱

```go
func (h *HTTPGateway) parseFindTracesQuery(q url.Values, w http.ResponseWriter) (
    *querysvc.TraceQueryParams, bool) {
    queryParams := &querysvc.TraceQueryParams{
        TraceQueryParams: tracestore.TraceQueryParams{
            ServiceName:   q.Get(paramServiceName),   // query.service_name
            OperationName: q.Get(paramOperationName), // query.operation_name
            Attributes:    pcommon.NewMap(),
        },
    }
    // start_time_min, start_time_max 필수
    timeMin := q.Get(paramTimeMin)
    timeMax := q.Get(paramTimeMax)
    if timeMin == "" || timeMax == "" {
        err := fmt.Errorf("%s and %s are required", paramTimeMin, paramTimeMax)
        h.tryHandleError(w, err, http.StatusBadRequest)
        return nil, true
    }
    // RFC3339Nano 형식 파싱
    timeMinParsed, _ := time.Parse(time.RFC3339Nano, timeMin)
    timeMaxParsed, _ := time.Parse(time.RFC3339Nano, timeMax)
    queryParams.StartTimeMin = timeMinParsed
    queryParams.StartTimeMax = timeMaxParsed
    // ...
    return queryParams, false
}
```

### 8.5 응답 형식 (v3)

API v3는 OTLP protobuf JSON 형식으로 응답한다:

```go
func (h *HTTPGateway) returnTraces(traces []ptrace.Traces, err error,
    w http.ResponseWriter) {
    // ...
    // 여러 트레이스를 하나로 합침
    combinedTrace := ptrace.NewTraces()
    for _, t := range traces {
        resources := t.ResourceSpans()
        for i := 0; i < resources.Len(); i++ {
            resource := resources.At(i)
            resource.CopyTo(combinedTrace.ResourceSpans().AppendEmpty())
        }
    }
    h.returnTrace(combinedTrace, w)
}

func (h *HTTPGateway) returnTrace(td ptrace.Traces, w http.ResponseWriter) {
    tracesData := jptrace.TracesData(td)
    response := &api_v3.GRPCGatewayWrapper{
        Result: &tracesData,
    }
    h.marshalResponse(response, w)
}
```

### 8.6 에러 응답 (v3)

```go
func (h *HTTPGateway) tryHandleError(w http.ResponseWriter, err error,
    statusCode int) bool {
    if err == nil {
        return false
    }
    if errors.Is(err, spanstore.ErrTraceNotFound) {
        statusCode = http.StatusNotFound
    }
    errorResponse := api_v3.GRPCGatewayError{
        Error: &api_v3.GRPCGatewayError_GRPCGatewayErrorDetails{
            HttpCode: int32(statusCode),
            Message:  err.Error(),
        },
    }
    resp, _ := json.Marshal(&errorResponse)
    http.Error(w, string(resp), statusCode)
    return true
}
```

---

## 9. gRPC 서비스

### 9.1 gRPC 서버 생성

```go
// 소스: cmd/jaeger/internal/extension/jaegerquery/internal/server.go

func createGRPCServer(
    ctx context.Context,
    options *QueryOptions,
    tm *tenancy.Manager,
    telset telemetry.Settings,
) (*grpc.Server, error) {
    // Unary 인터셉터
    unaryInterceptors := []grpc.UnaryServerInterceptor{
        bearertoken.NewUnaryServerInterceptor(),
    }
    // Stream 인터셉터
    streamInterceptors := []grpc.StreamServerInterceptor{
        bearertoken.NewStreamServerInterceptor(),
    }
    // 테넌시 인터셉터 (선택적)
    if tm.Enabled {
        unaryInterceptors = append(unaryInterceptors,
            tenancy.NewGuardingUnaryInterceptor(tm))
        streamInterceptors = append(streamInterceptors,
            tenancy.NewGuardingStreamInterceptor(tm))
    }
    // ...
    return options.GRPC.ToServer(ctx, extensions, telSettings, grpcOpts...)
}
```

### 9.2 gRPC 핸들러 등록

```go
func registerGRPCHandlers(
    server *grpc.Server,
    querySvc *querysvc.QueryService,
    telset telemetry.Settings,
) {
    reflection.Register(server)  // gRPC 리플렉션 서비스 활성화

    // API v2 핸들러
    handler := NewGRPCHandler(querySvc, GRPCHandlerOptions{Logger: telset.Logger})
    api_v2.RegisterQueryServiceServer(server, handler)

    // API v3 핸들러
    api_v3.RegisterQueryServiceServer(server,
        &apiv3.Handler{QueryService: querySvc})

    // 헬스 체크 서비스
    healthServer := health.NewServer()
    healthServer.SetServingStatus("jaeger.api_v2.QueryService",
        grpc_health_v1.HealthCheckResponse_SERVING)
    healthServer.SetServingStatus("jaeger.api_v2.metrics.MetricsQueryService",
        grpc_health_v1.HealthCheckResponse_SERVING)
    healthServer.SetServingStatus("jaeger.api_v3.QueryService",
        grpc_health_v1.HealthCheckResponse_SERVING)
    grpc_health_v1.RegisterHealthServer(server, healthServer)
}
```

gRPC 서버에 등록되는 서비스:
1. **api_v2.QueryService**: v2 gRPC API (Jaeger 모델 기반)
2. **api_v3.QueryService**: v3 gRPC API (OTLP 기반)
3. **grpc.health.v1.Health**: 헬스 체크
4. **grpc.reflection**: 리플렉션 (grpcurl 등 디버깅 도구 지원)

### 9.3 API v2 gRPC Handler

```go
// 소스: cmd/jaeger/internal/extension/jaegerquery/internal/grpc_handler.go

const maxSpanCountInChunk = 10

type GRPCHandler struct {
    queryService *querysvc.QueryService
    logger       *zap.Logger
    nowFn        func() time.Time
}
```

**GetTrace (v2 gRPC)**:

```go
func (g *GRPCHandler) GetTrace(r *api_v2.GetTraceRequest,
    stream api_v2.QueryService_GetTraceServer) error {
    if r == nil {
        return errNilRequest
    }
    if r.TraceID == (model.TraceID{}) {
        return errUninitializedTraceID
    }
    query := querysvc.GetTraceParams{
        TraceIDs: []tracestore.GetTraceParams{
            {
                TraceID: v1adapter.FromV1TraceID(r.TraceID),
                Start:   r.StartTime,
                End:     r.EndTime,
            },
        },
        RawTraces: r.RawTraces,
    }
    getTracesIter := g.queryService.GetTraces(stream.Context(), query)
    traces, err := v1adapter.V1TracesFromSeq2(getTracesIter)
    // ...
    for _, trace := range traces {
        if err := g.sendSpanChunks(trace.Spans, stream.Send); err != nil {
            return err
        }
    }
    return nil
}
```

**청크 전송**: v2 gRPC는 스팬을 10개 단위의 청크로 스트리밍한다:

```go
func (g *GRPCHandler) sendSpanChunks(spans []*model.Span,
    sendFn func(*api_v2.SpansResponseChunk) error) error {
    chunk := make([]model.Span, 0, len(spans))
    for i := 0; i < len(spans); i += maxSpanCountInChunk {
        chunk = chunk[:0]
        for j := i; j < len(spans) && j < i+maxSpanCountInChunk; j++ {
            chunk = append(chunk, *spans[j])
        }
        if err := sendFn(&api_v2.SpansResponseChunk{Spans: chunk}); err != nil {
            return err
        }
    }
    return nil
}
```

### 9.4 API v3 gRPC Handler

```go
// 소스: cmd/jaeger/internal/extension/jaegerquery/internal/apiv3/grpc_handler.go

type Handler struct {
    QueryService *querysvc.QueryService
}

func (h *Handler) GetTrace(request *api_v3.GetTraceRequest,
    stream api_v3.QueryService_GetTraceServer) error {
    traceID, err := model.TraceIDFromString(request.GetTraceId())
    // ...
    query := querysvc.GetTraceParams{
        TraceIDs: []tracestore.GetTraceParams{
            {
                TraceID:   v1adapter.FromV1TraceID(traceID),
                Start:     request.GetStartTime(),
                End:       request.GetEndTime(),
            },
        },
        RawTraces: request.GetRawTraces(),
    }
    getTracesIter := h.QueryService.GetTraces(stream.Context(), query)
    return receiveTraces(getTracesIter, stream.Send)
}
```

v3 gRPC는 `ptrace.Traces`를 `TracesData`로 래핑하여 스트리밍한다:

```go
func receiveTraces(
    seq iter.Seq2[[]ptrace.Traces, error],
    sendFn func(*jptrace.TracesData) error,
) error {
    for traces, err := range seq {
        if err != nil {
            return err
        }
        for _, trace := range traces {
            tracesData := jptrace.TracesData(trace)
            if err := sendFn(&tracesData); err != nil {
                return status.Error(codes.Internal,
                    fmt.Sprintf("failed to send response stream chunk: %v", err))
            }
        }
    }
    return nil
}
```

### 9.5 gRPC 인터셉터 스택

```
gRPC 요청 들어옴
    │
    ▼
┌──────────────────────────┐
│ bearertoken interceptor  │  ← 인증 토큰 컨텍스트에 추가
├──────────────────────────┤
│ tenancy interceptor      │  ← 테넌트 식별 (선택적)
├──────────────────────────┤
│    gRPC Server Handler   │
│  ┌────────────────────┐  │
│  │ api_v2.QueryService│  │
│  │ api_v3.QueryService│  │
│  │ health.v1.Health   │  │
│  └────────────────────┘  │
└──────────────────────────┘
```

---

## 10. 정적 자산 핸들러 (Static Handler)

### 10.1 StaticAssetsHandler 구조체

```go
// 소스: cmd/jaeger/internal/extension/jaegerquery/internal/static_handler.go

type StaticAssetsHandler struct {
    options   StaticAssetsHandlerOptions
    indexHTML atomic.Value   // stores []byte
    assetsFS  http.FileSystem
    watcher   *fswatcher.FSWatcher
}
```

- `indexHTML`: `atomic.Value`로 보관하여 동시 접근에 안전하게 hot-reload 가능
- `assetsFS`: 임베디드 자산 또는 커스텀 디렉토리의 파일 시스템
- `watcher`: UI 설정 파일 변경 감시

### 10.2 자산 소스 결정

```go
func NewStaticAssetsHandler(staticAssetsRoot string,
    options StaticAssetsHandlerOptions) (*StaticAssetsHandler, error) {
    // 기본: 임베디드 UI 자산 사용
    assetsFS := ui.GetStaticFiles(options.Logger)
    // AssetsPath가 지정되면 외부 디렉토리 사용
    if staticAssetsRoot != "" {
        assetsFS = http.Dir(staticAssetsRoot)
    }
    // ...
}
```

### 10.3 index.html 보강 (loadAndEnrichIndexHTML)

정적 핸들러의 핵심 기능은 `index.html`에 런타임 설정을 주입하는 것이다:

```go
func (sH *StaticAssetsHandler) loadAndEnrichIndexHTML(
    open func(string) (http.File, error)) ([]byte, error) {
    indexBytes, err := loadIndexHTML(open)
    // ...

    // 1. UI 설정 교체 (.json 또는 .js)
    if configObject, err := loadUIConfig(sH.options.ConfigFile); err != nil {
        return nil, err
    } else if configObject != nil {
        indexBytes = configObject.regexp.ReplaceAll(indexBytes, configObject.config)
    }

    // 2. 스토리지 기능 주입
    capabilitiesJSON, _ := json.Marshal(sH.options.StorageCapabilities)
    capabilitiesString := fmt.Sprintf(
        "JAEGER_STORAGE_CAPABILITIES = %s;", string(capabilitiesJSON))
    indexBytes = compabilityPattern.ReplaceAll(indexBytes,
        []byte(capabilitiesString))

    // 3. Jaeger 버전 주입
    versionJSON, _ := json.Marshal(version.Get())
    versionString := fmt.Sprintf("JAEGER_VERSION = %s;", string(versionJSON))
    indexBytes = versionPattern.ReplaceAll(indexBytes, []byte(versionString))

    // 4. Base path 교체
    if sH.options.BasePath != "/" {
        indexBytes = basePathPattern.ReplaceAll(indexBytes,
            fmt.Appendf(nil, `<base href="%s/"`, sH.options.BasePath))
    }

    return indexBytes, nil
}
```

**index.html에 주입되는 항목**:

| 패턴 | 교체 내용 | 용도 |
|------|----------|------|
| `JAEGER_CONFIG = DEFAULT_CONFIG;` | 사용자 정의 UI 설정 | UI 커스터마이징 |
| `JAEGER_VERSION = DEFAULT_VERSION;` | 실제 Jaeger 버전 정보 | 버전 표시 |
| `JAEGER_STORAGE_CAPABILITIES = DEFAULT_STORAGE_CAPABILITIES;` | 아카이브 지원 여부 등 | UI 기능 토글 |
| `<base href="/"` | 커스텀 base path | 서브 경로 배포 |

### 10.4 UI 설정 파일 형식

```go
func loadUIConfig(uiConfig string) (*loadedConfig, error) {
    // ...
    ext := filepath.Ext(uiConfig)
    switch strings.ToLower(ext) {
    case ".json":
        // JSON 형식: JAEGER_CONFIG = {...};
        var c map[string]any
        json.Unmarshal(bytesConfig, &c)
        r, _ = json.Marshal(c)
        return &loadedConfig{
            regexp: configPattern,
            config: append([]byte("JAEGER_CONFIG = "), append(r, byte(';'))...),
        }, nil
    case ".js":
        // JS 형식: function UIConfig() { ... }
        re := regexp.MustCompile(`function\s+UIConfig(\s)?\(\s?\)(\s)?{`)
        if !re.Match(r) {
            return nil, fmt.Errorf(
                "UI config file must define function UIConfig(): %v", uiConfig)
        }
        return &loadedConfig{
            regexp: configJsPattern,
            config: r,
        }, nil
    default:
        return nil, fmt.Errorf(
            "unrecognized UI config file format: %v", uiConfig)
    }
}
```

두 가지 설정 형식:
- **JSON** (.json): `{"dependencies":{"menuEnabled":true},"archiveEnabled":true}`
- **JavaScript** (.js): `function UIConfig() { return { ... }; }`

### 10.5 Hot-reload (파일 감시)

```go
func NewStaticAssetsHandler(...) (*StaticAssetsHandler, error) {
    // ...
    watcher, err := fswatcher.New(
        []string{options.ConfigFile},
        h.reloadUIConfig,
        h.options.Logger,
    )
    h.watcher = watcher
    h.indexHTML.Store(indexHTML)
    return h, nil
}

func (sH *StaticAssetsHandler) reloadUIConfig() {
    sH.options.Logger.Info("reloading UI config",
        zap.String("filename", sH.options.ConfigFile))
    content, err := sH.loadAndEnrichIndexHTML(sH.assetsFS.Open)
    if err != nil {
        sH.options.Logger.Error("error while reloading the UI config",
            zap.Error(err))
    }
    sH.indexHTML.Store(content)
    sH.options.Logger.Info("reloaded UI config",
        zap.String("filename", sH.options.ConfigFile))
}
```

`fswatcher.FSWatcher`가 설정 파일 변경을 감지하면 `reloadUIConfig`을 호출하여
`index.html`을 다시 생성하고 `atomic.Value`에 저장한다. 서버 재시작 없이 UI 설정을 변경할 수 있다.

### 10.6 SPA 라우팅

```go
func (sH *StaticAssetsHandler) RegisterRoutes(router *http.ServeMux) {
    basePath := sH.options.BasePath
    if basePath == "" {
        basePath = "/"
    }

    // 정적 파일 핸들러 (JS, CSS, 이미지 등)
    fileServer := http.FileServer(sH.assetsFS)
    if basePath != "/" {
        fileServer = http.StripPrefix(basePath+"/", fileServer)
    }
    router.Handle(staticPattern, sH.loggingHandler(fileServer))

    // SPA catch-all: 모든 비-API 라우트에 index.html 반환
    router.Handle(catchAllPattern,
        sH.loggingHandler(http.HandlerFunc(sH.notFound)))
}

func (sH *StaticAssetsHandler) notFound(w http.ResponseWriter, _ *http.Request) {
    w.Header().Set("Content-Type", "text/html; charset=utf-8")
    w.Write(sH.indexHTML.Load().([]byte))
}
```

SPA(Single Page Application) 라우팅 동작:
1. `/static/*` 경로는 파일 서버가 처리 (JS, CSS, 이미지 등)
2. `/api/*` 경로는 API 핸들러가 처리 (매칭되지 않으면 404 반환)
3. 그 외 모든 경로(`/`)는 `index.html` 반환 (SPA catch-all)
4. React Router가 클라이언트 측에서 경로를 해석

```
요청: /trace/abc123
    │
    ▼
ServeMux 라우팅:
  /api/v3/* → API v3 핸들러? NO
  /api/*    → API v2 핸들러? NO
  /api/     → API 404?       NO (패턴 불일치)
  /static/  → 파일 서버?     NO
  /         → catch-all      YES → index.html 반환
                                     │
                                     ▼
                              React Router 처리:
                              /trace/abc123 → TraceView 컴포넌트
```

---

## 11. 서버 종료 (Graceful Shutdown)

```go
// 소스: cmd/jaeger/internal/extension/jaegerquery/server.go

func (s *server) Shutdown(ctx context.Context) error {
    var errs []error
    if s.server != nil {
        errs = append(errs, s.server.Close())
    }
    if s.closeTracer != nil {
        errs = append(errs, s.closeTracer(ctx))
    }
    return errors.Join(errs...)
}
```

```go
// 소스: cmd/jaeger/internal/extension/jaegerquery/internal/server.go

func (s *Server) Close() error {
    var errs []error
    s.telset.Logger.Info("Closing HTTP server")
    errs = append(errs, s.httpServer.Close())  // HTTP 서버 종료
    s.telset.Logger.Info("Stopping gRPC server")
    s.grpcServer.Stop()                         // gRPC 서버 종료
    s.bgFinished.Wait()                         // 백그라운드 고루틴 대기
    s.telset.Logger.Info("Server stopped")
    return errors.Join(errs...)
}
```

종료 순서:
1. HTTP 서버 종료 (정적 핸들러의 파일 감시자 포함)
2. gRPC 서버 종료
3. 백그라운드 고루틴 완료 대기
4. TracerProvider 종료 (활성화된 경우)

---

## 12. 메트릭 서비스

### 12.1 메트릭 쿼리 서비스

메트릭 API는 Jaeger SPM(Service Performance Monitoring) 기능을 제공한다.
Prometheus 호환 백엔드에서 서비스별 레이턴시, 호출 수, 에러율을 조회한다.

```go
// 소스: cmd/jaeger/internal/extension/jaegerquery/server.go

func (s *server) createMetricReader(host component.Host) (
    metricstore.Reader, error) {
    if s.config.Storage.Metrics == "" {
        s.telset.Logger.Info("Metric storage not configured")
        return disabled.NewMetricsReader()  // 비활성화된 Reader 반환
    }
    msf, err := jaegerstorage.GetMetricStorageFactory(
        s.config.Storage.Metrics, host)
    metricsReader, err := msf.CreateMetricsReader()
    return metricsReader, nil
}
```

메트릭 스토리지가 설정되지 않으면 `disabled.MetricsReader`가 사용되어
모든 메트릭 API 호출에 `ErrDisabled` (501 Not Implemented)를 반환한다.

### 12.2 메트릭 API 엔드포인트

```go
func (aH *APIHandler) latencies(w http.ResponseWriter, r *http.Request) {
    q, err := strconv.ParseFloat(r.FormValue(quantileParam), 64)
    // ...
    aH.metrics(w, r, func(ctx context.Context,
        baseParams metricstore.BaseQueryParameters) (*metrics.MetricFamily, error) {
        return aH.metricsQueryService.GetLatencies(ctx,
            &metricstore.LatenciesQueryParameters{
                BaseQueryParameters: baseParams,
                Quantile:            q,
            })
    })
}
```

메트릭 쿼리 파라미터:

```
query     ::= services & [ optionalParams ]
services  ::= service & [ services ]
service   ::= 'service=' strValue
endTs     ::= 'endTs=' intValue (unix milliseconds)
lookback  ::= 'lookback=' intValue (milliseconds)
step      ::= 'step=' intValue (milliseconds)
ratePer   ::= 'ratePer=' intValue (milliseconds)
spanKind  ::= 'spanKind=' "server" | "client" | "internal" | ...
```

---

## 13. OTel HTTP 계측

HTTP 서버는 OpenTelemetry 계측을 내장하되, 정적 자산 경로는 제외한다:

```go
// 소스: cmd/jaeger/internal/extension/jaegerquery/internal/server.go

hs, err := queryOpts.HTTP.ToServer(
    ctx, extensions, telSettings, handler,
    xconfighttp.WithOtelHTTPOptions(
        // 정적 자산 경로 필터링
        otelhttp.WithFilter(func(r *http.Request) bool {
            ignorePath := path.Join("/", queryOpts.BasePath, "static")
            return !strings.HasPrefix(r.URL.Path, ignorePath)
        }),
        // 스팬 이름 포매터: 메서드 접두사와 basePath 제거
        otelhttp.WithSpanNameFormatter(
            func(_ string, r *http.Request) string {
                pattern := r.Pattern
                if pattern != "" {
                    // "GET /jaeger/api/v3/traces/{trace_id}"
                    // → "/api/v3/traces/{trace_id}"
                    if idx := strings.Index(pattern, " "); idx > 0 {
                        pattern = pattern[idx+1:]
                    }
                    if queryOpts.BasePath != "" &&
                       queryOpts.BasePath != "/" {
                        pattern = strings.TrimPrefix(
                            pattern, queryOpts.BasePath)
                    }
                }
                return pattern
            },
        ),
    ),
)
```

이 설정으로:
- `/static/` 경로 요청은 트레이스를 생성하지 않는다 (노이즈 방지)
- API 요청 스팬 이름에서 HTTP 메서드와 basePath가 제거되어 깔끔한 이름이 된다

---

## 14. 포트 정의

```go
// 소스: ports/ports.go

const (
    QueryGRPC = 16685   // gRPC API
    QueryHTTP = 16686   // HTTP API + UI
    MCPHTTP   = 16687   // MCP (Model Context Protocol) 서버
)
```

| 포트 | 프로토콜 | 용도 |
|------|---------|------|
| 16685 | gRPC | Query gRPC API (v2 + v3) |
| 16686 | HTTP | UI + HTTP API (v2 + v3) |
| 16687 | HTTP | MCP 서버 |

---

## 15. 전체 데이터 흐름 요약

### 15.1 트레이스 조회 흐름 (HTTP API v2)

```
클라이언트 (Jaeger UI)
    │
    │ GET /api/traces/abc123
    │
    ▼
┌──────────────────────────────────────────────────┐
│                   HTTP Server (:16686)             │
│                                                    │
│  미들웨어 체인:                                    │
│  traceResponse → tenancy → bearertoken → recovery  │
│                                                    │
│  ┌──────────────────────────────────┐              │
│  │ APIHandler.getTrace()            │              │
│  │  1. parseTraceID("abc123")       │              │
│  │  2. queryService.GetTraces(...)  │              │
│  │                                  │              │
│  │  ┌────────────────────────────┐  │              │
│  │  │ QueryService.GetTraces()   │  │              │
│  │  │  1. traceReader.GetTraces  │  │              │
│  │  │  2. receiveTraces()        │  │              │
│  │  │     - Aggregate chunks     │  │              │
│  │  │     - Apply Adjusters:     │  │              │
│  │  │       DeduplicateIDs       │  │              │
│  │  │       SortCollections      │  │              │
│  │  │       DeduplicateSpans     │  │              │
│  │  │       CorrectClockSkew     │  │              │
│  │  │       NormalizeIP          │  │              │
│  │  │       MoveLibrary          │  │              │
│  │  │       RemoveEmptyLinks     │  │              │
│  │  │  3. Archive fallback       │  │              │
│  │  └────────────────────────────┘  │              │
│  │                                  │              │
│  │  3. v1adapter.V1TracesFromSeq2   │              │
│  │  4. tracesToResponse()           │              │
│  │  5. writeJSON()                  │              │
│  └──────────────────────────────────┘              │
└──────────────────────────────────────────────────┘
    │
    │ HTTP 200 OK
    │ {"data": [{"traceID":"abc123","spans":[...]}], ...}
    │
    ▼
클라이언트
```

### 15.2 검색 흐름 (gRPC API v3)

```
gRPC 클라이언트
    │
    │ FindTraces(query)
    │
    ▼
┌──────────────────────────────────────────────────┐
│                  gRPC Server (:16685)              │
│                                                    │
│  인터셉터: bearertoken → tenancy                   │
│                                                    │
│  ┌──────────────────────────────────┐              │
│  │ apiv3.Handler.FindTraces()      │              │
│  │  1. 쿼리 파라미터 파싱           │              │
│  │  2. queryService.FindTraces()   │              │
│  │  3. receiveTraces()             │              │
│  │     → stream.Send(TracesData)   │              │
│  └──────────────────────────────────┘              │
└──────────────────────────────────────────────────┘
    │
    │ stream: TracesData, TracesData, ...
    │
    ▼
gRPC 클라이언트
```

---

## 16. 설계 결정과 트레이드오프

### 16.1 Iterator 기반 스트리밍

Go 1.23의 `iter.Seq2`를 활용하여 트레이스 데이터를 이터레이터로 스트리밍한다.
이를 통해 대량의 트레이스를 메모리에 모두 로드하지 않고 점진적으로 처리할 수 있다.

```go
// GetTraces는 iter.Seq2[[]ptrace.Traces, error]를 반환
func (qs QueryService) GetTraces(
    ctx context.Context,
    params GetTraceParams,
) iter.Seq2[[]ptrace.Traces, error]
```

### 16.2 Adjuster의 In-place 수정

Adjuster는 `ptrace.Traces`를 복사하지 않고 in-place로 수정한다.
이는 메모리 할당을 줄이지만, 원본 데이터가 변경된다는 것을 의미한다.
`rawTraces=true`로 조회하면 adjuster를 건너뛸 수 있다.

### 16.3 동일 포트 vs 분리 포트

HTTP와 gRPC가 동일한 포트를 사용할 수 있지만, TLS가 활성화되면 분리된 포트를 사용해야 한다:

```go
if (options.HTTP.TLS.HasValue() || options.GRPC.TLS.HasValue()) &&
   !separatePorts {
    return nil, errors.New(
        "server with TLS enabled can not use same host ports for gRPC and HTTP")
}
```

### 16.4 API v2와 v3의 공존

두 API 버전이 동일한 QueryService를 공유하므로 동일한 데이터를 반환한다.
차이는 직렬화 형식과 파라미터 명명 규칙뿐이다.
API v2는 레거시 호환성을 위해 유지되며, 점진적으로 v3로 전환될 예정이다.

### 16.5 Archive 폴백 전략

`GetTraces`에서만 Archive 폴백이 동작하고 `FindTraces`에서는 동작하지 않는다.
이는 의도적인 설계 결정으로, 검색은 Primary 스토리지에서만 수행하고
특정 Trace ID 조회 시에만 Archive를 확인한다.

---

## 17. 주요 소스 파일 참조

| 파일 경로 | 역할 |
|-----------|------|
| `cmd/jaeger/internal/extension/jaegerquery/extension.go` | Extension 인터페이스 정의 |
| `cmd/jaeger/internal/extension/jaegerquery/config.go` | Config 구조체 (Storage 참조) |
| `cmd/jaeger/internal/extension/jaegerquery/server.go` | Extension 구현 (Start/Shutdown) |
| `cmd/jaeger/internal/extension/jaegerquery/querysvc/service.go` | QueryService 핵심 로직 |
| `cmd/jaeger/internal/extension/jaegerquery/internal/flags.go` | QueryOptions, UIConfig, 기본 포트 |
| `cmd/jaeger/internal/extension/jaegerquery/internal/server.go` | HTTP+gRPC Server 생성/시작 |
| `cmd/jaeger/internal/extension/jaegerquery/internal/http_handler.go` | API v2 HTTP 핸들러 |
| `cmd/jaeger/internal/extension/jaegerquery/internal/query_parser.go` | 쿼리 파라미터 파서 |
| `cmd/jaeger/internal/extension/jaegerquery/internal/static_handler.go` | 정적 자산 + SPA 라우팅 |
| `cmd/jaeger/internal/extension/jaegerquery/internal/grpc_handler.go` | API v2 gRPC 핸들러 |
| `cmd/jaeger/internal/extension/jaegerquery/internal/apiv3/http_gateway.go` | API v3 HTTP 게이트웨이 |
| `cmd/jaeger/internal/extension/jaegerquery/internal/apiv3/grpc_handler.go` | API v3 gRPC 핸들러 |
| `cmd/jaeger/internal/extension/jaegerquery/internal/adjuster/adjuster.go` | Adjuster 인터페이스, Sequence |
| `cmd/jaeger/internal/extension/jaegerquery/internal/adjuster/standard.go` | StandardAdjusters 7종 |
| `cmd/jaeger/internal/extension/jaegerquery/internal/adjuster/clockskew.go` | 클록 스큐 보정 알고리즘 |
| `cmd/jaeger/internal/extension/jaegerquery/internal/adjuster/spaniduniquifier.go` | Zipkin 스팬 ID 중복 제거 |
| `ports/ports.go` | 포트 상수 정의 (16685, 16686) |
