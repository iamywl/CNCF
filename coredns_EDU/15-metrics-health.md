# 15. 메트릭과 헬스체크 Deep-Dive

## 개요

CoreDNS는 세 가지 HTTP 엔드포인트를 통해 운영 가시성(Observability)을 제공한다:

| 엔드포인트 | 플러그인 | 기본 포트 | 용도 |
|-----------|---------|----------|------|
| `/metrics` | prometheus | 9153 | Prometheus 메트릭 수집 |
| `/health` | health | 8080 | 프로세스 생존 확인 (Liveness) |
| `/ready` | ready | 8181 | 서비스 준비 상태 확인 (Readiness) |

이 세 플러그인은 각각 독립적인 HTTP 서버를 운영하며, Kubernetes의 liveness/readiness probe와 자연스럽게 통합된다.

소스 경로:
- `plugin/metrics/` - Prometheus 메트릭
- `plugin/metrics/vars/` - 메트릭 변수 정의와 Report 함수
- `plugin/health/` - 헬스체크
- `plugin/ready/` - 레디니스 체크
- `plugin/pkg/dnstest/` - 테스트용 Recorder

---

## Prometheus 메트릭 플러그인

### Metrics 구조체

```
// plugin/metrics/metrics.go

type Metrics struct {
    Next    plugin.Handler
    Addr    string                       // 리스닝 주소 (기본: localhost:9153)
    Reg     *prometheus.Registry         // Prometheus 레지스트리

    ln      net.Listener
    lnSetup bool

    mux     *http.ServeMux
    srv     *http.Server

    zoneNames []string                   // 이 서버 블록의 zone 이름들
    zoneMap   map[string]struct{}        // zone 이름 → 존재 여부
    zoneMu    sync.RWMutex              // zone 목록 동시성 보호

    plugins   map[string]struct{}        // 사용 가능한 모든 플러그인 목록
}
```

**핵심 설계 포인트:**

1. **Prometheus Registry 사용**: `prometheus.DefaultRegisterer`를 기본으로 사용하지만, 동일 주소에서 여러 서버 블록이 메트릭을 노출할 수 있도록 Registry를 공유한다.

2. **Zone 동적 관리**: `AddZone()`/`RemoveZone()`으로 zone 목록을 런타임에 변경할 수 있다. RWMutex로 동시성을 보호한다.

3. **플러그인 목록 관리**: `plugins` 맵은 어떤 플러그인이 응답을 작성했는지 판별하는 데 사용된다.

### 생성 및 초기화

```
func New(addr string) *Metrics {
    met := &Metrics{
        Addr:    addr,
        Reg:     prometheus.DefaultRegisterer.(*prometheus.Registry),
        zoneMap: make(map[string]struct{}),
        plugins: pluginList(caddy.ListPlugins()),
    }
    return met
}
```

`pluginList`는 Caddy에 등록된 모든 플러그인에서 `dns.` 접두사를 제거하여 플러그인 이름 맵을 생성한다:

```
func pluginList(m map[string][]string) map[string]struct{} {
    pm := map[string]struct{}{}
    for _, p := range m["others"] {
        if len(p) > 3 {
            pm[p[4:]] = struct{}{}   // "dns.forward" → "forward"
        }
    }
    return pm
}
```

### OnStartup - HTTP 서버 시작

```
func (m *Metrics) OnStartup() error {
    ln, err := reuseport.Listen("tcp", m.Addr)
    if err != nil {
        log.Errorf("Failed to start metrics handler: %s", err)
        return err
    }

    m.ln = ln
    m.lnSetup = true

    m.mux = http.NewServeMux()
    m.mux.Handle("/metrics", promhttp.HandlerFor(m.Reg, promhttp.HandlerOpts{}))

    server := &http.Server{
        Handler:      m.mux,
        ReadTimeout:  5 * time.Second,
        WriteTimeout: 5 * time.Second,
        IdleTimeout:  5 * time.Second,
    }
    m.srv = server

    go func() {
        server.Serve(ln)
    }()

    ListenAddr = ln.Addr().String()
    return nil
}
```

**HTTP 서버 설정:**

| 설정 | 값 | 이유 |
|------|-----|------|
| ReadTimeout | 5초 | 느린 클라이언트 보호 |
| WriteTimeout | 5초 | 메트릭 스크래핑 제한 |
| IdleTimeout | 5초 | 유휴 연결 정리 |
| reuseport | 사용 | SO_REUSEPORT로 graceful reload 지원 |

`reuseport.Listen()`은 SO_REUSEPORT 소켓 옵션을 사용하여 동일 포트에서 여러 프로세스가 리스닝할 수 있게 한다. 이는 CoreDNS의 설정 리로드(재시작) 시 다운타임을 방지한다.

### Zone 관리

```
func (m *Metrics) AddZone(z string) {
    m.zoneMu.Lock()
    m.zoneMap[z] = struct{}{}
    m.zoneNames = keys(m.zoneMap)    // 맵에서 슬라이스 재생성
    m.zoneMu.Unlock()
}

func (m *Metrics) RemoveZone(z string) {
    m.zoneMu.Lock()
    delete(m.zoneMap, z)
    m.zoneNames = keys(m.zoneMap)
    m.zoneMu.Unlock()
}

func (m *Metrics) ZoneNames() []string {
    m.zoneMu.RLock()
    s := m.zoneNames
    m.zoneMu.RUnlock()
    return s
}
```

Zone 이름 목록은 메트릭 핸들러(`ServeDNS`)에서 요청의 zone을 매칭할 때 사용된다. 맵과 슬라이스를 동시에 유지하여 O(1) 추가/삭제와 빠른 순회를 모두 지원한다.

### 종료 및 재시작

```
func (m *Metrics) OnRestart() error {
    if !m.lnSetup { return nil }
    u.Unset(m.Addr)
    return m.stopServer()
}

func (m *Metrics) stopServer() error {
    if !m.lnSetup { return nil }
    ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
    defer cancel()
    if err := m.srv.Shutdown(ctx); err != nil {
        log.Infof("Failed to stop prometheus http server: %s", err)
        return err
    }
    m.lnSetup = false
    m.ln.Close()
    return nil
}

func (m *Metrics) OnFinalShutdown() error { return m.stopServer() }
```

`Shutdown(ctx)`은 Go 표준 라이브러리의 graceful shutdown을 사용한다. 5초 내에 진행 중인 요청이 완료되지 않으면 강제 종료한다.

---

## 메트릭 정의 (vars 패키지)

### 핵심 메트릭 목록

```
// plugin/metrics/vars/vars.go

// 모든 메트릭의 Namespace: "coredns"
// Subsystem: "dns"
```

#### 1. RequestCount (카운터)

```
RequestCount = promauto.NewCounterVec(prometheus.CounterOpts{
    Namespace: plugin.Namespace,
    Subsystem: "dns",
    Name:      "requests_total",
    Help:      "Counter of DNS requests made per zone, protocol and family.",
}, []string{"server", "zone", "view", "proto", "family", "type"})
```

**메트릭 이름:** `coredns_dns_requests_total`

| 레이블 | 예시 | 설명 |
|--------|------|------|
| server | `dns://:53` | 서버 주소 |
| zone | `example.org.` | 매칭된 zone |
| view | `internal` | view 이름 (없으면 빈 문자열) |
| proto | `udp`, `tcp` | 전송 프로토콜 |
| family | `1`, `2` | 1=IPv4, 2=IPv6 |
| type | `A`, `AAAA`, `MX` | 쿼리 타입 |

#### 2. RequestDuration (히스토그램)

```
RequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
    Namespace:                   plugin.Namespace,
    Subsystem:                   "dns",
    Name:                        "request_duration_seconds",
    Buckets:                     plugin.TimeBuckets,
    NativeHistogramBucketFactor: plugin.NativeHistogramBucketFactor,
    Help:                        "Histogram of the time (in seconds) each request took per zone.",
}, []string{"server", "zone", "view"})
```

**메트릭 이름:** `coredns_dns_request_duration_seconds`

요청 처리 시간을 초 단위로 측정한다. Prometheus의 히스토그램 버킷과 Native Histogram을 모두 지원한다.

#### 3. RequestSize (히스토그램)

```
RequestSize = promauto.NewHistogramVec(prometheus.HistogramOpts{
    Namespace: plugin.Namespace,
    Subsystem: "dns",
    Name:      "request_size_bytes",
    Help:      "Size of the EDNS0 UDP buffer in bytes (64K for TCP) per zone and protocol.",
    Buckets:   []float64{0, 100, 200, 300, 400, 511, 1023, 2047, 4095, 8291, 16e3, 32e3, 48e3, 64e3},
}, []string{"server", "zone", "view", "proto"})
```

**메트릭 이름:** `coredns_dns_request_size_bytes`

DNS 요청 메시지 크기 분포. 버킷 경계는 DNS 메시지의 일반적 크기 분포에 맞게 설계되었다:
- 511: 전통적 UDP 최대 크기 근처
- 1023: 소규모 EDNS0
- 4095: 일반적 EDNS0 버퍼
- 64000: TCP 최대

#### 4. RequestDo (카운터)

```
RequestDo = promauto.NewCounterVec(prometheus.CounterOpts{
    Namespace: plugin.Namespace,
    Subsystem: "dns",
    Name:      "do_requests_total",
    Help:      "Counter of DNS requests with DO bit set per zone.",
}, []string{"server", "zone", "view"})
```

**메트릭 이름:** `coredns_dns_do_requests_total`

DNSSEC OK 비트가 설정된 요청 수. DNSSEC 채택률 모니터링에 유용하다.

#### 5. ResponseSize (히스토그램)

```
ResponseSize = promauto.NewHistogramVec(prometheus.HistogramOpts{
    Namespace: plugin.Namespace,
    Subsystem: "dns",
    Name:      "response_size_bytes",
    Help:      "Size of the returned response in bytes.",
    Buckets:   []float64{0, 100, 200, 300, 400, 511, 1023, 2047, 4095, 8291, 16e3, 32e3, 48e3, 64e3},
}, []string{"server", "zone", "view", "proto"})
```

**메트릭 이름:** `coredns_dns_response_size_bytes`

#### 6. ResponseRcode (카운터)

```
ResponseRcode = promauto.NewCounterVec(prometheus.CounterOpts{
    Namespace: plugin.Namespace,
    Subsystem: "dns",
    Name:      "responses_total",
    Help:      "Counter of response status codes.",
}, []string{"server", "zone", "view", "rcode", "plugin"})
```

**메트릭 이름:** `coredns_dns_responses_total`

| 레이블 | 예시 | 설명 |
|--------|------|------|
| rcode | `NOERROR`, `NXDOMAIN`, `SERVFAIL` | DNS 응답 코드 |
| plugin | `forward`, `file`, `cache` | 응답을 작성한 플러그인 |

이 메트릭은 운영에서 가장 중요한 메트릭 중 하나이다. `SERVFAIL` 비율이 높으면 upstream 장애, `NXDOMAIN` 비율이 높으면 잘못된 쿼리 패턴을 나타낸다.

#### 7. Panic (카운터)

```
Panic = promauto.NewCounter(prometheus.CounterOpts{
    Namespace: plugin.Namespace,
    Name:      "panics_total",
    Help:      "A metrics that counts the number of panics.",
})
```

**메트릭 이름:** `coredns_panics_total`

CoreDNS 내부에서 발생한 패닉 수. 0이 아니면 버그가 있다는 의미이다.

#### 8. PluginEnabled (게이지)

```
PluginEnabled = promauto.NewGaugeVec(prometheus.GaugeOpts{
    Namespace: plugin.Namespace,
    Name:      "plugin_enabled",
    Help:      "A metric that indicates whether a plugin is enabled on per server and zone basis.",
}, []string{"server", "zone", "view", "name"})
```

**메트릭 이름:** `coredns_plugin_enabled`

서버/zone별로 어떤 플러그인이 활성화되어 있는지 나타내는 게이지. 값은 항상 1이다. 설정 검증에 유용하다.

#### 9. HTTPS/QUIC 응답 카운터

```
HTTPSResponsesCount = promauto.NewCounterVec(prometheus.CounterOpts{
    Namespace: plugin.Namespace,
    Subsystem: "dns",
    Name:      "https_responses_total",
    Help:      "Counter of DoH responses per server and http status code.",
}, []string{"server", "status"})

HTTPS3ResponsesCount = promauto.NewCounterVec(prometheus.CounterOpts{
    Namespace: plugin.Namespace,
    Subsystem: "dns",
    Name:      "https3_responses_total",
    Help:      "Counter of DoH3 responses per server and http status code.",
}, []string{"server", "status"})

QUICResponsesCount = promauto.NewCounterVec(prometheus.CounterOpts{
    Namespace: plugin.Namespace,
    Subsystem: "dns",
    Name:      "quic_responses_total",
    Help:      "Counter of DoQ responses per server and QUIC application code.",
}, []string{"server", "status"})
```

DoH(DNS over HTTPS), DoH3, DoQ(DNS over QUIC) 프로토콜별 응답 카운터.

#### 10. BuildInfo (게이지)

```
// plugin/metrics/metrics.go

var buildInfo = promauto.NewGaugeVec(prometheus.GaugeOpts{
    Namespace: plugin.Namespace,
    Name:      "build_info",
    Help:      "A metric with a constant '1' value labeled by version, revision, and goversion.",
}, []string{"version", "revision", "goversion"})
```

**메트릭 이름:** `coredns_build_info`

빌드 정보를 메트릭으로 노출. 버전, Git 커밋, Go 버전을 레이블로 포함한다.

### 전체 메트릭 요약 테이블

```
+---------------------------------------------+----------+
| 메트릭 이름                                 | 타입     |
+---------------------------------------------+----------+
| coredns_dns_requests_total                  | Counter  |
| coredns_dns_request_duration_seconds        | Histogram|
| coredns_dns_request_size_bytes              | Histogram|
| coredns_dns_do_requests_total               | Counter  |
| coredns_dns_response_size_bytes             | Histogram|
| coredns_dns_responses_total                 | Counter  |
| coredns_panics_total                        | Counter  |
| coredns_plugin_enabled                      | Gauge    |
| coredns_dns_https_responses_total           | Counter  |
| coredns_dns_https3_responses_total          | Counter  |
| coredns_dns_quic_responses_total            | Counter  |
| coredns_build_info                          | Gauge    |
| coredns_health_request_duration_seconds     | Histogram|
| coredns_health_request_failures_total       | Counter  |
+---------------------------------------------+----------+
```

---

## Report 함수 - 메트릭 보고

### Report 함수 시그니처

```
// plugin/metrics/vars/report.go

type ReportOptions struct {
    OriginalReqSize int
}

type ReportOption func(*ReportOptions)

func WithOriginalReqSize(size int) ReportOption {
    return func(opts *ReportOptions) {
        opts.OriginalReqSize = size
    }
}

func Report(server string, req request.Request, zone, view, rcode, plugin string,
    size int, start time.Time, opts ...ReportOption) {
    // ...
}
```

**Functional Options 패턴:** `ReportOption`을 사용하여 선택적 파라미터를 전달한다. 이를 통해 기존 호출을 변경하지 않으면서 새로운 옵션을 추가할 수 있다.

### Report 내부 동작

```
func Report(server string, req request.Request, zone, view, rcode, plugin string,
    size int, start time.Time, opts ...ReportOption) {

    options := ReportOptions{OriginalReqSize: 0}
    for _, opt := range opts {
        opt(&options)
    }

    // 1. 프로토콜과 패밀리
    net := req.Proto()
    fam := "1"
    if req.Family() == 2 {
        fam = "2"
    }

    // 2. DNSSEC OK 비트
    if req.Do() {
        RequestDo.WithLabelValues(server, zone, view).Inc()
    }

    // 3. 요청 카운터
    qType := qTypeString(req.QType())
    RequestCount.WithLabelValues(server, zone, view, net, fam, qType).Inc()

    // 4. 요청 처리 시간
    RequestDuration.WithLabelValues(server, zone, view).Observe(time.Since(start).Seconds())

    // 5. 응답 크기
    ResponseSize.WithLabelValues(server, zone, view, net).Observe(float64(size))

    // 6. 요청 크기 (원본 크기 우선)
    reqSize := req.Len()
    if options.OriginalReqSize > 0 {
        reqSize = options.OriginalReqSize
    }
    RequestSize.WithLabelValues(server, zone, view, net).Observe(float64(reqSize))

    // 7. 응답 코드
    ResponseRcode.WithLabelValues(server, zone, view, rcode, plugin).Inc()
}
```

**Report가 호출되는 두 곳:**

1. **Metrics 플러그인 (handler.go)**: 정상적인 플러그인 체인을 통과한 요청

```
// plugin/metrics/handler.go
vars.Report(WithServer(ctx), state, zone, WithView(ctx),
            rcode.ToString(rc), rw.Plugin,
            rw.Len, rw.Start, vars.WithOriginalReqSize(originalSize))
```

2. **서버 (server.go)**: 플러그인 체인에 도달하지 못한 요청 (잘못된 요청, 매칭 실패 등)

```
// core/dnsserver/server.go
vars.Report(server, state, vars.Dropped, "", rcode.ToString(rc),
            "" /* plugin */, answer.Len(), time.Now())
```

`vars.Dropped`는 `"dropped"` 문자열로, 유효한 zone 이름이 될 수 없어(trailing dot 없음) 정상 트래픽과 구분된다.

### Report 데이터 흐름

```
DNS 요청 수신
     |
     v
+------------------+
| Metrics.ServeDNS |
|  state 생성      |
|  Recorder 생성   |
|  originalSize    |
|  캡처            |
+------------------+
     |
     v
+------------------+
| 다음 플러그인들  |
| (체인 실행)      |
+------------------+
     |
     v (Recorder에 기록됨)
     |  - Rcode
     |  - Len (응답 크기)
     |  - Plugin (응답 플러그인)
     |  - Start (시작 시간)
     |
     v
+------------------+
| vars.Report()    |
|  7개 메트릭 업데이트 |
+------------------+
     |
     v
Prometheus 스크래핑 → /metrics 엔드포인트
```

---

## Setup - 메트릭 플러그인 설정

### setup 함수

```
// plugin/metrics/setup.go

func init() { plugin.Register("prometheus", setup) }

func setup(c *caddy.Controller) error {
    m, err := parse(c)
    if err != nil { return plugin.Error("prometheus", err) }

    // Registry 공유 (동일 주소에서 하나의 Registry)
    m.Reg = registry.getOrSet(m.Addr, m.Reg)

    // Startup: Registry 설정 + HTTP 서버 시작
    c.OnStartup(func() error {
        m.Reg = registry.getOrSet(m.Addr, m.Reg)
        u.Set(m.Addr, m.OnStartup)
        return nil
    })
    c.OnStartup(func() error { return u.ForEach() })

    // PluginEnabled 메트릭 설정
    c.OnStartup(func() error {
        conf := dnsserver.GetConfig(c)
        for _, h := range conf.ListenHosts {
            addrstr := conf.Transport + "://" + net.JoinHostPort(h, conf.Port)
            for _, p := range conf.Handlers() {
                vars.PluginEnabled.WithLabelValues(addrstr, conf.Zone, conf.ViewName, p.Name()).Set(1)
            }
        }
        return nil
    })

    // Restart/Shutdown 핸들러
    c.OnRestart(m.OnRestart)
    c.OnRestart(func() error { vars.PluginEnabled.Reset(); return nil })
    c.OnFinalShutdown(m.OnFinalShutdown)

    // 빌드 정보
    buildInfo.WithLabelValues(coremain.CoreVersion, coremain.GitCommit, runtime.Version()).Set(1)

    // 플러그인 체인에 등록
    dnsserver.GetConfig(c).AddPlugin(func(next plugin.Handler) plugin.Handler {
        m.Next = next
        return m
    })
    return nil
}
```

**`uniq` 패턴:** `u.Set(addr, m.OnStartup)`과 `u.ForEach()`는 동일한 주소에서 HTTP 서버를 한 번만 시작하도록 보장한다. 여러 서버 블록에서 같은 prometheus 주소를 설정해도 하나의 HTTP 서버만 실행된다.

### parse 함수

```
func parse(c *caddy.Controller) (*Metrics, error) {
    met := New(defaultAddr)   // 기본: localhost:9153

    i := 0
    for c.Next() {
        if i > 0 { return nil, plugin.ErrOnce }  // 한 번만 설정 가능
        i++

        zones := plugin.OriginsFromArgsOrServerBlock(nil, c.ServerBlockKeys)
        for _, z := range zones {
            met.AddZone(z)
        }

        args := c.RemainingArgs()
        switch len(args) {
        case 0:    // 기본 주소 사용
        case 1:
            met.Addr = args[0]   // 사용자 지정 주소
        default:
            return met, c.ArgErr()
        }
    }
    return met, nil
}

const defaultAddr = "localhost:9153"
```

### Corefile 설정 예시

```
# 기본 설정 (localhost:9153)
. {
    prometheus
    forward . 8.8.8.8
}

# 사용자 지정 주소
. {
    prometheus 0.0.0.0:9253
    forward . 8.8.8.8
}
```

---

## Health 플러그인

### health 구조체

```
// plugin/health/health.go

type health struct {
    Addr      string              // 리스닝 주소 (기본: :8080)
    lameduck  time.Duration       // lameduck 기간
    healthURI *url.URL            // 헬스체크 URL

    ln      net.Listener
    srv     *http.Server
    nlSetup bool
    mux     *http.ServeMux

    stop context.CancelFunc       // overloaded 고루틴 취소 함수
}
```

### OnStartup - 서버 시작

```
func (h *health) OnStartup() error {
    if h.Addr == "" {
        h.Addr = ":8080"
    }

    // healthURI 파싱 (self-check용)
    h.healthURI, err = url.Parse("http://" + h.Addr)
    h.healthURI.Path = "/health"
    if h.healthURI.Host == "" {
        h.healthURI.Host = "localhost"
    }

    ln, err := reuseport.Listen("tcp", h.Addr)

    h.mux = http.NewServeMux()
    h.mux.HandleFunc(h.healthURI.Path, func(w http.ResponseWriter, r *http.Request) {
        // 항상 200 OK 반환
        w.WriteHeader(http.StatusOK)
        io.WriteString(w, http.StatusText(http.StatusOK))
    })

    ctx := context.Background()
    ctx, h.stop = context.WithCancel(ctx)

    h.srv = &http.Server{
        Handler:      h.mux,
        ReadTimeout:  5 * time.Second,
        WriteTimeout: 5 * time.Second,
        IdleTimeout:  5 * time.Second,
    }

    go func() { h.srv.Serve(h.ln) }()
    go func() { h.overloaded(ctx) }()    // 자기 자신을 모니터링

    return nil
}
```

**핵심 설계:**

1. **항상 200 OK**: health 엔드포인트는 CoreDNS 프로세스가 살아있는 한 항상 200을 반환한다. "건강" 여부를 판단하지 않고, 단순히 프로세스 생존 여부만 확인한다.

2. **Self-monitoring**: `overloaded()` 고루틴이 1초마다 자신의 `/health` 엔드포인트를 호출하여 응답 시간을 측정한다.

### Overloaded - 자가 모니터링

```
// plugin/health/overloaded.go

func (h *health) overloaded(ctx context.Context) {
    // 프록시를 우회하는 HTTP 클라이언트
    bypassProxy := &http.Transport{
        Proxy: nil,
        // ...
    }
    timeout := 3 * time.Second
    client := http.Client{Timeout: timeout, Transport: bypassProxy}

    req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.healthURI.String(), nil)
    tick := time.NewTicker(1 * time.Second)
    defer tick.Stop()

    for {
        select {
        case <-tick.C:
            start := time.Now()
            resp, err := client.Do(req)
            if err != nil && ctx.Err() == context.Canceled {
                return    // 정상 종료
            }
            if err != nil {
                HealthDuration.Observe(time.Since(start).Seconds())
                HealthFailures.Inc()
                log.Warningf("Local health request to %q failed: %s", req.URL.String(), err)
                continue
            }
            resp.Body.Close()
            elapsed := time.Since(start)
            HealthDuration.Observe(elapsed.Seconds())
            if elapsed > time.Second {
                log.Warningf("Local health request took more than 1s: %s", elapsed)
            }

        case <-ctx.Done():
            return
        }
    }
}
```

**왜 자신을 모니터링하는가?**

`/health` 엔드포인트 자체의 응답 시간이 느리다면, CoreDNS 프로세스가 과부하 상태임을 나타낸다. 이 정보는 두 가지 메트릭으로 노출된다:

```
// Health 전용 메트릭
var (
    HealthDuration = promauto.NewHistogram(prometheus.HistogramOpts{
        Namespace: plugin.Namespace,
        Subsystem: "health",
        Name:      "request_duration_seconds",
        Buckets:   plugin.SlimTimeBuckets,
        Help:      "Histogram of the time each request took.",
    })

    HealthFailures = promauto.NewCounter(prometheus.CounterOpts{
        Namespace: plugin.Namespace,
        Subsystem: "health",
        Name:      "request_failures_total",
        Help:      "The number of times the health check failed.",
    })
)
```

| 메트릭 | 의미 |
|--------|------|
| `coredns_health_request_duration_seconds` | 자가 헬스체크 응답 시간 |
| `coredns_health_request_failures_total` | 자가 헬스체크 실패 횟수 |

1초 이상 걸리면 경고를 로깅한다. 이는 DNS 응답도 느려지고 있음을 간접적으로 나타낸다.

### Lameduck 메커니즘

```
func (h *health) OnFinalShutdown() error {
    if !h.nlSetup { return nil }

    if h.lameduck > 0 {
        log.Infof("Going into lameduck mode for %s", h.lameduck)
        time.Sleep(h.lameduck)    // 설정된 시간만큼 대기
    }

    h.stop()   // overloaded 고루틴 종료

    ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
    defer cancel()
    if err := h.srv.Shutdown(ctx); err != nil {
        log.Infof("Failed to stop health http server: %s", err)
    }
    h.nlSetup = false
    return nil
}
```

**Lameduck의 동작:**

```
종료 시그널 수신
     |
     v
+-----------+
| lameduck  |  ← 이 기간 동안 /health는 여전히 200 반환
| 대기      |    → 로드밸런서가 이 서버를 제거할 시간
+-----------+
     |
     v (lameduck 종료)
+-----------+
| stop()    |  ← overloaded 고루틴 종료
+-----------+
     |
     v
+-----------+
| Shutdown  |  ← HTTP 서버 graceful 종료 (5초 타임아웃)
+-----------+
```

**왜 Lameduck이 필요한가?**

Kubernetes에서 Pod 종료 시:
1. Pod의 IP가 Service endpoints에서 제거됨
2. 하지만 kube-proxy의 iptables 업데이트에 시간이 걸림
3. 이 사이에 요청이 종료 중인 Pod로 라우팅될 수 있음

Lameduck 기간 동안 health 엔드포인트가 200을 반환하면서도 새 DNS 요청은 처리하지 않아, 안전한 전환이 가능하다.

### Setup 설정

```
// plugin/health/setup.go

func init() { plugin.Register("health", setup) }

func setup(c *caddy.Controller) error {
    addr, lame, err := parse(c)
    h := &health{Addr: addr, lameduck: lame}

    c.OnStartup(h.OnStartup)
    c.OnRestart(h.OnReload)
    c.OnFinalShutdown(h.OnFinalShutdown)
    c.OnRestartFailed(h.OnStartup)

    // 주의: AddPlugin을 호출하지 않음
    // health는 DNS 플러그인 체인에 참여하지 않고
    // 별도의 HTTP 서버만 운영함
    return nil
}

func parse(c *caddy.Controller) (string, time.Duration, error) {
    addr := ""
    dur := time.Duration(0)
    for c.Next() {
        args := c.RemainingArgs()
        switch len(args) {
        case 0:     // 기본 주소 :8080
        case 1:
            addr = args[0]
        }
        for c.NextBlock() {
            switch c.Val() {
            case "lameduck":
                l, err := time.ParseDuration(args[0])
                dur = l
            }
        }
    }
    return addr, dur, nil
}
```

**Corefile 예시:**

```
# 기본 설정
. {
    health
}

# 사용자 지정 주소 + lameduck
. {
    health :8081 {
        lameduck 5s
    }
}
```

---

## Ready 플러그인

### Readiness 인터페이스

```
// plugin/ready/readiness.go

type Readiness interface {
    Ready() bool
}
```

각 플러그인은 이 인터페이스를 구현하여 자신의 준비 상태를 보고한다. 예를 들어:
- forward 플러그인: upstream 서버와 연결이 확인되면 Ready
- kubernetes 플러그인: API 서버와 동기화가 완료되면 Ready
- file 플러그인: Zone 파일 로드가 완료되면 Ready

### ready 구조체

```
// plugin/ready/ready.go

type ready struct {
    Addr string           // 리스닝 주소 (기본: :8181)

    sync.RWMutex
    ln   net.Listener
    srv  *http.Server
    done bool             // 종료 상태
    mux  *http.ServeMux
}
```

### /ready 핸들러

```
func (rd *ready) onStartup() error {
    ln, err := reuseport.Listen("tcp", rd.Addr)

    rd.mux = http.NewServeMux()
    rd.mux.HandleFunc("/ready", func(w http.ResponseWriter, _ *http.Request) {
        rd.Lock()
        defer rd.Unlock()

        // 종료 중이면 503
        if !rd.done {
            w.WriteHeader(http.StatusServiceUnavailable)
            io.WriteString(w, "Shutting down")
            return
        }

        // 모든 플러그인이 Ready인지 확인
        ready, notReadyPlugins := plugins.Ready()
        if ready {
            w.WriteHeader(http.StatusOK)
            io.WriteString(w, http.StatusText(http.StatusOK))
            return
        }

        // 준비되지 않은 플러그인 로깅
        log.Infof("Plugins not ready: %q", notReadyPlugins)
        w.WriteHeader(http.StatusServiceUnavailable)
        io.WriteString(w, notReadyPlugins)
    })

    // ...
}
```

**응답 코드:**

| 상태 | HTTP 코드 | 의미 |
|------|----------|------|
| Ready | 200 OK | 모든 플러그인 준비 완료 |
| Not Ready | 503 Service Unavailable | 일부 플러그인 미준비 (본문: 플러그인 목록) |
| Shutting Down | 503 Service Unavailable | 서버 종료 중 |

### list - 플러그인 준비 상태 관리

```
// plugin/ready/list.go

type list struct {
    sync.RWMutex
    rs    []Readiness        // Readiness 인터페이스 목록
    names []string           // 플러그인 이름 목록

    keepReadiness bool       // 준비 후에도 모니터링 계속할지 여부
}

func (l *list) Ready() (bool, string) {
    l.RLock()
    defer l.RUnlock()
    ok := true
    s := []string{}
    for i, r := range l.rs {
        if r == nil { continue }       // 이미 Ready 확인 후 nil로 설정된 것
        if r.Ready() {
            if !l.keepReadiness {
                l.rs[i] = nil          // Ready 확인 후 더 이상 확인 안 함
            }
            continue
        }
        ok = false
        s = append(s, l.names[i])
    }
    if ok { return true, "" }
    sort.Strings(s)
    return false, strings.Join(s, ",")
}
```

**keepReadiness 옵션:**

```
func (l *list) Ready() (bool, string) {
    // ...
    if r.Ready() {
        if !l.keepReadiness {
            l.rs[i] = nil    // 한 번 Ready되면 nil로 설정 → 이후 체크 생략
        }
        continue
    }
}
```

- `keepReadiness = false` (기본, `monitor until-ready`): 플러그인이 한 번 Ready가 되면 `nil`로 설정하여 더 이상 검사하지 않는다. 리소스 절약.
- `keepReadiness = true` (`monitor continuously`): 플러그인의 Ready 상태를 지속적으로 모니터링한다. 플러그인이 Not Ready로 돌아갈 수 있는 경우에 사용.

### Setup 설정

```
// plugin/ready/setup.go

func init() { plugin.Register("ready", setup) }

func setup(c *caddy.Controller) error {
    addr, monType, err := parse(c)

    if monType == monitorTypeContinuously {
        plugins.keepReadiness = true
    } else {
        plugins.keepReadiness = false
    }

    rd := &ready{Addr: addr}

    // uniq 패턴: 동일 주소에서 한 번만 시작
    uniqAddr.Set(addr, rd.onStartup)
    c.OnStartup(func() error { uniqAddr.Set(addr, rd.onStartup); return nil })
    c.OnStartup(func() error { return uniqAddr.ForEach() })

    // Readiness 인터페이스를 구현하는 플러그인 수집
    c.OnStartup(func() error {
        plugins.Reset()
        for _, p := range dnsserver.GetConfig(c).Handlers() {
            if r, ok := p.(Readiness); ok {
                plugins.Append(r, p.Name())
            }
        }
        return nil
    })

    c.OnRestart(rd.onFinalShutdown)
    c.OnFinalShutdown(rd.onFinalShutdown)

    return nil
}
```

**Readiness 인터페이스 수집:**

서버 시작 시 플러그인 체인의 모든 핸들러를 순회하며, `Readiness` 인터페이스를 구현하는 플러그인만 수집한다. 이 플러그인들의 `Ready()` 메서드가 모두 `true`를 반환해야 `/ready`가 200을 반환한다.

### Monitor Type 설정

```
type monitorType string

const (
    monitorTypeUntilReady   monitorType = "until-ready"
    monitorTypeContinuously monitorType = "continuously"
)
```

**Corefile 예시:**

```
# 기본 설정 (until-ready 모드)
. {
    ready
}

# 사용자 지정 주소 + 지속 모니터링
. {
    ready :8282 {
        monitor continuously
    }
}
```

---

## 서버 레벨 메트릭 보고

### vars.Report in server.go

플러그인 체인에 도달하지 못한 요청도 메트릭으로 보고된다:

```
// core/dnsserver/server.go (line 426)

vars.Report(server, state, vars.Dropped, "",
            rcode.ToString(rc), "" /* plugin */,
            answer.Len(), time.Now())
```

이 호출은 다음 상황에서 발생한다:
- 잘못된 DNS 메시지
- 서버 블록에 매칭되지 않는 요청
- 내부 오류

`vars.Dropped` ("dropped")가 zone 레이블로 사용되어 정상 트래픽과 구분된다.

### PluginEnabled 메트릭 설정

```
// plugin/metrics/setup.go

c.OnStartup(func() error {
    conf := dnsserver.GetConfig(c)
    for _, h := range conf.ListenHosts {
        addrstr := conf.Transport + "://" + net.JoinHostPort(h, conf.Port)
        for _, p := range conf.Handlers() {
            vars.PluginEnabled.WithLabelValues(addrstr, conf.Zone, conf.ViewName, p.Name()).Set(1)
        }
    }
    return nil
})
```

서버 시작 시 모든 활성 플러그인에 대해 `coredns_plugin_enabled{server="dns://:53", zone=".", name="forward"} 1` 형태의 메트릭을 설정한다. 리스타트 시 `Reset()`으로 초기화한 후 다시 설정한다.

---

## Kubernetes 통합

### Liveness Probe

```yaml
livenessProbe:
  httpGet:
    path: /health
    port: 8080
  initialDelaySeconds: 60
  timeoutSeconds: 5
```

`/health`는 프로세스가 살아있으면 항상 200을 반환한다. 실패하면 kubelet이 Pod를 재시작한다.

### Readiness Probe

```yaml
readinessProbe:
  httpGet:
    path: /ready
    port: 8181
  initialDelaySeconds: 10
  periodSeconds: 5
```

`/ready`는 모든 플러그인이 준비될 때까지 503을 반환한다. Service endpoints에서 제외되어 트래픽을 받지 않는다.

### 일반적인 Corefile 설정

```
.:53 {
    errors
    health {
        lameduck 5s
    }
    ready
    kubernetes cluster.local in-addr.arpa ip6.arpa {
        pods insecure
        fallthrough in-addr.arpa ip6.arpa
    }
    prometheus :9153
    forward . /etc/resolv.conf
    cache 30
    loop
    reload
    loadbalance
}
```

---

## Grafana 대시보드 핵심 패널

### 1. 요청률 (QPS)

```
# PromQL
sum(rate(coredns_dns_requests_total[5m])) by (server)
```

서버별 초당 DNS 쿼리 수. 트래픽 패턴과 피크를 파악하는 기본 지표.

### 2. 응답 코드 분포

```
# PromQL
sum(rate(coredns_dns_responses_total[5m])) by (rcode)
```

| rcode | 정상 범위 | 이상 징후 |
|-------|----------|----------|
| NOERROR | 대부분 | - |
| NXDOMAIN | 5-15% | 급증 시 잘못된 쿼리 패턴 또는 도메인 탈취 |
| SERVFAIL | <1% | 급증 시 upstream 장애 |
| REFUSED | 매우 낮음 | 급증 시 설정 오류 |

### 3. 요청 지연 시간

```
# PromQL (P99)
histogram_quantile(0.99, sum(rate(coredns_dns_request_duration_seconds_bucket[5m])) by (le, server))
```

P99 지연 시간. 1ms 미만이 정상. 10ms 이상이면 upstream 지연이나 CPU 과부하 의심.

### 4. 캐시 적중률

```
# PromQL
sum(rate(coredns_dns_responses_total{plugin="cache"}[5m]))
/
sum(rate(coredns_dns_requests_total[5m]))
```

cache 플러그인이 응답한 비율. 높을수록 upstream 부하가 줄어든다.

### 5. 프로토콜별 트래픽

```
# PromQL
sum(rate(coredns_dns_requests_total[5m])) by (proto, family)
```

UDP vs TCP, IPv4 vs IPv6 비율. TCP 비율이 높으면 UDP 단편화 문제 의심.

### 6. DNSSEC 요청 비율

```
# PromQL
sum(rate(coredns_dns_do_requests_total[5m]))
/
sum(rate(coredns_dns_requests_total[5m]))
```

DNSSEC OK 비트가 설정된 요청의 비율. DNSSEC 배포 현황 파악에 유용.

### 7. 과부하 감지

```
# PromQL
histogram_quantile(0.99, rate(coredns_health_request_duration_seconds_bucket[5m]))
```

자가 헬스체크 응답 시간의 P99. 100ms 이상이면 과부하 상태.

### 8. 패닉 감지

```
# Alert Rule
coredns_panics_total > 0
```

패닉이 발생하면 즉시 알림. CoreDNS는 패닉을 복구(recover)하지만, 근본 원인 수정이 필요하다.

---

## 아키텍처 종합

```
                        CoreDNS 프로세스
+-------------------------------------------------------------+
|                                                             |
|  DNS 서버 (:53)          HTTP 서버들                        |
|  +----------------+     +---------+ +---------+ +--------+  |
|  | UDP/TCP        |     | :9153   | | :8080   | | :8181  |  |
|  | 리스너         |     | /metrics| | /health | | /ready |  |
|  +------+---------+     +----+----+ +----+----+ +---+----+  |
|         |                    |           |          |        |
|         v                    |           |          |        |
|  +------+---------+         |           |          |        |
|  | prometheus      |<--------+           |          |        |
|  | (metrics plugin)|                    |          |        |
|  | ServeDNS()      |                    |          |        |
|  | → Recorder      |                    |          |        |
|  | → Report()      |                    |          |        |
|  +------+---------+          +-----------+          |        |
|         |                    | overloaded()         |        |
|         v                    | (self-check)         |        |
|  +------+---------+         |                      |        |
|  | 다음 플러그인들 |         |           +-----------+        |
|  | (cache, forward,|        |           | plugins.Ready()   |
|  |  file, ...)     |        |           | (Readiness 체크)  |
|  +-----------------+        |           +-------------------+
|                             |                               |
+-------------------------------------------------------------+
                              |
                              v
                     Prometheus 서버
                     (스크래핑)
```

---

## 정리

| 항목 | 설명 |
|------|------|
| prometheus 플러그인 | DNS 플러그인 체인 참여, Recorder로 응답 추적, vars.Report()로 14개 메트릭 업데이트 |
| 핵심 메트릭 | requests_total, responses_total, request_duration_seconds, request_size_bytes, response_size_bytes |
| health 플러그인 | /health:8080, 항상 200 OK, self-check(overloaded), lameduck 지원 |
| ready 플러그인 | /ready:8181, Readiness 인터페이스 수집, 모든 플러그인 Ready 시 200 |
| lameduck | 종료 시 설정 시간만큼 대기하여 로드밸런서 전환 시간 확보 |
| monitor 모드 | until-ready(기본): 한 번 Ready → 체크 해제 / continuously: 지속 모니터링 |
| 서버 레벨 보고 | 플러그인 체인 도달 전 드롭된 요청도 vars.Dropped으로 보고 |
| Kubernetes 통합 | liveness → /health, readiness → /ready |
| Grafana 핵심 | QPS, 응답코드 분포, P99 지연, 캐시 적중률, 과부하 감지 |
