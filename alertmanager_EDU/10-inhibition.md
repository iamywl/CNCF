# Alertmanager Inhibition Deep Dive

## 1. 개요

Inhibition은 특정 Alert(Source)가 firing 상태일 때 관련 Alert(Target)을 자동으로 억제하는 메커니즘이다. 예를 들어, `severity=critical`인 Alert가 발생하면 동일 `alertname`의 `severity=warning` Alert를 억제할 수 있다. `inhibit/inhibit.go`에 구현되어 있다.

## 2. Inhibitor 구조체

```go
// inhibit/inhibit.go
type Inhibitor struct {
    alerts     provider.Alerts          // Alert 저장소 (구독)
    rules      []*InhibitRule           // 억제 규칙 목록
    marker     types.AlertMarker        // Alert 상태 추적
    logger     *slog.Logger
    propagator propagation.TextMapPropagator

    mtx             sync.RWMutex
    loadingFinished sync.WaitGroup      // 초기 Alert 로딩 완료
    cancel          func()              // context 취소
}
```

### 2.1 InhibitRule

```go
// inhibit/inhibit.go
type InhibitRule struct {
    Name           string                          // 규칙 이름
    SourceMatchers labels.Matchers                 // Source 조건 (억제하는 Alert)
    TargetMatchers labels.Matchers                 // Target 조건 (억제되는 Alert)
    Equal          map[model.LabelName]struct{}    // 동일해야 할 레이블
    scache         *store.Alerts                   // Source Alert 캐시
    sindex         *index                          // 빠른 Equal 레이블 조회 인덱스
}
```

## 3. 생성 (NewInhibitor)

```go
// inhibit/inhibit.go
func NewInhibitor(
    ap provider.Alerts,
    rs []config.InhibitRule,
    mk types.AlertMarker,
    logger *slog.Logger,
) *Inhibitor
```

config.InhibitRule을 내부 InhibitRule로 변환한다:

```
각 config.InhibitRule에 대해:
    1. SourceMatchers 파싱
    2. TargetMatchers 파싱
    3. Equal 레이블 집합 생성
    4. Source가 Target도 되는 "자기 억제" 방지:
       - SourceMatchers == TargetMatchers이면 건너뜀
    5. scache (store.Alerts) 생성 — Source Alert 캐시
    6. sindex (index) 생성 — Equal 레이블 인덱스
```

## 4. Run() 동작

```go
// inhibit/inhibit.go
func (ih *Inhibitor) Run() {
    // 1. 초기 Alert 로드
    initialAlerts, it := ih.alerts.SlurpAndSubscribe("inhibitor")
    ih.loadingFinished.Add(1)

    // 2. 초기 Alert를 각 Rule의 scache에 저장
    for _, alert := range initialAlerts {
        for _, rule := range ih.rules {
            if rule.SourceMatchers.Matches(alert.Labels) {
                rule.scache.Set(alert)
            }
        }
    }
    ih.loadingFinished.Done()

    // 3. 실시간 Alert 변경 감시
    for alert := range it.Next() {
        for _, rule := range ih.rules {
            if rule.SourceMatchers.Matches(alert.Labels) {
                rule.scache.Set(alert)
            }
        }
    }
}
```

```
┌──────────────────────────────────────┐
│ Inhibitor.Run() goroutine            │
│                                      │
│  Provider ──(Alert Iterator)──→ 수신 │
│                                      │
│  각 Alert에 대해:                     │
│    각 InhibitRule에 대해:            │
│      if SourceMatchers.Matches(alert)│
│        → scache.Set(alert)           │
│        → sindex 업데이트             │
│                                      │
│  scache = 현재 firing Source Alert    │
│  의 실시간 스냅샷                     │
└──────────────────────────────────────┘
```

## 5. Mutes() 알고리즘

```go
// inhibit/inhibit.go
func (ih *Inhibitor) Mutes(ctx context.Context, lset model.LabelSet) bool {
    ih.loadingFinished.Wait()  // 초기 로딩 완료 대기

    ih.mtx.RLock()
    defer ih.mtx.RUnlock()

    fp := lset.Fingerprint()

    for _, rule := range ih.rules {
        // 1. Target 매칭 확인
        if !rule.TargetMatchers.Matches(lset) {
            continue
        }

        // 2. Source Alert에서 Equal 레이블이 동일한 것 찾기
        if rule.hasMatchingSources(lset) {
            ih.marker.SetInhibited(fp, /* source alert IDs */)
            return true  // 억제
        }
    }

    ih.marker.SetInhibited(fp)  // 억제 해제
    return false
}
```

### 5.1 hasMatchingSources() 상세

```
rule.hasMatchingSources(targetLabels):
    1. Equal 레이블 없으면 → Source가 있기만 하면 억제
    2. Equal 레이블 있으면:
       a. targetLabels에서 Equal 레이블 값 추출
       b. sindex에서 해당 값으로 Source Alert 조회
       c. 매칭되는 Source Alert이 firing이면 → 억제
```

### 5.2 예시 시나리오

```yaml
# 설정
inhibit_rules:
  - source_matchers:
      - severity="critical"
    target_matchers:
      - severity="warning"
    equal: ['alertname', 'cluster']
```

```
Source Alert: {alertname="HighCPU", severity="critical", cluster="prod"}
  → scache에 저장됨

Target Alert: {alertname="HighCPU", severity="warning", cluster="prod"}
  → TargetMatchers: severity="warning" ✓
  → Equal 확인: alertname="HighCPU" 동일 ✓, cluster="prod" 동일 ✓
  → Source가 firing → 억제됨 ✓

Target Alert: {alertname="HighMemory", severity="warning", cluster="prod"}
  → TargetMatchers: severity="warning" ✓
  → Equal 확인: alertname="HighMemory" ≠ "HighCPU" ✗
  → 억제되지 않음 ✗
```

## 6. index 구조체

Source Alert의 Equal 레이블 값을 인덱싱하여 빠른 조회를 지원한다:

```
index:
    equalLabelsHash → [sourceAlert1, sourceAlert2, ...]

    Equal = ['alertname', 'cluster'] 일 때:
    hash("HighCPU|prod")  → [sourceAlert1]
    hash("HighMem|dev")   → [sourceAlert2]
```

Target Alert의 Equal 레이블 값을 해싱하여 O(1)로 매칭되는 Source Alert를 찾는다.

## 7. Source Alert 캐시 (scache)

```go
// store/store.go의 Alerts 사용
type Alerts struct {
    alerts map[model.Fingerprint]*types.Alert
}
```

scache는 각 InhibitRule별로 독립적인 Alert 저장소이다:
- SourceMatchers에 매칭되는 Alert만 저장
- Alert가 resolved되면 GC에서 제거
- 실시간으로 업데이트됨

## 8. 동시성

```
┌─────────────────────────────────────┐
│        동시성 제어                    │
│                                     │
│  [sync.RWMutex] ih.mtx              │
│    Write: Run()에서 scache 업데이트  │
│    Read: Mutes()에서 scache 조회     │
│                                     │
│  [sync.WaitGroup] loadingFinished   │
│    Mutes()가 초기 로딩 완료 전       │
│    호출되면 대기                     │
│                                     │
│  ┌─────────┐     ┌─────────┐       │
│  │  Run()  │     │ Mutes() │       │
│  │ goroutine│    │ 호출    │       │
│  │         │     │         │       │
│  │ Write   │     │ Read    │       │
│  │ Lock    │     │ Lock    │       │
│  └─────────┘     └─────────┘       │
└─────────────────────────────────────┘
```

## 9. 설정 리로드

```
Coordinator.Reload():
    1. 기존 Inhibitor.Stop()
       - cancel() → Run() goroutine 종료
    2. 새 Inhibitor 생성
       - NewInhibitor(alerts, newRules, marker, logger)
    3. 새 Inhibitor.Run() 시작
       - 새 규칙으로 Source Alert 재로드
```

규칙이 변경되면 Inhibitor가 완전히 재생성된다.

## 10. Edge Cases

### 10.1 Equal 레이블이 비어있는 경우

```yaml
inhibit_rules:
  - source_matchers:
      - severity="critical"
    target_matchers:
      - severity="warning"
    # equal 없음
```

Equal이 없으면 Source Alert가 하나라도 있으면 **모든** Target Alert가 억제된다.

### 10.2 Equal 레이블이 Source/Target 모두에 없는 경우

Equal에 지정된 레이블이 Source와 Target 모두에서 누락되면, 두 Alert의 해당 레이블 값이 동일한 것(빈 문자열)으로 간주되어 **억제가 적용된다**.

### 10.3 자기 억제 방지

SourceMatchers와 TargetMatchers가 동일하면 Alert가 자기 자신을 억제할 수 있다. NewInhibitor에서 이를 감지하고 규칙을 건너뛴다.

## 11. 메트릭

Inhibitor 자체는 별도 메트릭을 노출하지 않지만, `types.AlertMarker`의 `alertmanager_marked_alerts` 메트릭에서 `state=suppressed`로 억제된 Alert 수를 확인할 수 있다.

## 12. Pipeline에서의 위치

```
Notification Pipeline:
    ┌──────────────────────┐
    │ GossipSettleStage    │
    └──────────┬───────────┘
    ┌──────────▼───────────┐
    │ MuteStage            │ ← Inhibitor.Mutes() 호출
    │ (Inhibitor)          │    Target 매칭 → Source 조회 → Equal 비교
    └──────────┬───────────┘
    ┌──────────▼───────────┐
    │ MuteStage            │ ← Silencer.Mutes() 호출
    │ (Silencer)           │
    └──────────┬───────────┘
               │
               ▼
         (이후 Stage...)
```

Inhibitor는 Silencer보다 먼저 실행된다. 이는 Inhibition이 Silence보다 우선순위가 높다는 의미가 아니라, Pipeline 순서상 먼저 확인될 뿐이다.

## 13. 실제 소스 코드 심화 분석

### 13.1 index 구조체 상세

```go
// inhibit/index.go
type index struct {
    mtx   sync.RWMutex
    items map[model.Fingerprint]model.Fingerprint
}

func newIndex() *index {
    return &index{
        items: make(map[model.Fingerprint]model.Fingerprint),
    }
}

func (c *index) Get(key model.Fingerprint) (model.Fingerprint, bool) {
    c.mtx.RLock()
    defer c.mtx.RUnlock()
    fp, ok := c.items[key]
    return fp, ok
}

func (c *index) Set(key, value model.Fingerprint) {
    c.mtx.Lock()
    defer c.mtx.Unlock()
    c.items[key] = value
}

func (c *index) Delete(key model.Fingerprint) {
    c.mtx.Lock()
    defer c.mtx.Unlock()
    delete(c.items, key)
}
```

**왜 index에 별도 RWMutex가 필요한가?**

Inhibitor의 `mtx`는 rules 전체를 보호하지만, `sindex`는 규칙별로 독립적이다. `Mutes()`에서 `findEqualSourceAlert()`를 호출할 때 index를 읽고, `processAlert()`에서 `updateIndex()`를 호출할 때 index를 쓴다. 이 두 경로가 동시에 실행되므로 index 자체적으로 동기화가 필요하다.

### 13.2 fingerprintEquals() — Equal 레이블 해싱

```go
// inhibit/inhibit.go (318-324행)
func (r *InhibitRule) fingerprintEquals(lset model.LabelSet) model.Fingerprint {
    equalSet := model.LabelSet{}
    for n := range r.Equal {
        equalSet[n] = lset[n]
    }
    return equalSet.Fingerprint()
}
```

이 함수는 Alert의 레이블 중 Equal에 지정된 것만 추출하여 fingerprint를 계산한다. 이 fingerprint가 index의 키로 사용되어, Target Alert가 동일한 Equal 레이블 값을 가진 Source Alert를 O(1)로 찾을 수 있다.

### 13.3 updateIndex() — 인덱스 업데이트 전략

```go
// inhibit/inhibit.go (327-358행)
func (r *InhibitRule) updateIndex(alert *types.Alert) {
    fp := alert.Fingerprint()
    eq := r.fingerprintEquals(alert.Labels)

    indexed, ok := r.sindex.Get(eq)
    if !ok {
        r.sindex.Set(eq, fp)  // 새로 추가
        return
    }
    if indexed == fp {
        return  // 동일한 Alert — 업데이트 불필요
    }

    // 기존과 다른 Alert → EndsAt 비교
    existing, err := r.scache.Get(indexed)
    if err != nil {
        r.sindex.Set(eq, fp)  // 기존 Alert를 찾을 수 없으면 덮어쓰기
        return
    }

    if existing.ResolvedAt(alert.EndsAt) {
        r.sindex.Set(eq, fp)  // 새 Alert가 더 오래 지속 → 교체
    }
    // 기존이 더 오래 지속 → 유지
}
```

**왜 EndsAt 비교로 교체를 결정하는가?**

동일한 Equal 레이블 값을 가진 Source Alert가 여러 개 있을 수 있다. index는 하나의 값만 저장하므로, **가장 오래 유효한 Alert**를 유지한다. 이렇게 하면 짧은 Alert가 먼저 resolved되어도 longer-running Alert가 계속 inhibition을 유지한다.

### 13.4 gcCallback — 인덱스 정리

```go
// inhibit/inhibit.go (380-385행)
func (r *InhibitRule) gcCallback(alerts []*types.Alert) {
    for _, a := range alerts {
        fp := r.fingerprintEquals(a.Labels)
        r.sindex.Delete(fp)
    }
}
```

`store.Alerts`의 GC가 만료된 Alert를 삭제하면 이 콜백이 호출된다. 삭제된 Alert의 Equal fingerprint를 index에서도 제거하여 stale 인덱스 엔트리를 방지한다.

### 13.5 hasEqual() — 양쪽 매칭 배제

```go
// inhibit/inhibit.go (391-401행)
func (r *InhibitRule) hasEqual(lset model.LabelSet, excludeTwoSidedMatch bool, now time.Time) (model.Fingerprint, bool) {
    equal, found := r.findEqualSourceAlert(lset, now)
    if found {
        if excludeTwoSidedMatch && r.TargetMatchers.Matches(equal.Labels) {
            return model.Fingerprint(0), false  // Source가 Target도 매칭 → 억제 안 함
        }
        return equal.Fingerprint(), found
    }
    return model.Fingerprint(0), false
}
```

**왜 양쪽 매칭을 배제하는가?**

Alert가 SourceMatchers와 TargetMatchers 모두에 매칭되면, 이 Alert이 자기 자신을 억제할 수 있다. 예를 들어 `source_matchers: [severity="critical"]`, `target_matchers: [severity="critical"]`이면 모든 critical Alert이 다른 critical Alert를 억제하게 된다. `excludeTwoSidedMatch`가 true이면, Source Alert가 동시에 Target 조건도 만족하는 경우 억제를 하지 않는다.

---

## 14. OpenTelemetry 트레이싱 통합

```go
// inhibit/inhibit.go (108-134행)
func (ih *Inhibitor) processAlert(ctx context.Context, a *types.Alert) {
    _, span := tracer.Start(ctx, "inhibit.Inhibitor.processAlert",
        trace.WithAttributes(
            attribute.String("alerting.alert.name", a.Name()),
            attribute.String("alerting.alert.fingerprint", a.Fingerprint().String()),
        ),
        trace.WithSpanKind(trace.SpanKindInternal),
    )
    defer span.End()

    for _, r := range ih.rules {
        if r.SourceMatchers.Matches(a.Labels) {
            attr := attribute.String("alerting.inhibit_rule.name", r.Name)
            span.AddEvent("alert matched rule source", trace.WithAttributes(attr))
            if err := r.scache.Set(a); err != nil {
                span.SetStatus(codes.Error, "error on set alert")
                span.RecordError(err)
                continue
            }
            span.SetAttributes(attr)
            r.updateIndex(a)
        }
    }
}
```

**왜 트레이싱이 중요한가?**

Inhibitor는 모든 Alert 변경에 대해 실행되므로, 대량 Alert 환경에서 성능 병목이 될 수 있다. 트레이싱으로 각 `processAlert` 호출의 지연시간을 측정하고, 어떤 규칙에서 시간이 걸리는지 식별할 수 있다.

---

## 15. Run() — oklog/run.Group 활용

```go
// inhibit/inhibit.go (141-166행)
func (ih *Inhibitor) Run() {
    var (
        g   run.Group
        ctx context.Context
    )

    ih.mtx.Lock()
    ctx, ih.cancel = context.WithCancel(context.Background())
    ih.mtx.Unlock()
    runCtx, runCancel := context.WithCancel(ctx)

    // 각 규칙의 scache GC 루프 시작
    for _, rule := range ih.rules {
        go rule.scache.Run(runCtx, 15*time.Minute)
    }

    g.Add(func() error {
        ih.run(runCtx)
        return nil
    }, func(err error) {
        runCancel()
    })

    if err := g.Run(); err != nil {
        ih.logger.Warn("error running inhibitor", "err", err)
    }
}
```

**왜 `oklog/run.Group`을 사용하는가?**

`run.Group`은 여러 goroutine의 생명주기를 관리한다. 하나의 goroutine이 종료되면 다른 goroutine도 interrupt 함수를 통해 종료된다. 이 패턴으로 Inhibitor의 run goroutine과 scache GC goroutine의 정리가 보장된다.

---

## 16. 벤치마크 분석

```go
// inhibit/inhibit_bench_test.go (36-79행)
func BenchmarkMutes(b *testing.B) {
    b.Run("1 inhibition rule, 1 inhibiting alert", func(b *testing.B) { ... })
    b.Run("100 inhibition rules, 1000 inhibiting alerts", func(b *testing.B) { ... })
    b.Run("10000 inhibition rules, last rule matches", func(b *testing.B) { ... })
}
```

벤치마크는 두 가지 시나리오를 테스트한다:

| 시나리오 | 측정 대상 |
|---------|----------|
| `allRulesMatch` | 모든 규칙이 매칭되는 최선 경우 (첫 규칙에서 억제) |
| `lastRuleMatches` | 마지막 규칙만 매칭되는 최악 경우 (모든 규칙 순회) |

**성능 특성**: `sindex`의 O(1) 조회 덕분에, Source Alert 수가 증가해도 `hasEqual` 성능은 일정하다. 반면, 규칙 수가 증가하면 `Mutes()`에서 모든 규칙을 순회해야 하므로 O(n) 특성을 보인다.

---

## 17. 테스트 전략

### 17.1 fakeAlerts — 테스트용 Alert Provider

```go
// inhibit/inhibit_test.go (380-447행)
type fakeAlerts struct {
    alerts   []*types.Alert
    finished chan struct{}
}

func (f *fakeAlerts) SlurpAndSubscribe(name string) ([]*types.Alert, provider.AlertIterator) {
    ch := make(chan *provider.Alert)
    done := make(chan struct{})
    go func() {
        for _, a := range f.alerts {
            ch <- &provider.Alert{Data: a, Header: map[string]string{}}
        }
        // 의미 없는 Alert 추가 전송 → 모든 Alert 처리 완료 보장
        ch <- &provider.Alert{Data: &types.Alert{...}, Header: map[string]string{}}
        close(f.finished)
        <-done
    }()
    return []*types.Alert{}, provider.NewAlertIterator(ch, done, nil)
}
```

**왜 "의미 없는 Alert"을 추가로 전송하는가?**

Inhibitor는 Alert를 비동기로 처리한다. 테스트에서 모든 실제 Alert가 처리되었는지 확인하려면, 마지막 Alert 이후에 "센티넬" 메시지를 보내고 이것이 처리될 때까지 기다린다. `f.finished` 채널이 닫히면 모든 Alert가 처리된 것이 보장된다.

### 17.2 통합 테스트 패턴

```go
// inhibit/inhibit_test.go (449-559행)
func TestInhibit(t *testing.T) {
    // 시나리오 1: Source Alert 없음 → Target 억제 안 됨
    // 시나리오 2: Source Alert firing → Target 억제됨
    // 시나리오 3: Source Alert resolved → Target 억제 해제
}
```

3단계 시나리오로 Inhibitor의 전체 생명주기를 테스트한다: Alert 없음 → Alert 발생(억제) → Alert 해제(억제 해제).
