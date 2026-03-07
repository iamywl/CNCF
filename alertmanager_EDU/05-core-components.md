# Alertmanager 핵심 컴포넌트

## 1. 개요

Alertmanager의 핵심 컴포넌트는 크게 5가지이다:
1. **Dispatcher** — Alert 수집, 라우팅, 그룹핑
2. **Notification Pipeline** — Stage 체인으로 알림 필터링/전송
3. **Silencer** — 레이블 매칭 기반 침묵
4. **Inhibitor** — Source→Target 규칙 기반 억제
5. **Alert Provider** — 메모리 기반 Alert 저장소

## 2. Dispatcher

### 2.1 역할

Dispatcher는 Alertmanager의 중심 엔진으로, Provider로부터 Alert를 수집하여 Route 트리와 매칭하고, Aggregation Group으로 묶어 Notification Pipeline에 전달한다.

### 2.2 핵심 구조 (`dispatch/dispatch.go`)

```go
type Dispatcher struct {
    route     *Route               // 라우팅 트리 (설정에서 파싱)
    alerts    provider.Alerts      // Alert 저장소 구독
    stage     notify.Stage         // 알림 처리 파이프라인
    marker    types.GroupMarker    // 그룹 뮤트 상태 추적

    routeGroupsSlice []routeAggrGroups   // Route별 Aggregation Groups
    aggrGroupsNum    atomic.Int32        // 현재 Aggregation Group 수
    concurrency      int                 // Alert 수집 워커 수
    state            atomic.Int32        // Dispatcher 상태
}
```

### 2.3 동작 원리

```
[Provider] ──(Alert Iterator)──→ [Dispatcher.Run()]
                                      │
                                      ├─ N개 수집 워커 시작
                                      │
                                 routeAlert(alert)
                                      │
                                 Route.Match(labels)
                                      │  DFS로 라우팅 트리 탐색
                                      │
                                 groupAlert(route, alert)
                                      │
                                      ├─ group_by 레이블로 그룹 키 생성
                                      │  예: "cluster=A,alertname=High"
                                      │
                                      └─ AggregationGroup에 추가
                                           │
                                           ├─ 새 그룹: GroupWait 타이머 시작
                                           └─ 기존 그룹: Alert 추가
```

### 2.4 Aggregation Group

```go
// dispatch/dispatch.go (내부 구조)
type aggrGroup struct {
    labels   model.LabelSet    // 그룹핑 레이블
    opts     *RouteOpts        // GroupWait, GroupInterval 등
    routeID  string

    alerts   *store.Alerts     // 이 그룹에 속한 Alert들
    hasFlushed bool            // 첫 flush 여부
    timer      *time.Timer     // flush 타이머
}
```

flush 타이밍:
- **첫 번째 flush**: `GroupWait` (기본 30초) 후
- **이후 flush**: `GroupInterval` (기본 5분) 간격
- **반복 전송**: `RepeatInterval` (기본 4시간) 간격 (Pipeline의 DedupStage에서 제어)

## 3. Route 트리

### 3.1 핵심 구조 (`dispatch/route.go`)

```go
type Route struct {
    parent    *Route
    RouteOpts RouteOpts
    Matchers  labels.Matchers   // AND 로직
    Continue  bool              // true: 다음 형제도 매칭 시도
    Routes    []*Route          // 자식 Route
    Idx       int               // 고유 인덱스
}
```

### 3.2 매칭 알고리즘

`Route.Match()`는 **DFS(깊이 우선 탐색)**로 동작한다:

```
Route.Match(lset model.LabelSet) []*Route:
    1. 현재 Route의 Matchers가 lset과 매칭되는지 확인
    2. 매칭되면:
       a. 자식 Route들을 순서대로 매칭 시도
       b. 자식 중 매칭된 것이 있으면 → 그 자식 반환
       c. 자식 중 Continue=true이면 → 다음 형제도 시도
       d. 자식 없으면 → 현재 Route 반환
    3. 매칭 안 되면 → nil 반환
```

예시:

```
Alert: {alertname="HighLatency", severity="critical", team="infra"}

Route Tree:
├── root (receiver=default, group_by=[alertname])       ← 항상 매칭
│   ├── severity="critical" (receiver=pager)            ← 매칭!
│   │   └── team="infra" (receiver=infra-pager)         ← 매칭!
│   └── severity="warning" (receiver=slack)             ← 매칭 안 됨

결과: infra-pager (가장 깊은 매칭 Route)
```

## 4. Notification Pipeline

### 4.1 Stage 인터페이스 (`notify/notify.go`)

```go
type Stage interface {
    Exec(ctx context.Context, l *slog.Logger, alerts ...*types.Alert) (context.Context, []*types.Alert, error)
}

type StageFunc func(ctx context.Context, l *slog.Logger, alerts ...*types.Alert) (context.Context, []*types.Alert, error)
```

### 4.2 파이프라인 구성

Alert는 다음 Stage들을 순서대로 통과한다:

```
Alert[]
  │
  ▼
┌──────────────────┐
│ GossipSettleStage│  클러스터 안정화 대기
└────────┬─────────┘
         │
┌────────▼─────────┐
│   InhibitMute    │  Inhibition 규칙으로 필터링
│   Stage          │  (Inhibitor.Mutes)
└────────┬─────────┘
         │
┌────────▼─────────┐
│  SilenceMute     │  Silence로 필터링
│   Stage          │  (Silencer.Mutes)
└────────┬─────────┘
         │
┌────────▼─────────┐
│ TimeMuteStage    │  MuteTimeInterval로 필터링
└────────┬─────────┘
         │
┌────────▼─────────┐
│ TimeActiveStage  │  ActiveTimeInterval 확인
└────────┬─────────┘
         │
┌────────▼─────────┐
│   WaitStage      │  GroupWait/Interval 대기
└────────┬─────────┘
         │
┌────────▼─────────┐
│   DedupStage     │  nflog 참조, 중복 알림 필터링
└────────┬─────────┘
         │
┌────────▼─────────┐
│   RetryStage     │  각 Integration 호출
│                  │  실패 시 Exponential Backoff
└────────┬─────────┘
         │
┌────────▼─────────┐
│ SetNotifiesStage │  nflog에 발송 기록 저장
└──────────────────┘
```

### 4.3 Notifier 인터페이스

```go
type Notifier interface {
    Notify(context.Context, ...*types.Alert) (bool, error)
    // 반환값: (recoverable, error)
    // recoverable=true: 재시도 가능한 오류
    // recoverable=false: 영구 오류 또는 성공
}
```

### 4.4 Integration

```go
type Integration struct {
    notifier     Notifier
    rs           ResolvedSender
    name         string         // "slack", "email" 등
    idx          int            // 동일 Receiver 내 인덱스
    receiverName string         // Receiver 이름
}
```

## 5. Silencer

### 5.1 역할

Alert의 레이블이 활성 Silence의 Matcher와 일치하면 해당 Alert를 억제한다.

### 5.2 핵심 구조 (`silence/silence.go`)

```go
type Silencer struct {
    silences *Silences
    cache    *cache             // fingerprint → silence ID 매핑 캐시
    marker   types.AlertMarker
}
```

### 5.3 캐싱 메커니즘

Silencer는 성능을 위해 **버전 기반 캐시**를 사용한다:

```
Mutes(lset) 호출:
    1. lset의 Fingerprint 계산
    2. 캐시에서 (fingerprint, version) 조회
       ├── 캐시 HIT (version 동일): 이전 결과 반환
       └── 캐시 MISS (version 변경):
           a. 모든 활성 Silence 순회
           b. 각 Silence.Matchers와 lset 매칭
           c. 매칭된 Silence ID 수집
           d. 결과 캐시 저장
           e. Marker 업데이트
    3. silenceIDs가 있으면 true (뮤트)
```

Silence가 추가/삭제/만료되면 `version`이 증가하여 캐시가 자동 무효화된다.

## 6. Inhibitor

### 6.1 역할

특정 Alert(Source)가 firing 상태일 때, 관련 Alert(Target)을 자동으로 억제한다.

### 6.2 핵심 구조 (`inhibit/inhibit.go`)

```go
type Inhibitor struct {
    alerts  provider.Alerts     // Alert 구독
    rules   []*InhibitRule
    marker  types.AlertMarker
}

type InhibitRule struct {
    Name           string
    SourceMatchers labels.Matchers    // Source 조건
    TargetMatchers labels.Matchers    // Target 조건
    Equal          map[model.LabelName]struct{}  // 동일해야 할 레이블
    scache         *store.Alerts      // Source Alert 캐시
    sindex         *index             // 빠른 조회용 인덱스
}
```

### 6.3 동작 원리

```
Inhibitor.Run():
    Alert 변경 구독
    └→ 새 Alert 수신 시:
        각 Rule의 SourceMatchers와 매칭 확인
        └→ 매칭되면 scache에 저장

Inhibitor.Mutes(targetLabels):
    각 InhibitRule에 대해:
    1. TargetMatchers.Matches(targetLabels) 확인
    2. 매칭되면 scache에서 Source Alert 검색
    3. Source와 Target의 Equal 레이블 비교
    4. 모든 Equal 레이블이 동일 → 억제 (true 반환)
```

예시:

```yaml
inhibit_rules:
  - source_matchers:
      - severity="critical"
    target_matchers:
      - severity="warning"
    equal: ['alertname']
```

```
Source Alert: {alertname="HighCPU", severity="critical"}
Target Alert: {alertname="HighCPU", severity="warning"}
    → alertname이 동일하고, Source=critical, Target=warning
    → Target이 억제됨

Target Alert: {alertname="HighMemory", severity="warning"}
    → alertname이 다름 (HighCPU ≠ HighMemory)
    → Target이 억제되지 않음
```

## 7. Alert Provider (메모리)

### 7.1 역할

Alert를 인메모리에 저장하고, 변경을 구독자(Dispatcher, Inhibitor)에게 알린다.

### 7.2 핵심 구조 (`provider/mem/mem.go`)

```go
type Alerts struct {
    alerts    *store.Alerts         // 실제 저장소
    marker    types.AlertMarker     // 상태 추적
    listeners map[int]listeningAlerts  // 구독자들
    callback  AlertStoreCallback    // PreStore/PostStore/PostDelete 콜백
}
```

### 7.3 Provider 인터페이스 (`provider/provider.go`)

```go
type Alerts interface {
    Subscribe(name string) AlertIterator
    SlurpAndSubscribe(name string) ([]*types.Alert, AlertIterator)
    GetPending() AlertIterator
}
```

### 7.4 구독 모델

```
Provider
  ├── Dispatcher (SlurpAndSubscribe)
  │   └── 초기 Alert + 실시간 스트림
  │
  ├── Inhibitor (Subscribe)
  │   └── Alert 변경 감시
  │
  └── API (GetPending)
      └── 현재 Alert 스냅샷
```

`SlurpAndSubscribe`는 초기 Alert 목록과 실시간 Alert Iterator를 동시에 반환하여, Dispatcher가 기존 Alert를 놓치지 않도록 한다.

### 7.5 GC (Garbage Collection)

```
Provider GC 루프 (주기적):
    1. store.Alerts.GC() 호출
       - EndsAt이 지난 Alert 삭제
       - limit Bucket 정리
    2. PostGC 콜백
       - Marker.Delete(삭제된 fingerprints)
    3. 삭제된 Alert의 리스너 알림
```

## 8. 컴포넌트 상호작용 요약

```
┌─────────────────────────────────────────────────┐
│                  Config                          │
│  (Route, Receiver, InhibitRule, TimeInterval)   │
└──────────┬──────────────────────────────────────┘
           │ Coordinator.Subscribe()
           │
    ┌──────▼──────┐     ┌───────────┐
    │ Dispatcher  │     │ Inhibitor │
    │             │     │           │
    │ Route 트리  │     │ Rules     │
    │ → AggGroup  │     │ Source    │
    │ → flush()   │     │ Cache    │
    └──────┬──────┘     └─────┬─────┘
           │                  │
           │    ┌─────────────┤
           │    │             │
    ┌──────▼────▼──┐   ┌─────▼─────┐
    │  Notification │   │ Provider  │
    │  Pipeline     │   │ (Memory)  │
    │               │   │           │
    │ MuteStage ◄───┼───┤ Subscribe │
    │ DedupStage    │   │ GC        │
    │ RetryStage    │   └───────────┘
    └──────┬────────┘
           │
    ┌──────▼──────┐     ┌───────────┐
    │  Silences   │     │  nflog    │
    │             │◄───→│           │
    │ Matcher     │     │ 발송 기록 │
    │ Index       │     │ 중복 방지 │
    └──────┬──────┘     └─────┬─────┘
           │                  │
    ┌──────▼──────────────────▼──┐
    │        Cluster              │
    │   (Gossip / memberlist)    │
    │                            │
    │  Silences 동기화           │
    │  nflog 동기화              │
    └────────────────────────────┘
```
