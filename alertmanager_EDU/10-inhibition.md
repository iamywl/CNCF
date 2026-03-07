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
