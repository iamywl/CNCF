# Alertmanager 코드 구조

## 1. 디렉토리 구조

```
alertmanager/
├── cmd/                          # 진입점 (바이너리)
│   ├── alertmanager/             # 메인 서버
│   │   └── main.go              # main() → run() (682줄)
│   └── amtool/                   # CLI 관리 도구
│       └── main.go              # cli.Execute() 호출 (21줄)
│
├── api/                          # HTTP API
│   ├── api.go                   # API 통합, 동시성 제한
│   ├── v1_deprecation_router.go # v1 API deprecated 안내
│   ├── metrics/                 # Prometheus 메트릭 정의
│   └── v2/                      # REST API v2
│       ├── api.go              # go-swagger 핸들러 구현
│       ├── compat.go           # 호환성 레이어
│       ├── testing.go          # 테스트 유틸
│       ├── openapi.yaml        # OpenAPI 명세
│       ├── models/             # 자동 생성 모델 (24개)
│       ├── restapi/            # 자동 생성 서버 코드
│       │   └── operations/     # alert, silence, receiver 등
│       └── client/             # 자동 생성 클라이언트
│
├── alert/                        # Alert 핵심 모델
│   └── alert.go                 # Alert, AlertSlice 타입
│
├── cli/                          # amtool CLI 구현
│   ├── root.go                  # 루트 명령어
│   ├── alert*.go                # alert 서브커맨드
│   ├── silence*.go              # silence 서브커맨드
│   ├── config.go                # config 서브커맨드
│   ├── template*.go             # template 서브커맨드
│   └── format/                  # 출력 포맷터 (JSON, extended)
│
├── cluster/                      # HA 클러스터링
│   ├── cluster.go               # Peer 구현 (memberlist)
│   ├── channel.go               # ClusterChannel
│   ├── delegate.go              # memberlist Delegate
│   ├── tls_transport.go         # TLS 기반 전송
│   └── tls_connection_pool.go   # TLS 연결 풀
│
├── config/                       # 설정 관리
│   ├── config.go                # Config, Route, Receiver 등 (1149줄)
│   ├── coordinator.go           # 설정 리로드 조정자
│   ├── notifiers.go             # Notifier별 Config 구조체
│   └── receiver/                # Receiver 빌더
│
├── dispatch/                     # Alert 라우팅/디스패치
│   ├── dispatch.go              # Dispatcher, AggregationGroup
│   └── route.go                 # Route 트리, 매칭 로직
│
├── inhibit/                      # Inhibition (억제)
│   └── inhibit.go               # Inhibitor, InhibitRule
│
├── internal/                     # 내부 도구
│   └── tools/                   # 빌드 도구 의존성
│
├── limit/                        # Rate Limiting
│   └── bucket.go                # 용량 제한 Bucket (힙 기반)
│
├── matcher/                      # 레이블 매칭
│   ├── parse/                   # UTF-8 매처 파서
│   │   ├── parse.go            # Matchers(), Matcher()
│   │   ├── lexer.go            # 토큰 스캐너
│   │   └── token.go            # 토큰 정의
│   └── compat/                  # 호환성 레이어
│       └── parse.go            # Classic/UTF-8 파서 선택
│
├── nflog/                        # Notification Log
│   ├── nflog.go                 # Log, Store, Query
│   └── nflogpb/                 # Protobuf 정의
│       └── nflog.proto          # MeshEntry, Entry, Receiver
│
├── notify/                       # 알림 파이프라인
│   ├── notify.go                # Stage, Integration, Notifier 인터페이스
│   ├── mute.go                  # MuteStage (Silence/Inhibition)
│   └── impl/                    # 각 Receiver 구현
│       ├── slack/               # Slack
│       ├── email/               # Email
│       ├── pagerduty/           # PagerDuty
│       ├── webhook/             # Webhook
│       ├── opsgenie/            # OpsGenie
│       ├── sns/                 # AWS SNS
│       ├── telegram/            # Telegram
│       ├── discord/             # Discord
│       ├── msteams/             # Microsoft Teams
│       ├── jira/                # Jira
│       └── ...                  # 기타 Receiver
│
├── pkg/                          # 공개 라이브러리
│   ├── labels/                  # Matcher 타입 (공용)
│   │   └── matcher.go          # Matcher, Matchers, MatcherSet
│   └── modtimevfs/              # 파일 시스템 유틸
│
├── provider/                     # Alert 저장소 추상화
│   ├── provider.go              # Alerts 인터페이스
│   └── mem/                     # 메모리 기반 구현
│       └── mem.go               # Alerts (인메모리)
│
├── silence/                      # Silence 관리
│   ├── silence.go               # Silences, Silencer
│   └── silencepb/               # Protobuf 정의
│
├── store/                        # 내부 Alert 저장소
│   └── store.go                 # map[Fingerprint]*Alert
│
├── template/                     # 템플릿 엔진
│   ├── template.go              # Template, Data, Alert, KV
│   └── default_tmpl.go          # 기본 템플릿
│
├── timeinterval/                 # 시간 간격
│   └── timeinterval.go          # Intervener, TimeInterval
│
├── tracing/                      # 분산 추적
│   ├── tracing.go               # Manager (OpenTelemetry)
│   └── config.go                # TracingConfig
│
├── types/                        # 공통 타입
│   └── types.go                 # AlertMarker, GroupMarker, AlertStatus
│
├── featurecontrol/               # 기능 플래그
│   └── featurecontrol.go        # Flagger 인터페이스, Flags 구현
│
├── ui/                           # 웹 UI (React)
│   └── app/                     # 프론트엔드 소스
│
├── doc/                          # 문서
│   ├── arch.svg                 # 아키텍처 다이어그램
│   └── examples/                # 설정 예시
│
├── examples/                     # 설정 예시 (HA 등)
├── scripts/                      # 빌드/릴리스 스크립트
├── test/                         # 통합 테스트
│
├── go.mod                        # Go 모듈 정의
├── go.sum                        # 의존성 체크섬
├── Makefile                      # 빌드 시스템
├── Makefile.common               # 공통 빌드 규칙
├── Dockerfile                    # Docker 빌드
├── Procfile                      # goreman HA 테스트
└── buf.gen.yaml / buf.yaml       # Protobuf 빌드 설정
```

## 2. 패키지 의존성 그래프

```
cmd/alertmanager
    │
    ├── config          ← YAML 설정 파싱
    ├── api             ← HTTP API 서버
    │   └── api/v2      ← OpenAPI v2 핸들러
    ├── dispatch        ← Alert 라우팅
    ├── notify          ← 알림 파이프라인
    │   └── notify/impl ← Slack, Email 등 구현
    ├── silence         ← Silence 관리
    ├── inhibit         ← Inhibition 규칙
    ├── nflog           ← Notification Log
    ├── cluster         ← HA 클러스터링
    ├── provider/mem    ← Alert 저장소
    ├── store           ← 내부 저장소
    ├── types           ← 공통 타입
    ├── alert           ← Alert 모델
    ├── template        ← 템플릿 엔진
    ├── timeinterval    ← 시간 간격
    ├── featurecontrol  ← 기능 플래그
    └── tracing         ← 분산 추적
```

## 3. 빌드 시스템

### 3.1 Go 모듈

```
module github.com/prometheus/alertmanager
go 1.25.0
```

주요 외부 의존성:

| 의존성 | 용도 |
|--------|------|
| `hashicorp/memberlist` | Gossip 프로토콜 (클러스터링) |
| `go-openapi/*` | OpenAPI/Swagger 코드 생성 |
| `alecthomas/kingpin/v2` | CLI 플래그 파싱 |
| `prometheus/client_golang` | Prometheus 메트릭 |
| `prometheus/common` | 공통 모델 (Alert, LabelSet 등) |
| `prometheus/exporter-toolkit` | HTTP 서버 유틸 |
| `cenkalti/backoff/v4` | Exponential Backoff |
| `coder/quartz` | 테스트 가능한 Clock |
| `oklog/run` | goroutine 그룹 관리 |
| `oklog/ulid/v2` | ULID 생성 (Silence ID) |
| `go.opentelemetry.io/*` | 분산 추적 (OTLP) |
| `aws/aws-sdk-go-v2` | AWS SNS 연동 |
| `telebot.v3` | Telegram Bot 연동 |

### 3.2 Makefile

```makefile
# 주요 타겟
make build          # 바이너리 빌드
make test           # 유닛 테스트
make assets          # UI 에셋 빌드
make proto           # Protobuf 컴파일
make apigen          # OpenAPI 코드 생성
make docker          # Docker 이미지 빌드
```

### 3.3 Protobuf

`buf.gen.yaml`과 `buf.yaml`로 Protobuf 빌드를 관리한다:
- `nflog/nflogpb/nflog.proto` — Notification Log 메시지
- `silence/silencepb/silence.proto` — Silence 메시지

### 3.4 Docker

```dockerfile
# 멀티스테이지 빌드
FROM golang:... AS builder
RUN make build

FROM quay.io/prometheus/busybox-linux-amd64:latest
COPY alertmanager /bin/alertmanager
EXPOSE 9093
ENTRYPOINT ["/bin/alertmanager"]
```

## 4. 테스트 구조

```
alertmanager/
├── *_test.go                     # 각 패키지 내 유닛 테스트
│   ├── config/config_test.go
│   ├── dispatch/dispatch_test.go
│   ├── dispatch/route_test.go
│   ├── notify/notify_test.go
│   ├── silence/silence_test.go
│   ├── inhibit/inhibit_test.go
│   ├── nflog/nflog_test.go
│   ├── store/store_test.go
│   ├── provider/mem/mem_test.go
│   ├── template/template_test.go
│   ├── matcher/parse/parse_test.go
│   └── ...
│
├── test/                         # 통합 테스트
│   └── with_api_v2/
│       └── acceptance*.go       # E2E 수용 테스트
│
└── api/v2/
    └── api_test.go              # API 핸들러 테스트
```

## 5. 핵심 파일별 역할 요약

| 파일 | 줄 수(approx) | 핵심 역할 |
|------|--------------|----------|
| `cmd/alertmanager/main.go` | ~682 | 서버 초기화, goroutine 관리 |
| `config/config.go` | ~1149 | YAML 설정 모델, 파싱, 유효성 검증 |
| `dispatch/dispatch.go` | ~600 | Dispatcher, AggregationGroup |
| `dispatch/route.go` | ~300 | Route 트리, DFS 매칭 |
| `notify/notify.go` | ~700 | Stage 인터페이스, Pipeline 구축 |
| `silence/silence.go` | ~900 | Silences 저장소, Silencer |
| `inhibit/inhibit.go` | ~250 | Inhibitor, InhibitRule |
| `nflog/nflog.go` | ~400 | Notification Log, GC, 스냅샷 |
| `cluster/cluster.go` | ~500 | Gossip Peer, 상태 동기화 |
| `provider/mem/mem.go` | ~350 | 메모리 Alert 저장소 |
| `store/store.go` | ~200 | Alert map 저장소 |
| `types/types.go` | ~200 | AlertMarker, GroupMarker |
| `template/template.go` | ~400 | 템플릿 데이터, 함수 |
| `api/api.go` | ~150 | API 통합 |
| `api/v2/api.go` | ~800 | API v2 핸들러 |

## 6. 코드 생성

Alertmanager는 두 가지 코드 생성 메커니즘을 사용한다:

### 6.1 OpenAPI / go-swagger

`api/v2/openapi.yaml`에서 다음을 생성한다:
- `api/v2/models/` — 요청/응답 모델
- `api/v2/restapi/` — 서버 코드
- `api/v2/client/` — 클라이언트 코드

### 6.2 Protocol Buffers

`buf.gen.yaml`으로 다음을 생성한다:
- `nflog/nflogpb/*.pb.go` — nflog 메시지
- `silence/silencepb/*.pb.go` — Silence 메시지

이 생성된 코드는 클러스터 간 상태 동기화에 사용된다.

## 7. 진입점 분석 (cmd/alertmanager/main.go)

### 7.1 main() → run() 구조

```
main():
    1. kingpin 플래그 파싱 (CLI 옵션)
    2. run() 호출

run() 흐름 (약 682줄):
    1. 설정 파일 초기 로드 (config.LoadFile)
    2. Cluster Peer 생성 (cluster.Create)
    3. Alert Provider 생성 (mem.NewAlerts)
    4. Notification Log 생성 (nflog.New)
    5. Silences 저장소 생성 (silence.New)
    6. Cluster에 상태 등록:
       - peer.AddState("nfl", nflog)
       - peer.AddState("sil", silences)
    7. Marker 생성 (types.NewMarker)
    8. Inhibitor 생성
    9. Silencer 생성
    10. Dispatcher 생성
    11. API 서버 생성
    12. Coordinator 설정 (구독자 등록)
    13. oklog/run.Group으로 goroutine 관리:
        - HTTP 서버
        - Dispatcher
        - Inhibitor
        - nflog Maintenance
        - Silences Maintenance
        - Cluster Peer
```

### 7.2 oklog/run.Group 패턴

```go
// 각 컴포넌트를 run.Group에 등록
var g run.Group

g.Add(func() error {
    return httpServer.ListenAndServe()
}, func(err error) {
    httpServer.Shutdown(ctx)
})

g.Add(func() error {
    disp.Run()  // Dispatcher 메인 루프
    return nil
}, func(err error) {
    disp.Stop()
})

// 하나라도 종료되면 모든 컴포넌트 종료
if err := g.Run(); err != nil {
    logger.Error("error running alertmanager", "err", err)
}
```

**왜 oklog/run.Group인가?**

`run.Group`은 여러 goroutine 중 하나가 종료(에러 또는 정상)되면 나머지 모두를 interrupt 함수로 종료시킨다. HTTP 서버, Dispatcher, Inhibitor 등이 모두 연결되어 있어, 하나의 장애가 전체 시스템을 중단시키고 깔끔하게 재시작할 수 있게 한다.

## 8. CLI 구조 (amtool)

### 8.1 명령어 트리

```
amtool
├── alert
│   ├── query      — Alert 조회
│   └── add        — Alert 생성 (테스트용)
├── silence
│   ├── add        — Silence 생성
│   ├── expire     — Silence 만료
│   ├── import     — Silence 일괄 임포트
│   ├── query      — Silence 조회
│   └── update     — Silence 업데이트
├── config
│   ├── show       — 현재 설정 표시
│   └── routes     — Route 트리 시각화
│       ├── show   — 트리 표시
│       └── test   — Alert이 어느 Route로 매칭되는지 테스트
├── check-config   — 설정 파일 유효성 검증
├── cluster
│   └── show       — 클러스터 상태 표시
└── template
    └── render     — 템플릿 렌더링 테스트
```

### 8.2 amtool 구현 패턴

```
cli/root.go:
    kingpin.Application 초기화
    각 서브커맨드 등록
    Execute() → 선택된 명령어 실행

cli/alert_query.go:
    API v2 클라이언트 사용
    GET /api/v2/alerts 호출
    format/ 패키지로 출력 (JSON, extended, simple)
```

## 9. 내부 저장소 계층

```
┌───────────────────────────────────────────┐
│              Provider 계층                 │
│  provider/mem/mem.go                       │
│  - 구독/브로드캐스트 패턴                    │
│  - AlertStoreCallback                      │
│  - GC goroutine 관리                       │
│                                            │
│  ┌─────────────────────────────────────┐  │
│  │          Store 계층                  │  │
│  │  store/store.go                     │  │
│  │  - map[Fingerprint]*Alert           │  │
│  │  - limit.Bucket (용량 제한)          │  │
│  │  - GC (만료 Alert 삭제)             │  │
│  │                                     │  │
│  │  ┌─────────────────────────────┐    │  │
│  │  │    Limit 계층               │    │  │
│  │  │  limit/bucket.go           │    │  │
│  │  │  - 힙 기반 용량 제한        │    │  │
│  │  │  - alertname별 제한         │    │  │
│  │  └─────────────────────────────┘    │  │
│  └─────────────────────────────────────┘  │
└───────────────────────────────────────────┘
```

## 10. Feature Control

```go
// featurecontrol/featurecontrol.go
type Flagger interface {
    EnableReceiverNamesInMetrics() bool
    ClassicMode() bool
    UTF8StrictMode() bool
}
```

Feature flag로 런타임 동작을 제어한다:

| 플래그 | 효과 |
|--------|------|
| `--enable-feature=receiver-names-in-metrics` | 메트릭에 receiver 이름 레이블 추가 |
| `--enable-feature=classic-mode` | Classic Matcher 파서만 사용 |
| `--enable-feature=utf8-strict-mode` | UTF-8 Matcher 파서만 사용 |

## 11. Tracing 구성

```go
// tracing/tracing.go
type Manager struct {
    shutdownFunc func() error
}

func (m *Manager) InitTracing(cfg tracing.TracingConfig) error
func (m *Manager) Shutdown() error
```

OpenTelemetry OTLP 프로토콜로 분산 추적 데이터를 내보낸다. 설정 파일의 `tracing` 섹션이나 CLI 플래그로 활성화한다.

## 12. 통합 테스트

```
test/with_api_v2/
├── acceptance/
│   ├── send_test.go          — Alert 전송 E2E
│   ├── silence_test.go       — Silence CRUD E2E
│   ├── inhibit_test.go       — Inhibition E2E
│   ├── cluster_test.go       — HA 클러스터 E2E
│   └── ...
```

통합 테스트는 실제 Alertmanager 바이너리를 시작하고, HTTP API를 통해 Alert 전송, Silence 생성, 클러스터 동기화 등을 검증한다. `Procfile`을 사용하여 goreman으로 멀티 인스턴스 HA 환경을 구성할 수 있다.
