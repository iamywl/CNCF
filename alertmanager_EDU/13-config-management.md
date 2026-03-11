# Alertmanager 설정 관리 Deep Dive

## 1. 개요

Alertmanager의 설정은 YAML 파일로 정의되며, `config/config.go`에서 파싱하고 `config/coordinator.go`의 Coordinator가 동적 리로드를 관리한다.

## 2. Config 최상위 구조체

```go
// config/config.go
type Config struct {
    Global            *GlobalConfig          // 전역 설정
    Route             *Route                 // 라우팅 트리
    InhibitRules      []InhibitRule          // 억제 규칙
    Receivers         []Receiver             // 수신자 정의
    Templates         []string               // 템플릿 파일 경로
    MuteTimeIntervals []MuteTimeInterval     // (deprecated → TimeIntervals)
    TimeIntervals     []TimeInterval         // 시간 간격
    TracingConfig     tracing.TracingConfig  // 분산 추적 설정
    original          string                 // 원본 YAML
}
```

### 2.1 YAML 구조

```yaml
global:                    # GlobalConfig
  resolve_timeout: 5m
  smtp_smarthost: 'smtp:25'

templates:                 # []string
  - '/etc/am/templates/*.tmpl'

route:                     # Route (트리 루트)
  receiver: 'default'
  group_by: ['alertname']
  routes: [...]

inhibit_rules:             # []InhibitRule
  - source_matchers: [...]
    target_matchers: [...]
    equal: [...]

receivers:                 # []Receiver
  - name: 'default'
    slack_configs: [...]

time_intervals:            # []TimeInterval
  - name: 'business-hours'
    time_intervals: [...]
```

## 3. GlobalConfig

```go
// config/config.go
type GlobalConfig struct {
    ResolveTimeout model.Duration     // Alert 자동 해제 시간 (기본 5m)
    HTTPConfig     *commoncfg.HTTPClientConfig

    // SMTP 설정
    SMTPFrom         string
    SMTPSmarthost    config.HostPort
    SMTPHello        string
    SMTPAuthUsername  string
    SMTPAuthPassword config.Secret
    SMTPRequireTLS   bool

    // 각 Integration 전역 설정
    SlackAPIURL       *config.SecretURL
    SlackAPIURLFile   string
    PagerdutyURL      *config.URL
    OpsGenieAPIURL    *config.URL
    OpsGenieAPIKey    config.Secret
    WechatAPIURL      *config.URL
    WechatAPISecret   config.Secret
    WechatAPICorpID   string
    VictorOpsAPIURL   *config.URL
    VictorOpsAPIKey   config.Secret
    TelegramAPIUrl    *config.URL
    WebexAPIURL       *config.URL
}
```

GlobalConfig의 값은 각 Receiver 설정에서 지정하지 않은 경우 기본값으로 사용된다.

## 4. Route 설정 구조체

```go
// config/config.go
type Route struct {
    Receiver            string
    GroupByStr           []string              // YAML에서 파싱
    GroupBy              []model.LabelName     // 그룹핑 레이블
    GroupByAll           bool                  // '...'으로 설정 시
    Match                map[string]string     // (deprecated)
    MatchRE              MatchRegexps          // (deprecated)
    Matchers             Matchers              // 새로운 매칭 방식
    Continue             bool                  // 다음 형제도 매칭 시도
    Routes               []*Route              // 자식 Route
    GroupWait            *model.Duration       // 그룹화 대기
    GroupInterval        *model.Duration       // 그룹 알림 간격
    RepeatInterval       *model.Duration       // 반복 알림 간격
    MuteTimeIntervals    []string              // 뮤트 시간대 이름
    ActiveTimeIntervals  []string              // 활성 시간대 이름
}
```

### 4.1 옵션 상속 규칙

```
Route 트리에서 자식은 부모의 설정을 상속받는다:
- Receiver: 부모 값 사용 (자식에서 덮어쓰기 가능)
- GroupBy: 부모 값 사용 (자식에서 덮어쓰기 가능)
- GroupWait: 부모 값 사용 (자식에서 덮어쓰기 가능)
- GroupInterval: 부모 값 사용
- RepeatInterval: 부모 값 사용
- MuteTimeIntervals: 부모 값 사용
```

## 5. Receiver 구조체

```go
// config/config.go
type Receiver struct {
    Name               string
    EmailConfigs       []*EmailConfig
    PagerdutyConfigs   []*PagerdutyConfig
    SlackConfigs       []*SlackConfig
    WebhookConfigs     []*WebhookConfig
    OpsGenieConfigs    []*OpsGenieConfig
    WechatConfigs      []*WechatConfig
    PushoverConfigs    []*PushoverConfig
    VictorOpsConfigs   []*VictorOpsConfig
    SNSConfigs         []*SNSConfig
    TelegramConfigs    []*TelegramConfig
    DiscordConfigs     []*DiscordConfig
    WebexConfigs       []*WebexConfig
    MSTeamsConfigs     []*MSTeamsConfig
    MSTeamsV2Configs   []*MSTeamsV2Config
    JiraConfigs        []*JiraConfig
    RocketchatConfigs  []*RocketchatConfig
    SMTPConfigs        []*SMTPConfig
}
```

하나의 Receiver에 여러 Integration을 동시에 설정할 수 있다. 예를 들어 Slack과 Email을 동시에 설정하면 둘 다 알림을 받는다.

## 6. InhibitRule

```go
// config/config.go
type InhibitRule struct {
    Name            string
    SourceMatchers  Matchers    // Source 조건
    TargetMatchers  Matchers    // Target 조건
    Equal           []string    // 동일해야 할 레이블
    // deprecated 필드: SourceMatch, SourceMatchRE, TargetMatch, TargetMatchRE
}
```

## 7. Load와 LoadFile

```go
// config/config.go
func Load(s string) (*Config, error)           // YAML 문자열에서 로드
func LoadFile(filename string) (*Config, error) // 파일에서 로드
```

```
LoadFile() 흐름:
    1. 파일 읽기
    2. Load(content)
       a. YAML 언마샬링
       b. 유효성 검증:
          - Route 트리 필수
          - 루트 Route에 Receiver 필수
          - 루트 Route에 Matchers 금지
          - 모든 Route의 Receiver가 정의되어야 함
          - Receiver 이름 중복 불가
          - Template 파일 존재 확인
          - InhibitRule 유효성
          - TimeInterval 이름 유효성
       c. 유효성 통과 → Config 반환
       d. 실패 → 에러 반환 (기존 설정 유지)
```

### 7.1 유효성 검증 상세

```
검증 항목:
├── Route 트리
│   ├── 루트 Route에 receiver 필수
│   ├── 루트 Route에 matchers 금지
│   ├── 모든 Route의 receiver가 receivers에 정의되어야 함
│   ├── GroupWait >= 0
│   ├── GroupInterval >= 0
│   ├── RepeatInterval >= 0
│   ├── MuteTimeIntervals가 time_intervals에 정의되어야 함
│   └── ActiveTimeIntervals가 time_intervals에 정의되어야 함
│
├── Receivers
│   ├── 이름 중복 불가
│   └── 각 Integration Config 유효성 (URL, 인증 등)
│
├── Templates
│   └── Glob 패턴으로 파일 존재 확인
│
├── InhibitRules
│   └── SourceMatchers, TargetMatchers 파싱 유효성
│
└── TimeIntervals
    ├── 이름 중복 불가
    └── 시간 범위 유효성
```

## 8. Coordinator

```go
// config/coordinator.go
type Coordinator struct {
    configFilePath      string
    logger              *slog.Logger

    mutex               sync.Mutex
    config              *Config
    subscribers         []func(*Config) error

    configHashMetric        prometheus.Gauge
    configSuccessMetric     prometheus.Gauge
    configSuccessTimeMetric prometheus.Gauge
}
```

### 8.1 Subscribe()

```go
func (c *Coordinator) Subscribe(ss ...func(*Config) error)
```

설정 변경 시 호출될 콜백을 등록한다. 콜백은 등록 순서대로 실행된다.

### 8.2 Reload()

```go
func (c *Coordinator) Reload() error
```

```
Reload() 흐름:
    1. c.mutex.Lock()          // 동시 리로드 방지
    2. LoadFile(configFilePath) // YAML 파싱 + 유효성 검증
    3. 성공 시:
       a. 각 subscriber(newConfig) 순서대로 호출
       b. 하나라도 실패하면 에러 반환 (기존 설정 유지)
       c. 모두 성공하면:
          - c.config = newConfig
          - configHashMetric 업데이트
          - configSuccessMetric = 1
          - configSuccessTimeMetric = now
    4. 실패 시:
       - configSuccessMetric = 0
       - 에러 로깅
    5. c.mutex.Unlock()
```

### 8.3 구독자 콜백 순서 (main.go에서)

```
1. Template 재로드
2. Inhibitor 재구성 (Stop → New → Run)
3. TimeInterval Intervener 재구성
4. Silencer 재구성
5. Notification Pipeline 재구축 (RoutingStage)
6. Dispatcher 재시작 (Stop → New → Run)
7. API 업데이트 (config, setAlertStatus)
```

## 9. 설정 메트릭

| 메트릭 | 타입 | 설명 |
|--------|------|------|
| `alertmanager_config_hash` | Gauge | 현재 설정 해시값 |
| `alertmanager_config_last_reload_successful` | Gauge | 마지막 리로드 성공 (1) / 실패 (0) |
| `alertmanager_config_last_reload_success_timestamp_seconds` | Gauge | 마지막 성공 리로드 타임스탬프 |

## 10. 설정 리로드 트리거

```
방법 1: SIGHUP 시그널
    kill -HUP <pid>

방법 2: HTTP 엔드포인트
    POST /-/reload

방법 3: amtool
    amtool check-config alertmanager.yml  (검증만)
```

## 11. config/receiver/ 빌더 패턴

```go
// config/receiver/
// Receiver 설정을 Integration으로 빌드하는 함수들
func BuildReceiverIntegrations(
    nc config.Receiver,
    tmpl *template.Template,
    logger *slog.Logger,
    httpOpts ...commoncfg.HTTPClientOption,
) ([]Integration, error)
```

각 Receiver Config(SlackConfig, EmailConfig 등)를 실제 Notifier 구현으로 변환한다:

```
SlackConfig  → slack.New(cfg, tmpl, logger)   → slack.Notifier
EmailConfig  → email.New(cfg, tmpl, logger)   → email.Notifier
WebhookConfig → webhook.New(cfg, tmpl, logger) → webhook.Notifier
...
```

## 12. 에러 처리

```
설정 리로드 실패 시:
├── YAML 파싱 오류 → 에러 로깅, 기존 설정 유지
├── 유효성 검증 실패 → 에러 로깅, 기존 설정 유지
├── 구독자 콜백 실패 → 에러 로깅, 부분 적용 위험
│   (이미 실행된 콜백은 되돌릴 수 없음)
└── 모든 경우: configSuccessMetric = 0

베스트 프랙티스:
    amtool check-config로 먼저 검증 후 리로드
```

## 13. 실전 설정 예시

### 13.1 다중 팀 라우팅

```yaml
route:
  receiver: 'default'
  group_by: ['alertname', 'cluster']
  routes:
    - matchers:
        - team="backend"
      receiver: 'backend-slack'
      routes:
        - matchers:
            - severity="critical"
          receiver: 'backend-pager'
    - matchers:
        - team="frontend"
      receiver: 'frontend-slack'
      routes:
        - matchers:
            - severity="critical"
          receiver: 'frontend-pager'
```

### 13.2 시간 기반 라우팅

```yaml
time_intervals:
  - name: 'business-hours'
    time_intervals:
      - weekdays: ['monday:friday']
        times: [{start_time: '09:00', end_time: '18:00'}]
        location: 'Asia/Seoul'
  - name: 'off-hours'
    time_intervals:
      - weekdays: ['saturday:sunday']
      - times: [{start_time: '18:00', end_time: '09:00'}]

route:
  receiver: 'default'
  routes:
    - matchers: [severity="critical"]
      receiver: 'pager'
    - matchers: [severity="warning"]
      receiver: 'slack'
      mute_time_intervals: ['off-hours']
```

## 14. Coordinator 소스 코드 상세 분석

### 14.1 registerMetrics

```go
// config/coordinator.go
func (c *Coordinator) registerMetrics(r prometheus.Registerer) {
    configHash := promauto.With(r).NewGauge(prometheus.GaugeOpts{
        Name: "alertmanager_config_hash",
        Help: "Hash of the currently loaded alertmanager configuration.",
    })
    configSuccess := promauto.With(r).NewGauge(prometheus.GaugeOpts{
        Name: "alertmanager_config_last_reload_successful",
    })
    configSuccessTime := promauto.With(r).NewGauge(prometheus.GaugeOpts{
        Name: "alertmanager_config_last_reload_success_timestamp_seconds",
    })
    c.configHashMetric = configHash
    c.configSuccessMetric = configSuccess
    c.configSuccessTimeMetric = configSuccessTime
}
```

### 14.2 md5HashAsMetricValue

```go
// config/coordinator.go
func md5HashAsMetricValue(data []byte) float64 {
    sum := md5.Sum(data)
    smallSum := sum[0:6]          // 48비트만 사용
    bytes := make([]byte, 8)
    copy(bytes, smallSum)
    return float64(binary.LittleEndian.Uint64(bytes))
}
```

**왜 48비트만 사용하는가?** `float64`의 mantissa는 53비트이다. MD5 해시 전체(128비트)를 float64로 변환하면 정밀도 손실이 발생하여 서로 다른 설정이 같은 메트릭 값을 가질 수 있다. 48비트로 제한하면 Prometheus Gauge에서 정확하게 표현된다.

### 14.3 Reload 흐름의 원자성 문제

```go
// config/coordinator.go
func (c *Coordinator) Reload() error {
    c.mutex.Lock()
    defer c.mutex.Unlock()

    if err := c.loadFromFile(); err != nil {
        c.configSuccessMetric.Set(0)
        return err   // 파일 로드 실패 → 기존 설정 유지
    }

    if err := c.notifySubscribers(); err != nil {
        c.configSuccessMetric.Set(0)
        return err   // 구독자 실패 → 부분 적용 위험!
    }

    c.configSuccessMetric.Set(1)
    c.configSuccessTimeMetric.SetToCurrentTime()
    hash := md5HashAsMetricValue([]byte(c.config.original))
    c.configHashMetric.Set(hash)
    return nil
}
```

**부분 적용 문제**: `notifySubscribers()`에서 3번째 구독자가 실패하면, 1번과 2번 구독자는 이미 새 설정을 적용한 상태이다. 이를 롤백할 수 없으므로, 구독자 순서가 중요하다:

```
구독자 실행 순서 (main.go):
1. Template 재로드        ← 실패 시 영향 범위 작음
2. Inhibitor 재구성       ← 실패 시 억제 규칙만 영향
3. TimeInterval 재구성
4. Silencer 재구성
5. Notification Pipeline  ← 실패 시 알림 전송 영향
6. Dispatcher 재시작      ← 실패 시 라우팅 영향
7. API 업데이트           ← 마지막, 가장 가벼운 작업
```

**왜 롤백을 구현하지 않는가?** 각 구독자(Inhibitor, Silencer 등)의 내부 상태를 안전하게 복원하는 것은 매우 복잡하고 오류 가능성이 높다. 대신 `amtool check-config`로 사전 검증하는 것이 권장된다.

## 15. Config 유효성 검증 상세

### 15.1 Route 트리 검증

```go
// config/config.go (checkReceiver 내부)
func checkReceiver(config *Config, routeNode *Route) error {
    // 루트 Route에는 Matchers가 없어야 함
    if routeNode == config.Route && len(routeNode.Matchers) > 0 {
        return errors.New("root route must not have matchers")
    }
    // Receiver가 정의되어 있는지 확인
    if _, found := receiverNames[routeNode.Receiver]; !found {
        return fmt.Errorf("receiver %q not found", routeNode.Receiver)
    }
    // 자식 Route를 재귀적으로 검증
    for _, child := range routeNode.Routes {
        if err := checkReceiver(config, child); err != nil {
            return err
        }
    }
    return nil
}
```

### 15.2 TimeInterval 교차 검증

```
Route의 MuteTimeIntervals 또는 ActiveTimeIntervals에 참조된 이름이
실제 time_intervals에 정의되어 있는지 확인:

route:
  routes:
    - matchers: [severity="warning"]
      mute_time_intervals: ['off-hours']    # ← 이 이름이 존재해야 함

time_intervals:
  - name: 'off-hours'                        # ← 여기에 정의
    time_intervals: [...]
```

### 15.3 Secret 필드 마스킹

```go
// Config의 original 필드는 YAML 원본 텍스트를 저장하지만,
// API를 통해 반환할 때 Secret 필드는 마스킹된다:
// smtp_auth_password: <secret>
// slack_api_url: <secret>
```

## 16. SecretTemplateURL 타입

```go
// config/config.go
type SecretTemplateURL commoncfg.Secret

func (s *SecretTemplateURL) UnmarshalYAML(unmarshal func(any) error) error {
    // 1. 문자열로 파싱
    if err := unmarshal((*plain)(s)); err != nil {
        return err
    }
    // 2. Go 템플릿 문법 포함 여부 확인
    isTemplated, err := containsTemplating(urlStr)
    // 3. 템플릿이 아닌 경우에만 URL 유효성 검증
    if !isTemplated {
        if _, err := amcommoncfg.ParseURL(urlStr); err != nil {
            return fmt.Errorf("invalid URL: %w", err)
        }
    }
    return nil
}
```

**왜 템플릿 URL은 검증을 건너뛰는가?** Webhook URL에 `{{ .GroupLabels.alertname }}`처럼 Go 템플릿을 포함할 수 있다. 이런 URL은 런타임에 Alert 데이터로 렌더링되므로, 설정 로드 시점에는 유효한 URL이 아니다.

## 17. 에러 처리 패턴 정리

```
설정 리로드 에러 체계:

1. 파일 읽기 에러
   └─ os.ReadFile 실패 → 파일 경로/권한 문제

2. YAML 파싱 에러
   └─ yaml.Unmarshal 실패 → 문법 오류

3. 유효성 검증 에러
   ├─ Route 트리 검증 실패
   ├─ Receiver 미정의 참조
   ├─ TimeInterval 미정의 참조
   ├─ Matcher 파싱 실패
   ├─ Integration Config 검증 실패 (URL, 인증)
   └─ Template 파일 Glob 매칭 실패

4. 구독자 콜백 에러
   ├─ Inhibitor 재구성 실패
   ├─ Pipeline 구축 실패
   └─ Dispatcher 재시작 실패

모든 에러 유형:
  → configSuccessMetric = 0
  → 에러 로깅
  → 기존 설정 유지 (단, 4번은 부분 적용 가능)
```

## 18. 성능 고려사항

### 18.1 리로드 비용

설정 리로드는 전체 Pipeline을 재구축한다. 대규모 설정에서의 비용:

| 단계 | 비용 요소 |
|------|----------|
| YAML 파싱 | Route 트리 깊이, Receiver 수 |
| Matcher 파싱 | Fallback 모드에서 이중 파싱 |
| Inhibitor 재구성 | InhibitRule 수 × Matcher 컴파일 |
| Pipeline 구축 | Receiver 수 × Integration 수 |
| Dispatcher 재시작 | 기존 Aggregation Group 재분배 |

### 18.2 Coordinator mutex 영향

```
Reload() 동안 mutex가 잠겨 있으므로:
  - 다른 SIGHUP → 대기
  - API /-/reload → 대기
  - Config() 조회 → 대기

리로드가 1초 이상 걸리면 API 응답 지연이 발생할 수 있다.
대규모 설정에서는 amtool check-config로 사전 검증하여
실패로 인한 불필요한 리로드를 방지해야 한다.
```

## 19. 운영 가이드

### 19.1 설정 검증 흐름

```bash
# 1. 문법 검증
amtool check-config alertmanager.yml

# 2. 메트릭으로 리로드 상태 확인
curl -s localhost:9093/metrics | grep alertmanager_config

# 3. 리로드 트리거
curl -X POST localhost:9093/-/reload

# 4. 리로드 성공 확인
alertmanager_config_last_reload_successful 1
alertmanager_config_last_reload_success_timestamp_seconds <현재>
alertmanager_config_hash <새해시>
```

### 19.2 설정 변경 모니터링 알림 규칙

```yaml
# Prometheus 알림 규칙 예시
- alert: AlertmanagerConfigReloadFailed
  expr: alertmanager_config_last_reload_successful == 0
  for: 5m
  labels:
    severity: warning
  annotations:
    summary: "Alertmanager 설정 리로드 실패"
```
