# CoreDNS 아키텍처

## 1. 개요

CoreDNS는 **플러그인 체인 기반의 DNS 서버**로, Caddy 웹 서버 프레임워크를 DNS 서버 타입으로 확장하여 구현되었다. 모든 DNS 기능(캐싱, 포워딩, 로깅, K8s 서비스 디스커버리 등)이 독립적인 플러그인으로 구현되며, Corefile 설정에 따라 플러그인 체인이 조립되어 DNS 쿼리를 처리한다.

### 설계 철학

| 원칙 | 설명 |
|------|------|
| 플러그인 우선 | 코어는 최소한의 서버 엔진만 제공, 모든 기능은 플러그인 |
| 체인 패턴 | 플러그인은 순서대로 실행, 각 플러그인이 다음 플러그인 호출 결정 |
| Zone 기반 라우팅 | 쿼리 도메인에 따라 적절한 Zone의 플러그인 체인 선택 |
| 다중 프로토콜 | 동일한 플러그인 체인을 DNS/DoT/DoH/DoQ/gRPC에서 재사용 |

## 2. 전체 아키텍처

```
┌─────────────────────────────────────────────────────────────────┐
│                        CoreDNS Process                         │
│                                                                 │
│  ┌──────────────────────────────────────────────────────────┐   │
│  │                    Caddy Framework                        │   │
│  │  ┌─────────────┐  ┌──────────────┐  ┌───────────────┐   │   │
│  │  │ Corefile     │  │ ServerType   │  │  Instance     │   │   │
│  │  │ Parser       │  │ Registration │  │  Lifecycle    │   │   │
│  │  └──────┬──────┘  └──────┬───────┘  └───────┬───────┘   │   │
│  └─────────┼────────────────┼──────────────────┼────────────┘   │
│            │                │                  │                 │
│            v                v                  v                 │
│  ┌─────────────────────────────────────────────────────────┐    │
│  │                   dnsContext                             │    │
│  │  InspectServerBlocks() → MakeServers()                  │    │
│  └───────────────────────┬─────────────────────────────────┘    │
│                          │                                      │
│           ┌──────────────┼──────────────┐                       │
│           v              v              v                       │
│     ┌─────────┐    ┌──────────┐   ┌──────────┐                 │
│     │ Server  │    │ServerTLS │   │ServerQUIC│                  │
│     │UDP + TCP│    │  (DoT)   │   │  (DoQ)   │                  │
│     └────┬────┘    └────┬─────┘   └────┬─────┘                 │
│          │              │              │                        │
│          └──────────────┼──────────────┘                        │
│                         v                                       │
│     ┌──────────────────────────────────────────────────┐        │
│     │              ServeDNS()                          │        │
│     │  Zone 매칭 → Config 선택 → pluginChain.ServeDNS()│        │
│     └──────────────────────┬───────────────────────────┘        │
│                            │                                    │
│     ┌──────────┬───────────┼───────────┬──────────┐             │
│     v          v           v           v          v             │
│  ┌─────┐  ┌──────┐  ┌──────────┐  ┌───────┐  ┌───────┐        │
│  │ log │→│cache │→│  rewrite  │→│forward│→│whoami │        │
│  └─────┘  └──────┘  └──────────┘  └───────┘  └───────┘        │
│              플러그인 체인 (plugin.cfg 순서)                      │
└─────────────────────────────────────────────────────────────────┘
```

## 3. 초기화 흐름

### 3.1 진입점: main() → coremain.Run()

CoreDNS의 진입점은 `coredns.go`의 `main()` 함수다.

```
coredns.go:main()
  └── _ "github.com/coredns/coredns/core/plugin"  // 플러그인 자동 등록 (import side-effect)
  └── coremain.Run()
```

**소스코드 경로**: `coredns.go`

```go
func main() {
    coremain.Run()
}
```

`core/plugin/zplugin.go`는 `go generate`로 자동 생성되며, `plugin.cfg`에 정의된 모든 플러그인을 블랭크 임포트(`_ "..."`)하여 각 플러그인의 `init()` 함수가 실행되도록 한다.

### 3.2 Run() 함수: Corefile 로드 → Caddy 시작

**소스코드 경로**: `coremain/run.go`

```
Run()
  ├── caddy.TrapSignals()              // 시그널 핸들러 등록 (SIGTERM, SIGHUP)
  ├── flag.Parse()                     // CLI 플래그 파싱
  ├── maxprocs.Set()                   // GOMAXPROCS 자동 설정
  ├── caddy.LoadCaddyfile("dns")       // Corefile 로드
  │   ├── confLoader() 또는            // -conf 플래그로 지정된 파일
  │   └── defaultLoader()             // 기본 "./Corefile"
  ├── caddy.Start(corefile)            // 서버 인스턴스 시작
  │   ├── dnsContext.InspectServerBlocks()  // Zone 주소 정규화
  │   ├── 각 directive의 setup 함수 실행     // 플러그인 초기화
  │   ├── dnsContext.MakeServers()          // 서버 그룹 생성
  │   │   ├── propagateConfigParams()      // 블록 내 설정 공유
  │   │   ├── groupConfigsByListenAddr()   // 주소별 Config 그룹핑
  │   │   └── makeServersForGroup()        // 프로토콜별 서버 생성
  │   │       ├── NewServer()              // DNS (UDP/TCP)
  │   │       ├── NewServerTLS()           // DoT
  │   │       ├── NewServerQUIC()          // DoQ
  │   │       ├── NewServergRPC()          // gRPC
  │   │       ├── NewServerHTTPS()         // DoH
  │   │       └── NewServerHTTPS3()        // DoH3
  │   └── Server.Serve() / ServePacket()   // 리스닝 시작
  └── instance.Wait()                 // 종료 대기
```

### 3.3 init() 함수에서의 서버 타입 등록

**소스코드 경로**: `core/dnsserver/register.go`

`dnsserver` 패키지의 `init()` 함수에서 "dns" 서버 타입을 Caddy에 등록한다.

```go
func init() {
    caddy.RegisterServerType(serverType, caddy.ServerType{
        Directives: func() []string { return Directives },
        DefaultInput: func() caddy.Input {
            return caddy.CaddyfileInput{
                Filepath:       "Corefile",
                Contents:       []byte(".:" + Port + " {\nwhoami\nlog\n}\n"),
                ServerTypeName: serverType,
            }
        },
        NewContext: newContext,
    })
}
```

`Directives`는 `core/dnsserver/zdirectives.go`에서 `plugin.cfg`의 순서대로 정의된다.

## 4. 플러그인 체인 구축 과정

### 4.1 plugin.cfg의 역할

`plugin.cfg`는 플러그인의 **실행 순서**를 정의한다. 이 순서가 매우 중요한데, 각 플러그인은 자기 아래(이후)에 있는 플러그인의 영향만 받는다.

```
# plugin.cfg (상위 = 먼저 실행)
root:root
metadata:metadata
cancel:cancel
tls:tls
...
log:log
cache:cache
rewrite:rewrite
...
kubernetes:kubernetes
file:file
forward:forward
whoami:whoami
```

### 4.2 체인 조립: NewServer()

**소스코드 경로**: `core/dnsserver/server.go` (NewServer 함수)

```go
func NewServer(addr string, group []*Config) (*Server, error) {
    // ...
    for _, site := range group {
        var stack plugin.Handler
        // 플러그인 목록을 역순으로 순회하여 체인 구축
        for i := len(site.Plugin) - 1; i >= 0; i-- {
            stack = site.Plugin[i](stack)  // Plugin func: next Handler → new Handler
            site.registerHandler(stack)
        }
        site.pluginChain = stack  // 최종 체인의 헤드
    }
    // ...
}
```

**핵심 원리**: 플러그인 목록을 **역순**으로 순회하면서, 각 `Plugin` 함수에 이전 단계의 `Handler`(= 다음 플러그인)를 전달한다. 이렇게 하면 목록의 첫 번째 플러그인이 체인의 헤드가 된다.

```
Plugin 목록: [log, cache, forward]

역순 조립:
  1. stack = forward(nil)      → forwardHandler{Next: nil}
  2. stack = cache(stack)      → cacheHandler{Next: forwardHandler}
  3. stack = log(stack)        → logHandler{Next: cacheHandler}

결과: log → cache → forward
```

### 4.3 Plugin과 Handler 인터페이스

**소스코드 경로**: `plugin/plugin.go`

```go
type Plugin func(Handler) Handler

type Handler interface {
    ServeDNS(context.Context, dns.ResponseWriter, *dns.Msg) (int, error)
    Name() string
}
```

- `Plugin`: 다음 Handler를 받아 새 Handler를 반환하는 함수 (미들웨어 패턴)
- `Handler`: DNS 요청을 처리하는 인터페이스. `ServeDNS`는 rcode와 error를 반환

## 5. Zone 라우팅

### 5.1 ServeDNS 멀티플렉서

**소스코드 경로**: `core/dnsserver/server.go` (ServeDNS 메서드)

`Server.ServeDNS()`는 쿼리의 QNAME을 기반으로 가장 구체적인(longest match) Zone을 찾아 해당 Zone의 플러그인 체인을 실행한다.

```
쿼리: www.example.com.
Zone 맵: {"example.com.": [config1], ".": [config2]}

매칭 순서:
  1. "www.example.com." → 없음
  2. "example.com."     → config1 발견! → config1.pluginChain.ServeDNS()
```

```go
func (s *Server) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) {
    // EDNS 버전 체크, ScrubWriter 래핑
    w = request.NewScrubWriter(r, w)
    q := strings.ToLower(r.Question[0].Name)

    for {
        if z, ok := s.zones[q[off:]]; ok {
            for _, h := range z {
                if passAllFilterFuncs(ctx, h.FilterFuncs, &request.Request{Req: r, W: w}) {
                    rcode, _ := h.pluginChain.ServeDNS(ctx, w, r)
                    // ...
                    return
                }
            }
        }
        off, end = dns.NextLabel(q, off)
        if end { break }
    }

    // 마지막으로 루트 Zone "." 시도
    if z, ok := s.zones["."]; ok { ... }
    // 매칭 실패 시 REFUSED
}
```

### 5.2 FilterFunc을 통한 뷰(View) 지원

`Config.FilterFuncs`를 통해 동일 Zone에 대해 클라이언트 조건(예: 소스 IP)에 따라 다른 응답을 제공할 수 있다. `view` 플러그인이 이 메커니즘을 사용한다.

```go
type FilterFunc func(context.Context, *request.Request) bool
```

## 6. 프로토콜 지원 아키텍처

CoreDNS는 동일한 Zone/플러그인 설정에 대해 여러 프로토콜 서버를 생성할 수 있다.

### 6.1 프로토콜별 서버 구현

| 프로토콜 | 서버 타입 | 소스 파일 | Corefile 접두사 |
|----------|----------|-----------|----------------|
| DNS (UDP/TCP) | `Server` | `server.go` | `dns://` (기본) |
| DNS-over-TLS | `ServerTLS` | `server_tls.go` | `tls://` |
| DNS-over-QUIC | `ServerQUIC` | `server_quic.go` | `quic://` |
| DNS-over-HTTPS | `ServerHTTPS` | `server_https.go` | `https://` |
| DNS-over-HTTP/3 | `ServerHTTPS3` | `server_https3.go` | `https3://` |
| gRPC | `ServergRPC` | `server_grpc.go` | `grpc://` |

### 6.2 프로토콜 선택 흐름

**소스코드 경로**: `core/dnsserver/register.go` (makeServersForGroup 함수)

```go
func makeServersForGroup(addr string, group []*Config) ([]caddy.Server, error) {
    switch tr, _ := parse.Transport(addr); tr {
    case transport.DNS:
        s, err := NewServer(addr, group)
    case transport.TLS:
        s, err := NewServerTLS(addr, group)
    case transport.QUIC:
        s, err := NewServerQUIC(addr, group)
    case transport.GRPC:
        s, err := NewServergRPC(addr, group)
    case transport.HTTPS:
        s, err := NewServerHTTPS(addr, group)
    case transport.HTTPS3:
        s, err := NewServerHTTPS3(addr, group)
    }
}
```

## 7. 고루틴 구조

```
┌─────────────────────────────────────────────┐
│              Main Goroutine                  │
│  coremain.Run() → caddy.Start()             │
│  → instance.Wait() (블로킹)                  │
└──────────────────────┬──────────────────────┘
                       │
          ┌────────────┼────────────┐
          v            v            v
┌─────────────┐ ┌───────────┐ ┌───────────┐
│ TCP Listener│ │UDP Listener│ │ TLS/QUIC  │
│  Goroutine  │ │ Goroutine  │ │ Listener  │
└──────┬──────┘ └──────┬────┘ └─────┬─────┘
       │               │           │
       v               v           v
  ┌─────────┐    ┌─────────┐  ┌─────────┐
  │Per-Conn │    │Per-Packet│  │Per-Conn │
  │Goroutine│    │Goroutine │  │Goroutine│
  │ServeDNS │    │ServeDNS  │  │ServeDNS │
  └─────────┘    └─────────┘  └─────────┘
       │
       v
  ┌───────────────────────────┐
  │ Plugin Chain Execution    │
  │ (동기적, 단일 고루틴)       │
  │ 단, prefetch 등은          │
  │ 별도 고루틴 스폰            │
  └───────────────────────────┘
```

### 주요 고루틴 패턴

| 고루틴 | 역할 | 소스 위치 |
|--------|------|-----------|
| Main | Corefile 로드, 서버 시작, 종료 대기 | `coremain/run.go` |
| TCP Listener | TCP 연결 수락, per-connection 고루틴 스폰 | `server.go` (Serve) |
| UDP Listener | UDP 패킷 수신, ServeDNS 호출 | `server.go` (ServePacket) |
| Prefetch | 캐시 프리페치를 위한 백그라운드 쿼리 | `plugin/cache/handler.go` |
| Health Check | 업스트림 서버 헬스체크 | `plugin/pkg/proxy/` |
| Metrics HTTP | Prometheus 메트릭 HTTP 서버 | `plugin/metrics/` |
| Health HTTP | 헬스체크 HTTP 서버 | `plugin/health/` |

## 8. 서버 생명주기

```
┌──────────┐    ┌──────────┐    ┌──────────┐    ┌──────────┐
│  Create  │───>│  Listen  │───>│  Serve   │───>│   Stop   │
│(NewServer)│   │(Listen/  │   │(Serve/   │   │(graceful │
│          │    │ListenPkt)│    │ServePkt) │    │shutdown) │
└──────────┘    └──────────┘    └──────────┘    └──────────┘
                                     │
                                     v
                              ┌──────────────┐
                              │   Reload      │
                              │ (SIGHUP →     │
                              │  new instance │
                              │  → swap)      │
                              └──────────────┘
```

### Graceful Shutdown

**소스코드 경로**: `core/dnsserver/server.go` (Stop 메서드)

```go
func (s *Server) Stop() error {
    s.stopOnce.Do(func() {
        ctx, cancelCtx := context.WithTimeout(context.Background(), s.graceTimeout)
        defer cancelCtx()
        // TCP/UDP 서버 모두 종료
        for _, s1 := range s.server {
            if s1 != nil {
                s1.ShutdownContext(ctx)
            }
        }
    })
    return s.stopErr
}
```

- `graceTimeout`: 기본 5초
- `sync.Once`로 멱등성 보장 (동시 호출 시에도 한 번만 실행)

## 9. 핵심 설계 결정과 이유

### 왜 Caddy 프레임워크를 사용하는가?

CoreDNS는 Caddy 웹 서버의 **서버 타입 플러그인**으로 구현되었다. Caddy가 제공하는 기능을 재활용한다:

1. **Corefile 파싱**: Caddyfile 파서를 그대로 사용
2. **서버 생명주기 관리**: Start, Stop, Restart, Reload
3. **시그널 처리**: SIGTERM, SIGHUP (graceful reload)
4. **플러그인 등록 메커니즘**: `caddy.RegisterPlugin()`

### 왜 역순으로 체인을 조립하는가?

미들웨어 패턴에서 각 플러그인은 "다음 핸들러"를 알아야 한다. 역순 조립이면 마지막 플러그인부터 시작하여 각 단계에서 이전 결과를 "다음"으로 전달할 수 있다. 이것은 함수형 프로그래밍의 compose 패턴과 동일하다.

### 왜 Zone 매칭에 longest-match를 사용하는가?

DNS 계층 구조에서 더 구체적인 Zone이 우선해야 한다. 예를 들어 `k8s.example.com`을 별도로 관리하면서 `example.com`의 나머지는 다른 설정을 사용하는 것이 일반적이다. longest-match는 이를 자연스럽게 지원한다.
