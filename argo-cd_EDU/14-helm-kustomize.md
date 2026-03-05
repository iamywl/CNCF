# 14. Argo CD Helm 및 Kustomize 통합 Deep-Dive

Argo CD는 Kubernetes 매니페스트를 생성하기 위해 Helm과 Kustomize를 일급(first-class) 도구로 지원한다. 두 도구 모두 CLI 바이너리를 직접 래핑하는 방식을 택하며, repo-server 내부의 `reposerver/repository/repository.go`가 이 통합의 핵심 조율자 역할을 한다. 이 문서는 실제 소스코드(`util/helm/`, `util/kustomize/`, `reposerver/repository/`)를 직접 읽고 확인한 내용만을 인용한다.

---

## 1. Helm 통합 개요

Argo CD의 Helm 통합은 `util/helm/` 패키지에 집중되어 있다. 핵심 구성요소는 세 가지다.

| 구성요소 | 파일 | 역할 |
|---------|------|------|
| `Helm` 인터페이스 | `util/helm/helm.go` | helm 바이너리 래핑 고수준 인터페이스 |
| `Cmd` 구조체 | `util/helm/cmd.go` | 실제 `exec.Command("helm", ...)` 실행기 |
| `Client` 인터페이스 | `util/helm/client.go` | Helm 차트 저장소(레지스트리) 클라이언트 |

```
ManifestRequest
      │
      ▼
helmTemplate()          ← reposerver/repository/repository.go
      │
      ▼
NewHelmApp()            ← util/helm/helm.go
      │
      ├─ NewCmd()       ← util/helm/cmd.go (helm 바이너리 래핑)
      │
      └─ helm.Template() → Cmd.template() → exec("helm template ...")
```

Helm 통합의 핵심 설계 원칙: **Argo CD는 자체적으로 Helm 라이브러리를 임포트하지 않는다.** 대신 시스템에 설치된 `helm` CLI 바이너리를 `os/exec`로 실행한다. 이 이유는 [14절 "왜 이런 설계인가"](#14-왜why-이런-설계인가)에서 상세히 다룬다.

---

## 2. Helm Interface (util/helm/helm.go)

`Helm` 인터페이스는 `util/helm/helm.go`에 정의되어 있다.

```go
// util/helm/helm.go

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

구체 구현체는 `helm` 구조체다.

```go
// util/helm/helm.go

type helm struct {
    cmd             Cmd
    repos           []HelmRepository
    passCredentials bool
}
```

### NewHelmApp()

```go
// util/helm/helm.go

func NewHelmApp(workDir string, repos []HelmRepository, isLocal bool, version string, proxy string, noProxy string, passCredentials bool) (Helm, error) {
    cmd, err := NewCmd(workDir, version, proxy, noProxy)
    if err != nil {
        return nil, fmt.Errorf("failed to create new helm command: %w", err)
    }
    cmd.IsLocal = isLocal
    return &helm{repos: repos, cmd: *cmd, passCredentials: passCredentials}, nil
}
```

`isLocal`은 로컬 개발 모드일 때 `true`로 설정된다. 이 경우 `XDG_CACHE_HOME`, `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `HELM_CONFIG_HOME` 환경변수를 임시 디렉토리로 격리하지 않고 시스템 기본값을 사용한다.

### DependencyBuild()

`DependencyBuild()`는 OCI 레지스트리 의존성과 일반 HTTP Helm 레포지토리 의존성을 모두 처리한다.

```go
// util/helm/helm.go

func (h *helm) DependencyBuild() error {
    isHelmOci := h.cmd.IsHelmOci
    defer func() { h.cmd.IsHelmOci = isHelmOci }()

    for i := range h.repos {
        repo := h.repos[i]
        if repo.EnableOci {
            h.cmd.IsHelmOci = true
            helmPassword, err := repo.GetPassword()
            // ...
            if repo.GetUsername() != "" && helmPassword != "" {
                _, err := h.cmd.RegistryLogin(repo.Repo, repo.Creds)
                defer func() {
                    _, _ = h.cmd.RegistryLogout(repo.Repo, repo.Creds)
                }()
            }
        } else {
            _, err := h.cmd.RepoAdd(repo.Name, repo.Repo, repo.Creds, h.passCredentials)
        }
    }
    h.repos = nil
    _, err := h.cmd.dependencyBuild()
    return err
}
```

OCI 레지스트리는 `helm registry login` / `helm registry logout`을 사용하고, 일반 HTTP 레포지토리는 `helm repo add`를 사용한다.

### GetParameters()

`GetParameters()`는 values.yaml 파일들을 파싱하여 플랫(flat) 키-값 맵으로 변환한다. 원격 URL(`http://`, `https://`)을 통한 values 파일도 지원한다.

```go
// util/helm/helm.go

func (h *helm) GetParameters(valuesFiles []pathutil.ResolvedFilePath, appPath, repoRoot string) (map[string]string, error) {
    // ...
    output := map[string]string{}
    for _, file := range values {
        values := map[string]any{}
        if err := yaml.Unmarshal([]byte(file), &values); err != nil {
            return nil, fmt.Errorf("failed to parse values: %w", err)
        }
        flatVals(values, output)
    }
    return output, nil
}
```

`flatVals()`는 중첩된 YAML 구조를 `parent.child[0].leaf=value` 형태로 평탄화한다.

```go
// util/helm/helm.go

func flatVals(input any, output map[string]string, prefixes ...string) {
    switch i := input.(type) {
    case map[string]any:
        for k, v := range i {
            flatVals(v, output, append(prefixes, k)...)
        }
    case []any:
        p := append([]string(nil), prefixes...)
        for j, v := range i {
            flatVals(v, output, append(p[0:len(p)-1], fmt.Sprintf("%s[%v]", prefixes[len(p)-1], j))...)
        }
    default:
        output[strings.Join(prefixes, ".")] = fmt.Sprintf("%v", i)
    }
}
```

---

## 3. Helm Cmd (util/helm/cmd.go)

`Cmd`는 `helm` 바이너리를 직접 실행하는 저수준 래퍼다.

```go
// util/helm/cmd.go

// A thin wrapper around the "helm" command, adding logging and error translation.
type Cmd struct {
    helmHome        string
    WorkDir         string
    IsLocal         bool
    IsHelmOci       bool
    proxy           string
    noProxy         string
    runWithRedactor func(cmd *exec.Cmd, redactor func(text string) string) (string, error)
}
```

`helmHome`은 임시 디렉토리 경로다. `NewCmdWithVersion()`이 호출될 때마다 `os.MkdirTemp("", "helm")`으로 격리된 Helm 홈 디렉토리가 생성된다.

```go
// util/helm/cmd.go

func newCmdWithVersion(workDir string, isHelmOci bool, proxy string, noProxy string, runWithRedactor func(...) (string, error)) (*Cmd, error) {
    tmpDir, err := os.MkdirTemp("", "helm")
    // ...
    return &Cmd{WorkDir: workDir, helmHome: tmpDir, IsHelmOci: isHelmOci, ...}, err
}
```

### run() - 실제 명령 실행

```go
// util/helm/cmd.go

func (c Cmd) run(ctx context.Context, args ...string) (string, string, error) {
    cmd := exec.CommandContext(ctx, "helm", args...)
    cmd.Dir = c.WorkDir
    cmd.Env = os.Environ()
    if !c.IsLocal {
        cmd.Env = append(cmd.Env,
            fmt.Sprintf("XDG_CACHE_HOME=%s/cache", c.helmHome),
            fmt.Sprintf("XDG_CONFIG_HOME=%s/config", c.helmHome),
            fmt.Sprintf("XDG_DATA_HOME=%s/data", c.helmHome),
            fmt.Sprintf("HELM_CONFIG_HOME=%s/config", c.helmHome))
    }
    cmd.Env = proxy.UpsertEnv(cmd, c.proxy, c.noProxy)
    // ...
}
```

비로컬 모드에서 XDG 환경변수를 임시 디렉토리로 격리하는 이유: 여러 앱이 동시에 Helm을 실행할 때 `~/.helm/`, `~/.config/helm/` 등의 전역 상태가 충돌하지 않도록 방지하기 위해서다.

### 크리덴셜 보안: redactor

```go
// util/helm/cmd.go

var redactor = func(text string) string {
    return regexp.MustCompile("(--username|--password) [^ ]*").ReplaceAllString(text, "$1 ******")
}
```

로그에 `--username`, `--password` 값이 노출되지 않도록 마스킹한다.

### template() - helm template 명령

```go
// util/helm/cmd.go

type TemplateOpts struct {
    Name                 string
    Namespace            string
    KubeVersion          string
    APIVersions          []string
    Set                  map[string]string
    SetString            map[string]string
    SetFile              map[string]pathutil.ResolvedFilePath
    Values               []pathutil.ResolvedFilePath
    ExtraValues          pathutil.ResolvedFilePath   // Values/ValuesObject의 임시 파일
    SkipCrds             bool
    SkipSchemaValidation bool
    SkipTests            bool
}

func (c *Cmd) template(chartPath string, opts *TemplateOpts) (string, string, error) {
    if callback, err := cleanupChartLockFile(filepath.Clean(path.Join(c.WorkDir, chartPath))); err == nil {
        defer callback()
    }

    args := []string{"template", chartPath, "--name-template", opts.Name}

    if opts.Namespace != "" {
        args = append(args, "--namespace", opts.Namespace)
    }
    for key, val := range opts.Set {
        args = append(args, "--set", key+"="+cleanSetParameters(val))
    }
    // --set-string, --set-file, --values, --api-versions 등 추가...
    if !opts.SkipCrds {
        args = append(args, "--include-crds")
    }
    // ...
}
```

실제로 생성되는 명령 예시:

```
helm template . \
  --name-template my-app \
  --namespace prod \
  --values values.yaml \
  --values /tmp/uuid-generated-extra-values.yaml \
  --set image.tag=v1.2.3 \
  --include-crds \
  --kube-version 1.28
```

### Chart.lock 정리 워크어라운드

```go
// util/helm/cmd.go

// Workaround for Helm3 behavior (see https://github.com/helm/helm/issues/6870).
// The `helm template` command generates Chart.lock after which `helm dependency build` does not work.
func cleanupChartLockFile(chartPath string) (func(), error) {
    exists := true
    lockPath := path.Join(chartPath, "Chart.lock")
    if _, err := os.Stat(lockPath); err != nil {
        if !os.IsNotExist(err) {
            return nil, fmt.Errorf("failed to check lock file status: %w", err)
        }
        exists = false
    }
    return func() {
        if !exists {
            _ = os.Remove(lockPath)
        }
    }, nil
}
```

`helm template` 실행 후 생성된 `Chart.lock`을 정리하는 워크어라운드다. 이 파일이 남아 있으면 이후 `helm dependency build`가 실패하는 Helm 버그(#6870)를 회피한다.

---

## 4. Helm Client Interface (util/helm/client.go)

`Client` 인터페이스는 Helm 차트 저장소(레지스트리)와의 통신을 담당한다.

```go
// util/helm/client.go

type Client interface {
    CleanChartCache(chart string, version string) error
    ExtractChart(chart string, version string, passCredentials bool, manifestMaxExtractedSize int64, disableManifestMaxExtractedSize bool) (string, utilio.Closer, error)
    GetIndex(noCache bool, maxIndexSize int64) (*Index, error)
    GetTags(chart string, noCache bool) ([]string, error)
    TestHelmOCI() (bool, error)
}
```

| 메서드 | 역할 |
|--------|------|
| `CleanChartCache` | 캐시된 차트 tar 파일 삭제 (하드 리프레시 시) |
| `ExtractChart` | 차트를 레지스트리에서 pull하고 임시 디렉토리에 압축 해제 |
| `GetIndex` | HTTP Helm 레포지토리의 `index.yaml` 조회 |
| `GetTags` | OCI 레지스트리에서 차트 버전 태그 목록 조회 |
| `TestHelmOCI` | OCI 레지스트리 접근성 테스트 |

구체 구현체는 `nativeHelmChart`다.

```go
// util/helm/client.go

type nativeHelmChart struct {
    chartCachePaths utilio.TempPaths
    repoURL         string
    creds           Creds
    repoLock        sync.KeyLock
    enableOci       bool
    indexCache      indexCache
    proxy           string
    noProxy         string
    customUserAgent string
}
```

### ExtractChart() - 차트 다운로드 및 압축 해제

```
ExtractChart()
    │
    ├─ getCachedChartPath() → 캐시 경로 계산 (url+chart+version의 해시)
    │
    ├─ repoLock.Lock(cachedChartPath) → 동일 차트 동시 다운로드 방지
    │
    ├─ 캐시 miss 시:
    │   ├─ OCI:  helmCmd.PullOCI() → "helm pull oci://repo/chart --version v"
    │   └─ HTTP: helmCmd.Fetch()   → "helm pull --repo repo chart --version v"
    │
    └─ untarChart() → tar 압축 해제 (크기 제한 적용)
```

```go
// util/helm/client.go

func (c *nativeHelmChart) ExtractChart(chart string, version string, passCredentials bool, manifestMaxExtractedSize int64, disableManifestMaxExtractedSize bool) (string, utilio.Closer, error) {
    // ...
    c.repoLock.Lock(cachedChartPath)
    defer c.repoLock.Unlock(cachedChartPath)

    exists, err := fileExist(cachedChartPath)
    if !exists {
        if c.enableOci {
            _, err = helmCmd.PullOCI(c.repoURL, chart, version, tempDest, c.creds)
        } else {
            _, err = helmCmd.Fetch(c.repoURL, chart, version, tempDest, c.creds, passCredentials)
        }
        // tar 파일을 캐시 경로로 이동
        err = os.Rename(chartFilePath, cachedChartPath)
    }
    err = untarChart(context.Background(), tempDir, cachedChartPath, manifestMaxExtractedSize, disableManifestMaxExtractedSize)
    return path.Join(tempDir, normalizeChartName(chart)), utilio.NewCloser(func() error {
        return os.RemoveAll(tempDir)
    }), nil
}
```

반환값의 `io.Closer`는 호출자가 매니페스트 생성을 마친 뒤 임시 디렉토리를 정리하는 데 사용된다.

### User-Agent 설정

```go
// util/helm/client.go

func (c *nativeHelmChart) getUserAgent() string {
    if c.customUserAgent != "" {
        return c.customUserAgent
    }
    version := common.GetVersion()
    return fmt.Sprintf("argocd-repo-server/%s (%s)", version.Version, version.Platform)
}
```

HTTP Helm 레포지토리에서 `index.yaml`을 조회하거나 OCI 레지스트리에서 태그를 조회할 때 `User-Agent` 헤더를 설정한다. `argocd-repo-server/v2.x.x (linux/amd64)` 형태로, 레포지토리 관리자가 트래픽 출처를 식별할 수 있도록 한다. `HelmUserAgent`가 `RepoServerInitConstants`에 설정되어 있으면 커스텀 값이 우선 적용된다(`reposerver/repository/repository.go`의 `newHelmClient` 클로저 참조).

### GetTags() - OCI 태그 목록

```go
// util/helm/client.go

func (c *nativeHelmChart) GetTags(chart string, noCache bool) ([]string, error) {
    if !c.enableOci {
        return nil, ErrOCINotEnabled
    }
    // oras-go v2 라이브러리로 OCI 레지스트리 태그 조회
    err = repo.Tags(ctx, "", func(tagsResult []string) error {
        for _, tag := range tagsResult {
            // underscore → plus 변환 (SemVer + 구분자 문제 해결)
            convertedTag := strings.ReplaceAll(tag, "_", "+")
            entries.Tags = append(entries.Tags, convertedTag)
        }
        return nil
    })
    // ...
}
```

OCI 레지스트리에서 `_`는 `+`를 대체해서 저장되므로(SemVer의 빌드 메타데이터 구분자 문제), 조회 시 역변환한다.

---

## 5. ApplicationSourceHelm

`ApplicationSourceHelm`은 Argo CD Application CRD에서 Helm 소스를 정의하는 구조체다 (`pkg/apis/application/v1alpha1/types.go`).

```go
// pkg/apis/application/v1alpha1/types.go

type ApplicationSourceHelm struct {
    // values.yaml 파일 목록 (Git 경로 또는 URL)
    ValueFiles []string `json:"valueFiles,omitempty"`
    // --set 파라미터
    Parameters []HelmParameter `json:"parameters,omitempty"`
    // 릴리스 이름 (미설정 시 앱 이름 사용)
    ReleaseName string `json:"releaseName,omitempty"`
    // 인라인 values.yaml 블록 (ValuesObject가 우선)
    Values string `json:"values,omitempty"`
    // --set-file 파라미터
    FileParameters []HelmFileParameter `json:"fileParameters,omitempty"`
    // Helm 버전 ("3" 만 지원)
    Version string `json:"version,omitempty"`
    // --pass-credentials
    PassCredentials bool `json:"passCredentials,omitempty"`
    // values 파일이 없을 때 오류 무시
    IgnoreMissingValueFiles bool `json:"ignoreMissingValueFiles,omitempty"`
    // --skip-crds
    SkipCrds bool `json:"skipCrds,omitempty"`
    // 인라인 values (구조체 형태, Values보다 우선)
    ValuesObject *runtime.RawExtension `json:"valuesObject,omitempty"`
    // 네임스페이스 오버라이드
    Namespace string `json:"namespace,omitempty"`
    // Kubernetes API 버전 (--kube-version)
    KubeVersion string `json:"kubeVersion,omitempty"`
    // --api-versions
    APIVersions []string `json:"apiVersions,omitempty"`
    // --skip-tests
    SkipTests bool `json:"skipTests,omitempty"`
    // --skip-schema-validation
    SkipSchemaValidation bool `json:"skipSchemaValidation,omitempty"`
}
```

### Helm 소스 유형

Argo CD Application에서 Helm을 사용하는 두 가지 방법:

| 유형 | repoURL | chart | path | 설명 |
|------|---------|-------|------|------|
| Helm 레포지토리 | `https://charts.helm.sh/stable` | `nginx-ingress` | (없음) | Helm 레포지토리에서 차트 직접 |
| Git 레포 내 차트 | `https://github.com/org/repo` | (없음) | `charts/myapp` | Git 레포의 특정 경로에 있는 차트 |

```yaml
# Helm 레포지토리에서 차트
spec:
  source:
    repoURL: https://charts.helm.sh/stable
    chart: nginx-ingress
    targetRevision: 1.41.3
    helm:
      releaseName: my-nginx
      values: |
        controller:
          replicaCount: 2

# Git 레포의 로컬 차트
spec:
  source:
    repoURL: https://github.com/myorg/myrepo
    path: charts/myapp
    targetRevision: HEAD
    helm:
      parameters:
        - name: image.tag
          value: v1.2.3
```

### Values와 ValuesObject

```go
// pkg/apis/application/v1alpha1/types.go

// ValuesIsEmpty returns true if both Values and ValuesObject are empty
func (a *ApplicationSourceHelm) ValuesIsEmpty() bool {
    return len(a.Values) == 0 && (a.ValuesObject == nil || len(a.ValuesObject.Raw) == 0)
}

// ValuesYAML returns values as a []byte
func (a *ApplicationSourceHelm) ValuesYAML() []byte {
    if a.ValuesObject != nil && len(a.ValuesObject.Raw) > 0 {
        data, _ := yaml.JSONToYAML(a.ValuesObject.Raw)
        return data
    }
    return []byte(a.Values)
}
```

`Values`는 문자열 형태의 YAML이고, `ValuesObject`는 구조체 형태의 JSON/YAML이다. `ValuesObject`가 설정되어 있으면 우선 적용된다. 두 방식 모두 임시 파일에 기록된 뒤 `--values /tmp/uuid-file` 형태로 전달된다.

---

## 6. helmTemplate() 호출 흐름

`helmTemplate()`는 `reposerver/repository/repository.go`에 정의된 패키지 수준 함수다.

```
GenerateManifests() [reposerver/repository/repository.go]
    │
    ├─ newEnv()                         → ARGOCD_APP_* 환경변수 준비
    │
    ├─ GetAppSourceType()               → 소스 타입 감지 (Helm/Kustomize/Directory)
    │   └─ mergeSourceParameters()      → .argocd-source.yaml 오버라이드 적용
    │
    └─ helmTemplate()                   ← 이 절에서 분석
           │
           ├─ TemplateOpts 구성
           │   ├─ Name = appName (언더스코어 제거, 53자 제한)
           │   ├─ resolvedValueFiles()  → values 파일 경로 검증
           │   ├─ ValuesYAML() → 임시 파일 → ExtraValues
           │   ├─ Parameters → Set / SetString
           │   └─ FileParameters → SetFile
           │
           ├─ NewHelmApp()              → helm 구조체 생성
           │
           ├─ h.Template(templateOpts) → helm template 실행
           │   └─ 실패 + IsMissingDependencyErr → runHelmBuild()
           │
           └─ kube.SplitYAML()         → 출력 YAML → []*unstructured.Unstructured
```

### 릴리스 이름 처리

```go
// reposerver/repository/repository.go

func helmTemplate(appPath string, repoRoot string, env *v1alpha1.Env, q *apiclient.ManifestRequest, isLocal bool, gitRepoPaths utilio.TempPaths) ([]*unstructured.Unstructured, string, error) {
    // We use the app name as Helm's release name property, which must not
    // contain any underscore characters and must not exceed 53 characters.
    appName, _ := argo.ParseInstanceName(q.AppName, "")

    templateOpts := &helm.TemplateOpts{
        Name:      appName,
        Namespace: q.ApplicationSource.GetNamespaceOrDefault(q.Namespace),
        // ...
    }
    // ReleaseName이 명시적으로 설정된 경우 덮어쓰기
    if appHelm.ReleaseName != "" {
        templateOpts.Name = appHelm.ReleaseName
    }
    // ...
}
```

Helm의 릴리스 이름은 언더스코어를 허용하지 않고 53자를 초과할 수 없다. `argo.ParseInstanceName()`은 `appName` 파싱 시 이 제약을 처리한다.

### 환경변수 치환

```go
// reposerver/repository/repository.go

for i, j := range templateOpts.Set {
    templateOpts.Set[i] = env.Envsubst(j)
}
for i, j := range templateOpts.SetString {
    templateOpts.SetString[i] = env.Envsubst(j)
}
```

`--set` 파라미터 값에 `${ARGOCD_APP_REVISION}` 같은 환경변수가 포함된 경우 치환된다.

---

## 7. runHelmBuild() - 의존성 빌드 동시성 제어

```go
// reposerver/repository/repository.go

const (
    helmDepUpMarkerFile = ".argocd-helm-dep-up"
)

var manifestGenerateLock = sync.NewKeyLock()

// runHelmBuild executes `helm dependency build` in a given path and ensures that it is executed only once
// if multiple threads are trying to run it.
// Multiple goroutines might process same helm app in one repo concurrently when repo server process multiple
// manifest generation requests of the same commit.
func runHelmBuild(appPath string, h helm.Helm) error {
    manifestGenerateLock.Lock(appPath)
    defer manifestGenerateLock.Unlock(appPath)

    // the `helm dependency build` is potentially a time-consuming 1~2 seconds,
    // a marker file is used to check if command already run to avoid running it again unnecessarily
    // the file is removed when repository is re-initialized (e.g. when another commit is processed)
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

### 호출 시점

`helmTemplate()`에서 최초 `h.Template()` 호출 시 `IsMissingDependencyErr`가 발생하면 `runHelmBuild()`를 호출한다.

```go
// reposerver/repository/repository.go

out, command, err := h.Template(templateOpts)
if err != nil {
    if !helm.IsMissingDependencyErr(err) {
        return nil, "", err
    }
    err = runHelmBuild(appPath, h)
    // ...
}
```

`IsMissingDependencyErr`는 다음 두 오류 메시지를 감지한다.

```go
// util/helm/helm.go

func IsMissingDependencyErr(err error) bool {
    return strings.Contains(err.Error(), "found in requirements.yaml, but missing in charts") ||
        strings.Contains(err.Error(), "found in Chart.yaml, but missing in charts/ directory")
}
```

### 동시성 제어 흐름

```
고루틴 A: helmTemplate(appPath="path/to/chart")
고루틴 B: helmTemplate(appPath="path/to/chart")  ← 동시 요청

    A: h.Template() → IsMissingDependencyErr 발생
    B: h.Template() → IsMissingDependencyErr 발생

    A: manifestGenerateLock.Lock("path/to/chart")  ← 락 획득
    B: manifestGenerateLock.Lock("path/to/chart")  ← 블로킹 대기

    A: os.Stat(".argocd-helm-dep-up") → 없음
    A: h.DependencyBuild() → helm dependency build 실행
    A: os.WriteFile(".argocd-helm-dep-up", "marker")
    A: manifestGenerateLock.Unlock()

    B: 락 획득
    B: os.Stat(".argocd-helm-dep-up") → 있음 → return nil (생략)
    B: manifestGenerateLock.Unlock()
```

---

## 8. OCI 레지스트리 지원

### Helm OCI 감지

```go
// util/helm/client.go

// Ensures that given OCI registries URL does not have protocol
func IsHelmOciRepo(repoURL string) bool {
    if repoURL == "" {
        return false
    }
    parsed, err := url.Parse(repoURL)
    // URL 파서는 scheme이 없으면 hostname을 path로 처리하므로, Host가 비어있어야 함
    return err == nil && parsed.Host == ""
}
```

OCI 레지스트리 URL은 `oci://` 프로토콜 없이 레지스트리 호스트명만 사용한다. (예: `ghcr.io/org/repo`). `url.Parse()` 시 `Host`가 비어 있으면 OCI 레지스트리로 판단한다.

### PullOCI()

```go
// util/helm/cmd.go

func (c *Cmd) PullOCI(repo string, chart string, version string, destination string, creds Creds) (string, error) {
    args := []string{
        "pull", fmt.Sprintf("oci://%s/%s", repo, chart),
        "--version", version,
        "--destination", destination,
    }
    // CA, 인증서, 키 파일 추가...
    if creds.GetInsecureSkipVerify() {
        args = append(args, "--insecure-skip-tls-verify")
    }
    out, _, err := c.run(context.Background(), args...)
    // ...
}
```

실제로 실행되는 명령:

```
helm pull oci://ghcr.io/org/mychart \
  --version 1.2.3 \
  --destination /tmp/helm-XXXX/
```

### util/oci 패키지 - 범용 OCI 클라이언트

`util/oci/client.go`는 Helm 차트 외 일반 OCI 아티팩트를 위한 범용 클라이언트를 제공한다.

```go
// util/oci/client.go

const (
    helmOCIConfigType = "application/vnd.cncf.helm.config.v1+json"
    helmOCILayerType  = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
)

type Client interface {
    ResolveRevision(ctx context.Context, revision string, noCache bool) (string, error)
    DigestMetadata(ctx context.Context, digest string) (*imagev1.Manifest, error)
    CleanCache(revision string) error
    Extract(ctx context.Context, revision string) (string, utilio.Closer, error)
    TestRepo(ctx context.Context) (bool, error)
    GetTags(ctx context.Context, noCache bool) ([]string, error)
}
```

`oras-go v2` 라이브러리를 사용하여 OCI Distribution Spec을 구현한다. 주요 상수는 Helm OCI 차트의 미디어 타입이다.

### OCI 인증

OCI 레지스트리 인증은 두 가지 경로를 통해 이루어진다.

| 방법 | 조건 |
|------|------|
| 명시적 크리덴셜 | `username`, `password`가 Repository 시크릿에 설정됨 → `helm registry login` |
| Docker 크리덴셜 스토어 폴백 | username/password 없음 → `credentials.NewStoreFromDocker()` (Docker 설정 파일) |

```go
// util/helm/client.go (GetTags() 내부)

if c.creds.GetUsername() == "" && helmPassword == "" {
    store, _ := credentials.NewStoreFromDocker(credentials.StoreOptions{})
    if store != nil {
        credential = credentials.Credential(store)
    }
}
```

---

## 9. Kustomize 통합 개요

Kustomize 통합은 `util/kustomize/` 패키지에 집중되어 있다.

```
ManifestRequest
      │
      ▼
GenerateManifests()     ← reposerver/repository/repository.go
      │
      ▼
kustomizeApp.Build()    ← util/kustomize/kustomize.go
      │
      ├─ kustomize edit set nameprefix
      ├─ kustomize edit set namesuffix
      ├─ kustomize edit set image
      ├─ kustomize edit set namespace
      ├─ kustomize edit add label
      ├─ kustomize edit add annotation
      ├─ kustomize edit add component
      ├─ kustomization.yaml 직접 수정 (patches)
      │
      └─ kustomize build {path}
             │
             └─ kube.SplitYAML() → []*unstructured.Unstructured
```

---

## 10. Kustomize Interface (util/kustomize/kustomize.go)

```go
// util/kustomize/kustomize.go

// Kustomize provides wrapper functionality around the `kustomize` command.
type Kustomize interface {
    // Build returns a list of unstructured objects from a `kustomize build` command
    // and extract supported parameters
    Build(
        opts *v1alpha1.ApplicationSourceKustomize,
        kustomizeOptions *v1alpha1.KustomizeOptions,
        envVars *v1alpha1.Env,
        buildOpts *BuildOpts,
    ) ([]*unstructured.Unstructured, []Image, []string, error)
}
```

반환값:
- `[]*unstructured.Unstructured`: 생성된 Kubernetes 오브젝트 목록
- `[]Image`: 매니페스트에서 추출한 컨테이너 이미지 목록 (UI 표시용)
- `[]string`: 실행된 kustomize 명령어 목록 (감사 로깅용)

구체 구현체:

```go
// util/kustomize/kustomize.go

type kustomize struct {
    repoRoot   string   // Git 레포지토리 루트 경로
    path       string   // 앱 소스 경로 (kustomize build 실행 디렉토리)
    creds      git.Creds
    repo       string   // Git 레포지토리 URL
    binaryPath string   // 커스텀 kustomize 바이너리 경로 (선택)
    proxy      string
    noProxy    string
}
```

### NewKustomizeApp()

```go
// util/kustomize/kustomize.go

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

`binaryPath`가 설정되면 `kustomize` 대신 해당 경로의 바이너리를 사용한다. 이는 여러 버전의 kustomize를 동시에 지원하기 위한 설계다.

### 버전 감지 캐싱

```go
// util/kustomize/kustomize.go

var (
    unknownVersion = semver.MustParse("v99.99.99")
    semVer         *semver.Version
    semVerLock     sync.Mutex
)

func getSemverSafe(ctx context.Context, k *kustomize) *semver.Version {
    if semVer == nil {
        semVerLock.Lock()
        defer semVerLock.Unlock()
        if ver, err := getSemver(ctx, k); err != nil {
            semVer = unknownVersion
            log.Warnf("Failed to parse kustomize version: %v", err)
        } else {
            semVer = ver
        }
    }
    return semVer
}
```

버전 파싱에 실패하면 `v99.99.99` (최신 버전으로 간주)를 반환한다. 이는 신규 버전에서 버전 출력 형식이 변경되어 파싱이 실패하는 경우를 안전하게 처리하기 위한 폴백이다.

---

## 11. ApplicationSourceKustomize

```go
// pkg/apis/application/v1alpha1/types.go

type ApplicationSourceKustomize struct {
    // namePrefix 오버라이드
    NamePrefix string `json:"namePrefix,omitempty"`
    // nameSuffix 오버라이드
    NameSuffix string `json:"nameSuffix,omitempty"`
    // 이미지 오버라이드 [old=]new[:tag|@digest]
    Images KustomizeImages `json:"images,omitempty"`
    // 공통 레이블 추가
    CommonLabels map[string]string `json:"commonLabels,omitempty"`
    // kustomize 버전 선택
    Version string `json:"version,omitempty"`
    // 공통 어노테이션 추가
    CommonAnnotations map[string]string `json:"commonAnnotations,omitempty"`
    // 기존 레이블 강제 덮어쓰기
    ForceCommonLabels bool `json:"forceCommonLabels,omitempty"`
    // 기존 어노테이션 강제 덮어쓰기
    ForceCommonAnnotations bool `json:"forceCommonAnnotations,omitempty"`
    // 네임스페이스 설정
    Namespace string `json:"namespace,omitempty"`
    // 어노테이션 값에 환경변수 치환 적용
    CommonAnnotationsEnvsubst bool `json:"commonAnnotationsEnvsubst,omitempty"`
    // 레플리카 오버라이드
    Replicas KustomizeReplicas `json:"replicas,omitempty"`
    // Strategic merge / JSON patch
    Patches KustomizePatches `json:"patches,omitempty"`
    // kustomize components
    Components []string `json:"components,omitempty"`
    // 누락된 컴포넌트 무시
    IgnoreMissingComponents bool `json:"ignoreMissingComponents,omitempty"`
    // 레이블을 셀렉터에 적용하지 않음
    LabelWithoutSelector bool `json:"labelWithoutSelector,omitempty"`
    // Helm 사용 시 kube-version 전달
    KubeVersion string `json:"kubeVersion,omitempty"`
    // Helm 사용 시 api-versions 전달
    APIVersions []string `json:"apiVersions,omitempty"`
    // 레이블을 템플릿에도 적용
    LabelIncludeTemplates bool `json:"labelIncludeTemplates,omitempty"`
}
```

### KustomizeImage 형식

```go
// pkg/apis/application/v1alpha1/types.go

// KustomizeImage represents a Kustomize image definition in the format [old_image_name=]<image_name>:<image_tag>
type KustomizeImage string

func (i KustomizeImage) delim() string {
    for _, d := range []string{"=", ":", "@"} {
        if strings.Contains(string(i), d) {
            return d
        }
    }
    return ":"
}
```

지원하는 이미지 오버라이드 형식:

| 형식 | 예시 | 의미 |
|------|------|------|
| `image:tag` | `nginx:1.21` | 태그 변경 |
| `old=new:tag` | `nginx=myregistry/nginx:1.21` | 이미지 전체 교체 |
| `image@digest` | `nginx@sha256:abc123` | 다이제스트로 고정 |

### Replicas 오버라이드

```go
// util/kustomize/kustomize.go (Build() 내부)

if len(opts.Replicas) > 0 {
    args := []string{"edit", "set", "replicas"}
    for _, replica := range opts.Replicas {
        count, err := replica.GetIntCount()
        // ...
        arg := fmt.Sprintf("%s=%d", replica.Name, count)
        args = append(args, arg)
    }
    cmd := exec.CommandContext(ctx, k.getBinaryPath(), args...)
    // → "kustomize edit set replicas my-deployment=3 my-statefulset=2"
}
```

---

## 12. Kustomize Build() 흐름

`Build()`는 kustomize 편집 명령들을 순서대로 실행한 뒤 최종 `kustomize build`를 실행한다.

### 전체 흐름

```
Build(opts, kustomizeOptions, envVars, buildOpts)
    │
    ├─ 1. 환경변수 준비
    │   ├─ os.Environ()
    │   ├─ envVars.Environ()   (ARGOCD_APP_* 변수)
    │   └─ creds.Environ()     (Git 크리덴셜 환경변수)
    │
    ├─ 2. HTTPS 레포면 CA 인증서 설정
    │   └─ GIT_SSL_CAINFO 환경변수
    │
    ├─ 3. kustomize edit 명령 순서대로 실행
    │   ├─ NamePrefix → "kustomize edit set nameprefix --"
    │   ├─ NameSuffix → "kustomize edit set namesuffix --"
    │   ├─ Images    → "kustomize edit set image ..."
    │   ├─ Replicas  → "kustomize edit set replicas ..."
    │   ├─ CommonLabels → "kustomize edit add label [--force] ..."
    │   ├─ CommonAnnotations → "kustomize edit add annotation [--force] ..."
    │   ├─ Namespace → "kustomize edit set namespace --"
    │   ├─ Patches   → kustomization.yaml 직접 수정
    │   └─ Components → "kustomize edit add component ..."
    │
    ├─ 4. kustomize build {path} 실행
    │   └─ buildOptions가 있으면 추가 플래그 포함
    │
    ├─ 5. kube.SplitYAML() → []*unstructured.Unstructured
    │
    └─ 6. getImageParameters() → 컨테이너 이미지 목록 추출
```

### Patches - kustomization.yaml 직접 수정

Patches는 `kustomize edit` 명령이 없으므로 `kustomization.yaml`을 직접 파싱하고 수정한다.

```go
// util/kustomize/kustomize.go

if len(opts.Patches) > 0 {
    kustFile := findKustomizeFile(k.path)
    kustomizationPath := filepath.Join(k.path, kustFile)
    b, err := os.ReadFile(kustomizationPath)
    // ...
    var kustomization any
    err = yaml.Unmarshal(b, &kustomization)
    kMap := kustomization.(map[string]any)

    patches, ok := kMap["patches"]
    if ok {
        // 기존 patches에 추가
        patchesList = append(patchesList, untypedPatches...)
        kMap["patches"] = patchesList
    } else {
        kMap["patches"] = opts.Patches
    }

    updatedKustomization, err := yaml.Marshal(kMap)
    err = os.WriteFile(kustomizationPath, updatedKustomization, kustomizationFileInfo.Mode())
}
```

### Components - 버전 체크

```go
// util/kustomize/kustomize.go

if len(opts.Components) > 0 {
    // components only supported in kustomize >= v3.7.0
    if getSemverSafe(ctx, k).LessThan(semver.MustParse("v3.7.0")) {
        return nil, nil, nil, errors.New("kustomize components require kustomize v3.7.0 and above")
    }
    // ...
}
```

### v3.8.5 호환성 패치

```go
// util/kustomize/kustomize.go

// kustomize v3.8.5 patch release introduced a breaking change in "edit add <label/annotation>" commands:
func mapToEditAddArgs(ctx context.Context, val map[string]string) []string {
    var args []string
    if getSemverSafe(ctx, &kustomize{}).LessThan(semver.MustParse("v3.8.5")) {
        // v3.8.5 이전: "key1:val1,key2:val2" 형태의 단일 인수
        arg := ""
        for labelName, labelValue := range val {
            if arg != "" { arg += "," }
            arg += fmt.Sprintf("%s:%s", labelName, labelValue)
        }
        args = append(args, arg)
    } else {
        // v3.8.5 이후: "key1:val1" "key2:val2" 형태의 개별 인수
        for labelName, labelValue := range val {
            args = append(args, fmt.Sprintf("%s:%s", labelName, labelValue))
        }
    }
    return args
}
```

### buildOptions와 Helm 통합 (kustomize --enable-helm)

```go
// util/kustomize/kustomize.go

func parseKustomizeBuildOptions(ctx context.Context, k *kustomize, buildOptions string, buildOpts *BuildOpts) []string {
    buildOptsParams := append([]string{"build", k.path}, strings.Fields(buildOptions)...)

    if buildOpts != nil && !getSemverSafe(ctx, k).LessThan(semver.MustParse("v5.3.0")) && isHelmEnabled(buildOptions) {
        if buildOpts.KubeVersion != "" {
            buildOptsParams = append(buildOptsParams, "--helm-kube-version", buildOpts.KubeVersion)
        }
        for _, v := range buildOpts.APIVersions {
            buildOptsParams = append(buildOptsParams, "--helm-api-versions", v)
        }
    }
    return buildOptsParams
}

func isHelmEnabled(buildOptions string) bool {
    return strings.Contains(buildOptions, "--enable-helm")
}
```

kustomize v5.3.0+에서 `--enable-helm` 플래그를 사용하면 kustomize가 내부적으로 Helm을 처리할 수 있다. 이 경우 `KubeVersion`과 `APIVersions`도 kustomize에 전달된다.

### kustomizationNames

```go
// util/kustomize/kustomize.go

var KustomizationNames = []string{"kustomization.yaml", "kustomization.yml", "Kustomization"}

func IsKustomization(path string) bool {
    return slices.Contains(KustomizationNames, path)
}

func findKustomizeFile(dir string) string {
    for _, file := range KustomizationNames {
        path := filepath.Join(dir, file)
        if _, err := os.Stat(path); err == nil {
            return file
        }
    }
    return ""
}
```

Kustomize 앱 감지는 디렉토리에 `kustomization.yaml`, `kustomization.yml`, `Kustomization` 중 하나가 있는지 확인한다.

---

## 13. Directory (Plain YAML)

소스 타입이 Helm도 Kustomize도 아닌 경우 `Directory` 타입으로 처리된다.

```go
// pkg/apis/application/v1alpha1/types.go

type ApplicationSourceDirectory struct {
    // 하위 디렉토리 재귀 탐색
    Recurse bool `json:"recurse,omitempty"`
    // Jsonnet 옵션
    Jsonnet ApplicationSourceJsonnet `json:"jsonnet,omitempty"`
    // 제외 글로브 패턴 (예: "config/**")
    Exclude string `json:"exclude,omitempty"`
    // 포함 글로브 패턴 (예: "*.yaml")
    Include string `json:"include,omitempty"`
}
```

### findManifests()

```go
// reposerver/repository/repository.go

var manifestFile = regexp.MustCompile(`^.*\.(yaml|yml|json|jsonnet)$`)

func findManifests(logCtx *log.Entry, appPath string, repoRoot string, env *v1alpha1.Env, directory v1alpha1.ApplicationSourceDirectory, enabledManifestGeneration map[string]bool, maxCombinedManifestQuantity resource.Quantity) ([]*unstructured.Unstructured, error) {
    potentiallyValidManifests, err := getPotentiallyValidManifests(logCtx, appPath, repoRoot, directory.Recurse, directory.Include, directory.Exclude, maxCombinedManifestQuantity)
    // ...
    for _, potentiallyValidManifest := range potentiallyValidManifests {
        if strings.HasSuffix(manifestFileInfo.Name(), ".jsonnet") {
            vm, err := makeJsonnetVM(appPath, repoRoot, directory.Jsonnet, env)
            jsonStr, err := vm.EvaluateFile(manifestPath)
            // JSON 파싱...
        } else {
            err := getObjsFromYAMLOrJSON(logCtx, manifestPath, manifestFileInfo.Name(), &objs)
        }
    }
    return objs, nil
}
```

| 확장자 | 처리 방법 |
|--------|-----------|
| `.yaml`, `.yml` | `kube.SplitYAML()` |
| `.json` | `json.Decode()` |
| `.jsonnet` | `go-jsonnet` VM 실행 |

### Jsonnet 지원

```go
// reposerver/repository/repository.go

func makeJsonnetVM(appPath string, repoRoot string, sourceJsonnet v1alpha1.ApplicationSourceJsonnet, env *v1alpha1.Env) (*jsonnet.VM, error) {
    vm := jsonnet.MakeVM()

    // 환경변수 치환 적용
    for i, j := range sourceJsonnet.TLAs {
        sourceJsonnet.TLAs[i].Value = env.Envsubst(j.Value)
    }
    for i, j := range sourceJsonnet.ExtVars {
        sourceJsonnet.ExtVars[i].Value = env.Envsubst(j.Value)
    }

    // TLA (Top-Level Arguments) 설정
    for _, arg := range sourceJsonnet.TLAs {
        if arg.Code {
            vm.TLACode(arg.Name, arg.Value)
        } else {
            vm.TLAVar(arg.Name, arg.Value)
        }
    }
    // ExtVars 설정...

    // 임포트 경로: appPath + 추가 Libs 경로
    jpaths := []string{appPath}
    for _, p := range sourceJsonnet.Libs {
        // ...
    }
    vm.Importer(&jsonnet.FileImporter{JPaths: jpaths})
    return vm, nil
}
```

`github.com/google/go-jsonnet` 라이브러리를 사용한다. Jsonnet은 JSON의 상위 집합으로, Kubernetes 매니페스트를 동적으로 생성하는 데 사용된다.

---

## 14. .argocd-source.yaml 오버라이드

소스 파라미터를 Git 레포지토리 내 파일로 오버라이드할 수 있다. 이를 통해 GitOps 방식으로 소스 파라미터를 관리할 수 있다.

```go
// reposerver/repository/repository.go

const (
    repoSourceFile = ".argocd-source.yaml"
    appSourceFile  = ".argocd-source-%s.yaml"
)
```

| 파일명 | 적용 범위 |
|--------|-----------|
| `.argocd-source.yaml` | 해당 경로의 모든 앱 |
| `.argocd-source-{appName}.yaml` | 특정 앱만 |

### mergeSourceParameters()

```go
// reposerver/repository/repository.go

func mergeSourceParameters(source *v1alpha1.ApplicationSource, path, appName string) error {
    repoFilePath := filepath.Join(path, repoSourceFile)
    overrides := []string{repoFilePath}
    if appName != "" {
        overrides = append(overrides, filepath.Join(path, fmt.Sprintf(appSourceFile, appName)))
    }

    merged := *source.DeepCopy()

    for _, filename := range overrides {
        // 파일 없으면 건너뜀
        info, err := os.Stat(filename)
        switch {
        case os.IsNotExist(err):
            continue
        case info != nil && info.IsDir():
            continue
        }

        // JSON merge patch 적용
        data, err := json.Marshal(merged)
        patch, err := os.ReadFile(filename)
        patch, err = yaml.YAMLToJSON(patch)
        data, err = jsonpatch.MergePatch(data, patch)
        err = json.Unmarshal(data, &merged)
    }

    // 보호 필드: 덮어쓰기 불가
    merged.Chart = source.Chart
    merged.Path = source.Path
    merged.RepoURL = source.RepoURL
    merged.TargetRevision = source.TargetRevision

    *source = merged
    return nil
}
```

### 보호 필드 (Protected Fields)

`mergeSourceParameters()` 마지막에 다음 네 필드는 원본 값으로 강제 복원된다.

| 보호 필드 | 이유 |
|-----------|------|
| `Chart` | 레포지토리 내부에서 사용할 차트를 변경하면 보안 위협 |
| `Path` | 임의의 경로로 소스를 바꿀 수 없도록 |
| `RepoURL` | 다른 레포지토리로의 소스 변경 방지 |
| `TargetRevision` | 특정 커밋/브랜치 고정 우회 방지 |

### 오버라이드 예시

```yaml
# .argocd-source.yaml (모든 앱에 적용)
helm:
  parameters:
    - name: global.env
      value: production

# .argocd-source-my-app.yaml (my-app에만 적용)
helm:
  releaseName: my-app-prod
  values: |
    replicaCount: 3
```

### 호출 위치

```go
// reposerver/repository/repository.go

func GetAppSourceType(ctx context.Context, source *v1alpha1.ApplicationSource, appPath, repoPath, appName string, ...) (v1alpha1.ApplicationSourceType, error) {
    err := mergeSourceParameters(source, appPath, appName)  // 오버라이드 적용
    // ...
    appSourceType, err := source.ExplicitType()
    // ...
}
```

`GetAppSourceType()`에서 소스 타입을 감지하기 전에 오버라이드가 먼저 적용된다.

---

## 15. 환경변수 주입

`newEnv()`는 매니페스트 생성 컨텍스트에서 사용 가능한 환경변수 집합을 생성한다.

```go
// reposerver/repository/repository.go

func newEnv(q *apiclient.ManifestRequest, revision string) *v1alpha1.Env {
    shortRevision := shortenRevision(revision, 7)   // 7자 단축
    shortRevision8 := shortenRevision(revision, 8)  // 8자 단축
    return &v1alpha1.Env{
        &v1alpha1.EnvEntry{Name: "ARGOCD_APP_NAME",                    Value: q.AppName},
        &v1alpha1.EnvEntry{Name: "ARGOCD_APP_NAMESPACE",               Value: q.Namespace},
        &v1alpha1.EnvEntry{Name: "ARGOCD_APP_PROJECT_NAME",            Value: q.ProjectName},
        &v1alpha1.EnvEntry{Name: "ARGOCD_APP_REVISION",                Value: revision},
        &v1alpha1.EnvEntry{Name: "ARGOCD_APP_REVISION_SHORT",          Value: shortRevision},
        &v1alpha1.EnvEntry{Name: "ARGOCD_APP_REVISION_SHORT_8",        Value: shortRevision8},
        &v1alpha1.EnvEntry{Name: "ARGOCD_APP_SOURCE_REPO_URL",         Value: q.Repo.Repo},
        &v1alpha1.EnvEntry{Name: "ARGOCD_APP_SOURCE_PATH",             Value: q.ApplicationSource.Path},
        &v1alpha1.EnvEntry{Name: "ARGOCD_APP_SOURCE_TARGET_REVISION",  Value: q.ApplicationSource.TargetRevision},
    }
}
```

### 환경변수 목록

| 환경변수 | 예시 값 | 설명 |
|---------|---------|------|
| `ARGOCD_APP_NAME` | `my-app` | 앱 이름 |
| `ARGOCD_APP_NAMESPACE` | `production` | 대상 네임스페이스 |
| `ARGOCD_APP_PROJECT_NAME` | `default` | 프로젝트 이름 |
| `ARGOCD_APP_REVISION` | `a1b2c3d4e5f6...` | 전체 Git 커밋 해시 |
| `ARGOCD_APP_REVISION_SHORT` | `a1b2c3d` | 7자 단축 커밋 해시 |
| `ARGOCD_APP_REVISION_SHORT_8` | `a1b2c3d4` | 8자 단축 커밋 해시 |
| `ARGOCD_APP_SOURCE_REPO_URL` | `https://github.com/org/repo` | 소스 레포지토리 URL |
| `ARGOCD_APP_SOURCE_PATH` | `charts/myapp` | 소스 경로 |
| `ARGOCD_APP_SOURCE_TARGET_REVISION` | `main` | 타겟 리비전 |

### Env.Environ()과 Env.Envsubst()

```go
// pkg/apis/application/v1alpha1/types.go

// Environ returns a list of environment variables in name=value format
func (e Env) Environ() []string {
    var environ []string
    for _, item := range e {
        if !item.IsZero() {
            environ = append(environ, fmt.Sprintf("%s=%s", item.Name, item.Value))
        }
    }
    return environ
}

// Envsubst interpolates variable references in a string from a list of variables
func (e Env) Envsubst(s string) string {
    valByEnv := map[string]string{}
    for _, item := range e {
        valByEnv[item.Name] = item.Value
    }
    // ${VAR} 또는 $VAR 형태 치환
    // ...
}
```

- `Environ()`: `cmd.Env = append(cmd.Env, env.Environ()...)` 형태로 kustomize/helm 프로세스에 전달
- `Envsubst()`: `--set image.tag=${ARGOCD_APP_REVISION_SHORT}` 같은 값의 변수 치환

### 사용 예시

```yaml
# Helm values에서 사용
helm:
  parameters:
    - name: image.tag
      value: "${ARGOCD_APP_REVISION_SHORT}"  # → a1b2c3d

# Kustomize image 오버라이드에서 사용
kustomize:
  images:
    - myapp:${ARGOCD_APP_REVISION}
```

```yaml
# Kustomize commonAnnotations에서 환경변수 사용 (commonAnnotationsEnvsubst: true 필요)
kustomize:
  commonAnnotationsEnvsubst: true
  commonAnnotations:
    deploy-revision: "${ARGOCD_APP_REVISION_SHORT}"
```

---

## 16. 왜(Why) 이런 설계인가

### CLI 래핑: 왜 Helm/Kustomize 라이브러리를 직접 임포트하지 않는가

Argo CD는 `helm` SDK나 `kustomize` 라이브러리를 Go 의존성으로 임포트하지 않는다. 대신 시스템 바이너리를 `os/exec`로 실행한다.

```
┌─────────────────────────────────────────────────┐
│ 방법 A: 라이브러리 임포트 (Argo CD가 택하지 않음)  │
│                                                 │
│  import helm "helm.sh/helm/v3/pkg"              │
│  helm.Template(...)                             │
│                                                 │
│  - Pro: 프로세스 오버헤드 없음                     │
│  - Con: Helm 버전 업그레이드 = Argo CD 재빌드 필요 │
│  - Con: Go 모듈 충돌 위험                        │
└─────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────┐
│ 방법 B: CLI 래핑 (Argo CD가 선택한 방법)           │
│                                                 │
│  exec.Command("helm", "template", ...)          │
│                                                 │
│  - Pro: Helm 버전 독립적 → 바이너리만 교체          │
│  - Pro: 여러 버전 동시 지원 가능 (binaryPath)       │
│  - Pro: Helm 업스트림 버그 패치 즉시 적용           │
│  - Con: exec 오버헤드 (~수십 ms)                  │
└─────────────────────────────────────────────────┘
```

Helm 생태계는 빠르게 변화하며, Argo CD 사용자들은 다양한 Helm 버전에 의존하는 차트를 사용한다. CLI 래핑으로 Argo CD 코드 변경 없이 `helm` 바이너리를 업그레이드하는 것이 가능하다.

### manifestGenerateLock: 왜 경로별 뮤텍스를 사용하는가

```
┌─────────────────────────────────────────────────────────┐
│ 단순 전역 뮤텍스 사용 시 문제점                            │
│                                                         │
│  Lock() → 앱A dep build → Unlock()                      │
│           앱B는 대기 중 (다른 경로인데도!)                  │
│                                                         │
│  결과: 불필요한 직렬화로 throughput 저하                    │
└─────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────┐
│ KeyLock (경로별 뮤텍스) 사용 시 이점                       │
│                                                         │
│  Lock("path/appA") ─────────────────── Unlock()         │
│  Lock("path/appB") ─────────────────── Unlock()         │
│  ↑ 동시 실행 가능                                         │
│                                                         │
│  Lock("path/appA") ─── 대기 ─── Lock() (동일 경로만 직렬) │
│                                                         │
│  결과: 다른 앱은 동시 처리, 같은 앱만 직렬화               │
└─────────────────────────────────────────────────────────┘
```

`sync.NewKeyLock()`은 `github.com/argoproj/pkg/v2/sync`의 구현으로, 키(경로) 별로 독립적인 뮤텍스를 관리한다.

### 마커 파일: 왜 `.argocd-helm-dep-up`을 사용하는가

```
문제: helm dependency build는 1~2초 소요
     같은 커밋에 대해 여러 앱이 동시에 매니페스트를 요청하면
     dep build가 반복 실행됨

해결: 마커 파일로 "이미 실행했음" 상태를 디스크에 기록

                 dep build 완료
                       │
                  마커 파일 생성
                  (.argocd-helm-dep-up)
                       │
              ┌────────┴────────┐
          고루틴 B          고루틴 C
       마커 파일 존재 확인   마커 파일 존재 확인
       → dep build 생략    → dep build 생략
```

마커 파일은 새로운 커밋이 처리될 때 (레포지토리 재초기화 시) 제거된다.

```go
// reposerver/repository/repository.go (코드 주석)
// the file is removed when repository is re-initialized
// (e.g. when another commit is processed)
```

### Kustomize edit vs. kustomization.yaml 직접 수정

| 설정 항목 | 적용 방법 |
|-----------|-----------|
| NamePrefix, NameSuffix | `kustomize edit set` 명령 |
| Images, Replicas | `kustomize edit set` 명령 |
| CommonLabels, CommonAnnotations | `kustomize edit add` 명령 |
| Namespace | `kustomize edit set namespace` 명령 |
| Patches | **kustomization.yaml 직접 수정** |
| Components | `kustomize edit add component` 명령 |

Patches에 해당하는 `kustomize edit` 명령이 없기 때문에 직접 파일을 수정한다. 이는 소스 코드 주석에도 명시되어 있다.

```go
// util/kustomize/kustomize.go
commands = append(commands, "# kustomization.yaml updated with patches. There is no `kustomize edit` command for adding patches.")
```

### Values vs ValuesObject 설계

```
Values (string)          ValuesObject (runtime.RawExtension)
     │                              │
     │                              │ JSON → YAML 변환
     │                              ▼
     └──────────────────► ValuesYAML() → []byte
                                    │
                                    ▼
                          임시 파일 (/tmp/uuid)
                                    │
                                    ▼
                    helm template --values /tmp/uuid
```

`Values`는 UI에서 raw YAML 문자열로 편집 가능하고, `ValuesObject`는 structured JSON으로 저장되어 API 검증이 가능하다. 두 가지를 모두 지원하는 이유는 하위 호환성 유지다.

---

## 17. 소스 타입 자동 감지

Argo CD는 명시적으로 소스 타입을 지정하지 않아도 자동으로 감지한다 (`util/app/discovery/discovery.go`).

```
AppType(ctx, appPath, repoPath, ...) 감지 순서:
    │
    ├─ Chart.yaml 존재? → Helm
    ├─ kustomization.yaml / .yml / Kustomization 존재? → Kustomize
    └─ 그 외 → Directory (Plain YAML / Jsonnet)
```

### 감지 우선순위

| 조건 | 소스 타입 |
|------|-----------|
| `Chart.yaml` 존재 | `Helm` |
| `kustomization.yaml` 존재 | `Kustomize` |
| `.jsonnet` 파일 존재 | `Directory` (Jsonnet 처리) |
| `.yaml`, `.yml`, `.json` 존재 | `Directory` (Plain YAML) |

### 명시적 타입 지정

```yaml
spec:
  source:
    # 명시적으로 지정하면 자동 감지를 건너뜀
    helm:
      releaseName: myapp
```

`source.ExplicitType()`은 `Helm`, `Kustomize`, `Directory`, `Plugin` 중 하나가 설정되어 있으면 자동 감지를 건너뛴다.

---

## 18. 전체 아키텍처 요약

```
                       Argo CD repo-server
┌──────────────────────────────────────────────────────────────┐
│                                                              │
│  GenerateManifests()                                         │
│       │                                                      │
│       ├─ newEnv() → ARGOCD_APP_* 환경변수                    │
│       │                                                      │
│       ├─ GetAppSourceType()                                  │
│       │   └─ mergeSourceParameters()                         │
│       │       ├─ .argocd-source.yaml       JSON merge patch  │
│       │       └─ .argocd-source-{app}.yaml JSON merge patch  │
│       │                                                      │
│       ├─ [Helm]                                              │
│       │   ├─ helmTemplate()                                  │
│       │   │   ├─ TemplateOpts 구성                           │
│       │   │   ├─ NewHelmApp() → helm 구조체                  │
│       │   │   ├─ h.Template() → "helm template ."           │
│       │   │   └─ IsMissingDependencyErr → runHelmBuild()     │
│       │   │                                                  │
│       │   └─ runHelmBuild()                                  │
│       │       ├─ manifestGenerateLock.Lock(appPath)          │
│       │       ├─ .argocd-helm-dep-up 마커 확인               │
│       │       └─ h.DependencyBuild() → "helm dep build"      │
│       │                                                      │
│       ├─ [Kustomize]                                         │
│       │   └─ kustomizeApp.Build()                            │
│       │       ├─ "kustomize edit set ..."  (순서대로)         │
│       │       ├─ kustomization.yaml 직접 수정 (patches)       │
│       │       └─ "kustomize build {path}"                   │
│       │                                                      │
│       └─ [Directory]                                         │
│           └─ findManifests()                                 │
│               ├─ YAML/JSON 파싱                              │
│               └─ Jsonnet VM 실행 (go-jsonnet)                │
│                                                              │
└──────────────────────────────────────────────────────────────┘

         util/helm/             util/kustomize/
┌─────────────────────┐  ┌─────────────────────────┐
│ Helm interface       │  │ Kustomize interface      │
│   Template()         │  │   Build()                │
│   DependencyBuild()  │  │                          │
│   GetParameters()    │  │ kustomize struct          │
│   Dispose()          │  │   binaryPath             │
│                      │  │   repoRoot               │
│ Cmd struct           │  │   path                   │
│   run() → exec()     │  │                          │
│   template()         │  │ Version check:           │
│   dependencyBuild()  │  │   getSemverSafe()        │
│   RegistryLogin()    │  │   v3.7.0 (components)    │
│   RepoAdd()          │  │   v3.8.5 (edit args)     │
│                      │  │   v5.3.0 (helm-kube-ver) │
│ Client interface     │  └─────────────────────────┘
│   ExtractChart()     │
│   GetIndex()         │
│   GetTags()          │
│   CleanChartCache()  │
│   TestHelmOCI()      │
└─────────────────────┘
```

---

## 참고 파일

| 파일 | 설명 |
|------|------|
| `/Users/ywlee/CNCF/argo-cd/util/helm/helm.go` | Helm 인터페이스 및 구현체 |
| `/Users/ywlee/CNCF/argo-cd/util/helm/cmd.go` | helm CLI 래퍼, TemplateOpts |
| `/Users/ywlee/CNCF/argo-cd/util/helm/client.go` | Helm 레포지토리 클라이언트 |
| `/Users/ywlee/CNCF/argo-cd/util/helm/creds.go` | Helm 크리덴셜 인터페이스 |
| `/Users/ywlee/CNCF/argo-cd/util/kustomize/kustomize.go` | Kustomize 인터페이스 및 구현체 |
| `/Users/ywlee/CNCF/argo-cd/util/oci/client.go` | OCI 범용 클라이언트 |
| `/Users/ywlee/CNCF/argo-cd/reposerver/repository/repository.go` | helmTemplate(), runHelmBuild(), mergeSourceParameters(), newEnv(), findManifests() |
| `/Users/ywlee/CNCF/argo-cd/pkg/apis/application/v1alpha1/types.go` | ApplicationSourceHelm, ApplicationSourceKustomize, ApplicationSourceDirectory |
