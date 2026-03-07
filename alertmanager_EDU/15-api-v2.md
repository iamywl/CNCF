# Alertmanager API v2 Deep Dive

## 1. 개요

Alertmanager API v2는 OpenAPI 2.0(Swagger) 명세 기반으로 go-swagger를 통해 코드가 생성된다. `api/api.go`가 HTTP 통합을 담당하고, `api/v2/api.go`가 핵심 핸들러를 구현한다.

## 2. API 통합 구조체 (api/api.go)

```go
// api/api.go
type API struct {
    v2                    *apiv2.API
    deprecationRouter     *V1DeprecationRouter

    requestDuration          *prometheus.HistogramVec
    requestsInFlight         prometheus.Gauge
    concurrencyLimitExceeded prometheus.Counter
    timeout                  time.Duration
    inFlightSem              chan struct{}   // GET 동시성 제한 세마포어
}
```

### 2.1 Options

```go
// api/api.go
type Options struct {
    Alerts          provider.Alerts                               // 필수
    Silences        *silence.Silences                             // 필수
    AlertStatusFunc func(model.Fingerprint) types.AlertStatus     // 필수
    GroupMutedFunc   func(routeID, groupKey string) ([]string, bool) // 필수
    Peer            cluster.ClusterPeer                           // 선택
    Timeout         time.Duration                                 // HTTP 타임아웃
    Concurrency     int                                           // GET 동시성
    Logger          *slog.Logger
    Registry        prometheus.Registerer
    GroupFunc       func(context.Context, func(*dispatch.Route) bool,
                        func(*types.Alert, time.Time) bool) (dispatch.AlertGroups, map[model.Fingerprint][]string, error) // 필수
}
```

### 2.2 limitHandler — 동시성 제한

```go
// api/api.go
func (api *API) limitHandler(h http.Handler) http.Handler {
    // GET 요청에 대해:
    // 1. inFlightSem 채널에 토큰 전송 시도
    // 2. timeout 내에 토큰 확보 못하면 → 503 Service Unavailable
    // 3. 토큰 확보 → 핸들러 실행 후 토큰 반환
}
```

이 메커니즘은 대량 GET 요청이 Alertmanager를 과부하시키는 것을 방지한다.

## 3. API v2 구조체 (api/v2/api.go)

```go
// api/v2/api.go
type API struct {
    peer            cluster.ClusterPeer
    silences        *silence.Silences
    alerts          provider.Alerts
    alertGroups     groupsFn
    getAlertStatus  getAlertStatusFn
    groupMutedFunc  groupMutedFunc
    uptime          time.Time

    mtx                sync.RWMutex
    alertmanagerConfig *config.Config
    route              *dispatch.Route
    setAlertStatus     setAlertStatusFn

    logger  *slog.Logger
    m       *metrics.Alerts
    Handler http.Handler   // go-swagger 서버 핸들러
}
```

## 4. 엔드포인트 목록

| 메서드 | 경로 | 핸들러 | 설명 |
|--------|------|--------|------|
| POST | `/api/v2/alerts` | `postAlertsHandler` | Alert 수신 |
| GET | `/api/v2/alerts` | `getAlertsHandler` | Alert 조회 |
| GET | `/api/v2/alerts/groups` | `getAlertGroupsHandler` | Alert 그룹 조회 |
| POST | `/api/v2/silences` | `postSilencesHandler` | Silence 생성 |
| GET | `/api/v2/silences` | `getSilencesHandler` | Silence 조회 |
| GET | `/api/v2/silence/{silenceID}` | `getSilenceHandler` | Silence 단건 조회 |
| DELETE | `/api/v2/silence/{silenceID}` | `deleteSilenceHandler` | Silence 만료 |
| GET | `/api/v2/receivers` | `getReceiversHandler` | Receiver 목록 |
| GET | `/api/v2/status` | `getStatusHandler` | 상태 조회 |

## 5. 핵심 핸들러 분석

### 5.1 postAlertsHandler — Alert 수신

```
postAlertsHandler(params):
    1. 요청 바디에서 Alert 배열 추출
    2. 각 Alert 유효성 검증:
       - Labels 필수
       - StartsAt 설정 (없으면 now)
       - EndsAt 설정 (없으면 StartsAt + ResolveTimeout)
    3. 유효한 Alert만 Provider.Put()
    4. setAlertStatus() 호출 (Silencer 매칭)
    5. 반환: 200 OK
```

### 5.2 getAlertsHandler — Alert 조회

```
getAlertsHandler(params):
    1. 필터 파라미터 파싱:
       - filter[]: Matcher 문자열 배열
       - silenced: Silence된 Alert 포함 여부
       - inhibited: Inhibition된 Alert 포함 여부
       - active: 활성 Alert 포함 여부
       - unprocessed: 미처리 Alert 포함 여부
       - receiver: Receiver 이름으로 필터
    2. alertFilter() 생성
    3. Provider.GetPending() → 모든 Alert 조회
    4. alertFilter 적용
    5. Marker.Status() → 각 Alert 상태 첨부
    6. 정렬 후 반환
```

### 5.3 alertFilter() — 필터링 로직

```go
// api/v2/api.go
func (api *API) alertFilter(
    matchers []*labels.Matcher,
    silenced, inhibited, active bool,
) func(a *types.Alert, now time.Time) bool
```

```
alertFilter(alert, now):
    1. Matcher 매칭: 모든 Matcher가 Alert Labels와 일치해야 함
    2. 상태 필터:
       - Alert가 resolved이면 → false (pending만)
       - Alert가 silenced이고 silenced=false → false
       - Alert가 inhibited이고 inhibited=false → false
       - Alert가 active이고 active=false → false
       - Alert가 unprocessed이고 unprocessed=false → false
    3. 모든 조건 통과 → true
```

### 5.4 getAlertGroupsHandler — Alert 그룹 조회

```
getAlertGroupsHandler(params):
    1. 필터 파라미터 파싱
    2. Dispatcher.Groups(ctx, routeFilter, alertFilter)
    3. 각 AlertGroup에 대해:
       - Labels, Receiver, GroupKey 포함
       - Alert 목록에 상태 정보 첨부
       - GroupMutedFunc으로 뮤트 상태 확인
    4. 반환: AlertGroup 배열
```

### 5.5 postSilencesHandler — Silence 생성

```
postSilencesHandler(params):
    1. 요청 바디에서 Silence 모델 추출
    2. Matchers 파싱:
       - API 모델 → labels.Matcher 변환
       - 유효성 검증
    3. Silences.Set(silence)
    4. 반환: {silenceID: "..."}
```

### 5.6 deleteSilenceHandler — Silence 만료

```
deleteSilenceHandler(params):
    1. URL에서 silenceID 추출
    2. Silences.Expire(silenceID)
    3. 반환: 200 OK
```

### 5.7 getStatusHandler — 상태 조회

```
getStatusHandler(params):
    반환:
    {
        cluster: {
            name: "...",
            status: "ready|settling|disabled",
            peers: [{name, address}]
        },
        versionInfo: {version, revision, ...},
        config: {original: "yaml 원본"},
        uptime: "2024-01-01T00:00:00Z"
    }
```

## 6. OpenAPI 명세

`api/v2/openapi.yaml`에 전체 API 명세가 정의되어 있다:

```yaml
# api/v2/openapi.yaml (요약)
swagger: "2.0"
info:
  title: "Alertmanager API"
  version: "0.0.1"
basePath: "/api/v2/"

paths:
  /alerts:
    get:
      operationId: getAlerts
      parameters:
        - name: filter
          in: query
          type: array
        - name: silenced
          in: query
          type: boolean
        - name: inhibited
          in: query
          type: boolean
        - name: active
          in: query
          type: boolean
    post:
      operationId: postAlerts

  /alerts/groups:
    get:
      operationId: getAlertGroups

  /silences:
    get:
      operationId: getSilences
    post:
      operationId: postSilences

  /silence/{silenceID}:
    get:
      operationId: getSilence
    delete:
      operationId: deleteSilence

  /status:
    get:
      operationId: getStatus

  /receivers:
    get:
      operationId: getReceivers
```

## 7. 코드 생성 구조

```
api/v2/openapi.yaml
    │
    ├→ go-swagger generate server
    │   └→ api/v2/restapi/         # 서버 코드
    │       ├── configure_alertmanager.go
    │       └── operations/
    │           ├── alert/          # alert 엔드포인트
    │           ├── alertgroup/     # alertgroup 엔드포인트
    │           ├── general/        # status 엔드포인트
    │           ├── receiver/       # receiver 엔드포인트
    │           └── silence/        # silence 엔드포인트
    │
    ├→ go-swagger generate client
    │   └→ api/v2/client/           # 클라이언트 코드
    │
    └→ go-swagger generate model
        └→ api/v2/models/           # 24개 모델 파일
            ├── gettable_alert.go
            ├── gettable_alerts.go
            ├── gettable_silence.go
            ├── alert_group.go
            ├── alert_status.go
            ├── cluster_status.go
            ├── receiver.go
            ├── silence.go
            ├── silence_status.go
            └── ...
```

## 8. Update() — 설정 리로드

```go
// api/v2/api.go
func (api *API) Update(cfg *config.Config, setAlertStatus setAlertStatusFn) {
    api.mtx.Lock()
    defer api.mtx.Unlock()

    api.alertmanagerConfig = cfg
    api.route = dispatch.NewRoute(cfg.Route, nil)
    api.setAlertStatus = setAlertStatus
}
```

설정 리로드 시 API의 내부 설정과 Route 트리가 업데이트된다.

## 9. V1 API Deprecation

```go
// api/v1_deprecation_router.go
type V1DeprecationRouter struct{}
```

v1 API(`/api/v1/*`)에 접근하면 deprecated 메시지와 함께 v2 API 사용을 안내한다. v1은 0.27.0부터 완전히 제거되었다.

## 10. 메트릭

| 메트릭 | 타입 | 설명 |
|--------|------|------|
| `alertmanager_http_request_duration_seconds` | Histogram | HTTP 요청 소요시간 (handler, method, code) |
| `alertmanager_http_response_size_bytes` | Histogram | HTTP 응답 크기 |
| `alertmanager_http_requests_in_flight` | Gauge | 현재 처리 중인 요청 수 |
| `alertmanager_http_concurrency_limit_exceeded_total` | Counter | 동시성 제한 초과 횟수 |

## 11. compat.go — API 호환성

```go
// api/v2/compat.go
// Alert 모델 변환: API 모델 ↔ 내부 모델
func alertToOpenAPIAlert(alert *types.Alert, status types.AlertStatus, receivers []string) *open_api_models.GettableAlert
func openAPIAlertsToAlerts(apiAlerts open_api_models.PostableAlerts) []*types.Alert
```

API 모델(go-swagger 생성)과 내부 모델(types.Alert) 간의 변환을 담당한다.
