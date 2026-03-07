# Jaeger 데이터 모델

## 1. 분산 트레이싱 핵심 개념

### 1.1 Trace, Span, Process

```
Trace (하나의 요청 흐름)
├── Span A (root, frontend, /api/checkout)
│   ├── Span B (cart-service, getCart)
│   └── Span C (payment-service, processPayment)
│       └── Span D (payment-gateway, chargeCard)
```

| 개념 | 설명 |
|------|------|
| **Trace** | 분산 시스템에서 하나의 요청이 거치는 전체 경로. TraceID로 식별 |
| **Span** | Trace 내 하나의 작업 단위. SpanID로 식별, 부모-자식 관계 형성 |
| **Process** | Span을 생성한 서비스/프로세스. ServiceName + Tags |

### 1.2 TraceID와 SpanID

```
TraceID: 128-bit (16바이트)
  예: 1a2b3c4d5e6f78901a2b3c4d5e6f7890

SpanID: 64-bit (8바이트)
  예: 1a2b3c4d5e6f7890
```

## 2. Jaeger 데이터 모델 계층

Jaeger v2는 여러 데이터 모델 계층을 가진다:

```
┌───────────────────────┐
│   OTLP (ptrace)       │  ← v2 네이티브 모델 (OpenTelemetry)
│   ptrace.Traces       │
│   ptrace.Span         │
├───────────────────────┤
│   Jaeger IDL v1       │  ← v1 호환 모델 (jaeger-idl)
│   model.Span          │
│   model.Trace         │
│   model.Batch         │
├───────────────────────┤
│   UI Model            │  ← JSON API 응답 모델
│   uimodel.Trace       │
│   uimodel.Span        │
├───────────────────────┤
│   DB Models           │  ← 스토리지별 모델
│   Cassandra dbmodel   │
│   ES dbmodel          │
│   ClickHouse SpanRow  │
└───────────────────────┘
```

## 3. OTLP 모델 (v2 네이티브)

Jaeger v2의 주요 데이터 포맷. OpenTelemetry Collector의 `pdata` 패키지를 사용한다.

### 3.1 ptrace 구조

```
ptrace.Traces
└── ResourceSpans[]                    // 리소스(서비스)별 그룹
    ├── Resource                       // 서비스 정보 (attributes)
    │   └── Attributes (Map)           // service.name, host.name 등
    └── ScopeSpans[]                   // 계측 범위별 그룹
        ├── Scope                      // 계측 라이브러리 정보
        │   └── Name, Version
        └── Spans[]                    // 실제 Span 데이터
            ├── TraceID      [16]byte
            ├── SpanID       [8]byte
            ├── ParentSpanID [8]byte
            ├── Name         string     // 작업명 (예: "GET /api")
            ├── Kind         SpanKind   // SERVER, CLIENT, PRODUCER, CONSUMER, INTERNAL
            ├── StartTimestamp uint64   // 나노초
            ├── EndTimestamp   uint64   // 나노초
            ├── Status
            │   ├── Code     StatusCode // OK, ERROR, UNSET
            │   └── Message  string
            ├── Attributes   Map        // key-value 태그
            ├── Events[]               // 시간 이벤트 (로그)
            │   ├── Timestamp uint64
            │   ├── Name     string
            │   └── Attributes Map
            └── Links[]                // 다른 Span 참조
                ├── TraceID
                ├── SpanID
                └── Attributes Map
```

### 3.2 v2 스토리지 인터페이스

```go
// internal/storage/v2/api/tracestore/writer.go:13-19
type Writer interface {
    WriteTraces(ctx context.Context, td ptrace.Traces) error
}

// internal/storage/v2/api/tracestore/reader.go:19-66
type Reader interface {
    GetTraces(ctx context.Context, traceIDs ...GetTraceParams) iter.Seq2[[]ptrace.Traces, error]
    GetServices(ctx context.Context) ([]string, error)
    GetOperations(ctx context.Context, query OperationQueryParams) ([]Operation, error)
    FindTraces(ctx context.Context, query TraceQueryParams) iter.Seq2[[]ptrace.Traces, error]
    FindTraceIDs(ctx context.Context, query TraceQueryParams) iter.Seq2[[]FoundTraceID, error]
}
```

### 3.3 TraceQueryParams

```go
// internal/storage/v2/api/tracestore/reader.go:83-93
type TraceQueryParams struct {
    ServiceName   string
    OperationName string
    Attributes    pcommon.Map      // 태그 필터 (key-value)
    StartTimeMin  time.Time
    StartTimeMax  time.Time
    DurationMin   time.Duration
    DurationMax   time.Duration
    SearchDepth   int              // 결과 제한 수
}
```

## 4. Jaeger IDL v1 모델

레거시 호환을 위한 모델. `github.com/jaegertracing/jaeger-idl/model/v1` 패키지에 정의.

### 4.1 핵심 타입

```
model.Span
├── TraceID       TraceID        // 128-bit (High + Low uint64)
├── SpanID        SpanID         // 64-bit uint64
├── OperationName string
├── References    []SpanRef      // 부모-자식 관계
│   ├── RefType   SpanRefType    // CHILD_OF, FOLLOWS_FROM
│   ├── TraceID
│   └── SpanID
├── Flags         Flags          // 비트 플래그 (Firehose, Debug 등)
├── StartTime     time.Time
├── Duration      time.Duration
├── Tags          []KeyValue     // 태그
├── Logs          []Log          // 시간 이벤트
│   ├── Timestamp time.Time
│   └── Fields    []KeyValue
├── Process       *Process       // 서비스 정보
│   ├── ServiceName string
│   └── Tags        []KeyValue
└── Warnings      []string

model.KeyValue
├── Key     string
├── VType   ValueType  // STRING, BOOL, INT64, FLOAT64, BINARY
├── VStr    string
├── VBool   bool
├── VInt64  int64
├── VFloat64 float64
└── VBinary  []byte

model.Trace
├── Spans    []*Span
└── Warnings []string

model.Batch
├── Process *Process
└── Spans   []*Span
```

## 5. UI 모델

HTTP JSON API 응답에 사용되는 모델.

```go
// internal/uimodel/model.go
type Trace struct {
    TraceID   TraceID                  // hex 문자열
    Spans     []Span
    Processes map[ProcessID]Process    // 프로세스 중복 제거
    Warnings  []string
}

type Span struct {
    TraceID       TraceID
    SpanID        SpanID
    ParentSpanID  SpanID     // deprecated, References 사용
    OperationName string
    References    []Reference
    StartTime     uint64     // 마이크로초 (Unix epoch)
    Duration      uint64     // 마이크로초
    Tags          []KeyValue
    Logs          []Log
    ProcessID     ProcessID  // Processes 맵의 키
    Warnings      []string
}

type KeyValue struct {
    Key   string
    Type  ValueType   // "string", "bool", "int64", "float64", "binary"
    Value interface{} // 실제 값
}
```

### 5.1 v1 → UI 변환 시 주의사항

```go
// internal/uimodel/converter/v1/json/from_domain.go:120-122
// JavaScript의 안전한 정수 범위 체크 (2^53 - 1)
if kv.VType == model.Int64Type && kv.VInt64 > (1<<53-1) {
    // 문자열로 변환하여 정밀도 손실 방지
}
```

## 6. 데이터 변환 흐름

```
                    ┌──────────────┐
                    │ Jaeger Thrift│
                    │ (레거시 SDK) │
                    └──────┬───────┘
                           │ to_domain.go: ToDomain()
                           ▼
┌───────────────┐   ┌──────────────┐   ┌───────────────┐
│ ptrace.Traces │◄──│ model.Span   │──►│ uimodel.Trace │
│ (OTLP)        │   │ (Jaeger IDL) │   │ (JSON API)    │
└───────┬───────┘   └──────┬───────┘   └───────────────┘
        │                  │
        │  V1BatchesFrom   │ FromDomain()
        │  Traces()        │
        │                  ▼
        │           ┌──────────────┐
        │           │ DB Models    │
        │           │              │
        │           │ Cassandra:   │
        │           │  dbmodel.Span│
        │           │              │
        │           │ ES:          │
        │           │  dbmodel.Span│
        └──────────►│              │
     WriteTraces()  │ ClickHouse:  │
                    │  SpanRow     │
                    └──────────────┘
```

### 6.1 변환 함수 위치

| 변환 | 함수 | 파일 |
|------|------|------|
| Thrift → v1 Model | `ToDomain()` | `internal/converter/thrift/jaeger/to_domain.go:17` |
| v1 Model → UI | `FromDomain()` | `internal/uimodel/converter/v1/json/from_domain.go:23` |
| OTLP → v1 Model | `V1BatchesFromTraces()` | `internal/storage/v2/v1adapter/translator.go:20` |
| v1 Model → OTLP | `V1BatchesToTraces()` | `internal/storage/v2/v1adapter/translator.go:29` |
| v1 → Cassandra DB | `FromDomain()` | `internal/storage/v1/cassandra/spanstore/dbmodel/converter.go:40` |
| Cassandra DB → v1 | `ToDomain()` | `internal/storage/v1/cassandra/spanstore/dbmodel/converter.go:45` |

## 7. jptrace 유틸리티

OTLP 트레이스 데이터를 다루기 위한 유틸리티 패키지.

### 7.1 트레이스 집계

```go
// internal/jptrace/aggregator.go:17
func AggregateTraces(tracesSeq iter.Seq2[[]ptrace.Traces, error]) iter.Seq2[ptrace.Traces, error]

// internal/jptrace/aggregator.go:26
func AggregateTracesWithLimit(tracesSeq, maxSpans int) iter.Seq2[ptrace.Traces, error]
// 여러 청크를 TraceID별로 병합, 최대 Span 수 제한
```

### 7.2 Span 순회

```go
// internal/jptrace/spaniter.go:21
func SpanIter(traces ptrace.Traces) iter.Seq2[SpanIterPos, ptrace.Span]
// ResourceSpans → ScopeSpans → Spans 3중 중첩을 평탄화

// internal/jptrace/spanmap.go:11
func SpanMap[K comparable](traces ptrace.Traces, keyFn func(ptrace.Span) K) map[K]ptrace.Span
// Span을 키 함수로 맵핑
```

### 7.3 pcommon.Map ↔ map[string]string

```go
// internal/jptrace/ 내 유틸
func PcommonMapToPlainMap(m pcommon.Map) map[string]string
// OTLP Attributes(pcommon.Map)를 단순 문자열 맵으로 변환
```

## 8. 스토리지별 데이터 모델

### 8.1 Cassandra

```
Table: traces
├── trace_id       blob (16바이트)        // 파티션 키
├── span_id        bigint
├── span_hash      bigint                // 서비스 인덱스 버킷용
├── parent_id      bigint
├── operation_name text
├── flags          int
├── start_time     bigint (마이크로초)
├── duration       bigint (마이크로초)
├── tags           list<frozen<tag>>
├── logs           list<frozen<log>>
├── refs           list<frozen<span_ref>>
└── process        frozen<process>

// KeyValue 타입별 필드
tag:
├── key          text
├── value_type   text
├── value_string text
├── value_bool   boolean
├── value_long   bigint
├── value_double double
└── value_binary blob
```

### 8.2 Elasticsearch

```json
{
  "traceID": "1a2b3c...",
  "spanID": "5e6f78...",
  "operationName": "GET /api",
  "process": {
    "serviceName": "frontend",
    "tags": [...]
  },
  "startTime": 1704067200000000,      // 마이크로초
  "startTimeMillis": 1704067200000,   // ES 시간 쿼리용 밀리초
  "duration": 123000,                  // 마이크로초
  "tags": [
    {"key": "http.method", "type": "string", "value": "GET"},
    {"key": "error", "type": "bool", "value": true}
  ],
  "tag": {                            // Kibana 호환 플랫 태그
    "http.method": "GET",
    "error": true
  },
  "logs": [...],
  "references": [...]
}
```

### 8.3 ClickHouse

ClickHouse는 컬럼 지향 모델을 사용하여 속성을 타입별로 분리 저장한다:

```sql
-- 약 60개 컬럼
CREATE TABLE spans (
    TraceID       FixedString(16),
    SpanID        FixedString(8),
    ParentSpanID  FixedString(8),
    Name          String,
    Kind          Int32,
    StartTime     DateTime64(9),    -- 나노초 정밀도
    Duration      Int64,
    StatusCode    Int32,
    StatusMessage String,

    -- 속성은 타입별 Nested 구조
    BoolAttributes   Nested(Key String, Value UInt8),
    DoubleAttributes Nested(Key String, Value Float64),
    IntAttributes    Nested(Key String, Value Int64),
    StrAttributes    Nested(Key String, Value String),

    -- 이벤트, 링크도 Nested
    Events           Nested(Timestamp DateTime64(9), Name String, ...),
    Links            Nested(TraceID FixedString(16), SpanID FixedString(8), ...),

    -- 리소스/스코프 속성
    ServiceName      String,
    ResourceAttributes Nested(Key String, Value String),
    ...
)
```

## 9. 의존성 모델

서비스 간 호출 관계를 나타내는 모델.

```go
// internal/storage/v2/api/depstore/reader.go:12-17
type Link struct {
    Parent    string
    Child     string
    CallCount uint64
}

type Reader interface {
    GetDependencies(ctx context.Context, endTs time.Time, lookback time.Duration) ([]Link, error)
}
```

## 10. 소스코드 참조

| 파일 | 설명 |
|------|------|
| `internal/storage/v2/api/tracestore/reader.go` | v2 Reader 인터페이스, TraceQueryParams |
| `internal/storage/v2/api/tracestore/writer.go` | v2 Writer 인터페이스 |
| `internal/storage/v2/api/tracestore/factory.go` | v2 Factory 인터페이스 |
| `internal/uimodel/model.go` | UI JSON 모델 (Trace, Span, KeyValue 등) |
| `internal/jptrace/aggregator.go` | OTLP 트레이스 집계 유틸리티 |
| `internal/jptrace/spaniter.go` | Span 순회 이터레이터 |
| `internal/storage/v2/v1adapter/translator.go` | v1 ↔ OTLP 변환 |
| `internal/converter/thrift/jaeger/to_domain.go` | Thrift → v1 변환 |
| `internal/uimodel/converter/v1/json/from_domain.go` | v1 → UI 변환 |
