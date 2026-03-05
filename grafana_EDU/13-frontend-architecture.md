# 13. Grafana 프론트엔드 아키텍처 심화

## 목차

1. [전체 구조 개요](#1-전체-구조-개요)
2. [부트스트랩 진입점: app.ts](#2-부트스트랩-진입점-appts)
3. [Root React 컴포넌트: AppWrapper.tsx](#3-root-react-컴포넌트-appwrappertsx)
4. [Provider 계층 구조](#4-provider-계층-구조)
5. [상태 관리: Redux Toolkit + RTK Query](#5-상태-관리-redux-toolkit--rtk-query)
6. [라우팅 시스템](#6-라우팅-시스템)
7. [대시보드 렌더링 파이프라인](#7-대시보드-렌더링-파이프라인)
8. [패널 플러그인 SDK](#8-패널-플러그인-sdk)
9. [@grafana/* 패키지 생태계](#9-grafana-패키지-생태계)
10. [Explore 모드](#10-explore-모드)
11. [스타일링 시스템](#11-스타일링-시스템)
12. [빌드 도구 체인](#12-빌드-도구-체인)

---

## 1. 전체 구조 개요

Grafana 프론트엔드는 `public/app/` 디렉토리를 기반으로 하며, 대규모 모노리식 React 애플리케이션이다.
전체 디렉토리 구조는 다음과 같다.

```
public/app/
├── app.ts              # 부트스트랩 진입점 (GrafanaApp 클래스)
├── AppWrapper.tsx       # Root React 컴포넌트 (Provider 계층)
├── index.ts             # 최종 진입 모듈
├── initApp.ts           # 추가 초기화 로직
├── core/                # 핵심 인프라
│   ├── components/      # AppChrome, Login, Page, OptionsUI 등
│   ├── context/         # GrafanaContext (React Context)
│   ├── navigation/      # GrafanaRoute, types (RouteDescriptor)
│   ├── reducers/        # root.ts (40+ 리듀서 결합), navModel
│   ├── services/        # backend_srv, context_srv, echo, keybinding
│   └── utils/           # ConfigProvider(ThemeProvider), metrics
├── features/            # 52개 기능 모듈
│   ├── dashboard/       # 대시보드 핵심
│   ├── dashboard-scene/ # Scenes 기반 신규 대시보드
│   ├── explore/         # Explore 모드
│   ├── alerting/        # 통합 알림
│   ├── plugins/         # 플러그인 로딩/확장
│   ├── variables/       # 템플릿 변수 시스템
│   ├── query/           # QueryRunner, runRequest
│   ├── panel/           # PanelRenderer, PanelDataErrorView
│   └── ...              # 42개 추가 기능 모듈
├── plugins/             # 플러그인 임포트/로딩
├── routes/              # routes.tsx, RoutesWrapper
├── store/               # configureStore.ts, store.ts
└── types/               # 전역 타입 정의 (explore.ts 등)
```

### 왜 이 구조인가?

Grafana는 10년 이상 발전해온 프로젝트다. 초기 AngularJS 기반에서 React로 마이그레이션하면서
`features/` 디렉토리에 도메인별로 코드를 분리하는 구조를 채택했다. 현재 52개의 feature 모듈이 존재하며,
각 모듈은 자체 라우트, 리듀서, 컴포넌트를 포함한다. 이 구조 덕분에 각 팀이 독립적으로 기능을 개발할 수 있다.

---

## 2. 부트스트랩 진입점: app.ts

`public/app/app.ts`는 Grafana 프론트엔드의 실질적인 시작점이다.
`GrafanaApp` 클래스를 정의하고, `init()` 메서드에서 전체 초기화 파이프라인을 실행한다.

### 초기화 흐름 (검증된 코드 경로)

```
app.ts: GrafanaApp.init()
│
├── 1. preInitTasks()              # 라이프사이클 훅 (사전 초기화)
├── 2. initSystemJSHooks()         # SystemJS 플러그인 로딩 인터셉터
├── 3. initOpenFeature()           # Feature Flag 클라이언트 초기화
├── 4. initializeI18n()            # i18n 국제화 (NAMESPACES, loadTranslations)
├── 5. setBackendSrv(backendSrv)   # HTTP 클라이언트 등록
├── 6. initEchoSrv()               # 텔레메트리/분석 서비스
├── 7. 글로벌 서비스 등록
│   ├── setLocale, setWeekStart
│   ├── setPanelRenderer(PanelRenderer)
│   ├── setPluginPage(PluginPage)
│   ├── setLocationSrv(locationService)
│   ├── setCorrelationsService(CorrelationsService)
│   ├── setEmbeddedDashboard(EmbeddedDashboardLazy)
│   └── setTimeZoneResolver(...)
├── 8. initGrafanaLive()           # WebSocket 실시간 스트리밍
├── 9. configureStore()            # Redux Store 생성
├── 10. initAlerting()             # 통합 알림 초기화
├── 11. 레지스트리 초기화
│   ├── standardEditorsRegistry
│   ├── standardFieldConfigEditorRegistry
│   ├── standardTransformersRegistry
│   └── variableAdapters (9종)
├── 12. Monaco 에디터 설정
│   ├── monacoLanguageRegistry
│   └── setMonacoEnv()
├── 13. 쿼리 인프라
│   ├── setQueryRunnerFactory(() => new QueryRunner())
│   ├── setVariableQueryRunner(new VariableQueryRunner())
│   └── setRunRequest(runRequest)
├── 14. 플러그인 시스템
│   ├── setPluginImportUtils({importPanelPlugin, getPanelPluginFromCache})
│   ├── preloadPlugins(getAppPluginsToPreload())
│   ├── getPluginExtensionRegistries()
│   └── setPluginLinksHook, setPluginComponentHook 등
├── 15. Chrome 서비스
│   ├── AppChromeService
│   ├── KeybindingSrv
│   └── NewFrontendAssetsChecker
├── 16. GrafanaContextType 생성
│   └── { backend, location, chrome, keybindings, newAssetsChecker, config }
└── 17. React 렌더링
    ├── createRoot(document.getElementById('reactRoot'))
    └── root.render(createElement(AppWrapper, { context }))
```

### 변수 어댑터 등록 (variableAdapters)

`app.ts`에서는 9가지 변수 어댑터를 등록한다.

| 어댑터 | 용도 |
|--------|------|
| `createQueryVariableAdapter()` | 데이터소스 쿼리 기반 변수 |
| `createCustomVariableAdapter()` | 사용자 정의 값 목록 |
| `createTextBoxVariableAdapter()` | 자유 텍스트 입력 |
| `createConstantVariableAdapter()` | 상수 값 |
| `createDataSourceVariableAdapter()` | 데이터소스 선택 |
| `createIntervalVariableAdapter()` | 시간 간격 |
| `createAdHocVariableAdapter()` | Ad-hoc 필터 |
| `createSystemVariableAdapter()` | 시스템 변수 (__from, __to 등) |
| `createSwitchVariableAdapter()` | 스위치/토글 변수 |

### 왜 이렇게 많은 글로벌 서비스가 필요한가?

Grafana는 `@grafana/runtime` 패키지를 통해 플러그인에 서비스를 제공한다.
플러그인은 Grafana 코어와 별도로 번들링되므로, `set*` 함수로 런타임에 서비스 인스턴스를 주입해야 한다.
이는 일종의 서비스 로케이터 패턴으로, 플러그인이 코어 모듈에 직접 의존하지 않으면서도
`getBackendSrv()`, `getLocationSrv()` 등으로 서비스에 접근할 수 있게 한다.

---

## 3. Root React 컴포넌트: AppWrapper.tsx

`public/app/AppWrapper.tsx`는 Grafana의 최상위 React 컴포넌트다.
클래스 컴포넌트로 구현되어 있으며, 12단계의 Provider 중첩 구조를 가진다.

```typescript
// AppWrapper.tsx (검증된 코드)
export class AppWrapper extends Component<AppWrapperProps, AppWrapperState> {
  async componentDidMount() {
    const registries = await getPluginExtensionRegistries();
    this.setState({ ready: true, registries });
    this.removePreloader();
    // Icon 캐시 정리
  }

  render() {
    return (
      <Provider store={store}>                              {/* 1. Redux Store */}
        <ErrorBoundaryAlert style="page">                   {/* 2. 에러 경계 */}
          <OpenFeatureProvider client={...}>                 {/* 3. Feature Flags */}
            <GrafanaContext.Provider value={context}>        {/* 4. Grafana Context */}
              <ThemeProvider value={config.theme2}>          {/* 5. 테마 */}
                <CacheProvider name={iconCacheID}>           {/* 6. SVG 아이콘 캐시 */}
                  <KBarProvider actions={[]} options={...}>  {/* 7. 커맨드 팔레트 */}
                    <MaybeTimeRangeProvider>                 {/* 8. 시간 범위 */}
                      <ScopesContextProvider>                {/* 9. Scopes */}
                        <ExtensionRegistriesProvider>        {/* 10. 확장 레지스트리 */}
                          <ExtensionSidebarContextProvider>  {/* 11. 확장 사이드바 */}
                            <UNSAFE_PortalProvider>          {/* 12. React Aria 포탈 */}
                              <GlobalStyles />
                              <RouterWrapper {...props} />
                            </UNSAFE_PortalProvider>
                          </ExtensionSidebarContextProvider>
                        </ExtensionRegistriesProvider>
                      </ScopesContextProvider>
                    </MaybeTimeRangeProvider>
                  </KBarProvider>
                </CacheProvider>
              </ThemeProvider>
            </GrafanaContext.Provider>
          </OpenFeatureProvider>
        </ErrorBoundaryAlert>
      </Provider>
    );
  }
}
```

### AppWrapperState

```typescript
interface AppWrapperState {
  ready?: boolean;                    // 플러그인 레지스트리 로드 완료
  registries?: PluginExtensionRegistries; // 확장 레지스트리
}
```

`componentDidMount`에서 플러그인 확장 레지스트리를 비동기로 로드한 후에야 `ready`가 `true`가 되고,
라우트 렌더링이 시작된다. 이는 플러그인이 라우트를 오버라이드할 수 있기 때문이다.

### Enterprise 확장 포인트

```typescript
let bodyRenderHooks: ComponentType[] = [];
let pageBanners: ComponentType[] = [];
const enterpriseProviders: Array<ComponentType<{ children: ReactNode }>> = [];

export function addEnterpriseProviders(provider) { enterpriseProviders.push(provider); }
export function addBodyRenderHook(fn) { bodyRenderHooks.push(fn); }
export function addPageBanner(fn) { pageBanners.push(fn); }
```

Enterprise 에디션은 이 함수들을 통해 추가 Provider, 배너, 렌더 훅을 주입한다.
OSS 버전에서는 이 배열이 비어 있다.

---

## 4. Provider 계층 구조

12단계 Provider 각각의 역할을 상세히 분석한다.

```
┌─────────────────────────────────────────────────────────┐
│  1. Provider (Redux)                                    │
│  store: configureStore()로 생성된 Redux Store            │
│  ┌───────────────────────────────────────────────────┐   │
│  │  2. ErrorBoundaryAlert                            │   │
│  │  boundaryName="app-wrapper", style="page"         │   │
│  │  ┌─────────────────────────────────────────────┐   │   │
│  │  │  3. OpenFeatureProvider                      │   │   │
│  │  │  Feature Flag 평가 (getFeatureFlagClient())  │   │   │
│  │  │  ┌───────────────────────────────────────┐   │   │   │
│  │  │  │  4. GrafanaContext.Provider            │   │   │   │
│  │  │  │  value: { backend, location, chrome,   │   │   │   │
│  │  │  │          keybindings, config,           │   │   │   │
│  │  │  │          newAssetsChecker }             │   │   │   │
│  │  │  │  ┌─────────────────────────────────┐   │   │   │   │
│  │  │  │  │  5. ThemeProvider                │   │   │   │   │
│  │  │  │  │  value: config.theme2            │   │   │   │   │
│  │  │  │  │  (GrafanaTheme2 객체)             │   │   │   │   │
│  │  │  │  │  ┌───────────────────────────┐   │   │   │   │   │
│  │  │  │  │  │ 6~12. 나머지 Provider     │   │   │   │   │   │
│  │  │  │  │  │ (아래 상세 설명)            │   │   │   │   │   │
│  │  │  │  │  └───────────────────────────┘   │   │   │   │   │
│  │  │  │  └─────────────────────────────────┘   │   │   │   │
│  │  │  └───────────────────────────────────────┘   │   │   │
│  │  └─────────────────────────────────────────────┘   │   │
│  └───────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────┘
```

### 각 Provider 상세

| 순서 | Provider | 소스 파일 | 역할 |
|------|----------|----------|------|
| 1 | `Provider` (react-redux) | `app/store/store.ts` | Redux Store 제공 |
| 2 | `ErrorBoundaryAlert` | `@grafana/ui` | 전역 에러 캐치, 에러 페이지 표시 |
| 3 | `OpenFeatureProvider` | `@openfeature/react-sdk` | Feature Flag 평가 컨텍스트 |
| 4 | `GrafanaContext.Provider` | `app/core/context/GrafanaContext` | Grafana 핵심 서비스 접근 |
| 5 | `ThemeProvider` | `app/core/utils/ConfigProvider` | Emotion 테마 주입 |
| 6 | `CacheProvider` | `react-inlinesvg/provider` | SVG 아이콘 브라우저 캐시 |
| 7 | `KBarProvider` | `kbar` | 커맨드 팔레트 (Ctrl+K) |
| 8 | `MaybeTimeRangeProvider` | `@grafana/ui` (조건부) | 전역 시간 범위 (feature toggle) |
| 9 | `ScopesContextProvider` | `app/features/scopes` | Scope 기반 필터링 |
| 10 | `ExtensionRegistriesProvider` | `app/features/plugins/extensions` | 플러그인 확장 레지스트리 |
| 11 | `ExtensionSidebarContextProvider` | `app/core/components/AppChrome` | 확장 사이드바 상태 |
| 12 | `UNSAFE_PortalProvider` | `@react-aria/overlays` | 모달/오버레이 포탈 컨테이너 |

### GrafanaContextType 상세

```typescript
// app/core/context/GrafanaContext.ts
type GrafanaContextType = {
  backend: BackendSrv;           // HTTP 클라이언트 (API 호출)
  location: LocationService;     // 라우팅/네비게이션
  chrome: AppChromeService;      // 상단 바, 사이드 메뉴
  keybindings: KeybindingSrv;    // 키보드 단축키
  newAssetsChecker: NewFrontendAssetsChecker; // 새 에셋 체크
  config: GrafanaConfig;         // 전역 설정
};
```

---

## 5. 상태 관리: Redux Toolkit + RTK Query

### Store 설정

`public/app/store/configureStore.ts`에서 Redux Store를 생성한다.

```typescript
// configureStore.ts (검증된 코드)
export function configureStore(initialState?: Partial<StoreState>) {
  const store = reduxConfigureStore({
    reducer: createRootReducer(),
    middleware: (getDefaultMiddleware) =>
      getDefaultMiddleware({
        thunk: true,
        serializableCheck: false,   // 비직렬화 가능 값 허용
        immutableCheck: false        // 불변성 검사 비활성화
      }).concat(
        listenerMiddleware.middleware,
        alertingApi.middleware,        // RTK Query: 알림
        publicDashboardApi.middleware, // RTK Query: 공개 대시보드
        browseDashboardsAPI.middleware,// RTK Query: 대시보드 탐색
        legacyAPI.middleware,          // RTK Query: 레거시 API
        scopeAPIv0alpha1.middleware,   // RTK Query: Scopes
        ...allApiClientMiddleware,     // @grafana/api-clients
        ...extraMiddleware             // Enterprise 확장
      ),
    devTools: process.env.NODE_ENV !== 'production',
    preloadedState: { navIndex: buildInitialState(), ...initialState },
  });

  setupListeners(store.dispatch); // refetchOnFocus, refetchOnReconnect
  setStore(store);
  return store;
}
```

### 왜 serializableCheck와 immutableCheck를 비활성화하는가?

Grafana의 상태에는 `DataSourceApi` 인스턴스, `Observable`, `EventBus` 등 직렬화 불가능한 객체가 포함된다.
또한 40개 이상의 리듀서가 결합되므로, 불변성 검사를 활성화하면 성능이 크게 저하된다.
이는 성능과 유연성을 위한 의도적 선택이다.

### RTK Query API 목록

| API | 파일 | 용도 |
|-----|------|------|
| `alertingApi` | `features/alerting/unified/api/alertingApi` | 통합 알림 규칙 CRUD |
| `publicDashboardApi` | `features/dashboard/api/publicDashboardApi` | 공개 대시보드 관리 |
| `browseDashboardsAPI` | `features/browse-dashboards/api/browseDashboardsAPI` | 대시보드 탐색/검색 |
| `legacyAPI` | `@grafana/api-clients/rtkq/legacy` | 레거시 API 호환 |
| `scopeAPIv0alpha1` | `api/clients/scope/v0alpha1` | Scope 기반 필터링 |
| `allApiClientMiddleware` | `@grafana/api-clients/rtkq` | 자동 생성 API 클라이언트 |

### Root Reducer 구성

`public/app/core/reducers/root.ts`에서 40개 이상의 리듀서를 결합한다.

```typescript
// root.ts (검증된 코드)
const rootReducers = {
  ...sharedReducers,           // 공유 리듀서 (navModel 등)
  ...alertingReducers,         // 알림
  ...dashboardReducers,        // 대시보드
  ...exploreReducers,          // Explore
  ...dataSourcesReducers,      // 데이터소스
  ...usersReducers,            // 사용자 관리
  ...serviceAccountsReducer,   // 서비스 계정
  ...userReducers,             // 프로필
  ...invitesReducers,          // 초대
  ...organizationReducers,     // 조직
  ...browseDashboardsReducers, // 대시보드 탐색
  ...ldapReducers,             // LDAP
  ...importDashboardReducers,  // 대시보드 임포트
  ...panelEditorReducers,      // 패널 편집기
  ...panelsReducers,           // 패널
  ...templatingReducers,       // 템플릿 변수
  ...supportBundlesReducer,    // 지원 번들
  ...authConfigReducers,       // 인증 설정
  plugins: pluginsReducer,     // 플러그인 관리
  // RTK Query 리듀서들
  [alertingApi.reducerPath]: alertingApi.reducer,
  [publicDashboardApi.reducerPath]: publicDashboardApi.reducer,
  [browseDashboardsAPI.reducerPath]: browseDashboardsAPI.reducer,
  [scopeAPIv0alpha1.reducerPath]: scopeAPIv0alpha1.reducer,
  [legacyAPI.reducerPath]: legacyAPI.reducer,
  ...allApiClientReducers,     // 자동 생성 API 리듀서
};
```

### cleanUpAction 특수 처리

```typescript
export const createRootReducer = () => {
  const appReducer = combineReducers({ ...rootReducers, ...addedReducers });

  return (state, action) => {
    if (action.type !== cleanUpAction.type) {
      return appReducer(state, action);
    }
    // cleanUpAction은 state를 직접 변형 (mutation)
    const { cleanupAction } = action.payload;
    cleanupAction(state);
    return appReducer(state, action);
  };
};
```

`cleanUpAction`은 컴포넌트 언마운트 시 상태를 정리하는 특수 액션이다.
일반 리듀서와 달리 state를 직접 변형(mutate)하는데, 이는 Redux Toolkit의 Immer를 우회하는
설계 결정이다. 페이지 전환 시 이전 페이지의 상태를 효율적으로 정리하기 위함이다.

---

## 6. 라우팅 시스템

### RouteDescriptor 타입

```typescript
// app/core/navigation/types.ts (검증된 코드)
export interface RouteDescriptor {
  path: string;                // URL 경로 패턴
  component: GrafanaRouteComponent; // 렌더링할 컴포넌트
  roles?: () => string[];      // 접근 권한 (함수로 동적 평가)
  pageClass?: string;          // 페이지 CSS 클래스
  routeName?: string;          // 라우트 식별자
  chromeless?: boolean;        // Chrome UI 숨김 (embed 모드)
  sensitive?: boolean;         // 대소문자 구분
  allowAnonymous?: boolean | ((params) => boolean); // 익명 접근 허용
}
```

### 핵심 라우트 매핑 (routes/routes.tsx)

```typescript
// routes.tsx (검증된 코드에서 추출)
export function getAppRoutes(): RouteDescriptor[] {
  return [
    // 앱 플러그인 라우트가 최우선 (코어 라우트 오버라이드 가능)
    ...getAppPluginRoutes(),

    // 홈
    { path: '/', routeName: DashboardRoutes.Home,
      component: SafeDynamicImport(() => import('DashboardPageProxy')) },

    // 대시보드 (UID 기반)
    { path: '/d/:uid/:slug?', routeName: DashboardRoutes.Normal,
      component: SafeDynamicImport(() => import('DashboardPageProxy')) },

    // 새 대시보드
    { path: '/dashboard/new',
      roles: () => contextSrv.evaluatePermission([AccessControlAction.DashboardsCreate]),
      component: SafeDynamicImport(() => import('DashboardPageProxy')) },

    // Explore
    { path: '/explore', pageClass: 'page-explore',
      roles: () => contextSrv.evaluatePermission([AccessControlAction.DataSourcesExplore]),
      component: SafeDynamicImport(() => import('ExplorePage')) },

    // 대시보드 임포트
    { path: '/dashboard/import',
      component: SafeDynamicImport(() => import('DashboardImportPage')) },

    // 대시보드 목록
    { path: '/dashboards',
      component: SafeDynamicImport(() => import('BrowseDashboardsPage')) },

    // 알림
    ...getAlertingRoutes(),

    // 데이터 연결
    ...getDataConnectionsRoutes(),

    // 플러그인 카탈로그
    ...getPluginCatalogRoutes(),

    // 프로비저닝
    ...getProvisioningRoutes(),

    // 404 (최하단)
    { path: '/*', component: PageNotFound },
  ];
}
```

### SafeDynamicImport를 통한 코드 스플리팅

```typescript
// webpack ChunkName 어노테이션으로 번들 분리
SafeDynamicImport(
  () => import(/* webpackChunkName: "DashboardPageProxy" */
    '../features/dashboard/containers/DashboardPageProxy')
)
```

`SafeDynamicImport`는 `React.lazy`를 감싸면서 에러 경계와 로딩 상태를 추가한다.
webpack의 매직 코멘트 `webpackChunkName`으로 각 라우트가 별도 청크로 분리되어,
초기 로딩 시에는 현재 페이지에 필요한 코드만 다운로드된다.

### 왜 앱 플러그인 라우트가 최우선인가?

```typescript
// routes.tsx 주석 (원문 번역)
// Grafana 설정에 따라 독립 플러그인 페이지가 기존 코어 페이지를
// 오버라이드하거나 확장할 수 있다. 이를 가능하게 하려면
// <Switch>가 라우트를 평가하는 방식 때문에 먼저 등록해야 한다.
...getAppPluginRoutes(),
```

이 설계 덕분에 플러그인이 `/dashboards`나 `/explore` 같은 코어 경로를 완전히 대체할 수 있다.
Enterprise 에디션이나 Cloud 환경에서 커스텀 페이지를 주입할 때 활용된다.

---

## 7. 대시보드 렌더링 파이프라인

Grafana 대시보드의 렌더링은 여러 계층을 거치는 복잡한 파이프라인이다.

### 렌더링 흐름

```
사용자 요청: GET /d/:uid
│
├── DashboardPageProxy
│   └── DashboardScenePage (Scenes 기반) 또는 DashboardPage (레거시)
│
├── DashboardModel 로드
│   ├── Dashboard JSON 파싱
│   ├── PanelModel[] 생성
│   └── Variable 초기화
│
├── DashboardGrid (react-grid-layout)
│   ├── 레이아웃: 24 컬럼, 셀 높이 = 30px
│   ├── 각 PanelModel → DashboardPanel
│   └── 드래그/리사이즈 이벤트 처리
│
├── DashboardPanel
│   ├── ErrorBoundary
│   ├── PanelStateWrapper
│   │   ├── 플러그인 로드 (importPanelPlugin)
│   │   ├── QueryRunner 초기화
│   │   └── 데이터 구독
│   └── Plugin Component 렌더링
│       ├── PanelHeader (타이틀, 메뉴)
│       └── PanelComponent (시각화)
│
└── 데이터 흐름
    ├── TimeRangeUpdated (이벤트)
    ├── → Variable 평가
    ├── → QueryRunner.run() (RxJS Observable)
    ├── → DataFrames 수신
    └── → Panel 리렌더링
```

### 데이터 흐름 상세

```
┌──────────────┐    ┌───────────────┐    ┌──────────────┐
│  Time Picker  │───>│  Variables     │───>│  QueryRunner │
│  (TimeRange)  │    │  (Templates)   │    │  (RxJS)      │
└──────────────┘    └───────────────┘    └──────┬───────┘
                                                │
                    ┌───────────────────────────┘
                    │
              ┌─────▼─────┐    ┌──────────────┐    ┌──────────┐
              │ DataSource │───>│ HTTP/gRPC    │───>│ Backend  │
              │ Plugin     │    │ Request      │    │ Server   │
              └─────┬─────┘    └──────────────┘    └──────────┘
                    │
              ┌─────▼─────┐
              │ DataFrame  │  { fields: Field[], length: N }
              │ 변환/필터   │
              └─────┬─────┘
                    │
              ┌─────▼─────┐
              │ Panel      │  width, height, data, options
              │ Component  │  fieldConfig, timeRange, timeZone
              └────────────┘
```

### QueryRunner (RxJS 기반)

`features/query/state/QueryRunner.ts`는 쿼리 실행의 핵심이다.

```
QueryRunner.run(options)
│
├── 1. 쿼리 변환 (변수 치환, 시간 범위 적용)
├── 2. runRequest(datasource, request)
│   ├── Observable<PanelData> 반환
│   ├── 로딩 상태 관리
│   ├── 에러 핸들링
│   └── 캐싱 (조건부)
├── 3. 스트리밍 모드 지원 (Live)
└── 4. PanelData → Panel Component props
```

---

## 8. 패널 플러그인 SDK

### PanelPlugin 클래스

프론트엔드 패널 플러그인은 `PanelPlugin` 클래스를 사용하여 등록된다.

```typescript
// @grafana/data PanelPlugin (API)
class PanelPlugin<TOptions = any, TFieldConfig = any> {
  // 옵션 에디터 설정
  setPanelOptions(builder: (builder: PanelOptionsEditorBuilder) => void): this;

  // 필드 설정 사용
  useFieldConfig(config?: FieldConfigPropertyOverrides): this;

  // 변경 핸들러
  setPanelChangeHandler(handler: PanelChangeHandler): this;
}
```

### Panel Component Props

패널 컴포넌트가 받는 props 구조:

```typescript
interface PanelProps<T = any> {
  data: PanelData;           // 쿼리 결과
  options: T;                // 패널 옵션
  fieldConfig: FieldConfigSource; // 필드 설정
  width: number;             // 패널 너비 (px)
  height: number;            // 패널 높이 (px)
  timeRange: TimeRange;      // 현재 시간 범위
  timeZone: TimeZone;        // 타임존
  id: number;                // 패널 ID
  title: string;             // 패널 제목
  transparent: boolean;      // 투명 배경
  renderCounter: number;     // 렌더 카운터
  eventBus: EventBus;        // 이벤트 버스
  replaceVariables: InterpolateFunction; // 변수 치환 함수
  onOptionsChange: (options: T) => void; // 옵션 변경 콜백
  onFieldConfigChange: (config: FieldConfigSource) => void; // 필드 설정 변경
  onChangeTimeRange: (timeRange: AbsoluteTimeRange) => void; // 시간 범위 변경
}
```

### 내장 패널 플러그인 목록

| 플러그인 ID | 이름 | 용도 |
|------------|------|------|
| `timeseries` | Time Series | 시계열 그래프 (기본) |
| `table` | Table | 테이블 뷰 |
| `stat` | Stat | 단일 값 표시 |
| `gauge` | Gauge | 게이지 차트 |
| `piechart` | Pie Chart | 파이/도넛 차트 |
| `barchart` | Bar Chart | 막대 차트 |
| `heatmap` | Heatmap | 히트맵 |
| `histogram` | Histogram | 히스토그램 |
| `geomap` | Geomap | 지도 시각화 |
| `nodeGraph` | Node Graph | 노드/엣지 그래프 |
| `canvas` | Canvas | 커스텀 캔버스 |
| `text` | Text | 마크다운/HTML 텍스트 |
| `logs` | Logs | 로그 패널 |
| `traces` | Traces | 트레이스 뷰 |
| `flamegraph` | Flame Graph | 프로파일링 플레임 그래프 |

---

## 9. @grafana/* 패키지 생태계

`packages/` 디렉토리에는 Grafana 프론트엔드의 공개 패키지가 포함되어 있다.

### 패키지 구조

```
packages/
├── grafana-data/         # 핵심 데이터 타입, 변환
├── grafana-ui/           # UI 컴포넌트 라이브러리
├── grafana-runtime/      # 플러그인 런타임 SDK
├── grafana-schema/       # CUE 기반 스키마 생성 타입
├── grafana-e2e-selectors/# E2E 테스트 셀렉터
├── grafana-flamegraph/   # 플레임 그래프 컴포넌트
├── grafana-i18n/         # 국제화
├── grafana-prometheus/   # Prometheus 데이터소스 공통
├── grafana-sql/          # SQL 데이터소스 공통
├── grafana-alerting/     # 알림 공통 타입
├── grafana-api-clients/  # RTK Query 자동 생성 API
├── grafana-plugin-configs/# 플러그인 빌드 설정
├── grafana-openapi/      # OpenAPI 스펙 기반 타입
└── grafana-test-utils/   # 테스트 유틸리티
```

### @grafana/data 핵심 타입

```typescript
// DataFrame: Grafana의 통합 데이터 표현
interface DataFrame {
  name?: string;
  fields: Field[];     // 컬럼 배열
  length: number;      // 행 수
  refId?: string;      // 쿼리 참조 ID
  meta?: DataFrameMeta;
}

// Field: 단일 컬럼
interface Field<T = any> {
  name: string;
  type: FieldType;     // number, string, time, boolean, other
  values: T[];         // 실제 데이터 배열
  config: FieldConfig; // 표시 설정 (unit, decimals, thresholds)
  labels?: Labels;     // 라벨 (시계열 식별)
}

// EventBus: 컴포넌트 간 통신
interface EventBus {
  subscribe<T>(eventType: BusEventType<T>, handler: (event: T) => void): Unsubscribable;
  publish<T>(event: BusEvent<T>): void;
  getStream<T>(eventType: BusEventType<T>): Observable<T>;
}
```

### @grafana/ui 주요 컴포넌트

| 카테고리 | 컴포넌트 |
|----------|----------|
| 입력 | `Button`, `Input`, `Select`, `Checkbox`, `Switch`, `RadioButtonGroup` |
| 데이터 표시 | `Table`, `TimeSeries`, `Stat`, `Gauge`, `BarGauge` |
| 레이아웃 | `HorizontalGroup`, `VerticalGroup`, `Stack`, `Card`, `Drawer` |
| 피드백 | `Alert`, `ConfirmModal`, `Tooltip`, `Spinner` |
| 내비게이션 | `Tab`, `Menu`, `ContextMenu`, `Breadcrumb` |
| 시간 | `TimeRangePicker`, `DatePicker`, `RelativeTimeRangePicker` |
| 아이콘 | `Icon` (1000+ 아이콘, SVG 기반) |
| 에디터 | `CodeEditor` (Monaco), `ColorPicker`, `ThresholdsEditor` |

### @grafana/runtime 서비스

```typescript
// 플러그인에서 사용하는 런타임 서비스
getBackendSrv(): BackendSrv;        // API 호출
getLocationSrv(): LocationService;   // 라우팅
getTemplateSrv(): TemplateSrv;      // 변수 치환
getAppEvents(): EventBus;            // 전역 이벤트
config: GrafanaBootConfig;           // 부트 설정
```

---

## 10. Explore 모드

Explore는 Grafana에서 데이터를 ad-hoc으로 탐색하는 독립 모드다.
대시보드와 달리 쿼리 편집에 최적화되어 있다.

### ExploreState 구조

```typescript
// app/types/explore.ts (검증된 코드)
interface ExploreState {
  syncedTimes: boolean;               // 시간 동기화 여부
  panes: Record<string, ExploreItemState | undefined>; // 패인 맵
  correlationEditorDetails?: CorrelationEditorDetails;  // 상관관계 편집
  richHistory: RichHistoryQuery[];    // 쿼리 히스토리
  richHistorySearchFilters?: RichHistorySearchFilters;
  richHistorySettings?: RichHistorySettings;
  richHistoryStorageFull: boolean;    // 로컬 스토리지 용량 초과
  richHistoryLimitExceededWarningShown: boolean;
  largerExploreId?: string;           // 크기 조절 시 큰 쪽
  maxedExploreId?: string;            // 최대화된 패인
  evenSplitPanes?: boolean;           // 균등 분할
}
```

### ExploreItemState 구조

```typescript
// app/types/explore.ts (검증된 코드)
interface ExploreItemState {
  containerWidth: number;                // 그래프 간격 계산용 너비
  datasourceInstance?: DataSourceApi;    // 선택된 데이터소스
  eventBridge: EventBusExtended;         // 이벤트 버스
  graphResult: DataFrame[] | null;       // 그래프 결과
  tableResult: DataFrame[] | null;       // 테이블 결과
  logsResult: LogsModel | null;          // 로그 결과
  rawPrometheusResult: DataFrame | null; // Raw Prometheus 결과
  history: HistoryItem[];                // 쿼리 히스토리
  queries: DataQuery[];                  // 현재 쿼리
  range: TimeRange;                      // 시간 범위
  absoluteRange: AbsoluteTimeRange;      // 절대 시간 범위
  scanning: boolean;                     // 스캔 중 여부
  queryKeys: string[];                   // React 키
  isLive: boolean;                       // 라이브 테일링
  isPaused: boolean;                     // 테일링 일시정지
  queryResponse: ExplorePanelData;       // 전체 쿼리 응답
  showLogs?: boolean;                    // 로그 표시
  showMetrics?: boolean;                 // 메트릭 표시
  showTable?: boolean;                   // 테이블 표시
  showTrace?: boolean;                   // 트레이스 표시
  showNodeGraph?: boolean;               // 노드 그래프 표시
  showFlameGraph?: boolean;              // 플레임 그래프 표시
  compact: boolean;                      // 컴팩트 모드
  cache: Array<{ key: string; value: ExplorePanelData }>; // 쿼리 캐시 (5개)
  supplementaryQueries: SupplementaryQueries; // 보조 쿼리 (로그 볼륨 등)
  panelsState: ExplorePanelsState;       // 패널 상태
  correlations?: CorrelationData[];      // 상관관계 데이터
}
```

### ExplorePanelData 확장

```typescript
// 표준 PanelData를 확장하여 프레임 유형별 분류 제공
interface ExplorePanelData extends PanelData {
  graphFrames: DataFrame[];           // 그래프용
  tableFrames: DataFrame[];           // 테이블용
  logsFrames: DataFrame[];            // 로그용
  traceFrames: DataFrame[];           // 트레이스용
  customFrames: DataFrame[];          // 커스텀
  nodeGraphFrames: DataFrame[];       // 노드 그래프용
  rawPrometheusFrames: DataFrame[];   // Raw Prometheus
  flameGraphFrames: DataFrame[];      // 플레임 그래프용
  graphResult: DataFrame[] | null;
  tableResult: DataFrame[] | null;
  logsResult: LogsModel | null;
  rawPrometheusResult: DataFrame | null;
}
```

### Split View (멀티 패인)

```
┌─────────────────────────────────────────────────────────┐
│  Explore                                                │
├───────────────────────────┬─────────────────────────────┤
│  Pane A (Left)            │  Pane B (Right)             │
│  ┌─────────────────────┐  │  ┌─────────────────────┐    │
│  │ DataSource: Prometheus│ │  │ DataSource: Loki    │    │
│  ├─────────────────────┤  │  ├─────────────────────┤    │
│  │ Query Editor        │  │  │ Query Editor        │    │
│  │ rate(http_req[5m])  │  │  │ {job="app"} |= "err"│   │
│  ├─────────────────────┤  │  ├─────────────────────┤    │
│  │ Graph               │  │  │ Logs                │    │
│  │ Table               │  │  │ Table               │    │
│  └─────────────────────┘  │  └─────────────────────┘    │
│                           │                             │
│  syncedTimes: true ←──────┤──────→ 시간 동기화          │
└───────────────────────────┴─────────────────────────────┘
```

Explore는 최대 2개의 패인을 지원하며, `splitOpen` 액션으로 새 패인을 생성한다.
`syncedTimes`가 `true`이면 양쪽 패인의 시간 범위가 동기화된다.

---

## 11. 스타일링 시스템

### Emotion CSS-in-JS

Grafana는 Emotion 기반 CSS-in-JS를 주 스타일링 방식으로 사용한다.

```typescript
// useStyles2 패턴 (권장)
import { useStyles2 } from '@grafana/ui';
import { GrafanaTheme2 } from '@grafana/data';
import { css } from '@emotion/css';

function MyComponent() {
  const styles = useStyles2(getStyles);
  return <div className={styles.wrapper}>...</div>;
}

const getStyles = (theme: GrafanaTheme2) => ({
  wrapper: css({
    display: 'flex',
    padding: theme.spacing(2),        // 8px 기반 스페이싱
    backgroundColor: theme.colors.background.primary,
    border: `1px solid ${theme.colors.border.weak}`,
    borderRadius: theme.shape.radius.default,
    '&:hover': {
      backgroundColor: theme.colors.background.secondary,
    },
  }),
});
```

### GrafanaTheme2 구조

```typescript
interface GrafanaTheme2 {
  colors: {
    mode: 'dark' | 'light';
    primary: { main, text, border, shade };
    secondary: { main, text, border, shade };
    info, success, warning, error: { ... };
    background: { primary, secondary, canvas };
    text: { primary, secondary, disabled, link, maxContrast };
    border: { weak, medium, strong };
  };
  spacing: (amount: number) => string;  // 8px * amount
  typography: {
    fontFamily, fontFamilyMonospace;
    fontSize, body, bodySmall;
    h1, h2, h3, h4, h5, h6;
  };
  shape: {
    radius: { default, pill, circle };
  };
  breakpoints: { values, keys, up, down, between };
  shadows: { z1, z2, z3 };
  transitions: { duration, create };
  zIndex: { navbarFixed, sidemenu, dropdown, tooltip, modal, modalBackdrop };
}
```

### 왜 Emotion인가?

1. 테마 시스템과의 통합: `useStyles2`가 자동으로 현재 테마를 주입
2. 타입 안전성: TypeScript로 스타일 속성 검증
3. 런타임 성능: Emotion의 CSS 클래스 생성은 매우 효율적
4. SSR 호환: 서버 사이드 렌더링 시 스타일 추출 가능
5. 점진적 마이그레이션: 기존 Sass와 공존 가능

---

## 12. 빌드 도구 체인

### Webpack 5 설정

Grafana는 Webpack 5를 메인 번들러로 사용한다.

```
빌드 파이프라인
│
├── TypeScript/TSX
│   └── esbuild-loader (트랜스파일)    # tsc 대신 esbuild 사용 (빠름)
│
├── Sass (.scss)
│   ├── sass-loader → css-loader → style-loader
│   └── 레거시 스타일 (점진적 Emotion 마이그레이션 중)
│
├── Assets
│   ├── SVG → @svgr/webpack (React 컴포넌트 변환)
│   ├── Images → asset/resource
│   └── Fonts → asset/resource
│
├── 코드 스플리팅
│   ├── vendor chunk (node_modules)
│   ├── route-based chunks (SafeDynamicImport)
│   └── moment-timezone chunk (대용량)
│
└── 최적화
    ├── esbuild minifier
    ├── tree shaking
    └── source maps
```

### 테스트 도구

| 도구 | 용도 |
|------|------|
| Jest | 단위 테스트, 스냅샷 테스트 |
| React Testing Library | 컴포넌트 테스트 |
| Playwright | E2E 테스트 (`e2e-playwright/`) |
| Cypress | 레거시 E2E 테스트 (`cypress.config.js`) |

### ESLint 설정

```
eslint.config.js
├── @grafana/eslint-rules (커스텀 규칙)
├── eslint-suppressions.json (기존 경고 억제)
└── 핵심 규칙
    ├── no-restricted-imports (순환 의존성 방지)
    ├── react-hooks/exhaustive-deps
    └── @typescript-eslint/strict
```

---

## 요약

Grafana 프론트엔드는 다음과 같은 핵심 설계 결정을 기반으로 구축되어 있다.

| 영역 | 선택 | 이유 |
|------|------|------|
| 상태 관리 | Redux Toolkit + RTK Query | 대규모 앱의 예측 가능한 상태 관리, API 캐싱 |
| 라우팅 | React Router v5 (v6 compat) | 점진적 마이그레이션, 플러그인 호환성 |
| 스타일 | Emotion CSS-in-JS | 테마 통합, 타입 안전성, Sass에서 점진적 전환 |
| 빌드 | Webpack 5 + esbuild | 빠른 트랜스파일, 성숙한 플러그인 생태계 |
| 데이터 흐름 | RxJS Observable | 스트리밍, 취소, 복합 쿼리 파이프라인 |
| 컴포넌트 | @grafana/ui 패키지 | 플러그인과 코어 간 일관된 UI |
| 확장성 | Provider 중첩 + 플러그인 확장 점 | Enterprise/Cloud 기능 주입 |
| 코드 분할 | SafeDynamicImport + webpack chunks | 초기 로딩 시간 최소화 |

### 아키텍처 다이어그램

```
┌─────────────────────────────────────────────────────────────┐
│                    Browser (React DOM)                       │
├─────────────────────────────────────────────────────────────┤
│                                                             │
│  ┌─────────┐  ┌──────────┐  ┌──────────┐  ┌─────────────┐  │
│  │ Provider │  │ Routes   │  │ Features │  │ @grafana/*  │  │
│  │ Stack    │  │ System   │  │ (52)     │  │ Packages    │  │
│  │ (12)     │  │          │  │          │  │             │  │
│  └────┬─────┘  └────┬─────┘  └────┬─────┘  └──────┬──────┘  │
│       │             │             │               │          │
│  ┌────▼─────────────▼─────────────▼───────────────▼──────┐  │
│  │                Redux Store (40+ reducers)              │  │
│  │                RTK Query APIs (6+)                     │  │
│  └───────────────────────┬───────────────────────────────┘  │
│                          │                                   │
│  ┌───────────────────────▼───────────────────────────────┐  │
│  │              BackendSrv (HTTP Client)                  │  │
│  │              → API Proxy → Grafana Server              │  │
│  └───────────────────────────────────────────────────────┘  │
│                                                             │
├─────────────────────────────────────────────────────────────┤
│  Webpack 5 + esbuild │ Jest + RTL │ Playwright │ ESLint    │
└─────────────────────────────────────────────────────────────┘
```

이 아키텍처는 Grafana가 단순한 대시보드 도구를 넘어, 플러그인 확장 가능한 관찰 가능성 플랫폼으로
진화해온 결과를 반영한다. 12단계 Provider 중첩은 복잡해 보이지만, 각 Provider가 명확한 책임을 가지며
Enterprise 확장, 플러그인 시스템, 테마, 상태 관리를 깔끔하게 분리한다.
