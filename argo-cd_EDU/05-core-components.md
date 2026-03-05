# Argo CD 핵심 컴포넌트 심화 분석

## 목차

1. [API Server (argocd-server)](#1-api-server-argocd-server)
2. [Application Controller (argocd-application-controller)](#2-application-controller-argocd-application-controller)
3. [Repo Server (argocd-repo-server)](#3-repo-server-argocd-repo-server)
4. [ApplicationSet Controller](#4-applicationset-controller)
5. [GitOps Engine](#5-gitops-engine)
6. [Notification Controller](#6-notification-controller)
7. [컴포넌트 간 상호작용](#7-컴포넌트-간-상호작용)

---

## 1. API Server (argocd-server)

### 1.1 ArgoCDServer 구조체

`server/server.go`의 `ArgoCDServer`는 Argo CD의 프론트엔드 게이트웨이다. 모든 UI, CLI, gRPC 요청이 이 서버를 통해 들어온다.

```go
// server/server.go:185
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

**주요 필드 설명:**

| 필드 | 타입 | 역할 |
|------|------|------|
| `sessionMgr` | `*util_session.SessionManager` | JWT 토큰 생성·검증 |
| `settingsMgr` | `*settings_util.SettingsManager` | argocd-cm ConfigMap 관리 |
| `enf` | `*rbac.Enforcer` | Casbin 기반 RBAC 권한 검사 |
| `policyEnforcer` | `*rbacpolicy.RBACPolicyEnforcer` | 프로젝트별 정책 적용 |
| `serviceSet` | `*ArgoCDServiceSet` | 13개 gRPC 서비스 묶음 |
| `extensionManager` | `*extension.Manager` | 프록시 익스텐션 관리 |
| `terminateRequested` | `atomic.Bool` | 우아한 종료 플래그 |
| `available` | `atomic.Bool` | 헬스체크 가용성 플래그 |

### 1.2 cmux 기반 포트 멀티플렉싱

Argo CD는 단일 포트(기본 8080/8443)에서 HTTP/1.1, HTTPS, gRPC를 동시에 처리한다. 이를 위해 `soheilhy/cmux` 라이브러리를 사용한다.

```
클라이언트 요청 → 단일 TCP 리스너
        │
        ▼
    cmux.New(listeners.Main)
        │
        ├─ cmux.HTTP1Fast("PATCH")  → HTTP/1.1 리스너 (httpL)
        │
        ├─ cmux.HTTP2MatchHeaderFieldSendSettings(  → gRPC 리스너 (grpcL)
        │      "content-type", "application/grpc")
        │
        └─ cmux.Any()  → TLS 리스너 (tlsl)
                │
                └─ 내부 TLS cmux
                        ├─ HTTP1Fast("PATCH") → HTTPS 리스너
                        └─ HTTP2MatchHeader   → gRPC over TLS
```

```go
// server/server.go:622
tcpm := cmux.New(listeners.Main)
var tlsm cmux.CMux
// TLS 모드가 아닐 때:
httpL = tcpm.Match(cmux.HTTP1Fast("PATCH"))
grpcL = tcpm.MatchWithWriters(
    cmux.HTTP2MatchHeaderFieldSendSettings("content-type", "application/grpc"))

// TLS 모드일 때 (기본):
tlsl := tcpm.Match(cmux.Any())
tlsm = cmux.New(tlsl)
httpsL = tlsm.Match(cmux.HTTP1Fast("PATCH"))
grpcL = tlsm.MatchWithWriters(
    cmux.HTTP2MatchHeaderFieldSendSettings("content-type", "application/grpc"))
```

**왜 cmux를 사용하는가?** 클라이언트(UI, CLI, 외부 시스템)가 프로토콜별로 다른 포트에 접속하는 대신 단일 포트로 통합하면, 방화벽 규칙이 단순해지고 Ingress 설정이 하나로 통일된다. 또한 TLS 핸드셰이크를 gRPC 서버가 아닌 cmux 레벨에서 처리하므로, gRPC 서버 설정에서 `grpc.Creds(creds)`를 별도로 구성할 필요가 없다.

### 1.3 gRPC 인터셉터 체인

`newGRPCServer()`에서 스트림·유니어리 인터셉터를 체인으로 구성한다:

```go
// server/server.go:955
sOpts = append(sOpts, grpc.ChainStreamInterceptor(
    logging.StreamServerInterceptor(grpc_util.InterceptorLogger(server.log)),  // 1. 로깅
    serverMetrics.StreamServerInterceptor(),                                    // 2. Prometheus 메트릭
    grpc_auth.StreamServerInterceptor(server.Authenticate),                    // 3. 인증
    grpc_util.UserAgentStreamServerInterceptor(...),                            // 4. User-Agent 검사
    grpc_util.PayloadStreamServerInterceptor(server.log, true, func(...) bool {
        return !sensitiveMethods[c.FullMethod()]                                // 5. 페이로드 로깅 (민감 메서드 제외)
    }),
    grpc_util.ErrorCodeK8sStreamServerInterceptor(),                           // 6. K8s 에러코드 변환
    grpc_util.ErrorCodeGitStreamServerInterceptor(),                           // 6. Git 에러코드 변환
    recovery.StreamServerInterceptor(...),                                      // 7. 패닉 복구
))
```

**인터셉터 실행 순서 (요청 방향 기준):**

```
요청 →  [1.logging] → [2.metrics] → [3.auth] → [4.userAgent] → [5.payload] → [6.errorCode] → [7.recovery] → 핸들러
응답 ←  [1.logging] ← [2.metrics] ← [3.auth] ← [4.userAgent] ← [5.payload] ← [6.errorCode] ← [7.recovery] ← 핸들러
```

**인터셉터별 역할:**

| 순서 | 인터셉터 | 역할 |
|------|----------|------|
| 1 | `logging` | 모든 gRPC 호출 로깅 (go-grpc-middleware v2) |
| 2 | `serverMetrics` | Prometheus 히스토그램·카운터 업데이트 |
| 3 | `grpc_auth.Authenticate` | JWT 토큰 검증 및 claims 컨텍스트 주입 |
| 4 | `UserAgent` | 클라이언트 버전 호환성 검사 |
| 5 | `PayloadLogging` | 요청/응답 본문 로깅 (민감 메서드 제외) |
| 6 | `ErrorCodeK8s/Git` | K8s/Git 에러를 gRPC 상태코드로 변환 |
| 7 | `recovery` | 핸들러 패닉을 500 에러로 변환 |

### 1.4 ArgoCDServiceSet — 13개 gRPC 서비스

```go
// server/server.go:1006
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

**서비스 목록 및 역할:**

| 서비스 | 패키지 | 주요 기능 |
|--------|--------|-----------|
| `ClusterService` | `server/cluster` | 클러스터 CRUD, 연결 상태 |
| `RepoService` | `server/repository` | Git 리포지터리 관리 |
| `RepoCredsService` | `server/repocreds` | 리포지터리 자격증명 템플릿 |
| `SessionService` | `server/session` | 로그인/로그아웃, 토큰 발급 |
| `ApplicationService` | `server/application` | Application CRUD, 동기화, 로그 |
| `ApplicationSetService` | `server/applicationset` | ApplicationSet CRUD |
| `ProjectService` | `server/project` | AppProject CRUD, 역할 관리 |
| `SettingsService` | `server/settings` | 전역 설정 조회 |
| `AccountService` | `server/account` | 로컬 사용자 계정 관리 |
| `NotificationService` | `server/notification` | 알림 설정 조회 |
| `CertificateService` | `server/certificate` | TLS 인증서 관리 |
| `GpgkeyService` | `server/gpgkey` | GPG 공개키 관리 |
| `VersionService` | `server/version` | 버전 정보 |

### 1.5 newGRPCServer() 주요 설정

```go
// server/server.go:918
sOpts := []grpc.ServerOption{
    // 송수신 메시지 크기 상한: 200MB (환경변수 ARGOCD_GRPC_MAX_SIZE_MB로 조정 가능)
    grpc.MaxRecvMsgSize(apiclient.MaxGRPCMessageSize),
    grpc.MaxSendMsgSize(apiclient.MaxGRPCMessageSize),
    // 연결 타임아웃: 300초
    grpc.ConnectionTimeout(300 * time.Second),
    // Keepalive 최소 간격
    grpc.KeepaliveEnforcementPolicy(
        keepalive.EnforcementPolicy{
            MinTime: common.GetGRPCKeepAliveEnforcementMinimum(),
        },
    ),
}
```

> **왜 200MB인가?** Application의 리소스 목록이 많을 경우, 특히 `GetManifestsWithFiles` 같은 대용량 스트림 요청에서 단일 메시지가 클 수 있다. gRPC 기본값(4MB)으로는 대규모 클러스터의 리소스 트리를 한 번에 전달하기 어렵다. 적절한 해결책은 페이지네이션이지만, 그 구현이 완료되기 전 임시 대안으로 큰 제한값을 설정한다(코드 주석 참조).

### 1.6 sensitiveMethods — 민감 메서드 페이로드 마스킹

아래 19개 엔드포인트는 페이로드 로깅에서 제외된다(자격증명, 비밀번호, 대용량 파일 포함 가능):

```go
// server/server.go:931
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
    "/application.ApplicationService/GetManifestsWithFiles":        true,
}
```

### 1.7 Authenticate() 인터셉터 — 인증 흐름

```
gRPC 요청
    │
    ▼
Authenticate(ctx)
    │
    ├─ DisableAuth == true → ctx 그대로 반환
    │
    ▼
getClaims(ctx)
    │
    ├─ metadata에서 토큰 추출 (MetaDataTokenKey 또는 Authorization: Bearer)
    │
    ├─ sessionMgr.VerifyToken(ctx, tokenString)
    │       │
    │       ├─ Argo CD 자체 발급 토큰 → JWT 검증 + 만료 임박 시 자동 갱신
    │       └─ OIDC 토큰 → Dex/외부 IdP 검증
    │
    ├─ SSO 설정 시 → ssoClientApp.SetGroupsFromUserInfo()
    │               └─ OIDC 토큰 자동 갱신 (OIDCRefreshTokenThreshold)
    │
    └─ context.WithValue(ctx, "claims", claims)  ← RBAC 검사에 사용
```

```go
// server/server.go:1521
func (server *ArgoCDServer) Authenticate(ctx context.Context) (context.Context, error) {
    if server.DisableAuth {
        return ctx, nil
    }
    claims, newToken, claimsErr := server.getClaims(ctx)
    if claims != nil {
        ctx = context.WithValue(ctx, "claims", claims)
        if newToken != "" {
            // 갱신된 토큰을 grpc 메타데이터로 전달 → grpc-gateway가 Set-Cookie로 변환
            grpc.SendHeader(ctx, metadata.New(map[string]string{renewTokenKey: newToken}))
        }
    }
    if claimsErr != nil {
        if !argoCDSettings.AnonymousUserEnabled {
            return ctx, claimsErr  // 익명 사용자 비허용 시 에러 반환
        }
        ctx = context.WithValue(ctx, "claims", "")  // 익명 사용자
    }
    return ctx, nil
}
```

---

## 2. Application Controller (argocd-application-controller)

### 2.1 ApplicationController 구조체 (2,697줄)

`controller/appcontroller.go`의 `ApplicationController`는 GitOps의 핵심 조정 루프를 구현한다. 원하는 상태(Git)와 실제 상태(K8s 클러스터)를 지속적으로 비교하고 동기화한다.

```go
// controller/appcontroller.go:106
type ApplicationController struct {
    cache                *appstatecache.Cache
    namespace            string
    kubeClientset        kubernetes.Interface
    kubectl              kube.Kubectl
    applicationClientset appclientset.Interface
    auditLogger          *argo.AuditLogger

    // 5개 워크큐
    appRefreshQueue               workqueue.TypedRateLimitingInterface[string]
    appComparisonTypeRefreshQueue workqueue.TypedRateLimitingInterface[string]
    appOperationQueue             workqueue.TypedRateLimitingInterface[string]
    projectRefreshQueue           workqueue.TypedRateLimitingInterface[string]
    appHydrateQueue               workqueue.TypedRateLimitingInterface[string]
    hydrationQueue                workqueue.TypedRateLimitingInterface[hydratortypes.HydrationQueueKey]

    appInformer   cache.SharedIndexInformer
    appLister     applisters.ApplicationLister
    projInformer  cache.SharedIndexInformer

    appStateManager  AppStateManager
    stateCache       statecache.LiveStateCache

    statusRefreshTimeout      time.Duration
    statusHardRefreshTimeout  time.Duration
    statusRefreshJitter       time.Duration
    selfHealTimeout           time.Duration
    selfHealBackoff           *wait.Backoff
    syncTimeout               time.Duration

    db                    db.ArgoDB
    settingsMgr           *settings_util.SettingsManager
    refreshRequestedApps  map[string]CompareWith
    refreshRequestedAppsMutex *sync.Mutex
    metricsServer         *metrics.MetricsServer
    kubectlSemaphore      *semaphore.Weighted
    clusterSharding       sharding.ClusterShardingCache
    hydrator              *hydrator.Hydrator
}
```

### 2.2 워크큐 아키텍처

Application Controller는 6개의 분리된 워크큐를 사용하여 작업을 병렬 처리한다:

```
                    ┌─────────────────────────────────────────────────────────┐
                    │               ApplicationController 워크큐               │
                    └─────────────────────────────────────────────────────────┘
                         │              │              │             │
              ┌──────────┼──────────────┼──────────────┼─────────────┼──────────┐
              ▼          ▼              ▼              ▼             ▼          ▼
    appRefresh   appComparison    appOperation   projectRefresh  appHydrate  hydration
      Queue       TypeRefreshQ      Queue           Queue          Queue      Queue
         │              │              │              │             │
         │  비교 타입    │  sync/delete │  프로젝트    │  CMP/hydra  │
         │  지정 리프레시│  작업 실행   │  조건 갱신   │  작업       │
         ▼              ▼              ▼              ▼             ▼
  processAppRefresh  processAppComp  processAppOp  processProj  processAppHydrate
  QueueItem()       TypeQueueItem() QueueItem()   QueueItem()  QueueItem()
```

**각 큐의 역할:**

| 워크큐 | 처리 함수 | 역할 |
|--------|-----------|------|
| `appRefreshQueue` | `processAppRefreshQueueItem()` | 앱 상태 비교·갱신 (주 조정 루프) |
| `appComparisonTypeRefreshQueue` | `processAppComparisonTypeQueueItem()` | 비교 레벨 지정 리프레시 요청 |
| `appOperationQueue` | `processAppOperationQueueItem()` | Sync/삭제 작업 실행 |
| `projectRefreshQueue` | `processProjectQueueItem()` | 프로젝트 조건 갱신 |
| `appHydrateQueue` | `processAppHydrateQueueItem()` | Hydrator(Render 단계) 처리 |
| `hydrationQueue` | `processHydrationQueueItem()` | 실제 hydration 작업 수행 |

### 2.3 CompareWith 레벨

```go
// controller/appcontroller.go:84
type CompareWith int

const (
    // 비교 없이 리소스 트리만 갱신
    ComparisonWithNothing CompareWith = 0
    // 가장 최근 비교에서 사용한 리비전으로 비교
    CompareWithRecent CompareWith = 1
    // 최신 Git 리비전과 비교 (리비전 캐시 활용)
    CompareWithLatest CompareWith = 2
    // 최신 Git 리비전과 비교 (리비전 캐시 강제 갱신)
    CompareWithLatestForceResolve CompareWith = 3
)
```

**레벨별 사용 시나리오:**

| 레벨 | 값 | 트리거 조건 |
|------|----|-------------|
| `ComparisonWithNothing` | 0 | 리소스 트리만 갱신이 필요한 경우 |
| `CompareWithRecent` | 1 | 컨트롤러 내부 요청 (특정 레벨 지정) |
| `CompareWithLatest` | 2 | 타임아웃 만료, 스펙 변경 감지 |
| `CompareWithLatestForceResolve` | 3 | 사용자 명시적 리프레시, `spec.source` 변경 |

### 2.4 Run() — 컨트롤러 실행 흐름

```go
// controller/appcontroller.go:887
func (ctrl *ApplicationController) Run(ctx context.Context, statusProcessors int, operationProcessors int) {
    // 1. 클러스터 샤딩 초기화
    ctrl.clusterSharding.Init(clusters, appItems)

    // 2. 인포머 시작
    go ctrl.appInformer.Run(ctx.Done())
    go ctrl.projInformer.Run(ctx.Done())

    // 3. 클러스터 상태 캐시 초기화 (Watch API로 K8s 리소스 감시 시작)
    errors.CheckError(ctrl.stateCache.Init())

    // 4. 캐시 동기화 대기
    cache.WaitForCacheSync(ctx.Done(), ctrl.appInformer.HasSynced, ctrl.projInformer.HasSynced)

    // 5. 상태 캐시 실행 (goroutine)
    go func() { errors.CheckError(ctrl.stateCache.Run(ctx)) }()

    // 6. 상태 프로세서 goroutine pool (기본 20개)
    for range statusProcessors {
        go wait.Until(func() {
            for ctrl.processAppRefreshQueueItem() {}
        }, time.Second, ctx.Done())
    }

    // 7. 작업 프로세서 goroutine pool (기본 10개)
    for range operationProcessors {
        go wait.Until(func() {
            for ctrl.processAppOperationQueueItem() {}
        }, time.Second, ctx.Done())
    }

    // 8. 비교 타입 큐, 프로젝트 큐, 하이드레이션 큐 처리
    go wait.Until(func() { for ctrl.processAppComparisonTypeQueueItem() {} }, time.Second, ctx.Done())
    go wait.Until(func() { for ctrl.processProjectQueueItem() {} }, time.Second, ctx.Done())

    <-ctx.Done()
}
```

```
Run() 시작
    │
    ├─ 1. clusterSharding.Init()    ← 컨트롤러 샤딩 (여러 인스턴스 운용 시)
    ├─ 2. informer.Run()            ← K8s Watch API 시작
    ├─ 3. stateCache.Init()         ← 클러스터 리소스 초기 로드 (동기)
    ├─ 4. WaitForCacheSync()        ← 인포머 동기화 대기
    ├─ 5. stateCache.Run()          ← 클러스터 리소스 변경 지속 감시 (비동기)
    ├─ 6. statusProcessors goroutines   ← appRefreshQueue 처리
    └─ 7. operationProcessors goroutines ← appOperationQueue 처리
```

### 2.5 processAppRefreshQueueItem() — 주 조정 루프

```
appRefreshQueue에서 키 꺼내기 (namespace/name)
    │
    ▼
informer에서 Application 객체 조회
    │
    ▼
needRefreshAppStatus() 호출
    │
    ├─ needRefresh == false → 큐에서 완료 처리 후 종료
    │
    ▼ needRefresh == true
    │
    ├─ comparisonLevel == ComparisonWithNothing
    │       └─ 캐시에서 리소스 트리만 갱신 → persistAppStatus() → 종료
    │
    ▼
refreshAppConditions()   ← AppProject 검증
    │
    ├─ 에러 조건 있음 → persistAppStatus() → 종료
    │
    ▼
CompareAppState()        ← Repo Server 호출 → K8s 비교
    │
    ▼
setOperationState()      ← 이전 작업 완료 상태 처리
    │
    ▼
autoSync()               ← 자동 동기화 조건 검사
    │
    ▼
persistAppStatus()       ← Application.Status 패치
    │
    ▼
appOperationQueue.AddRateLimited()  ← 작업 큐에 추가 (항상)
```

### 2.6 needRefreshAppStatus() — 리프레시 필요 여부 판단

```go
// controller/appcontroller.go:2011
func (ctrl *ApplicationController) needRefreshAppStatus(...) (bool, appv1.RefreshType, CompareWith) {
    // 1. 사용자 명시적 리프레시 요청 확인
    if requestedType, ok := app.IsRefreshRequested(); ok {
        compareWith = CompareWithLatestForceResolve
        reason = fmt.Sprintf("%s refresh requested", refreshType)
    } else {
        // 2. spec.source 변경 확인
        if !currentSourceEqualsSyncedSource(app) {
            reason = "spec.source differs"
            compareWith = CompareWithLatestForceResolve
        // 3. 하드/소프트 타임아웃 만료
        } else if hardExpired || softExpired {
            ...
        // 4. spec.destination 변경
        } else if !reflect.DeepEqual(app.Spec.Destination, app.Status.Sync.ComparedTo.Destination) {
            reason = "spec.destination differs"
        // 5. ignoreDifferences 변경
        } else if !app.Spec.IgnoreDifferences.Equals(app.Status.Sync.ComparedTo.IgnoreDifferences) {
            reason = "spec.ignoreDifferences differs"
        // 6. 컨트롤러 내부 리프레시 요청
        } else if requested, level := ctrl.isRefreshRequested(app.QualifiedName()); requested {
            compareWith = level
            reason = "controller refresh requested"
        }
    }
}
```

### 2.7 autoSync() — 자동 동기화 가드 조건

`autoSync()`는 다음 조건 중 하나라도 해당되면 자동 동기화를 건너뛴다:

```
autoSync() 진입
    │
    ├─ SyncPolicy == nil OR !IsAutomatedSyncEnabled() → 건너뜀
    │
    ├─ app.Operation != nil → "operation in progress" → 건너뜀
    │
    ├─ DeletionTimestamp != nil → "deletion in progress" → 건너뜀
    │
    ├─ syncStatus != OutOfSync → "already synced" → 건너뜀
    │
    ├─ Prune == false AND 프루닝만 필요한 경우 → 건너뜀
    │
    ├─ alreadyAttempted == true
    │       │
    │       ├─ 이전 sync 실패 → SyncError 조건 설정 → 건너뜀
    │       │
    │       └─ SelfHeal == false → "already synced to revision" → 건너뜀
    │               │
    │               └─ SelfHeal == true
    │                       │
    │                       ├─ selfHealRemainingBackoff() > 0 → 지연 리프레시 요청 → 건너뜀
    │                       │
    │                       └─ SelfHealAttemptsCount++ → 동기화 진행
    │
    ├─ AllowEmpty == false AND 모든 리소스가 프루닝 대상 → 건너뜀
    │
    └─ setOperationState() → 실제 Sync 작업 시작
```

```go
// controller/appcontroller.go:2225
op := appv1.Operation{
    Sync: &appv1.SyncOperation{
        Source:    source,
        Revision:  syncStatus.Revision,
        Prune:     app.Spec.SyncPolicy.Automated.Prune,
        SyncOptions: app.Spec.SyncPolicy.SyncOptions,
    },
    InitiatedBy: appv1.OperationInitiator{Automated: true},
    Retry:       appv1.RetryStrategy{Limit: 5},
}
```

### 2.8 selfHeal 백오프 메커니즘

Self-heal은 클러스터에서 누군가 리소스를 수동 수정했을 때 Git 상태로 되돌리는 기능이다. 무한 루프를 방지하기 위해 백오프 전략을 사용한다:

```go
// controller/appcontroller.go:2363
func (ctrl *ApplicationController) selfHealRemainingBackoff(app *appv1.Application, selfHealAttemptsCount int) time.Duration {
    if ctrl.selfHealBackoff == nil {
        // 단순 타임아웃: selfHealTimeout에서 마지막 작업 후 경과 시간을 뺀 값
        retryAfter = ctrl.selfHealTimeout - *timeSinceOperation
    } else {
        // 지수 백오프: SelfHealAttemptsCount만큼 Step() 호출
        backOff := *ctrl.selfHealBackoff
        backOff.Steps = selfHealAttemptsCount
        delay = backOff.Step()  // 시도 횟수에 따라 증가하는 지연
        retryAfter = delay - *timeSinceOperation
    }
    return retryAfter
}
```

`SelfHealAttemptsCount`는 `op.Sync.SelfHealAttemptsCount`로 누적되어, 시도 횟수가 늘어날수록 대기 시간이 길어진다.

---

## 3. Repo Server (argocd-repo-server)

### 3.1 Service 구조체

`reposerver/repository/repository.go`의 `Service`는 Git/Helm/OCI 리포지터리에서 매니페스트를 생성하는 역할을 담당한다. Application Controller와 gRPC로 통신한다.

```go
// reposerver/repository/repository.go:82
type Service struct {
    gitCredsStore             git.CredsStore
    rootDir                   string
    gitRepoPaths              utilio.TempPaths     // Git 체크아웃 임시 경로 (랜덤화)
    chartPaths                utilio.TempPaths     // Helm 차트 임시 경로
    ociPaths                  utilio.TempPaths     // OCI 이미지 임시 경로
    gitRepoInitializer        func(rootPath string) goio.Closer
    repoLock                  *repositoryLock       // Git 리포 접근 직렬화
    cache                     *cache.Cache
    parallelismLimitSemaphore *semaphore.Weighted  // 동시 생성 제한
    metricsServer             *metrics.MetricsServer
    newOCIClient              func(...) (oci.Client, error)
    newGitClient              func(...) (git.Client, error)
    newHelmClient             func(...) helm.Client
    initConstants             RepoServerInitConstants
    now                       func() time.Time
}
```

### 3.2 동시성 제어 메커니즘

Repo Server는 두 가지 레벨의 동시성 제어를 사용한다:

```
요청 도착
    │
    ▼
┌─────────────────────────────────────┐
│  parallelismLimitSemaphore          │  ← 전역 동시 실행 수 제한
│  (semaphore.Weighted)               │     (ARGOCD_REPO_SERVER_PARALLELISM_LIMIT)
└─────────────────────────────────────┘
    │ Acquire(ctx, 1) 성공
    ▼
┌─────────────────────────────────────┐
│  repoLock (repositoryLock)          │  ← 동일 리포, 동일 리비전 직렬화
│  KeyLock 기반                       │     (allowConcurrent=false 시 상호 배제)
└─────────────────────────────────────┘
    │ Lock 획득
    ▼
  매니페스트 생성 실행
    │
    ▼
repoLock.Unlock() → sem.Release(1)
```

- **`parallelismLimitSemaphore`**: Repo Server 전체 동시 실행 수를 제한. 기본값은 `ParallelismLimit` 설정에 따라 결정.
- **`repoLock`**: 같은 Git 리포의 같은 리비전에 대한 동시 체크아웃을 방지. `allowConcurrent=true`인 소스(Helm, OCI)는 병렬 허용.

### 3.3 GenerateManifest() 전체 흐름

```go
// reposerver/repository/repository.go:591
func (s *Service) GenerateManifest(ctx context.Context, q *apiclient.ManifestRequest) (*apiclient.ManifestResponse, error) {
    // cacheFn: 캐시 히트 시 결과 반환
    cacheFn := func(cacheKey string, refSourceCommitSHAs cache.ResolvedRevisions, firstInvocation bool) (bool, error) {
        ok, resp, err := s.getManifestCacheEntry(cacheKey, q, refSourceCommitSHAs, firstInvocation)
        res = resp
        return ok, err
    }

    // operation: 실제 매니페스트 생성 로직
    operation := func(repoRoot, commitSHA, cacheKey string, ctxSrc operationContextSrc) error {
        promise = s.runManifestGen(ctx, repoRoot, commitSHA, cacheKey, ctxSrc, q)
        select {
        case err := <-promise.errCh:   return err
        case resp := <-promise.responseCh: res = resp
        case tarDone := <-promise.tarDoneCh: tarConcluded = tarDone
        }
        return nil
    }

    settings := operationSettings{
        sem:             s.parallelismLimitSemaphore,
        noCache:         q.NoCache,
        noRevisionCache: q.NoRevisionCache,
        allowConcurrent: q.ApplicationSource.AllowsConcurrentProcessing(),
    }
    err = s.runRepoOperation(ctx, q.Revision, q.Repo, q.ApplicationSource, q.VerifySignature, cacheFn, operation, settings, ...)
}
```

### 3.4 runRepoOperation() — Double-Check Locking 패턴

```
runRepoOperation() 진입
    │
    ├─ 소스 타입 판별: OCI / Helm / Git
    │       └─ 리비전 해석 (브랜치명 → commit SHA)
    │
    ├─ [1차 캐시 확인] settings.noCache == false 시
    │       └─ cacheFn(revision, repoRefs, firstInvocation=true)
    │               ├─ 캐시 히트 → 즉시 반환
    │               └─ 캐시 미스 → 계속
    │
    ├─ metricsServer.IncPendingRepoRequest(repo.Repo)
    │
    ├─ [세마포어 획득] settings.sem != nil
    │       └─ sem.Acquire(ctx, 1)  ← 여기서 대기 발생 가능
    │
    ├─ [2차 캐시 확인] ← Double-Check Locking 핵심!
    │       └─ 세마포어 대기 중 다른 goroutine이 캐시를 채웠을 수 있음
    │               ├─ 캐시 히트 → 세마포어 즉시 해제 후 반환
    │               └─ 캐시 미스 → 실제 생성 진행
    │
    ├─ 소스 타입별 처리:
    │       ├─ OCI: ociClient.Extract() → operation()
    │       ├─ Helm: helmClient.ExtractChart() → operation()
    │       └─ Git: repoLock.Lock() → checkoutRevision() → operation()
    │
    └─ operation(repoRoot, commitSHA, cacheKey, ctxSrc)
```

> **왜 Double-Check Locking인가?** 세마포어를 기다리는 동안 동일한 키에 대해 다른 Goroutine이 이미 매니페스트를 생성하고 캐시에 저장했을 수 있다. 세마포어 획득 후 캐시를 다시 확인함으로써 중복 생성을 방지하고 CPU/메모리 낭비를 줄인다.

### 3.5 소스 타입별 매니페스트 생성

`GenerateManifests()`에서 `appSourceType`에 따라 분기:

```go
// reposerver/repository/repository.go:1522
switch appSourceType {
case v1alpha1.ApplicationSourceTypeHelm:
    targetObjs, command, err = helmTemplate(appPath, repoRoot, env, q, ...)
case v1alpha1.ApplicationSourceTypeKustomize:
    k := kustomize.NewKustomizeApp(repoRoot, appPath, ...)
    targetObjs, _, commands, err = k.Build(q.ApplicationSource.Kustomize, ...)
case v1alpha1.ApplicationSourceTypePlugin:
    targetObjs, err = runConfigManagementPluginSidecars(ctx, appPath, ...)
case v1alpha1.ApplicationSourceTypeDirectory:
    targetObjs, err = findManifests(logCtx, appPath, repoRoot, env, *directory, ...)
}
```

**소스 타입별 특징:**

| 소스 타입 | 감지 기준 | 처리 방식 |
|-----------|-----------|-----------|
| `Helm` | `source.Chart` 존재 또는 `Chart.yaml` 발견 | `helm template` 실행 |
| `Kustomize` | `kustomization.yaml` 발견 | `kustomize build` 실행 |
| `Plugin (CMP)` | `plugin.name` 지정 또는 플러그인 자동 감지 | 사이드카 프로세스로 tar 전송 |
| `Directory` | 기본값 | YAML/JSON 파일 직접 파싱 |
| `OCI` | `oci://` 접두사 | OCI 레지스트리에서 이미지 다운로드 |
| `Kustomize+Jsonnet` | `jsonnet` 확장자 파일 | `go-jsonnet` 라이브러리 사용 |

### 3.6 에러 캐싱 — 생성 실패 시 Circuit Breaker

```go
// reposerver/repository/repository.go:898
if s.initConstants.PauseGenerationAfterFailedGenerationAttempts > 0 {
    // 연속 실패 횟수가 임계값을 초과하면 캐시에 에러 저장
    if res.NumberOfConsecutiveFailures >= s.initConstants.PauseGenerationAfterFailedGenerationAttempts {
        // PauseGenerationOnFailureForMinutes 동안 추가 요청을 캐시에서 에러로 반환
        if elapsedTimeInMinutes >= s.initConstants.PauseGenerationOnFailureForMinutes {
            // 대기 시간 초과 → 재시도
        }
        // PauseGenerationOnFailureForRequests 횟수까지 캐시 응답 반환
        if res.NumberOfCachedResponsesReturned >= s.initConstants.PauseGenerationOnFailureForRequests {
            // 요청 횟수 초과 → 재시도
        }
    }
}
```

| 환경변수 / 설정 | 기본값 | 역할 |
|----------------|--------|------|
| `PauseGenerationAfterFailedGenerationAttempts` | `0` (비활성) | 에러 캐싱 활성화 임계값 |
| `PauseGenerationOnFailureForMinutes` | `0` | 에러 캐시 TTL (분) |
| `PauseGenerationOnFailureForRequests` | `0` | 에러 캐시 최대 요청 수 |

### 3.7 .argocd-source 오버라이드 파일

```go
// reposerver/repository/repository.go:73
const (
    repoSourceFile = ".argocd-source.yaml"
    appSourceFile  = ".argocd-source-%s.yaml"
)
```

Git 리포지터리의 앱 경로에 다음 파일이 있으면 `ApplicationSource`에 추가 파라미터가 병합된다:

1. `.argocd-source.yaml` — 해당 경로의 모든 앱에 적용
2. `.argocd-source-{appName}.yaml` — 특정 앱에만 적용 (더 높은 우선순위)

```yaml
# .argocd-source-my-app.yaml 예시
helm:
  parameters:
    - name: image.tag
      value: v1.2.3
```

### 3.8 ARGOCD_APP_* 환경변수 주입

Config Management Plugin(CMP)이나 Helm 값 파일에서 앱 정보를 참조할 수 있도록 환경변수를 주입한다:

```go
// reposerver/repository/repository.go:1619
return &v1alpha1.Env{
    &v1alpha1.EnvEntry{Name: "ARGOCD_APP_NAME",                  Value: q.AppName},
    &v1alpha1.EnvEntry{Name: "ARGOCD_APP_NAMESPACE",             Value: q.Namespace},
    &v1alpha1.EnvEntry{Name: "ARGOCD_APP_PROJECT_NAME",          Value: q.ProjectName},
    &v1alpha1.EnvEntry{Name: "ARGOCD_APP_REVISION",              Value: revision},
    &v1alpha1.EnvEntry{Name: "ARGOCD_APP_REVISION_SHORT",        Value: shortRevision},   // 7자리
    &v1alpha1.EnvEntry{Name: "ARGOCD_APP_REVISION_SHORT_8",      Value: shortRevision8},  // 8자리
    &v1alpha1.EnvEntry{Name: "ARGOCD_APP_SOURCE_REPO_URL",       Value: q.Repo.Repo},
    &v1alpha1.EnvEntry{Name: "ARGOCD_APP_SOURCE_PATH",           Value: q.ApplicationSource.Path},
    &v1alpha1.EnvEntry{Name: "ARGOCD_APP_SOURCE_TARGET_REVISION",Value: q.ApplicationSource.TargetRevision},
}
```

---

## 4. ApplicationSet Controller

### 4.1 ApplicationSetReconciler 구조체

`applicationset/controllers/applicationset_controller.go`의 `ApplicationSetReconciler`는 `controller-runtime`(`sigs.k8s.io/controller-runtime`) 프레임워크를 기반으로 동작한다.

```go
// applicationset/controllers/applicationset_controller.go:96
type ApplicationSetReconciler struct {
    client.Client                                    // controller-runtime K8s 클라이언트
    Scheme               *runtime.Scheme
    Recorder             record.EventRecorder
    Generators           map[string]generators.Generator  // 등록된 제너레이터 맵
    ArgoDB               db.ArgoDB
    KubeClientset        kubernetes.Interface
    Policy               argov1alpha1.ApplicationsSyncPolicy
    EnablePolicyOverride bool
    utils.Renderer                                   // 템플릿 렌더러
    ArgoCDNamespace            string
    ApplicationSetNamespaces   []string
    EnableProgressiveSyncs     bool
    SCMRootCAPath              string
    GlobalPreservedAnnotations []string
    GlobalPreservedLabels      []string
    Metrics                    *metrics.ApplicationsetMetrics
    MaxResourcesStatusCount    int
    ClusterInformer            *settings.ClusterInformer
}
```

### 4.2 Generator 인터페이스

```go
// applicationset/generators/interface.go:14
type Generator interface {
    // ApplicationSet 스펙을 해석하여 파라미터 목록 생성
    GenerateParams(
        appSetGenerator *argoprojiov1alpha1.ApplicationSetGenerator,
        applicationSetInfo *argoprojiov1alpha1.ApplicationSet,
        client client.Client,
    ) ([]map[string]any, error)

    // 다음 조정 주기까지의 대기 시간
    GetRequeueAfter(appSetGenerator *argoprojiov1alpha1.ApplicationSetGenerator) time.Duration

    // 제너레이터 인라인 템플릿 (있는 경우)
    GetTemplate(appSetGenerator *argoprojiov1alpha1.ApplicationSetGenerator) *argoprojiov1alpha1.ApplicationSetTemplate
}
```

### 4.3 9개 제너레이터

```
ApplicationSet
    │
    ▼ Reconcile()
    │
    └─ GenerateApplications()
            │
            ├─ ListGenerator         → 정적 목록에서 파라미터 생성
            ├─ ClusterGenerator      → Argo CD 등록 클러스터에서 파라미터 생성
            ├─ GitGenerator          → Git 파일/디렉토리에서 파라미터 생성
            ├─ MatrixGenerator       → 두 제너레이터의 카르테시안 곱
            ├─ MergeGenerator        → 여러 제너레이터 결과 병합
            ├─ PullRequestGenerator  → PR 이벤트에서 파라미터 생성
            ├─ SCMProviderGenerator  → SCM 공급자(GitHub 등) 리포 목록
            ├─ PluginGenerator       → 외부 플러그인 호출
            └─ DuckTypeGenerator     → 커스텀 K8s 리소스에서 파라미터 생성
```

**제너레이터별 사용 예:**

| 제너레이터 | 파일 | 대표 사용 사례 |
|-----------|------|--------------|
| `List` | `generators/list.go` | 고정된 환경 목록(dev/staging/prod) |
| `Cluster` | `generators/cluster.go` | Argo CD에 등록된 모든 클러스터에 앱 배포 |
| `Git` | `generators/git.go` | 디렉토리별 마이크로서비스 자동 생성 |
| `Matrix` | `generators/matrix.go` | {클러스터} × {환경} 조합 |
| `Merge` | `generators/merge.go` | 기본값 + 오버라이드 병합 |
| `PullRequest` | `generators/pull_request.go` | PR별 임시 환경 배포 |
| `SCMProvider` | `generators/scm_provider.go` | GitHub/GitLab Org의 모든 리포 |
| `Plugin` | `generators/plugin.go` | 외부 HTTP 플러그인 |
| `DuckType` | `generators/duck_type.go` | ClusterDecisionResource 등 커스텀 CRD |

### 4.4 Reconcile() 흐름

```go
// applicationset/controllers/applicationset_controller.go:120
func (r *ApplicationSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
    // 1. ApplicationSet CR 조회
    r.Get(ctx, req.NamespacedName, &applicationSetInfo)

    // 2. 삭제 처리
    if applicationSetInfo.DeletionTimestamp != nil { ... }

    // 3. 제너레이터 실행 → 파라미터 목록 생성
    generatedApplications, applicationSetReason, err :=
        template.GenerateApplications(logCtx, applicationSetInfo, r.Generators, r.Renderer, r.Client)

    // 4. Progressive Sync 처리 (활성화된 경우)
    if r.EnableProgressiveSyncs {
        appSyncMap, err = r.performProgressiveSyncs(ctx, logCtx, applicationSetInfo, ...)
    }

    // 5. Application CR 생성/업데이트/삭제
    // ...
}
```

### 4.5 템플릿 렌더링

ApplicationSet은 Go 표준 템플릿 또는 fasttemplate을 사용하여 Application 스펙을 렌더링한다:

```yaml
# ApplicationSet 템플릿 예시
template:
  metadata:
    name: '{{.cluster}}-guestbook'
  spec:
    destination:
      server: '{{.url}}'
      namespace: '{{.namespace}}'
    source:
      repoURL: https://github.com/argoproj/argocd-example-apps
      targetRevision: HEAD
      path: guestbook
```

제너레이터가 `[{"cluster": "prod", "url": "https://prod.k8s.io", "namespace": "guestbook"}, ...]`와 같은 파라미터 목록을 반환하면, 각 파라미터 셋으로 템플릿을 렌더링하여 Application CR을 생성한다.

### 4.6 Progressive Syncs — 단계적 롤아웃

Progressive Syncs는 RollingSync 전략을 통해 ApplicationSet이 생성한 여러 Application을 순차적으로 동기화한다:

```
ApplicationSet (RollingSync)
    │
    ├─ Step 1: matchExpressions → [app-a, app-b]
    │       └─ maxUpdate: 1 → app-a 동기화
    │               └─ app-a Healthy → 다음 단계
    │
    ├─ Step 2: matchExpressions → [app-c, app-d]
    │       └─ maxUpdate: 2 → app-c, app-d 동기화
    │
    └─ 완료
```

상태 전이:
- `ProgressiveSyncWaiting` → `ProgressiveSyncPending` → `ProgressiveSyncProgressing` → `ProgressiveSyncHealthy`

---

## 5. GitOps Engine

### 5.1 구조 개요

`gitops-engine/pkg/`는 Argo CD의 핵심 GitOps 기능을 별도 라이브러리로 분리한 패키지다. 다른 GitOps 도구에서도 재사용 가능하도록 설계되었다.

```
gitops-engine/pkg/
├── cache/          ← ClusterCache: K8s 리소스 인메모리 캐시
├── diff/           ← 3가지 diff 전략
├── sync/           ← 동기화 실행 엔진
│   ├── sync_context.go   ← SyncContext 구현
│   ├── syncwaves/        ← Sync Wave 처리
│   ├── hook/             ← Hook 처리
│   └── common/           ← 상수 및 공통 타입
├── health/         ← 리소스 헬스 체크
└── engine/         ← GitOps 조정 루프
```

### 5.2 ClusterCache — K8s 리소스 인메모리 캐시

`gitops-engine/pkg/cache/cluster.go`의 `ClusterCache`는 K8s Watch API를 사용하여 클러스터 리소스를 인메모리에 캐싱한다.

```go
// gitops-engine/pkg/cache/cluster.go:140
type ClusterCache interface {
    EnsureSynced() error
    GetServerVersion() string
    GetAPIResources() []kube.APIResourceInfo
    GetOpenAPISchema() openapi.Resources
    GetGVKParser() *managedfields.GvkParser
    Invalidate(opts ...UpdateSettingsFunc)
    FindResources(namespace string, predicates ...func(r *Resource) bool) map[kube.ResourceKey]*Resource
    IterateHierarchyV2(keys []kube.ResourceKey, action func(resource *Resource, namespaceResources map[kube.ResourceKey]*Resource) bool)
    IsNamespaced(gk schema.GroupKind) (bool, error)
    GetManagedLiveObjs(targetObjs []*unstructured.Unstructured, isManaged func(r *Resource) bool) (map[kube.ResourceKey]*unstructured.Unstructured, error)
    GetClusterInfo() ClusterInfo
    OnResourceUpdated(handler OnResourceUpdatedHandler) Unsubscribe
    OnEvent(handler OnEventHandler) Unsubscribe
}
```

**ClusterCache 내부 구조:**

```go
// gitops-engine/pkg/cache/cluster.go:222
type clusterCache struct {
    resources    map[kube.ResourceKey]*Resource     // 전체 리소스 맵
    nsIndex      map[string]map[kube.ResourceKey]*Resource  // 네임스페이스 인덱스
    apisMeta     map[schema.GroupKind]*apiMeta       // API 메타데이터
    parentUIDToChildren map[types.UID][]kube.ResourceKey    // 부모-자식 관계

    watchResyncTimeout      time.Duration  // Watch 재연결 주기 (기본 10분)
    listSemaphore           *semaphore.Weighted  // 동시 List 요청 제한
    ...
}
```

**Watch 기반 실시간 동기화:**

```
초기화
    │
    ├─ List API: 전체 리소스 로드 → resources 맵 채우기
    │
    └─ Watch API: 리소스 변경 이벤트 구독
            │
            ├─ ADDED   → resources 맵에 추가
            ├─ MODIFIED → resources 맵 업데이트
            └─ DELETED  → resources 맵에서 제거
                    │
                    └─ OnResourceUpdated 핸들러 호출
                            └─ Application Controller에 변경 알림
```

```go
// gitops-engine/pkg/cache/cluster.go:793
w, err := watchutil.NewRetryWatcherWithContext(ctx, resourceVersion, &cache.ListWatch{
    WatchFuncWithContext: func(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
        res, err := resClient.Watch(ctx, options)
        if err != nil {
            c.stopWatching(api.GroupKind, ns)
        }
        return res, err
    },
})
```

### 5.3 Diff 엔진 — 3가지 Diff 전략

`gitops-engine/pkg/diff/diff.go`는 Git의 원하는 상태와 클러스터의 실제 상태를 비교하는 3가지 전략을 지원한다:

```go
// gitops-engine/pkg/diff/diff.go:76
func Diff(config, live *unstructured.Unstructured, opts ...Option) (*DiffResult, error) {
    if o.serverSideDiff {
        return ServerSideDiff(config, live, opts...)
    }
    if structuredMergeDiff {
        return StructuredMergeDiff(config, live, o.gvkParser, o.manager)
    }
    // 기본: 3-way diff (last-applied annotation 기반)
    orig, _ := GetLastAppliedConfigAnnotation(live)
    if orig != nil {
        return ThreeWayDiff(orig, config, live)
    }
    return TwoWayDiff(config, live)
}
```

**3가지 Diff 전략 비교:**

| 전략 | 함수 | 설명 | 적용 조건 |
|------|------|------|-----------|
| Three-Way Diff | `ThreeWayDiff(orig, config, live)` | 원본+원하는 상태+실제 상태 3자 비교 | 기본 (last-applied annotation 있을 때) |
| Server-Side Diff | `ServerSideDiff(config, live)` | K8s API dry-run apply로 예측 상태 계산 | `WithServerSideDiff(true)` 옵션 |
| Structured Merge Diff | `StructuredMergeDiff(config, live, gvkParser, manager)` | structured-merge-diff 라이브러리 사용 | `ServerSideApply=true` sync 옵션 |

**Three-Way Diff 원리:**
```
      orig (last-applied)
       /            \
      /              \
 config (Git)     live (K8s)
      \              /
       \            /
        [3-way diff]
             │
      patch = (config - orig) + (live - orig 제외)
             │
      최종 desired state
```

### 5.4 SyncContext — 동기화 실행

`gitops-engine/pkg/sync/sync_context.go`의 `syncContext`는 실제 K8s 리소스 생성/수정/삭제를 관리한다.

**Sync Phase와 Hook 타입:**

```go
// gitops-engine/pkg/sync/common/types.go:68
const (
    SyncPhasePreSync  = "PreSync"   // 메인 리소스 적용 전
    SyncPhaseSync     = "Sync"      // 메인 리소스 적용
    SyncPhasePostSync = "PostSync"  // 메인 리소스 적용 후
    SyncPhaseSyncFail = "SyncFail"  // 동기화 실패 시
)

type HookType string
const (
    HookTypePreSync  HookType = "PreSync"
    HookTypeSync     HookType = "Sync"
    HookTypePostSync HookType = "PostSync"
    HookTypeSkip     HookType = "Skip"     // 이 리소스는 sync에서 제외
    HookTypeSyncFail HookType = "SyncFail" // 실패 시에만 실행
)
```

**Sync Wave 처리:**

```go
// gitops-engine/pkg/sync/syncwaves/waves.go:12
func Wave(obj *unstructured.Unstructured) int {
    // argocd.argoproj.io/sync-wave 어노테이션 값 파싱
    text, ok := obj.GetAnnotations()[common.AnnotationSyncWave]
    if ok {
        val, err := strconv.Atoi(text)
        if err == nil {
            return val
        }
    }
    // Helm 훅의 경우 weight 어노테이션으로 폴백
    return helmhook.Weight(obj)
}
```

**전체 Sync 실행 흐름:**

```
Sync() 시작
    │
    ├─ Phase 1: PreSync 훅 실행 (wave 순서대로)
    │       └─ 각 wave 완료 후 syncWaveHook() 콜백
    │
    ├─ Phase 2: Sync (메인 리소스)
    │       ├─ wave -∞ ... wave 0 ... wave +∞ 순으로 적용
    │       │       ├─ dryRun 모드: kubectl apply --dry-run
    │       │       ├─ ServerSideApply: kubectl apply --server-side
    │       │       └─ 일반: kubectl apply
    │       └─ 각 wave 완료 후 다음 wave 대기
    │
    ├─ Phase 3: PostSync 훅 실행
    │
    └─ Phase 4 (실패 시): SyncFail 훅 실행
```

**syncContext 주요 옵션:**

| 옵션 | 어노테이션/플래그 | 효과 |
|------|----------------|------|
| `prune` | `SyncPolicy.Prune` | 불필요한 리소스 삭제 |
| `replace` | `Replace=true` | apply 대신 replace 사용 |
| `serverSideApply` | `ServerSideApply=true` | K8s 서버사이드 Apply |
| `skipHooks` | `Sync.SyncOptions.skipHooks` | 훅 건너뜀 |
| `pruneLast` | `PruneLast=true` | 프루닝을 마지막으로 |
| `applyOutOfSyncOnly` | `ApplyOutOfSyncOnly=true` | OutOfSync 리소스만 적용 |

### 5.5 Health 엔진 — 리소스 헬스 체크

`gitops-engine/pkg/health/health.go`의 `GetResourceHealth()`는 12개 이상의 리소스 타입에 대한 빌트인 헬스 체크를 제공한다.

```go
// gitops-engine/pkg/health/health.go:104
func GetHealthCheckFunc(gvk schema.GroupVersionKind) func(obj *unstructured.Unstructured) (*HealthStatus, error) {
    switch gvk.Group {
    case "apps":
        switch gvk.Kind {
        case kube.DeploymentKind:  return getDeploymentHealth
        case kube.StatefulSetKind: return getStatefulSetHealth
        case kube.ReplicaSetKind:  return getReplicaSetHealth
        case kube.DaemonSetKind:   return getDaemonSetHealth
        }
    case "":  // core API group
        switch gvk.Kind {
        case kube.ServiceKind:                return getServiceHealth
        case kube.PersistentVolumeClaimKind:  return getPVCHealth
        case kube.PodKind:                    return getPodHealth
        }
    case "batch":
        if gvk.Kind == kube.JobKind { return getJobHealth }
    case "autoscaling":
        if gvk.Kind == kube.HorizontalPodAutoscalerKind { return getHPAHealth }
    case "networking.k8s.io", "extensions":
        if gvk.Kind == kube.IngressKind { return getIngressHealth }
    case "apiregistration.k8s.io":
        if gvk.Kind == kube.APIServiceKind { return getAPIServiceHealth }
    case "argoproj.io":
        if gvk.Kind == "Workflow" { return getArgoWorkflowHealth }
    }
    return nil  // 미지원 타입 → healthOverride로 커스텀 체크 가능
}
```

**헬스 상태 코드:**

```go
// gitops-engine/pkg/health/health.go:14
const (
    HealthStatusUnknown     HealthStatusCode = "Unknown"
    HealthStatusProgressing HealthStatusCode = "Progressing"
    HealthStatusHealthy     HealthStatusCode = "Healthy"
    HealthStatusSuspended   HealthStatusCode = "Suspended"
    HealthStatusDegraded    HealthStatusCode = "Degraded"
    HealthStatusMissing     HealthStatusCode = "Missing"
)
```

**헬스 우선순위 (낮은 순):**

```
Healthy < Suspended < Progressing < Missing < Degraded < Unknown
```

**리소스별 헬스 판단 기준:**

| 리소스 | 파일 | Healthy 조건 |
|--------|------|-------------|
| Deployment | `health_deployment.go` | `availableReplicas >= desiredReplicas`, 업데이트 완료 |
| StatefulSet | `health_statefulset.go` | `readyReplicas >= desiredReplicas`, 업데이트 완료 |
| DaemonSet | `health_daemonset.go` | `desiredNumberScheduled == updatedNumberScheduled == numberAvailable` |
| Job | `health_job.go` | `succeeded >= completions` (단, `Failed` 조건 없어야 함) |
| Pod | `health_pod.go` | `phase == Running`, 모든 컨테이너 Ready |
| PVC | `health_pvc.go` | `phase == Bound` |
| HPA | `health_hpa.go` | `currentReplicas == desiredReplicas`, Able to scale |
| Ingress | `health_ingress.go` | `loadBalancer.ingress` 배정 완료 |
| Service | `health_service.go` | LoadBalancer 타입 시 IP/hostname 배정 완료 |

**GetResourceHealth() 조정 우선순위:**
1. `DeletionTimestamp != nil` → `Progressing` (삭제 진행 중)
2. `healthOverride.GetResourceHealth()` → 사용자 정의 헬스 루틴
3. `GetHealthCheckFunc()` → 빌트인 헬스 체크

---

## 6. Notification Controller

### 6.1 구조 개요

`notification_controller/controller/controller.go`의 `notificationController`는 `argoproj/notifications-engine` 라이브러리를 기반으로 동작한다.

```go
// notification_controller/controller/controller.go:57
type notificationController struct {
    ctrl              controller.NotificationController  // notifications-engine 컨트롤러
    appInformer       cache.SharedIndexInformer         // Application 감시
    appProjInformer   cache.SharedIndexInformer         // AppProject 감시
    secretInformer    cache.SharedIndexInformer         // Secret 감시 (알림 설정)
    configMapInformer cache.SharedIndexInformer         // ConfigMap 감시 (알림 설정)
}
```

### 6.2 알림 처리 흐름

```
K8s Watch API
    │
    ├─ Application CR 변경 감지
    │       └─ appInformer → 큐에 추가
    │
    ▼
notifications-engine 컨트롤러
    │
    ├─ 1. 트리거 조건 평가
    │       └─ (예) on-sync-succeeded: app.status.sync.status == 'Synced'
    │
    ├─ 2. 구독자 목록 조회
    │       ├─ Application 어노테이션: notifications.argoproj.io/subscribe.*
    │       └─ AppProject 어노테이션: 프로젝트 레벨 구독
    │
    ├─ 3. 알림 서비스 선택
    │       └─ argocd-notifications-secret + argocd-notifications-cm
    │
    └─ 4. 메시지 전송
            ├─ Slack
            ├─ Email (SMTP)
            ├─ PagerDuty
            ├─ Webhook (HTTP)
            ├─ Microsoft Teams
            ├─ Opsgenie
            ├─ Telegram
            └─ 기타 (notifications-engine 플러그인)
```

### 6.3 Skip 처리 조건

```go
// notification_controller/controller/controller.go
skipProcessingOpt := controller.WithSkipProcessing(func(obj metav1.Object) (bool, string) {
    app, ok := (obj).(*unstructured.Unstructured)
    if !ok {
        return false, ""
    }
    // 1. 허용 네임스페이스 외 앱 건너뜀
    if checkAppNotInAdditionalNamespaces(app, namespace, applicationNamespaces) {
        return true, "app is not in one of the application-namespaces, nor the notification controller namespace"
    }
    // 2. 동기화 상태가 갱신되지 않은 앱 건너뜀
    return !isAppSyncStatusRefreshed(app, ...), "sync status out of date"
})
```

### 6.4 알림 설정 구조

알림 설정은 두 개의 K8s 리소스에 저장된다:

```yaml
# argocd-notifications-cm ConfigMap
apiVersion: v1
kind: ConfigMap
metadata:
  name: argocd-notifications-cm
data:
  trigger.on-sync-succeeded: |
    - when: app.status.sync.status == 'Synced'
      send: [app-sync-succeeded]
  template.app-sync-succeeded: |
    message: |
      Application {{.app.metadata.name}} has been successfully synced.
  service.slack: |
    token: $slack-token
```

```yaml
# argocd-notifications-secret Secret
apiVersion: v1
kind: Secret
metadata:
  name: argocd-notifications-secret
stringData:
  slack-token: "xoxb-..."
  smtp-password: "..."
```

---

## 7. 컴포넌트 간 상호작용

### 7.1 전체 아키텍처 다이어그램

```
사용자/CI 시스템
    │
    │ HTTP/gRPC
    ▼
┌─────────────────────────────────────┐
│       argocd-server (API Server)    │
│  ┌───────────────────────────────┐  │
│  │ cmux (포트 멀티플렉싱)        │  │
│  │  ├─ HTTP/1.1                  │  │
│  │  ├─ gRPC                      │  │
│  │  └─ WebSocket (UI)            │  │
│  └───────────────────────────────┘  │
│  ┌───────────────────────────────┐  │
│  │ 13개 gRPC 서비스              │  │
│  │ (Application, Cluster, ...)   │  │
│  └───────────────────────────────┘  │
└─────────────────────────────────────┘
    │                       │
    │ gRPC                  │ K8s API
    ▼                       ▼
┌──────────────┐    ┌───────────────────────────────────┐
│  argocd-     │    │  argocd-application-controller    │
│  repo-server │◄───│                                   │
│              │    │  ┌─────────────────────────────┐  │
│  .Generate   │    │  │ appRefreshQueue             │  │
│  Manifest()  │    │  │ appOperationQueue           │  │
│              │    │  └─────────────────────────────┘  │
└──────────────┘    │  ┌─────────────────────────────┐  │
    │               │  │ gitops-engine               │  │
    │ Git/Helm/OCI  │  │  ├─ ClusterCache (Watch)    │  │
    ▼               │  │  ├─ Diff Engine             │  │
┌──────────────┐    │  │  ├─ SyncContext             │  │
│  Git/Helm/   │    │  │  └─ HealthCheck             │  │
│  OCI 레지스트리│   │  └─────────────────────────────┘  │
└──────────────┘    └───────────────────────────────────┘
                                    │
                            │       │ K8s Watch
                            ▼       ▼
                    ┌───────────────────────────┐
                    │  argocd-applicationset-   │
                    │  controller               │
                    │  (controller-runtime)     │
                    │                           │
                    │  9개 Generator            │
                    │  → Application CR 생성    │
                    └───────────────────────────┘
                                    │
                            ▼       │
                    ┌───────────────────────────┐
                    │  argocd-notification-     │
                    │  controller               │
                    │  (notifications-engine)   │
                    │                           │
                    │  트리거 → 알림 전송       │
                    └───────────────────────────┘
```

### 7.2 전형적인 GitOps 동기화 흐름

```
1. Git 커밋 푸시
    │
    │ (Webhook 또는 주기적 폴링)
    ▼
2. argocd-server → Webhook 수신
    │   appRefreshQueue.AddRateLimited(appKey)
    ▼
3. ApplicationController.processAppRefreshQueueItem()
    │
    ├─ needRefreshAppStatus() → CompareWithLatest
    │
    ├─ CompareAppState()
    │       │
    │       ├─ Repo Server: GenerateManifest()
    │       │       └─ Git clone/pull → helm template / kustomize build
    │       │
    │       └─ ClusterCache: GetManagedLiveObjs()
    │               └─ Watch 기반 인메모리 캐시에서 조회
    │
    ├─ Diff(desired, live) → OutOfSync 감지
    │
    ├─ autoSync() → Operation 생성
    │
    ▼
4. ApplicationController.processAppOperationQueueItem()
    │
    ├─ processRequestedAppOperation()
    │
    └─ SyncContext.Sync()
            ├─ Phase: PreSync 훅 실행
            ├─ Phase: Sync (kubectl apply) - wave 순서대로
            └─ Phase: PostSync 훅 실행
    │
    ▼
5. 헬스 체크
    │
    └─ GetResourceHealth() → Healthy
    │
    ▼
6. Application.Status 업데이트
    │
    ├─ sync.status: Synced
    └─ health.status: Healthy
    │
    ▼
7. NotificationController 알림
    │
    └─ on-sync-succeeded 트리거 → Slack/Email 전송
```

### 7.3 컴포넌트별 통신 프로토콜

| 통신 경로 | 프로토콜 | 포트 | 인증 |
|-----------|----------|------|------|
| CLI/UI → API Server | gRPC, HTTP | 8080/8443 | JWT |
| API Server → Repo Server | gRPC | 8081 | mTLS |
| Application Controller → Repo Server | gRPC | 8081 | mTLS |
| Application Controller → K8s API | HTTPS | 6443 | ServiceAccount/kubeconfig |
| ApplicationSet Controller → K8s API | HTTPS | 6443 | ServiceAccount |
| Notification Controller → K8s API | HTTPS | 6443 | ServiceAccount |
| Notification Controller → 외부 서비스 | HTTPS | 443 | 서비스별 토큰 |

### 7.4 컴포넌트별 주요 환경변수

| 컴포넌트 | 환경변수 | 기본값 | 설명 |
|----------|----------|--------|------|
| API Server | `ARGOCD_GRPC_MAX_SIZE_MB` | 200 | gRPC 메시지 최대 크기 (MB) |
| API Server | `ARGOCD_MAX_CONCURRENT_LOGIN_REQUESTS_COUNT` | 50 | 동시 로그인 요청 수 제한 |
| API Server | `ARGOCD_API_SERVER_REPLICAS` | 1 | API 서버 레플리카 수 |
| App Controller | `ARGOCD_APPLICATION_CONTROLLER_STATUS_PROCESSORS` | 20 | 상태 처리 워커 수 |
| App Controller | `ARGOCD_APPLICATION_CONTROLLER_OPERATION_PROCESSORS` | 10 | 작업 처리 워커 수 |
| Repo Server | `ARGOCD_REPO_SERVER_PARALLELISM_LIMIT` | 0 (무제한) | 동시 매니페스트 생성 수 |
| Repo Server | `ARGOCD_REPO_SERVER_PAUSE_ON_FAILURE_ATTEMPTS` | 0 | 에러 캐싱 임계값 |

---

## 요약 비교표

| 컴포넌트 | 핵심 파일 | 주요 기술 | 역할 |
|----------|-----------|----------|------|
| argocd-server | `server/server.go` | cmux, gRPC, JWT | API 게이트웨이 |
| argocd-application-controller | `controller/appcontroller.go` | workqueue, goroutine pool | GitOps 조정 루프 |
| argocd-repo-server | `reposerver/repository/repository.go` | semaphore, KeyLock | 매니페스트 생성 |
| argocd-applicationset-controller | `applicationset/controllers/applicationset_controller.go` | controller-runtime | 앱 대량 생성 |
| gitops-engine (cache) | `gitops-engine/pkg/cache/cluster.go` | Watch API, 인메모리 맵 | 클러스터 상태 캐시 |
| gitops-engine (diff) | `gitops-engine/pkg/diff/diff.go` | 3-way/SSA/SMD diff | 상태 비교 |
| gitops-engine (sync) | `gitops-engine/pkg/sync/sync_context.go` | wave, hook, prune | 동기화 실행 |
| gitops-engine (health) | `gitops-engine/pkg/health/health.go` | GVK 기반 핸들러 | 헬스 체크 |
| argocd-notification-controller | `notification_controller/controller/controller.go` | notifications-engine | 알림 전송 |
