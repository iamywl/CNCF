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

## 16. 실제 소스 코드 심화 분석

### 16.1 Template 생성 옵션

```go
// template/template.go
func New(options ...Option) (*Template, error) {
    t := &Template{
        text: tmpltext.New("").Option("missingkey=zero"),
        html: tmplhtml.New("").Option("missingkey=zero"),
    }
    for _, o := range options {
        o(t.text, t.html)
    }
    t.text.Funcs(tmpltext.FuncMap(DefaultFuncs))
    t.html.Funcs(tmplhtml.FuncMap(DefaultFuncs))
    return t, nil
}
```

**왜 `missingkey=zero` 옵션인가?**

템플릿에서 존재하지 않는 키를 참조하면, Go 기본 동작은 `<no value>`를 출력한다. `missingkey=zero`로 설정하면 해당 타입의 zero 값(빈 문자열, 0 등)을 반환한다. 알림 메시지에서 `<no value>`가 표시되면 사용자에게 혼란을 주므로, 빈 값으로 대체하는 것이 낫다.

### 16.2 내장 템플릿 로딩 (embed.FS)

```go
// template/template.go
//go:embed default.tmpl email.tmpl
var asset embed.FS

func FromGlobs(paths []string, options ...Option) (*Template, error) {
    t, err := New(options...)

    // 내장 템플릿을 먼저 로드
    defaultTemplates := []string{"default.tmpl", "email.tmpl"}
    for _, file := range defaultTemplates {
        f, err := asset.Open(file)
        if err := t.Parse(f); err != nil {
            return nil, err
        }
    }

    // 사용자 템플릿을 그 위에 로드 (덮어쓰기 가능)
    for _, tp := range paths {
        if err := t.FromGlob(tp); err != nil {
            return nil, err
        }
    }
    return t, nil
}
```

**왜 `embed.FS`를 사용하는가?**

Go 1.16의 `embed` 패키지로 기본 템플릿 파일을 바이너리에 내장한다. 별도 파일 배포 없이 바이너리만으로 동작할 수 있다. 사용자 템플릿이 기본 템플릿을 덮어쓸 수 있어, 커스터마이즈가 자유롭다.

### 16.3 Data 생성 — CommonLabels 계산 알고리즘

```
Template.Data(recv, groupLabels, reason, alerts...):
    1. 각 Alert → Alert 구조체 변환 (Labels → KV)

    2. Status 결정:
       firing := false
       for each alert:
           if alert.Status == "firing":
               firing = true
       status = firing ? "firing" : "resolved"

    3. CommonLabels 계산 (교집합):
       commonLabels = alerts[0].Labels의 복사본
       for each alert (1부터):
           for key, val in commonLabels:
               if alert.Labels[key] != val:
                   delete(commonLabels, key)
       결과: 모든 Alert에 공통인 레이블만 남음

    4. CommonAnnotations도 동일 알고리즘

    5. GroupLabels 설정 (Dispatcher에서 전달받음)
```

**왜 CommonLabels와 CommonAnnotations을 계산하는가?**

알림 메시지에서 "이 그룹의 모든 Alert에 공통인 정보"를 표시하면, 각 Alert를 나열하지 않아도 핵심 정보를 전달할 수 있다. 예를 들어 `severity=critical`이 공통이면 메시지 제목에 한 번만 표시하면 된다.

---

## 17. 에러 처리 패턴

### 17.1 템플릿 파싱 에러

```
FromGlobs() 에러 케이스:
    1. 내장 템플릿 오류 → 바이너리 빌드 시 발견
    2. Glob 패턴 매칭 실패 → 에러 반환
    3. 사용자 템플릿 구문 오류 → 에러 반환
    4. 설정 리로드 시 파싱 실패 → 기존 템플릿 유지
```

### 17.2 렌더링 에러

```
ExecuteTextString() 에러 케이스:
    1. 템플릿 구문 오류 → error 반환
    2. 함수 호출 실패 (예: 잘못된 정규식) → error 반환
    3. nil 포인터 참조 → missingkey=zero로 안전하게 처리
    4. 무한 루프 템플릿 → Go 런타임 제한에 의존

에러 전파:
    렌더링 실패 → Notifier.Notify() 실패
    → RetryStage에서 재시도
    → 실패 시 alertmanager_notifications_failed_total 증가
```

---

## 18. DeepCopyWithTemplate 상세

```go
// template/template.go
func DeepCopyWithTemplate(value any, tmplTextFunc TemplateFunc) (any, error)
```

이 함수는 Receiver Config 구조체의 모든 문자열 필드에 템플릿을 적용한다:

```
예시:
    WebhookConfig{
        URL: "https://hooks.example.com/{{ .CommonLabels.alertname }}",
    }

    DeepCopyWithTemplate 적용 후:
    WebhookConfig{
        URL: "https://hooks.example.com/HighCPU",
    }
```

리플렉션을 사용하여 구조체를 재귀적으로 순회하며, `string` 타입 필드에 템플릿을 적용한다. 이를 통해 URL, 채널명, 제목 등에 동적 값을 삽입할 수 있다.

---

## 19. 성능 고려사항

| 항목 | 설계 | 이유 |
|------|------|------|
| 정규식 컴파일 캐시 | DefaultFuncs의 `match`는 매번 `regexp.MatchString` 호출 | Go stdlib이 내부적으로 캐싱 |
| 템플릿 파싱 1회 | FromGlobs에서 파싱, 이후 재사용 | 매 렌더링마다 파싱하면 성능 저하 |
| HTML 이스케이프 | html/template 사용 | XSS 방지 (이메일 등) |
| missingkey=zero | 누락 키에 대해 zero 값 반환 | 에러 대신 빈 값으로 안전하게 처리 |

---

## 20. 커스텀 함수 추가 방법

```go
// Option 패턴으로 커스텀 함수 추가
tmpl, err := template.New(func(text *tmpltext.Template, html *tmplhtml.Template) {
    text.Funcs(tmpltext.FuncMap{
        "myFunc": func(s string) string { return strings.ToUpper(s) },
    })
    html.Funcs(tmplhtml.FuncMap{
        "myFunc": func(s string) string { return strings.ToUpper(s) },
    })
})
```

**주의**: DefaultFuncs는 Option 이후에 등록되므로, 기본 함수와 동일한 이름의 커스텀 함수는 DefaultFuncs에 의해 덮어쓰여진다. 커스텀 함수 이름은 기본 함수와 다르게 지정해야 한다.

---

## 21. YAML 직렬화 지원

```go
// template/template.go
var DefaultFuncs = FuncMap{
    // ...
    "toJson": func(v any) string {
        bytes, _ := json.Marshal(v)
        return string(bytes)
    },
    // YAML은 기본 함수에 없지만, yaml.v2 패키지를 import하여 사용 가능
}
```

`toJson` 함수는 PagerDuty, Webhook 등 JSON 기반 API에서 Alert 데이터를 직렬화할 때 유용하다. YAML은 기본 제공되지 않으므로, 필요하면 커스텀 함수로 추가해야 한다.

## 17. 템플릿 디버깅 가이드

템플릿 오류는 알림 전송 실패의 주요 원인 중 하나다. 디버깅 시 다음 단계를 따른다:

| 단계 | 방법 | 설명 |
|------|------|------|
| 1 | 로그 확인 | `level=error component=notify` 로그에서 템플릿 실행 에러 확인 |
| 2 | amtool 검증 | `amtool check-config alertmanager.yml`로 설정 파일 문법 검증 |
| 3 | 테스트 데이터 | 샘플 Alert 데이터로 템플릿 렌더링 결과 미리 확인 |
| 4 | 단계적 빌드 | 복잡한 템플릿은 `{{ define }}` 블록으로 분리하여 개별 테스트 |

```
# 템플릿 실행 흐름
template/template.go → Template.ExecuteTextString()
  → Go text/template.Execute()
    → FuncMap 함수 바인딩
    → Alert/KV 데이터 주입
    → 최종 문자열 렌더링
```

템플릿 내에서 `.CommonLabels`, `.CommonAnnotations`는 그룹 내 모든 알림이 공유하는 레이블/어노테이션만 포함하므로, 개별 알림 데이터는 `.Alerts` 배열을 순회하여 접근해야 한다.
