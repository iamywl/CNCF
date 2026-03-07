# 08. Memory & Badger 임베디드 스토리지 Deep-Dive

## 목차

1. [개요](#1-개요)
2. [Memory 스토리지](#2-memory-스토리지)
3. [Badger 스토리지](#3-badger-스토리지)
4. [비교 분석](#4-비교-분석)
5. [운영 가이드](#5-운영-가이드)

---

## 1. 개요

Jaeger는 외부 스토리지(Elasticsearch, Cassandra, Kafka) 없이도 동작할 수 있는 **임베디드 스토리지** 두 가지를 제공한다.

```
┌──────────────────────────────────────────────────────────┐
│                   Jaeger 스토리지 계층                      │
├─────────────────────┬────────────────────────────────────┤
│   임베디드 스토리지     │        외부 스토리지                  │
│                     │                                    │
│  ┌───────────────┐  │  ┌──────────┐  ┌───────────────┐   │
│  │   Memory      │  │  │   ES     │  │  Cassandra    │   │
│  │  (In-Process) │  │  │(Remote)  │  │  (Remote)     │   │
│  └───────────────┘  │  └──────────┘  └───────────────┘   │
│  ┌───────────────┐  │  ┌──────────┐                      │
│  │   Badger      │  │  │  Kafka   │                      │
│  │(Embedded KV)  │  │  │(Streaming│                      │
│  └───────────────┘  │  └──────────┘                      │
└─────────────────────┴────────────────────────────────────┘
```

| 특성 | Memory | Badger |
|------|--------|--------|
| 데이터 저장 위치 | 프로세스 힙 메모리 | 디스크 (LSM-tree) |
| 재시작 시 데이터 | 소실 | 유지 (ephemeral=false) |
| 주 사용처 | all-in-one 모드, 테스트 | 소규모 단독 배포 |
| 멀티테넌시 | 지원 (perTenant 맵) | 미지원 |
| API 버전 | v2 네이티브 (OTLP) | v1 (Jaeger model) + v1adapter 래핑 |

### 왜 두 가지 임베디드 스토리지가 필요한가?

- **Memory**: 별도 설정 없이 즉시 시작 가능. 개발/테스트/데모 환경에서 최소한의 오버헤드로 Jaeger를 실행할 때 적합하다. all-in-one 바이너리의 기본 스토리지다.
- **Badger**: 외부 DB 없이도 **프로세스 재시작 후 데이터를 보존**해야 하는 소규모 프로덕션 환경에서 사용한다. Dgraph의 Badger는 Go 네이티브 LSM-tree 기반 KV 스토어로, 별도 프로세스 없이 임베딩된다.

---

## 2. Memory 스토리지

### 2.1 아키텍처 개요

Memory 스토리지는 Jaeger v2의 OTLP 네이티브 스토리지다. v1adapter를 거치지 않고 `ptrace.Traces`를 직접 저장/조회한다.

```
┌─────────────────────────────────────────────────────────┐
│                    Factory (memory)                      │
│  internal/storage/v2/memory/factory.go                  │
│                                                         │
│  NewFactory(cfg, telset)                                │
│    ├── NewStore(cfg)  ─────────────────────────┐        │
│    └── metricsFactory                          │        │
│                                                │        │
│  CreateTraceReader()  → ReaderDecorator(store)  │        │
│  CreateTraceWriter()  → store (직접 반환)        │        │
│  CreateDependencyReader() → store               │        │
│  CreateSamplingStore()    → SamplingStore        │        │
│  CreateLock()             → Lock{}              │        │
│  Purge()                  → store.Purge()       │        │
└─────────────────────────────────────────────────────────┘
```

**소스 위치**: `internal/storage/v2/memory/factory.go`

```go
// Factory는 tracestore.Factory, storage.SamplingStoreFactory,
// storage.Purger 인터페이스를 모두 구현한다.
type Factory struct {
    store          *Store
    metricsFactory metrics.Factory
}

func NewFactory(cfg Configuration, telset telemetry.Settings) (*Factory, error) {
    store, err := NewStore(cfg)
    if err != nil {
        return nil, err
    }
    return &Factory{
        store:          store,
        metricsFactory: telset.Metrics,
    }, nil
}
```

핵심 설계 포인트:
- `Store`가 `tracestore.Reader`와 `tracestore.Writer`를 **동시에** 구현한다
- Reader 반환 시 `tracestoremetrics.NewReaderDecorator`로 감싸서 메트릭을 자동 수집한다
- Writer는 Store를 직접 반환한다 (추가 래핑 없음)

### 2.2 Configuration

**소스 위치**: `internal/storage/v2/memory/config.go`

```go
type Configuration struct {
    // MaxTraces는 메모리에 저장할 최대 트레이스 수.
    // 멀티테넌시 활성화 시, 이 제한은 테넌트별로 적용된다.
    // 0이면 무제한 (경고: 메모리 사용량이 무한정 증가).
    MaxTraces int `mapstructure:"max_traces"`
}
```

| 설정 항목 | 기본값 | 설명 |
|-----------|--------|------|
| `max_traces` | 0 (무제한) | 링 버퍼 크기. all-in-one에서는 보통 100000으로 설정 |

`MaxTraces`가 0 이하이면 `NewStore`에서 `errInvalidMaxTraces` 에러를 반환한다.

### 2.3 Store 구조체와 멀티테넌시

**소스 위치**: `internal/storage/v2/memory/memory.go`

```go
type Store struct {
    mu        sync.RWMutex
    cfg       Configuration
    perTenant map[string]*Tenant   // 테넌트별 격리된 저장소
}
```

Store는 **테넌트별로 완전히 격리된 데이터를 유지**한다. `perTenant` 맵의 키는 컨텍스트에서 추출한 `tenantID`이며, 각 테넌트는 독립적인 `Tenant` 인스턴스를 가진다.

```
┌─────────────────────────────────────────────┐
│                   Store                      │
│   mu: sync.RWMutex                          │
│   cfg: Configuration                        │
│   perTenant:                                │
│     ┌──────────┬─────────────────────────┐  │
│     │"tenant-A"│ → Tenant{traces, ids..} │  │
│     ├──────────┼─────────────────────────┤  │
│     │"tenant-B"│ → Tenant{traces, ids..} │  │
│     ├──────────┼─────────────────────────┤  │
│     │   ""     │ → Tenant{traces, ids..} │  │
│     │(default) │   (단일 테넌트 모드)       │  │
│     └──────────┴─────────────────────────┘  │
└─────────────────────────────────────────────┘
```

`getTenant` 메서드는 **double-checked locking** 패턴을 사용한다:

```go
func (st *Store) getTenant(tenantID string) *Tenant {
    st.mu.RLock()
    tenant, ok := st.perTenant[tenantID]
    st.mu.RUnlock()
    if !ok {
        st.mu.Lock()
        defer st.mu.Unlock()
        tenant, ok = st.perTenant[tenantID]  // 재확인
        if !ok {
            tenant = newTenant(&st.cfg)
            st.perTenant[tenantID] = tenant
        }
    }
    return tenant
}
```

이 패턴을 사용하는 이유: 대부분의 요청에서 테넌트가 이미 존재하므로 RLock만으로 처리할 수 있다. 새 테넌트가 생성되는 경우는 드물기 때문에 Write Lock의 경합을 최소화한다.

### 2.4 Tenant: 링 버퍼 기반 트레이스 저장

**소스 위치**: `internal/storage/v2/memory/tenant.go`

```go
type Tenant struct {
    mu     sync.RWMutex
    config *Configuration

    ids        map[pcommon.TraceID]int  // traceID → traces[] 내 인덱스
    traces     []traceAndId            // 링 버퍼
    mostRecent int                     // 가장 최근 추가된 위치

    services   map[string]struct{}
    operations map[string]map[tracestore.Operation]struct{}
}

type traceAndId struct {
    id        pcommon.TraceID
    trace     ptrace.Traces
    startTime time.Time
    endTime   time.Time
}
```

핵심 자료구조:

```
링 버퍼 (traces[]):  MaxTraces = 5 인 경우

인덱스:  [0]    [1]    [2]    [3]    [4]
         ┌──────┬──────┬──────┬──────┬──────┐
         │Trace │Trace │Trace │Trace │Trace │
         │  D   │  E   │  F   │  B   │  C   │
         └──────┴──────┴──────┴──────┴──────┘
                   ↑
            mostRecent = 2

ids 맵:
  TraceID_B → 3
  TraceID_C → 4
  TraceID_D → 0
  TraceID_E → 1
  TraceID_F → 2  (가장 최근)

새 트레이스 G 추가 시:
  mostRecent = (2+1) % 5 = 3
  traces[3]의 기존 TraceID_B를 ids에서 삭제
  traces[3] = Trace_G
  ids[TraceID_G] = 3
```

### 2.5 WriteTraces: ResourceSpans 재구성(Reshuffle)

**소스 위치**: `internal/storage/v2/memory/memory.go`

`WriteTraces`는 수신된 OTLP 데이터를 **TraceID 기준으로 재구성**한 뒤 저장한다. 이는 하나의 `ptrace.Traces` 배치 안에 여러 트레이스의 스팬이 섞여 있을 수 있기 때문이다.

```go
func (st *Store) WriteTraces(ctx context.Context, td ptrace.Traces) error {
    resourceSpansByTraceId := reshuffleResourceSpans(td.ResourceSpans())
    m := st.getTenant(tenancy.GetTenant(ctx))
    m.storeTraces(resourceSpansByTraceId)
    return nil
}
```

재구성 과정 (3단계):

```
입력: ptrace.Traces
  ResourceSpan-1 (service=A):
    ScopeSpan-1: [Span(T1), Span(T2)]
    ScopeSpan-2: [Span(T1)]
  ResourceSpan-2 (service=B):
    ScopeSpan-3: [Span(T1), Span(T2)]

     │
     ▼  reshuffleResourceSpans()
         │
         ├── reshuffleScopeSpans()    ← 각 ResourceSpan 내부의 ScopeSpan 재구성
         │   └── reshuffleSpans()     ← 각 ScopeSpan 내부의 Span 재구성
         │
         ▼
출력: map[TraceID]ResourceSpansSlice
  TraceID_1:
    ResourceSpan-1 (service=A):
      ScopeSpan-1: [Span(T1)]
      ScopeSpan-2: [Span(T1)]
    ResourceSpan-2 (service=B):
      ScopeSpan-3: [Span(T1)]
  TraceID_2:
    ResourceSpan-1 (service=A):
      ScopeSpan-1: [Span(T2)]
    ResourceSpan-2 (service=B):
      ScopeSpan-3: [Span(T2)]
```

`reshuffleSpans` 함수의 실제 구현:

```go
func reshuffleSpans(spanSlice ptrace.SpanSlice) map[pcommon.TraceID]ptrace.SpanSlice {
    spansByTraceId := make(map[pcommon.TraceID]ptrace.SpanSlice)
    for _, span := range spanSlice.All() {
        spansSlice, ok := spansByTraceId[span.TraceID()]
        if !ok {
            spansSlice = ptrace.NewSpanSlice()
            spansByTraceId[span.TraceID()] = spansSlice
        }
        span.CopyTo(spansSlice.AppendEmpty())
    }
    return spansByTraceId
}
```

### 2.6 storeTraces: 링 버퍼 저장 로직

**소스 위치**: `internal/storage/v2/memory/tenant.go` (60~118행)

```go
func (t *Tenant) storeTraces(tracesById map[pcommon.TraceID]ptrace.ResourceSpansSlice) {
    t.mu.Lock()
    defer t.mu.Unlock()
    for traceId, sameTraceIDResourceSpan := range tracesById {
        // 서비스명과 오퍼레이션 메타데이터 추출/캐싱
        // ...

        // 이미 존재하는 트레이스인 경우: 기존 트레이스에 병합
        if index, ok := t.ids[traceId]; ok {
            sameTraceIDResourceSpan.MoveAndAppendTo(t.traces[index].trace.ResourceSpans())
            // startTime, endTime 갱신
            continue
        }

        // 새 트레이스: 링 버퍼에 추가
        traces := ptrace.NewTraces()
        sameTraceIDResourceSpan.MoveAndAppendTo(traces.ResourceSpans())
        t.mostRecent = (t.mostRecent + 1) % t.config.MaxTraces

        // 덮어쓸 위치에 기존 트레이스가 있으면 ids 맵에서 제거
        if !t.traces[t.mostRecent].id.IsEmpty() {
            delete(t.ids, t.traces[t.mostRecent].id)
        }

        t.ids[traceId] = t.mostRecent
        t.traces[t.mostRecent] = traceAndId{
            id: traceId, trace: traces,
            startTime: startTime, endTime: endTime,
        }
    }
}
```

이 설계의 핵심 장점:
1. **O(1) 추가**: 링 버퍼이므로 새 트레이스 추가는 상수 시간
2. **자동 퇴출**: `MaxTraces`를 초과하면 가장 오래된 트레이스가 자동으로 교체됨
3. **중복 병합**: 동일 TraceID의 스팬이 분할 도착해도 기존 트레이스에 병합

### 2.7 읽기 연산

#### GetServices

```go
func (st *Store) GetServices(ctx context.Context) ([]string, error) {
    m := st.getTenant(tenancy.GetTenant(ctx))
    m.mu.RLock()
    defer m.mu.RUnlock()
    var retMe []string
    for k := range m.services {
        retMe = append(retMe, k)
    }
    return retMe, nil
}
```

`services`는 `map[string]struct{}`로, 쓰기 시점에 이미 채워져 있다. 별도 인덱스 스캔 없이 맵 순회만으로 응답한다.

#### GetOperations

```go
func (st *Store) GetOperations(ctx context.Context, query tracestore.OperationQueryParams) ([]tracestore.Operation, error) {
    m := st.getTenant(tenancy.GetTenant(ctx))
    m.mu.RLock()
    defer m.mu.RUnlock()
    var retMe []tracestore.Operation
    if operations, ok := m.operations[query.ServiceName]; ok {
        for operation := range operations {
            if query.SpanKind == "" || query.SpanKind == operation.SpanKind {
                retMe = append(retMe, operation)
            }
        }
    }
    return retMe, nil
}
```

`operations`는 `map[string]map[tracestore.Operation]struct{}`로, 서비스명을 키로 하여 해당 서비스의 모든 오퍼레이션을 저장한다. SpanKind 필터링도 지원한다.

#### FindTraces

```go
func (t *Tenant) findTraceAndIds(query tracestore.TraceQueryParams) ([]traceAndId, error) {
    if query.SearchDepth <= 0 || query.SearchDepth > t.config.MaxTraces {
        return nil, errInvalidSearchDepth
    }
    t.mu.RLock()
    defer t.mu.RUnlock()
    traceAndIds := make([]traceAndId, 0, query.SearchDepth)
    n := len(t.traces)
    for i := range t.traces {
        if len(traceAndIds) == query.SearchDepth {
            break
        }
        // 링 버퍼를 최신 → 과거 순서로 순회
        index := (t.mostRecent - i + n) % n
        traceById := t.traces[index]
        if traceById.id.IsEmpty() {
            break  // 링 버퍼 빈 공간 도달
        }
        if validTrace(traceById.trace, query) {
            traceAndIds = append(traceAndIds, traceById)
        }
    }
    return traceAndIds, nil
}
```

검색 전략:
- **최신 트레이스부터 역순 스캔**: `mostRecent` 위치에서 시작하여 과거로 이동
- **SearchDepth 제한**: 전체 링 버퍼를 스캔하지 않고 지정된 깊이만큼만 탐색
- **빈 슬롯 감지**: `id.IsEmpty()`이면 아직 채워지지 않은 영역이므로 조기 종료

#### validTrace / validSpan 필터링

`validSpan` 함수는 다양한 쿼리 조건을 검사한다:

| 조건 | 검사 대상 | 비고 |
|------|----------|------|
| `ServiceName` | Resource의 `service.name` 어트리뷰트 | 빈 문자열이면 전체 매칭 |
| `OperationName` | Span의 `Name()` | 빈 문자열이면 전체 매칭 |
| `StartTimeMin/Max` | Span의 시작 타임스탬프 | 시간 범위 필터 |
| `DurationMin/Max` | EndTimestamp - StartTimestamp | 지속시간 범위 필터 |
| `error` 어트리뷰트 | `span.Status().Code()` | StatusCodeError 검사 |
| `span.status` 어트리뷰트 | 상태 코드 (OK/ERROR/UNSET) | 문자열 → StatusCode 변환 |
| `span.kind` 어트리뷰트 | SpanKind (CLIENT/SERVER 등) | 문자열 → SpanKind 변환 |
| `scope.name/version` | InstrumentationScope | 스코프 메타데이터 필터 |
| `resource.*` 접두사 | Resource 어트리뷰트 | 리소스 전용 필터 |
| 일반 키-값 | Span/Scope/Resource 어트리뷰트, Events, Links | 모든 위치에서 검색 |

### 2.8 Dependency 계산

**소스 위치**: `internal/storage/v2/memory/tenant.go` (159~204행)

Memory 스토리지는 의존성 데이터를 별도로 저장하지 않는다. `getDependencies` 호출 시 저장된 트레이스를 실시간으로 분석하여 서비스 간 호출 관계를 계산한다.

```go
func (t *Tenant) getDependencies(query depstore.QueryParameters) ([]model.DependencyLink, error) {
    // 시간 범위 내의 트레이스만 대상
    for _, index := range t.ids {
        traceWithTime := t.traces[index]
        if !traceWithTime.traceIsBetweenStartAndEnd(query.StartTime, query.EndTime) {
            continue
        }
        // 각 스팬의 ParentSpanID로 부모 서비스 → 자식 서비스 관계 추출
        // depKey = "parentService&&&childService"
    }
}
```

### 2.9 SamplingStore

**소스 위치**: `internal/storage/v2/memory/sampling.go`

```go
type SamplingStore struct {
    mu                  sync.RWMutex
    throughputs         []*storedThroughput
    probabilitiesAndQPS *storedServiceOperationProbabilitiesAndQPS
    maxBuckets          int
}
```

적응형 샘플링을 위한 처리량(throughput)과 확률(probability) 데이터를 메모리에 저장한다. `maxBuckets`로 최대 버킷 수를 제한하며, `preprendThroughput`으로 최신 데이터를 앞에 추가하고 오래된 데이터를 제거한다.

---

## 3. Badger 스토리지

### 3.1 Badger란?

Badger는 Dgraph에서 개발한 Go 네이티브 **LSM-tree(Log-Structured Merge-tree)** 기반 임베디드 키-값 저장소다.

```
                  Badger 내부 구조
┌────────────────────────────────────────────┐
│              MemTable (Active)              │
│         ┌─────────────────────────┐        │
│         │  키-값 쌍 (정렬된 상태)    │        │
│         └──────────┬──────────────┘        │
│                    │ flush                  │
│         ┌──────────▼──────────────┐        │
│         │      WAL (Write-Ahead   │        │
│         │           Log)          │        │
│         └──────────┬──────────────┘        │
│                    │                       │
│  ┌─────────────────▼───────────────────┐   │
│  │          SSTable (Level 0)          │   │
│  ├─────────────────────────────────────┤   │
│  │          SSTable (Level 1)          │   │
│  ├─────────────────────────────────────┤   │
│  │          SSTable (Level 2)          │   │
│  └─────────────────────────────────────┘   │
│                                            │
│  Key-Value 분리 (Value Separation):        │
│  ┌──────────┐    ┌──────────────────┐      │
│  │ Key Dir  │    │   Value Log      │      │
│  │ (SST에   │    │ (별도 vlog       │      │
│  │  키+포인터)│    │  파일에 값 저장)  │      │
│  └──────────┘    └──────────────────┘      │
└────────────────────────────────────────────┘
```

Badger의 핵심 특성:
- **Key-Value Separation**: WiscKey 논문 기반. 키는 LSM-tree에, 큰 값은 별도 Value Log에 저장하여 쓰기 증폭(Write Amplification)을 줄인다
- **순수 Go 구현**: CGo 의존성 없이 Go 표준 라이브러리만으로 구현
- **TTL 지원**: 엔트리별 만료 시간 설정 가능
- **트랜잭션**: ACID 트랜잭션 지원 (MVCC 기반)

### 3.2 Factory 구조 (v1 + v2 래핑)

Badger 스토리지는 v1 API로 구현되어 있으며, v2 Factory가 이를 래핑한다.

```
┌─────────────────────────────────────────────────────────┐
│                v2/badger/Factory                         │
│  internal/storage/v2/badger/factory.go                  │
│                                                         │
│  ┌───────────────────────────────────────────────────┐  │
│  │              v1/badger/Factory                     │  │
│  │  internal/storage/v1/badger/factory.go            │  │
│  │                                                   │  │
│  │  store: *badger.DB                                │  │
│  │  cache: *CacheStore                               │  │
│  │  maintenanceDone: chan bool                        │  │
│  │                                                   │  │
│  │  goroutine: maintenance()   ← ValueLogGC 실행     │  │
│  │  goroutine: metricsCopier() ← 메트릭 수집          │  │
│  └───────────────────────────────────────────────────┘  │
│                                                         │
│  CreateTraceWriter():                                   │
│    v1Writer = v1Factory.CreateSpanWriter()               │
│    return v1adapter.NewTraceWriter(v1Writer)             │
│                                                         │
│  CreateTraceReader():                                   │
│    v1Reader = v1Factory.CreateSpanReader()               │
│    return v1adapter.NewTraceReader(v1Reader)             │
│                                                         │
│  CreateDependencyReader():                              │
│    v1Reader = v1Factory.CreateDependencyReader()         │
│    return v1adapter.NewDependencyReader(v1Reader)        │
│                                                         │
└─────────────────────────────────────────────────────────┘
```

**v2 Factory** (`internal/storage/v2/badger/factory.go`):

```go
type Factory struct {
    v1Factory *badger.Factory
}

func NewFactory(cfg badger.Config, telset telemetry.Settings) (*Factory, error) {
    v1Factory := badger.NewFactory()
    v1Factory.Config = &cfg
    err := v1Factory.Initialize(telset.Metrics, telset.Logger)
    if err != nil {
        return nil, err
    }
    return &Factory{v1Factory: v1Factory}, nil
}
```

**v1 Factory** (`internal/storage/v1/badger/factory.go`):

```go
type Factory struct {
    Config         *Config
    store          *badger.DB       // Badger DB 인스턴스
    cache          *badgerstore.CacheStore
    logger         *zap.Logger
    metricsFactory metrics.Factory

    tmpDir          string           // ephemeral 모드 시 임시 디렉토리
    maintenanceDone chan bool        // 백그라운드 고루틴 종료 시그널
    bgWg            sync.WaitGroup  // 백그라운드 고루틴 대기
    // ...
}
```

### 3.3 Initialize: DB 열기와 백그라운드 작업 시작

**소스 위치**: `internal/storage/v1/badger/factory.go` (81~138행)

```go
func (f *Factory) Initialize(metricsFactory metrics.Factory, logger *zap.Logger) error {
    opts := badger.DefaultOptions("")

    if f.Config.Ephemeral {
        // 임시 디렉토리에 저장, SyncWrites 비활성화
        opts.SyncWrites = false
        dir, _ := os.MkdirTemp("", "badger")
        f.tmpDir = dir
        opts.Dir = f.tmpDir
        opts.ValueDir = f.tmpDir
    } else {
        // 영구 디렉토리 초기화
        initializeDir(f.Config.Directories.Keys)
        initializeDir(f.Config.Directories.Values)
        opts.SyncWrites = f.Config.SyncWrites
        opts.Dir = f.Config.Directories.Keys
        opts.ValueDir = f.Config.Directories.Values
        opts.ReadOnly = f.Config.ReadOnly
    }

    store, err := badger.Open(opts)
    // ...
    f.cache = badgerstore.NewCacheStore(f.store, f.Config.TTL.Spans)

    // 백그라운드 고루틴 2개 시작
    f.bgWg.Add(2)
    go func() { defer f.bgWg.Done(); f.maintenance() }()
    go func() { defer f.bgWg.Done(); f.metricsCopier() }()

    return nil
}
```

초기화 흐름도:

```
NewFactory(cfg, telset)
    │
    ▼
Initialize(metricsFactory, logger)
    │
    ├── Ephemeral?
    │   ├── Yes: os.MkdirTemp() → tmpfs에 저장
    │   └── No:  initializeDir(keys), initializeDir(values) → 영구 디렉토리
    │
    ├── badger.Open(opts) → *badger.DB
    │
    ├── NewCacheStore(db, ttl) → *CacheStore
    │
    ├── 메트릭 게이지 초기화
    │   ├── badger_value_log_bytes_available
    │   ├── badger_key_log_bytes_available
    │   ├── badger_storage_maintenance_last_run
    │   └── badger_storage_valueloggc_last_run
    │
    ├── registerBadgerExpvarMetrics() → badger 내부 expvar 메트릭 등록
    │
    └── 백그라운드 고루틴 시작
        ├── maintenance()    ← 5분 주기 (기본값)
        └── metricsCopier() ← 10초 주기 (기본값)
```

### 3.4 Configuration

**소스 위치**: `internal/storage/v1/badger/config.go`

```go
type Config struct {
    TTL                   TTL           `mapstructure:"ttl"`
    Directories           Directories   `mapstructure:"directories"`
    Ephemeral             bool          `mapstructure:"ephemeral"`
    SyncWrites            bool          `mapstructure:"consistency"`
    MaintenanceInterval   time.Duration `mapstructure:"maintenance_interval"`
    MetricsUpdateInterval time.Duration `mapstructure:"metrics_update_interval"`
    ReadOnly              bool          `mapstructure:"read_only"`
}

type TTL struct {
    Spans time.Duration `mapstructure:"spans"`  // 기본값: 72시간
}

type Directories struct {
    Keys   string `mapstructure:"keys"`    // SSTable (인덱스) 디렉토리
    Values string `mapstructure:"values"`  // Value Log 디렉토리
}
```

| 설정 항목 | 플래그 | 기본값 | 설명 |
|-----------|--------|--------|------|
| `ephemeral` | `--badger.ephemeral` | `true` | tmpfs에 저장. 프로세스 종료 시 데이터 삭제 |
| `directories.keys` | `--badger.directory-key` | `{실행파일경로}/data/keys` | SSTable(인덱스) 저장 경로. SSD 권장 |
| `directories.values` | `--badger.directory-value` | `{실행파일경로}/data/values` | Value Log 저장 경로. HDD 가능 |
| `ttl.spans` | `--badger.span-store-ttl` | `72h` | 스팬 데이터 보존 기간 |
| `consistency` | `--badger.consistency` | `false` | true: 모든 쓰기를 즉시 디스크에 동기화 |
| `read_only` | `--badger.read-only` | `false` | 읽기 전용 모드. 여러 인스턴스가 동시 접근 가능 |
| `maintenance_interval` | `--badger.maintenance-interval` | `5m` | ValueLogGC 실행 주기 |
| `metrics_update_interval` | `--badger.metrics-update-interval` | `10s` | 메트릭 수집 주기 (테스트용) |

### 3.5 Key Schema (핵심 설계)

Badger는 정렬된 KV 스토어이므로, 키 설계가 쿼리 성능을 결정한다. Jaeger는 5가지 키 타입을 사용한다.

**소스 위치**: `internal/storage/v1/badger/spanstore/writer.go` (26~36행)

```go
const (
    spanKeyPrefix         byte = 0x80 // 모든 스팬 키의 첫 번째 비트가 1
    indexKeyRange         byte = 0x0F // 보조 인덱스는 하위 4비트 사용
    serviceNameIndexKey   byte = 0x81
    operationNameIndexKey byte = 0x82
    tagIndexKey           byte = 0x83
    durationIndexKey      byte = 0x84
    jsonEncoding          byte = 0x01
    protoEncoding         byte = 0x02
    defaultEncoding       byte = protoEncoding
)
```

#### 키 구조 상세

```
1) 스팬 키 (Primary Key) - Prefix: 0x80
┌──────┬──────────────────┬──────────────┬──────────────┐
│ 0x80 │   TraceID (16B)  │ StartTime(8B)│  SpanID (8B) │
│  1B  │  High(8) Low(8)  │  BigEndian   │  BigEndian   │
└──────┴──────────────────┴──────────────┴──────────────┘
 Value: Protobuf 직렬화된 Span 데이터
 UserMeta: 인코딩 타입 (0x02=proto, 0x01=json)

2) 서비스명 인덱스 - Prefix: 0x81
┌──────┬──────────────────┬──────────────┬──────────────────┐
│ 0x81 │  ServiceName     │ StartTime(8B)│   TraceID (16B)  │
│  1B  │  (가변 길이)       │  BigEndian   │  High(8) Low(8)  │
└──────┴──────────────────┴──────────────┴──────────────────┘
 Value: nil (키만 사용하는 인덱스)

3) 오퍼레이션명 인덱스 - Prefix: 0x82
┌──────┬──────────────────────────┬──────────────┬──────────────────┐
│ 0x82 │  ServiceName+OpName     │ StartTime(8B)│   TraceID (16B)  │
│  1B  │  (연결된 가변 길이)        │  BigEndian   │  High(8) Low(8)  │
└──────┴──────────────────────────┴──────────────┴──────────────────┘
 Value: nil

4) 태그 인덱스 - Prefix: 0x83
┌──────┬──────────────────────────────────┬──────────────┬──────────────────┐
│ 0x83 │  ServiceName+TagKey+TagValue    │ StartTime(8B)│   TraceID (16B)  │
│  1B  │  (연결된 가변 길이)                │  BigEndian   │  High(8) Low(8)  │
└──────┴──────────────────────────────────┴──────────────┴──────────────────┘
 Value: nil

5) 지속시간 인덱스 - Prefix: 0x84
┌──────┬──────────────────┬──────────────┬──────────────────┐
│ 0x84 │  Duration (8B)   │ StartTime(8B)│   TraceID (16B)  │
│  1B  │  마이크로초 단위    │  BigEndian   │  High(8) Low(8)  │
└──────┴──────────────────┴──────────────┴──────────────────┘
 Value: nil
```

#### 키 접두사 체계의 의미

```
비트 구조: [1][xxx][xxxx]
            │  │      │
            │  │      └── 하위 4비트: 인덱스 타입 구분
            │  └── 상위 3비트: 예약
            └── 최상위 비트: 항상 1 (스팬 키 영역)

0x80 = 1000_0000  →  스팬 데이터 (인덱스 비트 = 0000)
0x81 = 1000_0001  →  서비스명 인덱스
0x82 = 1000_0010  →  오퍼레이션명 인덱스
0x83 = 1000_0011  →  태그 인덱스
0x84 = 1000_0100  →  지속시간 인덱스
```

모든 키의 최상위 비트가 1이므로, Badger의 정렬 순서에서 스팬 관련 데이터가 키 공간의 상위 절반에 위치한다. 이는 샘플링 스토어(0x08, 0x09)와 자연스럽게 분리된다.

#### createIndexKey 함수

**소스 위치**: `internal/storage/v1/badger/spanstore/writer.go` (119~131행)

```go
func createIndexKey(indexPrefixKey byte, value []byte, startTime uint64, traceID model.TraceID) []byte {
    key := make([]byte, 1+len(value)+8+sizeOfTraceID)
    key[0] = (indexPrefixKey & indexKeyRange) | spanKeyPrefix
    pos := len(value) + 1
    copy(key[1:pos], value)
    binary.BigEndian.PutUint64(key[pos:], startTime)
    pos += 8
    binary.BigEndian.PutUint64(key[pos:], traceID.High)
    pos += 8
    binary.BigEndian.PutUint64(key[pos:], traceID.Low)
    return key
}
```

`key[0] = (indexPrefixKey & indexKeyRange) | spanKeyPrefix`에서:
- `indexKeyRange = 0x0F`: 하위 4비트만 추출
- `spanKeyPrefix = 0x80`: 최상위 비트 설정
- 결과적으로 `0x81 = (0x81 & 0x0F) | 0x80 = 0x01 | 0x80`

### 3.6 Writer: WriteSpan

**소스 위치**: `internal/storage/v1/badger/spanstore/writer.go` (57~117행)

```go
func (w *SpanWriter) WriteSpan(_ context.Context, span *model.Span) error {
    expireTime := uint64(time.Now().Add(w.ttl).Unix())
    startTime := model.TimeAsEpochMicroseconds(span.StartTime)

    // 트랜잭션 외부에서 엔트리 사전 구성 (트랜잭션 시간 최소화)
    entriesToStore := make([]*badger.Entry, 0, len(span.Tags)+4+len(span.Process.Tags)+len(span.Logs)*4)

    // 1) 스팬 데이터 (Primary Key)
    trace, err := w.createTraceEntry(span, startTime, expireTime)
    entriesToStore = append(entriesToStore, trace)

    // 2) 서비스명 인덱스
    entriesToStore = append(entriesToStore,
        w.createBadgerEntry(createIndexKey(serviceNameIndexKey,
            []byte(span.Process.ServiceName), startTime, span.TraceID), nil, expireTime))

    // 3) 오퍼레이션명 인덱스
    entriesToStore = append(entriesToStore,
        w.createBadgerEntry(createIndexKey(operationNameIndexKey,
            []byte(span.Process.ServiceName+span.OperationName), startTime, span.TraceID), nil, expireTime))

    // 4) 지속시간 인덱스
    durationValue := make([]byte, 8)
    binary.BigEndian.PutUint64(durationValue, uint64(model.DurationAsMicroseconds(span.Duration)))
    entriesToStore = append(entriesToStore,
        w.createBadgerEntry(createIndexKey(durationIndexKey, durationValue, startTime, span.TraceID), nil, expireTime))

    // 5) 태그 인덱스 (Span Tags + Process Tags + Log Fields)
    for _, kv := range span.Tags {
        entriesToStore = append(entriesToStore,
            w.createBadgerEntry(createIndexKey(tagIndexKey,
                []byte(span.Process.ServiceName+kv.Key+kv.AsString()), startTime, span.TraceID), nil, expireTime))
    }
    // Process.Tags, Logs 각각에 대해서도 동일한 패턴

    // 단일 트랜잭션으로 모두 기록
    err = w.store.Update(func(txn *badger.Txn) error {
        for i := range entriesToStore {
            err = txn.SetEntry(entriesToStore[i])
            if err != nil { return err }
        }
        return nil
    })

    // 트랜잭션 완료 후 캐시 업데이트
    w.cache.Update(span.Process.ServiceName, span.OperationName, expireTime)
    return err
}
```

쓰기 흐름도:

```
WriteSpan(span)
    │
    ├── 1. TTL 계산: now + ttl → expireTime
    │
    ├── 2. 엔트리 사전 구성 (트랜잭션 밖)
    │   ├── Primary: 0x80 + traceID + startTime + spanID → proto(span)
    │   ├── Service Index: 0x81 + serviceName + startTime + traceID → nil
    │   ├── Operation Index: 0x82 + service+op + startTime + traceID → nil
    │   ├── Duration Index: 0x84 + duration + startTime + traceID → nil
    │   └── Tag Indexes: 0x83 + service+key+value + startTime + traceID → nil
    │       (span.Tags + Process.Tags + Log.Fields 각각)
    │
    ├── 3. badger.Update() 트랜잭션
    │   └── 모든 엔트리를 txn.SetEntry()
    │
    └── 4. cache.Update(service, operation, expireTime)
```

**왜 인덱스 값(Value)이 nil인가?**

인덱스 엔트리는 키 자체가 검색 조건과 결과(TraceID)를 모두 포함한다. 값이 필요 없으므로 nil로 설정하여 Value Log 쓰기를 생략하고 디스크 사용량을 줄인다.

**왜 트랜잭션 외부에서 엔트리를 사전 구성하는가?**

Badger 트랜잭션은 MVCC 기반이므로, 트랜잭션 시간이 길어질수록 충돌 가능성이 높아진다. Protobuf 직렬화 같은 비용이 큰 연산을 트랜잭션 밖에서 수행하여 트랜잭션 범위를 최소화한다.

### 3.7 Reader: FindTraceIDs와 실행 계획

**소스 위치**: `internal/storage/v1/badger/spanstore/reader.go`

#### executionPlan 구조체

```go
type executionPlan struct {
    startTimeMin []byte   // 시작 시간 하한 (8바이트 BigEndian)
    startTimeMax []byte   // 시작 시간 상한 (8바이트 BigEndian)
    limit        int      // 최대 결과 수

    mergeOuter [][]byte                // merge-join 결과
    hashOuter  map[model.TraceID]struct{} // hash-join용 해시맵
}
```

#### FindTraceIDs 실행 흐름

```go
func (r *TraceReader) FindTraceIDs(_ context.Context, query *spanstore.TraceQueryParameters) ([]model.TraceID, error) {
    validateQuery(query)
    setQueryDefaults(query)

    // 1) 서비스 관련 인덱스 쿼리 준비
    indexSeeks := serviceQueries(query, indexSeeks)

    // 2) 시간 범위 설정
    plan := &executionPlan{
        startTimeMin: startStampBytes,
        startTimeMax: endStampBytes,
        limit:        query.NumTraces,
    }

    // 3) Duration 필터가 있으면 먼저 실행
    if query.DurationMax != 0 || query.DurationMin != 0 {
        plan.hashOuter = r.durationQueries(plan, query)
    }

    // 4) 인덱스 시크가 있으면 인덱스 기반 검색
    if len(indexSeeks) > 0 {
        return r.indexSeeksToTraceIDs(plan, indexSeeks)
    }

    // 5) 인덱스 없으면 전체 스캔
    return r.scanTimeRange(plan)
}
```

전체 실행 계획 흐름도:

```
                    FindTraceIDs(query)
                         │
                         ▼
              ┌─────────────────────┐
              │ query에 Duration    │
              │ 필터가 있는가?        │
              └──────┬──────────────┘
                     │
          ┌─Yes──────┤──────No──┐
          ▼                     │
  durationQueries()             │
  → plan.hashOuter              │
  (Duration 범위에               │
   해당하는 TraceID 집합)          │
                                │
          ├─────────────────────┘
          ▼
  ┌────────────────────┐
  │ indexSeeks가        │
  │ 있는가?              │
  └──────┬─────────────┘
         │
  ┌─Yes──┤──────No──┐
  ▼                  ▼
indexSeeksToTraceIDs()  scanTimeRange()
  │                    (전체 테이블 스캔)
  │
  ├── 각 인덱스에 대해 scanIndexKeys()
  │   └── 역순(최신→과거) 시크
  │       └── 타임스탬프 범위 내의 TraceID 수집
  │
  ├── 중간 결과끼리 merge-join
  │   mergeJoinIds(left, right)
  │   (정렬된 두 리스트의 교집합)
  │
  ├── 마지막 인덱스 결과를 hash-join
  │   buildHash(plan, outerIDs)
  │
  └── filterIDs(plan, innerIDs)
      → 최종 TraceID 목록 (limit 적용)
```

#### serviceQueries: 인덱스 선택 전략

**소스 위치**: `internal/storage/v1/badger/spanstore/reader.go` (261~287행)

```go
func serviceQueries(query *spanstore.TraceQueryParameters, indexSeeks [][]byte) [][]byte {
    if query.ServiceName != "" {
        tagQueryUsed := false
        for k, v := range query.Tags {
            // 태그 인덱스: 0x83 + serviceName+key+value
            tagSearchKey = append(tagSearchKey, tagIndexKey)
            tagSearchKey = append(tagSearchKey, []byte(query.ServiceName + k + v)...)
            indexSeeks = append(indexSeeks, tagSearchKey)
            tagQueryUsed = true
        }

        if query.OperationName != "" {
            // 오퍼레이션 인덱스: 0x82 + serviceName+operationName
            indexSearchKey = append(indexSearchKey, operationNameIndexKey)
            indexSearchKey = append(indexSearchKey, []byte(query.ServiceName+query.OperationName)...)
        } else if !tagQueryUsed {
            // 서비스명 인덱스: 0x81 + serviceName
            indexSearchKey = append(indexSearchKey, serviceNameIndexKey)
            indexSearchKey = append(indexSearchKey, []byte(query.ServiceName)...)
        }
    }
    return indexSeeks
}
```

인덱스 선택 우선순위:

| 쿼리 조건 | 사용 인덱스 | 이유 |
|-----------|-----------|------|
| service + tags | tagIndex (0x83) | 태그 자체가 서비스명을 포함하므로 가장 선택적 |
| service + operation | operationNameIndex (0x82) | service+operation 조합이 서비스명만보다 선택적 |
| service만 | serviceNameIndex (0x81) | 최소한의 필터링 |
| 아무것도 없음 | 전체 테이블 스캔 | 인덱스 사용 불가 |

#### merge-join과 hash-join

**소스 위치**: `internal/storage/v1/badger/spanstore/reader.go` (290~337행, 423~449행)

여러 인덱스에서 얻은 TraceID 집합의 교집합을 구하는 과정:

```
예시: ServiceName="frontend" + Tag="http.status=200" 쿼리

indexSeeks[0]: tagIndex에서 "frontend"+"http.status"+"200" 검색
indexSeeks[1]: serviceNameIndex에서 "frontend" 검색 (불필요하지만 존재할 수 있음)

실행 순서 (뒤에서부터):
1. indexSeeks[1] → scanIndexKeys() → innerIDs (서비스 인덱스 결과)
   정렬 후 중복 제거
   plan.mergeOuter = innerIDs

2. indexSeeks[0] → scanIndexKeys() → ids (태그 인덱스 결과)
   plan.hashOuter = buildHash(plan, mergeOuter)
   → mergeOuter의 TraceID를 해시맵에 등록

3. filterIDs(plan, ids)
   → ids에서 hashOuter에 존재하는 것만 선택
   → limit까지만 반환
```

merge-join 알고리즘:

```go
func mergeJoinIds(left, right [][]byte) [][]byte {
    // 정렬된 두 바이트 슬라이스의 교집합 (O(n+m))
    for r, l := 0, 0; r <= rMax && l <= lMax; {
        switch bytes.Compare(left[l], right[r]) {
        case 1:  r++        // left > right → right 전진
        case -1: l++        // left < right → left 전진
        default:            // 일치 → 결과에 추가, 양쪽 전진
            merged = append(merged, left[l])
            l++; r++
        }
    }
    return merged
}
```

### 3.8 GetTraces: TraceID로 스팬 조회

**소스 위치**: `internal/storage/v1/badger/spanstore/reader.go` (110~154행)

```go
func (r *TraceReader) getTraces(traceIDs []model.TraceID) ([]*model.Trace, error) {
    prefixes := make([][]byte, 0, len(traceIDs))
    for _, traceID := range traceIDs {
        prefixes = append(prefixes, createPrimaryKeySeekPrefix(traceID))
    }

    err := r.store.View(func(txn *badger.Txn) error {
        it := txn.NewIterator(opts)
        defer it.Close()

        for _, prefix := range prefixes {
            spans := make([]*model.Span, 0, 32)
            // prefix = 0x80 + traceID (17바이트)
            for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
                item := it.Item()
                val, _ := item.ValueCopy(val)
                sp, _ := decodeValue(val, item.UserMeta()&encodingTypeBits)
                spans = append(spans, sp)
            }
            if len(spans) > 0 {
                traces = append(traces, &model.Trace{Spans: spans})
            }
        }
        return nil
    })
    return traces, err
}
```

이 함수가 효율적인 이유:
- Primary Key가 `0x80 + traceID + startTime + spanID`로 구성되어 있으므로, `0x80 + traceID`를 접두사로 Seek하면 해당 트레이스의 모든 스팬이 **시작 시간 순서대로** 자동 정렬되어 반환된다
- Badger의 LSM-tree 특성상 접두사 검색이 O(log n)으로 빠르다

### 3.9 CacheStore: 메타데이터 캐싱

**소스 위치**: `internal/storage/v1/badger/spanstore/cache.go`

```go
type CacheStore struct {
    cacheLock  sync.Mutex
    services   map[string]uint64             // 서비스명 → 만료시간(Unix초)
    operations map[string]map[string]uint64   // 서비스명 → (오퍼레이션명 → 만료시간)
    store      *badger.DB
    ttl        time.Duration
}
```

**왜 캐시가 필요한가?**

`GetServices()`와 `GetOperations()` 쿼리는 인덱스 전체를 스캔해야 하는 비용이 큰 연산이다. CacheStore는 이 메타데이터를 메모리에 유지하여 반복 쿼리를 빠르게 처리한다.

```
캐시 갱신 흐름:

WriteSpan(span)                GetServices()
    │                               │
    ├── txn.SetEntry(...)           │
    │                               │
    └── cache.Update(               ▼
        service,               cache.GetServices()
        operation,               │
        expireTime)              ├── cacheLock.Lock()
         │                       ├── 만료된 서비스 제거
         ▼                       ├── 유효한 서비스 반환
    cacheLock.Lock()             └── cacheLock.Unlock()
    services[svc] = expireTime
    operations[svc][op] = expireTime
    cacheLock.Unlock()
```

캐시 초기화 (첫 Reader 생성 시):

```go
func NewTraceReader(db *badger.DB, c *CacheStore, prefillCache bool) *TraceReader {
    if prefillCache {
        services := reader.preloadServices()   // serviceNameIndex 스캔
        for _, service := range services {
            reader.preloadOperations(service)   // operationNameIndex 스캔
        }
    }
    return reader
}
```

`preloadServices`는 `0x81` 접두사를 가진 모든 키를 스캔하여 서비스명을 추출하고, 각 서비스에 대해 `preloadOperations`가 `0x82` 접두사 키에서 오퍼레이션명을 추출한다.

### 3.10 TTL (Time-To-Live)

Badger의 TTL은 **엔트리 단위**로 적용된다:

```go
func (*SpanWriter) createBadgerEntry(key []byte, value []byte, expireTime uint64) *badger.Entry {
    return &badger.Entry{
        Key:       key,
        Value:     value,
        ExpiresAt: expireTime,  // Unix 타임스탬프 (초)
    }
}
```

- `expireTime = time.Now().Add(w.ttl).Unix()`: 현재 시간 + TTL
- 기본 TTL: 72시간 (3일)
- 스팬 데이터와 모든 인덱스 엔트리에 **동일한 TTL**이 적용된다
- Badger는 내부적으로 compaction 시 만료된 키를 제거한다

### 3.11 Maintenance: ValueLogGC

**소스 위치**: `internal/storage/v1/badger/factory.go` (195~219행)

```go
func (f *Factory) maintenance() {
    maintenanceTicker := time.NewTicker(f.Config.MaintenanceInterval)
    defer maintenanceTicker.Stop()
    for {
        select {
        case <-f.maintenanceDone:
            return
        case t := <-maintenanceTicker.C:
            var err error
            // 더 이상 정리할 것이 없을 때까지 반복
            for err == nil {
                err = f.store.RunValueLogGC(0.5)
            }
            if errors.Is(err, badger.ErrNoRewrite) {
                f.metrics.LastValueLogCleaned.Update(t.UnixNano())
            }
            f.metrics.LastMaintenanceRun.Update(t.UnixNano())
            _ = f.diskStatisticsUpdate()
        }
    }
}
```

**ValueLogGC의 동작 원리**:

```
Value Log 파일:
┌─────────────────────────────────────────────┐
│  [유효]  [만료]  [유효]  [만료]  [만료]  [유효]  │
│   10%    15%    10%    20%    25%    20%   │
└─────────────────────────────────────────────┘
    │                                  │
    └──────── 유효 비율 = 40% ──────────┘
              만료 비율 = 60% > 50% (임계값)
              → 이 파일을 다시 작성(rewrite)

다시 작성 후:
┌──────────────────┐
│  [유효]  [유효]  [유효] │  ← 유효 데이터만 포함하는 새 파일
│   25%    25%    50%  │
└──────────────────────┘
```

- `0.5` 임계값: 파일 내 50% 이상이 무효 데이터이면 다시 작성
- `for err == nil`: 정리할 파일이 없을 때까지 반복 실행
- `badger.ErrNoRewrite`: 더 이상 정리할 파일이 없음을 의미 (정상 종료)

### 3.12 메트릭 수집

**소스 위치**: `internal/storage/v1/badger/factory.go` (221~253행)

```go
func (f *Factory) metricsCopier() {
    metricsTicker := time.NewTicker(f.Config.MetricsUpdateInterval)
    defer metricsTicker.Stop()
    for {
        select {
        case <-f.maintenanceDone:
            return
        case <-metricsTicker.C:
            expvar.Do(func(kv expvar.KeyValue) {
                if strings.HasPrefix(kv.Key, "badger") {
                    // expvar 메트릭을 Jaeger 메트릭 시스템으로 복사
                }
            })
        }
    }
}
```

Badger는 Go 표준 라이브러리의 `expvar` 패키지로 내부 메트릭을 노출한다. `metricsCopier`는 이를 주기적으로 Jaeger의 메트릭 시스템으로 복사한다.

주요 메트릭:

| 메트릭 이름 | 타입 | 설명 |
|-----------|------|------|
| `badger_value_log_bytes_available` | Gauge | Value Log 마운트 포인트 여유 공간 (바이트) |
| `badger_key_log_bytes_available` | Gauge | Key Log 마운트 포인트 여유 공간 (바이트) |
| `badger_storage_maintenance_last_run` | Gauge | 마지막 유지보수 실행 시간 (UnixNano) |
| `badger_storage_valueloggc_last_run` | Gauge | 마지막 ValueLogGC 실행 시간 (UnixNano) |
| `badger_*` (내부) | Gauge | Badger 자체 expvar 메트릭 (LSM 크기, vlog 크기 등) |

Linux에서는 `diskStatisticsUpdate`가 `unix.Statfs`를 사용하여 실제 디스크 여유 공간을 측정한다:

```go
// stats_linux.go
func (f *Factory) diskStatisticsUpdate() error {
    var keyDirStatfs unix.Statfs_t
    _ = unix.Statfs(f.Config.Directories.Keys, &keyDirStatfs)
    var valDirStatfs unix.Statfs_t
    _ = unix.Statfs(f.Config.Directories.Values, &valDirStatfs)
    f.metrics.ValueLogSpaceAvailable.Update(int64(valDirStatfs.Bavail) * int64(valDirStatfs.Bsize))
    f.metrics.KeyLogSpaceAvailable.Update(int64(keyDirStatfs.Bavail) * int64(keyDirStatfs.Bsize))
    return nil
}
```

### 3.13 종료 처리

**소스 위치**: `internal/storage/v1/badger/factory.go` (175~192행)

```go
func (f *Factory) Close() error {
    close(f.maintenanceDone)  // 백그라운드 고루틴에 종료 신호
    f.bgWg.Wait()             // 모든 백그라운드 고루틴 완료 대기

    if f.store == nil {
        return nil
    }
    err := f.store.Close()    // Badger DB 닫기

    if f.Config.Ephemeral {
        errSecondary := os.RemoveAll(f.tmpDir)  // 임시 디렉토리 정리
        if err == nil {
            err = errSecondary
        }
    }
    return err
}
```

종료 순서가 중요한 이유:
1. `maintenanceDone` 채널을 닫아 `maintenance()`와 `metricsCopier()` 고루틴이 종료되도록 한다
2. `bgWg.Wait()`로 모든 백그라운드 작업이 완료될 때까지 대기
3. 이후에 `store.Close()`를 호출해야 진행 중인 ValueLogGC와 충돌하지 않는다
4. ephemeral 모드인 경우 임시 디렉토리까지 삭제

### 3.14 v1adapter를 통한 OTLP 변환

v2 Factory는 v1의 Span Reader/Writer를 v1adapter로 래핑하여 OTLP(ptrace.Traces) 인터페이스를 제공한다.

**TraceWriter 래핑** (`internal/storage/v2/v1adapter/tracewriter.go`):

```go
func (t *TraceWriter) WriteTraces(ctx context.Context, td ptrace.Traces) error {
    batches := V1BatchesFromTraces(td)  // ptrace.Traces → []model.Batch
    var errs []error
    for _, batch := range batches {
        for _, span := range batch.Spans {
            if span.Process == nil {
                span.Process = batch.Process
            }
            err := t.spanWriter.WriteSpan(ctx, span)
            if err != nil {
                errs = append(errs, err)
            }
        }
    }
    return errors.Join(errs...)
}
```

**TraceReader 래핑** (`internal/storage/v2/v1adapter/tracereader.go`):

```go
func (tr *TraceReader) GetTraces(ctx context.Context, traceIDs ...tracestore.GetTraceParams) iter.Seq2[[]ptrace.Traces, error] {
    return func(yield func([]ptrace.Traces, error) bool) {
        for _, idParams := range traceIDs {
            query := spanstore.GetTraceParameters{
                TraceID: ToV1TraceID(idParams.TraceID),
                // ...
            }
            t, err := tr.spanReader.GetTrace(ctx, query)
            // model.Trace → ptrace.Traces 변환 후 yield
        }
    }
}
```

변환 경로:

```
OTLP 수신 경로 (쓰기):
  ptrace.Traces
    → V1BatchesFromTraces()    ← v1adapter/translator.go
    → []model.Batch
    → model.Span (각각)
    → WriteSpan() → Badger DB

OTLP 반환 경로 (읽기):
  Badger DB
    → getTraces() → []*model.Trace
    → V1BatchesToTraces()      ← v1adapter/translator.go
    → ptrace.Traces
```

### 3.15 Dependency Store

**소스 위치**: `internal/storage/v1/badger/dependencystore/storage.go`

Badger의 DependencyStore는 **별도 인덱스를 사용하지 않는다**. 의존성 조회 시 전체 트레이스를 스캔한다:

```go
func (s *DependencyStore) GetDependencies(ctx context.Context, endTs time.Time, lookback time.Duration) ([]model.DependencyLink, error) {
    params := &spanstore.TraceQueryParameters{
        StartTimeMin: endTs.Add(-1 * lookback),
        StartTimeMax: endTs,
    }
    // 전체 테이블 스캔 - 병목이 될 수 있음
    traces, err := s.reader.FindTraces(ctx, params)
    for _, tr := range traces {
        processTrace(deps, tr)
    }
    return depMapToSlice(deps), err
}
```

주석에도 명시되어 있듯이, 이것이 병목이 되면 별도 의존성 인덱스를 추가할 수 있지만 현재는 구현되어 있지 않다.

### 3.16 Sampling Store

**소스 위치**: `internal/storage/v1/badger/samplingstore/storage.go`

적응형 샘플링 데이터도 Badger에 저장할 수 있다. 별도의 키 접두사를 사용한다:

```go
const (
    throughputKeyPrefix    byte = 0x08  // 처리량 데이터
    probabilitiesKeyPrefix byte = 0x09  // 확률/QPS 데이터
)
```

이 접두사들(0x08, 0x09)은 스팬 키 접두사(0x80~0x84)와 완전히 분리된 키 공간에 위치한다.

---

## 4. 비교 분석

### 4.1 아키텍처 비교

```
┌──────────────────────────────────────────────────────────────┐
│                    Memory 스토리지                             │
│                                                              │
│  ptrace.Traces ──→ reshuffleResourceSpans() ──→ 링 버퍼       │
│                    (TraceID별 그룹핑)          ([]traceAndId)  │
│                                                              │
│  FindTraces() ──→ 링 버퍼 역순 스캔 ──→ validSpan() 필터      │
│                                                              │
│  특징: OTLP 네이티브, 변환 없음, O(n) 스캔                     │
└──────────────────────────────────────────────────────────────┘

┌──────────────────────────────────────────────────────────────┐
│                    Badger 스토리지                              │
│                                                              │
│  ptrace.Traces ──→ v1adapter ──→ WriteSpan() ──→ Badger DB    │
│                 (OTLP→Jaeger)   + 4개 인덱스     (LSM-tree)   │
│                                                              │
│  FindTraces() ──→ 인덱스 Seek ──→ merge/hash-join ──→ 스팬   │
│                  (서비스/태그/      (교집합 연산)       데이터  │
│                   지속시간)                           조회     │
│                                                              │
│  특징: v1 API, 모델 변환 필요, 인덱스 기반 O(log n) 조회       │
└──────────────────────────────────────────────────────────────┘
```

### 4.2 기능 비교표

| 항목 | Memory | Badger |
|------|--------|--------|
| **데이터 영속성** | 없음 (프로세스 종료 시 소실) | 있음 (ephemeral=false) |
| **멀티테넌시** | 지원 (perTenant 맵) | 미지원 |
| **최대 저장량** | MaxTraces 설정 (링 버퍼) | 디스크 용량 한도 + TTL |
| **TTL** | 자동 퇴출 (링 버퍼) | 엔트리별 TTL + GC |
| **쿼리 방식** | 선형 스캔 + 인메모리 필터 | 인덱스 Seek + merge/hash-join |
| **인덱스** | services/operations 맵 | 4개 보조 인덱스 (서비스, 오퍼레이션, 태그, 지속시간) |
| **인코딩** | OTLP 네이티브 (ptrace) | Protobuf/JSON (Jaeger model v1) |
| **의존성 계산** | 실시간 트레이스 분석 | FindTraces() 후 스팬 분석 |
| **읽기 전용 모드** | 없음 | 지원 (read_only) |
| **동시 접근** | 단일 프로세스 (RWMutex) | 여러 인스턴스 가능 (read_only 모드) |
| **백그라운드 유지보수** | 없음 | ValueLogGC, 디스크 통계, 메트릭 수집 |

### 4.3 성능 특성

| 연산 | Memory | Badger |
|------|--------|--------|
| **쓰기** | O(1) - 링 버퍼에 추가 | O(log n) - LSM-tree 삽입 + 인덱스 4~N개 |
| **ID로 조회** | O(1) - ids 맵 조회 | O(log n) - 접두사 Seek |
| **서비스/오퍼레이션 목록** | O(s) - 맵 순회 | O(1) - CacheStore에서 반환 |
| **FindTraces** | O(SearchDepth) - 링 버퍼 역순 스캔 | O(log n + k) - 인덱스 Seek + 결과 조합 |
| **메모리 사용량** | MaxTraces * 평균 트레이스 크기 | WAL + MemTable + 캐시 |
| **디스크 I/O** | 없음 | 읽기: SSD 권장, 쓰기: 배치 + WAL |

### 4.4 사용 시나리오 가이드

| 시나리오 | 권장 스토리지 | 이유 |
|---------|-------------|------|
| 로컬 개발 | Memory | 설정 불필요, 즉시 시작 |
| CI/CD 테스트 | Memory | 테스트 격리, 빠른 초기화 |
| 데모/PoC | Memory | 최소 리소스 |
| 소규모 프로덕션 (단일 노드) | Badger | 재시작 시 데이터 보존 |
| 대규모 프로덕션 | ES/Cassandra | 수평 확장, 고가용성 |
| 멀티테넌트 환경 | Memory (또는 ES) | Badger는 멀티테넌시 미지원 |

---

## 5. 운영 가이드

### 5.1 Memory 스토리지 튜닝

```yaml
# all-in-one 설정 예시
storage:
  memory:
    max_traces: 100000  # 프로덕션: 트레이스 유입량에 따라 조정
```

주의사항:
- `MaxTraces`가 클수록 메모리 사용량이 증가한다
- 평균 트레이스 크기가 10KB라면, MaxTraces=100000은 약 1GB 메모리를 사용한다
- 멀티테넌시 환경에서는 테넌트당 MaxTraces가 적용되므로 총 메모리 = MaxTraces * 테넌트 수 * 평균 크기

### 5.2 Badger 스토리지 튜닝

```yaml
# 영구 저장소 설정 예시
storage:
  badger:
    ephemeral: false
    directories:
      keys: /ssd/jaeger/badger/keys      # SSD에 배치 (인덱스)
      values: /hdd/jaeger/badger/values   # HDD 가능 (스팬 데이터)
    ttl:
      spans: 168h    # 7일 보존
    consistency: false  # 성능 우선 (true: 안정성 우선)
    maintenance_interval: 5m
    read_only: false
```

디렉토리 분리 전략:

```
┌──────────────────────────────────────────┐
│  SSD (/ssd/jaeger/badger/keys)           │
│                                          │
│  ├── SSTable Level 0                     │
│  ├── SSTable Level 1                     │
│  ├── SSTable Level 2                     │
│  └── ...                                 │
│                                          │
│  특징: 랜덤 읽기가 빈번 (인덱스 Seek)      │
│  SSD의 빠른 IOPS가 쿼리 성능에 직결       │
└──────────────────────────────────────────┘

┌──────────────────────────────────────────┐
│  HDD (/hdd/jaeger/badger/values)         │
│                                          │
│  ├── value.log.0001                      │
│  ├── value.log.0002                      │
│  └── ...                                 │
│                                          │
│  특징: 순차 쓰기 위주 (Value Log append)    │
│  HDD의 순차 I/O로도 충분한 성능            │
└──────────────────────────────────────────┘
```

### 5.3 모니터링 체크리스트

| 메트릭 | 경고 임계값 | 대응 |
|--------|-----------|------|
| `badger_value_log_bytes_available` | < 10GB | 디스크 증설 또는 TTL 단축 |
| `badger_key_log_bytes_available` | < 5GB | SSD 증설 |
| `badger_storage_maintenance_last_run` | > 15분 전 | maintenance 고루틴 상태 확인 |
| `badger_storage_valueloggc_last_run` | > 30분 전 | GC가 작동하는지 확인 |

### 5.4 트러블슈팅

| 증상 | 원인 | 해결 |
|------|------|------|
| 디스크 사용량 지속 증가 | ValueLogGC가 작동하지 않음 | maintenance_interval 확인, 로그에서 GC 에러 확인 |
| 쿼리 느림 | 태그 없이 전체 스캔 | 서비스명 + 태그로 쿼리 좁히기 |
| 프로세스 재시작 시 데이터 소실 | ephemeral=true (기본값) | ephemeral=false로 변경 |
| 메모리 부족 (Memory 스토리지) | MaxTraces 너무 큼 | MaxTraces 줄이거나 Badger로 전환 |
| "read-only" 에러 | ReadOnly=true 상태에서 쓰기 시도 | ReadOnly 설정 확인 |
| Badger 시작 실패 | WAL 재생 실패 또는 권한 문제 | 디렉토리 권한 확인, 필요 시 데이터 삭제 후 재시작 |

---

## 요약

Jaeger의 임베디드 스토리지는 외부 의존성 없이 트레이싱을 시작할 수 있게 해주는 핵심 컴포넌트다.

- **Memory 스토리지**는 OTLP 네이티브 v2 구현으로, 링 버퍼 기반의 단순하면서도 효율적인 인메모리 저장소다. 멀티테넌시를 지원하며, all-in-one 모드의 기본 스토리지로 사용된다. WriteTraces 시 ResourceSpans를 TraceID 기준으로 재구성(reshuffle)하는 과정이 핵심이다.

- **Badger 스토리지**는 v1 API 기반으로 구현되어 v1adapter를 통해 v2 인터페이스를 제공한다. LSM-tree 기반의 정렬된 키-값 저장소 위에 5가지 키 스키마(스팬 + 4개 보조 인덱스)를 설계하여, 서비스명/오퍼레이션/태그/지속시간 기반의 효율적인 쿼리를 지원한다. merge-join과 hash-join을 활용한 복합 쿼리 실행 계획, CacheStore를 통한 메타데이터 캐싱, ValueLogGC를 통한 자동 디스크 관리가 주요 설계 포인트다.

두 스토리지 모두 프로덕션 대규모 환경에는 적합하지 않지만, 개발/테스트 환경이나 소규모 배포에서는 외부 인프라 없이 Jaeger의 모든 기능을 활용할 수 있게 해준다.
