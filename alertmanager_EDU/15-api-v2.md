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

## 12. 실제 소스 코드 심화 분석

### 12.1 postAlertsHandler — Alert 유효성 검증 상세

```
postAlertsHandler(params):
    각 Alert에 대해:
    1. Labels 검증:
       - Labels가 nil이거나 비어있으면 → invalid 카운터 증가, 건너뜀
       - model.LabelName 유효성 검사 (영문자, 숫자, _만 허용)
       - model.LabelValue 유효성 검사 (UTF-8)

    2. 시간 설정:
       - StartsAt 미지정 → now
       - EndsAt 미지정 → StartsAt + Global.ResolveTimeout
       - EndsAt가 StartsAt 이전이면 → EndsAt = StartsAt + ResolveTimeout

    3. Annotations 검증:
       - Annotation 이름 유효성 검사

    4. 유효한 Alert만 alerts.Put()으로 저장
    5. setAlertStatus() → Silencer 매칭 상태 업데이트

    에러 메시지 수집:
    - 유효하지 않은 Alert은 건너뛰되, 에러 메시지를 모은다
    - 유효한 Alert이 하나도 없으면 400 반환
    - 일부만 유효하면 유효한 것만 저장 + 200 반환
```

**왜 부분 성공을 허용하는가?**

Prometheus가 여러 Alert를 한 번에 전송할 때, 일부 Alert의 레이블이 잘못되었다고 전체를 거부하면 유효한 Alert도 처리되지 않는다. 부분 성공으로 유효한 Alert은 즉시 처리하면서, 잘못된 Alert은 로그에 기록한다.

### 12.2 getAlertsHandler — Receiver 기반 필터링

```
getAlertsHandler에서 receiver 파라미터:
    1. Route 트리에서 해당 receiver로 매칭되는 Route 찾기
    2. 각 Alert에 대해:
       - Alert Labels가 해당 Route에 매칭되는지 확인
       - 매칭되면 결과에 포함
    3. 여러 Route가 동일 receiver를 사용하면 모두 확인

이 필터는 "이 Receiver가 처리할 Alert만 보여줘"라는 의미이다.
```

### 12.3 getAlertGroupsHandler — 그룹 뮤트 상태

```go
// api/v2/api.go
// 각 AlertGroup에 대해 뮤트 상태를 확인
mutedBy, isMuted := api.groupMutedFunc(routeID, group.Key())
if isMuted {
    alertGroup.MutedBy = mutedBy  // 뮤트 원인 (TimeInterval 이름)
}
```

**왜 그룹 단위로 뮤트 상태를 확인하는가?**

TimeInterval(mute_time_intervals)은 Route 단위로 적용된다. Route → Group 관계에서, Route가 뮤트되면 해당 Route의 모든 Group이 뮤트된다. API 응답에 뮤트 원인(TimeInterval 이름)을 포함하여 UI에서 표시한다.

---

## 13. 에러 처리 패턴

### 13.1 API 에러 응답 형식

```json
// 400 Bad Request
{
    "status": "error",
    "errorType": "bad_data",
    "error": "invalid label set: ..."
}

// 404 Not Found
{
    "status": "error",
    "errorType": "not_found",
    "error": "silence 12345 not found"
}

// 503 Service Unavailable (동시성 제한 초과)
{
    "status": "error",
    "errorType": "server_error",
    "error": "concurrency limit reached"
}
```

### 13.2 동시성 제한 메커니즘

```
limitHandler 동작:
    inFlightSem = make(chan struct{}, Concurrency)

    GET 요청:
    ├── select {
    │   case inFlightSem <- struct{}{}:  // 토큰 확보
    │       defer <-inFlightSem          // 완료 후 토큰 반환
    │       handler.ServeHTTP(w, r)      // 핸들러 실행
    │   case <-time.After(timeout):      // 타임아웃
    │       503 Service Unavailable
    │       concurrencyLimitExceeded.Inc()
    │   }

    POST/DELETE 요청:
    └── 동시성 제한 없이 직접 실행
```

**왜 GET만 제한하고 POST는 제한하지 않는가?**

GET은 Alert/Silence 전체 목록을 조회하는 비용이 높은 작업이다. 대시보드 등에서 자동 폴링하면 대량 GET이 발생할 수 있다. POST는 Alert 수신이므로 지연되면 모니터링 공백이 생긴다.

---

## 14. 성능 고려사항

| 항목 | 설계 | 이유 |
|------|------|------|
| GET 동시성 제한 | 세마포어 채널 | 대량 폴링으로 인한 과부하 방지 |
| Alert 부분 성공 | 유효한 Alert만 저장 | Prometheus 전송 실패 최소화 |
| RWMutex | config, route 읽기/쓰기 분리 | 설정 리로드 중에도 API 응답 가능 |
| go-swagger 코드 생성 | 자동 생성된 모델/핸들러 | 수동 코딩 에러 방지, 명세 일치 보장 |

---

## 15. 운영 가이드

### 15.1 동시성 제한 튜닝

```bash
alertmanager --web.get-concurrency=20 --web.timeout=30s
```

| 파라미터 | 기본값 | 권장 |
|---------|--------|------|
| `--web.get-concurrency` | 0 (무제한) | CPU 코어 수 * 2 |
| `--web.timeout` | 0 (무제한) | 30s |

### 15.2 모니터링 권장 쿼리

```promql
# 동시성 제한 초과 빈도
rate(alertmanager_http_concurrency_limit_exceeded_total[5m])

# API 응답 시간 p99
histogram_quantile(0.99, rate(alertmanager_http_request_duration_seconds_bucket[5m]))

# 현재 처리 중인 요청
alertmanager_http_requests_in_flight
```

## 16. API v2 엔드포인트 전체 목록

| 메서드 | 경로 | 설명 |
|--------|------|------|
| GET | `/api/v2/status` | Alertmanager 상태 조회 |
| GET | `/api/v2/alerts` | 활성 알림 목록 조회 |
| POST | `/api/v2/alerts` | 새 알림 생성/갱신 |
| GET | `/api/v2/alerts/groups` | 알림 그룹별 조회 |
| GET | `/api/v2/silences` | Silence 목록 조회 |
| POST | `/api/v2/silences` | 새 Silence 생성 |
| GET | `/api/v2/silence/{silenceID}` | 특정 Silence 조회 |
| DELETE | `/api/v2/silence/{silenceID}` | Silence 만료 처리 |
| GET | `/api/v2/receivers` | 수신자 목록 조회 |

모든 엔드포인트는 OpenAPI 2.0 스펙(`api/v2/openapi.yaml`)에 정의되어 있으며, `go-swagger`를 통해 서버 코드가 자동 생성된다. 핸들러 구현은 `api/v2/api.go`의 `API` 구조체에서 각 엔드포인트별 핸들러 함수를 주입받는 방식이다.

```
api/v2/openapi.yaml → go-swagger codegen → api/v2/restapi/ (자동생성)
api/v2/api.go → API struct → 핸들러 함수 주입 → 실제 비즈니스 로직 연결
```
