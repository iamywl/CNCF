# 13. CLI 커맨드 (pkg/cmd)

## 개요

Helm CLI는 [Cobra](https://github.com/spf13/cobra) 라이브러리 기반으로 구축된
커맨드 트리 구조를 가진다. 모든 CLI 커맨드는 `pkg/cmd` 패키지에 정의되며,
각 커맨드는 내부적으로 `pkg/action` 패키지의 Action 구조체를 호출하여 실제 작업을 수행한다.

이 아키텍처의 핵심 원칙:

1. **관심사 분리**: CLI(사용자 입력 파싱) vs Action(비즈니스 로직) 분리
2. **SDK 지원**: `pkg/action`은 CLI 없이도 Go 라이브러리로 사용 가능
3. **Cobra 활용**: 자동 도움말, 셸 자동완성, 플래그 파싱 등을 Cobra에 위임

## 아키텍처 전체 구조

```
main()                        cmd/helm/helm.go
  └── cmd.NewRootCmd()        pkg/cmd/root.go
        │
        ├── PersistentFlags    설정, 네임스페이스, kubeconfig 등
        │
        ├── 차트 커맨드
        │   ├── create         새 차트 스캐폴딩
        │   ├── dependency     의존성 관리 (build/update)
        │   ├── pull           차트 다운로드
        │   ├── show           차트 정보 표시 (all/chart/readme/values/crds)
        │   ├── lint           차트 검증
        │   ├── package        차트 패키징 (.tgz)
        │   ├── verify         프로비넌스 검증
        │   ├── template       템플릿 렌더링
        │   ├── repo           저장소 관리 (add/list/remove/update/index)
        │   └── search         차트 검색 (hub/repo)
        │
        ├── 릴리스 커맨드
        │   ├── install        차트 설치
        │   ├── upgrade        릴리스 업그레이드
        │   ├── rollback       이전 리비전으로 롤백
        │   ├── uninstall      릴리스 삭제
        │   ├── list           릴리스 목록
        │   ├── status         릴리스 상태 조회
        │   ├── history        릴리스 이력 조회
        │   ├── get            릴리스 세부정보 (all/hooks/manifest/notes/values/metadata)
        │   └── test           릴리스 테스트
        │
        ├── 레지스트리 커맨드
        │   ├── registry       레지스트리 관리 (login/logout)
        │   └── push           차트를 OCI 레지스트리에 Push
        │
        ├── 유틸리티 커맨드
        │   ├── completion     셸 자동완성 스크립트 생성
        │   ├── env            환경 정보 출력
        │   ├── plugin         플러그인 관리 (install/list/uninstall/update)
        │   ├── version        버전 정보 출력
        │   └── docs           문서 생성 (hidden)
        │
        └── 플러그인
            └── loadCLIPlugins()  외부 플러그인 로드
```

## NewRootCmd: 진입점

**소스**: `pkg/cmd/root.go` (라인 105~122)

```go
// pkg/cmd/root.go
func NewRootCmd(out io.Writer, args []string,
    logSetup func(bool)) (*cobra.Command, error) {
    actionConfig := action.NewConfiguration()
    cmd, err := newRootCmdWithConfig(actionConfig, out, args, logSetup)
    if err != nil {
        return nil, err
    }
    cobra.OnInitialize(func() {
        helmDriver := os.Getenv("HELM_DRIVER")
        if err := actionConfig.Init(settings.RESTClientGetter(),
            settings.Namespace(), helmDriver); err != nil {
            log.Fatal(err)
        }
        if helmDriver == "memory" {
            loadReleasesInMemory(actionConfig)
        }
        actionConfig.SetHookOutputFunc(hookOutputWriter)
    })
    return cmd, nil
}
```

### 초기화 순서

```
1. action.NewConfiguration() 생성
2. newRootCmdWithConfig() 호출
   ├── 2a. settings.AddFlags() - 전역 플래그 등록
   ├── 2b. addKlogFlags() - Kubernetes 로깅 플래그 (hidden)
   ├── 2c. flags.Parse(args) - 조기 파싱 (설정 수집)
   ├── 2d. logSetup(settings.Debug) - 로깅 설정
   ├── 2e. configureColorOutput() - 색상 출력 설정
   ├── 2f. newDefaultRegistryClient() - 레지스트리 클라이언트 생성
   ├── 2g. cmd.AddCommand(...) - 서브커맨드 등록
   └── 2h. loadCLIPlugins() - 플러그인 로드
3. cobra.OnInitialize() 등록
   └── 실행 시점에 actionConfig.Init() 호출
```

### 왜 OnInitialize를 사용하는가?

```
cobra.OnInitialize()는 cmd.Execute() 시 실행되지만
플래그 파싱 이후에 실행된다.

즉:
  flags.Parse(args)  ← 2단계에서 조기 파싱 (설정 수집용)
  cmd.Execute()      ← cobra가 정식 파싱 + OnInitialize 호출

이유: actionConfig.Init()은 namespace, helmDriver 등
      플래그 값이 필요하므로 플래그 파싱 이후에 실행해야 한다.
```

## 서브커맨드 등록

**소스**: `pkg/cmd/root.go` (라인 267~303)

```go
// pkg/cmd/root.go - newRootCmdWithConfig()
cmd.AddCommand(
    // 차트 커맨드
    newCreateCmd(out),
    newDependencyCmd(actionConfig, out),
    newPullCmd(actionConfig, out),
    newShowCmd(actionConfig, out),
    newLintCmd(out),
    newPackageCmd(out),
    newRepoCmd(out),
    newSearchCmd(out),
    newVerifyCmd(out),

    // 릴리스 커맨드
    newGetCmd(actionConfig, out),
    newHistoryCmd(actionConfig, out),
    newInstallCmd(actionConfig, out),
    newListCmd(actionConfig, out),
    newReleaseTestCmd(actionConfig, out),
    newRollbackCmd(actionConfig, out),
    newStatusCmd(actionConfig, out),
    newTemplateCmd(actionConfig, out),
    newUninstallCmd(actionConfig, out),
    newUpgradeCmd(actionConfig, out),

    newCompletionCmd(out),
    newEnvCmd(out),
    newPluginCmd(out),
    newVersionCmd(out),

    // Hidden
    newDocsCmd(out),
)

cmd.AddCommand(
    newRegistryCmd(actionConfig, out),
    newPushCmd(actionConfig, out),
)
```

### actionConfig 주입 여부

| 구분 | actionConfig 필요 | actionConfig 불필요 |
|------|-------------------|-------------------|
| 의미 | Kubernetes/릴리스 상호작용 | 로컬 전용 작업 |
| 커맨드 | install, upgrade, rollback, uninstall, list, status, history, get, test, template, pull, push, dependency, show, registry | create, lint, package, repo, search, verify, completion, env, plugin, version, docs |

## 커맨드 분류 상세

### 차트 관리 커맨드

| 커맨드 | 소스 파일 | 설명 |
|--------|----------|------|
| `helm create` | `create.go` | 차트 디렉토리 스캐폴딩 생성 |
| `helm dependency` | `dependency.go` | 차트 의존성 관리 |
| `helm dependency build` | `dependency_build.go` | charts/ 디렉토리에 의존성 빌드 |
| `helm dependency update` | `dependency_update.go` | Chart.lock 기반 의존성 업데이트 |
| `helm pull` | `pull.go` | 차트를 로컬로 다운로드 |
| `helm show` | `show.go` | 차트 정보 표시 |
| `helm show all` | `show.go` | 모든 정보 |
| `helm show chart` | `show.go` | Chart.yaml |
| `helm show readme` | `show.go` | README.md |
| `helm show values` | `show.go` | values.yaml |
| `helm show crds` | `show.go` | CRD 파일들 |
| `helm lint` | `lint.go` | 차트 구조/문법 검증 |
| `helm package` | `package.go` | 차트를 .tgz 아카이브로 패키징 |
| `helm verify` | `verify.go` | 프로비넌스 파일 검증 |

### 릴리스 관리 커맨드

| 커맨드 | 소스 파일 | 설명 |
|--------|----------|------|
| `helm install` | `install.go` | 차트를 클러스터에 설치 |
| `helm upgrade` | `upgrade.go` | 릴리스를 새 버전으로 업그레이드 |
| `helm rollback` | `rollback.go` | 이전 리비전으로 롤백 |
| `helm uninstall` | `uninstall.go` | 릴리스 삭제 |
| `helm list` | `list.go` | 릴리스 목록 조회 |
| `helm status` | `status.go` | 릴리스 상태 조회 |
| `helm history` | `history.go` | 릴리스 리비전 이력 |
| `helm get` | `get.go` | 릴리스 세부정보 조회 |
| `helm get all` | `get_all.go` | 모든 정보 |
| `helm get hooks` | `get_hooks.go` | Hook 매니페스트 |
| `helm get manifest` | `get_manifest.go` | 배포된 매니페스트 |
| `helm get notes` | `get_notes.go` | NOTES.txt |
| `helm get values` | `get_values.go` | 사용된 values |
| `helm get metadata` | `get_metadata.go` | 릴리스 메타데이터 |
| `helm test` | `release_testing.go` | 릴리스 테스트 실행 |
| `helm template` | `template.go` | 템플릿만 렌더링 (설치 없이) |

### 저장소/검색 커맨드

| 커맨드 | 소스 파일 | 설명 |
|--------|----------|------|
| `helm repo` | `repo.go` | 저장소 관리 |
| `helm repo add` | `repo_add.go` | 저장소 추가 |
| `helm repo list` | `repo_list.go` | 저장소 목록 |
| `helm repo remove` | `repo_remove.go` | 저장소 제거 |
| `helm repo update` | `repo_update.go` | 저장소 인덱스 갱신 |
| `helm repo index` | `repo_index.go` | 로컬 인덱스 생성 |
| `helm search` | `search.go` | 차트 검색 |
| `helm search hub` | `search_hub.go` | Artifact Hub 검색 |
| `helm search repo` | `search_repo.go` | 로컬 레포 검색 |

### 레지스트리 커맨드

| 커맨드 | 소스 파일 | 설명 |
|--------|----------|------|
| `helm registry login` | `registry_login.go` | OCI 레지스트리 로그인 |
| `helm registry logout` | `registry_logout.go` | OCI 레지스트리 로그아웃 |
| `helm push` | `push.go` | OCI 레지스트리에 차트 Push |

### 유틸리티 커맨드

| 커맨드 | 소스 파일 | 설명 |
|--------|----------|------|
| `helm completion` | `completion.go` | 셸 자동완성 스크립트 |
| `helm completion bash` | `completion.go` | Bash 자동완성 |
| `helm completion zsh` | `completion.go` | Zsh 자동완성 |
| `helm completion fish` | `completion.go` | Fish 자동완성 |
| `helm completion powershell` | `completion.go` | PowerShell 자동완성 |
| `helm env` | `env.go` | Helm 환경 변수 출력 |
| `helm plugin` | `plugin.go` | 플러그인 관리 |
| `helm plugin install` | `plugin_install.go` | 플러그인 설치 |
| `helm plugin list` | `plugin_list.go` | 플러그인 목록 |
| `helm plugin uninstall` | `plugin_uninstall.go` | 플러그인 제거 |
| `helm plugin update` | `plugin_update.go` | 플러그인 업데이트 |
| `helm version` | `version.go` | Helm 버전 정보 |
| `helm docs` | `docs.go` | 문서 생성 (hidden) |

## 커맨드 구현 패턴

### 전형적인 커맨드 구조

모든 Helm 서브커맨드는 동일한 패턴을 따른다:

```go
func newXxxCmd(actionConfig *action.Configuration, out io.Writer) *cobra.Command {
    // 1. Action 구조체 생성
    client := action.NewXxx(actionConfig)
    // 2. 추가 옵션 변수 선언
    var valuesOpts values.Options
    var outfmt output.Format

    // 3. cobra.Command 생성
    cmd := &cobra.Command{
        Use:   "xxx [RELEASE] [CHART]",
        Short: "간단한 설명",
        Long:  xxxDesc,  // 상세 설명 (상수)
        Args:  require.ExactArgs(2),
        ValidArgsFunction: func(...) {...},  // 셸 자동완성
        RunE: func(cmd *cobra.Command, args []string) error {
            // 4. 플래그 값 → Action 필드 매핑
            client.ReleaseName = args[0]
            // 5. Action 실행
            rel, err := client.Run(chart, vals)
            // 6. 결과 출력
            return outfmt.Write(out, ...)
        },
    }

    // 7. 플래그 등록
    f := cmd.Flags()
    f.BoolVar(&client.Wait, "wait", false, "wait until ready")
    addValueOptionsFlags(f, &valuesOpts)
    addChartPathOptionsFlags(f, &client.ChartPathOptions)
    bindOutputFlag(cmd, &outfmt)
    bindPostRenderFlag(cmd, &client.PostRenderer, settings)

    return cmd
}
```

### 왜 이 패턴인가?

1. **Action과 CLI 분리**: `action.NewXxx()`로 비즈니스 로직 생성,
   CLI는 플래그 파싱과 출력만 담당
2. **Args 검증**: `require.ExactArgs(N)`으로 인자 수 검증
3. **RunE 사용**: 에러를 반환할 수 있는 `RunE` (Run 대신)를 사용하여
   Cobra가 에러를 적절히 처리
4. **셸 자동완성**: `ValidArgsFunction`으로 동적 자동완성 지원

## 공통 플래그 시스템

**소스**: `pkg/cmd/flags.go`

### Value 옵션 플래그

```go
// pkg/cmd/flags.go
func addValueOptionsFlags(f *pflag.FlagSet, v *values.Options) {
    f.StringSliceVarP(&v.ValueFiles, "values", "f", []string{},
        "specify values in a YAML file or a URL")
    f.StringArrayVar(&v.Values, "set", []string{},
        "set values on the command line")
    f.StringArrayVar(&v.StringValues, "set-string", []string{},
        "set STRING values on the command line")
    f.StringArrayVar(&v.FileValues, "set-file", []string{},
        "set values from respective files")
    f.StringArrayVar(&v.JSONValues, "set-json", []string{},
        "set JSON values on the command line")
    f.StringArrayVar(&v.LiteralValues, "set-literal", []string{},
        "set a literal STRING value on the command line")
}
```

| 플래그 | 축약 | 용도 | 예시 |
|--------|------|------|------|
| `--values` | `-f` | YAML 파일에서 값 로드 | `-f values.yaml` |
| `--set` | - | 키=값 쌍 설정 | `--set image.tag=v1.0` |
| `--set-string` | - | 문자열 강제 | `--set-string num=1234567890` |
| `--set-file` | - | 파일 내용을 값으로 | `--set-file script=init.sh` |
| `--set-json` | - | JSON 객체 설정 | `--set-json 'a=[1,2]'` |
| `--set-literal` | - | 리터럴 문자열 (이스케이프 없이) | `--set-literal password=p@$$w0rd!` |

### 값 우선순위

```
우선순위 (높은 것이 낮은 것을 덮어씀):
  --set-literal > --set-json > --set-file > --set-string > --set > --values (-f)

여러 -f 파일 지정 시: 오른쪽 파일이 왼쪽을 덮어씀
  -f base.yaml -f override.yaml → override.yaml이 우선
```

### Chart Path 옵션 플래그

```go
// pkg/cmd/flags.go
func addChartPathOptionsFlags(f *pflag.FlagSet, c *action.ChartPathOptions) {
    f.StringVar(&c.Version, "version", "", "chart version constraint")
    f.BoolVar(&c.Verify, "verify", false, "verify the package")
    f.StringVar(&c.Keyring, "keyring", defaultKeyring(), "public keys location")
    f.StringVar(&c.RepoURL, "repo", "", "chart repository url")
    f.StringVar(&c.Username, "username", "", "repository username")
    f.StringVar(&c.Password, "password", "", "repository password")
    f.StringVar(&c.CertFile, "cert-file", "", "SSL certificate file")
    f.StringVar(&c.KeyFile, "key-file", "", "SSL key file")
    f.BoolVar(&c.InsecureSkipTLSVerify, "insecure-skip-tls-verify", false,
        "skip tls certificate checks")
    f.BoolVar(&c.PlainHTTP, "plain-http", false, "use HTTP connections")
    f.StringVar(&c.CaFile, "ca-file", "", "CA bundle for HTTPS verification")
    f.BoolVar(&c.PassCredentialsAll, "pass-credentials", false,
        "pass credentials to all domains")
}
```

### Wait 플래그

```go
// pkg/cmd/flags.go
func AddWaitFlag(cmd *cobra.Command, wait *kube.WaitStrategy) {
    cmd.Flags().Var(
        newWaitValue(kube.HookOnlyStrategy, wait),
        "wait",
        "wait until resources are ready. Use '--wait' alone for 'watcher' strategy")
    cmd.Flags().Lookup("wait").NoOptDefVal = string(kube.StatusWatcherStrategy)
}
```

`--wait` 플래그의 동작:

```
--wait 미지정     → hookOnly (Hook만 대기)
--wait            → watcher  (NoOptDefVal, kstatus 기반)
--wait=watcher    → watcher
--wait=legacy     → legacy   (Helm 3 스타일 폴링)
--wait=hookOnly   → hookOnly
--wait=true       → watcher  (deprecated, 경고 출력)
--wait=false      → hookOnly (deprecated, 경고 출력)
```

### Output 플래그

```go
func bindOutputFlag(cmd *cobra.Command, varRef *output.Format) {
    cmd.Flags().VarP(newOutputValue(output.Table, varRef), outputFlag, "o",
        "prints the output in the specified format")
}
```

지원 형식: `table`, `json`, `yaml`

### Post Renderer 플래그

```go
func bindPostRenderFlag(cmd *cobra.Command,
    varRef *postrenderer.PostRenderer, settings *cli.EnvSettings) {
    p := &postRendererOptions{varRef, "", []string{}, settings}
    cmd.Flags().Var(&postRendererString{p}, postRenderFlag,
        "postrenderer type plugin to be used")
    cmd.Flags().Var(&postRendererArgsSlice{p}, postRenderArgsFlag,
        "argument to the post-renderer")
}
```

Post Renderer는 렌더링된 매니페스트를 후처리하는 플러그인이다.
Kustomize, yq 등을 연동할 수 있다.

## 전역 플래그와 환경 변수

**소스**: `pkg/cmd/root.go` (라인 48~101)

```go
var settings = cli.New()
```

`cli.EnvSettings`는 전역 설정을 관리한다. 각 설정은 플래그와 환경 변수로 제어된다:

| 환경 변수 | 플래그 | 설명 |
|----------|--------|------|
| `$HELM_NAMESPACE` | `--namespace, -n` | Kubernetes 네임스페이스 |
| `$KUBECONFIG` | `--kubeconfig` | kubeconfig 파일 경로 |
| `$HELM_KUBECONTEXT` | `--kube-context` | kubeconfig 컨텍스트 |
| `$HELM_DRIVER` | - | 저장소 드라이버 (secret/configmap/memory/sql) |
| `$HELM_DEBUG` | `--debug` | 디버그 모드 |
| `$HELM_REGISTRY_CONFIG` | `--registry-config` | 레지스트리 설정 파일 |
| `$HELM_REPOSITORY_CACHE` | `--repository-cache` | 저장소 캐시 디렉토리 |
| `$HELM_REPOSITORY_CONFIG` | `--repository-config` | 저장소 설정 파일 |
| `$HELM_MAX_HISTORY` | - | 릴리스 이력 최대 수 |
| `$HELM_NO_PLUGINS` | - | 플러그인 비활성화 |
| `$HELM_PLUGINS` | - | 플러그인 디렉토리 |
| `$HELM_BURST_LIMIT` | `--burst-limit` | API 요청 버스트 제한 |
| `$HELM_QPS` | `--qps` | API 초당 요청 수 |
| `$HELM_COLOR` | `--color` | 색상 모드 (never/auto/always) |

### 색상 출력 설정

```go
// pkg/cmd/root.go
func configureColorOutput(settings *cli.EnvSettings) {
    switch settings.ColorMode {
    case "never":
        color.NoColor = true
    case "always":
        color.NoColor = false
    case "auto":
        // fatih/color가 터미널 자동 감지
    }
}
```

`$NO_COLOR` 환경 변수가 설정되면 `$HELM_COLOR`를 무시하고 색상을 비활성화한다.

## 셸 자동완성

**소스**: `pkg/cmd/completion.go`

```go
const bashCompDesc = `
Generate the autocompletion script for Helm for the bash shell.

To load completions in your current shell session:
    source <(helm completion bash)
`
const zshCompDesc = `
Generate the autocompletion script for Helm for the zsh shell.

To load completions in your current shell session:
    source <(helm completion zsh)
`
```

### 지원 셸

| 셸 | 커맨드 | 설치 방법 |
|----|--------|----------|
| Bash | `helm completion bash` | `source <(helm completion bash)` |
| Zsh | `helm completion zsh` | `helm completion zsh > "${fpath[1]}/_helm"` |
| Fish | `helm completion fish` | `helm completion fish \| source` |
| PowerShell | `helm completion powershell` | `helm completion powershell \| Out-String \| Invoke-Expression` |

### 동적 자동완성

셸 자동완성은 정적 플래그 완성 외에도 동적 완성을 지원한다:

#### 네임스페이스 자동완성

```go
// pkg/cmd/root.go
cmd.RegisterFlagCompletionFunc("namespace",
    func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
        if client, err := actionConfig.KubernetesClientSet(); err == nil {
            to := int64(3)  // 3초 타임아웃
            if namespaces, err := client.CoreV1().Namespaces().List(
                context.Background(),
                metav1.ListOptions{TimeoutSeconds: &to}); err == nil {
                nsNames := []string{}
                for _, ns := range namespaces.Items {
                    nsNames = append(nsNames, ns.Name)
                }
                return nsNames, cobra.ShellCompDirectiveNoFileComp
            }
        }
        return nil, cobra.ShellCompDirectiveDefault
    })
```

#### kube-context 자동완성

```go
cmd.RegisterFlagCompletionFunc("kube-context",
    func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
        loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
        if config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
            loadingRules, &clientcmd.ConfigOverrides{}).RawConfig(); err == nil {
            comps := []string{}
            for name, context := range config.Contexts {
                comps = append(comps,
                    fmt.Sprintf("%s\t%s", name, context.Cluster))
            }
            return comps, cobra.ShellCompDirectiveNoFileComp
        }
        return nil, cobra.ShellCompDirectiveNoFileComp
    })
```

#### 차트 버전 자동완성

```go
// pkg/cmd/flags.go
func compVersionFlag(chartRef string, _ string) ([]string, cobra.ShellCompDirective) {
    chartInfo := strings.Split(chartRef, "/")
    if len(chartInfo) != 2 {
        return nil, cobra.ShellCompDirectiveNoFileComp
    }
    repoName := chartInfo[0]
    chartName := chartInfo[1]

    path := filepath.Join(settings.RepositoryCache,
        helmpath.CacheIndexFile(repoName))

    var versions []string
    if indexFile, err := repo.LoadIndexFile(path); err == nil {
        for _, details := range indexFile.Entries[chartName] {
            versions = append(versions,
                fmt.Sprintf("%s\t%s%s%s",
                    details.Version,
                    appVersionDesc,
                    createdDesc,
                    deprecated))
        }
    }
    return versions, cobra.ShellCompDirectiveNoFileComp
}
```

## install 커맨드 상세

**소스**: `pkg/cmd/install.go`

```
helm install [NAME] [CHART] [flags]

주요 플래그:
  --values/-f        YAML 값 파일
  --set              키=값 쌍
  --namespace/-n     네임스페이스
  --create-namespace 네임스페이스 자동 생성
  --wait             리소스 준비 대기
  --timeout          대기 시간제한
  --dry-run          시뮬레이션 (none/client/server)
  --server-side-apply SSA 사용 (기본 true)
  --force-conflicts  SSA 충돌 강제 해결
  --version          차트 버전
  --description      릴리스 설명
  --rollback-on-failure 실패 시 롤백
```

### install 커맨드 → Action 매핑

```
cobra.Command.RunE
  │
  ├── 차트 로드: loader.Load(chartPath)
  │
  ├── 의존성 업데이트 (--dependency-update)
  │   └── downloader.Manager.Build()
  │
  ├── 값 머지: valuesOpts.MergeValues(getter.All(settings))
  │
  ├── Action 실행: client.RunWithContext(ctx, chart, vals)
  │   └── action.Install.RunWithContext()
  │       ├── 클러스터 연결 확인
  │       ├── 이름 검증
  │       ├── CRD 설치
  │       ├── 템플릿 렌더링
  │       ├── 리소스 빌드
  │       ├── 릴리스 저장
  │       └── 리소스 생성 + Hook 실행
  │
  └── 결과 출력
      └── PrintRelease(out, rel)
```

## upgrade 커맨드의 install 폴백

**소스**: `pkg/cmd/upgrade.go`

```go
// upgrade에서 --install 옵션 사용 시
if client.Install {
    // 릴리스가 없으면 install로 전환
    instClient := action.NewInstall(actionConfig)
    // upgrade의 설정을 install에 복사
    instClient.ServerSideApply = ...
    instClient.DryRunStrategy = ...
    instClient.WaitStrategy = ...
    // ...
    rel, err = instClient.RunWithContext(ctx, chartRequested, vals)
}
```

`helm upgrade --install`은 릴리스가 존재하면 업그레이드, 없으면 설치를 수행한다.
이 패턴은 CI/CD에서 멱등성(idempotency)을 보장하는 데 유용하다.

## 로깅 설정

**소스**: `pkg/cmd/root.go` (라인 124~134)

```go
func SetupLogging(debug bool) {
    logger := logging.NewLogger(func() bool { return debug })
    slog.SetDefault(logger)
}
```

Helm v4는 Go 표준 `log/slog`를 사용한다:

```
--debug 미지정: WARN 이상만 출력
--debug:        DEBUG 이상 출력 (HTTP 요청/응답 포함)
```

### klog 플래그 숨김

```go
// pkg/cmd/flags.go
func addKlogFlags(fs *pflag.FlagSet) {
    local := flag.NewFlagSet("klog", flag.ExitOnError)
    klog.InitFlags(local)
    local.VisitAll(func(fl *flag.Flag) {
        fl.Name = normalize(fl.Name)
        newflag := pflag.PFlagFromGoFlag(fl)
        newflag.Hidden = true  // 도움말에 표시하지 않음
        fs.AddFlag(newflag)
    })
}
```

Kubernetes client-go가 사용하는 klog 플래그를 등록하되 hidden으로 설정하여
`helm --help` 출력을 깔끔하게 유지한다.

## 플러그인 시스템

**소스**: `pkg/cmd/load_plugins.go`

```go
func loadCLIPlugins(cmd *cobra.Command, out io.Writer) {
    // $HELM_PLUGINS 또는 기본 플러그인 디렉토리에서 로드
}
```

플러그인은 별도의 실행 파일로 구현되며, Helm이 서브커맨드로 자동 등록한다:

```
$HELM_PLUGINS/
├── helm-diff/
│   ├── plugin.yaml
│   └── bin/helm-diff
├── helm-secrets/
│   ├── plugin.yaml
│   └── bin/helm-secrets
```

## 만료된 저장소 확인

**소스**: `pkg/cmd/root.go` (라인 357~402)

```go
func checkForExpiredRepos(repofile string) {
    expiredRepos := []struct {
        name string
        old  string
        new  string
    }{
        {name: "stable",
         old: "kubernetes-charts.storage.googleapis.com",
         new: "https://charts.helm.sh/stable"},
        {name: "incubator",
         old: "kubernetes-charts-incubator.storage.googleapis.com",
         new: "https://charts.helm.sh/incubator"},
    }
    // 구 URL 사용 시 마이그레이션 경고 출력
}
```

Google이 호스팅하던 구 stable/incubator 레포가 만료되었을 때 경고를 표시한다.

## Memory 드라이버 지원

**소스**: `pkg/cmd/root.go` (라인 314~350)

```go
func loadReleasesInMemory(actionConfig *action.Configuration) {
    filePaths := strings.Split(os.Getenv("HELM_MEMORY_DRIVER_DATA"), ":")
    store := actionConfig.Releases
    mem, ok := store.Driver.(*driver.Memory)
    // ...
    for _, path := range filePaths {
        b, _ := os.ReadFile(path)
        releases := []*release.Release{}
        yaml.Unmarshal(b, &releases)
        for _, rel := range releases {
            store.Create(rel)
        }
    }
    mem.SetNamespace(settings.Namespace())
}
```

`HELM_DRIVER=memory`와 `HELM_MEMORY_DRIVER_DATA`를 조합하면
파일 기반의 인메모리 릴리스 저장소를 사용할 수 있다. 테스트와 CI에서 유용하다.

## CommandError: 종료 코드 제어

```go
// pkg/cmd/root.go
type CommandError struct {
    error
    ExitCode int
}
```

특정 에러에 대해 커스텀 종료 코드를 반환할 수 있다.
기본 에러는 종료 코드 1을 반환하지만, CommandError를 사용하면 다른 코드를 지정할 수 있다.

## 커맨드 트리 시각화

```
helm
├── create         ─── 로컬 전용
├── dependency
│   ├── build      ─── 로컬 + 네트워크 (저장소 다운로드)
│   └── update     ─── 로컬 + 네트워크
├── env            ─── 로컬 전용
├── get
│   ├── all        ─── Kubernetes 필요
│   ├── hooks      ─── Kubernetes 필요
│   ├── manifest   ─── Kubernetes 필요
│   ├── metadata   ─── Kubernetes 필요
│   ├── notes      ─── Kubernetes 필요
│   └── values     ─── Kubernetes 필요
├── history        ─── Kubernetes 필요
├── install        ─── Kubernetes 필요 (--dry-run=client 제외)
├── lint           ─── 로컬 전용
├── list           ─── Kubernetes 필요
├── package        ─── 로컬 전용
├── plugin
│   ├── install    ─── 네트워크 (플러그인 다운로드)
│   ├── list       ─── 로컬 전용
│   ├── uninstall  ─── 로컬 전용
│   └── update     ─── 네트워크
├── pull           ─── 네트워크 (차트 다운로드)
├── push           ─── 네트워크 (OCI Push)
├── registry
│   ├── login      ─── 네트워크
│   └── logout     ─── 로컬 전용
├── repo
│   ├── add        ─── 네트워크
│   ├── index      ─── 로컬 전용
│   ├── list       ─── 로컬 전용
│   ├── remove     ─── 로컬 전용
│   └── update     ─── 네트워크
├── rollback       ─── Kubernetes 필요
├── search
│   ├── hub        ─── 네트워크 (Artifact Hub API)
│   └── repo       ─── 로컬 전용 (캐시 사용)
├── show
│   ├── all        ─── 로컬/네트워크 (차트 위치에 따라)
│   ├── chart      ─── 〃
│   ├── crds       ─── 〃
│   ├── readme     ─── 〃
│   └── values     ─── 〃
├── status         ─── Kubernetes 필요
├── template       ─── 로컬 전용 (기본)
├── test           ─── Kubernetes 필요
├── uninstall      ─── Kubernetes 필요
├── upgrade        ─── Kubernetes 필요
├── verify         ─── 로컬 전용
├── version        ─── 로컬 전용
└── completion     ─── 로컬 전용
```

## 핵심 설계 원칙 요약

| 원칙 | 구현 |
|------|------|
| **관심사 분리** | pkg/cmd(CLI) ↔ pkg/action(로직) |
| **SDK 지원** | action 패키지는 CLI 없이 Go 라이브러리로 사용 가능 |
| **Cobra 활용** | 자동 도움말, 셸 완성, 플래그 파싱 위임 |
| **동적 완성** | 네임스페이스, 컨텍스트, 버전 자동완성 |
| **환경 변수 통합** | 모든 주요 설정은 플래그 + 환경 변수로 제어 |
| **점진적 초기화** | OnInitialize로 플래그 파싱 후 Config 초기화 |
| **일관된 패턴** | 모든 커맨드가 동일한 구조 패턴 따름 |

## 관련 소스 파일 요약

| 파일 | 핵심 내용 |
|------|----------|
| `pkg/cmd/root.go` | NewRootCmd, 서브커맨드 등록, 로깅 설정, 레지스트리 클라이언트 |
| `pkg/cmd/flags.go` | 공통 플래그 (values, chartPath, wait, output, postRenderer) |
| `pkg/cmd/install.go` | helm install 커맨드 |
| `pkg/cmd/upgrade.go` | helm upgrade 커맨드 (--install 폴백 포함) |
| `pkg/cmd/rollback.go` | helm rollback 커맨드 |
| `pkg/cmd/uninstall.go` | helm uninstall 커맨드 |
| `pkg/cmd/list.go` | helm list 커맨드 |
| `pkg/cmd/status.go` | helm status 커맨드 |
| `pkg/cmd/history.go` | helm history 커맨드 |
| `pkg/cmd/pull.go` | helm pull 커맨드 |
| `pkg/cmd/push.go` | helm push 커맨드 |
| `pkg/cmd/completion.go` | 셸 자동완성 생성 |
| `pkg/cmd/repo.go` | helm repo 부모 커맨드 |
| `pkg/cmd/search.go` | helm search 부모 커맨드 |
| `pkg/cmd/template.go` | helm template 커맨드 |
| `pkg/cmd/load_plugins.go` | 플러그인 로더 |
| `pkg/cmd/helpers.go` | 출력 헬퍼, 차트 로더 |
| `pkg/cmd/printer.go` | 릴리스 출력 포맷터 |
