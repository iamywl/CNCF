# 13. File 플러그인 Deep-Dive

## 개요

CoreDNS의 `file` 플러그인은 RFC 1035 형식의 Zone 파일을 디스크에서 읽어 권한(Authoritative) DNS 서버로 동작하게 하는 핵심 플러그인이다. BIND의 `named.conf`에서 `type master`/`type slave`로 Zone을 서빙하는 것과 동일한 역할을 수행한다.

이 플러그인은 다음과 같은 핵심 기능을 제공한다:

- Zone 파일 파싱 및 메모리 내 트리 구조 저장
- DNS 쿼리에 대한 Authoritative 응답 (Lookup)
- Zone 파일 자동 리로드 (주기적 재파싱)
- Secondary Zone 지원 (AXFR/IXFR 수신, NOTIFY 처리)
- Delegation, CNAME/DNAME 체이닝, Wildcard 확장
- DNSSEC 서명(RRSIG, NSEC) 처리
- Fall-through 지원

소스 경로: `plugin/file/`

---

## 핵심 데이터 구조

### File 구조체

```
// plugin/file/file.go

type File struct {
    Next plugin.Handler
    Zones
    Xfer *transfer.Transfer
    Fall fall.F
}
```

| 필드 | 타입 | 역할 |
|------|------|------|
| `Next` | `plugin.Handler` | 플러그인 체인의 다음 핸들러 |
| `Zones` | `Zones` | Zone 이름 → Zone 데이터 매핑 |
| `Xfer` | `*transfer.Transfer` | Zone 전송(transfer) 플러그인 참조 |
| `Fall` | `fall.F` | Fall-through 설정 (NXDOMAIN 시 다음 플러그인으로 전달) |

`File` 구조체는 `plugin.Handler` 인터페이스를 구현하며, `ServeDNS()` 메서드를 통해 DNS 요청을 처리한다.

### Zones 구조체

```
// plugin/file/file.go

type Zones struct {
    Z     map[string]*Zone   // zone origin → Zone 데이터 매핑
    Names []string           // Z 맵의 키를 문자열 슬라이스로 보관
}
```

하나의 `file` 플러그인 인스턴스가 여러 Zone을 서빙할 수 있다. `Names` 슬라이스는 `plugin.Zones().Matches()` 호출 시 사용되며, 가장 긴 접미사 매칭을 통해 적합한 Zone을 찾는다.

### Zone 구조체

```
// plugin/file/zone.go

type Zone struct {
    origin  string           // Zone의 원점 (예: "example.org.")
    origLen int              // origin의 레이블 수
    file    string           // Zone 파일 경로
    *tree.Tree               // DNS 레코드를 저장하는 LLRB 트리
    Apex                     // SOA, NS 등 Apex 레코드
    Expired bool             // Zone 만료 여부

    sync.RWMutex             // 동시성 제어

    StartupOnce  sync.Once   // 시작 시 한 번만 실행 보장
    TransferFrom []string    // 프라이머리 서버 목록 (secondary 모드)

    ReloadInterval time.Duration  // 자동 리로드 주기
    reloadShutdown chan bool       // 리로드 고루틴 종료 채널

    Upstream *upstream.Upstream   // 외부 이름 해석용 upstream
}
```

**핵심 설계 포인트:**

1. **내장 RWMutex**: Zone 데이터에 대한 읽기/쓰기 동시성 제어. 다수의 DNS 쿼리가 동시에 `Lookup()`을 호출할 수 있으므로 `RLock()`을 사용하고, Zone 리로드나 전송 시에만 `Lock()`을 사용한다.

2. **tree.Tree 임베딩**: LLRB(Left-Leaning Red-Black) 트리를 직접 임베딩하여 `z.Search()`, `z.Insert()` 등을 Zone 레벨에서 직접 호출할 수 있다.

3. **StartupOnce**: 서버 시작 시 `Reload()` 또는 `TransferIn()`을 정확히 한 번만 호출하도록 보장한다.

### Apex 구조체

```
// plugin/file/zone.go

type Apex struct {
    SOA    *dns.SOA       // Zone의 SOA 레코드
    NS     []dns.RR       // Zone의 NS 레코드들
    SIGSOA []dns.RR       // SOA의 RRSIG
    SIGNS  []dns.RR       // NS의 RRSIG
}
```

Apex 레코드(SOA, NS)는 트리에 저장하지 않고 별도 구조체로 분리한다. 이유는:

- SOA는 모든 NXDOMAIN, NODATA 응답의 Authority 섹션에 포함되어 매우 빈번하게 접근된다
- NS 레코드는 모든 성공 응답의 Authority 섹션에 포함된다
- 트리 검색 없이 O(1)로 접근할 수 있어야 한다

```
func (a Apex) soa(do bool) []dns.RR {
    if do {
        ret := append([]dns.RR{a.SOA}, a.SIGSOA...)
        return ret
    }
    return []dns.RR{a.SOA}
}

func (a Apex) ns(do bool) []dns.RR {
    if do {
        ret := append(a.NS, a.SIGNS...)
        return ret
    }
    return a.NS
}
```

`do` 파라미터는 DNSSEC OK 비트로, 설정 시 RRSIG까지 함께 반환한다.

---

## LLRB 트리 구조 (tree 패키지)

### 트리 설계

```
// plugin/file/tree/tree.go

// Left-Leaning Red Black 트리
// Robert Sedgewick의 LLRB 알고리즘 기반
// DNS Zone 데이터 저장에 맞게 수정

type Tree struct {
    Root  *Node   // 루트 노드
    Count int     // 저장된 요소 수
}

type Node struct {
    Elem        *Elem           // DNS 레코드 요소
    Left, Right *Node           // 자식 노드
    Color       Color           // Red 또는 Black
}

type Color bool
const (
    red   Color = false   // 새 노드는 기본적으로 red
    black Color = true
)
```

**LLRB 트리를 선택한 이유:**

1. **균형 보장**: 일반 BST와 달리 최악의 경우에도 O(log n) 검색 보장
2. **구현 단순성**: AVL 트리나 일반 Red-Black 트리보다 구현이 간결
3. **DNS에 적합**: Zone 데이터는 한 번 로드 후 읽기가 대부분이므로, 삽입 성능보다 검색 성능이 중요

### 작동 모드

```
const (
    td234 = iota    // Top-Down 2-3-4 모드
    bu23            // Bottom-Up 2-3 모드
)

const mode = bu23   // CoreDNS는 bu23 모드 사용
```

BU23(Bottom-Up 2-3) 모드는 삽입 시 색상 뒤집기를 bottom-up으로 수행하여, TD234보다 회전 횟수가 적다.

### Elem (트리 요소)

```
// plugin/file/tree/elem.go

type Elem struct {
    m    map[uint16][]dns.RR   // RR 타입 → RR 레코드 목록
    name string                 // 소유자 이름 (owner name)
}
```

하나의 `Elem`은 **같은 이름(owner name)을 가진 모든 RR 타입**을 저장한다. 예를 들어 `www.example.org.`에 A, AAAA, RRSIG 레코드가 있다면:

```
Elem {
    name: "www.example.org."
    m: {
        dns.TypeA:     [A 레코드들...],
        dns.TypeAAAA:  [AAAA 레코드들...],
        dns.TypeRRSIG: [RRSIG 레코드들...],
    }
}
```

이 설계는 DNS 쿼리의 특성에 최적화되어 있다:
- 쿼리는 항상 (name, type) 쌍으로 들어온다
- 트리에서 name으로 O(log n) 검색 후, type으로 O(1) 맵 조회

### 트리 검색

```
// plugin/file/tree/tree.go

func (t *Tree) Search(qname string) (*Elem, bool) {
    if t.Root == nil {
        return nil, false
    }
    n, res := t.Root.search(qname)
    if n == nil {
        return nil, res
    }
    return n.Elem, res
}

func (n *Node) search(qname string) (*Node, bool) {
    for n != nil {
        switch c := Less(n.Elem, qname); {
        case c == 0:
            return n, true
        case c < 0:
            n = n.Left
        default:
            n = n.Right
        }
    }
    return n, false
}
```

검색은 반복문으로 구현되어 재귀 호출의 스택 오버헤드가 없다.

### DNSSEC 정규 순서 (Canonical Ordering)

```
// plugin/file/tree/less.go

// RFC 4034 Section 6.1에 따른 DNSSEC 정규 순서
func less(a, b string) int {
    aj := len(a)
    bj := len(b)
    for {
        ai, oka := dns.PrevLabel(a[:aj], 1)
        bi, okb := dns.PrevLabel(b[:bj], 1)
        if oka && okb {
            return 0
        }
        // 레이블별 비교 (소문자 변환 후)
        ab := []byte(strings.ToLower(a[ai:aj]))
        bb := []byte(strings.ToLower(b[bi:bj]))
        doDDD(ab)
        doDDD(bb)
        res := bytes.Compare(ab, bb)
        if res != 0 {
            return res
        }
        aj, bj = ai, bi
    }
}
```

**왜 이 순서가 중요한가?**

DNSSEC 검증을 위해 NSEC/NSEC3 레코드의 "다음 이름(next name)" 필드는 정규 순서를 따라야 한다. 트리가 이 순서대로 정렬되면, `Prev()`와 `Next()` 메서드로 NSEC 체인의 이전/다음 요소를 즉시 찾을 수 있다.

### Glue 레코드 검색

```
// plugin/file/tree/glue.go

func (t *Tree) Glue(nsrrs []dns.RR, do bool) []dns.RR {
    glue := []dns.RR{}
    for _, rr := range nsrrs {
        if ns, ok := rr.(*dns.NS); ok && dns.IsSubDomain(ns.Header().Name, ns.Ns) {
            glue = append(glue, t.searchGlue(ns.Ns, do)...)
        }
    }
    return glue
}
```

NS 레코드가 가리키는 네임서버가 같은 Zone 내에 있을 때(in-bailiwick), 해당 네임서버의 A/AAAA 레코드를 Glue로 Additional 섹션에 포함한다.

---

## Zone 파일 파싱

### Parse 함수

```
// plugin/file/file.go

func Parse(f io.Reader, origin, fileName string, serial int64) (*Zone, error) {
    zp := dns.NewZoneParser(f, dns.Fqdn(origin), fileName)
    zp.SetIncludeAllowed(true)   // $INCLUDE 지시자 허용
    z := NewZone(origin, fileName)
    seenSOA := false

    for rr, ok := zp.Next(); ok; rr, ok = zp.Next() {
        if !seenSOA {
            if s, ok := rr.(*dns.SOA); ok {
                seenSOA = true
                // 시리얼 비교: 변경 없으면 리로드 불필요
                if serial >= 0 && s.Serial == uint32(serial) {
                    return nil, &serialErr{...}
                }
            }
        }
        if err := z.Insert(rr); err != nil {
            return nil, err
        }
    }

    if !seenSOA {
        return nil, fmt.Errorf("file %q has no SOA record for origin %s", fileName, origin)
    }
    return z, nil
}
```

**파싱 흐름:**

```
Zone 파일 (RFC 1035 형식)
         |
         v
dns.NewZoneParser()  ← miekg/dns 라이브러리의 Zone 파서
         |
         v
RR 순회: zp.Next() 반복
         |
    +-----------+
    | SOA 확인  |──── 시리얼 비교 (변경 없으면 에러 반환)
    +-----------+
         |
         v
    z.Insert(rr)  ──── Zone 구조체에 삽입
         |
         v
  [SOA 없으면 에러]
         |
         v
    *Zone 반환
```

### Insert 메서드: 레코드 타입별 분기

```
// plugin/file/zone.go

func (z *Zone) Insert(r dns.RR) error {
    // SRV를 제외한 모든 이름을 소문자로 정규화
    if r.Header().Rrtype != dns.TypeSRV {
        r.Header().Name = strings.ToLower(r.Header().Name)
    }

    switch h := r.Header().Rrtype; h {
    case dns.TypeNS:
        // Apex NS는 별도 저장
        if r.Header().Name == z.origin {
            z.NS = append(z.NS, r)
            return nil
        }
    case dns.TypeSOA:
        z.SOA = r.(*dns.SOA)
        return nil
    case dns.TypeNSEC3, dns.TypeNSEC3PARAM:
        return fmt.Errorf("NSEC3 zone is not supported")
    case dns.TypeRRSIG:
        // SOA, NS의 서명은 Apex에 저장
        x := r.(*dns.RRSIG)
        switch x.TypeCovered {
        case dns.TypeSOA:
            z.SIGSOA = append(z.SIGSOA, x)
            return nil
        case dns.TypeNS:
            if r.Header().Name == z.origin {
                z.SIGNS = append(z.SIGNS, x)
                return nil
            }
        }
    case dns.TypeCNAME:
        r.(*dns.CNAME).Target = strings.ToLower(r.(*dns.CNAME).Target)
    case dns.TypeMX:
        r.(*dns.MX).Mx = strings.ToLower(r.(*dns.MX).Mx)
    }

    z.Tree.Insert(rr)   // 나머지는 트리에 삽입
    return nil
}
```

**레코드 분류 전략:**

```
+------------------+-------------------+
| 저장 위치        | 레코드 타입       |
+------------------+-------------------+
| Apex.SOA         | SOA               |
| Apex.NS          | Apex NS           |
| Apex.SIGSOA      | SOA RRSIG         |
| Apex.SIGNS       | Apex NS RRSIG     |
| Tree             | 그 외 모든 레코드 |
+------------------+-------------------+
| 거부(에러)       | NSEC3, NSEC3PARAM |
+------------------+-------------------+
```

NSEC3을 지원하지 않는 이유: NSEC3은 해시 기반으로 별도의 자료구조가 필요하며, file 플러그인은 NSEC만 지원한다.

---

## ServeDNS 흐름

### 전체 요청 처리 흐름

```
// plugin/file/file.go

func (f File) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
    state := request.Request{W: w, Req: r}
    qname := state.Name()

    // 1. Zone 매칭
    zone := plugin.Zones(f.Zones.Names).Matches(qname)
    if zone == "" {
        if f.Next == nil {
            return dns.RcodeRefused, nil
        }
        return plugin.NextOrFailure(f.Name(), f.Next, ctx, w, r)
    }

    z, ok := f.Z[zone]
    if !ok || z == nil {
        return dns.RcodeServerFailure, nil
    }

    // 2. AXFR/IXFR 요청 거부 (transfer 플러그인이 처리)
    if state.QType() == dns.TypeAXFR || state.QType() == dns.TypeIXFR {
        return dns.RcodeRefused, nil
    }

    // 3. NOTIFY 처리 (secondary zone)
    if r.Opcode == dns.OpcodeNotify {
        if z.isNotify(state) {
            // NOTIFY 응답 전송
            // shouldTransfer() → TransferIn()
        }
        return dns.RcodeSuccess, nil
    }

    // 4. Zone 만료 확인
    if z.Expired {
        return dns.RcodeServerFailure, nil
    }

    // 5. Lookup 실행
    answer, ns, extra, result := z.Lookup(ctx, state, qname)

    // 6. Fall-through 판단
    if len(answer) == 0 && (result == NameError || result == Success) && f.Fall.Through(qname) {
        return plugin.NextOrFailure(f.Name(), f.Next, ctx, w, r)
    }

    // 7. 응답 구성
    m := new(dns.Msg)
    m.SetReply(r)
    m.Authoritative = true
    m.Answer, m.Ns, m.Extra = answer, ns, extra

    switch result {
    case NameError:
        m.Rcode = dns.RcodeNameError
    case Delegation:
        m.Authoritative = false
    case ServerFailure:
        if len(m.Answer) == 0 {
            return dns.RcodeServerFailure, nil
        }
        m.Rcode = dns.RcodeServerFailure
    }

    w.WriteMsg(m)
    return dns.RcodeSuccess, nil
}
```

**요청 처리 시퀀스 다이어그램:**

```
Client          File Plugin           Zone              Tree
  |                 |                   |                  |
  |--- DNS Query -->|                   |                  |
  |                 |-- Zone Match ---->|                  |
  |                 |                   |                  |
  |                 |   [AXFR/IXFR?]   |                  |
  |                 |   → REFUSED       |                  |
  |                 |                   |                  |
  |                 |   [NOTIFY?]       |                  |
  |                 |   → shouldTransfer()                 |
  |                 |   → TransferIn()  |                  |
  |                 |                   |                  |
  |                 |   [Expired?]      |                  |
  |                 |   → SERVFAIL      |                  |
  |                 |                   |                  |
  |                 |--- Lookup() ----->|--- Search() ---->|
  |                 |                   |<-- Elem/nil -----|
  |                 |<-- answer/ns/extra|                  |
  |                 |                   |                  |
  |                 |   [Fall-through?] |                  |
  |                 |   → Next Plugin   |                  |
  |                 |                   |                  |
  |<-- Response ----|                   |                  |
```

---

## Lookup 메서드 상세 분석

### Result 타입

```
// plugin/file/lookup.go

type Result int

const (
    Success       Result = iota   // 성공 (NOERROR)
    NameError                     // NXDOMAIN
    Delegation                    // 위임 (비권한)
    NoData                        // NODATA (이름 존재, 타입 없음)
    ServerFailure                 // SERVFAIL
)
```

### Lookup 알고리즘

`Lookup()` 메서드는 DNS 이름 해석의 핵심 로직을 구현한다:

```
// plugin/file/lookup.go

func (z *Zone) Lookup(ctx context.Context, state request.Request, qname string) (
    []dns.RR, []dns.RR, []dns.RR, Result) {

    // 1. Apex 쿼리 최적화
    if qname == z.origin {
        switch qtype {
        case dns.TypeSOA:
            return ap.soa(do), ap.ns(do), nil, Success
        case dns.TypeNS:
            nsrrs := ap.ns(do)
            glue := tr.Glue(nsrrs, do)
            return nsrrs, nil, glue, Success
        }
    }

    // 2. 루프 감지 (CNAME 체이닝 보호)
    loop, _ := ctx.Value(dnsserver.LoopKey{}).(int)
    if loop > 8 {
        return nil, nil, nil, ServerFailure
    }

    // 3. 레이블별 순회 (오른쪽부터 왼쪽으로)
    for {
        parts, shot = z.nameFromRight(qname, i)
        if shot { break }

        elem, found = tr.Search(parts)
        if !found {
            // 와일드카드 검색
            wildcard := replaceWithAsteriskLabel(parts)
            if wild, found := tr.Search(wildcard); found {
                wildElem = wild
            }
            i++
            continue
        }

        // DNAME 처리
        if dnamerrs := elem.Type(dns.TypeDNAME); dnamerrs != nil {
            // CNAME 합성 후 외부 조회
        }

        // Delegation 처리
        if nsrrs := elem.Type(dns.TypeNS); nsrrs != nil {
            return nil, nsrrs, glue, Delegation
        }

        i++
    }
    // ... (이하 매칭 결과 처리)
}
```

**레이블별 순회(Label-by-Label Walk) 전략:**

```
쿼리: sub.www.example.org.
Zone: example.org.

순회 순서:
  i=0: "example.org."        → 트리 검색 (Apex, delegation 확인)
  i=1: "www.example.org."    → 트리 검색 (delegation 확인)
  i=2: "sub.www.example.org." → shot=true, 루프 종료

이 전략의 장점:
- Delegation을 놓치지 않음 (중간 레이블에서 NS 레코드 확인)
- Wildcard를 적절히 적용 (가장 가까운 wildcard 사용)
- Empty Non-Terminal 처리 가능
```

### CNAME 체이닝

```
// 완전히 일치하는 이름을 찾았을 때
if found && shot {
    if rrs := elem.Type(dns.TypeCNAME); len(rrs) > 0 && qtype != dns.TypeCNAME {
        ctx = context.WithValue(ctx, dnsserver.LoopKey{}, loop+1)
        return z.externalLookup(ctx, state, elem, rrs)
    }
    // ...
}
```

CNAME을 발견하면 `externalLookup()`을 호출하여 타겟을 재귀적으로 해석한다. 루프 카운터(`loop`)를 context에 저장하여 최대 8회까지만 허용한다.

### Wildcard 확장

```
// plugin/file/wildcard.go

func replaceWithAsteriskLabel(qname string) (wildcard string) {
    i, shot := dns.NextLabel(qname, 0)
    if shot {
        return ""
    }
    return "*." + qname[i:]
}
```

검색 중 정확히 일치하는 이름을 찾지 못하면, 각 레벨에서 `*.상위도메인` 형태로 와일드카드를 검색한다:

```
sub.www.example.org. → 검색 실패
*.www.example.org.   → 와일드카드 검색
```

와일드카드를 찾으면 `wildElem`에 저장해두고, 최종적으로 정확한 이름도 찾지 못했을 때 이 와일드카드를 사용한다:

```
// TypeForWildcard: 소유자 이름을 원래 쿼리 이름으로 교체
func (e *Elem) TypeForWildcard(qtype uint16, qname string) []dns.RR {
    rrs := e.m[qtype]
    if rrs == nil { return nil }
    copied := make([]dns.RR, len(rrs))
    for i := range rrs {
        copied[i] = dns.Copy(rrs[i])
        copied[i].Header().Name = qname   // *.example.org → sub.example.org
    }
    return copied
}
```

### DNAME 처리

```
// plugin/file/dname.go

func substituteDNAME(qname, owner, target string) string {
    if dns.IsSubDomain(owner, qname) && qname != owner {
        labels := dns.SplitDomainName(qname)
        labels = append(labels[0:len(labels)-dns.CountLabel(owner)],
                       dns.SplitDomainName(target)...)
        return dnsutil.Join(labels...)
    }
    return ""
}
```

DNAME은 RFC 6672에 따라 도메인 이름 전체를 다른 도메인으로 리다이렉트한다. CoreDNS는 DNAME을 발견하면 CNAME을 합성(synthesize)하여 클라이언트에 반환한다.

### Empty Non-Terminal 처리

```
// Lookup() 끝부분
rcode := NameError

if x, found := tr.Next(qname); found {
    if dns.IsSubDomain(qname, x.Name()) {
        rcode = Success   // NXDOMAIN이 아니라 빈 비단말
    }
}
```

예를 들어 `a.b.example.org.`가 존재하지만 `b.example.org.`에는 레코드가 없는 경우, `b.example.org.`는 Empty Non-Terminal이다. 이 경우 NXDOMAIN이 아닌 NOERROR(빈 응답)를 반환해야 한다.

### Additional Section 처리

```
func (z *Zone) additionalProcessing(answer []dns.RR, do bool) (extra []dns.RR) {
    for _, rr := range answer {
        name := ""
        switch x := rr.(type) {
        case *dns.SRV:
            name = x.Target
        case *dns.MX:
            name = x.Mx
        }
        if len(name) == 0 || !dns.IsSubDomain(z.origin, name) {
            continue
        }
        elem, _ := z.Search(name)
        if elem == nil { continue }
        // A/AAAA 레코드를 Additional 섹션에 추가
        for _, addr := range []uint16{dns.TypeA, dns.TypeAAAA} {
            if a := elem.Type(addr); a != nil {
                extra = append(extra, a...)
            }
        }
    }
    return extra
}
```

MX, SRV 레코드의 타겟이 같은 Zone 내에 있으면, 해당 타겟의 A/AAAA 레코드를 Additional 섹션에 자동으로 추가한다.

---

## Zone 자동 리로드

### Reload 메커니즘

```
// plugin/file/reload.go

func (z *Zone) Reload(t *transfer.Transfer) error {
    if z.ReloadInterval == 0 {
        return nil   // reload 0으로 설정하면 비활성화
    }
    tick := time.NewTicker(z.ReloadInterval)

    go func() {
        for {
            select {
            case <-tick.C:
                // 1. Zone 파일 열기
                reader, err := os.Open(filepath.Clean(zFile))

                // 2. 현재 SOA 시리얼 가져오기
                serial := z.SOASerialIfDefined()

                // 3. 파싱 (시리얼 변경 없으면 건너뛰기)
                zone, err := Parse(reader, z.origin, zFile, serial)

                // 4. 원자적 교체
                z.Lock()
                z.Apex = zone.Apex
                z.Tree = zone.Tree
                z.Unlock()

                // 5. 전송 플러그인에 NOTIFY
                if t != nil {
                    t.Notify(z.origin)
                }

            case <-z.reloadShutdown:
                tick.Stop()
                return
            }
        }
    }()
    return nil
}
```

**리로드 전략:**

```
+-------------------+       +---------+
| 주기적 타이머     |------>| 파일 열기|
| (ReloadInterval)  |       +---------+
+-------------------+            |
                                 v
                          +--------------+
                          | SOA 시리얼   |
                          | 비교         |
                          +--------------+
                           /           \
                     변경 없음      변경 있음
                         |              |
                    [건너뛰기]     +----------+
                                  | Parse()  |
                                  +----------+
                                       |
                                       v
                                +---------------+
                                | Lock()        |
                                | Apex/Tree교체 |
                                | Unlock()      |
                                +---------------+
                                       |
                                       v
                                +-----------+
                                | Notify()  |
                                +-----------+
```

**핵심 설계 포인트:**

1. **SOA 시리얼 비교**: 파일이 변경되었더라도 SOA 시리얼이 같으면 리로드하지 않는다. 이는 불필요한 메모리 할당과 CPU 사용을 방지한다.

2. **새 Zone 객체 생성 후 포인터 교체**: 기존 Zone을 수정하지 않고 새로운 Apex/Tree를 만들어 교체한다. 이 방식은 읽기 중인 고루틴에 영향을 주지 않는다 (RLock 해제 후 다음 읽기부터 새 데이터 사용).

3. **NOTIFY 전파**: 리로드 성공 시 transfer 플러그인을 통해 secondary 서버에 NOTIFY를 보내 동기화를 트리거한다.

### SOA 시리얼 안전 접근

```
func (z *Zone) SOASerialIfDefined() int64 {
    z.RLock()
    defer z.RUnlock()
    if z.SOA != nil {
        return int64(z.SOA.Serial)
    }
    return -1   // SOA가 없으면 -1 (항상 리로드)
}
```

---

## Secondary Zone과 NOTIFY

### NOTIFY 처리

```
// plugin/file/notify.go

func (z *Zone) isNotify(state request.Request) bool {
    if state.Req.Opcode != dns.OpcodeNotify { return false }
    if len(z.TransferFrom) == 0 { return false }

    // 프라이머리 서버 IP 검증
    remote := state.IP()
    for _, f := range z.TransferFrom {
        from, _, err := net.SplitHostPort(f)
        if err != nil { continue }
        if from == remote { return true }
    }
    return false
}
```

NOTIFY 메시지를 받으면 다음 과정을 거친다:

```
1. Opcode가 NOTIFY인지 확인
2. TransferFrom이 설정되어 있는지 확인 (secondary zone인지)
3. 보낸 IP가 TransferFrom 목록에 있는지 검증
4. shouldTransfer()로 SOA 시리얼 비교
5. 시리얼이 증가했으면 TransferIn() 실행
```

### Zone 전송 (Transfer)

```
// plugin/file/secondary.go

func (z *Zone) TransferIn(t *transfer.Transfer) error {
    if len(z.TransferFrom) == 0 { return nil }
    m := new(dns.Msg)
    m.SetAxfr(z.origin)

    z1 := z.CopyWithoutApex()  // 새 빈 Zone 생성

    for _, tr := range z.TransferFrom {
        t := new(dns.Transfer)
        c, err := t.In(m, tr)  // AXFR 전송 시작
        // ...
        for env := range c {
            for _, rr := range env.RR {
                z1.Insert(rr)  // 수신된 RR 삽입
            }
        }
        break  // 성공하면 중단
    }

    // 원자적 교체
    z.Lock()
    z.Tree = z1.Tree
    z.Apex = z1.Apex
    z.Expired = false
    z.Unlock()

    // Secondary 서버에 NOTIFY 전파
    if t != nil {
        t.Notify(z.origin)
    }
    return nil
}
```

### Zone 전송 제공 (ServeDNS → Transfer)

```
// plugin/file/xfr.go

func (z *Zone) Transfer(serial uint32) (<-chan []dns.RR, error) {
    apex, err := z.ApexIfDefined()
    if err != nil { return nil, err }

    ch := make(chan []dns.RR)
    go func() {
        // IXFR fallback: 시리얼이 같으면 SOA만 전송
        if serial != 0 && apex[0].(*dns.SOA).Serial == serial {
            ch <- []dns.RR{apex[0]}
            close(ch)
            return
        }

        // AXFR: SOA → 모든 레코드 → SOA
        ch <- apex
        z.Walk(func(e *tree.Elem, _ map[uint16][]dns.RR) error {
            ch <- e.All()
            return nil
        })
        ch <- []dns.RR{apex[0]}
        close(ch)
    }()
    return ch, nil
}
```

AXFR 전송 형식 (RFC 5936):

```
[SOA + NS + RRSIG]    ← Apex 레코드
[모든 트리 레코드...]  ← Walk로 순회
[SOA]                  ← 종료 표시
```

### Update 루프 (Secondary 자동 갱신)

```
// plugin/file/secondary.go

func (z *Zone) Update(updateShutdown chan bool, t *transfer.Transfer) error {
    // SOA가 로드될 때까지 대기
    for !z.hasSOA() {
        time.Sleep(1 * time.Second)
    }

Restart:
    soa := z.getSOA()
    refresh := time.Second * time.Duration(soa.Refresh)
    retry   := time.Second * time.Duration(soa.Retry)
    expire  := time.Second * time.Duration(soa.Expire)

    for {
        select {
        case <-expireTicker.C:
            // retry 중이면 Zone 만료 표시
            z.Expired = true

        case <-retryTicker.C:
            // shouldTransfer() → TransferIn()

        case <-refreshTicker.C:
            // shouldTransfer() → TransferIn()

        case <-updateShutdown:
            return nil
        }
    }
}
```

이 루프는 SOA의 Refresh/Retry/Expire 값에 따라 동작한다:

```
+---------+     +---------+     +--------+
| Refresh |---->| Check   |---->| 성공   |---> Restart (타이머 리셋)
| Ticker  |     | Transfer|     +--------+
+---------+     +---------+     | 실패   |---> retryActive = true
                                +--------+
                                     |
+---------+     +---------+     +--------+
| Retry   |---->| Check   |---->| 성공   |---> Restart
| Ticker  |     | Transfer|     +--------+
+---------+     +---------+     | 실패   |---> 계속 retry
                                +--------+

+---------+
| Expire  |---->  retryActive이면 z.Expired = true
| Ticker  |       (더 이상 응답 불가)
+---------+
```

### RFC 1982 시리얼 산술

```
const MaxSerialIncrement uint32 = 2147483647  // 2^31 - 1

func less(a, b uint32) bool {
    if a < b {
        return (b - a) <= MaxSerialIncrement
    }
    return (a - b) > MaxSerialIncrement
}
```

시리얼 번호는 32비트 unsigned로, 4294967295 다음에 0으로 순환한다. RFC 1982의 "serial number arithmetic"에 따라 비교해야 정확한 대소 관계를 파악할 수 있다.

---

## Setup과 Corefile 설정

### setup 함수

```
// plugin/file/setup.go

func init() { plugin.Register("file", setup) }

func setup(c *caddy.Controller) error {
    zones, fall, err := fileParse(c)
    f := File{Zones: zones, Fall: fall}

    // transfer 플러그인 연결
    c.OnStartup(func() error {
        t := dnsserver.GetConfig(c).Handler("transfer")
        if t == nil { return nil }
        f.Xfer = t.(*transfer.Transfer)
        go func() {
            for _, n := range zones.Names {
                f.Xfer.Notify(n)   // 시작 시 NOTIFY 전송
            }
        }()
        return nil
    })

    // 각 Zone의 리로드 시작
    for _, n := range zones.Names {
        z := zones.Z[n]
        c.OnShutdown(z.OnShutdown)
        c.OnStartup(func() error {
            z.StartupOnce.Do(func() { z.Reload(f.Xfer) })
            return nil
        })
    }

    dnsserver.GetConfig(c).AddPlugin(func(next plugin.Handler) plugin.Handler {
        f.Next = next
        return f
    })
    return nil
}
```

### fileParse 설정 파싱

```
func fileParse(c *caddy.Controller) (Zones, fall.F, error) {
    // 기본 리로드 간격: 1분
    reload := 1 * time.Minute

    for c.Next() {
        // file db.file [zones...]
        fileName := c.Val()
        origins := plugin.OriginsFromArgsOrServerBlock(c.RemainingArgs(), c.ServerBlockKeys)

        // 상대 경로 처리
        if !filepath.IsAbs(fileName) && config.Root != "" {
            fileName = filepath.Join(config.Root, fileName)
        }

        // Zone 파일 파싱
        for i := range origins {
            z[origins[i]] = NewZone(origins[i], fileName)
            zone, err := Parse(reader, origins[i], fileName, 0)
            z[origins[i]] = zone
        }

        // 블록 옵션
        for c.NextBlock() {
            switch c.Val() {
            case "fallthrough":
                fall.SetZonesFromArgs(c.RemainingArgs())
            case "reload":
                d, err := time.ParseDuration(t[0])
                reload = d
            case "upstream":
                // deprecated, 무시
            }
        }

        for i := range origins {
            z[origins[i]].ReloadInterval = reload
            z[origins[i]].Upstream = upstream.New()
        }
    }
    return Zones{Z: z, Names: names}, fall, nil
}
```

### Corefile 설정 예시

```
# 기본 사용법
example.org {
    file db.example.org
}

# 리로드 간격 변경, fall-through 설정
example.org {
    file db.example.org {
        reload 30s
        fallthrough
    }
}

# 리로드 비활성화
example.org {
    file db.example.org {
        reload 0
    }
}

# 여러 Zone
. {
    file db.example.org example.org
    file db.example.com example.com
}
```

---

## Shutdown 처리

```
// plugin/file/shutdown.go

func (z *Zone) OnShutdown() error {
    if 0 < z.ReloadInterval {
        z.reloadShutdown <- true   // 리로드 고루틴 종료
    }
    return nil
}
```

서버 종료 시 각 Zone의 리로드 고루틴을 깨끗하게 종료한다. `reloadShutdown` 채널에 신호를 보내면 `Reload()` 내부의 select 문에서 `tick.Stop()`을 호출하고 고루틴을 종료한다.

---

## ClosestEncloser

```
// plugin/file/closest.go

func (z *Zone) ClosestEncloser(qname string) (*tree.Elem, bool) {
    offset, end := dns.NextLabel(qname, 0)
    for !end {
        elem, _ := z.Search(qname)
        if elem != nil {
            return elem, true
        }
        qname = qname[offset:]
        offset, end = dns.NextLabel(qname, 0)
    }
    return z.Search(z.origin)
}
```

DNSSEC NXDOMAIN 증명에 사용된다. 쿼리 이름에서 레이블을 하나씩 제거하며 트리에서 존재하는 가장 가까운 상위 이름을 찾는다. 이 이름의 와일드카드 NSEC 레코드를 Authority 섹션에 포함하여 "이 와일드카드 아래에도 해당 이름이 없다"는 것을 증명한다.

---

## 정리

| 항목 | 설명 |
|------|------|
| 자료구조 | LLRB 트리 (DNSSEC 정규 순서) + Apex 분리 저장 |
| 파싱 | miekg/dns ZoneParser, $INCLUDE 지원, SOA 시리얼 기반 스킵 |
| 검색 | 레이블별 오른쪽→왼쪽 순회, delegation/wildcard/DNAME 처리 |
| 리로드 | 주기적 타이머 + SOA 시리얼 비교 + 원자적 포인터 교체 |
| Secondary | AXFR 수신, NOTIFY 처리, SOA Refresh/Retry/Expire 루프 |
| 동시성 | sync.RWMutex (읽기 다수, 쓰기 소수), sync.Once (시작 보장) |
| Fall-through | NXDOMAIN + 빈 응답 시 다음 플러그인 전달 |
| DNSSEC | RRSIG 포함, NSEC 증명, ClosestEncloser 계산 |
