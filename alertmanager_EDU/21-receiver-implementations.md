# 21. Receiver 구현체 Deep-Dive

> Alertmanager의 알림 전송을 담당하는 개별 Receiver(수신자) 구현체를 분석한다.
> 소스 기준: `notify/slack/`, `notify/email/`, `notify/webhook/`, `notify/pagerduty/`,
> `notify/opsgenie/`, `notify/telegram/`, `notify/discord/`, `notify/msteamsv2/`,
> `notify/sns/`, `notify/victorops/`, `notify/pushover/`, `notify/wechat/`,
> `notify/webex/`, `notify/rocketchat/`, `notify/mattermost/`, `notify/jira/`,
> `notify/incidentio/`

---

## 1. Receiver 아키텍처 개요

### 1.1 Notifier 인터페이스

모든 Receiver 구현체는 `notify.Notifier` 인터페이스를 구현한다.

```
// notify/notify.go
type Notifier interface {
    Notify(context.Context, ...*types.Alert) (bool, error)
}
```

반환값:
- `bool`: 재시도 가능 여부 (true이면 일시적 오류로 재시도)
- `error`: nil이면 성공

### 1.2 공통 패턴

모든 Receiver 구현체는 동일한 패턴을 따른다:

```
┌──────────────────────────────────────────────────────┐
│                  Receiver 구현체                       │
│                                                       │
│  ┌───────────┐  ┌──────────┐  ┌────────────┐         │
│  │   conf    │  │   tmpl   │  │   logger   │         │
│  │ (Config)  │  │(Template)│  │  (*slog)   │         │
│  └───────────┘  └──────────┘  └────────────┘         │
│                                                       │
│  ┌───────────┐  ┌──────────────────────────┐         │
│  │  client   │  │       retrier            │         │
│  │(*http)    │  │ (RetryCodes, Details)     │         │
│  └───────────┘  └──────────────────────────┘         │
│                                                       │
│  Notify(ctx, alerts...) → (retry bool, err error)    │
│    1. ExtractGroupKey(ctx)                            │
│    2. GetTemplateData(ctx, tmpl, alerts)              │
│    3. TmplText(tmpl, data, &err)                     │
│    4. Build request payload                           │
│    5. PostJSON / custom send                          │
│    6. retrier.Check(statusCode, body)                 │
│    7. Return (retry, error)                           │
│                                                       │
└──────────────────────────────────────────────────────┘
```

### 1.3 New() 생성자 공통 흐름

```go
func New(c *config.XxxConfig, t *template.Template, l *slog.Logger,
         httpOpts ...commoncfg.HTTPClientOption) (*Notifier, error) {
    // 1) HTTP 클라이언트 생성 (트레이싱 포함)
    client, err := notify.NewClientWithTracing(*c.HTTPConfig, "xxx", httpOpts...)
    if err != nil {
        return nil, err
    }
    // 2) Notifier 구성
    return &Notifier{
        conf:    c,
        tmpl:    t,
        logger:  l,
        client:  client,
        retrier: &notify.Retrier{RetryCodes: []int{...}},
    }, nil
}
```

모든 Receiver는 `notify.NewClientWithTracing()`을 통해 OpenTelemetry 트레이싱이 적용된 HTTP 클라이언트를 받는다.

---

## 2. HTTP 기반 Webhook Receiver

### 2.1 Webhook (`notify/webhook/webhook.go`)

가장 기본적인 Receiver. 알림 데이터를 JSON으로 직렬화하여 사용자 지정 URL에 POST한다.

```
// notify/webhook/webhook.go
type Notifier struct {
    conf    *config.WebhookConfig
    tmpl    *template.Template
    logger  *slog.Logger
    client  *http.Client
    retrier *notify.Retrier
}
```

**Webhook Message 구조:**

```go
type Message struct {
    *template.Data
    Version         string `json:"version"`         // "4"
    GroupKey        string `json:"groupKey"`
    TruncatedAlerts uint64 `json:"truncatedAlerts"`  // MaxAlerts 초과 시
}
```

**핵심 동작:**

1. `MaxAlerts` 설정이 있으면 알림을 잘라내고 `TruncatedAlerts`에 초과 수 기록
2. URL은 설정값 또는 `URLFile`에서 읽기 (파일 기반 시크릿 지원)
3. URL 자체도 Go 템플릿으로 렌더링 가능 (동적 URL)
4. `Timeout` 설정 시 `context.WithTimeoutCause`로 timeout context 생성
5. 2xx는 성공, 5xx는 재시도 가능, 4xx는 재시도 불가

```
Alert Groups → truncateAlerts(maxAlerts) → JSON Encode
    → Template URL → PostJSON → retrier.Check → Result
```

### 2.2 왜 Webhook이 중요한가

- Alertmanager가 직접 지원하지 않는 모든 서비스와 연동하는 범용 게이트웨이
- `Version: "4"` 필드로 페이로드 형식의 하위 호환성 보장
- `TruncatedAlerts` 필드로 수신측이 잘린 알림 수를 인지 가능

---

## 3. 메시징 플랫폼 Receiver

### 3.1 Slack (`notify/slack/slack.go`)

가장 성숙한 Receiver 구현체. Incoming Webhook과 API 방식 모두 지원.

```go
type Notifier struct {
    conf    *config.SlackConfig
    tmpl    *template.Template
    logger  *slog.Logger
    client  *http.Client
    retrier *notify.Retrier
    postJSONFunc func(ctx context.Context, client *http.Client, url string,
                      body io.Reader) (*http.Response, error)
}
```

**Slack 고유 기능:**

1. **Attachment 기반 리치 메시지**: Title, Pretext, Text, Fields, Actions, Color
2. **메시지 업데이트 (`UpdateMessage`)**: `nflog.Store`에 `threadTs`/`channelId` 저장하여 기존 메시지 수정
3. **API URL / Incoming Webhook 자동 감지**: JSON 응답(`{ok: true}`)이면 API, 평문(`ok`)이면 Webhook
4. **제한**: Title은 1024 rune, `TruncateInRunes()`로 자동 절단

```
┌─────────────────────────────────────────────┐
│              Slack Notify Flow              │
│                                             │
│  GetTemplateData → TmplText                 │
│       ↓                                     │
│  Build attachment{Title, Text, Fields, ...} │
│       ↓                                     │
│  UpdateMessage 확인                          │
│   ├─ Yes → nflog에서 threadTs/channelId 조회 │
│   │        → chat.update endpoint 사용       │
│   └─ No  → 일반 Webhook/API 전송             │
│       ↓                                     │
│  PostJSON → slackResponseHandler            │
│   ├─ JSON 응답 → {ok: true} 파싱             │
│   └─ Text 응답 → "ok" 확인 (Webhook)         │
│       ↓                                     │
│  성공 시 threadTs/channelId 저장             │
└─────────────────────────────────────────────┘
```

**`slackResponseHandler` 분석:**

```go
// notify/slack/slack.go:273
func (n *Notifier) slackResponseHandler(resp *http.Response, store *nflog.Store) (bool, error) {
    body, err := io.ReadAll(resp.Body)
    // Content-Type이 JSON이 아니면 plain text 확인
    if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
        return checkTextResponseError(body)  // "ok"이면 성공
    }
    var data slackResponse
    json.Unmarshal(body, &data)
    if !data.OK {
        return false, fmt.Errorf("error response from Slack: %s", data.Error)
    }
    // 성공 시 threadTs/channelId 저장 (메시지 업데이트용)
    if store != nil && data.Timestamp != "" && data.Channel != "" {
        store.SetStr("threadTs", data.Timestamp)
        store.SetStr("channelId", data.Channel)
    }
    return false, nil
}
```

### 3.2 Discord (`notify/discord/discord.go`)

Discord Webhook API를 사용하는 Receiver.

```go
type Notifier struct {
    conf       *config.DiscordConfig
    tmpl       *template.Template
    logger     *slog.Logger
    client     *http.Client
    retrier    *notify.Retrier
    webhookURL *amcommoncfg.SecretURL
}
```

**Discord 고유 특성:**

- Embed 기반 리치 메시지 (Title 256 rune, Description 4096 rune)
- Content 필드 2000 rune 제한
- 색상 코드: Red(`0x992D22`), Green(`0x2ECC71`), Grey(`0x95A5A6`)
- `WebhookURL`은 `SecretURL` 타입으로 로그에 노출 방지
- `?wait=true` 파라미터 추가하여 Discord 서버 응답 대기

### 3.3 MS Teams v2 (`notify/msteamsv2/msteamsv2.go`)

Adaptive Card 기반의 최신 Teams Webhook 포맷.

```go
// https://learn.microsoft.com/en-us/connectors/teams/?tabs=text1
type Content struct {
    Schema  string  `json:"$schema"`   // AdaptiveCard 스키마
    Type    string  `json:"type"`      // "AdaptiveCard"
    Version string  `json:"version"`   // "1.4"
    Body    []Body  `json:"body"`      // TextBlock 목록
    Msteams Msteams `json:"msteams,omitempty"`
}
```

**Teams v2 특성:**

- v1(MessageCard) 에서 v2(AdaptiveCard)로 전환
- `$schema` 참조 필수
- `postJSONFunc` 함수 포인터로 테스트 주입 용이

### 3.4 Telegram (`notify/telegram/telegram.go`)

Telegram Bot API를 사용하는 Receiver.

```go
type Notifier struct {
    conf    *config.TelegramConfig
    tmpl    *template.Template
    logger  *slog.Logger
    client  *telebot.Bot    // telebot.v3 라이브러리 사용
    retrier *notify.Retrier
}
```

**Telegram 고유 특성:**

- 메시지 최대 4096 rune
- `telebot.v3` 외부 라이브러리 사용 (다른 Receiver는 순수 HTTP)
- `ParseMode` 지원 (Markdown, HTML)
- ChatID 기반 전송 (채널, 그룹, 1:1)
- Bot Token은 `BotToken`/`BotTokenFile`로 관리

### 3.5 Mattermost (`notify/mattermost/`)

```
Mattermost는 Slack과 거의 동일한 Incoming Webhook 형식을 사용.
Slack의 attachment 형식 호환.
```

### 3.6 Rocket.Chat (`notify/rocketchat/`)

```
Rocket.Chat도 Slack 호환 Webhook 형식 사용.
channel, emoji 등 Slack과 유사한 필드 구조.
```

---

## 4. 인시던트 관리 Receiver

### 4.1 PagerDuty (`notify/pagerduty/pagerduty.go`)

가장 복잡한 Receiver 중 하나. Events API v1/v2 모두 지원.

```go
type Notifier struct {
    conf    *config.PagerdutyConfig
    tmpl    *template.Template
    logger  *slog.Logger
    apiV1   string        // v1 API URL (ServiceKey 사용 시)
    client  *http.Client
    retrier *notify.Retrier
}
```

**v1 vs v2 API 분기:**

```go
// notify/pagerduty/pagerduty.go:57
func New(c *config.PagerdutyConfig, ...) (*Notifier, error) {
    n := &Notifier{conf: c, tmpl: t, logger: l, client: client}
    if c.ServiceKey != "" || c.ServiceKeyFile != "" {
        // v1 API: ServiceKey 기반
        n.apiV1 = "https://events.pagerduty.com/generic/2010-04-15/create_event.json"
        n.retrier = &notify.Retrier{RetryCodes: []int{http.StatusForbidden}}  // 403=rate limit
    } else {
        // v2 API: RoutingKey 기반
        n.retrier = &notify.Retrier{RetryCodes: []int{http.StatusTooManyRequests}}  // 429=rate limit
    }
    return n, nil
}
```

**PagerDuty 고유 특성:**

- 이벤트 타입: `trigger` (발생), `resolve` (해결)
- 최대 이벤트 크기 512KB
- v1 Description 1024 rune, v2 Summary 1024 rune 제한
- RetryCodes가 API 버전에 따라 다름 (v1: 403, v2: 429)
- `errDetails` 커스텀 함수로 상세 오류 정보 추출

### 4.2 OpsGenie (`notify/opsgenie/opsgenie.go`)

Atlassian OpsGenie 인시던트 관리 통합.

```go
type opsGenieCreateMessage struct {
    Alias       string                           `json:"alias"`
    Message     string                           `json:"message"`
    Description string                           `json:"description,omitempty"`
    Details     map[string]string                `json:"details"`
    Responders  []opsGenieCreateMessageResponder `json:"responders,omitempty"`
    Tags        []string                         `json:"tags,omitempty"`
    Priority    string                           `json:"priority,omitempty"`
}
```

**OpsGenie 고유 특성:**

- 메시지 최대 130 rune (가장 짧은 제한)
- Responders: team, user, escalation, schedule 지원
- Close API로 알림 해제 (별도 URL)
- 429 Too Many Requests에서 재시도

### 4.3 VictorOps (`notify/victorops/victorops.go`)

Splunk On-Call(구 VictorOps) 통합.

```go
// 최대 메시지 길이 20480 rune (가장 관대한 제한)
const maxMessageLenRunes = 20480
```

### 4.4 Incident.io (`notify/incidentio/`)

최근 추가된 인시던트 관리 통합.

### 4.5 JIRA (`notify/jira/`)

Atlassian JIRA 이슈 생성/업데이트 통합.

---

## 5. Email Receiver

### 5.1 Email (`notify/email/email.go`)

유일한 비-HTTP Receiver. SMTP 프로토콜을 직접 구현.

```go
type Email struct {
    conf     *config.EmailConfig
    tmpl     *template.Template
    logger   *slog.Logger
    hostname string
}
```

**SMTP 인증 메커니즘:**

```go
// notify/email/email.go:73
func (n *Email) auth(mechs string) (smtp.Auth, error) {
    for mech := range strings.SplitSeq(mechs, " ") {
        switch mech {
        case "CRAM-MD5":
            return smtp.CRAMMD5Auth(username, secret), nil
        case "PLAIN":
            return smtp.PlainAuth(identity, username, password, host), nil
        case "LOGIN":
            return LoginAuth(username, password), nil  // 커스텀 구현!
        }
    }
}
```

**LOGIN 인증 커스텀 구현:**

```go
// notify/email/email.go:385
type loginAuth struct {
    username, password string
}
func (a *loginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
    return "LOGIN", []byte{}, nil
}
func (a *loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
    if more {
        switch strings.ToLower(string(fromServer)) {
        case "username:":
            return []byte(a.username), nil
        case "password:":
            return []byte(a.password), nil
        }
    }
    return nil, nil
}
```

**왜 LOGIN 인증을 직접 구현하나?** Go 표준 라이브러리의 `net/smtp`는 PLAIN과 CRAM-MD5만 지원한다. 많은 기업 메일 서버(Exchange 등)가 LOGIN만 지원하므로 커스텀 구현이 필요했다.

**Email Notify 핵심 흐름:**

```
┌───────────────────────────────────────────────────┐
│               Email Notify Flow                    │
│                                                    │
│  1. TLS 결정                                       │
│     ├─ ForceImplicitTLS 설정 → 명시적 TLS          │
│     ├─ Port 465 → 암시적 TLS (Implicit)            │
│     └─ Otherwise → 평문 시작                        │
│                                                    │
│  2. SMTP 연결                                      │
│     ├─ Implicit TLS → tls.Dial()                   │
│     └─ 평문 → net.Dial() → STARTTLS (선택)          │
│                                                    │
│  3. 인증                                           │
│     Extension("AUTH") → auth(mechs)                │
│     → CRAM-MD5 / PLAIN / LOGIN                     │
│                                                    │
│  4. 전송                                           │
│     MAIL FROM → RCPT TO → DATA                     │
│     → Headers (Subject, From, To, Message-Id)      │
│     → Multipart body (text/plain + text/html)      │
│                                                    │
│  5. 스레딩 (옵션)                                   │
│     Threading.Enabled → References + In-Reply-To    │
│     → GroupKey 해시 기반 스레드 ID                   │
│     → ThreadByDate: "daily" → 날짜별 스레드          │
└───────────────────────────────────────────────────┘
```

**Email 스레딩 구현:**

```go
// notify/email/email.go:277
if n.conf.Threading.Enabled {
    key, _ := notify.ExtractGroupKey(ctx)
    threadBy := ""
    if n.conf.Threading.ThreadByDate != "none" {
        threadBy = time.Now().Format("2006-01-02")  // 날짜별 스레드
    }
    keyHash := key.Hash()[:16]
    threadRootID := fmt.Sprintf("<alert-%s-%s@alertmanager>", keyHash, threadBy)
    fmt.Fprintf(buffer, "References: %s\r\n", threadRootID)
    fmt.Fprintf(buffer, "In-Reply-To: %s\r\n", threadRootID)
}
```

JWZ 알고리즘(대부분의 이메일 클라이언트가 사용)이 동일한 References를 가진 메시지를 같은 스레드로 그룹화한다.

**MIME Multipart 구성:**

```
multipart/alternative (boundary=xxx)
├── text/plain; charset=UTF-8
│   └── quoted-printable 인코딩된 텍스트 템플릿
└── text/html; charset=UTF-8
    └── quoted-printable 인코딩된 HTML 템플릿
```

RFC 2046 5.1.4에 따라 "선호 대안"(HTML)을 마지막에 배치한다.

---

## 6. 클라우드 서비스 Receiver

### 6.1 AWS SNS (`notify/sns/sns.go`)

AWS SDK v2를 사용하는 유일한 Receiver.

```go
type Notifier struct {
    conf    *config.SNSConfig
    tmpl    *template.Template
    logger  *slog.Logger
    client  *http.Client
    retrier *notify.Retrier
}
```

**SNS 고유 특성:**

- AWS SDK v2 (`aws-sdk-go-v2`) 직접 사용
- IAM 역할 기반 인증 (AssumeRole 지원)
- STS를 통한 교차 계정 접근
- Topic ARN / Phone / Target ARN 다중 대상
- SMS MessageAttributes 지원

### 6.2 Webex (`notify/webex/`)

Cisco Webex Teams Webhook 통합.

### 6.3 WeChat (`notify/wechat/`)

기업용 WeChat(WeCom) 통합.

### 6.4 Pushover (`notify/pushover/`)

Pushover 모바일 알림 통합.

---

## 7. Receiver 간 비교 분석

### 7.1 전송 프로토콜 비교

| Receiver | 프로토콜 | 인증 방식 | 제한 |
|----------|---------|----------|------|
| Webhook | HTTP POST | 없음/커스텀 | 없음 |
| Slack | HTTP POST | Bearer Token/Webhook | Title 1024 rune |
| Discord | HTTP POST | Webhook URL | Title 256, Desc 4096 rune |
| MS Teams v2 | HTTP POST | Webhook URL | AdaptiveCard 스키마 |
| Telegram | HTTP POST | Bot Token | 4096 rune |
| PagerDuty | HTTP POST | ServiceKey/RoutingKey | 512KB |
| OpsGenie | HTTP POST | GenieKey | Message 130 rune |
| VictorOps | HTTP POST | Routing Key | 20480 rune |
| Email | SMTP | PLAIN/CRAM-MD5/LOGIN | RFC 5321 |
| SNS | AWS SDK | IAM/AssumeRole | AWS 제한 |
| JIRA | HTTP POST | Basic/Bearer | 이슈 필드 |
| Incident.io | HTTP POST | API Key | - |

### 7.2 재시도 코드 비교

| Receiver | RetryCodes | 근거 |
|----------|-----------|------|
| Webhook | (기본: 5xx) | 범용 |
| Slack | (기본: 5xx) | Slack API 가이드 |
| PagerDuty v1 | 403 | rate limiting |
| PagerDuty v2 | 429 | rate limiting |
| OpsGenie | 429 | rate limiting |
| Discord | (기본: 5xx) | Discord API |
| SNS | (기본: 5xx) | AWS SDK 재시도 |

### 7.3 메시지 크기 제한 비교

```
┌─────────────┬──────────────────────────┐
│  Receiver   │  최대 메시지 크기 (rune)   │
├─────────────┼──────────────────────────┤
│  OpsGenie   │      130                 │  ← 가장 엄격
│  Discord    │      256 (title)         │
│  Slack      │    1,024 (title)         │
│  PagerDuty  │    1,024 (summary)       │
│  Telegram   │    4,096                 │
│  Discord    │    4,096 (desc)          │
│  VictorOps  │   20,480                 │
│  PagerDuty  │  512,000 (event)         │  ← 가장 관대
│  Webhook    │    제한 없음              │
└─────────────┴──────────────────────────┘
```

---

## 8. 공통 유틸리티 함수

### 8.1 `notify.PostJSON()`

```go
// notify/util.go
func PostJSON(ctx context.Context, client *http.Client, url string,
              body io.Reader) (*http.Response, error) {
    req, err := http.NewRequestWithContext(ctx, "POST", url, body)
    req.Header.Set("Content-Type", "application/json")
    return client.Do(req)
}
```

### 8.2 `notify.TruncateInRunes()`

```go
func TruncateInRunes(s string, n int) (string, bool) {
    // 문자열을 rune 단위로 잘라냄
    // (바이트가 아닌 유니코드 코드포인트 기준)
}
```

왜 rune 기준인가: API마다 "characters"로 제한을 표현하지만 실제로는 유니코드 코드포인트(rune)를 의미한다. 한글, 이모지 등 멀티바이트 문자가 정확히 처리되어야 한다.

### 8.3 `notify.NewClientWithTracing()`

```go
func NewClientWithTracing(httpConfig commoncfg.HTTPClientConfig,
                          name string, ...) (*http.Client, error) {
    // HTTP 클라이언트 생성 + OTel 트레이싱 Transport 래핑
    // tracing.Transport(rt)로 모든 HTTP 요청에 스팬 전파
}
```

### 8.4 `notify.RedactURL()`

URL에 포함된 시크릿(API 키, 토큰 등)을 `<secret>` 로 대체하여 로그 노출 방지.

### 8.5 `notify.Drain()`

응답 바디를 완전히 읽고 닫아서 HTTP 커넥션 재사용을 보장.

### 8.6 `notify.GetFailureReasonFromStatusCode()`

HTTP 상태 코드를 실패 사유(`ClientErrorReason`, `ServerErrorReason`)로 분류하여 메트릭에 반영.

---

## 9. Integration 래퍼

`notify.Integration`은 개별 Notifier를 래핑하여 트레이싱과 메타데이터를 추가한다.

```go
// notify/notify.go:70
type Integration struct {
    notifier     Notifier
    rs           ResolvedSender
    name         string          // "slack", "email" 등
    idx          int             // 동일 타입 내 인덱스
    receiverName string          // 설정의 receiver 이름
}

func (i *Integration) Notify(ctx context.Context, alerts ...*types.Alert) (bool, error) {
    ctx, span := tracer.Start(ctx, "notify.Integration.Notify",
        trace.WithAttributes(
            attribute.String("alerting.notify.integration.name", i.name),
            attribute.Int("alerting.alerts.count", len(alerts)),
        ),
        trace.WithSpanKind(trace.SpanKindClient),
    )
    defer func() {
        span.SetAttributes(attribute.Bool("alerting.notify.error.recoverable", recoverable))
        if err != nil {
            span.SetStatus(codes.Error, err.Error())
        }
        span.End()
    }()
    return i.notifier.Notify(ctx, alerts...)
}
```

모든 알림 전송은 `Integration.Notify()`를 통해 OpenTelemetry 스팬으로 추적된다.

---

## 10. 시크릿 관리 패턴

### 10.1 Secret 타입

```go
// 설정에서 시크릿을 두 가지 방식으로 제공
type SlackConfig struct {
    APIURL     *config.SecretURL  // 직접 값
    APIURLFile string             // 파일 경로
}
```

### 10.2 파일 기반 시크릿 로딩

```go
// 런타임에 파일에서 읽기 (Kubernetes Secret mount 패턴)
if n.conf.APIURL != nil {
    u = n.conf.APIURL.String()
} else {
    content, err := os.ReadFile(n.conf.APIURLFile)
    u = strings.TrimSpace(string(content))
}
```

### 10.3 왜 두 방식을 모두 지원하나

- 직접 값: 개발/테스트 환경에서 편리
- 파일 기반: Kubernetes Secret을 볼륨 마운트하여 자동 갱신
- `SecretURL` 타입: `String()` 호출 시 시크릿 부분을 마스킹

---

## 11. 오류 처리 및 분류

### 11.1 Retrier 구조

```go
type Retrier struct {
    RetryCodes       []int    // 재시도할 HTTP 상태 코드
    CustomDetailsFunc func(int, io.Reader) string  // 오류 상세 추출
}

func (r *Retrier) Check(statusCode int, body io.Reader) (bool, error) {
    // 2xx → 성공
    // RetryCodes 포함 → 재시도 가능 오류
    // 5xx → 재시도 가능 오류
    // 기타 → 재시도 불가 오류
}
```

### 11.2 ErrorWithReason

```go
type ErrorWithReason struct {
    Err    error
    Reason FailureReason  // ClientErrorReason, ServerErrorReason
}
```

실패 사유를 메트릭에 반영하여 어떤 유형의 오류가 발생하는지 모니터링 가능.

---

## 12. Timeout 처리

각 Receiver는 설정의 `Timeout` 필드를 통해 개별 타임아웃을 설정할 수 있다.

```go
// Slack, Webhook 등에서 동일한 패턴
if n.conf.Timeout > 0 {
    postCtx, cancel := context.WithTimeoutCause(ctx, n.conf.Timeout,
        fmt.Errorf("configured %s timeout reached (%s)", "slack", n.conf.Timeout))
    defer cancel()
    ctx = postCtx
}
```

`context.WithTimeoutCause`를 사용하여 타임아웃 발생 시 원인을 함께 전달한다. 이를 통해 "configured slack timeout reached (30s)" 같은 명확한 오류 메시지를 제공한다.

---

## 13. 설계 패턴 분석

### 13.1 Strategy 패턴

모든 Receiver가 동일한 `Notifier` 인터페이스를 구현하므로, 알림 파이프라인이 구체 구현을 알 필요 없이 교체 가능하다.

### 13.2 Template Method 패턴

`Notify()` 메서드의 흐름이 동일하다:
1. GroupKey 추출
2. 템플릿 데이터 준비
3. 페이로드 구성 (구현체별 차이)
4. 전송
5. 응답 검증

### 13.3 Factory 패턴

`cmd/alertmanager/main.go`에서 설정에 따라 적절한 Receiver를 생성한다.

### 13.4 Decorator 패턴

`Integration`이 `Notifier`를 래핑하여 트레이싱, 메타데이터, 재시도 로직을 투명하게 추가한다.

---

## 14. 새 Receiver 추가 가이드

Alertmanager에 새 Receiver를 추가하려면:

1. `notify/{name}/` 디렉토리 생성
2. `Notifier` 구조체 정의 (conf, tmpl, logger, client, retrier)
3. `New()` 생성자 구현 (`NewClientWithTracing` 사용)
4. `Notify()` 메서드 구현 (공통 패턴 따르기)
5. `config/notifiers.go`에 설정 구조체 추가
6. `cmd/alertmanager/main.go`에서 팩토리 등록
7. 테스트 작성 (`notify/{name}/{name}_test.go`)

---

## 15. 정리

Alertmanager의 Receiver 아키텍처는 **인터페이스 기반 다형성**의 교과서적 구현이다.

| 특성 | 설명 |
|------|------|
| 총 Receiver 수 | 17개 (Slack, Email, Webhook, PagerDuty, OpsGenie, Telegram, Discord, Teams v2, VictorOps, SNS, Webex, WeChat, Pushover, Rocket.Chat, Mattermost, JIRA, Incident.io) |
| 공통 인터페이스 | `Notifier.Notify(ctx, alerts...) (bool, error)` |
| 프로토콜 | HTTP POST (16개) + SMTP (1개) |
| 시크릿 관리 | 직접값 + 파일 기반 이중 지원 |
| 트레이싱 | 모든 Receiver에 OTel 스팬 자동 적용 |
| 재시도 | Receiver별 RetryCodes 차별화 |
| 크기 제한 | API별 rune 단위 자동 절단 |
| 테스트 | `postJSONFunc` 함수 포인터로 HTTP 모킹 |

핵심 설계 원칙: **"모든 외부 서비스 연동은 동일한 인터페이스 뒤에 숨기되, 각 서비스의 특성(인증, 제한, 재시도, 페이로드 형식)은 구현체가 완전히 캡슐화한다."**

---

*소스 참조: `notify/slack/slack.go`, `notify/email/email.go`, `notify/webhook/webhook.go`, `notify/pagerduty/pagerduty.go`, `notify/opsgenie/opsgenie.go`, `notify/telegram/telegram.go`, `notify/discord/discord.go`, `notify/msteamsv2/msteamsv2.go`, `notify/sns/sns.go`, `notify/victorops/victorops.go`, `notify/notify.go`*
