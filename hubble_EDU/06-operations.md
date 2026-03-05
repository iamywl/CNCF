# 06. Hubble 운영 가이드

## 목차
1. [개요](#1-개요)
2. [Hubble CLI 설치](#2-hubble-cli-설치)
3. [설정 파일](#3-설정-파일)
4. [gRPC 연결 옵션](#4-grpc-연결-옵션)
5. [TLS 설정](#5-tls-설정)
6. [Kubernetes Port-Forward](#6-kubernetes-port-forward)
7. [Hubble Relay 배포](#7-hubble-relay-배포)
8. [Prometheus 메트릭 모니터링](#8-prometheus-메트릭-모니터링)
9. [CLI 주요 명령어](#9-cli-주요-명령어)
10. [트러블슈팅](#10-트러블슈팅)
11. [운영 베스트 프랙티스](#11-운영-베스트-프랙티스)
12. [빌드 및 릴리스](#12-빌드-및-릴리스)

---

## 1. 개요

Hubble은 Cilium의 네트워크 관찰 플랫폼으로, Kubernetes 클러스터의 네트워크 트래픽을
실시간으로 관찰할 수 있는 도구이다. 운영 관점에서 Hubble은 세 가지 핵심 컴포넌트로
구성된다:

```
┌─────────────────────────────────────────────────────────┐
│                     Hubble 운영 구성요소                    │
├─────────────────────────────────────────────────────────┤
│                                                         │
│  ┌──────────┐    ┌──────────────┐    ┌──────────────┐  │
│  │ Hubble   │    │ Hubble       │    │ Hubble       │  │
│  │ CLI      │───>│ Relay        │───>│ Server       │  │
│  │          │    │ (Aggregator) │    │ (per-Node)   │  │
│  └──────────┘    └──────────────┘    └──────────────┘  │
│       │                                    │           │
│       │          ┌──────────────┐          │           │
│       └─────────>│ Hubble UI    │<─────────┘           │
│                  └──────────────┘                       │
│                                                         │
└─────────────────────────────────────────────────────────┘
```

- **Hubble Server**: 각 Cilium 에이전트 내부에 임베디드되어 실행. eBPF 이벤트를 수집하여 Flow로 변환
- **Hubble Relay**: 클러스터 내 모든 Hubble Server의 흐름을 집계하는 중앙 프록시
- **Hubble CLI**: gRPC 클라이언트로서 Relay 또는 Server에 직접 연결하여 Flow 관찰

### 기본 기본값 (defaults.go)

소스 경로: `hubble/pkg/defaults/defaults.go`

```go
const (
    // ServerAddress는 기본 서버 주소
    ServerAddress = "localhost:4245"

    // DialTimeout는 서버 연결 기본 타임아웃
    DialTimeout = 5 * time.Second

    // RequestTimeout는 클라이언트 요청 기본 타임아웃
    RequestTimeout = 12 * time.Second

    // FlowPrintCount는 hubble observe에서 기본 출력 Flow 수
    FlowPrintCount = 20

    // EventsPrintCount는 hubble events에서 기본 출력 이벤트 수
    EventsPrintCount = 20

    // TargetTLSPrefix는 TLS 연결을 나타내는 스키마
    TargetTLSPrefix = "tls://"
)
```

이 값들은 CLI에서 별도 옵션을 지정하지 않았을 때 사용되는 기본 설정이다.

---

## 2. Hubble CLI 설치

### 2.1 GitHub Release 설치

가장 직접적인 설치 방법은 GitHub Release에서 바이너리를 다운로드하는 것이다.

```bash
# 최신 버전 확인
HUBBLE_VERSION=$(curl -s https://raw.githubusercontent.com/cilium/hubble/main/stable.txt)

# Linux amd64
curl -L --remote-name-all \
  https://github.com/cilium/hubble/releases/download/${HUBBLE_VERSION}/hubble-linux-amd64.tar.gz \
  https://github.com/cilium/hubble/releases/download/${HUBBLE_VERSION}/hubble-linux-amd64.tar.gz.sha256sum

# 체크섬 검증
sha256sum --check hubble-linux-amd64.tar.gz.sha256sum

# 설치
tar xzvf hubble-linux-amd64.tar.gz
sudo install -m 0755 hubble /usr/local/bin/hubble
```

릴리스 바이너리는 다음 플랫폼을 지원한다 (Makefile `local-release` 타겟 기반):

| OS | 아키텍처 |
|-----|---------|
| darwin | amd64, arm64 |
| linux | amd64, arm64 |
| windows | amd64, arm64 |

### 2.2 Homebrew 설치 (macOS)

```bash
brew install hubble
```

Homebrew는 GitHub 릴리스의 tarball을 사용하여 빌드한다. 따라서 `.git` 디렉토리가
포함되지 않으며, `GIT_BRANCH`와 `GIT_HASH` 변수가 비어 있을 수 있다.

### 2.3 Helm 차트를 통한 설치 (Hubble Relay)

Cilium Helm 차트를 통해 Hubble Relay를 클러스터에 배포할 수 있다:

```bash
helm upgrade cilium cilium/cilium \
  --namespace kube-system \
  --reuse-values \
  --set hubble.enabled=true \
  --set hubble.relay.enabled=true \
  --set hubble.ui.enabled=true
```

### 2.4 소스에서 빌드

Makefile 경로: `hubble/Makefile`

```bash
# 기본 빌드
make hubble

# 설치 (/usr/local/bin에 복사)
make install

# 테스트
make test

# 벤치마크
make bench
```

빌드 시 다음 ldflags가 주입된다:

```makefile
GO_BUILD = CGO_ENABLED=0 $(GO) build $(GO_BUILD_FLAGS)

# 버전 정보 주입
-ldflags "-w -s \
  -X 'github.com/cilium/cilium/hubble/pkg.GitBranch=${GIT_BRANCH}' \
  -X 'github.com/cilium/cilium/hubble/pkg.GitHash=$(GIT_HASH)' \
  -X 'github.com/cilium/cilium/hubble/pkg.Version=${VERSION}'"
```

주요 특징:
- `CGO_ENABLED=0`: 정적 바이너리 생성
- `-w -s`: 디버그 정보 제거로 바이너리 크기 최소화
- 버전 정보: go.mod의 cilium 모듈 버전과 Git 정보 주입

### 2.5 Docker 이미지 빌드

```bash
# 이미지 빌드
make image

# 커스텀 레지스트리/태그
make image IMAGE_REPOSITORY=my-registry/hubble IMAGE_TAG=custom-tag
```

기본 이미지 레지스트리: `quay.io/cilium/hubble`

---

## 3. 설정 파일

### 3.1 설정 파일 위치

소스 경로: `hubble/pkg/defaults/defaults.go` - `init()` 함수

Hubble CLI는 두 가지 경로에서 설정 파일을 탐색한다:

```go
func init() {
    // 1순위: 사용자 설정 디렉토리 (XDG 표준)
    if dir, err := os.UserConfigDir(); err == nil {
        ConfigDir = filepath.Join(dir, "hubble")
    }
    // 2순위: 홈 디렉토리 폴백
    if dir, err := os.UserHomeDir(); err == nil {
        ConfigDirFallback = filepath.Join(dir, ".hubble")
    }

    switch {
    case ConfigDir != "":
        ConfigFile = filepath.Join(ConfigDir, "config.yaml")
    case ConfigDirFallback != "":
        ConfigFile = filepath.Join(ConfigDirFallback, "config.yaml")
    }
}
```

각 운영체제별 설정 파일 경로:

| OS | 1순위 경로 | 폴백 경로 |
|----|-----------|----------|
| Linux | `~/.config/hubble/config.yaml` | `~/.hubble/config.yaml` |
| macOS | `~/Library/Application Support/hubble/config.yaml` | `~/.hubble/config.yaml` |
| Windows | `%APPDATA%\hubble\config.yaml` | `%USERPROFILE%\.hubble\config.yaml` |

### 3.2 설정 파일 구조

Hubble CLI는 Viper 라이브러리를 사용하여 YAML 설정 파일을 로드한다. 설정 키는
CLI 플래그와 1:1로 매핑된다.

소스 경로: `hubble/cmd/common/config/flags.go`

```yaml
# ~/.config/hubble/config.yaml 예시

# 서버 연결 설정
server: "localhost:4245"
timeout: 5s
request-timeout: 12s

# TLS 설정
tls: false
tls-allow-insecure: false
tls-ca-cert-files: []
tls-client-cert-file: ""
tls-client-key-file: ""
tls-server-name: ""

# 인증 설정
basic-auth-username: ""
basic-auth-password: ""

# Kubernetes 포트 포워딩
port-forward: false
port-forward-port: 4245
kube-context: ""
kube-namespace: "kube-system"
kubeconfig: ""

# 기타
debug: false
```

### 3.3 설정 우선순위

Viper를 통해 다음 우선순위로 설정이 적용된다 (높은 것이 우선):

```
1. 명령행 플래그 (--server, --tls 등)
2. 환경 변수
3. 설정 파일 (config.yaml)
4. 기본값 (defaults.go)
```

### 3.4 전체 플래그 목록

소스 경로: `hubble/cmd/common/config/flags.go`

**글로벌 플래그:**

| 플래그 | 키 | 타입 | 기본값 | 설명 |
|--------|-----|------|--------|------|
| `--config` | `config` | string | `defaults.ConfigFile` | 설정 파일 경로 |
| `--debug`, `-D` | `debug` | bool | false | 디버그 메시지 활성화 |

**서버 플래그:**

| 플래그 | 키 | 타입 | 기본값 | 설명 |
|--------|-----|------|--------|------|
| `--server` | `server` | string | `localhost:4245` | Hubble 서버 주소 |
| `--timeout` | `timeout` | duration | 5s | 서버 연결 타임아웃 |
| `--request-timeout` | `request-timeout` | duration | 12s | 비스트리밍 RPC 타임아웃 |
| `--tls` | `tls` | bool | false | TLS 활성화 |
| `--tls-allow-insecure` | `tls-allow-insecure` | bool | false | 인증서 검증 건너뛰기 |
| `--tls-ca-cert-files` | `tls-ca-cert-files` | []string | nil | CA 인증서 파일 경로 |
| `--tls-client-cert-file` | `tls-client-cert-file` | string | "" | 클라이언트 인증서 |
| `--tls-client-key-file` | `tls-client-key-file` | string | "" | 클라이언트 키 |
| `--tls-server-name` | `tls-server-name` | string | "" | TLS 서버 이름 |
| `--basic-auth-username` | `basic-auth-username` | string | "" | Basic Auth 사용자 |
| `--basic-auth-password` | `basic-auth-password` | string | "" | Basic Auth 비밀번호 |
| `--port-forward`, `-P` | `port-forward` | bool | false | 자동 포트 포워딩 |
| `--port-forward-port` | `port-forward-port` | uint16 | 4245 | 로컬 포트 |
| `--kube-context` | `kube-context` | string | "" | K8s 컨텍스트 |
| `--kube-namespace` | `kube-namespace` | string | "kube-system" | Cilium 네임스페이스 |
| `--kubeconfig` | `kubeconfig` | string | "" | kubeconfig 경로 |

---

## 4. gRPC 연결 옵션

### 4.1 연결 초기화 구조

소스 경로: `hubble/cmd/common/conn/conn.go`

Hubble CLI의 gRPC 연결은 `GRPCOptionFunc` 슬라이스를 통해 초기화된다:

```go
// GRPCOptionFunc는 gRPC 다이얼 옵션을 구성하는 함수 타입
type GRPCOptionFunc func(vp *viper.Viper) (grpc.DialOption, error)

// GRPCOptionFuncs는 여러 gRPC 다이얼 옵션의 조합
var GRPCOptionFuncs []GRPCOptionFunc

func init() {
    GRPCOptionFuncs = append(
        GRPCOptionFuncs,
        grpcUnaryInterceptors,   // 단항 인터셉터
        grpcStreamInterceptors,  // 스트림 인터셉터
        grpcOptionTLS,           // TLS 설정
    )
}
```

초기화 순서:

```
                         Init(vp)
                            │
              ┌─────────────┼─────────────┐
              │             │             │
              v             v             v
   grpcUnaryInterceptors  grpcStream    grpcOptionTLS
              │          Interceptors        │
              │             │               │
              v             v               v
   ┌──────────────┐ ┌──────────────┐ ┌──────────────┐
   │  timeout     │ │  header      │ │  TLS/insecure│
   │  interceptor │ │  interceptor │ │  credentials │
   │  + header    │ │              │ │              │
   └──────────────┘ └──────────────┘ └──────────────┘
              │             │               │
              └─────────────┼───────────────┘
                            │
                     grpcDialOptions
                            │
                    New(target) / NewWithFlags()
                            │
                     grpc.NewClient()
```

### 4.2 인터셉터 체인

**Unary 인터셉터:**

```go
func grpcUnaryInterceptors(vp *viper.Viper) (grpc.DialOption, error) {
    option := grpc.WithChainUnaryInterceptor(
        // 1. 요청 타임아웃 인터셉터
        timeout.UnaryClientInterceptor(vp.GetDuration(config.KeyRequestTimeout)),
        // 2. 응답 헤더에서 버전 불일치 감지
        onReceiveHeaderUnaryInterceptor(logger.Logger, logVersionMismatch()),
    )
    return option, nil
}
```

**Stream 인터셉터:**

```go
func grpcStreamInterceptors(vp *viper.Viper) (grpc.DialOption, error) {
    option := grpc.WithChainStreamInterceptor(
        // 스트림 헤더에서 버전 불일치 감지
        onReceiveHeaderStreamInterceptor(logger.Logger, logVersionMismatch()),
    )
    return option, nil
}
```

스트림 인터셉터는 교착 상태 방지를 위해 헤더 추출을 별도 고루틴에서 수행한다:

```go
// stream.Header()는 메타데이터가 준비될 때까지 블로킹
// 교착 상태 방지를 위해 스트림 수명에 연결된 고루틴에서 수행
go func() {
    header, err := stream.Header()
    if err != nil {
        log.Warn("Failed to obtain grpc stream headers...")
        return
    }
    fn(log, header)
}()
```

### 4.3 연결 생성 함수

**기본 연결 (New):**

```go
func New(target string) (*grpc.ClientConn, error) {
    // "tls://" 접두사 제거
    t := strings.TrimPrefix(target, defaults.TargetTLSPrefix)
    conn, err := grpc.NewClient(t, grpcDialOptions...)
    if err != nil {
        return nil, fmt.Errorf("failed to create gRPC client to '%s': %w", target, err)
    }
    return conn, nil
}
```

**플래그 기반 연결 (NewWithFlags):**

```go
func NewWithFlags(ctx context.Context, vp *viper.Viper) (*grpc.ClientConn, error) {
    server := vp.GetString(config.KeyServer)

    // --port-forward 플래그가 설정된 경우 자동 포트 포워딩
    if vp.GetBool(config.KeyPortForward) {
        // K8s 포트 포워더 생성 후 hubble-relay 서비스에 연결
        res, err := pf.PortForwardService(ctx, kubeNamespace, "hubble-relay", ...)
        server = fmt.Sprintf("127.0.0.1:%d", res.ForwardedPort.Local)
    }

    conn, err := New(server)
    return conn, nil
}
```

---

## 5. TLS 설정

### 5.1 TLS 옵션 구성

소스 경로: `hubble/cmd/common/conn/tls.go`

TLS 설정은 `grpcOptionTLS` 함수에서 처리된다:

```go
func grpcOptionTLS(vp *viper.Viper) (grpc.DialOption, error) {
    target := vp.GetString(config.KeyServer)

    // TLS가 비활성화이고 서버 주소에 "tls://" 접두사가 없으면 insecure
    if !(vp.GetBool(config.KeyTLS) || strings.HasPrefix(target, defaults.TargetTLSPrefix)) {
        return grpc.WithTransportCredentials(insecure.NewCredentials()), nil
    }

    tlsConfig := tls.Config{
        InsecureSkipVerify: vp.GetBool(config.KeyTLSAllowInsecure),
        ServerName:         vp.GetString(config.KeyTLSServerName),
    }

    // 커스텀 CA 인증서 (선택)
    caFiles := vp.GetStringSlice(config.KeyTLSCACertFiles)
    if len(caFiles) > 0 {
        ca := x509.NewCertPool()
        for _, path := range caFiles {
            certPEM, err := os.ReadFile(filepath.Clean(path))
            ca.AppendCertsFromPEM(certPEM)
        }
        tlsConfig.RootCAs = ca
    }

    // mTLS 클라이언트 인증서 (선택)
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

### 5.2 TLS 구성 시나리오

```
┌─────────────────────────────────────────────────────────┐
│                    TLS 판단 로직                          │
├─────────────────────────────────────────────────────────┤
│                                                         │
│  --tls=false AND 서버 주소에 "tls://" 없음                │
│       → insecure.NewCredentials() (평문 통신)             │
│                                                         │
│  --tls=true OR 서버 주소가 "tls://"로 시작                │
│       ├── --tls-allow-insecure=true                     │
│       │   → TLS 사용하되 인증서 검증 건너뛰기               │
│       ├── --tls-ca-cert-files 지정                       │
│       │   → 커스텀 CA 풀 사용                             │
│       ├── --tls-client-cert-file + --tls-client-key-file│
│       │   → mTLS (상호 인증)                              │
│       └── 기본                                           │
│           → 시스템 CA 풀로 서버 인증서 검증                  │
│                                                         │
└─────────────────────────────────────────────────────────┘
```

### 5.3 TLS 사용 예시

```bash
# 기본 TLS (시스템 CA)
hubble observe --server tls://hubble-relay:4245

# 커스텀 CA
hubble observe \
  --tls \
  --tls-ca-cert-files /etc/hubble/ca.crt \
  --server hubble-relay:4245

# mTLS (상호 인증)
hubble observe \
  --tls \
  --tls-ca-cert-files /etc/hubble/ca.crt \
  --tls-client-cert-file /etc/hubble/client.crt \
  --tls-client-key-file /etc/hubble/client.key \
  --server hubble-relay:4245

# 서버 이름 지정 (SNI)
hubble observe \
  --tls \
  --tls-server-name instance.hubble-relay.cilium.io \
  --server hubble-relay:4245

# 인증서 검증 건너뛰기 (개발/테스트 전용, 비권장)
hubble observe \
  --tls \
  --tls-allow-insecure \
  --server hubble-relay:4245
```

---

## 6. Kubernetes Port-Forward

### 6.1 자동 포트 포워딩 메커니즘

소스 경로: `hubble/cmd/common/conn/conn.go` - `NewWithFlags()` 함수

`--port-forward` (`-P`) 플래그는 hubble-relay 파드에 대한 Kubernetes 포트 포워딩을
자동으로 설정한다:

```go
func NewWithFlags(ctx context.Context, vp *viper.Viper) (*grpc.ClientConn, error) {
    if vp.GetBool(config.KeyPortForward) {
        // 1. K8s REST 클라이언트 설정
        restClientGetter := genericclioptions.ConfigFlags{
            Context:    &kubeContext,
            KubeConfig: &kubeconfig,
        }

        // 2. 포트 포워더 생성
        pf := portforward.NewPortForwarder(clientset, config)

        // 3. hubble-relay 서비스에 포트 포워딩
        //    localPort=0이면 랜덤 포트 할당, svcPort=0이면 서비스 첫 번째 포트 사용
        res, err := pf.PortForwardService(
            ctx, kubeNamespace, "hubble-relay",
            int32(localPort), 0,
        )

        // 4. 로컬 주소로 연결
        server = fmt.Sprintf("127.0.0.1:%d", res.ForwardedPort.Local)
    }
    // ...
}
```

### 6.2 사용 예시

```bash
# 기본 포트 포워딩 (localhost:4245)
hubble observe --port-forward

# 커스텀 로컬 포트
hubble observe --port-forward --port-forward-port 8080

# 랜덤 포트 할당
hubble observe --port-forward --port-forward-port 0

# 특정 K8s 컨텍스트 및 네임스페이스
hubble observe \
  --port-forward \
  --kube-context production \
  --kube-namespace cilium-system \
  --kubeconfig ~/.kube/config

# 축약형
hubble observe -P
```

### 6.3 포트 포워딩 흐름

```
  hubble CLI
      │
      │ --port-forward
      │
      v
  ┌─────────────────────┐
  │ kubeconfig 로드       │
  │ (--kube-context,     │
  │  --kubeconfig)       │
  └──────────┬──────────┘
             │
             v
  ┌─────────────────────┐
  │ kubernetes.NewForConfig│
  └──────────┬──────────┘
             │
             v
  ┌─────────────────────┐     ┌─────────────────────┐
  │ PortForwardService  │────>│ hubble-relay Pod     │
  │ ("hubble-relay",    │     │ :4245                │
  │  localPort, 0)      │     └─────────────────────┘
  └──────────┬──────────┘
             │
             │ 127.0.0.1:{localPort}
             v
  ┌─────────────────────┐
  │ grpc.NewClient(     │
  │  "127.0.0.1:4245")  │
  └─────────────────────┘
```

---

## 7. Hubble Relay 배포

### 7.1 Cilium Helm 차트를 통한 배포

```bash
# Hubble + Relay 활성화
helm upgrade --install cilium cilium/cilium \
  --namespace kube-system \
  --set hubble.enabled=true \
  --set hubble.relay.enabled=true \
  --set hubble.ui.enabled=true

# Hubble 메트릭 활성화
helm upgrade cilium cilium/cilium \
  --namespace kube-system \
  --reuse-values \
  --set hubble.metrics.enabled="{dns,drop,tcp,flow,port-distribution,icmp,httpV2:exemplars=true;labelsContext=source_ip\,source_namespace\,source_workload\,destination_ip\,destination_namespace\,destination_workload\,traffic_direction}"
```

### 7.2 Hubble Relay 핵심 설정

| 설정 | 기본값 | 설명 |
|------|--------|------|
| `hubble.relay.replicas` | 1 | Relay 레플리카 수 |
| `hubble.relay.listenAddress` | `:4245` | gRPC 리스닝 주소 |
| `hubble.relay.peerService` | `unix:///var/run/cilium/hubble.sock` | Peer 서비스 주소 |
| `hubble.relay.retryTimeout` | 30s | 연결 재시도 타임아웃 |
| `hubble.relay.sortBufferLenMax` | 100 | 정렬 버퍼 최대 크기 |
| `hubble.relay.sortBufferDrainTimeout` | 1s | 정렬 버퍼 드레인 타임아웃 |
| `hubble.relay.tls.server.enabled` | true | 서버측 TLS |
| `hubble.relay.tls.client.enabled` | true | 클라이언트측 TLS |

### 7.3 배포 확인

```bash
# Relay 파드 상태 확인
kubectl get pods -n kube-system -l k8s-app=hubble-relay

# Relay 서비스 확인
kubectl get svc -n kube-system hubble-relay

# Hubble 상태 확인
hubble status --port-forward

# 기대 출력:
# Healthcheck (via localhost:4245): Ok
# Current/Max Flows: 4,096/4,096 (100.00%)
# Flows/s: 15.32
# Connected Nodes: 3/3

# 노드 목록 확인
hubble list nodes --port-forward
```

---

## 8. Prometheus 메트릭 모니터링

### 8.1 Hubble 메트릭 종류

Hubble은 다음과 같은 Prometheus 메트릭을 노출한다:

**Flow 메트릭:**

| 메트릭 | 타입 | 설명 |
|--------|------|------|
| `hubble_flows_processed_total` | Counter | 처리된 총 플로우 수 |
| `hubble_drop_total` | Counter | 드롭된 패킷 수 |
| `hubble_tcp_flags_total` | Counter | TCP 플래그별 카운트 |
| `hubble_port_distribution_total` | Counter | 포트 분포 |
| `hubble_icmp_total` | Counter | ICMP 메시지 수 |
| `hubble_dns_queries_total` | Counter | DNS 쿼리 수 |
| `hubble_dns_responses_total` | Counter | DNS 응답 수 |
| `hubble_http_requests_total` | Counter | HTTP 요청 수 |
| `hubble_http_responses_total` | Counter | HTTP 응답 수 |
| `hubble_http_request_duration_seconds` | Histogram | HTTP 요청 지연 |

**내부 메트릭:**

| 메트릭 | 타입 | 설명 |
|--------|------|------|
| `hubble_lost_events_total` | Counter | 손실된 이벤트 수 (소스별) |

### 8.2 메트릭 활성화 방법

Cilium ConfigMap 또는 Helm 값을 통해 메트릭을 활성화한다:

```yaml
# Helm values.yaml
hubble:
  metrics:
    enabled:
      - dns
      - drop
      - tcp
      - flow
      - port-distribution
      - icmp
      - httpV2:exemplars=true;labelsContext=source_ip,source_namespace,source_workload,destination_ip,destination_namespace,destination_workload,traffic_direction

    # 메트릭 서버 리스닝 포트
    serviceMonitor:
      enabled: true  # Prometheus ServiceMonitor 생성
```

### 8.3 Grafana 대시보드

Hubble 메트릭을 위한 Grafana 대시보드를 설정할 수 있다:

```bash
# Grafana 대시보드 가져오기 (Cilium/Hubble 공식 대시보드)
# Dashboard ID: 16611 (Hubble)
# Dashboard ID: 16612 (Hubble DNS)
# Dashboard ID: 16613 (Hubble HTTP)
```

### 8.4 알림 규칙 예시

```yaml
# PrometheusRule: Hubble 이벤트 손실 알림
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: hubble-alerts
spec:
  groups:
    - name: hubble
      rules:
        - alert: HubbleLostEvents
          expr: rate(hubble_lost_events_total[5m]) > 0
          for: 10m
          labels:
            severity: warning
          annotations:
            summary: "Hubble이 이벤트를 손실하고 있습니다"
            description: "소스 {{ $labels.source }}에서 5분간 {{ $value }} 이벤트/초 손실"

        - alert: HubbleRelayDisconnected
          expr: hubble_relay_connected_peers == 0
          for: 5m
          labels:
            severity: critical
          annotations:
            summary: "Hubble Relay에 연결된 피어가 없습니다"
```

---

## 9. CLI 주요 명령어

### 9.1 hubble observe

가장 많이 사용하는 명령어로, 네트워크 플로우를 실시간 관찰한다:

```bash
# 기본 사용
hubble observe

# 실시간 스트림 (follow)
hubble observe -f

# 최근 N개 플로우
hubble observe --last 100

# 네임스페이스 필터
hubble observe --namespace default

# 파드 필터
hubble observe --from-pod default/frontend --to-pod default/backend

# IP 필터
hubble observe --ip-src 10.0.0.1 --ip-dst 10.0.0.2

# CIDR 필터
hubble observe --ip-src 10.0.0.0/24

# 프로토콜 필터
hubble observe --protocol TCP --port 80

# HTTP 필터
hubble observe --http-method GET --http-path "/api/.*"

# DNS 필터
hubble observe --dns-query "*.example.com"

# Verdict 필터
hubble observe --verdict DROPPED
hubble observe --verdict FORWARDED

# 라벨 필터
hubble observe --label "app=nginx"

# 출력 형식
hubble observe -o json      # JSON 출력
hubble observe -o jsonpb    # Protobuf JSON 출력
hubble observe -o compact   # 컴팩트 출력
hubble observe -o dict      # 딕셔너리 출력
hubble observe -o table     # 테이블 출력 (기본)

# 시간 범위
hubble observe --since 5m
hubble observe --since "2024-01-01T00:00:00Z" --until "2024-01-01T01:00:00Z"

# FieldMask (전송 최적화)
hubble observe --field-mask source,destination,verdict

# 노드 이름 표시
hubble observe --node-name

# IP 변환 (파드/서비스 이름)
hubble observe --ip-translation
```

### 9.2 hubble status

서버 상태를 확인한다:

```bash
hubble status
hubble status --port-forward

# 출력 예시:
# Healthcheck (via localhost:4245): Ok
# Current/Max Flows: 8,190/8,190 (100.00%)
# Flows/s: 42.15
# Connected Nodes: 5/5
```

### 9.3 hubble list

```bash
# 노드 목록
hubble list nodes

# 네임스페이스 목록
hubble list namespaces
```

### 9.4 기타 명령어

```bash
# 버전 확인
hubble version

# 완성 스크립트 생성
hubble completion bash > /etc/bash_completion.d/hubble
hubble completion zsh > "${fpath[1]}/_hubble"
```

---

## 10. 트러블슈팅

### 10.1 연결 실패

**증상:** `hubble observe` 실행 시 연결 거부 또는 타임아웃

```
Error: failed to create gRPC client to 'localhost:4245': ...
```

**원인 및 해결:**

| 원인 | 확인 명령 | 해결 방법 |
|------|----------|----------|
| Hubble 비활성화 | `cilium config \| grep hubble` | `hubble.enabled=true` |
| Relay 미배포 | `kubectl get pods -l k8s-app=hubble-relay` | Relay 활성화 |
| 서비스 없음 | `kubectl get svc hubble-relay` | Helm 차트 재배포 |
| 포트 포워딩 미사용 | - | `--port-forward` 플래그 추가 |
| 네트워크 정책 차단 | `kubectl get cnp -A` | 적절한 정책 추가 |

```bash
# 진단 스크립트
echo "=== Hubble 상태 점검 ==="

# 1. Cilium에서 Hubble 활성화 확인
kubectl -n kube-system exec ds/cilium -- cilium status | grep Hubble

# 2. Relay 파드 상태
kubectl -n kube-system get pods -l k8s-app=hubble-relay -o wide

# 3. Relay 로그 확인
kubectl -n kube-system logs -l k8s-app=hubble-relay --tail=50

# 4. Cilium 에이전트의 Hubble 소켓 확인
kubectl -n kube-system exec ds/cilium -- ls -la /var/run/cilium/hubble.sock

# 5. 직접 연결 테스트
hubble observe --server localhost:4245 --last 1
```

### 10.2 빈 응답 (Empty Response)

**증상:** `hubble observe` 실행 시 플로우가 표시되지 않음

```bash
# 진단
hubble status --port-forward

# NumFlows가 0이면 → Hubble 서버에 이벤트가 없음
# Connected Nodes가 0이면 → Relay가 노드에 연결되지 않음
```

**원인 및 해결:**

| 원인 | 확인 방법 | 해결 |
|------|----------|------|
| 트래픽 없음 | `hubble status`로 SeenFlows 확인 | 워크로드 트래픽 생성 |
| 필터 너무 제한적 | 필터 없이 실행 | 필터 조건 완화 |
| Ring Buffer 크기 부족 | `hubble.eventQueueSize` 확인 | 큐 크기 증가 |
| 노드 연결 안됨 | `hubble list nodes` | Relay 재시작 |

```bash
# Ring Buffer 크기 확인
helm get values cilium -n kube-system | grep -i eventQueue

# Ring Buffer 크기 증가
helm upgrade cilium cilium/cilium \
  --namespace kube-system \
  --reuse-values \
  --set hubble.eventQueueSize=16383
```

### 10.3 TLS 에러

**증상:** TLS 핸드셰이크 실패

```
transport: authentication handshake failed: x509: certificate signed by unknown authority
```

**해결 단계:**

```bash
# 1. 인증서 상태 확인
kubectl -n kube-system get secret hubble-relay-client-certs
kubectl -n kube-system get secret hubble-server-certs

# 2. 인증서 만료 확인
kubectl -n kube-system get secret hubble-relay-client-certs \
  -o jsonpath='{.data.tls\.crt}' | base64 -d | openssl x509 -noout -dates

# 3. Cilium CA 확인
kubectl -n kube-system get secret cilium-ca \
  -o jsonpath='{.data.ca\.crt}' | base64 -d | openssl x509 -noout -text

# 4. 인증서 재생성 (주의: 서비스 중단 가능)
kubectl -n kube-system delete secret hubble-server-certs hubble-relay-client-certs
kubectl -n kube-system rollout restart deployment/hubble-relay
kubectl -n kube-system rollout restart ds/cilium

# 5. CLI에서 수동 CA 지정
hubble observe --tls \
  --tls-ca-cert-files /tmp/cilium-ca.crt \
  --server hubble-relay:4245

# 6. 개발 환경: insecure 모드 (비권장)
hubble observe --tls --tls-allow-insecure
```

### 10.4 이벤트 손실 (Lost Events)

**증상:** `EVENTS LOST` 메시지 출력

```
EVENTS LOST: PERF_EVENT_RING_BUFFER CPU(2) - 15
```

**원인:** CPU 부하 또는 버퍼 크기 부족

```bash
# 손실 이벤트 메트릭 확인
kubectl -n kube-system exec ds/cilium -- \
  curl -s localhost:9962/metrics | grep hubble_lost_events_total

# 해결: 큐 크기 증가
helm upgrade cilium cilium/cilium \
  --namespace kube-system \
  --reuse-values \
  --set hubble.eventQueueSize=16383 \
  --set hubble.eventBufferCapacity=65535
```

### 10.5 Relay 노드 연결 불안정

**증상:** `hubble list nodes`에서 일부 노드가 UNAVAILABLE

```bash
# Relay 로그에서 연결 문제 확인
kubectl -n kube-system logs deployment/hubble-relay | grep -i "fail\|error\|disconnect"

# Peer 서비스 상태 확인
kubectl -n kube-system exec ds/cilium -- cilium status | grep "Hubble"

# 해결: Relay 재시작
kubectl -n kube-system rollout restart deployment/hubble-relay
```

### 10.6 성능 문제

**증상:** `hubble observe` 출력이 느리거나 끊김

```bash
# FieldMask로 전송량 줄이기
hubble observe -o compact \
  --field-mask source.namespace,source.pod_name,destination.namespace,destination.pod_name,verdict

# 필터를 서버측에서 적용 (대역폭 절약)
hubble observe --verdict DROPPED  # 클라이언트 필터 대신 서버 필터 사용

# Relay 정렬 버퍼 조정
helm upgrade cilium cilium/cilium \
  --namespace kube-system \
  --reuse-values \
  --set hubble.relay.sortBufferLenMax=200 \
  --set hubble.relay.sortBufferDrainTimeout=2s
```

---

## 11. 운영 베스트 프랙티스

### 11.1 프로덕션 환경 권장 설정

```yaml
# Helm values.yaml - 프로덕션 권장
hubble:
  enabled: true
  eventQueueSize: 16383       # 큰 클러스터에서 권장
  eventBufferCapacity: 65535  # Ring Buffer 크기

  relay:
    enabled: true
    replicas: 2                # HA 구성
    resources:
      requests:
        cpu: 100m
        memory: 128Mi
      limits:
        cpu: 1000m
        memory: 1Gi
    sortBufferLenMax: 100
    sortBufferDrainTimeout: 1s

  tls:
    enabled: true
    auto:
      enabled: true
      method: cronJob          # certmanager 또는 cronJob
      certValidityDuration: 1095  # 3년

  metrics:
    enabled:
      - dns:query
      - drop
      - tcp
      - flow
      - httpV2
    serviceMonitor:
      enabled: true

  ui:
    enabled: true
    replicas: 1
```

### 11.2 모니터링 체크리스트

```
[ ] hubble status로 연결 상태 확인
[ ] Connected Nodes / Total Nodes 비율 확인
[ ] hubble_lost_events_total 메트릭 모니터링
[ ] TLS 인증서 만료일 모니터링
[ ] Relay 파드 리소스 사용량 모니터링
[ ] Ring Buffer 사용률 확인 (NumFlows/MaxFlows)
```

### 11.3 보안 권장사항

```
1. TLS 항상 활성화 (tls-allow-insecure 사용 금지)
2. mTLS 사용 권장
3. RBAC으로 hubble-relay 접근 제한
4. NetworkPolicy로 Relay 접근 제한
5. 인증서 자동 갱신 설정 (cert-manager)
6. 감사 로깅 활성화
```

---

## 12. 빌드 및 릴리스

### 12.1 릴리스 프로세스

소스 경로: `hubble/Makefile`

```bash
# 릴리스 바이너리 생성 (Docker 기반)
make release

# 로컬 릴리스 빌드
make local-release
```

릴리스 빌드는 Docker 컨테이너 내에서 실행되어 재현 가능한 빌드를 보장한다:

```makefile
release:
    $(CONTAINER_ENGINE) run --rm \
        --workdir /hubble \
        --volume `pwd`:/hubble \
        docker.io/library/golang:$(GOLANG_IMAGE_VERSION)@$(GOLANG_IMAGE_SHA) \
        sh -c "apk add --no-cache setpriv make git && \
            /bin/setpriv --reuid=$(RELEASE_UID) --regid=$(RELEASE_GID) \
            --clear-groups make GOCACHE=/tmp/gocache local-release"
```

### 12.2 릴리스 산출물

`local-release` 타겟은 각 OS/아키텍처별로 다음을 생성한다:

```
release/
├── hubble-darwin-amd64.tar.gz
├── hubble-darwin-amd64.tar.gz.sha256sum
├── hubble-darwin-arm64.tar.gz
├── hubble-darwin-arm64.tar.gz.sha256sum
├── hubble-linux-amd64.tar.gz
├── hubble-linux-amd64.tar.gz.sha256sum
├── hubble-linux-arm64.tar.gz
├── hubble-linux-arm64.tar.gz.sha256sum
├── hubble-windows-amd64.tar.gz
├── hubble-windows-amd64.tar.gz.sha256sum
├── hubble-windows-arm64.tar.gz
└── hubble-windows-arm64.tar.gz.sha256sum
```

### 12.3 FieldMask 기본값

소스 경로: `hubble/pkg/defaults/defaults.go`

CLI가 `dict`, `tab`, `compact` 출력 형식을 사용할 때 전송되는 기본 필드 목록:

```go
FieldMask = []string{
    "time",
    "source.identity", "source.namespace", "source.pod_name",
    "destination.identity", "destination.namespace", "destination.pod_name",
    "source_service", "destination_service",
    "l4", "IP", "ethernet", "l7",
    "Type", "node_name", "is_reply",
    "event_type", "verdict", "Summary",
}
```

이 FieldMask는 gRPC 서버에 필요한 필드만 요청하여 네트워크 대역폭과 서버 처리 비용을
절감한다. JSON 출력 형식에서는 FieldMask가 적용되지 않아 전체 Flow 데이터가 전송된다.

---

## 요약

| 항목 | 내용 |
|------|------|
| 기본 서버 주소 | `localhost:4245` |
| 기본 타임아웃 | 다이얼 5s, 요청 12s |
| 설정 파일 | `~/.config/hubble/config.yaml` |
| TLS 접두사 | `tls://` |
| 포트 포워딩 | `--port-forward` (`-P`) |
| 릴리스 플랫폼 | darwin/linux/windows (amd64/arm64) |
| 필수 의존성 | Cilium + Hubble Relay |
| 메트릭 엔드포인트 | `:9962/metrics` (Cilium), `:9966/metrics` (Relay) |
