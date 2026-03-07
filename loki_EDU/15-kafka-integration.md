# 15. Kafka 연동 Deep-Dive

## 목차
1. [개요: 왜 Kafka인가?](#1-개요-왜-kafka인가)
2. [듀얼 라이트 아키텍처](#2-듀얼-라이트-아키텍처)
3. [Distributor Config: KafkaEnabled와 IngesterEnabled](#3-distributor-config-kafkaenabled와-ingesterenabled)
4. [sendStreamToKafka() 상세 분석](#4-sendstreamtokafka-상세-분석)
5. [파티션 링: PartitionRing과 ActivePartitionForKey](#5-파티션-링-partitionring과-activepartitionforkey)
6. [DataObj Tee: 추가 데이터 경로](#6-dataobj-tee-추가-데이터-경로)
7. [SegmentationPartitionResolver: 세그멘테이션 기반 파티션 선택](#7-segmentationpartitionresolver-세그멘테이션-기반-파티션-선택)
8. [DataObj Consumer: Kafka 소비 → DataObj 생성](#8-dataobj-consumer-kafka-소비--dataobj-생성)
9. [메트릭: Kafka 쓰기 경로 관측](#9-메트릭-kafka-쓰기-경로-관측)
10. [장애 처리: 재시도와 파티션 재할당](#10-장애-처리-재시도와-파티션-재할당)
11. [설계 결정: 왜 Kafka인가?](#11-설계-결정-왜-kafka인가)

---

## 1. 개요: 왜 Kafka인가?

Grafana Loki는 전통적으로 Distributor가 Ingester에 직접 gRPC로 로그를 전송하는 아키텍처를 사용했다. 이 방식은 단순하지만 몇 가지 근본적인 한계가 있다:

1. **커플링**: Distributor와 Ingester가 강하게 결합되어 있어, Ingester 장애 시 Distributor까지 영향을 받음
2. **버퍼링 부재**: 트래픽 스파이크 시 백프레셔(backpressure)가 Distributor를 통해 클라이언트까지 전파
3. **재처리 불가**: 한번 Ingester에 전달된 데이터는 재처리가 불가능

Kafka를 도입함으로써 이 세 가지 문제를 해결한다:

```
전통적 아키텍처:
┌──────────┐     gRPC     ┌──────────┐
│Distributor├─────────────►│ Ingester │
└──────────┘              └──────────┘
    ↑ 장애 전파 ↓

Kafka 도입 후:
┌──────────┐             ┌──────────┐
│Distributor├──┬──────────►│ Ingester │  (선택적)
└──────────┘  │           └──────────┘
              │
              │  ┌───────────┐     ┌──────────────┐
              └─►│  Kafka    ├────►│ DataObj      │
                 │  Cluster  │     │ Consumer     │
                 └───────────┘     └──────┬───────┘
                                          │
                                    ┌─────▼──────┐
                                    │ Object     │
                                    │ Storage    │
                                    └────────────┘
```

---

## 2. 듀얼 라이트 아키텍처

Loki의 Kafka 통합에서 가장 핵심적인 설계 결정은 **듀얼 라이트(Dual Write)** 아키텍처다. Distributor는 Kafka와 Ingester에 **동시에** 데이터를 전송할 수 있다.

### 2.1 Distributor 구조체

소스 경로: `pkg/distributor/distributor.go`

```go
// Distributor coordinates replicates and distribution of log streams.
type Distributor struct {
    services.Service

    cfg              Config
    // ...
    tee              Tee               // 추가 데이터 경로 (DataObj Tee)

    // kafka
    kafkaWriter   KafkaProducer
    partitionRing ring.PartitionRingReader

    // kafka metrics
    kafkaAppends           *prometheus.CounterVec
    kafkaWriteBytesTotal   prometheus.Counter
    kafkaWriteLatency      prometheus.Histogram
    kafkaRecordsPerRequest prometheus.Histogram
}
```

### 2.2 듀얼 라이트 흐름

Push 요청 처리 시 Distributor는 설정에 따라 Kafka와 Ingester에 동시 전송한다:

```
Push 요청 수신
     │
     ▼
┌────────────────────────────────────────────────┐
│              Distributor.Push()                 │
│                                                │
│  1. 유효성 검증 (Validator)                     │
│  2. 스트림 샤딩 (ShardStreams)                  │
│  3. PushTracker 초기화                          │
│                                                │
│  ┌───────────────┐    ┌───────────────────┐    │
│  │ KafkaEnabled? │    │ IngesterEnabled?  │    │
│  │               │    │                   │    │
│  │ YES: kafka에  │    │ YES: ingester에   │    │
│  │ 스트림 전송   │    │ 스트림 전송       │    │
│  └───────┬───────┘    └────────┬──────────┘    │
│          │                     │               │
│          ▼                     ▼               │
│  sendStreamsToKafka()   sendStreams()            │
│                                                │
│  4. PushTracker 대기 (모든 전송 완료까지)       │
│                                                │
│  5. Tee.Duplicate() (DataObj Tee 포함)          │
└────────────────────────────────────────────────┘
```

### 2.3 PushTracker: 동시 전송 조율

`PushTracker`는 Kafka와 Ingester 양쪽 전송의 완료를 추적하는 구조체다:

```go
// pkg/distributor/distributor.go (line 523)
func (p *PushTracker) doneWithResult(err error) {
    // ...
}
```

PushTracker는 `streamsPending` 카운터를 통해 모든 비동기 전송(Kafka + Ingester + Tee)이 완료될 때까지 Push 요청의 응답을 지연시킨다. 이를 통해 **클라이언트에게 성공 응답을 반환하기 전에 모든 경로에 데이터가 안전하게 도달했음을 보장**한다.

---

## 3. Distributor Config: KafkaEnabled와 IngesterEnabled

소스 경로: `pkg/distributor/distributor.go` (line 84-145)

### 3.1 Config 구조체

```go
type Config struct {
    // ...
    KafkaEnabled              bool `yaml:"kafka_writes_enabled"`
    IngesterEnabled           bool `yaml:"ingester_writes_enabled"`
    IngestLimitsEnabled       bool `yaml:"ingest_limits_enabled"`

    KafkaConfig kafka.Config `yaml:"-"`

    DataObjTeeConfig DataObjTeeConfig `yaml:"dataobj_tee"`
}
```

### 3.2 핵심 플래그

| 플래그 | 기본값 | 설명 |
|--------|--------|------|
| `distributor.kafka-writes-enabled` | `false` | Kafka 쓰기 활성화 |
| `distributor.ingester-writes-enabled` | `true` | Ingester 쓰기 활성화 |
| `distributor.ingest-limits-enabled` | `false` | Ingest Limits 서비스 활용 |

### 3.3 유효성 검증

```go
// pkg/distributor/distributor.go (line 133)
func (cfg *Config) Validate() error {
    if !cfg.KafkaEnabled && !cfg.IngesterEnabled {
        return fmt.Errorf("at least one of kafka and ingestor writes must be enabled")
    }
    // ...
}
```

최소 하나의 쓰기 경로가 활성화되어야 한다. 두 경로를 모두 비활성화하면 설정 검증에서 실패한다.

### 3.4 KafkaWriter 초기화

Kafka가 활성화되면 Distributor 생성 시 franz-go 기반 프로듀서가 초기화된다:

```go
// pkg/distributor/distributor.go (line 299-308)
if cfg.KafkaEnabled {
    kafkaClient, err := kafka_client.NewWriterClient(
        "distributor", cfg.KafkaConfig, 20, logger, registerer)
    if err != nil {
        return nil, fmt.Errorf("failed to start kafka client: %w", err)
    }
    kafkaWriter = kafka_client.NewProducer("distributor", kafkaClient,
        cfg.KafkaConfig.ProducerMaxBufferedBytes,
        prometheus.WrapRegistererWithPrefix("loki_", registerer),
        kafka_client.WithRecordsInterceptor(
            validation.IngestionPoliciesKafkaProducerInterceptor),
    )
}
```

`franz-go`의 `kgo.Client`를 기반으로 하며, `ProducerMaxBufferedBytes`로 버퍼 크기를 제한한다.

---

## 4. sendStreamToKafka() 상세 분석

소스 경로: `pkg/distributor/distributor.go` (line 1296-1355)

### 4.1 함수 시그니처

```go
func (d *Distributor) sendStreamToKafka(
    ctx context.Context,
    stream KeyedStream,
    tenant string,
    subring *ring.PartitionRing,
) error
```

### 4.2 실행 흐름

```
sendStreamToKafka()
     │
     ▼
[1] 빈 스트림 체크 ─── entries == 0 → return nil
     │
     ▼
[2] 파티션 결정 ───── subring.ActivePartitionForKey(stream.HashKey)
     │
     │  실패 시: kafkaAppends("kafka","fail") + error
     │
     ▼
[3] 레코드 인코딩 ── kafka.Encode(partitionID, tenant, stream, maxRecordSize)
     │
     │  실패 시: kafkaAppends("partition_N","fail") + error
     │
     ▼
[4] 동기 프로듀스 ── d.kafkaWriter.ProduceSync(ctx, records)
     │
     ▼
[5] 결과 처리 ────── 각 레코드별 성공/실패 메트릭 기록
     │
     ▼
[6] 레이턴시/바이트 관측
```

### 4.3 상세 코드 분석

```go
func (d *Distributor) sendStreamToKafka(ctx context.Context, stream KeyedStream,
    tenant string, subring *ring.PartitionRing) error {

    if len(stream.Stream.Entries) == 0 {
        return nil
    }

    // [단계 2] 파티션 결정
    // HashKey는 스트림의 레이블 해시이므로, 같은 레이블의 스트림은
    // 항상 같은 파티션으로 라우팅된다.
    streamPartitionID, err := subring.ActivePartitionForKey(stream.HashKey)
    if err != nil {
        d.kafkaAppends.WithLabelValues("kafka", "fail").Inc()
        return fmt.Errorf("failed to find active partition for stream: %w", err)
    }

    startTime := time.Now()

    // [단계 3] 레코드 인코딩
    // 하나의 스트림이 여러 레코드로 분할될 수 있다 (maxRecordSize 초과 시)
    records, err := kafka.Encode(
        streamPartitionID,
        tenant,
        stream.Stream,
        d.cfg.KafkaConfig.ProducerMaxRecordSizeBytes,
    )
    if err != nil {
        d.kafkaAppends.WithLabelValues(
            fmt.Sprintf("partition_%d", streamPartitionID), "fail",
        ).Inc()
        return fmt.Errorf("failed to marshal write request to records: %w", err)
    }

    d.kafkaRecordsPerRequest.Observe(float64(len(records)))

    // [단계 4] 동기 프로듀스
    // ProduceSync는 모든 레코드의 쓰기가 완료될 때까지 블로킹
    produceResults := d.kafkaWriter.ProduceSync(ctx, records)

    // [단계 5-6] 메트릭 기록
    if count, sizeBytes := successfulProduceRecordsStats(produceResults); count > 0 {
        d.kafkaWriteLatency.Observe(time.Since(startTime).Seconds())
        d.kafkaWriteBytesTotal.Add(float64(sizeBytes))
    }

    var finalErr error
    for _, result := range produceResults {
        if result.Err != nil {
            d.kafkaAppends.WithLabelValues(
                fmt.Sprintf("partition_%d", streamPartitionID), "fail").Inc()
            finalErr = result.Err
        } else {
            d.kafkaAppends.WithLabelValues(
                fmt.Sprintf("partition_%d", streamPartitionID), "success").Inc()
        }
    }

    return finalErr
}
```

### 4.4 sendStreamsToKafka: 병렬 전송

```go
// pkg/distributor/distributor.go (line 1284-1294)
func (d *Distributor) sendStreamsToKafka(ctx context.Context, streams []KeyedStream,
    tenant string, tracker *PushTracker, subring *ring.PartitionRing) {
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

각 스트림은 **별도의 고루틴**에서 독립적으로 Kafka에 전송된다. 이는 파티션이 다른 스트림들이 서로를 블로킹하지 않도록 보장한다.

---

## 5. 파티션 링: PartitionRing과 ActivePartitionForKey

### 5.1 PartitionRing 개요

Loki의 Kafka 통합에서 파티션 링은 **어떤 Kafka 파티션이 활성 상태인지** 추적하는 핵심 메커니즘이다.

```
┌─────────────────────────────────────────────┐
│              Partition Ring                  │
│                                             │
│  Partition 0: ACTIVE   ◄── Ingester-0       │
│  Partition 1: ACTIVE   ◄── Ingester-1       │
│  Partition 2: ACTIVE   ◄── Ingester-2       │
│  Partition 3: INACTIVE                      │
│  Partition 4: ACTIVE   ◄── Ingester-4       │
│                                             │
└─────────────────────────────────────────────┘
```

### 5.2 ActivePartitionForKey

`ActivePartitionForKey`는 dskit의 `ring.PartitionRing`이 제공하는 메서드로, 주어진 해시 키에 대해 활성 파티션을 일관성 있게 반환한다:

```go
// subring.ActivePartitionForKey(stream.HashKey)
// → 해시키를 활성 파티션 수로 모듈러 연산하여 파티션 ID 결정
```

### 5.3 ShuffleShard

테넌트별 파티션 할당은 `ShuffleShard`를 통해 이루어진다:

```
전체 파티션 링: [0, 1, 2, 3, 4, 5, 6, 7]

테넌트 A (high rate):  ShuffleShard("A", 4) → [0, 2, 5, 7]
테넌트 B (low rate):   ShuffleShard("B", 2) → [1, 4]
테넌트 C (medium):     ShuffleShard("C", 3) → [0, 3, 6]
```

`ShuffleShard`는 테넌트 ID를 해시하여 결정적으로 파티션 서브셋을 선택한다. 이를 통해:
- **데이터 지역성**: 같은 테넌트의 데이터가 일관된 파티션 셋으로 라우팅
- **부하 분산**: 테넌트 간 파티션이 겹칠 수 있어 균등한 분배 달성
- **스케일링**: 테넌트의 전송률에 비례하여 파티션 수 자동 조정

### 5.4 KeyedStream 구조체

```go
// pkg/distributor/distributor.go (line 496)
type KeyedStream struct {
    HashKey        uint32           // 파티션 선택에 사용되는 해시
    HashKeyNoShard uint64           // 샤딩 전 원본 해시
    Stream         logproto.Stream  // 실제 로그 데이터
}
```

`HashKey`는 스트림의 레이블 집합을 해시한 32비트 값으로, 동일한 레이블을 가진 스트림은 항상 동일한 파티션으로 라우팅된다.

---

## 6. DataObj Tee: 추가 데이터 경로

소스 경로: `pkg/distributor/tee.go`, `pkg/distributor/dataobj_tee.go`

### 6.1 Tee 인터페이스

```go
// pkg/distributor/tee.go (line 8-14)
type Tee interface {
    Duplicate(ctx context.Context, tenant string, streams []KeyedStream,
        pushTracker *PushTracker)

    Register(ctx context.Context, tenant string, streams []KeyedStream,
        pushTracker *PushTracker)
}
```

Tee는 Distributor의 쓰기 경로에 추가 데이터 경로를 삽입하는 인터페이스다. `Register`는 추적할 스트림 수를 PushTracker에 등록하고, `Duplicate`는 실제 데이터를 복제한다.

### 6.2 multiTee: 체이닝

```go
// pkg/distributor/tee.go (line 17-25)
func WrapTee(existing, newTee Tee) Tee {
    if existing == nil {
        return newTee
    }
    if multi, ok := existing.(*multiTee); ok {
        return &multiTee{append(multi.tees, newTee)}
    }
    return &multiTee{tees: []Tee{existing, newTee}}
}
```

여러 Tee를 체이닝할 수 있어, 하나의 Push 요청이 여러 추가 경로로 동시에 복제된다.

### 6.3 DataObjTee 구조체

```go
// pkg/distributor/dataobj_tee.go (line 66-80)
type DataObjTee struct {
    cfg          *DataObjTeeConfig
    limitsClient *ingestLimits
    rateBatcher  *rateBatcher
    limits       Limits
    kafkaClient  *kgo.Client
    resolver     *SegmentationPartitionResolver
    logger       log.Logger

    // Metrics.
    streams         prometheus.Counter
    streamFailures  prometheus.Counter
    producedBytes   *prometheus.CounterVec
    producedRecords *prometheus.CounterVec
}
```

### 6.4 DataObjTee 설정

```go
// pkg/distributor/dataobj_tee.go (line 30-37)
type DataObjTeeConfig struct {
    Enabled               bool          `yaml:"enabled"`
    Topic                 string        `yaml:"topic"`
    MaxBufferedBytes      int           `yaml:"max_buffered_bytes"`
    PerPartitionRateBytes int           `yaml:"per_partition_rate_bytes"`
    DebugMetricsEnabled   bool          `yaml:"debug_metrics_enabled"`
    RateBatchWindow       time.Duration `yaml:"rate_batch_window"`
}
```

| 설정 | 기본값 | 설명 |
|------|--------|------|
| `enabled` | `false` | DataObj Tee 활성화 |
| `topic` | `""` | Kafka 토픽 이름 (필수) |
| `max_buffered_bytes` | 100MB | 최대 버퍼 크기 |
| `per_partition_rate_bytes` | 1MB/s | 파티션별 목표 전송률 |
| `rate_batch_window` | `0` | 레이트 업데이트 배치 윈도우 |

### 6.5 DataObjTee.Duplicate 흐름

```
DataObjTee.Duplicate()
     │
     ▼
[1] 세그멘테이션 키 추출
     │  service_name 레이블 사용
     │  없으면 "unknown_service"
     │
     ▼
[2] 레이트 조회/업데이트
     │  rateBatcher 또는 동기 UpdateRates
     │
     ▼
[3] 각 스트림을 고루틴으로 복제
     │
     ▼
[4] duplicate() per stream:
     │  → resolver.Resolve() → 파티션 결정
     │  → kafka.EncodeWithTopic() → 레코드 인코딩
     │  → kafkaClient.ProduceSync() → Kafka 전송
     │  → pushTracker.doneWithResult()
```

---

## 7. SegmentationPartitionResolver: 세그멘테이션 기반 파티션 선택

소스 경로: `pkg/distributor/segment.go`

### 7.1 SegmentationKey

```go
// pkg/distributor/segment.go (line 18-42)
type SegmentationKey string

func (key SegmentationKey) Sum64() uint64 {
    h := fnv.New64a()
    h.Write([]byte("__loki_segmentation_key__"))
    h.Write([]byte(key))
    return h.Sum64()
}

func GetSegmentationKey(stream KeyedStream) (SegmentationKey, error) {
    labels, err := syntax.ParseLabels(stream.Stream.Labels)
    if err != nil {
        return "", err
    }
    if serviceName := labels.Get("service_name"); serviceName != "" {
        return SegmentationKey(serviceName), nil
    }
    return SegmentationKey("unknown_service"), nil
}
```

세그멘테이션 키는 `service_name` 레이블을 기반으로 결정된다. 이를 통해 같은 서비스의 로그가 같은 파티션 셋으로 라우팅되어 **데이터 지역성**을 극대화한다.

### 7.2 SegmentationPartitionResolver

```go
// pkg/distributor/segment.go (line 44-75)
type SegmentationPartitionResolver struct {
    perPartitionRateBytes uint64
    ringReader            ring.PartitionRingReader
    logger                log.Logger

    // Metrics.
    failed          prometheus.Counter
    randomlySharded prometheus.Counter
    total           prometheus.Counter
}
```

### 7.3 Resolve 알고리즘

```go
// pkg/distributor/segment.go (line 77-119)
func (r *SegmentationPartitionResolver) Resolve(ctx context.Context,
    tenant string, key SegmentationKey, rateBytes, tenantRateBytes uint64,
) (int32, error) {
    ring := r.ringReader.PartitionRing()

    // [단계 1] 활성 파티션 존재 확인
    if ring.ActivePartitionsCount() == 0 {
        return 0, errors.New("no active partitions")
    }

    // [단계 2] 테넌트 서브링 결정
    //   tenantRateBytes / perPartitionRateBytes = 서브링 크기
    subring, err := r.getTenantSubring(ctx, ring, tenant, tenantRateBytes)

    // [단계 3] 레이트 0이면 랜덤 선택 (fallback)
    if rateBytes == 0 {
        r.randomlySharded.Inc()
        activePartitionIDs := subring.ActivePartitionIDs()
        // 랜덤 셔플 후 첫 번째 선택
        return activePartitionIDs[0], nil
    }

    // [단계 4] 세그멘테이션 키 기반 서브링 결정
    subring, err = r.getSegmentationKeySubring(ctx, subring, key, rateBytes)

    // [단계 5] 랜덤 파티션 선택
    activePartitionIDs := subring.ActivePartitionIDs()
    idx := rand.Intn(len(activePartitionIDs))
    return activePartitionIDs[idx], nil
}
```

### 7.4 2단계 ShuffleShard

```
전체 파티션 링: [0, 1, 2, 3, 4, 5, 6, 7, 8, 9]

Step 1: getTenantSubring("tenant-A", tenantRate=5MB/s)
  → perPartitionRate = 1MB/s
  → 필요 파티션 수 = 5MB/1MB = 5
  → ShuffleShard("tenant-A", 5) → [0, 2, 4, 7, 9]

Step 2: getSegmentationKeySubring("api-server", segRate=2MB/s)
  → 필요 파티션 수 = 2MB/1MB = 2
  → ShuffleShard("api-server", 2) → [2, 7]

최종: 파티션 [2, 7] 중 랜덤 선택
```

이 2단계 ShuffleShard 방식은:
- **테넌트 격리**: 테넌트의 전체 전송률에 비례하여 파티션 할당
- **서비스 지역성**: 같은 서비스 이름의 로그를 최소 파티션 수로 집중
- **핫 파티션 방지**: 고전송률 서비스는 자동으로 더 많은 파티션에 분산

---

## 8. DataObj Consumer: Kafka 소비 → DataObj 생성

소스 경로: `pkg/dataobj/consumer/service.go`, `pkg/dataobj/consumer/processor.go`

### 8.1 서비스 아키텍처

```
┌──────────────────────────────────────────────────────┐
│                DataObj Consumer Service               │
│                                                      │
│  ┌────────────┐   ┌──────────────┐   ┌────────────┐ │
│  │ Lifecycler │   │ PartitionRing│   │ Kafka      │ │
│  │            │   │ Lifecycler   │   │ Consumer   │ │
│  └─────┬──────┘   └──────┬───────┘   └─────┬──────┘ │
│        │                 │                  │        │
│        │     Ring에 자신을 등록            레코드 수신│
│        │     파티션 소유권 선언              │        │
│        │                 │                  │        │
│        │                 │           ┌──────▼──────┐ │
│        │                 │           │ Processor   │ │
│        │                 │           │  - decode   │ │
│        │                 │           │  - append   │ │
│        │                 │           │  - flush    │ │
│        │                 │           └──────┬──────┘ │
│        │                 │                  │        │
│        │                 │           ┌──────▼──────┐ │
│        │                 │           │FlushManager │ │
│        │                 │           │  - sort     │ │
│        │                 │           │  - upload   │ │
│        │                 │           │  - commit   │ │
│        │                 │           └─────────────┘ │
└──────────────────────────────────────────────────────┘
```

### 8.2 Service 구조체

```go
// pkg/dataobj/consumer/service.go (line 37-52)
type Service struct {
    services.Service
    cfg                         Config
    metastoreEvents             *kgo.Client
    lifecycler                  *ring.Lifecycler
    partitionInstanceLifecycler *ring.PartitionInstanceLifecycler
    consumer                    *kafkav2.SinglePartitionConsumer
    offsetReader                *kafkav2.OffsetReader
    partition                   int32
    processor                   *processor
    flusher                     *flusherImpl
    downscalePermitted          downscalePermittedFunc
    watcher                     *services.FailureWatcher
    logger                      log.Logger
    reg                         prometheus.Registerer
}
```

### 8.3 인스턴스 → 파티션 매핑

각 DataObj Consumer 인스턴스는 **정확히 하나의 파티션**을 담당한다:

```go
// pkg/dataobj/consumer/service.go (line 97-102)
instanceID := cfg.LifecyclerConfig.ID
partitionID, err := partitionring.ExtractPartitionID(instanceID)
if err != nil {
    return nil, fmt.Errorf("failed to extract partition ID: %w", err)
}
s.partition = partitionID
```

인스턴스 ID에서 파티션 ID를 추출하여 1:1 매핑을 보장한다.

### 8.4 Processor: 레코드 처리 루프

```go
// pkg/dataobj/consumer/processor.go (line 122-149)
func (p *processor) Run(ctx context.Context) error {
    for {
        select {
        case <-ctx.Done():
            return nil
        case rec, ok := <-p.records:
            if !ok {
                return nil
            }
            if err := p.processRecord(ctx, rec); err != nil {
                level.Error(p.logger).Log("msg", "failed to process record",
                    "err", err)
                p.observeRecordErr(rec)
            }
        case <-time.After(p.idleFlushTimeout):
            // 유휴 상태 → 현재 빌더 플러시
            if _, err := p.idleFlush(ctx); err != nil {
                level.Error(p.logger).Log("msg", "failed to idle flush",
                    "err", err)
            }
        }
    }
}
```

### 8.5 레코드 처리 상세

```go
// pkg/dataobj/consumer/processor.go (line 151-193)
func (p *processor) processRecord(ctx context.Context, rec *kgo.Record) error {
    now := time.Now()
    p.observeRecord(rec, now)

    // Kafka 레코드에서 테넌트 ID와 스트림 디코딩
    tenant := string(rec.Key)
    stream, err := p.decoder.DecodeWithoutLabels(rec.Value)
    if err != nil {
        return fmt.Errorf("failed to decode stream: %w", err)
    }

    // 빌더 최대 수명 체크 → 플러시 필요 시 플러시
    if p.shouldFlushDueToMaxAge() {
        if err := p.flush(ctx, flushReasonMaxAge); err != nil {
            return fmt.Errorf("failed to flush: %w", err)
        }
    }

    // 빌더에 스트림 추가
    if err := p.builder.Append(tenant, stream); err != nil {
        if !errors.Is(err, logsobj.ErrBuilderFull) {
            return fmt.Errorf("failed to append stream: %w", err)
        }
        // 빌더가 가득 참 → 플러시 후 재시도
        if err := p.flush(ctx, flushReasonBuilderFull); err != nil {
            return fmt.Errorf("failed to flush and commit: %w", err)
        }
        if err := p.builder.Append(tenant, stream); err != nil {
            return fmt.Errorf("failed to append stream after flushing: %w", err)
        }
    }

    p.lastOffset = rec.Offset
    return nil
}
```

### 8.6 플러시 트리거 조건

DataObj Consumer는 세 가지 조건에서 플러시를 수행한다:

| 조건 | 설정 | 설명 |
|------|------|------|
| **빌더 가득 참** | `target_size` | 빌더가 목표 크기에 도달 |
| **최대 수명 초과** | `max_builder_age` | 빌더가 생성된 지 너무 오래됨 |
| **유휴 타임아웃** | `idle_flush_timeout` | 새 레코드가 일정 시간 없음 |

```
레코드 수신 → processRecord()
     │
     ├── 빌더 가득 참? → flush(builderFull)
     │
     ├── 최대 수명 초과? → flush(maxAge)
     │
     └── 타임아웃? → idleFlush()
              │
              ▼
         FlushManager.Flush()
              │
              ├── Sort (시간순 정렬)
              │
              ├── Upload (Object Storage에 업로드)
              │
              ├── Metastore Event 발행
              │
              └── Offset Commit (Kafka 오프셋 커밋)
```

### 8.7 시작/중지 생명주기

```go
// pkg/dataobj/consumer/service.go (line 190-208)
func (s *Service) starting(ctx context.Context) error {
    // 1. 이전 오프셋에서 재개
    if err := s.initResumeOffset(ctx); err != nil {
        return fmt.Errorf("failed to initialize offset: %w", err)
    }
    // 2. Ring 등록
    if err := services.StartAndAwaitRunning(ctx, s.lifecycler); err != nil { ... }
    // 3. 파티션 Ring 등록 (활성 파티션 선언)
    if err := services.StartAndAwaitRunning(ctx, s.partitionInstanceLifecycler); err != nil { ... }
    // 4. Processor 시작
    if err := services.StartAndAwaitRunning(ctx, s.processor); err != nil { ... }
    // 5. Kafka Consumer 시작
    if err := services.StartAndAwaitRunning(ctx, s.consumer); err != nil { ... }
    return nil
}
```

---

## 9. 메트릭: Kafka 쓰기 경로 관측

### 9.1 Distributor Kafka 메트릭

소스 경로: `pkg/distributor/distributor.go` (line 379-403)

| 메트릭 이름 | 타입 | 레이블 | 설명 |
|-------------|------|--------|------|
| `loki_distributor_kafka_appends_total` | Counter | partition, status | Kafka 쓰기 시도 수 |
| `loki_distributor_kafka_latency_seconds` | Histogram | - | Kafka 쓰기 레이턴시 |
| `loki_distributor_kafka_sent_bytes_total` | Counter | - | Kafka 전송 총 바이트 |
| `loki_distributor_kafka_records_per_write_request` | Histogram | - | 요청당 레코드 수 |

### 9.2 DataObj Tee 메트릭

소스 경로: `pkg/distributor/dataobj_tee.go` (line 99-117)

| 메트릭 이름 | 타입 | 레이블 | 설명 |
|-------------|------|--------|------|
| `loki_distributor_dataobj_tee_duplicate_streams_total` | Counter | - | 복제된 스트림 수 |
| `loki_distributor_dataobj_tee_duplicate_stream_failures_total` | Counter | - | 복제 실패 수 |
| `loki_distributor_dataobj_tee_produced_bytes_total` | Counter | partition, tenant*, key* | 파티션별 전송 바이트 |
| `loki_distributor_dataobj_tee_produced_records_total` | Counter | partition, tenant*, key* | 파티션별 전송 레코드 수 |

*`tenant`와 `segmentation_key` 레이블은 `debug_metrics_enabled=true`일 때만 활성화*

### 9.3 SegmentationPartitionResolver 메트릭

소스 경로: `pkg/distributor/segment.go` (line 57-75)

| 메트릭 이름 | 타입 | 설명 |
|-------------|------|------|
| `loki_distributor_segmentation_partition_resolver_keys_total` | Counter | 전체 해석 시도 |
| `loki_distributor_segmentation_partition_resolver_keys_failed_total` | Counter | 해석 실패 수 |
| `loki_distributor_segmentation_partition_resolver_keys_randomly_sharded_total` | Counter | 랜덤 선택 fallback 수 |

### 9.4 모니터링 대시보드 핵심 지표

```
Kafka 쓰기 경로 건강 상태 점검:

1. 실패율:
   rate(loki_distributor_kafka_appends_total{status="fail"}[5m])
   ─────────────────────────────────────────────────────────
   rate(loki_distributor_kafka_appends_total[5m])

2. 레이턴시 P99:
   histogram_quantile(0.99, rate(loki_distributor_kafka_latency_seconds_bucket[5m]))

3. 처리량:
   rate(loki_distributor_kafka_sent_bytes_total[5m])

4. 랜덤 셔딩 비율 (높으면 레이트 조회 문제):
   rate(loki_distributor_segmentation_partition_resolver_keys_randomly_sharded_total[5m])
   ─────────────────────────────────────────────────────────────────────────────────────
   rate(loki_distributor_segmentation_partition_resolver_keys_total[5m])
```

---

## 10. 장애 처리: 재시도와 파티션 재할당

### 10.1 에러 코드 체계

소스 경로: `pkg/distributor/dataobj_tee.go` (line 22-28)

```go
type TeeErrorCodes int

const (
    TeeCouldntSolvePartitionError TeeErrorCodes = 1000
    TeeCouldntEncodeStreamError   TeeErrorCodes = 1001
    TeeCouldntProduceRecordsError TeeErrorCodes = 1002
)
```

### 10.2 장애 시나리오별 처리

```
┌─────────────────────────────────────────────────────────┐
│                  장애 시나리오 매트릭스                    │
├────────────────────┬──────────────────┬─────────────────┤
│ 시나리오           │ 에러 코드         │ 처리 방식        │
├────────────────────┼──────────────────┼─────────────────┤
│ 활성 파티션 없음   │ 1000             │ 에러 반환,       │
│                    │                  │ 메트릭 기록      │
├────────────────────┼──────────────────┼─────────────────┤
│ 레코드 인코딩 실패 │ 1001             │ 에러 반환,       │
│                    │                  │ 스킵 가능        │
├────────────────────┼──────────────────┼─────────────────┤
│ Kafka 프로듀스     │ 1002             │ 에러 반환,       │
│ 실패               │                  │ Push 실패 전파   │
├────────────────────┼──────────────────┼─────────────────┤
│ 파티션 재할당      │ -                │ ring 변경 감지,  │
│                    │                  │ 자동 라우팅 변경 │
├────────────────────┼──────────────────┼─────────────────┤
│ Consumer 장애      │ -                │ 오프셋 미커밋,   │
│                    │                  │ 재시작 시 재소비 │
└────────────────────┴──────────────────┴─────────────────┘
```

### 10.3 DataObj Consumer의 재시도

```go
// pkg/dataobj/consumer/service.go (line 247-264)
func (s *Service) initResumeOffset(ctx context.Context) error {
    b := backoff.New(ctx, backoff.Config{
        MinBackoff: 100 * time.Millisecond,
        MaxBackoff: 10 * time.Second,
        MaxRetries: 3,
    })
    var lastErr error
    for b.Ongoing() {
        initialOffset, err := s.offsetReader.ResumeOffset(ctx, s.partition)
        if err == nil {
            lastErr = s.consumer.SetInitialOffset(initialOffset)
            break
        }
        lastErr = fmt.Errorf("failed to fetch resume offset: %w", err)
        b.Wait()
    }
    return lastErr
}
```

Consumer는 재시작 시 마지막 커밋된 오프셋에서 재개한다. 이는 **at-least-once** 의미론을 보장한다.

### 10.4 Uploader의 재시도

```go
// pkg/dataobj/uploader/uploader.go (line 86-138)
func (d *Uploader) Upload(ctx context.Context, object *dataobj.Object) (key string, err error) {
    backoff := backoff.New(ctx, backoff.Config{
        MinBackoff: 100 * time.Millisecond,
        MaxBackoff: 10 * time.Second,
        MaxRetries: 20,
    })
    for backoff.Ongoing() {
        err = func() error {
            reader, err := object.Reader(ctx)
            if err != nil { return err }
            defer reader.Close()
            return d.bucket.Upload(ctx, objectPath, reader)
        }()
        if err == nil { break }
        backoff.Wait()
    }
    // ...
}
```

Object Storage 업로드는 최대 20번까지 지수 백오프로 재시도한다.

### 10.5 파티션 재할당과 PartitionRing

파티션 링의 변경(예: Consumer 스케일 업/다운)은 자동으로 감지된다:

```
스케일 다운 시나리오:

1. Consumer-2가 종료 신호 수신
2. Consumer-2의 파티션이 INACTIVE로 마킹
3. Distributor가 ring 변경 감지
4. 해당 파티션으로의 새 쓰기 중단
5. Consumer-2가 남은 데이터 플러시
6. Consumer-2 완전 종료

스케일 업 시나리오:

1. Consumer-5가 시작
2. 새 파티션 할당 및 ACTIVE 마킹
3. Distributor가 ring 변경 감지
4. 새 파티션으로 쓰기 시작
5. Consumer-5가 오프셋 0부터 (또는 마지막 커밋 위치에서) 소비 시작
```

---

## 11. 설계 결정: 왜 Kafka인가?

### 11.1 디커플링

```
Before (강결합):
  Distributor ────► Ingester
  │                    │
  │   Ingester 장애    │
  │   = Push 실패      │
  └────────────────────┘

After (Kafka 디커플링):
  Distributor ────► Kafka ────► Consumer
  │                   │
  │   Consumer 장애   │
  │   = Kafka에 버퍼  │
  │   = Push 성공     │
  └───────────────────┘
```

Kafka가 중간 버퍼 역할을 하여 Consumer(또는 Ingester)의 일시적 장애가 쓰기 경로에 영향을 주지 않는다.

### 11.2 버퍼링

Kafka는 디스크 기반 로그이므로 메모리 제한 없이 대량의 데이터를 버퍼링할 수 있다. 이는 트래픽 스파이크 시 Consumer가 처리 속도를 따라잡을 수 있는 시간적 여유를 제공한다.

### 11.3 재처리

Kafka의 오프셋 기반 소비 모델 덕분에:
- **오프셋 리셋**: Consumer의 오프셋을 과거로 되돌려 데이터 재처리 가능
- **다중 Consumer**: 같은 데이터를 여러 Consumer가 독립적으로 소비 가능
- **DataObj Tee**: Ingester 경로와 DataObj 경로가 같은 Kafka 데이터를 독립적으로 소비

### 11.4 컬럼나 저장과의 시너지

DataObj Consumer는 Kafka에서 소비한 데이터를 **컬럼나 포맷(DataObj)**으로 변환하여 Object Storage에 저장한다. 이는 기존의 청크 기반 저장 방식 대비:

- **쿼리 효율성**: 필요한 컬럼만 읽기 가능
- **압축률**: 같은 타입의 데이터가 함께 저장되어 압축 효율 향상
- **스키마 진화**: 새 컬럼 추가가 용이

### 11.5 아키텍처 진화 방향

```
현재 (Dual Write):
  Distributor ──┬──► Ingester (기존 경로)
                │
                └──► Kafka ──► DataObj Consumer ──► Object Storage

미래 (Kafka 단일 경로):
  Distributor ──► Kafka ──┬──► Ingester (Kafka에서 소비)
                          │
                          └──► DataObj Consumer ──► Object Storage
```

현재 듀얼 라이트는 점진적 마이그레이션을 위한 과도기적 설계다. 궁극적으로는 Kafka가 유일한 쓰기 경로가 되어 Ingester도 Kafka에서 데이터를 소비하는 구조로 진화할 가능성이 높다.

---

## 정리

Loki의 Kafka 통합은 단순한 메시지 큐 추가가 아니라, **쓰기 경로의 근본적인 재설계**다. 핵심 설계 원칙은:

1. **점진적 마이그레이션**: `KafkaEnabled`와 `IngesterEnabled` 플래그로 듀얼 라이트 지원
2. **데이터 지역성**: SegmentationKey 기반 2단계 ShuffleShard로 같은 서비스의 로그를 최소 파티션에 집중
3. **내결함성**: PushTracker로 모든 경로의 완료를 보장, at-least-once 의미론
4. **확장성**: 파티션 링 기반으로 Consumer의 수평 스케일링 지원
5. **관측 가능성**: 상세한 메트릭 시스템으로 Kafka 쓰기 경로의 모든 단계를 모니터링

| 구성요소 | 소스 경로 | 역할 |
|---------|----------|------|
| Distributor | `pkg/distributor/distributor.go` | Kafka 프로듀서, 듀얼 라이트 조율 |
| DataObj Tee | `pkg/distributor/dataobj_tee.go` | 추가 Kafka 토픽으로 데이터 복제 |
| SegmentationResolver | `pkg/distributor/segment.go` | 세그멘테이션 키 기반 파티션 결정 |
| DataObj Consumer | `pkg/dataobj/consumer/service.go` | Kafka 소비 → DataObj 생성 |
| Processor | `pkg/dataobj/consumer/processor.go` | 레코드 디코딩 및 빌더 관리 |
| Uploader | `pkg/dataobj/uploader/uploader.go` | Object Storage 업로드 |
