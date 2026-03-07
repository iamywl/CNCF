# 샘플링 시스템

## 목차

1. [개요](#1-개요)
2. [왜 샘플링이 필요한가](#2-왜-샘플링이-필요한가)
3. [샘플링 아키텍처 전체 구조](#3-샘플링-아키텍처-전체-구조)
4. [파일 기반 샘플링 (File-based Sampling)](#4-파일-기반-샘플링-file-based-sampling)
5. [적응형 샘플링 (Adaptive Sampling)](#5-적응형-샘플링-adaptive-sampling)
6. [확률 계산 알고리즘](#6-확률-계산-알고리즘)
7. [리더 선출과 분산 조정](#7-리더-선출과-분산-조정)
8. [SDK에 전략 제공하기](#8-sdk에-전략-제공하기)
9. [Remote Sampling Extension](#9-remote-sampling-extension)
10. [설정 및 운영](#10-설정-및-운영)
11. [내부 구현 상세](#11-내부-구현-상세)
12. [정리](#12-정리)

---

## 1. 개요

분산 트레이싱 시스템에서 모든 요청을 추적하는 것은 비용적으로 현실적이지 않다. Jaeger의 샘플링 시스템은 **비용(cost)과 가시성(visibility) 사이의 균형**을 맞추기 위한 핵심 서브시스템으로, 어떤 트레이스를 수집하고 어떤 트레이스를 버릴지를 결정한다.

Jaeger는 두 가지 샘플링 모드를 제공한다:

| 모드 | 설명 | 적합한 상황 |
|------|------|------------|
| **파일 기반(File-based)** | 정적 JSON 설정 파일에서 확률/속도 제한 전략 로드 | 예측 가능한 트래픽, 간단한 환경 |
| **적응형(Adaptive)** | 실시간 처리량을 관찰하여 자동으로 확률 조정 | 가변 트래픽, 대규모 마이크로서비스 |

핵심 소스 파일:

```
internal/sampling/samplingstrategy/
├── provider.go                              # Provider 인터페이스 정의
├── file/
│   ├── provider.go                          # 파일 기반 Provider 구현
│   ├── strategy.go                          # JSON 전략 구조체 정의
│   ├── options.go                           # 설정 옵션
│   └── constants.go                         # 기본값, 상수
└── adaptive/
    ├── provider.go                          # 적응형 Provider 구현
    ├── aggregator.go                        # 처리량 수집기
    ├── post_aggregator.go                   # 확률 계산 엔진
    ├── options.go                           # 적응형 옵션
    ├── weightvectorcache.go                 # 가중치 벡터 캐시
    ├── cache.go                             # 서비스-오퍼레이션 캐시
    └── calculationstrategy/
        └── percentage_increase_capped_calculator.go  # 증가 제한 계산기
```

---

## 2. 왜 샘플링이 필요한가

### 2.1 비용 vs 가시성 트레이드오프

대규모 마이크로서비스 환경에서 100% 샘플링의 문제:

```
┌──────────────────────────────────────────────────────┐
│                비용 (100% 샘플링)                      │
├──────────────────────────────────────────────────────┤
│  네트워크 대역폭:  스팬 전송 트래픽 폭증               │
│  CPU/메모리:       Collector 리소스 과부하             │
│  스토리지:         TB 단위 데이터 저장 비용             │
│  쿼리 성능:        대량 데이터로 검색 속도 저하          │
└──────────────────────────────────────────────────────┘

┌──────────────────────────────────────────────────────┐
│              가시성 (0.1% 샘플링)                      │
├──────────────────────────────────────────────────────┤
│  희귀 에러:       발견 확률 극히 낮음                   │
│  꼬리 지연:       P99 문제 놓칠 가능성                  │
│  새 서비스:       초기 트레이스 부족으로 디버깅 불가      │
│  저 트래픽 API:   샘플이 전혀 없을 수 있음              │
└──────────────────────────────────────────────────────┘
```

### 2.2 샘플링 결정 지점

Jaeger 트레이싱에서 샘플링 결정은 **루트 스팬(root span)**을 생성하는 시점에 이루어진다. 하위 스팬은 부모의 결정을 따른다. 이것을 **head-based sampling**이라 한다.

```
요청 수신 (root span 생성)
    │
    ▼
샘플링 결정: 이 트레이스를 추적할 것인가?
    │
    ├── YES → 모든 하위 스팬도 추적
    │
    └── NO  → 전체 트레이스 버림
```

SDK(클라이언트)는 주기적으로 Jaeger 백엔드에 자신의 서비스에 대한 샘플링 전략을 질의한다. 백엔드는 서비스/오퍼레이션별로 적절한 전략(확률 또는 속도 제한)을 반환한다.

---

## 3. 샘플링 아키텍처 전체 구조

```
┌─────────────────────────────────────────────────────────────────┐
│                        Jaeger Backend                           │
│                                                                 │
│  ┌─────────────────────────────────────────────────────────┐    │
│  │           Remote Sampling Extension                      │    │
│  │                                                         │    │
│  │  ┌──────────────────┐   ┌───────────────────────────┐   │    │
│  │  │  File Provider   │   │   Adaptive Provider       │   │    │
│  │  │                  │   │                           │   │    │
│  │  │  JSON 파일/URL   │   │  Aggregator              │   │    │
│  │  │  에서 전략 로드   │   │    ↓                      │   │    │
│  │  │  주기적 리로드    │   │  PostAggregator          │   │    │
│  │  │                  │   │    ↓                      │   │    │
│  │  │  atomic.Value로  │   │  SamplingStore           │   │    │
│  │  │  스레드 안전 교체 │   │    (확률 영속화)          │   │    │
│  │  └──────────────────┘   └───────────────────────────┘   │    │
│  │                                                         │    │
│  │  ┌────────────────────────────────────────────────┐     │    │
│  │  │        전략 제공 엔드포인트                       │     │    │
│  │  │  HTTP: GET /?service={name}  (port 5778)       │     │    │
│  │  │  gRPC: SamplingManager.GetSamplingStrategy()   │     │    │
│  │  └────────────────────────────────────────────────┘     │    │
│  └─────────────────────────────────────────────────────────┘    │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
          ▲                    ▲                    ▲
          │                    │                    │
     ┌────┴────┐          ┌───┴────┐          ┌───┴────┐
     │ SDK #1  │          │ SDK #2 │          │ SDK #3 │
     │ (폴링)  │          │ (폴링) │          │ (폴링) │
     └─────────┘          └────────┘          └────────┘
```

### 3.1 Provider 인터페이스

모든 샘플링 전략 제공자는 `samplingstrategy.Provider` 인터페이스를 구현한다.

**파일 경로**: `internal/sampling/samplingstrategy/provider.go`

```go
// Provider keeps track of service specific sampling strategies.
type Provider interface {
    // Close() from io.Closer stops the processor from calculating probabilities.
    io.Closer

    // GetSamplingStrategy retrieves the sampling strategy for the specified service.
    GetSamplingStrategy(ctx context.Context, serviceName string) (
        *api_v2.SamplingStrategyResponse, error,
    )
}
```

이 인터페이스는 의도적으로 단순하게 설계되었다. 서비스 이름을 입력으로 받아 해당 서비스에 적용할 샘플링 전략을 반환한다. 파일 기반이든 적응형이든 이 동일한 인터페이스를 통해 교체 가능하다.

---

## 4. 파일 기반 샘플링 (File-based Sampling)

### 4.1 개요

파일 기반 샘플링은 JSON 형식의 설정 파일에서 정적 전략을 로드한다. 가장 간단한 방식이며, 트래픽 패턴이 예측 가능한 환경에서 적합하다.

**핵심 파일**: `internal/sampling/samplingstrategy/file/provider.go`

### 4.2 Provider 구조체

```go
// provider.go
type samplingProvider struct {
    logger *zap.Logger

    storedStrategies atomic.Value // holds *storedStrategies

    cancelFunc context.CancelFunc

    options Options
}

type storedStrategies struct {
    defaultStrategy   *api_v2.SamplingStrategyResponse
    serviceStrategies map[string]*api_v2.SamplingStrategyResponse
}
```

**왜 `atomic.Value`를 사용하는가?**

`storedStrategies`는 `atomic.Value`에 저장된다. 이는 전략이 업데이트될 때 락 없이 안전하게 읽기/쓰기가 가능하게 한다. 전략을 질의하는 gRPC/HTTP 핸들러는 고빈도로 호출되므로, `sync.RWMutex` 대신 `atomic.Value`로 락-프리(lock-free) 읽기를 보장한다.

```
읽기 경로 (고빈도):
  GetSamplingStrategy() → storedStrategies.Load() → 락 없이 즉시 반환

쓰기 경로 (저빈도):
  parseStrategies() → 새 storedStrategies 생성 → Store()로 원자적 교체
```

### 4.3 초기화 흐름

```go
// provider.go - NewProvider
func NewProvider(options Options, logger *zap.Logger) (samplingstrategy.Provider, error) {
    ctx, cancelFunc := context.WithCancel(context.Background())
    h := &samplingProvider{
        logger:     logger,
        cancelFunc: cancelFunc,
        options:    options,
    }
    h.storedStrategies.Store(defaultStrategies(options.DefaultSamplingProbability))

    if options.StrategiesFile == "" {
        h.logger.Info("No sampling strategies source provided, using defaults")
        return h, nil
    }

    loadFn := h.samplingStrategyLoader(options.StrategiesFile)
    strategies, err := loadStrategies(loadFn)
    if err != nil {
        return nil, err
    } else if strategies == nil {
        h.logger.Info("No sampling strategies found or URL is unavailable, using defaults")
        return h, nil
    }
    h.parseStrategies(strategies)
    if options.ReloadInterval > 0 {
        go h.autoUpdateStrategies(ctx, loadFn)
    }
    return h, nil
}
```

초기화 순서:

```
NewProvider()
  │
  ├─ 1. 기본 전략으로 초기화 (DefaultSamplingProbability)
  │
  ├─ 2. StrategiesFile이 비어 있으면 기본값으로 반환
  │
  ├─ 3. 전략 로더 함수 결정 (파일 경로 vs HTTP URL)
  │     ├─ isURL() → HTTP에서 다운로드
  │     └─ 파일 → os.ReadFile()
  │
  ├─ 4. 전략 로드 및 파싱
  │
  └─ 5. ReloadInterval > 0이면 자동 업데이트 고루틴 시작
```

### 4.4 전략 타입

**파일 경로**: `internal/sampling/samplingstrategy/file/strategy.go`

```go
type strategy struct {
    Type  string  `json:"type"`
    Param float64 `json:"param"`
}

type operationStrategy struct {
    Operation string `json:"operation"`
    strategy
}

type serviceStrategy struct {
    Service             string               `json:"service"`
    OperationStrategies []*operationStrategy `json:"operation_strategies"`
    strategy
}

type strategies struct {
    DefaultStrategy   *serviceStrategy   `json:"default_strategy"`
    ServiceStrategies []*serviceStrategy `json:"service_strategies"`
}
```

두 가지 전략 타입이 지원된다:

| 타입 | Type 문자열 | Param 의미 | 범위 | 예시 |
|------|------------|-----------|------|------|
| **확률적(Probabilistic)** | `"probabilistic"` | 샘플링 확률 | 0.0 ~ 1.0 | 0.01 = 1% 샘플링 |
| **속도 제한(Rate Limiting)** | `"ratelimiting"` | 초당 최대 트레이스 수 | 양수 | 2 = 초당 최대 2개 |

**파일 경로**: `internal/sampling/samplingstrategy/file/constants.go`

```go
const (
    samplerTypeProbabilistic = "probabilistic"
    samplerTypeRateLimiting  = "ratelimiting"

    // DefaultSamplingProbability: 설정이 없을 때 기본값
    DefaultSamplingProbability = 0.001  // 0.1%
)
```

### 4.5 JSON 설정 형식

```json
{
  "default_strategy": {
    "type": "probabilistic",
    "param": 0.01,
    "operation_strategies": [
      {
        "operation": "health-check",
        "type": "probabilistic",
        "param": 0.0
      }
    ]
  },
  "service_strategies": [
    {
      "service": "payment-service",
      "type": "probabilistic",
      "param": 0.5,
      "operation_strategies": [
        {
          "operation": "processPayment",
          "type": "probabilistic",
          "param": 1.0
        },
        {
          "operation": "listTransactions",
          "type": "probabilistic",
          "param": 0.1
        }
      ]
    },
    {
      "service": "notification-service",
      "type": "ratelimiting",
      "param": 5
    }
  ]
}
```

계층 구조:

```
strategies
├── default_strategy          ← 미등록 서비스에 적용
│   ├── type + param          ← 서비스 기본 전략
│   └── operation_strategies  ← 오퍼레이션별 오버라이드
│       └── operation + type + param
│
└── service_strategies[]      ← 서비스별 전략
    ├── service               ← 서비스 이름
    ├── type + param          ← 서비스 기본 전략
    └── operation_strategies  ← 오퍼레이션별 오버라이드
        └── operation + type + param
```

### 4.6 전략 우선순위 및 병합

`parseStrategies()`는 다음 우선순위로 전략을 결정한다:

```
우선순위 (높음 → 낮음):
1. service_strategies[svc].operation_strategies[op]  ← 서비스+오퍼레이션 지정
2. service_strategies[svc].type/param                ← 서비스 기본
3. default_strategy.operation_strategies[op]          ← 전역 오퍼레이션 지정
4. default_strategy.type/param                        ← 전역 기본
5. DefaultSamplingProbability (0.001)                  ← 하드코딩된 기본값
```

병합 로직의 핵심은 서비스별 전략에 오퍼레이션 전략이 없을 때, default_strategy의 오퍼레이션 전략을 상속하는 것이다:

```go
// provider.go - parseStrategies (핵심 병합 로직)
if newStore.defaultStrategy.OperationSampling == nil {
    continue  // 기본 전략에도 오퍼레이션 전략이 없으면 건너뜀
}

opS := newStore.serviceStrategies[s.Service].OperationSampling
if opS == nil {
    // 서비스에 오퍼레이션 전략이 없으면 기본 전략에서 복사
    newOpS := *newStore.defaultStrategy.OperationSampling
    if newStore.serviceStrategies[s.Service].ProbabilisticSampling != nil {
        newOpS.DefaultSamplingProbability =
            newStore.serviceStrategies[s.Service].ProbabilisticSampling.SamplingRate
    }
    newStore.serviceStrategies[s.Service].OperationSampling = &newOpS
    continue
}

// 서비스에 자체 오퍼레이션 전략이 있으면 기본 전략과 병합
opS.PerOperationStrategies = mergePerOperationSamplingStrategies(
    opS.PerOperationStrategies,
    newStore.defaultStrategy.OperationSampling.PerOperationStrategies)
```

`mergePerOperationSamplingStrategies` 함수는 서비스 전략(a)이 기본 전략(b)보다 우선하도록 합집합을 만든다:

```go
func mergePerOperationSamplingStrategies(
    a, b []*api_v2.OperationSamplingStrategy,
) []*api_v2.OperationSamplingStrategy {
    m := make(map[string]bool)
    for _, aOp := range a {
        m[aOp.Operation] = true
    }
    for _, bOp := range b {
        if m[bOp.Operation] {
            continue  // 서비스 전략에 이미 있으면 건너뜀
        }
        a = append(a, bOp)  // 없으면 기본 전략에서 가져옴
    }
    return a
}
```

### 4.7 자동 리로드

`ReloadInterval`이 0보다 크면, 백그라운드 고루틴이 주기적으로 전략을 다시 로드한다.

```go
// provider.go - autoUpdateStrategies
func (h *samplingProvider) autoUpdateStrategies(ctx context.Context, loader strategyLoader) {
    lastValue := string(nullJSON)
    ticker := time.NewTicker(h.options.ReloadInterval)
    defer ticker.Stop()
    for {
        select {
        case <-ticker.C:
            lastValue = h.reloadSamplingStrategy(loader, lastValue)
        case <-ctx.Done():
            return
        }
    }
}
```

최적화 포인트: **변경 감지**

```go
func (h *samplingProvider) reloadSamplingStrategy(loadFn strategyLoader, lastValue string) string {
    newValue, err := loadFn()
    if err != nil {
        h.logger.Error("failed to re-load sampling strategies", zap.Error(err))
        return lastValue
    }
    if lastValue == string(newValue) {
        return lastValue  // 내용이 같으면 파싱 건너뜀
    }
    // ... 파싱 및 업데이트
}
```

이전 값과 비교하여 변경이 없으면 JSON 파싱을 건너뛴다. 이는 설정 파일이 자주 폴링되지만 실제 변경은 드문 경우 CPU를 절약한다.

### 4.8 로딩 소스: 파일 vs HTTP URL

```go
// provider.go - samplingStrategyLoader
func (h *samplingProvider) samplingStrategyLoader(strategiesFile string) strategyLoader {
    if isURL(strategiesFile) {
        return func() ([]byte, error) {
            return h.downloadSamplingStrategies(strategiesFile)
        }
    }
    return func() ([]byte, error) {
        currBytes, err := os.ReadFile(filepath.Clean(strategiesFile))
        if err != nil {
            return nil, fmt.Errorf("failed to read strategies file %s: %w",
                strategiesFile, err)
        }
        return currBytes, nil
    }
}

func isURL(str string) bool {
    u, err := url.Parse(str)
    return err == nil && u.Scheme != "" && u.Host != ""
}
```

HTTP URL에서 다운로드할 때는 1초 타임아웃이 적용되며, 503(Service Unavailable) 응답은 `null`로 처리되어 기본 전략이 유지된다.

```
전략 소스 결정:
  StrategiesFile
    │
    ├─ "http://..." or "https://..." → HTTP 다운로드 (1초 타임아웃)
    │   ├─ 200 OK → JSON 파싱
    │   ├─ 503    → null (기본 전략 유지)
    │   └─ 기타   → 에러 반환
    │
    └─ "/path/to/file.json" → os.ReadFile()
```

### 4.9 Options 구조체

**파일 경로**: `internal/sampling/samplingstrategy/file/options.go`

```go
type Options struct {
    // StrategiesFile은 JSON 형식 샘플링 전략 파일 경로
    StrategiesFile string
    // ReloadInterval은 전략 파일 재확인 주기
    ReloadInterval time.Duration
    // DefaultSamplingProbability는 정적 샘플링에 사용되는 기본 확률
    DefaultSamplingProbability float64
}
```

---

## 5. 적응형 샘플링 (Adaptive Sampling)

### 5.1 개요

적응형 샘플링은 각 서비스/오퍼레이션의 **실시간 처리량(throughput)**을 관찰하고, **목표 초당 샘플 수(target QPS)**에 맞도록 샘플링 확률을 자동으로 조정한다.

**왜 적응형 샘플링이 필요한가?**

```
고정 확률 1%의 문제:
  서비스 A (10,000 req/s) → 100 samples/s  ← 충분
  서비스 B (1 req/s)      → 0.01 samples/s ← 거의 샘플 없음!
  서비스 C (100,000 req/s) → 1,000 samples/s ← 과도한 샘플

적응형 샘플링 (목표: 1 sample/s):
  서비스 A (10,000 req/s) → 확률 0.0001 → ~1 sample/s
  서비스 B (1 req/s)      → 확률 1.0    → ~1 sample/s
  서비스 C (100,000 req/s) → 확률 0.00001 → ~1 sample/s
```

### 5.2 3계층 아키텍처

적응형 샘플링은 세 개의 핵심 컴포넌트로 구성된다:

```
┌───────────────────────────────────────────────────────┐
│                   Aggregator                          │
│  (처리량 수집: 루트 스팬 관찰 → 서비스/오퍼레이션별 카운트) │
│                                                       │
│  HandleRootSpan() → RecordThroughput()               │
│  CalculationInterval마다 → saveThroughput() → storage │
└───────────────────────┬───────────────────────────────┘
                        │ 매 CalculationInterval
                        ▼
┌───────────────────────────────────────────────────────┐
│                 PostAggregator                         │
│  (확률 계산 엔진: 처리량 → QPS → 가중 평균 → 새 확률)   │
│                                                       │
│  runCalculation()                                     │
│    → GetThroughput() from storage                     │
│    → throughputToQPS()                                │
│    → calculateWeightedQPS()                           │
│    → calculateProbability()                           │
│    → saveProbabilitiesAndQPS() to storage             │
└───────────────────────┬───────────────────────────────┘
                        │ 확률 저장
                        ▼
┌───────────────────────────────────────────────────────┐
│                    Provider                           │
│  (전략 제공: 저장된 확률 → SamplingStrategyResponse)    │
│                                                       │
│  loadProbabilities() from storage                     │
│  generateStrategyResponses()                          │
│  GetSamplingStrategy() → 캐시된 응답 반환              │
└───────────────────────────────────────────────────────┘
```

### 5.3 Aggregator: 처리량 수집

**파일 경로**: `internal/sampling/samplingstrategy/adaptive/aggregator.go`

```go
type aggregator struct {
    sync.Mutex

    operationsCounter   metrics.Counter
    servicesCounter     metrics.Counter
    currentThroughput   serviceOperationThroughput   // service → operation → Throughput
    postAggregator      *PostAggregator
    aggregationInterval time.Duration
    storage             samplingstore.Store
    stop                chan struct{}
    bgFinished          sync.WaitGroup
}
```

#### 루트 스팬 처리

```go
func (a *aggregator) HandleRootSpan(span *spanmodel.Span) {
    // parentId가 0인 스팬(루트 스팬)만 처리
    if span.ParentSpanID() != spanmodel.NewSpanID(0) {
        return
    }
    service := span.Process.ServiceName
    if service == "" || span.OperationName == "" {
        return
    }
    samplerType, samplerParam := getSamplerParams(span, a.postAggregator.logger)
    if samplerType == spanmodel.SamplerTypeUnrecognized {
        return
    }
    a.RecordThroughput(service, span.OperationName, samplerType, samplerParam)
}
```

**왜 루트 스팬만 처리하는가?**

샘플링 결정은 루트 스팬에서만 이루어진다. 하위 스팬은 부모의 결정을 따르므로, 처리량 측정에 포함하면 중복 카운트가 발생한다. 루트 스팬의 `sampler.type`과 `sampler.param` 태그를 확인하여 해당 트레이스가 어떤 샘플링 전략으로 수집되었는지 파악한다.

#### 처리량 기록

```go
func (a *aggregator) RecordThroughput(service, operation string,
    samplerType spanmodel.SamplerType, probability float64) {
    a.Lock()
    defer a.Unlock()
    // ... 서비스/오퍼레이션별 Throughput 구조체 생성/갱신 ...

    // 확률적(probabilistic) 샘플링으로 수집된 스팬만 카운트 증가
    if samplerType == spanmodel.SamplerTypeProbabilistic {
        throughput.Count++
    }
}
```

**왜 probabilistic 타입만 카운트하는가?**

확률적 샘플링은 `count / probability`로 실제 QPS를 추정할 수 있다. 하한(lowerbound) 샘플링은 저 트래픽 API를 위한 보완 매커니즘이므로, 실제 처리량 추정에 사용하면 왜곡이 발생한다. count=0이더라도 throughput 구조체를 생성하여 PostAggregator에 해당 엔드포인트의 존재를 알린다.

#### 주기적 집계 루프

```go
func (a *aggregator) runAggregationLoop() {
    ticker := time.NewTicker(a.aggregationInterval)
    for {
        select {
        case <-ticker.C:
            a.Lock()
            a.saveThroughput()                           // 현재 처리량을 storage에 저장
            a.currentThroughput = make(serviceOperationThroughput)  // 리셋
            a.postAggregator.runCalculation()             // 확률 재계산 트리거
            a.Unlock()
        case <-a.stop:
            ticker.Stop()
            return
        }
    }
}
```

### 5.4 PostAggregator: 확률 계산 엔진

**파일 경로**: `internal/sampling/samplingstrategy/adaptive/post_aggregator.go`

PostAggregator는 적응형 샘플링의 **두뇌**이다. 과거 처리량 데이터를 기반으로 각 오퍼레이션의 샘플링 확률을 계산한다.

```go
type PostAggregator struct {
    Options

    mu                  sync.RWMutex
    electionParticipant leaderelection.ElectionParticipant
    storage             samplingstore.Store
    logger              *zap.Logger
    hostname            string

    probabilities model.ServiceOperationProbabilities  // 최신 확률
    qps           model.ServiceOperationQPS            // 최신 QPS

    throughputs []*throughputBucket                     // 슬라이딩 윈도우

    weightVectorCache     *WeightVectorCache
    probabilityCalculator calculationstrategy.ProbabilityCalculator

    serviceCache []SamplingCache                       // 서비스 캐시 (25개)

    // ... 메트릭, 타이머 등
}
```

#### 슬라이딩 윈도우

```go
type throughputBucket struct {
    throughput serviceOperationThroughput
    interval   time.Duration
    endTime    time.Time
}
```

처리량 데이터는 슬라이딩 윈도우에 저장된다:

```
기본 설정 (AggregationBuckets=10, CalculationInterval=1min):

throughputs[]:
┌───────┬───────┬───────┬───────┬───────┬─────┬───────┐
│ t-2m  │ t-3m  │ t-4m  │ t-5m  │ t-6m  │ ... │ t-12m │
│ 최신  │       │       │       │       │     │ 최고  │
└───────┴───────┴───────┴───────┴───────┴─────┴───────┘
  ▲ 새 버킷은 앞에 추가 (prepend)
```

**왜 2분 딜레이(Delay)?**

```
시간선:
  now-12m ─── now-2m ─── now
  ←── 처리량 데이터 범위 ──→    ← 딜레이 →
                                 (2분)
```

SDK 클라이언트는 1분 간격으로 샘플링 전략을 폴링한다. 딜레이를 2분으로 설정하면, 모든 클라이언트가 새 확률을 적용한 후의 처리량 데이터만 사용하여 계산한다. 이렇게 하면 "확률 변경 → 처리량 변화 관찰" 사이의 피드백 루프가 안정적으로 동작한다.

### 5.5 Options (기본값)

**파일 경로**: `internal/sampling/samplingstrategy/adaptive/options.go`

```go
func DefaultOptions() Options {
    return Options{
        TargetSamplesPerSecond:       1,
        DeltaTolerance:               0.3,        // +-30%
        BucketsForCalculation:        1,
        CalculationInterval:          time.Minute, // 1분
        AggregationBuckets:           10,          // 10개 버킷
        Delay:                        time.Minute * 2,
        InitialSamplingProbability:   0.001,       // 0.1%
        MinSamplingProbability:       1e-5,        // 0.001%
        MinSamplesPerSecond:          1.0 / 60.0,  // 분당 1개
        LeaderLeaseRefreshInterval:   5 * time.Second,
        FollowerLeaseRefreshInterval: 60 * time.Second,
    }
}
```

| 옵션 | 기본값 | 의미 |
|------|--------|------|
| `TargetSamplesPerSecond` | 1 | 오퍼레이션당 목표 초당 샘플 수 |
| `DeltaTolerance` | 0.3 | 실제 QPS가 목표의 +-30% 이내면 조정 안 함 |
| `CalculationInterval` | 1분 | 확률 재계산 주기 |
| `AggregationBuckets` | 10 | 메모리에 유지할 과거 버킷 수 |
| `BucketsForCalculation` | 1 | QPS 계산에 사용할 버킷 수 |
| `Delay` | 2분 | 처리량 수집 시작 시점 지연 |
| `InitialSamplingProbability` | 0.001 | 새 오퍼레이션의 초기 확률 |
| `MinSamplingProbability` | 1e-5 | 최소 확률 (10만 요청당 1개) |
| `MinSamplesPerSecond` | 1/60 | 하한 샘플링 (분당 최소 1개) |
| `LeaderLeaseRefreshInterval` | 5초 | 리더 잠금 갱신 주기 |
| `FollowerLeaseRefreshInterval` | 60초 | 팔로워 잠금 시도 주기 |

---

## 6. 확률 계산 알고리즘

### 6.1 전체 계산 흐름

```
runCalculation()
  │
  ├─ 1. storage에서 처리량 데이터 로드
  │     GetThroughput(startTime, endTime)
  │
  ├─ 2. 처리량 집계 (여러 컬렉터의 데이터 병합)
  │     aggregateThroughput()
  │
  ├─ 3. 슬라이딩 윈도우에 새 버킷 추가
  │     prependThroughputBucket()
  │
  ├─ 4. 리더인 경우에만 확률 계산
  │     calculateProbabilitiesAndQPS()
  │       │
  │       ├─ throughputToQPS()          → 처리량 → QPS 변환
  │       ├─ calculateWeightedQPS()     → 가중 평균 QPS 계산
  │       └─ calculateProbability()     → 새 확률 계산
  │
  └─ 5. storage에 확률/QPS 저장
        saveProbabilitiesAndQPS()
```

### 6.2 처리량 → QPS 변환

```go
func (p *PostAggregator) throughputToQPS() serviceOperationQPS {
    qps := make(serviceOperationQPS)
    for _, bucket := range p.throughputs {
        for svc, operations := range bucket.throughput {
            if _, ok := qps[svc]; !ok {
                qps[svc] = make(map[string][]float64)
            }
            for op, throughput := range operations {
                if len(qps[svc][op]) >= p.BucketsForCalculation {
                    continue
                }
                qps[svc][op] = append(qps[svc][op],
                    calculateQPS(throughput.Count, bucket.interval))
            }
        }
    }
    return qps
}

func calculateQPS(count int64, interval time.Duration) float64 {
    seconds := float64(interval) / float64(time.Second)
    return float64(count) / seconds
}
```

### 6.3 가중 QPS 계산

최근 데이터에 더 높은 가중치를 부여하여 QPS를 계산한다.

**파일 경로**: `internal/sampling/samplingstrategy/adaptive/weightvectorcache.go`

```go
// GetWeights returns weights for the specified length { w(i) = i ^ 4, i=1..L }, normalized.
func (c *WeightVectorCache) GetWeights(length int) []float64 {
    // ...
    weights := make([]float64, 0, length)
    var sum float64
    for i := length; i > 0; i-- {
        w := math.Pow(float64(i), 4)   // i^4 가중치
        weights = append(weights, w)
        sum += w
    }
    // 정규화
    for i := range length {
        weights[i] /= sum
    }
    // ...
}
```

**왜 i^4 가중치를 사용하는가?**

```
버킷 수가 3일 때 가중치 (정규화 전):

  최신 버킷 (i=3): 3^4 = 81
  중간 버킷 (i=2): 2^4 = 16
  오래된 버킷 (i=1): 1^4 = 1

  정규화 후:
  최신 = 81/98 = 0.827 (82.7%)
  중간 = 16/98 = 0.163 (16.3%)
  오래 = 1/98  = 0.010 (1.0%)
```

4제곱 가중치는 최신 데이터에 매우 강한 편향을 주어, 트래픽 변화에 빠르게 반응하면서도 이전 데이터를 완전히 무시하지는 않는 균형을 제공한다.

```go
func (p *PostAggregator) calculateWeightedQPS(allQPS []float64) float64 {
    if len(allQPS) == 0 {
        return 0
    }
    weights := p.weightVectorCache.GetWeights(len(allQPS))
    var qps float64
    for i := range allQPS {
        qps += allQPS[i] * weights[i]
    }
    return qps
}
```

### 6.4 확률 계산

```go
func (p *PostAggregator) calculateProbability(service, operation string, qps float64) float64 {
    oldProbability := p.InitialSamplingProbability
    // 기존 확률이 있으면 가져오기
    if opProbabilities, ok := p.probabilities[service]; ok {
        if probability, ok := opProbabilities[operation]; ok {
            oldProbability = probability
        }
    }
    latestThroughput := p.throughputs[0].throughput

    usingAdaptiveSampling := p.isUsingAdaptiveSampling(
        oldProbability, service, operation, latestThroughput)
    p.serviceCache[0].Set(service, operation, &SamplingCacheEntry{
        Probability:   oldProbability,
        UsingAdaptive: usingAdaptiveSampling,
    })

    // 1. 목표 QPS에 충분히 가까우면 조정하지 않음 (DeltaTolerance)
    // 2. 적응형 샘플링을 사용하지 않는 서비스도 건너뜀
    if p.withinTolerance(qps, p.TargetSamplesPerSecond) || !usingAdaptiveSampling {
        return oldProbability
    }

    var newProbability float64
    if FloatEquals(qps, 0) {
        // QPS가 0이면 확률을 2배로 증가 (최소 1개 샘플 확보)
        newProbability = oldProbability * 2.0
    } else {
        // 핵심 공식: PercentageIncreaseCappedCalculator 사용
        newProbability = p.probabilityCalculator.Calculate(
            p.TargetSamplesPerSecond, qps, oldProbability)
    }
    return math.Min(maxSamplingProbability,
        math.Max(p.MinSamplingProbability, newProbability))
}
```

#### DeltaTolerance (허용 범위)

```go
func (p *PostAggregator) withinTolerance(actual, expected float64) bool {
    return math.Abs(actual-expected)/expected < p.DeltaTolerance
}
```

기본 DeltaTolerance는 0.3이므로, 실제 QPS가 목표의 +-30% 이내이면 확률을 조정하지 않는다. 이는 불필요한 진동(oscillation)을 방지한다.

```
목표 QPS = 1.0, DeltaTolerance = 0.3

  실제 QPS = 0.8 → |0.8-1.0|/1.0 = 0.2 < 0.3 → 조정 안 함
  실제 QPS = 1.2 → |1.2-1.0|/1.0 = 0.2 < 0.3 → 조정 안 함
  실제 QPS = 1.5 → |1.5-1.0|/1.0 = 0.5 > 0.3 → 확률 낮춤
  실제 QPS = 0.5 → |0.5-1.0|/1.0 = 0.5 > 0.3 → 확률 높임
```

### 6.5 PercentageIncreaseCappedCalculator

**파일 경로**: `internal/sampling/samplingstrategy/adaptive/calculationstrategy/percentage_increase_capped_calculator.go`

```go
type PercentageIncreaseCappedCalculator struct {
    percentageIncreaseCap float64  // 기본값: 0.5 (50%)
}

func (c PercentageIncreaseCappedCalculator) Calculate(
    targetQPS, curQPS, prevProbability float64) float64 {
    factor := targetQPS / curQPS
    newProbability := prevProbability * factor

    // 확률 증가 시: 50%까지만 허용 (오버샘플링 방지)
    // 확률 감소 시: 즉시 적용 (과도한 샘플링을 빠르게 교정)
    if factor > 1.0 {
        percentIncrease := (newProbability - prevProbability) / prevProbability
        if percentIncrease > c.percentageIncreaseCap {
            newProbability = prevProbability + (prevProbability * c.percentageIncreaseCap)
        }
    }
    return newProbability
}
```

**왜 증가만 제한하는가?**

```
┌────────────────────────────────────────────────────────────┐
│  증가 제한 (cap = 50%):                                    │
│                                                            │
│  prevProb=0.1, targetQPS=1.0, curQPS=0.1                  │
│  factor = 1.0/0.1 = 10                                    │
│  newProb = 0.1 * 10 = 1.0  (900% 증가!)                   │
│  → 50% 캡 적용: newProb = 0.1 + 0.1*0.5 = 0.15           │
│                                                            │
│  이유: 급격한 확률 증가는 갑작스러운 샘플 폭증을 유발하여    │
│        스토리지/네트워크를 과부하시킬 수 있음               │
├────────────────────────────────────────────────────────────┤
│  감소 무제한:                                              │
│                                                            │
│  prevProb=0.5, targetQPS=1.0, curQPS=100                  │
│  factor = 1.0/100 = 0.01                                  │
│  newProb = 0.5 * 0.01 = 0.005  (99% 감소)                 │
│  → 즉시 적용                                              │
│                                                            │
│  이유: 과도한 샘플링은 즉시 교정해야 시스템 부하를 줄일 수  │
│        있음. 감소를 서서히 하면 그 사이에 비용이 발생       │
└────────────────────────────────────────────────────────────┘
```

### 6.6 isUsingAdaptiveSampling 판별

```go
func (p *PostAggregator) isUsingAdaptiveSampling(
    probability float64, service string, operation string,
    throughput serviceOperationThroughput) bool {

    if FloatEquals(probability, p.InitialSamplingProbability) {
        // 초기 확률 그대로면 적응형 사용 중으로 간주
        return true
    }
    if opThroughput, ok := throughput.get(service, operation); ok {
        f := TruncateFloat(probability)
        _, ok := opThroughput.Probabilities[f]
        return ok  // 수집된 스팬의 sampler.param이 현재 확률과 일치하면 적응형 사용 중
    }
    // 이전 캐시 확인
    if len(p.serviceCache) > 1 {
        if e := p.serviceCache[1].Get(service, operation); e != nil {
            return e.UsingAdaptive &&
                !FloatEquals(e.Probability, p.InitialSamplingProbability)
        }
    }
    return false
}
```

이 로직은 서비스가 실제로 Jaeger의 적응형 샘플링을 사용하고 있는지 판별한다. 서비스가 자체 고정 샘플링을 사용하면, Jaeger가 확률을 조정해도 효과가 없으므로 계산을 건너뛴다.

---

## 7. 리더 선출과 분산 조정

### 7.1 왜 리더 선출이 필요한가

여러 Collector 인스턴스가 동시에 적응형 확률을 계산하면 문제가 발생한다:

```
Collector A: 자신이 본 처리량 기준으로 확률 계산 → storage에 저장
Collector B: 자신이 본 처리량 기준으로 확률 계산 → storage에 덮어쓰기!
                                                    ↑
                                                 데이터 충돌!
```

따라서 하나의 리더만 확률을 계산하고, 나머지 팔로워는 storage에서 결과를 읽어온다.

### 7.2 리더/팔로워 동작

**리더 (Leader)**:
- 처리량 데이터를 집계하고 확률을 계산
- 결과를 storage에 저장
- `CalculationInterval`(기본 1분)마다 재계산

```go
// post_aggregator.go - runCalculation
func (p *PostAggregator) runCalculation() {
    // ... 처리량 로드 및 집계 ...

    if p.isLeader() {
        probabilities, qps := p.calculateProbabilitiesAndQPS()
        p.mu.Lock()
        p.probabilities = probabilities
        p.qps = qps
        p.mu.Unlock()
        p.saveProbabilitiesAndQPS()
    }
}
```

**팔로워 (Follower)**:
- 직접 계산하지 않고 storage에서 확률을 주기적으로 로드
- `followerRefreshInterval`(기본 20초)마다 확률 갱신

```go
// provider.go - runUpdateProbabilitiesLoop
func (p *Provider) runUpdateProbabilitiesLoop() {
    select {
    case <-time.After(addJitter(p.followerRefreshInterval)):
    case <-p.shutdown:
        return
    }

    ticker := time.NewTicker(p.followerRefreshInterval)
    defer ticker.Stop()
    for {
        select {
        case <-ticker.C:
            if !p.isLeader() {
                p.loadProbabilities()
                p.generateStrategyResponses()
            }
        case <-p.shutdown:
            return
        }
    }
}
```

### 7.3 Jitter를 사용한 부하 분산

```go
func addJitter(jitterAmount time.Duration) time.Duration {
    return (jitterAmount / 2) + time.Duration(rand.Int63n(int64(jitterAmount/2)))
}
```

**왜 jitter가 필요한가?**

리더가 죽었을 때 모든 팔로워가 동시에 잠금을 시도하면 thundering herd 문제가 발생한다. Jitter는 50%~100% 범위의 랜덤 딜레이를 추가하여 부하를 분산한다.

```
jitterAmount = 20s일 때:
  실제 대기 시간 = 10s + random(0, 10s) = 10s ~ 20s 범위
```

### 7.4 SamplingStore 인터페이스

적응형 샘플링은 `samplingstore.Store` 인터페이스를 통해 처리량과 확률을 영속화한다:

```
samplingstore.Store
├── InsertThroughput([]*model.Throughput)
├── GetThroughput(start, end time.Time) → []*model.Throughput
├── InsertProbabilitiesAndQPS(hostname, probabilities, qps)
├── GetLatestProbabilities() → ServiceOperationProbabilities
└── CreateLock() / CreateSamplingStore()
```

---

## 8. SDK에 전략 제공하기

### 8.1 HTTP 엔드포인트

**파일 경로**: `internal/sampling/http/handler.go`

SDK는 HTTP GET 요청으로 샘플링 전략을 폴링한다:

```
GET /?service=frontend HTTP/1.1
Host: jaeger:5778
```

```go
// handler.go - RegisterRoutes
func (h *Handler) RegisterRoutes(router *http.ServeMux) {
    router.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
        h.serveSamplingHTTP(w, r, h.encodeProto)
    })
}

func (h *Handler) serveSamplingHTTP(
    w http.ResponseWriter, r *http.Request,
    encoder func(strategy *api_v2.SamplingStrategyResponse) ([]byte, error)) {

    service, err := h.serviceFromRequest(w, r)
    if err != nil {
        return
    }
    resp, err := h.params.ConfigManager.GetSamplingStrategy(r.Context(), service)
    if err != nil {
        http.Error(w, fmt.Sprintf("collector error: %+v", err),
            http.StatusInternalServerError)
        return
    }
    jsonBytes, err := encoder(resp)
    // ... JSON 응답 반환 ...
}
```

응답 예시:

```json
{
  "strategyType": "PROBABILISTIC",
  "probabilisticSampling": {
    "samplingRate": 0.01
  },
  "operationSampling": {
    "defaultSamplingProbability": 0.01,
    "defaultLowerBoundTracesPerSecond": 0.0167,
    "perOperationStrategies": [
      {
        "operation": "processPayment",
        "probabilisticSampling": {
          "samplingRate": 0.5
        }
      }
    ]
  }
}
```

### 8.2 gRPC 엔드포인트

**파일 경로**: `internal/sampling/grpc/grpc_handler.go`

```go
type Handler struct {
    samplingProvider samplingstrategy.Provider
}

func (s Handler) GetSamplingStrategy(
    ctx context.Context,
    param *api_v2.SamplingStrategyParameters,
) (*api_v2.SamplingStrategyResponse, error) {
    return s.samplingProvider.GetSamplingStrategy(ctx, param.GetServiceName())
}
```

gRPC 핸들러는 `api_v2.SamplingManager` 서비스를 구현한다. SDK가 gRPC로 연결할 때 사용된다.

### 8.3 응답 구조: SamplingStrategyResponse

```
SamplingStrategyResponse
├── StrategyType: PROBABILISTIC | RATE_LIMITING
├── ProbabilisticSampling
│   └── SamplingRate: float64 (0.0 ~ 1.0)
├── RateLimitingSampling
│   └── MaxTracesPerSecond: int32
└── OperationSampling (적응형에서 사용)
    ├── DefaultSamplingProbability: float64
    ├── DefaultLowerBoundTracesPerSecond: float64
    └── PerOperationStrategies[]
        ├── Operation: string
        └── ProbabilisticSampling
            └── SamplingRate: float64
```

---

## 9. Remote Sampling Extension

### 9.1 확장 구조

Jaeger v2에서 샘플링은 OTel Collector의 확장(extension)으로 동작한다.

**파일 경로**: `cmd/jaeger/internal/extension/remotesampling/`

```go
// config.go
type Config struct {
    File     configoptional.Optional[FileConfig]              `mapstructure:"file"`
    Adaptive configoptional.Optional[AdaptiveConfig]          `mapstructure:"adaptive"`
    HTTP     configoptional.Optional[confighttp.ServerConfig] `mapstructure:"http"`
    GRPC     configoptional.Optional[configgrpc.ServerConfig] `mapstructure:"grpc"`
}
```

**핵심 제약**: `file`과 `adaptive` 중 하나만 설정할 수 있다:

```go
func (cfg *Config) Validate() error {
    if !cfg.File.HasValue() && !cfg.Adaptive.HasValue() {
        return errNoProvider  // 최소 하나 필요
    }
    if cfg.File.HasValue() && cfg.Adaptive.HasValue() {
        return errMultipleProviders  // 둘 다 설정하면 에러
    }
    // ...
}
```

### 9.2 Extension 시작 흐름

```go
// extension.go - Start
func (ext *rsExtension) Start(ctx context.Context, host component.Host) error {
    if ext.cfg.File.HasValue() {
        ext.startFileBasedStrategyProvider(ctx)
    }
    if ext.cfg.Adaptive.HasValue() {
        ext.startAdaptiveStrategyProvider(host)
    }
    if ext.cfg.HTTP.HasValue() {
        ext.startHTTPServer(ctx, host)   // 기본 포트 5778
    }
    if ext.cfg.GRPC.HasValue() {
        ext.startGRPCServer(ctx, host)   // 기본 포트 5779
    }
    return nil
}
```

적응형 모드 초기화:

```go
func (ext *rsExtension) startAdaptiveStrategyProvider(host component.Host) error {
    // 1. SamplingStore 팩토리 획득
    storeFactory, err := jaegerstorage.GetSamplingStoreFactory(storageName, host)

    // 2. SamplingStore 생성
    store, err := storeFactory.CreateSamplingStore(adaptiveCfg.AggregationBuckets)

    // 3. 분산 잠금 생성
    lock, err := storeFactory.CreateLock()

    // 4. 리더 선출 참여자 생성 및 시작
    ep := leaderelection.NewElectionParticipant(lock, defaultResourceName, ...)
    ep.Start()

    // 5. Provider 생성 및 시작
    provider := adaptive.NewProvider(adaptiveCfg.Options, logger, ep, store)
    provider.Start()
}
```

### 9.3 all-in-one 기본 설정

**파일 경로**: `cmd/jaeger/internal/all-in-one.yaml`

```yaml
extensions:
  remote_sampling:
    file:
      path:                             # 비어 있으면 기본 전략 사용
      default_sampling_probability: 1   # 100% 샘플링 (개발 환경)
      reload_interval: 1s
    http:
      endpoint: "${env:JAEGER_LISTEN_HOST:-localhost}:5778"
    grpc:
      endpoint: "${env:JAEGER_LISTEN_HOST:-localhost}:5779"
```

---

## 10. 설정 및 운영

### 10.1 파일 기반 샘플링 설정 예시

```yaml
extensions:
  remote_sampling:
    file:
      path: /etc/jaeger/sampling-strategies.json
      reload_interval: 30s
      default_sampling_probability: 0.01
    http:
      endpoint: 0.0.0.0:5778
    grpc:
      endpoint: 0.0.0.0:5779
```

### 10.2 적응형 샘플링 설정 예시

```yaml
extensions:
  jaeger_storage:
    backends:
      main_storage:
        cassandra:
          # ...
    sampling_stores:
      adaptive_store:
        cassandra:
          # ...

  remote_sampling:
    adaptive:
      sampling_store: adaptive_store
      target_samples_per_second: 1.0
      delta_tolerance: 0.3
      calculation_interval: 1m
      aggregation_buckets: 10
      calculation_buckets: 1
      calculation_delay: 2m
      initial_sampling_probability: 0.001
      min_sampling_probability: 0.00001
      min_samples_per_second: 0.0167
      leader_lease_refresh_interval: 5s
      follower_lease_refresh_interval: 60s
    http:
      endpoint: 0.0.0.0:5778
    grpc:
      endpoint: 0.0.0.0:5779
```

### 10.3 모니터링 지표

적응형 샘플링에서 노출하는 메트릭:

| 메트릭 | 타입 | 설명 |
|--------|------|------|
| `sampling_operations` | Counter | 관찰된 총 오퍼레이션 수 |
| `sampling_services` | Counter | 관찰된 총 서비스 수 |
| `adaptive_sampling_processor.operations_calculated` | Gauge | 계산된 오퍼레이션 수 |
| `adaptive_sampling_processor.calculate_probabilities` | Timer | 확률 계산 소요 시간 |

HTTP 핸들러 메트릭:

| 메트릭 | 설명 |
|--------|------|
| `http-server.requests{type=sampling}` | 성공한 샘플링 요청 수 |
| `http-server.errors{status=4xx}` | 잘못된 요청 수 |
| `http-server.errors{status=5xx,source=collector-proxy}` | Provider 에러 수 |

### 10.4 트러블슈팅

```
┌─────────────────────────────────────────────────────────────┐
│ 증상: 특정 서비스의 트레이스가 수집되지 않음                    │
├─────────────────────────────────────────────────────────────┤
│ 확인 1: GET /?service=<서비스명> 으로 전략 조회                │
│   → samplingRate가 너무 낮은지 확인                          │
│                                                             │
│ 확인 2: 파일 기반이면 JSON 파일의 해당 서비스 항목 확인         │
│   → operation_strategies에 해당 operation이 있는지 확인       │
│                                                             │
│ 확인 3: 적응형이면 리더 선출 상태 확인                         │
│   → 로그에서 "failed to get throughput" 에러 확인             │
│   → SamplingStore 연결 상태 확인                              │
│                                                             │
│ 확인 4: SDK가 올바른 포트(5778)로 폴링하는지 확인               │
└─────────────────────────────────────────────────────────────┘
```

---

## 11. 내부 구현 상세

### 11.1 SamplingCache

**파일 경로**: `internal/sampling/samplingstrategy/adaptive/cache.go`

서비스-오퍼레이션별 마지막 확률과 적응형 사용 여부를 캐시한다:

```go
type SamplingCacheEntry struct {
    Probability   float64
    UsingAdaptive bool
}

type SamplingCache map[string]map[string]*SamplingCacheEntry

func (s SamplingCache) Set(service, operation string, entry *SamplingCacheEntry) {
    if _, ok := s[service]; !ok {
        s[service] = make(map[string]*SamplingCacheEntry)
    }
    s[service][operation] = entry
}
```

PostAggregator는 최대 25개(`serviceCacheSize`)의 과거 캐시를 유지한다. 이 캐시는 서비스가 적응형 샘플링을 사용하는지 판별하는 데 사용된다.

```go
func (p *PostAggregator) prependServiceCache() {
    p.serviceCache = append([]SamplingCache{make(SamplingCache)}, p.serviceCache...)
    if len(p.serviceCache) > serviceCacheSize {
        p.serviceCache = p.serviceCache[0:serviceCacheSize]
    }
}
```

### 11.2 Provider의 generateStrategyResponses

```go
// provider.go - generateStrategyResponses
func (p *Provider) generateStrategyResponses() {
    p.mu.RLock()
    strategies := make(map[string]*api_v2.SamplingStrategyResponse)
    for svc, opProbabilities := range p.probabilities {
        opStrategies := make([]*api_v2.OperationSamplingStrategy, len(opProbabilities))
        var idx int
        for op, probability := range opProbabilities {
            opStrategies[idx] = &api_v2.OperationSamplingStrategy{
                Operation: op,
                ProbabilisticSampling: &api_v2.ProbabilisticSamplingStrategy{
                    SamplingRate: probability,
                },
            }
            idx++
        }
        strategy := p.generateDefaultSamplingStrategyResponse()
        strategy.OperationSampling.PerOperationStrategies = opStrategies
        strategies[svc] = strategy
    }
    p.mu.RUnlock()

    p.mu.Lock()
    defer p.mu.Unlock()
    p.strategyResponses = strategies
}
```

이 함수는 내부 확률 맵(`ServiceOperationProbabilities`)을 gRPC/HTTP 응답 형식(`SamplingStrategyResponse`)으로 변환한다. 읽기 잠금(RLock)으로 확률을 읽고, 쓰기 잠금(Lock)으로 전략 캐시를 교체한다.

### 11.3 처리량 집계 (여러 Collector 병합)

```go
func (*PostAggregator) aggregateThroughput(
    throughputs []*model.Throughput) serviceOperationThroughput {
    aggregatedThroughput := make(serviceOperationThroughput)
    for _, throughput := range throughputs {
        service := throughput.Service
        operation := throughput.Operation
        if _, ok := aggregatedThroughput[service]; !ok {
            aggregatedThroughput[service] = make(map[string]*model.Throughput)
        }
        if t, ok := aggregatedThroughput[service][operation]; ok {
            t.Count += throughput.Count
            t.Probabilities = merge(t.Probabilities, throughput.Probabilities)
        } else {
            copyThroughput := model.Throughput{
                Service:       throughput.Service,
                Operation:     throughput.Operation,
                Count:         throughput.Count,
                Probabilities: copySet(throughput.Probabilities),
            }
            aggregatedThroughput[service][operation] = &copyThroughput
        }
    }
    return aggregatedThroughput
}
```

여러 Collector가 각자의 처리량을 storage에 기록하므로, 리더는 이 데이터를 합산(Count)하고 사용된 확률 집합(Probabilities)을 병합(union)한다.

---

## 12. 정리

### 12.1 핵심 설계 원칙

| 원칙 | 구현 방식 |
|------|----------|
| **비용-가시성 균형** | 서비스/오퍼레이션별 독립적 확률 |
| **점진적 조정** | 증가 50% 캡, 감소는 즉시 |
| **안정성** | DeltaTolerance로 불필요한 진동 방지 |
| **분산 환경 지원** | 리더 선출 + storage 기반 조정 |
| **락-프리 읽기** | atomic.Value (파일), RWMutex (적응형) |
| **최근 데이터 우선** | i^4 가중치로 최신 처리량 강조 |
| **유연한 설정 소스** | 파일 경로, HTTP URL 모두 지원 |

### 12.2 데이터 흐름 요약

```
                           파일 기반 모드
                    ┌─────────────────────┐
JSON 파일/URL ──→   │  samplingProvider    │ ──→  SDK
  주기적 리로드      │  (atomic.Value)     │    (HTTP/gRPC)
                    └─────────────────────┘

                           적응형 모드
Root Spans ──→ Aggregator ──→ PostAggregator ──→ SamplingStore
                  │                │                   │
                  │           리더만 계산               │
                  │                                    ▼
                  │                              Provider ──→ SDK
                  │                           (팔로워: 20초 간격 로드)
                  └── storage에 처리량 저장
```

### 12.3 파일 기반 vs 적응형 비교

| 특성 | 파일 기반 | 적응형 |
|------|----------|--------|
| 설정 복잡도 | 낮음 (JSON 파일 하나) | 높음 (storage + 리더 선출) |
| 트래픽 변화 대응 | 수동 (파일 수정) | 자동 (실시간 조정) |
| 필요 인프라 | 없음 | SamplingStore, 분산 잠금 |
| 적합한 규모 | 소~중규모 | 대규모 |
| 서비스별 공정성 | 보장 안 됨 (고정 확률) | 보장 (목표 QPS 기반) |
| 오버헤드 | 거의 없음 | CPU(계산), 네트워크(storage 통신) |
| 새 서비스 대응 | 수동 추가 필요 | 자동 탐지 및 확률 할당 |

### 12.4 관련 소스 파일 전체 목록

```
internal/sampling/
├── samplingstrategy/
│   ├── provider.go                                          # Provider 인터페이스
│   ├── file/
│   │   ├── provider.go                                      # 파일 기반 Provider
│   │   ├── strategy.go                                      # JSON 전략 구조체
│   │   ├── options.go                                       # 파일 기반 옵션
│   │   └── constants.go                                     # 상수, 기본 전략
│   └── adaptive/
│       ├── provider.go                                      # 적응형 Provider
│       ├── aggregator.go                                    # 처리량 수집기
│       ├── post_aggregator.go                               # 확률 계산 엔진
│       ├── options.go                                       # 적응형 옵션
│       ├── weightvectorcache.go                             # 가중치 벡터 캐시
│       ├── cache.go                                         # 서비스 캐시
│       └── calculationstrategy/
│           └── percentage_increase_capped_calculator.go      # 증가 캡 계산기
├── http/
│   ├── handler.go                                           # HTTP 샘플링 핸들러
│   └── cfgmgr.go                                            # ConfigManager 래퍼
└── grpc/
    └── grpc_handler.go                                      # gRPC 샘플링 핸들러

cmd/jaeger/internal/extension/remotesampling/
├── config.go                                                # Extension 설정
├── extension.go                                             # Extension 구현
└── factory.go                                               # Extension 팩토리
```
