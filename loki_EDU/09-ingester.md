# 09. Ingester Deep-Dive

## 목차

1. [Ingester 개요](#1-ingester-개요)
2. [Ingester 구조체](#2-ingester-구조체)
3. [라이프사이클](#3-라이프사이클)
4. [instance: 테넌트별 인스턴스 관리](#4-instance-테넌트별-인스턴스-관리)
5. [stream: 스트림 관리](#5-stream-스트림-관리)
6. [플러시](#6-플러시)
7. [WAL (Write-Ahead Log)](#7-wal-write-ahead-log)
8. [복구 (Recovery)](#8-복구-recovery)
9. [역인덱스 (Inverted Index)](#9-역인덱스-inverted-index)
10. [소유권 관리](#10-소유권-관리)
11. [테일링 (Tailing)](#11-테일링-tailing)

---

## 1. Ingester 개요

Ingester는 Loki의 쓰기 경로에서 핵심 역할을 담당한다. Distributor로부터 수신한 로그 데이터를 인메모리에 보관하며, 주기적으로 영속 스토리지(Object Store)에 플러시한다. 동시에 읽기 경로에서도 아직 플러시되지 않은 최신 데이터를 쿼리할 수 있게 한다.

### 핵심 역할

```
Distributor                                 Querier
    │                                          │
    │ Push (gRPC)                               │ Query (gRPC)
    ▼                                          ▼
┌────────────────────────────────────────────────────┐
│                     Ingester                        │
│                                                     │
│  ┌───────────────────────────────────────────┐      │
│  │               인메모리 저장                   │      │
│  │                                            │      │
│  │  instance(tenant-1)                        │      │
│  │    ├── stream A → chunk1, chunk2           │      │
│  │    └── stream B → chunk1                   │      │
│  │                                            │      │
│  │  instance(tenant-2)                        │      │
│  │    └── stream C → chunk1, chunk2, chunk3   │      │
│  │                                            │      │
│  └─────────────┬─────────────────────────────┘      │
│                │                                     │
│                │ 플러시                               │
│                ▼                                     │
│  ┌─────────────────────────┐                        │
│  │     Object Store         │                        │
│  │  (S3/GCS/Azure/FS)      │                        │
│  └─────────────────────────┘                        │
│                                                     │
│  ┌─────────────────────────┐                        │
│  │        WAL               │ (디스크 기반 내구성)      │
│  └─────────────────────────┘                        │
└────────────────────────────────────────────────────┘
```

---

## 2. Ingester 구조체

파일: `pkg/ingester/ingester.go`

### 2.1 Config

```go
type Config struct {
    LifecyclerConfig ring.LifecyclerConfig `yaml:"lifecycler,omitempty"`

    ConcurrentFlushes   int               `yaml:"concurrent_flushes"`
    FlushCheckPeriod    time.Duration     `yaml:"flush_check_period"`
    FlushOpBackoff      backoff.Config    `yaml:"flush_op_backoff"`
    FlushOpTimeout      time.Duration     `yaml:"flush_op_timeout"`
    RetainPeriod        time.Duration     `yaml:"chunk_retain_period"`
    MaxChunkIdle        time.Duration     `yaml:"chunk_idle_period"`
    BlockSize           int               `yaml:"chunk_block_size"`
    TargetChunkSize     int               `yaml:"chunk_target_size"`
    ChunkEncoding       string            `yaml:"chunk_encoding"`
    MaxChunkAge         time.Duration     `yaml:"max_chunk_age"`
    AutoForgetUnhealthy bool              `yaml:"autoforget_unhealthy"`

    SyncPeriod         time.Duration `yaml:"sync_period"`
    SyncMinUtilization float64       `yaml:"sync_min_utilization"`

    MaxReturnedErrors int `yaml:"max_returned_stream_errors"`

    QueryStoreMaxLookBackPeriod time.Duration `yaml:"query_store_max_look_back_period"`

    WAL WALConfig `yaml:"wal,omitempty"`

    IndexShards int `yaml:"index_shards"`

    MaxDroppedStreams int `yaml:"max_dropped_streams"`

    OwnedStreamsCheckInterval time.Duration `yaml:"owned_streams_check_interval"`

    KafkaIngestion KafkaIngestionConfig `yaml:"kafka_ingestion,omitempty"`
}
```

주요 설정 기본값:

| 설정 | 기본값 | 설명 |
|------|--------|------|
| `ConcurrentFlushes` | 32 | 동시 플러시 수 |
| `FlushCheckPeriod` | 30초 | 플러시 체크 주기 |
| `FlushOpTimeout` | 10분 | 개별 플러시 타임아웃 |
| `RetainPeriod` | 0 | 플러시 후 메모리 보관 기간 |
| `MaxChunkIdle` | 30분 | 청크 유휴 시간 |
| `BlockSize` | 256KB | 미압축 블록 크기 |
| `TargetChunkSize` | 1.5MB | 목표 압축 청크 크기 |
| `ChunkEncoding` | gzip | 압축 코덱 |
| `MaxChunkAge` | 2시간 | 최대 청크 수명 |
| `SyncPeriod` | 1시간 | 동기화 주기 |
| `SyncMinUtilization` | 0.1 | 동기화 최소 사용률 |
| `IndexShards` | 32 | 역인덱스 샤드 수 |
| `OwnedStreamsCheckInterval` | 30초 | 소유 스트림 체크 주기 |

### 2.2 Ingester 구조체

```go
type Ingester struct {
    services.Service

    cfg    Config
    logger log.Logger

    clientConfig  client.Config
    tenantConfigs *runtime.TenantConfigs

    shutdownMtx  sync.Mutex
    instancesMtx sync.RWMutex
    instances    map[string]*instance  // tenantID → instance
    readonly     bool

    lifecycler        *ring.Lifecycler
    lifecyclerWatcher *services.FailureWatcher

    store           Store
    periodicConfigs []config.PeriodConfig

    loopDone    sync.WaitGroup
    loopQuit    chan struct{}
    tailersQuit chan struct{}

    // 플러시 큐 (ConcurrentFlushes 개)
    flushQueues     []*util.PriorityQueue
    flushQueuesDone sync.WaitGroup
    flushRateLimiter *rate.Limiter

    limiter *Limiter

    tenantsRetention *retention.TenantsRetention

    flushOnShutdownSwitch *OnceSwitch
    terminateOnShutdown   bool

    replayController *replayController

    metrics *ingesterMetrics

    wal WAL

    chunkFilter      chunk.RequestChunkFilterer
    extractorWrapper lokilog.SampleExtractorWrapper
    pipelineWrapper  lokilog.PipelineWrapper

    streamRateCalculator *StreamRateCalculator
    writeLogManager      *writefailures.Manager
    customStreamsTracker  push.UsageTracker

    readRing                ring.ReadRing
    recalculateOwnedStreams *recalculateOwnedStreamsSvc

    // Kafka 관련
    ingestPartitionID       int32
    partitionRingLifecycler *ring.PartitionInstanceLifecycler
    partitionReader         *partition.ReaderService
}
```

### 2.3 Interface

```go
type Interface interface {
    services.Service

    logproto.PusherServer       // Push RPC
    logproto.QuerierServer      // Query RPC
    logproto.StreamDataServer   // 스트림 데이터 RPC

    CheckReady(ctx context.Context) error
    FlushHandler(w http.ResponseWriter, _ *http.Request)
    GetOrCreateInstance(instanceID string) (*instance, error)
    ShutdownHandler(w http.ResponseWriter, r *http.Request)
    PrepareShutdown(w http.ResponseWriter, r *http.Request)
    PreparePartitionDownscaleHandler(w http.ResponseWriter, r *http.Request)
}
```

---

## 3. 라이프사이클

### 3.1 starting()

파일: `pkg/ingester/ingester.go` (line 526)

```go
func (i *Ingester) starting(ctx context.Context) (err error) {
    defer func() {
        if err != nil {
            _ = services.StopAndAwaitTerminated(context.Background(), i.lifecycler)
        }
    }()

    // [1] WAL 복구
    if i.cfg.WAL.Enabled {
        // RetainPeriod 비활성화 (복구 중)
        oldRetain := i.cfg.RetainPeriod
        i.cfg.RetainPeriod = 0

        // 스트림 리밋 비활성화 (복구 중)
        i.limiter.DisableForWALReplay()

        recoverer := newIngesterRecoverer(i)

        // [1a] 체크포인트 복구
        checkpointReader, checkpointCloser, err := newCheckpointReader(i.cfg.WAL.Dir, i.logger)
        checkpointRecoveryErr := RecoverCheckpoint(checkpointReader, recoverer)

        // [1b] WAL 세그먼트 복구
        segmentReader, segmentCloser, err := wal.NewWalReader(i.cfg.WAL.Dir, -1)
        segmentRecoveryErr := RecoverWAL(ctx, segmentReader, recoverer)

        // [1c] 복구 완료
        recoverer.Close()
        i.cfg.RetainPeriod = oldRetain

        // [1d] WAL 시작
        i.wal.Start()
    }

    // [2] 플러시 큐 초기화
    i.InitFlushQueues()

    // [3] 셧다운 마커 확인
    shutdownMarker, err := shutdownMarkerExists(shutdownMarkerPath)
    if shutdownMarker {
        i.setPrepareShutdown()
    }

    // [4] 플러시 루프 시작
    i.loopDone.Add(1)
    go i.loop()

    // [5] Kafka 파티션 리더 시작 (선택적)
    if i.partitionReader != nil {
        services.StartAndAwaitRunning(ctx, i.partitionReader)
    }

    // [6] Lifecycler 시작 → Ring 등록
    i.lifecycler.StartAsync(context.Background())
    i.lifecycler.AwaitRunning(ctx)

    // [7] 소유 스트림 재계산 서비스 시작
    i.recalculateOwnedStreams.StartAsync(ctx)
    i.recalculateOwnedStreams.AwaitRunning(ctx)

    // [8] 파티션 Ring Lifecycler 시작 (선택적)
    if i.partitionRingLifecycler != nil {
        services.StartAndAwaitRunning(ctx, i.partitionRingLifecycler)
    }

    return nil
}
```

### 3.2 running()

```go
func (i *Ingester) running(ctx context.Context) error {
    var serviceError error
    select {
    case <-ctx.Done():
        return nil
    case err := <-i.lifecyclerWatcher.Chan():
        serviceError = fmt.Errorf("lifecycler failed: %w", err)
    }
    return serviceError
}
```

### 3.3 stopping()

```
stopping() 호출 시:
    1. 셧다운 마커 삭제
    2. Kafka 파티션 리더 중지
    3. 파티션 Ring Lifecycler 중지
    4. 테일러 종료
    5. 소유 스트림 재계산 서비스 중지
    6. WAL 플러시 결정
    7. Lifecycler 중지
    8. 플러시 루프 종료
    9. WAL 중지
```

### 3.4 라이프사이클 상태 전이

```
┌──────────┐
│ starting │
│          │
│ WAL 복구  │
│ 플러시 큐  │
│ Ring 등록  │
└────┬─────┘
     │
     ▼
┌──────────┐
│ running  │
│          │
│ Push 수신 │
│ Query 응답│
│ 플러시 루프│
│ WAL 기록  │
└────┬─────┘
     │ ctx.Done() 또는 에러
     ▼
┌──────────┐
│ stopping │
│          │
│ 테일러 종료│
│ 플러시     │
│ Ring 해제  │
│ WAL 중지   │
└──────────┘
```

---

## 4. instance: 테넌트별 인스턴스 관리

파일: `pkg/ingester/instance.go`

### 4.1 instance 구조체

```go
type instance struct {
    cfg *Config

    buf     []byte           // 핑거프린트 계산용 버퍼
    streams *streamsMap       // 스트림 맵

    index  *index.Multi      // 역인덱스
    mapper *FpMapper          // 핑거프린트 매퍼

    instanceID string         // 테넌트 ID

    streamsCreatedTotal prometheus.Counter
    streamsRemovedTotal prometheus.Counter

    tailers   map[uint32]*tailer   // 활성 테일러
    tailerMtx sync.RWMutex

    limiter            *Limiter
    streamCountLimiter *streamCountLimiter
    ownedStreamsSvc    *ownedStreamService

    configs *runtime.TenantConfigs

    wal WAL

    flushOnShutdownSwitch *OnceSwitch

    metrics *ingesterMetrics

    chunkFilter          chunk.RequestChunkFilterer
    pipelineWrapper      log.PipelineWrapper
    extractorWrapper     log.SampleExtractorWrapper
    streamRateCalculator *StreamRateCalculator

    writeFailures *writefailures.Manager

    schemaconfig *config.SchemaConfig

    customStreamsTracker push.UsageTracker
    tenantsRetention    *retention.TenantsRetention
}
```

### 4.2 streamsMap

스트림 맵은 핑거프린트(model.Fingerprint)를 키로 사용하여 스트림에 접근한다:

```go
type streamsMap struct {
    // 핑거프린트 → *stream 매핑
    // 락-프리 읽기 지원
}
```

주요 연산:
- `LoadByFP(fp)`: 핑거프린트로 스트림 조회
- `Store(fp, stream)`: 새 스트림 저장
- `Delete(fp)`: 스트림 삭제
- `ForEach(fn)`: 모든 스트림 순회

### 4.3 스트림 생성 흐름

```
Push(stream) 수신
       │
       ▼
getOrCreateInstance(tenantID)
       │
       ├── 기존 instance 있음 → 반환
       │
       └── 새 instance 생성
            │
            ▼
     getOrCreateStream(stream)
            │
            ├── 기존 stream 있음 → 반환
            │
            └── 새 stream 생성
                  │
                  ├── 스트림 수 리밋 체크
                  ├── 핑거프린트 계산
                  ├── 역인덱스에 추가
                  ├── WAL 기록
                  └── 메트릭 업데이트
```

---

## 5. stream: 스트림 관리

파일: `pkg/ingester/stream.go`

### 5.1 stream 구조체

```go
type stream struct {
    limiter *StreamRateLimiter
    cfg     *Config
    tenant  string

    // 청크 목록 (최신 청크가 마지막)
    chunks   []chunkDesc
    fp       model.Fingerprint
    chunkMtx sync.RWMutex

    labels           labels.Labels
    labelsString     string
    labelHash        uint64
    labelHashNoShard uint64

    // 중복 감지용
    lastLine line

    // 비순서 쓰기 지원
    highestTs time.Time

    metrics *ingesterMetrics

    tailers   map[uint32]*tailer
    tailerMtx sync.RWMutex

    entryCt int64    // WAL 복구 시 사용되는 카운터

    unorderedWrites      bool
    streamRateCalculator *StreamRateCalculator
    writeFailures        *writefailures.Manager

    chunkFormat          byte
    chunkHeadBlockFormat chunkenc.HeadBlockFmt

    configs *runtime.TenantConfigs

    retentionHours string
    policy         string
}
```

### 5.2 chunkDesc

```go
type chunkDesc struct {
    chunk   *chunkenc.MemChunk
    closed  bool         // 더 이상 쓰기 불가
    synced  bool         // 동기화 지점에 도달
    flushed time.Time    // 플러시 완료 시간
    reason  string       // 플러시 이유

    lastUpdated time.Time
}
```

### 5.3 line (중복 감지)

```go
type line struct {
    ts      time.Time
    content string
}
```

### 5.4 Push 흐름

스트림의 `Push()` 메서드는 로그 엔트리를 청크에 추가한다:

```
stream.Push(entries)
       │
       ▼
   ┌──────────────────────────┐
   │ 각 엔트리에 대해:          │
   │                           │
   │ 1. 중복 감지               │
   │    - lastLine과 비교       │
   │    - 동일 ts + content → skip│
   │                           │
   │ 2. 순서 검증               │
   │    - unorderedWrites가     │
   │      false면 순서 강제      │
   │    - true면 비순서 허용     │
   │                           │
   │ 3. 레이트 리밋 체크         │
   │                           │
   │ 4. 현재 청크에 추가         │
   │    - head block이 가득 →   │
   │      블록 컷 및 압축        │
   │    - 청크가 가득 →          │
   │      새 청크 생성           │
   │                           │
   │ 5. lastLine, highestTs 갱신│
   │                           │
   │ 6. WAL 기록                │
   │                           │
   │ 7. 테일러 브로드캐스트       │
   │                           │
   │ 8. entryCt 증분            │
   └──────────────────────────┘
```

### 5.5 청크 로테이션 조건

새 청크가 생성되는 조건:

| 조건 | 설명 | 설정 |
|------|------|------|
| 블록 크기 초과 | head block이 BlockSize를 초과 | `chunk_block_size` |
| 목표 크기 도달 | 압축 크기가 TargetChunkSize에 도달 | `chunk_target_size` |
| 최대 블록 수 | 10개 블록에 도달 | `blocksPerChunk=10` |
| 동기화 지점 | SyncPeriod 경계에서 사용률 체크 | `sync_period`, `sync_min_utilization` |

### 5.6 비순서 쓰기 (Out-of-Order Writes)

비순서 쓰기가 활성화되면, 타임스탬프 순서가 맞지 않는 엔트리도 수락된다:

```
순서 쓰기 모드:
    t1 → t2 → t3 → t2 (거부!)

비순서 쓰기 모드:
    t1 → t2 → t3 → t2 (수락)
    └── UnorderedHeadBlockFmt 사용
    └── 내부적으로 정렬하여 저장
```

비순서 쓰기의 유효 범위는 `highestTs`를 기준으로 제한된다. 너무 오래된 엔트리는 여전히 거부될 수 있다.

---

## 6. 플러시

파일: `pkg/ingester/flush.go`

### 6.1 플러시 시스템 개요

```
┌─────────────────────────────────────────────────┐
│                  Ingester                         │
│                                                   │
│  loop() ─── FlushCheckPeriod(30s) ──► sweepUsers()│
│                                          │        │
│                                          ▼        │
│                                    sweepInstance() │
│                                          │        │
│                                          ▼        │
│                                    sweepStream()  │
│                                          │        │
│                              shouldFlushChunk()   │
│                                          │        │
│                                          ▼        │
│                              flushQueues[N]에 삽입 │
│                                    │              │
│                           ┌────────┼────────┐    │
│                           ▼        ▼        ▼    │
│                       flushLoop  flushLoop  ...  │
│                        (0)       (1)      (N-1)  │
│                           │                      │
│                           ▼                      │
│                       flushOp()                  │
│                           │                      │
│                           ▼                      │
│                   flushUserSeries()              │
│                           │                      │
│                           ▼                      │
│                     flushChunks()                │
│                           │                      │
│                           ▼                      │
│                    Object Store에 쓰기             │
└─────────────────────────────────────────────────┘
```

### 6.2 플러시 이유

```go
const (
    flushReasonIdle     = "idle"       // chunk_idle_period 초과
    flushReasonMaxAge   = "max_age"    // max_chunk_age 초과
    flushReasonForced   = "forced"     // 수동 또는 셧다운 시
    flushReasonNotOwned = "not_owned"  // 소유하지 않은 스트림
    flushReasonFull     = "full"       // 청크가 꽉 참
    flushReasonSynced   = "synced"     // 동기화 지점 도달
)
```

### 6.3 shouldFlushChunk()

```go
func (i *Ingester) shouldFlushChunk(chunk *chunkDesc) (bool, string) {
    // 1. 이미 닫힌 청크
    if chunk.closed {
        if chunk.synced {
            return true, flushReasonSynced
        }
        return true, flushReasonFull
    }

    // 2. 유휴 시간 초과
    if time.Since(chunk.lastUpdated) > i.cfg.MaxChunkIdle {
        return true, flushReasonIdle
    }

    // 3. 최대 수명 초과
    if from, to := chunk.chunk.Bounds(); to.Sub(from) > i.cfg.MaxChunkAge {
        return true, flushReasonMaxAge
    }

    return false, ""
}
```

### 6.4 sweepUsers()

```go
func (i *Ingester) sweepUsers(immediate, mayRemoveStreams bool) {
    instances := i.getInstances()

    for _, instance := range instances {
        i.sweepInstance(instance, immediate, mayRemoveStreams)
    }
    i.setFlushRate()
}
```

### 6.5 sweepStream()

```go
func (i *Ingester) sweepStream(instance *instance, stream *stream, immediate bool) {
    stream.chunkMtx.RLock()
    defer stream.chunkMtx.RUnlock()

    if len(stream.chunks) == 0 {
        return
    }

    lastChunk := stream.chunks[len(stream.chunks)-1]
    shouldFlush, _ := i.shouldFlushChunk(&lastChunk)

    // 청크가 1개뿐이고, 즉시 플러시가 아니고, 플러시 조건에 해당하지 않고,
    // 소유한 스트림이면 건너뛴다
    if len(stream.chunks) == 1 && !immediate && !shouldFlush &&
        !instance.ownedStreamsSvc.isStreamNotOwned(stream.fp) {
        return
    }

    // 플러시 큐에 삽입
    flushQueueIndex := int(uint64(stream.fp) % uint64(i.cfg.ConcurrentFlushes))
    firstTime, _ := stream.chunks[0].chunk.Bounds()
    i.flushQueues[flushQueueIndex].Enqueue(&flushOp{
        model.TimeFromUnixNano(firstTime.UnixNano()),
        instance.instanceID,
        stream.fp,
        immediate,
    })
}
```

### 6.6 flushOp

```go
type flushOp struct {
    from      model.Time        // 우선순위 결정에 사용
    userID    string
    fp        model.Fingerprint
    immediate bool
}

func (o *flushOp) Key() string {
    return fmt.Sprintf("%s-%s-%v", o.userID, o.fp, o.immediate)
}

func (o *flushOp) Priority() int64 {
    return -int64(o.from)  // 오래된 청크가 우선
}
```

### 6.7 flushLoop()

```go
func (i *Ingester) flushLoop(j int) {
    defer i.flushQueuesDone.Done()

    for {
        o := i.flushQueues[j].Dequeue()  // 블로킹
        if o == nil {
            return  // 큐 닫힘
        }
        op := o.(*flushOp)

        // 레이트 리밋 (non-immediate만)
        if !op.immediate {
            _ = i.flushRateLimiter.Wait(context.Background())
        }

        err := i.flushOp(logger, op)
        if err != nil {
            level.Error(logger).Log("msg", "failed to flush", "err", err)
        }

        // 즉시 플러시 실패 시 재큐잉
        if op.immediate && err != nil {
            op.from = op.from.Add(flushBackoff)
            i.flushQueues[j].Enqueue(op)
        }
    }
}
```

### 6.8 속도 제한

```go
func (i *Ingester) setFlushRate() {
    totalQueueLength := 0
    for _, q := range i.flushQueues {
        totalQueueLength += q.Length()
    }
    const jitter = 1.05
    flushesPerSecond := float64(totalQueueLength) /
        i.cfg.FlushCheckPeriod.Seconds() * jitter

    if flushesPerSecond*i.cfg.FlushCheckPeriod.Seconds() < minFlushes {
        flushesPerSecond = minFlushes / i.cfg.FlushCheckPeriod.Seconds()
    }

    i.flushRateLimiter.SetLimit(rate.Limit(flushesPerSecond))
}
```

속도 제한의 목표: 플러시 작업을 전체 FlushCheckPeriod에 걸쳐 균등하게 분산시키는 것이다. 한꺼번에 대량 플러시가 발생하면 Object Store에 부하가 집중되므로, 이를 방지한다.

### 6.9 collectChunksToFlush()

```go
func (i *Ingester) collectChunksToFlush(instance *instance, fp model.Fingerprint,
    immediate bool) ([]*chunkDesc, labels.Labels, *sync.RWMutex) {

    stream, ok := instance.streams.LoadByFP(fp)
    if !ok {
        return nil, labels.EmptyLabels(), nil
    }

    stream.chunkMtx.Lock()
    defer stream.chunkMtx.Unlock()

    notOwnedStream := instance.ownedStreamsSvc.isStreamNotOwned(fp)

    var result []*chunkDesc
    for j := range stream.chunks {
        shouldFlush, reason := i.shouldFlushChunk(&stream.chunks[j])

        // 소유하지 않은 스트림의 청크도 플러시
        if !shouldFlush && notOwnedStream {
            shouldFlush, reason = true, flushReasonNotOwned
        }

        if immediate || shouldFlush {
            if !stream.chunks[j].closed {
                stream.chunks[j].closed = true
            }
            if stream.chunks[j].flushed.IsZero() {
                if immediate {
                    reason = flushReasonForced
                }
                stream.chunks[j].reason = reason
                result = append(result, &stream.chunks[j])
            }
        }
    }
    return result, stream.labels, &stream.chunkMtx
}
```

### 6.10 removeFlushedChunks()

플러시 완료 후 `RetainPeriod`가 지난 청크를 메모리에서 제거한다:

```go
func (i *Ingester) removeFlushedChunks(instance *instance, stream *stream,
    mayRemoveStream bool) {

    now := time.Now()
    stream.chunkMtx.Lock()
    defer stream.chunkMtx.Unlock()

    for len(stream.chunks) > 0 {
        if stream.chunks[0].flushed.IsZero() ||
            now.Sub(stream.chunks[0].flushed) < i.cfg.RetainPeriod {
            break
        }
        // WAL 백프레셔 해제
        i.replayController.Sub(int64(stream.chunks[0].chunk.UncompressedSize()))
        stream.chunks[0].chunk = nil  // GC 대상으로 만듦
        stream.chunks = stream.chunks[1:]
    }

    // 빈 스트림 제거
    if mayRemoveStream && len(stream.chunks) == 0 {
        instance.removeStream(stream)
    }
}
```

---

## 7. WAL (Write-Ahead Log)

### 7.1 WAL 개요

WAL은 디스크에 쓰기 작업을 기록하여, 프로세스 충돌 시 데이터 손실을 방지한다.

```
Push 요청
    │
    ├──► 인메모리 청크에 추가
    │
    └──► WAL에 기록 (디스크)
              │
              ├── 시리즈 레코드 (새 스트림)
              │
              └── 엔트리 레코드 (로그 라인)
```

### 7.2 WALConfig

```go
type WALConfig struct {
    Enabled           bool          `yaml:"enabled"`
    Dir               string        `yaml:"dir"`
    CheckpointDuration time.Duration `yaml:"checkpoint_duration"`
    FlushOnShutdown   bool          `yaml:"flush_on_shutdown"`
    ReplayMemoryCeiling flagext.ByteSize `yaml:"replay_memory_ceiling"`
}
```

### 7.3 WAL 인터페이스

```go
type WAL interface {
    Start()
    Log(record *wal.Record) error
    Stop() error
}
```

### 7.4 체크포인트

체크포인트는 WAL의 압축 형태로, 현재 인메모리 상태의 스냅샷이다:

```
WAL 디렉토리 구조:
    wal/
    ├── checkpoint.000005/     ← 가장 최근 체크포인트
    │   ├── 00000000
    │   └── 00000001
    ├── 00000006               ← 체크포인트 이후의 WAL 세그먼트
    ├── 00000007
    └── 00000008
```

체크포인트의 이점:
- **복구 속도**: 전체 WAL을 재생하지 않고 최신 체크포인트부터 시작
- **디스크 공간**: 오래된 WAL 세그먼트 삭제 가능
- **메모리 효율**: 중복 엔트리 제거

### 7.5 디스크 모니터링

WAL은 디스크 용량을 모니터링하여, 공간이 부족하면 플러시를 트리거한다:

```go
// flushOnShutdownSwitch
type OnceSwitch struct {
    triggered atomic.Bool
}
```

디스크가 가득 차면 `flushOnShutdownSwitch`가 켜지고, 셧다운 시 인메모리 데이터를 플러시한다.

---

## 8. 복구 (Recovery)

파일: `pkg/ingester/recovery.go`

### 8.1 Recoverer 인터페이스

```go
type Recoverer interface {
    NumWorkers() int
    Series(series *Series) error
    SetStream(ctx context.Context, userID string, series record.RefSeries) error
    Push(userID string, entries wal.RefEntries) error
    Done() <-chan struct{}
}
```

### 8.2 ingesterRecoverer

```go
type ingesterRecoverer struct {
    users  sync.Map  // map[userID]map[fingerprint]*stream
    ing    *Ingester
    logger log.Logger
    done   chan struct{}
}

// 모든 CPU 코어 사용
func (r *ingesterRecoverer) NumWorkers() int {
    return runtime.GOMAXPROCS(0)
}
```

### 8.3 RecoverCheckpoint

체크포인트에서 시리즈(스트림 + 청크) 정보를 복구한다:

```go
func (r *ingesterRecoverer) Series(series *Series) error {
    return r.ing.replayController.WithBackPressure(func() error {
        // 1. 테넌트 인스턴스 가져오기/생성
        inst, err := r.ing.GetOrCreateInstance(series.UserID)

        // 2. 스트림 가져오기/생성
        stream, err := inst.getOrCreateStream(context.Background(),
            logproto.Stream{Labels: ...}, nil, "loki")

        // 3. 청크 복원
        bytesAdded, entriesAdded, err := stream.setChunks(series.Chunks)

        // 4. 상태 복원
        stream.lastLine.ts = series.To
        stream.lastLine.content = series.LastLine
        stream.entryCt = series.EntryCt
        stream.highestTs = series.HighestTs

        // 5. 메트릭 업데이트
        r.ing.metrics.memoryChunks.Add(float64(len(series.Chunks)))
        r.ing.metrics.recoveredChunksTotal.Add(float64(len(series.Chunks)))
        r.ing.metrics.recoveredEntriesTotal.Add(float64(entriesAdded))

        // 6. 백프레셔 정보 전달
        r.ing.replayController.Add(int64(bytesAdded))

        // 7. 핑거프린트 매핑 저장
        streamsMap.Store(chunks.HeadSeriesRef(series.Fingerprint), stream)

        return nil
    })
}
```

### 8.4 RecoverWAL

체크포인트 이후의 WAL 세그먼트에서 개별 엔트리를 복구한다:

```go
func RecoverWAL(ctx context.Context, reader WALReader, recoverer Recoverer) error {
    // WAL 레코드를 순차적으로 읽어서 복구
    // SetStream: 새 시리즈 레코드 처리
    // Push: 엔트리 레코드 처리
}
```

### 8.5 백프레셔 (Backpressure)

복구 중 메모리 사용량이 급증할 수 있으므로, `replayController`가 백프레셔를 관리한다:

```go
type replayController struct {
    // 현재 복구된 데이터 크기 추적
    // ReplayMemoryCeiling 초과 시 플러시 대기
}
```

```
복구 흐름 (백프레셔 포함):

체크포인트 복구
    │
    ├── 시리즈 복구 (병렬, NumWorkers 개)
    │     │
    │     ├── replayController.Add(bytesAdded)
    │     │
    │     └── ceiling 초과 시 → WithBackPressure 대기
    │                              │
    │                              └── 플러시 완료 → 계속
    │
    ▼
WAL 세그먼트 복구
    │
    ├── 엔트리 복구
    │
    └── 동일한 백프레셔 메커니즘 적용
```

### 8.6 체크포인트 시작 시 정리

```go
func cleanupCheckpointsAtStartup(dir string, logger log.Logger) {
    // 1. 실패한 .tmp 체크포인트 디렉토리 삭제
    cleanupStaleTmpCheckpoints(dir, logger)

    // 2. 최신 체크포인트 보호
    _, latestCheckpointIdx, _ := lastCheckpoint(dir)

    // 3. 이전 체크포인트 삭제
    cleanupOldCheckpoints(dir, latestCheckpointIdx, logger)
}
```

---

## 9. 역인덱스 (Inverted Index)

파일: `pkg/ingester/index/index.go`

### 9.1 InvertedIndex 구조체

```go
type InvertedIndex struct {
    totalShards uint32
    shards      []*indexShard
}
```

역인덱스는 레이블 쌍(label pair)에서 핑거프린트(fingerprint)로의 매핑을 관리한다. 쓰기 경합을 줄이기 위해 여러 샤드로 분할된다.

### 9.2 Interface

```go
type Interface interface {
    Add(labels []logproto.LabelAdapter, fp model.Fingerprint) labels.Labels
    Lookup(matchers []*labels.Matcher, shard *logql.Shard) ([]model.Fingerprint, error)
    LabelNames(shard *logql.Shard) ([]string, error)
    LabelValues(name string, shard *logql.Shard) ([]string, error)
    Delete(labels labels.Labels, fp model.Fingerprint)
}
```

### 9.3 샤딩

```go
func NewWithShards(totalShards uint32) *InvertedIndex {
    shards := make([]*indexShard, totalShards)
    for i := uint32(0); i < totalShards; i++ {
        shards[i] = &indexShard{
            idx:   map[string]indexEntry{},
            shard: i,
        }
    }
    return &InvertedIndex{
        totalShards: totalShards,
        shards:      shards,
    }
}
```

기본 샤드 수: `DefaultIndexShards = 32`

### 9.4 Add (레이블 인덱싱)

```go
func (ii *InvertedIndex) Add(labels []logproto.LabelAdapter,
    fp model.Fingerprint) labels.Labels {

    shardIndex := labelsSeriesIDHash(logproto.FromLabelAdaptersToLabels(labels))
    shard := ii.shards[shardIndex % ii.totalShards]
    return shard.add(labels, fp)
}
```

레이블은 해시에 의해 특정 샤드에 할당되며, 각 샤드는 독립적인 락을 가진다.

### 9.5 Lookup (쿼리)

```go
func (ii *InvertedIndex) getShards(shard *index.ShardAnnotation) []*indexShard {
    if shard == nil {
        return ii.shards  // 모든 샤드 검색
    }

    // 쿼리 샤드에 해당하는 인덱스 샤드만 선택
    totalRequested := ii.totalShards / shard.Of
    result := make([]*indexShard, totalRequested)
    for i := uint32(0); i < totalRequested; i++ {
        subShard := ((shard.Shard) + (i * shard.Of))
        result[j] = ii.shards[subShard]
    }
    return result
}
```

### 9.6 역인덱스 구조 다이어그램

```
InvertedIndex (totalShards=32)
    │
    ├── shard[0]
    │     └── idx["app"]
    │           └── "nginx" → [fp1, fp3, fp7]
    │           └── "web"   → [fp2, fp5]
    │
    ├── shard[1]
    │     └── idx["env"]
    │           └── "prod"  → [fp1, fp2]
    │           └── "dev"   → [fp3]
    │
    ├── shard[2]
    │     └── idx["level"]
    │           └── "error" → [fp1]
    │
    ├── ...
    │
    └── shard[31]

Lookup({app="nginx", env="prod"}):
    1. shard[0]: app="nginx" → {fp1, fp3, fp7}
    2. shard[1]: env="prod"  → {fp1, fp2}
    3. 교집합: {fp1}
```

---

## 10. 소유권 관리

### 10.1 소유 스트림 서비스

Ring 토폴로지가 변경되면(Ingester 추가/제거), 일부 스트림은 현재 Ingester가 더 이상 소유하지 않게 된다. 이런 스트림은 플러시 후 제거해야 한다.

```go
type ownedStreamService struct {
    // 주기적으로 Ring을 확인하여 소유 상태 재계산
}
```

### 10.2 소유권 전략

**Ingester 전략:**

```go
type ownedStreamsIngesterStrategy struct {
    ingesterID string
    readRing   ring.ReadRing
    logger     log.Logger
}
```

Ingester Ring에서 현재 인스턴스가 소유하는 토큰 범위를 기반으로 소유 스트림을 결정한다.

**Partition 전략 (Kafka 모드):**

```go
type ownedStreamsPartitionStrategy struct {
    partitionID    int32
    partitionRing  ring.PartitionRingReader
    tenantShardSize func(string) int
    logger         log.Logger
}
```

파티션 링에서 현재 파티션이 소유하는 스트림을 결정한다.

### 10.3 recalculateOwnedStreamsSvc

```go
type recalculateOwnedStreamsSvc struct {
    // OwnedStreamsCheckInterval(30초)마다 Ring 변경 감지
    // 변경 시 모든 instance의 소유 스트림 재계산
}
```

### 10.4 소유하지 않은 스트림 플러시

`sweepStream()`에서 소유하지 않은 스트림의 청크도 플러시 대상에 포함된다:

```go
if !shouldFlush && notOwnedStream {
    shouldFlush, reason = true, flushReasonNotOwned
}
```

---

## 11. 테일링 (Tailing)

파일: `pkg/ingester/tailer.go`

### 11.1 tailer 구조체

```go
type tailer struct {
    id          uint32
    orgID       string
    matchers    []*labels.Matcher
    pipeline    syntax.Pipeline
    pipelineMtx sync.Mutex

    queue    chan tailRequest          // 매칭할 스트림 큐
    sendChan chan *logproto.Stream     // 전송할 스트림 큐

    closeChan chan struct{}
    closeOnce sync.Once
    closed    atomic.Bool

    blockedAt         *time.Time
    blockedMtx        sync.RWMutex
    droppedStreams     []*logproto.DroppedStream
    maxDroppedStreams  int

    conn TailServer
}

const (
    bufferSizeForTailResponse = 5
    bufferSizeForTailStream   = 100
)
```

### 11.2 테일러 등록

새 테일 요청이 오면 `newTailer()`로 테일러를 생성하고 instance에 등록한다:

```go
func newTailer(orgID string, expr syntax.LogSelectorExpr, conn TailServer,
    maxDroppedStreams int) (*tailer, error) {

    pipeline, err := expr.Pipeline()
    matchers := expr.Matchers()

    return &tailer{
        orgID:            orgID,
        matchers:         matchers,
        sendChan:         make(chan *logproto.Stream, bufferSizeForTailResponse),
        queue:            make(chan tailRequest, bufferSizeForTailStream),
        conn:             conn,
        droppedStreams:    make([]*logproto.DroppedStream, 0, maxDroppedStreams),
        maxDroppedStreams: maxDroppedStreams,
        id:               generateUniqueID(orgID, expr.String()),
        closeChan:        make(chan struct{}),
    }, nil
}
```

### 11.3 브로드캐스트

새 엔트리가 Push될 때 모든 등록된 테일러에게 브로드캐스트된다:

```
Push(entries)
    │
    ▼
stream.tailerMtx.RLock()
    │
    ▼
각 tailer에 대해:
    │
    ├── matchers 매칭 체크
    │
    ├── 매칭 성공 → tailer.queue에 전송
    │
    └── 매칭 실패 → skip
```

### 11.4 테일러 처리 루프

테일러는 두 개의 고루틴으로 구성된다:

```
[고루틴 1: 큐 처리]
    queue에서 tailRequest 수신
        │
        ▼
    pipeline 적용 (필터링, 파싱)
        │
        ▼
    sendChan에 결과 전송

[고루틴 2: 전송]
    sendChan에서 Stream 수신
        │
        ▼
    gRPC Send() 호출
        │
        ├── 성공 → 계속
        │
        └── 실패/차단 →
              blockedAt 기록
              droppedStreams에 추가
```

### 11.5 드롭 처리

테일러가 느려서 큐가 가득 차면, 스트림이 드롭된다:

```go
type TailServer interface {
    Send(*logproto.TailResponse) error
    Context() context.Context
}
```

`TailResponse`에는 드롭된 스트림 정보도 포함되어 클라이언트에게 데이터 손실을 알린다.

---

## 참고 파일 경로

| 파일 | 설명 |
|------|------|
| `pkg/ingester/ingester.go` | Ingester 구조체, Config, 라이프사이클 |
| `pkg/ingester/instance.go` | instance 구조체, 테넌트별 관리 |
| `pkg/ingester/stream.go` | stream 구조체, Push, 청크 관리 |
| `pkg/ingester/flush.go` | 플러시 시스템, sweepUsers, flushLoop |
| `pkg/ingester/recovery.go` | WAL 복구, RecoverCheckpoint, RecoverWAL |
| `pkg/ingester/checkpoint.go` | 체크포인트 생성/관리 |
| `pkg/ingester/wal/` | WAL 구현 |
| `pkg/ingester/index/index.go` | InvertedIndex, 샤딩된 역인덱스 |
| `pkg/ingester/tailer.go` | 테일링, 라이브 스트리밍 |
| `pkg/ingester/owned_streams.go` | 소유 스트림 서비스 |
| `pkg/ingester/recalculate_owned_streams.go` | 소유 스트림 재계산 |
| `pkg/ingester/limiter.go` | 스트림 수 리밋, 레이트 리밋 |
| `pkg/ingester/streams_map.go` | 스트림 맵 구현 |
| `pkg/ingester/mapper.go` | 핑거프린트 매퍼 |
| `pkg/ingester/metrics.go` | Ingester 메트릭 |
| `pkg/ingester/kafka_consumer.go` | Kafka 컨슈머 |
| `pkg/ingester/downscale.go` | 다운스케일 관리 |
| `pkg/ingester/replay_controller.go` | 복구 백프레셔 컨트롤러 |
| `pkg/ingester/stream_rate_calculator.go` | 스트림 속도 계산기 |
