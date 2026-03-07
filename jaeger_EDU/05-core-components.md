# 05. Jaeger 핵심 컴포넌트

Jaeger v2는 OpenTelemetry Collector 위에 구축된 분산 트레이싱 시스템으로, **Extension**, **Exporter**, **Processor**라는 OTel Collector의 컴포넌트 모델을 활용하여 스토리지, 쿼리, 샘플링 등 핵심 기능을 구현한다. 이 문서에서는 각 핵심 컴포넌트의 내부 구조, 초기화 흐름, 데이터 처리 방식을 소스코드 기반으로 분석한다.

---

## 1. Storage Extension (jaeger_storage)

### 1.1 개요

Storage Extension은 Jaeger의 **모든 스토리지 백엔드 접근을 중앙 관리**하는 OTel Collector Extension이다. 다른 컴포넌트(Query Extension, Storage Exporter, Remote Sampling Extension 등)는 이 Extension을 통해 스토리지 팩토리를 획득한다.

**소스 위치**: `cmd/jaeger/internal/extension/jaegerstorage/extension.go`

### 1.2 Extension 인터페이스

```go
// extension.go:30-34
type Extension interface {
    extension.Extension
    TraceStorageFactory(name string) (tracestore.Factory, error)
    MetricStorageFactory(name string) (storage.MetricStoreFactory, error)
}
```

두 개의 메서드가 핵심이다:
- **TraceStorageFactory**: 트레이스 저장소 팩토리를 이름으로 반환
- **MetricStorageFactory**: 메트릭 저장소 팩토리를 이름으로 반환

### 1.3 내부 구조체

```go
// extension.go:36-42
type storageExt struct {
    config           *Config
    telset           telemetry.Settings
    factories        map[string]tracestore.Factory
    metricsFactories map[string]storage.MetricStoreFactory
    factoryMu        sync.Mutex
}
```

`factories`와 `metricsFactories`는 **캐시 역할**을 한다. 한번 생성된 팩토리는 map에 저장되어 동일한 이름으로 요청 시 재사용된다. `factoryMu`(sync.Mutex)로 동시성을 보장한다.

### 1.4 지연 초기화(Lazy Initialization) 패턴

팩토리 생성은 `Start()` 시점이 아닌 **최초 접근 시점**에 수행된다. `Start()`에서는 설정 검증만 한다:

```go
// extension.go:152-170
func (s *storageExt) Start(_ context.Context, host component.Host) error {
    s.telset.Host = host
    s.telset.Metrics = otelmetrics.NewFactory(s.telset.MeterProvider).
        Namespace(metrics.NSOptions{Name: "jaeger"})

    // 설정 검증만 수행 - 팩토리 생성은 아직 안 함
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

실제 팩토리 생성은 `TraceStorageFactory()` 호출 시 수행된다:

```go
// extension.go:192-226
func (s *storageExt) TraceStorageFactory(name string) (tracestore.Factory, error) {
    s.factoryMu.Lock()
    defer s.factoryMu.Unlock()

    // 캐시된 팩토리가 있으면 즉시 반환
    if f, ok := s.factories[name]; ok {
        return f, nil
    }

    // 설정 존재 여부 확인
    cfg, ok := s.config.TraceBackends[name]
    if !ok {
        return nil, fmt.Errorf("storage '%s' not declared ...", name, componentType)
    }

    // 최초 접근 시 팩토리 생성
    factory, err := storageconfig.CreateTraceStorageFactory(
        context.Background(), name, cfg, s.telset, ...)
    if err != nil {
        return nil, fmt.Errorf("failed to initialize storage '%s': %w", name, err)
    }

    s.factories[name] = factory  // 캐시에 저장
    return factory, nil
}
```

이 패턴의 장점:
- 사용되지 않는 백엔드의 초기화 비용 절감
- Extension 간 순환 의존성 회피 (host가 Start 시점에만 제공되므로)
- 실패 격리 - 하나의 백엔드 초기화 실패가 다른 백엔드에 영향 안 줌

### 1.5 설정 구조: TraceBackend

**소스 위치**: `cmd/internal/storageconfig/config.go`

```go
// config.go:31-45
type Config struct {
    TraceBackends  map[string]TraceBackend  `mapstructure:"backends"`
    MetricBackends map[string]MetricBackend `mapstructure:"metric_backends"`
}

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

**핵심 규칙**: 각 TraceBackend는 **정확히 하나의 타입**만 설정할 수 있다. `Validate()` 메서드가 이를 강제한다:

```go
// config.go:102-132
func (cfg *TraceBackend) Validate() error {
    var backends []string
    if cfg.Memory != nil { backends = append(backends, "memory") }
    if cfg.Badger != nil { backends = append(backends, "badger") }
    // ... 나머지 타입들도 동일하게 체크
    if len(backends) == 0 {
        return errors.New("empty configuration")
    }
    if len(backends) > 1 {
        return fmt.Errorf("multiple backend types found for trace storage: %v", backends)
    }
    return nil
}
```

### 1.6 Factory 디스패치

**소스 위치**: `cmd/internal/storageconfig/factory.go`

`CreateTraceStorageFactory` 함수가 백엔드 타입에 따라 적절한 팩토리를 생성한다:

```go
// factory.go:32-91
func CreateTraceStorageFactory(
    ctx context.Context,
    name string,
    backend TraceBackend,
    telset telemetry.Settings,
    authResolver AuthResolver,
) (tracestore.Factory, error) {
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
        // Elasticsearch/Opensearch는 인증 설정도 처리
        var httpAuth extensionauth.HTTPClient
        if authResolver != nil {
            httpAuth, err = authResolver(backend.Elasticsearch.Authentication, ...)
        }
        factory, err = es.NewFactory(ctx, *backend.Elasticsearch, telset, httpAuth)
    case backend.ClickHouse != nil:
        factory, err = clickhouse.NewFactory(ctx, *backend.ClickHouse, telset)
    default:
        err = errors.New("empty configuration")
    }
    return factory, nil
}
```

### 1.7 글로벌 헬퍼 함수

다른 컴포넌트가 스토리지에 접근할 때 사용하는 헬퍼 함수들이다:

```
+------------------------------+------------------------------------------+
| 함수                          | 용도                                      |
+------------------------------+------------------------------------------+
| GetTraceStoreFactory()       | 트레이스 저장소 팩토리 획득                    |
| GetSamplingStoreFactory()    | 샘플링 저장소 팩토리 획득 (type assertion)     |
| GetPurger()                  | 데이터 삭제 인터페이스 획득 (type assertion)   |
| GetMetricStorageFactory()    | 메트릭 저장소 팩토리 획득                     |
+------------------------------+------------------------------------------+
```

`GetSamplingStoreFactory()`와 `GetPurger()`는 **타입 단언(type assertion)**을 사용하여, 해당 기능을 지원하지 않는 백엔드에서 호출하면 에러를 반환한다:

```go
// extension.go:87-99
func GetSamplingStoreFactory(name string, host component.Host) (storage.SamplingStoreFactory, error) {
    f, err := getStorageFactory(name, host)
    if err != nil { return nil, err }

    ssf, ok := f.(storage.SamplingStoreFactory)
    if !ok {
        return nil, fmt.Errorf("storage '%s' does not support sampling store", name)
    }
    return ssf, nil
}
```

### 1.8 Shutdown 처리

```go
// extension.go:172-190
func (s *storageExt) Shutdown(context.Context) error {
    var errs []error
    for _, factory := range s.factories {
        if closer, ok := factory.(io.Closer); ok {
            err := closer.Close()
            if err != nil { errs = append(errs, err) }
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

`io.Closer` 인터페이스를 구현하는 팩토리만 닫는다. `errors.Join`으로 모든 에러를 수집하여 반환한다.

---

## 2. Query Extension (jaeger_query)

### 2.1 개요

Query Extension은 Jaeger UI와 API 클라이언트에게 **트레이스 조회 기능을 제공**하는 서버 컴포넌트이다. HTTP(포트 16686)와 gRPC(포트 16685) 서버를 동시에 운영한다.

**소스 위치**:
- 서버 진입점: `cmd/jaeger/internal/extension/jaegerquery/server.go`
- HTTP/gRPC 서버: `cmd/jaeger/internal/extension/jaegerquery/internal/server.go`
- Extension 인터페이스: `cmd/jaeger/internal/extension/jaegerquery/extension.go`

### 2.2 Extension 인터페이스 및 의존성

```go
// extension.go:17-21
type Extension interface {
    extension.Extension
    QueryService() *querysvc.QueryService
}
```

Query Extension은 `extensioncapabilities.Dependent` 인터페이스를 구현하여 **반드시 jaeger_storage Extension 이후에 시작**되도록 보장한다:

```go
// server.go:54-56
func (*server) Dependencies() []component.ID {
    return []component.ID{jaegerstorage.ID}
}
```

### 2.3 시작 흐름 (Start)

`Start()` 메서드는 다음 순서로 실행된다:

```
Start()
  |
  +-- (1) TracerProvider 초기화 (EnableTracing 옵션)
  |
  +-- (2) jaegerstorage.GetTraceStoreFactory() → traceReader 생성
  |
  +-- (3) depstore.Factory로 타입 단언 → dependencyReader 생성
  |
  +-- (4) Archive Storage 설정 (선택사항)
  |
  +-- (5) QueryService 생성
  |
  +-- (6) MetricReader 생성 (선택사항)
  |
  +-- (7) queryapp.NewServer() → HTTP + gRPC 서버 시작
```

핵심 코드:

```go
// server.go:58-139
func (s *server) Start(ctx context.Context, host component.Host) error {
    // (2) 트레이스 스토리지 팩토리 → 리더 생성
    tf, err := jaegerstorage.GetTraceStoreFactory(s.config.Storage.TracesPrimary, host)
    traceReader, err := tf.CreateTraceReader()

    // (3) 의존성 리더 생성 (같은 팩토리에서 타입 단언)
    df, ok := tf.(depstore.Factory)
    depReader, err := df.CreateDependencyReader()

    // (4) 아카이브 스토리지 (선택)
    opts := querysvc.QueryServiceOptions{
        MaxClockSkewAdjust: s.config.MaxClockSkewAdjust,
        MaxTraceSize:       s.config.MaxTraceSize,
    }
    s.addArchiveStorage(&opts, host)

    // (5) QueryService 생성
    qs := querysvc.NewQueryService(traceReader, depReader, opts)

    // (7) HTTP + gRPC 서버 생성 및 시작
    s.server, err = queryapp.NewServer(ctx, qs, mqs, &s.config.QueryOptions, tm, telset)
    s.server.Start(ctx)
}
```

### 2.4 HTTP/gRPC 서버 구조

**소스 위치**: `cmd/jaeger/internal/extension/jaegerquery/internal/server.go`

```go
// server.go:40-48
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

gRPC 핸들러 등록:

```go
// server.go:91-108
func registerGRPCHandlers(server *grpc.Server, querySvc *querysvc.QueryService, telset telemetry.Settings) {
    reflection.Register(server)
    handler := NewGRPCHandler(querySvc, GRPCHandlerOptions{Logger: telset.Logger})
    healthServer := health.NewServer()

    api_v2.RegisterQueryServiceServer(server, handler)              // v2 legacy API
    api_v3.RegisterQueryServiceServer(server, &apiv3.Handler{...})  // v3 API

    healthServer.SetServingStatus("jaeger.api_v2.QueryService", ...)
    healthServer.SetServingStatus("jaeger.api_v3.QueryService", ...)
    grpc_health_v1.RegisterHealthServer(server, healthServer)
}
```

HTTP 라우터 초기화 (`initRouter`):

```go
// server.go:156-205
func initRouter(...) (http.Handler, io.Closer) {
    apiHandler := NewAPIHandler(querySvc, apiHandlerOptions...)
    r := http.NewServeMux()

    // APIv3 HTTP Gateway 등록
    (&apiv3.HTTPGateway{QueryService: querySvc, ...}).RegisterRoutes(r)

    // v2 Legacy API 등록
    apiHandler.RegisterRoutes(r)

    // /api/* 경로에 대한 404 핸들러 (SPA 라우팅 방지)
    r.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
        http.Error(w, "404 page not found", http.StatusNotFound)
    })

    // 정적 파일 핸들러 (SPA catch-all 포함)
    staticHandlerCloser := RegisterStaticHandler(r, telset.Logger, queryOpts, ...)

    // 미들웨어 체인
    handler = bearertoken.PropagationHandler(...)     // 토큰 전파
    handler = tenancy.ExtractTenantHTTPHandler(...)    // 멀티테넌시
    handler = traceResponseHandler(handler)            // 트레이스 ID 응답 헤더
    return handler, staticHandlerCloser
}
```

### 2.5 Static Asset 서빙과 SPA 라우팅

**소스 위치**: `cmd/jaeger/internal/extension/jaegerquery/internal/static_handler.go`

Jaeger UI는 React SPA(Single Page Application)로, index.html에 **런타임 설정을 주입**하는 방식을 사용한다:

```go
// static_handler.go:29-34
var (
    configPattern      = regexp.MustCompile("JAEGER_CONFIG *= *DEFAULT_CONFIG;")
    versionPattern     = regexp.MustCompile("JAEGER_VERSION *= *DEFAULT_VERSION;")
    compabilityPattern = regexp.MustCompile("JAEGER_STORAGE_CAPABILITIES *= *DEFAULT_STORAGE_CAPABILITIES;")
    basePathPattern    = regexp.MustCompile(`<base href="/"`)
)
```

`loadAndEnrichIndexHTML()` 메서드가 index.html을 읽고 정규 표현식으로 플레이스홀더를 치환한다:

```go
// static_handler.go:103-134
func (sH *StaticAssetsHandler) loadAndEnrichIndexHTML(...) ([]byte, error) {
    indexBytes, err := loadIndexHTML(open)

    // (1) UI 설정 주입 (JSON 또는 JS 파일)
    if configObject, err := loadUIConfig(sH.options.ConfigFile); ...
        indexBytes = configObject.regexp.ReplaceAll(indexBytes, configObject.config)

    // (2) 스토리지 기능 플래그 주입 (archiveStorage 등)
    capabilitiesJSON, _ := json.Marshal(sH.options.StorageCapabilities)
    indexBytes = compabilityPattern.ReplaceAll(indexBytes, ...)

    // (3) 버전 정보 주입
    versionJSON, _ := json.Marshal(version.Get())
    indexBytes = versionPattern.ReplaceAll(indexBytes, ...)

    // (4) Base Path 설정
    indexBytes = basePathPattern.ReplaceAll(indexBytes,
        fmt.Appendf(nil, `<base href="%s/"`, sH.options.BasePath))
    return indexBytes, nil
}
```

**UI 설정 파일 감시** - `fswatcher.FSWatcher`를 사용하여 UI config 파일 변경을 감지하고, 변경 시 `reloadUIConfig()`를 호출하여 index.html을 재생성한다. `atomic.Value`를 사용하여 lock-free로 최신 HTML을 서빙한다:

```go
// static_handler.go:54-59
type StaticAssetsHandler struct {
    options   StaticAssetsHandlerOptions
    indexHTML atomic.Value  // stores []byte - lock-free 읽기
    assetsFS  http.FileSystem
    watcher   *fswatcher.FSWatcher
}
```

**SPA 라우팅**: `/static/` 경로 이외의 모든 요청은 index.html을 반환한다 (catch-all):

```go
// static_handler.go:210-240
func (sH *StaticAssetsHandler) RegisterRoutes(router *http.ServeMux) {
    // /static/ 경로 → 실제 정적 파일 서빙
    router.Handle(staticPattern, sH.loggingHandler(fileServer))

    // 나머지 모든 경로 → index.html 반환 (SPA catch-all)
    router.Handle(catchAllPattern, sH.loggingHandler(http.HandlerFunc(sH.notFound)))
}

func (sH *StaticAssetsHandler) notFound(w http.ResponseWriter, _ *http.Request) {
    w.Header().Set("Content-Type", "text/html; charset=utf-8")
    w.Write(sH.indexHTML.Load().([]byte))  // atomic.Value에서 최신 HTML 읽기
}
```

### 2.6 설정 구조

```go
// config.go:16-29 (jaegerquery/config.go)
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

**소스 위치**: `cmd/jaeger/internal/extension/jaegerquery/internal/flags.go`

```go
// flags.go:28-48
type QueryOptions struct {
    BasePath               string                    `mapstructure:"base_path"`
    UIConfig               UIConfig                  `mapstructure:"ui"`
    BearerTokenPropagation bool                      `mapstructure:"bearer_token_propagation"`
    MaxClockSkewAdjust     time.Duration             `mapstructure:"max_clock_skew_adjust"`
    MaxTraceSize           int                       `mapstructure:"max_trace_size"`
    EnableTracing          bool                      `mapstructure:"enable_tracing"`
    HTTP                   confighttp.ServerConfig   `mapstructure:"http"`
    GRPC                   configgrpc.ServerConfig   `mapstructure:"grpc"`
}
```

기본 포트:
- HTTP: **16686** (Jaeger UI 및 REST API)
- gRPC: **16685** (gRPC Query API)

---

## 3. Storage Exporter (jaeger_storage_exporter)

### 3.1 개요

Storage Exporter는 OTel Collector의 **Exporter 컴포넌트**로, 파이프라인에서 수신한 `ptrace.Traces` 데이터를 스토리지 백엔드에 기록한다.

**소스 위치**:
- Exporter 로직: `cmd/jaeger/internal/exporters/storageexporter/exporter.go`
- Factory: `cmd/jaeger/internal/exporters/storageexporter/factory.go`
- Config: `cmd/jaeger/internal/exporters/storageexporter/config.go`

### 3.2 구조체

```go
// exporter.go:19-24
type storageExporter struct {
    config      *Config
    logger      *zap.Logger
    traceWriter tracestore.Writer
    sanitizer   sanitizer.Func
}
```

`sanitizer`는 생성 시점에 `sanitizer.Sanitize`로 초기화된다. 이것은 표준 sanitizer 체인이다.

### 3.3 초기화 흐름

```
createTracesExporter()
  |
  +-- newExporter(cfg, telset) → storageExporter 생성
  |
  +-- exporterhelper.NewTraces() 호출
       |
       +-- WithStart(ex.start)      → start() 콜백 등록
       +-- WithShutdown(ex.close)   → close() 콜백 등록
       +-- WithRetry(cfg.RetryConfig)
       +-- WithQueue(cfg.QueueConfig)
```

`start()` 메서드에서 Storage Extension으로부터 팩토리를 가져오고 Writer를 생성한다:

```go
// exporter.go:34-45
func (exp *storageExporter) start(_ context.Context, host component.Host) error {
    f, err := jaegerstorage.GetTraceStoreFactory(exp.config.TraceStorage, host)
    if err != nil {
        return fmt.Errorf("cannot find storage factory: %w", err)
    }
    if exp.traceWriter, err = f.CreateTraceWriter(); err != nil {
        return fmt.Errorf("cannot create trace writer: %w", err)
    }
    return nil
}
```

### 3.4 트레이스 쓰기 파이프라인

```go
// exporter.go:52-54
func (exp *storageExporter) pushTraces(ctx context.Context, td ptrace.Traces) error {
    return exp.traceWriter.WriteTraces(ctx, exp.sanitizer(td))
}
```

이 한 줄이 전체 쓰기 파이프라인을 표현한다:

```
수신된 ptrace.Traces
    |
    v
sanitizer(td)           ← 데이터 정제
    |
    v
traceWriter.WriteTraces  ← 스토리지에 기록
```

### 3.5 Sanitizer 체인

**소스 위치**: `internal/jptrace/sanitizer/sanitizer.go`

```go
// sanitizer.go:14
var Sanitize = NewChainedSanitizer(NewStandardSanitizers()...)

// sanitizer.go:18-25
func NewStandardSanitizers() []Func {
    return []Func{
        NewEmptyServiceNameSanitizer(),   // 빈 서비스명 → "empty-service-name"
        NewEmptySpanNameSanitizer(),      // 빈 스팬명 → "empty-span-name"
        NewUTF8Sanitizer(),              // 유효하지 않은 UTF-8 문자 제거
        NewNegativeDurationSanitizer(),  // 음수 duration 보정
    }
}
```

체인 실행 방식:

```go
// sanitizer.go:29-38
func NewChainedSanitizer(sanitizers ...Func) Func {
    if len(sanitizers) == 1 {
        return sanitizers[0]  // 단일 sanitizer → 간접 호출 최소화
    }
    return func(traces ptrace.Traces) ptrace.Traces {
        for _, s := range sanitizers {
            traces = s(traces)  // 순차 적용
        }
        return traces
    }
}
```

### 3.6 설정

```go
// config.go:21-25
type Config struct {
    TraceStorage string          `mapstructure:"trace_storage" valid:"required"`
    QueueConfig  configoptional.Optional[exporterhelper.QueueBatchConfig]
    RetryConfig  configretry.BackOffConfig `mapstructure:"retry_on_failure"`
}
```

`TraceStorage`는 `jaeger_storage` Extension에 선언된 백엔드 이름을 참조한다.

---

## 4. QueryService

### 4.1 개요

QueryService는 **트레이스 조회의 핵심 비즈니스 로직**을 담당한다. HTTP/gRPC 핸들러가 이 서비스를 호출하여 스토리지에서 트레이스를 읽고, 조정(adjust)하여 반환한다.

**소스 위치**: `cmd/jaeger/internal/extension/jaegerquery/querysvc/service.go`

### 4.2 구조체

```go
// service.go:47-52
type QueryService struct {
    traceReader      tracestore.Reader
    dependencyReader depstore.Reader
    adjuster         adjuster.Adjuster
    options          QueryServiceOptions
}
```

```go
// service.go:26-36
type QueryServiceOptions struct {
    ArchiveTraceReader tracestore.Reader
    ArchiveTraceWriter tracestore.Writer
    MaxClockSkewAdjust time.Duration
    MaxTraceSize       int
}
```

### 4.3 생성 시 Adjuster 초기화

```go
// service.go:71-86
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

**StandardAdjusters** 목록 (`cmd/jaeger/internal/extension/jaegerquery/internal/adjuster/standard.go`):

```go
// standard.go:12-23
func StandardAdjusters(maxClockSkewAdjust time.Duration) []Adjuster {
    return []Adjuster{
        DeduplicateClientServerSpanIDs(),  // 클라이언트/서버 스팬 ID 중복 제거
        SortCollections(),                 // 컬렉션 정렬
        DeduplicateSpans(),                // 스팬 중복 제거 (SortCollections 이후 실행)
        CorrectClockSkew(maxClockSkewAdjust), // 시계 왜곡 보정
        NormalizeIPAttributes(),           // IP 속성 정규화
        MoveLibraryAttributes(),           // 라이브러리 속성 이동
        RemoveEmptySpanLinks(),            // 빈 스팬 링크 제거
    }
}
```

Adjuster는 `Sequence` 패턴으로 결합되며, 에러가 발생해도 체인 실행을 중단하지 않는다:

```go
// adjuster.go:30-42
func Sequence(adjusters ...Adjuster) Adjuster {
    return sequence{adjusters: adjusters}
}

func (c sequence) Adjust(traces ptrace.Traces) {
    for _, adjuster := range c.adjusters {
        adjuster.Adjust(traces)  // 에러와 무관하게 순차 실행
    }
}
```

### 4.4 GetTraces: Primary + Archive 폴백

`GetTraces`는 Go 1.23의 `iter.Seq2` 이터레이터를 반환한다:

```go
// service.go:102-122
func (qs QueryService) GetTraces(
    ctx context.Context,
    params GetTraceParams,
) iter.Seq2[[]ptrace.Traces, error] {
    getTracesIter := qs.traceReader.GetTraces(ctx, params.TraceIDs...)
    return func(yield func([]ptrace.Traces, error) bool) {
        // (1) Primary Reader에서 조회
        foundTraceIDs, proceed := qs.receiveTraces(getTracesIter, yield, params.RawTraces)

        // (2) 찾지 못한 트레이스 → Archive Reader에서 조회
        if proceed && qs.options.ArchiveTraceReader != nil {
            var missingTraceIDs []tracestore.GetTraceParams
            for _, id := range params.TraceIDs {
                if _, found := foundTraceIDs[id.TraceID]; !found {
                    missingTraceIDs = append(missingTraceIDs, id)
                }
            }
            if len(missingTraceIDs) > 0 {
                getArchiveTracesIter := qs.options.ArchiveTraceReader.GetTraces(ctx, missingTraceIDs...)
                qs.receiveTraces(getArchiveTracesIter, yield, params.RawTraces)
            }
        }
    }
}
```

처리 흐름:

```
GetTraces(traceIDs)
    |
    +-- Primary Reader.GetTraces()
    |       |
    |       +-- receiveTraces() → Adjuster 적용 → yield
    |       |
    |       +-- foundTraceIDs 수집
    |
    +-- missingTraceIDs 계산 (요청 - 발견)
    |
    +-- Archive Reader.GetTraces(missingTraceIDs)
            |
            +-- receiveTraces() → Adjuster 적용 → yield
```

### 4.5 receiveTraces 내부

```go
// service.go:211-246
func (qs QueryService) receiveTraces(
    seq iter.Seq2[[]ptrace.Traces, error],
    yield func([]ptrace.Traces, error) bool,
    rawTraces bool,
) (map[pcommon.TraceID]struct{}, bool) {
    foundTraceIDs := make(map[pcommon.TraceID]struct{})

    processTraces := func(traces []ptrace.Traces, err error) bool {
        for _, trace := range traces {
            if !rawTraces {
                qs.adjuster.Adjust(trace)  // Adjuster 적용
            }
            // 발견된 트레이스 ID 기록
            jptrace.SpanIter(trace)(func(_ jptrace.SpanIterPos, span ptrace.Span) bool {
                foundTraceIDs[span.TraceID()] = struct{}{}
                return true
            })
        }
        proceed = yield(traces, nil)
        return proceed
    }

    if rawTraces {
        seq(processTraces)  // Raw 모드: 집계 없이 그대로 전달
    } else {
        // 일반 모드: 같은 트레이스의 청크를 하나로 집계
        jptrace.AggregateTracesWithLimit(seq, qs.options.MaxTraceSize)(...)
    }
    return foundTraceIDs, proceed
}
```

**RawTraces 모드**: Adjuster 적용 없이, 스토리지에서 반환된 청크를 그대로 전달
**일반 모드**: `AggregateTracesWithLimit`로 같은 트레이스의 청크를 합치고, `MaxTraceSize`로 스팬 수를 제한한 뒤, Adjuster를 적용

### 4.6 FindTraces

```go
// service.go:150-158
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

`FindTraces`는 아카이브 폴백 없이 Primary Reader만 조회한다. 결과에 대한 Adjuster 적용은 `receiveTraces`에서 동일하게 처리한다.

### 4.7 ArchiveTrace

```go
// service.go:163-192
func (qs QueryService) ArchiveTrace(ctx context.Context, query tracestore.GetTraceParams) error {
    if qs.options.ArchiveTraceWriter == nil {
        return errNoArchiveSpanStorage
    }
    getTracesIter := qs.GetTraces(ctx, GetTraceParams{TraceIDs: []tracestore.GetTraceParams{query}})
    var found bool
    var archiveErr error
    getTracesIter(func(traces []ptrace.Traces, err error) bool {
        for _, trace := range traces {
            found = true
            err = qs.options.ArchiveTraceWriter.WriteTraces(ctx, trace)
            if err != nil { archiveErr = errors.Join(archiveErr, err) }
        }
        return true
    })
    if archiveErr == nil && !found {
        return spanstore.ErrTraceNotFound
    }
    return archiveErr
}
```

`GetTraces` → `ArchiveTraceWriter.WriteTraces`로 이어지는 읽기-쓰기 패턴이다.

---

## 5. Remote Sampling Extension

### 5.1 개요

Remote Sampling Extension은 Jaeger 클라이언트에게 **샘플링 전략을 동적으로 제공**한다. 두 가지 모드를 지원한다:
- **File-based**: JSON 파일에서 정적 전략 로드 (자동 리로드 지원)
- **Adaptive**: 트래픽 패턴에 따라 확률 자동 조정

**소스 위치**: `cmd/jaeger/internal/extension/remotesampling/extension.go`

### 5.2 구조체

```go
// extension.go:44-53
type rsExtension struct {
    cfg              *Config
    telemetry        component.TelemetrySettings
    httpServer       *http.Server
    grpcServer       *grpc.Server
    strategyProvider samplingstrategy.Provider
    adaptiveStore    samplingstore.Store
    distLock         *leaderelection.DistributedElectionParticipant
    shutdownWG       sync.WaitGroup
}
```

### 5.3 설정 구조

**소스 위치**: `cmd/jaeger/internal/extension/remotesampling/config.go`

```go
// config.go:41-46
type Config struct {
    File     configoptional.Optional[FileConfig]              `mapstructure:"file"`
    Adaptive configoptional.Optional[AdaptiveConfig]          `mapstructure:"adaptive"`
    HTTP     configoptional.Optional[confighttp.ServerConfig] `mapstructure:"http"`
    GRPC     configoptional.Optional[configgrpc.ServerConfig] `mapstructure:"grpc"`
}
```

**제약 조건**: File과 Adaptive 중 **정확히 하나만** 설정할 수 있다:

```go
// config.go:64-71
func (cfg *Config) Validate() error {
    if !cfg.File.HasValue() && !cfg.Adaptive.HasValue() {
        return errNoProvider  // "no sampling strategy provider specified"
    }
    if cfg.File.HasValue() && cfg.Adaptive.HasValue() {
        return errMultipleProviders  // "only one sampling strategy provider can be specified"
    }
    // ...
}
```

### 5.4 Start 흐름

```go
// extension.go:103-139
func (ext *rsExtension) Start(ctx context.Context, host component.Host) error {
    // (1) 전략 프로바이더 초기화 (File 또는 Adaptive 중 하나)
    if ext.cfg.File.HasValue() {
        ext.startFileBasedStrategyProvider(ctx)
    }
    if ext.cfg.Adaptive.HasValue() {
        ext.startAdaptiveStrategyProvider(host)
    }

    // (2) HTTP 서버 시작 (선택사항)
    if ext.cfg.HTTP.HasValue() {
        ext.startHTTPServer(ctx, host)
    }

    // (3) gRPC 서버 시작 (선택사항)
    if ext.cfg.GRPC.HasValue() {
        ext.startGRPCServer(ctx, host)
    }
    return nil
}
```

### 5.5 File-based 프로바이더

```go
// extension.go:162-178
func (ext *rsExtension) startFileBasedStrategyProvider(_ context.Context) error {
    fileCfg := ext.cfg.File.Get()
    opts := file.Options{
        StrategiesFile:             fileCfg.Path,
        ReloadInterval:             fileCfg.ReloadInterval,
        DefaultSamplingProbability: fileCfg.DefaultSamplingProbability,
    }
    provider, err := file.NewProvider(opts, ext.telemetry.Logger)
    ext.strategyProvider = provider
    return nil
}
```

File-based 설정:

```go
// config.go:48-55
type FileConfig struct {
    Path                       string        `mapstructure:"path"`
    ReloadInterval             time.Duration `mapstructure:"reload_interval"`
    DefaultSamplingProbability float64       `mapstructure:"default_sampling_probability" valid:"range(0|1)"`
}
```

`ReloadInterval`을 설정하면 파일 변경 시 자동으로 전략을 다시 로드한다.

### 5.6 Adaptive 프로바이더

```go
// extension.go:180-218
func (ext *rsExtension) startAdaptiveStrategyProvider(host component.Host) error {
    adaptiveCfg := ext.cfg.Adaptive.Get()

    // (1) 샘플링 스토어 팩토리 획득
    storeFactory, err := jaegerstorage.GetSamplingStoreFactory(adaptiveCfg.SamplingStore, host)

    // (2) 샘플링 스토어 생성
    store, err := storeFactory.CreateSamplingStore(adaptiveCfg.AggregationBuckets)
    ext.adaptiveStore = store

    // (3) 분산 잠금 생성 + 리더 선출 참여
    lock, err := storeFactory.CreateLock()
    ep := leaderelection.NewElectionParticipant(lock, defaultResourceName,
        leaderelection.ElectionParticipantOptions{
            LeaderLeaseRefreshInterval:   adaptiveCfg.LeaderLeaseRefreshInterval,
            FollowerLeaseRefreshInterval: adaptiveCfg.FollowerLeaseRefreshInterval,
            Logger:                       ext.telemetry.Logger,
        })
    ep.Start()
    ext.distLock = ep

    // (4) Adaptive Provider 생성 및 시작
    provider := adaptive.NewProvider(adaptiveCfg.Options, ext.telemetry.Logger, ext.distLock, store)
    provider.Start()
    ext.strategyProvider = provider
    return nil
}
```

### 5.7 Adaptive Provider 내부 동작

**소스 위치**: `internal/sampling/samplingstrategy/adaptive/provider.go`

```go
// provider.go:26-47
type Provider struct {
    Options
    mu                  sync.RWMutex
    electionParticipant leaderelection.ElectionParticipant
    storage             samplingstore.Store
    probabilities       model.ServiceOperationProbabilities  // 서비스별 확률
    strategyResponses   map[string]*api_v2.SamplingStrategyResponse  // 캐시
    followerRefreshInterval time.Duration  // 기본값: 20초
}
```

**리더/팔로워 패턴**:

```go
// provider.go:90-112
func (p *Provider) runUpdateProbabilitiesLoop() {
    ticker := time.NewTicker(p.followerRefreshInterval)  // 20초 간격
    for {
        select {
        case <-ticker.C:
            // 팔로워만 스토리지에서 확률 갱신
            if !p.isLeader() {
                p.loadProbabilities()
                p.generateStrategyResponses()
            }
        case <-p.shutdown:
            return
        }
    }
}
```

- **리더**: PostAggregator가 확률을 계산하여 스토리지에 기록
- **팔로워**: 20초마다 스토리지에서 최신 확률을 읽어 로컬 캐시 갱신

### 5.8 HTTP/gRPC 서빙

HTTP 서버:

```go
// extension.go:221-256
func (ext *rsExtension) startHTTPServer(ctx context.Context, host component.Host) error {
    handler := samplinghttp.NewHandler(samplinghttp.HandlerParams{
        ConfigManager: &samplinghttp.ConfigManager{
            SamplingProvider: ext.strategyProvider,
        },
        MetricsFactory: mf,
    })
    httpMux := http.NewServeMux()
    handler.RegisterRoutes(httpMux)
    // ...
}
```

gRPC 서버:

```go
// extension.go:258-287
func (ext *rsExtension) startGRPCServer(ctx context.Context, host component.Host) error {
    // gRPC 서버 생성
    api_v2.RegisterSamplingManagerServer(ext.grpcServer,
        samplinggrpc.NewHandler(ext.strategyProvider))

    // Health check 등록
    healthServer := health.NewServer()
    healthServer.SetServingStatus("jaeger.api_v2.SamplingManager",
        grpc_health_v1.HealthCheckResponse_SERVING)
    grpc_health_v1.RegisterHealthServer(ext.grpcServer, healthServer)
    // ...
}
```

### 5.9 외부 컴포넌트 접근

`GetAdaptiveSamplingComponents()` 함수로 Adaptive Sampling Processor가 필요한 컴포넌트를 가져온다:

```go
// extension.go:72-101
func GetAdaptiveSamplingComponents(host component.Host) (*AdaptiveSamplingComponents, error) {
    // Extension 검색 → 타입 단언 → adaptiveStore/distLock 검증
    return &AdaptiveSamplingComponents{
        SamplingStore: ext.adaptiveStore,
        DistLock:      ext.distLock,
        Options:       &adaptiveCfg.Options,
    }, nil
}
```

---

## 6. Adaptive Sampling Processor

### 6.1 개요

Adaptive Sampling Processor는 OTel Collector의 **Processor 컴포넌트**로, 파이프라인을 흐르는 트레이스 데이터를 가로채 **처리량(throughput) 통계를 추출**한다. 트레이스 데이터 자체를 변경하지 않고 그대로 통과시키면서(pass-through), 루트 스팬에서 샘플링 정보를 수집한다.

**소스 위치**: `cmd/jaeger/internal/processors/adaptivesampling/processor.go`

### 6.2 구조체

```go
// processor.go:20-24
type traceProcessor struct {
    config     *Config
    aggregator samplingstrategy.Aggregator
    telset     component.TelemetrySettings
}
```

설정은 비어 있다. 모든 실질적인 설정은 Remote Sampling Extension에서 관리한다:

```go
// config.go:12-14
type Config struct {
    // all configuration for the processor is in the remotesampling extension
}
```

### 6.3 초기화

```go
// processor.go:33-56
func (tp *traceProcessor) start(_ context.Context, host component.Host) error {
    // (1) Remote Sampling Extension에서 컴포넌트 획득
    parts, err := remotesampling.GetAdaptiveSamplingComponents(host)

    // (2) Aggregator 생성
    agg, err := adaptive.NewAggregator(
        *parts.Options,
        tp.telset.Logger,
        otelmetrics.NewFactory(tp.telset.MeterProvider),
        parts.DistLock,
        parts.SamplingStore,
    )

    // (3) Aggregator 시작 (백그라운드 루프)
    agg.Start()
    tp.aggregator = agg
    return nil
}
```

### 6.4 트레이스 처리

```go
// processor.go:67-78
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
    return td, nil  // 트레이스 데이터는 변경 없이 통과
}
```

**핵심**: `return td, nil` - 트레이스 데이터를 변경하지 않는다. 단지 `HandleRootSpan()`을 호출하여 통계를 추출할 뿐이다.

### 6.5 Aggregator 내부

**소스 위치**: `internal/sampling/samplingstrategy/adaptive/aggregator.go`

#### HandleRootSpan - 루트 스팬 필터링

```go
// aggregator.go:142-157
func (a *aggregator) HandleRootSpan(span *spanmodel.Span) {
    // 루트 스팬이 아니면 무시 (ParentSpanID == 0)
    if span.ParentSpanID() != spanmodel.NewSpanID(0) {
        return
    }
    service := span.Process.ServiceName
    if service == "" || span.OperationName == "" {
        return
    }
    // sampler.type과 sampler.param 태그 추출
    samplerType, samplerParam := getSamplerParams(span, a.postAggregator.logger)
    if samplerType == spanmodel.SamplerTypeUnrecognized {
        return
    }
    a.RecordThroughput(service, span.OperationName, samplerType, samplerParam)
}
```

#### RecordThroughput - 처리량 기록

```go
// aggregator.go:101-126
func (a *aggregator) RecordThroughput(service, operation string, samplerType spanmodel.SamplerType, probability float64) {
    a.Lock()
    defer a.Unlock()

    // 서비스/오퍼레이션별 throughput 구조 초기화
    if _, ok := a.currentThroughput[service]; !ok {
        a.currentThroughput[service] = make(map[string]*model.Throughput)
    }
    throughput, ok := a.currentThroughput[service][operation]
    if !ok {
        throughput = &model.Throughput{
            Service:       service,
            Operation:     operation,
            Probabilities: make(map[string]struct{}),
        }
        a.currentThroughput[service][operation] = throughput
    }

    // 확률적 샘플링만 카운트 증가
    if samplerType == spanmodel.SamplerTypeProbabilistic {
        throughput.Count++
    }
}
```

**왜 확률적 샘플링만 카운트하는가?** Rate-limiting 샘플러로 수집된 스팬은 처리량 계산에서 제외한다. 확률적 샘플러의 카운트만 사용해야 실제 트래픽 볼륨을 정확히 추정할 수 있기 때문이다 (count / probability = estimated total QPS).

#### 집계 루프

```go
// aggregator.go:70-85
func (a *aggregator) runAggregationLoop() {
    ticker := time.NewTicker(a.aggregationInterval)  // 기본 1분
    for {
        select {
        case <-ticker.C:
            a.Lock()
            a.saveThroughput()           // 현재 처리량을 스토리지에 저장
            a.currentThroughput = make(serviceOperationThroughput)  // 리셋
            a.postAggregator.runCalculation()  // 확률 재계산
            a.Unlock()
        case <-a.stop:
            ticker.Stop()
            return
        }
    }
}
```

기본 옵션값 (`internal/sampling/samplingstrategy/adaptive/options.go`):

```
+----------------------------------+-----------+--------------------------------------+
| 옵션                              | 기본값     | 설명                                  |
+----------------------------------+-----------+--------------------------------------+
| TargetSamplesPerSecond           | 1         | 오퍼레이션당 목표 초당 샘플 수             |
| DeltaTolerance                   | 0.3       | 허용 편차 비율 (30%)                     |
| CalculationInterval              | 1분       | 확률 재계산 주기                          |
| AggregationBuckets               | 10        | 메모리에 유지할 처리량 버킷 수              |
| InitialSamplingProbability       | 0.001     | 새 오퍼레이션의 초기 샘플링 확률             |
| MinSamplingProbability           | 1e-5      | 최소 샘플링 확률                          |
| MinSamplesPerSecond              | 1/60      | 최소 초당 샘플 수 (분당 1개)               |
| LeaderLeaseRefreshInterval       | 5초       | 리더 잠금 갱신 주기                        |
| FollowerLeaseRefreshInterval     | 60초      | 팔로워 잠금 재시도 주기                     |
+----------------------------------+-----------+--------------------------------------+
```

---

## 7. 컴포넌트 간 관계 및 데이터 흐름

### 7.1 의존성 관계

```
+--------------------+
| jaeger_storage     |  ← 다른 모든 컴포넌트의 기반
| (Extension)        |
+--------+-----------+
         |
    +----+----+----------------+
    |         |                |
    v         v                v
+--------+ +--------+  +----------------+
| jaeger | | storage|  | remote_sampling|
| _query | | _exp.  |  | (Extension)    |
| (Ext.) | | (Exp.) |  +-------+--------+
+--------+ +--------+          |
                                v
                       +----------------+
                       | adaptive_samp. |
                       | (Processor)    |
                       +----------------+
```

### 7.2 트레이스 수집 파이프라인

```
OTLP Receiver
    |
    v
Adaptive Sampling Processor  ← 처리량 통계 추출 (pass-through)
    |
    v
Storage Exporter             ← sanitize → WriteTraces
    |
    v
Storage Backend (memory / badger / cassandra / elasticsearch / ...)
```

### 7.3 트레이스 조회 파이프라인

```
Client (UI / API)
    |
    v
HTTP/gRPC Server (Query Extension)
    |
    v
QueryService
    |
    +-- GetTraces / FindTraces
    |       |
    |       +-- Primary Reader → Adjuster → Response
    |       |
    |       +-- (miss) Archive Reader → Adjuster → Response
    |
    +-- ArchiveTrace
            |
            +-- GetTraces → Archive Writer
```

### 7.4 Adaptive Sampling 피드백 루프

```
+--------+    트레이스 데이터    +-----------+
| OTLP   | ----------------> | Adaptive  |
| Recv.  |                   | Processor |
+--------+                   +-----+-----+
                                   |
                            HandleRootSpan()
                                   |
                                   v
                            +------+------+
                            | Aggregator  |
                            | (throughput)|
                            +------+------+
                                   |
                          saveThroughput() (1분마다)
                                   |
                                   v
                            +------+------+
                            | Sampling    |
                            | Store       |
                            +------+------+
                                   |
                    loadProbabilities() (팔로워: 20초마다)
                                   |
                                   v
                            +------+------+
                            | Provider    |
                            | (strategies)|
                            +------+------+
                                   |
                        GetSamplingStrategy()
                                   |
                                   v
                            +------+------+
                            | HTTP/gRPC   |
                            | Endpoint    |
                            +------+------+
                                   |
                                   v
                            +------+------+
                            | Jaeger      |
                            | Client SDK  |
                            +-------------+
```

---

## 8. 설계 원칙 요약

### 8.1 Lazy Initialization
Storage Extension은 팩토리를 **최초 접근 시점에 생성**한다. 이는 불필요한 리소스 사용을 방지하고, Extension 시작 순서에 대한 의존성을 줄인다.

### 8.2 인터페이스 기반 추상화
`tracestore.Factory`, `tracestore.Reader`, `tracestore.Writer` 등 인터페이스를 통해 스토리지 구현체를 교체할 수 있다. 타입 단언(`type assertion`)으로 선택적 기능(SamplingStore, Purger, DependencyReader)을 제공한다.

### 8.3 Pass-through Processor
Adaptive Sampling Processor는 트레이스 데이터를 **변경하지 않고 그대로 통과**시킨다. 단지 루트 스팬에서 메타데이터를 추출할 뿐이다. 이는 데이터 무결성을 보장하면서도 관측 정보를 수집하는 패턴이다.

### 8.4 Leader-Follower 패턴
Adaptive Sampling은 분산 환경에서 **하나의 리더만 확률을 계산**하고, 나머지 팔로워는 스토리지에서 결과를 읽어 사용한다. 이를 통해 계산 중복을 방지하고 일관된 샘플링 전략을 제공한다.

### 8.5 Sanitizer + Adjuster 파이프라인
쓰기 경로에서는 **Sanitizer**(빈 이름 보정, UTF-8 검증, 음수 duration 보정)를, 읽기 경로에서는 **Adjuster**(시계 왜곡 보정, 중복 제거, IP 정규화)를 적용한다. 둘 다 체인 패턴으로 구성되어 확장이 용이하다.
