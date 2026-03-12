# CoreDNS 데이터 모델

## 1. 개요

CoreDNS의 데이터 모델은 DNS 프로토콜의 메시지 구조를 기반으로, 서버 설정과 플러그인 체인을 연결하는 구조체들로 구성된다. 핵심 DNS 타입은 `github.com/miekg/dns` 라이브러리에서 제공하며, CoreDNS는 이를 래핑하고 확장하여 사용한다.

## 2. DNS 메시지 구조 (dns.Msg)

DNS 메시지는 `github.com/miekg/dns` 패키지의 `dns.Msg` 구조체로 표현된다. CoreDNS의 모든 플러그인은 이 구조체를 기반으로 동작한다.

```
┌─────────────────────────────────────────┐
│              dns.Msg                     │
├─────────────────────────────────────────┤
│  MsgHdr (헤더)                           │
│  ├── Id         uint16  (트랜잭션 ID)    │
│  ├── Response   bool    (응답 여부)      │
│  ├── Opcode     int     (연산 코드)      │
│  ├── Authoritative bool (권한 응답)      │
│  ├── Truncated  bool    (잘림 여부)      │
│  ├── RecursionDesired   bool             │
│  ├── RecursionAvailable bool             │
│  ├── AuthenticatedData  bool (AD 비트)   │
│  ├── CheckingDisabled   bool (CD 비트)   │
│  └── Rcode      int     (응답 코드)      │
├─────────────────────────────────────────┤
│  Question  []Question   (질문 섹션)      │
│  ├── Name   string      (QNAME)         │
│  ├── Qtype  uint16      (질문 타입)      │
│  └── Qclass uint16      (질문 클래스)    │
├─────────────────────────────────────────┤
│  Answer    []RR          (응답 섹션)      │
├─────────────────────────────────────────┤
│  Ns        []RR          (권한 섹션)      │
├─────────────────────────────────────────┤
│  Extra     []RR          (추가 섹션)      │
│  └── OPT RR (EDNS0)                     │
└─────────────────────────────────────────┘
```

### 응답 코드 (Rcode)

| 코드 | 상수 | 의미 |
|------|------|------|
| 0 | `dns.RcodeSuccess` | 성공 |
| 1 | `dns.RcodeFormatError` | 형식 오류 (FORMERR) |
| 2 | `dns.RcodeServerFailure` | 서버 실패 (SERVFAIL) |
| 3 | `dns.RcodeNameError` | 이름 없음 (NXDOMAIN) |
| 4 | `dns.RcodeNotImplemented` | 미구현 (NOTIMP) |
| 5 | `dns.RcodeRefused` | 거부 (REFUSED) |

### ClientWrite 판단 로직

**소스코드 경로**: `plugin/plugin.go`

CoreDNS는 특정 Rcode가 반환되면 "아직 클라이언트에 응답을 쓰지 않았다"고 판단한다.

```go
func ClientWrite(rcode int) bool {
    switch rcode {
    case dns.RcodeServerFailure:
        fallthrough
    case dns.RcodeRefused:
        fallthrough
    case dns.RcodeFormatError:
        fallthrough
    case dns.RcodeNotImplemented:
        return false
    }
    return true
}
```

## 3. RR(Resource Record) 타입

DNS 리소스 레코드는 `dns.RR` 인터페이스로 추상화되며, 각 레코드 타입은 구체적인 구조체로 구현된다.

### 주요 RR 타입

| 타입 | 상수 | Go 구조체 | 용도 |
|------|------|-----------|------|
| A | `dns.TypeA` | `dns.A` | IPv4 주소 매핑 |
| AAAA | `dns.TypeAAAA` | `dns.AAAA` | IPv6 주소 매핑 |
| CNAME | `dns.TypeCNAME` | `dns.CNAME` | 별칭 (Canonical Name) |
| MX | `dns.TypeMX` | `dns.MX` | 메일 서버 |
| NS | `dns.TypeNS` | `dns.NS` | 네임서버 위임 |
| SOA | `dns.TypeSOA` | `dns.SOA` | Zone 권한 시작 |
| PTR | `dns.TypePTR` | `dns.PTR` | 역방향 DNS 조회 |
| SRV | `dns.TypeSRV` | `dns.SRV` | 서비스 레코드 (K8s에서 핵심) |
| TXT | `dns.TypeTXT` | `dns.TXT` | 텍스트 데이터 |
| OPT | `dns.TypeOPT` | `dns.OPT` | EDNS0 확장 |
| AXFR | `dns.TypeAXFR` | - | 전체 Zone 전송 |
| DS | `dns.TypeDS` | `dns.DS` | 위임 서명자 (DNSSEC) |
| RRSIG | `dns.TypeRRSIG` | `dns.RRSIG` | 레코드 서명 (DNSSEC) |
| NSEC | `dns.TypeNSEC` | `dns.NSEC` | 부재 증명 (DNSSEC) |

### RR 헤더 구조

모든 RR은 공통 헤더를 가진다:

```
┌───────────────────────────────────────┐
│            dns.RR_Header              │
├───────────────────────────────────────┤
│  Name     string   (소유자 이름)       │
│  Rrtype   uint16   (레코드 타입)       │
│  Class    uint16   (레코드 클래스)      │
│  Ttl      uint32   (TTL, 초)           │
│  Rdlength uint16   (RDATA 길이)        │
└───────────────────────────────────────┘
```

## 4. Request 구조체

**소스코드 경로**: `request/request.go`

`Request`는 DNS 요청을 추상화하여 플러그인에서 공통적으로 사용할 수 있는 편의 메서드를 제공한다.

```go
type Request struct {
    Req *dns.Msg          // 원본 DNS 메시지
    W   dns.ResponseWriter // 응답 라이터

    Zone string           // (옵션) 소문자 Zone 이름

    // 캐시된 값들 (lazy initialization)
    size      uint16      // UDP 버퍼 크기 또는 TCP 64K
    do        bool        // DNSSEC OK 비트
    family    int8        // 1=IPv4, 2=IPv6
    name      string      // 소문자 QNAME
    ip        string      // 클라이언트 IP
    port      string      // 클라이언트 포트
    localPort string      // 서버 포트
    localIP   string      // 서버 IP
}
```

### Request의 주요 메서드

| 메서드 | 반환 타입 | 설명 |
|--------|----------|------|
| `Name()` | `string` | 소문자 QNAME (캐시됨) |
| `QName()` | `string` | 원본 QNAME |
| `QType()` | `uint16` | 질문 타입 |
| `QClass()` | `uint16` | 질문 클래스 |
| `Type()` | `string` | 질문 타입 문자열 ("A", "AAAA" 등) |
| `IP()` | `string` | 클라이언트 IP (캐시됨) |
| `Port()` | `string` | 클라이언트 포트 |
| `LocalIP()` | `string` | 서버 IP |
| `Proto()` | `string` | "udp" 또는 "tcp" |
| `Family()` | `int` | 1(IPv4) 또는 2(IPv6) |
| `Do()` | `bool` | DNSSEC OK 비트 |
| `Size()` | `int` | 응답 버퍼 크기 |
| `SizeAndDo()` | `bool` | OPT 레코드 설정 |
| `Scrub()` | `*dns.Msg` | 응답 크기 맞춤 |
| `Match()` | `bool` | 응답이 요청과 매칭되는지 확인 |

### Lazy Caching 패턴

Request는 **lazy initialization** 패턴을 사용한다. `IP()`, `Size()` 등의 메서드는 처음 호출 시에만 계산하고 이후에는 캐시된 값을 반환한다.

```go
func (r *Request) IP() string {
    if r.ip != "" {
        return r.ip
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

## 5. ScrubWriter

**소스코드 경로**: `request/writer.go`

`ScrubWriter`는 `dns.ResponseWriter`를 래핑하여 응답 메시지가 클라이언트 버퍼에 맞도록 자동 조정한다.

```go
type ScrubWriter struct {
    dns.ResponseWriter
    req *dns.Msg  // 원본 요청
}

func (s *ScrubWriter) WriteMsg(m *dns.Msg) error {
    state := Request{Req: s.req, W: s.ResponseWriter}
    state.SizeAndDo(m)   // EDNS0 OPT 레코드 설정
    state.Scrub(m)       // 응답 크기 맞춤 (Truncate + Compress)
    return s.ResponseWriter.WriteMsg(m)
}
```

### Scrub 동작

```
┌──────────────────────────────────────────────┐
│              Scrub 알고리즘                    │
├──────────────────────────────────────────────┤
│ 1. reply.Truncate(size)                      │
│    → 응답이 버퍼 크기 초과 시 TC 비트 설정      │
│                                              │
│ 2. UDP 전송 시 추가 압축 검토:                 │
│    → IPv4: 응답 > 1480바이트 → 압축 활성화     │
│    → IPv6: 응답 > 1220바이트 → 압축 활성화     │
│    (NSD의 EDNS 단편화 방지 임계값 참조)         │
└──────────────────────────────────────────────┘
```

## 6. Config 구조체

**소스코드 경로**: `core/dnsserver/config.go`

`Config`는 하나의 서버 블록(Zone 설정)을 나타내며, Zone 이름, 플러그인 목록, 네트워크 설정 등을 포함한다.

```go
type Config struct {
    Zone        string          // Zone 이름 (예: "example.com.")
    ListenHosts []string        // 바인드 주소 목록
    Port        string          // 리스닝 포트
    NumSockets  int             // 소켓 수 (멀티소켓 지원)
    Root        string          // 기본 디렉토리
    Debug       bool            // 디버그 모드
    Stacktrace  bool            // 패닉 시 스택트레이스
    Transport   string          // 프로토콜 ("dns", "tls", "quic" 등)

    TLSConfig   *tls.Config     // TLS 설정
    FilterFuncs []FilterFunc    // 뷰 필터 함수

    ReadTimeout  time.Duration  // TCP 읽기 타임아웃 (기본 3초)
    WriteTimeout time.Duration  // TCP 쓰기 타임아웃 (기본 5초)
    IdleTimeout  time.Duration  // TCP 유휴 타임아웃 (기본 10초)

    TsigSecret  map[string]string  // TSIG 시크릿

    Plugin      []plugin.Plugin    // 플러그인 목록 (미조립)
    pluginChain plugin.Handler     // 조립된 플러그인 체인
    registry    map[string]plugin.Handler  // 핸들러 레지스트리

    firstConfigInBlock *Config          // 블록 내 첫 번째 Config 참조
    metaCollector      MetadataCollector // 메타데이터 수집기
}
```

### Config 관계도

```
┌─────────────────────────────────────────────────┐
│                  dnsContext                       │
│  keysToConfigs: map[string]*Config               │
│  configs: []*Config (마스터 목록)                 │
├─────────────────────────────────────────────────┤
│                                                  │
│  ┌──────────────────────────────────────────┐    │
│  │ Server Block: "example.com:53"            │    │
│  │  ┌─────────┐  ┌─────────┐               │    │
│  │  │ Config   │  │ Config   │              │    │
│  │  │ Zone:    │  │ Zone:    │              │    │
│  │  │"example. │  │"sub.     │              │    │
│  │  │ com."    │  │ example. │              │    │
│  │  │          │  │ com."    │              │    │
│  │  │ Plugin:  │←─│ Plugin:  │ (공유)       │    │
│  │  │ [log,    │  │ [log,    │              │    │
│  │  │  cache,  │  │  cache,  │              │    │
│  │  │  forward]│  │  forward]│              │    │
│  │  └─────────┘  └─────────┘               │    │
│  └──────────────────────────────────────────┘    │
│                                                  │
│  ┌──────────────────────────────────────────┐    │
│  │ Server Block: ".:53"                      │    │
│  │  ┌─────────┐                             │    │
│  │  │ Config   │                            │    │
│  │  │ Zone:"." │                            │    │
│  │  │ Plugin:  │                            │    │
│  │  │ [forward]│                            │    │
│  │  └─────────┘                             │    │
│  └──────────────────────────────────────────┘    │
└─────────────────────────────────────────────────┘
```

### propagateConfigParams

**소스코드 경로**: `core/dnsserver/register.go`

동일 서버 블록 내 모든 Config는 첫 번째 Config의 플러그인과 설정을 공유한다.

```go
func propagateConfigParams(configs []*Config) {
    for _, c := range configs {
        c.Plugin = c.firstConfigInBlock.Plugin
        c.ListenHosts = c.firstConfigInBlock.ListenHosts
        c.Debug = c.firstConfigInBlock.Debug
        c.TLSConfig = c.firstConfigInBlock.TLSConfig.Clone()
        c.TsigSecret = c.firstConfigInBlock.TsigSecret
        // ...
    }
}
```

## 7. FilterFunc

**소스코드 경로**: `core/dnsserver/config.go`

```go
type FilterFunc func(context.Context, *request.Request) bool
```

`FilterFunc`은 요청이 특정 Config에 매칭되는지 추가로 확인하는 함수다. 주로 `view` 플러그인과 `acl` 플러그인에서 사용된다.

## 8. Server 구조체

**소스코드 경로**: `core/dnsserver/server.go`

```go
type Server struct {
    Addr         string        // 리스닝 주소
    IdleTimeout  time.Duration // TCP 유휴 타임아웃 (기본 10초)
    ReadTimeout  time.Duration // TCP 읽기 타임아웃 (기본 3초)
    WriteTimeout time.Duration // TCP 쓰기 타임아웃 (기본 5초)

    server [2]*dns.Server     // [0]=TCP, [1]=UDP
    m      sync.Mutex

    zones        map[string][]*Config  // Zone별 Config 목록
    graceTimeout time.Duration         // Graceful shutdown 타임아웃 (5초)
    trace        trace.Trace           // 트레이싱 플러그인
    debug        bool                  // 디버그 모드
    classChaos   bool                  // CH 클래스 허용 여부

    tsigSecret   map[string]string     // TSIG 시크릿
    stopOnce     sync.Once             // 멱등 정지
}
```

## 9. Corefile 문법

Corefile은 Caddy의 Caddyfile 문법을 따르며, DNS 서버 블록과 플러그인 디렉티브로 구성된다.

### 기본 구조

```
# Zone:Port 선언
ZONE:PORT {
    DIRECTIVE [arguments...]
    DIRECTIVE {
        SUB_DIRECTIVE [arguments...]
    }
}
```

### 예제

```
# 기본 DNS 서버 (전체 도메인)
.:53 {
    errors                      # 에러 로깅
    log                         # 쿼리 로깅
    health :8080                # 헬스체크 엔드포인트
    prometheus :9153            # 메트릭 엔드포인트
    cache 30                    # 30초 캐시
    forward . 8.8.8.8 8.8.4.4  # Google DNS로 포워딩
}

# Kubernetes Zone
cluster.local:53 {
    errors
    kubernetes cluster.local {
        pods insecure
        fallthrough in-addr.arpa ip6.arpa
    }
    forward . /etc/resolv.conf
    cache 30
}

# DoT 서버
tls://.:853 {
    tls cert.pem key.pem
    forward . 1.1.1.1
}
```

### 문법 요소

| 요소 | 설명 | 예 |
|------|------|----|
| Zone | DNS Zone 이름 | `.`, `example.com`, `10.in-addr.arpa` |
| Port | 리스닝 포트 | `:53`, `:853`, `:443` |
| Transport | 프로토콜 접두사 | `dns://`, `tls://`, `https://`, `quic://`, `grpc://` |
| Directive | 플러그인 이름 | `forward`, `cache`, `log` |
| Block | 중괄호 블록 | `{ ... }` |
| Import | 외부 파일 포함 | `import common.conf` |

## 10. 캐시 아이템 모델

**소스코드 경로**: `plugin/cache/cache.go`

Cache 플러그인은 양성(positive) 캐시와 음성(negative) 캐시를 분리하여 관리한다.

```
Cache 구조체:
┌─────────────────────────────────────────────┐
│  pcache (양성 캐시)                          │
│  ├── 용량: pcap (기본 9984)                  │
│  ├── 최대 TTL: pttl (기본 3600초)            │
│  └── 최소 TTL: minpttl (기본 5초)            │
│                                             │
│  ncache (음성 캐시)                          │
│  ├── 용량: ncap (기본 9984)                  │
│  ├── 최대 TTL: nttl (기본 1800초)            │
│  └── 최소 TTL: minnttl (기본 5초)            │
│                                             │
│  프리페치 설정:                               │
│  ├── prefetch: 히트 수 임계값                │
│  ├── duration: 프리페치 윈도우               │
│  └── percentage: TTL 남은 비율 임계값         │
│                                             │
│  노화 서빙 설정:                              │
│  ├── staleUpTo: 만료 후 서빙 가능 시간        │
│  └── verifyStale: 만료 시 검증 여부           │
└─────────────────────────────────────────────┘
```

### 캐시 키 생성

```go
func hash(qname string, qtype uint16, do, cd bool) uint64 {
    h := fnv.New64()
    // qname + qtype + DO비트 + CD비트를 해시
}
```

캐시 키는 `QNAME + QTYPE + DO비트 + CD비트`의 FNV-64 해시다. 동일 도메인이라도 DNSSEC 비트에 따라 다른 캐시 항목을 사용한다.

## 11. Kubernetes 서비스 모델

**소스코드 경로**: `plugin/kubernetes/kubernetes.go`

Kubernetes 플러그인은 K8s API 서버의 Service/Endpoint 데이터를 DNS 레코드로 매핑한다.

```
K8s 서비스 DNS 스키마:
┌──────────────────────────────────────────────────┐
│ <service>.<namespace>.svc.<zone>                  │
│                                                   │
│ 예: my-svc.default.svc.cluster.local              │
│     → A 레코드: ClusterIP                         │
│                                                   │
│ _<port>._<proto>.<service>.<namespace>.svc.<zone> │
│ 예: _http._tcp.my-svc.default.svc.cluster.local   │
│     → SRV 레코드                                  │
│                                                   │
│ <ip-with-dashes>.<namespace>.pod.<zone>            │
│ 예: 10-0-0-1.default.pod.cluster.local             │
│     → A 레코드: Pod IP (podMode에 따라)            │
└──────────────────────────────────────────────────┘
```

### Pod 모드

| 모드 | 상수 | 동작 |
|------|------|------|
| disabled | `podModeDisabled` | Pod 요청 무시 (기본값) |
| verified | `podModeVerified` | Pod 존재 확인 후 응답 |
| insecure | `podModeInsecure` | 확인 없이 응답 |

## 12. Forward 프록시 모델

**소스코드 경로**: `plugin/forward/forward.go`

```go
type Forward struct {
    proxies    []*proxyPkg.Proxy  // 업스트림 프록시 목록
    p          Policy             // 선택 정책 (random, round_robin 등)
    hcInterval time.Duration      // 헬스체크 간격 (500ms)

    from    string               // 매칭 도메인
    ignored []string             // 제외 도메인

    maxfails       uint32        // 최대 실패 횟수 (기본 2)
    expire         time.Duration // 연결 만료 (기본 10초)
    maxConcurrent  int64         // 최대 동시 쿼리
    maxConnectAttempts uint32    // 최대 연결 시도

    Next plugin.Handler          // 다음 플러그인
}
```

## 13. 데이터 흐름 요약

```
클라이언트 요청
     │
     v
┌──────────┐     ┌───────────┐     ┌────────────┐
│ dns.Msg  │────>│  Request   │────>│  Config    │
│ (원본    │     │  (래핑/    │     │  (Zone     │
│  메시지)  │     │   캐시)    │     │   설정)    │
└──────────┘     └───────────┘     └────────────┘
                      │                   │
                      v                   v
               ┌─────────────────────────────┐
               │    pluginChain.ServeDNS()   │
               │                             │
               │  log → cache → forward      │
               │         │          │        │
               │         v          v        │
               │    ┌────────┐ ┌────────┐   │
               │    │ item   │ │dns.Msg │   │
               │    │(캐시)  │ │(응답)  │   │
               │    └────────┘ └────────┘   │
               └──────────────┬──────────────┘
                              │
                              v
                       ┌─────────────┐
                       │ ScrubWriter │
                       │ (크기 조정)  │
                       └──────┬──────┘
                              │
                              v
                       dns.ResponseWriter
                       (클라이언트 응답)
```
