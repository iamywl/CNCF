# 18. 보안과 DNSSEC (Security & DNSSEC)

## 개요

CoreDNS는 DNS 인프라의 보안을 다층적으로 지원한다. 전송 계층 암호화(TLS), DNS 메시지 인증(TSIG, DNSSEC), 접근 제어(ACL), 네트워크 바인딩(bind) 등 다양한 보안 메커니즘이 독립적인 플러그인으로 구현되어 있다.

| 보안 계층 | 플러그인/기능 | 소스 경로 | 역할 |
|----------|-------------|----------|------|
| 전송 계층 | tls | `plugin/tls/tls.go` | TLS 설정 관리 |
| DNS 인증 | dnssec | `plugin/dnssec/` | DNSSEC 온라인 서명 |
| DNS 인증 | tsigSecret | `core/dnsserver/server.go` | TSIG 트랜잭션 인증 |
| 접근 제어 | acl | `plugin/acl/acl.go` | IP/QTYPE 기반 ACL |
| 네트워크 | bind | `plugin/bind/bind.go` | 인터페이스 바인딩 |
| 클라이언트 인증 | tls (client_auth) | `plugin/tls/tls.go` | 클라이언트 인증서 검증 |

---

## 1. TLS 플러그인 (전송 계층 보안)

### 1.1 플러그인 설정 파싱

```
소스: plugin/tls/tls.go (15~77행)

func setup(c *caddy.Controller) error {
    err := parseTLS(c)
    // ...
}

func parseTLS(c *caddy.Controller) error {
    config := dnsserver.GetConfig(c)

    if config.TLSConfig != nil {
        return plugin.Error("tls", c.Errf("TLS already configured for this server instance"))
    }

    for c.Next() {
        args := c.RemainingArgs()
        if len(args) < 2 || len(args) > 3 {
            return plugin.Error("tls", c.ArgErr())
        }
        clientAuth := ctls.NoClientCert
        for c.NextBlock() {
            switch c.Val() {
            case "client_auth":
                authTypeArgs := c.RemainingArgs()
                switch authTypeArgs[0] {
                case "nocert":
                    clientAuth = ctls.NoClientCert
                case "request":
                    clientAuth = ctls.RequestClientCert
                case "require":
                    clientAuth = ctls.RequireAnyClientCert
                case "verify_if_given":
                    clientAuth = ctls.VerifyClientCertIfGiven
                case "require_and_verify":
                    clientAuth = ctls.RequireAndVerifyClientCert
                }
            }
        }
        // ...
        tls, err := tls.NewTLSConfigFromArgs(args...)
        tls.ClientAuth = clientAuth
        tls.ClientCAs = tls.RootCAs

        config.TLSConfig = tls
    }
}
```

핵심 설계 포인트:

1. **단일 인스턴스 제약**: 하나의 서버 블록에서 tls 플러그인은 한 번만 설정할 수 있다. 중복 설정 시 에러를 반환한다.
2. **인자 수에 따른 동작 변경**: 2~3개의 인자를 받는다 (cert, key, [ca]).
3. **ClientCAs = RootCAs**: 클라이언트 인증서 검증 시 서버 인증서와 동일한 CA 풀을 사용한다.
4. **상대 경로 지원**: 경로가 절대 경로가 아니면 Config.Root를 기준으로 결합한다.

### 1.2 Corefile 설정 형식

```
tls CERT KEY [CA]
```

| 인자 | 설명 |
|------|------|
| CERT | 서버 인증서 PEM 파일 경로 |
| KEY | 서버 개인키 PEM 파일 경로 |
| CA | (선택) CA 인증서 PEM 파일 경로 |

### 1.3 클라이언트 인증 옵션

```
tls /etc/ssl/cert.pem /etc/ssl/key.pem /etc/ssl/ca.pem {
    client_auth require_and_verify
}
```

| 옵션 | crypto/tls 상수 | 설명 |
|------|----------------|------|
| `nocert` | `NoClientCert` | 클라이언트 인증서 요구하지 않음 (기본값) |
| `request` | `RequestClientCert` | 인증서 요청하지만 필수 아님 |
| `require` | `RequireAnyClientCert` | 인증서 필수, 검증은 안 함 |
| `verify_if_given` | `VerifyClientCertIfGiven` | 인증서 제출 시에만 검증 |
| `require_and_verify` | `RequireAndVerifyClientCert` | 인증서 필수 + 검증 |

### 1.4 TLS 설정 생성 라이브러리

```
소스: plugin/pkg/tls/tls.go (59~92행)

func NewTLSConfigFromArgs(args ...string) (*tls.Config, error) {
    switch len(args) {
    case 0:
        // 클라이언트 인증서 없음, 시스템 CA 사용
        c, err = NewTLSClientConfig("")
    case 1:
        // 클라이언트 인증서 없음, 지정 CA 사용
        c, err = NewTLSClientConfig(certPath)
    case 2:
        // 클라이언트 인증서 있음, 시스템 CA 사용
        c, err = NewTLSConfig(certPath, keyPath, "")
    case 3:
        // 클라이언트 인증서 있음, 지정 CA 사용
        c, err = NewTLSConfig(certPath, keyPath, caPath)
    }
}
```

인자 수에 따른 네 가지 시나리오:

```
┌──────────────────────────────────────────────────────────┐
│          NewTLSConfigFromArgs 인자 시나리오                 │
├──────────────────────────────────────────────────────────┤
│                                                          │
│  0개 인자: 시스템 CA만 사용하는 클라이언트 설정               │
│            (공개 서명된 서버에 연결하는 클라이언트용)           │
│                                                          │
│  1개 인자 (CA): 지정 CA를 사용하는 클라이언트 설정            │
│            (사설 CA로 서명된 서버에 연결하는 클라이언트용)      │
│                                                          │
│  2개 인자 (cert, key): 인증서를 가진 서버/클라이언트 설정     │
│            (서버용, 또는 mTLS 클라이언트용)                   │
│            상대방 인증서는 시스템 CA로 검증                    │
│                                                          │
│  3개 인자 (cert, key, CA): 인증서 + 지정 CA 설정            │
│            (사설 PKI 환경의 서버/클라이언트용)                │
│            상대방 인증서를 지정 CA로 검증                     │
│                                                          │
└──────────────────────────────────────────────────────────┘
```

### 1.5 TLS 보안 기본값

```
소스: plugin/pkg/tls/tls.go (14~28행)

func setTLSDefaults(ctls *tls.Config) {
    ctls.MinVersion = tls.VersionTLS12
    ctls.MaxVersion = tls.VersionTLS13
    ctls.CipherSuites = []uint16{
        tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
        tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
        tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
        tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
        tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
        tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
        tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
        tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
        tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
    }
}
```

보안 기본값 분석:

| 설정 | 값 | 이유 |
|------|-----|------|
| 최소 버전 | TLS 1.2 | TLS 1.0/1.1은 보안 취약점이 알려져 있음 |
| 최대 버전 | TLS 1.3 | 최신 보안 표준 지원 |
| 키 교환 | ECDHE만 | Forward Secrecy 보장 |
| 암호화 | AES-GCM, ChaCha20-Poly1305 | AEAD 암호화만 허용 |
| 인증 | ECDSA, RSA | 두 가지 주요 서명 알고리즘 지원 |

**왜 이 설정인가?**

- CBC 모드 암호 스위트를 제외하여 BEAST, Lucky13 등의 공격을 방지한다.
- ECDHE만 허용하여, 서버 개인키가 유출되더라도 과거 통신을 복호화할 수 없도록 한다 (Forward Secrecy).
- ChaCha20-Poly1305를 포함하여, AES 하드웨어 가속이 없는 환경(ARM 등)에서도 좋은 성능을 제공한다.

### 1.6 CA 인증서 로딩

```
소스: plugin/pkg/tls/tls.go (135~150행)

func loadRoots(caPath string) (*x509.CertPool, error) {
    if caPath == "" {
        return nil, nil  // 시스템 CA 사용
    }
    roots := x509.NewCertPool()
    pem, err := os.ReadFile(filepath.Clean(caPath))
    // ...
    ok := roots.AppendCertsFromPEM(pem)
    if !ok {
        return nil, fmt.Errorf("could not read root certs: %s", err)
    }
    return roots, nil
}
```

CA 경로가 비어 있으면 `nil`을 반환하여 Go의 기본 동작(시스템 CA 풀 사용)을 따른다. 경로가 지정되면 해당 CA만 신뢰한다.

### 1.7 HTTPS 전송 설정

```
소스: plugin/pkg/tls/tls.go (153~166행)

func NewHTTPSTransport(cc *tls.Config) *http.Transport {
    tr := &http.Transport{
        Proxy: http.ProxyFromEnvironment,
        Dial: (&net.Dialer{
            Timeout:   30 * time.Second,
            KeepAlive: 30 * time.Second,
        }).Dial,
        TLSHandshakeTimeout: 10 * time.Second,
        TLSClientConfig:     cc,
        MaxIdleConnsPerHost: 25,
    }
    return tr
}
```

forward 플러그인 등에서 업스트림 HTTPS 연결에 사용하는 전송 설정이다.

---

## 2. DNSSEC 플러그인 (DNS 메시지 서명)

### 2.1 개요

CoreDNS의 dnssec 플러그인은 **온라인 서명(on-the-fly signing)**을 수행한다. 사전에 모든 레코드를 서명하는 것이 아니라, 요청이 들어올 때 실시간으로 응답에 서명을 추가한다.

부정 응답(NXDOMAIN)에는 **NSEC Black Lies** 기법을 사용한다.

### 2.2 Dnssec 구조체

```
소스: plugin/dnssec/dnssec.go (17~26행)

type Dnssec struct {
    Next      plugin.Handler
    zones     []string
    keys      []*DNSKEY
    splitkeys bool
    inflight  *singleflight.Group
    cache     *cache.Cache[[]dns.RR]
}
```

| 필드 | 설명 |
|------|------|
| zones | DNSSEC 서명 대상 zone 목록 |
| keys | DNSSEC 키 배열 (KSK/ZSK) |
| splitkeys | KSK/ZSK 분리 서명 모드 |
| inflight | 중복 서명 요청 병합 (singleflight) |
| cache | 서명 캐시 (키: RR 해시, 값: RRSIG 배열) |

### 2.3 DNSKEY 구조체

```
소스: plugin/dnssec/dnskey.go (23~29행)

type DNSKEY struct {
    K   *dns.DNSKEY
    D   *dns.DS
    s   crypto.Signer
    tag uint16
}
```

| 필드 | 설명 |
|------|------|
| K | DNS DNSKEY 레코드 (공개키) |
| D | DS 레코드 (SHA-256 다이제스트) |
| s | 개인키 (crypto.Signer 인터페이스) |
| tag | DNSKEY 태그 (Key ID) |

### 2.4 키 파일 파싱

```
소스: plugin/dnssec/dnskey.go (39~75행)

func ParseKeyFile(pubFile, privFile string) (*DNSKEY, error) {
    f, e := os.Open(filepath.Clean(pubFile))
    // 공개키 파일 읽기
    k, e := dns.ReadRR(f, pubFile)
    // ...
    dk, ok := k.(*dns.DNSKEY)
    // 개인키 파일 읽기
    p, e := dk.ReadPrivateKey(f, privFile)
    // ...
    // 키 타입별 Signer 생성
    if s, ok := p.(*rsa.PrivateKey); ok {
        return &DNSKEY{K: dk, D: dk.ToDS(dns.SHA256), s: s, tag: dk.KeyTag()}, nil
    }
    if s, ok := p.(*ecdsa.PrivateKey); ok {
        return &DNSKEY{K: dk, D: dk.ToDS(dns.SHA256), s: s, tag: dk.KeyTag()}, nil
    }
    if s, ok := p.(ed25519.PrivateKey); ok {
        return &DNSKEY{K: dk, D: dk.ToDS(dns.SHA256), s: s, tag: dk.KeyTag()}, nil
    }
}
```

지원하는 서명 알고리즘:

| 알고리즘 | Go 타입 | DNSSEC 알고리즘 번호 |
|---------|---------|-------------------|
| RSA | `*rsa.PrivateKey` | 8 (RSASHA256), 10 (RSASHA512) |
| ECDSA | `*ecdsa.PrivateKey` | 13 (ECDSAP256SHA256), 14 (ECDSAP384SHA384) |
| Ed25519 | `ed25519.PrivateKey` | 15 (ED25519) |

DS 레코드는 항상 SHA-256 다이제스트(`dns.SHA256`)로 생성된다.

### 2.5 AWS Secrets Manager 키 지원

```
소스: plugin/dnssec/dnskey.go (78~138행)

func ParseKeyFromAWSSecretsManager(secretID string) (*DNSKEY, error) {
    cfg, err := config.LoadDefaultConfig(context.TODO())
    client := secretsmanager.NewFromConfig(cfg)
    // ...
    var secretData SecretKeyData
    err = json.Unmarshal([]byte(*result.SecretString), &secretData)
    // ...
}

type SecretKeyData struct {
    Key     string `json:"key"`
    Private string `json:"private"`
}
```

AWS Secrets Manager에 저장된 DNSSEC 키를 직접 로드할 수 있다. 키 파일을 디스크에 저장하지 않아도 되므로 보안이 강화된다.

### 2.6 KSK와 ZSK 구분

```
소스: plugin/dnssec/dnskey.go (162~169행)

func (k DNSKEY) isZSK() bool {
    return k.K.Flags&(1<<8) == (1<<8) && k.K.Flags&1 == 0
}

func (k DNSKEY) isKSK() bool {
    return k.K.Flags&(1<<8) == (1<<8) && k.K.Flags&1 == 1
}
```

DNSKEY Flags 필드 비트 분석:

```
┌──────────────────────────────────────────────────┐
│              DNSKEY Flags (16비트)                 │
├──────────────────────────────────────────────────┤
│                                                  │
│  비트 7 (값 256): Zone Key Flag                   │
│  비트 15 (값 1):  SEP (Secure Entry Point) Flag   │
│                                                  │
│  ZSK: Zone Key = 1, SEP = 0 → Flags = 256       │
│  KSK: Zone Key = 1, SEP = 1 → Flags = 257       │
│                                                  │
│  Zone Key가 0이면 DNSSEC 키가 아님                 │
│                                                  │
└──────────────────────────────────────────────────┘
```

### 2.7 설정 파싱

```
소스: plugin/dnssec/setup.go (48~107행)

func dnssecParse(c *caddy.Controller) ([]string, []*DNSKEY, int, bool, error) {
    // zones 파싱
    zones = plugin.OriginsFromArgsOrServerBlock(c.RemainingArgs(), c.ServerBlockKeys)

    for c.NextBlock() {
        switch x := c.Val(); x {
        case "key":
            k, e := keyParse(c)
            keys = append(keys, k...)
        case "cache_capacity":
            cacheCap, err := strconv.Atoi(value)
            capacity = cacheCap
        }
    }

    // KSK/ZSK 자동 감지
    zsk, ksk := 0, 0
    for _, k := range keys {
        if k.isKSK() { ksk++ }
        else if k.isZSK() { zsk++ }
    }
    splitkeys := zsk > 0 && ksk > 0

    // 키 소유자 이름 검증
    for _, k := range keys {
        kname := plugin.Name(k.K.Header().Name)
        ok := slices.ContainsFunc(zones, kname.Matches)
        if !ok {
            return ..., fmt.Errorf("key %s (keyid: %d) can not sign any of the zones", ...)
        }
    }
}
```

자동 키 모드 감지:
- KSK와 ZSK가 **모두** 존재하면 `splitkeys = true` → KSK는 DNSKEY RRSet만, ZSK는 나머지를 서명
- 하나만 있거나 구분이 없으면 `splitkeys = false` → 모든 키가 모든 RRSet을 서명

### 2.8 키 소스 지정

```
소스: plugin/dnssec/setup.go (109~157행)

func keyParse(c *caddy.Controller) ([]*DNSKEY, error) {
    value := c.Val()
    switch value {
    case "file":
        // 로컬 파일에서 로드
        // Kmiek.nl.+013+26205.key / .private
    case "aws_secretsmanager":
        // AWS Secrets Manager에서 로드
    }
}
```

Corefile 설정:

```
dnssec example.com {
    key file Kexample.com.+013+12345
    cache_capacity 20000
}

# 또는 AWS Secrets Manager
dnssec example.com {
    key aws_secretsmanager my-dnssec-key-secret
}
```

### 2.9 ServeDNS 핸들러

```
소스: plugin/dnssec/handler.go (14~47행)

func (d Dnssec) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
    state := request.Request{W: w, Req: r}

    do := state.Do()
    qname := state.Name()
    qtype := state.QType()
    zone := plugin.Zones(d.zones).Matches(qname)
    if zone == "" {
        return plugin.NextOrFailure(d.Name(), d.Next, ctx, w, r)
    }

    state.Zone = zone
    server := metrics.WithServer(ctx)

    // DNSKEY 쿼리 인터셉트
    if qtype == dns.TypeDNSKEY {
        for _, z := range d.zones {
            if qname == z {
                resp := d.getDNSKEY(state, z, do, server)
                resp.Authoritative = true
                w.WriteMsg(resp)
                return dns.RcodeSuccess, nil
            }
        }
    }

    // DO 비트 설정 시 ResponseWriter 래핑
    if do {
        drr := &ResponseWriter{w, d, server}
        return plugin.NextOrFailure(d.Name(), d.Next, ctx, drr, r)
    }

    return plugin.NextOrFailure(d.Name(), d.Next, ctx, w, r)
}
```

DNSSEC 처리 흐름:

```
┌──────────────────────────────────────────────────────────┐
│              DNSSEC ServeDNS 처리 흐름                     │
├──────────────────────────────────────────────────────────┤
│                                                          │
│  1. zone 매칭 확인                                        │
│     └─ 매칭 안 되면 → 다음 플러그인으로 패스스루             │
│                                                          │
│  2. DNSKEY 쿼리 인터셉트                                   │
│     └─ zone 이름과 일치하면 → 공개키 + 서명 직접 응답        │
│                                                          │
│  3. DO 비트 확인 (DNSSEC OK)                              │
│     ├─ DO=1 → ResponseWriter 래핑 후 다음 플러그인 호출     │
│     │         → 응답 시 자동 서명                          │
│     └─ DO=0 → 서명 없이 다음 플러그인 호출                  │
│                                                          │
└──────────────────────────────────────────────────────────┘
```

### 2.10 ResponseWriter (자동 서명)

```
소스: plugin/dnssec/responsewriter.go (13~36행)

type ResponseWriter struct {
    dns.ResponseWriter
    d      Dnssec
    server string
}

func (d *ResponseWriter) WriteMsg(res *dns.Msg) error {
    state := request.Request{W: d.ResponseWriter, Req: res}

    zone := plugin.Zones(d.d.zones).Matches(state.Name())
    if zone == "" {
        return d.ResponseWriter.WriteMsg(res)
    }
    state.Zone = zone

    res = d.d.Sign(state, time.Now().UTC(), d.server)
    cacheSize.WithLabelValues(d.server, "signature").Set(float64(d.d.cache.Len()))

    return d.ResponseWriter.WriteMsg(res)
}
```

다음 플러그인이 응답을 쓸 때, ResponseWriter가 가로채서 서명을 추가한다. zone이 매칭되지 않으면 서명 없이 그대로 전달한다.

### 2.11 Sign 메서드 (핵심 서명 로직)

```
소스: plugin/dnssec/dnssec.go (44~112행)

func (d Dnssec) Sign(state request.Request, now time.Time, server string) *dns.Msg {
    req := state.Req
    incep, expir := incepExpir(now)

    mt, _ := response.Typify(req, time.Now().UTC())
    if mt == response.Delegation {
        // DS 레코드 서명 또는 NSEC 생성
    }

    if mt == response.NameError || mt == response.NoData {
        // SOA 서명 + NSEC Black Lies 생성
        if len(req.Ns) > 1 { req.Rcode = dns.RcodeSuccess }
    }

    // 일반 응답: Answer, Ns, Extra 각 RRSet 서명
    for _, r := range rrSets(req.Answer) {
        sigs, err := d.sign(r, state.Zone, ttl, incep, expir, server)
        req.Answer = append(req.Answer, sigs...)
    }
    // Ns, Extra 동일하게 처리
}
```

응답 타입별 서명 전략:

| 응답 타입 | 처리 |
|----------|------|
| Delegation | DS 레코드 서명 또는 NSEC(DS 없음 증명) |
| NameError (NXDOMAIN) | SOA 서명 + NSEC Black Lies + Rcode를 NOERROR로 변경 |
| NoData | SOA 서명 + NSEC Black Lies |
| NoError (일반) | Answer, Ns, Extra 각 RRSet별 RRSIG 생성 |

### 2.12 서명 유효 기간

```
소스: plugin/dnssec/dnssec.go (169~178행)

func incepExpir(now time.Time) (uint32, uint32) {
    incep := uint32(now.Add(-3 * time.Hour).Unix())  // 3시간 전
    expir := uint32(now.Add(eightDays).Unix())       // 8일 후
    return incep, expir
}

const (
    eightDays  = 8 * 24 * time.Hour
    twoDays    = 2 * 24 * time.Hour
    defaultCap = 10000
)
```

- **시작 시각**: 현재 시각 - 3시간 (일광 절약 시간 등 시계 오차 보정)
- **만료 시각**: 현재 시각 + 8일
- **캐시 갱신**: 서명 유효 기간의 75% (6일) 경과 시 캐시 미스로 처리하여 갱신

### 2.13 서명 캐싱

```
소스: plugin/dnssec/dnssec.go (114~167행)

func (d Dnssec) sign(rrs []dns.RR, signerName string, ttl, incep, expir uint32, server string) ([]dns.RR, error) {
    k := hash(rrs)
    sgs, ok := d.get(k, server)
    if ok {
        return sgs, nil
    }

    sigs, err := d.inflight.Do(k, func() (any, error) {
        var sigs []dns.RR
        for _, k := range d.keys {
            if d.splitkeys {
                // KSK는 DNSKEY만, ZSK는 나머지만 서명
            }
            sig := k.newRRSIG(signerName, ttl, incep, expir)
            sig.Sign(k.s, rrs)
            sigs = append(sigs, sig)
        }
        d.set(k, sigs)
        return sigs, nil
    })
    return sigs.([]dns.RR), err
}
```

캐싱 전략:

```
┌──────────────────────────────────────────────────────────┐
│              DNSSEC 서명 캐싱 전략                         │
├──────────────────────────────────────────────────────────┤
│                                                          │
│  1. RR 세트를 해시하여 캐시 키 생성                         │
│  2. 캐시에서 기존 서명 조회                                 │
│     ├─ 캐시 히트: 서명 유효 기간 75% 이내 → 캐시 반환       │
│     └─ 캐시 미스 또는 75% 경과 → 재서명                    │
│  3. singleflight: 동일 RR 세트에 대한 중복 서명 방지        │
│     └─ 여러 요청이 동시에 같은 RR을 서명하려 하면            │
│        하나만 실행하고 나머지는 결과 공유                     │
│  4. 새 서명을 캐시에 저장                                   │
│                                                          │
│  기본 캐시 용량: 10,000개 서명                              │
│  cache_capacity로 조정 가능                                │
│                                                          │
└──────────────────────────────────────────────────────────┘
```

### 2.14 NSEC Black Lies

```
소스: plugin/dnssec/black_lies.go (13~47행)

func (d Dnssec) nsec(state request.Request, mt response.Type, ttl, incep, expir uint32, server string) ([]dns.RR, error) {
    nsec := &dns.NSEC{}
    nsec.Hdr = dns.RR_Header{Name: state.QName(), Ttl: ttl, Class: dns.ClassINET, Rrtype: dns.TypeNSEC}
    nsec.NextDomain = "\\000." + state.QName()
    // ...
}
```

NSEC Black Lies는 RFC 드래프트(draft-valsorda-dnsop-black-lies)에 기반한 기법이다.

**왜 Black Lies인가?**

전통적인 DNSSEC에서 NXDOMAIN을 증명하려면 zone의 모든 이름을 정렬하여 NSEC 체인을 만들어야 한다. 이는:
1. zone 열거(zone walking) 공격에 취약하다
2. 모든 이름의 사전 서명이 필요하다

Black Lies는 다른 접근법을 취한다:
- `a.example.com`에 대한 NXDOMAIN 요청 시, `a.example.com → \000.a.example.com` 범위의 NSEC를 생성한다
- 이 범위는 `a.example.com`과 `\000.a.example.com` 사이에 다른 이름이 없음을 증명한다
- NXDOMAIN을 NODATA로 변환하여 Rcode를 NOERROR로 설정한다

```
a.example.com. 3600 IN NSEC \000.a.example.com. RRSIG NSEC ...
```

### 2.15 NSEC 비트맵

```
소스: plugin/dnssec/black_lies.go (50~53행)

var (
    delegationBitmap = [...]uint16{dns.TypeA, dns.TypeNS, dns.TypeHINFO, ...}
    zoneBitmap       = [...]uint16{dns.TypeA, dns.TypeHINFO, dns.TypeTXT, ...}
    apexBitmap       = [...]uint16{dns.TypeA, dns.TypeNS, dns.TypeSOA, dns.TypeMX, ...}
)
```

세 가지 비트맵:

| 비트맵 | 용도 | 특수 타입 |
|--------|------|----------|
| apexBitmap | zone 정점 (SOA 레벨) | NS, SOA, MX, DNSKEY 포함 |
| zoneBitmap | zone 내 일반 이름 | NS 없음 |
| delegationBitmap | 위임 지점 | NS 포함 |

NoData 응답 시 쿼리된 타입을 비트맵에서 제거하여 "이 타입은 없다"고 증명한다.

### 2.16 getDNSKEY (공개키 응답)

```
소스: plugin/dnssec/dnskey.go (141~159행)

func (d Dnssec) getDNSKEY(state request.Request, zone string, do bool, server string) *dns.Msg {
    keys := make([]dns.RR, len(d.keys))
    for i, k := range d.keys {
        keys[i] = dns.Copy(k.K)
        keys[i].Header().Name = zone
    }
    m := new(dns.Msg)
    m.SetReply(state.Req)
    m.Answer = keys
    if !do {
        return m
    }
    // DO 비트 설정 시 키도 서명
    incep, expir := incepExpir(time.Now().UTC())
    sigs, err := d.sign(keys, zone, 3600, incep, expir, server)
    m.Answer = append(m.Answer, sigs...)
    return m
}
```

DNSKEY 쿼리에 대해 모든 공개키를 반환하고, DO 비트가 설정되어 있으면 RRSIG도 포함한다.

---

## 3. ACL 플러그인 (접근 제어)

### 3.1 ACL 구조체

```
소스: plugin/acl/acl.go (18~22행)

type ACL struct {
    Next plugin.Handler
    Rules []rule
}
```

### 3.2 rule과 policy

```
소스: plugin/acl/acl.go (26~41행)

type rule struct {
    zones    []string
    policies []policy
}

type policy struct {
    action action
    qtypes map[uint16]struct{}
    filter *iptree.Tree
}
```

| 타입 | 설명 |
|------|------|
| rule | zone별 정책 그룹 |
| policy | 개별 접근 제어 정책 (액션 + 쿼리타입 필터 + IP 필터) |

### 3.3 액션 상수

```
소스: plugin/acl/acl.go (43~54행)

const (
    actionNone   = iota  // 아무 것도 하지 않음
    actionAllow          // 허용 (다음 플러그인으로 진행)
    actionBlock          // 차단 (REFUSED 반환)
    actionFilter         // 필터 (빈 NOERROR 반환)
    actionDrop           // 드롭 (응답 없이 무시)
)
```

| 액션 | 응답 | EDNS0 EDE | 설명 |
|------|------|-----------|------|
| allow | 다음 플러그인 | - | 정상 처리 |
| block | REFUSED | ExtendedErrorCodeBlocked | 거부 응답 |
| filter | NOERROR (빈 응답) | ExtendedErrorCodeFiltered | 빈 응답 |
| drop | 응답 없음 | - | 무시 (클라이언트 타임아웃) |

### 3.4 ServeDNS 처리

```
소스: plugin/acl/acl.go (59~108행)

func (a ACL) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
    state := request.Request{W: w, Req: r}

RulesCheckLoop:
    for _, rule := range a.Rules {
        zone := plugin.Zones(rule.zones).Matches(state.Name())
        if zone == "" {
            continue
        }

        action := matchWithPolicies(rule.policies, w, r)
        switch action {
        case actionDrop:
            RequestDropCount.WithLabelValues(...).Inc()
            return dns.RcodeSuccess, nil
        case actionBlock:
            m := new(dns.Msg).SetRcode(r, dns.RcodeRefused).SetEdns0(4096, true)
            ede := dns.EDNS0_EDE{InfoCode: dns.ExtendedErrorCodeBlocked}
            m.IsEdns0().Option = append(m.IsEdns0().Option, &ede)
            w.WriteMsg(m)
            RequestBlockCount.WithLabelValues(...).Inc()
            return dns.RcodeSuccess, nil
        case actionAllow:
            break RulesCheckLoop
        case actionFilter:
            m := new(dns.Msg).SetRcode(r, dns.RcodeSuccess).SetEdns0(4096, true)
            ede := dns.EDNS0_EDE{InfoCode: dns.ExtendedErrorCodeFiltered}
            m.IsEdns0().Option = append(m.IsEdns0().Option, &ede)
            w.WriteMsg(m)
            RequestFilterCount.WithLabelValues(...).Inc()
            return dns.RcodeSuccess, nil
        }
    }

    RequestAllowCount.WithLabelValues(...).Inc()
    return plugin.NextOrFailure(state.Name(), a.Next, ctx, w, r)
}
```

ACL 처리 흐름:

```
┌──────────────────────────────────────────────────────────┐
│                   ACL 처리 흐름                            │
├──────────────────────────────────────────────────────────┤
│                                                          │
│  Rules 순회 (순서 중요!)                                   │
│       │                                                  │
│       ├─ zone 매칭 확인                                   │
│       │   └─ 매칭 안 되면 → 다음 rule                     │
│       │                                                  │
│       ├─ policies 매칭 (IP + QTYPE)                      │
│       │   ├─ drop   → 무응답 (RequestDropCount++)        │
│       │   ├─ block  → REFUSED + EDE (RequestBlockCount++)│
│       │   ├─ allow  → 루프 탈출, 정상 처리                 │
│       │   ├─ filter → 빈 NOERROR + EDE (RequestFilterCount++)│
│       │   └─ none   → 다음 rule                          │
│       │                                                  │
│       └─ 모든 rule 통과 → 기본 허용                        │
│                                                          │
└──────────────────────────────────────────────────────────┘
```

### 3.5 정책 매칭

```
소스: plugin/acl/acl.go (112~146행)

func matchWithPolicies(policies []policy, w dns.ResponseWriter, r *dns.Msg) action {
    state := request.Request{W: w, Req: r}

    var ip net.IP
    if idx := strings.IndexByte(state.IP(), '%'); idx >= 0 {
        ip = net.ParseIP(state.IP()[:idx])
    } else {
        ip = net.ParseIP(state.IP())
    }

    if ip == nil {
        log.Errorf("Blocking request. Unable to parse source address: %v", state.IP())
        return actionBlock
    }

    qtype := state.QType()
    for _, policy := range policies {
        _, matchAll := policy.qtypes[dns.TypeNone]
        _, match := policy.qtypes[qtype]
        if !matchAll && !match {
            continue
        }
        _, contained := policy.filter.GetByIP(ip)
        if !contained {
            continue
        }
        return policy.action
    }
    return actionNone
}
```

매칭 로직:
1. 클라이언트 IP 파싱 (IPv6 존 ID 제거 포함)
2. IP 파싱 실패 시 → 안전하게 **차단** (기본 거부)
3. 쿼리 타입 매칭: `dns.TypeNone`은 와일드카드로 모든 타입 매칭
4. IP 매칭: `iptree.Tree`를 사용한 CIDR 기반 매칭

### 3.6 ACL 설정 예시

```
소스: plugin/acl/setup.go (42~138행)
```

Corefile 설정:

```
acl example.com {
    # 내부 네트워크에서만 허용
    allow net 10.0.0.0/8 192.168.0.0/16

    # 외부에서 AXFR 차단
    block type AXFR IXFR net 0.0.0.0/0

    # 특정 IP에서 TXT 쿼리 필터링
    filter type TXT net 203.0.113.0/24

    # 나머지 모든 쿼리 드롭
    drop net *
}
```

### 3.7 기본 필터 (와일드카드)

```
소스: plugin/acl/setup.go (19~26행)

func newDefaultFilter() *iptree.Tree {
    defaultFilter := iptree.NewTree()
    _, IPv4All, _ := net.ParseCIDR("0.0.0.0/0")
    _, IPv6All, _ := net.ParseCIDR("::/0")
    defaultFilter.InplaceInsertNet(IPv4All, struct{}{})
    defaultFilter.InplaceInsertNet(IPv6All, struct{}{})
    return defaultFilter
}
```

`net *` 또는 `net` 섹션 미지정 시, IPv4와 IPv6 전체 주소 공간을 매칭한다.

### 3.8 CIDR 정규화

```
소스: plugin/acl/setup.go (146~155행)

func normalize(rawNet string) string {
    if idx := strings.IndexAny(rawNet, "/"); idx >= 0 {
        return rawNet  // 이미 CIDR 표기
    }
    if idx := strings.IndexAny(rawNet, ":"); idx >= 0 {
        return rawNet + "/128"  // IPv6 단일 주소
    }
    return rawNet + "/32"  // IPv4 단일 주소
}
```

단일 IP 주소를 자동으로 /32(IPv4) 또는 /128(IPv6)으로 변환한다.

### 3.9 EDNS0 Extended DNS Error (EDE)

block과 filter 액션에서 EDNS0 EDE 옵션을 포함한다:

| 액션 | EDE InfoCode | 설명 |
|------|-------------|------|
| block | `ExtendedErrorCodeBlocked` | 정책에 의해 차단됨 |
| filter | `ExtendedErrorCodeFiltered` | 정책에 의해 필터링됨 |

이는 RFC 8914 (Extended DNS Errors)에 따라, 클라이언트에게 거부 이유를 구체적으로 알려준다.

---

## 4. TSIG 지원 (트랜잭션 서명)

### 4.1 tsigSecret 맵

```
소스: core/dnsserver/server.go (54행)

tsigSecret map[string]string
```

Server 구조체에 TSIG 비밀키 맵이 포함되어 있다. 키 이름 → HMAC 비밀키 매핑이다.

### 4.2 TSIG 키 복사

```
소스: core/dnsserver/server.go (100~101행)

// copy tsig secrets
maps.Copy(s.tsigSecret, site.TsigSecret)
```

Config에 설정된 TSIG 키를 Server로 복사한다. 여러 zone의 TSIG 키가 하나의 맵에 통합된다.

### 4.3 DNS 서버에 TSIG 전달

```
소스: core/dnsserver/server.go (152~153행)

s.server[tcp] = &dns.Server{
    // ...
    TsigSecret: s.tsigSecret,
    // ...
}
```

`miekg/dns` 라이브러리의 `dns.Server`에 TSIG 비밀키 맵을 전달하여, DNS 프로토콜 수준에서 TSIG 검증이 수행된다.

### 4.4 TSIG 사용 시나리오

TSIG는 주로 다음 용도로 사용된다:
- **Zone 전송(AXFR/IXFR) 인증**: Secondary 서버가 Primary에서 zone 데이터를 가져올 때
- **동적 업데이트(RFC 2136) 인증**: 권한 있는 클라이언트만 DNS 레코드를 갱신할 수 있도록

---

## 5. bind 플러그인 (인터페이스 바인딩)

### 5.1 구조체

```
소스: plugin/bind/bind.go (10~14행)

type bind struct {
    Next   plugin.Handler
    addrs  []string
    except []string
}
```

### 5.2 설정 파싱

```
소스: plugin/bind/setup.go (15~49행)

func setup(c *caddy.Controller) error {
    config := dnsserver.GetConfig(c)
    all := []string{}
    ifaces, err := net.Interfaces()
    // ...
    for c.Next() {
        b, err := parse(c)
        ips, err := listIP(b.addrs, ifaces)
        except, err := listIP(b.except, ifaces)

        for _, ip := range ips {
            if !slices.Contains(except, ip) {
                all = append(all, ip)
            }
        }
    }
    config.ListenHosts = all
}
```

### 5.3 인터페이스 이름 지원

```
소스: plugin/bind/setup.go (72~108행)

func listIP(args []string, ifaces []net.Interface) ([]string, error) {
    for _, a := range args {
        for _, iface := range ifaces {
            if a == iface.Name {
                addrs, err := iface.Addrs()
                for _, addr := range addrs {
                    if ipnet, ok := addr.(*net.IPNet); ok {
                        // IPv6 링크 로컬 주소에 zone ID 추가
                        if ipnet.IP.To4() == nil &&
                            (ipnet.IP.IsLinkLocalMulticast() || ipnet.IP.IsLinkLocalUnicast()) {
                            if ipa.Zone == "" {
                                ipa.Zone = iface.Name
                            }
                        }
                        all = append(all, ipa.String())
                    }
                }
            }
        }
        // 인터페이스 이름이 아니면 IP 주소로 파싱
        if net.ParseIP(a) == nil {
            return nil, fmt.Errorf("not a valid IP address or interface name: %q", a)
        }
        all = append(all, a)
    }
}
```

IP 주소와 인터페이스 이름 모두 지원한다. 인터페이스 이름이면 해당 인터페이스의 모든 IP 주소로 확장한다. IPv6 링크 로컬 주소에는 zone ID(인터페이스 이름)를 자동 추가한다.

### 5.4 except 지시자

```
소스: plugin/bind/setup.go (51~69행)

func parse(c *caddy.Controller) (*bind, error) {
    b := &bind{}
    b.addrs = c.RemainingArgs()
    for c.NextBlock() {
        switch c.Val() {
        case "except":
            b.except = c.RemainingArgs()
        }
    }
    return b, nil
}
```

특정 주소나 인터페이스를 바인딩에서 제외할 수 있다.

### 5.5 Corefile 설정

```
# 특정 IP에만 바인딩
.:53 {
    bind 10.0.0.1
}

# 인터페이스 이름으로 바인딩
.:53 {
    bind eth0
}

# 인터페이스 바인딩에서 특정 주소 제외
.:53 {
    bind eth0 {
        except 10.0.0.2
    }
}
```

### 5.6 보안 의의

bind 플러그인은 공격 표면을 줄이는 핵심 보안 도구이다:
- **내부 전용 DNS**: `bind 10.0.0.1`로 내부 네트워크에서만 접근 가능
- **관리 인터페이스 분리**: 관리용과 서비스용 인터페이스를 분리
- **IPv4/IPv6 분리**: 특정 프로토콜 스택에만 바인딩

---

## 6. 서버 레벨 보안 기능

### 6.1 CH 클래스 차단

```
소스: core/dnsserver/server.go (275~278행)

if !s.classChaos && r.Question[0].Qclass != dns.ClassINET {
    errorAndMetricsFunc(s.Addr, w, r, dns.RcodeRefused)
    return
}
```

기본적으로 CH(Chaos) 클래스 쿼리를 차단한다. `version.bind` 등으로 서버 정보를 노출하는 것을 방지한다.

```
소스: core/dnsserver/server.go (450~454행)

var EnableChaos = map[string]struct{}{
    "chaos":   {},
    "forward": {},
    "proxy":   {},
}
```

`chaos`, `forward`, `proxy` 플러그인이 로드되면 CH 클래스가 허용된다.

### 6.2 EDNS 버전 검사

```
소스: core/dnsserver/server.go (280~283행)

if m, err := edns.Version(r); err != nil {
    w.WriteMsg(m)
    return
}
```

지원하지 않는 EDNS 버전의 요청은 즉시 거부한다.

### 6.3 ScrubWriter (응답 크기 조정)

```
소스: core/dnsserver/server.go (286행)

w = request.NewScrubWriter(r, w)
```

클라이언트의 EDNS 버퍼 크기에 맞게 응답을 자동 조정한다. 과도하게 큰 응답이 전송되는 것을 방지한다.

### 6.4 Loop 감지

```
소스: core/dnsserver/server.go (162행)

ctx = context.WithValue(ctx, LoopKey{}, 0)
```

`LoopKey`로 서버 내 무한 루프를 감지한다. forward 등의 플러그인이 자기 자신에게 쿼리를 전달하는 경우를 탐지한다.

---

## 7. PROXY 프로토콜 지원

### 7.1 리스너 래핑

```
소스: core/dnsserver/server.go (190~194행)

if s.connPolicy != nil {
    l = &proxyproto.Listener{Listener: l, ConnPolicy: s.connPolicy}
}
```

PROXY 프로토콜을 사용하면 로드 밸런서 뒤에서도 실제 클라이언트 IP를 알 수 있다. `connPolicy`로 어떤 연결에서 PROXY 프로토콜 헤더를 기대할지 제어한다.

---

## 8. 보안 아키텍처 전체 구조

```
┌──────────────────────────────────────────────────────────────┐
│                  CoreDNS 보안 계층 구조                        │
├──────────────────────────────────────────────────────────────┤
│                                                              │
│  네트워크 계층                                                │
│  ├─ bind: 인터페이스/IP 바인딩 (공격 표면 축소)                 │
│  └─ PROXY Protocol: 실제 클라이언트 IP 파악                    │
│                                                              │
│  전송 계층                                                    │
│  ├─ TLS 1.2/1.3: 통신 암호화                                  │
│  ├─ mTLS: 클라이언트 인증서 검증                               │
│  └─ QUIC: 0-RTT + 내장 암호화                                 │
│                                                              │
│  DNS 프로토콜 계층                                             │
│  ├─ EDNS 버전 검사                                            │
│  ├─ CH 클래스 차단                                            │
│  ├─ ScrubWriter: 응답 크기 제한                                │
│  └─ TSIG: 트랜잭션 인증 (zone 전송, 동적 업데이트)              │
│                                                              │
│  애플리케이션 계층                                              │
│  ├─ ACL: IP/QTYPE 기반 접근 제어                               │
│  │   ├─ allow: 허용                                           │
│  │   ├─ block: REFUSED + EDE                                  │
│  │   ├─ filter: 빈 NOERROR + EDE                              │
│  │   └─ drop: 무응답                                          │
│  └─ DNSSEC: 응답 무결성 검증                                   │
│      ├─ 온라인 서명 (on-the-fly)                               │
│      ├─ NSEC Black Lies (zone 열거 방지)                       │
│      ├─ KSK/ZSK 분리                                         │
│      └─ 서명 캐싱 + singleflight                               │
│                                                              │
│  운영 계층                                                    │
│  ├─ panic recovery: 서버 안정성 보장                            │
│  ├─ Loop 감지: 무한 루프 방지                                   │
│  └─ 정규식 길이 제한: DoS 방지 (errors 플러그인)                 │
│                                                              │
└──────────────────────────────────────────────────────────────┘
```

---

## 9. 보안 설정 가이드

### 9.1 최소 보안 설정 (내부 DNS)

```
.:53 {
    bind 10.0.0.1
    acl {
        allow net 10.0.0.0/8
        block net *
    }
    forward . 8.8.8.8
}
```

### 9.2 DoT 서버 (외부 공개)

```
tls://.:853 {
    tls /etc/ssl/cert.pem /etc/ssl/key.pem
    acl {
        block type AXFR IXFR net *
    }
    forward . 8.8.8.8
}
```

### 9.3 DNSSEC 서명 서버

```
.:53 {
    dnssec example.com {
        key file Kexample.com.+013+12345
        cache_capacity 20000
    }
    file /etc/coredns/zones/example.com.db example.com
}
```

### 9.4 mTLS 서버 (클라이언트 인증)

```
tls://.:853 {
    tls /etc/ssl/cert.pem /etc/ssl/key.pem /etc/ssl/ca.pem {
        client_auth require_and_verify
    }
    forward . 8.8.8.8
}
```

### 9.5 다중 보안 계층 조합

```
tls://.:853 {
    bind eth1
    tls /etc/ssl/cert.pem /etc/ssl/key.pem /etc/ssl/ca.pem {
        client_auth require_and_verify
    }
    acl {
        allow net 10.0.0.0/8
        block type AXFR IXFR net *
        allow net *
    }
    dnssec example.com {
        key file Kexample.com.+013+12345
    }
    errors {
        stacktrace
        consolidate 30s ".*" warning
    }
    file /etc/coredns/zones/example.com.db example.com
    forward . 8.8.8.8
}
```

---

## 10. 보안 모범 사례

### 10.1 전송 계층

| 권장 사항 | 이유 |
|----------|------|
| DoT 또는 DoH 사용 | DNS 쿼리/응답 암호화로 도청 방지 |
| TLS 1.2 이상 강제 | CoreDNS 기본값으로 이미 적용됨 |
| 인증서 자동 갱신 | Let's Encrypt + certbot 등 사용 |
| mTLS 고려 | 신뢰할 수 있는 클라이언트만 접근 허용 |

### 10.2 접근 제어

| 권장 사항 | 이유 |
|----------|------|
| bind로 인터페이스 제한 | 불필요한 인터페이스 노출 방지 |
| ACL로 IP 기반 제어 | 허용된 네트워크만 쿼리 가능 |
| AXFR/IXFR 차단 | zone 전송 남용 방지 |
| drop보다 block 선호 | 클라이언트에게 거부 이유 통보 |

### 10.3 DNSSEC

| 권장 사항 | 이유 |
|----------|------|
| ECDSA P-256 키 사용 | RSA보다 작은 키 크기, 빠른 검증 |
| KSK/ZSK 분리 | KSK 교체 주기와 ZSK 교체 주기 독립 관리 |
| cache_capacity 적절히 설정 | 서명 캐시 히트율 향상 |
| AWS Secrets Manager 키 저장 | 키 파일 디스크 노출 방지 |

### 10.4 운영

| 권장 사항 | 이유 |
|----------|------|
| debug 모드 프로덕션 비사용 | panic recovery 유지 |
| errors 플러그인 항상 활성화 | 에러 가시성 확보 |
| 메트릭 모니터링 | Panic 카운트, ACL block/drop 카운트 추적 |
| 정기적 인증서/키 교체 | 장기 키 노출 위험 최소화 |

---

## 요약

CoreDNS의 보안 아키텍처는 다음 핵심 원칙을 따른다:

1. **다층 방어(Defense in Depth)**: 네트워크(bind), 전송(TLS), 프로토콜(EDNS, CH 차단), 애플리케이션(ACL, DNSSEC) 각 계층에서 독립적인 보안 메커니즘을 제공한다. 한 계층이 뚫려도 다른 계층이 보호한다.

2. **플러그인 기반 유연성**: 모든 보안 기능이 독립 플러그인으로 구현되어, 필요한 기능만 선택적으로 활성화할 수 있다. 불필요한 보안 오버헤드를 피하면서도 필요할 때 강력한 보안을 적용할 수 있다.

3. **안전한 기본값(Secure by Default)**: TLS 1.2 최소, 강력한 암호 스위트, CH 클래스 차단, panic recovery 등이 기본으로 활성화되어 있다. 관리자가 명시적으로 완화하지 않는 한 안전한 설정이 유지된다.

4. **성능과 보안의 균형**: DNSSEC 서명 캐싱, singleflight 중복 방지, 비동기 ACL 메트릭 등으로 보안 기능이 DNS 처리 성능에 미치는 영향을 최소화한다.
