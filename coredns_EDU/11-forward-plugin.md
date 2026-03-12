# 11. Forward 플러그인 Deep-Dive

## 개요

CoreDNS의 Forward 플러그인은 DNS 쿼리를 업스트림 DNS 서버로 전달(프록시)하는 플러그인이다. 연결 캐싱, 헬스체크, 다양한 로드밸런싱 정책, 장애조치 등의 기능을 제공한다. Kubernetes 환경에서는 클러스터 외부 도메인 해석에 필수적으로 사용된다.

소스코드 경로: `plugin/forward/` 및 `plugin/pkg/proxy/`

---

## 1. 핵심 데이터 구조

### 1.1 Forward 구조체

`plugin/forward/forward.go:36-68`에 정의된 Forward 구조체는 플러그인의 중심이다.

```
// plugin/forward/forward.go:36-68
type Forward struct {
    concurrent int64 // atomic 카운터, 구조체 맨 앞에 위치 (정렬 보장)

    proxies    []*proxyPkg.Proxy
    p          Policy
    hcInterval time.Duration

    from    string
    ignored []string

    nextAlternateRcodes []int

    tlsConfig                  *tls.Config
    tlsServerName              string
    maxfails                   uint32
    expire                     time.Duration
    maxAge                     time.Duration
    maxIdleConns               int
    maxConcurrent              int64
    failfastUnhealthyUpstreams bool
    failoverRcodes             []int
    maxConnectAttempts         uint32

    opts proxyPkg.Options

    ErrLimitExceeded error

    tapPlugins []*dnstap.Dnstap

    Next plugin.Handler
}
```

주요 필드 분석:

| 필드 | 타입 | 기본값 | 역할 |
|------|------|--------|------|
| `concurrent` | `int64` | 0 | 현재 동시 처리 중인 쿼리 수 (atomic) |
| `proxies` | `[]*Proxy` | - | 업스트림 프록시 목록 |
| `p` | `Policy` | `random` | 업스트림 선택 정책 |
| `hcInterval` | `Duration` | 500ms | 헬스체크 주기 |
| `from` | `string` | `"."` | 이 Forward가 담당하는 Zone |
| `ignored` | `[]string` | nil | 제외할 도메인 목록 (except) |
| `maxfails` | `uint32` | 2 | 이 횟수 초과 시 업스트림 Down 판정 |
| `expire` | `Duration` | 10s | 유휴 연결 만료 시간 |
| `maxAge` | `Duration` | 0 | 연결 최대 수명 (0=무제한) |
| `maxIdleConns` | `int` | 0 | 프로토콜별 최대 유휴 연결 수 (0=무제한) |
| `maxConcurrent` | `int64` | 0 | 최대 동시 쿼리 수 (0=무제한) |
| `failoverRcodes` | `[]int` | nil | 장애조치 응답 코드 목록 |
| `maxConnectAttempts` | `uint32` | 0 | 요청당 최대 연결 시도 횟수 |

### 1.2 기본값 설정

```
// plugin/forward/forward.go:71-73
func New() *Forward {
    f := &Forward{
        maxfails: 2,
        tlsConfig: new(tls.Config),
        expire: defaultExpire,
        p: new(random),
        from: ".",
        hcInterval: hcInterval,
        opts: proxyPkg.Options{
            ForceTCP: false,
            PreferUDP: false,
            HCRecursionDesired: true,
            HCDomain: ".",
        },
    }
    return f
}
```

### 1.3 상수

```
// plugin/forward/forward.go:29-32
const (
    defaultExpire = 10 * time.Second
    hcInterval    = 500 * time.Millisecond
)

// plugin/forward/forward.go:310
var defaultTimeout = 5 * time.Second

// plugin/forward/setup.go:429
const max = 15 // 최대 업스트림 수
```

---

## 2. ServeDNS 흐름

### 2.1 전체 흐름

`plugin/forward/forward.go:102-257`의 ServeDNS 메서드는 Forward 플러그인의 핵심이다.

```
DNS 쿼리 수신
    │
    ▼
Zone 매칭 (f.match)
    │
    ├── 불일치 → 다음 플러그인
    │
    ▼ 일치
동시 요청 제한 체크 (maxConcurrent)
    │
    ├── 초과 → REFUSED + ErrLimitExceeded
    │
    ▼ 통과
프록시 목록 가져오기 (f.List())
    │
    ▼
업스트림 순회 루프 (deadline까지)
    │
    ├── proxy.Down(maxfails)?
    │   ├── Yes → 다음 프록시
    │   └── 모두 Down?
    │       ├── failfastUnhealthyUpstreams → break
    │       └── 아니면 → 무작위 프록시 선택
    │
    ▼
proxy.Connect(ctx, state, opts)
    │
    ├── 에러 → 헬스체크 트리거 + 재시도
    │
    ├── 응답 검증 (state.Match(ret))
    │   └── 불일치 → FORMERR 응답
    │
    ├── failoverRcodes 매칭?
    │   └── Yes → 다음 프록시로 재시도
    │
    ├── nextAlternateRcodes 매칭?
    │   └── Yes → 다음 Forward 플러그인으로
    │
    └── 성공 → w.WriteMsg(ret)
```

### 2.2 Zone 매칭

```
// plugin/forward/forward.go:103-106
state := request.Request{W: w, Req: r}
if !f.match(state) {
    return plugin.NextOrFailure(f.Name(), f.Next, ctx, w, r)
}
```

```
// plugin/forward/forward.go:259-265
func (f *Forward) match(state request.Request) bool {
    if !plugin.Name(f.from).Matches(state.Name()) || !f.isAllowedDomain(state.Name()) {
        return false
    }
    return true
}
```

`match`는 두 가지를 확인한다:
1. 쿼리 이름이 `from` zone에 속하는가
2. `except`로 제외된 도메인이 아닌가

```
// plugin/forward/forward.go:267-278
func (f *Forward) isAllowedDomain(name string) bool {
    if dns.Name(name) == dns.Name(f.from) {
        return true
    }
    for _, ignore := range f.ignored {
        if plugin.Name(ignore).Matches(name) {
            return false
        }
    }
    return true
}
```

### 2.3 동시 요청 제한

```
// plugin/forward/forward.go:108-115
if f.maxConcurrent > 0 {
    count := atomic.AddInt64(&(f.concurrent), 1)
    defer atomic.AddInt64(&(f.concurrent), -1)
    if count > f.maxConcurrent {
        maxConcurrentRejectCount.Add(1)
        return dns.RcodeRefused, f.ErrLimitExceeded
    }
}
```

**왜 atomic을 사용하는가?**

여러 고루틴이 동시에 ServeDNS를 호출하므로, 카운터를 안전하게 증감해야 한다. `atomic.AddInt64`는 잠금 없이 원자적 증감을 보장한다.

**왜 concurrent 필드가 구조체 맨 앞인가?**

Go에서 `sync/atomic`의 64비트 연산은 8바이트 정렬이 필요하다. 구조체의 첫 번째 필드는 항상 정렬이 보장된다. 이는 특히 32비트 아키텍처에서 중요하다.

### 2.4 프록시 선택과 순회

```
// plugin/forward/forward.go:122-151
list := f.List()
deadline := time.Now().Add(defaultTimeout)

for time.Now().Before(deadline) && ctx.Err() == nil && (f.maxConnectAttempts == 0 || connectAttempts < f.maxConnectAttempts) {
    if i >= len(list) {
        i = 0
        fails = 0
    }
    proxy := list[i]
    i++
    if proxy.Down(f.maxfails) {
        fails++
        if fails < len(f.proxies) {
            continue
        }
        healthcheckBrokenCount.Add(1)
        if f.failfastUnhealthyUpstreams {
            break
        }
        // 모든 업스트림 다운 → 무작위 선택
        r := new(random)
        proxy = r.List(f.proxies)[0]
    }
    // ...
}
```

루프 종료 조건:
1. **타임아웃**: 5초 데드라인 초과
2. **컨텍스트 취소**: 클라이언트가 연결을 끊음
3. **연결 시도 횟수 초과**: `maxConnectAttempts` 도달
4. **모든 프록시 다운 + failfast 모드**: `failfastUnhealthyUpstreams` 설정 시

**모든 프록시 다운 시 행동**:
- `failfastUnhealthyUpstreams`가 true면 즉시 SERVFAIL
- 아니면 헬스체크가 깨졌다고 가정하고 무작위 프록시를 선택하여 시도

### 2.5 응답 검증

```
// plugin/forward/forward.go:215-222
if !state.Match(ret) {
    debug.Hexdumpf(ret, "Wrong reply for id: %d, %s %d", ret.Id, state.QName(), state.QType())
    formerr := new(dns.Msg)
    formerr.SetRcode(state.Req, dns.RcodeFormatError)
    w.WriteMsg(formerr)
    return 0, nil
}
```

업스트림 응답이 원래 쿼리와 매칭되지 않으면 FORMERR(Format Error)를 반환한다.

### 2.6 Failover Rcodes

```
// plugin/forward/forward.go:225-237
tryNext := false
for _, failoverRcode := range f.failoverRcodes {
    if failoverRcode == ret.Rcode {
        if fails < len(f.proxies) {
            tryNext = true
        }
    }
}
if tryNext {
    fails++
    continue
}
```

특정 응답 코드(예: SERVFAIL, REFUSED)를 받으면 다음 업스트림으로 장애조치한다. 모든 업스트림을 시도한 후에도 매칭되면 마지막 응답을 반환한다.

### 2.7 Next Alternate Rcodes

```
// plugin/forward/forward.go:240-246
for _, alternateRcode := range f.nextAlternateRcodes {
    if alternateRcode == ret.Rcode && f.Next != nil {
        if _, ok := f.Next.(*Forward); ok {
            return plugin.NextOrFailure(f.Name(), f.Next, ctx, w, r)
        }
    }
}
```

`next` 설정으로 특정 응답 코드를 받으면 다음 Forward 플러그인으로 전달한다. 다음 핸들러가 Forward 타입인 경우에만 동작한다.

---

## 3. Policy 인터페이스

### 3.1 Policy 정의

```
// plugin/forward/policy.go:12-15
type Policy interface {
    List([]*proxy.Proxy) []*proxy.Proxy
    String() string
}
```

### 3.2 random 정책 (기본값)

```
// plugin/forward/policy.go:18-40
type random struct{}

func (r *random) List(p []*proxy.Proxy) []*proxy.Proxy {
    switch len(p) {
    case 1:
        return p
    case 2:
        if rn.Int()%2 == 0 {
            return []*proxy.Proxy{p[1], p[0]}  // 50% 확률로 swap
        }
        return p
    }
    perms := rn.Perm(len(p))
    rnd := make([]*proxy.Proxy, len(p))
    for i, p1 := range perms {
        rnd[i] = p[p1]
    }
    return rnd
}
```

**최적화 포인트**:
- 프록시가 1개일 때: 그대로 반환
- 프록시가 2개일 때: 50% 확률로 순서를 바꿈 (배열 할당 회피)
- 3개 이상: 전체 순열 생성

### 3.3 round_robin 정책

```
// plugin/forward/policy.go:43-59
type roundRobin struct {
    robin uint32
}

func (r *roundRobin) List(p []*proxy.Proxy) []*proxy.Proxy {
    poolLen := uint32(len(p))
    i := atomic.AddUint32(&r.robin, 1) % poolLen

    robin := make([]*proxy.Proxy, 0, len(p))
    robin = append(robin, p[i])
    robin = append(robin, p[:i]...)
    robin = append(robin, p[i+1:]...)
    return robin
}
```

`atomic.AddUint32`으로 카운터를 원자적으로 증가시켜 동시성 안전한 라운드 로빈을 구현한다. 선택된 프록시를 리스트 맨 앞에 배치하고 나머지를 뒤에 추가한다.

### 3.4 sequential 정책

```
// plugin/forward/policy.go:62-68
type sequential struct{}

func (r *sequential) List(p []*proxy.Proxy) []*proxy.Proxy {
    return p
}
```

설정된 순서 그대로 반환한다. 첫 번째 업스트림이 기본 서버, 나머지가 백업 역할을 한다.

### 3.5 정책 비교

```
┌─────────────┬──────────────────────────────────────────────────────┐
│ 정책        │ 동작                                                │
├─────────────┼──────────────────────────────────────────────────────┤
│ random      │ 매 요청마다 프록시 순서를 무작위로 섞음               │
│             │ → 부하를 균등 분산                                   │
│             │ → 기본값                                             │
├─────────────┼──────────────────────────────────────────────────────┤
│ round_robin │ 순서대로 돌아가며 첫 번째 프록시 선택                │
│             │ → 예측 가능한 분산                                   │
│             │ → atomic 카운터로 동시성 안전                        │
├─────────────┼──────────────────────────────────────────────────────┤
│ sequential  │ 항상 동일한 순서                                    │
│             │ → 첫 번째가 기본, 나머지가 백업                      │
│             │ → Primary-Secondary 구성에 적합                     │
└─────────────┴──────────────────────────────────────────────────────┘
```

---

## 4. Proxy 구조체 (plugin/pkg/proxy)

### 4.1 Proxy 정의

```
// plugin/pkg/proxy/proxy.go:14-26
type Proxy struct {
    fails     uint32
    addr      string
    proxyName string

    transport *Transport

    readTimeout time.Duration

    probe  *up.Probe
    health HealthChecker
}
```

| 필드 | 역할 |
|------|------|
| `fails` | 연속 실패 횟수 (atomic) |
| `addr` | 업스트림 주소 (host:port) |
| `transport` | 연결 풀 관리 |
| `readTimeout` | 읽기 타임아웃 (기본 2초) |
| `probe` | 주기적 헬스체크 실행기 |
| `health` | 헬스체크 구현체 |

### 4.2 NewProxy

```
// plugin/pkg/proxy/proxy.go:29-42
func NewProxy(proxyName, addr, trans string) *Proxy {
    p := &Proxy{
        addr:        addr,
        fails:       0,
        probe:       up.New(),
        readTimeout: 2 * time.Second,
        transport:   newTransport(proxyName, addr),
        health:      NewHealthChecker(proxyName, trans, true, "."),
        proxyName:   proxyName,
    }
    runtime.SetFinalizer(p, (*Proxy).finalizer)
    return p
}
```

`runtime.SetFinalizer`로 GC 시 Transport를 정리한다.

### 4.3 Down 판정

```
// plugin/pkg/proxy/proxy.go:88-95
func (p *Proxy) Down(maxfails uint32) bool {
    if maxfails == 0 {
        return false  // maxfails=0이면 항상 사용 가능
    }
    fails := atomic.LoadUint32(&p.fails)
    return fails > maxfails
}
```

**maxfails=0의 의미**: 헬스체크를 무시하고 항상 프록시를 사용한다. 이는 단일 업스트림 환경에서 유용하다.

**fails > maxfails (>=가 아닌 >)**: maxfails=2일 때, 3번째 실패부터 Down으로 판정한다.

---

## 5. Connect - 실제 DNS 전달

### 5.1 Connect 메서드

```
// plugin/pkg/proxy/connect.go:106-188
func (p *Proxy) Connect(ctx context.Context, state request.Request, opts Options) (*dns.Msg, error) {
    start := time.Now()

    // 프로토콜 결정
    var proto string
    switch {
    case opts.ForceTCP:
        proto = "tcp"
    case opts.PreferUDP:
        proto = "udp"
    default:
        proto = state.Proto()  // 클라이언트 프로토콜 따라감
    }

    // 연결 획득 (캐시 또는 새 연결)
    pc, cached, err := p.transport.Dial(proto)
    if err != nil {
        return nil, err
    }

    // UDP 버퍼 크기 설정
    pc.c.UDPSize = max(uint16(state.Size()), 512)

    // 요청 ID 변경 (보안)
    originId := state.Req.Id
    state.Req.Id = dns.Id()
    defer func() { state.Req.Id = originId }()

    // 쿼리 전송
    pc.c.SetWriteDeadline(time.Now().Add(maxTimeout))
    if err := pc.c.WriteMsg(state.Req); err != nil {
        pc.c.Close()
        if err == io.EOF && cached {
            return nil, ErrCachedClosed
        }
        return nil, err
    }

    // 응답 수신
    pc.c.SetReadDeadline(time.Now().Add(p.readTimeout))
    for {
        ret, err = pc.c.ReadMsg()
        if err != nil {
            // UDP 오버플로우 처리 → Truncated 응답
            if ret != nil && (state.Req.Id == ret.Id) && ... && shouldTruncateResponse(err) {
                ret = truncateResponse(ret)
                break
            }
            pc.c.Close()
            if err == io.EOF && cached {
                return nil, ErrCachedClosed
            }
            return ret, err
        }
        // out-of-order 응답 무시
        if state.Req.Id == ret.Id {
            break
        }
    }

    ret.Id = originId  // 원래 ID 복원
    p.transport.Yield(pc)  // 연결 반환
    return ret, nil
}
```

### 5.2 Connect 흐름도

```
Connect() 호출
    │
    ▼
프로토콜 결정
    ├── ForceTCP   → "tcp"
    ├── PreferUDP  → "udp"
    └── default    → 클라이언트 프로토콜
    │
    ▼
Transport.Dial(proto)
    ├── 캐시된 연결 있음 → 재사용 (cached=true)
    └── 없음 → 새 연결 생성 (cached=false)
    │
    ▼
요청 ID 무작위 변경 (DNS ID 스푸핑 방지)
    │
    ▼
WriteMsg() → 업스트림에 쿼리 전송
    │
    ▼
ReadMsg() 루프
    ├── out-of-order 응답 → 무시하고 재읽기
    ├── UDP 오버플로우    → TC 비트 설정 응답
    ├── EOF + cached     → ErrCachedClosed (재시도 유도)
    ├── 기타 에러         → 연결 닫기 + 에러 반환
    └── 정상 응답         → break
    │
    ▼
원래 요청 ID 복원
    │
    ▼
Transport.Yield(pc) → 연결을 캐시에 반환
    │
    ▼
응답 반환
```

### 5.3 요청 ID 변경

```
originId := state.Req.Id
state.Req.Id = dns.Id()
defer func() { state.Req.Id = originId }()
```

**왜 요청 ID를 변경하는가?**

DNS ID 스푸핑 공격을 방지하기 위해서이다. 클라이언트의 원래 ID를 그대로 사용하면 중간자가 ID를 예측하여 가짜 응답을 주입할 수 있다. 새로운 무작위 ID를 사용하고, 응답 수신 후 원래 ID로 복원한다.

### 5.4 ErrCachedClosed 처리

```
if err == io.EOF && cached {
    return nil, ErrCachedClosed
}
```

캐시된 연결이 원격 측에 의해 닫힌 경우, `ErrCachedClosed`를 반환한다. `ServeDNS`의 내부 루프에서 이 에러를 받으면 새 연결로 재시도한다:

```
// plugin/forward/forward.go:169-181
for {
    ret, err = proxy.Connect(ctx, state, opts)
    if err == proxyPkg.ErrCachedClosed {
        continue  // 캐시 연결이 닫혀있으면 재시도
    }
    if ret != nil && ret.Truncated && !opts.ForceTCP && opts.PreferUDP {
        opts.ForceTCP = true
        continue  // Truncated + PreferUDP → TCP로 재시도
    }
    break
}
```

### 5.5 UDP 오버플로우 처리

```
// plugin/pkg/proxy/connect.go:193-213
func shouldTruncateResponse(err error) bool {
    if _, isDNSErr := err.(*dns.Error); isDNSErr && errors.Is(err, dns.ErrBuf) {
        return true
    } else if strings.Contains(err.Error(), "overflow") {
        return true
    }
    return false
}

func truncateResponse(response *dns.Msg) *dns.Msg {
    response.Answer = nil
    response.Extra = nil
    response.Ns = nil
    response.Truncated = true
    return response
}
```

업스트림이 EDNS0 없이 512바이트를 초과하는 UDP 응답을 보내면, TC(Truncated) 비트를 설정한 빈 응답을 반환하여 클라이언트가 TCP로 재시도하도록 유도한다.

---

## 6. Transport - 연결 관리

### 6.1 Transport 구조체

```
// plugin/pkg/proxy/persistent.go:20-32
type Transport struct {
    avgDialTime  int64
    conns        [typeTotalCount][]*persistConn  // UDP, TCP, TCP-TLS 버킷
    expire       time.Duration                   // 유휴 연결 만료 (기본 10초)
    maxAge       time.Duration                   // 연결 최대 수명 (0=무제한)
    maxIdleConns int                             // 프로토콜별 최대 유휴 연결 (0=무제한)
    addr         string
    tlsConfig    *tls.Config
    proxyName    string

    mu   sync.Mutex
    stop chan struct{}
}
```

### 6.2 persistConn 구조체

```
// plugin/pkg/proxy/persistent.go:13-17
type persistConn struct {
    c       *dns.Conn
    created time.Time
    used    time.Time
}
```

### 6.3 Dial - 연결 획득

```
// plugin/pkg/proxy/connect.go:52-103
func (t *Transport) Dial(proto string) (*persistConn, bool, error) {
    if t.tlsConfig != nil {
        proto = "tcp-tls"
    }

    // 1. 중지 상태 확인
    select {
    case <-t.stop:
        return nil, false, errors.New(ErrTransportStopped)
    default:
    }

    transtype := stringToTransportType(proto)

    // 2. 캐시에서 연결 검색 (FIFO)
    t.mu.Lock()
    var maxAgeDeadline time.Time
    if t.maxAge > 0 {
        maxAgeDeadline = time.Now().Add(-t.maxAge)
    }
    for len(t.conns[transtype]) > 0 {
        pc := t.conns[transtype][0]           // FIFO: 가장 오래된 연결 사용
        t.conns[transtype] = t.conns[transtype][1:]
        if time.Since(pc.used) > t.expire {   // 만료된 연결 폐기
            pc.c.Close()
            continue
        }
        if !maxAgeDeadline.IsZero() && pc.created.Before(maxAgeDeadline) {
            pc.c.Close()                       // maxAge 초과 연결 폐기
            continue
        }
        t.mu.Unlock()
        connCacheHitsCount.Add(1)
        return pc, true, nil                   // 캐시 히트
    }
    t.mu.Unlock()

    // 3. 새 연결 생성
    connCacheMissesCount.Add(1)
    reqTime := time.Now()
    timeout := t.dialTimeout()
    if proto == "tcp-tls" {
        conn, err := dns.DialTimeoutWithTLS("tcp", t.addr, t.tlsConfig, timeout)
        t.updateDialTimeout(time.Since(reqTime))
        return &persistConn{c: conn, created: time.Now()}, false, err
    }
    conn, err := dns.DialTimeout(proto, t.addr, timeout)
    t.updateDialTimeout(time.Since(reqTime))
    return &persistConn{c: conn, created: time.Now()}, false, err
}
```

**왜 FIFO를 사용하는가?**

```
// FIFO: take the oldest conn (front of slice) for source port diversity
```

가장 오래된 연결을 먼저 사용하면 소스 포트의 다양성이 증가하여 NAT 환경에서의 conntrack 충돌을 줄인다. LIFO를 사용하면 같은 소스 포트가 반복적으로 재사용되어 문제가 될 수 있다.

### 6.4 Yield - 연결 반환

```
// plugin/pkg/proxy/persistent.go:123-146
func (t *Transport) Yield(pc *persistConn) {
    select {
    case <-t.stop:
        pc.c.Close()
        return
    default:
    }

    pc.used = time.Now()

    t.mu.Lock()
    defer t.mu.Unlock()

    transtype := t.transportTypeFromConn(pc)

    if t.maxIdleConns > 0 && len(t.conns[transtype]) >= t.maxIdleConns {
        pc.c.Close()  // 최대 유휴 연결 수 초과 시 폐기
        return
    }

    t.conns[transtype] = append(t.conns[transtype], pc)
}
```

### 6.5 연결 정리 (connManager)

```
// plugin/pkg/proxy/persistent.go:47-59
func (t *Transport) connManager() {
    ticker := time.NewTicker(defaultExpire)
    defer ticker.Stop()
    for {
        select {
        case <-ticker.C:
            t.cleanup(false)
        case <-t.stop:
            t.cleanup(true)  // 중지 시 모든 연결 정리
            return
        }
    }
}
```

### 6.6 cleanup 구현

```
// plugin/pkg/proxy/persistent.go:69-120
func (t *Transport) cleanup(all bool) {
    var toClose []*persistConn

    t.mu.Lock()
    now := time.Now()
    staleTime := now.Add(-t.expire)
    var maxAgeDeadline time.Time
    if t.maxAge > 0 {
        maxAgeDeadline = now.Add(-t.maxAge)
    }
    for transtype, stack := range t.conns {
        if all {
            t.conns[transtype] = nil
            toClose = append(toClose, stack...)
            continue
        }
        if t.maxAge > 0 {
            // maxAge 설정 시: 선형 스캔으로 expire + maxAge 모두 확인
            var alive []*persistConn
            for _, pc := range stack {
                if !pc.used.After(staleTime) || pc.created.Before(maxAgeDeadline) {
                    toClose = append(toClose, pc)
                } else {
                    alive = append(alive, pc)
                }
            }
            t.conns[transtype] = alive
            continue
        }
        // expire만: 이진 검색으로 효율적 정리
        if stack[0].used.After(staleTime) {
            continue
        }
        good := sort.Search(len(stack), func(i int) bool {
            return stack[i].used.After(staleTime)
        })
        t.conns[transtype] = stack[good:]
        toClose = append(toClose, stack[:good]...)
    }
    t.mu.Unlock()

    closeConns(toClose)  // 잠금 해제 후 연결 닫기
}
```

**핵심 최적화**:
- **잠금 후 닫기 분리**: 연결 닫기는 I/O 작업이므로 잠금 밖에서 수행
- **이진 검색**: expire만 사용할 때, 연결이 `used` 시간으로 정렬되어 있으므로 `sort.Search`로 O(log n) 정리
- **선형 스캔**: maxAge가 설정되면 두 기준을 동시에 확인해야 하므로 O(n) 스캔

### 6.7 연결 관리 흐름도

```
┌──────────────────────────────────────────────────────────────┐
│                    연결 수명주기                              │
│                                                              │
│  Dial()                                                      │
│    ├── 캐시 검색 (FIFO)                                      │
│    │   ├── 만료(expire) 확인 → 닫기                         │
│    │   ├── maxAge 확인 → 닫기                               │
│    │   └── 유효 → 재사용 (cached=true)                      │
│    └── 캐시 미스 → 새 연결 (cached=false)                   │
│           ├── dns.DialTimeout (UDP/TCP)                      │
│           └── dns.DialTimeoutWithTLS (TCP-TLS)              │
│                                                              │
│  Connect() 내 사용                                           │
│    WriteMsg() → ReadMsg()                                    │
│                                                              │
│  Yield()                                                     │
│    ├── maxIdleConns 확인 → 초과 시 닫기                     │
│    └── 캐시에 반환 (used 시간 갱신)                         │
│                                                              │
│  connManager() (백그라운드 고루틴)                            │
│    └── 주기적 cleanup() (expire 간격마다)                    │
│        ├── expire된 연결 제거                                │
│        └── maxAge 초과 연결 제거                             │
└──────────────────────────────────────────────────────────────┘
```

---

## 7. 헬스체크 메커니즘

### 7.1 HealthChecker 인터페이스

```
// plugin/pkg/proxy/health.go:15-28
type HealthChecker interface {
    Check(*Proxy) error
    SetTLSConfig(*tls.Config)
    GetTLSConfig() *tls.Config
    SetRecursionDesired(bool)
    GetRecursionDesired() bool
    SetDomain(domain string)
    GetDomain() string
    SetTCPTransport()
    GetReadTimeout() time.Duration
    SetReadTimeout(time.Duration)
    GetWriteTimeout() time.Duration
    SetWriteTimeout(time.Duration)
}
```

### 7.2 dnsHc 구현

```
// plugin/pkg/proxy/health.go:31-37
type dnsHc struct {
    c                *dns.Client
    recursionDesired bool
    domain           string
    proxyName        string
}
```

```
// plugin/pkg/proxy/health.go:40-57
func NewHealthChecker(proxyName, trans string, recursionDesired bool, domain string) HealthChecker {
    switch trans {
    case transport.DNS, transport.TLS:
        c := new(dns.Client)
        c.Net = "udp"
        c.ReadTimeout = 1 * time.Second
        c.WriteTimeout = 1 * time.Second
        return &dnsHc{
            c:                c,
            recursionDesired: recursionDesired,
            domain:           domain,
            proxyName:        proxyName,
        }
    }
    return nil
}
```

### 7.3 헬스체크 쿼리

```
// plugin/pkg/proxy/health.go:119-134
func (h *dnsHc) send(addr string) error {
    ping := new(dns.Msg)
    ping.SetQuestion(h.domain, dns.TypeNS)  // 기본: ". IN NS" 쿼리
    ping.RecursionDesired = h.recursionDesired

    m, _, err := h.c.Exchange(ping, addr)
    if err != nil && m != nil {
        if m.Response || m.Opcode == dns.OpcodeQuery {
            err = nil  // 헤더가 오면 살아있다고 판단
        }
    }
    return err
}
```

**왜 `. IN NS` 쿼리를 사용하는가?**

루트 도메인의 NS 쿼리는 거의 모든 DNS 서버가 응답할 수 있는 가장 가벼운 쿼리이다. 내용은 중요하지 않고, 응답이 돌아오는지(연결이 살아있는지)만 확인한다.

**관대한 판정**: 에러가 발생하더라도 DNS 헤더가 수신되면 건강한 것으로 간주한다. I/O 에러만 실패로 처리한다.

### 7.4 Check와 fails 관리

```
// plugin/pkg/proxy/health.go:107-117
func (h *dnsHc) Check(p *Proxy) error {
    err := h.send(p.addr)
    if err != nil {
        healthcheckFailureCount.Add(1)
        p.incrementFails()
        return err
    }
    atomic.StoreUint32(&p.fails, 0)  // 성공 시 실패 카운터 초기화
    return nil
}
```

**원자적 실패 카운터**:

```
// plugin/pkg/proxy/proxy.go:112-119
func (p *Proxy) incrementFails() {
    curVal := atomic.LoadUint32(&p.fails)
    if curVal > curVal+1 {
        // 오버플로우 방지
        return
    }
    atomic.AddUint32(&p.fails, 1)
}
```

### 7.5 인밴드 헬스체크

```
// plugin/pkg/proxy/proxy.go:76-85
func (p *Proxy) Healthcheck() {
    if p.health == nil {
        return
    }
    p.probe.Do(func() error {
        return p.health.Check(p)
    })
}
```

`probe.Do`는 중복 실행을 방지하며, 현재 실행 중인 헬스체크가 있으면 추가 요청을 무시한다.

**인밴드 헬스체크 트리거 시점**:

```
// plugin/forward/forward.go:193-197
if err != nil {
    if f.maxfails != 0 {
        proxy.Healthcheck()
    }
    // ...
}
```

요청 실패 시 즉시 헬스체크를 트리거한다. 이는 주기적 헬스체크와 별개로, 실패를 빠르게 감지한다.

### 7.6 헬스체크 흐름도

```
┌──────────────────────────────────────────────────────────────┐
│                    헬스체크 메커니즘                          │
│                                                              │
│  주기적 헬스체크 (hcInterval: 500ms 기본)                    │
│    probe.Start(duration)                                     │
│    └── 매 interval마다: health.Check(proxy)                  │
│        ├── send(addr): ". IN NS" 쿼리                       │
│        ├── 성공 → fails = 0 (atomic)                         │
│        └── 실패 → fails++ (atomic)                           │
│                                                              │
│  인밴드 헬스체크 (요청 실패 시)                              │
│    proxy.Healthcheck()                                       │
│    └── probe.Do(): 중복 실행 방지                            │
│        └── health.Check(proxy)                               │
│                                                              │
│  Down 판정                                                   │
│    proxy.Down(maxfails)                                      │
│    └── fails > maxfails → true (Down)                        │
│                                                              │
│  예: maxfails=2                                              │
│    fails=0 → Up (정상)                                       │
│    fails=1 → Up                                              │
│    fails=2 → Up                                              │
│    fails=3 → Down! → 다른 프록시 시도                        │
│    (헬스체크 성공 시 fails=0으로 복원)                        │
└──────────────────────────────────────────────────────────────┘
```

---

## 8. 다이얼 타임아웃 자동 조정

### 8.1 적응적 타임아웃

```
// plugin/pkg/proxy/connect.go:27-36
func limitTimeout(currentAvg *int64, minValue time.Duration, maxValue time.Duration) time.Duration {
    rt := time.Duration(atomic.LoadInt64(currentAvg))
    if rt < minValue {
        return minValue
    }
    if rt < maxValue/2 {
        return 2 * rt  // 평균의 2배
    }
    return maxValue
}

func averageTimeout(currentAvg *int64, observedDuration time.Duration, weight int64) {
    dt := time.Duration(atomic.LoadInt64(currentAvg))
    atomic.AddInt64(currentAvg, int64(observedDuration-dt)/weight)
}
```

**이동 평균 알고리즘**:

```
newAvg = oldAvg + (observed - oldAvg) / weight
```

weight=4인 지수 이동 평균을 사용한다. 타임아웃은 관측된 평균 다이얼 시간의 2배로 설정되며, 최소 1초에서 최대 30초 사이로 제한된다.

```
// plugin/pkg/proxy/persistent.go:171-175
const (
    minDialTimeout = 1 * time.Second
    maxDialTimeout = 30 * time.Second
)
```

---

## 9. 프로토콜 지원

### 9.1 지원 프로토콜

| 프로토콜 | 식별 | 설명 |
|----------|------|------|
| DNS (UDP/TCP) | `dns://` 또는 기본 | 표준 DNS |
| DoT (DNS over TLS) | `tls://` | TLS 암호화된 DNS |

### 9.2 TLS 설정

```
// plugin/forward/setup.go:163-186
for i, hostWithZone := range toHosts {
    host, serverName := splitZone(hostWithZone)
    trans, h := parse.Transport(host)
    // ...
    if trans == transport.TLS && serverName != "" {
        tlsServerNames[i] = serverName
        perServerNameProxyCount[serverName]++
    }
    p := proxy.NewProxy("forward", h, trans)
    f.proxies = append(f.proxies, p)
}
```

프록시별 TLS 서버 이름 지정:
```
forward . tls://8.8.8.8%dns.google tls://8.8.4.4%dns.google
```

`%` 뒤의 문자열이 TLS ServerName으로 사용된다.

### 9.3 TLS ClientSessionCache

```
// plugin/forward/setup.go:202
f.tlsConfig.ClientSessionCache = tls.NewLRUClientSessionCache(len(f.proxies))
```

TLS 세션 캐시를 설정하여 후속 연결의 핸드셰이크를 가속화한다.

---

## 10. setup.go - 플러그인 초기화

### 10.1 등록

```
// plugin/forward/setup.go:24-26
func init() {
    plugin.Register("forward", setup)
}
```

### 10.2 다중 Forward 인스턴스 체이닝

```
// plugin/forward/setup.go:28-69
func setup(c *caddy.Controller) error {
    fs, err := parseForward(c)
    for i := range fs {
        f := fs[i]
        if i == len(fs)-1 {
            // 마지막 forward: next를 다음 플러그인으로
            dnsserver.GetConfig(c).AddPlugin(func(next plugin.Handler) plugin.Handler {
                f.Next = next
                return f
            })
        } else {
            // 중간 forward: next를 다음 forward로
            nextForward := fs[i+1]
            dnsserver.GetConfig(c).AddPlugin(func(plugin.Handler) plugin.Handler {
                f.Next = nextForward
                return f
            })
        }
    }
}
```

여러 `forward` 블록을 체이닝할 수 있다:

```
.:53 {
    forward example.com 10.0.0.1
    forward . 8.8.8.8
}
```

첫 번째 forward가 `example.com`을 처리하고, 매칭되지 않으면 두 번째 forward(모든 도메인)로 전달된다.

### 10.3 주요 설정 옵션

```
// plugin/forward/setup.go:227-427 (parseBlock)
switch c.Val() {
case "except":              // 제외 도메인
case "max_fails":           // 최대 실패 횟수 (기본 2)
case "max_connect_attempts": // 요청당 최대 연결 시도
case "health_check":        // 헬스체크 간격 + 옵션
case "force_tcp":           // TCP 강제 사용
case "prefer_udp":          // UDP 우선 사용
case "tls":                 // TLS 인증서
case "tls_servername":      // TLS 서버 이름
case "expire":              // 유휴 연결 만료 (기본 10s)
case "max_age":             // 연결 최대 수명
case "max_idle_conns":      // 최대 유휴 연결 수
case "policy":              // random|round_robin|sequential
case "max_concurrent":      // 최대 동시 쿼리 수
case "next":                // 특정 Rcode시 다음 forward로
case "failfast_all_unhealthy_upstreams": // 모두 다운시 즉시 실패
case "failover":            // 특정 Rcode시 다음 업스트림으로
}
```

### 10.4 OnStartup과 OnShutdown

```
// plugin/forward/setup.go:72-86
func (f *Forward) OnStartup() (err error) {
    for _, p := range f.proxies {
        p.Start(f.hcInterval)  // 헬스체크 고루틴 시작
    }
    return nil
}

func (f *Forward) OnShutdown() error {
    for _, p := range f.proxies {
        p.Stop()  // 헬스체크 고루틴 중지
    }
    return nil
}
```

---

## 11. Corefile 설정 예시

### 11.1 기본 설정

```
.:53 {
    forward . 8.8.8.8 8.8.4.4
}
```

### 11.2 고급 설정

```
.:53 {
    forward . 10.0.0.1 10.0.0.2 10.0.0.3 {
        policy round_robin
        max_fails 3
        max_concurrent 1000
        expire 30s
        max_idle_conns 20
        health_check 5s domain example.com
        failover SERVFAIL REFUSED
        failfast_all_unhealthy_upstreams
        max_connect_attempts 3
    }
}
```

### 11.3 DoT (DNS over TLS) 설정

```
.:53 {
    forward . tls://8.8.8.8 tls://8.8.4.4 {
        tls_servername dns.google
    }
}
```

### 11.4 분할 DNS (Split DNS)

```
.:53 {
    forward internal.company.com 10.0.0.53 {
        policy sequential
    }
    forward . 8.8.8.8 {
        except internal.company.com
    }
}
```

---

## 12. 에러 처리와 복원력

### 12.1 에러 유형

```
// plugin/forward/forward.go:289-296
var (
    ErrNoHealthy    = errors.New("no healthy proxies")
    ErrNoForward    = errors.New("no forwarder defined")
    ErrCachedClosed = errors.New("cached connection was closed by peer")
)
```

### 12.2 복원력 메커니즘 정리

```
┌───────────────────────────────────────────────────────────────┐
│                    Forward 복원력 계층                         │
│                                                               │
│  1층: 연결 재시도                                             │
│    └── ErrCachedClosed → 같은 프록시로 새 연결                │
│    └── Truncated + PreferUDP → TCP로 재시도                   │
│                                                               │
│  2층: 프록시 장애조치                                         │
│    └── 연결 실패 → 다음 프록시                                │
│    └── failoverRcodes 매칭 → 다음 프록시                     │
│                                                               │
│  3층: 헬스체크 기반 회피                                      │
│    └── Down(maxfails) → 해당 프록시 건너뜀                    │
│    └── 주기적 + 인밴드 헬스체크로 상태 추적                    │
│                                                               │
│  4층: 전체 장애 대응                                          │
│    └── 모든 프록시 Down + !failfast → 무작위 선택             │
│    └── 모든 프록시 Down + failfast → SERVFAIL                 │
│                                                               │
│  5층: Forward 체이닝                                          │
│    └── nextAlternateRcodes → 다음 Forward 플러그인            │
│                                                               │
│  6층: 자원 보호                                               │
│    └── maxConcurrent → REFUSED                                │
│    └── defaultTimeout (5s) → 데드라인                         │
│    └── maxConnectAttempts → 재시도 제한                        │
└───────────────────────────────────────────────────────────────┘
```

---

## 13. 메트릭과 관찰 가능성

Forward 플러그인은 다양한 Prometheus 메트릭을 제공한다:

| 메트릭 | 설명 |
|--------|------|
| `coredns_forward_request_duration_seconds` | 업스트림 요청 지속 시간 |
| `coredns_forward_healthcheck_broken_total` | 모든 업스트림 다운 횟수 |
| `coredns_forward_max_concurrent_rejects_total` | 동시 제한 거부 횟수 |
| `coredns_proxy_conn_cache_hits_total` | 연결 캐시 히트 |
| `coredns_proxy_conn_cache_misses_total` | 연결 캐시 미스 |
| `coredns_proxy_healthcheck_failures_total` | 헬스체크 실패 |

---

## 14. 성능 최적화 설계

### 14.1 연결 캐싱

50% 이상의 성능 향상 효과:
```
// 패키지 주석: "Depending on how you benchmark this looks to be
// 50% faster than just opening a new connection for every client."
```

### 14.2 Lock-free 카운터

`concurrent`, `fails`, `robin` 등 핫 경로의 카운터는 모두 `sync/atomic`을 사용하여 잠금 없이 원자적으로 증감한다.

### 14.3 적응적 다이얼 타임아웃

지수 이동 평균으로 실제 네트워크 지연에 적응하여 불필요한 대기를 줄인다.

### 14.4 잠금 밖 I/O

연결 닫기, 헬스체크 쿼리 등 I/O 작업은 반드시 뮤텍스 잠금을 해제한 후 수행한다.

---

## 15. 정리

```
┌──────────────────────────────────────────────────────────────┐
│                   Forward 플러그인 아키텍처                   │
│                                                              │
│  Corefile                                                    │
│  forward . 8.8.8.8 8.8.4.4 { policy round_robin }          │
│       │                                                      │
│       ▼                                                      │
│  setup() → parseForward() → parseStanza() + parseBlock()    │
│       │                                                      │
│       ▼                                                      │
│  Forward 인스턴스 생성                                       │
│    ├── Proxy[0] (8.8.8.8)                                   │
│    │   ├── Transport (연결 풀)                               │
│    │   ├── HealthChecker (dnsHc)                             │
│    │   └── Probe (주기적 헬스체크)                            │
│    └── Proxy[1] (8.8.4.4)                                   │
│        ├── Transport                                         │
│        ├── HealthChecker                                     │
│        └── Probe                                             │
│                                                              │
│  DNS 쿼리 처리:                                              │
│  ServeDNS()                                                  │
│    ├── match() → Zone + except 확인                         │
│    ├── maxConcurrent 확인                                    │
│    ├── Policy.List() → 프록시 순서 결정                      │
│    └── 순회 루프                                             │
│        ├── Down() 확인                                       │
│        ├── Connect() → Dial + WriteMsg + ReadMsg             │
│        ├── 에러 시: Healthcheck() + 다음 프록시              │
│        ├── failoverRcodes 확인                               │
│        └── 성공 → WriteMsg → 클라이언트 응답                 │
└──────────────────────────────────────────────────────────────┘
```

Forward 플러그인의 핵심 설계 결정:
1. **연결 캐싱**: FIFO 기반 연결 풀로 성능 50% 향상
2. **적응적 타임아웃**: 네트워크 상황에 자동 적응
3. **다층 복원력**: 재시도 → 장애조치 → 헬스체크 → 전체 장애 대응
4. **Lock-free 핫 경로**: atomic 연산으로 동시성 성능 보장
5. **플러그인 체이닝**: 여러 Forward 인스턴스를 연결하여 분할 DNS 구현
