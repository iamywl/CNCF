# Argo CD Repository Server 심화 분석

> 소스 파일: `reposerver/repository/repository.go` (3276줄)
> 관련 파일: `util/helm/helm.go`, `util/helm/client.go`, `util/kustomize/kustomize.go`, `cmpserver/plugin/plugin.go`, `cmpserver/plugin/config.go`

---

## 목차

1. [Repository Server 역할과 위치](#1-repository-server-역할과-위치)
2. [Service 구조체 분석](#2-service-구조체-분석)
3. [RepoServerInitConstants — 동작 파라미터](#3-reposerverinitconstants--동작-파라미터)
4. [runRepoOperation() — 소스 타입 분기의 핵심](#4-runrepooperation--소스-타입-분기의-핵심)
5. [GenerateManifest() — 진입점과 Promise 패턴](#5-generatemanifest--진입점과-promise-패턴)
6. [GenerateManifests() — 소스별 매니페스트 생성](#6-generatemanifests--소스별-매니페스트-생성)
7. [Helm 통합 심화](#7-helm-통합-심화)
8. [Kustomize 통합 심화](#8-kustomize-통합-심화)
9. [CMP (Config Management Plugin) 통합](#9-cmp-config-management-plugin-통합)
10. [캐시 전략 — getManifestCacheEntry()](#10-캐시-전략--getmanifestcacheentry)
11. [소스 파라미터 병합 — mergeSourceParameters()](#11-소스-파라미터-병합--mergesourceparameters)
12. [왜 이런 설계인가](#12-왜-이런-설계인가)

---

## 1. Repository Server 역할과 위치

### 시스템에서의 위치

Argo CD는 여러 컴포넌트로 구성되며, Repository Server는 그 중 매니페스트 생성을 전담하는 독립 프로세스이다.

```
┌─────────────────────────────────────────────────────────┐
│                     Argo CD 시스템                        │
│                                                          │
│  ┌──────────────┐     gRPC      ┌──────────────────────┐ │
│  │  API Server  │ ────────────► │   Repository Server  │ │
│  │              │               │                      │ │
│  │  Application │               │  - Git clone/fetch   │ │
│  │  Controller  │ ────────────► │  - Helm template     │ │
│  └──────────────┘               │  - Kustomize build   │ │
│                                 │  - CMP 통신           │ │
│                                 │  - 매니페스트 캐시    │ │
│                                 └──────────────────────┘ │
│                                           │               │
│                                           │ Unix Socket   │
│                                           ▼               │
│                                 ┌──────────────────────┐  │
│                                 │   CMP Sidecar 1..N   │  │
│                                 │  (plugin.yaml 기반)   │  │
│                                 └──────────────────────┘  │
└─────────────────────────────────────────────────────────┘
```

### 핵심 책임

| 책임 | 상세 |
|------|------|
| 소스 다운로드 | Git clone/fetch, Helm chart 추출, OCI 이미지 추출 |
| 매니페스트 생성 | Helm template, Kustomize build, CMP generate, plain YAML |
| 캐시 관리 | Redis 기반 매니페스트 캐시, 에러 캐시 |
| 보안 격리 | 별도 프로세스로 Git/Helm 바이너리 실행 |
| GPG 검증 | 커밋 서명 검증 (선택적) |

---

## 2. Service 구조체 분석

### 전체 구조

`reposerver/repository/repository.go` L.82-99:

```go
// Service implements ManifestService interface
type Service struct {
    gitCredsStore             git.CredsStore
    rootDir                   string
    gitRepoPaths              utilio.TempPaths
    chartPaths                utilio.TempPaths
    ociPaths                  utilio.TempPaths
    gitRepoInitializer        func(rootPath string) goio.Closer
    repoLock                  *repositoryLock
    cache                     *cache.Cache
    parallelismLimitSemaphore *semaphore.Weighted
    metricsServer             *metrics.MetricsServer
    newOCIClient              func(repoURL string, creds oci.Creds, proxy string, noProxy string, mediaTypes []string, opts ...oci.ClientOpts) (oci.Client, error)
    newGitClient              func(rawRepoURL string, root string, creds git.Creds, insecure bool, enableLfs bool, proxy string, noProxy string, opts ...git.ClientOpts) (git.Client, error)
    newHelmClient             func(repoURL string, creds helm.Creds, enableOci bool, proxy string, noProxy string, opts ...helm.ClientOpts) helm.Client
    initConstants             RepoServerInitConstants
    now                       func() time.Time
}
```

### 필드별 상세 분석

#### 경로 관리 필드

```
rootDir
  └─ /tmp/_argocd-repo/           (기본값)
       ├─ {UUID}/                 ← gitRepoPaths (Git 저장소 로컬 클론)
       ├─ {UUID}/                 ← chartPaths (Helm chart 압축 해제)
       └─ {UUID}/                 ← ociPaths (OCI 이미지 레이어)
```

- **rootDir**: 모든 임시 파일의 루트. 보안상 0o300(쓰기+실행, 읽기 없음) 권한으로 관리됨. 외부에서 디렉토리 목록을 볼 수 없다.
- **gitRepoPaths**: `utilio.TempPaths` 인터페이스. 랜덤 UUID 기반 경로를 생성하여 Git URL과 매핑. 에러 메시지에서 실제 경로를 숨기는 용도로도 사용됨.
- **chartPaths**: Helm chart tar 파일이 저장되는 경로 풀.
- **ociPaths**: OCI 이미지가 추출되는 경로 풀.

#### 동시성 제어 필드

```
┌─────────────────────────────────────────────────────┐
│             동시성 제어 계층                           │
│                                                      │
│  parallelismLimitSemaphore                           │
│  ┌────────────────────────────────┐                  │
│  │  최대 N개 동시 매니페스트 생성  │ ← ParallelismLimit │
│  └────────────────────────────────┘                  │
│                                                      │
│  repoLock (*repositoryLock)                          │
│  ┌────────────────────────────────┐                  │
│  │  같은 Git 저장소 접근 직렬화    │ ← repoPath 키 기반 │
│  └────────────────────────────────┘                  │
│                                                      │
│  manifestGenerateLock (전역 KeyLock)                  │
│  ┌────────────────────────────────┐                  │
│  │  같은 앱 경로 Helm 빌드 직렬화  │ ← appPath 키 기반  │
│  └────────────────────────────────┘                  │
└─────────────────────────────────────────────────────┘
```

- **parallelismLimitSemaphore**: `semaphore.Weighted` 타입. `ParallelismLimit > 0`일 때만 생성됨. 세마포어를 통해 동시 매니페스트 생성 개수를 제한하여 OOM을 방지함.
- **repoLock**: `*repositoryLock` 타입. Git 저장소의 checkout과 read를 조율. `allowConcurrent` 플래그로 읽기 전용 작업에 대한 동시 접근을 허용.

#### 팩토리 함수 필드

세 개의 클라이언트 생성 함수는 모두 함수 타입으로 저장된다. 이를 통해 테스트 시 mock 클라이언트로 교체할 수 있다.

| 필드 | 기본 구현 | 역할 |
|------|----------|------|
| `newGitClient` | `git.NewClientExt` | Git 저장소 클론/체크아웃 |
| `newOCIClient` | `oci.NewClient` | OCI 레지스트리 접근 |
| `newHelmClient` | `helm.NewClientWithLock` | Helm 차트 레지스트리 접근 |

`newHelmClient` 팩토리 내부에서는 `HelmUserAgent`가 설정된 경우 `helm.WithUserAgent()` 옵션을 자동으로 추가한다:

```go
newHelmClient: func(repoURL string, creds helm.Creds, enableOci bool, proxy string, noProxy string, opts ...helm.ClientOpts) helm.Client {
    if initConstants.HelmUserAgent != "" {
        opts = append(opts, helm.WithUserAgent(initConstants.HelmUserAgent))
    }
    return helm.NewClientWithLock(repoURL, creds, sync.NewKeyLock(), enableOci, proxy, noProxy, opts...)
},
```

### NewService() 초기화 흐름

`reposerver/repository/repository.go` L.127-159:

```
NewService() 호출
    │
    ├── ParallelismLimit > 0 → semaphore.NewWeighted(limit)
    │
    ├── NewRepositoryLock() → repoLock 생성
    │
    ├── utilio.NewRandomizedTempPaths(rootDir) × 3
    │   ├── gitRandomizedPaths  → gitRepoPaths
    │   ├── helmRandomizedPaths → chartPaths
    │   └── ociRandomizedPaths  → ociPaths
    │
    └── &Service{...} 반환
```

### Service.Init() — 재시작 시 복구

```go
func (s *Service) Init() error {
    // rootDir 없으면 생성 (권한: 0o300)
    // rootDir 있으면 읽기 권한 임시 부여 (0o700)
    // 기존 클론 디렉토리 순회 → git remote URL 복구 → gitRepoPaths에 재등록
    // 완료 후 읽기 권한 제거 (0o300)
}
```

프로세스 재시작 시 이미 클론된 저장소를 재사용하여 불필요한 네트워크 요청을 줄인다. `gogit.PlainOpen()`으로 기존 저장소를 열고 remote URL을 읽어 경로 맵에 복구한다.

---

## 3. RepoServerInitConstants — 동작 파라미터

`reposerver/repository/repository.go` L.101-122:

```go
type RepoServerInitConstants struct {
    OCIMediaTypes                                []string
    ParallelismLimit                             int64
    PauseGenerationAfterFailedGenerationAttempts int
    PauseGenerationOnFailureForMinutes           int
    PauseGenerationOnFailureForRequests          int
    SubmoduleEnabled                             bool
    MaxCombinedDirectoryManifestsSize            resource.Quantity
    CMPTarExcludedGlobs                          []string
    AllowOutOfBoundsSymlinks                     bool
    StreamedManifestMaxExtractedSize             int64
    StreamedManifestMaxTarSize                   int64
    HelmManifestMaxExtractedSize                 int64
    HelmRegistryMaxIndexSize                     int64
    OCIManifestMaxExtractedSize                  int64
    DisableOCIManifestMaxExtractedSize           bool
    DisableHelmManifestMaxExtractedSize          bool
    IncludeHiddenDirectories                     bool
    CMPUseManifestGeneratePaths                  bool
    EnableBuiltinGitConfig                       bool
    HelmUserAgent                                string
}
```

### 파라미터별 상세 설명

#### 동시성 제어

| 파라미터 | 기본값 | 설명 |
|---------|--------|------|
| `ParallelismLimit` | 0 (무제한) | 동시 매니페스트 생성 최대 개수. 0이면 세마포어를 생성하지 않음. |

#### 에러 캐싱 (3단계 회로 차단기)

```
에러 발생 시 동작 흐름:

연속 실패 횟수 < PauseGenerationAfterFailedGenerationAttempts
    → 에러를 기록하되 재시도 허용 (캐시 미스로 처리)

연속 실패 횟수 >= PauseGenerationAfterFailedGenerationAttempts
    → "일시정지 상태" 진입
    → 캐시된 에러 응답 반환 (NumberOfCachedResponsesReturned 증가)

일시정지 상태 해제 조건 (둘 중 하나):
    1. 경과 시간 >= PauseGenerationOnFailureForMinutes
    2. 캐시 반환 횟수 >= PauseGenerationOnFailureForRequests
```

| 파라미터 | 역할 |
|---------|------|
| `PauseGenerationAfterFailedGenerationAttempts` | 이 횟수만큼 연속 실패하면 에러 캐싱 모드 진입 |
| `PauseGenerationOnFailureForMinutes` | 에러 캐싱 모드를 X분간 유지 |
| `PauseGenerationOnFailureForRequests` | X회 캐시 응답 반환 후 재시도 허용 |

#### 보안 및 크기 제한

| 파라미터 | 역할 |
|---------|------|
| `AllowOutOfBoundsSymlinks` | false(기본)면 저장소 루트를 벗어나는 심볼릭 링크 차단 |
| `MaxCombinedDirectoryManifestsSize` | Directory 타입 앱의 전체 YAML 합산 크기 제한 |
| `HelmManifestMaxExtractedSize` | Helm chart 압축 해제 시 최대 크기 |
| `CMPTarExcludedGlobs` | CMP 사이드카에 전송하는 tar에서 제외할 파일 패턴 |

#### 기능 플래그

| 파라미터 | 역할 |
|---------|------|
| `SubmoduleEnabled` | Git 서브모듈 체크아웃 여부 |
| `CMPUseManifestGeneratePaths` | `argocd.argoproj.io/manifest-generate-paths` 어노테이션 사용 |
| `EnableBuiltinGitConfig` | Argo CD 내장 git 설정 사용 |

---

## 4. runRepoOperation() — 소스 타입 분기의 핵심

`reposerver/repository/repository.go` L.322-525:

### 함수 시그니처

```go
func (s *Service) runRepoOperation(
    ctx context.Context,
    revision string,
    repo *v1alpha1.Repository,
    source *v1alpha1.ApplicationSource,
    verifyCommit bool,
    cacheFn func(cacheKey string, refSourceCommitSHAs cache.ResolvedRevisions, firstInvocation bool) (bool, error),
    operation func(repoRoot, commitSHA, cacheKey string, ctxSrc operationContextSrc) error,
    settings operationSettings,
    hasMultipleSources bool,
    refSources map[string]*v1alpha1.RefTarget,
) error
```

이 함수는 전략 패턴(Strategy Pattern)을 구현한다. 소스 타입에 무관하게 동일한 인터페이스로 캐시 조회 → 소스 다운로드 → 작업 실행의 흐름을 처리한다.

### 소스 타입 분기 알고리즘

```
runRepoOperation() 실행 흐름:

Step 1: 소스 타입 판별 및 클라이언트 생성
    ┌─ source.IsOCI()  → newOCIClientResolveRevision()  → ociClient, digest
    ├─ source.IsHelm() → newHelmClientResolveRevision() → helmClient, version
    └─ default(Git)   → newClientResolveRevision()     → gitClient, commitSHA

Step 2: 참조 소스 해결 (다중 소스 앱의 경우)
    resolveReferencedSources() → repoRefs map[normalizedURL]commitSHA

Step 3: 첫 번째 캐시 체크 (세마포어 획득 전)
    if !settings.noCache:
        cacheFn(revision, repoRefs, firstInvocation=true)
        → 캐시 히트이면 즉시 반환

Step 4: 메트릭 기록
    metricsServer.IncPendingRepoRequest()
    defer metricsServer.DecPendingRepoRequest()

Step 5: 세마포어 획득 (ParallelismLimit 설정 시)
    settings.sem.Acquire(ctx, 1)
    defer settings.sem.Release(1)

Step 6: 소스 타입별 처리
    ┌─ OCI:
    │   ociClient.Extract() → ociPath
    │   CheckOutOfBoundsSymlinks()
    │   operation(ociPath, digest, digest, ctxSrc)
    │
    ├─ Helm:
    │   helmClient.ExtractChart() → chartPath
    │   CheckOutOfBoundsSymlinks()
    │   operation(chartPath, version, version, ctxSrc)
    │
    └─ Git:
        repoLock.Lock(root, revision, allowConcurrent, checkoutFn)
        CheckOutOfBoundsSymlinks()
        gitClient.CommitSHA() → commitSHA
        ↓
        Step 7: Double-check locking
        if !settings.noCache:
            cacheFn(revision, repoRefs, firstInvocation=false)
            → 세마포어 대기 중 다른 고루틴이 캐시를 채웠을 수 있음
        ↓
        operation(gitClient.Root(), commitSHA, revision, ctxSrc)
```

### Double-Check Locking (L.494-497)

```go
// double-check locking
if !settings.noCache {
    if ok, err := cacheFn(revision, repoRefs, false); ok {
        return err
    }
}
```

이 패턴은 다음 시나리오를 최적화한다:

```
시나리오:
    고루틴 A: 캐시 미스 → 세마포어 대기 중...
    고루틴 B: 세마포어 획득 → 매니페스트 생성 → 캐시 저장 → 세마포어 해제
    고루틴 A: 세마포어 획득 → [여기서 다시 캐시 체크]
                               → 이미 캐시에 있으므로 중복 생성 생략
```

세마포어 대기 시간만큼 캐시가 채워질 기회가 있으므로, 이중 확인으로 불필요한 매니페스트 생성을 방지한다.

### Git 체크아웃 흐름 상세

```go
closer, err := s.repoLock.Lock(gitClient.Root(), revision, settings.allowConcurrent, func() (goio.Closer, error) {
    return s.checkoutRevision(gitClient, revision, s.initConstants.SubmoduleEnabled, repo.Depth)
})
```

```
repoLock.Lock() 내부:
    ┌─ 이미 해당 revision 체크아웃됨 (allowConcurrent=true) → 공유 접근 허용
    └─ 체크아웃 필요 → 잠금 획득 → checkoutRevision() 실행
                                         │
                                         ├── gitClient.Init()
                                         ├── gitClient.Fetch("", depth) [기본 fetch]
                                         ├── gitClient.Checkout(revision)
                                         └── 실패 시 Fetch(revision) + Checkout("FETCH_HEAD")
```

### operationContext와 GPG 검증

```go
return operation(gitClient.Root(), commitSHA, revision, func() (*operationContext, error) {
    var signature string
    if verifyCommit {
        var rev string
        if gitClient.IsAnnotatedTag(revision) {
            rev = unresolvedRevision  // 어노테이션 태그는 원래 이름 사용
        } else {
            rev = revision            // 일반 커밋은 SHA 사용
        }
        signature, err = gitClient.VerifyCommitSignature(rev)
    }
    appPath, err := apppathutil.Path(gitClient.Root(), source.Path)
    return &operationContext{appPath, signature}, nil
})
```

`operationContextSrc`는 지연 평가(lazy evaluation)로 설계되어 있다. GPG 검증과 경로 해석은 실제로 필요한 시점에만 수행된다. 캐시 히트 시에는 이 함수가 호출되지 않아 불필요한 처리를 피할 수 있다.

---

## 5. GenerateManifest() — 진입점과 Promise 패턴

`reposerver/repository/repository.go` L.591-659:

### ref-only 소스 스킵

```go
// Skip this path for ref only sources
if q.HasMultipleSources && q.ApplicationSource.Path == "" && !q.ApplicationSource.IsOCI() && !q.ApplicationSource.IsHelm() && q.ApplicationSource.IsRef() {
    log.Debugf("Skipping manifest generation for ref only source...")
    _, revision, err := s.newClientResolveRevision(q.Repo, q.Revision, ...)
    res = &apiclient.ManifestResponse{Revision: revision}
    return res, err
}
```

다중 소스 앱에서 `ref` 필드만 설정된 소스(값 파일 참조용)는 매니페스트 생성을 건너뛴다. 이 소스는 다른 소스의 Helm 값 파일을 가져오는 용도로만 사용되기 때문이다.

### ManifestResponsePromise 패턴

```go
type ManifestResponsePromise struct {
    responseCh <-chan *apiclient.ManifestResponse
    tarDoneCh  <-chan bool
    errCh      <-chan error
}
```

이 구조는 CMP 사이드카 처리의 비동기성을 처리하기 위해 설계되었다.

```
CMP 처리 흐름:

1. runManifestGen() → 고루틴 시작, ManifestResponsePromise 반환
2. 저장소 잠금 보유 상태에서:
   select {
   case err := <-promise.errCh:    → 에러 즉시 반환
   case resp := <-promise.responseCh: → Helm/Kustomize 응답 (잠금 해제)
   case tarDone := <-promise.tarDoneCh: → CMP tar 전송 완료 신호 (잠금 해제 가능)
   }

3. tarDone 수신 시: 저장소 잠금 해제 (Git 저장소를 더 이상 읽지 않아도 됨)
4. 잠금 해제 후 CMP 서버가 매니페스트 생성 완료:
   select {
   case resp := <-promise.responseCh: → 최종 응답
   case err := <-promise.errCh:       → 에러
   }
```

### 왜 tarDoneCh 채널이 필요한가

CMP 처리는 두 단계로 나뉜다:
1. **tar 전송 단계**: Git 저장소 파일을 tar로 묶어 CMP 사이드카에 스트리밍. 이 단계에서는 Git 저장소에 대한 잠금이 필요하다.
2. **매니페스트 생성 단계**: CMP 사이드카가 받은 파일로 매니페스트를 생성. 이 단계에서는 Git 저장소가 필요 없다.

`tarDoneCh`는 1단계가 완료되면 신호를 보내, 저장소 잠금을 최대한 빨리 해제하게 한다. 이를 통해 같은 저장소를 사용하는 다른 앱의 대기 시간을 줄인다.

---

## 6. GenerateManifests() — 소스별 매니페스트 생성

`reposerver/repository/repository.go` L.1502-1614:

### 함수 시그니처

```go
func GenerateManifests(
    ctx context.Context,
    appPath, repoRoot, revision string,
    q *apiclient.ManifestRequest,
    isLocal bool,
    gitCredsStore git.CredsStore,
    maxCombinedManifestQuantity resource.Quantity,
    gitRepoPaths utilio.TempPaths,
    opts ...GenerateManifestOpt,
) (*apiclient.ManifestResponse, error)
```

### 소스 타입 스위치

```go
appSourceType, err := GetAppSourceType(ctx, q.ApplicationSource, appPath, repoRoot, ...)

switch appSourceType {
case v1alpha1.ApplicationSourceTypeHelm:
    targetObjs, command, err = helmTemplate(appPath, repoRoot, env, q, isLocal, gitRepoPaths)

case v1alpha1.ApplicationSourceTypeKustomize:
    k := kustomize.NewKustomizeApp(repoRoot, appPath, ...)
    targetObjs, _, commands, err = k.Build(q.ApplicationSource.Kustomize, q.KustomizeOptions, env, &kustomize.BuildOpts{...})

case v1alpha1.ApplicationSourceTypePlugin:
    targetObjs, err = runConfigManagementPluginSidecars(ctx, appPath, repoRoot, pluginName, env, q, ...)

case v1alpha1.ApplicationSourceTypeDirectory:
    targetObjs, err = findManifests(logCtx, appPath, repoRoot, env, *directory, ...)
}
```

### GetAppSourceType() — 소스 타입 자동 감지

```
GetAppSourceType() 실행 흐름:

1. mergeSourceParameters() → .argocd-source.yaml 오버라이드 적용
2. source.ExplicitType() → 명시적 타입이 있으면 사용
   (spec.source.helm, kustomize, plugin, directory 필드 존재 여부로 판단)
3. discovery.AppType() → 디렉토리 내용으로 자동 감지
   - Chart.yaml 존재 → Helm
   - kustomization.yaml/yml/Kustomization 존재 → Kustomize
   - CMP 플러그인 감지 → Plugin
   - 그 외 → Directory
```

### 리소스 추적 레이블 주입

```go
for _, target := range targets {
    if q.AppLabelKey != "" && q.AppName != "" && !kube.IsCRD(target) {
        err = resourceTracking.SetAppInstance(target, q.AppLabelKey, q.AppName, q.Namespace,
            v1alpha1.TrackingMethod(q.TrackingMethod), q.InstallationID)
    }
    manifestStr, err := json.Marshal(target.Object)
    manifests = append(manifests, string(manifestStr))
}
```

CRD를 제외한 모든 리소스에 앱 인스턴스 추적 레이블/어노테이션이 주입된다. `TrackingMethod`에 따라 레이블 방식 또는 어노테이션 방식이 선택된다.

### ARGOCD_APP_* 환경변수

`reposerver/repository/repository.go` L.1616-1630:

```go
func newEnv(q *apiclient.ManifestRequest, revision string) *v1alpha1.Env {
    shortRevision  := shortenRevision(revision, 7)
    shortRevision8 := shortenRevision(revision, 8)
    return &v1alpha1.Env{
        &v1alpha1.EnvEntry{Name: "ARGOCD_APP_NAME",                  Value: q.AppName},
        &v1alpha1.EnvEntry{Name: "ARGOCD_APP_NAMESPACE",             Value: q.Namespace},
        &v1alpha1.EnvEntry{Name: "ARGOCD_APP_PROJECT_NAME",          Value: q.ProjectName},
        &v1alpha1.EnvEntry{Name: "ARGOCD_APP_REVISION",              Value: revision},
        &v1alpha1.EnvEntry{Name: "ARGOCD_APP_REVISION_SHORT",        Value: shortRevision},
        &v1alpha1.EnvEntry{Name: "ARGOCD_APP_REVISION_SHORT_8",      Value: shortRevision8},
        &v1alpha1.EnvEntry{Name: "ARGOCD_APP_SOURCE_REPO_URL",       Value: q.Repo.Repo},
        &v1alpha1.EnvEntry{Name: "ARGOCD_APP_SOURCE_PATH",           Value: q.ApplicationSource.Path},
        &v1alpha1.EnvEntry{Name: "ARGOCD_APP_SOURCE_TARGET_REVISION",Value: q.ApplicationSource.TargetRevision},
    }
}
```

총 9개의 환경변수가 주입된다. Helm values 파일, Kustomize 이미지 태그, CMP 플러그인 스크립트에서 `${ARGOCD_APP_REVISION}` 형태로 참조 가능하다.

| 환경변수 | 예시 값 |
|---------|---------|
| `ARGOCD_APP_NAME` | `my-app` |
| `ARGOCD_APP_NAMESPACE` | `production` |
| `ARGOCD_APP_PROJECT_NAME` | `default` |
| `ARGOCD_APP_REVISION` | `a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2` |
| `ARGOCD_APP_REVISION_SHORT` | `a1b2c3d` (7자) |
| `ARGOCD_APP_REVISION_SHORT_8` | `a1b2c3d4` (8자) |
| `ARGOCD_APP_SOURCE_REPO_URL` | `https://github.com/org/repo` |
| `ARGOCD_APP_SOURCE_PATH` | `apps/my-app` |
| `ARGOCD_APP_SOURCE_TARGET_REVISION` | `main` |

---

## 7. Helm 통합 심화

### Helm 인터페이스

`util/helm/helm.go` L.34-43:

```go
// Helm provides wrapper functionality around the `helm` command.
type Helm interface {
    // Template returns a list of unstructured objects from a `helm template` command
    Template(opts *TemplateOpts) (string, string, error)
    // GetParameters returns a list of chart parameters taking into account values in provided YAML files.
    GetParameters(valuesFiles []pathutil.ResolvedFilePath, appPath, repoRoot string) (map[string]string, error)
    // DependencyBuild runs `helm dependency build` to download a chart's dependencies
    DependencyBuild() error
    // Dispose deletes temp resources
    Dispose()
}
```

### helmTemplate() 처리 흐름

`reposerver/repository/repository.go` L.1192-1348:

```
helmTemplate() 실행 흐름:

1. 앱 이름 파싱: argo.ParseInstanceName() → Helm release 이름
   (Helm release 이름은 53자 이하, 언더스코어 불가)

2. Kubernetes 버전 파싱:
   parseKubeVersion() → kubeVersion 문자열 정규화

3. TemplateOpts 구성:
   - Name: appName 또는 ReleaseName
   - Namespace: 앱 네임스페이스
   - KubeVersion, APIVersions
   - Values: 해결된 values 파일 경로 목록
   - ExtraValues: spec.source.helm.values 인라인 값 (임시 파일 생성)
   - Set, SetString, SetFile: 개별 파라미터

4. helm.NewHelmApp() 초기화
   → helm.Cmd 생성 (바이너리 경로 탐색)
   → repo 인증 정보 설정

5. h.Template(templateOpts) 실행
   → 성공 시: YAML 출력 파싱 → unstructured 객체 목록

6. 실패 + IsMissingDependencyErr():
   → runHelmBuild(appPath, h) 실행
   → h.Template() 재시도
```

### runHelmBuild() — 중복 빌드 방지

`reposerver/repository/repository.go` L.1151-1171:

```go
var manifestGenerateLock = sync.NewKeyLock()

func runHelmBuild(appPath string, h helm.Helm) error {
    manifestGenerateLock.Lock(appPath)
    defer manifestGenerateLock.Unlock(appPath)

    // .argocd-helm-dep-up 마커 파일 확인
    markerFile := path.Join(appPath, helmDepUpMarkerFile)
    _, err := os.Stat(markerFile)
    if err == nil {
        return nil  // 이미 실행됨
    } else if !os.IsNotExist(err) {
        return err
    }

    err = h.DependencyBuild()
    if err != nil {
        return fmt.Errorf("error building helm chart dependencies: %w", err)
    }
    return os.WriteFile(markerFile, []byte("marker"), 0o644)
}
```

`manifestGenerateLock`은 `sync.NewKeyLock()` 타입의 전역 잠금이다. appPath를 키로 하여 같은 앱 경로에 대한 `helm dependency build`가 동시에 실행되지 않도록 보장한다.

`.argocd-helm-dep-up` 마커 파일은 같은 커밋에서 이미 `dependency build`를 실행했음을 표시한다. 새 커밋이 체크아웃될 때 저장소가 다시 초기화되므로 마커 파일도 삭제된다.

```
.argocd-helm-dep-up 마커 파일 생명주기:

    새 커밋 체크아웃
        │
        └── 저장소 재초기화 (directoryPermissionInitializer)
                │
                └── 마커 파일 삭제됨
                        │
                        └── 다음 매니페스트 생성 시 dependency build 실행
                                │
                                └── 마커 파일 생성 → 이후 요청은 스킵
```

### Helm Client — 차트 추출

`util/helm/client.go`:

```go
type Client interface {
    CleanChartCache(chart string, version string) error
    ExtractChart(chart string, version string, passCredentials bool, manifestMaxExtractedSize int64, disableManifestMaxExtractedSize bool) (string, utilio.Closer, error)
    GetIndex(noCache bool, maxIndexSize int64) (*Index, error)
    GetTags(chart string, noCache bool) ([]string, error)
    TestHelmOCI() (bool, error)
}
```

`ExtractChart()` 내부 흐름:

```
ExtractChart() 실행:

1. getCachedChartPath(chart, version) → 캐시 경로 계산
2. repoLock.Lock(cachedChartPath) → 동일 차트 버전 동시 다운로드 방지
3. 캐시 경로에 파일 있으면 → 압축 해제만 수행
4. 캐시 없으면:
   if enableOci:
       helmCmd.RegistryLogin()
       helmCmd.PullOCI()    ← helm pull oci://...
   else:
       helmCmd.Fetch()      ← helm fetch <repo> <chart> --version <ver>
5. 다운로드된 .tgz를 캐시 경로로 이동
6. files.Untgz() 또는 tar 명령으로 압축 해제 (크기 제한 적용)
7. 임시 디렉토리 경로 반환 + Closer
```

### Helm 버전 해결

```go
func (s *Service) newHelmClientResolveRevision(repo *v1alpha1.Repository, revision string, chart string, noRevisionCache bool) (helm.Client, string, error) {
    // revision이 정확한 버전이면 그대로 사용
    if versions.IsVersion(revision) {
        return helmClient, revision, nil
    }

    // OCI: helmClient.GetTags()
    // non-OCI: helmClient.GetIndex() → entries.Tags()
    // → versions.MaxVersion(revision, tags) → semver 범위 해석
}
```

`revision`이 `>=1.2.0, <2.0.0` 같은 semver 범위 표현일 경우, 레지스트리에서 태그 목록을 가져와 최대 버전을 선택한다.

---

## 8. Kustomize 통합 심화

### Kustomize 인터페이스

`util/kustomize/kustomize.go` L.42-45:

```go
// Kustomize provides wrapper functionality around the `kustomize` command.
type Kustomize interface {
    // Build returns a list of unstructured objects from a `kustomize build` command and extract supported parameters
    Build(opts *v1alpha1.ApplicationSourceKustomize, kustomizeOptions *v1alpha1.KustomizeOptions, envVars *v1alpha1.Env, buildOpts *BuildOpts) ([]*unstructured.Unstructured, []Image, []string, error)
}
```

### NewKustomizeApp() 초기화

`util/kustomize/kustomize.go` L.48-58:

```go
func NewKustomizeApp(repoRoot string, path string, creds git.Creds, fromRepo string, binaryPath string, proxy string, noProxy string) Kustomize {
    return &kustomize{
        repoRoot:   repoRoot,
        path:       path,
        creds:      creds,
        repo:       fromRepo,
        binaryPath: binaryPath,
        proxy:      proxy,
        noProxy:    noProxy,
    }
}
```

### Build() 실행 흐름

`util/kustomize/kustomize.go` L.124-:

```
Build() 실행 순서:

1. 환경변수 설정:
   - os.Environ() + ARGOCD_APP_* 변수
   - git 인증 정보 (creds.Environ())
   - HTTPS 저장소이면 CA 번들 경로 (GIT_SSL_CAINFO)

2. 옵션 적용 (kustomize edit 명령):
   - NamePrefix    → kustomize edit set nameprefix
   - NameSuffix    → kustomize edit set namesuffix
   - Images        → kustomize edit set image (envsubst 적용)
   - Replicas      → kustomize edit set replicas
   - CommonLabels  → kustomize edit add label [--force]
   - CommonAnnotations → kustomize edit add annotation [--force]
   - Patches       → kustomize edit add patch

3. kustomize build 실행:
   cmd := exec.CommandContext(ctx, k.getBinaryPath(), "build", ".", "--load-restrictor=LoadRestrictionsNone", ...)
   → YAML 출력

4. kube.SplitYAML() → unstructured 객체 목록 반환
```

### KustomizeOptions와 바이너리 경로

```go
func (k *kustomize) getBinaryPath() string {
    if k.binaryPath != "" {
        return k.binaryPath
    }
    return "kustomize"
}
```

`KustomizeOptions`를 통해 특정 버전의 kustomize 바이너리 경로를 지정할 수 있다. 이를 통해 서로 다른 앱이 서로 다른 kustomize 버전을 사용할 수 있다.

### kustomization 파일 감지

```go
var KustomizationNames = []string{"kustomization.yaml", "kustomization.yml", "Kustomization"}

func IsKustomization(path string) bool {
    return slices.Contains(KustomizationNames, path)
}
```

디렉토리에 위 이름 중 하나의 파일이 존재하면 Kustomize 앱으로 자동 감지된다.

---

## 9. CMP (Config Management Plugin) 통합

### CMP 아키텍처

```
┌──────────────────────────────────────────────────────┐
│               Kubernetes Pod                          │
│                                                      │
│  ┌─────────────────────┐    Unix     ┌─────────────┐ │
│  │   repo-server       │   Socket    │  cmp-server │ │
│  │   컨테이너           │ ─────────── │  사이드카    │ │
│  │                     │  gRPC 스트림 │             │ │
│  │ runConfigManagement │             │ plugin.yaml │ │
│  │ PluginSidecars()    │             │ 기반 실행    │ │
│  └─────────────────────┘             └─────────────┘ │
│                                                      │
│  /home/argocd/cmp-server/            ← 소켓 디렉토리  │
│    └── {plugin-name}.sock                            │
└──────────────────────────────────────────────────────┘
```

### CMP 서버 Service 구조체

`cmpserver/plugin/plugin.go` L.38-44:

```go
// Service implements ConfigManagementPluginService interface
type Service struct {
    initConstants CMPServerInitConstants
}

type CMPServerInitConstants struct {
    PluginConfig PluginConfig
}
```

### PluginConfig 구조체

`cmpserver/plugin/config.go` L.19-33:

```go
type PluginConfig struct {
    metav1.TypeMeta `json:",inline"`
    Metadata        metav1.ObjectMeta `json:"metadata"`
    Spec            PluginConfigSpec  `json:"spec"`
}

type PluginConfigSpec struct {
    Version          string     `json:"version"`
    Init             Command    `json:"init,omitempty"`
    Generate         Command    `json:"generate"`
    Discover         Discover   `json:"discover"`
    Parameters       Parameters `yaml:"parameters"`
    PreserveFileMode bool       `json:"preserveFileMode,omitempty"`
    ProvideGitCreds  bool       `json:"provideGitCreds,omitempty"`
}
```

### plugin.yaml 예시

```yaml
apiVersion: argoproj.io/v1alpha1
kind: ConfigManagementPlugin
metadata:
  name: my-plugin
spec:
  version: v1.0
  init:
    command: [sh, -c]
    args: ["echo 'Initializing...'"]
  generate:
    command: [sh, -c]
    args: ["kustomize build ."]
  discover:
    fileName: "*.yaml"
  preserveFileMode: false
  provideGitCreds: true
```

### CMP gRPC 서비스 인터페이스

CMP 서버가 구현하는 세 가지 gRPC 메서드:

| 메서드 | 역할 |
|--------|------|
| `GenerateManifest` | 앱 파일을 받아 매니페스트 생성 |
| `MatchRepository` | 이 플러그인이 해당 앱을 처리할 수 있는지 판단 |
| `GetParametersAnnouncement` | 플러그인이 지원하는 파라미터 목록 반환 |

### MatchRepository 감지 알고리즘

`cmpserver/plugin/plugin.go` L.324-373:

```
matchRepository() 실행 순서:

1. spec.discover.fileName 설정됨?
   → filepath.Glob(appPath + "/" + fileName)
   → 매칭 파일 있으면 isSupported=true

2. spec.discover.find.glob 설정됨?
   → zglob.Glob(appPath + "/" + glob)  [** 지원]
   → 매칭 파일 있으면 isSupported=true

3. spec.discover.find.command 설정됨?
   → runCommand(ctx, command, appPath, env)
   → 출력이 비어있지 않으면 isSupported=true

4. 아무것도 설정되지 않음 → isDiscoveryEnabled=false
   (앱이 plugin.name을 명시적으로 지정해야 함)
```

### GenerateManifest 스트리밍 처리

`cmpserver/plugin/plugin.go` L.206-240:

```go
func (s *Service) generateManifestGeneric(stream GenerateManifestStream) error {
    ctx, cancel := buffered_context.WithEarlierDeadline(stream.Context(), cmpTimeoutBuffer)
    defer cancel()

    // 임시 작업 디렉토리 생성
    workDir, cleanup, err := getTempDirMustCleanup(common.GetCMPWorkDir())
    defer cleanup()

    // repo-server로부터 tar 스트림 수신 및 압축 해제
    metadata, err := cmp.ReceiveRepoStream(ctx, stream, workDir, s.initConstants.PluginConfig.Spec.PreserveFileMode)

    // appPath 계산 (보안: workDir 범위를 벗어나지 않도록 검증)
    appPath := filepath.Clean(filepath.Join(workDir, metadata.AppRelPath))
    if !strings.HasPrefix(appPath, workDir) {
        return errors.New("illegal appPath: out of workDir bound")
    }

    // Init 커맨드 실행 (선택적)
    if len(config.Spec.Init.Command) > 0 {
        _, err := runCommand(ctx, config.Spec.Init, appDir, env)
    }

    // Generate 커맨드 실행
    out, err := runCommand(ctx, config.Spec.Generate, appDir, env)

    // YAML 파싱 후 ManifestResponse 반환
    manifests, err := kube.SplitYAMLToString([]byte(out))
    return stream.SendAndClose(&apiclient.ManifestResponse{Manifests: manifests})
}
```

### repo-server → CMP 파일 전송

`reposerver/repository/repository.go` L.2164-2180:

```go
func generateManifestsCMP(ctx context.Context, appPath, rootPath string, env []string, cmpClient ...) (*pluginclient.ManifestResponse, error) {
    generateManifestStream, err := cmpClient.GenerateManifest(ctx, grpc_retry.Disable())

    opts := []cmp.SenderOption{
        cmp.WithTarDoneChan(tarDoneCh),
    }

    // appPath의 파일을 tar로 묶어 gRPC 스트림으로 전송
    err = cmp.SendRepoStream(ctx, appPath, rootPath, generateManifestStream, env, tarExcludedGlobs, opts...)

    return generateManifestStream.CloseAndRecv()
}
```

`CMPUseManifestGeneratePaths`가 활성화되면, `argocd.argoproj.io/manifest-generate-paths` 어노테이션에 명시된 경로만 포함한 최소한의 tar를 전송한다. 대형 저장소에서 전송 시간과 대역폭을 절약할 수 있다.

---

## 10. 캐시 전략 — getManifestCacheEntry()

`reposerver/repository/repository.go` L.953-1042:

### CachedManifestResponse 구조

```go
type CachedManifestResponse struct {
    // 성공적으로 생성된 매니페스트 응답
    ManifestResponse *apiclient.ManifestResponse

    // 에러 추적 필드
    NumberOfCachedResponsesReturned int
    NumberOfConsecutiveFailures     int
    FirstFailureTimestamp           int64
    MostRecentError                 string
}
```

### 캐시 판단 알고리즘

```
getManifestCacheEntry() 결과 판단:

cache.GetManifests() 호출
    │
    ├── 캐시 미스 (ErrCacheMiss) ────────────────────────── → (false, nil, nil)
    │                                                         재생성 필요
    │
    └── 캐시 히트
            │
            ├── res.FirstFailureTimestamp == 0 (정상 응답)
            │       └────────────────────────────────────── → (true, res.ManifestResponse, nil)
            │                                                  캐시 사용
            │
            └── res.FirstFailureTimestamp > 0 (에러 캐시)
                    │
                    ├── res.NumberOfConsecutiveFailures < PauseGenerationAfterFailedGenerationAttempts
                    │       └──────────────────────────────── → (false, res.ManifestResponse, nil)
                    │                                           아직 임계값 미도달, 재시도
                    │
                    └── res.NumberOfConsecutiveFailures >= PauseGenerationAfterFailedGenerationAttempts
                            │                               (일시정지 상태)
                            ├── 경과 시간 >= PauseGenerationOnFailureForMinutes
                            │       → DeleteManifests() 후
                            │       └───────────────────────── → (false, nil, nil)
                            │                                     캐시 리셋, 재시도
                            │
                            ├── CachedResponses >= PauseGenerationOnFailureForRequests
                            │       → DeleteManifests() 후
                            │       └───────────────────────── → (false, nil, nil)
                            │                                     캐시 리셋, 재시도
                            │
                            └── 일시정지 유지
                                    → NumberOfCachedResponsesReturned++ (첫 호출 시)
                                    └───────────────────────── → (true, nil, cachedError)
                                                                  에러 캐시 반환
```

### 에러 캐시 갱신 흐름

```
GenerateManifests() 에러 발생 시 (runManifestGenAsync):

1. cache.GetManifests() → 현재 캐시 상태 가져오기

2. innerRes.FirstFailureTimestamp == 0 이면:
   innerRes.FirstFailureTimestamp = s.now().Unix()  // 첫 실패 시각 기록

3. innerRes.NumberOfConsecutiveFailures++
   innerRes.MostRecentError = err.Error()

4. cache.SetManifests() → 업데이트된 에러 정보 저장

5. ch.errCh <- err  // 호출자에게 에러 전달
```

성공 시에는 에러 카운터를 모두 초기화한다:

```go
manifestGenCacheEntry := cache.CachedManifestResponse{
    ManifestResponse:                manifestGenResult,
    NumberOfCachedResponsesReturned: 0,
    NumberOfConsecutiveFailures:     0,
    FirstFailureTimestamp:           0,
    MostRecentError:                 "",
}
```

### 캐시 키 구성

```
캐시 키 구성 요소:

repo URL + 소스 경로 + revision(commitSHA)
+ ApplicationSource 설정 (values, parameters 등)
+ refSources (참조 소스들의 commitSHA)
+ Namespace + TrackingMethod + AppLabelKey + AppName
+ InstallationID
```

`refSources`가 캐시 키에 포함되므로, 참조 소스 저장소의 커밋이 변경되면 자동으로 캐시가 무효화된다.

---

## 11. 소스 파라미터 병합 — mergeSourceParameters()

`reposerver/repository/repository.go` L.1655-1706:

### 기능 개요

저장소의 `.argocd-source.yaml` 파일로 앱 소스 파라미터를 오버라이드할 수 있다. 이를 통해 Argo CD Application 매니페스트를 수정하지 않고도 저장소 수준에서 설정을 변경할 수 있다.

### 파일 목록

```
{appPath}/
├── .argocd-source.yaml          ← 전역 오버라이드 (모든 앱에 적용)
└── .argocd-source-{appName}.yaml ← 앱별 오버라이드 (특정 앱에만 적용)
```

### 병합 알고리즘

```go
func mergeSourceParameters(source *v1alpha1.ApplicationSource, path, appName string) error {
    repoFilePath := filepath.Join(path, repoSourceFile)     // .argocd-source.yaml
    overrides := []string{repoFilePath}
    if appName != "" {
        overrides = append(overrides, filepath.Join(path, fmt.Sprintf(appSourceFile, appName)))
        // .argocd-source-{appName}.yaml
    }

    merged := *source.DeepCopy()

    for _, filename := range overrides {
        // 파일 없으면 스킵
        // JSON으로 현재 상태 직렬화
        data, err := json.Marshal(merged)
        // 오버라이드 파일 읽기 (YAML → JSON 변환)
        patch, err := yaml.YAMLToJSON(patch)
        // JSON Merge Patch 적용 (RFC 7396)
        data, err = jsonpatch.MergePatch(data, patch)
        // 역직렬화
        err = json.Unmarshal(data, &merged)
    }

    // 보호 필드: 오버라이드 파일이 변경할 수 없는 필드
    merged.Chart          = source.Chart
    merged.Path           = source.Path
    merged.RepoURL        = source.RepoURL
    merged.TargetRevision = source.TargetRevision

    *source = merged
    return nil
}
```

### 보호 필드 이유

```
보호 필드: Chart, Path, RepoURL, TargetRevision
이유: 이 필드들은 "어디서 무엇을 가져올지"를 결정함
     저장소 내 파일이 이를 변경할 수 있으면 임의의 저장소를 참조하거나
     다른 경로의 파일을 실행할 수 있는 보안 위험이 생김
```

오버라이드 가능한 필드는 `Helm`, `Kustomize`, `Directory`, `Plugin` 등의 "어떻게 처리할지"에 해당하는 설정이다.

### 사용 예시

```yaml
# .argocd-source.yaml
helm:
  parameters:
    - name: image.tag
      value: latest
  valueFiles:
    - values-override.yaml
```

```yaml
# .argocd-source-my-app.yaml
helm:
  parameters:
    - name: replicaCount
      value: "3"
```

두 파일이 모두 존재하면 전역 파일이 먼저 적용되고, 앱별 파일이 추가로 적용된다.

---

## 12. 왜 이런 설계인가

### Q1. 왜 Repository Server를 별도 프로세스로 분리하는가?

```
모놀리식 방식의 문제점:
    API Server 프로세스 내에서 직접 git clone, helm template 실행
    → Git/Helm 바이너리 실행이 API Server 권한으로 실행됨
    → 악의적인 저장소의 hook 스크립트가 API Server 권한을 가짐
    → 메모리 폭발 또는 크래시가 API Server를 다운시킴

별도 프로세스의 장점:
    1. 보안 격리: API Server와 다른 Linux 네임스페이스/seccomp 프로파일 적용 가능
    2. 독립 재시작: repo-server 크래시가 API Server에 영향 없음
    3. 수평 확장: repo-server만 독립적으로 스케일 아웃 가능
    4. 리소스 격리: Helm/Kustomize의 높은 메모리 사용을 API Server로부터 분리
```

### Q2. 왜 세마포어로 동시 생성을 제한하는가?

```
Helm template과 Kustomize build의 특성:
    - 외부 바이너리 실행
    - 많은 YAML 파일을 메모리에 로드
    - 대형 차트의 경우 수백MB 메모리 사용 가능

제한 없이 병렬 실행 시:
    N개의 앱이 동시에 매니페스트 생성 요청
    → N × (메모리 사용량) → OOM Killer 발동
    → repo-server 재시작 → 서비스 중단

ParallelismLimit = 10으로 설정 시:
    → 최대 10개만 동시 실행
    → 나머지는 세마포어에서 대기
    → 메모리 사용량 예측 가능
```

### Q3. 왜 Double-Check Locking 패턴을 사용하는가?

```
문제 상황:
    100개의 앱이 동시에 같은 커밋의 매니페스트 요청
    ParallelismLimit = 5

    1번 고루틴: 캐시 미스 → 세마포어 대기
    ...
    100번 고루틴: 캐시 미스 → 세마포어 대기

    5개 처리 완료 → 캐시 저장
    → 6번째 고루틴: 세마포어 획득

Double-Check 없이:
    → 6번째 고루틴이 다시 생성 → 불필요한 중복 작업

Double-Check 있을 때:
    → 6번째 고루틴: 세마포어 획득 후 즉시 캐시 확인
    → 캐시 히트 → 즉시 반환
    → 세마포어 해제 → 다음 고루틴 진행
```

### Q4. 왜 에러를 캐싱하는가?

```
에러 캐싱 없는 시나리오:
    Git 서버 장애 발생
    → 모든 앱 동기화 요청이 Git에 재시도
    → repo-server → Git 서버에 수천 개의 동시 요청
    → Git 서버 복구 더 어려워짐 (retry storm)
    → repo-server OOM 위험

에러 캐싱 있을 때:
    Git 서버 장애 → 첫 N번 실패 → 에러 캐시 진입
    → 이후 요청: 캐시된 에러 즉시 반환
    → Git 서버에 요청 없음
    → X분 후 또는 Y번 후: 자동으로 재시도
    → 점진적 복구 가능
```

### Q5. 왜 .argocd-helm-dep-up 마커 파일을 사용하는가?

```
helm dependency build의 특성:
    - 인터넷에서 차트 의존성 다운로드
    - 실행 시간: 1~5초 (네트워크 상태에 따라)
    - 같은 커밋에 대해 여러 번 실행할 이유 없음

문제 상황:
    같은 앱의 매니페스트 생성이 여러 번 요청됨
    → manifestGenerateLock 없으면: N번의 dependency build 실행
    → manifestGenerateLock만 있으면: 직렬로 N번 실행

마커 파일 솔루션:
    첫 번째 고루틴: 잠금 획득 → build 실행 → 마커 파일 생성
    두 번째 고루틴: 잠금 획득 → 마커 파일 존재 확인 → 즉시 반환
    → dependency build가 커밋당 1회만 실행됨
```

### Q6. 왜 CMP에 Unix Domain Socket을 사용하는가?

```
설계 의도:
    CMP 플러그인은 사용자가 직접 구현하는 코드임
    보안상 repo-server와 동일한 네트워크 접근을 허용하면 안 됨

Unix Domain Socket의 특성:
    - 파일시스템 기반: Pod 내의 컨테이너끼리만 공유 가능
    - 네트워크 없음: 외부에서 접근 불가
    - 빠름: 루프백(localhost)보다 커널 내부 통신이 더 빠름

결과:
    CMP 플러그인: 파드 내 사이드카 컨테이너에만 격리
    외부 네트워크 접근 불가
    플러그인 취약점이 있어도 클러스터 네트워크 접근 제한
```

### 설계 결정 요약 테이블

| 설계 결정 | 이유 | 트레이드오프 |
|-----------|------|-------------|
| 별도 프로세스 | 보안 격리, 독립 재시작 | 네트워크 오버헤드 (gRPC) |
| 세마포어 동시성 제한 | OOM 방지 | 처리량 제한 |
| Double-Check Locking | 세마포어 대기 중 중복 생성 방지 | 코드 복잡도 증가 |
| 에러 캐싱 | Git 장애 시 과부하 방지 | 즉각적 장애 감지 불가 |
| .argocd-helm-dep-up 마커 | dependency build 중복 실행 방지 | 파일시스템 의존성 |
| Unix Domain Socket (CMP) | 플러그인 보안 격리 | Pod 내 통신으로 제한 |
| 랜덤 임시 경로 | 에러 메시지에서 경로 노출 방지 | 디버깅 어려울 수 있음 |
| ref-only 소스 스킵 | 불필요한 매니페스트 생성 방지 | 로직 복잡도 |

---

## 부록: 주요 파일 경로 참조

| 파일 | 역할 |
|------|------|
| `reposerver/repository/repository.go` | Repository Server 핵심 구현 (3276줄) |
| `util/helm/helm.go` | Helm CLI 래퍼 인터페이스 |
| `util/helm/client.go` | Helm 레지스트리 클라이언트 |
| `util/helm/cmd.go` | Helm 명령 실행 |
| `util/kustomize/kustomize.go` | Kustomize CLI 래퍼 |
| `cmpserver/plugin/plugin.go` | CMP 서버 Service 구현 |
| `cmpserver/plugin/config.go` | plugin.yaml 파싱 및 검증 |
| `reposerver/cache/cache.go` | 매니페스트 캐시 구현 |
| `reposerver/metrics/metrics.go` | 메트릭 서버 |
| `util/app/discovery/discovery.go` | 앱 소스 타입 자동 감지 |

## 부록: 상수 정의

`reposerver/repository/repository.go` L.70-77:

```go
const (
    cachedManifestGenerationPrefix = "Manifest generation error (cached)"
    helmDepUpMarkerFile            = ".argocd-helm-dep-up"
    repoSourceFile                 = ".argocd-source.yaml"
    appSourceFile                  = ".argocd-source-%s.yaml"
    ociPrefix                      = "oci://"
    skipFileRenderingMarker        = "+argocd:skip-file-rendering"
)

var manifestGenerateLock = sync.NewKeyLock()
var ErrExceededMaxCombinedManifestFileSize = errors.New("exceeded max combined manifest file size")
```

`skipFileRenderingMarker`는 파일 상단에 `+argocd:skip-file-rendering` 주석이 있으면 해당 파일을 렌더링하지 않는 기능에 사용된다.
