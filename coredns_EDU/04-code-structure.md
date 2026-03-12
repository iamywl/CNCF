# CoreDNS 코드 구조

## 1. 디렉토리 구조

```
coredns/
├── coredns.go                  # 진입점: main() → coremain.Run()
├── coremain/                   # 메인 실행 로직
│   └── run.go                  # Run() 함수, Corefile 로드, caddy.Start()
│
├── core/                       # 코어 엔진
│   ├── dnsserver/              # DNS 서버 구현
│   │   ├── server.go           # Server 구조체, ServeDNS(), Zone 라우팅
│   │   ├── server_tls.go       # DoT 서버 (ServerTLS)
│   │   ├── server_quic.go      # DoQ 서버 (ServerQUIC)
│   │   ├── server_https.go     # DoH 서버 (ServerHTTPS)
│   │   ├── server_https3.go    # DoH3 서버 (ServerHTTPS3)
│   │   ├── server_grpc.go      # gRPC 서버 (ServergRPC)
│   │   ├── config.go           # Config 구조체, FilterFunc
│   │   ├── register.go         # dnsContext, InspectServerBlocks, MakeServers
│   │   ├── address.go          # zoneAddr, 주소 파싱
│   │   ├── view.go             # Viewer 인터페이스
│   │   ├── onstartup.go        # 시작 시 Zone 정보 출력
│   │   ├── zdirectives.go      # [생성] plugin.cfg 기반 디렉티브 순서
│   │   └── *_test.go           # 단위 테스트
│   └── plugin/
│       └── zplugin.go          # [생성] 플러그인 블랭크 임포트
│
├── plugin/                     # 플러그인 구현 (50+)
│   ├── plugin.go               # Plugin/Handler 인터페이스 정의
│   │
│   ├── # === DNS 데이터 소스 플러그인 ===
│   ├── file/                   # Zone 파일 기반 권한 서버
│   ├── auto/                   # 자동 Zone 파일 로드
│   ├── secondary/              # 세컨더리 서버 (AXFR)
│   ├── hosts/                  # /etc/hosts 파일 기반
│   ├── etcd/                   # etcd 백엔드
│   ├── kubernetes/             # Kubernetes 서비스 디스커버리
│   ├── route53/                # AWS Route53
│   ├── azure/                  # Azure DNS
│   ├── clouddns/               # Google Cloud DNS
│   ├── k8s_external/           # 외부 K8s 서비스
│   ├── nomad/                  # HashiCorp Nomad
│   │
│   ├── # === 쿼리 처리 플러그인 ===
│   ├── forward/                # 업스트림 포워딩
│   ├── grpc/                   # gRPC 포워딩
│   ├── cache/                  # DNS 응답 캐싱
│   ├── rewrite/                # 쿼리/응답 재작성
│   ├── template/               # 템플릿 기반 응답 생성
│   ├── loadbalance/            # A/AAAA/MX 레코드 셔플링
│   ├── autopath/               # 자동 검색 경로
│   ├── dns64/                  # DNS64 변환
│   ├── any/                    # ANY 쿼리 차단
│   ├── minimal/                # 최소 응답 (추가 섹션 제거)
│   ├── header/                 # DNS 헤더 수정
│   ├── loop/                   # 루프 감지
│   │
│   ├── # === 보안/인증 플러그인 ===
│   ├── tls/                    # TLS 설정
│   ├── quic/                   # QUIC 설정
│   ├── https/                  # HTTPS (DoH) 설정
│   ├── https3/                 # HTTP/3 (DoH3) 설정
│   ├── grpc_server/            # gRPC 서버 설정
│   ├── dnssec/                 # DNSSEC 서명
│   ├── tsig/                   # TSIG 인증
│   ├── acl/                    # 접근 제어
│   ├── sign/                   # Zone 서명
│   │
│   ├── # === 관측성/운영 플러그인 ===
│   ├── log/                    # 쿼리 로깅
│   ├── errors/                 # 에러 로깅
│   ├── metrics/                # Prometheus 메트릭
│   ├── trace/                  # 분산 트레이싱
│   ├── dnstap/                 # DNSTap 프로토콜 로깅
│   ├── health/                 # 헬스체크 엔드포인트
│   ├── ready/                  # 레디니스 프로브
│   ├── pprof/                  # Go pprof 프로파일링
│   │
│   ├── # === 서버 설정 플러그인 ===
│   ├── bind/                   # 바인드 주소 설정
│   ├── root/                   # 루트 디렉토리 설정
│   ├── debug/                  # 디버그 모드
│   ├── reload/                 # 설정 핫 리로드
│   ├── cancel/                 # 요청 취소 컨텍스트
│   ├── metadata/               # 메타데이터 수집
│   ├── bufsize/                # EDNS 버퍼 크기 설정
│   ├── nsid/                   # NSID 옵션
│   ├── chaos/                  # CH 클래스 응답
│   ├── whoami/                 # 클라이언트 정보 반환
│   ├── erratic/                # 테스트용 (에러 주입)
│   ├── local/                  # 로컬 Zone 응답
│   ├── timeouts/               # 서버 타임아웃 설정
│   ├── multisocket/            # 멀티 소켓 설정
│   ├── view/                   # 뷰 기반 응답 분기
│   ├── geoip/                  # GeoIP 기반 메타데이터
│   ├── transfer/               # Zone 전송 (AXFR/IXFR)
│   ├── proxyproto/             # PROXY 프로토콜
│   │
│   ├── # === 공유 패키지 ===
│   ├── pkg/                    # 플러그인 간 공유 유틸리티
│   │   ├── cache/              # 제네릭 캐시 (shard + LRU)
│   │   ├── dnsutil/            # DNS 유틸리티 함수
│   │   ├── edns/               # EDNS0 처리
│   │   ├── fall/               # Fallthrough 설정
│   │   ├── log/                # 로깅 유틸리티
│   │   ├── parse/              # 주소/호스트 파싱
│   │   ├── proxy/              # 프록시 공통 (Proxy, HealthCheck)
│   │   ├── rcode/              # Rcode 유틸리티
│   │   ├── response/           # 응답 타입 분류
│   │   ├── reuseport/          # SO_REUSEPORT 지원
│   │   ├── trace/              # 트레이싱 인터페이스
│   │   ├── transport/          # 프로토콜 상수 정의
│   │   └── proxyproto/         # PROXY 프로토콜 PacketConn
│   │
│   └── test/                   # 플러그인 테스트 헬퍼
│
├── request/                    # Request 래퍼
│   ├── request.go              # Request 구조체 및 메서드
│   └── writer.go               # ScrubWriter
│
├── pb/                         # Protobuf 정의
│
├── test/                       # 통합 테스트
│   ├── server.go               # 테스트 서버 헬퍼
│   ├── *_test.go               # 통합 테스트 (50+)
│   └── fuzz_corefile.go        # 퍼징 테스트
│
├── man/                        # 매뉴얼 페이지
├── notes/                      # 릴리스 노트
│
├── plugin.cfg                  # 플러그인 실행 순서 정의
├── Makefile                    # 빌드 시스템
├── Dockerfile                  # Docker 이미지 빌드
├── go.mod                      # Go 모듈 정의
├── go.sum                      # 의존성 체크섬
│
├── directives_generate.go      # zdirectives.go 생성기
├── owners_generate.go          # CODEOWNERS 생성기
│
├── coredns.1.md                # man page 소스
├── corefile.5.md               # Corefile man page 소스
└── Makefile.doc / .docker / .release
```

## 2. Go 모듈

### go.mod 핵심 정보

**소스코드 경로**: `go.mod`

```
module github.com/coredns/coredns
go 1.25.0
```

### 주요 의존성

| 의존성 | 버전 | 역할 |
|--------|------|------|
| `github.com/miekg/dns` | v1.1.72 | DNS 프로토콜 라이브러리 (핵심) |
| `github.com/coredns/caddy` | v1.1.4+ | 서버 프레임워크 (Corefile 파싱, 생명주기) |
| `github.com/prometheus/client_golang` | v1.23.0 | Prometheus 메트릭 |
| `k8s.io/client-go` | v0.35.2 | Kubernetes API 클라이언트 |
| `k8s.io/api` | v0.35.2 | Kubernetes API 타입 |
| `github.com/quic-go/quic-go` | v0.59.0 | QUIC 프로토콜 지원 |
| `google.golang.org/grpc` | v1.79.2 | gRPC 서버/클라이언트 |
| `go.etcd.io/etcd/client/v3` | v3.6.8 | etcd 클라이언트 |
| `github.com/dnstap/golang-dnstap` | v0.4.0 | DNSTap 프로토콜 |
| `github.com/opentracing/opentracing-go` | v1.2.0 | 분산 트레이싱 |
| `go.uber.org/automaxprocs` | v1.6.0 | GOMAXPROCS 자동 설정 |

### 모듈 구조 특징

CoreDNS는 **단일 모듈** 구조를 사용한다. 모든 플러그인이 같은 모듈에 포함되어 있어, 외부 플러그인을 추가하려면 `plugin.cfg`를 수정하고 재빌드해야 한다.

## 3. 빌드 시스템

### Makefile

**소스코드 경로**: `Makefile`

```makefile
BINARY:=coredns
CGO_ENABLED?=0
LDFLAGS?=-ldflags="$(STRIP_FLAGS) -X github.com/coredns/coredns/coremain.GitCommit=$(GITCOMMIT)"

coredns: $(CHECKS)
    CGO_ENABLED=$(CGO_ENABLED) $(SYSTEM) go build $(BUILDOPTS) -tags="$(GOTAGS)" $(LDFLAGS) -o $(BINARY)

check: core/plugin/zplugin.go core/dnsserver/zdirectives.go

core/plugin/zplugin.go core/dnsserver/zdirectives.go: plugin.cfg
    go generate coredns.go
    go get
```

### 빌드 흐름

```
┌──────────────┐
│  plugin.cfg  │ ← 플러그인 실행 순서 정의
└──────┬───────┘
       │
       v
┌──────────────────────────────────────────┐
│  go generate coredns.go                  │
│  (directives_generate.go 실행)           │
├──────────────────────────────────────────┤
│  생성 파일:                               │
│  ├── core/plugin/zplugin.go              │
│  │   (플러그인 블랭크 임포트)              │
│  └── core/dnsserver/zdirectives.go       │
│      (Directives 슬라이스)               │
└──────┬───────────────────────────────────┘
       │
       v
┌──────────────────────────────────────────┐
│  go build -o coredns                     │
│  -ldflags: GitCommit 주입                │
│  -tags: grpcnotrace (기본)               │
│  CGO_ENABLED=0 (정적 바이너리)            │
└──────────────────────────────────────────┘
```

### go generate 디렉티브

**소스코드 경로**: `coredns.go`

```go
//go:generate go run directives_generate.go
//go:generate go run owners_generate.go
```

- `directives_generate.go`: `plugin.cfg`를 파싱하여 `zplugin.go`와 `zdirectives.go`를 생성
- `owners_generate.go`: CODEOWNERS 파일 생성

### plugin.cfg 형식

```
# <plugin-name>:<package-name>
# 또는
# <plugin-name>:<fully-qualified-package-name>

root:root
cache:cache
forward:forward
kubernetes:kubernetes

# 외부 플러그인 예:
# myplugin:github.com/user/myplugin
```

### 외부 플러그인 추가 방법

```bash
# 1. plugin.cfg에 외부 플러그인 추가
echo "myplugin:github.com/user/coredns-myplugin" >> plugin.cfg

# 2. 또는 COREDNS_PLUGINS 환경 변수 사용
export COREDNS_PLUGINS="myplugin:github.com/user/coredns-myplugin"

# 3. 재생성 + 빌드
make gen
make
```

## 4. 빌드 옵션

| 변수 | 기본값 | 설명 |
|------|--------|------|
| `CGO_ENABLED` | 0 | CGO 비활성화 (정적 바이너리) |
| `GOTAGS` | `grpcnotrace` | 빌드 태그 |
| `STRIP_FLAGS` | `-s -w` | 바이너리 최적화 (심볼/디버그 제거) |
| `BUILDOPTS` | `-v` | go build 옵션 |
| `COREDNS_PLUGINS` | (없음) | 추가 플러그인 (쉼표 구분) |

### Docker 빌드

**소스코드 경로**: `Dockerfile`

```dockerfile
FROM golang:${GOLANG_VERSION} AS build
# ...
RUN make
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /coredns/coredns /coredns
ENTRYPOINT ["/coredns"]
```

## 5. 플러그인 디렉토리 구조 패턴

각 플러그인은 일관된 디렉토리 구조를 따른다:

```
plugin/<name>/
├── <name>.go           # 핵심 로직 (구조체, ServeDNS)
├── setup.go            # Corefile 파싱, 플러그인 등록
├── handler.go          # Handler 인터페이스 구현 (선택)
├── metrics.go          # Prometheus 메트릭 정의 (선택)
├── README.md           # 플러그인 문서
├── <name>_test.go      # 단위 테스트
└── setup_test.go       # setup 테스트
```

### setup.go 패턴

모든 플러그인의 `setup.go`는 동일한 패턴을 따른다:

```go
func init() {
    plugin.Register("name", setup)
}

func setup(c *caddy.Controller) error {
    // 1. Corefile 디렉티브 파싱
    // 2. 플러그인 인스턴스 생성
    // 3. Config에 플러그인 추가
    dnsserver.GetConfig(c).AddPlugin(func(next plugin.Handler) plugin.Handler {
        return &MyPlugin{Next: next, ...}
    })
    return nil
}
```

## 6. 테스트 구조

### 단위 테스트

각 플러그인 디렉토리 내 `*_test.go` 파일에서 개별 플러그인을 테스트한다.

```
plugin/cache/
├── cache_test.go           # 캐시 로직 테스트
├── handler_test.go         # ServeDNS 테스트
└── setup_test.go           # Corefile 파싱 테스트

plugin/forward/
├── forward_test.go         # 포워딩 로직 테스트
├── policy_test.go          # 정책 선택 테스트
├── proxy_test.go           # 프록시 연결 테스트
└── setup_test.go           # 설정 파싱 테스트
```

### 통합 테스트

**소스코드 경로**: `test/`

통합 테스트는 실제 CoreDNS 서버를 시작하고 DNS 쿼리를 보내 응답을 검증한다.

```
test/
├── server.go               # 테스트 서버 헬퍼
├── cache_test.go            # 캐시 통합 테스트
├── file_test.go             # Zone 파일 통합 테스트
├── proxy_test.go            # 포워딩 통합 테스트
├── kubernetes_test.go       # (etcd 기반) K8s 테스트
├── metrics_test.go          # 메트릭 통합 테스트
├── server_test.go           # 서버 동작 테스트
├── fuzz_corefile.go         # Corefile 퍼징 테스트
└── ...                      # 50+ 통합 테스트
```

### 테스트 실행 방법

```bash
# 전체 단위 테스트
go test ./...

# 특정 플러그인 테스트
go test ./plugin/cache/...
go test ./plugin/forward/...

# 통합 테스트
go test ./test/...

# 코어 엔진 테스트
go test ./core/dnsserver/...

# 레이스 컨디션 검출
go test -race ./...
```

## 7. 코드 생성 파이프라인

```
┌──────────┐     ┌──────────────────────┐     ┌────────────────────────┐
│plugin.cfg│────>│directives_generate.go│────>│core/plugin/zplugin.go  │
│          │     │                      │     │  (플러그인 import)       │
│root:root │     │                      │     │                        │
│cache:cache│    │                      │     ├────────────────────────┤
│forward:  │     │                      │────>│core/dnsserver/         │
│ forward  │     │                      │     │  zdirectives.go        │
│...       │     │                      │     │  (Directives 슬라이스)  │
└──────────┘     └──────────────────────┘     └────────────────────────┘
```

### zdirectives.go 내용 (자동 생성)

```go
// generated by directives_generate.go; DO NOT EDIT
package dnsserver

var Directives = []string{
    "root",
    "metadata",
    "geoip",
    "cancel",
    // ... plugin.cfg 순서대로
    "forward",
    "grpc",
    "whoami",
    "sign",
    "view",
    "nomad",
}
```

## 8. 핵심 패키지 의존성 그래프

```
                    coredns.go (main)
                         │
                         v
                    coremain/run.go
                    ┌────┴────┐
                    v         v
             caddy (외부)   core/dnsserver/
                              │
                    ┌─────────┼─────────┐
                    v         v         v
              plugin/      request/   plugin/pkg/
              plugin.go    request.go  (공유 유틸)
                │
    ┌───────────┼───────────────────┐
    v           v                   v
plugin/cache  plugin/forward  plugin/kubernetes
    │           │                   │
    v           v                   v
plugin/pkg/   plugin/pkg/        k8s.io/
 cache         proxy             client-go
    │           │
    v           v
github.com/miekg/dns  (DNS 프로토콜 핵심)
```
