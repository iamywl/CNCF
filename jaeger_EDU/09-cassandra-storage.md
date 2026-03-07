# 09. Jaeger Cassandra 스토리지 백엔드 Deep-Dive

## 목차

1. [Cassandra 개요 -- 왜 분산 트레이싱에 Cassandra인가](#1-cassandra-개요----왜-분산-트레이싱에-cassandra인가)
2. [Factory 계층 구조 (V1/V2)](#2-factory-계층-구조-v1v2)
3. [테이블 스키마 상세](#3-테이블-스키마-상세)
4. [Writer 구현 분석](#4-writer-구현-분석)
5. [Reader 구현 분석](#5-reader-구현-분석)
6. [Duration 쿼리 ADR (001) -- 아키텍처 결정 기록](#6-duration-쿼리-adr-001----아키텍처-결정-기록)
7. [설정 체계](#7-설정-체계)
8. [운영 고려사항](#8-운영-고려사항)

---

## 1. Cassandra 개요 -- 왜 분산 트레이싱에 Cassandra인가

### 1.1 분산 트레이싱 워크로드의 특성

분산 트레이싱 시스템은 다음과 같은 워크로드 특성을 가진다:

| 특성 | 설명 |
|------|------|
| **쓰기 집약적(Write-Heavy)** | 모든 서비스의 모든 요청에서 span이 생성되므로 초당 수만~수십만 건의 쓰기 발생 |
| **시계열 데이터(Time-Series)** | span 데이터는 시간순으로 생성되고, 일정 기간 후 만료(TTL) |
| **수평 확장(Horizontal Scaling)** | 트래픽 증가에 따라 노드를 추가하여 처리량을 선형적으로 확장해야 함 |
| **읽기 패턴이 제한적** | 대부분의 읽기는 trace_id 기반 단일 조회이거나 서비스/태그 기반 인덱스 검색 |

### 1.2 Cassandra가 적합한 이유

```
+--------------------------------------------------------------+
|                    Cassandra 특성과 트레이싱 매칭               |
+--------------------------------------------------------------+
|                                                              |
|  [높은 쓰기 처리량]                                           |
|  - LSM-Tree 기반 → 순차 쓰기                                 |
|  - 모든 노드가 쓰기 가능 (masterless)                         |
|  - span 쓰기가 즉시 커밋 로그에 기록                           |
|                                                              |
|  [시계열 데이터 최적화]                                        |
|  - TimeWindowCompactionStrategy (TWCS)                       |
|  - 기본 TTL로 자동 만료                                       |
|  - 시간 기반 파티셔닝으로 오래된 데이터 효율적 제거              |
|                                                              |
|  [수평 확장]                                                  |
|  - Consistent Hashing으로 데이터 자동 분산                     |
|  - 노드 추가 시 자동 리밸런싱                                  |
|  - DC-aware 복제로 멀티 리전 지원                              |
|                                                              |
|  [제한된 읽기 패턴]                                           |
|  - Partition Key 기반 단일 조회 → O(1)                        |
|  - Clustering Column 기반 범위 조회 최적화                     |
|  - 사전 정의된 쿼리 패턴에 최적화된 테이블 설계                  |
|                                                              |
+--------------------------------------------------------------+
```

### 1.3 Cassandra의 트레이드오프

Cassandra를 선택하면 다음과 같은 제약도 함께 따라온다:

- **서버 측 조인 불가**: 서로 다른 인덱스 테이블의 결과를 서버 측에서 교차(intersection)할 수 없다
- **Partition Key 등가 제약**: WHERE 절에서 partition key는 반드시 등가(=) 조건이어야 한다
- **비정규화(Denormalization) 필수**: 쿼리 패턴마다 별도의 인덱스 테이블이 필요하다

이러한 제약이 Jaeger의 Cassandra 스토리지 설계 전반에 영향을 미치며, 이후 섹션에서 구체적으로 살펴본다.

---

## 2. Factory 계층 구조 (V1/V2)

Jaeger의 Cassandra 스토리지는 V1(레거시)과 V2(현대적) 두 계층으로 구성된다. V2 Factory는 V1 Factory를 래핑하여 새로운 인터페이스를 제공하는 어댑터 패턴을 사용한다.

### 2.1 전체 아키텍처

```
+------------------------------------------------------------------+
|                        V2 Factory Layer                           |
|   internal/storage/v2/cassandra/factory.go                       |
|                                                                  |
|   +---------+  +-----------+  +----------------+  +-----------+  |
|   | Create  |  | Create    |  | Create         |  | Create    |  |
|   | Trace   |  | Trace     |  | Dependency     |  | Sampling  |  |
|   | Reader  |  | Writer    |  | Reader         |  | Store     |  |
|   +---------+  +-----------+  +----------------+  +-----------+  |
|        |             |              |                    |        |
+--------|-------------|--------------|--------------------|---------+
         |             |              |                    |
    v2 adapter    v1 adapter     v1 adapter           직접 위임
         |             |              |                    |
+--------|-------------|--------------|--------------------|---------+
|        v             v              v                    v        |
|                        V1 Factory Layer                           |
|   internal/storage/v1/cassandra/factory.go                       |
|                                                                  |
|   +---------+  +-----------+  +----------------+  +-----------+  |
|   | Span    |  | Span      |  | Dependency     |  | Sampling  |  |
|   | Reader  |  | Writer    |  | Store          |  | Store     |  |
|   +---------+  +-----------+  +----------------+  +-----------+  |
|        |             |              |                    |        |
|        +-------+-----+------+------+--------------------+        |
|                |            |                                    |
|           cassandra.Session (gocql)                              |
+------------------------------------------------------------------+
```

### 2.2 V2 Factory

V2 Factory는 `internal/storage/v2/cassandra/factory.go`에 정의되어 있다. 핵심 구조체는 V1 Factory를 내부에 포함한다:

```go
// internal/storage/v2/cassandra/factory.go (25-30행)
type Factory struct {
    metricsFactory metrics.Factory
    logger         *zap.Logger
    v1Factory      *cassandra.Factory    // V1 Factory를 래핑
    tracer         trace.TracerProvider
}
```

`CreateTraceReader()`에서는 V1의 `SpanReader`를 생성한 뒤 V2 트레이스 리더로 변환하고, 메트릭 데코레이터를 감싸는 과정을 거친다:

```go
// internal/storage/v2/cassandra/factory.go (47-61행)
func (f *Factory) CreateTraceReader() (tracestore.Reader, error) {
    corereader, err := cspanstore.NewSpanReader(
        f.v1Factory.GetSession(),
        f.metricsFactory,
        f.logger,
        f.tracer.Tracer("cSpanStore.SpanReader"),
    )
    if err != nil { return nil, err }
    return tracestoremetrics.NewReaderDecorator(
        ctracestore.NewTraceReader(corereader),
        f.metricsFactory,
    ), nil
}
```

`CreateTraceWriter()`는 V1의 `SpanWriter`를 생성한 뒤 `v1adapter.NewTraceWriter`로 감싸 V2 인터페이스를 제공한다:

```go
// internal/storage/v2/cassandra/factory.go (63-69행)
func (f *Factory) CreateTraceWriter() (tracestore.Writer, error) {
    writer, err := f.v1Factory.CreateSpanWriter()
    if err != nil { return nil, err }
    return v1adapter.NewTraceWriter(writer), nil
}
```

### 2.3 V1 Factory

V1 Factory는 `internal/storage/v1/cassandra/factory.go`에 정의되어 있다. 실제 Cassandra 세션을 관리하고 스토리지 객체를 생성하는 핵심 계층이다:

```go
// internal/storage/v1/cassandra/factory.go (41-54행)
type Factory struct {
    Options *Options
    metricsFactory metrics.Factory
    logger         *zap.Logger
    tracer         trace.TracerProvider
    config         config.Configuration
    session        cassandra.Session
    sessionBuilderFn func(*config.Configuration) (cassandra.Session, error)
}
```

SpanWriter 생성 시 태그 필터 옵션을 처리하는 `writerOptions` 함수가 중요하다:

```go
// internal/storage/v1/cassandra/factory.go (169-196행)
func writerOptions(opts *Options) ([]cspanstore.Option, error) {
    var tagFilters []dbmodel.TagFilter
    // drop all tag filters
    if !opts.Index.Tags || !opts.Index.ProcessTags || !opts.Index.Logs {
        tagFilters = append(tagFilters, dbmodel.NewTagFilterDropAll(
            !opts.Index.Tags, !opts.Index.ProcessTags, !opts.Index.Logs))
    }
    // black/white list tag filters
    tagIndexBlacklist := opts.TagIndexBlacklist()
    tagIndexWhitelist := opts.TagIndexWhitelist()
    if len(tagIndexBlacklist) > 0 && len(tagIndexWhitelist) > 0 {
        return nil, errors.New("only one of TagIndexBlacklist and TagIndexWhitelist can be specified")
    }
    // ... 블랙리스트 또는 화이트리스트 필터 적용
}
```

---

## 3. 테이블 스키마 상세

Jaeger의 Cassandra 스키마는 `internal/storage/v1/cassandra/schema/v004.cql.tmpl` (셸 템플릿) 및 `v004-go-tmpl.cql.tmpl` (Go 템플릿)에 정의되어 있다. 총 5개의 UDT(User-Defined Type)와 9개의 테이블로 구성된다.

### 3.1 UDT (User-Defined Types)

```sql
-- 키-값 쌍 (태그, 프로세스 태그, 로그 필드에 공통 사용)
CREATE TYPE IF NOT EXISTS keyvalue (
    key             text,
    value_type      text,       -- "string", "bool", "int64", "float64", "binary"
    value_string    text,
    value_bool      boolean,
    value_long      bigint,
    value_double    double,
    value_binary    blob
);

-- 로그 엔트리
CREATE TYPE IF NOT EXISTS log (
    ts      bigint,             -- 마이크로초 단위 epoch
    fields  frozen<list<frozen<keyvalue>>>
);

-- span 참조 (CHILD_OF, FOLLOWS_FROM)
CREATE TYPE IF NOT EXISTS span_ref (
    ref_type        text,       -- "child-of" 또는 "follows-from"
    trace_id        blob,
    span_id         bigint
);

-- 프로세스 정보
CREATE TYPE IF NOT EXISTS process (
    service_name    text,
    tags            frozen<list<frozen<keyvalue>>>
);
```

### 3.2 traces 테이블 -- 메인 span 저장소

```sql
CREATE TABLE IF NOT EXISTS traces (
    trace_id        blob,       -- 16바이트 TraceID (High + Low)
    span_id         bigint,
    span_hash       bigint,     -- Zipkin 호환용 (동일 ID span 허용)
    parent_id       bigint,
    operation_name  text,
    flags           int,
    start_time      bigint,     -- 마이크로초 단위 epoch
    duration        bigint,     -- 마이크로초 단위
    tags            list<frozen<keyvalue>>,
    logs            list<frozen<log>>,
    refs            list<frozen<span_ref>>,
    process         frozen<process>,
    PRIMARY KEY (trace_id, span_id, span_hash)
)
    WITH compaction = {
        'compaction_window_size': '...',
        'compaction_window_unit': 'MINUTES',
        'class': 'TimeWindowCompactionStrategy'
    }
    AND default_time_to_live = <trace_ttl>
    AND speculative_retry = 'NONE'
    AND gc_grace_seconds = 10800;
```

**파티셔닝 전략:**

```
traces 테이블 파티셔닝
=====================

Partition Key: trace_id
Clustering Columns: span_id, span_hash

+-- Partition: trace_id = 0xABCD1234... --+
|                                          |
|  span_id=1001, span_hash=5678           |
|    operation_name: "GET /api/users"      |
|    duration: 15000 (15ms)                |
|    tags: [{key:"http.status_code",...}]  |
|                                          |
|  span_id=1002, span_hash=9012           |
|    operation_name: "SELECT users"        |
|    duration: 8000 (8ms)                  |
|    tags: [{key:"db.type",...}]           |
|                                          |
|  span_id=1003, span_hash=3456           |
|    operation_name: "redis.GET"           |
|    duration: 1200 (1.2ms)               |
|                                          |
+------------------------------------------+
```

- **trace_id가 partition key**: 동일 trace의 모든 span이 하나의 파티션에 저장된다
- **span_hash의 존재 이유**: Zipkin과의 하위 호환성 때문이다. Zipkin은 동일 span_id를 가진 span을 허용하므로 span_hash를 추가하여 uniqueness를 보장한다
- **start_time이 timestamp가 아닌 bigint**: 마이크로초 정밀도가 필요하기 때문이다. Cassandra의 timestamp 타입은 밀리초 정밀도만 제공한다

### 3.3 service_name_index 테이블 -- 버킷 기반 서비스 조회

```sql
CREATE TABLE IF NOT EXISTS service_name_index (
    service_name      text,
    bucket            int,        -- 0~9 (span_hash % 10)
    start_time        bigint,     -- 마이크로초 단위 epoch
    trace_id          blob,
    PRIMARY KEY ((service_name, bucket), start_time)
) WITH CLUSTERING ORDER BY (start_time DESC)
```

**왜 버킷이 필요한가?**

인기 서비스(예: API Gateway)는 초당 수천 개의 span을 생성한다. partition key가 `service_name`만이라면 해당 파티션이 핫스팟이 된다. `bucket`을 추가하여 하나의 서비스에 대해 10개의 파티션으로 분산시킨다.

```
service_name_index 버킷 분산 전략
=================================

쓰기 시 (writer.go 245-251행):
  bucketNo = uint64(span.SpanHash) % 10    // defaultNumBuckets = 10

읽기 시 (reader.go 29행):
  bucketRange = (0,1,2,3,4,5,6,7,8,9)      // IN 절로 모든 버킷 조회

+-- Partition: ("api-gateway", bucket=0) --+
|  start_time=1709712000000000, trace_id=... |
|  start_time=1709711990000000, trace_id=... |
+--------------------------------------------+

+-- Partition: ("api-gateway", bucket=1) --+
|  start_time=1709712001000000, trace_id=... |
|  start_time=1709711995000000, trace_id=... |
+--------------------------------------------+

         ...  (bucket 2~8)

+-- Partition: ("api-gateway", bucket=9) --+
|  start_time=1709712003000000, trace_id=... |
|  start_time=1709711998000000, trace_id=... |
+--------------------------------------------+
```

쓰기 시 `span.SpanHash % 10`으로 버킷 번호를 결정하고, 읽기 시에는 `bucket IN (0,1,2,3,4,5,6,7,8,9)`로 모든 버킷을 동시에 조회한다:

```sql
-- reader.go의 queryByServiceName (40-45행)
SELECT trace_id
FROM service_name_index
WHERE bucket IN (0,1,2,3,4,5,6,7,8,9)
  AND service_name = ?
  AND start_time > ? AND start_time < ?
ORDER BY start_time DESC
LIMIT ?
```

### 3.4 service_operation_index 테이블 -- 서비스+오퍼레이션 조회

```sql
CREATE TABLE IF NOT EXISTS service_operation_index (
    service_name        text,
    operation_name      text,
    start_time          bigint,     -- 마이크로초 단위 epoch
    trace_id            blob,
    PRIMARY KEY ((service_name, operation_name), start_time)
) WITH CLUSTERING ORDER BY (start_time DESC)
```

이 테이블은 `(service_name, operation_name)` 복합 partition key를 사용한다. 서비스와 오퍼레이션 조합으로 충분히 카디널리티가 분산되므로 별도의 버킷이 필요 없다.

```sql
-- reader.go의 queryByServiceAndOperationName (46-51행)
SELECT trace_id
FROM service_operation_index
WHERE service_name = ? AND operation_name = ?
  AND start_time > ? AND start_time < ?
ORDER BY start_time DESC
LIMIT ?
```

### 3.5 tag_index 테이블 -- 태그 기반 검색

```sql
CREATE TABLE IF NOT EXISTS tag_index (
    service_name    text,
    tag_key         text,
    tag_value       text,
    start_time      bigint,     -- 마이크로초 단위 epoch
    trace_id        blob,
    span_id         bigint,
    PRIMARY KEY ((service_name, tag_key, tag_value), start_time, trace_id, span_id)
)
    WITH CLUSTERING ORDER BY (start_time DESC)
```

**파티셔닝 구조:**

```
tag_index 파티셔닝
=================

Partition Key: (service_name, tag_key, tag_value)

+-- Partition: ("user-service", "http.status_code", "200") --+
|  start_time=1709712000, trace_id=0xAA.., span_id=1001     |
|  start_time=1709711990, trace_id=0xBB.., span_id=2001     |
|  start_time=1709711980, trace_id=0xCC.., span_id=3001     |
+------------------------------------------------------------+

+-- Partition: ("user-service", "http.status_code", "500") --+
|  start_time=1709712005, trace_id=0xDD.., span_id=4001     |
|  start_time=1709711975, trace_id=0xEE.., span_id=5001     |
+------------------------------------------------------------+

+-- Partition: ("user-service", "error", "true") --------+
|  start_time=1709712005, trace_id=0xDD.., span_id=4001  |
+--------------------------------------------------------+
```

읽기 쿼리에서는 각 태그 키-값 쌍마다 별도의 CQL 쿼리를 실행하고, 결과를 클라이언트 측에서 교차(intersection)한다:

```sql
-- reader.go의 queryByTag (35-39행)
SELECT trace_id
FROM tag_index
WHERE service_name = ? AND tag_key = ? AND tag_value = ?
  AND start_time > ? AND start_time < ?
ORDER BY start_time DESC
LIMIT ?
```

### 3.6 duration_index 테이블 -- 시간 버킷 기반 duration 범위 검색

```sql
CREATE TABLE IF NOT EXISTS duration_index (
    service_name    text,       -- 서비스 이름
    operation_name  text,       -- 오퍼레이션 이름 (비어있으면 서비스 전체 검색)
    bucket          timestamp,  -- 시간 버킷 (시작 시간을 1시간 단위로 반올림)
    duration        bigint,     -- span duration (마이크로초)
    start_time      bigint,     -- 마이크로초 단위 epoch
    trace_id        blob,
    PRIMARY KEY ((service_name, operation_name, bucket), duration, start_time, trace_id)
) WITH CLUSTERING ORDER BY (duration DESC, start_time DESC)
```

**시간 버킷 전략:**

```
duration_index 시간 버킷 전략
============================

writer.go (57행):
  durationBucketSize = time.Hour

writer.go (231행):
  timeBucket = startTime.Round(durationBucketSize)

span 시작 시간이 14:37 → 버킷은 15:00 (Round)
span 시작 시간이 14:22 → 버킷은 14:00 (Round)

시간축:
12:00    13:00    14:00    15:00    16:00    17:00
  |--------|--------|--------|--------|--------|
  | bucket | bucket | bucket | bucket | bucket |
  |  12:00 |  13:00 |  14:00 |  15:00 |  16:00 |

각 span은 두 개의 엔트리로 인덱싱된다 (writer.go 240-241행):
  1. (service_name, "",            bucket) → 서비스 전체 duration 검색용
  2. (service_name, operationName, bucket) → 서비스+오퍼레이션 duration 검색용
```

읽기 시에는 시간 범위에 해당하는 모든 시간 버킷을 역순으로 순회한다:

```sql
-- reader.go의 queryByDuration (52-56행)
SELECT trace_id
FROM duration_index
WHERE bucket = ? AND service_name = ? AND operation_name = ?
  AND duration > ? AND duration < ?
LIMIT ?
```

### 3.7 보조 테이블 요약

| 테이블 | 용도 | Partition Key | Clustering |
|--------|------|---------------|------------|
| `service_names` | 서비스 이름 목록 | `service_name` | -- |
| `operation_names_v2` | 서비스별 오퍼레이션 목록 | `service_name` | `span_kind, operation_name` |
| `dependencies_v2` | 서비스 간 의존성 | `ts_bucket` | `ts DESC` |
| `operation_throughput` | 적응형 샘플링 | `bucket` | `ts DESC` |
| `sampling_probabilities` | 샘플링 확률 | `bucket` | `ts DESC` |
| `leases` | 분산 잠금 | `name` | -- |

---

## 4. Writer 구현 분석

### 4.1 SpanWriter 구조체

`internal/storage/v1/cassandra/spanstore/writer.go`에 정의된 `SpanWriter`가 Cassandra 쓰기의 핵심이다:

```go
// writer.go (80-90행)
type SpanWriter struct {
    session              cassandra.Session
    serviceNamesWriter   serviceNamesWriter      // service_names 테이블 쓰기
    operationNamesWriter operationNamesWriter    // operation_names_v2 테이블 쓰기
    writerMetrics        spanWriterMetrics       // 테이블별 메트릭
    logger               *zap.Logger
    tagIndexSkipped      metrics.Counter         // 필터링된 태그 수 카운터
    tagFilter            dbmodel.TagFilter       // 태그 필터 (블랙/화이트리스트)
    storageMode          storageMode             // 저장 모드 (store+index / store only / index only)
    indexFilter          dbmodel.IndexFilter     // 인덱스 필터
}
```

### 4.2 WriteSpan 흐름

```
WriteSpan 전체 흐름
===================

model.Span (도메인 모델)
    |
    v
dbmodel.FromDomain(span)        ← converter.go: 도메인 → DB 모델 변환
    |                               SpanHash 계산 포함
    v
+-- storageMode 확인 ---------------+
|                                    |
|  storeFlag 설정됨?                 |
|  → writeSpanToDB()                |
|     INSERT INTO traces(...)       |
|                                    |
|  indexFlag 설정됨?                 |
|  → writeIndexes()                 |
|     ├─ saveServiceNameAndOperationName()
|     ├─ indexByService()    [indexFilter 통과 시]
|     ├─ indexByOperation()  [indexFilter 통과 시]
|     ├─ [Firehose 플래그 → return] ← 비싼 인덱싱 생략
|     ├─ indexByTags()
|     └─ indexByDuration()   [indexFilter 통과 시]
|                                    |
+------------------------------------+
```

코드에서 `storageMode`는 비트 플래그로 구현된다:

```go
// writer.go (60-63행)
const (
    storeFlag = storageMode(1 << iota)   // 0b01 = 1
    indexFlag                             // 0b10 = 2
)
```

`WriteSpan` 메서드는 이 플래그를 비트 AND로 확인한다:

```go
// writer.go (133-146행)
func (s *SpanWriter) WriteSpan(_ context.Context, span *model.Span) error {
    ds := dbmodel.FromDomain(span)
    if s.storageMode&storeFlag == storeFlag {
        if err := s.writeSpanToDB(span, ds); err != nil {
            return err
        }
    }
    if s.storageMode&indexFlag == indexFlag {
        if err := s.writeIndexes(span, ds); err != nil {
            return err
        }
    }
    return nil
}
```

기본값은 `storeFlag | indexFlag` (둘 다 활성화)이다:

```go
// writer_options.go (57-58행)
if o.storageMode == 0 {
    o.storageMode = storeFlag | indexFlag
}
```

### 4.3 서비스 인덱스 버킷 계산

`indexByService`에서 span의 해시값을 기반으로 10개 버킷 중 하나에 배정한다:

```go
// writer.go (245-251행)
func (s *SpanWriter) indexByService(span *dbmodel.Span) error {
    bucketNo := uint64(span.SpanHash) % defaultNumBuckets   // defaultNumBuckets = 10
    query := s.session.Query(serviceNameIndex)
    q := query.Bind(span.Process.ServiceName, bucketNo, span.StartTime, span.TraceID)
    return s.writerMetrics.serviceNameIndex.Exec(q, s.logger)
}
```

`SpanHash`는 `converter.go`의 `fromDomain` 메서드에서 `model.HashCode(span)`으로 계산된다(53-59행). 이 해시는 span의 내용을 기반으로 결정론적으로 생성되므로, 동일 span은 항상 같은 버킷에 할당된다.

### 4.4 태그 인덱싱과 필터링

태그 인덱싱은 여러 단계의 필터링을 거친다:

```
태그 인덱싱 파이프라인
=====================

span.Tags + span.Process.Tags + span.Logs[].Fields
    |
    v
TagFilter (dbmodel.TagFilter 인터페이스)
    ├─ DefaultTagFilter: 모든 태그 통과
    ├─ TagFilterDropAll: 카테고리별 전체 드롭 (Tags/ProcessTags/Logs)
    ├─ ExactMatchTagFilter (Blacklist): 특정 키 제외
    └─ ExactMatchTagFilter (Whitelist): 특정 키만 포함
    |
    v
GetAllUniqueTags()
    ├─ 바이너리 타입 제외
    ├─ 중복 제거 (정렬 후 비교)
    └─ TagInsertion{ServiceName, TagKey, TagValue} 리스트 생성
    |
    v
shouldIndexTag() - writer.go 260-272행
    ├─ len(tag.TagKey) < 256 (maximumTagKeyOrValueSize)
    ├─ len(tag.TagValue) < 256
    ├─ utf8.ValidString(tag.TagValue)
    ├─ utf8.ValidString(tag.TagKey)
    └─ !isJSON(tag.TagValue)    ← JSON 값은 인덱싱 제외
    |
    v
INSERT INTO tag_index(...)
```

`shouldIndexTag`의 구체적인 필터링 로직:

```go
// writer.go (260-272행)
func (*SpanWriter) shouldIndexTag(tag dbmodel.TagInsertion) bool {
    isJSON := func(s string) bool {
        var js json.RawMessage
        return strings.HasPrefix(s, "{") && json.Unmarshal([]byte(s), &js) == nil
    }

    return len(tag.TagKey) < maximumTagKeyOrValueSize &&      // 256바이트 미만
        len(tag.TagValue) < maximumTagKeyOrValueSize &&        // 256바이트 미만
        utf8.ValidString(tag.TagValue) &&                      // 유효한 UTF-8
        utf8.ValidString(tag.TagKey) &&                        // 유효한 UTF-8
        !isJSON(tag.TagValue)                                  // JSON이 아닌 경우만
}
```

**왜 이러한 필터링이 필요한가?**

| 필터 조건 | 이유 |
|-----------|------|
| 256바이트 제한 | Cassandra partition key 크기가 커지면 성능 저하. 또한 태그 값이 너무 길면 검색 의미가 없음 |
| UTF-8 검증 | CQL 텍스트 컬럼은 유효한 UTF-8만 허용. 잘못된 인코딩은 쿼리 오류 유발 |
| JSON 제외 | JSON 값은 구조화된 데이터이므로 문자열 등가 비교로 검색하기 부적합. partition key 크기도 비대해짐 |
| 바이너리 제외 | `GetAllUniqueTags`에서 `BinaryType`은 건너뜀 (unique_tags.go 18-19행) |

### 4.5 Duration 인덱싱

각 span에 대해 두 개의 duration 인덱스 엔트리가 생성된다:

```go
// writer.go (229-243행)
func (s *SpanWriter) indexByDuration(span *dbmodel.Span, startTime time.Time) error {
    query := s.session.Query(durationIndex)
    timeBucket := startTime.Round(durationBucketSize)  // 1시간 단위 반올림
    var err error
    indexByOperationName := func(operationName string) {
        q1 := query.Bind(span.Process.ServiceName, operationName,
            timeBucket, span.Duration, span.StartTime, span.TraceID)
        if err2 := s.writerMetrics.durationIndex.Exec(q1, s.logger); err2 != nil {
            _ = s.logError(span, err2, "Cannot index duration", s.logger)
            err = err2
        }
    }
    indexByOperationName("")                 // 서비스 이름만으로 검색 가능하도록
    indexByOperationName(span.OperationName) // 서비스 + 오퍼레이션으로 검색 가능하도록
    return err
}
```

### 4.6 Firehose 모드

`Firehose` 플래그가 설정된 span은 비용이 높은 인덱싱(태그, duration)을 건너뛴다:

```go
// writer.go (193-195행)
if span.Flags.IsFirehoseEnabled() {
    return nil // skipping expensive indexing
}
```

이 모드는 매우 높은 처리량의 서비스에서 인덱싱 비용을 줄이기 위해 사용된다. service_name 인덱스와 operation 인덱스는 여전히 기록된다.

### 4.7 서비스/오퍼레이션 이름 캐싱

`ServiceNamesStorage`와 `OperationNamesStorage`는 LRU 캐시를 사용하여 중복 쓰기를 방지한다:

```go
// service_names.go (46-55행)
serviceNames: cache.NewLRUWithOptions(
    10000,                    // 최대 10000개 서비스 이름 캐싱
    &cache.Options{
        TTL:             writeCacheTTL,      // 기본 12시간
        InitialCapacity: 1000,
    }),
```

```go
// operation_names.go (113-120행)
operationNames: cache.NewLRUWithOptions(
    100000,                   // 최대 100000개 오퍼레이션 이름 캐싱
    &cache.Options{
        TTL:             writeCacheTTL,      // 기본 12시간
        InitialCapacity: 10000,
    }),
```

캐시 히트 시 Cassandra 쓰기를 생략한다:

```go
// service_names.go (59-70행)
func (s *ServiceNamesStorage) Write(serviceName string) error {
    var err error
    query := s.session.Query(s.InsertStmt)
    if inCache := checkWriteCache(serviceName, s.serviceNames, s.writeCacheTTL); !inCache {
        q := query.Bind(serviceName)
        err2 := s.metrics.Exec(q, s.logger)
        if err2 != nil { err = err2 }
    }
    return err
}
```

---

## 5. Reader 구현 분석

### 5.1 SpanReader 구조체

`internal/storage/v1/cassandra/spanstore/reader.go`에 정의된 SpanReader:

```go
// reader.go (107-114행)
type SpanReader struct {
    session              cassandra.Session
    serviceNamesReader   serviceNamesReader
    operationNamesReader operationNamesReader
    metrics              spanReaderMetrics
    logger               *zap.Logger
    tracer               trace.Tracer        // OpenTelemetry 트레이서
}
```

### 5.2 FindTraceIDs 쿼리 분기 로직

`FindTraceIDs`는 쿼리 파라미터에 따라 서로 다른 코드 경로를 택한다:

```
FindTraceIDs 분기 로직
=====================

TraceQueryParameters
    |
    v
validateQuery()
    ├─ ServiceName 필수 (태그 쿼리 시)
    ├─ StartTimeMin/Max 필수
    ├─ DurationMin <= DurationMax
    ├─ Duration과 Tags 동시 불가 → ErrDurationAndTagQueryNotSupported
    └─ pass
    |
    v
findTraceIDsFromQuery()   ← reader.go 292-320행
    |
    +-- DurationMin 또는 DurationMax 설정됨?
    |   YES → queryByDuration()          [별도 경로 -- ADR 001]
    |         hourly 버킷 순회
    |
    +-- OperationName 설정됨?
    |   YES → queryByServiceNameAndOperation()
    |         +-- Tags 설정됨?
    |         |   YES → queryByTagsAndLogs()
    |         |         → IntersectTraceIDs(operation결과, tag결과)
    |         |   NO  → operation 결과만 반환
    |
    +-- Tags 설정됨?
    |   YES → queryByTagsAndLogs()
    |         각 태그별 쿼리 → IntersectTraceIDs
    |
    +-- 그 외 (ServiceName만)
        → queryByService()
          bucket IN (0..9) 전체 스캔
```

실제 코드:

```go
// reader.go (292-320행)
func (s *SpanReader) findTraceIDsFromQuery(ctx context.Context,
    traceQuery *spanstore.TraceQueryParameters) (dbmodel.UniqueTraceIDs, error) {

    // Duration 쿼리는 별도 경로 (ADR 001)
    if traceQuery.DurationMin != 0 || traceQuery.DurationMax != 0 {
        return s.queryByDuration(ctx, traceQuery)
    }

    if traceQuery.OperationName != "" {
        traceIds, err := s.queryByServiceNameAndOperation(ctx, traceQuery)
        if err != nil { return nil, err }
        if len(traceQuery.Tags) > 0 {
            tagTraceIds, err := s.queryByTagsAndLogs(ctx, traceQuery)
            if err != nil { return nil, err }
            return dbmodel.IntersectTraceIDs([]dbmodel.UniqueTraceIDs{
                traceIds, tagTraceIds,
            }), nil
        }
        return traceIds, nil
    }

    if len(traceQuery.Tags) > 0 {
        return s.queryByTagsAndLogs(ctx, traceQuery)
    }
    return s.queryByService(ctx, traceQuery)
}
```

### 5.3 Duration 쿼리 구현

Duration 쿼리는 시간 범위에 해당하는 모든 hourly 버킷을 역순으로 순회한다:

```go
// reader.go (352-394행)
func (s *SpanReader) queryByDuration(ctx context.Context,
    traceQuery *spanstore.TraceQueryParameters) (dbmodel.UniqueTraceIDs, error) {

    results := dbmodel.UniqueTraceIDs{}

    minDurationMicros := traceQuery.DurationMin.Nanoseconds() /
        int64(time.Microsecond/time.Nanosecond)
    maxDurationMicros := (time.Hour * 24).Nanoseconds() /
        int64(time.Microsecond/time.Nanosecond)
    if traceQuery.DurationMax != 0 {
        maxDurationMicros = traceQuery.DurationMax.Nanoseconds() /
            int64(time.Microsecond/time.Nanosecond)
    }

    startTimeByHour := traceQuery.StartTimeMin.Round(durationBucketSize)
    endTimeByHour := traceQuery.StartTimeMax.Round(durationBucketSize)

    // 최신 버킷부터 과거 버킷까지 역순 순회
    for timeBucket := endTimeByHour;
        timeBucket.After(startTimeByHour) || timeBucket.Equal(startTimeByHour);
        timeBucket = timeBucket.Add(-1 * durationBucketSize) {

        query := s.session.Query(queryByDuration,
            timeBucket,
            traceQuery.ServiceName,
            traceQuery.OperationName,
            minDurationMicros,
            maxDurationMicros,
            traceQuery.NumTraces*limitMultiple)  // limitMultiple = 3

        t, err := s.executeQuery(childSpan, query, s.metrics.queryDurationIndex)
        // ... 결과 수집, NumTraces에 도달하면 조기 종료
    }
    return results, nil
}
```

**DurationMax 미지정 시**: 기본값으로 24시간(`time.Hour * 24`)을 사용한다. 이는 하루를 초과하는 span은 일반적으로 없다는 가정이다.

**limitMultiple = 3**: 인덱스에서 반환된 trace ID 중 중복이 많을 수 있으므로, 사용자가 요청한 수의 3배를 가져온다.

### 5.4 태그 교차(Intersection) 쿼리

여러 태그 조건이 주어지면 각 태그별로 별도 쿼리를 실행한 뒤 클라이언트 측에서 교차한다:

```go
// reader.go (322-350행)
func (s *SpanReader) queryByTagsAndLogs(ctx context.Context,
    tq *spanstore.TraceQueryParameters) (dbmodel.UniqueTraceIDs, error) {

    results := make([]dbmodel.UniqueTraceIDs, 0, len(tq.Tags))
    for k, v := range tq.Tags {
        query := s.session.Query(queryByTag,
            tq.ServiceName, k, v,
            model.TimeAsEpochMicroseconds(tq.StartTimeMin),
            model.TimeAsEpochMicroseconds(tq.StartTimeMax),
            tq.NumTraces*limitMultiple,
        ).PageSize(0)
        t, err := s.executeQuery(childSpan, query, s.metrics.queryTagIndex)
        if err != nil { return nil, err }
        results = append(results, t)
    }
    return dbmodel.IntersectTraceIDs(results), nil
}
```

`IntersectTraceIDs`의 교차 알고리즘은 첫 번째 결과 셋을 기준으로 나머지 셋에 모두 존재하는 trace ID만 유지한다:

```go
// unique_ids.go (25-40행)
func IntersectTraceIDs(uniqueTraceIdsList []UniqueTraceIDs) UniqueTraceIDs {
    retMe := UniqueTraceIDs{}
    for key, value := range uniqueTraceIdsList[0] {
        keyExistsInAll := true
        for _, otherTraceIds := range uniqueTraceIdsList[1:] {
            if _, ok := otherTraceIds[key]; !ok {
                keyExistsInAll = false
                break
            }
        }
        if keyExistsInAll {
            retMe[key] = value
        }
    }
    return retMe
}
```

### 5.5 GetTrace -- trace_id 기반 직접 조회

```go
// reader.go (176-221행)
func (s *SpanReader) readTraceInSpan(_ context.Context, traceID dbmodel.TraceID) (*model.Trace, error) {
    start := time.Now()
    q := s.session.Query(querySpanByTraceID, traceID)
    i := q.Iter()
    // ... Scan으로 모든 span 읽기
    retMe := &model.Trace{}
    for i.Scan(&traceIDFromSpan, &spanID, &parentID, ...) {
        dbSpan := dbmodel.Span{...}
        span, err := dbmodel.ToDomain(&dbSpan)
        retMe.Spans = append(retMe.Spans, span)
    }
    // ...
    if len(retMe.Spans) == 0 {
        return nil, spanstore.ErrTraceNotFound
    }
    return retMe, nil
}
```

이 쿼리는 partition key인 `trace_id`만으로 조회하므로 단일 파티션 스캔만 수행되어 매우 효율적이다.

---

## 6. Duration 쿼리 ADR (001) -- 아키텍처 결정 기록

Jaeger 프로젝트는 `docs/adr/001-cassandra-find-traces-duration.md`에 duration 쿼리의 설계 결정을 공식 문서화했다.

### 6.1 문제 정의

Cassandra의 `duration_index` 테이블은 다음과 같은 partition key 구조를 가진다:

```
Partition Key: (service_name, operation_name, bucket)
Clustering:    duration DESC, start_time DESC, trace_id
```

Cassandra에서 partition key는 반드시 등가(=) 조건이어야 한다. 따라서:

```sql
-- 가능: partition key에 등가 조건
WHERE service_name = 'svc-A' AND operation_name = 'op-B' AND bucket = '2024-03-06 14:00'
  AND duration > 1000 AND duration < 5000

-- 불가능: partition key에 범위 조건
WHERE service_name = 'svc-A' AND bucket > '2024-03-06 14:00'
  AND duration > 1000
```

### 6.2 왜 태그 인덱스와 교차할 수 없는가

```
Duration + Tag 교차의 비효율성
==============================

tag_index:
  Partition Key = (service_name, tag_key, tag_value)

duration_index:
  Partition Key = (service_name, operation_name, bucket)

교차하려면:
  1. duration_index에서 모든 hourly 버킷 순회 → trace_id 목록 A
  2. tag_index에서 각 태그별 쿼리 → trace_id 목록 B
  3. A ∩ B 클라이언트 측 교차

문제점:
  - hourly 버킷 수가 많을수록(24시간 = 24 파티션) 쿼리 폭증
  - 각 결과 셋이 커질수록 네트워크 전송량 폭증
  - 클라이언트 메모리 사용량 증가
```

### 6.3 결정 사항

ADR에 명시된 결정:

> "The Cassandra spanstore will continue to treat duration queries as a separate query path that does not intersect with tag indices or other non-service/operation filters."

Duration 쿼리가 활성화되면:
- `duration_index` 테이블만 사용
- `ServiceName`과 `OperationName`만 유효 (partition key 구성 요소)
- 태그 필터 및 기타 파라미터는 무시
- hourly 시간 버킷을 순회하여 결과 수집

이 제약은 검증 단계에서도 강제된다:

```go
// reader.go (244-246행)
if (p.DurationMin != 0 || p.DurationMax != 0) && len(p.Tags) > 0 {
    return ErrDurationAndTagQueryNotSupported
}
```

### 6.4 Badger와의 비교

ADR에서는 Badger 스토리지 백엔드와의 차이를 명시적으로 설명한다:

| 특성 | Cassandra | Badger |
|------|-----------|--------|
| 아키텍처 | 분산, 파티션 기반 | 임베디드 KV 스토어 |
| 인덱스 교차 | 불가능 (서버 측 조인 없음) | 가능 (메모리 내 hash-join) |
| Duration + Tag | 별도 경로, 태그 무시 | `hashOuter`로 교차 가능 |
| 범위 스캔 | 파티션 내에서만 효율적 | 전체 범위 스캔 가능 |
| 확장성 | 수평 확장 | 단일 노드 제한 |

### 6.5 사용자를 위한 우회 방법

Duration과 태그를 모두 필터링해야 하는 경우:

1. Duration 필터로 후보 trace ID 집합 획득
2. 해당 trace들을 GetTrace로 전체 조회
3. 애플리케이션 코드에서 태그 값으로 필터링

또는 Badger 백엔드를 사용하여 결합 쿼리를 지원받을 수 있다.

---

## 7. 설정 체계

### 7.1 설정 구조체 계층

설정은 `internal/storage/cassandra/config/config.go`에 정의된 `Configuration` 구조체를 중심으로 구성된다:

```go
// config.go (20-24행)
type Configuration struct {
    Schema     Schema     `mapstructure:"schema"`
    Connection Connection `mapstructure:"connection"`
    Query      Query      `mapstructure:"query"`
}
```

### 7.2 연결 설정 (Connection)

```go
// config.go (26-51행)
type Connection struct {
    Servers              []string          // Cassandra 서버 목록
    LocalDC              string            // DC-aware 라우팅용 로컬 DC 이름
    Port                 int               // 연결 포트
    DisableAutoDiscovery bool              // 클러스터 자동 탐색 비활성화
    ConnectionsPerHost   int               // 호스트당 최대 연결 수
    ReconnectInterval    time.Duration     // 재연결 주기
    SocketKeepAlive      time.Duration     // TCP Keep-Alive
    TLS                  configtls.ClientConfig  // TLS 설정
    Timeout              time.Duration     // 연결 타임아웃
    Authenticator        Authenticator     // 인증 정보
    ProtoVersion         int               // CQL 프로토콜 버전
}
```

### 7.3 스키마 설정 (Schema)

```go
// config.go (53-75행)
type Schema struct {
    Keyspace           string        // 키스페이스 이름
    DisableCompression bool          // Snappy 압축 비활성화 (Azure Cosmos DB 등)
    CreateSchema       bool          // 세션 초기화 시 스키마 자동 생성
    Datacenter         string        // 네트워크 토폴로지용 DC 이름
    TraceTTL           time.Duration // 트레이스 데이터 TTL
    DependenciesTTL    time.Duration // 의존성 데이터 TTL
    ReplicationFactor  int           // 복제 인수
    CompactionWindow   time.Duration // TWCS 컴팩션 윈도우 크기
}
```

### 7.4 쿼리 설정 (Query)

```go
// config.go (77-85행)
type Query struct {
    Timeout          time.Duration  // 쿼리 타임아웃
    MaxRetryAttempts int            // 최대 재시도 횟수
    Consistency      string         // 일관성 수준 (LocalOne, Quorum 등)
}
```

### 7.5 기본값

```go
// config.go (100-122행)
func DefaultConfiguration() Configuration {
    return Configuration{
        Schema: Schema{
            CreateSchema:      false,
            Keyspace:          "jaeger_dc1",
            Datacenter:        "dc1",
            TraceTTL:          2 * 24 * time.Hour,     // 2일
            DependenciesTTL:   2 * 24 * time.Hour,     // 2일
            ReplicationFactor: 1,
            CompactionWindow:  2 * time.Hour,           // 2시간
        },
        Connection: Connection{
            Servers:            []string{"127.0.0.1"},
            Port:               9042,
            ProtoVersion:       4,
            ConnectionsPerHost: 2,
            ReconnectInterval:  60 * time.Second,
        },
        Query: Query{
            MaxRetryAttempts: 3,
        },
    }
}
```

### 7.6 전체 설정 항목 요약

| 카테고리 | 설정 키 | 기본값 | 설명 |
|---------|---------|--------|------|
| **연결** | `connection.servers` | `["127.0.0.1"]` | Cassandra 호스트 목록 |
| | `connection.port` | `9042` | CQL 네이티브 프로토콜 포트 |
| | `connection.connections_per_host` | `2` | 호스트당 연결 수 |
| | `connection.proto_version` | `4` | CQL 프로토콜 버전 |
| | `connection.reconnect_interval` | `60s` | 재연결 간격 |
| | `connection.local_dc` | `""` | DC-aware 라우팅용 |
| | `connection.timeout` | `0` (무제한) | 연결 타임아웃 |
| | `connection.disable_auto_discovery` | `false` | 터널링 시 자동 탐색 비활성화 |
| **인증** | `connection.auth.basic.username` | `""` | 사용자 이름 |
| | `connection.auth.basic.password` | `""` | 비밀번호 |
| **TLS** | `connection.tls.insecure` | (기본값 없음) | TLS 비활성화 |
| **스키마** | `schema.keyspace` | `"jaeger_dc1"` | 키스페이스 이름 |
| | `schema.datacenter` | `"dc1"` | 데이터센터 이름 |
| | `schema.trace_ttl` | `48h` | 트레이스 TTL |
| | `schema.dependencies_ttl` | `48h` | 의존성 TTL |
| | `schema.replication_factor` | `1` | 복제 인수 |
| | `schema.compaction_window` | `2h` | 컴팩션 윈도우 |
| | `schema.create` | `false` | 자동 스키마 생성 |
| | `schema.disable_compression` | `false` | Snappy 압축 비활성화 |
| **쿼리** | `query.timeout` | `0` | 쿼리 타임아웃 |
| | `query.max_retry_attempts` | `3` | 최대 재시도 |
| | `query.consistency` | `"LocalOne"` | 일관성 수준 |
| **인덱스** | `index.tags` | `true` | span 태그 인덱싱 |
| | `index.process_tags` | `true` | 프로세스 태그 인덱싱 |
| | `index.logs` | `true` | 로그 필드 인덱싱 |
| | `index.tag_blacklist` | `""` | 인덱싱 제외 태그 (쉼표 구분) |
| | `index.tag_whitelist` | `""` | 인덱싱 허용 태그만 (쉼표 구분) |
| **기타** | `span_store_write_cache_ttl` | `12h` | 서비스/오퍼레이션 쓰기 캐시 TTL |

### 7.7 호스트 선택 정책

`NewCluster`에서 설정되는 호스트 선택 정책은 TokenAware + DC-aware 조합이다:

```go
// config.go (202-206행)
fallbackHostSelectionPolicy := gocql.RoundRobinHostPolicy()
if c.Connection.LocalDC != "" {
    fallbackHostSelectionPolicy = gocql.DCAwareRoundRobinPolicy(c.Connection.LocalDC)
}
cluster.PoolConfig.HostSelectionPolicy = gocql.TokenAwareHostPolicy(
    fallbackHostSelectionPolicy, gocql.ShuffleReplicas())
```

이 정책은 다음과 같이 동작한다:

```
호스트 선택 전략
===============

1. TokenAwareHostPolicy
   - partition key의 token을 기반으로 데이터가 있는 노드를 식별
   - 해당 노드로 직접 쿼리 → 네트워크 홉 최소화

2. ShuffleReplicas
   - 같은 데이터를 가진 여러 replica 중 무작위 선택
   - replica 간 부하 균등 분산

3. Fallback: DCAwareRoundRobinPolicy
   - local_dc가 설정되면 해당 DC의 노드를 우선 사용
   - 로컬 DC 노드 불가 시 원격 DC로 폴백
```

### 7.8 설정 검증

```go
// config.go (240-258행)
func (c *Configuration) Validate() error {
    _, err := govalidator.ValidateStruct(c)
    if err != nil { return err }

    if !isValidTTL(c.Schema.TraceTTL) {
        return errors.New("trace_ttl can either be 0 or greater than or equal to 1 second")
    }
    if !isValidTTL(c.Schema.DependenciesTTL) {
        return errors.New("dependencies_ttl can either be 0 or greater than or equal to 1 second")
    }
    if c.Schema.CompactionWindow < time.Minute {
        return errors.New("compaction_window should at least be 1 minute")
    }
    return nil
}
```

TTL은 0(무제한) 또는 1초 이상이어야 하고, 컴팩션 윈도우는 최소 1분이어야 한다.

---

## 8. 운영 고려사항

### 8.1 컴팩션 전략

Jaeger의 Cassandra 스키마는 두 가지 컴팩션 전략을 사용한다:

| 전략 | 적용 테이블 | 이유 |
|------|------------|------|
| **TimeWindowCompactionStrategy (TWCS)** | traces, service_name_index, service_operation_index, duration_index, tag_index | 시계열 데이터. TTL 만료 후 전체 SSTable 삭제 가능 |
| **SizeTieredCompactionStrategy (STCS)** | service_names, operation_names_v2, dependencies_v2 | 크기가 작고 갱신이 적은 참조 데이터 |

**TWCS의 동작 원리:**

```
TimeWindowCompactionStrategy
============================

시간축:
|--윈도우1--|--윈도우2--|--윈도우3--|--윈도우4--|
   SST-1      SST-2      SST-3      SST-4
   SST-5      SST-6

- 같은 시간 윈도우 내의 SSTable만 함께 컴팩션
- TTL 만료 시 전체 SSTable이 한번에 삭제 → 쓰기 증폭(Write Amplification) 최소화
- 윈도우 크기 권장: TTL / 30~50

예: TTL = 48h, compaction_window = 2h
  → 약 24개 윈도우 (48 / 2)
  → 권장 범위 내 (30~50개 미만)
```

스키마 템플릿에서의 설정:

```sql
-- 인덱스 테이블: 1시간 윈도우 고정
WITH compaction = {
    'compaction_window_size': '1',
    'compaction_window_unit': 'HOURS',
    'class': 'TimeWindowCompactionStrategy'
}

-- traces 테이블: 설정 가능 (기본 2시간 = 120분)
WITH compaction = {
    'compaction_window_size': '<CompactionWindowInMinutes>',
    'compaction_window_unit': 'MINUTES',
    'class': 'TimeWindowCompactionStrategy'
}
```

### 8.2 복제와 일관성 수준의 트레이드오프

```
복제 인수(RF)와 일관성 수준(CL) 조합
=====================================

강한 일관성 공식: R + W > RF

+----------+------------+----------+------------------------------------+
| RF       | Write CL   | Read CL  | 특성                               |
+----------+------------+----------+------------------------------------+
| 1        | ONE        | ONE      | 최소 내구성, 최대 성능               |
|          |            |          | 단일 노드 장애 = 데이터 손실          |
+----------+------------+----------+------------------------------------+
| 3        | ONE        | ONE      | 적절한 내구성, 높은 성능              |
| (권장)    |            |          | 1노드 장애 허용, 일시적 비일관성 가능  |
+----------+------------+----------+------------------------------------+
| 3        | QUORUM     | QUORUM   | 강한 일관성, 중간 성능               |
|          |            |          | 1노드 장애 허용                      |
+----------+------------+----------+------------------------------------+
| 3        | LOCAL_ONE  | LOCAL_ONE| 멀티DC: 로컬 DC 내 1개 replica 확인  |
|          |            |          | 비동기 원격 DC 복제                   |
+----------+------------+----------+------------------------------------+
```

Jaeger의 기본 일관성 수준은 `LocalOne`이다:

```go
// config.go (196-200행)
if c.Query.Consistency == "" {
    cluster.Consistency = gocql.LocalOne
} else {
    cluster.Consistency = gocql.ParseConsistency(c.Query.Consistency)
}
```

**트레이싱 데이터에 LocalOne이 적절한 이유:**
- 트레이스 데이터는 "best effort" 성격. 소량의 데이터 손실보다 가용성이 중요하다
- 쓰기 지연을 최소화하여 애플리케이션에 미치는 영향을 줄인다
- 강한 일관성이 필요한 비즈니스 데이터와 성격이 다르다

### 8.3 핫 파티션 방지

#### service_name_index의 버킷 전략

고트래픽 서비스의 service_name_index 파티션이 핫스팟이 되는 것을 방지하기 위해 10개 버킷을 사용한다:

```
핫 파티션 문제와 해결
====================

[버킷 없이 단일 파티션]

Partition: ("api-gateway")
  → 초당 10,000개 span 삽입
  → 단일 노드에 쓰기 집중
  → 노드 과부하, 쓰기 지연 증가

[10개 버킷으로 분산]

Partition: ("api-gateway", 0)  → 초당 ~1,000 span
Partition: ("api-gateway", 1)  → 초당 ~1,000 span
...
Partition: ("api-gateway", 9)  → 초당 ~1,000 span

  → 10개 파티션으로 분산 (서로 다른 노드에 배치될 수 있음)
  → 노드당 부하 감소
```

#### tag_index의 카디널리티

tag_index는 `(service_name, tag_key, tag_value)`가 partition key이므로, 카디널리티가 높은 태그(예: 고유 request_id)는 매우 많은 파티션을 생성한다. 이는 읽기 시 성능에는 영향이 적지만, 쓰기 부하와 스토리지를 증가시킨다.

스키마 주석에서도 이 점을 언급한다:

```sql
-- v004.cql.tmpl (157-158행)
-- a bucketing strategy may have to be added for tag queries
-- we can make this table even better by adding a timestamp to it
```

### 8.4 TTL과 gc_grace_seconds

모든 트레이스 관련 테이블은 `gc_grace_seconds = 10800` (3시간)으로 설정된다:

```sql
AND gc_grace_seconds = 10800; -- 3 hours of downtime acceptable on nodes
```

- `gc_grace_seconds`: 삭제된 데이터(tombstone)를 유지하는 시간
- 기본값(10일)보다 훨씬 짧게 설정하여 tombstone 축적을 방지
- 3시간은 "노드 다운타임이 3시간까지는 허용"한다는 의미
- 노드가 3시간 이상 다운되면 이미 삭제된 데이터가 다시 나타날 수 있음 (zombie data)

```
TTL과 gc_grace_seconds의 관계
============================

시간축:
|------- TTL (48h) --------|
                            |-- gc_grace (3h) --|
                            ^                    ^
                         데이터 만료          tombstone 제거
                         (읽기 불가)          (디스크 해제)

TWCS와의 시너지:
  - TWCS는 시간 윈도우 단위로 SSTable을 그룹화
  - 전체 SSTable의 모든 데이터가 TTL을 초과하면 SSTable 전체 삭제
  - tombstone 없이 전체 파일 삭제 → 가장 효율적
```

### 8.5 speculative_retry = 'NONE'

모든 테이블에서 `speculative_retry = 'NONE'`으로 설정된다:

```sql
AND speculative_retry = 'NONE'
```

이는 느린 응답에 대해 다른 replica에 재시도(speculative retry)를 보내지 않겠다는 의미다. 트레이싱 데이터는 지연보다 처리량이 중요하므로 불필요한 중복 요청을 방지한다.

### 8.6 스키마 자동 생성

`schema.create = true`로 설정하면 Factory 초기화 시 스키마를 자동으로 생성한다:

```go
// factory.go (100-115행)
func newSessionPrerequisites(c *config.Configuration) error {
    if !c.Schema.CreateSchema {
        return nil
    }
    cfg := *c          // 클론: keyspace 없이 연결
    cfg.Schema.Keyspace = ""
    session, err := createSession(&cfg)
    if err != nil { return err }

    sc := schema.NewSchemaCreator(session, c.Schema)
    return sc.CreateSchemaIfNotPresent()
}
```

`SchemaCreator`는 Go 템플릿(`v004-go-tmpl.cql.tmpl`)을 파라미터로 렌더링하여 CQL 문을 생성한다:

```go
// schema.go (47-59행)
func (sc *Creator) constructTemplateParams() templateParams {
    replicationConfig := fmt.Sprintf(
        "{'class': 'SimpleStrategy', 'replication_factor': '%d'}",
        sc.schema.ReplicationFactor)
    if sc.schema.Datacenter != "" {
        replicationConfig = fmt.Sprintf(
            "{'class': 'NetworkTopologyStrategy', '%s': '%d' }",
            sc.schema.Datacenter, sc.schema.ReplicationFactor)
    }
    return templateParams{
        Keyspace:                  sc.schema.Keyspace,
        Replication:               replicationConfig,
        CompactionWindowInMinutes: int64(sc.schema.CompactionWindow / time.Minute),
        TraceTTLInSeconds:         int64(sc.schema.TraceTTL / time.Second),
        DependenciesTTLInSeconds:  int64(sc.schema.DependenciesTTL / time.Second),
    }
}
```

---

## 소스코드 참조 요약

| 파일 | 경로 | 역할 |
|------|------|------|
| V2 Factory | `internal/storage/v2/cassandra/factory.go` | V2 인터페이스 어댑터 |
| V1 Factory | `internal/storage/v1/cassandra/factory.go` | 세션 관리, 스토리지 객체 생성 |
| Options | `internal/storage/v1/cassandra/options.go` | 인덱스 설정, 캐시 TTL |
| Config | `internal/storage/cassandra/config/config.go` | 연결/스키마/쿼리 설정 |
| SpanWriter | `internal/storage/v1/cassandra/spanstore/writer.go` | span 쓰기 및 인덱싱 |
| Writer Options | `internal/storage/v1/cassandra/spanstore/writer_options.go` | 쓰기 모드, 태그 필터 |
| SpanReader | `internal/storage/v1/cassandra/spanstore/reader.go` | span 읽기 및 검색 |
| DB Model | `internal/storage/v1/cassandra/spanstore/dbmodel/model.go` | DB 스키마 대응 구조체 |
| TraceID | `internal/storage/v1/cassandra/spanstore/dbmodel/ids.go` | 16바이트 TraceID 직렬화 |
| Converter | `internal/storage/v1/cassandra/spanstore/dbmodel/converter.go` | 도메인-DB 모델 변환 |
| Index Filter | `internal/storage/v1/cassandra/spanstore/dbmodel/index_filter.go` | 인덱스 종류별 필터 |
| Tag Filter | `internal/storage/v1/cassandra/spanstore/dbmodel/tag_filter.go` | 태그 필터 인터페이스/체인 |
| Tag Filter DropAll | `internal/storage/v1/cassandra/spanstore/dbmodel/tag_filter_drop_all.go` | 카테고리별 전체 드롭 |
| Tag Filter ExactMatch | `internal/storage/v1/cassandra/spanstore/dbmodel/tag_filter_exact_match.go` | 블랙/화이트리스트 |
| Unique Tags | `internal/storage/v1/cassandra/spanstore/dbmodel/unique_tags.go` | 중복 제거 태그 추출 |
| Unique IDs | `internal/storage/v1/cassandra/spanstore/dbmodel/unique_ids.go` | TraceID 교차 연산 |
| Service Names | `internal/storage/v1/cassandra/spanstore/service_names.go` | 서비스 이름 캐싱 저장소 |
| Operation Names | `internal/storage/v1/cassandra/spanstore/operation_names.go` | 오퍼레이션 이름 캐싱 저장소 |
| CQL 스키마 템플릿 | `internal/storage/v1/cassandra/schema/v004.cql.tmpl` | 셸 변수 기반 스키마 |
| Go 스키마 템플릿 | `internal/storage/v1/cassandra/schema/v004-go-tmpl.cql.tmpl` | Go 템플릿 기반 스키마 |
| Schema Creator | `internal/storage/v1/cassandra/schema/schema.go` | 스키마 자동 생성 |
| ADR 001 | `docs/adr/001-cassandra-find-traces-duration.md` | Duration 쿼리 설계 결정 |
