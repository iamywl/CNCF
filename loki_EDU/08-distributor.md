# 08. Distributor Deep-Dive

## 목차

1. [Distributor 개요](#1-distributor-개요)
2. [Distributor 구조체](#2-distributor-구조체)
3. [Push 흐름 상세](#3-push-흐름-상세)
4. [검증 (Validator)](#4-검증-validator)
5. [레이트 리밋](#5-레이트-리밋)
6. [Ring 라우팅](#6-ring-라우팅)
7. [스트림 샤딩](#7-스트림-샤딩)
8. [Kafka 듀얼 라이트](#8-kafka-듀얼-라이트)
9. [동시성 모델](#9-동시성-모델)
10. [메트릭](#10-메트릭)

---

## 1. Distributor 개요

Distributor는 Loki의 쓰기 경로(write path)에서 첫 번째로 요청을 받는 컴포넌트이다. 클라이언트(Promtail, Grafana Agent 등)로부터 로그 데이터를 수신하여 검증, 전처리, 라우팅을 수행한 뒤 Ingester에 분배한다.

### 핵심 역할

```
┌──────────────┐
│   클라이언트    │  (Promtail, Grafana Agent, etc.)
└──────┬───────┘
       │ Push (HTTP/gRPC)
       ▼
┌──────────────────────────────────────────────────┐
│                  Distributor                       │
│                                                    │
│  1. 검증 (ValidateLabels, ValidateEntry)           │
│  2. 레이트 리밋 (로컬/글로벌)                        │
│  3. 레이블 파싱 및 캐싱                              │
│  4. 스트림 샤딩 (선택적)                             │
│  5. 해시 기반 Ingester 라우팅                        │
│  6. Kafka 듀얼 라이트 (선택적)                       │
│                                                    │
└─────┬───────────────┬──────────────────┬──────────┘
      │               │                  │
      ▼               ▼                  ▼
┌──────────┐   ┌──────────┐       ┌──────────┐
│Ingester 1│   │Ingester 2│  ...  │Ingester N│
└──────────┘   └──────────┘       └──────────┘
                     │
                     ▼ (선택적)
              ┌──────────┐
              │  Kafka    │
              └──────────┘
```

---

## 2. Distributor 구조체

파일: `pkg/distributor/distributor.go`

### 2.1 Config

```go
type Config struct {
    DistributorRing RingConfig `yaml:"ring,omitempty"`
    PushWorkerCount int        `yaml:"push_worker_count"`

    MaxRecvMsgSize      int   `yaml:"max_recv_msg_size"`
    MaxDecompressedSize int64 `yaml:"max_decompressed_size"`

    RateStore            RateStoreConfig    `yaml:"rate_store"`
    WriteFailuresLogging writefailures.Cfg  `yaml:"write_failures_logging"`
    OTLPConfig           push.GlobalOTLPConfig `yaml:"otlp_config"`

    KafkaEnabled    bool `yaml:"kafka_writes_enabled"`
    IngesterEnabled bool `yaml:"ingester_writes_enabled"`

    IngestLimitsEnabled       bool `yaml:"ingest_limits_enabled"`
    IngestLimitsDryRunEnabled bool `yaml:"ingest_limits_dry_run_enabled"`

    KafkaConfig      kafka.Config      `yaml:"-"`
    DataObjTeeConfig DataObjTeeConfig  `yaml:"dataobj_tee"`

    DefaultPolicyStreamMappings validation.PolicyStreamMapping `yaml:"default_policy_stream_mappings"`
}
```

주요 설정 기본값:

| 설정 | 기본값 | 설명 |
|------|--------|------|
| `PushWorkerCount` | 256 | Ingester 푸시 워커 수 |
| `MaxRecvMsgSize` | 100MB | 최대 수신 메시지 크기 |
| `MaxDecompressedSize` | 5000MB | 최대 압축 해제 크기 |
| `IngesterEnabled` | true | Ingester 쓰기 활성화 |
| `KafkaEnabled` | false | Kafka 쓰기 활성화 |

### 2.2 Distributor 구조체

```go
type Distributor struct {
    services.Service

    cfg              Config
    ingesterCfg      ingester.Config
    logger           log.Logger
    clientCfg        ingester_client.Config
    tenantConfigs    *runtime.TenantConfigs
    tenantsRetention *retention.TenantsRetention
    ingestersRing    ring.ReadRing
    validator        *Validator
    ingesterClients  *ring_client.Pool
    tee              Tee

    // 스트림 샤딩 지원
    rateStore    RateStore
    shardTracker *ShardTracker

    // 글로벌 레이트 리밋 지원
    distributorsLifecycler *ring.BasicLifecycler
    distributorsRing       *ring.Ring
    healthyInstancesCount  *atomic.Uint32

    rateLimitStrat string

    // 서브서비스 관리
    subservices        *services.Manager
    subservicesWatcher *services.FailureWatcher

    // 레이트 리밋
    ingestionRateLimiter *limiter.RateLimiter
    labelCache           *lru.Cache[string, labelData]

    // 쓰기 실패 관리
    writeFailuresManager *writefailures.Manager

    // 워커 풀
    ingesterTasks  chan pushIngesterTask
    ingesterTaskWg sync.WaitGroup

    // Kafka 관련
    kafkaWriter   KafkaProducer
    partitionRing ring.PartitionRingReader

    // 인플라이트 요청 추적
    inflightBytes      *atomic.Uint64
    inflightBytesGauge prometheus.Gauge

    // 인제스트 리밋
    ingestLimits *ingestLimits

    // 메트릭
    ingesterAppends        *prometheus.CounterVec
    ingesterAppendTimeouts *prometheus.CounterVec
    replicationFactor      prometheus.Gauge
    streamShardCount       prometheus.Counter
    kafkaAppends           *prometheus.CounterVec
    kafkaWriteBytesTotal   prometheus.Counter
    kafkaWriteLatency      prometheus.Histogram
    kafkaRecordsPerRequest prometheus.Histogram
}
```

### 2.3 보조 타입

```go
// 해시키가 포함된 스트림
type KeyedStream struct {
    HashKey        uint32   // Ingester 라우팅에 사용되는 해시
    HashKeyNoShard uint64   // 샤딩 이전의 원본 해시
    Stream         logproto.Stream
    Policy         string   // 정책 이름
}

// 스트림별 리플리케이션 추적
type streamTracker struct {
    KeyedStream
    minSuccess  int           // 최소 성공 횟수
    maxFailures int           // 최대 허용 실패 횟수
    succeeded   atomic.Int32
    failed      atomic.Int32
}

// 전체 푸시 요청 추적
type PushTracker struct {
    streamsPending atomic.Int32
    streamsFailed  atomic.Int32
    done           chan struct{}
    err            chan error
}
```

---

## 3. Push 흐름 상세

### 3.1 Push 진입점

```go
// pkg/distributor/distributor.go
func (d *Distributor) Push(ctx context.Context, req *logproto.PushRequest) (
    *logproto.PushResponse, error) {
    tenantID, err := tenant.TenantID(ctx)
    if err != nil {
        return nil, err
    }
    return d.PushWithResolver(ctx, req, newRequestScopedStreamResolver(
        tenantID, d.validator.Limits, d.logger), constants.Loki)
}
```

### 3.2 PushWithResolver 전체 단계

```go
func (d *Distributor) PushWithResolver(ctx context.Context, req *logproto.PushRequest,
    streamResolver *requestScopedStreamResolver, format string) (
    *logproto.PushResponse, error) {

    // [단계 1] 인플라이트 바이트 추적
    requestSizeBytes := uint64(req.Size())
    d.inflightBytes.Add(requestSizeBytes)
    defer d.inflightBytes.Sub(requestSizeBytes)

    // [단계 2] 테넌트 ID 추출
    tenantID, err := tenant.TenantID(ctx)

    // [단계 3] 시뮬레이션 대기 시간 (테스트용)
    start := time.Now()
    defer d.waitSimulatedLatency(ctx, tenantID, start)

    // [단계 4] 빈 요청 검사
    if len(req.Streams) == 0 {
        return &logproto.PushResponse{},
            httpgrpc.Errorf(http.StatusUnprocessableEntity,
                validation.MissingStreamsErrorMsg)
    }

    // [단계 5] 검증 컨텍스트 준비
    validationContext := d.validator.getValidationContextForTime(now, tenantID)
    fieldDetector := newFieldDetector(validationContext)
    shardStreamsCfg := d.validator.ShardStreams(tenantID)

    // [단계 6] 스트림별 검증 및 전처리
    for _, stream := range req.Streams {
        // 6a. 라인 트렁케이션
        d.truncateLines(validationContext, &stream)

        // 6b. 레이블 파싱 및 검증
        lbs, stream.Labels, stream.Hash, retentionHours, policy, err =
            d.parseStreamLabels(ctx, validationContext, stream.Labels,
                stream, streamResolver, format)

        // 6c. 레이블 검증
        d.validator.ValidateLabels(vCtx, lbs, stream, ...)

        // 6d. 필수 레이블 체크
        d.missingEnforcedLabels(lbs, tenantID, policy)

        // 6e. 인제스트 차단 체크
        d.validator.ShouldBlockIngestion(validationContext, now, policy)

        // 6f. 엔트리별 검증
        for _, entry := range stream.Entries {
            d.validator.ValidateEntry(ctx, validationContext, lbs, entry, ...)

            // 6g. 구조적 메타데이터 정규화
            // 6h. 로그 레벨 자동 감지
            // 6i. 제네릭 필드 자동 감지
            // 6j. 중복 타임스탬프 증분
        }

        // 6k. 스트림 샤딩
        maybeShardStreams(stream, lbs, streamEntriesSize, policy)
    }

    // [단계 7] 글로벌 레이트 리밋 체크
    if !d.ingestionRateLimiter.AllowN(now, tenantID, totalEntriesSize) {
        return nil, httpgrpc.Errorf(http.StatusTooManyRequests, ...)
    }

    // [단계 8] 인제스트 리밋 체크 (선택적)
    if d.cfg.IngestLimitsEnabled {
        accepted, rejected, err := d.ingestLimits.EnforceLimits(ctx, tenantID, streams)
    }

    // [단계 9] PushTracker 생성
    tracker := PushTracker{
        done: make(chan struct{}, 1),
        err:  make(chan error, 1),
    }
    tracker.streamsPending.Add(int32(streamsToWrite))

    // [단계 10] Tee 복제 (선택적)
    if d.tee != nil {
        d.tee.Duplicate(ctx, tenantID, streams, &tracker)
    }

    // [단계 11] Kafka 쓰기 (선택적)
    if d.cfg.KafkaEnabled {
        d.sendStreamsToKafka(ctx, streams, tenantID, &tracker, subring)
    }

    // [단계 12] Ingester 쓰기
    if d.cfg.IngesterEnabled {
        // Ring에서 각 스트림의 대상 Ingester 결정
        for i, stream := range streams {
            replicationSet, err := d.ingestersRing.Get(
                stream.HashKey, ring.WriteNoExtend, descs[:0], nil, nil)
            // ...
        }
        // 워커 풀을 통해 비동기 전송
        d.ingesterTasks <- pushIngesterTask{...}
    }

    // [단계 13] 완료 대기
    select {
    case err := <-tracker.err:
        return nil, err
    case <-tracker.done:
        return &logproto.PushResponse{}, validationErr
    case <-ctx.Done():
        return nil, ctx.Err()
    }
}
```

### 3.3 Push 흐름 다이어그램

```
PushRequest 수신
      │
      ▼
┌─────────────────────┐
│ 인플라이트 바이트 추적  │
└──────────┬──────────┘
           │
           ▼
┌─────────────────────┐
│ 빈 요청 체크          │──── 빈 요청 → 422 반환
└──────────┬──────────┘
           │
           ▼
┌─────────────────────────────────────────┐
│          스트림별 루프                      │
│                                          │
│  ┌──────────────────┐                    │
│  │라인 트렁케이션     │                    │
│  └────────┬─────────┘                    │
│           │                              │
│  ┌────────▼─────────┐                    │
│  │레이블 파싱 + 캐시  │── 실패 → 에러 수집  │
│  └────────┬─────────┘                    │
│           │                              │
│  ┌────────▼──────────────┐               │
│  │ValidateLabels         │── 실패 → skip │
│  │ + 필수 레이블 체크      │               │
│  │ + 인제스트 차단 체크    │               │
│  └────────┬──────────────┘               │
│           │                              │
│  ┌────────▼─────────┐                    │
│  │엔트리별 검증       │── 실패 → skip     │
│  │ + SM 정규화        │                    │
│  │ + 로그 레벨 감지    │                    │
│  │ + 중복 TS 증분     │                    │
│  └────────┬─────────┘                    │
│           │                              │
│  ┌────────▼─────────┐                    │
│  │스트림 샤딩 (선택적) │                    │
│  └──────────────────┘                    │
└──────────────┬──────────────────────────┘
               │
     ┌─────────▼──────────┐
     │글로벌 레이트 리밋     │── 초과 → 429 반환
     └─────────┬──────────┘
               │
     ┌─────────▼──────────┐
     │인제스트 리밋 (선택적) │── 거부 → 부분 수락
     └─────────┬──────────┘
               │
     ┌─────────┼──────────────┐
     │         │              │
     ▼         ▼              ▼
  Ingester   Kafka         Tee
  (Ring 기반) (파티션 기반)  (복제)
     │         │              │
     └─────────┼──────────────┘
               │
         PushTracker.done
               │
               ▼
         응답 반환
```

---

## 4. 검증 (Validator)

### 4.1 Validator 구조체

파일: `pkg/distributor/validator.go`

```go
type Validator struct {
    Limits
    usageTracker push.UsageTracker
}
```

### 4.2 validationContext

검증에 필요한 모든 파라미터를 미리 한 번 조회하여 캐싱한다:

```go
type validationContext struct {
    rejectOldSample       bool
    rejectOldSampleMaxAge int64
    creationGracePeriod   int64

    maxLineSize                   int
    maxLineSizeTruncate           bool
    maxLineSizeTruncateIdentifier string

    maxLabelNamesPerSeries int
    maxLabelNameLength     int
    maxLabelValueLength    int

    incrementDuplicateTimestamps bool
    discoverServiceName          []string
    discoverGenericFields        map[string][]string
    discoverLogLevels            bool
    logLevelFields               []string

    allowStructuredMetadata    bool
    maxStructuredMetadataSize  int
    maxStructuredMetadataCount int

    blockIngestionUntil      time.Time
    blockIngestionStatusCode int
    enforcedLabels           []string

    userID string
}
```

### 4.3 ValidateEntry: 엔트리 검증

```go
func (v Validator) ValidateEntry(ctx context.Context, vCtx validationContext,
    labels labels.Labels, entry logproto.Entry, retentionHours string,
    policy, format string) error {

    ts := entry.Timestamp.UnixNano()

    // 1. 라인 길이 히스토그램 기록
    validation.LineLengthHist.Observe(float64(len(entry.Line)))

    // 2. 너무 오래된 엔트리 거부
    if vCtx.rejectOldSample && ts < vCtx.rejectOldSampleMaxAge {
        return fmt.Errorf(validation.GreaterThanMaxSampleAgeErrorMsg, ...)
    }

    // 3. 미래 타임스탬프 거부
    if ts > vCtx.creationGracePeriod {
        return fmt.Errorf(validation.TooFarInFutureErrorMsg, ...)
    }

    // 4. 라인 크기 제한
    if maxSize := vCtx.maxLineSize; maxSize != 0 && len(entry.Line) > maxSize {
        return fmt.Errorf(validation.LineTooLongErrorMsg, maxSize, labels, len(entry.Line))
    }

    // 5. 구조적 메타데이터 검증
    if structuredMetadataCount > 0 {
        if !vCtx.allowStructuredMetadata {
            return fmt.Errorf(validation.DisallowedStructuredMetadataErrorMsg, ...)
        }
        if maxSize != 0 && structuredMetadataSizeBytes > maxSize {
            return fmt.Errorf(validation.StructuredMetadataTooLargeErrorMsg, ...)
        }
        if maxCount != 0 && structuredMetadataCount > maxCount {
            return fmt.Errorf(validation.StructuredMetadataTooManyErrorMsg, ...)
        }
    }

    return nil
}
```

### 4.4 ValidateLabels: 레이블 검증

```go
func (v Validator) ValidateLabels(vCtx validationContext, ls labels.Labels,
    stream logproto.Stream, retentionHours, policy, format string) error {

    // 1. 빈 레이블 거부
    if ls.IsEmpty() {
        return fmt.Errorf(validation.MissingLabelsErrorMsg)
    }

    // 2. 내부 스트림은 검증 건너뛰기
    if v.IsInternalStream(ls) {
        return nil
    }

    // 3. 레이블 수 제한
    numLabelNames := ls.Len()
    if ls.Has(push.LabelServiceName) {
        numLabelNames--  // service_name은 인프라에서 추가하므로 제외
    }
    if numLabelNames > vCtx.maxLabelNamesPerSeries {
        return fmt.Errorf(validation.MaxLabelNamesPerSeriesErrorMsg, ...)
    }

    // 4. 레이블 이름/값 길이, 중복 체크
    return ls.Validate(func(l labels.Label) error {
        if len(l.Name) > vCtx.maxLabelNameLength {
            return fmt.Errorf(validation.LabelNameTooLongErrorMsg, ...)
        }
        if len(l.Value) > vCtx.maxLabelValueLength {
            return fmt.Errorf(validation.LabelValueTooLongErrorMsg, ...)
        }
        if cmp := strings.Compare(lastLabelName, l.Name); cmp == 0 {
            return fmt.Errorf(validation.DuplicateLabelNamesErrorMsg, ...)
        }
        return nil
    })
}
```

### 4.5 검증 규칙 요약

| 규칙 | 체크 대상 | 거부 조건 |
|------|---------|----------|
| `GreaterThanMaxSampleAge` | 엔트리 타임스탬프 | 설정된 최대 나이 초과 |
| `TooFarInFuture` | 엔트리 타임스탬프 | 현재 + grace period 초과 |
| `LineTooLong` | 로그 라인 길이 | 최대 라인 크기 초과 |
| `MissingLabels` | 레이블 | 빈 레이블 |
| `MaxLabelNamesPerSeries` | 레이블 수 | 최대 레이블 수 초과 |
| `LabelNameTooLong` | 레이블 이름 길이 | 최대 이름 길이 초과 |
| `LabelValueTooLong` | 레이블 값 길이 | 최대 값 길이 초과 |
| `DuplicateLabelNames` | 레이블 이름 | 중복 레이블 이름 |
| `DisallowedStructuredMetadata` | SM | SM 비활성화 상태에서 SM 포함 |
| `StructuredMetadataTooLarge` | SM 크기 | 최대 SM 크기 초과 |
| `StructuredMetadataTooMany` | SM 수 | 최대 SM 수 초과 |
| `MissingEnforcedLabels` | 필수 레이블 | 필수 레이블 누락 |
| `RateLimited` | 수집 속도 | 테넌트 레이트 리밋 초과 |
| `StreamLimit` | 스트림 수 | 인제스트 리밋 초과 |

---

## 5. 레이트 리밋

### 5.1 로컬 vs 글로벌 전략

Distributor는 두 가지 레이트 리밋 전략을 지원한다:

```go
// 전략 선택
if overrides.IngestionRateStrategy() == validation.GlobalIngestionRateStrategy {
    d.rateLimitStrat = validation.GlobalIngestionRateStrategy
    ingestionRateStrategy = newGlobalIngestionRateStrategy(overrides, d)
} else {
    ingestionRateStrategy = newLocalIngestionRateStrategy(overrides)
}

d.ingestionRateLimiter = limiter.NewRateLimiter(ingestionRateStrategy, 10*time.Second)
```

**로컬 전략**: 각 Distributor 인스턴스가 독립적으로 리밋을 적용한다.
- 장점: 간단, 추가 의존성 없음
- 단점: 총 Distributor 수가 변하면 유효 리밋이 변동

**글로벌 전략**: 전체 Distributor 클러스터에서 공유하는 리밋을 적용한다.
- 장점: 총 리밋이 일정
- 단점: Distributor Ring 필요

```
글로벌 전략에서 각 Distributor의 유효 리밋:

유효_리밋 = 전체_리밋 / 건강한_Distributor_수

예: 전체 리밋 = 10MB/s, Distributor 5대
    → 각 Distributor: 2MB/s
```

### 5.2 RateStore

파일: `pkg/distributor/ratestore.go`

RateStore는 Ingester들로부터 스트림별 수집 속도를 주기적으로 가져온다. 이 데이터는 스트림 샤딩 결정에 사용된다.

```go
type rateStore struct {
    services.Service

    ring            ring.ReadRing
    clientPool      poolClientFactory
    rates           map[string]map[uint64]expiringRate  // tenant → fingerprint → rate
    rateLock        sync.RWMutex
    rateKeepAlive   time.Duration
    ingesterTimeout time.Duration
    maxParallelism  int
    limits          Limits
    metrics         *ratestoreMetrics
    debug           bool
}
```

### 5.3 지수 이동 평균 (EMA)

속도 집계에는 smoothing factor가 적용된다:

```go
const smoothingFactor = .4  // [0, 1] 범위, 클수록 최근 값에 가중
```

```
새로운_속도 = 이전_속도 * (1 - smoothingFactor) + 현재_속도 * smoothingFactor
```

이 EMA 방식은 순간적인 속도 변동에 과민하게 반응하지 않으면서도, 추세 변화를 빠르게 반영한다.

### 5.4 RateStore 설정

```go
type RateStoreConfig struct {
    MaxParallelism           int           `yaml:"max_request_parallelism"`
    StreamRateUpdateInterval time.Duration `yaml:"stream_rate_update_interval"`
    IngesterReqTimeout       time.Duration `yaml:"ingester_request_timeout"`
    Debug                    bool          `yaml:"debug"`
}
```

| 설정 | 기본값 | 설명 |
|------|--------|------|
| `MaxParallelism` | 200 | 최대 병렬 Ingester 요청 수 |
| `StreamRateUpdateInterval` | 1초 | 속도 업데이트 주기 |
| `IngesterReqTimeout` | 500ms | Ingester 요청 타임아웃 |

### 5.5 RateFor 조회

```go
func (s *rateStore) RateFor(tenant string, streamHash uint64) (int64, float64) {
    s.rateLock.RLock()
    defer s.rateLock.RUnlock()

    if t, ok := s.rates[tenant]; ok {
        rate := t[streamHash]
        return rate.rate, rate.pushes
    }
    return 0, 0
}
```

### 5.6 레이트 리밋 적용 지점

```go
// PushWithResolver 내부
if !d.ingestionRateLimiter.AllowN(now, tenantID, totalEntriesSize) {
    d.trackDiscardedData(ctx, req.Streams, validationContext, tenantID,
        validation.RateLimited, streamResolver, format)

    err = fmt.Errorf(validation.RateLimitedErrorMsg, tenantID,
        int(d.ingestionRateLimiter.Limit(now, tenantID)),
        totalLineCount, totalEntriesSize)

    return nil, httpgrpc.Errorf(http.StatusTooManyRequests, "%s", err.Error())
}
```

---

## 6. Ring 라우팅

### 6.1 해시 기반 Ingester 선택

각 스트림은 레이블 세트의 해시를 기반으로 Ingester에 할당된다:

```go
// 스트림의 해시키 계산
streams = append(streams, KeyedStream{
    HashKey:        lokiring.TokenFor(tenantID, stream.Labels),
    HashKeyNoShard: stream.Hash,
    Stream:         stream,
    Policy:         policy,
})
```

### 6.2 ReplicationSet 결정

```go
// Ring에서 스트림의 대상 Ingester 결정
for i, stream := range streams {
    replicationSet, err := d.ingestersRing.Get(
        stream.HashKey,    // 해시키
        ring.WriteNoExtend, // 확장 없는 쓰기 모드
        descs[:0],         // 재사용 버퍼
        nil, nil,
    )

    streamTrackers[i] = streamTracker{
        KeyedStream: stream,
        minSuccess:  len(replicationSet.Instances) - replicationSet.MaxErrors,
        maxFailures: replicationSet.MaxErrors,
    }

    for _, ingester := range replicationSet.Instances {
        streamsByIngester[ingester.Addr] = append(
            streamsByIngester[ingester.Addr], &streamTrackers[i])
        ingesterDescs[ingester.Addr] = ingester
    }
}
```

### 6.3 minSuccess와 maxFailures

Replication Factor(RF)에 따른 설정:

| RF | 전체 복제본 | minSuccess | maxFailures | 설명 |
|----|-----------|------------|-------------|------|
| 1  | 1         | 1          | 0           | 단일 복제본 |
| 3  | 3         | 2          | 1           | 과반수 쓰기 |
| 5  | 5         | 3          | 2           | 과반수 쓰기 |

```
minSuccess = RF - MaxErrors = RF - floor(RF/2) = ceil(RF/2) + 1 (홀수 RF)
maxFailures = floor(RF/2)
```

### 6.4 라우팅 다이어그램

```
스트림 A (hash=100)          스트림 B (hash=500)
        │                           │
        ▼                           ▼
┌──────────────────────────────────────────────┐
│              Consistent Hash Ring             │
│                                               │
│   Token: 50    150    300    450    600    750 │
│          │      │      │      │      │      │ │
│          I1     I2     I3     I1     I2     I3│
│                                               │
│   hash=100 → I2(150), I3(300), I1(450)       │
│   hash=500 → I2(600), I3(750), I1(50)        │
│                                               │
│   RF=3: 각 스트림은 3개 Ingester에 복제          │
└──────────────────────────────────────────────┘
```

---

## 7. 스트림 샤딩

### 7.1 샤딩이 필요한 이유

고처리량 스트림은 단일 Ingester에 병목을 유발할 수 있다. 스트림 샤딩은 하나의 논리적 스트림을 여러 물리적 스트림으로 분할하여 부하를 분산한다.

### 7.2 shardStream()

```go
func (d *Distributor) shardStream(stream logproto.Stream, pushSize int,
    tenantID string, policy string) []KeyedStream {

    shardStreamsCfg := d.validator.ShardStreams(tenantID)
    shardCount := d.shardCountFor(logger, &stream, pushSize, tenantID, shardStreamsCfg)

    if shardCount <= 1 {
        // 샤딩 불필요
        return []KeyedStream{{
            HashKey:        lokiring.TokenFor(tenantID, stream.Labels),
            HashKeyNoShard: stream.Hash,
            Stream:         stream,
            Policy:         policy,
        }}
    }

    d.streamShardCount.Inc()
    return d.divideEntriesBetweenShards(tenantID, shardCount, shardStreamsCfg, stream, policy)
}
```

### 7.3 shardCountFor() - 샤드 수 결정

샤드 수는 스트림의 현재 수집 속도와 설정된 목표 속도를 비교하여 결정된다:

```
원하는 샤드 수 = ceil(현재_속도 / 목표_샤드_속도)
```

### 7.4 divideEntriesBetweenShards() - 엔트리 분배

```go
func (d *Distributor) divideEntriesBetweenShards(tenantID string,
    totalShards int, shardStreamsCfg shardstreams.Config,
    stream logproto.Stream, policy string) []KeyedStream {

    derivedStreams := d.createShards(stream, totalShards, tenantID,
        shardStreamsCfg, policy)

    // 라운드-로빈 방식으로 엔트리 분배
    for i := 0; i < len(stream.Entries); i++ {
        streamIndex := i % len(derivedStreams)
        entries := append(derivedStreams[streamIndex].Stream.Entries,
            stream.Entries[i])
        derivedStreams[streamIndex].Stream.Entries = entries
    }

    return derivedStreams
}
```

### 7.5 샤드 생성

```go
func (d *Distributor) createShards(stream logproto.Stream, totalShards int,
    tenantID string, shardStreamsCfg shardstreams.Config, policy string) []KeyedStream {

    streamLabels := labelTemplate(stream.Labels, d.logger)
    // __stream_shard__ 라벨이 추가된 템플릿 생성

    startShard := d.shardTracker.LastShardNum(tenantID, stream.Hash)
    for i := 0; i < streamCount; i++ {
        shardNum := (startShard + i) % totalShards
        shard := d.createShard(streamLabels, streamPattern, shardNum, entriesPerShard)
        derivedStreams = append(derivedStreams, KeyedStream{
            HashKey:        lokiring.TokenFor(tenantID, shard.Labels),
            HashKeyNoShard: stream.Hash,
            Stream:         shard,
            Policy:         policy,
        })
    }
    d.shardTracker.SetLastShardNum(tenantID, stream.Hash, startShard+streamCount)

    return derivedStreams
}
```

### 7.6 샤딩 결과 예시

```
원본 스트림:
    {app="nginx", env="prod"} → 엔트리 6개

샤딩 (3개 샤드):
    {app="nginx", env="prod", __stream_shard__="0"} → 엔트리 [0,3]
    {app="nginx", env="prod", __stream_shard__="1"} → 엔트리 [1,4]
    {app="nginx", env="prod", __stream_shard__="2"} → 엔트리 [2,5]
```

### 7.7 시간 기반 샤딩

시간 기반 샤딩은 같은 스트림의 엔트리를 시간 범위별로 분리한다:

```go
// 시간 기반 샤딩 옵션
if shardStreamsCfg.TimeShardingEnabled {
    ignoreRecentFrom := now.Add(-shardStreamsCfg.TimeShardingIgnoreRecent)
    streamsByTime, ok := shardStreamByTime(stream, labels,
        d.ingesterCfg.MaxChunkAge/2, ignoreRecentFrom)
    // ...
}
```

이 기능은 `__time_shard__` 라벨을 사용하여 시간 범위가 넓은 백필(backfill) 데이터를 별도의 청크로 분리한다. 최근 데이터는 샤딩하지 않아 정상적인 쿼리 성능을 유지한다.

### 7.8 ShardTracker

```go
type ShardTracker struct {
    // 테넌트별, 스트림별 마지막 샤드 번호 추적
    // 연속적인 샤드 할당을 보장
}
```

`ShardTracker`는 각 스트림의 마지막 샤드 번호를 기억하여, 연속 Push에서 샤드를 순환적으로 할당한다. 이로써 엔트리가 특정 샤드에 몰리지 않고 균등하게 분배된다.

---

## 8. Kafka 듀얼 라이트

### 8.1 듀얼 라이트 아키텍처

Distributor는 Ingester와 Kafka에 동시에 쓸 수 있다:

```go
type Config struct {
    KafkaEnabled    bool `yaml:"kafka_writes_enabled"`
    IngesterEnabled bool `yaml:"ingester_writes_enabled"`
}
```

최소 하나의 경로는 활성화되어야 한다:

```go
func (cfg *Config) Validate() error {
    if !cfg.KafkaEnabled && !cfg.IngesterEnabled {
        return fmt.Errorf("at least one of kafka and ingestor writes must be enabled")
    }
    return nil
}
```

### 8.2 sendStreamsToKafka()

```go
func (d *Distributor) sendStreamsToKafka(ctx context.Context,
    streams []KeyedStream, tenant string,
    tracker *PushTracker, subring *ring.PartitionRing) {

    for _, s := range streams {
        go func(s KeyedStream) {
            err := d.sendStreamToKafka(ctx, s, tenant, subring)
            if err != nil {
                err = fmt.Errorf("failed to write stream to kafka: %w", err)
            }
            tracker.doneWithResult(err)
        }(s)
    }
}
```

### 8.3 파티션 선택

```go
func (d *Distributor) sendStreamToKafka(ctx context.Context,
    stream KeyedStream, tenant string,
    subring *ring.PartitionRing) error {

    // 파티션 링에서 스트림의 대상 파티션 결정
    streamPartitionID, err := subring.ActivePartitionForKey(stream.HashKey)

    // 스트림을 Kafka 레코드로 인코딩
    records, err := kafka.Encode(
        streamPartitionID,
        tenant,
        stream.Stream,
        d.cfg.KafkaConfig.ProducerMaxRecordSizeBytes,
    )

    // 동기 프로듀스
    produceResults := d.kafkaWriter.ProduceSync(ctx, records)

    // 결과 처리 및 메트릭 기록
    for _, result := range produceResults {
        if result.Err != nil {
            d.kafkaAppends.WithLabelValues(
                fmt.Sprintf("partition_%d", streamPartitionID), "fail").Inc()
        } else {
            d.kafkaAppends.WithLabelValues(
                fmt.Sprintf("partition_%d", streamPartitionID), "success").Inc()
        }
    }
    return finalErr
}
```

### 8.4 Kafka vs Ingester 경로 비교

```
┌─────────────────────────────────────────────────────────────┐
│                    Distributor                                │
│                                                              │
│  ┌──────────────────────┐  ┌───────────────────────────┐    │
│  │   Ingester 경로        │  │    Kafka 경로               │    │
│  │                       │  │                            │    │
│  │  - Ring 기반 라우팅     │  │  - Partition Ring 기반      │    │
│  │  - RF-way 리플리케이션  │  │  - 단일 파티션 쓰기         │    │
│  │  - 워커 풀 기반 전송    │  │  - 고루틴별 직접 전송       │    │
│  │  - minSuccess 대기     │  │  - 즉시 결과 처리           │    │
│  │                       │  │                            │    │
│  │  대상: Ingester gRPC   │  │  대상: Kafka 브로커         │    │
│  └──────────────────────┘  └───────────────────────────┘    │
│                                                              │
│  PushTracker로 두 경로 모두 완료 대기                          │
└─────────────────────────────────────────────────────────────┘
```

---

## 9. 동시성 모델

### 9.1 PushTracker 패턴

```go
type PushTracker struct {
    streamsPending atomic.Int32
    streamsFailed  atomic.Int32
    done           chan struct{}
    err            chan error
}

func (p *PushTracker) doneWithResult(err error) {
    if err == nil {
        if p.streamsPending.Dec() == 0 {
            p.done <- struct{}{}  // 모든 스트림 성공
        }
    } else {
        if p.streamsFailed.Inc() == 1 {
            p.err <- err  // 첫 번째 실패만 전달
        }
    }
}
```

이 패턴의 핵심:
- `streamsPending`이 0이 되면 `done` 채널로 완료 신호
- 실패는 첫 번째만 `err` 채널로 전달 (중복 방지)
- atomic 연산으로 동시성 안전

### 9.2 pushIngesterTask 워커 풀

```go
type pushIngesterTask struct {
    ingester      ring.InstanceDesc
    streamTracker []*streamTracker
    pushTracker   *PushTracker
    ctx           context.Context
    cancel        context.CancelFunc
}
```

Distributor는 고정 크기 워커 풀을 사용하여 Ingester에 배치를 전송한다:

```go
func (d *Distributor) running(ctx context.Context) error {
    ctx, cancel := context.WithCancel(ctx)
    defer func() {
        cancel()
        d.ingesterTaskWg.Wait()
    }()

    d.ingesterTaskWg.Add(d.cfg.PushWorkerCount)
    for i := 0; i < d.cfg.PushWorkerCount; i++ {
        go d.pushIngesterWorker(ctx)
    }
    // ...
}
```

워커는 채널에서 태스크를 받아 처리한다:

```go
// 태스크 제출
d.ingesterTasks <- pushIngesterTask{
    ingester:      ingesterDescs[ingester],
    streamTracker: samples,
    pushTracker:   &tracker,
    ctx:           localCtx,
    cancel:        cancel,
}
```

### 9.3 sendStreams: 결과 처리

```go
func (d *Distributor) sendStreams(task pushIngesterTask) {
    defer task.cancel()
    err := d.sendStreamsErr(task.ctx, task.ingester, task.streamTracker)

    for i := range task.streamTracker {
        if err != nil {
            if task.streamTracker[i].failed.Inc() <= int32(task.streamTracker[i].maxFailures) {
                continue  // 아직 허용 범위 내
            }
            task.pushTracker.doneWithResult(err)  // maxFailures 초과
        } else {
            if task.streamTracker[i].succeeded.Inc() != int32(task.streamTracker[i].minSuccess) {
                continue  // 아직 충분하지 않음
            }
            task.pushTracker.doneWithResult(nil)  // 최소 성공 달성
        }
    }
}
```

### 9.4 동시성 흐름 다이어그램

```
PushWithResolver()
        │
        │ 스트림별 Ring 조회
        ▼
┌───────────────────────────────────────────────┐
│  streamsByIngester 맵 구성                      │
│                                                │
│  Ingester A: [stream_1, stream_3]              │
│  Ingester B: [stream_1, stream_2, stream_3]    │
│  Ingester C: [stream_2]                        │
└───────────┬───────────────────────────────────┘
            │
            │ 각 Ingester에 대해 태스크 생성
            ▼
     ┌──────────────┐
     │ ingesterTasks │  <── 채널 (버퍼 없음)
     │   채널         │
     └──────┬───────┘
            │
     ┌──────┼──────┐──────┐
     │      │      │      │
     ▼      ▼      ▼      ▼
  Worker  Worker  Worker  ...  (PushWorkerCount=256)
     │      │      │
     │      │      │
     ▼      ▼      ▼
  sendStreamsErr()  ← Ingester gRPC Push
     │      │      │
     │      │      │
     ▼      ▼      ▼
  doneWithResult() ← 성공/실패 기록
     │
     ▼
  PushTracker.done / PushTracker.err
```

### 9.5 Context 관리

```go
// 메인 요청의 취소가 Ingester 쓰기에 전파되지 않도록 분리
localCtx, cancel := context.WithTimeout(
    context.WithoutCancel(ctx),  // 부모 취소로부터 격리
    d.clientCfg.RemoteTimeout,
)
```

이 설계는 의도적이다: 클라이언트가 요청을 취소하더라도, 이미 시작된 Ingester 쓰기는 완료되어야 데이터 일관성이 유지된다.

---

## 10. 메트릭

### 10.1 주요 메트릭

| 메트릭 | 타입 | 설명 |
|--------|------|------|
| `loki_distributor_ingester_appends_total` | Counter | Ingester별 전송 횟수 |
| `loki_distributor_ingester_append_timeouts_total` | Counter | Ingester별 타임아웃 횟수 |
| `loki_distributor_replication_factor` | Gauge | 설정된 RF |
| `loki_stream_sharding_count` | Counter | 스트림 샤딩 발생 횟수 |
| `loki_distributor_kafka_appends_total` | Counter | 파티션별 Kafka 전송 횟수 |
| `loki_distributor_kafka_latency_seconds` | Histogram | Kafka 쓰기 지연 시간 |
| `loki_distributor_kafka_sent_bytes_total` | Counter | Kafka 전송 바이트 |
| `loki_distributor_kafka_records_per_write_request` | Histogram | 요청당 Kafka 레코드 수 |
| `loki_distributor_inflight_bytes` | Gauge | 현재 인플라이트 바이트 |
| `loki_blocked_queries` | Counter | 차단된 쿼리 수 |
| `loki_distributor_push_structured_metadata_sanitized_total` | Counter | SM 정규화 횟수 |

### 10.2 라벨 캐시

레이블 파싱은 비용이 높은 연산이므로, LRU 캐시로 최적화한다:

```go
labelCache, err := lru.New[string, labelData](maxLabelCacheSize)
// maxLabelCacheSize = 100000
```

```go
type labelData struct {
    ls   labels.Labels
    hash uint64
}
```

캐시 히트 시 레이블 파싱과 해시 계산을 건너뛸 수 있다:

```go
func (d *Distributor) parseStreamLabels(ctx context.Context, vContext validationContext,
    key string, stream logproto.Stream, ...) (...) {

    if val, ok := d.labelCache.Get(key); ok {
        // 캐시 히트: 파싱 건너뛰기
        return val.ls, val.ls.String(), val.hash, retentionHours, policy, nil
    }

    // 캐시 미스: 파싱 수행
    ls, err := syntax.ParseLabels(key)
    // ...
}
```

### 10.3 라이프사이클 서비스

```go
func (d *Distributor) starting(ctx context.Context) error {
    return services.StartManagerAndAwaitHealthy(ctx, d.subservices)
}

func (d *Distributor) running(ctx context.Context) error {
    // 워커 풀 시작
    d.ingesterTaskWg.Add(d.cfg.PushWorkerCount)
    for i := 0; i < d.cfg.PushWorkerCount; i++ {
        go d.pushIngesterWorker(ctx)
    }
    // 종료 또는 서브서비스 실패 대기
    select {
    case <-ctx.Done():
        return nil
    case err := <-d.subservicesWatcher.Chan():
        return errors.Wrap(err, "distributor subservice failed")
    }
}

func (d *Distributor) stopping(_ error) error {
    if d.kafkaWriter != nil {
        d.kafkaWriter.Close()
    }
    return services.StopManagerAndAwaitStopped(context.Background(), d.subservices)
}
```

---

## 참고 파일 경로

| 파일 | 설명 |
|------|------|
| `pkg/distributor/distributor.go` | Distributor 구조체, Push 흐름, 샤딩, Kafka 쓰기 |
| `pkg/distributor/validator.go` | Validator, ValidateEntry, ValidateLabels |
| `pkg/distributor/ratestore.go` | RateStore, 지수 이동 평균 |
| `pkg/distributor/ingestion_rate_strategy.go` | 로컬/글로벌 레이트 전략 |
| `pkg/distributor/shard_tracker.go` | ShardTracker |
| `pkg/distributor/shardstreams/` | 샤드 스트림 설정 |
| `pkg/distributor/clientpool/` | Ingester 클라이언트 풀 |
| `pkg/distributor/writefailures/` | 쓰기 실패 로깅 관리 |
| `pkg/distributor/distributor_ring.go` | Distributor Ring 설정 |
| `pkg/distributor/tee.go` | Tee 인터페이스 (데이터 복제) |
| `pkg/distributor/field_detection.go` | 로그 레벨/필드 자동 감지 |
| `pkg/distributor/http.go` | HTTP 핸들러 |
| `pkg/distributor/limits.go` | Limits 인터페이스 |
| `pkg/distributor/segment.go` | 세그먼트 관리 |
| `pkg/distributor/ingest_limits.go` | 인제스트 리밋 서비스 클라이언트 |
