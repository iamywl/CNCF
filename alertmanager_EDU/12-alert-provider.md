# Alertmanager Alert Provider Deep Dive

## 1. 개요

Alert Provider는 Alert의 저장, 조회, 구독을 담당하는 추상 계층이다. 현재 유일한 구현은 메모리 기반(`provider/mem`)이며, 내부적으로 `store.Alerts`를 사용한다.

## 2. Provider 인터페이스

```go
// provider/provider.go
type Alert struct {
    Header map[string]string   // 메타데이터 (tracing 정보)
    Data   *types.Alert        // 실제 Alert
}

type Iterator interface {
    Err() error
    Close()
}

type AlertIterator interface {
    Iterator
    Next() <-chan *Alert       // Alert 스트림 채널
}

type Alerts interface {
    Subscribe(name string) AlertIterator
    SlurpAndSubscribe(name string) ([]*types.Alert, AlertIterator)
    GetPending() AlertIterator
}
```

### 2.1 Subscribe vs SlurpAndSubscribe

| 메서드 | 반환 | 용도 |
|--------|------|------|
| `Subscribe` | Iterator만 | 실시간 스트림만 필요 (Inhibitor) |
| `SlurpAndSubscribe` | 초기 Alert + Iterator | 기존 + 실시간 모두 필요 (Dispatcher) |
| `GetPending` | Iterator | 현재 스냅샷 (API) |

`SlurpAndSubscribe`는 Dispatcher가 시작할 때 기존 Alert를 놓치지 않도록 보장한다.

## 3. 메모리 구현 (mem.Alerts)

### 3.1 구조체

```go
// provider/mem/mem.go
type Alerts struct {
    cancel  context.CancelFunc

    mtx       sync.Mutex
    alerts    *store.Alerts              // 실제 Alert 저장소
    marker    types.AlertMarker          // Alert 상태 추적
    listeners map[int]listeningAlerts    // 구독자 맵
    next      int                        // 다음 구독자 ID

    callback   AlertStoreCallback        // PreStore/PostStore 콜백
    logger     *slog.Logger
    propagator propagation.TextMapPropagator
    flagger    featurecontrol.Flagger

    // 메트릭
    alertsLimit        prometheus.Gauge
    alertsLimitedTotal *prometheus.CounterVec
}
```

### 3.2 AlertStoreCallback

```go
// provider/mem/mem.go
type AlertStoreCallback interface {
    PreStore(alert *types.Alert, existing bool) error
    PostStore(alert *types.Alert, existing bool)
    PostDelete(alert *types.Alert)
    PostGC(fingerprints model.Fingerprints)
}
```

| 콜백 | 호출 시점 | 역할 |
|------|----------|------|
| `PreStore` | Alert 저장 전 | 유효성 검증, 제한 확인 |
| `PostStore` | Alert 저장 후 | 메트릭 업데이트 |
| `PostDelete` | Alert 삭제 후 | 정리 작업 |
| `PostGC` | GC 후 | Marker 정리 |

### 3.3 생성

```go
// provider/mem/mem.go
func NewAlerts(
    ctx context.Context,
    m types.AlertMarker,
    intervalGC time.Duration,
    perAlertNameLimit int,
    alertCallback AlertStoreCallback,
    l *slog.Logger,
    r prometheus.Registerer,
    flagger featurecontrol.Flagger,
) (*Alerts, error)
```

```
NewAlerts() 흐름:
    1. store.Alerts 생성 (내부 map)
    2. perAlertNameLimit 설정 (Alert 이름별 제한)
    3. GC 루프 시작 (goroutine)
    4. 콜백 등록
```

## 4. Put() — Alert 저장

```
Alerts.Put(alerts ...*types.Alert):
    각 Alert에 대해:
    1. Fingerprint 계산 (Labels 해시)
    2. 기존 Alert 존재 확인

    3. PreStore 콜백 호출
       - existing=true/false
       - 에러 반환 시 건너뜀

    4. 기존 Alert 있으면:
       - StartsAt: 기존 값 유지 (더 이르면)
       - EndsAt: 새 값 사용
       - Annotations: 새 값 사용
       - UpdatedAt: now

    5. store.Alerts.Set(alert)
       - map[fp] = alert
       - limit.Bucket 업데이트

    6. PostStore 콜백 호출

    7. 리스너들에게 브로드캐스트
       - 각 구독자 채널에 Alert 전송
```

## 5. 구독 모델

### 5.1 구독자 등록

```go
// provider/mem/mem.go
type listeningAlerts struct {
    alerts chan *provider.Alert
    done   chan struct{}
}
```

```
Subscribe(name):
    1. listeningAlerts 생성 (버퍼 채널)
    2. listeners[next] = listeningAlerts
    3. next++
    4. AlertIterator 반환
```

### 5.2 브로드캐스트

```
Put() 내부:
    for _, l := range a.listeners {
        select {
        case l.alerts <- &provider.Alert{
            Header: tracing 정보,
            Data:   alert,
        }:
        case <-l.done:
            // 구독자가 종료됨
        default:
            // 채널이 가득 참 → 건너뜀 (비차단)
        }
    }
```

**중요**: 채널이 가득 차면 Alert가 드롭된다. 이는 느린 소비자가 전체 시스템을 차단하지 않도록 하기 위함이다.

### 5.3 SlurpAndSubscribe

```
SlurpAndSubscribe(name):
    a.mtx.Lock()
    1. Subscribe() → Iterator 생성
    2. alerts.List() → 현재 모든 Alert 스냅샷
    a.mtx.Unlock()

    반환: (스냅샷, Iterator)
```

Lock 안에서 구독과 스냅샷을 동시에 수행하여, 스냅샷 이후의 Alert만 Iterator에서 수신되도록 보장한다.

## 6. store.Alerts (내부 저장소)

### 6.1 구조체

```go
// store/store.go
type Alerts struct {
    sync.Mutex
    alerts        map[model.Fingerprint]*types.Alert  // 핵심 저장소
    gcCallback    func([]*types.Alert)                // GC 콜백
    limits        map[string]*limit.Bucket[model.Fingerprint]  // 이름별 제한
    perAlertLimit int
    destroyed     bool
}
```

### 6.2 핵심 메서드

```go
func NewAlerts() *Alerts
func (a *Alerts) WithPerAlertLimit(lim int) *Alerts
func (a *Alerts) SetGCCallback(cb func([]*types.Alert))
func (a *Alerts) Run(ctx context.Context, interval time.Duration)

func (a *Alerts) Set(alert *types.Alert) error   // 추가/업데이트
func (a *Alerts) Get(fp model.Fingerprint) (*types.Alert, error)
func (a *Alerts) Delete(alert *types.Alert) error
func (a *Alerts) List() []*types.Alert            // 전체 목록
func (a *Alerts) Empty() bool
func (a *Alerts) Len() int

func (a *Alerts) GC() (deleted []*types.Alert)    // 만료 Alert 삭제
```

### 6.3 Set() 동작

```
store.Alerts.Set(alert):
    fp := alert.Fingerprint()

    if perAlertLimit > 0:
        alertName := alert.Labels["alertname"]
        bucket := limits[alertName]
        if bucket == nil:
            bucket = limit.NewBucket(perAlertLimit)
            limits[alertName] = bucket
        if !bucket.Upsert(fp, alert.ResolvedAt()):
            return ErrLimited  // 제한 초과

    alerts[fp] = alert
    return nil
```

## 7. limit.Bucket (용량 제한)

### 7.1 구조체

```go
// limit/bucket.go
type Bucket[V comparable] struct {
    mtx      sync.Mutex
    index    map[V]*item[V]          // 빠른 조회
    items    sortedItems[V]          // 힙 정렬 (만료 시간 기준)
    capacity int
}

type item[V any] struct {
    value    V
    priority time.Time              // 만료 시간
    index    int                    // 힙 인덱스
}
```

### 7.2 Upsert() 동작

```
Bucket.Upsert(value, priority):
    if value가 이미 존재:
        priority 업데이트 (힙 재정렬)
        return true

    if len(items) < capacity:
        새 item 추가
        return true

    // 용량 초과
    oldest := items[0]  // 힙의 루트 (가장 오래된 항목)
    if oldest.expired(now):
        oldest 제거, 새 item 추가
        return true

    return false  // 제한 초과, 추가 실패
```

힙을 사용하여 가장 오래된 항목을 O(log n)으로 찾고, 만료된 항목이 있으면 교체한다.

## 8. GC (Garbage Collection)

### 8.1 gcAlerts()

```
store.Alerts.gcAlerts():
    now := time.Now()
    var deleted []*types.Alert

    for fp, alert := range alerts:
        if alert.Resolved() && alert.ResolvedAt().Before(now):
            delete(alerts, fp)
            deleted = append(deleted, alert)

    return deleted
```

### 8.2 gcLimitBuckets()

```
store.Alerts.gcLimitBuckets():
    for name, bucket := range limits:
        if bucket.IsStale():
            delete(limits, name)  // 모든 항목이 만료된 Bucket 제거
```

### 8.3 Provider GC 루프

```
mem.Alerts GC goroutine:
    ticker := time.NewTicker(intervalGC)
    for {
        select {
        case <-ticker.C:
            1. store.Alerts.GC() → 만료 Alert 삭제
            2. gcCallback(deleted) → Provider 수준 정리
            3. PostGC 콜백 → Marker.Delete(fps)
        case <-ctx.Done():
            return
        }
    }
```

## 9. Alert 생명주기 (Provider 관점)

```
API POST /api/v2/alerts
    │
    ▼
mem.Alerts.Put(alert)
    │
    ├─ PreStore → 유효성 검증
    │
    ├─ store.Set(alert)
    │   └─ map[fp] = alert
    │
    ├─ PostStore
    │
    └─ 리스너 브로드캐스트
        ├─ Dispatcher (routeAlert)
        └─ Inhibitor (scache 업데이트)

    ...시간 경과...

GC Tick
    │
    ▼
store.GC()
    │
    ├─ Resolved Alert 삭제
    │
    └─ PostGC
        └─ Marker.Delete(fps) → 상태 정리
```

## 10. 에러 타입

```go
// store/store.go
var (
    ErrLimited   = errors.New("alert limited")    // 제한 초과
    ErrNotFound  = errors.New("alert not found")  // 없는 Alert
    ErrDestroyed = errors.New("alert store destroyed")  // 저장소 파괴됨
)
```

## 11. 메트릭

| 메트릭 | 타입 | 설명 |
|--------|------|------|
| `alertmanager_alerts` | Gauge | 현재 Alert 수 |
| `alertmanager_alerts_received_total` | Counter | 수신 Alert 총 수 |
| `alertmanager_alerts_invalid_total` | Counter | 유효하지 않은 Alert 수 |
| `alertmanager_alerts_limit` | Gauge | Alert 제한 (설정된 경우) |
| `alertmanager_alerts_limited_total` | Counter (alertname 레이블) | 제한 초과로 거부된 Alert 수 |

## 12. 동시성 모델

```
┌─────────────────────────────────────┐
│  mem.Alerts                         │
│                                     │
│  [sync.Mutex] mtx                   │
│    Put(), Subscribe(), GC() 보호    │
│                                     │
│  store.Alerts                       │
│  [sync.Mutex] 내장                  │
│    Set(), Get(), GC() 보호          │
│                                     │
│  limit.Bucket                       │
│  [sync.Mutex] mtx                   │
│    Upsert(), IsStale() 보호         │
│                                     │
│  리스너 채널:                        │
│    select + default로 비차단 전송    │
│    → 느린 소비자 영향 최소화         │
└─────────────────────────────────────┘
```

## 13. OpenTelemetry 추적

### 13.1 tracer 초기화

```go
// provider/mem/mem.go
var tracer = otel.Tracer("github.com/prometheus/alertmanager/provider/mem")
```

### 13.2 Put()에서의 추적 전파

```go
// Put() 내부에서 tracing 정보를 Alert Header에 주입
func (a *Alerts) Put(alerts ...*types.Alert) error {
    for _, alert := range alerts {
        // tracing context를 Header로 전파
        carrier := make(propagation.HeaderCarrier)
        a.propagator.Inject(ctx, carrier)

        providerAlert := &provider.Alert{
            Header: carrier,    // tracing 정보 포함
            Data:   alert,
        }
        // 리스너에게 전달 시 Header도 함께 전달
    }
}
```

**왜 Header에 tracing 정보를 주입하는가?** API로 수신된 Alert가 Dispatcher → Pipeline → Integration으로 전달될 때, 원래 API 요청의 trace context를 유지해야 end-to-end 추적이 가능하다. `propagation.HeaderCarrier`를 통해 context를 직렬화하여 Alert과 함께 전달한다.

## 14. alertChannelLength과 드롭 정책

```go
// provider/mem/mem.go
const alertChannelLength = 200
```

구독자 채널의 버퍼 크기는 200이다. Put()에서 리스너에게 Alert를 전송할 때:

```go
select {
case l.alerts <- providerAlert:
    // 전송 성공
case <-l.done:
    // 구독자 종료
default:
    // 채널 가득 참 → 드롭
}
```

**왜 드롭 정책인가?** 대안은 다음과 같다:

| 정책 | 장점 | 단점 |
|------|------|------|
| 차단 (blocking) | Alert 손실 없음 | 느린 소비자가 전체 시스템 차단 |
| 드롭 (current) | 시스템 안정성 유지 | 일부 Alert 손실 가능 |
| 무한 버퍼 | 손실 없고 차단 없음 | 메모리 폭발 위험 |

Alertmanager는 고가용성 시스템이므로, 전체 시스템 안정성이 개별 Alert 보장보다 우선한다. 드롭된 Alert는 다음 Prometheus scrape에서 다시 수신되므로, 영구 손실이 아니다.

### 14.1 subscriberChannelWrites 메트릭

```go
// provider/mem/mem.go
a.subscriberChannelWrites = promauto.With(r).NewCounterVec(
    prometheus.CounterOpts{
        Name: "alertmanager_alerts_subscriber_channel_writes_total",
        Help: "Total number of write attempts to subscriber channels.",
    },
    []string{"subscriber", "result"},  // result: "success" or "dropped"
)
```

이 메트릭으로 채널 드롭 빈도를 모니터링할 수 있다. `result="dropped"`가 증가하면 소비자가 처리 속도를 따라가지 못하는 것이므로, 시스템 스케일링이 필요하다.

## 15. Feature Control 플래그

```go
// provider/mem/mem.go
type Alerts struct {
    // ...
    flagger featurecontrol.Flagger
}

func (a *Alerts) registerMetrics(r prometheus.Registerer) {
    labels := []string{}
    if a.flagger.EnableAlertNamesInMetrics() {
        labels = append(labels, "alertname")
    }
    a.alertsLimitedTotal = promauto.With(r).NewCounterVec(
        prometheus.CounterOpts{
            Name: "alertmanager_alerts_limited_total",
        },
        labels,
    )
}
```

**왜 alertname 레이블이 선택적인가?** Alert 이름의 카디널리티가 높으면(수백 개 이상) Prometheus 메트릭의 시계열 수가 폭발할 수 있다. `EnableAlertNamesInMetrics` 플래그로 이를 제어하여, 필요한 환경에서만 활성화한다.

## 16. perAlertNameLimit 동작 원리

```
Alert 이름별 제한 흐름:

PUT alertname="HighCPU", instance="node-1"
  → bucket["HighCPU"].Upsert(fp1, resolvedAt)
  → 용량 미만 → 성공

PUT alertname="HighCPU", instance="node-2"
  → bucket["HighCPU"].Upsert(fp2, resolvedAt)
  → 용량 미만 → 성공

PUT alertname="HighCPU", instance="node-3"
  → bucket["HighCPU"].Upsert(fp3, resolvedAt)
  → 용량 == capacity

PUT alertname="HighCPU", instance="node-4"
  → bucket["HighCPU"].Upsert(fp4, resolvedAt)
  → 용량 초과
  → 힙에서 가장 오래된 항목 확인
  → 만료됨 → 교체 성공
  → 만료 안 됨 → ErrLimited 반환

핵심: 만료된 Alert를 자동으로 교체하여
      resolved Alert가 공간을 차지하지 않도록 한다.
```

## 17. 성능 고려사항

### 17.1 Put() 성능

```
Put() 비용 분석:

단일 Alert Put:
  1. Fingerprint 계산: O(L) where L = 레이블 수
  2. 기존 Alert 조회: O(1) (해시맵)
  3. PreStore 콜백: O(1)
  4. store.Set: O(1) (해시맵) + O(log N) (힙, limit 사용 시)
  5. PostStore 콜백: O(1)
  6. 리스너 브로드캐스트: O(S) where S = 구독자 수

배치 Put (N alerts):
  → O(N × (L + log M + S))
  where M = 현재 Alert 수, S = 구독자 수
```

### 17.2 GC 성능

```
GC() 비용:
  gcAlerts: O(N) — 전체 Alert 순회
  gcLimitBuckets: O(B) — 전체 Bucket 순회

GC 주기 최적화:
  - intervalGC이 너무 짧으면: 불필요한 CPU 사용
  - intervalGC이 너무 길면: 만료 Alert가 메모리에 오래 존재
  - 기본값은 적절한 균형을 유지
```

### 17.3 SlurpAndSubscribe의 Lock 비용

```go
func (a *Alerts) SlurpAndSubscribe(name string) ([]*types.Alert, AlertIterator) {
    a.mtx.Lock()
    iter := a.Subscribe(name)
    alerts := a.alerts.List()  // 전체 Alert 복사
    a.mtx.Unlock()
    return alerts, iter
}
```

Lock 내에서 전체 Alert를 List()로 복사한다. Alert 수가 수만 개이면 Lock 유지 시간이 길어져 다른 Put() 호출이 대기하게 된다. 그러나 이 메서드는 Dispatcher 시작 시에만 호출되므로, 정상 운영 중에는 영향이 없다.

## 18. 테스트 전략

### 18.1 AlertStoreCallback Mock

```go
type fakeCallback struct {
    preStoreErr error
    preStoreCalls int
    postStoreCalls int
    postDeleteCalls int
    postGCCalls int
}

func (f *fakeCallback) PreStore(alert *types.Alert, existing bool) error {
    f.preStoreCalls++
    return f.preStoreErr
}
```

### 18.2 핵심 테스트 시나리오

```
1. Put + Get 왕복
   - Alert 저장 후 Fingerprint로 조회 → 일치 확인

2. 구독 + 브로드캐스트
   - Subscribe → Put → 채널에서 Alert 수신 확인

3. SlurpAndSubscribe 원자성
   - 기존 Alert 존재 → SlurpAndSubscribe
   - 스냅샷에 기존 Alert 포함 + 이후 Alert만 Iterator에서 수신

4. 채널 드롭
   - 채널 버퍼 가득 참 → Put → 드롭 확인 (차단 안 됨)

5. GC
   - resolved Alert 생성 → GC → 삭제 확인
   - PostGC 콜백에서 Fingerprint 전달 확인

6. perAlertNameLimit
   - limit 설정 → limit+1번째 Alert → ErrLimited 확인
   - resolved Alert → GC → 새 Alert 저장 가능
```
