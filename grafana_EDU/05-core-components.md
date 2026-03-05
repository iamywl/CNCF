# Grafana 핵심 컴포넌트

## 개요

Grafana의 백엔드는 여러 핵심 컴포넌트의 협력으로 동작한다. 이 문서에서는 HTTP 서버, 서비스 레이어, 플러그인 매니저, SQLStore, 미들웨어 체인, 설정 시스템의 내부 동작 원리를 소스 코드 기반으로 상세히 분석한다.

---

## 1. HTTPServer

### 1.1 구조체 정의

`HTTPServer`는 Grafana의 HTTP API 서버를 담당하는 핵심 구조체이다. `pkg/api/http_server.go`에 정의되어 있으며, 70개 이상의 의존성을 주입받는다.

```go
// pkg/api/http_server.go
type HTTPServer struct {
    log              log.Logger
    web              *web.Mux
    context          context.Context
    httpSrv          *http.Server
    middlewares      []web.Handler
    namedMiddlewares []routing.RegisterNamedMiddleware
    bus              bus.Bus

    // 주요 서비스 의존성 (70+ 필드)
    pluginContextProvider        *plugincontext.Provider
    RouteRegister                routing.RouteRegister
    RenderService                rendering.Service
    Cfg                          *setting.Cfg
    Features                     featuremgmt.FeatureToggles
    SettingsProvider             setting.Provider
    HooksService                 *hooks.HooksService
    navTreeService               navtree.Service
    CacheService                 *localcache.CacheService
    DataSourceCache              datasources.CacheService
    AuthTokenService             auth.UserTokenService
    QuotaService                 quota.Service
    RemoteCacheService           *remotecache.RemoteCache
    ProvisioningService          provisioning.ProvisioningService
    License                      licensing.Licensing
    AccessControl                accesscontrol.AccessControl
    DataProxy                    *datasourceproxy.DataSourceProxyService
    pluginClient                 plugins.Client
    pluginStore                  pluginstore.Store
    pluginInstaller              plugins.Installer
    SearchService                search.Service
    ContextHandler               *contexthandler.ContextHandler
    LoggerMiddleware             loggermw.Logger
    SQLStore                     db.DB
    AlertNG                      *ngalert.AlertNG
    SocialService                social.Service
    EncryptionService            encryption.Internal
    SecretsService               secrets.Service
    DataSourcesService           datasources.DataSourceService
    DashboardService             dashboards.DashboardService
    folderService                folder.Service
    authnService                 authn.Service
    userService                  user.Service
    orgService                   org.Service
    TeamService                  team.Service
    accesscontrolService         accesscontrol.Service
    // ... 기타 다수
}
```

### 1.2 ProvideHTTPServer() - 생성 함수

`ProvideHTTPServer()`는 Wire DI에 의해 호출되는 팩토리 함수이다. 70개 이상의 매개변수를 받아 `HTTPServer` 인스턴스를 생성한다:

```go
// pkg/api/http_server.go (line 243)
func ProvideHTTPServer(opts ServerOptions, cfg *setting.Cfg,
    routeRegister routing.RouteRegister, bus bus.Bus,
    renderService rendering.Service, licensing licensing.Licensing,
    hooksService *hooks.HooksService,
    cacheService *localcache.CacheService, sqlStore db.DB,
    // ... 60+ 추가 매개변수
) (*HTTPServer, error) {
    web.Env = cfg.Env
    m := web.New()

    hs := &HTTPServer{
        Cfg:                cfg,
        RouteRegister:      routeRegister,
        bus:                bus,
        RenderService:      renderService,
        License:            licensing,
        // ... 모든 필드 초기화
        web:                m,
        log:                log.New("http.server"),
    }

    // Prometheus 메트릭 등록
    promRegister.MustRegister(hs.htmlHandlerRequestsDuration)
    promRegister.MustRegister(hs.dsConfigHandlerRequestsDuration)

    // API 라우트 등록
    hs.registerRoutes()

    // 접근 제어 스코프 리졸버 등록
    hs.AccessControl.RegisterScopeAttributeResolver(...)

    // 고정 역할 선언
    if err := hs.declareFixedRoles(); err != nil {
        return nil, err
    }

    return hs, nil
}
```

### 1.3 Run() - 서버 시작

`Run()` 메서드는 HTTP 서버를 시작하고 요청을 처리한다:

```go
// pkg/api/http_server.go (line 441)
func (hs *HTTPServer) Run(ctx context.Context) error {
    hs.context = ctx

    // 1. 미들웨어 및 라우트 적용
    hs.applyRoutes()

    // 2. HTTP 서버 생성
    host := strings.TrimSuffix(strings.TrimPrefix(hs.Cfg.HTTPAddr, "["), "]")
    hs.httpSrv = &http.Server{
        Addr:        net.JoinHostPort(host, hs.Cfg.HTTPPort),
        Handler:     hs.web,
        ReadTimeout: hs.Cfg.ReadTimeout,
    }

    // 3. TLS 설정 (프로토콜에 따라)
    switch hs.Cfg.Protocol {
    case setting.HTTP2Scheme, setting.HTTPSScheme, setting.SocketHTTP2Scheme:
        if err := hs.configureTLS(); err != nil {
            return err
        }
        // 인증서 자동 갱신 감시
        if hs.Cfg.CertWatchInterval > 0 {
            hs.httpSrv.TLSConfig.GetCertificate = hs.GetCertificate
            go hs.WatchAndUpdateCerts(ctx)
        }
    }

    // 4. 리스너 획득 및 서버 시작
    listeners, err := hs.getListeners()
    // ... 에러 처리

    // 5. 컨텍스트 종료 시 graceful shutdown
    go func() {
        <-ctx.Done()
        hs.httpSrv.Shutdown(context.Background())
    }()

    // 6. 리스너별 서빙 시작
    // ...
}
```

### 1.4 TLS 지원

Grafana는 HTTP, HTTPS, H2, Socket, SocketH2 프로토콜을 지원한다:

```go
// pkg/setting/setting.go
type Scheme string

const (
    HTTPScheme        Scheme = "http"
    HTTPSScheme       Scheme = "https"
    HTTP2Scheme       Scheme = "h2"
    SocketScheme      Scheme = "socket"
    SocketHTTP2Scheme Scheme = "socket_h2"
)
```

TLS 인증서의 자동 갱신(`CertWatchInterval`)도 지원하며, `WatchAndUpdateCerts()` 고루틴이 주기적으로 인증서 파일 변경을 감시한다.

```go
// pkg/api/http_server.go
type TLSCerts struct {
    certLock  sync.RWMutex
    certMtime time.Time
    keyMtime  time.Time
    certs     *tls.Certificate
}
```

### 1.5 Graceful Shutdown

서버 종료 시 `context.Done()` 채널을 감시하여 graceful shutdown을 수행한다. `errgroup`을 사용하여 여러 리스너의 종료를 병렬로 처리한다.

---

## 2. 서비스 레이어

### 2.1 Interface + Implementation + Provider 패턴

Grafana의 모든 서비스는 일관된 3단 구조를 따른다:

```
1. 인터페이스 정의   → pkg/services/{name}/{name}.go
2. 구현체           → pkg/services/{name}/{name}impl/{name}.go
3. Provider 함수    → Provide{Name}() 팩토리 함수 (Wire DI에 연결)
```

이 패턴의 장점:
- **테스트 용이성**: 인터페이스 기반 모킹이 가능
- **의존성 역전**: 상위 모듈이 하위 구현에 의존하지 않음
- **확장성**: OSS/Enterprise 빌드에서 다른 구현체 주입 가능

### 2.2 Command/Query 패턴

서비스 레이어는 읽기(Query)와 쓰기(Command)를 명확하게 분리한다:

```go
// 쓰기 작업 - Command
type SaveDashboardCommand struct {
    Dashboard *models.Dashboard
    FolderID  int64
    Overwrite bool
    Message   string
    OrgID     int64
    UserID    int64
}

// 읽기 작업 - Query
type GetDashboardQuery struct {
    ID    int64
    UID   string
    OrgID int64
}
```

Command는 상태를 변경하고, Query는 데이터를 조회한다. 모든 작업은 `context.Context`를 첫 번째 인자로 받아 취소, 타임아웃, 트레이싱을 지원한다.

### 2.3 주요 서비스 목록

| 서비스 | 패키지 | 역할 |
|--------|--------|------|
| DashboardService | `pkg/services/dashboards/` | 대시보드 CRUD, 버전 관리 |
| DataSourceService | `pkg/services/datasources/` | 데이터소스 등록, 설정, 캐싱 |
| UserService | `pkg/services/user/` | 사용자 관리, 프로필 |
| AuthService | `pkg/services/auth/` | 토큰 기반 인증, JWT |
| AuthnService | `pkg/services/authn/` | 통합 인증 (OAuth, LDAP, SAML 등) |
| AccessControl | `pkg/services/accesscontrol/` | RBAC 접근 제어 |
| SecretsService | `pkg/services/secrets/` | 비밀 키 암호화/복호화 |
| FolderService | `pkg/services/folder/` | 폴더 계층 관리 |
| FeatureManager | `pkg/services/featuremgmt/` | 피처 플래그 토글 |
| AlertNG | `pkg/services/ngalert/` | 차세대 알림 시스템 |
| ProvisioningService | `pkg/services/provisioning/` | 선언적 리소스 프로비저닝 |
| QueryService | `pkg/services/query/` | 쿼리 실행, 데이터소스 프록시 |
| LiveService | `pkg/services/live/` | WebSocket 기반 실시간 스트리밍 |
| EncryptionService | `pkg/services/encryption/` | 데이터 암호화 (Envelope Encryption) |
| RenderingService | `pkg/services/rendering/` | 패널/대시보드 이미지 렌더링 |

### 2.4 서비스 생명주기

서비스는 `registry.BackgroundService` 인터페이스를 구현하여 백그라운드 서비스로 등록될 수 있다:

```go
type BackgroundService interface {
    Run(ctx context.Context) error
}
```

서버 시작 시 `registry.BackgroundServiceRegistry`에 등록된 모든 백그라운드 서비스가 병렬로 시작된다. 서버 종료 시 컨텍스트 취소를 통해 모든 서비스에 종료 신호가 전달된다.

---

## 3. 플러그인 매니저

### 3.1 플러그인 라이프사이클 파이프라인

플러그인 매니저는 5단계 파이프라인으로 플러그인의 전체 라이프사이클을 관리한다:

```
Discovery → Bootstrap → Initialization → Validation → Termination
```

각 단계는 `pkg/plugins/manager/pipeline/` 아래에 독립 패키지로 구현되어 있다:

```
pkg/plugins/manager/pipeline/
├── discovery/          # 플러그인 발견
├── bootstrap/          # 플러그인 구성
├── initialization/     # 플러그인 초기화
├── validation/         # 플러그인 검증
└── termination/        # 플러그인 종료
```

### 3.2 Discovery 단계

플러그인 소스를 탐색하여 플러그인 번들을 발견한다:

```go
// pkg/plugins/manager/pipeline/discovery/discovery.go
type Discoverer interface {
    Discover(ctx context.Context, src plugins.PluginSource) ([]*plugins.FoundBundle, error)
}

type Discovery struct {
    filterSteps []FilterFunc
    log         log.Logger
    tracer      trace.Tracer
}

// FilterFunc - 발견된 플러그인 필터링
type FilterFunc func(ctx context.Context, class plugins.Class,
    bundles []*plugins.FoundBundle) ([]*plugins.FoundBundle, error)
```

Discovery 단계의 흐름:

```
플러그인 소스 (디스크, CDN 등)
    ↓
src.Discover() - 소스별 플러그인 발견
    ↓
FilterFunc[] - 필터 체인 적용
    ↓
[]*FoundBundle - 발견된 플러그인 번들
```

### 3.3 Bootstrap 단계

발견된 플러그인 번들을 초기 Plugin 구조체로 변환하고 메타데이터를 장식한다:

```go
// pkg/plugins/manager/pipeline/bootstrap/bootstrap.go
type Bootstrapper interface {
    Bootstrap(ctx context.Context, src plugins.PluginSource,
        bundle *plugins.FoundBundle) ([]*plugins.Plugin, error)
}

type Bootstrap struct {
    constructStep ConstructFunc
    decorateSteps []DecorateFunc
    log           log.Logger
    tracer        trace.Tracer
}

// ConstructFunc - 플러그인 구조체 생성
type ConstructFunc func(ctx context.Context, src plugins.PluginSource,
    bundle *plugins.FoundBundle) ([]*plugins.Plugin, error)

// DecorateFunc - 플러그인 메타데이터 장식
type DecorateFunc func(ctx context.Context, p *plugins.Plugin) (*plugins.Plugin, error)
```

Bootstrap 단계는 두 하위 단계로 구성된다:

```
FoundBundle
    ↓
ConstructFunc - Plugin 구조체 생성 (서명 계산 포함)
    ↓
DecorateFunc[] - 메타데이터 장식 (CDN 경로, 로고 등)
    ↓
[]*Plugin - 구성된 플러그인
```

`DefaultConstructFunc`는 서명 계산(`signature.DefaultCalculator`)과 로컬 에셋 프로바이더(`pluginassets.NewLocalProvider`)를 사용한다.

### 3.4 Initialization 단계

플러그인을 초기화하고 필요한 리소스를 준비한다:

```go
// pkg/plugins/manager/pipeline/initialization/initialization.go
type Initializer interface {
    Initialize(ctx context.Context, ps *plugins.Plugin) (*plugins.Plugin, error)
}

type Initialize struct {
    cfg             *config.PluginManagementCfg
    initializeSteps []InitializeFunc
    log             log.Logger
    tracer          trace.Tracer
}

type InitializeFunc func(ctx context.Context, p *plugins.Plugin) (*plugins.Plugin, error)
```

### 3.5 Validation 및 Termination 단계

```
Validation:
    플러그인 유효성 검증 (서명, 호환성, 의존성)

Termination:
    플러그인 프로세스 종료 및 리소스 해제
```

### 3.6 플러그인 인터페이스

플러그인 시스템의 핵심 인터페이스:

```go
// pkg/plugins/ifaces.go

// Installer - 플러그인 설치/제거
type Installer interface {
    Add(ctx context.Context, pluginID, version string, opts AddOpts) error
    Remove(ctx context.Context, pluginID, version string) error
}

// PluginSource - 플러그인 소스
type PluginSource interface {
    PluginClass(ctx context.Context) Class
    DefaultSignature(ctx context.Context, pluginID string) (Signature, bool)
    Discover(ctx context.Context) ([]*FoundBundle, error)
}

// FileStore - 플러그인 파일 저장소
type FileStore interface {
    File(ctx context.Context, pluginID, pluginVersion, filename string) (*File, error)
}

// FS - 플러그인 파일 시스템
type FS interface {
    fs.FS
    Type() FSType
    Base() string
    Files() ([]string, error)
    Rel(string) (string, error)
}
```

### 3.7 gRPC 플러그인 프로토콜

Grafana의 백엔드 플러그인은 gRPC를 통해 통신한다. `grafana-plugin-sdk-go`가 플러그인 SDK를 제공한다:

```
Grafana ←→ gRPC ←→ Plugin Process
    │                    │
    ├─ DataQuery         ├─ QueryData()
    ├─ Resource          ├─ CallResource()
    ├─ Diagnostics       ├─ CheckHealth()
    ├─ Stream            ├─ RunStream()
    └─ Admission         └─ MutateAdmission()
```

플러그인 프로세스는 별도의 OS 프로세스로 실행되며, go-plugin 프레임워크를 기반으로 핸드셰이크, 프로세스 관리, 로그 전달이 이루어진다.

---

## 4. SQLStore

### 4.1 구조체 정의

`SQLStore`는 Grafana의 데이터베이스 추상화 계층이다. XORM(커스텀 포크)을 ORM으로 사용한다:

```go
// pkg/services/sqlstore/sqlstore.go
type SQLStore struct {
    cfg         *setting.Cfg
    features    featuremgmt.FeatureToggles
    sqlxsession *session.SessionDB

    bus                          bus.Bus
    dbCfg                        *DatabaseConfig
    engine                       *xorm.Engine
    log                          log.Logger
    dialect                      migrator.Dialect
    migrations                   registry.DatabaseMigrator
    tracer                       tracing.Tracer
    recursiveQueriesAreSupported *bool
    recursiveQueriesMu           sync.Mutex
}
```

### 4.2 Provider 함수

```go
// pkg/services/sqlstore/sqlstore.go
func ProvideService(cfg *setting.Cfg,
    features featuremgmt.FeatureToggles,
    migrations registry.DatabaseMigrator,
    bus bus.Bus, tracer tracing.Tracer,
) (*SQLStore, error) {
    xorm.DefaultPostgresSchema = ""
    s, err := newStore(cfg, nil, features, migrations, bus, tracer)
    if err != nil {
        return nil, err
    }

    // 마이그레이션 실행
    if err := s.Migrate(s.dbCfg.MigrationLock); err != nil {
        return nil, err
    }

    // 기본 조직/관리자 사용자 생성
    if err := s.Reset(); err != nil {
        return nil, err
    }

    return s, nil
}
```

초기화 순서:

```
1. newStore()          - 엔진 초기화, DB 연결 설정
2. initEngine()        - XORM 엔진 생성
3. Migrate()           - DB 마이그레이션 실행
4. Reset()             - 기본 조직/관리자 사용자 보장
```

### 4.3 다중 DB 지원

| DB | 드라이버 | 기본 포트 | 용도 |
|----|---------|----------|------|
| SQLite3 | `sqlite3` | - | 기본값, 개발/단일 인스턴스 |
| PostgreSQL | `lib/pq` | 5432 | 프로덕션 권장 |
| MySQL | `go-sql-driver/mysql` | 3306 | 프로덕션 대안 |

```go
// conf/defaults.ini
// [database]
// type = sqlite3
// host = 127.0.0.1:3306
// name = grafana
```

### 4.4 마이그레이션 시스템

마이그레이션은 `pkg/services/sqlstore/migrator/` 패키지가 관리한다:

```go
// pkg/services/sqlstore/migrator/migrator.go
type Migrator struct {
    DBEngine     *xorm.Engine
    Dialect      Dialect
    migrations   []Migration
    migrationIds map[string]struct{}
    Logger       log.Logger
    Cfg          *setting.Cfg
    isLocked     atomic.Bool
    logMap       map[string]MigrationLog
    tableName    string
    metrics      migratorMetrics
}
```

마이그레이션 로그 테이블:

```go
type MigrationLog struct {
    Id          int64
    MigrationID string `xorm:"migration_id"`
    SQL         string `xorm:"sql"`
    Success     bool
    Error       string
    Timestamp   time.Time
}
```

마이그레이션 파일들은 `pkg/services/sqlstore/migrations/` 아래에 도메인별로 분리되어 있다:

```
migrations/
├── accesscontrol/      # 접근 제어 마이그레이션
├── alert_mig.go        # 알림 테이블
├── annotation_mig.go   # 어노테이션 테이블
├── apikey_mig.go        # API 키 테이블
├── dashboard_mig.go     # 대시보드 테이블
├── dashboard_acl.go     # 대시보드 ACL
├── datasource_mig.go    # 데이터소스 테이블
├── folder_mig.go        # 폴더 테이블
├── dashboard_version_mig.go  # 대시보드 버전 테이블
├── dashboard_snapshot_mig.go # 스냅샷 테이블
├── correlations_mig.go       # 상관관계 테이블
└── common.go           # 공통 마이그레이션 유틸
```

마이그레이션 실행 흐름:

```
서버 시작
    ↓
ProvideService() → s.Migrate(migrationLock)
    ↓
NewMigrator(engine, cfg)
    ↓
migrations.AddMigration(migrator) ← 모든 서비스가 마이그레이션 등록
    ↓
migrator.RunMigrations(ctx, lockEnabled, lockTimeout)
    ↓
각 마이그레이션 실행 및 MigrationLog 기록
```

### 4.5 세션 관리

데이터베이스 세션은 `context.Context`를 통해 전달되며, 트랜잭션을 지원한다:

```go
// ContextSessionKey - 컨텍스트에 세션 저장용 키
type ContextSessionKey struct{}
```

### 4.6 SQL Dialect

각 DB 종류에 맞는 SQL 방언을 추상화한다:

```go
// NewDialect 함수로 드라이버 이름에 따라 적절한 Dialect 반환
ss.dialect = migrator.NewDialect(ss.engine.DriverName())
```

---

## 5. 미들웨어 체인

### 5.1 미들웨어 등록 순서

`addMiddlewaresAndStaticRoutes()` 메서드에서 미들웨어가 순서대로 등록된다. 이 순서는 요청 처리 흐름을 결정한다:

```go
// pkg/api/http_server.go (line 668)
func (hs *HTTPServer) addMiddlewaresAndStaticRoutes() {
    m := hs.web

    // 1. 요청 메타데이터 설정
    m.Use(requestmeta.SetupRequestMetadata())

    // 2. 요청 트레이싱 (OpenTelemetry)
    m.Use(middleware.RequestTracing(hs.tracer, middleware.ShouldTraceWithExceptions))

    // 3. 요청 메트릭 수집 (Prometheus)
    m.Use(middleware.RequestMetrics(hs.Features, hs.Cfg, hs.promRegister))

    // 4. 로거 미들웨어
    m.UseMiddleware(hs.LoggerMiddleware.Middleware())

    // 5. Gzip 압축 (설정 활성화 시)
    if hs.Cfg.EnableGzip {
        m.UseMiddleware(middleware.Gziper())
    }

    // 6. 패닉 복구
    m.UseMiddleware(middleware.Recovery(hs.Cfg, hs.License))

    // 7. CSRF 보호
    m.UseMiddleware(hs.Csrf.Middleware())

    // 8. 정적 파일 서빙
    hs.mapStatic(m, hs.Cfg.StaticRootPath, "build", "public/build")
    hs.mapStatic(m, hs.Cfg.StaticRootPath, "", "public", "/public/views/swagger.html")
    hs.mapStatic(m, hs.Cfg.StaticRootPath, "robots.txt", "robots.txt")

    // 9. 커스텀 응답 헤더
    if len(hs.Cfg.CustomResponseHeaders) > 0 {
        m.Use(middleware.AddCustomResponseHeaders(hs.Cfg))
    }

    // 10. 기본 응답 헤더 (보안 헤더 포함)
    m.Use(middleware.AddDefaultResponseHeaders(hs.Cfg))

    // 11. 서브패스 리다이렉트
    if hs.Cfg.ServeFromSubPath && hs.Cfg.AppSubURL != "" {
        m.SetURLPrefix(hs.Cfg.AppSubURL)
        m.UseMiddleware(middleware.SubPathRedirect(hs.Cfg))
    }

    // 12. 템플릿 렌더러
    m.UseMiddleware(web.Renderer(...))

    // 13. 헬스체크/메트릭 엔드포인트
    m.Use(hs.healthzHandler)
    m.Use(hs.apiHealthHandler)
    m.Use(hs.metricsEndpoint)
    m.Use(hs.pluginMetricsEndpoint)
    m.Use(hs.frontendLogEndpoints())

    // 14. 컨텍스트 핸들러 (인증 처리)
    m.UseMiddleware(hs.ContextHandler.Middleware)

    // 15. 조직 리다이렉트
    m.Use(middleware.OrgRedirect(hs.Cfg, hs.userService))

    // 16. 도메인 검증
    if hs.Cfg.EnforceDomain {
        m.Use(middleware.ValidateHostHeader(hs.Cfg))
    }

    // 17. 액션 URL 검증
    m.UseMiddleware(middleware.ValidateActionUrl(hs.Cfg, hs.log))

    // 18. 캐시 제어 헤더
    m.Use(middleware.HandleNoCacheHeaders)

    // 19. Content Security Policy
    if hs.Cfg.CSPEnabled || hs.Cfg.CSPReportOnlyEnabled {
        m.UseMiddleware(middleware.ContentSecurityPolicy(hs.Cfg, hs.log))
    }

    // 20. 추가 미들웨어 (외부 등록)
    for _, mw := range hs.middlewares {
        m.Use(mw)
    }
}
```

### 5.2 미들웨어 실행 흐름

```
요청 수신
    ↓
[1] SetupRequestMetadata  ← 요청 메타데이터 초기화
    ↓
[2] RequestTracing        ← OpenTelemetry 스팬 시작
    ↓
[3] RequestMetrics        ← Prometheus 메트릭 기록 시작
    ↓
[4] Logger                ← 요청 로깅
    ↓
[5] Gziper                ← 응답 압축 (선택)
    ↓
[6] Recovery              ← 패닉 복구
    ↓
[7] CSRF                  ← CSRF 토큰 검증
    ↓
[8] Static Files          ← 정적 파일 매칭 시 바로 응답
    ↓
[9-10] Response Headers    ← 보안 헤더 추가
    ↓
[11] SubPath Redirect      ← 서브패스 처리
    ↓
[12] Renderer              ← 템플릿 엔진
    ↓
[13] Health/Metrics        ← /healthz, /metrics 엔드포인트
    ↓
[14] ContextHandler        ← 인증 처리, 사용자 컨텍스트 설정
    ↓
[15] OrgRedirect           ← 조직별 리다이렉트
    ↓
[16-19] Validation/CSP     ← 보안 검증
    ↓
API 핸들러 실행
```

### 5.3 라우트 적용 (applyRoutes)

```go
// pkg/api/http_server.go (line 659)
func (hs *HTTPServer) applyRoutes() {
    // 1. 미들웨어 및 정적 라우트 등록
    hs.addMiddlewaresAndStaticRoutes()

    // 2. 뷰 라우트 및 API 라우트 등록
    hs.RouteRegister.Register(hs.web, hs.namedMiddlewares...)

    // 3. 404 핸들러
    hs.web.NotFound(
        middleware.ProvideRouteOperationName("notfound"),
        middleware.ReqSignedIn,
        hs.NotFoundHandler,
    )
}
```

### 5.4 인증 미들웨어 상세

`pkg/middleware/auth.go`에 정의된 인증 관련 미들웨어:

```go
// ReqSignedIn - 로그인 필수
func ReqSignedIn(c *web.Context) { ... }

// ReqGrafanaAdmin - Grafana 관리자 권한 필수
func ReqGrafanaAdmin(c *web.Context) { ... }

// ReqOrgAdmin - 조직 관리자 권한 필수
func ReqOrgAdmin(c *web.Context) { ... }

// ReqEditorRole - 편집자 이상 권한 필수
func ReqEditorRole(c *web.Context) { ... }
```

---

## 6. 설정 시스템

### 6.1 Cfg 구조체

`setting.Cfg`는 Grafana의 모든 설정값을 담는 중앙 구조체이다. 200개 이상의 필드를 포함한다:

```go
// pkg/setting/setting.go
type Cfg struct {
    Target    []string
    Raw       *ini.File
    Logger    log.Logger

    // 설정 파일 추적
    configFiles                  []string
    appliedCommandLineProperties []string
    appliedEnvOverrides          []string

    // HTTP Server
    CertFile          string
    KeyFile           string
    CertPassword      string
    CertWatchInterval time.Duration
    HTTPAddr          string
    HTTPPort          string
    Env               string
    AppURL            string
    AppSubURL         string
    Protocol          Scheme
    Domain            string
    ReadTimeout       time.Duration
    EnableGzip        bool
    EnforceDomain     bool
    MinTLSVersion     string

    // Paths
    HomePath         string
    ProvisioningPath string
    DataPath         string
    LogsPath         string
    PluginsPaths     []string

    // Security
    SecretKey                    string
    CSPEnabled                   bool
    CSPReportOnlyEnabled         bool
    CookieSecure                 bool
    AllowEmbedding               bool
    StrictTransportSecurity      bool
    XSSProtectionHeader          bool
    ContentTypeProtectionHeader  bool

    // Build
    BuildVersion string
    BuildCommit  string
    IsEnterprise bool

    // Rendering
    RendererServerUrl              string
    RendererConcurrentRequestLimit int
    ImagesDir                      string

    // SMTP
    Smtp SmtpSettings

    // ... 200+ 추가 필드
}
```

### 6.2 설정 로딩 체인

`loadConfiguration()` 메서드가 설정을 단계적으로 로딩한다:

```go
// pkg/setting/setting.go (line 1027)
func (cfg *Cfg) loadConfiguration(args CommandLineArgs) (*ini.File, error) {
    // 1단계: 기본 설정 로드
    defaultConfigFile := path.Join(cfg.HomePath, "conf/defaults.ini")
    parsedFile, err := ini.Load(defaultConfigFile)

    // 2단계: 명령행 기본 속성 적용
    commandLineProps := cfg.getCommandLineProperties(args.Args)
    cfg.applyCommandLineDefaultProperties(commandLineProps, parsedFile)

    // 3단계: 사용자 지정 설정 파일 로드 (grafana.ini / custom.ini)
    err = cfg.loadSpecifiedConfigFile(args.Config, parsedFile)

    // 4단계: 환경 변수 오버라이드 적용
    err = cfg.applyEnvVariableOverrides(parsedFile)

    // 5단계: 명령행 속성 오버라이드 적용
    cfg.applyCommandLineProperties(commandLineProps, parsedFile)

    // 6단계: 환경 변수 포함 값 확장
    err = expandConfig(parsedFile)

    return parsedFile, err
}
```

로딩 우선순위 (후순위가 우선):

```
conf/defaults.ini              → 기본값 (최저 우선순위)
    ↓
conf/grafana.ini               → 사용자 설정
    ↓
conf/custom.ini                → 커스텀 오버라이드
    ↓
GF_{SECTION}_{KEY} 환경변수     → 환경 변수
    ↓
CLI 플래그                      → 명령행 인자 (최고 우선순위)
```

### 6.3 환경 변수 오버라이드

환경 변수는 `GF_{SECTION}_{KEY}` 패턴을 따른다:

```bash
# [server] 섹션의 http_port 설정
GF_SERVER_HTTP_PORT=8080

# [database] 섹션의 type 설정
GF_DATABASE_TYPE=postgres

# [security] 섹션의 admin_user 설정
GF_SECURITY_ADMIN_USER=myadmin

# [auth.github] 섹션의 enabled 설정
GF_AUTH_GITHUB_ENABLED=true
```

### 6.4 INI 파일 파싱

`gopkg.in/ini.v1` 라이브러리를 사용하여 INI 파일을 파싱한다:

```go
import "gopkg.in/ini.v1"

// 파일 로드
parsedFile, err := ini.Load(defaultConfigFile)

// 섹션 접근
section := parsedFile.Section("database")

// 키 값 읽기
dbType := section.Key("type").String()
```

### 6.5 NewCfg 변형들

```go
// 기본 생성자
func NewCfg() *Cfg

// 피처 플래그 포함 생성자
func NewCfgWithFeatures(features func(string) bool) *Cfg

// CLI 인자로부터 생성
func NewCfgFromArgs(args CommandLineArgs) (*Cfg, error)

// 바이트 슬라이스로부터 생성 (테스트용)
func NewCfgFromBytes(bytes []byte) (*Cfg, error)

// INI 파일 객체로부터 생성
func NewCfgFromINIFile(iniFile *ini.File) (*Cfg, error)
```

---

## 7. 헬스체크 시스템

### 7.1 HealthNotifier

```go
// pkg/server/health.go
type HealthNotifier struct {
    ready atomic.Bool
}

func NewHealthNotifier() *HealthNotifier { ... }
func (h *HealthNotifier) SetReady() { h.ready.Store(true) }
func (h *HealthNotifier) SetNotReady() { h.ready.Store(false) }
func (h *HealthNotifier) IsReady() bool { return h.ready.Load() }
```

### 7.2 헬스체크 핸들러

```go
// LivezHandler - 프로세스 생존 확인 (항상 200 OK)
func LivezHandler() http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        w.WriteHeader(http.StatusOK)
        _, _ = w.Write([]byte("OK"))
    }
}

// ReadyzHandler - 준비 상태 확인 (200 또는 503)
func ReadyzHandler(h *HealthNotifier) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        if h != nil && h.IsReady() {
            w.WriteHeader(http.StatusOK)
            // ...
        } else {
            w.WriteHeader(http.StatusServiceUnavailable)
        }
    }
}
```

### 7.3 메트릭 엔드포인트

```go
// pkg/api/http_server.go (line 735)
func (hs *HTTPServer) metricsEndpoint(ctx *web.Context) {
    if !hs.Cfg.MetricsEndpointEnabled {
        return
    }

    if ctx.Req.Method != http.MethodGet || ctx.Req.URL.Path != "/metrics" {
        return
    }

    // Basic Auth 검증 (설정된 경우)
    if hs.metricsEndpointBasicAuthEnabled() &&
        !BasicAuthenticatedRequest(ctx.Req, ...) {
        ctx.Resp.Header().Set("WWW-Authenticate", `Basic realm="Grafana"`)
        ctx.Resp.WriteHeader(http.StatusUnauthorized)
        return
    }

    // Prometheus 핸들러로 위임
    promhttp.HandlerFor(hs.promGatherer, promhttp.HandlerOpts{
        EnableOpenMetrics: true,
    }).ServeHTTP(ctx.Resp, ctx.Req)
}
```

---

## 요약

Grafana의 핵심 컴포넌트는 명확한 책임 분리와 일관된 패턴을 따른다:

| 컴포넌트 | 파일 | 핵심 역할 |
|----------|------|----------|
| HTTPServer | `pkg/api/http_server.go` | 70+ 의존성, HTTP 서빙, 라우트 관리 |
| 서비스 레이어 | `pkg/services/` (74개) | Interface+Impl+Provider, Command/Query |
| 플러그인 매니저 | `pkg/plugins/manager/pipeline/` | 5단계 파이프라인 (Discovery~Termination) |
| SQLStore | `pkg/services/sqlstore/` | XORM ORM, 마이그레이션, 다중 DB |
| 미들웨어 | `pkg/middleware/` | 20단계 체인 (인증, 보안, 메트릭) |
| 설정 시스템 | `pkg/setting/` | 5단계 로딩 체인, 200+ 설정 필드 |
| 헬스체크 | `pkg/server/health.go` | Liveness/Readiness 프로브 |

Wire DI가 이 모든 컴포넌트를 연결하여 하나의 서버 인스턴스로 조합한다. 서비스 간 의존성은 인터페이스를 통해 느슨하게 결합되며, OSS/Enterprise 빌드에서 다른 구현체를 주입할 수 있다.
