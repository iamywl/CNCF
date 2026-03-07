# Alertmanager 템플릿 엔진 Deep Dive

## 1. 개요

Alertmanager는 Go의 `text/template`과 `html/template`을 사용하여 알림 메시지를 렌더링한다. `template/template.go`에 구현되어 있으며, 커스텀 함수와 데이터 구조를 제공한다.

## 2. Template 구조체

```go
// template/template.go
type Template struct {
    text        *tmpltext.Template    // text/template
    html        *tmplhtml.Template    // html/template (XSS 방지)
    ExternalURL *url.URL              // Alertmanager 외부 URL
}
```

### 2.1 생성

```go
func New(options ...Option) (*Template, error)
func FromGlobs(paths []string, options ...Option) (*Template, error)
```

```
FromGlobs() 흐름:
    1. New() → 빈 Template 생성
    2. DefaultFuncs 등록
    3. 각 glob 경로에 대해:
       - filepath.Glob(path) → 매칭 파일 목록
       - 각 파일을 Parse()
    4. 기본 템플릿 (default_tmpl.go) 로드
```

### 2.2 Option 패턴

```go
type Option func(text *tmpltext.Template, html *tmplhtml.Template)
```

## 3. Data 구조체

```go
// template/template.go
type Data struct {
    Receiver           string  `json:"receiver"`            // Receiver 이름
    Status             string  `json:"status"`              // "firing" 또는 "resolved"
    Alerts             Alerts  `json:"alerts"`              // Alert 목록
    NotificationReason string  `json:"notification_reason"` // 알림 사유
    GroupLabels        KV      `json:"groupLabels"`         // 그룹핑 레이블
    CommonLabels       KV      `json:"commonLabels"`        // 공통 레이블
    CommonAnnotations  KV      `json:"commonAnnotations"`   // 공통 어노테이션
    ExternalURL        string  `json:"externalURL"`         // AM 외부 URL
}
```

### 3.1 Data 생성

```go
func (t *Template) Data(
    recv string,
    groupLabels model.LabelSet,
    notificationReason string,
    alerts ...*types.Alert,
) *Data
```

```
Template.Data() 흐름:
    1. 각 Alert를 템플릿 Alert 구조로 변환
    2. Status 결정:
       - 하나라도 firing → "firing"
       - 모두 resolved → "resolved"
    3. CommonLabels 계산:
       - 모든 Alert에 공통인 레이블
    4. CommonAnnotations 계산:
       - 모든 Alert에 공통인 어노테이션
    5. GroupLabels 설정
```

## 4. Alert 구조체 (템플릿용)

```go
// template/template.go
type Alert struct {
    Status       string    `json:"status"`        // "firing" 또는 "resolved"
    Labels       KV        `json:"labels"`        // 레이블
    Annotations  KV        `json:"annotations"`   // 어노테이션
    StartsAt     time.Time `json:"startsAt"`      // 시작 시간
    EndsAt       time.Time `json:"endsAt"`        // 종료 시간
    GeneratorURL string    `json:"generatorURL"`  // 생성자 URL
    Fingerprint  string    `json:"fingerprint"`   // 고유 식별자
}
```

## 5. KV 타입

```go
// template/template.go
type KV map[string]string

func (kv KV) SortedPairs() Pairs      // 정렬된 key-value 쌍
func (kv KV) Remove(keys []string) KV  // 특정 키 제거
func (kv KV) Names() []string          // 키 목록
func (kv KV) Values() []string         // 값 목록

type Pair struct {
    Name, Value string
}
type Pairs []Pair

func (ps Pairs) Names() []string       // 키 목록
func (ps Pairs) Values() []string      // 값 목록
func (ps Pairs) String() string        // "key1=val1, key2=val2"
```

## 6. Alerts 타입

```go
// template/template.go
type Alerts []Alert

func (as Alerts) Firing() []Alert     // firing Alert만 필터링
func (as Alerts) Resolved() []Alert   // resolved Alert만 필터링
```

## 7. DefaultFuncs (내장 함수)

```go
// template/template.go
var DefaultFuncs = FuncMap{
    "toUpper":           strings.ToUpper,
    "toLower":           strings.ToLower,
    "title":             cases.Title(language.AmericanEnglish).String,
    "trimSpace":         strings.TrimSpace,
    "join":              strings.Join,
    "match":             regexp.MatchString,
    "safeHtml":          func(s string) template.HTML { return template.HTML(s) },
    "safeUrl":           func(s string) template.URL { return template.URL(s) },
    "urlUnescape":       url.QueryUnescape,
    "reReplaceAll":      func(pattern, repl, text string) string { ... },
    "stringSlice":       func(s ...string) []string { return s },
    "date":              func(fmt string, t time.Time) string { return t.Format(fmt) },
    "tz":                func(name string, t time.Time) (time.Time, error) { ... },
    "since":             time.Since,
    "humanizeDuration":  func(v float64) string { ... },
    "toJson":            func(v any) string { ... },
}
```

| 함수 | 설명 | 예시 |
|------|------|------|
| `toUpper` | 대문자 변환 | `{{ "hello" \| toUpper }}` → "HELLO" |
| `toLower` | 소문자 변환 | `{{ "HELLO" \| toLower }}` → "hello" |
| `title` | 제목 케이스 | `{{ "hello world" \| title }}` → "Hello World" |
| `trimSpace` | 양쪽 공백 제거 | `{{ " hi " \| trimSpace }}` → "hi" |
| `join` | 문자열 결합 | `{{ .Names \| join ", " }}` |
| `match` | 정규식 매칭 | `{{ match "^High" .AlertName }}` |
| `safeHtml` | HTML 이스케이프 방지 | `{{ .Description \| safeHtml }}` |
| `date` | 시간 포맷 | `{{ .StartsAt \| date "2006-01-02" }}` |
| `tz` | 시간대 변환 | `{{ .StartsAt \| tz "Asia/Seoul" }}` |
| `since` | 경과 시간 | `{{ .StartsAt \| since }}` |
| `humanizeDuration` | 지속 시간 포맷 | `{{ 3600.0 \| humanizeDuration }}` → "1h" |
| `toJson` | JSON 변환 | `{{ .Labels \| toJson }}` |

## 8. 렌더링 메서드

```go
func (t *Template) ExecuteTextString(text string, data any) (string, error)
func (t *Template) ExecuteHTMLString(html string, data any) (string, error)
```

```
ExecuteTextString() 흐름:
    1. text/template으로 파싱
    2. data를 전달하여 렌더링
    3. 결과 문자열 반환

ExecuteHTMLString() 흐름:
    1. html/template으로 파싱 (XSS 방지)
    2. data를 전달하여 렌더링
    3. 결과 문자열 반환
```

## 9. DeepCopyWithTemplate

```go
func DeepCopyWithTemplate(value any, tmplTextFunc TemplateFunc) (any, error)
```

구조체의 모든 문자열 필드에 Go 템플릿을 적용한다. Receiver Config의 필드(URL, 채널명 등)에 템플릿 변수를 사용할 수 있게 한다.

## 10. 기본 템플릿 (default_tmpl.go)

Alertmanager는 기본 템플릿을 내장하고 있다:

```go
// template/default_tmpl.go
const DefaultTmpl = `
{{ define "__alertmanager" }}AlertManager{{ end }}
{{ define "__alertmanagerURL" }}{{ .ExternalURL }}/#/alerts?receiver={{ .Receiver | urlquery }}{{ end }}

{{ define "__subject" }}[{{ .Status | toUpper }}{{ if eq .Status "firing" }}:{{ .Alerts.Firing | len }}{{ end }}] {{ .GroupLabels.SortedPairs.Values | join " " }} {{ if gt (len .CommonLabels) (len .GroupLabels) }}({{ with .CommonLabels.Remove .GroupLabels.Names }}{{ .Values | join " " }}{{ end }}){{ end }}{{ end }}

{{ define "__description" }}{{ end }}
{{ define "__text_alert_list" }}{{ range . }}Labels:
{{ range .Labels.SortedPairs }} - {{ .Name }} = {{ .Value }}
{{ end }}Annotations:
{{ range .Annotations.SortedPairs }} - {{ .Name }} = {{ .Value }}
{{ end }}Source: {{ .GeneratorURL }}
{{ end }}{{ end }}
...
`
```

## 11. Slack 템플릿 예시

```
{{ define "slack.default.title" }}
{{ template "__subject" . }}
{{ end }}

{{ define "slack.default.text" }}
{{ range .Alerts }}
*Alert:* {{ .Annotations.summary }}
*Description:* {{ .Annotations.description }}
*Severity:* {{ .Labels.severity }}
*Started:* {{ .StartsAt | date "2006-01-02 15:04:05" | tz "Asia/Seoul" }}
{{ end }}
{{ end }}
```

## 12. 커스텀 템플릿 사용

```yaml
# alertmanager.yml
templates:
  - '/etc/alertmanager/templates/*.tmpl'

receivers:
  - name: 'slack'
    slack_configs:
      - api_url: 'https://hooks.slack.com/...'
        title: '{{ template "custom.title" . }}'
        text: '{{ template "custom.text" . }}'
```

```
# /etc/alertmanager/templates/custom.tmpl
{{ define "custom.title" }}
[{{ .Status | toUpper }}] {{ .CommonLabels.alertname }}
{{ end }}

{{ define "custom.text" }}
{{ if eq .Status "firing" }}
🔥 *{{ .Alerts.Firing | len }}개 Alert 발생*
{{ range .Alerts.Firing }}
• {{ .Labels.alertname }}: {{ .Annotations.summary }}
  시작: {{ .StartsAt | date "15:04:05" }}
{{ end }}
{{ end }}
{{ if .Alerts.Resolved }}
✅ *{{ .Alerts.Resolved | len }}개 Alert 해제*
{{ range .Alerts.Resolved }}
• {{ .Labels.alertname }}: {{ .Annotations.summary }}
{{ end }}
{{ end }}
{{ end }}
```

## 13. 템플릿에서 사용 가능한 데이터

```
.Receiver              → "slack-channel"
.Status                → "firing" 또는 "resolved"
.Alerts                → Alert 배열
.Alerts.Firing         → firing Alert만
.Alerts.Resolved       → resolved Alert만
.GroupLabels           → KV {"alertname": "HighCPU"}
.CommonLabels          → KV (모든 Alert 공통)
.CommonAnnotations     → KV (모든 Alert 공통)
.ExternalURL           → "http://alertmanager:9093"
.NotificationReason    → "new", "resolved", "repeat"

각 Alert:
.Status                → "firing" 또는 "resolved"
.Labels                → KV {"alertname": "HighCPU", ...}
.Annotations           → KV {"summary": "...", ...}
.StartsAt              → time.Time
.EndsAt                → time.Time
.GeneratorURL          → "http://prometheus:9090/..."
.Fingerprint           → "abc123..."
```

## 14. amtool 템플릿 테스트

```bash
amtool template render \
  --template.glob='/path/to/templates/*.tmpl' \
  --template.text='{{ template "custom.text" . }}'
```

## 15. 메트릭

템플릿 엔진 자체는 별도 메트릭을 노출하지 않지만, 템플릿 렌더링 실패는 알림 전송 실패 메트릭(`alertmanager_notifications_failed_total`)에 반영된다.
