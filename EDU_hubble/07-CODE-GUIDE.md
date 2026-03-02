# 07. 코드 레벨 가이드 (Code-Level Guide)

## 진입점

### main.go

```
hubble/main.go
  └─ cmd.Execute()
       └─ rootCmd.Execute() (Cobra)
```

`main.go`는 단 하나의 일만 합니다: `cmd.Execute()` 호출.
이 설계는 의도적입니다 — CLI 바이너리의 진입점은 최소한으로 유지하고, 모든 로직은 `cmd` 패키지에 위임합니다.

---

## 핵심 패키지 구조

### CLI 계층 (`vendor/github.com/cilium/cilium/hubble/`)

```
hubble/
├── cmd/                        # CLI 커맨드 (Cobra 기반)
│   ├── root.go                 # 루트 커맨드 + 설정 초기화
│   ├── observe/                # observe 서브커맨드
│   │   ├── observe.go          # 커맨드 등록 + 공통 로직
│   │   ├── flows.go            # GetFlows RPC 호출
│   │   ├── flows_filter.go     # 필터 플래그 → FlowFilter 변환
│   │   ├── agent_events.go     # GetAgentEvents RPC
│   │   ├── debug_events.go     # GetDebugEvents RPC
│   │   ├── io_reader_observer.go  # 파일에서 플로우 읽기 (--input-file)
│   │   ├── identity.go         # Identity 파싱 유틸
│   │   └── workload.go         # 워크로드 유틸
│   ├── list/                   # list 서브커맨드
│   │   ├── list.go             # 커맨드 등록
│   │   ├── node.go             # GetNodes RPC
│   │   └── namespaces.go       # GetNamespaces RPC
│   ├── status/                 # status 커맨드
│   │   └── status.go           # ServerStatus RPC + Health Check
│   ├── config/                 # config 커맨드
│   │   ├── config.go           # 커맨드 등록
│   │   ├── get.go / set.go / reset.go / view.go
│   ├── record/                 # record 커맨드 (실험적)
│   ├── watch/                  # watch 커맨드 (개발용)
│   │   ├── watch.go
│   │   └── peer.go
│   ├── version/                # version 커맨드
│   ├── reflect/                # gRPC reflection (숨김)
│   └── common/                 # 공통 유틸리티
│       ├── config/             # 설정 관리
│       │   ├── flags.go        # 플래그 정의 (서버, TLS 등)
│       │   ├── viper.go        # Viper 초기화 + 환경변수 바인딩
│       │   └── compat.go       # HUBBLE_COMPAT 호환성 옵션
│       ├── conn/               # gRPC 연결
│       │   └── conn.go         # TLS, 인증, port-forward, 헬스체크
│       ├── template/           # 도움말 텍스트 템플릿
│       └── validate/           # 설정 검증
└── pkg/                        # CLI 유틸리티
    ├── printer/                # 출력 포맷팅
    │   ├── printer.go          # 메인 Printer 구현
    │   ├── formatter.go        # 숫자/시간 포맷터
    │   ├── color.go            # 터미널 색상
    │   ├── terminal.go         # ANSI 이스케이프
    │   └── options.go          # 프린터 옵션
    ├── logger/                 # 로깅 (slog 래퍼)
    ├── time/                   # 시간 파싱 유틸리티
    ├── defaults/               # 기본값 상수
    └── version.go              # 버전 정보 (빌드 시 주입)
```

### 서버 계층 (`vendor/github.com/cilium/cilium/pkg/hubble/`)

```
pkg/hubble/
├── api/v1/                     # 내부 API 타입
│   └── types.go                # Event 래퍼 타입
├── observer/                   # 핵심: 플로우 옵저버
│   ├── local_observer.go       # LocalObserverServer (메인 구현)
│   ├── types/
│   │   └── types.go            # MonitorEvent, PerfEvent, LostEvent
│   └── observeroption/
│       └── option.go           # Hook 시스템 정의 (7개 확장 포인트)
├── parser/                     # BPF 이벤트 → Flow 변환
│   ├── parser.go               # 메인 Decoder 인터페이스 + 구현
│   ├── threefour/              # L3/L4 파서
│   ├── seven/                  # L7 파서 (DNS, HTTP)
│   ├── debug/                  # 디버그 이벤트 파서
│   ├── sock/                   # 소켓 트레이스 파서
│   └── getters/
│       └── getters.go          # K8s 메타데이터 접근 인터페이스
├── filters/                    # 플로우 필터링 시스템 (50+ 필터)
│   └── filters.go              # FilterFunc, OnBuildFilter 인터페이스
├── container/                  # 데이터 저장소
│   └── ring.go                 # 순환 버퍼 (power-of-2 용량)
├── metrics/                    # Prometheus 메트릭
│   ├── metrics.go              # 레지스트리 + 초기화
│   ├── api/                    # 메트릭 핸들러 인터페이스
│   ├── dns/                    # DNS 메트릭
│   ├── http/                   # HTTP 메트릭
│   ├── tcp/                    # TCP 메트릭
│   ├── drop/                   # Drop 메트릭
│   ├── flow/                   # Flow 카운트 메트릭
│   ├── policy/                 # 정책 메트릭
│   ├── icmp/                   # ICMP 메트릭
│   ├── sctp/                   # SCTP 메트릭
│   ├── port-distribution/      # 포트 분포 메트릭
│   └── flows-to-world/         # 외부 트래픽 메트릭
├── peer/                       # 피어 관리
│   └── service.go              # Peer gRPC 서비스 구현
├── server/                     # gRPC 서버
│   └── server.go               # 서버 초기화 + 시작
├── exporter/                   # Flow 내보내기
├── recorder/                   # 패킷 캡처
└── relay/                      # 멀티 노드 Relay
```

---

## 핵심 디자인 패턴

### 1. Hook/Plugin 패턴 (Observer)

Observer의 모든 처리 단계에 Hook을 삽입할 수 있는 구조입니다.

```
MonitorEvent 수신
    ↓
[OnMonitorEvent hooks] ← 전처리 (유효성 검증, 텔레메트리)
    ↓
Decode (Parser)
    ↓
[OnDecodedFlow hooks] ← 후처리 (enrichment, 커스텀 메트릭)
    ↓
Filter 적용
    ↓
[OnFlowDelivery hooks] ← 전달 전 (최종 변환, 감사 로깅)
    ↓
클라이언트 전송
```

**왜 이 패턴인가?**
- Cilium 내부에서 메트릭 수집, Flow 내보내기, 속도 제한 등을 Hook으로 구현
- 새 기능 추가 시 Observer 코어 코드 수정 없이 Hook만 추가하면 됨
- 각 Hook은 `(stop bool, error)` 반환 — 체인 중단 가능

**Hook 인터페이스:**

```go
// 모든 Hook의 공통 패턴
type OnDecodedFlow interface {
    OnDecodedFlow(ctx context.Context, flow *pb.Flow) (stop bool, err error)
}

// 함수 타입으로도 사용 가능 (편의성)
type OnDecodedFlowFunc func(ctx context.Context, flow *pb.Flow) (stop bool, err error)
```

**Options 구조체의 Hook 슬라이스:**

```go
type Options struct {
    OnServerInit    []OnServerInit       // 서버 초기화 후
    OnMonitorEvent  []OnMonitorEvent     // 이벤트 디코딩 전
    OnDecodedFlow   []OnDecodedFlow      // 플로우 디코딩 후
    OnDecodedEvent  []OnDecodedEvent     // 이벤트 디코딩 후
    OnBuildFilter   []OnBuildFilter      // 필터 구성 시
    OnFlowDelivery  []OnFlowDelivery     // API 전달 전
    OnGetFlows      []OnGetFlows         // GetFlows RPC 호출 시
}
```

### 2. Getter 인터페이스 패턴 (Parser)

Parser가 K8s 메타데이터에 접근할 때 직접 API를 호출하지 않고 인터페이스를 통해 접근합니다.

```go
// 각 메타데이터 소스에 대한 독립된 인터페이스
type DNSGetter interface {
    GetNamesOf(sourceEpID uint32, ip netip.Addr) []string
}

type EndpointGetter interface {
    GetEndpointInfo(ip netip.Addr) (endpoint EndpointInfo, ok bool)
    GetEndpointInfoByID(id uint16) (endpoint EndpointInfo, ok bool)
}

type IdentityGetter interface {
    GetIdentity(id uint32) (*identity.Identity, error)
}

type IPGetter interface {
    GetK8sMetadata(ip netip.Addr) *ipcache.K8sMetadata
    LookupSecIDByIP(ip netip.Addr) (ipcache.Identity, bool)
}

type ServiceGetter interface {
    GetServiceByAddr(ip netip.Addr, port uint16) *flowpb.Service
}

type LinkGetter interface {
    GetIfNameCached(ifIndex int) (string, bool)
    Name(ifIndex uint32) string
}
```

**왜 이 패턴인가?**
- **테스트 용이성**: 각 Getter를 Mock으로 교체 가능
- **관심사 분리**: Parser는 "BPF 바이트를 Flow로 변환"에만 집중, 메타데이터 조회는 Getter 담당
- **느슨한 결합**: K8s API, Cilium 내부 캐시 등 구현이 바뀌어도 Parser 코드 불변

### 3. Ring Buffer 패턴 (Container)

```go
type Ring struct {
    // power-of-2 용량의 순환 버퍼
    // 새 이벤트가 오래된 이벤트를 덮어씀 (FIFO)
}
```

**왜 Ring Buffer인가?**
- **고정 메모리**: 버퍼 크기가 미리 정해져 있어 메모리 사용량 예측 가능
- **GC 프리**: 사전 할당된 슬롯을 재사용하므로 GC 압력 없음
- **O(1) 연산**: 읽기/쓰기 모두 상수 시간
- **power-of-2 용량**: 비트 마스킹으로 모듈로 연산 대체 (성능 최적화)

### 4. Filter Chain 패턴

```go
type FilterFunc func(ev *v1.Event) bool
type FilterFuncs []FilterFunc

// 모든 필터가 true를 반환해야 매치 (AND 조건)
func (fs FilterFuncs) MatchAll(ev *v1.Event) bool {
    for _, f := range fs {
        if !f(ev) {
            return false
        }
    }
    return true
}
```

**필터 적용 로직:**

```
whitelist (OR):  filter1 OR filter2 OR filter3
                 → 하나라도 매치하면 포함

blacklist (OR):  filter1 OR filter2
                 → 하나라도 매치하면 제외

각 FlowFilter 내부 (AND):
  source_pod=X AND verdict=DROPPED AND protocol=tcp
  → 모든 조건이 매치해야 함
```

### 5. Cobra/Viper 설정 통합 패턴

```go
// 플래그 정의 (flags.go)
cmd.Flags().String("server", defaults.ServerAddress, "Hubble server address")

// 환경 변수 바인딩 (viper.go)
viper.SetEnvPrefix("HUBBLE")
viper.AutomaticEnv()
viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))

// 설정 파일 탐색
viper.SetConfigName("config")
viper.AddConfigPath(".")
viper.AddConfigPath("$XDG_CONFIG_HOME/hubble")
viper.AddConfigPath("$HOME/.hubble")
```

**왜 이 조합인가?**
- Cobra: 서브커맨드, 플래그 파싱, 도움말 자동 생성
- Viper: 환경 변수, 설정 파일, 플래그를 통합 우선순위로 관리
- `HUBBLE_` 접두어로 다른 환경 변수와 충돌 방지

---

## 프로토콜 파싱 흐름

### L3/L4 파서 (`threefour.Parser`)

```
Raw BPF 바이트
  ↓
이더넷 헤더 파싱 (14 bytes)
  → Source MAC, Destination MAC
  ↓
EtherType 확인
  ├─ 0x0800: IPv4 헤더 파싱 (20+ bytes)
  │    → Source IP, Dest IP, Protocol, TTL
  ├─ 0x86DD: IPv6 헤더 파싱 (40 bytes)
  │    → Source IP, Dest IP, Next Header
  └─ 기타: 스킵
  ↓
L4 프로토콜 파싱
  ├─ TCP (Protocol 6):  포트, 시퀀스, 플래그 (SYN/ACK/FIN/RST)
  ├─ UDP (Protocol 17): 포트
  ├─ ICMP (Protocol 1): Type, Code
  └─ SCTP (Protocol 132): 포트
  ↓
K8s 메타데이터 Enrichment
  ├─ IP → Pod 이름 (EndpointGetter)
  ├─ IP → Service 이름 (ServiceGetter)
  ├─ IP → DNS 이름 (DNSGetter)
  ├─ SecurityID → Identity (IdentityGetter)
  └─ ifIndex → 인터페이스 이름 (LinkGetter)
  ↓
Flow Protobuf 메시지 생성
```

### L7 파서 (`seven.Parser`)

```
L7 이벤트 데이터
  ↓
프로토콜 타입 판별
  ├─ DNS:
  │    → 쿼리 이름, 타입 (A/AAAA/CNAME)
  │    → 응답 코드 (NOERROR/NXDOMAIN/SERVFAIL)
  │    → 응답 IP 목록
  │
  ├─ HTTP:
  │    → 메서드 (GET/POST/PUT/DELETE)
  │    → URL 경로
  │    → 상태 코드 (200/404/500)
  │    → 헤더 (선택적)
  │    → 지연 시간 (나노초)
  │
  └─ Kafka (deprecated):
       → API Key, API Version
       → Topic
       → Correlation ID
  ↓
Flow.L7 필드 설정
```

---

## 메트릭 시스템 구조

### 메트릭 플러그인 아키텍처

```
metrics/
├── api/                  # NamedHandler 인터페이스
├── dns/handler.go        # DNS 메트릭 핸들러
├── http/handler.go       # HTTP 메트릭 핸들러
├── tcp/handler.go        # TCP 메트릭 핸들러
├── drop/handler.go       # Drop 메트릭 핸들러
├── flow/handler.go       # Flow 카운트 핸들러
├── policy/handler.go     # 정책 판정 핸들러
└── ...
```

각 메트릭 핸들러는 `OnDecodedFlow` Hook으로 등록되어 Flow 디코딩 후 자동으로 메트릭을 업데이트합니다.

```go
// 메트릭 핸들러의 공통 패턴
type Handler struct {
    counter   *prometheus.CounterVec
    histogram *prometheus.HistogramVec
}

func (h *Handler) OnDecodedFlow(ctx context.Context, flow *pb.Flow) (bool, error) {
    // Flow에서 필요한 필드 추출
    // Prometheus 메트릭 업데이트
    h.counter.WithLabelValues(...).Inc()
    return false, nil  // 체인 계속 진행
}
```

### 주요 메트릭

| 메트릭 이름 | 타입 | 레이블 | 설명 |
|-------------|------|--------|------|
| `hubble_flows_processed_total` | Counter | source, destination, verdict | 처리된 플로우 총 수 |
| `hubble_drop_total` | Counter | reason, protocol | 드랍된 패킷 수 |
| `hubble_dns_queries_total` | Counter | query, rcode | DNS 쿼리 수 |
| `hubble_http_requests_total` | Counter | method, status | HTTP 요청 수 |
| `hubble_http_request_duration_seconds` | Histogram | method | HTTP 지연 시간 분포 |
| `hubble_tcp_flags_total` | Counter | flag, direction | TCP 플래그별 카운트 |
| `hubble_lost_events_total` | Counter | source | 유실된 이벤트 수 |
| `hubble_policy_verdicts_total` | Counter | verdict, direction | 정책 판정 수 |

---

## 코드 컨벤션

### 라이선스 헤더

모든 소스 파일에 SPDX 라이선스 헤더가 포함되어야 합니다:

```go
// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium
```

### 에러 처리

```go
// 에러를 상위로 전파할 때 컨텍스트 추가
if err != nil {
    return fmt.Errorf("failed to connect to hubble server: %w", err)
}
```

### 테스트

- 테스트 파일은 `_test.go` 접미어
- 테이블 기반 테스트 패턴 사용 (Go 표준)
- Mock은 Getter 인터페이스를 구현하여 작성

---

## 코드 탐색 팁

### "이 필터는 어디서 구현되었나?"

```bash
# 필터 구현 찾기
grep -r "source-pod" vendor/github.com/cilium/cilium/hubble/cmd/observe/
# → flows_filter.go 에서 플래그 정의 및 FlowFilter 변환

# 서버 사이드 필터 적용
grep -r "FilterFunc" vendor/github.com/cilium/cilium/pkg/hubble/filters/
# → filters.go 에서 FilterFunc 타입 및 매칭 로직
```

### "새 CLI 커맨드를 추가하려면?"

1. `hubble/cmd/` 아래에 새 디렉토리 생성
2. Cobra Command 정의
3. `root.go`의 `rootCmd.AddCommand()`에 등록
4. 필요한 경우 `common/config/flags.go`에 플래그 추가

### "새 메트릭을 추가하려면?"

1. `pkg/hubble/metrics/` 아래에 새 디렉토리 생성
2. `NamedHandler` 인터페이스 구현
3. `OnDecodedFlow` Hook에서 메트릭 업데이트
4. `metrics.go`의 레지스트리에 등록

### "새 L7 프로토콜 파서를 추가하려면?"

1. `pkg/hubble/parser/seven/` 에 파서 추가
2. `flow.proto`에 새 프로토콜 메시지 정의
3. `seven.Parser.Decode()`에서 분기 추가
4. 해당 프로토콜의 필터 구현

---

## 직접 실행해보기 (PoC)

| PoC | 실행 | 학습 내용 |
|-----|------|----------|
| [poc-ring-buffer](poc-ring-buffer/) | `cd poc-ring-buffer && go run main.go` | 순환 버퍼, power-of-2 비트 마스킹, FIFO 덮어쓰기 |
| [poc-hook-system](poc-hook-system/) | `cd poc-hook-system && go run main.go` | Hook 체인 패턴, stop 반환값으로 체인 중단 |
| [poc-getter-interface](poc-getter-interface/) | `cd poc-getter-interface && go run main.go` | Getter 인터페이스, Mock 테스트, 의존성 역전 |
| [poc-filter-chain](poc-filter-chain/) | `cd poc-filter-chain && go run main.go` | FilterFunc, Whitelist/Blacklist, AND/OR 조합 |
