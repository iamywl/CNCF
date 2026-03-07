# Loki 운영 가이드

## 1. 배포 모드

### 1.1 모노리스 (Single Binary)

```yaml
# docker-compose.yaml
services:
  loki:
    image: grafana/loki:latest
    ports:
      - "3100:3100"
    command: -config.file=/etc/loki/local-config.yaml
    volumes:
      - ./loki-config.yaml:/etc/loki/local-config.yaml
      - loki-data:/tmp/loki
```

**특징:**
- `-target=all` (기본값)
- 모든 컴포넌트가 단일 프로세스에서 실행
- 개발/소규모 환경에 적합
- 스케일링 불가 (수직 확장만)

### 1.2 Simple Scalable (SSD)

```
┌──────────────┐  ┌──────────────┐  ┌──────────────┐
│ Write Nodes  │  │  Read Nodes  │  │Backend Nodes │
│ -target=write│  │ -target=read │  │-target=backend│
│ Distributor  │  │QueryFrontend │  │Compactor     │
│ Ingester     │  │Querier       │  │IndexGateway  │
│              │  │              │  │Ruler         │
│ × 3 replicas │  │ × 2 replicas │  │ × 1 replica  │
└──────────────┘  └──────────────┘  └──────────────┘
```

**Helm 배포:**
```bash
helm install loki grafana/loki \
  --set deploymentMode=SimpleScalable \
  --set write.replicas=3 \
  --set read.replicas=2 \
  --set backend.replicas=1
```

### 1.3 마이크로서비스

각 컴포넌트를 독립 서비스로 배포. 대규모 환경에 적합.

```bash
# 각 컴포넌트 개별 배포
loki -target=distributor -config.file=config.yaml
loki -target=ingester -config.file=config.yaml
loki -target=querier -config.file=config.yaml
loki -target=query-frontend -config.file=config.yaml
loki -target=compactor -config.file=config.yaml
loki -target=ruler -config.file=config.yaml
loki -target=index-gateway -config.file=config.yaml
```

---

## 2. 핵심 설정

### 2.1 최소 설정

```yaml
auth_enabled: false

server:
  http_listen_port: 3100
  grpc_listen_port: 9095

common:
  ring:
    instance_addr: 127.0.0.1
    kvstore:
      store: inmemory
  replication_factor: 1
  path_prefix: /tmp/loki

schema_config:
  configs:
    - from: 2024-01-01
      store: tsdb
      object_store: filesystem
      schema: v13
      index:
        prefix: index_
        period: 24h

storage_config:
  filesystem:
    directory: /tmp/loki/chunks

limits_config:
  reject_old_samples: true
  reject_old_samples_max_age: 168h   # 7일
```

### 2.2 프로덕션 설정 (S3)

```yaml
auth_enabled: true

server:
  http_listen_port: 3100
  grpc_listen_port: 9095
  grpc_server_max_recv_msg_size: 8388608   # 8MB

distributor:
  ring:
    kvstore:
      store: memberlist

ingester:
  lifecycler:
    ring:
      kvstore:
        store: memberlist
      replication_factor: 3
    heartbeat_period: 5s
  chunk_idle_period: 30m
  max_chunk_age: 2h
  chunk_retain_period: 1m
  wal:
    dir: /var/loki/wal
    replay_memory_ceiling: 4GB

schema_config:
  configs:
    - from: 2024-01-01
      store: tsdb
      object_store: s3
      schema: v13
      index:
        prefix: loki_index_
        period: 24h

storage_config:
  aws:
    s3: s3://region/bucket-name
    s3forcepathstyle: false

limits_config:
  ingestion_rate_mb: 10
  ingestion_burst_size_mb: 20
  max_streams_per_user: 10000
  max_line_size: 256000
  reject_old_samples: true
  reject_old_samples_max_age: 168h
  retention_period: 720h            # 30일

compactor:
  working_directory: /var/loki/compactor
  retention_enabled: true

memberlist:
  join_members:
    - loki-memberlist:7946
```

### 2.3 테넌트별 제한 오버라이드

```yaml
# runtime-config.yaml (동적 리로드 가능)
overrides:
  "tenant-heavy":
    ingestion_rate_mb: 50
    ingestion_burst_size_mb: 100
    max_streams_per_user: 50000
    max_query_parallelism: 64
    max_query_series: 5000

  "tenant-light":
    ingestion_rate_mb: 5
    max_streams_per_user: 5000
    max_query_parallelism: 16
```

```yaml
# loki 설정에서 runtime-config 연결
runtime_config:
  file: /etc/loki/runtime-config.yaml
  period: 10s    # 10초마다 리로드
```

---

## 3. 모니터링

### 3.1 핵심 메트릭

**수집 경로:**

| 메트릭 | 설명 | 알림 기준 |
|--------|------|----------|
| `loki_distributor_bytes_received_total` | 수신 바이트 | 급격한 증감 |
| `loki_distributor_lines_received_total` | 수신 라인 수 | 급격한 증감 |
| `loki_ingester_memory_streams` | 인메모리 스트림 수 | 제한 근접 |
| `loki_ingester_chunks_flushed_total` | 플러시된 청크 | 0이면 문제 |
| `loki_ingester_wal_records_logged_total` | WAL 기록 수 | 디스크 I/O |
| `loki_discarded_samples_total` | 버려진 샘플 | > 0 지속 |

**쿼리 경로:**

| 메트릭 | 설명 | 알림 기준 |
|--------|------|----------|
| `loki_request_duration_seconds` | 요청 지연시간 | P99 > 30s |
| `loki_query_frontend_queue_length` | 프론트엔드 큐 길이 | 지속 증가 |
| `loki_querier_store_chunks_downloaded_total` | 다운로드 청크 수 | 과도한 증가 |
| `cortex_frontend_query_result_cache_hits_total` | 캐시 히트 | 히트율 < 50% |

**인프라:**

| 메트릭 | 설명 | 알림 기준 |
|--------|------|----------|
| `cortex_ring_members` | Ring 멤버 수 | 예상 값과 불일치 |
| `cortex_ring_member_heartbeat_failures_total` | 하트비트 실패 | > 0 |
| `loki_compactor_runs_completed_total` | 압축 실행 수 | 오랜 시간 0 |

### 3.2 Grafana 대시보드

Loki mixin 사용:

```bash
# 대시보드 + 알림 규칙 생성
cd production/loki-mixin
make build

# 결과:
#   loki-mixin-compiled/dashboards/*.json  → Grafana 대시보드
#   loki-mixin-compiled/alerts.yaml        → Prometheus 알림 규칙
#   loki-mixin-compiled/rules.yaml         → 녹화 규칙
```

### 3.3 헬스 체크 엔드포인트

| 엔드포인트 | 설명 |
|-----------|------|
| `GET /ready` | 모든 서비스 준비 완료 여부 (200/503) |
| `GET /services` | 서비스 상태 목록 |
| `GET /ring` | Ingester Ring 상태 (웹 UI) |
| `GET /config` | 현재 설정 확인 |
| `GET /metrics` | Prometheus 메트릭 |
| `GET /loki/api/v1/status/buildinfo` | 빌드 정보 |
| `GET /debug/fgprof` | 프로파일링 |

---

## 4. 스케일링 가이드

### 4.1 쓰기 경로 스케일링

```
문제: 수집 속도 증가
해결:

1. Distributor 수평 확장 (Stateless)
   └── 로드밸런서 뒤에 N개 배포

2. Ingester 수평 확장
   ├── Ring에 새 Ingester 추가
   ├── 자동 스트림 재분배 (토큰 리밸런싱)
   └── WAL 기반 안전한 스케일 아웃

3. 스트림 샤딩 활성화
   └── 대용량 스트림을 여러 Ingester에 분산
```

### 4.2 읽기 경로 스케일링

```
문제: 쿼리 지연/타임아웃
해결:

1. Querier 수평 확장 (Stateless)
   └── Query Scheduler가 자동 분배

2. Query Frontend 쿼리 분할
   ├── split_queries_by_interval: 15m → 더 작은 단위
   └── max_query_parallelism: 32 → 더 많은 병렬 실행

3. 캐시 도입
   ├── results_cache: Memcached/Redis
   ├── index_queries_cache: 인덱스 쿼리 캐시
   └── chunks_cache: 청크 캐시

4. Index Gateway 도입
   └── Querier의 인덱스 조회 부담 오프로드
```

### 4.3 스토리지 스케일링

```
문제: 스토리지 비용/성능
해결:

1. 압축 코덱 최적화
   └── snappy (빠름) → lz4 (균형) → zstd (최대 압축)

2. 보존 정책 설정
   ├── 글로벌: limits_config.retention_period
   └── 테넌트별: overrides.{tenant}.retention_period

3. Compactor 설정
   └── 인덱스 압축 주기, 삭제 배치 크기 조정
```

---

## 5. 트러블슈팅

### 5.1 수집 문제

**증상: 로그가 들어오지 않음**

```bash
# 1. Distributor 상태 확인
curl http://loki:3100/ready
curl http://loki:3100/services

# 2. Ring 상태 확인 (Ingester 등록 여부)
curl http://loki:3100/ring

# 3. 레이트 리밋 확인
curl -s http://loki:3100/metrics | grep loki_discarded_samples_total

# 4. Push 직접 테스트
curl -X POST http://loki:3100/loki/api/v1/push \
  -H "Content-Type: application/json" \
  -d '{"streams":[{"stream":{"app":"test"},"values":[["'$(date +%s)000000000'","test log"]]}]}'
```

**증상: "rate limit exceeded" 에러**

```yaml
# limits_config에서 수집 제한 조정
limits_config:
  ingestion_rate_mb: 20       # MB/s로 증가
  ingestion_burst_size_mb: 40  # 버스트 허용량 증가
  max_streams_per_user: 20000  # 스트림 수 증가
```

**증상: "entry too far behind" 에러**

```yaml
limits_config:
  reject_old_samples: true
  reject_old_samples_max_age: 336h  # 14일로 확장
  # 또는
  unordered_writes: true  # 비순서 쓰기 허용
```

### 5.2 쿼리 문제

**증상: 쿼리 타임아웃**

```yaml
# 1. 쿼리 시간 제한 조정
limits_config:
  query_timeout: 5m              # 쿼리 타임아웃 증가

# 2. 병렬 처리 증가
frontend:
  max_outstanding_per_tenant: 2048
  compress_responses: true

# 3. 쿼리 분할 조정
query_range:
  split_queries_by_interval: 15m
  parallelise_shardable_queries: true
```

**증상: "too many outstanding requests"**

```yaml
# 프론트엔드 큐 크기 확인
frontend:
  max_outstanding_per_tenant: 4096  # 증가
# 또는 Querier 수 증가
```

### 5.3 스토리지 문제

**증상: 플러시 실패**

```bash
# 1. 스토리지 접근 확인
curl -s http://loki:3100/metrics | grep loki_ingester_chunks_flushed

# 2. WAL 디스크 사용량 확인
du -sh /var/loki/wal/

# 3. 오브젝트 스토리지 연결 테스트
aws s3 ls s3://your-bucket/
```

**증상: 인덱스 쿼리 느림**

```yaml
# Index Gateway 도입
index_gateway:
  mode: ring
  ring:
    kvstore:
      store: memberlist

# 또는 캐시 추가
storage_config:
  index_queries_cache_config:
    memcached:
      host: memcached:11211
      service: memcached
```

---

## 6. 운영 체크리스트

### 6.1 일일 점검

- [ ] `/ready` 엔드포인트 200 확인
- [ ] Ring 멤버 수 정상 확인 (`/ring`)
- [ ] 수집 속도 정상 범위 (`loki_distributor_bytes_received_total`)
- [ ] 버려진 샘플 없음 (`loki_discarded_samples_total`)
- [ ] 쿼리 지연시간 정상 (`loki_request_duration_seconds`)

### 6.2 주간 점검

- [ ] WAL 디스크 사용량 확인
- [ ] 오브젝트 스토리지 사용량 확인
- [ ] Compactor 정상 실행 확인
- [ ] 캐시 히트율 확인
- [ ] 테넌트별 리소스 사용량 검토

### 6.3 Ingester 안전 종료

```bash
# 1. 사전 준비 (트래픽 제거 대기)
curl -X POST http://ingester:3100/ingester/prepare_shutdown

# 2. 로드밸런서에서 제거 대기 (30초 이상)
sleep 30

# 3. 종료 (자동으로 플러시 → Ring 해제)
kill -SIGTERM <pid>
# 또는
curl -X POST http://ingester:3100/ingester/shutdown
```

### 6.4 긴급 대응

```bash
# 플러시 강제 실행
curl -X POST http://ingester:3100/flush

# 쿼리 프론트엔드 큐 확인
curl http://frontend:3100/metrics | grep cortex_query_frontend_queue_length

# 설정 실시간 확인
curl http://loki:3100/config | jq .
```

---

## 7. 참고 자료

- 설정 레퍼런스: `pkg/loki/loki.go` (Config struct)
- Helm 차트: `production/helm/loki/`
- Docker Compose: `production/docker/`
- Mixin 대시보드: `production/loki-mixin/`
- 공식 문서: [grafana.com/docs/loki](https://grafana.com/docs/loki/latest/)
