# 12. Credentials (인증/보안) 서브시스템 심화 분석

## 목차

1. [개요](#1-개요)
2. [핵심 인터페이스 아키텍처](#2-핵심-인터페이스-아키텍처)
3. [TransportCredentials 인터페이스](#3-transportcredentials-인터페이스)
4. [PerRPCCredentials 인터페이스](#4-perrpccredentials-인터페이스)
5. [AuthInfo 인터페이스와 SecurityLevel](#5-authinfo-인터페이스와-securitylevel)
6. [Bundle 인터페이스](#6-bundle-인터페이스)
7. [TLS 구현](#7-tls-구현)
8. [Insecure 구현](#8-insecure-구현)
9. [Local 구현](#9-local-구현)
10. [OAuth2 구현](#10-oauth2-구현)
11. [ALTS 개요](#11-alts-개요)
12. [Google Default Credentials Bundle](#12-google-default-credentials-bundle)
13. [서버 핸드셰이크 흐름](#13-서버-핸드셰이크-흐름)
14. [클라이언트 핸드셰이크 흐름](#14-클라이언트-핸드셰이크-흐름)
15. [보안 레벨 체크 메커니즘](#15-보안-레벨-체크-메커니즘)
16. [내부 유틸리티](#16-내부-유틸리티)
17. [설계 원칙과 Why](#17-설계-원칙과-why)

---

## 1. 개요

gRPC-Go의 credentials 패키지는 **전송 계층 보안(Transport Security)**과 **RPC별 인증(Per-RPC Authentication)**을 분리된 인터페이스로 추상화한다. 이 설계는 TLS, ALTS, mTLS 같은 전송 보안 프로토콜과 OAuth2, JWT 같은 인증 토큰 메커니즘을 독립적으로 조합할 수 있게 한다.

```
소스 경로: credentials/credentials.go
패키지 선언: package credentials // import "google.golang.org/grpc/credentials"
```

### 왜 이런 분리가 필요한가?

전송 보안과 RPC 인증은 근본적으로 다른 계층의 관심사다:

| 구분 | TransportCredentials | PerRPCCredentials |
|------|---------------------|-------------------|
| 적용 시점 | 연결 수립 시 (1회) | 매 RPC 호출마다 |
| 담당 역할 | 연결 암호화, 서버 신원 검증 | 호출자 신원 증명 (토큰) |
| 프로토콜 예 | TLS, ALTS, insecure | OAuth2, JWT, API Key |
| 수명 | 연결 전체 | 단일 RPC |

이 두 가지를 하나의 인터페이스로 합치면, TLS + OAuth2, ALTS + JWT 같은 조합을 만들 때마다 새로운 구현이 필요해진다. 분리함으로써 `N개의 전송 보안 x M개의 인증 방식 = N*M 조합`을 N+M개의 구현만으로 달성한다.

### 전체 아키텍처 다이어그램

```
                        credentials 패키지 아키텍처
 ┌─────────────────────────────────────────────────────────────────┐
 │                    credentials.Bundle                           │
 │         (TransportCredentials + PerRPCCredentials 결합)          │
 │                                                                  │
 │  ┌──────────────────────────┐  ┌──────────────────────────────┐ │
 │  │  TransportCredentials    │  │    PerRPCCredentials          │ │
 │  │  (전송 계층 보안)          │  │    (RPC별 인증)               │ │
 │  │                          │  │                              │ │
 │  │  ┌──────────────────┐    │  │  ┌────────────────────┐      │ │
 │  │  │ ClientHandshake  │    │  │  │ GetRequestMetadata │      │ │
 │  │  │ ServerHandshake  │    │  │  │ RequireTransport   │      │ │
 │  │  │ Info / Clone     │    │  │  │   Security         │      │ │
 │  │  └──────────────────┘    │  │  └────────────────────┘      │ │
 │  │                          │  │                              │ │
 │  │  구현체:                  │  │  구현체:                      │ │
 │  │  - tlsCreds (TLS)        │  │  - oauth.TokenSource         │ │
 │  │  - insecureTC            │  │  - oauth.jwtAccess           │ │
 │  │  - localTC               │  │  - oauth.serviceAccount      │ │
 │  │  - altsTC (ALTS)         │  │  - oauth.oauthAccess         │ │
 │  └──────────────────────────┘  └──────────────────────────────┘ │
 │                                                                  │
 │  AuthInfo (핸드셰이크 결과)                                       │
 │  ┌──────────────────────────────────────────────────────────┐   │
 │  │ CommonAuthInfo { SecurityLevel }                         │   │
 │  │ - TLSInfo      (SecurityLevel: PrivacyAndIntegrity)      │   │
 │  │ - insecure.info(SecurityLevel: NoSecurity)               │   │
 │  │ - local.info   (SecurityLevel: 연결 유형에 따라 동적)       │   │
 │  │ - alts.AuthInfo(ALTS 전용 보안 정보)                      │   │
 │  └──────────────────────────────────────────────────────────┘   │
 └─────────────────────────────────────────────────────────────────┘
```

---

## 2. 핵심 인터페이스 아키텍처

credentials 패키지는 4개의 핵심 인터페이스를 정의한다. 이들의 관계를 살펴보자.

```
소스: credentials/credentials.go:38-226
```

### 인터페이스 관계도

```
 ┌─────────────────────────────────┐
 │      TransportCredentials       │  ← 연결 수립 시 핸드셰이크
 │  ClientHandshake() → AuthInfo   │
 │  ServerHandshake() → AuthInfo   │
 │  Info() → ProtocolInfo          │
 │  Clone()                        │
 │  OverrideServerName()           │
 └──────────────┬──────────────────┘
                │ 핸드셰이크 결과
                ▼
 ┌─────────────────────────────────┐
 │         AuthInfo                │  ← 보안 정보 캐리어
 │  AuthType() string              │
 │  (embed CommonAuthInfo)         │
 │    └─ SecurityLevel             │
 └──────────────┬──────────────────┘
                │ 보안 레벨 참조
                ▼
 ┌─────────────────────────────────┐
 │      PerRPCCredentials          │  ← 매 RPC마다 토큰 첨부
 │  GetRequestMetadata()           │
 │  RequireTransportSecurity()     │──→ CheckSecurityLevel(AuthInfo)
 └─────────────────────────────────┘

 ┌─────────────────────────────────┐
 │          Bundle                 │  ← 위 둘을 결합
 │  TransportCredentials()         │
 │  PerRPCCredentials()            │
 │  NewWithMode()                  │
 └─────────────────────────────────┘
```

---

## 3. TransportCredentials 인터페이스

전송 계층 보안의 핵심 인터페이스. 클라이언트와 서버 양측 모두에서 사용된다.

```go
// 소스: credentials/credentials.go:150-197
type TransportCredentials interface {
    ClientHandshake(context.Context, string, net.Conn) (net.Conn, AuthInfo, error)
    ServerHandshake(net.Conn) (net.Conn, AuthInfo, error)
    Info() ProtocolInfo
    Clone() TransportCredentials
    OverrideServerName(string) error
}
```

### 3.1 ClientHandshake

```go
ClientHandshake(ctx context.Context, authority string, rawConn net.Conn) (net.Conn, AuthInfo, error)
```

**매개변수 설명:**
- `ctx`: 타임아웃과 취소를 위한 컨텍스트. `ClientHandshakeInfo`가 포함되어 있어 resolver/balancer에서 전달한 속성에 접근 가능
- `authority`: `:authority` 헤더 값. 핸드셰이크 시 서버 이름으로 사용 (TLS의 경우 SNI에 설정)
- `rawConn`: 아직 핸드셰이크되지 않은 원시 TCP 연결

**반환값:**
- `net.Conn`: 핸드셰이크 완료된 보안 연결 (TLS 연결 등)
- `AuthInfo`: 핸드셰이크 결과 정보 (CommonAuthInfo 내장, SecurityLevel 포함)
- `error`: 임시 오류(io.EOF, context.DeadlineExceeded)는 재연결 시도, 영구 오류는 연결 실패

**왜 authority를 별도 매개변수로 받는가?**

하나의 TransportCredentials 인스턴스가 여러 서버에 대한 연결에 재사용될 수 있다. 각 연결마다 다른 서버 이름을 사용해야 하므로, Clone()으로 복사하지 않고도 authority를 동적으로 전달받도록 설계했다.

### 3.2 ServerHandshake

```go
ServerHandshake(rawConn net.Conn) (net.Conn, AuthInfo, error)
```

ClientHandshake와 달리 `context.Context`를 받지 않는다. 서버는 자체적으로 타임아웃을 관리하기 때문이다 (ALTS의 경우 내부에서 30초 타임아웃 컨텍스트를 생성).

### 3.3 Info와 ProtocolInfo

```go
// 소스: credentials/credentials.go:98-123
type ProtocolInfo struct {
    ProtocolVersion  string  // deprecated, 미사용
    SecurityProtocol string  // "tls", "insecure", "alts", "local" 등
    SecurityVersion  string  // deprecated, Peer.AuthInfo 사용 권장
    ServerName       string  // deprecated, grpc.WithAuthority 사용 권장
}
```

`SecurityProtocol` 필드는 클라이언트 전송 계층에서 URL 스킴을 결정하는 데 사용된다:

```go
// 소스: internal/transport/http2_client.go:309
if transportCreds.Info().SecurityProtocol == "tls" {
    scheme = "https"
}
```

### 3.4 Clone

Thread-safety를 위해 TransportCredentials의 복사본을 만든다. TLS의 경우 `tls.Config`를 복제하여 독립적인 인스턴스를 생성한다.

```go
// 소스: credentials/tls.go:199-201
func (c *tlsCreds) Clone() TransportCredentials {
    return NewTLS(c.config)
}
```

### 3.5 OverrideServerName (Deprecated)

```go
OverrideServerName(string) error
```

TLS에서 SNI(Server Name Indication)와 인증서 검증에 사용되는 서버 이름을 오버라이드한다. 현재는 `grpc.WithAuthority` 사용이 권장된다.

---

## 4. PerRPCCredentials 인터페이스

매 RPC 호출마다 인증 메타데이터를 첨부하는 인터페이스.

```go
// 소스: credentials/credentials.go:36-52
type PerRPCCredentials interface {
    GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error)
    RequireTransportSecurity() bool
}
```

### 4.1 GetRequestMetadata

```go
GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error)
```

- `ctx`에는 `RequestInfo`가 포함되어 있어 `AuthInfo`(보안 레벨)에 접근 가능
- `uri`는 요청 대상의 URI (audience 결정에 사용)
- 반환된 `map[string]string`은 HTTP/2 헤더에 설정됨

**실제 호출 흐름 (클라이언트):**

```go
// 소스: internal/transport/http2_client.go:665-689
func (t *http2Client) getTrAuthData(ctx context.Context, audience string) (map[string]string, error) {
    if len(t.perRPCCreds) == 0 {
        return nil, nil
    }
    authData := map[string]string{}
    for _, c := range t.perRPCCreds {
        data, err := c.GetRequestMetadata(ctx, audience)
        if err != nil {
            // ... 에러 처리 ...
            return nil, status.Errorf(codes.Unauthenticated,
                "transport: per-RPC creds failed due to error: %v", err)
        }
        for k, v := range data {
            k = strings.ToLower(k)  // HTTP/2는 소문자 헤더만 허용
            authData[k] = v
        }
    }
    return authData, nil
}
```

**왜 `uri ...string`은 가변 인자인가?**

초기 설계에서는 여러 URI에 대해 한 번에 메타데이터를 가져올 수 있도록 하려 했지만, 실제로는 단일 audience URI만 전달된다. 가변 인자로 남겨둔 것은 하위 호환성 유지를 위한 것이다.

### 4.2 RequireTransportSecurity

```go
RequireTransportSecurity() bool
```

이 메서드가 `true`를 반환하면, 전송 보안 없이는 크레덴셜을 전송할 수 없다. 클라이언트 연결 시점에 보안 레벨 검증이 이루어진다:

```go
// 소스: internal/transport/http2_client.go:296-307
for _, cd := range perRPCCreds {
    if cd.RequireTransportSecurity() {
        if ci, ok := authInfo.(interface {
            GetCommonAuthInfo() credentials.CommonAuthInfo
        }); ok {
            secLevel := ci.GetCommonAuthInfo().SecurityLevel
            if secLevel != credentials.InvalidSecurityLevel &&
               secLevel < credentials.PrivacyAndIntegrity {
                return nil, connectionErrorf(true, nil,
                    "transport: cannot send secure credentials on an insecure connection")
            }
        }
    }
}
```

### 4.3 RequestInfo 컨텍스트

PerRPCCredentials의 `GetRequestMetadata`에 전달되는 컨텍스트에는 `RequestInfo`가 포함된다:

```go
// 소스: credentials/credentials.go:231-236
type RequestInfo struct {
    Method   string    // "/some.Service/Method" 형식
    AuthInfo AuthInfo  // 전송 핸드셰이크 결과
}
```

이를 통해 PerRPCCredentials 구현체는 현재 연결의 보안 레벨을 확인할 수 있다.

---

## 5. AuthInfo 인터페이스와 SecurityLevel

### 5.1 AuthInfo

핸드셰이크 결과를 나타내는 인터페이스:

```go
// 소스: credentials/credentials.go:125-130
type AuthInfo interface {
    AuthType() string
}
```

단일 메서드 인터페이스지만, 실제 구현체들은 `CommonAuthInfo`를 내장하여 `SecurityLevel`을 포함한다.

### 5.2 SecurityLevel

```go
// 소스: credentials/credentials.go:54-69
type SecurityLevel int

const (
    InvalidSecurityLevel SecurityLevel = iota  // 0: 하위 호환용 무효값
    NoSecurity                                  // 1: 보안 없음
    IntegrityOnly                               // 2: 무결성만
    PrivacyAndIntegrity                         // 3: 암호화 + 무결성
)
```

**왜 `InvalidSecurityLevel`이 존재하는가?**

Go에서 int의 zero value는 0이다. SecurityLevel을 iota로 정의할 때, 기존 코드에서 `CommonAuthInfo{}`를 zero value로 초기화하면 SecurityLevel이 자동으로 0이 된다. 이때 보안 체크를 통과시키려면 0을 "아직 설정되지 않음"으로 처리해야 한다. 이것이 `InvalidSecurityLevel`의 존재 이유다:

```go
// 소스: credentials/credentials.go:290-308
func CheckSecurityLevel(ai AuthInfo, level SecurityLevel) error {
    type internalInfo interface {
        GetCommonAuthInfo() CommonAuthInfo
    }
    if ai == nil {
        return errors.New("AuthInfo is nil")
    }
    if ci, ok := ai.(internalInfo); ok {
        // zero value인 경우 하위 호환을 위해 통과
        if ci.GetCommonAuthInfo().SecurityLevel == InvalidSecurityLevel {
            return nil
        }
        if ci.GetCommonAuthInfo().SecurityLevel < level {
            return fmt.Errorf("requires SecurityLevel %v; connection has %v",
                level, ci.GetCommonAuthInfo().SecurityLevel)
        }
    }
    // GetCommonAuthInfo()를 구현하지 않는 옛날 AuthInfo도 통과
    return nil
}
```

### 5.3 CommonAuthInfo

```go
// 소스: credentials/credentials.go:84-96
type CommonAuthInfo struct {
    SecurityLevel SecurityLevel
}

func (c CommonAuthInfo) GetCommonAuthInfo() CommonAuthInfo {
    return c
}
```

모든 AuthInfo 구현체는 이 구조체를 내장(embed)하여 `SecurityLevel`을 노출한다.

### 5.4 AuthorityValidator (선택적 인터페이스)

```go
// 소스: credentials/credentials.go:132-144
type AuthorityValidator interface {
    ValidateAuthority(authority string) error
}
```

AuthInfo가 이 인터페이스도 구현하면, `:authority` 헤더 오버라이드 시 유효성 검증이 수행된다. TLS의 경우 피어 인증서의 호스트네임과 대조한다:

```go
// 소스: credentials/tls.go:56-69
func (t TLSInfo) ValidateAuthority(authority string) error {
    host, _, err := net.SplitHostPort(authority)
    if err != nil {
        host = authority
    }
    if len(t.State.PeerCertificates) == 0 {
        return fmt.Errorf("credentials: no peer certificates found to verify authority %q", host)
    }
    return t.State.PeerCertificates[0].VerifyHostname(host)
}
```

---

## 6. Bundle 인터페이스

TransportCredentials와 PerRPCCredentials를 하나로 결합하는 인터페이스:

```go
// 소스: credentials/credentials.go:199-226
type Bundle interface {
    TransportCredentials() TransportCredentials
    PerRPCCredentials() PerRPCCredentials
    NewWithMode(mode string) (Bundle, error)
}
```

### 6.1 왜 Bundle이 필요한가?

Google Cloud 환경에서는 연결 대상에 따라 다른 전송 프로토콜과 인증 방식을 사용해야 한다:

- GCP 내부 (같은 데이터센터): ALTS + (선택적) 서비스 계정 토큰
- GCP 외부/인터넷: TLS + OAuth2 토큰
- gRPCLB 밸런서: ALTS + 밸런서 전용 토큰

하나의 `Bundle`이 `NewWithMode()`를 통해 이런 모드 전환을 지원한다.

### 6.2 모드 상수

```go
// 소스: internal/internal.go:261-269
const (
    CredsBundleModeFallback             = "fallback"
    CredsBundleModeBalancer             = "balancer"
    CredsBundleModeBackendFromBalancer  = "backend-from-balancer"
)
```

### 6.3 Bundle 사용 흐름

```
 클라이언트 연결 생성
     │
     ▼
 CredsBundle != nil ?
     │
     ├─ Yes ──▶ bundle.TransportCredentials() → transportCreds
     │          bundle.PerRPCCredentials()    → perRPCCreds에 추가
     │
     └─ No ──▶ opts.TransportCredentials 사용
                opts.PerRPCCredentials 사용
```

실제 코드:

```go
// 소스: internal/transport/http2_client.go:283-289
if b := opts.CredsBundle; b != nil {
    if t := b.TransportCredentials(); t != nil {
        transportCreds = t
    }
    if t := b.PerRPCCredentials(); t != nil {
        perRPCCreds = append(perRPCCreds, t)
    }
}
```

---

## 7. TLS 구현

gRPC-Go에서 가장 널리 사용되는 전송 보안 구현체.

```
소스: credentials/tls.go
```

### 7.1 tlsCreds 구조체

```go
// 소스: credentials/tls.go:98-102
type tlsCreds struct {
    config *tls.Config
}
```

단순하게 `tls.Config`를 감싸고 있다. 핵심은 `NewTLS`에서 적용하는 기본값(defaults)에 있다.

### 7.2 NewTLS와 기본값 적용

```go
// 소스: credentials/tls.go:222-257
func NewTLS(c *tls.Config) TransportCredentials {
    config := applyDefaults(c)
    if config.GetConfigForClient != nil {
        oldFn := config.GetConfigForClient
        config.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
            cfgForClient, err := oldFn(hello)
            if err != nil || cfgForClient == nil {
                return cfgForClient, err
            }
            return applyDefaults(cfgForClient), nil
        }
    }
    return &tlsCreds{config: config}
}

func applyDefaults(c *tls.Config) *tls.Config {
    config := credinternal.CloneTLSConfig(c)
    config.NextProtos = credinternal.AppendH2ToNextProtos(config.NextProtos)
    // HTTP/2 RFC 7540 요구사항: 최소 TLS 1.2
    if config.MinVersion == 0 && (config.MaxVersion == 0 || config.MaxVersion >= tls.VersionTLS12) {
        config.MinVersion = tls.VersionTLS12
    }
    // HTTP/2에서 금지된 Cipher Suite 제외 (RFC 7540 Appendix A)
    if config.CipherSuites == nil {
        for _, cs := range tls.CipherSuites() {
            if _, ok := tls12ForbiddenCipherSuites[cs.ID]; !ok {
                config.CipherSuites = append(config.CipherSuites, cs.ID)
            }
        }
    }
    return config
}
```

**왜 이런 기본값이 필요한가?**

1. **ALPN(h2) 강제**: gRPC는 HTTP/2 위에서 동작하므로, TLS 핸드셰이크 시 `h2` 프로토콜을 반드시 협상해야 한다
2. **TLS 1.2 최소 버전**: RFC 7540(HTTP/2 명세)에서 요구하는 최소 TLS 버전
3. **금지된 Cipher Suite 제외**: RFC 7540 Appendix A에서 HTTP/2와 함께 사용을 금지한 cipher suite 목록

### 7.3 클라이언트 핸드셰이크 (TLS)

```go
// 소스: credentials/tls.go:112-166
func (c *tlsCreds) ClientHandshake(ctx context.Context, authority string, rawConn net.Conn) (_ net.Conn, _ AuthInfo, err error) {
    // 여러 엔드포인트에서 재사용 시 ServerName 충돌 방지를 위해 복제
    cfg := credinternal.CloneTLSConfig(c.config)

    serverName, _, err := net.SplitHostPort(authority)
    if err != nil {
        serverName = authority
    }
    cfg.ServerName = serverName  // SNI 설정

    conn := tls.Client(rawConn, cfg)
    errChannel := make(chan error, 1)
    go func() {
        errChannel <- conn.Handshake()
        close(errChannel)
    }()
    select {
    case err := <-errChannel:
        if err != nil {
            conn.Close()
            return nil, nil, err
        }
    case <-ctx.Done():
        conn.Close()
        return nil, nil, ctx.Err()
    }

    // ALPN 협상 결과 확인
    np := conn.ConnectionState().NegotiatedProtocol
    if np == "" {
        if envconfig.EnforceALPNEnabled {
            conn.Close()
            return nil, nil, fmt.Errorf("credentials: cannot check peer: missing selected ALPN property. %s", alpnFailureHelpMessage)
        }
        // 경고만 출력하고 계속 진행 (향후 버전에서 차단 예정)
    }

    tlsInfo := TLSInfo{
        State: conn.ConnectionState(),
        CommonAuthInfo: CommonAuthInfo{
            SecurityLevel: PrivacyAndIntegrity,
        },
    }
    // SPIFFE ID 추출 (서비스 메시 환경)
    id := credinternal.SPIFFEIDFromState(conn.ConnectionState())
    if id != nil {
        tlsInfo.SPIFFEID = id
    }
    return credinternal.WrapSyscallConn(rawConn, conn), tlsInfo, nil
}
```

**핵심 설계 포인트:**

1. **비동기 핸드셰이크**: `go func()` + `select`로 핸드셰이크를 별도 고루틴에서 수행. `ctx.Done()`으로 타임아웃 감지 가능
2. **ALPN 강제**: gRPC 1.67부터 ALPN 미지원 서버에 대한 연결을 차단 (`EnforceALPNEnabled`)
3. **SPIFFE ID 추출**: 서비스 메시 환경에서 워크로드 신원 식별을 위해 인증서의 SAN URI에서 SPIFFE ID를 파싱
4. **WrapSyscallConn**: rawConn의 syscall.Conn 인터페이스를 보존하면서 TLS conn으로 감싸기

### 7.4 서버 핸드셰이크 (TLS)

```go
// 소스: credentials/tls.go:168-197
func (c *tlsCreds) ServerHandshake(rawConn net.Conn) (net.Conn, AuthInfo, error) {
    conn := tls.Server(rawConn, c.config)
    if err := conn.Handshake(); err != nil {
        conn.Close()
        return nil, nil, err
    }
    cs := conn.ConnectionState()
    if cs.NegotiatedProtocol == "" {
        if envconfig.EnforceALPNEnabled {
            conn.Close()
            return nil, nil, fmt.Errorf("credentials: cannot check peer: missing selected ALPN property. %s", alpnFailureHelpMessage)
        }
    }
    tlsInfo := TLSInfo{
        State: cs,
        CommonAuthInfo: CommonAuthInfo{
            SecurityLevel: PrivacyAndIntegrity,
        },
    }
    id := credinternal.SPIFFEIDFromState(conn.ConnectionState())
    if id != nil {
        tlsInfo.SPIFFEID = id
    }
    return credinternal.WrapSyscallConn(rawConn, conn), tlsInfo, nil
}
```

**클라이언트와의 차이점:** 서버는 `context.Context`를 받지 않으므로, 핸드셰이크를 동기적으로(blocking) 수행한다. 타임아웃은 상위 레이어에서 `rawConn.SetDeadline()`으로 처리된다.

### 7.5 편의 생성 함수들

| 함수 | 용도 | 소스 위치 |
|------|------|----------|
| `NewTLS(c *tls.Config)` | 커스텀 TLS 설정 | tls.go:222 |
| `NewClientTLSFromCert(cp, name)` | CA 인증서 풀 기반 클라이언트 | tls.go:269 |
| `NewClientTLSFromFile(certFile, name)` | PEM 파일 기반 클라이언트 | tls.go:283 |
| `NewServerTLSFromCert(cert)` | 인증서 객체 기반 서버 | tls.go:296 |
| `NewServerTLSFromFile(certFile, keyFile)` | PEM 파일 기반 서버 | tls.go:302 |

### 7.6 TLSInfo

```go
// 소스: credentials/tls.go:39-46
type TLSInfo struct {
    State tls.ConnectionState
    CommonAuthInfo
    SPIFFEID *url.URL  // experimental
}
```

`tls.ConnectionState`를 직접 내장하여, 사용자가 핸드셰이크 후 피어 인증서, 협상된 프로토콜, cipher suite 등에 접근할 수 있다.

### 7.7 금지된 Cipher Suite 목록

```go
// 소스: credentials/tls.go:208-219
var tls12ForbiddenCipherSuites = map[uint16]struct{}{
    tls.TLS_RSA_WITH_AES_128_CBC_SHA:         {},
    tls.TLS_RSA_WITH_AES_256_CBC_SHA:         {},
    tls.TLS_RSA_WITH_AES_128_GCM_SHA256:      {},
    tls.TLS_RSA_WITH_AES_256_GCM_SHA384:      {},
    tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA: {},
    tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA: {},
    tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA:   {},
    tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA:   {},
}
```

RFC 7540 Appendix A에 따라 HTTP/2에서 사용이 금지된 cipher suite들이다. 주로 CBC 모드 cipher가 포함되어 있으며, 이들은 BEAST 공격 등에 취약할 수 있다.

---

## 8. Insecure 구현

```
소스: credentials/insecure/insecure.go
```

### 8.1 왜 Insecure가 필요한가?

gRPC-Go에서 `grpc.Dial` 또는 `grpc.NewClient`를 호출할 때, TransportCredentials를 **반드시** 제공해야 한다. 보안 없는 연결을 원할 때도 "보안을 사용하지 않겠다"는 것을 **명시적으로** 선언해야 한다는 설계 철학이다.

```go
// 실수로 보안 없이 연결하는 것을 방지
conn, err := grpc.NewClient("localhost:50051",
    grpc.WithTransportCredentials(insecure.NewCredentials()),
)
```

이전에는 `grpc.WithInsecure()` 다이얼 옵션이 있었지만 deprecated 되었고, 현재는 `insecure.NewCredentials()`가 표준이다.

### 8.2 insecureTC 구현

```go
// 소스: credentials/insecure/insecure.go:34-61
func NewCredentials() credentials.TransportCredentials {
    return insecureTC{}
}

type insecureTC struct{}

func (insecureTC) ClientHandshake(_ context.Context, _ string, conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
    return conn, info{credentials.CommonAuthInfo{SecurityLevel: credentials.NoSecurity}}, nil
}

func (insecureTC) ServerHandshake(conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
    return conn, info{credentials.CommonAuthInfo{SecurityLevel: credentials.NoSecurity}}, nil
}

func (insecureTC) Info() credentials.ProtocolInfo {
    return credentials.ProtocolInfo{SecurityProtocol: "insecure"}
}
```

**핵심:** 핸드셰이크 시 연결을 그대로 통과시키되(pass-through), `SecurityLevel: NoSecurity`를 설정한다. 이로 인해 `RequireTransportSecurity() == true`인 PerRPCCredentials와 함께 사용하면 런타임 에러가 발생한다.

### 8.3 insecureBundle

```go
// 소스: credentials/insecure/insecure.go:80-104
type insecureBundle struct{}

func NewBundle() credentials.Bundle {
    return insecureBundle{}
}

func (insecureBundle) TransportCredentials() credentials.TransportCredentials {
    return NewCredentials()
}

func (insecureBundle) PerRPCCredentials() credentials.PerRPCCredentials {
    return nil  // 비보안 번들에는 per-RPC 크레덴셜 없음
}
```

테스트 환경에서 Bundle 인터페이스가 필요한 경우를 위한 비보안 번들 구현이다.

### 8.4 insecure.info와 ValidateAuthority

```go
// 소스: credentials/insecure/insecure.go:65-78
type info struct {
    credentials.CommonAuthInfo
}

func (info) AuthType() string {
    return "insecure"
}

func (info) ValidateAuthority(string) error {
    return nil  // insecure 연결에서는 어떤 authority도 허용
}
```

비보안 연결에서는 authority 검증이 의미 없으므로, 항상 `nil`(성공)을 반환한다.

---

## 9. Local 구현

```
소스: credentials/local/local.go
```

### 9.1 연결 유형에 따른 동적 보안 레벨

Local 크레덴셜의 핵심 특징은 **연결 유형을 자동 감지**하여 보안 레벨을 동적으로 결정하는 것이다.

```go
// 소스: credentials/local/local.go:69-84
func getSecurityLevel(network, addr string) (credentials.SecurityLevel, error) {
    switch {
    // 로컬 TCP (127.0.0.0/8 또는 ::1)
    case strings.HasPrefix(addr, "127."), strings.HasPrefix(addr, "[::1]:"):
        return credentials.NoSecurity, nil
    // Windows Named Pipe
    case network == "pipe" && strings.HasPrefix(addr, `\\.\pipe\`):
        return credentials.NoSecurity, nil
    // Unix Domain Socket
    case network == "unix":
        return credentials.PrivacyAndIntegrity, nil
    // 비로컬 연결 → 거부
    default:
        return credentials.InvalidSecurityLevel,
            fmt.Errorf("local credentials rejected connection to non-local address %q", addr)
    }
}
```

### 9.2 보안 레벨 결정 로직

```
 연결 유형 판별
     │
     ├─ Unix Domain Socket ──▶ PrivacyAndIntegrity (레벨 3)
     │   이유: UDS는 OS 커널이 프로세스 격리를 보장
     │         네트워크를 통하지 않으므로 도청/변조 불가
     │
     ├─ 127.0.0.0/8 또는 ::1 ──▶ NoSecurity (레벨 1)
     │   이유: 로컬 TCP는 같은 머신이지만
     │         루프백 인터페이스를 통과하므로 이론상 스니핑 가능
     │
     ├─ Windows Named Pipe ──▶ NoSecurity (레벨 1)
     │   이유: Named Pipe도 로컬 IPC이지만
     │         보안 레벨을 보수적으로 설정
     │
     └─ 기타 (원격 주소) ──▶ 에러 (연결 거부)
         이유: Local 크레덴셜은 로컬 연결 전용
```

**왜 UDS가 PrivacyAndIntegrity인가?**

Unix Domain Socket은 커널 내부에서 데이터를 전달하며, 네트워크 스택을 거치지 않는다. 따라서:
- **기밀성(Privacy)**: 네트워크 패킷이 존재하지 않아 도청이 불가능
- **무결성(Integrity)**: 커널이 데이터 전달을 보장
- **접근 제어**: 파일 시스템 권한으로 접근을 제한 가능

이 덕분에 UDS + Local 크레덴셜 조합에서 `RequireTransportSecurity() == true`인 PerRPCCredentials(예: OAuth2)도 사용할 수 있다.

### 9.3 핸드셰이크

```go
// 소스: credentials/local/local.go:86-99
func (*localTC) ClientHandshake(_ context.Context, _ string, conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
    secLevel, err := getSecurityLevel(conn.RemoteAddr().Network(), conn.RemoteAddr().String())
    if err != nil {
        return nil, nil, err
    }
    return conn, info{credentials.CommonAuthInfo{SecurityLevel: secLevel}}, nil
}

func (*localTC) ServerHandshake(conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
    secLevel, err := getSecurityLevel(conn.RemoteAddr().Network(), conn.RemoteAddr().String())
    if err != nil {
        return nil, nil, err
    }
    return conn, info{credentials.CommonAuthInfo{SecurityLevel: secLevel}}, nil
}
```

insecure와 마찬가지로 연결을 그대로 통과시키지만, 보안 레벨은 연결 유형에 따라 동적으로 결정된다.

---

## 10. OAuth2 구현

```
소스: credentials/oauth/oauth.go
```

OAuth2 패키지는 `PerRPCCredentials` 인터페이스의 여러 구현체를 제공한다.

### 10.1 TokenSource

가장 범용적인 OAuth2 크레덴셜. `oauth2.TokenSource`를 감싸서 gRPC PerRPCCredentials로 변환한다.

```go
// 소스: credentials/oauth/oauth.go:36-58
type TokenSource struct {
    oauth2.TokenSource
}

func (ts TokenSource) GetRequestMetadata(ctx context.Context, _ ...string) (map[string]string, error) {
    token, err := ts.Token()
    if err != nil {
        return nil, err
    }
    // 보안 레벨 확인 - PrivacyAndIntegrity 필요
    ri, _ := credentials.RequestInfoFromContext(ctx)
    if err = credentials.CheckSecurityLevel(ri.AuthInfo, credentials.PrivacyAndIntegrity); err != nil {
        return nil, fmt.Errorf("unable to transfer TokenSource PerRPCCredentials: %v", err)
    }
    return map[string]string{
        "authorization": token.Type() + " " + token.AccessToken,
    }, nil
}

func (ts TokenSource) RequireTransportSecurity() bool {
    return true  // 항상 전송 보안 필요
}
```

**왜 보안 레벨을 이중 체크하는가?**

`RequireTransportSecurity()`는 연결 수립 시 검사되고, `CheckSecurityLevel()`은 RPC 호출 시 검사된다. 이 이중 체크는 다음 시나리오를 방어한다:

1. 연결 수립 시점에는 보안이 있었지만 이후 변경된 경우 (이론적)
2. Bundle 등을 통해 런타임에 크레덴셜이 교체된 경우
3. 보안 레벨 숫자값의 세밀한 비교 (IntegrityOnly vs PrivacyAndIntegrity)

### 10.2 jwtAccess

Self-Signed JWT를 사용하는 방식. OAuth2 토큰 교환 없이 직접 JWT를 생성한다.

```go
// 소스: credentials/oauth/oauth.go:70-116
type jwtAccess struct {
    jsonKey []byte
}

func (j jwtAccess) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
    // RPC 서비스 이름을 URI에서 제거하여 audience 구성
    aud, err := removeServiceNameFromJWTURI(uri[0])
    if err != nil {
        return nil, err
    }
    // JWT 토큰 소스 생성 (매번 새로 만듦 - TODO: 캐싱)
    ts, err := google.JWTAccessTokenSourceFromJSON(j.jsonKey, aud)
    if err != nil {
        return nil, err
    }
    token, err := ts.Token()
    if err != nil {
        return nil, err
    }
    // 보안 레벨 체크
    ri, _ := credentials.RequestInfoFromContext(ctx)
    if err = credentials.CheckSecurityLevel(ri.AuthInfo, credentials.PrivacyAndIntegrity); err != nil {
        return nil, fmt.Errorf("unable to transfer jwtAccess PerRPCCredentials: %v", err)
    }
    return map[string]string{
        "authorization": token.Type() + " " + token.AccessToken,
    }, nil
}
```

### 10.3 serviceAccount

서비스 계정 키로 OAuth2 토큰을 획득하는 방식. 토큰 캐싱과 갱신을 자체적으로 처리한다.

```go
// 소스: credentials/oauth/oauth.go:152-180
type serviceAccount struct {
    mu     sync.Mutex
    config *jwt.Config
    t      *oauth2.Token
}

func (s *serviceAccount) GetRequestMetadata(ctx context.Context, _ ...string) (map[string]string, error) {
    s.mu.Lock()
    defer s.mu.Unlock()
    if !s.t.Valid() {
        var err error
        s.t, err = s.config.TokenSource(ctx).Token()
        if err != nil {
            return nil, err
        }
    }
    ri, _ := credentials.RequestInfoFromContext(ctx)
    if err := credentials.CheckSecurityLevel(ri.AuthInfo, credentials.PrivacyAndIntegrity); err != nil {
        return nil, fmt.Errorf("unable to transfer serviceAccount PerRPCCredentials: %v", err)
    }
    return map[string]string{
        "authorization": s.t.Type() + " " + s.t.AccessToken,
    }, nil
}
```

**토큰 캐싱 패턴:** `sync.Mutex`로 동시 접근을 보호하고, `s.t.Valid()`로 만료 여부를 확인한 후 필요할 때만 새 토큰을 획득한다.

### 10.4 OAuth2 구현체 비교

| 구현체 | 토큰 획득 방식 | 캐싱 | 용도 |
|--------|---------------|------|------|
| `TokenSource` | `oauth2.TokenSource` 위임 | TokenSource 구현에 의존 | 범용 OAuth2 |
| `jwtAccess` | Self-Signed JWT 직접 생성 | 없음 (TODO) | GCP 서비스 계정 |
| `serviceAccount` | JWT Config → Token 교환 | `sync.Mutex` + `Valid()` | GCP 서비스 계정 (scope 필요) |
| `oauthAccess` | 정적 토큰 (deprecated) | 해당 없음 | 테스트용 |

### 10.5 NewApplicationDefault

```go
// 소스: credentials/oauth/oauth.go:204-244
func NewApplicationDefault(ctx context.Context, scope ...string) (credentials.PerRPCCredentials, error) {
    creds, err := google.FindDefaultCredentials(ctx, scope...)
    // ...
    // JSON이 nil이면 GCE 환경에서 실행 중
    if creds.JSON == nil {
        return TokenSource{creds.TokenSource}, nil
    }
    // scope가 있으면 OAuth2 토큰 교환
    if len(scope) != 0 {
        return TokenSource{creds.TokenSource}, nil
    }
    // scope 없이 서비스 계정이면 JWT 직접 사용
    if _, err := google.JWTConfigFromJSON(creds.JSON); err != nil {
        return TokenSource{creds.TokenSource}, nil
    }
    return NewJWTAccessFromKey(creds.JSON)
}
```

**ADC(Application Default Credentials) 결정 트리:**

```
 FindDefaultCredentials()
     │
     ├─ JSON == nil (GCE 메타데이터 서버) ──▶ TokenSource
     │
     ├─ scope 있음 ──▶ TokenSource (OAuth2 교환)
     │
     └─ scope 없음
         │
         ├─ 서비스 계정 키 ──▶ JWTAccess (직접 JWT)
         │
         └─ 기타 ──▶ TokenSource (fallback)
```

---

## 11. ALTS 개요

```
소스: credentials/alts/alts.go
```

ALTS(Application Layer Transport Security)는 Google이 내부 인프라를 위해 설계한 전송 보안 프로토콜이다. GCP(Google Cloud Platform) 환경에서만 동작한다.

### 11.1 왜 ALTS인가? TLS와의 차이

| 특성 | TLS | ALTS |
|------|-----|------|
| 인증서 관리 | 사용자가 직접 | GCP가 자동 |
| 핸드셰이크 수행 | 프로세스 내부 | 외부 핸드셰이커 서비스 |
| 신원 모델 | X.509 인증서 | GCP 서비스 계정 |
| 환경 제한 | 어디서든 가능 | GCP VM에서만 |
| 키 교환 방식 | 프로세스 메모리 | 하이퍼바이저/TEE |

ALTS의 핵심 장점은 **인증서 관리의 자동화**다. TLS에서는 인증서 생성, 배포, 갱신이 필요하지만, ALTS에서는 GCP 인프라가 이를 모두 처리한다.

### 11.2 altsTC 구조체

```go
// 소스: credentials/alts/alts.go:135-141
type altsTC struct {
    info             *credentials.ProtocolInfo
    side             core.Side          // ClientSide 또는 ServerSide
    accounts         []string           // 허용된 대상 서비스 계정
    hsAddress        string             // 핸드셰이커 서비스 주소
    boundAccessToken string             // 바인딩된 액세스 토큰
}
```

### 11.3 ALTS 핸드셰이크 흐름

```
 ┌──────────┐                  ┌──────────────────┐                  ┌──────────┐
 │  Client  │                  │ Handshaker Svc   │                  │  Server  │
 │ (altsTC) │                  │ (metadata.google │                  │ (altsTC) │
 │          │                  │  .internal:8080) │                  │          │
 └─────┬────┘                  └────────┬─────────┘                  └─────┬────┘
       │                                │                                  │
       │  1. GCE VM 확인                │                                  │
       │  (vmOnGCP == true?)            │                                  │
       │                                │                                  │
       │  2. Dial(hsAddress)            │                                  │
       │ ─────────────────────────────▶ │                                  │
       │                                │                                  │
       │  3. NewClientHandshaker()      │                                  │
       │      + ClientHandshake()       │                                  │
       │ ─────────────────────────────▶ │ ◀─────────────────────────────── │
       │                                │     NewServerHandshaker()        │
       │                                │     + ServerHandshake()          │
       │                                │                                  │
       │  4. RPC Version 호환성 확인     │                                  │
       │                                │                                  │
       │  5. secConn, authInfo 반환     │                                  │
       │ ◀───────────────────────────── │ ──────────────────────────────▶  │
       │                                │                                  │
```

### 11.4 GCE VM 검증

```go
// 소스: credentials/alts/alts.go:172-175
func (g *altsTC) ClientHandshake(ctx context.Context, addr string, rawConn net.Conn) (_ net.Conn, _ credentials.AuthInfo, err error) {
    if !vmOnGCP {
        return nil, nil, ErrUntrustedPlatform
    }
    // ...
}
```

ALTS는 GCP VM에서만 동작한다. 비-GCP 환경에서 사용하면 `ErrUntrustedPlatform` 에러가 반환된다. 이는 ALTS 핸드셰이커 서비스가 하이퍼바이저 수준에서 제공되기 때문이다.

### 11.5 RPC Version 호환성 체크

```go
// 소스: credentials/alts/alts.go:312-334
func checkRPCVersions(local, peer *altspb.RpcProtocolVersions) (bool, *altspb.RpcProtocolVersions_Version) {
    // maxCommonVersion = MIN(local.max, peer.max)
    maxCommonVersion := local.GetMaxRpcVersion()
    if compareRPCVersions(local.GetMaxRpcVersion(), peer.GetMaxRpcVersion()) > 0 {
        maxCommonVersion = peer.GetMaxRpcVersion()
    }
    // minCommonVersion = MAX(local.min, peer.min)
    minCommonVersion := peer.GetMinRpcVersion()
    if compareRPCVersions(local.GetMinRpcVersion(), peer.GetMinRpcVersion()) > 0 {
        minCommonVersion = local.GetMinRpcVersion()
    }
    // 교집합이 존재하면 호환
    if compareRPCVersions(maxCommonVersion, minCommonVersion) < 0 {
        return false, nil
    }
    return true, maxCommonVersion
}
```

이 알고리즘은 두 피어의 지원 버전 범위에서 **교집합**을 찾는다:

```
 Local:  [min_L ─────── max_L]
 Peer:          [min_P ────────── max_P]
 Common:        [min_P ── max_L]   ← 교집합이 존재하면 호환
```

---

## 12. Google Default Credentials Bundle

```
소스: credentials/google/google.go
```

Google Cloud 환경에서 ALTS와 TLS를 자동으로 전환하는 Bundle 구현.

### 12.1 creds 구조체

```go
// 소스: credentials/google/google.go:94-104
type creds struct {
    opts DefaultCredentialsOptions

    mode           string
    transportCreds credentials.TransportCredentials
    perRPCCreds    credentials.PerRPCCredentials
}
```

### 12.2 모드별 동작

```go
// 소스: credentials/google/google.go:131-154
func (c *creds) NewWithMode(mode string) (credentials.Bundle, error) {
    newCreds := &creds{opts: c.opts, mode: mode}
    switch mode {
    case internal.CredsBundleModeFallback:
        // ALTS와 TLS를 모두 지원하는 클러스터 전송 크레덴셜
        newCreds.transportCreds = newClusterTransportCreds(newTLS(), newALTS())
    case internal.CredsBundleModeBackendFromBalancer, internal.CredsBundleModeBalancer:
        // 밸런서 모드에서는 ALTS만 사용
        newCreds.transportCreds = newALTS()
    default:
        return nil, fmt.Errorf("unsupported mode: %v", mode)
    }
    // fallback과 backend-from-balancer 모드에서 PerRPC 크레덴셜 설정
    if mode == internal.CredsBundleModeFallback || mode == internal.CredsBundleModeBackendFromBalancer {
        newCreds.perRPCCreds = newCreds.opts.PerRPCCreds
    }
    return newCreds, nil
}
```

### 12.3 dualPerRPCCreds

ALTS와 TLS 연결에서 다른 PerRPCCredentials를 사용하기 위한 래퍼:

```go
// 소스: credentials/google/google.go:156-178
type dualPerRPCCreds struct {
    perRPCCreds     credentials.PerRPCCredentials
    altsPerRPCCreds credentials.PerRPCCredentials
}

func (d *dualPerRPCCreds) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
    ri, ok := credentials.RequestInfoFromContext(ctx)
    if !ok {
        return nil, fmt.Errorf("request info not found from context")
    }
    // AuthType으로 전송 프로토콜 판별
    if authType := ri.AuthInfo.AuthType(); authType == "alts" {
        return d.altsPerRPCCreds.GetRequestMetadata(ctx, uri...)
    }
    return d.perRPCCreds.GetRequestMetadata(ctx, uri...)
}
```

**왜 이 설계인가?**

Google 내부 네트워크에서는 ALTS를 사용하고, 외부에서는 TLS를 사용한다. 각 전송 프로토콜에 최적화된 인증 토큰이 다를 수 있으므로, `AuthType()`을 확인하여 적절한 크레덴셜을 선택한다.

---

## 13. 서버 핸드셰이크 흐름

서버에서 새 연결을 받아 인증 핸드셰이크를 수행하는 전체 흐름:

```
 ┌─────────────────────────────────────────────────────────────┐
 │                    서버 핸드셰이크 흐름                       │
 └─────────────────────────────────────────────────────────────┘

 net.Listener.Accept()
     │
     ▼
 Server.handleRawConn(lisAddr, rawConn)    ← server.go:965
     │
     ├─ rawConn.SetDeadline(connectionTimeout)   ← 타임아웃 설정
     │
     ▼
 Server.newHTTP2Transport(rawConn)          ← server.go:996
     │
     ├─ ServerConfig.Credentials = s.opts.creds  ← 서버 크레덴셜 전달
     │
     ▼
 transport.NewServerTransport(conn, config) ← http2_server.go:149
     │
     ├─ config.Credentials != nil ?
     │   │
     │   ├─ Yes ──▶ config.Credentials.ServerHandshake(rawConn)
     │   │              │
     │   │              ├─ 성공 → conn = secureConn, authInfo 저장
     │   │              │
     │   │              └─ 실패 → ErrConnDispatched ? → 연결 유지
     │   │                        io.EOF ?           → nil, nil 반환
     │   │                        기타               → 에러 반환
     │   │
     │   └─ No ──▶ 인증 없이 계속
     │
     ▼
 rawConn.SetDeadline(time.Time{})  ← 타임아웃 해제
     │
     ▼
 HTTP/2 프레임 처리 시작
```

### 실제 코드

```go
// 소스: server.go:965-992
func (s *Server) handleRawConn(lisAddr string, rawConn net.Conn) {
    if s.quit.HasFired() {
        rawConn.Close()
        return
    }
    rawConn.SetDeadline(time.Now().Add(s.opts.connectionTimeout))

    // 핸드셰이크 (HTTP2)
    st := s.newHTTP2Transport(rawConn)
    rawConn.SetDeadline(time.Time{})
    if st == nil {
        return
    }
    // ...
}
```

```go
// 소스: server.go:996-1016
func (s *Server) newHTTP2Transport(c net.Conn) transport.ServerTransport {
    config := &transport.ServerConfig{
        // ...
        Credentials: s.opts.creds,  // TransportCredentials 전달
        // ...
    }
    st, err := transport.NewServerTransport(c, config)
    // ...
}
```

```go
// 소스: internal/transport/http2_server.go:149-164
func NewServerTransport(conn net.Conn, config *ServerConfig) (_ ServerTransport, err error) {
    var authInfo credentials.AuthInfo
    rawConn := conn
    if config.Credentials != nil {
        var err error
        conn, authInfo, err = config.Credentials.ServerHandshake(rawConn)
        if err != nil {
            if err == credentials.ErrConnDispatched || err == io.EOF {
                return nil, err
            }
            return nil, connectionErrorf(false, err,
                "ServerHandshake(%q) failed: %v", rawConn.RemoteAddr(), err)
        }
    }
    // ... HTTP/2 프레임 처리 시작
}
```

**ErrConnDispatched의 의미:**

```go
// 소스: credentials/credentials.go:148
var ErrConnDispatched = errors.New("credentials: rawConn is dispatched out of gRPC")
```

일부 크레덴셜 구현(예: xDS)에서는 핸드셰이크 과정에서 연결을 gRPC 외부로 전달할 수 있다. 이 경우 gRPC는 연결을 닫지 않아야 한다.

---

## 14. 클라이언트 핸드셰이크 흐름

클라이언트에서 서버에 연결하여 인증 핸드셰이크를 수행하는 전체 흐름:

```
 ┌─────────────────────────────────────────────────────────────┐
 │                  클라이언트 핸드셰이크 흐름                    │
 └─────────────────────────────────────────────────────────────┘

 grpc.NewClient() → Dial/Connect
     │
     ▼
 newHTTP2Client(connectCtx, addr, opts)  ← http2_client.go 진입
     │
     ├─ 1. ClientHandshakeInfo를 컨텍스트에 설정
     │     connectCtx = icredentials.NewClientHandshakeInfoContext(
     │         connectCtx,
     │         credentials.ClientHandshakeInfo{Attributes: addr.Attributes})
     │
     ├─ 2. TCP 연결 수립
     │     conn, err := dial(connectCtx, opts.Dialer, addr, ...)
     │
     ├─ 3. 크레덴셜 해석 (Bundle 우선)
     │     transportCreds := opts.TransportCredentials
     │     perRPCCreds := opts.PerRPCCredentials
     │     if b := opts.CredsBundle; b != nil {
     │         transportCreds = b.TransportCredentials()
     │         perRPCCreds = append(perRPCCreds, b.PerRPCCredentials())
     │     }
     │
     ├─ 4. 전송 핸드셰이크
     │     conn, authInfo, err = transportCreds.ClientHandshake(
     │         connectCtx, addr.ServerName, conn)
     │
     ├─ 5. PerRPC 크레덴셜 보안 레벨 검증
     │     for _, cd := range perRPCCreds {
     │         if cd.RequireTransportSecurity() {
     │             // SecurityLevel < PrivacyAndIntegrity → 에러
     │         }
     │     }
     │
     ├─ 6. isSecure = true, scheme 결정
     │     if transportCreds.Info().SecurityProtocol == "tls" {
     │         scheme = "https"
     │     }
     │
     └─ 7. http2Client 구조체에 perRPCCreds 저장
           (이후 매 RPC마다 getTrAuthData() 호출)
```

### RPC 호출 시 인증 데이터 첨부

연결 수립 후, 매 RPC 호출 시 두 종류의 인증 데이터가 첨부된다:

```
 RPC 호출 (NewStream)
     │
     ├─ 1. audience 생성
     │     "https://" + host + method[:lastSlash]
     │
     ├─ 2. 전송 레벨 인증 데이터 (Dial 옵션 + Bundle)
     │     getTrAuthData(ctx, audience)
     │     → t.perRPCCreds[i].GetRequestMetadata(ctx, audience)
     │     → map[string]string 반환
     │
     ├─ 3. 호출 레벨 인증 데이터 (Call 옵션)
     │     getCallAuthData(ctx, audience, callHdr)
     │     → callHdr.Creds.GetRequestMetadata(ctx, audience)
     │     → map[string]string 반환
     │
     └─ 4. 두 map을 HTTP/2 헤더에 병합
```

```go
// 소스: internal/transport/http2_client.go:692-710
func (t *http2Client) getCallAuthData(ctx context.Context, audience string, callHdr *CallHdr) (map[string]string, error) {
    var callAuthData map[string]string
    if callCreds := callHdr.Creds; callCreds != nil {
        if callCreds.RequireTransportSecurity() {
            ri, _ := credentials.RequestInfoFromContext(ctx)
            if !t.isSecure || credentials.CheckSecurityLevel(ri.AuthInfo, credentials.PrivacyAndIntegrity) != nil {
                return nil, status.Error(codes.Unauthenticated,
                    "transport: cannot send secure credentials on an insecure connection")
            }
        }
        data, err := callCreds.GetRequestMetadata(ctx, audience)
        // ...
    }
    return callAuthData, nil
}
```

**Dial 옵션 vs Call 옵션:**
- **Dial 옵션** (`grpc.WithPerRPCCredentials`): 연결 전체에 적용, 모든 RPC에 동일 크레덴셜
- **Call 옵션** (`grpc.PerRPCCredsCallOption`): 개별 RPC에 적용, 호출별 다른 크레덴셜 가능
- 둘 다 제공된 경우, **양쪽 모두** 적용된다 (병합)

---

## 15. 보안 레벨 체크 메커니즘

gRPC-Go의 보안 레벨 체크는 여러 지점에서 수행된다.

### 15.1 체크 지점 종합

```
 ┌─────────────────────────────────────────────────────────────┐
 │               보안 레벨 체크 지점                             │
 └─────────────────────────────────────────────────────────────┘

 [체크 1] 연결 수립 시 (http2_client.go:296-307)
     시점: ClientHandshake 직후
     대상: Dial 옵션의 PerRPCCredentials
     조건: RequireTransportSecurity() == true
     체크: AuthInfo.SecurityLevel >= PrivacyAndIntegrity

 [체크 2] RPC 호출 시 - 전송 레벨 (oauth/oauth.go 각 구현체)
     시점: GetRequestMetadata() 내부
     대상: 자기 자신의 보안 요구사항
     조건: CheckSecurityLevel(ri.AuthInfo, PrivacyAndIntegrity)
     체크: 현재 연결의 보안 레벨 확인

 [체크 3] RPC 호출 시 - 호출 레벨 (http2_client.go:698-701)
     시점: getCallAuthData() 내부
     대상: Call 옵션의 PerRPCCredentials
     조건: RequireTransportSecurity() == true && isSecure
     체크: t.isSecure && CheckSecurityLevel(ri.AuthInfo, PrivacyAndIntegrity)
```

### 15.2 CheckSecurityLevel 함수 상세

```go
// 소스: credentials/credentials.go:290-308
func CheckSecurityLevel(ai AuthInfo, level SecurityLevel) error {
    type internalInfo interface {
        GetCommonAuthInfo() CommonAuthInfo
    }
    if ai == nil {
        return errors.New("AuthInfo is nil")
    }
    if ci, ok := ai.(internalInfo); ok {
        // 하위 호환: zero value는 통과
        if ci.GetCommonAuthInfo().SecurityLevel == InvalidSecurityLevel {
            return nil
        }
        // 실제 비교
        if ci.GetCommonAuthInfo().SecurityLevel < level {
            return fmt.Errorf("requires SecurityLevel %v; connection has %v",
                level, ci.GetCommonAuthInfo().SecurityLevel)
        }
    }
    // GetCommonAuthInfo 미구현 → 통과 (하위 호환)
    return nil
}
```

**하위 호환성 전략:**

이 함수는 3가지 "통과" 경로를 제공한다:
1. `AuthInfo`가 `GetCommonAuthInfo()`를 구현하지 않는 경우 (오래된 구현체)
2. `SecurityLevel`이 `InvalidSecurityLevel`(0, zero value)인 경우 (초기화 안 된 구현체)
3. 실제 비교에서 `SecurityLevel >= level`인 경우 (정상)

이 설계 덕분에 `CommonAuthInfo`를 도입하기 전에 작성된 커스텀 크레덴셜도 깨지지 않고 동작한다.

### 15.3 보안 레벨 매핑 테이블

| 크레덴셜 구현 | AuthType | SecurityLevel | 비고 |
|-------------|----------|---------------|------|
| TLS (`tlsCreds`) | "tls" | PrivacyAndIntegrity (3) | 항상 최고 레벨 |
| Insecure (`insecureTC`) | "insecure" | NoSecurity (1) | 최저 레벨 |
| Local (UDS) (`localTC`) | "local" | PrivacyAndIntegrity (3) | UDS일 때 |
| Local (TCP 127.0.0.1) | "local" | NoSecurity (1) | 로컬 TCP |
| ALTS (`altsTC`) | "alts" | ALTS 프로토콜에 의해 결정 | GCP 전용 |

---

## 16. 내부 유틸리티

```
소스: internal/credentials/
```

### 16.1 CloneTLSConfig

```go
// 소스: internal/credentials/util.go:46-51
func CloneTLSConfig(cfg *tls.Config) *tls.Config {
    if cfg == nil {
        return &tls.Config{}
    }
    return cfg.Clone()
}
```

**왜 직접 `cfg.Clone()`을 호출하지 않는가?**

`cfg`가 `nil`일 수 있기 때문이다. `NewTLS(nil)`을 호출하면 시스템 기본 CA를 사용하는 TLS 설정이 생성되는데, 이때 `nil.Clone()`은 패닉을 발생시킨다.

### 16.2 AppendH2ToNextProtos

```go
// 소스: internal/credentials/util.go:28-34
func AppendH2ToNextProtos(ps []string) []string {
    for _, p := range ps {
        if p == alpnProtoStrH2 {
            return ps  // 이미 있으면 추가하지 않음
        }
    }
    return append(ps, alpnProtoStrH2)
}
```

ALPN(Application-Layer Protocol Negotiation)에서 `h2`를 협상하기 위해, `NextProtos`에 `"h2"`를 추가한다. 이미 있으면 중복 추가하지 않는다.

### 16.3 WrapSyscallConn

```go
// 소스: internal/credentials/syscallconn.go:49-55
func WrapSyscallConn(rawConn, newConn net.Conn) net.Conn {
    sysConn, ok := rawConn.(syscall.Conn)
    if !ok {
        return newConn
    }
    return &syscallConn{
        Conn:    newConn,
        sysConn: sysConn,
    }
}
```

TLS 핸드셰이크 후 원래 TCP 연결의 `syscall.Conn` 인터페이스(파일 디스크립터 접근)를 보존한다. 이는 커널 수준 최적화(예: `SO_KEEPALIVE`, `TCP_NODELAY`)를 TLS 연결에서도 사용할 수 있게 한다.

### 16.4 SPIFFEIDFromState

```go
// 소스: internal/credentials/spiffe.go:36-41
func SPIFFEIDFromState(state tls.ConnectionState) *url.URL {
    if len(state.PeerCertificates) == 0 || len(state.PeerCertificates[0].URIs) == 0 {
        return nil
    }
    return SPIFFEIDFromCert(state.PeerCertificates[0])
}
```

서비스 메시 환경(Istio, Linkerd 등)에서 사용되는 SPIFFE(Secure Production Identity Framework For Everyone) ID를 TLS 인증서의 SAN(Subject Alternative Name) URI에서 추출한다.

### 16.5 ClientHandshakeInfo 컨텍스트

```go
// 소스: internal/credentials/credentials.go:23-35
type clientHandshakeInfoKey struct{}

func ClientHandshakeInfoFromContext(ctx context.Context) any {
    return ctx.Value(clientHandshakeInfoKey{})
}

func NewClientHandshakeInfoContext(ctx context.Context, chi any) context.Context {
    return context.WithValue(ctx, clientHandshakeInfoKey{}, chi)
}
```

resolver나 balancer에서 전달한 주소 속성(Attributes)을 핸드셰이크 함수까지 전달하는 메커니즘이다. 클라이언트 전송 계층에서 설정된다:

```go
// 소스: internal/transport/http2_client.go:220
connectCtx = icredentials.NewClientHandshakeInfoContext(connectCtx,
    credentials.ClientHandshakeInfo{Attributes: addr.Attributes})
```

---

## 17. 설계 원칙과 Why

### 17.1 인터페이스 기반 추상화

gRPC-Go의 credentials 시스템은 철저한 인터페이스 기반 설계를 따른다. 구체적인 구현(TLS, ALTS 등)은 인터페이스 뒤에 숨어 있으며, 사용자는 인터페이스만 알면 된다.

**왜?** gRPC는 다양한 환경(GCP, AWS, 온프레미스, 서비스 메시)에서 동작해야 한다. 각 환경은 고유한 보안 요구사항이 있으므로, 구현을 교체 가능하게 만드는 것이 필수적이다.

### 17.2 명시적 보안 선택

insecure 크레덴셜도 명시적으로 생성해야 한다. "보안 없음"이 기본값이 아니라, 의도적 선택이다.

**왜?** 프로덕션 환경에서 실수로 보안 없이 서비스를 노출하는 것은 심각한 보안 사고다. 개발자가 `insecure.NewCredentials()`를 직접 호출하게 함으로써, "이 연결은 보안 없이 사용한다"는 것을 코드에서 명시적으로 표현하게 된다.

### 17.3 하위 호환성 우선

`CheckSecurityLevel`에서 `InvalidSecurityLevel`을 통과시키고, `GetCommonAuthInfo()`를 구현하지 않는 AuthInfo도 통과시키는 것은 모두 하위 호환성을 위한 것이다.

**왜?** gRPC-Go는 널리 사용되는 라이브러리이며, 커스텀 크레덴셜 구현이 많다. 새로운 보안 레벨 시스템을 도입하면서 기존 구현을 깨뜨리면 안 된다. "파괴 없는 발전(non-breaking evolution)"이 핵심 원칙이다.

### 17.4 컨텍스트를 통한 정보 전달

`RequestInfo`, `ClientHandshakeInfo` 등 보안 관련 정보는 모두 `context.Context`를 통해 전달된다.

**왜?** Go의 관용적 패턴을 따르면서, 함수 시그니처를 변경하지 않고도 새로운 정보를 전달할 수 있다. 인터페이스 메서드의 시그니처는 한번 정해지면 바꾸기 어렵지만, 컨텍스트에 새로운 키-값을 추가하는 것은 하위 호환이 가능하다.

### 17.5 두 계층의 인증 분리

TransportCredentials(연결 단위)와 PerRPCCredentials(RPC 단위)의 분리는 "관심사의 분리(Separation of Concerns)" 원칙의 적용이다.

**왜?** 연결 보안(TLS 등)은 인프라팀이 관리하고, RPC 인증(OAuth2 등)은 애플리케이션팀이 관리하는 것이 일반적이다. 두 관심사를 분리함으로써 각 팀이 독립적으로 보안 정책을 변경할 수 있다.

---

## 참고 파일 경로 요약

| 파일 | 역할 |
|------|------|
| `credentials/credentials.go` | 핵심 인터페이스 (TransportCredentials, PerRPCCredentials, AuthInfo, Bundle) |
| `credentials/tls.go` | TLS 구현 (tlsCreds, TLSInfo, NewTLS, applyDefaults) |
| `credentials/insecure/insecure.go` | 비보안 구현 (insecureTC, insecureBundle) |
| `credentials/local/local.go` | 로컬 연결 구현 (localTC, getSecurityLevel) |
| `credentials/oauth/oauth.go` | OAuth2 구현 (TokenSource, jwtAccess, serviceAccount) |
| `credentials/alts/alts.go` | ALTS 구현 (altsTC, checkRPCVersions) |
| `credentials/google/google.go` | Google Default Credentials Bundle (creds, dualPerRPCCreds) |
| `internal/credentials/credentials.go` | ClientHandshakeInfo 컨텍스트 헬퍼 |
| `internal/credentials/util.go` | CloneTLSConfig, AppendH2ToNextProtos |
| `internal/credentials/syscallconn.go` | WrapSyscallConn |
| `internal/credentials/spiffe.go` | SPIFFEIDFromState |
| `internal/transport/http2_client.go` | 클라이언트 핸드셰이크 흐름, getTrAuthData, getCallAuthData |
| `internal/transport/http2_server.go` | 서버 핸드셰이크 흐름 (NewServerTransport) |
| `server.go` | handleRawConn, newHTTP2Transport |
| `internal/internal.go` | CredsBundleMode 상수 |
