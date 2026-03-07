# Alertmanager Matcher 시스템 Deep Dive

## 1. 개요

Matcher 시스템은 Alert의 레이블(Labels)을 조건에 따라 매칭하는 핵심 메커니즘이다. Route 매칭, Silence 매칭, Inhibition 매칭, API 필터링 등 Alertmanager 전반에서 사용된다. `pkg/labels/matcher.go`, `matcher/parse/`, `matcher/compat/`에 구현되어 있다.

## 2. Matcher 구조체

```go
// pkg/labels/matcher.go
type MatchType int

const (
    MatchEqual     MatchType = iota   // =  (정확 일치)
    MatchNotEqual                      // != (불일치)
    MatchRegexp                        // =~ (정규식 일치)
    MatchNotRegexp                     // !~ (정규식 불일치)
)

type Matcher struct {
    Type  MatchType
    Name  string           // 레이블 이름
    Value string           // 매칭 값 (또는 정규식 패턴)
    re    *regexp.Regexp   // 컴파일된 정규식 (캐시)
}
```

### 2.1 생성

```go
func NewMatcher(t MatchType, n, v string) (*Matcher, error)
```

정규식 타입(MatchRegexp, MatchNotRegexp)이면 `regexp.Compile()`로 정규식을 컴파일하여 `re` 필드에 캐시한다.

### 2.2 Matches() — 단일 값 매칭

```go
func (m *Matcher) Matches(s string) bool
```

```
MatchEqual:     s == m.Value
MatchNotEqual:  s != m.Value
MatchRegexp:    m.re.MatchString(s)
MatchNotRegexp: !m.re.MatchString(s)
```

### 2.3 String() — 문자열 표현

```go
func (m *Matcher) String() string
// 예: alertname="HighCPU", severity!="info", instance=~".+node1"
```

## 3. Matchers (복수)

```go
// pkg/labels/matcher.go
type Matchers []*Matcher

func (ms Matchers) Matches(lset model.LabelSet) bool {
    for _, m := range ms {
        if !m.Matches(string(lset[model.LabelName(m.Name)])) {
            return false
        }
    }
    return true
}
```

**AND 로직**: 모든 Matcher가 일치해야 true를 반환한다.

### 3.1 정렬

```go
func (ms Matchers) Len() int
func (ms Matchers) Swap(i, j int)
func (ms Matchers) Less(i, j int) bool  // Name 기준 정렬
```

### 3.2 JSON 직렬화

```go
func (m *Matcher) MarshalJSON() ([]byte, error)
func (m *Matcher) UnmarshalJSON(data []byte) error
```

API에서 Matcher를 JSON으로 주고받을 때 사용된다.

## 4. MatcherSet (OR 로직)

```go
// pkg/labels/matcher.go
type MatcherSet []*Matchers

func (ms MatcherSet) Matches(lset model.LabelSet) bool {
    for _, m := range ms {
        if m.Matches(lset) {
            return true  // 하나라도 매칭되면 true
        }
    }
    return false
}
```

Silence에서 여러 Matcher 세트 중 하나라도 매칭되면 억제하는 OR 로직에 사용된다.

## 5. UTF-8 파서 (matcher/parse/)

### 5.1 구조

```
matcher/parse/
├── parse.go    # 파서 (Matchers(), Matcher())
├── lexer.go    # 토큰 스캐너
└── token.go    # 토큰 정의
```

### 5.2 token 정의

```go
// matcher/parse/token.go
type tokenKind int

const (
    tokenEOF tokenKind = iota
    tokenOpenBrace      // {
    tokenCloseBrace     // }
    tokenComma          // ,
    tokenEquals         // =
    tokenNotEquals      // !=
    tokenMatches        // =~
    tokenNotMatches     // !~
    tokenQuoted         // "value"
    tokenUnquoted       // value (따옴표 없음)
)

type token struct {
    kind     tokenKind
    value    string
    position              // 위치 정보
}

type position struct {
    offsetStart int
    offsetEnd   int
    columnStart int
    columnEnd   int
}
```

### 5.3 lexer (토큰 스캐너)

```go
// matcher/parse/lexer.go
type lexer struct {
    input  string
    err    error
    start  int      // 현재 토큰 시작 오프셋
    pos    int      // 현재 커서 위치
    width  int      // 마지막 rune 너비
    column int
    cols   int
}

func (l *lexer) scan() (token, error)
func (l *lexer) scanOperator() (token, error)
func (l *lexer) scanQuoted() (token, error)
func (l *lexer) scanUnquoted() (token, error)
```

### 5.4 parser

```go
// matcher/parse/parse.go
func Matchers(input string) (labels.Matchers, error)
func Matcher(input string) (*labels.Matcher, error)
```

```
파싱 흐름:
    입력: '{alertname="HighCPU", severity=~"crit.*"}'

    1. lexer.scan() → tokenOpenBrace '{'
    2. lexer.scan() → tokenUnquoted 'alertname'
    3. lexer.scan() → tokenEquals '='
    4. lexer.scan() → tokenQuoted 'HighCPU'
       → Matcher{MatchEqual, "alertname", "HighCPU"}
    5. lexer.scan() → tokenComma ','
    6. lexer.scan() → tokenUnquoted 'severity'
    7. lexer.scan() → tokenMatches '=~'
    8. lexer.scan() → tokenQuoted 'crit.*'
       → Matcher{MatchRegexp, "severity", "crit.*"}
    9. lexer.scan() → tokenCloseBrace '}'
    10. lexer.scan() → tokenEOF

    결과: [Matcher{=, alertname, HighCPU}, Matcher{=~, severity, crit.*}]
```

### 5.5 에러 타입

```go
// matcher/parse/lexer.go
type expectedError struct { ... }      // 예상 토큰 불일치
type invalidInputError struct { ... }  // 유효하지 않은 입력
type unterminatedError struct { ... }  // 닫히지 않은 따옴표
```

## 6. 호환성 레이어 (matcher/compat/)

### 6.1 파서 팩토리

```go
// matcher/compat/parse.go
type ParseMatcher func(input, origin string) (*labels.Matcher, error)
type ParseMatchers func(input, origin string) (labels.Matchers, error)
```

### 6.2 세 가지 파서 모드

```go
func ClassicMatcherParser(l *slog.Logger) ParseMatcher    // 기존 파서
func UTF8MatcherParser(l *slog.Logger) ParseMatcher      // 새 UTF-8 파서
func FallbackMatcherParser(l *slog.Logger) ParseMatcher  // 폴백: 둘 다 시도
```

```
┌─────────────────────────────────────────┐
│  FeatureControl 기반 파서 선택           │
│                                         │
│  classic-mode    → ClassicMatcherParser │
│  utf8-strict-mode → UTF8MatcherParser  │
│  (기본)           → FallbackMatcherParser│
│                                         │
│  FallbackMatcherParser:                 │
│    1. UTF8 파서로 시도                   │
│    2. 실패하면 Classic 파서로 시도        │
│    3. 둘 다 실패하면 에러                │
└─────────────────────────────────────────┘
```

### 6.3 InitFromFlags

```go
func InitFromFlags(l *slog.Logger, f featurecontrol.Flagger)
```

Feature flag에 따라 전역 파서를 설정한다:
- `FeatureClassicMode` → Classic 파서만
- `FeatureUTF8StrictMode` → UTF-8 파서만
- 기본 → Fallback (UTF-8 우선, Classic 폴백)

## 7. 사용처별 매칭

### 7.1 Route 매칭

```go
// dispatch/route.go
func (r *Route) Match(lset model.LabelSet) []*Route {
    if !r.Matchers.Matches(lset) {
        return nil
    }
    // ...
}
```

### 7.2 Silence 매칭

```go
// silence/silence.go
// Silence의 MatcherSet이 Alert Labels와 매칭되는지 확인
```

### 7.3 Inhibition 매칭

```go
// inhibit/inhibit.go
// SourceMatchers, TargetMatchers가 Alert Labels와 매칭되는지 확인
```

### 7.4 API 필터링

```go
// api/v2/api.go
// 쿼리 파라미터의 filter[]를 Matcher로 파싱하여 Alert 필터링
```

## 8. 매칭 예시

```
Alert Labels: {alertname="HighCPU", severity="critical", instance="node-1", cluster="prod"}

Matcher: alertname="HighCPU"
  → MatchEqual: "HighCPU" == "HighCPU" → true

Matcher: severity!="info"
  → MatchNotEqual: "critical" != "info" → true

Matcher: instance=~"node-.*"
  → MatchRegexp: "node-1" matches "node-.*" → true

Matcher: cluster!~"dev|staging"
  → MatchNotRegexp: "prod" not matches "dev|staging" → true

Matchers: [alertname="HighCPU", severity="critical"]
  → AND: true && true → true

Matchers: [alertname="HighCPU", severity="warning"]
  → AND: true && false → false
```

## 9. 정규식 최적화

정규식 Matcher는 생성 시 `regexp.Compile()`로 컴파일되어 `re` 필드에 캐시된다. 매 매칭 시 재컴파일하지 않아 성능이 향상된다.

```go
func NewMatcher(t MatchType, n, v string) (*Matcher, error) {
    m := &Matcher{Type: t, Name: n, Value: v}
    if t == MatchRegexp || t == MatchNotRegexp {
        re, err := regexp.Compile("^(?:" + v + ")$")
        if err != nil {
            return nil, err
        }
        m.re = re
    }
    return m, nil
}
```

**주의**: 정규식은 `^(?:...)$`로 감싸져 전체 문자열 매칭이 강제된다.

## 10. UTF-8 지원

UTF-8 모드에서는 레이블 이름과 값에 UTF-8 문자를 사용할 수 있다:

```
기존 (Classic): alertname, severity (ASCII만)
UTF-8: 경고이름, 심각도 (한국어 등 지원)
```

Feature flag `utf8-strict-mode`로 활성화한다.

## 11. YAML 설정에서의 사용

```yaml
# 새로운 방식 (matchers)
route:
  routes:
    - matchers:
        - alertname="HighCPU"
        - severity=~"critical|warning"
      receiver: 'pager'

# 기존 방식 (deprecated)
route:
  routes:
    - match:
        alertname: HighCPU
      match_re:
        severity: "critical|warning"
      receiver: 'pager'
```
