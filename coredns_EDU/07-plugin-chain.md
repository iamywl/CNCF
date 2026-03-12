# 07. 플러그인 체인 아키텍처

## 개요

CoreDNS의 핵심 설계 철학은 **모든 기능을 플러그인으로 구현**하는 것이다. DNS 쿼리 처리, 캐싱, 로깅, 포워딩까지 모두 플러그인이며, 이 플러그인들이 **체인(chain)** 형태로 연결되어 요청을 순차적으로 처리한다.

이 문서에서는 CoreDNS 플러그인 체인의 핵심 타입, 체인 구축 과정, 실행 흐름, 응답 추적 메커니즘, 그리고 다양한 플러그인 패턴을 소스코드 수준에서 분석한다.

---

## 1. 핵심 타입 정의 (`plugin/plugin.go`)

### 1.1 Plugin 타입: 고차 함수 패턴

```
소스 위치: plugin/plugin.go:19
```

```go
Plugin func(Handler) Handler
```

`Plugin`은 **고차 함수(Higher-Order Function)** 타입이다. `Handler`를 받아서 새로운 `Handler`를 반환한다. 이것이 CoreDNS 플러그인 체인의 핵심 구조이다.

이 패턴이 중요한 이유:
- **각 플러그인은 자신의 다음(next) 핸들러를 클로저로 캡처**한다
- 플러그인을 역순으로 조립하면 자연스럽게 체인이 형성된다
- 미들웨어 패턴과 동일한 원리이며, Go의 `http.Handler` 미들웨어와 같은 구조

```
개념 다이어그램:

Plugin_A(Plugin_B(Plugin_C(nil)))

요청 → [A] → [B] → [C] → (종료)
응답 ← [A] ← [B] ← [C] ← (종료)
```

### 1.2 Handler 인터페이스

```
소스 위치: plugin/plugin.go:51-54
```

```go
Handler interface {
    ServeDNS(context.Context, dns.ResponseWriter, *dns.Msg) (int, error)
    Name() string
}
```

`Handler`는 CoreDNS 플러그인이 구현해야 하는 핵심 인터페이스이다.

| 메서드 | 설명 |
|--------|------|
| `ServeDNS()` | DNS 요청을 처리하고 응답 코드(rcode)와 에러를 반환 |
| `Name()` | 플러그인 이름 반환 (메트릭, 로깅, 레지스트리에 사용) |

`ServeDNS`의 반환값 `(int, error)` 설계가 표준 DNS 핸들러(`dns.Handler`)와 다른 점이다. 표준 `dns.Handler`는 반환값이 없지만, CoreDNS는 rcode를 반환하여 **응답이 이미 작성되었는지 판단**할 수 있게 했다.

### 1.3 HandlerFunc 타입

```
소스 위치: plugin/plugin.go:59
```

```go
HandlerFunc func(context.Context, dns.ResponseWriter, *dns.Msg) (int, error)
```

`HandlerFunc`은 Go 표준 라이브러리의 `http.HandlerFunc`과 동일한 컨벤션이다. 함수를 `Handler` 인터페이스로 감싸는 어댑터 역할을 한다.

```go
// ServeDNS implements the Handler interface.
func (f HandlerFunc) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
    return f(ctx, w, r)
}

// Name implements the Handler interface.
func (f HandlerFunc) Name() string { return "handlerfunc" }
```

Name()이 항상 "handlerfunc"을 반환하므로, 프로덕션 코드에서는 직접 `Handler` 인터페이스를 구현하는 것이 일반적이다.

---

## 2. NextOrFailure: 체인 연결 함수

```
소스 위치: plugin/plugin.go:74-87
```

```go
func NextOrFailure(name string, next Handler, ctx context.Context,
    w dns.ResponseWriter, r *dns.Msg) (int, error) {
    if next != nil {
        if span := ot.SpanFromContext(ctx); span != nil {
            child := span.Tracer().StartSpan(next.Name(), ot.ChildOf(span.Context()))
            defer child.Finish()
            ctx = ot.ContextWithSpan(ctx, child)
        }
        pw := &pluginWriter{ResponseWriter: w, plugin: next.Name()}
        return next.ServeDNS(ctx, pw, r)
    }
    return dns.RcodeServerFailure, Error(name, errors.New("no next plugin found"))
}
```

`NextOrFailure`는 플러그인 체인에서 **다음 플러그인을 호출하는 표준 방법**이다.

### 핵심 동작:

1. **nil 체크**: next가 nil이면 SERVFAIL 반환 (체인의 끝)
2. **트레이싱 통합**: OpenTracing span이 있으면 자식 span 생성
3. **pluginWriter 래핑**: ResponseWriter를 pluginWriter로 감싸서 어떤 플러그인이 응답을 작성했는지 추적
4. **체인 진행**: `next.ServeDNS()` 호출

### 사용 예시 (forward 플러그인):

```
소스 위치: plugin/forward/forward.go:105
```

```go
func (f *Forward) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
    state := request.Request{W: w, Req: r}
    if !f.match(state) {
        return plugin.NextOrFailure(f.Name(), f.Next, ctx, w, r)
    }
    // ... 포워딩 로직
}
```

forward 플러그인은 요청이 자신이 처리할 도메인에 매치되지 않으면, `NextOrFailure`를 통해 다음 플러그인으로 요청을 넘긴다.

---

## 3. pluginWriter: 응답 추적 메커니즘

```
소스 위치: plugin/plugin.go:90-134
```

### 3.1 PluginTracker 인터페이스

```go
type PluginTracker interface {
    SetPlugin(name string)
    GetPlugin() string
}
```

### 3.2 pluginWriter 구조체

```go
type pluginWriter struct {
    dns.ResponseWriter
    plugin string
}
```

`pluginWriter`는 `dns.ResponseWriter`를 래핑하여 **어떤 플러그인이 실제로 응답을 작성했는지 추적**한다.

### 동작 원리:

```
NextOrFailure 호출 시:
    pw := &pluginWriter{ResponseWriter: w, plugin: next.Name()}
    return next.ServeDNS(ctx, pw, r)

플러그인이 WriteMsg 호출 시:
    func (pw *pluginWriter) WriteMsg(m *dns.Msg) error {
        if tracker, ok := pw.ResponseWriter.(PluginTracker); ok {
            tracker.SetPlugin(pw.plugin)  // 플러그인 이름 기록
        }
        return pw.ResponseWriter.WriteMsg(m)
    }
```

이 메커니즘이 제공하는 기능:
- **메트릭 수집**: 어떤 플러그인이 응답을 생성했는지 Prometheus 메트릭에 기록
- **로깅**: 응답을 생성한 플러그인을 로그에 표시
- **디버깅**: 복잡한 체인에서 어디서 응답이 발생했는지 추적

### pluginWriter가 구현하는 전체 메서드 목록:

| 메서드 | 동작 |
|--------|------|
| `WriteMsg(m *dns.Msg)` | 플러그인 이름 추적 후 원본 writer로 위임 |
| `Write(b []byte)` | 플러그인 이름 추적 후 원본 writer로 위임 |
| `LocalAddr()` | 원본 writer로 위임 |
| `RemoteAddr()` | 원본 writer로 위임 |
| `Close()` | 원본 writer로 위임 |
| `TsigStatus()` | 원본 writer로 위임 |
| `TsigTimersOnly(b bool)` | 원본 writer로 위임 |
| `Hijack()` | 원본 writer로 위임 |

WriteMsg와 Write만 추적 로직이 있고, 나머지는 순수 위임(delegation)이다.

---

## 4. ClientWrite: 응답 코드 판정

```
소스 위치: plugin/plugin.go:137-149
```

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

`ClientWrite`는 **플러그인의 반환 rcode를 기반으로 응답이 이미 클라이언트에 전송되었는지 판단**하는 함수이다.

### 판정 규칙:

| rcode | ClientWrite 반환값 | 의미 |
|-------|-------------------|------|
| SERVFAIL (2) | `false` | 응답 미작성 - 서버가 대신 에러 응답 전송 |
| REFUSED (5) | `false` | 응답 미작성 |
| FORMERR (1) | `false` | 응답 미작성 |
| NOTIMP (4) | `false` | 응답 미작성 |
| NOERROR (0) | `true` | 플러그인이 이미 응답 작성 완료 |
| NXDOMAIN (3) | `true` | 플러그인이 이미 응답 작성 완료 |
| 기타 모든 값 | `true` | 플러그인이 이미 응답 작성 완료 |

### 서버에서의 사용:

```
소스 위치: core/dnsserver/server.go:315-318
```

```go
rcode, _ := h.pluginChain.ServeDNS(ctx, w, r)
if !plugin.ClientWrite(rcode) {
    errorFunc(s.Addr, w, r, rcode)
}
```

플러그인 체인이 SERVFAIL, REFUSED, FORMERR, NOTIMP를 반환하면, 서버가 직접 에러 응답을 작성한다.

---

## 5. 체인 구축: NewServer의 역순 루프

```
소스 위치: core/dnsserver/server.go:68-141
```

### 5.1 역순 루프의 원리

```go
func NewServer(addr string, group []*Config) (*Server, error) {
    // ...
    for _, site := range group {
        var stack plugin.Handler
        for i := len(site.Plugin) - 1; i >= 0; i-- {
            stack = site.Plugin[i](stack)
            site.registerHandler(stack)
            // MetadataCollector 감지
            if mdc, ok := stack.(MetadataCollector); ok {
                site.metaCollector = mdc
            }
            // trace 플러그인 감지
            if s.trace == nil && stack.Name() == "trace" {
                if t, ok := stack.(trace.Trace); ok {
                    s.trace = t
                }
            }
            // CH 클래스 쿼리 허용 플러그인 감지
            if _, ok := EnableChaos[stack.Name()]; ok {
                s.classChaos = true
            }
        }
        site.pluginChain = stack
    }
    // ...
}
```

### 역순 루프가 필요한 이유:

`Plugin` 타입은 `func(Handler) Handler`이다. 체인의 마지막 플러그인부터 조립해야 각 플러그인이 자신의 **다음 핸들러**를 올바르게 참조할 수 있다.

```
site.Plugin = [A_plugin, B_plugin, C_plugin]  (Directives 순서)

역순 루프:
  i=2: stack = C_plugin(nil)       → C_handler (next=nil)
  i=1: stack = B_plugin(C_handler) → B_handler (next=C_handler)
  i=0: stack = A_plugin(B_handler) → A_handler (next=B_handler)

최종: site.pluginChain = A_handler

요청 흐름:
  A_handler.ServeDNS → B_handler.ServeDNS → C_handler.ServeDNS
```

### 5.2 체인 구축 중 수행되는 추가 작업

| 작업 | 조건 | 목적 |
|------|------|------|
| `registerHandler(stack)` | 항상 | 다른 플러그인이 핸들러를 조회할 수 있도록 레지스트리에 등록 |
| MetadataCollector 감지 | `stack.(MetadataCollector)` 성공 시 | 메타데이터 수집기 설정 |
| trace 감지 | `stack.Name() == "trace"` | 서버 전체의 트레이싱 설정 |
| EnableChaos 감지 | chaos/forward/proxy 이름 | CH 클래스 쿼리 허용 |

---

## 6. 실행 순서: Directives 목록

```
소스 위치: core/dnsserver/zdirectives.go
```

Directives 목록은 플러그인의 **실행 순서**를 정의한다. 이 파일은 `go generate`로 자동 생성된다.

### 전체 실행 순서:

```
var Directives = []string{
    "root",           // 1. 기본 디렉토리 설정
    "metadata",       // 2. 메타데이터 수집
    "geoip",          // 3. GeoIP 기반 위치 정보
    "cancel",         // 4. 요청 타임아웃/취소
    "tls",            // 5. TLS 설정
    "proxyproto",     // 6. Proxy Protocol 설정
    "quic",           // 7. QUIC 프로토콜 설정
    "grpc_server",    // 8. gRPC 서버 설정
    "https",          // 9. HTTPS (DoH) 설정
    "https3",         // 10. HTTP/3 (DoH3) 설정
    "timeouts",       // 11. 타임아웃 설정
    "multisocket",    // 12. 다중 소켓 설정
    "reload",         // 13. 설정 리로드
    "nsid",           // 14. NSID 응답
    "bufsize",        // 15. EDNS 버퍼 크기
    "bind",           // 16. 바인드 주소
    "debug",          // 17. 디버그 모드
    "trace",          // 18. 분산 트레이싱
    "ready",          // 19. 레디니스 프로브
    "health",         // 20. 헬스체크
    "pprof",          // 21. 프로파일링
    "prometheus",     // 22. 메트릭 수집
    "errors",         // 23. 에러 로깅
    "log",            // 24. 쿼리 로깅
    "dnstap",         // 25. DNS TAP 로깅
    "local",          // 26. 로컬 레코드
    "dns64",          // 27. DNS64 변환
    "acl",            // 28. 접근 제어
    "any",            // 29. ANY 쿼리 처리
    "chaos",          // 30. CH 클래스 쿼리
    "loadbalance",    // 31. 응답 레코드 순서 무작위화
    "tsig",           // 32. TSIG 인증
    "cache",          // 33. 응답 캐싱
    "rewrite",        // 34. 요청/응답 재작성
    "header",         // 35. 헤더 수정
    "dnssec",         // 36. DNSSEC 서명
    "autopath",       // 37. 자동 검색 경로
    "minimal",        // 38. 최소 응답
    "template",       // 39. 템플릿 응답
    "transfer",       // 40. 존 전송
    "hosts",          // 41. /etc/hosts 파일
    "route53",        // 42. AWS Route53
    "azure",          // 43. Azure DNS
    "clouddns",       // 44. Google Cloud DNS
    "k8s_external",   // 45. K8s 외부 DNS
    "kubernetes",     // 46. Kubernetes DNS
    "file",           // 47. 존 파일
    "auto",           // 48. 자동 존 로드
    "secondary",      // 49. 세컨더리 존
    "etcd",           // 50. etcd 백엔드
    "loop",           // 51. 루프 감지
    "forward",        // 52. DNS 포워딩
    "grpc",           // 53. gRPC 프록시
    "erratic",        // 54. 테스트용 에러 생성
    "whoami",         // 55. 클라이언트 정보 반환
    "on",             // 56. 이벤트 핸들러
    "sign",           // 57. DNSSEC 서명
    "view",           // 58. 뷰 기반 라우팅
    "nomad",          // 59. HashiCorp Nomad
}
```

### 순서가 중요한 이유:

```
순서 규칙: "Every plugin will feel the effects of all other plugin
           below (after) them during a request"

예시: cache(33번)가 forward(52번)보다 앞에 있으므로:

요청 → cache → ... → forward → 업스트림
                                    ↓
응답 ← cache ← ... ← forward ← 업스트림 응답

1. 요청이 cache에 먼저 도달
2. 캐시 히트면 즉시 응답 (forward까지 가지 않음)
3. 캐시 미스면 forward까지 도달
4. 응답이 돌아오면서 cache가 결과를 캐싱
```

### 순서 설계 원칙:

| 영역 | 순서 위치 | 이유 |
|------|----------|------|
| 설정/인프라 (root, tls, bind) | 최상위 | 서버 인프라 설정 우선 |
| 관측성 (trace, prometheus, log) | 상위 | 모든 요청을 관측 가능 |
| 수정/필터 (acl, cache, rewrite) | 중간 | 요청/응답 변환 |
| 백엔드 (kubernetes, file, forward) | 하위 | 실제 데이터 소스 |
| 테스트/유틸 (erratic, whoami) | 최하위 | 폴백 또는 디버깅용 |

---

## 7. 플러그인 등록 메커니즘

### 7.1 plugin.Register()

```
소스 위치: plugin/register.go
```

```go
func Register(name string, action caddy.SetupFunc) {
    caddy.RegisterPlugin(name, caddy.Plugin{
        ServerType: "dns",
        Action:     action,
    })
}
```

`Register`는 플러그인을 Caddy 프레임워크에 등록한다. `ServerType: "dns"`로 DNS 서버 타입에 한정된다.

### 7.2 init() 함수를 통한 자동 등록

모든 플러그인은 `init()` 함수에서 자신을 등록한다:

```go
// plugin/whoami/setup.go
func init() { plugin.Register("whoami", setup) }

// plugin/cache/setup.go
func init() { plugin.Register("cache", setup) }

// plugin/forward/setup.go
func init() { plugin.Register("forward", setup) }

// plugin/log/setup.go
func init() { plugin.Register("log", setup) }
```

### 7.3 zplugin.go: 임포트 기반 등록

```
소스 위치: core/plugin/zplugin.go
```

이 파일은 `go generate`로 자동 생성되며, 모든 내장 플러그인을 blank import(`_`)로 임포트한다:

```go
package plugin

import (
    _ "github.com/coredns/coredns/plugin/acl"
    _ "github.com/coredns/coredns/plugin/any"
    _ "github.com/coredns/coredns/plugin/auto"
    _ "github.com/coredns/coredns/plugin/cache"
    _ "github.com/coredns/coredns/plugin/forward"
    _ "github.com/coredns/coredns/plugin/kubernetes"
    // ... 전체 플러그인 목록
)
```

Go의 blank import는 패키지의 `init()` 함수만 실행하므로, 임포트하는 것만으로 모든 플러그인이 Caddy에 등록된다.

### 등록 전체 흐름:

```
┌─────────────────────────────────────────────────────────────┐
│  1. coredns.go: import _ "github.com/coredns/coredns/core/plugin"  │
│                                                                      │
│  2. core/plugin/zplugin.go:                                         │
│     import _ "github.com/coredns/coredns/plugin/cache"              │
│     import _ "github.com/coredns/coredns/plugin/forward"            │
│     import _ "github.com/coredns/coredns/plugin/kubernetes"         │
│     ...                                                              │
│                                                                      │
│  3. 각 플러그인의 init():                                            │
│     plugin.Register("cache", setup)                                  │
│     plugin.Register("forward", setup)                                │
│     plugin.Register("kubernetes", setup)                             │
│     ...                                                              │
│                                                                      │
│  4. caddy 내부에 (이름 → SetupFunc) 매핑 저장                       │
└─────────────────────────────────────────────────────────────┘
```

---

## 8. setup 함수 패턴

모든 플러그인의 setup 함수는 동일한 패턴을 따른다.

### 8.1 가장 단순한 형태: whoami

```
소스 위치: plugin/whoami/setup.go
```

```go
func setup(c *caddy.Controller) error {
    c.Next() // 'whoami' 디렉티브 자체를 소비
    if c.NextArg() {
        return plugin.Error("whoami", c.ArgErr())
    }

    dnsserver.GetConfig(c).AddPlugin(func(next plugin.Handler) plugin.Handler {
        return Whoami{}
    })

    return nil
}
```

### 8.2 설정 파싱이 있는 형태: cache

```
소스 위치: plugin/cache/setup.go:21-38
```

```go
func setup(c *caddy.Controller) error {
    ca, err := cacheParse(c)
    if err != nil {
        return plugin.Error("cache", err)
    }

    c.OnStartup(func() error {
        ca.viewMetricLabel = dnsserver.GetConfig(c).ViewName
        return nil
    })

    dnsserver.GetConfig(c).AddPlugin(func(next plugin.Handler) plugin.Handler {
        ca.Next = next
        return ca
    })

    return nil
}
```

### 8.3 복잡한 형태: forward (다중 인스턴스)

```
소스 위치: plugin/forward/setup.go:28-57
```

```go
func setup(c *caddy.Controller) error {
    fs, err := parseForward(c)
    if err != nil {
        return plugin.Error("forward", err)
    }
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
        c.OnStartup(func() error {
            return f.OnStartup()
        })
        // ...
    }
    return nil
}
```

### setup 함수 공통 패턴 정리:

```
setup(c *caddy.Controller) error
│
├── 1. c.Next()로 디렉티브 이름 소비
│
├── 2. c.RemainingArgs(), c.NextBlock()으로 설정 파싱
│
├── 3. dnsserver.GetConfig(c).AddPlugin(...)으로 플러그인 등록
│   └── func(next plugin.Handler) plugin.Handler {
│       └── handler.Next = next  // 다음 핸들러 연결
│       └── return handler
│   }
│
├── 4. c.OnStartup(...)으로 시작 시 콜백 등록 (선택)
│
├── 5. c.OnShutdown(...)으로 종료 시 콜백 등록 (선택)
│
└── return nil 또는 error
```

### AddPlugin 메서드:

```
소스 위치: core/dnsserver/register.go:183-185
```

```go
func (c *Config) AddPlugin(m plugin.Plugin) {
    c.Plugin = append(c.Plugin, m)
}
```

Config의 `Plugin` 필드(`[]plugin.Plugin`)에 플러그인 팩토리 함수를 추가한다. 나중에 `NewServer`에서 역순으로 조립된다.

---

## 9. 플러그인 유형별 패턴

### 9.1 Terminal 플러그인 (종단 플러그인)

체인에서 요청을 직접 처리하고, 다음 플러그인을 호출하지 않는 유형이다.

**예시: whoami**

```
소스 위치: plugin/whoami/whoami.go:22-57
```

```go
func (wh Whoami) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
    state := request.Request{W: w, Req: r}
    a := new(dns.Msg)
    a.SetReply(r)
    a.Authoritative = true
    // ... 응답 구성
    w.WriteMsg(a)
    return 0, nil  // 체인 진행 없음, 여기서 종료
}
```

Terminal 플러그인의 특징:
- `NextOrFailure`를 호출하지 않음
- 자체적으로 응답을 생성하고 `w.WriteMsg(a)` 호출
- rcode 0 (NOERROR) 반환

### 9.2 Wrapper 플러그인 (래퍼 플러그인)

요청을 가로채서 전처리/후처리하고, 다음 플러그인에 위임하는 유형이다.

**예시: log**

```
소스 위치: plugin/log/setup.go:23-25
```

```go
dnsserver.GetConfig(c).AddPlugin(func(next plugin.Handler) plugin.Handler {
    return Logger{Next: next, Rules: rules, repl: replacer.New()}
})
```

Logger는 요청을 로깅한 후 `Next.ServeDNS()`를 호출하여 체인을 계속 진행한다.

Wrapper 플러그인의 특징:
- `Next` 필드를 가짐
- `NextOrFailure` 또는 `Next.ServeDNS()` 호출
- 전처리/후처리 로직만 추가

### 9.3 Conditional 플러그인 (조건부 플러그인)

조건에 따라 자체 처리 또는 다음 플러그인 위임을 결정하는 유형이다.

**예시: forward**

```
소스 위치: plugin/forward/forward.go:102-106
```

```go
func (f *Forward) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
    state := request.Request{W: w, Req: r}
    if !f.match(state) {
        return plugin.NextOrFailure(f.Name(), f.Next, ctx, w, r)
    }
    // ... 포워딩 로직 (자체 처리)
}
```

Conditional 플러그인의 특징:
- 조건(도메인 매치, ACL 등)에 따라 분기
- 매치하면 자체 처리 (Terminal처럼)
- 매치하지 않으면 `NextOrFailure`로 체인 진행 (Wrapper처럼)

### 9.4 Hybrid 플러그인 (혼합 플러그인)

**예시: cache**

```
소스 위치: plugin/cache/cache.go:21-24
```

```go
type Cache struct {
    Next  plugin.Handler
    Zones []string
    // ...
}
```

cache는 다음과 같이 동작한다:
- 캐시 히트: Terminal처럼 즉시 응답 반환
- 캐시 미스: Next.ServeDNS() 호출 후 결과를 캐싱 (Wrapper처럼)
- 도메인 불일치: NextOrFailure로 체인 진행 (Conditional처럼)

---

## 10. Handler 레지스트리

```
소스 위치: core/dnsserver/register.go:190-229
```

### 10.1 registerHandler

```go
func (c *Config) registerHandler(h plugin.Handler) {
    if c.registry == nil {
        c.registry = make(map[string]plugin.Handler)
    }
    c.registry[h.Name()] = h
}
```

체인 구축 시 각 핸들러가 레지스트리에 등록된다. 같은 이름의 핸들러가 있으면 덮어쓴다.

### 10.2 Handler 조회

```go
func (c *Config) Handler(name string) plugin.Handler {
    if c.registry == nil {
        return nil
    }
    if h, ok := c.registry[name]; ok {
        return h
    }
    return nil
}
```

다른 플러그인이 특정 플러그인의 존재 여부를 확인하거나, 상호작용할 때 사용한다.

```go
// forward setup에서 dnstap 플러그인 존재 여부 확인
if taph := dnsserver.GetConfig(c).Handler("dnstap"); taph != nil {
    // dnstap 플러그인과 연동
}
```

### 10.3 순서 의존성

레지스트리는 Directives 순서에 따라 채워진다. 따라서:
- forward(52번)의 setup에서 dnstap(25번)을 조회할 수 있음 (이미 등록됨)
- 하지만 dnstap(25번)의 setup에서 forward(52번)을 조회할 수 없음 (아직 등록 안 됨)

---

## 11. ErrOnce: 중복 설정 방지

```
소스 위치: plugin/plugin.go:165
```

```go
var ErrOnce = errors.New("this plugin can only be used once per Server Block")
```

일부 플러그인은 서버 블록당 한 번만 설정할 수 있다. 이를 위해 setup 함수에서 카운터를 관리한다:

```go
// plugin/cache/setup.go:44-47
j := 0
for c.Next() {
    if j > 0 {
        return nil, plugin.ErrOnce
    }
    j++
    // ...
}
```

---

## 12. 에러 처리 유틸리티

```
소스 위치: plugin/plugin.go:71
```

```go
func Error(name string, err error) error {
    return fmt.Errorf("%s/%s: %w", "plugin", name, err)
}
```

모든 플러그인 에러에 `plugin/이름:` 접두사를 붙여 에러의 출처를 명확히 한다.

---

## 13. 메트릭 관련 상수

```
소스 위치: plugin/plugin.go:152-162
```

```go
const Namespace = "coredns"

var TimeBuckets = prometheus.ExponentialBuckets(0.00025, 2, 16)      // 0.25ms ~ 8s
var SlimTimeBuckets = prometheus.ExponentialBuckets(0.00025, 10, 5)  // 0.25ms ~ 2.5s
var NativeHistogramBucketFactor = 1.05
```

| 상수/변수 | 용도 |
|----------|------|
| `Namespace` | Prometheus 메트릭 네임스페이스 ("coredns") |
| `TimeBuckets` | 상세 히스토그램용 버킷 (16단계) |
| `SlimTimeBuckets` | 저카디널리티 히스토그램용 버킷 (5단계) |
| `NativeHistogramBucketFactor` | 네이티브 히스토그램 해상도 |

---

## 14. 체인 실행 전체 흐름 다이어그램

```
DNS 쿼리 수신
     │
     ▼
Server.ServeDNS()
     │
     ├── 1. Question 섹션 검증
     ├── 2. panic recovery 설정
     ├── 3. INET 클래스 검증 (CH 차단)
     ├── 4. EDNS 버전 검증
     ├── 5. ScrubWriter 래핑
     │
     ▼
Zone 매칭 (longest suffix match)
     │
     ├── s.zones[q[off:]] 검색
     ├── FilterFuncs 통과 여부 확인
     │
     ▼
h.pluginChain.ServeDNS(ctx, w, r)
     │
     ▼
┌──────────────────────────────────────────────┐
│  Plugin Chain                                 │
│                                               │
│  metadata → cancel → prometheus → errors →    │
│  log → cache → rewrite → kubernetes →         │
│  forward → whoami                             │
│                                               │
│  각 플러그인:                                  │
│  1. 자체 로직 실행                             │
│  2. NextOrFailure() 또는 직접 응답             │
│     └── pluginWriter로 ResponseWriter 래핑    │
│     └── 다음 플러그인의 ServeDNS() 호출        │
└──────────────────────────────────────────────┘
     │
     ▼
rcode 반환
     │
     ├── ClientWrite(rcode) == true  → 이미 응답 전송됨
     └── ClientWrite(rcode) == false → errorFunc()로 에러 응답 전송
```

---

## 15. 플러그인 체인과 HTTP 미들웨어 비교

| 측면 | CoreDNS Plugin Chain | Go HTTP Middleware |
|------|---------------------|-------------------|
| 타입 | `func(Handler) Handler` | `func(http.Handler) http.Handler` |
| 인터페이스 | `ServeDNS(ctx, w, r) (int, error)` | `ServeHTTP(w, r)` |
| 반환값 | rcode + error | 없음 |
| 체인 연결 | `NextOrFailure` | 직접 `next.ServeHTTP` 호출 |
| 응답 추적 | `pluginWriter` | 없음 (직접 구현 필요) |
| 순서 관리 | `Directives` 전역 배열 | 수동 조립 |

CoreDNS의 추가 기능:
- rcode 기반 응답 상태 판정 (`ClientWrite`)
- 자동 응답 추적 (`pluginWriter`)
- 글로벌 순서 관리 (`Directives`, `plugin.cfg`)

---

## 16. 외부 플러그인 추가 방법

### 16.1 plugin.cfg 수정

```
# plugin.cfg에 추가
myplugin:github.com/myorg/coredns-myplugin
```

### 16.2 go generate 실행

```bash
go generate && go build
```

이 명령은:
1. `directives_generate.go`를 실행
2. `plugin.cfg`를 파싱하여
3. `core/plugin/zplugin.go` (임포트 목록) 재생성
4. `core/dnsserver/zdirectives.go` (Directives 배열) 재생성

### 16.3 코드 생성기 (`directives_generate.go`)

```
소스 위치: directives_generate.go:13-60
```

```go
func main() {
    mi := make(map[string]string, 0)  // name → import path
    md := []string{}                    // directive 순서

    // plugin.cfg 파싱
    file, _ := os.Open(pluginFile)
    scanner := bufio.NewScanner(file)
    for scanner.Scan() {
        line := scanner.Text()
        if strings.HasPrefix(line, "#") { continue }
        // "name:package" 형식 파싱
        parsePlugin(line)
    }

    // COREDNS_PLUGINS 환경변수도 처리
    for _, element := range strings.Split(os.Getenv("COREDNS_PLUGINS"), ",") {
        parsePlugin(element)
    }

    genImports("core/plugin/zplugin.go", "plugin", mi)
    genDirectives("core/dnsserver/zdirectives.go", "dnsserver", md)
}
```

---

## 17. 정리: 플러그인 체인 핵심 원리

```
┌─────────────────────────────────────────────────┐
│  1. 등록: init() → plugin.Register(name, setup) │
│                                                  │
│  2. 설정: setup(c) → GetConfig(c).AddPlugin(fn)  │
│     fn = func(next Handler) Handler { ... }      │
│                                                  │
│  3. 조립: NewServer → 역순 루프                   │
│     for i := len(plugins)-1; i >= 0; i--         │
│       stack = plugins[i](stack)                   │
│                                                  │
│  4. 실행: ServeDNS → pluginChain.ServeDNS         │
│     각 플러그인: NextOrFailure(next) 호출          │
│                                                  │
│  5. 추적: pluginWriter가 응답 작성 플러그인 기록   │
│                                                  │
│  6. 판정: ClientWrite(rcode)로 응답 상태 확인      │
└─────────────────────────────────────────────────┘
```

CoreDNS의 플러그인 체인은 단순한 고차 함수 조합이지만, pluginWriter를 통한 응답 추적, ClientWrite를 통한 상태 판정, Directives를 통한 전역 순서 관리 등 DNS 서버에 특화된 기능을 추가하여 강력한 확장성과 관측성을 제공한다.
