# Helm v4 핵심 컴포넌트

## 1. 개요

Helm v4의 핵심 컴포넌트는 6개로 구성된다.

```
┌─────────────────────────────────────────────────────────────┐
│                       Configuration                         │
│  (의존성 주입 컨테이너 — 모든 Action의 공유 의존성)           │
│                                                             │
│  ┌──────────┐  ┌─────────┐  ┌───────────┐  ┌────────────┐  │
│  │  Engine   │  │ Storage │  │ KubeClient│  │ Registry   │  │
│  │ (렌더링)  │  │ (저장소)│  │ (K8s API) │  │  Client    │  │
│  └──────────┘  └─────────┘  └───────────┘  └────────────┘  │
└─────────────────────────────────────────────────────────────┘
                         │
              ┌──────────▼──────────┐
              │     ChartLoader     │
              │ (차트 로딩/파싱)     │
              └─────────────────────┘
```

## 2. Configuration -- 의존성 주입 컨테이너

**소스 파일:** `pkg/action/action.go`

### 구조체 정의

```go
// pkg/action/action.go
type Configuration struct {
    // RESTClientGetter is an interface that loads Kubernetes clients.
    RESTClientGetter RESTClientGetter

    // Releases stores records of releases.
    Releases *storage.Storage

    // KubeClient is a Kubernetes API client.
    KubeClient kube.Interface

    // RegistryClient is a client for working with registries
    RegistryClient *registry.Client

    // Capabilities describes the capabilities of the Kubernetes cluster.
    Capabilities *common.Capabilities

    // CustomTemplateFuncs is defined by users to provide custom template funcs
    CustomTemplateFuncs template.FuncMap

    // HookOutputFunc called with container name and returns and expects writer
    // that will receive the log output.
    HookOutputFunc func(namespace, pod, container string) io.Writer

    // Mutex is an exclusive lock for concurrent access to the action
    mutex sync.Mutex

    // Embed a LogHolder to provide logger functionality
    logging.LogHolder
}
```

Configuration은 Helm의 모든 Action(Install, Upgrade, Rollback 등)이 공유하는 의존성 주입(DI) 컨테이너이다. 각 Action은 생성 시 `*Configuration`을 받아 필요한 의존성에 접근한다.

### 핵심 필드 역할

| 필드 | 타입 | 역할 |
|------|------|------|
| `RESTClientGetter` | `RESTClientGetter` 인터페이스 | Kubernetes REST 클라이언트 팩토리 (kubeconfig 로드) |
| `Releases` | `*storage.Storage` | 릴리스 CRUD 스토리지 |
| `KubeClient` | `kube.Interface` | Kubernetes 리소스 생성/수정/삭제 |
| `RegistryClient` | `*registry.Client` | OCI 레지스트리 Push/Pull |
| `Capabilities` | `*common.Capabilities` | K8s 버전, API 버전 정보 (템플릿 `.Capabilities`에 노출) |
| `CustomTemplateFuncs` | `template.FuncMap` | 사용자 정의 템플릿 함수 |
| `mutex` | `sync.Mutex` | 동시 접근 보호 (goroutine-safe) |
| `LogHolder` | `logging.LogHolder` | 구조화 로깅 (`slog` 기반) |

### Init() 메서드 -- 초기화 흐름

```go
// pkg/action/action.go
func (cfg *Configuration) Init(getter genericclioptions.RESTClientGetter,
    namespace, helmDriver string) error {

    kc := kube.New(getter)
    kc.SetLogger(cfg.Logger().Handler())

    lazyClient := &lazyClient{
        namespace: namespace,
        clientFn:  kc.Factory.KubernetesClientSet,
    }

    var store *storage.Storage
    switch helmDriver {
    case "secret", "secrets", "":
        d := driver.NewSecrets(newSecretClient(lazyClient))
        d.SetLogger(cfg.Logger().Handler())
        store = storage.Init(d)
    case "configmap", "configmaps":
        d := driver.NewConfigMaps(newConfigMapClient(lazyClient))
        d.SetLogger(cfg.Logger().Handler())
        store = storage.Init(d)
    case "memory":
        d := driver.NewMemory()
        d.SetLogger(cfg.Logger().Handler())
        d.SetNamespace(namespace)
        store = storage.Init(d)
    case "sql":
        d, err := driver.NewSQL(
            os.Getenv("HELM_DRIVER_SQL_CONNECTION_STRING"),
            namespace,
        )
        if err != nil {
            return fmt.Errorf("unable to instantiate SQL driver: %w", err)
        }
        d.SetLogger(cfg.Logger().Handler())
        store = storage.Init(d)
    default:
        return fmt.Errorf("unknown driver %q", helmDriver)
    }

    cfg.RESTClientGetter = getter
    cfg.KubeClient = kc
    cfg.Releases = store
    cfg.HookOutputFunc = func(_, _, _ string) io.Writer { return io.Discard }

    return nil
}
```

**초기화 흐름:**

```
Init(getter, namespace, helmDriver)
  │
  ├─ 1. kube.New(getter) → KubeClient 생성
  │
  ├─ 2. lazyClient 생성 → K8s ClientSet 지연 초기화
  │
  ├─ 3. helmDriver 분기 → Storage 드라이버 선택
  │     ├─ "secret" (기본값) → driver.NewSecrets()
  │     ├─ "configmap"       → driver.NewConfigMaps()
  │     ├─ "memory"          → driver.NewMemory()
  │     └─ "sql"             → driver.NewSQL()
  │
  ├─ 4. storage.Init(d) → Storage 생성
  │
  └─ 5. cfg에 모든 의존성 주입 완료
```

**왜 lazyClient를 사용하는가?** Kubernetes 클라이언트 생성은 kubeconfig 파일 읽기, TLS 핸드셰이크 등 비용이 크다. `lazyClient`는 실제로 K8s API를 호출할 때까지 클라이언트 생성을 지연시켜, `helm list` 같은 명령이 불필요한 API 호출을 하지 않도록 한다.

### renderResources() -- 템플릿 렌더링 오케스트레이션

```go
// pkg/action/action.go
func (cfg *Configuration) renderResources(ch *chart.Chart, values common.Values,
    releaseName, outputDir string, subNotes, useReleaseName, includeCrds bool,
    pr postrenderer.PostRenderer, interactWithRemote, enableDNS, hideSecret bool,
) ([]*release.Hook, *bytes.Buffer, string, error)
```

이 메서드는 Install, Upgrade, Template 등 여러 Action에서 공통으로 사용하는 렌더링 파이프라인이다.

```
renderResources() 처리 흐름
  │
  ├─ 1. getCapabilities() → K8s 버전/API 정보 수집
  │
  ├─ 2. KubeVersion 호환성 검사
  │
  ├─ 3. Engine.Render(chart, values) → 템플릿 렌더링
  │     ├─ interactWithRemote=true  → engine.New(restConfig) (lookup 함수 활성화)
  │     └─ interactWithRemote=false → 로컬 전용 Engine
  │
  ├─ 4. NOTES.txt 추출 → 사용자 노트 분리
  │
  ├─ 5. PostRenderer 실행 (선택)
  │     ├─ annotateAndMerge()  → 파일별 어노테이션 + 머지
  │     ├─ pr.Run()            → 외부 후처리 (Kustomize 등)
  │     └─ splitAndDeannotate() → 원본 파일명으로 분리
  │
  ├─ 6. SortManifests() → Hook/매니페스트 분류 + 설치 순서 정렬
  │
  └─ 7. 매니페스트 출력 (버퍼 또는 파일)
```

**포스트 렌더링의 annotate/merge/split 패턴:** PostRenderer는 단일 YAML 스트림을 입력으로 받고 수정된 스트림을 반환한다. 하지만 Helm 내부에서는 파일명별로 매니페스트를 관리한다. 이 불일치를 해결하기 위해:
1. `annotateAndMerge()`: 각 문서에 `postrenderer.helm.sh/postrender-filename` 어노테이션을 추가하고 단일 스트림으로 합친다
2. PostRenderer가 처리한다
3. `splitAndDeannotate()`: 어노테이션을 기반으로 원본 파일명별로 다시 분리한다

### RESTClientGetter 인터페이스

```go
// pkg/action/action.go
type RESTClientGetter interface {
    ToRESTConfig() (*rest.Config, error)
    ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error)
    ToRESTMapper() (meta.RESTMapper, error)
}
```

Kubernetes 클라이언트를 생성하는 팩토리 인터페이스이다. `k8s.io/cli-runtime`의 `genericclioptions.ConfigFlags`가 이를 구현한다.

### DryRunStrategy

```go
// pkg/action/action.go
type DryRunStrategy string

const (
    DryRunNone   DryRunStrategy = "none"     // 실제 실행
    DryRunClient DryRunStrategy = "client"   // 클라이언트 사이드 (K8s API 호출 없음)
    DryRunServer DryRunStrategy = "server"   // 서버 사이드 (API 호출하되 저장 안 함)
)
```

## 3. Engine -- 템플릿 렌더링 엔진

**소스 파일:** `pkg/engine/engine.go`, `pkg/engine/funcs.go`

### 구조체 정의

```go
// pkg/engine/engine.go
type Engine struct {
    // If strict is enabled, template rendering will fail if a template references
    // a value that was not passed in.
    Strict bool
    // In LintMode, some 'required' template values may be missing, so don't fail
    LintMode bool
    // optional provider of clients to talk to the Kubernetes API
    clientProvider *ClientProvider
    // EnableDNS tells the engine to allow DNS lookups when rendering templates
    EnableDNS bool
    // CustomTemplateFuncs is defined by users to provide custom template funcs
    CustomTemplateFuncs template.FuncMap
}
```

### Render() 메서드

```go
// pkg/engine/engine.go
func (e Engine) Render(chrt ci.Charter, values common.Values) (map[string]string, error) {
    tmap := allTemplates(chrt, values)
    return e.render(tmap)
}
```

`Render()`는 차트와 값을 받아 렌더링된 매니페스트 맵(`파일명 → 내용`)을 반환한다.

### 렌더링 파이프라인

```
Render(chart, values)
  │
  ├─ 1. allTemplates(chart, values) → renderable 맵 생성
  │     └─ recAllTpls() → 재귀적으로 의존 차트까지 수집
  │         ├─ 루트 차트: Values 전체 전달
  │         ├─ 서브 차트: Values.{서브차트명} 만 전달
  │         └─ 라이브러리 차트: _ 프리픽스 파일만 포함
  │
  ├─ 2. render(tpls) → Go template 실행
  │     ├─ template.New("gotpl") 생성
  │     ├─ missingkey 옵션 설정 (strict → error, 일반 → zero)
  │     ├─ initFunMap() → 함수 맵 초기화
  │     ├─ sortTemplates() → 경로 깊이순 정렬 (상위 우선)
  │     ├─ 모든 템플릿 Parse
  │     └─ 순서대로 ExecuteTemplate
  │         ├─ _ 프리픽스 파일 스킵 (partial)
  │         └─ <no value> → "" 치환
  │
  └─ 3. 결과: map[string]string (파일명 → 렌더링 결과)
```

### 템플릿 함수 체계

`pkg/engine/funcs.go`에서 함수 맵을 구성한다:

```go
// pkg/engine/funcs.go
func funcMap() template.FuncMap {
    f := sprig.TxtFuncMap()      // Sprig 160+ 함수
    delete(f, "env")              // 보안: 환경변수 접근 차단
    delete(f, "expandenv")        // 보안: 환경변수 확장 차단

    extra := template.FuncMap{
        "toToml":        toTOML,
        "fromToml":      fromTOML,
        "toYaml":        toYAML,
        "mustToYaml":    mustToYAML,
        "toYamlPretty":  toYAMLPretty,
        "fromYaml":      fromYAML,
        "fromYamlArray": fromYAMLArray,
        "toJson":        toJSON,
        "mustToJson":    mustToJSON,
        // ... 추가 함수들
    }
    // ...
}
```

| 함수 카테고리 | 함수 예시 | 설명 |
|-------------|---------|------|
| **Sprig 기본** | `default`, `empty`, `coalesce`, `toStrings` | 160+ 범용 함수 |
| **YAML/JSON** | `toYaml`, `fromYaml`, `toJson`, `fromJson` | 직렬화/역직렬화 |
| **TOML** | `toToml`, `fromToml` | TOML 변환 |
| **제어** | `required`, `fail` | 필수 값 검증, 강제 실패 |
| **템플릿** | `include`, `tpl` | 동적 템플릿 포함/실행 |
| **K8s** | `lookup` | 클러스터에서 리소스 조회 (런타임) |
| **DNS** | `getHostByName` | DNS 조회 (EnableDNS=true일 때) |
| **보안 차단** | ~~`env`~~, ~~`expandenv`~~ | Sprig에서 삭제 -- 환경변수 유출 방지 |

### initFunMap() -- 컨텍스트 종속 함수 바인딩

```go
// pkg/engine/engine.go
func (e Engine) initFunMap(t *template.Template) {
    funcMap := funcMap()
    includedNames := make(map[string]int)

    // include 함수: 재귀 호출 최대 1000번 제한
    funcMap["include"] = includeFun(t, includedNames)
    // tpl 함수: 동적 템플릿 렌더링
    funcMap["tpl"] = tplFun(t, includedNames, e.Strict)
    // required 함수: LintMode에서는 경고만
    funcMap["required"] = func(warn string, val any) (any, error) { ... }
    // fail 함수: LintMode에서는 무시
    funcMap["fail"] = func(msg string) (string, error) { ... }
    // lookup 함수: K8s API 연동 시에만 활성화
    if !e.LintMode && e.clientProvider != nil {
        funcMap["lookup"] = newLookupFunction(*e.clientProvider)
    }
    // DNS 비활성화 시 빈 문자열 반환
    if !e.EnableDNS {
        funcMap["getHostByName"] = func(_ string) string { return "" }
    }
    // 사용자 정의 함수 추가
    maps.Copy(funcMap, e.CustomTemplateFuncs)

    t.Funcs(funcMap)
}
```

**왜 `include`에 재귀 제한(1000회)을 두는가?** 무한 재귀를 방지하기 위해서이다. `include`로 자기 자신을 호출하거나 순환 참조가 생기면 스택 오버플로우 대신 깔끔한 에러를 반환한다.

### 값 스코핑 (Value Scoping)

```go
// pkg/engine/engine.go - recAllTpls()
next := map[string]any{
    "Chart":        chartMetaData,
    "Files":        newFiles(accessor.Files()),
    "Release":      vals["Release"],
    "Capabilities": vals["Capabilities"],
    "Values":       make(common.Values),
    "Subcharts":    subCharts,
}

// 루트 차트: 전체 Values
if accessor.IsRoot() {
    next["Values"] = vals["Values"]
// 서브 차트: 부모의 Values.{차트명} 섹션만
} else if vs, err := values.Table("Values." + accessor.Name()); err == nil {
    next["Values"] = vs
}
```

이 설계는 차트 간 값 격리를 보장한다. 서브 차트 `bar`는 부모 차트 `foo`의 전체 Values에 접근할 수 없고, `foo`가 `Values.bar`로 명시적으로 전달한 값만 받는다.

## 4. Storage -- 릴리스 스토리지

**소스 파일:** `pkg/storage/storage.go`, `pkg/storage/driver/driver.go`

### Storage 구조체

```go
// pkg/storage/storage.go
type Storage struct {
    driver.Driver

    // MaxHistory specifies the maximum number of historical releases that will
    // be retained, including the most recent release. Values of 0 or less are
    // ignored (meaning no limits are imposed).
    MaxHistory int

    // Embed a LogHolder to provide logger functionality
    logging.LogHolder
}
```

Storage는 `driver.Driver` 인터페이스를 임베딩하여, 기본 CRUD를 드라이버에 위임하면서 히스토리 관리, 로깅 등의 상위 기능을 추가한다.

### Driver 인터페이스 계층

```go
// pkg/storage/driver/driver.go
type Creator interface {
    Create(key string, rls release.Releaser) error
}

type Updator interface {
    Update(key string, rls release.Releaser) error
}

type Deletor interface {
    Delete(key string) (release.Releaser, error)
}

type Queryor interface {
    Get(key string) (release.Releaser, error)
    List(filter func(release.Releaser) bool) ([]release.Releaser, error)
    Query(labels map[string]string) ([]release.Releaser, error)
}

type Driver interface {
    Creator
    Updator
    Deletor
    Queryor
    Name() string
}
```

4개의 인터페이스를 조합하여 Driver를 구성한다. 이 분리는 Interface Segregation Principle을 따른다.

### 스토리지 드라이버 구현체

| 드라이버 | 파일 | 설명 |
|---------|------|------|
| `Secrets` | `driver/secrets.go` | **기본값.** K8s Secret에 릴리스 저장. base64 + gzip 인코딩 |
| `ConfigMaps` | `driver/cfgmaps.go` | K8s ConfigMap에 릴리스 저장 |
| `Memory` | `driver/memory.go` | 인메모리 저장 (테스트, 임시) |
| `SQL` | `driver/sql.go` | PostgreSQL에 릴리스 저장 (대규모 운영) |

### 키 생성 규칙

```go
// pkg/storage/storage.go
const HelmStorageType = "sh.helm.release.v1"

func makeKey(rlsname string, version int) string {
    return fmt.Sprintf("%s.%s.v%d", HelmStorageType, rlsname, version)
}
// 예: "sh.helm.release.v1.my-app.v3"
```

이 키 형식은 Kubernetes 리소스명으로 사용되며, 릴리스 이름 + 버전 조합으로 유일성을 보장한다.

### 핵심 CRUD 메서드

```go
// pkg/storage/storage.go

// Get: 이름+버전으로 릴리스 조회
func (s *Storage) Get(name string, version int) (release.Releaser, error) {
    return s.Driver.Get(makeKey(name, version))
}

// Create: 새 릴리스 생성 + MaxHistory 초과 시 오래된 릴리스 삭제
func (s *Storage) Create(rls release.Releaser) error {
    rac, err := release.NewAccessor(rls)
    if err != nil { return err }
    if s.MaxHistory > 0 {
        if err := s.removeLeastRecent(rac.Name(), s.MaxHistory-1); err != nil &&
            !errors.Is(err, driver.ErrReleaseNotFound) {
            return err
        }
    }
    return s.Driver.Create(makeKey(rac.Name(), rac.Version()), rls)
}

// History: 이름으로 모든 리비전 조회
func (s *Storage) History(name string) ([]release.Releaser, error) {
    return s.Query(map[string]string{"name": name, "owner": "helm"})
}

// Last: 최신 리비전 조회
func (s *Storage) Last(name string) (release.Releaser, error) {
    h, err := s.History(name)
    // ...
    relutil.Reverse(rls, relutil.SortByRevision)
    return rls[0], nil
}

// Deployed: 최신 DEPLOYED 상태 릴리스 조회
func (s *Storage) Deployed(name string) (release.Releaser, error) {
    ls, err := s.DeployedAll(name)
    // ...
    relutil.Reverse(rls, relutil.SortByRevision)
    return rls[0], nil
}
```

### MaxHistory와 히스토리 정리

```go
// pkg/storage/storage.go
func (s *Storage) removeLeastRecent(name string, maximum int) error {
    h, err := s.History(name)
    if len(h) <= maximum { return nil }

    // 가장 오래된 것부터 정렬
    relutil.SortByRevision(rls)

    // 현재 배포된 릴리스는 삭제하지 않음
    lastDeployed, err := s.Deployed(name)
    // ...
    for _, rel := range rls {
        if len(rls)-len(toDelete) == maximum { break }
        if rel.Version != ldac.Version() {
            toDelete = append(toDelete, rel)
        }
    }
    // 삭제 실행
}
```

**핵심 정책:** MaxHistory를 초과하면 가장 오래된 릴리스부터 삭제하되, 현재 DEPLOYED 상태인 릴리스는 절대 삭제하지 않는다. API 처리량 제한이 있을 경우 여러 번 호출하면 결국 모두 정리된다.

### Init() -- Storage 팩토리

```go
// pkg/storage/storage.go
func Init(d driver.Driver) *Storage {
    if d == nil {
        d = driver.NewMemory()   // 기본값: 인메모리
    }
    s := &Storage{Driver: d}
    // 드라이버에서 로거 상속
    if ls, ok := d.(logging.LoggerSetterGetter); ok {
        h = ls.Logger().Handler()
    }
    s.SetLogger(h)
    return s
}
```

## 5. KubeClient -- Kubernetes API 클라이언트

**소스 파일:** `pkg/kube/interface.go`, `pkg/kube/client.go`

### Interface 인터페이스

```go
// pkg/kube/interface.go
type Interface interface {
    // Get: 배포된 리소스 상세 조회 (관련 Pod 포함 가능)
    Get(resources ResourceList, related bool) (map[string][]runtime.Object, error)

    // Create: 리소스 생성
    Create(resources ResourceList, options ...ClientCreateOption) (*Result, error)

    // Delete: 리소스 삭제 (전파 정책 지정)
    Delete(resources ResourceList, policy metav1.DeletionPropagation) (*Result, []error)

    // Update: 리소스 업데이트 (없으면 생성)
    Update(original, target ResourceList, options ...ClientUpdateOption) (*Result, error)

    // Build: YAML 스트림 → ResourceList 변환
    Build(reader io.Reader, validate bool) (ResourceList, error)

    // IsReachable: 클러스터 연결 확인
    IsReachable() error

    // GetWaiter: 대기 전략별 Waiter 획득
    GetWaiter(ws WaitStrategy) (Waiter, error)

    // GetPodList: Pod 목록 조회
    GetPodList(namespace string, listOptions metav1.ListOptions) (*v1.PodList, error)

    // OutputContainerLogsForPodList: Pod 로그 출력
    OutputContainerLogsForPodList(podList *v1.PodList, namespace string,
        writerFunc func(namespace, pod, container string) io.Writer) error

    // BuildTable: 표 형식으로 리소스 빌드
    BuildTable(reader io.Reader, validate bool) (ResourceList, error)
}
```

### Waiter 인터페이스

```go
// pkg/kube/interface.go
type Waiter interface {
    // Wait: 리소스가 Ready 상태가 될 때까지 대기
    Wait(resources ResourceList, timeout time.Duration) error

    // WaitWithJobs: Job 완료까지 대기
    WaitWithJobs(resources ResourceList, timeout time.Duration) error

    // WaitForDelete: 리소스 삭제 완료까지 대기
    WaitForDelete(resources ResourceList, timeout time.Duration) error

    // WatchUntilReady: 리소스 변경 감시 (Hook용)
    WatchUntilReady(resources ResourceList, timeout time.Duration) error
}
```

### Client 구조체

```go
// pkg/kube/client.go
type Client struct {
    // kubectl Factory 인터페이스 (K8s 리소스 빌더/디스커버리)
    Factory Factory
    // 네임스페이스 오버라이드
    Namespace string
    // 대기 전략용 컨텍스트
    WaitContext context.Context
    Waiter
    kubeClient kubernetes.Interface
    // 구조화 로깅
    logging.LogHolder
}
```

### WaitStrategy -- 대기 전략

Helm v4는 3가지 대기 전략을 도입했다:

```go
// pkg/kube/client.go
const (
    // StatusWatcherStrategy: kstatus 기반 이벤트 구동 대기
    // Watch + 상태 리더를 사용해 리소스 준비 상태를 실시간 감시
    StatusWatcherStrategy WaitStrategy = "watcher"

    // LegacyStrategy: Helm 3 방식 주기적 폴링
    // list RBAC만 필요, 호환성 우선
    LegacyStrategy WaitStrategy = "legacy"

    // HookOnlyStrategy: Hook Pod/Job만 대기
    // 일반 차트 리소스는 대기하지 않음
    HookOnlyStrategy WaitStrategy = "hookOnly"
)
```

| 전략 | 작동 방식 | RBAC 요구 | 사용 시점 |
|------|---------|----------|---------|
| `watcher` | kstatus Watch + 집계 리더 | list + watch | 기본값 (`--wait`) |
| `legacy` | 주기적 폴링 | list | Watch 불가 환경 |
| `hookOnly` | Hook만 대기 | list | Hook 기반 테스트 |

### Update 메서드의 3-Way Merge

KubeClient의 `Update()`는 Helm의 핵심적인 리소스 관리 메커니즘인 3-way strategic merge patch를 구현한다:

```
┌──────────────┐   ┌──────────────┐   ┌──────────────┐
│   Original   │   │    Target    │   │   Current    │
│ (이전 차트   │   │ (새 차트     │   │ (클러스터에  │
│  렌더링 결과)│   │  렌더링 결과)│   │  실제 상태)  │
└──────┬───────┘   └──────┬───────┘   └──────┬───────┘
       │                  │                  │
       └────────────┬─────┘                  │
                    │                        │
              3-Way Strategic                │
              Merge Patch 계산               │
                    │                        │
                    └────────────────────────▼
                              PATCH 적용
```

**왜 3-Way Merge인가?** 2-way merge(target vs current)는 사용자가 kubectl로 직접 수정한 값을 무시할 수 있다. 3-way merge는 "원래 Helm이 만든 것 → 새로 만들 것 → 현재 클러스터 상태"를 모두 비교하여, 사용자의 수동 변경을 보존하면서 차트 업그레이드를 적용한다.

## 6. RegistryClient -- OCI 레지스트리 클라이언트

**소스 파일:** `pkg/registry/client.go`

### Client 구조체

```go
// pkg/registry/client.go
type Client struct {
    debug              bool
    enableCache        bool
    credentialsFile    string
    username           string
    password           string
    out                io.Writer
    authorizer         *auth.Client
    registryAuthorizer RemoteClient
    credentialsStore   credentials.Store
    httpClient         *http.Client
    plainHTTP          bool
}
```

### 인증 체계

```go
// pkg/registry/client.go
func NewClient(options ...ClientOption) (*Client, error) {
    // ...
    // 자격 증명 저장소: Helm 전용 + Docker fallback
    store, err := credentials.NewStore(client.credentialsFile, storeOptions)
    dockerStore, err := credentials.NewStoreFromDocker(storeOptions)
    client.credentialsStore = credentials.NewStoreWithFallbacks(store, dockerStore)

    // 인증 클라이언트 설정
    authorizer := auth.Client{Client: client.httpClient}
    authorizer.SetUserAgent(version.GetUserAgent())
    if client.username != "" && client.password != "" {
        // 명시적 Basic Auth
        authorizer.Credential = func(...) (auth.Credential, error) {
            return auth.Credential{Username: client.username, Password: client.password}, nil
        }
    } else {
        // 자격 증명 저장소에서 자동 로드
        authorizer.Credential = credentials.Credential(client.credentialsStore)
    }
    // ...
}
```

**자격 증명 우선순위:**
1. 명시적 username/password (CLI 플래그)
2. Helm 자격 증명 파일 (`~/.config/helm/registry/config.json`)
3. Docker 자격 증명 (`~/.docker/config.json`) -- fallback

### Push() -- 차트 업로드

```go
// pkg/registry/client.go
func (c *Client) Push(data []byte, ref string, options ...PushOption) (*PushResult, error)
```

```
Push(chartData, "oci://registry.example.com/charts/myapp:1.0.0")
  │
  ├─ 1. parseReference(ref) → OCI 레퍼런스 파싱
  │
  ├─ 2. extractChartMeta(data) → Chart.yaml에서 메타데이터 추출
  │
  ├─ 3. strictMode 검증: ref의 이름:태그가 Chart.yaml과 일치하는지
  │
  ├─ 4. OCI 레이어 구성 (oras-go 사용)
  │     ├─ ChartLayerMediaType    → 차트 아카이브 레이어
  │     ├─ ConfigMediaType        → 메타데이터 (JSON)
  │     └─ ProvLayerMediaType     → 서명 파일 (선택)
  │
  ├─ 5. 매니페스트 태깅 → OCI Image Manifest v2
  │
  ├─ 6. oras.ExtendedCopy() → 레지스트리에 업로드
  │
  └─ 7. PushResult 반환 (Manifest Digest, Chart Digest 등)
```

### Pull() -- 차트 다운로드

```go
// pkg/registry/client.go
func (c *Client) Pull(ref string, options ...PullOption) (*PullResult, error)
```

```
Pull("oci://registry.example.com/charts/myapp:1.0.0")
  │
  ├─ 1. allowedMediaTypes 구성
  │     ├─ ConfigMediaType
  │     ├─ ChartLayerMediaType (+ Legacy)
  │     └─ ProvLayerMediaType (선택)
  │
  ├─ 2. GenericClient.PullGeneric() → OCI 아티팩트 다운로드
  │
  ├─ 3. processChartPull() → 차트 전용 처리
  │     ├─ 디스크립터 분류 (config, chart, prov)
  │     ├─ 메타데이터 JSON 언마샬
  │     └─ PullResult 구성
  │
  └─ 4. 결과: Chart 데이터 + 메타데이터 + 서명
```

### OCI 미디어 타입

| 상수 | 값 | 용도 |
|------|-----|------|
| `ConfigMediaType` | `application/vnd.cncf.helm.config.v1+json` | 차트 메타데이터 |
| `ChartLayerMediaType` | `application/vnd.cncf.helm.chart.content.v1.tar+gzip` | 차트 아카이브 |
| `ProvLayerMediaType` | `application/vnd.cncf.helm.chart.provenance.v1.prov` | 서명 파일 |
| `LegacyChartLayerMediaType` | `application/tar+gzip` | 레거시 호환 |

### 시맨틱 버전 처리

OCI 태그는 `+` 기호를 지원하지 않으므로, Helm은 `+`를 `_`로 치환하여 저장하고 가져올 때 복원한다:

```
Push: 1.0.0+build.123  →  태그: 1.0.0_build.123
Pull: 태그: 1.0.0_build.123  →  1.0.0+build.123
```

## 7. ChartLoader -- 차트 로딩

**소스 파일:** `pkg/chart/loader/load.go`

### 로딩 인터페이스

```go
// pkg/chart/loader/load.go
type ChartLoader interface {
    Load() (chart.Charter, error)
}
```

### Load() -- 통합 로딩 함수

```go
// pkg/chart/loader/load.go
func Load(name string) (chart.Charter, error) {
    l, err := Loader(name)
    if err != nil { return nil, err }
    return l.Load()
}

func Loader(name string) (ChartLoader, error) {
    fi, err := os.Stat(name)
    if fi.IsDir() {
        return DirLoader(name), nil
    }
    return FileLoader(name), nil
}
```

### 버전 감지 및 분기

```
Load(name)
  │
  ├─ os.Stat(name)
  │   ├─ 디렉토리 → DirLoader → LoadDir()
  │   └─ 파일     → FileLoader → LoadFile()
  │
  ├─ Chart.yaml 읽기
  │
  ├─ apiVersion 감지
  │   ├─ "v1" / "v2" / "" → c2load.Load()  (pkg/chart/v2/loader)
  │   └─ "v3"             → c3load.Load()  (internal/chart/v3/loader)
  │
  └─ Charter 반환
```

### DirLoader vs FileLoader

```go
// 디렉토리 로딩: 파일 시스템에서 직접 읽기
type DirLoader string
func (l DirLoader) Load() (chart.Charter, error) {
    return LoadDir(string(l))
}

// 파일 로딩: .tar.gz 아카이브 파싱
type FileLoader string
func (l FileLoader) Load() (chart.Charter, error) {
    return LoadFile(string(l))
}
```

### LoadFile() -- 아카이브 로딩

```go
// pkg/chart/loader/load.go
func LoadFile(name string) (chart.Charter, error) {
    // 1. 파일 존재 확인
    // 2. archive.EnsureArchive() → 유효한 아카이브인지 검증
    // 3. archive.LoadArchiveFiles() → gzip + tar 해제
    // 4. Chart.yaml 파싱 → apiVersion 감지
    // 5. 버전별 로더로 분기
}
```

### LoadArchive() -- 스트림 로딩

```go
// pkg/chart/loader/load.go
func LoadArchive(in io.Reader) (chart.Charter, error)
```

`io.Reader`에서 직접 차트를 로딩한다. SDK 사용자(Flux 등)가 네트워크 스트림에서 직접 차트를 로딩할 때 사용한다.

## 8. 컴포넌트 상호작용 -- Install 흐름

모든 핵심 컴포넌트가 어떻게 협력하는지 Install 흐름으로 살펴본다:

```
helm install my-app ./mychart
  │
  ├─ 1. pkg/cmd: Install Cobra 커맨드 실행
  │     └─ action.NewInstall(cfg) → Install 액션 생성
  │
  ├─ 2. ChartLoader: 차트 로딩
  │     └─ loader.Load("./mychart") → Charter 반환
  │
  ├─ 3. Values 병합
  │     └─ cli/values.Options.MergeValues() → Values 맵 반환
  │
  ├─ 4. Configuration.renderResources()
  │     ├─ Engine.Render(chart, values) → 매니페스트 맵
  │     ├─ PostRenderer (선택) → 후처리
  │     └─ SortManifests() → Hook + 매니페스트 분류
  │
  ├─ 5. Storage.Create() → 릴리스 레코드 생성 (PENDING_INSTALL)
  │
  ├─ 6. KubeClient.Create() → K8s 리소스 생성
  │     ├─ CRD 먼저 (IncludeCRDs)
  │     ├─ pre-install Hook 실행
  │     ├─ 매니페스트 리소스 생성
  │     └─ post-install Hook 실행
  │
  ├─ 7. Waiter.Wait() → 리소스 Ready 대기 (--wait)
  │
  ├─ 8. Storage.Update() → 릴리스 상태 DEPLOYED로 갱신
  │
  └─ 9. NOTES.txt 출력
```

## 9. 컴포넌트 설계 원칙 요약

| 원칙 | 적용 |
|------|------|
| **의존성 주입** | Configuration이 모든 의존성을 주입, Action은 인터페이스만 참조 |
| **인터페이스 분리** | Driver = Creator + Updator + Deletor + Queryor |
| **개방-폐쇄** | Charter/Releaser를 `any`로 선언, 새 버전 추가 시 기존 코드 불변 |
| **지연 초기화** | lazyClient로 K8s 클라이언트 생성 지연 |
| **전략 패턴** | WaitStrategy, DryRunStrategy로 런타임 행동 교체 |
| **파이프라인 패턴** | renderResources: 렌더링 → 후처리 → 정렬 → 출력 |
| **옵션 패턴** | ClientOption, PullOption, PushOption 등 함수형 옵션 |
