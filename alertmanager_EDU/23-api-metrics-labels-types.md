# 23. API Metrics, pkg/labels, types 패키지 Deep-Dive

> Alertmanager의 API 계층 메트릭 수집, 레이블 매칭 라이브러리, 핵심 타입 시스템을 분석한다.
> 소스 기준: `api/metrics/metrics.go`, `api/api.go`, `pkg/labels/matcher.go`,
> `pkg/labels/parse.go`, `types/types.go`, `alert/alert.go`

---

## 1. API Metrics 시스템

### 1.1 개요

Alertmanager의 API 계층은 Prometheus 메트릭을 통해 알림 수신 현황과 HTTP 요청 성능을 모니터링한다. 이 메트릭들은 Alertmanager 자체의 건강 상태를 판단하는 핵심 지표다.

### 1.2 Alerts 메트릭 구조

```go
// api/metrics/metrics.go:22
type Alerts struct {
    firing   prometheus.Counter
    resolved prometheus.Counter
    invalid  prometheus.Counter
}
```

세 가지 카운터로 API를 통해 수신된 알림을 분류한다:

| 메트릭 | 의미 | 레이블 |
|--------|------|--------|
| `alertmanager_alerts_received_total{status="firing"}` | 발생 중인 알림 수신 수 | version="v2" |
| `alertmanager_alerts_received_total{status="resolved"}` | 해결된 알림 수신 수 | version="v2" |
| `alertmanager_alerts_invalid_total` | 유효하지 않은 알림 수 | version="v2" |

### 1.3 NewAlerts 팩토리

```go
// api/metrics/metrics.go:30
func NewAlerts(r prometheus.Registerer) *Alerts {
    if r == nil {
        return nil  // Registry 없으면 메트릭 비활성화
    }
    numReceivedAlerts := promauto.With(r).NewCounterVec(prometheus.CounterOpts{
        Name:        "alertmanager_alerts_received_total",
        Help:        "The total number of received alerts.",
        ConstLabels: prometheus.Labels{"version": "v2"},
    }, []string{"status"})

    numInvalidAlerts := promauto.With(r).NewCounter(prometheus.CounterOpts{
        Name:        "alertmanager_alerts_invalid_total",
        Help:        "The total number of received alerts that were invalid.",
        ConstLabels: prometheus.Labels{"version": "v2"},
    })

    return &Alerts{
        firing:   numReceivedAlerts.WithLabelValues("firing"),
        resolved: numReceivedAlerts.WithLabelValues("resolved"),
        invalid:  numInvalidAlerts,
    }
}
```

**설계 결정:**

1. **ConstLabels `version: "v2"`**: API v1이 0.27에서 제거되었으므로 v2만 남았지만, 하위 호환성을 위해 레이블 유지
2. **promauto**: 자동 등록으로 registry 관리 단순화
3. **nil Registry 지원**: 테스트 환경에서 메트릭 비활성화 가능

### 1.4 접근자 메서드

```go
func (a *Alerts) Firing() prometheus.Counter { return a.firing }
func (a *Alerts) Resolved() prometheus.Counter { return a.resolved }
func (a *Alerts) Invalid() prometheus.Counter { return a.invalid }
```

캡슐화를 통해 외부에서 카운터를 직접 생성하지 못하게 한다.

### 1.5 API 계층 HTTP 메트릭

```go
// api/api.go:43
type API struct {
    v2                *apiv2.API
    deprecationRouter *V1DeprecationRouter

    requestDuration          *prometheus.HistogramVec   // 요청 지연 히스토그램
    requestsInFlight         prometheus.Gauge           // 현재 처리 중인 요청 수
    concurrencyLimitExceeded prometheus.Counter          // 동시성 한도 초과 횟수
    timeout                  time.Duration
    inFlightSem              chan struct{}               // 세마포어
}
```

### 1.6 HTTP 메트릭 등록

```go
// api/api.go:139
requestsInFlight := prometheus.NewGauge(prometheus.GaugeOpts{
    Name:        "alertmanager_http_requests_in_flight",
    Help:        "Current number of HTTP requests being processed.",
    ConstLabels: prometheus.Labels{"method": "get"},
})
concurrencyLimitExceeded := prometheus.NewCounter(prometheus.CounterOpts{
    Name:        "alertmanager_http_concurrency_limit_exceeded_total",
    Help:        "Total number of times an HTTP request failed because the concurrency limit was reached.",
    ConstLabels: prometheus.Labels{"method": "get"},
})
```

| 메트릭 | 타입 | 설명 |
|--------|------|------|
| `alertmanager_http_requests_in_flight` | Gauge | 현재 처리 중인 GET 요청 수 |
| `alertmanager_http_concurrency_limit_exceeded_total` | Counter | 동시성 한도 초과 횟수 |
| (외부 주입) `requestDuration` | HistogramVec | 핸들러별 요청 처리 시간 |

### 1.7 동시성 제한 (Concurrency Limiter)

```go
// api/api.go:209
func (api *API) limitHandler(h http.Handler) http.Handler {
    concLimiter := http.HandlerFunc(func(rsp http.ResponseWriter, req *http.Request) {
        if req.Method == http.MethodGet {  // GET만 제한
            select {
            case api.inFlightSem <- struct{}{}:  // 세마포어 획득
                api.requestsInFlight.Inc()
                defer func() {
                    <-api.inFlightSem
                    api.requestsInFlight.Dec()
                }()
            default:  // 세마포어 가득 참
                api.concurrencyLimitExceeded.Inc()
                http.Error(rsp, fmt.Sprintf(
                    "Limit of concurrent GET requests reached (%d)",
                    cap(api.inFlightSem),
                ), http.StatusServiceUnavailable)
                return
            }
        }
        h.ServeHTTP(rsp, req)
    })
    // 타임아웃 핸들러 래핑
    if api.timeout <= 0 {
        return concLimiter
    }
    return http.TimeoutHandler(concLimiter, api.timeout, "timeout")
}
```

**동작 흐름:**

```
GET 요청 → inFlightSem 획득 시도
├─ 성공 → requestsInFlight++ → 처리 → requestsInFlight--
└─ 실패 → concurrencyLimitExceeded++ → 503 반환
```

**기본 동시성 한도:**

```go
concurrency := opts.Concurrency
if concurrency < 1 {
    concurrency = max(runtime.GOMAXPROCS(0), 8)  // GOMAXPROCS 또는 8 중 큰 값
}
```

### 1.8 계측 핸들러 (Instrumentation)

```go
// api/api.go:237
func (api *API) instrumentHandler(prefix string, h http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        path, _ := strings.CutPrefix(r.URL.Path, prefix)
        // 카디널리티 제어: silence ID를 플레이스홀더로 치환
        if strings.HasPrefix(path, "/api/v2/silence/") {
            path = "/api/v2/silence/{silenceID}"
        }
        promhttp.InstrumentHandlerDuration(
            api.requestDuration.MustCurryWith(prometheus.Labels{"handler": path}),
            otelhttp.NewHandler(h, path),  // OTel 트레이싱도 함께 적용
        ).ServeHTTP(w, r)
    })
}
```

**왜 silence ID를 치환하는가?**

Silence ID는 UUID이므로 수만 개의 고유 값이 생길 수 있다. 이를 그대로 메트릭 레이블에 사용하면 카디널리티가 폭발하여 Prometheus 메모리를 과도하게 사용한다. `/api/v2/silence/{silenceID}`로 정규화하여 하나의 시계열로 집약한다.

### 1.9 API 등록 구조

```go
// api/api.go:176
func (api *API) Register(r *route.Router, routePrefix string) *http.ServeMux {
    // v1 API 사용 중단 라우터 등록
    api.deprecationRouter.Register(r.WithPrefix("/api/v1"))

    mux := http.NewServeMux()
    mux.Handle("/", api.limitHandler(r))  // 기본 라우터

    // v2 API: 계측 + 제한 + 스트립 접두사
    mux.Handle(apiPrefix+"/api/v2/",
        api.instrumentHandler(apiPrefix,
            api.limitHandler(
                http.StripPrefix(apiPrefix, api.v2.Handler))))

    return mux
}
```

**미들웨어 체인:**

```
요청 → instrumentHandler (메트릭+OTel)
    → limitHandler (동시성 제한+타임아웃)
        → StripPrefix
            → v2.Handler (실제 API 핸들러)
```

---

## 2. pkg/labels 패키지

### 2.1 개요

`pkg/labels`는 Alertmanager의 레이블 매칭 시스템의 핵심 라이브러리다. Prometheus의 PromQL 매처 구문과 호환되는 레이블 필터를 파싱하고 매칭한다.

### 2.2 MatchType 열거형

```go
// pkg/labels/matcher.go:29
type MatchType int

const (
    MatchEqual     MatchType = iota  // =
    MatchNotEqual                     // !=
    MatchRegexp                       // =~
    MatchNotRegexp                    // !~
)
```

네 가지 매칭 유형:

| 타입 | 연산자 | 의미 | 예시 |
|------|--------|------|------|
| MatchEqual | `=` | 값이 정확히 일치 | `severity="critical"` |
| MatchNotEqual | `!=` | 값이 불일치 | `env!="test"` |
| MatchRegexp | `=~` | 정규식 일치 | `instance=~"web-.*"` |
| MatchNotRegexp | `!~` | 정규식 불일치 | `job!~"test-.*"` |

### 2.3 Matcher 구조체

```go
// pkg/labels/matcher.go:53
type Matcher struct {
    Type  MatchType
    Name  string
    Value string
    re    *regexp.Regexp  // 정규식 매칭 시 캐시된 정규식
}
```

**NewMatcher:**

```go
func NewMatcher(t MatchType, n, v string) (*Matcher, error) {
    m := &Matcher{Type: t, Name: n, Value: v}
    if t == MatchRegexp || t == MatchNotRegexp {
        re, err := regexp.Compile("^(?:" + v + ")$")
        if err != nil {
            return nil, err
        }
        m.re = re
    }
    return m, nil
}
```

**왜 `^(?:` + v + `)$` 패턴을 사용하는가?**

- `^...$`: 전체 문자열 매칭 보장 (부분 매칭 방지)
- `(?:...)`: 비캡처 그룹으로 사용자 정규식을 래핑 (그룹 번호 충돌 방지)
- 예: `web-.*` → `^(?:web-.*)$` — 전체 값이 `web-`으로 시작해야 매칭

### 2.4 Matches 메서드

```go
// pkg/labels/matcher.go:86
func (m *Matcher) Matches(s string) bool {
    switch m.Type {
    case MatchEqual:
        return s == m.Value
    case MatchNotEqual:
        return s != m.Value
    case MatchRegexp:
        return m.re.MatchString(s)
    case MatchNotRegexp:
        return !m.re.MatchString(s)
    }
    panic("labels.Matcher.Matches: invalid match type")
}
```

단순하지만 효율적인 구현:
- Equal/NotEqual은 문자열 비교 (O(n))
- Regexp/NotRegexp는 컴파일된 정규식 사용 (O(n) ~ O(nm))

### 2.5 Matcher 직렬화

#### String() 출력

```go
func (m *Matcher) String() string {
    if strings.ContainsFunc(m.Name, isReserved) {
        return fmt.Sprintf(`%s%s%s`, strconv.Quote(m.Name), m.Type, strconv.Quote(m.Value))
    }
    return fmt.Sprintf(`%s%s"%s"`, m.Name, m.Type, openMetricsEscape(m.Value))
}
```

레이블 이름에 예약 문자가 포함되면 따옴표로 감싸고, 값은 OpenMetrics 이스케이프를 적용한다.

#### JSON 직렬화 (API v1 호환)

```go
// pkg/labels/matcher.go:100
type apiV1Matcher struct {
    Name    string `json:"name"`
    Value   string `json:"value"`
    IsRegex bool   `json:"isRegex"`
    IsEqual bool   `json:"isEqual"`
}
```

API v1의 `IsRegex`/`IsEqual` 형식과 v2의 `Type` 형식 사이를 변환한다.

```
MatchEqual     → IsRegex=false, IsEqual=true
MatchNotEqual  → IsRegex=false, IsEqual=false
MatchRegexp    → IsRegex=true,  IsEqual=true
MatchNotRegexp → IsRegex=true,  IsEqual=false
```

### 2.6 openMetricsEscape

```go
func openMetricsEscape(s string) string {
    r := strings.NewReplacer(
        `\`, `\\`,     // 백슬래시 이스케이프
        "\n", `\n`,    // 줄바꿈 이스케이프
        `"`, `\"`,     // 따옴표 이스케이프
    )
    return r.Replace(s)
}
```

OpenMetrics 표준의 최소 이스케이프 규칙. 세 가지 문자만 이스케이프한다.

### 2.7 Matchers 슬라이스 (AND 로직)

```go
// pkg/labels/matcher.go:162
type Matchers []*Matcher

// sort.Interface 구현 (Name → Value → Type 순)
func (ms Matchers) Less(i, j int) bool { ... }

// AND 매칭: 모든 매처가 일치해야 true
func (ms Matchers) Matches(lset model.LabelSet) bool {
    for _, m := range ms {
        if !m.Matches(string(lset[model.LabelName(m.Name)])) {
            return false
        }
    }
    return true
}

// {name="val",name2!="val2"} 형식 출력
func (ms Matchers) String() string {
    var buf bytes.Buffer
    buf.WriteByte('{')
    for i, m := range ms {
        if i > 0 { buf.WriteByte(',') }
        buf.WriteString(m.String())
    }
    buf.WriteByte('}')
    return buf.String()
}
```

`Matchers`는 **AND 로직**을 구현한다: 모든 매처가 일치해야 전체 Matchers가 일치한다.

### 2.8 MatcherSet (OR 로직)

```go
// pkg/labels/matcher.go:211
type MatcherSet []*Matchers

func (ms MatcherSet) Matches(lset model.LabelSet) bool {
    for _, matchers := range ms {
        if (*matchers).Matches(lset) {
            return true  // 하나라도 일치하면 true
        }
    }
    return false
}
```

`MatcherSet`은 **OR 로직**을 구현한다: 하나의 `Matchers`라도 일치하면 전체 MatcherSet이 일치한다.

**논리 구조:**

```
MatcherSet = Matchers₁ OR Matchers₂ OR ... OR Matchersₙ

Matchersᵢ = Matcher₁ AND Matcher₂ AND ... AND Matcherₘ
```

이 조합으로 복잡한 레이블 필터를 표현할 수 있다:

```
(severity="critical" AND team="oncall") OR (service=~"web-.*")
```

### 2.9 isReserved 함수

```go
func isReserved(r rune) bool {
    return unicode.IsSpace(r) || strings.ContainsRune("{}!=~,\\\"'`", r)
}
```

레이블 이름에 이 문자들이 포함되면 따옴표가 필요하다. 이 함수는 UTF-8 매처 전환기에 클래식 파서와 UTF-8 파서를 구분하는 휴리스틱으로 사용된다.

---

## 3. pkg/labels/parse.go — 매처 파싱

### 3.1 ParseMatchers (쉼표 구분 목록)

```go
// pkg/labels/parse.go:55
func ParseMatchers(s string) ([]*Matcher, error) {
    matchers := []*Matcher{}
    s = strings.TrimPrefix(s, "{")
    s = strings.TrimSuffix(s, "}")

    var (
        insideQuotes bool
        escaped      bool
        token        strings.Builder
        tokens       []string
    )
    for _, r := range s {
        switch r {
        case ',':
            if !insideQuotes {  // 따옴표 밖의 쉼표만 구분자
                tokens = append(tokens, token.String())
                token.Reset()
                continue
            }
        case '"':
            if !escaped { insideQuotes = !insideQuotes }
            else { escaped = false }
        case '\\':
            escaped = !escaped
        default:
            escaped = false
        }
        token.WriteRune(r)
    }
    // 각 토큰을 ParseMatcher로 파싱
    for _, token := range tokens {
        m, err := ParseMatcher(token)
        matchers = append(matchers, m)
    }
    return matchers, nil
}
```

**핵심 알고리즘:**

1. 선택적으로 `{}`를 제거
2. 따옴표 상태를 추적하며 쉼표 기준으로 토큰 분리
3. 이스케이프된 따옴표(`\"`)는 무시
4. 각 토큰을 `ParseMatcher()`로 개별 파싱

**지원 형식:**

```
{foo="bar", dings!="bums"}           # 중괄호 포함
foo=bar,dings!=bums                  # 중괄호 없음
{quote="She said: \"Hi\""}          # 이스케이프된 따옴표
statuscode=~"5.."                    # 정규식
```

### 3.2 ParseMatcher (개별 매처)

```go
// pkg/labels/parse.go:117
var re = regexp.MustCompile(
    `^\s*([a-zA-Z_:][a-zA-Z0-9_:]*)\s*(=~|=|!=|!~)\s*((?s).*?)\s*$`)

func ParseMatcher(s string) (_ *Matcher, err error) {
    ms := re.FindStringSubmatch(s)
    if len(ms) == 0 {
        return nil, fmt.Errorf("bad matcher format: %s", s)
    }
    // ms[1] = 이름, ms[2] = 연산자, ms[3] = 값
    // 값의 따옴표 제거 및 이스케이프 해제
    // ...
    return NewMatcher(typeMap[ms[2]], ms[1], value.String())
}
```

**정규식 분석:**

```
^\s*                                    # 앞 공백 허용
([a-zA-Z_:][a-zA-Z0-9_:]*)            # 그룹1: 레이블 이름 (Prometheus 규칙)
\s*                                     # 공백 허용
(=~|=|!=|!~)                           # 그룹2: 연산자 (=~ 먼저!)
\s*                                     # 공백 허용
((?s).*?)                              # 그룹3: 값 (줄바꿈 포함, 최소 매칭)
\s*$                                    # 뒤 공백 허용
```

**왜 `=~`가 `=` 앞에 있는가?** `=`가 먼저 있으면 `=~`에서 `=`만 소비하고 `~`가 값의 일부가 되어 파싱이 실패한다.

### 3.3 값 이스케이프 해제

```go
// OpenMetrics 이스케이프 해제
for i, r := range rawValue {
    if escaped {
        switch r {
        case 'n':  value.WriteByte('\n')     // \n → 줄바꿈
        case '"':  value.WriteRune(r)        // \" → "
        case '\\': value.WriteRune(r)        // \\ → \
        default:                              // 미지 이스케이프
            value.WriteByte('\\')
            value.WriteRune(r)               // \x → \x (관대한 처리)
        }
        continue
    }
    // ...
}
```

**관대한 파싱**: 미지 이스케이프 시퀀스(`\x`)를 리터럴로 처리한다. 엄격한 파서는 에러를 반환하겠지만, 대화형 사용(CLI, UI)에서의 편의를 위해 관대하게 처리한다.

---

## 4. types 패키지

### 4.1 개요

`types` 패키지는 Alertmanager의 핵심 도메인 타입을 정의한다. `alert` 패키지로의 마이그레이션이 진행 중이다.

### 4.2 타입 별칭 (Deprecated)

```go
// types/types.go:25
// Deprecated: Use alert.Alert directly.
type Alert = alert.Alert

// Deprecated: Use alert.AlertSlice directly.
type AlertSlice = alert.AlertSlice

// Deprecated: Use alert.Alerts directly.
var Alerts = alert.Alerts

// Deprecated: Use alert.AlertState constants directly.
type AlertState = alert.AlertState
const AlertStateActive AlertState = alert.AlertStateActive
const AlertStateSuppressed AlertState = alert.AlertStateSuppressed
const AlertStateUnprocessed AlertState = alert.AlertStateUnprocessed

// Deprecated: Use alert.AlertStatus directly.
type AlertStatus = alert.AlertStatus
```

**왜 별칭을 유지하는가?** `types` 패키지는 원래 모든 핵심 타입이 있었으나, 리팩토링으로 `alert` 패키지로 이동했다. 기존 코드와의 호환성을 위해 타입 별칭을 유지한다.

### 4.3 AlertMarker 인터페이스

```go
// types/types.go:58
type AlertMarker interface {
    SetActiveOrSilenced(alert model.Fingerprint, activeSilenceIDs []string)
    SetInhibited(alert model.Fingerprint, alertIDs ...string)
    Count(...AlertState) int
    Status(model.Fingerprint) AlertStatus
    Delete(...model.Fingerprint)
    Unprocessed(model.Fingerprint) bool
    Active(model.Fingerprint) bool
    Silenced(model.Fingerprint) (activeIDs []string, silenced bool)
    Inhibited(model.Fingerprint) ([]string, bool)
}
```

`AlertMarker`는 알림의 상태(Active, Silenced, Inhibited, Unprocessed)를 추적하는 인터페이스다.

**상태 전이 규칙:**

```
┌──────────────┐
│ Unprocessed  │  ← 초기 상태
└──────┬───────┘
       │
       ▼
┌──────────────┐                  ┌──────────────┐
│    Active    │ ←── 침묵/억제 │   Suppressed │
│              │     해제      │              │
│              │ ──────────→  │ (Silenced    │
│              │  침묵/억제    │  + Inhibited) │
└──────────────┘   발동        └──────────────┘
```

### 4.4 GroupMarker 인터페이스

```go
// types/types.go:100
type GroupMarker interface {
    Muted(routeID, groupKey string) ([]string, bool)
    SetMuted(routeID, groupKey string, timeIntervalNames []string)
    DeleteByGroupKey(routeID, groupKey string)
}
```

`GroupMarker`는 알림 그룹의 뮤팅 상태를 추적한다. `routeID`가 필요한 이유는 `groupKey`만으로는 고유 식별이 안 되기 때문이다 (#3817).

### 4.5 MemMarker 구현

```go
// types/types.go:125
type MemMarker struct {
    alerts map[model.Fingerprint]*AlertStatus
    groups map[string]*groupStatus
    mtx    sync.RWMutex
}
```

메모리 기반 마커 구현. `sync.RWMutex`로 동시 접근을 보호한다.

#### SetActiveOrSilenced

```go
func (m *MemMarker) SetActiveOrSilenced(alert model.Fingerprint, activeIDs []string) {
    m.mtx.Lock()
    defer m.mtx.Unlock()

    s, found := m.alerts[alert]
    if !found {
        s = &AlertStatus{}
        m.alerts[alert] = s
    }
    s.SilencedBy = activeIDs

    // 침묵 ID와 억제 ID가 모두 없으면 Active
    if len(activeIDs) == 0 && len(s.InhibitedBy) == 0 {
        s.State = AlertStateActive
        return
    }
    s.State = AlertStateSuppressed
}
```

**핵심 로직**: 침묵과 억제는 독립적으로 작동한다. 둘 중 하나라도 활성 ID가 있으면 Suppressed 상태가 된다.

#### SetInhibited

```go
func (m *MemMarker) SetInhibited(alert model.Fingerprint, ids ...string) {
    m.mtx.Lock()
    defer m.mtx.Unlock()

    s, found := m.alerts[alert]
    if !found {
        s = &AlertStatus{}
        m.alerts[alert] = s
    }
    s.InhibitedBy = ids

    if len(ids) == 0 && len(s.SilencedBy) == 0 {
        s.State = AlertStateActive
        return
    }
    s.State = AlertStateSuppressed
}
```

동일한 "침묵+억제 합산" 로직. 억제 ID 목록을 교체하고 상태를 결정한다.

#### Muted (GroupMarker)

```go
func (m *MemMarker) Muted(routeID, groupKey string) ([]string, bool) {
    m.mtx.Lock()
    defer m.mtx.Unlock()
    status, ok := m.groups[routeID+groupKey]
    if !ok {
        return nil, false
    }
    return status.mutedBy, len(status.mutedBy) > 0
}
```

`routeID + groupKey`를 맵 키로 사용하여 그룹의 뮤팅 상태를 조회한다.

### 4.6 메트릭 등록

```go
// types/types.go:161
func (m *MemMarker) registerMetrics(r prometheus.Registerer) {
    newMarkedAlertMetricByState := func(st AlertState) prometheus.GaugeFunc {
        return prometheus.NewGaugeFunc(
            prometheus.GaugeOpts{
                Name:        "alertmanager_marked_alerts",
                Help:        "How many alerts by state are currently marked.",
                ConstLabels: prometheus.Labels{"state": string(st)},
            },
            func() float64 {
                return float64(m.Count(st))
            },
        )
    }
    r.MustRegister(
        newMarkedAlertMetricByState(AlertStateActive),
        newMarkedAlertMetricByState(AlertStateSuppressed),
        newMarkedAlertMetricByState(AlertStateUnprocessed),
    )
}
```

`GaugeFunc`를 사용하여 메트릭 조회 시점에 실시간으로 Count()를 호출한다.

| 메트릭 | 설명 |
|--------|------|
| `alertmanager_marked_alerts{state="active"}` | 활성 알림 수 |
| `alertmanager_marked_alerts{state="suppressed"}` | 억제된 알림 수 |
| `alertmanager_marked_alerts{state="unprocessed"}` | 미처리 알림 수 |

### 4.7 groupStatus

```go
type groupStatus struct {
    mutedBy []string  // 뮤팅 중인 시간 간격 이름들
}
```

그룹의 뮤팅 상태를 저장한다. `SetMuted()`로 시간 간격 이름 목록을 설정하면 해당 그룹은 뮤팅된다.

---

## 5. 세 패키지의 상호작용

```
┌─────────────────────────────────────────────────────────┐
│                    데이터 흐름                            │
│                                                          │
│  POST /api/v2/alerts                                     │
│       │                                                  │
│       ├─ api/metrics → alerts_received_total 증가         │
│       ├─ api/api.go → requestDuration 기록               │
│       │                                                  │
│       ▼                                                  │
│  Alert 저장 (types.Alert = alert.Alert)                  │
│       │                                                  │
│       ├─ AlertMarker.SetActiveOrSilenced()               │
│       ├─ AlertMarker.SetInhibited()                      │
│       │  → alertmanager_marked_alerts 갱신               │
│       │                                                  │
│       ▼                                                  │
│  GET /api/v2/alerts?filter=...                           │
│       │                                                  │
│       ├─ pkg/labels.ParseMatchers(filter)                │
│       ├─ Matchers.Matches(alert.Labels)                  │
│       │                                                  │
│       ├─ api/api.go → limitHandler                       │
│       │  → http_requests_in_flight 갱신                  │
│       │  → concurrency_limit_exceeded 증가 (초과 시)     │
│       │                                                  │
│       └─ AlertMarker.Status(fingerprint)                 │
│          → AlertStatus{State, SilencedBy, InhibitedBy}   │
└─────────────────────────────────────────────────────────┘
```

---

## 6. 설계 패턴 분석

### 6.1 API Metrics: Singleton 패턴

`Alerts` 메트릭은 API 계층에서 한 번 생성되어 전체 생명 주기 동안 공유된다.

### 6.2 pkg/labels: Composite 패턴

`Matcher` → `Matchers` (AND) → `MatcherSet` (OR) 계층 구조로 복합 필터를 표현한다.

### 6.3 types: Observer 패턴

`GaugeFunc`를 통해 메트릭 수집기가 `MemMarker.Count()`를 콜백으로 호출한다.

### 6.4 types: State 패턴

`AlertState`(Unprocessed, Active, Suppressed)에 따라 알림의 행동이 달라진다.

---

## 7. 운영 시 주요 메트릭 대시보드 구성

```
# Alertmanager 건강 상태 대시보드

# 1. 알림 수신률
rate(alertmanager_alerts_received_total[5m])

# 2. 유효하지 않은 알림 비율
rate(alertmanager_alerts_invalid_total[5m])
  / rate(alertmanager_alerts_received_total[5m])

# 3. 현재 처리 중인 요청
alertmanager_http_requests_in_flight

# 4. 동시성 한도 초과율
rate(alertmanager_http_concurrency_limit_exceeded_total[5m])

# 5. 알림 상태 분포
alertmanager_marked_alerts

# 6. API 응답 시간 (99th percentile)
histogram_quantile(0.99,
  rate(alertmanager_http_request_duration_seconds_bucket[5m]))
```

---

## 8. 정리

| 패키지 | 핵심 타입 | 역할 |
|--------|----------|------|
| `api/metrics/` | `Alerts` | API 수준 알림 수신 카운터 |
| `api/api.go` | `API` | HTTP 미들웨어 (메트릭, 동시성, 타임아웃) |
| `pkg/labels/` | `Matcher`, `Matchers`, `MatcherSet` | 레이블 매칭 AND/OR 로직 |
| `types/` | `AlertMarker`, `GroupMarker`, `MemMarker` | 알림/그룹 상태 추적 |

| 메트릭 | 소스 | 용도 |
|--------|------|------|
| `alertmanager_alerts_received_total` | `api/metrics/` | 수신 알림 모니터링 |
| `alertmanager_alerts_invalid_total` | `api/metrics/` | 유효성 검사 실패 감지 |
| `alertmanager_http_requests_in_flight` | `api/api.go` | 부하 모니터링 |
| `alertmanager_http_concurrency_limit_exceeded_total` | `api/api.go` | 용량 부족 감지 |
| `alertmanager_marked_alerts` | `types/` | 알림 상태 분포 |

---

*소스 참조: `api/metrics/metrics.go`, `api/api.go`, `pkg/labels/matcher.go`, `pkg/labels/parse.go`, `types/types.go`*
