# 08. DNS 서버 엔진

## 개요

CoreDNS의 DNS 서버 엔진은 Caddy 프레임워크의 서버 인터페이스를 구현하면서, DNS 프로토콜에 특화된 요청 라우팅, 존 매칭, 에러 처리, 그리고 다양한 프로토콜(UDP/TCP/TLS/QUIC/gRPC/HTTPS) 지원을 제공한다.

이 문서에서는 `core/dnsserver/server.go`의 `Server` 구조체를 중심으로 DNS 요청의 전체 처리 흐름을 소스코드 수준에서 분석한다.

---

## 1. Server 구조체

```
소스 위치: core/dnsserver/server.go:36-59
```

```go
type Server struct {
    Addr         string        // 리스닝 주소
    IdleTimeout  time.Duration // TCP 유휴 타임아웃
    ReadTimeout  time.Duration // TCP 읽기 타임아웃
    WriteTimeout time.Duration // TCP 쓰기 타임아웃

    connPolicy proxyproto.ConnPolicyFunc // Proxy Protocol 연결 정책

    server [2]*dns.Server // [0]=TCP, [1]=UDP
    m      sync.Mutex     // 서버 보호용 뮤텍스

    zones        map[string][]*Config // 존별 설정 (존 이름 → Config 목록)
    graceTimeout time.Duration        // 그레이스풀 종료 최대 대기 시간
    trace        trace.Trace          // 트레이싱 플러그인
    debug        bool                 // panic recovery 비활성화
    stacktrace   bool                 // 복구 시 스택트레이스 포함
    classChaos   bool                 // CH 클래스 쿼리 허용

    tsigSecret map[string]string     // TSIG 인증 시크릿

    stopOnce sync.Once // Stop 멱등성 보장
    stopErr  error
}
```

### Server 필드 상세 설명

| 필드 | 타입 | 기본값 | 설명 |
|------|------|--------|------|
| `Addr` | string | - | "dns://0.0.0.0:53" 형식의 리스닝 주소 |
| `IdleTimeout` | Duration | 10초 | TCP 연결 유휴 타임아웃 |
| `ReadTimeout` | Duration | 3초 | TCP 읽기 타임아웃 |
| `WriteTimeout` | Duration | 5초 | TCP 쓰기 타임아웃 |
| `server[2]` | [2]*dns.Server | - | TCP(인덱스 0)와 UDP(인덱스 1) 서버 |
| `zones` | map[string][]*Config | - | 존 이름을 키로 하는 설정 목록 |
| `graceTimeout` | Duration | 5초 | 그레이스풀 종료 최대 대기 |
| `debug` | bool | false | true이면 panic recovery 비활성화 |
| `classChaos` | bool | false | true이면 CH 클래스 쿼리 허용 |

### TCP/UDP 듀얼 서버 설계

```go
const (
    tcp = 0
    udp = 1
    tcpMaxQueries = -1  // 무제한
)
```

Server는 `[2]*dns.Server` 배열로 TCP와 UDP를 동시에 지원한다. `tcpMaxQueries = -1`은 TCP 연결당 쿼리 수를 무제한으로 설정한다.

---

## 2. NewServer: 서버 생성과 플러그인 체인 조립

```
소스 위치: core/dnsserver/server.go:68-141
```

```go
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

    for _, site := range group {
        // ...
    }
    return s, nil
}
```

### NewServer의 주요 작업

NewServer는 각 Config(존 설정)에 대해 다음 작업을 수행한다:

#### 2.1 디버그 모드 설정

```go
if site.Debug {
    s.debug = true
    log.D.Set()
}
s.stacktrace = site.Stacktrace
```

하나의 존이라도 Debug가 활성화되면 서버 전체가 디버그 모드로 전환된다.

#### 2.2 존 설정 매핑

```go
s.zones[site.Zone] = append(s.zones[site.Zone], site)
```

같은 존 이름에 여러 Config가 매핑될 수 있다 (View 기능 사용 시).

#### 2.3 타임아웃 설정

```go
if site.ReadTimeout != 0 {
    s.ReadTimeout = site.ReadTimeout
}
if site.WriteTimeout != 0 {
    s.WriteTimeout = site.WriteTimeout
}
if site.IdleTimeout != 0 {
    s.IdleTimeout = site.IdleTimeout
}
```

Config에서 설정된 타임아웃이 서버의 기본값을 덮어쓴다.

#### 2.4 TSIG 시크릿 복사

```go
maps.Copy(s.tsigSecret, site.TsigSecret)
```

모든 존의 TSIG 시크릿을 서버 레벨로 병합한다.

#### 2.5 플러그인 체인 조립 (핵심)

```go
var stack plugin.Handler
for i := len(site.Plugin) - 1; i >= 0; i-- {
    stack = site.Plugin[i](stack)
    site.registerHandler(stack)

    if mdc, ok := stack.(MetadataCollector); ok {
        site.metaCollector = mdc
    }
    if s.trace == nil && stack.Name() == "trace" {
        if t, ok := stack.(trace.Trace); ok {
            s.trace = t
        }
    }
    if _, ok := EnableChaos[stack.Name()]; ok {
        s.classChaos = true
    }
}
site.pluginChain = stack
```

역순 루프로 플러그인 체인을 조립하면서 동시에 수행하는 작업:

| 작업 | 설명 |
|------|------|
| `registerHandler(stack)` | 핸들러 레지스트리에 등록 (플러그인간 상호 참조용) |
| MetadataCollector 감지 | metadata 플러그인 인스턴스 저장 |
| trace 감지 | 서버 전체 트레이서 설정 |
| EnableChaos 감지 | CH 클래스 쿼리 허용 여부 |

#### 2.6 MetadataCollector 인터페이스

```
소스 위치: core/dnsserver/server.go:62-64
```

```go
type MetadataCollector interface {
    Collect(context.Context, request.Request) context.Context
}
```

metadata 플러그인이 이 인터페이스를 구현한다. ServeDNS에서 플러그인 체인 실행 전에 메타데이터를 수집하여 context에 추가한다.

#### 2.7 EnableChaos 맵

```
소스 위치: core/dnsserver/server.go:450-454
```

```go
var EnableChaos = map[string]struct{}{
    "chaos":   {},
    "forward": {},
    "proxy":   {},
}
```

기본적으로 CoreDNS는 CH(Chaos) 클래스 쿼리를 차단한다. 위 플러그인이 로드되면 CH 클래스 쿼리가 허용된다.

---

## 3. ServeDNS: 요청 처리의 핵심

```
소스 위치: core/dnsserver/server.go:251-374
```

ServeDNS는 DNS 요청의 진입점이다. 요청이 도착하면 다음 단계를 순서대로 처리한다.

### 3.1 전체 흐름 다이어그램

```
ServeDNS(ctx, w, r)
    │
    ├── 1단계: 기본 검증
    │   ├── r == nil 또는 len(r.Question) == 0 → SERVFAIL
    │   └── CH 클래스 차단 (classChaos==false && QClass!=INET) → REFUSED
    │
    ├── 2단계: panic recovery 설정 (debug==false일 때)
    │
    ├── 3단계: EDNS 버전 검증
    │   └── edns.Version(r) 실패 → BADVERS 응답 직접 전송
    │
    ├── 4단계: ScrubWriter 래핑
    │   └── w = request.NewScrubWriter(r, w)
    │
    ├── 5단계: Zone 매칭 (longest suffix match)
    │   └── 매칭된 Config의 pluginChain.ServeDNS() 호출
    │
    ├── 6단계: DS 레코드 특수 처리
    │
    ├── 7단계: 와일드카드 매칭 (루트 존 폴백)
    │
    └── 8단계: 매칭 실패 → REFUSED
```

### 3.2 Step 1: 기본 검증

```go
if r == nil || len(r.Question) == 0 {
    errorAndMetricsFunc(s.Addr, w, r, dns.RcodeServerFailure)
    return
}
```

Question 섹션이 없는 비정상 요청은 즉시 SERVFAIL로 거부한다.

```go
if !s.classChaos && r.Question[0].Qclass != dns.ClassINET {
    errorAndMetricsFunc(s.Addr, w, r, dns.RcodeRefused)
    return
}
```

CH 클래스 쿼리는 chaos/forward/proxy 플러그인이 로드되지 않으면 REFUSED로 거부한다. 이는 보안을 위한 기본 정책이다.

### 3.3 Step 2: Panic Recovery

```go
if !s.debug {
    defer func() {
        if rec := recover(); rec != nil {
            if s.stacktrace {
                log.Errorf("Recovered from panic in server: %q %v\n%s",
                    s.Addr, rec, string(debug.Stack()))
            } else {
                log.Errorf("Recovered from panic in server: %q %v", s.Addr, rec)
            }
            vars.Panic.Inc()
            errorAndMetricsFunc(s.Addr, w, r, dns.RcodeServerFailure)
        }
    }()
}
```

프로덕션 모드(debug==false)에서는 플러그인의 panic이 서버를 크래시시키지 않도록 recover한다.

| 옵션 | 동작 |
|------|------|
| `debug = false, stacktrace = false` | panic 복구, 에러 메시지만 로깅 |
| `debug = false, stacktrace = true` | panic 복구, 스택트레이스 포함 로깅 |
| `debug = true` | panic 복구 없음 (디버깅 시 crash 유도) |

`vars.Panic.Inc()`로 Prometheus 메트릭에 panic 횟수를 기록한다.

### 3.4 Step 3: EDNS 버전 검증

```go
if m, err := edns.Version(r); err != nil {
    w.WriteMsg(m)
    return
}
```

EDNS0가 아닌 EDNS 버전(EDNS1 등)을 사용하는 요청은 즉시 BADVERS 응답을 반환한다.

### 3.5 Step 4: ScrubWriter 래핑

```go
w = request.NewScrubWriter(r, w)
```

```
소스 위치: request/writer.go:6-21
```

```go
type ScrubWriter struct {
    dns.ResponseWriter
    req *dns.Msg
}

func (s *ScrubWriter) WriteMsg(m *dns.Msg) error {
    state := Request{Req: s.req, W: s.ResponseWriter}
    state.SizeAndDo(m)
    state.Scrub(m)
    return s.ResponseWriter.WriteMsg(m)
}
```

ScrubWriter는 응답 메시지를 클라이언트의 버퍼 크기에 맞게 자동으로 잘라준다.

| 메서드 | 역할 |
|--------|------|
| `SizeAndDo(m)` | 클라이언트의 EDNS UDP 버퍼 크기와 DO 비트 반영 |
| `Scrub(m)` | 응답이 버퍼보다 크면 레코드를 제거하여 맞춤 |

이 래핑은 **모든 플러그인에 투명하게 적용**된다. 플러그인은 크기 제한을 신경 쓸 필요 없이 응답을 작성하면 된다.

### 3.6 Step 5: Zone 매칭 (Longest Suffix Match)

```go
q := strings.ToLower(r.Question[0].Name)
var (
    off       int
    end       bool
    dshandler *Config
)

for {
    if z, ok := s.zones[q[off:]]; ok {
        for _, h := range z {
            if h.pluginChain == nil {
                errorAndMetricsFunc(s.Addr, w, r, dns.RcodeRefused)
                return
            }
            if h.metaCollector != nil {
                ctx = h.metaCollector.Collect(ctx, request.Request{Req: r, W: w})
            }
            if passAllFilterFuncs(ctx, h.FilterFuncs, &request.Request{Req: r, W: w}) {
                if h.ViewName != "" {
                    ctx = context.WithValue(ctx, ViewKey{}, h.ViewName)
                }
                if r.Question[0].Qtype != dns.TypeDS {
                    rcode, _ := h.pluginChain.ServeDNS(ctx, w, r)
                    if !plugin.ClientWrite(rcode) {
                        errorFunc(s.Addr, w, r, rcode)
                    }
                    return
                }
                dshandler = h
            }
        }
    }
    off, end = dns.NextLabel(q, off)
    if end {
        break
    }
}
```

### Longest Suffix Match 알고리즘 상세

이 알고리즘은 쿼리 도메인 이름에서 **가장 구체적인(가장 긴) 존을 먼저 찾는다**.

```
예시: 쿼리 = "web.staging.example.com."

s.zones = {
    "example.com.":  [config_A],
    "staging.example.com.": [config_B],
    ".":             [config_C],
}

검색 순서:
  1차: "web.staging.example.com." → 없음
  2차: "staging.example.com."    → config_B 발견! → 이 체인으로 처리
  (만약 없으면)
  3차: "example.com."            → config_A
  4차: "com."                    → 없음
  5차: "."                       → config_C
```

`dns.NextLabel(q, off)` 함수는 도메인의 다음 레이블 위치를 반환한다:
- `"web.staging.example.com."` → off=4 ("staging.example.com."), off=12 ("example.com."), off=20 ("com."), off=24 (".")

### 3.7 FilterFuncs와 View 지원

```go
func passAllFilterFuncs(ctx context.Context, filterFuncs []FilterFunc, req *request.Request) bool {
    for _, ff := range filterFuncs {
        if !ff(ctx, req) {
            return false
        }
    }
    return true
}
```

같은 존에 여러 Config가 존재할 때, FilterFuncs를 통해 요청을 적절한 Config로 라우팅한다.

```
소스 위치: core/dnsserver/config.go:120
```

```go
type FilterFunc func(context.Context, *request.Request) bool
```

View 기능의 동작:

```
서버 블록 1: example.com { view internal { ... } forward . 10.0.0.1 }
서버 블록 2: example.com { forward . 8.8.8.8 }

→ 같은 존 "example.com."에 대해:
  - config_1: FilterFuncs = [internal.Filter]
  - config_2: FilterFuncs = [] (필터 없음)

요청 도착 시:
  1. config_1의 FilterFuncs 평가 → 내부 네트워크면 true → 이 체인 사용
  2. config_1 통과 못하면 config_2 시도 → 필터 없으므로 항상 통과
```

### 3.8 DS 레코드 특수 처리

```go
if r.Question[0].Qtype != dns.TypeDS {
    rcode, _ := h.pluginChain.ServeDNS(ctx, w, r)
    if !plugin.ClientWrite(rcode) {
        errorFunc(s.Addr, w, r, rcode)
    }
    return
}
// DS 쿼리: 핸들러를 저장하고 부모 존 검색 계속
dshandler = h
```

DS(Delegation Signer) 레코드는 **부모 존**에서 서비스해야 한다. 자식 존에서 매칭되더라도, 부모 존이 있는지 계속 검색한다.

```go
if r.Question[0].Qtype == dns.TypeDS && dshandler != nil && dshandler.pluginChain != nil {
    rcode, _ := dshandler.pluginChain.ServeDNS(ctx, w, r)
    if !plugin.ClientWrite(rcode) {
        errorFunc(s.Addr, w, r, rcode)
    }
    return
}
```

부모 존이 없으면 자식 존의 핸들러로 폴백한다.

### 3.9 와일드카드 매칭 (루트 존 폴백)

```go
if z, ok := s.zones["."]; ok {
    for _, h := range z {
        if h.pluginChain == nil {
            continue
        }
        if h.metaCollector != nil {
            ctx = h.metaCollector.Collect(ctx, request.Request{Req: r, W: w})
        }
        if passAllFilterFuncs(ctx, h.FilterFuncs, &request.Request{Req: r, W: w}) {
            if h.ViewName != "" {
                ctx = context.WithValue(ctx, ViewKey{}, h.ViewName)
            }
            rcode, _ := h.pluginChain.ServeDNS(ctx, w, r)
            if !plugin.ClientWrite(rcode) {
                errorFunc(s.Addr, w, r, rcode)
            }
            return
        }
    }
}
```

일반 존 매칭에서 아무것도 찾지 못한 경우, 루트 존(".")을 **마지막 수단**으로 시도한다.

### 3.10 최종 폴백: REFUSED

```go
errorAndMetricsFunc(s.Addr, w, r, dns.RcodeRefused)
```

모든 매칭이 실패하면 REFUSED를 반환한다.

---

## 4. Context 키

```
소스 위치: core/dnsserver/server.go:438-447
```

```go
type (
    Key     struct{} // 현재 서버 인스턴스
    LoopKey struct{} // 루프 감지 카운터
    ViewKey struct{} // 현재 뷰 이름
)
```

| 키 | 값 타입 | 용도 |
|----|---------|------|
| `Key{}` | `*Server` | 현재 요청을 처리하는 서버 인스턴스 참조 |
| `LoopKey{}` | `int` | 서버 내 루프 감지 (loop 플러그인이 사용) |
| `ViewKey{}` | `string` | 현재 적용된 뷰 이름 (메트릭 레이블링) |

### Context 설정 위치

```go
// Serve 메서드 (TCP)
ctx := context.WithValue(context.Background(), Key{}, s)
ctx = context.WithValue(ctx, LoopKey{}, 0)
s.ServeDNS(ctx, w, r)

// ServePacket 메서드 (UDP)
ctx := context.WithValue(context.Background(), Key{}, s)
ctx = context.WithValue(ctx, LoopKey{}, 0)
s.ServeDNS(ctx, w, r)
```

각 요청은 새로운 Background context에서 시작하며, 서버 참조와 루프 카운터 0으로 초기화된다.

---

## 5. Listen/ListenPacket: 리스닝 설정

### 5.1 TCP 리스닝

```
소스 위치: core/dnsserver/server.go:186-195
```

```go
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
```

- `reuseport.Listen`: SO_REUSEPORT 옵션으로 여러 소켓이 같은 포트를 공유
- 주소에서 `dns://` 접두사 제거
- Proxy Protocol이 설정되면 리스너를 proxyproto 리스너로 래핑

### 5.2 UDP 리스닝

```
소스 위치: core/dnsserver/server.go:203-212
```

```go
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

TCP와 동일한 패턴으로 UDP PacketConn을 생성한다.

---

## 6. Serve/ServePacket: 서버 시작

### 6.1 TCP 서버 시작

```
소스 위치: core/dnsserver/server.go:148-169
```

```go
func (s *Server) Serve(l net.Listener) error {
    s.m.Lock()
    s.server[tcp] = &dns.Server{
        Listener:      l,
        Net:           "tcp",
        TsigSecret:    s.tsigSecret,
        MaxTCPQueries: tcpMaxQueries,  // -1 (무제한)
        ReadTimeout:   s.ReadTimeout,
        WriteTimeout:  s.WriteTimeout,
        IdleTimeout: func() time.Duration {
            return s.IdleTimeout
        },
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

핵심 포인트:
- `dns.Server`의 `Handler`에 CoreDNS의 `ServeDNS`를 연결하는 어댑터
- 각 요청마다 새 context 생성 (Key, LoopKey 포함)
- `IdleTimeout`이 함수로 제공됨 (런타임 변경 가능)

### 6.2 UDP 서버 시작

```
소스 위치: core/dnsserver/server.go:173-183
```

```go
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

UDP 서버는 TCP보다 단순하다. 타임아웃 설정이 없다 (UDP는 커넥션리스).

---

## 7. Stop: 그레이스풀 종료

```
소스 위치: core/dnsserver/server.go:219-242
```

```go
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

### 종료 흐름:

```
Stop() 호출
  │
  ├── stopOnce.Do() → 멱등성 보장 (동시 호출 시 한 번만 실행)
  │
  ├── graceTimeout 타임아웃 context 생성 (기본 5초)
  │
  ├── TCP 서버와 UDP 서버를 병렬로 ShutdownContext
  │   ├── wg.Go(tcp.ShutdownContext)
  │   └── wg.Go(udp.ShutdownContext)
  │
  ├── wg.Wait() → 모든 서버 종료 대기
  │
  └── 타임아웃 초과 시 ctx.Err() 반환
```

`sync.Once`를 사용하여 Stop이 **동시에 여러 번 호출되어도 안전**하다. 예를 들어 설정 리로드와 SIGTERM이 동시에 발생해도 한 번만 종료된다.

---

## 8. 에러 처리 함수

### 8.1 errorFunc

```
소스 위치: core/dnsserver/server.go:409-417
```

```go
func errorFunc(server string, w dns.ResponseWriter, r *dns.Msg, rc int) {
    state := request.Request{W: w, Req: r}
    answer := new(dns.Msg)
    answer.SetRcode(r, rc)
    state.SizeAndDo(answer)
    w.WriteMsg(answer)
}
```

플러그인 체인이 에러 rcode를 반환했을 때 (ClientWrite == false) 호출된다. 메트릭 기록 없이 에러 응답만 전송한다.

### 8.2 errorAndMetricsFunc

```
소스 위치: core/dnsserver/server.go:419-428
```

```go
func errorAndMetricsFunc(server string, w dns.ResponseWriter, r *dns.Msg, rc int) {
    state := request.Request{W: w, Req: r}
    answer := new(dns.Msg)
    answer.SetRcode(r, rc)
    state.SizeAndDo(answer)
    vars.Report(server, state, vars.Dropped, "", rcode.ToString(rc), "", answer.Len(), time.Now())
    w.WriteMsg(answer)
}
```

서버 레벨에서 요청을 거부할 때 호출된다 (존 매칭 실패, 잘못된 요청 등). `vars.Report`로 Prometheus 메트릭에 기록한다.

### 두 함수의 사용 구분:

| 함수 | 사용 시점 | 메트릭 기록 |
|------|----------|------------|
| `errorFunc` | 플러그인 체인이 에러 rcode 반환 시 | 없음 (플러그인이 자체 메트릭 기록) |
| `errorAndMetricsFunc` | 서버가 직접 요청 거부 시 | `vars.Dropped`로 기록 |

---

## 9. Tracer 접근

```
소스 위치: core/dnsserver/server.go:400-406
```

```go
func (s *Server) Tracer() ot.Tracer {
    if s.trace == nil {
        return nil
    }
    return s.trace.Tracer()
}
```

trace 플러그인이 로드되어 있으면 OpenTracing 트레이서를 반환한다. 플러그인들은 이 트레이서를 사용하여 분산 트레이싱 span을 생성한다.

---

## 10. OnStartupComplete: 시작 로그

```
소스 위치: core/dnsserver/server.go:388-397
```

```go
func (s *Server) OnStartupComplete() {
    if Quiet {
        return
    }
    out := startUpZones("", s.Addr, s.zones)
    if out != "" {
        fmt.Print(out)
    }
}
```

서버 시작 완료 시 호출되어 서비스 중인 존 목록을 출력한다.

```
소스 위치: core/dnsserver/onstartup.go:25-57
```

```go
func startUpZones(protocol, addr string, zones map[string][]*Config) string {
    keys := make([]string, len(zones))
    // 존 이름 정렬
    sort.Strings(keys)
    var sb strings.Builder
    for _, zone := range keys {
        if !checkZoneSyntax(zone) {
            fmt.Fprintf(&sb, "Warning: Domain %q does not follow RFC1035 preferred syntax\n", zone)
        }
        _, ip, port, err := SplitProtocolHostPort(addr)
        if ip == "" {
            fmt.Fprintln(&sb, protocol+zone+":"+port)
        } else {
            fmt.Fprintln(&sb, protocol+zone+":"+port+" on "+ip)
        }
    }
    return sb.String()
}
```

출력 예시:
```
example.com.:53
staging.example.com.:53
.:53
```

---

## 11. 서버 확장: 프로토콜별 서버 타입

CoreDNS는 기본 `Server`를 **임베딩(embedding)**하여 다양한 프로토콜을 지원한다.

### 11.1 ServerTLS (DNS-over-TLS)

```
소스 위치: core/dnsserver/server_tls.go:19-21
```

```go
type ServerTLS struct {
    *Server
    tlsConfig *tls.Config
}
```

- TCP만 사용 (ServePacket은 no-op)
- `tls.NewListener`로 TCP 리스너를 TLS 리스너로 래핑
- `Net: "tcp-tls"`

### 11.2 ServerQUIC (DNS-over-QUIC)

```
소스 위치: core/dnsserver/server_quic.go
```

- QUIC 프로토콜 사용 (HTTP/3 기반 아님, 순수 DNS-over-QUIC)
- `MaxQUICStreams`, `MaxQUICWorkerPoolSize` 설정 지원
- UDP 기반이지만 연결 지향적

### 11.3 ServerGRPC (DNS-over-gRPC)

```
소스 위치: core/dnsserver/server_grpc.go
```

- gRPC 프로토콜 사용
- `MaxGRPCStreams`, `MaxGRPCConnections` 설정 지원
- 서비스 메시 환경에서 유용

### 11.4 ServerHTTPS (DNS-over-HTTPS, DoH)

```
소스 위치: core/dnsserver/server_https.go
```

- HTTP/2 기반 DNS-over-HTTPS
- `MaxHTTPSConnections` 설정 지원

### 11.5 ServerHTTPS3 (DNS-over-HTTPS/3)

```
소스 위치: core/dnsserver/server_https3.go
```

- HTTP/3(QUIC) 기반 DNS-over-HTTPS
- `MaxHTTPS3Streams` 설정 지원

### 공통 구조:

```
┌────────────────────────────────────────┐
│  Server (기본, UDP+TCP)                │
│  ├── zones, pluginChain, ServeDNS     │
│  └── 모든 요청 처리 로직              │
├────────────────────────────────────────┤
│  ServerTLS     ← *Server 임베딩       │
│  ServerQUIC    ← *Server 임베딩       │
│  ServerGRPC    ← *Server 임베딩       │
│  ServerHTTPS   ← *Server 임베딩       │
│  ServerHTTPS3  ← *Server 임베딩       │
│                                        │
│  차이점: Listen/Serve 메서드만 오버라이드  │
│  ServeDNS는 기본 Server 것을 그대로 사용  │
└────────────────────────────────────────┘
```

모든 프로토콜별 서버가 동일한 ServeDNS를 공유하므로, 플러그인 체인은 프로토콜에 무관하게 동작한다.

---

## 12. 서버 생성 (프로토콜별)

```
소스 위치: core/dnsserver/register.go:303-362
```

```go
func makeServersForGroup(addr string, group []*Config) ([]caddy.Server, error) {
    numSockets := 1
    if group[0].NumSockets > 0 {
        numSockets = group[0].NumSockets
    }

    var servers []caddy.Server
    for range numSockets {
        switch tr, _ := parse.Transport(addr); tr {
        case transport.DNS:
            s, err := NewServer(addr, group)
            // ...
        case transport.TLS:
            s, err := NewServerTLS(addr, group)
            // ...
        case transport.QUIC:
            s, err := NewServerQUIC(addr, group)
            // ...
        case transport.GRPC:
            s, err := NewServergRPC(addr, group)
            // ...
        case transport.HTTPS:
            s, err := NewServerHTTPS(addr, group)
            // ...
        case transport.HTTPS3:
            s, err := NewServerHTTPS3(addr, group)
            // ...
        }
    }
    return servers, nil
}
```

### NumSockets (다중 소켓)

`multisocket` 플러그인으로 설정된 `NumSockets` 값에 따라 같은 주소에 여러 서버 인스턴스를 생성한다. SO_REUSEPORT와 함께 사용하여 멀티코어 성능을 극대화한다.

---

## 13. Caddy 인터페이스 준수

```
소스 위치: core/dnsserver/server.go:144
```

```go
var _ caddy.GracefulServer = &Server{}
```

컴파일 타임에 Server가 `caddy.GracefulServer` 인터페이스를 구현하는지 확인한다.

### Server가 구현하는 Caddy 인터페이스:

| 인터페이스 | 메서드 | 설명 |
|-----------|--------|------|
| `caddy.TCPServer` | `Listen()`, `Serve()` | TCP 리스닝 |
| `caddy.UDPServer` | `ListenPacket()`, `ServePacket()` | UDP 리스닝 |
| `caddy.Stopper` | `Stop()` | 서버 종료 |
| `caddy.GracefulServer` | `Address()`, `WrapListener()` | 그레이스풀 종료 |

```go
func (s *Server) Address() string { return s.Addr }

func (s *Server) WrapListener(ln net.Listener) net.Listener {
    return ln  // 기본 구현은 래핑 없음
}
```

---

## 14. Config 구조체

```
소스 위치: core/dnsserver/config.go:18-117
```

Config는 하나의 서버 블록(존)에 대한 전체 설정을 담는 구조체이다.

```go
type Config struct {
    Zone        string           // 존 이름 ("example.com.")
    ListenHosts []string         // 바인드 주소 목록
    Port        string           // 리스닝 포트
    NumSockets  int              // 소켓 수 (multisocket)
    Root        string           // 베이스 디렉토리
    Debug       bool             // 디버그 모드
    Stacktrace  bool             // 스택트레이스 활성화
    Transport   string           // 프로토콜 ("dns", "tls", "grpc" 등)

    HTTPRequestValidateFunc func(*http.Request) bool

    FilterFuncs []FilterFunc     // 뷰 필터 함수 목록
    ViewName    string           // 뷰 이름
    TLSConfig   *tls.Config      // TLS 설정
    MaxQUICStreams *int           // QUIC 최대 스트림
    MaxQUICWorkerPoolSize *int   // QUIC 워커 풀 크기
    ProxyProtoConnPolicy proxyproto.ConnPolicyFunc

    MaxGRPCStreams     *int      // gRPC 최대 스트림
    MaxGRPCConnections *int     // gRPC 최대 연결 수
    MaxHTTPSConnections *int    // HTTPS 최대 연결 수
    MaxHTTPS3Streams   *int     // HTTPS3 최대 스트림

    ReadTimeout  time.Duration   // 읽기 타임아웃
    WriteTimeout time.Duration   // 쓰기 타임아웃
    IdleTimeout  time.Duration   // 유휴 타임아웃
    TsigSecret   map[string]string // TSIG 시크릿

    Plugin      []plugin.Plugin  // 플러그인 팩토리 목록
    pluginChain plugin.Handler   // 컴파일된 플러그인 체인
    registry    map[string]plugin.Handler // 핸들러 레지스트리

    firstConfigInBlock *Config   // 서버 블록의 첫 번째 Config
    metaCollector      MetadataCollector // 메타데이터 수집기
}
```

### GetConfig: 설정 접근

```
소스 위치: core/dnsserver/config.go:129-140
```

```go
func GetConfig(c *caddy.Controller) *Config {
    ctx := c.Context().(*dnsContext)
    key := keyForConfig(c.ServerBlockIndex, c.ServerBlockKeyIndex)
    if cfg, ok := ctx.keysToConfigs[key]; ok {
        return cfg
    }
    ctx.saveConfig(key, &Config{ListenHosts: []string{""}})
    return GetConfig(c)
}
```

플러그인의 setup 함수에서 현재 서버 블록의 Config를 가져오는 데 사용한다.

---

## 15. 요청 처리 전체 시퀀스

```
┌─────────┐     ┌───────────┐     ┌──────────────┐
│ Client  │────▶│ dns.Server│────▶│ Server       │
│         │     │ (miekg)   │     │ .ServeDNS()  │
└─────────┘     └───────────┘     └──────┬───────┘
                                          │
                    ┌─────────────────────┘
                    │
                    ▼
        ┌───────────────────────────┐
        │ 1. Question 검증           │
        │ 2. Panic Recovery 설정     │
        │ 3. CH 클래스 검증          │
        │ 4. EDNS 버전 검증          │
        │ 5. ScrubWriter 래핑        │
        └──────────┬────────────────┘
                   │
                   ▼
        ┌───────────────────────────┐
        │ Zone 매칭                  │
        │ (longest suffix match)     │
        │                            │
        │ 매칭 성공                   │
        │ ├── MetadataCollector.     │
        │ │   Collect()              │
        │ ├── FilterFuncs 평가       │
        │ └── ViewName 설정          │
        └──────────┬────────────────┘
                   │
                   ▼
        ┌───────────────────────────┐
        │ pluginChain.ServeDNS()    │
        │                            │
        │ Plugin A → B → C → ...    │
        │   (NextOrFailure로 연결)   │
        └──────────┬────────────────┘
                   │
                   ▼
        ┌───────────────────────────┐
        │ rcode 판정                 │
        │ ClientWrite(rcode)?        │
        │ ├── true:  응답 이미 전송  │
        │ └── false: errorFunc 호출  │
        └───────────────────────────┘
```

---

## 16. 정리

CoreDNS 서버 엔진의 핵심 설계 원칙:

| 원칙 | 구현 |
|------|------|
| **프로토콜 무관** | Server 임베딩으로 TCP/UDP/TLS/QUIC/gRPC/HTTPS 지원 |
| **존 기반 라우팅** | longest suffix match + FilterFuncs로 정밀 라우팅 |
| **안전성** | panic recovery, EDNS 검증, ScrubWriter |
| **관측성** | Context 키, 메트릭 기록, 트레이싱 통합 |
| **그레이스풀** | sync.Once 기반 멱등 종료, 병렬 서버 셧다운 |
| **확장성** | 플러그인 체인으로 모든 기능 확장 가능 |

Server는 단순히 DNS 패킷을 수신하는 것이 아니라, 안전한 요청 검증, 정밀한 존 라우팅, 투명한 응답 크기 조정, 그리고 풍부한 관측성을 제공하는 완전한 DNS 서버 엔진이다.
