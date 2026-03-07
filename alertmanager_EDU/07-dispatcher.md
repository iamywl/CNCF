# Alertmanager Dispatcher Deep Dive

## 1. 개요

Dispatcher는 Alertmanager의 핵심 엔진으로, Provider로부터 Alert를 수집하여 Route 트리와 매칭하고, Aggregation Group으로 묶어 Notification Pipeline에 전달한다. `dispatch/dispatch.go`와 `dispatch/route.go`에 구현되어 있다.

## 2. Dispatcher 구조체

```go
// dispatch/dispatch.go:89
type Dispatcher struct {
    route      *Route                         // 라우팅 트리 루트
    alerts     provider.Alerts                // Alert 저장소 (구독)
    stage      notify.Stage                   // 알림 처리 파이프라인
    marker     types.GroupMarker              // 그룹 뮤트 상태 추적
    metrics    *DispatcherMetrics             // Prometheus 메트릭
    limits     Limits                         // Aggregation Group 수 제한
    propagator propagation.TextMapPropagator  // OpenTelemetry 전파자

    timeout func(time.Duration) time.Duration // 타임아웃 조정 함수

    loaded   chan struct{}                    // 초기 Alert 로딩 완료 신호
    finished sync.WaitGroup                  // goroutine 종료 대기
    ctx      context.Context                 // 취소 컨텍스트
    cancel   func()                          // 취소 함수

    routeGroupsSlice []routeAggrGroups       // Route별 Aggregation Group 슬라이스
    aggrGroupsNum    atomic.Int32            // 현재 전체 Aggregation Group 수

    maintenanceInterval time.Duration        // 유지보수 루프 간격
    concurrency         int                  // Alert 수집 워커 수

    logger     *slog.Logger
    startTimer *time.Timer                   // 시작 지연 타이머
    state      atomic.Int32                  // Dispatcher 상태
}
```

### 2.1 상태 머신

```go
// dispatch/dispatch.go:42
const (
    DispatcherStateUnknown        = iota  // 초기 상태
    DispatcherStateWaitingToStart         // 시작 대기 중
    DispatcherStateRunning                // 실행 중
    DispatcherStateStopped                // 중지됨
)
```

```
Unknown ──→ WaitingToStart ──→ Running ──→ Stopped
   │                                          ↑
   └──────────────────────────────────────────┘
```

### 2.2 routeAggrGroups

```go
// dispatch/dispatch.go:125
type routeAggrGroups struct {
    route     *Route
    groups    sync.Map       // map[string]*aggrGroup (그룹 키 → aggrGroup)
    groupsLen atomic.Int64   // 현재 그룹 수 (sync.Map의 빠른 카운트)
}
```

Route 트리의 각 노드에 대응하는 Aggregation Group 컨테이너이다. `routeGroupsSlice`는 Route의 `Idx`를 인덱스로 사용하여 O(1) 접근한다.

### 2.3 Limits 인터페이스

```go
// dispatch/dispatch.go:118
type Limits interface {
    MaxNumberOfAggregationGroups() int
}
```

0 또는 음수는 무제한을 의미한다.

## 3. 생성 (NewDispatcher)

```go
// dispatch/dispatch.go:131
func NewDispatcher(
    alerts provider.Alerts,
    route *Route,
    stage notify.Stage,
    marker types.GroupMarker,
    timeout func(time.Duration) time.Duration,
    maintenanceInterval time.Duration,
    limits Limits,
    logger *slog.Logger,
    metrics *DispatcherMetrics,
) *Dispatcher
```

핵심 계산:

```go
// 동시성 자동 결정: GOMAXPROCS/2, 최소 2, 최대 8
concurrency := min(max(runtime.GOMAXPROCS(0)/2, 2), 8)
```

## 4. Run() 실행 흐름

```go
// dispatch/dispatch.go:170
func (d *Dispatcher) Run(dispatchStartTime time.Time) {
    // 1. 상태 전환: Unknown → WaitingToStart
    if !d.state.CompareAndSwap(DispatcherStateUnknown, DispatcherStateWaitingToStart) {
        return
    }

    // 2. routeGroupsSlice 초기화 (Route 트리 Walk)
    d.routeGroupsSlice = make([]routeAggrGroups, d.route.Idx+1)
    d.route.Walk(func(r *Route) {
        d.routeGroupsSlice[r.Idx] = routeAggrGroups{route: r}
    })

    // 3. 초기 Alert 로드
    initialAlerts, it := d.alerts.SlurpAndSubscribe("dispatcher")
    for _, alert := range initialAlerts {
        d.routeAlert(d.ctx, alert)
    }
    close(d.loaded)  // 로딩 완료 신호

    // 4. run() 호출
    d.run(it)
}
```

### 4.1 run() 내부 goroutine 구조

```
run(it)
  │
  ├─ [goroutine 1] 유지보수 루프
  │   └─ ticker(maintenanceInterval) → doMaintenance()
  │      └─ 파괴된 aggrGroup 정리
  │
  ├─ [goroutine 2] 시작 타이머
  │   └─ startTimer 만료 → WaitingToStart → Running
  │      └─ 모든 기존 aggrGroup의 runAG() 시작
  │
  ├─ [goroutine 3~N] Alert 수집 워커 (concurrency개)
  │   └─ alertCh ← it.Next()
  │      └─ routeAlert(ctx, alert)
  │
  └─ <-d.ctx.Done() // 취소 대기
```

### 4.2 Alert 수집 워커

```go
// dispatch/dispatch.go:235
alertCh := it.Next()
for i := 0; i < d.concurrency; i++ {
    go func(workerID int) {
        for {
            select {
            case alert, ok := <-alertCh:
                if !ok { return }
                ctx := d.ctx
                if alert.Header != nil {
                    ctx = d.propagator.Extract(ctx, propagation.MapCarrier(alert.Header))
                }
                d.routeAlert(ctx, alert.Data)
            case <-d.ctx.Done():
                return
            }
        }
    }(i)
}
```

여러 워커가 동일한 채널에서 Alert를 경쟁적으로 소비한다. OpenTelemetry 컨텍스트도 전파된다.

## 5. routeAlert() 라우팅 알고리즘

```go
// dispatch/dispatch.go:274
func (d *Dispatcher) routeAlert(ctx context.Context, alert *types.Alert) {
    now := time.Now()
    for _, r := range d.route.Match(alert.Labels) {
        d.groupAlert(ctx, alert, r)
    }
    d.metrics.processingDuration.Observe(time.Since(now).Seconds())
}
```

1. `d.route.Match(alert.Labels)` — Route 트리에서 매칭되는 모든 Route 반환
2. 각 매칭 Route에 대해 `groupAlert()` 호출

## 6. Route 트리

### 6.1 Route 구조체

```go
// dispatch/route.go:42
type Route struct {
    parent    *Route              // 부모 Route
    RouteOpts RouteOpts           // 라우팅 옵션 (상속됨)
    Matchers  labels.Matchers     // 레이블 매칭 조건
    Continue  bool                // true: 형제 Route도 계속 매칭
    Routes    []*Route            // 자식 Route
    Idx       int                 // 고유 인덱스
}
```

### 6.2 RouteOpts (기본값)

```go
// dispatch/route.go:32
var DefaultRouteOpts = RouteOpts{
    GroupWait:      30 * time.Second,
    GroupInterval:  5 * time.Minute,
    RepeatInterval: 4 * time.Hour,
    GroupBy:        map[model.LabelName]struct{}{},
    GroupByAll:     false,
}
```

### 6.3 newRoute() 옵션 상속

```go
// dispatch/route.go:68
func newRoute(cr *config.Route, parent *Route, counter *int) *Route {
    opts := DefaultRouteOpts
    if parent != nil {
        opts = parent.RouteOpts  // 부모 옵션 상속
    }

    // 자식 설정으로 덮어쓰기
    if cr.Receiver != "" { opts.Receiver = cr.Receiver }
    if cr.GroupBy != nil { /* GroupBy 설정 */ }
    if cr.GroupWait != nil { opts.GroupWait = time.Duration(*cr.GroupWait) }
    if cr.GroupInterval != nil { opts.GroupInterval = time.Duration(*cr.GroupInterval) }
    if cr.RepeatInterval != nil { opts.RepeatInterval = time.Duration(*cr.RepeatInterval) }

    // 자식 Route 먼저 생성 (낮은 인덱스)
    route.Routes = newRoutes(cr.Routes, route, counter)
    route.Idx = *counter  // 인덱스 할당
    *counter++

    return route
}
```

**중요**: 자식 Route가 먼저 생성되므로, 부모의 `Idx`가 자식보다 크다. 루트 Route의 `Idx`가 가장 크며, `routeGroupsSlice`의 길이를 결정한다.

### 6.4 Match() DFS 알고리즘

```go
// dispatch/route.go:160
func (r *Route) Match(lset model.LabelSet) []*Route {
    if !r.Matchers.Matches(lset) {
        return nil  // 현재 Route 매칭 실패
    }

    var all []*Route
    for _, cr := range r.Routes {
        matches := cr.Match(lset)      // 재귀 DFS
        all = append(all, matches...)

        if matches != nil && !cr.Continue {
            break  // Continue=false이면 다음 형제 건너뜀
        }
    }

    if len(all) == 0 {
        all = append(all, r)  // 자식 매칭 없으면 자기 자신 반환
    }

    return all
}
```

```
매칭 알고리즘:
1. 현재 Route의 Matchers가 Alert Labels와 일치하는지 확인
2. 일치하면 자식 Route들을 왼쪽→오른쪽 순서로 재귀 매칭
3. 자식 중 매칭된 것이 있으면:
   - Continue=false → 첫 매칭 후 중단
   - Continue=true → 다음 형제도 시도
4. 자식 매칭 없으면 → 자기 자신을 결과에 추가
5. 결과: 가장 깊이 매칭된 Route(들)의 목록
```

예시:

```
Route Tree:
├── root (receiver=default)              ← 항상 매칭
│   ├── severity="critical" (pager)      ← Alert에 severity=critical 있으면 매칭
│   │   └── team="infra" (infra-pager)   ← team=infra도 있으면 매칭
│   └── severity="warning" (slack)

Alert: {severity="critical", team="infra"}

DFS 과정:
1. root 매칭 ✓
2. severity="critical" 매칭 ✓ (Continue=false)
3.   team="infra" 매칭 ✓ → 결과: [infra-pager]
4. severity="warning" 건너뜀 (2번에서 break)
최종: [infra-pager]
```

### 6.5 Walk() 트리 순회

```go
// dispatch/route.go:222
func (r *Route) Walk(visit func(*Route)) {
    visit(r)
    for i := range r.Routes {
        r.Routes[i].Walk(visit)
    }
}
```

깊이 우선 선순회(pre-order). Dispatcher 초기화 시 `routeGroupsSlice` 구축에 사용된다.

## 7. groupAlert() 그룹핑

```
groupAlert(ctx, alert, route):
    1. 그룹 키 생성
       - GroupByAll → 모든 레이블의 정렬된 문자열
       - GroupBy → 지정된 레이블만 추출
       예: "alertname=HighCPU,cluster=prod"

    2. routeGroupsSlice[route.Idx]에서 그룹 조회
       - groups.Load(groupKey)

    3a. 기존 그룹 → 그룹에 Alert 추가
        ag.insert(alert)

    3b. 새 그룹:
        - Limits 확인 (MaxNumberOfAggregationGroups)
        - 새 aggrGroup 생성
        - groups.Store(groupKey, ag)
        - Dispatcher 상태가 Running이면 runAG(ag) 즉시 시작
```

## 8. Aggregation Group

### 8.1 aggrGroup 구조체

```go
// (dispatch/dispatch.go 내부)
type aggrGroup struct {
    labels    model.LabelSet     // 그룹핑 레이블
    opts      *RouteOpts         // GroupWait, GroupInterval 등
    routeID   string             // Route 고유 ID
    alerts    *store.Alerts      // 이 그룹에 속한 Alert들
    hasFlushed bool              // 첫 flush 수행 여부
    timer     *time.Timer        // flush 타이머
    // ...
}
```

### 8.2 flush 타이밍

```
┌──────────────────────────────────────────────────────┐
│                                                      │
│  Alert 도착 ──→ GroupWait(30s) ──→ 첫 flush          │
│                                      │               │
│                                GroupInterval(5m)     │
│                                      │               │
│                              후속 Alert 있으면       │
│                                    flush             │
│                                      │               │
│                              GroupInterval(5m)       │
│                                      │               │
│                                    ...               │
│                                                      │
│  Pipeline의 DedupStage에서 RepeatInterval(4h) 확인  │
│  → 변경 없어도 4시간마다 재전송                       │
│                                                      │
└──────────────────────────────────────────────────────┘
```

- **GroupWait**: 첫 알림까지 대기 시간. 짧은 시간 내 같은 그룹의 Alert를 모아서 한 번에 전송
- **GroupInterval**: 이후 flush 간격. 새 Alert가 추가된 경우에만 전송
- **RepeatInterval**: 변경 없이도 반복 전송. Pipeline의 DedupStage에서 제어

### 8.3 flush → Notification Pipeline

```
ag.flush():
    1. alerts.List() → 현재 Alert 목록 수집
    2. context에 메타데이터 추가:
       - keyGroupLabels: 그룹핑 레이블
       - keyGroupKey: 그룹 키
       - keyRouteID: Route ID
       - keyReceiverName: Receiver 이름
       - keyRepeatInterval: RepeatInterval
       - keyMuteTimeIntervals: 뮤트 시간대
       - keyActiveTimeIntervals: 활성 시간대
    3. stage.Exec(ctx, alerts...) → Pipeline 실행
    4. 결과에 따라 다음 flush 타이머 설정
```

## 9. 유지보수 (doMaintenance)

```go
// dispatch/dispatch.go:298
func (d *Dispatcher) doMaintenance() {
    for i := range d.routeGroupsSlice {
        d.routeGroupsSlice[i].groups.Range(func(_, el any) bool {
            ag := el.(*aggrGroup)
            if ag.destroyed() {
                ag.stop()
                d.marker.DeleteByGroupKey(ag.routeID, ag.GroupKey())
                deleted := d.routeGroupsSlice[i].groups.CompareAndDelete(ag.fingerprint(), ag)
                if deleted {
                    d.routeGroupsSlice[i].groupsLen.Add(-1)
                    d.aggrGroupsNum.Add(-1)
                    d.metrics.aggrGroups.Set(float64(d.aggrGroupsNum.Load()))
                }
            }
            return true
        })
    }
}
```

`maintenanceInterval` 간격으로 실행되며, 파괴된(Alert가 모두 만료된) Aggregation Group을 정리한다.

## 10. Stop() 종료

```go
// dispatch/dispatch.go:427
func (d *Dispatcher) Stop() {
    if d == nil { return }
    d.state.Store(DispatcherStateStopped)
    d.cancel()      // context 취소 → 모든 goroutine 종료
    d.finished.Wait() // 모든 goroutine 종료 대기
}
```

## 11. Groups() API 지원

```go
// dispatch/dispatch.go:346
func (d *Dispatcher) Groups(ctx context.Context,
    routeFilter func(*Route) bool,
    alertFilter func(*types.Alert, time.Time) bool,
) (AlertGroups, map[model.Fingerprint][]string, error)
```

API의 `/api/v2/alerts/groups` 엔드포인트에서 사용된다. `routeGroupsSlice`를 순회하며 각 Route의 Aggregation Group을 AlertGroup으로 변환한다.

## 12. 메트릭

```go
// dispatch/dispatch.go:52
type DispatcherMetrics struct {
    aggrGroups            prometheus.Gauge    // 활성 Aggregation Group 수
    processingDuration    prometheus.Summary  // Alert 처리 지연시간
    aggrGroupLimitReached prometheus.Counter  // 그룹 제한 도달 횟수
}
```

| 메트릭 이름 | 타입 | 설명 |
|------------|------|------|
| `alertmanager_dispatcher_aggregation_groups` | Gauge | 활성 Aggregation Group 수 |
| `alertmanager_dispatcher_alert_processing_duration_seconds` | Summary | Alert 처리 소요시간 |
| `alertmanager_dispatcher_aggregation_group_limit_reached_total` | Counter | 그룹 수 제한 도달 횟수 |

## 13. 동시성 모델 요약

```
┌──────────────────────────────────────────┐
│  Dispatcher                              │
│                                          │
│  [sync.Map] routeGroupsSlice[i].groups   │
│     └─ 동시 읽기/쓰기 안전               │
│                                          │
│  [atomic.Int32] aggrGroupsNum            │
│     └─ 락 없이 카운트                    │
│                                          │
│  [atomic.Int32] state                    │
│     └─ CAS로 상태 전환                   │
│                                          │
│  [chan struct{}] loaded                   │
│     └─ 초기 로딩 완료 신호               │
│                                          │
│  [sync.WaitGroup] finished               │
│     └─ 모든 goroutine 종료 대기          │
└──────────────────────────────────────────┘
```

핵심: `sync.Map`을 사용하여 Route별 Aggregation Group에 대한 동시 접근을 안전하게 처리한다. 이는 여러 수집 워커가 동시에 `groupAlert()`을 호출할 수 있기 때문이다.

## 14. 설정 리로드 시 Dispatcher 재생성

```
Coordinator.Reload():
    1. 기존 Dispatcher.Stop()
       - ctx 취소
       - 모든 goroutine 종료 대기
       - 모든 aggrGroup 정리

    2. 새 Route 트리 생성
       - NewRoute(config.Route, nil)

    3. 새 Dispatcher 생성
       - NewDispatcher(alerts, newRoute, newStage, ...)

    4. 새 Dispatcher.Run(dispatchStartTime)
       - 기존 Alert 재로드
       - 새 Route 트리로 재라우팅
```

기존 Aggregation Group은 모두 폐기되고, 새 Route 트리로 모든 Alert가 다시 라우팅된다.
