# 07. Argo CD API 서버 Deep-Dive

## 목차

1. [개요](#1-개요)
2. [ArgoCDServer 구조체](#2-argoCDServer-구조체)
3. [cmux 포트 멀티플렉싱](#3-cmux-포트-멀티플렉싱)
4. [gRPC 서버 구성](#4-grpc-서버-구성)
5. [HTTP 서버 구성](#5-http-서버-구성)
6. [인증 체계](#6-인증-체계)
7. [ArgoCDServiceSet](#7-argoCDServiceSet)
8. [Application gRPC 서비스](#8-application-grpc-서비스)
9. [설정 감시 (watchSettings)](#9-설정-감시-watchsettings)
10. [왜 이런 설계인가](#10-왜-이런-설계인가)

---

## 1. 개요

Argo CD API 서버(`argocd-server`)는 Argo CD의 핵심 진입점이다. 브라우저 UI, CLI, 외부 시스템이 모두 이 서버를 통해 Argo CD와 통신한다. API 서버의 역할은 다음과 같다.

- gRPC 및 REST API 노출 (단일 포트, TLS 선택적)
- JWT/OIDC 기반 인증, Casbin 기반 RBAC 권한 검사
- Kubernetes API 서버에 대한 프록시 역할 (Application CRD CRUD)
- 실시간 스트리밍 (Watch, 터미널 세션)
- Dex SSO 역프록시

```
                          ┌──────────────────────────────────────────┐
                          │           ArgoCDServer (:8080)           │
                          │                                          │
  브라우저 ──────────────► │  cmux  ┌──► HTTP/1.1  → httpS           │
  argocd CLI ────────────► │        ├──► TLS+HTTP  → httpsS          │
  외부 시스템 ─────────────► │        └──► TLS+gRPC  → grpcS           │
                          │                                          │
                          │  grpc-gateway: gRPC ↔ REST 트랜스코딩    │
                          │  grpcweb: gRPC ↔ HTTP/1.1 래핑          │
                          └──────────────────────────────────────────┘
                                          │
                    ┌─────────────────────┼─────────────────────┐
                    ▼                     ▼                     ▼
           K8s API Server          Repo Server           Redis Cache
           (Application CRD)      (매니페스트 생성)       (앱 상태)
```

소스 파일:
- `server/server.go` — ArgoCDServer 구조체 및 Run() 함수
- `server/application/application.go` — Application gRPC 서비스 구현

---

## 2. ArgoCDServer 구조체

`server/server.go:185-220`에 정의된 `ArgoCDServer`는 API 서버 전체를 대표하는 구조체다.

```go
// ArgoCDServer is the API server for Argo CD
type ArgoCDServer struct {
    ArgoCDServerOpts
    ApplicationSetOpts

    ssoClientApp    *oidc.ClientApp
    settings        *settings_util.ArgoCDSettings
    log             *log.Entry
    sessionMgr      *util_session.SessionManager
    settingsMgr     *settings_util.SettingsManager
    enf             *rbac.Enforcer
    projInformer    cache.SharedIndexInformer
    policyEnforcer  *rbacpolicy.RBACPolicyEnforcer
    clusterInformer *settings_util.ClusterInformer
    appInformer     cache.SharedIndexInformer
    appLister       applisters.ApplicationLister
    appsetInformer  cache.SharedIndexInformer
    appsetLister    applisters.ApplicationSetLister
    db              db.ArgoDB

    // stopCh is the channel which when closed, will shutdown the Argo CD server
    stopCh             chan os.Signal
    userStateStorage   util_session.UserStateStorage
    indexDataInit      gosync.Once
    indexData          []byte
    indexDataErr       error
    staticAssets       http.FileSystem
    apiFactory         api.Factory
    secretInformer     cache.SharedIndexInformer
    configMapInformer  cache.SharedIndexInformer
    serviceSet         *ArgoCDServiceSet
    extensionManager   *extension.Manager
    Shutdown           func()
    terminateRequested atomic.Bool
    available          atomic.Bool
}
```

### 각 필드의 역할

| 필드 | 타입 | 역할 |
|------|------|------|
| `ssoClientApp` | `*oidc.ClientApp` | OIDC SSO 클라이언트 앱 (Dex 연동, 그룹 정보 조회, 토큰 갱신) |
| `settings` | `*settings_util.ArgoCDSettings` | argocd-cm ConfigMap에서 로드된 현재 설정 (URL, OIDC, TLS 인증서 등) |
| `sessionMgr` | `*util_session.SessionManager` | JWT 토큰 발급/검증/갱신 담당, HMAC-SHA256 서명 |
| `settingsMgr` | `*settings_util.SettingsManager` | ConfigMap/Secret 읽기/쓰기, 설정 변경 구독 |
| `enf` | `*rbac.Enforcer` | Casbin 기반 RBAC 권한 검사 엔진 |
| `policyEnforcer` | `*rbacpolicy.RBACPolicyEnforcer` | RBAC 정책 로더 및 스코프 설정 |
| `appInformer` | `cache.SharedIndexInformer` | Application CRD 공유 인포머 (watch/list 캐시) |
| `appLister` | `applisters.ApplicationLister` | 인포머 캐시에서 Application 조회하는 리스터 |
| `appsetInformer` | `cache.SharedIndexInformer` | ApplicationSet CRD 공유 인포머 |
| `appsetLister` | `applisters.ApplicationSetLister` | 인포머 캐시에서 ApplicationSet 조회하는 리스터 |
| `db` | `db.ArgoDB` | 클러스터/레포지토리 정보를 K8s Secret에서 읽는 데이터 계층 |
| `serviceSet` | `*ArgoCDServiceSet` | 13개 gRPC 서비스 구현체의 집합 |
| `extensionManager` | `*extension.Manager` | 프록시 확장(extensions) 등록 및 관리 |
| `stopCh` | `chan os.Signal` | OS 시그널 수신 채널 (SIGTERM, SIGINT, GracefulRestartSignal) |
| `terminateRequested` | `atomic.Bool` | 종료 요청 플래그 (원자적 읽기/쓰기) |
| `available` | `atomic.Bool` | 서버 가용 상태 플래그 (헬스 체크용) |

### ArgoCDServerOpts — 설정 옵션

```go
type ArgoCDServerOpts struct {
    DisableAuth             bool
    ContentTypes            []string
    EnableGZip              bool
    Insecure                bool
    StaticAssetsDir         string
    ListenPort              int
    ListenHost              string
    MetricsPort             int
    MetricsHost             string
    Namespace               string
    DexServerAddr           string
    DexTLSConfig            *dexutil.DexTLSConfig
    BaseHRef                string
    RootPath                string
    DynamicClientset        dynamic.Interface
    KubeControllerClientset client.Client
    KubeClientset           kubernetes.Interface
    AppClientset            appclientset.Interface
    RepoClientset           repoapiclient.Clientset
    Cache                   *servercache.Cache
    RepoServerCache         *repocache.Cache
    RedisClient             *redis.Client
    TLSConfigCustomizer     tlsutil.ConfigCustomizer
    XFrameOptions           string
    ContentSecurityPolicy   string
    ApplicationNamespaces   []string
    EnableProxyExtension    bool
    WebhookParallelism      int
    EnableK8sEvent          []string
    HydratorEnabled         bool
    SyncWithReplaceAllowed  bool
}
```

`Insecure` 모드 또는 TLS 인증서 없을 때 HTTP 전용으로 동작한다. `ApplicationNamespaces`는 기본 네임스페이스 외에 Application이 생성될 수 있는 추가 네임스페이스 목록이다.

---

## 3. cmux 포트 멀티플렉싱

`server/server.go:577-758`의 `Run()` 함수는 단일 TCP 포트에서 HTTP/1.1, HTTPS, gRPC를 동시에 처리하기 위해 `github.com/soheilhy/cmux`를 사용한다.

### 전체 흐름 (TLS 모드)

```
TCP :8080
     │
     ▼
  tcpm (cmux.New)
     │
     ├─── HTTP1Fast("PATCH") ──────────────► httpL → httpS (HTTP → HTTPS 리다이렉트)
     │
     └─── Any() ──────────────────────────► tlsl (TLS 언랩)
                                                 │
                                              tlsm (cmux.New, TLS 내부)
                                                 │
                                                 ├─── HTTP1Fast("PATCH") ──► httpsL → httpsS
                                                 │
                                                 └─── HTTP2MatchHeader
                                                      content-type:
                                                      application/grpc ────► grpcL → grpcS
```

### 소스 코드 (핵심 부분)

```go
// server/server.go:621-655
tcpm := cmux.New(listeners.Main)
var tlsm cmux.CMux
var grpcL net.Listener
var httpL net.Listener
var httpsL net.Listener

if !server.useTLS() {
    httpL = tcpm.Match(cmux.HTTP1Fast("PATCH"))
    grpcL = tcpm.MatchWithWriters(
        cmux.HTTP2MatchHeaderFieldSendSettings("content-type", "application/grpc"))
} else {
    // HTTP 1.1을 먼저 매칭 (TLS가 아닌 일반 HTTP 요청 처리)
    httpL = tcpm.Match(cmux.HTTP1Fast("PATCH"))

    // 나머지는 TLS로 가정
    tlsl := tcpm.Match(cmux.Any())
    tlsConfig := tls.Config{
        // http/1.1을 먼저 명시 → HTTPS 클라이언트가 HTTP/1.1 사용
        // h2를 포함 → gRPC 클라이언트가 HTTP/2 사용
        NextProtos: []string{"http/1.1", "h2"},
    }
    tlsConfig.GetCertificate = func(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
        return server.settings.Certificate, nil
    }
    tlsl = tls.NewListener(tlsl, &tlsConfig)

    // TLS 내부에서 다시 cmux로 분기
    tlsm = cmux.New(tlsl)
    httpsL = tlsm.Match(cmux.HTTP1Fast("PATCH"))
    grpcL = tlsm.MatchWithWriters(
        cmux.HTTP2MatchHeaderFieldSendSettings("content-type", "application/grpc"))
}
```

### grpc-web 래핑

브라우저 UI는 HTTP/2 gRPC를 직접 사용할 수 없다. Argo CD는 `improbable-eng/grpc-web` 라이브러리로 gRPC를 HTTP/1.1로 래핑한다.

```go
// server/server.go:599-600
grpcS, appResourceTreeFn := server.newGRPCServer(metricsServ.PrometheusRegistry)
grpcWebS := grpcweb.WrapServer(grpcS)
```

```go
// server/server.go:1178-1180
// handlerSwitcher: content-type으로 라우팅
contentTypeToHandler: map[string]http.Handler{
    "application/grpc-web+proto": grpcWebHandler,
},
```

브라우저가 `content-type: application/grpc-web+proto`로 요청하면 `grpcWebHandler`로 분기된다. 이는 Envoy 프록시 없이도 브라우저에서 직접 gRPC 호출을 가능하게 한다.

### 고루틴 구조

```go
// server/server.go:662-671
go func() { server.checkServeErr("grpcS",   grpcS.Serve(grpcL))       }()
go func() { server.checkServeErr("httpS",   httpS.Serve(httpL))       }()
if server.useTLS() {
    go func() { server.checkServeErr("httpsS", httpsS.Serve(httpsL))  }()
    go func() { server.checkServeErr("tlsm",   tlsm.Serve())          }()
}
go server.watchSettings()
go server.rbacPolicyLoader(ctx)
go func() { server.checkServeErr("tcpm",    tcpm.Serve())             }()
go func() { server.checkServeErr("metrics", metricsServ.Serve(listeners.Metrics)) }()
```

각 서버는 독립 고루틴에서 실행되며, `checkServeErr`는 종료 중이 아닌 상황에서 에러 발생 시 panic한다.

### Graceful Shutdown

```go
// server/server.go:676-740
shutdownFunc := func() {
    server.available.Store(false)
    shutdownCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
    defer cancel()
    var wg gosync.WaitGroup

    wg.Go(func() { httpS.Shutdown(shutdownCtx)  })
    wg.Go(func() { httpsS.Shutdown(shutdownCtx) }) // TLS 모드일 때
    wg.Go(func() { grpcS.GracefulStop()          })
    wg.Go(func() { metricsServ.Shutdown(shutdownCtx) })
    wg.Go(func() { tlsm.Close()                  }) // TLS 모드일 때
    wg.Go(func() { tcpm.Close()                  })
    // 20초 내 완료 대기, 초과 시 경고 후 종료
}
```

---

## 4. gRPC 서버 구성

`server/server.go:910-1004`의 `newGRPCServer()`가 gRPC 서버를 구성한다.

### 서버 옵션

```go
sOpts := []grpc.ServerOption{
    // MaxGRPCMessageSize = ARGOCD_GRPC_MAX_SIZE_MB (기본 200) * 1024 * 1024
    // 실제 코드에서는 apiclient.MaxGRPCMessageSize 상수를 사용
    grpc.MaxRecvMsgSize(apiclient.MaxGRPCMessageSize),  // 기본값 200MB
    grpc.MaxSendMsgSize(apiclient.MaxGRPCMessageSize),  // 기본값 200MB
    grpc.ConnectionTimeout(300 * time.Second),           // 연결 타임아웃 5분
    grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
        MinTime: common.GetGRPCKeepAliveEnforcementMinimum(),
    }),
}
```

`MaxGRPCMessageSize`는 환경변수 `ARGOCD_GRPC_MAX_SIZE_MB`로 조정 가능하며, 기본값은 200MB다(`pkg/apiclient/apiclient.go:68`). 대규모 클러스터에서 많은 수의 리소스를 동기화할 때 단일 응답 크기가 클 수 있기 때문에 높은 한계치를 설정한다.

### 인터셉터 체인

gRPC 인터셉터는 미들웨어 체인을 형성한다. Unary(단방향) 인터셉터 기준으로 순서는 다음과 같다.

```
요청 →
  [1] bug21955WorkaroundInterceptor  (URL 인코딩 우회 버그 수정)
  [2] logging.UnaryServerInterceptor  (요청/응답 로깅)
  [3] serverMetrics.UnaryServerInterceptor  (Prometheus 메트릭 수집)
  [4] grpc_auth.UnaryServerInterceptor(server.Authenticate)  (JWT 인증)
  [5] UserAgentUnaryServerInterceptor  (클라이언트 버전 호환성 체크)
  [6] PayloadUnaryServerInterceptor  (페이로드 로깅, sensitiveMethods 제외)
  [7] ErrorCodeK8sUnaryServerInterceptor  (K8s API 에러 → gRPC 상태 코드 변환)
  [8] ErrorCodeGitUnaryServerInterceptor  (Git 에러 → gRPC 상태 코드 변환)
  [9] recovery.UnaryServerInterceptor  (panic 복구)
  ▼
  핸들러 (실제 비즈니스 로직)
← 응답
```

Stream(스트리밍) 인터셉터도 동일한 순서로 구성된다(단, `bug21955WorkaroundInterceptor` 제외).

```go
sOpts = append(sOpts, grpc.ChainUnaryInterceptor(
    bug21955WorkaroundInterceptor,
    logging.UnaryServerInterceptor(grpc_util.InterceptorLogger(server.log)),
    serverMetrics.UnaryServerInterceptor(),
    grpc_auth.UnaryServerInterceptor(server.Authenticate),
    grpc_util.UserAgentUnaryServerInterceptor(common.ArgoCDUserAgentName, clientConstraint),
    grpc_util.PayloadUnaryServerInterceptor(server.log, true, func(_ context.Context, c interceptors.CallMeta) bool {
        return !sensitiveMethods[c.FullMethod()]
    }),
    grpc_util.ErrorCodeK8sUnaryServerInterceptor(),
    grpc_util.ErrorCodeGitUnaryServerInterceptor(),
    recovery.UnaryServerInterceptor(recovery.WithRecoveryHandler(grpc_util.LoggerRecoveryHandler(server.log))),
))
```

### sensitiveMethods 맵 — 로그 마스킹

자격증명이나 민감한 데이터를 포함하는 엔드포인트는 페이로드 로깅에서 제외된다.

```go
// server/server.go:931-952
sensitiveMethods := map[string]bool{
    "/cluster.ClusterService/Create":                               true,
    "/cluster.ClusterService/Update":                               true,
    "/session.SessionService/Create":                               true,
    "/account.AccountService/UpdatePassword":                       true,
    "/gpgkey.GPGKeyService/CreateGnuPGPublicKey":                   true,
    "/repository.RepositoryService/Create":                         true,
    "/repository.RepositoryService/Update":                         true,
    "/repository.RepositoryService/CreateRepository":               true,
    "/repository.RepositoryService/UpdateRepository":               true,
    "/repository.RepositoryService/ValidateAccess":                 true,
    "/repocreds.RepoCredsService/CreateRepositoryCredentials":      true,
    "/repocreds.RepoCredsService/UpdateRepositoryCredentials":      true,
    "/repository.RepositoryService/CreateWriteRepository":          true,
    "/repository.RepositoryService/UpdateWriteRepository":          true,
    "/repository.RepositoryService/ValidateWriteAccess":            true,
    "/repocreds.RepoCredsService/CreateWriteRepositoryCredentials": true,
    "/repocreds.RepoCredsService/UpdateWriteRepositoryCredentials": true,
    "/application.ApplicationService/PatchResource":                true,
    // 내용이 민감하고 크기가 클 수 있으므로 로그에서 제외
    "/application.ApplicationService/GetManifestsWithFiles":        true,
}
```

총 19개 엔드포인트가 마스킹된다. 클러스터 자격증명, 레포지토리 비밀번호, 사용자 비밀번호 변경 등이 포함된다.

### gRPC 서비스 등록 (13개)

```go
// server/server.go:986-998
grpc_health_v1.RegisterHealthServer(grpcS, healthService)       // 표준 헬스체크
versionpkg.RegisterVersionServiceServer(grpcS, ...)             // 버전 정보
clusterpkg.RegisterClusterServiceServer(grpcS, ...)             // 클러스터 관리
applicationpkg.RegisterApplicationServiceServer(grpcS, ...)     // Application CRUD
applicationsetpkg.RegisterApplicationSetServiceServer(grpcS, ...)  // ApplicationSet
notificationpkg.RegisterNotificationServiceServer(grpcS, ...)   // 알림
repositorypkg.RegisterRepositoryServiceServer(grpcS, ...)       // 레포지토리
repocredspkg.RegisterRepoCredsServiceServer(grpcS, ...)         // 레포 자격증명
sessionpkg.RegisterSessionServiceServer(grpcS, ...)             // 세션/로그인
settingspkg.RegisterSettingsServiceServer(grpcS, ...)           // 설정
projectpkg.RegisterProjectServiceServer(grpcS, ...)             // 프로젝트
accountpkg.RegisterAccountServiceServer(grpcS, ...)             // 계정
certificatepkg.RegisterCertificateServiceServer(grpcS, ...)     // TLS 인증서
gpgkeypkg.RegisterGPGKeyServiceServer(grpcS, ...)               // GPG 키
reflection.Register(grpcS)                                      // gRPC 리플렉션
```

OpenTelemetry 트레이싱도 `grpc.StatsHandler(otelgrpc.NewServerHandler())`로 등록된다.

---

## 5. HTTP 서버 구성

`server/server.go:1165-1280`의 `newHTTPServer()`가 HTTP 서버를 구성한다.

### 라우팅 구조

```
HTTP ServeMux
├── /api/badge          → badge.NewHandler (배지 이미지 생성)
├── /api/logout         → logout.NewHandler (로그아웃 처리)
├── /api/              → gwmux (grpc-gateway REST 트랜스코딩)
│                          └── content-type: application/grpc-web+proto
│                              → grpcWebHandler (grpc-web 래핑)
├── /terminal           → application.NewHandler (웹 터미널, exec)
├── /extensions/*       → extensionManager (프록시 확장, 알파 기능)
├── /extensions.js      → 확장 JavaScript 번들
├── /swagger-ui         → Swagger UI 정적 파일
├── /healthz            → 헬스체크
├── /dex/*              → Dex SSO 역프록시
├── /api/webhook        → webhook.NewHandler (Git 이벤트 처리)
├── /download/*         → argocd CLI 바이너리 다운로드
└── /                   → 정적 에셋 (React SPA)
```

### grpc-gateway 설정

```go
// server/server.go:1190-1192
gwMuxOpts := runtime.WithMarshalerOption(runtime.MIMEWildcard, new(grpc_util.JSONMarshaler))
gwCookieOpts := runtime.WithForwardResponseOption(server.translateGrpcCookieHeader)
gwmux := runtime.NewServeMux(gwMuxOpts, gwCookieOpts)
```

**커스텀 JSON 마샬러**: `grpc_util.JSONMarshaler`를 사용하는 이유는 golang/protobuf의 기본 jsonpb가 `time.Time` 타입을 지원하지 않기 때문이다. gogo/protobuf는 `time.Time`을 지원하지만 커스텀 `MarshalJSON()`을 지원하지 않는다. 따라서 두 라이브러리의 장점을 결합한 커스텀 마샬러를 사용한다.

**Cookie 변환**: `translateGrpcCookieHeader`는 gRPC 응답의 메타데이터를 HTTP `Set-Cookie` 헤더로 변환한다. 로그인 시 JWT 토큰이 쿠키로 브라우저에 전달되는 경로다.

```go
// server/server.go:1115-1131
func (server *ArgoCDServer) translateGrpcCookieHeader(
    ctx context.Context, w http.ResponseWriter, resp golang_proto.Message) error {
    if sessionResp, ok := resp.(*sessionpkg.SessionResponse); ok {
        // 로그인 응답: 토큰을 쿠키로 설정
        return server.setTokenCookie(sessionResp.Token, w)
    } else if md, ok := runtime.ServerMetadataFromContext(ctx); ok {
        // 토큰 갱신: 메타데이터에서 갱신된 토큰을 쿠키로 설정
        renewToken := md.HeaderMD[renewTokenKey]
        if len(renewToken) > 0 {
            return server.setTokenCookie(renewToken[0], w)
        }
    }
    return nil
}
```

### handlerSwitcher — URL/Content-Type 기반 라우팅

```go
// server/server.go:1639-1653
type handlerSwitcher struct {
    handler              http.Handler
    urlToHandler         map[string]http.Handler
    contentTypeToHandler map[string]http.Handler
}

func (s *handlerSwitcher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    if urlHandler, ok := s.urlToHandler[r.URL.Path]; ok {
        urlHandler.ServeHTTP(w, r)
    } else if contentHandler, ok := s.contentTypeToHandler[r.Header.Get("content-type")]; ok {
        contentHandler.ServeHTTP(w, r)
    } else {
        s.handler.ServeHTTP(w, r)
    }
}
```

URL이 먼저 매칭된다. URL이 매칭되지 않으면 Content-Type으로 라우팅한다. 두 조건 모두 매칭되지 않으면 기본 mux 핸들러로 처리된다.

### Content-Type 강제 (CSRF 방어)

```go
// server/server.go:1209-1213
if len(server.ContentTypes) > 0 {
    handler = enforceContentTypes(handler, server.ContentTypes)
} else {
    log.WithField(common.SecurityField, common.SecurityHigh).
        Warnf("Content-Type enforcement is disabled, which may make your API vulnerable to CSRF attacks")
}
```

Content-Type 강제가 비활성화되면 보안 경고를 로그에 기록한다. CSRF 공격을 방지하기 위해 허용된 Content-Type만 처리한다.

### GZip 압축

```go
// server/server.go:1195-1197
if server.EnableGZip {
    handler = compressHandler(handler)
}
```

SSE(Server-Sent Events) 스트림은 압축에서 제외된다. SSE는 실시간 스트리밍이므로 압축 버퍼링이 지연을 유발할 수 있다.

```go
// server/server.go:1154-1163
func compressHandler(handler http.Handler) http.Handler {
    compr := handlers.CompressHandler(handler)
    return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
        if request.Header.Get("Accept") == "text/event-stream" {
            handler.ServeHTTP(writer, request)  // SSE는 압축 미적용
        } else {
            compr.ServeHTTP(writer, request)
        }
    })
}
```

---

## 6. 인증 체계

### Authenticate() — gRPC 인증 인터셉터

`server/server.go:1520-1560`의 `Authenticate()`는 `grpc_auth.UnaryServerInterceptor`에 등록된 인증 함수다.

```go
func (server *ArgoCDServer) Authenticate(ctx context.Context) (context.Context, error) {
    ctx, span = tracer.Start(ctx, "server.ArgoCDServer.Authenticate")
    defer span.End()

    if server.DisableAuth {
        return ctx, nil  // 인증 비활성화 모드
    }

    claims, newToken, claimsErr := server.getClaims(ctx)
    if claims != nil {
        // claims를 컨텍스트에 추가 (RBAC 검사에 사용)
        ctx = context.WithValue(ctx, "claims", claims)
        if newToken != "" {
            // 갱신된 토큰을 gRPC 메타데이터로 전달
            // grpc-gateway가 이를 Set-Cookie로 변환
            grpc.SendHeader(ctx, metadata.New(map[string]string{renewTokenKey: newToken}))
        }
    }

    if claimsErr != nil {
        if !argoCDSettings.AnonymousUserEnabled {
            return ctx, claimsErr  // 인증 실패
        }
        // 익명 사용자 허용 시 빈 claims로 처리
        ctx = context.WithValue(ctx, "claims", "")
    }

    return ctx, nil
}
```

### getClaims() — 토큰 소스 우선순위

`server/server.go:1562-1603`의 `getClaims()`는 세 가지 소스에서 토큰을 순서대로 탐색한다.

```go
func getToken(md metadata.MD) string {
    // 1순위: gRPC 메타데이터의 "token" 키
    //        (argocd CLI가 사용하는 방식)
    tokens, ok := md[apiclient.MetaDataTokenKey]  // MetaDataTokenKey = "token"
    if ok && len(tokens) > 0 {
        return tokens[0]
    }

    // 2순위: HTTP Authorization 헤더의 "Bearer <token>"
    //        (외부 시스템이 사용하는 방식)
    for _, t := range md["authorization"] {
        token := strings.TrimPrefix(t, "Bearer ")
        if strings.HasPrefix(t, "Bearer ") && jwtutil.IsValid(token) {
            return token
        }
    }

    // 3순위: HTTP 쿠키 (grpcgateway-cookie 헤더로 전달됨)
    //        (브라우저 UI가 사용하는 방식)
    for _, t := range md["grpcgateway-cookie"] {
        header := http.Header{}
        header.Add("Cookie", t)
        request := http.Request{Header: header}
        token, err := httputil.JoinCookies(common.AuthCookieName, request.Cookies())
        if err == nil && jwtutil.IsValid(token) {
            return token
        }
    }

    return ""
}
```

### VerifyToken — HMAC vs OIDC 분기

```go
// getClaims 내부 (server/server.go:1579)
claims, newToken, err := server.sessionMgr.VerifyToken(ctx, tokenString)
```

`SessionManager.VerifyToken()`은 토큰의 `iss`(issuer) 클레임을 확인해 분기한다.

```
VerifyToken(token)
    │
    ├─ issuer == "argocd"
    │      └─ HMAC-SHA256 서명 검증
    │         → argocd-secret의 server.secretkey로 서명된 로컬 사용자 토큰
    │
    └─ issuer != "argocd" (예: Dex 또는 외부 OIDC)
           └─ OIDC 공개 키로 서명 검증
              → Dex/외부 OIDC 프로바이더에서 발급된 토큰
```

### 토큰 자동 갱신

```go
// server/server.go:1592-1601
if server.settings.IsSSOConfigured() {
    // SSO 설정 시 그룹 정보를 사용자 정보 엔드포인트에서 조회
    updatedClaims, err := server.ssoClientApp.SetGroupsFromUserInfo(ctx, claims, ...)
    // OIDC 토큰이 만료 임박 시 자동 갱신
    refreshedToken, err := server.ssoClientApp.CheckAndRefreshToken(ctx, updatedClaims,
        server.settings.OIDCRefreshTokenThreshold)
    if refreshedToken != "" && refreshedToken != tokenString {
        newToken = refreshedToken
    }
}
```

`OIDCRefreshTokenThreshold` 설정값(기본값: 만료 5분 전)에 따라 OIDC 토큰이 자동 갱신된다. 갱신된 토큰은 `grpc.SendHeader`로 메타데이터에 실려 클라이언트에게 전달되고, grpc-gateway가 이를 `Set-Cookie`로 변환해 브라우저 쿠키를 업데이트한다.

### AnonymousUser 지원

```go
// server/server.go:1547-1557
if claimsErr != nil {
    argoCDSettings, err := server.settingsMgr.GetSettings()
    if !argoCDSettings.AnonymousUserEnabled {
        return ctx, claimsErr  // 익명 비허용: 401 반환
    }
    ctx = context.WithValue(ctx, "claims", "")  // 익명 허용: 빈 claims
}
```

익명 사용자가 활성화된 경우 인증 실패 시에도 요청이 계속 진행된다. 이후 RBAC 검사에서 빈 claims에 대한 권한만 허용된다.

### 인증 흐름 다이어그램

```
클라이언트 요청
      │
      ▼
getToken(metadata)
      │
      ├─ "token" 메타데이터 키 ────────────────────┐
      ├─ Authorization: Bearer <token> ─────────────┤ tokenString
      └─ grpcgateway-cookie ────────────────────────┘
                              │
                              ▼
               VerifyToken(tokenString)
                              │
              issuer == "argocd"?
                    │              │
                   예             아니오
                    │              │
              HMAC-SHA256       OIDC 검증
              서명 검증           공개 키
                    │              │
                    └──────┬───────┘
                           │
                     claims 유효?
                           │
              ┌────────────┴────────────┐
             예                        아니오
              │                         │
        ctx에 claims 추가         AnonymousUser 허용?
        토큰 갱신 필요시                 │
        SendHeader (newToken)    ┌───────┴───────┐
              │                예              아니오
              ▼                 │                │
          다음 인터셉터     ctx에 "" claims    401 반환
          (RBAC 검사)           │
                           다음 인터셉터
```

---

## 7. ArgoCDServiceSet

`server/server.go:1006-1021`에 정의된 `ArgoCDServiceSet`은 13개 gRPC 서비스 구현체를 하나의 구조체로 묶는다.

```go
type ArgoCDServiceSet struct {
    ClusterService        *cluster.Server
    RepoService           *repository.Server
    RepoCredsService      *repocreds.Server
    SessionService        *session.Server
    ApplicationService    applicationpkg.ApplicationServiceServer
    AppResourceTreeFn     application.AppResourceTreeFn
    ApplicationSetService applicationsetpkg.ApplicationSetServiceServer
    ProjectService        *project.Server
    SettingsService       *settings.Server
    AccountService        *account.Server
    NotificationService   notificationpkg.NotificationServiceServer
    CertificateService    *certificate.Server
    GpgkeyService         *gpgkey.Server
    VersionService        *version.Server
}
```

### 서비스별 역할

| 서비스 | 구현 패키지 | 주요 기능 |
|--------|-----------|----------|
| `ClusterService` | `server/cluster` | K8s 클러스터 등록/수정/삭제, 클러스터 리소스 조회 |
| `RepoService` | `server/repository` | Git/Helm/OCI 레포지토리 관리, 앱 세부 정보 조회 |
| `RepoCredsService` | `server/repocreds` | 레포지토리 자격증명 템플릿 관리 |
| `SessionService` | `server/session` | 로그인/로그아웃, JWT 발급 |
| `ApplicationService` | `server/application` | Application CRD CRUD, 동기화, 리소스 조회 |
| `ApplicationSetService` | `server/applicationset` | ApplicationSet CRD 관리 |
| `ProjectService` | `server/project` | AppProject CRD 관리, 역할/토큰 관리 |
| `SettingsService` | `server/settings` | argocd-cm 설정 조회/수정 |
| `AccountService` | `server/account` | 로컬 계정 관리, 비밀번호 변경, API 키 |
| `NotificationService` | `server/notification` | 알림 템플릿/트리거 조회 |
| `CertificateService` | `server/certificate` | 레포지토리 TLS 인증서 관리 |
| `GpgkeyService` | `server/gpgkey` | GPG 공개 키 등록/삭제 (커밋 서명 검증용) |
| `VersionService` | `server/version` | Argo CD 버전 정보, 클라이언트 호환성 정보 |

### newArgoCDServiceSet() — 서비스 초기화

```go
// server/server.go:1023-1112
func newArgoCDServiceSet(a *ArgoCDServer) *ArgoCDServiceSet {
    kubectl := kubeutil.NewKubectl()
    // 모든 서비스는 공유 의존성(db, enf, cache 등)을 주입받음
    clusterService := cluster.NewServer(a.db, a.enf, a.Cache, kubectl)
    repoService := repository.NewServer(a.RepoClientset, a.db, a.enf, a.Cache,
        a.appLister, a.projInformer, a.Namespace, a.settingsMgr, a.HydratorEnabled)
    // ...

    // Application과 ApplicationSet 서비스는 projectLock을 공유
    // — 동일 프로젝트에 대한 동시 쓰기 직렬화
    projectLock := sync.NewKeyLock()
    applicationService, appResourceTreeFn := application.NewServer(
        a.Namespace, a.KubeClientset, a.AppClientset, a.appLister, a.appInformer,
        nil, a.RepoClientset, a.Cache, kubectl, a.db, a.enf,
        projectLock, a.settingsMgr, a.projInformer,
        a.ApplicationNamespaces, a.EnableK8sEvent, a.SyncWithReplaceAllowed,
    )
    // ...
}
```

---

## 8. Application gRPC 서비스

`server/application/application.go`에 구현된 `Server` 구조체가 `ApplicationServiceServer` 인터페이스를 구현한다.

### Server 구조체

```go
// server/application/application.go:88-106
type Server struct {
    ns                     string
    kubeclientset          kubernetes.Interface
    appclientset           appclientset.Interface
    appLister              applisters.ApplicationLister
    appInformer            cache.SharedIndexInformer
    appBroadcaster         broadcast.Broadcaster[v1alpha1.ApplicationWatchEvent]
    repoClientset          apiclient.Clientset
    kubectl                kube.Kubectl
    db                     db.ArgoDB
    enf                    *rbac.Enforcer
    projectLock            sync.KeyLock
    auditLogger            *argo.AuditLogger
    settingsMgr            *settings.SettingsManager
    cache                  *servercache.Cache
    projInformer           cache.SharedIndexInformer
    enabledNamespaces      []string
    syncWithReplaceAllowed bool
}
```

### Create() — 애플리케이션 생성

```go
// server/application/application.go:346-425
func (s *Server) Create(ctx context.Context, q *application.ApplicationCreateRequest) (*v1alpha1.Application, error) {
    a := q.GetApplication()

    // 1. RBAC 권한 검사 (create 액션)
    if err := s.enf.EnforceErr(ctx.Value("claims"), rbac.ResourceApplications,
        rbac.ActionCreate, a.RBACName(s.ns)); err != nil {
        return nil, err
    }

    // 2. 프로젝트 락 획득 (같은 프로젝트에 대한 동시 변경 직렬화)
    s.projectLock.RLock(a.Spec.GetProject())
    defer s.projectLock.RUnlock(a.Spec.GetProject())

    // 3. 앱 검증 및 정규화 (레포지토리 접근성, 목적지 클러스터, 프로젝트 권한)
    proj, err := s.getAppProject(ctx, a, log.WithFields(applog.GetAppLogFields(a)))
    err = s.validateAndNormalizeApp(ctx, a, proj, validate)

    // 4. Operation 필드 강제 제거 (보안: 생성 시 직접 sync 방지)
    if a.Operation != nil {
        a.Operation = nil  // branch protection 우회 방지
    }

    // 5. K8s API로 Application CRD 생성
    created, err := s.appclientset.ArgoprojV1alpha1().Applications(appNs).Create(ctx, a, metav1.CreateOptions{})
    if err == nil {
        s.logAppEvent(ctx, created, argo.EventReasonResourceCreated, "created application")
        s.waitSync(created)
        return created, nil
    }

    // 6. 이미 존재하는 경우: upsert 플래그에 따라 업데이트 또는 에러
    if apierrors.IsAlreadyExists(err) {
        existing, _ := s.appLister.Applications(appNs).Get(a.Name)
        if equalSpecs {
            return existing, nil  // 스펙이 동일하면 idempotent 처리
        }
        if !*q.Upsert {
            return nil, status.Errorf(codes.InvalidArgument, "existing application spec is different...")
        }
        // upsert: update 권한 재확인 후 업데이트
        s.enf.EnforceErr(ctx.Value("claims"), rbac.ResourceApplications, rbac.ActionUpdate, ...)
        return s.updateApp(ctx, existing, a, true)
    }
}
```

### Get() — 클라이언트 직접 GET (informer 우회)

```go
// server/application/application.go:771-787
// We must use a client Get instead of an informer Get, because it's common to call Get
// immediately following a Watch (which is not yet powered by an informer), and the Get
// must reflect what was previously seen by the client.
func (s *Server) Get(ctx context.Context, q *application.ApplicationQuery) (*v1alpha1.Application, error) {
    // informer 캐시(appLister)가 아닌 K8s API를 직접 호출
    a, proj, err := s.getApplicationEnforceRBACClient(ctx, rbac.ActionGet,
        project, appNs, appName, q.GetResourceVersion())
    // ...
}
```

Watch API 이후 즉시 Get을 호출할 때 인포머 캐시가 아직 동기화되지 않았을 수 있으므로, Get은 항상 K8s API를 직접 조회한다.

### Sync() — 동기화 트리거

```go
// server/application/application.go:2017-2133
func (s *Server) Sync(ctx context.Context, syncReq *application.ApplicationSyncRequest) (*v1alpha1.Application, error) {
    a, proj, err := s.getApplicationEnforceRBACClient(ctx, rbac.ActionGet, ...)

    // 1. SyncWindow 체크 — 동기화 허용 시간 창 검사
    canSync, err := proj.Spec.SyncWindows.Matches(a).CanSync(true)
    if !canSync {
        return a, status.Errorf(codes.PermissionDenied, "cannot sync: blocked by sync window")
    }

    // 2. sync 권한 검사
    if err := s.enf.EnforceErr(ctx.Value("claims"), rbac.ResourceApplications,
        rbac.ActionSync, a.RBACName(s.ns)); err != nil {
        return nil, err
    }

    // 3. 로컬 매니페스트 사용 시 override 권한 추가 필요
    if syncReq.Manifests != nil {
        s.enf.EnforceErr(ctx.Value("claims"), rbac.ResourceApplications,
            rbac.ActionOverride, a.RBACName(s.ns))
    }

    // 4. Operation 객체 구성
    op := v1alpha1.Operation{
        Sync: &v1alpha1.SyncOperation{
            Source:       source,
            Revision:     revision,
            Prune:        syncReq.GetPrune(),
            DryRun:       syncReq.GetDryRun(),
            SyncOptions:  syncOptions,
            SyncStrategy: syncReq.Strategy,
            Resources:    resources,
        },
        InitiatedBy: v1alpha1.OperationInitiator{Username: session.Username(ctx)},
    }

    // 5. K8s Patch로 Application.Operation 필드 설정
    // App Controller가 이를 감지해 실제 동기화를 수행
    a, err = argo.SetAppOperation(appIf, appName, &op)
}
```

Sync API는 실제 동기화를 직접 수행하지 않는다. Operation 필드를 설정하면 App Controller(argocd-application-controller)가 informer를 통해 변경을 감지하고 동기화를 실행한다.

### Watch() — SSE 스트리밍

```go
// server/application/application.go:1230-1298
func (s *Server) Watch(q *application.ApplicationQuery,
    ws application.ApplicationService_WatchServer) error {

    // 1. 초기 상태 전송 (resourceVersion 없을 때)
    if q.GetResourceVersion() == "" || q.GetName() != "" {
        apps, _ := s.appLister.List(selector)
        for i := range apps {
            sendIfPermitted(*apps[i], watch.Added)
        }
    }

    // 2. broadcaster 구독 (버퍼 크기: watchAPIBufferSize = 1000)
    events := make(chan *v1alpha1.ApplicationWatchEvent, watchAPIBufferSize)
    unsubscribe := s.appBroadcaster.Subscribe(events)
    defer unsubscribe()

    // 3. 이벤트 스트리밍
    for {
        select {
        case event := <-events:
            sendIfPermitted(event.Application, event.Type)
        case <-ws.Context().Done():
            return nil
        }
    }
}
```

`appBroadcaster`는 `broadcast.Broadcaster`인터페이스를 구현하며, `appInformer`의 이벤트 핸들러로 등록되어 Application 생성/수정/삭제 이벤트를 모든 구독자에게 브로드캐스트한다.

```go
// server/application/application.go:128-140
appBroadcaster = broadcast.NewHandler[v1alpha1.Application, v1alpha1.ApplicationWatchEvent](
    func(app *v1alpha1.Application, eventType watch.EventType) *v1alpha1.ApplicationWatchEvent {
        return &v1alpha1.ApplicationWatchEvent{Application: *app, Type: eventType}
    },
    applog.GetAppLogFields,
)
_, err := appInformer.AddEventHandler(appBroadcaster)
```

### getAppEnforceRBAC() — 타이밍 어택 방지

```go
// server/application/application.go:174-250
func (s *Server) getAppEnforceRBAC(ctx context.Context, action, project, namespace, name string,
    getApp func() (*v1alpha1.Application, error)) (*v1alpha1.Application, *v1alpha1.AppProject, error) {

    if project != "" {
        // 초기 RBAC 검사
        if err := s.enf.EnforceErr(ctx.Value("claims"), rbac.ResourceApplications,
            action, givenRBACName); err != nil {
            // 권한 없을 때: 앱이 존재하는지 여부를 숨기기 위해 동일한 코드 경로 실행
            // 응답 시간으로 존재 여부를 추론할 수 없도록 함
            _, _ = getApp()  // 타이밍 일치를 위해 실행
            return nil, nil, argocommon.PermissionDeniedAPIError
        }
    }

    a, err := getApp()
    if err != nil {
        if apierrors.IsNotFound(err) {
            if project != "" {
                return nil, nil, status.Error(codes.NotFound, ...)
            }
            // project 미지정 시: 앱이 없어도 403 반환 (404 vs 403으로 존재 추론 방지)
            return nil, nil, argocommon.PermissionDeniedAPIError
        }
    }

    // 두 번째 RBAC 검사: 실제 앱의 프로젝트로 재검사
    if err := s.enf.EnforceErr(ctx.Value("claims"), rbac.ResourceApplications,
        action, a.RBACName(s.ns)); err != nil {
        if project != "" {
            // project 지정 시: 앱이 있지만 다른 프로젝트에 있는 경우 404 반환
            // (앱 존재 여부를 숨김)
            return nil, nil, status.Error(codes.NotFound, ...)
        }
        return nil, nil, argocommon.PermissionDeniedAPIError
    }
}
```

보안 설계 원칙:
1. 사용자가 project를 지정하지 않은 경우: 앱이 없어도, 권한이 없어도 항상 403 반환 → 앱 존재 열거(enumeration) 방지
2. RBAC 실패 시 `getApp()`을 실행하여 응답 시간을 일치시킴 → 타이밍 사이드채널 방지
3. 두 번의 RBAC 검사: 요청 파라미터 기반 → 실제 앱의 프로젝트 기반

### updateApp() — 낙관적 동시성

```go
// server/application/application.go:1000-1030
func (s *Server) updateApp(ctx context.Context, app, newApp *v1alpha1.Application,
    merge bool) (*v1alpha1.Application, error) {
    for range 10 {  // 최대 10회 재시도
        app.Spec = newApp.Spec
        // merge 모드: 레이블/어노테이션을 덮어쓰지 않고 병합
        if merge {
            app.Labels = collections.Merge(app.Labels, newApp.Labels)
            app.Annotations = collections.Merge(app.Annotations, newApp.Annotations)
        } else {
            app.Labels = newApp.Labels
            app.Annotations = newApp.Annotations
        }

        res, err := s.appclientset.ArgoprojV1alpha1().Applications(app.Namespace).Update(
            ctx, app, metav1.UpdateOptions{})
        if err == nil {
            s.waitSync(res)
            return res, nil
        }

        // 409 Conflict: ResourceVersion 불일치 → 최신 버전 재조회 후 재시도
        if !apierrors.IsConflict(err) {
            return nil, err  // 다른 에러는 즉시 반환
        }

        // 최신 앱 상태 조회
        app, err = s.appclientset.ArgoprojV1alpha1().Applications(app.Namespace).Get(
            ctx, newApp.Name, metav1.GetOptions{})
    }
    return nil, status.Errorf(codes.Internal, "Failed to update application. Too many conflicts")
}
```

K8s의 낙관적 동시성 제어(Optimistic Concurrency Control)를 활용한다. ResourceVersion이 서버와 일치하지 않으면 409 Conflict가 반환되고, 클라이언트(API 서버)는 최신 버전을 다시 읽어 최대 10회까지 재시도한다.

### waitSync() — informer 동기화 대기

```go
// server/application/application.go:967-998
var informerSyncTimeout = 2 * time.Second

func (s *Server) waitSync(app *v1alpha1.Application) {
    deadline := time.Now().Add(informerSyncTimeout)  // 2초 타임아웃
    minVersion, _ := strconv.Atoi(app.ResourceVersion)

    for {
        if currApp, err := s.appLister.Applications(app.Namespace).Get(app.Name); err == nil {
            currVersion, err := strconv.Atoi(currApp.ResourceVersion)
            if err == nil && currVersion >= minVersion {
                return  // 인포머가 최신 버전을 반영함
            }
        }
        if time.Now().After(deadline) {
            break  // 2초 초과 시 경고 후 반환
        }
        time.Sleep(20 * time.Millisecond)  // 20ms 폴링
    }
    logCtx.Warnf("waitSync failed: timed out")
}
```

생성/수정 후 인포머 캐시가 최신 상태를 반영할 때까지 최대 2초 대기한다. 인포머는 K8s Watch API를 통해 비동기로 업데이트되므로, API 응답 직후 List/Get 요청 시 오래된 데이터가 반환될 수 있다. 이를 최소화하기 위해 20ms 간격으로 폴링한다.

---

## 9. 설정 감시 (watchSettings)

`server/server.go:795-884`의 `watchSettings()`는 ConfigMap/Secret 변경을 감시하고, 재시작이 필요한 변경이 발생하면 GracefulRestartSignal을 보낸다.

### 감시 항목 및 트리거

```go
func (server *ArgoCDServer) watchSettings() {
    updateCh := make(chan *settings_util.ArgoCDSettings, 1)
    server.settingsMgr.Subscribe(updateCh)  // argocd-cm 변경 구독

    // 이전 값 스냅샷
    prevURL := server.settings.URL
    prevOIDCConfig := server.settings.OIDCConfig()
    prevDexCfgBytes, _ := dexutil.GenerateDexConfigYAML(...)
    prevGitHubSecret := server.settings.GetWebhookGitHubSecret()
    // ... 기타 Webhook 시크릿들

    for {
        newSettings := <-updateCh
        server.settings = newSettings

        // 재시작 트리거 조건들
        if !bytes.Equal(newDexCfgBytes, prevDexCfgBytes) {
            log.Infof("dex config modified. restarting"); break
        }
        if checkOIDCConfigChange(prevOIDCConfig, server.settings) {
            log.Infof("oidc config modified. restarting"); break
        }
        if prevURL != server.settings.URL {
            log.Infof("url modified. restarting"); break
        }
        if prevGitHubSecret != server.settings.GetWebhookGitHubSecret() {
            log.Infof("github secret modified. restarting"); break
        }
        // ... 기타 Webhook 시크릿 변경 체크

        // 확장 설정 변경: 재시작 없이 동적 반영
        if !reflect.DeepEqual(prevExtConfig, server.settings.ExtensionConfig) {
            server.extensionManager.UpdateExtensionRegistry(server.settings)
        }

        // TLS 인증서 변경: GetCertificate 콜백으로 자동 반영 (재시작 불필요)
        if newCert != prevCert || newCertKey != prevCertKey {
            log.Infof("tls certificate modified. reloading certificate")
            // tls.Config.GetCertificate가 매 요청마다 호출되므로 자동 반영
        }
    }

    // GracefulRestartSignal: 현재 요청을 완료하고 재시작
    server.stopCh <- GracefulRestartSignal{}
}
```

### 변경 유형별 대응

| 변경 항목 | 대응 방식 |
|----------|----------|
| Dex 설정 변경 | GracefulRestartSignal → 프로세스 재시작 |
| OIDC 설정 변경 | GracefulRestartSignal → 프로세스 재시작 |
| URL 변경 | GracefulRestartSignal → 프로세스 재시작 |
| Webhook 시크릿 변경 | GracefulRestartSignal → 프로세스 재시작 |
| TLS 인증서 변경 | `GetCertificate` 콜백으로 자동 반영 (재시작 불필요) |
| 확장 설정 변경 | `UpdateExtensionRegistry()`로 동적 반영 |

### GracefulRestartSignal

```go
// server/server.go:265-286
type GracefulRestartSignal struct{}

func (g GracefulRestartSignal) String() string { return "GracefulRestartSignal" }
func (g GracefulRestartSignal) Signal() {}  // os.Signal 인터페이스 구현
```

```go
// server/server.go:745-757
select {
case signal := <-server.stopCh:
    gracefulRestartSignal := GracefulRestartSignal{}
    if signal != gracefulRestartSignal {
        server.terminateRequested.Store(true)  // 영구 종료
    }
    // GracefulRestartSignal이면 terminateRequested는 false
    // → 상위 시스템(Kubernetes)이 새 프로세스를 시작할 것으로 기대
    server.Shutdown()
}
```

OS 시그널(SIGTERM, SIGINT)과 달리 GracefulRestartSignal은 `terminateRequested`를 false로 남긴다. 이를 통해 Kubernetes가 롤링 재시작을 실행하도록 유도한다.

### rbacPolicyLoader()

```go
// server/server.go:886-901
func (server *ArgoCDServer) rbacPolicyLoader(ctx context.Context) {
    err := server.enf.RunPolicyLoader(ctx, func(cm *corev1.ConfigMap) error {
        var scopes []string
        if scopesStr, ok := cm.Data[rbac.ConfigMapScopesKey]; scopesStr != "" && ok {
            yaml.Unmarshal([]byte(scopesStr), &scopes)
        }
        server.policyEnforcer.SetScopes(scopes)
        return nil
    })
}
```

`RunPolicyLoader`는 `argocd-rbac-cm` ConfigMap을 주기적으로(기본 10분) 다시 읽어 RBAC 정책을 갱신한다. 정책 변경이 반영되기까지 최대 10분의 지연이 발생할 수 있다.

---

## 10. 왜 이런 설계인가

### cmux: 단일 포트 멀티플렉싱

**문제**: gRPC(HTTP/2)와 REST(HTTP/1.1)는 서로 다른 프로토콜이다. 일반적으로 별도 포트가 필요하다.

**해결**: `cmux`는 첫 몇 바이트를 읽어 프로토콜을 식별하고 적절한 서버로 분기한다.

**이점**:
- 방화벽 규칙 단순화 (포트 하나만 열면 됨)
- Ingress 설정 단순화
- TLS 종료를 API 서버가 직접 처리 (중간 프록시 불필요)

```
전통적 설계:
  :8080 → REST
  :8081 → gRPC
  방화벽에서 두 포트 모두 허용 필요

cmux 설계:
  :8080 → REST + gRPC + WebSocket 모두 처리
  방화벽에서 하나의 포트만 허용
```

### grpc-web: 브라우저 직접 gRPC

**문제**: 브라우저는 HTTP/2 gRPC를 직접 지원하지 않는다. 기존 해결책은 Envoy 프록시 또는 grpc-gateway였다.

**해결**: `improbable-eng/grpc-web`은 gRPC 메시지를 HTTP/1.1 요청으로 래핑한다. 브라우저는 `Content-Type: application/grpc-web+proto`로 요청하고, grpcWebHandler가 이를 gRPC로 변환한다.

**이점**:
- Envoy 프록시 없이 브라우저 UI에서 직접 gRPC 호출
- 별도 인프라 없이 gRPC의 스트리밍, protobuf 직렬화 이점 활용
- grpc-gateway(JSON 변환 오버헤드)보다 효율적

### 타이밍 어택 방지

**문제**: 권한 없는 사용자가 존재하지 않는 앱에 대해 요청할 때 에러 응답 시간의 차이로 앱 존재 여부를 추론할 수 있다.

```
앱 없음 → 빠른 응답 (DB 조회 없음) → "이 앱은 없구나"
앱 있음 + 권한 없음 → 느린 응답 (DB 조회 후 권한 검사) → "이 앱은 있구나"
```

**해결**: `getAppEnforceRBAC()`는 권한 검사 실패 시에도 `getApp()`을 호출하여 DB 조회를 수행한다. 모든 경우에 동일한 코드 경로를 실행하므로 응답 시간이 일치한다.

```go
if err := s.enf.EnforceErr(...); err != nil {
    _, _ = getApp()  // 타이밍 일치를 위해 DB 조회 실행
    return nil, nil, argocommon.PermissionDeniedAPIError
}
```

### sensitiveMethods: 자격증명 로그 방지

**문제**: 레포지토리 비밀번호, 클러스터 자격증명 등이 gRPC 페이로드 로그에 노출될 수 있다.

**해결**: `PayloadServerInterceptor`에 `sensitiveMethods` 맵을 전달한다. 해당 엔드포인트의 페이로드는 로그에 기록되지 않는다. 이는 규정 준수(GDPR, PCI-DSS 등)와 보안을 위한 필수 설계다.

### informer vs 클라이언트 직접 호출

Argo CD는 두 가지 읽기 경로를 사용한다.

| 상황 | 읽기 경로 | 이유 |
|------|----------|------|
| List(), Watch() | informer 캐시 (`appLister`) | 성능 — K8s API 부하 감소, O(1) 조회 |
| Get() | K8s 클라이언트 직접 호출 | 최신성 보장 — Watch 직후 Get 시 캐시 미반영 방지 |
| Create/Update 후 | `waitSync()` + 인포머 캐시 | 일관성 — 쓰기 후 읽기의 최종 일관성 근사 |

### 낙관적 동시성 (Optimistic Concurrency)

K8s의 ResourceVersion 기반 낙관적 잠금을 API 서버 레벨에서 처리한다. 409 Conflict 발생 시 최대 10회 재시도로 대부분의 동시 수정 충돌을 해결한다. 비관적 잠금(Pessimistic Lock) 대비:
- 잠금 획득/해제 오버헤드 없음
- 충돌이 드문 환경에서 높은 처리량
- 분산 환경에서 deadlock 없음

---

## 요약: API 서버 핵심 아키텍처

```
┌─────────────────────────────────────────────────────────────────────┐
│                        ArgoCDServer                                  │
│                                                                      │
│  ┌─────────────┐  ┌─────────────┐  ┌──────────────────────────────┐ │
│  │   cmux      │  │  gRPC 서버  │  │       HTTP 서버              │ │
│  │  포트 분기  │  │             │  │                              │ │
│  │             │  │ 인터셉터 체인│  │ grpc-gateway (REST 트랜스코딩)│ │
│  │ HTTP/1.1 ──►│  │ logging    │  │ grpc-web (브라우저 지원)     │ │
│  │ TLS+HTTP ──►│  │ metrics    │  │ /api/* → gwmux               │ │
│  │ TLS+gRPC ──►│  │ auth(JWT)  │  │ /terminal → exec handler     │ │
│  └─────────────┘  │ rbac       │  │ /api/webhook → git events    │ │
│                   │ payload    │  └──────────────────────────────┘ │
│                   │ recovery   │                                    │
│                   └─────────────┘                                   │
│                                                                      │
│  ┌──────────────────────────────────────────────────────────────┐   │
│  │                  ArgoCDServiceSet (13개 서비스)                │   │
│  │ Application│Cluster│Project│Repository│Session│Settings│...  │   │
│  └──────────────────────────────────────────────────────────────┘   │
│                                                                      │
│  ┌─────────────┐  ┌──────────────┐  ┌────────────────────────────┐  │
│  │ watchSettings│  │rbacPolicyLoader│  │      Authenticate()        │  │
│  │ (설정 변경 감시)│  │(정책 주기 갱신)│  │ metadata > Bearer > Cookie │  │
│  └─────────────┘  └──────────────┘  └────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────────┘
         │                    │                    │
         ▼                    ▼                    ▼
  K8s API Server         Repo Server          Redis Cache
  (Application CRD)      (매니페스트 생성)     (앱 상태/리소스 트리)
```

Argo CD API 서버는 단일 포트에서 gRPC, REST, WebSocket을 처리하는 단일 바이너리다. 보안을 위해 타이밍 어택 방지, 자격증명 로그 마스킹, 이중 RBAC 검사를 구현하고, 성능을 위해 informer 캐시와 낙관적 동시성 제어를 활용한다.
