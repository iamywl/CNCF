# 22. Feature Control 및 Distributed Tracing Deep-Dive

> Alertmanager의 기능 플래그 시스템(Feature Control)과 OpenTelemetry 기반 분산 추적(Distributed Tracing) 서브시스템을 분석한다.
> 소스 기준: `featurecontrol/featurecontrol.go`, `tracing/tracing.go`, `tracing/config.go`, `tracing/http.go`

---

## 1. Feature Control 시스템

### 1.1 개요

Feature Control은 Alertmanager의 실험적 기능이나 동작 변경을 런타임에 활성화/비활성화하는 플래그 시스템이다. `--enable-feature` 명령행 옵션으로 제어한다.

```
alertmanager --enable-feature=receiver-name-in-metrics,auto-gomemlimit
```

### 1.2 Flagger 인터페이스

```go
// featurecontrol/featurecontrol.go:41
type Flagger interface {
    EnableAlertNamesInMetrics() bool
    EnableReceiverNamesInMetrics() bool
    ClassicMode() bool
    UTF8StrictMode() bool
    EnableAutoGOMEMLIMIT() bool
    EnableAutoGOMAXPROCS() bool
}
```

`Flagger`는 각 기능 플래그의 활성화 상태를 조회하는 인터페이스다. 컴포넌트가 이 인터페이스에 의존하므로 테스트에서 쉽게 모킹할 수 있다.

### 1.3 지원 기능 플래그 목록

```go
// featurecontrol/featurecontrol.go:23
const (
    FeatureAlertNamesInMetrics   = "alert-names-in-metrics"
    FeatureReceiverNameInMetrics = "receiver-name-in-metrics"
    FeatureClassicMode           = "classic-mode"
    FeatureUTF8StrictMode        = "utf8-strict-mode"
    FeatureAutoGOMEMLIMIT        = "auto-gomemlimit"
    FeatureAutoGOMAXPROCS        = "auto-gomaxprocs"
)
```

| 플래그 | 기능 | 위험도 | 용도 |
|--------|------|--------|------|
| `alert-names-in-metrics` | 알림 이름을 Prometheus 메트릭 레이블에 포함 | 높음 | 디버깅 (카디널리티 폭발 위험) |
| `receiver-name-in-metrics` | Receiver 이름을 메트릭 레이블에 포함 | 중간 | 실험적 기능 |
| `classic-mode` | UTF-8 이전 클래식 매처 모드 | 낮음 | 하위 호환성 |
| `utf8-strict-mode` | UTF-8 엄격 검증 모드 | 낮음 | 데이터 정합성 |
| `auto-gomemlimit` | Linux 컨테이너 메모리 한도에 맞춰 GOMEMLIMIT 자동 설정 | 중간 | 성능 최적화 |
| `auto-gomaxprocs` | Linux 컨테이너 CPU 쿼터에 맞춰 GOMAXPROCS 자동 설정 | 중간 | 성능 최적화 |

### 1.4 Flags 구조체 구현

```go
// featurecontrol/featurecontrol.go:50
type Flags struct {
    logger                       *slog.Logger
    enableAlertNamesInMetrics    bool
    enableReceiverNamesInMetrics bool
    classicMode                  bool
    utf8StrictMode               bool
    enableAutoGOMEMLIMIT         bool
    enableAutoGOMAXPROCS         bool
}
```

각 필드는 `bool` 타입으로, 단순한 on/off 토글이다. `Flagger` 인터페이스의 각 메서드는 해당 필드를 반환한다.

### 1.5 NewFlags 팩토리

```go
// featurecontrol/featurecontrol.go:122
func NewFlags(logger *slog.Logger, features string) (Flagger, error) {
    fc := &Flags{logger: logger}
    opts := []flagOption{}

    if len(features) == 0 {
        return NoopFlags{}, nil  // 빈 문자열이면 Noop 반환
    }

    for feature := range strings.SplitSeq(features, ",") {
        switch feature {
        case FeatureAlertNamesInMetrics:
            opts = append(opts, enableAlertNamesInMetrics())
            logger.Warn("Alert names in metrics enabled")
        case FeatureReceiverNameInMetrics:
            opts = append(opts, enableReceiverNameInMetrics())
            logger.Warn("Experimental receiver name in metrics enabled")
        // ... 나머지 플래그들
        default:
            return nil, fmt.Errorf("unknown option '%s' for --enable-feature", feature)
        }
    }

    for _, opt := range opts {
        opt(fc)
    }

    // 상호 배타 검증
    if fc.classicMode && fc.utf8StrictMode {
        return nil, errors.New("cannot have both classic and UTF-8 modes enabled")
    }

    return fc, nil
}
```

**핵심 설계 결정:**

1. **쉼표 구분 파싱**: `--enable-feature=a,b,c` 형식으로 여러 플래그를 한 번에 전달
2. **미지 플래그 에러**: 알 수 없는 플래그는 에러로 처리 (오타 방지)
3. **상호 배타 검증**: `classic-mode`와 `utf8-strict-mode`는 동시에 활성화 불가
4. **경고 로깅**: 모든 플래그 활성화 시 Warn 레벨로 로그 출력 (의도적 사용 확인)

### 1.6 Functional Options 패턴

```go
type flagOption func(flags *Flags)

func enableReceiverNameInMetrics() flagOption {
    return func(configs *Flags) {
        configs.enableReceiverNamesInMetrics = true
    }
}
```

각 플래그 활성화 함수는 `flagOption` 클로저를 반환한다. 이 패턴은:
- 개별 옵션을 독립적으로 테스트 가능
- 새 플래그 추가 시 기존 코드 변경 최소화
- 옵션 조합의 유효성 검증을 후처리 단계에서 수행

### 1.7 NoopFlags

```go
// featurecontrol/featurecontrol.go:166
type NoopFlags struct{}

func (n NoopFlags) EnableAlertNamesInMetrics() bool { return false }
func (n NoopFlags) EnableReceiverNamesInMetrics() bool { return false }
func (n NoopFlags) ClassicMode() bool { return false }
func (n NoopFlags) UTF8StrictMode() bool { return false }
func (n NoopFlags) EnableAutoGOMEMLIMIT() bool { return false }
func (n NoopFlags) EnableAutoGOMAXPROCS() bool { return false }
```

기능 플래그를 전혀 사용하지 않을 때 반환되는 Null Object 패턴 구현. 모든 메서드가 `false`를 반환한다.

### 1.8 Feature Control 전체 구조

```
┌──────────────────────────────────────────────────┐
│                 Feature Control                   │
│                                                   │
│  --enable-feature="a,b,c"                        │
│       │                                           │
│       ▼                                           │
│  NewFlags(logger, features)                       │
│       │                                           │
│       ├─ features == "" → NoopFlags{}             │
│       │                                           │
│       ├─ SplitSeq(",")                            │
│       │   ├─ "alert-names-in-metrics"             │
│       │   │   → enableAlertNamesInMetrics()       │
│       │   ├─ "receiver-name-in-metrics"           │
│       │   │   → enableReceiverNameInMetrics()     │
│       │   ├─ "classic-mode"                       │
│       │   │   → enableClassicMode()               │
│       │   ├─ "utf8-strict-mode"                   │
│       │   │   → enableUTF8StrictMode()            │
│       │   ├─ "auto-gomemlimit"                    │
│       │   │   → enableAutoGOMEMLIMIT()            │
│       │   ├─ "auto-gomaxprocs"                    │
│       │   │   → enableAutoGOMAXPROCS()            │
│       │   └─ unknown → error                      │
│       │                                           │
│       ├─ Apply flagOptions → Flags{}              │
│       │                                           │
│       └─ Validate: classic ∧ utf8 → error         │
│                                                   │
│  Flagger 인터페이스로 각 컴포넌트에 주입           │
│       ├─ notify/ → EnableReceiverNamesInMetrics() │
│       ├─ matcher/ → ClassicMode(), UTF8Strict()   │
│       └─ cmd/ → AutoGOMEMLIMIT(), AutoGOMAXPROCS()│
└──────────────────────────────────────────────────┘
```

### 1.9 각 기능 플래그의 영향

#### alert-names-in-metrics

알림 이름(`alertname` 레이블)을 Prometheus 메트릭의 레이블 값으로 포함한다.

**위험**: 알림 이름이 수백~수천 개일 수 있어 Prometheus 카디널리티가 폭발할 수 있다. 운영 환경에서는 주의 필요.

#### receiver-name-in-metrics

Receiver 이름을 `alertmanager_notifications_total` 등의 메트릭에 레이블로 추가한다.

**이전 동작**: Receiver 이름 없이 통합 이름(slack, email 등)만 포함
**변경 후**: `receiver="team-oncall"` 같은 사용자 정의 이름 추가

#### classic-mode / utf8-strict-mode

매처 파싱 모드를 제어한다:
- **classic-mode**: 기존 Prometheus 호환 매처 (`{alertname="foo"}`)
- **utf8-strict-mode**: UTF-8 전용 매처 (따옴표된 레이블 이름 지원)
- **기본 (둘 다 비활성)**: 하이브리드 모드 (자동 감지)

#### auto-gomemlimit / auto-gomaxprocs

Go 런타임 파라미터를 컨테이너 환경에 맞게 자동 조정한다:

- **GOMEMLIMIT**: cgroup v2 메모리 한도에 맞춰 GC 압력 최적화
- **GOMAXPROCS**: CPU cgroup 쿼터에 맞춰 고루틴 스케줄러 최적화

---

## 2. Distributed Tracing 시스템

### 2.1 개요

Alertmanager는 OpenTelemetry(OTel)를 사용하여 분산 추적을 지원한다. 알림의 전체 수명 주기를 추적할 수 있다: API 수신 → 디스패치 → 파이프라인 → 개별 Receiver 전송.

### 2.2 TracingConfig 구조

```go
// tracing/config.go:55
type TracingConfig struct {
    ClientType       TracingClientType    `yaml:"client_type,omitempty"`
    Endpoint         string               `yaml:"endpoint,omitempty"`
    SamplingFraction float64              `yaml:"sampling_fraction,omitempty"`
    Insecure         bool                 `yaml:"insecure,omitempty"`
    TLSConfig        *commoncfg.TLSConfig `yaml:"tls_config,omitempty"`
    Headers          *commoncfg.Headers   `yaml:"headers,omitempty"`
    Compression      string               `yaml:"compression,omitempty"`
    Timeout          model.Duration       `yaml:"timeout,omitempty"`
}
```

| 필드 | 타입 | 기본값 | 설명 |
|------|------|--------|------|
| `client_type` | `"http"` / `"grpc"` | `"grpc"` | OTLP 전송 프로토콜 |
| `endpoint` | string | (없음) | OTel Collector 주소 |
| `sampling_fraction` | float64 | 0.0 | 샘플링 비율 (0.0~1.0) |
| `insecure` | bool | false | TLS 비활성화 |
| `tls_config` | TLSConfig | nil | TLS 인증서 설정 |
| `headers` | Headers | nil | 커스텀 HTTP/gRPC 헤더 |
| `compression` | string | "" | 압축 ("gzip") |
| `timeout` | Duration | 0 | 전송 타임아웃 |

### 2.3 YAML 설정 예시

```yaml
tracing:
  client_type: grpc
  endpoint: otel-collector:4317
  sampling_fraction: 0.1
  insecure: false
  tls_config:
    cert_file: /etc/certs/cert.pem
    key_file: /etc/certs/key.pem
  compression: gzip
  timeout: 5s
```

### 2.4 TracingClientType

```go
// tracing/config.go:28
type TracingClientType string

const (
    TracingClientHTTP TracingClientType = "http"
    TracingClientGRPC TracingClientType = "grpc"
)
```

OTLP(OpenTelemetry Protocol)는 두 가지 전송 방식을 지원한다:

| 프로토콜 | 기본 포트 | 용도 |
|---------|----------|------|
| gRPC | 4317 | 고성능, 양방향 스트리밍 |
| HTTP | 4318 | 방화벽 우회, 프록시 호환 |

### 2.5 설정 검증

```go
// tracing/config.go:73
func (t *TracingConfig) UnmarshalYAML(unmarshal func(any) error) error {
    *t = TracingConfig{
        ClientType: TracingClientGRPC,  // 기본값: gRPC
    }
    type plain TracingConfig
    if err := unmarshal((*plain)(t)); err != nil {
        return err
    }
    if t.Endpoint == "" {
        return errors.New("tracing endpoint must be set")
    }
    if t.Compression != "" && t.Compression != GzipCompression {
        return fmt.Errorf("invalid compression type %s", t.Compression)
    }
    return nil
}
```

**검증 규칙:**
- `endpoint`는 필수 (빈 문자열 불허)
- `compression`은 `"gzip"` 또는 빈 문자열만 허용
- `client_type`은 `"http"` 또는 `"grpc"`만 허용

### 2.6 Tracing Manager

```go
// tracing/tracing.go:42
type Manager struct {
    logger       *slog.Logger
    done         chan struct{}
    config       TracingConfig
    shutdownFunc func() error
}
```

Manager는 TracerProvider의 수명 주기를 관리한다.

#### Manager.Run()

```go
func (m *Manager) Run() {
    otel.SetTextMapPropagator(propagation.TraceContext{})
    otel.SetErrorHandler(otelErrHandler(func(err error) {
        m.logger.Error("OpenTelemetry handler returned an error", "err", err)
    }))
    <-m.done  // 블로킹 — 종료 신호까지 대기
}
```

**핵심 동작:**
1. W3C TraceContext 전파기를 글로벌 등록 — 모든 HTTP 요청/응답에 `traceparent` 헤더 자동 전파
2. 글로벌 OTel 오류 핸들러 등록 — 내부 오류를 구조화된 로그로 출력
3. `done` 채널에서 블로킹 — 고루틴으로 실행됨

#### Manager.ApplyConfig()

```go
// tracing/tracing.go:69
func (m *Manager) ApplyConfig(cfg TracingConfig) error {
    // 변경 감지: 설정이 같고 TLS가 없으면 스킵
    if reflect.DeepEqual(m.config, cfg) &&
       (m.config.TLSConfig == nil || *m.config.TLSConfig == blankTLSConfig) {
        return nil
    }

    // 기존 provider 종료
    if m.shutdownFunc != nil {
        if err := m.shutdownFunc(); err != nil {
            return fmt.Errorf("failed to shut down: %w", err)
        }
    }

    // endpoint 비어 있으면 트레이싱 비활성화
    if cfg.Endpoint == "" {
        otel.SetTracerProvider(noop.NewTracerProvider())
        m.logger.Info("Tracing provider uninstalled.")
        return nil
    }

    // 새 provider 생성 및 설치
    tp, shutdownFunc, err := buildTracerProvider(ctx, cfg)
    otel.SetTracerProvider(tp)
    m.logger.Info("Successfully installed a new tracer provider.")
    return nil
}
```

**왜 설정 변경을 감지하는가?**

설정 리로드(SIGHUP)마다 `ApplyConfig`가 호출되지만, 변경이 없으면 불필요한 provider 재생성을 피한다. 단, TLS 설정이 있으면 인증서 갱신을 위해 항상 재생성한다.

#### Manager.Stop()

```go
func (m *Manager) Stop() {
    defer close(m.done)   // Run()의 블로킹 해제
    if m.shutdownFunc == nil {
        return
    }
    if err := m.shutdownFunc(); err != nil {
        m.logger.Error("failed to shut down", "err", err)
    }
}
```

### 2.7 TracerProvider 빌드

```go
// tracing/tracing.go:129
func buildTracerProvider(ctx context.Context, cfg TracingConfig) (
    trace.TracerProvider, func() error, error) {

    // 1. OTLP 클라이언트 생성 (gRPC 또는 HTTP)
    client, err := getClient(cfg)

    // 2. OTLP Exporter 생성
    exp, err := otlptrace.New(ctx, client)

    // 3. 서비스 리소스 정의
    res, err := resource.New(ctx,
        resource.WithSchemaURL(semconv.SchemaURL),
        resource.WithAttributes(
            semconv.ServiceNameKey.String("alertmanager"),
            semconv.ServiceVersionKey.String(version.Version),
        ),
        resource.WithProcessRuntimeDescription(),
        resource.WithTelemetrySDK(),
    )

    // 4. TracerProvider 생성
    tp := tracesdk.NewTracerProvider(
        tracesdk.WithBatcher(exp),                    // 배치 전송
        tracesdk.WithSampler(tracesdk.ParentBased(    // 부모 기반 샘플링
            tracesdk.TraceIDRatioBased(cfg.SamplingFraction),
        )),
        tracesdk.WithResource(res),
    )

    // 5. Shutdown 함수 반환 (5초 타임아웃)
    return tp, func() error {
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()
        return tp.Shutdown(ctx)
    }, nil
}
```

**샘플링 전략:**

```
ParentBased(TraceIDRatioBased(fraction))
```

- **ParentBased**: 부모 스팬의 샘플링 결정을 따름
  - 부모가 샘플링됨 → 자식도 샘플링
  - 부모가 없음 → `TraceIDRatioBased` 적용
- **TraceIDRatioBased(0.1)**: TraceID 해시 기반 10% 확률 샘플링

이 조합으로 분산 시스템에서 동일 트레이스의 모든 스팬이 일관되게 샘플링/드롭된다.

### 2.8 OTLP 클라이언트 구성

```go
// tracing/tracing.go:199
func getClient(cfg TracingConfig) (otlptrace.Client, error) {
    switch cfg.ClientType {
    case TracingClientGRPC:
        opts := []otlptracegrpc.Option{
            otlptracegrpc.WithEndpoint(cfg.Endpoint),
        }
        // TLS/Insecure/Compression/Headers/Timeout 옵션 추가
        switch {
        case cfg.Insecure:
            opts = append(opts, otlptracegrpc.WithInsecure())
        case cfg.TLSConfig != nil:
            tlsConf, _ := commoncfg.NewTLSConfig(cfg.TLSConfig)
            opts = append(opts, otlptracegrpc.WithTLSCredentials(
                credentials.NewTLS(tlsConf)))
        }
        // ...
        client = otlptracegrpc.NewClient(opts...)

    case TracingClientHTTP:
        opts := []otlptracehttp.Option{
            otlptracehttp.WithEndpoint(cfg.Endpoint),
        }
        // 유사한 옵션 처리 (HTTP 전용)
        // ...
        client = otlptracehttp.NewClient(opts...)
    }
    return client, nil
}
```

**gRPC vs HTTP 차이:**

| 옵션 | gRPC | HTTP |
|------|------|------|
| TLS | `WithTLSCredentials(credentials.NewTLS())` | `WithTLSClientConfig()` |
| 압축 | `WithCompressor("gzip")` | `WithCompression(GzipCompression)` |
| Insecure | `WithInsecure()` | `WithInsecure()` |

### 2.9 HTTP 트레이싱 미들웨어

```go
// tracing/http.go:32
func Transport(rt http.RoundTripper) http.RoundTripper {
    rt = otelhttp.NewTransport(rt,
        otelhttp.WithClientTrace(func(ctx context.Context) *httptrace.ClientTrace {
            return otelhttptrace.NewClientTrace(ctx)
        }),
    )
    return rt
}
```

**용도**: 모든 Receiver의 HTTP 클라이언트에 적용. 외부 서비스로의 HTTP 요청마다 자동으로:
- 스팬 생성 (HTTP 메서드, URL, 상태 코드 기록)
- `traceparent` 헤더로 컨텍스트 전파
- DNS 해석, 연결, TLS 핸드셰이크 시간 기록 (ClientTrace)

```go
// tracing/http.go:44
func Middleware(handler http.Handler) http.Handler {
    return otelhttp.NewHandler(handler, "",
        otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
            return fmt.Sprintf("%s %s", r.Method, r.URL.Path)
        }),
    )
}
```

**용도**: Alertmanager의 HTTP 서버(API)에 적용. 들어오는 요청마다:
- `"GET /api/v2/alerts"` 형식의 스팬 이름 생성
- 요청 시작~응답 완료까지 스팬 기록

### 2.10 Notify 통합의 트레이싱

```go
// notify/notify.go:44
var tracer = otel.Tracer("github.com/prometheus/alertmanager/notify")

// notify/notify.go:90
func (i *Integration) Notify(ctx context.Context, alerts ...*types.Alert) (bool, error) {
    ctx, span := tracer.Start(ctx, "notify.Integration.Notify",
        trace.WithAttributes(
            attribute.String("alerting.notify.integration.name", i.name),
            attribute.Int("alerting.alerts.count", len(alerts)),
        ),
        trace.WithSpanKind(trace.SpanKindClient),
    )
    defer func() {
        span.SetAttributes(attribute.Bool("alerting.notify.error.recoverable", recoverable))
        if err != nil {
            span.SetStatus(codes.Error, err.Error())
        }
        span.End()
    }()
    return i.notifier.Notify(ctx, alerts...)
}
```

**스팬 속성:**

| 속성 | 값 예시 | 설명 |
|------|---------|------|
| `alerting.notify.integration.name` | `"slack"` | Receiver 타입 |
| `alerting.alerts.count` | `3` | 전송할 알림 수 |
| `alerting.notify.error.recoverable` | `true/false` | 재시도 가능 여부 |

### 2.11 전체 추적 흐름

```
┌────────────────────────────────────────────────────────────────┐
│                    Alertmanager 추적 흐름                       │
│                                                                │
│  [Prometheus] ─traceparent→ [Alertmanager API]                 │
│                                 │                              │
│  API 수신 스팬 ─────────────────┤                              │
│  "POST /api/v2/alerts"          │                              │
│                                 ▼                              │
│  Dispatcher 스팬 ─────────────────┤                            │
│  "dispatch.processAlert"         │                             │
│                                  ▼                             │
│  Notify Pipeline 스팬 ──────────────┤                          │
│  "notify.Pipeline.Exec"            │                           │
│                                    ▼                           │
│  Integration 스팬 ────────────────────┤                        │
│  "notify.Integration.Notify"         │                         │
│  {name="slack", count=3}             │                         │
│                                      ▼                         │
│  HTTP Client 스팬 ──────────────────────┤                      │
│  "POST https://hooks.slack.com/xxx"    │                       │
│  {status=200}                          │                        │
│                                        ▼                       │
│                                  [Slack API]                   │
└────────────────────────────────────────────────────────────────┘
```

### 2.12 headersToMap 유틸리티

```go
// tracing/tracing.go:177
func headersToMap(headers *commoncfg.Headers) (map[string]string, error) {
    if headers == nil || len(headers.Headers) == 0 {
        return nil, nil
    }
    result := make(map[string]string)
    for name, header := range headers.Headers {
        if len(header.Values) > 0 {
            result[name] = header.Values[0]
        } else if len(header.Secrets) > 0 {
            result[name] = string(header.Secrets[0])
        } else if len(header.Files) > 0 {
            return nil, fmt.Errorf("header files not supported for tracing")
        }
    }
    return result, nil
}
```

prometheus/common의 `Headers` 구조체를 OTLP 클라이언트가 이해하는 `map[string]string`으로 변환한다. 파일 기반 헤더는 지원하지 않는다.

---

## 3. Feature Control과 Tracing의 상호작용

### 3.1 receiver-name-in-metrics + 트레이싱

`receiver-name-in-metrics` 플래그가 활성화되면 메트릭에 receiver 이름이 추가되고, 트레이싱 스팬에도 이미 `integration.name` 속성으로 포함된다. 이 조합으로 특정 receiver의 성능 문제를 메트릭과 트레이스 양쪽에서 진단할 수 있다.

### 3.2 alert-names-in-metrics + 트레이싱

마찬가지로 알림 이름이 메트릭과 스팬 모두에 나타나, 특정 알림 규칙의 전체 수명 주기를 추적할 수 있다.

---

## 4. 설계 패턴 분석

### 4.1 Feature Control의 Null Object 패턴

`NoopFlags{}`는 Null Object 패턴의 전형적 구현이다. 기능 플래그를 사용하지 않을 때 nil 체크 없이 안전하게 호출할 수 있다.

### 4.2 Feature Control의 Functional Options 패턴

```go
type flagOption func(flags *Flags)
```

각 플래그를 독립적인 함수로 캡슐화하여 조합 가능하게 만든다.

### 4.3 Tracing Manager의 Hot Reload 패턴

`ApplyConfig()`로 런타임에 트레이싱 설정을 변경할 수 있다. 설정 리로드(SIGHUP) 시 기존 provider를 안전하게 종료하고 새 provider를 설치한다.

### 4.4 Tracing의 Decorator 패턴

`tracing.Transport()`와 `tracing.Middleware()`는 기존 HTTP 핸들러/트랜스포트를 래핑하여 투명하게 트레이싱을 추가한다.

---

## 5. 운영 가이드

### 5.1 Feature Flag 활성화

```bash
# 단일 플래그
alertmanager --enable-feature=auto-gomemlimit

# 복수 플래그
alertmanager --enable-feature=auto-gomemlimit,auto-gomaxprocs,receiver-name-in-metrics
```

### 5.2 트레이싱 활성화 (YAML)

```yaml
# alertmanager.yml
tracing:
  client_type: grpc
  endpoint: tempo:4317
  sampling_fraction: 0.05   # 5% 샘플링
  compression: gzip
```

### 5.3 모니터링 체크리스트

| 항목 | 메트릭/추적 | 설명 |
|------|------------|------|
| 알림 전송 성공률 | `alertmanager_notifications_total` | Receiver별 성공/실패 |
| 전송 지연 | 트레이싱 스팬 duration | Receiver별 응답 시간 |
| 샘플링 비율 | `otel_trace_span_events_total` | 실제 전송되는 스팬 수 |
| Feature 상태 | 시작 로그 | Warn 레벨 로그 확인 |

---

## 6. 정리

| 항목 | Feature Control | Distributed Tracing |
|------|----------------|-------------------|
| 소스 위치 | `featurecontrol/` | `tracing/` |
| 인터페이스 | `Flagger` | `trace.TracerProvider` |
| 설정 방식 | `--enable-feature` CLI 플래그 | YAML 설정 파일 |
| 런타임 변경 | 불가 (시작 시 고정) | 가능 (SIGHUP 리로드) |
| 핵심 패턴 | Functional Options, Null Object | Decorator, Hot Reload |
| 플래그/옵션 수 | 6개 | 8개 설정 필드 |
| 상호작용 | 메트릭 레이블 제어 | 분산 추적 데이터 수집 |

---

*소스 참조: `featurecontrol/featurecontrol.go`, `tracing/tracing.go`, `tracing/config.go`, `tracing/http.go`, `notify/notify.go`*
