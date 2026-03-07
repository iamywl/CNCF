# 14. Pilot Agent와 SDS (Secret Discovery Service) Deep-Dive

## 목차

1. [개요](#1-개요)
2. [Pilot Agent의 역할과 아키텍처](#2-pilot-agent의-역할과-아키텍처)
3. [Agent 초기화 흐름](#3-agent-초기화-흐름)
4. [SDS Server 구현 상세](#4-sds-server-구현-상세)
5. [SecretManagerClient 구현](#5-secretmanagerclient-구현)
6. [인증서 소스 우선순위](#6-인증서-소스-우선순위)
7. [인증서 로테이션 상세](#7-인증서-로테이션-상세)
8. [파일 기반 인증서와 심링크 처리](#8-파일-기반-인증서와-심링크-처리)
9. [xDS 프록시](#9-xds-프록시)
10. [VM 지원](#10-vm-지원)
11. [Envoy 부트스트랩 생성과 상태 관리](#11-envoy-부트스트랩-생성과-상태-관리)
12. [전체 데이터 흐름 정리](#12-전체-데이터-흐름-정리)
13. [운영 관련 설정과 트러블슈팅](#13-운영-관련-설정과-트러블슈팅)

---

## 1. 개요

Istio의 `pilot-agent`는 사이드카/게이트웨이 컨테이너 안에서 실행되는 바이너리로, Envoy 프록시의 부트스트랩과 인증서 관리를 책임진다. 특히 **SDS (Secret Discovery Service)**를 통해 Envoy에 mTLS 인증서를 제공하고, **xDS 프록시**를 통해 Istiod와 Envoy 사이의 설정 배포를 중계한다.

이 문서에서는 pilot-agent의 핵심 구성 요소를 소스코드 수준에서 분석한다.

### 핵심 소스 파일

| 파일 경로 | 역할 |
|-----------|------|
| `pilot/cmd/pilot-agent/main.go` | 진입점, SDS 팩토리 주입 |
| `pilot/cmd/pilot-agent/app/cmd.go` | CLI 커맨드 정의, 초기화 흐름 |
| `pkg/istio-agent/agent.go` | Agent 핵심 구조체, Run() 실행 흐름 |
| `pkg/istio-agent/xds_proxy.go` | Envoy-Istiod 간 xDS 프록싱 |
| `pkg/istio-agent/plugins.go` | CA 클라이언트 팩토리 |
| `security/pkg/nodeagent/sds/server.go` | SDS gRPC 서버 |
| `security/pkg/nodeagent/sds/sdsservice.go` | SDS gRPC 서비스 구현 |
| `security/pkg/nodeagent/cache/secretcache.go` | SecretManagerClient (캐시, CSR, 로테이션) |
| `pkg/security/security.go` | 보안 옵션, 인터페이스, 상수 정의 |

---

## 2. Pilot Agent의 역할과 아키텍처

Pilot Agent는 단순히 Envoy를 실행하는 래퍼가 아니다. 다음 네 가지 핵심 역할을 수행한다.

### 2.1 역할 요약

```
+------------------------------------------------------------------+
|                        Pod (Sidecar 또는 Gateway)                  |
|                                                                    |
|  +-----------------------------------------------------------+    |
|  |                      pilot-agent                           |    |
|  |                                                            |    |
|  |  1. SDS Server                                             |    |
|  |     - UDS 소켓으로 인증서 제공                              |    |
|  |     - SecretManagerClient를 통한 캐시/CSR                  |    |
|  |                                                            |    |
|  |  2. xDS Proxy                                              |    |
|  |     - Envoy <-> Istiod 간 ADS 스트림 중계                  |    |
|  |     - NDS (Name Table), PCDS 등 내부 타입 처리             |    |
|  |                                                            |    |
|  |  3. Envoy Bootstrap                                        |    |
|  |     - 부트스트랩 JSON 생성                                  |    |
|  |     - Envoy 프로세스 시작/종료 관리                         |    |
|  |                                                            |    |
|  |  4. Health Check / Status                                  |    |
|  |     - 애플리케이션 헬스체크 대리                             |    |
|  |     - DNS 프록시                                            |    |
|  +-----------------------------------------------------------+    |
|                          |                                         |
|                    UDS Sockets                                      |
|                          |                                         |
|  +-----------------------------------------------------------+    |
|  |                       Envoy                                |    |
|  |  - SDS 요청 (인증서)    -> UDS -> SDS Server               |    |
|  |  - ADS 요청 (설정)      -> UDS -> xDS Proxy -> Istiod      |    |
|  +-----------------------------------------------------------+    |
+------------------------------------------------------------------+
```

### 2.2 왜 Agent가 중간에 있는가?

직접 Envoy가 Istiod에 연결하지 않고 Agent를 거치는 이유는 다음과 같다:

1. **인증서 관리 분리**: Envoy는 SDS 프로토콜만 알면 되고, CSR 생성/CA 연동 로직은 Agent가 담당
2. **연결 통합**: 하나의 TCP 연결에 여러 gRPC 스트림을 다중화하여 연결 수 절감
3. **내부 타입 처리**: NDS(DNS 테이블), PCDS(ProxyConfig) 등 Envoy에 전달할 필요 없는 리소스를 Agent가 직접 소비
4. **VM 지원**: 쿠버네티스 서비스 어카운트 토큰이 없는 VM 환경에서 인증 흐름을 유연하게 처리

---

## 3. Agent 초기화 흐름

### 3.1 진입점: main.go

`pilot/cmd/pilot-agent/main.go`가 프로그램의 진입점이다. 여기서 핵심적으로 하는 일은 **SDS 팩토리 함수**를 `NewRootCommand`에 주입하는 것이다:

```go
// pilot/cmd/pilot-agent/main.go
func main() {
    log.EnableKlogWithCobra()
    rootCmd := app.NewRootCommand(
        func(options *security.Options, workloadSecretCache security.SecretManager,
             pkpConf *meshconfig.PrivateKeyProvider) istioagent.SDSService {
            return sds.NewServer(options, workloadSecretCache, pkpConf)
        })
    if err := rootCmd.Execute(); err != nil {
        log.Error(err)
        os.Exit(-1)
    }
}
```

이 팩토리 패턴은 테스트 시 SDS 서버를 모킹할 수 있게 해주며, `sds.NewServer`의 직접 의존성을 agent 패키지에서 분리한다.

### 3.2 커맨드 구조: cmd.go

`NewRootCommand`는 Cobra CLI를 구성한다:

```go
// pilot/cmd/pilot-agent/app/cmd.go
func NewRootCommand(sds istioagent.SDSServiceFactory) *cobra.Command {
    rootCmd := &cobra.Command{
        Use:   "pilot-agent",
        Short: "Istio Pilot agent.",
        Long:  "Istio Pilot agent runs in the sidecar or gateway container
                and bootstraps Envoy.",
    }
    proxyCmd := newProxyCommand(sds)
    rootCmd.AddCommand(proxyCmd)
    rootCmd.AddCommand(requestCmd)   // 디버깅용 요청
    rootCmd.AddCommand(waitCmd)      // Envoy 준비 대기
    rootCmd.AddCommand(version.CobraCommand())
    rootCmd.AddCommand(iptables.GetCommand(loggingOptions))  // iptables 설정
    return rootCmd
}
```

### 3.3 프록시 커맨드 실행 흐름

`pilot-agent proxy` 명령이 실행되면 다음 순서로 초기화가 진행된다:

```
main()
  |
  v
NewRootCommand(sds factory)
  |
  v
newProxyCommand(sds) .RunE:
  |
  +-- initProxy(args)           ... IP 주소 발견, 프록시 타입 결정
  |
  +-- config.ConstructProxyConfig() ... 메시 설정, ProxyConfig 생성
  |
  +-- options.NewSecurityOptions()  ... TLS/CA 보안 옵션
  |
  +-- options.NewAgentOptions()     ... DNS, xDS, SDS 팩토리 등 에이전트 옵션
  |
  +-- istioagent.NewAgent()         ... Agent 구조체 생성
  |
  +-- initStatusServer()            ... 헬스체크/프로파일링 HTTP 서버
  |
  +-- agent.Run(ctx)                ... 핵심 실행 (SDS + xDS + Envoy)
  |
  +-- wait()                        ... 블로킹 대기
```

### 3.4 initProxy: 프록시 정보 수집

`initProxy`는 Pod의 네트워크 환경을 파악한다:

```go
// pilot/cmd/pilot-agent/app/cmd.go
func initProxy(args []string) error {
    proxyArgs.Type = model.SidecarProxy  // 기본값
    if len(args) > 0 {
        proxyArgs.Type = model.NodeType(args[0])  // "router" 등 가능
    }

    podIP, _ := netip.ParseAddr(options.InstanceIPVar.Get())
    if podIP.IsValid() {
        proxyArgs.IPAddresses = []string{podIP.String()}
    }

    // 노드의 모든 private IP 수집
    if ipAddrs, ok := network.GetPrivateIPs(context.Background()); ok {
        proxyAddrs = append(proxyAddrs, ipAddrs...)
    }

    // traffic.sidecar.istio.io/excludeInterfaces 어노테이션 처리
    excludeAddrs := getExcludeInterfaces()
    // ...

    proxyArgs.ID = proxyArgs.PodName + "." + proxyArgs.PodNamespace
    proxyArgs.DNSDomain = getDNSDomain(proxyArgs.PodNamespace, proxyArgs.DNSDomain)
    return nil
}
```

### 3.5 Agent 구조체

```go
// pkg/istio-agent/agent.go
type Agent struct {
    proxyConfig *mesh.ProxyConfig
    cfg         *AgentOptions
    secOpts     *security.Options
    envoyOpts   envoy.ProxyConfig

    envoyAgent  *envoy.Agent          // Envoy 프로세스 관리자
    sdsServer   SDSService            // SDS gRPC 서버
    secretCache *cache.SecretManagerClient  // 인증서 캐시/생성
    xdsProxy    *XdsProxy             // Envoy<->Istiod xDS 프록시
    fileWatcher filewatcher.FileWatcher    // 파일 변경 감시
    localDNSServer *dnsClient.LocalDNSServer  // DNS 프록시

    wg sync.WaitGroup  // graceful shutdown 대기
}
```

---

## 4. SDS Server 구현 상세

### 4.1 SDS 서버 아키텍처

SDS 서버는 **UDS (Unix Domain Socket)**를 통해 Envoy에 인증서를 제공하는 gRPC 서비스다.

```
+------------------+     UDS Socket     +------------------+
|                  |  /var/run/secrets/  |                  |
|    Envoy         | workload-spiffe-uds |   SDS Server     |
|                  | ==================> |                  |
| StreamSecrets()  |     /socket         | sdsservice       |
|                  |                     |   .generate()    |
+------------------+                     |   .push()        |
                                         +--------+---------+
                                                  |
                                                  v
                                         +------------------+
                                         | SecretManager    |
                                         | Client           |
                                         | .GenerateSecret()|
                                         +--------+---------+
                                                  |
                                         +--------v---------+
                                         |  CA Client       |
                                         |  (Citadel/       |
                                         |   External CA)   |
                                         +------------------+
```

### 4.2 Server 구조체

`security/pkg/nodeagent/sds/server.go`에 정의된 Server는 gRPC 서버의 라이프사이클을 관리한다:

```go
// security/pkg/nodeagent/sds/server.go
type Server struct {
    workloadSds          *sdsservice
    grpcWorkloadListener net.Listener
    grpcWorkloadServer   *grpc.Server
    stopped              *atomic.Bool
}

func NewServer(options *security.Options, workloadSecretCache security.SecretManager,
    pkpConf *mesh.PrivateKeyProvider) *Server {
    s := &Server{stopped: atomic.NewBool(false)}
    s.workloadSds = newSDSService(workloadSecretCache, options, pkpConf)
    s.initWorkloadSdsService(options)
    return s
}
```

### 4.3 UDS 소켓 리스닝

`initWorkloadSdsService`는 UDS 소켓을 생성하고, 지수 백오프로 재시도한다:

```go
// security/pkg/nodeagent/sds/server.go
func (s *Server) initWorkloadSdsService(opts *security.Options) {
    s.grpcWorkloadServer = grpc.NewServer(s.grpcServerOptions()...)
    s.workloadSds.register(s.grpcWorkloadServer)

    // 소켓 경로 결정
    path := security.GetIstioSDSServerSocketPath()
    if opts.ServeOnlyFiles {
        path = security.FileCredentialNameSocketPath
    }
    s.grpcWorkloadListener, err = uds.NewListener(path)

    go func() {
        waitTime := time.Second
        for i := 0; i < maxRetryTimes; i++ {  // maxRetryTimes = 5
            if s.stopped.Load() { return }
            // ... UDS 셋업 및 Serve 재시도
            time.Sleep(waitTime)
            waitTime *= 2  // 지수 백오프: 1s, 2s, 4s, 8s, 16s
        }
    }()
}
```

소켓 경로 상수:

| 상수 | 값 | 용도 |
|------|----|------|
| `WorkloadIdentityPath` | `./var/run/secrets/workload-spiffe-uds` | SDS 소켓 디렉토리 |
| `DefaultWorkloadIdentitySocketFile` | `socket` | 기본 소켓 파일명 |
| `FileCredentialNameSocketPath` | `./var/run/secrets/credential-uds/files-socket` | 파일 전용 SDS 소켓 |

### 4.4 sdsservice 핵심 구현

```go
// security/pkg/nodeagent/sds/sdsservice.go
type sdsservice struct {
    st         security.SecretManager   // SecretManagerClient
    stop       chan struct{}
    rootCaPath string
    pkpConf    *mesh.PrivateKeyProvider

    sync.Mutex
    clients map[string]*Context  // 연결된 클라이언트 추적
}
```

#### StreamSecrets: 스트리밍 방식

Envoy는 `StreamSecrets` RPC를 통해 인증서를 요청한다. Istio는 `xds.Stream` 헬퍼를 사용해 일반적인 xDS 스트림 패턴을 구현한다:

```go
// security/pkg/nodeagent/sds/sdsservice.go
func (s *sdsservice) StreamSecrets(stream sds.SecretDiscoveryService_StreamSecretsServer) error {
    return xds.Stream(&Context{
        BaseConnection: xds.NewConnection("", stream),
        s:              s,
        w:              &Watch{},
    })
}
```

#### generate: 인증서 생성/조회

`generate`는 리소스 이름별로 `SecretManager.GenerateSecret`을 호출하고, Envoy가 이해하는 `tls.Secret` protobuf로 변환한다:

```go
func (s *sdsservice) generate(resourceNames []string) (*discovery.DiscoveryResponse, error) {
    resources := xds.Resources{}
    for _, resourceName := range resourceNames {
        secret, err := s.st.GenerateSecret(resourceName)
        if err != nil {
            return nil, fmt.Errorf("failed to generate secret for %v: %v",
                                   resourceName, err)
        }
        res := protoconv.MessageToAny(toEnvoySecret(secret, s.rootCaPath, s.pkpConf))
        resources = append(resources, &discovery.Resource{
            Name:     resourceName,
            Resource: res,
        })
    }
    return &discovery.DiscoveryResponse{
        TypeUrl:     model.SecretType,
        VersionInfo: time.Now().Format(time.RFC3339) + "/" +
                     strconv.FormatUint(version.Inc(), 10),
        Nonce:       uuid.New().String(),
        Resources:   xds.ResourcesToAny(resources),
    }, nil
}
```

#### push: 인증서 갱신 알림

인증서가 갱신되면 모든 연결된 클라이언트에 push 알림을 보낸다:

```go
func (s *sdsservice) push(secretName string) {
    s.Lock()
    defer s.Unlock()
    for _, client := range s.clients {
        go func(client *Context) {
            select {
            case client.XdsConnection().PushCh() <- secretName:
            case <-client.XdsConnection().StreamDone():
            }
        }(client)
    }
}
```

### 4.5 toEnvoySecret: Envoy 비밀 변환

`SecretItem`을 Envoy의 `tls.Secret` protobuf로 변환한다. 리소스 이름에 따라 두 가지 타입을 생성한다:

```
리소스명 "ROOTCA" -> tls.Secret_ValidationContext (루트 인증서)
리소스명 "default" -> tls.Secret_TlsCertificate   (워크로드 키/인증서)
```

Private Key Provider가 설정된 경우 CryptoMB나 QAT 하드웨어 가속기 설정도 포함된다:

```go
switch pkpConf.GetProvider().(type) {
case *mesh.PrivateKeyProvider_Cryptomb:
    // CryptoMB 하드웨어 가속 설정
case *mesh.PrivateKeyProvider_Qat:
    // QAT 하드웨어 가속 설정
default:
    // 기본: inline bytes로 키/인증서 전달
    secret.Type = &tls.Secret_TlsCertificate{
        TlsCertificate: &tls.TlsCertificate{
            CertificateChain: &core.DataSource{
                Specifier: &core.DataSource_InlineBytes{InlineBytes: s.CertificateChain},
            },
            PrivateKey: &core.DataSource{
                Specifier: &core.DataSource_InlineBytes{InlineBytes: s.PrivateKey},
            },
        },
    }
}
```

### 4.6 인증서 워밍 (Pre-generation)

SDS 서비스 생성 시 백그라운드에서 인증서를 미리 생성하여 시작 지연 시간을 줄인다:

```go
// security/pkg/nodeagent/sds/sdsservice.go - newSDSService 내부
go func() {
    b := backoff.NewExponentialBackOff(backoff.DefaultOption())
    ctx, cancel := context.WithCancel(context.Background())
    // ...
    _ = b.RetryWithContext(ctx, func() error {
        _, err := st.GenerateSecret(security.WorkloadKeyCertResourceName)  // "default"
        if err != nil { return err }
        _, err = st.GenerateSecret(security.RootCertReqResourceName)       // "ROOTCA"
        if err != nil { return err }
        return nil
    })
}()
```

이 워밍은 **FileMountedCerts**나 **ServeOnlyFiles** 모드에서는 건너뛴다.

---

## 5. SecretManagerClient 구현

### 5.1 구조체 정의

`SecretManagerClient`는 인증서 캐시, CSR 생성, CA 연동, 파일 감시를 통합하는 핵심 컴포넌트다:

```go
// security/pkg/nodeagent/cache/secretcache.go
type SecretManagerClient struct {
    caClient       security.Client       // CA와 통신하는 클라이언트
    configOptions  *security.Options     // 보안 설정

    secretHandler  func(resourceName string)  // 갱신 콜백 (SDS Server로 전달)

    cache          secretCache           // 인메모리 캐시
    generateMutex  sync.Mutex            // 동시 CSR 방지

    existingCertificateFile security.SdsCertificateConfig  // 기존 파일 인증서

    certWatcher    *fsnotify.Watcher     // 파일 변경 감시
    fileCerts      map[FileCert]struct{} // 감시 중인 파일 목록
    certMutex      sync.RWMutex

    outputMutex    sync.Mutex            // 디스크 쓰기 보호

    configTrustBundleMutex sync.RWMutex
    configTrustBundle      []byte        // 동적 Trust Bundle

    queue          queue.Delayed         // 인증서 로테이션 예약 큐
    stop           chan struct{}
    caRootPath     string
}
```

### 5.2 인메모리 캐시

```go
type secretCache struct {
    mu       sync.RWMutex
    workload *security.SecretItem  // 워크로드 인증서 캐시
    certRoot []byte                // 루트 인증서 캐시
}
```

캐시는 단일 워크로드 인증서만 유지한다. 파일 기반 인증서는 조회 비용이 낮으므로 캐시하지 않는다.

### 5.3 SecretItem 데이터 구조

```go
// pkg/security/security.go
type SecretItem struct {
    CertificateChain []byte   // 워크로드 인증서 체인 (PEM)
    PrivateKey       []byte   // 개인 키 (PEM)
    RootCert         []byte   // CA 루트 인증서 (PEM)
    ResourceName     string   // "default" 또는 "ROOTCA"
    CreatedTime      time.Time
    ExpireTime       time.Time
}
```

### 5.4 GenerateSecret 흐름

`GenerateSecret`은 SDS 요청의 핵심 진입점이다:

```
GenerateSecret(resourceName)
  |
  +-- 1. 파일에서 생성 시도 (generateFileSecret)
  |       - 워크로드 인증서 파일 존재? -> 읽어서 반환
  |       - 루트 인증서 파일 존재? -> 읽어서 반환
  |       - "file-cert:" / "file-root:" 접두사? -> 파일 경로 파싱 후 읽기
  |
  +-- 2. 캐시 확인 (getCachedSecret)
  |       - 캐시된 워크로드 인증서가 있으면 반환
  |
  +-- 3. generateMutex 획득 후 캐시 재확인 (Double-Check Locking)
  |
  +-- 4. 새 인증서 생성 (generateNewSecret)
  |       - CSR 생성 (pkiutil.GenCSR)
  |       - CA에 CSR 서명 요청 (caClient.CSRSign)
  |       - 루트 인증서 번들 수집 (caClient.GetRootCertBundle)
  |
  +-- 5. 캐시에 등록 + 로테이션 예약 (registerSecret)
  |
  +-- 6. 디스크에 출력 (OUTPUT_CERTS 설정 시)
```

소스코드의 핵심 부분:

```go
// security/pkg/nodeagent/cache/secretcache.go
func (sc *SecretManagerClient) GenerateSecret(resourceName string) (
    secret *security.SecretItem, err error) {
    // defer: 생성된 인증서를 디스크에 출력 (OUTPUT_CERTS)
    defer func() {
        if secret == nil || err != nil { return }
        sc.outputMutex.Lock()
        defer sc.outputMutex.Unlock()
        if resourceName == security.RootCertReqResourceName ||
           resourceName == security.WorkloadKeyCertResourceName {
            nodeagentutil.OutputKeyCertToDir(sc.configOptions.OutputKeyCertToDir,
                secret.PrivateKey, secret.CertificateChain, secret.RootCert)
        }
    }()

    // 1. 파일에서 생성 시도
    if sdsFromFile, ns, err := sc.generateFileSecret(resourceName); sdsFromFile {
        return ns, err
    }

    // 2. 캐시 확인
    ns := sc.getCachedSecret(resourceName)
    if ns != nil { return ns, nil }

    // 3. Double-check locking
    sc.generateMutex.Lock()
    defer sc.generateMutex.Unlock()
    ns = sc.getCachedSecret(resourceName)
    if ns != nil { return ns, nil }

    // 4. 새 인증서 생성
    ns, err = sc.generateNewSecret(resourceName)
    if err != nil { return nil, err }

    // 5. 캐시 등록 + 로테이션 예약
    sc.registerSecret(*ns)

    // 루트 인증서 변경 감지
    oldRoot := sc.cache.GetRoot()
    if !bytes.Equal(oldRoot, ns.RootCert) {
        sc.cache.SetRoot(ns.RootCert)
        sc.OnSecretUpdate(security.RootCertReqResourceName)
    }

    return ns, nil
}
```

### 5.5 CSR 생성 및 CA 서명

```go
func (sc *SecretManagerClient) generateNewSecret(resourceName string) (
    *security.SecretItem, error) {

    // SPIFFE ID 구성
    csrHostName := &spiffe.Identity{
        TrustDomain:    sc.configOptions.TrustDomain,
        Namespace:      sc.configOptions.WorkloadNamespace,
        ServiceAccount: sc.configOptions.ServiceAccount,
    }

    // CSR 옵션
    options := pkiutil.CertOptions{
        Host:       csrHostName.String(),  // spiffe://trust-domain/ns/xxx/sa/yyy
        RSAKeySize: sc.configOptions.WorkloadRSAKeySize,
        PKCS8Key:   sc.configOptions.Pkcs8Keys,
        ECSigAlg:   pkiutil.SupportedECSignatureAlgorithms(...),
        ECCCurve:   pkiutil.SupportedEllipticCurves(...),
    }

    // CSR + 개인 키 생성
    csrPEM, keyPEM, err := pkiutil.GenCSR(options)

    // CA에 서명 요청
    certChainPEM, err := sc.caClient.CSRSign(csrPEM,
        int64(sc.configOptions.SecretTTL.Seconds()))

    // 루트 인증서 번들 수집
    trustBundlePEM, err = sc.caClient.GetRootCertBundle()

    // SecretItem 생성
    return &security.SecretItem{
        CertificateChain: certChain,
        PrivateKey:       keyPEM,
        ResourceName:     resourceName,
        CreatedTime:      time.Now(),
        ExpireTime:       expireTime,
        RootCert:         rootCertPEM,
    }, nil
}
```

---

## 6. 인증서 소스 우선순위

Agent는 세 가지 소스에서 인증서를 가져올 수 있으며, 명확한 우선순위가 있다.

### 6.1 결정 흐름도

```
시작: Agent.Run()
  |
  v
+----------------------------------+
| Workload SPIFFE UDS 소켓 확인     |
| 경로: /var/run/secrets/           |
|       workload-spiffe-uds/socket  |
+------------------+---------------+
                   |
          +--------+--------+
          |                 |
     [소켓 존재]        [소켓 없음]
          |                 |
          v                 v
  ServeOnlyFiles=true    +----------------------------------+
  (Istio SDS는           | 워크로드 인증서 파일 확인          |
   파일 전용으로 동작)     | cert-chain.pem + key.pem +       |
                         | root-cert.pem                     |
                         | 경로: /var/run/secrets/            |
                         |   workload-spiffe-credentials/    |
                         +------------------+---------------+
                                            |
                                   +--------+--------+
                                   |                 |
                              [파일 존재]        [파일 없음]
                                   |                 |
                                   v                 v
                          FileMountedCerts=true    기본 CA 흐름
                          (파일 감시 모드)          (CSR -> Istiod CA)
```

### 6.2 소스별 상세

| 우선순위 | 소스 | 조건 | 동작 |
|---------|------|------|------|
| 1 | 외부 SPIFFE UDS 소켓 | 소켓 파일이 존재하고 응답 가능 | Agent의 SDS는 파일 전용으로 전환 |
| 2 | 사전 마운트된 파일 | `cert-chain.pem`, `key.pem`, `root-cert.pem` 파일이 존재 | 파일 감시 모드, CA 클라이언트 불필요 |
| 3 | CA Client (Citadel) | 위 두 조건 모두 해당 없음 | CSR 생성 -> CA 서명 -> 캐시 저장 |

### 6.3 소켓 헬스체크

외부 SDS 소켓이 존재하는 경우, Agent는 실제로 응답 가능한지 확인한다:

```go
// pkg/istio-agent/agent.go
func checkSocket(ctx context.Context, socketPath string) (bool, error) {
    socketExists := socketFileExists(socketPath)
    if !socketExists { return false, nil }

    err := socketHealthCheck(ctx, socketPath)
    if err != nil {
        // 소켓이 있지만 응답 불가 -> 삭제 시도
        err = os.Remove(socketPath)
        if err != nil {
            return false, fmt.Errorf("existing SDS socket could not be removed: %v", err)
        }
        return false, nil
    }
    return true, nil
}

func socketHealthCheck(ctx context.Context, socketPath string) error {
    ctx, cancel := context.WithDeadline(ctx, time.Now().Add(time.Second))
    defer cancel()
    conn, err := grpc.DialContext(ctx, fmt.Sprintf("unix:%s", socketPath),
        grpc.WithTransportCredentials(insecure.NewCredentials()),
        grpc.WithBlock(),
    )
    // ...
}
```

### 6.4 Agent.initSdsServer의 결정 로직

```go
// pkg/istio-agent/agent.go
func (a *Agent) initSdsServer() error {
    // 파일 마운트 인증서 확인
    if security.CheckWorkloadCertificate(
        security.WorkloadIdentityCertChainPath,
        security.WorkloadIdentityKeyPath,
        security.WorkloadIdentityRootCertPath) {
        a.secOpts.FileMountedCerts = true
    }

    // CA 클라이언트 생성 여부 결정
    createCaClient := !a.secOpts.FileMountedCerts && !a.secOpts.ServeOnlyFiles
    a.secretCache, err = a.newSecretManager(createCaClient)

    // SDS 서버 생성 + 콜백 등록
    pkpConf := a.proxyConfig.GetPrivateKeyProvider()
    a.sdsServer = a.cfg.SDSFactory(a.secOpts, a.secretCache, pkpConf)
    a.secretCache.RegisterSecretHandler(a.sdsServer.OnSecretUpdate)
    return nil
}
```

---

## 7. 인증서 로테이션 상세

### 7.1 로테이션 타이밍 계산

인증서 만료 전에 갱신해야 하며, 대규모 클러스터에서 동시 갱신으로 인한 CA 부하를 방지하기 위해 **지터(jitter)**를 적용한다:

```go
// security/pkg/nodeagent/cache/secretcache.go
var rotateTime = func(secret security.SecretItem,
    graceRatio float64, graceRatioJitter float64) time.Duration {

    // 임의의 지터를 [-jitter, +jitter] 범위에서 생성
    jitter := (rand.Float64() * graceRatioJitter) *
              float64(rand.IntN(2)*2-1)
    jitterGraceRatio := graceRatio + jitter

    // 범위 제한 [0, 1]
    if jitterGraceRatio > 1 { jitterGraceRatio = 1 }
    if jitterGraceRatio < 0 { jitterGraceRatio = 0 }

    secretLifeTime := secret.ExpireTime.Sub(secret.CreatedTime)
    gracePeriod := time.Duration(jitterGraceRatio * float64(secretLifeTime))
    delay := time.Until(secret.ExpireTime.Add(-gracePeriod))

    if delay < 0 { delay = 0 }
    return delay
}
```

예시 계산:

```
인증서 TTL: 24시간
graceRatio: 0.5 (SECRET_GRACE_PERIOD_RATIO 기본값)
graceRatioJitter: 0.01

secretLifeTime = 24h
gracePeriod = 0.5 * 24h = 12h (+-jitter)
delay = expireTime - 12h - now

결과: 인증서 발급 후 약 12시간(+-몇분) 후에 갱신
```

### 7.2 Delayed Queue를 이용한 로테이션 예약

```go
func (sc *SecretManagerClient) registerSecret(item security.SecretItem) {
    delay := rotateTime(item, sc.configOptions.SecretRotationGracePeriodRatio,
                        sc.configOptions.SecretRotationGracePeriodRatioJitter)

    // 이미 예약된 경우 건너뛰기
    if sc.cache.GetWorkload() != nil {
        return
    }
    sc.cache.SetWorkload(&item)

    // 지연 큐에 로테이션 작업 등록
    sc.queue.PushDelayed(func() error {
        if cached := sc.cache.GetWorkload(); cached != nil {
            // 시간이 일치하면 스테일 체크 통과 -> 캐시 무효화 + 갱신 트리거
            if cached.CreatedTime == item.CreatedTime {
                sc.cache.SetWorkload(nil)
                sc.OnSecretUpdate(item.ResourceName)
            }
        }
        return nil
    }, delay)
}
```

### 7.3 로테이션 전체 시퀀스

```
시간 T=0: 인증서 발급, registerSecret 호출
  |
  +-- rotateTime 계산: delay = ~12h (jitter 적용)
  +-- queue.PushDelayed(rotateFunc, delay)
  |
시간 T=12h: rotateFunc 실행
  |
  +-- cache.SetWorkload(nil)   ... 캐시 무효화
  +-- OnSecretUpdate("default") ... 핸들러 호출
       |
       v
  secretHandler("default")     ... SDS Server.OnSecretUpdate
       |
       v
  sdsservice.push("default")  ... 모든 클라이언트에 알림
       |
       v
  Context.Push("default")
       |
       +-- w.requested("default") 확인
       +-- s.generate(["default"])
            |
            +-- SecretManager.GenerateSecret("default")
                 |
                 +-- 캐시 비어있음 -> generateNewSecret
                      |
                      +-- GenCSR -> CSRSign -> 새 인증서 획득
                 |
                 +-- registerSecret: 다시 로테이션 예약
            |
            +-- toEnvoySecret 변환
       |
       v
  Envoy로 새 인증서 Push
```

### 7.4 왜 Delayed Queue인가?

단순 타이머 대신 Delayed Queue를 사용하는 이유:

1. **스테일 검증**: `CreatedTime` 비교로 중복 갱신 방지
2. **구독 기반**: Envoy가 여전히 인증서를 구독 중인지 확인 후 갱신
3. **메모리 효율**: 타이머 객체 대신 큐 항목으로 관리

---

## 8. 파일 기반 인증서와 심링크 처리

### 8.1 fsnotify 기반 파일 감시

`SecretManagerClient`는 `fsnotify`를 사용해 인증서 파일 변경을 감지한다:

```go
// security/pkg/nodeagent/cache/secretcache.go
func NewSecretManagerClient(caClient security.Client, options *security.Options) (
    *SecretManagerClient, error) {
    watcher, err := fsnotify.NewWatcher()
    ret := &SecretManagerClient{
        queue:       queue.NewDelayed(queue.DelayQueueBuffer(0)),
        certWatcher: watcher,
        fileCerts:   make(map[FileCert]struct{}),
        // ...
    }
    go ret.queue.Run(ret.stop)
    go ret.handleFileWatch()  // 파일 이벤트 루프 시작
    return ret, nil
}
```

### 8.2 파일 이벤트 처리

```go
func (sc *SecretManagerClient) handleFileWatch() {
    for {
        select {
        case event, ok := <-sc.certWatcher.Events:
            if !ok { return }
            // Write, Remove, Create, Chmod 이벤트만 처리
            if !(isWrite(event) || isRemove(event) ||
                 isCreate(event) || event.Op&fsnotify.Chmod != 0) {
                continue
            }
            // 영향 받는 리소스 파악
            updatedResources := sc.handleFileEvent(event, resources)
            // 콜백 트리거
            for resourceName := range updatedResources {
                sc.OnSecretUpdate(resourceName)
            }
        case err, ok := <-sc.certWatcher.Errors:
            // 에러 로깅
        }
    }
}
```

### 8.3 심링크 감시의 복잡성

쿠버네티스 Secret은 심링크를 통해 마운트된다. 예를 들어:

```
/var/run/secrets/workload-spiffe-credentials/
  cert-chain.pem -> ..data/cert-chain.pem
  ..data -> ..2024_01_01/
  ..2024_01_01/
    cert-chain.pem  (실제 파일)
```

이런 구조에서 인증서 갱신 시 `..data` 심링크가 새 디렉토리를 가리키도록 변경된다. `SecretManagerClient`는 이를 세 가지 수준의 감시자로 처리한다:

```go
// security/pkg/nodeagent/cache/secretcache.go
func (sc *SecretManagerClient) addSymlinkWatcher(filePath string,
    resourceName string) error {
    // ...
    symlinkPath, found := sc.findSymlinkInPath(absFilePath)
    targetPath, err := sc.resolveSymlink(symlinkPath)

    // 1. 심링크 자체 감시
    //    - 심링크 삭제/재생성/권한변경 감지
    sc.certWatcher.Add(symlinkPath)

    // 2. 실제 대상 파일 감시
    //    - 파일 내용 변경, 삭제, 재생성 감지
    sc.certWatcher.Add(resolvedFilePath)

    // 3. 심링크가 있는 디렉토리 감시
    //    - 디렉토리 삭제/재생성 감지
    symlinkDir := filepath.Dir(symlinkPath)
    sc.certWatcher.Add(symlinkDir)
}
```

### 8.4 시스템 심링크 무시

macOS에서 `/var`는 `/private/var`의 심링크인데, 이것을 인증서 심링크로 오인하면 안 된다:

```go
func (sc *SecretManagerClient) isSystemSymlink(dir string) bool {
    return dir == "/var" || dir == "/tmp" || dir == "/usr"
}
```

### 8.5 심링크 변경 시 재해석

심링크 대상이 변경되면 기존 감시를 제거하고 새 대상을 등록한다:

```go
func (sc *SecretManagerClient) handleSymlinkChange(fc FileCert) {
    newTargetPath, err := sc.resolveSymlink(fc.Filename)
    // 기존 엔트리 삭제
    delete(sc.fileCerts, fc)
    // 새 엔트리 등록
    sc.fileCerts[FileCert{
        ResourceName: fc.ResourceName,
        Filename:     fc.Filename,
        TargetPath:   newTargetPath,
    }] = struct{}{}
    // 감시 대상 업데이트
    if newTargetPath != fc.TargetPath {
        sc.certWatcher.Remove(fc.TargetPath)
        sc.certWatcher.Add(newTargetPath)
    }
}
```

---

## 9. xDS 프록시

### 9.1 아키텍처

xDS 프록시는 Envoy와 Istiod 사이에서 ADS (Aggregated Discovery Service) 스트림을 중계한다:

```
+----------+     UDS       +----------+     gRPC/TLS     +----------+
|          | ------------> |          | ----------------> |          |
|  Envoy   |   (로컬)      | XdsProxy |    (네트워크)      |  Istiod  |
|          | <------------ |          | <---------------- |          |
+----------+  DiscoveryResp +-----+----+  DiscoveryResp   +----------+
                                  |
                            내부 핸들러:
                            - NDS (DNS 테이블)
                            - PCDS (ProxyConfig)
                            - Health Info
                            - ECDS (Wasm 변환)
```

### 9.2 XdsProxy 구조체

```go
// pkg/istio-agent/xds_proxy.go
type XdsProxy struct {
    stopChan             chan struct{}
    clusterID            string
    downstreamListener   net.Listener       // Envoy UDS 리스너
    downstreamGrpcServer *grpc.Server
    istiodAddress        string             // 업스트림 주소
    dialOptions          []grpc.DialOption   // Istiod 연결 옵션
    handlers             map[string]ResponseHandler  // 내부 타입 핸들러
    healthChecker        *health.WorkloadHealthChecker
    xdsHeaders           map[string]string
    xdsUdsPath           string             // UDS 소켓 경로

    connected            *ProxyConnection   // 현재 활성 연결
    wasmCache            wasm.Cache
}
```

### 9.3 연결 수립 흐름

```
Envoy -> UDS Socket -> XdsProxy.StreamAggregatedResources()
                            |
                            v
                       handleStream()
                            |
                       +----+----+
                       |         |
                  registerStream  buildUpstreamConn
                       |         |
                       |    +----v----+
                       |    | Istiod  |
                       |    | gRPC    |
                       |    | 연결    |
                       |    +---------+
                       |
                  handleUpstream(ctx, con, xds)
                       |
                  +----+----+----+
                  |         |    |
            [Recv 고루틴] [Request] [Response]
```

### 9.4 요청/응답 처리

**Envoy -> Istiod 방향** (handleUpstreamRequest):

```go
func (p *XdsProxy) handleUpstreamRequest(con *ProxyConnection) {
    go func() {
        for {
            req, err := con.downstream.Recv()  // Envoy에서 수신
            con.sendRequest(req)               // Istiod로 전달

            // 첫 LDS 요청 후 부가 요청 발사
            if !initialRequestsSent.Load() && req.TypeUrl == model.ListenerType {
                // NDS 요청 (DNS 테이블)
                if _, f := p.handlers[model.NameTableType]; f {
                    con.sendRequest(&discovery.DiscoveryRequest{
                        TypeUrl: model.NameTableType})
                }
                // PCDS 요청 (ProxyConfig)
                if _, f := p.handlers[model.ProxyConfigType]; f {
                    con.sendRequest(&discovery.DiscoveryRequest{
                        TypeUrl: model.ProxyConfigType})
                }
                // 초기 헬스체크 요청
                initialRequest := p.initialHealthRequest
                if initialRequest != nil {
                    con.sendRequest(initialRequest)
                }
            }
        }
    }()
    // ... 요청 채널에서 읽어 업스트림으로 전송
}
```

**Istiod -> Envoy 방향** (handleUpstreamResponse):

```go
func (p *XdsProxy) handleUpstreamResponse(con *ProxyConnection) {
    for {
        select {
        case resp := <-con.responsesChan:
            // 내부 핸들러가 있는 타입은 직접 처리
            if h, f := p.handlers[resp.TypeUrl]; f {
                err := h(resp.Resources[0])
                // ACK/NACK 전송
                con.sendRequest(&discovery.DiscoveryRequest{...})
                continue
            }
            // ECDS는 Wasm 변환 후 전달
            switch resp.TypeUrl {
            case model.ExtensionConfigurationType:
                go p.rewriteAndForward(con, resp, ...)
            default:
                forwardToEnvoy(con, resp)
            }
        }
    }
}
```

### 9.5 무한 버퍼 채널로 데드락 방지

xDS 프록시에는 Envoy 요청과 Istiod 푸시가 동시에 발생할 때 데드락이 생길 수 있다. 이를 방지하기 위해 요청 채널을 **무한 버퍼**로 만든다:

```go
// pkg/istio-agent/xds_proxy.go
con := &ProxyConnection{
    // Unbounded channel - 데드락 방지 핵심
    requestsChan: channels.NewUnbounded[*discovery.DiscoveryRequest](),
    // 응답은 버퍼 1로 제한 (최대 2개: 처리 중 1 + 대기 1)
    responsesChan: make(chan *discovery.DiscoveryResponse, 1),
}
```

### 9.6 내부 핸들러 등록

```go
// pkg/istio-agent/xds_proxy.go - initXdsProxy
if ia.localDNSServer != nil {
    proxy.handlers[model.NameTableType] = func(resp *anypb.Any) error {
        var nt dnsProto.NameTable
        resp.UnmarshalTo(&nt)
        ia.localDNSServer.UpdateLookupTable(&nt)
        return nil
    }
}
if ia.cfg.EnableDynamicProxyConfig && ia.secretCache != nil {
    proxy.handlers[model.ProxyConfigType] = func(resp *anypb.Any) error {
        pc := &meshconfig.ProxyConfig{}
        resp.UnmarshalTo(pc)
        // Trust Bundle 업데이트
        ia.secretCache.UpdateConfigTrustBundle(trustBundle)
        return nil
    }
}
```

---

## 10. VM 지원

### 10.1 VM에서의 인증 흐름

VM은 쿠버네티스 서비스 어카운트 토큰을 자동으로 받지 못한다. 대신 짧은 수명의 JWT 토큰을 프로비저닝하고, 이후 mTLS로 전환한다:

```
+-------------------+         +-------------------+
|  VM (pilot-agent) |         |   Istiod (CA)     |
+--------+----------+         +--------+----------+
         |                             |
    1. JWT 토큰으로 첫 CSR 요청         |
         |------ CSRSign(jwt) -------->|
         |<----- cert + key ----------|
         |                             |
    2. 인증서를 디스크에 저장             |
         | (OUTPUT_CERTS / PROV_CERT)  |
         |                             |
    3. 이후 mTLS로 CSR 요청             |
         |------ CSRSign(mtls) ------->|
         |<----- new cert + key -------|
         |                             |
    4. JWT 만료 후에도 mTLS로 갱신 가능  |
         |                             |
```

### 10.2 PROV_CERT와 OUTPUT_CERTS

```
PROV_CERT (ProvCert):
  - CA 서버와 통신할 때 사용할 mTLS 인증서 경로
  - 워크로드 mTLS가 아닌, 컨트롤 플레인 인증용

OUTPUT_CERTS (OutputKeyCertToDir):
  - GenerateSecret 결과를 디스크에 기록하는 경로
  - VM 재시작 시 인증서를 디스크에서 읽어 부트스트랩 가능
```

코드에서의 처리:

```go
// pkg/istio-agent/agent.go
func (a *Agent) FindRootCAForXDS() (string, error) {
    // ...
    if a.secOpts.ProvCert != "" {
        // VM: PROV_CERT 경로의 루트 인증서 사용
        return a.secOpts.ProvCert + "/root-cert.pem", nil
    }
    // ...
}

func (a *Agent) GetKeyCertsForCA() (string, string) {
    if a.secOpts.ProvCert != "" {
        key := path.Join(a.secOpts.ProvCert, constants.KeyFilename)
        cert := path.Join(a.secOpts.ProvCert, constants.CertChainFilename)
        return key, cert
    }
    return "", ""
}
```

### 10.3 CredentialFetcher

VM 환경에서는 JWT 토큰을 파일이 아닌 메타데이터 서버에서 가져올 수 있다:

```go
// pkg/security/security.go
type CredFetcher interface {
    GetPlatformCredential() (string, error)
    GetIdentityProvider() string
    Stop()
}
```

지원되는 CredentialFetcher 타입:

| 타입 | 용도 |
|------|------|
| `GoogleComputeEngine` | GCE 메타데이터 서버에서 토큰 획득 |
| `JWT` | 파일에서 JWT 토큰 읽기 |
| `Mock` | 테스트 전용 |

### 10.4 인증서 디스크 지속

VM에서는 인증서를 메모리 뿐만 아니라 디스크에도 저장한다. 이는 `GenerateSecret`의 defer에서 처리된다:

```go
func (sc *SecretManagerClient) GenerateSecret(resourceName string) (...) {
    defer func() {
        if secret == nil || err != nil { return }
        sc.outputMutex.Lock()
        defer sc.outputMutex.Unlock()
        // OUTPUT_CERTS 설정 시 디스크에 기록
        nodeagentutil.OutputKeyCertToDir(sc.configOptions.OutputKeyCertToDir,
            secret.PrivateKey, secret.CertificateChain, secret.RootCert)
    }()
    // ...
}
```

VM 재시작 시나리오:

1. VM 재시작 -> 디스크에 저장된 인증서 발견
2. PROV_CERT 경로에서 인증서를 읽어 CA와 mTLS로 통신
3. 새 인증서 발급 (기존 인증서가 만료되지 않은 경우)
4. JWT 토큰 없이도 갱신 가능

---

## 11. Envoy 부트스트랩 생성과 상태 관리

### 11.1 부트스트랩 생성 흐름

```go
// pkg/istio-agent/agent.go
func (a *Agent) initializeEnvoyAgent(_ context.Context) error {
    // 1. 노드 메타데이터 생성
    node, err := a.generateNodeMetadata()

    // 2. 부트스트랩 JSON 생성
    if len(a.proxyConfig.CustomConfigFile) > 0 {
        // 커스텀 설정 파일 사용
        a.envoyOpts.ConfigPath = a.proxyConfig.CustomConfigFile
    } else {
        out, err := bootstrap.New(bootstrap.Config{
            Node:             node,
            CompliancePolicy: common_features.CompliancePolicy,
            LogAsJSON:        a.envoyOpts.LogAsJSON,
        }).CreateFile()
        a.envoyOpts.ConfigPath = out
    }

    // 3. Envoy 프로세스 에이전트 생성
    envoyProxy := envoy.NewProxy(a.envoyOpts)
    a.envoyAgent = envoy.NewAgent(envoyProxy,
        drainDuration, a.cfg.MinimumDrainDuration,
        localHostAddr, adminPort, statusPort,
        prometheusPort, exitOnZeroActiveConnections)
    return nil
}
```

### 11.2 노드 메타데이터

부트스트랩에 포함되는 메타데이터는 Istiod가 프록시를 식별하고 적절한 설정을 생성하는 데 필수적이다:

```go
func (a *Agent) generateNodeMetadata() (*model.Node, error) {
    var pilotSAN []string
    if a.proxyConfig.ControlPlaneAuthPolicy == mesh.AuthenticationPolicy_MUTUAL_TLS {
        pilotSAN = []string{config.GetPilotSan(a.proxyConfig.DiscoveryAddress)}
    }

    // CredentialName SDS 소켓 확인
    credentialSocketExists, err := checkSocket(context.TODO(),
        security.CredentialNameSocketPath)

    return bootstrap.GetNodeMetaData(bootstrap.MetadataOptions{
        ID:                     a.cfg.ServiceNode,
        Envs:                   os.Environ(),
        Platform:               a.cfg.Platform,
        InstanceIPs:            a.cfg.ProxyIPAddresses,
        StsPort:                a.secOpts.STSPort,
        ProxyConfig:            a.proxyConfig,
        PilotSubjectAltName:    pilotSAN,
        CredentialSocketExists: credentialSocketExists,
        // ...
    })
}
```

### 11.3 Agent.Run(): 전체 실행 흐름

```go
// pkg/istio-agent/agent.go
func (a *Agent) Run(ctx context.Context) (func(), error) {
    // 1. DNS 서버 시작
    a.initLocalDNSServer()

    // 2. 외부 SDS 소켓 확인
    configuredAgentSocketPath := security.GetWorkloadSDSSocketListenPath(
        a.cfg.WorkloadIdentitySocketFile)
    socketExists, err := checkSocket(ctx, configuredAgentSocketPath)
    if socketExists {
        a.secOpts.ServeOnlyFiles = true
    }

    // 3. SDS 서버 시작
    a.initSdsServer()

    // 4. xDS 프록시 시작
    a.xdsProxy, err = initXdsProxy(a)

    // 5. gRPC 부트스트랩 생성 (proxyless gRPC용)
    if a.cfg.GRPCBootstrapPath != "" {
        a.generateGRPCBootstrap()
    }

    // 6. 루트 CA 파일 감시 시작
    if a.proxyConfig.ControlPlaneAuthPolicy != mesh.AuthenticationPolicy_NONE {
        rootCAForXDS, _ := a.FindRootCAForXDS()
        go a.startFileWatcher(ctx, rootCAForXDS, func() {
            a.xdsProxy.initIstiodDialOptions(a)
        })
    }

    // 7. Envoy 프로세스 시작
    if !a.EnvoyDisabled() {
        a.initializeEnvoyAgent(ctx)
        a.wg.Add(1)
        go func() {
            defer a.wg.Done()
            a.envoyAgent.Run(ctx)
        }()
    }

    return a.wg.Wait, nil
}
```

### 11.4 상태 서버 (Status Server)

상태 서버는 Kubernetes의 liveness/readiness 프로브를 처리한다:

```go
// pilot/cmd/pilot-agent/app/cmd.go
func initStatusServer(ctx context.Context, proxyConfig *meshconfig.ProxyConfig,
    envoyPrometheusPort int, enableProfiling bool,
    agent *istioagent.Agent, shutdown context.CancelCauseFunc) error {

    o := options.NewStatusServerOptions(proxyArgs.IsIPv6(), proxyArgs.Type,
        proxyConfig, agent)
    o.EnvoyPrometheusPort = envoyPrometheusPort
    o.EnableProfiling = enableProfiling
    statusServer, err := status.NewServer(*o)
    go statusServer.Run(ctx)
    return nil
}
```

Agent 자체도 `ready.Prober` 인터페이스를 구현하여 DNS 준비 상태를 알린다:

```go
var _ ready.Prober = &Agent{}

func (a *Agent) Check() (err error) {
    if a.isDNSServerEnabled() {
        if !a.localDNSServer.IsReady() {
            return errors.New("DNS lookup table is not ready yet")
        }
    }
    return nil
}
```

### 11.5 Graceful Shutdown

```go
// pilot/cmd/pilot-agent/app/cmd.go - newProxyCommand
ctx, cancel := context.WithCancelCause(context.Background())
defer cancel(errors.New("application shutdown"))
defer agent.Close()

// SIGINT/SIGTERM 수신 시 컨텍스트 취소
go cmd.WaitSignalFunc(cancel)

wait, err := agent.Run(ctx)
wait()  // 모든 고루틴 종료 대기

// agent.Close()에서 리소스 정리
func (a *Agent) Close() {
    if a.xdsProxy != nil    { a.xdsProxy.close() }
    if a.localDNSServer != nil { a.localDNSServer.Close() }
    if a.sdsServer != nil   { a.sdsServer.Stop() }
    if a.secretCache != nil { a.secretCache.Close() }
    if a.fileWatcher != nil { a.fileWatcher.Close() }
}
```

---

## 12. 전체 데이터 흐름 정리

### 12.1 인증서 발급 시퀀스

```
Envoy                  SDS Server          SecretManager        CA (Istiod)
  |                        |                    |                    |
  |-- StreamSecrets ------>|                    |                    |
  |   (resource: default)  |                    |                    |
  |                        |                    |                    |
  |                        |-- GenerateSecret ->|                    |
  |                        |   ("default")      |                    |
  |                        |                    |                    |
  |                        |                    |-- GenCSR() ------->|
  |                        |                    |   (SPIFFE SAN)     |
  |                        |                    |                    |
  |                        |                    |<-- CSRSign() ------|
  |                        |                    |   (certChain)      |
  |                        |                    |                    |
  |                        |                    |-- cache.Set() ---->|
  |                        |                    |   queue.Push()     |
  |                        |                    |                    |
  |                        |<-- SecretItem -----|                    |
  |                        |                    |                    |
  |<-- DiscoveryResponse --|                    |                    |
  |   (tls.Secret)         |                    |                    |
  |                        |                    |                    |
  | ... 12시간 후 ...       |                    |                    |
  |                        |                    |                    |
  |                        |              [queue 트리거]              |
  |                        |                    |                    |
  |                        |<- OnSecretUpdate --|                    |
  |                        |   ("default")      |                    |
  |                        |                    |                    |
  |                        |-- push("default")->|                    |
  |                        |                    |                    |
  |                        |-- GenerateSecret ->|                    |
  |                        |   ("default")      |                    |
  |                        |                    |-- CSRSign() ------>|
  |                        |                    |<-- new cert ------|
  |                        |                    |                    |
  |<-- DiscoveryResponse --|                    |                    |
  |   (갱신된 tls.Secret)   |                    |                    |
```

### 12.2 xDS 설정 배포 시퀀스

```
Envoy              XdsProxy              Istiod
  |                    |                    |
  |-- ADS (UDS) ------>|                    |
  |   (LDS 요청)       |                    |
  |                    |-- ADS (gRPC) ----->|
  |                    |   (LDS + NDS)      |
  |                    |                    |
  |                    |<-- LDS 응답 -------|
  |<-- LDS 응답 -------|                    |
  |                    |                    |
  |                    |<-- NDS 응답 -------|
  |                    |   (Agent가 직접 소비)|
  |                    |                    |
  |-- ADS (CDS) ------>|-- ADS (CDS) ----->|
  |<-- CDS 응답 -------|<-- CDS 응답 ------|
  |                    |                    |
```

---

## 13. 운영 관련 설정과 트러블슈팅

### 13.1 주요 환경 변수

| 변수 | 설명 | 기본값 |
|------|------|--------|
| `CA_ADDR` | CA 주소 | discoveryAddress |
| `CA_PROVIDER` | CA 프로바이더 (GoogleCA/Citadel) | Citadel |
| `PROV_CERT` | 컨트롤 플레인 인증용 인증서 경로 | (없음) |
| `OUTPUT_CERTS` | 인증서 디스크 출력 경로 | (없음) |
| `FILE_MOUNTED_CERTS` | 파일 마운트 인증서 사용 | false |
| `SECRET_GRACE_PERIOD_RATIO` | 갱신 시점 비율 | 0.5 |
| `SECRET_TTL` | 인증서 TTL | 24h |
| `PILOT_CERT_PROVIDER` | XDS/CA 루트 인증서 결정 | istiod |
| `XDS_ROOT_CA` | XDS 루트 CA 경로 | (자동 감지) |
| `CA_ROOT_CA` | CA 루트 CA 경로 | (자동 감지) |

### 13.2 루트 CA 결정 로직

```go
// pkg/istio-agent/agent.go
func (a *Agent) FindRootCAForXDS() (string, error) {
    // 1. SYSTEM -> 시스템 루트 인증서 사용
    if a.cfg.XDSRootCerts == security.SystemRootCerts { return "", nil }

    // 2. XDS_ROOT_CA 명시 -> 해당 경로
    if a.cfg.XDSRootCerts != "" { return a.cfg.XDSRootCerts, nil }

    // 3. /etc/certs/root-cert.pem 존재 -> 레거시 마운트
    if fileExists(security.DefaultRootCertFilePath) {
        return security.DefaultRootCertFilePath, nil
    }

    // 4. PROV_CERT -> VM용 인증서
    if a.secOpts.ProvCert != "" {
        return a.secOpts.ProvCert + "/root-cert.pem", nil
    }

    // 5. FILE_MOUNTED_CERTS -> ProxyMetadata에서
    if a.secOpts.FileMountedCerts {
        return a.proxyConfig.ProxyMetadata[MetadataClientRootCert], nil
    }

    // 6. 기본: istio-ca-root-cert ConfigMap 마운트
    return path.Join(CitadelCACertPath,
        constants.CACertNamespaceConfigMapDataName), nil
    // -> "./var/run/secrets/istio/root-cert.pem"
}
```

### 13.3 CA 클라이언트 생성

```go
// pkg/istio-agent/plugins.go
func createCAClient(opts *security.Options, a RootCertProvider) (
    security.Client, error) {
    provider, ok := providers[opts.CAProviderName]
    if !ok {
        return nil, fmt.Errorf("CA provider %q not registered",
            opts.CAProviderName)
    }
    return provider(opts, a)
}

// 기본 Citadel CA 클라이언트
func createCitadel(opts *security.Options, a RootCertProvider) (
    security.Client, error) {
    // 포트 15010 = TLS 없음 (디버그/보안 네트워크)
    if strings.HasSuffix(opts.CAEndpoint, ":15010") {
        // TLS 없이 연결
    } else {
        // TLS 설정: 루트 CA + 클라이언트 인증서 (있는 경우)
        tlsOpts.RootCert, _ = a.FindRootCAForCA()
        tlsOpts.Key, tlsOpts.Cert = a.GetKeyCertsForCA()
    }
    return citadel.NewCitadelClient(opts, tlsOpts)
}
```

### 13.4 트러블슈팅 체크리스트

**인증서가 제공되지 않는 경우:**

1. SDS 소켓 존재 확인: `ls -la /var/run/secrets/workload-spiffe-uds/socket`
2. Agent 로그에서 SDS 서버 시작 확인: `"Starting SDS server for workload certificates"`
3. CA 연결 확인: `"CA Endpoint"` 로그 메시지
4. CSR 서명 실패: `"failed to sign"` 에러 메시지

**인증서 로테이션 실패:**

1. `"rotating certificate"` 로그 확인
2. `SECRET_GRACE_PERIOD_RATIO` 값 확인 (0에 가까우면 만료 직전에 갱신)
3. CA 서버 가용성 확인
4. `"slow generate secret lock"` 경고 -> 동시 요청 경합

**xDS 프록시 문제:**

1. UDS 소켓 확인: `ls -la {configPath}/XDS`
2. `"failed to connect to upstream"` -> Istiod 연결 실패
3. `"downstream terminated"` -> Envoy 연결 끊김
4. TLS 설정 확인: 루트 CA, 클라이언트 인증서 경로

### 13.5 메트릭

SecretManagerClient는 다음 메트릭을 노출한다:

| 메트릭 | 설명 |
|--------|------|
| `numOutgoingRequests` | CSR 요청 횟수 |
| `numFailedOutgoingRequests` | CSR 실패 횟수 |
| `outgoingLatency` | CSR 응답 시간 (ms) |
| `numFileSecretFailures` | 파일 인증서 읽기 실패 |
| `numFileWatcherFailures` | 파일 감시 추가 실패 |
| `certExpirySeconds` | 인증서 만료까지 남은 시간 |

xDS 프록시 메트릭:

| 메트릭 | 설명 |
|--------|------|
| `XdsProxyRequests` | Envoy -> Istiod 요청 수 |
| `XdsProxyResponses` | Istiod -> Envoy 응답 수 |
| `IstiodConnectionFailures` | Istiod 연결 실패 수 |
| `IstiodConnectionErrors` | Istiod 연결 에러 수 |
| `IstiodConnectionCancellations` | 정상 연결 종료 수 |

---

## 요약

Pilot Agent는 Istio의 데이터 플레인 보안과 설정 배포의 핵심 중간 계층이다.

| 기능 | 구현체 | 핵심 메커니즘 |
|------|--------|---------------|
| SDS (인증서 제공) | sds.Server + sdsservice | UDS gRPC, StreamSecrets |
| 인증서 관리 | SecretManagerClient | 인메모리 캐시, CSR/CA 연동, fsnotify |
| 인증서 로테이션 | queue.Delayed + rotateTime | 지터 포함 지연 큐, 콜백 체인 |
| xDS 프록싱 | XdsProxy | UDS 다운스트림, gRPC 업스트림, 핸들러 패턴 |
| VM 지원 | CredFetcher + OUTPUT_CERTS | JWT->mTLS 전환, 디스크 지속 |
| 부트스트랩 | bootstrap.New().CreateFile() | 노드 메타데이터 기반 JSON 생성 |
| 상태 관리 | StatusServer + Agent.Check() | liveness/readiness, DNS 준비 |

이 설계의 핵심 철학은 **관심사 분리**다. Envoy는 SDS/ADS 프로토콜만 알면 되고, Agent가 CA 연동, 인증서 로테이션, 파일 감시, VM 호환성 등 복잡한 로직을 모두 처리한다. 이를 통해 Envoy의 설정을 단순하게 유지하면서도 다양한 배포 환경(쿠버네티스, VM, 멀티클러스터)을 지원할 수 있다.
