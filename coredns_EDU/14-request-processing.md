# 14. Request 처리 Deep-Dive

## 개요

CoreDNS의 `request` 패키지는 DNS 요청을 추상화하여 모든 플러그인이 통일된 방식으로 요청을 처리할 수 있게 한다. 이 패키지의 핵심 설계 철학은 **Lazy Caching(지연 캐싱)**이다: 요청의 각 속성(이름, IP, 포트, 프로토콜 등)은 최초 접근 시에만 파싱되고, 이후 호출에서는 캐시된 값을 반환한다.

이 문서에서 다루는 핵심 컴포넌트:

| 파일 | 역할 |
|------|------|
| `request/request.go` | Request 구조체, Lazy Caching 접근자, Scrub, Match |
| `request/writer.go` | ScrubWriter (자동 크기 조정 ResponseWriter) |
| `request/edns0.go` | EDNS0 옵션 필터링 |
| `plugin/pkg/dnstest/recorder.go` | Recorder (테스트/메트릭용 ResponseWriter 래퍼) |
| `plugin/metrics/recorder.go` | 플러그인 트래킹 Recorder |

소스 경로: `request/`, `plugin/pkg/dnstest/`, `plugin/metrics/`

---

## Request 구조체

### 구조체 정의

```
// request/request.go

type Request struct {
    Req *dns.Msg             // 원본 DNS 메시지
    W   dns.ResponseWriter   // 응답 작성기 (네트워크 연결 정보 포함)

    // Optional lowercased zone of this query.
    Zone string

    // EDNS0 캐시 (Size 또는 Do 호출 시 설정)
    size uint16   // UDP 버퍼 크기 (TCP면 64K)
    do   bool     // DNSSEC OK 비트

    // 지연 캐시 필드들
    family    int8     // 전송 계층 패밀리 (1=IPv4, 2=IPv6)
    name      string   // 소문자 변환된 qname
    ip        string   // 클라이언트 IP
    port      string   // 클라이언트 포트
    localPort string   // 서버 포트
    localIP   string   // 서버 IP
}
```

**설계 의도:**

`Request`는 값 타입(struct)으로, `dns.Msg`와 `dns.ResponseWriter`를 래핑하여 플러그인 체인 전체에서 공유된다. 포인터가 아닌 값 복사로 전달되므로, 각 플러그인이 독립적인 캐시 상태를 가질 수 있다.

```
플러그인 체인에서의 Request 생성:

ServeDNS(ctx, w, r) {
    state := request.Request{W: w, Req: r}
    // state.Name(), state.IP() 등 필요할 때만 파싱
}
```

모든 플러그인이 동일한 패턴으로 Request를 생성한다. 예를 들어 file 플러그인, forward 플러그인, metrics 플러그인 모두 `request.Request{W: w, Req: r}` 형태로 초기화한다.

---

## Lazy Caching 패턴

### 왜 Lazy Caching인가?

DNS 요청 처리에서 모든 속성이 항상 필요한 것은 아니다:

| 속성 | 사용 빈도 | 파싱 비용 |
|------|----------|----------|
| `Name()` | 거의 모든 플러그인 | 소문자 변환 + FQDN 정규화 |
| `QType()` | 대부분의 플러그인 | 배열 접근 (저비용) |
| `IP()` | ACL, 로깅, NOTIFY | `net.SplitHostPort()` 호출 |
| `Port()` | 로깅, 일부 플러그인 | `net.SplitHostPort()` 호출 |
| `Family()` | 메트릭, Scrub | 타입 어설션 |
| `Size()` | Scrub, 메트릭 | EDNS0 OPT 파싱 |
| `Do()` | DNSSEC 처리 | EDNS0 OPT 파싱 |

Lazy Caching은 "사용하지 않는 것은 파싱하지 않는다"는 원칙을 따른다. DNS 서버는 초당 수십만 건의 쿼리를 처리하므로, 불필요한 파싱을 피하는 것이 성능에 직접적으로 영향을 준다.

### Name() - 쿼리 이름

```
// request/request.go

func (r *Request) Name() string {
    if r.name != "" {
        return r.name       // 캐시 히트
    }
    if r.Req == nil {
        r.name = "."
        return "."
    }
    if len(r.Req.Question) == 0 {
        r.name = "."
        return "."
    }
    // 소문자 변환 + FQDN 보장
    r.name = strings.ToLower(dns.Name(r.Req.Question[0].Name).String())
    return r.name
}
```

**특징:**
- 항상 소문자로 반환 (DNS는 대소문자 무시, RFC 4343)
- 항상 trailing dot 포함 (FQDN 형식)
- Malformed 요청에 대해 "." 반환 (패닉 방지)

**QName()과의 차이:**

```
func (r *Request) QName() string {
    if r.Req == nil { return "." }
    if len(r.Req.Question) == 0 { return "." }
    return dns.Name(r.Req.Question[0].Name).String()
}
```

`QName()`은 원본 대소문자를 유지하며, 캐싱하지 않는다. 로깅이나 메트릭에서 원본 이름이 필요할 때 사용한다.

### IP() - 클라이언트 IP

```
func (r *Request) IP() string {
    if r.ip != "" {
        return r.ip          // 캐시 히트
    }
    ip, _, err := net.SplitHostPort(r.W.RemoteAddr().String())
    if err != nil {
        r.ip = r.W.RemoteAddr().String()
        return r.ip
    }
    r.ip = ip
    return r.ip
}
```

`net.SplitHostPort()`는 `"192.168.1.1:53"` 형태의 문자열에서 IP와 포트를 분리한다. 이 함수는 메모리 할당을 수반하므로 캐싱이 효과적이다.

### Port() - 클라이언트 포트

```
func (r *Request) Port() string {
    if r.port != "" {
        return r.port
    }
    _, port, err := net.SplitHostPort(r.W.RemoteAddr().String())
    if err != nil {
        r.port = "0"
        return r.port
    }
    r.port = port
    return r.port
}
```

`IP()`와 `Port()`는 같은 `RemoteAddr()`에서 파싱하지만 별도로 캐싱한다. 이는 각각 독립적으로 호출될 수 있기 때문이다.

### LocalIP(), LocalPort() - 서버 주소

```
func (r *Request) LocalIP() string {
    if r.localIP != "" { return r.localIP }
    ip, _, err := net.SplitHostPort(r.W.LocalAddr().String())
    if err != nil {
        r.localIP = r.W.LocalAddr().String()
        return r.localIP
    }
    r.localIP = ip
    return r.localIP
}

func (r *Request) LocalPort() string {
    if r.localPort != "" { return r.localPort }
    _, port, err := net.SplitHostPort(r.W.LocalAddr().String())
    if err != nil {
        r.localPort = "0"
        return r.localPort
    }
    r.localPort = port
    return r.localPort
}
```

서버 측 주소 정보도 동일한 Lazy Caching 패턴을 따른다. 멀티 인터페이스 서버에서 어떤 인터페이스로 요청이 들어왔는지 확인할 때 사용한다.

### Proto() - 전송 프로토콜

```
func (r *Request) Proto() string {
    if _, ok := r.W.RemoteAddr().(*net.UDPAddr); ok {
        return "udp"
    }
    if _, ok := r.W.RemoteAddr().(*net.TCPAddr); ok {
        return "tcp"
    }
    return "udp"    // 기본값
}
```

`Proto()`는 캐싱하지 않는다. 타입 어설션은 매우 빠른 연산이므로 캐싱 오버헤드가 더 클 수 있다. `RemoteAddr()`의 구체적 타입이 `*net.UDPAddr`인지 `*net.TCPAddr`인지로 프로토콜을 판별한다.

### Family() - IP 버전

```
func (r *Request) Family() int {
    if r.family != 0 {
        return int(r.family)   // 캐시 히트
    }

    var a net.IP
    ip := r.W.RemoteAddr()
    if i, ok := ip.(*net.UDPAddr); ok {
        a = i.IP
    }
    if i, ok := ip.(*net.TCPAddr); ok {
        a = i.IP
    }

    if a.To4() != nil {
        r.family = 1    // IPv4
        return 1
    }
    r.family = 2        // IPv6
    return 2
}
```

**왜 `int8`로 캐싱하는가?**

`family` 필드는 `int8` 타입이다. 0은 "아직 계산되지 않음"을 나타내고, 1은 IPv4, 2는 IPv6이다. `int8`을 사용하여 Request 구조체의 크기를 최소화한다 (패딩 최적화).

### QType(), Type() - 쿼리 타입

```
func (r *Request) QType() uint16 {
    if r.Req == nil { return 0 }
    if len(r.Req.Question) == 0 { return 0 }
    return r.Req.Question[0].Qtype
}

func (r *Request) Type() string {
    if r.Req == nil { return "" }
    if len(r.Req.Question) == 0 { return "" }
    return dns.Type(r.Req.Question[0].Qtype).String()
}
```

`QType()`는 캐싱하지 않는다. 배열 인덱싱은 O(1)이므로 캐싱 필요가 없다. `Type()`은 문자열 변환을 수행하지만, 역시 캐싱하지 않는다 (dns.Type.String()은 내부적으로 맵 조회를 수행하지만 매우 빠르다).

### QClass(), Class() - 쿼리 클래스

```
func (r *Request) QClass() uint16 {
    if r.Req == nil { return 0 }
    if len(r.Req.Question) == 0 { return 0 }
    return r.Req.Question[0].Qclass
}

func (r *Request) Class() string {
    if r.Req == nil { return "" }
    if len(r.Req.Question) == 0 { return "" }
    return dns.Class(r.Req.Question[0].Qclass).String()
}
```

### Clear() - 캐시 초기화

```
func (r *Request) Clear() {
    r.name = ""
    r.ip = ""
    r.localIP = ""
    r.port = ""
    r.localPort = ""
    r.family = 0
    r.size = 0
    r.do = false
}
```

요청이 수정되었을 때(예: 쿼리 이름 변경) 캐시를 초기화하여 다음 접근 시 재파싱하도록 한다.

---

## EDNS0 처리

### Size() - 버퍼 크기

```
func (r *Request) Size() int {
    if r.size != 0 {
        return int(r.size)    // 캐시 히트
    }

    size := uint16(0)
    if o := r.Req.IsEdns0(); o != nil {
        r.do = o.Do()           // DO 비트도 동시에 캐싱
        size = o.UDPSize()
    }

    // 크기 정규화
    size = edns.Size(r.Proto(), size)
    r.size = size
    return int(size)
}
```

**Size()와 Do()의 연동 캐싱:**

`Size()`가 호출되면 EDNS0 OPT 레코드를 한 번만 파싱하고, `do`와 `size`를 동시에 캐싱한다. `Do()`가 먼저 호출되면:

```
func (r *Request) Do() bool {
    if r.size != 0 {
        return r.do       // Size()가 이미 호출된 경우
    }
    r.Size()              // Size()를 호출하여 do도 캐싱
    return r.do
}
```

이 설계는 EDNS0 OPT 레코드를 최대 한 번만 파싱하도록 보장한다.

**UDP 버퍼 크기 정규화:**

```
// Size()에서 호출하는 edns.Size()는 다음 규칙을 적용:
//
// UDP:
//   - 클라이언트가 EDNS0을 보내지 않으면: 512 바이트 (RFC 1035)
//   - 클라이언트가 보낸 버퍼 크기가 512 미만이면: 512 바이트
//   - 그 외: 클라이언트가 광고한 크기
//
// TCP:
//   - 항상 65535 바이트 (64K)
```

### SizeAndDo() - 응답 OPT 설정

```
func (r *Request) SizeAndDo(m *dns.Msg) bool {
    o := r.Req.IsEdns0()
    if o == nil {
        return false    // 요청에 EDNS0이 없으면 응답에도 추가하지 않음
    }

    // 응답에 이미 OPT가 있으면 업데이트
    if mo := m.IsEdns0(); mo != nil {
        mo.Hdr.Name = "."
        mo.Hdr.Rrtype = dns.TypeOPT
        mo.SetVersion(0)
        mo.SetUDPSize(o.UDPSize())
        mo.Hdr.Ttl &= 0xff00    // 플래그 클리어
        if o.Do() { mo.SetDo() }
        return true
    }

    // 요청의 OPT 레코드를 재사용하여 응답에 추가
    o.Hdr.Name = "."
    o.Hdr.Rrtype = dns.TypeOPT
    o.SetVersion(0)
    o.Hdr.Ttl &= 0xff00
    if len(o.Option) > 0 {
        o.Option = supportedOptions(o.Option)   // 지원하는 옵션만 필터
    }
    m.Extra = append(m.Extra, o)
    return true
}
```

**EDNS0 옵션 필터링:**

```
// request/edns0.go

func supportedOptions(o []dns.EDNS0) []dns.EDNS0 {
    var supported = make([]dns.EDNS0, 0, 3)
    for _, opt := range o {
        switch code := opt.Option(); code {
        case dns.EDNS0NSID:           // 네임서버 식별자
        case dns.EDNS0EXPIRE:         // Zone 만료 시간
        case dns.EDNS0COOKIE:         // DNS Cookie
        case dns.EDNS0TCPKEEPALIVE:   // TCP 유지
        case dns.EDNS0PADDING:        // 패딩
            supported = append(supported, opt)
        default:
            if edns.SupportedOption(code) {
                supported = append(supported, opt)
            }
        }
    }
    return supported
}
```

CoreDNS가 기본 지원하는 EDNS0 옵션:

| 옵션 | RFC | 용도 |
|------|-----|------|
| NSID | RFC 5001 | 네임서버 식별 |
| EXPIRE | RFC 7314 | Zone 만료 타이머 |
| COOKIE | RFC 7873 | DNS 쿠키 (스푸핑 방지) |
| TCP-KEEPALIVE | RFC 7828 | TCP 연결 유지 |
| PADDING | RFC 7830 | 쿼리/응답 길이 패딩 |

### NewWithQuestion() - 새 쿼리 생성

```
func (r *Request) NewWithQuestion(name string, typ uint16) Request {
    req1 := Request{W: r.W, Req: r.Req.Copy()}
    req1.Req.Question[0] = dns.Question{
        Name:   dns.Fqdn(name),
        Qclass: dns.ClassINET,
        Qtype:  typ,
    }
    return req1
}
```

CNAME 추적이나 내부 조회 시 원본 요청을 복사하고 질문 섹션만 변경하여 새로운 Request를 만든다. 원본 `dns.Msg`를 `Copy()`하므로 원본이 변경되지 않는다. 캐시는 초기화된 상태(빈 문자열/0)로 시작한다.

---

## Scrub - 응답 크기 조정

### Scrub() 메서드

```
// request/request.go

func (r *Request) Scrub(reply *dns.Msg) *dns.Msg {
    // 1. 응답을 클라이언트 버퍼 크기에 맞게 절단
    reply.Truncate(r.Size())

    // 2. 이미 압축이 설정되어 있으면 그대로 반환
    if reply.Compress {
        return reply
    }

    // 3. UDP이고 단편화 위험이 있으면 압축 활성화
    if r.Proto() == "udp" {
        rl := reply.Len()
        // IPv4: 1480바이트 초과 시 압축
        if rl > 1480 && r.Family() == 1 {
            reply.Compress = true
        }
        // IPv6: 1220바이트 초과 시 압축
        if rl > 1220 && r.Family() == 2 {
            reply.Compress = true
        }
    }

    return reply
}
```

**Scrub의 동작 원리:**

```
                    DNS 응답 메시지
                         |
                         v
              +-----------------------+
              | reply.Truncate(size)  |
              | (dns 라이브러리 호출) |
              +-----------------------+
                         |
              크기 초과 시: Answer → NS → Extra 순서로 제거
              제거 후에도 초과 시: TC(Truncation) 비트 설정
                         |
                         v
              +-----------------------+
              | 이미 압축 설정됨?    |
              +-----------------------+
               Yes |           | No
                   v           v
              [반환]    +-----------+
                        | UDP인가? |
                        +-----------+
                         Yes |    | No
                             v    v
                    +----------+ [반환]
                    | 크기체크 |
                    +----------+
                    IPv4>1480? → 압축
                    IPv6>1220? → 압축
```

**단편화 방지 임계값:**

| 패밀리 | 임계값 | 근거 |
|--------|--------|------|
| IPv4 | 1480 바이트 | MTU 1500 - IP 헤더 20 = 1480 |
| IPv6 | 1220 바이트 | IPv6 최소 MTU 1280 - IPv6 헤더 40 - UDP 헤더 8 ≈ 1220 |

이 값들은 NSD(Name Server Daemon)의 설정을 참고한 것이다. DNS 메시지가 이 크기를 초과하면 IP 단편화가 발생할 수 있으며, 이는 방화벽에서 차단되거나 패킷 손실을 유발할 수 있다.

### Truncate 동작 (dns 라이브러리)

`reply.Truncate(size)`는 다음 순서로 섹션을 축소한다:

```
1. Extra 섹션에서 OPT가 아닌 레코드 제거
2. NS 섹션 축소
3. Answer 섹션 축소
4. 압축(Compress) 활성화
5. 여전히 초과하면 TC 비트 설정
```

TC(Truncation) 비트가 설정되면 클라이언트는 TCP로 재시도해야 한다.

---

## ScrubWriter

### 정의와 역할

```
// request/writer.go

type ScrubWriter struct {
    dns.ResponseWriter         // 원본 ResponseWriter 임베딩
    req *dns.Msg               // 원본 요청 메시지
}

func NewScrubWriter(req *dns.Msg, w dns.ResponseWriter) *ScrubWriter {
    return &ScrubWriter{w, req}
}

func (s *ScrubWriter) WriteMsg(m *dns.Msg) error {
    state := Request{Req: s.req, W: s.ResponseWriter}
    state.SizeAndDo(m)    // EDNS0 설정 반영
    state.Scrub(m)        // 크기 조정
    return s.ResponseWriter.WriteMsg(m)
}
```

**ScrubWriter의 용도:**

ScrubWriter는 "자동 크기 조정" ResponseWriter이다. 플러그인이 응답을 작성할 때 `WriteMsg()`를 호출하면, 자동으로:

1. 요청의 EDNS0 설정을 응답에 반영 (SizeAndDo)
2. 응답 크기를 클라이언트 버퍼에 맞게 조정 (Scrub)
3. 원본 ResponseWriter로 전달

```
사용 패턴:

// 플러그인에서 ScrubWriter 사용
func (p Plugin) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) {
    sw := request.NewScrubWriter(r, w)
    // ... 플러그인 로직 ...
    sw.WriteMsg(response)  // 자동으로 Scrub 적용
}
```

**데코레이터 패턴:**

ScrubWriter는 ResponseWriter 인터페이스를 구현하면서 원본을 래핑하는 데코레이터 패턴이다:

```
+------------------+
| ScrubWriter      |
|  +-------------+ |
|  | Original    | |
|  | Response    | |
|  | Writer      | |
|  +-------------+ |
|                  |
|  WriteMsg():     |
|  1. SizeAndDo()  |
|  2. Scrub()      |
|  3. delegate()   |
+------------------+
```

---

## Match - 응답 검증

```
// request/request.go

func (r *Request) Match(reply *dns.Msg) bool {
    if len(reply.Question) != 1 {
        return false
    }
    if !reply.Response {
        return false
    }
    if strings.ToLower(reply.Question[0].Name) != r.Name() {
        return false
    }
    if reply.Question[0].Qtype != r.QType() {
        return false
    }
    return true
}
```

**Match의 검증 항목:**

| 검사 | 조건 | 이유 |
|------|------|------|
| Question 수 | 정확히 1개 | DNS는 하나의 질문만 허용 |
| Response 비트 | true | 응답 메시지여야 함 |
| 이름 일치 | 소문자 비교 | 대소문자 무시 (RFC 4343) |
| 타입 일치 | 정확히 일치 | 같은 타입의 응답이어야 함 |

Match는 forward 플러그인 등에서 upstream으로부터 받은 응답이 원래 요청에 대한 것인지 검증할 때 사용한다. 이는 DNS 스푸핑 방어의 일부이다.

---

## RemoteAddr, LocalAddr - 직접 접근

```
func (r *Request) RemoteAddr() string { return r.W.RemoteAddr().String() }
func (r *Request) LocalAddr() string  { return r.W.LocalAddr().String() }
```

IP, Port를 분리하지 않고 `"host:port"` 형태 전체가 필요할 때 사용한다. 캐싱하지 않는다 (ResponseWriter의 메서드를 직접 위임).

### Len() - 요청 크기

```
func (r *Request) Len() int { return r.Req.Len() }
```

원본 DNS 메시지의 와이어 포맷 크기를 바이트 단위로 반환한다. 메트릭 보고에 사용된다.

---

## dnstest.Recorder

### 구조체 정의

```
// plugin/pkg/dnstest/recorder.go

type Recorder struct {
    dns.ResponseWriter        // 원본 ResponseWriter 임베딩
    Rcode int                 // 응답 코드
    Len   int                 // 응답 크기 (누적)
    Msg   *dns.Msg            // 마지막 응답 메시지
    Start time.Time           // 요청 시작 시간
}

func NewRecorder(w dns.ResponseWriter) *Recorder {
    return &Recorder{
        ResponseWriter: w,
        Rcode:          0,
        Msg:            nil,
        Start:          time.Now(),
    }
}
```

**Recorder의 역할:**

Recorder는 테스트와 메트릭 수집에서 사용되는 ResponseWriter 래퍼이다. 응답 코드, 크기, 메시지를 기록하면서 원본 ResponseWriter로 데이터를 전달한다.

```
Recorder 동작 흐름:

플러그인 → Recorder.WriteMsg(m) → 기록(Rcode, Len, Msg) → 원본.WriteMsg(m) → 네트워크
```

### WriteMsg 메서드

```
func (r *Recorder) WriteMsg(res *dns.Msg) error {
    r.Rcode = res.Rcode        // 응답 코드 기록
    r.Len += res.Len()         // 크기 누적 (AXFR에서 여러 번 호출될 수 있음)
    r.Msg = res                // 마지막 메시지 저장
    return r.ResponseWriter.WriteMsg(res)
}
```

**크기 누적의 이유:** AXFR(Zone 전송) 응답은 여러 개의 DNS 메시지로 분할되어 전송된다. 각 메시지마다 `WriteMsg()`가 호출되므로, 총 전송 크기를 정확히 측정하려면 누적해야 한다.

### Write 메서드

```
func (r *Recorder) Write(buf []byte) (int, error) {
    n, err := r.ResponseWriter.Write(buf)
    if err == nil {
        r.Len += n             // 원시 바이트 쓰기도 크기 추적
    }
    return n, err
}
```

---

## Metrics Recorder (플러그인 트래킹)

### 구조체 정의

```
// plugin/metrics/recorder.go

type Recorder struct {
    *dnstest.Recorder            // dnstest.Recorder 임베딩
    Plugin string                // 응답을 작성한 플러그인 이름
}

func NewRecorder(w dns.ResponseWriter) *Recorder {
    return &Recorder{Recorder: dnstest.NewRecorder(w)}
}
```

### PluginTracker 인터페이스

```
func (r *Recorder) SetPlugin(name string) {
    r.Plugin = name
}

func (r *Recorder) GetPlugin() string {
    return r.Plugin
}
```

`PluginTracker` 인터페이스는 어떤 플러그인이 실제로 응답을 작성했는지 추적한다. 이 정보는 `coredns_dns_responses_total` 메트릭의 `plugin` 레이블에 사용된다.

**플러그인 트래킹 흐름:**

```
요청 → prometheus 플러그인 (Recorder 생성)
     → 다음 플러그인들...
     → 실제 응답 작성 플러그인 (SetPlugin 호출)
     → prometheus 플러그인 (Recorder.Plugin 읽기)
     → vars.Report(..., rw.Plugin, ...)
```

### Metrics ServeDNS에서의 사용

```
// plugin/metrics/handler.go

func (m *Metrics) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
    state := request.Request{W: w, Req: r}
    originalSize := r.Len()      // 원본 요청 크기 캡처

    qname := state.QName()
    zone := plugin.Zones(m.ZoneNames()).Matches(qname)
    if zone == "" { zone = "." }

    // Recorder로 ResponseWriter 래핑
    rw := NewRecorder(w)
    status, err := plugin.NextOrFailure(m.Name(), m.Next, ctx, rw, r)

    rc := rw.Rcode
    if !plugin.ClientWrite(status) {
        rc = status    // 응답이 작성되지 않았으면 상태 코드 사용
    }

    // 메트릭 보고
    vars.Report(WithServer(ctx), state, zone, WithView(ctx),
                rcode.ToString(rc), rw.Plugin,
                rw.Len, rw.Start,
                vars.WithOriginalReqSize(originalSize))

    return status, err
}
```

**Report 호출 시 전달되는 정보:**

| 파라미터 | 출처 | 용도 |
|----------|------|------|
| `server` | context | 메트릭 레이블 |
| `state` | Request | 프로토콜, 패밀리, qtype, DO비트 |
| `zone` | 매칭 결과 | 메트릭 레이블 |
| `view` | context | view 레이블 |
| `rcode` | Recorder.Rcode | 응답 코드 레이블 |
| `plugin` | Recorder.Plugin | 응답 플러그인 레이블 |
| `size` | Recorder.Len | 응답 크기 히스토그램 |
| `start` | Recorder.Start | 지연 시간 히스토그램 |
| `originalSize` | r.Len() | 요청 크기 히스토그램 |

---

## Request 사용 패턴 모음

### 패턴 1: 기본 플러그인 사용

```
func (p Plugin) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
    state := request.Request{W: w, Req: r}

    // Lazy access - 필요할 때만 파싱
    name := state.Name()       // 첫 호출: 파싱 + 캐싱
    qtype := state.QType()     // 직접 접근 (캐싱 불필요)
    proto := state.Proto()     // 타입 어설션 (캐싱 불필요)

    // 두 번째 호출: 캐시 히트
    _ = state.Name()           // 즉시 반환
}
```

### 패턴 2: 조건부 DNSSEC 처리

```
func handleQuery(state request.Request) {
    if state.Do() {
        // DNSSEC OK 비트 설정됨
        // RRSIG 레코드 포함
    }

    size := state.Size()    // Do()에서 이미 캐싱됨 → 즉시 반환
}
```

### 패턴 3: 응답 크기 조정

```
func respond(state request.Request, w dns.ResponseWriter, m *dns.Msg) {
    state.SizeAndDo(m)     // EDNS0 설정 반영
    state.Scrub(m)         // 크기 조정
    w.WriteMsg(m)
}

// 또는 ScrubWriter 사용 (자동화)
sw := request.NewScrubWriter(r, w)
sw.WriteMsg(m)             // SizeAndDo + Scrub + WriteMsg 자동
```

### 패턴 4: 새 쿼리 생성

```
func followCNAME(state request.Request, target string) {
    newState := state.NewWithQuestion(target, dns.TypeA)
    // newState는 원본과 같은 연결 정보를 가지지만
    // Question 섹션만 다르다
    // 캐시는 초기화된 상태
}
```

### 패턴 5: 메트릭 수집

```
func (m *Metrics) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
    state := request.Request{W: w, Req: r}
    rw := NewRecorder(w)

    status, err := plugin.NextOrFailure(m.Name(), m.Next, ctx, rw, r)

    // Recorder에서 정보 추출
    vars.Report(...,
        rw.Len,        // 응답 크기
        rw.Start,      // 시작 시간
    )
}
```

---

## Lazy Caching 성능 분석

### 캐싱 전략 비교

```
+-------------------+--------+--------+-----------+
| 접근자            | 캐싱   | 이유                      |
+-------------------+--------+-----------+
| Name()            | O      | strings.ToLower + FQDN 변환       |
| IP()              | O      | net.SplitHostPort 메모리 할당      |
| Port()            | O      | net.SplitHostPort 메모리 할당      |
| LocalIP()         | O      | net.SplitHostPort 메모리 할당      |
| LocalPort()       | O      | net.SplitHostPort 메모리 할당      |
| Family()          | O      | 타입 어설션 + IP 변환             |
| Size()            | O      | EDNS0 OPT 파싱 (Do와 연동)        |
| Do()              | O      | Size()와 연동 캐싱                |
| Proto()           | X      | 타입 어설션만 (매우 빠름)          |
| QType()           | X      | 배열 인덱싱 (O(1))               |
| QClass()          | X      | 배열 인덱싱 (O(1))               |
| Type()            | X      | 맵 조회 (충분히 빠름)             |
| QName()           | X      | 원본 이름 유지 필요               |
| Len()             | X      | dns.Msg.Len() 위임               |
+-------------------+--------+-----------+
```

### 메모리 레이아웃

```
Request 구조체 메모리 레이아웃 (64비트 시스템):

+--------+--------+--------+--------+  offset
| Req (*dns.Msg)  | W (interface)   |  0-24
+--------+--------+--------+--------+
| Zone (string)   | size   | do     |  24-50
+--------+--------+--------+--------+
| family | pad    | name (string)   |  50-72
+--------+--------+--------+--------+
| ip (string)     | port (string)   |  72-104
+--------+--------+--------+--------+
| localPort       | localIP         |  104-136
+--------+--------+--------+--------+

대략 136바이트 크기로, 스택 할당이 가능하여 GC 부담이 없다.
```

---

## ResponseWriter 래퍼 체인

CoreDNS에서 ResponseWriter는 여러 층의 래퍼로 감싸질 수 있다:

```
네트워크 연결 (dns.ResponseWriter)
    |
    +--- ScrubWriter (크기 조정)
    |       |
    |       +--- Metrics Recorder (메트릭 수집)
    |               |
    |               +--- dnstest.Recorder (Rcode/Len 기록)
    |                       |
    |                       +--- 원본 dns.ResponseWriter
    |
    +--- 다른 플러그인의 래퍼들...
```

각 래퍼는 `dns.ResponseWriter` 인터페이스를 구현하면서 원본을 임베딩한다:

```
type ScrubWriter struct {
    dns.ResponseWriter      // 임베딩
    req *dns.Msg
}

type Recorder struct {
    dns.ResponseWriter      // 임베딩
    Rcode int
    Len   int
    ...
}
```

Go의 인터페이스 임베딩 덕분에 `RemoteAddr()`, `LocalAddr()` 등 모든 메서드가 자동으로 위임된다. 래퍼는 `WriteMsg()`만 오버라이드하여 추가 로직을 삽입한다.

---

## 정리

| 항목 | 설명 |
|------|------|
| Request | DNS 요청 추상화, Lazy Caching으로 성능 최적화 |
| Lazy Caching | 최초 접근 시 파싱, 이후 캐시 반환. Name/IP/Port/Size/Do |
| 캐싱 안 하는 것 | Proto/QType/QClass (접근 비용이 캐싱 비용보다 작음) |
| Size+Do 연동 | EDNS0 OPT를 한 번만 파싱, 두 값을 동시 캐싱 |
| Scrub | UDP 단편화 방지, IPv4 1480/IPv6 1220 임계값 |
| ScrubWriter | 자동 크기 조정 ResponseWriter 데코레이터 |
| Match | 응답 검증 (이름 + 타입 + Response 비트) |
| dnstest.Recorder | 테스트/메트릭용, Rcode/Len/Msg/Start 기록 |
| Metrics Recorder | PluginTracker 추가, 어떤 플러그인이 응답했는지 추적 |
| 구조체 크기 | ~136바이트, 스택 할당 가능, GC 부담 없음 |
