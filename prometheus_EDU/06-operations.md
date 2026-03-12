# Prometheus 운영 가이드

## 목차

1. [배포 및 설치](#1-배포-및-설치)
2. [핵심 설정 파일 (prometheus.yml)](#2-핵심-설정-파일)
3. [주요 CLI 플래그](#3-주요-cli-플래그)
4. [모니터링 (자체 메트릭)](#4-모니터링-자체-메트릭)
5. [트러블슈팅](#5-트러블슈팅)
6. [promtool CLI](#6-promtool-cli)
7. [고가용성 (HA)](#7-고가용성-ha)

---

## 1. 배포 및 설치

### 1.1 바이너리 설치

Prometheus 공식 릴리스 페이지에서 플랫폼에 맞는 바이너리를 다운로드한다.

```bash
# 최신 릴리스 다운로드 (예: Linux amd64)
wget https://github.com/prometheus/prometheus/releases/download/v2.53.0/prometheus-2.53.0.linux-amd64.tar.gz

# 압축 해제
tar xvfz prometheus-2.53.0.linux-amd64.tar.gz
cd prometheus-2.53.0.linux-amd64/

# 실행
./prometheus --config.file=prometheus.yml
```

바이너리 패키지에는 `prometheus`(서버)와 `promtool`(CLI 도구)이 함께 포함된다.

### 1.2 Docker 설치

```bash
# 기본 실행
docker run -p 9090:9090 prom/prometheus

# 커스텀 설정 파일 마운트
docker run -p 9090:9090 \
  -v /path/to/prometheus.yml:/etc/prometheus/prometheus.yml \
  -v /path/to/data:/prometheus \
  prom/prometheus

# 추가 플래그 전달
docker run -p 9090:9090 \
  -v /path/to/prometheus.yml:/etc/prometheus/prometheus.yml \
  prom/prometheus \
  --config.file=/etc/prometheus/prometheus.yml \
  --storage.tsdb.retention.time=30d \
  --web.enable-lifecycle
```

### 1.3 소스 빌드

```bash
# 저장소 클론
git clone https://github.com/prometheus/prometheus.git
cd prometheus

# 빌드 (Go 1.21+ 필요)
make build

# 빌드 결과물 확인
ls -la prometheus promtool

# 실행
./prometheus --config.file=prometheus.yml
```

`Makefile`의 `build` 타겟은 `cmd/prometheus/main.go`를 컴파일하여 `prometheus` 바이너리를 생성한다.

### 1.4 최소 설정으로 시작

```yaml
# prometheus.yml (최소 설정)
global:
  scrape_interval: 15s
  evaluation_interval: 15s

scrape_configs:
  - job_name: "prometheus"
    static_configs:
      - targets: ["localhost:9090"]
```

위 설정만으로 Prometheus가 자기 자신의 메트릭을 수집하면서 실행된다. 기본 포트는 `9090`이며, `--web.listen-address` 플래그로 변경할 수 있다.

```bash
# 기본 포트(9090)로 실행
./prometheus --config.file=prometheus.yml

# 포트 변경
./prometheus --config.file=prometheus.yml --web.listen-address=0.0.0.0:9191
```

실행 후 브라우저에서 `http://localhost:9090`에 접속하면 Prometheus Web UI를 확인할 수 있다.

---

## 2. 핵심 설정 파일

### 2.1 전체 구조

Prometheus 설정 파일(`prometheus.yml`)은 YAML 형식이며, 아래 최상위 섹션으로 구성된다.
설정 파싱 로직은 `config/config.go`의 `Load()` 함수에서 처리되며, 각 섹션의 기본값은 `DefaultConfig`와 `DefaultGlobalConfig`에 정의되어 있다.

```yaml
# prometheus.yml 전체 구조
global:
  # 전역 설정

scrape_configs:
  # 스크래프 대상 설정

rule_files:
  # 규칙 파일 경로

alerting:
  # Alertmanager 연동 설정

remote_write:
  # 원격 쓰기 설정

remote_read:
  # 원격 읽기 설정

storage:
  # TSDB 스토리지 설정
```

### 2.2 global (전역 설정)

`config/config.go`의 `DefaultGlobalConfig`에 정의된 기본값:

| 설정 항목 | 기본값 | 설명 |
|----------|--------|------|
| `scrape_interval` | `1m` | 메트릭 수집 주기 |
| `scrape_timeout` | `10s` | 스크래프 타임아웃 |
| `evaluation_interval` | `1m` | 규칙 평가 주기 |
| `external_labels` | `{}` | 외부 시스템 연동 시 추가되는 라벨 |

```yaml
global:
  scrape_interval: 15s          # 15초마다 메트릭 수집
  scrape_timeout: 10s           # 10초 내 응답 없으면 타임아웃
  evaluation_interval: 15s      # 15초마다 규칙 평가
  external_labels:
    cluster: "production"
    region: "ap-northeast-2"
```

**주의사항:**
- `scrape_timeout`은 `scrape_interval`보다 클 수 없다. `config/config.go`에서 검증 로직이 이를 체크한다.
- `external_labels`는 Federation, Remote Write, Alertmanager 전송 시 모든 시계열에 자동 추가된다.

### 2.3 scrape_configs (스크래프 설정)

각 `job`별로 스크래프 대상과 방식을 정의한다.

```yaml
scrape_configs:
  # 정적 타겟 설정
  - job_name: "node-exporter"
    metrics_path: "/metrics"        # 기본값: /metrics
    scheme: "http"                  # 기본값: http
    scrape_interval: 30s            # global 값 오버라이드 가능
    static_configs:
      - targets:
          - "server1:9100"
          - "server2:9100"
        labels:
          env: "production"

  # 서비스 디스커버리 (Kubernetes 예시)
  - job_name: "kubernetes-pods"
    kubernetes_sd_configs:
      - role: pod
        namespaces:
          names:
            - "default"
            - "monitoring"
    relabel_configs:
      - source_labels: [__meta_kubernetes_pod_annotation_prometheus_io_scrape]
        action: keep
        regex: "true"
      - source_labels: [__meta_kubernetes_pod_annotation_prometheus_io_path]
        action: replace
        target_label: __metrics_path__
        regex: (.+)

  # 파일 기반 서비스 디스커버리
  - job_name: "file-sd"
    file_sd_configs:
      - files:
          - "/etc/prometheus/targets/*.json"
        refresh_interval: 30s

  # Consul 서비스 디스커버리
  - job_name: "consul-services"
    consul_sd_configs:
      - server: "consul.example.com:8500"
        services:
          - "web"
          - "api"
```

**주요 서비스 디스커버리 메커니즘:**

| SD 방식 | 설정 키 | 용도 |
|---------|---------|------|
| 정적 | `static_configs` | 고정 타겟 |
| 파일 | `file_sd_configs` | JSON/YAML 파일 기반 동적 타겟 |
| Kubernetes | `kubernetes_sd_configs` | K8s Pod/Service/Node 자동 발견 |
| Consul | `consul_sd_configs` | Consul 서비스 레지스트리 |
| DNS | `dns_sd_configs` | DNS SRV/A 레코드 기반 |
| EC2 | `ec2_sd_configs` | AWS EC2 인스턴스 자동 발견 |

### 2.4 rule_files (규칙 파일)

Recording 규칙과 Alerting 규칙 파일 경로를 지정한다. 글로브 패턴을 지원한다.

```yaml
rule_files:
  - "rules/*.yml"
  - "alerts/*.yml"
```

Recording 규칙 예시 (`rules/recording.yml`):

```yaml
groups:
  - name: node_exporter
    interval: 15s
    rules:
      - record: instance:node_cpu_utilisation:rate5m
        expr: 1 - avg without(cpu) (rate(node_cpu_seconds_total{mode="idle"}[5m]))
      - record: instance:node_memory_utilisation:ratio
        expr: 1 - node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes
```

Alerting 규칙 예시 (`alerts/node.yml`):

```yaml
groups:
  - name: node_alerts
    rules:
      - alert: HighCPUUsage
        expr: instance:node_cpu_utilisation:rate5m > 0.9
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "높은 CPU 사용률 ({{ $labels.instance }})"
          description: "CPU 사용률이 90%를 초과하여 5분 이상 지속됨"
```

### 2.5 alerting (Alertmanager 설정)

```yaml
alerting:
  alert_relabel_configs:
    - source_labels: [severity]
      regex: "info"
      action: drop                  # info 수준 알림 제외
  alertmanagers:
    - static_configs:
        - targets:
            - "alertmanager1:9093"
            - "alertmanager2:9093"
      timeout: 10s
      api_version: v2
```

### 2.6 remote_write / remote_read (원격 저장소)

```yaml
# 원격 쓰기 (장기 저장소로 데이터 전송)
remote_write:
  - url: "http://thanos-receive:19291/api/v1/receive"
    queue_config:
      max_samples_per_send: 1000
      batch_send_deadline: 5s
      max_shards: 200
    write_relabel_configs:
      - source_labels: [__name__]
        regex: "go_.*"
        action: drop                # go_ 접두사 메트릭 제외

# 원격 읽기 (장기 저장소에서 데이터 조회)
remote_read:
  - url: "http://thanos-store:19090/api/v1/read"
    read_recent: false              # 최근 데이터는 로컬 TSDB에서 읽기
```

### 2.7 storage (TSDB 설정)

```yaml
storage:
  tsdb:
    retention:
      time: 30d                     # 시간 기반 보존 (기본: 15d)
      size: 50GB                    # 크기 기반 보존
    out_of_order_time_window: 5m    # 순서 어긋난 샘플 허용 범위
```

---

## 3. 주요 CLI 플래그

`cmd/prometheus/main.go`에 정의된 주요 CLI 플래그 목록이다.

### 3.1 설정 및 웹 관련

| 플래그 | 기본값 | 설명 |
|--------|--------|------|
| `--config.file` | `prometheus.yml` | 설정 파일 경로 |
| `--web.listen-address` | `0.0.0.0:9090` | 리스닝 주소 (반복 지정 가능) |
| `--web.enable-lifecycle` | `false` | `/-/reload`, `/-/quit` HTTP 엔드포인트 활성화 |
| `--web.enable-admin-api` | `false` | 관리 API(`/api/v1/admin/`) 활성화 |
| `--web.config.file` | - | TLS/인증 설정 파일 경로 |

### 3.2 스토리지 관련

| 플래그 | 기본값 | 설명 |
|--------|--------|------|
| `--storage.tsdb.path` | `data/` | TSDB 데이터 디렉토리 |
| `--storage.tsdb.retention.time` | `15d` | 시간 기반 데이터 보존 기간 |
| `--storage.tsdb.retention.size` | - | 크기 기반 보존 (예: `512MB`, `50GB`) |
| `--storage.tsdb.wal-compression` | `true` | WAL 압축 활성화 |

`defaultRetentionString`은 `cmd/prometheus/main.go`에서 `"15d"`로 정의되어 있다. `retention.time`과 `retention.size`가 모두 설정되지 않으면 이 기본값이 적용된다.

### 3.3 쿼리 관련

| 플래그 | 기본값 | 설명 |
|--------|--------|------|
| `--query.timeout` | `2m` | 쿼리 최대 실행 시간 |
| `--query.max-concurrency` | `20` | 동시 실행 가능한 최대 쿼리 수 |
| `--query.max-samples` | `50000000` | 쿼리당 메모리에 로드 가능한 최대 샘플 수 (5천만) |

### 3.4 기능 플래그 및 Agent 모드

```bash
# 기능 플래그 활성화 (쉼표로 구분)
./prometheus --enable-feature=exemplar-storage,native-histograms

# Agent 모드 실행 (로컬 쿼리/규칙 비활성, remote_write만 동작)
./prometheus --agent --config.file=agent.yml
```

| 플래그 | 설명 |
|--------|------|
| `--enable-feature` | 실험적/선택적 기능 활성화 |
| `--agent` | Agent 모드로 실행 (remote_write 전용) |

**Agent 모드의 특징:**
- TSDB 대신 WAL 기반 스토리지 사용 (`--storage.agent.path`, 기본: `data-agent/`)
- 로컬 쿼리, Recording 규칙, Alerting 규칙 비활성화
- `remote_write` 설정만 동작하여 중앙 수집기로 데이터 전달
- 서버 모드 전용 플래그(`--storage.tsdb.*`, `--query.*` 등) 사용 불가

### 3.5 CLI 플래그 실사용 예시

```bash
# 프로덕션 환경 실행 예시
./prometheus \
  --config.file=/etc/prometheus/prometheus.yml \
  --storage.tsdb.path=/var/lib/prometheus/data \
  --storage.tsdb.retention.time=90d \
  --storage.tsdb.retention.size=100GB \
  --web.listen-address=0.0.0.0:9090 \
  --web.enable-lifecycle \
  --web.enable-admin-api \
  --query.timeout=5m \
  --query.max-concurrency=30 \
  --query.max-samples=100000000

# Agent 모드 실행 예시
./prometheus \
  --agent \
  --config.file=/etc/prometheus/agent.yml \
  --storage.agent.path=/var/lib/prometheus/agent-data \
  --web.listen-address=0.0.0.0:9090
```

### 3.6 설정 리로드

설정을 변경한 후 재시작 없이 반영하는 방법:

```bash
# 방법 1: SIGHUP 시그널 전송
kill -HUP $(pidof prometheus)

# 방법 2: HTTP 엔드포인트 (--web.enable-lifecycle 필요)
curl -X POST http://localhost:9090/-/reload

# 리로드 성공 여부 확인 (메트릭으로)
# prometheus_config_last_reload_successful == 1 이면 성공
curl -s http://localhost:9090/api/v1/query?query=prometheus_config_last_reload_successful
```

---

## 4. 모니터링 (자체 메트릭)

Prometheus는 자기 자신의 운영 상태를 메트릭으로 노출한다. `/metrics` 엔드포인트에서 확인 가능하다.

### 4.1 설정 및 상태 메트릭

| 메트릭 이름 | 타입 | 설명 |
|------------|------|------|
| `prometheus_config_last_reload_successful` | Gauge | 마지막 설정 리로드 성공 여부 (1=성공, 0=실패) |
| `prometheus_config_last_reload_success_timestamp_seconds` | Gauge | 마지막 성공적 리로드 타임스탬프 |
| `prometheus_build_info` | Gauge | 빌드 정보 (버전, 리비전, 브랜치 등) |
| `prometheus_ready` | Gauge | Prometheus 준비 상태 |

`prometheus_config_last_reload_successful`은 `cmd/prometheus/main.go`에서 직접 정의되어 있으며, 설정 리로드 실패 시 즉각적인 알림을 받는 데 활용한다.

### 4.2 TSDB 메트릭

| 메트릭 이름 | 타입 | 설명 |
|------------|------|------|
| `prometheus_tsdb_head_series` | Gauge | Head 블록의 현재 시계열 수 |
| `prometheus_tsdb_head_chunks` | Gauge | Head 블록의 현재 청크 수 |
| `prometheus_tsdb_head_samples_appended_total` | Counter | Head에 추가된 총 샘플 수 |
| `prometheus_tsdb_blocks_loaded` | Gauge | 현재 로드된 영구 블록 수 |
| `prometheus_tsdb_compactions_total` | Counter | 실행된 총 Compaction 수 |
| `prometheus_tsdb_compaction_duration_seconds` | Histogram | Compaction 소요 시간 |
| `prometheus_tsdb_wal_corruptions_total` | Counter | WAL 손상 발견 횟수 |
| `prometheus_tsdb_head_series_created_total` | Counter | 생성된 총 시계열 수 |
| `prometheus_tsdb_head_series_removed_total` | Counter | 제거된 총 시계열 수 |

### 4.3 쿼리 엔진 메트릭

| 메트릭 이름 | 타입 | 설명 |
|------------|------|------|
| `prometheus_engine_queries` | Gauge | 현재 실행 중인 쿼리 수 |
| `prometheus_engine_queries_concurrent_max` | Gauge | 최대 동시 쿼리 수 |
| `prometheus_engine_query_duration_seconds` | Summary | 쿼리 실행 소요 시간 |
| `prometheus_engine_query_samples_total` | Counter | 쿼리가 접근한 총 샘플 수 |

### 4.4 스크래프 메트릭

| 메트릭 이름 | 타입 | 설명 |
|------------|------|------|
| `scrape_duration_seconds` | Gauge | 마지막 스크래프 소요 시간 (타겟별) |
| `scrape_samples_scraped` | Gauge | 마지막 스크래프에서 수집한 샘플 수 |
| `scrape_samples_post_metric_relabeling` | Gauge | Relabeling 후 남은 샘플 수 |
| `scrape_series_added` | Gauge | 마지막 스크래프에서 추가된 시계열 수 |
| `up` | Gauge | 타겟 스크래프 성공 여부 (1=성공, 0=실패) |
| `prometheus_target_scrape_duration_seconds` | Summary | 전체 스크래프 소요 시간 분포 |

`scrape_duration_seconds`는 `scrape/scrape.go`에서 각 타겟의 스크래프 완료 시 기록된다.

### 4.5 핵심 모니터링 알림 규칙

자체 모니터링을 위한 알림 규칙 예시:

```yaml
groups:
  - name: prometheus_self_monitoring
    rules:
      # 설정 리로드 실패 감지
      - alert: PrometheusConfigReloadFailed
        expr: prometheus_config_last_reload_successful == 0
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "Prometheus 설정 리로드 실패"

      # 높은 시계열 카디널리티 감지
      - alert: PrometheusHighCardinalitySeries
        expr: prometheus_tsdb_head_series > 5000000
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "Head 시계열 수가 500만 초과"

      # 스크래프 실패 감지
      - alert: PrometheusTargetDown
        expr: up == 0
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "타겟 {{ $labels.instance }} 스크래프 실패"

      # 쿼리 엔진 포화
      - alert: PrometheusQueryEngineNearCapacity
        expr: prometheus_engine_queries / prometheus_engine_queries_concurrent_max > 0.8
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "쿼리 동시 실행 용량 80% 초과"

      # WAL 손상 감지
      - alert: PrometheusTSDBWALCorruption
        expr: increase(prometheus_tsdb_wal_corruptions_total[1h]) > 0
        labels:
          severity: critical
        annotations:
          summary: "WAL 손상 감지됨"
```

### 4.6 Grafana 대시보드용 주요 쿼리

```promql
# 초당 수집 샘플 수
rate(prometheus_tsdb_head_samples_appended_total[5m])

# 시계열 증가/감소 추세
rate(prometheus_tsdb_head_series_created_total[1h]) - rate(prometheus_tsdb_head_series_removed_total[1h])

# 쿼리 평균 실행 시간
rate(prometheus_engine_query_duration_seconds_sum[5m]) / rate(prometheus_engine_query_duration_seconds_count[5m])

# 디스크 사용량 추세 (로드된 블록 수 기반)
prometheus_tsdb_blocks_loaded

# 스크래프 소요 시간 상위 10개 타겟
topk(10, scrape_duration_seconds)
```

---

## 5. 트러블슈팅

### 5.1 높은 메모리 사용

**증상:** Prometheus 프로세스의 RSS 메모리가 지속적으로 증가하거나 OOM 발생

**원인 분석:**

```promql
# 현재 시계열 카디널리티 확인
prometheus_tsdb_head_series

# 카디널리티 급증 여부 확인
rate(prometheus_tsdb_head_series_created_total[1h])

# 쿼리 메모리 사용 확인
prometheus_engine_query_samples_total
```

**해결 방법:**

| 조치 | 방법 |
|------|------|
| 카디널리티 줄이기 | `metric_relabel_configs`로 불필요한 라벨/메트릭 제거 |
| 쿼리 샘플 제한 | `--query.max-samples` 조정 (기본 5천만) |
| 수집 대상 줄이기 | `relabel_configs`로 불필요한 타겟 필터링 |
| 스크래프 주기 늘리기 | `scrape_interval` 증가 (카디널리티에 영향 없지만 메모리 절약) |

```yaml
# 높은 카디널리티 라벨 제거 예시
scrape_configs:
  - job_name: "high-cardinality-app"
    metric_relabel_configs:
      - source_labels: [__name__]
        regex: "go_gc_.*"
        action: drop
      - source_labels: [le]
        regex: ".+"
        action: keep           # 히스토그램 버킷만 유지
```

### 5.2 느린 쿼리

**증상:** 쿼리 실행이 타임아웃되거나 UI 응답이 느림

**원인 분석:**

```promql
# 느린 쿼리 통계 확인
prometheus_engine_query_duration_seconds

# 동시 실행 쿼리 수 확인
prometheus_engine_queries
```

**해결 방법:**

```bash
# 쿼리 타임아웃 늘리기
./prometheus --query.timeout=5m

# 동시 쿼리 수 늘리기
./prometheus --query.max-concurrency=40
```

| 쿼리 최적화 기법 | 설명 |
|----------------|------|
| 범위 축소 | `[5m]` 대신 `[1m]` 사용 (데이터 양에 따라) |
| Recording 규칙 | 자주 사용하는 복잡한 쿼리를 사전 계산 |
| 서브쿼리 주의 | `rate(metric[5m])[1h:1m]` 같은 서브쿼리는 계산량이 큼 |
| 정규식 최소화 | `{__name__=~".*"}` 같은 광범위 매칭 회피 |
| `without` 사용 | `by()`보다 `without()`이 효율적인 경우가 있음 |

### 5.3 WAL 손상

**증상:** 시작 시 WAL 관련 에러 로그 출력, 데이터 손실

**원인:** 비정상 종료(OOM Kill, 강제 종료), 디스크 장애

**해결 방법:**

```bash
# WAL 상태 확인
ls -la data/wal/

# WAL 압축 활성화 (디스크 I/O 및 공간 절약)
./prometheus --storage.tsdb.wal-compression

# TSDB 상태 확인 (promtool 사용)
./promtool tsdb list data/

# 심각한 손상 시: 체크포인트까지만 복구
# WAL 디렉토리의 손상된 세그먼트를 삭제하고 재시작
# (데이터 손실 가능성 있음, 마지막 수단)
```

**예방 조치:**
- WAL 압축 활성화: 디스크 I/O를 줄여 손상 가능성 감소
- 적절한 셧다운: `SIGTERM` 시그널 사용 또는 `/-/quit` 엔드포인트
- 디스크 모니터링: IOPS 및 지연시간 확인
- UPS/안정적 스토리지: 갑작스러운 전원 차단 방지

### 5.4 스크래프 실패

**증상:** 특정 타겟에서 `up == 0`

**진단 순서:**

```
1. Web UI → Status → Targets 페이지 확인
   http://localhost:9090/targets

2. 타겟 상태 및 에러 메시지 확인
   - "connection refused": 타겟 서비스 미실행
   - "context deadline exceeded": 타임아웃
   - "server returned HTTP status 403": 인증 문제

3. 메트릭으로 확인:
   scrape_duration_seconds{job="문제_잡"}  → 타임아웃 근접 여부
   scrape_samples_scraped{job="문제_잡"}   → 수집된 샘플 수
```

**일반적인 해결 방법:**

| 에러 | 원인 | 해결 |
|------|------|------|
| `connection refused` | 타겟 다운 또는 포트 불일치 | 타겟 서비스 확인, 포트 검증 |
| `context deadline exceeded` | 스크래프 타임아웃 | `scrape_timeout` 증가 또는 메트릭 수 줄이기 |
| `HTTP 401/403` | 인증 실패 | `basic_auth` 또는 `bearer_token` 설정 확인 |
| `TLS handshake error` | 인증서 문제 | `tls_config` 설정, 인증서 유효성 확인 |
| `no such host` | DNS 해석 실패 | 타겟 주소 확인, DNS 서버 상태 점검 |

### 5.5 디스크 공간 부족

**증상:** 디스크 사용량 지속 증가, Compaction 실패

**진단:**

```bash
# 데이터 디렉토리 크기 확인
du -sh data/
du -sh data/wal/
du -sh data/chunks_head/

# 블록별 크기 확인
ls -lh data/
```

```promql
# 로드된 블록 수 추이
prometheus_tsdb_blocks_loaded

# Compaction 상태
rate(prometheus_tsdb_compactions_total[1h])
prometheus_tsdb_compaction_duration_seconds
```

**해결 방법:**

```bash
# 보존 기간 줄이기
./prometheus --storage.tsdb.retention.time=7d

# 크기 기반 보존 설정
./prometheus --storage.tsdb.retention.size=50GB

# Admin API로 불필요한 시계열 삭제 (--web.enable-admin-api 필요)
curl -X POST \
  -g 'http://localhost:9090/api/v1/admin/tsdb/delete_series?match[]={job="old-service"}'

# 삭제 후 공간 회수 (Compaction 트리거)
curl -X POST http://localhost:9090/api/v1/admin/tsdb/clean_tombstones
```

---

## 6. promtool CLI

`promtool`은 Prometheus와 함께 배포되는 CLI 유틸리티로, 설정 검증, 규칙 테스트, TSDB 관리 등에 사용한다.

### 6.1 설정 검증

```bash
# 설정 파일 문법/구조 검증
promtool check config prometheus.yml

# 출력 예시 (성공)
# Checking prometheus.yml
#  SUCCESS: prometheus.yml is valid prometheus config file

# 출력 예시 (실패)
# Checking prometheus.yml
#  FAILED: parsing YAML file prometheus.yml: unknown fields in scrape_config: scrape_inteval
```

설정 변경 후 반드시 `check config`로 검증하는 것이 권장된다. CI/CD 파이프라인에 포함하면 배포 전 오류를 방지할 수 있다.

### 6.2 규칙 파일 검증

```bash
# 규칙 파일 문법 검증
promtool check rules rules/*.yml

# 규칙 단위 테스트 실행
promtool test rules test/*.yml
```

규칙 단위 테스트 파일 예시:

```yaml
# test/recording_rules_test.yml
rule_files:
  - ../rules/recording.yml

evaluation_interval: 1m

tests:
  - interval: 1m
    input_series:
      - series: 'node_cpu_seconds_total{cpu="0",mode="idle"}'
        values: "0+60x10"
      - series: 'node_cpu_seconds_total{cpu="0",mode="user"}'
        values: "0+40x10"
    alert_rule_test: []
    promql_expr_test:
      - expr: instance:node_cpu_utilisation:rate5m
        eval_time: 5m
        exp_samples:
          - value: 0.4
```

### 6.3 원격 쿼리 실행

```bash
# 즉시 쿼리
promtool query instant http://localhost:9090 'up'

# 범위 쿼리
promtool query range http://localhost:9090 \
  --start="2024-01-01T00:00:00Z" \
  --end="2024-01-01T01:00:00Z" \
  --step=1m \
  'rate(prometheus_tsdb_head_samples_appended_total[5m])'

# 시계열 메타데이터 조회
promtool query series http://localhost:9090 \
  --match='prometheus_tsdb_head_series'

# 라벨 값 조회
promtool query labels http://localhost:9090 job
```

### 6.4 TSDB 관리

```bash
# TSDB 블록 목록 조회
promtool tsdb list data/

# TSDB 벤치마크 (읽기 성능 측정)
promtool tsdb bench write data/

# TSDB 분석 (블록 상세 정보)
promtool tsdb analyze data/

# TSDB 덤프 (데이터 내보내기)
promtool tsdb dump data/

# OpenMetrics 형식으로 백필 (과거 데이터 임포트)
promtool tsdb create-blocks-from openmetrics input.txt data/
```

### 6.5 promtool 활용 체크리스트

| 작업 | 명령어 | 시점 |
|------|--------|------|
| 설정 검증 | `promtool check config` | 설정 변경 시 |
| 규칙 검증 | `promtool check rules` | 규칙 파일 변경 시 |
| 규칙 테스트 | `promtool test rules` | CI/CD 파이프라인 |
| 상태 점검 | `promtool query instant` | 운영 중 디버깅 |
| TSDB 분석 | `promtool tsdb analyze` | 디스크/성능 문제 시 |

---

## 7. 고가용성 (HA)

### 7.1 기본 HA 구성

Prometheus는 내장된 클러스터링 기능이 없다. HA를 위해 **동일한 설정의 두 인스턴스를 병렬 실행**하는 방식을 사용한다.

```
┌─────────────────────┐     ┌─────────────────────┐
│   Prometheus A      │     │   Prometheus B      │
│                     │     │                     │
│ external_labels:    │     │ external_labels:    │
│   replica: "a"      │     │   replica: "b"      │
│                     │     │                     │
│ 동일한 scrape_configs│     │ 동일한 scrape_configs│
│ 동일한 rule_files   │     │ 동일한 rule_files   │
└────────┬────────────┘     └────────┬────────────┘
         │                           │
         ▼                           ▼
   ┌──────────┐                ┌──────────┐
   │ TSDB (A) │                │ TSDB (B) │
   └──────────┘                └──────────┘
         │                           │
         └─────────┬─────────────────┘
                   ▼
           ┌──────────────┐
           │ Alertmanager │
           │ (중복 제거)   │
           └──────────────┘
```

### 7.2 HA 설정 예시

```yaml
# prometheus-a.yml
global:
  scrape_interval: 15s
  evaluation_interval: 15s
  external_labels:
    cluster: "production"
    replica: "a"              # 인스턴스 A 식별

scrape_configs:
  - job_name: "app"
    static_configs:
      - targets: ["app:8080"]

alerting:
  alertmanagers:
    - static_configs:
        - targets:
            - "alertmanager1:9093"
            - "alertmanager2:9093"
```

```yaml
# prometheus-b.yml (replica 라벨만 다름)
global:
  scrape_interval: 15s
  evaluation_interval: 15s
  external_labels:
    cluster: "production"
    replica: "b"              # 인스턴스 B 식별

# 나머지 설정은 동일
```

**핵심 원칙:**
- 두 인스턴스가 동일한 타겟을 독립적으로 수집
- `external_labels`의 `replica` 라벨로 인스턴스를 구분
- Alertmanager의 deduplication 기능이 중복 알림을 제거
- 한 인스턴스가 다운되어도 다른 인스턴스가 수집/알림을 계속 처리

### 7.3 Thanos 연동

장기 저장소 및 글로벌 뷰가 필요한 경우 Thanos를 연동한다.

```
┌──────────────────┐    ┌──────────────────┐
│  Prometheus A    │    │  Prometheus B    │
│  + Thanos Sidecar│    │  + Thanos Sidecar│
└────────┬─────────┘    └────────┬─────────┘
         │                       │
         ▼                       ▼
   ┌───────────┐           ┌───────────┐
   │ Object    │           │ Object    │
   │ Storage   │◄──────────│ Storage   │
   │ (S3/GCS)  │           │ (S3/GCS)  │
   └─────┬─────┘           └─────┬─────┘
         │                       │
         └──────────┬────────────┘
                    ▼
            ┌──────────────┐
            │ Thanos Query │  ← 글로벌 뷰, 중복 제거
            └──────────────┘
```

Thanos Sidecar를 위한 Prometheus 설정:

```yaml
# prometheus.yml (Thanos Sidecar 연동)
global:
  external_labels:
    cluster: "production"
    replica: "a"

# Thanos Sidecar는 TSDB 블록을 Object Storage에 업로드
# --storage.tsdb.min-block-duration=2h
# --storage.tsdb.max-block-duration=2h
# 위 플래그로 블록 크기를 고정하여 Sidecar 호환성 보장
```

```bash
# Thanos Sidecar 실행
thanos sidecar \
  --tsdb.path=/var/lib/prometheus/data \
  --prometheus.url=http://localhost:9090 \
  --objstore.config-file=bucket.yml \
  --grpc-address=0.0.0.0:10901
```

### 7.4 HA 설계 시 고려사항

| 항목 | 권장 사항 |
|------|----------|
| 인스턴스 수 | 최소 2개 (동일 설정) |
| 외부 라벨 | `replica` 라벨로 인스턴스 구분 |
| Alertmanager | 클러스터 모드로 실행, 중복 알림 자동 제거 |
| 장기 저장 | Thanos 또는 Cortex/Mimir 연동 |
| 데이터 일관성 | 두 인스턴스 간 약간의 타이밍 차이는 정상 |
| 페일오버 | 로드밸런서 또는 Thanos Query로 자동 전환 |
| 디스크 | 각 인스턴스가 독립적인 스토리지 사용 |

### 7.5 Federation (연합 수집)

여러 Prometheus 인스턴스의 데이터를 상위 인스턴스에서 수집하는 패턴:

```yaml
# 상위 Prometheus 설정 (Federation)
scrape_configs:
  - job_name: "federate"
    scrape_interval: 30s
    honor_labels: true
    metrics_path: "/federate"
    params:
      match[]:
        - '{job="app"}'                    # 특정 잡의 메트릭만 수집
        - '{__name__=~"job:.*"}'           # Recording 규칙 결과만 수집
    static_configs:
      - targets:
          - "prometheus-dc1:9090"
          - "prometheus-dc2:9090"
```

**Federation 주의사항:**
- 모든 메트릭을 수집하면 성능 문제 발생 (Recording 규칙 결과만 수집 권장)
- `honor_labels: true`로 원본 라벨 유지
- 장기적으로 Thanos/Mimir 같은 전용 솔루션이 더 적합

---

## 부록: 운영 체크리스트

### 배포 전 체크리스트

- [ ] `promtool check config`로 설정 파일 검증
- [ ] `promtool check rules`로 규칙 파일 검증
- [ ] 적절한 `retention.time` / `retention.size` 설정
- [ ] `--web.enable-lifecycle` 활성화 (무중단 리로드 지원)
- [ ] `external_labels` 설정 (HA/Federation/Remote Write 시 필수)
- [ ] 자체 모니터링 알림 규칙 구성

### 일상 운영 체크리스트

- [ ] `prometheus_config_last_reload_successful` 확인
- [ ] `prometheus_tsdb_head_series` 추이 모니터링 (카디널리티 폭증 감지)
- [ ] `up == 0` 타겟 확인
- [ ] 디스크 사용량 모니터링
- [ ] 쿼리 성능 (`prometheus_engine_query_duration_seconds`) 확인
- [ ] WAL 크기 및 상태 확인

### 장애 대응 체크리스트

- [ ] `/targets` 페이지에서 타겟 상태 확인
- [ ] 로그 확인 (`--log.level=debug`로 상세 로그 활성화)
- [ ] TSDB 상태 확인 (`promtool tsdb analyze`)
- [ ] 메모리/CPU/디스크 리소스 확인
- [ ] 최근 설정 변경 이력 확인
