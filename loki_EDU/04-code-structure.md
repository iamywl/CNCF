# Loki 코드 구조

## 1. 프로젝트 개요

| 항목 | 내용 |
|------|------|
| 모듈 | `github.com/grafana/loki/v3` |
| Go 버전 | 1.25+ |
| 빌드 시스템 | Makefile + Go modules |
| 패키지 관리 | vendor/ (vendored dependencies) |
| 라이선스 | AGPL-3.0 |

---

## 2. 최상위 디렉토리 구조

```
loki/
├── cmd/                    # 실행 바이너리 진입점 (11개)
├── pkg/                    # 핵심 라이브러리 패키지 (48+개)
├── clients/                # 로그 수집 에이전트 (Promtail 등)
├── operator/               # Kubernetes Operator
├── production/             # 배포 설정 (Helm, Terraform, Docker, Nomad)
├── tools/                  # 개발/운영 유틸리티
├── docs/                   # 공식 문서 (mkdocs)
├── examples/               # 빠른 시작 예제
├── integration/            # 통합 테스트
├── vendor/                 # Go 벤더 의존성
├── debug/                  # 디버그 도구
├── nix/                    # Nix 빌드 설정
├── loki-build-image/       # Docker 빌드 이미지
├── go.mod                  # Go 모듈 정의
├── go.sum                  # 의존성 해시
├── Makefile                # 빌드 타겟
├── mkdocs.yml              # 문서 빌드 설정
└── .github/                # CI/CD 워크플로우
```

---

## 3. cmd/ — 실행 바이너리

| 바이너리 | 위치 | 설명 |
|----------|------|------|
| **loki** | `cmd/loki/` | 메인 서버 (distributor, ingester, querier 등) |
| **logcli** | `cmd/logcli/` | LogQL 쿼리 CLI 도구 |
| **loki-canary** | `cmd/loki-canary/` | 로그 파이프라인 헬스체크 (로그 손실 감지) |
| **querytee** | `cmd/querytee/` | 쿼리 트래픽 복제/비교 도구 |
| **lokitool** | `cmd/lokitool/` | 관리 유틸리티 |
| **migrate** | `cmd/migrate/` | 데이터 마이그레이션 도구 |
| **chunks-inspect** | `cmd/chunks-inspect/` | 청크 파일 검사 도구 |
| **dataobj-inspect** | `cmd/dataobj-inspect/` | DataObj 파일 검사 |
| **logql-analyzer** | `cmd/logql-analyzer/` | LogQL 쿼리 분석기 |

### 3.1 loki 메인 진입점

소스: `cmd/loki/main.go`

```go
func main() {
    // 1. 설정 파싱 (YAML + CLI 플래그)
    cfg.DynamicUnmarshal(&config, os.Args)

    // 2. 유효성 검증
    config.Validate()

    // 3. Loki 인스턴스 생성
    t := loki.New(config.Config)

    // 4. 실행 (모듈 초기화 → 서버 시작)
    t.Run(RunOpts{StartTime: startTime})
}
```

---

## 4. pkg/ — 핵심 패키지

### 4.1 오케스트레이션

| 패키지 | 설명 |
|--------|------|
| `pkg/loki/` | Loki 메인 구조체, Config, Module Manager |
| `pkg/loki/modules.go` | 30+ 모듈 등록, 의존성 그래프 |

### 4.2 쓰기 경로 (Write Path)

| 패키지 | 설명 |
|--------|------|
| `pkg/distributor/` | 로그 분배, 검증, 레이트 리밋, Ring 라우팅 |
| `pkg/ingester/` | 인메모리 로그 저장, WAL, 청크 관리, 플러시 |
| `pkg/ingester/index/` | 인메모리 역인덱스 (레이블 → 핑거프린트) |
| `pkg/ingester/wal/` | Write-Ahead Log 구현 |
| `pkg/push/` | Push API 타입 (Stream, Entry) |
| `pkg/validation/` | 입력 검증, 테넌트별 제한 (Limits) |

### 4.3 읽기 경로 (Read Path)

| 패키지 | 설명 |
|--------|------|
| `pkg/querier/` | 쿼리 실행, 듀얼 소스 (Ingester + Store) 병합 |
| `pkg/lokifrontend/` | 쿼리 프론트엔드, 캐싱, 요청 큐잉 |
| `pkg/scheduler/` | 쿼리 스케줄러, 공정 큐 |
| `pkg/logql/` | LogQL 쿼리 엔진 (파서, 평가기) |
| `pkg/logql/syntax/` | LogQL AST 정의, 파서 (yacc 생성) |
| `pkg/logql/log/` | 파이프라인 스테이지 (필터, 파서, 포매터) |
| `pkg/logqlmodel/` | 쿼리 응답 모델 |
| `pkg/iter/` | 반복자 추상화 (Entry, Sample, Merge) |

### 4.4 스토리지

| 패키지 | 설명 |
|--------|------|
| `pkg/storage/` | 스토리지 백엔드 추상화 |
| `pkg/storage/chunk/` | 영구 청크 구조 |
| `pkg/storage/stores/` | 스토리지 구현체 |
| `pkg/storage/stores/shipper/indexshipper/tsdb/` | TSDB 인덱스 |
| `pkg/chunkenc/` | 청크 인코딩/압축 (MemChunk, Block) |
| `pkg/compression/` | 압축 코덱 (gzip, lz4, snappy, zstd) |

### 4.5 백엔드 서비스

| 패키지 | 설명 |
|--------|------|
| `pkg/compactor/` | 인덱스 압축, 보존 정책, 삭제 처리 |
| `pkg/ruler/` | 알림 규칙 평가, Alertmanager 연동 |
| `pkg/indexgateway/` | 인덱스 게이트웨이, Querier 오프로드 |
| `pkg/bloomgateway/` | 블룸 필터 게이트웨이 (실험적) |
| `pkg/bloombuild/` | 블룸 인덱스 빌더 (실험적) |
| `pkg/pattern/` | 로그 패턴 감지 (Drain 알고리즘) |

### 4.6 인프라

| 패키지 | 설명 |
|--------|------|
| `pkg/logproto/` | gRPC 프로토콜 정의 (.proto + 생성 코드) |
| `pkg/loghttp/` | HTTP API 구조체 |
| `pkg/util/` | 범용 유틸리티 |
| `pkg/runtime/` | 런타임 설정 리로드 |
| `pkg/limits/` | 레이트 리밋, 테넌트 제한 |
| `pkg/tracing/` | 분산 트레이싱 (OpenTelemetry) |
| `pkg/analytics/` | 사용 통계 수집 |

### 4.7 고급 기능

| 패키지 | 설명 |
|--------|------|
| `pkg/columnar/` | 컬럼나 데이터 포맷 |
| `pkg/dataobj/` | DataObj 직렬화 |
| `pkg/engine/` | 쿼리 엔진 v2 |
| `pkg/kafka/` | Kafka 연동 (레거시) |
| `pkg/kafkav2/` | Kafka 연동 v2 |
| `pkg/canary/` | Canary 모니터링 로직 |

---

## 5. clients/ — 로그 수집 에이전트

```
clients/
├── cmd/
│   ├── promtail/              # Promtail 메인 바이너리
│   ├── docker-driver/         # Docker 로그 드라이버 플러그인
│   ├── fluentd/               # Fluentd 플러그인
│   ├── fluent-bit/            # Fluent Bit 플러그인
│   └── logstash/              # Logstash 플러그인
└── pkg/
    └── promtail/
        ├── client/            # Loki 전송 클라이언트
        ├── targets/           # 로그 소스 (19개 타겟)
        │   ├── docker/        # Docker 컨테이너 로그
        │   ├── kubernetes/    # Kubernetes Pod 로그
        │   ├── journal/       # systemd journal
        │   ├── syslog/        # Syslog
        │   ├── file/          # 파일 기반
        │   └── ...
        ├── server/            # Promtail HTTP/gRPC 서버
        ├── config/            # 설정 파싱
        ├── positions/         # 파일 읽기 위치 추적
        └── wal/               # Write-Ahead Log
```

---

## 6. production/ — 배포 설정

```
production/
├── helm/
│   ├── loki/                  # Loki Helm 차트 (메인)
│   ├── promtail/              # Promtail Helm 차트
│   ├── loki-stack/            # 통합 스택 차트
│   ├── fluent-bit/            # Fluent Bit 차트
│   └── meta-monitoring/       # 메타 모니터링 차트
├── terraform/                 # Terraform 모듈
├── docker/                    # Docker Compose 설정
├── ksonnet/                   # Jsonnet 템플릿
├── nomad/                     # Nomad 배포 설정
├── loki-mixin/                # Grafana 대시보드 + 알림 규칙
└── loki-mixin-compiled/       # 컴파일된 mixin
```

---

## 7. 빌드 시스템

### 7.1 Makefile 주요 타겟

| 타겟 | 설명 |
|------|------|
| `make all` | 모든 바이너리 빌드 (loki, logcli, promtail, loki-canary) |
| `make loki` | Loki 서버 빌드 |
| `make logcli` | LogQL CLI 빌드 |
| `make promtail` | Promtail 빌드 |
| `make loki-canary` | Canary 빌드 |
| `make test` | 유닛 테스트 |
| `make test-integration` | 통합 테스트 |
| `make lint` | 린터 실행 |
| `make docker-build` | Docker 이미지 빌드 |
| `make fmt-proto` | Protobuf 포맷팅 |

### 7.2 빌드 플래그

```makefile
# 정적 링크, 심볼 제거, 버전 주입
CGO_ENABLED=0 go build \
    -ldflags="-s -w \
        -X github.com/grafana/loki/v3/pkg/util/build.Version=$(VERSION) \
        -X github.com/grafana/loki/v3/pkg/util/build.Revision=$(REVISION) \
        -X github.com/grafana/loki/v3/pkg/util/build.Branch=$(BRANCH)" \
    -o cmd/loki/loki cmd/loki/main.go
```

### 7.3 Docker 이미지

| 이미지 | 태그 | 용도 |
|--------|------|------|
| `grafana/loki` | `latest` | Loki 서버 |
| `grafana/promtail` | `latest` | Promtail 에이전트 |
| `grafana/logcli` | `latest` | LogQL CLI |
| `grafana/loki-canary` | `latest` | 카나리 모니터 |
| `grafana/loki-operator` | `latest` | K8s Operator |

---

## 8. 핵심 의존성

소스: `go.mod`

| 의존성 | 용도 |
|--------|------|
| `github.com/grafana/dskit` | Ring, KV 스토어, 서비스 프레임워크, 미들웨어 |
| `github.com/prometheus/prometheus` | 레이블, 쿼리 엔진 기반 |
| `github.com/prometheus/client_golang` | Prometheus 메트릭 |
| `google.golang.org/grpc` | gRPC 통신 |
| `github.com/grpc-ecosystem/go-grpc-middleware/v2` | gRPC 인터셉터 |
| `github.com/aws/aws-sdk-go-v2` | S3 스토리지 |
| `cloud.google.com/go/storage` | GCS 스토리지 |
| `github.com/Azure/azure-sdk-for-go` | Azure Blob 스토리지 |
| `go.etcd.io/bbolt` | BoltDB 인덱스 |
| `github.com/cespare/xxhash/v2` | 고속 해싱 |
| `github.com/grafana/regexp` | 최적화된 정규식 |

---

## 9. 코드 생성

### 9.1 Protobuf

```
pkg/logproto/logproto.proto     → logproto.pb.go (gRPC 서비스)
pkg/push/push.proto             → push.pb.go (Push API)
pkg/logproto/indexgateway.proto → indexgateway.pb.go
pkg/logproto/pattern.proto      → pattern.pb.go
pkg/logproto/bloomgateway.proto → bloomgateway.pb.go
```

### 9.2 LogQL 파서

```
pkg/logql/syntax/syntax.y       → syntax.y.go (yacc 생성 파서)
pkg/logql/syntax/lex.go          → 렉서 (수동 구현)
```

### 9.3 Ring (dskit 기반)

```
dskit/ring/ring.go               → Consistent Hash Ring
dskit/kv/memberlist/             → Memberlist 기반 KV
dskit/services/                  → 서비스 라이프사이클
```

---

## 10. 테스트 구조

```
# 유닛 테스트 — 패키지 내 *_test.go
pkg/ingester/instance_test.go
pkg/distributor/distributor_test.go
pkg/querier/querier_test.go
pkg/logql/syntax/ast_test.go

# 통합 테스트
integration/
├── util/
├── client/
└── cluster/

# 벤치마크 (파일 내 Benchmark* 함수)
pkg/chunkenc/memchunk_test.go   # BenchmarkMemChunk*
pkg/logql/syntax/lex_test.go     # BenchmarkLexer*
```

---

## 11. 설정 파일 구조

```yaml
# loki-config.yaml 최소 설정
auth_enabled: false

server:
  http_listen_port: 3100

common:
  ring:
    instance_addr: 127.0.0.1
    kvstore:
      store: inmemory
  replication_factor: 1
  path_prefix: /tmp/loki

schema_config:
  configs:
    - from: 2020-10-24
      store: tsdb
      object_store: filesystem
      schema: v13
      index:
        prefix: index_
        period: 24h

storage_config:
  filesystem:
    directory: /tmp/loki/chunks
```

---

## 12. 참고 자료

- 메인 진입점: `cmd/loki/main.go`
- 모듈 등록: `pkg/loki/modules.go`
- Go 모듈: `go.mod`
- 빌드: `Makefile`
- Helm 차트: `production/helm/loki/`
- Docker: `production/docker/`
