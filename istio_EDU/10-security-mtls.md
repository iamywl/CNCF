# 10. Istio 보안 아키텍처와 mTLS Deep-Dive

## 목차

1. [개요](#1-개요)
2. [SPIFFE 아이덴티티](#2-spiffe-아이덴티티)
3. [CA 구현: IstioCA.Sign()](#3-ca-구현-istiocasign)
4. [Self-Signed vs Plugged-In CA](#4-self-signed-vs-plugged-in-ca)
5. [Root Cert 로테이션: SelfSignedCARootCertRotator](#5-root-cert-로테이션-selfsignedcarootcertrotator)
6. [SDS Server: Unix Domain Socket과 Delta 업데이트](#6-sds-server-unix-domain-socket과-delta-업데이트)
7. [SecretManagerClient: 캐시와 로테이션 큐](#7-secretmanagerclient-캐시와-로테이션-큐)
8. [인증서 로테이션: rotateTime 알고리즘](#8-인증서-로테이션-rotatetime-알고리즘)
9. [CitadelClient: CSR 서명 요청 흐름](#9-citadelclient-csr-서명-요청-흐름)
10. [PeerAuthentication: STRICT/PERMISSIVE/DISABLE 모드](#10-peerauthentication-strictpermissivedisable-모드)
11. [mTLS 핸드셰이크 전체 흐름](#11-mtls-핸드셰이크-전체-흐름)
12. [보안 설계 원칙](#12-보안-설계-원칙)

---

## 1. 개요

Istio의 보안 아키텍처는 서비스 메시 내 모든 워크로드 간 통신을 자동으로 암호화하고, 상호 인증(mutual TLS)을 수행하며, SPIFFE 기반의 강력한 아이덴티티 체계를 제공한다. 이 문서에서는 Istio 보안의 핵심 구성요소들을 소스코드 수준에서 분석한다.

### 보안 아키텍처 전체 구조

```
+------------------------------------------------------------------+
|                        Control Plane (istiod)                     |
|                                                                   |
|  +--------------------+    +---------------------------+          |
|  |   CA Server        |    |  Pilot (xDS 서버)          |          |
|  |   (gRPC)           |    |  - PeerAuthentication     |          |
|  |                    |    |  - DestinationRule        |          |
|  |  CreateCertificate |    |  - AuthorizationPolicy    |          |
|  |        |           |    +---------------------------+          |
|  |        v           |                                           |
|  |  Authenticator     |                                           |
|  |        |           |                                           |
|  |        v           |                                           |
|  |  IstioCA.Sign()    |                                           |
|  |        |           |                                           |
|  |        v           |                                           |
|  |  GenCertFromCSR    |                                           |
|  +--------------------+                                           |
+------------------------------------------------------------------+
         ^  |                                          |
    CSR  |  | Signed Cert                    xDS Config|
         |  v                                          v
+------------------------------------------------------------------+
|                       Data Plane (각 Pod)                         |
|                                                                   |
|  +----------------------------+   +--------------------------+    |
|  |  istio-agent (pilot-agent) |   |  Envoy Proxy (sidecar)   |    |
|  |                            |   |                          |    |
|  |  SecretManagerClient       |   |  Listener (inbound)      |    |
|  |    - CSR 생성              |<--|    - TLS Inspector        |    |
|  |    - 인증서 캐시           |   |    - mTLS filter chain   |    |
|  |    - 로테이션 스케줄링     |   |                          |    |
|  |         |                  |   |  Cluster (outbound)      |    |
|  |         v                  |   |    - UpstreamTlsContext  |    |
|  |  SDS Server (UDS)  ------>|-->|    - SPIFFE 검증          |    |
|  |                            |   |                          |    |
|  +----------------------------+   +--------------------------+    |
+------------------------------------------------------------------+
```

### 핵심 소스코드 위치

| 컴포넌트 | 소스 경로 |
|---------|----------|
| SPIFFE 아이덴티티 | `pkg/spiffe/spiffe.go` |
| IstioCA | `security/pkg/pki/ca/ca.go` |
| Root Cert 로테이터 | `security/pkg/pki/ca/selfsignedcarootcertrotator.go` |
| SDS Server | `security/pkg/nodeagent/sds/server.go`, `sdsservice.go` |
| SecretManagerClient | `security/pkg/nodeagent/cache/secretcache.go` |
| CitadelClient | `security/pkg/nodeagent/caclient/providers/citadel/client.go` |
| Security Options | `pkg/security/security.go` |
| CA gRPC Server | `security/pkg/server/ca/server.go` |
| PeerAuthentication | `pilot/pkg/security/authn/policy_applier.go` |
| MutualTLSMode | `pilot/pkg/model/authentication.go` |

---

## 2. SPIFFE 아이덴티티

### 2.1 SPIFFE란 무엇인가

SPIFFE(Secure Production Identity Framework For Everyone)는 분산 시스템에서 워크로드 아이덴티티를 표현하기 위한 표준이다. Istio는 SPIFFE를 핵심 아이덴티티 체계로 채택하여, 모든 워크로드에 URI 형식의 고유한 아이덴티티를 부여한다.

### 2.2 SPIFFE URI 형식

```
spiffe://<trust-domain>/ns/<namespace>/sa/<service-account>
```

- **trust-domain**: 신뢰 도메인. 기본값은 `cluster.local`
- **namespace**: Kubernetes 네임스페이스
- **service-account**: Kubernetes 서비스 어카운트

예시: `spiffe://cluster.local/ns/default/sa/bookinfo-productpage`

### 2.3 Identity 구조체

`pkg/spiffe/spiffe.go`에서 정의된 `Identity` 구조체는 SPIFFE URI를 파싱하고 생성하는 핵심 타입이다.

```go
// pkg/spiffe/spiffe.go

const (
    Scheme = "spiffe"
    URIPrefix    = Scheme + "://"
    URIPrefixLen = len(URIPrefix)

    ServiceAccountSegment = "sa"
    NamespaceSegment      = "ns"
)

type Identity struct {
    TrustDomain    string
    Namespace      string
    ServiceAccount string
}
```

### 2.4 ParseIdentity: URI 파싱

`ParseIdentity` 함수는 SPIFFE URI 문자열을 `Identity` 구조체로 변환한다. 정확히 5개 세그먼트(`trust-domain/ns/namespace/sa/service-account`)로 분할되어야 하며, `ns`와 `sa` 키워드가 올바른 위치에 있어야 한다.

```go
// pkg/spiffe/spiffe.go

func ParseIdentity(s string) (Identity, error) {
    if !strings.HasPrefix(s, URIPrefix) {
        return Identity{}, fmt.Errorf("identity is not a spiffe format")
    }
    split := strings.Split(s[URIPrefixLen:], "/")
    if len(split) != 5 {
        return Identity{}, fmt.Errorf("identity is not a spiffe format")
    }
    if split[1] != NamespaceSegment || split[3] != ServiceAccountSegment {
        return Identity{}, fmt.Errorf("identity is not a spiffe format")
    }
    return Identity{
        TrustDomain:    split[0],
        Namespace:      split[2],
        ServiceAccount: split[4],
    }, nil
}
```

### 2.5 Identity.String(): URI 생성

```go
func (i Identity) String() string {
    return URIPrefix + i.TrustDomain + "/ns/" + i.Namespace + "/sa/" + i.ServiceAccount
}
```

### 2.6 Trust Domain 확장

멀티클러스터 환경에서 trust domain alias를 지원하기 위해 `ExpandWithTrustDomains` 함수가 제공된다. 하나의 SPIFFE 아이덴티티를 여러 trust domain으로 확장하여, 서로 다른 클러스터의 워크로드가 상호 인증할 수 있게 한다.

```go
// pkg/spiffe/spiffe.go

func ExpandWithTrustDomains(spiffeIdentities sets.String,
    trustDomainAliases []string) sets.String {
    // 입력: {"spiffe://td1/ns/def/sa/def"}, aliases: {"td1", "td2"}
    // 출력: {"spiffe://td1/ns/def/sa/def", "spiffe://td2/ns/def/sa/def"}
    ...
}
```

### 2.7 PeerCertVerifier: 인증서의 SPIFFE 검증

`PeerCertVerifier`는 TLS 핸드셰이크에서 피어 인증서의 SPIFFE URI SAN을 추출하고, 해당 trust domain에 매핑된 루트 인증서 풀로 검증한다.

```go
// pkg/spiffe/spiffe.go

type PeerCertVerifier struct {
    generalCertPool *x509.CertPool
    certPools       map[string]*x509.CertPool  // trust domain -> cert pool
}

func (v *PeerCertVerifier) VerifyPeerCert(rawCerts [][]byte, _ [][]*x509.Certificate) error {
    // 1. 피어 인증서 파싱
    // 2. URI SAN에서 trust domain 추출
    trustDomain, err := GetTrustDomainFromURISAN(peerCert.URIs[0].String())
    // 3. trust domain에 해당하는 루트 인증서 풀로 검증
    rootCertPool, ok := v.certPools[trustDomain]
    _, err = peerCert.Verify(x509.VerifyOptions{
        Roots:         rootCertPool,
        Intermediates: intCertPool,
    })
    return err
}
```

**왜 trust domain별 cert pool을 유지하는가?** 멀티클러스터 환경에서 각 클러스터가 서로 다른 루트 CA를 사용할 수 있으므로, trust domain별로 독립된 인증서 풀을 관리해야 올바른 체인 검증이 가능하다.

---

## 3. CA 구현: IstioCA.Sign()

### 3.1 IstioCA 구조체

`security/pkg/pki/ca/ca.go`에서 정의된 `IstioCA`는 Istio의 핵심 인증기관(CA) 구현체이다.

```go
// security/pkg/pki/ca/ca.go

type IstioCA struct {
    defaultCertTTL time.Duration
    maxCertTTL     time.Duration
    caRSAKeySize   int

    keyCertBundle *util.KeyCertBundle

    // rootCertRotator: self-signed CA에서만 사용
    rootCertRotator *SelfSignedCARootCertRotator
}
```

### 3.2 Sign() 메서드: CSR 서명 전체 흐름

`Sign()`은 PEM 인코딩된 CSR을 받아 서명된 인증서를 반환하는 핵심 메서드이다.

```go
// security/pkg/pki/ca/ca.go

func (ca *IstioCA) Sign(csrPEM []byte, certOpts CertOpts) ([]byte, error) {
    return ca.sign(csrPEM, certOpts.SubjectIDs, certOpts.TTL, true, certOpts.ForCA)
}
```

내부 `sign()` 메서드의 상세 흐름:

```go
func (ca *IstioCA) sign(csrPEM []byte, subjectIDs []string,
    requestedLifetime time.Duration, checkLifetime, forCA bool) ([]byte, error) {

    // 1단계: CA 키/인증서 번들에서 서명 키 가져오기
    signingCert, signingKey, _, _ := ca.keyCertBundle.GetAll()
    if signingCert == nil {
        return nil, caerror.NewError(caerror.CANotReady,
            fmt.Errorf("Istio CA is not ready"))
    }

    // 2단계: CSR 파싱 및 서명 검증
    csr, err := util.ParsePemEncodedCSR(csrPEM)
    if err := csr.CheckSignature(); err != nil {
        return nil, caerror.NewError(caerror.CSRError, err)
    }

    // 3단계: TTL 결정
    lifetime := requestedLifetime
    if requestedLifetime.Seconds() <= 0 {
        lifetime = ca.defaultCertTTL
    }
    if checkLifetime && requestedLifetime.Seconds() > ca.maxCertTTL.Seconds() {
        return nil, caerror.NewError(caerror.TTLError, ...)
    }

    // 4단계: 인증서 생성
    certBytes, err := util.GenCertFromCSR(csr, signingCert,
        csr.PublicKey, *signingKey, subjectIDs, lifetime, forCA)

    // 5단계: PEM 인코딩
    block := &pem.Block{Type: "CERTIFICATE", Bytes: certBytes}
    cert := pem.EncodeToMemory(block)
    return cert, nil
}
```

### 3.3 Sign() 처리 흐름 다이어그램

```
CSR (PEM) 입력
    |
    v
+-------------------+
| ParsePemEncodedCSR|  CSR 파싱
+-------------------+
    |
    v
+-------------------+
| CheckSignature()  |  CSR 서명 유효성 검증
+-------------------+  (CSR을 생성한 주체가 개인키 소유자인지 확인)
    |
    v
+-------------------+
| TTL 결정          |
|  - 요청값 <= 0    |---> defaultCertTTL 사용
|  - 요청값 > max   |---> TTLError 반환
|  - 정상 범위      |---> 요청값 사용
+-------------------+
    |
    v
+-------------------+
| GenCertFromCSR()  |  X.509 인증서 생성
|  - signingCert    |  (CA 인증서로 서명)
|  - signingKey     |  (CA 개인키로 서명)
|  - subjectIDs     |  (SAN: SPIFFE URI)
|  - lifetime       |  (유효 기간)
+-------------------+
    |
    v
+-------------------+
| PEM Encode        |  DER -> PEM 변환
+-------------------+
    |
    v
서명된 인증서 (PEM) 반환
```

### 3.4 CertOpts: 인증서 옵션

```go
// security/pkg/pki/ca/ca.go

type CertOpts struct {
    SubjectIDs []string     // SAN (Subject Alternative Name) - SPIFFE URI
    TTL        time.Duration // 인증서 유효 기간
    ForCA      bool          // CA 인증서 여부
    CertSigner string        // 외부 인증서 서명자
}
```

### 3.5 minTTL: 인증서 체인 만료 시간 고려

IstioCA는 워크로드 인증서의 TTL이 CA 인증서 체인의 남은 유효 기간보다 길어지지 않도록 `minTTL`로 보호한다. 이 설계는 CA 인증서가 워크로드 인증서보다 먼저 만료되어 전체 체인이 무효화되는 것을 방지한다.

```go
func (ca *IstioCA) minTTL(defaultCertTTL time.Duration) (time.Duration, error) {
    certChainPem := ca.keyCertBundle.GetCertChainPem()
    if len(certChainPem) == 0 {
        return defaultCertTTL, nil
    }
    certChainExpiration, err := util.TimeBeforeCertExpires(certChainPem, time.Now())
    if defaultCertTTL > certChainExpiration {
        return certChainExpiration, nil  // CA 인증서 만료 시간으로 제한
    }
    return defaultCertTTL, nil
}
```

---

## 4. Self-Signed vs Plugged-In CA

### 4.1 CA 타입 열거형

```go
// security/pkg/pki/ca/ca.go

type caTypes int

const (
    selfSignedCA  caTypes = iota  // 자체 서명 CA
    pluggedCertCA                 // 외부 주입 CA
)
```

### 4.2 Self-Signed CA

자체 서명 CA는 Istio가 자체적으로 루트 인증서와 개인키를 생성하여 사용하는 방식이다. 주로 개발/테스트 환경이나 별도의 PKI가 없는 환경에서 사용한다.

**초기화 흐름 (`NewSelfSignedIstioCAOptions`):**

```
1. istio-ca-secret 시크릿 조회 시도
    |
    +---> 존재하면 로드하여 KeyCertBundle 생성
    |
    +---> 존재하지 않으면:
          |
          +---> useCacertsSecretName 활성화 시 cacerts 시크릿 조회
          |         |
          |         +---> 존재하면 로드
          |         +---> 존재하지 않으면 새로 생성
          |
          +---> 비활성화 시 istio-ca-secret 생성
                    |
                    v
              GenCertKeyFromOptions()
              (자체 서명 CA 인증서 + 개인키 생성)
                    |
                    v
              Kubernetes Secret에 저장 (지속성 확보)
```

**핵심 옵션:**

```go
options := util.CertOptions{
    TTL:          caCertTTL,       // CA 인증서 유효 기간 (기본 10년)
    Org:          org,             // 조직명
    IsCA:         true,            // CA 인증서 플래그
    IsSelfSigned: true,            // 자체 서명 플래그
    RSAKeySize:   caRSAKeySize,    // RSA 키 크기 (기본 2048)
    IsDualUse:    dualUse,         // 듀얼 유즈 (CA + 서버 인증서)
}
```

### 4.3 Plugged-In CA (외부 CA)

운영 환경에서는 기업의 기존 PKI 인프라에서 발급한 중간 CA 인증서를 Istio에 주입하여 사용한다. 이를 통해 Istio의 루트 CA를 기업의 신뢰 체인에 연결할 수 있다.

```go
// security/pkg/pki/ca/ca.go

func NewPluggedCertIstioCAOptions(fileBundle SigningCAFileBundle, ...) (*IstioCAOptions, error) {
    caOpts = &IstioCAOptions{
        CAType:         pluggedCertCA,
        DefaultCertTTL: defaultCertTTL,
        MaxCertTTL:     maxCertTTL,
    }

    // 파일에서 인증서 번들 로드
    caOpts.KeyCertBundle, err = util.NewVerifiedKeyCertBundleFromFile(
        fileBundle.SigningCertFile,   // 서명용 인증서
        fileBundle.SigningKeyFile,    // 서명용 개인키
        fileBundle.CertChainFiles,   // 인증서 체인
        fileBundle.RootCertFile,     // 루트 인증서
        fileBundle.CRLFile,          // CRL (인증서 폐기 목록)
    )

    // 서명 인증서가 CA인지 검증
    cert, err := x509.ParseCertificate(block.Bytes)
    if !cert.IsCA {
        return nil, fmt.Errorf("certificate is not authorized to sign other certificates")
    }
    return caOpts, nil
}
```

**Plugged-In CA에서 필요한 파일:**

```go
type SigningCAFileBundle struct {
    RootCertFile    string    // 루트 CA 인증서
    CertChainFiles  []string  // 중간 CA 체인
    SigningCertFile string    // 서명용 인증서 (중간 CA)
    SigningKeyFile  string    // 서명용 개인키
    CRLFile         string    // 인증서 폐기 목록 (선택)
}
```

### 4.4 Self-Signed vs Plugged-In 비교

| 항목 | Self-Signed CA | Plugged-In CA |
|------|---------------|---------------|
| 루트 인증서 | Istio 자체 생성 | 외부 PKI에서 제공 |
| 신뢰 체인 | 독립적 | 기업 PKI에 연결 |
| 루트 로테이션 | `SelfSignedCARootCertRotator` | 외부 관리 |
| Kubernetes Secret | `istio-ca-secret` 또는 `cacerts` | `cacerts` |
| 사용 환경 | 개발/테스트 | 프로덕션 |
| 설정 복잡도 | 낮음 (자동 설정) | 높음 (인증서 준비 필요) |
| `caTypes` 값 | `selfSignedCA (0)` | `pluggedCertCA (1)` |

---

## 5. Root Cert 로테이션: SelfSignedCARootCertRotator

### 5.1 왜 루트 인증서 로테이션이 필요한가

자체 서명 CA의 루트 인증서도 만료 시간이 있다. 루트 인증서가 만료되면 해당 CA로 서명된 모든 워크로드 인증서가 무효화되므로, 만료 전에 새로운 루트 인증서를 생성하고 배포해야 한다.

### 5.2 SelfSignedCARootCertRotator 구조체

```go
// security/pkg/pki/ca/selfsignedcarootcertrotator.go

type SelfSignedCARootCertRotator struct {
    caSecretController *controller.CaSecretController
    config             *SelfSignedCARootCertRotatorConfig
    backOffTime        time.Duration
    ca                 *IstioCA
    onRootCertUpdate   func() error
}

type SelfSignedCARootCertRotatorConfig struct {
    certInspector      certutil.CertUtil
    caStorageNamespace string
    org                string
    rootCertFile       string
    secretName         string            // "istio-ca-secret" 또는 "cacerts"
    client             corev1.CoreV1Interface
    CheckInterval      time.Duration     // 검사 주기
    caCertTTL          time.Duration     // CA 인증서 TTL
    retryInterval      time.Duration
    retryMax           time.Duration
    dualUse            bool
    enableJitter       bool              // 지터 활성화 여부
}
```

### 5.3 Run() 메서드: 주기적 검사 루프

```go
// security/pkg/pki/ca/selfsignedcarootcertrotator.go

func (rotator *SelfSignedCARootCertRotator) Run(stopCh chan struct{}) {
    // Jitter가 활성화된 경우 랜덤 대기 후 시작
    // (멀티 istiod 레플리카에서 동시 로테이션 방지)
    if rotator.config.enableJitter {
        select {
        case <-time.After(rotator.backOffTime):
        case <-stopCh:
            return
        }
    }

    // 주기적 검사 타이머
    ticker := time.NewTicker(rotator.config.CheckInterval)
    for {
        select {
        case <-ticker.C:
            rotator.checkAndRotateRootCert()
        case _, ok := <-stopCh:
            if !ok {
                ticker.Stop()
                return
            }
        }
    }
}
```

### 5.4 checkAndRotateRootCertForSigningCertCitadel: 로테이션 판단 로직

이 메서드는 루트 인증서의 만료 시점을 확인하고 로테이션 여부를 결정한다. 핵심 판단 기준은 `certInspector.GetWaitTime()`이 반환하는 대기 시간이다.

```
루트 인증서 만료 검사 흐름
    |
    v
+----------------------------+
| GetWaitTime(caCert, now)   |
|   waitTime > 0 ?           |
+----------------------------+
    |            |
    | Yes        | No (만료 임박)
    v            v
+----------+  +-----------------------------+
| 로테이션  |  | 로테이션 수행                |
| 불필요   |  |  1. 기존 cert 옵션 추출      |
|          |  |  2. 기존 개인키로 새 cert 생성|
| (단, 메모 |  |  3. Secret 업데이트          |
|  리와     |  |  4. KeyCertBundle 업데이트   |
|  Secret의 |  |  5. onRootCertUpdate 콜백   |
|  cert이   |  +-----------------------------+
|  다르면   |
|  리로드)  |
+----------+
```

### 5.5 기존 키 재사용 전략

**왜 기존 개인키를 재사용하는가?** 로테이션 시 새 루트 인증서를 기존 개인키(`SignerPrivPem`)로 서명함으로써, 이전 인증서로 서명된 워크로드 인증서들과의 호환성을 유지한다. 워크로드 인증서의 서명을 검증할 때 동일한 공개키를 사용하므로, 루트 인증서가 교체되더라도 기존 워크로드 인증서가 즉시 무효화되지 않는다.

```go
options := util.CertOptions{
    TTL:           rotator.config.caCertTTL,
    SignerPrivPem: caSecret.Data[CAPrivateKeyFile],  // 기존 개인키 재사용
    Org:           rotator.config.org,
    IsCA:          true,
    IsSelfSigned:  true,
    RSAKeySize:    rotator.ca.caRSAKeySize,
    IsDualUse:     rotator.config.dualUse,
}
options = util.MergeCertOptions(options, oldCertOptions)
pemCert, pemKey, ckErr := util.GenRootCertFromExistingKey(options)
```

### 5.6 롤백 메커니즘

`updateRootCertificate`는 원자적이지 않은 다단계 업데이트(Secret -> KeyCertBundle -> ConfigMap)를 수행하므로, 실패 시 롤백 메커니즘을 제공한다.

```go
func (rotator *SelfSignedCARootCertRotator) updateRootCertificate(
    caSecret *v1.Secret, rollForward bool,
    cert, key, rootCert []byte) (bool, error) {

    // 1단계: K8s Secret 업데이트
    caSecret.Data[CACertFile] = cert
    caSecret.Data[CAPrivateKeyFile] = key
    err = rotator.caSecretController.UpdateCASecretWithRetry(caSecret, ...)

    // 2단계: 메모리 KeyCertBundle 업데이트
    err := rotator.ca.GetCAKeyCertBundle().VerifyAndSetAll(cert, key, nil, rootCert, nil)
    if err != nil && rollForward {
        return true, err  // 롤백 필요
    }

    // 3단계: 콜백 호출
    if rotator.onRootCertUpdate != nil {
        _ = rotator.onRootCertUpdate()
    }
    return false, nil
}
```

호출부에서의 롤백 처리:

```go
if rollback, err := rotator.updateRootCertificate(caSecret, true, pemCert, pemKey, pemRootCerts); err != nil {
    if rollback {
        // 롤포워드 실패 -> 이전 인증서로 롤백
        _, err = rotator.updateRootCertificate(nil, false, oldCaCert, oldCaPrivateKey, oldRootCerts)
    }
}
```

---

## 6. SDS Server: Unix Domain Socket과 Delta 업데이트

### 6.1 SDS란 무엇인가

SDS(Secret Discovery Service)는 Envoy의 xDS API 중 하나로, 인증서와 개인키를 동적으로 제공하는 프로토콜이다. Istio의 istio-agent는 SDS 서버를 구현하여, Envoy 사이드카에 인증서를 제공한다.

### 6.2 Server 구조체

```go
// security/pkg/nodeagent/sds/server.go

const (
    maxStreams    = 100000
    maxRetryTimes = 5
)

type Server struct {
    workloadSds          *sdsservice
    grpcWorkloadListener net.Listener
    grpcWorkloadServer   *grpc.Server
    stopped              *atomic.Bool
}
```

### 6.3 NewServer: 서버 생성과 사전 워밍

```go
// security/pkg/nodeagent/sds/server.go

func NewServer(options *security.Options,
    workloadSecretCache security.SecretManager,
    pkpConf *mesh.PrivateKeyProvider) *Server {

    s := &Server{stopped: atomic.NewBool(false)}
    s.workloadSds = newSDSService(workloadSecretCache, options, pkpConf)
    s.initWorkloadSdsService(options)
    return s
}
```

### 6.4 Unix Domain Socket (UDS) 기반 통신

SDS 서버는 TCP가 아닌 UDS를 통해 통신한다. 이는 같은 Pod 내에서만 접근 가능하므로 네트워크 노출 없이 안전하게 인증서를 전달할 수 있다.

```go
// security/pkg/nodeagent/sds/server.go

func (s *Server) initWorkloadSdsService(opts *security.Options) {
    s.grpcWorkloadServer = grpc.NewServer(s.grpcServerOptions()...)
    s.workloadSds.register(s.grpcWorkloadServer)

    // UDS 소켓 경로 결정
    path := security.GetIstioSDSServerSocketPath()
    // 기본값: "./var/run/secrets/workload-spiffe-uds/socket"

    s.grpcWorkloadListener, err = uds.NewListener(path)
    go func() {
        // 재시도 루프 (최대 5회)
        for i := 0; i < maxRetryTimes; i++ {
            if err = s.grpcWorkloadServer.Serve(s.grpcWorkloadListener); err != nil {
                time.Sleep(waitTime)
                waitTime *= 2
            }
        }
    }()
}
```

**UDS 소켓 경로:**

```go
// pkg/security/security.go

const (
    WorkloadIdentityPath                 = "./var/run/secrets/workload-spiffe-uds"
    DefaultWorkloadIdentitySocketFile    = "socket"
)

func GetIstioSDSServerSocketPath() string {
    return filepath.Join(WorkloadIdentityPath, DefaultWorkloadIdentitySocketFile)
}
```

### 6.5 사전 워밍 (Pre-warming)

SDS 서비스는 생성 시 워크로드 인증서와 루트 인증서를 사전 생성하여, Envoy가 처음 요청할 때 지연 없이 응답할 수 있도록 한다.

```go
// security/pkg/nodeagent/sds/sdsservice.go

func newSDSService(st security.SecretManager, options *security.Options, ...) *sdsservice {
    ret := &sdsservice{
        st:      st,
        stop:    make(chan struct{}),
        clients: make(map[string]*Context),
    }

    if options.FileMountedCerts || options.ServeOnlyFiles {
        return ret  // 파일 기반이면 워밍 불필요
    }

    // 백그라운드 고루틴에서 인증서 사전 생성
    go func() {
        b := backoff.NewExponentialBackOff(backoff.DefaultOption())
        _ = b.RetryWithContext(ctx, func() error {
            // 워크로드 인증서 생성
            _, err := st.GenerateSecret(security.WorkloadKeyCertResourceName)
            // 루트 인증서 생성
            _, err = st.GenerateSecret(security.RootCertReqResourceName)
            return nil
        })
    }()
    return ret
}
```

### 6.6 StreamSecrets: xDS 스트리밍

Envoy와 SDS 서버 간의 통신은 gRPC 양방향 스트리밍으로 이루어진다. `StreamSecrets`가 진입점이며, 내부적으로 xDS 프레임워크를 활용한다.

```go
// security/pkg/nodeagent/sds/sdsservice.go

func (s *sdsservice) StreamSecrets(
    stream sds.SecretDiscoveryService_StreamSecretsServer) error {
    return xds.Stream(&Context{
        BaseConnection: xds.NewConnection("", stream),
        s:              s,
        w:              &Watch{},
    })
}
```

### 6.7 generate: 시크릿 리소스 생성

```go
func (s *sdsservice) generate(resourceNames []string) (*discovery.DiscoveryResponse, error) {
    resources := xds.Resources{}
    for _, resourceName := range resourceNames {
        secret, err := s.st.GenerateSecret(resourceName)
        res := protoconv.MessageToAny(toEnvoySecret(secret, s.rootCaPath, s.pkpConf))
        resources = append(resources, &discovery.Resource{
            Name:     resourceName,
            Resource: res,
        })
    }
    return &discovery.DiscoveryResponse{
        TypeUrl:     model.SecretType,
        VersionInfo: time.Now().Format(time.RFC3339) + "/" + strconv.FormatUint(version.Inc(), 10),
        Nonce:       uuid.New().String(),
        Resources:   xds.ResourcesToAny(resources),
    }, nil
}
```

### 6.8 Push 기반 업데이트

인증서가 갱신되면 SDS 서버는 연결된 모든 Envoy 클라이언트에 push 알림을 보낸다.

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

### 6.9 SDS 통신 전체 흐름

```
+------------------+     UDS      +------------------+     gRPC     +------------------+
|  Envoy Proxy     |<----------->| SDS Server        |<----------->| SecretManager     |
|                  |              | (sdsservice)      |              | Client (cache)    |
|  DiscoveryReq    |----(1)----->| Process()         |              |                   |
|  (resource:      |              |   |               |              |                   |
|   "default"      |              |   v               |              |                   |
|   or "ROOTCA")   |              | generate()        |----(2)----->| GenerateSecret()  |
|                  |              |   |               |              |   |               |
|                  |              |   v               |              |   v               |
|  DiscoveryRes    |<---(4)------| toEnvoySecret()   |<---(3)------| SecretItem        |
|  (tls.Secret)    |              |                   |              |                   |
|                  |              |                   |              |                   |
|  --- push ---    |              |                   |              |                   |
|  DiscoveryRes    |<---(6)------| push()            |<---(5)------| OnSecretUpdate()  |
|  (갱신된 cert)   |              |                   |              | (로테이션 트리거) |
+------------------+              +------------------+              +------------------+
```

---

## 7. SecretManagerClient: 캐시와 로테이션 큐

### 7.1 SecretManagerClient 개요

`SecretManagerClient`는 istio-agent의 인증서 관리 핵심 컴포넌트이다. CA로부터 CSR 서명을 받고, 인증서를 캐시하며, 만료 전 자동 로테이션을 스케줄링한다.

```go
// security/pkg/nodeagent/cache/secretcache.go

type SecretManagerClient struct {
    caClient      security.Client       // CA 클라이언트 (CitadelClient)
    configOptions *security.Options     // 설정 옵션
    secretHandler func(resourceName string) // 변경 콜백 (SDS push 트리거)

    cache         secretCache           // 인증서 캐시
    generateMutex sync.Mutex            // 동시 CSR 요청 방지

    existingCertificateFile security.SdsCertificateConfig // 파일 기반 인증서 경로

    certWatcher *fsnotify.Watcher       // 파일 변경 감시
    fileCerts   map[FileCert]struct{}   // 감시 중인 파일 인증서
    certMutex   sync.RWMutex

    queue       queue.Delayed           // 로테이션 지연 큐
    stop        chan struct{}
    caRootPath  string
}
```

### 7.2 secretCache: 스레드 세이프 캐시

```go
type secretCache struct {
    mu       sync.RWMutex
    workload *security.SecretItem  // 워크로드 인증서 캐시
    certRoot []byte                // 루트 인증서 캐시
}

func (s *secretCache) GetWorkload() *security.SecretItem {
    s.mu.RLock()
    defer s.mu.RUnlock()
    return s.workload
}

func (s *secretCache) SetWorkload(value *security.SecretItem) {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.workload = value
}
```

### 7.3 GenerateSecret: 인증서 생성 전체 흐름

```go
func (sc *SecretManagerClient) GenerateSecret(resourceName string) (*security.SecretItem, error) {
    // 1. 파일 기반 인증서 확인
    if sdsFromFile, ns, err := sc.generateFileSecret(resourceName); sdsFromFile {
        return ns, nil
    }

    // 2. 캐시 확인 (락 없이)
    ns := sc.getCachedSecret(resourceName)
    if ns != nil {
        return ns, nil
    }

    // 3. 락 획득 후 다시 캐시 확인 (double-check locking)
    sc.generateMutex.Lock()
    defer sc.generateMutex.Unlock()
    ns = sc.getCachedSecret(resourceName)
    if ns != nil {
        return ns, nil
    }

    // 4. 새 인증서 생성 (CSR -> CA 서명)
    ns, err = sc.generateNewSecret(resourceName)

    // 5. 캐시 저장 및 로테이션 스케줄링
    sc.registerSecret(*ns)

    // 6. 루트 인증서 변경 감지 시 push
    oldRoot := sc.cache.GetRoot()
    if !bytes.Equal(oldRoot, ns.RootCert) {
        sc.cache.SetRoot(ns.RootCert)
        sc.OnSecretUpdate(security.RootCertReqResourceName)
    }

    return ns, nil
}
```

### 7.4 generateNewSecret: CSR 생성부터 서명까지

```go
func (sc *SecretManagerClient) generateNewSecret(resourceName string) (*security.SecretItem, error) {
    // 1. SPIFFE 아이덴티티 구성
    csrHostName := &spiffe.Identity{
        TrustDomain:    sc.configOptions.TrustDomain,
        Namespace:      sc.configOptions.WorkloadNamespace,
        ServiceAccount: sc.configOptions.ServiceAccount,
    }

    // 2. CSR 생성 (개인키 + CSR PEM)
    options := pkiutil.CertOptions{
        Host:       csrHostName.String(),
        RSAKeySize: sc.configOptions.WorkloadRSAKeySize,
        PKCS8Key:   sc.configOptions.Pkcs8Keys,
        ECSigAlg:   pkiutil.SupportedECSignatureAlgorithms(sc.configOptions.ECCSigAlg),
    }
    csrPEM, keyPEM, err := pkiutil.GenCSR(options)

    // 3. CA에 CSR 서명 요청
    certChainPEM, err := sc.caClient.CSRSign(csrPEM,
        int64(sc.configOptions.SecretTTL.Seconds()))

    // 4. 신뢰 번들 가져오기
    trustBundlePEM, err = sc.caClient.GetRootCertBundle()

    // 5. SecretItem 생성
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

### 7.5 SecretItem: 인증서 캐시 항목

```go
// pkg/security/security.go

type SecretItem struct {
    CertificateChain []byte    // 인증서 체인 (PEM)
    PrivateKey       []byte    // 개인키 (PEM)
    RootCert         []byte    // 루트 인증서 (PEM)
    ResourceName     string    // "default" 또는 "ROOTCA"
    CreatedTime      time.Time // 생성 시간
    ExpireTime       time.Time // 만료 시간
}
```

### 7.6 파일 기반 인증서 지원

SecretManagerClient는 두 가지 모드를 동시에 지원한다:

```
인증서 소스 결정 로직
    |
    v
+---------------------------+
| resourceName == "ROOTCA"  |
| && 파일 존재?             |
+---------------------------+
    |           |
    | Yes       | No
    v           |
파일에서 로드   |
    |           v
    |   +---------------------------+
    |   | resourceName == "default" |
    |   | && key+cert 파일 존재?    |
    |   +---------------------------+
    |       |           |
    |       | Yes       | No
    |       v           v
    |   파일에서 로드   CA에 CSR 요청
    |
    v
파일 워처 등록
(fsnotify로 변경 감시)
```

파일 경로:
- 인증서: `./etc/certs/cert-chain.pem`
- 개인키: `./etc/certs/key.pem`
- 루트 인증서: `./etc/certs/root-cert.pem`

---

## 8. 인증서 로테이션: rotateTime 알고리즘

### 8.1 rotateTime 함수

`rotateTime`은 인증서 만료 전 로테이션 시점을 계산하는 핵심 함수이다. grace ratio와 jitter를 결합하여 대규모 클러스터에서의 CA 부하 집중을 방지한다.

```go
// security/pkg/nodeagent/cache/secretcache.go

var rotateTime = func(secret security.SecretItem,
    graceRatio float64, graceRatioJitter float64) time.Duration {

    // 1. jitter 계산 (랜덤 부호로 양/음 방향)
    jitter := (rand.Float64() * graceRatioJitter) * float64(rand.IntN(2)*2-1)
    jitterGraceRatio := graceRatio + jitter

    // 2. 경계값 클램핑
    if jitterGraceRatio > 1 { jitterGraceRatio = 1 }
    if jitterGraceRatio < 0 { jitterGraceRatio = 0 }

    // 3. 로테이션 시점 계산
    secretLifeTime := secret.ExpireTime.Sub(secret.CreatedTime)
    gracePeriod := time.Duration(jitterGraceRatio * float64(secretLifeTime))
    delay := time.Until(secret.ExpireTime.Add(-gracePeriod))

    if delay < 0 { delay = 0 }
    return delay
}
```

### 8.2 수학적 분석

기본 설정값:
- `SecretRotationGracePeriodRatio` = 0.5 (grace ratio)
- `SecretRotationGracePeriodRatioJitter` = 0.01 (jitter)

**계산 예시:**

```
인증서 TTL: 24시간 (86400초)
  생성 시간: 00:00
  만료 시간: 24:00

graceRatio = 0.5
jitter     = rand * 0.01 * (+-1)
           = [-0.01, +0.01] 범위의 랜덤값

jitterGraceRatio = 0.5 + jitter
                 = [0.49, 0.51] 범위

gracePeriod = jitterGraceRatio * 24h
            = [11.76h, 12.24h] 범위

rotateAt = expireTime - gracePeriod
         = 24:00 - [11.76h, 12.24h]
         = [11:46, 12:14] 범위

delay = rotateAt - now(00:00)
      = [11h46m, 12h14m]
```

### 8.3 시간 축 시각화

```
|-------- secretLifeTime (24h) --------|
|                                       |
생성(00:00)                        만료(24:00)
|                                       |
|<------ delay ------>|<-- gracePeriod -->|
|                     |                  |
|            rotateAt(~12:00)            |
|              +/- 14min jitter          |
```

### 8.4 왜 jitter를 사용하는가

대규모 클러스터(수천 개의 Pod)에서 모든 워크로드가 동시에 인증서를 갱신하면 CA 서버에 순간적인 과부하가 발생한다. jitter는 각 워크로드의 갱신 시점을 약간씩 다르게 만들어 이 부하를 분산시킨다.

```
jitter 없이 (모든 Pod이 동시에 갱신):

CA 부하
  ^
  |         |||||||||||||
  |         |||||||||||||
  |         |||||||||||||
  +---------+----+--------> 시간
          12:00

jitter 적용 후 (분산된 갱신):

CA 부하
  ^
  |      |  ||  ||| | ||  |
  |     || |||| ||| || ||
  |    ||| ||||||||| || |||
  +----+----+----+----+----> 시간
       11:46    12:00  12:14
```

### 8.5 registerSecret: 로테이션 스케줄링

```go
func (sc *SecretManagerClient) registerSecret(item security.SecretItem) {
    delay := rotateTime(item,
        sc.configOptions.SecretRotationGracePeriodRatio,
        sc.configOptions.SecretRotationGracePeriodRatioJitter)

    item.ResourceName = security.WorkloadKeyCertResourceName

    // 이미 스케줄된 로테이션이 있으면 중복 등록 방지
    if sc.cache.GetWorkload() != nil {
        return
    }

    sc.cache.SetWorkload(&item)

    // 지연 큐에 로테이션 작업 등록
    sc.queue.PushDelayed(func() error {
        if cached := sc.cache.GetWorkload(); cached != nil {
            if cached.CreatedTime == item.CreatedTime {
                // 캐시 클리어 -> 다음 GenerateSecret 호출 시 새 인증서 생성
                sc.cache.SetWorkload(nil)
                // SDS push 트리거 -> Envoy에 새 인증서 전달
                sc.OnSecretUpdate(item.ResourceName)
            }
        }
        return nil
    }, delay)
}
```

**왜 `CreatedTime`을 비교하는가?** `UpdateConfigTrustBundle` 호출 등으로 인해 워크로드 인증서가 이미 재서명된 경우, 이전에 스케줄된 로테이션 작업이 실행되면 안 된다. `CreatedTime` 비교를 통해 stale한 로테이션 작업을 무시한다.

---

## 9. CitadelClient: CSR 서명 요청 흐름

### 9.1 CitadelClient 구조체

```go
// security/pkg/nodeagent/caclient/providers/citadel/client.go

type CitadelClient struct {
    tlsOpts  *TLSOptions                       // TLS 연결 옵션
    client   pb.IstioCertificateServiceClient   // gRPC 클라이언트
    conn     *grpc.ClientConn                   // gRPC 연결
    provider credentials.PerRPCCredentials      // 인증 토큰 제공자
    opts     *security.Options                  // 보안 옵션
}
```

### 9.2 CSRSign: CSR 서명 요청

```go
func (c *CitadelClient) CSRSign(csrPEM []byte, certValidTTLInSec int64) ([]string, error) {
    // 1. 요청 구성
    crMetaStruct := &structpb.Struct{
        Fields: map[string]*structpb.Value{
            security.CertSigner: {
                Kind: &structpb.Value_StringValue{StringValue: c.opts.CertSigner},
            },
        },
    }
    req := &pb.IstioCertificateRequest{
        Csr:              string(csrPEM),
        ValidityDuration: certValidTTLInSec,
        Metadata:         crMetaStruct,
    }

    // 2. 실패 시 재연결 (defer)
    defer func() {
        if err != nil {
            c.reconnect()  // 루트 인증서 갱신 후 TLS 재설정
        }
    }()

    // 3. gRPC 호출 (ClusterID 메타데이터 포함)
    ctx := metadata.NewOutgoingContext(context.Background(),
        metadata.Pairs("ClusterID", c.opts.ClusterID))

    // 추가 헤더 설정
    for k, v := range c.opts.CAHeaders {
        ctx = metadata.AppendToOutgoingContext(ctx, k, v)
    }

    // 4. CreateCertificate RPC 호출
    resp, err := c.client.CreateCertificate(ctx, req)

    // 5. 응답 검증
    if len(resp.CertChain) <= 1 {
        return nil, errors.New("invalid empty CertChain")
    }

    return resp.CertChain, nil
}
```

### 9.3 CSR 서명 요청 전체 시퀀스

```
istio-agent                    istiod (CA Server)
    |                              |
    |  1. GenCSR(options)          |
    |  (개인키 + CSR 생성)         |
    |                              |
    |  2. CreateCertificate(req)   |
    |----------------------------->|
    |  - CSR (PEM)                 |  3. Authenticate(ctx)
    |  - ValidityDuration          |     (JWT 토큰 또는 mTLS 인증)
    |  - Metadata (CertSigner)     |
    |  - ClusterID                 |  4. caller.Identities 추출
    |                              |     (SPIFFE URI로 SAN 설정)
    |                              |
    |                              |  5. IstioCA.Sign(csrPEM, certOpts)
    |                              |     - CSR 파싱/검증
    |                              |     - TTL 결정
    |                              |     - GenCertFromCSR()
    |                              |     - PEM 인코딩
    |                              |
    |  6. IstioCertificateResponse |
    |<-----------------------------|
    |  - CertChain[]               |
    |    [0]: leaf cert             |
    |    [1]: intermediate cert     |
    |    [2]: root cert             |
    |                              |
    |  7. SecretItem 생성          |
    |  8. 캐시 저장                |
    |  9. 로테이션 스케줄링        |
    |  10. SDS push -> Envoy       |
    |                              |
```

### 9.4 서버 측 CreateCertificate 핸들러

```go
// security/pkg/server/ca/server.go

func (s *Server) CreateCertificate(ctx context.Context,
    request *pb.IstioCertificateRequest) (*pb.IstioCertificateResponse, error) {

    // 1. 인증
    caller, err := security.Authenticate(ctx, s.Authenticators)
    if caller == nil || err != nil {
        return nil, status.Error(codes.Unauthenticated, "request authenticate failure")
    }

    // 2. SAN 결정 (기본: caller의 아이덴티티)
    sans := caller.Identities

    // 3. 신원 위임(Impersonation) 처리
    impersonatedIdentity := crMetadata[security.ImpersonatedIdentity].GetStringValue()
    if impersonatedIdentity != "" {
        // nodeAuthorizer로 위임 권한 검증
        sans = []string{impersonatedIdentity}
    }

    // 4. CA로 서명
    certOpts := ca.CertOpts{
        SubjectIDs: sans,
        TTL:        time.Duration(request.ValidityDuration) * time.Second,
        ForCA:      false,
        CertSigner: certSigner,
    }
    cert, signErr = s.ca.Sign([]byte(request.Csr), certOpts)

    // 5. 응답 구성 (인증서 체인)
    respCertChain = []string{string(cert), string(certChainBytes), string(rootCertBytes)}
    return &pb.IstioCertificateResponse{CertChain: respCertChain}, nil
}
```

### 9.5 자동 재연결 메커니즘

루트 인증서가 갱신되면 기존 TLS 연결이 유효하지 않을 수 있다. CitadelClient는 CSR 서명 실패 시 자동으로 gRPC 연결을 재구축한다.

```go
func (c *CitadelClient) reconnect() error {
    c.conn.Close()
    conn, err := c.buildConnection()
    c.conn = conn
    c.client = pb.NewIstioCertificateServiceClient(conn)
    return nil
}
```

---

## 10. PeerAuthentication: STRICT/PERMISSIVE/DISABLE 모드

### 10.1 MutualTLSMode 열거형

```go
// pilot/pkg/model/authentication.go

type MutualTLSMode int

const (
    MTLSUnknown    MutualTLSMode = iota  // 미설정 (초기화 전)
    MTLSDisable                           // mTLS 비활성화
    MTLSPermissive                        // 허용 모드 (평문+mTLS)
    MTLSStrict                            // 강제 모드 (mTLS만 허용)
)
```

### 10.2 각 모드의 동작

```
+------------------+--------------------------------------------------+
|     모드         |     동작                                          |
+------------------+--------------------------------------------------+
| STRICT           | mTLS 필수. 평문 요청 거부.                       |
|                  | Envoy가 TLS만 허용하는 filter chain 구성.         |
|                  | 인증서 없는 클라이언트는 접속 불가.               |
+------------------+--------------------------------------------------+
| PERMISSIVE       | mTLS + 평문 모두 허용 (기본값).                   |
|                  | Envoy가 TLS Inspector로 프로토콜 자동 감지.       |
|                  | 메시 마이그레이션 기간에 사용.                    |
+------------------+--------------------------------------------------+
| DISABLE          | mTLS 비활성화. 평문만 사용.                       |
|                  | Envoy가 평문 filter chain만 구성.                |
|                  | 보안이 불필요한 내부 트래픽에 제한적 사용.        |
+------------------+--------------------------------------------------+
```

### 10.3 PeerAuthentication 정책 계층 구조

Istio의 PeerAuthentication 정책은 3단계 계층으로 적용된다:

```
+--------------------------------------------------+
|  Mesh-level (root namespace에 정의)               |
|  - 전체 메시에 적용되는 기본 정책                   |
|  - 예: istio-system 네임스페이스에 정의              |
+--------------------------------------------------+
          |
          v  (오버라이드)
+--------------------------------------------------+
|  Namespace-level (특정 네임스페이스에 정의)          |
|  - selector 없이 정의                              |
|  - 해당 네임스페이스의 모든 워크로드에 적용           |
+--------------------------------------------------+
          |
          v  (오버라이드)
+--------------------------------------------------+
|  Workload-level (selector로 특정 Pod 지정)          |
|  - selector.matchLabels로 대상 지정                |
|  - 포트별 mTLS 모드 지정 가능                       |
+--------------------------------------------------+
```

### 10.4 ComposePeerAuthentication: 정책 병합

`ComposePeerAuthentication` 함수는 3단계 정책을 병합하여 최종 적용할 mTLS 모드를 결정한다.

```go
// pilot/pkg/security/authn/policy_applier.go

func ComposePeerAuthentication(rootNamespace string,
    configs []*config.Config) MergedPeerAuthentication {

    var meshCfg, namespaceCfg, workloadCfg *config.Config

    // 기본값: PERMISSIVE
    outputPolicy := MergedPeerAuthentication{
        Mode: model.MTLSPermissive,
    }

    // 정책 분류
    for _, cfg := range configs {
        spec := cfg.Spec.(*v1beta1.PeerAuthentication)
        if spec.Selector == nil || len(spec.Selector.MatchLabels) == 0 {
            if cfg.Namespace == rootNamespace {
                meshCfg = cfg       // mesh-level
            } else {
                namespaceCfg = cfg  // namespace-level
            }
        } else if cfg.Namespace != rootNamespace {
            workloadCfg = cfg       // workload-level
        }
    }

    // 계층적 오버라이드 (mesh -> namespace -> workload)
    if meshCfg != nil {
        outputPolicy.Mode = model.ConvertToMutualTLSMode(
            meshCfg.Spec.(*v1beta1.PeerAuthentication).Mtls.Mode)
    }
    if namespaceCfg != nil {
        outputPolicy.Mode = model.ConvertToMutualTLSMode(
            namespaceCfg.Spec.(*v1beta1.PeerAuthentication).Mtls.Mode)
    }
    if workloadCfg != nil {
        // 워크로드 레벨에서는 포트별 모드도 지원
        ...
    }

    return outputPolicy
}
```

### 10.5 ConvertToMutualTLSMode: API 타입 변환

```go
// pilot/pkg/model/authentication.go

func ConvertToMutualTLSMode(
    mode v1beta1.PeerAuthentication_MutualTLS_Mode) MutualTLSMode {
    switch mode {
    case v1beta1.PeerAuthentication_MutualTLS_DISABLE:
        return MTLSDisable
    case v1beta1.PeerAuthentication_MutualTLS_PERMISSIVE:
        return MTLSPermissive
    case v1beta1.PeerAuthentication_MutualTLS_STRICT:
        return MTLSStrict
    default:
        return MTLSUnknown
    }
}
```

### 10.6 InboundMTLSSettings: Envoy 설정 생성

```go
// pilot/pkg/security/authn/policy_applier.go

func (a policyApplier) InboundMTLSSettings(
    endpointPort uint32,
    node *model.Proxy,
    trustDomainAliases []string,
    modeOverride model.MutualTLSMode) MTLSSettings {

    effectiveMTLSMode := modeOverride
    if effectiveMTLSMode == model.MTLSUnknown {
        effectiveMTLSMode = a.GetMutualTLSModeForPort(endpointPort)
    }

    minTLSVersion := authn_utils.GetMinTLSVersion(
        mc.GetMeshMTLS().GetMinProtocolVersion())

    return MTLSSettings{
        Port: endpointPort,
        Mode: effectiveMTLSMode,
        TCP:  authn_utils.BuildInboundTLS(effectiveMTLSMode, node,
            networking.ListenerProtocolTCP, trustDomainAliases, minTLSVersion, mc),
        HTTP: authn_utils.BuildInboundTLS(effectiveMTLSMode, node,
            networking.ListenerProtocolHTTP, trustDomainAliases, minTLSVersion, mc),
    }
}
```

### 10.7 PERMISSIVE 모드의 구현: TLS Inspector

PERMISSIVE 모드에서 Envoy는 TLS Inspector를 사용하여 들어오는 연결이 TLS인지 평문인지를 자동으로 감지한다.

```
클라이언트 연결
    |
    v
+-------------------+
| TLS Inspector     |  첫 바이트 검사
| (Envoy)           |  - 0x16 (TLS ClientHello) -> TLS filter chain
+-------------------+  - 기타 -> 평문 filter chain
    |          |
    |          |
    v          v
+--------+  +--------+
| mTLS   |  | 평문   |
| filter |  | filter |
| chain  |  | chain  |
+--------+  +--------+
```

---

## 11. mTLS 핸드셰이크 전체 흐름

### 11.1 전체 시퀀스

```
Service A (client)                                   Service B (server)
Envoy Proxy                                          Envoy Proxy
    |                                                    |
    |  --- TLS ClientHello ---------------------------->|
    |  (SNI: outbound_.80_._.service-b.ns.svc.cluster.local)
    |                                                    |
    |                                                    | [TLS Inspector]
    |                                                    | -> mTLS filter chain 선택
    |                                                    |
    |  <-- TLS ServerHello + Certificate + CertRequest --|
    |      (서버 인증서: SPIFFE URI SAN 포함)              |
    |      (서버가 클라이언트 인증서 요청)                  |
    |                                                    |
    |  [클라이언트 인증서 검증]                            |
    |  1. 서버 인증서의 URI SAN에서 trust domain 추출     |
    |  2. trust domain에 해당하는 root cert pool로 검증   |
    |  3. SPIFFE 아이덴티티 확인                          |
    |                                                    |
    |  --- Certificate + CertVerify + Finished -------->|
    |      (클라이언트 인증서: SPIFFE URI SAN 포함)        |
    |                                                    |
    |                                                    | [클라이언트 인증서 검증]
    |                                                    | 1. URI SAN에서 trust domain 추출
    |                                                    | 2. root cert pool로 체인 검증
    |                                                    | 3. SPIFFE 아이덴티티 확인
    |                                                    | 4. AuthorizationPolicy 적용
    |                                                    |
    |  <-- Finished ------------------------------------|
    |                                                    |
    |  ====== 암호화된 애플리케이션 데이터 양방향 전송 ======|
    |                                                    |
```

### 11.2 인증서 체인 검증 과정

```
Leaf Certificate (워크로드 인증서)
  |
  | URI SAN: spiffe://cluster.local/ns/default/sa/my-service
  | Issuer: Istio CA (중간 CA)
  |
  v
Intermediate CA Certificate (Istio CA 인증서)
  |
  | Subject: Istio CA
  | Issuer: Root CA
  |
  v
Root CA Certificate (루트 인증서)
  |
  | Subject: Root CA (self-signed 또는 외부 PKI)
  | trust domain별 cert pool에서 검증
  |
  v
[검증 완료] -> 통신 허용
```

### 11.3 Envoy Filter Chain 구조 (STRICT 모드)

```
Listener (inbound)
  |
  +--- Filter Chain (mTLS)
  |      |
  |      +--- DownstreamTlsContext
  |      |      |
  |      |      +--- CommonTlsContext
  |      |      |      +--- TlsCertificateSdsSecretConfigs
  |      |      |      |      +--- name: "default" (SDS로 워크로드 cert 가져옴)
  |      |      |      |
  |      |      |      +--- ValidationContextSdsSecretConfig
  |      |      |      |      +--- name: "ROOTCA" (SDS로 root cert 가져옴)
  |      |      |      |
  |      |      |      +--- AlpnProtocols: ["istio-peer-exchange", "h2", "http/1.1"]
  |      |      |
  |      |      +--- RequireClientCertificate: true (mTLS 강제)
  |      |
  |      +--- Filters
  |             +--- HTTP Connection Manager
  |             +--- 인증/인가 필터
  |
  +--- (PERMISSIVE인 경우) Filter Chain (평문)
         +--- Filters
                +--- TCP Proxy 또는 HTTP Connection Manager
```

### 11.4 Outbound mTLS 설정 (UpstreamTlsContext)

클라이언트 측 Envoy는 DestinationRule과 PeerAuthentication 정책에 따라 outbound 트래픽의 mTLS를 설정한다.

```
Cluster (outbound)
  |
  +--- UpstreamTlsContext
         |
         +--- CommonTlsContext
         |      +--- TlsCertificateSdsSecretConfigs
         |      |      +--- name: "default" (클라이언트 인증서)
         |      |
         |      +--- ValidationContextSdsSecretConfig
         |             +--- name: "ROOTCA" (서버 인증서 검증용)
         |
         +--- Sni: outbound_.80_._.service-b.ns.svc.cluster.local
```

---

## 12. 보안 설계 원칙

### 12.1 Zero Trust 아키텍처

Istio의 보안 설계는 Zero Trust 원칙을 따른다:

| 원칙 | Istio 구현 |
|------|-----------|
| 네트워크를 신뢰하지 않음 | 모든 서비스 간 통신을 mTLS로 암호화 |
| 항상 인증 | SPIFFE 기반 상호 인증 (양쪽 모두 인증서 제시) |
| 최소 권한 | AuthorizationPolicy로 세밀한 접근 제어 |
| 지속적 검증 | 인증서 자동 로테이션, 만료 전 갱신 |

### 12.2 인증서 관리 보안 설계

**1. 개인키는 노드를 떠나지 않는다**

워크로드의 개인키는 istio-agent에서 생성되어 CSR만 CA에 전송된다. 개인키 자체는 네트워크를 통해 전송되지 않는다.

```
istio-agent                    istiod (CA)
    |                              |
    | GenCSR() -> 개인키 + CSR     |
    | (개인키는 로컬에 보관)        |
    |                              |
    | CSR만 전송 ----------------->|
    |                              | Sign(CSR)
    | <-- 서명된 인증서 반환 ------|
    |                              |
    | 개인키 + 인증서 -> Envoy     |
    | (UDS로 같은 Pod 내에서만)     |
```

**2. UDS를 통한 인증서 전달**

SDS 서버는 Unix Domain Socket을 사용하여 같은 Pod 내의 Envoy에만 인증서를 전달한다. 네트워크를 통한 인증서 유출 위험이 없다.

**3. 인증서 수명 관리**

```
인증서 수명 계층:
    |
    +--- Root CA: 10년 (자체 서명) 또는 외부 PKI에 따름
    |     |
    |     +--- 만료 전 자동 로테이션 (SelfSignedCARootCertRotator)
    |     +--- 기존 키 재사용으로 호환성 유지
    |
    +--- Workload Cert: 24시간 (기본, SecretTTL)
          |
          +--- 50% 수명에서 갱신 (grace ratio)
          +--- +/- 1% jitter로 부하 분산
          +--- 갱신 실패 시 지수 백오프 재시도
```

**4. 인증 체인**

```
요청 인증 흐름:

CSR 요청 도착
    |
    v
+----------------------------+
| Authenticator 체인         |
|  1. ClientCertAuthenticator|  mTLS 인증서 기반
|  2. KubeJWTAuthenticator   |  Kubernetes JWT 기반
|  3. XDSAuthenticator       |  xDS 토큰 기반
+----------------------------+
    |
    | (하나라도 성공하면 통과)
    v
+----------------------------+
| Caller 추출               |
|  - AuthSource              |
|  - Identities (SPIFFE URI) |
|  - KubernetesInfo          |
+----------------------------+
    |
    v
+----------------------------+
| 인증서 서명                |
|  SAN = caller.Identities   |
|  (서버가 아이덴티티 결정)    |
+----------------------------+
```

### 12.3 보안 감사 지점

| 이벤트 | 로그 스코프 | 주요 로그 |
|--------|-----------|----------|
| CSR 서명 요청 | `serverCaLog` | `generating a certificate, sans: %v` |
| 인증 실패 | `securityLog` | `request authenticate failure` |
| 루트 인증서 로테이션 | `rootcertrotator` | `Root certificate rotation is completed` |
| 인증서 캐시 갱신 | `cache` | `generated new workload certificate` |
| SDS push | `sds` | `Trigger on secret update` |
| 신원 위임 | `serverCaLog` | `impersonation failed for identity` |

### 12.4 보안 옵션 요약

```go
// pkg/security/security.go

type Options struct {
    CAEndpoint       string         // CA 엔드포인트
    TrustDomain      string         // SPIFFE trust domain
    WorkloadRSAKeySize int          // RSA 키 크기 (기본 2048)
    Pkcs8Keys        bool           // PKCS#8 형식 사용 여부
    SecretTTL        time.Duration  // 인증서 TTL
    SecretRotationGracePeriodRatio        float64 // 갱신 비율 (0.5)
    SecretRotationGracePeriodRatioJitter  float64 // 지터 (0.01)
    FileMountedCerts bool           // 파일 마운트 인증서 사용
    PilotCertProvider string        // Pilot 인증서 제공자
    ECCSigAlg        string         // ECC 서명 알고리즘
    ECCCurve         string         // ECC 커브
}
```

### 12.5 위협 모델과 대응

```
+-------------------------------------------+-------------------------------------+
| 위협                                      | Istio의 대응                        |
+-------------------------------------------+-------------------------------------+
| 네트워크 스니핑                            | mTLS로 모든 트래픽 암호화           |
| 서비스 사칭 (Spoofing)                     | SPIFFE 기반 상호 인증               |
| Man-in-the-Middle 공격                    | 인증서 체인 검증, trust domain 분리 |
| 인증서 도난                                | 짧은 TTL (24h), 자동 로테이션       |
| CA 인증서 만료                             | SelfSignedCARootCertRotator         |
| CA 과부하 (인증서 갱신 폭주)               | jitter, 지수 백오프                 |
| 개인키 유출                                | 키가 노드를 떠나지 않음, UDS 전달   |
| 비인가 인증서 발급                         | JWT/mTLS 인증, Impersonation 검증  |
| 인증서 폐기                                | CRL 지원 (EnableCACRL)             |
+-------------------------------------------+-------------------------------------+
```

---

## 참고: 핵심 소스 파일 요약

| 파일 | 주요 구조체/함수 | 역할 |
|------|----------------|------|
| `pkg/spiffe/spiffe.go` | `Identity`, `ParseIdentity()`, `PeerCertVerifier` | SPIFFE 아이덴티티 파싱, 생성, 검증 |
| `security/pkg/pki/ca/ca.go` | `IstioCA`, `Sign()`, `sign()`, `CertOpts` | CA 구현, CSR 서명 |
| `security/pkg/pki/ca/selfsignedcarootcertrotator.go` | `SelfSignedCARootCertRotator`, `Run()`, `checkAndRotateRootCertForSigningCertCitadel()` | 루트 인증서 자동 로테이션 |
| `security/pkg/nodeagent/sds/server.go` | `Server`, `NewServer()`, `initWorkloadSdsService()` | SDS gRPC 서버 (UDS) |
| `security/pkg/nodeagent/sds/sdsservice.go` | `sdsservice`, `generate()`, `StreamSecrets()`, `toEnvoySecret()` | SDS 서비스 로직, Envoy 시크릿 변환 |
| `security/pkg/nodeagent/cache/secretcache.go` | `SecretManagerClient`, `GenerateSecret()`, `rotateTime()`, `registerSecret()` | 인증서 캐시, 로테이션 스케줄링 |
| `security/pkg/nodeagent/caclient/providers/citadel/client.go` | `CitadelClient`, `CSRSign()`, `reconnect()` | istiod CA에 CSR 서명 요청 |
| `pkg/security/security.go` | `Options`, `SecretItem`, `SecretManager`, `Authenticator` | 보안 설정, 인터페이스 정의 |
| `security/pkg/server/ca/server.go` | `Server.CreateCertificate()` | CA gRPC 서버 핸들러 |
| `pilot/pkg/security/authn/policy_applier.go` | `ComposePeerAuthentication()`, `InboundMTLSSettings()` | PeerAuthentication 정책 병합 |
| `pilot/pkg/model/authentication.go` | `MutualTLSMode`, `ConvertToMutualTLSMode()` | mTLS 모드 열거형, 변환 |
