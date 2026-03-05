# 04. Hubble 코드 구조

## 개요

Hubble 코드는 두 저장소에 분산되어 있다.
**hubble** 저장소는 CLI 바이너리이며, 핵심 서버 로직은 **cilium** 저장소의 `pkg/hubble` 패키지에 위치한다.
API 정의(Protobuf)는 cilium의 `api/v1` 디렉토리에 있다.

---

## 1. Hubble CLI 저장소 구조

```
hubble/                                    # Hubble CLI 저장소
├── main.go                                # 진입점: cmd.Execute() 호출
├── Makefile                               # 빌드 (make hubble)
├── go.mod                                 # Go 모듈 (github.com/cilium/hubble 의존)
├── go.sum
├── README.md
├── CHANGELOG.md
├── CONTRIBUTING.md
├── LICENSE                                # Apache-2.0
├── stable.txt                             # 안정 버전 번호
├── RELEASE.md
├── Documentation/
├── install/                               # 설치 스크립트
├── policies/                              # 보안 정책
├── tutorials/                             # 튜토리얼
└── vendor/                                # 벤더링된 의존성
    └── github.com/cilium/cilium/
        └── hubble/
            ├── cmd/                       # CLI 커맨드
            │   ├── root.go               # 루트 커맨드 (New, Execute)
            │   ├── observe/              # hubble observe
            │   │   └── observe.go
            │   ├── status/               # hubble status
            │   ├── list/                 # hubble list (nodes, namespaces)
            │   ├── watch/                # hubble watch
            │   ├── record/               # hubble record
            │   ├── config/               # hubble config
            │   ├── reflect/              # hubble reflect (gRPC 리플렉션)
            │   ├── version/              # hubble version
            │   └── common/               # 공통 유틸리티
            │       ├── config/           # 플래그/키 정의 (flags.go, viper.go)
            │       ├── conn/             # gRPC 연결 (conn.go, tls.go)
            │       ├── template/         # 사용법 템플릿
            │       └── validate/         # 플래그 검증
            └── pkg/                      # CLI 패키지
                ├── defaults/             # 기본값 (주소, 타임아웃, FlowPrintCount)
                │   └── defaults.go
                ├── printer/              # Flow 출력 포맷터
                ├── logger/               # 로깅
                └── time/                 # 시간 파싱 유틸리티
```

### 진입점 추적

```
main.go (Line 17)
  └── cmd.Execute() -> cmd.New().Execute()
        └── cmd/root.go - NewWithViper(vp)
              ├── cobra.OnInitialize: 설정 파일 로드, 로거 초기화
              ├── PersistentPreRunE: validate.Flags, conn.Init
              └── AddCommand: config, list, observe, record, reflect, status, version, watch
```

---

## 2. Cilium 내 Hubble Server 코드 구조

```
cilium/pkg/hubble/                         # Hubble 핵심 서버 코드
├── api/
│   └── v1/
│       └── types.go                       # v1.Event (Ring Buffer 저장 단위)
├── build/                                 # 버전 정보
├── cell/
│   └── cell.go                            # Hive Cell 정의 (최상위 모듈)
├── container/
│   ├── ring.go                            # Ring Buffer (lock-free, atomic)
│   └── ring_reader.go                     # RingReader (Next, Previous, NextFollow)
├── defaults/                              # 서버 기본값
├── dropeventemitter/                      # Drop 이벤트 Kubernetes 전송
├── exporter/
│   ├── exporter.go                        # FlowLogExporter (파일 출력)
│   ├── cell/                              # Exporter Hive Cell
│   └── testdata/
├── filters/
│   └── filters.go                         # 필터 엔진 (24개 필터, Apply, BuildFilterList)
├── k8s/                                   # Kubernetes 통합
├── math/                                  # 비트 연산 유틸리티 (MSB, GetMask)
├── metrics/
│   ├── api/                               # 메트릭 인터페이스
│   │   └── api.go
│   ├── cell/                              # 메트릭 Hive Cell
│   ├── dns/                               # DNS 메트릭
│   ├── drop/                              # Drop 메트릭
│   ├── flow/                              # Flow 메트릭
│   ├── flows-to-world/                    # External Flow 메트릭
│   ├── http/                              # HTTP 메트릭
│   ├── icmp/                              # ICMP 메트릭
│   ├── policy/                            # Policy 메트릭
│   ├── port-distribution/                 # 포트 분포 메트릭
│   ├── sctp/                              # SCTP 메트릭
│   └── tcp/                               # TCP 메트릭 (flags, SYN/FIN/RST 등)
├── monitor/                               # MonitorAgent 연동
├── observer/
│   ├── local_observer.go                  # LocalObserverServer (핵심 이벤트 루프)
│   ├── namespace/                         # 네임스페이스 추적 관리자
│   ├── observeroption/                    # Observer 옵션/Hook 정의
│   └── types/
│       └── types.go                       # MonitorEvent, PerfEvent, LostEvent
├── parser/
│   ├── parser.go                          # Parser (Decoder 인터페이스, 타입 분기)
│   ├── threefour/                         # L3/L4 Parser (IP, TCP, UDP, ICMP)
│   ├── seven/                             # L7 Parser (HTTP, DNS, Kafka)
│   ├── debug/                             # Debug 이벤트 Parser
│   ├── sock/                              # Socket Trace Parser
│   ├── agent/                             # Agent 이벤트 변환
│   ├── cell/                              # Parser Hive Cell
│   ├── common/                            # 공통 파서 유틸리티
│   ├── errors/                            # 파서 에러 정의
│   ├── fieldaggregate/                    # 필드 집계
│   ├── fieldmask/                         # FieldMask 처리
│   ├── getters/                           # 데이터 조회 인터페이스
│   └── options/                           # 파서 옵션
├── peer/
│   ├── cell/                              # Peer Hive Cell
│   ├── serviceoption/                     # 서비스 옵션
│   └── types/                             # Peer 타입 (ClientBuilder 등)
├── relay/
│   ├── defaults/                          # Relay 기본값
│   ├── observer/
│   │   ├── observer.go                    # Relay Observer (다중노드 집계)
│   │   └── server.go                      # Relay Observer 서버 (GetFlows, ServerStatus)
│   ├── pool/
│   │   └── manager.go                     # PeerManager (연결 풀 관리)
│   ├── queue/
│   │   └── priority_queue.go              # PriorityQueue (타임스탬프 정렬)
│   └── server/
│       └── server.go                      # Relay gRPC Server
├── server/
│   ├── server.go                          # Hubble gRPC Server (로컬)
│   └── serveroption/                      # 서버 옵션
└── testutils/                             # 테스트 유틸리티
```

---

## 3. API 정의 (Protobuf)

```
cilium/api/v1/
├── flow/
│   ├── flow.proto                         # Flow, Endpoint, Layer4, Layer7, Verdict, FlowFilter
│   └── flow.pb.go                         # 자동 생성 Go 코드
├── observer/
│   ├── observer.proto                     # Observer 서비스, GetFlows, ServerStatus
│   ├── observer.pb.go                     # 메시지 Go 코드
│   └── observer_grpc.pb.go               # gRPC 서비스 Go 코드
├── peer/
│   ├── peer.proto                         # Peer 서비스, ChangeNotification
│   ├── peer.pb.go
│   └── peer_grpc.pb.go
├── relay/
│   ├── relay.proto                        # NodeStatusEvent, NodeState
│   └── relay.pb.go
├── Makefile                               # Proto 컴파일 규칙
└── Makefile.protoc                        # protoc 설정
```

---

## 4. 빌드 시스템

### CLI 빌드

```makefile
# 소스: hubble/Makefile (Line 1-39)
GO := go
GO_BUILD = CGO_ENABLED=0 $(GO) build $(GO_BUILD_FLAGS)

TARGET=hubble
VERSION=$(shell go list -f {{.Version}} -m github.com/cilium/cilium)

hubble-bin:
    $(GO_BUILD) $(if $(GO_TAGS),-tags $(GO_TAGS)) \
        -ldflags "-w -s \
            -X 'github.com/cilium/cilium/hubble/pkg.GitBranch=${GIT_BRANCH}' \
            -X 'github.com/cilium/cilium/hubble/pkg.GitHash=$(GIT_HASH)' \
            -X 'github.com/cilium/cilium/hubble/pkg.Version=${VERSION}'" \
        -o $(TARGET) .
```

주요 빌드 특징:
- `CGO_ENABLED=0`: 순수 Go 빌드 (C 의존성 없음)
- `-w -s`: 디버그 심볼 제거 (바이너리 크기 최소화)
- `-ldflags -X`: 빌드 시 버전 정보 주입
- `vendor/` 디렉토리 사용 (의존성 벤더링)

### Makefile 타겟

| 타겟 | 설명 |
|------|------|
| `make hubble` | CLI 바이너리 빌드 |
| `make test` | 테스트 실행 (-race -cover) |
| `make bench` | 벤치마크 실행 |
| `make install` | 바이너리 설치 (`/usr/local/bin`) |
| `make clean` | 빌드 산출물 삭제 |
| `make release` | 크로스 플랫폼 릴리스 빌드 (docker) |
| `make image` | Docker 이미지 빌드 |

### 크로스 플랫폼 릴리스

```
release 타겟은 Docker 컨테이너에서 실행:
- OS: darwin, linux, windows
- Arch: amd64, arm64
- 산출물: hubble-{os}-{arch}.tar.gz + sha256sum
```

---

## 5. 모듈 의존성 구조

```
hubble (CLI 바이너리)
  └── github.com/cilium/cilium (vendor)
        ├── hubble/cmd/          # CLI 커맨드 로직
        ├── hubble/pkg/          # CLI 유틸리티
        ├── pkg/hubble/          # 서버 핵심 로직 (참조만)
        └── api/v1/              # Protobuf 메시지

cilium (Agent 바이너리)
  └── pkg/hubble/
        ├── cell/               # Hive 통합
        ├── observer/           # 이벤트 처리
        ├── parser/             # Flow 파싱
        ├── container/          # Ring Buffer
        ├── filters/            # 필터 엔진
        ├── server/             # gRPC Server
        ├── relay/              # Relay 로직
        ├── peer/               # Peer 서비스
        ├── metrics/            # Prometheus 메트릭
        └── exporter/           # Flow Export
```

### CLI와 Server의 관계

```
+-----------------+        +-----------------+
|   hubble CLI    |        | Cilium Agent    |
|                 |        |                 |
| main.go         |        | pkg/hubble/     |
| cmd/ (커맨드)   |        |   observer/     |
| pkg/ (포맷터)   | gRPC   |   parser/       |
|                 +------->|   container/    |
| Protobuf 메시지 |        |   filters/      |
| (api/v1 참조)   |        |   server/       |
|                 |        |   metrics/      |
+-----------------+        +-----------------+
         ^                          ^
         |                          |
         +---- api/v1/ 공유 --------+
         (flow.proto, observer.proto, peer.proto)
```

---

## 6. 주요 패키지 역할

### CLI 패키지

| 패키지 | 파일 | 역할 |
|--------|------|------|
| `cmd` | `root.go` | 루트 커맨드, Viper 설정, gRPC 연결 초기화 |
| `cmd/observe` | `observe.go` | Flow 관찰 (핵심 커맨드), 필터 플래그, 출력 제어 |
| `cmd/status` | `status.go` | ServerStatus 조회, 포맷 출력 |
| `cmd/list` | | 노드/네임스페이스 목록 조회 |
| `cmd/watch` | | 피어 상태 실시간 감시 |
| `cmd/record` | | Flow 녹화 (파일 저장) |
| `cmd/config` | | 설정 관리 (get/set/reset/view) |
| `cmd/reflect` | | gRPC 리플렉션 서비스 조회 |
| `cmd/common/config` | `flags.go` | 플래그 키 상수, GlobalFlags/ServerFlags 정의 |
| `cmd/common/conn` | `conn.go` | gRPC 연결 관리 (TLS, 인터셉터, 포트 포워딩) |
| `pkg/defaults` | `defaults.go` | 기본값 (ServerAddress, DialTimeout, FlowPrintCount) |
| `pkg/printer` | | Flow 출력 포맷 (JSON, compact, dict, table) |
| `pkg/logger` | | 로깅 (slog) |
| `pkg/time` | | 시간 문자열 파싱 |

### Server 패키지

| 패키지 | 핵심 파일 | 역할 |
|--------|----------|------|
| `cell` | `cell.go` | Hive Cell 모듈 정의, DI 파라미터, 통합 구성 |
| `observer` | `local_observer.go` | 이벤트 루프, Hook 체인, GetFlows/GetAgentEvents |
| `observer/types` | `types.go` | MonitorEvent, PerfEvent, LostEvent 정의 |
| `observer/observeroption` | | Observer 옵션 (MaxFlows, MonitorBuffer, Hooks) |
| `observer/namespace` | | 네임스페이스 추적 관리자 |
| `parser` | `parser.go` | Decoder 인터페이스, 타입별 파서 분기 |
| `parser/threefour` | | L3/L4 파서 (IP/TCP/UDP/ICMP 헤더 디코딩) |
| `parser/seven` | | L7 파서 (HTTP, DNS, Kafka) |
| `parser/debug` | | Debug 이벤트 파서 |
| `parser/sock` | | Socket Trace 파서 |
| `parser/getters` | | 데이터 조회 인터페이스 (Endpoint, Identity, DNS, IP) |
| `container` | `ring.go` | Lock-free Ring Buffer (atomic 연산) |
| `container` | `ring_reader.go` | RingReader (Next, Previous, NextFollow) |
| `filters` | `filters.go` | 24개 필터 빌더, Apply (whitelist/blacklist) |
| `server` | `server.go` | Hubble gRPC Server (Observer, Peer, Health 등록) |
| `relay/observer` | `server.go` | Relay Observer (다중노드 GetFlows, ServerStatus) |
| `relay/observer` | `observer.go` | Flow 수집/정렬/에러 집계 로직 |
| `relay/pool` | `manager.go` | PeerManager (연결 풀, backoff, 알림 감시) |
| `relay/queue` | `priority_queue.go` | min-heap PriorityQueue (타임스탬프 정렬) |
| `relay/server` | `server.go` | Relay gRPC Server (메트릭, 헬스, 시작/종료) |
| `metrics` | 하위 디렉토리들 | DNS/HTTP/TCP/Drop/Flow/Policy 메트릭 핸들러 |
| `exporter` | `exporter.go` | FlowLogExporter (필터/인코딩/파일 출력) |
| `peer` | | Peer gRPC 서비스 |
| `api/v1` | `types.go` | v1.Event (Ring Buffer 저장 단위) |

---

## 7. 의존성 구조

### CLI 핵심 의존성

| 패키지 | 용도 |
|--------|------|
| `github.com/spf13/cobra` | CLI 프레임워크 |
| `github.com/spf13/viper` | 설정 관리 (파일, 환경변수, 플래그) |
| `google.golang.org/grpc` | gRPC 클라이언트 |
| `google.golang.org/protobuf` | Protobuf 런타임 |
| `k8s.io/client-go` | Kubernetes 클라이언트 (포트 포워딩) |

### Server 핵심 의존성

| 패키지 | 용도 |
|--------|------|
| `google.golang.org/grpc` | gRPC 서버 |
| `google.golang.org/protobuf` | Protobuf 런타임 |
| `github.com/cilium/hive` | 의존성 주입 프레임워크 |
| `github.com/prometheus/client_golang` | Prometheus 메트릭 |
| `golang.org/x/sync/errgroup` | 동시성 에러 그룹 |
| `github.com/google/uuid` | UUID 생성 |
| `github.com/grpc-ecosystem/go-grpc-prometheus` | gRPC 메트릭 |

---

## 8. 코드 규모

### CLI (vendor 포함)

| 항목 | 대략 규모 |
|------|----------|
| cmd/ 디렉토리 | 약 10개 서브커맨드 |
| pkg/ 디렉토리 | 4개 패키지 (defaults, printer, logger, time) |
| main.go | 22행 |

### Server

| 항목 | 대략 규모 |
|------|----------|
| pkg/hubble/ 전체 | 약 50개 패키지 |
| observer/ | LocalObserverServer 830행 |
| container/ | Ring 400행, RingReader 132행 |
| parser/ | 약 10개 파서 패키지 |
| filters/ | 24개 필터 타입 |
| relay/ | observer, pool, queue, server 4개 서브패키지 |
| metrics/ | 10+ 메트릭 타입 (dns, http, tcp, drop 등) |

### API

| 항목 | 대략 규모 |
|------|----------|
| flow.proto | 1031행 (40+ 메시지/enum 타입) |
| observer.proto | 303행 (6 RPC, 20+ 메시지) |
| peer.proto | 59행 (1 RPC, 4 메시지) |
| relay.proto | 40행 (2 메시지) |
