# Argo CD Git 통합 Deep-Dive

## 목차

1. [개요: Git이 Argo CD의 중심인 이유](#개요)
2. [Git Client 인터페이스 (`util/git/client.go`)](#git-client-인터페이스)
3. [nativeGitClient: git CLI 래핑 구현](#nativegitclient-구현)
4. [Credential 관리 (`util/git/creds.go`)](#credential-관리)
5. [argocd-git-ask-pass: 안전한 자격증명 전달](#argocd-git-ask-pass)
6. [Repository 저장소 (`util/db/repository.go`)](#repository-저장소)
7. [RepoURLToSecretName: FNV-32a 해시 기반 네이밍](#repourlttosecretname)
8. [Credential 템플릿과 enrichCredsToRepo](#credential-템플릿)
9. [Git 작업 흐름: Init → Fetch → Checkout](#git-작업-흐름)
10. [repositoryLock: 동시성 제어](#repositorylock)
11. [gitRepoPaths: TempPaths 체크아웃 경로 관리](#gitrepopaths)
12. [Webhook 처리 (`util/webhook/webhook.go`)](#webhook-처리)
13. [manifest-generate-paths 최적화](#manifest-generate-paths)
14. [GPG 서명 검증](#gpg-서명-검증)
15. [Git 프록시 지원](#git-프록시-지원)
16. [Hydration: commitserver와 Git Notes](#hydration-commitserver)
17. [왜 이런 설계인가](#왜-이런-설계인가)
18. [전체 흐름 요약](#전체-흐름-요약)

---

## 개요

Argo CD는 GitOps의 핵심 원칙인 "Git이 단일 진실의 원천(Single Source of Truth)"을 구현한다. 모든 애플리케이션의 desired state는 Git 리포지토리에 저장되고, Argo CD는 주기적으로 Git을 폴링하거나 Webhook을 받아서 실제 상태와 비교한다.

```
[Git Repository] ←→ [Argo CD Repo Server] ←→ [Kubernetes Cluster]
     (Desired)           (GitOps Engine)           (Actual)
```

Git 통합은 크게 다섯 가지 영역으로 구성된다:

| 영역 | 위치 | 역할 |
|------|------|------|
| Git Client | `util/git/client.go` | git 명령 실행, 인터페이스 정의 |
| Credential | `util/git/creds.go`, `util/askpass/` | 자격증명 관리 및 안전한 전달 |
| Repository DB | `util/db/repository*.go` | K8s Secret 기반 설정 저장 |
| Webhook | `util/webhook/webhook.go` | 푸시 이벤트 수신 및 앱 갱신 |
| Hydration | `commitserver/commit/` | 렌더링 결과를 Git에 커밋 |

---

## Git Client 인터페이스

### 소스: `util/git/client.go`

`Client` 인터페이스는 Argo CD가 Git 리포지토리와 상호작용하는 모든 작업을 추상화한다. 실제 소스코드에서 확인한 전체 인터페이스 정의는 다음과 같다:

```go
// Client is a generic git client interface
type Client interface {
    Root() string
    Init() error
    Fetch(revision string, depth int64) error
    Submodule() error
    Checkout(revision string, submoduleEnabled bool) (string, error)
    LsRefs() (*Refs, error)
    LsRemote(revision string) (string, error)
    LsFiles(path string, enableNewGitFileGlobbing bool) ([]string, error)
    LsLargeFiles() ([]string, error)
    CommitSHA() (string, error)
    RevisionMetadata(revision string) (*RevisionMetadata, error)
    VerifyCommitSignature(string) (string, error)
    IsAnnotatedTag(string) bool
    ChangedFiles(revision string, targetRevision string) ([]string, error)
    IsRevisionPresent(revision string) bool
    SetAuthor(name, email string) (string, error)
    CheckoutOrOrphan(branch string, submoduleEnabled bool) (string, error)
    CheckoutOrNew(branch, base string, submoduleEnabled bool) (string, error)
    RemoveContents(paths []string) (string, error)
    CommitAndPush(branch, message string) (string, error)
    GetCommitNote(sha string, namespace string) (string, error)
    AddAndPushNote(sha string, namespace string, note string) error
    HasFileChanged(filePath string) (bool, error)
}
```

**각 메서드의 역할:**

| 메서드 | 역할 |
|--------|------|
| `Root()` | 로컬 체크아웃 경로 반환 |
| `Init()` | git init + remote origin 설정 |
| `Fetch()` | origin에서 최신 업데이트 가져오기 (LFS 포함) |
| `Submodule()` | 서브모듈 동기화 및 업데이트 |
| `Checkout()` | 특정 리비전 체크아웃 |
| `LsRefs()` | 원격 리포의 브랜치/태그 목록 반환 |
| `LsRemote()` | 브랜치/태그/SHA를 실제 커밋 SHA로 해석 |
| `CommitSHA()` | HEAD의 커밋 SHA 반환 |
| `RevisionMetadata()` | 커밋 작성자, 날짜, 메시지, 태그 반환 |
| `VerifyCommitSignature()` | GPG 서명 검증 |
| `ChangedFiles()` | 두 리비전 간 변경된 파일 목록 |
| `CommitAndPush()` | 변경사항 커밋 후 푸시 (Hydration용) |
| `GetCommitNote()` | git notes에서 메타데이터 조회 |
| `AddAndPushNote()` | git notes에 메타데이터 추가 후 푸시 |

### go-git 사용 범위

주목할 점은 Argo CD가 go-git 라이브러리를 제한적으로만 사용한다는 것이다:

```go
// go-git은 LsRemote (원격 refs 조회)에서만 사용
repo, err := git.Init(memory.NewStorage(), nil)
remote, err := repo.CreateRemote(&config.RemoteConfig{
    Name: git.DefaultRemoteName,
    URLs: []string{m.repoURL},
})
res, err := listRemote(remote, &git.ListOptions{Auth: auth}, ...)
```

`getRefs()` 함수는 go-git의 in-memory storage를 이용해 원격 refs를 조회한다. 로컬 클론이 없어도 동작하며, 동시 실행이 안전하다. 반면 Checkout, Fetch 같은 실제 파일시스템 작업은 모두 git CLI(`exec.CommandContext`)를 직접 호출한다.

---

## nativeGitClient 구현

### 소스: `util/git/client.go`

`nativeGitClient`는 `Client` 인터페이스의 실제 구현체다:

```go
type nativeGitClient struct {
    EventHandlers

    // URL of the repository
    repoURL string
    // Root path of repository
    root string
    // Authenticator credentials for private repositories
    creds Creds
    // Whether to connect insecurely to repository
    insecure bool
    // Whether the repository is LFS enabled
    enableLfs bool
    // gitRefCache knows how to cache git refs
    gitRefCache gitRefCache
    // indicates if client allowed to load refs from cache
    loadRefFromCache bool
    // HTTP/HTTPS proxy used to access repository
    proxy string
    // list of targets that shouldn't use the proxy
    noProxy string
    // git configuration environment variables
    gitConfigEnv []string
}
```

### 클라이언트 생성

```go
func NewClient(rawRepoURL string, creds Creds, insecure bool, enableLfs bool,
    proxy string, noProxy string, opts ...ClientOpts) (Client, error) {
    r := regexp.MustCompile(`([/:])`)
    normalizedGitURL := NormalizeGitURL(rawRepoURL)
    // URL 경로 구분자를 _로 변환하여 디렉토리 경로 생성
    // 예: github.com/org/repo → /tmp/github.com_org_repo
    root := filepath.Join(os.TempDir(), r.ReplaceAllString(normalizedGitURL, "_"))
    return NewClientExt(rawRepoURL, root, creds, insecure, enableLfs, proxy, noProxy, opts...)
}
```

### 핵심 환경변수 설정

`runCmdOutput`에서 git CLI를 실행할 때 여러 환경변수를 명시적으로 설정한다:

```go
func (m *nativeGitClient) runCmdOutput(cmd *exec.Cmd, ropts runOpts) (string, error) {
    cmd.Dir = m.root
    cmd.Env = append(os.Environ(), cmd.Env...)
    // 외부 SSH 키(~/.ssh) 등 사용 방지 - 보안상 이유
    cmd.Env = append(cmd.Env, "HOME=/dev/null")
    // 대부분의 git 작업에서 LFS 스킵 (명시적 요청 시만 처리)
    cmd.Env = append(cmd.Env, "GIT_LFS_SKIP_SMUDGE=1")
    // git 터미널 프롬프트 비활성화 (인터랙티브 입력 방지)
    cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=false")
    // ArgoCD 내장 git 설정 적용
    cmd.Env = append(cmd.Env, m.gitConfigEnv...)
    // 프록시 설정 적용
    cmd.Env = proxy.UpsertEnv(cmd, m.proxy, m.noProxy)
    ...
}
```

### 내장 Git 설정 (builtinGitConfig)

```go
var builtinGitConfig = map[string]string{
    "maintenance.autoDetach": "false",
    "gc.autoDetach":          "false",
}
```

`gc.autoDetach=false`는 git gc가 백그라운드로 실행되어 프로세스가 유실되는 문제를 방지한다. 이 설정은 `GIT_CONFIG_KEY_*` / `GIT_CONFIG_VALUE_*` 환경변수를 통해 전달된다.

### Init() 구현

```go
func (m *nativeGitClient) Init() error {
    // 이미 초기화된 리포지토리이면 스킵
    _, err := git.PlainOpen(m.root)
    if err == nil {
        return nil
    }
    if !errors.Is(err, git.ErrRepositoryNotExists) {
        return err
    }
    // 기존 디렉토리 제거 후 재생성
    err = os.RemoveAll(m.root)
    err = os.MkdirAll(m.root, 0o755)
    repo, err := git.PlainInit(m.root, false)
    // remote origin 설정
    _, err = repo.CreateRemote(&config.RemoteConfig{
        Name: git.DefaultRemoteName,
        URLs: []string{m.repoURL},
    })
    return err
}
```

### Fetch() 구현

```go
func (m *nativeGitClient) fetch(ctx context.Context, revision string, depth int64) error {
    args := []string{"fetch", "origin"}
    if revision != "" {
        args = append(args, revision)
    }
    if depth > 0 {
        args = append(args, "--depth", strconv.FormatInt(depth, 10))
    } else {
        args = append(args, "--tags")
    }
    args = append(args, "--force", "--prune")
    return m.runCredentialedCmd(ctx, args...)
}

func (m *nativeGitClient) Fetch(revision string, depth int64) error {
    // 이벤트 핸들러 호출 (메트릭 측정용)
    if m.OnFetch != nil {
        done := m.OnFetch(m.repoURL)
        defer done()
    }
    err := m.fetch(ctx, revision, depth)
    // LFS 처리
    if err == nil && m.IsLFSEnabled() {
        largeFiles, err := m.LsLargeFiles()
        if err == nil && len(largeFiles) > 0 {
            err = m.runCredentialedCmd(ctx, "lfs", "fetch", "--all")
        }
    }
    return err
}
```

### Checkout() 구현

```go
func (m *nativeGitClient) Checkout(revision string, submoduleEnabled bool) (string, error) {
    if revision == "" || revision == "HEAD" {
        revision = "origin/HEAD"
    }
    if out, err := m.runCmd(ctx, "checkout", "--force", revision); err != nil {
        return out, fmt.Errorf("failed to checkout %s: %w", revision, err)
    }
    // LFS 파일 체크아웃
    if m.IsLFSEnabled() {
        largeFiles, err := m.LsLargeFiles()
        if len(largeFiles) > 0 {
            m.runCmd(ctx, "lfs", "checkout")
        }
    }
    // 서브모듈 처리
    if _, err := os.Stat(m.root + "/.gitmodules"); !os.IsNotExist(err) {
        if submoduleEnabled {
            m.Submodule()
        }
    }
    // 추적되지 않는 파일/디렉토리 제거 (-ffdx: 중첩 git 리포 포함)
    m.runCmd(ctx, "clean", "-ffdx")
    return "", nil
}
```

`-ffdx`에서 두 번의 `f`는 의도적이다: 첫 번째 `f`는 추적되지 않은 파일/디렉토리를 삭제하고, 두 번째 `f`는 추적되지 않은 중첩 git 리포지토리(예: 제거된 서브모듈)도 삭제한다.

---

## Credential 관리

### 소스: `util/git/creds.go`

Argo CD는 다양한 Git 인증 방식을 지원한다. 모든 자격증명 유형은 `Creds` 인터페이스를 구현한다:

```go
type Creds interface {
    Environ() (io.Closer, []string, error)
    // GetUserInfo gets the username and email address for the credentials
    GetUserInfo(ctx context.Context) (string, string, error)
}
```

`Environ()`은 자격증명을 환경변수 목록으로 반환하고, 사용 후 정리해야 할 리소스(임시 파일 등)를 `io.Closer`로 반환한다.

### 지원하는 자격증명 유형

```
Creds (인터페이스)
├── NopCreds              # 공개 리포지토리 (인증 없음)
├── HTTPSCreds            # HTTPS 사용자명/비밀번호, Bearer Token
├── SSHCreds              # SSH 개인키
├── GitHubAppCreds        # GitHub App (앱 ID + 설치 ID + 개인키)
├── GoogleCloudCreds      # Google Cloud Source Repositories
└── AzureWorkloadIdentityCreds  # Azure DevOps (Workload Identity)
```

### HTTPSCreds

```go
type HTTPSCreds struct {
    username       string
    password       string
    bearerToken    string
    insecure       bool
    clientCertData string   // mTLS 클라이언트 인증서
    clientCertKey  string   // mTLS 클라이언트 키
    store          CredsStore
    forceBasicAuth bool
}
```

`Environ()` 구현에서 주목할 점:

```go
func (creds HTTPSCreds) Environ() (io.Closer, []string, error) {
    var env []string

    // TLS 검증 무시 설정
    if creds.insecure {
        env = append(env, "GIT_SSL_NO_VERIFY=true")
    }

    // mTLS 클라이언트 인증서 - 임시 파일에 저장
    if creds.HasClientCert() {
        certFile, _ := os.CreateTemp(argoio.TempDir, "")
        keyFile, _ := os.CreateTemp(argoio.TempDir, "")
        certFile.WriteString(creds.clientCertData)
        keyFile.WriteString(creds.clientCertKey)
        env = append(env, "GIT_SSL_CERT="+certFile.Name())
        env = append(env, "GIT_SSL_KEY="+keyFile.Name())
    }

    // Basic Auth 강제 사용 (인증 협상 스킵)
    if creds.password != "" && creds.forceBasicAuth {
        env = append(env, fmt.Sprintf("%s=%s", forceBasicAuthHeaderEnv, creds.BasicAuthHeader()))
    } else if creds.bearerToken != "" {
        env = append(env, fmt.Sprintf("%s=%s", bearerAuthHeaderEnv, creds.BearerAuthHeader()))
    }

    // CredsStore를 통해 GIT_ASKPASS 방식으로 자격증명 제공
    nonce := creds.store.Add(username, creds.password)
    env = append(env, creds.store.Environ(nonce)...)
    return closer, env, nil
}
```

### SSHCreds

```go
func (c SSHCreds) Environ() (io.Closer, []string, error) {
    // SSH 개인키를 임시 파일에 저장 (SHM 기반 /dev/shm 사용으로 보안 강화)
    file, _ := os.CreateTemp(argoio.TempDir, "")
    file.WriteString(c.sshPrivateKey + "\n")

    // GIT_SSH_COMMAND로 SSH 옵션 전달
    args := []string{"ssh", "-i", file.Name()}
    if c.insecure {
        args = append(args, "-o", "StrictHostKeyChecking=no",
            "-o", "UserKnownHostsFile=/dev/null")
    } else {
        knownHostsFile := certutil.GetSSHKnownHostsDataPath()
        args = append(args, "-o", "StrictHostKeyChecking=yes",
            "-o", "UserKnownHostsFile="+knownHostsFile)
    }

    // SOCKS5 프록시 처리
    if c.proxy != "" {
        parsedProxyURL, _ := url.Parse(c.proxy)
        args = append(args, "-o", fmt.Sprintf(
            "ProxyCommand='connect-proxy -S %s:%s -5 %%h %%p'",
            parsedProxyURL.Hostname(), parsedProxyURL.Port()))
    }

    env = append(env, "GIT_SSH_COMMAND="+strings.Join(args, " "))
    return sshCloser, env, nil
}
```

### GitHubAppCreds

GitHub App은 앱 ID와 설치 ID로 인증하며, 단기 토큰(Installation Access Token)을 발급받는다:

```go
type GitHubAppCreds struct {
    appID          int64
    appInstallId   int64   // 0이면 자동 탐지
    privateKey     string
    baseURL        string  // GitHub Enterprise용
    clientCertData string
    clientCertKey  string
    insecure       bool
    proxy          string
    noProxy        string
    store          CredsStore
    repoURL        string  // 설치 ID 자동 탐지용
}
```

토큰 캐시 메커니즘:

```go
func (g GitHubAppCreds) getInstallationTransport() (*ghinstallation.Transport, error) {
    // SHA-256 해시로 캐시 키 생성
    h := sha256.New()
    fmt.Fprintf(h, "%s %d %d %s", g.privateKey, g.appID, installationID, g.baseURL)
    key := hex.EncodeToString(h.Sum(nil))

    // 캐시에서 transport 조회 (60분 TTL)
    t, found := githubAppTokenCache.Get(key)
    if found {
        itr := t.(*ghinstallation.Transport)
        return itr, nil
    }

    // 새 transport 생성 및 캐시 저장
    itr, _ := ghinstallation.New(c.Transport, g.appID, installationID, []byte(g.privateKey))
    githubAppTokenCache.Set(key, itr, time.Minute*60)
    return itr, nil
}
```

`appInstallId`가 0이면 `DiscoverGitHubAppInstallationID()`를 호출하여 GitHub API에서 자동으로 설치 ID를 탐지한다:

```go
func DiscoverGitHubAppInstallationID(ctx context.Context, appId int64,
    privateKey, enterpriseBaseURL, org string, ...) (int64, error) {
    // 동시 API 호출 방지를 위한 뮤텍스
    githubInstallationIdCacheMutex.Lock()
    defer githubInstallationIdCacheMutex.Unlock()

    // GitHub API로 설치 목록 조회 (페이지네이션)
    for {
        installations, resp, _ := client.Apps.ListInstallations(ctx, opts)
        allInstallations = append(allInstallations, installations...)
        if resp.NextPage == 0 {
            break
        }
        opts.Page = resp.NextPage
    }

    // 모든 설치 정보를 캐시 (60분 TTL)
    for _, installation := range allInstallations {
        githubInstallationIdCache.Set(cacheKey, *installation.ID, ...)
    }
}
```

---

## argocd-git-ask-pass

### 소스: `util/askpass/server.go`

`argocd-git-ask-pass`는 Git 자격증명을 안전하게 전달하는 메커니즘이다. git 프로세스의 환경변수에 직접 비밀번호를 넣는 대신, Unix 소켓을 통한 gRPC로 자격증명을 조회한다.

```
┌─────────────────────────────────────────────────────┐
│                    Repo Server                       │
│                                                      │
│  ┌──────────────┐    ┌──────────────────────────┐   │
│  │  ask-pass    │    │    git fetch/push         │   │
│  │  gRPC Server │    │                           │   │
│  │  (Unix socket│◄───│  GIT_ASKPASS=argocd       │   │
│  │   /tmp/...)  │    │  ARGOCD_GIT_ASKPASS_NONCE │   │
│  └──────────────┘    │  =<uuid>                  │   │
│         │            │  AKSPASS_SOCKET_PATH=...  │   │
│         │            └──────────────────────────┘   │
│         ▼                                            │
│  creds[uuid] = {username, password}                  │
└─────────────────────────────────────────────────────┘
```

동작 방식:

```go
// 1. 자격증명 등록 (Add)
func (s *server) Add(username string, password string) string {
    id := uuid.New().String()
    s.creds[id] = Creds{Username: username, Password: password}
    return id  // nonce 반환
}

// 2. git 실행 시 환경변수 설정 (Environ)
func (s *server) Environ(id string) []string {
    return []string{
        "GIT_ASKPASS=argocd",                              // git이 호출할 binary
        fmt.Sprintf("%s=%s", ASKPASS_NONCE_ENV, id),       // ARGOCD_GIT_ASKPASS_NONCE=<uuid>
        "GIT_TERMINAL_PROMPT=0",                           // 터미널 프롬프트 비활성화
        "ARGOCD_BINARY_NAME=argocd-git-ask-pass",          // argocd binary 모드 전환
        fmt.Sprintf("%s=%s", AKSPASS_SOCKET_PATH_ENV, s.socketPath), // 소켓 경로
    }
}

// 3. git이 자격증명 요청 시 gRPC 호출 (GetCredentials)
func (s *server) GetCredentials(_ context.Context, q *CredentialsRequest) (*CredentialsResponse, error) {
    creds, ok := s.getCreds(q.Nonce)  // nonce로 자격증명 조회
    return &CredentialsResponse{Username: creds.Username, Password: creds.Password}, nil
}
```

**왜 이 방식인가:** Kustomize 등 외부 도구가 git을 호출할 때 환경변수를 매니페스트에 노출시키는 버그가 과거에 있었다. nonce만 환경변수에 노출되므로, nonce가 유출되더라도 서버 접근 권한이 없으면 실제 자격증명을 획득할 수 없다.

---

## Repository 저장소

### 소스: `util/db/repository.go`, `util/db/repository_secrets.go`

Argo CD는 리포지토리 설정을 Kubernetes Secret에 저장한다. Secret의 label `argocd.argoproj.io/secret-type: repository`로 식별한다.

### Secret 구조

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: repo-1234567890          # RepoURLToSecretName()으로 생성
  namespace: argocd
  labels:
    argocd.argoproj.io/secret-type: repository
type: Opaque
data:
  url: aHR0cHM6Ly9naXRodWIuY29t...    # base64 인코딩된 URL
  username: dXNlcm5hbWU=
  password: cGFzc3dvcmQ=
  sshPrivateKey: LS0tLS1CRUdJTi...
  githubAppID: MTIzNDU2
  githubAppInstallationID: Nzg5MDEy
  githubAppPrivateKey: LS0tLS1...
  bearerToken: dG9rZW4...
  tlsClientCertData: LS0tLS1...
  tlsClientCertKey: LS0tLS1...
  type: git                      # git 또는 helm
  insecure: "false"
  insecureIgnoreHostKey: "false"
  enableLfs: "false"
  proxy: ""
  noProxy: ""
  project: ""                    # 프로젝트 범위 제한
  gcpServiceAccountKey: ...      # Google Cloud용
  depth: "0"                     # shallow clone 깊이
```

`secretToRepository()` 함수에서 Secret의 Data 필드를 `v1alpha1.Repository` 구조체로 변환한다:

```go
func secretToRepository(secret *corev1.Secret) (*appsv1.Repository, error) {
    secretCopy := secret.DeepCopy()
    repository := &appsv1.Repository{
        Name:                       string(secretCopy.Data["name"]),
        Repo:                       string(secretCopy.Data["url"]),
        Username:                   string(secretCopy.Data["username"]),
        Password:                   string(secretCopy.Data["password"]),
        BearerToken:                string(secretCopy.Data["bearerToken"]),
        SSHPrivateKey:              string(secretCopy.Data["sshPrivateKey"]),
        TLSClientCertData:          string(secretCopy.Data["tlsClientCertData"]),
        TLSClientCertKey:           string(secretCopy.Data["tlsClientCertKey"]),
        GithubAppPrivateKey:        string(secretCopy.Data["githubAppPrivateKey"]),
        GitHubAppEnterpriseBaseURL: string(secretCopy.Data["githubAppEnterpriseBaseUrl"]),
        Proxy:                      string(secretCopy.Data["proxy"]),
        NoProxy:                    string(secretCopy.Data["noProxy"]),
        GCPServiceAccountKey:       string(secretCopy.Data["gcpServiceAccountKey"]),
        ...
    }
    return repository, nil
}
```

---

## RepoURLToSecretName

### 소스: `util/db/repository.go:472`

```go
// RepoURLToSecretName hashes repo URL to a secret name using a formula.
// NOTE: this formula should not be considered stable and may change in future releases.
// Do NOT rely on this formula as a means of secret lookup, only secret creation.
func RepoURLToSecretName(prefix string, repo string, project string) string {
    h := fnv.New32a()
    _, _ = h.Write([]byte(repo))
    _, _ = h.Write([]byte(project))
    return fmt.Sprintf("%s-%v", prefix, h.Sum32())
}
```

**FNV-32a 알고리즘:**
- FNV(Fowler-Noll-Vo)는 비암호화 해시 알고리즘으로 속도가 빠름
- 32비트 결과로 Secret 이름 길이를 합리적으로 유지
- `prefix-{hash}` 형식: 예) `repo-2847382910`, `creds-1923847561`

**prefix 종류:**

| prefix | 상수 | 용도 |
|--------|------|------|
| `repo` | `repoSecretPrefix` | 읽기 전용 리포지토리 Secret |
| `repo-write` | `repoWriteSecretPrefix` | 쓰기 전용 리포지토리 Secret (Hydration용) |
| `creds` | `credSecretPrefix` | 자격증명 템플릿 Secret |
| `creds-write` | `credWriteSecretPrefix` | 쓰기 자격증명 템플릿 Secret |

---

## Credential 템플릿

### 소스: `util/db/repository.go:434`

자격증명 템플릿(RepoCreds)은 URL prefix를 기준으로 여러 리포지토리에 자격증명을 일괄 적용하는 기능이다.

```
┌──────────────────────────────────────────────────────────┐
│                  자격증명 상속 흐름                         │
│                                                           │
│  repo-creds Secret (prefix: https://github.com/myorg/)   │
│  ├── username: mybot                                      │
│  └── sshPrivateKey: -----BEGIN...                         │
│                          │                                │
│                          │ URL prefix 매칭               │
│                    ┌─────▼─────────────────────┐         │
│                    │  enrichCredsToRepo()       │         │
│                    └─────┬─────────────────────┘         │
│                    ┌─────▼──────────────────────────┐    │
│  App1 (github.com/myorg/repo-a) → 자격증명 상속 ✓   │    │
│  App2 (github.com/myorg/repo-b) → 자격증명 상속 ✓   │    │
│  App3 (gitlab.com/other/repo)   → 자격증명 없음     │    │
│                    └────────────────────────────────┘    │
└──────────────────────────────────────────────────────────┘
```

`enrichCredsToRepo()` 구현:

```go
func (db *db) enrichCredsToRepo(ctx context.Context, repository *v1alpha1.Repository) error {
    if !repository.HasCredentials() {
        // 해당 리포지토리에 자체 자격증명이 없으면
        // URL prefix가 매칭되는 RepoCreds를 찾아서 상속
        creds, err := db.GetRepositoryCredentials(ctx, repository.Repo)
        if creds != nil {
            repository.CopyCredentialsFrom(creds)
            repository.InheritedCreds = true  // 상속됨을 표시
        }
    }
    return nil
}
```

`GetRepositoryCredentials()`는 `getRepoCredsSecret()`을 호출하여 저장된 모든 `repo-creds` Secret을 순회하고, URL prefix가 가장 길게 매칭되는 항목을 반환한다.

---

## Git 작업 흐름

### Init → Fetch → Checkout 순서

```
Application Sync 요청
        │
        ▼
┌────────────────────────────────────────────────────────┐
│  runRepoOperation() - reposerver/repository/repository.go │
│                                                        │
│  1. gitClient.Init()        # git init + remote 설정   │
│         │                                              │
│  2. repoLock.Lock()         # 동일 리포 직렬화         │
│         │                                              │
│  3. checkoutRevision()      # Fetch + Checkout         │
│     ├── gitClient.Fetch()   # git fetch origin ...     │
│     └── gitClient.Checkout()# git checkout --force ...  │
│         │                                              │
│  4. GenerateManifests()     # 매니페스트 생성          │
│         │                                              │
│  5. Lock 해제               # 다음 요청 처리 가능      │
└────────────────────────────────────────────────────────┘
```

`checkoutRevision()` 함수:

```go
func (s *Service) checkoutRevision(gitClient git.Client, revision string,
    submoduleEnabled bool, depth int64) (goio.Closer, error) {
    err := gitClient.Init()
    if err != nil {
        return nil, fmt.Errorf("failed to initialize git repo: %w", err)
    }
    err = gitClient.Fetch(revision, depth)
    if err != nil {
        return nil, status.Errorf(codes.Internal, "failed to fetch: %v", err)
    }
    _, err = gitClient.Checkout(revision, submoduleEnabled)
    if err != nil {
        return nil, status.Errorf(codes.Internal, "failed to checkout: %v", err)
    }
    return directoryPermissionInitializer(gitClient.Root()), nil
}
```

---

## repositoryLock

### 소스: `reposerver/repository/lock.go`

`repositoryLock`은 동일한 리포지토리에 대한 동시 접근을 제어한다:

```go
type repositoryLock struct {
    lock       sync.Mutex
    stateByKey map[string]*repositoryState
}

type repositoryState struct {
    cond            *sync.Cond
    revision        string
    initCloser      io.Closer
    processCount    int
    allowConcurrent bool
}
```

`Lock()` 동작 원리:

```go
func (r *repositoryLock) Lock(path string, revision string,
    allowConcurrent bool, init func() (io.Closer, error)) (io.Closer, error) {
    for {
        state.cond.L.Lock()
        if state.revision == "" {
            // 진행 중인 작업이 없음 → 즉시 시작
            initCloser, _ := init()
            state.revision = revision
            state.processCount = 1
            state.allowConcurrent = allowConcurrent
            state.cond.L.Unlock()
            return closer, nil
        } else if state.revision == revision && state.allowConcurrent && allowConcurrent {
            // 동일 revision, 동시 처리 허용 → 참여
            state.processCount++
            state.cond.L.Unlock()
            return closer, nil
        }
        // 대기
        state.cond.Wait()
        state.cond.L.Unlock()
    }
}
```

**동시성 정책:**

| 상황 | 결과 |
|------|------|
| 동일 리포, 동일 revision, `allowConcurrent=true` | 병렬 처리 허용 (processCount 증가) |
| 동일 리포, 다른 revision | 이전 작업 완료까지 대기 |
| 동일 리포, `allowConcurrent=false` | 항상 대기 |

`processCount`가 0이 되면 `initCloser.Close()`를 호출하고 `revision`을 초기화하여 대기 중인 요청에 알린다.

---

## gitRepoPaths

### 소스: `reposerver/repository/repository.go`

`gitRepoPaths`는 리포지토리 URL과 로컬 체크아웃 경로 간의 매핑을 관리한다:

```go
type Service struct {
    gitRepoPaths  utilio.TempPaths
    chartPaths    utilio.TempPaths
    ociPaths      utilio.TempPaths
    repoLock      *repositoryLock
    ...
}

// 서비스 생성 시 초기화
gitRandomizedPaths := utilio.NewRandomizedTempPaths(rootDir)
```

**서비스 재시작 시 경로 복원 (`Init()`):**

```go
func (s *Service) Init() error {
    dirEntries, _ := os.ReadDir(s.rootDir)
    for _, file := range dirEntries {
        if !file.IsDir() {
            continue
        }
        fullPath := filepath.Join(s.rootDir, file.Name())
        // 기존 git 리포지토리인지 확인
        if repo, err := gogit.PlainOpen(fullPath); err == nil {
            if remotes, _ := repo.Remotes(); len(remotes) > 0 {
                // URL → 경로 매핑 복원
                s.gitRepoPaths.Add(
                    git.NormalizeGitURL(remotes[0].Config().URLs[0]),
                    fullPath,
                )
            }
        }
    }
    return nil
}
```

**다중 소스 Helm chart에서의 사용:**

```go
// Helm value files가 다른 리포지토리를 참조할 때
func getResolvedRefValueFile(rawValueFile string, ..., refSourceRepo string,
    gitRepoPaths utilio.TempPaths) (pathutil.ResolvedFilePath, error) {
    repoPath := gitRepoPaths.GetPathIfExists(git.NormalizeGitURL(refSourceRepo))
    // 참조된 리포가 이미 체크아웃된 경우 해당 경로 재사용
}
```

---

## Webhook 처리

### 소스: `util/webhook/webhook.go`

Webhook 핸들러는 여러 Git 플랫폼의 푸시 이벤트를 처리한다.

### 지원 플랫폼

```go
type ArgoCDWebhookHandler struct {
    github          *github.Webhook
    gitlab          *gitlab.Webhook
    bitbucket       *bitbucket.Webhook
    bitbucketserver *bitbucketserver.Webhook
    azuredevops     *azuredevops.Webhook
    gogs            *gogs.Webhook
    queue           chan any           // 비동기 처리 큐 (50000 버퍼)
    ...
}
```

### 초기화 및 시크릿 검증

```go
func NewHandler(..., set *settings.ArgoCDSettings, ...) *ArgoCDWebhookHandler {
    // 각 플랫폼별 시크릿으로 Webhook 객체 초기화
    githubWebhook, _ := github.New(github.Options.Secret(set.GetWebhookGitHubSecret()))
    gitlabWebhook, _ := gitlab.New(gitlab.Options.Secret(set.GetWebhookGitLabSecret()))
    bitbucketWebhook, _ := bitbucket.New(bitbucket.Options.UUID(set.GetWebhookBitbucketUUID()))
    azuredevopsWebhook, _ := azuredevops.New(azuredevops.Options.BasicAuth(
        set.GetWebhookAzureDevOpsUsername(), set.GetWebhookAzureDevOpsPassword()))

    acdWebhook := ArgoCDWebhookHandler{...}
    acdWebhook.startWorkerPool(webhookParallelism)  // 워커 풀 시작
    return &acdWebhook
}
```

비동기 워커 풀:

```go
func (a *ArgoCDWebhookHandler) startWorkerPool(webhookParallelism int) {
    for range webhookParallelism {
        a.Go(func() {
            for {
                payload, ok := <-a.queue
                if !ok {
                    return
                }
                guard.RecoverAndLog(func() { a.HandleEvent(payload) }, ...)
            }
        })
    }
}
```

### affectedRevisionInfo(): 플랫폼별 페이로드 파싱

```go
func (a *ArgoCDWebhookHandler) affectedRevisionInfo(payloadIf any) (
    webURLs []string, revision string, change changeInfo,
    touchedHead bool, changedFiles []string) {

    switch payload := payloadIf.(type) {
    case github.PushPayload:
        webURLs = append(webURLs, payload.Repository.HTMLURL)
        revision = ParseRevision(payload.Ref)  // "refs/heads/main" → "main"
        change.shaAfter = ParseRevision(payload.After)
        change.shaBefore = ParseRevision(payload.Before)
        touchedHead = payload.Repository.DefaultBranch == revision
        for _, commit := range payload.Commits {
            changedFiles = append(changedFiles, commit.Added...)
            changedFiles = append(changedFiles, commit.Modified...)
            changedFiles = append(changedFiles, commit.Removed...)
        }
    case gitlab.PushEventPayload:
        // GitLab 페이로드 처리 (구조 유사)
        ...
    case azuredevops.GitPushEvent:
        // Azure DevOps는 변경 파일 목록 미제공
        webURLs = append(webURLs, payload.Resource.Repository.RemoteURL)
        ...
    }
}
```

### HandleEvent(): 앱 갱신 트리거

```go
func (a *ArgoCDWebhookHandler) HandleEvent(payload any) {
    webURLs, revision, change, touchedHead, changedFiles := a.affectedRevisionInfo(payload)

    // 모든 Application 목록 조회
    apps, _ := appIf.List(labels.Everything())

    for _, webURL := range webURLs {
        repoRegexp, _ := GetWebURLRegex(webURL)

        for _, app := range filteredApps {
            sources := app.Spec.GetSources()

            for _, source := range sources {
                if sourceRevisionHasChanged(source, revision, touchedHead) &&
                    sourceUsesURL(source, webURL, repoRegexp) {

                    refreshPaths := path.GetSourceRefreshPaths(&app, source)
                    if path.AppFilesHaveChanged(refreshPaths, changedFiles) {
                        // 변경된 파일이 refresh 경로와 겹치면 앱 갱신
                        argo.RefreshApp(namespacedAppInterface, app.Name,
                            v1alpha1.RefreshTypeNormal, hydrate)
                        break
                    } else if change.shaBefore != "" && change.shaAfter != "" {
                        // 파일 변경 없어도 캐시된 매니페스트 revision 키 업데이트
                        a.storePreviouslyCachedManifests(&app, change, ...)
                    }
                }
            }
        }
    }
}
```

### GetWebURLRegex(): URL 정규화 매칭

다양한 Git URL 형식(HTTPS, SSH, altssh 등)을 정규화하여 매칭한다:

```go
func GetWebURLRegex(webURL string) (*regexp.Regexp, error) {
    // 패턴: http(s)/ssh + 선택적 사용자명 + (alt)ssh 서브도메인 + 호스트 + 포트 + 경로 + .git
    return getURLRegex(webURL,
        `(?i)^((https?|ssh)://)?(%[1]s@)?((alt)?ssh\.)?%[2]s(:\d+)?[:/]%[3]s(\.git)?$`)
}
```

---

## manifest-generate-paths 최적화

### 소스: `util/app/path/path.go`, `pkg/apis/application/v1alpha1/application_annotations.go`

annotation 정의:
```go
AnnotationKeyManifestGeneratePaths = "argocd.argoproj.io/manifest-generate-paths"
```

이 annotation은 Webhook으로 받은 변경 파일 목록과 비교하여 불필요한 manifest 재생성을 건너뛰는 최적화를 제공한다.

### 사용 예

```yaml
metadata:
  annotations:
    argocd.argoproj.io/manifest-generate-paths: .;/shared/common
```

`;`로 구분된 경로 목록. 절대 경로(`/`로 시작)는 그대로, 상대 경로는 source.path 기준으로 해석된다.

### GetSourceRefreshPaths()

```go
func GetSourceRefreshPaths(app *v1alpha1.Application, source v1alpha1.ApplicationSource) []string {
    annotationPaths, hasAnnotation := app.Annotations[v1alpha1.AnnotationKeyManifestGeneratePaths]

    // Source Hydrator가 설정된 경우 sync source는 source.Path만 사용
    if app.Spec.SourceHydrator != nil {
        syncSource := app.Spec.SourceHydrator.GetSyncSource()
        if (source).Equals(&syncSource) {
            return []string{source.Path}
        }
    }

    var paths []string
    if hasAnnotation && annotationPaths != "" {
        for item := range strings.SplitSeq(annotationPaths, ";") {
            item = strings.TrimSpace(item)
            if item == "" {
                continue
            }
            if filepath.IsAbs(item) {
                paths = append(paths, item[1:])  // 절대 경로: 앞의 / 제거
            } else {
                // 상대 경로: source.Path와 결합
                paths = append(paths, filepath.Clean(filepath.Join(source.Path, item)))
            }
        }
    }
    return paths
}
```

### AppFilesHaveChanged()

```go
func AppFilesHaveChanged(refreshPaths []string, changedFiles []string) bool {
    // 변경 파일 목록 없음 → 항상 갱신 (안전한 기본값)
    if len(changedFiles) == 0 {
        return true
    }
    // refresh 경로 없음 → 항상 갱신 (annotation 없는 경우)
    if len(refreshPaths) == 0 {
        return true
    }
    // 변경 파일 중 하나라도 refresh 경로 아래에 있으면 갱신
    for _, f := range changedFiles {
        f = ensureAbsPath(f)
        for _, item := range refreshPaths {
            item = ensureAbsPath(item)
            if f == item {
                return true
            } else if _, err := security.EnforceToCurrentRoot(item, f); err == nil {
                return true  // f가 item 디렉토리 하위에 있음
            } else if matched, _ := filepath.Match(item, f); matched {
                return true  // glob 패턴 매칭
            }
        }
    }
    return false
}
```

**최적화 효과:**

```
상황 1: annotation 없음
  → changedFiles 상관없이 항상 refresh (기존 동작)

상황 2: annotation = "app/service"
  → app/service/ 아래 파일만 변경됐을 때만 refresh
  → app/ui/ 파일만 변경된 경우 스킵

상황 3: annotation = "."
  → source.path 아래 모든 파일 변경 시 refresh
  → 다른 경로의 변경은 무시
```

---

## GPG 서명 검증

### 소스: `util/gpg/gpg.go`, `util/git/client.go`

Argo CD는 GPG 서명 검증을 통해 신뢰할 수 있는 커밋만 배포할 수 있도록 한다.

### 활성화 확인

```go
// IsGPGEnabled returns true if GPG feature is enabled
func IsGPGEnabled() bool {
    if en := os.Getenv("ARGOCD_GPG_ENABLED"); strings.EqualFold(en, "false") ||
        strings.EqualFold(en, "no") {
        return false
    }
    return true
}
```

기본값은 활성화 상태이며, 환경변수 `ARGOCD_GPG_ENABLED=false`로 비활성화한다.

### VerifyCommitSignature()

```go
// VerifyCommitSignature runs verify-commit on a given revision
func (m *nativeGitClient) VerifyCommitSignature(revision string) (string, error) {
    // git-verify-wrapper.sh 스크립트를 GNUPGHOME 환경변수와 함께 실행
    out, err := m.runGnuPGWrapper(context.Background(), "git-verify-wrapper.sh", revision)
    if err != nil {
        log.Errorf("error verifying commit signature: %v", err)
        return "", errors.New("permission denied")
    }
    return out, nil
}

func (m *nativeGitClient) runGnuPGWrapper(ctx context.Context, wrapper string, args ...string) (string, error) {
    cmd := exec.CommandContext(ctx, wrapper, args...)
    // GNUPGHOME: ArgoCD 전용 GnuPG 홈 디렉토리
    cmd.Env = append(cmd.Env, "GNUPGHOME="+common.GetGnuPGHomePath(), "LANG=C")
    return m.runCmdOutput(cmd, runOpts{})
}
```

### ParseGitCommitVerification()

```go
type PGPVerifyResult struct {
    Date     string  // 서명 날짜
    KeyID    string  // 서명에 사용된 키 ID
    Identity string  // 키 소유자
    Trust    string  // 키 신뢰 수준 (unknown/never/marginal/full/ultimate)
    Cipher   string  // 암호화 알고리즘 (RSA, EdDSA 등)
    Result   string  // Good / Bad / Invalid / Unknown
    Message  string  // 추가 정보
}

const (
    VerifyResultGood    = "Good"
    VerifyResultBad     = "Bad"
    VerifyResultInvalid = "Invalid"
    VerifyResultUnknown = "Unknown"
)
```

### 검증 흐름

```go
// reposerver/repository/repository.go
if gpg.IsGPGEnabled() && q.CheckSignature {
    cs, err := gitClient.VerifyCommitSignature(q.Revision)
    if err != nil {
        return nil, fmt.Errorf("error verifying commit signature: %w", err)
    }
    vr := gpg.ParseGitCommitVerification(cs)
    if vr.Result == gpg.VerifyResultUnknown {
        // 서명 없거나 알 수 없는 키
    }
    signatureInfo = fmt.Sprintf("%s signature from %s key %s",
        vr.Result, vr.Cipher, gpg.KeyID(vr.KeyID))
}
```

### GPG 키 관리

GPG 공개키는 `argocd-gpg-keys-cm` ConfigMap에 저장된다. 프로젝트(AppProject)에서 신뢰할 키 ID를 지정한다:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: AppProject
spec:
  signatureKeys:
  - keyID: "ABC123DEF456"    # 허용된 GPG 키 ID
```

`InitializeGnuPG()`는 repo-server 시작 시 GNUPGHOME 디렉토리를 초기화하고, ConfigMap의 공개키를 가져와서 GnuPG 키링에 가져온다.

**신뢰 수준:**

```
TrustUnknown  = "unknown"   (2) → 검증 불가
TrustNone     = "never"     (3) → 신뢰하지 않음
TrustMarginal = "marginal"  (4) → 부분 신뢰
TrustFull     = "full"      (5) → 완전 신뢰 ✓
TrustUltimate = "ultimate"  (6) → 궁극적 신뢰 ✓
```

---

## Git 프록시 지원

### 소스: `util/proxy/proxy.go`, `util/git/client.go`

### HTTP/HTTPS 프록시

```go
// util/proxy/proxy.go
func GetCallback(proxy string, noProxy string) func(*http.Request) (*url.URL, error) {
    if proxy != "" {
        c := httpproxy.Config{
            HTTPProxy:  proxy,
            HTTPSProxy: proxy,
            NoProxy:    noProxy,
        }
        return func(r *http.Request) (*url.URL, error) {
            return c.ProxyFunc()(r.URL)
        }
    }
    // 프록시 미설정 시 환경변수(HTTP_PROXY, HTTPS_PROXY) 사용
    return DefaultProxyCallback
}

// git CLI 명령에 프록시 환경변수 주입
func UpsertEnv(cmd *exec.Cmd, proxy string, noProxy string) []string {
    envs := []string{}
    if proxy == "" {
        return cmd.Env
    }
    // 기존 프록시 환경변수 제거
    for i, env := range cmd.Env {
        proxyEnv := strings.ToLower(env)
        if strings.HasPrefix(proxyEnv, "http_proxy") ||
            strings.HasPrefix(proxyEnv, "https_proxy") ||
            strings.HasPrefix(proxyEnv, "no_proxy") {
            continue
        }
        envs = append(envs, cmd.Env[i])
    }
    // 리포지토리별 프록시 설정 적용
    return append(envs, httpProxy(proxy), httpsProxy(proxy), noProxyVar(noProxy))
}
```

`runCmdOutput()`에서는 `proxy.UpsertEnv(cmd, m.proxy, m.noProxy)`를 호출하여 리포지토리별 프록시를 적용한다.

### SSH 프록시 (SOCKS5)

SSH 연결에 대한 프록시는 `SSHCreds.Environ()`에서 처리한다:

```go
if c.proxy != "" {
    parsedProxyURL, _ := url.Parse(c.proxy)
    args = append(args, "-o", fmt.Sprintf(
        "ProxyCommand='connect-proxy -S %s:%s -5 %%h %%p'",
        parsedProxyURL.Hostname(), parsedProxyURL.Port()))
    if parsedProxyURL.User != nil {
        proxyEnv = append(proxyEnv, "SOCKS5_USER="+parsedProxyURL.User.Username())
        proxyEnv = append(proxyEnv, "SOCKS5_PASSWD="+passwd)
    }
}
env = append(env, "GIT_SSH_COMMAND="+strings.Join(args, " "))
```

### 설정 방법

리포지토리별 프록시는 Secret에 저장된다:

```yaml
data:
  proxy: "http://proxy.company.com:3128"
  noProxy: "internal.company.com,10.0.0.0/8"
```

---

## Hydration: commitserver와 Git Notes

### 소스: `commitserver/commit/commit.go`, `commitserver/commit/hydratorhelper.go`

Source Hydrator는 Argo CD v3에서 도입된 기능으로, 렌더링된 매니페스트(Hydrated)를 별도 브랜치에 Git으로 커밋한다. 이를 통해 Kubernetes 클러스터가 렌더링 없이 직접 매니페스트를 적용할 수 있다.

### 아키텍처

```
┌───────────────────────────────────────────────────────────────┐
│                    Source Hydrator 흐름                        │
│                                                               │
│  Dry Source (템플릿)          Hydrated (렌더링 결과)            │
│  ┌─────────────────────┐     ┌──────────────────────────┐    │
│  │ main 브랜치          │     │ env/prod 브랜치           │    │
│  │ app/                │────▶│ app/manifest.yaml        │    │
│  │   kustomization.yaml│     │ hydrator.metadata        │    │
│  │   deployment.yaml   │     │ README.md                │    │
│  └─────────────────────┘     └──────────────────────────┘    │
│           │ DrySHA                         │ HydratedSHA      │
│           └──────────── git notes ─────────┘                  │
│                    (hydrator.metadata NS)                      │
└───────────────────────────────────────────────────────────────┘
```

### CommitHydratedManifests()

```go
const NoteNamespace = "hydrator.metadata"

func (s *Service) handleCommitRequest(logCtx *log.Entry,
    r *apiclient.CommitHydratedManifestsRequest) (string, string, error) {

    // 1. Git 클라이언트 초기화 (임시 디렉토리)
    gitClient, dirPath, cleanup, _ := s.initGitClient(logCtx, r)
    defer cleanup()

    // 2. sync 브랜치 체크아웃 (없으면 orphan 브랜치 생성)
    gitClient.CheckoutOrOrphan(r.SyncBranch, false)

    // 3. target 브랜치 체크아웃 (없으면 sync 브랜치 기반으로 생성)
    gitClient.CheckoutOrNew(r.TargetBranch, r.SyncBranch, false)

    hydratedSha, _ := gitClient.CommitSHA()

    // 4. 이미 hydration됐는지 git notes로 확인
    isHydrated, _ := IsHydrated(gitClient, r.DrySha, hydratedSha)
    if isHydrated {
        return "", hydratedSha, nil  // 중복 작업 방지
    }

    // 5. 매니페스트 파일 쓰기
    shouldCommit, _ := WriteForPaths(root, r.Repo.Repo, r.DrySha,
        r.DryCommitMetadata, r.Paths, gitClient)

    if !shouldCommit {
        // 매니페스트 변경 없음 - 노트만 추가
        AddNote(gitClient, r.DrySha, hydratedSha)
        return "", hydratedSha, nil
    }

    // 6. 커밋 및 푸시
    gitClient.CommitAndPush(r.TargetBranch, r.CommitMessage)

    sha, _ := gitClient.CommitSHA()

    // 7. DrySHA → HydratedSHA 매핑을 git notes에 저장
    AddNote(gitClient, r.DrySha, sha)
    return "", sha, nil
}
```

### initGitClient()

```go
func (s *Service) initGitClient(...) (git.Client, string, func(), error) {
    // /tmp/_commit-service 아래에 임시 디렉토리 생성
    dirPath, _ := files.CreateTempDir("/tmp/_commit-service")

    gitClient, _ := s.repoClientFactory.NewClient(r.Repo, dirPath)
    gitClient.Init()
    gitClient.Fetch("", 0)  // 전체 리포 가져오기

    // 작성자 설정 (GitHub App 정보 또는 기본값)
    authorName := r.AuthorName
    if authorName == "" {
        authorName = "Argo CD"
    }
    authorEmail := r.AuthorEmail
    if authorEmail == "" {
        authorEmail = "argo-cd@example.com"
    }
    gitClient.SetAuthor(authorName, authorEmail)

    return gitClient, dirPath, cleanupOrLog, nil
}
```

### Git Notes: DrySHA → HydratedSHA 매핑

```go
// AddAndPushNote: 지수 백오프로 동시 업데이트 처리
func (m *nativeGitClient) AddAndPushNote(sha string, namespace string, note string) error {
    notesRef := "refs/notes/" + namespace  // "refs/notes/hydrator.metadata"

    b := backoff.NewExponentialBackOff()
    b.InitialInterval = 50 * time.Millisecond
    b.MaxInterval = 1 * time.Second

    operation := func() (struct{}, error) {
        // 최신 notes fetch (+ prefix로 강제 업데이트)
        m.runCredentialedCmd(ctx, "fetch", "origin",
            fmt.Sprintf("+%s:%s", notesRef, notesRef))

        // 로컬에 note 추가 (-f: 덮어쓰기)
        m.runCmd(ctx, "notes", "--ref="+namespace, "add", "-f", "-m", note, sha)

        // 푸시 (--force 없이 - 충돌 시 재시도)
        err = m.runCredentialedCmd(ctx, "push", "origin", notesRef)
        if err != nil {
            // 재시도 가능한 에러 판별
            isRetryable := strings.Contains(errStr, "fetch first") ||
                strings.Contains(errStr, "reference already exists") ||
                strings.Contains(errStr, "incorrect old value")
            if !isRetryable {
                return struct{}{}, backoff.Permanent(err)
            }
            return struct{}{}, err  // 재시도
        }
        return struct{}{}, nil
    }
    _, err := backoff.Retry(ctx, operation, backoff.WithBackOff(b),
        backoff.WithMaxElapsedTime(5*time.Second))
}
```

### IsHydrated() 확인

```go
func IsHydrated(gitClient git.Client, drySha, commitSha string) (bool, error) {
    note, err := gitClient.GetCommitNote(commitSha, NoteNamespace)
    if errors.Is(err, git.ErrNoNoteFound) {
        return false, nil  // 노트 없음 = 아직 hydration 안됨
    }
    if err != nil {
        return false, err
    }
    var commitNote CommitNote
    json.Unmarshal([]byte(note), &commitNote)
    return commitNote.DrySHA == drySha, nil  // DrySHA 일치 여부 확인
}

type CommitNote struct {
    DrySHA string `json:"drySa"` // hydration을 트리거한 원본 커밋 SHA
}
```

### WriteForPaths()

```go
func WriteForPaths(root *os.Root, repoUrl, drySha string,
    dryCommitMetadata *appv1.RevisionMetadata,
    paths []*apiclient.PathDetails, gitClient git.Client) (bool, error) {

    // 최상위 hydrator.metadata 파일 작성
    writeMetadata(root, "", hydratorMetadata)

    // .gitattributes 작성 (README.md, hydrator.metadata를 generated로 표시)
    writeGitAttributes(root)

    var atleastOneManifestChanged bool
    for _, p := range paths {
        // 각 경로에 manifest.yaml 작성
        writeManifests(root, hydratePath, p.Manifests)

        // git diff로 실제 변경 여부 확인
        changed, _ := gitClient.HasFileChanged(filepath.Join(hydratePath, ManifestYaml))
        if !changed {
            continue  // 변경 없으면 해당 경로 스킵
        }
        atleastOneManifestChanged = true

        // hydrator.metadata 업데이트
        writeMetadata(root, hydratePath, hydratorMetadata)
        // README.md 업데이트
        writeReadme(root, hydratePath, hydratorMetadata)
    }
    return atleastOneManifestChanged, nil
}
```

---

## 왜 이런 설계인가

### 1. git CLI 래핑 vs go-git 라이브러리

**선택: git CLI 바이너리 래핑 (주요 작업)**

```
장점:
- git의 모든 기능을 그대로 활용 (submodule, LFS, notes 등)
- git 버전 업데이트 시 자동으로 새 기능 획득
- 디버깅 용이 (git 명령을 직접 재현 가능)
- go-git이 지원하지 않는 기능 처리 가능

단점:
- 외부 프로세스 실행 오버헤드
- git 바이너리 의존성
```

**go-git 제한적 사용 (LsRemote/LsRefs)**

```
이유:
- 원격 refs 조회는 로컬 클론 없이도 가능해야 함
- in-memory storage로 동시 실행 안전
- 네트워크 레이어만 go-git 활용
```

### 2. repoLock: 동일 리포 직렬화

```
문제: 동일 리포지토리를 동시에 다른 revision으로 체크아웃하면?
     → git checkout은 파일시스템을 변경하므로 race condition 발생

해결: repositoryLock으로 동일 path에 대한 접근 직렬화
     - 동일 revision + allowConcurrent=true → 병렬 허용 (읽기 작업)
     - 다른 revision → 순차 처리
```

### 3. Credential 템플릿: URL prefix 매칭

```
문제: 수백 개의 리포지토리에 동일한 자격증명을 개별 설정?
     → 관리 불가능, Secret 수 폭증

해결: URL prefix로 자격증명 상속
     - github.com/myorg/ → 해당 조직의 모든 리포에 적용
     - 새 리포 추가 시 별도 설정 불필요
```

### 4. argocd-git-ask-pass: Nonce 기반 자격증명

```
문제: 환경변수에 비밀번호를 직접 넣으면 외부 도구가 유출할 수 있음
     (Kustomize 버그 사례)

해결: nonce(UUID)만 환경변수에 노출
     - nonce가 유출되어도 서버 접근 없이는 실제 자격증명 불가
     - git 완료 후 즉시 nonce 삭제
```

### 5. FNV-32a 해시 기반 Secret 네이밍

```
문제: Git URL을 Secret 이름으로 직접 사용?
     - URL에는 /, :, . 등 K8s 이름에 허용되지 않는 문자 포함
     - URL 길이가 Secret 이름 최대 길이(253자)를 초과할 수 있음

해결: FNV-32a 해시로 짧고 유효한 이름 생성
     - 빠른 비암호화 해시 (보안 불필요, 고유성만 필요)
     - 결과: "repo-2847382910" 형식
```

### 6. Git Notes: DrySHA → HydratedSHA 매핑

```
문제: 렌더링 결과(Hydrated SHA)와 원본(Dry SHA)의 관계를 어디에 저장?
     - 별도 DB: 추가 인프라 필요
     - ConfigMap: etcd 부하

해결: git notes 활용
     - Git 리포지토리 자체에 메타데이터 저장
     - refs/notes/hydrator.metadata 네임스페이스 사용
     - 중복 hydration 방지 (이미 처리된 DrySHA는 스킵)
```

---

## 전체 흐름 요약

### Webhook 기반 동기화 흐름

```
Git Push 이벤트
      │
      ▼
┌─────────────────────────────────────────────────────────────────┐
│                   API Server (Webhook Handler)                   │
│                                                                  │
│  1. Webhook 시크릿 검증                                           │
│  2. 페이로드 파싱 → webURL, revision, changedFiles                │
│  3. payloadQueueSize(50000) 버퍼 큐에 추가                        │
│  4. 워커 풀에서 HandleEvent() 처리                                 │
│  5. 모든 Application 순회                                         │
│  6. sourceRevisionHasChanged + sourceUsesURL 확인                 │
│  7. GetSourceRefreshPaths + AppFilesHaveChanged 확인              │
│  8. RefreshApp() 호출 → Application 리소스에 annotation 추가      │
└─────────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────────┐
│                   Application Controller                         │
│                                                                  │
│  9. Application watch → refresh annotation 감지                   │
│  10. Repo Server에 GenerateManifests 요청                         │
└─────────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────────┐
│                       Repo Server                                │
│                                                                  │
│  11. GetRepository() → K8s Secret에서 자격증명 로드               │
│  12. enrichCredsToRepo() → 크리덴셜 템플릿 상속                   │
│  13. newClient() → nativeGitClient 생성                          │
│  14. repoLock.Lock() → 동시 접근 제어                             │
│  15. checkoutRevision():                                          │
│      a. gitClient.Init()                                          │
│      b. gitClient.Fetch(revision, depth)                          │
│      c. gitClient.Checkout(revision, submoduleEnabled)            │
│  16. GPG 서명 검증 (설정된 경우)                                   │
│  17. GenerateManifests() → Helm/Kustomize/Directory 렌더링        │
│  18. Lock 해제                                                    │
└─────────────────────────────────────────────────────────────────┘
```

### 자격증명 흐름 다이어그램

```
┌────────────────────────────────────────────────────────────────┐
│                     자격증명 처리 흐름                           │
│                                                                 │
│  K8s Secret          DB Layer          Git Client               │
│  (argocd ns)         (util/db)         (util/git)               │
│                                                                 │
│  repo-XXXXX  ──────► GetRepository ──► HTTPSCreds              │
│  Secret      secret  ()               .Environ()               │
│              ToRepo  ▲                    │                     │
│                      │ prefix 매칭        │ 환경변수 목록         │
│  creds-YYYYY ────────┤                    ▼                     │
│  Secret      enrich  │              askpass.server              │
│              CredsTo │              .Add(user, pass)            │
│              Repo()  │                    │                     │
│                      │              nonce 반환                   │
│                                          │                     │
│                                    git fetch                    │
│                                    GIT_ASKPASS=argocd           │
│                                    ARGOCD_GIT_ASKPASS_NONCE=...│
│                                          │                     │
│                                    argocd git-ask-pass          │
│                                    → gRPC로 nonce 조회          │
│                                    → username/password 반환     │
└────────────────────────────────────────────────────────────────┘
```

### 핵심 파일 참조표

| 파일 | 주요 구조체/함수 | 역할 |
|------|----------------|------|
| `util/git/client.go` | `Client` (인터페이스), `nativeGitClient` | Git 작업 추상화 및 구현 |
| `util/git/creds.go` | `HTTPSCreds`, `SSHCreds`, `GitHubAppCreds` | 자격증명 유형별 구현 |
| `util/askpass/server.go` | `server`, `CredsStore` | GIT_ASKPASS 기반 자격증명 서버 |
| `util/db/repository.go` | `enrichCredsToRepo()`, `RepoURLToSecretName()` | 리포 DB 조작 |
| `util/db/repository_secrets.go` | `secretsRepositoryBackend`, `secretToRepository()` | K8s Secret 백엔드 |
| `util/webhook/webhook.go` | `ArgoCDWebhookHandler`, `HandleEvent()` | Webhook 처리 |
| `util/app/path/path.go` | `GetSourceRefreshPaths()`, `AppFilesHaveChanged()` | manifest-generate-paths |
| `util/gpg/gpg.go` | `ParseGitCommitVerification()`, `IsGPGEnabled()` | GPG 서명 검증 |
| `util/proxy/proxy.go` | `UpsertEnv()`, `GetCallback()` | 프록시 처리 |
| `reposerver/repository/lock.go` | `repositoryLock`, `repositoryState` | 동시성 제어 |
| `reposerver/repository/repository.go` | `Service.Init()`, `checkoutRevision()` | Repo Server 핵심 로직 |
| `commitserver/commit/commit.go` | `Service`, `handleCommitRequest()` | Hydration 커밋 |
| `commitserver/commit/hydratorhelper.go` | `WriteForPaths()`, `IsHydrated()`, `AddNote()` | Hydration 헬퍼 |
