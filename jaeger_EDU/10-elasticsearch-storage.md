# Elasticsearch 스토리지

## 목차

1. [개요](#1-개요)
2. [Factory 아키텍처](#2-factory-아키텍처)
3. [설정 구조 (Configuration)](#3-설정-구조-configuration)
4. [인덱스 네이밍 전략](#4-인덱스-네이밍-전략)
5. [필드 매핑과 데이터 모델](#5-필드-매핑과-데이터-모델)
6. [태그 스키마: Object Tags vs Nested Tags](#6-태그-스키마-object-tags-vs-nested-tags)
7. [ensureRequiredFields 메커니즘](#7-ensurerequiredfields-메커니즘)
8. [Service Operation 캐시](#8-service-operation-캐시)
9. [SpanWriter 동작 원리](#9-spanwriter-동작-원리)
10. [SpanReader와 쿼리 빌드](#10-spanreader와-쿼리-빌드)
11. [인덱스 롤오버 (Rollover)](#11-인덱스-롤오버-rollover)
12. [ES Index Cleaner 도구](#12-es-index-cleaner-도구)
13. [ES Rollover 도구](#13-es-rollover-도구)
14. [ILM (Index Lifecycle Management) 지원](#14-ilm-index-lifecycle-management-지원)
15. [Bulk Processing](#15-bulk-processing)
16. [OpenSearch 호환성](#16-opensearch-호환성)
17. [인증 메커니즘](#17-인증-메커니즘)
18. [아키텍처 다이어그램 종합](#18-아키텍처-다이어그램-종합)

---

## 1. 개요

Jaeger의 Elasticsearch 스토리지 백엔드는 프로젝트 초기부터 존재한 핵심 저장소 구현이다. 분산 트레이싱 데이터를 Elasticsearch 클러스터에 저장하고 조회하는 전체 라이프사이클을 관리한다.

소스코드는 두 가지 버전으로 나뉘어 있다:

| 계층 | 경로 | 역할 |
|------|------|------|
| v2 Factory | `internal/storage/v2/elasticsearch/factory.go` | 최신 인터페이스 구현, `tracestore.Factory` + `depstore.Factory` |
| v1 FactoryBase | `internal/storage/v1/elasticsearch/factory.go` | ES 클라이언트 관리, 템플릿 생성, 패스워드 감시 등 핵심 로직 |
| 공통 config | `internal/storage/elasticsearch/config/config.go` | 설정 구조체, 클라이언트 생성 |
| 공통 dbmodel | `internal/storage/elasticsearch/dbmodel/model.go` | ES 문서 구조체 정의 |
| spanstore | `internal/storage/v1/elasticsearch/spanstore/` | Reader, Writer, ServiceOperationStorage |

```
┌─────────────────────────────────────────────────┐
│              v2 Factory (최신 API)                │
│  internal/storage/v2/elasticsearch/factory.go   │
│  ┌─────────────────────────────────────────┐    │
│  │  ensureRequiredFields(cfg)              │    │
│  │  → span.kind, error 태그 자동 추가       │    │
│  └─────────────────────────────────────────┘    │
│         │ delegates to                           │
│  ┌─────────────────────────────────────────┐    │
│  │      v1 FactoryBase (핵심 엔진)          │    │
│  │  - ES 클라이언트 생성/관리               │    │
│  │  - 인덱스 템플릿 생성                    │    │
│  │  - 패스워드 파일 감시                    │    │
│  └─────────────────────────────────────────┘    │
│         │ creates                                │
│  ┌─────────────┐  ┌──────────────┐              │
│  │ SpanReader  │  │ SpanWriter   │              │
│  │ (v2 wrap)   │  │ (v2 wrap)    │              │
│  └─────────────┘  └──────────────┘              │
└─────────────────────────────────────────────────┘
```

---

## 2. Factory 아키텍처

### v2 Factory

`internal/storage/v2/elasticsearch/factory.go`에 정의된 v2 Factory는 OpenTelemetry Collector의 스토리지 인터페이스를 구현한다.

```go
// internal/storage/v2/elasticsearch/factory.go
type Factory struct {
    coreFactory    *elasticsearch.FactoryBase
    config         escfg.Configuration
    metricsFactory metrics.Factory
}
```

이 Factory는 세 가지 인터페이스를 구현한다:
- `io.Closer` -- 리소스 정리
- `tracestore.Factory` -- 트레이스 Reader/Writer 생성
- `depstore.Factory` -- 의존성 Reader 생성

### v1 FactoryBase

`internal/storage/v1/elasticsearch/factory.go`에 정의된 FactoryBase가 실제 ES 클라이언트 관리의 핵심이다.

```go
// internal/storage/v1/elasticsearch/factory.go
type FactoryBase struct {
    metricsFactory  metrics.Factory
    logger          *zap.Logger
    tracer          trace.TracerProvider
    newClientFn     func(ctx context.Context, c *config.Configuration, ...) (es.Client, error)
    config          *config.Configuration
    client          atomic.Pointer[es.Client]  // 원자적 클라이언트 교체 지원
    pwdFileWatcher  *fswatcher.FSWatcher       // 패스워드 파일 변경 감시
    templateBuilder es.TemplateBuilder
    tags            []string
}
```

핵심 설계 포인트:
- `atomic.Pointer[es.Client]`: 패스워드 변경 시 다운타임 없이 클라이언트를 교체할 수 있다
- `fswatcher.FSWatcher`: 패스워드 파일을 감시하여 자동으로 새 클라이언트를 생성한다
- `templateBuilder`: 인덱스 템플릿을 동적으로 렌더링한다

### 초기화 흐름

```
NewFactory(ctx, cfg, telset, httpAuth)
    │
    ├─ ensureRequiredFields(cfg)   // span.kind, error 태그 추가
    │
    └─ NewFactoryBase(ctx, cfg, metricsFactory, logger, httpAuth)
        │
        ├─ cfg.TagKeysAsFields()         // 파일 + 파라미터에서 태그 키 로드
        ├─ f.newClientFn(ctx, cfg, ...)  // ES 클라이언트 생성
        ├─ f.client.Store(&client)       // 원자적 저장
        ├─ fswatcher.New(...)            // 패스워드 파일 감시 시작
        └─ f.createTemplates(ctx)        // jaeger-span, jaeger-service 템플릿 생성
```

---

## 3. 설정 구조 (Configuration)

`internal/storage/elasticsearch/config/config.go`에 정의된 `Configuration` 구조체는 ES 연결의 모든 측면을 제어한다.

### 주요 설정 카테고리

```go
// internal/storage/elasticsearch/config/config.go
type Configuration struct {
    // ---- 연결 관련 ----
    Servers                   []string          // ES 서버 URL 목록
    RemoteReadClusters        []string          // 크로스 클러스터 읽기용 원격 클러스터
    Authentication            Authentication    // 인증 설정
    TLS                       configtls.ClientConfig
    Sniffing                  Sniffing          // 노드 자동 발견
    SendGetBodyAs             string            // HTTP verb 설정
    QueryTimeout              time.Duration     // 쿼리 타임아웃
    HTTPCompression           bool              // gzip 압축

    // ---- 클라이언트 관련 ----
    BulkProcessing            BulkProcessing    // 벌크 처리 설정
    Version                   uint              // ES 버전 (0=자동감지)

    // ---- 인덱스 관련 ----
    Indices                   Indices           // 인덱스 옵션
    UseReadWriteAliases       bool              // 읽기/쓰기 별칭 사용
    UseILM                    bool              // ILM 사용 여부
    CreateIndexTemplates      bool              // 시작 시 템플릿 생성 여부

    // ---- Jaeger 전용 ----
    MaxDocCount               int               // 쿼리당 최대 문서 수
    MaxSpanAge                time.Duration     // 최대 span 조회 기간
    ServiceCacheTTL           time.Duration     // 서비스 캐시 TTL
    Tags                      TagsAsFields      // 태그 스키마 설정
}
```

### 인덱스 옵션

```go
// internal/storage/elasticsearch/config/config.go
type IndexOptions struct {
    Priority          int64   // 인덱스 템플릿 우선순위 (ESv8)
    DateLayout        string  // 날짜 포맷 (기본: "2006-01-02")
    Shards            int64   // 샤드 수
    Replicas          *int64  // 레플리카 수
    RolloverFrequency string  // "hour" 또는 "day"
}

type Indices struct {
    IndexPrefix   IndexPrefix   // 인덱스 접두어 (예: "production")
    Spans         IndexOptions  // span 인덱스 옵션
    Services      IndexOptions  // service 인덱스 옵션
    Dependencies  IndexOptions  // dependency 인덱스 옵션
    Sampling      IndexOptions  // sampling 인덱스 옵션
}
```

### IndexPrefix 동작

`IndexPrefix.Apply()` 메서드는 인덱스 이름에 접두어를 추가한다:

```go
// internal/storage/elasticsearch/config/config.go
func (p IndexPrefix) Apply(indexName string) string {
    ps := string(p)
    if ps == "" {
        return indexName
    }
    if strings.HasSuffix(ps, IndexPrefixSeparator) {
        return ps + indexName
    }
    return ps + IndexPrefixSeparator + indexName
}
```

예를 들어 `IndexPrefix("production")`이면:
- `Apply("jaeger-span")` → `"production-jaeger-span"`
- `IndexPrefix("")`이면 → `"jaeger-span"` (변경 없음)

---

## 4. 인덱스 네이밍 전략

Jaeger는 시간 기반 인덱스를 사용하여 데이터를 파티셔닝한다. 인덱스 이름 생성 로직은 `internal/storage/v1/elasticsearch/spanstore/index_utils.go`에 있다.

### 날짜 기반 인덱스

```go
// internal/storage/v1/elasticsearch/spanstore/index_utils.go
func indexWithDate(indexPrefix, indexDateLayout string, date time.Time) string {
    spanDate := date.UTC().Format(indexDateLayout)
    return indexPrefix + spanDate
}
```

기본 `DateLayout`이 `"2006-01-02"`이므로 다음과 같은 인덱스가 생성된다:

| 인덱스 유형 | 이름 패턴 | 예시 |
|------------|-----------|------|
| Span | `jaeger-span-YYYY-MM-DD` | `jaeger-span-2024-03-15` |
| Service | `jaeger-service-YYYY-MM-DD` | `jaeger-service-2024-03-15` |
| Dependencies | `jaeger-dependencies-YYYY-MM-DD` | `jaeger-dependencies-2024-03-15` |
| Sampling | `jaeger-sampling-YYYY-MM-DD` | `jaeger-sampling-2024-03-15` |

### 인덱스 접두어 적용

`IndexPrefix`가 `"production"`이면:
- `production-jaeger-span-2024-03-15`
- `production-jaeger-service-2024-03-15`

### 별칭 모드 (UseReadWriteAliases)

롤오버 사용 시 날짜 기반 대신 별칭을 사용한다:

```
읽기: jaeger-span-read
쓰기: jaeger-span-write
```

또는 명시적 별칭(`SpanReadAlias`, `SpanWriteAlias`)을 설정할 수 있다:

```go
// internal/storage/v1/elasticsearch/spanstore/reader.go
const (
    spanIndexBaseName    = "jaeger-span-"
    serviceIndexBaseName = "jaeger-service-"
)
```

### 시간 범위 인덱스 계산

쿼리 시 startTime과 endTime을 기반으로 필요한 인덱스 목록을 계산한다:

```go
// internal/storage/v1/elasticsearch/spanstore/reader.go
func timeRangeIndices(indexName, indexDateLayout string, startTime time.Time,
    endTime time.Time, reduceDuration time.Duration) []string {
    var indices []string
    firstIndex := indexWithDate(indexName, indexDateLayout, startTime)
    currentIndex := indexWithDate(indexName, indexDateLayout, endTime)
    for currentIndex != firstIndex && endTime.After(startTime) {
        if len(indices) == 0 || indices[len(indices)-1] != currentIndex {
            indices = append(indices, currentIndex)
        }
        endTime = endTime.Add(reduceDuration)
        currentIndex = indexWithDate(indexName, indexDateLayout, endTime)
    }
    indices = append(indices, firstIndex)
    return indices
}
```

`reduceDuration`은 `RolloverFrequencyAsNegativeDuration()`으로 계산된다:

```go
// internal/storage/elasticsearch/config/config.go
func RolloverFrequencyAsNegativeDuration(frequency string) time.Duration {
    if frequency == "hour" {
        return -1 * time.Hour
    }
    return -24 * time.Hour
}
```

---

## 5. 필드 매핑과 데이터 모델

`internal/storage/elasticsearch/dbmodel/model.go`에 정의된 ES 문서 구조는 다음과 같다.

### Span 문서 구조

```go
// internal/storage/elasticsearch/dbmodel/model.go
type Span struct {
    TraceID         TraceID     `json:"traceID"`
    SpanID          SpanID      `json:"spanID"`
    ParentSpanID    SpanID      `json:"parentSpanID,omitempty"`
    Flags           uint32      `json:"flags,omitempty"`
    OperationName   string      `json:"operationName"`
    References      []Reference `json:"references"`
    StartTime       uint64      `json:"startTime"`        // 마이크로초 (Unix epoch)
    StartTimeMillis uint64      `json:"startTimeMillis"`  // 밀리초 (ES range query용)
    Duration        uint64      `json:"duration"`          // 마이크로초
    Tags            []KeyValue  `json:"tags"`              // nested 태그
    Tag             map[string]any `json:"tag,omitempty"`  // object 태그 (Kibana용)
    Logs            []Log       `json:"logs"`
    Process         Process     `json:"process"`
}
```

### 핵심 필드 역할

| 필드 | JSON 키 | 타입 | 용도 |
|------|---------|------|------|
| `TraceID` | `traceID` | keyword | 트레이스 그룹핑 및 조회 |
| `Duration` | `duration` | long | 지속시간 기반 검색 |
| `StartTime` | `startTime` | long(마이크로초) | 정밀 타임스탬프 |
| `StartTimeMillis` | `startTimeMillis` | date(밀리초) | ES range query 최적화 |
| `Process.ServiceName` | `process.serviceName` | keyword | 서비스별 검색 |
| `OperationName` | `operationName` | keyword | 오퍼레이션별 검색 |

### StartTime vs StartTimeMillis

ES는 마이크로초 단위 Unix Epoch 타임스탬프를 date 타입으로 직접 지원하지 않는다. 따라서 Jaeger는 두 필드를 사용한다:

```
StartTime      = 1710489600000000  (마이크로초, long 타입)
StartTimeMillis = 1710489600000    (밀리초, date 타입)
```

`startTimeMillis`를 date 필드로 사용하면 ES가 **불필요한 샤드를 건너뛸 수 있어** 쿼리 성능이 향상된다:

```go
// internal/storage/v1/elasticsearch/spanstore/reader.go
func (*SpanReader) buildStartTimeQuery(startTimeMin, startTimeMax time.Time) elastic.Query {
    minStartTimeMicros := model.TimeAsEpochMicroseconds(startTimeMin)
    maxStartTimeMicros := model.TimeAsEpochMicroseconds(startTimeMax)
    return esquery.NewRangeQuery(startTimeMillisField).
        Gte(minStartTimeMicros / 1000).
        Lte(maxStartTimeMicros / 1000)
}
```

---

## 6. 태그 스키마: Object Tags vs Nested Tags

Jaeger ES 스토리지에서 가장 중요한 설계 결정 중 하나는 태그 저장 방식이다.

### Nested Tags (기본)

기본적으로 태그는 **nested object 배열**로 저장된다:

```json
{
  "tags": [
    {"key": "http.method", "type": "string", "value": "GET"},
    {"key": "http.status_code", "type": "int64", "value": 200}
  ]
}
```

ES 쿼리에서는 **nested query**를 사용해야 한다:

```go
// internal/storage/v1/elasticsearch/spanstore/reader.go
const (
    nestedTagsField        = "tags"
    nestedProcessTagsField = "process.tags"
    nestedLogFieldsField   = "logs.fields"
)

// 검색할 nested 필드 목록
nestedTagFieldList = []string{nestedTagsField, nestedProcessTagsField, nestedLogFieldsField}
```

### Object Tags (tags-as-fields)

`tags_as_fields` 설정을 활성화하면 태그가 **flat object**로도 저장된다:

```json
{
  "tag": {
    "http.method": "GET",
    "http_status_code": 200
  }
}
```

```go
// internal/storage/v1/elasticsearch/spanstore/reader.go
const (
    objectTagsField        = "tag"
    objectProcessTagsField = "process.tag"
)

objectTagFieldList = []string{objectTagsField, objectProcessTagsField}
```

### 태그 변환 로직

Writer는 `splitElevatedTags()`로 태그를 분류한다:

```go
// internal/storage/v1/elasticsearch/spanstore/writer.go
func (s *SpanWriter) splitElevatedTags(keyValues []dbmodel.KeyValue) ([]dbmodel.KeyValue, map[string]any) {
    if !s.allTagsAsFields && len(s.tagKeysAsFields) == 0 {
        return keyValues, nil
    }
    var tagsMap map[string]any
    var kvs []dbmodel.KeyValue
    for _, kv := range keyValues {
        if kv.Type != dbmodel.BinaryType && (s.allTagsAsFields || s.tagKeysAsFields[kv.Key]) {
            if tagsMap == nil {
                tagsMap = map[string]any{}
            }
            tagsMap[strings.ReplaceAll(kv.Key, ".", s.tagDotReplacement)] = kv.Value
        } else {
            kvs = append(kvs, kv)
        }
    }
    return kvs, tagsMap
}
```

핵심 동작:
1. `allTagsAsFields`가 true이거나, 해당 키가 `tagKeysAsFields` 맵에 있으면 → object 태그
2. Binary 타입은 항상 nested 태그로 유지
3. 키의 `.`은 `tagDotReplacement` 문자로 치환 (ES에서 `.`은 계층 구분자)

### 검색 시 양쪽 모두 조회

Reader는 태그 검색 시 object 필드와 nested 필드 **양쪽 모두를 조회**한다:

```go
// internal/storage/v1/elasticsearch/spanstore/reader.go
func (s *SpanReader) buildTagQuery(k string, v string) elastic.Query {
    objectTagListLen := len(objectTagFieldList)
    queries := make([]elastic.Query, len(nestedTagFieldList)+objectTagListLen)
    kd := s.dotReplacer.ReplaceDot(k)
    for i := range objectTagFieldList {
        queries[i] = s.buildObjectQuery(objectTagFieldList[i], kd, v)
    }
    for i := range nestedTagFieldList {
        queries[i+objectTagListLen] = s.buildNestedQuery(nestedTagFieldList[i], k, v)
    }
    return elastic.NewBoolQuery().Should(queries...)
}
```

---

## 7. ensureRequiredFields 메커니즘

v2 Factory 생성 시 `ensureRequiredFields()`가 호출되어 필수 태그를 항상 object 필드로 저장하도록 보장한다.

```go
// internal/storage/v2/elasticsearch/factory.go
const tagError = "error"

func ensureRequiredFields(cfg escfg.Configuration) escfg.Configuration {
    if cfg.Tags.AllAsFields {
        return cfg  // 모든 태그가 이미 필드이므로 추가 불필요
    }
    if cfg.Tags.Include != "" && !strings.HasSuffix(cfg.Tags.Include, ",") {
        cfg.Tags.Include += ","
    }
    cfg.Tags.Include += model.SpanKindKey + "," + tagError
    return cfg
}
```

이 함수가 추가하는 필드:
- `span.kind` -- span 유형 (client, server, producer, consumer)
- `error` -- 에러 발생 여부

**왜 필요한가?** UI에서 span 종류별 필터링과 에러 필터링은 가장 빈번한 쿼리이다. 이 필드들을 object 태그로 저장하면 nested query 없이 효율적으로 검색할 수 있다.

---

## 8. Service Operation 캐시

`internal/storage/v1/elasticsearch/spanstore/service_operation.go`의 `ServiceOperationStorage`는 서비스-오퍼레이션 쌍을 ES에 저장하고 캐시한다.

### 구조

```go
// internal/storage/v1/elasticsearch/spanstore/service_operation.go
type ServiceOperationStorage struct {
    client       func() es.Client
    logger       *zap.Logger
    serviceCache cache.Cache
}

func NewServiceOperationStorage(client func() es.Client, logger *zap.Logger,
    cacheTTL time.Duration) *ServiceOperationStorage {
    return &ServiceOperationStorage{
        client: client,
        logger: logger,
        serviceCache: cache.NewLRUWithOptions(
            100000,          // 최대 10만 항목
            &cache.Options{
                TTL: cacheTTL,  // 캐시 TTL
            },
        ),
    }
}
```

### 캐시 TTL 기본값

```go
// internal/storage/v1/elasticsearch/spanstore/writer.go
const (
    serviceCacheTTLDefault = 12 * time.Hour  // 기본 12시간
    indexCacheTTLDefault   = 48 * time.Hour
)
```

### 쓰기 시 중복 방지

서비스-오퍼레이션 쌍을 쓸 때 FNV 해시로 캐시 키를 생성하고, 이미 캐시에 있으면 ES에 쓰지 않는다:

```go
// internal/storage/v1/elasticsearch/spanstore/service_operation.go
func (s *ServiceOperationStorage) Write(indexName string, jsonSpan *dbmodel.Span) {
    service := dbmodel.Service{
        ServiceName:   jsonSpan.Process.ServiceName,
        OperationName: jsonSpan.OperationName,
    }
    cacheKey := hashCode(service)
    if !keyInCache(cacheKey, s.serviceCache) {
        s.client().Index().Index(indexName).Type(serviceType).
            Id(cacheKey).BodyJson(service).Add()
        writeCache(cacheKey, s.serviceCache)
    }
}

func hashCode(s dbmodel.Service) string {
    h := fnv.New64a()
    h.Write([]byte(s.ServiceName))
    h.Write([]byte(s.OperationName))
    return strconv.FormatUint(h.Sum64(), 16)
}
```

이 설계의 이점:
1. **쓰기 감소**: 같은 서비스-오퍼레이션 쌍을 12시간 동안 한 번만 ES에 쓴다
2. **멱등성**: FNV 해시를 문서 ID로 사용하므로 중복 쓰기도 안전하다
3. **메모리 효율**: LRU 캐시로 최대 10만 항목만 유지한다

### 읽기: 집계 쿼리

서비스 목록과 오퍼레이션 목록은 ES Terms Aggregation으로 조회한다:

```go
// internal/storage/v1/elasticsearch/spanstore/service_operation.go
func getServicesAggregation(maxDocCount int) elastic.Query {
    return elastic.NewTermsAggregation().
        Field(serviceName).
        Size(maxDocCount)
}

func getOperationsAggregation(maxDocCount int) elastic.Query {
    return elastic.NewTermsAggregation().
        Field(operationNameField).
        Size(maxDocCount)
}
```

---

## 9. SpanWriter 동작 원리

### 초기화

```go
// internal/storage/v1/elasticsearch/spanstore/writer.go
func NewSpanWriter(p SpanWriterParams) *SpanWriter {
    serviceCacheTTL := p.ServiceCacheTTL
    if p.ServiceCacheTTL == 0 {
        serviceCacheTTL = serviceCacheTTLDefault  // 12시간
    }

    writeAliasSuffix := ""
    if p.UseReadWriteAliases {
        if p.WriteAliasSuffix != "" {
            writeAliasSuffix = p.WriteAliasSuffix
        } else {
            writeAliasSuffix = "write"
        }
    }

    tags := map[string]bool{}
    for _, k := range p.TagKeysAsFields {
        tags[k] = true
    }

    serviceOperationStorage := NewServiceOperationStorage(p.Client, p.Logger, serviceCacheTTL)
    return &SpanWriter{
        client:            p.Client,
        logger:            p.Logger,
        writerMetrics:     spanstoremetrics.NewWriter(p.MetricsFactory, "spans"),
        serviceWriter:     serviceOperationStorage.Write,
        spanServiceIndex:  getSpanAndServiceIndexFn(p, writeAliasSuffix),
        tagKeysAsFields:   tags,
        allTagsAsFields:   p.AllTagsAsFields,
        tagDotReplacement: p.TagDotReplacement,
    }
}
```

### Span 쓰기 흐름

```go
// internal/storage/v1/elasticsearch/spanstore/writer.go
func (s *SpanWriter) WriteSpan(spanStartTime time.Time, span *dbmodel.Span) {
    s.writerMetrics.Attempts.Inc(1)
    s.convertNestedTagsToFieldTags(span)              // 1. 태그 분류
    spanIndexName, serviceIndexName := s.spanServiceIndex(spanStartTime) // 2. 인덱스 결정
    if serviceIndexName != "" {
        s.writeService(serviceIndexName, span)         // 3. 서비스 문서 쓰기
    }
    s.writeSpanToIndex(spanIndexName, span)            // 4. span 문서 쓰기
}
```

### 인덱스 이름 결정 전략

```go
// internal/storage/v1/elasticsearch/spanstore/writer.go
func getSpanAndServiceIndexFn(p SpanWriterParams, writeAlias string) spanAndServiceIndexFn {
    // 1. 명시적 별칭이 있으면 그대로 사용
    if p.SpanWriteAlias != "" && p.ServiceWriteAlias != "" {
        return func(_ time.Time) (string, string) {
            return p.SpanWriteAlias, p.ServiceWriteAlias
        }
    }
    // 2. 읽기/쓰기 별칭 모드
    if p.UseReadWriteAliases {
        return func(_ time.Time) (string, string) {
            return spanIndexPrefix + writeAlias, serviceIndexPrefix + writeAlias
        }
    }
    // 3. 기본: 날짜 기반 인덱스
    return func(date time.Time) (string, string) {
        return indexWithDate(spanIndexPrefix, p.SpanIndex.DateLayout, date),
               indexWithDate(serviceIndexPrefix, p.ServiceIndex.DateLayout, date)
    }
}
```

---

## 10. SpanReader와 쿼리 빌드

### FindTraceIDs 쿼리 구조

Reader는 복잡한 Bool 쿼리를 구성하여 트레이스를 검색한다:

```json
{
  "size": 0,
  "query": {
    "bool": {
      "must": [
        { "match": { "process.serviceName": "service1" } },
        { "match": { "operationName": "op1" } },
        { "range": { "startTimeMillis": { "gte": ..., "lte": ... } } },
        { "range": { "duration": { "gte": ..., "lte": ... } } },
        { "bool": { "should": [
            { "nested": { "path": "tags", "query": ... } },
            { "nested": { "path": "process.tags", "query": ... } },
            { "nested": { "path": "logs.fields", "query": ... } },
            { "bool": { "must": { "match": { "tag.key": ... } } } },
            { "bool": { "must": { "match": { "process.tag.key": ... } } } }
        ] } }
      ]
    }
  },
  "aggs": { "traceIDs": { "terms": { "size": 100, "field": "traceID" } } }
}
```

### 멀티 리드 (multiRead)

트레이스 ID 목록으로 전체 span을 가져올 때 `multiRead`를 사용한다:

```go
// internal/storage/v1/elasticsearch/spanstore/reader.go
func (s *SpanReader) multiRead(ctx context.Context, traceIDs []dbmodel.TraceID,
    startTime, endTime time.Time) ([]dbmodel.Trace, error) {
    // 양쪽으로 1시간 확장 (인덱스 경계를 걸치는 트레이스 처리)
    indices := s.timeRangeIndices(
        s.spanIndexPrefix,
        s.spanIndex.DateLayout,
        startTime.Add(-time.Hour),
        endTime.Add(time.Hour),
        cfg.RolloverFrequencyAsNegativeDuration(s.spanIndex.RolloverFrequency),
    )
    // MultiSearch로 병렬 조회
    // ...
}
```

핵심 설계:
1. **시간 범위 확장**: 인덱스 경계를 걸치는 트레이스를 위해 +-1시간 확장
2. **페이지네이션**: `search_after`를 사용하여 대량 span을 가진 트레이스를 처리
3. **MultiSearch**: 여러 트레이스 ID를 한 번의 요청으로 조회

---

## 11. 인덱스 롤오버 (Rollover)

### 롤오버 인덱스 네이밍

`cmd/es-rollover/app/index_options.go`에 정의된 롤오버 인덱스 형식:

```go
// cmd/es-rollover/app/index_options.go
const (
    writeAliasFormat    = "%s-write"
    readAliasFormat     = "%s-read"
    rolloverIndexFormat = "%s-000001"
)
```

예: `jaeger-span`의 경우
- 초기 인덱스: `jaeger-span-000001`
- 읽기 별칭: `jaeger-span-read`
- 쓰기 별칭: `jaeger-span-write`

### 롤오버 대상 인덱스

```go
// cmd/es-rollover/app/index_options.go
func RolloverIndices(archive, skipDependencies, adaptiveSampling bool, prefix string) []IndexOption {
    if archive {
        return []IndexOption{{prefix: prefix, indexType: "jaeger-span-archive", Mapping: "jaeger-span"}}
    }
    indexOptions := []IndexOption{
        {prefix: prefix, Mapping: "jaeger-span", indexType: "jaeger-span"},
        {prefix: prefix, Mapping: "jaeger-service", indexType: "jaeger-service"},
    }
    if !skipDependencies {
        indexOptions = append(indexOptions, IndexOption{...indexType: "jaeger-dependencies"})
    }
    if adaptiveSampling {
        indexOptions = append(indexOptions, IndexOption{...indexType: "jaeger-sampling"})
    }
    return indexOptions
}
```

---

## 12. ES Index Cleaner 도구

`cmd/es-index-cleaner/`는 오래된 Jaeger 인덱스를 삭제하는 독립 실행 도구이다.

### 사용법

```bash
jaeger-es-index-cleaner NUM_OF_DAYS http://HOSTNAME:PORT
```

### 동작 흐름

```
1. GetJaegerIndices(indexPrefix)  // ES에서 jaeger 인덱스 목록 조회
2. CalculateDeletionCutoff(...)   // 삭제 기준 날짜 계산
3. filter.Filter(indices)         // 패턴 매칭 + 날짜 필터링
4. DeleteIndices(indices)         // 필터링된 인덱스 삭제
```

### 인덱스 필터링 로직

```go
// cmd/es-index-cleaner/app/index_filter.go
func (i *IndexFilter) filterByPattern(indices []client.Index) []client.Index {
    var reg *regexp.Regexp
    switch {
    case i.Archive:
        reg, _ = regexp.Compile(fmt.Sprintf("^%sjaeger-span-archive-\\d{6}", i.IndexPrefix))
    case i.Rollover:
        reg, _ = regexp.Compile(fmt.Sprintf("^%sjaeger-(span|service|dependencies|sampling)-\\d{6}", i.IndexPrefix))
    default:
        reg, _ = regexp.Compile(fmt.Sprintf("^%sjaeger-(span|service|dependencies|sampling)-\\d{4}%s\\d{2}%s\\d{2}",
            i.IndexPrefix, i.IndexDateSeparator, i.IndexDateSeparator))
    }
    // ...
}
```

**중요**: write 별칭이 붙은 인덱스는 절대 삭제하지 않는다:

```go
if in.Aliases[i.IndexPrefix+"jaeger-span-write"] ||
    in.Aliases[i.IndexPrefix+"jaeger-service-write"] ||
    in.Aliases[i.IndexPrefix+"jaeger-span-archive-write"] ||
    in.Aliases[i.IndexPrefix+"jaeger-dependencies-write"] ||
    in.Aliases[i.IndexPrefix+"jaeger-sampling-write"] {
    continue
}
```

### 설정 옵션

```go
// cmd/es-index-cleaner/app/flags.go
type Config struct {
    IndexPrefix              string
    Archive                  bool    // 아카이브 인덱스만 삭제
    Rollover                 bool    // 롤오버 인덱스 삭제
    MasterNodeTimeoutSeconds int     // ES 마스터 노드 타임아웃
    IndexDateSeparator       string  // 날짜 구분자 (기본: "-")
    Username                 string
    Password                 string
}
```

---

## 13. ES Rollover 도구

`cmd/es-rollover/`는 세 가지 서브커맨드를 제공하는 인덱스 롤오버 관리 도구이다.

### 서브커맨드

| 커맨드 | 역할 | 사용법 |
|--------|------|--------|
| `init` | 인덱스, 템플릿, 별칭 생성 | `jaeger-es-rollover init http://HOST:PORT` |
| `rollover` | 새 쓰기 인덱스로 롤오버 | `jaeger-es-rollover rollover http://HOST:PORT` |
| `lookback` | 읽기 별칭에서 오래된 인덱스 제거 | `jaeger-es-rollover lookback http://HOST:PORT` |

### Init 액션

```go
// cmd/es-rollover/app/init/action.go
func (c Action) init(version uint, indexopt app.IndexOption) error {
    // 1. 매핑 생성
    mapping, err := c.getMapping(version, mappingType)
    // 2. 인덱스 템플릿 생성
    err = c.IndicesClient.CreateTemplate(mapping, indexopt.TemplateName())
    // 3. 초기 인덱스 생성 (예: jaeger-span-000001)
    index := indexopt.InitialRolloverIndex()
    err = createIndexIfNotExist(c.IndicesClient, index)
    // 4. 읽기/쓰기 별칭 생성
    readAlias := indexopt.ReadAliasName()
    writeAlias := indexopt.WriteAliasName()
    // ...
}
```

### Rollover 액션

```go
// cmd/es-rollover/app/rollover/action.go
func (a *Action) rollover(indexSet app.IndexOption) error {
    // 1. 조건에 따라 롤오버 실행
    conditionsMap := map[string]any{}
    if a.Conditions != "" {
        json.Unmarshal([]byte(a.Config.Conditions), &conditionsMap)
    }
    writeAlias := indexSet.WriteAliasName()
    readAlias := indexSet.ReadAliasName()
    err := a.IndicesClient.Rollover(writeAlias, conditionsMap)
    // 2. 새 인덱스에 읽기 별칭 추가
    jaegerIndex, _ := a.IndicesClient.GetJaegerIndices(a.Config.IndexPrefix)
    indicesWithWriteAlias := filter.ByAlias(jaegerIndex, []string{writeAlias})
    for _, index := range indicesWithWriteAlias {
        aliases = append(aliases, client.Alias{Index: index.Index, Name: readAlias})
    }
    return a.IndicesClient.CreateAlias(aliases)
}
```

### Lookback 액션

```go
// cmd/es-rollover/app/lookback/action.go
func (a *Action) lookback(indexSet app.IndexOption) error {
    // 1. 읽기 별칭의 모든 인덱스 조회
    readAliasIndices := filter.ByAlias(jaegerIndex, []string{readAliasName})
    // 2. 쓰기 별칭이 있는 인덱스 제외
    excludedWriteIndex := filter.ByAliasExclude(readAliasIndices, []string{writeAlias})
    // 3. 날짜 기준으로 오래된 인덱스 필터링
    finalIndices := filter.ByDate(excludedWriteIndex, getTimeReference(...))
    // 4. 읽기 별칭에서 제거 (인덱스 자체는 삭제하지 않음)
    return a.IndicesClient.DeleteAlias(aliases)
}
```

### 롤오버 라이프사이클

```
[Day 1] init
  jaeger-span-000001 ← jaeger-span-read, jaeger-span-write

[Day 2] rollover (조건 충족 시)
  jaeger-span-000001 ← jaeger-span-read
  jaeger-span-000002 ← jaeger-span-read, jaeger-span-write

[Day 3] rollover
  jaeger-span-000001 ← jaeger-span-read
  jaeger-span-000002 ← jaeger-span-read
  jaeger-span-000003 ← jaeger-span-read, jaeger-span-write

[Day 3] lookback (7일 설정)
  jaeger-span-000001 (읽기 별칭 제거, 인덱스 유지)
  jaeger-span-000002 ← jaeger-span-read
  jaeger-span-000003 ← jaeger-span-read, jaeger-span-write
```

---

## 14. ILM (Index Lifecycle Management) 지원

ILM은 ES 7+에서 제공하는 인덱스 생명주기 관리 기능이다.

### 제약 조건

```go
// internal/storage/elasticsearch/config/config.go
func (c *Configuration) Validate() error {
    if c.UseILM && !c.UseReadWriteAliases {
        return errors.New("UseILM must always be used in conjunction with UseReadWriteAliases")
    }
    if c.CreateIndexTemplates && c.UseILM {
        return errors.New("when UseILM is set true, CreateIndexTemplates must be set to false")
    }
    return nil
}
```

ILM 사용 시 규칙:
1. `UseReadWriteAliases`가 반드시 true여야 한다
2. `CreateIndexTemplates`는 false여야 한다 (es-rollover init이 템플릿을 생성)
3. ES 버전 7 이상이어야 한다

### ILM init 흐름

```go
// cmd/es-rollover/app/init/action.go
func (c Action) Do() error {
    if c.Config.UseILM {
        if version < ilmVersionSupport {  // 7
            return errors.New("ILM is supported only for ES version 7+")
        }
        policyExist, _ := c.ILMClient.Exists(c.Config.ILMPolicyName)
        if !policyExist {
            return fmt.Errorf("ILM policy %s doesn't exist", c.Config.ILMPolicyName)
        }
    }
    // ...
}
```

별칭 생성 시 ILM이 활성화되면 `IsWriteIndex: true`를 설정한다:

```go
aliases = append(aliases, client.Alias{
    Index:        index,
    Name:         writeAlias,
    IsWriteIndex: c.Config.UseILM,  // ILM에서는 write index 마킹 필요
})
```

---

## 15. Bulk Processing

ES에 대한 쓰기는 Bulk Processor를 통해 배치 처리된다.

### 설정

```go
// internal/storage/elasticsearch/config/config.go
type BulkProcessing struct {
    MaxBytes      int           // 플러시 트리거 바이트 수
    MaxActions    int           // 플러시 트리거 액션 수
    FlushInterval time.Duration // 자동 플러시 간격
    Workers       int           // 동시 워커 수
}
```

### 벌크 처리 생성

```go
// internal/storage/elasticsearch/config/config.go (NewClient)
bulkProc, err := rawClient.BulkProcessor().
    Before(func(id int64, _ []elastic.BulkableRequest) {
        bcb.startTimes.Store(id, time.Now())
    }).
    After(bcb.invoke).
    BulkSize(c.BulkProcessing.MaxBytes).
    Workers(c.BulkProcessing.Workers).
    BulkActions(c.BulkProcessing.MaxActions).
    FlushInterval(c.BulkProcessing.FlushInterval).
    Do(ctx)
```

### 메트릭 수집

벌크 콜백에서 성능 메트릭을 수집한다:

```go
// internal/storage/elasticsearch/config/config.go
func (bcb *bulkCallback) invoke(id int64, requests []elastic.BulkableRequest,
    response *elastic.BulkResponse, err error) {
    latency := time.Since(start.(time.Time))
    if err != nil {
        bcb.sm.LatencyErr.Record(latency)
    } else {
        bcb.sm.LatencyOk.Record(latency)
    }
    total := len(requests)
    bcb.sm.Attempts.Inc(int64(total))
    bcb.sm.Inserts.Inc(int64(total - failed))
    bcb.sm.Errors.Inc(int64(failed))
}
```

---

## 16. OpenSearch 호환성

Jaeger는 OpenSearch를 ES 7.x 호환 모드로 처리한다.

```go
// internal/storage/elasticsearch/config/config.go (NewClient)
if strings.Contains(pingResult.TagLine, "OpenSearch") {
    if pingResult.Version.Number[0] == '1' {
        logger.Info("OpenSearch 1.x detected, using ES 7.x index mappings")
        esVersion = 7
    }
    if pingResult.Version.Number[0] == '2' {
        logger.Info("OpenSearch 2.x detected, using ES 7.x index mappings")
        esVersion = 7
    }
    if pingResult.Version.Number[0] == '3' {
        logger.Info("OpenSearch 3.x detected, using ES 7.x index mappings")
        esVersion = 7
    }
}
```

핵심 호환성 전략:
- OpenSearch 1.x, 2.x, 3.x 모두 ES 7.x 매핑을 사용한다
- Ping 응답의 `TagLine`에 "OpenSearch"가 포함되어 있는지 확인한다
- 별도의 설정 키 없이 **동일한 코드**로 두 엔진을 모두 지원한다
- 자동 감지가 작동하지 않는 경우 `Version` 설정으로 수동 지정 가능

---

## 17. 인증 메커니즘

### 지원하는 인증 방식

```go
// internal/storage/elasticsearch/config/config.go
type Authentication struct {
    BasicAuthentication configoptional.Optional[BasicAuthentication]  // Basic Auth
    BearerTokenAuth     configoptional.Optional[TokenAuthentication]  // Bearer Token
    APIKeyAuth          configoptional.Optional[TokenAuthentication]  // API Key
    configauth.Config                                                 // OTel 인증 확장
}
```

### Basic Authentication

```go
type BasicAuthentication struct {
    Username         string
    Password         string
    PasswordFilePath string        // 파일에서 패스워드 로드
    ReloadInterval   time.Duration // 패스워드 파일 리로드 간격
}
```

### 패스워드 파일 자동 리로드

FactoryBase는 패스워드 파일 변경을 감시하여 자동으로 ES 클라이언트를 교체한다:

```go
// internal/storage/v1/elasticsearch/factory.go
func (f *FactoryBase) onClientPasswordChange(cfg *config.Configuration,
    client *atomic.Pointer[es.Client], mf metrics.Factory) {
    newPassword, err := loadTokenFromFile(basicAuth.PasswordFilePath)
    // ... 새 패스워드로 설정 복사 ...
    newClient, err := f.newClientFn(context.Background(), &newCfg, f.logger, mf, nil)
    // 원자적 클라이언트 교체
    if oldClient := *client.Swap(&newClient); oldClient != nil {
        oldClient.Close()
    }
}
```

### HTTP Transport 레이어

인증은 HTTP RoundTripper 레이어에서 처리된다:

```go
// internal/storage/elasticsearch/config/config.go
func GetHTTPRoundTripper(ctx context.Context, c *Configuration, ...) (http.RoundTripper, error) {
    var authMethods []auth.Method
    // API Key → Bearer Token → Basic Auth 순서로 추가
    if c.Authentication.APIKeyAuth.HasValue() { ... }
    if c.Authentication.BearerTokenAuth.HasValue() { ... }
    if c.Authentication.BasicAuthentication.HasValue() { ... }

    var roundTripper http.RoundTripper = transport
    if len(authMethods) > 0 {
        roundTripper = &auth.RoundTripper{
            Transport: transport,
            Auths:     authMethods,
        }
    }
    // OTel HTTP Authenticator 확장 (예: SigV4)
    if httpAuth != nil {
        wrappedRT, _ := httpAuth.RoundTripper(roundTripper)
        return wrappedRT, nil
    }
    return roundTripper, nil
}
```

---

## 18. 아키텍처 다이어그램 종합

### 전체 데이터 흐름

```
┌─────────────┐     ┌──────────────────────────────────────────────┐
│   Span 수신  │────→│  v2 Factory                                  │
│  (Collector) │     │  ┌──────────────────────────────────────┐   │
│              │     │  │ ensureRequiredFields                  │   │
│              │     │  │ (span.kind + error를 tags_as_fields) │   │
│              │     │  └──────────────────────────────────────┘   │
│              │     │         │                                    │
│              │     │  ┌──────────────────────────────────────┐   │
│              │     │  │ SpanWriter                            │   │
│              │     │  │  1. convertNestedTagsToFieldTags()   │   │
│              │     │  │  2. spanServiceIndex(startTime)      │   │
│              │     │  │  3. writeService() [캐시 체크]        │   │
│              │     │  │  4. writeSpanToIndex()               │   │
│              │     │  └──────────────────────────────────────┘   │
│              │     │         │                                    │
│              │     │  ┌──────────────────────────────────────┐   │
│              │     │  │ BulkProcessor                         │   │
│              │     │  │  - Workers: N                         │   │
│              │     │  │  - FlushInterval: 200ms              │   │
│              │     │  │  - MaxActions / MaxBytes              │   │
│              │     │  └──────────────────────────────────────┘   │
│              │     └─────────────────────│────────────────────────┘
│              │                           │
│              │                           ▼
│              │              ┌──────────────────────┐
│              │              │   Elasticsearch       │
│              │              │                       │
│              │              │  jaeger-span-YYYY-MM-DD
│              │              │  jaeger-service-YYYY-MM-DD
│              │              │  jaeger-dependencies-YYYY-MM-DD
│              │              └──────────────────────┘
│              │                           │
│              │                           │
│   Query 요청 │     ┌──────────────────────│────────────────────────┐
│  (UI/API)    │────→│  SpanReader                                   │
│              │     │  1. timeRangeIndices() → 인덱스 목록 계산     │
│              │     │  2. buildFindTraceIDsQuery()                  │
│              │     │     - serviceName, operation, duration filter │
│              │     │     - tag query (nested + object 양쪽)       │
│              │     │  3. Terms Aggregation → traceID 수집         │
│              │     │  4. multiRead() → 전체 span 조회             │
│              │     │     - MultiSearch API                         │
│              │     │     - search_after 페이지네이션              │
└─────────────┘     └───────────────────────────────────────────────┘
```

### 인덱스 관리 도구

```
┌───────────────────────────────────────┐
│  es-rollover                          │
│  ┌──────────┐                         │
│  │   init   │ → 템플릿 + 초기 인덱스  │
│  │          │   + 읽기/쓰기 별칭      │
│  └──────────┘                         │
│  ┌──────────┐                         │
│  │ rollover │ → 새 쓰기 인덱스 생성   │
│  │          │   + 읽기 별칭 추가      │
│  └──────────┘                         │
│  ┌──────────┐                         │
│  │ lookback │ → 읽기 별칭에서         │
│  │          │   오래된 인덱스 제거    │
│  └──────────┘                         │
└───────────────────────────────────────┘

┌───────────────────────────────────────┐
│  es-index-cleaner                     │
│                                       │
│  NUM_OF_DAYS 기반으로                 │
│  오래된 인덱스 물리적 삭제            │
│  (write 별칭 인덱스 보호)             │
└───────────────────────────────────────┘
```

### 매핑 템플릿 시스템

```
internal/storage/v1/elasticsearch/mappings/
├── jaeger-span-7.json          # ES 7.x span 매핑
├── jaeger-span-8.json          # ES 8.x span 매핑
├── jaeger-service-7.json       # ES 7.x service 매핑
├── jaeger-service-8.json       # ES 8.x service 매핑
├── jaeger-dependencies-7.json  # ES 7.x dependencies 매핑
├── jaeger-dependencies-8.json  # ES 8.x dependencies 매핑
├── jaeger-sampling-7.json      # ES 7.x sampling 매핑
├── jaeger-sampling-8.json      # ES 8.x sampling 매핑
└── mapping.go                  # Go 템플릿 렌더러
```

```go
// internal/storage/v1/elasticsearch/mappings/mapping.go
func (mb *MappingBuilder) GetMapping(mappingType MappingType) (string, error) {
    templateOpts := mb.getMappingTemplateOptions(mappingType)
    esVersion := min(mb.EsVersion, 8)  // ES v9는 v8 템플릿 재사용
    return mb.renderMapping(
        fmt.Sprintf("%s-%d.json", mappingType.String(), esVersion),
        templateOpts,
    )
}
```

---

## 요약

Jaeger의 Elasticsearch 스토리지는 다음과 같은 핵심 설계 원칙을 따른다:

1. **시간 기반 파티셔닝**: 날짜별 인덱스로 데이터를 분산하고, 오래된 데이터의 효율적 삭제를 지원
2. **이중 태그 스키마**: nested + object 태그를 병행하여 유연한 검색과 Kibana 호환성 확보
3. **롤오버 지원**: 별칭 기반 읽기/쓰기 분리로 무중단 인덱스 관리
4. **자동 감지**: ES 버전, OpenSearch 호환성을 자동으로 감지
5. **벌크 처리**: 고성능 쓰기를 위한 BulkProcessor와 서비스 캐시
6. **인증 유연성**: Basic, Bearer Token, API Key, OTel 확장까지 다양한 인증 지원
7. **무중단 패스워드 교체**: 원자적 클라이언트 교체로 패스워드 변경 시 다운타임 없음
