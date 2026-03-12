# 09. Corefile 설정 파싱

## 개요

Corefile은 CoreDNS의 설정 파일이다. Caddy 웹 서버의 Caddyfile 문법을 기반으로 하되, DNS 서버에 특화된 존(zone) 개념과 프로토콜 지원이 추가되었다.

이 문서에서는 Corefile의 문법 구조, 파싱 과정, 서버 블록에서 실제 서버 인스턴스가 생성되기까지의 전체 흐름을 소스코드 수준에서 분석한다.

---

## 1. Corefile 문법

### 1.1 기본 구조

```
[transport://]zone[:port] [transport://]zone[:port] ... {
    plugin_directive [args...]
    plugin_directive [args...] {
        sub_directive [args...]
    }
}
```

### 1.2 서버 블록(Server Block)

Corefile은 하나 이상의 서버 블록으로 구성된다. 각 서버 블록은 **존 이름**과 **플러그인 디렉티브**로 이루어진다.

```
# 단일 존 서버 블록
example.com {
    forward . 8.8.8.8
    log
    errors
}

# 다중 존 서버 블록 (같은 플러그인 공유)
example.com example.org {
    forward . 8.8.8.8
    log
}

# 루트 존 (모든 쿼리 처리)
. {
    forward . /etc/resolv.conf
    cache 30
    log
}
```

### 1.3 프로토콜(Transport) 지정

```
# DNS-over-TLS
tls://example.com {
    tls cert.pem key.pem
    forward . 8.8.8.8
}

# DNS-over-QUIC
quic://example.com {
    quic
    forward . 8.8.8.8
}

# DNS-over-gRPC
grpc://example.com {
    grpc_server
    forward . 8.8.8.8
}

# DNS-over-HTTPS (DoH)
https://example.com {
    https
    forward . 8.8.8.8
}
```

### 1.4 포트 지정

```
# 기본 포트 (프로토콜별 다름)
example.com {          # dns://  → 53
    ...
}

# 커스텀 포트
example.com:1053 {
    ...
}

# 여러 존에 같은 포트
example.com:1053 example.org:1053 {
    ...
}
```

### 프로토콜별 기본 포트

| 프로토콜 | 접두사 | 기본 포트 |
|---------|--------|----------|
| DNS | `dns://` (또는 없음) | 53 |
| DNS-over-TLS | `tls://` | 853 |
| DNS-over-QUIC | `quic://` | 853 |
| DNS-over-gRPC | `grpc://` | 443 |
| DNS-over-HTTPS | `https://` | 443 |
| DNS-over-HTTPS3 | `https3://` | 443 |

### 1.5 역방향 존(Reverse Zone)

```
# CIDR 표기법으로 역방향 존 지정
10.0.0.0/24 {
    ...
}
# → 0.0.10.in-addr.arpa. 로 확장

# 비옥텟 경계 (예: /17)
10.0.0.0/17 {
    ...
}
# → 여러 역방향 존으로 확장
```

### 1.6 플러그인 디렉티브 문법

```
# 인수 없음
log

# 단일 인수
cache 30

# 복수 인수
forward . 8.8.8.8 8.8.4.4

# 블록 인수
cache 30 {
    success 10000 3600
    denial 5000 300
    prefetch 10 1m
}

# 복합
forward . 8.8.8.8 {
    max_fails 3
    expire 10s
    force_tcp
}
```

---

## 2. 기본 설정 (DefaultInput)

```
소스 위치: core/dnsserver/register.go:19-31
```

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

Corefile이 없으면 기본 설정이 적용된다:

```
.:53 {
    whoami
    log
}
```

이 기본 설정은:
- 루트 존(".")에서 모든 쿼리를 처리
- `whoami`로 클라이언트 정보 반환
- `log`로 쿼리 로깅

### RegisterServerType의 세 가지 구성요소:

| 구성요소 | 역할 |
|---------|------|
| `Directives` | 허용되는 디렉티브 목록 및 실행 순서 (zdirectives.go) |
| `DefaultInput` | Corefile 없을 때 기본 설정 |
| `NewContext` | 서버 블록 파싱 컨텍스트 생성 |

---

## 3. dnsContext: 파싱 컨텍스트

```
소스 위치: core/dnsserver/register.go:33-47
```

```go
func newContext(i *caddy.Instance) caddy.Context {
    return &dnsContext{keysToConfigs: make(map[string]*Config)}
}

type dnsContext struct {
    keysToConfigs map[string]*Config  // "블록인덱스:키인덱스" → Config
    configs       []*Config           // 전체 Config 마스터 목록
}

func (h *dnsContext) saveConfig(key string, cfg *Config) {
    h.configs = append(h.configs, cfg)
    h.keysToConfigs[key] = cfg
}
```

`dnsContext`는 Corefile 파싱의 전체 생명주기를 관리한다.

| 필드 | 역할 |
|------|------|
| `keysToConfigs` | 블록/키 인덱스로 Config를 빠르게 조회 |
| `configs` | 모든 Config의 순서 보장 마스터 목록 |

### keyForConfig: 설정 식별

```
소스 위치: core/dnsserver/config.go:123-125
```

```go
func keyForConfig(blocIndex int, blocKeyIndex int) string {
    return fmt.Sprintf("%d:%d", blocIndex, blocKeyIndex)
}
```

- `blocIndex`: 서버 블록의 순서 (0부터)
- `blocKeyIndex`: 서버 블록 내 존 키의 순서 (0부터)

예시:
```
# 블록 0
example.com example.org {  # 키 0:0, 0:1
    ...
}

# 블록 1
staging.example.com {       # 키 1:0
    ...
}
```

---

## 4. InspectServerBlocks: 존 정규화와 검증

```
소스 위치: core/dnsserver/register.go:55-135
```

`InspectServerBlocks`는 Caddy가 Corefile을 파싱한 후, 디렉티브를 실행하기 전에 호출된다. 서버 블록의 키(존 이름)를 검증하고 정규화한다.

### 4.1 전체 흐름

```go
func (h *dnsContext) InspectServerBlocks(sourceFile string,
    serverBlocks []caddyfile.ServerBlock) ([]caddyfile.ServerBlock, error) {

    for ib, s := range serverBlocks {
        zoneAddrs := []zoneAddr{}
        for ik, k := range s.Keys {
            // 1. Transport 파싱 (dns://, tls:// 등)
            trans, k1 := parse.Transport(k)

            // 2. 호스트와 포트 분리
            hosts, port, err := plugin.SplitHostPort(k1)

            // 3. FQDN 유효성 검증
            for ih := range hosts {
                _, _, err := plugin.SplitHostPort(dns.Fqdn(hosts[ih]))
            }

            // 4. 기본 포트 설정
            if port == "" {
                switch trans {
                case transport.DNS:   port = Port
                case transport.TLS:   port = transport.TLSPort
                case transport.QUIC:  port = transport.QUICPort
                case transport.GRPC:  port = transport.GRPCPort
                case transport.HTTPS: port = transport.HTTPSPort
                case transport.HTTPS3: port = transport.HTTPSPort
                }
            }

            // 5. 다중 호스트 처리 (역방향 존 확장)
            if len(hosts) > 1 {
                s.Keys[ik] = hosts[0] + ":" + port
                for _, h := range hosts[1:] {
                    s.Keys = append(s.Keys, h+":"+port)
                }
            }

            // 6. zoneAddr 생성
            for i := range hosts {
                zoneAddrs = append(zoneAddrs, zoneAddr{
                    Zone: dns.Fqdn(hosts[i]),
                    Port: port,
                    Transport: trans,
                })
            }
        }

        // 7. 키 업데이트 및 Config 생성
        serverBlocks[ib].Keys = s.Keys
        var firstConfigInBlock *Config
        for ik := range s.Keys {
            za := zoneAddrs[ik]
            s.Keys[ik] = za.String()
            cfg := &Config{
                Zone:        za.Zone,
                ListenHosts: []string{""},
                Port:        za.Port,
                Transport:   za.Transport,
            }
            if ik == 0 {
                firstConfigInBlock = cfg
            }
            cfg.firstConfigInBlock = firstConfigInBlock
            keyConfig := keyForConfig(ib, ik)
            h.saveConfig(keyConfig, cfg)
        }
    }
    return serverBlocks, nil
}
```

### 4.2 주요 처리 단계 상세

#### Transport 파싱

```go
trans, k1 := parse.Transport(k)
```

입력: `"tls://example.com:853"` → trans=`"tls"`, k1=`"example.com:853"`
입력: `"example.com"` → trans=`"dns"`, k1=`"example.com"`

#### CIDR 역방향 존 확장

```go
hosts, port, err := plugin.SplitHostPort(k1)
```

```
소스 위치: plugin/normalize.go:135
```

`SplitHostPort`는 CIDR 표기법을 역방향 DNS 존 이름으로 확장한다:
- `10.0.0.0/24` → `["0.0.10.in-addr.arpa."]` (1개)
- `10.0.0.0/17` → `["0.0.10.in-addr.arpa.", "128.0.10.in-addr.arpa.", ...]` (여러 개)

CIDR이 옥텟 경계에 맞지 않으면 여러 역방향 존으로 확장된다.

#### FQDN 검증

```go
for ih := range hosts {
    _, _, err := plugin.SplitHostPort(dns.Fqdn(hosts[ih]))
    if err != nil {
        return nil, err
    }
}
```

도메인 이름을 FQDN(Fully Qualified Domain Name)으로 변환한 후 유효성을 검증한다. 주석에 따르면, 퍼징에서 발견된 버그(`"ȶ"`는 OK이지만 `"ȶ."`은 실패)를 방지하기 위한 것이다.

#### firstConfigInBlock

```go
if ik == 0 {
    firstConfigInBlock = cfg
}
cfg.firstConfigInBlock = firstConfigInBlock
```

서버 블록의 첫 번째 Config를 기록한다. 나중에 `propagateConfigParams`에서 이 참조를 사용하여 같은 블록의 모든 존이 동일한 플러그인 인스턴스를 공유하게 한다.

---

## 5. MakeServers: 서버 인스턴스 생성

```
소스 위치: core/dnsserver/register.go:138-180
```

```go
func (h *dnsContext) MakeServers() ([]caddy.Server, error) {
    // 1. 설정 전파
    propagateConfigParams(h.configs)

    // 2. 주소별 그룹화
    groups, err := groupConfigsByListenAddr(h.configs)
    if err != nil {
        return nil, err
    }

    // 3. 그룹별 서버 생성
    var servers []caddy.Server
    for addr, group := range groups {
        serversForGroup, err := makeServersForGroup(addr, group)
        if err != nil {
            return nil, err
        }
        servers = append(servers, serversForGroup...)
    }

    // 4. View Filter 설정
    for _, c := range h.configs {
        for _, d := range Directives {
            if vf, ok := c.registry[d].(Viewer); ok {
                if c.ViewName != "" {
                    return nil, fmt.Errorf("multiple views defined in server block")
                }
                c.ViewName = vf.ViewName()
                c.FilterFuncs = append(c.FilterFuncs, vf.Filter)
            }
        }
    }

    // 5. 존 겹침 검증
    errValid := h.validateZonesAndListeningAddresses()
    if errValid != nil {
        return nil, errValid
    }

    return servers, nil
}
```

### MakeServers의 5단계 처리

```
┌──────────────────────────────────────┐
│ MakeServers()                        │
│                                      │
│ 1. propagateConfigParams()           │
│    └── 블록 내 설정 공유              │
│                                      │
│ 2. groupConfigsByListenAddr()        │
│    └── 주소별 Config 그룹화           │
│                                      │
│ 3. makeServersForGroup()             │
│    └── 프로토콜별 서버 생성           │
│                                      │
│ 4. Viewer 인터페이스 감지             │
│    └── FilterFuncs 설정              │
│                                      │
│ 5. validateZonesAndListeningAddresses│
│    └── 존 겹침/중복 검증              │
└──────────────────────────────────────┘
```

---

## 6. propagateConfigParams: 블록 내 설정 공유

```
소스 위치: core/dnsserver/register.go:262-277
```

```go
func propagateConfigParams(configs []*Config) {
    for _, c := range configs {
        c.Plugin = c.firstConfigInBlock.Plugin
        c.ListenHosts = c.firstConfigInBlock.ListenHosts
        c.Debug = c.firstConfigInBlock.Debug
        c.Stacktrace = c.firstConfigInBlock.Stacktrace
        c.NumSockets = c.firstConfigInBlock.NumSockets
        c.TLSConfig = c.firstConfigInBlock.TLSConfig.Clone()
        c.ReadTimeout = c.firstConfigInBlock.ReadTimeout
        c.WriteTimeout = c.firstConfigInBlock.WriteTimeout
        c.IdleTimeout = c.firstConfigInBlock.IdleTimeout
        c.TsigSecret = c.firstConfigInBlock.TsigSecret
    }
}
```

### 전파되는 설정과 이유

| 설정 | 공유 방식 | 이유 |
|------|----------|------|
| `Plugin` | 직접 참조 복사 | 같은 블록의 모든 존이 같은 플러그인 체인 사용 |
| `ListenHosts` | 직접 참조 복사 | bind 플러그인은 블록 단위 설정 |
| `Debug` | 값 복사 | 블록 전체에 적용 |
| `Stacktrace` | 값 복사 | 블록 전체에 적용 |
| `NumSockets` | 값 복사 | multisocket은 블록 단위 설정 |
| `TLSConfig` | **Clone()으로 복사** | 보안 설정은 독립적으로 관리 |
| 타임아웃들 | 값 복사 | 블록 단위 설정 |
| `TsigSecret` | 직접 참조 복사 | TSIG는 블록 단위 설정 |

**TLSConfig만 Clone()으로 깊은 복사하는 이유**: TLS 설정은 존별로 독립적인 인증서 상태를 가질 수 있으므로, 참조가 아닌 복사본을 사용한다.

### 예시

```
# 이 서버 블록에서:
example.com example.org {
    forward . 8.8.8.8
    cache 30
}

# 결과:
# config_example_com.Plugin == config_example_org.Plugin (같은 슬라이스 참조)
# 두 존 모두 동일한 forward+cache 플러그인 체인을 사용
```

---

## 7. groupConfigsByListenAddr: 주소별 그룹화

```
소스 위치: core/dnsserver/register.go:284-298
```

```go
func groupConfigsByListenAddr(configs []*Config) (map[string][]*Config, error) {
    groups := make(map[string][]*Config)
    for _, conf := range configs {
        for _, h := range conf.ListenHosts {
            addr, err := net.ResolveTCPAddr("tcp", net.JoinHostPort(h, conf.Port))
            if err != nil {
                return nil, err
            }
            addrstr := conf.Transport + "://" + addr.String()
            groups[addrstr] = append(groups[addrstr], conf)
        }
    }
    return groups, nil
}
```

### 그룹화 원리

같은 `transport://ip:port`를 공유하는 Config들이 하나의 서버 인스턴스에서 처리된다.

```
Corefile:
  example.com:53     { forward . 8.8.8.8 }
  example.org:53     { forward . 1.1.1.1 }
  staging.example.com:1053 { forward . 10.0.0.1 }
  tls://secure.com   { tls cert.pem key.pem; forward . 8.8.8.8 }

그룹화 결과:
  "dns://0.0.0.0:53"   → [config_example_com, config_example_org]  → Server 1개
  "dns://0.0.0.0:1053"  → [config_staging]                          → Server 1개
  "tls://0.0.0.0:853"   → [config_secure]                           → ServerTLS 1개
```

### net.ResolveTCPAddr의 역할

호스트가 비어있으면 `0.0.0.0`으로 해석한다. 이것이 기본 바인드 동작이다.

---

## 8. makeServersForGroup: 프로토콜별 서버 생성

```
소스 위치: core/dnsserver/register.go:303-362
```

```go
func makeServersForGroup(addr string, group []*Config) ([]caddy.Server, error) {
    if len(group) == 0 {
        return nil, fmt.Errorf("no configs for group defined")
    }
    numSockets := 1
    if group[0].NumSockets > 0 {
        numSockets = group[0].NumSockets
    }

    var servers []caddy.Server
    for range numSockets {
        switch tr, _ := parse.Transport(addr); tr {
        case transport.DNS:
            s, err := NewServer(addr, group)
            servers = append(servers, s)
        case transport.TLS:
            s, err := NewServerTLS(addr, group)
            servers = append(servers, s)
        case transport.QUIC:
            s, err := NewServerQUIC(addr, group)
            servers = append(servers, s)
        case transport.GRPC:
            s, err := NewServergRPC(addr, group)
            servers = append(servers, s)
        case transport.HTTPS:
            s, err := NewServerHTTPS(addr, group)
            servers = append(servers, s)
        case transport.HTTPS3:
            s, err := NewServerHTTPS3(addr, group)
            servers = append(servers, s)
        }
    }
    return servers, nil
}
```

### NumSockets와 다중 서버 인스턴스

`multisocket` 플러그인으로 `NumSockets`를 설정하면, 같은 주소에 대해 여러 서버 인스턴스가 생성된다.

```
example.com {
    multisocket 4
    forward . 8.8.8.8
}

→ NewServer() 4번 호출
→ 4개의 Server 인스턴스가 같은 포트에서 SO_REUSEPORT로 리스닝
→ 커널이 요청을 4개 소켓에 분산
```

---

## 9. Viewer 인터페이스와 View 필터

```
소스 위치: core/dnsserver/view.go
```

```go
type Viewer interface {
    Filter(ctx context.Context, req *request.Request) bool
    ViewName() string
}
```

MakeServers에서 Viewer를 감지하는 코드:

```go
for _, c := range h.configs {
    for _, d := range Directives {
        if vf, ok := c.registry[d].(Viewer); ok {
            if c.ViewName != "" {
                return nil, fmt.Errorf("multiple views defined in server block")
            }
            c.ViewName = vf.ViewName()
            c.FilterFuncs = append(c.FilterFuncs, vf.Filter)
        }
    }
}
```

| 규칙 | 설명 |
|------|------|
| Directives 순서로 감지 | 필터 함수의 평가 순서를 일관되게 유지 |
| 서버 블록당 최대 1개 뷰 | 두 번째 Viewer 발견 시 에러 반환 |

---

## 10. validateZonesAndListeningAddresses: 존 겹침 검증

```
소스 위치: core/dnsserver/register.go:231-256
```

```go
func (h *dnsContext) validateZonesAndListeningAddresses() error {
    checker := newOverlapZone()
    for _, conf := range h.configs {
        for _, h := range conf.ListenHosts {
            akey := zoneAddr{
                Transport: conf.Transport,
                Zone:      conf.Zone,
                Address:   h,
                Port:      conf.Port,
            }
            var existZone, overlapZone *zoneAddr
            if len(conf.FilterFuncs) > 0 {
                existZone, overlapZone = checker.check(akey)
            } else {
                existZone, overlapZone = checker.registerAndCheck(akey)
            }
            if existZone != nil {
                return fmt.Errorf("cannot serve %s - it is already defined", akey.String())
            }
            if overlapZone != nil {
                return fmt.Errorf("cannot serve %s - zone overlap listener capacity with %v",
                    akey.String(), overlapZone.String())
            }
        }
    }
    return nil
}
```

### zoneOverlap 검증 로직

```
소스 위치: core/dnsserver/address.go:40-86
```

```go
type zoneOverlap struct {
    registeredAddr map[zoneAddr]zoneAddr
    unboundOverlap map[zoneAddr]zoneAddr
}
```

#### registerAndCheck 동작

```go
func (zo *zoneOverlap) registerAndCheck(z zoneAddr) (*zoneAddr, *zoneAddr) {
    existingZone, overlappingZone := zo.check(z)
    if existingZone != nil || overlappingZone != nil {
        return existingZone, overlappingZone
    }
    zo.registeredAddr[z] = z
    zo.unboundOverlap[z.unbound()] = z
    return nil, nil
}
```

#### check 동작

```go
func (zo *zoneOverlap) check(z zoneAddr) (*zoneAddr, *zoneAddr) {
    // 1. 정확히 같은 존이 이미 등록됨
    if exist, ok := zo.registeredAddr[z]; ok {
        return &exist, nil
    }
    // 2. 바인드되지 않은 주소와의 겹침 확인
    uz := z.unbound()
    if already, ok := zo.unboundOverlap[uz]; ok {
        if z.Address == "" {
            // 현재 바인드 안 됨, 하지만 바인드된 것이 이미 있음
            return nil, &already
        }
        if _, ok := zo.registeredAddr[uz]; ok {
            // 현재 바인드됨, 하지만 바인드 안 된 것이 이미 있음
            return nil, &uz
        }
    }
    return nil, nil
}
```

### 겹침 검증 규칙

```
허용:
  example.com:53 on 10.0.0.1
  example.com:53 on 10.0.0.2
  → 서로 다른 바인드 주소이므로 OK

거부 (이미 정의됨):
  example.com:53
  example.com:53
  → 정확히 같은 존+포트 중복

거부 (겹침):
  example.com:53 on 10.0.0.1
  example.com:53          (바인드 없음 = 모든 주소)
  → 바인드된 것과 바인드되지 않은 것이 겹침
```

### FilterFuncs와의 관계

필터가 있는 Config(View 사용)는 `check`만 수행하고 등록하지 않는다. 같은 존에 다른 뷰를 정의하는 것은 허용되기 때문이다.

```
# 이것은 허용됨 (같은 존, 다른 뷰)
example.com {
    view internal { ... }
    forward . 10.0.0.1
}

example.com {
    forward . 8.8.8.8
}
```

---

## 11. caddy.Controller: 디렉티브 파싱 인터페이스

플러그인의 setup 함수는 `*caddy.Controller`를 받아서 Corefile의 디렉티브를 파싱한다.

### 11.1 주요 메서드

| 메서드 | 동작 | 반환값 |
|--------|------|--------|
| `c.Next()` | 다음 토큰으로 이동 | bool (토큰 있으면 true) |
| `c.NextBlock()` | 블록 내 다음 토큰으로 이동 | bool |
| `c.Val()` | 현재 토큰의 문자열 값 | string |
| `c.NextArg()` | 같은 줄의 다음 인수로 이동 | bool |
| `c.RemainingArgs()` | 현재 줄의 남은 인수들 | []string |
| `c.ArgErr()` | 인수 에러 생성 | error |
| `c.OnStartup(fn)` | 서버 시작 시 콜백 등록 | - |
| `c.OnShutdown(fn)` | 서버 종료 시 콜백 등록 | - |
| `c.ServerBlockIndex` | 현재 서버 블록 인덱스 | int |
| `c.ServerBlockKeyIndex` | 현재 키 인덱스 | int |
| `c.ServerBlockKeys` | 서버 블록의 모든 키 | []string |

### 11.2 파싱 패턴 예시

#### 가장 단순한 형태 (whoami)

```
소스 위치: plugin/whoami/setup.go:11-22
```

```go
func setup(c *caddy.Controller) error {
    c.Next()  // 디렉티브 이름 "whoami" 소비
    if c.NextArg() {
        return plugin.Error("whoami", c.ArgErr())  // 인수 없어야 함
    }
    dnsserver.GetConfig(c).AddPlugin(func(next plugin.Handler) plugin.Handler {
        return Whoami{}
    })
    return nil
}
```

Corefile: `whoami`

#### 인수와 블록이 있는 형태 (cache)

```
소스 위치: plugin/cache/setup.go:40-258
```

```go
func cacheParse(c *caddy.Controller) (*Cache, error) {
    ca := New()
    j := 0
    for c.Next() {  // 디렉티브 반복 (여러 번 나올 수 있는 경우)
        if j > 0 {
            return nil, plugin.ErrOnce  // cache는 한 번만 허용
        }
        j++

        args := c.RemainingArgs()  // "cache 30 example.com" → ["30", "example.com"]
        if len(args) > 0 {
            ttl, err := strconv.Atoi(args[0])
            if err == nil {
                ca.pttl = time.Duration(ttl) * time.Second
                ca.nttl = time.Duration(ttl) * time.Second
                args = args[1:]
            }
        }
        origins := plugin.OriginsFromArgsOrServerBlock(args, c.ServerBlockKeys)

        for c.NextBlock() {  // 블록 내부 파싱
            switch c.Val() {
            case "success":
                args := c.RemainingArgs()
                // ...
            case "denial":
                args := c.RemainingArgs()
                // ...
            case "prefetch":
                args := c.RemainingArgs()
                // ...
            case "serve_stale":
                args := c.RemainingArgs()
                // ...
            case "servfail":
                args := c.RemainingArgs()
                // ...
            case "disable":
                args := c.RemainingArgs()
                // ...
            case "keepttl":
                // ...
            default:
                return nil, c.ArgErr()
            }
        }
    }
    return ca, nil
}
```

#### 복수 인수 형태 (log)

```
소스 위치: plugin/log/setup.go:30-102
```

```go
func logParse(c *caddy.Controller) ([]Rule, error) {
    var rules []Rule
    for c.Next() {
        args := c.RemainingArgs()
        switch len(args) {
        case 0:
            rules = append(rules, Rule{NameScope: ".", Format: DefaultLogFormat})
        case 1:
            rules = append(rules, Rule{NameScope: dns.Fqdn(args[0])})
        default:
            format := DefaultLogFormat
            if strings.Contains(args[len(args)-1], "{") {
                format = args[len(args)-1]
                args = args[:len(args)-1]
            }
            for _, str := range args {
                rules = append(rules, Rule{NameScope: dns.Fqdn(str), Format: format})
            }
        }
        // 블록 내 class 파싱
        for c.NextBlock() {
            switch c.Val() {
            case "class":
                classesArgs := c.RemainingArgs()
                // ...
            }
        }
    }
    return rules, nil
}
```

---

## 12. GetConfig: 설정 컨텍스트 접근

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
    // 테스트에서만 도달 (정상 흐름에서는 InspectServerBlocks에서 생성됨)
    ctx.saveConfig(key, &Config{ListenHosts: []string{""}})
    return GetConfig(c)
}
```

모든 플러그인 setup 함수에서 사용하는 핵심 함수이다:

```go
// cache setup에서
dnsserver.GetConfig(c).AddPlugin(func(next plugin.Handler) plugin.Handler {
    ca.Next = next
    return ca
})

// forward setup에서
dnsserver.GetConfig(c).AddPlugin(func(next plugin.Handler) plugin.Handler {
    f.Next = next
    return f
})
```

---

## 13. plugin.cfg: 플러그인 빌드 순서 정의

```
소스 위치: plugin.cfg
```

```
# 형식: <plugin-name>:<package-name>
root:root
metadata:metadata
geoip:geoip
cancel:cancel
tls:tls
quic:quic
grpc_server:grpc_server
https:https
https3:https3
timeouts:timeouts
multisocket:multisocket
reload:reload
nsid:nsid
bufsize:bufsize
bind:bind
debug:debug
trace:trace
ready:ready
health:health
pprof:pprof
prometheus:metrics    # 디렉티브명 != 패키지명
errors:errors
log:log
dnstap:dnstap
local:local
dns64:dns64
any:any
chaos:chaos
loadbalance:loadbalance
tsig:tsig
cache:cache
rewrite:rewrite
acl:acl
header:header
dnssec:dnssec
autopath:autopath
minimal:minimal
template:template
transfer:transfer
hosts:hosts
route53:route53
azure:azure
clouddns:clouddns
k8s_external:k8s_external
kubernetes:kubernetes
file:file
auto:auto
secondary:secondary
etcd:etcd
loop:loop
forward:forward
grpc:grpc
erratic:erratic
whoami:whoami
on:github.com/coredns/caddy/onevent   # 외부 패키지
sign:sign
view:view
nomad:nomad
```

### plugin.cfg 형식 규칙

```
# 내부 플러그인 (짧은 형식)
<directive-name>:<package-directory-name>
# 예: cache:cache → github.com/coredns/coredns/plugin/cache

# 내부 플러그인 (디렉티브명과 패키지명이 다른 경우)
prometheus:metrics → 디렉티브는 "prometheus", 패키지는 plugin/metrics

# 외부 플러그인 (완전한 임포트 경로)
on:github.com/coredns/caddy/onevent
```

### 순서가 결정하는 것

plugin.cfg의 순서는 두 가지를 결정한다:

1. **Directives 배열의 순서** → 플러그인 실행 순서
2. **zplugin.go의 임포트 순서** → 빌드에 포함되는 플러그인

---

## 14. go generate: 코드 자동 생성

```
소스 위치: coredns.go:3-4
```

```go
//go:generate go run directives_generate.go
//go:generate go run owners_generate.go
```

### directives_generate.go의 동작

```
소스 위치: directives_generate.go:13-60
```

```go
func main() {
    mi := make(map[string]string, 0)  // name → import path
    md := []string{}                    // directive 이름 순서

    parsePlugin := func(element string) {
        items := strings.Split(element, ":")
        name, repo := items[0], items[1]
        md = append(md, name)
        mi[name] = pluginPath + repo  // 기본: github.com/coredns/coredns/plugin/<repo>

        if _, err := os.Stat(pluginFSPath + repo); err != nil {
            mi[name] = repo  // 파일시스템에 없으면 외부 패키지
        }
    }

    // plugin.cfg 파싱
    file, _ := os.Open(pluginFile)
    scanner := bufio.NewScanner(file)
    for scanner.Scan() {
        line := scanner.Text()
        if strings.HasPrefix(line, "#") { continue }
        parsePlugin(line)
    }

    // COREDNS_PLUGINS 환경변수 추가 처리
    for _, element := range strings.Split(os.Getenv("COREDNS_PLUGINS"), ",") {
        parsePlugin(element)
    }

    genImports("core/plugin/zplugin.go", "plugin", mi)
    genDirectives("core/dnsserver/zdirectives.go", "dnsserver", md)
}
```

### 생성 파일 1: zplugin.go

```go
// generated by directives_generate.go; DO NOT EDIT
package plugin

import (
    _ "github.com/coredns/coredns/plugin/acl"
    _ "github.com/coredns/coredns/plugin/any"
    _ "github.com/coredns/coredns/plugin/auto"
    _ "github.com/coredns/coredns/plugin/cache"
    // ...
    _ "github.com/coredns/caddy/onevent"  // 외부 패키지
)
```

### 생성 파일 2: zdirectives.go

```go
// generated by directives_generate.go; DO NOT EDIT
package dnsserver

var Directives = []string{
    "root",
    "metadata",
    "geoip",
    // ... plugin.cfg 순서 그대로
    "nomad",
}
```

### COREDNS_PLUGINS 환경변수

```bash
# 빌드 시 추가 플러그인 주입
COREDNS_PLUGINS="myplugin:github.com/myorg/coredns-myplugin" go generate && go build
```

plugin.cfg를 수정하지 않고도 빌드 시 플러그인을 추가할 수 있다.

---

## 15. 외부 플러그인 vs 내부 플러그인 판별

```
소스 위치: directives_generate.go:32-34
```

```go
if _, err := os.Stat(pluginFSPath + repo); err != nil {
    mi[name] = repo  // 외부 패키지: 경로를 그대로 사용
}
```

판별 로직:
1. `plugin/<repo>` 디렉토리가 파일시스템에 존재하는지 확인
2. 존재하면 내부 플러그인: `github.com/coredns/coredns/plugin/<repo>`
3. 존재하지 않으면 외부 플러그인: `<repo>` 경로 그대로 사용

```
내부: cache:cache       → github.com/coredns/coredns/plugin/cache
외부: on:github.com/coredns/caddy/onevent → github.com/coredns/caddy/onevent
```

---

## 16. zoneAddr: 존 주소 구조체

```
소스 위치: core/dnsserver/address.go:9-23
```

```go
type zoneAddr struct {
    Zone      string // "example.com."
    Port      string // "53"
    Transport string // "dns", "tls", "grpc"
    Address   string // 바인드 주소 (검증용)
}

func (z zoneAddr) String() string {
    s := z.Transport + "://" + z.Zone + ":" + z.Port
    if z.Address != "" {
        s += " on " + z.Address
    }
    return s
}
```

### SplitProtocolHostPort

```
소스 위치: core/dnsserver/address.go:26-38
```

```go
func SplitProtocolHostPort(address string) (protocol, ip, port string, err error) {
    parts := strings.Split(address, "://")
    switch len(parts) {
    case 1:
        ip, port, err := net.SplitHostPort(parts[0])
        return "", ip, port, err
    case 2:
        ip, port, err := net.SplitHostPort(parts[1])
        return parts[0], ip, port, err
    default:
        return "", "", "", fmt.Errorf("provided value is not in an address format : %s", address)
    }
}
```

---

## 17. 전체 파싱 흐름 다이어그램

```
┌─────────────────────────────────────────────────────────┐
│                    Corefile 파싱 흐름                      │
├─────────────────────────────────────────────────────────┤
│                                                          │
│  1. 빌드 시 (go generate)                                 │
│     plugin.cfg ──→ zplugin.go (임포트)                    │
│                 ──→ zdirectives.go (순서)                  │
│                                                          │
│  2. 초기화 시 (init)                                      │
│     zplugin.go의 blank import                             │
│     → 각 플러그인 init() 실행                             │
│     → plugin.Register(name, setup) 호출                   │
│     → Caddy에 모든 플러그인 등록                          │
│                                                          │
│  3. Caddy 시작                                            │
│     RegisterServerType("dns", ...)                        │
│     → Directives, DefaultInput, NewContext 등록            │
│                                                          │
│  4. Corefile 파싱                                         │
│     Caddy Caddyfile Parser                                │
│     → ServerBlock 목록 생성                               │
│                                                          │
│  5. InspectServerBlocks()                                 │
│     ├── Transport 파싱 (dns://, tls://)                   │
│     ├── 호스트/포트 분리                                   │
│     ├── CIDR 역방향 존 확장                                │
│     ├── FQDN 유효성 검증                                   │
│     ├── 기본 포트 할당                                     │
│     └── Config 생성 및 저장                                │
│                                                          │
│  6. 디렉티브 실행 (Directives 순서)                        │
│     각 플러그인의 setup(c *caddy.Controller) 호출           │
│     ├── c.Next(), c.RemainingArgs() 등으로 인수 파싱       │
│     ├── GetConfig(c).AddPlugin(fn) 으로 플러그인 등록      │
│     └── c.OnStartup(), c.OnShutdown() 콜백 등록           │
│                                                          │
│  7. MakeServers()                                         │
│     ├── propagateConfigParams() → 블록 내 설정 전파        │
│     ├── groupConfigsByListenAddr() → 주소별 그룹화         │
│     ├── makeServersForGroup() → 프로토콜별 서버 생성       │
│     ├── Viewer 감지 → FilterFuncs 설정                    │
│     └── validateZonesAndListeningAddresses() → 겹침 검증  │
│                                                          │
│  8. 서버 시작                                              │
│     각 Server의 Listen() + Serve()                        │
│                  ListenPacket() + ServePacket()            │
└─────────────────────────────────────────────────────────┘
```

---

## 18. 실전 Corefile 예시와 파싱 결과

### 예시 Corefile

```
example.com:53 {
    log
    cache 30
    forward . 8.8.8.8 8.8.4.4
}

staging.example.com:53 {
    log
    forward . 10.0.0.1
}

tls://secure.example.com:853 {
    tls /etc/ssl/cert.pem /etc/ssl/key.pem
    forward . 8.8.8.8
}
```

### 파싱 결과

```
InspectServerBlocks 결과:
  Config 0: Zone="example.com.", Port="53", Transport="dns"
  Config 1: Zone="staging.example.com.", Port="53", Transport="dns"
  Config 2: Zone="secure.example.com.", Port="853", Transport="tls"

groupConfigsByListenAddr 결과:
  "dns://0.0.0.0:53"  → [Config 0, Config 1]
  "tls://0.0.0.0:853" → [Config 2]

makeServersForGroup 결과:
  Server 1: NewServer("dns://0.0.0.0:53", [Config 0, Config 1])
    zones: {
      "example.com.":         [Config 0],
      "staging.example.com.": [Config 1],
    }
  Server 2: NewServerTLS("tls://0.0.0.0:853", [Config 2])
    zones: {
      "secure.example.com.":  [Config 2],
    }

DNS 요청 "web.staging.example.com." 처리:
  1. Server 1의 ServeDNS 호출
  2. "web.staging.example.com." 검색 → 없음
  3. "staging.example.com." 검색 → Config 1 발견
  4. Config 1의 pluginChain.ServeDNS() 호출
  5. log → forward → 10.0.0.1로 포워딩
```

---

## 19. 정리

Corefile 파싱 시스템의 핵심 설계:

| 설계 결정 | 이유 |
|----------|------|
| Caddy 프레임워크 기반 | 성숙한 설정 파싱, 서버 관리, 리로드 메커니즘 활용 |
| plugin.cfg + go generate | 컴파일 타임에 플러그인 구성 결정, 바이너리 크기 최적화 |
| InspectServerBlocks | 디렉티브 실행 전 선검증으로 빠른 에러 감지 |
| firstConfigInBlock | 같은 블록의 존이 플러그인 인스턴스를 공유하여 메모리 절약 |
| zoneOverlap 검증 | 잘못된 설정(존 겹침)의 조기 감지 |
| Viewer/FilterFuncs | 같은 존에 대해 클라이언트별 다른 처리 가능 |

CoreDNS의 Corefile 파싱은 단순한 설정 읽기를 넘어서, 존 정규화, 역방향 존 확장, 프로토콜별 서버 생성, 그리고 존 겹침 검증까지 포괄하는 완전한 설정 관리 시스템이다.
