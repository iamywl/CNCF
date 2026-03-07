# 10. 스토리지 레이어 Deep-Dive

## 목차

1. [개요](#1-개요)
2. [Store 인터페이스 계층](#2-store-인터페이스-계층)
3. [LokiStore: 최상위 스토어](#3-lokistore-최상위-스토어)
4. [CompositeStore: 멀티 피리어드 라우팅](#4-compositestore-멀티-피리어드-라우팅)
5. [스키마 설정 (SchemaConfig)](#5-스키마-설정-schemaconfig)
6. [청크 인코딩 (MemChunk)](#6-청크-인코딩-memchunk)
7. [압축 코덱](#7-압축-코덱)
8. [TSDB 인덱스](#8-tsdb-인덱스)
9. [인덱스 시퍼 (IndexShipper)](#9-인덱스-시퍼-indexshipper)
10. [오브젝트 스토어 클라이언트](#10-오브젝트-스토어-클라이언트)
11. [캐시 계층](#11-캐시-계층)
12. [Chunk Fetcher](#12-chunk-fetcher)
13. [읽기 경로 전체 흐름](#13-읽기-경로-전체-흐름)
14. [쓰기 경로와 스토리지](#14-쓰기-경로와-스토리지)
15. [운영 관점 핵심 포인트](#15-운영-관점-핵심-포인트)

---

## 1. 개요

Loki의 스토리지 레이어는 로그 데이터의 영구 저장과 조회를 담당하는 핵심 서브시스템이다. Prometheus의 TSDB에서 영감을 받았지만 로그 데이터의 특수성(대용량, 비정형 텍스트, 시계열+텍스트 이중 특성)에 맞게 완전히 재설계되었다.

### 핵심 설계 원칙

| 원칙 | 설명 |
|------|------|
| **인덱스/데이터 분리** | 인덱스(메타데이터)와 청크(로그 데이터)를 독립적으로 저장 |
| **시간 기반 파티셔닝** | PeriodConfig로 시간 범위별 다른 스토리지 백엔드 사용 가능 |
| **다중 캐시 계층** | 인덱스 캐시, 청크 캐시(L1/L2), Write Dedupe 캐시 |
| **플러거블 백엔드** | S3, GCS, Azure, Filesystem 등 다양한 오브젝트 스토어 지원 |
| **압축 최적화** | 블록 단위 압축으로 읽기/쓰기 성능 최적화 |

### 아키텍처 개요

```
+------------------------------------------------------------------+
|                          LokiStore                                |
|  +------------------------------------------------------------+  |
|  |                     CompositeStore                          |  |
|  |  +------------------+  +------------------+                 |  |
|  |  | storeEntry       |  | storeEntry       |  ...            |  |
|  |  | (period 1)       |  | (period 2)       |                 |  |
|  |  | - IndexReader    |  | - IndexReader    |                 |  |
|  |  | - ChunkWriter    |  | - ChunkWriter    |                 |  |
|  |  | - Fetcher        |  | - Fetcher        |                 |  |
|  |  +------------------+  +------------------+                 |  |
|  +------------------------------------------------------------+  |
|                                                                   |
|  +--------------+  +-----------+  +----------+  +-----------+    |
|  | indexReadCache|  |chunksCache|  |chunksCacheL2| |writeDedupeCache|
|  +--------------+  +-----------+  +----------+  +-----------+    |
+------------------------------------------------------------------+
         |                    |                    |
    +----+----+         +----+----+          +----+----+
    |  TSDB   |         | Object  |          |  Cache  |
    | Index   |         | Store   |          | Backend |
    | (disk)  |         | (S3/..) |          | (Redis/.|
    +---------+         +---------+          +---------+
```

---

## 2. Store 인터페이스 계층

Loki의 스토리지는 다중 레벨의 인터페이스로 구성된다. 각 레벨은 서로 다른 책임을 캡슐화한다.

### 2.1 stores.Store (하위 레벨)

```
// 파일: pkg/storage/stores/composite_store.go

type ChunkWriter interface {
    Put(ctx context.Context, chunks []chunk.Chunk) error
    PutOne(ctx context.Context, from, through model.Time, chunk chunk.Chunk) error
}

type ChunkFetcherProvider interface {
    GetChunkFetcher(tm model.Time) *fetcher.Fetcher
}

type ChunkFetcher interface {
    GetChunks(
        ctx context.Context,
        userID string,
        from, through model.Time,
        predicate chunk.Predicate,
        storeChunksOverride *logproto.ChunkRefGroup,
    ) ([][]chunk.Chunk, []*fetcher.Fetcher, error)
}

type Store interface {
    index.BaseReader       // GetSeries, LabelValuesForMetricName, LabelNamesForMetricName
    index.StatsReader      // Stats, Volume, GetShards
    index.Filterable       // SetChunkFilterer
    ChunkWriter            // Put, PutOne
    ChunkFetcher           // GetChunks
    ChunkFetcherProvider   // GetChunkFetcher
    Stop()
}
```

`stores.Store`는 인덱스 읽기, 청크 쓰기/읽기, 통계 수집 등의 기본 저장소 연산을 정의한다. `CompositeStore`가 이 인터페이스의 주요 구현체이다.

### 2.2 SelectStore (상위 레벨)

```
// 파일: pkg/storage/store.go

type SelectStore interface {
    SelectSamples(ctx context.Context, req logql.SelectSampleParams) (iter.SampleIterator, error)
    SelectLogs(ctx context.Context, req logql.SelectLogParams) (iter.EntryIterator, error)
    SelectSeries(ctx context.Context, req logql.SelectLogParams) ([]logproto.SeriesIdentifier, error)
}
```

`SelectStore`는 LogQL 엔진이 직접 호출하는 인터페이스이다. `SelectLogs`와 `SelectSamples`는 LogQL의 로그 쿼리와 메트릭 쿼리에 각각 대응한다.

### 2.3 storage.Store (통합 인터페이스)

```
// 파일: pkg/storage/store.go

type Store interface {
    stores.Store         // 하위 레벨 스토어
    SelectStore          // LogQL 연산
    SchemaConfigProvider // 스키마 설정 제공
    Instrumentable       // Pipeline/Extractor 래퍼 설정
}
```

최상위 `Store` 인터페이스는 모든 레벨의 기능을 통합한다. `LokiStore`가 이 인터페이스를 구현한다.

### 인터페이스 계층 다이어그램

```
                    storage.Store (최상위)
                    /       |        \
                   /        |         \
     stores.Store    SelectStore    Instrumentable
     /    |    \         |    \          |
    /     |     \        |     \         |
BaseReader Writer Fetcher Select Select  Set
                         Logs   Samples  Wrapper
```

**왜 이렇게 많은 인터페이스 레벨이 필요한가?**

1. **관심사 분리**: 하위 레벨은 원시 데이터 접근, 상위 레벨은 LogQL 의미론을 처리
2. **유연한 조합**: CompositeStore는 stores.Store만 구현하면 되고, LokiStore가 SelectStore를 추가
3. **테스트 용이성**: 각 레벨을 독립적으로 모킹 가능

---

## 3. LokiStore: 최상위 스토어

### 3.1 구조체 정의

```go
// 파일: pkg/storage/store.go

type LokiStore struct {
    stores.Store                      // CompositeStore 임베딩

    cfg       Config                  // 전체 스토리지 설정
    storeCfg  config.ChunkStoreConfig // 청크 스토어 설정
    schemaCfg config.SchemaConfig     // 스키마 설정 (PeriodConfig 목록)

    chunkMetrics       *ChunkMetrics
    chunkClientMetrics client.ChunkClientMetrics
    clientMetrics      ClientMetrics
    registerer         prometheus.Registerer

    indexReadCache   cache.Cache    // 인덱스 읽기 캐시
    chunksCache      cache.Cache    // 청크 L1 캐시
    chunksCacheL2    cache.Cache    // 청크 L2 캐시
    writeDedupeCache cache.Cache    // 쓰기 중복 제거 캐시

    limits StoreLimits
    logger log.Logger

    chunkFilterer               chunk.RequestChunkFilterer
    extractorWrapper            lokilog.SampleExtractorWrapper
    pipelineWrapper             lokilog.PipelineWrapper
    congestionControllerFactory func(...) congestion.Controller

    metricsNamespace string
}
```

LokiStore는 4종류의 캐시를 보유한다:

| 캐시 | 용도 | 무효화 |
|------|------|--------|
| `indexReadCache` | 인덱스 쿼리 결과 캐싱 | CacheGenNum 미들웨어로 무효화 |
| `chunksCache` | 최근 청크 데이터 (L1) | 불변이므로 무효화 불필요 |
| `chunksCacheL2` | 오래된 청크 데이터 (L2) | 시간 기반 핸드오프 |
| `writeDedupeCache` | 중복 쓰기 방지 | CacheGenNum 미들웨어로 무효화 |

### 3.2 초기화 과정

`NewStore()` 함수의 초기화 순서:

```
NewStore()
  |
  +-- 1. 캐시 생성
  |     +-- indexReadCache (인덱스 쿼리 캐시)
  |     +-- writeDedupeCache (쓰기 중복 제거)
  |     +-- chunksCache (L1 청크 캐시)
  |     +-- chunksCacheL2 (L2 청크 캐시)
  |
  +-- 2. 캐시 래핑
  |     +-- StopOnce: 다중 스토어가 공유하므로 Stop() 중복 호출 방지
  |     +-- CacheGenNumMiddleware: 세대 번호 기반 무효화 (chunksCache 제외)
  |
  +-- 3. CompositeStore 생성
  |
  +-- 4. s.init() 호출
        +-- PeriodConfig별 스토어 구성
```

### 3.3 init() - 피리어드별 스토어 구성

```go
// 파일: pkg/storage/store.go (라인 199~228)

func (s *LokiStore) init() error {
    for i, p := range s.schemaCfg.Configs {
        // 1. 피리어드별 청크 클라이언트 생성
        chunkClient, err := s.chunkClientForPeriod(p)

        // 2. Fetcher 생성 (L1/L2 캐시 포함)
        f, err := fetcher.New(s.chunksCache, s.chunksCacheL2, ...)

        // 3. 피리어드 종료 시간 계산
        periodEndTime := config.DayTime{Time: math.MaxInt64}
        if i < len(s.schemaCfg.Configs)-1 {
            periodEndTime = config.DayTime{Time: s.schemaCfg.Configs[i+1].From.Add(-time.Millisecond)}
        }

        // 4. 인덱스/쓰기 구성요소 생성
        w, idx, stop, err := s.storeForPeriod(p, ...)

        // 5. CompositeStore에 등록
        s.Store.(*stores.CompositeStore).AddStore(p.From.Time, f, idx, w, stop)
    }
    return nil
}
```

**왜 피리어드별로 분리하는가?**

- 스키마 마이그레이션: 예를 들어 v10에서 v13으로 업그레이드할 때, 기존 데이터는 v10 형식으로, 신규 데이터는 v13 형식으로 저장
- 스토리지 백엔드 변경: filesystem에서 S3로 전환할 때, 기존 데이터는 filesystem에서, 신규 데이터는 S3에서 읽기
- 인덱스 타입 변경: boltdb-shipper에서 TSDB로 전환

### 3.4 storeForPeriod() - TSDB 분기

```go
// 파일: pkg/storage/store.go (라인 261~329)

func (s *LokiStore) storeForPeriod(p config.PeriodConfig, ...) (...) {
    if p.IndexType == types.TSDBType {
        if shouldUseIndexGatewayClient(s.cfg.TSDBShipperConfig) {
            // Index Gateway 사용 (읽기 전용 모드)
            gw, _ := indexgateway.NewGatewayClient(...)
            idx := series.NewIndexGatewayClientStore(gw, ...)
            return failingChunkWriter{}, idx, ...
        }

        // 로컬 TSDB 스토어 생성
        objectClient, _ := NewObjectClient(p.ObjectType, ...)
        indexReaderWriter, stopFunc, _ := tsdb.NewStore(name, ...)
        chunkWriter := stores.NewChunkWriter(f, ...)
        return chunkWriter, indexReaderWriter, ...
    }

    // 레거시 인덱스 (boltdb 등)
    idx, _ := NewIndexClient(...)
    idx = series_index.NewCachingIndexClient(idx, s.indexReadCache, ...)
    ...
}
```

**TSDB 사용 시 흐름:**
```
storeForPeriod(TSDB)
  |
  +-- shouldUseIndexGatewayClient?
  |     |-- Yes: IndexGatewayClient (읽기 전용, gRPC)
  |     |-- No:  로컬 TSDB Store
  |
  +-- objectClient 생성 (S3/GCS/Azure)
  +-- tsdb.NewStore() (IndexShipper + HeadManager)
  +-- ChunkWriter 생성
```

---

## 4. CompositeStore: 멀티 피리어드 라우팅

### 4.1 구조와 역할

```go
// 파일: pkg/storage/stores/composite_store.go

type CompositeStore struct {
    limits StoreLimits
    stores []compositeStoreEntry
}

// compositeStoreEntry는 시작 시간과 Store를 묶는다
type compositeStoreEntry struct {
    start model.Time
    Store
}
```

CompositeStore는 **시간 범위 기반 라우팅** 패턴을 구현한다. 각 엔트리는 `start` 시간을 가지며, 요청된 시간 범위에 따라 적절한 스토어로 분배한다.

### 4.2 forStores() - 시간 범위 분배 알고리즘

```go
// 파일: pkg/storage/stores/composite_store.go (라인 351~390)

func (c CompositeStore) forStores(ctx context.Context, from, through model.Time,
    callback func(innerCtx context.Context, from, through model.Time, store Store) error) error {

    if len(c.stores) == 0 {
        return nil
    }

    // 1. from 이전 또는 같은 시작 시간을 가진 가장 가까운 스토어 찾기
    i := sort.Search(len(c.stores), func(i int) bool {
        return c.stores[i].start > from
    })
    if i > 0 {
        i--
    }

    // 2. through 이후 시작하는 첫 스토어 찾기
    j := sort.Search(len(c.stores), func(j int) bool {
        return c.stores[j].start > through
    })

    // 3. 각 스토어에 해당 시간 범위로 콜백 호출
    start := from
    for ; i < j; i++ {
        nextSchemaStarts := model.Latest
        if i+1 < len(c.stores) {
            nextSchemaStarts = c.stores[i+1].start
        }
        end := min(through, nextSchemaStarts-1)
        err := callback(ctx, start, end, c.stores[i])
        if err != nil {
            return err
        }
        start = nextSchemaStarts
    }
    return nil
}
```

### 시간 범위 분배 예시

```
stores:  [S1: 2024-01-01] [S2: 2024-07-01] [S3: 2025-01-01]

쿼리: from=2024-06-15, through=2025-02-01

분배 결과:
  S1: 2024-06-15 ~ 2024-06-30  (S2 시작 직전까지)
  S2: 2024-07-01 ~ 2024-12-31  (S3 시작 직전까지)
  S3: 2025-01-01 ~ 2025-02-01  (원래 through까지)
```

### 4.3 결과 병합 패턴

CompositeStore의 각 메서드는 forStores()를 사용하되, 결과 병합 방식이 다르다:

| 메서드 | 병합 방식 |
|--------|----------|
| `GetSeries` | 해시 기반 중복 제거 + 정렬 |
| `LabelValuesForMetricName` | `UniqueStrings`로 중복 제거 |
| `GetChunks` | 단순 append (중복 없음) |
| `Stats` | `MergeStats()`로 합산 |
| `Volume` | `seriesvolume.Merge()`로 합산 |
| `GetShards` | 가장 많은 샤드를 가진 그룹 선택 |

**GetShards의 특수한 병합:**

```go
// 파일: pkg/storage/stores/composite_store.go (라인 246~257)

// 샤드는 쉽게 병합할 수 없으므로, 가장 많은 샤드를 반환한 그룹을 선택
sort.Slice(groups, func(i, j int) bool {
    return len(groups[i].Shards) > len(groups[j].Shards)
})
return groups[0], nil
```

이 방식은 보수적(conservative) 전략이다. 더 세밀한 샤딩 결과를 선택함으로써 쿼리 병렬성이 떨어지는 것보다는, 약간의 불균형을 감수하되 충분한 병렬성을 보장한다.

---

## 5. 스키마 설정 (SchemaConfig)

### 5.1 PeriodConfig 구조체

```go
// 파일: pkg/storage/config/schema_config.go (라인 137~151)

type PeriodConfig struct {
    From        DayTime                  `yaml:"from"`          // 적용 시작일 (YYYY-MM-DD)
    IndexType   string                   `yaml:"store"`         // 인덱스 타입: tsdb, boltdb-shipper
    ObjectType  string                   `yaml:"object_store"`  // 오브젝트 스토어: s3, gcs, azure, filesystem
    Schema      string                   `yaml:"schema"`        // 스키마 버전: v11, v12, v13
    IndexTables IndexPeriodicTableConfig `yaml:"index"`         // 인덱스 테이블 설정
    ChunkTables PeriodicTableConfig      `yaml:"chunks"`        // 청크 테이블 설정
    RowShards   uint32                   `yaml:"row_shards"`    // 행 샤드 수 (v10+, 기본 16)
}
```

### 5.2 SchemaConfig: 피리어드 목록

```go
// 파일: pkg/storage/config/schema_config.go (라인 275~279)

type SchemaConfig struct {
    Configs  []PeriodConfig `yaml:"configs"`
    fileName string
}
```

### 5.3 DayTime과 TableRange

```go
// 파일: pkg/storage/config/schema_config.go (라인 190~192)

type DayTime struct {
    model.Time       // 밀리초 단위 Unix 타임스탬프
}

// 파일: pkg/storage/config/schema_config.go (라인 72~76)

type TableRange struct {
    Start, End   int64          // 테이블 번호 범위 (양쪽 포함)
    PeriodConfig *PeriodConfig  // 해당 피리어드 설정 참조
}
```

### 5.4 테이블 번호 계산

```go
// 파일: pkg/storage/config/schema_config.go (라인 168~181)

func (cfg *PeriodConfig) GetIndexTableNumberRange(schemaEndDate DayTime) TableRange {
    if cfg.IndexTables.Period == 0 {
        return TableRange{PeriodConfig: cfg}
    }
    return TableRange{
        Start:        cfg.From.Unix() / int64(cfg.IndexTables.Period/time.Second),
        End:          schemaEndDate.Unix() / int64(cfg.IndexTables.Period/time.Second),
        PeriodConfig: cfg,
    }
}
```

**테이블 번호 예시** (24시간 피리어드):

```
Unix epoch:  0
하루 초:     86400

2024-01-01 = Unix 1704067200
테이블 번호 = 1704067200 / 86400 = 19723

2024-01-02 → 테이블 번호 19724
2024-07-01 → 테이블 번호 19905
```

### 5.5 스키마 버전과 청크 포맷

```go
// 파일: pkg/storage/config/schema_config.go (라인 433~445)

func (cfg *PeriodConfig) ChunkFormat() (byte, chunkenc.HeadBlockFmt, error) {
    sver, _ := cfg.VersionAsInt()
    switch {
    case sver <= 12:
        return chunkenc.ChunkFormatV3, chunkenc.UnorderedHeadBlockFmt, nil
    default: // v13+
        return chunkenc.ChunkFormatV4, chunkenc.UnorderedWithStructuredMetadataHeadBlockFmt, nil
    }
}
```

| 스키마 버전 | 청크 포맷 | HeadBlock 포맷 | 특징 |
|------------|----------|---------------|------|
| v1~v11 | V1~V3 | OrderedHeadBlockFmt | 순서 보장 필수 |
| v12 | V3 | UnorderedHeadBlockFmt | 비순서 쓰기 지원 |
| v13 | V4 | UnorderedWithStructuredMetadata | 구조화된 메타데이터 |

### YAML 설정 예시

```yaml
schema_config:
  configs:
    - from: 2024-01-01
      store: tsdb
      object_store: s3
      schema: v13
      index:
        prefix: loki_index_
        period: 24h
        path_prefix: index/
```

---

## 6. 청크 인코딩 (MemChunk)

### 6.1 MemChunk 구조체

```go
// 파일: pkg/chunkenc/memchunk.go (라인 121~142)

type MemChunk struct {
    blockSize  int               // 비압축 블록 크기 (bytes)
    targetSize int               // 목표 압축 청크 크기

    symbolizer *symbolizer       // 라벨 심볼 테이블 (V4)
    blocks     []block           // 완성된 압축 블록들
    cutBlockSize int             // 모든 블록의 압축 크기 합

    head     HeadBlock           // 현재 쓰기 중인 미압축 블록
    format   byte                // 청크 포맷 버전 (V1~V4)
    encoding compression.Codec   // 압축 코덱
    headFmt  HeadBlockFmt        // 헤드블록 포맷

    compressedSize int           // 전체 압축 크기
}
```

### 6.2 block 구조체

```go
// 파일: pkg/chunkenc/memchunk.go (라인 144~153)

type block struct {
    b          []byte   // 압축된 바이트 데이터
    numEntries int      // 엔트리 수

    mint, maxt int64    // 블록 내 최소/최대 타임스탬프

    offset           int  // 청크 내 오프셋
    uncompressedSize int  // 비압축 크기
}
```

### 6.3 headBlock (미압축 쓰기 버퍼)

```go
// 파일: pkg/chunkenc/memchunk.go (라인 157~163)

type headBlock struct {
    entries []entry      // 원시 로그 엔트리 목록
    size    int          // 비압축 바이트 크기

    mint, maxt int64     // 최소/최대 타임스탬프
}

// 파일: pkg/chunkenc/memchunk.go (라인 353~357)

type entry struct {
    t                  int64          // 타임스탬프 (나노초)
    s                  string         // 로그 라인
    structuredMetadata labels.Labels  // 구조화된 메타데이터 (V4)
}
```

### 6.4 청크 내부 구조 (바이너리 레이아웃)

```
MemChunk 바이너리 포맷:
+------------------------------------------------------------------+
| Magic Number (4B) | Format Version (1B) | Encoding (1B)          |
+------------------------------------------------------------------+
| Block 1                                                           |
| +--------------------------------------------------------------+ |
| | numEntries (uvarint) | mint (varint64) | maxt (varint64)      | |
| | offset (uvarint)     | compressed_data_len (uvarint)         | |
| | [compressed block data...]                                    | |
| +--------------------------------------------------------------+ |
| Block 2                                                           |
| +--------------------------------------------------------------+ |
| | ...                                                           | |
| +--------------------------------------------------------------+ |
| ...                                                               |
| Block N                                                           |
+------------------------------------------------------------------+
| Section: Chunk Metas (V3+)                                        |
| Section: Structured Metadata (V4)                                 |
+------------------------------------------------------------------+
| Checksum (4B, CRC32 Castagnoli)                                   |
+------------------------------------------------------------------+
```

### 6.5 블록 압축 직렬화

```go
// 파일: pkg/chunkenc/memchunk.go (라인 202~231)

func (hb *headBlock) Serialise(pool compression.WriterPool) ([]byte, error) {
    inBuf := serializeBytesBufferPool.Get().(*bytes.Buffer)
    outBuf := &bytes.Buffer{}

    compressedWriter := pool.GetWriter(outBuf)
    for _, logEntry := range hb.entries {
        // 타임스탬프를 varint로 인코딩
        n := binary.PutVarint(encBuf, logEntry.t)
        inBuf.Write(encBuf[:n])

        // 로그 라인 길이를 uvarint로 인코딩
        n = binary.PutUvarint(encBuf, uint64(len(logEntry.s)))
        inBuf.Write(encBuf[:n])

        // 로그 라인 원본
        inBuf.WriteString(logEntry.s)
    }

    // 전체를 한번에 압축
    compressedWriter.Write(inBuf.Bytes())
    compressedWriter.Close()

    return outBuf.Bytes(), nil
}
```

**왜 블록 단위로 압축하는가?**

1. **부분 읽기 최적화**: 전체 청크를 복호화하지 않고 특정 블록만 복호화 가능
2. **시간 범위 스킵**: 블록의 mint/maxt로 불필요한 블록 건너뛰기
3. **메모리 효율**: 한 번에 하나의 블록만 메모리에 올리면 됨
4. **쓰기 지연 감소**: 작은 블록 단위로 빠르게 압축 가능

### 6.6 HeadBlockFmt 종류

```go
// 파일: pkg/chunkenc/memchunk.go (라인 78~87)

const (
    OrderedHeadBlockFmt                        HeadBlockFmt = iota + 3
    UnorderedHeadBlockFmt                                     // V3
    UnorderedWithStructuredMetadataHeadBlockFmt               // V4
)
```

| 포맷 | 순서 | 구조화 메타데이터 | 대응 ChunkFormat |
|------|------|-----------------|-----------------|
| OrderedHeadBlockFmt | 순서 필수 | 미지원 | V1, V2 |
| UnorderedHeadBlockFmt | 비순서 허용 | 미지원 | V3 |
| UnorderedWithStructuredMetadata | 비순서 허용 | 지원 | V4 |

### 6.7 청크 상수

```go
// 파일: pkg/chunkenc/memchunk.go (라인 31~48)

const (
    ChunkFormatV1 byte = iota + 1   // 기본
    ChunkFormatV2                    // + CRC 체크섬
    ChunkFormatV3                    // + 비순서 지원
    ChunkFormatV4                    // + 구조화 메타데이터

    blocksPerChunk = 10              // 청크당 기본 블록 수
    maxLineLength  = 1024 * 1024 * 1024  // 최대 로그 라인 길이 (1GB)
    defaultBlockSize = 256 * 1024    // 기본 블록 크기 (256KB)
)
```

---

## 7. 압축 코덱

### 7.1 지원 코덱

```go
// 파일: pkg/compression/codec.go (라인 9~26)

type Codec byte

const (
    None    Codec = iota   // 압축 없음
    GZIP                   // gzip (높은 압축률, 느림)
    Dumb                   // 미지원 (예약)
    LZ4_64k                // LZ4 64KB 블록
    Snappy                 // Snappy (빠른 압축/해제)
    LZ4_256k               // LZ4 256KB 블록
    LZ4_1M                 // LZ4 1MB 블록
    LZ4_4M                 // LZ4 4MB 블록 (기본 "lz4")
    Flate                  // Deflate
    Zstd                   // Zstandard (높은 압축률, 적당히 빠름)
)
```

### 7.2 코덱 선택 가이드

| 코덱 | 압축률 | 압축 속도 | 해제 속도 | 권장 용도 |
|------|-------|----------|----------|----------|
| Snappy | 낮음 | 매우 빠름 | 매우 빠름 | 높은 처리량 우선 |
| LZ4_4M | 중간 | 빠름 | 빠름 | 기본 권장 |
| GZIP | 높음 | 느림 | 느림 | 스토리지 비용 최적화 |
| Zstd | 높음 | 적당 | 빠름 | 압축률+속도 균형 |
| Flate | 높음 | 느림 | 중간 | 호환성 |

**왜 LZ4에 다양한 블록 크기 옵션이 있는가?**

LZ4의 블록 크기는 압축 윈도우를 결정한다. 작은 블록(64K)은 메모리를 적게 사용하지만 압축률이 낮고, 큰 블록(4M)은 더 높은 압축률을 제공한다. 로그 데이터의 평균 크기와 반복 패턴에 따라 최적값이 다르다.

---

## 8. TSDB 인덱스

### 8.1 TSDB Index 인터페이스

```go
// 파일: pkg/storage/stores/shipper/indexshipper/tsdb/index.go (라인 22~44)

type Index interface {
    Bounded                             // Bounds() (from, through model.Time)
    SetChunkFilterer(chunk.RequestChunkFilterer)
    Close() error
    sharding.ForSeries                  // ForSeries(...) 시리즈 순회

    GetChunkRefs(ctx context.Context, userID string, from, through model.Time,
        res []logproto.ChunkRefWithSizingInfo, fpFilter index.FingerprintFilter,
        matchers ...*labels.Matcher) ([]logproto.ChunkRefWithSizingInfo, error)

    Series(ctx context.Context, userID string, from, through model.Time,
        res []Series, fpFilter index.FingerprintFilter,
        matchers ...*labels.Matcher) ([]Series, error)

    LabelNames(ctx context.Context, userID string, from, through model.Time,
        matchers ...*labels.Matcher) ([]string, error)

    LabelValues(ctx context.Context, userID string, from, through model.Time,
        name string, matchers ...*labels.Matcher) ([]string, error)

    Stats(ctx context.Context, ...) error
    Volume(ctx context.Context, ...) error
}
```

### 8.2 ChunkMeta 구조체

```go
// 파일: pkg/storage/stores/shipper/indexshipper/tsdb/index/chunk.go (라인 13~22)

type ChunkMeta struct {
    Checksum uint32         // 청크 체크섬
    MinTime, MaxTime int64  // 시간 범위

    KB      uint32          // 저장 크기 (KB 단위, 반올림)
    Entries uint32          // 로그 엔트리 수
}

func (c ChunkMeta) From() model.Time    { return model.Time(c.MinTime) }
func (c ChunkMeta) Through() model.Time { return model.Time(c.MaxTime) }
```

ChunkMeta는 TSDB 인덱스에 저장되는 청크 메타데이터이다. `KB`와 `Entries` 필드를 통해 실제 청크를 가져오기 전에 크기를 알 수 있어, 쿼리 플래닝에 활용된다.

### 8.3 TOC (Table of Contents)

```go
// 파일: pkg/storage/stores/shipper/indexshipper/tsdb/index/index.go (라인 159~168)

type TOC struct {
    Symbols            uint64    // 심볼 테이블 오프셋
    Series             uint64    // 시리즈 데이터 오프셋
    LabelIndices       uint64    // 라벨 인덱스 오프셋
    LabelIndicesTable  uint64    // 라벨 인덱스 테이블 오프셋
    Postings           uint64    // 포스팅 리스트 오프셋
    PostingsTable      uint64    // 포스팅 테이블 오프셋
    FingerprintOffsets uint64    // 핑거프린트 오프셋 테이블
    Metadata           Metadata  // 메타데이터
}
```

### TSDB 파일 레이아웃

```
+--------------------------------------------------------------+
| Header: Magic (4B) + Version (1B)                             |
+--------------------------------------------------------------+
| Symbols Section                                               |
| +----------------------------------------------------------+ |
| | len | symbol_1 | symbol_2 | ... | symbol_N | CRC32       | |
| +----------------------------------------------------------+ |
+--------------------------------------------------------------+
| Series Section                                                |
| +----------------------------------------------------------+ |
| | series_1: labels + chunks[]                               | |
| | series_2: labels + chunks[]                               | |
| | ...                                                       | |
| +----------------------------------------------------------+ |
+--------------------------------------------------------------+
| Label Indices Section                                         |
+--------------------------------------------------------------+
| Postings Section                                              |
| +----------------------------------------------------------+ |
| | postings_1: [series_ref_1, series_ref_2, ...]            | |
| | postings_2: [series_ref_3, series_ref_4, ...]            | |
| +----------------------------------------------------------+ |
+--------------------------------------------------------------+
| Fingerprint Offsets Table                                     |
+--------------------------------------------------------------+
| TOC (Table of Contents)                                       |
+--------------------------------------------------------------+
```

### 8.4 TSDBIndex: Index 구현체

```go
// 파일: pkg/storage/stores/shipper/indexshipper/tsdb/single_file_index.go (라인 122~125)

type TSDBIndex struct {
    reader      IndexReader
    chunkFilter chunk.RequestChunkFilterer
}
```

### 8.5 ForSeries: 시리즈 순회의 핵심

```go
// 파일: pkg/storage/stores/shipper/indexshipper/tsdb/single_file_index.go (라인 164~201)

func (i *TSDBIndex) ForSeries(ctx context.Context, _ string,
    fpFilter index.FingerprintFilter,
    from model.Time, through model.Time,
    fn func(labels.Labels, model.Fingerprint, []index.ChunkMeta) (stop bool),
    matchers ...*labels.Matcher) error {

    var filterer chunk.Filterer
    if i.chunkFilter != nil {
        filterer = i.chunkFilter.ForRequest(ctx)
    }
    return i.forSeriesAndLabels(ctx, fpFilter, filterer, from, through, fn, matchers...)
}

func (i *TSDBIndex) forSeriesAndLabels(...) error {
    var ls labels.Labels
    chks := ChunkMetasPool.Get()         // 오브젝트 풀 활용
    defer func() { ChunkMetasPool.Put(chks) }()

    return i.forPostings(ctx, fpFilter, from, through, matchers, func(p index.Postings) error {
        for p.Next() {
            hash, err := i.reader.Series(p.At(), int64(from), int64(through), &ls, &chks)

            // 샤드 필터링
            if fpFilter != nil && !fpFilter.Match(model.Fingerprint(hash)) {
                continue
            }

            // 청크 필터링 (테넌트별 제한 등)
            if filterer != nil && filterer.ShouldFilter(ls) {
                continue
            }

            if stop := fn(ls, model.Fingerprint(hash), chks); stop {
                break
            }
        }
        return p.Err()
    })
}
```

**ForSeries의 실행 순서:**

```
ForSeries(matchers)
  |
  +-- forPostings(): 매처에 해당하는 Posting 리스트 조회
  |     |
  |     +-- PostingsForMatchers(): 각 매처의 포스팅을 교집합
  |
  +-- Postings 순회
        |
        +-- reader.Series(): 시리즈 ID로 라벨 + 청크메타 조회
        +-- fpFilter.Match(): 샤드 필터 적용
        +-- filterer.ShouldFilter(): 테넌트/커스텀 필터 적용
        +-- fn(): 콜백 호출 (라벨, 핑거프린트, 청크메타)
```

### 8.6 TSDBFile: 파일 기반 인덱스

```go
// 파일: pkg/storage/stores/shipper/indexshipper/tsdb/single_file_index.go (라인 86~95)

type TSDBFile struct {
    Identifier                     // 파일 위치 식별
    Index                          // TSDBIndex 임베딩
    getRawFileReader GetRawFileReaderFunc  // 원본 파일 리더
}
```

TSDBFile은 `shipperindex.Index` 인터페이스를 구현하여 IndexShipper에서 사용된다. 실제 디스크 파일을 메모리에 로드하여 `IndexReader`를 통해 접근한다.

### 8.7 인덱스 포맷 버전

```go
// 파일: pkg/storage/stores/shipper/indexshipper/tsdb/index/index.go (라인 47~59)

const (
    MagicIndex = 0xBAAAD700     // 매직 넘버
    HeaderLen  = 5              // 헤더 길이

    FormatV1 = 1                // 기본 포맷
    FormatV2 = 2                // 청크 통계 포함
    FormatV3 = 3                // 청크 배치 페이징 지원

    fingerprintInterval = 1 << 10  // 매 1024 시리즈마다 핑거프린트 오프셋 기록
    TenantLabel = "__loki_tenant__" // 멀티테넌트 인덱스의 테넌트 라벨
)
```

---

## 9. 인덱스 시퍼 (IndexShipper)

### 9.1 IndexShipper 역할

IndexShipper는 인덱스 파일의 전체 생명주기를 관리한다:
- **쓰기 경로**: 인제스터가 생성한 인덱스 파일을 오브젝트 스토어에 업로드
- **읽기 경로**: 쿼리에 필요한 인덱스 파일을 오브젝트 스토어에서 다운로드/캐싱

### 9.2 IndexShipper 인터페이스

```go
// 파일: pkg/storage/stores/shipper/indexshipper/shipper.go (라인 52~61)

type IndexShipper interface {
    // AddIndex: 불변 인덱스를 논리 테이블에 추가 (업로드 대기열)
    AddIndex(tableName, userID string, index index.Index) error

    // ForEach: 테이블의 각 인덱스 파일 순회
    // 쓰기: 업로드 대기 중인 파일 순회
    // 읽기: 다운로드된 파일 순회 (없으면 다운로드 후 순회)
    ForEach(ctx context.Context, tableName, userID string,
        callback index.ForEachIndexCallback) error

    ForEachConcurrent(ctx context.Context, tableName, userID string,
        callback index.ForEachIndexCallback) error

    Stop()
}
```

### 9.3 동작 모드

```go
// 파일: pkg/storage/stores/shipper/indexshipper/shipper.go (라인 28~37)

const (
    ModeReadWrite = Mode("RW")   // 읽기+쓰기 (인제스터)
    ModeReadOnly  = Mode("RO")   // 읽기 전용 (쿼리어)
    ModeWriteOnly = Mode("WO")   // 쓰기 전용
    ModeDisabled  = Mode("NO")   // 비활성화 (블록빌더)

    UploadInterval = 1 * time.Minute  // 업로드 체크 간격
)
```

### 9.4 indexShipper 내부 구조

```go
// 파일: pkg/storage/stores/shipper/indexshipper/shipper.go (라인 129~137)

type indexShipper struct {
    cfg               Config
    openIndexFileFunc  index.OpenIndexFileFunc   // TSDB 파일 열기 함수
    uploadsManager     uploads.TableManager      // 업로드 관리자
    downloadsManager   downloads.TableManager    // 다운로드 관리자
    logger             log.Logger
    stopOnce           sync.Once
}
```

### 9.5 Config 구조

```go
// 파일: pkg/storage/stores/shipper/indexshipper/shipper.go (라인 63~74)

type Config struct {
    ActiveIndexDirectory     string        // 인제스터가 인덱스를 쓰는 디렉토리
    CacheLocation            string        // 다운로드한 인덱스 캐시 위치
    CacheTTL                 time.Duration // 캐시 TTL (기본 24h)
    ResyncInterval           time.Duration // 오브젝트 스토어와 재동기화 간격 (기본 5m)
    QueryReadyNumDays        int           // 미리 다운로드할 일수
    IndexGatewayClientConfig indexgateway.ClientConfig

    IngesterName           string
    Mode                   Mode
    IngesterDBRetainPeriod time.Duration
}
```

### 9.6 업로드/다운로드 사이클

```
인덱스 시퍼 생명주기:

쓰기 경로 (인제스터):
+----------+     +-----------+     +----------------+     +----------+
| HeadMgr  | --> | WAL       | --> | TSDB Build     | --> | Upload   |
| (in-mem) |     | (disk)    |     | (multitenant)  |     | Manager  |
+----------+     +-----------+     +----------------+     +----------+
                                                               |
                                                     1분 간격 업로드
                                                               |
                                                               v
                                                     +------------------+
                                                     | Object Store     |
                                                     | (S3/GCS/Azure)   |
                                                     +------------------+
                                                               |
                                                     5분 간격 다운로드
                                                               |
읽기 경로 (쿼리어):                                              v
+----------+     +---------------+     +-----------+     +----------+
| Query    | <-- | Index Client  | <-- | Download  | <-- | Local    |
|          |     |               |     | Manager   |     | Cache    |
+----------+     +---------------+     +-----------+     +----------+
```

### 9.7 HeadManager 디렉토리 구조

```go
// 파일: pkg/storage/stores/shipper/indexshipper/tsdb/head_manager.go (라인 65~92)

/*
HeadManager가 관리하는 디스크 구조:

tsdb/
  v1/
    scratch/                              # 임시 TSDB 파일 빌드 디렉토리
    wal/
      <timestamp>                         # WAL 파일 (인제스터 쓰기)
    multitenant/
      <timestamp>-<ingester-name>.tsdb    # 멀티테넌트 TSDB (빌드 결과)
    per_tenant/
      <tenant>/
        <bucket>/
          index-<from>-<through>-<checksum>.tsdb  # 컴팩션된 테넌트별 인덱스
*/
```

```go
// 파일: pkg/storage/stores/shipper/indexshipper/tsdb/head_manager.go (라인 46~51)

const (
    defaultRotationPeriod      = period(15 * time.Minute)  // 헤드 로테이션 주기
    defaultRotationCheckPeriod = 1 * time.Minute           // 로테이션 체크 주기
)
```

HeadManager는 인제스터의 인덱스 쓰기를 관리한다:

1. **인메모리 Heads**: 현재 들어오는 시리즈/청크 메타데이터를 메모리에 유지
2. **WAL**: 인메모리 데이터의 내구성을 위한 선행 기록 로그
3. **로테이션**: 15분마다 현재 Heads를 TSDB 파일로 빌드
4. **업로드**: 빌드된 TSDB 파일을 IndexShipper를 통해 오브젝트 스토어에 업로드

---

## 10. 오브젝트 스토어 클라이언트

### 10.1 ObjectClient 인터페이스

```go
// 파일: pkg/storage/chunk/client/object_client.go (라인 24~48)

type ObjectClient interface {
    ObjectExists(ctx context.Context, objectKey string) (bool, error)
    GetAttributes(ctx context.Context, objectKey string) (ObjectAttributes, error)

    PutObject(ctx context.Context, objectKey string, object io.Reader) error
    GetObject(ctx context.Context, objectKey string) (io.ReadCloser, int64, error)
    GetObjectRange(ctx context.Context, objectKey string, off, length int64) (io.ReadCloser, error)

    List(ctx context.Context, prefix string, delimiter string) ([]StorageObject, []StorageCommonPrefix, error)
    DeleteObject(ctx context.Context, objectKey string) error
    IsObjectNotFoundErr(err error) bool
    IsRetryableErr(err error) bool
    Stop()
}
```

ObjectClient는 키-값 기반의 범용 오브젝트 저장소 인터페이스이다. `GetObjectRange`를 통해 부분 읽기를 지원하여, 큰 인덱스 파일의 특정 섹션만 읽을 수 있다.

### 10.2 Client (청크 클라이언트)

```go
// 파일: pkg/storage/chunk/client/client.go (라인 19~26)

type Client interface {
    Stop()
    PutChunks(ctx context.Context, chunks []chunk.Chunk) error
    GetChunks(ctx context.Context, chunks []chunk.Chunk) ([]chunk.Chunk, error)
    DeleteChunk(ctx context.Context, userID, chunkID string) error
    IsChunkNotFoundErr(err error) bool
    IsRetryableErr(err error) bool
}
```

`Client`는 `ObjectClient`를 래핑하여 청크 수준의 추상화를 제공한다. 청크의 인코딩/디코딩, 키 변환, 병렬 가져오기를 담당한다.

### 10.3 청크 클라이언트 래핑

```go
// 파일: pkg/storage/chunk/client/object_client.go (라인 88~108)

type client struct {
    store               ObjectClient      // 실제 오브젝트 스토어
    keyEncoder          KeyEncoder         // 키 인코딩 함수
    getChunkMaxParallel int               // 병렬 가져오기 최대 수 (기본 150)
    schema              config.SchemaConfig
}
```

### 10.4 병렬 청크 가져오기

```go
// 파일: pkg/storage/chunk/client/object_client.go (라인 158~164)

func (o *client) GetChunks(ctx context.Context, chunks []chunk.Chunk) ([]chunk.Chunk, error) {
    getChunkMaxParallel := o.getChunkMaxParallel
    if getChunkMaxParallel == 0 {
        getChunkMaxParallel = defaultMaxParallel  // 150
    }
    return util.GetParallelChunks(ctx, getChunkMaxParallel, chunks, o.getChunk)
}
```

`getChunkMaxParallel`은 동시에 오브젝트 스토어에서 가져올 수 있는 청크 수를 제한한다. 기본값 150은 대부분의 클라우드 오브젝트 스토어에서 안정적으로 동작하는 수준이다.

### 10.5 키 인코딩 전략

```go
// 파일: pkg/storage/chunk/client/object_client.go (라인 71~84)

var FSEncoder = func(schema config.SchemaConfig, chk chunk.Chunk) string {
    key := schema.ExternalKey(chk.ChunkRef)
    if schema.VersionForChunk(chk.ChunkRef) > 11 {
        // v12+: 디렉토리 구조 유지, 마지막 부분만 base64
        split := strings.LastIndexByte(key, '/')
        encodedTail := base64Encoder(key[split+1:])
        return strings.Join([]string{key[:split], encodedTail}, "/")
    }
    // v11 이하: 전체 base64
    return base64Encoder(key)
}
```

**v12 이후 키 구조:**
```
fake/tenant_id/YhJhNQ==

fake/              -> 테넌트 디렉토리
  tenant_id/       -> 테넌트 ID 디렉토리
    YhJhNQ==       -> base64 인코딩된 청크 ID
```

이 변경의 이유: 수백만 개의 청크를 단일 디렉토리에 저장하면 파일시스템 성능이 크게 저하된다. 디렉토리 구조를 유지함으로써 객체 목록 조회와 접근 성능이 개선된다.

### 10.6 지원 오브젝트 스토어

| 스토어 | ObjectType 값 | 특징 |
|--------|-------------|------|
| AWS S3 | `s3` 또는 `aws` | 가장 널리 사용, SSE 지원 |
| Google Cloud Storage | `gcs` | Google Cloud 네이티브 |
| Azure Blob Storage | `azure` | Azure 네이티브 |
| Alibaba Cloud OSS | `alibabacloud` | 알리바바 클라우드 |
| Filesystem | `filesystem` | 로컬/NFS, 개발/테스트용 |
| Baidu BOS | `bos` | 바이두 클라우드 |
| Tencent COS | `cos` | 텐센트 클라우드 |
| OpenStack Swift | `swift` | OpenStack 환경 |

---

## 11. 캐시 계층

### 11.1 Cache 인터페이스

```go
// 파일: pkg/storage/chunk/cache/cache.go (라인 18~24)

type Cache interface {
    Store(ctx context.Context, key []string, buf [][]byte) error
    Fetch(ctx context.Context, keys []string) (found []string, bufs [][]byte, missing []string, err error)
    Stop()
    GetCacheType() stats.CacheType
}
```

Cache 인터페이스는 단순한 키-값 저장소이다. `Fetch`는 found/missing을 분리하여 반환하므로, 호출자가 missing 키에 대해 다른 소스에서 가져올 수 있다.

### 11.2 캐시 설정 구조

```go
// 파일: pkg/storage/chunk/cache/cache.go (라인 27~41)

type Config struct {
    DefaultValidity time.Duration         `yaml:"default_validity"`

    Background     BackgroundConfig       `yaml:"background"`
    Memcache       MemcachedConfig        `yaml:"memcached"`
    MemcacheClient MemcachedClientConfig  `yaml:"memcached_client"`
    Redis          RedisConfig            `yaml:"redis"`
    EmbeddedCache  EmbeddedCacheConfig    `yaml:"embedded_cache"`

    Prefix string `yaml:"prefix"`  // 메트릭 접두사
    Cache  Cache  `yaml:"-"`       // 테스트용 주입
}
```

### 11.3 캐시 생성 로직

```go
// 파일: pkg/storage/chunk/cache/cache.go (라인 88~141)

func New(cfg Config, reg prometheus.Registerer, ...) (Cache, error) {
    var caches []Cache

    // 1. EmbeddedCache (인메모리)
    if cfg.EmbeddedCache.IsEnabled() {
        cache := NewEmbeddedCache(cfg.Prefix+"embedded-cache", ...)
        caches = append(caches, CollectStats(Instrument(cache)))
    }

    // 2. Memcached (둘 중 하나만 가능)
    if IsMemcacheSet(cfg) {
        client := NewMemcachedClient(...)
        cache := NewMemcached(...)
        caches = append(caches, CollectStats(NewBackground(Instrument(cache))))
    }

    // 3. Redis (둘 중 하나만 가능)
    if IsRedisSet(cfg) {
        client, _ := NewRedisClient(&cfg.Redis)
        cache := NewRedisCache(...)
        caches = append(caches, CollectStats(NewBackground(Instrument(cache))))
    }

    // 4. Tiered 캐시로 조합
    cache := NewTiered(caches)
    if len(caches) > 1 {
        cache = Instrument(cfg.Prefix+"tiered", cache, reg)
    }
    return cache, nil
}
```

### 캐시 래핑 체인

```
원본 캐시 (EmbeddedCache / Memcached / Redis)
  |
  +-- Instrument: 메트릭 수집 (hit/miss ratio, latency)
  |
  +-- Background: 비동기 쓰기 (외부 캐시에 적용)
  |
  +-- CollectStats: 쿼리 통계 수집
  |
  +-- Tiered: 다중 캐시 계층화
```

### 11.4 Tiered 캐시

```go
// 파일: pkg/storage/chunk/cache/tiered.go

type tiered []Cache

func (t tiered) Fetch(ctx context.Context, keys []string) (...) {
    found := make(map[string][]byte, len(keys))
    missing := keys
    previousCaches := make([]Cache, 0, len(t))

    for _, c := range []Cache(t) {
        // 현재 캐시에서 찾기
        passKeys, passBufs, missing, err = c.Fetch(ctx, missing)

        // 이전 캐시들에 write-back (하위 레벨에서 찾은 것을 상위에 저장)
        tiered(previousCaches).Store(ctx, passKeys, passBufs)

        for i, key := range passKeys {
            found[key] = passBufs[i]
        }

        if len(missing) == 0 {
            break  // 모두 찾음
        }

        previousCaches = append(previousCaches, c)
    }

    return resultKeys, resultBufs, missing, nil
}
```

**Tiered 캐시 동작 원리:**

```
Fetch(["a", "b", "c", "d"])

Step 1: EmbeddedCache.Fetch(["a","b","c","d"])
  → found: ["a"], missing: ["b","c","d"]

Step 2: Memcached.Fetch(["b","c","d"])
  → found: ["b","c"], missing: ["d"]
  → EmbeddedCache.Store(["b","c"])  ← write-back!

Step 3: 결과 조합
  → found: ["a","b","c"], missing: ["d"]
  → missing "d"는 Object Store에서 가져와야 함
```

**왜 write-back을 하는가?**

하위 레벨 캐시(Memcached)에서 찾은 데이터를 상위 레벨(EmbeddedCache)에 저장함으로써, 동일 키에 대한 후속 요청이 더 빠른 레벨에서 응답될 수 있다. 이는 일종의 캐시 워밍(warming) 효과를 준다.

### 11.5 EmbeddedCache (인메모리)

```go
// 파일: pkg/storage/chunk/cache/embeddedcache.go (라인 49~69)

type EmbeddedCache[K comparable, V any] struct {
    cacheType stats.CacheType

    lock          sync.RWMutex
    maxSizeItems  int              // 최대 아이템 수
    maxSizeBytes  uint64           // 최대 메모리 (바이트)
    currSizeBytes uint64           // 현재 메모리 사용량

    entries map[K]*list.Element    // 해시맵 (O(1) 조회)
    lru     *list.List             // LRU 순서 유지 (doubly-linked list)

    done chan struct{}             // 퍼지 고루틴 종료 신호
    // ... 메트릭
}

// 파일: pkg/storage/chunk/cache/embeddedcache.go (라인 78~87)

type EmbeddedCacheConfig struct {
    Enabled      bool          `yaml:"enabled,omitempty"`
    MaxSizeMB    int64         `yaml:"max_size_mb"`      // 최대 메모리 (MB, 기본 100)
    MaxSizeItems int           `yaml:"max_size_items"`   // 최대 아이템 수
    TTL          time.Duration `yaml:"ttl"`              // TTL (기본 1h)
}
```

EmbeddedCache는 FIFO 슬라이드 기반의 LRU 캐시이다. 제네릭을 활용하여 `string -> []byte` 뿐 아니라 다양한 타입 조합을 지원한다.

**두 가지 제거 정책:**

| 정책 | 설정 | 동작 |
|------|------|------|
| 아이템 수 기반 | `MaxSizeItems` | 아이템 수 초과 시 LRU 제거 |
| 메모리 기반 | `MaxSizeMB` | 메모리 초과 시 LRU 제거 |
| TTL 기반 | `TTL` | 1분 간격으로 만료 아이템 퍼지 |

두 설정이 모두 있으면 **먼저 도달하는** 쪽이 적용된다.

### 11.6 LokiStore의 4대 캐시

```go
// 파일: pkg/storage/store.go (라인 122~156)

// NewStore에서의 캐시 초기화:

// 1. 인덱스 읽기 캐시
indexReadCache, _ := cache.New(cfg.IndexQueriesCacheConfig, ...)

// 2. Write Dedupe 캐시 (레거시, TSDB에서는 불필요)
writeDedupeCache, _ := cache.New(storeCfg.WriteDedupeCacheConfig, ...)

// 3. 청크 L1 캐시 (prefix: "chunks")
chunkCacheCfg.Prefix = "chunks"
chunksCache, _ := cache.New(chunkCacheCfg, ...)

// 4. 청크 L2 캐시 (prefix: "chunksl2")
chunkCacheCfgL2.Prefix = "chunksl2"
chunksCacheL2, _ := cache.New(chunkCacheCfgL2, ...)

// StopOnce 래핑: 다중 피리어드 스토어가 공유하므로 중복 Stop 방지
indexReadCache = cache.StopOnce(indexReadCache)
chunksCache = cache.StopOnce(chunksCache)
chunksCacheL2 = cache.StopOnce(chunksCacheL2)
writeDedupeCache = cache.StopOnce(writeDedupeCache)

// CacheGenNum 미들웨어: 세대 번호로 캐시 무효화
// chunksCache는 제외 (청크는 불변이므로)
indexReadCache = cache.NewCacheGenNumMiddleware(indexReadCache)
writeDedupeCache = cache.NewCacheGenNumMiddleware(writeDedupeCache)
```

**왜 chunksCache에는 CacheGenNum을 적용하지 않는가?**

청크의 내용은 ID(체크섬)에 의해 결정되므로, 같은 ID의 청크는 항상 같은 내용을 가진다. 따라서 청크 캐시는 무효화할 필요가 없다. 인덱스가 변경(삭제)되면 해당 청크를 가리키는 인덱스 엔트리가 사라지므로, 청크 캐시에 남아있어도 참조되지 않는다.

---

## 12. Chunk Fetcher

### 12.1 Fetcher 구조체

```go
// 파일: pkg/storage/chunk/fetcher/fetcher.go (라인 47~61)

type Fetcher struct {
    schema     config.SchemaConfig
    storage    client.Client          // 오브젝트 스토어 클라이언트
    cache      cache.Cache            // L1 캐시
    cachel2    cache.Cache            // L2 캐시
    cacheStubs bool                   // 스텁만 캐시할지 여부

    l2CacheHandoff                   time.Duration  // L2 전환 시간
    skipQueryWritebackCacheOlderThan time.Duration  // 캐시 write-back 스킵

    wait           sync.WaitGroup
    decodeRequests chan decodeRequest  // 디코딩 요청 채널

    stopOnce sync.Once
}
```

### 12.2 병렬 디코딩 워커

```go
// 파일: pkg/storage/chunk/fetcher/fetcher.go (라인 42~93)

const chunkDecodeParallelism = 16  // 병렬 디코딩 워커 수

func New(cache cache.Cache, cachel2 cache.Cache, ...) (*Fetcher, error) {
    c := &Fetcher{
        // ...
        decodeRequests: make(chan decodeRequest),
    }

    // 16개 디코딩 워커 시작
    c.wait.Add(chunkDecodeParallelism)
    for i := 0; i < chunkDecodeParallelism; i++ {
        go c.worker()
    }
    return c, nil
}

func (c *Fetcher) worker() {
    defer c.wait.Done()
    decodeContext := chunk.NewDecodeContext()
    for req := range c.decodeRequests {
        err := req.chunk.Decode(decodeContext, req.buf)
        if err != nil {
            cacheCorrupt.Inc()
        }
        req.responses <- decodeResponse{
            chunk: req.chunk,
            err:   err,
        }
    }
}
```

### 12.3 FetchChunks: L1/L2 이중 캐시 전략

```go
// 파일: pkg/storage/chunk/fetcher/fetcher.go (라인 128~233)

func (c *Fetcher) FetchChunks(ctx context.Context, chunks []chunk.Chunk) ([]chunk.Chunk, error) {
    // L2 핸드오프에 10% 마진 추가 (슬라이딩 윈도우 오버랩)
    extendedHandoff := c.l2CacheHandoff + (c.l2CacheHandoff / 10)

    keys := make([]string, 0, len(chunks))
    l2OnlyChunks := make([]chunk.Chunk, 0, len(chunks))

    for _, m := range chunks {
        // 너무 오래된 청크는 캐시 write-back 스킵
        if c.skipQueryWritebackCacheOlderThan > 0 &&
           m.From.Time().Before(time.Now().UTC().Add(-c.skipQueryWritebackCacheOlderThan)) {
            continue
        }
        // L2 핸드오프보다 오래된 청크는 L2에서만 검색
        if c.l2CacheHandoff > 0 &&
           m.From.Time().Before(time.Now().UTC().Add(-extendedHandoff)) {
            l2OnlyChunks = append(l2OnlyChunks, m)
            continue
        }
        keys = append(keys, c.schema.ExternalKey(m.ChunkRef))
    }

    // 1단계: L1 캐시에서 가져오기
    cacheHits, cacheBufs, _, _ := c.cache.Fetch(ctx, keys)

    // 2단계: L2 캐시에서 나머지 가져오기
    if c.l2CacheHandoff > 0 {
        cacheHitsL2, cacheBufsL2, _, _ := c.cachel2.Fetch(ctx, missingL1Keys)
        cacheHits = append(cacheHits, cacheHitsL2...)
        cacheBufs = append(cacheBufs, cacheBufsL2...)
    }

    // 3단계: 캐시 히트 디코딩 + 미스 계산
    fromCache, missing, _ := c.processCacheResponse(ctx, chunks, cacheHits, cacheBufs)

    // 4단계: 미싱 청크를 스토리지에서 가져오기
    var fromStorage []chunk.Chunk
    if len(missing) > 0 {
        fromStorage, _ = c.storage.GetChunks(ctx, missing)
    }

    // 5단계: 스토리지에서 가져온 청크를 캐시에 write-back
    c.WriteBackCache(ctx, fromStorage)

    return append(fromCache, fromStorage...), nil
}
```

### L1/L2 캐시 핸드오프 다이어그램

```
시간축:
  ←── 과거 ─────────────────── 현재 ──→

  [skip zone]  [L2 only zone]  [L1 zone]
  ←─────────→  ←────────────→  ←───────→
  skipWriteback   l2CacheHandoff  최근 데이터

  |             |                |         now
  ← 매우 오래됨   ← L2 핸드오프      ← L1 핸드오프

  캐시 쓰기 안함    L2에만 쓰기       L1에만 쓰기
  스토어 직접조회   L2에서 읽기       L1에서 읽기
```

### 12.4 WriteBackCache: L1/L2 분기

```go
// 파일: pkg/storage/chunk/fetcher/fetcher.go (라인 235~278)

func (c *Fetcher) WriteBackCache(ctx context.Context, chunks []chunk.Chunk) error {
    keys := make([]string, 0, len(chunks))
    bufs := make([][]byte, 0, len(chunks))
    keysL2 := make([]string, 0, len(chunks))
    bufsL2 := make([][]byte, 0, len(chunks))

    for i := range chunks {
        // 너무 오래된 청크는 write-back 스킵
        if c.skipQueryWritebackCacheOlderThan > 0 &&
           chunks[i].From.Time().Before(time.Now().UTC().Add(-c.skipQueryWritebackCacheOlderThan)) {
            continue
        }

        encoded, _ := chunks[i].Encoded()

        // L2 핸드오프 기준으로 캐시 결정
        if c.l2CacheHandoff == 0 ||
           chunks[i].From.Time().After(time.Now().UTC().Add(-c.l2CacheHandoff)) {
            keys = append(keys, c.schema.ExternalKey(chunks[i].ChunkRef))
            bufs = append(bufs, encoded)
        } else {
            keysL2 = append(keysL2, c.schema.ExternalKey(chunks[i].ChunkRef))
            bufsL2 = append(bufsL2, encoded)
        }
    }

    c.cache.Store(ctx, keys, bufs)    // L1 캐시 저장
    if len(keysL2) > 0 {
        c.cachel2.Store(ctx, keysL2, bufsL2)  // L2 캐시 저장
    }
    return nil
}
```

### 12.5 processCacheResponse: 병렬 디코딩

```go
// 파일: pkg/storage/chunk/fetcher/fetcher.go (라인 282~328)

func (c *Fetcher) processCacheResponse(ctx context.Context, chunks []chunk.Chunk,
    keys []string, bufs [][]byte) ([]chunk.Chunk, []chunk.Chunk, error) {

    // 캐시 히트된 키를 맵으로 구성
    cm := make(map[string][]byte, len(chunks))
    for i, k := range keys {
        cm[k] = bufs[i]
    }

    // 히트/미스 분류
    var requests []decodeRequest
    var missing []chunk.Chunk
    for i, ck := range chunks {
        if b, ok := cm[c.schema.ExternalKey(ck.ChunkRef)]; ok {
            requests = append(requests, decodeRequest{
                chunk: chunks[i], buf: b, responses: responses,
            })
        } else {
            missing = append(missing, chunks[i])
        }
    }

    // 워커에 디코딩 요청 전송 (비동기)
    go func() {
        for _, request := range requests {
            c.decodeRequests <- request
        }
    }()

    // 모든 응답 수집
    found := make([]chunk.Chunk, 0, len(requests))
    for i := 0; i < len(requests); i++ {
        response := <-responses
        if response.err != nil {
            err = response.err
        } else {
            found = append(found, response.chunk)
        }
    }
    return found, missing, err
}
```

**왜 16개 고정 워커인가?**

청크 디코딩은 CPU 집약적이다(압축 해제 + 바이너리 파싱). 16개 워커는 대부분의 서버 환경에서 CPU 코어 수와 잘 맞으며, 너무 많은 고루틴에 의한 컨텍스트 스위칭 비용을 피한다. 이 값은 `chunkDecodeParallelism` 상수로 고정되어 있다.

---

## 13. 읽기 경로 전체 흐름

### 13.1 SelectLogs 전체 흐름

```go
// 파일: pkg/storage/store.go (라인 480~520)

func (s *LokiStore) SelectLogs(ctx context.Context, req logql.SelectLogParams) (iter.EntryIterator, error) {
    // 1. 요청 디코딩: 매처, 시간 범위 추출
    matchers, from, through, _ := decodeReq(req)

    // 2. LazyChunks 조회: 인덱스에서 청크 참조 가져오기
    lazyChunks, _ := s.lazyChunks(ctx, from, through,
        chunk.NewPredicate(matchers, req.Plan), req.GetStoreChunks())

    // 3. 파이프라인 구성: 필터, 파서 등
    expr, _ := req.LogSelector()
    pipeline, _ := expr.Pipeline()
    pipeline, _ = deletion.SetupPipeline(req, pipeline)

    // 4. 파이프라인 래퍼 적용
    if s.pipelineWrapper != nil {
        pipeline = s.pipelineWrapper.Wrap(ctx, pipeline, ...)
    }

    // 5. 배치 이터레이터 생성 (실제 청크 데이터 가져오기는 지연)
    return newLogBatchIterator(ctx, s.schemaCfg, s.chunkMetrics,
        lazyChunks, s.cfg.MaxChunkBatchSize, matchers,
        pipeline, req.Direction, req.Start, req.End, ...)
}
```

### 13.2 lazyChunks: 인덱스 → 청크 참조

```go
// 파일: pkg/storage/store.go (라인 393~437)

func (s *LokiStore) lazyChunks(ctx context.Context, from, through model.Time,
    predicate chunk.Predicate, storeChunksOverride *logproto.ChunkRefGroup) ([]*LazyChunk, error) {

    // 1. CompositeStore.GetChunks(): 인덱스에서 청크 참조 조회
    chks, fetchers, _ := s.GetChunks(ctx, userID, from, through, predicate, storeChunksOverride)

    // 2. 시간 범위 필터링
    for i := range chks {
        prefiltered += len(chks[i])
        chks[i] = filterChunksByTime(from, through, chks[i])
        filtered += len(chks[i])
    }

    // 3. LazyChunk 생성 (아직 데이터를 가져오지 않음)
    lazyChunks := make([]*LazyChunk, 0, filtered)
    for i := range chks {
        for _, c := range chks[i] {
            lazyChunks = append(lazyChunks, &LazyChunk{
                Chunk:   c,
                Fetcher: fetchers[i],  // 나중에 이 Fetcher로 데이터 가져오기
            })
        }
    }
    return lazyChunks, nil
}
```

### 전체 읽기 경로 시퀀스

```
LogQL Engine
  |
  v
LokiStore.SelectLogs()
  |
  +-- decodeReq(): 매처 + 시간 범위 추출
  |
  +-- lazyChunks()
  |     |
  |     +-- CompositeStore.GetChunks()
  |     |     |
  |     |     +-- forStores(): 시간 범위별 스토어 분배
  |     |           |
  |     |           +-- storeEntry.GetChunks()
  |     |                 |
  |     |                 +-- indexReader.GetChunkRefs(): TSDB 인덱스 조회
  |     |                 |     |
  |     |                 |     +-- ForSeries(): 매처 → 포스팅 → 시리즈 → ChunkMeta
  |     |                 |
  |     |                 +-- Fetcher.GetChunkFetcher()에서 Fetcher 반환
  |     |
  |     +-- filterChunksByTime(): 시간 범위로 추가 필터링
  |     +-- LazyChunk[] 생성
  |
  +-- Pipeline 구성 (필터, 파서 등)
  |
  +-- newLogBatchIterator(): 배치 이터레이터 생성
        |
        +-- (지연 실행) LazyChunk.Fetch()
              |
              +-- Fetcher.FetchChunks()
                    |
                    +-- L1 캐시 조회
                    +-- L2 캐시 조회 (핸드오프 기준)
                    +-- 스토리지 조회 (캐시 미스)
                    +-- 병렬 디코딩 (16 워커)
                    +-- Write-back to 캐시
```

---

## 14. 쓰기 경로와 스토리지

### 14.1 인제스터에서 스토리지까지

```
Ingester
  |
  +-- stream.Push()
  |     +-- MemChunk.Append() → headBlock에 엔트리 추가
  |
  +-- Flush (조건 충족 시)
  |     +-- headBlock → block (압축)
  |     +-- MemChunk.Encoded() → 바이트 직렬화
  |
  +-- ChunkWriter.PutOne()
  |     +-- Fetcher: 인덱스에 청크 참조 기록
  |     +-- client.PutChunks(): 오브젝트 스토어에 청크 데이터 저장
  |
  +-- HeadManager.Append()
        +-- WAL에 인덱스 엔트리 기록
        +-- 15분 로테이션 → TSDB 파일 빌드
        +-- IndexShipper.AddIndex(): 오브젝트 스토어에 인덱스 업로드
```

### 14.2 데이터 내구성 보장

```
쓰기 내구성 계층:

Layer 1: 인메모리 (headBlock)
  - 위험: 프로세스 크래시 시 손실
  - 보호: WAL로 복구 가능

Layer 2: WAL (Write-Ahead Log)
  - 위험: 디스크 장애 시 손실
  - 보호: 다중 인제스터 복제 (replication_factor)

Layer 3: Flush된 청크 (오브젝트 스토어)
  - 위험: 오브젝트 스토어 장애
  - 보호: 오브젝트 스토어 자체 복제 (S3의 경우 11 nines)

Layer 4: TSDB 인덱스 (오브젝트 스토어)
  - 위험: 인덱스-청크 불일치
  - 보호: 체크섬 검증, 컴팩션
```

---

## 15. 운영 관점 핵심 포인트

### 15.1 스키마 마이그레이션

```yaml
# 안전한 스키마 마이그레이션 예시
schema_config:
  configs:
    # 기존 피리어드 (변경하지 않음!)
    - from: 2024-01-01
      store: tsdb
      object_store: s3
      schema: v12
      index:
        prefix: loki_index_
        period: 24h
    # 새 피리어드 (미래 날짜로 추가)
    - from: 2025-01-01
      store: tsdb
      object_store: s3
      schema: v13
      index:
        prefix: loki_index_v13_
        period: 24h
```

**주의사항:**
- 기존 피리어드 설정은 절대 변경하지 말 것
- 새 피리어드는 반드시 미래 날짜로 설정
- 인덱스 prefix를 다르게 하면 테이블 충돌 방지

### 15.2 캐시 사이징 가이드

| 캐시 | 권장 크기 | 기준 |
|------|----------|------|
| indexReadCache | 1~2GB | 활성 시리즈 수에 비례 |
| chunksCache (L1) | 4~8GB | 최근 쿼리 패턴의 워킹셋 |
| chunksCacheL2 | 8~16GB | 장기 쿼리의 히트율 목표 |
| writeDedupeCache | 256MB~1GB | TSDB 사용 시 불필요 |

### 15.3 주요 메트릭

| 메트릭 | 의미 |
|--------|------|
| `loki_cache_corrupt_chunks_total` | 캐시에서 발견된 손상 청크 수 |
| `loki_chunk_fetcher_fetched_size_bytes` | 청크 크기 분포 (source: cache/cache_l2/store) |
| `loki_embeddedcache_entries_current` | 현재 캐시 엔트리 수 |
| `loki_embeddedcache_memory_bytes` | 현재 캐시 메모리 사용량 |
| `loki_tsdb_shipper_*` | 인덱스 시퍼 업로드/다운로드 메트릭 |

### 15.4 성능 튜닝 요약

| 항목 | 설정 | 효과 |
|------|------|------|
| `chunk-target-size` | 1.5MB~2MB | 청크 크기 ↑ → 오브젝트 수 ↓, 읽기 효율 ↑ |
| `chunk-block-size` | 256KB | 블록 크기 ↑ → 압축률 ↑, 부분 읽기 효율 ↓ |
| `chunk-encoding` | snappy/lz4 | snappy: 속도, lz4: 균형, zstd: 압축률 |
| `l2-chunk-cache-handoff` | 24h~72h | L1→L2 전환 시점 조절 |
| `max-chunk-batch-size` | 50 | 배치 크기 ↑ → 처리량 ↑, 메모리 ↑ |

---

## 요약

Loki의 스토리지 레이어는 다음과 같은 핵심 설계로 대규모 로그 데이터를 효율적으로 관리한다:

1. **계층적 인터페이스**: `stores.Store` → `SelectStore` → `storage.Store`로 관심사를 명확히 분리
2. **시간 기반 라우팅**: `CompositeStore`의 `forStores()`가 이진 검색으로 O(log n) 라우팅
3. **이중 인덱스/데이터 분리**: TSDB 인덱스(메타데이터)와 오브젝트 스토어(청크 데이터)의 독립 관리
4. **다중 캐시 계층**: L1(빠름/작음) → L2(느림/큼) → 스토리지(영구) 순서로 폴스루
5. **블록 기반 압축**: MemChunk의 블록 단위 압축으로 부분 읽기와 시간 범위 스킵 최적화
6. **지연 로딩**: LazyChunk 패턴으로 실제 데이터 접근을 이터레이션 시점까지 지연
7. **생명주기 관리**: IndexShipper의 업로드/다운로드 사이클로 인덱스 자동 배포

이러한 설계는 Prometheus의 TSDB에서 영감을 받았지만, 로그 데이터의 특수성(대용량, 비정형, append-only)에 맞게 청크 인코딩, 캐시 전략, 인덱스 구조를 완전히 재설계한 결과이다.
