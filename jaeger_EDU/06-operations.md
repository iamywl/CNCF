# Jaeger 운영 가이드 (Operations)

## 개요

이 문서는 Jaeger를 프로덕션 환경에서 운영하기 위한 실무 가이드이다. 배포 모델 선택, YAML 기반 설정, 스토리지 백엔드 비교, 모니터링 체계, Elasticsearch 인덱스 관리, 트러블슈팅, 보안 설정을 다룬다. 모든 내용은 Jaeger v2 (OpenTelemetry Collector 기반) 아키텍처를 기준으로 한다.

---

## 1. 배포 모델 (Deployment Models)

Jaeger v2는 단일 바이너리(`jaeger`)로 배포되며, YAML 설정 파일에 따라 역할이 결정된다. OTel Collector의 파이프라인 모델(receivers -> processors -> exporters)을 그대로 따르기 때문에, 동일한 바이너리로 다양한 배포 토폴로지를 구성할 수 있다.

### 1.1 All-in-One 모드 (개발/테스트)

단일 프로세스에서 Collector, Query, Storage를 모두 실행한다. 기본 내장 설정 파일은 `cmd/jaeger/internal/all-in-one.yaml`에 정의되어 있다.

```
┌──────────────────────────────────────────────┐
│              All-in-One Process               │
│                                              │
│  ┌─────────┐  ┌───────┐  ┌──────────────┐   │
│  │Receivers│→ │Batch  │→ │Storage       │   │
│  │(OTLP,   │  │Process│  │Exporter      │   │
│  │ Jaeger, │  │       │  │(memory)      │   │
│  │ Zipkin) │  └───────┘  └──────────────┘   │
│  └─────────┘                                 │
│  ┌──────────────────────────────────────┐    │
│  │Extensions: jaeger_query, jaeger_mcp, │    │
│  │  remote_sampling, healthcheckv2,     │    │
│  │  expvar, zpages                      │    │
│  └──────────────────────────────────────┘    │
└──────────────────────────────────────────────┘
```

**주요 특징:**
- 메모리 스토리지 사용 (`max_traces: 100000`)
- 프로세스 재시작 시 데이터 소실
- 환경변수 `JAEGER_LISTEN_HOST`로 바인딩 주소 제어 (기본값: `localhost`)
- 모든 디버그 익스텐션(expvar, zpages) 활성화

**기본 포트 구성** (`all-in-one.yaml` 기준):

| 포트 | 프로토콜 | 용도 | 소스 참조 |
|------|---------|------|----------|
| 4317 | gRPC | OTLP 수신 | `receivers.otlp.protocols.grpc` |
| 4318 | HTTP | OTLP 수신 | `receivers.otlp.protocols.http` |
| 14250 | gRPC | Jaeger gRPC 수신 | `receivers.jaeger.protocols.grpc` |
| 14268 | HTTP | Jaeger Thrift HTTP 수신 | `receivers.jaeger.protocols.thrift_http` |
| 6831 | UDP | Jaeger Thrift Compact | `receivers.jaeger.protocols.thrift_compact` |
| 6832 | UDP | Jaeger Thrift Binary | `receivers.jaeger.protocols.thrift_binary` |
| 9411 | HTTP | Zipkin 호환 수신 | `receivers.zipkin` |
| 16686 | HTTP | Query UI / API | `ports.QueryHTTP` |
| 16685 | gRPC | Query gRPC API | `ports.QueryGRPC` |
| 16687 | HTTP | MCP 서버 | `ports.MCPHTTP` |
| 5778 | HTTP | Remote Sampling HTTP | `extensions.remote_sampling.http` |
| 5779 | gRPC | Remote Sampling gRPC | `extensions.remote_sampling.grpc` |
| 8888 | HTTP | Prometheus 메트릭 | `telemetry.metrics.readers` |
| 13133 | HTTP | 헬스체크 | `extensions.healthcheckv2.http` |
| 27777 | HTTP | expvar 디버그 | `extensions.expvar` |
| 27778 | HTTP | zpages 디버그 | `extensions.zpages` |
| 1777 | HTTP | pprof 프로파일링 | `extensions.pprof` (config.yaml) |

**실행:**
```bash
# 설정 파일 없이 실행하면 내장 all-in-one.yaml 사용
jaeger

# 명시적 설정 파일 지정
jaeger --config cmd/jaeger/config.yaml
```

### 1.2 프로덕션 모드 (Collector + Query 분리)

Collector와 Query를 별도 프로세스로 분리하고, 영구 스토리지(Elasticsearch, Cassandra 등)를 사용한다.

```
┌────────────────┐     ┌────────────────────────┐
│  Application   │     │    Jaeger Collector     │
│  (OTel SDK)    │────→│  config-elasticsearch   │
│                │OTLP │  .yaml                  │
└────────────────┘     │  receivers → batch →    │
                       │  jaeger_storage_exporter│
                       └──────────┬─────────────┘
                                  │ write
                       ┌──────────▼─────────────┐
                       │   Elasticsearch /       │
                       │   Cassandra / Badger    │
                       └──────────┬─────────────┘
                                  │ read
                       ┌──────────▼─────────────┐
                       │    Jaeger Query         │
                       │  config-query.yaml      │
                       │  jaeger_query extension │
                       │  → remote_storage(grpc) │
                       └────────────────────────┘
```

**Query 전용 설정** (`config-query.yaml`에서 확인):
```yaml
service:
  extensions: [jaeger_storage, jaeger_query, healthcheckv2]
  pipelines:
    traces:
      receivers: [nop]         # 수신 없음 (Query 전용)
      processors: [batch]
      exporters: [nop]         # 내보내기 없음
```

Query 서비스는 `nop` receiver/exporter를 사용하여 트레이스 수신 없이 조회만 수행한다. 스토리지 접근은 gRPC remote storage 또는 직접 백엔드 연결로 구성한다.

### 1.3 Kafka 파이프라인 (고처리량)

대규모 트래픽 환경에서는 Kafka를 버퍼로 사용하여 수집과 저장을 비동기로 분리한다. 이를 통해 스토리지 장애가 수집기에 영향을 주지 않고, 처리량을 독립적으로 스케일링할 수 있다.

```
┌──────────┐     ┌───────────────────┐     ┌─────────┐     ┌──────────────────┐     ┌─────────┐
│App (SDK) │────→│ Jaeger Collector  │────→│  Kafka  │────→│ Jaeger Ingester  │────→│ Storage │
│          │OTLP │ config-kafka-     │kafka│         │     │ config-kafka-    │     │         │
└──────────┘     │ collector.yaml    │     │         │     │ ingester.yaml    │     │         │
                 └───────────────────┘     └─────────┘     └──────────────────┘     └─────────┘
```

**Collector 측 설정** (`config-kafka-collector.yaml`):
```yaml
exporters:
  kafka:
    brokers:
      - ${env:KAFKA_BROKER:-localhost:9092}
    traces:
      topic: ${env:KAFKA_TOPIC:-jaeger-spans}
      encoding: ${env:KAFKA_ENCODING:-otlp_proto}
```

**Ingester 측 설정** (`config-kafka-ingester.yaml`):
```yaml
receivers:
  kafka:
    brokers:
      - localhost:9092
    traces:
      topic: ${env:KAFKA_TOPIC:-jaeger-spans}
      encoding: ${env:KAFKA_ENCODING:-otlp_proto}
    initial_offset: earliest
```

Ingester는 Collector와 포트 충돌을 피하기 위해 별도 포트를 사용한다:
- 메트릭: `8889` (Collector는 `8888`)
- 헬스체크: `14133` (Collector는 `13133`)

### 1.4 Docker 배포

```bash
# All-in-One 실행
docker run -d --name jaeger \
  -p 16686:16686 \
  -p 4317:4317 \
  -p 4318:4318 \
  -e JAEGER_LISTEN_HOST=0.0.0.0 \
  jaegertracing/jaeger:latest
```

Docker 컨테이너 내부에서는 `JAEGER_LISTEN_HOST=0.0.0.0`을 설정해야 외부에서 접근 가능하다. 내장 설정 파일(`all-in-one.yaml`)의 기본값이 `localhost`이므로, 이 환경변수 없이는 컨테이너 외부에서 접속할 수 없다.

---

## 2. 설정 (Configuration)

### 2.1 YAML 기반 설정 구조

Jaeger v2는 OTel Collector 설정 형식을 따른다. 설정 파일은 다섯 개의 최상위 섹션으로 구성된다.

```yaml
# 전체 설정 구조
service:           # 파이프라인 구성, 익스텐션 목록, 텔레메트리 설정
extensions:        # 부가 기능 (jaeger_query, jaeger_storage, healthcheck 등)
receivers:         # 데이터 수신 (otlp, jaeger, zipkin, kafka)
processors:        # 데이터 처리 (batch, adaptive_sampling)
exporters:         # 데이터 내보내기 (jaeger_storage_exporter, kafka, prometheus)
connectors:        # 파이프라인 간 연결 (spanmetrics 등, 선택 사항)
```

**`service` 섹션 상세:**
```yaml
service:
  extensions: [jaeger_storage, jaeger_query, healthcheckv2]
  pipelines:
    traces:
      receivers: [otlp, jaeger, zipkin]
      processors: [batch, adaptive_sampling]
      exporters: [jaeger_storage_exporter]
  telemetry:
    resource:
      service.name: jaeger
    metrics:
      level: detailed       # none, basic, normal, detailed
      readers:
        - pull:
            exporter:
              prometheus:
                host: 0.0.0.0
                port: 8888
    logs:
      level: info            # debug, info, warn, error
```

### 2.2 환경변수 치환

설정 파일 내에서 `${env:변수명:-기본값}` 구문으로 환경변수를 참조할 수 있다. 이는 OTel Collector의 config provider 기능이다.

```yaml
# 실제 all-in-one.yaml에서 사용 중인 예시
extensions:
  healthcheckv2:
    http:
      endpoint: "${env:JAEGER_LISTEN_HOST:-localhost}:13133"

  expvar:
    endpoint: "${env:JAEGER_LISTEN_HOST:-localhost}:27777"

receivers:
  otlp:
    protocols:
      grpc:
        endpoint: "${env:JAEGER_LISTEN_HOST:-localhost}:4317"
```

**지원하는 Config Provider:**

| Provider | URI 형식 | 용도 |
|----------|---------|------|
| file | `file:./config.yaml` | 로컬 파일 시스템에서 설정 로드 |
| env | `${env:VAR_NAME}` | 환경변수 치환 |
| http/https | `http://server/config` | 원격 HTTP 서버에서 설정 로드 |
| yaml | `yaml:inline-config` | 인라인 YAML 구성 |

### 2.3 설정 파일 지정 및 병합

```bash
# 단일 설정 파일
jaeger --config config-elasticsearch.yaml

# 여러 설정 파일 병합 (나중 파일이 이전 파일을 override)
jaeger --config config.yaml --config config-override.yaml
```

### 2.4 Jaeger 전용 익스텐션 설정

**jaeger_storage** -- 스토리지 백엔드 정의:
```yaml
extensions:
  jaeger_storage:
    backends:
      primary_store:       # 이름은 자유롭게 지정
        elasticsearch:     # 백엔드 유형: memory, badger, cassandra, elasticsearch, opensearch, clickhouse, grpc
          server_urls:
            - http://localhost:9200
    metric_backends:       # SPM(Service Performance Monitoring) 전용 메트릭 백엔드
      metrics_store:
        prometheus:
          endpoint: http://prometheus:9090
```

**jaeger_query** -- 쿼리 서비스 설정:
```yaml
extensions:
  jaeger_query:
    storage:
      traces: primary_store         # 트레이스 조회용 스토리지
      traces_archive: archive_store # 아카이브 스토리지 (선택)
      metrics: metrics_store        # SPM 메트릭 (선택)
    ui:
      config_file: ./config-ui.json # UI 설정 파일 경로
    max_clock_skew_adjust: 0s       # 시계 보정 최대값 (0 = 비활성)
```

**remote_sampling** -- 샘플링 전략 제공:
```yaml
extensions:
  remote_sampling:
    # 파일 기반 전략
    file:
      path: ./sampling-strategies.json
      default_sampling_probability: 1
      reload_interval: 1s
    # 또는 적응형 샘플링
    # adaptive:
    #   sampling_store: some_store
    #   initial_sampling_probability: 0.1
    #   target_samples_per_second: 1.0
    http:
      endpoint: "${env:JAEGER_LISTEN_HOST:-localhost}:5778"
    grpc:
      endpoint: "${env:JAEGER_LISTEN_HOST:-localhost}:5779"
```

---

## 3. 스토리지 선택 가이드

### 3.1 스토리지 비교 테이블

| 항목 | Memory | Badger | Cassandra | Elasticsearch/OpenSearch | ClickHouse |
|------|--------|--------|-----------|------------------------|------------|
| 영속성 | 없음 | 있음 | 있음 | 있음 | 있음 |
| 분산 | 불가 | 단일 노드 | 수평 확장 | 수평 확장 | 수평 확장 |
| 검색 | 기본 | 기본 | 기본 | 전문 검색(Full-text) | 분석 쿼리 |
| 적합 환경 | 개발/테스트 | 소규모 단일 서버 | 대규모, 쓰기 집중 | 대규모, 검색 집중 | 대규모, 분석 집중 |
| 운영 복잡도 | 매우 낮음 | 낮음 | 높음 | 중간~높음 | 중간 |

### 3.2 Memory

```yaml
jaeger_storage:
  backends:
    some_storage:
      memory:
        max_traces: 100000    # 최대 보관 트레이스 수
```

- 프로세스 재시작 시 모든 데이터 소실
- `max_traces` 초과 시 오래된 트레이스부터 삭제
- 개발/테스트/데모 환경에서만 사용

### 3.3 Badger

```yaml
jaeger_storage:
  backends:
    some_store:
      badger:
        directories:
          keys: "/tmp/jaeger/"           # 키 데이터 디렉토리
          values: "/tmp/jaeger/"         # 값 데이터 디렉토리
        ephemeral: false                 # true이면 임시 디렉토리 사용
        ttl:
          spans: 48h                     # 스팬 TTL (예: 48시간)
        metrics_update_interval: ${env:BADGER_METRICS_UPDATE_INTERVAL:-10s}
```

- Go로 작성된 내장형 KV 스토어 (dgraph-io/badger)
- 단일 프로세스만 접근 가능 (파일 잠금)
- TTL 기반 자동 데이터 만료
- 프로덕션 소규모 배포(단일 서버)에 적합
- 아카이브 스토어를 별도 디렉토리로 분리 가능

### 3.4 Cassandra

```yaml
jaeger_storage:
  backends:
    some_storage:
      cassandra:
        schema:
          keyspace: "jaeger_v1_dc1"
          create: "${env:CASSANDRA_CREATE_SCHEMA:-true}"
        connection:
          servers: ["${env:CASSANDRA_CONTACT_POINTS:-127.0.0.1:9042}"]
          auth:
            basic:
              username: "cassandra"
              password: "cassandra"
          tls:
            insecure: true
```

- 수평 확장에 강함 (쓰기 처리량 선형 증가)
- Eventually Consistent -- 쓰기 직후 즉시 읽기 불가능할 수 있음
- 스키마 자동 생성 가능 (`create: true`)
- 아카이브용 별도 keyspace 지원 (예: `jaeger_v1_dc1_archive`)
- 적응형 샘플링(Adaptive Sampling) 지원 -- 샘플링 데이터 저장소로 사용 가능

### 3.5 Elasticsearch / OpenSearch

```yaml
jaeger_storage:
  backends:
    some_storage:
      elasticsearch:               # 또는 opensearch:
        server_urls:
          - http://localhost:9200
        indices:
          index_prefix: "jaeger-main"
          spans:
            date_layout: "2006-01-02"
            rollover_frequency: "day"
            shards: 5
            replicas: 1
          services:
            date_layout: "2006-01-02"
            rollover_frequency: "day"
            shards: 5
            replicas: 1
          dependencies:
            date_layout: "2006-01-02"
            rollover_frequency: "day"
            shards: 5
            replicas: 1
          sampling:
            date_layout: "2006-01-02"
            rollover_frequency: "day"
            shards: 5
            replicas: 1
```

- 전문 검색(Full-text Search) 지원 -- 태그, 로그, 서비스명으로 트레이스 검색
- Kibana/OpenSearch Dashboards와 통합 가능
- ILM(Index Lifecycle Management) 지원
- 인덱스 명명 규칙: `{prefix}-jaeger-span-{date}`, `{prefix}-jaeger-service-{date}`
- `custom_headers` 옵션으로 커스텀 HTTP 헤더 추가 가능 (AWS 프록시 등)
- SPM 메트릭 백엔드로도 사용 가능 (`metric_backends` 섹션)

### 3.6 ClickHouse

```yaml
jaeger_storage:
  backends:
    some-storage:
      clickhouse:
        addresses:
          - localhost:9000
        database: jaeger
        auth:
          basic:
            username: default
            password: password
        create_schema: true
```

- 열 지향(Column-Oriented) OLAP 데이터베이스
- 분석 쿼리에 뛰어난 성능
- 높은 압축률로 스토리지 비용 절감
- 스키마 자동 생성 지원

---

## 4. 모니터링 (Monitoring)

### 4.1 메트릭 엔드포인트

Jaeger는 OTel Collector의 텔레메트리 프레임워크를 통해 자체 메트릭을 노출한다.

| 엔드포인트 | 기본 포트 | 용도 | 설정 위치 |
|-----------|----------|------|----------|
| Prometheus metrics | 8888 | Prometheus 스크래핑용 메트릭 | `telemetry.metrics.readers` |
| expvar | 27777 | Go 런타임 변수 디버그 | `extensions.expvar` |
| zpages | 27778 | OTel 파이프라인 내부 디버그 | `extensions.zpages` |
| Health check | 13133 | 서비스 상태 확인 | `extensions.healthcheckv2.http` |
| pprof | 1777 | Go 프로파일링 | `extensions.pprof` |

**메트릭 레벨 설정:**
```yaml
telemetry:
  metrics:
    level: detailed    # none | basic | normal | detailed
```

- `none`: 메트릭 수집 비활성화
- `basic`: 기본 메트릭만 (수신/내보내기 카운터)
- `normal`: 표준 메트릭 (처리 시간 히스토그램 포함)
- `detailed`: 모든 메트릭 (레이턴시 분포, 큐 크기 등)

### 4.2 Prometheus 알림 규칙

Jaeger 프로젝트는 `monitoring/jaeger-mixin/` 디렉토리에 사전 정의된 Prometheus 알림 규칙을 제공한다.

**v1 알림 규칙** (`prometheus_alerts.yml`):

| 알림명 | 조건 | 심각도 |
|-------|------|--------|
| JaegerHTTPServerErrs | HTTP 서버 에러율 > 1% (15분 지속) | warning |
| JaegerRPCRequestsErrors | RPC HTTP 요청 에러율 > 1% | warning |
| JaegerClientSpansDropped | 클라이언트 스팬 드롭율 > 1% | warning |
| JaegerAgentSpansDropped | 에이전트 배치 실패율 > 1% | warning |
| JaegerCollectorDroppingSpans | Collector 스팬 드롭율 > 1% | warning |
| JaegerSamplingUpdateFailing | 샘플링 정책 업데이트 실패율 > 1% | warning |
| JaegerThrottlingUpdateFailing | 스로틀링 정책 업데이트 실패율 > 1% | warning |
| JaegerQueryReqsFailing | Query 요청 에러율 > 1% | warning |

**v2 알림 규칙** (`prometheus_alerts_v2.yml`) -- OTel Collector 기반:

| 알림명 | 조건 | 심각도 |
|-------|------|--------|
| OtelHttpServerErrors | HTTP 5xx 에러율 > 1% | warning |
| OtelExporterQueueFull | Exporter 큐 사용량 > 80% | warning |
| OtelHighMemoryUsage | RSS 메모리 > 100MB | warning |
| OtelHighCpuUsage | CPU 사용률 5분 평균 > 0.8초 | warning |
| OtelProcessorBatchHighCardinality | 배치 프로세서 메타데이터 카디널리티 > 1000 | warning |

### 4.3 Grafana 대시보드

`monitoring/jaeger-mixin/dashboard-for-grafana.json` 파일로 사전 구성된 Grafana 대시보드를 제공한다. Jsonnet 기반의 mixin 패턴을 사용하여 커스터마이즈할 수 있다.

```
monitoring/jaeger-mixin/
├── mixin.libsonnet           # 메인 mixin 정의
├── dashboards.libsonnet      # 대시보드 Jsonnet 정의
├── alerts.libsonnet          # 알림 Jsonnet 정의
├── dashboard-for-grafana.json # 빌드된 Grafana 대시보드 JSON
├── prometheus_alerts.yml     # v1 알림 규칙
└── prometheus_alerts_v2.yml  # v2 알림 규칙 (OTel 기반)
```

### 4.4 SPM (Service Performance Monitoring)

Jaeger는 spanmetrics connector를 통해 RED 메트릭(Rate, Error, Duration)을 자동 생성하고, 이를 Monitor 탭에서 시각화할 수 있다.

```yaml
# config-spm.yaml에서 확인한 설정
service:
  pipelines:
    traces:
      exporters: [jaeger_storage_exporter, spanmetrics]
    metrics/spanmetrics:
      receivers: [spanmetrics]
      exporters: [prometheus]

connectors:
  spanmetrics:
    metrics_flush_interval: ${env:SPANMETRICS_FLUSH_INTERVAL:-60s}

extensions:
  jaeger_query:
    storage:
      traces: some_storage
      metrics: some_metrics_storage    # 메트릭 백엔드 지정
  jaeger_storage:
    metric_backends:
      some_metrics_storage:
        prometheus:
          endpoint: http://prometheus:9090
          normalize_calls: true
          normalize_duration: true
```

---

## 5. Elasticsearch 운영

### 5.1 인덱스 명명 규칙

Jaeger는 날짜 기반 인덱스 전략을 사용한다.

| 인덱스 유형 | 패턴 | 예시 |
|------------|------|------|
| 스팬 | `{prefix}-jaeger-span-{date}` | `jaeger-main-jaeger-span-2026-03-06` |
| 서비스 | `{prefix}-jaeger-service-{date}` | `jaeger-main-jaeger-service-2026-03-06` |
| 의존성 | `{prefix}-jaeger-dependencies-{date}` | `jaeger-main-jaeger-dependencies-2026-03-06` |
| 샘플링 | `{prefix}-jaeger-sampling-{date}` | `jaeger-main-jaeger-sampling-2026-03-06` |

`date_layout`과 `rollover_frequency` 설정으로 인덱스 생성 주기를 조절한다:
- `date_layout: "2006-01-02"` -- Go 날짜 형식 (YYYY-MM-DD)
- `rollover_frequency: "day"` -- 일별 인덱스 생성

### 5.2 인덱스 클리너 (Index Cleaner)

`jaeger-es-index-cleaner` 도구는 지정된 일수보다 오래된 인덱스를 삭제한다.

```bash
# 사용법: jaeger-es-index-cleaner NUM_OF_DAYS http://HOSTNAME:PORT
jaeger-es-index-cleaner 7 http://localhost:9200
```

**소스코드 참조** (`cmd/es-index-cleaner/main.go`):
- 인자로 `NUM_OF_DAYS`와 ES URL을 받음
- `app.CalculateDeletionCutoff()` 함수로 삭제 기준 날짜 계산
- `IndexFilter`로 대상 인덱스를 필터링 후 일괄 삭제
- `--index-prefix` 플래그로 특정 prefix 인덱스만 삭제
- `--archive` 플래그로 아카이브 인덱스 삭제
- `--rollover` 플래그로 롤오버 인덱스 삭제
- TLS, Basic Auth 설정 지원

**Feature Gate** (`es.index.relativeTimeIndexDeletion`):
- Alpha 단계 (v2.5.0+)
- 활성화하면 현재 시각 기준으로 삭제 (기본값: 다음 날 자정 기준)

**CronJob 예시 (Kubernetes):**
```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: jaeger-es-index-cleaner
spec:
  schedule: "55 23 * * *"
  jobTemplate:
    spec:
      template:
        spec:
          containers:
          - name: jaeger-es-index-cleaner
            image: jaegertracing/jaeger-es-index-cleaner:latest
            args: ["7", "http://elasticsearch:9200"]
          restartPolicy: OnFailure
```

### 5.3 인덱스 롤오버 (Index Rollover)

`jaeger-es-rollover` 도구는 세 단계의 인덱스 라이프사이클을 관리한다.

**소스코드 참조** (`cmd/es-rollover/main.go`):

```
init → rollover → lookback
 │         │          │
 ▼         ▼          ▼
인덱스/   새 쓰기    오래된 인덱스를
앨리어스  인덱스로   읽기 앨리어스에서
생성     전환      제거
```

**1단계 -- init (초기화):**
```bash
jaeger-es-rollover init http://localhost:9200
```
- 초기 인덱스와 읽기/쓰기 앨리어스 생성
- ILM 클라이언트를 사용하여 ILM 정책 적용 가능

**2단계 -- rollover (전환):**
```bash
jaeger-es-rollover rollover http://localhost:9200
```
- 조건 충족 시 새 쓰기 인덱스 생성 및 쓰기 앨리어스 전환
- 조건: 인덱스 크기, 문서 수, 경과 시간 등

**3단계 -- lookback (정리):**
```bash
jaeger-es-rollover lookback http://localhost:9200
```
- 지정된 기간보다 오래된 인덱스를 읽기 앨리어스에서 제거
- 인덱스 자체는 삭제하지 않음 (데이터 보존)

### 5.4 ILM (Index Lifecycle Management) 통합

Elasticsearch ILM과 통합하여 인덱스 라이프사이클을 자동 관리할 수 있다. `jaeger-es-rollover init` 명령 실행 시 `ILMClient`를 통해 ILM 정책을 확인하고 적용한다.

---

## 6. 트러블슈팅 (Troubleshooting)

### 6.1 일반적인 문제와 해결 방법

| 증상 | 원인 | 해결 |
|------|------|------|
| UI에 트레이스 안 보임 | SDK가 잘못된 엔드포인트로 전송 | OTLP 엔드포인트 확인 (4317/4318) |
| 포트 충돌 | 다른 서비스가 같은 포트 사용 | 설정에서 포트 변경 또는 프로세스 확인 |
| 스토리지 연결 실패 | 잘못된 URL/인증 정보 | 로그에서 에러 메시지 확인, 네트워크 접근성 테스트 |
| Docker에서 접근 불가 | `localhost` 바인딩 | `JAEGER_LISTEN_HOST=0.0.0.0` 설정 |
| 메모리 부족 | `max_traces` 초과 또는 버퍼 과다 | `max_traces` 조정, batch processor 튜닝 |
| Kafka 연결 실패 | 브로커 주소 오류 | `KAFKA_BROKER` 환경변수 확인 |
| Collector-Ingester 포트 충돌 | 동일 호스트에 두 프로세스 | Ingester 포트를 다르게 설정 (8889, 14133) |

### 6.2 디버그 도구

**tracegen -- 합성 트레이스 생성:**

`cmd/tracegen/main.go`에 정의된 도구로, 테스트용 트레이스를 자동 생성한다.

```bash
# OTLP HTTP로 합성 트레이스 전송
tracegen -trace-exporter otlp-http

# OTLP gRPC 사용
tracegen -trace-exporter otlp-grpc

# stdout으로 트레이스 출력 (네트워크 없이 디버그)
tracegen -trace-exporter stdout

# 적응형 샘플링 테스트
tracegen -adaptive-sampling http://localhost:14268/api/sampling
```

지원하는 exporter 유형:
- `otlp` / `otlp-http`: OTLP HTTP 프로토콜
- `otlp-grpc`: OTLP gRPC 프로토콜
- `stdout`: 표준 출력

**anonymizer -- 데이터 익명화:**

`cmd/anonymizer/main.go`에 정의된 도구로, 트레이스 데이터를 익명화하여 안전하게 공유할 수 있게 한다.

```bash
# 트레이스 익명화
jaeger-anonymizer --trace-id <TRACE_ID> --query-host-port localhost:16685
```

출력 파일:
- `{trace_id}.original.json` -- 원본 트레이스
- `{trace_id}.anonymized.json` -- 익명화된 트레이스
- `{trace_id}.mapping.json` -- 매핑 정보
- `{trace_id}.anonymized-ui-trace.json` -- UI 호환 익명화 파일

익명화 옵션: `--hash-standard-tags`, `--hash-custom-tags`, `--hash-logs`, `--hash-process`

**zpages -- 파이프라인 내부 디버그:**

zpages 익스텐션은 OTel Collector 내부 파이프라인 상태를 웹 UI로 제공한다.

```
http://localhost:27778/debug/tracez     # 내부 트레이스 확인
http://localhost:27778/debug/pipelinez  # 파이프라인 상태 확인
```

**expvar -- Go 런타임 변수:**

```
http://localhost:27777/debug/vars       # Go 런타임 메트릭 (memstats 등)
```

### 6.3 로그 레벨 설정

```yaml
telemetry:
  logs:
    level: debug    # debug | info | warn | error
```

프로덕션 환경에서는 `info` 레벨을 권장한다. 문제 조사 시 일시적으로 `debug`로 변경한다.

### 6.4 헬스체크

```bash
# HTTP 헬스체크
curl http://localhost:13133/status

# v2 헬스체크 (healthcheckv2 익스텐션)
curl http://localhost:13133/
```

`healthcheckv2` 익스텐션은 `use_v2: true`로 설정하여 v2 형식의 상태 응답을 받을 수 있다.

---

## 7. 보안 (Security)

### 7.1 TLS 설정

Jaeger는 OTel Collector의 TLS 설정을 그대로 사용한다. 각 receiver, exporter, 스토리지 연결에 개별적으로 TLS를 구성할 수 있다.

```yaml
# OTLP receiver에 TLS 적용
receivers:
  otlp:
    protocols:
      grpc:
        tls:
          cert_file: /path/to/server.crt
          key_file: /path/to/server.key
          ca_file: /path/to/ca.crt
      http:
        tls:
          cert_file: /path/to/server.crt
          key_file: /path/to/server.key

# 스토리지 연결 TLS
extensions:
  jaeger_storage:
    backends:
      some_storage:
        cassandra:
          connection:
            tls:
              insecure: true       # true이면 TLS 비활성화
              # cert_file: ...
              # key_file: ...
              # ca_file: ...
```

`insecure: true`는 TLS를 완전히 비활성화하는 설정이다. 프로덕션에서는 반드시 인증서를 설정하고 `insecure: false`(기본값)로 두어야 한다.

### 7.2 Bearer Token 전파

Query 서비스에서 스토리지 백엔드로 Bearer Token을 전파할 수 있다. 이는 Elasticsearch 등의 백엔드가 토큰 기반 인증을 요구할 때 유용하다.

```yaml
# QueryOptions 구조체 (flags.go)
extensions:
  jaeger_query:
    bearer_token_propagation: true   # 스토리지로 Bearer Token 전달
```

**소스코드 참조** (`cmd/jaeger/internal/extension/jaegerquery/internal/flags.go`):
```go
type QueryOptions struct {
    BearerTokenPropagation bool `mapstructure:"bearer_token_propagation"`
    // ...
}
```

### 7.3 Basic Auth

스토리지 백엔드 연결에 Basic Auth를 직접 설정할 수 있다.

```yaml
# Cassandra Basic Auth
cassandra:
  connection:
    auth:
      basic:
        username: "cassandra"
        password: "cassandra"

# ClickHouse Basic Auth
clickhouse:
  auth:
    basic:
      username: default
      password: password
```

`jaeger-es-index-cleaner`와 `jaeger-es-rollover`도 `--es.username`, `--es.password` 플래그를 통해 Basic Auth를 지원한다.

### 7.4 멀티 테넌시 (Multi-Tenancy)

Jaeger는 HTTP 헤더 기반 멀티 테넌시를 지원한다. 테넌트 정보는 `x-tenant` 헤더(기본값)를 통해 전달되며, Context에 전파된다.

**소스코드 참조** (`internal/tenancy/manager.go`):
```go
type Options struct {
    Enabled bool
    Header  string      // 기본값: "x-tenant"
    Tenants []string    // 허용된 테넌트 목록 (빈 경우 모든 테넌트 허용)
}

type Manager struct {
    Enabled bool
    Header  string
    guard   guard       // 테넌트 검증 인터페이스
}
```

**테넌시 가드 전략 (`tenancyGuardFactory`):**
1. 테넌시 비활성화 또는 테넌트 목록 없음 -> 모든 테넌트 허용 (`tenantDontCare`)
2. 테넌트 목록 지정 -> 목록에 있는 테넌트만 허용 (`tenantList`)

**설정 예시:**
```yaml
extensions:
  jaeger_query:
    multi_tenancy:
      enabled: true
      header: "x-tenant"
      tenants: ["acme", "megacorp"]   # 허용된 테넌트 목록
```

**gRPC 인터셉터** (`internal/tenancy/grpc.go`):
- `NewGuardingStreamInterceptor`: Stream RPC에서 테넌시 헤더 검증
- `NewGuardingUnaryInterceptor`: Unary RPC에서 테넌시 헤더 검증

**Context 전파** (`internal/tenancy/context.go`):
```go
// 테넌트 정보를 Context에 저장
ctx = tenancy.WithTenant(ctx, "acme")

// Context에서 테넌트 정보 조회
tenant := tenancy.GetTenant(ctx)
```

### 7.5 SigV4 인증 (AWS)

AWS 서비스(Amazon OpenSearch Service 등) 접근 시 SigV4 서명 인증을 사용할 수 있다. OTel Collector의 `sigv4authextension`을 활용한다.

**소스코드 참조** (`cmd/jaeger/internal/components.go`):
```go
import "github.com/open-telemetry/opentelemetry-collector-contrib/extension/sigv4authextension"

// 익스텐션 팩토리 등록
sigv4authextension.NewFactory()
```

**설정 예시:**
```yaml
extensions:
  sigv4auth:
    region: "us-east-1"
    service: "es"       # Amazon OpenSearch Service

  jaeger_storage:
    backends:
      some_storage:
        elasticsearch:
          server_urls:
            - https://my-domain.us-east-1.es.amazonaws.com
          auth:
            authenticator_id: sigv4auth
```

Elasticsearch/OpenSearch 스토리지 백엔드에서 `authenticator_id`를 지정하면, 해당 인증 익스텐션이 모든 HTTP 요청에 SigV4 서명을 추가한다.

---

## 8. 운영 체크리스트

### 프로덕션 배포 전 확인 사항

```
[ ] 스토리지 백엔드 선택 및 용량 계획
[ ] 인덱스 롤오버/클리너 CronJob 설정 (ES 사용 시)
[ ] TLS 인증서 설정 (receiver, storage 연결)
[ ] 멀티 테넌시 설정 (필요 시)
[ ] Prometheus 스크래핑 설정 (port 8888)
[ ] 알림 규칙 적용 (prometheus_alerts_v2.yml)
[ ] Grafana 대시보드 import
[ ] 헬스체크 엔드포인트 모니터링 설정 (port 13133)
[ ] 로그 레벨 info로 설정 (debug는 개발 환경에서만)
[ ] batch processor 튜닝 (처리량에 따라)
[ ] Kafka 파이프라인 도입 여부 결정 (고처리량 환경)
[ ] 백업 및 복구 계획 수립
```

---

## 참조 파일

| 파일 | 경로 |
|------|------|
| All-in-One 기본 설정 | `cmd/jaeger/internal/all-in-one.yaml` |
| 프로덕션 설정 예시 | `cmd/jaeger/config.yaml` |
| Elasticsearch 설정 | `cmd/jaeger/config-elasticsearch.yaml` |
| OpenSearch 설정 | `cmd/jaeger/config-opensearch.yaml` |
| Cassandra 설정 | `cmd/jaeger/config-cassandra.yaml` |
| Badger 설정 | `cmd/jaeger/config-badger.yaml` |
| ClickHouse 설정 | `cmd/jaeger/config-clickhouse.yaml` |
| Kafka Collector 설정 | `cmd/jaeger/config-kafka-collector.yaml` |
| Kafka Ingester 설정 | `cmd/jaeger/config-kafka-ingester.yaml` |
| Query 전용 설정 | `cmd/jaeger/config-query.yaml` |
| Remote Storage 설정 | `cmd/jaeger/config-remote-storage.yaml` |
| SPM 설정 | `cmd/jaeger/config-spm.yaml` |
| 포트 정의 | `ports/ports.go` |
| 테넌시 관리자 | `internal/tenancy/manager.go` |
| Query 옵션 | `cmd/jaeger/internal/extension/jaegerquery/internal/flags.go` |
| 인덱스 클리너 | `cmd/es-index-cleaner/main.go` |
| 인덱스 롤오버 | `cmd/es-rollover/main.go` |
| tracegen | `cmd/tracegen/main.go` |
| anonymizer | `cmd/anonymizer/main.go` |
| Prometheus 알림 (v1) | `monitoring/jaeger-mixin/prometheus_alerts.yml` |
| Prometheus 알림 (v2) | `monitoring/jaeger-mixin/prometheus_alerts_v2.yml` |
| Grafana 대시보드 | `monitoring/jaeger-mixin/dashboard-for-grafana.json` |
