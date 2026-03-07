# 11. Querier 쿼리 실행기 Deep-Dive

## 목차

1. [개요](#1-개요)
2. [SingleTenantQuerier 구조체](#2-singletenantquerier-구조체)
3. [Config 설정 체계](#3-config-설정-체계)
4. [SelectLogs / SelectSamples: 듀얼 소스 쿼리 전략](#4-selectlogs--selectsamples-듀얼-소스-쿼리-전략)
5. [buildQueryIntervals: Ingester vs Store 시간 분할](#5-buildqueryintervals-ingester-vs-store-시간-분할)
6. [Query Frontend](#6-query-frontend)
7. [Query Scheduler: 공정 큐와 워커 관리](#7-query-scheduler-공정-큐와-워커-관리)
8. [Frontend-Scheduler-Querier 연결 모델](#8-frontend-scheduler-querier-연결-모델)
9. [Iterator 계층 구조](#9-iterator-계층-구조)
10. [쿼리 최적화 전략](#10-쿼리-최적화-전략)
11. [멀티 테넌트 쿼리](#11-멀티-테넌트-쿼리)
12. [메트릭 및 모니터링](#12-메트릭-및-모니터링)
13. [설계 결정 분석](#13-설계-결정-분석)

---

## 1. 개요

Loki의 Querier는 로그 데이터를 실시간(Ingester)과 장기 저장소(Store) 양쪽에서 통합 조회하는 핵심 컴포넌트이다. 단순한 데이터 조회를 넘어, 시간 기반 쿼리 분할, Iterator 기반 병합, 중복 제거, 캐싱, 쿼리 샤딩 등 복합적인 최적화 전략을 수행한다.

```
소스 위치:
- pkg/querier/querier.go          -- SingleTenantQuerier, Querier 인터페이스
- pkg/querier/intervals.go        -- QueryInterval, 시간 분할 로직
- pkg/querier/ingester_querier.go -- IngesterQuerier (Ingester 통신)
- pkg/querier/multi_tenant_querier.go -- 멀티 테넌트 쿼리
- pkg/iter/iterator.go            -- EntryIterator, SampleIterator 인터페이스
- pkg/iter/entry_iterator.go      -- MergeEntryIterator 구현
- pkg/iter/sample_iterator.go     -- MergeSampleIterator 구현
- pkg/querier/queryrange/         -- Query Frontend 미들웨어
- pkg/querier/worker/             -- Querier Worker
```

### 아키텍처 위치

```
┌──────────────────────────────────────────────────────────────┐
│                       Client (Grafana)                       │
└──────────────────────┬───────────────────────────────────────┘
                       │ LogQL 쿼리
                       ▼
┌──────────────────────────────────────────────────────────────┐
│                    Query Frontend                            │
│  ┌────────────┐ ┌──────────────┐ ┌────────────────────────┐ │
│  │ 시간 분할  │ │ 쿼리 샤딩    │ │ 결과 캐싱              │ │
│  └────────────┘ └──────────────┘ └────────────────────────┘ │
└──────────────────────┬───────────────────────────────────────┘
                       │ 분할된 서브쿼리
                       ▼
┌──────────────────────────────────────────────────────────────┐
│                   Query Scheduler                            │
│  ┌────────────────────────────────────────────────────────┐  │
│  │ 테넌트별 공정 큐 (Round-Robin)                         │  │
│  └────────────────────────────────────────────────────────┘  │
└──────────────────────┬───────────────────────────────────────┘
                       │ 작업 분배
                       ▼
┌──────────────────────────────────────────────────────────────┐
│                      Querier                                 │
│  ┌────────────────────────┐  ┌───────────────────────────┐  │
│  │   IngesterQuerier      │  │       Store               │  │
│  │  (최근 데이터, gRPC)   │  │  (장기 데이터, Object)    │  │
│  └────────────┬───────────┘  └────────────┬──────────────┘  │
│               │                           │                  │
│               └───────────┬───────────────┘                  │
│                           ▼                                  │
│              ┌──────────────────────┐                        │
│              │  MergeIterator       │                        │
│              │  (중복 제거 + 정렬)  │                        │
│              └──────────────────────┘                        │
└──────────────────────────────────────────────────────────────┘
```

---

## 2. SingleTenantQuerier 구조체

### 핵심 구조체 정의

`SingleTenantQuerier`는 단일 테넌트 쿼리 처리의 핵심 구현체이다.

```go
// 소스: pkg/querier/querier.go:128-136
type SingleTenantQuerier struct {
    cfg             Config
    store           Store
    limits          querier_limits.Limits
    ingesterQuerier *IngesterQuerier
    patternQuerier  pattern.PatterQuerier
    deleteGetter    deletion.DeleteGetter
    logger          log.Logger
}
```

### 필드별 역할

| 필드 | 타입 | 역할 |
|------|------|------|
| `cfg` | `Config` | 쿼리어 설정 (타임아웃, 동시성, 모드 등) |
| `store` | `Store` | 장기 저장소 인터페이스 (Object Store 기반) |
| `limits` | `querier_limits.Limits` | 테넌트별 쿼리 제한 (시간 범위, 동시성 등) |
| `ingesterQuerier` | `*IngesterQuerier` | Ingester gRPC 통신 클라이언트 |
| `patternQuerier` | `pattern.PatterQuerier` | 패턴 쿼리어 (패턴 감지 기능) |
| `deleteGetter` | `deletion.DeleteGetter` | 삭제 요청 필터 (삭제된 데이터 제외) |
| `logger` | `log.Logger` | 로깅 |

### Querier 인터페이스

```go
// 소스: pkg/querier/querier.go:96-107
type Querier interface {
    logql.Querier
    Label(ctx context.Context, req *logproto.LabelRequest) (*logproto.LabelResponse, error)
    Series(ctx context.Context, req *logproto.SeriesRequest) (*logproto.SeriesResponse, error)
    IndexStats(ctx context.Context, req *loghttp.RangeQuery) (*stats.Stats, error)
    IndexShards(ctx context.Context, req *loghttp.RangeQuery, targetBytesPerShard uint64) (*logproto.ShardsResponse, error)
    Volume(ctx context.Context, req *logproto.VolumeRequest) (*logproto.VolumeResponse, error)
    DetectedFields(ctx context.Context, req *logproto.DetectedFieldsRequest) (*logproto.DetectedFieldsResponse, error)
    Patterns(ctx context.Context, req *logproto.QueryPatternsRequest) (*logproto.QueryPatternsResponse, error)
    DetectedLabels(ctx context.Context, req *logproto.DetectedLabelsRequest) (*logproto.DetectedLabelsResponse, error)
    WithPatternQuerier(patternQuerier pattern.PatterQuerier)
}
```

이 인터페이스는 `logql.Querier`를 임베딩하여 `SelectLogs`와 `SelectSamples`를 포함하며, 라벨 조회, 시리즈 조회, 인덱스 통계, 볼륨 분석 등 다양한 쿼리 기능을 정의한다.

### Store 인터페이스

```go
// 소스: pkg/querier/querier.go:110-125
type Store interface {
    SelectSamples(ctx context.Context, req logql.SelectSampleParams) (iter.SampleIterator, error)
    SelectLogs(ctx context.Context, req logql.SelectLogParams) (iter.EntryIterator, error)
    SelectSeries(ctx context.Context, req logql.SelectLogParams) ([]logproto.SeriesIdentifier, error)
    LabelValuesForMetricName(ctx context.Context, userID string, from, through model.Time, metricName string, labelName string, matchers ...*labels.Matcher) ([]string, error)
    LabelNamesForMetricName(ctx context.Context, userID string, from, through model.Time, metricName string, matchers ...*labels.Matcher) ([]string, error)
    Stats(ctx context.Context, userID string, from, through model.Time, matchers ...*labels.Matcher) (*stats.Stats, error)
    Volume(ctx context.Context, userID string, from, through model.Time, limit int32, targetLabels []string, aggregateBy string, matchers ...*labels.Matcher) (*logproto.VolumeResponse, error)
    GetShards(ctx context.Context, userID string, from, through model.Time, targetBytesPerShard uint64, predicate chunk.Predicate) (*logproto.ShardsResponse, error)
}
```

---

## 3. Config 설정 체계

### 주요 설정 필드

```go
// 소스: pkg/querier/querier.go:53-67
type Config struct {
    TailMaxDuration           time.Duration    `yaml:"tail_max_duration"`
    ExtraQueryDelay           time.Duration    `yaml:"extra_query_delay,omitempty"`
    QueryIngestersWithin      time.Duration    `yaml:"query_ingesters_within,omitempty"`
    Engine                    logql.EngineOpts `yaml:"engine,omitempty"`
    MaxConcurrent             int              `yaml:"max_concurrent"`
    QueryStoreOnly            bool             `yaml:"query_store_only"`
    QueryIngesterOnly         bool             `yaml:"query_ingester_only"`
    MultiTenantQueriesEnabled bool             `yaml:"multi_tenant_queries_enabled"`
    PerRequestLimitsEnabled   bool             `yaml:"per_request_limits_enabled"`
    QueryPartitionIngesters   bool             `yaml:"query_partition_ingesters"`

    IngesterQueryStoreMaxLookback time.Duration `yaml:"-"`
    QueryPatternIngestersWithin   time.Duration `yaml:"-"`
}
```

### 설정 상세 설명

| 설정 | 기본값 | 설명 |
|------|--------|------|
| `TailMaxDuration` | 1h | 라이브 테일링 최대 지속 시간 |
| `ExtraQueryDelay` | 0 | 최소 성공 요청 이후 추가 대기 시간 |
| `QueryIngestersWithin` | 3h | 이 시간 범위 내의 쿼리만 Ingester로 전송 |
| `MaxConcurrent` | 4 | 동시 처리 가능한 최대 쿼리 수 |
| `QueryStoreOnly` | false | Store만 쿼리 (Ingester 무시) |
| `QueryIngesterOnly` | false | Ingester만 쿼리 (Store 무시) |
| `MultiTenantQueriesEnabled` | false | 멀티 테넌트 쿼리 허용 |
| `PerRequestLimitsEnabled` | false | 요청별 한도 적용 |
| `QueryPartitionIngesters` | false | 파티션 Ingester로 라우팅 (실험적) |

### 상호 배타적 검증

```go
// 소스: pkg/querier/querier.go:88-93
func (cfg *Config) Validate() error {
    if cfg.QueryStoreOnly && cfg.QueryIngesterOnly {
        return errors.New("querier.query_store_only and querier.query_ingester_only cannot both be true")
    }
    return nil
}
```

`QueryStoreOnly`과 `QueryIngesterOnly`는 동시에 활성화될 수 없다. 이 검증은 논리적으로 모순되는 설정을 방지한다.

---

## 4. SelectLogs / SelectSamples: 듀얼 소스 쿼리 전략

### SelectLogs 흐름

`SelectLogs`는 로그 엔트리를 조회하는 핵심 메서드이다.

```go
// 소스: pkg/querier/querier.go:153-217
func (q *SingleTenantQuerier) SelectLogs(ctx context.Context, params logql.SelectLogParams) (iter.EntryIterator, error) {
    ctx = NewPartitionContext(ctx)
    var err error
    params.Start, params.End, err = querier_limits.ValidateQueryRequest(ctx, params, q.limits)
    if err != nil {
        return nil, err
    }
    // ... 삭제 필터 로드 ...
    params.Deletes, err = deletion.DeletesForUserQuery(ctx, params.Start, params.End, q.deleteGetter)

    ingesterQueryInterval, storeQueryInterval := q.buildQueryIntervals(params.Start, params.End)

    iters := []iter.EntryIterator{}
    if !q.cfg.QueryStoreOnly && ingesterQueryInterval != nil {
        // Ingester 쿼리
        ingesterIters, err := q.ingesterQuerier.SelectLogs(ctx, newParams)
        iters = append(iters, ingesterIters...)
    }
    if !q.cfg.QueryIngesterOnly && storeQueryInterval != nil {
        // Store 쿼리
        storeIter, err := q.store.SelectLogs(ctx, params)
        iters = append(iters, storeIter)
    }
    if len(iters) == 1 {
        return iters[0], nil
    }
    return iter.NewMergeEntryIterator(ctx, iters, params.Direction), nil
}
```

### SelectLogs 실행 흐름도

```
SelectLogs(params) 호출
        │
        ▼
┌───────────────────────┐
│  PartitionContext     │  -- Ingester 파티션 추적 컨텍스트 생성
│  생성                 │
└───────────┬───────────┘
            ▼
┌───────────────────────┐
│  ValidateQueryRequest │  -- 시간 범위 검증, 한도 적용
└───────────┬───────────┘
            ▼
┌───────────────────────┐
│  AggregatedMetric     │  -- 집계 메트릭 쿼리 검증
│  Query 검증           │
└───────────┬───────────┘
            ▼
┌───────────────────────┐
│  삭제 요청 필터 로드  │  -- DeletesForUserQuery
└───────────┬───────────┘
            ▼
┌───────────────────────┐
│  buildQueryIntervals  │  -- Ingester/Store 시간 구간 결정
└───────────┬───────────┘
            ▼
    ┌───────┴───────┐
    │               │
    ▼               ▼
┌────────┐   ┌──────────┐
│Ingester│   │  Store   │
│ 쿼리   │   │  쿼리    │
└───┬────┘   └────┬─────┘
    │             │
    └──────┬──────┘
           ▼
┌──────────────────────┐
│ MergeEntryIterator   │  -- 중복 제거 + 정렬 병합
└──────────────────────┘
```

### SelectSamples 흐름

`SelectSamples`는 메트릭 쿼리(`rate`, `count_over_time` 등)에 사용되는 샘플 데이터를 조회한다. 구조는 `SelectLogs`와 동일하되, 반환 타입이 `SampleIterator`이다.

```go
// 소스: pkg/querier/querier.go:219-275
func (q *SingleTenantQuerier) SelectSamples(ctx context.Context, params logql.SelectSampleParams) (iter.SampleIterator, error) {
    ctx = NewPartitionContext(ctx)
    // ... 검증 및 삭제 필터 ...
    ingesterQueryInterval, storeQueryInterval := q.buildQueryIntervals(params.Start, params.End)

    iters := []iter.SampleIterator{}
    if !q.cfg.QueryStoreOnly && ingesterQueryInterval != nil {
        ingesterIters, err := q.ingesterQuerier.SelectSample(ctx, newParams)
        iters = append(iters, ingesterIters...)
    }
    if !q.cfg.QueryIngesterOnly && storeQueryInterval != nil {
        storeIter, err := q.store.SelectSamples(ctx, params)
        iters = append(iters, storeIter)
    }
    return iter.NewMergeSampleIterator(ctx, iters), nil
}
```

### 핵심 차이점: SelectLogs vs SelectSamples

| 항목 | SelectLogs | SelectSamples |
|------|-----------|---------------|
| 반환 타입 | `iter.EntryIterator` | `iter.SampleIterator` |
| 데이터 타입 | `logproto.Entry` (로그 라인) | `logproto.Sample` (수치 값) |
| 병합 방식 | `NewMergeEntryIterator` (중복 제거) | `NewMergeSampleIterator` |
| 정렬 방향 | `params.Direction` (FORWARD/BACKWARD) | 항상 타임스탬프 오름차순 |
| 용도 | `{app="foo"}` 로그 조회 | `rate({app="foo"}[5m])` 메트릭 쿼리 |

---

## 5. buildQueryIntervals: Ingester vs Store 시간 분할

### 시간 분할의 필요성

Loki는 최근 데이터는 Ingester의 메모리에, 과거 데이터는 Object Store에 저장한다. 쿼리 시 두 소스의 시간 범위가 겹치지 않도록 분할하는 것이 핵심이다.

### QueryInterval 구조체

```go
// 소스: pkg/querier/intervals.go:5-7
type QueryInterval struct {
    start, end time.Time
}
```

### BuildQueryIntervalsWithLookback 핵심 로직

```go
// 소스: pkg/querier/intervals.go:9-79
func BuildQueryIntervalsWithLookback(cfg Config, queryStart, queryEnd time.Time, queryIngestersWithin time.Duration) (*QueryInterval, *QueryInterval) {
    limitQueryInterval := cfg.IngesterQueryStoreMaxLookback != 0
    ingesterMLB := calculateIngesterMaxLookbackPeriod(cfg, queryIngestersWithin)

    // Case 1: ingesterMLB == -1 → Ingester에서 전체 기간 쿼리
    if ingesterMLB == -1 {
        i := &QueryInterval{start: queryStart, end: queryEnd}
        if limitQueryInterval {
            return i, nil  // Ingester만 쿼리
        }
        return i, i  // 양쪽 모두 전체 범위
    }

    // Case 2: 쿼리 범위가 Ingester 범위 밖
    ingesterQueryWithinRange := isWithinIngesterMaxLookbackPeriod(ingesterMLB, queryEnd)
    if !ingesterQueryWithinRange {
        return nil, &QueryInterval{start: queryStart, end: queryEnd}  // Store만 쿼리
    }

    // Case 3: 겹침이 있고, 제한 없음
    if !limitQueryInterval {
        i := &QueryInterval{start: queryStart, end: queryEnd}
        return i, i
    }

    // Case 4: 시간 분할이 필요한 경우
    ingesterOldestStartTime := time.Now().Add(-ingesterMLB)
    if ingesterOldestStartTime.Before(queryStart) {
        return &QueryInterval{start: queryStart, end: queryEnd}, nil
    }

    // Ingester: [ingesterOldestStartTime, queryEnd]
    // Store:    [queryStart, ingesterOldestStartTime]
    ingesterQueryInterval := &QueryInterval{start: ingesterOldestStartTime, end: queryEnd}
    storeQueryInterval := &QueryInterval{start: queryStart, end: ingesterOldestStartTime}

    if storeQueryInterval.start.After(storeQueryInterval.end) {
        storeQueryInterval = nil
    }
    return ingesterQueryInterval, storeQueryInterval
}
```

### 시간 분할 시나리오

```
시나리오 1: IngesterQueryStoreMaxLookback = 0, QueryIngestersWithin = 0
─────────────────────────────────────────────────────
│           전체 쿼리 범위                          │
│  Ingester: [start, end]                           │
│  Store:    [start, end]                           │
─────────────────────────────────────────────────────

시나리오 2: QueryIngestersWithin = 3h (기본값)
─────────────────────────────────────────────────────
│  queryStart        now-3h              queryEnd   │
│  ├────────────────┤├────────────────────┤         │
│  Store 전용 구간    양쪽 쿼리 구간               │
│                                                    │
│  Ingester: [start, end] (겹침 허용)               │
│  Store:    [start, end] (겹침 허용)               │
─────────────────────────────────────────────────────

시나리오 3: IngesterQueryStoreMaxLookback = 3h (시간 분할)
─────────────────────────────────────────────────────
│  queryStart        now-3h              queryEnd   │
│  ├────────────────┤├────────────────────┤         │
│  Store 전용 구간    Ingester 전용 구간            │
│                                                    │
│  Ingester: [now-3h, end]                          │
│  Store:    [start, now-3h]                        │
─────────────────────────────────────────────────────

시나리오 4: 쿼리 범위가 Ingester 범위 밖
─────────────────────────────────────────────────────
│  queryStart  queryEnd      now-3h                 │
│  ├────────────┤            │                      │
│  Store 전용 (Ingester 범위 밖)                    │
│                                                    │
│  Ingester: nil                                    │
│  Store:    [start, end]                           │
─────────────────────────────────────────────────────
```

### 왜 이런 설계인가?

1. **데이터 신선도**: Ingester는 아직 플러시되지 않은 최신 데이터를 보유한다. `QueryIngestersWithin` 시간 내의 데이터는 Store에 없을 수 있다.
2. **중복 방지**: `IngesterQueryStoreMaxLookback` 설정 시, 같은 데이터를 양쪽에서 읽지 않아 I/O를 절약한다.
3. **유연성**: 운영자가 `QueryStoreOnly` 또는 `QueryIngesterOnly` 플래그로 한쪽만 쿼리할 수 있어 장애 상황에서 유연하게 대응 가능하다.

---

## 6. Query Frontend

Query Frontend는 클라이언트 요청을 받아 최적화된 서브쿼리로 분할하고, 결과를 캐싱하는 프록시 컴포넌트이다.

### 핵심 미들웨어 체인

```
소스 위치: pkg/querier/queryrange/
```

```
클라이언트 요청
      │
      ▼
┌─────────────────────────────┐
│  StatsCollector Middleware  │ -- 쿼리 통계 수집
└──────────────┬──────────────┘
               ▼
┌─────────────────────────────┐
│  Limits Middleware          │ -- 쿼리 시간 범위, 라인 수 제한 검증
└──────────────┬──────────────┘
               ▼
┌─────────────────────────────┐
│  Split by Time Middleware   │ -- 시간 단위로 쿼리 분할 (예: 1h 단위)
│  (시간 분할)                │
└──────────────┬──────────────┘
               ▼
┌─────────────────────────────┐
│  Shard Middleware           │ -- 인덱스 기반 쿼리 샤딩
│  (쿼리 샤딩)                │   (데이터 양에 따른 병렬 처리)
└──────────────┬──────────────┘
               ▼
┌─────────────────────────────┐
│  Results Cache Middleware   │ -- 이전 결과 캐시 확인/저장
│  (결과 캐싱)                │
└──────────────┬──────────────┘
               ▼
┌─────────────────────────────┐
│  Retry Middleware           │ -- 실패 시 재시도
└──────────────┬──────────────┘
               ▼
       Query Scheduler 또는
       직접 Querier 호출
```

### 시간 분할 (Split by Time)

시간 분할 미들웨어는 장시간 쿼리를 작은 시간 단위(예: 1시간)로 나누어 병렬 실행한다.

```
원본 쿼리: rate({app="foo"}[24h])  시간 범위 = 24시간

시간 분할 (1h 단위):
┌──────┬──────┬──────┬──────┬─────────┬──────┐
│ 0-1h │ 1-2h │ 2-3h │ 3-4h │  ...    │23-24h│
└──────┴──────┴──────┴──────┴─────────┴──────┘
   │      │      │      │               │
   ▼      ▼      ▼      ▼               ▼
   서브쿼리 1,2,3,4,...24 → 병렬 실행 → 결과 병합
```

### 쿼리 샤딩

쿼리 샤딩은 데이터를 fingerprint 범위별로 분할하여 여러 Querier에서 병렬 처리한다.

```
원본 쿼리: count_over_time({app="foo"}[1h])

IndexShards 호출 → targetBytesPerShard = 256MB
└→ 반환: [shard_0_of_4, shard_1_of_4, shard_2_of_4, shard_3_of_4]

샤딩 후:
┌──────────────┬──────────────┬──────────────┬──────────────┐
│   Shard 0/4  │   Shard 1/4  │   Shard 2/4  │   Shard 3/4  │
│  FP: 0-25%   │  FP: 25-50%  │  FP: 50-75%  │  FP: 75-100% │
└──────┬───────┴──────┬───────┴──────┬───────┴──────┬───────┘
       │              │              │              │
       ▼              ▼              ▼              ▼
    Querier 1      Querier 2      Querier 3      Querier 4
```

### 결과 캐싱

```
캐시 키 구조:
tenant_id + query_hash + time_range_hash + shard_info

캐시 적중 시:
┌────────────────────────────────────────┐
│  쿼리 시간 범위: 09:00 ~ 12:00        │
│                                        │
│  ┌──────────┐ ┌──────────┐ ┌────────┐ │
│  │ 09-10    │ │ 10-11    │ │ 11-12  │ │
│  │ 캐시 HIT │ │ 캐시 HIT │ │ MISS   │ │
│  └──────────┘ └──────────┘ └────────┘ │
│                                  │     │
│                                  ▼     │
│                          Querier 실행  │
│                          11:00-12:00   │
└────────────────────────────────────────┘
```

---

## 7. Query Scheduler: 공정 큐와 워커 관리

### Query Scheduler의 역할

Query Scheduler는 Query Frontend와 Querier 사이에서 작업을 공정하게 분배하는 독립 컴포넌트이다.

```
┌──────────────────────────────────────────────────────┐
│                 Query Scheduler                       │
│                                                       │
│  ┌─────────────────────────────────────────────────┐ │
│  │            테넌트별 공정 큐                       │ │
│  │                                                  │ │
│  │  Tenant A: [q1] [q2] [q3]                       │ │
│  │  Tenant B: [q4]                                  │ │
│  │  Tenant C: [q5] [q6]                            │ │
│  │                                                  │ │
│  │  Round-Robin 스케줄링:                           │ │
│  │  A → B → C → A → B → C → ...                   │ │
│  └─────────────────────────────────────────────────┘ │
│                                                       │
│  ┌─────────────────────────────────────────────────┐ │
│  │          워커 레지스트리                          │ │
│  │  Worker 1: 활성, 처리중 쿼리 2개                 │ │
│  │  Worker 2: 활성, 대기중                          │ │
│  │  Worker 3: 비활성 (연결 해제)                    │ │
│  └─────────────────────────────────────────────────┘ │
└──────────────────────────────────────────────────────┘
```

### 공정 큐 동작 원리

1. **테넌트 격리**: 각 테넌트의 쿼리는 별도 큐에 저장되어 한 테넌트가 다른 테넌트를 압도하지 못한다.
2. **Round-Robin**: 스케줄러는 테넌트 간 Round-Robin으로 작업을 분배한다.
3. **최대 대기열 제한**: 테넌트별 최대 대기 쿼리 수를 설정하여 과도한 부하를 방지한다.

### 워커 등록/해제

```
Querier 시작 → Worker 스레드 생성
                    │
                    ▼
         ┌─────────────────────┐
         │ Scheduler에 gRPC    │
         │ 연결 및 워커 등록   │
         └─────────┬───────────┘
                   │
                   ▼
         ┌─────────────────────┐
         │ QuerierLoop() 시작  │ -- 무한 루프로 작업 수신
         │ (양방향 스트리밍)   │
         └─────────┬───────────┘
                   │
              작업 수신 루프
                   │
         ┌─────────┴───────────┐
         │ 작업 수신            │
         │  → 쿼리 실행         │
         │  → 결과 반환         │
         │  → 다음 작업 대기    │
         └─────────────────────┘
                   │
         Querier 종료 시
                   │
                   ▼
         ┌─────────────────────┐
         │ Scheduler에서       │
         │ 워커 등록 해제       │
         └─────────────────────┘
```

---

## 8. Frontend-Scheduler-Querier 연결 모델

### 3-Tier 아키텍처

Loki는 Query Frontend, Query Scheduler, Querier를 분리하여 수평 확장과 공정한 작업 분배를 달성한다.

```
┌────────────┐   ┌────────────┐   ┌────────────┐
│ Frontend 1 │   │ Frontend 2 │   │ Frontend 3 │
└──────┬─────┘   └──────┬─────┘   └──────┬─────┘
       │                │                │
       │    gRPC 연결   │                │
       └────────┬───────┘────────────────┘
                │
                ▼
       ┌─────────────────┐
       │  Scheduler      │
       │  (공정 큐)      │
       └────────┬────────┘
                │
       ┌────────┼────────┐
       │        │        │
       ▼        ▼        ▼
  ┌─────────┐ ┌─────────┐ ┌─────────┐
  │Querier 1│ │Querier 2│ │Querier 3│
  └─────────┘ └─────────┘ └─────────┘
```

### 통신 흐름 상세

```
1. Frontend → Scheduler: EnqueueRequest(query)
   - 테넌트 ID + 쿼리 파라미터 전송
   - Scheduler가 테넌트별 큐에 적재

2. Querier → Scheduler: QuerierLoop() 양방향 스트림
   - Querier가 Scheduler에 연결
   - 작업이 없으면 대기 (블로킹)
   - 작업이 있으면 Scheduler가 푸시

3. Querier → Scheduler: 결과 반환
   - 쿼리 실행 완료 후 결과를 스트림으로 반환
   - Scheduler가 결과를 대기 중인 Frontend에 전달

4. Scheduler → Frontend: 결과 전달
   - Frontend가 결과를 조합하여 클라이언트에 응답
```

### 연결 모델 비교

| 모드 | 구성 | 장점 | 단점 |
|------|------|------|------|
| Frontend 직접 연결 | Frontend → Querier | 간단한 구성 | 부하 분산 불균형 |
| Scheduler 사용 | Frontend → Scheduler → Querier | 공정 큐, 테넌트 격리 | 추가 컴포넌트 |
| Scheduler + DNS | DNS SRV 기반 디스커버리 | 동적 확장 | DNS 의존 |

---

## 9. Iterator 계층 구조

### Iterator 인터페이스 정의

```go
// 소스: pkg/iter/iterator.go:14-21
type StreamIterator[T logprotoType] interface {
    v2.CloseIterator[T]
    Labels() string        // 스트림 라벨 반환
    StreamHash() uint64    // 원본 스트림 해시 반환
}

type EntryIterator StreamIterator[logproto.Entry]
type SampleIterator StreamIterator[logproto.Sample]
```

### Iterator 계층도

```
                     ┌─────────────────────┐
                     │   EntryIterator     │  (인터페이스)
                     │   SampleIterator    │
                     └──────────┬──────────┘
                                │
            ┌───────────────────┼───────────────────┐
            │                   │                   │
            ▼                   ▼                   ▼
   ┌──────────────┐   ┌──────────────┐   ┌──────────────────┐
   │ streamIter   │   │ mergeEntry   │   │ noOpIterator     │
   │              │   │   Iterator   │   │                  │
   │ (단일 스트림)│   │ (병합+중복   │   │ (빈 결과)        │
   │              │   │   제거)      │   │                  │
   └──────────────┘   └──────┬───────┘   └──────────────────┘
                             │
                    ┌────────┴────────┐
                    │                 │
                    ▼                 ▼
          ┌──────────────┐   ┌──────────────┐
          │ Loser Tree   │   │  fillBuffer  │
          │ (토너먼트    │   │  (중복 제거  │
          │  정렬 트리)  │   │   버퍼링)    │
          └──────────────┘   └──────────────┘
```

### MergeEntryIterator: Loser Tree 기반 정렬

```go
// 소스: pkg/iter/entry_iterator.go:67-88
type mergeEntryIterator struct {
    tree  *loser.Tree[sortFields, EntryIterator]
    stats *stats.Context
    buffer    []entryWithLabels
    currEntry entryWithLabels
    errs      []error
}

func NewMergeEntryIterator(ctx context.Context, is []EntryIterator, direction logproto.Direction) MergeEntryIterator {
    maxVal, less := treeLess(direction)
    result := &mergeEntryIterator{stats: stats.FromContext(ctx)}
    result.tree = loser.New(is, maxVal, sortFieldsAt, less, result.closeEntry)
    result.buffer = make([]entryWithLabels, 0, len(is))
    return result
}
```

### Loser Tree 동작 원리

Loser Tree는 K-way merge를 위한 효율적인 자료구조로, O(log K)의 시간 복잡도로 K개 정렬된 입력에서 다음 최소/최대 값을 찾는다.

```
                     Winner
                      │
              ┌───────┴───────┐
              │ Loser: Iter3  │   ← 패자 기록
              ├───────────────┤
         ┌────┴────┐    ┌────┴────┐
         │Loser:   │    │Loser:   │
         │ Iter2   │    │ Iter4   │
         ├─────────┤    ├─────────┤
      ┌──┴──┐  ┌──┴──┐ ┌──┴──┐ ┌──┴──┐
      │Iter1│  │Iter2│ │Iter3│ │Iter4│
      │ts:1 │  │ts:3 │ │ts:2 │ │ts:5 │
      └─────┘  └─────┘ └─────┘ └─────┘

    Winner = Iter1 (ts:1, FORWARD 방향 기준)

    Next() 호출 시:
    1. Iter1에서 다음 값을 읽음
    2. 트리를 O(log K)로 재조정
    3. 새로운 Winner 결정
```

### 중복 제거 로직

```go
// 소스: pkg/iter/entry_iterator.go:116-155
func (i *mergeEntryIterator) fillBuffer() {
    if !i.tree.Next() {
        return
    }
    for {
        next := i.tree.Winner()
        entry := next.At()
        i.buffer = append(i.buffer, entryWithLabels{
            Entry:      entry,
            labels:     next.Labels(),
            streamHash: next.StreamHash(),
        })
        // 같은 타임스탬프 + 같은 스트림 해시면 중복 가능
        if len(i.buffer) > 1 &&
            (i.buffer[0].streamHash != next.StreamHash() ||
                !i.buffer[0].Timestamp.Equal(entry.Timestamp)) {
            break
        }
        // 같은 타임스탬프의 이전 항목과 라인이 같으면 중복 제거
        var dupe bool
        for _, t := range previous {
            if t.Line == entry.Line {
                i.stats.AddDuplicates(1)
                dupe = true
                break
            }
        }
        if dupe {
            i.buffer = previous
        }
        if !i.tree.Next() {
            break
        }
    }
}
```

중복 제거 조건:
- 동일한 `StreamHash` (같은 스트림)
- 동일한 `Timestamp`
- 동일한 `Line` 내용

이 세 조건이 모두 충족되면 해당 엔트리를 중복으로 판단하고 제거한다.

### SampleIterator Heap

```go
// 소스: pkg/iter/sample_iterator.go:115-148
type SampleIteratorHeap struct {
    its []SampleIterator
}

func (h SampleIteratorHeap) Less(i, j int) bool {
    s1, s2 := h.its[i].At(), h.its[j].At()
    if s1.Timestamp == s2.Timestamp {
        if h.its[i].StreamHash() == 0 {
            return h.its[i].Labels() < h.its[j].Labels()
        }
        return h.its[i].StreamHash() < h.its[j].StreamHash()
    }
    return s1.Timestamp < s2.Timestamp
}
```

SampleIterator는 `container/heap`을 사용하여 타임스탬프 기반 정렬을 수행한다. 타임스탬프가 같을 경우 StreamHash로 2차 정렬하여 결정적 순서를 보장한다.

---

## 10. 쿼리 최적화 전략

### 10.1 캐시 히트 최적화

```
전략 1: 시간 정렬(Time-Aligned) 분할
─────────────────────────────────────────
쿼리: 09:17 ~ 11:43

시간 정렬 없이:
  [09:17─────────────────────11:43]  ← 캐시 미스 확률 높음

시간 정렬 분할 (1h 단위):
  [09:00─10:00] [10:00─11:00] [11:00─12:00]
       │              │             │
    앞뒤 트림       캐시 적중      앞뒤 트림
    가능성          가능성 높음    가능성

전략 2: 단계적 캐시
  L1: 인메모리 캐시 (Querier 로컬)
  L2: Memcached/Redis 캐시 (공유)
  L3: Object Store 직접 읽기
```

### 10.2 병렬화 전략

```
쿼리 병렬화 계층:

Level 1: 시간 분할 병렬화
  24시간 쿼리 → 24개 서브쿼리 → 최대 N개 동시 실행

Level 2: 샤드 병렬화
  각 서브쿼리 → 4개 샤드 → 4개 Querier에서 동시 실행

Level 3: Ingester/Store 병렬화
  각 샤드 쿼리 → Ingester 쿼리 + Store 쿼리 동시 실행

총 병렬도: 시간 분할 * 샤드 수 * 소스 수
예: 24 * 4 * 2 = 192 (이론적 최대, 실제로는 동시성 제한 적용)
```

### 10.3 인덱스 프리페치

```go
// IndexShards를 통한 샤드 정보 프리페치
func (q *SingleTenantQuerier) IndexShards(ctx context.Context, req *loghttp.RangeQuery, targetBytesPerShard uint64) (*logproto.ShardsResponse, error) {
    // 소스: pkg/querier/querier.go:559-596
    p, err := indexgateway.ExtractShardRequestMatchersAndAST(req.Query)
    shards, err := q.store.GetShards(ctx, userID, from, through, targetBytesPerShard, p)
    return shards, nil
}
```

인덱스 프리페치 흐름:
1. Frontend가 쿼리 파싱 후 `IndexShards` 호출
2. 인덱스에서 데이터 분포 통계 조회
3. `targetBytesPerShard` 기준으로 최적 샤드 수 결정
4. 각 샤드를 별도 Querier에 분배

### 10.4 삭제 필터 최적화

```go
// 소스: pkg/querier/querier.go:171-173
params.Deletes, err = deletion.DeletesForUserQuery(ctx, params.Start, params.End, q.deleteGetter)
```

삭제 요청이 있는 경우, 쿼리 시작 전에 해당 시간 범위의 삭제 필터를 미리 로드한다. 이렇게 하면 Store/Ingester에서 반환된 데이터 중 삭제 대상을 효율적으로 필터링할 수 있다.

---

## 11. 멀티 테넌트 쿼리

### MultiTenantQuerier

`MultiTenantQueriesEnabled` 설정이 활성화되면, 단일 쿼리로 여러 테넌트의 데이터를 조회할 수 있다.

```
소스: pkg/querier/multi_tenant_querier.go
```

```
멀티 테넌트 쿼리 흐름:

요청: X-Scope-OrgID: "tenant-a|tenant-b|tenant-c"
          │
          ▼
┌─────────────────────────────────────┐
│ MultiTenantQuerier                  │
│                                     │
│  for each tenantID:                 │
│    ├─ tenant-a → SingleTenantQuerier│
│    ├─ tenant-b → SingleTenantQuerier│
│    └─ tenant-c → SingleTenantQuerier│
│                                     │
│  결과 병합 + 테넌트 라벨 추가       │
└─────────────────────────────────────┘
```

각 테넌트의 결과에는 `__loki_tenant__` 라벨이 자동으로 추가되어 결과에서 테넌트를 구분할 수 있다.

---

## 12. 메트릭 및 모니터링

### 핵심 메트릭

| 메트릭 | 타입 | 설명 |
|--------|------|------|
| `loki_querier_tail_active` | Gauge | 활성 테일 연결 수 |
| `loki_query_frontend_queries_total` | Counter | Frontend 쿼리 총 수 |
| `loki_query_frontend_query_duration_seconds` | Histogram | 쿼리 실행 시간 |
| `loki_query_scheduler_queue_length` | Gauge | 스케줄러 큐 길이 |
| `loki_query_scheduler_inflight_requests` | Gauge | 처리 중인 요청 수 |
| `loki_querier_store_chunks_downloaded_total` | Counter | 다운로드된 청크 수 |
| `loki_querier_ingester_append_time_seconds` | Histogram | Ingester 응답 시간 |

### 트레이싱 통합

```go
// 소스: pkg/querier/querier.go:50
var tracer = otel.Tracer("pkg/querier")
```

Loki의 Querier는 OpenTelemetry 트레이서를 사용하여 쿼리 경로의 각 단계를 추적한다.

```go
// SelectLogs에서의 스팬 이벤트 기록
// 소스: pkg/querier/querier.go:189-191
sp.AddEvent("querying ingester", trace.WithAttributes(
    attribute.Stringer("params", newParams),
))
```

### Grafana 대시보드 권장 패널

```
1. 쿼리 처리율 (QPS)
   rate(loki_query_frontend_queries_total[5m])

2. 쿼리 지연 시간 (p99)
   histogram_quantile(0.99, rate(loki_query_frontend_query_duration_seconds_bucket[5m]))

3. 스케줄러 큐 깊이
   loki_query_scheduler_queue_length

4. Ingester vs Store 쿼리 비율
   rate(loki_querier_ingester_queries_total[5m])
   / rate(loki_querier_store_queries_total[5m])

5. 중복 제거된 항목 수
   rate(loki_querier_duplicates_total[5m])
```

---

## 13. 설계 결정 분석

### 왜 듀얼 소스 전략인가?

**문제**: Loki는 최근 데이터를 Ingester의 메모리에 보관하고, 주기적으로 Object Store에 플러시한다. 플러시 전 데이터는 Store에 없다.

**해결**: `buildQueryIntervals`로 시간 범위를 분할하여 각 소스에서 가장 적합한 데이터를 읽는다.

**트레이드오프**:
- (+) 최신 데이터도 즉시 조회 가능
- (+) Store에 부하를 분산
- (-) 겹치는 구간에서 중복 데이터 발생 가능
- (-) 두 소스의 응답 시간이 다를 수 있어 최종 지연 시간 증가

### 왜 Loser Tree인가?

**문제**: K개의 정렬된 Iterator를 병합할 때, 단순 비교는 O(K)의 시간이 소요된다.

**해결**: Loser Tree를 사용하여 O(log K)로 병합. 이는 동시에 많은 스트림을 처리할 때 성능 차이가 크다.

**대안과 비교**:
- Min-Heap: O(log K)이지만, Loser Tree가 캐시 지역성에서 더 효율적
- 순차 병합: O(N*K)로 비효율적
- Loser Tree: O(N * log K)로 최적

### 왜 3-Tier 아키텍처인가?

**문제**: 대규모 멀티 테넌트 환경에서 쿼리 부하가 특정 Querier에 집중될 수 있다.

**해결**: Frontend → Scheduler → Querier 3단계 분리
1. **Frontend**: 쿼리 최적화 (분할, 캐싱) 담당
2. **Scheduler**: 공정한 작업 분배 담당
3. **Querier**: 순수 데이터 조회 담당

**이점**:
- 각 계층의 독립적 수평 확장
- 테넌트 간 공정성 보장
- 장애 격리 (Frontend 장애가 Querier에 영향 없음)

### PartitionContext의 역할

```go
// 소스: pkg/querier/querier.go:154-156
ctx = NewPartitionContext(ctx)
```

`PartitionContext`는 동일 쿼리 내의 연속적인 서브쿼리(SelectLogs, SelectSamples)에서 같은 Ingester를 재사용하도록 보장한다. 이는 Ingester 파티셔닝이 활성화된 환경에서 일관된 결과를 보장하기 위해 필요하다.

---

## 요약

Loki의 Querier는 다음 핵심 설계 원칙을 따른다:

1. **듀얼 소스 전략**: Ingester(실시간)와 Store(장기)를 시간 기반으로 분할하여 최적 소스에서 데이터를 읽는다.
2. **계층적 최적화**: Frontend(시간 분할, 샤딩, 캐싱) → Scheduler(공정 큐) → Querier(데이터 조회) 3단계로 쿼리 처리를 최적화한다.
3. **효율적 병합**: Loser Tree 기반 MergeIterator로 O(log K) 복잡도의 K-way 병합과 중복 제거를 수행한다.
4. **테넌트 격리**: 멀티 테넌트 환경에서 공정 큐와 테넌트별 한도로 자원을 공정하게 분배한다.
5. **유연한 설정**: `QueryStoreOnly`, `QueryIngesterOnly`, `QueryIngestersWithin` 등의 설정으로 운영 환경에 맞게 동작을 조정할 수 있다.
