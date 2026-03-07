# 16. Ruler 알림 규칙 엔진 Deep-Dive

## 목차
1. [개요: Ruler란?](#1-개요-ruler란)
2. [Ruler 아키텍처](#2-ruler-아키텍처)
3. [Evaluator 인터페이스](#3-evaluator-인터페이스)
4. [LocalEvaluator: 직접 LogQL 엔진 호출](#4-localevaluator-직접-logql-엔진-호출)
5. [RemoteEvaluator: Query Frontend 통해 실행](#5-remoteevaluator-query-frontend-통해-실행)
6. [JitterEvaluator: 결정적 지터로 썬더링 허드 방지](#6-jitterevaluator-결정적-지터로-썬더링-허드-방지)
7. [MultiTenantManager: 테넌트별 규칙 관리](#7-multitenantmanager-테넌트별-규칙-관리)
8. [규칙 저장소: RuleStore](#8-규칙-저장소-rulestore)
9. [평가 루프: 알림 규칙 vs 녹화 규칙](#9-평가-루프-알림-규칙-vs-녹화-규칙)
10. [Alertmanager 연동](#10-alertmanager-연동)
11. [Ring 기반 분산](#11-ring-기반-분산)
12. [Remote Write와 WAL Registry](#12-remote-write와-wal-registry)
13. [메트릭과 모니터링](#13-메트릭과-모니터링)
14. [설정 참조](#14-설정-참조)

---

## 1. 개요: Ruler란?

Loki Ruler는 LogQL 쿼리를 주기적으로 평가하여 **알림 규칙(alerting rules)**과 **녹화 규칙(recording rules)**을 실행하는 컴포넌트다. Prometheus의 Rules Engine을 기반으로 하되, LogQL 쿼리 엔진과 통합되어 있다.

```
┌─────────────────────────────────────────────────────┐
│                     Ruler                           │
│                                                     │
│  ┌──────────┐    ┌───────────┐    ┌──────────────┐ │
│  │ Rule     │───►│ Evaluator │───►│ LogQL Engine │ │
│  │ Store    │    │           │    │ 또는 Query   │ │
│  │(S3/Local)│    │ (Local/   │    │ Frontend     │ │
│  └──────────┘    │  Remote)  │    └──────────────┘ │
│                  └─────┬─────┘                     │
│                        │                           │
│              ┌─────────▼──────────┐                │
│              │   결과 처리         │                │
│              │                    │                │
│              ├─► 알림 → Alertmgr  │                │
│              │                    │                │
│              └─► 메트릭 → Remote  │                │
│                   Write           │                │
└─────────────────────────────────────────────────────┘
```

---

## 2. Ruler 아키텍처

### 2.1 NewRuler 함수

소스 경로: `pkg/ruler/ruler.go`

```go
// pkg/ruler/ruler.go (line 16-51)
func NewRuler(cfg Config, evaluator Evaluator, reg prometheus.Registerer,
    logger log.Logger, ruleStore rulestore.RuleStore,
    limits RulesLimits, metricsNamespace string,
) (*ruler.Ruler, error) {
    // Remote Write 설정 호환성 처리
    if len(cfg.RemoteWrite.Clients) > 0 && cfg.RemoteWrite.Client != nil {
        return nil, errors.New("both 'client' and 'clients' options are defined")
    }

    if len(cfg.RemoteWrite.Clients) == 0 && cfg.RemoteWrite.Client != nil {
        cfg.RemoteWrite.Clients["default"] = *cfg.RemoteWrite.Client
    }

    // MultiTenantManager 생성
    mgr, err := ruler.NewDefaultMultiTenantManager(
        cfg.Config,
        MultiTenantRuleManager(cfg, evaluator, limits, logger, reg),
        reg, logger, limits, metricsNamespace,
    )
    if err != nil {
        return nil, err
    }

    // Ruler 인스턴스 생성 (Ring 기반 분산 포함)
    return ruler.NewRuler(
        cfg.Config,
        MultiTenantManagerAdapter(mgr),
        reg, logger, ruleStore, limits, metricsNamespace,
    )
}
```

### 2.2 계층 구조

```
┌─────────────────────────────────────────────┐
│            ruler.Ruler (base)               │
│  ─ Ring 기반 테넌트 분배                    │
│  ─ 규칙 동기화 루프                          │
│  ─ API 제공                                 │
│                                             │
│  ┌───────────────────────────────────────┐  │
│  │  DefaultMultiTenantManager            │  │
│  │  ─ 테넌트별 RulesManager 관리         │  │
│  │  ─ Notifier 관리                      │  │
│  │                                       │  │
│  │  ┌─────────────────────────────────┐  │  │
│  │  │  Prometheus Rules Manager       │  │  │
│  │  │  ─ 개별 규칙 그룹 평가          │  │  │
│  │  │  ─ Evaluator 호출              │  │  │
│  │  └─────────────────────────────────┘  │  │
│  └───────────────────────────────────────┘  │
└─────────────────────────────────────────────┘
```

### 2.3 Config 구조체

소스 경로: `pkg/ruler/config.go`

```go
// pkg/ruler/config.go (line 18-28)
type Config struct {
    ruler.Config `yaml:",inline"`          // base ruler config

    WAL         instance.Config `yaml:"wal,omitempty"`
    WALCleaner  cleaner.Config  `yaml:"wal_cleaner,omitempty"`
    RemoteWrite RemoteWriteConfig `yaml:"remote_write,omitempty"`

    Evaluation EvaluationConfig `yaml:"evaluation,omitempty"`
}
```

---

## 3. Evaluator 인터페이스

소스 경로: `pkg/ruler/evaluator.go`

### 3.1 인터페이스 정의

```go
// pkg/ruler/evaluator.go (line 14-17)
type Evaluator interface {
    Eval(ctx context.Context, qs string, now time.Time) (*logqlmodel.Result, error)
}
```

`Evaluator`는 Ruler의 핵심 추상화다. LogQL 쿼리 문자열과 평가 시점을 받아 결과를 반환한다. 이 인터페이스를 통해 **로컬 평가**와 **원격 평가**를 투명하게 전환할 수 있다.

### 3.2 EvaluationConfig

```go
// pkg/ruler/evaluator.go (line 19-24)
type EvaluationConfig struct {
    Mode      string        `yaml:"mode,omitempty"`     // "local" 또는 "remote"
    MaxJitter time.Duration `yaml:"max_jitter"`

    QueryFrontend QueryFrontendConfig `yaml:"query_frontend,omitempty"`
}
```

| 설정 | 기본값 | 설명 |
|------|--------|------|
| `ruler.evaluation.mode` | `local` | 평가 모드 (local/remote) |
| `ruler.evaluation.max-jitter` | `0` | 최대 지터 시간 |
| `ruler.evaluation.query-frontend.address` | `""` | Query Frontend 주소 |

### 3.3 Evaluator 데코레이터 패턴

```
┌──────────────────────────────────────────┐
│         Evaluator 체인                    │
│                                          │
│  EvaluatorWithJitter                     │
│    └─► LocalEvaluator                    │
│         └─► LogQL Engine                 │
│                                          │
│  또는                                    │
│                                          │
│  EvaluatorWithJitter                     │
│    └─► RemoteEvaluator                   │
│         └─► httpgrpc → Query Frontend    │
└──────────────────────────────────────────┘
```

---

## 4. LocalEvaluator: 직접 LogQL 엔진 호출

소스 경로: `pkg/ruler/evaluator_local.go`

### 4.1 구조체

```go
// pkg/ruler/evaluator_local.go (line 20-28)
const EvalModeLocal = "local"

type LocalEvaluator struct {
    engine *logql.QueryEngine
    logger log.Logger

    insightsLogger log.Logger  // 인사이트 로깅용 별도 로거
}
```

### 4.2 생성

```go
// pkg/ruler/evaluator_local.go (line 30-40)
func NewLocalEvaluator(engine *logql.QueryEngine, logger log.Logger,
) (*LocalEvaluator, error) {
    if engine == nil {
        return nil, fmt.Errorf("given engine is nil")
    }

    return &LocalEvaluator{
        engine:         engine,
        logger:         logger,
        insightsLogger: log.With(util_log.Logger,
            "msg", "request timings",
            "insight", "true",
            "source", "loki_ruler"),
    }, nil
}
```

### 4.3 Eval 메서드

```go
// pkg/ruler/evaluator_local.go (line 42-69)
func (l *LocalEvaluator) Eval(ctx context.Context, qs string, now time.Time,
) (*logqlmodel.Result, error) {
    // LogQL 파라미터 생성 (instant query)
    params, err := logql.NewLiteralParams(
        qs,          // 쿼리 문자열
        now,         // 시작 시간 = 현재
        now,         // 종료 시간 = 현재 (instant query)
        0,           // step
        0,           // interval
        logproto.FORWARD,
        0,           // limit
        nil, nil,
    )
    if err != nil {
        return nil, err
    }

    // LogQL 엔진으로 쿼리 실행
    q := l.engine.Query(params)
    res, err := q.Exec(ctx)
    if err != nil {
        return nil, err
    }

    // 규칙 컨텍스트에서 규칙 이름/타입 추출
    ruleName, ruleType := GetRuleDetailsFromContext(ctx)

    // 인사이트 로깅
    level.Info(l.insightsLogger).Log(
        "rule_name", ruleName,
        "rule_type", ruleType,
        "total", res.Statistics.Summary.ExecTime,
        "total_bytes", res.Statistics.Summary.TotalBytesProcessed,
        "query_hash", util.HashedQuery(qs),
    )

    return &res, nil
}
```

### 4.4 LocalEvaluator 실행 흐름

```
LocalEvaluator.Eval(ctx, query, now)
     │
     ▼
[1] logql.NewLiteralParams() ── instant query 파라미터
     │
     ▼
[2] engine.Query(params) ── 쿼리 객체 생성
     │
     ▼
[3] q.Exec(ctx) ── 쿼리 실행
     │
     │  ┌──────────────────────────────────────┐
     │  │  LogQL Engine 내부:                   │
     │  │  1. 파싱 (AST 생성)                  │
     │  │  2. 최적화 (샤딩, 범위 제한)          │
     │  │  3. Store에서 데이터 읽기             │
     │  │  4. 집계/필터 실행                    │
     │  │  5. 결과 반환                         │
     │  └──────────────────────────────────────┘
     │
     ▼
[4] 인사이트 로깅 (exec_time, bytes_processed)
     │
     ▼
[5] *logqlmodel.Result 반환
```

---

## 5. RemoteEvaluator: Query Frontend 통해 실행

소스 경로: `pkg/ruler/evaluator_remote.go`

### 5.1 구조체

```go
// pkg/ruler/evaluator_remote.go (line 70-81)
type RemoteEvaluator struct {
    client    httpgrpc.HTTPClient
    overrides RulesLimits
    logger    log.Logger

    insightsLogger log.Logger

    metrics *metrics
}
```

### 5.2 metrics 구조체

```go
// pkg/ruler/evaluator_remote.go (line 61-68)
type metrics struct {
    reqDurationSecs     *prometheus.HistogramVec
    responseSizeBytes   *prometheus.HistogramVec
    responseSizeSamples *prometheus.HistogramVec

    successfulEvals *prometheus.CounterVec
    failedEvals     *prometheus.CounterVec
}
```

### 5.3 Eval 메서드

```go
// pkg/ruler/evaluator_remote.go (line 150-173)
func (r *RemoteEvaluator) Eval(ctx context.Context, qs string, now time.Time,
) (*logqlmodel.Result, error) {
    orgID, err := user.ExtractOrgID(ctx)
    if err != nil {
        return nil, fmt.Errorf("failed to retrieve tenant ID: %w", err)
    }

    ch := make(chan queryResponse, 1)

    // 테넌트별 타임아웃 설정
    timeout := r.overrides.RulerRemoteEvaluationTimeout(orgID)
    tCtx, cancel := context.WithTimeout(ctx, timeout)
    defer cancel()

    // 비동기 쿼리 실행
    go r.Query(tCtx, ch, orgID, qs, now)

    for {
        select {
        case <-tCtx.Done():
            r.metrics.failedEvals.WithLabelValues("timeout", orgID).Inc()
            return nil, fmt.Errorf("remote rule evaluation exceeded deadline")
        case res := <-ch:
            return res.res, res.err
        }
    }
}
```

### 5.4 원격 쿼리 실행 흐름

```
RemoteEvaluator.Eval()
     │
     ▼
[1] 테넌트 ID 추출
     │
     ▼
[2] 타임아웃 컨텍스트 생성
     │  (ruler_remote_evaluation_timeout)
     │
     ▼
[3] 고루틴으로 Query() 실행
     │
     │  Query() 내부:
     │  ┌────────────────────────────────────────┐
     │  │ HTTP POST /loki/api/v1/query           │
     │  │                                        │
     │  │ Headers:                               │
     │  │  - User-Agent: loki-ruler/VERSION      │
     │  │  - Content-Type: form-urlencoded       │
     │  │  - X-Query-Tags: source=ruler,         │
     │  │       rule_name=xxx, rule_type=xxx      │
     │  │  - X-Scope-OrgID: tenant_id            │
     │  │                                        │
     │  │ Body: query=...&direction=forward       │
     │  │       &time=RFC3339                     │
     │  └────────────────────────────────────────┘
     │
     ▼
[4] 응답 처리:
     ├── 2xx: JSON 디코딩 → logqlmodel.Result
     ├── Non-2xx: 에러 반환 (upstream_error)
     ├── 크기 초과: 에러 반환 (max_size)
     └── 타임아웃: 에러 반환 (timeout)
```

### 5.5 Query Frontend 연결

```go
// pkg/ruler/evaluator_remote.go (line 176-205)
func DialQueryFrontend(cfg *QueryFrontendConfig) (httpgrpc.HTTPClient, error) {
    tlsDialOptions, err := cfg.TLS.GetGRPCDialOptions(cfg.TLSEnabled)
    dialOptions := append(
        []grpc.DialOption{
            grpc.WithKeepaliveParams(keepalive.ClientParameters{
                Time:                10 * time.Second,
                Timeout:             5 * time.Second,
                PermitWithoutStream: true,
            }),
            grpc.WithChainUnaryInterceptor(
                middleware.ClientUserHeaderInterceptor,
            ),
            grpc.WithDefaultServiceConfig(serviceConfig), // round_robin
            grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
        },
        tlsDialOptions...,
    )

    conn, err := grpc.Dial(cfg.Address, dialOptions...)
    return httpgrpc.NewHTTPClient(conn), nil
}
```

### 5.6 Local vs Remote 비교

```
┌────────────────────┬─────────────────────┬─────────────────────┐
│                    │ LocalEvaluator       │ RemoteEvaluator      │
├────────────────────┼─────────────────────┼─────────────────────┤
│ 실행 위치          │ Ruler 프로세스 내    │ Query Frontend       │
│ 네트워크 비용      │ 없음                 │ gRPC 호출           │
│ 리소스 사용        │ Ruler에 집중         │ Frontend에 분산      │
│ 캐싱              │ 제한적               │ Frontend 캐시 활용   │
│ 샤딩              │ 불가                 │ Frontend 자동 샤딩  │
│ 복잡한 쿼리        │ 메모리 부담          │ 분산 처리 가능      │
│ 장애 격리          │ Ruler에 영향         │ Frontend로 격리     │
│ 설정              │ mode: local          │ mode: remote         │
│                    │                     │ + frontend address   │
└────────────────────┴─────────────────────┴─────────────────────┘
```

---

## 6. JitterEvaluator: 결정적 지터로 썬더링 허드 방지

소스 경로: `pkg/ruler/evaluator_jitter.go`

### 6.1 문제: 썬더링 허드(Thundering Herd)

여러 규칙이 동일한 `evaluation_interval`로 설정되어 있을 때, 모든 규칙이 동시에 평가되면 쿼리 엔진에 순간적인 부하가 집중된다.

```
시간  │ 지터 없음 (썬더링 허드)
──────┼──────────────────────
0:00  │ ████████████████████  (모든 규칙 동시 실행)
0:01  │
0:02  │ ████████████████████  (다시 동시 실행)
      │

시간  │ 결정적 지터 적용
──────┼──────────────────────
0:00  │ ████                  (규칙 A, B)
0:00.5│   ████                (규칙 C, D)
0:01  │      ████             (규칙 E, F)
0:01.5│         ████          (규칙 G, H)
      │
```

### 6.2 EvaluatorWithJitter 구조체

```go
// pkg/ruler/evaluator_jitter.go (line 23-30)
type EvaluatorWithJitter struct {
    mu sync.Mutex

    inner     Evaluator
    maxJitter time.Duration
    hasher    hash.Hash32
    logger    log.Logger
}
```

### 6.3 생성

```go
// pkg/ruler/evaluator_jitter.go (line 32-44)
func NewEvaluatorWithJitter(inner Evaluator, maxJitter time.Duration,
    hasher hash.Hash32, logger log.Logger) Evaluator {
    if maxJitter <= 0 {
        return inner  // 지터 비활성화 시 원본 Evaluator 반환
    }

    return &EvaluatorWithJitter{
        inner:     inner,
        maxJitter: maxJitter,
        hasher:    hasher,
        logger:    logger,
    }
}
```

### 6.4 결정적 지터 계산

```go
// pkg/ruler/evaluator_jitter.go (line 46-56)
func (e *EvaluatorWithJitter) Eval(ctx context.Context, qs string, now time.Time,
) (*logqlmodel.Result, error) {
    logger := log.With(e.logger, "query", qs, "query_hash", util.HashedQuery(qs))
    jitter := e.calculateJitter(qs, logger)

    if jitter > 0 {
        level.Debug(logger).Log("msg", "applying jitter", "jitter", jitter)
        time.Sleep(jitter)  // 결정적 지연
    }

    return e.inner.Eval(ctx, qs, now)
}

// pkg/ruler/evaluator_jitter.go (line 58-77)
func (e *EvaluatorWithJitter) calculateJitter(qs string, logger log.Logger,
) time.Duration {
    var h uint32

    e.mu.Lock()
    {
        _, err := e.hasher.Write([]byte(qs))
        if err != nil {
            level.Warn(logger).Log(
                "msg", "could not hash query to determine rule jitter",
                "err", err)
            return 0
        }
        h = e.hasher.Sum32()
        e.hasher.Reset()
    }
    e.mu.Unlock()

    // 해시값을 0~1 비율로 변환 → maxJitter에 곱셈
    ratio := float32(h) / math.MaxUint32
    return time.Duration(ratio * float32(e.maxJitter.Nanoseconds()))
}
```

### 6.5 결정적 지터의 특성

**"결정적"**이란 같은 쿼리 문자열이 항상 같은 지터 값을 생성한다는 의미다:

```
쿼리: "rate({app="api"} |= "error" [5m])"
  → Hash32: 0x7A3F2B1C
  → ratio: 0x7A3F2B1C / 0xFFFFFFFF = 0.478
  → jitter: 0.478 * maxJitter

쿼리: "count_over_time({app="web"} [1h])"
  → Hash32: 0x2D8E4F91
  → ratio: 0x2D8E4F91 / 0xFFFFFFFF = 0.178
  → jitter: 0.178 * maxJitter
```

이 결정적 특성이 중요한 이유:
1. **재현성**: 같은 규칙은 항상 같은 지터를 가져 예측 가능한 평가 스케줄
2. **안정성**: 규칙 재로드 시에도 지터 값이 변하지 않아 불필요한 부하 변동 방지
3. **분산**: 쿼리 문자열의 해시 분포가 균등하여 자연스러운 부하 분산

---

## 7. MultiTenantManager: 테넌트별 규칙 관리

소스 경로: `pkg/ruler/base/manager.go`

### 7.1 DefaultMultiTenantManager 구조체

```go
// pkg/ruler/base/manager.go (line 31-56)
type DefaultMultiTenantManager struct {
    cfg            Config
    notifiersCfg   map[string]*config.Config
    managerFactory ManagerFactory
    limits         RulesLimits

    mapper *mapper

    // 테넌트별 Prometheus Rules Manager
    userManagerMtx     sync.Mutex
    userManagers       map[string]RulesManager
    userManagerMetrics *ManagerMetrics

    // 테넌트별 Alertmanager Notifier
    notifiersMtx sync.Mutex
    notifiers    map[string]*rulerNotifier

    // 메트릭
    managersTotal                 prometheus.Gauge
    lastReloadSuccessful          *prometheus.GaugeVec
    lastReloadSuccessfulTimestamp *prometheus.GaugeVec
    configUpdatesTotal            *prometheus.CounterVec
    registry                      prometheus.Registerer
    logger                        log.Logger
}
```

### 7.2 SyncRuleGroups: 규칙 동기화

```go
// pkg/ruler/base/manager.go (line 109-135)
func (r *DefaultMultiTenantManager) SyncRuleGroups(ctx context.Context,
    ruleGroups map[string]rulespb.RuleGroupList) {
    r.userManagerMtx.Lock()
    defer r.userManagerMtx.Unlock()

    // 각 테넌트의 규칙 그룹 동기화
    for userID, ruleGroup := range ruleGroups {
        r.syncRulesToManager(ctx, userID, ruleGroup)
    }

    // 삭제된 테넌트 정리
    for userID, mngr := range r.userManagers {
        if _, exists := ruleGroups[userID]; !exists {
            go mngr.Stop()
            delete(r.userManagers, userID)

            r.mapper.cleanupUser(userID)
            r.lastReloadSuccessful.DeleteLabelValues(userID)
            r.lastReloadSuccessfulTimestamp.DeleteLabelValues(userID)
            r.configUpdatesTotal.DeleteLabelValues(userID)
            r.userManagerMetrics.RemoveUserRegistry(userID)
            level.Info(r.logger).Log("msg", "deleted rule manager", "user", userID)
        }
    }

    r.managersTotal.Set(float64(len(r.userManagers)))
}
```

### 7.3 syncRulesToManager: 변경 감지

```go
// pkg/ruler/base/manager.go (line 139-150+)
func (r *DefaultMultiTenantManager) syncRulesToManager(ctx context.Context,
    user string, groups rulespb.RuleGroupList) {
    // 규칙 파일을 디스크에 매핑
    update, files, err := r.mapper.MapRules(user, groups.Formatted())
    if err != nil {
        r.lastReloadSuccessful.WithLabelValues(user).Set(0)
        level.Error(r.logger).Log(
            "msg", "unable to map rule files", "user", user, "err", err)
        return
    }

    manager, exists := r.userManagers[user]
    if !exists || update {
        // Manager가 없거나 규칙이 변경되었으면 재로드
        // ...
    }
}
```

### 7.4 테넌트 생명주기

```
┌─────────────────────────────────────────────────────────┐
│                테넌트 규칙 생명주기                       │
│                                                         │
│  [1] RuleStore에서 규칙 로드                              │
│       │                                                 │
│       ▼                                                 │
│  [2] SyncRuleGroups(ruleGroups)                           │
│       │                                                 │
│       ├── 새 테넌트: Manager 생성 → Rules 매핑 → 시작    │
│       │                                                 │
│       ├── 기존 테넌트 (변경됨): Rules 매핑 → Manager 재로드│
│       │                                                 │
│       ├── 기존 테넌트 (변경 없음): 스킵                   │
│       │                                                 │
│       └── 삭제된 테넌트: Manager 중지 → 정리             │
│                                                         │
│  [3] Manager 실행:                                       │
│       ├── evaluation_interval 주기로 규칙 평가            │
│       ├── 알림 규칙: Alertmanager에 알림 전송             │
│       └── 녹화 규칙: Remote Write로 메트릭 전송           │
└─────────────────────────────────────────────────────────┘
```

---

## 8. 규칙 저장소: RuleStore

소스 경로: `pkg/ruler/rulestore/config.go`

### 8.1 RuleStore 설정

```go
// pkg/ruler/rulestore/config.go (line 16-20)
type Config struct {
    bucket.Config `yaml:",inline"`
    Backend       string       `yaml:"backend"`
    Local         local.Config `yaml:"local"`
}
```

### 8.2 지원 백엔드

| 백엔드 | 설명 | 사용 시나리오 |
|--------|------|--------------|
| `filesystem` | 로컬 파일 시스템 | 개발/테스트 |
| `s3` | AWS S3 | 프로덕션 (AWS) |
| `gcs` | Google Cloud Storage | 프로덕션 (GCP) |
| `azure` | Azure Blob Storage | 프로덕션 (Azure) |
| `swift` | OpenStack Swift | 프로덕션 (OpenStack) |

### 8.3 규칙 파일 구조

```yaml
# 규칙 그룹 예시
groups:
  - name: high-error-rate
    interval: 1m
    rules:
      # 알림 규칙
      - alert: HighErrorRate
        expr: |
          sum(rate({app="api"} |= "error" [5m])) > 100
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "High error rate detected"

      # 녹화 규칙
      - record: api:error_rate:5m
        expr: |
          sum(rate({app="api"} |= "error" [5m]))
```

### 8.4 규칙 저장소 로드 흐름

```
Ruler 시작
     │
     ▼
[1] PollInterval (기본 1분) 마다 규칙 폴링
     │
     ▼
[2] RuleStore.ListAllUsers()
     │  → 모든 테넌트 목록 조회
     │
     ▼
[3] Ring 기반 필터링
     │  → 이 Ruler가 담당하는 테넌트만 선택
     │
     ▼
[4] RuleStore.ListRuleGroupsForUserAndNamespace()
     │  → 테넌트별 규칙 그룹 로드
     │
     ▼
[5] MultiTenantManager.SyncRuleGroups()
     │  → 변경된 규칙만 Manager 재로드
```

---

## 9. 평가 루프: 알림 규칙 vs 녹화 규칙

### 9.1 base Ruler Config

소스 경로: `pkg/ruler/base/ruler.go`

```go
// pkg/ruler/base/ruler.go (line 79-124)
type Config struct {
    ExternalURL    flagext.URLValue `yaml:"external_url"`
    DatasourceUID  string           `yaml:"datasource_uid"`
    ExternalLabels labels.Labels    `yaml:"external_labels,omitempty"`

    // 핵심 타이밍 설정
    EvaluationInterval time.Duration `yaml:"evaluation_interval"` // 기본 1분
    PollInterval       time.Duration `yaml:"poll_interval"`       // 기본 1분

    // Alertmanager 설정
    AlertManagerConfig `yaml:",inline"`

    // "for" 상태 복구
    OutageTolerance time.Duration `yaml:"for_outage_tolerance"`  // 기본 1시간
    ForGracePeriod  time.Duration `yaml:"for_grace_period"`      // 기본 10분
    ResendDelay     time.Duration `yaml:"resend_delay"`          // 기본 1분

    // 분산 설정
    EnableSharding   bool   `yaml:"enable_sharding"`
    ShardingStrategy string `yaml:"sharding_strategy"`
    ShardingAlgo     string `yaml:"sharding_algo"`

    // Ring 설정
    Ring RingConfig `yaml:"ring"`

    EnableAPI bool `yaml:"enable_api"`
}
```

### 9.2 알림 규칙 (Alerting Rules)

알림 규칙의 실행 흐름:

```
evaluation_interval 도래
     │
     ▼
[1] Evaluator.Eval(ctx, expr, now)
     │  → LogQL 쿼리 실행
     │
     ▼
[2] 결과가 비어있지 않으면 → 알림 활성 상태
     │
     ├── [3a] "for" 기간 미경과: PENDING
     │
     └── [3b] "for" 기간 경과: FIRING
              │
              ▼
         [4] Alertmanager에 알림 전송
              │
              │  POST /api/v2/alerts
              │  {
              │    "labels": { ... },
              │    "annotations": { ... },
              │    "startsAt": "...",
              │    "endsAt": "..."
              │  }
```

### 9.3 녹화 규칙 (Recording Rules)

녹화 규칙은 LogQL 쿼리 결과를 Prometheus 메트릭으로 변환한다:

```
evaluation_interval 도래
     │
     ▼
[1] Evaluator.Eval(ctx, expr, now)
     │  → LogQL 쿼리 실행 (메트릭 쿼리)
     │
     ▼
[2] 결과를 Prometheus 메트릭으로 변환
     │  record 이름 = 메트릭 이름
     │  labels = 쿼리 결과의 레이블
     │  value = 쿼리 결과의 값
     │
     ▼
[3] WAL Registry → Appender
     │
     ▼
[4] Remote Write → 외부 Prometheus 또는 Mimir
```

### 9.4 "for" 상태 관리

```
┌──────────────────────────────────────────────────────┐
│             알림 상태 전이                            │
│                                                      │
│  INACTIVE ──(조건 충족)──► PENDING                    │
│     ▲                        │                       │
│     │                  (for 기간 경과)                │
│     │                        │                       │
│     │                        ▼                       │
│     │                    FIRING                      │
│     │                        │                       │
│     └──(조건 미충족)─────────┘                       │
│                                                      │
│  OutageTolerance: 장애 후 "for" 상태 복원 허용 시간   │
│  ForGracePeriod:  복원 시 최소 대기 시간              │
│  ResendDelay:     Alertmanager 재전송 간격            │
└──────────────────────────────────────────────────────┘
```

---

## 10. Alertmanager 연동

### 10.1 Alertmanager 설정

소스 경로: `pkg/ruler/base/ruler.go` (line 170-176)

```
-ruler.alertmanager-url       # Alertmanager URL (쉼표 구분)
-ruler.alertmanager-discovery # DNS SRV 디스커버리
-ruler.alertmanager-refresh-interval  # 디스커버리 갱신 주기
-ruler.alertmanager-use-v2   # Alertmanager V2 API 사용
-ruler.notification-queue-capacity    # 알림 큐 용량
-ruler.notification-timeout           # 알림 전송 타임아웃
```

### 10.2 테넌트별 Notifier

MultiTenantManager는 각 테넌트에 대해 별도의 Notifier를 관리한다:

```
┌───────────────────────────────────────────┐
│        DefaultMultiTenantManager          │
│                                           │
│  notifiers:                               │
│    "tenant-A" → rulerNotifier             │
│       └─► Prometheus Notifier             │
│           └─► POST Alertmanager           │
│               X-Scope-OrgID: tenant-A     │
│                                           │
│    "tenant-B" → rulerNotifier             │
│       └─► Prometheus Notifier             │
│           └─► POST Alertmanager           │
│               X-Scope-OrgID: tenant-B     │
│                                           │
│    "tenant-C" → rulerNotifier             │
│       └─► ...                             │
└───────────────────────────────────────────┘
```

### 10.3 알림 전송 포맷

Loki Ruler는 Alertmanager V2 API를 사용하여 알림을 전송한다:

```
POST /api/v2/alerts
Content-Type: application/json
X-Scope-OrgID: <tenant_id>

[
  {
    "status": "firing",
    "labels": {
      "alertname": "HighErrorRate",
      "severity": "critical",
      "rule_group": "high-error-rate"
    },
    "annotations": {
      "summary": "High error rate detected",
      "dashboard": "https://grafana.example.com/d/xxx"
    },
    "startsAt": "2026-03-07T10:00:00Z",
    "endsAt": "0001-01-01T00:00:00Z",
    "generatorURL": "https://loki.example.com/..."
  }
]
```

---

## 11. Ring 기반 분산

소스 경로: `pkg/ruler/base/ruler.go`

### 11.1 Ring 설정

```go
// pkg/ruler/base/ruler.go (line 108-113)
type Config struct {
    // ...
    EnableSharding   bool          `yaml:"enable_sharding"`
    ShardingStrategy string        `yaml:"sharding_strategy"` // default, shuffle
    ShardingAlgo     string        `yaml:"sharding_algo"`     // by-group, by-rule
    Ring             RingConfig    `yaml:"ring"`
}
```

### 11.2 샤딩 전략

| 전략 | 설명 |
|------|------|
| `default` | 해시링 기반 기본 분배 |
| `shuffle` | ShuffleShard 기반 테넌트 격리 |

### 11.3 샤딩 알고리즘

| 알고리즘 | 설명 |
|---------|------|
| `by-group` | 규칙 그룹 단위로 Ruler에 분배 |
| `by-rule` | 개별 규칙 단위로 Ruler에 분배 |

### 11.4 Ring 기반 규칙 분배

```
┌──────────────────────────────────────────────┐
│              Ruler Ring                      │
│                                              │
│  Ruler-0: [tenant-A/group-1, tenant-C/group-1]│
│  Ruler-1: [tenant-A/group-2, tenant-B/group-1]│
│  Ruler-2: [tenant-B/group-2, tenant-C/group-2]│
│                                              │
│  해시(tenant_id + "/" + group_name) → Ruler  │
└──────────────────────────────────────────────┘
```

### 11.5 규칙 동기화 트리거

```go
// 소스: pkg/ruler/base/ruler.go
const (
    rulerSyncReasonInitial    = "initial"     // Ruler 시작 시
    rulerSyncReasonPeriodic   = "periodic"    // PollInterval 주기
    rulerSyncReasonRingChange = "ring-change" // Ring 멤버십 변경 시
)
```

Ring 변경이 감지되면 규칙 재동기화가 자동으로 트리거된다. 이를 통해 Ruler 인스턴스의 추가/제거 시 규칙이 자동으로 재분배된다.

---

## 12. Remote Write와 WAL Registry

소스 경로: `pkg/ruler/registry.go`

### 12.1 walRegistry 구조체

```go
// pkg/ruler/registry.go (line 34-45)
type walRegistry struct {
    logger  log.Logger
    manager instance.Manager

    metrics     *storageRegistryMetrics
    overridesMu sync.Mutex

    config         Config
    overrides      RulesLimits
    lastUpdateTime time.Time
    cleaner        *cleaner.WALCleaner
}
```

### 12.2 Remote Write 설정

소스 경로: `pkg/ruler/config.go`

```go
// pkg/ruler/config.go (line 55-61)
type RemoteWriteConfig struct {
    Client              *config.RemoteWriteConfig
    Clients             map[string]config.RemoteWriteConfig
    Enabled             bool
    ConfigRefreshPeriod time.Duration
    AddOrgIDHeader      bool
}
```

### 12.3 WAL 기반 Remote Write 흐름

```
녹화 규칙 평가 결과
     │
     ▼
walRegistry.Appender(ctx)
     │
     ├── Remote Write 비활성화 → discardingAppender (버림)
     │
     ├── WAL 인스턴스 미준비 → notReadyAppender (에러)
     │
     └── 정상 → WAL Instance의 Appender
              │
              ▼
         WAL에 메트릭 기록
              │
              ▼
         Remote Write 전송
              │
              │  POST /api/v1/write
              │  X-Scope-OrgID: <tenant_id>
              │
              ▼
         Prometheus / Mimir 수신
```

### 12.4 테넌트별 Remote Write 구성

walRegistry는 테넌트별로 Remote Write 설정을 커스터마이즈할 수 있다:

```go
// pkg/ruler/registry.go (line 238-352)
func (r *walRegistry) getTenantRemoteWriteConfig(tenant string,
    base RemoteWriteConfig) (*RemoteWriteConfig, error) {
    overrides, err := base.Clone()
    // ...

    // 테넌트별 오버라이드 적용
    if r.overrides.RulerRemoteWriteDisabled(tenant) {
        overrides.Enabled = false
    }

    for id, clt := range overrides.Clients {
        clt.Name = fmt.Sprintf("%s-rw-%s", tenant, id)

        // URL 오버라이드
        if v := r.overrides.RulerRemoteWriteURL(tenant); v != "" { ... }
        // 타임아웃 오버라이드
        if v := r.overrides.RulerRemoteWriteTimeout(tenant); v > 0 { ... }
        // 헤더 오버라이드
        if v := r.overrides.RulerRemoteWriteHeaders(tenant); v != nil { ... }
        // Relabel 설정
        // Queue 설정 (Capacity, Shards, Backoff 등)
        // SigV4 설정
    }
    return overrides, nil
}
```

---

## 13. 메트릭과 모니터링

### 13.1 Ruler 핵심 메트릭

| 메트릭 이름 | 타입 | 설명 |
|-------------|------|------|
| `ruler_managers_total` | Gauge | 활성 Manager 수 |
| `ruler_config_last_reload_successful` | Gauge | 마지막 리로드 성공 여부 |
| `ruler_config_last_reload_successful_seconds` | Gauge | 마지막 리로드 시간 |
| `ruler_config_updates_total` | Counter | 설정 업데이트 횟수 |

### 13.2 Remote Evaluator 메트릭

소스 경로: `pkg/ruler/evaluator_remote.go` (line 93-143)

| 메트릭 이름 | 타입 | 레이블 | 설명 |
|-------------|------|--------|------|
| `loki_ruler_remote_eval_request_duration_seconds` | Histogram | user | 원격 평가 레이턴시 |
| `loki_ruler_remote_eval_response_bytes` | Histogram | user | 응답 크기 |
| `loki_ruler_remote_eval_response_samples` | Histogram | user | 응답 샘플 수 |
| `loki_ruler_remote_eval_success_total` | Counter | user | 성공한 평가 수 |
| `loki_ruler_remote_eval_failure_total` | Counter | reason, user | 실패한 평가 수 |

### 13.3 실패 이유별 분류

```
failedEvals 레이블 값:
  "timeout"        → 타임아웃 초과
  "error"          → gRPC 에러
  "upstream_error"  → Query Frontend 비2xx 응답
  "max_size"       → 응답 크기 제한 초과
```

### 13.4 모니터링 대시보드 핵심 쿼리

```
# 규칙 평가 실패율
rate(loki_ruler_remote_eval_failure_total[5m])
/ (rate(loki_ruler_remote_eval_success_total[5m]) +
   rate(loki_ruler_remote_eval_failure_total[5m]))

# 평가 레이턴시 P99
histogram_quantile(0.99,
  rate(loki_ruler_remote_eval_request_duration_seconds_bucket[5m]))

# Manager 수 변화 (테넌트 수 추적)
ruler_managers_total

# 리로드 실패 테넌트 탐지
ruler_config_last_reload_successful == 0
```

---

## 14. 설정 참조

### 14.1 핵심 Ruler 설정

```yaml
ruler:
  # 기본 설정
  evaluation_interval: 1m         # 규칙 평가 주기
  poll_interval: 1m               # 규칙 스토어 폴링 주기

  # 평가 모드
  evaluation:
    mode: local                    # local 또는 remote
    max_jitter: 0                  # 최대 지터 (0 = 비활성)
    query_frontend:
      address: ""                  # Query Frontend gRPC 주소

  # Alertmanager
  alertmanager_url: "http://alertmanager:9093"
  alertmanager_discovery: false
  alertmanager-use-v2: true

  # Ring (분산 모드)
  enable_sharding: true
  sharding_strategy: default       # default, shuffle
  sharding_algo: by-group          # by-group, by-rule
  ring:
    kvstore:
      store: memberlist

  # Remote Write
  remote_write:
    enabled: false
    clients:
      default:
        url: "http://mimir:9009/api/v1/push"

  # "for" 상태 관리
  for_outage_tolerance: 1h
  for_grace_period: 10m
  resend_delay: 1m

  # API
  enable_api: true

# 규칙 저장소
ruler_storage:
  backend: s3
  s3:
    bucket_name: loki-rules
    endpoint: s3.amazonaws.com
```

### 14.2 설정 조합 예시

```
┌─────────────────────────────────────────────────────┐
│  시나리오 1: 단일 Ruler (개발 환경)                   │
│                                                     │
│  evaluation.mode: local                             │
│  enable_sharding: false                             │
│  ruler_storage.backend: filesystem                  │
│  remote_write.enabled: false                        │
└─────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────┐
│  시나리오 2: 분산 Ruler (프로덕션)                    │
│                                                     │
│  evaluation.mode: remote                            │
│  evaluation.query_frontend.address:                 │
│    dns:///query-frontend:9095                        │
│  enable_sharding: true                              │
│  sharding_strategy: shuffle                         │
│  ruler_storage.backend: s3                          │
│  remote_write.enabled: true                         │
└─────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────┐
│  시나리오 3: 대규모 멀티테넌트                        │
│                                                     │
│  evaluation.mode: remote                            │
│  evaluation.max_jitter: 10s                         │
│  enable_sharding: true                              │
│  sharding_algo: by-rule                             │
│  ruler_storage.backend: gcs                         │
│  remote_write.enabled: true                         │
│  remote_write.add_org_id_header: true               │
└─────────────────────────────────────────────────────┘
```

---

## 정리

Loki Ruler는 Prometheus의 규칙 엔진을 기반으로 LogQL 쿼리의 주기적 평가를 구현한다. 핵심 설계 원칙은:

1. **평가 모드 분리**: `Evaluator` 인터페이스를 통해 Local과 Remote 평가를 투명하게 전환
2. **결정적 지터**: 쿼리 해시 기반의 일관된 지터로 썬더링 허드 문제 해결
3. **멀티테넌시**: 테넌트별 독립된 Rules Manager, Notifier, WAL 인스턴스
4. **Ring 기반 분산**: 규칙 그룹 또는 개별 규칙 단위의 샤딩으로 수평 확장
5. **테넌트별 커스터마이제이션**: Remote Write, 타임아웃, 레이트 리밋 등 테넌트 단위 오버라이드

| 구성요소 | 소스 경로 | 역할 |
|---------|----------|------|
| NewRuler | `pkg/ruler/ruler.go` | Ruler 인스턴스 생성 |
| Config | `pkg/ruler/config.go` | Ruler 설정 |
| Evaluator | `pkg/ruler/evaluator.go` | 평가 인터페이스 |
| LocalEvaluator | `pkg/ruler/evaluator_local.go` | LogQL 직접 실행 |
| RemoteEvaluator | `pkg/ruler/evaluator_remote.go` | Query Frontend 경유 |
| EvaluatorWithJitter | `pkg/ruler/evaluator_jitter.go` | 결정적 지터 적용 |
| base.Ruler | `pkg/ruler/base/ruler.go` | 핵심 Ruler 로직 |
| DefaultMultiTenantManager | `pkg/ruler/base/manager.go` | 테넌트별 Manager |
| walRegistry | `pkg/ruler/registry.go` | Remote Write WAL |
| RuleStore Config | `pkg/ruler/rulestore/config.go` | 규칙 저장소 설정 |
