# ClickHouse 스토리지

## 목차

1. [개요](#1-개요)
2. [왜 ClickHouse인가](#2-왜-clickhouse인가)
3. [Factory 아키텍처](#3-factory-아키텍처)
4. [설정 구조 (Configuration)](#4-설정-구조-configuration)
5. [스키마 구조](#5-스키마-구조)
6. [spans 테이블 상세](#6-spans-테이블-상세)
7. [Materialized View 시스템](#7-materialized-view-시스템)
8. [SpanRow 데이터 모델](#8-spanrow-데이터-모델)
9. [속성(Attributes) 저장 전략](#9-속성attributes-저장-전략)
10. [Writer: 배치 삽입](#10-writer-배치-삽입)
11. [ToRow: OTLP에서 SpanRow로 변환](#11-torow-otlp에서-spanrow로-변환)
12. [FromRow: SpanRow에서 OTLP로 변환](#12-fromrow-spanrow에서-otlp로-변환)
13. [Reader: 트레이스 조회](#13-reader-트레이스-조회)
14. [Reader: 서비스/오퍼레이션 조회](#14-reader-서비스오퍼레이션-조회)
15. [FindTraces: 복합 쿼리 빌더](#15-findtraces-복합-쿼리-빌더)
16. [Attribute Metadata와 타입 추론](#16-attribute-metadata와-타입-추론)
17. [Purge: 전체 데이터 삭제](#17-purge-전체-데이터-삭제)
18. [아키텍처 다이어그램 종합](#18-아키텍처-다이어그램-종합)

---

## 1. 개요

Jaeger의 ClickHouse 스토리지 백엔드는 비교적 최근에 추가된 네이티브 v2 구현이다. Elasticsearch와 달리 v1 호환 레이어 없이 처음부터 OpenTelemetry 데이터 모델(OTLP)을 직접 지원하도록 설계되었다.

소스코드 구조:

```
internal/storage/v2/clickhouse/
├── factory.go                    # Factory: 연결, 스키마 생성, Reader/Writer 생성
├── config.go                     # Configuration 구조체
├── sql/
│   ├── queries.go                # SQL 쿼리 상수 + 임베디드 SQL 파일
│   ├── create_spans_table.sql    # spans 테이블 DDL
│   ├── create_services_table.sql
│   ├── create_services_mv.sql
│   ├── create_operations_table.sql
│   ├── create_operations_mv.sql
│   ├── create_trace_id_timestamps_table.sql
│   ├── create_trace_id_timestamps_mv.sql
│   ├── create_attribute_metadata_table.sql
│   ├── create_attribute_metadata_mv.sql
│   ├── create_event_attribute_metadata_mv.sql
│   └── create_link_attribute_metadata_mv.sql
├── tracestore/
│   ├── writer.go                 # Writer: 배치 삽입
│   ├── reader.go                 # Reader: GetTraces, FindTraces, GetServices 등
│   ├── query_builder.go          # 동적 SQL 쿼리 빌드
│   ├── attribute_metadata.go     # 속성 메타데이터 조회
│   └── dbmodel/
│       ├── spanrow.go            # SpanRow 구조체 + ScanRow
│       ├── to.go                 # OTLP → SpanRow 변환
│       ├── from.go               # SpanRow → OTLP 변환
│       ├── service.go            # Service 구조체
│       ├── operation.go          # Operation 구조체
│       └── attribute_metadata.go # AttributeMetadata 구조체
└── depstore/
    └── reader.go                 # 의존성 Reader (아직 미구현)
```

---

## 2. 왜 ClickHouse인가

ClickHouse는 칼럼 지향(column-oriented) OLAP 데이터베이스로, 트레이싱 데이터에 특히 적합한 특성을 가진다:

| 특성 | 트레이싱에서의 이점 |
|------|-------------------|
| 칼럼 지향 저장 | 특정 필드만 읽는 집계 쿼리(서비스 목록, 오퍼레이션 목록)가 빠르다 |
| 높은 압축률 | trace_id, service_name 같은 반복 값이 잘 압축된다 |
| 벡터화 처리 | 대량 span 스캔 시 CPU 효율이 높다 |
| MergeTree 엔진 | 시간 기반 파티셔닝과 정렬이 자연스럽다 |
| Nested 타입 | 가변 속성(attributes)을 효율적으로 저장할 수 있다 |
| Materialized View | 삽입 시 자동으로 서비스/오퍼레이션 메타데이터를 갱신한다 |

Elasticsearch 대비 주요 차이:

```
Elasticsearch:
  - JSON 문서 단위 저장
  - 풀텍스트 검색에 강점
  - 인덱스 롤오버/ILM 관리 필요
  - v1 호환 레이어 + v2 래퍼

ClickHouse:
  - 칼럼 단위 저장
  - 분석/집계 쿼리에 강점
  - 파티셔닝이 자동 (toDate(start_time))
  - 네이티브 v2 (OTLP 직접 지원)
```

---

## 3. Factory 아키텍처

`internal/storage/v2/clickhouse/factory.go`에 정의된 Factory는 네 가지 인터페이스를 구현한다.

```go
// internal/storage/v2/clickhouse/factory.go
var (
    _ io.Closer          = (*Factory)(nil)
    _ depstore.Factory   = (*Factory)(nil)
    _ tracestore.Factory = (*Factory)(nil)
    _ storage.Purger     = (*Factory)(nil)
)

type Factory struct {
    config Configuration
    telset telemetry.Settings
    conn   driver.Conn  // ClickHouse 드라이버 연결
}
```

### 초기화 흐름

```go
// internal/storage/v2/clickhouse/factory.go
func NewFactory(ctx context.Context, cfg Configuration, telset telemetry.Settings) (*Factory, error) {
    cfg.applyDefaults()
    f := &Factory{config: cfg, telset: telset}

    // 1. ClickHouse 연결 옵션 구성
    opts := &clickhouse.Options{
        Protocol: getProtocol(f.config.Protocol),  // native 또는 http
        Addr:     f.config.Addresses,
        Auth: clickhouse.Auth{
            Database: f.config.Database,
        },
        DialTimeout: f.config.DialTimeout,
    }
    // Basic Auth 설정
    basicAuth := f.config.Auth.Basic.Get()
    if basicAuth != nil {
        opts.Auth.Username = basicAuth.Username
        opts.Auth.Password = string(basicAuth.Password)
    }

    // 2. 연결 및 핑 테스트
    conn, err := clickhouse.Open(opts)
    conn.Ping(ctx)

    // 3. 스키마 생성 (create_schema=true일 때)
    if f.config.CreateSchema {
        // 11개 테이블/뷰를 순서대로 생성
        schemas := []struct{ name, query string }{
            {"spans table", sql.CreateSpansTable},
            {"services table", sql.CreateServicesTable},
            {"services materialized view", sql.CreateServicesMaterializedView},
            {"operations table", sql.CreateOperationsTable},
            {"operations materialized view", sql.CreateOperationsMaterializedView},
            {"trace id timestamps table", sql.CreateTraceIDTimestampsTable},
            {"trace id timestamps materialized view", sql.CreateTraceIDTimestampsMaterializedView},
            {"attribute metadata table", sql.CreateAttributeMetadataTable},
            {"attribute metadata materialized view", sql.CreateAttributeMetadataMaterializedView},
            {"event attribute metadata materialized view", sql.CreateEventAttributeMetadataMaterializedView},
            {"link attribute metadata materialized view", sql.CreateLinkAttributeMetadataMaterializedView},
        }
        for _, schema := range schemas {
            conn.Exec(ctx, schema.query)
        }
    }
    f.conn = conn
    return f, nil
}
```

### 프로토콜 선택

```go
// internal/storage/v2/clickhouse/factory.go
func getProtocol(protocol string) clickhouse.Protocol {
    if protocol == "http" {
        return clickhouse.HTTP
    }
    return clickhouse.Native  // 기본값
}
```

Native 프로토콜은 바이너리 포맷으로 더 효율적이고, HTTP 프로토콜은 프록시 환경에서 유리하다.

---

## 4. 설정 구조 (Configuration)

```go
// internal/storage/v2/clickhouse/config.go
type Configuration struct {
    Protocol           string        // "native" (기본) 또는 "http"
    Addresses          []string      // ClickHouse 서버 주소
    Database           string        // 데이터베이스 이름 (기본: "jaeger")
    Auth               Authentication
    DialTimeout        time.Duration // 연결 타임아웃
    CreateSchema       bool          // 스키마 자동 생성 여부
    DefaultSearchDepth int           // 기본 검색 깊이 (기본: 1000)
    MaxSearchDepth     int           // 최대 검색 깊이 (기본: 10000)
}

const (
    defaultProtocol       = "native"
    defaultDatabase       = "jaeger"
    defaultSearchDepth    = 1000
    defaultMaxSearchDepth = 10000
)
```

### 기본값 적용

```go
// internal/storage/v2/clickhouse/config.go
func (cfg *Configuration) applyDefaults() {
    if cfg.Protocol == "" {
        cfg.Protocol = "native"
    }
    if cfg.Database == "" {
        cfg.Database = defaultDatabase  // "jaeger"
    }
    if cfg.DefaultSearchDepth == 0 {
        cfg.DefaultSearchDepth = defaultSearchDepth  // 1000
    }
    if cfg.MaxSearchDepth == 0 {
        cfg.MaxSearchDepth = defaultMaxSearchDepth  // 10000
    }
}
```

---

## 5. 스키마 구조

ClickHouse 스키마는 5개의 테이블과 6개의 Materialized View로 구성된다.

### 테이블/뷰 목록

| # | 유형 | 이름 | 엔진 | 용도 |
|---|------|------|------|------|
| 1 | 테이블 | `spans` | MergeTree | 스팬 데이터 메인 테이블 |
| 2 | 테이블 | `services` | AggregatingMergeTree | 서비스 이름 목록 |
| 3 | MV | `services_mv` | -- | spans → services 자동 갱신 |
| 4 | 테이블 | `operations` | AggregatingMergeTree | 오퍼레이션 목록 |
| 5 | MV | `operations_mv` | -- | spans → operations 자동 갱신 |
| 6 | 테이블 | `trace_id_timestamps` | MergeTree | 트레이스별 시작/종료 시간 |
| 7 | MV | `trace_id_timestamps_mv` | -- | spans → trace_id_timestamps |
| 8 | 테이블 | `attribute_metadata` | AggregatingMergeTree | 속성 키/타입/레벨 메타데이터 |
| 9 | MV | `attribute_metadata_mv` | -- | spans → attribute_metadata (span 레벨) |
| 10 | MV | `event_attribute_metadata_mv` | -- | spans events → attribute_metadata |
| 11 | MV | `link_attribute_metadata_mv` | -- | spans links → attribute_metadata |

### 스키마 관계도

```
                        ┌──────────────────────┐
                        │      spans           │
                        │  (MergeTree)         │
                        │  PARTITION BY        │
                        │  toDate(start_time)  │
                        │  ORDER BY (trace_id) │
                        └──────┬───────────────┘
                               │ INSERT 트리거
              ┌────────────────┼────────────────┬─────────────────┐
              ▼                ▼                ▼                 ▼
   ┌──────────────┐  ┌─────────────────┐  ┌──────────────┐  ┌───────────────────┐
   │ services_mv  │  │ operations_mv   │  │ trace_id_    │  │ attribute_        │
   │      │       │  │       │         │  │ timestamps_mv│  │ metadata_mv (x3)  │
   │      ▼       │  │       ▼         │  │      │       │  │      │            │
   │  services    │  │  operations     │  │      ▼       │  │      ▼            │
   │  (Agg.MT)    │  │  (Agg.MT)       │  │  trace_id_   │  │  attribute_       │
   │              │  │                  │  │  timestamps  │  │  metadata         │
   └──────────────┘  └─────────────────┘  │  (MergeTree) │  │  (Agg.MT)         │
                                          └──────────────┘  └───────────────────┘
```

---

## 6. spans 테이블 상세

`internal/storage/v2/clickhouse/sql/create_spans_table.sql`:

```sql
CREATE TABLE IF NOT EXISTS spans (
    -- 스팬 기본 필드
    id String,
    trace_id String,
    trace_state String,
    parent_span_id String,
    name String,
    kind String,
    start_time DateTime64(9),        -- 나노초 정밀도
    status_code String,
    status_message String,
    duration Int64,                   -- 나노초 단위

    -- 스팬 속성 (타입별 Nested)
    bool_attributes Nested (key String, value Bool),
    double_attributes Nested (key String, value Float64),
    int_attributes Nested (key String, value Int64),
    str_attributes Nested (key String, value String),
    complex_attributes Nested (key String, value String),

    -- 이벤트 (이중 Nested)
    events Nested (
        name String,
        timestamp DateTime64(9),
        bool_attributes Nested (key String, value Bool),
        double_attributes Nested (key String, value Float64),
        int_attributes Nested (key String, value Int64),
        str_attributes Nested (key String, value String),
        complex_attributes Nested (key String, value String)
    ),

    -- 링크 (이중 Nested)
    links Nested (
        trace_id String,
        span_id String,
        trace_state String,
        bool_attributes Nested (key String, value Bool),
        double_attributes Nested (key String, value Float64),
        int_attributes Nested (key String, value Int64),
        str_attributes Nested (key String, value String),
        complex_attributes Nested (key String, value String)
    ),

    -- 리소스
    service_name String,
    resource_bool_attributes Nested (key String, value Bool),
    resource_double_attributes Nested (key String, value Float64),
    resource_int_attributes Nested (key String, value Int64),
    resource_str_attributes Nested (key String, value String),
    resource_complex_attributes Nested (key String, value String),

    -- 스코프
    scope_name String,
    scope_version String,
    scope_bool_attributes Nested (key String, value Bool),
    scope_double_attributes Nested (key String, value Float64),
    scope_int_attributes Nested (key String, value Int64),
    scope_str_attributes Nested (key String, value String),
    scope_complex_attributes Nested (key String, value String),

    -- 인덱스
    INDEX idx_service_name service_name TYPE set(500) GRANULARITY 1,
    INDEX idx_name name TYPE set(1000) GRANULARITY 1,
    INDEX idx_start_time start_time TYPE minmax GRANULARITY 1,
    INDEX idx_duration duration TYPE minmax GRANULARITY 1,
    INDEX idx_attributes_keys str_attributes.key TYPE bloom_filter GRANULARITY 1,
    INDEX idx_attributes_values str_attributes.value TYPE bloom_filter GRANULARITY 1,
    INDEX idx_resource_attributes_keys resource_str_attributes.key TYPE bloom_filter GRANULARITY 1,
    INDEX idx_resource_attributes_values resource_str_attributes.value TYPE bloom_filter GRANULARITY 1,
) ENGINE = MergeTree
PARTITION BY toDate(start_time)
ORDER BY (trace_id)
```

### 인덱스 전략

| 인덱스 | 타입 | 대상 | 목적 |
|--------|------|------|------|
| `idx_service_name` | set(500) | service_name | 서비스 필터링 가속 |
| `idx_name` | set(1000) | name (operation) | 오퍼레이션 필터링 가속 |
| `idx_start_time` | minmax | start_time | 시간 범위 쿼리 최적화 |
| `idx_duration` | minmax | duration | 지속시간 범위 쿼리 최적화 |
| `idx_attributes_keys` | bloom_filter | str_attributes.key | 속성 키 존재 여부 빠른 확인 |
| `idx_attributes_values` | bloom_filter | str_attributes.value | 속성 값 존재 여부 빠른 확인 |

### 파티셔닝과 정렬

- **PARTITION BY toDate(start_time)**: 일 단위 파티셔닝으로 시간 범위 쿼리 시 불필요한 파티션을 건너뛸 수 있다
- **ORDER BY (trace_id)**: 같은 트레이스의 span들이 물리적으로 인접하여 트레이스 단위 조회가 빠르다

---

## 7. Materialized View 시스템

### services_mv

```sql
-- create_services_mv.sql
CREATE MATERIALIZED VIEW IF NOT EXISTS services_mv TO services AS
SELECT
    service_name AS name
FROM spans
GROUP BY service_name;
```

span이 삽입될 때마다 자동으로 `services` 테이블에 서비스 이름이 추가된다.

### operations_mv

```sql
-- create_operations_mv.sql
CREATE MATERIALIZED VIEW IF NOT EXISTS operations_mv TO operations AS
SELECT
    name,
    kind AS span_kind,
    service_name
FROM spans;
```

span이 삽입될 때마다 오퍼레이션 이름, span kind, 서비스 이름이 `operations` 테이블에 추가된다.

### 보조 테이블 DDL

```sql
-- services 테이블
CREATE TABLE IF NOT EXISTS services (
    name String
) ENGINE = AggregatingMergeTree
ORDER BY (name);

-- operations 테이블
CREATE TABLE IF NOT EXISTS operations (
    service_name String,
    name String,
    span_kind String
) ENGINE = AggregatingMergeTree
ORDER BY (service_name, span_kind);

-- trace_id_timestamps 테이블
CREATE TABLE IF NOT EXISTS trace_id_timestamps (
    trace_id String,
    start DateTime64(9),
    end DateTime64(9)
) ENGINE = MergeTree()
ORDER BY (trace_id);

-- attribute_metadata 테이블
CREATE TABLE IF NOT EXISTS attribute_metadata (
    attribute_key String,
    type String,    -- 'bool', 'double', 'int', 'str', 'bytes', 'map', 'slice'
    level String    -- 'resource', 'scope', 'span'
) ENGINE = AggregatingMergeTree
ORDER BY (attribute_key, type, level);
```

AggregatingMergeTree 엔진은 백그라운드 머지 시 중복을 자동으로 집계하여 메타데이터 테이블의 크기를 억제한다.

---

## 8. SpanRow 데이터 모델

`internal/storage/v2/clickhouse/tracestore/dbmodel/spanrow.go`에 정의된 `SpanRow`는 ClickHouse의 한 행을 나타낸다.

```go
// internal/storage/v2/clickhouse/tracestore/dbmodel/spanrow.go
type SpanRow struct {
    // --- Span 기본 필드 ---
    ID              string
    TraceID         string
    TraceState      string
    ParentSpanID    string
    Name            string
    Kind            string
    StartTime       time.Time
    StatusCode      string
    StatusMessage   string
    Duration        int64         // 나노초

    // --- Span 속성 ---
    Attributes      Attributes

    // --- 이벤트 ---
    EventNames      []string
    EventTimestamps []time.Time
    EventAttributes Attributes2D  // 2차원 속성 (이벤트별)

    // --- 링크 ---
    LinkTraceIDs    []string
    LinkSpanIDs     []string
    LinkTraceStates []string
    LinkAttributes  Attributes2D  // 2차원 속성 (링크별)

    // --- 리소스 ---
    ServiceName        string
    ResourceAttributes Attributes

    // --- 스코프 ---
    ScopeName       string
    ScopeVersion    string
    ScopeAttributes Attributes
}
```

### Attributes 구조

```go
// internal/storage/v2/clickhouse/tracestore/dbmodel/spanrow.go
type Attributes struct {
    BoolKeys      []string
    BoolValues    []bool
    DoubleKeys    []string
    DoubleValues  []float64
    IntKeys       []string
    IntValues     []int64
    StrKeys       []string
    StrValues     []string
    ComplexKeys   []string    // @bytes@, @slice@, @map@ 접두어
    ComplexValues []string    // base64 또는 JSON 인코딩
}
```

타입별 key-value 쌍을 별도 배열로 저장한다. 이 설계는 ClickHouse의 Nested 타입과 정확히 일치한다.

### Attributes2D 구조

이벤트와 링크는 여러 개가 있고, 각각 자체 속성을 가지므로 2차원 배열이 필요하다:

```go
type Attributes2D struct {
    BoolKeys      [][]string   // [이벤트/링크 인덱스][속성 인덱스]
    BoolValues    [][]bool
    DoubleKeys    [][]string
    DoubleValues  [][]float64
    IntKeys       [][]string
    IntValues     [][]int64
    StrKeys       [][]string
    StrValues     [][]string
    ComplexKeys   [][]string
    ComplexValues [][]string
}
```

### ScanRow: 행 읽기

`ScanRow` 함수는 ClickHouse 드라이버의 `rows.Scan()`을 호출하여 57개 칼럼을 SpanRow로 매핑한다:

```go
// internal/storage/v2/clickhouse/tracestore/dbmodel/spanrow.go
func ScanRow(rows driver.Rows) (*SpanRow, error) {
    var sr SpanRow
    err := rows.Scan(
        &sr.ID, &sr.TraceID, &sr.TraceState, &sr.ParentSpanID,
        &sr.Name, &sr.Kind, &sr.StartTime, &sr.StatusCode,
        &sr.StatusMessage, &sr.Duration,
        &sr.Attributes.BoolKeys, &sr.Attributes.BoolValues,
        // ... 총 57개 칼럼 ...
        &sr.ScopeAttributes.ComplexKeys, &sr.ScopeAttributes.ComplexValues,
    )
    return &sr, err
}
```

---

## 9. 속성(Attributes) 저장 전략

### Complex Attributes

OTLP의 비원시 타입(bytes, slice, map)은 문자열로 직렬화되어 `complex_attributes`에 저장된다.

```go
// internal/storage/v2/clickhouse/tracestore/dbmodel/spanrow.go 주석
// - pcommon.ValueTypeBytes:
//     Base64 인코딩, 키 접두어 `@bytes@`
// - pcommon.ValueTypeSlice:
//     JSON 직렬화, 키 접두어 `@slice@`
// - pcommon.ValueTypeMap:
//     JSON 직렬화, 키 접두어 `@map@`
```

### 타입별 저장 예시

| OTLP 타입 | ClickHouse 칼럼 | 키 | 값 |
|-----------|----------------|-----|-----|
| Bool | `bool_attributes` | `"enabled"` | `true` |
| Double | `double_attributes` | `"latency"` | `1.5` |
| Int | `int_attributes` | `"http.status_code"` | `200` |
| String | `str_attributes` | `"http.method"` | `"GET"` |
| Bytes | `complex_attributes` | `"@bytes@data"` | `"aGVsbG8="` (base64) |
| Slice | `complex_attributes` | `"@slice@tags"` | `[1, 2, 3]` (JSON) |
| Map | `complex_attributes` | `"@map@headers"` | `{"a":"b"}` (JSON) |

### 타입 분류 로직

```go
// internal/storage/v2/clickhouse/tracestore/dbmodel/to.go
func extractAttributes(attrs pcommon.Map) *Attributes {
    out := &Attributes{}
    attrs.Range(func(k string, v pcommon.Value) bool {
        switch v.Type() {
        case pcommon.ValueTypeBool:
            out.BoolKeys = append(out.BoolKeys, k)
            out.BoolValues = append(out.BoolValues, v.Bool())
        case pcommon.ValueTypeDouble:
            out.DoubleKeys = append(out.DoubleKeys, k)
            out.DoubleValues = append(out.DoubleValues, v.Double())
        case pcommon.ValueTypeInt:
            out.IntKeys = append(out.IntKeys, k)
            out.IntValues = append(out.IntValues, v.Int())
        case pcommon.ValueTypeStr:
            out.StrKeys = append(out.StrKeys, k)
            out.StrValues = append(out.StrValues, v.Str())
        case pcommon.ValueTypeBytes:
            key := "@bytes@" + k
            encoded := base64.StdEncoding.EncodeToString(v.Bytes().AsRaw())
            out.ComplexKeys = append(out.ComplexKeys, key)
            out.ComplexValues = append(out.ComplexValues, encoded)
        case pcommon.ValueTypeSlice:
            key := "@slice@" + k
            m := &xpdata.JSONMarshaler{}
            b, _ := m.MarshalValue(v)
            out.ComplexKeys = append(out.ComplexKeys, key)
            out.ComplexValues = append(out.ComplexValues, string(b))
        case pcommon.ValueTypeMap:
            key := "@map@" + k
            m := &xpdata.JSONMarshaler{}
            b, _ := m.MarshalValue(v)
            out.ComplexKeys = append(out.ComplexKeys, key)
            out.ComplexValues = append(out.ComplexValues, string(b))
        }
        return true
    })
    return out
}
```

---

## 10. Writer: 배치 삽입

`internal/storage/v2/clickhouse/tracestore/writer.go`의 Writer는 ClickHouse의 `PrepareBatch` API를 사용한다.

```go
// internal/storage/v2/clickhouse/tracestore/writer.go
type Writer struct {
    conn driver.Conn
}

func (w *Writer) WriteTraces(ctx context.Context, td ptrace.Traces) error {
    // 1. 배치 준비
    batch, err := w.conn.PrepareBatch(ctx, sql.InsertSpan)
    defer batch.Close()

    // 2. ResourceSpans → ScopeSpans → Spans 순회
    for _, rs := range td.ResourceSpans().All() {
        for _, ss := range rs.ScopeSpans().All() {
            for _, span := range ss.Spans().All() {
                // 3. OTLP span을 SpanRow로 변환
                sr := dbmodel.ToRow(rs.Resource(), ss.Scope(), span)
                // 4. 배치에 추가 (57개 칼럼)
                err = batch.Append(
                    sr.ID, sr.TraceID, sr.TraceState, sr.ParentSpanID,
                    sr.Name, sr.Kind, sr.StartTime, sr.StatusCode,
                    sr.StatusMessage, sr.Duration,
                    sr.Attributes.BoolKeys, sr.Attributes.BoolValues,
                    // ... 2D attributes는 toTuple()로 변환 ...
                    toTuple(sr.EventAttributes.BoolKeys, sr.EventAttributes.BoolValues),
                    // ...
                )
            }
        }
    }
    // 5. 배치 전송
    return batch.Send()
}
```

### toTuple: 2D 속성 변환

이벤트/링크의 중첩 속성은 ClickHouse의 Nested-of-Nested에 맞게 변환한다:

```go
// internal/storage/v2/clickhouse/tracestore/writer.go
func toTuple[T any](keys [][]string, values [][]T) [][][]any {
    tuple := make([][][]any, 0, len(keys))
    for i := range keys {
        inner := make([][]any, 0, len(keys[i]))
        for j := range keys[i] {
            inner = append(inner, []any{keys[i][j], values[i][j]})
        }
        tuple = append(tuple, inner)
    }
    return tuple
}
```

---

## 11. ToRow: OTLP에서 SpanRow로 변환

```go
// internal/storage/v2/clickhouse/tracestore/dbmodel/to.go
func ToRow(resource pcommon.Resource, scope pcommon.InstrumentationScope,
    span ptrace.Span) *SpanRow {
    serviceName, _ := resource.Attributes().Get(otelsemconv.ServiceNameKey)
    duration := span.EndTimestamp().AsTime().Sub(span.StartTimestamp().AsTime()).Nanoseconds()

    sr := &SpanRow{
        ID:            span.SpanID().String(),           // hex 문자열
        TraceID:       span.TraceID().String(),          // hex 문자열
        TraceState:    span.TraceState().AsRaw(),
        ParentSpanID:  span.ParentSpanID().String(),
        Name:          span.Name(),
        Kind:          jptrace.SpanKindToString(span.Kind()),
        StartTime:     span.StartTimestamp().AsTime(),
        StatusCode:    span.Status().Code().String(),
        StatusMessage: span.Status().Message(),
        Duration:      duration,                          // 나노초
        ServiceName:   serviceName.Str(),
        ScopeName:     scope.Name(),
        ScopeVersion:  scope.Version(),
    }

    appendAttributes(&sr.Attributes, span.Attributes())
    for _, event := range span.Events().All() {
        sr.appendEvent(event)
    }
    for _, link := range span.Links().All() {
        sr.appendLink(link)
    }
    appendAttributes(&sr.ResourceAttributes, resource.Attributes())
    appendAttributes(&sr.ScopeAttributes, scope.Attributes())
    return sr
}
```

### 이벤트/링크 추가

```go
func (sr *SpanRow) appendEvent(event ptrace.SpanEvent) {
    sr.EventNames = append(sr.EventNames, event.Name())
    sr.EventTimestamps = append(sr.EventTimestamps, event.Timestamp().AsTime())
    appendAttributes2D(&sr.EventAttributes, event.Attributes())
}

func (sr *SpanRow) appendLink(link ptrace.SpanLink) {
    sr.LinkTraceIDs = append(sr.LinkTraceIDs, link.TraceID().String())
    sr.LinkSpanIDs = append(sr.LinkSpanIDs, link.SpanID().String())
    sr.LinkTraceStates = append(sr.LinkTraceStates, link.TraceState().AsRaw())
    appendAttributes2D(&sr.LinkAttributes, link.Attributes())
}
```

---

## 12. FromRow: SpanRow에서 OTLP로 변환

```go
// internal/storage/v2/clickhouse/tracestore/dbmodel/from.go
func FromRow(storedSpan *SpanRow) ptrace.Traces {
    trace := ptrace.NewTraces()
    resourceSpans := trace.ResourceSpans().AppendEmpty()
    scopeSpans := resourceSpans.ScopeSpans().AppendEmpty()
    span := scopeSpans.Spans().AppendEmpty()

    sp, err := convertSpan(storedSpan)
    sp.CopyTo(span)
    if err != nil {
        jptrace.AddWarnings(span, err.Error())
    }

    resource := resourceSpans.Resource()
    rs := convertResource(storedSpan, span)
    rs.CopyTo(resource)

    scope := scopeSpans.Scope()
    sc := convertScope(storedSpan, span)
    sc.CopyTo(scope)

    return trace
}
```

### Complex Attributes 역변환

```go
// internal/storage/v2/clickhouse/tracestore/dbmodel/from.go
func putAttributes(attrs pcommon.Map, storedAttrs *Attributes, spanForWarnings ptrace.Span) {
    // 원시 타입은 직접 매핑
    for i := 0; i < len(storedAttrs.BoolKeys); i++ {
        attrs.PutBool(storedAttrs.BoolKeys[i], storedAttrs.BoolValues[i])
    }
    // ... Double, Int, Str 동일 ...

    // Complex 타입은 접두어로 분류하여 역변환
    for i := 0; i < len(storedAttrs.ComplexKeys); i++ {
        switch {
        case strings.HasPrefix(storedAttrs.ComplexKeys[i], "@bytes@"):
            decoded, _ := base64.StdEncoding.DecodeString(storedAttrs.ComplexValues[i])
            k := strings.TrimPrefix(storedAttrs.ComplexKeys[i], "@bytes@")
            attrs.PutEmptyBytes(k).FromRaw(decoded)

        case strings.HasPrefix(storedAttrs.ComplexKeys[i], "@slice@"):
            k := strings.TrimPrefix(storedAttrs.ComplexKeys[i], "@slice@")
            m := &xpdata.JSONUnmarshaler{}
            val, _ := m.UnmarshalValue([]byte(storedAttrs.ComplexValues[i]))
            attrs.PutEmptySlice(k).FromRaw(val.Slice().AsRaw())

        case strings.HasPrefix(storedAttrs.ComplexKeys[i], "@map@"):
            k := strings.TrimPrefix(storedAttrs.ComplexKeys[i], "@map@")
            m := &xpdata.JSONUnmarshaler{}
            val, _ := m.UnmarshalValue([]byte(storedAttrs.ComplexValues[i]))
            attrs.PutEmptyMap(k).FromRaw(val.Map().AsRaw())
        }
    }
}
```

---

## 13. Reader: 트레이스 조회

### GetTraces

```go
// internal/storage/v2/clickhouse/tracestore/reader.go
func (r *Reader) GetTraces(ctx context.Context,
    traceIDs ...tracestore.GetTraceParams) iter.Seq2[[]ptrace.Traces, error] {
    return func(yield func([]ptrace.Traces, error) bool) {
        for _, traceID := range traceIDs {
            // SQL: SELECT ... FROM spans s WHERE s.trace_id = ?
            rows, err := r.conn.Query(ctx, sql.SelectSpansByTraceID, traceID.TraceID)
            for rows.Next() {
                span, _ := dbmodel.ScanRow(rows)
                trace := dbmodel.FromRow(span)
                if !yield([]ptrace.Traces{trace}, nil) {
                    return  // 소비자가 중단 요청
                }
            }
        }
    }
}
```

사용하는 SQL:

```sql
-- sql.SelectSpansByTraceID
SELECT id, trace_id, trace_state, parent_span_id, name, kind,
       start_time, status_code, status_message, duration,
       bool_attributes.key, bool_attributes.value,
       -- ... 57개 칼럼 ...
FROM spans s
WHERE s.trace_id = ?
```

---

## 14. Reader: 서비스/오퍼레이션 조회

### GetServices

```go
// internal/storage/v2/clickhouse/tracestore/reader.go
func (r *Reader) GetServices(ctx context.Context) ([]string, error) {
    rows, _ := r.conn.Query(ctx, sql.SelectServices)
    var services []string
    for rows.Next() {
        var service dbmodel.Service
        rows.ScanStruct(&service)
        services = append(services, service.Name)
    }
    return services, nil
}
```

SQL:
```sql
SELECT name FROM services GROUP BY name
```

### GetOperations

```go
// internal/storage/v2/clickhouse/tracestore/reader.go
func (r *Reader) GetOperations(ctx context.Context,
    query tracestore.OperationQueryParams) ([]tracestore.Operation, error) {
    var rows driver.Rows
    if query.SpanKind == "" {
        rows, _ = r.conn.Query(ctx, sql.SelectOperationsAllKinds, query.ServiceName)
    } else {
        rows, _ = r.conn.Query(ctx, sql.SelectOperationsByKind, query.ServiceName, query.SpanKind)
    }
    // ...
}
```

SQL:
```sql
-- SpanKind 미지정
SELECT name, span_kind FROM operations
WHERE service_name = ? GROUP BY name, span_kind

-- SpanKind 지정
SELECT name, span_kind FROM operations
WHERE service_name = ? AND span_kind = ? GROUP BY name, span_kind
```

---

## 15. FindTraces: 복합 쿼리 빌더

`internal/storage/v2/clickhouse/tracestore/query_builder.go`의 `buildFindTraceIDsQuery`는 동적으로 SQL을 구성한다.

### 쿼리 빌드 흐름

```go
// internal/storage/v2/clickhouse/tracestore/query_builder.go
func (r *Reader) buildFindTraceIDsQuery(ctx context.Context,
    query tracestore.TraceQueryParams) (string, []any, error) {
    limit := query.SearchDepth
    if limit == 0 {
        limit = r.config.DefaultSearchDepth  // 1000
    }
    if limit > r.config.MaxSearchDepth {
        return "", nil, fmt.Errorf("search depth %d exceeds maximum %d", limit, r.config.MaxSearchDepth)
    }

    var inner strings.Builder
    inner.WriteString(sql.SearchTraceIDsBase)  // "SELECT DISTINCT s.trace_id FROM spans s WHERE 1=1"
    args := []any{}

    if query.ServiceName != "" {
        appendAnd(&inner, "s.service_name = ?")
        args = append(args, query.ServiceName)
    }
    if query.OperationName != "" {
        appendAnd(&inner, "s.name = ?")
        args = append(args, query.OperationName)
    }
    if query.DurationMin > 0 {
        appendAnd(&inner, "s.duration >= ?")
        args = append(args, query.DurationMin.Nanoseconds())
    }
    if query.DurationMax > 0 {
        appendAnd(&inner, "s.duration <= ?")
        args = append(args, query.DurationMax.Nanoseconds())
    }
    if !query.StartTimeMin.IsZero() {
        appendAnd(&inner, "s.start_time >= ?")
        args = append(args, query.StartTimeMin)
    }
    if !query.StartTimeMax.IsZero() {
        appendAnd(&inner, "s.start_time <= ?")
        args = append(args, query.StartTimeMax)
    }

    // 속성 조건 추가 (타입 메타데이터 참조)
    attributeMetadata, _ := r.getAttributeMetadata(ctx, query.Attributes)
    args, _ = buildAttributeConditions(&inner, args, query.Attributes, attributeMetadata)

    inner.WriteString("\nLIMIT ?")
    args = append(args, limit)

    // 외부 래핑: trace_id_timestamps JOIN
    q := fmt.Sprintf(sql.SearchTraceIDs, indentBlock(inner.String()))
    return q, args, nil
}
```

### 생성되는 SQL 구조

```sql
SELECT l.trace_id, t.start, t.end
FROM (
    SELECT DISTINCT s.trace_id
    FROM spans s
    WHERE 1=1
        AND s.service_name = ?
        AND s.name = ?
        AND s.start_time >= ?
        AND s.start_time <= ?
        AND s.duration >= ?
        AND (
            arrayExists((key, value) -> key = ? AND value = ?, s.str_attributes.key, s.str_attributes.value)
            OR arrayExists((key, value) -> key = ? AND value = ?, s.resource_str_attributes.key, s.resource_str_attributes.value)
            OR ...
        )
    LIMIT ?
) l
LEFT JOIN trace_id_timestamps t ON l.trace_id = t.trace_id
```

### 속성 검색의 arrayExists 패턴

ClickHouse의 Nested 칼럼에서 key-value 쌍을 검색하려면 `arrayExists`를 사용한다:

```go
// internal/storage/v2/clickhouse/tracestore/query_builder.go
func appendArrayExists(q *strings.Builder, indent int, prefix string, valueType pcommon.ValueType) {
    strColumnType := jptrace.ValueTypeToString(valueType)
    columnPrefix := ""
    if prefix != "" {
        columnPrefix = prefix + "_"
    }
    q.WriteString("arrayExists((key, value) -> key = ? AND value = ?, " +
        "s." + columnPrefix + strColumnType + "_attributes.key, " +
        "s." + columnPrefix + strColumnType + "_attributes.value)")
}
```

이벤트/링크는 이중 Nested이므로 중첩 `arrayExists`를 사용한다:

```go
func appendNestedArrayExists(q *strings.Builder, indent int, nestedArray string, valueType pcommon.ValueType) {
    q.WriteString("arrayExists(x -> arrayExists((key, value) -> key = ? AND value = ?, " +
        "x." + strColumnType + "_attributes.key, " +
        "x." + strColumnType + "_attributes.value), " +
        "s." + nestedArray + ")")
}
```

---

## 16. Attribute Metadata와 타입 추론

### 문제

쿼리 서비스는 모든 속성 값을 문자열로 전달한다 (`AsString()`). 하지만 ClickHouse에서는 `int_attributes`에 정수로 저장된 값을 문자열로 검색할 수 없다.

### 해결: attribute_metadata 테이블

`attribute_metadata` 테이블은 각 속성 키의 **실제 저장 타입**과 **레벨**(resource, scope, span)을 추적한다:

```sql
-- attribute_metadata 테이블
CREATE TABLE IF NOT EXISTS attribute_metadata (
    attribute_key String,
    type String,    -- 'bool', 'double', 'int', 'str', 'bytes', 'map', 'slice'
    level String    -- 'resource', 'scope', 'span'
) ENGINE = AggregatingMergeTree
ORDER BY (attribute_key, type, level)
```

### 조회 및 활용

```go
// internal/storage/v2/clickhouse/tracestore/query_builder.go
func buildStringAttributeCondition(q *strings.Builder, args []any, key string,
    attr pcommon.Value, metadata attributeMetadata) []any {
    levelTypes, ok := metadata[key]
    if !ok {
        // 메타데이터 없으면 문자열로 가정
        return appendStringAttributeFallback(q, args, key, attr)
    }

    // 메타데이터가 있으면 실제 타입으로 변환 시도
    for _, t := range levelTypes.resource {
        tav, err := parseStringToTypedValue(key, attr, t)
        if err == nil {
            appendArrayExists(q, 2, "resource", tav.valueType)
            args = append(args, tav.key, tav.value)
        }
    }
    // scope, span, event, link도 동일하게 처리
}
```

예를 들어 `http.status_code=200`이 문자열로 전달되면:
1. `attribute_metadata`에서 `http.status_code`가 `int` 타입임을 확인
2. `"200"`을 `int64(200)`으로 변환
3. `int_attributes`에서 검색

---

## 17. Purge: 전체 데이터 삭제

테스트에서 사용하는 `Purge()` 함수는 모든 테이블을 TRUNCATE한다:

```go
// internal/storage/v2/clickhouse/factory.go
func (f *Factory) Purge(ctx context.Context) error {
    tables := []struct{ name, query string }{
        {"spans", sql.TruncateSpans},                         // TRUNCATE TABLE spans
        {"services", sql.TruncateServices},                   // TRUNCATE TABLE services
        {"operations", sql.TruncateOperations},               // TRUNCATE TABLE operations
        {"trace_id_timestamps", sql.TruncateTraceIDTimestamps}, // TRUNCATE TABLE trace_id_timestamps
        {"attribute_metadata", sql.TruncateAttributeMetadata},  // TRUNCATE TABLE attribute_metadata
    }
    for _, table := range tables {
        if err := f.conn.Exec(ctx, table.query); err != nil {
            return fmt.Errorf("failed to purge %s: %w", table.name, err)
        }
    }
    return nil
}
```

Materialized View는 TRUNCATE 대상이 아니다. 대상 테이블이 비워지면 MV는 새 데이터만 반영한다.

---

## 18. 아키텍처 다이어그램 종합

### 쓰기 경로

```
┌──────────────────┐
│  OTLP Receiver   │
│  (Collector)     │
└────────┬─────────┘
         │ ptrace.Traces
         ▼
┌──────────────────────────────────────────┐
│  Writer.WriteTraces()                    │
│                                          │
│  1. PrepareBatch(InsertSpan)             │
│  2. for ResourceSpans:                   │
│       for ScopeSpans:                    │
│         for Spans:                       │
│           sr = dbmodel.ToRow(...)        │
│           batch.Append(57 columns)       │
│  3. batch.Send()                         │
└────────┬─────────────────────────────────┘
         │
         ▼
┌──────────────────────────────────────────┐
│  ClickHouse                              │
│                                          │
│  ┌───────────────────┐                   │
│  │     spans         │ ← 직접 삽입       │
│  └───────┬───────────┘                   │
│          │ Materialized Views (자동)      │
│   ┌──────┼──────┬──────┬──────┐          │
│   ▼      ▼      ▼      ▼      ▼         │
│ services ops  trace_id attr_meta(x3)     │
│                _ts                        │
└──────────────────────────────────────────┘
```

### 읽기 경로

```
┌─────────────────┐
│  Query Service   │
│  (API 요청)      │
└────────┬────────┘
         │
    ┌────┴─────────────────────────┐
    │                              │
    ▼                              ▼
GetTraces                    FindTraces
    │                              │
    ▼                              ▼
SELECT ... FROM spans      buildFindTraceIDsQuery()
WHERE trace_id = ?         ┌─ SELECT DISTINCT trace_id
    │                      │  FROM spans WHERE ...
    │                      │  AND arrayExists(...)
    │                      │  LIMIT ?
    │                      └─ LEFT JOIN trace_id_timestamps
    │                              │
    ▼                              ▼
dbmodel.ScanRow()          SELECT ... FROM spans
dbmodel.FromRow()          WHERE trace_id IN (...)
    │                      ORDER BY trace_id
    │                              │
    ▼                              ▼
iter.Seq2[ptrace.Traces]   iter.Seq2[ptrace.Traces]
```

### ES vs ClickHouse 비교 요약

```
┌─────────────────────────┬──────────────────────────────────┐
│     Elasticsearch       │        ClickHouse                │
├─────────────────────────┼──────────────────────────────────┤
│ v1 + v2 래퍼 구조       │ 네이티브 v2 구현                  │
│ JSON 문서 저장           │ 칼럼 지향 저장                    │
│ BulkProcessor 비동기    │ PrepareBatch 동기                │
│ 날짜별 인덱스 파티셔닝  │ toDate(start_time) 파티셔닝      │
│ 롤오버/ILM 도구 필요     │ MergeTree 자동 관리              │
│ nested/object 이중 태그 │ 타입별 Nested 칼럼               │
│ Terms Aggregation       │ Materialized View + GROUP BY     │
│ 서비스 캐시 (12h LRU)   │ 캐시 불필요 (MV가 실시간 갱신)    │
│ OpenSearch 호환         │ ClickHouse 전용                  │
└─────────────────────────┴──────────────────────────────────┘
```

---

## 요약

Jaeger의 ClickHouse 스토리지는 다음과 같은 핵심 설계 원칙을 따른다:

1. **OTLP 네이티브**: v1 호환 레이어 없이 `ptrace.Traces`를 직접 처리한다
2. **타입별 칼럼 분리**: Bool, Double, Int, Str, Complex를 별도 Nested 칼럼에 저장하여 타입 안전성과 쿼리 효율 확보
3. **Materialized View 자동화**: spans 테이블 삽입 시 서비스, 오퍼레이션, 트레이스 타임스탬프, 속성 메타데이터가 자동 갱신
4. **배치 삽입**: `PrepareBatch`로 여러 span을 한 번에 전송하여 네트워크 오버헤드 최소화
5. **동적 쿼리 빌드**: `arrayExists`와 `attribute_metadata`를 활용한 타입-인식 검색
6. **Complex Attributes**: bytes, slice, map 타입을 접두어 기반 문자열로 직렬화하여 일관된 스키마 유지
