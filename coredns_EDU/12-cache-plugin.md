# 12. Cache 플러그인 Deep-Dive

## 개요

CoreDNS의 Cache 플러그인은 DNS 응답을 캐싱하여 반복 쿼리에 대한 응답 속도를 높이고 업스트림 서버의 부하를 줄이는 플러그인이다. 양성 응답(Success)과 음성 응답(Denial)을 별도의 캐시로 관리하며, 프리페치(prefetch)와 스테일 서빙(stale serving) 등 고급 기능을 제공한다.

소스코드 경로: `plugin/cache/`

---

## 1. 핵심 데이터 구조

### 1.1 Cache 구조체

`plugin/cache/cache.go:21-57`에 정의된 Cache 구조체는 플러그인의 중심이다.

```
// plugin/cache/cache.go:21-57
type Cache struct {
    Next  plugin.Handler
    Zones []string

    zonesMetricLabel string
    viewMetricLabel  string

    ncache  *cache.Cache[*item]    // 음성 캐시 (NXDOMAIN, NODATA, SERVFAIL)
    ncap    int                     // 음성 캐시 용량
    nttl    time.Duration           // 음성 캐시 최대 TTL
    minnttl time.Duration           // 음성 캐시 최소 TTL

    pcache  *cache.Cache[*item]    // 양성 캐시 (NoError, Delegation)
    pcap    int                     // 양성 캐시 용량
    pttl    time.Duration           // 양성 캐시 최대 TTL
    minpttl time.Duration           // 양성 캐시 최소 TTL
    failttl time.Duration           // SERVFAIL 캐싱 TTL

    prefetch   int                  // 프리페치 활성화 임계값 (히트 수)
    duration   time.Duration        // 프리페치 빈도 측정 기간
    percentage int                  // TTL 잔여 비율 (프리페치 트리거)

    staleUpTo   time.Duration       // 스테일 서빙 허용 기간
    verifyStale bool                // 스테일 응답 전 검증 여부

    pexcept []string                // 양성 캐시 제외 zone
    nexcept []string                // 음성 캐시 제외 zone

    keepttl bool                    // 원래 TTL 유지 여부

    now func() time.Time            // 테스트를 위한 시간 함수
}
```

주요 필드 분석:

| 필드 | 기본값 | 역할 |
|------|--------|------|
| `pcache` | 10000 항목 | 양성(성공) 응답 캐시 |
| `ncache` | 10000 항목 | 음성(실패) 응답 캐시 |
| `pttl` | maxTTL | 양성 캐시 최대 TTL |
| `nttl` | maxNTTL (maxTTL/2) | 음성 캐시 최대 TTL |
| `minpttl` | minTTL | 양성 캐시 최소 TTL |
| `minnttl` | minNTTL | 음성 캐시 최소 TTL |
| `failttl` | minNTTL | SERVFAIL 캐싱 TTL |
| `prefetch` | 0 (비활성) | 프리페치 활성화 히트 임계값 |
| `duration` | 1분 | 프리페치 빈도 측정 기간 |
| `percentage` | 10% | TTL 잔여 비율 트리거 |
| `staleUpTo` | 0 (비활성) | 스테일 서빙 허용 기간 |

### 1.2 기본값과 상수

```
// plugin/cache/cache.go:306-318
const (
    maxTTL  = dnsutil.MaximumDefaultTTL       // 양성 캐시 최대 TTL
    minTTL  = dnsutil.MinimalDefaultTTL       // 양성 캐시 최소 TTL
    maxNTTL = dnsutil.MaximumDefaultTTL / 2   // 음성 캐시 최대 TTL
    minNTTL = dnsutil.MinimalDefaultTTL       // 음성 캐시 최소 TTL

    defaultCap = 10000  // 기본 캐시 용량

    Success = "success"  // 양성 캐시 분류
    Denial  = "denial"   // 음성 캐시 분류
)
```

### 1.3 New 함수

```
// plugin/cache/cache.go:61-78
func New() *Cache {
    return &Cache{
        Zones:      []string{"."},
        pcap:       defaultCap,
        pcache:     cache.New[*item](defaultCap),
        pttl:       maxTTL,
        minpttl:    minTTL,
        ncap:       defaultCap,
        ncache:     cache.New[*item](defaultCap),
        nttl:       maxNTTL,
        minnttl:    minNTTL,
        failttl:    minNTTL,
        prefetch:   0,
        duration:   1 * time.Minute,
        percentage: 10,
        now:        time.Now,
    }
}
```

---

## 2. 이중 캐시 아키텍처

### 2.1 양성 캐시 (pcache)와 음성 캐시 (ncache)

```
┌──────────────────────────────────────────────────────────────┐
│                    이중 캐시 아키텍처                         │
│                                                              │
│  DNS 응답 분류                                               │
│                                                              │
│  ┌──────────────┐        ┌──────────────┐                   │
│  │  pcache      │        │  ncache      │                   │
│  │  (양성 캐시) │        │  (음성 캐시) │                   │
│  │              │        │              │                   │
│  │  NoError     │        │  NameError   │                   │
│  │  Delegation  │        │  (NXDOMAIN)  │                   │
│  │              │        │  NoData      │                   │
│  │  pttl: max   │        │  ServerError │                   │
│  │  minpttl: min│        │  (SERVFAIL)  │                   │
│  │  pcap: 10000 │        │              │                   │
│  │              │        │  nttl: max/2 │                   │
│  │              │        │  minnttl: min│                   │
│  │              │        │  ncap: 10000 │                   │
│  └──────────────┘        └──────────────┘                   │
│                                                              │
│  캐싱되지 않는 응답:                                         │
│    - Truncated 응답                                          │
│    - OtherError                                              │
│    - Meta / Update 응답                                      │
└──────────────────────────────────────────────────────────────┘
```

**왜 이중 캐시를 사용하는가?**

1. **TTL 정책 분리**: 양성과 음성 응답에 다른 TTL 한도를 적용할 수 있다. 음성 응답은 보통 더 짧은 TTL이 적절하다.
2. **용량 분리**: 양성과 음성 캐시의 크기를 독립적으로 설정할 수 있다.
3. **선택적 비활성화**: `disable` 옵션으로 양성 또는 음성 캐시만 특정 zone에서 비활성화할 수 있다.

### 2.2 응답 타입 분류

Cache 플러그인은 `response.Typify`를 사용하여 DNS 응답을 분류한다:

| response.Type | DNS 의미 | 캐시 위치 |
|---------------|----------|----------|
| `NoError` | 성공 응답 (Answer 존재) | pcache |
| `Delegation` | 위임 응답 (NS 레코드) | pcache |
| `NameError` | NXDOMAIN | ncache |
| `NoData` | 이름은 존재하지만 해당 타입 없음 | ncache |
| `ServerError` | SERVFAIL | ncache |
| `OtherError` | 기타 에러 | 캐싱 안 함 |
| `Meta` | 메타 응답 | 캐싱 안 함 |
| `Update` | 동적 업데이트 | 캐싱 안 함 |

---

## 3. 캐시 키 생성

### 3.1 key 함수

```
// plugin/cache/cache.go:83-94
func key(qname string, m *dns.Msg, t response.Type, do, cd bool) (bool, uint64) {
    // Truncated 응답은 캐싱하지 않음
    if m.Truncated {
        return false, 0
    }
    // 에러, Meta, Update 응답도 캐싱하지 않음
    if t == response.OtherError || t == response.Meta || t == response.Update {
        return false, 0
    }
    return true, hash(qname, m.Question[0].Qtype, do, cd)
}
```

### 3.2 hash 함수 - FNV-64 해시

```
// plugin/cache/cache.go:99-119
func hash(qname string, qtype uint16, do, cd bool) uint64 {
    h := fnv.New64()

    if do {
        h.Write(one)   // []byte("1")
    } else {
        h.Write(zero)  // []byte("0")
    }

    if cd {
        h.Write(one)
    } else {
        h.Write(zero)
    }

    var qtypeBytes [2]byte
    binary.BigEndian.PutUint16(qtypeBytes[:], qtype)
    h.Write(qtypeBytes[:])
    h.Write([]byte(qname))
    return h.Sum64()
}
```

**캐시 키 구성 요소**:

```
캐시 키 = FNV-64( DO비트 + CD비트 + Qtype + Qname )

예시:
  쿼리: example.com. A (DO=0, CD=0)
  키:   FNV-64("0" + "0" + [0x00, 0x01] + "example.com.")

  쿼리: example.com. A (DO=1, CD=0)
  키:   FNV-64("1" + "0" + [0x00, 0x01] + "example.com.")
  → DO 비트가 다르면 다른 캐시 항목!
```

**왜 DO와 CD 비트를 키에 포함하는가?**

- **DO (DNSSEC OK)**: DNSSEC을 요청하면 응답에 RRSIG 등 추가 레코드가 포함된다. DO=0인 쿼리에 DNSSEC 레코드를 반환하면 안 된다.
- **CD (Checking Disabled)**: DNSSEC 검증을 비활성화한 응답은 검증된 응답과 다를 수 있다.

**왜 FNV-64를 사용하는가?**

FNV(Fowler-Noll-Vo) 해시는:
1. 매우 빠르다 (암호학적 해시보다 훨씬 빠름)
2. 분포가 균일하다
3. 64비트 출력으로 충돌 확률이 극히 낮다
4. 스트리밍 방식으로 메모리 할당이 최소화된다

---

## 4. item 구조체

### 4.1 정의

```
// plugin/cache/item.go:13-28
type item struct {
    Name               string
    QType              uint16
    Rcode              int
    AuthenticatedData  bool
    RecursionAvailable bool
    Answer             []dns.RR
    Ns                 []dns.RR
    Extra              []dns.RR
    wildcard           string

    origTTL uint32
    stored  time.Time

    *freq.Freq
}
```

주요 필드 분석:

| 필드 | 역할 |
|------|------|
| `Name` | 쿼리 이름 (Question 섹션에서) |
| `QType` | 쿼리 타입 |
| `Rcode` | 응답 코드 (NOERROR, NXDOMAIN 등) |
| `AuthenticatedData` | DNSSEC AD 비트 |
| `RecursionAvailable` | RA 비트 |
| `Answer` | Answer 섹션 RR 목록 |
| `Ns` | Authority 섹션 RR 목록 |
| `Extra` | Additional 섹션 RR 목록 (OPT 제외) |
| `wildcard` | 와일드카드 소스 레코드 이름 |
| `origTTL` | 캐싱 시점의 TTL (초) |
| `stored` | 캐싱 시점의 타임스탬프 |
| `*freq.Freq` | 프리페치용 빈도 추적기 |

### 4.2 newItem 함수

```
// plugin/cache/item.go:30-59
func newItem(m *dns.Msg, now time.Time, d time.Duration) *item {
    i := new(item)
    if len(m.Question) != 0 {
        i.Name = m.Question[0].Name
        i.QType = m.Question[0].Qtype
    }
    i.Rcode = m.Rcode
    i.AuthenticatedData = m.AuthenticatedData
    i.RecursionAvailable = m.RecursionAvailable
    i.Answer = m.Answer
    i.Ns = m.Ns
    // Extra 섹션에서 OPT 레코드 제외
    i.Extra = make([]dns.RR, len(m.Extra))
    j := 0
    for _, e := range m.Extra {
        if e.Header().Rrtype == dns.TypeOPT {
            continue  // OPT 레코드는 hop-by-hop이므로 캐싱하지 않음
        }
        i.Extra[j] = e
        j++
    }
    i.Extra = i.Extra[:j]

    i.origTTL = uint32(d.Seconds())
    i.stored = now.UTC()
    i.Freq = new(freq.Freq)

    return i
}
```

**왜 OPT 레코드를 제외하는가?**

OPT 레코드(EDNS0)는 hop-by-hop 프로토콜 메타데이터이다. 클라이언트별로 다른 EDNS0 옵션을 가질 수 있으므로 캐싱 대상이 아니다.

### 4.3 TTL 계산

```
// plugin/cache/item.go:93-96
func (i *item) ttl(now time.Time) int {
    ttl := int(i.origTTL) - int(now.UTC().Sub(i.stored).Seconds())
    return ttl
}
```

TTL은 **경과 시간**에 따라 동적으로 감소한다:

```
TTL = origTTL - (현재시간 - 캐싱시간)

예시:
  origTTL = 300초
  캐싱 시간: 10:00:00
  현재 시간: 10:02:00
  TTL = 300 - 120 = 180초
```

음수 TTL은 캐시 항목이 만료되었음을 의미하지만, `staleUpTo`가 설정되어 있으면 스테일 서빙에 사용된다.

### 4.4 matches 함수

```
// plugin/cache/item.go:98-103
func (i *item) matches(state request.Request) bool {
    if state.QType() == i.QType && strings.EqualFold(state.QName(), i.Name) {
        return true
    }
    return false
}
```

해시 충돌을 방지하기 위해 QType과 QName을 다시 비교한다. `EqualFold`로 대소문자를 구분하지 않는다.

### 4.5 toMsg - 캐시 항목을 DNS 메시지로 변환

```
// plugin/cache/item.go:68-91
func (i *item) toMsg(m *dns.Msg, now time.Time, do bool, ad bool) *dns.Msg {
    m1 := new(dns.Msg)
    m1.SetReply(m)
    m1.Authoritative = true  // 호환성을 위해 항상 true
    m1.AuthenticatedData = i.AuthenticatedData
    if !do && !ad {
        // DNSSEC을 요청하지 않으면 AD 비트 제거
        // 단, 요청자가 AD 비트를 설정한 경우는 유지 (RFC6840 5.7-5.8)
        m1.AuthenticatedData = false
    }
    m1.RecursionAvailable = i.RecursionAvailable
    m1.Rcode = i.Rcode

    ttl := uint32(i.ttl(now))
    m1.Answer = filterRRSlice(i.Answer, ttl, true)  // TTL 갱신 + 복제
    m1.Ns = filterRRSlice(i.Ns, ttl, true)
    m1.Extra = filterRRSlice(i.Extra, ttl, true)

    return m1
}
```

**왜 Authoritative를 항상 true로 설정하는가?**

코드 주석에 따르면, 일부 레거시 DNS 클라이언트(ubuntu 14.04의 glibc 2.20 등)는 Authoritative=false인 응답을 완전히 무시한다. 기술적으로는 캐시 응답은 Non-Authoritative이지만, 호환성을 위해 true로 설정한다.

---

## 5. ServeDNS 흐름

### 5.1 handler.go의 ServeDNS

`plugin/cache/handler.go:17-82`가 Cache 플러그인의 진입점이다.

```
// plugin/cache/handler.go:17-82
func (c *Cache) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
    rc := r.Copy()  // 원본 메시지 보호
    state := request.Request{W: w, Req: rc}
    do := state.Do()
    cd := r.CheckingDisabled
    ad := r.AuthenticatedData

    zone := plugin.Zones(c.Zones).Matches(state.Name())
    if zone == "" {
        return plugin.NextOrFailure(c.Name(), c.Next, ctx, w, rc)
    }

    now := c.now().UTC()
    server := metrics.WithServer(ctx)

    // 1. 캐시 조회
    i := c.getIfNotStale(now, state, server)
    if i == nil {
        // 캐시 미스 → 다음 플러그인 호출 (ResponseWriter 래핑)
        crr := &ResponseWriter{
            ResponseWriter: w, Cache: c, state: state, server: server,
            do: do, ad: ad, cd: cd,
            nexcept: c.nexcept, pexcept: c.pexcept,
            wildcardFunc: wildcardFunc(ctx),
        }
        return c.doRefresh(ctx, state, crr)
    }

    // 2. 캐시 히트
    ttl := i.ttl(now)
    if ttl < 0 {
        // 만료된 항목 → 스테일 서빙 분기
        if c.verifyStale {
            // verify 모드: 업스트림 확인 후 판단
            crr := &ResponseWriter{...}
            cw := newVerifyStaleResponseWriter(crr)
            ret, err := c.doRefresh(ctx, state, cw)
            if cw.refreshed {
                return ret, err  // 새 응답으로 대체
            }
        }
        // TTL 0으로 조정하여 스테일 응답 제공
        now = now.Add(time.Duration(ttl) * time.Second)
        if !c.verifyStale {
            cw := newPrefetchResponseWriter(server, state, c)
            go c.doPrefetch(ctx, state, cw, i, now)
        }
        servedStale.Inc()
    } else if c.shouldPrefetch(i, now) {
        // 프리페치 조건 충족 → 백그라운드 갱신
        cw := newPrefetchResponseWriter(server, state, c)
        go c.doPrefetch(ctx, state, cw, i, now)
    }

    // 3. keepttl 처리
    if c.keepttl {
        now = i.stored  // 원래 TTL 유지를 위해 시간을 캐싱 시점으로 조작
    }

    // 4. 응답 구성 및 전송
    resp := i.toMsg(r, now, do, ad)
    w.WriteMsg(resp)
    return dns.RcodeSuccess, nil
}
```

### 5.2 전체 흐름도

```
DNS 쿼리 수신
    │
    ▼
Zone 매칭 ──── 불일치 ──→ 다음 플러그인
    │
    │ 일치
    ▼
캐시 조회 (getIfNotStale)
    │
    ├── 캐시 미스 (nil)
    │   │
    │   ▼
    │   ResponseWriter 래핑
    │   │
    │   ▼
    │   다음 플러그인 호출 (doRefresh)
    │   │
    │   ▼
    │   ResponseWriter.WriteMsg()에서 응답 캐싱
    │   │
    │   └── 응답을 클라이언트에 전달
    │
    ├── 캐시 히트 (TTL > 0)
    │   │
    │   ├── shouldPrefetch()?
    │   │   └── Yes → 백그라운드 프리페치 (go doPrefetch)
    │   │
    │   ▼
    │   item.toMsg()로 응답 구성
    │   │
    │   └── w.WriteMsg() → 클라이언트에 캐시된 응답
    │
    └── 캐시 히트 (TTL < 0, 만료됨)
        │
        ├── staleUpTo 허용 범위 내?
        │   │
        │   ├── verifyStale?
        │   │   ├── Yes → 업스트림 확인
        │   │   │   ├── 성공 → 새 응답 반환
        │   │   │   └── 실패 → 스테일 응답 반환
        │   │   └── No → 스테일 응답 + 백그라운드 프리페치
        │   │
        │   └── TTL=0으로 스테일 응답 전송
        │
        └── 허용 범위 밖 → 캐시 미스와 동일
```

---

## 6. 캐시 조회 (getIfNotStale)

### 6.1 구현

```
// plugin/cache/handler.go:125-145
func (c *Cache) getIfNotStale(now time.Time, state request.Request, server string) *item {
    k := hash(state.Name(), state.QType(), state.Do(), state.Req.CheckingDisabled)
    cacheRequests.Inc()

    // 1. 음성 캐시 먼저 확인
    if i, ok := c.ncache.Get(k); ok {
        ttl := i.ttl(now)
        if i.matches(state) && (ttl > 0 || (c.staleUpTo > 0 && -ttl < int(c.staleUpTo.Seconds()))) {
            cacheHits.WithLabelValues(server, Denial).Inc()
            return i
        }
    }
    // 2. 양성 캐시 확인
    if i, ok := c.pcache.Get(k); ok {
        ttl := i.ttl(now)
        if i.matches(state) && (ttl > 0 || (c.staleUpTo > 0 && -ttl < int(c.staleUpTo.Seconds()))) {
            cacheHits.WithLabelValues(server, Success).Inc()
            return i
        }
    }
    cacheMisses.Inc()
    return nil
}
```

**왜 음성 캐시를 먼저 확인하는가?**

NXDOMAIN은 해당 이름이 존재하지 않음을 의미한다. 음성 캐시에 항목이 있으면, 양성 캐시를 확인할 필요가 없다. 또한 부정적 응답은 DNS 확장 공격의 대상이 될 수 있으므로, 빠르게 캐시된 부정적 응답을 반환하는 것이 중요하다.

**스테일 서빙 조건**:

```
ttl > 0                                       → 신선한 응답
(c.staleUpTo > 0 && -ttl < staleUpTo.Seconds()) → 스테일하지만 허용 범위
```

예시:
```
origTTL = 300, stored = 10:00:00
현재 = 10:06:00 → ttl = 300 - 360 = -60
staleUpTo = 1h → -(-60) = 60 < 3600 → 스테일 서빙 허용
```

### 6.2 exists 함수 (프리페치용)

```
// plugin/cache/handler.go:148-157
func (c *Cache) exists(state request.Request) *item {
    k := hash(state.Name(), state.QType(), state.Do(), state.Req.CheckingDisabled)
    if i, ok := c.ncache.Get(k); ok {
        return i
    }
    if i, ok := c.pcache.Get(k); ok {
        return i
    }
    return nil
}
```

`exists`는 `getIfNotStale`와 달리 만료 여부를 확인하지 않는다. 프리페치 후 빈도 정보를 복사하기 위해 사용된다.

---

## 7. ResponseWriter 래핑

### 7.1 ResponseWriter 구조체

```
// plugin/cache/cache.go:127-143
type ResponseWriter struct {
    dns.ResponseWriter
    *Cache
    state  request.Request
    server string

    do         bool    // 원본 요청의 DO 비트
    cd         bool    // 원본 요청의 CD 비트
    ad         bool    // 원본 요청의 AD 비트
    prefetch   bool    // true면 클라이언트에 응답하지 않음
    remoteAddr net.Addr

    wildcardFunc func() string

    pexcept []string  // 양성 캐시 제외 zone
    nexcept []string  // 음성 캐시 제외 zone
}
```

### 7.2 WriteMsg - 응답 가로채기 + 캐싱

```
// plugin/cache/cache.go:180-226
func (w *ResponseWriter) WriteMsg(res *dns.Msg) error {
    res = res.Copy()  // 원본 보호
    mt, _ := response.Typify(res, w.now().UTC())

    // 캐싱 가능한 키 확인
    hasKey, key := key(w.state.Name(), res, mt, w.do, w.cd)

    // TTL 계산
    msgTTL := dnsutil.MinimalTTL(res, mt)
    var duration time.Duration
    switch mt {
    case response.NameError, response.NoData:
        duration = computeTTL(msgTTL, w.minnttl, w.nttl)
    case response.ServerError:
        duration = w.failttl
    default:
        duration = computeTTL(msgTTL, w.minpttl, w.pttl)
    }

    // 응답의 모든 RR에 계산된 TTL 적용
    ttl := uint32(duration.Seconds())
    res.Answer = filterRRSlice(res.Answer, ttl, false)
    res.Ns = filterRRSlice(res.Ns, ttl, false)
    res.Extra = filterRRSlice(res.Extra, ttl, false)

    // DNSSEC 관련 AD 비트 조정
    if !w.do && !w.ad {
        res.AuthenticatedData = false
    }

    // 캐시에 저장
    if hasKey && duration > 0 {
        if w.state.Match(res) {
            w.set(res, key, mt, duration)
        } else {
            cacheDrops.Inc()
        }
    }

    // prefetch 모드면 클라이언트에 응답하지 않음
    if w.prefetch {
        return nil
    }

    return w.ResponseWriter.WriteMsg(res)
}
```

**캐싱 프록시 패턴**:

```
┌────────────┐    ┌──────────────┐    ┌───────────────┐
│ 클라이언트 │    │    Cache     │    │  다음 플러그인 │
│            │    │  플러그인    │    │  (forward 등)  │
└─────┬──────┘    └───────┬──────┘    └───────┬───────┘
      │                   │                   │
      │  DNS 쿼리         │                   │
      │──────────────────>│                   │
      │                   │                   │
      │                   │  캐시 미스         │
      │                   │  ┌────────────┐   │
      │                   │  │ResponseWriter│  │
      │                   │  │  래핑       │  │
      │                   │  └─────┬──────┘  │
      │                   │        │          │
      │                   │        │  쿼리     │
      │                   │        │─────────>│
      │                   │        │          │
      │                   │        │  응답     │
      │                   │        │<─────────│
      │                   │        │          │
      │                   │  WriteMsg()에서:   │
      │                   │  1. 응답 캐싱      │
      │                   │  2. 클라이언트 전달 │
      │   캐시된 응답      │                   │
      │<──────────────────│                   │
```

### 7.3 TTL 계산 (computeTTL)

```
// plugin/cache/cache.go:121-124
func computeTTL(msgTTL, minTTL, maxTTL time.Duration) time.Duration {
    ttl := min(max(msgTTL, minTTL), maxTTL)
    return ttl
}
```

```
최종 TTL = min(max(메시지TTL, 최소TTL), 최대TTL)

예시 (양성 캐시, minpttl=5s, pttl=3600s):
  메시지 TTL = 2s   → max(2, 5) = 5   → min(5, 3600) = 5s
  메시지 TTL = 300s  → max(300, 5) = 300 → min(300, 3600) = 300s
  메시지 TTL = 7200s → max(7200, 5) = 7200 → min(7200, 3600) = 3600s
```

### 7.4 set - 캐시 저장

```
// plugin/cache/cache.go:228-267
func (w *ResponseWriter) set(m *dns.Msg, key uint64, mt response.Type, duration time.Duration) {
    switch mt {
    case response.NoError, response.Delegation:
        if plugin.Zones(w.pexcept).Matches(m.Question[0].Name) != "" {
            return  // 제외 zone이면 캐싱하지 않음
        }
        i := newItem(m, w.now(), duration)
        if w.wildcardFunc != nil {
            i.wildcard = w.wildcardFunc()
        }
        if w.pcache.Add(key, i) {
            evictions.Inc()  // 기존 항목이 교체됨
        }
        // 프리페치 시: 양성 응답이 오면 음성 캐시에서 제거
        if w.prefetch {
            w.ncache.Remove(key)
        }

    case response.NameError, response.NoData, response.ServerError:
        if plugin.Zones(w.nexcept).Matches(m.Question[0].Name) != "" {
            return  // 제외 zone이면 캐싱하지 않음
        }
        i := newItem(m, w.now(), duration)
        if w.ncache.Add(key, i) {
            evictions.Inc()
        }

    case response.OtherError:
        // 캐싱하지 않음
    }
}
```

**프리페치 시 음성 캐시 제거**:

```
if w.prefetch {
    w.ncache.Remove(key)
}
```

프리페치로 양성 응답을 얻으면, 동일 키의 음성 캐시 항목을 제거한다. 이전에 NXDOMAIN이었던 이름이 나중에 생성될 수 있기 때문이다.

---

## 8. 프리페치 (Prefetch) 메커니즘

### 8.1 프리페치 개념

프리페치는 자주 조회되는 캐시 항목의 TTL이 만료되기 **전에** 백그라운드에서 미리 갱신하는 메커니즘이다. 이를 통해 클라이언트는 항상 신선한 캐시 응답을 받을 수 있다.

### 8.2 shouldPrefetch 판단

```
// plugin/cache/handler.go:112-119
func (c *Cache) shouldPrefetch(i *item, now time.Time) bool {
    if c.prefetch <= 0 {
        return false
    }
    i.Update(c.duration, now)
    threshold := int(math.Ceil(float64(c.percentage) / 100 * float64(i.origTTL)))
    return i.Hits() >= c.prefetch && i.ttl(now) <= threshold
}
```

프리페치 트리거 조건:
1. `prefetch > 0` (프리페치 활성화)
2. `Hits() >= prefetch` (충분한 조회 빈도)
3. `ttl(now) <= percentage% * origTTL` (TTL 잔여량이 임계값 이하)

```
예시: prefetch=2, duration=1m, percentage=10%, origTTL=300s

  TTL 잔여량 ≤ 30초 (300 * 10% = 30)
  AND
  최근 1분 내 2회 이상 조회됨
  → 프리페치 트리거!
```

### 8.3 Freq (빈도 추적기)

```
// plugin/cache/freq/freq.go:11-18
type Freq struct {
    last time.Time    // 마지막 조회 시간
    hits int          // 기간 내 조회 횟수
    sync.RWMutex
}
```

```
// plugin/cache/freq/freq.go:28-40
func (f *Freq) Update(d time.Duration, now time.Time) int {
    earliest := now.Add(-1 * d)
    f.Lock()
    defer f.Unlock()
    if f.last.Before(earliest) {
        // 기간 밖: 카운터 리셋
        f.last = now
        f.hits = 1
        return f.hits
    }
    // 기간 내: 카운터 증가
    f.last = now
    f.hits++
    return f.hits
}
```

**슬라이딩 윈도우 방식**:

```
시간: ──────────────────────────────────────>
         |<─── duration (1분) ───>|
         │                        │ now
         │                        │
 이전 조회들이 이 기간 내에      현재 조회
 있으면 hits 누적, 아니면 리셋
```

### 8.4 doPrefetch 실행

```
// plugin/cache/handler.go:94-106
func (c *Cache) doPrefetch(ctx context.Context, state request.Request, cw *ResponseWriter, i *item, now time.Time) {
    ctx = metadata.ContextWithMetadata(ctx)
    cachePrefetches.Inc()
    c.doRefresh(ctx, state, cw)

    // 프리페치 후 빈도 정보 복원
    if i1 := c.exists(state); i1 != nil {
        i1.Reset(now, i.Hits())
    }
}
```

**왜 빈도 정보를 복원하는가?**

프리페치로 새 item이 캐시에 저장되면 Freq가 초기화된다. 기존 item의 조회 빈도(Hits)를 새 item에 복사하여, 다음 프리페치 판단에서 빈도 정보가 유실되지 않게 한다.

### 8.5 프리페치 흐름도

```
┌──────────────────────────────────────────────────────────────┐
│                    프리페치 메커니즘                          │
│                                                              │
│  1. 캐시 히트 (TTL > 0)                                     │
│     │                                                        │
│     ▼                                                        │
│  2. shouldPrefetch() 판단                                    │
│     │                                                        │
│     ├── Hits >= prefetch?                                    │
│     │   └── No → 프리페치 안 함                              │
│     │                                                        │
│     ├── TTL ≤ percentage% * origTTL?                         │
│     │   └── No → 프리페치 안 함                              │
│     │                                                        │
│     └── 두 조건 모두 충족                                    │
│         │                                                    │
│         ▼                                                    │
│  3. 백그라운드 고루틴으로 doPrefetch() 실행                  │
│     │                                                        │
│     ├── 새 메타데이터 컨텍스트 생성                           │
│     ├── doRefresh() → 업스트림 쿼리                          │
│     └── 새 item에 기존 빈도 정보 복원                        │
│                                                              │
│  4. 동시에 클라이언트에는 캐시된 응답 즉시 반환               │
│                                                              │
│  시간축:                                                     │
│  ──────────────────────────────────────────────>              │
│  |cached|          |prefetch zone|expire|                    │
│          ←───────→ ←──────────→                              │
│          fresh       percentage%                             │
│                    ↑ 여기서 프리페치                          │
│                      (아직 만료 전)                           │
└──────────────────────────────────────────────────────────────┘
```

### 8.6 PrefetchResponseWriter

```
// plugin/cache/cache.go:148-169
func newPrefetchResponseWriter(server string, state request.Request, c *Cache) *ResponseWriter {
    addr := state.W.RemoteAddr()
    // UDP → TCP로 변환하여 불필요한 Truncation 방지
    if u, ok := addr.(*net.UDPAddr); ok {
        addr = &net.TCPAddr{IP: u.IP, Port: u.Port, Zone: u.Zone}
    }
    return &ResponseWriter{
        ResponseWriter: state.W,
        Cache:          c,
        state:          state,
        server:         server,
        do:             state.Do(),
        cd:             state.Req.CheckingDisabled,
        prefetch:       true,       // WriteMsg에서 클라이언트에 응답하지 않음
        remoteAddr:     addr,
    }
}
```

**왜 UDP를 TCP로 변환하는가?**

프리페치 쿼리는 원래 연결이 이미 닫힌 후에 실행될 수 있다. `request.Proto`는 RemoteAddr의 타입으로 프로토콜을 판단하는데, TCP로 설정하면 응답 크기 제한이 풀려 불필요한 Truncation을 방지한다.

**prefetch=true의 효과**:

```
// plugin/cache/cache.go:221-223
if w.prefetch {
    return nil  // 클라이언트에 아무것도 보내지 않음
}
```

---

## 9. 스테일 서빙 (Stale Serving)

### 9.1 스테일 서빙 개념

RFC 8767(Serving Stale Data to Improve DNS Resiliency)을 구현한다. 캐시가 만료되어도 일정 기간(`staleUpTo`) 동안 만료된 캐시를 TTL=0으로 제공한다.

### 9.2 두 가지 모드

```
serve_stale 1h            # immediate 모드 (기본)
serve_stale 1h verify     # verify 모드
```

#### Immediate 모드 (기본)

```
// plugin/cache/handler.go:55-61
if ttl < 0 {
    // ...
    now = now.Add(time.Duration(ttl) * time.Second)  // TTL=0 조정
    if !c.verifyStale {
        cw := newPrefetchResponseWriter(server, state, c)
        go c.doPrefetch(ctx, state, cw, i, now)  // 백그라운드 갱신
    }
    servedStale.Inc()
}
```

즉시 스테일 응답(TTL=0)을 반환하고, 백그라운드에서 업스트림을 확인한다.

#### Verify 모드

```
// plugin/cache/handler.go:46-53
if c.verifyStale {
    crr := &ResponseWriter{...}
    cw := newVerifyStaleResponseWriter(crr)
    ret, err := c.doRefresh(ctx, state, cw)
    if cw.refreshed {
        return ret, err  // 새 응답으로 대체
    }
    // 업스트림 확인 실패 → 스테일 응답 사용
}
```

먼저 업스트림을 확인하고, 성공하면 새 응답을 반환한다. 실패하면 스테일 응답을 사용한다.

### 9.3 verifyStaleResponseWriter

```
// plugin/cache/cache.go:281-304
type verifyStaleResponseWriter struct {
    *ResponseWriter
    refreshed bool
}

func (w *verifyStaleResponseWriter) WriteMsg(res *dns.Msg) error {
    w.refreshed = false
    if res.Rcode == dns.RcodeSuccess || res.Rcode == dns.RcodeNameError {
        w.refreshed = true
        return w.ResponseWriter.WriteMsg(res)  // 캐시에 저장 + 클라이언트에 전달
    }
    return nil  // SERVFAIL 등은 무시 → 스테일 응답 사용
}
```

RFC 8767, Section 4에 따라 NoError와 NXDomain만 캐시 갱신에 유효한 응답으로 간주한다.

### 9.4 TTL=0 조정 트릭

```
now = now.Add(time.Duration(ttl) * time.Second)
```

예시:
```
origTTL = 300, stored = 10:00:00
현재 = 10:06:00 → ttl = -60
조정: now = 10:06:00 + (-60s) = 10:05:00
toMsg에서: ttl = 300 - (10:05:00 - 10:00:00) = 0
```

이 트릭으로 `toMsg`가 TTL=0인 응답을 생성한다.

### 9.5 스테일 서빙 흐름 비교

```
┌──────────────────────────────────────────────────────────────┐
│             immediate 모드              verify 모드          │
│                                                              │
│  캐시 만료 감지                     캐시 만료 감지            │
│       │                                  │                   │
│       ▼                                  ▼                   │
│  즉시 스테일 응답 (TTL=0)          업스트림에 쿼리            │
│       │                                  │                   │
│       ▼                            ┌─────┴─────┐            │
│  백그라운드 프리페치              성공?       실패?           │
│  (go doPrefetch)                   │           │             │
│                                    ▼           ▼             │
│                              새 응답 반환  스테일 응답 반환   │
│                              (캐시 갱신)   (TTL=0)           │
│                                                              │
│  장점: 최소 지연              장점: 가능하면 신선한 응답      │
│  단점: 항상 스테일 응답       단점: 업스트림 지연 발생 가능    │
└──────────────────────────────────────────────────────────────┘
```

---

## 10. SERVFAIL 캐싱

### 10.1 failttl 설정

```
// plugin/cache/setup.go:197-213
case "servfail":
    d, err := time.ParseDuration(args[0])
    if d > 5*time.Minute {
        // RFC 2308: SERVFAIL 캐싱은 5분을 초과할 수 없음
        return nil, errors.New("caching SERVFAIL responses over 5 minutes is not permitted")
    }
    ca.failttl = d
```

### 10.2 적용

```
// plugin/cache/cache.go:189-196
switch mt {
case response.NameError, response.NoData:
    duration = computeTTL(msgTTL, w.minnttl, w.nttl)
case response.ServerError:
    duration = w.failttl  // SERVFAIL은 고정 TTL
default:
    duration = computeTTL(msgTTL, w.minpttl, w.pttl)
}
```

SERVFAIL은 `failttl`로 고정된 TTL을 사용한다. 다른 응답 타입과 달리 메시지의 TTL을 참조하지 않는다.

---

## 11. keepttl 옵션

### 11.1 동작

```
// plugin/cache/handler.go:74-78
if c.keepttl {
    now = i.stored  // 시간을 캐싱 시점으로 되돌림
}
resp := i.toMsg(r, now, do, ad)
```

`keepttl`이 활성화되면, 응답의 TTL이 항상 원래 캐싱된 값 그대로 반환된다:

```
일반 모드:    TTL = origTTL - (now - stored) → 시간에 따라 감소
keepttl 모드: TTL = origTTL - (stored - stored) = origTTL → 항상 동일
```

**사용 사례**: 특정 클라이언트가 TTL 카운트다운에 혼란을 겪는 경우, 또는 TTL을 일정하게 유지하고 싶을 때 사용한다.

---

## 12. setup.go - 플러그인 초기화

### 12.1 등록

```
// plugin/cache/setup.go:19
func init() { plugin.Register("cache", setup) }
```

### 12.2 cacheParse 함수

```
// plugin/cache/setup.go:40-261
func cacheParse(c *caddy.Controller) (*Cache, error) {
    ca := New()

    // cache [ttl] [zones..]
    args := c.RemainingArgs()
    if len(args) > 0 {
        ttl, err := strconv.Atoi(args[0])
        if err == nil {
            ca.pttl = time.Duration(ttl) * time.Second
            ca.nttl = time.Duration(ttl) * time.Second
            args = args[1:]
        }
    }
    origins := plugin.OriginsFromArgsOrServerBlock(args, c.ServerBlockKeys)

    for c.NextBlock() {
        switch c.Val() {
        case Success:     // success cap [ttl [minttl]]
        case Denial:      // denial cap [ttl [minttl]]
        case "prefetch":  // prefetch amount [duration [percentage%]]
        case "serve_stale": // serve_stale [duration [immediate|verify]]
        case "servfail":  // servfail duration
        case "disable":   // disable success|denial [zones...]
        case "keepttl":   // keepttl
        }
    }

    ca.Zones = origins
    ca.pcache = cache.New[*item](ca.pcap)
    ca.ncache = cache.New[*item](ca.ncap)
}
```

### 12.3 Corefile 설정 상세

#### Success (양성 캐시) 설정

```
cache {
    success 10000 3600 5
    # success <용량> [<최대TTL> [<최소TTL>]]
    # 용량: 최대 캐시 항목 수
    # 최대TTL: 이 값을 초과하는 TTL은 잘림
    # 최소TTL: 이 값 미만의 TTL은 올림
}
```

#### Denial (음성 캐시) 설정

```
cache {
    denial 5000 1800 5
    # denial <용량> [<최대TTL> [<최소TTL>]]
}
```

#### Prefetch 설정

```
cache {
    prefetch 2 1m 10%
    # prefetch <히트임계값> [<측정기간>] [<TTL잔여비율>]
    # 히트임계값: 이 횟수 이상 조회되어야 프리페치
    # 측정기간: 히트를 측정하는 시간 윈도우
    # TTL잔여비율: TTL이 이 비율 이하로 남으면 프리페치
}
```

#### Serve Stale 설정

```
cache {
    serve_stale 1h immediate   # 즉시 스테일 응답 + 백그라운드 갱신
    serve_stale 1h verify      # 업스트림 확인 후 판단
}
```

#### Disable 설정

```
cache {
    disable success example.com  # example.com 양성 캐시 비활성화
    disable denial internal.     # internal. 음성 캐시 비활성화
}
```

### 12.4 전체 설정 예시

```
.:53 {
    cache 30 {
        success 20000 3600 10
        denial 5000 1800 5
        prefetch 3 1m 20%
        serve_stale 1h verify
        servfail 30s
        disable denial internal.corp.
        keepttl
    }
    forward . 8.8.8.8
}
```

---

## 13. 와일드카드 처리

### 13.1 wildcardFunc

```
// plugin/cache/handler.go:84-92
func wildcardFunc(ctx context.Context) func() string {
    return func() string {
        if f := metadata.ValueFunc(ctx, "zone/wildcard"); f != nil {
            return f()
        }
        return ""
    }
}
```

와일드카드 레코드(예: `*.example.com`)로 합성된 응답을 캐싱할 때, 원본 와일드카드 이름을 메타데이터로 저장한다.

```
// item에 저장
i.wildcard = w.wildcardFunc()

// 캐시 히트 시 복원
if i.wildcard != "" {
    metadata.SetValueFunc(ctx, "zone/wildcard", func() string {
        return i.wildcard
    })
}
```

---

## 14. 메트릭

Cache 플러그인이 제공하는 Prometheus 메트릭:

| 메트릭 | 설명 |
|--------|------|
| `coredns_cache_requests_total` | 캐시 조회 요청 수 |
| `coredns_cache_hits_total` | 캐시 히트 수 (success/denial별) |
| `coredns_cache_misses_total` | 캐시 미스 수 |
| `coredns_cache_drops_total` | 캐시 저장 거부 수 (응답 불일치) |
| `coredns_cache_served_stale_total` | 스테일 응답 제공 수 |
| `coredns_cache_prefetch_total` | 프리페치 실행 수 |
| `coredns_cache_evictions_total` | 캐시 교체(eviction) 수 |
| `coredns_cache_size` | 현재 캐시 크기 (success/denial별) |

---

## 15. 성능 최적화 설계

### 15.1 FNV-64 해시

분산이 균일한 비암호화 해시로 빠른 키 생성. 메모리 할당 최소화.

### 15.2 Copy-on-Write 응답

```
res = res.Copy()  // WriteMsg에서
rc := r.Copy()    // ServeDNS에서
```

원본 메시지를 복사한 후 수정하여, 다른 플러그인이 같은 메시지를 참조해도 영향을 받지 않는다.

### 15.3 음성 캐시 우선 조회

NXDOMAIN은 해당 이름 전체가 존재하지 않음을 의미하므로, 양성 캐시 조회를 건너뛸 수 있다.

### 15.4 Lazy TTL 계산

TTL은 저장 시 고정되지 않고, 조회 시 현재 시간과 저장 시간의 차이로 동적 계산된다. 주기적 TTL 갱신이 불필요하다.

### 15.5 OPT 레코드 제외

hop-by-hop 데이터인 OPT 레코드를 캐싱에서 제외하여 메모리를 절약하고, 클라이언트별 EDNS0 호환성 문제를 방지한다.

---

## 16. 에러 처리와 안전장치

### 16.1 응답 불일치 감지

```
if w.state.Match(res) {
    w.set(res, key, mt, duration)
} else {
    cacheDrops.Inc()  // 불일치 응답은 캐싱하지 않음
}
```

### 16.2 SERVFAIL 캐싱 제한

RFC 2308에 따라 SERVFAIL 캐싱은 5분을 초과할 수 없다:
```
if d > 5*time.Minute {
    return nil, errors.New("caching SERVFAIL responses over 5 minutes is not permitted")
}
```

### 16.3 프리페치 메타데이터 격리

```
ctx = metadata.ContextWithMetadata(ctx)
```

프리페치 고루틴은 새로운 메타데이터 맵을 생성하여 원래 요청의 메타데이터와의 동시 쓰기 충돌을 방지한다.

---

## 17. 캐시 항목 수명주기

```
┌──────────────────────────────────────────────────────────────┐
│                  캐시 항목 수명주기                           │
│                                                              │
│  시간축:                                                     │
│  ──────────────────────────────────────────────>              │
│  │stored│           │prefetch│expire│staleUpTo│              │
│  │      │           │ zone   │      │         │              │
│  ├──────┼───────────┼────────┼──────┼─────────┤              │
│                                                              │
│  구간별 동작:                                                │
│                                                              │
│  [stored ~ expire-threshold]                                 │
│    → 캐시 히트 시 즉시 응답 (TTL 감소)                       │
│    → 프리페치 조건 불충족                                    │
│                                                              │
│  [expire-threshold ~ expire]  (프리페치 zone)                │
│    → 캐시 히트 시 즉시 응답 (TTL 감소)                       │
│    → 빈도 충족 시 백그라운드 프리페치                        │
│    → 프리페치 성공 시 origTTL 갱신                           │
│                                                              │
│  [expire ~ expire+staleUpTo]  (스테일 zone)                  │
│    → TTL < 0 (만료됨)                                       │
│    → immediate: TTL=0 응답 + 백그라운드 갱신                 │
│    → verify: 업스트림 확인 후 판단                           │
│                                                              │
│  [expire+staleUpTo ~ ∞]                                     │
│    → 캐시 미스                                               │
│    → 정상적인 업스트림 쿼리                                  │
└──────────────────────────────────────────────────────────────┘
```

---

## 18. 정리

```
┌──────────────────────────────────────────────────────────────┐
│                   Cache 플러그인 아키텍처                     │
│                                                              │
│  Corefile                                                    │
│  cache 30 { success 20000; prefetch 2 1m 10% }              │
│       │                                                      │
│       ▼                                                      │
│  setup() → cacheParse()                                      │
│       │                                                      │
│       ▼                                                      │
│  Cache 인스턴스 생성                                         │
│    ├── pcache (양성 캐시, 20000 항목)                        │
│    ├── ncache (음성 캐시, 10000 항목)                        │
│    └── 설정: TTL, 프리페치, 스테일 등                        │
│                                                              │
│  DNS 쿼리 처리:                                              │
│  ServeDNS()                                                  │
│    ├── Zone 매칭                                             │
│    ├── getIfNotStale() → hash → ncache/pcache 조회           │
│    │   │                                                     │
│    │   ├── 캐시 미스                                         │
│    │   │   └── ResponseWriter 래핑 → 다음 플러그인           │
│    │   │       └── WriteMsg()에서 응답 캐싱                  │
│    │   │                                                     │
│    │   ├── 캐시 히트 (신선)                                  │
│    │   │   ├── shouldPrefetch()? → 백그라운드 갱신            │
│    │   │   └── toMsg() → 클라이언트 응답                     │
│    │   │                                                     │
│    │   └── 캐시 히트 (스테일)                                │
│    │       ├── verify? → 업스트림 확인                       │
│    │       └── immediate? → TTL=0 응답 + 프리페치            │
│    │                                                         │
│    └── 응답 전송                                             │
│                                                              │
│  캐시 키: FNV-64(DO + CD + Qtype + Qname)                   │
│  TTL 계산: min(max(msgTTL, minTTL), maxTTL)                 │
│  프리페치: Hits >= threshold && TTL <= percentage% * origTTL │
└──────────────────────────────────────────────────────────────┘
```

Cache 플러그인의 핵심 설계 결정:
1. **이중 캐시**: 양성/음성 응답을 분리하여 독립적 TTL과 용량 관리
2. **FNV-64 해시**: DO/CD 비트를 포함하여 DNSSEC 응답 분리
3. **ResponseWriter 래핑**: 투명한 캐싱 -- 다음 플러그인의 수정 불필요
4. **프리페치**: 자주 조회되는 항목을 만료 전에 갱신하여 항상 신선한 응답 제공
5. **스테일 서빙**: RFC 8767 구현으로 업스트림 장애 시에도 서비스 연속성 보장
6. **Lazy TTL**: 저장 시 고정하지 않고 조회 시 동적 계산으로 오버헤드 최소화
