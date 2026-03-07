# MCP 서버

## 목차

1. [개요](#1-개요)
2. [MCP(Model Context Protocol)란](#2-mcpmodel-context-protocol란)
3. [Progressive Disclosure 아키텍처](#3-progressive-disclosure-아키텍처)
4. [Extension 구조](#4-extension-구조)
5. [MCP 서버 구현](#5-mcp-서버-구현)
6. [7개 MCP 도구 상세](#6-7개-mcp-도구-상세)
7. [Critical Path 알고리즘](#7-critical-path-알고리즘)
8. [타입 시스템](#8-타입-시스템)
9. [설정 및 운영](#9-설정-및-운영)
10. [설계 결정과 트레이드오프](#10-설계-결정과-트레이드오프)
11. [실전 사용 시나리오](#11-실전-사용-시나리오)
12. [정리](#12-정리)

---

## 1. 개요

Jaeger MCP 서버는 **LLM(Large Language Model)**이 분산 트레이싱 데이터를 효율적으로 조회하고 분석할 수 있도록 설계된 확장(Extension)이다. MCP(Model Context Protocol)라는 개방형 표준을 통해 AI 에이전트가 Jaeger의 트레이스 데이터에 구조화된 방식으로 접근한다.

**왜 MCP 서버가 필요한가?**

분산 트레이스는 수백~수천 개의 스팬으로 구성될 수 있다. 전체 트레이스를 LLM의 컨텍스트 윈도우에 로드하면:

```
문제점:
  ┌─────────────────────────────────────────────────────┐
  │ 1. 토큰 폭발: 하나의 트레이스가 수만 토큰 소모        │
  │ 2. 정보 과부하: 대부분의 속성은 디버깅에 불필요        │
  │ 3. 구조적 추론 불가: 플랫 데이터로는 인과 관계 파악 곤란 │
  │ 4. 비용 증가: 불필요한 데이터로 API 호출 비용 증가      │
  └─────────────────────────────────────────────────────┘
```

MCP 서버는 이 문제를 **progressive disclosure(점진적 공개)** 패턴으로 해결한다. LLM이 필요한 정보만, 필요한 시점에, 적절한 수준으로 조회할 수 있다.

핵심 소스 파일:

```
cmd/jaeger/internal/extension/jaegermcp/
├── config.go                                  # 설정 구조체
├── factory.go                                 # Extension 팩토리
├── server.go                                  # Extension 구현 (서버 라이프사이클)
└── internal/
    ├── criticalpath/                          # Critical Path 알고리즘
    │   ├── cpspan.go                          # CPSpan 구조체, 변환 함수
    │   ├── criticalpath.go                    # 핵심 알고리즘
    │   ├── find_lfc.go                        # Last Finishing Child 탐색
    │   ├── get_child_of_spans.go              # FOLLOWS_FROM 스팬 제거
    │   └── sanitize.go                        # 오버플로우 자식 스팬 정리
    ├── handlers/                              # MCP 도구 핸들러
    │   ├── get_services.go                    # get_services 도구
    │   ├── get_span_names.go                  # get_span_names 도구
    │   ├── search_traces.go                   # search_traces 도구
    │   ├── get_trace_topology.go              # get_trace_topology 도구
    │   ├── get_critical_path.go               # get_critical_path 도구
    │   ├── get_span_details.go                # get_span_details 도구
    │   └── get_trace_errors.go                # get_trace_errors 도구
    └── types/                                 # 입출력 타입 정의
        ├── get_services.go
        ├── get_span_names.go
        ├── search_traces.go
        ├── get_trace_topology.go
        ├── get_critical_path.go
        ├── get_span_details.go
        └── get_trace_errors.go
```

ADR(Architecture Decision Record): `docs/adr/002-mcp-server.md`

---

## 2. MCP(Model Context Protocol)란

### 2.1 프로토콜 개요

MCP(Model Context Protocol)는 LLM 애플리케이션과 외부 데이터 소스 간의 통합을 위한 **개방형 표준**이다. AI 에이전트가 도구(tool)를 발견하고 호출할 수 있는 구조화된 방법을 정의한다.

```
┌────────────────┐     MCP 프로토콜      ┌────────────────┐
│   LLM 에이전트  │ ◄─────────────────▶  │   MCP 서버     │
│   (Claude,      │                      │   (Jaeger)     │
│    GPT 등)      │     도구 발견         │                │
│                │ ◄─────────────────── │  get_services  │
│                │     도구 호출         │  search_traces │
│                │ ──────────────────▶  │  get_topology  │
│                │     결과 반환         │  get_critical  │
│                │ ◄─────────────────── │  get_details   │
└────────────────┘                      └────────────────┘
```

### 2.2 핵심 개념

| 개념 | 설명 |
|------|------|
| **Tool** | 서버가 제공하는 실행 가능한 함수 (get_services, search_traces 등) |
| **Input Schema** | 도구 입력 파라미터의 JSON Schema 정의 |
| **Output** | 도구 실행 결과 (JSON 형식) |
| **Transport** | 통신 방식 (Streamable HTTP, SSE, StdIO) |
| **Session** | 클라이언트-서버 간 상태 관리 |

### 2.3 Jaeger에서 사용하는 MCP Go SDK

**의존성**: `github.com/modelcontextprotocol/go-sdk/mcp`

이 SDK는 Google과 협력하여 공식적으로 유지보수되는 Go MCP 구현체이다.

```go
// server.go에서 사용하는 핵심 SDK API
import "github.com/modelcontextprotocol/go-sdk/mcp"

// MCP 서버 생성
s.mcpServer = mcp.NewServer(impl, &mcp.ServerOptions{})

// 도구 등록
mcp.AddTool(s.mcpServer, &mcp.Tool{
    Name:        "get_services",
    Description: "List available service names.",
}, handler)

// HTTP 핸들러 생성 (Streamable HTTP + SSE)
mcpHandler := mcp.NewStreamableHTTPHandler(
    func(_ *http.Request) *mcp.Server { return s.mcpServer },
    &mcp.StreamableHTTPOptions{
        JSONResponse:   false,        // SSE 스트리밍 사용
        Stateless:      false,        // 세션 상태 관리
        SessionTimeout: 5 * time.Minute,
    },
)
```

---

## 3. Progressive Disclosure 아키텍처

### 3.1 4단계 워크플로우

MCP 서버의 핵심 설계 원칙은 **progressive disclosure(점진적 공개)**이다. LLM이 전체 트레이스를 한 번에 로드하는 대신, 4단계를 거쳐 점진적으로 세부 정보에 접근한다.

```
┌─────────────────────────────────────────────────────────────┐
│                    Progressive Disclosure                     │
│                                                             │
│  1단계: Search (탐색)                                        │
│  ┌─────────────────────────────────────────┐                │
│  │ get_services  → 서비스 목록 발견          │                │
│  │ get_span_names → 스팬 이름 발견           │                │
│  │ search_traces → 후보 트레이스 검색        │                │
│  │                                         │                │
│  │ 토큰 비용: 매우 낮음 (메타데이터만)        │                │
│  └──────────────────┬──────────────────────┘                │
│                     ▼                                       │
│  2단계: Map (구조 파악)                                      │
│  ┌─────────────────────────────────────────┐                │
│  │ get_trace_topology → 트리 구조 확인       │                │
│  │                                         │                │
│  │ 토큰 비용: 낮음 (속성 없이 구조만)         │                │
│  └──────────────────┬──────────────────────┘                │
│                     ▼                                       │
│  3단계: Diagnose (진단)                                      │
│  ┌─────────────────────────────────────────┐                │
│  │ get_critical_path → 병목 구간 식별        │                │
│  │ get_trace_errors  → 에러 스팬 확인        │                │
│  │                                         │                │
│  │ 토큰 비용: 중간 (핵심 스팬만 선별)         │                │
│  └──────────────────┬──────────────────────┘                │
│                     ▼                                       │
│  4단계: Inspect (상세 조사)                                   │
│  ┌─────────────────────────────────────────┐                │
│  │ get_span_details → 의심 스팬의 전체 데이터 │                │
│  │                                         │                │
│  │ 토큰 비용: 높음 (전체 OTLP 속성)           │                │
│  │ 하지만 소수 스팬만 조회하므로 총비용 최소화 │                │
│  └─────────────────────────────────────────┘                │
└─────────────────────────────────────────────────────────────┘
```

### 3.2 토큰 효율성

```
전통적 접근 (전체 트레이스 덤프):
  100개 스팬 트레이스 → ~50,000 토큰 → 대부분 불필요한 정보

Progressive Disclosure:
  1단계: search_traces        → ~200 토큰 (요약만)
  2단계: get_trace_topology   → ~2,000 토큰 (구조만)
  3단계: get_critical_path    → ~500 토큰 (경로만)
  4단계: get_span_details(3)  → ~1,500 토큰 (3개 스팬만)
  ─────────────────────────────────────
  총: ~4,200 토큰 (약 12배 절약)
```

---

## 4. Extension 구조

### 4.1 팩토리

**파일 경로**: `cmd/jaeger/internal/extension/jaegermcp/factory.go`

```go
// componentType은 설정에서 이 Extension을 식별하는 이름
var componentType = component.MustNewType("jaeger_mcp")

// ID는 이 Extension의 식별자
var ID = component.NewID(componentType)

func NewFactory() extension.Factory {
    return extension.NewFactory(
        componentType,
        createDefaultConfig,
        createExtension,
        component.StabilityLevelAlpha,
    )
}
```

기본 설정:

```go
func createDefaultConfig() component.Config {
    ver := version.Get().GitVersion
    if ver == "" {
        ver = "dev"
    }
    return &Config{
        HTTP: confighttp.ServerConfig{
            NetAddr: confignet.AddrConfig{
                Endpoint:  ports.PortToHostPort(ports.MCPHTTP),  // localhost:16687
                Transport: confignet.TransportTypeTCP,
            },
        },
        ServerName:               "jaeger",
        ServerVersion:            ver,
        MaxSpanDetailsPerRequest: 20,
        MaxSearchResults:         100,
    }
}
```

### 4.2 설정

**파일 경로**: `cmd/jaeger/internal/extension/jaegermcp/config.go`

```go
type Config struct {
    // HTTP 서버 설정 (MCP 프로토콜 엔드포인트)
    HTTP confighttp.ServerConfig `mapstructure:"http"`

    // MCP 프로토콜 식별용 서버 이름
    ServerName string `mapstructure:"server_name"`

    // MCP 서버 버전
    ServerVersion string `mapstructure:"server_version" valid:"required"`

    // 단일 요청당 최대 스팬 조회 수 (기본: 20, 범위: 1~100)
    MaxSpanDetailsPerRequest int `mapstructure:"max_span_details_per_request" valid:"range(1|100)"`

    // 최대 검색 결과 수 (기본: 100, 범위: 1~1000)
    MaxSearchResults int `mapstructure:"max_search_results" valid:"range(1|1000)"`
}
```

검증:

```go
func (cfg *Config) Validate() error {
    _, err := govalidator.ValidateStruct(cfg)
    return err
}
```

### 4.3 의존성

MCP 서버는 `jaeger_query` Extension에 의존한다. `jaeger_query`가 제공하는 `QueryService`를 통해 트레이스 데이터에 접근한다.

```go
// server.go - Dependencies
func (*server) Dependencies() []component.ID {
    return []component.ID{jaegerquery.ID}
}
```

```
의존성 체인:
  jaeger_storage  ←── jaeger_query  ←── jaeger_mcp
  (스토리지 관리)     (쿼리 API)        (MCP 서버)
```

---

## 5. MCP 서버 구현

### 5.1 서버 구조체

**파일 경로**: `cmd/jaeger/internal/extension/jaegermcp/server.go`

```go
type server struct {
    config     *Config
    telset     component.TelemetrySettings
    httpServer *http.Server
    listener   net.Listener
    mcpServer  *mcp.Server
    queryAPI   *querysvc.QueryService
}
```

인터페이스 준수:

```go
var (
    _ extension.Extension             = (*server)(nil)
    _ extensioncapabilities.Dependent = (*server)(nil)
)
```

### 5.2 시작 흐름

```go
func (s *server) Start(ctx context.Context, host component.Host) error {
    s.telset.Logger.Info("Starting Jaeger MCP server",
        zap.String("endpoint", s.config.HTTP.NetAddr.Endpoint))

    // 1. jaeger_query Extension에서 QueryService 획득
    queryExt, err := jaegerquery.GetExtension(host)
    if err != nil {
        return fmt.Errorf("cannot get %s extension: %w", jaegerquery.ID, err)
    }
    s.queryAPI = queryExt.QueryService()

    // 2. MCP 서버 초기화
    impl := &mcp.Implementation{
        Name:    s.config.ServerName,
        Version: s.config.ServerVersion,
    }
    s.mcpServer = mcp.NewServer(impl, &mcp.ServerOptions{})

    // 3. MCP 도구 등록
    s.registerTools()

    // 4. TCP 리스너 생성
    lc := net.ListenConfig{}
    listener, err := lc.Listen(ctx, "tcp", s.config.HTTP.NetAddr.Endpoint)
    if err != nil {
        return fmt.Errorf("failed to listen on %s: %w",
            s.config.HTTP.NetAddr.Endpoint, err)
    }
    s.listener = listener

    // 5. MCP Streamable HTTP 핸들러 생성
    mcpHandler := mcp.NewStreamableHTTPHandler(
        func(_ *http.Request) *mcp.Server { return s.mcpServer },
        &mcp.StreamableHTTPOptions{
            JSONResponse:   false,          // SSE 사용
            Stateless:      false,          // 세션 관리
            SessionTimeout: 5 * time.Minute,
        },
    )

    // 6. HTTP 서버 설정 (MCP + 헬스체크)
    mux := http.NewServeMux()
    mux.Handle("/mcp", mcpHandler)
    mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
        w.WriteHeader(http.StatusOK)
        w.Write([]byte("MCP server is running"))
    })

    s.httpServer = &http.Server{
        Handler:           corsMiddleware(mux),
        ReadHeaderTimeout: 30 * time.Second,
    }

    // 7. 백그라운드에서 서버 시작
    go func() {
        if err := s.httpServer.Serve(s.listener); err != nil &&
            !errors.Is(err, http.ErrServerClosed) {
            s.telset.Logger.Error("MCP server error", zap.Error(err))
        }
    }()

    return nil
}
```

시작 순서 다이어그램:

```
Start()
  │
  ├─ 1. jaeger_query Extension에서 QueryService 획득
  │     └─ jaegerquery.GetExtension(host)
  │        └─ host.GetExtensions() → QueryService 인스턴스
  │
  ├─ 2. MCP 서버 생성
  │     └─ mcp.NewServer(impl, opts)
  │
  ├─ 3. 7개 MCP 도구 등록
  │     └─ registerTools() → mcp.AddTool() x 8
  │
  ├─ 4. TCP 리스너 바인딩
  │     └─ net.Listen("tcp", "localhost:16687")
  │
  ├─ 5. Streamable HTTP 핸들러 생성
  │     └─ mcp.NewStreamableHTTPHandler()
  │
  ├─ 6. HTTP 라우팅 설정
  │     ├─ /mcp    → MCP 프로토콜
  │     └─ /health → 헬스 체크
  │
  └─ 7. CORS 미들웨어 적용 + 서버 시작
```

### 5.3 CORS 미들웨어

브라우저 기반 MCP 클라이언트(MCP Inspector 등)를 지원하기 위해 CORS 미들웨어가 적용된다.

```go
func corsMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Access-Control-Allow-Origin", "*")
        w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
        w.Header().Set("Access-Control-Allow-Headers",
            "Content-Type, Accept, Mcp-Session-Id, Mcp-Protocol-Version, Last-Event-ID")
        w.Header().Set("Access-Control-Expose-Headers", "Mcp-Session-Id")

        if r.Method == http.MethodOptions {
            w.WriteHeader(http.StatusNoContent)
            return
        }

        next.ServeHTTP(w, r)
    })
}
```

**왜 `Mcp-Session-Id`와 `Mcp-Protocol-Version` 헤더를 허용하는가?**

MCP 프로토콜은 세션 관리를 위해 커스텀 헤더를 사용한다. 브라우저의 CORS 정책은 기본적으로 커스텀 헤더를 차단하므로, 명시적으로 허용해야 한다.

### 5.4 종료

```go
func (s *server) Shutdown(ctx context.Context) error {
    s.telset.Logger.Info("Shutting down Jaeger MCP server")

    var errs []error
    if s.httpServer != nil {
        if err := s.httpServer.Shutdown(ctx); err != nil {
            errs = append(errs, fmt.Errorf("failed to shutdown HTTP server: %w", err))
        }
    }

    return errors.Join(errs...)
}
```

---

## 6. 7개 MCP 도구 상세

### 6.1 도구 등록

**파일 경로**: `cmd/jaeger/internal/extension/jaegermcp/server.go`

```go
func (s *server) registerTools() {
    // 1. get_services
    getServicesHandler := handlers.NewGetServicesHandler(s.queryAPI)
    mcp.AddTool(s.mcpServer, &mcp.Tool{
        Name:        "get_services",
        Description: "List available service names. Use this first to discover valid service names for search_traces.",
    }, getServicesHandler)

    // 2. get_span_names
    getSpanNamesHandler := handlers.NewGetSpanNamesHandler(s.queryAPI)
    mcp.AddTool(s.mcpServer, &mcp.Tool{
        Name:        "get_span_names",
        Description: "List available span names for a service. Supports regex filtering and span kind filtering.",
    }, getSpanNamesHandler)

    // 3. health
    mcp.AddTool(s.mcpServer, &mcp.Tool{
        Name:        "health",
        Description: "Check if the Jaeger MCP server is running",
    }, s.healthTool)

    // 4. search_traces
    searchTracesHandler := handlers.NewSearchTracesHandler(s.queryAPI)
    mcp.AddTool(s.mcpServer, &mcp.Tool{
        Name:        "search_traces",
        Description: "Find traces matching service, time, attributes, and duration criteria. Returns trace summary only.",
    }, searchTracesHandler)

    // 5. get_span_details
    getSpanDetailsHandler := handlers.NewGetSpanDetailsHandler(s.queryAPI)
    mcp.AddTool(s.mcpServer, &mcp.Tool{
        Name:        "get_span_details",
        Description: "Fetch full details (attributes, events, links, status) for specific spans.",
    }, getSpanDetailsHandler)

    // 6. get_trace_errors
    getTraceErrorsHandler := handlers.NewGetTraceErrorsHandler(s.queryAPI)
    mcp.AddTool(s.mcpServer, &mcp.Tool{
        Name:        "get_trace_errors",
        Description: "Get full details for all spans with error status.",
    }, getTraceErrorsHandler)

    // 7. get_trace_topology
    getTraceTopologyHandler := handlers.NewGetTraceTopologyHandler(s.queryAPI)
    mcp.AddTool(s.mcpServer, &mcp.Tool{
        Name:        "get_trace_topology",
        Description: "Get the structural tree of a trace showing parent-child relationships, timing, and error locations. Does NOT return attributes or logs.",
    }, getTraceTopologyHandler)

    // 8. get_critical_path
    getCriticalPathHandler := handlers.NewGetCriticalPathHandler(s.queryAPI)
    mcp.AddTool(s.mcpServer, &mcp.Tool{
        Name:        "get_critical_path",
        Description: "Identify the sequence of spans forming the critical latency path (the blocking execution path).",
    }, getCriticalPathHandler)
}
```

### 6.2 도구 1: get_services

**파일 경로**: `cmd/jaeger/internal/extension/jaegermcp/internal/handlers/get_services.go`

서비스 이름 목록을 반환한다. Progressive Disclosure의 첫 단계.

```
입력: { pattern?: string, limit?: int }
출력: { services: string[] }
```

```go
func (h *getServicesHandler) handle(
    ctx context.Context, _ *mcp.CallToolRequest,
    input types.GetServicesInput,
) (*mcp.CallToolResult, types.GetServicesOutput, error) {
    // 1. 스토리지에서 모든 서비스 조회
    services, err := h.queryService.GetServices(ctx)

    // 2. 정규식 패턴 필터 적용
    if input.Pattern != "" {
        re, _ := regexp.Compile(input.Pattern)
        filtered := make([]string, 0)
        for _, service := range services {
            if re.MatchString(service) {
                filtered = append(filtered, service)
            }
        }
        services = filtered
    }

    // 3. 정렬 후 제한 (기본 100개)
    sort.Strings(services)
    if len(services) > limit {
        services = services[:limit]
    }

    return nil, types.GetServicesOutput{Services: services}, nil
}
```

### 6.3 도구 2: get_span_names

**파일 경로**: `cmd/jaeger/internal/extension/jaegermcp/internal/handlers/get_span_names.go`

서비스의 스팬(오퍼레이션) 이름 목록을 반환한다.

```
입력: { service_name: string, pattern?: string, span_kind?: string, limit?: int }
출력: { span_names: [{ name: string, span_kind: string }] }
```

```go
func (h *getSpanNamesHandler) handle(
    ctx context.Context, _ *mcp.CallToolRequest,
    input types.GetSpanNamesInput,
) (*mcp.CallToolResult, types.GetSpanNamesOutput, error) {
    query := tracestore.OperationQueryParams{
        ServiceName: input.ServiceName,
        SpanKind:    input.SpanKind,
    }

    operations, err := h.queryService.GetOperations(ctx, query)
    // ... 정규식 필터링, 정렬, 제한 적용 ...

    spanNames := make([]types.SpanNameInfo, 0, len(filteredOps))
    for _, op := range filteredOps {
        spanNames = append(spanNames, types.SpanNameInfo{
            Name:     op.Name,
            SpanKind: op.SpanKind,
        })
    }

    return nil, types.GetSpanNamesOutput{SpanNames: spanNames}, nil
}
```

### 6.4 도구 3: health

서버 상태 확인 도구. 간단한 ping/pong 역할.

```go
func (s *server) healthTool(
    _ context.Context, _ *mcp.CallToolRequest, _ struct{},
) (*mcp.CallToolResult, HealthToolOutput, error) {
    return nil, HealthToolOutput{
        Status:  "ok",
        Server:  s.config.ServerName,
        Version: s.config.ServerVersion,
    }, nil
}
```

### 6.5 도구 4: search_traces

**파일 경로**: `cmd/jaeger/internal/extension/jaegermcp/internal/handlers/search_traces.go`

조건에 맞는 트레이스를 검색하여 **요약 정보만** 반환한다.

```
입력:
{
  service_name: string (필수),
  start_time_min?: string (기본: "-1h"),
  start_time_max?: string (기본: "now"),
  span_name?: string,
  attributes?: { key: value },
  with_errors?: bool,
  duration_min?: string,
  duration_max?: string,
  search_depth?: int (기본: 10, 최대: 100)
}

출력:
{
  traces: [{
    trace_id, root_service, root_span_name,
    start_time, duration_us, span_count,
    service_count, has_errors
  }]
}
```

핵심 구현:

```go
func (h *searchTracesHandler) handle(
    ctx context.Context, _ *mcp.CallToolRequest,
    input types.SearchTracesInput,
) (*mcp.CallToolResult, types.SearchTracesOutput, error) {
    query, err := h.buildQuery(input)
    tracesIter := h.queryService.FindTraces(ctx, query)

    // 트레이스를 완전한 단위로 집계
    aggregatedIter := jptrace.AggregateTraces(tracesIter)

    var summaries []types.TraceSummary
    for trace, err := range aggregatedIter {
        if err != nil {
            processErrs = append(processErrs, err)
            continue
        }
        summary := buildTraceSummary(trace)
        summaries = append(summaries, summary)
    }

    return nil, types.SearchTracesOutput{Traces: summaries}, nil
}
```

**시간 파라미터 파싱**:

상대 시간(`-1h`, `-30m`)과 RFC3339 절대 시간을 모두 지원한다.

```go
func parseTimeParam(input string) (time.Time, error) {
    if input == "now" {
        return time.Now(), nil
    }
    if strings.HasPrefix(input, "-") {
        duration, err := time.ParseDuration(input[1:])
        return time.Now().Add(-duration), nil
    }
    return time.Parse(time.RFC3339, input)
}
```

**TraceSummary 구성**:

```go
func buildTraceSummary(trace ptrace.Traces) types.TraceSummary {
    // 모든 스팬을 순회하며:
    // - 루트 스팬 찾기 (ParentSpanID가 비어있는 스팬)
    // - 유니크 서비스 수 카운트
    // - 전체 span 수 카운트
    // - 시작/종료 시간에서 총 duration 계산
    // - 에러 상태 스팬 존재 여부 확인
}
```

### 6.6 도구 5: get_trace_topology

**파일 경로**: `cmd/jaeger/internal/extension/jaegermcp/internal/handlers/get_trace_topology.go`

트레이스의 **구조적 트리**를 반환한다. 속성(attributes)이나 이벤트(events) 없이 부모-자식 관계, 타이밍, 에러 위치만 포함한다. 이를 통해 **토큰을 절약**한다.

```
입력: { trace_id: string, depth?: int }
출력: {
  trace_id: string,
  root_span: {
    span_id, service, span_name, start_time,
    duration_us, status, children: [...]
  },
  orphans: [...]
}
```

트리 구축 과정:

```go
func (h *getTraceTopologyHandler) buildTree(
    spans []*types.SpanNode, maxDepth int,
) (*types.SpanNode, []*types.SpanNode) {
    // 1. span_id → SpanNode 맵 생성
    spanMap := make(map[string]*types.SpanNode)
    for i := range spans {
        spanMap[spans[i].SpanID] = spans[i]
    }

    var rootSpan *types.SpanNode
    var orphans []*types.SpanNode

    // 2. 부모-자식 관계 연결
    for i := range spans {
        if spans[i].ParentID == "" {
            rootSpan = spans[i]  // 루트 스팬
        } else {
            parent, ok := spanMap[spans[i].ParentID]
            if ok {
                parent.Children = append(parent.Children, spans[i])
            } else {
                orphans = append(orphans, spans[i])  // 부모 없는 고아 스팬
            }
        }
    }

    // 3. 깊이 제한 적용
    if maxDepth > 0 && rootSpan != nil {
        h.limitDepth(rootSpan, 1, maxDepth)
    }

    return rootSpan, orphans
}
```

**깊이 제한(depth limiting)**:

```go
func (h *getTraceTopologyHandler) limitDepth(
    node *types.SpanNode, currentDepth int, maxDepth int) {
    if currentDepth >= maxDepth {
        node.TruncatedChildren = len(node.Children)
        node.Children = nil  // 자식 제거하고 잘린 수만 기록
        return
    }
    for _, child := range node.Children {
        h.limitDepth(child, currentDepth+1, maxDepth)
    }
}
```

깊이 제한은 대규모 트레이스에서 응답 크기를 제어하는 데 유용하다. `depth=2`로 설정하면 루트와 그 직접 자식만 반환하고, 더 깊은 수준은 `truncated_children` 카운트로 표시한다.

### 6.7 도구 6: get_critical_path

**파일 경로**: `cmd/jaeger/internal/extension/jaegermcp/internal/handlers/get_critical_path.go`

트레이스에서 **임계 경로(critical path)**를 식별한다. 임계 경로는 전체 트레이스 지연에 직접적으로 기여하는 블로킹 실행 경로이다.

```
입력: { trace_id: string }
출력: {
  trace_id: string,
  total_duration_us: uint64,
  critical_path_duration_us: uint64,
  segments: [{
    span_id, service, span_name,
    self_time_us, start_offset_us, end_offset_us
  }]
}
```

```go
func (h *getCriticalPathHandler) handle(
    ctx context.Context, _ *mcp.CallToolRequest,
    input types.GetCriticalPathInput,
) (*mcp.CallToolResult, types.GetCriticalPathOutput, error) {
    // 1. 트레이스 데이터 로드
    params, _ := h.buildQuery(input)
    tracesIter := h.queryService.GetTraces(ctx, params)
    aggregatedIter := jptrace.AggregateTraces(tracesIter)

    var trace ptrace.Traces
    for t, err := range aggregatedIter {
        trace = t
        break  // 단일 trace_id이므로 첫 번째만 사용
    }

    // 2. Critical Path 알고리즘 실행
    criticalPathSections, err := criticalpath.ComputeCriticalPathFromTraces(trace)

    // 3. 결과를 출력 형식으로 변환
    output := h.buildOutput(input.TraceID, trace, criticalPathSections)
    return nil, output, nil
}
```

출력의 `segments`에는 같은 스팬이 여러 번 나타날 수 있다. 자식 스팬 전후에 자체 작업이 있는 경우:

```
|----------- spanA (10ms) -----------|
    |--- spanB (3ms) ---|   |--- spanC (4ms) ---|

Critical Path:
  segment 1: spanA [0ms, 1ms]    (spanB 시작 전)
  segment 2: spanB [1ms, 4ms]    (spanB 전체)
  segment 3: spanA [4ms, 5ms]    (spanB 끝 ~ spanC 시작)
  segment 4: spanC [5ms, 9ms]    (spanC 전체)
  segment 5: spanA [9ms, 10ms]   (spanC 끝 ~ spanA 끝)
```

### 6.8 도구 7: get_span_details

**파일 경로**: `cmd/jaeger/internal/extension/jaegermcp/internal/handlers/get_span_details.go`

특정 스팬의 **전체 OTLP 데이터**(속성, 이벤트, 링크, 상태)를 반환한다. Progressive Disclosure의 마지막 단계로, 의심되는 스팬만 선별적으로 상세 조회한다.

```
입력: { trace_id: string, span_ids: string[] }
출력: {
  trace_id: string,
  spans: [{
    span_id, trace_id, parent_span_id,
    service, span_name, start_time, duration_us,
    status: { code, message },
    attributes: { key: value },
    events: [{ name, timestamp, attributes }],
    links: [{ trace_id, span_id, attributes }]
  }]
}
```

```go
func (h *getSpanDetailsHandler) handle(
    ctx context.Context, _ *mcp.CallToolRequest,
    input types.GetSpanDetailsInput,
) (*mcp.CallToolResult, types.GetSpanDetailsOutput, error) {
    // 요청된 span_id를 Set으로 변환하여 O(1) 룩업
    spanIDSet := make(map[string]struct{}, len(input.SpanIDs))
    for _, spanID := range input.SpanIDs {
        spanIDSet[spanID] = struct{}{}
    }

    tracesIter := h.queryService.GetTraces(ctx, params)
    aggregatedIter := jptrace.AggregateTraces(tracesIter)

    var spanDetails []types.SpanDetail
    for trace, err := range aggregatedIter {
        for pos, span := range jptrace.SpanIter(trace) {
            spanIDStr := span.SpanID().String()
            if _, found := spanIDSet[spanIDStr]; found {
                detail := buildSpanDetail(pos, span)
                spanDetails = append(spanDetails, detail)
                delete(spanIDSet, spanIDStr)  // 찾은 것은 제거
            }
        }
    }

    // 찾지 못한 span_id가 있으면 에러 메시지에 포함
    output := types.GetSpanDetailsOutput{TraceID: input.TraceID, Spans: spanDetails}
    if len(spanIDSet) > 0 {
        output.Error = fmt.Sprintf("spans not found: %v", missingIDs)
    }

    return nil, output, nil
}
```

**속성 변환**:

```go
func attributesToMap(attrs pcommon.Map) map[string]any {
    result := make(map[string]any)
    for k, v := range attrs.All() {
        result[k] = convertAttributeValue(v)
    }
    return result
}

func convertAttributeValue(v pcommon.Value) any {
    switch v.Type() {
    case pcommon.ValueTypeStr:    return v.Str()
    case pcommon.ValueTypeInt:    return v.Int()
    case pcommon.ValueTypeDouble: return v.Double()
    case pcommon.ValueTypeBool:   return v.Bool()
    case pcommon.ValueTypeBytes:  return v.Bytes().AsRaw()
    case pcommon.ValueTypeSlice:  // 재귀적 변환
    case pcommon.ValueTypeMap:    // 재귀적 변환
    default:                     return nil
    }
}
```

### 6.9 도구 8: get_trace_errors

**파일 경로**: `cmd/jaeger/internal/extension/jaegermcp/internal/handlers/get_trace_errors.go`

트레이스에서 **에러 상태인 스팬만** 추출하여 전체 상세 정보를 반환한다.

```
입력: { trace_id: string }
출력: {
  trace_id: string,
  error_count: int,
  spans: [SpanDetail]  // get_span_details와 동일한 형식
}
```

```go
func (h *getTraceErrorsHandler) handle(
    ctx context.Context, _ *mcp.CallToolRequest,
    input types.GetTraceErrorsInput,
) (*mcp.CallToolResult, types.GetTraceErrorsOutput, error) {
    tracesIter := h.queryService.GetTraces(ctx, params)
    aggregatedIter := jptrace.AggregateTraces(tracesIter)

    var errorSpans []types.SpanDetail
    for trace, err := range aggregatedIter {
        for pos, span := range jptrace.SpanIter(trace) {
            // 에러 상태인 스팬만 선별
            if span.Status().Code() == ptrace.StatusCodeError {
                detail := buildSpanDetail(pos, span)
                errorSpans = append(errorSpans, detail)
            }
        }
    }

    return nil, types.GetTraceErrorsOutput{
        TraceID:    input.TraceID,
        ErrorCount: len(errorSpans),
        Spans:      errorSpans,
    }, nil
}
```

**왜 별도 도구로 만들었는가?**

`get_span_details`로도 에러 스팬을 조회할 수 있지만, 그러려면 먼저 `get_trace_topology`에서 에러 스팬을 식별해야 한다. `get_trace_errors`는 이 과정을 하나의 호출로 단축하여 LLM의 도구 호출 횟수를 줄인다.

---

## 7. Critical Path 알고리즘

### 7.1 배경

Critical Path(임계 경로) 알고리즘은 원래 Jaeger UI의 TypeScript 코드에만 존재했다. MCP 서버를 위해 Go로 포팅되었다. 이 알고리즘은 분산 트레이스에서 전체 지연에 **직접적으로 기여하는** 스팬 구간을 식별한다.

**파일 경로**: `cmd/jaeger/internal/extension/jaegermcp/internal/criticalpath/`

### 7.2 CPSpan 구조체

**파일 경로**: `cmd/jaeger/internal/extension/jaegermcp/internal/criticalpath/cpspan.go`

원본 `ptrace.Span`을 수정하지 않기 위해 별도의 경량 구조체를 사용한다.

```go
type CPSpan struct {
    SpanID       pcommon.SpanID
    StartTime    uint64               // 마이크로초 단위
    Duration     uint64               // 마이크로초 단위
    References   []CPSpanReference
    ChildSpanIDs []pcommon.SpanID
}

type CPSpanReference struct {
    RefType string          // "CHILD_OF" 또는 "FOLLOWS_FROM"
    SpanID  pcommon.SpanID
    TraceID pcommon.TraceID
    Span    *CPSpan         // sanitize 단계에서 채워짐
}
```

**왜 별도 구조체를 사용하는가?**

1. `ptrace.Span`은 원본 트레이스 데이터이므로 수정하면 안 된다
2. sanitize 과정에서 StartTime/Duration을 조정해야 한다
3. 부모-자식 관계를 양방향으로 탐색해야 한다 (ChildSpanIDs 필드)

### 7.3 CPSpan 맵 생성

```go
func CreateCPSpanMap(traces ptrace.Traces) map[pcommon.SpanID]CPSpan {
    spanMap := make(map[pcommon.SpanID]CPSpan)
    childrenMap := make(map[pcommon.SpanID][]pcommon.SpanID)

    // 1단계: 자식 맵 구축
    for i := 0; i < traces.ResourceSpans().Len(); i++ {
        rs := traces.ResourceSpans().At(i)
        for j := 0; j < rs.ScopeSpans().Len(); j++ {
            ss := rs.ScopeSpans().At(j)
            for k := 0; k < ss.Spans().Len(); k++ {
                span := ss.Spans().At(k)
                if !span.ParentSpanID().IsEmpty() {
                    parentID := span.ParentSpanID()
                    childrenMap[parentID] = append(childrenMap[parentID], span.SpanID())
                }
            }
        }
    }

    // 2단계: CPSpan 객체 생성 (자식 관계 포함)
    // ... 두 번째 패스에서 각 스팬을 CPSpan으로 변환 ...

    return spanMap
}
```

### 7.4 전처리 단계

Critical Path 계산 전에 두 가지 전처리가 수행된다.

#### 7.4.1 FOLLOWS_FROM 스팬 제거

**파일 경로**: `cmd/jaeger/internal/extension/jaegermcp/internal/criticalpath/get_child_of_spans.go`

```go
func getChildOfSpans(spanMap map[pcommon.SpanID]CPSpan) map[pcommon.SpanID]CPSpan {
    var followFromSpanIDs []pcommon.SpanID
    var followFromSpansDescendantIDs []pcommon.SpanID

    // FOLLOWS_FROM 관계의 스팬 찾기
    for spanID, span := range spanMap {
        if len(span.References) > 0 && span.References[0].RefType == "FOLLOWS_FROM" {
            followFromSpanIDs = append(followFromSpanIDs, spanID)
            // 부모 스팬의 ChildSpanIDs에서 제거
        }
    }

    // FOLLOWS_FROM 스팬의 자손도 재귀적으로 수집하여 삭제
    findDescendantSpans(followFromSpanIDs)

    // 모두 삭제
    for _, id := range idsToBeDeleted {
        delete(spanMap, id)
    }

    return spanMap
}
```

**왜 FOLLOWS_FROM을 제거하는가?**

FOLLOWS_FROM 관계의 스팬은 부모의 실행을 **블로킹하지 않는다**. 비동기 작업이나 후속 처리를 나타내므로, 임계 경로 계산에서 제외한다.

```
CHILD_OF: 부모가 자식의 완료를 기다림 → 임계 경로에 포함
FOLLOWS_FROM: 자식이 부모와 독립적으로 실행 → 임계 경로에서 제외

|---- parentSpan ----|
  |-- childOf --|        ← 포함 (부모 블로킹)
                   |-- followsFrom --|   ← 제외 (비동기)
```

#### 7.4.2 오버플로우 자식 스팬 정리

**파일 경로**: `cmd/jaeger/internal/extension/jaegermcp/internal/criticalpath/sanitize.go`

클록 스큐(clock skew)로 인해 자식 스팬이 부모의 시간 범위를 벗어나는 경우를 처리한다.

```go
func removeOverflowingChildren(
    spanMap map[pcommon.SpanID]CPSpan,
) map[pcommon.SpanID]CPSpan {
    for _, spanID := range spanIDs {
        span := spanMap[spanID]
        if len(span.References) == 0 { continue }

        parentSpan := spanMap[span.References[0].SpanID]
        childEndTime := span.StartTime + span.Duration
        parentEndTime := parentSpan.StartTime + parentSpan.Duration

        // 케이스 1: 자식이 부모 범위 밖에 있음 → 삭제
        //      |----parent----|
        //                        |----child--|
        if span.StartTime >= parentEndTime {
            delete(spanMap, span.SpanID)
            continue
        }

        // 케이스 2: 자식이 부모 끝을 넘어감 → 자르기
        //      |----parent----|
        //              |----child--|
        if childEndTime > parentEndTime {
            span.Duration = parentEndTime - span.StartTime
            spanMap[span.SpanID] = span
            continue
        }

        // 케이스 3: 자식이 부모 시작 전에 시작 → 자르기
        //      |----parent----|
        //   |----child--|
        if span.StartTime < parentSpan.StartTime {
            span.StartTime = parentSpan.StartTime
            span.Duration = childEndTime - parentSpan.StartTime
            spanMap[span.SpanID] = span
        }
    }
    return spanMap
}
```

시각적으로:

```
정상:                    자식이 부모 밖:         자식이 부모를 넘어감:
|----parent----|        |----parent----|       |----parent----|
  |--child--|                            |--child--|
                                       →        |--child|  (잘림)

자식이 부모 전에 시작:   자식이 부모를 감싸:
   |----parent----|     |----parent----|
 |----child--|          |--------child---------|
→  |--child--|  (잘림)  →|----child----|  (잘림)
```

### 7.5 핵심 알고리즘: computeCriticalPath

**파일 경로**: `cmd/jaeger/internal/extension/jaegermcp/internal/criticalpath/criticalpath.go`

```go
func computeCriticalPath(
    spanMap map[pcommon.SpanID]CPSpan,
    spanID pcommon.SpanID,
    criticalPath []Section,
    returningChildStartTime *uint64,
) []Section {
    currentSpan := spanMap[spanID]

    // 1. Last Finishing Child (LFC) 찾기
    lastFinishingChildSpan := findLastFinishingChildSpan(
        spanMap, currentSpan, returningChildStartTime)

    if lastFinishingChildSpan != nil {
        // LFC가 있는 경우:
        // 현재 스팬의 끝 ~ LFC의 끝 구간을 critical path에 추가
        endTime := currentSpan.StartTime + currentSpan.Duration
        if returningChildStartTime != nil {
            endTime = *returningChildStartTime
        }

        section := Section{
            SpanID:       currentSpan.SpanID.String(),
            SectionStart: lastFinishingChildSpan.StartTime + lastFinishingChildSpan.Duration,
            SectionEnd:   endTime,
        }
        if section.SectionStart != section.SectionEnd {
            criticalPath = append(criticalPath, section)
        }

        // LFC로 재귀 → 다시 LFC 찾기
        criticalPath = computeCriticalPath(
            spanMap, lastFinishingChildSpan.SpanID, criticalPath, nil)
    } else {
        // LFC가 없는 경우 (리프 노드):
        // 현재 스팬의 전체 구간을 critical path에 추가
        endTime := currentSpan.StartTime + currentSpan.Duration
        if returningChildStartTime != nil {
            endTime = *returningChildStartTime
        }

        section := Section{
            SpanID:       currentSpan.SpanID.String(),
            SectionStart: currentSpan.StartTime,
            SectionEnd:   endTime,
        }
        if section.SectionStart != section.SectionEnd {
            criticalPath = append(criticalPath, section)
        }

        // 부모로 돌아가면서 이전 형제 스팬 찾기
        if len(currentSpan.References) > 0 {
            parentSpanID := currentSpan.References[0].SpanID
            criticalPath = computeCriticalPath(
                spanMap, parentSpanID, criticalPath, &currentSpan.StartTime)
        }
    }

    return criticalPath
}
```

### 7.6 알고리즘 시각화

```
예시 트레이스:
|-------------spanA (0-10)--------------|
   |--spanB (1-4)--|    |--spanC (5-9)--|

알고리즘 실행:

1. spanA에서 시작
   - LFC 찾기 → spanC (끝=9, 가장 늦게 끝남)
   - spanA의 [9, 10] 구간 = critical path (spanC 끝 ~ spanA 끝)
   - spanC로 재귀

2. spanC로 이동
   - LFC 찾기 → 없음 (자식 없음)
   - spanC의 [5, 9] 구간 = critical path (spanC 전체)
   - 부모(spanA)로 복귀, returningChildStartTime = 5

3. spanA로 복귀 (returningChildStartTime = 5)
   - LFC 찾기 (끝 < 5인 자식) → spanB (끝=4)
   - spanA의 [4, 5] 구간 = critical path (spanB 끝 ~ spanC 시작)
   - spanB로 재귀

4. spanB로 이동
   - LFC 찾기 → 없음
   - spanB의 [1, 4] 구간 = critical path
   - 부모(spanA)로 복귀, returningChildStartTime = 1

5. spanA로 복귀 (returningChildStartTime = 1)
   - LFC 찾기 (끝 < 1인 자식) → 없음
   - spanA의 [0, 1] 구간 = critical path (spanA 시작 ~ spanB 시작)
   - 부모 없음 → 종료

결과 (역순):
  spanA [9,10] → spanC [5,9] → spanA [4,5] → spanB [1,4] → spanA [0,1]

시각화:
|A|--spanB--|A|----spanC----|A|
 ^          ^ ^              ^ ^
 |          | |              | |
 0          4 5              9 10

총 critical path = 10ms = trace 전체 duration
```

### 7.7 findLastFinishingChildSpan

**파일 경로**: `cmd/jaeger/internal/extension/jaegermcp/internal/criticalpath/find_lfc.go`

```go
func findLastFinishingChildSpan(
    spanMap map[pcommon.SpanID]CPSpan,
    currentSpan CPSpan,
    returningChildStartTime *uint64,
) *CPSpan {
    var lastFinishingChildSpan *CPSpan
    var maxEndTime uint64 = 0

    for _, childID := range currentSpan.ChildSpanIDs {
        childSpan := spanMap[childID]
        childEndTime := childSpan.StartTime + childSpan.Duration

        if returningChildStartTime != nil {
            // 복귀 시: returningChildStartTime 이전에 끝나는 자식 중 가장 늦게 끝나는 것
            if childEndTime < *returningChildStartTime {
                if childEndTime > maxEndTime {
                    maxEndTime = childEndTime
                    childSpanCopy := childSpan
                    lastFinishingChildSpan = &childSpanCopy
                }
            }
        } else {
            // 최초 호출: 가장 늦게 끝나는 자식
            if childEndTime > maxEndTime {
                maxEndTime = childEndTime
                childSpanCopy := childSpan
                lastFinishingChildSpan = &childSpanCopy
            }
        }
    }

    return lastFinishingChildSpan
}
```

### 7.8 전체 계산 흐름

```go
func ComputeCriticalPathFromTraces(traces ptrace.Traces) ([]Section, error) {
    // 1. 루트 스팬 찾기
    var rootSpanID pcommon.SpanID
    // ... 모든 스팬을 순회하여 ParentSpanID가 비어있는 스팬 찾기 ...

    // 2. CPSpan 맵 생성
    spanMap := CreateCPSpanMap(traces)

    // 3. 전처리
    refinedSpanMap := getChildOfSpans(spanMap)        // FOLLOWS_FROM 제거
    sanitizedSpanMap := removeOverflowingChildren(refinedSpanMap)  // 오버플로우 정리

    // 4. Critical Path 계산
    var criticalPath []Section
    criticalPath = computeCriticalPath(sanitizedSpanMap, rootSpanID, criticalPath, nil)

    return criticalPath, nil
}
```

---

## 8. 타입 시스템

### 8.1 입출력 타입 개요

각 MCP 도구는 강타입(strongly-typed) 입출력 구조체를 가진다. `jsonschema` 태그로 MCP SDK가 자동으로 JSON Schema를 생성한다.

**파일 경로**: `cmd/jaeger/internal/extension/jaegermcp/internal/types/`

```
types/
├── get_services.go        # GetServicesInput, GetServicesOutput
├── get_span_names.go      # GetSpanNamesInput, GetSpanNamesOutput, SpanNameInfo
├── search_traces.go       # SearchTracesInput, SearchTracesOutput, TraceSummary
├── get_trace_topology.go  # GetTraceTopologyInput, GetTraceTopologyOutput, SpanNode
├── get_critical_path.go   # GetCriticalPathInput, GetCriticalPathOutput, CriticalPathSegment
├── get_span_details.go    # GetSpanDetailsInput, GetSpanDetailsOutput, SpanDetail, SpanStatus, SpanEvent, SpanLink
└── get_trace_errors.go    # GetTraceErrorsInput, GetTraceErrorsOutput
```

### 8.2 핵심 타입

#### TraceSummary

```go
type TraceSummary struct {
    TraceID      string `json:"trace_id"`
    RootService  string `json:"root_service"`
    RootSpanName string `json:"root_span_name"`
    StartTime    string `json:"start_time"`
    DurationUs   int64  `json:"duration_us"`
    SpanCount    int    `json:"span_count"`
    ServiceCount int    `json:"service_count"`
    HasErrors    bool   `json:"has_errors"`
}
```

#### SpanNode (토폴로지 트리용)

```go
type SpanNode struct {
    SpanID            string      `json:"span_id"`
    ParentID          string      `json:"parent_id,omitempty"`
    Service           string      `json:"service"`
    SpanName          string      `json:"span_name"`
    StartTime         string      `json:"start_time"`
    DurationUs        int64       `json:"duration_us"`
    Status            string      `json:"status"`
    Children          []*SpanNode `json:"children,omitempty" jsonschema:"-"`
    TruncatedChildren int         `json:"truncated_children,omitempty"`
}
```

**`jsonschema:"-"` 태그**: `Children` 필드는 재귀적 타입(`*SpanNode`)이므로, MCP SDK의 JSON Schema 생성기가 순환 참조를 감지한다. `jsonschema:"-"`로 스키마 생성에서 제외하되, 런타임에는 정상적으로 직렬화된다.

#### SpanDetail (전체 OTLP 데이터)

```go
type SpanDetail struct {
    SpanID       string         `json:"span_id"`
    TraceID      string         `json:"trace_id"`
    ParentSpanID string         `json:"parent_span_id,omitempty"`
    Service      string         `json:"service"`
    SpanName     string         `json:"span_name"`
    StartTime    string         `json:"start_time"`
    DurationUs   int64          `json:"duration_us"`
    Status       SpanStatus     `json:"status"`
    Attributes   map[string]any `json:"attributes,omitempty"`
    Events       []SpanEvent    `json:"events,omitempty"`
    Links        []SpanLink     `json:"links,omitempty"`
}
```

#### CriticalPathSegment

```go
type CriticalPathSegment struct {
    SpanID        string `json:"span_id"`
    Service       string `json:"service"`
    SpanName      string `json:"span_name"`
    SelfTimeUs    uint64 `json:"self_time_us"`
    StartOffsetUs uint64 `json:"start_offset_us"`
    EndOffsetUs   uint64 `json:"end_offset_us"`
}
```

### 8.3 MCP SDK 핸들러 시그니처

```go
// mcp.ToolHandlerFor[Input, Output] 타입을 사용
func NewSearchTracesHandler(
    queryService *querysvc.QueryService,
) mcp.ToolHandlerFor[types.SearchTracesInput, types.SearchTracesOutput] {
    h := &searchTracesHandler{queryService: queryService}
    return h.handle
}

// 핸들러 함수 시그니처
func (h *searchTracesHandler) handle(
    ctx context.Context,
    _ *mcp.CallToolRequest,
    input types.SearchTracesInput,
) (*mcp.CallToolResult, types.SearchTracesOutput, error) {
    // ...
}
```

MCP SDK는 `ToolHandlerFor[Input, Output]` 제네릭 타입을 사용하여 입출력의 JSON Schema를 자동 생성하고, 입력 역직렬화와 출력 직렬화를 처리한다.

---

## 9. 설정 및 운영

### 9.1 기본 설정

```yaml
extensions:
  jaeger_mcp:
    http:
      endpoint: "localhost:16687"
    server_name: jaeger
    server_version: dev
    max_span_details_per_request: 20
    max_search_results: 100
```

### 9.2 프로덕션 설정

```yaml
extensions:
  jaeger_mcp:
    http:
      endpoint: "0.0.0.0:16687"
      # TLS 설정 (선택)
      # tls:
      #   cert_file: /path/to/cert.pem
      #   key_file: /path/to/key.pem
    server_name: jaeger-prod
    max_span_details_per_request: 50
    max_search_results: 200
```

### 9.3 LLM 연결 설정

MCP 클라이언트(Claude, GPT 등)에서 Jaeger MCP 서버에 연결하려면:

```
MCP 서버 URL: http://localhost:16687/mcp
프로토콜: Streamable HTTP (SSE)
```

### 9.4 포트 참조

**파일 경로**: `ports/ports.go`

```go
const (
    MCPHTTP = 16687  // MCP 서버 기본 포트
)
```

### 9.5 헬스 체크

```
GET http://localhost:16687/health
→ 200 OK, "MCP server is running"
```

---

## 10. 설계 결정과 트레이드오프

### 10.1 Extension으로 구현한 이유

MCP 서버를 OTel Collector의 Extension으로 구현한 이유:

```
대안 1: 별도 바이너리
  장점: 독립적 배포/스케일링
  단점: 별도 인프라, Jaeger 스토리지 접근 복잡

대안 2: jaeger_query Extension 내부
  장점: 코드 공유, 추가 포트 불필요
  단점: 관심사 혼합, MCP 관련 의존성이 쿼리에 영향

선택: 별도 Extension ✓
  장점: 깔끔한 관심사 분리, 독립적 설정, 별도 포트
  단점: Extension 간 의존성 필요 (Dependencies() 메서드로 해결)
```

### 10.2 Critical Path 알고리즘 포팅

기존에 TypeScript(Jaeger UI)에만 있던 알고리즘을 Go로 포팅했다.

```
장점:
  - 서버 사이드에서 계산 → LLM이 결과만 받음
  - Go API로 노출 → MCP 외에도 활용 가능
  - TypeScript 의존성 없이 동작

단점:
  - 코드 중복 (TypeScript + Go)
  - 두 구현체 동기화 필요

향후 계획:
  - Go 구현을 gRPC 쿼리 API로도 노출
  - TypeScript 구현은 Go API 호출로 대체 가능
```

### 10.3 SSE vs JSON Response

```go
mcpHandler := mcp.NewStreamableHTTPHandler(
    func(_ *http.Request) *mcp.Server { return s.mcpServer },
    &mcp.StreamableHTTPOptions{
        JSONResponse: false,  // SSE 사용
    },
)
```

SSE(Server-Sent Events)를 사용하면 LLM 클라이언트가 실시간으로 중간 결과를 받을 수 있다. 대규모 검색 결과나 복잡한 토폴로지를 반환할 때 유용하다.

### 10.4 GetTraceTopologyOutput에서 any 타입 사용

```go
type GetTraceTopologyOutput struct {
    TraceID  string `json:"trace_id"`
    RootSpan any    `json:"root_span,omitempty"`   // *SpanNode 대신 any
    Orphans  any    `json:"orphans,omitempty"`      // []*SpanNode 대신 any
}
```

**왜 `any`를 사용하는가?**

`SpanNode`는 `Children []*SpanNode` 필드를 가져 재귀적 타입이다. MCP Go SDK의 JSON Schema 생성기가 이를 순환 참조로 감지하여 에러를 발생시킨다. `any`로 선언하면 타입 분석을 우회하면서도 런타임에는 `*SpanNode`가 정상적으로 JSON 직렬화된다.

---

## 11. 실전 사용 시나리오

### 11.1 에러 디버깅 워크플로우

LLM이 Jaeger MCP 서버를 사용하여 프로덕션 에러를 디버깅하는 시나리오:

```
사용자: "checkout 서비스에서 최근 1시간 동안 발생한 에러를 분석해줘"

LLM 동작:
  ┌─────────────────────────────────────────────────────────┐
  │ 1. get_services(pattern="checkout")                     │
  │    → ["checkout-service"]                               │
  │                                                         │
  │ 2. search_traces(                                       │
  │      service_name="checkout-service",                   │
  │      start_time_min="-1h",                              │
  │      with_errors=true,                                  │
  │      search_depth=20                                    │
  │    )                                                    │
  │    → [trace_id: "abc123", duration: 5000ms, errors: Y]  │
  │    → [trace_id: "def456", duration: 3200ms, errors: Y]  │
  │                                                         │
  │ 3. get_trace_topology(trace_id="abc123")                │
  │    → 트리 구조에서 ERROR 상태 스팬 위치 파악              │
  │      frontend → checkout → payment(ERROR) → gateway(ERR)│
  │                                                         │
  │ 4. get_trace_errors(trace_id="abc123")                  │
  │    → payment: "Upstream service timeout"                │
  │    → gateway: "Connection timeout to payment processor"  │
  │                                                         │
  │ 5. get_span_details(                                    │
  │      trace_id="abc123",                                 │
  │      span_ids=["payment_span", "gateway_span"]          │
  │    )                                                    │
  │    → 전체 속성 확인:                                     │
  │      http.status_code=504, retry.count=3,               │
  │      net.peer.name="payment-db.internal"                │
  └─────────────────────────────────────────────────────────┘

LLM 분석 결과:
  "payment-gateway에서 payment-db.internal로의 연결 타임아웃이
   3회 재시도 후 504 에러로 이어졌습니다. 데이터베이스 서버의
   상태를 확인하거나 연결 타임아웃 설정을 검토하세요."
```

### 11.2 레이턴시 분석 워크플로우

```
사용자: "API 응답이 느려졌어. 어디서 시간이 걸리는지 분석해줘"

LLM 동작:
  1. search_traces(service_name="api-gateway", duration_min="2s")
     → 느린 트레이스 목록

  2. get_critical_path(trace_id="slow_trace_123")
     → segments:
        api-gateway: 50ms
        auth-service: 100ms
        catalog-service: 1800ms  ← 병목!
        api-gateway: 50ms

  3. get_span_details(trace_id="slow_trace_123",
                       span_ids=["catalog_span"])
     → db.statement: "SELECT * FROM products WHERE ..."
     → db.rows_affected: 50000

LLM 분석:
  "catalog-service의 데이터베이스 쿼리가 전체 지연의 90%를
   차지합니다. 50,000행을 반환하는 SELECT 쿼리의 인덱싱과
   페이지네이션을 검토하세요."
```

---

## 12. 정리

### 12.1 핵심 설계 원칙

| 원칙 | 구현 방식 |
|------|----------|
| **Progressive Disclosure** | 4단계 워크플로우: Search → Map → Diagnose → Inspect |
| **토큰 효율성** | 토폴로지는 속성 없이, 상세는 선택된 스팬만 |
| **표준 준수** | MCP 프로토콜, SSE 전송, JSON Schema |
| **깔끔한 의존성** | Extension → Extension 패턴, Dependencies() 메서드 |
| **원본 데이터 보호** | CPSpan으로 복사 후 수정, ptrace.Span은 불변 |
| **알고리즘 재사용** | Critical Path를 Go 패키지로 분리, MCP 외에도 활용 가능 |

### 12.2 도구 요약 테이블

| 도구 | 용도 | 단계 | 토큰 비용 |
|------|------|------|----------|
| `get_services` | 서비스 목록 조회 | 1. Search | 매우 낮음 |
| `get_span_names` | 스팬 이름 조회 | 1. Search | 매우 낮음 |
| `health` | 서버 상태 확인 | - | 최소 |
| `search_traces` | 트레이스 검색 (요약만) | 1. Search | 낮음 |
| `get_trace_topology` | 트리 구조 (속성 없음) | 2. Map | 낮음~중간 |
| `get_critical_path` | 임계 경로 식별 | 3. Diagnose | 낮음 |
| `get_trace_errors` | 에러 스팬 전체 상세 | 3. Diagnose | 중간 |
| `get_span_details` | 선택 스팬 전체 상세 | 4. Inspect | 높음 (제한적) |

### 12.3 관련 소스 파일 전체 목록

```
cmd/jaeger/internal/extension/jaegermcp/
├── config.go                                  # Config 구조체, 검증
├── factory.go                                 # Extension 팩토리, 기본 설정
├── server.go                                  # 서버 라이프사이클, 도구 등록, CORS
└── internal/
    ├── criticalpath/
    │   ├── cpspan.go                          # CPSpan/CPSpanReference, CreateCPSpanMap
    │   ├── criticalpath.go                    # computeCriticalPath, ComputeCriticalPathFromTraces
    │   ├── find_lfc.go                        # findLastFinishingChildSpan
    │   ├── get_child_of_spans.go              # getChildOfSpans (FOLLOWS_FROM 제거)
    │   └── sanitize.go                        # removeOverflowingChildren
    ├── handlers/
    │   ├── get_services.go                    # get_services 핸들러
    │   ├── get_span_names.go                  # get_span_names 핸들러
    │   ├── search_traces.go                   # search_traces 핸들러, parseTimeParam
    │   ├── get_trace_topology.go              # get_trace_topology 핸들러, buildTree
    │   ├── get_critical_path.go               # get_critical_path 핸들러
    │   ├── get_span_details.go                # get_span_details 핸들러, attributesToMap
    │   └── get_trace_errors.go                # get_trace_errors 핸들러
    └── types/
        ├── get_services.go                    # GetServicesInput/Output
        ├── get_span_names.go                  # GetSpanNamesInput/Output, SpanNameInfo
        ├── search_traces.go                   # SearchTracesInput/Output, TraceSummary
        ├── get_trace_topology.go              # GetTraceTopologyInput/Output, SpanNode
        ├── get_critical_path.go               # GetCriticalPathInput/Output, CriticalPathSegment
        ├── get_span_details.go                # GetSpanDetailsInput/Output, SpanDetail, SpanStatus, SpanEvent, SpanLink
        └── get_trace_errors.go                # GetTraceErrorsInput/Output

docs/adr/002-mcp-server.md                     # Architecture Decision Record
ports/ports.go                                  # MCPHTTP = 16687
```
