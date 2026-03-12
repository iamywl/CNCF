# Prometheus 핵심 컴포넌트 분석

> 소스 기준: github.com/prometheus/prometheus (main 브랜치)

Prometheus의 6가지 핵심 컴포넌트를 소스코드 수준에서 분석한다.
각 컴포넌트의 구조체 정의, 동작 원리, 설계 의도(Why)를 중심으로 설명한다.

---

## 목차

1. [TSDB (시계열 데이터베이스)](#1-tsdb-시계열-데이터베이스)
2. [PromQL 엔진](#2-promql-엔진)
3. [Scrape Manager](#3-scrape-manager)
4. [Rule Manager](#4-rule-manager)
5. [Notifier](#5-notifier)
6. [Storage Layer](#6-storage-layer)

---

## 1. TSDB (시계열 데이터베이스)

### 1.1 DB 구조체

TSDB의 최상위 진입점은 `tsdb/db.go`의 `DB` 구조체다.

```go
// tsdb/db.go
type DB struct {
    dir    string
    locker *tsdbutil.DirLocker

    logger  *slog.Logger
    metrics *dbMetrics
    opts    *Options
    chunkPool   chunkenc.Pool
    compactor   Compactor
    blocksToDelete BlocksToDeleteFunc

    mtx    sync.RWMutex
    blocks []*Block

    head *Head

    compactc chan struct{}
    donec    chan struct{}
    stopc    chan struct{}

    cmtx sync.Mutex       // compaction + deletion 동시 실행 방지
    autoCompactMtx sync.Mutex
    autoCompact    bool
}
```

**왜 이런 구조인가:**
- `head`와 `blocks`를 분리하여 "최근 데이터는 메모리에, 과거 데이터는 디스크에" 전략 구현
- `compactc` 채널로 컴팩션 트리거를 비동기로 처리
- `cmtx`로 컴팩션과 삭제가 동시에 실행되는 것을 방지

**기본 옵션값** (`tsdb/db.go:DefaultOptions()`):

| 옵션 | 기본값 | 설명 |
|------|--------|------|
| `WALSegmentSize` | 128 MB | WAL 세그먼트 최대 크기 |
| `RetentionDuration` | 15일 | 데이터 보존 기간 |
| `MinBlockDuration` | 2시간 | 최소 블록 기간 (Head 청크 범위) |
| `MaxBlockDuration` | 2시간 | 최대 블록 기간 (컴팩션 후) |
| `StripeSize` | 16384 (1<<14) | stripeSeries 해시맵 엔트리 수 |
| `SamplesPerChunk` | 기본값 | 청크당 목표 샘플 수 |

`Open()` 함수로 DB를 열면 내부적으로 `validateOpts()` → `open()` 순서로 호출된다:

```go
// tsdb/db.go:860
func Open(dir string, l *slog.Logger, r prometheus.Registerer,
    opts *Options, stats *DBStats) (db *DB, err error) {
    var rngs []int64
    opts, rngs = validateOpts(opts, nil)
    return open(dir, l, r, opts, rngs, stats)
}
```

### 1.2 Head 블록: 인메모리 시계열 버퍼

`Head`는 최근 2시간(기본값) 데이터를 메모리에 보관하는 핵심 구조체다.

```go
// tsdb/head.go
type Head struct {
    chunkRange  atomic.Int64    // 청크 시간 범위 (기본 2시간)
    numSeries   atomic.Uint64   // 현재 시계열 수
    minTime     atomic.Int64    // 최소 타임스탬프
    maxTime     atomic.Int64    // 최대 타임스탬프
    lastSeriesID atomic.Uint64  // 마지막 시계열 ID

    wal, wbl    *wlog.WL        // Write-Ahead Log, Write-Behind Log
    series      *stripeSeries   // 해시 분산 시계열 저장소
    postings    *index.MemPostings  // 역색인 (라벨→시계열)
    tombstones  *tombstones.MemTombstones
    iso         *isolation      // 읽기/쓰기 격리
    chunkDiskMapper *chunks.ChunkDiskMapper  // 청크 디스크 매핑
}
```

#### stripeSeries: 해시 분산 잠금으로 동시성 제어

```go
// tsdb/head.go:1978-1994
const DefaultStripeSize = 1 << 14  // 16384

type stripeSeries struct {
    size    int
    series  []map[chunks.HeadSeriesRef]*memSeries  // ref로 샤딩
    hashes  []seriesHashmap                         // 라벨 해시로 샤딩
    locks   []stripeLock                            // 샤드별 잠금
    seriesLifecycleCallback SeriesLifecycleCallback
}

type stripeLock struct {
    sync.RWMutex
    _ [40]byte  // 캐시 라인 충돌 방지 패딩
}
```

**왜 stripeSeries를 사용하는가:**
- 단일 뮤텍스는 수십만 시계열에서 병목 발생
- 16384개 스트라이프로 분산 잠금하면 경합(contention) 대폭 감소
- `_ [40]byte` 패딩으로 서로 다른 잠금이 같은 CPU 캐시 라인에 위치하는 것을 방지 (false sharing 문제 해결)

```
┌─────────────────────────────────────────────┐
│                stripeSeries                  │
│                                              │
│  series[0] ──→ map[ref]*memSeries  ◄── lock[0]  │
│  series[1] ──→ map[ref]*memSeries  ◄── lock[1]  │
│  series[2] ──→ map[ref]*memSeries  ◄── lock[2]  │
│     ...           ...                  ...    │
│  series[16383] → map[ref]*memSeries ◄── lock[16383] │
│                                              │
│  hashes[0] ──→ seriesHashmap                 │
│  hashes[1] ──→ seriesHashmap                 │
│     ...                                      │
└─────────────────────────────────────────────┘
```

#### memSeries: 개별 시계열 표현

```go
// tsdb/head.go:2386
type memSeries struct {
    ref       chunks.HeadSeriesRef  // 시계열 고유 참조 (불변)
    meta      *metadata.Metadata     // 메타데이터 (불변)
    shardHash uint64                 // 샤딩용 해시 (불변)

    sync.Mutex                       // 아래 필드 보호

    lset labels.Labels               // 라벨 셋
    // 디스크에 mmap된 불변 청크들 (시간 순서)
    // 컴팩션 시: mmappedChunks=[p5,p6,p7,p8,p9] firstChunkID=5
    //   컴팩션 후: mmappedChunks=[p7,p8,p9]       firstChunkID=7
}
```

#### MemPostings: 역색인 구조

```go
// tsdb/index/postings.go:60
type MemPostings struct {
    mtx sync.RWMutex
    // 라벨 이름 → 라벨 값 → 시계열 참조 리스트
    m map[string]map[string][]storage.SeriesRef
}
```

**역색인 동작 원리:**

```
쿼리: {job="api", method="GET"}

MemPostings:
  "job" → "api"      → [ref1, ref3, ref5, ref7]
  "method" → "GET"   → [ref1, ref2, ref5, ref8]

교집합(Intersect) → [ref1, ref5]
```

### 1.3 WAL (Write-Ahead Log)

WAL은 데이터 내구성을 보장하는 로그 구조다 (`tsdb/wlog/wlog.go`).

```go
// tsdb/wlog/wlog.go
const (
    DefaultSegmentSize = 128 * 1024 * 1024  // 128 MB
    pageSize           = 32 * 1024           // 32 KB
    recordHeaderSize   = 7
)

var castagnoliTable = crc32.MakeTable(crc32.Castagnoli)
```

**WAL 레코드 형식:**

```
┌──────────────────────────────────────────┐
│             WAL 세그먼트 (128MB)           │
├──────────────────────────────────────────┤
│ ┌────────────────────────────────┐       │
│ │         페이지 (32KB)            │       │
│ │ ┌────────────────────────────┐ │       │
│ │ │ 레코드 헤더 (7 bytes)        │ │       │
│ │ │  - 타입 (1 byte)            │ │       │
│ │ │  - 길이 (2 bytes)           │ │       │
│ │ │  - CRC32 (4 bytes)         │ │       │
│ │ ├────────────────────────────┤ │       │
│ │ │ 레코드 데이터               │ │       │
│ │ │  (Snappy 압축 가능)         │ │       │
│ │ └────────────────────────────┘ │       │
│ └────────────────────────────────┘       │
│ ┌────────────────────────────────┐       │
│ │         페이지 (32KB)            │       │
│ │           ...                   │       │
│ └────────────────────────────────┘       │
└──────────────────────────────────────────┘
```

**CRC32 Castagnoli를 사용하는 이유:**
- 하드웨어 가속(SSE 4.2) 지원으로 성능 우수
- 비트 에러 검출률이 기존 CRC32보다 높음

### 1.4 Block: 불변 디스크 블록

Head 블록에서 컴팩션되어 디스크에 저장된 불변 데이터 단위.

```
data/
├── 01BKGV7JBM69T2G1BGBGM6KB12/   ← ULID 식별자
│   ├── chunks/                      ← 청크 데이터
│   │   └── 000001                   ← 청크 세그먼트 파일
│   ├── index                        ← 색인 (시계열 라벨 → 청크 참조)
│   ├── tombstones                   ← 삭제 마커
│   └── meta.json                    ← 블록 메타데이터
├── 01BKGTZQ1SYQJTR4PB43C8PD98/
│   ├── chunks/
│   ├── index
│   ├── tombstones
│   └── meta.json
├── wal/                              ← WAL 디렉토리
│   ├── 00000000                     ← 세그먼트 파일
│   ├── 00000001
│   └── 00000002
└── chunks_head/                      ← Head 청크 mmap 파일
```

**ULID를 블록 식별자로 쓰는 이유:**
- UUID와 달리 시간순 정렬 가능 (앞 48비트가 타임스탬프)
- 랜덤 부분으로 충돌 방지
- 파일시스템에서 자연스러운 시간순 정렬

### 1.5 Compaction: LeveledCompactor

```go
// tsdb/compact.go
type Compactor interface {
    Plan(dir string) ([]string, error)
    Write(dest string, b BlockReader, mint, maxt int64, base *BlockMeta) ([]ulid.ULID, error)
    Compact(dest string, dirs []string, open []*Block) ([]ulid.ULID, error)
}

type LeveledCompactor struct {
    metrics              *CompactorMetrics
    logger               *slog.Logger
    ranges               []int64          // 레벨별 시간 범위
    chunkPool            chunkenc.Pool
    mergeFunc            storage.VerticalChunkSeriesMergeFunc
    enableOverlappingCompaction bool
}
```

**컴팩션 흐름:**

```
  Head (2h)
    │
    ▼ 매 2시간
  Block [2h]  Block [2h]  Block [2h]  Block [2h]
    │              │           │           │
    └──────┬───────┘           │           │
           ▼                   │           │
      Block [4h]               │           │
           │                   │           │
           └───────┬───────────┘           │
                   ▼                       │
              Block [8h]                   │
                   │                       │
                   └───────────┬───────────┘
                               ▼
                          Block [16h]

ExponentialBlockRanges(minSize=2h, steps, stepSize=3)
→ [2h, 6h, 18h, 54h, ...]
```

**컴팩션이 필요한 이유:**
- 작은 블록이 많으면 쿼리 시 IO 오버헤드 증가
- 큰 블록으로 합치면 인덱스 조회 효율 향상
- 삭제된 시계열(tombstones)을 물리적으로 제거

---

## 2. PromQL 엔진

### 2.1 Engine 구조체

```go
// promql/engine.go:345
type Engine struct {
    logger                   *slog.Logger
    metrics                  *engineMetrics
    timeout                  time.Duration
    maxSamplesPerQuery       int
    activeQueryTracker       QueryTracker
    queryLogger              QueryLogger
    lookbackDelta            time.Duration  // 기본 5분
    noStepSubqueryIntervalFn func(rangeMillis int64) int64
    enableAtModifier         bool
    enableNegativeOffset     bool
    enablePerStepStats       bool
    enableDelayedNameRemoval bool
    parser                   parser.Parser
}
```

**EngineOpts 주요 설정:**

| 옵션 | 기본값 | 설명 |
|------|--------|------|
| `MaxSamples` | 설정 필요 | 쿼리당 최대 샘플 수 (메모리 보호) |
| `Timeout` | 설정 필요 | 쿼리 타임아웃 |
| `LookbackDelta` | 5분 | 시계열이 stale 판정되는 마지막 샘플 이후 시간 |
| `ActiveQueryTracker` | nil | 동시 쿼리 추적 및 제한 |

### 2.2 쿼리 인터페이스와 실행

```go
// promql/engine.go
type QueryEngine interface {
    NewInstantQuery(ctx context.Context, q storage.Queryable,
        opts QueryOpts, qs string, ts time.Time) (Query, error)
    NewRangeQuery(ctx context.Context, q storage.Queryable,
        opts QueryOpts, qs string, start, end time.Time,
        interval time.Duration) (Query, error)
}

type Query interface {
    Exec(ctx context.Context) *Result
    Close()
    Statement() parser.Statement
    Stats() *stats.Statistics
    Cancel()
    String() string
}
```

### 2.3 쿼리 실행 파이프라인

```
사용자 쿼리 문자열
    │
    ▼
┌──────────────┐
│  파싱 (Parse) │  parser/parse.go → AST 생성
│              │  VectorSelector, MatrixSelector,
│              │  BinaryExpr, AggregateExpr, Call
└──────┬───────┘
       │
       ▼
┌──────────────┐
│ 준비 (Prepare)│  populateSeries() → 시계열 데이터 로드
│              │  querier.Select() 호출
└──────┬───────┘
       │
       ▼
┌──────────────┐
│  평가 (Eval)  │  evaluator.Eval() → 재귀적 AST 순회
│              │  각 노드 타입별 평가 함수 호출
└──────┬───────┘
       │
       ▼
┌──────────────┐
│ 정렬 (Sort)   │  결과 시계열 정렬 (선택적)
└──────┬───────┘
       │
       ▼
    Result
```

`exec()` 함수의 핵심 흐름 (`promql/engine.go:673`):

```go
func (ng *Engine) exec(ctx context.Context, q *query) (v parser.Value, ws annotations.Annotations, err error) {
    ng.metrics.currentQueries.Inc()
    defer func() {
        ng.metrics.currentQueries.Dec()
        ng.metrics.querySamples.Add(float64(q.sampleStats.TotalSamples))
    }()

    ctx, cancel := context.WithTimeout(ctx, ng.timeout)
    q.cancel = cancel
    // ... 로깅, 트레이싱 ...

    switch s := q.Statement().(type) {
    case *parser.EvalStmt:
        return ng.execEvalStmt(ctx, q, s)
    case parser.TestStmt:
        return nil, nil, s(ctx)
    }
}
```

`execEvalStmt()`의 즉시 쿼리 vs 범위 쿼리 분기 (`promql/engine.go:773`):

```go
func (ng *Engine) execEvalStmt(ctx context.Context, query *query, s *parser.EvalStmt) (...) {
    // 1. 시간 범위 계산
    mint, maxt := FindMinMaxTime(s)
    querier, err := query.queryable.Querier(mint, maxt)

    // 2. 시계열 데이터 선 로드
    ng.populateSeries(ctxPrepare, querier, s)

    // 3. 즉시 쿼리 (Start == End, Interval == 0)
    if s.Start.Equal(s.End) && s.Interval == 0 {
        evaluator := &evaluator{
            startTimestamp: start,
            endTimestamp:   start,
            interval:       1,
            maxSamples:     ng.maxSamplesPerQuery,
            lookbackDelta:  s.LookbackDelta,
            // ...
        }
        val, warnings, err := evaluator.Eval(s.Expr)
        // Vector 또는 Scalar 반환
    }

    // 4. 범위 쿼리: 여러 스텝에 걸쳐 평가 → Matrix 반환
    evaluator := &evaluator{
        startTimestamp: timeMilliseconds(s.Start),
        endTimestamp:   timeMilliseconds(s.End),
        interval:       durationMilliseconds(s.Interval),
        maxSamples:     ng.maxSamplesPerQuery,
        // ...
    }
    val, warnings, err := evaluator.Eval(s.Expr)
    // Matrix 반환
}
```

### 2.4 동시성 제어: QueryTracker

```go
// promql/engine.go:277
type QueryTracker interface {
    io.Closer
    GetMaxConcurrent() int
    Insert(ctx context.Context, query string) (int, error)
    Delete(insertIndex int)
}
```

**QueryTracker의 역할:**
1. 동시 실행 쿼리 수 제한 (`GetMaxConcurrent()`)
2. 크래시 시 실행 중이던 쿼리 추적 (재시작 시 로깅)
3. `Insert()`가 최대 동시성에 도달하면 블로킹

### 2.5 에러 타입

```go
type ErrQueryTimeout string    // 쿼리 타임아웃
type ErrQueryCanceled string   // 쿼리 취소
type ErrTooManySamples string  // maxSamples 초과
type ErrStorage struct{ Err error }  // 스토리지 에러
```

### 2.6 즉시 쿼리 vs 범위 쿼리 비교

| 항목 | 즉시 쿼리 (Instant) | 범위 쿼리 (Range) |
|------|---------------------|-------------------|
| 시간 조건 | `Start == End`, `Interval == 0` | `Start < End`, `Interval > 0` |
| 반환 타입 | `Vector` 또는 `Scalar` | `Matrix` |
| 평가 횟수 | 1회 (단일 시점) | `(End-Start)/Interval` 회 |
| 용도 | 현재 상태 조회, 알림 평가 | 그래프 그리기, 추세 분석 |
| API 엔드포인트 | `/api/v1/query` | `/api/v1/query_range` |

---

## 3. Scrape Manager

### 3.1 Manager 구조체

```go
// scrape/manager.go
type Manager struct {
    appendable     storage.Appendable
    appendableV2   storage.AppendableV2
    opts           *Options
    logger         *slog.Logger
    scrapeConfigs  map[string]*config.ScrapeConfig
    scrapePools    map[string]*scrapePool   // Job별 스크래핑 풀
    graceShut      chan struct{}
    triggerReload  chan struct{}
    metrics        *scrapeMetrics
    buffers        pool.Pool   // 버퍼 풀 (1KB ~ 100MB, 3배 증가)
}
```

**NewManager에서 `appendable`과 `appendableV2` 중 하나만 사용:**
```go
func NewManager(..., appendable storage.Appendable,
    appendableV2 storage.AppendableV2, ...) (*Manager, error) {
    if appendable != nil && appendableV2 != nil {
        return nil, errors.New("appendable and appendableV2 cannot be provided at the same time")
    }
}
```

### 3.2 scrapePool: Job별 스크래핑 풀

```go
// scrape/scrape.go:84
type scrapePool struct {
    appendable   storage.Appendable
    appendableV2 storage.AppendableV2
    logger       *slog.Logger
    ctx          context.Context
    cancel       context.CancelFunc
    options      *Options

    mtx    sync.Mutex
    config *config.ScrapeConfig
    client *http.Client
    loops  map[uint64]loop          // 타겟별 scrapeLoop
    symbolTable *labels.SymbolTable  // 심볼 테이블 (메모리 최적화)
}
```

**계층 구조:**

```
Manager
  ├── scrapePool("job_a")  ← job_name별 하나
  │     ├── scrapeLoop(target_1)  ← 타겟별 하나
  │     ├── scrapeLoop(target_2)
  │     └── scrapeLoop(target_3)
  ├── scrapePool("job_b")
  │     ├── scrapeLoop(target_4)
  │     └── scrapeLoop(target_5)
  └── scrapePool("job_c")
        └── scrapeLoop(target_6)
```

### 3.3 scrapeLoop: 타겟별 스크래핑 루프

`scrapeLoop.run()` 함수 (`scrape/scrape.go:1234`):

```go
func (sl *scrapeLoop) run(errc chan<- error) {
    // 1. offset 분산: 모든 타겟이 동시에 스크래핑하지 않도록
    if !sl.skipOffsetting {
        select {
        case <-time.After(sl.scraper.offset(sl.interval, sl.offsetSeed)):
        case <-sl.ctx.Done():
            close(sl.stopped)
            return
        }
    }

    // 2. 정밀한 타이밍으로 주기적 스크래핑
    alignedScrapeTime := time.Now().Round(0)
    ticker := time.NewTicker(sl.interval)
    defer ticker.Stop()

    for {
        select {
        case <-sl.ctx.Done():
            break mainLoop
        default:
        }
        // scrape 실행 ...
    }
}
```

**스크래핑 타임스탬프 정렬:**

```go
// scrape/scrape.go:64-70
var ScrapeTimestampTolerance = 2 * time.Millisecond
var AlignScrapeTimestamps = true
```

**왜 타임스탬프를 정렬하는가:**
- TSDB 압축 효율 향상: 정렬된 타임스탬프는 delta-of-delta 인코딩에서 더 작은 값 생성
- 2ms 이내 차이는 동일 시점으로 취급

### 3.4 메트릭 파싱

Prometheus는 3가지 형식의 메트릭을 파싱할 수 있다:

| 형식 | Content-Type | 패키지 |
|------|-------------|--------|
| Prometheus text | `text/plain` | `model/textparse` |
| OpenMetrics | `application/openmetrics-text` | `model/textparse` |
| Protobuf | `application/vnd.google.protobuf` | `model/textparse` |

### 3.5 Stale Marker

타겟이 사라지거나 시계열이 더 이상 보고되지 않을 때, Prometheus는 특수한 NaN 값(stale marker)을 기록한다.

```go
// model/value/value.go
var StaleNaN = math.Float64frombits(value.StaleNaN)
```

**Stale marker의 역할:**
- 시계열이 더 이상 활성 상태가 아님을 TSDB에 알림
- PromQL의 `lookbackDelta` (기본 5분)와 함께 작동
- stale marker 이후에는 해당 시계열이 쿼리 결과에서 제외

### 3.6 라벨 처리

스크래핑된 메트릭은 여러 단계의 라벨 변환을 거친다:

```
원본 메트릭 라벨
    │
    ▼
┌───────────────────────┐
│ PopulateLabels()       │  타겟 라벨 + 메트릭 라벨 병합
│ - instance 라벨 추가    │  __address__ → instance
│ - job 라벨 추가         │  job_name → job
└───────┬───────────────┘
        │
        ▼
┌───────────────────────┐
│ mutateSampleLabels()   │  metric_relabel_configs 적용
│ - 라벨 재작성           │
│ - 라벨 삭제             │
│ - 라벨 값 변경          │
└───────┬───────────────┘
        │
        ▼
    최종 라벨 (TSDB 저장)
```

---

## 4. Rule Manager

### 4.1 Manager 구조체

```go
// rules/manager.go:97
type Manager struct {
    opts    *ManagerOptions
    groups  map[string]*Group   // 그룹 키 → Group
    mtx     sync.RWMutex
    block   chan struct{}
    done    chan struct{}
    restored bool
    logger  *slog.Logger
}

// rules/manager.go:112
type ManagerOptions struct {
    ExternalURL       *url.URL
    QueryFunc         QueryFunc           // PromQL 실행 함수
    NotifyFunc        NotifyFunc          // 알림 발송 함수
    Context           context.Context
    Appendable        storage.Appendable  // 결과 저장소
    Queryable         storage.Queryable   // 데이터 조회
    DefaultEvaluationInterval time.Duration
    // ...
}
```

### 4.2 Group: 룰 그룹

```go
// rules/group.go:45
type Group struct {
    name     string
    file     string
    interval time.Duration       // 평가 주기
    rules    []Rule              // 그룹 내 룰들
    seriesInPreviousEval []map[string]labels.Labels  // 이전 평가의 시계열
    staleSeries []labels.Labels  // stale 처리할 시계열
    opts     *ManagerOptions

    evaluationTime    time.Duration  // 평가 소요 시간
    lastEvaluation    time.Time       // 마지막 평가 시각
    lastEvalTimestamp time.Time       // 마지막 평가 타임슬롯

    shouldRestore bool
    markStale     bool
    done          chan struct{}
    terminated    chan struct{}
}
```

### 4.3 Group 평가 루프

```go
// rules/group.go:208
func (g *Group) run(ctx context.Context) {
    defer close(g.terminated)

    // 1. 일관된 타임슬롯 대기
    evalTimestamp := g.EvalTimestamp(time.Now().UnixNano()).Add(g.interval)
    select {
    case <-time.After(time.Until(evalTimestamp)):
    case <-g.done:
        return
    }

    // 2. interval 주기로 평가 반복
    tick := time.NewTicker(g.interval)
    defer tick.Stop()

    for {
        select {
        case <-g.done:
            return
        default:
        }
        // g.Eval(ctx, evalTimestamp) 호출
    }
}
```

**DefaultEvalIterationFunc** (`rules/manager.go:81`):

```go
func DefaultEvalIterationFunc(ctx context.Context, g *Group, evalTimestamp time.Time) {
    g.metrics.IterationsScheduled.WithLabelValues(GroupKey(g.file, g.name)).Inc()
    start := time.Now()
    g.Eval(ctx, evalTimestamp)
    timeSinceStart := time.Since(start)

    g.metrics.IterationDuration.Observe(timeSinceStart.Seconds())
    g.setEvaluationTime(timeSinceStart)
    g.setLastEvaluation(start)
    g.setLastEvalTimestamp(evalTimestamp)
}
```

### 4.4 RecordingRule: 쿼리 결과를 새 메트릭으로 저장

```go
// rules/recording.go:38
type RecordingRule struct {
    name   string
    vector parser.Expr    // PromQL 표현식
    labels labels.Labels  // 추가 라벨
    health *atomic.String
    evaluationTimestamp *atomic.Time
    lastError           *atomic.Error
    evaluationDuration  *atomic.Duration

    dependenciesMutex sync.RWMutex
    dependentRules    []Rule   // 이 룰에 의존하는 룰들
    dependencyRules   []Rule   // 이 룰이 의존하는 룰들
}
```

**RecordingRule 동작:**

```
PromQL: sum(rate(http_requests_total[5m])) by (job)
    │
    ▼
┌──────────────────────┐
│ QueryFunc 실행         │
│ → Vector 결과 반환     │
└──────┬───────────────┘
       │
       ▼
┌──────────────────────┐
│ 결과를 새 시계열로 저장  │
│ job:http_requests:rate5m │
│ Appender.Append()    │
└──────────────────────┘
```

### 4.5 AlertingRule: 상태 머신

```go
// rules/alerting.go:54-67
type AlertState int

const (
    StateUnknown  AlertState = iota
    StateInactive                     // 조건 미충족
    StatePending                      // for 대기 중
    StateFiring                       // 발화 상태
)
```

**Alert 구조체:**

```go
// rules/alerting.go:84
type Alert struct {
    State       AlertState
    Labels      labels.Labels
    Annotations labels.Labels
    Value       float64         // 마지막 평가 값
    ActiveAt    time.Time       // 활성화 시작 시간
    FiredAt     time.Time       // 발화 시작 시간
    ResolvedAt  time.Time       // 해소 시간
    LastSentAt  time.Time       // 마지막 전송 시간
    ValidUntil  time.Time       // 유효 기간
    KeepFiringSince time.Time   // 지속 발화 시작 시간
}
```

**상태 전이 다이어그램:**

```
                  조건 미충족
              ┌──────────────┐
              │              │
              ▼              │
    ┌──────────────┐    ┌────┴─────┐
    │  Inactive    │───→│ Pending  │
    │ (조건 미충족)  │    │ (for 대기)│
    └──────────────┘    └────┬─────┘
          ▲                  │ for 경과
          │                  ▼
          │            ┌──────────┐
          └────────────│  Firing  │
            조건 미충족   │ (발화 중) │
                       └──────────┘
```

**needsSending 로직:**

```go
// rules/alerting.go:102
func (a *Alert) needsSending(ts time.Time, resendDelay time.Duration) bool {
    if a.State == StatePending {
        return false  // Pending 상태에서는 전송하지 않음
    }
    // 마지막 전송 후 해소되었으면 재전송
    if a.ResolvedAt.After(a.LastSentAt) {
        return true
    }
    // resendDelay 경과 후 재전송
    return a.LastSentAt.Add(resendDelay).Before(ts)
}
```

### 4.6 RecordingRule vs AlertingRule 비교

| 항목 | RecordingRule | AlertingRule |
|------|-------------|-------------|
| 목적 | 쿼리 결과를 새 시계열로 저장 | 조건 기반 알림 생성 |
| 출력 | 새 메트릭 (storage에 Append) | Alert 객체 (Notifier로 전달) |
| 상태 | 없음 (매 평가 독립적) | Inactive → Pending → Firing |
| `for` 절 | 없음 | 있음 (발화 전 대기 시간) |
| 의존성 | `dependentRules`, `dependencyRules` | 동일 |
| 메트릭 | 지정한 이름의 시계열 | `ALERTS`, `ALERTS_FOR_STATE` |

---

## 5. Notifier

### 5.1 Manager 구조체

```go
// notifier/manager.go:53
type Manager struct {
    opts          *Options
    metrics       *alertMetrics
    mtx           sync.RWMutex
    stopOnce      *sync.Once
    stopRequested chan struct{}
    alertmanagers map[string]*alertmanagerSet  // AM 그룹별 관리
    logger        *slog.Logger
}

// notifier/manager.go:68
type Options struct {
    QueueCapacity   int              // 알림 큐 용량
    DrainOnShutdown bool             // 종료 시 큐 비우기
    ExternalLabels  labels.Labels    // 외부 라벨
    RelabelConfigs  []*relabel.Config
    Do func(ctx context.Context, client *http.Client,
        req *http.Request) (*http.Response, error)
    MaxBatchSize    int              // 배치당 최대 알림 수 (기본 256)
    Registerer      prometheus.Registerer
}
```

### 5.2 sendLoop: Alertmanager별 전송 루프

```go
// notifier/sendloop.go:30
type sendLoop struct {
    alertmanagerURL string
    cfg     *config.AlertmanagerConfig
    client  *http.Client
    opts    *Options
    metrics *alertMetrics

    mtx      sync.RWMutex
    queue    []*Alert        // 알림 큐
    hasWork  chan struct{}   // 작업 신호
    stopped  chan struct{}
    stopOnce sync.Once
    logger   *slog.Logger
}
```

**큐 관리 로직** (`notifier/sendloop.go:75`):

```go
func (s *sendLoop) add(alerts ...*Alert) {
    // 큐 용량보다 큰 배치 → 오래된 알림 드롭
    if d := len(alerts) - s.opts.QueueCapacity; d > 0 {
        s.logger.Warn("Alert batch larger than queue capacity, dropping alerts", "count", d)
        alerts = alerts[d:]
    }
    // 큐가 가득 참 → 오래된 알림 제거
    if d := (len(s.queue) + len(alerts)) - s.opts.QueueCapacity; d > 0 {
        s.logger.Warn("Alert notification queue full, dropping alerts", "count", d)
        s.queue = s.queue[d:]
    }
    s.queue = append(s.queue, alerts...)
    s.notifyWork()  // 비동기 전송 트리거
}
```

**왜 오래된 알림을 먼저 드롭하는가:**
- 최신 알림이 더 중요 (현재 상태 반영)
- Alertmanager는 같은 알림의 최신 상태만 필요

### 5.3 Alert 구조체

```go
// notifier/alert.go:25
type Alert struct {
    Labels       labels.Labels `json:"labels"`
    Annotations  labels.Labels `json:"annotations"`
    StartsAt     time.Time     `json:"startsAt,omitempty"`
    EndsAt       time.Time     `json:"endsAt,omitempty"`
    GeneratorURL string        `json:"generatorURL,omitempty"`
}
```

**Resolved 판정:**

```go
func (a *Alert) Resolved() bool {
    return a.ResolvedAt(time.Now())
}

func (a *Alert) ResolvedAt(ts time.Time) bool {
    if a.EndsAt.IsZero() {
        return false
    }
    return !a.EndsAt.After(ts)
}
```

### 5.4 알림 전송 흐름

```
AlertingRule.Eval()
    │
    ▼ NotifyFunc 호출
┌──────────────────┐
│ Manager          │
│  - relabelAlerts │  ExternalLabels 추가 + relabel_configs 적용
│  - 각 AM에 분배   │
└──────┬───────────┘
       │
       ▼
┌──────────────────┐     ┌──────────────────┐
│ sendLoop (AM-1)  │     │ sendLoop (AM-2)  │
│ queue: [....]    │     │ queue: [....]    │
│ batch: MaxBatch  │     │ batch: MaxBatch  │
│  = 256           │     │  = 256           │
└──────┬───────────┘     └──────┬───────────┘
       │                        │
       ▼                        ▼
  POST /api/v1/alerts      POST /api/v1/alerts
  Content-Type: JSON       Content-Type: JSON
  → Alertmanager-1         → Alertmanager-2
```

### 5.5 알림 발송 조건 비교

| 조건 | 발송 여부 | 설명 |
|------|---------|------|
| `State == Pending` | X | for 대기 중에는 미발송 |
| `ResolvedAt > LastSentAt` | O | 해소 후 반드시 재전송 |
| `LastSentAt + resendDelay < now` | O | 재전송 주기 도달 |
| 큐 용량 초과 | 최신 우선 | 오래된 알림 드롭 |

---

## 6. Storage Layer

### 6.1 핵심 인터페이스

```go
// storage/interface.go
type Storage interface {
    SampleAndChunkQueryable   // Queryable + ChunkQueryable
    Appendable                // Appender(ctx) Appender
    AppendableV2              // AppenderV2(opts) AppenderV2 (마이그레이션 중)
    StartTime() (int64, error)
    Close() error
}

type Queryable interface {
    Querier(mint, maxt int64) (Querier, error)
}

type Querier interface {
    LabelQuerier
    Select(ctx context.Context, sortSeries bool,
        hints *SelectHints, matchers ...*labels.Matcher) SeriesSet
}

type Appendable interface {
    Appender(ctx context.Context) Appender
}
```

**인터페이스 계층 구조:**

```
Storage
  ├── SampleAndChunkQueryable
  │     ├── Queryable
  │     │     └── Querier
  │     │           ├── LabelQuerier
  │     │           └── Select()
  │     └── ChunkQueryable
  │           └── ChunkQuerier
  ├── Appendable          ← (Deprecated, Q2 2026 제거 예정)
  │     └── Appender
  │           ├── Append()
  │           ├── AppendExemplar()
  │           ├── AppendHistogram()
  │           ├── Commit()
  │           └── Rollback()
  └── AppendableV2        ← (새로운 방식)
        └── AppenderV2
```

### 6.2 FanoutStorage: 다중 스토리지 프록시

```go
// storage/fanout.go:29
type fanout struct {
    logger      *slog.Logger
    primary     Storage       // 주 스토리지 (TSDB)
    secondaries []Storage     // 보조 스토리지 (Remote Storage 등)
}
```

**FanoutStorage 동작 원칙:**

```
                 ┌───────────────────┐
    Write ──────→│  FanoutStorage    │
                 │                   │
    Read ───────→│  primary (TSDB)   │ ← 에러 시 전체 실패
                 │  secondary-1      │ ← 에러 시 결과 버리고 경고
                 │  secondary-2      │ ← 에러 시 결과 버리고 경고
                 └───────────────────┘
```

```go
// storage/fanout.go:45
func NewFanout(logger *slog.Logger, primary Storage, secondaries ...Storage) Storage {
    return &fanout{
        logger:      logger,
        primary:     primary,
        secondaries: secondaries,
    }
}
```

**읽기 경로에서 primary vs secondary 차이:**

| 항목 | Primary | Secondary |
|------|---------|-----------|
| 에러 처리 | 전체 쿼리 실패 | 해당 결과 무시 + 경고 반환 |
| 역할 | TSDB (로컬 데이터) | Remote Storage (원격 데이터) |
| 결과 병합 | 기본 결과 | MergeQuerier로 병합 |

```go
// storage/fanout.go:74
func (f *fanout) Querier(mint, maxt int64) (Querier, error) {
    primary, err := f.primary.Querier(mint, maxt)
    if err != nil {
        return nil, err  // primary 에러 → 전체 실패
    }

    secondaries := make([]Querier, 0, len(f.secondaries))
    for _, storage := range f.secondaries {
        querier, err := storage.Querier(mint, maxt)
        if err != nil {
            // secondary 에러 → 정리 후 실패
            errs := []error{err, primary.Close()}
            // ...
            return nil, errors.Join(errs...)
        }
        secondaries = append(secondaries, querier)
    }
    return NewMergeQuerier([]Querier{primary}, secondaries, ChainedSeriesMerge), nil
}
```

### 6.3 Remote Storage

```go
// storage/remote/storage.go:54
type Storage struct {
    deduper *logging.Deduper
    logger  *slog.Logger
    mtx     sync.Mutex

    rws *WriteStorage                         // 원격 쓰기
    queryables []storage.SampleAndChunkQueryable  // 원격 읽기
    localStartTimeCallback startTimeCallback
}
```

**Remote Storage 아키텍처:**

```
┌────────────────────────────────────────────────┐
│                Prometheus                       │
│                                                 │
│  ┌──────────┐    ┌──────────────────────────┐  │
│  │   TSDB   │    │    Remote Storage         │  │
│  │ (primary)│    │  ┌────────────────────┐   │  │
│  │          │    │  │ WriteStorage (rws)  │   │  │
│  │          │    │  │  - QueueManager     │   │  │
│  │          │    │  │  - WAL 기반 신뢰성    │   │  │
│  │          │    │  └────────┬───────────┘   │  │
│  │          │    │           │               │  │
│  │          │    │  ┌────────┴───────────┐   │  │
│  │          │    │  │ Read Queryables     │   │  │
│  │          │    │  │  - Remote Read API  │   │  │
│  │          │    │  └────────────────────┘   │  │
│  └──────────┘    └──────────────────────────┘  │
│        │                      │                 │
│        └──────────┬───────────┘                 │
│                   │                              │
│            FanoutStorage                        │
└────────────────────────────────────────────────┘
         │                      │
         ▼                      ▼
   로컬 디스크             원격 저장소
                        (Cortex, Thanos, etc.)
```

### 6.4 Appendable의 진화: V1 → V2 마이그레이션

현재 Prometheus는 `Appendable` (V1) → `AppendableV2` (V2)로 마이그레이션 중이다.

```go
// V1 (Deprecated, Q2 2026 제거 예정)
type Appendable interface {
    Appender(ctx context.Context) Appender
}

// V2 (새로운 방식)
// - Start Timestamp (ST) 지원
// - 메타데이터 항상 전달
// - Exemplar per sample
```

**왜 V2로 전환하는가:**
- V1은 ST(Start Timestamp), 메타데이터, Exemplar를 별도 메서드로 처리해야 함
- V2는 이를 통합하여 구현을 단순화하고 기능을 확장
- `scrape.Manager`는 이미 `appendableV2` 옵션 지원

---

## 컴포넌트 간 상호작용 종합

```
┌─────────────┐         ┌─────────────┐
│  Discovery   │────────→│   Scrape    │
│ (SD 결과)    │ 타겟 목록 │  Manager   │
└─────────────┘         └──────┬──────┘
                               │ 샘플 저장
                               ▼
┌─────────────┐         ┌─────────────┐         ┌─────────────┐
│   PromQL    │◄────────│    TSDB     │────────→│   Remote    │
│   Engine    │ 쿼리/결과 │  (Storage) │ 원격 쓰기  │  Storage   │
└──────┬──────┘         └─────────────┘         └─────────────┘
       │ 쿼리 실행
       ▼
┌─────────────┐         ┌─────────────┐
│    Rule     │────────→│  Notifier   │
│   Manager   │ 알림 전달 │  (Manager) │
└─────────────┘         └──────┬──────┘
                               │ POST /api/v1/alerts
                               ▼
                        ┌─────────────┐
                        │Alertmanager │
                        └─────────────┘
```

### 데이터 흐름 요약

| 단계 | 컴포넌트 | 입력 | 출력 |
|------|---------|------|------|
| 1 | Discovery | 설정 파일 | 타겟 목록 |
| 2 | Scrape Manager | 타겟 URL | HTTP 응답 (메트릭 텍스트) |
| 3 | textparse | 메트릭 텍스트 | 파싱된 샘플 |
| 4 | TSDB (Head) | 샘플 | 인메모리 시계열 + WAL |
| 5 | Compactor | Head 블록 | 디스크 블록 |
| 6 | PromQL Engine | 쿼리 문자열 | Vector/Matrix/Scalar |
| 7 | Rule Manager | PromQL 결과 | RecordingRule→샘플, AlertingRule→Alert |
| 8 | Notifier | Alert 객체 | HTTP POST (JSON) → Alertmanager |

---

## 참고 파일 경로

| 컴포넌트 | 주요 파일 |
|---------|----------|
| TSDB DB | `tsdb/db.go` |
| Head | `tsdb/head.go`, `tsdb/head_append.go` |
| WAL | `tsdb/wlog/wlog.go` |
| Compaction | `tsdb/compact.go` |
| PromQL Engine | `promql/engine.go` |
| Parser | `promql/parser/parse.go` |
| Scrape Manager | `scrape/manager.go` |
| Scrape Loop | `scrape/scrape.go` |
| Rule Manager | `rules/manager.go`, `rules/group.go` |
| AlertingRule | `rules/alerting.go` |
| RecordingRule | `rules/recording.go` |
| Notifier | `notifier/manager.go`, `notifier/sendloop.go`, `notifier/alert.go` |
| Storage Interface | `storage/interface.go` |
| FanoutStorage | `storage/fanout.go` |
| Remote Storage | `storage/remote/storage.go` |
