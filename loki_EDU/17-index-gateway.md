# 17. 인덱스 게이트웨이 Deep-Dive

## 목차
1. [개요: 왜 별도 서비스인가?](#1-개요-왜-별도-서비스인가)
2. [Gateway 구조체와 인터페이스](#2-gateway-구조체와-인터페이스)
3. [QueryIndex(): 테이블 기반 라우팅과 배치 응답](#3-queryindex-테이블-기반-라우팅과-배치-응답)
4. [GetChunkRef(): 청크 참조 조회와 블룸 필터링](#4-getchunkref-청크-참조-조회와-블룸-필터링)
5. [GetShards(): 바운디드 샤딩](#5-getshards-바운디드-샤딩)
6. [IndexQuerier 인터페이스](#6-indexquerier-인터페이스)
7. [멀티 피리어드 라우팅: IndexClientWithRange](#7-멀티-피리어드-라우팅-indexclientwithrange)
8. [Ring 모드 vs Simple 모드](#8-ring-모드-vs-simple-모드)
9. [ShuffleShard: 테넌트별 게이트웨이 할당](#9-shuffleshard-테넌트별-게이트웨이-할당)
10. [클라이언트 풀과 연결 관리](#10-클라이언트-풀과-연결-관리)
11. [gRPC 인터셉터: PerTenantRequestCount](#11-grpc-인터셉터-pertenantrequestcount)
12. [블룸 쿼리어 통합](#12-블룸-쿼리어-통합)
13. [메트릭과 모니터링](#13-메트릭과-모니터링)
14. [설정 참조](#14-설정-참조)

---

## 1. 개요: 왜 별도 서비스인가?

인덱스 게이트웨이(Index Gateway)는 Loki의 인덱스 조회를 전담하는 별도 서비스다. 기존에는 각 Querier가 직접 인덱스를 읽었지만, 이 방식에는 심각한 문제가 있었다:

```
Before (Querier 직접 인덱스 읽기):
┌──────────┐    ┌─────────────┐
│ Querier-0├───►│ Object      │
│ (인덱스  │    │ Storage     │
│  캐시)   │    │ (인덱스)    │
├──────────┤    │             │
│ Querier-1├───►│             │
│ (인덱스  │    │             │
│  캐시)   │    └─────────────┘
├──────────┤
│ Querier-2│     문제:
│ (인덱스  │     1. 각 Querier에 인덱스 캐시 중복
│  캐시)   │     2. 메모리 낭비 (N * 인덱스 크기)
└──────────┘     3. 콜드 스타트 시 느린 쿼리

After (인덱스 게이트웨이):
┌──────────┐    ┌──────────────┐    ┌─────────────┐
│ Querier-0├───►│ Index        ├───►│ Object      │
├──────────┤    │ Gateway      │    │ Storage     │
│ Querier-1├───►│ (인덱스 캐시 │    │ (인덱스)    │
├──────────┤    │  집중 관리)  │    │             │
│ Querier-2├───►│              │    └─────────────┘
└──────────┘    └──────────────┘
                 장점:
                 1. 인덱스 캐시 단일화
                 2. Querier 메모리 오프로드
                 3. 인덱스 캐시 적중률 향상
```

---

## 2. Gateway 구조체와 인터페이스

소스 경로: `pkg/indexgateway/gateway.go`

### 2.1 핵심 인터페이스

```go
// pkg/indexgateway/gateway.go (line 47-67)
type IndexQuerier interface {
    stores.ChunkFetcher
    index.BaseReader
    index.StatsReader
    Stop()
}

type IndexClient interface {
    seriesindex.ReadClient
    Stop()
}

type IndexClientWithRange struct {
    IndexClient
    TableRange config.TableRange
}

type BloomQuerier interface {
    FilterChunkRefs(ctx context.Context, tenant string,
        from, through model.Time,
        series map[uint64]labels.Labels,
        chunks []*logproto.ChunkRef,
        plan plan.QueryPlan,
    ) ([]*logproto.ChunkRef, bool, error)
}
```

### 2.2 Gateway 구조체

```go
// pkg/indexgateway/gateway.go (line 68-79)
type Gateway struct {
    services.Service

    indexQuerier IndexQuerier          // TSDB 기반 인덱스 조회
    indexClients []IndexClientWithRange // 레거시 인덱스 클라이언트 (피리어드별)
    bloomQuerier BloomQuerier           // 블룸 게이트웨이 연동

    metrics      *Metrics
    cfg          Config
    limits       Limits
    log          log.Logger
}
```

### 2.3 Gateway 생성

```go
// pkg/indexgateway/gateway.go (line 82-110)
func NewIndexGateway(cfg Config, limits Limits, log log.Logger,
    r prometheus.Registerer, indexQuerier IndexQuerier,
    indexClients []IndexClientWithRange,
    bloomQuerier BloomQuerier,
) (*Gateway, error) {
    g := &Gateway{
        indexQuerier: indexQuerier,
        bloomQuerier: bloomQuerier,
        cfg:          cfg,
        limits:       limits,
        log:          log,
        indexClients: indexClients,
        metrics:      NewMetrics(r),
    }

    // 최신 피리어드 먼저 조회하도록 정렬
    sort.Slice(g.indexClients, func(i, j int) bool {
        return g.indexClients[i].TableRange.Start >
               g.indexClients[j].TableRange.Start
    })

    g.Service = services.NewIdleService(nil, func(_ error) error {
        g.indexQuerier.Stop()
        for _, indexClient := range g.indexClients {
            indexClient.Stop()
        }
        return nil
    })

    return g, nil
}
```

---

## 3. QueryIndex(): 테이블 기반 라우팅과 배치 응답

소스 경로: `pkg/indexgateway/gateway.go` (line 112-177)

### 3.1 상수

```go
// pkg/indexgateway/gateway.go (line 43-45)
const (
    maxIndexEntriesPerResponse = 1000
)
```

### 3.2 QueryIndex 흐름

```
QueryIndex(request)
     │
     ▼
[1] 쿼리 변환: logproto.IndexQuery → seriesindex.Query
     │  ├── 유효한 테이블 번호 추출
     │  └── 잘못된 테이블 이름은 스킵
     │
     ▼
[2] 테이블 번호 기준 정렬
     │  (오래된 테이블부터 처리)
     │
     ▼
[3] 각 indexClient에 대해:
     │
     │  ┌─────────────────────────────────────────┐
     │  │ indexClient.TableRange로 해당 쿼리 찾기   │
     │  │                                         │
     │  │ start = BinarySearch(tableNum >= Start)  │
     │  │ end   = BinarySearch(tableNum > End)     │
     │  │                                         │
     │  │ queries[start:end]를 이 클라이언트에서 실행│
     │  └─────────────────────────────────────────┘
     │
     ▼
[4] QueryPages 콜백:
     │  buildResponses()
     │
     │  ┌─────────────────────────────────────────┐
     │  │ 배치 응답 (maxIndexEntriesPerResponse)    │
     │  │                                         │
     │  │ 결과를 1000개씩 잘라서 gRPC 스트림 전송   │
     │  │ → 메모리 사용량 제한                      │
     │  │ → 대량 인덱스 응답의 안전한 전송           │
     │  └─────────────────────────────────────────┘
```

### 3.3 코드 상세

```go
// pkg/indexgateway/gateway.go (line 112-177)
func (g *Gateway) QueryIndex(request *logproto.QueryIndexRequest,
    server logproto.IndexGateway_QueryIndexServer) error {

    queries := make([]seriesindex.Query, 0, len(request.Queries))
    for _, query := range request.Queries {
        // 유효하지 않은 테이블 이름 스킵
        if _, err := config.ExtractTableNumberFromName(query.TableName); err != nil {
            level.Error(log).Log("msg", "skip querying table",
                "table", query.TableName, "err", err)
            continue
        }
        queries = append(queries, seriesindex.Query{
            TableName:        query.TableName,
            HashValue:        query.HashValue,
            RangeValuePrefix: query.RangeValuePrefix,
            RangeValueStart:  query.RangeValueStart,
            ValueEqual:       query.ValueEqual,
        })
    }

    // 테이블 번호 기준 정렬
    sort.Slice(queries, func(i, j int) bool {
        ta, _ := config.ExtractTableNumberFromName(queries[i].TableName)
        tb, _ := config.ExtractTableNumberFromName(queries[j].TableName)
        return ta < tb
    })

    sendBatchMtx := sync.Mutex{}
    for _, indexClient := range g.indexClients {
        // 이 클라이언트가 처리할 수 있는 쿼리 범위 결정
        start := sort.Search(len(queries), func(i int) bool {
            tableNumber, _ := config.ExtractTableNumberFromName(
                queries[i].TableName)
            return tableNumber >= indexClient.TableRange.Start
        })
        end := sort.Search(len(queries), func(j int) bool {
            tableNumber, _ := config.ExtractTableNumberFromName(
                queries[j].TableName)
            return tableNumber > indexClient.TableRange.End
        })
        if end-start <= 0 {
            continue
        }

        outerErr = indexClient.QueryPages(server.Context(),
            queries[start:end],
            func(query seriesindex.Query,
                 batch seriesindex.ReadBatchResult) bool {
                // 배치 응답 전송 (Mutex로 직렬화)
                innerErr = buildResponses(query, batch,
                    func(response *logproto.QueryIndexResponse) error {
                        sendBatchMtx.Lock()
                        defer sendBatchMtx.Unlock()
                        return server.Send(response)
                    })
                return innerErr == nil
            })
        // ...
    }
    return nil
}
```

### 3.4 buildResponses: 배치 응답

```go
// pkg/indexgateway/gateway.go (line 179-212)
func buildResponses(query seriesindex.Query,
    batch seriesindex.ReadBatchResult,
    callback func(*logproto.QueryIndexResponse) error) error {

    itr := batch.Iterator()
    var resp []*logproto.Row

    for itr.Next() {
        // 1000개에 도달하면 전송
        if len(resp) == maxIndexEntriesPerResponse {
            err := callback(&logproto.QueryIndexResponse{
                QueryKey: seriesindex.QueryKey(query),
                Rows:     resp,
            })
            if err != nil { return err }
            resp = []*logproto.Row{}
        }

        resp = append(resp, &logproto.Row{
            RangeValue: itr.RangeValue(),
            Value:      itr.Value(),
        })
    }

    // 나머지 전송
    if len(resp) != 0 {
        return callback(&logproto.QueryIndexResponse{
            QueryKey: seriesindex.QueryKey(query),
            Rows:     resp,
        })
    }
    return nil
}
```

---

## 4. GetChunkRef(): 청크 참조 조회와 블룸 필터링

소스 경로: `pkg/indexgateway/gateway.go` (line 214-308)

### 4.1 GetChunkRef 흐름

```
GetChunkRef(req)
     │
     ▼
[1] 테넌트 ID 추출
     │
     ▼
[2] 매처 파싱 (syntax.ParseMatchers)
     │
     ▼
[3] indexQuerier.GetChunks()
     │  → 인덱스에서 청크 참조 조회
     │  → chunkRefsLookupDuration 측정
     │
     ▼
[4] 결과 변환 (ChunkRef 리스트)
     │
     ├── 블룸 쿼리어 없음? → 그대로 반환
     │
     ├── 테스트 가능한 LabelFilter 없음? → 그대로 반환
     │
     └── 블룸 필터링 가능:
         │
         ▼
    [5] indexQuerier.GetSeries()
         │  → 시리즈 정보 조회 (블룸에 필요)
         │
         ▼
    [6] bloomQuerier.FilterChunkRefs()
         │  → 블룸 필터로 불필요한 청크 제거
         │
         ▼
    [7] 필터링된 결과 반환
         │  → Stats 포함 (필터 전/후 청크 수, 블룸 사용 여부)
```

### 4.2 코드 상세

```go
func (g *Gateway) GetChunkRef(ctx context.Context,
    req *logproto.GetChunkRefRequest,
) (result *logproto.GetChunkRefResponse, err error) {

    instanceID, err := tenant.TenantID(ctx)
    matchers, err := syntax.ParseMatchers(req.Matchers, true)

    predicate := chunk.NewPredicate(matchers, &req.Plan)

    // [단계 3] 인덱스 조회
    chunkRefsLookupStart := time.Now()
    chunks, _, err := g.indexQuerier.GetChunks(ctx, instanceID,
        req.From, req.Through, predicate, nil)
    chunkRefsLookupDuration := time.Since(chunkRefsLookupStart)

    result = &logproto.GetChunkRefResponse{
        Refs: make([]*logproto.ChunkRef, 0, len(chunks)),
    }
    for _, cs := range chunks {
        for i := range cs {
            result.Refs = append(result.Refs, &cs[i].ChunkRef)
        }
    }

    initialChunkCount := len(result.Refs)

    // 유니크 스트림 수 계산
    seen := make(map[uint64]struct{}, initialChunkCount)
    for _, ref := range result.Refs {
        seen[ref.Fingerprint] = struct{}{}
    }
    result.Stats.TotalStreams = int64(len(seen))

    // [블룸 필터링]
    if g.bloomQuerier == nil { return result, nil }
    if len(v1.ExtractTestableLabelMatchers(req.Plan.AST)) == 0 {
        return result, nil
    }

    // 시리즈 정보 조회 (블룸에 필요)
    series, err := g.indexQuerier.GetSeries(ctx, instanceID,
        req.From, req.Through, matchers...)
    seriesMap := make(map[uint64]labels.Labels, len(series))
    for _, s := range series {
        seriesMap[labels.StableHash(s)] = s
    }

    // 블룸 필터링
    chunkRefs, used, err := g.bloomQuerier.FilterChunkRefs(ctx, instanceID,
        req.From, req.Through, seriesMap, result.Refs, req.Plan)

    result.Refs = chunkRefs
    result.Stats.PostFilterChunks = int64(len(result.Refs))
    result.Stats.UsedBloomFilters = used
    result.Stats.BloomFilterTime = bloomFilterDuration.Seconds()

    return result, nil
}
```

### 4.3 블룸 필터링 효과

```
인덱스 조회 결과: 100,000 청크
     │
     ▼
블룸 필터 적용
     │  - 쿼리에 |= "error" 포함
     │  - 블룸 필터로 "error"가 없는 청크 제거
     │
     ▼
필터링 후: 5,000 청크 (95% 감소)
     │
     ▼
Object Storage에서 5,000개만 읽기 (I/O 95% 절감)
```

---

## 5. GetShards(): 바운디드 샤딩

소스 경로: `pkg/indexgateway/gateway.go` (line 421-585)

### 5.1 GetShards 개요

GetShards는 쿼리의 **데이터 볼륨**을 기반으로 최적의 샤드 경계를 계산한다. 이를 통해 Querier가 균등한 양의 데이터를 처리하도록 쿼리를 분할한다.

```
GetShards(req)
     │
     ▼
[1] 쿼리 파싱 → Predicate 추출
     │
     ▼
[2] 인덱스가 사이징 정보를 지원하는가?
     │
     ├── NO → indexQuerier.GetShards() (기본 구현)
     │
     └── YES → boundedShards() (최적화된 구현)
```

### 5.2 boundedShards 상세

```go
// pkg/indexgateway/gateway.go (line 460-585)
func (g *Gateway) boundedShards(ctx context.Context,
    req *logproto.ShardsRequest,
    server logproto.IndexGateway_GetShardsServer,
    instanceID string, p chunk.Predicate,
) error {
    // [1] 사이징 정보가 포함된 청크 참조 조회
    refs, err := g.indexQuerier.GetChunkRefsWithSizingInfo(ctx,
        instanceID, req.From, req.Through, p)

    // [2] 블룸 필터링 (현재 비활성)
    filtered := refs

    // [3] 샤드 경계 계산
    shards, chunkGrps, err := accumulateChunksToShards(req, refs)

    resp := &logproto.ShardsResponse{
        Shards: shards,
    }

    // [4] 프리컴퓨트된 청크 참조 포함 (설정에 따라)
    if g.limits.TSDBPrecomputeChunks(instanceID) {
        resp.ChunkGroups = chunkGrps
    }

    return server.Send(resp)
}
```

### 5.3 accumulateChunksToShards: 샤드 경계 계산

```go
// pkg/indexgateway/gateway.go (line 618-658)
func accumulateChunksToShards(req *logproto.ShardsRequest,
    filtered []logproto.ChunkRefWithSizingInfo,
) ([]logproto.Shard, []logproto.ChunkRefGroup, error) {

    // 핑거프린트별 청크 그룹화
    filteredM := make(map[model.Fingerprint][]logproto.ChunkRefWithSizingInfo)
    for _, ref := range filtered {
        filteredM[model.Fingerprint(ref.Fingerprint)] = append(...)
    }

    // 시리즈별 통계 누적 (Chunks, Entries, Bytes)
    collectedSeries := sharding.SizedFPs(...)
    for fp, chks := range filteredM {
        x := sharding.SizedFP{Fp: fp}
        x.Stats.Chunks = uint64(len(chks))
        for _, chk := range chks {
            x.Stats.Entries += uint64(chk.Entries)
            x.Stats.Bytes += uint64(chk.KB << 10)
        }
        collectedSeries = append(collectedSeries, x)
    }
    sort.Sort(collectedSeries)

    // TargetBytesPerShard 기반으로 샤드 경계 결정
    shards := collectedSeries.ShardsFor(req.TargetBytesPerShard)

    // 각 샤드에 해당하는 청크 그룹 생성
    chkGrps := make([]logproto.ChunkRefGroup, 0, len(shards))
    for _, s := range shards {
        // 바이너리 서치로 해당 샤드 범위의 청크 찾기
        from := sort.Search(...)
        through := sort.Search(...)
        chkGrps = append(chkGrps, logproto.ChunkRefGroup{
            Refs: refsWithSizingInfoToRefs(filtered[from:through]),
        })
    }

    return shards, chkGrps, nil
}
```

### 5.4 바운디드 샤딩 시각화

```
TargetBytesPerShard = 500MB

시리즈 (핑거프린트 정렬):
FP 0001: 100MB (200 chunks)
FP 0002: 150MB (300 chunks)
FP 0003:  50MB (100 chunks)
FP 0004: 200MB (400 chunks)
FP 0005: 300MB (600 chunks)
FP 0006: 100MB (200 chunks)
FP 0007: 250MB (500 chunks)

샤드 결과:
┌────────────────────────────────────────┐
│ Shard 0: FP [0001-0003]  = 300MB      │
│ Shard 1: FP [0004-0004]  = 200MB      │ ← 큰 시리즈는 단독 샤드
│ Shard 2: FP [0005-0005]  = 300MB      │
│ Shard 3: FP [0006-0007]  = 350MB      │
└────────────────────────────────────────┘
```

---

## 6. IndexQuerier 인터페이스

소스 경로: `pkg/indexgateway/gateway.go` (line 47-52)

### 6.1 합성 인터페이스

```go
type IndexQuerier interface {
    stores.ChunkFetcher   // 청크 조회
    index.BaseReader      // 시리즈/레이블 조회
    index.StatsReader     // 통계 조회
    Stop()
}
```

### 6.2 제공 메서드

```
IndexQuerier가 제공하는 메서드:

ChunkFetcher:
  ├── GetChunks(ctx, userID, from, through, predicate, storeChunks)
  └── GetChunkRefsWithSizingInfo(ctx, userID, from, through, predicate)

BaseReader:
  ├── GetSeries(ctx, userID, from, through, matchers...)
  ├── LabelNamesForMetricName(ctx, userID, from, through, metricName, matchers...)
  ├── LabelValuesForMetricName(ctx, userID, from, through, metricName, labelName, matchers...)
  ├── GetShards(ctx, userID, from, through, targetBytes, predicate)
  └── HasChunkSizingInfo(from, through) bool

StatsReader:
  ├── Stats(ctx, userID, from, through, matchers...)
  └── Volume(ctx, userID, from, through, limit, targetLabels, aggregateBy, matchers...)
```

### 6.3 Gateway API 메서드

인덱스 게이트웨이는 gRPC를 통해 다음 API를 제공한다:

```
┌──────────────────────────────────────────────────┐
│              Index Gateway gRPC API              │
│                                                  │
│  QueryIndex(QueryIndexRequest)                   │
│  → 레거시 인덱스 클라이언트를 통한 직접 쿼리      │
│                                                  │
│  GetChunkRef(GetChunkRefRequest)                 │
│  → TSDB 기반 청크 참조 조회 + 블룸 필터링         │
│                                                  │
│  GetSeries(GetSeriesRequest)                     │
│  → 시리즈 목록 조회                               │
│                                                  │
│  LabelNamesForMetricName(...)                    │
│  → 레이블 이름 조회                               │
│                                                  │
│  LabelValuesForMetricName(...)                   │
│  → 레이블 값 조회                                 │
│                                                  │
│  GetStats(IndexStatsRequest)                     │
│  → 인덱스 통계 조회                               │
│                                                  │
│  GetVolume(VolumeRequest)                        │
│  → 볼륨 데이터 조회                               │
│                                                  │
│  GetShards(ShardsRequest)                        │
│  → 바운디드 샤드 경계 계산                        │
└──────────────────────────────────────────────────┘
```

---

## 7. 멀티 피리어드 라우팅: IndexClientWithRange

### 7.1 개념

Loki는 인덱스 테이블을 **피리어드(period)**별로 분할한다. 각 피리어드는 특정 시간 범위의 인덱스를 포함한다.

```go
// pkg/indexgateway/gateway.go (line 59-62)
type IndexClientWithRange struct {
    IndexClient
    TableRange config.TableRange  // Start, End 테이블 번호
}
```

### 7.2 테이블 레이아웃

```
시간 →

│   Period 1    │   Period 2    │   Period 3    │
│ Table 19700-  │ Table 19730-  │ Table 19760-  │
│ Table 19729   │ Table 19759   │ Table 19789   │
│               │               │               │
│ IndexClient-0 │ IndexClient-1 │ IndexClient-2 │

쿼리가 2주 범위를 커버하면:
  → Period 2의 IndexClient-1 에서 처리
  → Period 3의 IndexClient-2 에서 처리
```

### 7.3 라우팅 로직

```go
// Gateway 생성 시 최신 피리어드 우선 정렬
sort.Slice(g.indexClients, func(i, j int) bool {
    return g.indexClients[i].TableRange.Start >
           g.indexClients[j].TableRange.Start
})

// QueryIndex에서 바이너리 서치로 적합한 클라이언트 찾기
for _, indexClient := range g.indexClients {
    start := sort.Search(len(queries), func(i int) bool {
        tableNumber, _ := config.ExtractTableNumberFromName(
            queries[i].TableName)
        return tableNumber >= indexClient.TableRange.Start
    })
    end := sort.Search(len(queries), func(j int) bool {
        tableNumber, _ := config.ExtractTableNumberFromName(
            queries[j].TableName)
        return tableNumber > indexClient.TableRange.End
    })
    // queries[start:end] 를 이 클라이언트로 라우팅
}
```

### 7.4 멀티 피리어드의 이점

```
┌────────────────────────────────────────────────────────┐
│  1. 스토리지 마이그레이션 지원                          │
│     - Period 1: BoltDB Shipper                         │
│     - Period 2: TSDB                                   │
│     - 같은 쿼리에서 두 피리어드 자동 라우팅              │
│                                                        │
│  2. 테이블 회전(rotation)                               │
│     - 오래된 테이블 자동 아카이브                        │
│     - 새 테이블 자동 생성                               │
│                                                        │
│  3. 데이터 보존 정책                                    │
│     - 피리어드별 다른 보존 기간 적용 가능                │
└────────────────────────────────────────────────────────┘
```

---

## 8. Ring 모드 vs Simple 모드

소스 경로: `pkg/indexgateway/config.go`

### 8.1 모드 정의

```go
// pkg/indexgateway/config.go (line 47-55)
const (
    SimpleMode Mode = "simple"
    RingMode   Mode = "ring"
)
```

### 8.2 Config 구조체

```go
// pkg/indexgateway/config.go (line 58-67)
type Config struct {
    Mode Mode          `yaml:"mode"`
    Ring ring.RingConfig `yaml:"ring,omitempty"`
}
```

### 8.3 모드 비교

```
┌────────────────────┬──────────────────────┬──────────────────────┐
│                    │ Simple 모드           │ Ring 모드             │
├────────────────────┼──────────────────────┼──────────────────────┤
│ 인스턴스 수        │ 1개 (또는 수동 LB)    │ N개 (Ring 자동 분배) │
│ 테넌트 분배        │ 모든 테넌트 처리      │ Ring으로 분배         │
│ 디스커버리         │ DNS 기반             │ Ring KV Store         │
│ 캐시 효율          │ 전체 인덱스 캐시      │ 테넌트별 분산 캐시   │
│ 적합한 규모        │ 소규모               │ 대규모 멀티테넌트     │
│ 장애 영향          │ 전체 장애            │ 부분 장애            │
│ 스케일링           │ 수동                 │ 자동                 │
│ 설정               │ server-address       │ ring.kvstore         │
└────────────────────┴──────────────────────┴──────────────────────┘
```

### 8.4 Ring 모드 상수

```go
// pkg/indexgateway/config.go (line 13-15)
const (
    NumTokens         = 128
    ReplicationFactor = 3
)
```

- **NumTokens = 128**: 각 게이트웨이 인스턴스가 링에서 128개의 토큰을 가짐
- **ReplicationFactor = 3**: 각 테넌트의 인덱스가 3개의 게이트웨이에 복제

---

## 9. ShuffleShard: 테넌트별 게이트웨이 할당

소스 경로: `pkg/indexgateway/shufflesharding.go`

### 9.1 ShuffleShardingStrategy

```go
// pkg/indexgateway/shufflesharding.go (line 35-49)
type ShuffleShardingStrategy struct {
    r            ring.ReadRing
    limits       Limits
    instanceAddr string
    instanceID   string
}

func NewShuffleShardingStrategy(r ring.ReadRing, l Limits,
    instanceAddr, instanceID string) *ShuffleShardingStrategy {
    return &ShuffleShardingStrategy{
        r:            r,
        limits:       l,
        instanceAddr: instanceAddr,
        instanceID:   instanceID,
    }
}
```

### 9.2 FilterTenants

```go
// pkg/indexgateway/shufflesharding.go (line 52-74)
func (s *ShuffleShardingStrategy) FilterTenants(tenantIDs []string,
) ([]string, error) {
    // 자신이 건강한 상태인지 확인
    if set, err := s.r.GetAllHealthy(IndexesSync); err != nil {
        return nil, err
    } else if !set.Includes(s.instanceAddr) {
        return nil, errGatewayUnhealthy
    }

    var filteredIDs []string
    for _, tenantID := range tenantIDs {
        subRing := GetShuffleShardingSubring(s.r, tenantID, s.limits)

        // 이 인스턴스가 해당 테넌트를 담당하는지 확인
        if subRing.HasInstance(s.instanceID) {
            filteredIDs = append(filteredIDs, tenantID)
        }
    }
    return filteredIDs, nil
}
```

### 9.3 GetShuffleShardingSubring

```go
// pkg/indexgateway/shufflesharding.go (line 79-92)
func GetShuffleShardingSubring(ring ring.ReadRing, tenantID string,
    limits Limits) ring.ReadRing {
    shardSize := limits.IndexGatewayShardSize(tenantID)

    // shardSize가 0이면 전체 링 사용 (셔플 샤딩 비활성)
    if shardSize <= 0 {
        return ring
    }

    return ring.ShuffleShard(tenantID, shardSize)
}
```

### 9.4 ShuffleShard 시각화

```
전체 Ring: [GW-0, GW-1, GW-2, GW-3, GW-4]

ShardSize = 3 인 경우:

tenant-A → ShuffleShard("tenant-A", 3) → [GW-0, GW-2, GW-4]
tenant-B → ShuffleShard("tenant-B", 3) → [GW-1, GW-3, GW-4]
tenant-C → ShuffleShard("tenant-C", 3) → [GW-0, GW-1, GW-3]

GW-0이 FilterTenants 호출:
  → tenant-A: GW-0 포함 ✓
  → tenant-B: GW-0 미포함 ✗
  → tenant-C: GW-0 포함 ✓
  → 결과: [tenant-A, tenant-C]
```

### 9.5 Limits 인터페이스

```go
// pkg/indexgateway/shufflesharding.go (line 22-27)
type Limits interface {
    IndexGatewayShardSize(tenantID string) int
    IndexGatewayMaxCapacity(tenantID string) float64
    TSDBMaxBytesPerShard(string) int
    TSDBPrecomputeChunks(string) bool
}
```

### 9.6 Ring Operations

```go
// pkg/indexgateway/shufflesharding.go (line 10-19)
var (
    // 인덱스 동기화 (소유권 확인)
    IndexesSync = ring.NewOp([]ring.InstanceState{
        ring.JOINING, ring.ACTIVE, ring.LEAVING}, nil)

    // 인덱스 읽기 (쿼리 라우팅)
    IndexesRead = ring.NewOp([]ring.InstanceState{
        ring.ACTIVE}, nil)
)
```

---

## 10. 클라이언트 풀과 연결 관리

소스 경로: `pkg/indexgateway/client.go`

### 10.1 GatewayClient 구조체

```go
// pkg/indexgateway/client.go (line 110-120)
type GatewayClient struct {
    logger                            log.Logger
    cfg                               ClientConfig
    storeGatewayClientRequestDuration *prometheus.HistogramVec
    dnsProvider                       *discovery.DNS
    pool                              *client.Pool
    ring                              ring.ReadRing
    limits                            Limits
    buckets                           []time.Duration
    done                              chan struct{}
}
```

### 10.2 연결 풀 설정

```go
// pkg/indexgateway/client.go (line 181-184)
sgClient.cfg.PoolConfig.RemoteTimeout = 2 * time.Second
sgClient.cfg.PoolConfig.ClientCleanupPeriod = 5 * time.Second
sgClient.cfg.PoolConfig.HealthCheckIngesters = true
```

### 10.3 클라이언트 풀 동작

```
GatewayClient.poolDo()
     │
     ▼
[1] 테넌트 ID 추출
     │
     ▼
[2] 서버 주소 조회
     ├── Ring 모드: ShuffleShard subring → ReplicationSet
     └── Simple 모드: DNS 디스커버리
     │
     ▼
[3] 주소 필터링 (시간 기반 샤딩, Simple 모드만)
     │
     ▼
[4] 주소 셔플 (부하 분산)
     │
     ▼
[5] 순차 시도:
     for addr in addrs {
         client = pool.GetClientFor(addr)
         err = callback(client)
         if err == nil { return nil }
         // 실패 시 다음 주소 시도
     }
```

### 10.4 시간 기반 클라이언트 샤딩 (Simple 모드)

```go
// pkg/indexgateway/client.go (line 638-676)
func addressesForQueryEndTime(addrs []string, t time.Time,
    buckets []time.Duration, now time.Time) []string {

    n := len(addrs)
    m := len(buckets)

    if m < 1 { return addrs }
    if n < (1 << m) { return addrs }

    // 예시: 3개 버킷, 8개 인스턴스
    // Bucket 0: 최근 7일    → addrs[0:4]
    // Bucket 1: 7-14일 전   → addrs[4:6]
    // Bucket 2: 14-21일 전  → addrs[6:7]
    // Remainder: 21일 이상  → addrs[7:8]
}
```

```
┌──────────────────────────────────────────────────┐
│       시간 기반 인덱스 게이트웨이 샤딩              │
│                                                  │
│  addrs = [GW0, GW1, GW2, GW3, GW4, GW5, GW6, GW7] │
│                                                  │
│  쿼리 시간          사용할 게이트웨이               │
│  ────────────────── ─────────────────────          │
│  최근 7일           GW0, GW1, GW2, GW3  (50%)     │
│  7-14일 전          GW4, GW5             (25%)     │
│  14-21일 전         GW6                  (12.5%)   │
│  21일 이상          GW7                  (12.5%)   │
│                                                  │
│  → 최근 쿼리에 더 많은 인스턴스 할당               │
│  → 오래된 쿼리는 적은 인스턴스로 처리               │
│  → 캐시 적중률 극대화                              │
└──────────────────────────────────────────────────┘
```

### 10.5 Jump Hash 기반 테넌트 분배

```go
// pkg/indexgateway/client.go (line 541-566)
func (s *GatewayClient) jumpHashShuffleSharding(tenant string,
    addrs []string) []string {

    f := s.limits.IndexGatewayMaxCapacity(tenant)
    if f == 1.0 || f == 0.0 { return addrs }

    maxAvailableGateways := len(addrs)
    numUserGateways := int(math.Ceil(float64(maxAvailableGateways) * f))

    cs := xxhash.Sum64String(tenant)
    idx := int(jumphash.Hash(cs, maxAvailableGateways))

    subset := make([]string, 0, numUserGateways)
    for i := range numUserGateways {
        subset = append(subset, addrs[(idx+i)%len(addrs)])
    }
    return subset
}
```

---

## 11. gRPC 인터셉터: PerTenantRequestCount

소스 경로: `pkg/indexgateway/grpc.go`

### 11.1 ServerInterceptors

```go
// pkg/indexgateway/grpc.go (line 14-47)
type ServerInterceptors struct {
    reqCount              *prometheus.CounterVec
    PerTenantRequestCount grpc.UnaryServerInterceptor
}

func NewServerInterceptors(r prometheus.Registerer) *ServerInterceptors {
    requestCount := promauto.With(r).NewCounterVec(prometheus.CounterOpts{
        Namespace: constants.Loki,
        Subsystem: "index_gateway",
        Name:      "requests_total",
        Help:      "Total amount of requests served by the index gateway",
    }, []string{"operation", "status", "tenant"})

    perTenantRequestCount := func(ctx context.Context, req interface{},
        info *grpc.UnaryServerInfo,
        handler grpc.UnaryHandler,
    ) (resp interface{}, err error) {
        tenantID, err := tenant.TenantID(ctx)
        if err != nil {
            // 테넌트 ID 없으면 메트릭 없이 처리
            return handler(ctx, req)
        }

        resp, err = handler(ctx, req)
        status := "success"
        if err != nil {
            status = "error"
        }
        requestCount.WithLabelValues(
            info.FullMethod, status, tenantID).Inc()
        return
    }

    return &ServerInterceptors{
        reqCount:              requestCount,
        PerTenantRequestCount: perTenantRequestCount,
    }
}
```

### 11.2 인터셉터 역할

```
gRPC 요청 → PerTenantRequestCount 인터셉터
     │
     ├── 테넌트 ID 추출
     │
     ├── 실제 핸들러 실행
     │
     └── 메트릭 기록:
         loki_index_gateway_requests_total{
             operation="/logproto.IndexGateway/GetChunkRef",
             status="success",
             tenant="tenant-A"
         }
```

---

## 12. 블룸 쿼리어 통합

### 12.1 BloomQuerier 인터페이스

```go
// pkg/indexgateway/gateway.go (line 64-67)
type BloomQuerier interface {
    FilterChunkRefs(ctx context.Context, tenant string,
        from, through model.Time,
        series map[uint64]labels.Labels,
        chunks []*logproto.ChunkRef,
        plan plan.QueryPlan,
    ) ([]*logproto.ChunkRef, bool, error)
}
```

### 12.2 블룸 필터링 조건

블룸 필터링은 다음 조건이 모두 충족될 때만 실행된다:

1. `g.bloomQuerier != nil` (Bloom Gateway 설정됨)
2. `v1.ExtractTestableLabelMatchers(req.Plan.AST)` 결과가 비어있지 않음
3. 쿼리에 테스트 가능한 필터 표현식이 포함됨 (예: `|= "error"`)

### 12.3 블룸 필터링 흐름

```
쿼리: {app="api"} |= "error" | json

[1] 인덱스에서 청크 참조 조회
     → 100,000 청크

[2] ExtractTestableLabelMatchers(AST)
     → [|= "error"]  (테스트 가능한 매처 발견)

[3] GetSeries()
     → 시리즈 목록 + 레이블 조회

[4] bloomQuerier.FilterChunkRefs()
     │
     │  Bloom Gateway:
     │  ├── 각 청크에 대해 블룸 필터 조회
     │  ├── "error" 문자열이 포함되지 않은 청크 제거
     │  └── 필터링된 청크 목록 반환
     │
     └── 결과: 5,000 청크 (95% 감소)

[5] 메트릭 기록:
     preFilterChunks: 100,000
     postFilterChunks: 5,000
     usedBloomFilters: true
     bloomFilterTime: 0.5s
```

---

## 13. 메트릭과 모니터링

### 13.1 Gateway 메트릭

소스 경로: `pkg/indexgateway/metrics.go`

```go
// pkg/indexgateway/metrics.go (line 15-37)
type Metrics struct {
    preFilterChunks  *prometheus.HistogramVec   // 필터 전 청크 수
    postFilterChunks *prometheus.HistogramVec   // 필터 후 청크 수
}
```

| 메트릭 | 타입 | 레이블 | 설명 |
|--------|------|--------|------|
| `loki_index_gateway_prefilter_chunks` | Histogram | route | 필터 전 청크 수 |
| `loki_index_gateway_postfilter_chunks` | Histogram | route | 필터 후 청크 수 |
| `loki_index_gateway_requests_total` | Counter | operation, status, tenant | 테넌트별 요청 수 |
| `loki_index_gateway_request_duration_seconds` | Histogram | operation, status_code | 요청 레이턴시 |

### 13.2 route 레이블 값

```go
// pkg/indexgateway/metrics.go (line 10-12)
const (
    routeChunkRefs = "chunk_refs"
    routeShards    = "shards"
)
```

### 13.3 핵심 모니터링 쿼리

```
# 블룸 필터 효과 측정
1 - (
  rate(loki_index_gateway_postfilter_chunks_sum{route="chunk_refs"}[5m])
  /
  rate(loki_index_gateway_prefilter_chunks_sum{route="chunk_refs"}[5m])
)

# 테넌트별 에러율
rate(loki_index_gateway_requests_total{status="error"}[5m])
/
rate(loki_index_gateway_requests_total[5m])

# 요청 레이턴시 P99
histogram_quantile(0.99,
  rate(loki_index_gateway_request_duration_seconds_bucket[5m]))

# 바운디드 샤딩 효과
rate(loki_index_gateway_prefilter_chunks_sum{route="shards"}[5m])
rate(loki_index_gateway_postfilter_chunks_sum{route="shards"}[5m])
```

---

## 14. 설정 참조

### 14.1 서버 설정

```yaml
index_gateway:
  mode: ring                    # simple 또는 ring
  ring:
    kvstore:
      store: memberlist
    num_tokens: 128             # 고정, 변경 불가
    replication_factor: 3
```

### 14.2 클라이언트 설정

```yaml
index_gateway_client:
  server_address: "index-gateway:9095"  # Simple 모드
  log_gateway_requests: false
  grpc_client_config:
    max_recv_msg_size: 104857600  # 100MB
    max_send_msg_size: 104857600

  # 실험적: 시간 기반 샤딩 버킷
  time_based_sharding_buckets:
    - "168h"    # 7일
    - "336h"    # 14일
    - "504h"    # 21일
```

### 14.3 제한 설정

```yaml
limits_config:
  index_gateway_shard_size: 3        # 테넌트별 게이트웨이 수
  index_gateway_max_capacity: 1.0    # 테넌트별 최대 용량 비율
  tsdb_max_bytes_per_shard: 0        # 샤드별 최대 바이트
  tsdb_precompute_chunks: false      # 프리컴퓨트 청크 활성화
```

---

## 정리

인덱스 게이트웨이는 Loki의 쿼리 성능을 근본적으로 개선하는 핵심 인프라 컴포넌트다:

1. **Querier 메모리 오프로드**: 인덱스 캐시를 게이트웨이에 집중하여 Querier 메모리 사용량 대폭 감소
2. **인덱스 캐시 집중**: 중앙화된 캐시로 적중률 향상, 콜드 스타트 문제 해결
3. **블룸 필터 통합**: 인덱스 조회 결과를 블룸 필터로 추가 필터링하여 I/O 절감
4. **바운디드 샤딩**: 데이터 볼륨 기반의 지능적 쿼리 분할
5. **멀티 피리어드 라우팅**: 스토리지 마이그레이션을 투명하게 지원
6. **유연한 분배**: Ring/Simple 모드, ShuffleShard, Jump Hash, 시간 기반 샤딩

| 구성요소 | 소스 경로 | 역할 |
|---------|----------|------|
| Gateway | `pkg/indexgateway/gateway.go` | 인덱스 게이트웨이 서버 |
| Config | `pkg/indexgateway/config.go` | 서버 설정 |
| GatewayClient | `pkg/indexgateway/client.go` | 클라이언트 (연결 풀) |
| ShuffleSharding | `pkg/indexgateway/shufflesharding.go` | 테넌트별 분배 전략 |
| ServerInterceptors | `pkg/indexgateway/grpc.go` | gRPC 미들웨어 |
| Metrics | `pkg/indexgateway/metrics.go` | 관측 메트릭 |
