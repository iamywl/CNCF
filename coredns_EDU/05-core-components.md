# CoreDNS 핵심 컴포넌트

## 1. 개요

CoreDNS의 핵심 컴포넌트는 서버 엔진, 플러그인 시스템, 그리고 주요 플러그인(Cache, Forward, Kubernetes)으로 나뉜다. 이 문서에서는 각 컴포넌트의 내부 동작 원리를 설명한다.

## 2. Server (DNS 서버 엔진)

### 2.1 Server 구조체

**소스코드 경로**: `core/dnsserver/server.go`

```go
type Server struct {
    Addr         string
    IdleTimeout  time.Duration    // 기본 10초
    ReadTimeout  time.Duration    // 기본 3초
    WriteTimeout time.Duration    // 기본 5초

    server [2]*dns.Server        // [0]=TCP, [1]=UDP
    m      sync.Mutex

    zones        map[string][]*Config  // Zone별 Config 목록
    graceTimeout time.Duration         // 5초
    trace        trace.Trace
    debug        bool
    classChaos   bool
    tsigSecret   map[string]string
    stopOnce     sync.Once
}
```

Server는 하나의 리스닝 주소에 대해 TCP와 UDP를 동시에 처리한다. `server[0]`이 TCP, `server[1]`이 UDP 서버다.

### 2.2 듀얼 프로토콜 리스닝

**Serve (TCP)**:
```go
func (s *Server) Serve(l net.Listener) error {
    s.server[tcp] = &dns.Server{
        Listener: l,
        Net: "tcp",
        Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
            ctx := context.WithValue(context.Background(), Key{}, s)
            ctx = context.WithValue(ctx, LoopKey{}, 0)
            s.ServeDNS(ctx, w, r)
        }),
    }
    return s.server[tcp].ActivateAndServe()
}
```

**ServePacket (UDP)**:
```go
func (s *Server) ServePacket(p net.PacketConn) error {
    s.server[udp] = &dns.Server{
        PacketConn: p,
        Net: "udp",
        Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
            ctx := context.WithValue(context.Background(), Key{}, s)
            ctx = context.WithValue(ctx, LoopKey{}, 0)
            s.ServeDNS(ctx, w, r)
        }),
    }
    return s.server[udp].ActivateAndServe()
}
```

### 2.3 ServeDNS: Zone 라우팅 멀티플렉서

`ServeDNS`는 CoreDNS의 핵심 요청 라우팅 로직이다.

```
수신된 쿼리: www.sub.example.com. A?

Zone 맵:
  "example.com."  → [config1]
  "sub.example.com." → [config2]
  "." → [config3]

매칭 순서 (longest match):
  1. "www.sub.example.com." → 없음
  2. "sub.example.com."     → config2 발견!
     → config2.pluginChain.ServeDNS() 실행
```

**핵심 로직**:

```go
func (s *Server) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) {
    // 1. 유효성 검증
    if r == nil || len(r.Question) == 0 {
        errorAndMetricsFunc(s.Addr, w, r, dns.RcodeServerFailure)
        return
    }

    // 2. panic 복구 (운영 안정성)
    defer func() {
        if rec := recover(); rec != nil {
            vars.Panic.Inc()
            errorAndMetricsFunc(s.Addr, w, r, dns.RcodeServerFailure)
        }
    }()

    // 3. CH 클래스 차단 (chaos/forward 플러그인 미사용 시)
    if !s.classChaos && r.Question[0].Qclass != dns.ClassINET {
        errorAndMetricsFunc(s.Addr, w, r, dns.RcodeRefused)
        return
    }

    // 4. EDNS 버전 체크
    if m, err := edns.Version(r); err != nil {
        w.WriteMsg(m)
        return
    }

    // 5. ScrubWriter 래핑 (응답 크기 자동 조정)
    w = request.NewScrubWriter(r, w)

    // 6. Zone 매칭 (longest match)
    q := strings.ToLower(r.Question[0].Name)
    for {
        if z, ok := s.zones[q[off:]]; ok {
            // FilterFunc 통과 확인 후 pluginChain 실행
            ...
        }
        off, end = dns.NextLabel(q, off)
        if end { break }
    }

    // 7. 마지막으로 루트 Zone "." 시도
    // 8. 없으면 REFUSED
}
```

### 2.4 Context에 저장되는 값

| Key | 타입 | 용도 |
|-----|------|------|
| `Key{}` | `*Server` | 현재 서버 인스턴스 참조 |
| `LoopKey{}` | `int` | 루프 감지 카운터 |
| `ViewKey{}` | `string` | 뷰 이름 (view 플러그인) |

## 3. Plugin/Handler 시스템

### 3.1 핵심 인터페이스

**소스코드 경로**: `plugin/plugin.go`

```go
// Plugin은 미들웨어 팩토리 함수
type Plugin func(Handler) Handler

// Handler는 DNS 요청 처리기
type Handler interface {
    ServeDNS(context.Context, dns.ResponseWriter, *dns.Msg) (int, error)
    Name() string
}
```

### 3.2 NextOrFailure: 체인 실행

```go
func NextOrFailure(name string, next Handler, ctx context.Context,
    w dns.ResponseWriter, r *dns.Msg) (int, error) {
    if next != nil {
        // 트레이싱 지원: 자식 span 생성
        if span := ot.SpanFromContext(ctx); span != nil {
            child := span.Tracer().StartSpan(next.Name(), ot.ChildOf(span.Context()))
            defer child.Finish()
            ctx = ot.ContextWithSpan(ctx, child)
        }
        // pluginWriter로 래핑하여 응답 기록 플러그인 추적
        pw := &pluginWriter{ResponseWriter: w, plugin: next.Name()}
        return next.ServeDNS(ctx, pw, r)
    }
    return dns.RcodeServerFailure, Error(name, errors.New("no next plugin found"))
}
```

### 3.3 pluginWriter: 응답 추적

`pluginWriter`는 `dns.ResponseWriter`를 래핑하여 어떤 플러그인이 최종 응답을 썼는지 추적한다.

```go
type pluginWriter struct {
    dns.ResponseWriter
    plugin string  // 현재 플러그인 이름
}

func (pw *pluginWriter) WriteMsg(m *dns.Msg) error {
    if tracker, ok := pw.ResponseWriter.(PluginTracker); ok {
        tracker.SetPlugin(pw.plugin)  // 응답 기록 플러그인 설정
    }
    return pw.ResponseWriter.WriteMsg(m)
}
```

이 정보는 Prometheus 메트릭에서 `plugin` 레이블로 사용된다.

### 3.4 플러그인 등록과 setup

각 플러그인의 `init()` → `setup()` 흐름:

```
plugin/cache/setup.go:
  init() {
    plugin.Register("cache", setup)  // Caddy에 "cache" 디렉티브 등록
  }

  setup(c *caddy.Controller) error {
    // 1. Corefile에서 cache 블록 파싱
    // 2. Cache 인스턴스 생성
    // 3. Config에 Plugin 함수 추가
    dnsserver.GetConfig(c).AddPlugin(func(next plugin.Handler) plugin.Handler {
        return &Cache{Next: next, ...}
    })
  }
```

## 4. Request 래퍼

### 4.1 Request 구조체

**소스코드 경로**: `request/request.go`

```go
type Request struct {
    Req *dns.Msg
    W   dns.ResponseWriter
    Zone string

    // Lazy-cached values
    size      uint16
    do        bool
    family    int8
    name      string
    ip        string
    port      string
    localPort string
    localIP   string
}
```

### 4.2 왜 Request 래퍼가 필요한가?

1. **일관된 인터페이스**: 모든 플러그인이 동일한 API로 요청 정보에 접근
2. **Lazy Caching**: IP, 포트, EDNS 정보 등을 한 번만 파싱하고 캐시
3. **대소문자 정규화**: DNS 이름을 소문자로 통일
4. **프로토콜 추상화**: UDP/TCP/TLS 등 프로토콜 차이를 추상화

### 4.3 ScrubWriter

**소스코드 경로**: `request/writer.go`

ScrubWriter는 응답 메시지를 클라이언트 버퍼 크기에 맞게 자동 조정한다.

```go
func (s *ScrubWriter) WriteMsg(m *dns.Msg) error {
    state := Request{Req: s.req, W: s.ResponseWriter}
    state.SizeAndDo(m)  // 1. EDNS0 OPT 레코드 반영
    state.Scrub(m)      // 2. 크기 맞춤 (Truncate + Compress)
    return s.ResponseWriter.WriteMsg(m)
}
```

Scrub 알고리즘:
- `reply.Truncate(size)`: 버퍼 초과 시 레코드 제거 + TC 비트 설정
- UDP IPv4에서 1480바이트 초과 시 → 압축 활성화
- UDP IPv6에서 1220바이트 초과 시 → 압축 활성화

## 5. Cache 플러그인

### 5.1 이중 캐시 구조

**소스코드 경로**: `plugin/cache/cache.go`

```go
type Cache struct {
    Next  plugin.Handler
    Zones []string

    ncache  *cache.Cache[*item]  // 음성 캐시 (NXDOMAIN, NODATA)
    ncap    int                  // 음성 캐시 용량 (기본 9984)
    nttl    time.Duration        // 음성 최대 TTL (기본 1800초)

    pcache  *cache.Cache[*item]  // 양성 캐시 (성공 응답)
    pcap    int                  // 양성 캐시 용량 (기본 9984)
    pttl    time.Duration        // 양성 최대 TTL (기본 3600초)

    prefetch   int               // 프리페치 히트 임계값
    duration   time.Duration     // 프리페치 윈도우 (1분)
    percentage int               // TTL 남은 비율 임계값 (10%)

    staleUpTo   time.Duration    // 노화 서빙 시간
    verifyStale bool             // 만료 시 검증
}
```

### 5.2 캐시 키 생성

```go
func hash(qname string, qtype uint16, do, cd bool) uint64 {
    h := fnv.New64()
    // QNAME + QTYPE + DO비트 + CD비트 → FNV-64 해시
}
```

동일 도메인이라도 DNSSEC 관련 비트가 다르면 별도 캐시 항목을 사용한다.

### 5.3 ServeDNS 흐름

**소스코드 경로**: `plugin/cache/handler.go`

```
Cache.ServeDNS(ctx, w, r)
  │
  ├── Zone 매칭 확인
  │
  ├── getIfNotStale(now, state, server)
  │   ├── ncache.Get(key) → 음성 캐시 조회
  │   └── pcache.Get(key) → 양성 캐시 조회
  │
  ├── [캐시 히트 + 유효]
  │   ├── shouldPrefetch() 확인
  │   │   └── [yes] → go doPrefetch() (백그라운드)
  │   └── item.toMsg() → w.WriteMsg()
  │
  ├── [캐시 히트 + 만료 (stale)]
  │   ├── [verifyStale] → doRefresh() 시도
  │   └── [!verifyStale] → 0 TTL로 서빙 + go doPrefetch()
  │
  └── [캐시 미스]
      └── doRefresh() → Next.ServeDNS()
          (ResponseWriter가 응답을 캐시에 저장)
```

### 5.4 프리페치 알고리즘

```go
func (c *Cache) shouldPrefetch(i *item, now time.Time) bool {
    if c.prefetch <= 0 { return false }
    i.Update(c.duration, now)
    threshold := int(math.Ceil(float64(c.percentage) / 100 * float64(i.origTTL)))
    return i.Hits() >= c.prefetch && i.ttl(now) <= threshold
}
```

프리페치 조건:
1. 히트 수 >= `prefetch` 임계값 (캐시 윈도우 내)
2. 남은 TTL <= `percentage`% of 원래 TTL

예: TTL 300초, percentage 10% → TTL이 30초 이하일 때 프리페치 시작

## 6. Forward 플러그인

### 6.1 Forward 구조체

**소스코드 경로**: `plugin/forward/forward.go`

```go
type Forward struct {
    concurrent int64              // atomic 카운터 (구조체 정렬 위해 첫 필드)

    proxies    []*proxyPkg.Proxy  // 업스트림 프록시 목록
    p          Policy             // 선택 정책 (random이 기본)
    hcInterval time.Duration      // 헬스체크 간격 (500ms)

    from       string             // 매칭 도메인
    ignored    []string           // 제외 도메인

    maxfails       uint32         // 실패 임계값 (기본 2)
    expire         time.Duration  // 연결 만료 (기본 10초)
    maxConcurrent  int64          // 최대 동시 쿼리 (0=무제한)

    Next plugin.Handler
}
```

### 6.2 프록시 선택 정책

Forward 플러그인은 여러 업스트림 서버 중 하나를 선택하기 위해 Policy 인터페이스를 사용한다.

| 정책 | 동작 |
|------|------|
| `random` | 프록시 목록을 랜덤 셔플 (기본) |
| `round_robin` | 순차적 회전 |
| `sequential` | 순서대로 (첫 번째 우선) |

### 6.3 ServeDNS 핵심 로직

```
Forward.ServeDNS(ctx, w, r)
  │
  ├── match(state) 확인 (from 도메인 매칭)
  │   └── [불일치] → NextOrFailure() → 다음 플러그인
  │
  ├── maxConcurrent 확인
  │   └── [초과] → RcodeRefused, ErrLimitExceeded
  │
  ├── list = f.List() (정책에 따른 프록시 목록)
  │
  └── loop (deadline 5초 이내)
      ├── proxy.Down(maxfails) 확인
      │   └── [다운] → 다음 프록시 시도
      │       └── [모든 프록시 다운]
      │           ├── [failfast] → SERVFAIL
      │           └── [else] → 랜덤 선택 (헬스체크 무시)
      │
      ├── proxy.Connect(ctx, state, opts)
      │   └── [ErrCachedClosed] → TCP 재연결 시도
      │   └── [Truncated + PreferUDP] → ForceTCP로 재시도
      │
      ├── state.Match(ret) 검증
      │   └── [불일치] → FORMERR 응답
      │
      ├── failoverRcodes 확인
      │   └── [매칭] → 다음 프록시 시도
      │
      └── w.WriteMsg(ret) → 성공 반환
```

### 6.4 연결 캐싱

Forward 플러그인은 업스트림 연결을 캐싱하여 성능을 향상시킨다. 패키지 주석에 따르면 "50% 더 빠르다"고 한다.

```
// Package forward implements a forwarding proxy. It caches an upstream net.Conn
// for some time, so if the same client returns the upstream's Conn will be
// precached. Depending on how you benchmark this looks to be 50% faster than
// just opening a new connection for every client.
```

## 7. Kubernetes 플러그인

### 7.1 Kubernetes 구조체

**소스코드 경로**: `plugin/kubernetes/kubernetes.go`

```go
type Kubernetes struct {
    Next             plugin.Handler
    Zones            []string          // 서빙 Zone (예: ["cluster.local."])
    Upstream         Upstreamer        // CNAME 해석용
    APIServerList    []string          // API 서버 주소
    APIConn          dnsController     // K8s API 커넥터 (Informer)
    Namespaces       map[string]struct{}  // 노출 네임스페이스
    podMode          string            // "disabled", "verified", "insecure"
    endpointNameMode bool              // 엔드포인트 이름 모드
    Fall             fall.F            // Fallthrough 설정
    ttl              uint32            // 기본 TTL (5초)
    primaryZoneIndex int               // 주 Zone 인덱스
}
```

### 7.2 DNS 스키마 매핑

```
K8s 서비스 DNS 스키마 (RFC 준수):

일반 서비스:
  <service>.<namespace>.svc.<zone>
  → ClusterIP의 A/AAAA 레코드

Headless 서비스:
  <service>.<namespace>.svc.<zone>
  → 각 Pod IP의 A/AAAA 레코드

SRV 레코드:
  _<port-name>._<proto>.<service>.<namespace>.svc.<zone>
  → SRV 레코드 (priority, weight, port, target)

Pod 레코드:
  <ip-with-dashes>.<namespace>.pod.<zone>
  → Pod IP의 A 레코드
```

### 7.3 dnsController 인터페이스

Kubernetes 플러그인은 `dnsController` 인터페이스를 통해 K8s API 데이터에 접근한다. 내부적으로 client-go의 Informer를 사용하여 로컬 캐시를 유지한다.

```
┌─────────────┐     Watch      ┌──────────────┐
│ K8s API     │◄──────────────│ Informer/     │
│ Server      │  Add/Update/  │ Reflector     │
│             │───Delete────>│              │
└─────────────┘               └──────┬───────┘
                                     │
                                     v
                              ┌──────────────┐
                              │ 로컬 캐시      │
                              │ (Store)       │
                              │ ├── Services  │
                              │ ├── Endpoints │
                              │ ├── Pods      │
                              │ └── Namespaces│
                              └──────┬───────┘
                                     │
                                     v
                              ┌──────────────┐
                              │dnsController │
                              │.Services()   │
                              │.EpIndex()    │
                              │.PodIndex()   │
                              └──────────────┘
```

### 7.4 Services() 메서드

```go
func (k *Kubernetes) Services(ctx context.Context, state request.Request,
    exact bool, opt plugin.Options) (svcs []msg.Service, err error) {
    // 1. parseRequest로 QNAME 파싱
    // 2. namespace 노출 확인
    // 3. serviceName으로 Service 검색
    // 4. ClusterIP/EndpointIP → msg.Service 변환
}
```

## 8. LoadBalance 플러그인

### 8.1 동작 원리

**소스코드 경로**: `plugin/loadbalance/loadbalance.go`

LoadBalance 플러그인은 DNS 응답의 A, AAAA, MX 레코드를 셔플하여 클라이언트 측 로드밸런싱을 구현한다.

```go
type LoadBalanceResponseWriter struct {
    dns.ResponseWriter
    shuffle func(*dns.Msg) *dns.Msg
}

func (r *LoadBalanceResponseWriter) WriteMsg(res *dns.Msg) error {
    if res.Rcode != dns.RcodeSuccess { return r.ResponseWriter.WriteMsg(res) }
    return r.ResponseWriter.WriteMsg(r.shuffle(res))
}
```

### 8.2 Round-Robin 셔플

```go
func roundRobin(in []dns.RR) []dns.RR {
    cname := []dns.RR{}
    address := []dns.RR{}  // A, AAAA
    mx := []dns.RR{}
    rest := []dns.RR{}

    // 타입별 분리
    for _, r := range in { ... }

    // address와 mx만 셔플
    roundRobinShuffle(address)
    roundRobinShuffle(mx)

    // CNAME → rest → address → mx 순서로 재조합
    out := append(cname, rest...)
    out = append(out, address...)
    out = append(out, mx...)
    return out
}
```

CNAME은 반드시 먼저 와야 하므로 셔플하지 않는다.

## 9. 컴포넌트 상호작용 요약

```
┌──────────────────────────────────────────────────────────────┐
│                     Server.ServeDNS()                        │
│                                                              │
│  ┌─────────┐  ┌─────────┐  ┌───────────┐  ┌──────────────┐ │
│  │  log    │→│ cache   │→│loadbalance│→│  kubernetes │ │
│  │ (로깅)  │  │ (캐싱)  │  │ (셔플)    │  │ (K8s 조회)  │ │
│  └─────────┘  └────┬────┘  └───────────┘  └──────┬───────┘ │
│                    │                              │         │
│               ┌────┴────┐                    ┌────┴───┐    │
│               │ pcache  │                    │Informer│    │
│               │ ncache  │                    │ Cache  │    │
│               └─────────┘                    └────────┘    │
│                                                              │
│  또는:                                                       │
│  ┌─────────┐  ┌─────────┐  ┌──────────┐                    │
│  │  log    │→│ cache   │→│ forward  │                    │
│  │ (로깅)  │  │ (캐싱)  │  │(포워딩)  │                    │
│  └─────────┘  └─────────┘  └────┬─────┘                    │
│                                  │                          │
│                           ┌──────┴──────┐                   │
│                           │  proxies[]  │                   │
│                           │ (업스트림)   │                   │
│                           └─────────────┘                   │
└──────────────────────────────────────────────────────────────┘
```
