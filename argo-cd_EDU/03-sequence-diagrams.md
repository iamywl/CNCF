# Argo CD 시퀀스 다이어그램

Argo CD의 핵심 요청 흐름을 단계별로 분석한다. 각 흐름은 실제 소스코드를 직접 추적하여 작성되었다.

---

## 목차

1. [Application 생성 흐름](#1-application-생성-흐름)
2. [Reconciliation (조정) 흐름](#2-reconciliation-조정-흐름)
3. [Sync (동기화) 실행 흐름](#3-sync-동기화-실행-흐름)
4. [Manifest 생성 흐름](#4-manifest-생성-흐름)
5. [인증/인가 흐름](#5-인증인가-흐름)
6. [Auto-Sync 흐름](#6-auto-sync-흐름)
7. [ApplicationSet 흐름](#7-applicationset-흐름)
8. [Webhook 흐름](#8-webhook-흐름)

---

## 1. Application 생성 흐름

**소스**: `server/application/application.go` — `(s *Server) Create()`

### 개요

사용자가 `argocd app create` 명령어를 실행하면 argocd CLI는 API Server에 gRPC 요청을 보낸다. API Server는 RBAC 검사, 프로젝트 잠금, 유효성 검사, Kubernetes API 저장, Informer 동기화 대기 순서로 처리한다.

```mermaid
sequenceDiagram
    actor User
    participant CLI as argocd CLI
    participant APIGW as gRPC-Gateway
    participant AS as API Server<br/>(Server.Create)
    participant RBAC as Enforcer<br/>(EnforceErr)
    participant PL as projectLock
    participant VAL as validateAndNormalizeApp
    participant K8S as Kubernetes API<br/>(appclientset)
    participant INF as appInformer<br/>(waitSync)

    User->>CLI: argocd app create myapp ...
    CLI->>APIGW: gRPC ApplicationService.Create(req)
    APIGW->>AS: Create(ctx, ApplicationCreateRequest)

    Note over AS: q.GetApplication() nil 검사

    AS->>RBAC: EnforceErr(claims, "applications", "create", app.RBACName())
    alt 권한 없음
        RBAC-->>AS: PermissionDenied error
        AS-->>CLI: gRPC PermissionDenied
    end
    RBAC-->>AS: nil (권한 있음)

    AS->>PL: projectLock.RLock(app.Spec.GetProject())
    Note over PL: 프로젝트 동시 수정 방지<br/>RLock = 읽기 공유 잠금

    AS->>AS: getAppProject(ctx, a) — AppProject 조회
    AS->>VAL: validateAndNormalizeApp(ctx, a, proj, validate)
    Note over VAL: 소스 유효성 검사<br/>목적지 클러스터 검증<br/>정책 준수 확인<br/>스펙 정규화

    alt 유효성 검사 실패
        VAL-->>AS: error
        AS->>PL: RUnlock
        AS-->>CLI: InvalidArgument error
    end
    VAL-->>AS: nil

    Note over AS: a.Operation = nil 강제 설정<br/>(보안: 브랜치 보호 우회 방지)

    AS->>K8S: appclientset.ArgoprojV1alpha1().Applications(ns).Create(ctx, a)

    alt 이미 존재 (AlreadyExists)
        K8S-->>AS: AlreadyExists error
        AS->>AS: appLister.Applications(ns).Get(a.Name)

        alt 스펙 동일 (idempotent)
            AS->>PL: RUnlock
            AS-->>CLI: existing Application 반환
        else Upsert 플래그 없음
            AS->>PL: RUnlock
            AS-->>CLI: InvalidArgument: "use upsert flag"
        else Upsert=true
            AS->>RBAC: EnforceErr(claims, "applications", "update", ...)
            AS->>AS: updateApp(ctx, existing, a, true)
            AS->>PL: RUnlock
            AS-->>CLI: updated Application
        end
    else 생성 성공
        K8S-->>AS: created Application
        AS->>AS: logAppEvent(ctx, created, "created application")
        AS->>INF: waitSync(created)
        Note over INF: Informer 캐시가 새 Application을<br/>반영할 때까지 대기<br/>(ResourceVersion 기반 동기화)
        INF-->>AS: 동기화 완료
        AS->>PL: RUnlock
        AS-->>CLI: Application (gRPC response)
    end

    CLI-->>User: Application 'myapp' created
```

### 주요 포인트

| 단계 | 코드 위치 | 설명 |
|------|----------|------|
| RBAC 검사 | `application.go:352` | `s.enf.EnforceErr(ctx.Value("claims"), rbac.ResourceApplications, rbac.ActionCreate, a.RBACName(s.ns))` |
| 프로젝트 잠금 | `application.go:356` | `s.projectLock.RLock(a.Spec.GetProject())` — 동시 프로젝트 수정 경합 방지 |
| Operation 강제 제거 | `application.go:381-387` | 사용자가 Operation을 직접 설정하려는 시도를 차단 (브랜치 보호 우회 방지) |
| Kubernetes 저장 | `application.go:389` | `s.appclientset.ArgoprojV1alpha1().Applications(appNs).Create()` |
| Informer 동기화 | `application.go:392` | `s.waitSync(created)` — 캐시 불일치 방지 |

---

## 2. Reconciliation (조정) 흐름

**소스**: `controller/appcontroller.go` — `(ctrl *ApplicationController) processAppRefreshQueueItem()`

### 개요

Application Controller는 Informer 이벤트로 `appRefreshQueue`에 항목이 추가되면 `processAppRefreshQueueItem()`을 호출한다. 이 함수는 리프레시 필요 여부 판단 → 상태 비교 → 자동 동기화 → 상태 저장의 순서로 진행한다.

### CompareWith 레벨

```
CompareWithLatestForceResolve (3)  최신 리비전 강제 재분석 (ref 소스 포함)
CompareWithLatest              (2)  최신 리비전과 비교
CompareWithRecent              (1)  최근 캐시된 리비전과 비교
ComparisonWithNothing          (0)  Git 없이 캐시된 리소스만 업데이트
```

소스: `controller/appcontroller.go:84-94`

```go
type CompareWith int

const (
    CompareWithLatestForceResolve CompareWith = 3
    CompareWithLatest             CompareWith = 2
    CompareWithRecent             CompareWith = 1
    ComparisonWithNothing         CompareWith = 0
)
```

```mermaid
sequenceDiagram
    participant INF as appInformer<br/>(K8s Informer)
    participant RQ as appRefreshQueue<br/>(workqueue)
    participant CTRL as processAppRefreshQueueItem
    participant NR as needRefreshAppStatus
    participant CA as CompareAppState
    participant RS as repo-server<br/>(gRPC)
    participant AS as autoSync
    participant DB as persistAppStatus<br/>(K8s PATCH)
    participant OQ as appOperationQueue

    INF->>RQ: 이벤트 발생 → appKey 추가<br/>(Add/Update/Delete)
    RQ->>CTRL: appKey dequeue

    CTRL->>CTRL: appInformer.GetIndexer().GetByKey(appKey)
    Note over CTRL: informer 캐시에서 App 객체 조회<br/>없으면 삭제된 것 → 종료

    CTRL->>NR: needRefreshAppStatus(origApp, statusRefreshTimeout, statusHardRefreshTimeout)

    Note over NR: 리프레시 필요 조건 판단:<br/>1. app.IsRefreshRequested() → level 3<br/>2. currentSource != syncedSource → level 3<br/>3. hardExpired or softExpired → level 2<br/>4. destination/ignoreDiff 변경 → level 2<br/>5. controller refresh 요청 → 요청된 level

    alt needRefresh = false
        NR-->>CTRL: false
        CTRL->>OQ: appOperationQueue.AddRateLimited(appKey)
        Note over CTRL: defer에서 항상 OperationQueue에 push
    else needRefresh = true
        NR-->>CTRL: (true, refreshType, comparisonLevel)

        alt comparisonLevel == ComparisonWithNothing (0)
            CTRL->>CTRL: cache.GetAppManagedResources() 조회
            CTRL->>CTRL: getResourceTree() 업데이트
            CTRL->>DB: persistAppStatus() — Git 호출 없이 캐시만 업데이트
        else comparisonLevel >= CompareWithRecent (1)
            CTRL->>CTRL: refreshAppConditions(app) — 프로젝트/권한 검증

            CTRL->>CA: CompareAppState(app, project, revisions, sources, ...)

            CA->>RS: GenerateManifest gRPC 호출
            RS-->>CA: manifest 목록

            CA->>CA: diff 알고리즘 수행<br/>(Three-way / SSA / ServerSide)
            CA->>CA: health assessment — 리소스 헬스 평가
            CA-->>CTRL: compareResult (syncStatus, resources, healthStatus)

            CTRL->>CTRL: normalizeApplication(origApp, app)
            CTRL->>CTRL: setAppManagedResources(destCluster, app, compareResult)

            CTRL->>CTRL: project.Spec.SyncWindows.Matches(app).CanSync(false)

            alt SyncWindow 허용
                CTRL->>AS: autoSync(app, compareResult.syncStatus, compareResult.resources, ...)
                AS-->>CTRL: (syncErrCond, opDuration)
            else SyncWindow 차단
                CTRL->>CTRL: log "Sync prevented by sync window"
            end

            CTRL->>CTRL: app.Status 업데이트<br/>(Sync, Health, Resources, SourceType)
            CTRL->>CTRL: updateFinalizers (pre-delete hooks)
            CTRL->>DB: persistAppStatus(origApp, &app.Status)
        end

        CTRL->>OQ: appOperationQueue.AddRateLimited(appKey)
        Note over OQ: defer에서 항상 실행됨
    end

    DB-->>CTRL: patchDuration
    CTRL->>CTRL: metricsServer.IncReconcile(origApp, destServer, reconcileDuration)
```

### needRefreshAppStatus 판단 로직 상세

```
소스: controller/appcontroller.go:2011

판단 우선순위 (위에서 아래로 첫 매치 사용):
1. app.IsRefreshRequested()         → compareWith = 3 (ForceResolve)
2. !currentSourceEqualsSyncedSource → compareWith = 3 (ForceResolve)
3. hardExpired                      → compareWith = 2 (Latest), refreshType = Hard
4. softExpired                      → compareWith = 2 (Latest), refreshType = Normal
5. destination 변경                  → reason 설정, compareWith = 2 (Latest)
6. ignoreDifferences 변경            → reason 설정, compareWith = 2 (Latest)
7. ctrl.isRefreshRequested()        → compareWith = 요청된 level

reason이 설정된 경우에만 needRefresh = true 반환
```

---

## 3. Sync (동기화) 실행 흐름

**소스**: `controller/appcontroller.go` — `processAppOperationQueueItem()`, `processRequestedAppOperation()`
**소스**: `controller/sync.go` — `(m *appStateManager) SyncAppState()`
**소스**: `gitops-engine/pkg/sync/sync_context.go` — `(sc *syncContext) Sync()`

### 개요

`appOperationQueue`에서 꺼낸 Application에 `Operation`이 설정된 경우 실제 Sync를 수행한다. API Server에서 직접 최신 상태를 조회하고 (Informer 캐시 미사용), Wave 순서대로 Hook을 실행하며, 완료 후 이력을 저장한다.

```mermaid
sequenceDiagram
    participant OQ as appOperationQueue
    participant POI as processAppOperationQueueItem
    participant K8S as Kubernetes API<br/>(freshApp 조회)
    participant PRO as processRequestedAppOperation
    participant SAS as SyncAppState
    participant CA as CompareAppState
    participant SC as syncCtx.Sync()<br/>(gitops-engine)
    participant KA as kubectl apply
    participant RH as persistRevisionHistory

    OQ->>POI: appKey dequeue

    POI->>POI: appInformer.GetIndexer().GetByKey(appKey)

    alt app.Operation != nil
        POI->>K8S: Applications(ns).Get(name, GetOptions{})
        Note over K8S: Informer 캐시 아닌<br/>API Server에서 직접 최신 조회<br/>(stale 데이터 방지)
        K8S-->>POI: freshApp
        POI->>PRO: processRequestedAppOperation(freshApp)
    else app.DeletionTimestamp != nil
        POI->>POI: finalizeApplicationDeletion()
    end

    PRO->>PRO: isOperationInProgress(app) 확인

    alt 진행 중인 작업 있음
        alt Phase == Terminating
            PRO->>PRO: 종료 흐름 진행
        else syncTimeout 초과
            PRO->>PRO: Phase = Terminating, message = "timeout"
        else Running + FinishedAt != nil (재시도 대기)
            PRO->>PRO: NextRetryAt 계산 → retryAfter > 0이면 skip
        end
    else 새 Operation
        PRO->>PRO: state = NewOperationState(*app.Operation)
        PRO->>PRO: setOperationState(app, state) — Phase = Running
        PRO->>PRO: syncTimeout 설정된 경우 appOperationQueue.AddAfter(syncTimeout)
    end

    PRO->>PRO: getAppProj(app) — AppProject 로드
    PRO->>SAS: SyncAppState(app, project, state)

    Note over SAS: syncWindowPreventsSync() 검사
    Note over SAS: state.SyncResult 초기화

    SAS->>CA: CompareAppState(app, project, revisions, sources, noCache=false, forceResolve=true, ...)
    CA-->>SAS: compareResult

    SAS->>SAS: 공유 리소스 충돌 검사 (FailOnSharedResource 옵션)
    SAS->>SAS: 오류 조건 검사 (ComparisonError, InvalidSpecError)
    SAS->>SAS: GetDestinationCluster() — 대상 클러스터 조회

    SAS->>SC: sync.NewSyncContext(revision, compareResult.managedResources, ...)
    SAS->>SC: syncCtx.Sync()

    Note over SC: Wave 순서대로 실행:<br/>PreSync → Sync → PostSync<br/>SyncFail (실패 시)

    loop 각 Wave (0, 1, 2, ...)
        SC->>SC: PreSync hooks 실행 (wave N)
        Note over SC: Hook type: PreSync<br/>wave annotation 기반 정렬

        SC->>KA: kubectl apply (wave N 리소스들)
        Note over KA: ServerSideApply / ClientSideApply<br/>리소스별 apply 방식 결정

        alt apply 실패
            SC->>SC: executeSyncFailPhase(syncFailTasks, ...)
            Note over SC: SyncFail hook 실행
        end

        SC->>SC: PostSync hooks 실행 (wave N)

        alt syncWaveHook 설정됨
            SC->>SC: syncWaveHook(phase, wave, finalWave)
        end
    end

    SC-->>SAS: sync 완료

    alt state.Phase == Succeeded
        SAS->>RH: persistRevisionHistory(app, revision, source, revisions, sources, isMultiSource, startedAt, initiatedBy)
        Note over RH: app.Status.History에 이력 추가<br/>RevisionHistoryLimit 초과분 정리
    end

    SAS-->>PRO: 완료

    PRO->>PRO: setOperationState(app, state)

    alt state.Phase.Completed() && !DryRun
        alt InitiatedBy.Automated
            PRO->>PRO: requestAppRefresh(CompareWithLatest)
            Note over PRO: 자동 동기화는 ls-remote 과부하 방지를 위해<br/>ForceResolve 미사용
        else 수동 동기화
            PRO->>PRO: requestAppRefresh(CompareWithLatestForceResolve)
            Note over PRO: UI 최신 상태 반영을 위해<br/>ForceResolve 사용 (#18153)
        end
    end
```

### Sync Wave 실행 순서 상세

```
소스: gitops-engine/pkg/sync/sync_context.go:450 (sc *syncContext) Sync()

Wave 0 → Wave 1 → Wave 2 → ...

각 Wave 내 실행 순서:
1. PreSync hooks (wave N)
2. 일반 리소스 apply (wave N)
3. PostSync hooks (wave N)

실패 시:
- SyncFail hooks 실행 (executeSyncFailPhase)
- 이전 wave의 실패한 태스크 전달

Hook 유형:
  argocd.argoproj.io/hook: PreSync    — sync 이전 실행 (DB 마이그레이션 등)
  argocd.argoproj.io/hook: Sync       — sync 중 실행
  argocd.argoproj.io/hook: PostSync   — sync 완료 후 실행 (smoke test 등)
  argocd.argoproj.io/hook: SyncFail   — sync 실패 시 실행 (롤백, 알림 등)
```

---

## 4. Manifest 생성 흐름

**소스**: `reposerver/repository/repository.go` — `GenerateManifest()`, `runRepoOperation()`, `GenerateManifests()`

### 개요

Application Controller가 상태 비교를 위해 Repo Server에 gRPC로 Manifest 생성을 요청한다. Repo Server는 소스 타입(Git/Helm/OCI)을 분기하고, 캐시를 확인한 뒤 세마포어를 획득하여 병렬성을 제한하고, 실제 Manifest를 생성하여 캐시에 저장한다.

```mermaid
sequenceDiagram
    participant CTRL as Application Controller<br/>(CompareAppState)
    participant GW as repo-server gRPC<br/>(GenerateManifest)
    participant RRO as runRepoOperation
    participant CACHE as ManifestCache<br/>(Redis)
    participant SEM as parallelismLimitSemaphore
    participant GIT as git.Client
    participant HELM as helm.Client
    participant OCI as oci.Client
    participant GEN as GenerateManifests

    CTRL->>GW: GenerateManifest(ManifestRequest)
    Note over GW: ref-only 소스 스킵 처리<br/>(path도 chart도 없는 ref 소스)

    GW->>GW: cacheFn 클로저 정의<br/>(getManifestCacheEntry)
    GW->>GW: operation 클로저 정의<br/>(runManifestGen)

    GW->>RRO: runRepoOperation(ctx, revision, repo, source, verifyCommit, cacheFn, operation, settings, ...)

    Note over RRO: 소스 타입 분기 (switch)

    alt source.IsOCI()
        RRO->>OCI: newOCIClientResolveRevision(ctx, repo, revision, noCache)
        OCI-->>RRO: ociClient, resolvedRevision
    else source.IsHelm()
        RRO->>HELM: newHelmClientResolveRevision(repo, revision, source.Chart, noCache)
        HELM-->>RRO: helmClient, resolvedRevision
    else Git (default)
        RRO->>GIT: newClientResolveRevision(repo, revision, withCache)
        GIT-->>RRO: gitClient, resolvedRevision
    end

    alt !settings.noCache (캐시 활성화)
        RRO->>CACHE: cacheFn(revision, repoRefs, firstInvocation=true)
        alt 캐시 히트
            CACHE-->>RRO: (true, cachedResponse, nil)
            RRO-->>GW: 캐시된 ManifestResponse 반환
            GW-->>CTRL: 캐시 응답
        end
    end

    Note over RRO: metricsServer.IncPendingRepoRequest(repo.Repo)

    alt settings.sem != nil
        RRO->>SEM: sem.Acquire(ctx, 1)
        Note over SEM: 병렬 Manifest 생성 수 제한<br/>(--parallelism-limit 설정값)
        SEM-->>RRO: 획득 완료
    end

    alt source.IsOCI()
        RRO->>OCI: ociClient.Extract(ctx, revision) → ociPath
        RRO->>RRO: CheckOutOfBoundsSymlinks(ociPath)
        RRO->>GEN: operation(ociPath, revision, revision, ctxSrc)
    else source.IsHelm()
        RRO->>HELM: helm pull → chartPath
        RRO->>GEN: operation(chartPath, revision, cacheKey, ctxSrc)
    else Git
        RRO->>GIT: repoLock.Lock() — 동일 저장소 직렬화
        RRO->>GIT: checkoutRevision(gitClient, revision)

        alt verifyCommit
            RRO->>GIT: GPG 서명 검증
            Note over GIT: gpg --verify 실행<br/>실패 시 operation 중단
        end

        RRO->>CACHE: cacheFn(commitSHA, repoRefs, false) — double-check
        alt 캐시 히트 (다른 goroutine이 먼저 생성)
            CACHE-->>RRO: 캐시 응답 반환
        else 캐시 미스
            RRO->>GEN: operation(repoRoot, commitSHA, cacheKey, ctxSrc)
        end
        RRO->>GIT: repoLock.Unlock()
    end

    GEN->>GEN: GenerateManifests(ctx, appPath, repoRoot, revision, q, ...)
    Note over GEN: GetAppSourceType() — 소스 타입 감지<br/>(Helm/Kustomize/Plugin/Directory)

    alt Helm 소스
        GEN->>GEN: helmTemplate(appPath, q) → YAML 목록
        Note over GEN: helm template 실행<br/>values 파일 적용
    else Kustomize 소스
        GEN->>GEN: k.Build() — kustomize build 실행
        Note over GEN: 이미지 오버라이드 적용
    else Config Management Plugin (CMP)
        GEN->>GEN: runConfigManagementPluginSidecars()
        Note over GEN: 사이드카 플러그인에 tar 전송<br/>응답 대기 (promise 패턴)
    else 디렉토리 소스
        GEN->>GEN: findManifests(appPath, q)
        Note over GEN: .yaml/.yml/.json 파일 재귀 검색
    end

    GEN->>GEN: Resource tracking label 주입
    Note over GEN: app.kubernetes.io/instance: {appName}<br/>또는 설정된 appInstanceLabelKey

    GEN-->>RRO: ManifestResponse

    RRO->>CACHE: 결과 캐싱 (commitSHA 키)
    Note over CACHE: TTL: cache.repoCacheExpiration<br/>기본 24시간

    RRO-->>GW: ManifestResponse
    GW-->>CTRL: ManifestResponse
```

### 캐시 이중 확인 (Double-Check Locking) 패턴

```
소스: reposerver/repository/repository.go:365-368, 그리고 Git checkout 이후

1차 캐시 확인: repoLock 획득 전 (revision이 이미 있으면 바로 반환)
2차 캐시 확인: repoLock 획득 후, checkout 이후 (다른 goroutine이 먼저 완료한 경우 처리)

이 패턴으로 동일 revision에 대한 중복 Git 작업을 방지한다.
```

### 소스 타입별 처리 요약

| 소스 타입 | 판별 방법 | 생성 함수 | 특이사항 |
|----------|----------|----------|---------|
| OCI | `source.IsOCI()` | `ociClient.Extract()` | symlink 검사 필수 |
| Helm | `source.IsHelm()` | `helmTemplate()` | chart 이름으로 revision 분리 |
| Kustomize | `kustomize.yaml` 존재 | `k.Build()` | 이미지 오버라이드 가능 |
| Plugin (CMP) | 플러그인 설정 | `runConfigManagementPluginSidecars()` | promise 패턴, tar 전송 |
| 디렉토리 | 기본값 | `findManifests()` | 재귀 YAML 탐색 |

---

## 5. 인증/인가 흐름

**소스**: `server/server.go` — `(server *ArgoCDServer) Authenticate()`
**소스**: `util/session/sessionmanager.go` — `(mgr *SessionManager) VerifyToken()`
**소스**: `util/rbac/rbac.go` — `(e *Enforcer) EnforceErr()`

### 개요

모든 gRPC 요청은 `Authenticate()` 인터셉터를 통과한다. 토큰을 추출하고 검증한 뒤 Claims를 컨텍스트에 주입한다. 이후 각 API 핸들러에서 `EnforceErr()`로 RBAC 정책을 검사한다.

```mermaid
sequenceDiagram
    actor User
    participant CLI as argocd CLI
    participant INTC as gRPC 인터셉터<br/>(Authenticate)
    participant GC as getClaims
    participant GT as getToken
    participant SM as SessionManager<br/>(VerifyToken)
    participant HMAC as mgr.Parse()<br/>(HMAC-SHA256)
    participant OIDC as OIDC Provider<br/>(prov.Verify)
    participant CTX as context.WithValue<br/>("claims")
    participant RBAC as Enforcer<br/>(EnforceErr)
    participant CB as Casbin<br/>(globMatchFunc)
    participant API as API Handler<br/>(예: Server.Create)

    User->>CLI: argocd app create ...
    CLI->>INTC: gRPC 요청 (Authorization: Bearer <token>)

    alt server.DisableAuth == true
        INTC-->>CLI: 인증 스킵
    end

    INTC->>GC: getClaims(ctx)

    GC->>GT: getToken(metadata)
    Note over GT: 토큰 소스 우선순위:<br/>1. gRPC metadata Authorization 헤더<br/>2. Cookie (argocd.token)

    GT-->>GC: tokenString

    alt tokenString == ""
        GC-->>INTC: ErrNoSession
        INTC-->>CLI: Unauthenticated error
    end

    GC->>SM: VerifyToken(ctx, tokenString)

    Note over SM: parser.ParseUnverified(tokenString) 로<br/>issuer 필드만 먼저 추출

    alt issuer == "argocd" (SessionManagerClaimsIssuer)
        SM->>HMAC: mgr.Parse(tokenString)
        Note over HMAC: jwt.ParseWithClaims()<br/>서명 알고리즘: HMAC-SHA256<br/>secret: argocd-secret의 server.secretkey
        HMAC-->>SM: (claims, newToken, err)
    else issuer == OIDC Provider (Dex, Okta 등)
        SM->>OIDC: mgr.provider()
        OIDC-->>SM: prov
        SM->>OIDC: prov.Verify(ctx, tokenString, argoSettings)
        Note over OIDC: OIDC ID Token 검증:<br/>- 서명 검증<br/>- aud 확인<br/>- exp 확인
        alt 토큰 만료
            OIDC-->>SM: TokenExpiredError
            SM-->>GC: claims{iss: "sso"}, ErrTokenVerification
        end
        OIDC-->>SM: idToken
        SM->>SM: idToken.Claims(&claims) — 클레임 파싱
        SM-->>GC: (claims, "", nil)
    end

    GC-->>INTC: (claims, newToken, nil)

    alt newToken != "" (토큰 갱신)
        INTC->>INTC: grpc.SendHeader(renewTokenKey: newToken)
        Note over INTC: 만료 임박 토큰 자동 갱신<br/>Set-Cookie 헤더로 응답
    end

    INTC->>CTX: context.WithValue(ctx, "claims", claims)
    INTC->>API: API Handler 호출 (claims 포함된 ctx 전달)

    API->>RBAC: EnforceErr(ctx.Value("claims"), resource, action, object)
    Note over RBAC: rvals = [claims, "applications", "create", "myproject/myapp"]

    RBAC->>CB: Casbin enforce(getCasbinEnforcer(), defaultRole, claimsEnforcerFunc, rvals...)
    Note over CB: Casbin 모델:<br/>g(sub, role) — 역할 상속<br/>p(role, res, act) — 정책<br/>globMatch(res, p.res) && globMatch(act, p.act) && globMatch(obj, p.obj)

    alt 정책 허용
        CB-->>RBAC: true
        RBAC-->>API: nil (허용)
        API->>API: 핵심 로직 실행
    else 정책 거부
        CB-->>RBAC: false
        RBAC-->>API: PermissionDenied error
        API-->>CLI: gRPC PermissionDenied
    end
```

### 토큰 검증 분기 상세

```
소스: util/session/sessionmanager.go:550

func (mgr *SessionManager) VerifyToken(ctx, tokenString) (jwt.Claims, string, error) {
    parser := jwt.NewParser(jwt.WithoutClaimsValidation())
    _, _, err := parser.ParseUnverified(tokenString, &claims)
    // issuer만 추출 (서명 검증 없음)

    issuer := claims["iss"].(string)
    switch issuer {
    case SessionManagerClaimsIssuer:  // "argocd"
        return mgr.Parse(tokenString)  // HMAC-SHA256 검증
    default:
        prov, _ := mgr.provider()
        idToken, err := prov.Verify(ctx, tokenString, argoSettings)  // OIDC 검증
        ...
    }
}
```

### Casbin RBAC 모델

```
[request_definition]
r = sub, res, act, obj

[policy_definition]
p = sub, res, act, obj

[role_definition]
g = _, _        # 역할 상속

[policy_effect]
e = some(where (p.eft == allow)) && !some(where (p.eft == deny))

[matchers]
m = g(r.sub, p.sub) && globMatch(r.res, p.res) && globMatch(r.act, p.act) && globMatch(r.obj, p.obj)
```

기본 정책 예시:
```
p, role:admin, applications, *, */*
p, role:readonly, applications, get, */*
g, admin, role:admin
```

---

## 6. Auto-Sync 흐름

**소스**: `controller/appcontroller.go` — `(ctrl *ApplicationController) autoSync()`

### 개요

`processAppRefreshQueueItem()` 내에서 상태 비교(CompareAppState) 결과가 OutOfSync이면 `autoSync()`를 호출한다. 여러 Guard Condition을 통과해야 실제 동기화 Operation이 생성되며, Self-Heal 경로는 별도의 백오프 로직을 사용한다.

```mermaid
sequenceDiagram
    participant CTRL as processAppRefreshQueueItem
    participant AS as autoSync
    participant SP as SyncWindow 검사<br/>(SyncWindows.Matches)
    participant AA as alreadyAttemptedSync
    participant SH as selfHealRemainingBackoff
    participant K8S as Kubernetes API<br/>(Operation 설정)
    participant OQ as appOperationQueue

    CTRL->>SP: project.Spec.SyncWindows.Matches(app).CanSync(false)

    alt SyncWindow 차단
        SP-->>CTRL: false
        CTRL->>CTRL: log "Sync prevented by sync window"
        Note over CTRL: autoSync 호출 자체를 건너뜀
    else SyncWindow 허용
        SP-->>CTRL: true
        CTRL->>AS: autoSync(app, syncStatus, resources, shouldCompareRevisions)
    end

    Note over AS: Guard Condition 순서대로 검사

    alt app.Spec.SyncPolicy == nil || !IsAutomatedSyncEnabled()
        AS-->>CTRL: nil, 0 (자동 동기화 정책 없음)
    end

    alt app.Operation != nil
        AS-->>CTRL: nil, 0 (이미 진행 중인 Operation)
        Note over AS: log "Skipping auto-sync: another operation is in progress"
    end

    alt app.DeletionTimestamp != nil
        AS-->>CTRL: nil, 0 (삭제 진행 중)
        Note over AS: log "Skipping auto-sync: deletion in progress"
    end

    alt syncStatus.Status != OutOfSync
        AS-->>CTRL: nil, 0 (이미 동기화됨)
        Note over AS: log "Skipping auto-sync: application status is {status}"
    end

    alt !app.Spec.SyncPolicy.Automated.Prune
        AS->>AS: requirePruneOnly 확인
        alt 모든 변경이 prune만 필요
            AS-->>CTRL: nil, 0 (prune 비활성화 + prune만 필요)
            Note over AS: log "need to prune extra resources only but automated prune is disabled"
        end
    end

    Note over AS: op = Operation{Sync: {...}, InitiatedBy: {Automated: true}}

    AS->>AA: alreadyAttemptedSync(app, desiredRevisions, shouldCompareRevisions)
    AA-->>AS: (alreadyAttempted, lastAttemptedRevisions, lastAttemptedPhase)

    alt alreadyAttempted == true
        alt !lastAttemptedPhase.Successful()
            AS-->>CTRL: SyncError condition (이전 시도 실패)
            Note over AS: log "Skipping auto-sync: failed previous sync attempt"
        else !app.Spec.SyncPolicy.Automated.SelfHeal
            AS-->>CTRL: nil, 0 (이미 시도했고 SelfHeal 비활성)
            Note over AS: log "most recent sync already to {revision}"
        else SelfHeal 활성화
            Note over AS: SelfHeal 경로:<br/>이전 성공 sync 이후 클러스터 drift 감지

            AS->>AS: op.Sync.SelfHealAttemptsCount 복원
            AS->>SH: selfHealRemainingBackoff(app, selfHealAttemptsCount)

            alt remainingTime > 0 (백오프 중)
                SH-->>AS: remainingTime
                AS->>AS: requestAppRefresh(CompareWithLatest, &remainingTime)
                AS-->>CTRL: nil, 0 (백오프 대기)
                Note over AS: log "Skipping auto-sync: timeout (retrying in {remainingTime})"
            else 백오프 완료
                SH-->>AS: 0
                AS->>AS: op.Sync.SelfHealAttemptsCount++
                AS->>AS: 비동기 리소스만 op.Sync.Resources에 추가
            end
        end
    end

    alt Prune 활성화 + !AllowEmpty + 모든 리소스가 prune 대상
        AS-->>CTRL: SyncError condition (빈 애플리케이션 방지)
        Note over AS: log "auto-sync will wipe out all resources"
    end

    AS->>K8S: setAppOperation(app, op)
    Note over K8S: app.Operation = &op<br/>PATCH를 통해 Kubernetes에 저장<br/>이로 인해 processAppOperationQueueItem에서 처리됨

    K8S-->>AS: opDuration

    AS-->>CTRL: (nil, opDuration)
    Note over CTRL: 완료 후 OQ에는 이미 defer에서 push됨
```

### Guard Condition 요약표

| 조건 | 건너뜀 이유 | 코드 위치 |
|------|----------|----------|
| `SyncPolicy == nil` 또는 AutoSync 비활성 | 자동 동기화 정책 없음 | `appcontroller.go:2184` |
| `app.Operation != nil` | 이미 Operation 진행 중 | `appcontroller.go:2188` |
| `app.DeletionTimestamp != nil` | 삭제 진행 중 | `appcontroller.go:2192` |
| `syncStatus != OutOfSync` | 이미 동기화됨 | `appcontroller.go:2199` |
| Prune 비활성 + prune만 필요 | prune 정책 미충족 | `appcontroller.go:2204` |
| `alreadyAttempted` + 실패 | 이전 실패 재시도 방지 | `appcontroller.go:2247` |
| `alreadyAttempted` + SelfHeal 비활성 | 반복 동기화 방지 | `appcontroller.go:2253` |
| SelfHeal 백오프 중 | 지수 백오프 | `appcontroller.go:2264` |
| AllowEmpty=false + 모두 prune | 전체 삭제 방지 | `appcontroller.go:2283` |

---

## 7. ApplicationSet 흐름

**소스**: `applicationset/controllers/applicationset_controller.go` — `(r *ApplicationSetReconciler) Reconcile()`

### 개요

ApplicationSet Controller는 controller-runtime 프레임워크 위에서 동작한다. ApplicationSet 리소스가 변경될 때마다 Reconcile이 호출되어 Generator들이 파라미터를 생성하고, Template을 통해 Application 스펙을 만들며, 현재 상태와 비교하여 Create/Update/Delete를 수행한다.

```mermaid
sequenceDiagram
    participant K8S as Kubernetes<br/>(ApplicationSet 이벤트)
    participant REC as ApplicationSetReconciler<br/>.Reconcile()
    participant GEN as Generators<br/>(List/Cluster/Git/Matrix/...)
    participant TPL as template.GenerateApplications
    participant VAL as validateGeneratedApplications
    participant CURR as getCurrentApplications
    participant COU as createOrUpdateInCluster
    participant DEL as deleteInCluster
    participant STAT as setApplicationSetStatusCondition

    K8S->>REC: Reconcile(ctx, req{NamespacedName})

    REC->>K8S: r.Get(ctx, req.NamespacedName, &applicationSetInfo)

    alt ApplicationSet 없음 (삭제됨)
        K8S-->>REC: NotFound
        REC-->>K8S: ctrl.Result{}, nil
    end

    alt DeletionTimestamp 설정됨
        Note over REC: 삭제 처리 분기
        alt isProgressiveSyncDeletionOrderReversed
            REC->>CURR: getCurrentApplications(ctx, applicationSetInfo)
            CURR-->>REC: currentApplications
            REC->>REC: performReverseDeletion() — 역순 삭제
        end
        REC->>K8S: controllerutil.RemoveFinalizer() + Update()
        REC-->>K8S: ctrl.Result{}, nil
    end

    REC->>REC: migrateStatus() — 상태 마이그레이션

    Note over REC: utils.CheckInvalidGenerators() — 알 수 없는 Generator 경고

    REC->>TPL: template.GenerateApplications(logCtx, applicationSetInfo, r.Generators, r.Renderer, r.Client)

    loop 각 Generator
        TPL->>GEN: GenerateParams(appSetGenerator, applicationSetInfo, r.Client)
        Note over GEN: Generator 종류:<br/>List: 직접 파라미터 목록<br/>Cluster: 클러스터 목록에서<br/>Git: Git 파일/디렉토리에서<br/>SCMProvider: GitHub/GitLab 저장소에서<br/>Matrix: Generator 교차곱<br/>Merge: Generator 병합
        GEN-->>TPL: []map[string]string (파라미터 목록)

        loop 각 파라미터 세트
            TPL->>TPL: r.Renderer.RenderTemplateParams(appSetTemplate, params)
            Note over TPL: Go template 엔진으로<br/>{{.path.basename}}, {{.cluster.name}} 등 치환
            TPL-->>TPL: Application 스펙 생성
        end
    end

    TPL-->>REC: generatedApplications []Application

    alt 생성 오류
        REC->>STAT: setApplicationSetStatusCondition(ErrorOccurred)
        REC-->>K8S: ctrl.Result{RequeueAfter: ReconcileRequeueOnValidationError}, nil
    end

    REC->>VAL: validateGeneratedApplications(ctx, generatedApplications, applicationSetInfo)
    Note over VAL: 각 Application 스펙 유효성 검사<br/>중복 Application 이름 검사<br/>프로젝트 권한 검사
    VAL-->>REC: validateErrors map[string]error

    REC->>CURR: getCurrentApplications(ctx, applicationSetInfo)
    CURR->>K8S: List Applications with ownerReference 필터
    K8S-->>CURR: []Application (현재 소유 중인 Application들)
    CURR-->>REC: currentApps

    Note over REC: 비교: generatedApplications vs currentApps

    REC->>COU: createOrUpdateInCluster(ctx, logCtx, applicationSetInfo, desiredApplications)

    loop 각 desired Application
        alt 존재하지 않음
            COU->>K8S: Create(ctx, application)
        else 존재하고 스펙 변경됨
            COU->>K8S: Update(ctx, application)
            Note over COU: ownerReference, labels, annotations 포함 업데이트
        else 스펙 동일
            Note over COU: 변경 없음, 건너뜀
        end
    end

    REC->>DEL: deleteInCluster(ctx, logCtx, applicationSetInfo, desiredApplications)

    loop 현재 소유 중이지만 desired에 없는 Application
        alt policy.AllowDelete()
            DEL->>K8S: Delete(ctx, application)
        else 삭제 불허 정책
            DEL->>DEL: ownerReference 제거만 (고아화)
        end
    end

    REC->>STAT: setApplicationSetStatusCondition(ResourcesUpToDate or error 상태)
    REC-->>K8S: ctrl.Result{}, nil
```

### Generator 종류별 파라미터 생성

```
소스: applicationset/generators/ 디렉토리

List Generator:
  입력: spec.generators.list.elements
  출력: 각 element가 파라미터 세트

Cluster Generator:
  입력: Kubernetes Secret (argocd 클러스터 등록 정보)
  출력: {name, server, metadata.*} 파라미터

Git Generator:
  입력: Git 저장소의 디렉토리 구조 또는 JSON/YAML 파일
  출력: {path, path.basename, ...} 또는 파일 내용 파라미터

SCMProvider Generator:
  입력: GitHub/GitLab/Bitbucket 조직
  출력: {organization, repository, branch, url, ...}

Matrix Generator:
  입력: 두 Generator의 조합
  출력: 교차곱 (N * M 파라미터 세트)

Merge Generator:
  입력: 여러 Generator + 병합 키
  출력: 키 기반 병합 파라미터 세트
```

---

## 8. Webhook 흐름

**소스**: `util/webhook/webhook.go` — `Handler()`, `HandleEvent()`

### 개요

GitHub/GitLab/Bitbucket 등의 Git 저장소가 Push 이벤트를 보내면 Argo CD는 `/api/webhook` 엔드포인트에서 이를 수신한다. 시크릿 검증 후 영향받는 Application을 식별하여 RefreshApp annotation을 설정함으로써 즉각적인 Reconciliation을 트리거한다.

```mermaid
sequenceDiagram
    participant GH as Git 저장소<br/>(GitHub/GitLab/Bitbucket)
    participant HTTP as HTTP Server<br/>/api/webhook
    participant HDL as ArgoCDWebhookHandler<br/>.Handler()
    participant WQ as Worker Pool<br/>(webhookParallelism)
    participant HEV as HandleEvent()
    participant ARI as affectedRevisionInfo()
    participant APL as appLister
    participant RF as argo.RefreshApp
    participant CACHE as Manifest Cache<br/>(Redis)
    participant INF as appInformer<br/>(→ appRefreshQueue)

    GH->>HTTP: POST /api/webhook
    Note over HTTP: r.Body = MaxBytesReader(w, body, maxPayloadSize)

    HTTP->>HDL: Handler(w, r)

    Note over HDL: HTTP 헤더로 Git 제공자 판별

    alt X-GitHub-Event 헤더
        HDL->>HDL: github.Parse(r, PushEvent, PingEvent)
        Note over HDL: HMAC-SHA256 시크릿 검증<br/>ErrHMACVerificationFailed → SecurityHigh 로그
    else X-Gitlab-Event 헤더
        HDL->>HDL: gitlab.Parse(r, PushEvents, TagEvents, SystemHookEvents)
        Note over HDL: GitLab Token 검증<br/>ErrGitLabTokenVerificationFailed
    else X-Hook-UUID 헤더 (Bitbucket Cloud)
        HDL->>HDL: bitbucket.Parse(r, RepoPushEvent)
        Note over HDL: UUID 검증
    else X-Event-Key 헤더 (Bitbucket Server)
        HDL->>HDL: bitbucketserver.Parse(r, RepositoryReferenceChangedEvent)
        Note over HDL: HMAC 검증
    else X-Vss-Activityid 헤더 (Azure DevOps)
        HDL->>HDL: azuredevops.Parse(r, GitPushEventType)
        Note over HDL: Basic Auth 검증
    else X-Gogs-Event 헤더 (Gogs)
        HDL->>HDL: gogs.Parse(r, PushEvent)
    else 알 수 없는 이벤트
        HDL-->>GH: 400 Bad Request "Unknown webhook event"
    end

    alt 파싱 오류
        alt 페이로드 크기 초과
            HDL-->>GH: 400 "payload is too large (must be under N MB)"
        else 기타 오류
            HDL-->>GH: 400 "Webhook processing failed: {error}"
        end
    end

    HDL->>WQ: workerChan <- payload
    Note over WQ: 비동기 처리로 Git 저장소에 빠른 응답<br/>webhookParallelism: 기본 10

    HDL-->>GH: 200 OK (즉시 응답)

    WQ->>HEV: HandleEvent(payload)

    HEV->>ARI: affectedRevisionInfo(payload)
    Note over ARI: payload에서 추출:<br/>- webURLs (저장소 URL 목록)<br/>- revision (커밋 SHA / 브랜치)<br/>- changedFiles (변경된 파일 목록)<br/>- touchedHead (HEAD 브랜치 변경 여부)<br/>- change.shaBefore, shaAfter

    alt webURLs == empty
        HEV->>HEV: log "Ignoring webhook event"
        Note over HEV: 처리할 URL 없음, 종료
    end

    HEV->>APL: appLister.Applications(nsFilter).List(labels.Everything())
    Note over APL: nsFilter: 단일 네임스페이스 또는 ""(전체)<br/>appNs 설정에 따라 결정

    HEV->>HEV: nsFilter로 filteredApps 필터링
    Note over HEV: app.Namespace == a.ns 또는<br/>glob.MatchStringInList(a.appNs, app.Namespace)

    loop 각 webURL
        HEV->>HEV: GetWebURLRegex(webURL) — 정규식 생성

        loop 각 filteredApp
            HEV->>HEV: app.Spec.GetSources() + SourceHydrator.GetDrySource()

            loop 각 source
                HEV->>HEV: sourceRevisionHasChanged(source, revision, touchedHead)?
                HEV->>HEV: sourceUsesURL(source, webURL, repoRegexp)?

                alt 리비전 변경 && URL 일치
                    HEV->>HEV: GetSourceRefreshPaths(&app, source) — refresh 경로 조회
                    HEV->>HEV: AppFilesHaveChanged(refreshPaths, changedFiles)?

                    alt 파일 변경됨 (refresh 필요)
                        HEV->>RF: argo.RefreshApp(namespacedAppInterface, app.Name, RefreshTypeNormal, hydrate)
                        Note over RF: app.Annotations["argocd.argoproj.io/refresh"] = "normal"<br/>PATCH로 Kubernetes에 저장
                        RF-->>HEV: (app, error)
                        Note over HEV: break — 같은 앱에 대해 소스 반복 중단
                    else 파일 미변경 (SHA 변경만)
                        HEV->>CACHE: storePreviouslyCachedManifests(app, change, ...)
                        Note over CACHE: shaBefore의 캐시를 shaAfter 키로 복사<br/>불필요한 Manifest 재생성 방지
                    end
                end
            end
        end
    end

    Note over INF: annotation 설정된 App이<br/>appInformer Update 이벤트 발생
    INF->>INF: appRefreshQueue에 추가
    Note over INF: → processAppRefreshQueueItem 실행<br/>→ needRefreshAppStatus() → true<br/>→ CompareAppState() 즉시 수행
```

### Webhook 처리 전체 타임라인

```
T+0ms:   Git Push → GitHub가 webhook POST 전송
T+50ms:  Argo CD Handler() 수신 → 시크릿 검증 → 200 OK 즉시 응답
T+50ms:  workerChan에 payload 전달 (비동기)
T+60ms:  HandleEvent() 시작 → affectedRevisionInfo() 파싱
T+80ms:  영향받는 Application 식별 → RefreshApp() PATCH 호출
T+100ms: Kubernetes Informer가 annotation 변경 감지
T+100ms: appRefreshQueue에 Application 추가
T+200ms: processAppRefreshQueueItem() 실행 시작
T+500ms: CompareAppState() → Repo Server gRPC → manifest 생성
T+1000ms: 상태 비교 완료 → persistAppStatus()
```

### Webhook 보안 검증 방식

| Git 제공자 | 헤더 | 검증 방식 |
|-----------|------|---------|
| GitHub | `X-GitHub-Event` | HMAC-SHA256 (secret token) |
| GitLab | `X-Gitlab-Event` | Secret Token 비교 |
| Bitbucket Cloud | `X-Hook-UUID` | UUID 비교 |
| Bitbucket Server | `X-Event-Key` | HMAC-SHA256 |
| Azure DevOps | `X-Vss-Activityid` | Basic Auth |
| Gogs | `X-Gogs-Event` | HMAC-SHA256 |

---

## 흐름 간 연결 관계

```
                          ┌─────────────────────────────────────────────────────┐
                          │                 정상 운영 사이클                      │
                          │                                                       │
  사용자/CLI              │  Webhook                                              │
      │                   │     │                                                  │
      ▼                   │     ▼                                                  │
  [1. App 생성]           │  [8. Webhook]──────RefreshApp──────┐                  │
      │                   │                                     │                  │
      │ appclientset       │                                     ▼                  │
      │ .Create()          │                           appInformer Update           │
      │                   │                                     │                  │
      ▼                   │                                     ▼                  │
  K8s에 저장              │         ┌──────────────────[2. Reconciliation]──────── ┤
                          │         │                           │                  │
  informer 감지            │         │               needRefreshAppStatus()         │
      │                   │         │                           │                  │
      ▼                   │         │                           ▼                  │
  appRefreshQueue         │         │               CompareAppState()              │
      │                   │         │                           │                  │
      ▼                   │         │               [4. Manifest 생성]             │
  [2. Reconciliation] ◄───┘         │               (repo-server gRPC)            │
      │                             │                           │                  │
      ├──OutOfSync──────────────────┤               autoSync() 판단               │
      │                             │                           │                  │
      ▼                             │                           ▼                  │
  autoSync()              │         │               [6. Auto-Sync]                 │
      │                   │         │                           │                  │
      ▼                   │         │               setAppOperation()              │
  appOperationQueue       │         │                           │                  │
      │                   │         │                           ▼                  │
      ▼                   │         └──────────────────[3. Sync 실행]──────────── ┤
  [3. Sync 실행]          │                                     │                  │
      │                   │                           SyncAppState()               │
      │                   │                           syncCtx.Sync()              │
      │                   │                           Wave 순서 실행              │
      │                   │                                     │                  │
      ▼                   │                           성공 시 requestAppRefresh   │
  완료 → requestAppRefresh│                                     │                  │
      │                   └─────────────────────────────────────┘                  │
      └─────────────────────────────────────────────────────────────────────────┘

[5. 인증/인가]: 모든 API 요청에 횡단적으로 적용 (gRPC 인터셉터)
[7. ApplicationSet]: 독립적인 컨트롤러, 복수의 Application을 자동 관리
```

---

## 핵심 데이터 구조 참조

### Application 상태 (SyncStatus)

```go
// vendor/github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1/types.go

type SyncStatus struct {
    Status     SyncStatusCode `json:"status"`      // Synced / OutOfSync / Unknown
    ComparedTo ComparedTo     `json:"comparedTo"`
    Revision   string         `json:"revision,omitempty"`
    Revisions  []string       `json:"revisions,omitempty"`
}

// SyncStatusCode 값
const (
    SyncStatusCodeUnknown  SyncStatusCode = "Unknown"
    SyncStatusCodeSynced   SyncStatusCode = "Synced"
    SyncStatusCodeOutOfSync SyncStatusCode = "OutOfSync"
)
```

### Operation (동기화 요청)

```go
type Operation struct {
    Sync        *SyncOperation     `json:"sync,omitempty"`
    InitiatedBy OperationInitiator `json:"initiatedBy,omitempty"`
    Retry       RetryStrategy      `json:"retry,omitempty"`
}

type SyncOperation struct {
    Revision             string              `json:"revision,omitempty"`
    Prune                bool                `json:"prune,omitempty"`
    SyncOptions          SyncOptions         `json:"syncOptions,omitempty"`
    SelfHealAttemptsCount int64              `json:"selfHealAttemptsCount,omitempty"`
    Resources            []SyncOperationResource `json:"resources,omitempty"`
}
```

### CompareWith 레벨과 동작

```
레벨 0 (ComparisonWithNothing): Git 호출 없음, 캐시된 리소스로 트리 업데이트만
레벨 1 (CompareWithRecent):     기존 revision 사용, repo 호출은 하되 ls-remote 생략
레벨 2 (CompareWithLatest):     최신 revision 조회 (ls-remote), manifest 재생성
레벨 3 (CompareWithLatestForceResolve): 레벨 2 + ref 소스의 revision도 강제 재분석

Webhook, 수동 refresh → 레벨 3
spec.source 변경 → 레벨 3
주기적 갱신 (softExpired) → 레벨 2
하드 만료 (hardExpired) → 레벨 2 + RefreshTypeHard
캐시 tree 업데이트만 → 레벨 0
```
