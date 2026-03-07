# 12. Compactor 인덱스 압축기 Deep-Dive

## 목차

1. [개요](#1-개요)
2. [Compactor 구조체](#2-compactor-구조체)
3. [Config 설정 체계](#3-config-설정-체계)
4. [리더 선출: Ring 기반 단일 실행 보장](#4-리더-선출-ring-기반-단일-실행-보장)
5. [초기화 과정 상세](#5-초기화-과정-상세)
6. [인덱스 압축 프로세스](#6-인덱스-압축-프로세스)
7. [보존 정책 (Retention)](#7-보존-정책-retention)
8. [삭제 처리 시스템](#8-삭제-처리-시스템)
9. [Lazy Deletion 패턴: Mark-Sweep 2단계](#9-lazy-deletion-패턴-mark-sweep-2단계)
10. [수평 스케일링 모드](#10-수평-스케일링-모드)
11. [tablesManager: 테이블 관리](#11-tablesmanager-테이블-관리)
12. [메트릭 및 운영 가이드](#12-메트릭-및-운영-가이드)
13. [설계 결정 분석](#13-설계-결정-분석)

---

## 1. 개요

Loki의 Compactor는 인덱스 파일을 압축(compaction)하고, 데이터 보존 정책(retention)을 적용하며, 삭제 요청을 처리하는 백그라운드 서비스이다. 기본적으로 Ring 기반 리더 선출을 통해 클러스터 내 단 하나의 인스턴스만 Compactor 작업을 수행하도록 보장한다.

```
소스 위치:
- pkg/compactor/compactor.go         -- Compactor 구조체, 라이프사이클
- pkg/compactor/config.go            -- Config 설정
- pkg/compactor/tables_manager.go    -- tablesManager, 테이블 순회/압축
- pkg/compactor/retention/           -- 보존 정책, TableMarker, Sweeper
- pkg/compactor/retention/expiration.go -- ExpirationChecker
- pkg/compactor/retention/retention.go  -- Marker, Sweeper, Series/Chunk 인터페이스
- pkg/compactor/deletion/            -- 삭제 요청 처리
- pkg/compactor/jobqueue/            -- 수평 스케일링 작업 큐
```

### Compactor의 7단계 작업 흐름

소스 코드의 주석(`pkg/compactor/compactor.go:37-43`)에 명시된 압축 과정:

```
1단계: 테이블 이름으로 인덱스 타입 식별 (schemaPeriodForTable)
2단계: 해당 인덱스 타입에 등록된 IndexCompactor 찾기
3단계: IndexCompactor.NewIndexCompactor로 TableCompactor 인스턴스 빌드
4단계: TableCompactor.Compact 실행 → CompactedIndex 생성
5단계: 보존 활성화 시 CompactedIndex에 retention 적용
6단계: IndexCompactor.ToIndexFile로 업로드용 파일 변환
7단계: 업로드 성공 시 이전 인덱스 파일 삭제
```

### 아키텍처 개요

```
┌─────────────────────────────────────────────────────────┐
│                      Compactor                          │
│                                                         │
│  ┌──────────┐  ┌───────────────┐  ┌───────────────────┐│
│  │ Ring     │  │ Tables        │  │ Deletion          ││
│  │ Leader   │  │ Manager       │  │ Manager           ││
│  │ Election │  │               │  │                   ││
│  └────┬─────┘  └───────┬───────┘  └────────┬──────────┘│
│       │                │                   │            │
│       ▼                ▼                   ▼            │
│  ┌──────────────────────────────────────────────────┐  │
│  │              Store Containers                     │  │
│  │  (Period별 IndexStorageClient + TableMarker +     │  │
│  │   Sweeper)                                        │  │
│  └──────────────────────────────────────────────────┘  │
│                        │                                │
│                        ▼                                │
│  ┌──────────────────────────────────────────────────┐  │
│  │            Object Store                           │  │
│  │  (S3, GCS, Azure Blob 등)                        │  │
│  └──────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────┘
```

---

## 2. Compactor 구조체

### 핵심 구조체 정의

```go
// 소스: pkg/compactor/compactor.go:83-115
type Compactor struct {
    services.Service

    cfg                       Config
    indexStorageClient        storage.Client
    tableMarker               retention.TableMarker
    sweeper                   *retention.Sweeper
    deleteRequestsStore       deletion.DeleteRequestsStore
    DeleteRequestsHandler     *deletion.DeleteRequestHandler
    DeleteRequestsGRPCHandler *deletion.GRPCRequestHandler
    deleteRequestsManager     *deletion.DeleteRequestsManager
    expirationChecker         retention.ExpirationChecker
    metrics                   *metrics
    running                   bool
    wg                        sync.WaitGroup
    indexCompactors           map[string]IndexCompactor
    schemaConfig              config.SchemaConfig
    limits                    Limits
    JobQueue                  *jobqueue.Queue

    tablesManager *tablesManager

    // Ring 관련
    ringLifecycler *ring.BasicLifecycler
    ring           *ring.Ring
    ringPollPeriod time.Duration

    // 서브서비스 관리
    subservices        *services.Manager
    subservicesWatcher *services.FailureWatcher

    // 스키마 기간별 스토어 컨테이너
    storeContainers map[config.DayTime]storeContainer
}
```

### storeContainer: 기간별 스토어

```go
// 소스: pkg/compactor/compactor.go:117-121
type storeContainer struct {
    tableMarker        retention.TableMarker
    sweeper            *retention.Sweeper
    indexStorageClient storage.Client
}
```

각 스키마 기간(Period)마다 별도의 `storeContainer`를 유지한다. 이는 Loki가 스키마 버전을 업그레이드할 때 이전 기간의 인덱스와 새 기간의 인덱스를 동시에 관리할 수 있게 한다.

### Limits 인터페이스

```go
// 소스: pkg/compactor/compactor.go:123-127
type Limits interface {
    deletion.Limits
    retention.Limits
    DefaultLimits() *validation.Limits
}
```

이 인터페이스는 삭제 정책과 보존 정책의 한도를 통합하며, 테넌트별로 다른 정책을 적용할 수 있게 한다.

---

## 3. Config 설정 체계

### 주요 설정 필드

```go
// 소스: pkg/compactor/config.go:20-46
type Config struct {
    WorkingDirectory                string                `yaml:"working_directory"`
    CompactionInterval              time.Duration         `yaml:"compaction_interval"`
    ApplyRetentionInterval          time.Duration         `yaml:"apply_retention_interval"`
    RetentionEnabled                bool                  `yaml:"retention_enabled"`
    RetentionDeleteDelay            time.Duration         `yaml:"retention_delete_delay"`
    RetentionDeleteWorkCount        int                   `yaml:"retention_delete_worker_count"`
    RetentionTableTimeout           time.Duration         `yaml:"retention_table_timeout"`
    RetentionBackoffConfig          backoff.Config        `yaml:"retention_backoff_config"`
    DeleteRequestStore              string                `yaml:"delete_request_store"`
    DeleteRequestStoreKeyPrefix     string                `yaml:"delete_request_store_key_prefix"`
    DeleteRequestStoreDBType        string                `yaml:"delete_request_store_db_type"`
    DeleteBatchSize                 int                   `yaml:"delete_batch_size"`
    DeleteRequestCancelPeriod       time.Duration         `yaml:"delete_request_cancel_period"`
    DeleteMaxInterval               time.Duration         `yaml:"delete_max_interval"`
    MaxCompactionParallelism        int                   `yaml:"max_compaction_parallelism"`
    UploadParallelism               int                   `yaml:"upload_parallelism"`
    CompactorRing                   lokiring.RingConfig   `yaml:"compactor_ring,omitempty"`
    RunOnce                         bool                  `yaml:"_"`
    TablesToCompact                 int                   `yaml:"tables_to_compact"`
    SkipLatestNTables               int                   `yaml:"skip_latest_n_tables"`
    HorizontalScalingMode           string                `yaml:"horizontal_scaling_mode"`
    DeletionMarkerObjectStorePrefix string                `yaml:"deletion_marker_object_store_prefix"`
}
```

### 설정 상세 표

| 설정 | 기본값 | 설명 |
|------|--------|------|
| `WorkingDirectory` | `/var/loki/compactor` | 압축 작업용 임시 디렉토리 |
| `CompactionInterval` | 10m | 압축 실행 간격 |
| `ApplyRetentionInterval` | 0 (=CompactionInterval) | 보존 정책 적용 간격 |
| `RetentionEnabled` | false | 보존 정책 활성화 |
| `RetentionDeleteDelay` | 2h | 청크 삭제까지 대기 시간 |
| `RetentionDeleteWorkCount` | 150 | 삭제 워커 수 |
| `RetentionTableTimeout` | 0 (무제한) | 테이블당 보존 적용 타임아웃 |
| `DeleteBatchSize` | 70 | 압축 주기당 최대 삭제 요청 수 |
| `DeleteRequestCancelPeriod` | 24h | 삭제 요청 취소 가능 기간 |
| `DeleteMaxInterval` | 24h | 라인 필터 포함 삭제 요청 최대 간격 |
| `MaxCompactionParallelism` | 1 | 동시 압축 테이블 수 |
| `UploadParallelism` | 10 | 압축 완료 후 업로드 병렬도 |
| `RunOnce` | false | 1회 실행 후 종료 (클린업용) |
| `TablesToCompact` | 0 (전체) | 압축 대상 테이블 수 제한 |
| `SkipLatestNTables` | 0 | 최신 N개 테이블 건너뛰기 |

### Ring 상수 정의

```go
// 소스: pkg/compactor/compactor.go:45-67
const (
    ringAutoForgetUnhealthyPeriods = 10     // 비정상 인스턴스 자동 제거 기간
    ringKey                        = "compactor"
    ringNameForServer              = "compactor"
    ringKeyOfLeader                = 0      // 리더 선출용 키
    ringReplicationFactor          = 1      // 복제 팩터 (항상 1)
    ringNumTokens                  = 1      // 토큰 수 (항상 1)
)
```

---

## 4. 리더 선출: Ring 기반 단일 실행 보장

### 왜 단일 인스턴스 실행인가?

인덱스 압축은 동시에 여러 인스턴스가 같은 테이블을 수정하면 데이터 손상이 발생할 수 있다. 따라서 Ring을 사용하여 클러스터 내 정확히 하나의 인스턴스만 Compactor 작업을 수행하도록 보장한다.

### Ring 기반 리더 선출 흐름

```
┌─────────────────────────────────────────────────┐
│           Compactor Ring (KV Store)             │
│                                                 │
│  Token Ring:                                    │
│  ┌───────────────────────────────────────────┐  │
│  │     0                                     │  │
│  │   ╱   ╲                                   │  │
│  │  ╱     ╲    ringKeyOfLeader = 0           │  │
│  │ C1(ACTIVE) ← Get(0, Write) → C1이 리더   │  │
│  │  ╲     ╱                                  │  │
│  │   ╲   ╱                                   │  │
│  │    C2(ACTIVE)                             │  │
│  │                                           │  │
│  │  토큰: C1=[42], C2=[157]                  │  │
│  │  ringKeyOfLeader(0)에 가장 가까운 C1이    │  │
│  │  리더로 선출됨                             │  │
│  └───────────────────────────────────────────┘  │
└─────────────────────────────────────────────────┘
```

### 리더 선출 코드

```go
// 소스: pkg/compactor/compactor.go:464-527
func (c *Compactor) loop(ctx context.Context) error {
    // ... RunOnce 처리 ...

    syncTicker := time.NewTicker(c.ringPollPeriod)  // 5초마다 확인
    defer syncTicker.Stop()

    for {
        select {
        case <-ctx.Done():
            // 종료 처리
        case <-syncTicker.C:
            bufDescs, bufHosts, bufZones := ring.MakeBuffersForGet()
            rs, err := c.ring.Get(ringKeyOfLeader, ring.Write, bufDescs, bufHosts, bufZones)

            addrs := rs.GetAddresses()
            if len(addrs) != 1 {
                continue  // 정확히 1개 주소가 아니면 스킵
            }

            if c.ringLifecycler.GetInstanceAddr() == addrs[0] {
                // 내가 리더! → 실행 시작
                if !c.running {
                    c.tablesManager.start(runningCtx)
                    if c.deleteRequestsManager != nil {
                        c.deleteRequestsManager.Start(runningCtx)
                    }
                    if c.cfg.HorizontalScalingMode == HorizontalScalingModeMain {
                        c.JobQueue.Start(runningCtx)
                    }
                    c.running = true
                    c.metrics.compactorRunning.Set(1)
                }
            } else {
                // 내가 리더가 아님 → 실행 중이면 중지
                if c.running {
                    runningCancel()
                    wg.Wait()
                    c.running = false
                    c.metrics.compactorRunning.Set(0)
                }
            }
        }
    }
}
```

### 인스턴스 등록

```go
// 소스: pkg/compactor/compactor.go:601-618
func (c *Compactor) OnRingInstanceRegister(_ *ring.BasicLifecycler, ringDesc ring.Desc, instanceExists bool, _ string, instanceDesc ring.InstanceDesc) (ring.InstanceState, ring.Tokens) {
    var tokens []uint32
    if instanceExists {
        tokens = instanceDesc.GetTokens()
    }

    takenTokens := ringDesc.GetTokens()
    gen := ring.NewRandomTokenGenerator()
    newTokens := gen.GenerateTokens(ringNumTokens-len(tokens), takenTokens)
    tokens = append(tokens, newTokens...)

    return ring.JOINING, tokens
}
```

### 시작 흐름: JOINING → ACTIVE

```go
// 소스: pkg/compactor/compactor.go:387-433
func (c *Compactor) starting(ctx context.Context) (err error) {
    // 서브서비스 시작
    if err := services.StartManagerAndAwaitHealthy(ctx, c.subservices); err != nil {
        return errors.Wrap(err, "unable to start compactor subservices")
    }

    // JOINING 상태 대기
    ring.WaitInstanceState(ctx, c.ring, c.ringLifecycler.GetInstanceID(), ring.JOINING)

    // ACTIVE로 상태 변경
    c.ringLifecycler.ChangeState(ctx, ring.ACTIVE)

    // ACTIVE 상태 확인 대기
    ring.WaitInstanceState(ctx, c.ring, c.ringLifecycler.GetInstanceID(), ring.ACTIVE)

    return nil
}
```

```
시작 흐름:

1. 서브서비스 시작 (Ring Lifecycler, Ring)
       │
       ▼
2. JOINING 상태로 Ring 등록
       │
       ▼
3. Ring에서 자신의 JOINING 상태 확인 대기
       │
       ▼
4. ACTIVE 상태로 변경
       │
       ▼
5. Ring에서 자신의 ACTIVE 상태 확인 대기
       │
       ▼
6. loop() 시작 → 5초마다 리더 확인
```

---

## 5. 초기화 과정 상세

### NewCompactor 함수

```go
// 소스: pkg/compactor/compactor.go:129-200
func NewCompactor(cfg Config, objectStoreClients map[config.DayTime]client.ObjectClient,
    deleteStoreClient client.ObjectClient, schemaConfig config.SchemaConfig,
    limits Limits, indexUpdatePropagationMaxDelay time.Duration,
    r prometheus.Registerer, metricsNamespace string) (*Compactor, error) {

    // 1. 통계 메트릭 설정
    retentionEnabledStats.Set("false"/"true")
    defaultRetentionStats.Set(limits.DefaultLimits().RetentionPeriod.String())

    // 2. Compactor 인스턴스 생성
    compactor := &Compactor{
        cfg:             cfg,
        ringPollPeriod:  5 * time.Second,
        indexCompactors: map[string]IndexCompactor{},
        schemaConfig:    schemaConfig,
        limits:          limits,
    }

    // 3. Ring KV Store 생성
    ringStore, err := kv.NewClient(cfg.CompactorRing.KVStore, ...)

    // 4. Ring Lifecycler 생성
    compactor.ringLifecycler, err = ring.NewBasicLifecycler(...)

    // 5. Ring 클라이언트 생성
    ringCfg := cfg.CompactorRing.ToRingConfig(ringReplicationFactor)
    compactor.ring, err = ring.NewWithStoreClientAndStrategy(ringCfg, ...)

    // 6. 서브서비스 매니저 설정
    compactor.subservices, err = services.NewManager(compactor.ringLifecycler, compactor.ring)
    compactor.subservicesWatcher = services.NewFailureWatcher()

    // 7. init() 호출 - 디렉토리, 삭제 시스템, 스토어 컨테이너 초기화
    compactor.init(objectStoreClients, deleteStoreClient, schemaConfig, ...)

    // 8. 서비스 생성
    compactor.Service = services.NewBasicService(compactor.starting, compactor.loop, compactor.stopping)
    return compactor, nil
}
```

### init() 상세

```go
// 소스: pkg/compactor/compactor.go:202-342
func (c *Compactor) init(...) error {
    // 1. 작업 디렉토리 생성
    chunk_util.EnsureDirectory(c.cfg.WorkingDirectory)

    // 2. 보존 활성화 시 삭제 시스템 초기화
    if c.cfg.RetentionEnabled {
        c.initDeletes(deleteStoreClient, ...)
    }

    // 3. 스키마 기간별 storeContainer 생성
    c.storeContainers = make(map[config.DayTime]storeContainer)
    for from, objectClient := range objectStoreClients {
        var sc storeContainer
        sc.indexStorageClient = storage.NewIndexStorageClient(objectClient, ...)

        if c.cfg.RetentionEnabled {
            // 마커 스토리지 클라이언트 생성 (로컬 또는 오브젝트 스토어)
            markerStorageClient, _ = local.NewFSObjectClient(...)
            // 레거시 마커 파일 마이그레이션
            retention.CopyMarkers(...)
            // Sweeper 생성 (청크 삭제 워커)
            sc.sweeper, _ = retention.NewSweeper(markerStorageClient, chunkClient, ...)
            // TableMarker 생성 (만료 청크 마킹)
            sc.tableMarker, _ = retention.NewMarker(markerStorageClient, c.expirationChecker, ...)
        }

        c.storeContainers[from] = sc
    }

    // 4. 메트릭 생성
    c.metrics = newMetrics(r)

    // 5. tablesManager 생성
    c.tablesManager = newTablesManager(c.cfg, c.storeContainers, c.indexCompactors, ...)

    // 6. 수평 스케일링 모드 시 JobQueue 설정
    if c.cfg.HorizontalScalingMode == HorizontalScalingModeMain {
        c.JobQueue = jobqueue.NewQueue(r)
    }

    return nil
}
```

### 초기화 흐름도

```
NewCompactor()
      │
      ├─ retentionEnabledStats 설정
      ├─ Ring KV Store 생성
      ├─ Ring Lifecycler 생성 (토큰 1개)
      ├─ Ring 클라이언트 생성 (복제 팩터 1)
      ├─ 서브서비스 매니저 설정
      │
      └─ init()
           │
           ├─ WorkingDirectory 생성 확인
           ├─ RetentionEnabled?
           │    ├─ Yes → initDeletes()
           │    │         ├─ DeleteRequestsStore 생성
           │    │         ├─ DeleteRequestHandler 생성
           │    │         ├─ DeleteRequestsManager 생성
           │    │         └─ ExpirationChecker 생성
           │    └─ No → NeverExpiringExpirationChecker
           │
           ├─ 스키마 기간별 storeContainer 생성
           │    ├─ indexStorageClient 생성
           │    ├─ (Retention) 마커 스토리지 생성
           │    ├─ (Retention) 마커 마이그레이션
           │    ├─ (Retention) Sweeper 생성
           │    └─ (Retention) TableMarker 생성
           │
           ├─ 메트릭 생성
           ├─ tablesManager 생성
           └─ (HorizontalScaling) JobQueue 생성
```

---

## 6. 인덱스 압축 프로세스

### tablesManager.start()

```go
// 소스: pkg/compactor/tables_manager.go:59-151
func (c *tablesManager) start(ctx context.Context) {
    // 1. Ring 안정화를 위해 CompactionInterval만큼 대기
    t := time.NewTimer(c.cfg.CompactionInterval)
    select {
    case <-ctx.Done():
        return
    case <-t.C:
        // 대기 완료
    }

    // 2. 초기 압축 실행
    c.runCompaction(ctx, false)

    // 3. 주기적 압축 시작
    go func() {
        ticker := time.NewTicker(c.cfg.CompactionInterval)
        for {
            select {
            case <-ticker.C:
                c.runCompaction(ctx, false)
            case <-ctx.Done():
                return
            }
        }
    }()

    // 4. 보존 활성화 시 별도 루프
    if c.cfg.RetentionEnabled {
        go func() {
            // 초기 보존 적용
            c.runCompaction(ctx, true)
            // 주기적 보존 적용
            ticker := time.NewTicker(c.cfg.ApplyRetentionInterval)
            // ...
        }()

        // 5. 각 storeContainer의 Sweeper 시작
        for _, container := range c.storeContainers {
            go func(sc storeContainer) {
                sc.sweeper.Start()
                <-ctx.Done()
            }(container)
        }
    }
}
```

### 테이블 목록 조회 및 정렬

```go
// 소스: pkg/compactor/tables_manager.go:153-195
func (c *tablesManager) listTableNames(ctx context.Context) ([]string, error) {
    var tables []string
    seen := make(map[string]struct{})

    for _, sc := range c.storeContainers {
        sc.indexStorageClient.RefreshIndexTableNamesCache(ctx)
        tbls, _ := sc.indexStorageClient.ListTables(ctx)
        for _, table := range tbls {
            if table == deletion.DeleteRequestsTableName {
                continue  // 삭제 요청 테이블 제외
            }
            if _, ok := seen[table]; !ok {
                tables = append(tables, table)
                seen[table] = struct{}{}
            }
        }
    }

    // 최신 테이블 우선 정렬
    SortTablesByRange(tables)

    // 제한 적용
    if c.cfg.SkipLatestNTables <= len(tables) {
        tables = tables[c.cfg.SkipLatestNTables:]
    }
    if c.cfg.TablesToCompact > 0 {
        tables = tables[:c.cfg.TablesToCompact]
    }

    return tables, nil
}
```

### SortTablesByRange 함수

```go
// 소스: pkg/compactor/compactor.go:625-635
func SortTablesByRange(tables []string) {
    tableRanges := make(map[string]model.Interval)
    for _, table := range tables {
        tableRanges[table] = retention.ExtractIntervalFromTableName(table)
    }
    sort.Slice(tables, func(i, j int) bool {
        return tableRanges[tables[i]].Start.After(tableRanges[tables[j]].Start)
    })
}
```

테이블은 시간 범위를 기준으로 **최신 우선**으로 정렬된다. 이는 가장 최근 데이터부터 압축하여 쿼리 성능에 즉시 영향을 미치도록 하기 위함이다.

### 압축 프로세스 상세 흐름

```
runCompaction(applyRetention=false)
         │
         ▼
┌───────────────────────────────┐
│ 1. listTableNames()          │
│    → 모든 인덱스 테이블 조회  │
│    → 최신 우선 정렬           │
│    → SkipLatestN 적용         │
└───────────┬───────────────────┘
            ▼
┌───────────────────────────────┐
│ 2. for each table:           │
│    ├─ SchemaPeriodForTable() │  ← 테이블의 스키마 기간 결정
│    │                          │
│    ├─ indexCompactor 조회     │  ← 인덱스 타입에 맞는 압축기
│    │                          │
│    ├─ CompactTable()          │
│    │   ├─ 테넌트별 인덱스 다운로드
│    │   ├─ 인덱스 병합 (dedupe)
│    │   ├─ CompactedIndex 생성
│    │   └─ 새 인덱스 파일 업로드
│    │                          │
│    └─ (applyRetention 시)    │
│       ├─ FindAndMarkChunksForDeletion()
│       └─ 만료 청크 마커 생성  │
└───────────────────────────────┘
```

### SchemaPeriodForTable

```go
// 소스: pkg/compactor/compactor.go:637-645
func SchemaPeriodForTable(cfg config.SchemaConfig, tableName string) (config.PeriodConfig, bool) {
    tableInterval := retention.ExtractIntervalFromTableName(tableName)
    schemaCfg, err := cfg.SchemaForTime(tableInterval.Start)
    if err != nil || schemaCfg.IndexTables.TableFor(tableInterval.Start) != tableName {
        return config.PeriodConfig{}, false
    }
    return schemaCfg, true
}
```

---

## 7. 보존 정책 (Retention)

### ExpirationChecker 인터페이스

```go
// 소스: pkg/compactor/retention/expiration.go:26-35
type ExpirationChecker interface {
    Expired(userID []byte, chk Chunk, lbls labels.Labels, seriesID []byte, tableName string, now model.Time) (bool, filter.Func)
    IntervalMayHaveExpiredChunks(interval model.Interval, userID string) bool
    MarkPhaseStarted()
    MarkPhaseFailed()
    MarkPhaseTimedOut()
    MarkPhaseFinished()
    DropFromIndex(userID []byte, chk Chunk, labels labels.Labels, tableEndTime model.Time, now model.Time) bool
    CanSkipSeries(userID []byte, lbls labels.Labels, seriesID []byte, seriesStart model.Time, tableName string, now model.Time) bool
    MarkSeriesAsProcessed(userID, seriesID []byte, lbls labels.Labels, tableName string) error
}
```

### 복합 ExpirationChecker

Compactor는 보존 정책과 삭제 요청을 모두 처리하기 위해 두 개의 ExpirationChecker를 결합한다.

```go
// 소스: pkg/compactor/compactor.go:544-551
type expirationChecker struct {
    retentionExpiryChecker retention.ExpirationChecker  // 시간 기반 보존
    deletionExpiryChecker  retention.ExpirationChecker  // 삭제 요청 기반
}

func newExpirationChecker(retentionExpiryChecker, deletionExpiryChecker retention.ExpirationChecker) retention.ExpirationChecker {
    return &expirationChecker{retentionExpiryChecker, deletionExpiryChecker}
}
```

### Expired 판정 로직

```go
// 소스: pkg/compactor/compactor.go:553-559
func (e *expirationChecker) Expired(userID []byte, chk retention.Chunk, lbls labels.Labels, seriesID []byte, tableName string, now model.Time) (bool, filter.Func) {
    // 1단계: 보존 정책에 의한 만료 확인
    if expired, nonDeletedIntervals := e.retentionExpiryChecker.Expired(userID, chk, lbls, seriesID, tableName, now); expired {
        return expired, nonDeletedIntervals
    }
    // 2단계: 삭제 요청에 의한 만료 확인
    return e.deletionExpiryChecker.Expired(userID, chk, lbls, seriesID, tableName, now)
}
```

### 보존 기간 만료 판정

```go
// 소스: pkg/compactor/retention/expiration.go:57-65
func (e *expirationChecker) Expired(userID []byte, chk Chunk, lbls labels.Labels, _ []byte, _ string, now model.Time) (bool, filter.Func) {
    userIDStr := unsafeGetString(userID)
    period := e.tenantsRetention.RetentionPeriodFor(userIDStr, lbls)
    if period <= 0 {
        return false, nil  // 0은 보존 비활성화
    }
    return now.Sub(chk.Through) > period, nil
}
```

```
보존 판정 시각화:

시간축: ──────────────────────────────────────────►
        │                    │              │
        chk.From    chk.Through          now
                             │              │
                             ├──────────────┤
                             │  경과 시간   │
                             │              │
        if 경과 시간 > retention_period → 만료!
```

### TableMarker: 만료 청크 마킹

```go
// 소스: pkg/compactor/retention/retention.go:119-125
type TableMarker interface {
    FindAndMarkChunksForDeletion(ctx context.Context, tableName, userID string, indexProcessor IndexProcessor, logger log.Logger) (bool, bool, error)
    MarkChunksForDeletion(tableName string, chunks []string) error
}

// 소스: pkg/compactor/retention/retention.go:127-133
type Marker struct {
    markerStorageClient client.ObjectClient
    expiration          ExpirationChecker
    markerMetrics       *markerMetrics
    chunkClient         client.Client
    markTimeout         time.Duration
}
```

### 글로벌 vs 테넌트별 보존

```
글로벌 보존 정책:
┌──────────────────────────────────────────────┐
│ limits:                                      │
│   retention_period: 720h  # 30일             │
│                                              │
│ 모든 테넌트에 동일하게 30일 보존 적용        │
└──────────────────────────────────────────────┘

테넌트별 보존 정책:
┌──────────────────────────────────────────────┐
│ overrides:                                   │
│   tenant-a:                                  │
│     retention_period: 2160h  # 90일          │
│   tenant-b:                                  │
│     retention_period: 168h   # 7일           │
│                                              │
│ 스트림 레벨 보존:                            │
│   tenant-a:                                  │
│     retention_stream:                        │
│       - selector: '{env="prod"}'             │
│         priority: 1                          │
│         period: 4320h  # 180일               │
│       - selector: '{env="dev"}'              │
│         priority: 2                          │
│         period: 72h    # 3일                 │
└──────────────────────────────────────────────┘
```

---

## 8. 삭제 처리 시스템

### DeleteRequestsStore

```go
// 소스: pkg/compactor/deletion/delete_requests_store.go:32-47
type DeleteRequestsStore interface {
    AddDeleteRequest(ctx context.Context, userID, query string, startTime, endTime model.Time, shardByInterval time.Duration) (string, error)
    GetAllRequests(ctx context.Context) ([]deletionproto.DeleteRequest, error)
    GetAllDeleteRequestsForUser(ctx context.Context, userID string, forQuerytimeFiltering bool, timeRange *TimeRange) ([]deletionproto.DeleteRequest, error)
    RemoveDeleteRequest(ctx context.Context, userID string, requestID string) error
    GetDeleteRequest(ctx context.Context, userID, requestID string) (deletionproto.DeleteRequest, error)
    GetCacheGenerationNumber(ctx context.Context, userID string) (string, error)
    MergeShardedRequests(ctx context.Context) error
    MarkShardAsProcessed(ctx context.Context, req deletionproto.DeleteRequest) error
    GetUnprocessedShards(ctx context.Context) ([]deletionproto.DeleteRequest, error)
    Stop()
}
```

### 지원 DB 타입

```go
// 소스: pkg/compactor/deletion/delete_requests_store.go:17-24
type DeleteRequestsStoreDBType string

const (
    DeleteRequestsStoreDBTypeBoltDB DeleteRequestsStoreDBType = "boltdb"
    DeleteRequestsStoreDBTypeSQLite DeleteRequestsStoreDBType = "sqlite"
)
```

### 삭제 요청 라이프사이클

```
1. 삭제 요청 접수
   POST /loki/api/v1/delete
   → DeleteRequestHandler.AddDeleteRequest()
   → DeleteRequestsStore.AddDeleteRequest()
         │
         ▼
2. 취소 가능 기간 (DeleteRequestCancelPeriod = 24h)
   └── 이 기간 내에는 삭제 요청 취소 가능
         │
         ▼
3. 삭제 요청 처리 (Compactor 압축 주기에서 실행)
   └── DeleteRequestsManager가 미처리 요청 조회
       └── 각 요청에 대해:
           ├── 인덱스에서 매칭되는 청크 검색
           ├── ExpirationChecker에 삭제 정보 전달
           └── Mark 단계: 삭제 대상 청크 마킹
         │
         ▼
4. 청크 삭제 (Sweep 단계)
   └── RetentionDeleteDelay (2h) 대기 후
       └── Sweeper가 마킹된 청크를 Object Store에서 삭제
         │
         ▼
5. 삭제 완료
   └── 삭제 요청 상태를 "processed"로 업데이트
```

### initDeletes 함수

```go
// 소스: pkg/compactor/compactor.go:344-385
func (c *Compactor) initDeletes(objectClient client.ObjectClient, indexUpdatePropagationMaxDelay time.Duration, r prometheus.Registerer, limits Limits) error {
    // 1. 삭제 작업 디렉토리 설정
    deletionWorkDir := filepath.Join(c.cfg.WorkingDirectory, "deletion")

    // 2. DeleteRequestsStore 생성
    store, _ := deletion.NewDeleteRequestsStore(
        deletion.DeleteRequestsStoreDBType(c.cfg.DeleteRequestStoreDBType),
        deletionWorkDir,
        indexStorageClient,
        deletion.DeleteRequestsStoreDBType(c.cfg.BackupDeleteRequestStoreDBType),
        indexUpdatePropagationMaxDelay,
    )
    c.deleteRequestsStore = store

    // 3. HTTP 핸들러 생성 (REST API)
    c.DeleteRequestsHandler = deletion.NewDeleteRequestHandler(
        c.deleteRequestsStore,
        c.cfg.DeleteMaxInterval,
        c.cfg.DeleteRequestCancelPeriod,
        r,
    )

    // 4. gRPC 핸들러 생성
    c.DeleteRequestsGRPCHandler = deletion.NewGRPCRequestHandler(c.deleteRequestsStore, limits)

    // 5. DeleteRequestsManager 생성
    c.deleteRequestsManager, _ = deletion.NewDeleteRequestsManager(
        deletionWorkDir,
        c.deleteRequestsStore,
        c.cfg.DeleteRequestCancelPeriod,
        c.cfg.DeleteBatchSize,
        limits,
        c.cfg.HorizontalScalingMode == HorizontalScalingModeMain,
        client.NewPrefixedObjectClient(objectClient, c.cfg.JobsConfig.Deletion.DeletionManifestStorePrefix),
        r,
    )

    // 6. 복합 ExpirationChecker 생성
    c.expirationChecker = newExpirationChecker(retention.NewExpirationChecker(limits), c.deleteRequestsManager)
    return nil
}
```

---

## 9. Lazy Deletion 패턴: Mark-Sweep 2단계

### Mark-Sweep 패턴 설명

Loki의 보존/삭제 시스템은 가비지 컬렉터와 유사한 Mark-Sweep 2단계 패턴을 사용한다.

```
┌──────────────────────────────────────────────────────┐
│                  Mark 단계                            │
│                                                      │
│  ExpirationChecker.Expired() 호출                    │
│       │                                              │
│       ├─ 보존 정책 만료? → Yes                       │
│       │   └─ TableMarker.FindAndMarkChunksForDeletion│
│       │       └─ 마커 파일 생성 (로컬 또는 오브젝트) │
│       │                                              │
│       └─ 삭제 요청 매칭? → Yes                       │
│           └─ 삭제 대상 청크 마킹                     │
│                                                      │
│  ┌──────────────────────────────────────────┐        │
│  │  마커 파일 저장 위치:                     │        │
│  │  {WorkingDir}/retention/{period}/markers/ │        │
│  │  또는                                     │        │
│  │  {ObjectStore}/{DeletionMarkerPrefix}/    │        │
│  └──────────────────────────────────────────┘        │
└──────────────────────────────────────────────────────┘
                        │
                        │ RetentionDeleteDelay (2h)
                        ▼
┌──────────────────────────────────────────────────────┐
│                  Sweep 단계                           │
│                                                      │
│  Sweeper.Start()                                     │
│       │                                              │
│       ├─ 마커 파일 목록 조회                         │
│       ├─ 각 마커에 대해:                             │
│       │   ├─ 생성 시간 + DeleteDelay > now? → 스킵   │
│       │   ├─ 대상 청크를 Object Store에서 삭제        │
│       │   └─ 마커 파일 삭제                          │
│       │                                              │
│       └─ 워커 풀 (RetentionDeleteWorkCount)로 병렬   │
│          처리 (기본 150개 워커)                       │
└──────────────────────────────────────────────────────┘
```

### 왜 Lazy Deletion인가?

1. **안전성**: 즉시 삭제하면 인덱스와 청크 사이의 불일치가 발생할 수 있다. Mark 후 일정 시간(기본 2시간) 대기하여 인덱스 업데이트가 전파되도록 한다.
2. **복구 가능성**: Mark 단계에서만 마커를 생성하므로, 실수로 잘못된 보존 정책을 적용한 경우 Sweep 전에 마커를 삭제하여 복구할 수 있다.
3. **성능**: 인덱스 순회와 실제 청크 삭제를 분리하여 인덱스 작업의 성능에 영향을 주지 않는다.

### Sweeper 초기화

```go
// 소스: pkg/compactor/compactor.go:293
sc.sweeper, err = retention.NewSweeper(
    markerStorageClient,
    chunkClient,
    c.cfg.RetentionDeleteWorkCount,
    c.cfg.RetentionDeleteDelay,
    c.cfg.RetentionBackoffConfig,
    r,
)
```

### DropFromIndex 최적화

```go
// 소스: pkg/compactor/retention/expiration.go:70-78
func (e *expirationChecker) DropFromIndex(userID []byte, _ Chunk, labels labels.Labels, tableEndTime model.Time, now model.Time) bool {
    userIDStr := unsafeGetString(userID)
    period := e.tenantsRetention.RetentionPeriodFor(userIDStr, labels)
    if period <= 0 {
        return false
    }
    return now.Sub(tableEndTime) > period
}
```

`DropFromIndex`는 테이블의 끝 시간이 보존 기간을 넘긴 경우, 해당 청크 엔트리를 인덱스에서 바로 제거할 수 있는 최적화이다. 이 경우 실제 청크 데이터 삭제 없이 인덱스에서만 제거하므로 성능이 좋다.

### CanSkipSeries 최적화

```go
// 소스: pkg/compactor/retention/expiration.go:89-98
func (e *expirationChecker) CanSkipSeries(userID []byte, lbls labels.Labels, _ []byte, seriesStart model.Time, _ string, now model.Time) bool {
    period := e.tenantsRetention.RetentionPeriodFor(userIDStr, lbls)
    if period <= 0 {
        return true  // 보존 비활성화 시 스킵
    }
    return now.Sub(seriesStart) < period  // 시리즈가 아직 유효하면 스킵
}
```

시리즈의 시작 시간이 보존 기간 이내인 경우, 해당 시리즈의 모든 청크는 아직 유효하므로 전체 시리즈를 건너뛸 수 있다.

---

## 10. 수평 스케일링 모드

### 모드 정의

```go
// 소스: pkg/compactor/compactor.go:70-74
const (
    HorizontalScalingModeDisabled = "disabled"  // 기본: 단일 인스턴스
    HorizontalScalingModeMain     = "main"      // 메인: 작업 분배
    HorizontalScalingModeWorker   = "worker"    // 워커: 작업 수행
)
```

### 수평 스케일링 아키텍처

```
모드: disabled (기본)
┌────────────────────────────────┐
│  Compactor (단일 인스턴스)     │
│  ├─ 인덱스 압축                │
│  ├─ 보존 정책 적용             │
│  └─ 삭제 처리                  │
└────────────────────────────────┘

모드: main + worker
┌────────────────────────────────┐
│  Compactor Main                │
│  ├─ 인덱스 압축                │
│  ├─ 보존 정책 적용             │
│  ├─ 작업 생성 및 분배          │
│  │                             │
│  │  JobQueue                   │
│  │  ┌──────────────────────┐   │
│  │  │ 삭제 작업 1          │   │──── 분배 ────┐
│  │  │ 삭제 작업 2          │   │              │
│  │  │ 삭제 작업 3          │   │              │
│  │  └──────────────────────┘   │              │
│  │                             │              │
│  └─ 삭제 요청 관리             │              │
└────────────────────────────────┘              │
                                                │
┌────────────────────┐  ┌────────────────────┐  │
│  Compactor Worker1 │  │  Compactor Worker2 │ ◄┘
│  ├─ 작업 수신      │  │  ├─ 작업 수신      │
│  └─ 청크 삭제 수행 │  │  └─ 청크 삭제 수행 │
└────────────────────┘  └────────────────────┘
```

### JobQueue 초기화

```go
// 소스: pkg/compactor/compactor.go:332-339
if c.cfg.HorizontalScalingMode == HorizontalScalingModeMain {
    c.JobQueue = jobqueue.NewQueue(r)
    if c.cfg.RetentionEnabled {
        if err := c.JobQueue.RegisterBuilder(
            grpc.JOB_TYPE_DELETION,
            c.deleteRequestsManager.JobBuilder(),
            c.cfg.JobsConfig.Deletion.Timeout,
            c.cfg.JobsConfig.Deletion.MaxRetries,
            r,
        ); err != nil {
            return err
        }
    }
}
```

---

## 11. tablesManager: 테이블 관리

### tablesManager 구조체

```go
// 소스: pkg/compactor/tables_manager.go:26-35
type tablesManager struct {
    cfg               Config
    expirationChecker retention.ExpirationChecker
    storeContainers   map[config.DayTime]storeContainer
    indexCompactors   map[string]IndexCompactor
    schemaConfig      config.SchemaConfig
    metrics           *metrics
    tableLocker       *tableLocker
    wg                sync.WaitGroup
}
```

### TablesManager 인터페이스

```go
// 소스: pkg/compactor/tables_manager.go:19-23
type TablesManager interface {
    CompactTable(ctx context.Context, tableName string, applyRetention bool) error
    ApplyStorageUpdates(ctx context.Context, iterator deletion.StorageUpdatesIterator) error
    IterateTables(ctx context.Context, callback func(string, deletion.Table) error) (err error)
}
```

### tableLocker

`tableLocker`는 동일 테이블에 대해 동시에 압축과 보존이 실행되지 않도록 테이블 수준의 잠금을 제공한다.

```
테이블별 잠금 시나리오:

Thread 1: 압축 (table_1234)
Thread 2: 보존 (table_1234) ← 대기

┌─────────────────────────────────────────┐
│  tableLocker                            │
│                                         │
│  locks:                                 │
│    table_1234: [locked by Thread 1]     │
│    table_1235: [unlocked]               │
│    table_1236: [locked by Thread 2]     │
│                                         │
│  Thread 2: table_1234 lock 요청         │
│  → 대기 (Thread 1이 해제할 때까지)      │
└─────────────────────────────────────────┘
```

### 시작 시 대기 전략

```go
// 소스: pkg/compactor/tables_manager.go:66-79
func (c *tablesManager) start(ctx context.Context) {
    t := time.NewTimer(c.cfg.CompactionInterval)
    defer t.Stop()
    level.Info(util_log.Logger).Log("msg",
        fmt.Sprintf("waiting %v for ring to stay stable and previous compactions to finish before starting compactor",
            c.cfg.CompactionInterval))
    select {
    case <-ctx.Done():
        stopped = true
        return
    case <-t.C:
        // 대기 완료
    }
    // ...
}
```

이 대기는 Ring 토폴로지가 안정화되고, 이전 Compactor 인스턴스가 작업을 완료할 시간을 확보하기 위함이다. Ring의 리더가 바뀔 때, 이전 리더의 작업이 아직 진행 중일 수 있으므로 CompactionInterval만큼 기다린다.

---

## 12. 메트릭 및 운영 가이드

### 핵심 메트릭

| 메트릭 | 타입 | 설명 |
|--------|------|------|
| `loki_compactor_running` | Gauge | Compactor 실행 상태 (0/1) |
| `loki_compactor_compaction_duration_seconds` | Histogram | 압축 작업 소요 시간 |
| `loki_compactor_tables_processed_total` | Counter | 처리된 테이블 수 |
| `loki_compactor_retention_marks_total` | Counter | 생성된 삭제 마커 수 |
| `loki_compactor_retention_sweeps_total` | Counter | 실제 삭제된 청크 수 |
| `loki_compactor_delete_requests_total` | Counter | 삭제 요청 수 |
| `loki_compactor_delete_requests_processed_total` | Counter | 처리된 삭제 요청 수 |

### 운영 체크리스트

```
1. Ring 상태 확인
   GET /compactor/ring
   - 정확히 1개의 ACTIVE 인스턴스가 있어야 함
   - compactorRunning = 1 확인

2. 디스크 공간 확인
   - WorkingDirectory에 충분한 여유 공간
   - MaxCompactionParallelism * 테이블_크기만큼 필요

3. 보존 정책 적용 모니터링
   - loki_compactor_retention_marks_total 증가 확인
   - loki_compactor_retention_sweeps_total 증가 확인
   - RetentionDeleteDelay 이후에만 실제 삭제 발생

4. 삭제 요청 상태 확인
   GET /loki/api/v1/delete
   - 미처리 요청 수 확인
   - DeleteBatchSize가 미처리 요청보다 큰지 확인
```

### 트러블슈팅 가이드

```
문제: 보존 정책이 적용되지 않음
──────────────────────────────────
1. RetentionEnabled = true 확인
2. Ring에서 리더가 선출되었는지 확인
   (loki_compactor_running == 1)
3. ApplyRetentionInterval이 0이 아닌지 확인
4. 테넌트별 retention_period가 설정되었는지 확인
5. 로그에서 "failed to apply retention" 검색

문제: 압축이 느림
──────────────────────────────────
1. MaxCompactionParallelism 증가 고려
2. UploadParallelism 증가 고려
3. TablesToCompact로 처리량 제한
4. SkipLatestNTables로 최신 테이블 건너뛰기
5. 디스크 I/O 병목 확인

문제: 삭제 요청이 처리되지 않음
──────────────────────────────────
1. DeleteRequestCancelPeriod(24h) 이후에 처리 시작
2. DeleteBatchSize(70)보다 많은 요청이 쌓여있는지 확인
3. DeleteMaxInterval(24h) 초과 요청은 자동 분할됨
4. RetentionDeleteDelay(2h) 후에 실제 삭제 발생
```

---

## 13. 설계 결정 분석

### 왜 Ring 기반 단일 인스턴스인가?

**문제**: 인덱스 압축은 테이블 파일을 읽고, 병합하고, 새 파일을 쓰고, 이전 파일을 삭제한다. 여러 인스턴스가 동시에 같은 작업을 수행하면 데이터 손실이 발생한다.

**설계 결정**:
- Ring을 사용하여 리더를 선출하고, 리더만 압축 작업을 수행한다.
- `ringReplicationFactor = 1`과 `ringNumTokens = 1`로 설정하여 정확히 하나의 인스턴스만 선출되도록 보장한다.
- 리더가 변경되면 이전 리더는 자동으로 작업을 중지하고, 새 리더는 CompactionInterval만큼 대기한 후 작업을 시작한다.

**트레이드오프**:
- (+) 단순하고 안전한 설계
- (+) Ring 기반이므로 자동 장애 조치
- (-) 단일 인스턴스가 모든 작업을 처리하므로 수직 확장 한계
- (-) 리더 전환 시 CompactionInterval만큼의 지연

### 왜 Mark-Sweep 2단계인가?

**문제**: 인덱스에서 청크 참조를 제거하고 동시에 Object Store에서 청크를 삭제하면, 중간에 장애가 발생할 때 인덱스와 데이터의 불일치가 생긴다.

**설계 결정**:
1. **Mark**: 인덱스에서 만료된 청크를 식별하고 마커 파일만 생성
2. **Delay**: `RetentionDeleteDelay`(기본 2시간) 동안 대기
3. **Sweep**: 마커 파일을 읽어 실제 청크를 삭제

**안전 보장**:
- 마커 생성은 멱등(idempotent)하므로 반복 실행해도 안전
- 청크 삭제는 마커가 있는 경우에만 수행
- 마커가 있지만 청크가 이미 삭제된 경우 무시(noop)

### 왜 storeContainer를 기간별로 분리하는가?

**문제**: Loki의 스키마 버전이 업그레이드되면, 이전 기간과 새 기간의 인덱스 형식이 다를 수 있다.

**설계 결정**: `storeContainers` 맵에서 `config.DayTime`을 키로 사용하여 기간별로 독립적인 인덱스 스토리지 클라이언트, TableMarker, Sweeper를 유지한다.

```go
storeContainers map[config.DayTime]storeContainer
```

이를 통해:
- 각 기간의 인덱스 형식에 맞는 IndexCompactor를 사용
- 각 기간의 Object Store 경로 프리픽스를 독립적으로 관리
- 기간별로 다른 보존 정책 적용 가능

---

## 요약

Loki의 Compactor는 다음 핵심 원칙으로 설계되었다:

1. **단일 실행 보장**: Ring 기반 리더 선출로 정확히 하나의 인스턴스만 작업을 수행한다.
2. **안전한 삭제**: Mark-Sweep 2단계 패턴과 DeleteDelay로 데이터 안전성을 보장한다.
3. **유연한 보존**: 글로벌, 테넌트별, 스트림별 보존 정책을 계층적으로 적용한다.
4. **수평 확장 가능**: HorizontalScalingMode를 통해 Main-Worker 패턴으로 삭제 작업을 분산할 수 있다.
5. **스키마 호환성**: 기간별 storeContainer로 스키마 버전 간 호환성을 유지한다.
