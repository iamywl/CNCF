# Prometheus 코드 구조

> Prometheus v3.x 소스코드 기준. 모듈 경로: `github.com/prometheus/prometheus`

---

## 1. 최상위 디렉토리 구조

```
prometheus/
├── cmd/                    # 실행 바이너리 진입점
│   ├── prometheus/         #   Prometheus 서버 (main.go, 2151줄)
│   └── promtool/           #   CLI 유틸리티 도구 (main.go, 1427줄)
├── config/                 # 설정 파싱 (YAML → Go 구조체)
├── discovery/              # 서비스 디스커버리 프레임워크 + 프로바이더
├── model/                  # 데이터 모델 (labels, textparse, relabel 등)
├── notifier/               # Alertmanager 알림 발송
├── plugins/                # 빌드 태그 기반 SD 플러그인 등록
├── prompb/                 # Protobuf 정의 (Remote Write/Read 프로토콜)
├── promql/                 # PromQL 엔진 + 파서
├── rules/                  # 규칙 평가 (alerting, recording)
├── scrape/                 # 메트릭 스크래핑
├── schema/                 # 레이블 스키마 검증
├── storage/                # 저장소 추상화 인터페이스 + Remote Storage
├── template/               # 알림 템플릿 엔진
├── tracing/                # OpenTelemetry 트레이싱 통합
├── tsdb/                   # 시계열 데이터베이스 (핵심 저장소 엔진)
├── util/                   # 유틸리티 패키지 모음
├── web/                    # HTTP 서버, API v1, UI
├── internal/               # 내부 도구 (코드 생성 등)
├── compliance/             # Remote Write 준수 테스트
├── docs/                   # 내부 문서
├── documentation/          # 예제 설정 파일
├── scripts/                # 빌드/릴리스 스크립트
├── go.mod                  # Go 모듈 정의
├── Makefile                # 빌드 타겟
└── .promu.yml              # promu 빌드 도구 설정
```

---

## 2. 핵심 패키지 상세

### 2.1 cmd/ — 실행 바이너리

#### cmd/prometheus/ (서버 진입점)

| 파일 | 줄수 | 역할 |
|------|------|------|
| `main.go` | 2,151 | 전체 서버 조립: 플래그 파싱 → 설정 로드 → 컴포넌트 생성 → `run.Group` 실행 |

`main.go`는 Prometheus의 **조립(Assembly) 지점**이다. 모든 핵심 컴포넌트를 생성하고 `oklog/run.Group`을 통해 병렬 실행한다.

```
main() 흐름:
  1. kingpin/v2로 CLI 플래그 파싱
  2. config.LoadFile()로 YAML 설정 로드
  3. 컴포넌트 생성:
     - tsdb.Open() → 로컬 TSDB
     - storage.NewFanout() → 로컬 + 리모트 스토리지 통합
     - promql.NewEngine() → PromQL 엔진
     - scrape.NewManager() → 스크래프 매니저
     - rules.NewManager() → 규칙 평가 매니저
     - notifier.NewManager() → Alertmanager 알림 매니저
     - discovery.NewManager() → 서비스 디스커버리 매니저
     - web.New() → HTTP 서버 + API
  4. run.Group에 12개 Actor 등록 (g.Add)
  5. g.Run() → 모든 컴포넌트 병렬 실행, 하나라도 실패하면 전체 종료
```

`run.Group`에 등록되는 주요 Actor들:

| Actor | 역할 |
|-------|------|
| 시그널 핸들러 | SIGTERM/SIGINT 수신 시 graceful shutdown |
| 설정 리로더 | SIGHUP 또는 /-/reload API로 설정 재적용 |
| 디스커버리 매니저 | 서비스 디스커버리 실행 |
| 스크래프 매니저 | 타겟 스크래핑 루프 |
| 규칙 매니저 | 규칙 평가 루프 |
| 노티파이어 | Alertmanager 알림 발송 |
| TSDB | 로컬 저장소 라이프사이클 관리 |
| 웹 서버 | HTTP API + UI 서빙 |
| Remote Write/Read | 원격 저장소 연동 |

#### cmd/promtool/ (CLI 도구)

| 파일 | 역할 |
|------|------|
| `main.go` (1,427줄) | 서브커맨드 라우팅 (kingpin 기반) |
| `query.go` | 인스턴트/레인지 쿼리, 메타데이터 조회 |
| `rules.go` | 규칙 파일 검증 |
| `sd.go` | 서비스 디스커버리 디버깅 |
| `tsdb.go` | TSDB 벤치마크, 덤프, 복구 |
| `unittest.go` | 규칙 유닛 테스트 실행 |
| `backfill.go` | OpenMetrics 데이터 → TSDB 블록 변환 |
| `analyze.go` | TSDB 블록 분석 (카디널리티, 크기) |
| `debug.go` | pprof, 메트릭 등 디버그 정보 수집 |
| `archive.go` | TSDB 블록 아카이브 |
| `metrics.go` | 메트릭 포맷 검증 |

---

### 2.2 config/ — 설정 파싱

| 파일 | 줄수 | 역할 |
|------|------|------|
| `config.go` | 1,725 | `Config` 구조체 정의, YAML 파싱, 유효성 검증 |
| `reload.go` | - | 설정 리로드 헬퍼 |

핵심 구조체 계층:

```
Config
├── GlobalConfig          # 전역 설정 (scrape_interval, evaluation_interval 등)
├── RuntimeConfig         # 런타임 설정
├── []AlertingConfig      # Alertmanager 연결 설정
├── []ScrapeConfig        # 스크래프 잡 설정
├── []RemoteWriteConfig   # Remote Write 엔드포인트
├── []RemoteReadConfig    # Remote Read 엔드포인트
├── []RuleFiles           # 규칙 파일 경로 (glob 패턴)
├── TracingConfig         # OpenTelemetry 트레이싱 설정
└── StorageConfig         # TSDB 로컬 저장소 설정
```

설정 파일은 `go.yaml.in/yaml/v2`로 파싱되며, 각 필드에 YAML 태그와 커스텀 `UnmarshalYAML`이 정의되어 있다.

---

### 2.3 discovery/ — 서비스 디스커버리

```
discovery/
├── manager.go            # SD 매니저 (디스커버리 프로바이더 라이프사이클 관리)
├── discovery.go          # Discoverer 인터페이스 정의
├── registry.go           # 프로바이더 레지스트리 (초기화 시 등록)
├── metrics.go            # SD 관련 메트릭
├── util.go               # 유틸리티
├── refresh/              # 주기적 새로고침 베이스 디스커버리
├── targetgroup/          # TargetGroup 정의
├── install/              # 전체 SD 등록 (레거시)
├── kubernetes/           # Kubernetes SD (Pod, Service, Endpoint, Node, Ingress)
├── dns/                  # DNS SD (SRV, A, AAAA, MX 레코드)
├── consul/               # Consul SD
├── aws/                  # AWS EC2/ECS/Lightsail SD
├── azure/                # Azure VM SD
├── gce/                  # Google Compute Engine SD
├── digitalocean/         # DigitalOcean SD
├── hetzner/              # Hetzner Cloud SD
├── file/                 # File SD (JSON/YAML 파일 감시)
├── http/                 # HTTP SD (HTTP 엔드포인트 폴링)
├── openstack/            # OpenStack SD
├── eureka/               # Netflix Eureka SD
├── marathon/             # Marathon SD
├── moby/                 # Docker/Moby SD
├── nomad/                # HashiCorp Nomad SD
├── linode/               # Linode SD
├── ionos/                # IONOS Cloud SD
├── ovhcloud/             # OVH Cloud SD
├── puppetdb/             # PuppetDB SD
├── scaleway/             # Scaleway SD
├── stackit/              # STACKIT SD
├── triton/               # Triton SD
├── uyuni/                # Uyuni SD
├── vultr/                # Vultr SD
├── xds/                  # xDS (Envoy) SD
└── zookeeper/            # ZooKeeper SD
```

`discovery.Manager`가 모든 프로바이더를 관리한다. 각 프로바이더는 `Discoverer` 인터페이스를 구현하며, `targetgroup.Group` 채널을 통해 타겟 목록을 전달한다.

**빌드 태그 기반 선택적 포함:**

SD 프로바이더는 `plugins/` 디렉토리의 빌드 태그로 선택적으로 포함된다:

```go
// plugins/plugin_kubernetes.go
//go:build !remove_all_sd || enable_kubernetes_sd
package plugins
import _ "github.com/prometheus/prometheus/discovery/kubernetes"
```

- `remove_all_sd` 태그: 모든 SD 프로바이더 제거 (file, http 제외)
- `enable_<name>_sd` 태그: 특정 프로바이더만 개별 활성화
- `plugins/minimum.go`: file SD와 http SD는 항상 포함 (빌드 태그 없음)

이 설계로 임베디드 환경이나 특수 용도 빌드에서 바이너리 크기를 크게 줄일 수 있다.

---

### 2.4 model/ — 데이터 모델

```
model/
├── labels/       # 레이블 셋 (정렬된 name=value 쌍, 해시, 비교, Builder)
├── textparse/    # 텍스트 파서 (Prometheus, OpenMetrics, ProtoBuf 노출 포맷)
├── relabel/      # 레이블 릴레이블링 규칙 적용 엔진
├── histogram/    # 네이티브 히스토그램 데이터 모델
├── exemplar/     # Exemplar (트레이스 연결용 샘플)
├── metadata/     # 메트릭 메타데이터 (HELP, TYPE)
├── timestamp/    # 밀리초 타임스탬프 유틸리티
├── value/       # 특수 값 (stale marker, histogram 타입)
└── rulefmt/      # 규칙 파일 포맷 (YAML 구조체)
```

`labels.Labels`는 Prometheus에서 가장 빈번하게 사용되는 타입이다. 시계열 식별자 역할을 하며, 정렬된 `__name__=value` 쌍의 불변 리스트로 구현되어 있다.

---

### 2.5 promql/ — PromQL 엔진

```
promql/
├── engine.go     (4,610줄)  # PromQL 실행 엔진 (쿼리 계획, 평가, 최적화)
├── functions.go  (2,382줄)  # 내장 함수 구현 (rate, sum, avg, histogram_quantile 등)
├── value.go                  # 쿼리 결과 타입 (Scalar, Vector, Matrix, String)
├── quantile.go               # 분위수 계산
├── query_logger.go           # 쿼리 로깅
├── parser/                   # PromQL 파서 (렉서 + YACC)
│   ├── lex.go                #   렉서 (토큰 분리)
│   ├── parse.go              #   파서 (토큰 → AST)
│   ├── ast.go                #   AST 노드 정의
│   ├── generated_parser.y    #   YACC 문법 정의
│   ├── generated_parser.y.go #   YACC 생성 코드
│   ├── functions.go          #   함수 시그니처 정의
│   ├── value.go              #   파서 값 타입
│   ├── printer.go            #   AST → 문자열 변환
│   ├── prettier.go           #   AST 포맷팅
│   └── posrange/             #   소스 위치 추적
└── promqltest/               # PromQL 테스트 프레임워크
```

`engine.go`는 가장 큰 단일 파일(4,610줄)로, PromQL의 전체 평가 로직을 담당한다:

```
쿼리 실행 흐름:
  1. Parser: PromQL 문자열 → AST (parser/parse.go)
  2. Planner: AST 분석, 실행 계획 최적화 (engine.go)
  3. Evaluator: AST 노드별 재귀 평가 (engine.go)
     - VectorSelector → Storage에서 시계열 조회
     - MatrixSelector → 범위 벡터 조회
     - Call → 내장 함수 호출 (functions.go)
     - BinaryExpr → 벡터 연산 (매칭 + 연산)
     - AggregateExpr → 집계 (sum, avg, count 등)
  4. Result: Vector, Matrix, Scalar 타입으로 반환
```

---

### 2.6 scrape/ — 메트릭 스크래핑

| 파일 | 줄수 | 역할 |
|------|------|------|
| `manager.go` | - | 스크래프 매니저 (풀별 라이프사이클 관리) |
| `scrape.go` | 2,271 | 스크래프 루프 (HTTP 요청 → 파싱 → 저장) |
| `target.go` | - | 타겟 정의 (레이블, 상태, 마지막 스크래프 결과) |
| `metrics.go` | - | 스크래프 관련 Prometheus 메트릭 |
| `clientprotobuf.go` | - | Protobuf 노출 포맷 클라이언트 |

`scrape.go`의 스크래프 루프는 Prometheus의 핵심 데이터 수집 경로이다:

```
스크래프 루프:
  ticker(scrape_interval) → HTTP GET /metrics → textparse 파싱
    → relabel 적용 → storage.Appender로 샘플 추가
    → 타겟 상태/메트릭 업데이트
```

---

### 2.7 storage/ — 저장소 인터페이스

```
storage/
├── interface.go          (553줄)   # 핵심 인터페이스 (Queryable, Appendable, Storage)
├── interface_append.go             # Appender 인터페이스 상세
├── fanout.go                       # FanoutStorage (로컬 + 리모트 통합)
├── merge.go                        # 여러 저장소 결과 병합
├── buffer.go                       # 샘플 버퍼링
├── lazy.go                         # 지연 초기화 래퍼
├── secondary.go                    # 보조 저장소 (에러 무시)
├── series.go                       # Series 유틸리티
├── noop.go                         # No-op 구현 (테스트용)
├── generic.go                      # 제네릭 유틸리티
└── remote/                         # 원격 저장소 구현
    ├── storage.go                  #   Remote Storage 통합
    ├── write.go                    #   Remote Write 클라이언트
    ├── write_handler.go            #   Remote Write 수신 핸들러
    ├── write_otlp_handler.go       #   OTLP Write 핸들러
    ├── read.go                     #   Remote Read 클라이언트
    ├── read_handler.go             #   Remote Read 핸들러
    ├── queue_manager.go  (2,314줄) #   Write 큐 매니저 (배치, 재시도, 샤딩)
    ├── client.go                   #   HTTP 클라이언트 (인증, 압축)
    ├── codec.go                    #   Protobuf 인코딩/디코딩
    ├── intern.go                   #   문자열 인터닝 (메모리 최적화)
    ├── ewma.go                     #   지수가중이동평균 (처리량 추정)
    ├── metadata_watcher.go         #   메타데이터 감시
    ├── max_timestamp.go            #   최대 타임스탬프 추적
    ├── chunked.go                  #   청크 기반 전송
    ├── dial_context.go             #   네트워크 연결 관리
    ├── stats.go                    #   전송 통계
    ├── azuread/                    #   Azure AD 인증
    ├── googleiam/                  #   Google IAM 인증
    └── otlptranslator/             #   OTLP → Prometheus 변환
```

핵심 인터페이스 계층:

```
Storage (최상위)
├── Queryable          → Querier 생성
│   └── Querier        → Select(matchers) → SeriesSet
├── Appendable         → Appender 생성
│   └── Appender       → Append(ref, labels, t, v) → 샘플 추가
├── ExemplarQueryable  → Exemplar 조회
└── ChunkQueryable     → 청크 단위 조회 (최적화)
```

`FanoutStorage`는 로컬 TSDB와 Remote Storage를 하나의 `Storage` 인터페이스 뒤에 통합한다. 쓰기는 모두에게 팬아웃, 읽기는 로컬 우선 + 리모트 병합.

---

### 2.8 tsdb/ — 시계열 데이터베이스

```
tsdb/
├── db.go           (2,624줄)   # DB 최상위 (Open, Compact, Querier, Appender)
├── head.go         (2,674줄)   # Head Block (인메모리 활성 블록)
├── head_append.go              # Head에 샘플 추가 로직
├── head_append_v2.go           # V2 Append (최적화 경로)
├── head_read.go                # Head에서 읽기
├── head_wal.go                 # WAL 연동
├── head_dedupelabels.go        # 레이블 중복 제거
├── compact.go      (939줄)     # 컴팩션 (Head → Block, Block 병합)
├── block.go                    # 영구 블록 (디스크 기반, 불변)
├── blockwriter.go              # 블록 쓰기
├── querier.go                  # TSDB 쿼리 구현
├── isolation.go                # 트랜잭션 격리 (MVCC)
├── repair.go                   # 손상 복구
├── ooo_head.go                 # Out-of-Order 샘플 처리 (Head)
├── ooo_head_read.go            # OOO 읽기
├── ooo_isolation.go            # OOO 격리
├── exemplar.go                 # Exemplar 저장소
├── agent/                      # Agent 모드 (WAL만 사용, 블록 생성 안 함)
│
├── index/                      # 역인덱스
│   ├── index.go                #   인덱스 리더/라이터 (레이블 → 포스팅 리스트)
│   ├── postings.go             #   포스팅 리스트 (시계열 ID 집합 연산)
│   └── postingsstats.go        #   포스팅 통계
│
├── chunkenc/                   # 청크 인코딩
│   ├── chunk.go                #   Chunk 인터페이스
│   ├── xor.go                  #   XOR 인코딩 (float64, Gorilla 압축)
│   ├── histogram.go            #   히스토그램 청크 인코딩
│   ├── float_histogram.go      #   실수 히스토그램 인코딩
│   └── varbit.go               #   가변 비트 인코딩 유틸
│
├── chunks/                     # 청크 파일 관리
│   ├── chunks.go               #   청크 리더/라이터 (디스크 I/O)
│   ├── head_chunks.go          #   Head 청크 (메모리 매핑)
│   ├── queue.go                #   청크 쓰기 큐
│   └── samples.go              #   샘플 유틸리티
│
├── wlog/                       # Write-Ahead Log
│   ├── wlog.go                 #   WAL 구현 (세그먼트 기반 순차 쓰기)
│   ├── reader.go               #   WAL 리더
│   ├── live_reader.go          #   라이브 WAL 리더 (tailing)
│   ├── watcher.go              #   WAL 감시 (Remote Write용)
│   └── checkpoint.go           #   WAL 체크포인트
│
├── record/                     # WAL 레코드 인코딩
├── encoding/                   # 인코딩 유틸리티
├── fileutil/                   # 파일 시스템 유틸리티
├── tombstones/                 # 삭제 마커
├── tsdbutil/                   # TSDB 유틸리티
├── goversion/                  # Go 버전 호환성
└── docs/                       # TSDB 내부 문서
```

TSDB 계층 구조:

```
┌─────────────────────────────────────────────────────────┐
│                        DB                                │
│  ┌──────────┐  ┌─────────┐  ┌─────────┐  ┌─────────┐  │
│  │   Head   │  │ Block 1 │  │ Block 2 │  │ Block N │  │
│  │ (인메모리) │  │ (디스크)  │  │ (디스크)  │  │ (디스크)  │  │
│  │          │  │         │  │         │  │         │  │
│  │ ┌──────┐ │  │ ┌─────┐ │  │ ┌─────┐ │  │ ┌─────┐ │  │
│  │ │ WAL  │ │  │ │index│ │  │ │index│ │  │ │index│ │  │
│  │ │      │ │  │ │chunk│ │  │ │chunk│ │  │ │chunk│ │  │
│  │ │chunks│ │  │ │meta │ │  │ │meta │ │  │ │meta │ │  │
│  │ └──────┘ │  │ └─────┘ │  │ └─────┘ │  │ └─────┘ │  │
│  └──────────┘  └─────────┘  └─────────┘  └─────────┘  │
│       ↓ 컴팩션                                          │
│  Head → Block (2시간 블록 생성) → Block 병합 (시간 범위 확장) │
└─────────────────────────────────────────────────────────┘
```

---

### 2.9 rules/ — 규칙 평가

| 파일 | 줄수 | 역할 |
|------|------|------|
| `manager.go` | 646 | 규칙 그룹 매니저 (주기적 평가 스케줄링) |
| `group.go` | - | 규칙 그룹 (evaluation_interval마다 평가) |
| `alerting.go` | - | 알림 규칙 (PromQL 조건 → Alert 상태 머신) |
| `recording.go` | - | 레코딩 규칙 (PromQL → 새 시계열 저장) |
| `rule.go` | - | Rule 인터페이스 정의 |
| `origin.go` | - | 규칙 원본 추적 |

---

### 2.10 notifier/ — 알림 발송

| 파일 | 역할 |
|------|------|
| `manager.go` (326줄) | Alertmanager 매니저 (엔드포인트 관리) |
| `alertmanager.go` | Alertmanager 클라이언트 |
| `alertmanagerset.go` | Alertmanager 셋 (여러 인스턴스 관리) |
| `sendloop.go` | 알림 전송 루프 (배치, 재시도) |
| `alert.go` | Alert 데이터 모델 |
| `metric.go` | 알림 관련 메트릭 |

---

### 2.11 web/ — HTTP 서버 + API

```
web/
├── web.go         (961줄)    # HTTP 서버 (라우팅, 미들웨어, lifecycle API)
├── federate.go               # Federation 엔드포인트 (/federate)
├── api/
│   ├── v1/
│   │   ├── api.go (2,321줄)  # REST API v1 (쿼리, 메타데이터, 타겟, 규칙 등)
│   │   ├── openapi.go        # OpenAPI 스펙 검증
│   │   └── translate_ast.go  # AST 변환 (API 응답용)
│   └── testhelpers/          # API 테스트 헬퍼
└── ui/                       # React UI (빌드 결과물)
```

주요 API 엔드포인트:

| 경로 | 메서드 | 역할 |
|------|--------|------|
| `/api/v1/query` | GET/POST | 인스턴트 쿼리 |
| `/api/v1/query_range` | GET/POST | 레인지 쿼리 |
| `/api/v1/series` | GET/POST | 시계열 메타데이터 조회 |
| `/api/v1/labels` | GET/POST | 레이블 이름 목록 |
| `/api/v1/label/{name}/values` | GET | 레이블 값 목록 |
| `/api/v1/targets` | GET | 스크래프 타겟 목록 |
| `/api/v1/rules` | GET | 규칙 목록 |
| `/api/v1/alerts` | GET | 활성 알림 목록 |
| `/api/v1/metadata` | GET | 메트릭 메타데이터 |
| `/api/v1/write` | POST | Remote Write 수신 |
| `/api/v1/read` | POST | Remote Read |
| `/federate` | GET | Federation (PromQL 기반 메트릭 노출) |
| `/-/reload` | POST | 설정 리로드 |
| `/-/healthy` | GET | 헬스 체크 |
| `/-/ready` | GET | 준비 상태 |

---

### 2.12 prompb/ — Protobuf 정의

| 파일 | 역할 |
|------|------|
| `remote.proto` | Remote Write/Read 프로토콜 (WriteRequest, ReadRequest 등) |
| `types.proto` | 공통 타입 (TimeSeries, Sample, Label, Histogram 등) |
| `remote.pb.go` | 생성된 Go 코드 |
| `types.pb.go` | 생성된 Go 코드 |
| `codec.go` | 커스텀 인코딩/디코딩 |
| `custom.go` | 커스텀 메서드 |
| `io/` | Protobuf I/O 유틸리티 |
| `rwcommon/` | Remote Write 공통 코드 |
| `buf.gen.yaml` | buf 코드 생성 설정 |

---

### 2.13 util/ — 유틸리티 패키지

주요 유틸리티:

| 패키지 | 역할 |
|--------|------|
| `pool/` | 바이트 슬라이스 풀 (메모리 재사용) |
| `gate/` | 동시성 게이트 (쿼리 병렬도 제한) |
| `logging/` | 구조화 로깅 유틸리티 |
| `stats/` | 쿼리 통계 |
| `annotations/` | 쿼리 경고/정보 어노테이션 |
| `compression/` | 압축 유틸리티 |
| `convertnhcb/` | 네이티브 히스토그램 변환 |
| `features/` | 피처 플래그 |
| `fmtutil/` | 포맷 유틸리티 |
| `kahansum/` | Kahan 합산 (부동소수점 정밀도 보존) |
| `netconnlimit/` | 네트워크 연결 제한 |
| `notifications/` | 알림 유틸리티 |
| `osutil/` | OS 유틸리티 |
| `runtime/` | 런타임 유틸리티 (GOMAXPROCS, 메모리) |
| `strutil/` | 문자열 유틸리티 |
| `zeropool/` | Zero-value 풀 |
| `almost/` | 근사 비교 |
| `teststorage/` | 테스트용 저장소 |
| `testutil/` | 테스트 유틸리티 |
| `testwal/` | 테스트용 WAL |
| `treecache/` | ZooKeeper TreeCache |
| `documentcli/` | CLI 문서 생성 |
| `fuzzing/` | 퍼징 유틸리티 |
| `httputil/` | HTTP 유틸리티 |
| `jsonutil/` | JSON 유틸리티 |
| `junitxml/` | JUnit XML 출력 |
| `namevalidationutil/` | 메트릭 이름 검증 |
| `runutil/` | 실행 유틸리티 |

---

## 3. 빌드 시스템

### 3.1 Go 모듈

```
모듈: github.com/prometheus/prometheus
Go 버전: 1.25.0 (go.mod) / 1.26 (.promu.yml 빌드)
```

### 3.2 promu 빌드 도구

`.promu.yml`에 정의된 빌드 설정:

```yaml
build:
  binaries:
    - name: prometheus        # cmd/prometheus → prometheus 바이너리
      path: ./cmd/prometheus
    - name: promtool          # cmd/promtool → promtool 바이너리
      path: ./cmd/promtool
  tags:
    all: [netgo, builtinassets]     # 모든 플랫폼
    windows: [builtinassets]        # Windows
  ldflags: |
    -X .../version.Version={{.Version}}
    -X .../version.Revision={{.Revision}}
    ...
```

promu는 Prometheus 프로젝트 전용 빌드 도구로, 크로스 컴파일과 릴리스 패키징을 자동화한다.

### 3.3 Makefile 타겟

| 타겟 | 역할 |
|------|------|
| `build` | promu로 바이너리 빌드 |
| `test` | 전체 테스트 실행 |
| `assets` | React UI 빌드 + 임베딩 |
| `format` | 코드 포맷팅 (gofmt, goimports) |
| `lint` | golangci-lint 실행 |
| `docker` | Docker 이미지 빌드 |
| `tarball` | 릴리스 아카이브 생성 |

### 3.4 빌드 태그

| 태그 | 효과 |
|------|------|
| `netgo` | 순수 Go 네트워크 스택 사용 (CGO 불필요) |
| `builtinassets` | UI 정적 파일을 바이너리에 임베딩 |
| `remove_all_sd` | 모든 SD 프로바이더 제거 (file, http 제외) |
| `enable_<name>_sd` | `remove_all_sd`와 함께 사용, 특정 SD만 활성화 |

### 3.5 Docker 빌드

```
지원 아키텍처: amd64, armv7, arm64, ppc64le, riscv64, s390x
Dockerfile 변형: 기본, distroless (riscv64 제외)
레지스트리: Docker Hub, Quay.io (riscv64 제외)
```

---

## 4. 핵심 의존성

### 4.1 런타임 핵심 의존성

| 라이브러리 | 버전 | 역할 |
|-----------|------|------|
| `oklog/run` | v1.2.0 | 병렬 컴포넌트 실행 그룹 (`run.Group`) |
| `alecthomas/kingpin/v2` | v2.4.0 | CLI 플래그 파싱 |
| `prometheus/client_golang` | v1.23.2 | 자체 메트릭 계측 (self-monitoring) |
| `go.yaml.in/yaml/v2` | v2.4.3 | YAML 설정 파싱 |
| `grafana/regexp` | v0.0.0-... | 최적화된 정규식 (표준 라이브러리 대체) |
| `cespare/xxhash/v2` | v2.3.0 | 고성능 해시 (레이블 해싱) |
| `klauspost/compress` | v1.18.4 | 압축 (snappy, zstd) |
| `edsrzf/mmap-go` | v1.2.0 | 메모리 매핑 파일 I/O (TSDB 블록) |
| `gogo/protobuf` | v1.3.2 | Protobuf 직렬화 (Remote Write/Read) |
| `google.golang.org/protobuf` | v1.36.11 | 신규 Protobuf 직렬화 |
| `fsnotify/fsnotify` | v1.9.0 | 파일 시스템 이벤트 감시 (File SD) |
| `json-iterator/go` | v1.1.12 | 고성능 JSON 직렬화 |
| `bboreham/go-loser` | v0.0.0-... | Loser Tree (정렬 병합, TSDB 쿼리) |
| `dennwc/varint` | v1.0.0 | 가변 길이 정수 인코딩 |

### 4.2 서비스 디스커버리 의존성

| 라이브러리 | 역할 |
|-----------|------|
| `k8s.io/client-go` v0.35.1 | Kubernetes API 클라이언트 |
| `hashicorp/consul/api` v1.32.1 | Consul SD |
| `aws/aws-sdk-go-v2` v1.41.2 | AWS SD (EC2, ECS, Lightsail 등) |
| `Azure/azure-sdk-for-go` | Azure SD |
| `google.golang.org/api` v0.267.0 | GCE SD |
| `digitalocean/godo` v1.175.0 | DigitalOcean SD |
| `hetznercloud/hcloud-go/v2` v2.36.0 | Hetzner SD |
| `docker/docker` v28.5.2 | Docker/Moby SD |
| `miekg/dns` v1.1.72 | DNS SD |
| `go-zookeeper/zk` v1.0.4 | ZooKeeper SD |
| `hashicorp/nomad/api` | Nomad SD |
| `gophercloud/gophercloud/v2` v2.10.0 | OpenStack SD |

### 4.3 관찰성 의존성

| 라이브러리 | 역할 |
|-----------|------|
| `go.opentelemetry.io/otel` v1.40.0 | OpenTelemetry 트레이싱 |
| `go.opentelemetry.io/otel/exporters/otlp/otlptrace` | OTLP 트레이스 내보내기 |
| `prometheus/common` v0.67.5 | Prometheus 공통 유틸리티, 로깅 |
| `prometheus/exporter-toolkit` v0.15.1 | TLS, 인증 등 HTTP 서버 도구 |

### 4.4 OpenTelemetry 통합

| 라이브러리 | 역할 |
|-----------|------|
| `go.opentelemetry.io/collector/pdata` v1.51.0 | OTLP 데이터 모델 |
| `go.opentelemetry.io/collector/consumer` v1.51.0 | OTel 컬렉터 소비자 인터페이스 |
| `open-telemetry/...deltatocumulativeprocessor` v0.145.0 | Delta → Cumulative 변환 |

---

## 5. 패키지 의존성 그래프

```
                        ┌──────────────┐
                        │ cmd/prometheus│
                        │   (main.go)  │
                        └──────┬───────┘
                               │ 조립(assembly)
          ┌────────────┬───────┼───────┬────────────┬──────────┐
          ▼            ▼       ▼       ▼            ▼          ▼
    ┌──────────┐ ┌─────────┐ ┌────┐ ┌──────┐ ┌──────────┐ ┌────────┐
    │ discovery │ │ scrape  │ │web │ │rules │ │ notifier │ │promql  │
    │          │ │         │ │    │ │      │ │          │ │        │
    └────┬─────┘ └────┬────┘ └──┬─┘ └──┬───┘ └──────────┘ └───┬────┘
         │            │        │      │                        │
         │            │        │      ▼                        │
         │            │        │  ┌────────┐                   │
         │            │        └─→│api/v1  │←──────────────────┘
         │            │           └────────┘
         │            ▼                │
         │     ┌────────────┐          │
         │     │   config   │          │
         │     └────────────┘          │
         │            │                │
         ▼            ▼                ▼
    ┌─────────────────────────────────────────┐
    │              storage (인터페이스)          │
    │  ┌────────────────┐  ┌───────────────┐  │
    │  │   fanout.go    │  │   remote/     │  │
    │  └───────┬────────┘  └───────┬───────┘  │
    │          │                   │           │
    └──────────┼───────────────────┼───────────┘
               ▼                   │
    ┌─────────────────────┐        │
    │        tsdb          │        │
    │  ┌──────┐ ┌───────┐ │        │
    │  │ head │ │ block  │ │        │
    │  │      │ │        │ │        │
    │  │ WAL  │ │ index  │ │        │
    │  │chunk │ │chunkenc│ │        │
    │  └──────┘ └───────┘ │        │
    └─────────────────────┘        │
                                   ▼
                          ┌─────────────────┐
                          │    prompb        │
                          │ (protobuf 정의)   │
                          └─────────────────┘

    ※ 횡단 의존성:
       model/labels  ← 거의 모든 패키지가 의존
       model/textparse ← scrape
       model/relabel  ← scrape, discovery
       model/histogram ← tsdb, promql, storage
```

---

## 6. 코드 규모 요약

### 6.1 핵심 파일 크기 (줄수 기준)

| 파일 | 줄수 | 비고 |
|------|------|------|
| `promql/engine.go` | 4,610 | 가장 큰 파일, PromQL 평가 엔진 |
| `tsdb/head.go` | 2,674 | Head Block (인메모리 TSDB) |
| `tsdb/db.go` | 2,624 | TSDB 최상위 |
| `promql/functions.go` | 2,382 | PromQL 내장 함수 |
| `web/api/v1/api.go` | 2,321 | REST API v1 |
| `storage/remote/queue_manager.go` | 2,314 | Remote Write 큐 |
| `scrape/scrape.go` | 2,271 | 스크래프 루프 |
| `cmd/prometheus/main.go` | 2,151 | 서버 진입점 |
| `config/config.go` | 1,725 | 설정 파싱 |
| `cmd/promtool/main.go` | 1,427 | CLI 도구 진입점 |
| `web/web.go` | 961 | HTTP 서버 |
| `tsdb/compact.go` | 939 | 컴팩션 |
| `rules/manager.go` | 646 | 규칙 매니저 |
| `storage/interface.go` | 553 | 저장소 인터페이스 |
| `discovery/manager.go` | 533 | SD 매니저 |
| `notifier/manager.go` | 326 | 알림 매니저 |

### 6.2 디렉토리별 특성

| 디렉토리 | 특성 | 복잡도 |
|----------|------|--------|
| `tsdb/` | 파일 I/O, mmap, 동시성, 압축 알고리즘 | 매우 높음 |
| `promql/` | 파서(YACC), 재귀 평가, 벡터 연산 | 높음 |
| `scrape/` | HTTP 클라이언트, 텍스트 파싱, 동시성 | 중간 |
| `storage/remote/` | 네트워크, 배치, 재시도, 샤딩 | 높음 |
| `discovery/` | 외부 API 통합, 20+ 프로바이더 | 넓지만 개별은 단순 |
| `config/` | YAML 파싱, 유효성 검증 | 중간 |
| `rules/` | 상태 머신, 주기적 평가 | 중간 |
| `web/` | HTTP 라우팅, API 핸들러 | 중간 |
| `notifier/` | HTTP 클라이언트, 배치 전송 | 낮음 |

---

## 7. 설계 원칙

### 7.1 인터페이스 기반 분리

Prometheus는 `storage.Storage` 인터페이스를 중심으로 깔끔하게 분리되어 있다. PromQL 엔진, 스크래프, 규칙 평가 모두 `Storage` 인터페이스에만 의존하며, 실제 구현(TSDB, Remote)은 알지 못한다.

### 7.2 Actor 모델 (`oklog/run`)

`cmd/prometheus/main.go`에서 `run.Group`으로 모든 컴포넌트를 독립 Actor로 실행한다. 각 Actor는 `execute`/`interrupt` 함수 쌍을 가지며, 하나라도 종료하면 모두 graceful shutdown된다.

### 7.3 플러그인 패턴 (빌드 태그 + import)

SD 프로바이더는 Go의 `init()` 함수 + 빌드 태그 조합으로 플러그인처럼 동작한다. `plugins/` 디렉토리의 각 파일이 빌드 태그로 조건부 임포트되며, 임포트 시 `init()`에서 레지스트리에 자동 등록된다.

### 7.4 Manager 패턴

대부분의 서브시스템이 Manager 패턴을 따른다:
- `scrape.Manager` — 스크래프 풀 관리
- `rules.Manager` — 규칙 그룹 관리
- `discovery.Manager` — SD 프로바이더 관리
- `notifier.Manager` — Alertmanager 연결 관리

각 Manager는 설정 리로드 메서드(`ApplyConfig`)를 노출하여, 런타임에 설정 변경이 가능하다.

### 7.5 모노리식 but 모듈화

Prometheus는 단일 바이너리(모노리식)지만, 내부는 명확한 패키지 경계로 모듈화되어 있다. 각 패키지는 인터페이스를 통해 느슨하게 결합되며, `cmd/prometheus/main.go`가 유일한 조립 지점이다.
