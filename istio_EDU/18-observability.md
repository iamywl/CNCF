# 18. Istio 관측 가능성(Observability)과 텔레메트리 Deep-Dive

## 목차
1. [개요](#1-개요)
2. [텔레메트리 아키텍처 전체 구조](#2-텔레메트리-아키텍처-전체-구조)
3. [컨트롤 플레인 메트릭 (Istiod)](#3-컨트롤-플레인-메트릭-istiod)
4. [데이터 플레인 메트릭 (Envoy)](#4-데이터-플레인-메트릭-envoy)
5. [Telemetry API](#5-telemetry-api)
6. [분산 트레이싱](#6-분산-트레이싱)
7. [액세스 로깅](#7-액세스-로깅)
8. [Prometheus 통합](#8-prometheus-통합)
9. [Grafana 대시보드](#9-grafana-대시보드)
10. [Kiali: 서비스 그래프 시각화](#10-kiali-서비스-그래프-시각화)
11. [텔레메트리 설정 최적화](#11-텔레메트리-설정-최적화)
12. [내부 모니터링 프레임워크](#12-내부-모니터링-프레임워크)
13. [요약](#13-요약)

---

## 1. 개요

Istio의 관측 가능성(Observability)은 서비스 메시 내의 모든 트래픽에 대한 메트릭, 트레이싱, 로깅을 애플리케이션 코드 변경 없이 자동으로 수집하는 기능이다. Istio는 크게 두 가지 레이어에서 텔레메트리를 생성한다.

1. **컨트롤 플레인 텔레메트리**: Istiod 자체의 운영 상태를 나타내는 메트릭 (xDS push 횟수, 수렴 시간, 서비스 수 등)
2. **데이터 플레인 텔레메트리**: Envoy 사이드카 프록시가 생성하는 요청 기반 메트릭, 트레이싱 스팬, 액세스 로그

이 문서에서는 Istio 소스코드를 기반으로 관측 가능성 시스템의 내부 구현을 분석한다. 핵심 소스 파일은 다음과 같다.

| 소스 파일 | 역할 |
|-----------|------|
| `pilot/pkg/xds/monitoring.go` | xDS 관련 컨트롤 플레인 메트릭 정의 |
| `pilot/pkg/bootstrap/monitoring.go` | Istiod 모니터링 서버 초기화 |
| `pilot/pkg/model/telemetry.go` | Telemetry API CRD 처리 및 메트릭 필터 생성 |
| `pilot/pkg/model/telemetry_logging.go` | 액세스 로그 설정 생성 |
| `pilot/pkg/networking/core/tracing.go` | 분산 트레이싱 설정 |
| `pilot/pkg/networking/core/accesslog.go` | 액세스 로그 빌더 |
| `pilot/pkg/features/telemetry.go` | 텔레메트리 관련 피처 플래그 |
| `pkg/monitoring/monitoring.go` | 내부 모니터링 프레임워크 (OpenTelemetry 기반) |
| `pkg/tracing/tracing.go` | Istiod 자체 트레이싱 초기화 |
| `istioctl/pkg/metrics/metrics.go` | istioctl metrics 명령어 구현 |

---

## 2. 텔레메트리 아키텍처 전체 구조

### 2.1 전체 데이터 흐름

```
+------------------------------------------------------------------+
|                        Istio 텔레메트리 아키텍처                     |
+------------------------------------------------------------------+
|                                                                    |
|  +-----------+     xDS Push      +------------+                    |
|  |  Istiod   |  ───────────────> |   Envoy    |                    |
|  | (Pilot)   |  (telemetry cfg)  | (Sidecar)  |                    |
|  +-----+-----+                   +------+-----+                    |
|        |                                |                          |
|        | /metrics                       | /stats/prometheus        |
|        v                                v                          |
|  +-----+----+                    +------+-----+                    |
|  |Prometheus |<───── scrape ────>| Prometheus |                    |
|  | (control) |                   | (data)     |                    |
|  +-----+----+                    +------+-----+                    |
|        |                                |                          |
|        +--------+     +---------+-------+                          |
|                 |     |                                            |
|                 v     v                                            |
|            +----+-----+----+                                       |
|            |    Grafana     |                                       |
|            |  Dashboards   |                                       |
|            +---------------+                                       |
|                                                                    |
|  +------------+           +-----------+                            |
|  |   Envoy    |──spans──> | Jaeger /  |                            |
|  |  (Sidecar) |           | Zipkin /  |                            |
|  +------------+           | OTel Coll |                            |
|                           +-----------+                            |
|                                                                    |
|  +------------+           +-----------+                            |
|  |   Envoy    |──logs───> | stdout /  |                            |
|  |  (Sidecar) |           | gRPC ALS /|                            |
|  +------------+           | OTel ALS  |                            |
|                           +-----------+                            |
+------------------------------------------------------------------+
```

### 2.2 세 가지 관측 가능성 축

Istio의 관측 가능성은 세 가지 축으로 나뉜다.

| 축 | 데이터 소스 | 수집 방식 | 용도 |
|----|-----------|----------|------|
| **메트릭** | Envoy stats + Istiod metrics | Prometheus scrape | 대시보드, 알림 |
| **트레이싱** | Envoy 트레이싱 스팬 | Push (gRPC/HTTP) | 분산 요청 추적 |
| **로깅** | Envoy 액세스 로그 | File/gRPC/OTel | 요청 레벨 디버깅 |

이 세 축은 Telemetry API (`telemetry.istio.io/v1alpha1`)를 통해 통합적으로 관리된다.

---

## 3. 컨트롤 플레인 메트릭 (Istiod)

### 3.1 xDS 관련 메트릭

Istiod의 핵심 메트릭은 `pilot/pkg/xds/monitoring.go`에 정의되어 있다. 이 파일은 xDS 프로토콜 관련 성능 지표를 Prometheus 형식으로 노출한다.

```
소스: pilot/pkg/xds/monitoring.go
```

#### 주요 메트릭 정의

| 메트릭 이름 | 타입 | 설명 | 레이블 |
|------------|------|------|--------|
| `pilot_services` | Gauge | Pilot이 인식한 전체 서비스 수 | - |
| `pilot_xds` | Gauge | XDS로 연결된 엔드포인트 수 | `version` |
| `pilot_xds_pushes` | Sum | xDS push 성공/실패 횟수 | `type` (cds, eds, lds, rds, cds_senderr 등) |
| `pilot_debounce_time` | Distribution | 설정 변경 후 디바운싱 시간 | - |
| `pilot_pushcontext_init_seconds` | Distribution | PushContext 초기화 시간 | - |
| `pilot_xds_push_time` | Distribution | xDS push 총 소요 시간 | `type` |
| `pilot_proxy_queue_time` | Distribution | 프록시가 push 큐에서 대기한 시간 | - |
| `pilot_push_triggers` | Sum | push가 트리거된 횟수와 이유 | `type` |
| `pilot_proxy_convergence_time` | Distribution | 설정 변경~프록시 수렴까지 지연 시간 | - |
| `pilot_inbound_updates` | Sum | Pilot이 수신한 업데이트 수 | `type` (config, eds, svc, svcdelete) |
| `pilot_sds_certificate_errors_total` | Sum | SDS 인증서 페치 실패 횟수 | - |
| `pilot_xds_config_size_bytes` | Distribution | 클라이언트에 push된 설정 크기 | - |

소스코드에서 이 메트릭들이 정의된 패턴을 살펴보면 다음과 같다.

```go
// pilot/pkg/xds/monitoring.go:32-35
monServices = monitoring.NewGauge(
    "pilot_services",
    "Total services known to pilot.",
)
```

```go
// pilot/pkg/xds/monitoring.go:47-50
pushes = monitoring.NewSum(
    "pilot_xds_pushes",
    "Pilot build and send errors for lds, rds, cds and eds.",
)
```

```go
// pilot/pkg/xds/monitoring.go:86-90
proxiesConvergeDelay = monitoring.NewDistribution(
    "pilot_proxy_convergence_time",
    "Delay in seconds between config change and a proxy receiving all required configuration.",
    []float64{.1, .5, 1, 3, 5, 10, 20, 30},
)
```

### 3.2 Push 트리거 메트릭

Push가 발생하는 이유를 추적하기 위해 `triggerMetric` 맵이 사전 계산된다.

```go
// pilot/pkg/xds/monitoring.go:126-139
var triggerMetric = map[model.TriggerReason]monitoring.Metric{
    model.EndpointUpdate:  pushTriggers.With(typeTag.Value(string(model.EndpointUpdate))),
    model.ConfigUpdate:    pushTriggers.With(typeTag.Value(string(model.ConfigUpdate))),
    model.ServiceUpdate:   pushTriggers.With(typeTag.Value(string(model.ServiceUpdate))),
    model.ProxyUpdate:     pushTriggers.With(typeTag.Value(string(model.ProxyUpdate))),
    model.GlobalUpdate:    pushTriggers.With(typeTag.Value(string(model.GlobalUpdate))),
    model.UnknownTrigger:  pushTriggers.With(typeTag.Value(string(model.UnknownTrigger))),
    model.DebugTrigger:    pushTriggers.With(typeTag.Value(string(model.DebugTrigger))),
    model.SecretTrigger:   pushTriggers.With(typeTag.Value(string(model.SecretTrigger))),
    model.NetworksTrigger: pushTriggers.With(typeTag.Value(string(model.NetworksTrigger))),
    model.ProxyRequest:    pushTriggers.With(typeTag.Value(string(model.ProxyRequest))),
    model.NamespaceUpdate: pushTriggers.With(typeTag.Value(string(model.NamespaceUpdate))),
    model.ClusterUpdate:   pushTriggers.With(typeTag.Value(string(model.ClusterUpdate))),
}
```

이 사전 계산은 메트릭 레코딩 시 불필요한 메모리 할당을 피하기 위한 최적화이다.

### 3.3 xDS 오류 추적 메트릭

`pkg/xds/monitoring.go`에는 xDS 프로토콜의 오류를 추적하는 별도 메트릭이 정의되어 있다.

| 메트릭 이름 | 타입 | 설명 |
|------------|------|------|
| `pilot_total_xds_internal_errors` | Sum | 내부 XDS 오류 총 수 |
| `pilot_xds_expired_nonce` | Sum | 만료된 nonce로 수신된 XDS 요청 수 |
| `pilot_total_xds_rejects` | Sum | 프록시에 의해 거부된 XDS 응답 수 |
| `pilot_xds_write_timeout` | Sum | XDS 응답 쓰기 타임아웃 수 |
| `pilot_xds_send_time` | Distribution | XDS 응답 전송 소요 시간 |
| `pilot_xds_recv_max` | Gauge | 수신된 최대 XDS 요청 크기 |

```go
// pkg/xds/monitoring.go:67-70
totalXDSRejects = monitoring.NewSum(
    "pilot_total_xds_rejects",
    "Total number of XDS responses from pilot rejected by proxy.",
)
```

### 3.4 Istiod 부트스트랩 메트릭

`pilot/pkg/bootstrap/monitoring.go`에서는 Istiod 서버의 기본 운영 메트릭을 정의한다.

```go
// pilot/pkg/bootstrap/monitoring.go:42-53
_ = monitoring.NewDerivedGauge(
    "istiod_uptime_seconds",
    "Current istiod server uptime in seconds",
).ValueFrom(func() float64 {
    return time.Since(serverStart).Seconds()
})

pilotVersion = monitoring.NewGauge(
    "pilot_info",
    "Pilot version and build information.",
)
```

Istiod의 메트릭 엔드포인트(`/metrics`)는 `addMonitor()` 함수에서 HTTP 멀티플렉서에 등록된다.

```go
// pilot/pkg/bootstrap/monitoring.go:56-58
func addMonitor(exporter http.Handler, mux *http.ServeMux) {
    mux.Handle(metricsPath, metricsMiddleware(exporter))
    // ...
}
```

### 3.5 사이드카 인젝션 메트릭

`pkg/kube/inject/monitoring.go`에서 정의되는 인젝션 관련 메트릭도 컨트롤 플레인 텔레메트리의 일부이다.

| 메트릭 이름 | 설명 |
|------------|------|
| `sidecar_injection_requests_total` | 사이드카 인젝션 요청 총 수 |
| `sidecar_injection_success_total` | 성공한 인젝션 수 |
| `sidecar_injection_failure_total` | 실패한 인젝션 수 |
| `sidecar_injection_skip_total` | 건너뛴 인젝션 수 |
| `sidecar_injection_time_seconds` | 인젝션 소요 시간 |

---

## 4. 데이터 플레인 메트릭 (Envoy)

### 4.1 표준 메트릭

Envoy 사이드카 프록시는 Istio의 Wasm 통계 필터(stats filter)를 통해 표준 서비스 메시 메트릭을 생성한다. `pilot/pkg/model/telemetry.go`에 정의된 매핑 테이블에서 이 메트릭들의 이름을 확인할 수 있다.

```go
// pilot/pkg/model/telemetry.go:957-968
var metricToPrometheusMetric = map[string]string{
    "REQUEST_COUNT":          "requests_total",
    "REQUEST_DURATION":       "request_duration_milliseconds",
    "REQUEST_SIZE":           "request_bytes",
    "RESPONSE_SIZE":          "response_bytes",
    "TCP_OPENED_CONNECTIONS": "tcp_connections_opened_total",
    "TCP_CLOSED_CONNECTIONS": "tcp_connections_closed_total",
    "TCP_SENT_BYTES":         "tcp_sent_bytes_total",
    "TCP_RECEIVED_BYTES":     "tcp_received_bytes_total",
    "GRPC_REQUEST_MESSAGES":  "request_messages_total",
    "GRPC_RESPONSE_MESSAGES": "response_messages_total",
}
```

실제 Prometheus에서 노출되는 메트릭 이름에는 `istio_` 접두어가 붙는다.

#### HTTP/gRPC 메트릭

| 메트릭 이름 | Prometheus 이름 | 설명 |
|------------|----------------|------|
| REQUEST_COUNT | `istio_requests_total` | 총 요청 수 (counter) |
| REQUEST_DURATION | `istio_request_duration_milliseconds` | 요청 응답 시간 (histogram) |
| REQUEST_SIZE | `istio_request_bytes` | 요청 바디 크기 (histogram) |
| RESPONSE_SIZE | `istio_response_bytes` | 응답 바디 크기 (histogram) |
| GRPC_REQUEST_MESSAGES | `istio_request_messages_total` | gRPC 요청 메시지 수 |
| GRPC_RESPONSE_MESSAGES | `istio_response_messages_total` | gRPC 응답 메시지 수 |

#### TCP 메트릭

| 메트릭 이름 | Prometheus 이름 | 설명 |
|------------|----------------|------|
| TCP_OPENED_CONNECTIONS | `istio_tcp_connections_opened_total` | 열린 TCP 연결 수 |
| TCP_CLOSED_CONNECTIONS | `istio_tcp_connections_closed_total` | 닫힌 TCP 연결 수 |
| TCP_SENT_BYTES | `istio_tcp_sent_bytes_total` | 송신 바이트 수 |
| TCP_RECEIVED_BYTES | `istio_tcp_received_bytes_total` | 수신 바이트 수 |

### 4.2 메트릭 레이블

모든 데이터 플레인 메트릭에는 다음 표준 레이블이 포함된다.

| 레이블 | 설명 |
|--------|------|
| `reporter` | 메트릭 리포터 (source, destination, waypoint) |
| `source_workload` | 소스 워크로드 이름 |
| `source_workload_namespace` | 소스 워크로드 네임스페이스 |
| `source_canonical_service` | 소스 canonical 서비스 |
| `source_canonical_revision` | 소스 canonical 리비전 |
| `destination_workload` | 대상 워크로드 이름 |
| `destination_workload_namespace` | 대상 워크로드 네임스페이스 |
| `destination_service` | 대상 서비스 호스트명 |
| `destination_service_name` | 대상 서비스 이름 |
| `destination_service_namespace` | 대상 서비스 네임스페이스 |
| `destination_canonical_service` | 대상 canonical 서비스 |
| `destination_canonical_revision` | 대상 canonical 리비전 |
| `request_protocol` | 요청 프로토콜 (HTTP, gRPC, TCP) |
| `response_code` | HTTP 응답 코드 |
| `response_flags` | Envoy 응답 플래그 |
| `connection_security_policy` | 연결 보안 정책 (mutual_tls, none) |

### 4.3 Stat Prefix 구성

`pilot/pkg/networking/telemetry/telemetry.go`에서 통계 접두어(stat prefix)를 구성하는 로직을 확인할 수 있다.

```go
// pilot/pkg/networking/telemetry/telemetry.go:29-36
var (
    serviceStatPattern           = "%SERVICE%"
    serviceNameStatPattern       = "%SERVICE_NAME%"
    serviceFQDNStatPattern       = "%SERVICE_FQDN%"
    servicePortStatPattern       = "%SERVICE_PORT%"
    serviceTargetPortStatPattern = "%TARGET_PORT%"
    servicePortNameStatPattern   = "%SERVICE_PORT_NAME%"
    subsetNameStatPattern        = "%SUBSET_NAME%"
)
```

`BuildStatPrefix()` 함수는 이 패턴들을 실제 서비스 정보로 대체하여 통계 접두어를 생성한다.

```go
// pilot/pkg/networking/telemetry/telemetry.go:39-48
func BuildStatPrefix(statPattern string, host string, subset string,
    port *model.Port, targetPort int, attributes *model.ServiceAttributes) string {
    prefix := strings.ReplaceAll(statPattern, serviceStatPattern,
        shortHostName(host, attributes))
    prefix = strings.ReplaceAll(prefix, serviceNameStatPattern,
        serviceName(host, attributes))
    // ... (나머지 패턴 치환)
    return prefix
}
```

### 4.4 메트릭 교환을 위한 피처 플래그

`pilot/pkg/features/telemetry.go`에서 메트릭 관련 피처 플래그를 확인할 수 있다.

```go
// pilot/pkg/features/telemetry.go:43-53
EnableTelemetryLabel = env.Register("PILOT_ENABLE_TELEMETRY_LABEL", true,
    "If true, pilot will add telemetry related metadata to cluster and endpoint resources, "+
    "which will be consumed by telemetry filter.",
).Get()

MetadataExchange = env.Register("PILOT_ENABLE_METADATA_EXCHANGE", true,
    "If true, pilot will add metadata exchange filters, "+
    "which will be consumed by telemetry filter.",
).Get()
```

| 환경 변수 | 기본값 | 설명 |
|----------|--------|------|
| `PILOT_ENABLE_TELEMETRY_LABEL` | `true` | 클러스터/엔드포인트에 텔레메트리 메타데이터 추가 |
| `PILOT_ENDPOINT_TELEMETRY_LABEL` | `true` | 엔드포인트에 텔레메트리 메타데이터 추가 |
| `PILOT_ENABLE_METADATA_EXCHANGE` | `true` | 메타데이터 교환 필터 활성화 |
| `PILOT_TRACE_SAMPLING` | `1.0` | 기본 트레이스 샘플링 비율 (0.0~100.0) |
| `PILOT_MX_ADDITIONAL_LABELS` | `""` | 메타데이터 교환에 추가할 라벨 (쉼표 구분) |
| `PILOT_SPAWN_UPSTREAM_SPAN_FOR_GATEWAY` | `true` | 게이트웨이에서 업스트림 요청별 별도 스팬 생성 |

---

## 5. Telemetry API

### 5.1 Telemetry CRD 구조

Telemetry API는 `telemetry.istio.io/v1alpha1` GVK의 CRD로 관리된다. `pilot/pkg/model/telemetry.go`에서 내부 표현을 확인할 수 있다.

```go
// pilot/pkg/model/telemetry.go:44-48
type Telemetry struct {
    Name      string         `json:"name"`
    Namespace string         `json:"namespace"`
    Spec      *tpb.Telemetry `json:"spec"`
}
```

`Telemetries` 구조체는 네임스페이스별로 텔레메트리 설정을 조직화하고, 성능 최적화를 위한 캐시를 관리한다.

```go
// pilot/pkg/model/telemetry.go:55-78
type Telemetries struct {
    NamespaceToTelemetries map[string][]Telemetry `json:"namespace_to_telemetries"`
    RootNamespace          string                 `json:"root_namespace"`
    meshConfig             *meshconfig.MeshConfig

    // 캐시: 메트릭 필터를 모든 리스너에 삽입하는 비용이 높으므로
    // Telemetry 키 + 클래스 + 프로토콜 기반으로 캐싱
    computedMetricsFilters map[metricsKey]any
    computedLoggingConfig  map[loggingKey][]LoggingConfig
    mu                     sync.Mutex
}
```

### 5.2 텔레메트리 계층 구조와 병합

Telemetry 설정은 세 가지 수준에서 적용되며, 아래로 갈수록 우선순위가 높다.

```
+---------------------+
| Root Namespace      |  ← 메시 전체 기본 설정
| (istio-system)      |
+----------+----------+
           |
+----------v----------+
| Namespace-level     |  ← 네임스페이스별 설정
| (selector 없음)     |
+----------+----------+
           |
+----------v----------+
| Workload-level      |  ← 워크로드별 설정
| (selector 있음)     |
+---------------------+
```

이 병합 로직은 `applicableTelemetries()` 메서드에 구현되어 있다.

```go
// pilot/pkg/model/telemetry.go:407-474
func (t *Telemetries) applicableTelemetries(proxy *Proxy, svc *Service) computedTelemetries {
    // 순서가 중요: 뒤의 요소가 앞의 요소를 오버라이드
    ms := []*tpb.Metrics{}
    ls := []*computedAccessLogging{}
    ts := []*tpb.Tracing{}

    // 1. 루트 네임스페이스 설정
    if t.RootNamespace != "" {
        telemetry := t.namespaceWideTelemetryConfig(t.RootNamespace)
        // ... 메트릭, 로깅, 트레이싱 추가
    }

    // 2. 프록시 네임스페이스 설정
    if namespace != t.RootNamespace {
        telemetry := t.namespaceWideTelemetryConfig(namespace)
        // ... 메트릭, 로깅, 트레이싱 추가
    }

    // 3. 워크로드 레벨 설정 (selector/targetRef 매칭)
    for _, telemetry := range t.NamespaceToTelemetries[namespace] {
        if matcher.ShouldAttachPolicy(gvk.Telemetry, ...) {
            ct = appendApplicableTelemetries(ct, telemetry, spec)
        }
    }

    return *ct
}
```

### 5.3 computedTelemetries 구조

병합된 결과는 `computedTelemetries` 구조체에 담긴다.

```go
// pilot/pkg/model/telemetry.go:179-184
type computedTelemetries struct {
    telemetryKey
    Metrics []*tpb.Metrics
    Logging []*computedAccessLogging
    Tracing []*tpb.Tracing
}
```

### 5.4 메트릭 병합 (mergeMetrics)

`mergeMetrics()` 함수는 여러 Telemetry 설정의 메트릭 오버라이드를 프로바이더별, 모드별(CLIENT/SERVER), 메트릭별로 정규화한다.

```go
// pilot/pkg/model/telemetry.go:699-863
func mergeMetrics(metrics []*tpb.Metrics, mesh *meshconfig.MeshConfig) map[string]metricsConfig {
    // provider -> mode -> metric -> overrides 형태로 정규화
    providers := map[string]map[tpb.WorkloadMode]map[string]metricOverride{}
    // ...
}
```

병합 시 다음 규칙이 적용된다.

1. 프로바이더가 명시되면 부모의 프로바이더를 오버라이드
2. 프로바이더가 없으면 부모의 프로바이더를 상속
3. `ALL_METRICS`에 대한 disable은 해당 모드의 전체 필터를 드롭
4. 개별 메트릭 오버라이드는 이전 오버라이드 위에 누적

### 5.5 Stats 필터 생성

병합된 메트릭 설정은 `generateStatsConfig()` 함수를 통해 Envoy의 Stats 필터 설정으로 변환된다.

```go
// pilot/pkg/model/telemetry.go:970-1012
func generateStatsConfig(class networking.ListenerClass,
    filterConfig telemetryFilterConfig, isWaypoint bool) *anypb.Any {

    cfg := stats.PluginConfig{
        DisableHostHeaderFallback: disableHostHeaderFallback(class),
        TcpReportingDuration:     filterConfig.ReportingInterval,
    }

    for _, override := range listenerCfg.Overrides {
        metricName, f := metricToPrometheusMetric[override.Name]
        mc := &stats.MetricConfig{
            Dimensions: map[string]string{},
            Name:       metricName,
            Drop:       override.Disabled,
        }
        for _, t := range override.Tags {
            if t.Remove {
                mc.TagsToRemove = append(mc.TagsToRemove, t.Name)
            } else {
                mc.Dimensions[t.Name] = t.Value
            }
        }
        cfg.Metrics = append(cfg.Metrics, mc)
    }

    return protoconv.MessageToAny(&cfg)
}
```

### 5.6 Telemetry API YAML 예시

```yaml
# 메시 전체 기본 설정 (root namespace)
apiVersion: telemetry.istio.io/v1alpha1
kind: Telemetry
metadata:
  name: mesh-default
  namespace: istio-system
spec:
  # 메트릭 설정
  metrics:
  - providers:
    - name: prometheus
    overrides:
    - match:
        metric: ALL_METRICS
      tagOverrides:
        request_method:
          operation: UPSERT
          value: "request.method"
    - match:
        metric: REQUEST_COUNT
      disabled: false

  # 트레이싱 설정
  tracing:
  - providers:
    - name: otel-tracing
    randomSamplingPercentage: 10.0

  # 액세스 로깅 설정
  accessLogging:
  - providers:
    - name: envoy
    filter:
      expression: "response.code >= 400"

---
# 네임스페이스 레벨 설정
apiVersion: telemetry.istio.io/v1alpha1
kind: Telemetry
metadata:
  name: namespace-config
  namespace: my-app
spec:
  metrics:
  - providers:
    - name: prometheus
    overrides:
    - match:
        metric: REQUEST_COUNT
        mode: SERVER
      tagOverrides:
        custom_tag:
          operation: UPSERT
          value: "'custom-value'"
```

---

## 6. 분산 트레이싱

### 6.1 트레이싱 아키텍처

Istio의 분산 트레이싱은 Envoy 프록시의 내장 트레이싱 기능을 활용한다. `pilot/pkg/networking/core/tracing.go`에서 트레이싱 설정이 생성된다.

```
+----------+   HTTP Request   +----------+   HTTP Request   +----------+
|  Client  | ───────────────> | Envoy A  | ───────────────> | Envoy B  |
|          |                  | (source) |                  |  (dest)  |
+----------+                  +----+-----+                  +----+-----+
                                   |                             |
                              trace headers              trace headers
                              propagation                propagation
                                   |                             |
                                   v                             v
                            +------+------+              +------+------+
                            | Trace Span  |              | Trace Span  |
                            | (client)    |              | (server)    |
                            +------+------+              +------+------+
                                   |                             |
                                   +--------+     +--------------+
                                            |     |
                                            v     v
                                     +------+-----+------+
                                     | Tracing Backend   |
                                     | (Jaeger/Zipkin/   |
                                     |  OTel Collector)  |
                                     +-------------------+
```

### 6.2 트레이싱 설정 흐름

`configureTracing()` 함수가 트레이싱의 진입점이다.

```go
// pilot/pkg/networking/core/tracing.go:61-70
func configureTracing(
    push *model.PushContext,
    proxy *model.Proxy,
    httpConnMgr *hcm.HttpConnectionManager,
    class networking.ListenerClass,
    svc *model.Service,
) *requestidextension.UUIDRequestIDExtensionContext {
    tracingCfg := push.Telemetry.Tracing(proxy, svc)
    return configureTracingFromTelemetry(tracingCfg, push, proxy, httpConnMgr, class, svc)
}
```

### 6.3 지원 트레이싱 프로바이더

```go
// pilot/pkg/networking/core/tracing.go:50-54
const (
    envoyDatadog       = "envoy.tracers.datadog"
    envoyOpenTelemetry = "envoy.tracers.opentelemetry"
    envoySkywalking    = "envoy.tracers.skywalking"
    envoyZipkin        = "envoy.tracers.zipkin"
)
```

| 프로바이더 | Envoy 트레이서 이름 | 프로토콜 | 주요 특성 |
|-----------|-------------------|----------|----------|
| Zipkin | `envoy.tracers.zipkin` | HTTP (JSON) | B3 전파, 128bit trace ID |
| Datadog | `envoy.tracers.datadog` | HTTP | 서비스명 기반 |
| SkyWalking | `envoy.tracers.skywalking` | gRPC | child span 자동 생성 |
| OpenTelemetry | `envoy.tracers.opentelemetry` | gRPC/HTTP | W3C Trace Context, 커스텀 샘플러 |

### 6.4 OpenTelemetry 트레이싱 설정

OpenTelemetry는 가장 기능이 풍부한 프로바이더이다.

```go
// pilot/pkg/networking/core/tracing.go:339-440
func otelConfig(serviceName string,
    otelProvider *meshconfig.MeshConfig_ExtensionProvider_OpenTelemetryTracingProvider,
    pushCtx *model.PushContext, proxy *model.Proxy,
) (*anypb.Any, bool, error) {
    oc := &tracingcfg.OpenTelemetryConfig{
        ServiceName: serviceName,
    }

    // HTTP 또는 gRPC 내보내기 선택
    if otelProvider.GetHttp() != nil {
        oc.HttpService = &core.HttpService{ /* ... */ }
    } else {
        oc.GrpcService = &core.GrpcService{ /* ... */ }
    }

    // 리소스 디텍터 설정 (Environment, Dynatrace)
    // 커스텀 샘플러 설정 (Dynatrace Sampler)
}
```

OpenTelemetry의 service.name 결정 로직은 OTel 시맨틱 컨벤션을 따른다.

```go
// pilot/pkg/networking/core/tracing.go:912-958
func otelServiceName(proxy *model.Proxy) string {
    // 우선순위 폴백 체인:
    // 1. resource.opentelemetry.io/service.name 어노테이션
    // 2. app.kubernetes.io/instance 레이블
    // 3. app.kubernetes.io/name 레이블
    // 4. 소유 리소스 이름 (Deployment, StatefulSet 등)
    // 5. Pod 이름
    // 6. 컨테이너 이름 (단일 컨테이너인 경우)
    // 7. "unknown_service"
}
```

### 6.5 샘플링 설정

트레이싱 샘플링 비율은 다음 우선순위로 결정된다.

```
Provider 커스텀 샘플러 (100% 고정)
    > Telemetry API RandomSamplingPercentage
    > ProxyConfig defaultConfig.tracing.sampling
    > PILOT_TRACE_SAMPLING 환경변수 (기본 1.0%)
```

```go
// pilot/pkg/networking/core/tracing.go:123-135
var sampling float64
if useCustomSampler {
    // 커스텀 샘플러가 있으면 100%로 설정하여 샘플러에 위임
    sampling = 100
} else if spec.RandomSamplingPercentage != nil {
    sampling = *spec.RandomSamplingPercentage
} else {
    sampling = proxyConfigSamplingValue(proxyCfg)
}
```

### 6.6 커스텀 트레이스 태그

Istio는 자동으로 서비스 메시 관련 태그를 트레이스 스팬에 추가한다.

```go
// pilot/pkg/networking/core/tracing.go:634-703
func buildServiceTags(node *model.Proxy) []*tracing.CustomTag {
    // 자동 추가되는 태그:
    // - istio.canonical_revision
    // - istio.canonical_service
    // - istio.mesh_id
    // - istio.namespace
    // - istio.cluster_id
}
```

Waypoint 프록시의 경우 소스/대상 워크로드 정보를 추가로 캡처한다.

```go
// pilot/pkg/networking/core/tracing.go:591-632
func buildWaypointSourceTags() []*tracing.CustomTag {
    // FILTER_STATE를 통해 다음 태그 추출:
    // - istio.destination_workload, namespace, cluster_id, ...
    // - istio.source_workload, namespace, cluster_id, ...
}
```

### 6.7 TracingConfig 구조

```go
// pilot/pkg/model/telemetry.go:194-207
type TracingConfig struct {
    ServerSpec TracingSpec
    ClientSpec TracingSpec
}

type TracingSpec struct {
    Provider                     *meshconfig.MeshConfig_ExtensionProvider
    Disabled                     bool
    RandomSamplingPercentage     *float64
    CustomTags                   map[string]*tpb.Tracing_CustomTag
    UseRequestIDForTraceSampling bool
    EnableIstioTags              bool
    DisableContextPropagation    bool
}
```

`ServerSpec`은 인바운드 리스너에, `ClientSpec`은 아웃바운드/게이트웨이 리스너에 적용된다.

### 6.8 Istiod 자체 트레이싱

Istiod 자체도 OpenTelemetry를 통해 트레이싱을 지원한다.

```go
// pkg/tracing/tracing.go:90-105
func Initialize() (func(), error) {
    exp, err := newExporter()
    tp := trace.NewTracerProvider(
        trace.WithBatcher(exp),
        trace.WithResource(newResource()),
    )
    otel.SetTracerProvider(tp)
    // ...
}
```

OTLP 내보내기는 환경 변수로 설정한다.

| 환경 변수 | 설명 |
|----------|------|
| `OTEL_TRACES_EXPORTER` | `otlp`으로 설정 시 내보내기 활성화 |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | OTLP 엔드포인트 URL |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | 프로토콜 (`grpc` 또는 `http/protobuf`) |

---

## 7. 액세스 로깅

### 7.1 로깅 아키텍처

Istio의 액세스 로깅은 두 가지 경로로 설정된다.

1. **레거시 MeshConfig**: `meshConfig.accessLogFile`, `meshConfig.enableEnvoyAccessLogService`
2. **Telemetry API**: 프로바이더 기반 액세스 로깅 설정

```
+-------------------+
| Telemetry API     |
| (telemetry.go)    |
+--------+----------+
         |
         v
+--------+----------+     +-----------------------+
| AccessLogBuilder  | --> | Envoy Access Log      |
| (accesslog.go)    |     | Configuration         |
+-------------------+     +-----+-----+-----------+
                                |     |
                    +-----------+     +-----------+
                    v                             v
             +------+------+              +------+------+
             | File Logger |              | gRPC ALS    |
             | (/dev/stdout)|              | (TCP/HTTP)  |
             +-------------+              +------+------+
                                                 |
                                                 v
                                          +------+------+
                                          | OTel ALS    |
                                          | (OTLP)      |
                                          +-------------+
```

### 7.2 기본 텍스트 로그 포맷

`pilot/pkg/model/telemetry_logging.go`에서 기본 로그 포맷을 확인할 수 있다.

```go
// pilot/pkg/model/telemetry_logging.go:44-51
EnvoyTextLogFormat = "[%START_TIME%] \"%REQ(:METHOD)% %REQ(X-ENVOY-ORIGINAL-PATH?:PATH)% " +
    "%PROTOCOL%\" %RESPONSE_CODE% %RESPONSE_FLAGS% " +
    "%RESPONSE_CODE_DETAILS% %CONNECTION_TERMINATION_DETAILS% " +
    "\"%UPSTREAM_TRANSPORT_FAILURE_REASON%\" %BYTES_RECEIVED% %BYTES_SENT% " +
    "%DURATION% %RESP(X-ENVOY-UPSTREAM-SERVICE-TIME)% \"%REQ(X-FORWARDED-FOR)%\" " +
    "\"%REQ(USER-AGENT)%\" \"%REQ(X-REQUEST-ID)%\" \"%REQ(:AUTHORITY)%\" " +
    "\"%UPSTREAM_HOST%\" %UPSTREAM_CLUSTER_RAW% %UPSTREAM_LOCAL_ADDRESS% " +
    "%DOWNSTREAM_LOCAL_ADDRESS% %DOWNSTREAM_REMOTE_ADDRESS% " +
    "%REQUESTED_SERVER_NAME% %ROUTE_NAME%\n"
```

### 7.3 JSON 로그 포맷

JSON 형식의 로그는 `EnvoyJSONLogFormatIstio` 변수에 정의되어 있으며, 다음 필드를 포함한다.

| 필드 | Envoy 변수 |
|------|-----------|
| `start_time` | `%START_TIME%` |
| `method` | `%REQ(:METHOD)%` |
| `path` | `%REQ(X-ENVOY-ORIGINAL-PATH?:PATH)%` |
| `protocol` | `%PROTOCOL%` |
| `response_code` | `%RESPONSE_CODE%` |
| `response_flags` | `%RESPONSE_FLAGS%` |
| `response_code_details` | `%RESPONSE_CODE_DETAILS%` |
| `bytes_received` | `%BYTES_RECEIVED%` |
| `bytes_sent` | `%BYTES_SENT%` |
| `duration` | `%DURATION%` |
| `upstream_host` | `%UPSTREAM_HOST%` |
| `upstream_cluster` | `%UPSTREAM_CLUSTER_RAW%` |
| `downstream_remote_address` | `%DOWNSTREAM_REMOTE_ADDRESS%` |
| `requested_server_name` | `%REQUESTED_SERVER_NAME%` |
| `route_name` | `%ROUTE_NAME%` |

### 7.4 액세스 로그 프로바이더 종류

`telemetryAccessLog()` 함수에서 지원되는 프로바이더를 확인할 수 있다.

```go
// pilot/pkg/model/telemetry_logging.go:129-163
func telemetryAccessLog(push *PushContext, fp *meshconfig.MeshConfig_ExtensionProvider) *accesslog.AccessLog {
    switch prov := fp.Provider.(type) {
    case *meshconfig.MeshConfig_ExtensionProvider_EnvoyFileAccessLog:
        // 파일 기반 (기본: /dev/stdout)
    case *meshconfig.MeshConfig_ExtensionProvider_EnvoyHttpAls:
        // HTTP gRPC 액세스 로그 서비스
    case *meshconfig.MeshConfig_ExtensionProvider_EnvoyTcpAls:
        // TCP gRPC 액세스 로그 서비스
    case *meshconfig.MeshConfig_ExtensionProvider_EnvoyOtelAls:
        // OpenTelemetry 액세스 로그
    }
}
```

| 프로바이더 타입 | 내부 이름 | 전송 방식 |
|---------------|----------|----------|
| `EnvoyFileAccessLog` | `envoy.access_loggers.file` | 파일 출력 |
| `EnvoyHttpAls` | `envoy.http_grpc_access_log` | HTTP gRPC |
| `EnvoyTcpAls` | `envoy.tcp_grpc_access_log` | TCP gRPC |
| `EnvoyOtelAls` | `envoy.access_loggers.open_telemetry` | OpenTelemetry OTLP |

### 7.5 OpenTelemetry 액세스 로그

OTel 액세스 로그는 `openTelemetryLog()` 함수에서 설정된다.

```go
// pilot/pkg/model/telemetry_logging.go:427-458
func openTelemetryLog(pushCtx *PushContext,
    provider *meshconfig.MeshConfig_ExtensionProvider_EnvoyOpenTelemetryLogProvider,
) *accesslog.AccessLog {
    cfg := buildOpenTelemetryAccessLogConfig(logName, hostname, cluster, f, labels)
    return &accesslog.AccessLog{
        Name:       OtelEnvoyALSName,
        ConfigType: &accesslog.AccessLog_TypedConfig{TypedConfig: protoconv.MessageToAny(cfg)},
    }
}
```

OTel 로그 설정에서는 `FilterStateObjectsToLog`에 새로운 피어 메타데이터 키를 포함한다.

```go
// pilot/pkg/model/telemetry_logging.go:114-115
envoyWasmStateToLog = []string{
    "upstream_peer", "downstream_peer",              // 1.24.0부터
    "wasm.upstream_peer", "wasm.upstream_peer_id",   // 하위 호환
    "wasm.downstream_peer", "wasm.downstream_peer_id",
}
```

### 7.6 AccessLogBuilder

`pilot/pkg/networking/core/accesslog.go`의 `AccessLogBuilder`는 액세스 로그를 리스너에 설정하는 빌더 패턴을 구현한다.

```go
// pilot/pkg/networking/core/accesslog.go:59-71
type AccessLogBuilder struct {
    tcpGrpcAccessLog         *accesslog.AccessLog
    httpGrpcAccessLog        *accesslog.AccessLog
    tcpGrpcListenerAccessLog *accesslog.AccessLog
    coreAccessLog             cachedMeshConfigAccessLog
    listenerAccessLog         cachedMeshConfigAccessLog
    hboneOriginationAccessLog cachedMeshConfigAccessLog
    hboneTerminationAccessLog cachedMeshConfigAccessLog
}
```

Telemetry API 설정이 없으면 레거시 MeshConfig로 폴백한다.

```go
// pilot/pkg/networking/core/accesslog.go:200-220
func (b *AccessLogBuilder) setHTTPAccessLog(...) {
    cfgs := push.Telemetry.AccessLogging(push, proxy, class, svc)
    if len(cfgs) == 0 {
        // 레거시 폴백
        if mesh.AccessLogFile != "" {
            connectionManager.AccessLog = append(connectionManager.AccessLog,
                b.coreAccessLog.buildOrFetch(mesh))
        }
        if mesh.EnableEnvoyAccessLogService {
            connectionManager.AccessLog = append(connectionManager.AccessLog,
                b.httpGrpcAccessLog)
        }
        return
    }
    // Telemetry API 사용
    if al := buildAccessLogFromTelemetry(cfgs, nil); len(al) != 0 {
        connectionManager.AccessLog = append(connectionManager.AccessLog, al...)
    }
}
```

### 7.7 리스너 액세스 로그 필터

리스너 레벨의 액세스 로그는 `NR` (No Route) 플래그가 있는 경우에만 기록한다.

```go
// pilot/pkg/networking/core/accesslog.go:251-258
func listenerAccessLogFilter() *accesslog.AccessLogFilter {
    return &accesslog.AccessLogFilter{
        FilterSpecifier: &accesslog.AccessLogFilter_ResponseFlagFilter{
            ResponseFlagFilter: &accesslog.ResponseFlagFilter{
                Flags: []string{"NR"},
            },
        },
    }
}
```

Telemetry API의 CEL 필터 표현식도 지원된다.

```go
// pilot/pkg/networking/core/accesslog.go:181-198
func buildAccessLogFilterFromTelemetry(spec model.LoggingConfig) *accesslog.AccessLogFilter {
    fl := &cel.ExpressionFilter{
        Expression: spec.Filter.Expression,
    }
    return &accesslog.AccessLogFilter{
        FilterSpecifier: &accesslog.AccessLogFilter_ExtensionFilter{
            ExtensionFilter: &accesslog.ExtensionFilter{
                Name: celFilter,
                // ...
            },
        },
    }
}
```

### 7.8 로깅 병합 (mergeLogs)

```go
// pilot/pkg/model/telemetry.go:583-648
func mergeLogs(logs []*computedAccessLogging,
    mesh *meshconfig.MeshConfig, mode tpb.WorkloadMode) map[string]loggingSpec {
    // 프로바이더별로 로깅 설정을 수집
    // WorkloadMode(CLIENT/SERVER) 필터링 적용
    // 하위 레벨이 상위 레벨의 disabled 설정을 오버라이드 가능
}
```

---

## 8. Prometheus 통합

### 8.1 내부 모니터링 프레임워크

Istio의 내부 메트릭은 `pkg/monitoring/monitoring.go`에 정의된 OpenTelemetry 기반 프레임워크를 사용한다.

```go
// pkg/monitoring/monitoring.go:34-37
var meter = func() api.Meter {
    return otel.GetMeterProvider().Meter("istio")
}
```

이 프레임워크는 다음 메트릭 타입을 지원한다.

| 함수 | 타입 | 용도 |
|------|------|------|
| `NewSum()` | Counter (누적) | 요청 수, 오류 수 등 |
| `NewGauge()` | Gauge (최근값) | 현재 연결 수, 서비스 수 등 |
| `NewDistribution()` | Histogram | 지연 시간, 크기 분포 등 |
| `NewDerivedGauge()` | Gauge (함수 기반) | 업타임 등 동적 값 |

### 8.2 Prometheus Exporter 등록

```go
// pkg/monitoring/monitoring.go:48-74
func RegisterPrometheusExporter(reg prometheus.Registerer,
    gatherer prometheus.Gatherer) (http.Handler, error) {

    promOpts := []otelprom.Option{
        otelprom.WithoutScopeInfo(),
        otelprom.WithoutTargetInfo(),
        otelprom.WithoutUnits(),
        otelprom.WithRegisterer(reg),
        otelprom.WithoutCounterSuffixes(),
    }
    prom, err := otelprom.New(promOpts...)
    // ...
    mp := metric.NewMeterProvider(opts...)
    otel.SetMeterProvider(mp)
    handler := promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{})
    return handler, nil
}
```

### 8.3 Metric 인터페이스

```go
// pkg/monitoring/monitoring.go:77-108
type Metric interface {
    Increment()
    Decrement()
    Name() string
    Record(value float64)
    RecordInt(value int64)
    With(labelValues ...LabelValue) Metric
    Register() error
}
```

### 8.4 Histogram 버킷 설정

OpenTelemetry의 제한으로 인해 히스토그램 버킷은 계측 시점이 아닌 View 등록 시점에 설정된다.

```go
// pkg/monitoring/monitoring.go:279-298
func (d *metrics) toHistogramViews() []metric.Option {
    for name, def := range d.known {
        if def.Bounds == nil {
            continue
        }
        v := metric.WithView(metric.NewView(
            metric.Instrument{Name: name},
            metric.Stream{Aggregation: metric.AggregationExplicitBucketHistogram{
                Boundaries: def.Bounds,
            }},
        ))
        opts = append(opts, v)
    }
    return opts
}
```

### 8.5 Envoy의 /stats/prometheus 엔드포인트

Envoy 사이드카는 `/stats/prometheus` 엔드포인트에서 Prometheus 형식의 메트릭을 노출한다. Prometheus가 이 엔드포인트를 스크레이핑하면 Istio 데이터 플레인 메트릭(`istio_requests_total` 등)과 Envoy 내부 메트릭을 함께 수집한다.

### 8.6 istioctl metrics 명령어

`istioctl experimental metrics` 명령어는 Prometheus에서 직접 워크로드 메트릭을 쿼리한다.

```go
// istioctl/pkg/metrics/metrics.go:44-49
const (
    destWorkloadLabel          = "destination_workload"
    destWorkloadNamespaceLabel = "destination_workload_namespace"
    reqTot                     = "istio_requests_total"
    reqDur                     = "istio_request_duration_milliseconds"
)
```

쿼리되는 메트릭 항목:

| 항목 | PromQL 쿼리 패턴 |
|------|-----------------|
| Total RPS | `sum(rate(istio_requests_total{destination_workload=~"name.*",reporter="destination"}[1m]))` |
| Error RPS | 위와 동일 + `response_code=~"[45][0-9]{2}"` |
| P50 Latency | `histogram_quantile(0.5, sum(rate(istio_request_duration_milliseconds_bucket{...}[1m])) by (le))` |
| P90 Latency | P50과 동일, quantile=0.9 |
| P99 Latency | P50과 동일, quantile=0.99 |

---

## 9. Grafana 대시보드

### 9.1 제공되는 대시보드

Istio는 `manifests/addons/dashboards/` 디렉토리에 Grafana 대시보드 JSON 파일을 제공한다.

| 대시보드 파일 | 용도 |
|-------------|------|
| `istio-mesh-dashboard.gen.json` | 메시 전체 개요 |
| `istio-service-dashboard.json` | 서비스별 상세 메트릭 |
| `istio-workload-dashboard.json` | 워크로드별 상세 메트릭 |
| `istio-performance-dashboard.json` | Istiod 성능 모니터링 |
| `pilot-dashboard.gen.json` | Pilot(Istiod) 상태 모니터링 |
| `istio-extension-dashboard.json` | 확장 기능 모니터링 |
| `ztunnel-dashboard.gen.json` | ztunnel(Ambient 모드) 모니터링 |

### 9.2 메시 대시보드 쿼리 패턴

대시보드에서 사용되는 PromQL 쿼리는 `manifests/addons/dashboards/lib/queries.libsonnet`에 정의되어 있다.

#### 전역 요청 속도

```jsonnet
// queries.libsonnet
globalRequest: self.rawQuery(
    round(sum(rate(labels('istio_requests_total', { reporter: '~source|waypoint' }))))
),
```

#### 전역 성공률

```jsonnet
globalRequestSuccessRate: self.rawQuery(
    sum(rate(labels('istio_requests_total',
        { reporter: '~source|waypoint', response_code: '!~5..' })))
    + ' / ' +
    sum(rate(labels('istio_requests_total',
        { reporter: '~source|waypoint' })))
),
```

#### 4xx/5xx 오류율

```jsonnet
globalRequest4xx: self.rawQuery(
    round(sum(rate(labels('istio_requests_total',
        { reporter: '~source|waypoint', response_code: '~4..' }))))
    + 'or vector(0)'
),

globalRequest5xx: self.rawQuery(
    round(sum(rate(labels('istio_requests_total',
        { reporter: '~source|waypoint', response_code: '~5..' }))))
    + 'or vector(0)'
),
```

### 9.3 워크로드 메트릭 테이블

대시보드의 HTTP 워크로드 테이블은 다음 쿼리로 구성된다.

```jsonnet
httpWorkloads: [
    // 요청 총량 (RPS)
    sum(rate(labels('istio_requests_total', { reporter: '~source|waypoint' })),
        by=['destination_workload', 'destination_workload_namespace', 'destination_service'])

    // P50 레이턴시
    quantile('0.5', sum(rate(
        labels('istio_request_duration_milliseconds_bucket', { reporter: '~source|waypoint' })),
        by=['le', 'destination_workload', 'destination_workload_namespace']))

    // P90 레이턴시
    quantile('0.9', ...)

    // P99 레이턴시
    quantile('0.99', ...)

    // 성공률
    sum(rate(istio_requests_total{response_code!~"5.."}))
    / sum(rate(istio_requests_total))
]
```

### 9.4 Pilot 대시보드 쿼리

Pilot(Istiod) 대시보드는 다음 핵심 쿼리를 포함한다.

```jsonnet
// xDS Push 속도
xdsPushes: self.query('{{type}}', sum(irate('pilot_xds_pushes'), by=['type'])),

// xDS 오류
xdsErrors: [
    self.query('Rejected Config ({{type}})',
        sum('pilot_total_xds_rejects', by=['type'])),
    self.query('Internal Errors', 'pilot_total_xds_internal_errors'),
],

// xDS 연결 수
xdsConnections: [
    self.query('Connections (client reported)',
        'sum(envoy_cluster_upstream_cx_active{cluster_name="xds-grpc"})'),
    self.query('Connections (server reported)',
        sum('pilot_xds')),
],

// Push 시간 히트맵
pushTime: self.query('{{le}}',
    'sum(rate(pilot_xds_push_time_bucket{}[$__rate_interval])) by (le)'),

// Push 크기 히트맵
pushSize: self.query('{{le}}',
    'sum(rate(pilot_xds_config_size_bytes_bucket{}[$__rate_interval])) by (le)'),
```

### 9.5 리소스 사용량 쿼리

```jsonnet
// CPU 사용량
cpuUsage: self.query('Container ({{pod}})',
    sum(irate(labels('container_cpu_usage_seconds_total', containerLabels)), by=['pod'])),

// 메모리 사용량
memUsage: self.query('Container ({{pod}})',
    sum(labels('container_memory_working_set_bytes', containerLabels), by=['pod'])),

// Go 메모리 상세
goMemoryUsage: [
    // 컨테이너 워킹셋, 스택, 힙(In Use), 힙(Allocated)
],

// Goroutine 수
goroutines: self.query('Goroutines ({{pod}})',
    sum(labels('go_goroutines', appLabels), by=['pod'])),
```

---

## 10. Kiali: 서비스 그래프 시각화

### 10.1 Kiali 개요

Kiali는 Istio 서비스 메시의 관측 가능성 콘솔이다. Istio의 텔레메트리 데이터를 기반으로 서비스 그래프, 트래픽 흐름, 설정 검증을 시각화한다.

### 10.2 데이터 소스

```
+-----------+     +-----------+     +-----------+
| Prometheus| --> |   Kiali   | <-- |  Jaeger   |
| (메트릭)   |     | (시각화)   |     | (트레이싱) |
+-----------+     +-----+-----+     +-----------+
                        |
                        v
                  +-----+-----+
                  | Kubernetes|
                  |  API      |
                  | (설정)     |
                  +-----------+
```

Kiali는 다음 Istio 메트릭을 사용하여 서비스 그래프를 생성한다.

| 메트릭 | 용도 |
|--------|------|
| `istio_requests_total` | 서비스 간 요청 비율, 성공/실패 |
| `istio_request_duration_milliseconds` | 서비스 간 레이턴시 |
| `istio_tcp_sent_bytes_total` | TCP 트래픽 송신량 |
| `istio_tcp_received_bytes_total` | TCP 트래픽 수신량 |

### 10.3 서비스 그래프 시각화

Kiali 서비스 그래프는 다음 정보를 표시한다.

- **노드**: 서비스, 워크로드, 앱
- **엣지**: 서비스 간 트래픽 흐름
- **색상**: 정상(초록), 경고(주황), 오류(빨강)
- **두께**: 트래픽 볼륨
- **레이블**: RPS, 오류율, 레이턴시

### 10.4 Kiali와 Istio 텔레메트리 관계

Kiali가 올바르게 작동하려면 다음 레이블이 메트릭에 포함되어야 한다.

- `source_workload`, `source_workload_namespace`
- `destination_workload`, `destination_workload_namespace`
- `destination_service`, `destination_service_name`
- `response_code`, `reporter`

이 레이블들은 Istio의 메타데이터 교환(metadata exchange) 필터가 자동으로 추가하며, `PILOT_ENABLE_METADATA_EXCHANGE=true` (기본값)일 때 활성화된다.

---

## 11. 텔레메트리 설정 최적화

### 11.1 메트릭 필터링

불필요한 메트릭을 비활성화하여 Prometheus 스토리지 사용량과 Envoy 메모리를 줄일 수 있다.

```yaml
apiVersion: telemetry.istio.io/v1alpha1
kind: Telemetry
metadata:
  name: metric-optimization
  namespace: istio-system
spec:
  metrics:
  - providers:
    - name: prometheus
    overrides:
    # TCP 메트릭 비활성화
    - match:
        metric: TCP_OPENED_CONNECTIONS
      disabled: true
    - match:
        metric: TCP_CLOSED_CONNECTIONS
      disabled: true
    # 불필요한 태그 제거
    - match:
        metric: REQUEST_COUNT
      tagOverrides:
        request_protocol:
          operation: REMOVE
```

### 11.2 메트릭 캐싱 최적화

Istio는 메트릭 필터 계산 결과를 캐싱하여 성능을 최적화한다. 캐시 키는 다음 요소로 구성된다.

```go
// pilot/pkg/model/telemetry.go:98-105
type metricsKey struct {
    telemetryKey
    Class     networking.ListenerClass
    Protocol  networking.ListenerProtocol
    ProxyType NodeType
    Service   types.NamespacedName
}
```

캐시의 수명은 `Telemetries` 객체에 바인딩된다. PushContext 생성 시 Telemetry 리소스가 변경되지 않았으면 캐시가 유지된다.

### 11.3 트레이싱 샘플링 비율 최적화

프로덕션 환경에서 과도한 트레이싱은 성능에 영향을 줄 수 있다.

```yaml
apiVersion: telemetry.istio.io/v1alpha1
kind: Telemetry
metadata:
  name: tracing-config
  namespace: istio-system
spec:
  tracing:
  - providers:
    - name: otel-tracing
    # 프로덕션: 1-10%, 디버깅: 100%
    randomSamplingPercentage: 1.0
    # Request ID 기반 샘플링 비활성화로 더 정확한 분포
    useRequestIdForTraceSampling: false
```

### 11.4 액세스 로그 조건부 활성화

CEL 표현식 필터를 사용하여 특정 조건에서만 액세스 로그를 기록할 수 있다.

```yaml
apiVersion: telemetry.istio.io/v1alpha1
kind: Telemetry
metadata:
  name: access-log-filter
  namespace: istio-system
spec:
  accessLogging:
  - providers:
    - name: envoy
    filter:
      # 오류 응답만 로깅
      expression: "response.code >= 400"
  - providers:
    - name: envoy
    match:
      mode: CLIENT
    filter:
      # 느린 요청만 로깅 (1초 이상)
      expression: "response.duration > duration('1s')"
```

### 11.5 WorkloadMode 기반 최적화

클라이언트/서버 모드별로 다른 설정을 적용할 수 있다.

```go
// pilot/pkg/model/telemetry.go:221-235
func workloadMode(class networking.ListenerClass) tpb.WorkloadMode {
    switch class {
    case networking.ListenerClassGateway:
        return tpb.WorkloadMode_CLIENT
    case networking.ListenerClassSidecarInbound:
        return tpb.WorkloadMode_SERVER
    case networking.ListenerClassSidecarOutbound:
        return tpb.WorkloadMode_CLIENT
    }
    return tpb.WorkloadMode_CLIENT
}
```

```yaml
apiVersion: telemetry.istio.io/v1alpha1
kind: Telemetry
metadata:
  name: mode-specific
  namespace: my-app
spec:
  metrics:
  - providers:
    - name: prometheus
    overrides:
    # 서버 측에서만 커스텀 태그 추가
    - match:
        metric: REQUEST_COUNT
        mode: SERVER
      tagOverrides:
        my_custom_tag:
          operation: UPSERT
          value: "'my-value'"
    # 클라이언트 측에서 응답 크기 메트릭 비활성화
    - match:
        metric: RESPONSE_SIZE
        mode: CLIENT
      disabled: true
```

### 11.6 Reporting Interval 조정

TCP 메트릭의 보고 주기를 조정하여 메트릭 볼륨을 제어할 수 있다.

```go
// pilot/pkg/model/telemetry.go:131-135
type metricsConfig struct {
    ClientMetrics     metricConfig
    ServerMetrics     metricConfig
    ReportingInterval *durationpb.Duration
}
```

### 11.7 설정 크기 모니터링

`pilot_xds_config_size_bytes` 메트릭의 히스토그램 버킷으로 push되는 설정 크기를 모니터링할 수 있다.

```go
// pilot/pkg/xds/monitoring.go:107-115
configSizeBytes = monitoring.NewDistribution(
    "pilot_xds_config_size_bytes",
    "Distribution of configuration sizes pushed to clients",
    // 주요 경계: 10K, 1M, 4M, 10M, 40M
    // 4M: gRPC 기본 수신 제한, 10M: 시스템 부담 시작,
    // 40M: 지원 가능한 설정 크기의 상한
    []float64{1, 10000, 1000000, 4000000, 10000000, 40000000},
    monitoring.WithUnit(monitoring.Bytes),
)
```

---

## 12. 내부 모니터링 프레임워크

### 12.1 MetricDefinition

Istio의 모든 메트릭은 전역 레지스트리에 등록된다.

```go
// pkg/monitoring/monitoring.go:239-244
type MetricDefinition struct {
    Name        string
    Type        string
    Description string
    Bounds      []float64
}
```

### 12.2 메트릭 등록 순서

메트릭은 반드시 exporter 설정 전에 등록되어야 한다.

```go
// pkg/monitoring/monitoring.go:268-275
func (d *metrics) register(def MetricDefinition) {
    d.mu.Lock()
    defer d.mu.Unlock()
    if d.started {
        log.Fatalf("Attempting to initialize metric %q after metrics have started", def.Name)
    }
    d.known[def.Name] = def
}
```

### 12.3 RecordHook

모니터링 프레임워크는 메트릭 기록 시 콜백을 등록할 수 있는 `RecordHook` 인터페이스를 제공한다.

```go
// pkg/monitoring/monitoring.go:160-163
type RecordHook interface {
    OnRecord(name string, tags []LabelValue, value float64)
}
```

이를 통해 메트릭 값 변경 시 추가 로직(예: 알림, 집계)을 실행할 수 있다.

### 12.4 sendError 기록

xDS push 실패 시 오류 유형에 따라 적절한 메트릭이 기록된다.

```go
// pilot/pkg/xds/monitoring.go:162-178
func recordSendError(xdsType string, err error) bool {
    if isUnexpectedError(err) {
        switch xdsType {
        case v3.ListenerType:
            ldsSendErrPushes.Increment()
        case v3.ClusterType:
            cdsSendErrPushes.Increment()
        case v3.EndpointType:
            edsSendErrPushes.Increment()
        case v3.RouteType:
            rdsSendErrPushes.Increment()
        }
        return true
    }
    return false
}
```

`Unavailable`과 `Canceled` 코드는 정상적인 연결 종료로 간주되어 오류로 기록하지 않는다.

```go
// pilot/pkg/xds/monitoring.go:152-158
func isUnexpectedError(err error) bool {
    s, ok := status.FromError(err)
    isError := s.Code() != codes.Unavailable && s.Code() != codes.Canceled
    return !ok || isError
}
```

---

## 13. 요약

### 핵심 설계 원칙

| 원칙 | 구현 방식 |
|------|----------|
| **투명성** | 애플리케이션 코드 변경 없이 Envoy가 자동으로 텔레메트리 수집 |
| **통합 API** | Telemetry CRD로 메트릭/트레이싱/로깅을 일원화 관리 |
| **계층적 설정** | Root → Namespace → Workload 3단계 오버라이드 |
| **성능 최적화** | 메트릭 필터 캐싱, triggerMetric 사전 계산 |
| **확장성** | ExtensionProvider를 통한 다양한 백엔드 지원 |
| **폴백** | Telemetry API 미설정 시 레거시 MeshConfig로 폴백 |

### 소스코드 참조 요약

```
pilot/pkg/xds/monitoring.go         ← 컨트롤 플레인 xDS 메트릭 (11개)
pkg/xds/monitoring.go               ← xDS 오류 추적 메트릭 (6개)
pilot/pkg/bootstrap/monitoring.go   ← Istiod 서버 메트릭, /metrics 엔드포인트
pilot/pkg/model/telemetry.go        ← Telemetry API 처리, 메트릭 병합, 필터 생성
pilot/pkg/model/telemetry_logging.go ← 액세스 로그 설정, 포맷, OTel 로그
pilot/pkg/networking/core/tracing.go ← 트레이싱 프로바이더 설정
pilot/pkg/networking/core/accesslog.go ← AccessLogBuilder, 리스너 로그
pilot/pkg/features/telemetry.go     ← 텔레메트리 피처 플래그
pilot/pkg/networking/telemetry/telemetry.go ← StatPrefix 구성
pkg/monitoring/monitoring.go        ← OpenTelemetry 기반 모니터링 프레임워크
pkg/tracing/tracing.go             ← Istiod 자체 OTLP 트레이싱
istioctl/pkg/metrics/metrics.go    ← istioctl metrics CLI
manifests/addons/dashboards/       ← Grafana 대시보드 JSON
```

### 텔레메트리 세 축 비교

```
+------------------------------------------------------------------+
|     축     |   수집 방식   |    백엔드    |     설정 경로          |
|------------|-------------|------------|-------------------------|
| 메트릭     | Prometheus   | Prometheus | Telemetry API +         |
|            | scrape      | + Grafana  | MeshConfig              |
|------------|-------------|------------|-------------------------|
| 트레이싱   | Push         | Jaeger,    | Telemetry API +         |
|            | (gRPC/HTTP) | Zipkin,    | ExtensionProvider       |
|            |             | OTel Coll  |                         |
|------------|-------------|------------|-------------------------|
| 로깅      | File /       | stdout,    | Telemetry API +         |
|            | Push (gRPC) | gRPC ALS,  | MeshConfig              |
|            |             | OTel ALS   |                         |
+------------------------------------------------------------------+
```
