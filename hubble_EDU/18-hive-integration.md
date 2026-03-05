# 18. Hive Cell 통합 (Hive Cell Integration)

## 1. 개요

Hubble은 Cilium 에이전트의 서브시스템으로서, Cilium의 **Hive** 의존성 주입(DI) 프레임워크를 통해
초기화되고 관리된다. Hive는 `cell.Module`, `cell.Provide`, `cell.Config`, `job.Group` 등의
프리미티브를 제공하여 컴포넌트 간의 의존성을 선언적으로 정의하고, 라이프사이클을 자동으로 관리한다.

이 문서에서는 Hubble이 Hive를 통해 어떻게 구성되는지, 각 Cell의 역할과 의존성 그래프,
설정(Config) 관리, Monitor Consumer의 브리지 패턴, 그리고 각 서브시스템 Cell의 내부 구현을
소스코드 기반으로 심층 분석한다.

### 핵심 소스 파일

| 파일 | 역할 |
|------|------|
| `pkg/hubble/cell/cell.go` | 최상위 Hubble Cell 모듈 정의 |
| `pkg/hubble/cell/config.go` | Hubble 핵심 설정 (EnableHubble, EventBuffer 등) |
| `pkg/hubble/cell/hubbleintegration.go` | hubbleIntegration 구현 (Launch, Status) |
| `pkg/hubble/monitor/consumer.go` | Monitor Consumer - Cilium 모니터와 Hubble 브리지 |
| `pkg/hubble/exporter/cell/cell.go` | Exporter Cell (정적/동적 익스포터) |
| `pkg/hubble/exporter/cell/builder.go` | FlowLogExporterBuilder 패턴 |
| `pkg/hubble/parser/cell/cell.go` | Parser Cell (페이로드 파서 + Getter) |
| `pkg/hubble/parser/cell/config.go` | Parser 설정 (리댁션, cgroup, 네트워크 정책) |
| `pkg/hubble/metrics/cell/cell.go` | Metrics Cell (정적/동적 메트릭스) |
| `pkg/hubble/dropeventemitter/cell.go` | Drop Event Emitter Cell |
| `pkg/hubble/peer/cell/cell.go` | Peer Service Cell |
| `pkg/hubble/observer/namespace/cell.go` | Namespace Manager Cell |


## 2. Hive DI 프레임워크 기초

### 2.1 핵심 개념

Hive는 Cilium 프로젝트에서 개발한 의존성 주입 프레임워크로, Go의 `uber/fx`에 영감을 받았으나
Cilium의 요구사항에 맞게 단순화되었다.

```
+------------------------------------------+
|              Hive 핵심 개념               |
+------------------------------------------+
|                                          |
|  cell.Module(name, desc, cells...)       |
|    - 논리적 그룹화 단위                    |
|    - 이름과 설명으로 식별                   |
|                                          |
|  cell.Provide(constructor)               |
|    - 의존성 등록 (타입 기반 자동 주입)       |
|    - 생성자 함수의 매개변수 → 의존성          |
|    - 반환 타입 → 제공하는 값                 |
|                                          |
|  cell.Config(defaults)                   |
|    - 설정 구조체 등록                       |
|    - pflag 바인딩 자동 생성                  |
|    - mapstructure 태그로 CLI 플래그 매핑      |
|                                          |
|  cell.Group(cells...)                    |
|    - 이름 없는 그룹 (Module과 달리 무명)      |
|    - 의존성 격리 없이 논리적 그룹화            |
|                                          |
|  job.Group                               |
|    - 비동기 작업 관리                        |
|    - OneShot: 한번 실행 후 종료              |
|    - Timer: 주기적 실행                      |
|                                          |
|  cell.Lifecycle                          |
|    - OnStart/OnStop 훅                    |
|    - 의존성 순서대로 시작, 역순으로 중지        |
|                                          |
+------------------------------------------+
```

### 2.2 의존성 주입 메커니즘

Hive의 DI는 Go의 구조체 태그와 인터페이스를 활용한다:

```go
// 의존성 수신 (cell.In 임베딩)
type myParams struct {
    cell.In                          // DI 컨테이너에서 자동 주입됨을 표시
    Logger    *slog.Logger           // 로거 자동 주입
    Config    myConfig               // 설정 자동 주입
    Options   []Option `group:"my-options"`  // 그룹 태그로 다중 수집
}

// 의존성 제공 (cell.Out 임베딩)
type myOut struct {
    cell.Out
    Service MyService                       // 타입으로 제공
    Builder *Builder `group:"builders,flatten"`  // 그룹으로 제공
}
```

**`group` 태그 패턴**: 여러 Cell에서 같은 이름의 그룹으로 값을 제공하면,
수신 측에서 슬라이스로 모아 받을 수 있다. `flatten` 옵션은 슬라이스의 슬라이스를
단일 슬라이스로 펼친다.


## 3. 최상위 Hubble Cell 구조

### 3.1 Cell 모듈 트리

```
pkg/hubble/cell/cell.go에서 정의된 최상위 구조:

Cell = cell.Module("hubble", "Exposes the Observer gRPC API and Hubble metrics",
    Core,                    // 핵심 (hubbleIntegration + config)
    ConfigProviders,         // 설정 제공자 그룹
    certloaderGroup,         // TLS 인증서 로더
    exportercell.Cell,       // 플로우 로그 익스포터
    metricscell.Cell,        // 메트릭스 서버
    dropeventemitter.Cell,   // 드롭 이벤트 이미터
    parsercell.Cell,         // 페이로드 파서
    namespace.Cell,          // 네임스페이스 모니터
    peercell.Cell,           // 피어 서비스
)
```

이를 트리로 시각화하면:

```
hubble (cell.Module)
├── Core (cell.Group)
│   ├── cell.Provide(newHubbleIntegration)    → HubbleIntegration
│   └── cell.Config(defaultConfig)            → config
│
├── ConfigProviders (cell.Group)
│   └── cell.ProvidePrivate(...)              → *peercell.HubbleConfig
│
├── certloaderGroup
│   └── TLS 인증서 로딩/핫 리로드
│
├── hubble-exporters (cell.Module)
│   ├── cell.Config(DefaultConfig)            → Config
│   ├── cell.Provide(NewValidatedConfig)      → ValidatedConfig
│   ├── cell.Provide(newHubbleStaticExporter) → []*FlowLogExporterBuilder
│   └── cell.Provide(newHubbleDynamicExporter)→ []*FlowLogExporterBuilder
│
├── hubble-metrics (cell.Module)
│   ├── cell.Config(defaultConfig)            → Config
│   ├── cell.Provide(grpc_prometheus.NewServerMetrics)
│   ├── cell.Provide(newMetricsServer)
│   └── cell.Provide(newFlowProcessor)        → metrics.FlowProcessor
│
├── hubble-dropeventemitter (cell.Module)
│   ├── cell.Config(defaultConfig)            → config
│   └── cell.Provide(newDropEventEmitter)     → FlowProcessor
│
├── payload-parser (cell.Module)
│   ├── cell.Config(defaultConfig)            → config
│   └── cell.Provide(newPayloadParser)        → parser.Decoder
│
├── namespace (cell.ProvidePrivate)
│   └── Manager + job.Timer("hubble-namespace-cleanup")
│
└── hubble-peer-service (cell.Module)
    └── cell.Provide(newPeerService)          → *peer.Service
```

### 3.2 의존성 그래프

```
                    ┌─────────────────────┐
                    │  HubbleIntegration   │
                    │  (Core cell)         │
                    └─────────┬───────────┘
                              │ 의존
          ┌───────────────────┼───────────────────┐
          │                   │                   │
    ┌─────▼─────┐     ┌──────▼──────┐    ┌──────▼──────┐
    │ parser     │     │ exporter    │    │ metrics     │
    │ .Decoder   │     │ Builders    │    │ FlowProc.   │
    └─────┬─────┘     └──────┬──────┘    └──────┬──────┘
          │                  │                   │
          │           ┌──────┴──────┐            │
          │           │ static +    │            │
          │           │ dynamic     │            │
          │           └─────────────┘            │
          │                                      │
    ┌─────▼──────────────────────────────────────▼─────┐
    │              Cilium 인프라 의존성                    │
    │  - IdentityAllocator  - EndpointManager           │
    │  - IPCache            - CGroupManager             │
    │  - NodeManager        - MonitorAgent              │
    │  - NodeLocalStore     - K8sWatcher                │
    └──────────────────────────────────────────────────┘
```

### 3.3 Core Cell

```go
// pkg/hubble/cell/cell.go
var Core = cell.Group(
    cell.Provide(newHubbleIntegration),
    cell.Config(defaultConfig),
)
```

`Core`는 `cell.Group`으로 정의되어 이름 없이 부모 모듈의 스코프에 포함된다.
두 가지를 등록한다:

1. **`cell.Config(defaultConfig)`**: `config` 구조체를 CLI 플래그로 등록
2. **`cell.Provide(newHubbleIntegration)`**: `HubbleIntegration` 인터페이스 구현체 생성

### 3.4 왜(Why) Module과 Group을 분리하는가?

`cell.Module`은 독립적인 이름 공간을 가지며, 로깅과 메트릭스에서 식별자로 사용된다.
`cell.Group`은 이름이 없으며, 단순히 여러 Cell을 묶는 용도이다.

Core를 `cell.Group`으로 만든 이유는 **hubbleIntegration의 설정(config)이 부모 hubble
모듈 수준에서 노출되어야 하기 때문**이다. 만약 별도의 `cell.Module`로 만들면,
`--enable-hubble` 같은 플래그가 하위 모듈 이름공간에 격리되어 다른 Cell에서 접근하기
어렵다.


## 4. 설정(Config) 관리

### 4.1 Hubble 핵심 설정

```go
// pkg/hubble/cell/config.go
type config struct {
    EnableHubble          bool          `mapstructure:"enable-hubble"`
    EventBufferCapacity   int           `mapstructure:"hubble-event-buffer-capacity"`
    EventQueueSize        int           `mapstructure:"hubble-event-queue-size"`
    MonitorEvents         []string      `mapstructure:"hubble-monitor-events"`
    LostEventSendInterval time.Duration `mapstructure:"hubble-lost-event-send-interval"`
    SocketPath            string        `mapstructure:"hubble-socket-path"`
    ListenAddress         string        `mapstructure:"hubble-listen-address"`
    PreferIpv6            bool          `mapstructure:"hubble-prefer-ipv6"`
}
```

| 플래그 | 기본값 | 설명 |
|--------|--------|------|
| `--enable-hubble` | `false` | Hubble 서버 활성화 여부 |
| `--hubble-event-buffer-capacity` | `4095` (MaxFlows 기본값) | 링 버퍼 크기 (2^n - 1) |
| `--hubble-event-queue-size` | `0` (동적 계산) | 모니터 이벤트 채널 버퍼 크기 |
| `--hubble-monitor-events` | `[]` (전체) | 관찰할 모니터 이벤트 타입 |
| `--hubble-lost-event-send-interval` | 설정 기반 | 유실 이벤트 전송 간격 |
| `--hubble-socket-path` | `/var/run/cilium/hubble.sock` | UNIX 도메인 소켓 경로 |
| `--hubble-listen-address` | `""` (비활성) | TCP 리슨 주소 (예: `:4244`) |
| `--hubble-prefer-ipv6` | `false` | IPv6 우선 사용 여부 |

### 4.2 동적 EventQueueSize 계산

```go
// pkg/hubble/cell/config.go
func (cfg *config) normalize() {
    if cfg.EventQueueSize == 0 {
        cfg.EventQueueSize = getDefaultMonitorQueueSize(runtime.NumCPU())
    }
}

func getDefaultMonitorQueueSize(numCPU int) int {
    monitorQueueSize := min(
        numCPU * ciliumDefaults.MonitorQueueSizePerCPU,
        ciliumDefaults.MonitorQueueSizePerCPUMaximum,
    )
    return monitorQueueSize
}
```

`EventQueueSize`가 0(기본값)이면 `normalize()` 함수가 런타임 CPU 수에 비례하여
자동 계산한다. 이는 **다양한 하드웨어 환경에서 최적의 성능을 보장**하기 위한 설계이다:

- `numCPU * MonitorQueueSizePerCPU`: CPU 코어 수에 비례
- `MonitorQueueSizePerCPUMaximum`: 메모리 과다 사용 방지를 위한 상한선
- `min()` 함수로 둘 중 작은 값 선택

### 4.3 ConfigProviders 패턴

```go
// pkg/hubble/cell/config.go
var ConfigProviders = cell.Group(
    cell.ProvidePrivate(func(cfg config, tlsCfg certloaderConfig) *peercell.HubbleConfig {
        return &peercell.HubbleConfig{
            ListenAddress:   cfg.ListenAddress,
            PreferIpv6:      cfg.PreferIpv6,
            EnableServerTLS: !tlsCfg.DisableServerTLS,
        }
    }),
)
```

**ConfigProviders**는 여러 설정 소스를 결합하여 하위 컴포넌트용 설정 객체를 생성한다.
여기서는 Hubble 핵심 설정(`config`)과 TLS 설정(`certloaderConfig`)을 결합하여
Peer Service가 필요로 하는 `HubbleConfig`를 만든다.

`cell.ProvidePrivate`를 사용하여 이 설정이 hubble 모듈 외부에서는 접근할 수 없도록 제한한다.

### 4.4 Parser 설정

```go
// pkg/hubble/parser/cell/config.go
type config struct {
    SkipUnknownCGroupIDs          bool     `mapstructure:"hubble-skip-unknown-cgroup-ids"`
    EnableNetworkPolicyCorrelation bool    `mapstructure:"hubble-network-policy-correlation-enabled"`
    EnableRedact                  bool     `mapstructure:"hubble-redact-enabled"`
    RedactHttpURLQuery            bool     `mapstructure:"hubble-redact-http-urlquery"`
    RedactHttpUserInfo            bool     `mapstructure:"hubble-redact-http-userinfo"`
    RedactHttpHeadersAllow        []string `mapstructure:"hubble-redact-http-headers-allow"`
    RedactHttpHeadersDeny         []string `mapstructure:"hubble-redact-http-headers-deny"`
}
```

Parser 설정은 보안과 관련된 중요한 옵션들을 포함한다:

| 카테고리 | 플래그 | 기본값 | 설명 |
|----------|--------|--------|------|
| CGroup | `skip-unknown-cgroup-ids` | `true` | 알 수 없는 cgroup ID 이벤트 건너뛰기 |
| 정책 | `network-policy-correlation-enabled` | `true` | 네트워크 정책 상관관계 활성화 |
| 리댁션 | `redact-enabled` | `false` | L7 민감정보 마스킹 활성화 |
| 리댁션 | `redact-http-urlquery` | `false` | URL 쿼리 마스킹 |
| 리댁션 | `redact-http-userinfo` | `true` | 사용자 정보 마스킹 |
| 리댁션 | `redact-http-headers-allow` | `[]` | 노출 허용 HTTP 헤더 |
| 리댁션 | `redact-http-headers-deny` | `[]` | 마스킹 대상 HTTP 헤더 |

**검증 규칙**: `allow`와 `deny`를 동시에 지정할 수 없다.

```go
func (cfg config) validate() error {
    if len(cfg.RedactHttpHeadersAllow) > 0 && len(cfg.RedactHttpHeadersDeny) > 0 {
        return fmt.Errorf(
            "Only one of --hubble-redact-http-headers-allow and " +
            "--hubble-redact-http-headers-deny can be specified, not both",
        )
    }
    return nil
}
```


## 5. hubbleIntegration 라이프사이클

### 5.1 HubbleIntegration 인터페이스

```go
// pkg/hubble/cell/cell.go
type HubbleIntegration interface {
    Launch(ctx context.Context) error
    Status(ctx context.Context) *models.HubbleStatus
}
```

이 인터페이스는 Cilium 데몬이 Hubble의 상태를 제어하고 모니터링하는 진입점이다.

### 5.2 생성 흐름 (newHubbleIntegration)

```go
// pkg/hubble/cell/cell.go
func newHubbleIntegration(params hubbleParams) (HubbleIntegration, error) {
    h, err := createHubbleIntegration(
        params.IdentityAllocator,
        params.EndpointManager,
        params.IPCache,
        params.CGroupManager,
        params.NodeManager,
        params.NodeLocalStore,
        params.MonitorAgent,
        params.TLSConfigPromise,
        params.ObserverOptions,
        params.ExporterBuilders,
        params.DropEventEmitter,
        params.PayloadParser,
        params.NamespaceManager,
        params.GRPCMetrics,
        params.MetricsFlowProcessor,
        params.PeerService,
        params.Config,
        params.Logger,
    )
    if err != nil {
        return nil, fmt.Errorf("failed to create hubble integration: %w", err)
    }

    params.JobGroup.Add(job.OneShot("hubble", func(ctx context.Context, _ cell.Health) error {
        return h.Launch(ctx)
    }))

    return h, nil
}
```

생성 시퀀스:

```
Hive 시작
  │
  ├─ 1. 의존성 해석: hubbleParams의 모든 필드 주입
  │     ├─ IdentityAllocator   (identity/cache/cell)
  │     ├─ EndpointManager     (endpointmanager)
  │     ├─ IPCache             (ipcache)
  │     ├─ CGroupManager       (cgroups/manager)
  │     ├─ NodeManager         (node/manager)
  │     ├─ NodeLocalStore      (node)
  │     ├─ MonitorAgent        (monitor/agent)
  │     ├─ TLSConfigPromise    (certloader)
  │     ├─ ObserverOptions     (group:"hubble-observer-options")
  │     ├─ ExporterBuilders    (group:"hubble-exporter-builders")
  │     ├─ DropEventEmitter    (dropeventemitter)
  │     ├─ PayloadParser       (parser/cell)
  │     ├─ NamespaceManager    (namespace)
  │     ├─ GRPCMetrics         (metrics/cell)
  │     ├─ MetricsFlowProcessor (metrics/cell)
  │     ├─ PeerService         (peer/cell)
  │     └─ Config              (cell.Config)
  │
  ├─ 2. createHubbleIntegration() 호출
  │     ├─ config.normalize()    → EventQueueSize 동적 계산
  │     ├─ ResolveExporters()    → 빌더 → 실제 익스포터 인스턴스
  │     └─ hubbleIntegration 구조체 생성
  │
  └─ 3. job.OneShot("hubble", h.Launch) 등록
        └─ Hive 시작 완료 후 비동기 실행
```

### 5.3 ExporterBuilder 해석 타이밍

```go
// pkg/hubble/cell/hubbleintegration.go (createHubbleIntegration 내)
// NOTE: exporter builders MUST always be resolved early and outside of a
// Hive job.Group or cell.Lifecycle hook. This is because their Build()
// function may have captured pointers to these and append new jobs/hooks,
// which we don't want to see happening after the hive startup.
exporters, err := exportercell.ResolveExporters(exporterBuilders)
```

이 주석은 매우 중요한 설계 제약을 설명한다:

- **ExporterBuilder.Build()** 함수는 `job.Group.Add()`나 `Lifecycle.Append()` 등을
  호출할 수 있다
- 이 호출은 **반드시 Hive 시작 전**에 이루어져야 한다
- Hive가 이미 시작된 후에 새로운 Job이나 Lifecycle Hook을 추가하면 **레이스 컨디션**이 발생한다
- 따라서 `createHubbleIntegration()`에서 **즉시** 빌더를 해석한다

### 5.4 Launch 흐름

```go
// pkg/hubble/cell/hubbleintegration.go
func (h *hubbleIntegration) Launch(ctx context.Context) error {
    if !h.config.EnableHubble {
        h.log.Info("Hubble server is disabled")
        return nil
    }

    observer, err := h.launch(ctx)
    if err != nil {
        h.log.Error("Failed to launch hubble", logfields.Error, err)
        errStr := err.Error()
        h.launchError.Store(&errStr)
        return err
    }

    h.observer.Store(observer)
    return nil
}
```

`Launch()`는 `job.OneShot`에 의해 Hive 시작 후 비동기로 실행된다.

```
Launch() 실행 흐름
│
├─ EnableHubble == false → 조기 반환
│
├─ launch(ctx) 호출
│   │
│   ├─ 1. MonitorFilter 설정 (선택적)
│   │     └─ h.config.MonitorEvents가 있으면 필터 생성
│   │
│   ├─ 2. DropEventEmitter 등록 (선택적)
│   │     └─ OnDecodedFlowFunc로 드롭 이벤트 처리
│   │
│   ├─ 3. LocalNodeWatcher 생성
│   │     └─ 로컬 노드 정보를 Flow에 주입
│   │
│   ├─ 4. Observer 옵션 설정
│   │     ├─ MaxFlows (EventBufferCapacity)
│   │     ├─ MonitorBuffer (EventQueueSize)
│   │     └─ LostEventSendInterval
│   │
│   ├─ 5. Exporter 등록
│   │     └─ OnDecodedEventFunc로 각 익스포터 연결
│   │
│   ├─ 6. Metrics FlowProcessor 등록 (선택적)
│   │     └─ OnDecodedFlowFunc로 메트릭스 처리
│   │
│   ├─ 7. 주입된 ObserverOptions 추가 (마지막)
│   │
│   ├─ 8. LocalObserverServer 생성 및 시작
│   │     ├─ observer.NewLocalServer(parser, nsManager, log, opts...)
│   │     ├─ go hubbleObserver.Start()
│   │     └─ monitorAgent.RegisterNewConsumer(monitor.NewConsumer(...))
│   │
│   ├─ 9. 로컬 UNIX 도메인 소켓 서버 시작
│   │     ├─ "unix:///var/run/cilium/hubble.sock"
│   │     ├─ InsecureServer (UDS는 TLS 불필요)
│   │     ├─ Health/Observer/Peer 서비스 등록
│   │     └─ gRPC 인터셉터 (버전, 메트릭스)
│   │
│   └─ 10. TCP 서버 시작 (ListenAddress가 있을 때)
│         ├─ TLS 설정 (tlsConfigPromise.Await)
│         ├─ Health/Observer/Peer 서비스 등록
│         └─ gRPC 인터셉터 (버전)
│
├─ 성공: h.observer.Store(observer)
│   └─ atomic.Pointer로 안전하게 저장
│
└─ 실패: h.launchError.Store(&errStr)
    └─ Status()에서 에러 보고용
```

### 5.5 Observer 옵션 등록 순서의 의미

`launch()` 함수에서 Observer 옵션이 등록되는 순서는 의도적이다:

```go
// 1. MonitorFilter (가장 먼저 - 이벤트 필터링)
// 2. DropEventEmitter (필터 후, 노드 정보 채우기 전)
// 3. LocalNodeWatcher (노드 정보 주입 - 메트릭스/익스포터 전에 실행되어야 함)
// 4. Exporters (플로우 익스포트)
// 5. MetricsFlowProcessor (메트릭스 수집)
// 6. Injected ObserverOptions (마지막 - 외부에서 주입된 옵션)
```

특히 코드 주석에서 이를 명시한다:

```go
// fill in the local node information after the dropEventEmitter logique,
// but before anything else (e.g. metrics).

// register injected observer options last to allow
// for explicit ordering of known dependencies
```

### 5.6 Status 보고

```go
// pkg/hubble/cell/hubbleintegration.go
func (h *hubbleIntegration) Status(ctx context.Context) *models.HubbleStatus {
    if !h.config.EnableHubble {
        return &models.HubbleStatus{State: models.HubbleStatusStateDisabled}
    }

    launchError := h.launchError.Load()
    if launchError != nil {
        return &models.HubbleStatus{
            State: models.HubbleStatusStateWarning,
            Msg:   *launchError,
        }
    }

    obs := h.observer.Load()
    if obs == nil {
        return &models.HubbleStatus{
            State: models.HubbleStatusStateWarning,
            Msg:   "Hubble starting",
        }
    }

    status, err := obs.ServerStatus(ctx, &observerpb.ServerStatusRequest{})
    if err != nil {
        return &models.HubbleStatus{State: models.HubbleStatusStateFailure, Msg: err.Error()}
    }

    return &models.HubbleStatus{
        State: models.StatusStateOk,
        Observer: &models.HubbleStatusObserver{
            CurrentFlows: int64(status.NumFlows),
            MaxFlows:     int64(status.MaxFlows),
            SeenFlows:    int64(status.SeenFlows),
            Uptime:       strfmt.Duration(time.Duration(status.UptimeNs)),
        },
    }
}
```

상태 결정 로직:

```
Status() 상태 판단 플로우차트
│
├── EnableHubble == false
│   └── State: "Disabled"
│
├── launchError != nil
│   └── State: "Warning" + 에러 메시지
│
├── observer == nil (Launch 아직 진행 중)
│   └── State: "Warning" + "Hubble starting"
│
├── obs.ServerStatus() 실패
│   └── State: "Failure" + 에러 메시지
│
└── 정상
    └── State: "Ok" + Observer 통계
        ├── CurrentFlows: 현재 버퍼 내 플로우 수
        ├── MaxFlows: 최대 용량
        ├── SeenFlows: 누적 관찰 플로우 수
        └── Uptime: 가동 시간
```

`atomic.Pointer`를 사용하는 이유: `Status()`는 Cilium 데몬의 헬스체크 프로브에 의해
별도 goroutine에서 호출될 수 있으므로, `Launch()`와의 데이터 레이스를 방지해야 한다.


## 6. Monitor Consumer: Cilium과 Hubble의 브리지

### 6.1 아키텍처 개요

Monitor Consumer는 Cilium의 모니터 에이전트(eBPF perf event ring buffer)에서
이벤트를 수신하여 Hubble Observer의 이벤트 채널로 전달하는 브리지 역할을 한다.

```
┌─────────────────────────────────────────────────────────┐
│                    Cilium Agent                         │
│                                                         │
│  ┌──────────────┐    ┌──────────────┐    ┌───────────┐  │
│  │ eBPF         │    │ Monitor      │    │ Monitor   │  │
│  │ Perf Ring    │───>│ Agent        │───>│ Consumer  │  │
│  │ Buffer       │    │              │    │ (Hubble)  │  │
│  └──────────────┘    └──────────────┘    └─────┬─────┘  │
│                                                │        │
│                                                ▼        │
│  ┌─────────────────────────────────────────────────────┐│
│  │              Hubble Observer                        ││
│  │              (events channel)                       ││
│  └─────────────────────────────────────────────────────┘│
└─────────────────────────────────────────────────────────┘
```

### 6.2 consumer 구조체

```go
// pkg/hubble/monitor/consumer.go
type consumer struct {
    uuider   *bufuuid.Generator    // UUID 생성기
    observer Observer               // Hubble Observer

    lostLock         lock.Mutex    // 유실 이벤트 보호 뮤텍스
    lostEventCounter *counter.IntervalRangeCounter  // 유실 이벤트 집계
    logLimiter       logging.Limiter                // 로그 빈도 제한

    metricLostPerfEvents     prometheus.Counter      // perf 유실 메트릭
    metricLostObserverEvents prometheus.Counter      // observer 유실 메트릭
}
```

### 6.3 이벤트 수신 인터페이스

consumer는 `MonitorConsumer` 인터페이스를 구현하여 세 종류의 이벤트를 수신한다:

```go
// NotifyAgentEvent: Cilium 에이전트 내부 이벤트 (정책 변경, 엔드포인트 업데이트 등)
func (c *consumer) NotifyAgentEvent(typ int, message any) {
    c.sendEvent(func() any {
        return &observerTypes.AgentEvent{
            Type:    typ,
            Message: message,
        }
    })
}

// NotifyPerfEvent: eBPF perf ring buffer에서 수신한 패킷 이벤트
func (c *consumer) NotifyPerfEvent(data []byte, cpu int) {
    c.sendEvent(func() any {
        return &observerTypes.PerfEvent{
            Data: data,
            CPU:  cpu,
        }
    })
}

// NotifyPerfEventLost: perf ring buffer 오버플로우로 유실된 이벤트 알림
func (c *consumer) NotifyPerfEventLost(numLostEvents uint64, cpu int) {
    c.sendEvent(func() any {
        return &observerTypes.LostEvent{
            Source:        observerTypes.LostEventSourcePerfRingBuffer,
            NumLostEvents: numLostEvents,
            CPU:           cpu,
        }
    })
    c.metricLostPerfEvents.Inc()
}
```

**지연 페이로드 생성 패턴**: 페이로드를 `func() any` 클로저로 전달하는 이유는
`sendEvent` 내부에서 `newEvent`를 호출할 때만 페이로드를 생성하기 위함이다.
채널이 가득 찬 경우 페이로드 생성을 건너뛰어 불필요한 할당을 방지한다.

### 6.4 sendEvent: 핵심 전송 로직

```go
// pkg/hubble/monitor/consumer.go
func (c *consumer) sendEvent(payloader func() any) {
    c.lostLock.Lock()
    defer c.lostLock.Unlock()

    now := time.Now()
    c.trySendLostEventLocked(now)     // 1. 먼저 밀린 유실 이벤트 전송 시도

    select {
    case c.observer.GetEventsChannel() <- c.newEvent(now, payloader):
        // 2. 채널에 이벤트 전송 성공
    default:
        c.incrementLostEventLocked(now)  // 3. 채널 가득 참 → 유실 카운트 증가
    }
}
```

이 함수의 동작을 시퀀스로 표현하면:

```
sendEvent() 호출
│
├─ 1. lostLock 획득
│
├─ 2. trySendLostEventLocked(now)
│   ├── lostEventCounter.IsElapsed(now)?
│   │   ├── No → 아무것도 안 함 (간격 미도달)
│   │   └── Yes
│   │       ├── count = lostEventCounter.Peek()
│   │       ├── lostEvent 생성 (LostEventSourceEventsQueue)
│   │       └── select:
│   │           ├── channel <- lostEvent → counter.Clear() (성공)
│   │           └── default → 아무것도 안 함 (채널 여전히 가득)
│   │
├─ 3. select:
│   ├── channel <- newEvent(now, payloader) → 성공 (정상 경로)
│   └── default → incrementLostEventLocked(now)
│       ├── count == 0 && logLimiter.Allow()?
│       │   └── Yes → 경고 로그 출력
│       ├── lostEventCounter.Increment(now)
│       └── metricLostObserverEvents.Inc()
│
└─ 4. lostLock 해제
```

### 6.5 유실 이벤트 배칭(Batching)

```go
// IntervalRangeCounter 사용
lostEventCounter: counter.NewIntervalRangeCounter(lostEventSendInterval),

// 로그 빈도 제한
logLimiter: logging.NewLimiter(30*time.Second, 1),
```

유실 이벤트를 매번 개별적으로 보내면 Observer 채널이 유실 이벤트로 가득 차서
정상 이벤트 처리를 방해할 수 있다. 이를 방지하기 위해:

1. **IntervalRangeCounter**: 일정 간격(`lostEventSendInterval`)마다 누적된 유실 수를
   한 번에 보고
2. **logLimiter**: 30초마다 최대 1건의 경고 로그만 출력하여 로그 폭주 방지

유실 이벤트의 구조:

```go
&observerTypes.LostEvent{
    Source:        observerTypes.LostEventSourceEventsQueue,  // Observer 큐 유실
    NumLostEvents: count.Count,   // 누적 유실 수
    First:         count.First,   // 첫 유실 시각
    Last:          count.Last,    // 마지막 유실 시각
}
```

### 6.6 UUID 생성

```go
func (c *consumer) newEvent(ts time.Time, payloader func() any) *observerTypes.MonitorEvent {
    ev := &observerTypes.MonitorEvent{
        Timestamp: ts,
        NodeName:  nodeTypes.GetAbsoluteNodeName(),
        Payload:   payloader(),
    }
    c.uuider.NewInto(&ev.UUID)
    return ev
}
```

`bufuuid.Generator`는 버퍼링된 UUID 생성기로, 매번 `uuid.New()`를 호출하는 것보다
효율적이다. 이벤트의 고유 식별을 위해 각 `MonitorEvent`에 UUID를 할당한다.

### 6.7 왜(Why) 이 Consumer 패턴인가?

**Cilium 모니터와 Hubble의 분리**: Monitor Agent는 Cilium의 핵심 컴포넌트로,
eBPF로부터 직접 이벤트를 수신한다. Hubble은 이 이벤트의 **소비자** 중 하나이다.
Consumer 패턴을 사용함으로써:

- Monitor Agent는 Hubble의 존재를 알 필요 없다 (느슨한 결합)
- 여러 소비자가 동시에 모니터 이벤트를 받을 수 있다
- 채널 기반 비동기 전달로 Monitor Agent의 성능에 영향을 주지 않는다
- 유실 이벤트 처리가 Consumer 레벨에서 독립적으로 관리된다


## 7. Exporter Cell 심층 분석

### 7.1 Cell 구조

```go
// pkg/hubble/exporter/cell/cell.go
var Cell = cell.Module(
    "hubble-exporters",
    "Exports hubble events to remote destination",

    cell.Provide(NewValidatedConfig),         // 검증된 설정
    cell.Provide(newHubbleStaticExporter),     // 정적 익스포터
    cell.Provide(newHubbleDynamicExporter),    // 동적 익스포터
    cell.Config(DefaultConfig),               // 설정 등록
)
```

### 7.2 FlowLogExporterBuilder 패턴

```go
// pkg/hubble/exporter/cell/builder.go
type FlowLogExporterBuilder struct {
    Name     string
    Replaces string
    Build    func() (exporter.FlowLogExporter, error)
}
```

Builder 패턴을 사용하는 이유:

1. **지연 생성**: 익스포터의 실제 인스턴스를 Hive 시작 시점까지 지연
2. **교체 가능성**: `Replaces` 필드로 기존 빌더를 새 빌더로 교체 가능
3. **부작용 관리**: `Build()` 내에서 Job/Lifecycle Hook 등록이 가능하도록

```
FlowLogExporterBuilder 해석 흐름
│
├─ replaceBuilders(builders)
│   ├── 이름별 맵 생성
│   ├── 중복 이름 검사 → 있으면 panic
│   └── Replaces 지정된 빌더 처리
│       ├── Replaces == "" → 무시
│       ├── Replaces == Name → 무시 (자기 자신)
│       └── Replaces != "" → 대상 빌더 삭제, 새 빌더로 교체
│
└─ buildExporters(builders)
    ├── 각 빌더의 Build() 호출
    ├── err != nil → 전파
    ├── exp == nil → 건너뛰기 (비활성 익스포터)
    └── 결과 슬라이스에 추가
```

### 7.3 정적 익스포터 생성

```go
// pkg/hubble/exporter/cell/cell.go
func newHubbleStaticExporter(params hubbleExportersParams) (hubbleExportersOut, error) {
    if params.Config.ExportFilePath == "" {
        params.Logger.Info("The Hubble static exporter is disabled")
        return hubbleExportersOut{}, nil
    }

    builder := &FlowLogExporterBuilder{
        Name: "static-exporter",
        Build: func() (exporter.FlowLogExporter, error) {
            // ... 필터, FieldMask, 파일 라이터 설정 ...
            staticExporter, err := exporter.NewExporter(params.Logger, exporterOpts...)
            if err != nil {
                params.Logger.Error("Failed to configure...", logfields.Error, err)
                return nil, nil  // non-fatal: nil 반환으로 무시
            }

            // Lifecycle Hook으로 종료 시 Stop 보장
            params.Lifecycle.Append(cell.Hook{
                OnStop: func(hc cell.HookContext) error {
                    return staticExporter.Stop()
                },
            })
            return staticExporter, nil
        },
    }
    return hubbleExportersOut{
        ExporterBuilders: []*FlowLogExporterBuilder{builder},
    }, nil
}
```

핵심 특징:
- **`ExportFilePath == ""`**: 비어있으면 정적 익스포터 비활성화 (빌더 자체를 반환하지 않음)
- **Non-fatal 에러 처리**: 익스포터 생성 실패 시 `nil, nil`을 반환하여 Hubble 전체 시작을 차단하지 않음
- **Lifecycle Hook**: `OnStop`에서 `staticExporter.Stop()`을 호출하여 파일 핸들 정리 보장
- **stdout vs file**: `ExportFilePath == "stdout"`이면 `StdoutNoOpWriter`, 아니면 `FileWriter`(lumberjack 기반)

### 7.4 동적 익스포터 생성

```go
func newHubbleDynamicExporter(params hubbleExportersParams) (hubbleExportersOut, error) {
    if params.Config.FlowlogsConfigFilePath == "" {
        return hubbleExportersOut{}, nil
    }

    builder := &FlowLogExporterBuilder{
        Name: "dynamic-exporter",
        Build: func() (exporter.FlowLogExporter, error) {
            dynamicExporter := exporter.NewDynamicExporter(...)

            // job.OneShot으로 Watch 시작
            params.JobGroup.Add(job.OneShot("hubble-dynamic-exporter",
                func(ctx context.Context, health cell.Health) error {
                    return dynamicExporter.Watch(ctx)
                },
            ))

            params.Lifecycle.Append(cell.Hook{
                OnStop: func(hc cell.HookContext) error {
                    return dynamicExporter.Stop()
                },
            })
            return dynamicExporter, nil
        },
    }
    return hubbleExportersOut{...}, nil
}
```

동적 익스포터의 차이점:
- **`job.OneShot`**: `Watch(ctx)` 메서드를 별도 goroutine에서 실행 (설정 파일 변경 감시)
- **Build() 내에서 JobGroup에 Job 추가**: 이것이 바로 ExporterBuilder를 Hive 시작 전에
  해석해야 하는 이유이다 (5.3절 참조)

### 7.5 Exporter Config 검증

```go
// pkg/hubble/exporter/cell/cell.go
type ValidatedConfig Config

func NewValidatedConfig(cfg Config) (ValidatedConfig, error) {
    if err := cfg.Validate(); err != nil {
        return ValidatedConfig{}, fmt.Errorf("hubble-exporter configuration error: %w", err)
    }
    return ValidatedConfig(cfg), nil
}

func (cfg Config) Validate() error {
    if fm := cfg.ExportFieldmask; len(fm) > 0 {
        _, err := fieldmaskpb.New(&flowpb.Flow{}, fm...)
        if err != nil {
            return fmt.Errorf("hubble-export-fieldmask contains invalid fieldmask '%v': %w", fm, err)
        }
    }
    // ... ExportFieldAggregate도 동일하게 검증
    return nil
}
```

**ValidatedConfig 패턴**: `Config`를 `ValidatedConfig` 타입으로 래핑하여, 검증이
완료된 설정만 하위 컴포넌트에 전달될 수 있도록 타입 시스템으로 보장한다. Hive DI에서
`Config`와 `ValidatedConfig`는 서로 다른 타입이므로, `ValidatedConfig`를 요구하는
생성자는 반드시 검증된 설정만 받는다.


## 8. Parser Cell 심층 분석

### 8.1 Cell 구조

```go
// pkg/hubble/parser/cell/cell.go
var Cell = cell.Module(
    "payload-parser",
    "Provides a payload parser for Hubble",

    cell.Provide(newPayloadParser),
    cell.Config(defaultConfig),
)
```

### 8.2 payloadGetters: 다중 인터페이스 구현

```go
// pkg/hubble/parser/cell/cell.go
type payloadGetters struct {
    log               *slog.Logger
    identityAllocator identitycell.CachingIdentityAllocator
    endpointManager   endpointmanager.EndpointManager
    ipcache           *ipcache.IPCache
    db                *statedb.DB
    frontends         statedb.Table[*loadbalancer.Frontend]
}
```

`payloadGetters`는 하나의 구조체로 4개의 Getter 인터페이스를 구현한다:

```
payloadGetters 인터페이스 구현
│
├── IdentityGetter
│   └── GetIdentity(securityIdentity uint32) → *identity.Identity
│       └── identityAllocator.LookupIdentityByID()
│
├── EndpointGetter
│   ├── GetEndpointInfo(ip netip.Addr) → EndpointInfo
│   │   └── endpointManager.LookupIP(ip)
│   └── GetEndpointInfoByID(id uint16) → EndpointInfo
│       └── endpointManager.LookupCiliumID(id)
│
├── DNSGetter
│   └── GetNamesOf(sourceEpID uint32, ip netip.Addr) → []string
│       ├── endpointManager.LookupCiliumID(sourceEpID)
│       ├── ep.DNSHistory.LookupIP(ip)
│       └── strings.TrimSuffix(name, ".") // trailing dot 제거
│
└── ServiceGetter
    └── GetServiceByAddr(ip netip.Addr, port uint16) → *flowpb.Service
        ├── loadbalancer.LookupFrontendByTuple(TCP)
        ├── loadbalancer.LookupFrontendByTuple(UDP) // TCP 실패 시
        └── {Namespace, Name} 반환
```

### 8.3 GetIdentity 상세

```go
func (p *payloadGetters) GetIdentity(securityIdentity uint32) (*identity.Identity, error) {
    ident := p.identityAllocator.LookupIdentityByID(
        context.Background(),
        identity.NumericIdentity(securityIdentity),
    )
    if ident == nil {
        return nil, fmt.Errorf("identity %d not found", securityIdentity)
    }
    return ident, nil
}
```

Cilium의 보안 ID(Security Identity)를 Hubble Flow의 소스/대상 라벨로 변환한다.
Security Identity는 eBPF 데이터패스에서 패킷에 태그된 숫자 ID이며, 이를
`k8s:app=frontend` 같은 라벨 셋으로 매핑한다.

### 8.4 GetNamesOf 상세: DNS 역변환

```go
func (h *payloadGetters) GetNamesOf(sourceEpID uint32, ip netip.Addr) []string {
    ep := h.endpointManager.LookupCiliumID(uint16(sourceEpID))
    if ep == nil {
        return nil
    }
    if !ip.IsValid() {
        return nil
    }
    names := ep.DNSHistory.LookupIP(ip)
    for i := range names {
        names[i] = strings.TrimSuffix(names[i], ".")
    }
    return names
}
```

**소스 엔드포인트의 DNS 히스토리를 사용하는 이유**: Cilium은 각 엔드포인트별로
DNS 프록시를 통해 관찰한 DNS 응답을 기록한다. IP 주소에서 도메인 이름으로의
역변환은 **해당 엔드포인트가 실제로 해석한 DNS 응답**을 기반으로 해야 정확하다.
다른 엔드포인트의 DNS 히스토리를 참조하면 잘못된 도메인 이름이 표시될 수 있다.

### 8.5 GetServiceByAddr 상세

```go
func (h *payloadGetters) GetServiceByAddr(ip netip.Addr, port uint16) *flowpb.Service {
    if !ip.IsValid() {
        return nil
    }
    addrCluster := cmtypes.AddrClusterFrom(ip, 0)
    txn := h.db.ReadTxn()
    fe, found := loadbalancer.LookupFrontendByTuple(
        txn, h.frontends, addrCluster, loadbalancer.TCP, port, loadbalancer.ScopeExternal,
    )
    if !found {
        fe, found = loadbalancer.LookupFrontendByTuple(
            txn, h.frontends, addrCluster, loadbalancer.UDP, port, loadbalancer.ScopeExternal,
        )
    }
    if !found {
        return nil
    }
    return &flowpb.Service{
        Namespace: fe.ServiceName.Namespace(),
        Name:      fe.ServiceName.Name(),
    }
}
```

서비스 조회 전략:
1. **TCP 먼저**: 대부분의 서비스가 TCP이므로 먼저 시도
2. **UDP 폴백**: TCP에서 못 찾으면 UDP로 재시도
3. **StateDB 사용**: `statedb.ReadTxn()`으로 트랜잭션 읽기 (lock-free snapshot)
4. **ClusterMesh 지원**: `AddrClusterFrom(ip, 0)`으로 멀티클러스터 주소 처리

### 8.6 Parser 생성자 옵션 조립

```go
func newPayloadParser(params payloadParserParams) (parser.Decoder, error) {
    if err := params.Config.validate(); err != nil {
        return nil, fmt.Errorf("failed to validate configuration: %w", err)
    }
    g := &payloadGetters{...}

    var parserOpts []parserOptions.Option
    if params.Config.EnableRedact {
        parserOpts = append(parserOpts,
            parserOptions.WithRedact(
                params.Config.RedactHttpURLQuery,
                params.Config.RedactHttpUserInfo,
                params.Config.RedactHttpHeadersAllow,
                params.Config.RedactHttpHeadersDeny,
            ),
        )
    }
    parserOpts = append(parserOpts,
        parserOptions.WithNetworkPolicyCorrelation(
            params.Config.EnableNetworkPolicyCorrelation,
        ),
    )
    parserOpts = append(parserOpts,
        parserOptions.WithSkipUnknownCGroupIDs(
            params.Config.SkipUnknownCGroupIDs,
        ),
    )
    // 외부에서 주입된 옵션 (group:"hubble-parser-options")
    parserOpts = append(parserOpts, params.ParserOptions...)

    return parser.New(params.Log, g, g, g, params.Ipcache, g,
        params.LinkCache, params.CGroupManager, parserOpts...)
}
```

**`g`가 네 번 전달되는 이유**: `payloadGetters`가 DNSGetter, EndpointGetter,
IdentityGetter, ServiceGetter 네 개의 인터페이스를 모두 구현하기 때문이다.
`parser.New()`의 시그니처에서 각각 다른 인터페이스 타입으로 받으므로, 같은 인스턴스를
여러 번 전달한다.


## 9. Metrics Cell 분석

### 9.1 Cell 구조

```go
// pkg/hubble/metrics/cell/cell.go
var Cell = cell.Module(
    "hubble-metrics",
    "Provides metrics for Hubble",

    cell.Provide(func() *grpc_prometheus.ServerMetrics {
        return grpc_prometheus.NewServerMetrics()
    }),
    certloaderGroup,
    cell.ProvidePrivate(newValidatedConfig),
    cell.Provide(newMetricsServer),
    cell.Provide(newFlowProcessor),
    cell.Config(defaultConfig),
)
```

### 9.2 정적 vs 동적 메트릭스

```go
func newFlowProcessor(p params) (metrics.FlowProcessor, error) {
    if p.Config.MetricsServer == "" {
        return nil, nil  // 메트릭스 서버 비활성화
    }

    var fp metrics.FlowProcessor
    switch {
    case len(p.Config.Metrics) > 0:
        // 정적 메트릭스: CLI 플래그로 지정
        metricConfigs := api.ParseStaticMetricsConfig(strings.Fields(p.Config.Metrics))
        err := metrics.InitMetrics(p.Logger, metrics.Registry, metricConfigs, p.GRPCServerMetrics)
        fp = metrics.NewStaticFlowProcessor(p.Logger, metrics.EnabledMetrics)

    case p.Config.DynamicMetricConfigFilePath != "":
        // 동적 메트릭스: 설정 파일로 관리 (런타임 변경 가능)
        metrics.InitHubbleInternalMetrics(metrics.Registry, p.GRPCServerMetrics)
        fp = metrics.NewDynamicFlowProcessor(
            metrics.Registry, p.Logger, p.Config.DynamicMetricConfigFilePath,
        )
    }

    return fp, nil
}
```

검증 규칙: 정적(`--hubble-metrics`)과 동적(`--hubble-dynamic-metrics-config-path`)은
동시에 사용할 수 없다.

```go
func (cfg Config) Validate() error {
    if cfg.DynamicMetricConfigFilePath != "" && len(cfg.Metrics) > 0 {
        return errors.New("cannot configure both static and dynamic Hubble metrics")
    }
    return nil
}
```


## 10. Drop Event Emitter Cell 분석

### 10.1 Cell 구조

```go
// pkg/hubble/dropeventemitter/cell.go
var Cell = cell.Module(
    "hubble-dropeventemitter",
    "Emits k8s events on packet drop",

    cell.Provide(newDropEventEmitter),
    cell.Config(defaultConfig),
)
```

### 10.2 FlowProcessor 인터페이스

```go
type FlowProcessor interface {
    ProcessFlow(ctx context.Context, flow *flowpb.Flow) error
}
```

Drop Event Emitter는 패킷 드롭 시 Kubernetes Events를 생성한다.
이는 `kubectl get events`로 관찰할 수 있어 운영자에게 즉각적인 가시성을 제공한다.

### 10.3 설정

| 플래그 | 기본값 | 설명 |
|--------|--------|------|
| `--hubble-drop-events` | `false` | K8s 이벤트 생성 활성화 |
| `--hubble-drop-events-interval` | `2m` | 동일 소스/대상 IP의 최소 이벤트 간격 |
| `--hubble-drop-events-reasons` | `auth_required, policy_denied` | 이벤트 생성 대상 드롭 이유 |
| `--hubble-drop-events-extended` | `false` | L4 네트워크 정책 정보 포함 |
| `--hubble-drop-events-rate-limit` | `1` | 초당 최대 이벤트 수 (0=무제한) |

### 10.4 normalize()의 하위 호환성

```go
func (cfg *config) normalize() {
    // Before: flags.String()으로 등록, viper.GetStringSlice()로 파싱 → 공백 구분
    // After: flags.StringSlice()로 등록 → 쉼표 구분
    // 하위 호환: 단일 문자열이면 공백으로 분리
    if len(cfg.K8sDropEventsReasons) == 1 {
        cfg.K8sDropEventsReasons = strings.Fields(cfg.K8sDropEventsReasons[0])
    }
}
```

이 `normalize()` 함수는 플래그 등록 방식이 변경되면서 발생한 하위 호환성 문제를
해결한다. 이전에는 `"auth_required policy_denied"`처럼 공백으로 구분된 단일
문자열이었으나, 현재는 `flags.StringSlice()`로 변경되어 쉼표 구분을 사용한다.


## 11. Peer Service Cell 분석

### 11.1 Cell 구조

```go
// pkg/hubble/peer/cell/cell.go
var Cell = cell.Module(
    "hubble-peer-service",
    "Hubble peer service for handling peer discovery and notifications",

    cell.Provide(newPeerService),
)
```

### 11.2 HubbleConfig 의존성

```go
type HubbleConfig struct {
    ListenAddress   string   // TCP 리슨 주소
    PreferIpv6      bool     // IPv6 우선 여부
    EnableServerTLS bool     // TLS 활성화 여부
}
```

이 설정은 `ConfigProviders`에서 생성되어 Peer Service에 주입된다 (4.3절 참조).

### 11.3 Peer Service 생성

```go
func newPeerService(params peerServiceParams) (*peer.Service, error) {
    var peerServiceOptions []serviceoption.Option

    if !params.Config.EnableServerTLS {
        peerServiceOptions = append(peerServiceOptions,
            serviceoption.WithoutTLSInfo(),
        )
    }

    if params.Config.PreferIpv6 {
        peerServiceOptions = append(peerServiceOptions,
            serviceoption.WithAddressFamilyPreference(serviceoption.AddressPreferIPv6),
        )
    }

    if addr := params.Config.ListenAddress; addr != "" {
        port, err := getPort(addr)
        if err != nil {
            params.Health.Degraded(...)  // Cell Health 상태를 Degraded로
        } else {
            peerServiceOptions = append(peerServiceOptions,
                serviceoption.WithHubblePort(port),
            )
        }
    }

    service := peer.NewService(params.NodeManager, peerServiceOptions...)

    params.Lifecycle.Append(cell.Hook{
        OnStop: func(cell.HookContext) error {
            return service.Close()
        },
    })

    return service, nil
}
```

**Cell Health 활용**: 포트 파싱 실패 시 `params.Health.Degraded()`를 호출하여
Hive의 헬스 체크 시스템에 degraded 상태를 보고한다. 이는 서비스를 중단시키지 않으면서
운영자에게 문제를 알린다.


## 12. Namespace Manager Cell 분석

### 12.1 Cell 정의

```go
// pkg/hubble/observer/namespace/cell.go
var Cell = cell.ProvidePrivate(func(jobGroup job.Group) Manager {
    m := NewManager()
    jobGroup.Add(job.Timer(
        "hubble-namespace-cleanup",
        func(_ context.Context) error {
            m.cleanupNamespaces()
            return nil
        },
        cleanupInterval,
    ))
    return m
})
```

이 Cell은 `cell.Module`이 아닌 `cell.ProvidePrivate`를 직접 사용한다.
`ProvidePrivate`는 hubble 모듈 내부에서만 접근 가능하도록 제한한다.

**`job.Timer` 패턴**: `cleanupInterval` 주기로 `cleanupNamespaces()`를 실행하여
더 이상 관찰되지 않는 네임스페이스를 정리한다. `job.Timer`는 Hive가 관리하는
주기적 작업으로, 컨텍스트 취소 시 자동으로 중지된다.


## 13. gRPC 인터셉터

### 13.1 서버 버전 주입

```go
// pkg/hubble/cell/hubbleintegration.go
var serverVersionHeader = metadata.Pairs(
    defaults.GRPCMetadataServerVersionKey,
    build.ServerVersion.SemVer(),
)

func serverVersionUnaryInterceptor() grpc.UnaryServerInterceptor {
    return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo,
        handler grpc.UnaryHandler) (any, error) {
        resp, err := handler(ctx, req)
        grpc.SetHeader(ctx, serverVersionHeader)
        return resp, err
    }
}

func serverVersionStreamInterceptor() grpc.StreamServerInterceptor {
    return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo,
        handler grpc.StreamHandler) error {
        ss.SetHeader(serverVersionHeader)
        return handler(srv, ss)
    }
}
```

모든 gRPC 응답에 서버 버전 메타데이터를 주입한다. Hubble CLI는 이 메타데이터를
사용하여 서버 버전 호환성을 확인할 수 있다.

**Unary vs Stream 차이**:
- Unary: `grpc.SetHeader(ctx, header)` - 핸들러 호출 **후** 설정
- Stream: `ss.SetHeader(header)` - 핸들러 호출 **전** 설정 (스트림 헤더는 첫 응답 전에 전송)


## 14. 서버 구성: UDS + TCP 이중 서버

### 14.1 UDS (UNIX Domain Socket) 서버

```go
// pkg/hubble/cell/hubbleintegration.go (launch 메서드 내)
sockPath := "unix://" + h.config.SocketPath
localSrvOpts = append(localSrvOpts,
    serveroption.WithUnixSocketListener(h.log, sockPath),
    serveroption.WithHealthService(),
    serveroption.WithObserverService(hubbleObserver),
    serveroption.WithPeerService(h.peerService),
    serveroption.WithInsecure(),                          // TLS 불필요
    serveroption.WithGRPCUnaryInterceptor(serverVersionUnaryInterceptor()),
    serveroption.WithGRPCStreamInterceptor(serverVersionStreamInterceptor()),
    serveroption.WithGRPCMetrics(h.grpcMetrics),
    serveroption.WithGRPCStreamInterceptor(h.grpcMetrics.StreamServerInterceptor()),
    serveroption.WithGRPCUnaryInterceptor(h.grpcMetrics.UnaryServerInterceptor()),
)
```

UDS 서버의 특징:
- **항상 생성**: `EnableHubble`이 true이면 반드시 시작
- **Insecure**: UNIX 소켓은 파일시스템 권한으로 보호되므로 TLS 불필요
- **로컬 전용**: Cilium Pod 내부에서 `hubble` CLI로 직접 접속 시 사용
- **gRPC 메트릭스 포함**: Prometheus 메트릭스 수집

### 14.2 TCP 서버 (선택적)

```go
address := h.config.ListenAddress
if address != "" {
    if !tlsEnabled {
        h.log.Warn("Hubble server will be exposing its API insecurely...")
    }
    options := []serveroption.Option{
        serveroption.WithTCPListener(address),
        serveroption.WithHealthService(),
        serveroption.WithPeerService(h.peerService),
        serveroption.WithObserverService(hubbleObserver),
        // ...
    }

    if !tlsEnabled {
        options = append(options, serveroption.WithInsecure())
    } else {
        tlsConfig, err := h.tlsConfigPromise.Await(ctx)
        options = append(options, serveroption.WithServerTLS(tlsConfig))
    }

    srv, err := server.NewServer(h.log, options...)
    // ...
}
```

TCP 서버의 특징:
- **선택적 생성**: `ListenAddress`가 비어있으면 생성하지 않음
- **TLS 지원**: `tlsConfigPromise`를 통해 비동기적으로 TLS 인증서 대기
- **Relay 연결용**: Hubble Relay가 이 TCP 서버에 연결하여 Flow 수집
- **보안 경고**: TLS 없이 TCP를 노출하면 경고 로그 출력

### 14.3 TLS Config Promise 패턴

```go
tlsConfig, err := h.tlsConfigPromise.Await(ctx)
```

`tlsConfigPromise`는 Hive의 Promise 패턴으로, TLS 인증서가 로드될 때까지
비동기적으로 대기한다. certloader Cell이 인증서를 성공적으로 로드하면 Promise가
해석(resolve)되어 `Await()`가 반환된다.

이 패턴이 필요한 이유:
- 인증서 로딩은 파일시스템 접근이나 외부 시스템(Vault 등)과의 통신이 필요할 수 있다
- Hubble 서버 시작이 인증서 로딩에 의해 블로킹되지 않아야 한다
- 인증서가 준비되면 자동으로 서버가 TLS로 전환된다


## 15. 그레이스풀 셧다운

### 15.1 셧다운 시퀀스

```go
// launch() 내에서 등록된 셧다운 핸들러들:

// UDS 서버 셧다운
go func() {
    <-ctx.Done()
    localSrv.Stop()
}()

// TCP 서버 셧다운
go func() {
    <-ctx.Done()
    srv.Stop()
}()
```

각 서버는 컨텍스트 취소를 감지하는 별도 goroutine을 가진다.
Hive가 중지되면:

```
Hive 중지 시그널
│
├─ ctx.Done() 닫힘
│   ├── localSrv.Stop()      → UDS gRPC 서버 중지
│   └── srv.Stop()           → TCP gRPC 서버 중지
│
├─ Lifecycle OnStop 훅 (역순 실행)
│   ├── dynamicExporter.Stop()  → 동적 익스포터 중지
│   ├── staticExporter.Stop()   → 정적 익스포터 중지
│   ├── peerService.Close()     → 피어 서비스 중지
│   └── dropEventEmitter.Shutdown() → 드롭 이벤트 이미터 중지
│
└─ Job 중지
    ├── "hubble" OneShot 취소
    ├── "hubble-dynamic-exporter" OneShot 취소
    └── "hubble-namespace-cleanup" Timer 취소
```


## 16. 전체 의존성 해석 순서

Hive는 의존성 그래프를 토폴로지 정렬하여 초기화 순서를 결정한다.
Hubble의 경우 다음과 같은 순서로 해석된다:

```
1. cell.Config 등록
   ├── hubble/cell/config             → config (EnableHubble, EventBuffer 등)
   ├── exporter/cell/config           → Config (ExportFilePath 등)
   ├── parser/cell/config             → config (SkipUnknownCGroupIDs 등)
   ├── metrics/cell/config            → Config (MetricsServer 등)
   └── dropeventemitter/config        → config (EnableK8sDropEvents 등)

2. 독립 의존성 해석
   ├── grpc_prometheus.NewServerMetrics() → *ServerMetrics
   └── 외부 의존성 (IPCache, EndpointManager 등)

3. 하위 컴포넌트 생성
   ├── NewValidatedConfig()           → ValidatedConfig
   ├── newPayloadParser()             → parser.Decoder
   ├── newDropEventEmitter()          → FlowProcessor
   ├── NewManager() + Timer           → namespace.Manager
   ├── newFlowProcessor()             → metrics.FlowProcessor
   ├── newPeerService()               → *peer.Service
   ├── newHubbleStaticExporter()      → []*FlowLogExporterBuilder
   └── newHubbleDynamicExporter()     → []*FlowLogExporterBuilder

4. ConfigProviders
   └── config + certloaderConfig      → *peercell.HubbleConfig

5. hubbleIntegration 생성
   ├── 모든 의존성 주입
   ├── ExporterBuilder 즉시 해석
   └── job.OneShot("hubble") 등록

6. Hive Start
   └── Launch() 비동기 실행
       ├── Observer 생성 및 시작
       ├── Monitor Consumer 등록
       ├── UDS 서버 시작
       └── TCP 서버 시작 (선택적)
```


## 17. 왜(Why) Hive를 사용하는가?

### 17.1 전통적 초기화 방식의 문제

Hive 도입 전, Cilium/Hubble의 초기화는 다음과 같은 문제가 있었다:

1. **순서 의존성 관리**: 컴포넌트 A가 B를 필요로 하고, B가 C를 필요로 할 때,
   main() 함수에서 정확한 초기화 순서를 수동으로 관리해야 했다
2. **순환 의존성 감지**: 런타임에야 발견되는 순환 의존성
3. **테스트 어려움**: 특정 컴포넌트만 격리하여 테스트하기 어려움
4. **설정 분산**: 각 컴포넌트의 설정이 여기저기 흩어져 있음

### 17.2 Hive가 해결하는 문제

```
전통적 방식                          Hive 방식
───────────────                    ──────────
func main() {                     var Cell = cell.Module(
  cfg := loadConfig()               "hubble",
  cache := NewCache(cfg)             cell.Config(defaultConfig),
  parser := NewParser(cfg, cache)    cell.Provide(newParser),
  observer := NewObserver(           cell.Provide(newObserver),
    cfg, parser, cache)            )
  server := NewServer(cfg, observer)
  server.Start()                   // 순서, 의존성은 Hive가 관리
}
```

1. **선언적 의존성**: 생성자 함수의 매개변수로 의존성을 선언, Hive가 자동 주입
2. **컴파일 타임 안전성**: 타입 불일치 시 Hive 시작 시 즉시 에러
3. **자동 순서 관리**: 토폴로지 정렬로 올바른 초기화 순서 보장
4. **설정 통합**: `cell.Config`로 플래그 등록과 검증을 한 곳에서 관리
5. **라이프사이클 관리**: OnStart/OnStop 훅으로 리소스 정리 보장

### 17.3 group 태그의 활용

```go
// 여러 Cell에서 옵션을 제공
ObserverOptions  []observeroption.Option `group:"hubble-observer-options"`

// 여러 Cell에서 빌더를 제공
ExporterBuilders []*FlowLogExporterBuilder `group:"hubble-exporter-builders,flatten"`
```

`group` 태그는 **확장 포인트(Extension Point)** 역할을 한다:
- 새로운 Observer 옵션이나 Exporter를 추가할 때 기존 코드를 수정할 필요 없이
  새 Cell에서 같은 그룹 이름으로 제공하면 자동으로 수집된다
- 이는 **Open/Closed Principle**을 구조적으로 보장한다

### 17.4 ValidatedConfig 패턴의 의미

```go
type ValidatedConfig Config

func NewValidatedConfig(cfg Config) (ValidatedConfig, error) { ... }
```

이 패턴은 Go의 타입 시스템을 활용한 **컴파일 타임 안전 장치**이다:
- 검증되지 않은 `Config`를 직접 사용하는 것을 방지
- `ValidatedConfig`를 요구하는 생성자는 반드시 검증된 설정만 받음
- Hive의 DI가 `Config` → `ValidatedConfig` → 소비자 순서로 해석을 보장


## 18. 정리

### 18.1 Cell 요약 테이블

| Cell | 타입 | 제공 타입 | 설정 |
|------|------|-----------|------|
| hubble | Module | - (최상위) | - |
| Core | Group | HubbleIntegration | config |
| ConfigProviders | Group | *HubbleConfig | - |
| hubble-exporters | Module | []*FlowLogExporterBuilder | Config |
| hubble-metrics | Module | FlowProcessor, *ServerMetrics | Config |
| hubble-dropeventemitter | Module | FlowProcessor | config |
| payload-parser | Module | parser.Decoder | config |
| namespace | ProvidePrivate | Manager | - |
| hubble-peer-service | Module | *peer.Service | - |

### 18.2 핵심 설계 원칙

1. **Cell 당 하나의 책임**: 각 Cell은 하나의 서브시스템만 담당
2. **인터페이스 기반 의존성**: 구체 타입이 아닌 인터페이스로 의존성 선언
3. **지연 초기화**: Builder 패턴으로 실제 인스턴스 생성을 Hive 시작까지 지연
4. **Graceful Degradation**: 비활성 컴포넌트는 nil 반환, 에러는 non-fatal 처리
5. **설정 검증 분리**: ValidatedConfig으로 검증과 사용을 타입으로 분리
6. **확장 가능한 파이프라인**: group 태그로 Observer 옵션과 Exporter를 동적 확장

### 18.3 Hive 없이 같은 구조를 만들려면?

Hive 없이 이 구조를 구현하려면 다음이 필요하다:

- 약 200줄의 초기화 코드 (`main()` 또는 `NewDaemon()`)
- 수동 의존성 순서 관리 (토폴로지 정렬 직접 구현)
- 각 컴포넌트별 `Close()` 호출을 `defer`로 관리
- 설정 검증을 초기화 함수에 산발적으로 배치
- 새 컴포넌트 추가 시 초기화 코드 수정 필수

Hive를 사용함으로써 이 모든 것이 **선언적**이고 **타입 안전**하며 **확장 가능**한
방식으로 관리된다. 이는 Cilium처럼 수십 개의 서브시스템을 가진 대규모 프로젝트에서
특히 중요한 설계 결정이다.
