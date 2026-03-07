# Jaeger 코드 구조

## 개요

Jaeger v2는 OpenTelemetry Collector를 런타임 프레임워크로 채택하면서 코드 구조가 근본적으로 재편되었다.
기존 Jaeger v1의 독립 컴포넌트(Agent, Collector, Query, Ingester)가 하나의 바이너리(`cmd/jaeger`)로 통합되었고,
OTel Collector의 Extension/Exporter/Processor/Receiver 체계 위에서 Jaeger 고유 기능을 구현한다.

이 문서에서는 Jaeger 프로젝트의 디렉토리 구조, 빌드 시스템, 주요 의존성, 코드 구성 패턴을 분석한다.

---

## 1. 전체 디렉토리 구조

```
jaeger/
├── cmd/                          # CLI 진입점 (실행 바이너리)
│   ├── jaeger/                   #   메인 바이너리 (All-in-One)
│   │   ├── main.go               #     프로그램 진입점
│   │   └── internal/             #     OTel Collector 컴포넌트 등록
│   │       ├── command.go        #       cobra 명령어 + OTel Collector 설정
│   │       ├── components.go     #       Extension/Receiver/Exporter/Processor/Connector 팩토리 등록
│   │       ├── all-in-one.yaml   #       기본 All-in-One 설정 (embed)
│   │       ├── exporters/        #       Jaeger 커스텀 Exporter
│   │       │   └── storageexporter/
│   │       ├── extension/        #       Jaeger 커스텀 Extension
│   │       │   ├── jaegerquery/  #         Query API + UI 서빙
│   │       │   ├── jaegerstorage/#         스토리지 팩토리 관리
│   │       │   ├── jaegermcp/    #         MCP 서버 (LLM 통합)
│   │       │   ├── remotesampling/  #      원격 샘플링
│   │       │   ├── remotestorage/#         원격 스토리지 gRPC 서버
│   │       │   └── expvar/       #         expvar 메트릭 노출
│   │       ├── processors/       #       Jaeger 커스텀 Processor
│   │       │   └── adaptivesampling/  #   적응형 샘플링
│   │       └── integration/      #       통합 테스트 유틸
│   │           └── storagecleaner/
│   ├── anonymizer/               #   트레이스 익명화 도구
│   ├── remote-storage/           #   원격 스토리지 gRPC 서버 (독립 바이너리)
│   ├── tracegen/                 #   부하 테스트용 트레이스 생성기
│   ├── es-index-cleaner/         #   Elasticsearch 인덱스 정리 도구
│   ├── es-rollover/              #   Elasticsearch 인덱스 롤오버 도구
│   ├── esmapping-generator/      #   Elasticsearch 매핑 생성기
│   └── internal/                 #   cmd 간 공유 유틸리티
│       ├── docs/                 #     문서 생성 명령어
│       └── storageconfig/        #     스토리지 설정 공통 로직
│
├── internal/                     # 핵심 패키지 (비공개)
│   ├── auth/                     #   인증 처리
│   ├── cache/                    #   캐시 추상화
│   ├── config/                   #   설정 관리 (TLS, 플래그)
│   ├── converter/                #   데이터 형식 변환
│   ├── distributedlock/          #   분산 락 인터페이스
│   ├── fswatcher/                #   파일 시스템 감시 (인증서 갱신)
│   ├── gogocodec/                #   gogo/protobuf 코덱
│   ├── grpctest/                 #   gRPC 테스트 유틸
│   ├── gzipfs/                   #   gzip 압축 파일 서빙
│   ├── hostname/                 #   호스트명 조회
│   ├── httpfs/                   #   HTTP 파일 시스템
│   ├── jaegerclientenv2otel/     #   Jaeger 환경변수 → OTel 변환
│   ├── jiter/                    #   JSON 이터레이터 유틸
│   ├── jptrace/                  #   ptrace(OTel pdata) 유틸리티
│   ├── jtracer/                  #   Jaeger 자체 트레이싱
│   ├── leaderelection/           #   리더 선출
│   ├── metrics/                  #   메트릭 추상화
│   ├── metricstest/              #   메트릭 테스트 유틸
│   ├── proto/                    #   Protobuf 정의 (생성된 코드)
│   ├── proto-gen/                #   Protobuf 생성 코드 (자동생성)
│   ├── recoveryhandler/          #   패닉 복구 핸들러
│   ├── safeexpvar/               #   스레드 안전 expvar
│   ├── sampling/                 #   샘플링 로직
│   ├── storage/                  #   스토리지 레이어 (아래 상세)
│   ├── telemetry/                #   텔레메트리 설정
│   ├── tenancy/                  #   멀티테넌시
│   ├── testutils/                #   테스트 유틸리티
│   ├── tools/                    #   빌드 도구 의존성
│   ├── tracegen/                 #   트레이스 생성 라이브러리
│   ├── uimodel/                  #   UI 데이터 모델
│   └── version/                  #   버전 정보
│
├── internal/storage/             # 스토리지 레이어
│   ├── v1/                       #   레거시 스토리지 API (v1)
│   │   ├── api/                  #     스토리지 인터페이스 정의
│   │   │   ├── spanstore/        #       SpanReader/SpanWriter 인터페이스
│   │   │   ├── dependencystore/  #       의존성 저장소 인터페이스
│   │   │   ├── samplingstore/    #       샘플링 저장소 인터페이스
│   │   │   └── metricstore/      #       메트릭 저장소 인터페이스
│   │   ├── badger/               #     BadgerDB 구현
│   │   ├── cassandra/            #     Cassandra 구현
│   │   ├── elasticsearch/        #     Elasticsearch 구현
│   │   ├── blackhole/            #     블랙홀 (데이터 폐기) 구현
│   │   ├── mocks/                #     목 구현 (테스트용)
│   │   └── factory.go            #     팩토리 인터페이스 정의
│   │
│   ├── v2/                       #   모던 스토리지 API (v2)
│   │   ├── api/                  #     v2 스토리지 인터페이스 정의
│   │   │   ├── tracestore/       #       Reader/Writer/Factory 인터페이스
│   │   │   └── depstore/         #       의존성 저장소 인터페이스
│   │   ├── memory/               #     인메모리 구현 (v2 네이티브)
│   │   ├── badger/               #     BadgerDB v2 래퍼 (v1 위임)
│   │   ├── cassandra/            #     Cassandra v2 래퍼
│   │   ├── elasticsearch/        #     Elasticsearch v2 래퍼
│   │   ├── clickhouse/           #     ClickHouse 구현 (v2 네이티브)
│   │   ├── grpc/                 #     gRPC 원격 스토리지 클라이언트
│   │   └── v1adapter/            #     v1 → v2 어댑터 브릿지
│   │
│   ├── cassandra/                #   Cassandra 공통 유틸리티
│   ├── elasticsearch/            #   Elasticsearch 공통 설정
│   ├── metricstore/              #   메트릭 스토리지 (Prometheus, ES)
│   ├── distributedlock/          #   분산 락 구현 (Cassandra)
│   └── integration/              #   스토리지 통합 테스트
│
├── examples/                     # 예제 애플리케이션
│   ├── hotrod/                   #   HotROD 데모 마이크로서비스
│   │   ├── main.go               #     진입점
│   │   ├── services/             #     4개 서비스 (customer, driver, frontend, route)
│   │   ├── pkg/                  #     공유 패키지 (delay, httperr, log, tracing)
│   │   └── cmd/                  #     CLI 명령어
│   ├── grafana-integration/      #   Grafana 통합 예제
│   ├── oci/                      #   OCI 컨테이너 예제
│   ├── otel-demo/                #   OTel 데모 통합 예제
│   ├── reverse-proxy/            #   리버스 프록시 예제
│   └── service-performance-monitoring/  # SPM 예제
│
├── docker-compose/               # Docker Compose 스토리지 백엔드 설정
│   ├── cassandra/                #   Cassandra 설정
│   ├── clickhouse/               #   ClickHouse 설정
│   ├── elasticsearch/            #   Elasticsearch 설정
│   ├── kafka/                    #   Kafka 설정 (버퍼링 파이프라인)
│   ├── opensearch/               #   OpenSearch 설정
│   ├── scylladb/                 #   ScyllaDB 설정
│   ├── monitor/                  #   SPM (Service Performance Monitoring) 설정
│   └── tail-sampling/            #   테일 샘플링 설정
│
├── docs/                         # 프로젝트 문서
│   ├── adr/                      #   Architecture Decision Records
│   │   ├── 001-cassandra-find-traces-duration.md
│   │   ├── 002-mcp-server.md
│   │   ├── 003-lazy-storage-factory-initialization.md
│   │   └── 004-migrating-coverage-gating-to-github-actions.md
│   ├── release/                  #   릴리스 절차 문서
│   └── security/                 #   보안 문서
│
├── idl/                          # Protobuf IDL 정의 (git submodule)
├── jaeger-ui/                    # UI 프론트엔드 (git submodule)
├── ports/                        # 포트 상수 정의
│   └── ports.go                  #   QueryHTTP(16686), QueryGRPC(16685), MCPHTTP(16687) 등
├── monitoring/                   # 모니터링 대시보드
│   └── jaeger-mixin/             #   Grafana Mixin (Jsonnet 기반 대시보드)
├── scripts/                      # 빌드/배포 스크립트
│   ├── build/                    #   빌드 스크립트
│   ├── makefiles/                #   분할된 Makefile 모듈
│   ├── lint/                     #   린트 스크립트
│   ├── release/                  #   릴리스 스크립트
│   ├── e2e/                      #   E2E 테스트 스크립트
│   └── utils/                    #   유틸리티 스크립트
│
├── Makefile                      # 메인 빌드 파일
├── go.mod                        # Go 모듈 정의
├── go.sum                        # 의존성 체크섬
└── doc.go                        # 패키지 문서
```

### 주요 디렉토리 역할 요약

| 디렉토리 | 역할 | 설명 |
|----------|------|------|
| `cmd/jaeger/` | 메인 바이너리 | OTel Collector 기반 All-in-One 실행 파일 |
| `cmd/jaeger/internal/` | OTel 컴포넌트 등록 | Extension, Exporter, Processor 팩토리 조립 |
| `cmd/remote-storage/` | 원격 스토리지 서버 | 독립 gRPC 스토리지 서비스 |
| `cmd/anonymizer/` | 트레이스 익명화 | 프로덕션 데이터 익명화 도구 |
| `cmd/tracegen/` | 부하 생성기 | 테스트용 트레이스 데이터 생성 |
| `cmd/es-index-cleaner/` | ES 인덱스 정리 | Elasticsearch 오래된 인덱스 삭제 |
| `cmd/es-rollover/` | ES 인덱스 롤오버 | Elasticsearch 인덱스 롤오버 관리 |
| `cmd/esmapping-generator/` | ES 매핑 생성 | Elasticsearch 인덱스 매핑 스키마 생성 |
| `internal/` | 핵심 비공개 패키지 | 외부에서 import 불가한 내부 구현 |
| `internal/storage/v1/` | 레거시 스토리지 | SpanReader/SpanWriter 기반 v1 API |
| `internal/storage/v2/` | 모던 스토리지 | Reader/Writer/Factory 기반 v2 API |
| `examples/hotrod/` | 데모 앱 | 4개 마이크로서비스 분산 추적 데모 |
| `idl/` | Protobuf IDL | jaeger-idl 서브모듈 (데이터 모델 정의) |
| `jaeger-ui/` | UI 프론트엔드 | React 기반 웹 UI (서브모듈) |

---

## 2. 빌드 시스템

### 2.1 Go 모듈

```
module github.com/jaegertracing/jaeger
go 1.26.0
```

Jaeger는 단일 Go 모듈(`go.mod`)로 관리된다. Go 1.26을 최소 버전으로 요구하며,
모든 패키지가 하나의 모듈 내에 포함되어 의존성 관리가 단순하다.

### 2.2 Makefile 구조

메인 `Makefile`은 7개의 분할 Makefile을 포함한다.

```makefile
# Makefile (상단)
include scripts/makefiles/BuildBinaries.mk    # 바이너리 빌드
include scripts/makefiles/BuildInfo.mk         # 버전/빌드 정보
include scripts/makefiles/Docker.mk            # Docker 이미지 빌드
include scripts/makefiles/IntegrationTests.mk  # 통합 테스트
include scripts/makefiles/Protobuf.mk          # Protobuf 코드 생성
include scripts/makefiles/Tools.mk             # 빌드 도구 설치
include scripts/makefiles/Windows.mk           # Windows 빌드 지원
```

### 2.3 주요 Makefile 타겟

| 타겟 | 명령어 | 설명 |
|------|--------|------|
| 기본 목표 | `make` | `test` + `fmt` + `lint` 실행 |
| 포맷팅 | `make fmt` | gofmt + gofumpt + import 정렬 + 라이선스 헤더 |
| 린트 | `make lint` | fmt, license, imports, semconv, go version, goleak, golangci-lint |
| 테스트 | `make test` | `go test -race -v -tags=memory_storage_integration ./...` |
| 커버리지 | `make cover` | 테스트 + 커버리지 리포트 생성 |
| 빌드 | `make build-jaeger` | UI 빌드 포함 메인 바이너리 빌드 |
| 전체 빌드 | `make build-binaries` | 현재 플랫폼의 모든 바이너리 빌드 |
| 클린 | `make clean` | 빌드 아티팩트 및 캐시 정리 |
| UI 빌드 | `make build-ui` | jaeger-ui 서브모듈에서 UI 빌드 후 gzip 압축 |

### 2.4 빌드 바이너리

`_build-platform-binaries` 타겟이 빌드하는 전체 바이너리 목록:

```makefile
# scripts/makefiles/BuildBinaries.mk
_build-platform-binaries: \
    build-jaeger \              # cmd/jaeger         → 메인 바이너리 (All-in-One)
    build-remote-storage \      # cmd/remote-storage → 원격 스토리지 서버
    build-examples \            # examples/hotrod    → HotROD 데모
    build-tracegen \            # cmd/tracegen       → 트레이스 생성기
    build-anonymizer \          # cmd/anonymizer     → 트레이스 익명화
    build-esmapping-generator \ # cmd/esmapping-generator → ES 매핑 생성
    build-es-index-cleaner \    # cmd/es-index-cleaner    → ES 인덱스 정리
    build-es-rollover           # cmd/es-rollover         → ES 인덱스 롤오버
```

빌드 명령은 CGO를 비활성화하고 경로를 정리하여 재현 가능한 바이너리를 생성한다:

```makefile
# BuildBinaries.mk
GOBUILD_EXEC := CGO_ENABLED=0 installsuffix=cgo $(GO) build -trimpath
```

### 2.5 멀티 플랫폼 지원

Jaeger는 7개 플랫폼을 공식 지원한다:

```makefile
# Makefile
PLATFORMS="linux/amd64,linux/arm64,linux/s390x,linux/ppc64le,darwin/amd64,darwin/arm64,windows/amd64"
```

| 플랫폼 | 빌드 타겟 |
|--------|----------|
| `linux/amd64` | `make build-binaries-linux-amd64` |
| `linux/arm64` | `make build-binaries-linux-arm64` |
| `linux/s390x` | `make build-binaries-linux-s390x` |
| `linux/ppc64le` | `make build-binaries-linux-ppc64le` |
| `darwin/amd64` | `make build-binaries-darwin-amd64` |
| `darwin/arm64` | `make build-binaries-darwin-arm64` |
| `windows/amd64` | `make build-binaries-windows-amd64` |

`jaeger`와 `remote-storage` 바이너리는 디버그 빌드(`-gcflags="all=-N -l"`)도 추가로 생성된다.
`SKIP_DEBUG_BINARIES=1` 환경변수로 CI에서 디버그 빌드를 건너뛸 수 있다.

---

## 3. 주요 의존성

### 3.1 OpenTelemetry Collector (코어 런타임)

Jaeger v2의 런타임 프레임워크이다. `cmd/jaeger/main.go`에서 `otelcol.NewCommand()`로 Collector를 시작한다.

```
go.opentelemetry.io/collector/otelcol       v0.146.1   # Collector 런타임
go.opentelemetry.io/collector/component      v1.52.0    # 컴포넌트 인터페이스
go.opentelemetry.io/collector/extension      v1.52.0    # Extension 인터페이스
go.opentelemetry.io/collector/receiver       v1.52.0    # Receiver 인터페이스
go.opentelemetry.io/collector/exporter       v1.52.0    # Exporter 인터페이스
go.opentelemetry.io/collector/processor      v1.52.0    # Processor 인터페이스
go.opentelemetry.io/collector/connector      v0.146.1   # Connector 인터페이스
go.opentelemetry.io/collector/confmap        v1.52.0    # 설정 관리
go.opentelemetry.io/collector/pdata          v1.52.0    # 파이프라인 데이터 모델
```

### 3.2 OTel Collector Contrib (수신/내보내기/처리)

`cmd/jaeger/internal/components.go`에서 등록되는 Contrib 컴포넌트들:

| 카테고리 | 패키지 | 용도 |
|---------|--------|------|
| Receiver | `jaegerreceiver` | Jaeger Thrift/gRPC 프로토콜 수신 |
| Receiver | `kafkareceiver` | Kafka 토픽에서 트레이스 소비 |
| Receiver | `zipkinreceiver` | Zipkin 프로토콜 수신 |
| Exporter | `kafkaexporter` | Kafka 토픽으로 트레이스 전송 |
| Exporter | `prometheusexporter` | Prometheus 메트릭 노출 |
| Processor | `tailsamplingprocessor` | 테일 기반 샘플링 |
| Processor | `attributesprocessor` | 속성 추가/수정/삭제 |
| Processor | `filterprocessor` | 트레이스 필터링 |
| Connector | `spanmetricsconnector` | 스팬 → 메트릭 변환 (RED 메트릭) |
| Extension | `healthcheckv2extension` | 헬스체크 엔드포인트 |
| Extension | `pprofextension` | Go pprof 프로파일링 |
| Extension | `basicauthextension` | HTTP 기본 인증 |
| Extension | `sigv4authextension` | AWS SigV4 인증 |

### 3.3 스토리지 드라이버

```
github.com/dgraph-io/badger/v4                v4.9.1   # 임베디드 KV 스토어 (로컬)
github.com/apache/cassandra-gocql-driver/v2    v2.0.0   # Cassandra 드라이버
github.com/elastic/go-elasticsearch/v9         v9.3.1   # Elasticsearch v9 클라이언트
github.com/olivere/elastic/v7                  v7.0.32  # Elasticsearch v7 클라이언트 (레거시)
github.com/ClickHouse/clickhouse-go/v2         v2.43.0  # ClickHouse 드라이버
github.com/ClickHouse/ch-go                    v0.71.0  # ClickHouse 저수준 드라이버
```

### 3.4 Jaeger IDL

```
github.com/jaegertracing/jaeger-idl  v0.6.0
```

Jaeger의 데이터 모델(Span, Trace, Process 등)을 정의하는 Protobuf IDL이다.
`idl/` 서브모듈로도 포함되어 있으며, `jaeger-idl` 패키지를 통해 Go 코드로 참조한다.

### 3.5 MCP SDK

```
github.com/modelcontextprotocol/go-sdk  v1.3.1
```

ADR-002에 따라 도입된 MCP(Model Context Protocol) 서버 기능이다.
`cmd/jaeger/internal/extension/jaegermcp/`에서 LLM이 Jaeger 데이터를 조회할 수 있는 MCP 서버를 제공한다.

### 3.6 CLI 프레임워크

```
github.com/spf13/cobra   v1.10.2   # CLI 명령어 프레임워크
github.com/spf13/viper   v1.21.0   # 설정 관리 (환경변수, 파일, 플래그)
github.com/spf13/pflag   v1.0.10   # POSIX 호환 CLI 플래그
```

### 3.7 Protobuf

```
github.com/gogo/protobuf    v1.3.2     # v1 스토리지 API의 직렬화 (레거시)
google.golang.org/protobuf  v1.36.11   # 표준 Protobuf (v2 API)
google.golang.org/grpc      v1.79.1    # gRPC 프레임워크
```

### 3.8 의존성 구조 다이어그램

```
cmd/jaeger/main.go
    │
    ├── spf13/cobra (CLI)
    ├── spf13/viper (설정)
    │
    └── otelcol.NewCommand() ──────────── OTel Collector 런타임
         │
         ├── Receivers ─── otlpreceiver, jaegerreceiver, kafkareceiver, zipkinreceiver
         ├── Processors ── batchprocessor, tailsamplingprocessor, adaptivesampling
         ├── Exporters ─── storageexporter, kafkaexporter, otlpexporter
         ├── Connectors ── forwardconnector, spanmetricsconnector
         └── Extensions ── jaegerstorage, jaegerquery, jaegermcp, remotesampling
                               │
                               └── Storage Factories
                                    ├── v2/memory     (인메모리)
                                    ├── v2/badger     → v1/badger (v1adapter)
                                    ├── v2/cassandra  → v1/cassandra (v1adapter)
                                    ├── v2/elasticsearch → v1/elasticsearch (v1adapter)
                                    ├── v2/clickhouse (v2 네이티브)
                                    └── v2/grpc       (원격 스토리지)
```

---

## 4. 코드 구성 패턴

### 4.1 Factory 패턴

Jaeger의 모든 OTel Collector 컴포넌트는 Factory 패턴으로 생성된다.
OTel Collector 프레임워크가 요구하는 표준 패턴이다.

```
[Factory 인터페이스]
  component.Type       → 컴포넌트 타입명 (예: "jaeger_storage")
  createDefaultConfig  → 기본 설정 생성
  createExtension      → 인스턴스 생성 (설정 주입)
  StabilityLevel       → 안정성 수준 (Alpha/Beta/Stable)
```

실제 코드 예시 (`cmd/jaeger/internal/extension/jaegerstorage/factory.go`):

```go
// componentType is the name of this extension in configuration.
var componentType = component.MustNewType("jaeger_storage")

func NewFactory() extension.Factory {
    return extension.NewFactory(
        componentType,
        createDefaultConfig,
        createExtension,
        component.StabilityLevelBeta,
    )
}

func createDefaultConfig() component.Config {
    return &Config{}
}

func createExtension(
    _ context.Context,
    set extension.Settings,
    cfg component.Config,
) (extension.Extension, error) {
    return newStorageExt(cfg.(*Config), set.TelemetrySettings), nil
}
```

`cmd/jaeger/internal/components.go`에서 모든 Factory를 조립한다:

```go
func Components() (otelcol.Factories, error) {
    return defaultBuilders().build()
}
```

이 `build()` 메서드 안에서 Extension, Receiver, Exporter, Processor, Connector 각각의
Factory를 `otelcol.MakeFactoryMap`으로 등록한다.

### 4.2 Extension 인터페이스 패턴 (라이프사이클 관리)

Jaeger의 핵심 기능(`jaegerstorage`, `jaegerquery`, `remotesampling`, `jaegermcp`)은
모두 OTel Collector Extension으로 구현된다. Extension은 라이프사이클 관리를 제공한다:

```
[Extension 라이프사이클]
  Start(ctx, host)  → 초기화 (스토리지 연결, HTTP 서버 시작 등)
  Shutdown(ctx)      → 정리 (연결 종료, 리소스 해제)
```

`jaegerstorage` Extension의 역할:

```go
// extension.go
type Extension interface {
    extension.Extension
    TraceStorageFactory(name string) (tracestore.Factory, error)
    MetricStorageFactory(name string) (storage.MetricStoreFactory, error)
}
```

다른 컴포넌트(예: `storageexporter`, `jaegerquery`)가 `host`에서 이 Extension을 찾아
스토리지 팩토리를 조회하는 서비스 로케이터 패턴으로 동작한다:

```go
func getStorageFactory(name string, host component.Host) (tracestore.Factory, error) {
    ext, err := findExtension(host)
    if err != nil {
        return nil, err
    }
    f, err := ext.TraceStorageFactory(name)
    // ...
}
```

### 4.3 v1/v2 어댑터 패턴

Jaeger는 v1(레거시)에서 v2(모던) 스토리지 API로 전환 중이다.
`internal/storage/v2/v1adapter/` 패키지가 두 API를 브릿지한다.

**v1 API** (`internal/storage/v1/api/spanstore/`):
- `SpanReader` / `SpanWriter` 인터페이스
- Jaeger 고유 데이터 모델(`model.Span`) 사용
- gogo/protobuf 기반

**v2 API** (`internal/storage/v2/api/tracestore/`):
- `Reader` / `Writer` / `Factory` 인터페이스
- OTel pdata(`ptrace.Traces`) 사용
- 표준 protobuf 기반

```go
// internal/storage/v2/api/tracestore/factory.go
type Factory interface {
    CreateTraceReader() (Reader, error)
    CreateTraceWriter() (Writer, error)
}
```

v1adapter의 역할은 v1 SpanWriter를 v2 Writer로 변환하는 것이다:

```go
// internal/storage/v2/badger/factory.go
func (f *Factory) CreateTraceWriter() (tracestore.Writer, error) {
    v1Writer, _ := f.v1Factory.CreateSpanWriter()
    return v1adapter.NewTraceWriter(v1Writer), nil  // v1 → v2 어댑터
}
```

**전환 상태 요약**:

| 스토리지 백엔드 | v2 구현 방식 | 설명 |
|---------------|------------|------|
| Memory | v2 네이티브 | `v2/memory/` 직접 구현 |
| ClickHouse | v2 네이티브 | `v2/clickhouse/` 직접 구현 |
| BadgerDB | v1 위임 | `v2/badger/` → `v1/badger/` + v1adapter |
| Cassandra | v1 위임 | `v2/cassandra/` → `v1/cassandra/` + v1adapter |
| Elasticsearch | v1 위임 | `v2/elasticsearch/` → `v1/elasticsearch/` + v1adapter |
| gRPC | v2 네이티브 | `v2/grpc/` 직접 구현 (원격 스토리지) |

### 4.4 패키지 네이밍 컨벤션

| 패턴 | 위치 | 설명 |
|------|------|------|
| `internal/` | 프로젝트 루트 | 모든 핵심 로직 — 외부 import 불가 |
| `cmd/` | 프로젝트 루트 | 실행 바이너리 진입점만 포함 |
| `cmd/*/internal/` | cmd 하위 | 해당 바이너리 전용 내부 패키지 |
| `api/` | 스토리지 하위 | 인터페이스 정의 (구현 분리) |
| `mocks/` | 테스트 전용 | 자동 생성된 목 구현체 |
| `*_test.go` | 각 패키지 | 테스트 파일 (모든 패키지에 필수) |

Go의 `internal` 패키지 규칙에 의해 `internal/` 하위의 모든 패키지는
`github.com/jaegertracing/jaeger` 모듈 외부에서 import할 수 없다.
이는 Jaeger의 API 표면을 의도적으로 제한하여 내부 구현 변경의 자유도를 확보한다.

### 4.5 서브모듈 구조

```
jaeger/
├── idl/        → github.com/jaegertracing/jaeger-idl    # Protobuf IDL 정의
└── jaeger-ui/  → github.com/jaegertracing/jaeger-ui     # React UI 프론트엔드
```

두 서브모듈은 독립 저장소에서 관리되며, Jaeger 메인 저장소에서는 특정 커밋을 참조한다.
UI 빌드 결과물은 `cmd/jaeger/internal/extension/jaegerquery/internal/ui/actual/`에
gzip 압축된 형태로 임베드된다.

---

## 5. 진입점 흐름

`cmd/jaeger/main.go`에서 시작하는 실행 흐름을 정리한다:

```
main()                                          # cmd/jaeger/main.go
  ├── viper.New()                               # 설정 관리자 생성
  ├── internal.Command()                        # cmd/jaeger/internal/command.go
  │     ├── component.BuildInfo{...}            # 버전 정보 설정
  │     ├── otelcol.CollectorSettings{          # OTel Collector 설정
  │     │     Factories: Components,            #   → components.go의 팩토리 등록 함수
  │     │     ConfigProviderSettings: {         #   → 설정 소스 (env, file, http, yaml)
  │     │       envprovider, fileprovider,
  │     │       httpprovider, httpsprovider,
  │     │       yamlprovider
  │     │     }
  │     │   }
  │     └── otelcol.NewCommand(settings)        # OTel Collector 명령어 생성
  │           └── RunE: checkConfigAndRun()     # --config 미지정 시 all-in-one.yaml 사용
  ├── command.AddCommand(version, docs, mappings)
  ├── config.AddFlags(v, command)
  └── command.Execute()                         # cobra 실행 → OTel Collector 시작
```

`--config` 플래그 없이 실행하면 `all-in-one.yaml`이 임베드된 설정으로 자동 적용되어,
메모리 스토리지 기반의 All-in-One 모드로 동작한다.

---

## 참고

- 소스 경로: `github.com/jaegertracing/jaeger` (분석 시점 main 브랜치)
- Go 모듈 버전: Go 1.26.0
- OTel Collector 버전: v0.146.1 / v1.52.0
- 자동 생성 파일(`*.pb.go`, `*_mock.go`, `internal/proto-gen/`)은 수동 편집 대상이 아님
