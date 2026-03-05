# 07. Hubble CLI 아키텍처

## 목차

1. [개요](#1-개요)
2. [Cobra 커맨드 트리 구조](#2-cobra-커맨드-트리-구조)
3. [Root 커맨드 초기화](#3-root-커맨드-초기화)
4. [Viper 설정 시스템](#4-viper-설정-시스템)
5. [gRPC 연결 관리](#5-grpc-연결-관리)
6. [TLS 설정](#6-tls-설정)
7. [인터셉터 체계](#7-인터셉터-체계)
8. [Observe 커맨드 상세](#8-observe-커맨드-상세)
9. [플래그 파싱과 필터 디스패치](#9-플래그-파싱과-필터-디스패치)
10. [출력 포맷팅 시스템](#10-출력-포맷팅-시스템)
11. [K8s Port-Forward 지원](#11-k8s-port-forward-지원)
12. [보조 커맨드들](#12-보조-커맨드들)
13. [설정 우선순위와 환경변수](#13-설정-우선순위와-환경변수)
14. [버전 불일치 감지](#14-버전-불일치-감지)
15. [설계 분석](#15-설계-분석)

---

## 1. 개요

Hubble CLI는 Cilium이 관리하는 클러스터의 네트워크 플로우와 이벤트를 관찰하고 검사하기 위한 커맨드라인 도구이다. CLI는 **Cobra** 프레임워크로 커맨드 트리를 구성하고, **Viper**로 설정을 관리하며, **gRPC**를 통해 Hubble 서버(또는 Hubble Relay)와 통신한다.

### 핵심 설계 원칙

| 원칙 | 구현 방식 |
|------|----------|
| 모듈형 커맨드 | Cobra 서브커맨드로 각 기능 독립 패키지화 |
| 설정 유연성 | Viper를 통한 플래그/환경변수/설정파일 통합 |
| 연결 추상화 | conn 패키지로 gRPC 연결 로직 캡슐화 |
| 확장 가능 필터 | filterDispatch 패턴으로 동적 필터 디스패치 |
| 오프라인 분석 | IOReaderObserver로 파일 기반 분석 지원 |

### 소스코드 위치

```
hubble/cmd/
├── root.go                      # 루트 커맨드 및 초기화
├── common/
│   ├── config/
│   │   ├── flags.go             # 글로벌/서버 플래그 정의
│   │   ├── viper.go             # Viper 인스턴스 생성
│   │   └── compat.go            # 호환성 설정
│   ├── conn/
│   │   ├── conn.go              # gRPC 연결 관리
│   │   ├── tls.go               # TLS 설정
│   │   ├── auth.go              # Basic Auth
│   │   └── version.go           # 버전 불일치 감지
│   ├── template/usage.go        # 사용법 템플릿
│   └── validate/                # 플래그 유효성 검증
├── observe/
│   ├── observe.go               # observe 커맨드 엔트리
│   ├── flows.go                 # flows 서브커맨드 (핵심)
│   ├── flows_filter.go          # 필터 플래그 → protobuf 변환
│   ├── agent_events.go          # agent-events 서브커맨드
│   ├── debug_events.go          # debug-events 서브커맨드
│   ├── events.go                # 이벤트 출력 핸들러
│   ├── io_reader_observer.go    # 파일 입력 옵저버
│   ├── identity.go              # 보안 ID 파싱
│   └── workload.go              # 워크로드 파싱
├── status/status.go             # status 커맨드
├── list/
│   ├── list.go                  # list 커맨드
│   ├── node.go                  # list nodes
│   └── namespaces.go            # list namespaces
├── watch/                       # watch 커맨드 (개발용)
├── record/record.go             # record 커맨드 (실험적)
├── config/                      # config 서브커맨드들
├── reflect/reflect.go           # gRPC reflection
└── version/version.go           # version 커맨드
```

---

## 2. Cobra 커맨드 트리 구조

Hubble CLI의 전체 커맨드 트리는 다음과 같은 계층 구조를 갖는다.

```
hubble (root)
├── observe                  # 플로우 관찰 (가장 많이 사용)
│   ├── flows               # 플로우 전용 (observe의 alias 성격)
│   ├── agent-events        # Cilium 에이전트 이벤트
│   └── debug-events        # 디버그 이벤트
├── status                   # 서버 상태 확인
├── list                     # 객체 목록 조회
│   ├── nodes               # 노드 목록
│   └── namespaces          # 네임스페이스 목록
├── watch                    # 실시간 감시 (숨김)
│   └── peer                # 피어 변경 감시
├── record                   # 패킷 캡처 (숨김/실험적)
├── config                   # 설정 관리
│   ├── get                 # 설정값 조회
│   ├── set                 # 설정값 변경
│   ├── reset               # 설정값 초기화
│   └── view                # 전체 설정 보기
├── reflect                  # gRPC API 탐색 (숨김)
└── version                  # 버전 표시
```

### 커맨드 등록 코드

`root.go`의 `NewWithViper` 함수에서 모든 서브커맨드를 등록한다:

```go
// 소스: hubble/cmd/root.go:96-105
rootCmd.AddCommand(
    cmdConfig.New(vp),
    list.New(vp),
    observe.New(vp),
    record.New(vp),
    reflect.New(vp),
    status.New(vp),
    version.New(),
    watch.New(vp),
)
```

모든 서브커맨드(version 제외)는 `vp *viper.Viper`를 인자로 받아 동일한 설정 인스턴스를 공유한다. 이는 글로벌 설정(서버 주소, TLS 등)이 모든 커맨드에서 일관되게 적용되도록 보장한다.

### Observe 서브커맨드 구성

```go
// 소스: hubble/cmd/observe/observe.go:170-181
func New(vp *viper.Viper) *cobra.Command {
    observeCmd := newObserveCmd(vp)
    flowsCmd := newFlowsCmd(vp)

    observeCmd.AddCommand(
        newAgentEventsCommand(vp),
        newDebugEventsCommand(vp),
        flowsCmd,
    )

    return observeCmd
}
```

`observe` 커맨드 자체도 플로우를 표시하고, `observe flows`는 더 상세한 예제와 설명을 포함한 전용 서브커맨드이다. 두 커맨드 모두 내부적으로 `newFlowsCmdHelper`를 사용하여 동일한 핵심 로직을 공유한다.

---

## 3. Root 커맨드 초기화

### 초기화 흐름

```
NewWithViper(vp) 호출
  │
  ├── defer template.Initialize()    ← 서브커맨드 추가 후 실행
  │
  ├── rootCmd 생성
  │   ├── SilenceErrors: true        ← main에서 에러 처리
  │   └── PersistentPreRunE          ← 모든 서브커맨드 실행 전
  │       ├── validate.Flags()       ← TLS/Auth 플래그 검증
  │       └── conn.Init(vp)          ← gRPC 옵션 초기화
  │
  ├── cobra.OnInitialize()           ← 설정 파일 로드
  │   ├── vp.SetConfigFile()         ← --config 플래그
  │   ├── vp.ReadInConfig()          ← 설정 파일 읽기
  │   ├── logger.Initialize()        ← 로거 초기화
  │   └── BasicAuth 설정             ← 사용자명/비밀번호
  │
  ├── PersistentFlags 등록
  │   ├── config.GlobalFlags         ← --config, --debug
  │   └── config.ServerFlags         ← --server, --tls, ...
  │
  └── 서브커맨드 추가
```

### PersistentPreRunE의 역할

```go
// 소스: hubble/cmd/root.go:48-53
PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
    if err := validate.Flags(cmd, vp); err != nil {
        return err
    }
    return conn.Init(vp)
},
```

이 함수는 모든 서브커맨드 실행 전에 호출되어:
1. **플래그 유효성 검증**: TLS 관련 플래그 조합이 올바른지 확인
2. **gRPC 연결 초기화**: 인터셉터, TLS, 타임아웃 등 gRPC 다이얼 옵션 구성

단, `config` 커맨드는 자체 `PersistentPreRunE`를 정의하여 이 검증을 건너뛴다. 잘못된 설정 상태에서도 설정을 수정/조회할 수 있어야 하기 때문이다.

```go
// 소스: hubble/cmd/config/config.go:36-41
PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
    // override root persistent pre-run to avoid flag/config checks
    // as we want to be able to modify/view the config even if it is
    // invalid
    return nil
},
```

---

## 4. Viper 설정 시스템

### Viper 인스턴스 생성

```go
// 소스: hubble/cmd/common/config/viper.go:15-36
func NewViper() *viper.Viper {
    vp := viper.New()

    // 설정 파일 검색
    vp.SetConfigName("config")       // 파일명: config.yaml
    vp.SetConfigType("yaml")
    vp.AddConfigPath(".")            // 현재 디렉토리
    if defaults.ConfigDir != "" {
        vp.AddConfigPath(defaults.ConfigDir)       // 기본 설정 디렉토리
    }
    if defaults.ConfigDirFallback != "" {
        vp.AddConfigPath(defaults.ConfigDirFallback)  // 폴백 디렉토리
    }

    // 환경변수 설정
    vp.SetEnvPrefix("hubble")                        // HUBBLE_ 접두사
    vp.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))  // - → _
    vp.AutomaticEnv()
    return vp
}
```

### 설정 키 목록

```go
// 소스: hubble/cmd/common/config/flags.go:14-36
const (
    // 글로벌 플래그
    KeyConfig = "config"  // 설정 파일 경로
    KeyDebug  = "debug"   // 디버그 모드

    // 서버 연결 플래그
    KeyServer            = "server"               // 서버 주소
    KeyTLS               = "tls"                  // TLS 활성화
    KeyTLSAllowInsecure  = "tls-allow-insecure"   // 인증서 검증 건너뛰기
    KeyTLSCACertFiles    = "tls-ca-cert-files"    // CA 인증서 파일
    KeyTLSClientCertFile = "tls-client-cert-file" // 클라이언트 인증서
    KeyTLSClientKeyFile  = "tls-client-key-file"  // 클라이언트 키
    KeyTLSServerName     = "tls-server-name"      // TLS 서버명
    KeyBasicAuthUsername = "basic-auth-username"   // Basic Auth 사용자
    KeyBasicAuthPassword = "basic-auth-password"   // Basic Auth 비밀번호
    KeyTimeout           = "timeout"              // 연결 타임아웃
    KeyRequestTimeout    = "request-timeout"      // 요청 타임아웃
    KeyPortForward       = "port-forward"         // 포트 포워딩 활성화
    KeyPortForwardPort   = "port-forward-port"    // 포트 포워딩 로컬 포트
    KeyKubeContext       = "kube-context"          // K8s 컨텍스트
    KeyKubeNamespace     = "kube-namespace"        // K8s 네임스페이스
    KeyKubeconfig        = "kubeconfig"            // kubeconfig 경로
)
```

### 설정 우선순위

Viper의 설정 우선순위는 다음과 같다 (높은 것이 우선):

```
1. 커맨드라인 플래그          (--server localhost:4245)
2. 환경변수                  (HUBBLE_SERVER=localhost:4245)
3. 설정 파일                 (config.yaml의 server: localhost:4245)
4. 기본값                    (defaults.ServerAddress)
```

환경변수 매핑 규칙:
- 접두사: `HUBBLE_`
- 하이픈은 밑줄로: `tls-server-name` → `HUBBLE_TLS_SERVER_NAME`

---

## 5. gRPC 연결 관리

### 연결 초기화

`conn` 패키지는 gRPC 연결 생성을 담당한다. `GRPCOptionFuncs` 슬라이스에 옵션 생성 함수를 등록하는 패턴을 사용한다.

```go
// 소스: hubble/cmd/common/conn/conn.go:27-38
type GRPCOptionFunc func(vp *viper.Viper) (grpc.DialOption, error)

var GRPCOptionFuncs []GRPCOptionFunc

func init() {
    GRPCOptionFuncs = append(
        GRPCOptionFuncs,
        grpcUnaryInterceptors,    // 단항 RPC 인터셉터
        grpcStreamInterceptors,   // 스트리밍 RPC 인터셉터
        grpcOptionTLS,            // TLS 설정
    )
}
```

### Init 함수

```go
// 소스: hubble/cmd/common/conn/conn.go:103-112
func Init(vp *viper.Viper) error {
    for _, fn := range GRPCOptionFuncs {
        dialOpt, err := fn(vp)
        if err != nil {
            return err
        }
        grpcDialOptions = append(grpcDialOptions, dialOpt)
    }
    return nil
}
```

### 연결 생성

```go
// 소스: hubble/cmd/common/conn/conn.go:115-122
func New(target string) (*grpc.ClientConn, error) {
    t := strings.TrimPrefix(target, defaults.TargetTLSPrefix)
    conn, err := grpc.NewClient(t, grpcDialOptions...)
    if err != nil {
        return nil, fmt.Errorf("failed to create gRPC client to '%s': %w", target, err)
    }
    return conn, nil
}
```

### 연결 흐름 다이어그램

```
hubble observe --server relay.example.com:4245
    │
    ▼
conn.Init(vp)
    │
    ├── grpcUnaryInterceptors(vp)     → timeout + version check
    ├── grpcStreamInterceptors(vp)    → version check (비동기)
    └── grpcOptionTLS(vp)             → TLS credentials
    │
    ▼
conn.NewWithFlags(ctx, vp)
    │
    ├── --port-forward 활성화?
    │   ├── Yes → K8s port-forward → 127.0.0.1:{local_port}
    │   └── No  → vp.GetString("server")
    │
    └── conn.New(server)
        └── grpc.NewClient(target, grpcDialOptions...)
```

---

## 6. TLS 설정

TLS 설정은 `conn/tls.go`에서 처리한다. 서버 주소가 `tls://` 접두사를 갖거나 `--tls` 플래그가 활성화된 경우 TLS를 사용한다.

```go
// 소스: hubble/cmd/common/conn/tls.go:23-65
func grpcOptionTLS(vp *viper.Viper) (grpc.DialOption, error) {
    target := vp.GetString(config.KeyServer)
    if !(vp.GetBool(config.KeyTLS) || strings.HasPrefix(target, defaults.TargetTLSPrefix)) {
        return grpc.WithTransportCredentials(insecure.NewCredentials()), nil
    }

    tlsConfig := tls.Config{
        InsecureSkipVerify: vp.GetBool(config.KeyTLSAllowInsecure),
        ServerName:         vp.GetString(config.KeyTLSServerName),
    }

    // 커스텀 CA 인증서 (선택적)
    caFiles := vp.GetStringSlice(config.KeyTLSCACertFiles)
    if len(caFiles) > 0 {
        ca := x509.NewCertPool()
        for _, path := range caFiles {
            certPEM, err := os.ReadFile(filepath.Clean(path))
            // ... CA 인증서 추가
        }
        tlsConfig.RootCAs = ca
    }

    // mTLS (선택적)
    clientCertFile := vp.GetString(config.KeyTLSClientCertFile)
    clientKeyFile := vp.GetString(config.KeyTLSClientKeyFile)
    if clientCertFile != "" && clientKeyFile != "" {
        c, err := tls.LoadX509KeyPair(clientCertFile, clientKeyFile)
        tlsConfig.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
            return &c, nil
        }
    }

    return grpc.WithTransportCredentials(credentials.NewTLS(&tlsConfig)), nil
}
```

### TLS 모드 결정 로직

```
서버 주소가 tls:// 접두사? ──Yes──▶ TLS 활성화
         │
         No
         │
--tls 플래그 활성화? ──Yes──▶ TLS 활성화
         │
         No
         │
         ▼
    Insecure 모드 (기본)
```

### TLS 설정 옵션 조합

| 시나리오 | 플래그 조합 |
|----------|-----------|
| 비암호화 연결 | (기본값) |
| 서버 인증서 검증 | `--tls` |
| 커스텀 CA | `--tls --tls-ca-cert-files ca.pem` |
| 인증서 검증 건너뛰기 | `--tls --tls-allow-insecure` |
| mTLS | `--tls --tls-client-cert-file client.pem --tls-client-key-file key.pem` |
| TLS 서버명 지정 | `--tls --tls-server-name relay.example.com` |

---

## 7. 인터셉터 체계

### Unary 인터셉터

단항 RPC(ServerStatus, GetNodes 등)에 적용되는 인터셉터 체인:

```go
// 소스: hubble/cmd/common/conn/conn.go:41-47
func grpcUnaryInterceptors(vp *viper.Viper) (grpc.DialOption, error) {
    option := grpc.WithChainUnaryInterceptor(
        timeout.UnaryClientInterceptor(vp.GetDuration(config.KeyRequestTimeout)),
        onReceiveHeaderUnaryInterceptor(logger.Logger, logVersionMismatch()),
    )
    return option, nil
}
```

1. **timeout 인터셉터**: `--request-timeout` 값을 사용하여 단항 RPC에 타임아웃 적용
2. **버전 검증 인터셉터**: 응답 헤더에서 서버 버전을 추출하여 CLI 버전과 비교

### Stream 인터셉터

스트리밍 RPC(GetFlows, GetAgentEvents 등)에 적용되는 인터셉터:

```go
// 소스: hubble/cmd/common/conn/conn.go:49-54
func grpcStreamInterceptors(vp *viper.Viper) (grpc.DialOption, error) {
    option := grpc.WithChainStreamInterceptor(
        onReceiveHeaderStreamInterceptor(logger.Logger, logVersionMismatch()),
    )
    return option, nil
}
```

스트리밍 인터셉터의 헤더 추출은 **고루틴**에서 비동기적으로 수행된다. 이는 `stream.Header()`가 메타데이터가 준비될 때까지 블록하기 때문에, 데드락을 방지하기 위함이다.

```go
// 소스: hubble/cmd/common/conn/conn.go:74-97
func onReceiveHeaderStreamInterceptor(log *slog.Logger, fn onReceiveHeader) grpc.StreamClientInterceptor {
    return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn,
        method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
        stream, err := streamer(ctx, desc, cc, method, opts...)
        if err != nil {
            return nil, err
        }
        // 데드락 방지를 위해 고루틴에서 헤더 추출
        go func() {
            header, err := stream.Header()
            if err != nil {
                log.Warn("Failed to obtain grpc stream headers...")
                return
            }
            fn(log, header)
        }()
        return stream, nil
    }
}
```

### 인터셉터 실행 순서

```
[Client Request]
    │
    ▼
Timeout Interceptor      ← 단항 RPC만 적용
    │
    ▼
Version Mismatch Check   ← 모든 RPC에 적용
    │
    ▼
[Server Response]
```

---

## 8. Observe 커맨드 상세

### 명령 실행 흐름

`hubble observe` 명령의 전체 실행 흐름:

```
hubble observe [filters] [flags]
    │
    ├── PreRunE: vp.BindPFlags(rawFilterFlags)  ← allowlist/denylist 바인딩
    │
    └── RunE:
        ├── handleFlowArgs()                     ← Printer 초기화
        ├── getFlowsRequest()                    ← GetFlowsRequest 구성
        │   ├── since/until 타임스탬프 변환
        │   ├── first/last/all/follow 처리
        │   ├── whitelist/blacklist 필터 수집
        │   └── field mask 설정 (experimental)
        ├── GetHubbleClientFunc()                ← gRPC 클라이언트 획득
        │   ├── --input-file → IOReaderObserver
        │   └── 기본 → gRPC 서버 연결
        └── getFlows()                           ← 스트림 수신 및 출력
            ├── client.GetFlows(ctx, req)
            └── for { b.Recv() → printer.WriteGetFlowsResponse() }
```

### 셀렉터 옵션

```go
// 소스: hubble/cmd/observe/observe.go:21-27
var selectorOpts struct {
    all          bool    // --all: 버퍼의 모든 플로우
    last         uint64  // --last N: 마지막 N개 (기본값: 20)
    since, until string  // --since/--until: 시간 범위
    follow       bool    // --follow: 실시간 추적
    first        uint64  // --first N: 처음 N개
}
```

### 요청 구성 로직

```go
// 소스: hubble/cmd/observe/flows.go:743-848
func getFlowsRequest(ofilter *flowFilter, allowlist []string, denylist []string) (*observerpb.GetFlowsRequest, error) {
    // 상호 배타적 옵션 검증
    // first + last, first + all, first + follow, last + all 조합 불가

    // since/until 타임스탬프 파싱
    // follow 모드에서는 until 무시

    // 기본값 처리
    if since == nil && until == nil && !first {
        switch {
        case selectorOpts.all:
            selectorOpts.last = ^uint64(0)  // uint64 최대값
        case selectorOpts.last == 0 && !selectorOpts.follow && otherOpts.inputFile == "":
            selectorOpts.last = defaults.FlowPrintCount  // 기본 20개
        }
    }

    // GetFlowsRequest 구성
    req := &observerpb.GetFlowsRequest{
        Number:    number,
        Follow:    selectorOpts.follow,
        Whitelist: wl,     // 허용 필터
        Blacklist: bl,     // 차단 필터
        Since:     since,
        Until:     until,
        First:     first,
    }
    // ...
}
```

### 플로우 수신 루프

```go
// 소스: hubble/cmd/observe/flows.go:850-874
func getFlows(ctx context.Context, client observerpb.ObserverClient,
    req *observerpb.GetFlowsRequest) error {
    b, err := client.GetFlows(ctx, req)
    if err != nil {
        return err
    }
    defer printer.Close()

    for {
        getFlowResponse, err := b.Recv()
        switch {
        case errors.Is(err, io.EOF), errors.Is(err, context.Canceled):
            return nil
        case err == nil:
        default:
            if status.Code(err) == codes.Canceled {
                return nil
            }
            return err
        }
        if err = printer.WriteGetFlowsResponse(getFlowResponse); err != nil {
            return err
        }
    }
}
```

---

## 9. 플래그 파싱과 필터 디스패치

### flowFilter 구조

Hubble의 필터 시스템은 복잡한 플래그 조합을 protobuf `FlowFilter`로 변환한다.

```go
// 소스: hubble/cmd/observe/flows_filter.go:167-178
type flowFilter struct {
    whitelist *filterTracker  // 허용 필터
    blacklist *filterTracker  // 차단 필터

    // --not 플래그로 다음 필터를 blacklist에 추가
    blacklisting bool

    conflicts [][]string  // 충돌 규칙
}
```

### filterTracker의 이중 필터 패턴

`filterTracker`는 `left`와 `right` 두 개의 `FlowFilter`를 유지한다. 이는 양방향 필터(예: `--pod`, `--ip`)를 처리하기 위함이다.

```go
// 소스: hubble/cmd/observe/flows_filter.go:28-39
type filterTracker struct {
    left, right *flowpb.FlowFilter  // OR 관계

    ns, srcNs, dstNs namespaceModifier  // 네임스페이스 수정자
    changed []string  // 사용자가 변경한 플래그 추적
}
```

예를 들어 `--pod myapp`은 다음과 같이 변환된다:
- `left.SourcePod = ["myapp"]`
- `right.DestinationPod = ["myapp"]`
- 결과: source가 myapp이거나 destination이 myapp인 플로우 (OR)

반면 `--from-pod myapp`은 방향이 고정되어:
- `left.SourcePod = ["myapp"]` AND `right.SourcePod = ["myapp"]`

### filterDispatch 패턴

Cobra는 `Set()` 호출 시 플래그 이름을 전달하지 않는다. Hubble은 `filterDispatch`로 이를 해결한다:

```go
// 소스: hubble/cmd/observe/flows_filter.go:754-759
type filterDispatch struct {
    *flowFilter
    name string    // 플래그 이름 보존
    def  []string  // 기본값
}

func (d filterDispatch) Set(s string) error {
    return d.flowFilter.Set(d.name, s, true)
}
```

### 필터 등록 예시

```go
// 소스: hubble/cmd/observe/flows.go:434-445
filterFlags.Var(filterVar(
    "from-ip", ofilter,
    "Show all flows originating at the given IP address..."))
filterFlags.Var(filterVar(
    "ip", ofilter,
    "Show all flows originating or terminating at the given IP address..."))
filterFlags.Var(filterVar(
    "to-ip", ofilter,
    "Show all flows terminating at the given IP address..."))
```

### --not 플래그 동작

```
hubble observe --not --from-pod foo --not --to-port 80

실행 순서:
1. --not → blacklisting = true
2. --from-pod foo → blacklist에 SourcePod: ["foo"] 추가, blacklisting = false
3. --not → blacklisting = true
4. --to-port 80 → blacklist에 DestinationPort: ["80"] 추가, blacklisting = false

결과: source가 foo가 아니고, destination port가 80이 아닌 플로우만 표시
```

### 충돌 감지

```go
// 소스: hubble/cmd/observe/flows_filter.go:182-217 (일부)
conflicts: [][]string{
    {"from-fqdn", "from-ip", "ip", "fqdn", "from-namespace", "namespace", ...},
    {"to-fqdn", "to-ip", "ip", "fqdn", "to-namespace", "namespace", ...},
    {"label", "from-label"},
    {"label", "to-label"},
    {"service", "from-service"},
    // ...
}
```

같은 그룹 내의 플래그를 동시에 사용하면 에러가 발생한다. 예: `--ip`와 `--from-ip`는 충돌한다.

---

## 10. 출력 포맷팅 시스템

### 포맷팅 옵션

```go
// 소스: hubble/cmd/observe/observe.go:29-39
var formattingOpts struct {
    output string              // compact, dict, json, jsonpb, table

    timeFormat string          // StampMilli, RFC3339, ...

    enableIPTranslation bool   // IP → 논리적 이름 변환
    nodeName            bool   // 노드 이름 표시
    policyNames         bool   // 정책 이름 표시
    numeric             bool   // 모든 값을 숫자로 표시
    color               string // auto, always, never
}
```

### Printer 초기화

```go
// 소스: hubble/cmd/observe/flows.go:659-722
func handleFlowArgs(writer io.Writer, ofilter *flowFilter, debug bool) (err error) {
    var opts = []hubprinter.Option{
        hubprinter.Writer(writer),
        hubprinter.WithTimeFormat(hubtime.FormatNameToLayout(formattingOpts.timeFormat)),
        hubprinter.WithColor(formattingOpts.color),
    }

    switch formattingOpts.output {
    case "compact":
        opts = append(opts, hubprinter.Compact())
    case "dict":
        opts = append(opts, hubprinter.Dict())
    case "json", "JSON":
        // Legacy JSON 호환성 처리
        if config.Compat.LegacyJSONOutput {
            opts = append(opts, hubprinter.JSONLegacy())
            break
        }
        fallthrough
    case "jsonpb":
        opts = append(opts, hubprinter.JSONPB())
    case "tab", "table":
        if selectorOpts.follow {
            return fmt.Errorf("table output format is not compatible with follow mode")
        }
        opts = append(opts, hubprinter.Tab())
    }
    // ...
    printer = hubprinter.New(opts...)
}
```

### 출력 포맷 비교

| 포맷 | 용도 | follow 호환 |
|------|------|-------------|
| compact | 기본, 한 줄 요약 | O |
| dict | KEY:VALUE 쌍 | O |
| json/jsonpb | Proto3 JSON | O |
| table | 탭 정렬 테이블 | X |

---

## 11. K8s Port-Forward 지원

`--port-forward` (`-P`) 플래그를 사용하면 자동으로 hubble-relay 파드로 포트 포워딩을 설정한다.

```go
// 소스: hubble/cmd/common/conn/conn.go:126-156
func NewWithFlags(ctx context.Context, vp *viper.Viper) (*grpc.ClientConn, error) {
    server := vp.GetString(config.KeyServer)

    if vp.GetBool(config.KeyPortForward) {
        kubeContext := vp.GetString(config.KeyKubeContext)
        kubeconfig := vp.GetString(config.KeyKubeconfig)
        kubeNamespace := vp.GetString(config.KeyKubeNamespace)
        localPort := vp.GetUint16(config.KeyPortForwardPort)

        pf, err := newPortForwarder(kubeContext, kubeconfig)
        if err != nil {
            return nil, fmt.Errorf("failed to create k8s port forwader: %w", err)
        }

        // hubble-relay 서비스의 첫 번째 포트로 포워딩
        res, err := pf.PortForwardService(ctx, kubeNamespace, "hubble-relay",
            int32(localPort), 0)
        if err != nil {
            return nil, fmt.Errorf("failed to port forward: %w", err)
        }

        server = fmt.Sprintf("127.0.0.1:%d", res.ForwardedPort.Local)
    }

    return New(server)
}
```

### 포트 포워딩 흐름

```
hubble observe -P
    │
    ├── K8s clientset 생성
    │   ├── --kube-context (기본: 현재 컨텍스트)
    │   └── --kubeconfig (기본: ~/.kube/config)
    │
    ├── PortForwardService 호출
    │   ├── namespace: --kube-namespace (기본: kube-system)
    │   ├── service: "hubble-relay"
    │   └── localPort: --port-forward-port (기본: 4245, 0=랜덤)
    │
    └── 127.0.0.1:{localPort}로 gRPC 연결
```

---

## 12. 보조 커맨드들

### status 커맨드

서버 연결 상태와 Hubble 서버 상태를 확인한다.

```go
// 소스: hubble/cmd/status/status.go:77-116
func runStatus(ctx context.Context, out io.Writer, conn *grpc.ClientConn) error {
    // 1단계: gRPC 헬스체크
    healthy, status, err := getHC(ctx, conn)
    // ...
    if !healthy {
        return errors.New("not healthy")
    }

    // 2단계: Hubble 서버 상태 조회
    ss, err := getStatus(ctx, conn)
    // ServerStatusResponse: Version, MaxFlows, NumFlows, SeenFlows, UptimeNs, FlowsRate
    // ...
}
```

### list 커맨드

노드와 네임스페이스 목록을 조회한다.

```go
// 소스: hubble/cmd/list/list.go:23-37
func New(vp *viper.Viper) *cobra.Command {
    listCmd := &cobra.Command{
        Use:   "list",
        Short: "List Hubble objects",
    }
    listCmd.AddCommand(
        newNodeCommand(vp),       // hubble list nodes
        newNamespacesCommand(vp),  // hubble list namespaces
    )
    return listCmd
}
```

### record 커맨드

실험적인 패킷 캡처 기능이다. 필터로 지정한 트래픽을 pcap 파일로 저장한다.

```go
// 소스: hubble/cmd/record/record.go:42-87
func New(vp *viper.Viper) *cobra.Command {
    recordCmd := &cobra.Command{
        Use:     "record [flags] filter1 filter2 ... filterN",
        Short:   "Capture and record network packets",
        Hidden:  true,  // 실험적 기능
        RunE: func(cmd *cobra.Command, args []string) error {
            filters, err := parseFilters(args)
            // ...
            return runRecord(ctx, hubbleConn, filters)
        },
    }
    // ...
}
```

필터 형식: `"srcPrefix srcPort dstPrefix dstPort proto"`
- 예: `"192.168.1.0/24 0 10.0.0.0/16 80 TCP"`

### reflect 커맨드

gRPC Reflection을 사용하여 서버의 API 스키마를 탐색한다.

```go
// 소스: hubble/cmd/reflect/reflect.go:24-46
func New(vp *viper.Viper) *cobra.Command {
    reflectCmd := &cobra.Command{
        Use:    "reflect",
        Short:  "Use gRPC reflection to explore Hubble's API",
        Hidden: true,  // 개발/디버깅 전용
        // ...
    }
}
```

### config 커맨드

설정 파일을 관리하는 4개의 서브커맨드를 제공한다.

```go
// 소스: hubble/cmd/config/config.go:47-53
configCmd.AddCommand(
    newGetCommand(vp),    // hubble config get <key>
    newResetCommand(vp),  // hubble config reset <key>
    newSetCommand(vp),    // hubble config set <key> <value>
    newViewCommand(vp),   // hubble config view
)
```

### 커맨드 가시성

| 커맨드 | 상태 | 이유 |
|--------|------|------|
| observe | 공개 | 핵심 기능 |
| status | 공개 | 기본 헬스체크 |
| list | 공개 | 노드/네임스페이스 조회 |
| config | 공개 | 설정 관리 |
| version | 공개 | 버전 확인 |
| watch | **숨김** | 개발/디버깅 전용 |
| record | **숨김** | 실험적 기능 |
| reflect | **숨김** | 개발/디버깅 전용 |

---

## 13. 설정 우선순위와 환경변수

### 전체 우선순위 체계

```
┌─────────────────────────────────────────────────────┐
│ 1. 커맨드라인 플래그 (최우선)                          │
│    hubble observe --server localhost:4245            │
├─────────────────────────────────────────────────────┤
│ 2. 환경변수                                          │
│    HUBBLE_SERVER=localhost:4245                      │
├─────────────────────────────────────────────────────┤
│ 3. 설정 파일                                         │
│    ~/.config/hubble/config.yaml                     │
│    server: localhost:4245                           │
├─────────────────────────────────────────────────────┤
│ 4. 기본값                                            │
│    localhost:4245                                    │
└─────────────────────────────────────────────────────┘
```

### 설정 파일 검색 경로

Viper는 다음 경로에서 `config.yaml`을 순서대로 검색한다:

1. 현재 작업 디렉토리 (`./config.yaml`)
2. 기본 설정 디렉토리 (`defaults.ConfigDir`)
3. 폴백 디렉토리 (`defaults.ConfigDirFallback`)
4. `--config` 플래그로 지정한 경로

### 환경변수 매핑 예시

| 플래그 | 환경변수 |
|--------|---------|
| `--server` | `HUBBLE_SERVER` |
| `--tls` | `HUBBLE_TLS` |
| `--tls-allow-insecure` | `HUBBLE_TLS_ALLOW_INSECURE` |
| `--tls-ca-cert-files` | `HUBBLE_TLS_CA_CERT_FILES` |
| `--port-forward` | `HUBBLE_PORT_FORWARD` |
| `--kube-context` | `HUBBLE_KUBE_CONTEXT` |
| `--debug` | `HUBBLE_DEBUG` |

### Basic Auth 설정

설정 파일이나 환경변수로 Basic Auth를 구성할 수 있다:

```go
// 소스: hubble/cmd/root.go:68-75
username := vp.GetString(config.KeyBasicAuthUsername)
password := vp.GetString(config.KeyBasicAuthPassword)
if username != "" && password != "" {
    optFunc := func(*viper.Viper) (grpc.DialOption, error) {
        return conn.WithBasicAuth(username, password), nil
    }
    conn.GRPCOptionFuncs = append(conn.GRPCOptionFuncs, optFunc)
}
```

---

## 14. 버전 불일치 감지

CLI와 서버 버전이 불일치하면 경고를 출력한다.

```go
// 소스: hubble/cmd/common/conn/version.go:31-56
func logVersionMismatch() onReceiveHeader {
    return func(log *slog.Logger, header metadata.MD) {
        relayVersion, err := parseVersionFromHeader(header,
            relaydefaults.GRPCMetadataRelayVersionKey)
        serverVersion, err := parseVersionFromHeader(header,
            serverdefaults.GRPCMetadataServerVersionKey)

        if cliVersionComparator.IsLowerThan(relayVersion) {
            log.Warn("Hubble CLI version is lower than Hubble Relay...")
        }

        if cliVersionComparator.IsLowerThan(serverVersion) {
            log.Warn("Hubble CLI version is lower than Hubble Server...")
        }
    }
}
```

### 버전 비교 로직

Minor 버전까지만 비교하고 Patch/Pre/Build는 무시한다:

```go
// 소스: hubble/cmd/common/conn/version.go:58-69
type minorVersionComparator struct {
    version semver.Version
}

func (c *minorVersionComparator) IsLowerThan(v semver.Version) bool {
    versionMissing := v.EQ(zeroVersion)
    return !versionMissing && c.version.LT(versionTruncateMinor(v))
}

func versionTruncateMinor(v semver.Version) semver.Version {
    minorV := v
    minorV.Patch = 0
    minorV.Pre = nil
    minorV.Build = nil
    return minorV
}
```

### 버전 검사 시나리오

| CLI 버전 | 서버 버전 | 결과 |
|----------|----------|------|
| 1.16.0 | 1.16.3 | 정상 (Patch 무시) |
| 1.16.0 | 1.17.0 | **경고** (Minor 불일치) |
| 1.17.0 | 1.16.0 | 정상 (CLI가 높음) |
| 1.16.0 | (없음) | 정상 (버전 미보고) |

---

## 15. 설계 분석

### 왜 Viper를 커맨드마다 공유하는가?

모든 서브커맨드가 동일한 Viper 인스턴스를 공유함으로써:
- **서버 연결 설정이 일관되게 적용**: `--server`, `--tls` 등의 설정이 어떤 서브커맨드에서든 동일하게 작동
- **설정 파일 한 번만 로드**: 초기화 시 한 번만 설정 파일을 읽고 전체 커맨드에 적용
- **환경변수 오버라이드가 자연스럽게 동작**: `HUBBLE_SERVER` 한 번 설정하면 모든 커맨드에 적용

### 왜 filterDispatch 패턴을 사용하는가?

Cobra의 `pflag.Value` 인터페이스는 `Set(string)` 시그니처만 제공하여 어떤 플래그에서 호출되었는지 알 수 없다. `filterDispatch`는 플래그 이름을 클로저에 캡처하여 이 제한을 우회한다. 이 패턴 덕분에 40개 이상의 필터 플래그를 하나의 `flowFilter.Set()` 메서드로 통합 처리할 수 있다.

### 왜 left/right 이중 필터를 사용하는가?

`--pod myapp`처럼 방향이 없는 필터는 "source가 myapp이거나 destination이 myapp"이라는 OR 조건을 의미한다. 이를 두 개의 `FlowFilter`로 분리하면 protobuf API의 `Whitelist` 배열에서 자연스럽게 OR 연산이 된다. `proto.Equal`로 두 필터가 동일한 경우 하나만 반환하여 불필요한 중복을 방지한다.

### 왜 스트리밍 인터셉터에서 고루틴을 사용하는가?

`stream.Header()`는 서버가 헤더 메타데이터를 전송하거나 스트림이 닫힐 때까지 블록한다. 만약 서버가 헤더를 명시적으로 전송하지 않으면 첫 번째 데이터 읽기까지 블록될 수 있다. 고루틴으로 분리하면 스트림 생성 자체는 즉시 반환되고, 헤더 메타데이터 처리는 백그라운드에서 수행된다.

### IOReaderObserver의 존재 이유

`--input-file` 옵션은 이전에 `hubble observe -o jsonpb`로 저장한 플로우를 오프라인으로 분석할 수 있게 한다. `IOReaderObserver`는 `observerpb.ObserverClient` 인터페이스를 구현하여, 파일에서 읽는 것이든 gRPC 서버에서 읽는 것이든 동일한 코드 경로로 처리할 수 있다.

```
# 플로우 저장
hubble observe -o jsonpb --last 1000 > flows.json

# 오프라인 분석
cat flows.json | hubble observe --input-file - --from-pod myapp
```

이 설계는 재현 가능한 디버깅과 자동화된 분석 파이프라인을 가능하게 한다.
