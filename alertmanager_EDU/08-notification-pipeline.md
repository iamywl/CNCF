# Alertmanager 알림 파이프라인 Deep Dive

## 1. 개요

Notification Pipeline은 Aggregation Group이 flush할 때 실행되는 Stage 체인이다. Alert를 필터링(Silence, Inhibition, 시간 간격), 중복 제거(nflog), 전송(Integration), 기록(SetNotifies) 순서로 처리한다. `notify/notify.go`와 `notify/mute.go`에 구현되어 있다.

## 2. Stage 인터페이스

```go
// notify/notify.go
type Stage interface {
    Exec(ctx context.Context, l *slog.Logger, alerts ...*types.Alert) (context.Context, []*types.Alert, error)
}

type StageFunc func(ctx context.Context, l *slog.Logger, alerts ...*types.Alert) (context.Context, []*types.Alert, error)

func (f StageFunc) Exec(ctx context.Context, l *slog.Logger, alerts ...*types.Alert) (context.Context, []*types.Alert, error) {
    return f(ctx, l, alerts...)
}
```

모든 Stage는 `Exec()`를 구현하며, Alert 목록을 받아 필터링/처리 후 다음 Stage에 전달한다.

## 3. Pipeline 구조

### 3.1 MultiStage (순차 실행)

```go
// notify/notify.go
type MultiStage []Stage

func (ms MultiStage) Exec(ctx context.Context, l *slog.Logger, alerts ...*types.Alert) (context.Context, []*types.Alert, error) {
    var err error
    for _, s := range ms {
        if len(alerts) == 0 {
            return ctx, nil, nil
        }
        ctx, alerts, err = s.Exec(ctx, l, alerts...)
        if err != nil {
            return ctx, nil, err
        }
    }
    return ctx, alerts, nil
}
```

Stage를 **순서대로** 실행한다. Alert가 0개가 되면 조기 종료한다.

### 3.2 FanoutStage (병렬 실행)

```go
// notify/notify.go
type FanoutStage []Stage

func (fs FanoutStage) Exec(ctx context.Context, l *slog.Logger, alerts ...*types.Alert) (context.Context, []*types.Alert, error) {
    // 각 Stage를 goroutine으로 병렬 실행
    // 모든 Stage 완료 후 결과 합산
}
```

여러 Receiver에게 동시에 알림을 전송할 때 사용한다.

### 3.3 RoutingStage (Receiver별 라우팅)

```go
// notify/notify.go
type RoutingStage map[string]Stage

func (rs RoutingStage) Exec(ctx context.Context, l *slog.Logger, alerts ...*types.Alert) (context.Context, []*types.Alert, error) {
    receiver, ok := ReceiverName(ctx)
    if !ok {
        return ctx, nil, errors.New("receiver missing")
    }
    s, ok := rs[receiver]
    if !ok {
        return ctx, nil, errors.New("stage for receiver not found")
    }
    return s.Exec(ctx, l, alerts...)
}
```

Context에서 Receiver 이름을 꺼내어 해당 Receiver의 Stage 체인으로 라우팅한다.

## 4. 전체 Pipeline 구성

```
Alert[] (Aggregation Group flush)
  │
  ▼
┌────────────────────────┐
│ RoutingStage           │  Receiver 이름으로 Stage 체인 선택
└──────────┬─────────────┘
           │
┌──────────▼─────────────┐
│ MultiStage             │  순차 실행
│                        │
│ ┌────────────────────┐ │
│ │ GossipSettleStage  │ │  클러스터 안정화 대기
│ └────────┬───────────┘ │
│ ┌────────▼───────────┐ │
│ │ MuteStage          │ │  Inhibition 필터링
│ │ (Inhibitor)        │ │
│ └────────┬───────────┘ │
│ ┌────────▼───────────┐ │
│ │ MuteStage          │ │  Silence 필터링
│ │ (Silencer)         │ │
│ └────────┬───────────┘ │
│ ┌────────▼───────────┐ │
│ │ TimeMuteStage      │ │  MuteTimeInterval 필터링
│ └────────┬───────────┘ │
│ ┌────────▼───────────┐ │
│ │ TimeActiveStage    │ │  ActiveTimeInterval 확인
│ └────────┬───────────┘ │
│                        │
│ ┌────────▼───────────┐ │
│ │ FanoutStage        │ │  각 Integration 병렬 실행
│ │                    │ │
│ │ ┌────────────────┐ │ │
│ │ │ MultiStage     │ │ │  Integration 1 (Slack)
│ │ │ ├─ WaitStage   │ │ │
│ │ │ ├─ DedupStage  │ │ │
│ │ │ ├─ RetryStage  │ │ │
│ │ │ └─ SetNotifies │ │ │
│ │ └────────────────┘ │ │
│ │ ┌────────────────┐ │ │
│ │ │ MultiStage     │ │ │  Integration 2 (Email)
│ │ │ ├─ WaitStage   │ │ │
│ │ │ ├─ DedupStage  │ │ │
│ │ │ ├─ RetryStage  │ │ │
│ │ │ └─ SetNotifies │ │ │
│ │ └────────────────┘ │ │
│ └────────────────────┘ │
└────────────────────────┘
```

## 5. 각 Stage 상세

### 5.1 GossipSettleStage

```go
// notify/notify.go
type GossipSettleStage struct {
    peer Peer
}

func (n *GossipSettleStage) Exec(ctx context.Context, ...) {
    n.peer.WaitReady(ctx)  // 클러스터 안정화 대기
}
```

클러스터 모드에서 피어들이 연결을 완료할 때까지 대기한다. 시작 직후 알림 중복을 방지한다.

### 5.2 MuteStage

```go
// notify/mute.go
type Muter interface {
    Mutes(ctx context.Context, lset model.LabelSet) bool
}

type MuteStage struct {
    muter   Muter
    metrics *Metrics
}

func (n *MuteStage) Exec(ctx context.Context, logger *slog.Logger, alerts ...*types.Alert) (context.Context, []*types.Alert, error) {
    var dominated []*types.Alert
    for _, a := range alerts {
        if !n.muter.Mutes(ctx, a.Labels) {
            dominated = append(dominated, a)
        }
    }
    return ctx, dominated, nil
}
```

`Muter` 인터페이스를 통해 Alert를 필터링한다. Silencer와 Inhibitor 모두 `Muter`를 구현한다.

### 5.3 WaitStage

GroupWait/GroupInterval이 만료된 후의 대기를 처리한다.

### 5.4 DedupStage

```go
// notify/notify.go
type DedupStage struct {
    nflog NotificationLog
    recv  *nflogpb.Receiver
    now   func() time.Time
    hash  func(*types.Alert) uint64
}
```

nflog에서 이전 발송 기록을 조회하여 중복 알림을 필터링한다:

```
DedupStage.Exec():
    1. nflog.Query(receiver, groupKey) → 이전 발송 기록
    2. 이전에 전송한 Alert와 현재 Alert 비교:
       - firing Alert가 변경되었는지
       - resolved Alert가 새로 추가되었는지
       - RepeatInterval이 지났는지
    3. 변경 없고 RepeatInterval 미경과 → Alert 필터링 (빈 목록 반환)
    4. 변경 있으면 → 통과
```

### 5.5 RetryStage

```go
// notify/notify.go
type RetryStage struct {
    integration Integration
    groupName   string
    metrics     *Metrics
    labelValues []string
}
```

Integration의 `Notify()`를 호출하며, 실패 시 재시도한다:

```
RetryStage.Exec():
    1. integration.Notify(ctx, alerts...)
    2. 결과 확인:
       - (false, nil) → 성공
       - (true, error) → recoverable 오류 → Exponential Backoff 재시도
       - (false, error) → permanent 오류 → 재시도 중단, 에러 로깅
    3. 메트릭 기록:
       - numNotifications.Inc()
       - notificationLatencySeconds.Observe()
       - 실패 시 numTotalFailedNotifications.Inc()
```

### 5.6 SetNotifiesStage

```go
// notify/notify.go
type SetNotifiesStage struct {
    nflog NotificationLog
    recv  *nflogpb.Receiver
}
```

알림 전송 성공 후 nflog에 발송 기록을 저장한다:

```
SetNotifiesStage.Exec():
    1. firing/resolved Alert의 Fingerprint 해시 수집
    2. nflog.Log(receiver, groupKey, firingAlerts, resolvedAlerts, store, expiry)
    3. 클러스터 모드에서는 broadcast로 다른 인스턴스에 전파
```

## 6. Integration과 Notifier

### 6.1 Notifier 인터페이스

```go
// notify/notify.go
type Notifier interface {
    Notify(context.Context, ...*types.Alert) (bool, error)
}
```

반환값:
- `(false, nil)` — 성공
- `(true, error)` — 재시도 가능한 오류 (네트워크 일시 장애 등)
- `(false, error)` — 영구 오류 (잘못된 설정, 인증 실패 등)

### 6.2 Integration 구조체

```go
// notify/notify.go
type Integration struct {
    notifier     Notifier
    rs           ResolvedSender
    name         string         // "slack", "email", "pagerduty" 등
    idx          int            // 동일 Receiver 내 인덱스
    receiverName string         // Receiver 이름
}

type ResolvedSender interface {
    SendResolved() bool
}
```

`SendResolved()`가 false이면 resolved Alert를 전송하지 않는다.

### 6.3 지원 Integration 목록

| Integration | 패키지 |
|-------------|--------|
| Slack | `notify/impl/slack/` |
| Email | `notify/impl/email/` |
| PagerDuty | `notify/impl/pagerduty/` |
| Webhook | `notify/impl/webhook/` |
| OpsGenie | `notify/impl/opsgenie/` |
| Discord | `notify/impl/discord/` |
| MS Teams | `notify/impl/msteams/` |
| Telegram | `notify/impl/telegram/` |
| SNS | `notify/impl/sns/` |
| Jira | `notify/impl/jira/` |
| Pushover | `notify/impl/pushover/` |
| VictorOps | `notify/impl/victorops/` |
| Wechat | `notify/impl/wechat/` |
| Webex | `notify/impl/webex/` |
| Rocketchat | `notify/impl/rocketchat/` |

## 7. Context 키와 데이터 전달

```go
// notify/notify.go
const (
    keyReceiverName        // Receiver 이름
    keyRepeatInterval      // 반복 전송 간격
    keyGroupLabels         // 그룹핑 레이블
    keyGroupKey            // 그룹 키
    keyFiringAlerts        // firing Alert Fingerprint 목록
    keyResolvedAlerts      // resolved Alert Fingerprint 목록
    keyNow                 // 현재 시간
    keyMuteTimeIntervals   // 뮤트 시간대 이름 목록
    keyActiveTimeIntervals // 활성 시간대 이름 목록
    keyRouteID             // Route ID
    keyNflogStore          // nflog Store (key-value)
    keyNotificationReason  // 알림 사유 ("new", "resolved", "repeat" 등)
)
```

Context를 통해 Pipeline 전체에서 메타데이터를 공유한다. Dispatcher의 `flush()`에서 설정되고, 각 Stage에서 읽는다.

## 8. 메트릭

```go
// notify/notify.go
type Metrics struct {
    numNotifications             *prometheus.CounterVec
    numTotalFailedNotifications  *prometheus.CounterVec
    numNotificationRequestsTotal *prometheus.CounterVec
    numNotificationRequestsFailedTotal *prometheus.CounterVec
    notificationLatencySeconds   *prometheus.HistogramVec
    // ...
}
```

| 메트릭 | 레이블 | 설명 |
|--------|--------|------|
| `alertmanager_notifications_total` | integration | 전체 알림 전송 수 |
| `alertmanager_notifications_failed_total` | integration, reason | 실패한 알림 수 |
| `alertmanager_notification_latency_seconds` | integration | 알림 전송 지연시간 |
| `alertmanager_notification_requests_total` | integration | HTTP 요청 수 |
| `alertmanager_notification_requests_failed_total` | integration | HTTP 요청 실패 수 |

## 9. Pipeline 재구축

설정 리로드 시 Pipeline이 완전히 재구축된다:

```
Coordinator.Reload():
    1. 템플릿 재로드
    2. 각 Receiver에 대해:
       - Integration 목록 생성
       - MultiStage 구축:
         [WaitStage, DedupStage, RetryStage, SetNotifiesStage]
       - FanoutStage로 Integration들 묶기
    3. RoutingStage 구축: map[receiverName]Stage
    4. 전역 MuteStage 추가 (Inhibitor, Silencer, TimeInterval)
    5. Dispatcher에 새 Pipeline 주입
```

## 10. 에러 처리 패턴

```
RetryStage 에러 처리:
    ┌───────────────────────┐
    │ Notify() 호출         │
    └─────────┬─────────────┘
              │
    ┌─────────▼─────────────┐
    │ (recoverable, err)    │
    └─────────┬─────────────┘
              │
    ┌─────────▼─────────────────┐
    │ recoverable=true & err!=nil│
    │ → Backoff 재시도           │
    │   min: 100ms              │
    │   max: 5min               │
    │   factor: 2               │
    └─────────┬─────────────────┘
              │
    ┌─────────▼─────────────────┐
    │ recoverable=false & err!=nil│
    │ → 즉시 실패, 로깅         │
    └─────────┬─────────────────┘
              │
    ┌─────────▼─────────────────┐
    │ err==nil                   │
    │ → 성공                     │
    └───────────────────────────┘
```
