# 20. Web UI Deep-Dive

## 1. 개요

### Web UI란?

Alertmanager Web UI는 브라우저를 통해 알림 상태를 조회하고, 사일런스를 관리하며, 설정과 클러스터 상태를 확인할 수 있는 내장 웹 인터페이스이다. Alertmanager 서버 프로세스에 임베딩되어 별도의 프론트엔드 서버 없이 동작한다.

### 왜(Why) Web UI가 필요한가?

1. **즉각적인 상황 인식**: 현재 발생 중인 알림을 한 눈에 파악 가능
2. **사일런스 관리 편의성**: GUI 폼으로 매처 기반 사일런스를 쉽게 생성/수정/만료
3. **설정 확인**: 현재 실행 중인 Alertmanager 설정을 브라우저에서 직접 확인
4. **클러스터 모니터링**: HA 클러스터의 피어 상태와 연결 정보를 시각적으로 확인
5. **배포 제로**: 별도의 프론트엔드 인프라 없이 Alertmanager 바이너리 하나로 서비스
6. **디버깅 지원**: pprof 엔드포인트를 통한 런타임 프로파일링 접근

### 기술 스택 이중 구조

Alertmanager Web UI는 두 가지 프론트엔드 기술 스택을 가지고 있다:

| 구분 | 레거시 UI (현재 프로덕션) | 새 UI (개발 중) |
|------|------------------------|----------------|
| 프레임워크 | Elm (함수형 언어) | React 19 + TypeScript |
| UI 라이브러리 | Bootstrap 4 | Mantine 8 |
| 상태 관리 | Elm Architecture (MVU) | TanStack React Query |
| 라우팅 | Elm URL Parser | React Router v7 |
| 빌드 | elm make | Vite 7 |
| 경로 | `ui/app/` | `ui/mantine-ui/` |

**왜 두 가지 UI가 존재하는가?**
- 레거시 Elm UI는 안정적이지만 Elm 생태계 축소로 유지보수가 어려움
- Mantine 기반 새 UI는 React 생태계의 풍부한 라이브러리를 활용하기 위해 개발 중
- 현재는 레거시 Elm UI가 빌드되어 Go 바이너리에 임베딩됨

## 2. 소스코드 구조

```
alertmanager/ui/
├── web.go                      # Go 서버 측 핸들러 등록
├── web_test.go                 # 웹 라우트 테스트
├── Dockerfile                  # UI 빌드용 Dockerfile
│
├── app/                        # 레거시 Elm UI (프로덕션)
│   ├── index.html              # SPA 진입 HTML
│   ├── script.js               # 컴파일된 Elm → JS
│   ├── favicon.ico             # 파비콘
│   ├── elm.json                # Elm 패키지 설정
│   ├── Makefile                # Elm 빌드 스크립트
│   ├── lib/                    # 정적 라이브러리 (Bootstrap, Font-Awesome 등)
│   └── src/                    # Elm 소스코드
│       ├── Main.elm            # 앱 진입점 (init, update, view)
│       ├── Types.elm           # Model, Msg, Route 타입 정의
│       ├── Views.elm           # 메인 뷰 라우팅
│       ├── Parsing.elm         # URL 파싱
│       ├── Updates.elm         # 상태 업데이트 로직
│       ├── Data/               # API 데이터 모델
│       ├── Utils/              # 유틸리티 (API 호출, 필터 등)
│       └── Views/              # 뷰 컴포넌트
│           ├── AlertList/      # 알림 목록 뷰
│           ├── SilenceList/    # 사일런스 목록 뷰
│           ├── SilenceForm/    # 사일런스 생성/수정 폼
│           ├── SilenceView/    # 사일런스 상세 뷰
│           ├── Status/         # 상태 페이지
│           ├── NavBar/         # 네비게이션 바
│           ├── FilterBar/      # 필터 바
│           ├── GroupBar/       # 그룹 바
│           ├── ReceiverBar/    # 리시버 바
│           ├── Settings/       # 설정 뷰
│           ├── NotFound/       # 404 페이지
│           └── Shared/         # 공유 컴포넌트
│
└── mantine-ui/                 # 새 React UI (개발 중)
    ├── package.json            # npm 의존성 (React 19, Mantine 8)
    ├── vite.config.mjs         # Vite 빌드 설정
    ├── tsconfig.json           # TypeScript 설정
    └── src/
        ├── App.tsx             # 라우팅 설정, Provider 구성
        ├── main.tsx            # React 진입점
        ├── theme.ts            # Mantine 테마 설정
        ├── components/         # 공유 컴포넌트
        │   ├── Header.tsx      # 네비게이션 헤더
        │   ├── ErrorBoundary.tsx
        │   ├── InfoPageCard.tsx
        │   └── InfoPageStack.tsx
        ├── pages/              # 페이지 컴포넌트
        │   ├── Alerts.page.tsx
        │   ├── Silences.page.tsx
        │   ├── Status.page.tsx
        │   └── Config.page.tsx
        └── data/               # API 데이터 레이어
            ├── api.ts          # API 호출 유틸리티
            ├── status.ts       # 상태 API 훅
            ├── groups.ts       # 알림 그룹 API 훅
            └── silences.ts     # 사일런스 API 훅
```

## 3. Go 서버 측 아키텍처

### web.go - 핵심 핸들러 등록

```go
// 파일: ui/web.go

//go:embed app/script.js app/index.html app/favicon.ico app/lib
var asset embed.FS
```

Go 1.16의 `embed` 패키지를 사용하여 프론트엔드 정적 파일을 바이너리에 임베딩한다. 이것이 **별도의 프론트엔드 서버 없이 Alertmanager 바이너리만으로 Web UI를 서비스**할 수 있는 핵심 메커니즘이다.

### Register 함수 상세

```go
// 파일: ui/web.go
func Register(r *route.Router, reloadCh chan<- chan error, logger *slog.Logger) {
    // 1. 메트릭 엔드포인트
    r.Get("/metrics", promhttp.Handler().ServeHTTP)

    // 2. 임베딩된 파일시스템에서 서브 디렉토리 추출
    appFS, err := fs.Sub(asset, "app")
    fs := http.FileServerFS(appFS)

    // 3. 정적 파일 라우트 등록 (캐싱 비활성화)
    r.Get("/", func(w http.ResponseWriter, req *http.Request) {
        disableCaching(w)
        fs.ServeHTTP(w, req)
    })
    r.Get("/script.js", ...)   // 메인 JavaScript
    r.Get("/favicon.ico", ...) // 파비콘
    r.Get("/lib/*path", ...)   // 정적 라이브러리 (Bootstrap, Font-Awesome 등)

    // 4. 설정 리로드 엔드포인트
    r.Post("/-/reload", func(w http.ResponseWriter, req *http.Request) {
        errc := make(chan error)
        defer close(errc)
        reloadCh <- errc
        if err := <-errc; err != nil {
            http.Error(w, fmt.Sprintf("failed to reload config: %s", err),
                http.StatusInternalServerError)
        }
    })

    // 5. 헬스체크 엔드포인트
    r.Get("/-/healthy", ...)  // 200 OK
    r.Head("/-/healthy", ...) // HEAD 지원
    r.Get("/-/ready", ...)    // 200 OK
    r.Head("/-/ready", ...)   // HEAD 지원

    // 6. 디버그(pprof) 엔드포인트
    r.Get("/debug/*subpath", debugHandlerFunc)
    r.Post("/debug/*subpath", debugHandlerFunc)
}
```

### 캐싱 비활성화

```go
// 파일: ui/web.go
func disableCaching(w http.ResponseWriter) {
    w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
    w.Header().Set("Pragma", "no-cache")
    w.Header().Set("Expires", "0")
}
```

**왜 캐싱을 비활성화하는가?**
- 알림 데이터는 실시간으로 변하므로 오래된 캐시는 위험하다
- 설정 변경 후 즉시 반영되어야 한다
- 프록시가 중간에서 오래된 응답을 반환하는 것을 방지한다

### 서버 통합 구조

```go
// 파일: cmd/alertmanager/main.go (라인 581-598)
router := route.New().WithInstrumentation(instrumentHandler)
if *routePrefix != "/" {
    router.Get("/", func(w http.ResponseWriter, r *http.Request) {
        http.Redirect(w, r, *routePrefix, http.StatusFound)
    })
    router = router.WithPrefix(*routePrefix)
}

webReload := make(chan chan error)
ui.Register(router, webReload, logger)    // ← UI 핸들러 등록
mux := api.Register(router, *routePrefix) // ← API 핸들러 등록

srv := &http.Server{
    Handler: tracing.Middleware(mux),
}
```

```
HTTP 요청 처리 흐름:
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

  클라이언트 HTTP 요청
         │
         ▼
  ┌──────────────────┐
  │  tracing.Middle  │   ← OpenTelemetry 트레이싱
  │     ware(mux)    │
  └────────┬─────────┘
           ▼
  ┌──────────────────┐
  │   http.ServeMux  │   ← api.Register()가 생성
  │                  │
  │  / → router      │   ← UI 핸들러 포함
  │  /api/v2/ → API  │   ← REST API 핸들러
  └────────┬─────────┘
           │
     ┌─────┴──────┐
     ▼            ▼
  ┌──────┐   ┌──────────┐
  │  UI  │   │  API v2  │
  │ 핸들러│   │  핸들러  │
  └──────┘   └──────────┘
```

### 요청 라우팅 매핑

| 경로 패턴 | 핸들러 | 설명 |
|-----------|--------|------|
| `GET /` | FileServer (index.html) | SPA 진입점 |
| `GET /script.js` | FileServer | 컴파일된 Elm JS |
| `GET /favicon.ico` | FileServer | 파비콘 |
| `GET /lib/*path` | FileServer | 정적 라이브러리 |
| `GET /metrics` | promhttp.Handler | Prometheus 메트릭 |
| `POST /-/reload` | reloadHandler | 설정 리로드 트리거 |
| `GET /-/healthy` | healthHandler | 헬스체크 |
| `GET /-/ready` | readyHandler | 준비 상태 확인 |
| `GET,POST /debug/*` | pprof | Go 프로파일링 |
| `/api/v2/*` | API v2 핸들러 | REST API |

## 4. API v2 - Web UI 백엔드

Web UI의 모든 데이터는 API v2를 통해 제공된다. OpenAPI (Swagger) 스펙으로 정의된다.

### OpenAPI 스펙 (api/v2/openapi.yaml)

```yaml
# 파일: api/v2/openapi.yaml
swagger: '2.0'
info:
  version: 0.0.1
  title: Alertmanager API
basePath: "/api/v2/"

paths:
  /status:        # 상태 정보
  /receivers:     # 리시버 목록
  /silences:      # 사일런스 CRUD
  /silence/{silenceID}: # 개별 사일런스
  /alerts:        # 알림 목록/추가
  /alerts/groups: # 알림 그룹
```

### API 핸들러 등록 (api/v2/api.go)

```go
// 파일: api/v2/api.go (라인 133-141)
openAPI.AlertGetAlertsHandler = alert_ops.GetAlertsHandlerFunc(api.getAlertsHandler)
openAPI.AlertPostAlertsHandler = alert_ops.PostAlertsHandlerFunc(api.postAlertsHandler)
openAPI.AlertgroupGetAlertGroupsHandler = alertgroup_ops.GetAlertGroupsHandlerFunc(api.getAlertGroupsHandler)
openAPI.GeneralGetStatusHandler = general_ops.GetStatusHandlerFunc(api.getStatusHandler)
openAPI.ReceiverGetReceiversHandler = receiver_ops.GetReceiversHandlerFunc(api.getReceiversHandler)
openAPI.SilenceDeleteSilenceHandler = silence_ops.DeleteSilenceHandlerFunc(api.deleteSilenceHandler)
openAPI.SilenceGetSilenceHandler = silence_ops.GetSilenceHandlerFunc(api.getSilenceHandler)
openAPI.SilenceGetSilencesHandler = silence_ops.GetSilencesHandlerFunc(api.getSilencesHandler)
openAPI.SilencePostSilencesHandler = silence_ops.PostSilencesHandlerFunc(api.postSilencesHandler)
```

### API v2 구조체

```go
// 파일: api/v2/api.go
type API struct {
    peer           cluster.ClusterPeer
    silences       *silence.Silences
    alerts         provider.Alerts
    alertGroups    groupsFn
    getAlertStatus getAlertStatusFn
    groupMutedFunc groupMutedFunc
    uptime         time.Time

    // mtx는 동적으로 변하는 필드를 보호
    mtx                sync.RWMutex
    alertmanagerConfig *config.Config
    route              *dispatch.Route
    setAlertStatus     setAlertStatusFn

    logger *slog.Logger
    m      *metrics.Alerts
    Handler http.Handler     // ← Web UI에서 호출하는 HTTP 핸들러
}
```

### getStatusHandler - 상태 페이지 데이터

```go
// 파일: api/v2/api.go (라인 176-233)
func (api *API) getStatusHandler(params general_ops.GetStatusParams) middleware.Responder {
    api.mtx.RLock()
    defer api.mtx.RUnlock()

    original := api.alertmanagerConfig.String()
    uptime := strfmt.DateTime(api.uptime)

    resp := open_api_models.AlertmanagerStatus{
        Uptime: &uptime,
        VersionInfo: &open_api_models.VersionInfo{
            Version:   &version.Version,
            Revision:  &version.Revision,
            Branch:    &version.Branch,
            BuildUser: &version.BuildUser,
            BuildDate: &version.BuildDate,
            GoVersion: &version.GoVersion,
        },
        Config: &open_api_models.AlertmanagerConfig{
            Original: &original,
        },
        Cluster: &open_api_models.ClusterStatus{...},
    }
    return general_ops.NewGetStatusOK().WithPayload(&resp)
}
```

### CORS 및 응답 헤더 설정

```go
// 파일: api/v2/api.go
handleCORS := cors.Default().Handler
api.Handler = handleCORS(setResponseHeaders(openAPI.Serve(nil)))

var responseHeaders = map[string]string{
    "Cache-Control": "no-store",
}
```

**왜 CORS가 필요한가?**
- 개발 환경에서 `vite dev`가 다른 포트에서 실행될 때 API 호출을 허용하기 위함
- 외부 모니터링 도구에서 API를 직접 호출할 수 있도록 허용

## 5. 레거시 Elm UI 상세

### Elm Architecture (MVU 패턴)

```
┌─────────────────────────────────────────────┐
│              Elm Runtime                     │
│                                              │
│  ┌───────┐    Msg     ┌───────────┐         │
│  │       │───────────▶│           │         │
│  │ View  │            │  Update   │         │
│  │       │◀───────────│           │         │
│  │(HTML) │   Model    │ (state    │         │
│  │       │            │  machine) │         │
│  └───────┘            └─────┬─────┘         │
│                             │ Cmd           │
│                             ▼               │
│                      ┌──────────┐           │
│                      │ Effects  │           │
│                      │(HTTP, JS)│           │
│                      └──────────┘           │
└─────────────────────────────────────────────┘
```

### Main.elm - 앱 진입점

```elm
-- 파일: ui/app/src/Main.elm
main : Program Json.Value Model Msg
main =
    Browser.application
        { init = init
        , update = update
        , view = \model ->
            { title = "Alertmanager"
            , body = [ Views.view model ]
            }
        , subscriptions = always Sub.none
        , onUrlRequest = ...
        , onUrlChange = urlUpdate
        }
```

### Model 정의

```elm
-- 파일: ui/app/src/Types.elm
type alias Model =
    { silenceList : SilenceList.Model
    , silenceView : SilenceView.Model
    , silenceForm : SilenceForm.Model
    , alertList : AlertList.Model
    , route : Route
    , filter : Filter
    , status : StatusModel
    , basePath : String
    , apiUrl : String
    , libUrl : String
    , bootstrapCSS : ApiData String
    , fontAwesomeCSS : ApiData String
    , elmDatepickerCSS : ApiData String
    , defaultCreator : String
    , expandAll : Bool
    , key : Key
    , settings : SettingsView.Model
    }
```

### Route 정의

```elm
-- 파일: ui/app/src/Types.elm
type Route
    = AlertsRoute Filter          -- /#/alerts
    | NotFoundRoute               -- 404
    | SilenceFormEditRoute String  -- /#/silences/<id>/edit
    | SilenceFormNewRoute SilenceFormGetParams  -- /#/silences/new
    | SilenceListRoute Filter     -- /#/silences
    | SilenceViewRoute String     -- /#/silences/<id>
    | StatusRoute                 -- /#/status
    | TopLevelRoute               -- / (리다이렉트)
    | SettingsRoute               -- /#/settings
```

**URL 해시 라우팅**: 레거시 UI는 `/#/` 기반 라우팅을 사용한다. SPA이므로 서버 측에서는 항상 `index.html`을 반환하고, Elm이 URL 해시를 파싱하여 라우팅한다.

### Msg 타입 (이벤트 정의)

```elm
-- 파일: ui/app/src/Types.elm
type Msg
    = MsgForAlertList AlertListMsg
    | MsgForSilenceView SilenceViewMsg
    | MsgForSilenceForm SilenceFormMsg
    | MsgForSilenceList SilenceListMsg
    | MsgForStatus StatusMsg
    | MsgForSettings SettingsMsg
    | NavigateToAlerts Filter
    | NavigateToSilenceView String
    | NavigateToSilenceFormEdit String
    | NavigateToSilenceFormNew SilenceFormGetParams
    | NavigateToSilenceList Filter
    | NavigateToStatus
    | NavigateToSettings
    | ...
```

### 뷰 라우팅

```elm
-- 파일: ui/app/src/Views.elm
currentView : Model -> Html Msg
currentView model =
    case model.route of
        SettingsRoute ->
            SettingsView.view model.settings |> Html.map MsgForSettings

        StatusRoute ->
            Status.view model.status

        SilenceViewRoute _ ->
            SilenceView.view model.silenceView

        AlertsRoute filter ->
            AlertList.view model.alertList filter

        SilenceListRoute _ ->
            SilenceList.view model.silenceList

        SilenceFormNewRoute getParams ->
            SilenceForm.view Nothing getParams model.defaultCreator model.silenceForm
                |> Html.map MsgForSilenceForm

        SilenceFormEditRoute silenceId ->
            SilenceForm.view (Just silenceId) emptySilenceFormGetParams ""
                model.silenceForm |> Html.map MsgForSilenceForm

        TopLevelRoute ->
            Utils.Views.loading

        NotFoundRoute ->
            NotFound.view
```

### Elm 뷰 컴포넌트 구조

```
Views/
├── NavBar/          네비게이션 바 (Alerts | Silences | Status)
├── AlertList/       알림 목록
│   ├── Types.elm    AlertList 모델/메시지 정의
│   ├── Views.elm    알림 목록 렌더링
│   ├── AlertView.elm 개별 알림 렌더링
│   ├── Updates.elm  상태 업데이트 로직
│   └── Parsing.elm  URL 파싱
├── SilenceList/     사일런스 목록
│   ├── Types.elm    SilenceList 모델/메시지 정의
│   ├── Views.elm    사일런스 목록 렌더링
│   ├── SilenceView.elm 개별 사일런스 렌더링
│   ├── Updates.elm  상태 업데이트
│   └── Parsing.elm  URL 파싱
├── SilenceForm/     사일런스 생성/수정 폼
│   ├── Types.elm    SilenceForm 모델/메시지 정의
│   ├── Views.elm    폼 렌더링
│   ├── Updates.elm  폼 상태 관리
│   └── Parsing.elm  URL 파싱
├── SilenceView/     사일런스 상세 뷰
├── Status/          상태 페이지
│   ├── Types.elm    StatusModel 정의
│   ├── Views.elm    버전/클러스터 정보 렌더링
│   ├── Updates.elm  API 호출/응답 처리
│   └── Parsing.elm  데이터 파싱
├── FilterBar/       필터 바 (매처 입력)
├── GroupBar/        알림 그룹 바
├── ReceiverBar/     리시버 필터 바
├── Settings/        사용자 설정 뷰
├── NotFound/        404 페이지
└── Shared/          공유 유틸리티
```

## 6. 새 Mantine UI (React) 상세

### 기술 스택

```json
// 파일: ui/mantine-ui/package.json (주요 의존성)
{
  "dependencies": {
    "@mantine/core": "^8.3.13",          // UI 컴포넌트 라이브러리
    "@mantine/code-highlight": "^8.3.14", // 코드 하이라이팅
    "@tanstack/react-query": "^5.90.20",  // 서버 상태 관리
    "react": "^19.1.0",                   // React 19
    "react-router-dom": "^7.13.0"         // 클라이언트 라우팅
  }
}
```

### App.tsx - 라우팅 및 Provider 구성

```tsx
// 파일: ui/mantine-ui/src/App.tsx
export default function App() {
  return (
    <BrowserRouter>
      <MantineProvider theme={theme}>
        <CodeHighlightAdapterProvider adapter={highlightJsAdapter}>
          <QueryClientProvider client={queryClient}>
            <AppShell padding="md" header={{ height: 60 }}>
              <Header />
              <AppShell.Main>
                <ErrorBoundary key={location.pathname}>
                  <Suspense fallback={<Skeleton ... />}>
                    <Routes>
                      <Route path="/" element={<Navigate to="/alerts" replace />} />
                      <Route path="/alerts" element={<AlertsPage />} />
                      <Route path="/silences" element={<SilencesPage />} />
                      <Route path="/status" element={<StatusPage />} />
                      <Route path="/config" element={<ConfigPage />} />
                    </Routes>
                  </Suspense>
                </ErrorBoundary>
              </AppShell.Main>
            </AppShell>
          </QueryClientProvider>
        </CodeHighlightAdapterProvider>
      </MantineProvider>
    </BrowserRouter>
  );
}
```

**Provider 스택 (바깥 → 안쪽)**:

```
BrowserRouter          ← URL 라우팅
  └─ MantineProvider   ← UI 테마/스타일
       └─ CodeHighlight ← YAML 코드 하이라이팅
            └─ QueryClient ← API 캐시/상태 관리
                 └─ AppShell  ← 레이아웃 (헤더+본문)
                      └─ ErrorBoundary ← 에러 처리
                           └─ Suspense ← 로딩 상태 (스켈레톤 UI)
                                └─ Routes ← 페이지 라우팅
```

### 데이터 레이어 (data/)

#### api.ts - API 호출 유틸리티

```typescript
// 파일: ui/mantine-ui/src/data/api.ts
const pathPrefix = '';
export const API_PATH = 'api/v2';

const createQueryFn = <T>({ pathPrefix, path, params, recordResponseTime }) =>
  async ({ signal }: { signal: AbortSignal }) => {
    const res = await fetch(`${pathPrefix}/${API_PATH}${path}${queryString}`, {
      cache: 'no-store',           // 캐싱 비활성화
      credentials: 'same-origin',  // 인증 쿠키 포함
      signal,                      // AbortController 시그널
    });
    // ... 응답 파싱
  };

// Suspense용 훅 (데이터 로딩 중 자동 서스펜스)
export const useSuspenseAPIQuery = <T>({ key, path, params }) => {
  return useSuspenseQuery<T>({
    queryKey: key !== undefined ? key : [path, params],
    retry: false,
    refetchOnWindowFocus: false,
    gcTime: 0,           // 캐시 즉시 삭제 (항상 최신 데이터)
    queryFn: createQueryFn({ pathPrefix, path, params }),
  });
};
```

**핵심 설계 결정**:
- `gcTime: 0`: 캐시를 사용하지 않아 항상 최신 데이터 표시
- `retry: false`: 실패 시 자동 재시도 안 함 (사용자가 직접 새로고침)
- `refetchOnWindowFocus: false`: 탭 전환 시 자동 갱신 안 함

#### status.ts - 상태 API 타입 및 훅

```typescript
// 파일: ui/mantine-ui/src/data/status.ts
type Status = {
  cluster: {
    name: string;
    peers: Array<{ name: string; address: string }>;
    status: 'ready' | 'not_ready';
  };
  config: { original: string };
  uptime: string;
  versionInfo: {
    branch: string;
    buildDate: string;
    buildUser: string;
    goVersion: string;
    version: string;
    revision: string;
  };
};

export const useStatus = () => {
  return useSuspenseAPIQuery<Status>({ path: '/status' });
};
```

#### groups.ts - 알림 그룹 API

```typescript
// 파일: ui/mantine-ui/src/data/groups.ts
type AlertStatus = {
  inhibitedBy: string[];
  silencedBy: string[];
  mutedBy: string[];
  state: 'active';
};

type Alert = {
  annotations: Record<string, string>;
  endsAt: string;
  fingerprint: string;
  receivers: Receiver[];
  startsAt: string;
  status: AlertStatus;
  updatedAt: string;
  labels: Record<string, string>;
};

type Group = {
  alerts: Alert[];
  labels: Record<string, string>;
  receiver: Receiver;
};

export const useGroups = () => {
  return useSuspenseAPIQuery<Array<Group>>({ path: '/alerts/groups' });
};
```

#### silences.ts - 사일런스 API

```typescript
// 파일: ui/mantine-ui/src/data/silences.ts
type Silence = {
  id: string;
  status: { state: 'active' | 'expired' | 'pending' };
  startsAt: string;
  updatedAt: string;
  endsAt: string;
  createdBy: string;
  comment: string;
  matchers: Array<{
    name: string;
    value: string;
    isRegex: boolean;
    isEqual: boolean;
  }>;
};

export const useSilences = () => {
  return useSuspenseAPIQuery<Array<Silence>>({ path: '/silences' });
};

export const useSilence = (id: string) => {
  return useSuspenseAPIQuery<Silence>({ path: `/silence/${id}` });
};
```

### 페이지 컴포넌트

#### Status 페이지

```tsx
// 파일: ui/mantine-ui/src/pages/Status.page.tsx
export function StatusPage() {
  const { data } = useStatus();
  return (
    <InfoPageStack>
      <InfoPageCard title="Build information">
        <Table layout="fixed">
          <Table.Tbody>
            <Table.Tr>
              <Table.Th>Version</Table.Th>
              <Table.Td>{data.versionInfo.version}</Table.Td>
            </Table.Tr>
            {/* ... revision, branch, buildUser, buildDate, goVersion */}
          </Table.Tbody>
        </Table>
      </InfoPageCard>
      <InfoPageCard title="Runtime information">
        {/* uptime, cluster name/status, peer count */}
      </InfoPageCard>
      <InfoPageCard title="Cluster Peers">
        {/* peer name/address 테이블 */}
      </InfoPageCard>
    </InfoPageStack>
  );
}
```

#### Config 페이지

```tsx
// 파일: ui/mantine-ui/src/pages/Config.page.tsx
export function ConfigPage() {
  const { data } = useStatus();
  return (
    <CodeHighlight
      language="yaml"
      code={data.config.original}
      miw="50vw"
      w="fit-content"
      maw="calc(100vw - 75px)"
      mx="auto"
      mt="xs"
    />
  );
}
```

YAML 구문 하이라이팅으로 현재 실행 중인 Alertmanager 설정을 표시한다.

### Header 컴포넌트 (네비게이션)

```tsx
// 파일: ui/mantine-ui/src/components/Header.tsx
export const Header = () => {
  const mainNavPages = [
    { title: 'Alerts', path: '/alerts', element: <AlertsPage /> },
    { title: 'Silences', path: '/silences', element: <SilencesPage /> },
  ];

  // Status 드롭다운 메뉴
  // ├── Runtime & Build Information → /status
  // └── Configuration → /config

  return (
    <AppShell.Header>
      <Group h="100%" px="md" wrap="nowrap">
        {/* Alertmanager 로고 + 네비게이션 링크 */}
        <Link to="/">Alertmanager</Link>
        {navLinks}
      </Group>
    </AppShell.Header>
  );
};
```

**네비게이션 구조**:

```
┌────────────────────────────────────────────────────────────┐
│  Alertmanager    [Alerts]  [Silences]  [Status ▼]          │
│                                         ├ Runtime & Build  │
│                                         └ Configuration    │
└────────────────────────────────────────────────────────────┘
```

## 7. 동시성 제어와 보안

### API 동시성 제한 (api/api.go)

```go
// 파일: api/api.go
type API struct {
    // ...
    requestsInFlight         prometheus.Gauge
    concurrencyLimitExceeded prometheus.Counter
    timeout                  time.Duration
    inFlightSem              chan struct{}  // 세마포어
}

func (api *API) limitHandler(h http.Handler) http.Handler {
    concLimiter := http.HandlerFunc(func(rsp http.ResponseWriter, req *http.Request) {
        if req.Method == http.MethodGet {
            select {
            case api.inFlightSem <- struct{}{}:
                // 세마포어 획득 성공
                api.requestsInFlight.Inc()
                defer func() {
                    <-api.inFlightSem
                    api.requestsInFlight.Dec()
                }()
            default:
                // 동시성 제한 초과 → 503 반환
                api.concurrencyLimitExceeded.Inc()
                http.Error(rsp, fmt.Sprintf(
                    "Limit of concurrent GET requests reached (%d), try again later.\n",
                    cap(api.inFlightSem)),
                    http.StatusServiceUnavailable)
                return
            }
        }
        h.ServeHTTP(rsp, req)
    })

    if api.timeout <= 0 {
        return concLimiter
    }
    return http.TimeoutHandler(concLimiter, api.timeout, ...)
}
```

**왜 GET만 제한하는가?**
- GET 요청은 알림/사일런스 목록 조회로, 대량 데이터 처리가 필요
- POST 요청(알림 수신, 사일런스 생성)은 제한하면 데이터 유실 위험
- 기본 동시성: `max(GOMAXPROCS, 8)`

### API 요청 계측 (Instrumentation)

```go
// 파일: api/api.go
func (api *API) instrumentHandler(prefix string, h http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        path, _ := strings.CutPrefix(r.URL.Path, prefix)
        // 높은 카디널리티 방지: 사일런스 ID를 플레이스홀더로 교체
        if strings.HasPrefix(path, "/api/v2/silence/") {
            path = "/api/v2/silence/{silenceID}"
        }
        promhttp.InstrumentHandlerDuration(
            api.requestDuration.MustCurryWith(prometheus.Labels{"handler": path}),
            otelhttp.NewHandler(h, path),
        ).ServeHTTP(w, r)
    })
}
```

## 8. 빌드 및 임베딩 파이프라인

### 전체 빌드 플로우

```
┌──────────────────────────────────────────────────────────┐
│                    빌드 파이프라인                         │
│                                                          │
│  1. Elm 소스 컴파일                                      │
│     ui/app/src/*.elm                                    │
│         │  elm make                                     │
│         ▼                                               │
│     ui/app/script.js (단일 JS 파일)                     │
│                                                          │
│  2. 정적 자산 수집                                       │
│     ui/app/index.html                                   │
│     ui/app/script.js                                    │
│     ui/app/favicon.ico                                  │
│     ui/app/lib/ (Bootstrap, Font-Awesome 등)            │
│                                                          │
│  3. Go embed                                            │
│     //go:embed app/script.js app/index.html             │
│                app/favicon.ico app/lib                  │
│     var asset embed.FS                                  │
│         │  go build                                     │
│         ▼                                               │
│     alertmanager 바이너리 (UI 포함)                      │
│                                                          │
└──────────────────────────────────────────────────────────┘
```

### embed 디렉티브

```go
// 파일: ui/web.go
//go:embed app/script.js app/index.html app/favicon.ico app/lib
var asset embed.FS
```

**왜 embed를 사용하는가?**
- 단일 바이너리 배포: 별도의 정적 파일 디렉토리 불필요
- 컨테이너 이미지 최소화: FROM scratch 가능
- 파일 누락 방지: 컴파일 시점에 파일 존재 여부 확인

## 9. 테스트

### web_test.go - 웹 라우트 테스트

```go
// 파일: ui/web_test.go
func TestWebRoutes(t *testing.T) {
    router := route.New()
    logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
    Register(router, make(chan chan error), logger)

    tests := []struct {
        name         string
        path         string
        expectedCode int
    }{
        {name: "root", path: "/"},
        {name: "script.js", path: "/script.js"},
        {name: "favicon.ico", path: "/favicon.ico"},
        {name: "Lib wildcard path",
         path: "/lib/elm-datepicker/css/elm-datepicker.css"},
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            req := httptest.NewRequest(http.MethodGet, tt.path, nil)
            w := httptest.NewRecorder()
            router.ServeHTTP(w, req)
            require.Equal(t, http.StatusOK, res.StatusCode)
        })
    }
}
```

### pprof 라우트 프리픽스 테스트

```go
// 파일: ui/web_test.go
func TestDebugHandlersWithRoutePrefix(t *testing.T) {
    routePrefix := "/prometheus/alertmanager"
    router := route.New().WithPrefix(routePrefix)
    Register(router, reloadCh, logger)

    // 프리픽스 포함한 pprof 접근 테스트
    req := httptest.NewRequest("GET", routePrefix+"/debug/pprof/", nil)
    w := httptest.NewRecorder()
    router.ServeHTTP(w, req)
    require.Equal(t, http.StatusOK, w.Code)
    require.Contains(t, w.Body.String(), "/debug/pprof/")
}
```

## 10. 디버그 엔드포인트

### pprof 핸들러

```go
// 파일: ui/web.go
debugHandlerFunc := func(w http.ResponseWriter, req *http.Request) {
    subpath := route.Param(req.Context(), "subpath")
    req.URL.Path = path.Join("/debug", subpath)
    // path.Join은 후행 슬래시를 제거하지만 pprof는 슬래시가 필요
    if strings.HasSuffix(subpath, "/") && !strings.HasSuffix(req.URL.Path, "/") {
        req.URL.Path += "/"
    }
    http.DefaultServeMux.ServeHTTP(w, req)
}
r.Get("/debug/*subpath", debugHandlerFunc)
r.Post("/debug/*subpath", debugHandlerFunc)
```

**사용 가능한 pprof 엔드포인트**:

| 경로 | 설명 |
|------|------|
| `/debug/pprof/` | 프로파일 인덱스 페이지 |
| `/debug/pprof/heap` | 힙 메모리 프로파일 |
| `/debug/pprof/goroutine` | 고루틴 스택 |
| `/debug/pprof/profile` | CPU 프로파일 |
| `/debug/pprof/trace` | 실행 트레이스 |

`import _ "net/http/pprof"` 블랭크 임포트로 pprof 핸들러가 `http.DefaultServeMux`에 자동 등록된다.

## 11. 설정 리로드 메커니즘

### 리로드 채널 패턴

```go
// 파일: ui/web.go
r.Post("/-/reload", func(w http.ResponseWriter, req *http.Request) {
    errc := make(chan error)
    defer close(errc)

    reloadCh <- errc          // 리로드 요청을 채널로 전송
    if err := <-errc; err != nil {  // 결과 대기
        http.Error(w, fmt.Sprintf("failed to reload config: %s", err),
            http.StatusInternalServerError)
    }
})
```

```
리로드 흐름:
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

  HTTP POST /-/reload
       │
       ▼
  ┌──────────────┐   errc chan   ┌──────────────────┐
  │ Web Handler  │──────────────▶│ Main Goroutine   │
  │              │               │                  │
  │ errc 생성    │               │ config.LoadFile()│
  │ reloadCh←errc│               │ 검증 및 적용     │
  │              │◀──────────────│                  │
  │ err=<-errc   │   err/nil     │ errc <- err      │
  │              │               │                  │
  │ 응답 반환    │               │                  │
  └──────────────┘               └──────────────────┘
```

**동기 방식 설계**: 리로드 요청 후 결과를 기다려서 HTTP 응답에 성공/실패를 반영한다. 비동기 리로드는 결과를 알 수 없어 운영에 불편하다.

## 12. 다른 서브시스템과의 관계

```
┌─────────────────────────────────────────────────────────┐
│                    Web UI 아키텍처                        │
│                                                          │
│  ┌──────────────┐    HTTP    ┌───────────────────────┐  │
│  │ 브라우저      │◀──────────│ ui/web.go             │  │
│  │ (Elm/React)  │            │ (정적 파일 서빙)      │  │
│  └──────┬───────┘            └───────────────────────┘  │
│         │ fetch                                          │
│         ▼                                                │
│  ┌──────────────────────────────────────────────────┐   │
│  │                 API v2 Server                     │   │
│  │  ┌──────────┐ ┌──────────┐ ┌────────────────┐   │   │
│  │  │ Alert    │ │ Silence  │ │ General/Status │   │   │
│  │  │ Handler  │ │ Handler  │ │ Handler        │   │   │
│  │  └────┬─────┘ └────┬─────┘ └────────┬───────┘   │   │
│  └───────┼─────────────┼────────────────┼───────────┘   │
│          ▼             ▼                ▼                │
│  ┌────────────┐ ┌───────────┐ ┌──────────────────┐     │
│  │ provider   │ │ silence   │ │ config/dispatch  │     │
│  │ .Alerts    │ │ .Silences │ │ (설정/라우팅)     │     │
│  └────────────┘ └───────────┘ └──────────────────┘     │
└─────────────────────────────────────────────────────────┘
```

- **API v2** (15-api-v2.md): Web UI의 모든 데이터를 제공하는 백엔드
- **Silence** (09-silence.md): 사일런스 CRUD 로직
- **Alert Provider** (12-alert-provider.md): 알림 데이터 스토어
- **Config Management** (13-config-management.md): 설정 리로드 메커니즘
- **Dispatcher** (07-dispatcher.md): 알림 그룹핑 로직

## 13. 설계 원칙 요약

| 원칙 | 구현 |
|------|------|
| **단일 바이너리** | `embed.FS`로 프론트엔드를 Go 바이너리에 포함 |
| **캐시 무효화** | 모든 정적 응답에 `Cache-Control: no-cache` 설정 |
| **동시성 보호** | GET 요청에 세마포어 기반 동시성 제한 적용 |
| **CORS 지원** | 개발/외부 도구 연동을 위한 CORS 허용 |
| **헬스체크** | `/-/healthy`, `/-/ready` 엔드포인트로 로드밸런서 통합 |
| **디버깅 내장** | pprof 엔드포인트로 운영 중 프로파일링 지원 |
| **동기 리로드** | 채널 기반 동기 리로드로 결과 확인 가능 |
| **점진적 마이그레이션** | Elm → React 전환을 두 UI 병행으로 안전하게 진행 |
| **OpenAPI 기반** | Swagger 스펙으로 API 계약 명시 (클라이언트 자동 생성) |
