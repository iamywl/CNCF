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

## 12. 파서 내부 구현 상세 (Finite State Automata)

### 12.1 parser 구조체

```go
// matcher/parse/parse.go
type parser struct {
    matchers labels.Matchers
    hasOpenBrace bool      // 입력이 '{'로 시작했는지 추적
    lexer        lexer
}
```

**왜 hasOpenBrace를 추적하는가?** Matcher 입력은 `{alertname="HighCPU"}` 형태(중괄호 포함)와 `alertname="HighCPU"` 형태(중괄호 없음) 모두를 허용한다. 여는 중괄호가 있으면 반드시 닫는 중괄호가 있어야 하므로 이를 추적한다.

### 12.2 parseFunc 상태 전이

```go
// matcher/parse/parse.go
type parseFunc func(l *lexer) (parseFunc, error)
```

파서는 유한 상태 자동기계(FSA)로 구현된다. 각 상태는 `parseFunc`이며, 다음 상태를 반환한다:

```
상태 전이 다이어그램:

parseOpenBrace
    ├─ '{'가 있음 → acceptPeek(closeBrace)
    │   ├─ '}' 다음 → parseCloseBrace (빈 매처 세트)
    │   └─ 다른 토큰 → parseMatcher
    ├─ '{'가 없음 → parseMatcher
    └─ EOF → parseEOF

parseMatcher
    1. expect(quoted/unquoted) → 레이블 이름
    2. expect(=, !=, =~, !~)  → 연산자
    3. expect(quoted/unquoted) → 레이블 값
    4. labels.NewMatcher() 생성
    → parseEndOfMatcher

parseEndOfMatcher
    ├─ ',' → parseComma
    ├─ '}' → parseCloseBrace
    └─ EOF → parseCloseBrace

parseComma
    ├─ '}' → parseCloseBrace
    ├─ quoted/unquoted → parseMatcher (다음 매처)
    └─ EOF → parseCloseBrace

parseCloseBrace
    ├─ hasOpenBrace && '}' → parseEOF
    ├─ !hasOpenBrace && '}' → 에러 (여는 중괄호 없음)
    └─ hasOpenBrace && !'}' → 에러 (닫는 중괄호 없음)

parseEOF
    ├─ EOF → nil (정상 종료)
    └─ 다른 토큰 → 에러 (예상치 못한 입력)
```

### 12.3 accept/expect 패턴

```go
// accept: 토큰이 예상 종류 중 하나이면 true, 아니면 false (소비)
func (p *parser) accept(l *lexer, kinds ...tokenKind) (ok bool, err error)

// acceptPeek: accept와 동일하지만 토큰을 소비하지 않음
func (p *parser) acceptPeek(l *lexer, kinds ...tokenKind) (bool, error)

// expect: 토큰이 예상 종류가 아니면 에러 (소비)
func (p *parser) expect(l *lexer, kind ...tokenKind) (token, error)

// expectPeek: expect와 동일하지만 토큰을 소비하지 않음
func (p *parser) expectPeek(l *lexer, kind ...tokenKind) (token, error)
```

**왜 peek 변형이 필요한가?** 파서가 다음 토큰을 확인하여 분기를 결정하되, 아직 소비하지 않아야 하는 경우가 있다. 예를 들어 `parseEndOfMatcher`에서 다음 토큰이 `,`인지 `}`인지 확인한 후, 해당 상태에서 실제로 소비한다.

### 12.4 panic 복구

```go
// matcher/parse/parse.go
func Matchers(input string) (matchers labels.Matchers, err error) {
    defer func() {
        if r := recover(); r != nil {
            fmt.Fprintf(os.Stderr, "parser panic: %s, %s", r, debug.Stack())
            err = errors.New("parser panic: ...")
        }
    }()
    // ...
}
```

**왜 panic을 복구하는가?** 파서 내부의 예상치 못한 버그가 호출자 전체를 크래시시키지 않도록 방어한다. Alertmanager처럼 장기 실행 서버에서는 파서 오류가 프로세스 종료로 이어지면 안 되므로, panic을 에러로 변환한다.

## 13. Fallback 파서의 Disagreement 감지

### 13.1 FallbackMatcherParser 동작

```go
// matcher/compat/parse.go
func FallbackMatcherParser(l *slog.Logger) ParseMatcher {
    return func(input, origin string) (matcher *labels.Matcher, err error) {
        nMatcher, nErr := parse.Matcher(input)   // 새 UTF-8 파서
        cMatcher, cErr := labels.ParseMatcher(input) // 기존 Classic 파서

        if nErr != nil {
            if cErr != nil {
                return nil, cErr  // 둘 다 실패 → 에러
            }
            // Classic만 성공 → 경고 + Classic 결과 반환
            suggestion := cMatcher.String()
            l.Warn("... incompatible ...", "suggestion", suggestion)
            return cMatcher, nil
        }
        // 둘 다 성공했지만 결과가 다름 → disagreement
        if cErr == nil && !reflect.DeepEqual(nMatcher, cMatcher) {
            l.Warn("Matchers input has disagreement", ...)
            return cMatcher, nil  // Classic 결과 우선
        }
        return nMatcher, nil  // UTF-8 결과 반환
    }
}
```

**왜 두 파서를 모두 실행하는가?** UTF-8 파서로의 마이그레이션 기간 중 호환성을 보장하기 위함이다. 사용자가 인지하지 못하는 사이에 파싱 결과가 달라지면 라우팅 동작이 변할 수 있으므로, 불일치 시 경고 로그를 남기고 Classic 결과를 우선한다.

### 13.2 세 가지 시나리오

```
시나리오 1: 두 파서 모두 성공, 결과 동일
  → UTF-8 결과 반환 (정상 경로)

시나리오 2: UTF-8 실패, Classic 성공
  → 경고 로그 + suggestion 출력 + Classic 결과 반환
  → 사용자에게 "이 입력은 UTF-8 파서와 호환되지 않음" 알림

시나리오 3: 두 파서 모두 성공, 결과 다름 (disagreement)
  → 경고 로그 + Classic 결과 우선 반환
  → "Matchers input has disagreement" 경고
```

## 14. Matchers 정렬 규칙 상세

```go
// pkg/labels/matcher.go
func (ms Matchers) Less(i, j int) bool {
    if ms[i].Name > ms[j].Name {
        return false
    }
    if ms[i].Name < ms[j].Name {
        return true
    }
    if ms[i].Value > ms[j].Value {
        return false
    }
    if ms[i].Value < ms[j].Value {
        return true
    }
    return ms[i].Type < ms[j].Type
}
```

**정렬 우선순위**: Name → Value → Type 순서로 비교한다.

**왜 정렬이 필요한가?** Matchers의 `String()` 메서드가 일관된 문자열 표현을 생성하려면 순서가 결정적이어야 한다. 또한 Silence의 fingerprint 계산이나 nflog의 키 생성에서 동일한 매처 세트가 항상 같은 키를 생성하려면 정렬이 필수적이다.

## 15. JSON 직렬화의 v1 API 호환성

```go
// pkg/labels/matcher.go
type apiV1Matcher struct {
    Name    string `json:"name"`
    Value   string `json:"value"`
    IsRegex bool   `json:"isRegex"`
    IsEqual bool   `json:"isEqual"`
}

func (m Matcher) MarshalJSON() ([]byte, error) {
    return json.Marshal(apiV1Matcher{
        Name:    m.Name,
        Value:   m.Value,
        IsRegex: m.Type == MatchRegexp || m.Type == MatchNotRegexp,
        IsEqual: m.Type == MatchRegexp || m.Type == MatchEqual,
    })
}
```

**왜 이 변환이 필요한가?** v1 API는 `IsRegex` + `IsEqual` 불리언 조합으로 매치 타입을 표현했다. v2 내부에서는 `MatchType` 열거형을 사용하지만, JSON 직렬화는 v1 호환성을 유지한다:

```
MatchType       → IsEqual  IsRegex
MatchEqual      → true     false
MatchNotEqual   → false    false
MatchRegexp     → true     true
MatchNotRegexp  → false    true
```

`UnmarshalJSON`에서도 기본값이 `IsEqual: true`로 설정되어, `isEqual` 필드가 없는 오래된 JSON도 올바르게 파싱된다.

## 16. isReserved 함수와 OpenMetrics 이스케이프

```go
// pkg/labels/matcher.go
func isReserved(r rune) bool {
    return unicode.IsSpace(r) || strings.ContainsRune("{}!=~,\\\"'`", r)
}
```

이 함수는 레이블 이름에 예약 문자가 포함되어 있는지 확인한다. `String()` 메서드에서 사용되며, 예약 문자가 있으면 `strconv.Quote()`로 이름을 감싸고, 없으면 그대로 출력한다:

```go
func (m *Matcher) String() string {
    if strings.ContainsFunc(m.Name, isReserved) {
        return fmt.Sprintf(`%s%s%s`,
            strconv.Quote(m.Name), m.Type, strconv.Quote(m.Value))
    }
    return fmt.Sprintf(`%s%s"%s"`,
        m.Name, m.Type, openMetricsEscape(m.Value))
}
```

**왜 두 가지 출력 형식인가?** UTF-8 전환 기간에 Classic 파서와 UTF-8 파서 모두가 읽을 수 있는 형식을 생성해야 한다. Classic 파서는 레이블 이름 주위의 따옴표를 이해하지 못하므로, 예약 문자가 없는 일반적인 경우에는 따옴표 없이 출력한다.

## 17. 성능 고려사항

### 17.1 정규식 컴파일 캐싱

정규식 Matcher는 생성 시점에 `regexp.Compile()`을 한 번만 수행하고 `re` 필드에 캐시한다. Alert 매칭은 초당 수천 번 발생할 수 있으므로, 매 호출마다 정규식을 컴파일하면 심각한 성능 저하가 발생한다.

### 17.2 Matchers AND 로직의 Short-Circuit

```go
func (ms Matchers) Matches(lset model.LabelSet) bool {
    for _, m := range ms {
        if !m.Matches(string(lset[model.LabelName(m.Name)])) {
            return false  // 첫 불일치에서 즉시 반환
        }
    }
    return true
}
```

AND 로직에서 첫 번째 불일치 시 즉시 `false`를 반환하여 나머지 매처를 평가하지 않는다. 정규식 매처가 비용이 높으므로, 단순 등호 매처를 먼저 배치하면 성능이 향상된다.

### 17.3 MatcherSet OR 로직의 Early Return

```go
func (ms MatcherSet) Matches(lset model.LabelSet) bool {
    for _, matchers := range ms {
        if (*matchers).Matches(lset) {
            return true  // 첫 일치에서 즉시 반환
        }
    }
    return false
}
```

### 17.4 Fallback 파서의 이중 파싱 비용

Fallback 모드에서는 모든 입력을 UTF-8과 Classic 두 파서로 파싱한다. 이는 설정 리로드 시에만 발생하므로 런타임 성능에는 영향이 없지만, 대규모 설정 파일에서는 리로드 시간이 늘어날 수 있다. `utf8-strict-mode`로 전환하면 이중 파싱을 피할 수 있다.

## 18. 테스트 전략

### 18.1 파서 퍼즈 테스트

UTF-8 파서의 견고성을 검증하기 위해 퍼즈 테스트가 사용된다. 무작위 입력에 대해 파서가 panic 없이 동작하는지, Classic 파서와 결과가 일치하는지를 확인한다.

### 18.2 Disagreement 테스트

Fallback 파서의 핵심 테스트 시나리오:
- 두 파서가 동일한 결과를 생성하는 입력
- UTF-8 파서만 성공하는 입력 (UTF-8 레이블 이름)
- Classic 파서만 성공하는 입력 (하위 호환 형식)
- 두 파서가 다른 결과를 생성하는 입력 (disagreement)

### 18.3 에러 메시지 품질

파서 에러에는 위치 정보가 포함된다:

```
0:5: unexpected "!": expected end of input
3:7: "invalid": expected an operator such as '=', '!=', '=~' or '!~'
```

`columnStart:columnEnd` 형식으로 정확한 에러 위치를 알려주어, 사용자가 설정 파일의 문법 오류를 쉽게 찾을 수 있다.
