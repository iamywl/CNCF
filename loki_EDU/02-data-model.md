# Loki 데이터 모델

## 1. 개요

Loki의 데이터 모델은 Prometheus의 레이블 체계를 기반으로 하되, **로그 내용을 인덱싱하지 않는** 핵심 철학을 구현한다. 메트릭 시스템과 달리 Loki는 레이블 셋(label set)으로 스트림을 식별하고, 각 스트림에 타임스탬프가 붙은 로그 라인을 청크 단위로 압축 저장한다.

```
┌─────────────────────────────────────────────────────┐
│                    Loki 데이터 모델                    │
│                                                     │
│  ┌─────────────┐   ┌─────────────┐                  │
│  │ Label Set   │──▶│   Stream    │                  │
│  │{app="api",  │   │ (고유 식별)   │                  │
│  │ env="prod"} │   └──────┬──────┘                  │
│  └─────────────┘          │                         │
│                    ┌──────┴──────┐                   │
│                    │  Chunk(s)   │                   │
│                    │ ┌────────┐  │                   │
│                    │ │ Block  │  │ ← 압축된 로그 라인들  │
│                    │ ├────────┤  │                   │
│                    │ │ Block  │  │                   │
│                    │ ├────────┤  │                   │
│                    │ │ Head   │  │ ← 현재 쓰기 중      │
│                    │ └────────┘  │                   │
│                    └─────────────┘                   │
└─────────────────────────────────────────────────────┘
```

---

## 2. 핵심 데이터 타입

### 2.1 Stream과 Entry

로그 수집의 기본 단위. Push API를 통해 클라이언트가 전송하는 데이터 구조이다.

소스: `pkg/push/types.go:16-28`

```go
// Stream은 고유한 레이블 셋과 그에 속하는 로그 엔트리 집합이다.
type Stream struct {
    Labels  string  `json:"labels"`     // 레이블 셋 문자열 (예: `{app="api"}`)
    Entries []Entry `json:"entries"`    // 로그 엔트리 배열
    Hash    uint64  `json:"-"`          // 레이블의 해시값 (라우팅에 사용)
}

// Entry는 타임스탬프가 붙은 개별 로그 라인이다.
type Entry struct {
    Timestamp          time.Time     `json:"ts"`
    Line               string        `json:"line"`
    StructuredMetadata LabelsAdapter `json:"structuredMetadata,omitempty"`
    Parsed             LabelsAdapter `json:"parsed,omitempty"`
}
```

**핵심 포인트:**
- `Stream.Labels`는 `{app="api", env="prod"}` 형태의 문자열로, **스트림의 고유 식별자** 역할
- `Stream.Hash`는 레이블 셋의 해시값으로, Ring에서 담당 Ingester를 결정하는 데 사용
- `Entry.StructuredMetadata`는 로그 라인에 부착되는 추가 키-값 쌍 (인덱싱되지 않음)
- `Entry.Parsed`는 쿼리 시 파서가 추출한 레이블 (런타임 전용)

### 2.2 LabelAdapter

Prometheus의 `labels.Label` 타입과 호환되는 레이블 어댑터.

소스: `pkg/push/types.go:59-60`

```go
// LabelAdapter는 Prometheus labels.Label의 복사본이다.
// Prometheus 패키지를 직접 임포트하면 의존성이 커지므로 별도 정의한다.
type LabelAdapter struct {
    Name, Value string
}

type LabelsAdapter []LabelAdapter
```

**왜 별도 타입인가?**
- `pkg/push`는 외부 프로젝트(Promtail, Alloy 등)에서 임포트하는 패키지
- Prometheus 전체 의존성을 가져오면 바이너리 크기와 빌드 시간 증가
- 동일한 메모리 레이아웃을 유지하여 `unsafe.Pointer`로 캐스팅 가능

---

## 3. 청크 인코딩

### 3.1 MemChunk 구조

Ingester가 인메모리에서 로그를 저장하는 핵심 구조. 로그 라인을 블록 단위로 압축하여 메모리를 절약한다.

소스: `pkg/chunkenc/memchunk.go:121-142`

```go
type MemChunk struct {
    blockSize    int              // 비압축 바이트 기준 블록 크기
    targetSize   int              // 압축 바이트 기준 목표 청크 크기
    symbolizer   *symbolizer      // 구조화 메타데이터 심볼 테이블
    blocks       []block          // 완성된 압축 블록들
    cutBlockSize int              // 모든 블록의 압축 바이트 합계
    head         HeadBlock        // 현재 쓰기 중인 비압축 블록
    format       byte             // 청크 포맷 버전 (V1~V4)
    encoding     compression.Codec // 압축 코덱 (gzip, lz4, snappy, zstd, flate)
    headFmt      HeadBlockFmt     // 헤드 블록 포맷
    compressedSize int            // 전체 압축 크기
}
```

### 3.2 Block 구조

소스: `pkg/chunkenc/memchunk.go:144-153`

```go
type block struct {
    b                []byte  // 압축된 바이트
    numEntries       int     // 엔트리 수
    mint, maxt       int64   // 최소/최대 타임스탬프
    offset           int     // 청크 내 오프셋
    uncompressedSize int     // 비압축 크기
}

// 비압축 상태의 헤드 블록
type headBlock struct {
    entries []entry           // 비압축 엔트리 목록
    size    int               // 비압축 바이트 크기
    mint, maxt int64          // 최소/최대 타임스탬프
}
```

### 3.3 청크 라이프사이클

```
Entry 수신
    │
    ▼
┌──────────┐    블록 크기 도달    ┌──────────┐
│ HeadBlock│ ─────────────────▶ │  Block   │ (압축)
│ (비압축)  │                    │ (압축됨)  │
└──────────┘                    └──────────┘
    │                               │
    │                               │ 반복
    │                               ▼
    │                    ┌────────────────┐
    │                    │  blocks[]      │
    │                    │  Block 0 (압축) │
    │                    │  Block 1 (압축) │
    │                    │  ...           │
    │                    └────────┬───────┘
    │                             │ 청크 크기 도달
    │                             ▼
    │                    ┌────────────────┐
    │                    │ Object Store   │
    │                    │ (S3/GCS/Azure) │
    │                    └────────────────┘
```

### 3.4 청크 포맷 버전

소스: `pkg/chunkenc/memchunk.go:31-36`

| 버전 | 상수 | 헤드 블록 포맷 | 특징 |
|------|------|-------------|------|
| V1 | `ChunkFormatV1` | Ordered | 초기 포맷, 순서 보장 |
| V2 | `ChunkFormatV2` | Ordered | 블록 메타데이터 추가 |
| V3 | `ChunkFormatV3` | Unordered | 비순서 쓰기 지원 |
| V4 | `ChunkFormatV4` | UnorderedWithStructuredMetadata | 구조화 메타데이터 지원 |

### 3.5 압축 코덱

Loki는 블록 레벨에서 다양한 압축 코덱을 지원한다:

| 코덱 | 압축률 | 속도 | 용도 |
|------|-------|------|------|
| **gzip** | 높음 | 느림 | 스토리지 비용 최소화 |
| **lz4** | 중간 | 빠름 | 균형 (기본 추천) |
| **snappy** | 낮음 | 매우 빠름 | 실시간 처리 |
| **zstd** | 매우 높음 | 중간 | 최대 압축 |
| **flate** | 높음 | 느림 | gzip 대안 |

---

## 4. Chunk 인터페이스

소스: `pkg/chunkenc/interface.go:52-73`

```go
type Chunk interface {
    Bounds() (time.Time, time.Time)              // 청크의 시간 범위
    SpaceFor(*logproto.Entry) bool               // 추가 엔트리를 수용할 공간이 있는지
    Append(*logproto.Entry) (bool, error)         // 엔트리 추가 (중복 시 true 반환)
    Iterator(...) (iter.EntryIterator, error)      // 로그 반복자
    SampleIterator(...) iter.SampleIterator        // 메트릭 샘플 반복자
    Blocks(mintT, maxtT time.Time) []Block        // 시간 범위의 블록 목록
    Size() int                                     // 엔트리 수
    Bytes() ([]byte, error)                        // 직렬화
    BlockCount() int                               // 블록 수
    Utilization() float64                          // 사용률
    UncompressedSize() int                         // 비압축 크기
    CompressedSize() int                           // 압축 크기
    Close() error
    Encoding() compression.Codec                   // 압축 코덱
    Rewrite(filter filter.Func) (Chunk, error)     // 필터링된 새 청크 생성
}
```

**Block 인터페이스:**

소스: `pkg/chunkenc/interface.go:76-89`

```go
type Block interface {
    MinTime() int64                             // 블록 최소 타임스탬프
    MaxTime() int64                             // 블록 최대 타임스탬프
    Offset() int                                // 청크 내 오프셋 위치
    Entries() int                               // 엔트리 수
    Iterator(ctx, pipeline) iter.EntryIterator   // 엔트리 반복자
    SampleIterator(ctx, extractor) iter.SampleIterator // 샘플 반복자
}
```

---

## 5. Ingester 데이터 구조

### 5.1 stream (인메모리 스트림)

Ingester 내부에서 각 로그 스트림을 관리하는 구조체.

소스: `pkg/ingester/stream.go:41-89`

```go
type stream struct {
    limiter      *StreamRateLimiter     // 스트림별 레이트 리밋
    cfg          *Config
    tenant       string                 // 테넌트 ID
    chunks       []chunkDesc            // 청크 목록 (newest at n-1)
    fp           model.Fingerprint      // 레이블 핑거프린트
    chunkMtx     sync.RWMutex

    labels           labels.Labels      // Prometheus 레이블
    labelsString     string             // 레이블 문자열 표현
    labelHash        uint64             // 레이블 해시
    labelHashNoShard uint64             // 샤드 제외 해시

    lastLine    line                    // 마지막 푸시된 라인 (중복 방지)
    highestTs   time.Time              // 수용된 최대 타임스탬프

    tailers     map[uint32]*tailer     // 실시간 테일링 구독자
    entryCt     int64                  // 수용된 엔트리 카운터

    unorderedWrites      bool          // 비순서 쓰기 허용 여부
    streamRateCalculator *StreamRateCalculator

    chunkFormat          byte          // 청크 포맷 버전
    chunkHeadBlockFormat chunkenc.HeadBlockFmt
}

type chunkDesc struct {
    chunk       *chunkenc.MemChunk     // 실제 청크 데이터
    closed      bool                   // 플러시 대상 여부
    synced      bool                   // 스토리지에 동기화 완료
    flushed     time.Time             // 플러시 시각
    reason      string                 // 플러시 이유
    lastUpdated time.Time             // 마지막 업데이트 시각
}
```

### 5.2 instance (테넌트 인스턴스)

테넌트별로 생성되는 Ingester 인스턴스. 모든 스트림과 인덱스를 관리한다.

소스: `pkg/ingester/instance.go:90-133`

```go
type instance struct {
    cfg     *Config
    buf     []byte                     // 핑거프린트 계산 버퍼
    streams *streamsMap                // 스트림 맵 (fp → stream)
    index   *index.Multi              // 인메모리 역인덱스
    mapper  *FpMapper                 // 핑거프린트 충돌 매퍼

    instanceID string                 // 테넌트 ID

    limiter            *Limiter        // 글로벌 제한
    streamCountLimiter *streamCountLimiter
    ownedStreamsSvc    *ownedStreamService

    wal WAL                           // Write-Ahead Log

    chunkFilter          chunk.RequestChunkFilterer
    pipelineWrapper      log.PipelineWrapper
    streamRateCalculator *StreamRateCalculator

    schemaconfig *config.SchemaConfig
    tenantsRetention *retention.TenantsRetention
}
```

### 5.3 역인덱스 (Inverted Index)

Ingester 내부에서 레이블 기반 스트림 검색을 위한 인메모리 역인덱스.

소스: `pkg/ingester/index/index.go`

```go
type Interface interface {
    Add(labels []logproto.LabelAdapter, fp model.Fingerprint) labels.Labels
    Lookup(matchers []*labels.Matcher, shard *logql.Shard) ([]model.Fingerprint, error)
    LabelNames(shard *logql.Shard) ([]string, error)
    LabelValues(name string, shard *logql.Shard) ([]string, error)
    Delete(labels labels.Labels, fp model.Fingerprint)
}

// InvertedIndex는 샤딩된 역인덱스이다.
type InvertedIndex struct {
    totalShards uint32
    shards      []*indexShard
}
```

**왜 샤딩하는가?**
- 동시 접근 시 락 경합 감소
- 각 샤드가 독립 뮤텍스를 가져 병렬 읽기/쓰기 가능
- 레이블 해시값으로 샤드 결정

---

## 6. 스토리지 청크

### 6.1 Storage Chunk

오브젝트 스토리지에 저장되는 영구 청크 표현.

소스: `pkg/storage/chunk/chunk.go:42-55`

```go
type Chunk struct {
    logproto.ChunkRef              // 청크 참조 (핑거프린트, 테넌트, 시간 범위, 체크섬)
    Metric   labels.Labels         // 레이블
    Encoding Encoding              // 인코딩 타입
    Data     Data                  // 실제 청크 데이터
    encoded  []byte                // 인코딩된 바이트 (캐시용)
}
```

### 6.2 ChunkRef (Protobuf)

```protobuf
message ChunkRef {
    uint64 fingerprint = 1;       // 스트림 핑거프린트
    string user_id     = 2;       // 테넌트 ID
    int64  from        = 3;       // 시작 타임스탬프
    int64  through     = 4;       // 종료 타임스탬프
    uint32 checksum    = 5;       // 데이터 체크섬
}
```

**오브젝트 스토리지 키 구조:**
```
{tenant_id}/{table_name}/{fingerprint}:{from}:{through}:{checksum}
```

---

## 7. TSDB 인덱스

### 7.1 인덱스 인터페이스

TSDB 포맷의 인덱스 파일로 레이블 기반 청크 검색을 지원한다.

소스: `pkg/storage/stores/shipper/indexshipper/tsdb/index.go:22-44`

```go
type Index interface {
    Bounded
    SetChunkFilterer(chunkFilter chunk.RequestChunkFilterer)
    Close() error

    GetChunkRefs(ctx context.Context, userID string,
        from, through model.Time,
        res []logproto.ChunkRefWithSizingInfo,
        fpFilter index.FingerprintFilter,
        matchers ...*labels.Matcher,
    ) ([]logproto.ChunkRefWithSizingInfo, error)

    Series(ctx context.Context, userID string,
        from, through model.Time,
        res []Series,
        fpFilter index.FingerprintFilter,
        matchers ...*labels.Matcher,
    ) ([]Series, error)

    LabelNames(...) ([]string, error)
    LabelValues(...) ([]string, error)
    Stats(...) error
    Volume(...) error
}
```

### 7.2 TSDB 파일 구조

소스: `pkg/storage/stores/shipper/indexshipper/tsdb/index/index.go:46-79`

```
┌──────────────────────────────────┐
│         TSDB Index File          │
│                                  │
│  Magic: 0xBAAAD700              │
│  Version: 1-3                    │
│                                  │
│  ┌──────────────────────────┐   │
│  │ Symbol Table             │   │ ← 레이블 이름/값 문자열
│  ├──────────────────────────┤   │
│  │ Series Section           │   │ ← 시리즈(스트림) 정의 + 청크 목록
│  ├──────────────────────────┤   │
│  │ Label Indices            │   │ ← 레이블별 시리즈 목록
│  ├──────────────────────────┤   │
│  │ Postings                 │   │ ← 역인덱스 (레이블 → 시리즈 참조)
│  ├──────────────────────────┤   │
│  │ Fingerprint Offsets      │   │ ← 핑거프린트 → 시리즈 오프셋
│  ├──────────────────────────┤   │
│  │ TOC (Table of Contents)  │   │ ← 각 섹션의 오프셋
│  └──────────────────────────┘   │
└──────────────────────────────────┘
```

### 7.3 ChunkMeta (인덱스 내 청크 메타데이터)

소스: `pkg/storage/stores/shipper/indexshipper/tsdb/index/chunk.go:12-67`

```go
type ChunkMeta struct {
    Checksum         uint32    // 데이터 체크섬
    MinTime, MaxTime int64     // 시간 범위
    KB               uint32    // KB 단위 크기
    Entries          uint32    // 엔트리 수
}

type ChunkMetas []ChunkMeta

// (MinTime, MaxTime, Checksum) 순으로 정렬
func (c ChunkMetas) Less(i, j int) bool { ... }

// 정렬 + 중복 제거
func (c ChunkMetas) Finalize() ChunkMetas { ... }
```

---

## 8. gRPC 프로토콜 정의

### 8.1 Pusher 서비스

소스: `pkg/logproto/logproto.proto`

```protobuf
service Pusher {
    rpc Push(PushRequest) returns (PushResponse) {};
}

message PushRequest {
    repeated StreamAdapter streams = 1;
}

message StreamAdapter {
    string labels = 1;
    repeated EntryAdapter entries = 2;
    uint64 hash = 3;
}

message EntryAdapter {
    google.protobuf.Timestamp timestamp = 1;
    string line = 2;
    repeated LabelPairAdapter structuredMetadata = 3;
}
```

### 8.2 Querier 서비스

```protobuf
service Querier {
    rpc Query(QueryRequest) returns (stream QueryResponse) {}
    rpc QuerySample(SampleQueryRequest) returns (stream SampleQueryResponse) {}
    rpc Label(LabelRequest) returns (LabelResponse) {}
    rpc Tail(TailRequest) returns (stream TailResponse) {}
    rpc Series(SeriesRequest) returns (SeriesResponse) {}
    rpc GetStats(IndexStatsRequest) returns (IndexStatsResponse) {}
    rpc GetVolume(VolumeRequest) returns (VolumeResponse) {}
}

message QueryRequest {
    string selector  = 1;       // LogQL 셀렉터
    uint32 limit     = 2;       // 결과 제한
    Timestamp start  = 3;       // 시작 시각
    Timestamp end    = 4;       // 종료 시각
    Direction direction = 5;    // FORWARD 또는 BACKWARD
    repeated string shards = 7; // 샤드 필터
    Plan plan = 9;              // 실행 계획
}

message QueryResponse {
    repeated StreamAdapter streams = 1;
    stats.Ingester stats = 2;   // 쿼리 통계
}

enum Direction {
    FORWARD  = 0;               // 시간 정순
    BACKWARD = 1;               // 시간 역순
}
```

### 8.3 Sample (메트릭 추출)

```protobuf
message Sample {
    int64  timestamp = 1;
    double value     = 2;
    uint64 hash      = 3;
}

message Series {
    string labels          = 1;
    repeated Sample samples = 2;
    uint64 streamHash      = 3;
}
```

---

## 9. LogQL AST

### 9.1 Expression 인터페이스

LogQL 쿼리의 추상 구문 트리(AST) 타입 계층.

소스: `pkg/logql/syntax/ast.go:30-104`

```go
// Expr은 모든 LogQL 표현식의 기본 인터페이스이다.
type Expr interface {
    Shardable(topLevel bool) bool   // 병렬 샤딩 가능 여부
    Walkable                         // AST 순회
    fmt.Stringer                     // 문자열 표현
    Pretty(level int) string         // 포맷팅된 문자열
}

// LogSelectorExpr은 로그 스트림을 선택하는 표현식이다.
type LogSelectorExpr interface {
    Matchers() []*labels.Matcher     // 레이블 매처
    Pipeline() (Pipeline, error)     // 파이프라인 스테이지
    HasFilter() bool                 // 필터 포함 여부
    Expr
}

// SampleExpr은 메트릭 샘플을 생성하는 표현식이다.
type SampleExpr interface {
    Selector() (LogSelectorExpr, error)
    Extractors() ([]SampleExtractor, error)
    Expr
}
```

### 9.2 주요 AST 노드

| 노드 | 설명 | LogQL 예시 |
|------|------|-----------|
| `MatchersExpr` | 레이블 매칭 | `{app="api"}` |
| `PipelineExpr` | 매처 + 파이프라인 | `{app="api"} \|= "error"` |
| `LineFilterExpr` | 라인 필터 | `\|= "error"`, `!= "debug"` |
| `LogfmtParserExpr` | Logfmt 파서 | `\| logfmt` |
| `JSONExpressionParserExpr` | JSON 파서 | `\| json` |
| `LabelFilterExpr` | 레이블 필터 | `\| level="error"` |
| `LineFmtExpr` | 라인 포맷 | `\| line_format "{{.msg}}"` |
| `LabelFmtExpr` | 레이블 포맷 | `\| label_format dst="{{.src}}"` |
| `RangeAggregationExpr` | 범위 집계 | `rate({app="api"}[5m])` |
| `VectorAggregationExpr` | 벡터 집계 | `sum by (level) (rate(...))` |

### 9.3 LogQL 쿼리 구조

```
LogQL 쿼리: {app="api"} |= "error" | logfmt | level="error"

AST:
  PipelineExpr
  ├── Left: MatchersExpr
  │     └── Matchers: [{Name:"app", Value:"api", Type:Equal}]
  └── MultiStages:
        ├── LineFilterExpr {Ty: LineMatchEqual, Match: "error"}
        ├── LogfmtParserExpr {}
        └── LabelFilterExpr {Name: "level", Value: "error"}
```

---

## 10. 데이터 흐름 요약

### 10.1 쓰기 경로 데이터 변환

```
클라이언트 JSON/Protobuf
    │
    ▼
PushRequest ([]Stream)
    │
    ▼
Distributor: 유효성 검증 → Stream.Hash 계산 → Ring 조회
    │
    ▼
Ingester.instance: fp → stream 조회/생성
    │
    ▼
stream.Push(): Entry → MemChunk.Append()
    │
    ├── HeadBlock에 비압축 저장
    │
    ├── blockSize 도달 → Block으로 압축
    │
    └── targetSize 도달 → Chunk 플러시
         │
         ├── Object Store에 청크 저장
         └── TSDB Index에 ChunkMeta 기록
```

### 10.2 읽기 경로 데이터 변환

```
LogQL 쿼리 문자열
    │
    ▼
Parser → AST (Expr 트리)
    │
    ▼
Querier: 매처 추출 → 인덱스 조회
    │
    ├── Ingester: InvertedIndex → fp → stream → MemChunk.Iterator()
    │
    └── Store: TSDB Index → ChunkRef → Object Store → MemChunk 디코딩
         │
         ▼
    MergeIterator: 양 소스 병합 + Pipeline 적용
         │
         ▼
    QueryResponse ([]Stream + Stats)
```

---

## 11. 핵심 상수

소스: `pkg/chunkenc/memchunk.go:38-44`, `pkg/ingester/instance.go:56-63`

| 상수 | 값 | 설명 |
|------|---|------|
| `blocksPerChunk` | 10 | 청크당 기본 블록 수 |
| `maxLineLength` | 1GB | 최대 로그 라인 길이 |
| `defaultBlockSize` | 256KB | 기본 블록 크기 |
| `ShardLbName` | `__stream_shard__` | 스트림 샤딩 내부 레이블 |
| `queryBatchSize` | 128 | 쿼리 배치 크기 |
| `queryBatchSampleSize` | 512 | 샘플 쿼리 배치 크기 |

---

## 12. 참고 자료

- Push 타입: `pkg/push/types.go`
- 청크 인코딩: `pkg/chunkenc/memchunk.go`, `pkg/chunkenc/interface.go`
- Ingester 스트림: `pkg/ingester/stream.go`, `pkg/ingester/instance.go`
- 역인덱스: `pkg/ingester/index/index.go`
- 스토리지 청크: `pkg/storage/chunk/chunk.go`
- TSDB 인덱스: `pkg/storage/stores/shipper/indexshipper/tsdb/index/index.go`
- gRPC 프로토: `pkg/logproto/logproto.proto`, `pkg/push/push.proto`
- LogQL AST: `pkg/logql/syntax/ast.go`
