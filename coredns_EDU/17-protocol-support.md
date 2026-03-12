# 17. 프로토콜 지원 (Protocol Support)

## 개요

CoreDNS는 전통적인 UDP/TCP DNS 외에도 다양한 현대적 DNS 전송 프로토콜을 지원한다. 각 프로토콜은 별도의 서버 구조체로 구현되며, 공통 기반인 `Server` 구조체를 임베딩하여 핵심 DNS 처리 로직을 공유한다.

| 프로토콜 | 약어 | 서버 구조체 | 소스 파일 | 기본 포트 | RFC |
|---------|------|-----------|----------|----------|-----|
| DNS (UDP/TCP) | - | `Server` | `server.go` | 53 | RFC 1035 |
| DNS over TLS | DoT | `ServerTLS` | `server_tls.go` | 853 | RFC 7858 |
| DNS over HTTPS | DoH | `ServerHTTPS` | `server_https.go` | 443 | RFC 8484 |
| DNS over HTTP/3 | DoH3 | `ServerHTTPS3` | `server_https3.go` | 443 | - |
| DNS over QUIC | DoQ | `ServerQUIC` | `server_quic.go` | 853 | RFC 9250 |
| DNS over gRPC | - | `ServergRPC` | `server_grpc.go` | 443 | - |

모든 서버 파일은 `core/dnsserver/` 디렉토리에 위치한다.

---

## 1. Transport 상수 정의

```
소스: plugin/pkg/transport/transport.go (4~12행)

const (
    DNS    = "dns"
    TLS    = "tls"
    QUIC   = "quic"
    GRPC   = "grpc"
    HTTPS  = "https"
    HTTPS3 = "https3"
    UNIX   = "unix"
)
```

각 프로토콜별 기본 포트:

```
소스: plugin/pkg/transport/transport.go (15~26행)

const (
    Port      = "53"   // DNS
    TLSPort   = "853"  // DoT
    QUICPort  = "853"  // DoQ
    GRPCPort  = "443"  // gRPC
    HTTPSPort = "443"  // DoH
)
```

Corefile에서 프로토콜 선택:
```
dns://.:53      # 기본 DNS (UDP/TCP)
tls://.:853     # DNS over TLS
https://.:443   # DNS over HTTPS
https3://.:443  # DNS over HTTP/3
quic://.:853    # DNS over QUIC
grpc://.:443    # DNS over gRPC
```

---

## 2. 기본 DNS 서버 (Server) - UDP/TCP

### 2.1 Server 구조체

```
소스: core/dnsserver/server.go (36~59행)

type Server struct {
    Addr         string
    IdleTimeout  time.Duration
    ReadTimeout  time.Duration
    WriteTimeout time.Duration

    connPolicy proxyproto.ConnPolicyFunc

    server [2]*dns.Server  // 0: TCP, 1: UDP
    m      sync.Mutex

    zones        map[string][]*Config
    graceTimeout time.Duration
    trace        trace.Trace
    debug        bool
    stacktrace   bool
    classChaos   bool

    tsigSecret map[string]string

    stopOnce sync.Once
    stopErr  error
}
```

핵심 필드:

| 필드 | 설명 | 기본값 |
|------|------|--------|
| `server[0]` | TCP 서버 (net.Listener) | - |
| `server[1]` | UDP 서버 (net.PacketConn) | - |
| `IdleTimeout` | TCP 유휴 타임아웃 | 10초 |
| `ReadTimeout` | TCP 읽기 타임아웃 | 3초 |
| `WriteTimeout` | TCP 쓰기 타임아웃 | 5초 |
| `graceTimeout` | 우아한 종료 대기 시간 | 5초 |
| `tsigSecret` | TSIG 인증 키 맵 | 빈 맵 |

### 2.2 서버 생성 (NewServer)

```
소스: core/dnsserver/server.go (68~141행)

func NewServer(addr string, group []*Config) (*Server, error) {
    s := &Server{
        Addr:         addr,
        zones:        make(map[string][]*Config),
        graceTimeout: 5 * time.Second,
        IdleTimeout:  10 * time.Second,
        ReadTimeout:  3 * time.Second,
        WriteTimeout: 5 * time.Second,
        tsigSecret:   make(map[string]string),
    }
    // ...
    for _, site := range group {
        // 플러그인 체인 역순 빌드
        var stack plugin.Handler
        for i := len(site.Plugin) - 1; i >= 0; i-- {
            stack = site.Plugin[i](stack)
            site.registerHandler(stack)
        }
        site.pluginChain = stack
    }
    return s, nil
}
```

플러그인 체인 빌드가 여기서 수행된다. 모든 프로토콜별 서버가 이 `NewServer`를 호출하여 공통 초기화를 수행한 뒤, 프로토콜 특화 설정을 추가한다.

### 2.3 TCP 서버 시작 (Serve)

```
소스: core/dnsserver/server.go (148~169행)

func (s *Server) Serve(l net.Listener) error {
    s.m.Lock()
    s.server[tcp] = &dns.Server{
        Listener:      l,
        Net:           "tcp",
        TsigSecret:    s.tsigSecret,
        MaxTCPQueries: tcpMaxQueries,  // -1 (무제한)
        ReadTimeout:   s.ReadTimeout,
        WriteTimeout:  s.WriteTimeout,
        IdleTimeout:   func() time.Duration { return s.IdleTimeout },
        Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
            ctx := context.WithValue(context.Background(), Key{}, s)
            ctx = context.WithValue(ctx, LoopKey{}, 0)
            s.ServeDNS(ctx, w, r)
        }),
    }
    s.m.Unlock()
    return s.server[tcp].ActivateAndServe()
}
```

### 2.4 UDP 서버 시작 (ServePacket)

```
소스: core/dnsserver/server.go (173~183행)

func (s *Server) ServePacket(p net.PacketConn) error {
    s.m.Lock()
    s.server[udp] = &dns.Server{
        PacketConn: p,
        Net:        "udp",
        Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
            ctx := context.WithValue(context.Background(), Key{}, s)
            ctx = context.WithValue(ctx, LoopKey{}, 0)
            s.ServeDNS(ctx, w, r)
        }),
        TsigSecret: s.tsigSecret,
    }
    s.m.Unlock()
    return s.server[udp].ActivateAndServe()
}
```

### 2.5 리스너 생성

```
소스: core/dnsserver/server.go (186~212행)

func (s *Server) Listen() (net.Listener, error) {
    l, err := reuseport.Listen("tcp", s.Addr[len(transport.DNS+"://"):])
    if err != nil {
        return nil, err
    }
    if s.connPolicy != nil {
        l = &proxyproto.Listener{Listener: l, ConnPolicy: s.connPolicy}
    }
    return l, nil
}

func (s *Server) ListenPacket() (net.PacketConn, error) {
    p, err := reuseport.ListenPacket("udp", s.Addr[len(transport.DNS+"://"):])
    if err != nil {
        return nil, err
    }
    if s.connPolicy != nil {
        p = &cproxyproto.PacketConn{PacketConn: p, ConnPolicy: s.connPolicy}
    }
    return p, nil
}
```

`reuseport` 패키지를 사용하여 SO_REUSEPORT 옵션을 활성화한다. PROXY 프로토콜이 설정되면 리스너를 `proxyproto.Listener`로 래핑한다.

### 2.6 우아한 종료 (Stop)

```
소스: core/dnsserver/server.go (219~242행)

func (s *Server) Stop() error {
    s.stopOnce.Do(func() {
        ctx, cancelCtx := context.WithTimeout(context.Background(), s.graceTimeout)
        defer cancelCtx()

        var wg sync.WaitGroup
        s.m.Lock()
        for _, s1 := range s.server {
            if s1 == nil {
                continue
            }
            wg.Go(func() {
                s1.ShutdownContext(ctx)
            })
        }
        s.m.Unlock()
        wg.Wait()
        s.stopErr = ctx.Err()
    })
    return s.stopErr
}
```

`sync.Once`를 사용하여 동시 호출 시에도 한 번만 종료 절차를 실행한다. TCP와 UDP 서버를 병렬로 셧다운한다.

### 2.7 ServeDNS (DNS 멀티플렉서)

```
소스: core/dnsserver/server.go (251~374행)
```

Server.ServeDNS는 모든 프로토콜의 공통 진입점이다. 주요 처리 단계:

```
┌──────────────────────────────────────────────────────────┐
│                  Server.ServeDNS 처리 흐름                 │
├──────────────────────────────────────────────────────────┤
│                                                          │
│  1. Question 섹션 유효성 검사                               │
│     └─ 없으면 SERVFAIL 반환                                │
│                                                          │
│  2. panic recovery 설정 (debug 모드 아닌 경우)              │
│                                                          │
│  3. CH 클래스 차단 (classChaos 비활성화 시)                  │
│     └─ REFUSED 반환                                       │
│                                                          │
│  4. EDNS 버전 검사                                        │
│                                                          │
│  5. ScrubWriter 래핑 (응답 크기 자동 조정)                   │
│                                                          │
│  6. Zone 매칭 (가장 긴 접미사 매칭)                          │
│     ├─ zone 찾음 → 해당 zone의 pluginChain.ServeDNS 호출    │
│     ├─ DS 타입 → 상위 zone 검색 계속                        │
│     └─ zone 못 찾음 → "." 와일드카드 매칭 시도               │
│                                                          │
│  7. 와일드카드도 실패 → REFUSED 반환                         │
│                                                          │
└──────────────────────────────────────────────────────────┘
```

### 2.8 Context 키

```
소스: core/dnsserver/server.go (438~447행)

type (
    Key      struct{}  // 현재 서버 인스턴스
    LoopKey  struct{}  // 서버 레벨 루프 감지
    ViewKey  struct{}  // 현재 뷰 이름
)
```

### 2.9 상수

```
소스: core/dnsserver/server.go (431~436행)

const (
    tcp = 0
    udp = 1
    tcpMaxQueries = -1  // 무제한
)
```

---

## 3. DNS over TLS (ServerTLS)

### 3.1 구조체

```
소스: core/dnsserver/server_tls.go (18~21행)

type ServerTLS struct {
    *Server
    tlsConfig *tls.Config
}
```

Server를 임베딩하고 TLS 설정만 추가한다. 이 임베딩을 통해 ServeDNS, Stop 등 공통 메서드를 자동으로 상속한다.

### 3.2 서버 생성

```
소스: core/dnsserver/server_tls.go (25~41행)

func NewServerTLS(addr string, group []*Config) (*ServerTLS, error) {
    s, err := NewServer(addr, group)
    if err != nil {
        return nil, err
    }
    var tlsConfig *tls.Config
    for _, z := range s.zones {
        for _, conf := range z {
            tlsConfig = conf.TLSConfig
        }
    }
    return &ServerTLS{Server: s, tlsConfig: tlsConfig}, nil
}
```

TLS 설정은 `tls` 플러그인에 의해 Config.TLSConfig에 저장된다. NewServerTLS는 이를 가져와 서버에 적용한다.

### 3.3 TCP-TLS 리스너

```
소스: core/dnsserver/server_tls.go (47~72행)

func (s *ServerTLS) Serve(l net.Listener) error {
    s.m.Lock()
    if s.tlsConfig != nil {
        l = tls.NewListener(l, s.tlsConfig)
    }
    s.server[tcp] = &dns.Server{
        Listener:      l,
        Net:           "tcp-tls",
        MaxTCPQueries: tlsMaxQueries,  // -1 (무제한)
        ReadTimeout:   s.ReadTimeout,
        WriteTimeout:  s.WriteTimeout,
        IdleTimeout:   func() time.Duration { return s.IdleTimeout },
        Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
            ctx := context.WithValue(context.Background(), Key{}, s.Server)
            ctx = context.WithValue(ctx, LoopKey{}, 0)
            s.ServeDNS(ctx, w, r)
        }),
    }
    s.m.Unlock()
    return s.server[tcp].ActivateAndServe()
}
```

핵심: `tls.NewListener(l, s.tlsConfig)`로 일반 TCP 리스너를 TLS 리스너로 래핑한다. DNS 프로토콜 자체는 변하지 않으며, 전송 계층만 암호화된다.

### 3.4 UDP 미지원

```
소스: core/dnsserver/server_tls.go (75~76행)

func (s *ServerTLS) ServePacket(p net.PacketConn) error { return nil }
```

DoT는 TCP 전용이므로 UDP 서버는 nil을 반환한다.

### 3.5 주소 파싱

```
소스: core/dnsserver/server_tls.go (78~87행)

func (s *ServerTLS) Listen() (net.Listener, error) {
    l, err := reuseport.Listen("tcp", s.Addr[len(transport.TLS+"://"):])
    // ...
}
```

주소에서 `tls://` 접두사를 제거하여 실제 바인드 주소를 추출한다.

---

## 4. DNS over HTTPS (ServerHTTPS)

### 4.1 구조체

```
소스: core/dnsserver/server_https.go (32~39행)

type ServerHTTPS struct {
    *Server
    httpsServer    *http.Server
    listenAddr     net.Addr
    tlsConfig      *tls.Config
    validRequest   func(*http.Request) bool
    maxConnections int
}
```

| 필드 | 설명 |
|------|------|
| httpsServer | Go 표준 HTTP 서버 |
| validRequest | HTTP 요청 유효성 검증 함수 |
| maxConnections | 최대 동시 연결 수 (기본 200) |

### 4.2 서버 생성

```
소스: core/dnsserver/server_https.go (55~108행)

func NewServerHTTPS(addr string, group []*Config) (*ServerHTTPS, error) {
    s, err := NewServer(addr, group)
    // ...
    if tlsConfig != nil {
        tlsConfig.NextProtos = []string{"h2", "http/1.1"}
    }
    // ...
    if validator == nil {
        validator = func(r *http.Request) bool { return r.URL.Path == doh.Path }
    }
    // ...
    maxConnections := DefaultHTTPSMaxConnections  // 200
}
```

핵심 설정:
- **ALPN**: `h2`, `http/1.1` 프로토콜 협상 (HTTP/2 우선)
- **요청 검증**: 기본적으로 `/dns-query` 경로만 허용 (RFC 8484)
- **동시 연결 제한**: `DefaultHTTPSMaxConnections = 200`

### 4.3 HTTP 핸들러

```
소스: core/dnsserver/server_https.go (174~229행)

func (s *ServerHTTPS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    if !s.validRequest(r) {
        http.Error(w, "", http.StatusNotFound)
        return
    }

    msg, err := doh.RequestToMsg(r)
    if err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }

    h, p, _ := net.SplitHostPort(r.RemoteAddr)
    port, _ := strconv.Atoi(p)
    dw := &DoHWriter{
        laddr:   s.listenAddr,
        raddr:   &net.TCPAddr{IP: net.ParseIP(h), Port: port},
        request: r,
    }

    ctx := context.WithValue(r.Context(), Key{}, s.Server)
    ctx = context.WithValue(ctx, LoopKey{}, 0)
    ctx = context.WithValue(ctx, HTTPRequestKey{}, r)
    s.ServeDNS(ctx, dw, msg)

    // 응답 처리
    buf, _ := dw.Msg.Pack()
    mt, _ := response.Typify(dw.Msg, time.Now().UTC())
    age := dnsutil.MinimalTTL(dw.Msg, mt)

    w.Header().Set("Content-Type", doh.MimeType)
    w.Header().Set("Cache-Control", fmt.Sprintf("max-age=%d", uint32(age.Seconds())))
    w.WriteHeader(http.StatusOK)
    w.Write(buf)
}
```

DoH 처리 흐름:

```
┌──────────────────────────────────────────────────────────┐
│                   DoH 요청 처리 흐름                       │
├──────────────────────────────────────────────────────────┤
│                                                          │
│  1. HTTP 요청 경로 검증 (/dns-query)                       │
│  2. doh.RequestToMsg()로 HTTP → DNS 메시지 변환            │
│     ├─ GET: base64url 쿼리 파라미터 디코딩                  │
│     └─ POST: 요청 바디 디코딩                              │
│  3. DoHWriter 생성 (HTTP → DNS ResponseWriter 어댑터)      │
│  4. HTTP 요청 컨텍스트를 DNS 처리에 전파                     │
│  5. Server.ServeDNS() 호출 (공통 DNS 처리)                 │
│  6. DNS 응답 → HTTP 응답 변환                              │
│     ├─ Content-Type: application/dns-message               │
│     ├─ Cache-Control: max-age=<TTL>                        │
│     └─ 바이너리 DNS 메시지 본문                              │
│                                                          │
└──────────────────────────────────────────────────────────┘
```

### 4.4 HTTPRequestKey

```
소스: core/dnsserver/server_https.go (52행)

type HTTPRequestKey struct{}
```

HTTP 요청 객체를 컨텍스트에 저장하여, 다운스트림 플러그인이 HTTP 헤더, 클라이언트 IP 등을 참조할 수 있게 한다.

### 4.5 연결 제한

```
소스: core/dnsserver/server_https.go (114~129행)

func (s *ServerHTTPS) Serve(l net.Listener) error {
    // ...
    if s.maxConnections > 0 {
        l = netutil.LimitListener(l, s.maxConnections)
    }
    if s.tlsConfig != nil {
        l = tls.NewListener(l, s.tlsConfig)
    }
    return s.httpsServer.Serve(l)
}
```

`netutil.LimitListener`로 동시 연결 수를 제한한다. TLS 래핑보다 **먼저** 적용하여, TLS 핸드셰이크 전에 연결 제한을 걸 수 있다.

### 4.6 로그 어댑터

```
소스: core/dnsserver/server_https.go (42~48행)

type loggerAdapter struct{}

func (l *loggerAdapter) Write(p []byte) (n int, err error) {
    clog.Debug(string(p))
    return len(p), nil
}
```

HTTP 서버의 에러 로그를 CoreDNS의 DEBUG 레벨 로그로 리다이렉트한다.

---

## 5. DNS over HTTP/3 (ServerHTTPS3)

### 5.1 구조체

```
소스: core/dnsserver/server_https3.go (31~39행)

type ServerHTTPS3 struct {
    *Server
    httpsServer  *http3.Server
    listenAddr   net.Addr
    tlsConfig    *tls.Config
    quicConfig   *quic.Config
    validRequest func(*http.Request) bool
    maxStreams   int
}
```

DoH와 유사하지만, HTTP/3(QUIC 기반)을 사용한다.

### 5.2 서버 생성

```
소스: core/dnsserver/server_https3.go (42~108행)

func NewServerHTTPS3(addr string, group []*Config) (*ServerHTTPS3, error) {
    // ...
    if tlsConfig == nil {
        return nil, fmt.Errorf("DoH3 requires TLS, no TLS config found")
    }
    tlsConfig.NextProtos = []string{"h3"}

    qconf := &quic.Config{
        MaxIdleTimeout: s.IdleTimeout,
        Allow0RTT:      true,
    }
    if maxStreams > 0 {
        qconf.MaxIncomingStreams = int64(maxStreams)
        qconf.MaxIncomingUniStreams = int64(maxStreams)
    }

    h3srv := &http3.Server{
        TLSConfig:       tlsConfig,
        EnableDatagrams: true,
        QUICConfig:      qconf,
    }
    // ...
}
```

핵심 차이점:
- **ALPN**: `h3` (HTTP/3 전용)
- **TLS 필수**: DoH3는 TLS 없이 사용할 수 없다
- **0-RTT**: 기본 활성화 (재연결 시 왕복 없이 즉시 데이터 전송)
- **EnableDatagrams**: QUIC 데이터그램 지원 활성화
- **기본 최대 스트림**: `DefaultHTTPS3MaxStreams = 256`

### 5.3 ServePacket (UDP 기반)

```
소스: core/dnsserver/server_https3.go (125~131행)

func (s *ServerHTTPS3) ServePacket(pc net.PacketConn) error {
    s.m.Lock()
    s.listenAddr = pc.LocalAddr()
    s.m.Unlock()
    return s.httpsServer.Serve(pc)
}
```

HTTP/3는 QUIC를 사용하므로 UDP 소켓에서 서빙한다. TCP 리스너는 사용하지 않는다.

### 5.4 HTTP 핸들러

```
소스: core/dnsserver/server_https3.go (168~214행)

func (s *ServerHTTPS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    // DoH와 동일한 처리 로직
    // 차이점: raddr가 *net.UDPAddr (TCP가 아닌 UDP)
    dw := &DoHWriter{
        laddr:   s.listenAddr,
        raddr:   &net.UDPAddr{IP: net.ParseIP(h), Port: port},
        request: r,
    }
    // ...
}
```

DoH와 거의 동일한 핸들러이지만, 원격 주소가 `net.UDPAddr`이다.

---

## 6. DNS over QUIC (ServerQUIC)

### 6.1 구조체

```
소스: core/dnsserver/server_quic.go (47~55행)

type ServerQUIC struct {
    *Server
    listenAddr        net.Addr
    tlsConfig         *tls.Config
    quicConfig        *quic.Config
    quicListener      *quic.Listener
    maxStreams        int
    streamProcessPool chan struct{}
}
```

| 필드 | 설명 |
|------|------|
| quicListener | QUIC 리스너 |
| maxStreams | 연결당 최대 동시 스트림 수 |
| streamProcessPool | 워커 풀 세마포어 |

### 6.2 DoQ 에러 코드

```
소스: core/dnsserver/server_quic.go (23~44행)

const (
    DoQCodeNoError       quic.ApplicationErrorCode = 0  // 정상 종료
    DoQCodeInternalError quic.ApplicationErrorCode = 1  // 내부 에러
    DoQCodeProtocolError quic.ApplicationErrorCode = 2  // 프로토콜 에러

    DefaultMaxQUICStreams     = 256   // 연결당 최대 스트림
    DefaultQUICStreamWorkers = 1024  // 워커 풀 크기
)
```

### 6.3 서버 생성

```
소스: core/dnsserver/server_quic.go (58~102행)

func NewServerQUIC(addr string, group []*Config) (*ServerQUIC, error) {
    // ...
    if tlsConfig != nil {
        tlsConfig.NextProtos = []string{"doq"}
    }

    var quicConfig = &quic.Config{
        MaxIdleTimeout:       s.IdleTimeout,
        MaxIncomingStreams:    int64(maxStreams),
        MaxIncomingUniStreams: int64(maxStreams),
        Allow0RTT:            true,
    }

    return &ServerQUIC{
        Server:            s,
        tlsConfig:         tlsConfig,
        quicConfig:        quicConfig,
        maxStreams:        maxStreams,
        streamProcessPool: make(chan struct{}, streamProcessPoolSize),
    }, nil
}
```

핵심 설정:
- **ALPN**: `doq` (DNS over QUIC 전용)
- **0-RTT**: 기본 활성화
- **워커 풀**: 채널 기반 세마포어 (`streamProcessPool`)로 동시 처리 스트림 수 제한

### 6.4 QUIC 연결 처리

```
소스: core/dnsserver/server_quic.go (122~184행)

func (s *ServerQUIC) ServeQUIC() error {
    for {
        conn, err := s.quicListener.Accept(context.Background())
        if err != nil {
            // 에러 처리...
            return err
        }
        go s.serveQUICConnection(conn)
    }
}

func (s *ServerQUIC) serveQUICConnection(conn *quic.Conn) {
    for {
        stream, err := conn.AcceptStream(context.Background())
        if err != nil {
            // 에러 처리...
            return
        }
        // 워커 풀에서 슬롯 획득 후 스트림 처리
        select {
        case s.streamProcessPool <- struct{}{}:
            go func(st *quic.Stream, cn *quic.Conn) {
                defer func() { <-s.streamProcessPool }()
                s.serveQUICStream(st, cn)
            }(stream, conn)
        default:
            go func(st *quic.Stream, cn *quic.Conn) {
                select {
                case s.streamProcessPool <- struct{}{}:
                    defer func() { <-s.streamProcessPool }()
                    s.serveQUICStream(st, cn)
                case <-conn.Context().Done():
                    st.Close()
                    return
                }
            }(stream, conn)
        }
    }
}
```

DoQ 처리 아키텍처:

```
┌──────────────────────────────────────────────────────────┐
│                  DoQ 처리 아키텍처                          │
├──────────────────────────────────────────────────────────┤
│                                                          │
│  QUIC Listener                                           │
│       │                                                  │
│       ├─ Accept Connection ─── goroutine per connection  │
│       │       │                                          │
│       │       ├─ Accept Stream ─── 워커 풀에서 슬롯 획득   │
│       │       │       │                                  │
│       │       │       ├─ 슬롯 즉시 확보 → 스트림 처리      │
│       │       │       └─ 슬롯 대기 → 연결 컨텍스트 취소?   │
│       │       │               ├─ 아니오 → 대기 후 처리     │
│       │       │               └─ 예 → 스트림 닫기          │
│       │       │                                          │
│       │       └─ (다음 스트림...)                          │
│       │                                                  │
│       └─ (다음 연결...)                                    │
│                                                          │
└──────────────────────────────────────────────────────────┘
```

### 6.5 스트림 처리

```
소스: core/dnsserver/server_quic.go (186~236행)

func (s *ServerQUIC) serveQUICStream(stream *quic.Stream, conn *quic.Conn) {
    buf, err := readDOQMessage(stream)
    // ...

    req := &dns.Msg{}
    err = req.Unpack(buf)
    // ...

    if !validRequest(req) {
        s.closeQUICConn(conn, DoQCodeProtocolError)
        return
    }

    w := &DoQWriter{
        localAddr:  conn.LocalAddr(),
        remoteAddr: conn.RemoteAddr(),
        stream:     stream,
        Msg:        req,
    }

    dnsCtx := context.WithValue(stream.Context(), Key{}, s.Server)
    dnsCtx = context.WithValue(dnsCtx, LoopKey{}, 0)
    s.ServeDNS(dnsCtx, w, req)
}
```

### 6.6 DoQ 메시지 읽기

```
소스: core/dnsserver/server_quic.go (348~376행)

func readDOQMessage(r io.Reader) ([]byte, error) {
    sizeBuf := make([]byte, 2)
    _, err := io.ReadFull(r, sizeBuf)
    // ...
    size := binary.BigEndian.Uint16(sizeBuf)
    if size == 0 {
        return nil, fmt.Errorf("message size is 0: probably unsupported DoQ version")
    }
    buf := make([]byte, size)
    _, err = io.ReadFull(r, buf)
    return buf, err
}
```

RFC 9250에 따라, DoQ 메시지는 2바이트 길이 접두사 + DNS 메시지 형식이다. DNS-over-TCP와 동일한 프레이밍이지만, QUIC 스트림 위에서 동작한다.

### 6.7 프로토콜 에러 검증

```
소스: core/dnsserver/server_quic.go (311~342행)

func validRequest(req *dns.Msg) (ok bool) {
    // 1. Message ID는 반드시 0이어야 한다 (RFC 9250)
    if req.Id != 0 {
        return false
    }
    // 2. edns-tcp-keepalive 옵션 금지
    if opt := req.IsEdns0(); opt != nil {
        for _, option := range opt.Option {
            if option.Option() == dns.EDNS0TCPKEEPALIVE {
                return false
            }
        }
    }
    return true
}
```

RFC 9250에서 정의한 프로토콜 규칙을 검증한다. 위반 시 `DoQCodeProtocolError`로 연결을 강제 종료한다.

### 6.8 에러 처리

```
소스: core/dnsserver/server_quic.go (380~405행)

func (s *ServerQUIC) isExpectedErr(err error) bool {
    if errors.Is(err, quic.ErrServerClosed) {
        return true  // 정상적인 서버 종료
    }
    var qAppErr *quic.ApplicationError
    if errors.As(err, &qAppErr) && qAppErr.ErrorCode == 2 {
        return true  // 프로토콜 에러로 인한 종료
    }
    var qIdleErr *quic.IdleTimeoutError
    return errors.As(err, &qIdleErr)  // 유휴 타임아웃
}
```

---

## 7. DNS over gRPC (ServergRPC)

### 7.1 구조체

```
소스: core/dnsserver/server_grpc.go (40~48행)

type ServergRPC struct {
    *Server
    *pb.UnimplementedDnsServiceServer
    grpcServer     *grpc.Server
    listenAddr     net.Addr
    tlsConfig      *tls.Config
    maxStreams     int
    maxConnections int
}
```

### 7.2 상수

```
소스: core/dnsserver/server_grpc.go (24~37행)

const (
    maxDNSMessageBytes      = dns.MaxMsgSize         // 65535
    maxProtobufPayloadBytes = maxDNSMessageBytes + 4  // protobuf 오버헤드

    DefaultGRPCMaxStreams     = 256  // 연결당 최대 스트림
    DefaultGRPCMaxConnections = 200  // 최대 동시 연결
)
```

### 7.3 서버 생성

```
소스: core/dnsserver/server_grpc.go (51~87행)

func NewServergRPC(addr string, group []*Config) (*ServergRPC, error) {
    // ...
    if tlsConfig != nil {
        tlsConfig.NextProtos = []string{"h2"}
    }
    // ...
}
```

gRPC는 HTTP/2를 기반으로 하므로, ALPN에 `h2`를 설정한다.

### 7.4 gRPC 서버 설정

```
소스: core/dnsserver/server_grpc.go (93~129행)

func (s *ServergRPC) Serve(l net.Listener) error {
    // ...
    serverOpts := []grpc.ServerOption{
        grpc.MaxRecvMsgSize(maxProtobufPayloadBytes),
        grpc.MaxSendMsgSize(maxProtobufPayloadBytes),
    }

    if s.maxStreams > 0 {
        serverOpts = append(serverOpts, grpc.MaxConcurrentStreams(uint32(s.maxStreams)))
    }

    if s.Tracer() != nil {
        // OpenTracing 인터셉터 (부모 스팬이 있을 때만 추적)
        serverOpts = append(serverOpts, grpc.UnaryInterceptor(
            otgrpc.OpenTracingServerInterceptor(s.Tracer(),
                otgrpc.IncludingSpans(onlyIfParent)),
        ))
    }

    s.grpcServer = grpc.NewServer(serverOpts...)
    pb.RegisterDnsServiceServer(s.grpcServer, s)

    if s.tlsConfig != nil {
        l = tls.NewListener(l, s.tlsConfig)
    }
    if s.maxConnections > 0 {
        l = netutil.LimitListener(l, s.maxConnections)
    }

    return s.grpcServer.Serve(l)
}
```

### 7.5 Query 메서드 (gRPC 진입점)

```
소스: core/dnsserver/server_grpc.go (176~208행)

func (s *ServergRPC) Query(ctx context.Context, in *pb.DnsPacket) (*pb.DnsPacket, error) {
    if len(in.GetMsg()) > dns.MaxMsgSize {
        return nil, fmt.Errorf("dns message exceeds size limit: %d", len(in.GetMsg()))
    }
    msg := new(dns.Msg)
    err := msg.Unpack(in.GetMsg())
    // ...

    p, ok := peer.FromContext(ctx)
    // ...
    a, ok := p.Addr.(*net.TCPAddr)
    // ...

    w := &gRPCresponse{localAddr: s.listenAddr, remoteAddr: a, Msg: msg}

    dnsCtx := context.WithValue(ctx, Key{}, s.Server)
    dnsCtx = context.WithValue(dnsCtx, LoopKey{}, 0)
    s.ServeDNS(dnsCtx, w, msg)

    packed, err := w.Msg.Pack()
    return &pb.DnsPacket{Msg: packed}, nil
}
```

gRPC DNS 처리 흐름:

```
┌────────────────────────────────────────────────────┐
│              gRPC DNS 처리 흐름                      │
├────────────────────────────────────────────────────┤
│                                                    │
│  1. Protobuf DnsPacket 수신                         │
│  2. 크기 제한 검사 (65535 바이트)                     │
│  3. dns.Msg.Unpack()으로 DNS 메시지 디코딩           │
│  4. gRPC peer 컨텍스트에서 클라이언트 주소 추출        │
│  5. gRPCresponse 어댑터 생성                        │
│  6. Server.ServeDNS() 호출                         │
│  7. 응답 DNS 메시지를 Protobuf로 패킹               │
│  8. DnsPacket 반환                                 │
│                                                    │
└────────────────────────────────────────────────────┘
```

### 7.6 gRPCresponse 어댑터

```
소스: core/dnsserver/server_grpc.go (218~242행)

type gRPCresponse struct {
    localAddr  net.Addr
    remoteAddr net.Addr
    Msg        *dns.Msg
}

func (r *gRPCresponse) Write(b []byte) (int, error) {
    r.Msg = new(dns.Msg)
    return len(b), r.Msg.Unpack(b)
}

func (r *gRPCresponse) WriteMsg(m *dns.Msg) error { r.Msg = m; return nil }
```

gRPCresponse는 `dns.ResponseWriter` 인터페이스를 구현하는 어댑터이다. 실제로 네트워크에 쓰는 것이 아니라, 메모리에 DNS 응답을 저장하여 나중에 Protobuf로 변환한다.

### 7.7 우아한 종료

```
소스: core/dnsserver/server_grpc.go (164~171행)

func (s *ServergRPC) Stop() (err error) {
    s.m.Lock()
    defer s.m.Unlock()
    if s.grpcServer != nil {
        s.grpcServer.GracefulStop()
    }
    return
}
```

`GracefulStop()`은 진행 중인 RPC를 완료한 뒤 서버를 종료한다.

---

## 8. 프로토콜별 비교

### 8.1 아키텍처 비교

```
┌──────────────────────────────────────────────────────────────────┐
│                    프로토콜별 계층 구조                              │
├──────────────────────────────────────────────────────────────────┤
│                                                                  │
│  DNS (UDP)    : UDP → dns.Server → ServeDNS                      │
│  DNS (TCP)    : TCP → dns.Server → ServeDNS                      │
│  DoT          : TCP → TLS → dns.Server → ServeDNS                │
│  DoH          : TCP → TLS → HTTP/1.1|2 → ServeHTTP → ServeDNS   │
│  DoH3         : UDP → QUIC → HTTP/3 → ServeHTTP → ServeDNS      │
│  DoQ          : UDP → QUIC → Stream → serveDOQStream → ServeDNS │
│  gRPC         : TCP → TLS → HTTP/2 → gRPC → Query → ServeDNS    │
│                                                                  │
└──────────────────────────────────────────────────────────────────┘
```

### 8.2 기능 비교 테이블

| 기능 | DNS | DoT | DoH | DoH3 | DoQ | gRPC |
|------|-----|-----|-----|------|-----|------|
| 전송 프로토콜 | UDP+TCP | TCP | TCP | UDP(QUIC) | UDP(QUIC) | TCP |
| 암호화 | 없음 | TLS | TLS | TLS+QUIC | TLS+QUIC | TLS(선택) |
| 0-RTT | 해당없음 | 해당없음 | 해당없음 | 지원 | 지원 | 해당없음 |
| 연결 제한 | 없음 | 없음 | 200 | 스트림 256 | 스트림 256 | 200/256 |
| TSIG | 지원 | 미지원 | 미지원 | 미지원 | 미지원 | 미지원 |
| Proxy Protocol | 지원 | 지원 | 지원 | 지원 | 지원 | 지원 |
| 메트릭 | 기본 | 기본 | HTTP 상태코드 | HTTP 상태코드 | DoQ 에러코드 | 기본 |
| Tracing | 지원 | 지원 | 지원 | 지원 | 지원 | OpenTracing |

### 8.3 ResponseWriter 어댑터 비교

각 프로토콜은 고유한 ResponseWriter 어댑터를 사용하여 DNS 응답을 해당 프로토콜의 형식으로 변환한다:

| 프로토콜 | 어댑터 | 특징 |
|---------|--------|------|
| DNS | `dns.ResponseWriter` (직접) | 표준 DNS 와이어 형식 |
| DoT | `dns.ResponseWriter` (직접) | TCP-TLS 위의 표준 DNS |
| DoH | `DoHWriter` | HTTP 응답으로 변환, Cache-Control 헤더 |
| DoH3 | `DoHWriter` | HTTP/3 응답, UDPAddr 사용 |
| DoQ | `DoQWriter` | QUIC 스트림에 2바이트 길이 접두사 + DNS 메시지 |
| gRPC | `gRPCresponse` | Protobuf DnsPacket으로 변환 |

---

## 9. TLS 설정 관리

### 9.1 tls 플러그인

```
소스: plugin/tls/tls.go (23~77행)

func parseTLS(c *caddy.Controller) error {
    config := dnsserver.GetConfig(c)
    if config.TLSConfig != nil {
        return plugin.Error("tls", c.Errf("TLS already configured for this server instance"))
    }

    for c.Next() {
        args := c.RemainingArgs()
        // args: cert_file key_file [ca_file]

        clientAuth := ctls.NoClientCert
        for c.NextBlock() {
            switch c.Val() {
            case "client_auth":
                // nocert, request, require, verify_if_given, require_and_verify
            }
        }

        tls, err := tls.NewTLSConfigFromArgs(args...)
        tls.ClientAuth = clientAuth
        tls.ClientCAs = tls.RootCAs
        config.TLSConfig = tls
    }
}
```

### 9.2 TLS 기본값

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
        // ...
    }
}
```

모든 프로토콜에서 공유하는 TLS 보안 기본값:
- 최소 TLS 1.2, 최대 TLS 1.3
- ECDHE 키 교환만 허용
- GCM 또는 ChaCha20-Poly1305 암호화만 허용

---

## 10. Corefile 프로토콜 설정 예시

### 10.1 기본 DNS

```
.:53 {
    forward . 8.8.8.8
}
```

### 10.2 DNS over TLS

```
tls://.:853 {
    tls /etc/ssl/cert.pem /etc/ssl/key.pem
    forward . 8.8.8.8
}
```

### 10.3 DNS over HTTPS

```
https://.:443 {
    tls /etc/ssl/cert.pem /etc/ssl/key.pem
    forward . 8.8.8.8
}
```

### 10.4 DNS over QUIC

```
quic://.:853 {
    tls /etc/ssl/cert.pem /etc/ssl/key.pem
    forward . 8.8.8.8
}
```

### 10.5 DNS over gRPC

```
grpc://.:443 {
    tls /etc/ssl/cert.pem /etc/ssl/key.pem
    forward . 8.8.8.8
}
```

### 10.6 다중 프로토콜 동시 운용

```
.:53 {
    forward . 8.8.8.8
}

tls://.:853 {
    tls /etc/ssl/cert.pem /etc/ssl/key.pem
    forward . 8.8.8.8
}

https://.:443 {
    tls /etc/ssl/cert.pem /etc/ssl/key.pem
    forward . 8.8.8.8
}
```

---

## 요약

CoreDNS의 프로토콜 지원 아키텍처는 다음 핵심 원칙을 따른다:

1. **임베딩 기반 코드 재사용**: 모든 프로토콜별 서버가 `Server` 구조체를 임베딩하여 `ServeDNS`, `Stop` 등 공통 로직을 공유한다. 프로토콜 특화 부분만 오버라이드한다.

2. **어댑터 패턴**: 각 프로토콜은 고유한 ResponseWriter 어댑터를 제공하여, HTTP, QUIC, gRPC 등 다양한 전송 계층을 표준 `dns.ResponseWriter` 인터페이스로 통합한다.

3. **중앙 집중식 TLS 관리**: `tls` 플러그인이 TLS 설정을 한 곳에서 관리하고, 각 프로토콜 서버가 이를 참조한다. 보안 기본값(TLS 1.2+, 강력한 암호 스위트)이 모든 프로토콜에 일관되게 적용된다.

4. **자원 제한**: 각 프로토콜에 적합한 연결/스트림 제한 메커니즘을 제공한다. DoH는 `netutil.LimitListener`, DoQ는 QUIC 스트림 제한 + 워커 풀, gRPC는 `MaxConcurrentStreams` + 연결 제한을 사용한다.
