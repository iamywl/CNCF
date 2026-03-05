# 14. Grafana 플러그인 시스템 심화

## 목차

1. [플러그인 아키텍처 개요](#1-플러그인-아키텍처-개요)
2. [Plugin 구조체와 핵심 인터페이스](#2-plugin-구조체와-핵심-인터페이스)
3. [gRPC 프로토콜](#3-grpc-프로토콜)
4. [플러그인 라이프사이클](#4-플러그인-라이프사이클)
5. [로드 파이프라인](#5-로드-파이프라인)
6. [쿼리 디스패치](#6-쿼리-디스패치)
7. [미들웨어 스택](#7-미들웨어-스택)
8. [프론트엔드 플러그인](#8-프론트엔드-플러그인)
9. [플러그인 설치 및 관리](#9-플러그인-설치-및-관리)
10. [보안: 서명 검증](#10-보안-서명-검증)

---

## 1. 플러그인 아키텍처 개요

Grafana 플러그인 시스템은 Grafana의 핵심 확장 메커니즘이다.
데이터소스, 패널, 앱 세 가지 유형의 플러그인을 지원하며, 백엔드(Go) 플러그인은 별도 프로세스로
실행되어 gRPC를 통해 Grafana 서버와 통신한다.

### 전체 아키텍처

```
┌─────────────────────────────────────────────────────────────┐
│                     Grafana Server (Go)                      │
│                                                              │
│  ┌──────────────────────────────────────────────────────┐    │
│  │              Plugin Manager                           │    │
│  │  ┌─────────┐  ┌───────────┐  ┌──────────────────┐    │    │
│  │  │Discovery │→ │Bootstrap  │→ │Initialization    │    │    │
│  │  │(scan)    │  │(parse)    │  │(register+start)  │    │    │
│  │  └─────────┘  └───────────┘  └──────────────────┘    │    │
│  │  ┌──────────┐  ┌────────────┐                         │    │
│  │  │Validation│  │Termination │                         │    │
│  │  │(sign)    │  │(shutdown)  │                         │    │
│  │  └──────────┘  └────────────┘                         │    │
│  └──────────────────────────────────────────────────────┘    │
│                           │                                   │
│  ┌────────────────────────▼─────────────────────────────┐    │
│  │           Middleware Stack (15 layers)                 │    │
│  │  Tracing → Metrics → Logger → TracingHeaders →        │    │
│  │  ClearAuth → OAuth → Cookies → Caching → ForwardID → │    │
│  │  AlertHeaders → UserHeader → ACHeader → HTTP → Error  │    │
│  └────────────────────────┬─────────────────────────────┘    │
│                           │                                   │
│  ┌────────────────────────▼─────────────────────────────┐    │
│  │           Client (manager/client/client.go)           │    │
│  │  QueryData() / CallResource() / CheckHealth()         │    │
│  └────────────────────────┬─────────────────────────────┘    │
│                           │                                   │
│              ┌────────────▼────────────┐                     │
│              │    Plugin Registry      │                     │
│              │    (in-memory map)      │                     │
│              └──────┬──────┬──────┬────┘                     │
│                     │      │      │                           │
│              ┌──────▼──┐ ┌─▼────┐ ┌▼─────────┐              │
│              │Core     │ │Local │ │Container  │              │
│              │(in-proc)│ │(exec)│ │(Docker)   │              │
│              └─────────┘ └──────┘ └───────────┘              │
│                              │          │                     │
│                         gRPC │     gRPC │                     │
│                              ▼          ▼                     │
│                     ┌────────────┐ ┌──────────┐              │
│                     │Plugin      │ │Container │              │
│                     │Process     │ │Process   │              │
│                     │(separate)  │ │(Docker)  │              │
│                     └────────────┘ └──────────┘              │
└─────────────────────────────────────────────────────────────┘
```

### 플러그인 유형

| 유형 | Type 상수 | 설명 |
|------|----------|------|
| 데이터소스 | `TypeDataSource` = `"datasource"` | 외부 데이터 연결 (Prometheus, MySQL 등) |
| 패널 | `TypePanel` = `"panel"` | 시각화 컴포넌트 (그래프, 테이블 등) |
| 앱 | `TypeApp` = `"app"` | 페이지/라우트를 포함하는 복합 플러그인 |
| 렌더러 | `TypeRenderer` = `"renderer"` | 이미지 렌더링 (특수 용도) |

### 플러그인 클래스

```go
// pkg/plugins/plugins.go (검증된 코드)
type Class string

const (
    ClassCore     Class = "core"      // Grafana에 내장된 플러그인
    ClassExternal Class = "external"  // 별도 설치된 플러그인
)
```

---

## 2. Plugin 구조체와 핵심 인터페이스

### Plugin 구조체

`pkg/plugins/plugins.go:31-68`에 정의된 `Plugin` 구조체는 플러그인의 모든 메타데이터와
런타임 상태를 포함한다.

```go
// pkg/plugins/plugins.go (검증된 코드)
type Plugin struct {
    JSONData                        // plugin.json에서 파싱된 메타데이터

    FS    FS                        // 플러그인 파일시스템 (가상 FS)
    Class Class                     // core 또는 external

    // App 필드
    IncludedInAppID string          // 상위 앱 플러그인 ID
    DefaultNavURL   string          // 기본 내비게이션 URL
    Pinned          bool            // 고정 여부

    // 서명 필드
    Signature     SignatureStatus   // valid, invalid, modified, unsigned
    SignatureType SignatureType     // grafana, commercial, community
    SignatureOrg  string            // 서명 조직
    Parent        *Plugin           // 부모 플러그인 (앱 하위)
    Children      []*Plugin         // 자식 플러그인
    Error         *Error            // 로드 에러

    // SystemJS 필드
    Module          string          // 프론트엔드 모듈 경로
    BaseURL         string          // 정적 에셋 기본 URL
    LoadingStrategy LoadingStrategy // 로딩 전략

    Angular AngularMeta             // Angular 감지 정보

    ExternalService *auth.ExternalService // 외부 서비스 인증

    Renderer pluginextensionv2.RendererPlugin // 렌더러 플러그인
    client   backendplugin.Plugin             // 백엔드 클라이언트 (private)
    log      log.Logger                       // 로거

    SkipHostEnvVars bool            // 호스트 환경변수 스킵
    mu sync.Mutex                   // 동시성 보호
    Translations map[string]string  // 번역 리소스
}
```

### Plugin이 구현하는 인터페이스

```go
// pkg/plugins/plugins.go:70-79 (검증된 코드)
var (
    _ backend.CollectMetricsHandler   = (*Plugin)(nil)
    _ backend.CheckHealthHandler      = (*Plugin)(nil)
    _ backend.QueryDataHandler        = (*Plugin)(nil)
    _ backend.QueryChunkedDataHandler = (*Plugin)(nil)
    _ backend.CallResourceHandler     = (*Plugin)(nil)
    _ backend.StreamHandler           = (*Plugin)(nil)
    _ backend.AdmissionHandler        = (*Plugin)(nil)
    _ backend.ConversionHandler       = (*Plugin)(nil)
)
```

이 컴파일 타임 체크는 `Plugin`이 8개의 핸들러 인터페이스를 모두 구현함을 보장한다.
실제로 각 메서드는 내부 `client` 필드에 위임한다.

```go
// 예: QueryData 위임 패턴 (검증된 코드)
func (p *Plugin) QueryData(ctx context.Context, req *backend.QueryDataRequest) (*backend.QueryDataResponse, error) {
    pluginClient, ok := p.Client()
    if !ok {
        return nil, ErrPluginUnavailable
    }
    return pluginClient.QueryData(ctx, req)
}
```

### JSONData: plugin.json 메타데이터

```go
// pkg/plugins/plugins.go:86-140 (검증된 코드)
type JSONData struct {
    ID           string       `json:"id"`            // 고유 ID (grafana-clock-panel)
    Type         Type         `json:"type"`           // datasource, panel, app
    Name         string       `json:"name"`           // 표시 이름
    AliasIDs     []string     `json:"aliasIDs"`       // 이전 ID 호환
    Info         Info         `json:"info"`            // 작성자, 링크, 로고
    Dependencies Dependencies `json:"dependencies"`    // 의존성
    Includes     []*Includes  `json:"includes"`        // 포함 리소스
    State        ReleaseState `json:"state"`           // alpha, beta, stable
    Category     string       `json:"category"`        // 카테고리
    Backend      bool         `json:"backend"`         // 백엔드 플러그인 여부
    Routes       []*Route     `json:"routes"`          // 프록시 라우트
    Executable   string       `json:"executable"`      // 실행 파일 이름

    // 데이터소스 전용
    Annotations  bool         `json:"annotations"`     // 어노테이션 지원
    Metrics      bool         `json:"metrics"`         // 메트릭 지원
    Alerting     bool         `json:"alerting"`        // 알림 지원
    Explore      bool         `json:"explore"`         // Explore 지원
    Logs         bool         `json:"logs"`            // 로그 지원
    Tracing      bool         `json:"tracing"`         // 트레이스 지원
    Streaming    bool         `json:"streaming"`       // 스트리밍 지원

    // 패널 전용
    SkipDataQuery bool        `json:"skipDataQuery"`   // 데이터 쿼리 불필요

    // IAM 인증
    IAM *auth.IAM             `json:"iam"`             // 서비스 인증 설정
}
```

### PluginClient 인터페이스

```go
// pkg/plugins/plugins.go:473-482 (검증된 코드)
type PluginClient interface {
    backend.QueryDataHandler         // 쿼리 실행
    backend.QueryChunkedDataHandler  // 청크 쿼리
    backend.CollectMetricsHandler    // 메트릭 수집
    backend.CheckHealthHandler       // 헬스 체크
    backend.CallResourceHandler      // 리소스 호출
    backend.AdmissionHandler         // 어드미션 (K8s 통합)
    backend.ConversionHandler        // 오브젝트 변환
    backend.StreamHandler            // 스트리밍
}
```

---

## 3. gRPC 프로토콜

### Handshake 설정

```go
// pkg/plugins/backendplugin/grpcplugin/client.go:24-33 (검증된 코드)
var handshake = goplugin.HandshakeConfig{
    ProtocolVersion: grpcplugin.ProtocolVersion, // 프로토콜 버전
    MagicCookieKey:   grpcplugin.MagicCookieKey,   // "GF_PLUGIN_GRPC_TOKEN"
    MagicCookieValue: grpcplugin.MagicCookieValue,  // 매직 쿠키 값
}
```

Handshake는 HashiCorp `go-plugin` 프레임워크의 핵심 보안 메커니즘이다.
Grafana 서버가 플러그인 프로세스를 시작할 때, 환경 변수로 매직 쿠키를 전달한다.
플러그인은 이 값을 검증하여 Grafana가 아닌 다른 프로그램이 실행한 것이 아닌지 확인한다.

### Plugin Set (gRPC 서비스 매핑)

```go
// pkg/plugins/backendplugin/grpcplugin/client.go:36-46 (검증된 코드)
var pluginSet = map[int]goplugin.PluginSet{
    grpcplugin.ProtocolVersion: {
        "diagnostics": &grpcplugin.DiagnosticsGRPCPlugin{}, // 헬스체크, 메트릭
        "resource":    &grpcplugin.ResourceGRPCPlugin{},     // 리소스 호출
        "data":        &grpcplugin.DataGRPCPlugin{},         // 쿼리 실행
        "stream":      &grpcplugin.StreamGRPCPlugin{},       // 스트리밍
        "admission":   &grpcplugin.AdmissionGRPCPlugin{},    // K8s 어드미션
        "conversion":  &grpcplugin.ConversionGRPCPlugin{},   // K8s 변환
        "renderer":    &pluginextensionv2.RendererGRPCPlugin{}, // 이미지 렌더링
    },
}
```

각 서비스는 독립적으로 구현할 수 있다. 예를 들어, 단순 데이터소스 플러그인은
`data`와 `diagnostics`만 구현하면 된다. 구현되지 않은 서비스 호출 시
`ErrMethodNotImplemented` 에러가 반환된다.

### 클라이언트 생성 (Process vs Container 모드)

```go
// pkg/plugins/backendplugin/grpcplugin/client.go:61-103 (검증된 코드)
func newClientConfig(descriptor PluginDescriptor, env []string, logger log.Logger,
    tracer trace.Tracer) (*goplugin.ClientConfig, error) {

    // Linux에서 컨테이너 모드가 활성화된 경우
    if runtime.GOOS == "linux" && descriptor.containerMode.enabled {
        return containerClientConfig(executablePath, containerImage, containerTag,
            logger, versionedPlugins, skipHostEnvVars, tracer), nil
    }

    // 기본: 프로세스 모드
    cfg := &goplugin.ClientConfig{
        HandshakeConfig:  handshake,
        VersionedPlugins: versionedPlugins,
        SkipHostEnv:      skipHostEnvVars,
        Logger:           logWrapper{Logger: logger},
        AllowedProtocols: []goplugin.Protocol{goplugin.ProtocolGRPC},
        GRPCDialOptions: []grpc.DialOption{
            grpc.WithStatsHandler(otelgrpc.NewClientHandler(
                otelgrpc.WithTracerProvider(newClientTracerProvider(tracer)))),
        },
    }

    if descriptor.runnerFunc != nil {
        // Runner 모드: Unix 소켓 통신
        cfg.RunnerFunc = descriptor.runnerFunc
        cfg.UnixSocketConfig = &goplugin.UnixSocketConfig{TempDir: td}
    } else {
        // 프로세스 모드: exec.Command로 직접 실행
        cfg.Cmd = exec.Command(executablePath, descriptor.executableArgs...)
        cfg.Cmd.Env = env
    }

    return cfg, nil
}
```

### 실행 모드 비교

```
┌─────────────────────────────────────────────────────────┐
│                    실행 모드 비교                         │
├──────────────┬───────────────┬───────────────────────────┤
│              │ Process Mode  │ Container Mode            │
├──────────────┼───────────────┼───────────────────────────┤
│ 플랫폼       │ 모든 OS       │ Linux만                   │
│ 통신 방식    │ gRPC (TCP)    │ gRPC (Unix Socket)        │
│ 실행 방법    │ exec.Command  │ Docker Container          │
│ 격리 수준    │ 프로세스 격리  │ 컨테이너 격리             │
│ 환경 변수    │ 호스트 전달    │ 선택적 전달               │
│ 보안         │ OS 레벨       │ 컨테이너 샌드박스          │
│ 사용 사례    │ 기본 모드      │ 엔터프라이즈 보안 요건    │
└──────────────┴───────────────┴───────────────────────────┘
```

### ClientV2 구성

```go
// pkg/plugins/backendplugin/grpcplugin/client_v2.go:27-35 (검증된 코드)
type ClientV2 struct {
    grpcplugin.DiagnosticsClient    // CheckHealth, CollectMetrics
    grpcplugin.ResourceClient       // CallResource
    grpcplugin.DataClient           // QueryData, QueryChunkedData
    grpcplugin.StreamClient         // Subscribe, Publish, RunStream
    grpcplugin.AdmissionClient      // ValidateAdmission, MutateAdmission
    grpcplugin.ConversionClient     // ConvertObjects
    pluginextensionv2.RendererPlugin // Render (이미지 렌더링)
}
```

`newClientV2` 함수에서 각 서비스를 `rpcClient.Dispense("서비스명")`으로 가져온다.
서비스가 구현되지 않았으면 해당 필드는 `nil`로 남고,
호출 시 `ErrMethodNotImplemented`가 반환된다.

---

## 4. 플러그인 라이프사이클

### 상태 머신

```go
// pkg/plugins/backendplugin/grpcplugin/grpc_plugin.go:28-36 (검증된 코드)
type pluginState int

const (
    pluginStateNotStarted  pluginState = iota  // 0: 시작 안 됨
    pluginStateStartInit                        // 1: 시작 초기화 중
    pluginStateStartSuccess                     // 2: 시작 성공
    pluginStateStartFail                        // 3: 시작 실패
    pluginStateStopped                          // 4: 정지됨
)
```

### 상태 전이 다이어그램

```
                    ┌──────────────────┐
                    │  NotStarted (0)  │
                    └────────┬─────────┘
                             │ Start()
                             ▼
                    ┌──────────────────┐
                    │  StartInit (1)   │
                    └────┬────────┬────┘
                         │        │
              성공       │        │ 실패
                         ▼        ▼
              ┌────────────┐  ┌────────────┐
              │StartSuccess│  │ StartFail  │
              │   (2)      │  │   (3)      │
              └──────┬─────┘  └────────────┘
                     │ Stop()
                     ▼
              ┌────────────┐
              │  Stopped   │
              │   (4)      │
              └────────────┘
```

### Start() 흐름 상세

```go
// pkg/plugins/backendplugin/grpcplugin/grpc_plugin.go:68-103 (검증된 코드)
func (p *grpcPlugin) Start(_ context.Context) error {
    p.mutex.Lock()
    defer p.mutex.Unlock()

    p.state = pluginStateStartInit   // 1단계: 시작 초기화

    // 2단계: go-plugin Client 생성
    var err error
    p.client, err = p.clientFactory()
    if err != nil {
        p.state = pluginStateStartFail
        return err
    }

    // 3단계: RPC Client 획득
    rpcClient, err := p.client.Client()
    if err != nil {
        p.state = pluginStateStartFail
        return err
    }

    // 4단계: 프로토콜 버전 확인 (v2 이상 필수)
    if p.client.NegotiatedVersion() < 2 {
        p.state = pluginStateStartFail
        return errors.New("plugin protocol version not supported")
    }

    // 5단계: ClientV2 생성 (gRPC 서비스 디스펜스)
    p.pluginClient, err = newClientV2(p.descriptor, p.logger, rpcClient)
    if err != nil {
        p.state = pluginStateStartFail
        return err
    }

    if p.pluginClient == nil {
        p.state = pluginStateStartFail
        return errors.New("no compatible plugin implementation found")
    }

    p.state = pluginStateStartSuccess  // 6단계: 시작 성공
    return nil
}
```

### Stop() 흐름

```go
// grpc_plugin.go:105-114 (검증된 코드)
func (p *grpcPlugin) Stop(_ context.Context) error {
    p.mutex.Lock()
    defer p.mutex.Unlock()

    if p.client != nil {
        p.client.Kill()  // gRPC 프로세스 종료
    }
    p.state = pluginStateStopped
    return nil
}
```

### 시퀀스 다이어그램: 플러그인 시작

```
Grafana Server         go-plugin Client        Plugin Process
     │                       │                       │
     │  Start()              │                       │
     ├──────────────────────>│                       │
     │                       │  exec.Command()       │
     │                       ├──────────────────────>│
     │                       │                       │ 프로세스 시작
     │                       │  gRPC Handshake       │
     │                       │<─────────────────────>│
     │                       │                       │
     │  rpcClient.Client()   │                       │
     ├──────────────────────>│                       │
     │                       │  NegotiatedVersion()  │
     │  version >= 2 확인    │                       │
     │                       │                       │
     │  Dispense("data")     │                       │
     ├──────────────────────>│  gRPC 서비스 연결      │
     │                       ├──────────────────────>│
     │  Dispense("diagnostics")                      │
     ├──────────────────────>│                       │
     │  Dispense("resource") │                       │
     ├──────────────────────>│                       │
     │  Dispense("stream")   │                       │
     ├──────────────────────>│                       │
     │  Dispense("admission")│                       │
     ├──────────────────────>│                       │
     │  Dispense("conversion")                       │
     ├──────────────────────>│                       │
     │  Dispense("renderer") │                       │
     ├──────────────────────>│                       │
     │                       │                       │
     │  ClientV2 생성 완료    │                       │
     │  state = StartSuccess │                       │
     │                       │                       │
```

### getPluginClient: 요청 시 클라이언트 검증

```go
// grpc_plugin.go:148-173 (검증된 코드)
func (p *grpcPlugin) getPluginClient(ctx context.Context) (*ClientV2, bool) {
    p.mutex.RLock()
    defer p.mutex.RUnlock()

    // 클라이언트가 살아있고 pluginClient가 존재하면 반환
    if p.client != nil && !p.client.Exited() && p.pluginClient != nil {
        return p.pluginClient, true
    }

    // 상태별 디버그 로깅
    logger := p.Logger().FromContext(ctx)
    switch p.state {
    case pluginStateNotStarted:
        logger.Debug("Plugin client has not been started yet")
    case pluginStateStartInit:
        logger.Debug("Plugin client is starting")
    case pluginStateStartFail:
        logger.Debug("Plugin client failed to start")
    case pluginStateStopped:
        logger.Debug("Plugin client has stopped")
    }

    return nil, false
}
```

---

## 5. 로드 파이프라인

### 파이프라인 문서 (공식)

```go
// pkg/plugins/manager/pipeline/doc.go (검증된 코드)
// 파이프라인은 순서대로 실행되는 단계(stage)의 시퀀스이다.
// 각 단계는 일련의 스텝(step)으로 구성된다.
//
//   Discovery:      플러그인 발견 (디스크, 원격 등), 결과 필터링
//   Bootstrap:      발견된 플러그인 생성, 메타데이터 보강
//   Validation:     플러그인 검증 (서명, Angular 등)
//   Initialization: 플러그인 초기화 (등록, 백엔드 프로세스 시작, RBAC 등)
//   Termination:    플러그인 종료 (백엔드 프로세스 중지, 정리)
```

### 파이프라인 단계별 상세

```
┌──────────────────────────────────────────────────────────────┐
│                     Load Pipeline                             │
│                                                               │
│  ┌──────────┐    ┌──────────┐    ┌────────────┐              │
│  │Discovery │───>│Bootstrap │───>│Validation  │              │
│  │          │    │          │    │            │              │
│  │ - 경로 스캔│    │ - JSON 파싱│    │ - 서명 검증│              │
│  │ - plugin  │    │ - 메타데이터│    │ - 보안 정책│              │
│  │   .json   │    │   보강     │    │ - Angular │              │
│  │   찾기    │    │ - 의존성   │    │   감지    │              │
│  │          │    │   해석     │    │           │              │
│  └──────────┘    └──────────┘    └─────┬──────┘              │
│                                        │                      │
│                                        ▼                      │
│  ┌──────────────┐    ┌────────────────────────┐              │
│  │Termination   │    │Initialization          │              │
│  │              │    │                        │              │
│  │ - 프로세스    │    │ - Registry 등록        │              │
│  │   종료       │    │ - 백엔드 프로세스 시작   │              │
│  │ - 리소스     │    │ - gRPC 연결 수립        │              │
│  │   정리       │    │ - RBAC 역할 선언        │              │
│  └──────────────┘    └────────────────────────┘              │
└──────────────────────────────────────────────────────────────┘
```

### Discovery 단계

```
pkg/plugins/manager/pipeline/discovery/
│
├── 입력: PluginsPaths (설정에서 지정된 경로 목록)
├── 동작:
│   1. 각 경로를 순회하며 plugin.json 파일 검색
│   2. 서브디렉토리 재귀 탐색
│   3. 중복 ID 필터링
│   4. 발견된 플러그인 목록 반환
└── 출력: []{pluginID, pluginDir, pluginJSON}
```

### Bootstrap 단계

```
pkg/plugins/manager/pipeline/bootstrap/
│
├── 입력: 발견된 플러그인 목록
├── 동작:
│   1. plugin.json 파싱 → JSONData 구조체
│   2. ReadPluginJSON() 검증
│   3. 의존성 해석 (Grafana 버전 호환성)
│   4. 기본값 설정 (Dependencies.GrafanaVersion = "*")
│   5. Include 역할 기본값 (Viewer)
│   6. 앱 플러그인 Include 액션 (AppAccess)
└── 출력: []Plugin (초기화된 Plugin 구조체)
```

### Validation 단계

```
pkg/plugins/manager/pipeline/validation/
│
├── 입력: 부트스트랩된 플러그인
├── 동작:
│   1. 서명 검증 (MANIFEST.txt)
│   2. 허용되지 않은 unsigned 플러그인 차단
│   3. Angular 플러그인 감지 (사용 중단 경고)
│   4. 보안 정책 적용
└── 출력: 검증된 플러그인 (SignatureStatus 설정)
```

### Initialization 단계

```
pkg/plugins/manager/pipeline/initialization/
│
├── 입력: 검증된 플러그인
├── 동작:
│   1. Plugin Registry에 등록
│   2. Backend=true인 플러그인:
│      a. PluginFactoryFunc 호출
│      b. grpcPlugin 생성
│      c. Start() 호출 → gRPC 프로세스 시작
│   3. RBAC 역할 선언 (Roles, ActionSets)
│   4. Static Route 등록 (프론트엔드 에셋)
└── 출력: 실행 중인 플러그인
```

### Termination 단계

```
pkg/plugins/manager/pipeline/termination/
│
├── 입력: 종료할 플러그인
├── 동작:
│   1. Decommission() → 비활성화 마킹
│   2. Stop() → gRPC 프로세스 Kill
│   3. Registry에서 제거
│   4. 리소스 정리
└── 출력: (없음)
```

---

## 6. 쿼리 디스패치

### Client Service (manager/client/client.go)

```go
// pkg/plugins/manager/client/client.go (검증된 코드)
type Service struct {
    pluginRegistry registry.Service
}

func ProvideService(pluginRegistry registry.Service) *Service {
    return &Service{pluginRegistry: pluginRegistry}
}
```

### QueryData 흐름

```go
// client.go:50-95 (검증된 코드)
func (s *Service) QueryData(ctx context.Context, req *backend.QueryDataRequest) (*backend.QueryDataResponse, error) {
    if req == nil {
        return nil, errNilRequest
    }

    // 1. Plugin Registry에서 플러그인 조회
    p, exists := s.plugin(ctx, req.PluginContext.PluginID, req.PluginContext.PluginVersion)
    if !exists {
        return nil, plugins.ErrPluginNotRegistered
    }

    // 2. 플러그인에 쿼리 위임
    resp, err := p.QueryData(ctx, req)
    if err != nil {
        // 3. 에러 처리: passthrough 에러는 그대로 반환
        for _, e := range passthroughErrors {
            if errors.Is(err, e) {
                return nil, err
            }
        }

        // 4. gRPC 연결 불가 에러
        if errors.Is(err, plugins.ErrPluginGrpcConnectionUnavailableBaseFn(ctx)) {
            return nil, err
        }

        // 5. 취소된 요청
        if errors.Is(err, context.Canceled) {
            return nil, plugins.ErrPluginRequestCanceledErrorBase.Errorf(
                "client: query data request canceled: %w", err)
        }

        // 6. gRPC 상태 코드 매핑
        if s, ok := grpcstatus.FromError(err); ok && s.Code() == grpccodes.Canceled {
            return nil, plugins.ErrPluginRequestCanceledErrorBase.Errorf(
                "client: query data request canceled: %w", err)
        }

        // 7. 일반 실패
        return nil, plugins.ErrPluginRequestFailureErrorBase.Errorf(
            "client: failed to query data: %w", err)
    }

    // 8. RefID 보정
    for refID, res := range resp.Responses {
        for _, f := range res.Frames {
            if f.RefID == "" {
                f.RefID = refID
            }
        }
    }

    return resp, err
}
```

### Passthrough 에러

```go
// client.go:34-38 (검증된 코드)
var passthroughErrors = []error{
    plugins.ErrPluginUnavailable,                // 플러그인 사용 불가
    plugins.ErrMethodNotImplemented,             // 메서드 미구현
    plugins.ErrPluginGrpcResourceExhaustedBase,  // gRPC 리소스 소진
}
```

이 에러들은 미들웨어 스택을 거치지 않고 직접 호출자에게 전달된다.
이를 통해 호출자가 에러 유형에 따라 적절한 처리를 할 수 있다.

### plugin() 조회 함수

```go
// client.go:306-317 (검증된 코드)
func (s *Service) plugin(ctx context.Context, pluginID, pluginVersion string) (*plugins.Plugin, bool) {
    p, exists := s.pluginRegistry.Plugin(ctx, pluginID, pluginVersion)
    if !exists {
        return nil, false
    }

    // Decommissioned 플러그인은 사용 불가
    if p.IsDecommissioned() {
        return nil, false
    }

    return p, true
}
```

### 시퀀스 다이어그램: 쿼리 요청

```
HTTP Client         API Server         Middleware Stack       Client Service      Plugin
    │                   │                    │                     │                 │
    │  POST /api/ds/    │                    │                     │                 │
    │  query            │                    │                     │                 │
    ├──────────────────>│                    │                     │                 │
    │                   │  QueryData(req)    │                     │                 │
    │                   ├───────────────────>│                     │                 │
    │                   │                    │  Tracing            │                 │
    │                   │                    │  Metrics            │                 │
    │                   │                    │  Logger             │                 │
    │                   │                    │  TracingHeaders     │                 │
    │                   │                    │  ClearAuthHeaders   │                 │
    │                   │                    │  OAuthToken         │                 │
    │                   │                    │  Cookies            │                 │
    │                   │                    │  Caching            │                 │
    │                   │                    │  ForwardID          │                 │
    │                   │                    │  AlertHeaders       │                 │
    │                   │                    │  UserHeader         │                 │
    │                   │                    │  HttpClient         │                 │
    │                   │                    │  ErrorSource        │                 │
    │                   │                    │  QueryData(req)     │                 │
    │                   │                    ├────────────────────>│                 │
    │                   │                    │                     │ plugin lookup   │
    │                   │                    │                     │ p.QueryData()   │
    │                   │                    │                     ├────────────────>│
    │                   │                    │                     │                 │
    │                   │                    │                     │  gRPC call      │
    │                   │                    │                     │<────────────────│
    │                   │                    │                     │  resp           │
    │                   │                    │<────────────────────│                 │
    │                   │<───────────────────│                     │                 │
    │<──────────────────│  JSON response     │                     │                 │
    │                   │                    │                     │                 │
```

---

## 7. 미들웨어 스택

### 미들웨어 구성

`pkg/services/pluginsintegration/pluginsintegration.go:189-227`에서 정의된
미들웨어 스택은 모든 플러그인 요청을 가로챈다.

```go
// pluginsintegration.go:189-227 (검증된 코드)
func CreateMiddlewares(cfg *setting.Cfg, oAuthTokenService oauthtoken.OAuthTokenService,
    tracer tracing.Tracer, cachingServiceClient *caching.CachingServiceClient,
    features featuremgmt.FeatureToggles, promRegisterer prometheus.Registerer,
    registry registry.Service) []backend.HandlerMiddleware {

    middlewares := []backend.HandlerMiddleware{
        // 1. 분산 추적
        clientmiddleware.NewTracingMiddleware(tracer),
        // 2. Prometheus 메트릭
        clientmiddleware.NewMetricsMiddleware(promRegisterer, registry),
        // 3. 컨텍스트 로거 (요청별 로깅)
        clientmiddleware.NewContextualLoggerMiddleware(),
    }

    // 4. 요청 로거 (선택적, 설정 기반)
    if cfg.PluginLogBackendRequests {
        middlewares = append(middlewares,
            clientmiddleware.NewLoggerMiddleware(log.New("plugin.instrumentation"), registry))
    }

    middlewares = append(middlewares,
        // 5. 추적 헤더 전파
        clientmiddleware.NewTracingHeaderMiddleware(),
        // 6. 인증 헤더 제거 (JWT, AuthProxy)
        clientmiddleware.NewClearAuthHeadersMiddleware(&cfg.JWTAuth, &cfg.AuthProxy),
        // 7. OAuth 토큰 주입
        clientmiddleware.NewOAuthTokenMiddleware(oAuthTokenService),
        // 8. 쿠키 전달 (로그인 쿠키 제외)
        clientmiddleware.NewCookiesMiddleware(skipCookiesNames),
        // 9. 응답 캐싱
        clientmiddleware.NewCachingMiddleware(cachingServiceClient),
        // 10. Forward ID 헤더
        clientmiddleware.NewForwardIDMiddleware(),
        // 11. 알림 헤더 (Alert evaluation 표시)
        clientmiddleware.NewUseAlertHeadersMiddleware(),
    )

    // 12. 사용자 헤더 (선택적)
    if cfg.SendUserHeader {
        middlewares = append(middlewares, clientmiddleware.NewUserHeaderMiddleware())
    }

    // 13. IP Range AC 헤더 (Hosted Grafana, 선택적)
    if cfg.IPRangeACEnabled {
        middlewares = append(middlewares,
            clientmiddleware.NewHostedGrafanaACHeaderMiddleware(cfg))
    }

    // 14. HTTP 클라이언트 설정
    middlewares = append(middlewares, clientmiddleware.NewHTTPClientMiddleware())

    // 15. 에러 소스 (최하단 -- 필수)
    middlewares = append(middlewares, backend.NewErrorSourceMiddleware())

    return middlewares
}
```

### 미들웨어 상세 설명

| 순서 | 미들웨어 | 역할 | 파일 |
|------|----------|------|------|
| 1 | `TracingMiddleware` | OpenTelemetry 스팬 생성, 플러그인 호출 추적 | `tracing_middleware.go` |
| 2 | `MetricsMiddleware` | 요청 수, 지연 시간 Prometheus 메트릭 | `metrics_middleware.go` |
| 3 | `ContextualLoggerMiddleware` | 요청 컨텍스트에 로거 주입 | `contextual_logger_middleware.go` |
| 4 | `LoggerMiddleware` | 요청/응답 상세 로깅 (선택적) | `logger_middleware.go` |
| 5 | `TracingHeaderMiddleware` | W3C TraceContext 헤더 전파 | `tracing_header_middleware.go` |
| 6 | `ClearAuthHeadersMiddleware` | JWT, AuthProxy 인증 헤더 제거 | `clear_auth_headers_middleware.go` |
| 7 | `OAuthTokenMiddleware` | 데이터소스 OAuth 토큰 자동 주입 | `oauthtoken_middleware.go` |
| 8 | `CookiesMiddleware` | 허용된 쿠키 전달 (로그인 쿠키 제외) | `cookies_middleware.go` |
| 9 | `CachingMiddleware` | 쿼리 응답 캐싱 (Enterprise) | `caching_middleware.go` |
| 10 | `ForwardIDMiddleware` | Grafana 요청 ID 전달 | `forward_id_middleware.go` |
| 11 | `UseAlertHeadersMiddleware` | 알림 평가 컨텍스트 표시 | `usealertingheaders_middleware.go` |
| 12 | `UserHeaderMiddleware` | X-Grafana-User 헤더 주입 | `user_header_middleware.go` |
| 13 | `HostedGrafanaACHeaderMiddleware` | IP 범위 접근 제어 | `grafana_request_id_header_middleware.go` |
| 14 | `HTTPClientMiddleware` | HTTP 클라이언트 설정 전파 | `httpclient_middleware.go` |
| 15 | `ErrorSourceMiddleware` | 에러 소스 분류 (plugin/downstream) | SDK 내장 |

### 왜 15개나 되는 미들웨어가 필요한가?

각 미들웨어는 하나의 관심사만 담당한다 (Single Responsibility).

1. **보안**: 인증 헤더 제거(6), OAuth 토큰(7), 사용자 헤더(12), IP AC(13) -- 플러그인이 불필요한 인증 정보에 접근하지 못하게 하면서, 필요한 인증 정보는 정확히 전달
2. **관측**: 추적(1,5), 메트릭(2), 로깅(3,4) -- 플러그인 호출의 완전한 관측 가능성
3. **기능**: 캐싱(9), 쿠키(8), 알림 컨텍스트(11) -- 비즈니스 로직 지원
4. **에러 처리**: ErrorSource(15) -- 에러가 플러그인에서 온 것인지, 다운스트림에서 온 것인지 분류

### 미들웨어 체인 동작

```
┌──────────────┐
│ 요청 (req)    │
└──────┬───────┘
       │
       ▼
┌──────────────┐  ┌──────────────┐  ┌──────────────┐
│  Tracing     │─>│  Metrics     │─>│ ContextLogger│─> ...
│  (before)    │  │  (before)    │  │  (before)    │
└──────────────┘  └──────────────┘  └──────────────┘
                                           │
       ... ────────────────────────────────>│
                                           ▼
                                    ┌──────────────┐
                                    │ ErrorSource  │
                                    │ (최하단)      │
                                    └──────┬───────┘
                                           │
                                           ▼
                                    ┌──────────────┐
                                    │ Client       │
                                    │ Service      │
                                    │ (실제 호출)   │
                                    └──────┬───────┘
                                           │
                                           ▼
       <───────────────────────────────────│
┌──────────────┐  ┌──────────────┐  ┌──────────────┐
│ ContextLogger│<─│  Metrics     │<─│  Tracing     │
│  (after)     │  │  (after)     │  │  (after)     │
└──────────────┘  └──────────────┘  └──────────────┘
       │
       ▼
┌──────────────┐
│ 응답 (resp)   │
└──────────────┘
```

---

## 8. 프론트엔드 플러그인

### 패널 플러그인

```typescript
// @grafana/data PanelPlugin API
import { PanelPlugin } from '@grafana/data';

// 플러그인 등록
export const plugin = new PanelPlugin<MyOptions>(MyPanelComponent)
  .setPanelOptions((builder) => {
    builder
      .addTextInput({
        path: 'title',
        name: 'Title',
        defaultValue: 'Hello',
      })
      .addBooleanSwitch({
        path: 'showLegend',
        name: 'Show Legend',
        defaultValue: true,
      });
  })
  .useFieldConfig({
    standardOptions: {
      color: { defaultValue: { mode: 'palette-classic' } },
    },
  });

// 패널 컴포넌트
function MyPanelComponent(props: PanelProps<MyOptions>) {
  const { data, options, width, height, timeRange, fieldConfig } = props;

  // data.series: DataFrame[]
  // options: MyOptions (setPanelOptions에서 정의한 옵션)
  // fieldConfig: FieldConfigSource (useFieldConfig에서 정의한 필드 설정)

  return <div style={{ width, height }}>
    {/* 시각화 렌더링 */}
  </div>;
}
```

### 데이터소스 플러그인

```typescript
// @grafana/data DataSourceApi (API)
import { DataSourceApi, DataQueryRequest, DataQueryResponse } from '@grafana/data';

export class MyDataSource extends DataSourceApi<MyQuery, MyOptions> {
  // 쿼리 실행 (필수)
  async query(request: DataQueryRequest<MyQuery>): Promise<DataQueryResponse> {
    const { targets, range, maxDataPoints } = request;
    // targets: MyQuery[] (사용자가 입력한 쿼리)
    // range: TimeRange (시간 범위)
    // maxDataPoints: number (최대 데이터포인트 수)

    const frames = await this.fetchData(targets, range);
    return { data: frames };
  }

  // 연결 테스트 (필수)
  async testDatasource(): Promise<{ status: string; message: string }> {
    try {
      await this.healthCheck();
      return { status: 'success', message: 'Connection OK' };
    } catch (err) {
      return { status: 'error', message: err.message };
    }
  }
}
```

### 앱 플러그인

```typescript
// @grafana/data AppPlugin API
import { AppPlugin } from '@grafana/data';

export const plugin = new AppPlugin()
  .setRootPage(RootPage)         // 기본 페이지
  .addConfigPage({               // 설정 페이지
    title: 'Configuration',
    icon: 'cog',
    body: ConfigPage,
    id: 'config',
  });

// 앱은 자체 라우트를 등록할 수 있음
// routes.tsx의 getAppPluginRoutes()에서 로드됨
```

### 플러그인 로딩 과정

```
1. routes.tsx: getAppPluginRoutes()
   └── 등록된 앱 플러그인의 라우트 수집

2. importPanelPlugin(pluginId)
   └── SystemJS.import() 또는 webpack dynamic import
       └── plugin.json의 module 경로로 JS 번들 로드
           └── PanelPlugin 인스턴스 반환

3. 플러그인 캐싱
   └── syncGetPanelPlugin(): 메모리 캐시에서 즉시 반환
       └── 로드되지 않은 경우 importPanelPlugin() 호출
```

---

## 9. 플러그인 설치 및 관리

### Wire 의존성 주입

`pluginsintegration.go`의 `WireSet`은 플러그인 시스템의 모든 컴포넌트를 Wire로 연결한다.

```go
// pluginsintegration.go:69-144 (검증된 코드, 주요 부분)
var WireSet = wire.NewSet(
    // 설정
    pluginconfig.ProvidePluginManagementConfig,
    pluginconfig.ProvidePluginInstanceConfig,

    // Store (플러그인 조회)
    pluginstore.ProvideService,
    wire.Bind(new(pluginstore.Store), new(*pluginstore.Service)),

    // 프로세스 관리
    process.ProvideService,

    // 코어 플러그인 레지스트리
    coreplugin.ProvideCoreRegistry,

    // CDN
    pluginscdn.ProvideService,

    // 파이프라인 단계
    pipeline.ProvideDiscoveryStage,    // 발견
    pipeline.ProvideBootstrapStage,    // 부트스트랩
    pipeline.ProvideInitializationStage, // 초기화
    pipeline.ProvideTerminationStage,  // 종료
    pipeline.ProvideValidationStage,   // 검증

    // 서명
    signature.ProvideValidatorService,
    signature.ProvideService,

    // 로더
    loader.ProvideService,

    // 에러 추적
    pluginerrs.ProvideErrorTracker,

    // 레지스트리 (in-memory)
    registry.ProvideService,

    // 리포지토리 (grafana.com API)
    repo.ProvideService,

    // 라이센싱
    licensing.ProvideLicensing,

    // 설정 관리
    pluginSettings.ProvideService,

    // 키 저장소 (서명 검증 키)
    keystore.ProvideService,
    keyretriever.ProvideService,

    // 렌더러
    renderer.ProvideService,

    // 설치
    plugininstaller.ProvideService,

    // 에셋
    pluginassets.ProvideService,

    // SSO
    pluginsso.ProvideDefaultSettingsProvider,
)
```

### 플러그인 설치 흐름

```
사용자: grafana-cli plugins install grafana-clock-panel
│
├── 1. repo.Service: grafana.com API 호출
│   └── GET https://grafana.com/api/plugins/grafana-clock-panel/versions
│       └── 최신 호환 버전 확인
│
├── 2. 다운로드
│   └── ZIP 파일 다운로드 → plugins/ 디렉토리에 해제
│
├── 3. 서명 검증
│   └── MANIFEST.txt 확인 → 파일 해시 비교
│
├── 4. Grafana 재시작 시
│   └── Load Pipeline 실행
│       └── Discovery → Bootstrap → Validation → Initialization
│
└── 5. 플러그인 사용 가능
```

### Preinstall (자동 설치)

```go
// pluginchecker.Preinstall 인터페이스
// 서버 시작 시 필수 플러그인을 자동으로 설치
type Preinstall interface {
    // Sync: 동기적 설치 (서버 시작 차단)
    Sync(ctx context.Context) error
    // Async: 비동기적 설치 (백그라운드)
    Async(ctx context.Context) error
}
```

### InstallSync (설치 동기화)

```go
// installsync.Syncer: 여러 Grafana 인스턴스 간 플러그인 설치 동기화
// 데이터베이스에 설치된 플러그인 목록을 저장하고,
// 새 인스턴스가 시작될 때 동기화
```

---

## 10. 보안: 서명 검증

### 서명 유형

```
┌──────────────────────────────────────────────────────┐
│                 서명 검증 시스템                       │
├──────────────┬──────────────┬────────────────────────┤
│ 유형          │ 서명자        │ 신뢰 수준              │
├──────────────┼──────────────┼────────────────────────┤
│ grafana      │ Grafana Labs │ 최고 (코어/공식)        │
│ commercial   │ 파트너사      │ 높음 (상업 계약)        │
│ community    │ 커뮤니티      │ 보통 (공개 키 검증)     │
│ unsigned     │ 없음          │ 경고/차단              │
└──────────────┴──────────────┴────────────────────────┘
```

### MANIFEST.txt 구조

```
-----BEGIN PGP SIGNED MESSAGE-----
Hash: SHA256

{
  "manifestVersion": "2.0.0",
  "signatureType": "grafana",
  "signedByOrg": "grafana",
  "signedByOrgName": "Grafana Labs",
  "rootUrls": [],
  "plugin": "grafana-clock-panel",
  "version": "2.1.0",
  "files": {
    "plugin.json": "sha256:abc123...",
    "module.js": "sha256:def456...",
    "module.js.map": "sha256:ghi789..."
  },
  "time": 1641234567890
}
-----BEGIN PGP SIGNATURE-----
...PGP 서명...
-----END PGP SIGNATURE-----
```

### 검증 프로세스

```
MANIFEST.txt 로드
│
├── 1. PGP 서명 검증
│   └── Grafana 공개 키로 서명 유효성 확인
│
├── 2. 매니페스트 파싱
│   └── JSON 파싱 → plugin ID, version, files 추출
│
├── 3. 파일 해시 비교
│   └── 각 파일의 SHA256 해시 계산 → 매니페스트와 비교
│       ├── 일치: SignatureStatus = "valid"
│       ├── 불일치: SignatureStatus = "modified"
│       └── 누락: SignatureStatus = "modified"
│
├── 4. Unsigned 처리
│   └── allow_loading_unsigned_plugins 설정 확인
│       ├── 허용: 경고 로그 + 로드
│       └── 차단: 에러 로그 + 로드 거부
│
└── 5. SignatureStatus 설정
    └── Plugin.Signature = valid | invalid | modified | unsigned
```

### 왜 서명 검증이 중요한가?

Grafana 플러그인은 서버에서 임의 코드를 실행할 수 있다 (백엔드 플러그인의 경우).
서명 검증은 다음을 보장한다:

1. **무결성**: 플러그인 파일이 배포 후 변조되지 않았음
2. **출처 확인**: 신뢰할 수 있는 조직이 빌드했음
3. **공급망 보안**: 악성 코드 주입 방지

---

## 요약

### 플러그인 시스템 아키텍처 총괄

```
┌─────────────────────────────────────────────────────────────────┐
│                    Grafana Plugin System                         │
│                                                                  │
│  ┌──────────────────────────────────────────────────────────┐    │
│  │  Frontend                                                │    │
│  │  ┌────────────┐  ┌──────────────┐  ┌──────────────────┐  │    │
│  │  │PanelPlugin │  │DataSourceApi │  │AppPlugin         │  │    │
│  │  │(React)     │  │(query/test)  │  │(pages/routes)    │  │    │
│  │  └──────┬─────┘  └──────┬───────┘  └──────┬───────────┘  │    │
│  │         │               │                  │              │    │
│  │         └───────────────┼──────────────────┘              │    │
│  │                         │ @grafana/runtime                │    │
│  │                         ▼                                 │    │
│  │              ┌──────────────────┐                         │    │
│  │              │ BackendSrv       │                         │    │
│  │              │ (HTTP API calls) │                         │    │
│  │              └────────┬─────────┘                         │    │
│  └───────────────────────┼──────────────────────────────────┘    │
│                          │                                       │
│  ┌───────────────────────▼──────────────────────────────────┐    │
│  │  Backend                                                  │    │
│  │  ┌────────────────────────────────────────────────────┐   │    │
│  │  │  Middleware Stack (15 layers)                       │   │    │
│  │  │  Tracing → Metrics → Logger → ... → ErrorSource    │   │    │
│  │  └───────────────────────┬────────────────────────────┘   │    │
│  │                          │                                │    │
│  │  ┌───────────────────────▼────────────────────────────┐   │    │
│  │  │  Client Service → Plugin Registry → Plugin         │   │    │
│  │  │                                      │             │   │    │
│  │  │                          ┌───────────┼───────────┐ │   │    │
│  │  │                          │ Core      │ External  │ │   │    │
│  │  │                          │ (in-proc) │ (gRPC)    │ │   │    │
│  │  │                          └───────────┴───────────┘ │   │    │
│  │  └────────────────────────────────────────────────────┘   │    │
│  │                                                           │    │
│  │  ┌────────────────────────────────────────────────────┐   │    │
│  │  │  Load Pipeline                                     │   │    │
│  │  │  Discovery → Bootstrap → Validation →              │   │    │
│  │  │  Initialization → (Termination)                    │   │    │
│  │  └────────────────────────────────────────────────────┘   │    │
│  │                                                           │    │
│  │  ┌────────────────────────────────────────────────────┐   │    │
│  │  │  Security                                          │   │    │
│  │  │  Signature Verification → MANIFEST.txt → PGP       │   │    │
│  │  └────────────────────────────────────────────────────┘   │    │
│  └───────────────────────────────────────────────────────────┘    │
└─────────────────────────────────────────────────────────────────┘
```

### 핵심 설계 결정 요약

| 영역 | 선택 | 이유 |
|------|------|------|
| IPC | gRPC (HashiCorp go-plugin) | 언어 독립성, 타입 안전성, 스트리밍 |
| 프로세스 모델 | 별도 프로세스 (+ 컨테이너 옵션) | 격리, 안정성, 크래시 복구 |
| 미들웨어 | 15단계 체인 | 관심사 분리, 선택적 적용, 확장성 |
| 파이프라인 | 5단계 로드 파이프라인 | 단계별 검증, 실패 시 롤백 가능 |
| 보안 | PGP 서명 검증 | 공급망 보안, 무결성 보장 |
| 프론트엔드 | SDK 기반 (PanelPlugin, DataSourceApi) | 표준화된 API, 하위 호환성 |
| 상태 관리 | State Machine (5 states) | 명확한 라이프사이클, 동시성 안전 |
| 레지스트리 | In-memory map | 빠른 조회, 단순한 구현 |

이 플러그인 시스템은 Grafana가 "관측 가능성 플랫폼"으로 진화할 수 있게 만든 핵심 인프라다.
100개 이상의 공식 플러그인과 수천 개의 커뮤니티 플러그인이 이 시스템 위에서 동작하며,
gRPC 기반 프로세스 격리와 15단계 미들웨어 체인이 보안과 관측 가능성을 보장한다.
