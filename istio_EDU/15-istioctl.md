# 15. istioctl CLI Deep-Dive

## 목차

1. [개요](#1-개요)
2. [진입점과 초기화 흐름](#2-진입점과-초기화-흐름)
3. [전체 명령어 트리](#3-전체-명령어-트리)
4. [proxy-status (ps): xDS 동기화 상태 조회](#4-proxy-status-ps-xds-동기화-상태-조회)
5. [proxy-config (pc): Envoy 설정 덤프](#5-proxy-config-pc-envoy-설정-덤프)
6. [analyze: 설정 분석 및 검증](#6-analyze-설정-분석-및-검증)
7. [kube-inject: 수동 사이드카 인젝션](#7-kube-inject-수동-사이드카-인젝션)
8. [install: 설치 명령과 프로파일 시스템](#8-install-설치-명령과-프로파일-시스템)
9. [dashboard: 대시보드 접근](#9-dashboard-대시보드-접근)
10. [version: 버전 정보](#10-version-버전-정보)
11. [experimental: 실험적 명령어](#11-experimental-실험적-명령어)
12. [설정 아키텍처와 Viper 통합](#12-설정-아키텍처와-viper-통합)
13. [CLI Context 시스템](#13-cli-context-시스템)
14. [multixds: 다중 컨트롤 플레인 통신](#14-multixds-다중-컨트롤-플레인-통신)
15. [정리](#15-정리)

---

## 1. 개요

`istioctl`은 Istio 서비스 메시의 공식 CLI 도구로, 서비스 운영자가 메시를 설치, 설정, 디버그, 진단하는 데 사용한다. 단순한 `kubectl` 래퍼가 아니라, xDS 프로토콜을 통해 Istiod 컨트롤 플레인에 직접 질의하고, Envoy 프록시의 admin API에 접근하며, 설정을 분석하는 독립적인 진단 도구이다.

**핵심 설계 원칙:**

- **Cobra + Viper 기반 CLI**: spf13/cobra로 명령어 트리를 구성하고 spf13/viper로 설정을 관리
- **xDS 직접 통신**: Istiod에 xDS DiscoveryRequest를 보내 프록시 상태를 실시간 조회
- **Envoy Admin API 활용**: 개별 프록시의 config_dump, clusters, stats, logging 엔드포인트에 접근
- **다중 리비전 지원**: Canary 배포 시나리오에서 특정 리비전의 컨트롤 플레인을 대상으로 지정 가능

**소스코드 위치:**

```
istio/
├── istioctl/
│   ├── cmd/
│   │   ├── istioctl/main.go         # 진입점
│   │   └── root.go                   # 루트 명령어 트리 구성
│   └── pkg/
│       ├── analyze/                  # analyze 명령
│       ├── authz/                    # authz check
│       ├── cli/                      # CLI Context 인터페이스
│       ├── dashboard/                # dashboard 명령
│       ├── describe/                 # describe 명령
│       ├── kubeinject/               # kube-inject 명령
│       ├── metrics/                  # metrics 명령
│       ├── multixds/                 # 다중 Istiod 통신
│       ├── precheck/                 # precheck 명령
│       ├── proxyconfig/              # proxy-config 명령
│       ├── proxystatus/              # proxy-status 명령
│       ├── root/                     # 루트 설정
│       ├── tag/                      # revision tag 관리
│       ├── validate/                 # validate 명령
│       ├── version/                  # version 명령
│       ├── waypoint/                 # waypoint 관리 (Ambient)
│       ├── workload/                 # workload 관리
│       └── ztunnelconfig/            # ztunnel 설정
└── operator/
    └── cmd/mesh/                     # install/uninstall/manifest/upgrade
```

---

## 2. 진입점과 초기화 흐름

### main() 함수

소스 파일: `istioctl/cmd/istioctl/main.go`

```go
func main() {
    if err := cmd.ConfigAndEnvProcessing(); err != nil {
        fmt.Fprintf(os.Stderr, "Could not initialize: %v\n", err)
        exitCode := cmd.GetExitCode(err)
        os.Exit(exitCode)
    }

    rootCmd := cmd.GetRootCmd(os.Args[1:])
    log.EnableKlogWithCobra()

    if err := rootCmd.Execute(); err != nil {
        exitCode := cmd.GetExitCode(err)
        os.Exit(exitCode)
    }
}
```

초기화 과정은 정확히 세 단계로 진행된다:

```
1. ConfigAndEnvProcessing()  -->  Viper 설정 로드 (환경변수 + 파일)
2. GetRootCmd(args)          -->  전체 Cobra 명령어 트리 구성
3. rootCmd.Execute()         -->  인자 파싱 후 해당 명령 실행
```

### ConfigAndEnvProcessing: Viper 초기화

소스 파일: `istioctl/cmd/root.go`

```go
func ConfigAndEnvProcessing() error {
    configPath := filepath.Dir(root.IstioConfig)
    baseName := filepath.Base(root.IstioConfig)
    configType := filepath.Ext(root.IstioConfig)
    configName := baseName[0 : len(baseName)-len(configType)]

    viper.SetEnvPrefix("ISTIOCTL")
    viper.AutomaticEnv()
    viper.AllowEmptyEnv(true)
    viper.SetConfigName(configName)
    viper.SetConfigType(configType)
    viper.AddConfigPath(configPath)
    viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
    err := viper.ReadInConfig()
    if root.IstioConfig != defaultIstioctlConfig {
        return err
    }
    return nil
}
```

**왜 이렇게 설계했는가?** Viper를 통해 CLI 플래그, 환경변수, 설정 파일의 세 가지 설정 소스를 단일 체계로 통합한다. `ISTIOCTL_` 접두사로 환경변수를 매핑하고, `$HOME/.istioctl/config.yaml`에서 기본값을 읽는다. 이로써 사용자는 반복적인 플래그 없이 자주 사용하는 설정을 영속적으로 관리할 수 있다.

기본값은 `init()` 함수에서 설정된다:

```go
func init() {
    viper.SetDefault("istioNamespace", constants.IstioSystemNamespace)
    viper.SetDefault("xds-port", 15012)
}
```

### 초기화 흐름 다이어그램

```
main()
 │
 ├──> ConfigAndEnvProcessing()
 │     ├──> viper.SetEnvPrefix("ISTIOCTL")       # ISTIOCTL_XXX 환경변수 바인딩
 │     ├──> viper.AutomaticEnv()                  # 자동 환경변수 매핑
 │     ├──> viper.ReadInConfig()                  # $HOME/.istioctl/config.yaml 로드
 │     └──> return nil (기본 경로면 에러 무시)
 │
 ├──> GetRootCmd(os.Args[1:])
 │     ├──> cobra.Command{Use: "istioctl"} 생성
 │     ├──> cli.AddRootFlags(flags)               # --namespace, --istioNamespace 등
 │     ├──> cli.NewCLIContext(rootOptions)         # CLI Context 객체 생성
 │     ├──> 서브커맨드 등록 (30개 이상)
 │     └──> BFS로 모든 서브커맨드에 FlagErrorFunc 설정
 │
 ├──> log.EnableKlogWithCobra()                   # k8s klog 활성화
 │
 └──> rootCmd.Execute()                           # Cobra 명령 실행
```

---

## 3. 전체 명령어 트리

`GetRootCmd()` 함수에서 등록되는 모든 명령어를 소스코드 기반으로 정리한다.

소스 파일: `istioctl/cmd/root.go`

### 최상위 명령어

| 명령어 | 별칭 | 등록 코드 | 설명 |
|--------|------|-----------|------|
| `proxy-status` | `ps` | `proxystatus.StableXdsStatusCommand(ctx)` | xDS 동기화 상태 조회 |
| `proxy-config` | `pc` | `proxyconfig.ProxyConfig(ctx)` | Envoy 설정 조회 |
| `analyze` | - | `analyze.Analyze(ctx)` | Istio 설정 분석/검증 |
| `kube-inject` | - | `kubeinject.InjectCommand(ctx)` | 수동 사이드카 인젝션 |
| `install` | `apply` | `mesh.InstallCmd(ctx)` | Istio 설치 |
| `uninstall` | - | `mesh.UninstallCmd(ctx)` | Istio 제거 |
| `upgrade` | - | `mesh.UpgradeCmd(ctx)` | Istio 업그레이드 |
| `manifest` | - | `mesh.ManifestCmd(ctx)` | 매니페스트 생성 |
| `dashboard` | `dash`, `d` | `dashboard.Dashboard(ctx)` | 웹 UI 대시보드 접근 |
| `version` | - | `version.NewVersionCommand(ctx)` | 버전 정보 표시 |
| `validate` | - | `validate.NewValidateCommand(ctx)` | YAML 유효성 검사 |
| `tag` | - | `tag.TagCommand(ctx)` | 리비전 태그 관리 |
| `bug-report` | - | `bugreport.Cmd(ctx, ...)` | 버그 리포트 수집 |
| `waypoint` | - | `waypoint.Cmd(ctx)` | Ambient 모드 웨이포인트 |
| `ztunnel-config` | - | `ztunnelconfig.ZtunnelConfig(ctx)` | ztunnel 설정 |
| `remote-clusters` | - | `proxyconfig.ClustersCommand(ctx)` | 원격 클러스터 목록 |
| `create-remote-secret` | - | `multicluster.NewCreateRemoteSecretCommand(ctx)` | 멀티클러스터 시크릿 |
| `experimental` | `x`, `exp` | 직접 생성 | 실험적 명령어 그룹 |

### experimental (x) 서브 명령어

| 명령어 | 등록 코드 | 설명 |
|--------|-----------|------|
| `authz` | `authz.AuthZ(ctx)` | AuthorizationPolicy 검사 |
| `metrics` (m) | `metrics.Cmd(ctx)` | Prometheus 워크로드 메트릭 |
| `describe` | `describe.Cmd(ctx)` | 서비스/Pod 상세 설명 |
| `config` | `config.Cmd()` | istioctl 설정 관리 |
| `workload` | `workload.Cmd(ctx)` | VM 워크로드 관리 |
| `internal-debug` | `internaldebug.DebugCommand(ctx)` | 내부 디버그 |
| `precheck` | `precheck.Cmd(ctx)` | 설치/업그레이드 사전 검사 |
| `envoy-stats` (es) | `proxyconfig.StatsConfigCmd(ctx)` | Envoy 메트릭 조회 |
| `check-inject` | `checkinject.Cmd(ctx)` | 인젝션 상태 확인 |
| `injector` | `injector.Cmd(ctx)` | 인젝터 상태 |
| `version` | `version.XdsVersionCommand(ctx)` | xDS 기반 버전 조회 |
| `proxy-status` | `proxystatus.XdsStatusCommand(ctx)` | xDS 기반 상태 조회 |

### proxy-config 서브 명령어

소스 파일: `istioctl/pkg/proxyconfig/proxyconfig.go` (1353-1382줄)

| 서브커맨드 | 별칭 | 설명 |
|------------|------|------|
| `cluster` | `clusters`, `c` | 클러스터(업스트림) 설정 |
| `listener` | `listeners`, `l` | 리스너 설정 |
| `route` | `routes`, `r` | 라우트 설정 |
| `endpoint` | `endpoints`, `ep` | 엔드포인트 정보 |
| `bootstrap` | `b` | 부트스트랩 설정 |
| `log` | `o` | 로깅 레벨 조회/변경 |
| `secret` | `secrets`, `s` | 시크릿(인증서) 정보 |
| `all` | `a` | 전체 설정 덤프 |
| `ecds` | - | Extension Config Discovery |
| `eds` | - | Endpoint Discovery Service |
| `rootca-compare` | - | Root CA 비교 |

### 전체 명령어 트리 ASCII 다이어그램

```
istioctl
├── proxy-status (ps)          # xDS 동기화 상태
├── proxy-config (pc)          # Envoy 설정 조회
│   ├── cluster (c)
│   ├── listener (l)
│   ├── route (r)
│   ├── endpoint (ep)
│   ├── bootstrap (b)
│   ├── log (o)
│   ├── secret (s)
│   ├── all (a)
│   ├── ecds
│   ├── eds
│   └── rootca-compare
├── analyze                    # 설정 분석
├── kube-inject                # 사이드카 인젝션
├── install (apply)            # 설치
├── uninstall                  # 제거
├── upgrade                    # 업그레이드
├── manifest                   # 매니페스트 생성
│   ├── generate
│   └── diff
├── dashboard (dash, d)        # 대시보드
│   ├── kiali
│   ├── prometheus
│   ├── grafana
│   ├── jaeger
│   ├── zipkin
│   ├── skywalking
│   ├── envoy (deprecated)
│   ├── proxy
│   ├── controlz
│   └── istiod-debug
├── version                    # 버전 정보
├── validate                   # YAML 검증
├── tag                        # 리비전 태그
│   ├── set
│   ├── generate
│   ├── list
│   └── remove
├── waypoint                   # Ambient 웨이포인트
├── ztunnel-config             # ztunnel 설정
├── remote-clusters            # 원격 클러스터
├── create-remote-secret       # 멀티클러스터 시크릿
├── bug-report                 # 버그 리포트
└── experimental (x, exp)      # 실험적 명령어
    ├── authz check
    ├── metrics (m)
    ├── describe
    ├── config
    ├── workload
    ├── internal-debug
    ├── precheck
    ├── envoy-stats (es)
    ├── check-inject
    ├── injector
    ├── version
    └── proxy-status
```

---

## 4. proxy-status (ps): xDS 동기화 상태 조회

### 개요

`proxy-status`는 Istiod가 각 Envoy 프록시에 마지막으로 보낸 xDS 설정과 프록시가 ACK한 상태의 차이를 보여준다. 메시 전체의 설정 전파 상태를 한눈에 파악할 수 있는 핵심 진단 명령이다.

소스 파일: `istioctl/pkg/proxystatus/proxystatus.go`

### 두 가지 동작 모드

**모드 1: 전체 메시 동기화 상태 (인자 없음)**

```bash
# 모든 Envoy의 동기화 상태 조회
istioctl proxy-status

# 특정 네임스페이스만
istioctl proxy-status --namespace foo

# JSON 출력
istioctl proxy-status --output json

# 상세 출력 (모든 xDS 타입 표시)
istioctl proxy-status -v 1
```

인자 없이 실행하면 `TypeDebugSyncronization` 타입의 xDS 요청을 모든 Istiod 인스턴스에 보낸다:

```go
// 인자가 없는 경우 - 전체 동기화 상태 조회
xdsRequest := discovery.DiscoveryRequest{
    TypeUrl: pilotxds.TypeDebugSyncronization,  // "istio.io/debug/syncz"
}
xdsResponses, err := multixds.AllRequestAndProcessXds(
    &xdsRequest, centralOpts, ctx.IstioNamespace(),
    "", "", kubeClient, multiXdsOpts,
)
```

`TypeDebugSyncronization`은 Istiod의 StatusGen이 처리하는 커스텀 디버그 타입으로, 소스 파일 `pilot/pkg/xds/statusgen.go`에 다음과 같이 정의되어 있다:

```go
const (
    TypeDebugSyncronization = v3.DebugType + "/syncz"    // "istio.io/debug/syncz"
    TypeDebugConfigDump     = v3.DebugType + "/config_dump"
)
```

응답은 `XdsStatusWriter`를 통해 테이블 형태로 출력된다:

```go
sw := pilot.XdsStatusWriter{
    Writer:       c.OutOrStdout(),
    Namespace:    ctx.Namespace(),
    OutputFormat: outputFormat,
    Verbosity:    verbosity,
}
return sw.PrintAll(xdsResponses)
```

출력 예시:

```
NAME                        CLUSTER        CDS     LDS     EDS     RDS     ECDS    ISTIOD                    VERSION
httpbin-74fb669cc6-abc.ns   Kubernetes     SYNCED  SYNCED  SYNCED  SYNCED  -       istiod-1234-5678.istio    1.20.0
productpage-v1-abc.ns       Kubernetes     SYNCED  SYNCED  SYNCED  SYNCED  -       istiod-1234-5678.istio    1.20.0
```

**모드 2: 단일 프록시 diff 비교 (인자 있음)**

```bash
# 특정 프록시의 동기화 diff 확인
istioctl proxy-status istio-egressgateway-59585c5b9c-ndc59.istio-system

# 파일 기반 비교
istioctl proxy-status my-pod.default --file envoy-config.json
```

특정 Pod를 인자로 지정하면 두 소스를 비교한다:

```go
if len(args) > 0 {
    // 1. Envoy admin API에서 실제 config_dump 가져오기
    envoyDump, err = kubeClient.EnvoyDoWithPort(
        context.TODO(), podName, ns, "GET", "config_dump", proxyAdminPort,
    )

    // 2. Istiod에서 해당 프록시의 기대 설정 가져오기
    xdsRequest := discovery.DiscoveryRequest{
        ResourceNames: []string{fmt.Sprintf("%s.%s", podName, ns)},
        TypeUrl:       pilotxds.TypeDebugConfigDump,  // "istio.io/debug/config_dump"
    }
    xdsResponses, err := multixds.FirstRequestAndProcessXds(
        &xdsRequest, centralOpts, ctx.IstioNamespace(), ...
    )

    // 3. 두 설정을 비교
    c, err := compare.NewXdsComparator(c.OutOrStdout(), xdsResponses, envoyDump)
    return c.Diff()
}
```

**왜 이렇게 설계했는가?** `AllRequestAndProcessXds`와 `FirstRequestAndProcessXds`를 구분하는 이유는, 전체 상태 조회는 모든 Istiod 인스턴스에서 응답을 수집해야 하지만, 단일 프록시 diff는 해당 프록시가 연결된 하나의 Istiod에서만 응답이 오면 되기 때문이다.

### StableXdsStatusCommand vs XdsStatusCommand

```go
func StableXdsStatusCommand(ctx cli.Context) *cobra.Command {
    cmd := XdsStatusCommand(ctx)
    unstableFlags := []string{}
    cmd.PreRunE = func(cmd *cobra.Command, args []string) error {
        for _, flag := range unstableFlags {
            if cmd.PersistentFlags().Changed(flag) {
                return fmt.Errorf("--%s is experimental. Use `istioctl experimental ps --%s`", flag, flag)
            }
        }
        return nil
    }
    // ...
}
```

안정(stable) 버전은 실험적 플래그를 숨기고, `experimental` 서브커맨드의 버전은 모든 플래그를 노출한다. 이 방식으로 신규 기능을 점진적으로 졸업(graduation)시킨다.

---

## 5. proxy-config (pc): Envoy 설정 덤프

### 개요

`proxy-config`는 Envoy 프록시의 내부 설정을 직접 조회하는 명령 그룹이다. Envoy의 admin API `/config_dump` 엔드포인트를 사용하여 클러스터, 리스너, 라우트, 엔드포인트 등의 세부 설정을 추출한다.

소스 파일: `istioctl/pkg/proxyconfig/proxyconfig.go`

### 핵심 함수: extractConfigDump

```go
func extractConfigDump(kubeClient kube.CLIClient, podName, podNamespace string, additionPath string) ([]byte, error) {
    path := "config_dump" + additionPath
    debug, err := kubeClient.EnvoyDoWithPort(
        context.TODO(), podName, podNamespace, "GET", path, proxyAdminPort,
    )
    if err != nil {
        return nil, fmt.Errorf("failed to execute command on %s.%s sidecar: %v", podName, podNamespace, err)
    }
    return debug, err
}
```

`EnvoyDoWithPort`는 Kubernetes exec API를 통해 Pod 내부의 Envoy admin 포트(기본 15000)로 HTTP 요청을 보낸다.

### 서브커맨드별 config_dump 필터 경로

각 서브커맨드는 config_dump의 특정 부분만 요청한다. 이는 Envoy admin API의 mask 파라미터를 사용한다:

```go
const (
    edsPath       = "?include_eds=true"
    secretPath    = "?mask=dynamic_active_secrets,dynamic_warming_secrets"
    clusterPath   = "?mask=dynamic_active_clusters,dynamic_warming_clusters,static_clusters"
    listenerPath  = "?mask=dynamic_listeners,static_listeners"
    routePath     = "?mask=dynamic_route_configs,static_route_configs"
    bootstrapPath = "?mask=bootstrap"
)
```

**왜 mask를 사용하는가?** 전체 config_dump는 대규모 메시에서 수십 MB에 달할 수 있다. mask 파라미터로 필요한 부분만 요청하면 네트워크 대역폭과 파싱 시간을 크게 줄인다.

### cluster 서브커맨드

```bash
# 클러스터(업스트림 서비스) 요약 조회
istioctl proxy-config clusters httpbin-abc.default

# 특정 포트 필터링
istioctl proxy-config clusters httpbin-abc.default --port 9080

# 특정 FQDN과 방향 필터, JSON 출력
istioctl proxy-config clusters httpbin-abc.default \
    --fqdn details.default.svc.cluster.local --direction inbound -o json

# 파일 기반 (Kubernetes 없이)
istioctl proxy-config clusters --file envoy-config.json
```

내부적으로 `ConfigWriter`를 통해 출력을 포맷팅한다:

```go
configWriter, err = setupPodConfigdumpWriter(kubeClient, podName, podNamespace, clusterPath, c.OutOrStdout())
filter := configdump.ClusterFilter{
    FQDN:      host.Name(fqdn),
    Port:      port,
    Subset:    subset,
    Direction: model.TrafficDirection(direction),
}
switch outputFormat {
case summaryOutput:
    return configWriter.PrintClusterSummary(filter)
case jsonOutput, yamlOutput:
    return configWriter.PrintClusterDump(filter, outputFormat)
}
```

### listener 서브커맨드

```bash
# 리스너 요약
istioctl proxy-config listeners httpbin-abc.default

# 포트와 타입 필터
istioctl proxy-config listeners httpbin-abc.default --port 9080 --type HTTP

# 와일드카드 주소 필터, JSON 출력
istioctl proxy-config listeners httpbin-abc.default --address 0.0.0.0 -o json
```

### route 서브커맨드

```bash
# 라우트 요약
istioctl proxy-config routes httpbin-abc.default

# 이름으로 필터
istioctl proxy-config routes httpbin-abc.default --name 9080
```

### log 서브커맨드

Envoy 프록시의 런타임 로깅 레벨을 조회하고 변경한다:

```bash
# 현재 로깅 레벨 조회
istioctl proxy-config log httpbin-abc.default

# 전체 로거 레벨 변경
istioctl proxy-config log httpbin-abc.default --level debug

# 특정 로거만 변경
istioctl proxy-config log httpbin-abc.default --level http:debug,redis:debug

# 기본값(warning)으로 리셋
istioctl proxy-config log httpbin-abc.default -r
```

Envoy의 `/logging` admin API 엔드포인트에 POST 요청을 보낸다:

```go
func setupEnvoyLogConfig(kubeClient kube.CLIClient, param, podName, podNamespace string) (string, error) {
    path := "logging"
    if param != "" {
        path = path + "?" + param
    }
    result, err := kubeClient.EnvoyDoWithPort(
        context.TODO(), podName, podNamespace, "POST", path, proxyAdminPort,
    )
    return string(result), nil
}
```

로깅 레벨은 7단계로 정의되어 있다:

```go
const (
    OffLevel Level = iota
    CriticalLevel
    ErrorLevel
    WarningLevel     // 기본값
    InfoLevel
    DebugLevel
    TraceLevel
)
```

### all 서브커맨드

모든 설정(클러스터, 리스너, 라우트, 엔드포인트)을 한 번에 출력한다:

```bash
# 전체 설정 요약
istioctl proxy-config all httpbin-abc.default

# JSON 형태의 전체 덤프
istioctl proxy-config all httpbin-abc.default -o json
```

### ProxyConfig 명령 등록 구조

```go
func ProxyConfig(ctx cli.Context) *cobra.Command {
    configCmd := &cobra.Command{
        Use:     "proxy-config",
        Aliases: []string{"pc"},
        // ...
    }
    configCmd.PersistentFlags().IntVar(&proxyAdminPort, "proxy-admin-port",
        istioctlutil.DefaultProxyAdminPort, "Envoy proxy admin port")

    configCmd.AddCommand(clusterConfigCmd(ctx))
    configCmd.AddCommand(allConfigCmd(ctx))
    configCmd.AddCommand(listenerConfigCmd(ctx))
    configCmd.AddCommand(logCmd(ctx))
    configCmd.AddCommand(routeConfigCmd(ctx))
    configCmd.AddCommand(bootstrapConfigCmd(ctx))
    configCmd.AddCommand(endpointConfigCmd(ctx))
    configCmd.AddCommand(edsConfigCmd(ctx))
    configCmd.AddCommand(secretConfigCmd(ctx))
    configCmd.AddCommand(rootCACompareConfigCmd(ctx))
    configCmd.AddCommand(ecdsConfigCmd(ctx))
    return configCmd
}
```

### config_dump 처리 파이프라인

```
사용자 명령
    │
    v
getPodName()               # Pod 이름/네임스페이스 추론
    │
    v
extractConfigDump()        # kubeClient.EnvoyDoWithPort("GET", "config_dump"+mask)
    │                         (k8s exec -> Pod 내부 Envoy admin:15000)
    v
setupConfigdumpEnvoyConfigWriter()
    │
    ├──> cw.Prime(debug)   # JSON 파싱 및 내부 구조 구성
    │
    v
configWriter.PrintXxxSummary(filter)   # 필터 적용 후 테이블/JSON/YAML 출력
```

---

## 6. analyze: 설정 분석 및 검증

### 개요

`analyze` 명령은 Istio 설정의 잠재적 문제를 탐지하는 정적 분석 도구이다. 라이브 클러스터, 로컬 YAML 파일, 또는 둘의 조합을 분석할 수 있다.

소스 파일: `istioctl/pkg/analyze/analyze.go`

### 사용 예시

```bash
# 라이브 클러스터 분석
istioctl analyze

# 특정 리비전 분석
istioctl analyze --revision 1-16

# 로컬 파일 분석 (클러스터 연결 없이)
istioctl analyze --use-kube=false a.yaml b.yaml

# 라이브 클러스터 + 추가 YAML 시뮬레이션
istioctl analyze a.yaml b.yaml my-app-config/

# 특정 메시지 억제
istioctl analyze -S "IST0103=Pod mypod.testing"

# 와일드카드로 네임스페이스 전체 억제
istioctl analyze -S "IST0103=Pod *.testing" -S "IST0107=Deployment foobar.default"

# 사용 가능한 분석기 목록
istioctl analyze -L

# 특정 분석기만 실행
istioctl analyze --analyzer "gateway.ConflictingGatewayAnalyzer"

# 모든 네임스페이스 분석
istioctl analyze -A

# JSON 출력
istioctl analyze -o json
```

### 분석 파이프라인

```go
// 1. Combined Analyzer 생성
combinedAnalyzers := analyzers.AllCombined()
if len(selectedAnalyzers) != 0 {
    combinedAnalyzers = analyzers.NamedCombined(selectedAnalyzers...)
}

// 2. IstiodAnalyzer 초기화
sa := local.NewIstiodAnalyzer(combinedAnalyzers,
    resource.Namespace(selectedNamespace),
    resource.Namespace(ctx.IstioNamespace()), nil)

// 3. Suppression 설정
sa.SetSuppressions(suppressions)

// 4. 소스 추가
if useKube {
    sa.AddRunningKubeSourceWithRevision(k, revisionSpecified, c.remote)
}
if len(readers) > 0 {
    sa.AddReaderKubeSource(readers)
}

// 5. 분석 실행
result, err := sa.Analyze(cancel)

// 6. 결과 필터링 및 출력
outputMessages := result.Messages.
    SetDocRef("istioctl-analyze").
    FilterOutLowerThan(outputThreshold.Level)
output, err := formatting.Print(outputMessages, msgOutputFormat, colorize)
```

### 분석기 아키텍처

```
analyze 명령
    │
    v
local.NewIstiodAnalyzer()
    │
    ├──> AddRunningKubeSourceWithRevision()   # 라이브 클러스터 데이터
    ├──> AddReaderKubeSource(readers)          # 로컬 YAML 파일
    ├──> AddFileKubeMeshConfig(meshCfgFile)   # 커스텀 메시 설정
    │
    v
sa.Analyze(cancel)
    │
    ├──> Gateway 분석기들                      # Gateway 충돌 감지
    ├──> VirtualService 분석기들               # 라우팅 규칙 검증
    ├──> DestinationRule 분석기들              # 트래픽 정책 검증
    ├──> AuthorizationPolicy 분석기들          # 보안 정책 검증
    ├──> Injection 분석기들                    # 사이드카 인젝션 검증
    └──> ...기타 분석기들
    │
    v
result.Messages                               # IST0xxx 형태의 진단 메시지
```

### 심각도 레벨과 임계값

```go
var (
    failureThreshold = formatting.MessageThreshold{Level: diag.Error}  // exit code 결정
    outputThreshold  = formatting.MessageThreshold{Level: diag.Info}   // 출력 필터
)
```

- `Info`: 정보성 메시지 (기본 출력에 포함)
- `Warning`: 잠재적 문제
- `Error`: 명확한 설정 오류 (기본적으로 비정상 종료 코드 발생)

**왜 failureThreshold와 outputThreshold를 분리했는가?** CI/CD 파이프라인에서 Warning은 보여주되 Error만 빌드를 실패시키고 싶은 사용자가 있기 때문이다. 예를 들어 `--failure-threshold Warning`으로 설정하면 Warning도 비정상 종료를 유발한다.

### 멀티클러스터 분석

```go
func getClients(ctx cli.Context) ([]*Client, error) {
    client, err := ctx.CLIClient()
    clients := []*Client{{client: client, remote: false}}

    // remoteContexts가 지정된 경우
    if len(remoteContexts) > 0 {
        remoteClients, err := getClientsFromContexts(ctx)
        clients = append(clients, remoteClients...)
        return clients, nil
    }

    // 또는 클러스터의 remote secrets에서 자동 탐색
    secrets, err := client.Kube().CoreV1().Secrets(ctx.IstioNamespace()).List(
        context.Background(), metav1.ListOptions{
            LabelSelector: fmt.Sprintf("%s=%s", multicluster.MultiClusterSecretLabel, "true"),
        },
    )
    // 각 시크릿에서 kubeconfig를 추출하여 원격 클라이언트 생성
    // ...
}
```

### 파일 수집

분석 대상 파일은 `.json`, `.yaml`, `.yml` 확장자만 처리한다:

```go
var fileExtensions = []string{".json", ".yaml", ".yml"}

func gatherFiles(cmd *cobra.Command, args []string) ([]local.ReaderSource, error) {
    for _, f := range args {
        if f == "-" {
            // stdin 처리
        } else if fi.IsDir() {
            // 디렉토리 재귀 탐색
            dirReaders, err := gatherFilesInDirectory(cmd, f)
        } else {
            // 개별 파일
            rs, err := gatherFile(f)
        }
    }
    return readers, nil
}
```

---

## 7. kube-inject: 수동 사이드카 인젝션

### 개요

`kube-inject`는 Kubernetes 리소스 YAML에 Istio 사이드카 프록시를 수동으로 주입하는 명령이다. 자동 인젝션(MutatingWebhook)이 불가능한 환경이나, 인젝션 결과를 사전에 확인하고 싶을 때 사용한다.

소스 파일: `istioctl/pkg/kubeinject/kubeinject.go`

### 사용 예시

```bash
# 즉석에서 인젝션 후 적용
kubectl apply -f <(istioctl kube-inject -f deployment.yaml)

# 인젝션된 결과를 파일로 저장
istioctl kube-inject -f deployment.yaml -o deployment-injected.yaml

# 파이프라인으로 기존 배포 업데이트
kubectl get deployment -o yaml | istioctl kube-inject -f - | kubectl apply -f -

# 커스텀 설정 사용
istioctl kube-inject -f bookinfo.yaml \
    --injectConfigFile /tmp/inj-template.tmpl \
    --meshConfigFile /tmp/mesh.yaml \
    --valuesFile /tmp/values.json

# IstioOperator 파일 기반 설정
istioctl kube-inject -f deployment.yaml \
    --operatorFileName my-iop.yaml
```

### ExternalInjector 구조체

`ExternalInjector`는 Istiod의 webhook 엔드포인트에 직접 인젝션 요청을 보내는 핵심 구조체이다:

```go
type ExternalInjector struct {
    client          kube.CLIClient
    clientConfig    *admissionregistration.WebhookClientConfig
    injectorAddress string
}
```

### 인젝션 흐름

```
istioctl kube-inject -f deployment.yaml
    │
    v
validateFlags()                         # 입력 파일 필수 확인
    │
    v
setupKubeInjectParameters()
    │
    ├──> getIOPConfigs()                # IstioOperator 파일에서 설정 추출
    │    └──> meshConfig, valuesConfig
    │
    ├──> meshConfig 결정 순서:
    │    1. IstioOperator 파일의 spec.meshConfig
    │    2. --meshConfigFile 플래그
    │    3. ConfigMap "istio" (클러스터에서)
    │
    ├──> sidecarTemplate 결정 순서:
    │    1. --injectConfigFile 플래그
    │    2. ExternalInjector (MutatingWebhookConfiguration에서)
    │    3. ConfigMap "istio-sidecar-injector" (폴백)
    │
    v
inject.IntoResourceFile(injector, templs, vc, rev, meshConfig, reader, writer, ...)
    │
    └──> 각 리소스에 대해:
         ├──> Deployment, Pod 등 지원되는 타입 식별
         ├──> 사이드카 컨테이너 추가
         ├──> initContainer 추가 (iptables 설정)
         ├──> Volume/VolumeMount 추가
         └──> 결과 YAML 출력
```

### ExternalInjector.Inject() 메서드

이 메서드는 Istiod의 인젝션 webhook에 직접 AdmissionReview 요청을 보낸다:

```go
func (e ExternalInjector) Inject(pod *corev1.Pod, deploymentNS string) ([]byte, error) {
    // 1. 주소 결정
    cc := e.clientConfig
    if cc.Service != nil {
        // Service 기반 - port-forward를 통해 접근
        svc, _ := e.client.Kube().CoreV1().Services(cc.Service.Namespace).Get(...)
        pod, _ := GetFirstPod(e.client.Kube().CoreV1(), namespace, selector.String())
        f, _ := e.client.NewPortForwarder(pod.Name, pod.Namespace, "", 0, podPort)
        f.Start()
        address = fmt.Sprintf("https://%s%s", f.Address(), *cc.Service.Path)
    }

    // 2. AdmissionReview 구성
    rev := &admission.AdmissionReview{
        Request: &admission.AdmissionRequest{
            Object:    runtime.RawExtension{Raw: podBytes},
            Name:      pod.Name,
            Namespace: deploymentNS,
        },
    }

    // 3. HTTPS POST로 전송
    resp, err := client.Post(address, "application/json", bytes.NewBuffer(revBytes))

    // 4. 응답에서 패치 추출
    ar, err := kube.AdmissionReviewKubeToAdapter(out)
    return ar.Response.Patch, nil
}
```

**왜 ExternalInjector를 사용하는가?** 직접 Istiod의 인젝션 webhook을 호출하면 실제 자동 인젝션과 동일한 결과를 보장한다. ConfigMap에서 템플릿을 읽어 로컬에서 처리하는 것보다 Istiod의 최신 설정이 반영된다.

### MutatingWebhookConfiguration 조회

```go
func setUpExternalInjector(ctx cli.Context, revision, injectorAddress string) (*ExternalInjector, error) {
    whcList, err := client.Kube().AdmissionregistrationV1().
        MutatingWebhookConfigurations().List(context.TODO(),
            metav1.ListOptions{
                LabelSelector: fmt.Sprintf("%s=%s", label.IoIstioRev.Name, revision),
            },
        )
    for _, wh := range whcList.Items[0].Webhooks {
        if strings.HasSuffix(wh.Name, defaultWebhookName) {  // "sidecar-injector.istio.io"
            return &ExternalInjector{client, &wh.ClientConfig, injectorAddress}, nil
        }
    }
}
```

---

## 8. install: 설치 명령과 프로파일 시스템

### 개요

`install` 명령은 Istio 매니페스트를 생성하고 클러스터에 적용한다. IstioOperator API를 통해 설치 프로파일과 커스텀 설정을 지원한다.

소스 파일: `operator/cmd/mesh/install.go`

### 사용 예시

```bash
# 기본 설치
istioctl install

# demo 프로파일 (확인 생략)
istioctl install --set profile=demo --skip-confirmation

# Tracing 활성화
istioctl install --set meshConfig.enableTracing=true

# IstioOperator 파일 사용
istioctl install -f my-operator.yaml

# 특정 리비전으로 Canary 설치
istioctl install --revision canary

# 강제 설치 (유효성 검사 무시)
istioctl install --force

# 설치 후 검증
istioctl install --verify
```

### InstallArgs 구조체

```go
type InstallArgs struct {
    InFilenames      []string      // IstioOperator CR 파일 경로들
    ReadinessTimeout time.Duration // 리소스 준비 대기 시간 (기본 300초)
    SkipConfirmation bool          // 확인 프롬프트 생략
    Force            bool          // 유효성 검사 오류 무시
    Verify           bool          // 설치 후 검증
    Set              []string      // "path=value" 형태의 설정 덮어쓰기
    ManifestsPath    string        // 로컬 매니페스트 경로
    Revision         string        // 컨트롤 플레인 리비전
}
```

### 관련 명령어

| 명령어 | 설명 |
|--------|------|
| `istioctl install` | 설치/재설정 |
| `istioctl uninstall` | 제거 |
| `istioctl upgrade` | 업그레이드 |
| `istioctl manifest generate` | 매니페스트 생성 (적용하지 않음) |
| `istioctl manifest diff` | 두 매니페스트 비교 |

### 프로파일 시스템

Istio는 사전 정의된 설치 프로파일을 제공한다:

```bash
# 사용 가능한 프로파일
# default: 운영 환경 추천
# demo: 모든 기능 활성화 (테스트용)
# minimal: 최소 기능
# remote: 멀티클러스터의 원격 클러스터
# empty: 아무것도 설치하지 않음
# ambient: Ambient 메시 모드

istioctl install --set profile=demo
```

---

## 9. dashboard: 대시보드 접근

### 개요

`dashboard` 명령은 Istio 에코시스템의 웹 UI에 포트포워딩을 설정하고 브라우저를 자동으로 여는 편의 도구이다.

소스 파일: `istioctl/pkg/dashboard/dashboard.go`

### 지원 대시보드

| 서브커맨드 | 기본 포트 | 레이블 셀렉터 | 설명 |
|------------|-----------|---------------|------|
| `kiali` | 20001 | `app=kiali` | Kiali 서비스 메시 시각화 |
| `prometheus` | 9090 | `app.kubernetes.io/name=prometheus` | Prometheus 모니터링 |
| `grafana` | 3000 | `app.kubernetes.io/name=grafana` | Grafana 대시보드 |
| `jaeger` | 16686 | `app=jaeger` | Jaeger 분산 트레이싱 |
| `zipkin` | 9411 | `app=zipkin` | Zipkin 분산 트레이싱 |
| `skywalking` | 8080 | `app=skywalking-ui` | SkyWalking APM |
| `envoy` | 15000 | Pod 직접 지정 | Envoy admin UI (deprecated) |
| `proxy` | 15000 | Pod 직접 지정 | Envoy/ztunnel admin UI |
| `controlz` | 9876 | Pod/리비전 | ControlZ 디버그 UI |
| `istiod-debug` | 15014 | Pod/리비전 | Istiod 디버그 페이지 |

### 사용 예시

```bash
# Kiali 대시보드
istioctl dashboard kiali
istioctl dash kiali     # 단축 별칭
istioctl d kiali        # 더 짧은 별칭

# Grafana 대시보드
istioctl dashboard grafana

# 특정 주소와 포트로 바인딩
istioctl dashboard grafana --address 0.0.0.0 --port 8080

# 브라우저 자동 열기 비활성화
istioctl dashboard grafana --browser=false

# Envoy 프록시 admin UI
istioctl dashboard proxy productpage-v1-abc.default

# Istiod 디버그 (리비전 지정)
istioctl dashboard istiod-debug --revision canary
```

### 포트포워딩 메커니즘

```go
func portForward(podName, namespace, flavor, urlFormat, localAddress string,
    remotePort int, client kube.CLIClient, writer io.Writer, browser bool) error {

    // 포트 우선순위:
    // 1. --listenPort가 지정되면 해당 포트 사용
    // 2. 아니면 원격 포트를 로컬에도 사용 시도, 실패 시 랜덤 포트
    var portPrefs []int
    if listenPort != 0 {
        portPrefs = []int{listenPort}
    } else {
        portPrefs = []int{remotePort, 0}
    }

    for _, localPort := range portPrefs {
        fw, err := client.NewPortForwarder(podName, namespace, localAddress, localPort, remotePort)
        if err = fw.Start(); err != nil {
            fw.Close()
            continue  // 다음 포트 시도
        }
        ClosePortForwarderOnInterrupt(fw)
        openBrowser(fmt.Sprintf(urlFormat, fw.Address()), writer, browser)
        fw.WaitForStop()
        return nil
    }
    return fmt.Errorf("failure running port forward process: %v", err)
}
```

### Pod 자동 탐색

```go
func inferPodMeta(ctx cli.Context, client kube.CLIClient, labelSelector string) (name, namespace string, err error) {
    for _, ns := range []string{ctx.IstioNamespace(), ctx.NamespaceOrDefault(ctx.Namespace())} {
        pl, err := client.PodsForSelector(context.TODO(), ns, labelSelector)
        if err != nil { continue }
        if len(pl.Items) > 0 {
            return pl.Items[0].Name, pl.Items[0].Namespace, nil
        }
    }
    return "", "", fmt.Errorf("no pods found with selector %s", labelSelector)
}
```

`inferPodMeta`는 먼저 `istio-system` 네임스페이스에서 찾고, 없으면 현재 네임스페이스에서 찾는다. 이렇게 하면 대부분의 경우 네임스페이스를 명시적으로 지정하지 않아도 된다.

### 브라우저 열기

```go
func openBrowser(url string, writer io.Writer, browser bool) {
    fmt.Fprintf(writer, "%s\n", url)
    if !browser { return }

    switch runtime.GOOS {
    case "linux":
        err = exec.Command("xdg-open", url).Start()
    case "windows":
        err = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
    case "darwin":
        err = exec.Command("open", url).Start()
    }
}
```

---

## 10. version: 버전 정보

### 개요

`version` 명령은 istioctl 클라이언트, Istiod 컨트롤 플레인, Envoy 프록시의 버전 정보를 표시한다.

소스 파일: `istioctl/pkg/version/version.go`

### 사용 예시

```bash
# 버전 정보 (클라이언트 + 서버)
istioctl version

# 클라이언트 버전만
istioctl version --remote=false

# JSON 출력
istioctl version -o json

# xDS 기반 버전 조회 (실험적)
istioctl x version
```

### 두 가지 버전 명령

**1. 안정 버전 (NewVersionCommand)**

```go
func NewVersionCommand(ctx cli.Context) *cobra.Command {
    versionCmd = istioVersion.CobraCommandWithOptions(istioVersion.CobraOptions{
        GetRemoteVersion: getRemoteInfoWrapper(ctx, &versionCmd, &opts),
        GetProxyVersions: getProxyInfoWrapper(ctx, &opts),
    })
    // 기본적으로 --short=true, --remote=true
}
```

서버 정보는 `kubeClient.GetIstioVersions()`를 통해 Istiod Pod의 컨테이너 이미지에서 추출하고, 프록시 버전은 `proxy.GetProxyInfo()`를 통해 수집한다.

**2. xDS 기반 버전 (XdsVersionCommand)**

```go
func XdsVersionCommand(ctx cli.Context) *cobra.Command {
    versionCmd := istioVersion.CobraCommandWithOptions(istioVersion.CobraOptions{
        GetRemoteVersion: xdsRemoteVersionWrapper(ctx, &opts, &centralOpts, &xdsResponses),
        GetProxyVersions: xdsProxyVersionWrapper(&xdsResponses),
    })
}
```

xDS 기반 버전은 `TypeDebugSyncronization` 요청을 통해 Istiod에 직접 질의한다. 응답의 `ControlPlane.Identifier`에서 컨트롤 플레인 정보를 파싱하고, 각 리소스의 Node 메타데이터에서 프록시 버전을 추출한다:

```go
func getIstioVersionFromXdsMetadata(metadata *structpb.Struct) string {
    meta, err := model.ParseMetadata(metadata)
    if err != nil {
        return "unknown sidecar version"
    }
    return meta.IstioVersion
}
```

---

## 11. experimental: 실험적 명령어

### 개요

`experimental` (별칭: `x`, `exp`) 서브커맨드는 아직 졸업하지 않은 실험적 기능을 포함한다. 이 기능들은 향후 변경되거나 제거될 수 있다.

### authz check

소스 파일: `istioctl/pkg/authz/authz.go`

Pod에 적용된 AuthorizationPolicy를 Envoy config dump에서 직접 확인한다:

```bash
# Pod의 AuthorizationPolicy 확인
istioctl x authz check httpbin-88ddbcfdd-nt5jb

# Deployment 기반
istioctl x authz check deployment/productpage-v1

# 파일 기반
istioctl x authz check -f httpbin_config_dump.json
```

내부 동작:

```go
func checkCmd(ctx cli.Context) *cobra.Command {
    // ...
    RunE: func(cmd *cobra.Command, args []string) error {
        var configDump *configdump.Wrapper
        if configDumpFile != "" {
            configDump, err = getConfigDumpFromFile(configDumpFile)
        } else if len(args) == 1 {
            configDump, err = getConfigDumpFromPod(kubeClient, podName, podNamespace, proxyAdminPort)
        }
        analyzer, err := NewAnalyzer(configDump)
        analyzer.Print(cmd.OutOrStdout())
        return nil
    },
}
```

`getConfigDumpFromPod`는 Envoy admin API에 `config_dump`를 요청하고, `NewAnalyzer`가 RBAC 필터 설정을 파싱하여 적용된 정책을 표시한다.

### describe

소스 파일: `istioctl/pkg/describe/describe.go`

서비스나 Pod에 대한 Istio 관점의 종합적인 설명을 제공한다. VirtualService, DestinationRule, AuthorizationPolicy, PeerAuthentication 등이 어떻게 적용되는지 보여준다.

```bash
# Pod 설명
istioctl x describe pod httpbin-abc.default

# 서비스 설명
istioctl x describe svc httpbin.default
```

이 명령은 Envoy config dump와 Istio CRD를 모두 조합하여 트래픽 라우팅 경로, mTLS 설정 상태, 적용된 정책 등을 종합적으로 보여주는 가장 풍부한 진단 정보를 제공한다.

### metrics

소스 파일: `istioctl/pkg/metrics/metrics.go`

Prometheus에 직접 질의하여 워크로드의 핵심 메트릭을 표시한다:

```bash
# 단일 워크로드 메트릭
istioctl x metrics productpage-v1

# 여러 워크로드
istioctl x metrics productpage-v1 reviews-v1

# 커스텀 시간 범위
istioctl x metrics productpage-v1 -d 2m

# 네임스페이스 지정
istioctl x metrics productpage-v1.foo reviews-v1.bar
```

질의하는 메트릭:

```go
const (
    destWorkloadLabel          = "destination_workload"
    destWorkloadNamespaceLabel = "destination_workload_namespace"
    reqTot                     = "istio_requests_total"
    reqDur                     = "istio_request_duration_milliseconds"
)
```

| 메트릭 | 쿼리 | 설명 |
|--------|------|------|
| Total RPS | `sum(rate(istio_requests_total{...}[1m]))` | 초당 총 요청 수 |
| Error RPS | `sum(rate(istio_requests_total{...,response_code=~"[45][0-9]{2}"}[1m]))` | 초당 에러 요청 수 |
| P50 Latency | `histogram_quantile(0.5, sum(rate(istio_request_duration_milliseconds_bucket{...}[1m])) by (le))` | 50번째 백분위 지연시간 |
| P90 Latency | 위와 동일 (0.9) | 90번째 백분위 지연시간 |
| P99 Latency | 위와 동일 (0.99) | 99번째 백분위 지연시간 |

출력 예시:

```
                          WORKLOAD   TOTAL RPS   ERROR RPS   P50 LATENCY   P90 LATENCY   P99 LATENCY
                  productpage-v1       1.234       0.000         4ms          12ms          45ms
```

**왜 Prometheus에 직접 질의하는가?** istioctl은 포트포워딩을 통해 Prometheus Pod에 접근하고, Prometheus API를 사용하여 사전 정의된 PromQL 쿼리를 실행한다. 이렇게 하면 Grafana 없이도 빠르게 핵심 메트릭을 확인할 수 있다.

### precheck

소스 파일: `istioctl/pkg/precheck/precheck.go`

Istio 설치/업그레이드 전 클러스터가 요구사항을 충족하는지 검사한다:

```bash
# 설치/업그레이드 사전 검사
istioctl x precheck

# 특정 네임스페이스만
istioctl x precheck --namespace default

# 특정 버전 이후의 동작 변경 확인
istioctl x precheck --from-version 1.10
```

검사 항목: Kubernetes 버전 호환성, CRD 존재 여부, RBAC 권한, 기존 설치와의 호환성 등.

### tag

소스 파일: `istioctl/pkg/tag/tag.go`

리비전 태그를 생성하고 관리한다. 리비전 태그는 컨트롤 플레인 리비전에 대한 변경 가능한 별칭(alias)이다.

```bash
# 태그 생성/변경
istioctl tag set prod --revision 1-8-0

# 태그 목록
istioctl tag list

# 태그 제거
istioctl tag remove prod

# 태그 매니페스트 생성 (적용하지 않음)
istioctl tag generate prod --revision 1-8-0
```

**왜 리비전 태그가 필요한가?** 카나리 업그레이드 시 네임스페이스의 `istio.io/rev` 레이블을 `1-7-6`에서 `1-8-1`으로 변경하는 대신, `prod` 태그를 가리키는 리비전만 변경하면 된다. 이렇게 하면 네임스페이스 레이블을 수정하지 않고도 인젝션 리비전을 전환할 수 있다.

```
네임스페이스 레이블: istio.io/rev=prod
                                  │
                       tag "prod" │
                                  ▼
                          리비전 1-8-0 (변경 가능)
```

---

## 12. 설정 아키텍처와 Viper 통합

### 설정 소스의 우선순위

istioctl은 Viper를 통해 세 가지 설정 소스를 지원한다:

```
CLI 플래그 (최고 우선순위)
    │
    v
환경변수 (ISTIOCTL_XXX)
    │
    v
설정 파일 ($HOME/.istioctl/config.yaml)  (최저 우선순위)
```

### 환경변수 매핑 규칙

```go
viper.SetEnvPrefix("ISTIOCTL")
viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
```

이 설정으로 인해:
- `--istioNamespace` 플래그 -> `ISTIOCTL_ISTIONAMESPACE` 환경변수
- `--xds-port` 플래그 -> `ISTIOCTL_XDS_PORT` 환경변수
- `--cert-dir` 플래그 -> `ISTIOCTL_CERT_DIR` 환경변수

```bash
# 환경변수로 기본 네임스페이스 설정
export ISTIOCTL_ISTIONAMESPACE=custom-istio-system

# 환경변수로 인증서 디렉토리 비활성화
export ISTIOCTL_CERT_DIR=""
```

### 설정 파일 형식

기본 경로: `$HOME/.istioctl/config.yaml`

```yaml
# config.yaml 예시
istioNamespace: custom-istio-system
xds-port: 15012
```

### ISTIOCONFIG 환경변수

```go
var IstioConfig = env.Register("ISTIOCONFIG", defaultIstioctlConfig,
    "Default values for istioctl flags").Get()
```

기본 설정 파일 경로를 `ISTIOCONFIG` 환경변수로 변경할 수 있다:

```bash
export ISTIOCONFIG=/etc/istioctl/custom-config.yaml
```

### 로깅 설정

소스 파일: `istioctl/pkg/root/root.go`

```go
func defaultLogOptions() *log.Options {
    o := log.DefaultOptions()
    o.SetDefaultOutputLevel("all", log.WarnLevel)      // 기본: Warning
    o.SetDefaultOutputLevel("validation", log.ErrorLevel)
    o.SetDefaultOutputLevel("processing", log.ErrorLevel)
    o.SetDefaultOutputLevel("kube", log.ErrorLevel)
    return o
}
```

istioctl은 기본적으로 Warning 이상의 로그만 출력한다. CLI 도구이므로 과도한 로그는 사용자 경험을 방해하기 때문이다.

### PREFER-EXPERIMENTAL 모드

```go
if viper.GetBool("PREFER-EXPERIMENTAL") {
    // 실험적 명령어를 루트 레벨에 배치
    for _, c := range xdsBasedTroubleshooting {
        rootCmd.AddCommand(c)
    }
    debugCmdAttachmentPoint = legacyCmd
} else {
    // 기본: 안정 버전을 루트에, 실험적 버전을 x/ 하위에
    debugCmdAttachmentPoint = rootCmd
}
```

이 설정을 활성화하면 실험적 명령어가 최상위에 배치되고 기존 명령어가 `legacy` 하위로 이동한다.

---

## 13. CLI Context 시스템

### Context 인터페이스

소스 파일: `istioctl/pkg/cli/context.go`

`cli.Context`는 모든 명령어가 공유하는 Kubernetes 클라이언트와 네임스페이스 정보를 관리하는 중앙 인터페이스이다:

```go
type Context interface {
    CLIClient() (kube.CLIClient, error)
    CLIClientWithRevision(rev string) (kube.CLIClient, error)
    RevisionOrDefault(rev string) string
    InferPodInfoFromTypedResource(name, namespace string) (pod string, ns string, err error)
    InferPodsFromTypedResource(name, namespace string) ([]string, string, error)
    Namespace() string
    IstioNamespace() string
    NamespaceOrDefault(namespace string) string
    CLIClientsForContexts(contexts []string) ([]kube.CLIClient, error)
}
```

### 클라이언트 캐싱

```go
type instance struct {
    clients        map[string]kube.CLIClient      // 리비전별 캐시
    remoteClients  map[string]kube.CLIClient      // 컨텍스트별 캐시
    defaultWatcher revisions.DefaultWatcher        // 기본 리비전 감시
    RootFlags
}
```

**왜 클라이언트를 캐싱하는가?** 동일한 리비전에 대한 반복적인 클라이언트 생성을 방지한다. 특히 여러 서브커맨드가 같은 리비전의 Istiod와 통신해야 할 때 효율적이다.

### 클라이언트 생성

```go
func newKubeClientWithRevision(kubeconfig, configContext, revision string,
    timeout time.Duration, impersonateConfig rest.ImpersonationConfig) (kube.CLIClient, error) {

    rc, err := kube.DefaultRestConfig(kubeconfig, configContext, func(config *rest.Config) {
        config.QPS = 50     // 높은 QPS로 설치 속도 향상
        config.Burst = 100
        config.Impersonate = impersonateConfig
    })
    return kube.NewCLIClient(
        kube.NewClientConfigForRestConfig(rc),
        kube.WithRevision(revision),
        kube.WithCluster(cluster.ID(configContext)),
        kube.WithTimeout(timeout),
    )
}
```

QPS/Burst를 기본값보다 높게 설정한 이유는, istioctl이 일회성 명령을 실행하는 로컬 도구이므로 API 서버 레이트 리밋에 덜 민감하고, 설치 시간을 줄이는 것이 더 중요하기 때문이다.

### 루트 플래그

```
--namespace, -n        작업 대상 네임스페이스
--istioNamespace, -i   Istio 설치 네임스페이스 (기본: istio-system)
--kubeconfig           kubeconfig 파일 경로
--context              kubeconfig 컨텍스트
--impersonate          사용자 가장 (RBAC 테스트용)
--impersonate-uid      가장할 사용자 UID
--impersonate-group    가장할 그룹
```

### Pod 이름 추론

`InferPodInfoFromTypedResource`는 다양한 형태의 입력을 Pod 이름으로 변환한다:

```bash
# 직접 Pod 이름
istioctl pc clusters httpbin-abc.default

# Deployment를 통한 간접 지정
istioctl pc clusters deployment/httpbin.default

# 네임스페이스 포함
istioctl pc clusters httpbin-abc.my-namespace
```

---

## 14. multixds: 다중 컨트롤 플레인 통신

### 개요

`multixds` 패키지는 여러 Istiod 인스턴스에 xDS 요청을 보내고 응답을 수집하는 기능을 제공한다. Istio는 고가용성을 위해 여러 Istiod 레플리카를 실행하며, 각 Envoy 프록시는 그 중 하나에만 연결된다.

소스 파일: `istioctl/pkg/multixds/gather.go`

### 세 가지 요청 패턴

```go
// 1. 모든 Istiod에 요청하고 응답을 합병 (deprecated)
func RequestAndProcessXds(dr *discovery.DiscoveryRequest, ...) (*discovery.DiscoveryResponse, error)

// 2. 모든 Istiod에 요청하고 개별 응답 반환
func AllRequestAndProcessXds(dr *discovery.DiscoveryRequest, ...) ([]*discovery.DiscoveryResponse, error)

// 3. 첫 번째 유효한 응답만 반환
func FirstRequestAndProcessXds(dr *discovery.DiscoveryRequest, ...) (*discovery.DiscoveryResponse, error)
```

### Istiod Pod 탐색

```go
func queryEachShard(all bool, dr *discovery.DiscoveryRequest, istioNamespace string,
    kubeClient kube.CLIClient, centralOpts clioptions.CentralControlPlaneOptions) ([]*discovery.DiscoveryResponse, error) {

    labelSelector := centralOpts.XdsPodLabel
    if labelSelector == "" {
        labelSelector = "app=istiod"
    }
    pods, err := kubeClient.GetIstioPods(context.TODO(), istioNamespace, metav1.ListOptions{
        LabelSelector: labelSelector,
        FieldSelector: kube.RunningStatus,
    })
    if len(pods) == 0 {
        return nil, ControlPlaneNotFoundError{istioNamespace}
    }
    // 각 Pod에 개별적으로 xDS 요청
}
```

### 통신 방식

```
istioctl
    │
    ├──> CentralControlPlane (외부 접근)
    │    └──> --xds-address istio.example.com:15012
    │         --cert-dir ~/.istio-certs
    │
    └──> Kubernetes 내부 접근
         ├──> GetIstioPods("app=istiod")    # Istiod Pod 목록
         ├──> Port-Forward → Pod:15012      # 각 Pod로 포트포워딩
         └──> xDS DiscoveryRequest 전송     # gRPC/xDS 프로토콜
```

### 보안 옵션

| 옵션 | 설명 |
|------|------|
| `--xds-address` | Istiod 직접 접근 주소 |
| `--xds-label` | 특정 라벨의 Istiod 선택 (예: `istio.io/rev=default`) |
| `--cert-dir` | RSA 인증서 디렉토리 |
| `--plaintext` | TLS 없이 접근 |

```bash
# 인클러스터 접근 (기본, 토큰 인증)
istioctl proxy-status

# 외부 접근 (토큰 인증)
istioctl ps --xds-address istio.cloudprovider.example.com:15012

# 외부 접근 (인증서 인증)
istioctl ps --xds-address istio.example.com:15012 --cert-dir ~/.istio-certs

# 특정 리비전의 컨트롤 플레인 선택
istioctl ps --xds-label istio.io/rev=canary
```

---

## 15. 정리

### istioctl 명령어 카테고리 요약

| 카테고리 | 명령어 | 목적 |
|----------|--------|------|
| **설치/관리** | install, uninstall, upgrade, manifest | 메시 생명주기 관리 |
| **진단** | proxy-status, proxy-config, analyze | 메시 상태 진단 |
| **인젝션** | kube-inject, tag, check-inject | 사이드카 인젝션 관리 |
| **검증** | validate, precheck | 설정 유효성 검사 |
| **관찰** | dashboard, metrics, version | 모니터링과 시각화 |
| **보안** | authz check | 보안 정책 검사 |
| **디버그** | describe, bug-report, internal-debug | 심층 진단 |
| **멀티클러스터** | remote-clusters, create-remote-secret | 멀티클러스터 관리 |
| **Ambient** | waypoint, ztunnel-config | Ambient 모드 관리 |

### 핵심 설계 패턴

**1. Cobra 명령어 팩토리 패턴**

모든 서브커맨드는 `func XxxCommand(ctx cli.Context) *cobra.Command` 형태의 팩토리 함수로 생성된다. CLI Context를 주입받아 Kubernetes 클라이언트와 네임스페이스 정보를 공유한다.

**2. 졸업(Graduation) 패턴**

실험적 기능은 `experimental` 하위에서 시작하여, 안정화되면 루트 레벨로 졸업한다:
- `seeExperimentalCmd()`: 아직 졸업하지 않은 명령에 대한 안내 메시지
- `StableXdsStatusCommand()`: 졸업한 명령에서 실험적 플래그를 숨김
- `PREFER-EXPERIMENTAL`: 실험적 명령을 루트에 배치하는 모드

**3. 이중 데이터 소스 패턴**

진단 명령들은 두 가지 소스를 지원한다:
- **라이브 클러스터**: Kubernetes API, Envoy admin API, xDS
- **파일 기반**: `--file` 플래그로 오프라인 분석

이 패턴은 프로덕션 환경에 직접 접근이 불가능할 때 config dump를 파일로 내보내 분석하는 워크플로우를 지원한다.

**4. 필터 + Writer 패턴**

proxy-config 명령들은 `extractConfigDump() -> Filter -> ConfigWriter.PrintXxx()` 파이프라인을 따른다. 각 서브커맨드는 자체 Filter 구조체(ClusterFilter, ListenerFilter, RouteFilter 등)를 정의하고, ConfigWriter가 필터를 적용하여 출력을 생성한다.

### 핵심 파일 요약

| 파일 | 역할 |
|------|------|
| `istioctl/cmd/istioctl/main.go` | 진입점, 초기화 흐름 |
| `istioctl/cmd/root.go` | Cobra 명령어 트리 구성, Viper 초기화 |
| `istioctl/pkg/cli/context.go` | CLI Context 인터페이스, 클라이언트 관리 |
| `istioctl/pkg/root/root.go` | 설정 파일 경로, 로깅 옵션 |
| `istioctl/pkg/proxystatus/proxystatus.go` | xDS 동기화 상태 조회 |
| `istioctl/pkg/proxyconfig/proxyconfig.go` | Envoy 설정 덤프 및 조회 |
| `istioctl/pkg/analyze/analyze.go` | 설정 분석 엔진 |
| `istioctl/pkg/kubeinject/kubeinject.go` | 수동 사이드카 인젝션 |
| `istioctl/pkg/dashboard/dashboard.go` | 대시보드 포트포워딩 |
| `istioctl/pkg/version/version.go` | 버전 조회 (일반/xDS) |
| `istioctl/pkg/authz/authz.go` | AuthorizationPolicy 검사 |
| `istioctl/pkg/metrics/metrics.go` | Prometheus 메트릭 조회 |
| `istioctl/pkg/precheck/precheck.go` | 설치 사전 검사 |
| `istioctl/pkg/tag/tag.go` | 리비전 태그 관리 |
| `istioctl/pkg/multixds/gather.go` | 다중 Istiod xDS 통신 |
| `pilot/pkg/xds/statusgen.go` | TypeDebugSyncronization/ConfigDump 정의 |
| `operator/cmd/mesh/install.go` | 설치 명령 구현 |
