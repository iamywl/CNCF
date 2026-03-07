# Alertmanager 데이터 모델

## 1. 개요

Alertmanager의 핵심 데이터 모델은 **Alert**, **Silence**, **Config**(Route, Receiver, InhibitRule) 세 축으로 구성된다. 모든 데이터는 메모리에 저장되며, Silence와 Notification Log만 디스크 스냅샷으로 영속화된다.

## 2. Alert 모델

### 2.1 기본 Alert 구조

`alert/alert.go`에서 정의된 Alert 타입:

```go
// alert/alert.go
type Alert struct {
    model.Alert               // Prometheus 공통 Alert 모델 포함
    UpdatedAt time.Time       // 마지막 업데이트 타임스탬프
    Timeout   bool            // 타임아웃으로 해제(resolved) 여부
}
```

Prometheus `model.Alert`(github.com/prometheus/common/model)이 내장되어 있다:

```go
// prometheus/common/model
type Alert struct {
    Labels       LabelSet  `json:"labels"`
    Annotations  LabelSet  `json:"annotations"`
    StartsAt     time.Time `json:"startsAt,omitempty"`
    EndsAt       time.Time `json:"endsAt,omitempty"`
    GeneratorURL string    `json:"generatorURL"`
}
```

### 2.2 Fingerprint

Alert의 고유 식별자는 **Fingerprint**이다. Labels의 해시값으로 생성된다:

```go
// prometheus/common/model
type Fingerprint uint64

func (ls LabelSet) Fingerprint() Fingerprint
```

동일한 Labels를 가진 Alert는 동일한 Fingerprint를 가지므로, 새 Alert가 기존 Alert를 업데이트하는 방식으로 **중복 제거**가 이루어진다.

### 2.3 AlertSlice

```go
// alert/alert.go
type AlertSlice []*Alert

func (as AlertSlice) Less(i, j int) bool  // StartsAt, EndsAt, Fingerprint 순 정렬
func (as AlertSlice) Swap(i, j int)
func (as AlertSlice) Len() int
```

### 2.4 Alert 상태

`types/types.go`에서 Alert의 상태를 추적한다:

```go
// types/types.go
type AlertState int

const (
    AlertStateUnprocessed AlertState = iota  // 아직 처리되지 않음
    AlertStateActive                          // 활성 (firing)
    AlertStateSuppressed                      // 억제됨 (silenced/inhibited)
)

type AlertStatus struct {
    State       AlertState
    SilencedBy  []string    // Silence ID 목록
    InhibitedBy []string    // 억제하는 Alert의 fingerprint
}
```

### 2.5 AlertMarker 인터페이스

```go
// types/types.go
type AlertMarker interface {
    SetActiveOrSilenced(alert model.Fingerprint, activeSilenceIDs []string)
    SetInhibited(alert model.Fingerprint, alertIDs ...string)
    Count(...AlertState) int
    Status(model.Fingerprint) AlertStatus
    Delete(...model.Fingerprint)
    Unprocessed(model.Fingerprint) bool
    Active(model.Fingerprint) bool
    Silenced(model.Fingerprint) (activeIDs []string, silenced bool)
    Inhibited(model.Fingerprint) ([]string, bool)
}
```

`MemMarker`가 이를 구현하며, 내부적으로 `map[model.Fingerprint]*AlertStatus`를 유지한다.

### 2.6 GroupMarker 인터페이스

Aggregation Group의 뮤트 상태를 추적한다:

```go
// types/types.go
type GroupMarker interface {
    Muted(routeID, groupKey string) ([]string, bool)
    SetMuted(routeID, groupKey string, timeIntervalNames []string)
    DeleteByGroupKey(routeID, groupKey string)
}
```

## 3. Provider Alert 래퍼

```go
// provider/provider.go
type Alert struct {
    Header map[string]string   // 메타데이터 (tracing 정보)
    Data   *types.Alert        // 실제 Alert
}
```

Provider 계층에서는 tracing 정보를 `Header`에 담아 전달한다.

## 4. Silence 모델

### 4.1 Silence Protobuf 정의

Silence는 Protocol Buffers로 정의되어 클러스터 간 직렬화에 사용된다:

```
// silence/silencepb/silence.proto (개념)
message Silence {
    string id = 1;
    repeated MatcherSet matcher_sets = 2;
    google.protobuf.Timestamp starts_at = 3;
    google.protobuf.Timestamp ends_at = 4;
    google.protobuf.Timestamp updated_at = 5;
    string created_by = 6;
    string comment = 7;
}
```

### 4.2 Silences 저장소

```go
// silence/silence.go
type Silences struct {
    clock     quartz.Clock
    retention time.Duration     // 만료 후 보관 기간

    mtx       sync.RWMutex
    st        state             // 내부 상태 맵
    version   int               // 변경 추적 (캐시 무효화)
    broadcast func([]byte)      // 클러스터 브로드캐스트
    mi        matcherIndex      // 레이블 매칭 인덱스
    vi        versionIndex      // 버전 인덱스
}

type Limits struct {
    MaxSilences     func() int    // 최대 Silence 수
    MaxSilenceSizeBytes func() int // 최대 Silence 크기
}
```

### 4.3 Silencer (Muter 구현)

```go
// silence/silence.go
type Silencer struct {
    silences *Silences
    cache    *cache              // fingerprint → silence ID 캐시
    marker   types.AlertMarker
}

// Muter 인터페이스 구현
func (s *Silencer) Mutes(ctx context.Context, lset model.LabelSet) bool
```

## 5. Config 데이터 모델

### 5.1 최상위 Config

```go
// config/config.go
type Config struct {
    Global            *GlobalConfig
    Route             *Route
    InhibitRules      []InhibitRule
    Receivers         []Receiver
    Templates         []string
    MuteTimeIntervals []MuteTimeInterval   // deprecated
    TimeIntervals     []TimeInterval
    TracingConfig     tracing.TracingConfig
    original          string               // 원본 YAML
}
```

### 5.2 GlobalConfig

```go
// config/config.go
type GlobalConfig struct {
    ResolveTimeout model.Duration    // Alert 자동 해제 시간 (기본 5분)
    HTTPConfig     *commoncfg.HTTPClientConfig

    // SMTP
    SMTPFrom         string
    SMTPSmarthost    config.HostPort
    SMTPAuthUsername  string
    // ...

    // Slack, PagerDuty, OpsGenie 등 전역 API 설정
    SlackAPIURL       *config.SecretURL
    PagerdutyURL      *config.URL
    OpsGenieAPIURL    *config.URL
    // ...
}
```

### 5.3 Route (라우팅 트리)

```go
// config/config.go
type Route struct {
    Receiver          string
    GroupByStr         []string              // YAML에서 파싱
    GroupBy            []model.LabelName     // 그룹핑 레이블
    GroupByAll         bool                  // '...'으로 설정 시
    Matchers           Matchers              // 레이블 매칭 조건
    Continue           bool                  // 다음 Route도 계속 매칭
    Routes             []*Route              // 자식 Route
    GroupWait          *model.Duration       // 그룹화 대기 (기본 30s)
    GroupInterval      *model.Duration       // 그룹 알림 간격 (기본 5m)
    RepeatInterval     *model.Duration       // 반복 간격 (기본 4h)
    MuteTimeIntervals  []string              // 뮤트 시간대 이름
    ActiveTimeIntervals []string             // 활성 시간대 이름
}
```

**라우팅 트리 예시**:

```yaml
route:                           # 루트 Route (항상 매칭)
  receiver: 'default'
  group_by: ['alertname']
  routes:
    - matchers:                  # 자식 Route 1
        - severity="critical"
      receiver: 'pager'
      routes:
        - matchers:              # 손자 Route
            - team="infra"
          receiver: 'infra-pager'
    - matchers:                  # 자식 Route 2
        - severity="warning"
      receiver: 'slack'
```

대응되는 내부 트리:

```
Route[root: receiver=default, group_by=[alertname]]
  ├── Route[severity="critical", receiver=pager]
  │     └── Route[team="infra", receiver=infra-pager]
  └── Route[severity="warning", receiver=slack]
```

### 5.4 Receiver

```go
// config/config.go
type Receiver struct {
    Name             string
    EmailConfigs     []*EmailConfig
    PagerdutyConfigs []*PagerdutyConfig
    SlackConfigs     []*SlackConfig
    WebhookConfigs   []*WebhookConfig
    OpsGenieConfigs  []*OpsGenieConfig
    WechatConfigs    []*WechatConfig
    PushoverConfigs  []*PushoverConfig
    VictorOpsConfigs []*VictorOpsConfig
    SNSConfigs       []*SNSConfig
    TelegramConfigs  []*TelegramConfig
    DiscordConfigs   []*DiscordConfig
    WebexConfigs     []*WebexConfig
    MSTeamsConfigs   []*MSTeamsConfig
    MSTeamsV2Configs []*MSTeamsV2Config
    JiraConfigs      []*JiraConfig
    RocketchatConfigs []*RocketchatConfig
    SMTPConfigs      []*SMTPConfig
}
```

하나의 Receiver에 여러 Integration을 동시에 설정할 수 있다.

### 5.5 InhibitRule

```go
// config/config.go
type InhibitRule struct {
    Name            string
    SourceMatchers  Matchers    // 억제 출발지 조건
    TargetMatchers  Matchers    // 억제 대상 조건
    Equal           []string    // 동일해야 할 레이블 이름
}
```

### 5.6 TimeInterval

```go
// config/config.go
type TimeInterval struct {
    Name          string
    TimeIntervals []timeinterval.TimeInterval
}
```

## 6. Dispatch 데이터 모델

### 6.1 내부 Route (dispatch)

설정의 Route를 파싱한 내부 표현:

```go
// dispatch/route.go
type Route struct {
    parent    *Route
    RouteOpts RouteOpts
    Matchers  labels.Matchers    // 파싱된 Matcher
    Continue  bool
    Routes    []*Route           // 자식 경로
    Idx       int                // 고유 인덱스
}

type RouteOpts struct {
    Receiver            string
    GroupBy             map[model.LabelName]struct{}
    GroupByAll          bool
    GroupWait           time.Duration
    GroupInterval       time.Duration
    RepeatInterval      time.Duration
    MuteTimeIntervals   []string
    ActiveTimeIntervals []string
}
```

### 6.2 AlertGroup

```go
// dispatch/dispatch.go
type AlertGroup struct {
    Alerts   []types.Alert
    Labels   model.LabelSet     // 그룹핑 레이블
    Receiver string
    GroupKey string
    RouteID  string
}

type AlertGroups []*AlertGroup
```

## 7. Notification Log 데이터 모델

### 7.1 nflog Entry (Protobuf)

```
// nflog/nflogpb/nflog.proto (개념)
message MeshEntry {
    Entry entry = 1;
    google.protobuf.Timestamp expires_at = 2;
}

message Entry {
    string group_key = 1;
    Receiver receiver = 2;
    repeated uint64 firing_alerts = 3;
    repeated uint64 resolved_alerts = 4;
    google.protobuf.Timestamp timestamp = 5;
    map<string, ReceiverDataValue> receiver_data = 6;
}

message Receiver {
    string group_name = 1;
    string integration = 2;
    uint32 idx = 3;
}
```

### 7.2 nflog Store

```go
// nflog/nflog.go
type Store struct {
    data map[string]*pb.ReceiverDataValue
}

func (s *Store) GetInt(key string) (int64, bool)
func (s *Store) GetFloat(key string) (float64, bool)
func (s *Store) GetStr(key string) (string, bool)
func (s *Store) SetInt(key string, v int64)
func (s *Store) SetFloat(key string, v float64)
func (s *Store) SetStr(key, v string)
```

## 8. Matcher 데이터 모델

### 8.1 Matcher

```go
// pkg/labels/matcher.go
type MatchType int

const (
    MatchEqual     MatchType = iota   // =
    MatchNotEqual                      // !=
    MatchRegexp                        // =~
    MatchNotRegexp                     // !~
)

type Matcher struct {
    Type  MatchType
    Name  string
    Value string
    re    *regexp.Regexp   // 정규식 매칭용 (캐시)
}

type Matchers []*Matcher
```

### 8.2 매칭 로직

```go
func (m *Matcher) Matches(s string) bool   // 단일 값 매칭
func (ms Matchers) Matches(lset model.LabelSet) bool  // 전체 레이블셋 매칭
```

`Matchers.Matches()`는 **모든** Matcher가 일치해야 true (AND 로직).

## 9. Template 데이터 모델

```go
// template/template.go
type Data struct {
    Receiver          string  `json:"receiver"`
    Status            string  `json:"status"`        // "firing" or "resolved"
    Alerts            Alerts  `json:"alerts"`
    NotificationReason string `json:"notification_reason"`
    GroupLabels       KV      `json:"groupLabels"`
    CommonLabels      KV      `json:"commonLabels"`
    CommonAnnotations KV      `json:"commonAnnotations"`
    ExternalURL       string  `json:"externalURL"`
}

type Alert struct {
    Status       string    `json:"status"`
    Labels       KV        `json:"labels"`
    Annotations  KV        `json:"annotations"`
    StartsAt     time.Time `json:"startsAt"`
    EndsAt       time.Time `json:"endsAt"`
    GeneratorURL string    `json:"generatorURL"`
    Fingerprint  string    `json:"fingerprint"`
}

type KV map[string]string   // 레이블/어노테이션
type Alerts []Alert
type Pairs []Pair
type Pair struct { Name, Value string }
```

## 10. 데이터 저장 구조

```
┌──────────────────────────────────────────┐
│              메모리 (런타임)               │
│                                          │
│  store.Alerts                            │
│  ├── map[Fingerprint]*Alert              │
│  └── limit.Bucket (알림별 제한)           │
│                                          │
│  Silences.st                             │
│  └── state (map[string]*MeshEntry)       │
│                                          │
│  nflog.Log.st                            │
│  └── state (map[string]*MeshEntry)       │
│                                          │
│  MemMarker                               │
│  └── map[Fingerprint]*AlertStatus        │
│                                          │
│  Dispatcher.routeGroupsSlice             │
│  └── []routeAggrGroups                   │
│      └── map[string]*aggrGroup           │
└──────────────────────────────────────────┘

┌──────────────────────────────────────────┐
│              디스크 (스냅샷)               │
│                                          │
│  {data-dir}/nflog                        │
│  └── nflog 상태의 protobuf 스냅샷        │
│                                          │
│  {data-dir}/silences                     │
│  └── Silences 상태의 protobuf 스냅샷      │
└──────────────────────────────────────────┘
```

## 11. 데이터 생명주기

### Alert 생명주기

```
생성 (Prometheus POST)
    │
    ▼
Active (firing)
    │
    ├── Silenced (Silence 매칭 시)
    ├── Inhibited (Inhibition 규칙 매칭 시)
    │
    ├── EndsAt 도달 → Resolved
    ├── Timeout (ResolveTimeout) → Resolved + Timeout=true
    │
    └── GC (Resolved 후 일정 시간) → 삭제
```

### Silence 생명주기

```
생성 (API POST)
    │
    ▼
Pending (StartsAt 이전)
    │
    ▼
Active (StartsAt ~ EndsAt)
    │
    ▼
Expired (EndsAt 이후)
    │
    ▼
GC (retention 이후) → 삭제
```
