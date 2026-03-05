# 16. 인증과 암호화 (Authentication & Encryption)

## 목차
1. [개요](#1-개요)
2. [인증 아키텍처](#2-인증-아키텍처)
3. [Mutual Auth (SPIFFE/SPIRE)](#3-mutual-auth-spiffespire)
4. [Auth Map (BPF)](#4-auth-map-bpf)
5. [IPsec 암호화](#5-ipsec-암호화)
6. [WireGuard 암호화](#6-wireguard-암호화)
7. [IPsec vs WireGuard 비교](#7-ipsec-vs-wireguard-비교)
8. [BPF 데이터패스 통합](#8-bpf-데이터패스-통합)
9. [키 관리와 로테이션](#9-키-관리와-로테이션)
10. [왜 이 아키텍처인가?](#10-왜-이-아키텍처인가)
11. [참고 파일 목록](#11-참고-파일-목록)

---

## 1. 개요

Cilium의 인증/암호화 서브시스템은 크게 세 가지 축으로 구성된다.

| 축 | 역할 | 핵심 구현 |
|---|---|---|
| **Identity-based Authentication** | 워크로드 간 상호 인증 (mTLS) | SPIFFE/SPIRE 기반 mutual auth |
| **IPsec Encryption** | 노드 간 패킷 암호화 (커널 XFRM) | Linux XFRM policy/state 관리 |
| **WireGuard Encryption** | 노드 간 터널 암호화 | WireGuard 커널 모듈 (cilium_wg0) |

이 세 메커니즘은 독립적으로 동작하거나 조합하여 사용할 수 있다.

```
+-------------------------------------------------------------------+
|                     Cilium Agent (Userspace)                      |
|                                                                   |
|  +----------------+   +----------------+   +------------------+   |
|  | AuthManager    |   | IPsec Agent    |   | WireGuard Agent  |   |
|  | (mutual auth)  |   | (XFRM 관리)    |   | (wg0 관리)       |   |
|  +-------+--------+   +-------+--------+   +--------+---------+   |
|          |                     |                     |             |
|  +-------v--------+   +-------v--------+   +--------v---------+   |
|  | cilium_auth_map|   | XFRM Policy/   |   | WireGuard Peer   |   |
|  | (BPF HashMap)  |   | State (Kernel) |   | Config (Kernel)  |   |
|  +-------+--------+   +-------+--------+   +--------+---------+   |
+-----------|-----------------------|------------------|-----------+
            |                       |                  |
            v                       v                  v
     [Policy 판정에서     [패킷 암호화/복호화]   [터널 암호화/복호화]
      인증 상태 확인]
```

**핵심 설계 원칙**: 인증(Authentication)과 암호화(Encryption)를 분리한다. 인증은 "누구인가"를 검증하고, 암호화는 "전송 중 기밀성"을 보장한다. Cilium은 이 둘을 독립적인 레이어로 구현하여 각각을 선택적으로 활성화할 수 있게 한다.

---

## 2. 인증 아키텍처

### 2.1 AuthManager 구조

인증의 중심에는 `AuthManager`가 있다. BPF 데이터패스가 인증이 필요한 트래픽을 감지하면 시그널을 보내고, `AuthManager`가 이를 처리한다.

**소스**: `pkg/auth/manager.go` (30-40행)
```go
type AuthManager struct {
    logger                *slog.Logger
    nodeIDHandler         types.NodeIDHandler
    authHandlers          map[policyTypes.AuthType]authHandler
    authmap               authMapCacher
    authSignalBackoffTime time.Duration

    mutex                    lock.Mutex
    pending                  map[authKey]struct{}
    handleAuthenticationFunc func(a *AuthManager, k authKey, reAuth bool)
}
```

| 필드 | 역할 |
|------|------|
| `nodeIDHandler` | 원격 노드 ID를 IP 주소로 변환 |
| `authHandlers` | AuthType별 핸들러 맵 (예: SPIRE 핸들러) |
| `authmap` | BPF cilium_auth_map 캐시 인터페이스 |
| `pending` | 현재 진행 중인 인증 요청의 중복 방지 맵 |
| `authSignalBackoffTime` | 동일 키에 대한 재인증 방지를 위한 백오프 시간 |

### 2.2 AuthType 열거형

**소스**: `pkg/policy/types/auth.go` (15-22행)
```go
type AuthType uint8

const (
    AuthTypeDisabled  AuthType = iota   // 0: 인증 불필요
    AuthTypeSpire                       // 1: SPIFFE/SPIRE mutual auth
    AuthTypeAlwaysFail                  // 2: 항상 거부 (테스트용)
)
```

`AuthRequirement`는 `AuthType`에 "명시적(explicit)" 플래그를 결합한 타입이다.

**소스**: `pkg/policy/types/auth.go` (32-37행)
```go
type AuthRequirement AuthType

const (
    NoAuthRequirement  AuthRequirement = 0
    AuthTypeIsExplicit AuthRequirement = 1 << 7  // 비트 7: 명시적 설정 플래그
)
```

이 설계의 의미는 다음과 같다:
- **비트 0-6**: AuthType 값 (0=disabled, 1=spire, 2=always-fail)
- **비트 7**: 해당 정책이 auth_type을 명시적으로 설정했는지 여부

### 2.3 authHandler 인터페이스

**소스**: `pkg/auth/manager.go` (43-48행)
```go
type authHandler interface {
    authenticate(*authRequest) (*authResponse, error)
    authType() policyTypes.AuthType
    subscribeToRotatedIdentities() <-chan certs.CertificateRotationEvent
    certProviderStatus() *models.Status
}
```

모든 인증 핸들러는 이 인터페이스를 구현해야 한다. 현재는 `mutualAuthHandler`가 유일한 구현체이며, `AuthTypeSpire`를 처리한다.

### 2.4 인증 흐름

전체 인증 흐름은 다음과 같은 순서로 진행된다:

```
 패킷 도착 (BPF)
     |
     v
 정책 검사: auth_type 필드 확인
     |
     v
 auth_type > 0 ?
     |
    YES ──> cilium_auth_map에서 인증 캐시 조회
              |
              v
         캐시 존재 & 유효? ──YES──> 패킷 통과 (CTX_ACT_OK)
              |
              NO
              |
              v
         DROP_POLICY_AUTH_REQUIRED 반환
         + send_signal_auth_required() 시그널 전송
              |
              v
         Agent가 시그널 수신
              |
              v
         AuthManager.handleAuthRequest() 호출
              |
              v
         pending 맵에서 중복 확인
              |
              v
         goroutine으로 authenticate() 실행
              |
              v
         mutualAuthHandler.authenticate()
         (TLS 핸드셰이크)
              |
              v
         성공 시 cilium_auth_map 업데이트
              |
              v
         다음 패킷부터 BPF에서 캐시 히트
```

**소스**: `pkg/auth/manager.go` (84-101행) - 시그널 수신 처리
```go
func (a *AuthManager) handleAuthRequest(_ context.Context, key signalAuthKey) error {
    k := authKey{
        localIdentity:  identity.NumericIdentity(key.LocalIdentity),
        remoteIdentity: identity.NumericIdentity(key.RemoteIdentity),
        remoteNodeID:   key.RemoteNodeID,
        authType:       policyTypes.AuthType(key.AuthType),
    }
    // Reserved identity는 인증 불가 — 스킵
    if k.localIdentity.IsReservedIdentity() || k.remoteIdentity.IsReservedIdentity() {
        return nil
    }
    a.handleAuthenticationFunc(a, k, false)
    return nil
}
```

**소스**: `pkg/auth/manager.go` (128-162행) - 인증 실행 (goroutine)
```go
func handleAuthentication(a *AuthManager, k authKey, reAuth bool) {
    if !a.markPendingAuth(k) {
        return  // 이미 진행 중인 인증이 있으면 스킵
    }
    go func(key authKey) {
        defer a.clearPendingAuth(key)
        // backoff 시간 내에 이미 인증된 캐시가 있으면 스킵
        if !reAuth {
            if i, err := a.authmap.GetCacheInfo(key); err == nil &&
                i.expiration.After(time.Now()) &&
                time.Now().Before(i.storedAt.Add(a.authSignalBackoffTime)) {
                return
            }
        }
        if err := a.authenticate(key); err != nil {
            // 인증 실패 로그
        }
    }(k)
}
```

### 2.5 인증 결과 업데이트

**소스**: `pkg/auth/manager.go` (188-235행)
```go
func (a *AuthManager) authenticate(key authKey) error {
    h, ok := a.authHandlers[key.authType]
    if !ok {
        return fmt.Errorf("unknown requested auth type: %s", key.authType)
    }
    nodeIP := a.nodeIDHandler.GetNodeIP(key.remoteNodeID)
    authReq := &authRequest{
        localIdentity:  key.localIdentity,
        remoteIdentity: key.remoteIdentity,
        remoteNodeIP:   nodeIP,
    }
    authResp, err := h.authenticate(authReq)
    if err != nil {
        return fmt.Errorf("failed to authenticate with auth type %s: %w", key.authType, err)
    }
    // BPF auth map에 인증 결과(만료 시간) 기록
    return a.updateAuthMap(key, authResp.expirationTime)
}
```

인증 성공 시 `cilium_auth_map`에 만료 시간을 기록한다. 이후 동일한 (local_id, remote_id, remote_node_id, auth_type) 조합의 패킷은 BPF에서 바로 통과한다.

---

## 3. Mutual Auth (SPIFFE/SPIRE)

### 3.1 mutualAuthHandler 구조

**소스**: `pkg/auth/mutual_authhandler.go` (78-89행)
```go
type mutualAuthHandler struct {
    cell.In
    cfg             MutualAuthConfig
    log             *slog.Logger
    cert            certs.CertificateProvider
    cancelSocketListen context.CancelFunc
    endpointManager endpointGetter
}
```

| 필드 | 역할 |
|------|------|
| `cfg` | 리스너 포트, 연결 타임아웃 설정 |
| `cert` | SPIRE 인증서 제공자 (CertificateProvider 인터페이스) |
| `cancelSocketListen` | 리스너 소켓 종료 함수 |
| `endpointManager` | 로컬 엔드포인트 목록 조회 |

### 3.2 MutualAuthConfig

**소스**: `pkg/auth/mutual_authhandler.go` (66-76행)
```go
type MutualAuthConfig struct {
    MutualAuthListenerPort   int           `mapstructure:"mesh-auth-mutual-listener-port"`
    MutualAuthConnectTimeout time.Duration `mapstructure:"mesh-auth-mutual-connect-timeout"`
}
```

- `mesh-auth-mutual-listener-port`: Agent가 mutual auth 핸드셰이크를 수행할 TCP 포트 (기본값 0 = 비활성)
- `mesh-auth-mutual-connect-timeout`: 원격 노드에 TCP 연결 시 타임아웃 (기본값 5초)

### 3.3 CertificateProvider 인터페이스

**소스**: `pkg/auth/certs/provider.go` (19-44행)
```go
type CertificateProvider interface {
    GetTrustBundle() (*x509.CertPool, error)
    GetCertificateForIdentity(id identity.NumericIdentity) (*tls.Certificate, error)
    ValidateIdentity(id identity.NumericIdentity, cert *x509.Certificate) (bool, error)
    NumericIdentityToSNI(id identity.NumericIdentity) string
    SNIToNumericIdentity(sni string) (identity.NumericIdentity, error)
    SubscribeToRotatedIdentities() <-chan CertificateRotationEvent
    Status() *models.Status
}
```

이 인터페이스는 SPIRE 또는 다른 인증서 제공자를 추상화한다:

| 메서드 | 역할 |
|--------|------|
| `GetTrustBundle()` | CA 인증서 번들 (신뢰 앵커) 반환 |
| `GetCertificateForIdentity()` | Cilium Identity에 해당하는 TLS 인증서 반환 |
| `ValidateIdentity()` | 상대방 인증서의 SAN을 Cilium Identity와 대조 |
| `NumericIdentityToSNI()` | Cilium Identity를 SNI 문자열로 변환 |
| `SNIToNumericIdentity()` | SNI 문자열을 Cilium Identity로 역변환 |
| `SubscribeToRotatedIdentities()` | 인증서 갱신 이벤트 구독 채널 |

### 3.4 클라이언트 측 인증 (authenticate)

**소스**: `pkg/auth/mutual_authhandler.go` (91-160행)

클라이언트 측 핸드셰이크는 다음 순서로 진행된다:

```
 클라이언트 (인증 요청 측)
     |
     v
 1. cert.GetCertificateForIdentity(localIdentity)
    → 로컬 워크로드의 SPIFFE 인증서 획득
     |
     v
 2. cert.GetTrustBundle()
    → CA 번들 획득
     |
     v
 3. net.DialTimeout("tcp", remoteNodeIP:port, 5s)
    → 원격 노드에 TCP 연결
     |
     v
 4. tls.Client(conn, &tls.Config{...})
    → TLS 1.3 핸드셰이크 시작
    → ServerName: NumericIdentityToSNI(remoteIdentity)
    → GetClientCertificate: 로컬 인증서 제공
    → VerifyPeerCertificate: 상대방 인증서 수동 검증
     |
     v
 5. verifyPeerCertificate()
    → x509 체인 검증 + Identity SAN 매칭
     |
     v
 6. 두 인증서의 만료 시간 중 더 이른 것을 반환
```

핵심 코드:
```go
func (m *mutualAuthHandler) authenticate(ar *authRequest) (*authResponse, error) {
    clientCert, _ := m.cert.GetCertificateForIdentity(ar.localIdentity)
    caBundle, _ := m.cert.GetTrustBundle()

    conn, _ := net.DialTimeout("tcp",
        net.JoinHostPort(ar.remoteNodeIP, strconv.Itoa(m.cfg.MutualAuthListenerPort)),
        m.cfg.MutualAuthConnectTimeout)
    defer conn.Close()

    var expirationTime = &clientCert.Leaf.NotAfter

    tlsConn := tls.Client(conn, &tls.Config{
        ServerName: m.cert.NumericIdentityToSNI(ar.remoteIdentity),
        GetClientCertificate: func(info *tls.CertificateRequestInfo) (*tls.Certificate, error) {
            return clientCert, nil
        },
        MinVersion:         tls.VersionTLS13,
        InsecureSkipVerify: true,  // 수동 검증 사용
        VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
            // x509 파싱 → verifyPeerCertificate() 호출
            // 만료 시간이 더 짧은 쪽으로 갱신
            ...
        },
        ClientCAs: caBundle,
        RootCAs:   caBundle,
    })
    defer tlsConn.Close()
    tlsConn.Handshake()

    return &authResponse{expirationTime: *expirationTime}, nil
}
```

**왜 `InsecureSkipVerify: true`인가?**
Go의 기본 TLS 검증은 DNS 이름 기반이지만, Cilium은 SPIFFE ID(URI SAN) 기반으로 검증해야 한다. 따라서 Go 기본 검증을 끄고 `VerifyPeerCertificate` 콜백에서 직접 x509 체인 검증 + SPIFFE ID 매칭을 수행한다. 이는 "안전하지 않은" 것이 아니라 "다른 방식의 검증"이다.

### 3.5 서버 측 연결 처리

**소스**: `pkg/auth/mutual_authhandler.go` (166-220행)

```
 서버 (인증 응답 측)
     |
     v
 1. listenForConnections()
    → TCP 리스너 시작 (cfg.MutualAuthListenerPort)
     |
     v
 2. conn = l.Accept()
    → 새 연결 수신
     |
     v
 3. go handleConnection(ctx, conn)
    → goroutine으로 처리
     |
     v
 4. tls.Server(conn, &tls.Config{
        ClientAuth: tls.RequireAndVerifyClientCert,
        GetCertificate: GetCertificateForIncomingConnection,
        MinVersion: tls.VersionTLS13,
        ClientCAs: caBundle,
    })
     |
     v
 5. GetCertificateForIncomingConnection()
    → SNI에서 Identity 추출
    → 로컬 엔드포인트에 해당 Identity가 있는지 확인
    → 매칭되는 인증서 반환
     |
     v
 6. TLS 핸드셰이크 완료
```

**소스**: `pkg/auth/mutual_authhandler.go` (222-247행) - SNI 기반 인증서 선택
```go
func (m *mutualAuthHandler) GetCertificateForIncomingConnection(info *tls.ClientHelloInfo) (*tls.Certificate, error) {
    id, _ := m.cert.SNIToNumericIdentity(info.ServerName)

    // 로컬 엔드포인트에서 해당 Identity 존재 여부 확인
    localEPs := m.endpointManager.GetEndpoints()
    matched := false
    for _, ep := range localEPs {
        if ep.SecurityIdentity != nil && ep.SecurityIdentity.ID == id {
            matched = true
            break
        }
    }
    if !matched {
        return nil, fmt.Errorf("no local endpoint present for identity %s", id.String())
    }
    return m.cert.GetCertificateForIdentity(id)
}
```

이 검증은 중요한 보안 역할을 한다: 원격 노드가 자신이 가지고 있지 않은 Identity의 인증서를 요청하는 것을 방지한다.

### 3.6 피어 인증서 검증

**소스**: `pkg/auth/mutual_authhandler.go` (268-311행)
```go
func (m *mutualAuthHandler) verifyPeerCertificate(
    id *identity.NumericIdentity,
    caBundle *x509.CertPool,
    certChains [][]*x509.Certificate,
) (*time.Time, error) {
    for _, chain := range certChains {
        opts := x509.VerifyOptions{
            Roots:         caBundle,
            Intermediates: x509.NewCertPool(),
        }
        var leaf *x509.Certificate
        for _, cert := range chain {
            if cert.IsCA {
                opts.Intermediates.AddCert(cert)
            } else {
                leaf = cert
            }
        }
        // 1. x509 체인 검증
        leaf.Verify(opts)
        // 2. SPIFFE Identity 매칭
        if id != nil {
            valid, _ := m.cert.ValidateIdentity(*id, leaf)
            if !valid {
                return nil, fmt.Errorf("unable to validate SAN")
            }
        }
        expirationTime = &leaf.NotAfter
    }
    return expirationTime, nil
}
```

검증 과정:
1. CA 번들로 인증서 체인 유효성 검증 (x509 표준 검증)
2. leaf 인증서의 SAN(Subject Alternative Name)에서 SPIFFE ID 추출
3. 기대하는 Cilium Identity와 SPIFFE ID 매칭 확인

### 3.7 인증서 로테이션 처리

**소스**: `pkg/auth/manager.go` (104-126행)
```go
func (a *AuthManager) handleCertificateRotationEvent(_ context.Context, event certs.CertificateRotationEvent) error {
    all, _ := a.authmap.All()
    for k := range all {
        if k.localIdentity == event.Identity || k.remoteIdentity == event.Identity {
            if event.Deleted {
                // 인증서 삭제 → auth map 엔트리도 삭제
                a.authmap.Delete(k)
            } else {
                // 인증서 갱신 → 재인증 트리거
                a.handleAuthenticationFunc(a, k, true)
            }
        }
    }
    return nil
}
```

인증서가 갱신되면 해당 Identity와 관련된 모든 auth map 엔트리를 재인증한다. 삭제된 경우에는 auth map에서도 제거하여 해당 트래픽이 다시 인증 과정을 거치도록 한다.

---

## 4. Auth Map (BPF)

### 4.1 BPF 데이터 구조

**소스**: `bpf/lib/common.h` (180-191행)
```c
struct auth_key {
    __u32       local_sec_label;    // 로컬 워크로드의 Security Identity
    __u32       remote_sec_label;   // 원격 워크로드의 Security Identity
    __u16       remote_node_id;     // 원격 노드 ID (로컬 노드는 0)
    __u8        auth_type;          // 인증 타입 (1=SPIRE, 2=always-fail)
    __u8        pad;                // 정렬 패딩
};

struct auth_info {
    __u64       expiration;         // 만료 시간 (Unix epoch, ns/512 단위)
};
```

**왜 ns/512 단위인가?** BPF에서 시간 비교를 효율적으로 수행하기 위해 나노초를 512(2^9)로 나눈 값을 사용한다. 이는 약 9비트의 정밀도를 포기하는 대신, 64비트 정수로 표현 가능한 시간 범위를 확장하고 비교 연산을 단순화한다.

### 4.2 Go 측 맵 정의

**소스**: `pkg/maps/authmap/auth_map.go` (14-38행)
```go
const MapName = "cilium_auth_map"

type Map interface {
    Lookup(key AuthKey) (AuthInfo, error)
    Update(key AuthKey, expiration utime.UTime) error
    Delete(key AuthKey) error
    IterateWithCallback(cb IterateCallback) error
    MaxEntries() uint32
}
```

**소스**: `pkg/maps/authmap/auth_map.go` (89-113행)
```go
type AuthKey struct {
    LocalIdentity  uint32 `align:"local_sec_label"`
    RemoteIdentity uint32 `align:"remote_sec_label"`
    RemoteNodeID   uint16 `align:"remote_node_id"`
    AuthType       uint8  `align:"auth_type"`
    Pad            uint8  `align:"pad"`
}

type AuthInfo struct {
    Expiration utime.UTime `align:"expiration"`
}
```

`align` 태그는 BPF 맵의 C 구조체와 Go 구조체의 메모리 레이아웃을 일치시킨다. 이 정렬이 깨지면 BPF-userspace 간 데이터 교환이 실패한다.

### 4.3 BPF auth_lookup() 함수

**소스**: `bpf/lib/auth.h` (12-54행)

```c
/* Global auth map for enforcing authentication policy */
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __type(key, struct auth_key);
    __type(value, struct auth_info);
    __uint(pinning, LIBBPF_PIN_BY_NAME);
    __uint(max_entries, 524288);
    __uint(map_flags, BPF_F_NO_PREALLOC);
} cilium_auth_map __section_maps_btf;

static __always_inline int
auth_lookup(struct __ctx_buff *ctx, __u32 local_id, __u32 remote_id,
            __u32 remote_node_ip, __u8 auth_type)
{
    const struct node_value *node_value = NULL;
    struct auth_info *auth;
    struct auth_key key = {
        .local_sec_label = local_id,
        .remote_sec_label = remote_id,
        .auth_type = auth_type,
        .pad = 0,
    };

    if (remote_node_ip) {
        node_value = lookup_ip4_node(remote_node_ip);
        if (!node_value || !node_value->id)
            return DROP_NO_NODE_ID;
        key.remote_node_id = node_value->id;
    } else {
        key.remote_node_id = 0;  // 로컬 노드
    }

    auth = map_lookup_elem(&cilium_auth_map, &key);
    if (likely(auth)) {
        if (utime_get_time() < auth->expiration)
            return CTX_ACT_OK;           // 캐시 히트 & 유효 → 통과
    }

    send_signal_auth_required(ctx, &key); // Agent에 시그널 전송
    return DROP_POLICY_AUTH_REQUIRED;      // 패킷 드롭
}
```

| 맵 속성 | 값 | 의미 |
|---------|-----|------|
| 타입 | `BPF_MAP_TYPE_HASH` | 키-값 해시맵 |
| max_entries | 524288 (512K) | 최대 엔트리 수 |
| map_flags | `BPF_F_NO_PREALLOC` | 필요 시에만 메모리 할당 |
| pinning | `LIBBPF_PIN_BY_NAME` | BPF 파일시스템에 이름으로 고정 |

### 4.4 시그널 메커니즘

**소스**: `bpf/lib/signal.h` (64-68행)
```c
static __always_inline void send_signal_auth_required(
    struct __ctx_buff *ctx,
    const struct auth_key *auth)
{
    SEND_SIGNAL(ctx, SIGNAL_AUTH_REQUIRED, auth, *auth);
}
```

BPF에서 userspace로의 시그널은 `bpf_perf_event_output`을 통해 전달된다. Agent는 perf event ring buffer를 폴링하여 인증 요청을 수신한다.

```
 BPF 데이터패스                           Agent (Userspace)
 +-------------------+                   +------------------+
 | auth_lookup()     |                   | signalmap 폴링   |
 |   → 캐시 미스     |  perf event       |                  |
 |   → SEND_SIGNAL   | ────────────────> | handleAuthRequest|
 |   → DROP 반환     |                   |   → authenticate |
 +-------------------+                   |   → updateAuthMap|
                                         +------------------+
```

---

## 5. IPsec 암호화

### 5.1 IPsec Agent 구조

**소스**: `pkg/datapath/linux/ipsec/ipsec_linux.go` (137-167행)
```go
type Agent struct {
    ipSecLock lock.RWMutex

    log        *slog.Logger
    localNode  *node.LocalNodeStore
    jobs       job.Group
    config     Config
    encryptMap encrypt.EncryptMap

    authKeySize int
    spi         uint8
    ipSecKeysGlobal      map[string]*ipSecKey
    ipSecCurrentKeySPI   uint8
    ipSecKeysRemovalTime map[uint8]time.Time
    xfrmStateCache       *xfrmStateListCache
}
```

| 필드 | 역할 |
|------|------|
| `spi` | 현재 활성 SPI (Security Parameter Index) |
| `ipSecKeysGlobal` | IP별 글로벌 IPsec 키 맵 (빈 문자열 = 기본 키) |
| `ipSecCurrentKeySPI` | 현재 사용 중인 키의 SPI |
| `ipSecKeysRemovalTime` | 키 교체 시 이전 키의 제거 시간 추적 |
| `xfrmStateCache` | XFRM state 캐시 (불필요한 커널 쿼리 방지) |

### 5.2 ipSecKey 구조

**소스**: `pkg/datapath/linux/ipsec/ipsec_linux.go` (82-89행)
```go
type ipSecKey struct {
    Spi    uint8
    KeyLen int
    ReqID  int
    Auth   *netlink.XfrmStateAlgo   // 인증 알고리즘 (hmac-sha256 등)
    Crypt  *netlink.XfrmStateAlgo   // 암호화 알고리즘 (aes-cbc 등)
    Aead   *netlink.XfrmStateAlgo   // AEAD 알고리즘 (rfc4106-gcm-aes 등)
}
```

IPsec 키는 두 가지 포맷을 지원한다:
1. **AEAD 포맷**: `[spi] aead-algo aead-key icv-len`
2. **분리 포맷**: `[spi] auth-algo auth-key enc-algo enc-key [IP]`

### 5.3 IPsec 키 파일 로딩

**소스**: `pkg/datapath/linux/ipsec/ipsec_linux.go` (1057-1064행)
```go
func (a *Agent) loadIPSecKeysFile(path string) (int, uint8, error) {
    file, _ := os.Open(path)
    defer file.Close()
    return a.LoadIPSecKeys(file)
}
```

**소스**: `pkg/datapath/linux/ipsec/ipsec_linux.go` (1066-1150행)

키 파일은 라인 단위로 파싱하며, 각 라인은 하나의 IPsec 키를 정의한다:
```
# AEAD 포맷 예시
3 rfc4106(gcm(aes)) 0x<hex-key> 128

# 분리 포맷 예시
3 hmac(sha256) 0x<auth-key> cbc(aes) 0x<enc-key>
```

### 5.4 노드별 키 파생

**소스**: `pkg/datapath/linux/ipsec/ipsec_linux.go` (254-272행)
```go
func computeNodeIPsecKey(globalKey, srcNodeIP, dstNodeIP, srcBootID, dstBootID []byte) []byte {
    inputLen := len(globalKey) + len(srcNodeIP) + len(dstNodeIP) + len(srcBootID) + len(dstBootID)
    input := make([]byte, 0, inputLen)
    input = append(input, globalKey...)
    input = append(input, srcNodeIP...)
    input = append(input, dstNodeIP...)
    input = append(input, srcBootID[:36]...)
    input = append(input, dstBootID[:36]...)

    var hash []byte
    if len(globalKey) <= 32 {
        h := sha256.Sum256(input)
        hash = h[:]
    } else {
        h := sha512.Sum512(input)
        hash = h[:]
    }
    return hash[:len(globalKey)]
}
```

**왜 노드별로 키를 파생하는가?**
전체 클러스터가 동일한 PSK(Pre-Shared Key)를 사용하면 한 노드가 침해되었을 때 모든 노드 간 통신이 위험해진다. 노드 IP와 Boot ID를 해시 입력에 포함하여 각 노드 쌍이 고유한 키를 갖게 함으로써, 하나의 노드가 침해되더라도 다른 노드 쌍의 통신에는 영향을 미치지 않는다.

### 5.5 XFRM Policy/State 관리

**소스**: `pkg/datapath/linux/ipsec/ipsec_linux.go` (869-914행)
```go
func (a *Agent) UpsertIPsecEndpoint(params *types.IPSecParameters) (uint8, error) {
    if !params.SourceTunnelIP.Equal(*params.DestTunnelIP) {
        if params.Dir&IPSecDirIn != 0 {
            spi, _ = a.ipSecReplaceStateIn(params)
            a.ipSecReplacePolicyIn(params)
        }
        if params.Dir&IPSecDirFwd != 0 {
            a.ipsecReplacePolicyFwd(params)
        }
        if params.Dir&IPSecDirOut != 0 {
            spi, _ = a.ipSecReplaceStateOut(params)
            a.ipSecReplacePolicyOut(params)
        }
    }
    return spi, nil
}
```

XFRM(Transform) 프레임워크에서의 IPsec 구조:

```
 노드 A (10.0.1.1)                              노드 B (10.0.2.1)
 +---------------------------+                   +---------------------------+
 | XFRM Policy (OUT):       |                   | XFRM Policy (IN):        |
 |   src=10.0.1.0/24        |                   |   src=10.0.1.0/24        |
 |   dst=10.0.2.0/24        |                   |   dst=10.0.2.0/24        |
 |   dir=OUT                |                   |   dir=IN                 |
 |   tmpl(spi=3, reqid=1)   |                   |   tmpl(spi=3, reqid=1)   |
 +---------------------------+                   +---------------------------+
 | XFRM State (OUT):        |   ESP 패킷         | XFRM State (IN):        |
 |   dst=10.0.2.1           | ================> |   dst=10.0.2.1           |
 |   spi=3, reqid=1         |                   |   spi=3, reqid=1         |
 |   aead=gcm(aes)          |                   |   aead=gcm(aes)          |
 |   key=<derived-key>      |                   |   key=<derived-key>      |
 +---------------------------+                   +---------------------------+
```

### 5.6 Direction 상수

**소스**: `pkg/datapath/linux/ipsec/ipsec_linux.go` (47-49행)
```go
const (
    IPSecDirIn  types.IPSecDir = 1 << iota  // 1: 수신 방향
    IPSecDirOut                              // 2: 송신 방향
    IPSecDirFwd                              // 4: 포워딩 방향
)
```

### 5.7 기본 드롭 정책

**소스**: `pkg/datapath/linux/ipsec/ipsec_linux.go` (108-127행)
```go
var (
    defaultDropMark = &netlink.XfrmMark{
        Value: linux_defaults.RouteMarkEncrypt,
        Mask:  linux_defaults.IPsecMarkBitMask,
    }
    defaultDropPolicyIPv4 = &netlink.XfrmPolicy{
        Dir:      netlink.XFRM_DIR_OUT,
        Src:      wildcardCIDRv4,       // 0.0.0.0/0
        Dst:      wildcardCIDRv4,       // 0.0.0.0/0
        Mark:     defaultDropMark,
        Action:   netlink.XFRM_POLICY_BLOCK,
        Priority: defaultDropPriority,  // 100
    }
)
```

**왜 기본 드롭 정책이 필요한가?**
IPsec이 활성화된 상태에서 XFRM state가 아직 설정되지 않은 노드로 패킷이 나가면, 암호화되지 않은 채로 전송될 수 있다. 기본 드롭 정책은 "암호화 마크가 있지만 매칭되는 XFRM state가 없는" 패킷을 차단하여 평문 유출을 방지한다.

---

## 6. WireGuard 암호화

### 6.1 WireGuard Agent 구조

**소스**: `pkg/wireguard/agent/agent.go` (68-97행)
```go
type Agent struct {
    lock.RWMutex

    logger            *slog.Logger
    config            Config
    ipCache           *ipcache.IPCache
    sysctl            sysctl.Sysctl
    jobGroup          job.Group
    db                *statedb.DB
    mtuTable          statedb.Table[mtu.RouteMTU]
    localNode         *node.LocalNodeStore
    nodeManager       nodeManager.NodeManager
    nodeDiscovery     *nodediscovery.NodeDiscovery
    ipIdentityWatcher *ipcache.LocalIPIdentityWatcher
    clustermesh       *clustermesh.ClusterMesh
    cacheStatus       k8sSynced.CacheStatus

    listenPort       int
    privKeyPath      string
    peerByNodeName   map[string]*peerConfig
    nodeNameByNodeIP map[string]string
    nodeNameByPubKey map[wgtypes.Key]string

    optOut   bool
    privKey  wgtypes.Key
    wgClient wireguardClient
}
```

| 필드 | 역할 |
|------|------|
| `peerByNodeName` | 노드 이름 → 피어 설정 매핑 |
| `nodeNameByPubKey` | 공개키 → 노드 이름 매핑 (중복 키 감지) |
| `nodeNameByNodeIP` | 노드 IP → 노드 이름 매핑 |
| `privKey` | 로컬 노드의 WireGuard 개인키 |
| `wgClient` | WireGuard 커널 모듈 제어 클라이언트 |
| `listenPort` | WireGuard 리스닝 포트 (51871) |
| `privKeyPath` | 개인키 파일 경로 |

### 6.2 WireGuard 상수

**소스**: `pkg/wireguard/types/types.go` (12-17행)
```go
const (
    ListenPort      = 51871
    IfaceName       = "cilium_wg0"
    PrivKeyFilename = "cilium_wg0.key"
)
```

### 6.3 초기화 흐름

**소스**: `pkg/wireguard/agent/agent.go` (258-313행)

```
 Agent.Start()
     |
     v
 init()
     |
     +---> loadOrGeneratePrivKey(privKeyPath)
     |     → 파일 있으면 로드, 없으면 새 키 생성 후 저장
     |
     +---> netlink.LinkAdd(&netlink.Wireguard{Name: "cilium_wg0"})
     |     → WireGuard 네트워크 인터페이스 생성
     |
     +---> sysctl.Disable("net.ipv4.conf.cilium_wg0.rp_filter")
     |     → 역방향 경로 필터 비활성화
     |
     +---> wgctrl.New()
     |     → WireGuard 클라이언트 생성
     |
     +---> wgClient.ConfigureDevice("cilium_wg0", Config{
     |         PrivateKey:   &privKey,
     |         ListenPort:   &listenPort,     // 51871
     |         FirewallMark: MagicMarkWireGuardEncrypted,
     |     })
     |
     +---> netlink.LinkSetUp(link)
           → 인터페이스 활성화
```

### 6.4 개인키 관리

**소스**: `pkg/wireguard/agent/agent.go` (727-737행)
```go
func loadOrGeneratePrivKey(filePath string) (key wgtypes.Key, err error) {
    bytes, err := os.ReadFile(filePath)
    if os.IsNotExist(err) {
        key, _ = wgtypes.GeneratePrivateKey()
        os.WriteFile(filePath, key[:], 0600)
        return key, nil
    }
    // 기존 키 파싱...
}
```

키 파일은 `0600` 퍼미션으로 저장되어 root만 읽을 수 있다. 노드 재시작 시에도 동일한 공개키를 유지하여 피어 관계가 끊기지 않는다.

### 6.5 피어 관리

**소스**: `pkg/wireguard/agent/agent.go` (461-540행)

```go
func (a *Agent) updatePeer(nodeName, pubKeyHex string, nodeIPv4, nodeIPv6 net.IP) error {
    pubKey, _ := wgtypes.ParseKey(pubKeyHex)

    // 중복 공개키 감지
    if prevNodeName, ok := a.nodeNameByPubKey[pubKey]; ok {
        if nodeName != prevNodeName {
            return fmt.Errorf("detected duplicate public key")
        }
    }

    peer := a.peerByNodeName[nodeName]

    // 공개키 변경 시 기존 피어 삭제 후 재생성
    if peer != nil && peer.pubKey != pubKey {
        a.deletePeerByPubKey(peer.pubKey)
        peer = nil
    }

    // 새 피어 초기화
    if peer == nil {
        peer = &peerConfig{}
        if a.needsIPCache() {
            peer.queueAllowedIPsInsert(a.ipCache.LookupByHostRLocked(nodeIPv4, nodeIPv6)...)
        }
    }

    // AllowedIPs 업데이트 (노드 IP + 해당 노드의 Pod CIDR)
    ...
}
```

WireGuard 피어 관리 흐름:

```
 CiliumNode CR 변경 (또는 kvstore 이벤트)
     |
     v
 NodeManager → NodeHandler.NodeUpdate()
     |
     v
 WireGuard Agent.updatePeer()
     |
     +---> 공개키 파싱 및 검증
     |
     +---> 피어 설정 생성/업데이트
     |
     +---> AllowedIPs 계산
     |     (노드 IP + 해당 노드가 관리하는 Pod CIDR)
     |
     +---> wgClient.ConfigureDevice("cilium_wg0", Config{
               Peers: [{
                   PublicKey:  pubKey,
                   Endpoint:   nodeIP:51871,
                   AllowedIPs: [...],
               }],
           })
```

### 6.6 노드 암호화 옵트아웃

**소스**: `pkg/wireguard/agent/agent.go` (234-255행)
```go
func (a *Agent) initLocalNodeFromWireGuard(localNode *node.LocalNode, sel k8sLabels.Selector) {
    localNode.EncryptionKey = types.StaticEncryptKey
    localNode.WireguardPubKey = a.privKey.PublicKey().String()
    localNode.Annotations[annotation.WireguardPubKey] = localNode.WireguardPubKey

    if a.config.EncryptNode && sel.Matches(k8sLabels.Set(localNode.Labels)) {
        localNode.Local.OptOutNodeEncryption = true
        localNode.EncryptionKey = 0
    }
}
```

레이블 셀렉터를 통해 특정 노드가 노드 간 암호화에서 제외될 수 있다. 이는 성능 민감한 워크로드가 있는 노드에서 유용하다.

### 6.7 MTU 조정

**소스**: `pkg/wireguard/agent/agent.go` (317-347행)

WireGuard 오버헤드를 고려하여 MTU를 자동 조정한다:
```go
linkMTU := mtuRoute.DeviceMTU - mtu.WireguardOverhead
```

WireGuard 패킷 오버헤드는 약 60-80 바이트이며, 이를 차감한 MTU를 `cilium_wg0` 인터페이스에 설정한다.

---

## 7. IPsec vs WireGuard 비교

### 7.1 기능 비교

| 항목 | IPsec | WireGuard |
|------|-------|-----------|
| **커널 모듈** | XFRM (기본 내장) | wireguard (5.6+ 내장) |
| **키 관리** | PSK 파일 + 노드별 파생 | Curve25519 키쌍 자동 생성 |
| **프로토콜** | ESP (IP 프로토콜 50) | UDP 포트 51871 |
| **암호화 알고리즘** | AES-GCM, AES-CBC+HMAC-SHA256 | ChaCha20-Poly1305 (고정) |
| **상태 관리** | XFRM Policy + State (N^2) | 피어 목록 (N) |
| **키 로테이션** | SPI 기반 (수동/자동) | 내장 (자동) |
| **성능** | 하드웨어 가속 가능 | 소프트웨어 전용 |
| **설정 복잡성** | 높음 | 낮음 |
| **IPv6 지원** | O | O |
| **노드-노드 암호화** | O | O |
| **Pod-Pod 암호화** | O | O |
| **멀티클러스터** | O | O |

### 7.2 아키텍처 비교

```
 IPsec 아키텍처:
 +--------+     ESP 패킷      +--------+
 | 노드 A | ================> | 노드 B |
 +--------+     (IP proto 50) +--------+
    |                              |
    +-- XFRM Policy (OUT) --------+-- XFRM Policy (IN)
    +-- XFRM State  (OUT) --------+-- XFRM State  (IN)
    (노드 쌍마다 양방향 Policy+State 필요 = O(N^2))


 WireGuard 아키텍처:
 +--------+     UDP:51871     +--------+
 | 노드 A | ================> | 노드 B |
 +--------+   (cilium_wg0)   +--------+
    |                              |
    +-- Peer B: pubkey, endpoint --+-- Peer A: pubkey, endpoint
    (노드당 N-1개의 피어 = O(N))
```

### 7.3 선택 기준

| 상황 | 권장 |
|------|------|
| 하드웨어 AES-NI 가속이 있는 경우 | IPsec (AES-GCM) |
| 설정 단순화가 우선인 경우 | WireGuard |
| FIPS 140-2 인증이 필요한 경우 | IPsec |
| 커널 5.6 이상인 경우 | WireGuard |
| 노드 수가 매우 많은 대규모 클러스터 | WireGuard (상태 O(N) vs O(N^2)) |
| NAT/방화벽 뒤에서 운영하는 경우 | WireGuard (UDP 기반) |

---

## 8. BPF 데이터패스 통합

### 8.1 정책 엔트리의 auth_type 필드

**소스**: `bpf/lib/policy.h` (178-214행)

BPF 정책 맵의 각 엔트리에는 `auth_type`과 `has_explicit_auth_type` 필드가 있다.

```c
static __always_inline int
__policy_check(const struct policy_entry *policy,
               const struct policy_entry *policy2,
               __s8 *ext_err, __u16 *proxy_port, __u32 *cookie)
{
    __u8 auth_type;

    if (unlikely(policy->deny))
        return DROP_POLICY_DENY;

    *proxy_port = policy->proxy_port;

    auth_type = policy->auth_type;
    /* 같은 우선순위의 더 일반적인 정책에서 auth_type 전파 */
    if (unlikely(policy2 && policy2->precedence == policy->precedence &&
                 !policy->has_explicit_auth_type &&
                 policy2->auth_type > auth_type)) {
        auth_type = policy2->auth_type;
    }

    if (unlikely(auth_type)) {
        if (ext_err)
            *ext_err = (__s8)auth_type;
        return DROP_POLICY_AUTH_REQUIRED;
    }
    return CTX_ACT_OK;
}
```

### 8.2 auth_type 전파 규칙

정책 평가 시 `auth_type`이 결정되는 규칙:

```
 L3/L4 매치 (policy)     L4-only 매치 (policy2)
 +------------------+    +------------------+
 | precedence: 10   |    | precedence: 10   |
 | auth_type: 0     |    | auth_type: 1     |
 | explicit: false  |    | explicit: true   |
 +------------------+    +------------------+
         |                        |
         v                        v
 동일 precedence이고 policy의 auth_type이
 explicit가 아닌 경우 → policy2의 auth_type 사용
 결과: auth_type = 1 (SPIRE)
```

이 메커니즘의 핵심 규칙:
1. `policy->deny`이면 즉시 `DROP_POLICY_DENY` 반환
2. 선택된 policy의 `auth_type` 사용
3. 동일 우선순위의 policy2가 있고, policy의 auth_type이 explicit가 아니며, policy2의 auth_type이 더 높으면 policy2의 값을 사용
4. `auth_type > 0`이면 `DROP_POLICY_AUTH_REQUIRED` 반환하고 `ext_err`에 auth_type 기록

### 8.3 전체 패킷 처리 흐름

```
 패킷 수신 (BPF bpf_lxc.c)
     |
     v
 정책 맵 조회 (policy_can_access)
     |
     v
 __policy_check()
     |
     v
 auth_type > 0?
     |
    YES ──> auth_lookup(local_id, remote_id, remote_node_ip, auth_type)
              |
              v
         cilium_auth_map 조회
              |
              +--- 캐시 히트 & 미만료 → CTX_ACT_OK (패킷 통과)
              |
              +--- 캐시 미스/만료 → send_signal_auth_required()
                                     → DROP_POLICY_AUTH_REQUIRED
     |
     NO ──> CTX_ACT_OK (인증 불필요, 패킷 통과)
```

### 8.4 AuthRequirement의 BPF 매핑

**소스**: `pkg/policy/types/auth.go` (28-57행)

`AuthRequirement`는 BPF 정책 맵의 `auth_type` 필드에 직접 매핑된다:

```
 비트 레이아웃 (8비트):
 +---+---+---+---+---+---+---+---+
 | 7 | 6 | 5 | 4 | 3 | 2 | 1 | 0 |
 +---+---+---+---+---+---+---+---+
   |   \___________ ___________/
   |               |
   explicit flag   AuthType 값 (0-127)
```

```go
func (a AuthRequirement) IsExplicit() bool {
    return a&AuthTypeIsExplicit != 0      // 비트 7 확인
}

func (a AuthRequirement) AsDerived() AuthRequirement {
    return a & ^AuthTypeIsExplicit        // 비트 7 클리어
}

func (a AuthRequirement) AuthType() AuthType {
    return AuthType(a.AsDerived())        // AuthType 추출
}
```

**왜 explicit 플래그가 필요한가?**
L3/L4 정책과 L4-only 정책이 동일한 트래픽에 매치될 수 있다. 더 구체적인 정책(L3/L4)이 auth를 명시적으로 설정하지 않았다면, 덜 구체적인 정책(L4-only)의 auth_type을 물려받는다. 이 전파 규칙을 구현하기 위해 "명시적 설정 여부"를 추적해야 한다.

---

## 9. 키 관리와 로테이션

### 9.1 IPsec SPI 로테이션

IPsec에서 키 로테이션은 SPI(Security Parameter Index) 변경을 통해 이루어진다.

**소스**: `pkg/datapath/linux/ipsec/ipsec_linux.go` (188-210행)
```go
func (a *Agent) Start(cell.HookContext) error {
    if !a.config.EncryptNode {
        a.deleteIPsecEncryptRoute()
    }
    if !a.Enabled() {
        return nil
    }
    a.authKeySize, a.spi, err = a.loadIPSecKeysFile(a.config.IPsecKeyFile)
    a.setIPSecSPI(a.spi)
    a.localNode.Update(func(n *node.LocalNode) {
        n.EncryptionKey = a.spi
    })
    return nil
}
```

키 로테이션 과정:

```
 1. 관리자가 키 파일 업데이트 (새 SPI 번호와 키)
     |
     v
 2. startKeyfileWatcher() → fsnotify가 파일 변경 감지
     |
     v
 3. loadIPSecKeysFile() → 새 키 로드
     |
     v
 4. setIPSecSPI(newSPI) → 새 SPI를 현재 활성으로 설정
     |
     v
 5. localNode.Update() → EncryptionKey를 새 SPI로 업데이트
     |
     v
 6. NodeHandler.NodeConfigurationChanged() → 모든 노드에 새 XFRM state 설정
     |
     v
 7. onTimer() (1분마다) → stale key reclaimer가 이전 SPI의 XFRM state/policy 삭제
```

### 9.2 키 파일 워처

**소스**: `pkg/datapath/linux/ipsec/ipsec_linux.go` (1277-1293행)
```go
func (a *Agent) startKeyfileWatcher(nodeHandler types.NodeHandler) error {
    if !a.config.EnableIPsecKeyWatcher {
        return nil
    }
    keyfilePath := a.config.IPsecKeyFile
    watcher, _ := fswatcher.New(a.log, []string{keyfilePath})

    a.jobs.Add(job.OneShot("keyfile-watcher", func(ctx context.Context, health cell.Health) error {
        return a.keyfileWatcher(ctx, watcher, keyfilePath, nodeHandler, health)
    }))
    return nil
}
```

### 9.3 Stale Key Reclaimer

**소스**: `pkg/datapath/linux/ipsec/ipsec_linux.go` (1295-1331행, 1405-1420행)
```go
func (a *Agent) ipSecSPICanBeReclaimed(spi uint8, reclaimTimestamp time.Time) bool {
    // 현재 활성 SPI는 절대 제거하지 않음
    if spi == a.ipSecCurrentKeySPI {
        return false
    }
    // 키가 교체된 시점 확인
    keyRemovalTime, ok := a.ipSecKeysRemovalTime[spi]
    if !ok {
        a.ipSecKeysRemovalTime[spi] = time.Now()
        return false  // 처음 발견: 시간 기록만 하고 제거하지 않음
    }
    // 충분한 시간이 지났는지 확인
    if reclaimTimestamp.Sub(keyRemovalTime) < a.config.IPsecKeyRotationDuration {
        return false
    }
    return true
}

func (a *Agent) onTimer(ctx context.Context) error {
    a.ipSecLock.Lock()
    defer a.ipSecLock.Unlock()
    if a.ipSecCurrentKeySPI == 0 {
        return nil
    }
    reclaimTimestamp := time.Now()
    a.deleteStaleXfrmStates(reclaimTimestamp)
    // ... stale policy 삭제
    return nil
}
```

**왜 즉시 삭제하지 않는가?**
키 로테이션 시 이전 키를 즉시 삭제하면, 아직 이전 키로 암호화된 패킷이 전송 중일 수 있다. `IPsecKeyRotationDuration` 기간 동안 이전 키를 유지하여 전송 중인 패킷이 정상적으로 복호화될 수 있게 한다.

### 9.4 XFRM State 삭제

**소스**: `pkg/datapath/linux/ipsec/ipsec_linux.go` (1333-1351행)
```go
func (a *Agent) deleteStaleXfrmStates(reclaimTimestamp time.Time) error {
    xfrmStateList, _ := a.xfrmStateCache.XfrmStateList()
    errs := resiliency.NewErrorSet("failed to delete stale xfrm states", len(xfrmStateList))
    for _, s := range xfrmStateList {
        stateSPI := uint8(s.Spi)
        if !a.ipSecSPICanBeReclaimed(stateSPI, reclaimTimestamp) {
            continue
        }
        if err := a.xfrmStateCache.XfrmStateDel(&s); err != nil {
            errs.Add(fmt.Errorf("failed to delete stale xfrm state spi (%d): %w", stateSPI, err))
        }
    }
    return errs.Error()
}
```

### 9.5 WireGuard 키 로테이션

WireGuard의 키 로테이션은 프로토콜 레벨에서 자동으로 처리된다:

```
 WireGuard 키 관리:
 +-------------------+
 | Static Key Pair   |  ← 노드 시작 시 생성, 파일에 저장
 | (Curve25519)      |     loadOrGeneratePrivKey()
 +-------------------+
         |
         v
 +-------------------+
 | Session Key       |  ← WireGuard 프로토콜이 자동 교환
 | (ChaCha20-Poly1305)|    2분마다 새 키 생성
 +-------------------+
         |
         v
 [키 교환은 Noise_IKpsk2 프로토콜로 수행]
 [별도의 키 파일 업데이트 불필요]
```

IPsec과 달리 WireGuard는:
- 세션 키 교환이 프로토콜 내장 (2분 간격)
- 관리자의 수동 키 로테이션 불필요
- 공개키 변경 시 CiliumNode CR을 통해 자동 전파

### 9.6 SPIFFE/SPIRE 인증서 로테이션

SPIRE는 단시간 인증서(SVID)를 발급하며, 자동으로 갱신한다:

```
 SPIRE Agent (노드)
     |
     +---> Workload API로 SVID 제공
     |     (기본 TTL: 1시간)
     |
     +---> TTL의 50%가 지나면 자동 갱신
     |
     v
 CertificateProvider.SubscribeToRotatedIdentities()
     |
     v
 AuthManager.handleCertificateRotationEvent()
     |
     v
 관련 auth map 엔트리 재인증 또는 삭제
```

---

## 10. 왜 이 아키텍처인가?

### 10.1 왜 인증과 암호화를 분리하는가?

Cilium은 인증(Authentication)과 암호화(Encryption)를 완전히 별개의 레이어로 설계한다:

| 관점 | 인증 | 암호화 |
|------|------|--------|
| 질문 | "누구인가?" | "엿볼 수 없는가?" |
| 구현 | SPIFFE/SPIRE + Auth Map | IPsec 또는 WireGuard |
| 범위 | 워크로드 Identity 단위 | 노드 단위 (터널) |
| 성능 영향 | 첫 패킷만 (이후 캐시) | 모든 패킷 |

이 분리의 이점:
1. **독립적 활성화**: 인증만 필요하면 인증만 켤 수 있고, 암호화만 필요하면 암호화만 켤 수 있다
2. **제로 트러스트**: 인증은 L7 수준의 워크로드 식별, 암호화는 L3 수준의 기밀성을 각각 담당
3. **성능 최적화**: 인증은 첫 핸드셰이크 비용만 발생하고, 이후는 BPF 맵 조회로 O(1) 비용

### 10.2 왜 BPF Signal 기반 비동기 인증인가?

Cilium의 인증은 "인증 필요 → 패킷 드롭 → Agent에게 시그널 → 비동기 인증 → 맵 업데이트 → 다음 패킷 통과"라는 비동기 모델을 사용한다.

**동기 인증을 하지 않는 이유**:
- BPF 프로그램은 커널 컨텍스트에서 실행되며, TLS 핸드셰이크 같은 복잡한 작업을 수행할 수 없다
- 패킷 처리 경로에서 블로킹은 전체 네트워크 성능을 저하시킨다
- 비동기 모델은 "첫 패킷 드롭"이라는 비용만 지불하고, 이후 모든 패킷은 O(1) 비용으로 처리한다

**pending 맵의 역할**:
```go
pending map[authKey]struct{}
```
BPF 데이터패스가 매 패킷마다 시그널을 보낼 수 있으므로, 동일한 (localID, remoteID, nodeID, authType) 조합에 대해 중복 인증을 방지한다. 이 맵이 없으면 수천 개의 goroutine이 동시에 같은 인증을 시도할 수 있다.

### 10.3 왜 노드별 IPsec 키 파생인가?

단일 PSK로 클러스터 전체를 보호하는 대신, 노드 쌍마다 고유한 키를 파생한다:

```
 Global PSK + NodeA_IP + NodeB_IP + NodeA_BootID + NodeB_BootID
                    |
                    v
              SHA-256/SHA-512
                    |
                    v
            Per-Node-Pair Key
```

이 설계의 보안 이점:
1. **키 격리**: 한 노드가 침해되어도 해당 노드와의 통신만 영향받음
2. **Forward Secrecy 보완**: Boot ID가 포함되어 노드 재시작 시 새 키가 파생됨
3. **단일 장애점 제거**: 글로벌 키 유출이 전체 클러스터를 위험에 빠뜨리지 않음

### 10.4 왜 WireGuard를 추가 옵션으로 제공하는가?

IPsec은 오래된 표준이고 하드웨어 가속을 활용할 수 있지만, 다음과 같은 단점이 있다:
- XFRM state/policy가 O(N^2)로 증가하여 대규모 클러스터에서 관리 비용이 높다
- 키 로테이션이 복잡하다 (SPI 관리, stale key reclaim, timing 문제)
- 설정이 복잡하다 (키 파일 포맷, 알고리즘 선택 등)

WireGuard는 이러한 문제를 해결한다:
- 피어 목록이 O(N)으로 관리 부담 감소
- 키 교환이 프로토콜에 내장되어 관리자 개입 불필요
- 단일 암호화 스위트(ChaCha20-Poly1305)로 설정 단순화

### 10.5 왜 Auth Map의 만료 시간을 추적하는가?

Auth Map 엔트리에 만료 시간을 두는 이유:
1. **인증서 갱신 강제**: SVID(SPIFFE Verifiable Identity Document)는 TTL이 있으므로, 만료된 인증을 계속 허용하면 안 된다
2. **보안 위생**: 오래된 인증 캐시를 자동으로 무효화하여 "한번 인증하면 영원히 유효"한 상황을 방지
3. **리소스 관리**: 더 이상 사용되지 않는 엔트리가 맵을 영구히 차지하지 않도록 함

BPF에서의 만료 확인:
```c
if (utime_get_time() < auth->expiration)
    return CTX_ACT_OK;  // 아직 유효
```
이 검사는 `__always_inline` 함수 내에서 단순 정수 비교로 수행되어 성능 영향이 거의 없다.

### 10.6 왜 `tls.VersionTLS13`을 강제하는가?

**소스**: `pkg/auth/mutual_authhandler.go` (124행, 212행)

TLS 1.3만 허용하는 이유:
1. **0-RTT 핸드셰이크**: TLS 1.3은 더 적은 라운드트립으로 핸드셰이크 완료
2. **강화된 암호화**: PFS(Perfect Forward Secrecy) 필수, 약한 cipher suite 제거
3. **간소화된 프로토콜**: 레거시 호환성 코드가 없어 구현이 단순하고 공격 표면이 감소
4. **SPIFFE 호환성**: SPIFFE 표준은 TLS 1.3 이상을 권장

---

## 11. 참고 파일 목록

### 인증 (Authentication)

| 파일 경로 | 역할 |
|-----------|------|
| `pkg/auth/manager.go` | AuthManager 구조체, 인증 흐름 관리, 시그널 처리 |
| `pkg/auth/mutual_authhandler.go` | mutualAuthHandler, TLS 클라이언트/서버 핸드셰이크 |
| `pkg/auth/certs/provider.go` | CertificateProvider 인터페이스 (SPIRE 추상화) |
| `pkg/policy/types/auth.go` | AuthType 열거형, AuthRequirement 비트 레이아웃 |
| `pkg/maps/authmap/auth_map.go` | cilium_auth_map Go 래퍼, AuthKey/AuthInfo 구조체 |

### BPF 데이터패스

| 파일 경로 | 역할 |
|-----------|------|
| `bpf/lib/auth.h` | cilium_auth_map 정의, auth_lookup() 함수 |
| `bpf/lib/common.h` | struct auth_key, struct auth_info C 구조체 |
| `bpf/lib/signal.h` | send_signal_auth_required() BPF-userspace 시그널 |
| `bpf/lib/policy.h` | __policy_check() 정책 평가, auth_type 전파 로직 |
| `bpf/bpf_lxc.c` | LXC 데이터패스에서 auth_lookup() 호출 |

### IPsec 암호화

| 파일 경로 | 역할 |
|-----------|------|
| `pkg/datapath/linux/ipsec/ipsec_linux.go` | IPsec Agent, XFRM 관리, 키 로딩/파생/로테이션 |

### WireGuard 암호화

| 파일 경로 | 역할 |
|-----------|------|
| `pkg/wireguard/agent/agent.go` | WireGuard Agent, 인터페이스/피어 관리, 키 생성 |
| `pkg/wireguard/types/types.go` | 상수 정의 (IfaceName, ListenPort, PrivKeyFilename) |

---

> **핵심 요약**: Cilium의 인증/암호화 아키텍처는 "BPF 데이터패스에서 빠르게 판정하고, 복잡한 인증은 userspace에서 비동기로 처리한다"는 철학을 따른다. Auth Map은 이 두 세계를 연결하는 공유 상태이며, IPsec과 WireGuard는 전송 중 기밀성을 보장하는 독립적인 암호화 레이어다. SPIFFE/SPIRE와의 통합을 통해 제로 트러스트 네트워킹의 "워크로드 간 상호 인증"을 구현하되, 성능 영향을 최소화하기 위해 첫 핸드셰이크 이후의 모든 패킷 처리는 커널 공간에서 O(1) 비용으로 수행된다.
