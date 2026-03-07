# 13. 블룸 필터 서브시스템 Deep-Dive

## 목차

1. [개요](#1-개요)
2. [블룸 필터 기초 이론](#2-블룸-필터-기초-이론)
3. [ScalableBloomFilter 구현](#3-scalablebloomfilter-구현)
4. [Bloom 래퍼와 인코딩/디코딩](#4-bloom-래퍼와-인코딩디코딩)
5. [블룸 블록 구조](#5-블룸-블록-구조)
6. [Bloom Gateway](#6-bloom-gateway)
7. [Bloom Gateway Worker](#7-bloom-gateway-worker)
8. [Bloom Gateway Processor](#8-bloom-gateway-processor)
9. [GatewayClient: DNS 기반 디스커버리](#9-gatewayclient-dns-기반-디스커버리)
10. [Bloom Builder](#10-bloom-builder)
11. [Bloom Planner](#11-bloom-planner)
12. [쿼리 가속: FilterChunkRefs 흐름](#12-쿼리-가속-filterchunkrefs-흐름)
13. [PrefetchBloomBlocks: 사전 다운로드](#13-prefetchbloomblocks-사전-다운로드)
14. [설계 결정 분석](#14-설계-결정-분석)

---

## 1. 개요

Loki의 블룸 필터 서브시스템은 쿼리 시 불필요한 청크를 사전 필터링하여 I/O를 크게 줄이는 쿼리 가속 기능이다. 확률적 자료구조인 블룸 필터를 각 청크의 로그 라인에 대해 빌드하고, 쿼리 시 로그 라인 필터(`|= "error"` 등)가 특정 청크에 존재할 수 있는지 빠르게 판단한다.

```
소스 위치:
- pkg/storage/bloom/v1/bloom.go     -- Bloom 래퍼, BloomPageDecoder
- pkg/storage/bloom/v1/filter/      -- ScalableBloomFilter, PartitionedBloomFilter
- pkg/bloomgateway/bloomgateway.go  -- Gateway 구조체, FilterChunkRefs
- pkg/bloomgateway/worker.go        -- 워커 패턴
- pkg/bloomgateway/processor.go     -- 블룸 블록 처리
- pkg/bloomgateway/client.go        -- GatewayClient, JumpHash
- pkg/bloombuild/builder/builder.go -- Builder 구조체
- pkg/bloombuild/planner/planner.go -- Planner 구조체
```

### 블룸 필터의 쿼리 가속 원리

```
일반 쿼리 (블룸 필터 없이):
{app="foo"} |= "error"
    │
    ▼
┌───────────────────────────────────────┐
│ 인덱스 조회: {app="foo"} 매칭 청크   │
│ → 1000개 청크 식별                    │
└───────────┬───────────────────────────┘
            ▼
┌───────────────────────────────────────┐
│ Object Store에서 1000개 청크 다운로드 │  ← 높은 I/O 비용
│ → 각 청크에서 "error" 검색            │
│ → 50개 청크에만 "error" 존재          │
└───────────────────────────────────────┘

블룸 필터를 사용한 쿼리:
{app="foo"} |= "error"
    │
    ▼
┌───────────────────────────────────────┐
│ 인덱스 조회: {app="foo"} 매칭 청크   │
│ → 1000개 청크 식별                    │
└───────────┬───────────────────────────┘
            ▼
┌───────────────────────────────────────┐
│ Bloom Gateway: 블룸 필터로 사전 필터  │  ← 낮은 I/O 비용
│ → "error"가 확실히 없는 청크 제거     │
│ → ~60개 청크 남음 (50 실제 + ~10 FP)  │
└───────────┬───────────────────────────┘
            ▼
┌───────────────────────────────────────┐
│ Object Store에서 ~60개 청크만 다운로드│  ← 94% I/O 절감
│ → 50개 청크에서 결과 반환             │
└───────────────────────────────────────┘
```

---

## 2. 블룸 필터 기초 이론

### 확률적 자료구조

블룸 필터는 집합의 멤버십을 테스트하는 공간 효율적인 확률적 자료구조이다.

```
특성:
- 원소가 집합에 "확실히 없다"고 판단 → 100% 정확
- 원소가 집합에 "아마 있다"고 판단 → False Positive 가능
- False Negative 없음 (있는데 없다고 하지 않음)

┌────────────────────────────────────────────┐
│  Bloom Filter (비트 배열)                  │
│                                            │
│  인덱스: 0 1 2 3 4 5 6 7 8 9 ...          │
│  비트:   0 1 0 1 0 0 1 0 1 0 ...          │
│                                            │
│  "hello" 삽입:                             │
│  h1("hello") = 1  → bit[1] = 1            │
│  h2("hello") = 3  → bit[3] = 1            │
│  h3("hello") = 6  → bit[6] = 1            │
│                                            │
│  "hello" 조회:                             │
│  bit[1]=1, bit[3]=1, bit[6]=1 → "아마 있음"│
│                                            │
│  "world" 조회:                             │
│  h1("world") = 2  → bit[2]=0 → "확실히 없음"│
└────────────────────────────────────────────┘
```

### False Positive Rate (FPR)

```
FPR 공식:
  p = (1 - e^(-kn/m))^k

  m: 비트 배열 크기
  k: 해시 함수 개수
  n: 삽입된 원소 수

예시:
  m = 1024 bits, k = 7, n = 100
  → FPR ≈ 0.82%

Loki 기본 설정:
  fpRate = 0.01 (1%)
  → 100개 쿼리 중 ~1개가 불필요한 청크를 포함
```

### Scalable Bloom Filter

일반 블룸 필터는 삽입 원소 수를 미리 알아야 한다. ScalableBloomFilter는 필터가 가득 차면 새 필터를 기하급수적으로 감소하는 FPR로 추가하여 동적으로 확장한다.

```
ScalableBloomFilter 확장 패턴:

시간 →
Filter 0: fpRate=0.01, capacity=1024
          ├── 가득 찼음!
          ▼
Filter 1: fpRate=0.01*r, capacity=1024*s
          ├── 가득 찼음!
          ▼
Filter 2: fpRate=0.01*r^2, capacity=1024*s^2
          ├── ...
          ▼

r = tightening ratio (기본 0.8)
s = space growth factor (기본 4)

전체 FPR은 모든 필터의 FPR 합보다 작음:
  총 FPR ≤ fp * (1 + r + r^2 + ...) = fp / (1-r)
  = 0.01 / (1-0.8) = 0.05 (5%)
```

---

## 3. ScalableBloomFilter 구현

### 구조체 정의

```go
// 소스: pkg/storage/bloom/v1/filter/scalable.go:51-69
type ScalableBloomFilter struct {
    filters []*PartitionedBloomFilter   // 기하급수 감소 FPR 필터들
    r       float64                      // tightening ratio (기본 0.8)
    fp      float64                      // target false-positive rate
    p       float64                      // partition fill ratio
    hint    uint                         // 첫 필터 크기 힌트
    s       uint                         // space growth factor (2 또는 4)
    additionsSinceFillRatioCheck uint    // 필 비율 체크 최적화
}
```

### 생성자

```go
// 소스: pkg/storage/bloom/v1/filter/scalable.go:75-87
func NewScalableBloomFilter(hint uint, fpRate, r float64) *ScalableBloomFilter {
    s := &ScalableBloomFilter{
        filters: make([]*PartitionedBloomFilter, 0, 1),
        r:       r,
        fp:      fpRate,
        p:       fillRatio,
        hint:    hint,
        s:       4,
    }
    s.addFilter()
    return s
}
```

### Capacity 메서드

```go
// 소스: pkg/storage/bloom/v1/filter/scalable.go:91-97
func (s *ScalableBloomFilter) Capacity() uint {
    capacity := uint(0)
    for _, bf := range s.filters {
        capacity += bf.Capacity()
    }
    return capacity
}
```

### 필 비율 체크 최적화

```go
// 소스: pkg/storage/bloom/v1/filter/scalable.go (주석 60-68)
// additionsSinceFillRatioCheck는 마지막 필 비율 체크 이후 추가 수를 추적한다.
// 필 비율은 추가 수에 기반하여 추정되므로, 체크 비용을 분산시키기 위해 사용한다.
// 특히 중복 키를 많이 추가할 때, 실제 설정된 비트 수가 증가하지 않지만
// 추정 필 비율이 인위적으로 높아질 수 있다.
// 새 필터 추가 시 리셋된다.
```

---

## 4. Bloom 래퍼와 인코딩/디코딩

### Bloom 구조체

```go
// 소스: pkg/storage/bloom/v1/bloom.go:22-24
type Bloom struct {
    filter.ScalableBloomFilter
}

func NewBloom() *Bloom {
    return &Bloom{
        ScalableBloomFilter: *filter.NewScalableBloomFilter(1024, 0.01, 0.8),
    }
}
```

기본 설정:
- `hint = 1024`: 초기 필터 크기 힌트 (1024 비트)
- `fpRate = 0.01`: 1% false positive rate
- `r = 0.8`: tightening ratio

### 인코딩

```go
// 소스: pkg/storage/bloom/v1/bloom.go:33-48
func (b *Bloom) Encode(enc *encoding.Encbuf) error {
    buf := bytes.NewBuffer(make([]byte, 0, int(b.Capacity()/8)))
    _, err := b.WriteTo(buf)
    if err != nil {
        return errors.Wrap(err, "encoding bloom filter")
    }
    data := buf.Bytes()
    enc.PutUvarint(len(data))  // 블룸 필터 길이 (가변 길이 정수)
    enc.PutBytes(data)          // 블룸 필터 데이터
    return nil
}
```

### 디코딩

```go
// 소스: pkg/storage/bloom/v1/bloom.go:50-60
func (b *Bloom) Decode(dec *encoding.Decbuf) error {
    ln := dec.Uvarint()
    data := dec.Bytes(ln)
    _, err := b.DecodeFrom(data)
    if err != nil {
        return errors.Wrap(err, "decoding bloom filter")
    }
    return nil
}
```

---

## 5. 블룸 블록 구조

### BloomPageHeader

```go
// 소스: pkg/storage/bloom/v1/bloom.go:215-217
type BloomPageHeader struct {
    N, Offset, Len, DecompressedLen int
}
```

| 필드 | 설명 |
|------|------|
| `N` | 페이지 내 블룸 필터 수 |
| `Offset` | 블록 파일 내 페이지 시작 오프셋 |
| `Len` | 압축된 페이지 크기 (바이트) |
| `DecompressedLen` | 압축 해제된 페이지 크기 |

### BloomBlock

```go
// 소스: pkg/storage/bloom/v1/bloom.go:234-237
type BloomBlock struct {
    schema      Schema
    pageHeaders []BloomPageHeader
}
```

### 블록 파일 레이아웃

```
블룸 블록 파일 구조:
┌──────────────────────────────────────────────────┐
│  Schema (버전, 인코딩 정보)                       │
├──────────────────────────────────────────────────┤
│  Page 0 데이터                                    │
│  ┌─────────────────────────────────────────────┐ │
│  │  Bloom 0 (Uvarint 길이 + ScalableBloomFilter)│ │
│  │  Bloom 1                                     │ │
│  │  ...                                         │ │
│  │  Bloom N-1                                   │ │
│  │  [블룸 개수: 8바이트 BE64]                    │ │
│  └─────────────────────────────────────────────┘ │
│  [CRC32 체크섬: 4바이트]                          │
├──────────────────────────────────────────────────┤
│  Page 1 데이터                                    │
│  ...                                              │
├──────────────────────────────────────────────────┤
│  Page Headers (모든 페이지의 메타데이터)          │
│  ┌─────────────────────────────────────────────┐ │
│  │  [페이지 수: Uvarint]                        │ │
│  │  Header 0: N, Offset, Len, DecompressedLen  │ │
│  │  Header 1: N, Offset, Len, DecompressedLen  │ │
│  │  ...                                         │ │
│  └─────────────────────────────────────────────┘ │
│  [CRC32 체크섬: 4바이트]                          │
├──────────────────────────────────────────────────┤
│  [Headers 오프셋: 8바이트 BE64]                   │
│  [전체 체크섬: 4바이트 BE32]                      │
└──────────────────────────────────────────────────┘
```

### BloomPageDecoder

```go
// 소스: pkg/storage/bloom/v1/bloom.go:151-158
type BloomPageDecoder struct {
    data []byte
    dec  *encoding.Decbuf
    n    int     // 페이지 내 블룸 수
    cur  *Bloom  // 현재 블룸
    err  error
}
```

```go
// 소스: pkg/storage/bloom/v1/bloom.go:124-141
func NewBloomPageDecoder(data []byte) *BloomPageDecoder {
    // 마지막 8바이트: 블룸 수
    dec := encoding.DecWith(data[len(data)-8:])
    n := int(dec.Be64())
    // 블룸 데이터 영역
    data = data[:len(data)-8]
    dec.B = data
    return &BloomPageDecoder{
        dec:  &dec,
        data: data,
        n:    n,
    }
}
```

### 최대 페이지 크기 제한

```go
// 소스: pkg/storage/bloom/v1/bloom.go:20
var DefaultMaxPageSize = 64 << 20  // 64MB
```

일부 블룸 페이지는 400MB 이상으로 커질 수 있어 OOM을 유발한다. 기본 최대 페이지 크기를 64MB로 제한하고, 이를 초과하는 페이지는 건너뛴다.

```go
// 소스: pkg/storage/bloom/v1/bloom.go:285-299
func (b *BloomBlock) BloomPageDecoder(r io.ReadSeeker, alloc mempool.Allocator, pageIdx int, maxPageSize int, metrics *Metrics) (res *BloomPageDecoder, skip bool, err error) {
    page := b.pageHeaders[pageIdx]
    if page.Len > maxPageSize {
        metrics.pagesSkipped.WithLabelValues(pageTypeBloom, skipReasonTooLarge).Inc()
        return nil, true, nil  // 건너뛰기
    }
    // ...
}
```

### 메모리 풀 통합

```go
// 소스: pkg/storage/bloom/v1/bloom.go:62-97
func LazyDecodeBloomPage(r io.Reader, alloc mempool.Allocator, pool compression.ReaderPool, page BloomPageHeader) (*BloomPageDecoder, error) {
    data, err := alloc.Get(page.Len)       // 메모리 풀에서 할당
    defer alloc.Put(data)                   // 사용 후 반환
    // ...
    b, err := alloc.Get(page.DecompressedLen)  // 압축 해제용 버퍼
    // ...
}
```

메모리 풀(`mempool.Allocator`)을 사용하여 GC 압력을 줄인다. 블룸 페이지 디코딩에서 대량의 메모리 할당이 발생하므로 풀링이 중요하다.

---

## 6. Bloom Gateway

### Gateway 구조체

```go
// 소스: pkg/bloomgateway/bloomgateway.go:48-65
type Gateway struct {
    services.Service

    cfg     Config
    logger  log.Logger
    metrics *metrics

    queue       *queue.RequestQueue
    activeUsers *util.ActiveUsersCleanupService
    bloomStore  bloomshipper.Store

    pendingTasks *atomic.Int64

    serviceMngr    *services.Manager
    serviceWatcher *services.FailureWatcher

    workerConfig workerConfig
}
```

### 생성자

```go
// 소스: pkg/bloomgateway/bloomgateway.go:79-105
func New(cfg Config, store bloomshipper.Store, logger log.Logger, reg prometheus.Registerer) (*Gateway, error) {
    g := &Gateway{
        cfg:     cfg,
        logger:  logger,
        metrics: newMetrics(reg, constants.Loki, metricsSubsystem),
        workerConfig: workerConfig{
            maxItems:         cfg.NumMultiplexItems,
            queryConcurrency: cfg.BlockQueryConcurrency,
            async:            cfg.FetchBlocksAsync,
        },
        pendingTasks: &atomic.Int64{},
        bloomStore:   store,
    }

    queueMetrics := queue.NewMetrics(reg, constants.Loki, metricsSubsystem)
    g.queue = queue.NewRequestQueue(cfg.MaxOutstandingPerTenant, time.Minute, &fixedQueueLimits{0}, queueMetrics)
    g.activeUsers = util.NewActiveUsersCleanupWithDefaultValues(queueMetrics.Cleanup)

    g.initServices()
    g.Service = services.NewBasicService(g.starting, g.running, g.stopping).WithName("bloom-gateway")
    return g, nil
}
```

### 서비스 초기화

```go
// 소스: pkg/bloomgateway/bloomgateway.go:107-122
func (g *Gateway) initServices() error {
    svcs := []services.Service{g.queue, g.activeUsers}
    for i := 0; i < g.cfg.WorkerConcurrency; i++ {
        id := fmt.Sprintf("bloom-query-worker-%d", i)
        w := newWorker(id, g.workerConfig, g.queue, g.bloomStore, g.pendingTasks, g.logger, g.metrics.workerMetrics)
        svcs = append(svcs, w)
    }
    g.serviceMngr, _ = services.NewManager(svcs...)
    g.serviceWatcher = services.NewFailureWatcher()
    g.serviceWatcher.WatchManager(g.serviceMngr)
    return nil
}
```

### Gateway 아키텍처

```
┌──────────────────────────────────────────────────────────┐
│                    Bloom Gateway                          │
│                                                          │
│  ┌────────────────────────────────────────────────────┐  │
│  │              Request Queue                         │  │
│  │  (테넌트별 공정 큐, MaxOutstandingPerTenant)       │  │
│  └──────────────────────┬─────────────────────────────┘  │
│                         │                                │
│         ┌───────────────┼───────────────┐                │
│         │               │               │                │
│         ▼               ▼               ▼                │
│  ┌──────────┐   ┌──────────┐   ┌──────────┐            │
│  │ Worker 0 │   │ Worker 1 │   │ Worker 2 │  ...       │
│  │          │   │          │   │          │            │
│  │ Dequeue  │   │ Dequeue  │   │ Dequeue  │            │
│  │ → Process│   │ → Process│   │ → Process│            │
│  └────┬─────┘   └────┬─────┘   └────┬─────┘            │
│       │               │               │                 │
│       └───────────────┼───────────────┘                 │
│                       │                                 │
│                       ▼                                 │
│  ┌────────────────────────────────────────────────────┐ │
│  │              Bloom Store                           │ │
│  │  (Object Store에서 블룸 블록 다운로드/캐시)        │ │
│  └────────────────────────────────────────────────────┘ │
└──────────────────────────────────────────────────────────┘
```

---

## 7. Bloom Gateway Worker

### worker 구조체

```go
// 소스: pkg/bloomgateway/worker.go:33-43
type worker struct {
    services.Service
    id      string
    cfg     workerConfig
    queue   *queue.RequestQueue
    store   bloomshipper.Store
    pending *atomic.Int64
    logger  log.Logger
    metrics *workerMetrics
}
```

### workerConfig

```go
// 소스: pkg/bloomgateway/worker.go:22-26
type workerConfig struct {
    maxItems         int   // 한 번에 디큐할 최대 작업 수
    queryConcurrency int   // 블록 쿼리 동시성
    async            bool  // 비동기 블록 다운로드 여부
}
```

### 워커 실행 루프

```go
// 소스: pkg/bloomgateway/worker.go:65-123
func (w *worker) running(_ context.Context) error {
    idx := queue.StartIndexWithLocalQueue
    p := newProcessor(w.id, w.cfg.queryConcurrency, w.cfg.async, w.store, w.logger, w.metrics)

    for st := w.State(); st == services.Running || st == services.Stopping; {
        taskCtx := context.Background()
        // 1. 큐에서 작업 배치 디큐
        items, newIdx, err := w.queue.DequeueMany(taskCtx, idx, w.id, w.cfg.maxItems)
        idx = newIdx

        if len(items) == 0 {
            w.queue.ReleaseRequests(items)
            continue
        }

        // 2. 작업을 Task로 변환
        tasks := make([]Task, 0, len(items))
        for _, item := range items {
            task := item.(Task)
            _ = w.pending.Dec()
            tasks = append(tasks, task)
        }

        // 3. Processor로 작업 처리
        err = p.processTasks(taskCtx, tasks)

        // 4. 작업 반환
        w.queue.ReleaseRequests(items)
    }
    return nil
}
```

### 워커 등록/해제

```go
// 소스: pkg/bloomgateway/worker.go:59-63
func (w *worker) starting(_ context.Context) error {
    w.queue.RegisterConsumerConnection(w.id)
    return nil
}

func (w *worker) stopping(err error) error {
    w.queue.UnregisterConsumerConnection(w.id)
    return nil
}
```

---

## 8. Bloom Gateway Processor

### processor 구조체

```go
// 소스: pkg/bloomgateway/processor.go:29-36
type processor struct {
    id          string
    concurrency int    // 블록 처리 동시성
    async       bool   // 비동기 다운로드
    store       bloomshipper.Store
    logger      log.Logger
    metrics     *workerMetrics
}
```

### 처리 흐름

```go
// 소스: pkg/bloomgateway/processor.go:38-54
func (p *processor) processTasks(ctx context.Context, tasks []Task) error {
    tenant := tasks[0].tenant

    for ts, tasks := range group(tasks, func(t Task) config.DayTime { return t.table }) {
        err := p.processTasksForDay(ctx, tenant, ts, tasks)
        if err != nil {
            for _, task := range tasks {
                task.CloseWithError(err)
            }
            return err
        }
        for _, task := range tasks {
            task.Close()
        }
    }
    return nil
}
```

### 블록 처리 상세

```go
// 소스: pkg/bloomgateway/processor.go:56-103
func (p *processor) processTasksForDay(ctx context.Context, _ string, _ config.DayTime, tasks []Task) error {
    // 1. 모든 태스크의 블록 레퍼런스 수집
    blocksRefs := make([]bloomshipper.BlockRef, 0, len(tasks[0].blocks)*len(tasks))
    for _, task := range tasks {
        blocksRefs = append(blocksRefs, task.blocks...)
    }

    // 2. 블록-태스크 매핑 (같은 블록을 참조하는 태스크 그룹화)
    tasksByBlock := partitionTasksByBlock(tasks, blocksRefs)

    // 3. 블룸 블록 다운로드 (캐시 포함)
    bqs, err := p.store.FetchBlocks(ctx, refs,
        bloomshipper.WithFetchAsync(p.async),
        bloomshipper.WithIgnoreNotFound(true),
        bloomshipper.WithPool(p.store.Allocator()),
    )

    // 4. 블록별 블룸 필터 쿼리 실행
    res := p.processBlocks(ctx, bqs, tasksByBlock)
    return res
}
```

### processBlock: 블룸 필터 매칭

```go
// 소스: pkg/bloomgateway/processor.go:145-192
func (p *processor) processBlock(_ context.Context, bq *bloomshipper.CloseableBlockQuerier, tasks []Task) (err error) {
    blockQuerier := bq.BlockQuerier
    schema, _ := blockQuerier.Schema()

    // V3+ 스키마 필수
    if schema.Version() < v1.V3 {
        return v1.ErrUnsupportedSchemaVersion
    }

    // 태스크별 Request Iterator 생성
    iters := make([]iter.PeekIterator[v1.Request], 0, len(tasks))
    for _, task := range tasks {
        it := iter.NewPeekIter(task.RequestIter())
        iters = append(iters, it)
    }

    // Fuse: 여러 태스크의 요청을 블록에 대해 일괄 처리
    fq := blockQuerier.Fuse(iters, logger)
    err = fq.Run()
    return err
}
```

### 처리 흐름도

```
processTasks(tasks)
       │
       ▼
┌────────────────────────────┐
│ 태스크를 일(day)별 그룹화  │
└──────────┬─────────────────┘
           │
    for each day:
           │
           ▼
┌────────────────────────────┐
│ processTasksForDay()       │
│  ├─ 블록 레퍼런스 수집     │
│  ├─ 블록-태스크 매핑       │
│  ├─ FetchBlocks()          │  ← Object Store/캐시에서 다운로드
│  └─ processBlocks()        │
│     ├─ 동시성 제한 실행    │
│     └─ for each block:     │
│        └─ processBlock()   │
│           ├─ Fuse() 생성   │
│           └─ Fuse.Run()    │  ← 블룸 필터 매칭
└────────────────────────────┘
```

---

## 9. GatewayClient: DNS 기반 디스커버리

### GatewayClient 구조체

```go
// 소스: pkg/bloomgateway/client.go:116-122
type GatewayClient struct {
    cfg         ClientConfig
    logger      log.Logger
    metrics     *clientMetrics
    pool        clientPool
    dnsProvider *discovery.DNS
}
```

### ClientConfig

```go
// 소스: pkg/bloomgateway/client.go:62-72
type ClientConfig struct {
    PoolConfig       PoolConfig           `yaml:"pool_config,omitempty"`
    GRPCClientConfig grpcclient.Config    `yaml:"grpc_client_config"`
    Addresses        string               `yaml:"addresses,omitempty"`
}
```

### JumpHash 기반 로드밸런싱

```go
// 소스: pkg/bloomgateway/client.go:124-156
func NewClient(cfg ClientConfig, registerer prometheus.Registerer, logger log.Logger) (*GatewayClient, error) {
    // gRPC 연결 팩토리
    clientFactory := func(addr string) (ringclient.PoolClient, error) {
        pool, _ := NewBloomGatewayGRPCPool(addr, dialOpts)
        return pool, nil
    }

    // DNS 기반 서비스 디스커버리
    dnsProvider := discovery.NewDNS(logger, cfg.PoolConfig.CheckInterval, cfg.Addresses, nil)
    dnsProvider.RunOnce()  // 초기 DNS 조회

    // JumpHash 클라이언트 풀 생성
    pool, _ := NewJumpHashClientPool(clientFactory, dnsProvider, cfg.PoolConfig.CheckInterval, logger)

    return &GatewayClient{
        cfg:         cfg,
        pool:        pool,
        dnsProvider: dnsProvider,
    }, nil
}
```

### JumpHash 동작 원리

```
JumpHash 로드밸런싱:

Bloom Gateway 인스턴스:
  Instance 0: 10.0.0.1:9096
  Instance 1: 10.0.0.2:9096
  Instance 2: 10.0.0.3:9096

블록 레퍼런스 → JumpHash → 인스턴스

block_ref_1 → JumpHash(hash("block_ref_1"), 3) → Instance 2
block_ref_2 → JumpHash(hash("block_ref_2"), 3) → Instance 0
block_ref_3 → JumpHash(hash("block_ref_3"), 3) → Instance 1

특성:
- 동일한 블록은 항상 같은 인스턴스로 라우팅
- 인스턴스 추가/제거 시 최소한의 리밸런싱
- 캐시 효율성 극대화 (같은 블록이 같은 인스턴스에)
```

### PrefetchBloomBlocks 분배

```go
// 소스: pkg/bloomgateway/client.go:163-200
func (c *GatewayClient) PrefetchBloomBlocks(ctx context.Context, blocks []bloomshipper.BlockRef) error {
    // 블록을 서버별로 그룹화
    pos := make(map[string]int)
    servers := make([]addrWithBlocks, 0, len(blocks))
    for _, block := range blocks {
        addr, _ := c.pool.Addr(block.String())  // JumpHash로 서버 결정
        if idx, found := pos[addr]; found {
            servers[idx].blocks = append(servers[idx].blocks, block.String())
        } else {
            pos[addr] = len(servers)
            servers = append(servers, addrWithBlocks{addr: addr, blocks: []string{block.String()}})
        }
    }

    // 각 서버에 병렬로 프리페치 요청
    return concurrency.ForEachJob(ctx, len(servers), len(servers), func(ctx context.Context, i int) error {
        rs := servers[i]
        return c.doForAddrs([]string{rs.addr}, func(client logproto.BloomGatewayClient) error {
            req := &logproto.PrefetchBloomBlocksRequest{Blocks: rs.blocks}
            _, err := client.PrefetchBloomBlocks(ctx, req)
            return err
        })
    })
}
```

---

## 10. Bloom Builder

### Builder 구조체

```go
// 소스: pkg/bloombuild/builder/builder.go:42-61
type Builder struct {
    services.Service

    ID string

    cfg     Config
    limits  Limits
    metrics *Metrics
    logger  log.Logger

    bloomStore   bloomshipper.Store
    chunkLoader  ChunkLoader
    bloomGateway bloomgateway.Client

    client protos.PlannerForBuilderClient

    ringWatcher *common.RingWatcher
}
```

### Builder 라이프사이클

```go
// 소스: pkg/bloombuild/builder/builder.go:140-184
func (b *Builder) running(ctx context.Context) error {
    retries := backoff.New(ctx, b.cfg.BackoffConfig)
    for retries.Ongoing() {
        err := b.connectAndBuild(ctx)
        if err != nil {
            err = standardizeRPCError(err)

            if errors.Is(err, context.Canceled) && b.State() != services.Running {
                break  // 종료 시
            }

            if errors.Is(err, io.EOF) {
                retries.Reset()  // Planner 재연결
                continue
            }

            retries.Wait()  // 백오프 후 재시도
            continue
        }
        break
    }
    return retries.Err()
}
```

### Builder-Planner 통신

```
┌────────────────────────────────────────────────────┐
│                 Planner                             │
│  ┌──────────────────────────────────────────────┐  │
│  │           Tasks Queue                         │  │
│  │  Task 1: tenant-a, table-1234, series [...]  │  │
│  │  Task 2: tenant-b, table-1235, series [...]  │  │
│  └──────────────────┬───────────────────────────┘  │
│                     │                               │
│         gRPC 양방향 스트리밍                        │
│                     │                               │
└─────────────────────┼───────────────────────────────┘
                      │
         ┌────────────┼────────────┐
         │            │            │
         ▼            ▼            ▼
┌─────────────┐ ┌─────────────┐ ┌─────────────┐
│  Builder 1  │ │  Builder 2  │ │  Builder 3  │
│             │ │             │ │             │
│ 1. Task 수신│ │ 1. Task 수신│ │ 1. Task 수신│
│ 2. 청크 로드│ │ 2. 청크 로드│ │ 2. 청크 로드│
│ 3. 블룸 빌드│ │ 3. 블룸 빌드│ │ 3. 블룸 빌드│
│ 4. 블록 업로드││ 4. 블록 업로드││ 4. 블록 업로드│
│ 5. 결과 보고│ │ 5. 결과 보고│ │ 5. 결과 보고│
└─────────────┘ └─────────────┘ └─────────────┘
```

### 종료 시 Planner 통지

```go
// 소스: pkg/bloombuild/builder/builder.go:111-138
func (b *Builder) stopping(_ error) error {
    if b.client != nil {
        ctx, _ := user.InjectIntoGRPCRequest(user.InjectOrgID(context.Background(), "fake"))
        req := &protos.NotifyBuilderShutdownRequest{BuilderID: b.ID}
        b.client.NotifyBuilderShutdown(ctx, req)
    }
    return nil
}
```

Builder가 종료되면 Planner에 `NotifyBuilderShutdown`을 보내어 해당 Builder에 할당된 작업을 다른 Builder에 재분배할 수 있도록 한다.

---

## 11. Bloom Planner

### Planner 구조체

```go
// 소스: pkg/bloombuild/planner/planner.go:39-62
type Planner struct {
    services.Service
    subservices        *services.Manager
    subservicesWatcher *services.FailureWatcher
    retentionManager   *RetentionManager

    cfg       Config
    limits    Limits
    schemaCfg config.SchemaConfig

    tsdbStore  common.TSDBStore
    bloomStore bloomshipper.StoreBase

    tasksQueue  *queue.Queue
    planFactory *strategies.Factory

    metrics *Metrics
    logger  log.Logger

    ringWatcher *common.RingWatcher
}
```

### Planner 생성자

```go
// 소스: pkg/bloombuild/planner/planner.go:64-126
func New(cfg Config, limits Limits, schemaCfg config.SchemaConfig, ...) (*Planner, error) {
    // TSDB Store 생성 (시리즈 정보 조회용)
    tsdbStore, _ := common.NewTSDBStores("bloom-planner", schemaCfg, ...)

    // 작업 큐 생성
    tasksQueue, _ := queue.NewQueue(logger, cfg.Queue, queueLimits, queueMetrics, ...)

    p := &Planner{
        tsdbStore:   tsdbStore,
        bloomStore:  bloomStore,
        tasksQueue:  tasksQueue,
        planFactory: strategies.NewFactory(limits, strategies.NewMetrics(r), logger),
    }

    // 보존 매니저 생성
    p.retentionManager = NewRetentionManager(p.cfg.RetentionConfig, p.limits, p.bloomStore, ...)

    // 리더 감시 (SSD 모드에서)
    if rm != nil {
        p.ringWatcher = common.NewRingWatcher(rm.RingLifecycler.GetInstanceID(), rm.Ring, ...)
    }

    p.Service = services.NewBasicService(p.starting, p.running, p.stopping)
    return p, nil
}
```

### Planner 실행 루프

```go
// 소스: pkg/bloombuild/planner/planner.go:157-191
func (p *Planner) running(ctx context.Context) error {
    go p.trackInflightRequests(ctx)

    // 초기 1분 대기 (Ring 안정화)
    initialPlanningTimer := time.NewTimer(time.Minute)

    planningTicker := time.NewTicker(p.cfg.PlanningInterval)

    for {
        select {
        case <-ctx.Done():
            return nil
        case <-initialPlanningTimer.C:
            p.runOne(ctx)  // 초기 빌드
        case <-planningTicker.C:
            p.runOne(ctx)  // 주기적 빌드
        }
    }
}
```

### 리더 확인

```go
// 소스: pkg/bloombuild/planner/planner.go:128-135
func (p *Planner) isLeader() bool {
    if p.ringWatcher == nil {
        return true  // 마이크로서비스 모드: 싱글톤이므로 항상 리더
    }
    return p.ringWatcher.IsLeader()
}
```

### 작업 분배 전략

```
Planner의 작업 분배 흐름:

runOne(ctx)
    │
    ├─ isLeader() 확인
    │   └─ No → 스킵
    │
    ├─ 각 스키마 기간에 대해:
    │   ├─ TSDB에서 시리즈 목록 조회
    │   ├─ 기존 블룸 블록 메타데이터 조회
    │   ├─ 빌드가 필요한 시리즈 식별
    │   │   (새 시리즈 또는 변경된 시리즈)
    │   │
    │   ├─ 전략 팩토리로 작업 생성
    │   │   ├─ SplitKeyspace: 키 공간 분할
    │   │   └─ ChunkSize: 청크 크기 기반
    │   │
    │   └─ tasksQueue에 작업 추가
    │
    └─ 보존 관리자 실행
        └─ 오래된 블룸 블록 정리
```

---

## 12. 쿼리 가속: FilterChunkRefs 흐름

### FilterChunkRefs 전체 흐름

```go
// 소스: pkg/bloomgateway/bloomgateway.go:205-354
func (g *Gateway) FilterChunkRefs(ctx context.Context, req *logproto.FilterChunkRefRequest) (*logproto.FilterChunkRefResponse, error) {
    tenantID, _ := tenant.TenantID(ctx)

    // 1. 필터 매처 추출
    matchers := v1.ExtractTestableLabelMatchers(req.Plan.AST)
    if len(matchers) == 0 {
        return &logproto.FilterChunkRefResponse{ChunkRefs: req.Refs}, nil  // 필터 없음 → 전체 반환
    }

    // 2. 블록 키 디코딩
    blocks, _ := decodeBlockKeys(req.Blocks)
    if len(blocks) == 0 {
        return &logproto.FilterChunkRefResponse{ChunkRefs: req.Refs}, nil
    }

    // 3. 시리즈를 일(day)별로 파티셔닝
    seriesByDay := partitionRequest(req)

    // 4. Task 생성
    series := seriesByDay[0]
    task := newTask(ctx, tenantID, series, matchers, blocks)
    task.responses = responsesPool.Get(len(series.series))
    defer responsesPool.Put(task.responses)

    // 5. 큐에 태스크 삽입
    task.enqueueTime = time.Now()
    g.queue.Enqueue(tenantID, nil, task, func() {
        _ = g.pendingTasks.Inc()
    })

    // 6. 결과 소비 고루틴 시작
    go g.consumeTask(ctx, task, tasksCh)

    // 7. 결과 대기
    select {
    case <-ctx.Done():
        return nil, ctx.Err()
    case task = <-tasksCh:
        if task.Err() != nil {
            return nil, task.Err()
        }
    }

    // 8. 결과 필터링 (제거 대상 청크 삭제)
    filtered := filterChunkRefs(req, task.responses)

    return &logproto.FilterChunkRefResponse{ChunkRefs: filtered}, nil
}
```

### filterChunkRefs 함수

```go
// 소스: pkg/bloomgateway/bloomgateway.go:388-480
func filterChunkRefs(req *logproto.FilterChunkRefRequest, responses []v1.Output) []*logproto.GroupedChunkRefs {
    // 1. 응답을 fingerprint로 정렬
    sort.Slice(responses, func(i, j int) bool { return responses[i].Fp < responses[j].Fp })

    // 2. 같은 시리즈의 결과 중복 제거 (DedupingIter)
    dedupedResps := iter.NewDedupingIter(
        func(o1, o2 v1.Output) bool { return o1.Fp == o2.Fp },
        iter.Identity[v1.Output],
        // 같은 시리즈의 제거 목록 병합 (정렬된 머지)
        func(o1, o2 v1.Output) v1.Output { /* merge removals */ },
        iter.NewPeekIter(iter.NewSliceIter(responses)),
    )

    // 3. 원본 요청에서 제거 대상 삭제
    res := make([]*logproto.GroupedChunkRefs, 0, len(req.Refs))
    for i := 0; i < len(req.Refs); i++ {
        cur := req.Refs[i]
        if !next || cur.Fingerprint < uint64(at.Fp) {
            res = append(res, cur)  // 제거 대상 아님
            continue
        }
        // 제거 대상 적용
        filterChunkRefsForSeries(cur, at.Removals)
        if len(cur.Refs) > 0 {
            res = append(res, cur)
        }
    }
    return res
}
```

### FilterChunkRefs 흐름도

```
FilterChunkRefs 요청
       │
       ▼
┌─────────────────────────────┐
│ 1. 테넌트 ID 추출           │
│ 2. 필터 매처 추출            │
│    (|= "error", |~ "warn.*")│
│ 3. 블록 레퍼런스 디코딩      │
└──────────────┬──────────────┘
               ▼
┌─────────────────────────────┐
│ 4. 시리즈/블록 정보로        │
│    Task 생성                 │
│ 5. RequestQueue에 삽입       │
└──────────────┬──────────────┘
               ▼
┌─────────────────────────────┐
│ 6. Worker가 Task 디큐       │
│ 7. Processor가 블록 처리     │
│    ├─ 블룸 블록 다운로드      │
│    ├─ 블룸 필터 매칭          │
│    └─ 제거 대상 청크 식별     │
└──────────────┬──────────────┘
               ▼
┌─────────────────────────────┐
│ 8. consumeTask()가 결과 수집 │
│ 9. filterChunkRefs()로       │
│    제거 대상 청크 삭제        │
│ 10. 필터링된 청크 반환       │
└─────────────────────────────┘
```

---

## 13. PrefetchBloomBlocks: 사전 다운로드

### Gateway 측 구현

```go
// 소스: pkg/bloomgateway/bloomgateway.go:167-202
func (g *Gateway) PrefetchBloomBlocks(_ context.Context, req *logproto.PrefetchBloomBlocksRequest) (*logproto.PrefetchBloomBlocksResponse, error) {
    refs, _ := decodeBlockKeys(req.Blocks)

    bqs, err := g.bloomStore.FetchBlocks(
        context.Background(),  // 핸들러 컨텍스트와 분리
        refs,
        bloomshipper.WithFetchAsync(true),
        bloomshipper.WithIgnoreNotFound(true),
        bloomshipper.WithCacheGetOptions(
            bloomshipper.WithSkipHitMissMetrics(true),
        ),
    )

    // 이미 다운로드된 블록은 즉시 닫음
    for _, bq := range bqs {
        if bq != nil {
            bq.Close()
        }
    }

    return &logproto.PrefetchBloomBlocksResponse{}, nil
}
```

### 프리페치 전략

```
쿼리 실행 전 프리페치:

1. Query Frontend가 쿼리 계획 수립
2. 필요한 블룸 블록 식별
3. GatewayClient.PrefetchBloomBlocks() 호출
   ├─ JumpHash로 블록별 Gateway 결정
   ├─ Gateway별로 블록 그룹화
   └─ 각 Gateway에 병렬 프리페치 요청

4. Gateway가 Object Store에서 비동기 다운로드
   └─ 캐시에 저장 (HitMiss 메트릭 건너뜀)

5. 실제 FilterChunkRefs 호출 시 → 캐시 히트!

┌──────────────────────────────────────────┐
│  시간축                                   │
│  ├─ t0: 프리페치 요청                     │
│  ├─ t1: 블록 다운로드 시작 (비동기)       │
│  ├─ t2: FilterChunkRefs 호출              │
│  ├─ t3: 블록이 이미 캐시에 있음!          │
│  └─ t4: 즉시 블룸 필터 매칭              │
└──────────────────────────────────────────┘
```

---

## 14. 설계 결정 분석

### 왜 별도 서비스(Bloom Gateway)인가?

**문제**: 블룸 블록은 Object Store에 저장되며, 각 블록이 수십~수백 MB로 크다. Querier에서 직접 블룸 필터를 조회하면 메모리와 I/O 부하가 크다.

**설계 결정**: Bloom Gateway를 독립 서비스로 분리
1. **캐시 효율**: JumpHash로 같은 블록이 같은 Gateway에 라우팅되어 캐시 히트율이 높다.
2. **메모리 관리**: Gateway에서 메모리 풀(`mempool.Allocator`)을 사용하여 GC 압력을 줄인다.
3. **수평 확장**: Gateway 인스턴스를 독립적으로 확장할 수 있다.
4. **장애 격리**: Gateway 장애가 Querier에 영향을 주지 않으며, 블룸 필터 없이도 쿼리가 동작한다.

### 왜 ScalableBloomFilter인가?

**문제**: 각 청크의 로그 라인 수가 미리 알려지지 않으므로 고정 크기 블룸 필터는 적합하지 않다.

**설계 결정**: ScalableBloomFilter 사용
- 기하급수 감소 FPR(`r=0.8`)으로 새 필터를 추가하여 전체 FPR을 제어
- 공간 성장 팩터(`s=4`)로 적절한 메모리 사용량 유지
- 초기 힌트(`hint=1024`)로 불필요한 재할당 최소화

### 왜 Builder와 Planner를 분리하는가?

**문제**: 블룸 블록 빌드는 CPU와 I/O 집약적이며, 작업 계획(어떤 시리즈를 빌드할지)과 실제 빌드를 같은 프로세스에서 하면 확장이 어렵다.

**설계 결정**:
- **Planner**: 싱글톤으로 작업 계획 수립 (TSDB 스캔, 기존 블룸 메타 비교)
- **Builder**: 다수 인스턴스로 실제 블룸 빌드 수행 (청크 읽기, 블룸 생성, 업로드)

```
확장 모델:
  Planner (1개) → 작업 큐 → Builder (N개, 수평 확장)
```

### 페이지 크기 제한의 이유

```go
var DefaultMaxPageSize = 64 << 20  // 64MB
```

**문제**: 일부 블룸 페이지가 400MB 이상으로 커질 수 있어 Gateway의 OOM을 유발한다.

**설계 결정**: 최대 페이지 크기를 64MB로 제한하고, 초과 페이지는 `skip=true`로 건너뛴다. 건너뛴 블록의 청크는 블룸 필터 없이 통과시키므로 안전하지만, 해당 청크의 쿼리 가속 효과는 없다.

### 메모리 풀 사용 이유

```go
// Relinquish는 메모리를 풀에 반환한다.
// 읽기 작업(bloom-gw)에서만 안전하게 사용 가능.
func (d *BloomPageDecoder) Relinquish(alloc mempool.Allocator) {
    data := d.data
    d.data = nil
    d.Reset()
    if cap(data) > 0 {
        _ = alloc.Put(data)
    }
}
```

블룸 페이지 디코딩에서 대량의 바이트 슬라이스가 할당되고 해제된다. 이를 풀링하여:
1. GC 압력 감소
2. 메모리 할당/해제 오버헤드 감소
3. 메모리 사용량 예측 가능

---

## 요약

Loki의 블룸 필터 서브시스템은 다음 핵심 원칙으로 설계되었다:

1. **확률적 사전 필터링**: ScalableBloomFilter로 쿼리 시 불필요한 청크를 제거하여 I/O를 최대 90% 이상 절감한다.
2. **독립 서비스 분리**: Bloom Gateway, Builder, Planner를 독립 서비스로 분리하여 각각 수평 확장과 장애 격리를 달성한다.
3. **캐시 친화적 라우팅**: JumpHash로 동일 블록이 동일 Gateway에 라우팅되어 캐시 히트율을 극대화한다.
4. **메모리 효율성**: 메모리 풀, 페이지 크기 제한, 비동기 프리페치로 메모리 사용을 최적화한다.
5. **안전한 폴백**: 블룸 필터가 없거나 페이지가 너무 크면 필터링 없이 전체 청크를 통과시켜 정확성을 보장한다.
