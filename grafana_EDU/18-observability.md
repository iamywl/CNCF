# Grafana 옵저버빌리티 심화

## 목차

1. [개요](#1-개요)
2. [Prometheus 메트릭](#2-prometheus-메트릭)
3. [HTTP 요청 메트릭 상세](#3-http-요청-메트릭-상세)
4. [플러그인 메트릭](#4-플러그인-메트릭)
5. [알림 메트릭](#5-알림-메트릭)
6. [SSE 표현식 메트릭](#6-sse-표현식-메트릭)
7. [/metrics 엔드포인트](#7-metrics-엔드포인트)
8. [OpenTelemetry 트레이싱](#8-opentelemetry-트레이싱)
9. [요청 트레이싱 미들웨어](#9-요청-트레이싱-미들웨어)
10. [플러그인 트레이싱](#10-플러그인-트레이싱)
11. [표현식 트레이싱](#11-표현식-트레이싱)
12. [구조화된 로깅](#12-구조화된-로깅)
13. [Health 엔드포인트](#13-health-엔드포인트)
14. [에러 추적과 에러 소스](#14-에러-추적과-에러-소스)
15. [프로파일링](#15-프로파일링)
16. [Grafana Live (실시간 업데이트)](#16-grafana-live-실시간-업데이트)
17. [운영 모니터링 구성 예시](#17-운영-모니터링-구성-예시)

---

## 1. 개요

Grafana 자체도 모니터링 대상이다. Grafana는 자기 자신의 동작 상태를 관찰할 수 있도록
Prometheus 메트릭, OpenTelemetry 트레이싱, 구조화된 로깅, Health 체크 엔드포인트 등
포괄적인 옵저버빌리티 기능을 내장하고 있다.

```
┌──────────────────────────────────────────────────────────────┐
│                Grafana 옵저버빌리티 스택                       │
│                                                              │
│  ┌──────────────────────────────────────────────────────┐   │
│  │                  Prometheus 메트릭                     │   │
│  │  /metrics 엔드포인트                                   │   │
│  │  - HTTP 요청 히스토그램                                │   │
│  │  - 플러그인 요청 카운터                                │   │
│  │  - 알림 상태 카운터                                    │   │
│  │  - DB 쿼리 메트릭                                     │   │
│  └──────────────────────────────────────────────────────┘   │
│                                                              │
│  ┌──────────────────────────────────────────────────────┐   │
│  │               OpenTelemetry 트레이싱                   │   │
│  │  - 요청별 스팬 생성                                    │   │
│  │  - 플러그인 호출 추적                                  │   │
│  │  - SSE 파이프라인 추적                                 │   │
│  │  - grafana-trace-id 헤더                               │   │
│  └──────────────────────────────────────────────────────┘   │
│                                                              │
│  ┌──────────────────────────────────────────────────────┐   │
│  │                  구조화된 로깅                          │   │
│  │  - JSON 또는 콘솔 출력                                 │   │
│  │  - 컨텍스트 기반 로거                                  │   │
│  │  - 필터링 가능                                        │   │
│  └──────────────────────────────────────────────────────┘   │
│                                                              │
│  ┌──────────────────────────────────────────────────────┐   │
│  │                Health 체크                             │   │
│  │  /healthz - 항상 200 반환 (웹서버 살아있음)             │   │
│  │  /api/health - DB 연결 상태 포함                       │   │
│  └──────────────────────────────────────────────────────┘   │
│                                                              │
└──────────────────────────────────────────────────────────────┘
```

---

## 2. Prometheus 메트릭

### 메트릭 네임스페이스

모든 Grafana 메트릭은 `grafana` 네임스페이스를 사용한다:

```go
// pkg/infra/metrics/metrics.go
const ExporterName = "grafana"
```

### 주요 메트릭 카테고리

| 카테고리 | 접두사 | 설명 |
|----------|--------|------|
| HTTP 요청 | `grafana_http_*` | 요청 시간, 크기, 상태 코드 |
| API 상태 | `grafana_api_*` | API 응답 상태별 카운터 |
| 대시보드 | `grafana_api_dashboard_*` | 대시보드 CRUD 시간 |
| 데이터소스 | `grafana_api_dataproxy_*` | 데이터소스 프록시 요청 |
| 알림 | `grafana_alerting_*` | 알림 실행, 상태, 발송 |
| 렌더링 | `grafana_rendering_*` | 이미지 렌더링 요청 |
| 인증 | `grafana_api_login_*` | 로그인 카운터 |
| RBAC | `grafana_access_*` | 접근 제어 평가 |
| SSE | `grafana_sse_*` | 서버사이드 표현식 |
| 통합 스토리지 | `grafana_unified_*` | K8s 스타일 스토리지 |

---

## 3. HTTP 요청 메트릭 상세

### RequestMetrics 미들웨어

`pkg/middleware/request_metrics.go`에 구현된 HTTP 요청 메트릭 미들웨어:

```go
func RequestMetrics(features featuremgmt.FeatureToggles, cfg *setting.Cfg,
    promRegister prometheus.Registerer) web.Middleware {

    // In-flight 게이지
    httpRequestsInFlight := prometheus.NewGauge(prometheus.GaugeOpts{
        Namespace: "grafana",
        Name:      "http_request_in_flight",
        Help:      "A gauge of requests currently being served by Grafana.",
    })

    // 히스토그램 라벨
    histogramLabels := []string{
        "handler",        // 라우트 핸들러 이름
        "status_code",    // HTTP 상태 코드
        "method",         // HTTP 메서드 (GET, POST, ...)
        "status_source",  // 에러 소스 (server, plugin, downstream)
        "slo_group",      // SLO 그룹 분류
    }
    // grafana_team 라벨은 선택적
    if cfg.MetricsIncludeTeamLabel {
        histogramLabels = append(histogramLabels, "grafana_team")
    }

    // ...
}
```

### 요청 시간 히스토그램

```go
httpRequestDurationHistogram := prometheus.NewHistogramVec(
    prometheus.HistogramOpts{
        Namespace: "grafana",
        Name:      "http_request_duration_seconds",
        Help:      "Histogram of latencies for HTTP requests.",
        Buckets:   []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 25},
        // Native Histogram 설정 (고효율)
        NativeHistogramBucketFactor:    1.1,
        NativeHistogramMaxBucketNumber: 160,
        NativeHistogramMinResetDuration: time.Hour,
    },
    histogramLabels,
)
```

### 히스토그램 버킷 분석

| 버킷 (초) | 의미 |
|-----------|------|
| 0.005 | 5ms 이하 (매우 빠른 응답) |
| 0.01 | 10ms 이하 |
| 0.025 | 25ms 이하 |
| 0.05 | 50ms 이하 |
| 0.1 | 100ms 이하 |
| 0.25 | 250ms 이하 |
| 0.5 | 500ms 이하 |
| 1 | 1초 이하 |
| 2.5 | 2.5초 이하 |
| 5 | 5초 이하 |
| 10 | 10초 이하 |
| 25 | 25초 이하 (가장 느린 버킷) |

### Native Histogram

Grafana는 Prometheus Native Histogram을 지원한다.
`ClassicHTTPHistogramEnabled` 설정으로 클래식 히스토그램을 비활성화하고
네이티브 히스토그램만 노출할 수 있다:

```go
if !cfg.ClassicHTTPHistogramEnabled {
    // 클래식 히스토그램 비활성화 → 카디널리티 감소
    reqDurationOptions.Buckets = nil
    reqSizeOptions.Buckets = nil
}
```

### 응답 크기 히스토그램

```go
httpRequestSizeHistogram := prometheus.NewHistogramVec(
    prometheus.HistogramOpts{
        Namespace: "grafana",
        Name:      "http_response_size_bytes",
        Help:      "Histogram of request sizes for HTTP requests.",
        Buckets:   prometheus.ExponentialBuckets(128, 2, 16),
        // 128, 256, 512, 1024, ..., 4,194,304 (4MB)
    },
    histogramLabels,
)
```

### 응답 크기 버킷 분석

| 버킷 (바이트) | 의미 |
|--------------|------|
| 128 | 128B (최소 응답) |
| 256 | 256B |
| 512 | 512B |
| 1,024 | 1KB |
| 2,048 | 2KB |
| 4,096 | 4KB |
| 8,192 | 8KB |
| 16,384 | 16KB |
| 32,768 | 32KB |
| 65,536 | 64KB |
| 131,072 | 128KB |
| 262,144 | 256KB |
| 524,288 | 512KB |
| 1,048,576 | 1MB |
| 2,097,152 | 2MB |
| 4,194,304 | 4MB (최대) |

### Exemplar 지원

트레이스 ID가 있으면 히스토그램 exemplar로 기록하여 메트릭과 트레이스를 연결한다:

```go
if traceID := tracing.TraceIDFromContext(r.Context(), true); traceID != "" {
    durationHistogram.(prometheus.ExemplarObserver).ObserveWithExemplar(
        elapsedTime, prometheus.Labels{"traceID": traceID},
    )
    sizeHistogram.(prometheus.ExemplarObserver).ObserveWithExemplar(
        responseSize, prometheus.Labels{"traceID": traceID},
    )
} else {
    durationHistogram.Observe(elapsedTime)
    sizeHistogram.Observe(responseSize)
}
```

### 요청 유형별 카운터

```go
switch {
case strings.HasPrefix(r.RequestURI, "/api/datasources/proxy"):
    countProxyRequests(status)
case strings.HasPrefix(r.RequestURI, "/api/"):
    countApiRequests(status)
default:
    countPageRequests(status)
}
```

---

## 4. 플러그인 메트릭

### 플러그인 요청 메트릭

`pkg/infra/metrics/metrics.go`에 정의된 플러그인 관련 메트릭:

| 메트릭 | 타입 | 라벨 | 설명 |
|--------|------|------|------|
| `grafana_api_dataproxy_request_all_milliseconds` | Summary | - | 데이터소스 프록시 요청 시간 |
| `grafana_api_status` | Counter | status_code | API 응답 상태 |
| `grafana_proxy_status` | Counter | status_code | 프록시 응답 상태 |

### SSE 데이터소스 요청 메트릭

```go
// pkg/expr/nodes.go
s.metrics.DSRequests.WithLabelValues(
    respStatus,                                    // "success" 또는 "failure"
    fmt.Sprintf("%t", useDataplane),              // dataplane 응답 여부
    dn.datasource.Type,                            // 데이터소스 타입
).Inc()
```

---

## 5. 알림 메트릭

### 알림 실행 메트릭

```go
// pkg/infra/metrics/metrics.go

// 알림 실행 결과 상태
MAlertingResultState *prometheus.CounterVec
// 라벨: alertstate (ok, alerting, no_data, paused, pending)

// 알림 알림 발송 상태
MAlertingNotificationSent *prometheus.CounterVec
// 라벨: type (email, slack, pagerduty, ...)

// 알림 발송 실패
MAlertingNotificationFailed *prometheus.CounterVec
// 라벨: type

// 활성 알림 수
MAlertingActiveAlerts prometheus.Gauge

// 알림 실행 시간
MAlertingExecutionTime prometheus.Summary
```

### 알림 메트릭 테이블

| 메트릭 이름 | 타입 | 설명 |
|------------|------|------|
| `grafana_alerting_result_total` | Counter | 알림 평가 결과 수 |
| `grafana_alerting_notification_sent_total` | Counter | 발송된 알림 수 |
| `grafana_alerting_notification_failed_total` | Counter | 실패한 알림 발송 수 |
| `grafana_alerting_active_alerts` | Gauge | 현재 활성 알림 수 |
| `grafana_alerting_execution_time_milliseconds` | Summary | 알림 평가 실행 시간 |
| `grafana_alerting_rule_evaluations_total` | Counter | 규칙 평가 총 횟수 |
| `grafana_alerting_rule_evaluation_duration_seconds` | Histogram | 규칙 평가 시간 |

---

## 6. SSE 표현식 메트릭

### ExprMetrics 구조

```go
// pkg/expr/metrics/
type ExprMetrics struct {
    DSRequests          *prometheus.CounterVec   // 데이터소스 요청 수
    SqlCommandCount     *prometheus.CounterVec   // SQL 커맨드 실행 수
    SqlCommandInputCount *prometheus.CounterVec  // SQL 입력 변환 수
}
```

### SQL 커맨드 메트릭

| 메트릭 | 라벨 | 설명 |
|--------|------|------|
| `grafana_sse_sql_command_count_total` | status, category | SQL 표현식 실행 수 |
| `grafana_sse_sql_command_input_count_total` | status, converted, type, data_type | SQL 입력 변환 수 |

---

## 7. /metrics 엔드포인트

### 엔드포인트 구성

```go
// pkg/api/http_server.go
func (hs *HTTPServer) metricsEndpoint(ctx *web.Context) {
    if !hs.Cfg.MetricsEndpointEnabled {
        return
    }
    if hs.metricsEndpointBasicAuthEnabled() && !BasicAuthenticatedRequest(ctx.Req, ...) {
        ctx.Resp.WriteHeader(http.StatusUnauthorized)
        return
    }
    promhttp.
        HandlerFor(hs.promGatherer, promhttp.HandlerOpts{EnableOpenMetrics: true}).
        ServeHTTP(ctx.Resp, ctx.Req)
}
```

### 메트릭 엔드포인트 설정

```ini
[metrics]
# 메트릭 엔드포인트 활성화 (기본: true)
enabled = true

# 인터벌 초 (내부 메트릭 수집 주기)
interval_seconds = 10

# Basic Auth 설정 (선택적)
basic_auth_username = prometheus
basic_auth_password = ${METRICS_PASSWORD}

# grafana_team 라벨 포함 (카디널리티 증가)
include_team_label = false

# 클래식 히스토그램 활성화 (Native Histogram과 함께)
classic_http_histogram_enabled = true
```

### OpenMetrics 포맷

`EnableOpenMetrics: true`로 설정되어 있어 OpenMetrics 형식(protobuf)으로도
메트릭을 노출할 수 있다. Prometheus 2.x+에서 더 효율적인 스크래핑이 가능하다.

### 메트릭 인증

```ini
# Basic Auth로 보호
[metrics]
basic_auth_username = metrics
basic_auth_password = secret

# 또는 인증 없이 (내부 네트워크)
[metrics]
enabled = true
# basic_auth_username 미설정 = 인증 없음
```

---

## 8. OpenTelemetry 트레이싱

### 트레이싱 설정

```ini
[tracing.opentelemetry.otlp]
# OTLP 엔드포인트
address = tempo:4317

# 또는 Jaeger
[tracing.opentelemetry.jaeger]
address = jaeger:14268

# 공통 설정
[tracing]
# 샘플링 비율 (0.0 ~ 1.0)
# 1.0 = 모든 요청 추적
# 0.01 = 1% 샘플링
sampler_type = const
sampler_param = 1.0

# 서비스 이름 (기본: grafana)
service_name = grafana

# 커스텀 태그
custom_tags = environment:production,region:us-east-1
```

### 트레이싱 미들웨어 구조

```
┌──────────────────────────────────────────────────────────────┐
│                  Grafana 트레이싱 흐름                         │
│                                                              │
│  HTTP 요청 수신                                               │
│       │                                                      │
│       v                                                      │
│  ┌──────────────────────────────────────────────────────┐    │
│  │  Request Tracing Middleware                           │    │
│  │  - 요청별 루트 스팬 생성                              │    │
│  │  - trace-id를 컨텍스트에 저장                         │    │
│  │  - 응답 헤더에 grafana-trace-id 설정                  │    │
│  └───────────────────────┬──────────────────────────────┘    │
│                          │                                    │
│       ┌──────────────────┼──────────────────────────┐        │
│       │                  │                          │        │
│       v                  v                          v        │
│  ┌──────────┐   ┌──────────────┐           ┌──────────────┐ │
│  │Query     │   │Plugin Client │           │Provisioning  │ │
│  │Service   │   │TracingMiddle │           │Service       │ │
│  │          │   │ware          │           │              │ │
│  │SSE.Build │   │PluginClient. │           │              │ │
│  │Pipeline  │   │QueryData     │           │              │ │
│  │SSE.Exec  │   │              │           │              │ │
│  │Pipeline  │   │plugin_id     │           │              │ │
│  │SSE.Exec  │   │org_id        │           │              │ │
│  │Node      │   │datasource_*  │           │              │ │
│  └──────────┘   └──────────────┘           └──────────────┘ │
│                                                              │
└──────────────────────────────────────────────────────────────┘
```

---

## 9. 요청 트레이싱 미들웨어

### grafana-trace-id 헤더

`pkg/middleware/middleware.go`에서 응답 헤더에 trace ID를 설정한다:

```go
func AddDefaultResponseHeaders(cfg *setting.Cfg) web.Handler {
    return func(c *web.Context) {
        c.Resp.Before(func(w web.ResponseWriter) {
            if w.Written() {
                return
            }

            traceId := tracing.TraceIDFromContext(c.Req.Context(), false)
            if traceId != "" {
                w.Header().Set("grafana-trace-id", traceId)
            }
            // ...
        })
    }
}
```

### 트레이스 ID 활용

```
HTTP 요청:
  POST /api/ds/query

HTTP 응답 헤더:
  grafana-trace-id: abc123def456

이 trace ID로:
  1. Tempo/Jaeger에서 트레이스 조회
  2. 요청의 전체 실행 경로 확인
  3. 각 단계의 소요 시간 분석
  4. 에러 발생 지점 파악
```

---

## 10. 플러그인 트레이싱

### TracingMiddleware

`pkg/services/pluginsintegration/clientmiddleware/tracing_middleware.go`:

```go
type TracingMiddleware struct {
    backend.BaseHandler
    tracer tracing.Tracer
}

func (m *TracingMiddleware) traceWrap(
    ctx context.Context, pluginContext backend.PluginContext,
) (context.Context, func(error)) {
    endpoint := backend.EndpointFromContext(ctx)
    ctx, span := m.tracer.Start(ctx,
        "PluginClient."+string(endpoint),
        trace.WithAttributes(
            attribute.String("plugin_id", pluginContext.PluginID),
            attribute.Int64("org_id", pluginContext.OrgID),
        ),
    )

    // 데이터소스 정보 추가
    if settings := pluginContext.DataSourceInstanceSettings; settings != nil {
        span.SetAttributes(
            attribute.String("datasource_name", settings.Name),
            attribute.String("datasource_uid", settings.UID),
        )
    }

    // 사용자 정보 추가
    if u := pluginContext.User; u != nil {
        span.SetAttributes(attribute.String("user", u.Login))
    }

    // HTTP 헤더에서 추가 속성
    if reqCtx := contexthandler.FromContext(ctx); reqCtx != nil {
        if v, err := strconv.Atoi(reqCtx.Req.Header.Get(query.HeaderPanelID)); err == nil {
            span.SetAttributes(attribute.Int("panel_id", v))
        }
        setSpanAttributeFromHTTPHeader(reqCtx.Req.Header, span,
            "query_group_id", query.HeaderQueryGroupID)
        setSpanAttributeFromHTTPHeader(reqCtx.Req.Header, span,
            "dashboard_uid", query.HeaderDashboardUID)
    }

    return ctx, func(err error) {
        if err != nil {
            span.SetStatus(codes.Error, err.Error())
            span.RecordError(err)
        }
        span.End()
    }
}
```

### 트레이싱되는 플러그인 메서드

| 메서드 | 스팬 이름 | 설명 |
|--------|----------|------|
| `QueryData` | `PluginClient.queryData` | 데이터소스 쿼리 |
| `CallResource` | `PluginClient.callResource` | 리소스 호출 |
| `CheckHealth` | `PluginClient.checkHealth` | 헬스 체크 |
| `CollectMetrics` | `PluginClient.collectMetrics` | 메트릭 수집 |
| `SubscribeStream` | `PluginClient.subscribeStream` | 스트림 구독 |
| `PublishStream` | `PluginClient.publishStream` | 스트림 발행 |
| `RunStream` | `PluginClient.runStream` | 스트림 실행 |
| `ValidateAdmission` | `PluginClient.validateAdmission` | 어드미션 검증 |
| `MutateAdmission` | `PluginClient.mutateAdmission` | 어드미션 변환 |
| `ConvertObjects` | `PluginClient.convertObjects` | 오브젝트 변환 |

### 플러그인 스팬 속성

| 속성 | 설명 |
|------|------|
| `plugin_id` | 플러그인 식별자 (예: prometheus, loki) |
| `org_id` | 조직 ID |
| `datasource_name` | 데이터소스 이름 |
| `datasource_uid` | 데이터소스 UID |
| `user` | 사용자 로그인 이름 |
| `panel_id` | 패널 ID (X-Panel-Id 헤더) |
| `dashboard_uid` | 대시보드 UID (X-Dashboard-Uid 헤더) |
| `query_group_id` | 쿼리 그룹 ID (X-Query-Group-Id 헤더) |

---

## 11. 표현식 트레이싱

### SSE 트레이싱 계층

```
SSE.BuildPipeline
│
├── SSE.BuildDependencyGraph
│   ├── Node 생성 (TypeCMDNode, TypeDatasourceNode, TypeMLNode)
│   └── Edge 생성 (의존성 연결)
│
SSE.ExecutePipeline
│
├── SSE.ExecuteDatasourceQuery [datasource.type=prometheus, datasource.uid=prom-1]
│   └── PluginClient.queryData [plugin_id=prometheus]
│
├── SSE.ExecuteNode [node.refId=B, node.inputRefIDs=[A]]
│   └── (Reduce 커맨드 실행)
│
└── SSE.ExecuteNode [node.refId=C, node.inputRefIDs=[B]]
    └── (Threshold 커맨드 실행)
```

### 코드에서의 트레이싱 구현

```go
// ExecutePipeline 스팬
func (s *Service) ExecutePipeline(ctx context.Context, now time.Time,
    pipeline DataPipeline) (*backend.QueryDataResponse, error) {
    ctx, span := s.tracer.Start(ctx, "SSE.ExecutePipeline")
    defer span.End()
    // ...
}

// BuildPipeline 스팬
func (s *Service) buildPipeline(ctx context.Context, req *Request) (DataPipeline, error) {
    _, span := s.tracer.Start(ctx, "SSE.BuildPipeline")
    defer span.End()
    // ...
}

// 노드별 스팬
c, span := s.tracer.Start(c, "SSE.ExecuteNode")
span.SetAttributes(attribute.String("node.refId", node.RefID()))
if len(node.NeedsVars()) > 0 {
    span.SetAttributes(attribute.StringSlice("node.inputRefIDs", node.NeedsVars()))
}

// 데이터소스 쿼리 스팬
ctx, span := s.tracer.Start(ctx, "SSE.ExecuteDatasourceQuery")
span.SetAttributes(
    attribute.String("datasource.type", dn.datasource.Type),
    attribute.String("datasource.uid", dn.datasource.UID),
)
```

### 에러 시 스팬 상태

```go
if e != nil {
    span.SetStatus(codes.Error, "failed to query data source")
    span.RecordError(e)
}
```

---

## 12. 구조화된 로깅

### 로거 생성

Grafana는 구조화된 로깅을 사용하며, 컴포넌트별로 이름이 지정된 로거를 생성한다:

```go
// 컴포넌트별 로거
log.New("provisioning")
log.New("provisioning.datasources")
log.New("provisioning.dashboard")
log.New("expr")
log.New("secrets")
log.New("sqlstore")
```

### 로거 사용 패턴

```go
// 기본 로깅
ps.log.Info("starting to provision dashboards")
ps.log.Error("Failed to provision data sources", "error", err)
ps.log.Debug("provisioned dashboard is up to date",
    "provisioner", fr.Cfg.Name,
    "file", path,
    "folderUid", dash.Dashboard.FolderUID)

// 컨텍스트 기반 로거 (요청 정보 포함)
logger := logger.FromContext(ctx).New(
    "datasourceType", firstNode.datasource.Type,
    "queryRefId", firstNode.refID,
    "datasourceUid", firstNode.datasource.UID,
)
```

### 로깅 설정

```ini
[log]
# 로그 모드: console, file, syslog
mode = console file

# 기본 로그 레벨
level = info

# 필터 (컴포넌트별 레벨 설정)
filters = rendering:debug provisioning:debug sqlstore:info

# 콘솔 설정
[log.console]
level = info
format = console    # console 또는 json

# 파일 설정
[log.file]
level = info
format = json
log_rotate = true
max_lines = 1000000
max_size_shift = 28   # 256MB
daily_rotate = true
max_days = 7
```

### 로그 포맷 예시

```
# console 포맷
t=2024-01-01T00:00:00+09:00 lvl=info msg="starting to provision dashboards" logger=provisioning.dashboard

# json 포맷
{"t":"2024-01-01T00:00:00+09:00","lvl":"info","msg":"starting to provision dashboards","logger":"provisioning.dashboard"}
```

### 로그 레벨 체계

| 레벨 | 용도 |
|------|------|
| `debug` | 개발/디버깅용 상세 정보 |
| `info` | 정상 운영 이벤트 |
| `warn` | 경고 (서비스는 정상 동작) |
| `error` | 에러 (기능 장애) |
| `critical` | 심각한 에러 (서비스 중단 가능) |

---

## 13. Health 엔드포인트

### /healthz 엔드포인트

```go
// pkg/api/http_server.go

// healthzHandler always return 200 - Ok if Grafana's web server is running
func (hs *HTTPServer) healthzHandler(ctx *web.Context) {
    notHeadOrGet := ctx.Req.Method != http.MethodGet && ctx.Req.Method != http.MethodHead
    if notHeadOrGet || ctx.Req.URL.Path != "/healthz" {
        return
    }
    ctx.Resp.WriteHeader(http.StatusOK)
    if _, err := ctx.Resp.Write([]byte("Ok")); err != nil {
        hs.log.Error("could not write to response", "err", err)
    }
}
```

**특징:**
- 항상 200 OK 반환 (웹서버가 살아있는 한)
- DB 연결 상태와 무관
- Kubernetes liveness probe에 적합
- GET과 HEAD 메서드만 처리

### /api/health 엔드포인트

```go
// pkg/api/http_server.go

func (hs *HTTPServer) apiHealthHandler(ctx *web.Context) {
    notHeadOrGet := ctx.Req.Method != http.MethodGet && ctx.Req.Method != http.MethodHead
    if notHeadOrGet || ctx.Req.URL.Path != "/api/health" {
        return
    }

    data := healthResponse{
        Database: "ok",
    }

    // 버전 정보 포함 (익명 접근 시 숨김 가능)
    if !hs.Cfg.Anonymous.HideVersion {
        data.Version = hs.Cfg.BuildVersion
        data.Commit = hs.Cfg.BuildCommit
        if hs.Cfg.EnterpriseBuildCommit != "NA" && hs.Cfg.EnterpriseBuildCommit != "" {
            data.EnterpriseCommit = hs.Cfg.EnterpriseBuildCommit
        }
    }

    // DB 헬스 체크
    if !hs.databaseHealthy(ctx.Req.Context()) {
        data.Database = "failing"
        ctx.Resp.WriteHeader(http.StatusServiceUnavailable)  // 503
    } else {
        ctx.Resp.WriteHeader(http.StatusOK)                  // 200
    }
    // ...
}
```

**응답 구조:**

```go
type healthResponse struct {
    Database         string `json:"database"`
    Version          string `json:"version,omitempty"`
    Commit           string `json:"commit,omitempty"`
    EnterpriseCommit string `json:"enterpriseCommit,omitempty"`
}
```

### Health 엔드포인트 비교

| 엔드포인트 | 응답 코드 | DB 체크 | 용도 |
|-----------|----------|---------|------|
| `/healthz` | 항상 200 | 안 함 | Liveness probe |
| `/api/health` | 200 또는 503 | 함 | Readiness probe |

### 응답 예시

```json
// 정상
{
  "database": "ok",
  "version": "11.0.0",
  "commit": "abc123"
}

// DB 장애
{
  "database": "failing",
  "version": "11.0.0",
  "commit": "abc123"
}
```

### Kubernetes 프로브 설정 예시

```yaml
livenessProbe:
  httpGet:
    path: /healthz
    port: 3000
  initialDelaySeconds: 10
  periodSeconds: 10

readinessProbe:
  httpGet:
    path: /api/health
    port: 3000
  initialDelaySeconds: 10
  periodSeconds: 10
```

---

## 14. 에러 추적과 에러 소스

### 에러 소스 분류

Grafana는 에러의 원인을 분류하여 메트릭 라벨에 포함한다:

| 소스 | 설명 |
|------|------|
| `server` | Grafana 서버 자체의 에러 |
| `plugin` | 플러그인(데이터소스)에서 발생한 에러 |
| `downstream` | 하류 서비스(프록시 대상)의 에러 |

### SLO 그룹 분류

```go
// requestmeta 패키지에서 관리
type SLOGroup string

const (
    SLOGroupLow    SLOGroup = "low"
    SLOGroupMedium SLOGroup = "medium"
    SLOGroupHigh   SLOGroup = "high"
)
```

### 트레이스 ID in 에러 응답

에러 응답에 trace ID를 포함하여 디버깅을 용이하게 한다:

```json
{
  "message": "Internal Server Error",
  "traceID": "abc123def456789",
  "status": "error"
}
```

### 요청 메타데이터

```go
type RequestMetaData struct {
    StatusSource StatusSource  // server, plugin, downstream
    SLOGroup     SLOGroup      // low, medium, high
    Team         string        // grafana_team 라벨 값
}
```

---

## 15. 프로파일링

### 내장 프로파일러

Grafana는 Go의 `net/http/pprof`를 통해 런타임 프로파일링을 지원한다:

```ini
[diagnostics.profiling]
# 프로파일러 활성화
enabled = false

# 프로파일러 포트 (기본: 6060)
port = 6060

# 블록 프로파일 비율
block_profile_rate = 0

# 뮤텍스 프로파일 분율
mutex_profile_fraction = 0
```

### 프로파일링 엔드포인트

프로파일러가 활성화되면 다음 엔드포인트를 사용할 수 있다:

| 엔드포인트 | 설명 |
|-----------|------|
| `/debug/pprof/` | 프로파일 인덱스 |
| `/debug/pprof/heap` | 힙 메모리 프로파일 |
| `/debug/pprof/goroutine` | 고루틴 프로파일 |
| `/debug/pprof/allocs` | 메모리 할당 프로파일 |
| `/debug/pprof/block` | 블로킹 프로파일 |
| `/debug/pprof/mutex` | 뮤텍스 경합 프로파일 |
| `/debug/pprof/profile` | CPU 프로파일 (30초) |
| `/debug/pprof/trace` | 실행 트레이스 |

### 프로파일링 사용 예시

```bash
# CPU 프로파일 수집 (30초)
go tool pprof http://localhost:6060/debug/pprof/profile?seconds=30

# 힙 메모리 프로파일
go tool pprof http://localhost:6060/debug/pprof/heap

# 고루틴 덤프
curl http://localhost:6060/debug/pprof/goroutine?debug=2
```

---

## 16. Grafana Live (실시간 업데이트)

### 개념

Grafana Live는 WebSocket 기반의 실시간 데이터 스트리밍 시스템이다.
대시보드에서 데이터가 변경되면 브라우저에 즉시 푸시할 수 있다.

```
┌──────────────────────────────────────────────────────────────┐
│                  Grafana Live 아키텍처                        │
│                                                              │
│  ┌──────────┐     WebSocket     ┌─────────────────┐         │
│  │ Browser  │ ←────────────────→│ Grafana Server   │         │
│  │          │                   │                  │         │
│  │ Dashboard│                   │ ┌──────────────┐ │         │
│  │ Panel    │                   │ │  Live Hub    │ │         │
│  └──────────┘                   │ │              │ │         │
│                                 │ │ - Channels   │ │         │
│                                 │ │ - Subscribers│ │         │
│                                 │ │ - Publishers │ │         │
│                                 │ └──────────────┘ │         │
│                                 │                  │         │
│                                 │ ┌──────────────┐ │         │
│                                 │ │ Plugin Stream│ │         │
│                                 │ │ (RunStream)  │ │         │
│                                 │ └──────────────┘ │         │
│                                 └─────────────────┘         │
│                                                              │
└──────────────────────────────────────────────────────────────┘
```

### Live 관련 메트릭

| 메트릭 | 타입 | 설명 |
|--------|------|------|
| `grafana_live_users` | Gauge | 현재 연결된 WebSocket 사용자 수 |
| `grafana_live_channels` | Gauge | 활성 채널 수 |
| `grafana_live_messages_sent_total` | Counter | 전송된 메시지 수 |

### Live 설정

```ini
[live]
# 최대 연결 수
max_connections = 100

# 허용된 오리진 (CORS)
allowed_origins = *

# HA 모드 (Redis 기반)
ha_engine = redis
ha_engine_address = redis:6379
```

---

## 17. 운영 모니터링 구성 예시

### Grafana 자기 모니터링 대시보드

Grafana로 Grafana 자신을 모니터링하는 "메타 모니터링" 구성:

```yaml
# Prometheus scrape 설정
scrape_configs:
  - job_name: 'grafana'
    metrics_path: '/metrics'
    basic_auth:
      username: prometheus
      password: ${METRICS_PASSWORD}
    static_configs:
      - targets: ['grafana:3000']
    scrape_interval: 15s
```

### 핵심 모니터링 쿼리

```promql
# 요청 처리량 (초당 요청 수)
rate(grafana_http_request_duration_seconds_count[5m])

# P99 응답 시간
histogram_quantile(0.99,
  rate(grafana_http_request_duration_seconds_bucket[5m])
)

# 에러율
sum(rate(grafana_http_request_duration_seconds_count{status_code=~"5.."}[5m]))
/
sum(rate(grafana_http_request_duration_seconds_count[5m]))

# 현재 처리 중인 요청 수
grafana_http_request_in_flight

# 활성 알림 수
grafana_alerting_active_alerts

# 알림 발송 실패율
rate(grafana_alerting_notification_failed_total[5m])

# DB 헬스 (api/health 기반)
probe_success{job="grafana-health"}
```

### 알림 규칙 예시

```yaml
groups:
  - name: grafana-self-monitoring
    rules:
      - alert: GrafanaHighErrorRate
        expr: |
          sum(rate(grafana_http_request_duration_seconds_count{status_code=~"5.."}[5m]))
          /
          sum(rate(grafana_http_request_duration_seconds_count[5m]))
          > 0.05
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: 'Grafana error rate is above 5%'

      - alert: GrafanaSlowRequests
        expr: |
          histogram_quantile(0.99,
            rate(grafana_http_request_duration_seconds_bucket[5m])
          ) > 5
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: 'Grafana P99 latency is above 5 seconds'

      - alert: GrafanaHighInFlight
        expr: grafana_http_request_in_flight > 100
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: 'Too many in-flight requests'

      - alert: GrafanaDatabaseUnhealthy
        expr: probe_success{job="grafana-health"} == 0
        for: 1m
        labels:
          severity: critical
        annotations:
          summary: 'Grafana database is unhealthy'
```

### 트레이싱 연동 설정

```ini
# Grafana → Tempo 연동
[tracing.opentelemetry.otlp]
address = tempo:4317

# 대시보드에서 Exemplar 클릭 시 Tempo로 이동
# 데이터소스 프로비저닝에서:
datasources:
  - name: Prometheus
    jsonData:
      exemplarTraceIdDestinations:
        - name: traceID
          datasourceUid: tempo-uid
```

---

## 소스 코드 참조

| 파일 | 역할 |
|------|------|
| `pkg/middleware/request_metrics.go` | HTTP 요청 메트릭 미들웨어 |
| `pkg/middleware/middleware.go` | grafana-trace-id 헤더 설정, 캐시 제어 |
| `pkg/infra/metrics/metrics.go` | 전역 Prometheus 메트릭 정의 |
| `pkg/infra/tracing/` | OpenTelemetry 트레이싱 통합 |
| `pkg/infra/log/` | 구조화된 로깅 시스템 |
| `pkg/services/pluginsintegration/clientmiddleware/tracing_middleware.go` | 플러그인 트레이싱 미들웨어 |
| `pkg/expr/service.go` | SSE 트레이싱 (ExecutePipeline, BuildPipeline) |
| `pkg/expr/graph.go` | 노드별 트레이싱 (ExecuteNode) |
| `pkg/expr/nodes.go` | 데이터소스 쿼리 트레이싱 (ExecuteDatasourceQuery) |
| `pkg/expr/metrics/` | SSE 전용 메트릭 정의 |
| `pkg/api/http_server.go` | /healthz, /api/health, /metrics 핸들러 |
| `pkg/api/health.go` | databaseHealthy() 함수 |
| `pkg/middleware/requestmeta/` | 요청 메타데이터 (StatusSource, SLOGroup) |
