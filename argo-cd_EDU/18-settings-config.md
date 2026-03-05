# Argo CD Settings 및 Config 관리 Deep-Dive

## 목차

1. [설정 관리 개요](#1-설정-관리-개요)
2. [ArgoCDSettings 구조체](#2-argocdsettings-구조체)
3. [SettingsManager 구조체](#3-settingsmanager-구조체)
4. [ConfigMap 키 상수](#4-configmap-키-상수)
5. [ArgoDB 인터페이스](#5-argodb-인터페이스)
6. [클러스터 저장](#6-클러스터-저장)
7. [레포지토리 저장](#7-레포지토리-저장)
8. [Secret 인덱서](#8-secret-인덱서)
9. [설정 업데이트 메커니즘](#9-설정-업데이트-메커니즘)
10. [리소스 추적 방식](#10-리소스-추적-방식)
11. [Resource Customizations](#11-resource-customizations)
12. [초기 비밀번호 생성](#12-초기-비밀번호-생성)
13. [왜 이런 설계인가](#13-왜-이런-설계인가)

---

## 1. 설정 관리 개요

Argo CD는 별도의 외부 데이터베이스를 사용하지 않는다. 모든 운영 설정은 Kubernetes 네이티브 리소스인 **Secret**과 **ConfigMap**에 저장된다. 이 설계는 Argo CD가 배포된 Kubernetes 클러스터 자체를 영구 저장소로 활용하므로, PostgreSQL 같은 외부 DB 없이 완전한 GitOps 방식으로 운영할 수 있다.

```
+----------------------------------------------------------+
|                   Argo CD 설정 저장소                     |
|                                                          |
|  argocd-cm (ConfigMap)                                   |
|  ├── url: https://argocd.example.com                    |
|  ├── dex.config: |                                       |
|  │     connectors: ...                                   |
|  ├── application.resourceTrackingMethod: annotation      |
|  ├── resource.customizations: ...                        |
|  └── exec.enabled: "false"                              |
|                                                          |
|  argocd-secret (Secret)                                  |
|  ├── server.secretkey: <JWT 서명 키>                      |
|  ├── tls.crt: <TLS 인증서>                               |
|  ├── tls.key: <TLS 개인키>                               |
|  ├── webhook.github.secret: <GitHub Webhook 시크릿>      |
|  └── webhook.gitlab.secret: <GitLab Webhook 시크릿>      |
|                                                          |
|  cluster-{host}-{hash} (Secret, 클러스터당 1개)           |
|  ├── server: https://k8s.example.com                     |
|  ├── name: production                                    |
|  ├── config: {"bearerToken": "..."}                     |
|  └── label: argocd.argoproj.io/secret-type: cluster     |
|                                                          |
|  repo-{hash} (Secret, 레포지토리당 1개)                   |
|  ├── url: https://github.com/myorg/myrepo.git            |
|  ├── username: git                                       |
|  ├── password: ***                                       |
|  └── label: argocd.argoproj.io/secret-type: repository  |
+----------------------------------------------------------+
```

**SettingsManager**가 이 모든 설정 리소스에 대한 중앙 게이트웨이 역할을 한다. 모든 컴포넌트(API 서버, App Controller, Repo Server)는 SettingsManager를 통해 설정을 읽고 변경 통지를 받는다.

### 핵심 K8s 리소스 목록

| 리소스 이름 | 종류 | 역할 |
|------------|------|------|
| `argocd-cm` | ConfigMap | 전체 운영 설정 (URL, OIDC, 추적 방식 등) |
| `argocd-secret` | Secret | JWT 키, TLS 인증서, Webhook 시크릿 |
| `argocd-initial-admin-secret` | Secret | 초기 admin 비밀번호 |
| `argocd-server-tls` | Secret | 외부 관리 TLS 인증서 (선택) |
| `argocd-ssh-known-hosts-cm` | ConfigMap | SSH known hosts |
| `argocd-tls-certs-cm` | ConfigMap | 커스텀 TLS 인증서 |
| `argocd-gpg-keys-cm` | ConfigMap | GPG 공개키 |
| `cluster-{host}-{hash}` | Secret | 등록된 클러스터 크리덴셜 |
| `repo-{hash}` | Secret | 등록된 레포지토리 크리덴셜 |
| `creds-{hash}` | Secret | 레포지토리 크리덴셜 템플릿 |

---

## 2. ArgoCDSettings 구조체

`util/settings/settings.go` L.64-153에 정의된 `ArgoCDSettings`는 Kubernetes ConfigMap과 Secret에서 읽어 들인 모든 설정을 메모리 안에서 표현하는 중심 구조체다.

```go
// util/settings/settings.go L.64-153
type ArgoCDSettings struct {
    // URL은 사용자가 Argo CD에 접근하는 외부 URL.
    // SSO 설정 시 콜백 URL로 사용됨. 비어 있으면 SSO 비활성화.
    URL string `json:"url,omitempty"`

    // AdditionalURLs: 멀티 도메인에서 SSO를 지원할 때 사용하는 추가 URL 목록
    AdditionalURLs []string `json:"additionalUrls,omitempty"`

    // DexConfig: Dex OIDC 설정 (YAML 문자열)
    DexConfig string `json:"dexConfig,omitempty"`

    // OIDCConfigRAW: 외부 OIDC 제공자 설정 (YAML 문자열)
    OIDCConfigRAW string `json:"oidcConfig,omitempty"`

    // ServerSignature: JWT 토큰 서명에 사용되는 HMAC-SHA256 키
    // argocd-secret의 server.secretkey 필드에서 로드됨
    ServerSignature []byte `json:"serverSignature,omitempty"`

    // Certificate: Argo CD API 서버의 TLS 인증서/개인키 쌍.
    // nil이면 TLS 없이 insecure 모드로 실행됨.
    Certificate *tls.Certificate `json:"-"`

    // CertificateIsExternal: argocd-server-tls Secret에서 로드된 외부 인증서인지 여부
    CertificateIsExternal bool `json:"-"`

    // Webhook 시크릿: 각 Git 플랫폼의 webhook 이벤트 인증에 사용
    WebhookGitHubSecret          string `json:"webhookGitHubSecret,omitempty"`
    WebhookGitLabSecret          string `json:"webhookGitLabSecret,omitempty"`
    WebhookBitbucketUUID         string `json:"webhookBitbucketUUID,omitempty"`
    WebhookBitbucketServerSecret string `json:"webhookBitbucketServerSecret,omitempty"`
    WebhookGogsSecret            string `json:"webhookGogsSecret,omitempty"`
    WebhookAzureDevOpsUsername   string `json:"webhookAzureDevOpsUsername,omitempty"`
    WebhookAzureDevOpsPassword   string `json:"webhookAzureDevOpsPassword,omitempty"`

    // Secrets: argocd-secret을 포함한 모든 argocd 관련 Secret의 키-값 맵
    // OIDC/Dex 설정에서 $secret_name:key_name 형식으로 참조됨
    Secrets map[string]string `json:"secrets,omitempty"`

    // KustomizeBuildOptions: Kustomize 빌드 시 전달할 전역 옵션 문자열
    KustomizeBuildOptions string `json:"kustomizeBuildOptions,omitempty"`

    // AnonymousUserEnabled: 인증 없이 읽기 전용 접근 허용 여부
    AnonymousUserEnabled bool `json:"anonymousUserEnabled,omitempty"`

    // UserSessionDuration: JWT 토큰 유효 기간 (기본: 24h)
    UserSessionDuration time.Duration `json:"userSessionDuration,omitempty"`

    // InClusterEnabled: https://kubernetes.default.svc 주소 허용 여부
    InClusterEnabled bool `json:"inClusterEnabled"`

    // ExecEnabled: UI의 exec 터미널 기능 활성화 여부
    ExecEnabled bool `json:"execEnabled"`

    // ExecShells: exec에서 허용할 셸 목록 (시도 순서 반영)
    ExecShells []string `json:"execShells"`

    // TrackingMethod: 리소스 추적 방식 (annotation/label/annotation+label)
    TrackingMethod string `json:"application.resourceTrackingMethod,omitempty"`

    // ImpersonationEnabled: App sync 권한을 control plane에서 분리하는 impersonation 기능
    ImpersonationEnabled bool `json:"impersonationEnabled"`

    // RequireOverridePrivilegeForRevisionSync: sync 시 외부 revision 지정을 override로 취급할지 여부
    RequireOverridePrivilegeForRevisionSync bool `json:"requireOverridePrivilegeForRevisionSync"`
}
```

### 주요 필드 설명

#### URL 및 SSO 관련

`URL` 필드는 SSO(Dex 또는 외부 OIDC)에서 콜백 URL을 구성할 때 반드시 필요하다. URL이 비어 있으면 `IsDexConfigured()` 메서드가 `false`를 반환하여 SSO가 자동으로 비활성화된다.

```go
// util/settings/settings.go L.1780-1790
func (a *ArgoCDSettings) IsDexConfigured() bool {
    if a.URL == "" {
        return false
    }
    dexCfg, err := UnmarshalDexConfig(a.DexConfig)
    if err != nil {
        log.Warnf("invalid dex yaml config: %s", err.Error())
        return false
    }
    return len(dexCfg) > 0
}
```

#### ServerSignature와 JWT

`ServerSignature`는 HMAC-SHA256으로 JWT 토큰을 서명하는 데 쓰이는 바이트 슬라이스다. `argocd-secret`의 `server.secretkey` 필드에서 읽어온다. 또한 AES 기반 암호화 키도 이 서명에서 파생된다.

```go
// util/settings/settings.go L.1793-1795
func (a *ArgoCDSettings) GetServerEncryptionKey() ([]byte, error) {
    return crypto.KeyFromPassphrase(string(a.ServerSignature))
}
```

#### Secrets 맵과 Secret 참조

`Secrets` 맵은 `argocd-secret`을 포함한 `app.kubernetes.io/part-of=argocd` 레이블이 붙은 모든 Secret의 키-값 쌍을 저장한다. OIDC 설정 등에서 `$secret_name:key_name` 형식으로 시크릿을 간접 참조할 수 있다.

```go
// util/settings/settings.go L.1599-1608
secretValues := make(map[string]string, len(argoCDSecret.Data))
for _, s := range secrets {
    for k, v := range s.Data {
        secretValues[fmt.Sprintf("%s:%s", s.Name, k)] = string(v)
    }
}
for k, v := range argoCDSecret.Data {
    secretValues[k] = string(v)
}
settings.Secrets = secretValues
```

---

## 3. SettingsManager 구조체

`util/settings/settings.go` L.575-595에 정의된 `SettingsManager`는 Kubernetes API와 informer를 통해 설정 리소스를 캐시하고, 변경 시 구독자에게 알림을 전파한다.

```go
// util/settings/settings.go L.575-595
type SettingsManager struct {
    ctx             context.Context
    clientset       kubernetes.Interface       // Kubernetes API 클라이언트

    // informer 기반 캐시 (API 서버 직접 호출 없이 메모리 조회)
    secrets         v1listers.SecretLister
    secretsInformer cache.SharedIndexInformer
    configmaps      v1listers.ConfigMapLister

    namespace string

    // subscribers: 설정 변경 시 알림을 받는 채널 목록
    subscribers []chan<- *ArgoCDSettings

    // mutex: subscribers 목록 및 초기화 플래그 보호
    mutex                     *sync.Mutex
    initContextCancel         func()
    reposOrClusterChanged     func()

    // TLS 인증서 파싱 함수 및 캐시
    tlsCertParser             func([]byte, []byte) (tls.Certificate, error)
    tlsCertCache              *tls.Certificate
    tlsCertCacheSecretName    string
    tlsCertCacheSecretVersion string

    // 클러스터 조회 최적화를 위한 전용 informer
    clusterInformer *ClusterInformer
}
```

### SettingsManager 생성

```go
// util/settings/settings.go L.1749-1763
func NewSettingsManager(ctx context.Context, clientset kubernetes.Interface, namespace string, opts ...SettingsManagerOpts) *SettingsManager {
    mgr := &SettingsManager{
        ctx:           ctx,
        clientset:     clientset,
        namespace:     namespace,
        mutex:         &sync.Mutex{},
        tlsCertParser: tls.X509KeyPair,  // 기본 TLS 파서
    }
    for i := range opts {
        opts[i](mgr)  // 함수형 옵션 패턴으로 확장 지원
    }
    return mgr
}
```

### 초기화 흐름

```
NewSettingsManager()
    |
    +-- ensureSynced() 호출 시 initialize() 실행
         |
         +-- ConfigMap informer 시작 (app.kubernetes.io/part-of=argocd 레이블 필터)
         +-- Secret informer 시작
         +-- ClusterInformer 시작 (클러스터 전용 최적화 캐시)
         +-- WaitForCacheSync() -- 모든 캐시가 동기화될 때까지 대기
         |
         +-- 변경 이벤트 핸들러 등록:
              UpdateFunc → tryNotify() → notifySubscribers()
              AddFunc    → tryNotify() (새로 생성된 오브젝트만)
```

실제 초기화 코드:

```go
// util/settings/settings.go L.1349-1415
func (mgr *SettingsManager) initialize(ctx context.Context) error {
    tweakConfigMap := func(options *metav1.ListOptions) {
        // argocd 관련 ConfigMap만 감시
        cmLabelSelector := fields.ParseSelectorOrDie(partOfArgoCDSelector)
        options.LabelSelector = cmLabelSelector.String()
    }

    indexers := cache.Indexers{
        cache.NamespaceIndex:      cache.MetaNamespaceIndexFunc,
        ByClusterURLIndexer:       byClusterURLIndexerFunc,
        ByClusterNameIndexer:      byClusterNameIndexerFunc,
        ByProjectClusterIndexer:   byProjectIndexerFunc(common.LabelValueSecretTypeCluster),
        ByProjectRepoIndexer:      byProjectIndexerFunc(common.LabelValueSecretTypeRepository),
        ByProjectRepoWriteIndexer: byProjectIndexerFunc(common.LabelValueSecretTypeRepositoryWrite),
    }

    cmInformer := informersv1.NewFilteredConfigMapInformer(...)
    secretsInformer := informersv1.NewSecretInformer(...)
    clusterInformer, err := NewClusterInformer(...)

    // 동기화 대기
    if !cache.WaitForCacheSync(ctx.Done(), cmInformer.HasSynced, secretsInformer.HasSynced, clusterInformer.HasSynced) {
        return errors.New("timed out waiting for settings cache to sync")
    }
    // ...
}
```

---

## 4. ConfigMap 키 상수

`util/settings/settings.go` L.419-558에 정의된 ConfigMap 키 상수들이다. 이 상수들은 `argocd-cm` ConfigMap의 `data` 필드 키로 사용된다.

| 상수명 | ConfigMap 키 | 설명 | 기본값 |
|--------|-------------|------|-------|
| `settingURLKey` | `url` | Argo CD 외부 접근 URL (SSO 콜백 필수) | - |
| `settingAdditionalUrlsKey` | `additionalUrls` | 멀티 도메인 SSO 지원용 추가 URL 목록 | - |
| `settingDexConfigKey` | `dex.config` | Dex OIDC 공급자 설정 YAML | - |
| `settingsOIDCConfigKey` | `oidc.config` | 외부 OIDC 제공자 설정 YAML | - |
| `statusBadgeEnabledKey` | `statusbadge.enabled` | 상태 배지 기능 활성화 | `false` |
| `settingsWebhookGitHubSecretKey` | `webhook.github.secret` | GitHub webhook 공유 시크릿 | - |
| `settingsWebhookGitLabSecretKey` | `webhook.gitlab.secret` | GitLab webhook 공유 시크릿 | - |
| `settingsWebhookBitbucketUUIDKey` | `webhook.bitbucket.uuid` | Bitbucket webhook UUID | - |
| `settingsWebhookBitbucketServerSecretKey` | `webhook.bitbucketserver.secret` | BitbucketServer webhook 시크릿 | - |
| `settingsWebhookGogsSecretKey` | `webhook.gogs.secret` | Gogs webhook 시크릿 | - |
| `settingsWebhookAzureDevOpsUsernameKey` | `webhook.azuredevops.username` | Azure DevOps webhook 사용자명 | - |
| `settingsWebhookAzureDevOpsPasswordKey` | `webhook.azuredevops.password` | Azure DevOps webhook 비밀번호 | - |
| `settingsWebhookMaxPayloadSizeMB` | `webhook.maxPayloadSizeMB` | Webhook 최대 페이로드 크기(MB) | `50` |
| `settingsApplicationInstanceLabelKey` | `application.instanceLabelKey` | 앱 인스턴스 추적 레이블 키 | `app.kubernetes.io/instance` |
| `settingsResourceTrackingMethodKey` | `application.resourceTrackingMethod` | 리소스 추적 방식 | `annotation` |
| `resourceCustomizationsKey` | `resource.customizations` | Lua 기반 리소스 커스터마이제이션 맵 | - |
| `resourceExclusionsKey` | `resource.exclusions` | 동기화에서 제외할 리소스 목록 | - |
| `resourceInclusionsKey` | `resource.inclusions` | 감시할 리소스 명시적 포함 목록 | - |
| `kustomizeBuildOptionsKey` | `kustomize.buildOptions` | Kustomize 빌드 전역 옵션 | - |
| `kustomizeVersionKeyPrefix` | `kustomize.version` | 특정 버전 Kustomize 설정 | - |
| `kustomizePathPrefixKey` | `kustomize.path` | 특정 버전 Kustomize 바이너리 경로 | - |
| `anonymousUserEnabledKey` | `users.anonymous.enabled` | 익명 사용자 허용 여부 | `false` |
| `userSessionDurationKey` | `users.session.duration` | 사용자 세션(JWT) 유효 기간 | `24h` |
| `resourceCompareOptionsKey` | `resource.compareoptions` | diff 옵션 (상태 무시 여부 등) | - |
| `settingUICSSURLKey` | `ui.cssurl` | UI 커스텀 CSS URL | - |
| `settingUIBannerContentKey` | `ui.bannercontent` | UI 배너 내용 | - |
| `inClusterEnabledKey` | `cluster.inClusterEnabled` | in-cluster 주소 허용 여부 | `true` |
| `settingsMaxPodLogsToRender` | `server.maxPodLogsToRender` | 렌더링할 최대 Pod 로그 수 | `10` |
| `execEnabledKey` | `exec.enabled` | UI exec 터미널 기능 활성화 | `false` |
| `execShellsKey` | `exec.shells` | exec에서 허용할 셸 목록 (콤마 구분) | `bash,sh,powershell,cmd` |
| `oidcTLSInsecureSkipVerifyKey` | `oidc.tls.insecure.skip.verify` | OIDC TLS 검증 건너뛰기 | `false` |
| `helmValuesFileSchemesKey` | `helm.valuesFileSchemes` | Helm values 파일 허용 스키마 | `https,http` |
| `impersonationEnabledKey` | `application.sync.impersonation.enabled` | 동기화 권한 분리(impersonation) 활성화 | `false` |
| `globalProjectsKey` | `globalProjects` | 글로벌 프로젝트 설정 | - |
| `extensionConfig` | `extension.config` | Argo CD 프록시 확장 설정 | - |
| `RespectRBAC` | `resource.respectRBAC` | 리소스 감시 시 RBAC 적용 여부 | 비활성 |

### Secret 전용 키

`argocd-secret`에서 읽는 키들:

| 상수명 | Secret 키 | 설명 |
|--------|----------|------|
| `settingServerSignatureKey` | `server.secretkey` | JWT HMAC 서명 키 |
| `settingServerCertificate` | `tls.crt` | TLS 공개 인증서 (PEM) |
| `settingServerPrivateKey` | `tls.key` | TLS 개인키 (PEM) |

### 설정 로딩 과정

```
GetSettings()
    |
    +-- getConfigMap()    → argocd-cm 조회 (informer 캐시)
    +-- getSecret()       → argocd-secret 조회 (informer 캐시)
    +-- getSecrets()      → part-of=argocd 레이블 Secret 전체 조회
    |
    +-- updateSettingsFromSecret()  → ServerSignature, TLS, Webhook 시크릿 로드
    +-- updateSettingsFromConfigMap() → URL, OIDC, Tracking, Exec 등 로드
    |
    --> *ArgoCDSettings 반환
```

---

## 5. ArgoDB 인터페이스

`util/db/db.go` L.25-125에 정의된 `ArgoDB` 인터페이스는 Kubernetes Secret 기반 CRUD 연산을 추상화한다. 별도 데이터베이스 없이 K8s Secret이 영구 저장소 역할을 한다.

```go
// util/db/db.go L.25-125
type ArgoDB interface {
    // ---- 클러스터 관리 ----
    ListClusters(ctx context.Context) (*appv1.ClusterList, error)
    CreateCluster(ctx context.Context, c *appv1.Cluster) (*appv1.Cluster, error)
    WatchClusters(ctx context.Context,
        handleAddEvent func(cluster *appv1.Cluster),
        handleModEvent func(oldCluster *appv1.Cluster, newCluster *appv1.Cluster),
        handleDeleteEvent func(clusterServer string)) error
    GetCluster(ctx context.Context, server string) (*appv1.Cluster, error)
    GetClusterServersByName(ctx context.Context, name string) ([]string, error)
    GetProjectClusters(ctx context.Context, project string) ([]*appv1.Cluster, error)
    UpdateCluster(ctx context.Context, c *appv1.Cluster) (*appv1.Cluster, error)
    DeleteCluster(ctx context.Context, server string) error

    // ---- 레포지토리 관리 (읽기용) ----
    ListRepositories(ctx context.Context) ([]*appv1.Repository, error)
    CreateRepository(ctx context.Context, r *appv1.Repository) (*appv1.Repository, error)
    GetRepository(ctx context.Context, url, project string) (*appv1.Repository, error)
    GetProjectRepositories(project string) ([]*appv1.Repository, error)
    RepositoryExists(ctx context.Context, repoURL, project string) (bool, error)
    UpdateRepository(ctx context.Context, r *appv1.Repository) (*appv1.Repository, error)
    DeleteRepository(ctx context.Context, name, project string) error

    // ---- 레포지토리 관리 (쓰기용, hydration) ----
    CreateWriteRepository(ctx context.Context, r *appv1.Repository) (*appv1.Repository, error)
    GetWriteRepository(ctx context.Context, url, project string) (*appv1.Repository, error)
    GetProjectWriteRepositories(project string) ([]*appv1.Repository, error)
    WriteRepositoryExists(ctx context.Context, repoURL, project string) (bool, error)
    UpdateWriteRepository(ctx context.Context, r *appv1.Repository) (*appv1.Repository, error)
    DeleteWriteRepository(ctx context.Context, name, project string) error

    // ---- 레포지토리 크리덴셜 템플릿 ----
    ListRepositoryCredentials(ctx context.Context) ([]string, error)
    GetRepositoryCredentials(ctx context.Context, name string) (*appv1.RepoCreds, error)
    CreateRepositoryCredentials(ctx context.Context, r *appv1.RepoCreds) (*appv1.RepoCreds, error)
    UpdateRepositoryCredentials(ctx context.Context, r *appv1.RepoCreds) (*appv1.RepoCreds, error)
    DeleteRepositoryCredentials(ctx context.Context, name string) error

    // ---- 쓰기용 레포지토리 크리덴셜 템플릿 ----
    ListWriteRepositoryCredentials(ctx context.Context) ([]string, error)
    GetWriteRepositoryCredentials(ctx context.Context, name string) (*appv1.RepoCreds, error)
    // ...

    // ---- 인증서 관리 ----
    ListRepoCertificates(ctx context.Context, selector *CertificateListSelector) (*appv1.RepositoryCertificateList, error)
    CreateRepoCertificate(ctx context.Context, certificate *appv1.RepositoryCertificateList, upsert bool) (*appv1.RepositoryCertificateList, error)
    RemoveRepoCertificates(ctx context.Context, selector *CertificateListSelector) (*appv1.RepositoryCertificateList, error)

    // ---- Helm/OCI 레포지토리 ----
    GetAllHelmRepositoryCredentials(ctx context.Context) ([]*appv1.RepoCreds, error)
    GetAllOCIRepositoryCredentials(ctx context.Context) ([]*appv1.RepoCreds, error)
    ListHelmRepositories(ctx context.Context) ([]*appv1.Repository, error)
    ListOCIRepositories(ctx context.Context) ([]*appv1.Repository, error)

    // ---- GPG 키 관리 ----
    ListConfiguredGPGPublicKeys(ctx context.Context) (map[string]*appv1.GnuPGPublicKey, error)
    AddGPGPublicKey(ctx context.Context, keyData string) (map[string]*appv1.GnuPGPublicKey, []string, error)
    DeleteGPGPublicKey(ctx context.Context, keyID string) error

    // ---- App Controller 복제 수 조회 ----
    GetApplicationControllerReplicas() int
}
```

### db 구현체

```go
// util/db/db.go L.127-140
type db struct {
    ns            string                    // 네임스페이스
    kubeclientset kubernetes.Interface      // K8s API 클라이언트
    settingsMgr   *settings.SettingsManager // 설정 캐시 접근
}

func NewDB(namespace string, settingsMgr *settings.SettingsManager, kubeclientset kubernetes.Interface) ArgoDB {
    return &db{
        settingsMgr:   settingsMgr,
        ns:            namespace,
        kubeclientset: kubeclientset,
    }
}
```

### GetApplicationControllerReplicas

App Controller 레플리카 수는 Deployment 리소스에서 읽는다. Deployment가 없으면 환경 변수 `ARGOCD_CONTROLLER_REPLICAS`에서 읽는다.

```go
// util/db/db.go L.143-157
func (db *db) GetApplicationControllerReplicas() int {
    applicationControllerName := env.StringFromEnv(common.EnvAppControllerName, common.DefaultApplicationControllerName)
    appControllerDeployment, err := db.kubeclientset.AppsV1().Deployments(db.settingsMgr.GetNamespace()).Get(
        context.Background(), applicationControllerName, metav1.GetOptions{})
    if err != nil {
        // NotFound가 아닌 오류는 경고만 출력
        if !apierrors.IsNotFound(err) {
            log.Warnf("error retrieveing Argo CD controller deployment: %s", err)
        }
    }
    if appControllerDeployment != nil && appControllerDeployment.Spec.Replicas != nil {
        return int(*appControllerDeployment.Spec.Replicas)
    }
    return env.ParseNumFromEnv(common.EnvControllerReplicas, 0, 0, math.MaxInt32)
}
```

---

## 6. 클러스터 저장

클러스터 정보는 Kubernetes Secret으로 저장된다. `util/db/cluster.go`에서 관련 로직을 찾을 수 있다.

### Secret 이름 생성: URIToSecretName

`util/db/secrets.go` L.160-187에 구현된 `URIToSecretName`은 클러스터 서버 URL을 Secret 이름으로 변환한다.

```go
// util/db/secrets.go L.160-187
func URIToSecretName(uriType, uri string) (string, error) {
    parsedURI, err := url.ParseRequestURI(uri)
    if err != nil {
        return "", err
    }

    // 호스트 추출: IPv6 주소의 경우 대괄호 제거 및 콜론을 하이픈으로 변환
    host := parsedURI.Host
    if strings.HasPrefix(host, "[") {
        last := strings.Index(host, "]")
        if last >= 0 {
            addr, err := netip.ParseAddr(host[1:last])
            if err != nil {
                return "", err
            }
            host = strings.ReplaceAll(addr.String(), ":", "-")
        }
    } else {
        // 포트 번호 제거
        last := strings.Index(host, ":")
        if last >= 0 {
            host = host[0:last]
        }
    }

    // FNV-32a 해시로 URI 전체 해싱
    h := fnv.New32a()
    _, _ = h.Write([]byte(uri))
    host = strings.ToLower(host)

    // 최종 이름: "cluster-{호스트}-{FNV32a 해시}"
    return fmt.Sprintf("%s-%s-%v", uriType, host, h.Sum32()), nil
}
```

예시:
- `https://k8s.example.com` → `cluster-k8s.example.com-2847562398`
- `https://192.168.1.100:6443` → `cluster-192.168.1.100-1234567890`

### clusterToSecret: 클러스터를 Secret으로 변환

```go
// util/db/cluster.go L.356-399
func clusterToSecret(c *appv1.Cluster, secret *corev1.Secret) error {
    data := make(map[string][]byte)
    data["server"] = []byte(strings.TrimRight(c.Server, "/"))  // 서버 URL
    if c.Name == "" {
        data["name"] = []byte(c.Server)
    } else {
        data["name"] = []byte(c.Name)  // 클러스터 표시 이름
    }
    if len(c.Namespaces) != 0 {
        data["namespaces"] = []byte(strings.Join(c.Namespaces, ","))
    }

    // ClusterConfig 구조체를 JSON으로 직렬화 (bearerToken, TLS 설정 등 포함)
    configBytes, err := json.Marshal(c.Config)
    if err != nil {
        return err
    }
    data["config"] = configBytes

    if c.Shard != nil {
        data["shard"] = []byte(strconv.Itoa(int(*c.Shard)))  // App Controller 샤드 번호
    }
    if c.ClusterResources {
        data["clusterResources"] = []byte("true")
    }
    if c.Project != "" {
        data["project"] = []byte(c.Project)  // 프로젝트 범위 클러스터
    }
    secret.Data = data

    // argocd.argoproj.io/secret-type: cluster 레이블 추가
    addSecretMetadata(secret, common.LabelValueSecretTypeCluster)
    return nil
}
```

### SecretToCluster: Secret을 클러스터 객체로 역변환

```go
// util/db/cluster.go L.402-464
func SecretToCluster(s *corev1.Secret) (*appv1.Cluster, error) {
    var config appv1.ClusterConfig
    if len(s.Data["config"]) > 0 {
        err := json.Unmarshal(s.Data["config"], &config)
        if err != nil {
            return nil, fmt.Errorf("failed to unmarshal cluster config: %w", err)
        }
    }

    // 콤마로 구분된 네임스페이스 파싱
    var namespaces []string
    for ns := range strings.SplitSeq(string(s.Data["namespaces"]), ",") {
        if ns = strings.TrimSpace(ns); ns != "" {
            namespaces = append(namespaces, ns)
        }
    }

    cluster := appv1.Cluster{
        ID:               string(s.UID),
        Server:           strings.TrimRight(string(s.Data["server"]), "/"),
        Name:             string(s.Data["name"]),
        Namespaces:       namespaces,
        ClusterResources: string(s.Data["clusterResources"]) == "true",
        Config:           config,
        Shard:            shard,
        Project:          string(s.Data["project"]),
        Labels:           labels,
        Annotations:      annotations,
    }
    return &cluster, nil
}
```

### 로컬 클러스터 (in-cluster)

Argo CD가 배포된 클러스터 자신은 `https://kubernetes.default.svc` 주소로 표현된다. 이 주소는 K8s 내부 API 서버 주소이며, 별도 Secret 없이 Pod의 ServiceAccount 토큰으로 자동 인증된다.

```go
// util/db/cluster.go L.28-36
var (
    localCluster = appv1.Cluster{
        Name:   "in-cluster",
        Server: appv1.KubernetesInternalAPIServerAddr,  // "https://kubernetes.default.svc"
        Info: appv1.ClusterInfo{
            ConnectionState: appv1.ConnectionState{Status: appv1.ConnectionStatusSuccessful},
        },
    }
    initLocalCluster sync.Once
)
```

`InClusterEnabled` 설정이 `false`이면 이 클러스터는 목록에서 제외된다:

```go
// util/db/cluster.go L.71-96 (ListClusters 내부)
inClusterEnabled := settings.InClusterEnabled
hasInClusterCredentials := false
for _, clusterSecret := range clusterSecrets {
    cluster, err := SecretToCluster(clusterSecret)
    if cluster.Server == appv1.KubernetesInternalAPIServerAddr {
        if inClusterEnabled {
            hasInClusterCredentials = true
            clusterList.Items = append(clusterList.Items, *cluster)
        }
    } else {
        clusterList.Items = append(clusterList.Items, *cluster)
    }
}
if inClusterEnabled && !hasInClusterCredentials {
    // Secret 없이 로컬 클러스터를 자동으로 목록에 추가
    clusterList.Items = append(clusterList.Items, *db.getLocalCluster())
}
```

### 클러스터 Secret 구조 요약

```
apiVersion: v1
kind: Secret
metadata:
  name: cluster-k8s.example.com-2847562398
  namespace: argocd
  labels:
    argocd.argoproj.io/secret-type: cluster  # 필수 레이블
  annotations:
    managed-by: argocd.argoproj.io           # Argo CD 관리 마커
data:
  server: aHR0cHM6Ly9rOHMuZXhhbXBsZS5jb20=  # base64(https://k8s.example.com)
  name: cHJvZHVjdGlvbg==                      # base64(production)
  config: eyJiZWFyZXJUb2tlbiI6Ii4uLiJ9       # base64(JSON)
  namespaces: ZGVmYXVsdCxhcHBz               # base64(default,apps)
  shard: MQ==                                  # base64(1)
  project: bXktcHJvamVjdA==                   # base64(my-project)
```

---

## 7. 레포지토리 저장

레포지토리 크리덴셜도 Kubernetes Secret으로 저장된다. `util/db/repository.go`에 관련 구현이 있다.

### Secret 이름 생성: RepoURLToSecretName

```go
// util/db/repository.go L.468-477
func RepoURLToSecretName(prefix string, repo string, project string) string {
    h := fnv.New32a()
    _, _ = h.Write([]byte(repo))
    _, _ = h.Write([]byte(project))
    return fmt.Sprintf("%s-%v", prefix, h.Sum32())
}
```

Secret 이름 패턴:
- 읽기 레포지토리: `repo-{FNV32a(url+project)}`
- 쓰기 레포지토리: `repo-write-{FNV32a(url+project)}`
- 크리덴셜 템플릿: `creds-{FNV32a(url)}`
- 쓰기 크리덴셜 템플릿: `creds-write-{FNV32a(url)}`

관련 상수 (`util/db/repository.go` L.18-35):

```go
const (
    repoSecretPrefix      = "repo"        // 읽기 레포지토리 Secret 접두사
    repoWriteSecretPrefix = "repo-write"  // 쓰기 레포지토리 Secret 접두사
    credSecretPrefix      = "creds"       // 크리덴셜 템플릿 Secret 접두사
    credWriteSecretPrefix = "creds-write" // 쓰기 크리덴셜 템플릿 Secret 접두사
    username              = "username"    // Secret 내 사용자명 키
    password              = "password"    // Secret 내 비밀번호 키
    project               = "project"     // Secret 내 프로젝트 키
    sshPrivateKey         = "sshPrivateKey" // Secret 내 SSH 키 키
)
```

### 크리덴셜 상속: enrichCredsToRepo

레포지토리가 자체 크리덴셜을 갖지 않을 때, URL prefix 매칭으로 크리덴셜 템플릿에서 자동으로 크리덴셜을 상속한다.

```go
// util/db/repository.go L.434-449
func (db *db) enrichCredsToRepo(ctx context.Context, repository *v1alpha1.Repository) error {
    if !repository.HasCredentials() {
        // 크리덴셜 없으면 URL prefix로 템플릿 검색
        creds, err := db.GetRepositoryCredentials(ctx, repository.Repo)
        if err != nil {
            return fmt.Errorf("failed to get repository credentials for %q: %w", repository.Repo, err)
        }
        if creds != nil {
            repository.CopyCredentialsFrom(creds)
            repository.InheritedCreds = true  // 상속 여부 표시
        }
    } else {
        log.Debugf("%s has credentials", repository.Repo)
    }
    return nil
}
```

크리덴셜 템플릿 매칭 예시:

```
크리덴셜 템플릿 URL: https://github.com/myorg
  -> 매칭됨: https://github.com/myorg/repo-a.git
  -> 매칭됨: https://github.com/myorg/repo-b.git
  -> 매칭 안됨: https://github.com/otherorg/repo.git
```

### 레포지토리 Secret 구조

```
apiVersion: v1
kind: Secret
metadata:
  name: repo-1234567890
  namespace: argocd
  labels:
    argocd.argoproj.io/secret-type: repository
data:
  url: aHR0cHM6Ly9naXRodWIuY29t...  # 레포지토리 URL
  username: Z2l0                       # 사용자명
  password: ***                         # 비밀번호 또는 토큰
  sshPrivateKey: LS0tLS1C...           # SSH 개인키 (선택)
  project: bXktcHJvamVjdA==            # 프로젝트 스코프 (선택)
  type: Z2l0                            # git 또는 helm
  insecure: ZmFsc2U=                    # TLS 검증 건너뛰기 여부
```

---

## 8. Secret 인덱서

효율적인 조회를 위해 `secretsInformer`에 여러 인덱서가 등록된다. 인덱서는 Secret 데이터를 특정 필드로 역인덱싱하여 O(1) 조회를 가능하게 한다.

### 인덱서 정의 (util/settings/settings.go L.232-288)

```go
var (
    // 클러스터 URL로 Secret 조회
    ByClusterURLIndexer     = "byClusterURL"
    byClusterURLIndexerFunc = func(obj any) ([]string, error) {
        s, ok := obj.(*corev1.Secret)
        if !ok { return nil, nil }
        if s.Labels == nil || s.Labels[common.LabelKeySecretType] != common.LabelValueSecretTypeCluster {
            return nil, nil
        }
        if url, ok := s.Data["server"]; ok {
            return []string{strings.TrimRight(string(url), "/")}, nil
        }
        return nil, nil
    }

    // 클러스터 이름으로 Secret 조회
    ByClusterNameIndexer     = "byClusterName"
    byClusterNameIndexerFunc = func(obj any) ([]string, error) {
        s, ok := obj.(*corev1.Secret)
        if !ok { return nil, nil }
        if s.Labels == nil || s.Labels[common.LabelKeySecretType] != common.LabelValueSecretTypeCluster {
            return nil, nil
        }
        if name, ok := s.Data["name"]; ok {
            return []string{string(name)}, nil
        }
        return nil, nil
    }

    // 프로젝트 이름으로 클러스터/레포 Secret 조회
    ByProjectClusterIndexer   = "byProjectCluster"
    ByProjectRepoIndexer      = "byProjectRepo"
    ByProjectRepoWriteIndexer = "byProjectRepoWrite"
    byProjectIndexerFunc      = func(secretType string) func(obj any) ([]string, error) {
        return func(obj any) ([]string, error) {
            s, ok := obj.(*corev1.Secret)
            if !ok { return nil, nil }
            if s.Labels == nil || s.Labels[common.LabelKeySecretType] != secretType {
                return nil, nil
            }
            if project, ok := s.Data["project"]; ok {
                return []string{string(project)}, nil
            }
            return nil, nil
        }
    }
)
```

### 인덱서 등록 (initialize 함수 내부)

```go
// util/settings/settings.go L.1366-1373
indexers := cache.Indexers{
    cache.NamespaceIndex:      cache.MetaNamespaceIndexFunc,
    ByClusterURLIndexer:       byClusterURLIndexerFunc,
    ByClusterNameIndexer:      byClusterNameIndexerFunc,
    ByProjectClusterIndexer:   byProjectIndexerFunc(common.LabelValueSecretTypeCluster),
    ByProjectRepoIndexer:      byProjectIndexerFunc(common.LabelValueSecretTypeRepository),
    ByProjectRepoWriteIndexer: byProjectIndexerFunc(common.LabelValueSecretTypeRepositoryWrite),
}
```

### 인덱서 활용 예시

```go
// util/db/cluster.go L.264-282 (GetProjectClusters)
func (db *db) GetProjectClusters(_ context.Context, project string) ([]*appv1.Cluster, error) {
    informer, err := db.settingsMgr.GetSecretsInformer()
    // ByProjectClusterIndexer로 project 값 기준 O(1) 조회
    secrets, err := informer.GetIndexer().ByIndex(settings.ByProjectClusterIndexer, project)
    var res []*appv1.Cluster
    for i := range secrets {
        cluster, err := SecretToCluster(secrets[i].(*corev1.Secret))
        res = append(res, cluster)
    }
    return res, nil
}
```

```
인덱서 동작 원리:

Secret 추가/수정 시:
  byClusterURLIndexerFunc(secret)
      → ["https://k8s.example.com"]
      → 인덱스: "byClusterURL" → "https://k8s.example.com" → [secret]

GetCluster("https://k8s.example.com") 호출 시:
  informer.GetIndexer().ByIndex("byClusterURL", "https://k8s.example.com")
      → [secret]   (O(1) 해시맵 조회)
      → SecretToCluster(secret)
      → *Cluster
```

---

## 9. 설정 업데이트 메커니즘

### updateSecret / updateConfigMap

설정을 변경할 때는 직접 K8s API를 호출하는 것이 아니라, `updateSecret` 또는 `updateConfigMap` 헬퍼를 통한다. 이 함수들은 DeepCopy → 콜백 → 변경 감지 → Create/Update 패턴을 따른다.

```go
// util/settings/settings.go L.673-709
func (mgr *SettingsManager) updateSecret(callback func(*corev1.Secret) error) error {
    argoCDSecret, err := mgr.getSecret()
    createSecret := false
    if err != nil {
        if !apierrors.IsNotFound(err) {
            return err
        }
        // Secret이 없으면 새로 생성할 준비
        argoCDSecret = &corev1.Secret{
            ObjectMeta: metav1.ObjectMeta{Name: common.ArgoCDSecretName},
            Data: make(map[string][]byte),
        }
        createSecret = true
    }

    // 변경 전 상태 저장 (DeepCopy)
    beforeUpdate := argoCDSecret.DeepCopy()
    err = callback(argoCDSecret)  // 콜백으로 실제 변경 수행
    if err != nil {
        return err
    }

    // 변경이 없으면 불필요한 API 호출 스킵 (낙관적 최적화)
    if !createSecret && reflect.DeepEqual(beforeUpdate.Data, argoCDSecret.Data) {
        return nil
    }

    if createSecret {
        _, err = mgr.clientset.CoreV1().Secrets(mgr.namespace).Create(...)
    } else {
        _, err = mgr.clientset.CoreV1().Secrets(mgr.namespace).Update(...)
    }
    if err != nil {
        return err
    }

    // 업데이트 후 informer 강제 리싱크
    return mgr.ResyncInformers()
}
```

`updateConfigMap`도 동일한 패턴:

```go
// util/settings/settings.go L.711-747
func (mgr *SettingsManager) updateConfigMap(callback func(*corev1.ConfigMap) error) error {
    argoCDCM, err := mgr.getConfigMap()
    createCM := false
    // ... (동일 패턴)
    beforeUpdate := argoCDCM.DeepCopy()
    err = callback(argoCDCM)
    // ...
    if !createCM && reflect.DeepEqual(beforeUpdate.Data, argoCDCM.Data) {
        return nil  // 변경 없으면 스킵
    }
    // Create 또는 Update
    return mgr.ResyncInformers()
}
```

### ResyncInformers

업데이트 후 informer 캐시를 강제로 재동기화한다.

```go
// util/settings/settings.go L.1765-1767
func (mgr *SettingsManager) ResyncInformers() error {
    return mgr.ensureSynced(true)  // forceResync=true
}
```

`ensureSynced(true)` 는 기존 informer context를 취소하고 새로 초기화한다:

```go
// util/settings/settings.go L.1458-1471
func (mgr *SettingsManager) ensureSynced(forceResync bool) error {
    mgr.mutex.Lock()
    defer mgr.mutex.Unlock()
    if !forceResync && mgr.secrets != nil && mgr.configmaps != nil {
        return nil  // 이미 초기화됨
    }
    if mgr.initContextCancel != nil {
        mgr.initContextCancel()  // 기존 informer 중단
    }
    ctx, cancel := context.WithCancel(mgr.ctx)
    mgr.initContextCancel = cancel
    return mgr.initialize(ctx)  // 새 informer 시작
}
```

### 구독자 알림 메커니즘

설정 변경이 감지되면 등록된 구독자 채널로 새 설정을 전파한다.

```go
// util/settings/settings.go L.2109-2143
func (mgr *SettingsManager) Subscribe(subCh chan<- *ArgoCDSettings) {
    mgr.mutex.Lock()
    defer mgr.mutex.Unlock()
    mgr.subscribers = append(mgr.subscribers, subCh)
}

func (mgr *SettingsManager) Unsubscribe(subCh chan<- *ArgoCDSettings) {
    mgr.mutex.Lock()
    defer mgr.mutex.Unlock()
    for i, ch := range mgr.subscribers {
        if ch == subCh {
            mgr.subscribers = append(mgr.subscribers[:i], mgr.subscribers[i+1:]...)
            return
        }
    }
}

func (mgr *SettingsManager) notifySubscribers(newSettings *ArgoCDSettings) {
    mgr.mutex.Lock()
    defer mgr.mutex.Unlock()
    if len(mgr.subscribers) > 0 {
        subscribers := make([]chan<- *ArgoCDSettings, len(mgr.subscribers))
        copy(subscribers, mgr.subscribers)
        // 데드락 방지를 위해 별도 고루틴에서 알림 전송
        go func() {
            log.Infof("Notifying %d settings subscribers: %v", len(subscribers), subscribers)
            for _, sub := range subscribers {
                sub <- newSettings
            }
        }()
    }
}
```

전체 업데이트 흐름:

```
ConfigMap/Secret 변경 (K8s API)
    |
    +--> informer 이벤트 핸들러 (UpdateFunc)
         |
         +--> tryNotify()
              |
              +--> GetSettings()       -- 현재 설정 다시 읽기
              |
              +--> notifySubscribers() -- 구독자에게 새 설정 전파
                   |
                   +--> API 서버: OIDC 재설정, TLS 재로드 등
                   +--> App Controller: 설정 기반 로직 재적용
```

---

## 10. 리소스 추적 방식

Argo CD는 동기화한 리소스가 어느 Application에 속하는지 추적하기 위해 K8s 리소스에 마커를 주입한다. 세 가지 방식이 지원된다.

### TrackingMethod 종류

| 방식 | ConfigMap 값 | 동작 |
|------|-------------|------|
| `annotation` | `annotation` | `argocd.argoproj.io/app-name` 어노테이션 추가 (기본) |
| `label` | `label` | `app.kubernetes.io/instance` 레이블 추가 |
| `annotation+label` | `annotation+label` | 어노테이션과 레이블 모두 추가 |

### 인스턴스 레이블 키 조회

```go
// util/settings/settings.go L.840-850
func (mgr *SettingsManager) GetAppInstanceLabelKey() (string, error) {
    argoCDCM, err := mgr.getConfigMap()
    if err != nil {
        return "", err
    }
    label := argoCDCM.Data[settingsApplicationInstanceLabelKey]
    if label == "" {
        return common.LabelKeyAppInstance, nil  // 기본값: "app.kubernetes.io/instance"
    }
    return label, nil
}
```

### 추적 방식 조회

```go
// util/settings/settings.go L.852-862
func (mgr *SettingsManager) GetTrackingMethod() (string, error) {
    argoCDCM, err := mgr.getConfigMap()
    if err != nil {
        return "", err
    }
    tm := argoCDCM.Data[settingsResourceTrackingMethodKey]
    if tm == "" {
        return string(v1alpha1.TrackingMethodAnnotation), nil  // 기본값: "annotation"
    }
    return tm, nil
}
```

### 리소스에 추적 마커 주입

실제 마커 주입은 gitops-engine의 `SetAppInstance` 함수에서 이루어지며, `ArgoCDSettings.TrackingMethod` 값에 따라 어노테이션 또는 레이블을 선택한다.

```
annotation 방식:
  metadata:
    annotations:
      argocd.argoproj.io/app-name: my-namespace/my-app

label 방식:
  metadata:
    labels:
      app.kubernetes.io/instance: my-app

annotation+label 방식:
  metadata:
    annotations:
      argocd.argoproj.io/app-name: my-namespace/my-app
    labels:
      app.kubernetes.io/instance: my-app
```

---

## 11. Resource Customizations

Argo CD는 Lua 스크립트 기반의 리소스 커스터마이제이션을 지원한다. Health check, Actions, ignoreDifferences 등을 `argocd-cm`에 설정할 수 있다.

### ConfigMap 기반 커스터마이제이션

```yaml
# argocd-cm 예시
data:
  resource.customizations: |
    cert-manager.io/Certificate:
      health.lua: |
        hs = {}
        if obj.status ~= nil then
          if obj.status.conditions ~= nil then
            for i, condition in ipairs(obj.status.conditions) do
              if condition.type == "Ready" and condition.status == "False" then
                hs.status = "Degraded"
                hs.message = condition.message
                return hs
              end
              if condition.type == "Ready" and condition.status == "True" then
                hs.status = "Healthy"
                hs.message = condition.message
                return hs
              end
            end
          end
        end
        hs.status = "Progressing"
        hs.message = "Waiting for certificate"
        return hs
```

### 분리 키 방식 (Split Keys)

`resource.customizations.<type>.<group_kind>` 형식으로 커스터마이제이션 타입별로 분리해서 설정할 수도 있다.

```yaml
data:
  resource.customizations.health.cert-manager.io_Certificate: |
    hs = {}
    ...
  resource.customizations.actions.apps_Deployment: |
    discovery.lua: |
      ...
```

분리 키 파싱 로직:

```go
// util/settings/settings.go L.1087-1151
func (mgr *SettingsManager) appendResourceOverridesFromSplitKeys(cmData map[string]string, resourceOverrides map[string]v1alpha1.ResourceOverride) error {
    for k, v := range cmData {
        if !strings.HasPrefix(k, resourceCustomizationsKey) {
            continue
        }
        // "resource.customizations.<type>.<group_kind>" 형식 파싱
        parts := strings.SplitN(k, ".", 4)
        if len(parts) < 4 {
            continue
        }

        // "cert-manager.io_Certificate" → "cert-manager.io/Certificate"
        overrideKey, err := convertToOverrideKey(parts[3])

        customizationType := parts[2]
        switch customizationType {
        case "health":
            overrideVal.HealthLua = v
        case "useOpenLibs":
            overrideVal.UseOpenLibs, _ = strconv.ParseBool(v)
        case "actions":
            overrideVal.Actions = v
        case "ignoreDifferences":
            yaml.Unmarshal([]byte(v), &overrideVal.IgnoreDifferences)
        case "ignoreResourceUpdates":
            yaml.Unmarshal([]byte(v), &overrideVal.IgnoreResourceUpdates)
        case "knownTypeFields":
            yaml.Unmarshal([]byte(v), &overrideVal.KnownTypeFields)
        }
    }
    return nil
}
```

### 기본 Resource Status 무시

Argo CD는 기본적으로 모든 리소스의 `.status` 필드 차이를 무시한다. 이는 `ArgoCDDiffOptions`의 기본값에서 확인할 수 있다:

```go
// util/settings/settings.go L.1167-1169
func GetDefaultDiffOptions() ArgoCDDiffOptions {
    return ArgoCDDiffOptions{
        IgnoreAggregatedRoles: false,
        IgnoreResourceStatusField: IgnoreResourceStatusInAll,  // 기본: 모든 리소스 status 무시
        IgnoreDifferencesOnResourceUpdates: true,
    }
}
```

세 가지 status 무시 방식:

| 값 | 동작 |
|----|------|
| `all` | 모든 리소스의 status 무시 (기본) |
| `crd` | CRD의 status만 무시 |
| `none` | status 무시 안 함 |

### 내장 커스터마이제이션

Argo CD 소스코드에는 널리 사용되는 K8s 리소스에 대한 내장 Lua 커스터마이제이션이 포함되어 있다. 이는 `resource_customizations/` 디렉토리에 위치한다.

```
resource_customizations/
├── apps/
│   └── Deployment/
│       └── health.lua
├── batch/
│   └── Job/
│       └── health.lua
├── cert-manager.io/
│   └── Certificate/
│       ├── health.lua
│       └── testdata/
└── ...
```

---

## 12. 초기 비밀번호 생성

Argo CD 첫 배포 시 `InitializeSettings` 함수가 admin 계정의 초기 비밀번호를 자동 생성한다.

### 생성 로직

```go
// util/settings/settings.go L.2151-2188
func (mgr *SettingsManager) InitializeSettings(insecureModeEnabled bool) (*ArgoCDSettings, error) {
    const letters = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-"

    err := mgr.UpdateAccount(common.ArgoCDAdminUsername, func(adminAccount *Account) error {
        if adminAccount.Enabled {
            now := time.Now().UTC()
            if adminAccount.PasswordHash == "" {
                // 16자리 랜덤 비밀번호 생성
                randBytes := make([]byte, initialPasswordLength)  // 16
                for i := range initialPasswordLength {
                    num, err := rand.Int(rand.Reader, big.NewInt(int64(len(letters))))
                    randBytes[i] = letters[num.Int64()]
                }
                initialPassword := string(randBytes)

                // bcrypt로 해싱
                hashedPassword, err := password.HashPassword(initialPassword)

                // argocd-initial-admin-secret에 평문 저장
                ku := kube.NewKubeUtil(mgr.ctx, mgr.clientset)
                err = ku.CreateOrUpdateSecretField(mgr.namespace,
                    initialPasswordSecretName,    // "argocd-initial-admin-secret"
                    initialPasswordSecretField,   // "password"
                    initialPassword)

                adminAccount.PasswordHash = hashedPassword
            }
        }
        return nil
    })
    // ...

    // JWT 서명 키가 없으면 생성
    if cdSettings.ServerSignature == nil {
        signature, err := util.MakeSignature(32)  // 32바이트 랜덤
        cdSettings.ServerSignature = signature
    }

    // TLS 인증서가 없고 insecure 모드가 아니면 자체 서명 인증서 생성
    if cdSettings.Certificate == nil && !insecureModeEnabled {
        hosts := []string{
            "localhost",
            "argocd-server",
            "argocd-server." + mgr.namespace,
            fmt.Sprintf("argocd-server.%s.svc", mgr.namespace),
            fmt.Sprintf("argocd-server.%s.svc.cluster.local", mgr.namespace),
        }
        certOpts := tlsutil.CertOptions{Hosts: hosts, Organization: "Argo CD", IsCA: false}
        cert, err := tlsutil.GenerateX509KeyPair(certOpts)
        cdSettings.Certificate = cert
    }
    // ...
}
```

### 관련 상수

```go
// util/settings/settings.go L.518-526
const (
    // 초기 admin 비밀번호를 저장하는 Secret 이름
    initialPasswordSecretName = "argocd-initial-admin-secret"

    // Secret 내 비밀번호 필드 이름
    initialPasswordSecretField = "password"

    // 생성되는 초기 비밀번호 길이 (16자)
    initialPasswordLength = 16

    // 외부 TLS 인증서를 저장하는 Secret 이름
    externalServerTLSSecretName = "argocd-server-tls"
)
```

### 초기 비밀번호 조회 방법

```bash
# 초기 비밀번호 확인
kubectl get secret argocd-initial-admin-secret \
  -n argocd \
  -o jsonpath='{.data.password}' | base64 -d

# 로그인 후 비밀번호 변경 필수
argocd account update-password
```

비밀번호 변경 후에도 `argocd-initial-admin-secret`은 자동으로 삭제되지 않으므로, 보안을 위해 수동으로 삭제해야 한다.

---

## 13. 왜 이런 설계인가

### K8s Native: 별도 DB 없는 이유

Argo CD가 별도 데이터베이스(PostgreSQL 등)를 사용하지 않고 K8s Secret/ConfigMap을 영구 저장소로 활용하는 이유:

1. **배포 단순화**: 외부 DB 없이 K8s 클러스터만 있으면 동작한다. StatefulSet, PVC, DB 연결 설정이 필요 없다.
2. **K8s 네이티브 보안**: Secret은 etcd에서 at-rest 암호화를 지원하며, RBAC로 접근 제어가 가능하다.
3. **자연스러운 변경 감지**: K8s informer 메커니즘을 그대로 활용하여 설정 변경을 실시간으로 감지할 수 있다.
4. **운영 도구 활용**: `kubectl get secret`, `kubectl edit configmap` 등 익숙한 K8s 도구로 설정을 관리할 수 있다.

### FNV-32a 해시: 선택 이유

Secret 이름 생성에 FNV-32a 해시 알고리즘을 선택한 이유:

1. **속도**: MD5, SHA1 대비 훨씬 빠르다. 암호학적 강도가 필요 없는 이름 생성 용도에 적합하다.
2. **충돌 가능성**: 32비트 해시로 약 40억 개의 고유 값을 생성할 수 있다. 수천 개의 클러스터/레포를 관리하는 Argo CD 환경에서 충돌 가능성이 극히 낮다.
3. **디버깅 가능성**: Secret 이름에 호스트명(`cluster-{host}-{hash}`)을 포함시켜 사람이 읽을 수 있는 형태를 유지한다.

```go
// FNV-32a 동작 방식:
h := fnv.New32a()
h.Write([]byte("https://k8s.example.com"))
hash := h.Sum32()  // 예: 2847562398
// Secret 이름: "cluster-k8s.example.com-2847562398"
```

### 크리덴셜 템플릿: 대규모 레포 관리

수백 개의 레포지토리가 동일 Git 서버에 있을 때, 각각 크리덴셜을 저장하면 Secret이 폭발적으로 증가한다. URL prefix 기반 크리덴셜 템플릿으로 이를 해결한다:

```
크리덴셜 템플릿 1개 (creds-NNNN):
  url: https://github.com/myorg
  username: git
  token: ghp_xxxxx

자동으로 매칭되는 레포 (Secret 불필요):
  https://github.com/myorg/app-a.git
  https://github.com/myorg/app-b.git
  https://github.com/myorg/app-c.git
  ... (수백 개)
```

### 인덱서: O(1) 조회

informer의 기본 조회는 O(n) 선형 탐색이다. 클러스터나 레포가 수백 개일 때 매 조회마다 전체를 순회하면 성능 문제가 생긴다. Secret 인덱서를 통해 특정 필드로 O(1) 해시맵 조회가 가능하다:

```
인덱서 없는 경우:
  GetCluster("https://k8s.example.com")
      → 모든 cluster Secret 순회 O(n)
      → 각 Secret.Data["server"] 비교

인덱서 활용:
  GetCluster("https://k8s.example.com")
      → indexer.ByIndex("byClusterURL", "https://k8s.example.com")
      → 해시맵 조회 O(1)
      → 결과 Secret 반환
```

### 구독-발행 패턴: 설정 변경 전파

설정이 변경될 때마다 모든 컴포넌트가 K8s API를 polling하면 API 서버에 부하가 생긴다. 대신 각 컴포넌트가 SettingsManager에 채널을 구독(Subscribe)하고, 변경 시 채널을 통해 새 설정을 받는다.

```
구독 방식의 장점:
1. API 서버 polling 없음 — informer 이벤트 기반 push 방식
2. 컴포넌트 분리 — SettingsManager가 구독자 목록만 관리
3. 데드락 방지 — 별도 goroutine에서 채널 전송

// 실제 코드: 데드락 방지를 위한 goroutine 분리
go func() {
    for _, sub := range subscribers {
        sub <- newSettings  // 블로킹 채널 전송
    }
}()
```

---

## 전체 설정 관리 흐름 다이어그램

```
초기화 흐름:
=============
Argo CD 기동
    |
    +-- SettingsManager 생성 (NewSettingsManager)
    |       |
    |       +-- ensureSynced() → initialize()
    |               |
    |               +-- ConfigMap informer 시작
    |               +-- Secret informer 시작
    |               +-- ClusterInformer 시작
    |               +-- WaitForCacheSync()
    |
    +-- InitializeSettings()
            |
            +-- admin 비밀번호 없으면 생성 → argocd-initial-admin-secret
            +-- JWT 서명 키 없으면 생성  → argocd-secret.server.secretkey
            +-- TLS 인증서 없으면 생성  → argocd-secret.tls.crt/key

런타임 설정 조회:
==================
컴포넌트 → GetSettings()
               |
               +-- getConfigMap()  → informer 캐시에서 argocd-cm 조회
               +-- getSecret()     → informer 캐시에서 argocd-secret 조회
               +-- getSecrets()    → part-of=argocd 레이블 Secret 전체
               |
               +-- updateSettingsFromSecret()
               +-- updateSettingsFromConfigMap()
               |
               --> *ArgoCDSettings 반환

설정 변경 전파:
================
관리자가 argocd-cm 수정
    |
    +-- K8s API 서버 → informer 이벤트
    |
    +-- UpdateFunc 핸들러 발동
    |
    +-- tryNotify()
        |
        +-- GetSettings()  -- 새 설정 읽기
        |
        +-- notifySubscribers(newSettings)
            |
            goroutine:
            +-- API 서버 채널 ← newSettings  (OIDC 재설정)
            +-- App Controller 채널 ← newSettings (설정 재적용)
```

---

## 참고 소스 파일

| 파일 | 역할 |
|------|------|
| `/Users/ywlee/CNCF/argo-cd/util/settings/settings.go` | ArgoCDSettings, SettingsManager, 상수 정의 |
| `/Users/ywlee/CNCF/argo-cd/util/db/db.go` | ArgoDB 인터페이스, db 구현체 |
| `/Users/ywlee/CNCF/argo-cd/util/db/cluster.go` | 클러스터 CRUD, clusterToSecret, SecretToCluster |
| `/Users/ywlee/CNCF/argo-cd/util/db/repository.go` | 레포지토리 CRUD, enrichCredsToRepo |
| `/Users/ywlee/CNCF/argo-cd/util/db/secrets.go` | URIToSecretName, addSecretMetadata |
| `/Users/ywlee/CNCF/argo-cd/util/db/repository_secrets.go` | RepoURLToSecretName, Secret 기반 레포 백엔드 |
