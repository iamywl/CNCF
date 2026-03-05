# Grafana 코드 구조

## 개요

Grafana는 Go 백엔드와 React/TypeScript 프론트엔드로 구성된 대규모 풀스택 애플리케이션이다. 단일 리포지토리(monorepo) 구조로 백엔드, 프론트엔드, 플러그인, 설정, 빌드 시스템이 모두 하나의 저장소에 포함되어 있다. Go 워크스페이스(`go.work`)와 Yarn 워크스페이스를 활용하여 모듈 간 의존성을 관리한다.

---

## 1. 최상위 디렉토리 구조

```
grafana/
├── apps/                   # K8s App SDK 기반 앱 (dashboard, folder, alerting, iam 등)
├── conf/                   # 설정 파일 (defaults.ini, sample.ini, provisioning/)
├── contribute/             # 기여 가이드 문서
├── devenv/                 # 개발 환경 설정 (Docker Compose, 테스트 데이터)
├── docs/                   # 프로젝트 문서
├── e2e/                    # Cypress E2E 테스트
├── e2e-playwright/         # Playwright E2E 테스트
├── emails/                 # 이메일 템플릿
├── grafana-mixin/          # Grafana 모니터링 mixin (Prometheus 규칙)
├── hack/                   # 개발 유틸리티 스크립트
├── kinds/                  # CUE 스키마 정의 (dashboard 등)
├── packages/               # @grafana/* 공유 프론트엔드 패키지
├── pkg/                    # Go 백엔드 핵심 코드
├── public/                 # 프론트엔드 앱, 정적 자원
├── scripts/                # 빌드, CI/CD, 코드 생성 스크립트
├── Dockerfile              # 컨테이너 빌드
├── Makefile                # 백엔드 빌드 타겟
├── go.mod                  # Go 모듈 정의
├── go.work                 # Go 워크스페이스 정의
├── package.json            # 프론트엔드 패키지 정의
└── embed.go                # Go embed 정적 자원
```

### 핵심 디렉토리 역할

| 디렉토리 | 역할 | 언어 |
|----------|------|------|
| `pkg/` | 백엔드 핵심 로직 전체 | Go |
| `public/app/` | 프론트엔드 React SPA | TypeScript/React |
| `packages/` | 공유 프론트엔드 라이브러리 | TypeScript |
| `apps/` | K8s App SDK 기반 마이크로서비스 앱 | Go |
| `conf/` | 서버 설정, 프로비저닝 YAML | INI/YAML |
| `kinds/` | CUE 스키마 (코드 생성 원본) | CUE |

---

## 2. 백엔드 디렉토리 구조 (pkg/)

`pkg/` 디렉토리는 Grafana 백엔드의 핵심이다. 74개 이상의 서비스 패키지를 포함하며, 모든 비즈니스 로직이 이곳에 구현되어 있다.

```
pkg/
├── api/                    # HTTP API 핸들러, 라우트 등록
├── apimachinery/           # K8s API 매커니즘 공통
├── apis/                   # K8s 스타일 API 그룹 정의
├── apiserver/              # 내장 K8s API 서버
├── build/                  # 빌드 관련 유틸
├── bus/                    # 이벤트 버스
├── cmd/                    # CLI 명령어 진입점
├── components/             # 재사용 컴포넌트
├── configprovider/         # 설정 제공자 인터페이스
├── events/                 # 도메인 이벤트 정의
├── expr/                   # 서버사이드 표현식 엔진
├── extensions/             # OSS/Enterprise 확장 포인트
├── infra/                  # 인프라 계층 (DB, 캐시, 로깅, 트레이싱, 메트릭)
├── login/                  # 소셜 로그인 커넥터
├── middleware/             # HTTP 미들웨어 체인
├── models/                 # 레거시 데이터 모델
├── modules/                # 모듈 시스템
├── plugins/                # 플러그인 시스템 코어
├── promlib/                # Prometheus 쿼리 라이브러리
├── registry/               # 서비스 레지스트리, API 등록
├── server/                 # 서버 초기화, Wire DI, 헬스체크
├── services/               # 74개 이상의 비즈니스 로직 서비스
├── setting/                # 설정 관리 (Cfg 구조체)
├── storage/                # 통합 스토리지 추상화
├── tsdb/                   # 시계열 DB 쿼리 백엔드
├── util/                   # 유틸리티 함수
└── web/                    # 웹 프레임워크 래퍼 (Mux)
```

### 2.1 pkg/api/ - HTTP API 핸들러

HTTP API의 진입점이다. `HTTPServer` 구조체가 모든 API 핸들러를 관리한다.

```
pkg/api/
├── http_server.go          # HTTPServer 구조체, ProvideHTTPServer(), Run()
├── api.go                  # registerRoutes() - 모든 API 라우트 등록
├── dashboard.go            # 대시보드 CRUD 핸들러
├── datasources.go          # 데이터소스 CRUD 핸들러
├── admin.go                # 관리자 API
├── alerting.go             # 알림 API
├── annotations.go          # 어노테이션 API
├── avatar/                 # Gravatar 아바타 캐싱
├── datasource/             # 데이터소스 연결 클라이언트
├── routing/                # 라우트 레지스터 인터페이스
├── static/                 # 정적 파일 서빙
└── apierrors/              # API 에러 타입 정의
```

`HTTPServer` 구조체는 70개 이상의 의존성을 주입받는다. 소스 코드에서 확인한 주요 의존성 목록:

```go
// pkg/api/http_server.go - HTTPServer 구조체 필드 (일부)
type HTTPServer struct {
    log              log.Logger
    web              *web.Mux
    context          context.Context
    httpSrv          *http.Server
    middlewares      []web.Handler
    bus              bus.Bus

    Cfg                          *setting.Cfg
    Features                     featuremgmt.FeatureToggles
    RouteRegister                routing.RouteRegister
    RenderService                rendering.Service
    HooksService                 *hooks.HooksService
    CacheService                 *localcache.CacheService
    DataSourceCache              datasources.CacheService
    AuthTokenService             auth.UserTokenService
    QuotaService                 quota.Service
    RemoteCacheService           *remotecache.RemoteCache
    ProvisioningService          provisioning.ProvisioningService
    License                      licensing.Licensing
    AccessControl                accesscontrol.AccessControl
    DataProxy                    *datasourceproxy.DataSourceProxyService
    SearchService                search.Service
    Live                         *live.GrafanaLive
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
    // ... 총 70+ 필드
}
```

### 2.2 pkg/services/ - 비즈니스 로직 서비스

74개 이상의 서비스 패키지가 존재한다. 각 서비스는 Interface + Implementation + Provider 패턴을 따른다.

```
pkg/services/
├── accesscontrol/          # RBAC 접근 제어
├── annotations/            # 어노테이션 서비스
├── anonymous/              # 익명 사용자 서비스
├── apikey/                 # API 키 관리
├── apiserver/              # 내장 K8s API 서버
├── auth/                   # 인증 토큰 관리
├── authn/                  # 인증 통합 (authnimpl)
├── authz/                  # 인가 서비스
├── caching/                # 캐싱 서비스
├── cleanup/                # 정리 작업 (세션, 임시 파일)
├── cloudmigration/         # 클라우드 마이그레이션
├── contexthandler/         # HTTP 컨텍스트 핸들러
├── correlations/           # 상관관계 서비스
├── dashboardimport/        # 대시보드 가져오기
├── dashboards/             # 대시보드 CRUD 서비스
├── dashboardsnapshots/     # 대시보드 스냅샷
├── dashboardversion/       # 대시보드 버전 관리
├── datasourceproxy/        # 데이터소스 프록시
├── datasources/            # 데이터소스 관리
├── encryption/             # 암호화 서비스
├── extsvcauth/             # 외부 서비스 인증
├── featuremgmt/            # 피처 플래그 관리
├── folder/                 # 폴더 서비스
├── frontend/               # 프론트엔드 설정 서비스
├── grpcserver/             # gRPC 서버
├── hooks/                  # 후크 시스템
├── ldap/                   # LDAP 연동
├── libraryelements/        # 라이브러리 요소
├── librarypanels/          # 라이브러리 패널
├── licensing/              # 라이선스 관리
├── live/                   # 라이브 스트리밍 (WebSocket)
├── login/                  # 로그인 서비스
├── loginattempt/           # 로그인 시도 추적
├── navtree/                # 내비게이션 트리
├── ngalert/                # 차세대 알림 시스템
├── notifications/          # 알림 전송
├── oauthtoken/             # OAuth 토큰 관리
├── org/                    # 조직 관리
├── playlist/               # 플레이리스트 서비스
├── plugindashboards/       # 플러그인 대시보드
├── pluginsintegration/     # 플러그인 통합 (설정, 컨텍스트, 스토어)
├── preference/             # 사용자 환경설정
├── provisioning/           # 프로비저닝 서비스
├── publicdashboards/       # 공개 대시보드
├── query/                  # 쿼리 서비스
├── queryhistory/           # 쿼리 히스토리
├── quota/                  # 쿼터 관리
├── rendering/              # 이미지 렌더링
├── search/                 # 검색 서비스
├── searchusers/            # 사용자 검색
├── secrets/                # 비밀 키 관리
├── serviceaccounts/        # 서비스 계정
├── shorturls/              # 단축 URL
├── sqlstore/               # SQL 데이터베이스 추상화
├── ssosettings/            # SSO 설정
├── star/                   # 즐겨찾기
├── stats/                  # 통계 서비스
├── store/                  # 스토어 서비스
├── supportbundles/         # 지원 번들
├── tag/                    # 태그 서비스
├── team/                   # 팀 관리
├── temp_user/              # 임시 사용자
├── updatemanager/          # 업데이트 확인
├── user/                   # 사용자 관리
└── validations/            # 유효성 검증
```

### 2.3 pkg/server/ - 서버 초기화 및 Wire DI

서버의 진입점과 의존성 주입(DI) 코드가 위치한다.

```
pkg/server/
├── server.go               # Server 구조체, New(), Init(), Run()
├── wire.go                 # Wire DI 주입 정의 (wireinject 빌드 태그)
├── wire_gen.go             # Wire 자동 생성 코드
├── wireexts_oss.go         # OSS 전용 Wire 확장
├── health.go               # HealthNotifier, LivezHandler, ReadyzHandler
├── service.go              # 서비스 인터페이스
├── module_server.go        # 모듈 서버
├── module_runner.go        # 모듈 실행기
├── runner.go               # 서비스 실행기
├── ring.go                 # 해시 링
├── memberlist.go           # 멤버리스트 통합
└── instrumentation_service.go  # 계측 서비스
```

`wire.go`는 `//go:build wireinject` 빌드 태그를 사용하며, Google Wire를 통해 모든 서비스 의존성을 연결한다:

```go
// pkg/server/wire.go (상단)
//go:build wireinject
// +build wireinject

package server

import (
    "github.com/google/wire"
    "github.com/grafana/grafana/pkg/api"
    "github.com/grafana/grafana/pkg/infra/db"
    "github.com/grafana/grafana/pkg/infra/tracing"
    "github.com/grafana/grafana/pkg/services/accesscontrol"
    // ... 수십 개의 서비스 임포트
)
```

`server.go`의 `New()` 함수는 서버 인스턴스를 생성하고 초기화한다:

```go
// pkg/server/server.go
func New(opts Options, cfg *setting.Cfg, httpServer *api.HTTPServer,
    roleRegistry accesscontrol.RoleRegistry,
    provisioningService provisioning.ProvisioningService,
    backgroundServiceProvider registry.BackgroundServiceRegistry,
    usageStatsProvidersRegistry registry.UsageStatsProvidersRegistry,
    statsCollectorService *statscollector.Service,
    tracerProvider *tracing.TracingService,
    features featuremgmt.FeatureToggles,
    promReg prometheus.Registerer,
) (*Server, error) {
    // ...
}
```

### 2.4 pkg/tsdb/ - 시계열 DB 쿼리 백엔드

20개 이상의 데이터소스 구현이 포함되어 있다:

```
pkg/tsdb/
├── azuremonitor/                   # Azure Monitor
├── cloud-monitoring/               # Google Cloud Monitoring
├── cloudwatch/                     # AWS CloudWatch
├── elasticsearch/                  # Elasticsearch
├── grafana-postgresql-datasource/  # PostgreSQL
├── grafana-pyroscope-datasource/   # Pyroscope (프로파일링)
├── grafana-testdata-datasource/    # 테스트 데이터
├── grafanads/                      # 내장 Grafana 데이터소스
├── graphite/                       # Graphite
├── influxdb/                       # InfluxDB
├── jaeger/                         # Jaeger (트레이싱)
├── loki/                           # Grafana Loki (로그)
├── mssql/                          # Microsoft SQL Server
├── mysql/                          # MySQL
├── opentsdb/                       # OpenTSDB
├── parca/                          # Parca (프로파일링)
├── prometheus/                     # Prometheus
├── tempo/                          # Grafana Tempo (트레이싱)
└── zipkin/                         # Zipkin (트레이싱)
```

### 2.5 pkg/plugins/ - 플러그인 시스템

```
pkg/plugins/
├── ifaces.go               # 플러그인 인터페이스 (Installer, FileStore, PluginSource)
├── apiserver.go            # 플러그인 API 서버 통합
├── errors.go               # 에러 타입 정의
├── config/                 # 플러그인 설정
├── envvars/                # 환경 변수 처리
├── auth/                   # 플러그인 인증
├── backendplugin/          # 백엔드 플러그인 관리
├── codegen/                # 코드 생성
├── httpresponsesender/     # HTTP 응답 전송
├── instrumentationutils/   # 계측 유틸
├── log/                    # 플러그인 로깅
├── manager/                # 플러그인 매니저
│   ├── client/             # 플러그인 클라이언트
│   ├── filestore/          # 파일 스토어
│   ├── installer/          # 설치 관리자
│   ├── loader/             # 로더
│   ├── pipeline/           # 라이프사이클 파이프라인
│   │   ├── bootstrap/      # 부트스트랩 단계
│   │   ├── discovery/      # 발견 단계
│   │   ├── initialization/ # 초기화 단계
│   │   ├── validation/     # 검증 단계
│   │   └── termination/    # 종료 단계
│   ├── process/            # 프로세스 관리
│   ├── registry/           # 레지스트리
│   ├── signature/          # 서명 검증
│   └── sources/            # 플러그인 소스
└── localfiles/             # 로컬 파일 시스템
```

### 2.6 pkg/infra/ - 인프라 계층

```
pkg/infra/
├── db/                     # 데이터베이스 접근 인터페이스
├── filestorage/            # 파일 스토리지 추상화
├── fs/                     # 파일 시스템 유틸
├── httpclient/             # HTTP 클라이언트 팩토리
├── kvstore/                # 키-값 저장소
├── localcache/             # 로컬 인메모리 캐시
├── log/                    # 로깅 (slogadapter 포함)
├── metrics/                # Prometheus 메트릭 등록
├── network/                # 네트워크 유틸
├── process/                # 프로세스 관리
├── remotecache/            # 원격 캐시 (Redis, Memcached)
├── serverlock/             # 서버 잠금 (HA)
├── slugify/                # 슬러그 변환
├── tracing/                # 분산 트레이싱 (OpenTelemetry)
└── usagestats/             # 사용량 통계
```

### 2.7 pkg/middleware/ - HTTP 미들웨어

```
pkg/middleware/
├── middleware.go           # 공통 미들웨어 (ReqSignedIn, ReqGrafanaAdmin 등)
├── auth.go                 # 인증 미들웨어
├── csp.go                  # Content Security Policy
├── csrf/                   # CSRF 보호
├── dashboard_redirect.go   # 대시보드 리다이렉트
├── gziper.go               # Gzip 압축
├── loggermw/               # 요청 로깅
├── org_redirect.go         # 조직 리다이렉트
├── quota.go                # 쿼터 체크
├── recovery.go             # 패닉 복구
├── request_metadata_test.go  # 요청 메타데이터
├── request_metrics.go      # 요청 메트릭 수집
├── request_tracing.go      # 요청 트레이싱
├── requestmeta/            # 요청 메타데이터 (SetupRequestMetadata)
├── subpath_redirect.go     # 서브패스 리다이렉트
├── validate_action_url.go  # 액션 URL 검증
└── validate_host.go        # 호스트 검증
```

### 2.8 pkg/setting/ - 설정 관리

```go
// pkg/setting/setting.go
type Cfg struct {
    Target    []string
    Raw       *ini.File
    Logger    log.Logger

    // HTTP Server Settings
    CertFile          string
    KeyFile           string
    HTTPAddr          string
    HTTPPort          string
    Protocol          Scheme
    AppURL            string
    AppSubURL         string
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

    // Rendering
    RendererServerUrl              string
    RendererCallbackUrl            string
    RendererConcurrentRequestLimit int

    // SMTP
    Smtp SmtpSettings

    // Build info
    BuildVersion string
    BuildCommit  string
    IsEnterprise bool
    // ... 200+ 추가 필드
}
```

### 2.9 pkg/expr/ - 서버사이드 표현식 엔진

```
pkg/expr/
├── commands.go             # 표현식 명령 (math, reduce, resample, threshold, classic)
├── converter.go            # 데이터 타입 변환
├── dataplane.go            # 데이터 플레인 처리
├── classic/                # 클래식 컨디션 (레거시 알림 호환)
└── convert_to_full_long.go # 롱 포맷 변환
```

---

## 3. 프론트엔드 디렉토리 구조

### 3.1 public/app/ - 메인 React SPA

```
public/app/
├── app.ts                  # 애플리케이션 부트스트랩
├── AppWrapper.tsx           # React 루트 컴포넌트
├── initApp.ts              # 앱 초기화 함수
├── index.ts                # 진입점
├── api/                    # API 클라이언트
├── core/                   # 핵심 프론트엔드 로직
├── extensions/             # 확장 시스템
├── features/               # 기능별 모듈
├── plugins/                # 프론트엔드 플러그인 시스템
├── routes/                 # 라우팅 정의
├── store/                  # Redux 스토어
└── types/                  # TypeScript 타입 정의
```

### 3.2 packages/ - 공유 패키지 (@grafana/*)

```
packages/
├── grafana-data/           # 핵심 데이터 타입, 유틸 (@grafana/data)
├── grafana-ui/             # UI 컴포넌트 라이브러리 (@grafana/ui)
├── grafana-runtime/        # 런타임 서비스 (@grafana/runtime)
├── grafana-schema/         # 스키마 정의 (@grafana/schema)
├── grafana-alerting/       # 알림 관련 공유 코드
├── grafana-e2e-selectors/  # E2E 셀렉터
├── grafana-eslint-rules/   # ESLint 규칙
├── grafana-flamegraph/     # 플레임그래프 컴포넌트
├── grafana-i18n/           # 국제화
├── grafana-o11y-ds-frontend/ # 옵저버빌리티 데이터소스 프론트엔드
├── grafana-openapi/        # OpenAPI 클라이언트
├── grafana-plugin-configs/ # 플러그인 설정
├── grafana-prometheus/     # Prometheus 프론트엔드 공통
├── grafana-sql/            # SQL 데이터소스 공통
└── grafana-test-utils/     # 테스트 유틸
```

---

## 4. apps/ - K8s App SDK 기반 앱

Grafana는 내부 기능을 점진적으로 K8s API 서버 스타일의 독립 앱으로 분리하고 있다:

```
apps/
├── advisor/                # 어드바이저 앱
├── alerting/               # 알림 시스템 앱
│   ├── alertenrichment/    # 알림 보강
│   ├── historian/          # 알림 히스토리
│   ├── notifications/      # 알림 전송
│   └── rules/              # 알림 규칙
├── annotation/             # 어노테이션 앱
├── collections/            # 컬렉션 앱
├── correlations/           # 상관관계 앱
├── dashboard/              # 대시보드 앱
├── dashvalidator/          # 대시보드 검증기
├── example/                # 예제 앱
├── folder/                 # 폴더 앱
├── iam/                    # ID/접근 관리 앱
├── live/                   # 라이브 스트리밍 앱
├── logsdrilldown/          # 로그 드릴다운 앱
├── playlist/               # 플레이리스트 앱
├── plugins/                # 플러그인 관리 앱
├── preferences/            # 환경설정 앱
├── provisioning/           # 프로비저닝 앱
├── quotas/                 # 쿼터 앱
├── scope/                  # 스코프 앱
├── secret/                 # 비밀 키 앱
└── shorturl/               # 단축 URL 앱
```

각 앱은 `go.work`에 독립 모듈로 등록되어 있으며, Grafana App SDK를 사용하여 K8s API 서버와 통합된다.

---

## 5. 설정 파일 구조 (conf/)

```
conf/
├── defaults.ini            # 기본 설정값 (수정 불가)
├── sample.ini              # 사용자 정의 설정 샘플
├── ldap.toml               # LDAP 설정
├── ldap_multiple.toml      # 다중 LDAP 설정
└── provisioning/           # 프로비저닝 설정
    ├── access-control/     # 접근 제어 프로비저닝
    ├── alerting/           # 알림 프로비저닝
    ├── dashboards/         # 대시보드 프로비저닝
    ├── datasources/        # 데이터소스 프로비저닝
    ├── plugins/            # 플러그인 프로비저닝
    └── sample/             # 샘플 프로비저닝 파일
```

`defaults.ini`의 주요 섹션:

| 섹션 | 용도 | 키 예시 |
|------|------|--------|
| `[paths]` | 파일 경로 설정 | `data`, `logs`, `plugins`, `provisioning` |
| `[server]` | HTTP 서버 설정 | `protocol`, `http_addr`, `http_port`, `domain`, `root_url` |
| `[database]` | 데이터베이스 | `type`, `host`, `name`, `user`, `password` |
| `[remote_cache]` | 원격 캐시 | `type` (redis/memcached/database) |
| `[dataproxy]` | 데이터 프록시 | `timeout`, `keep_alive_seconds` |
| `[analytics]` | 분석/통계 | `reporting_enabled`, `check_for_updates` |
| `[security]` | 보안 | `admin_user`, `secret_key`, `cookie_secure` |
| `[users]` | 사용자 관리 | `allow_sign_up`, `auto_assign_org_role` |
| `[auth]` | 인증 | `login_cookie_name`, `oauth_auto_login` |
| `[dashboards]` | 대시보드 설정 | `versions_to_keep`, `min_refresh_interval` |
| `[grpc_server]` | gRPC 서버 | `network`, `address`, `use_tls` |

---

## 6. 빌드 시스템

### 6.1 Makefile 주요 타겟

Grafana의 Makefile은 백엔드 빌드 과정을 관리한다:

```makefile
# 주요 빌드 변수
GO = go
GO_VERSION = 1.25.7
WIRE_TAGS = "oss"

# 버전 정보 (ldflags로 주입)
GO_LDFLAGS = -X main.version=$(BUILD_VERSION) \
    -X main.commit=$(BUILD_COMMIT) \
    -X main.buildBranch=$(BUILD_BRANCH) \
    -X main.buildstamp=$(BUILD_STAMP)
```

| 타겟 | 설명 |
|------|------|
| `make deps-go` | Go 의존성 다운로드 (`go mod download`) |
| `make deps-js` | 프론트엔드 의존성 설치 (`yarn install --immutable`) |
| `make deps` | 모든 의존성 설치 |
| `make build` | 전체 빌드 |
| `make run` | 개발 서버 실행 |
| `make test-go` | Go 테스트 실행 |
| `make lint-go` | Go 린팅 |
| `make swagger-oss-gen` | OpenAPI 스펙 생성 |
| `make gen-go` | Wire DI 코드 생성 |
| `make gen-cue` | CUE 스키마 코드 생성 |

### 6.2 Wire DI 코드 생성

Google Wire를 사용한 의존성 주입 코드 생성:

```bash
# wire.go (wireinject 빌드 태그) → wire_gen.go (자동 생성)
# wireexts_oss.go - OSS 빌드 전용 Wire 확장
```

Wire는 `pkg/server/wire.go`에 정의된 의존성 그래프를 분석하여 `wire_gen.go`를 자동 생성한다. 이 파일에는 모든 서비스의 초기화 코드가 포함된다.

### 6.3 프론트엔드 빌드

```json
// package.json 주요 스크립트
{
    "build": "NODE_ENV=production nx exec -- webpack --config scripts/webpack/webpack.prod.js",
    "dev": "NODE_ENV=dev nx exec -- webpack --config scripts/webpack/webpack.dev.js",
    "test": "jest --notify --watch",
    "test:ci": "jest --ci --reporters=default --reporters=jest-junit",
    "lint:ts": "eslint ./ ./public/app/extensions/ --cache",
    "lint:sass": "yarn stylelint '{public/sass,packages}/**/*.scss' --cache",
    "e2e:playwright": "yarn playwright test"
}
```

---

## 7. 의존성 관리

### 7.1 Go 의존성

**go.mod**: 758줄의 Go 모듈 파일이며, Go 1.25.7을 사용한다.

```go
module github.com/grafana/grafana
go 1.25.7
```

주요 의존성:

| 라이브러리 | 용도 |
|-----------|------|
| `github.com/google/wire` | 의존성 주입 코드 생성 |
| `gopkg.in/ini.v1` | INI 파일 파싱 (설정) |
| `github.com/grafana/grafana-plugin-sdk-go` | 플러그인 SDK |
| `github.com/prometheus/client_golang` | Prometheus 메트릭 |
| `go.opentelemetry.io/otel` | OpenTelemetry 트레이싱 |
| `github.com/jmoiron/sqlx` | SQL 확장 |
| XORM (커스텀 포크) | ORM |
| `google.golang.org/grpc` | gRPC |

**go.work**: Go 워크스페이스를 정의하며, 40개 이상의 모듈을 통합 관리한다.

```go
go 1.25.7

use (
    .
    ./apps/advisor
    ./apps/alerting/alertenrichment
    ./apps/alerting/historian
    ./apps/alerting/notifications
    ./apps/alerting/rules
    ./apps/dashboard
    ./apps/folder
    ./apps/iam
    ./pkg/aggregator
    ./pkg/apimachinery
    ./pkg/apiserver
    ./pkg/plugins
    ./pkg/promlib
    // ... 40+ 모듈
)
```

### 7.2 프론트엔드 의존성

**package.json**: Yarn 4.11 (Berry) 패키지 매니저를 사용한다.

- `@grafana/data`, `@grafana/ui`, `@grafana/runtime`, `@grafana/schema` 등 내부 패키지
- React 18+ 기반 SPA
- Webpack 빌드
- Nx 모노레포 도구 활용
- Jest 테스트 프레임워크
- Playwright E2E 테스트

---

## 8. 테스트 구조

### 8.1 백엔드 테스트

| 테스트 유형 | 위치 | 실행 명령 |
|------------|------|----------|
| 단위 테스트 | `*_test.go` (소스 옆) | `go test ./pkg/...` |
| 통합 테스트 | `pkg/tests/` | `go test -tags integration` |
| 벤치마크 | `*_test.go` (Benchmark 함수) | `go test -bench=.` |

### 8.2 프론트엔드 테스트

| 테스트 유형 | 도구 | 실행 명령 |
|------------|------|----------|
| 단위/컴포넌트 | Jest | `yarn test` |
| E2E (레거시) | Cypress | `yarn e2e` |
| E2E (신규) | Playwright | `yarn e2e:playwright` |
| 린팅 | ESLint + Stylelint | `yarn lint` |
| 스토리북 | Storybook | `yarn storybook` |

### 8.3 테스트 샤딩

CI에서 테스트 병렬 실행을 위한 샤딩 지원:

```makefile
SHARD ?= 1
SHARDS ?= 1
```

```json
"test:ci": "jest --ci --shard=${TEST_SHARD:-1}/${TEST_SHARD_TOTAL:-1}"
```

---

## 9. 빌드 태그

Grafana는 빌드 태그를 사용하여 OSS와 Enterprise 빌드를 분리한다:

| 빌드 태그 | 용도 |
|----------|------|
| `oss` | OSS 빌드 (기본) |
| `wireinject` | Wire DI 코드 생성 시 사용 |
| `integration` | 통합 테스트 실행 |
| `requires_buildifer` | Buildifier 필요 테스트 |

```makefile
WIRE_TAGS = "oss"
```

`pkg/extensions/` 디렉토리가 빌드 태그에 따라 OSS/Enterprise 코드를 분기하는 확장 포인트 역할을 한다. `pkg/server/wireexts_oss.go`에 OSS 전용 Wire 확장이 정의되어 있다.

---

## 10. 코드 패턴 요약

### 10.1 Interface + Implementation + Provider 패턴

Grafana의 모든 서비스는 세 가지 구성 요소로 이루어진다:

```
pkg/services/{서비스명}/
├── {서비스명}.go            # 인터페이스 정의
└── {서비스명}impl/
    └── {서비스명}.go        # 구현체 + Provide* 팩토리 함수
```

예시 (SQLStore):

```go
// 인터페이스: pkg/infra/db/db.go
type DB interface { ... }

// 구현: pkg/services/sqlstore/sqlstore.go
type SQLStore struct { ... }

// Provider: pkg/services/sqlstore/sqlstore.go
func ProvideService(cfg *setting.Cfg, ...) (*SQLStore, error) { ... }
```

### 10.2 Command/Query 분리 패턴

읽기와 쓰기 작업을 명확하게 분리한다:

```go
// Command - 쓰기 작업
type SaveDashboardCommand struct { ... }

// Query - 읽기 작업
type GetDashboardQuery struct { ... }
```

### 10.3 설정 로딩 체인

설정은 다음 순서로 로딩되며, 후순위가 우선한다:

```
conf/defaults.ini          # 1. 기본값 (변경 불가)
    ↓
conf/grafana.ini           # 2. 사용자 정의 (선택)
    ↓
conf/custom.ini            # 3. 커스텀 설정 (선택)
    ↓
GF_{SECTION}_{KEY} 환경변수  # 4. 환경 변수 오버라이드
    ↓
CLI 플래그                  # 5. 명령행 인자
```

---

## 11. 주요 진입점 정리

| 파일 | 함수 | 역할 |
|------|------|------|
| `pkg/cmd/grafana/main.go` | `main()` | 프로그램 시작점 |
| `pkg/server/server.go` | `New()` | 서버 인스턴스 생성 및 초기화 |
| `pkg/server/wire.go` | `Initialize()` | Wire DI로 모든 의존성 주입 |
| `pkg/api/http_server.go` | `ProvideHTTPServer()` | HTTP 서버 생성 (70+ 의존성) |
| `pkg/api/http_server.go` | `Run()` | HTTP 서버 시작 |
| `pkg/api/api.go` | `registerRoutes()` | API 라우트 등록 |
| `pkg/api/http_server.go` | `applyRoutes()` | 미들웨어 및 라우트 적용 |
| `public/app/app.ts` | 부트스트랩 | 프론트엔드 앱 초기화 |

---

## 요약

Grafana의 코드 구조는 대규모 풀스택 애플리케이션의 전형적인 모노레포 패턴을 따른다. 백엔드는 `pkg/` 아래 74개 이상의 서비스 패키지로 구성되며, Google Wire를 통한 의존성 주입으로 연결된다. 프론트엔드는 React SPA로 `public/app/`에 위치하며, `packages/` 아래의 공유 라이브러리(`@grafana/*`)를 활용한다. 최근에는 `apps/` 디렉토리를 통해 K8s App SDK 기반의 마이크로서비스 아키텍처로 점진적으로 이전하고 있다. 설정은 INI 파일 기반의 계층적 로딩 체인을 사용하며, 환경 변수를 통한 오버라이드를 지원한다.
