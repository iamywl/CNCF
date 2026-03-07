# 07. LogQL 쿼리 엔진 Deep-Dive

## 목차

1. [LogQL 문법 개요](#1-logql-문법-개요)
2. [렉서(Lexer)](#2-렉서lexer)
3. [파서(Parser)와 AST 생성](#3-파서parser와-ast-생성)
4. [AST 노드 타입](#4-ast-노드-타입)
5. [파이프라인 스테이지](#5-파이프라인-스테이지)
6. [쿼리 엔진](#6-쿼리-엔진)
7. [평가기(Evaluator)](#7-평가기evaluator)
8. [쿼리 샤딩](#8-쿼리-샤딩)
9. [최적화](#9-최적화)
10. [내부 동작 흐름 종합](#10-내부-동작-흐름-종합)

---

## 1. LogQL 문법 개요

LogQL은 Grafana Loki의 쿼리 언어로, PromQL에서 영감을 받아 로그 데이터에 특화된 문법을 제공한다. LogQL은 크게 두 가지 유형의 쿼리를 지원한다.

### 1.1 로그 쿼리 (Log Query)

로그 쿼리는 로그 라인 자체를 반환한다. 스트림 셀렉터와 파이프라인의 조합으로 구성된다.

```
{app="nginx"} |= "error" | json | line_format "{{.message}}"
```

구조:
```
로그 쿼리 = 스트림 셀렉터 [파이프라인]

스트림 셀렉터: {label=value, ...}
파이프라인:    | 스테이지1 | 스테이지2 | ...
```

스트림 셀렉터에서 사용 가능한 매칭 연산자:

| 연산자 | 의미 | 예시 |
|--------|------|------|
| `=`    | 정확히 일치 | `{app="nginx"}` |
| `!=`   | 일치하지 않음 | `{app!="nginx"}` |
| `=~`   | 정규식 일치 | `{app=~"ng.*"}` |
| `!~`   | 정규식 불일치 | `{app!~"ng.*"}` |

### 1.2 메트릭 쿼리 (Metric Query)

메트릭 쿼리는 로그로부터 수치 값을 계산하여 시계열 데이터를 반환한다.

```
rate({app="nginx"} |= "error" [5m])
```

메트릭 쿼리는 다시 두 유형으로 나뉜다:

**범위 집계 (Range Aggregation):**
```
rate(...)           # 초당 로그 라인 수
count_over_time()   # 시간 범위 내 로그 라인 수
bytes_rate()        # 초당 바이트 수
bytes_over_time()   # 시간 범위 내 바이트 수
avg_over_time()     # 시간 범위 내 평균
sum_over_time()     # 시간 범위 내 합
min_over_time()     # 시간 범위 내 최솟값
max_over_time()     # 시간 범위 내 최댓값
first_over_time()   # 시간 범위 내 첫 번째 값
last_over_time()    # 시간 범위 내 마지막 값
quantile_over_time()# 시간 범위 내 분위수
```

**벡터 집계 (Vector Aggregation):**
```
sum(...)            # 합계
avg(...)            # 평균
max(...)            # 최대
min(...)            # 최소
count(...)          # 개수
topk(k, ...)        # 상위 k개
bottomk(k, ...)     # 하위 k개
sort(...)           # 정렬
sort_desc(...)      # 역순 정렬
```

### 1.3 쿼리 문법의 전체 구조

```
┌──────────────────────────────────────────────────────────────┐
│                        LogQL 쿼리                            │
├────────────────────────┬─────────────────────────────────────┤
│     로그 쿼리           │          메트릭 쿼리                │
│                        │                                     │
│  스트림셀렉터 [파이프라인] │  범위집계(스트림셀렉터 [파이프라인] │
│                        │           [범위])                   │
│                        │  벡터집계(메트릭쿼리)                │
│                        │           [by/without (라벨)]       │
└────────────────────────┴─────────────────────────────────────┘
```

---

## 2. 렉서(Lexer)

렉서는 LogQL 문자열을 토큰(token)으로 분해하는 역할을 한다.

### 2.1 렉서 구조체

파일: `pkg/logql/syntax/lex.go`

```go
type lexer struct {
    Scanner
    errs    []logqlmodel.ParseError
    builder strings.Builder
}
```

`lexer`는 Go 표준 라이브러리의 `text/scanner.Scanner`를 내장하며, 파싱 에러 목록과 문자열 빌더를 가진다.

### 2.2 토큰 정의

렉서는 두 종류의 토큰 맵을 관리한다:

**일반 토큰 (`tokens` 맵):**

```go
// pkg/logql/syntax/lex.go
var tokens = map[string]int{
    ",":            COMMA,
    ".":            DOT,
    "{":            OPEN_BRACE,
    "}":            CLOSE_BRACE,
    "=":            EQ,
    OpTypeNEQ:      NEQ,        // "!="
    "=~":           RE,
    "!~":           NRE,
    "|=":           PIPE_EXACT,
    "|~":           PIPE_MATCH,
    "|>":           PIPE_PATTERN,
    OpPipe:         PIPE,       // "|"
    OpUnwrap:       UNWRAP,
    "by":           BY,
    "without":      WITHOUT,
    // ... 바이너리 연산자, 파서 키워드 등
}
```

**함수 토큰 (`functionTokens` 맵):**

```go
var functionTokens = map[string]int{
    // 범위 벡터 연산
    OpRangeTypeRate:        RATE,            // "rate"
    OpRangeTypeCount:       COUNT_OVER_TIME, // "count_over_time"
    OpRangeTypeBytesRate:   BYTES_RATE,      // "bytes_rate"
    OpRangeTypeAvg:         AVG_OVER_TIME,   // "avg_over_time"
    // ...
    // 벡터 연산
    OpTypeSum:      SUM,       // "sum"
    OpTypeAvg:      AVG,       // "avg"
    OpTypeTopK:     TOPK,      // "topk"
    // ...
}
```

함수 토큰은 `isFunction()` 검사를 통해 식별자(identifier)와 구분된다. 함수는 반드시 뒤에 `(`가 오거나, `by`/`without` 키워드가 뒤따라야 한다.

### 2.3 Lex() 메서드의 토큰화 과정

```go
func (l *lexer) Lex(lval *syntaxSymType) int {
    r := l.Scan()

    switch r {
    case '#':
        // 주석: 개행까지 건너뛰기
        for next := l.Peek(); next != '\n' && next != scanner.EOF; next = l.Next() {
        }
        return l.Lex(lval) // 재귀 호출로 다음 토큰 반환

    case scanner.EOF:
        return 0

    case scanner.Int, scanner.Float:
        numberText := l.TokenText()
        // 1) Duration 시도 (예: "5m", "1h30m")
        duration, ok := tryScanDuration(numberText, &l.Scanner)
        if ok {
            lval.dur = duration
            return DURATION
        }
        // 2) Bytes 시도 (예: "100MB", "1GiB")
        bytes, ok := tryScanBytes(numberText, &l.Scanner)
        if ok {
            lval.bytes = bytes
            return BYTES
        }
        // 3) 그냥 숫자
        lval.str = numberText
        return NUMBER

    case scanner.String, scanner.RawString:
        lval.str, _ = strutil.Unquote(tokenText)
        return STRING
    }

    // '[' 뒤에 오는 duration range 처리: [5m]
    if r == '[' {
        // ']'까지 읽어서 RANGE 토큰 반환
    }

    // 함수 토큰과 일반 토큰 매칭
    tokenTextLower := strings.ToLower(l.TokenText())
    if tok, ok := functionTokens[tokenTextLower]; ok {
        if isFunction(l.Scanner) {
            return tok
        }
        // 함수가 아니면 식별자로 반환
        lval.str = tokenText
        return IDENTIFIER
    }
    // ...
}
```

### 2.4 Duration 및 Bytes 파싱

렉서는 숫자 다음에 오는 접미사를 검사하여 duration이나 bytes 값을 인식한다:

```go
func tryScanDuration(number string, l *Scanner) (time.Duration, bool) {
    // 스캐너 복사본에서 시험적으로 다음 문자들을 읽음
    // 성공하면 원본 스캐너도 전진, 실패하면 원본은 그대로
}

func isDurationRune(r rune) bool {
    switch r {
    case 'n', 'u', 'µ', 'm', 's', 'h', 'd', 'w', 'y':
        return true
    }
    return false
}
```

이 설계의 핵심은 "스캐너 복사본에서 먼저 시도"하는 점이다. 만약 duration이 아닌 것으로 판명되면 원본 스캐너는 전혀 전진하지 않으므로, 다음 토큰 인식에 영향을 주지 않는다.

### 2.5 토큰화 흐름 다이어그램

```
입력: rate({app="nginx"} |= "error" [5m])

┌─────┐   ┌─────┐   ┌─────┐   ┌────┐   ┌──────┐   ┌────┐   ┌──────┐   ┌──────┐   ┌────┐   ┌─────┐
│RATE │ → │  (  │ → │  {  │ → │app │ → │  =   │ → │ "  │ → │nginx │ → │  "   │ → │ }  │ → │ |=  │ ...
│func │   │OPEN │   │OPEN │   │IDENT│  │  EQ  │   │    │   │STRING│   │      │   │CLOSE│  │PIPE │
│TOKEN│   │PAREN│   │BRACE│   │    │   │      │   │    │   │      │   │      │   │BRACE│  │EXACT│
└─────┘   └─────┘   └─────┘   └────┘   └──────┘   └────┘   └──────┘   └──────┘   └────┘   └─────┘
```

---

## 3. 파서(Parser)와 AST 생성

### 3.1 파서 구조

파일: `pkg/logql/syntax/parser.go`

Loki는 `goyacc`로 생성된 LALR(1) 파서를 사용한다. 문법 정의 파일은 `pkg/logql/syntax/syntax.y`이다.

```go
type parser struct {
    p *syntaxParserImpl    // yacc가 생성한 파서 구현체
    *lexer                 // 렉서 (토큰 공급)
    expr Expr              // 파싱 결과 AST
    *strings.Reader        // 입력 문자열 리더
}
```

파서 인스턴스는 성능을 위해 `sync.Pool`로 재사용된다:

```go
var parserPool = sync.Pool{
    New: func() interface{} {
        p := &parser{
            p:      &syntaxParserImpl{},
            Reader: strings.NewReader(""),
            lexer:  &lexer{},
        }
        return p
    },
}
```

### 3.2 파싱 과정

```go
// pkg/logql/syntax/parser.go
func ParseExpr(input string) (Expr, error) {
    expr, err := ParseExprWithoutValidation(input)
    if err != nil {
        return nil, err
    }
    if err := validateExpr(expr); err != nil {
        return nil, err
    }
    return expr, nil
}

func ParseExprWithoutValidation(input string) (expr Expr, err error) {
    if len(input) >= maxInputSize {  // 128KB 제한
        return nil, logqlmodel.NewParseError(...)
    }

    p := parserPool.Get().(*parser)
    defer parserPool.Put(p)

    p.Reset(input)
    p.Init(p.Reader)
    return p.Parse()
}
```

파싱 흐름:

```
문자열 입력
    │
    ▼
┌─────────────┐
│ ParseExpr() │
└──────┬──────┘
       │
       ▼
┌────────────────────────┐
│ParseExprWithoutValidation│
│  1. 입력 크기 체크        │
│  2. 파서 풀에서 가져오기   │
│  3. 렉서 초기화           │
│  4. yacc 파서 실행        │
└──────────┬─────────────┘
           │
           ▼
┌──────────────────┐
│ validateExpr()    │
│  - 매처 검증       │
│  - SampleExpr 검증 │
│  - 정렬 그룹핑 검증 │
└──────────┬───────┘
           │
           ▼
      AST (Expr)
```

### 3.3 검증 규칙

파싱 후 검증에서 수행하는 주요 체크:

```go
func validateMatchers(matchers []*labels.Matcher) error {
    _, matchers = util.SplitFiltersAndMatchers(matchers)
    if len(matchers) == 0 {
        return logqlmodel.NewParseError(
            errAtleastOneEqualityMatcherRequired, 0, 0)
    }
    return nil
}
```

**핵심 규칙**: 모든 쿼리는 최소 하나의 비어있지 않은 등호/정규식 매처를 포함해야 한다. `{app=~".*"}`처럼 모든 스트림에 매칭되는 쿼리는 거부된다.

---

## 4. AST 노드 타입

### 4.1 인터페이스 계층

파일: `pkg/logql/syntax/ast.go`

```
                        Expr (최상위)
                       /            \
           LogSelectorExpr        SampleExpr
          /        \              /        \
  MatchersExpr  PipelineExpr  RangeAggregationExpr  VectorAggregationExpr
                                                     LiteralExpr
                                                     VectorExpr
                                                     LabelReplaceExpr
```

**Expr 인터페이스 (모든 AST 노드의 기반):**

```go
// pkg/logql/syntax/ast.go
type Expr interface {
    Shardable(topLevel bool) bool  // 샤딩 가능 여부
    Walkable                        // AST 순회
    AcceptVisitor                   // 비지터 패턴
    fmt.Stringer                    // 문자열 표현
    Pretty(level int) string        // 포맷팅된 문자열
    isExpr()                        // 인터페이스 마커
}
```

**LogSelectorExpr 인터페이스:**

```go
type LogSelectorExpr interface {
    Matchers() []*labels.Matcher  // 스트림 매처 반환
    Pipeline() (Pipeline, error)  // 파이프라인 반환
    HasFilter() bool              // 필터 존재 여부
    Expr
}
```

**SampleExpr 인터페이스:**

```go
type SampleExpr interface {
    Selector() (LogSelectorExpr, error)           // 로그 셀렉터
    Extractors() ([]SampleExtractor, error)       // 샘플 추출기
    MatcherGroups() ([]MatcherRange, error)       // 매처 그룹
    Expr
}
```

### 4.2 MatchersExpr (스트림 셀렉터)

```go
type MatchersExpr struct {
    Mts []*labels.Matcher
}
```

가장 기본적인 노드로, `{app="nginx", env="prod"}` 같은 레이블 매처 집합을 나타낸다.

```go
func (e *MatchersExpr) Pipeline() (log.Pipeline, error) {
    return log.NewNoopPipeline(), nil  // 파이프라인 없음
}

func (e *MatchersExpr) HasFilter() bool {
    return false
}
```

### 4.3 PipelineExpr (파이프라인)

```go
type PipelineExpr struct {
    MultiStages MultiStageExpr  // 파이프라인 스테이지 목록
    Left        *MatchersExpr   // 스트림 셀렉터
}
```

`{app="nginx"} |= "error" | json | status >= 400` 같은 쿼리를 나타낸다.

`MultiStageExpr`는 `[]StageExpr`의 타입 별칭이다:

```go
type MultiStageExpr []StageExpr
```

### 4.4 LineFilterExpr (라인 필터)

```go
type LineFilterExpr struct {
    LineFilter
    Left      *LineFilterExpr  // 체인된 이전 필터
    Or        *LineFilterExpr  // OR 연결된 필터
    IsOrChild bool
}

type LineFilter struct {
    Ty    log.LineMatchType  // |=, !=, |~, !~, |>, !>
    Match string             // 매칭 문자열
    Op    string             // "ip" 등 특수 연산자
}
```

라인 필터는 연결 리스트 구조로 체이닝된다. `OR` 필터도 지원한다:

```
{app="loki"} |= "test" |= "foo" or "bar"

해석: "test" AND ("foo" OR "bar")
```

### 4.5 RangeAggregationExpr (범위 집계)

```go
type RangeAggregationExpr struct {
    Left      *LogRangeExpr  // 범위가 있는 로그 셀렉터
    Operation string          // rate, count_over_time 등
    Params    *float64         // quantile_over_time의 파라미터
    Grouping  *Grouping        // by/without 그룹핑
}
```

### 4.6 VectorAggregationExpr (벡터 집계)

```go
type VectorAggregationExpr struct {
    Left      SampleExpr  // 내부 샘플 표현식
    Grouping  *Grouping   // by/without 그룹핑
    Params    int          // topk/bottomk의 k 값
    Operation string      // sum, avg, topk 등
}
```

### 4.7 AST 구조 예시

쿼리 `sum(rate({app="nginx"} |= "error" | json [5m])) by (status)`:

```
VectorAggregationExpr (sum, by=[status])
  └── RangeAggregationExpr (rate)
        └── LogRangeExpr (interval=5m)
              └── PipelineExpr
                    ├── Left: MatchersExpr ({app="nginx"})
                    └── MultiStages:
                          ├── LineFilterExpr (|= "error")
                          └── LineParserExpr (json)
```

---

## 5. 파이프라인 스테이지

### 5.1 Pipeline 인터페이스

파일: `pkg/logql/log/pipeline.go`

```go
type Pipeline interface {
    ForStream(labels labels.Labels) StreamPipeline
    Reset()
}

type StreamPipeline interface {
    BaseLabels() LabelsResult
    Process(ts int64, line []byte, structuredMetadata labels.Labels) (
        resultLine []byte, resultLabels LabelsResult, matches bool)
    ProcessString(ts int64, line string, structuredMetadata labels.Labels) (
        resultLine string, resultLabels LabelsResult, matches bool)
    ReferencedStructuredMetadata() bool
}

type Stage interface {
    Process(ts int64, line []byte, lbs *LabelsBuilder) ([]byte, bool)
    RequiredLabelNames() []string
}
```

파이프라인은 여러 Stage를 순차적으로 적용한다:

```go
// pkg/logql/log/pipeline.go
func (p *streamPipeline) Process(ts int64, line []byte,
    structuredMetadata labels.Labels) ([]byte, LabelsResult, bool) {
    var ok bool
    p.builder.Reset()
    p.builder.Add(StructuredMetadataLabel, structuredMetadata)

    for _, s := range p.stages {
        line, ok = s.Process(ts, line, p.builder)
        if !ok {
            return nil, nil, false  // 필터에 의해 제거됨
        }
    }
    return line, p.builder.LabelsResult(), true
}
```

### 5.2 LineFilter (라인 필터 스테이지)

파일: `pkg/logql/log/filter.go`

```go
type LineMatchType int

const (
    LineMatchEqual      LineMatchType = iota  // |=
    LineMatchNotEqual                          // !=
    LineMatchRegexp                            // |~
    LineMatchNotRegexp                         // !~
    LineMatchPattern                           // |>
    LineMatchNotPattern                        // !>
)

type Filterer interface {
    Filter(line []byte) bool
    ToStage() Stage
}
```

필터는 성능에 따라 여러 구현체를 선택한다:
- 단순 문자열: `bytes.Contains` 기반
- 정규식: `regexp.Regexp` 기반
- 패턴: pattern 매칭 엔진 기반

### 5.3 Parser 스테이지

파일: `pkg/logql/log/parser.go`

**JSON 파서:**

```go
type JSONParser struct {
    prefixBuffer    [][]byte
    lbs             *LabelsBuilder
    captureJSONPath bool
    keys            internedStringSet
    parserHints     ParserHint
}

func (j *JSONParser) Process(_ int64, line []byte, lbs *LabelsBuilder) ([]byte, bool) {
    parserHints := lbs.ParserLabelHints()
    if parserHints.NoLabels() {
        return line, true  // 라벨 추출 필요 없음 → 바로 통과
    }
    // JSON 파싱하여 라벨 추출
    if err := jsonparser.ObjectEach(line, j.parseObject); err != nil {
        // 에러 시 __error__ 라벨 추가
        addErrLabel(errJSON, err, lbs)
    }
    return line, true
}
```

**지원 파서 목록:**

| 파서 | AST 노드 | 생성 함수 | 설명 |
|------|---------|---------|------|
| `json` | `LineParserExpr` | `NewJSONParser()` | JSON 키-값 추출 |
| `logfmt` | `LogfmtParserExpr` | `NewLogfmtParser()` | logfmt 형식 파싱 |
| `regexp` | `LineParserExpr` | `NewRegexpParser()` | 정규식 캡처 그룹 |
| `pattern` | `LineParserExpr` | `NewPatternParser()` | 패턴 매칭 |
| `unpack` | `LineParserExpr` | `NewUnpackParser()` | Loki 내부 형식 |

### 5.4 LabelFilter (라벨 필터 스테이지)

파일: `pkg/logql/log/label_filter.go`

```go
type LabelFilterer interface {
    Filter(lbs Labels) (bool, error)
    String() string
    Stage
}
```

라벨 필터는 파서가 추출한 라벨 값에 대한 조건부 필터링을 수행한다:

```
{app="nginx"} | json | status >= 400
                        ^^^^^^^^^^^^^^
                        LabelFilterExpr
```

### 5.5 LineFmt / LabelFmt (포맷 스테이지)

파일: `pkg/logql/log/fmt.go`

```go
type LineFormatter struct {
    *template.Template
    buf *bytes.Buffer
}

type LabelsFormatter struct {
    formats []LabelFmt
}
```

`line_format`은 Go의 `text/template` 엔진을 사용한다. 사용 가능한 템플릿 함수들:

```go
var functionMap = template.FuncMap{
    "ToLower":    strings.ToLower,
    "ToUpper":    strings.ToUpper,
    "Replace":    strings.Replace,
    "Trim":       strings.Trim,
    "regexReplaceAll": func(regex, s, repl string) (string, error) { ... },
    "urldecode":   url.QueryUnescape,
    "urlencode":   url.QueryEscape,
    "bytes":       convertBytes,
    "duration":    convertDuration,
    // ... sprig 함수 포함
}
```

### 5.6 스테이지 재정렬 최적화

`MultiStageExpr.reorderStages()`는 라인 필터를 가능한 앞으로 이동시킨다:

```go
// pkg/logql/syntax/ast.go
func (m MultiStageExpr) reorderStages() []StageExpr {
    // LineFilter는 앞으로 이동
    // LabelFilter는 구분점 역할 (필터 묶음을 분리)
    // LineFmt, unpack 이후의 필터는 이동 불가
}
```

왜 이렇게 하는가? 라인 필터는 가장 빠른 연산이므로, 비용이 큰 파싱 스테이지 이전에 적용하면 불필요한 파싱을 줄일 수 있다.

```
원래: {app="nginx"} | json | status >= 400 |= "error"
최적화: {app="nginx"} |= "error" | json | status >= 400
                       ^^^^^^^^^
                       앞으로 이동됨
```

---

## 6. 쿼리 엔진

### 6.1 QueryEngine 구조체

파일: `pkg/logql/engine.go`

```go
type QueryEngine struct {
    logger           log.Logger
    evaluatorFactory EvaluatorFactory
    limits           Limits
    opts             EngineOpts
}

type EngineOpts struct {
    MaxLookBackPeriod         time.Duration  // instant 쿼리의 최대 룩백 (기본 30초)
    LogExecutingQuery         bool           // 쿼리 실행 로깅 여부
    MaxCountMinSketchHeapSize int            // topk 쿼리의 힙 크기
}
```

엔진 생성:

```go
func NewEngine(opts EngineOpts, q Querier, l Limits, logger log.Logger) *QueryEngine {
    opts.applyDefault()
    return &QueryEngine{
        logger:           logger,
        evaluatorFactory: NewDefaultEvaluator(q, opts.MaxLookBackPeriod,
                                              opts.MaxCountMinSketchHeapSize),
        limits:           l,
        opts:             opts,
    }
}
```

### 6.2 Query 인터페이스와 구현

```go
type Engine interface {
    Query(Params) Query
}

type Query interface {
    Exec(ctx context.Context) (logqlmodel.Result, error)
}

type query struct {
    logger       log.Logger
    params       Params
    limits       Limits
    evaluator    EvaluatorFactory
    record       bool
    logExecQuery bool
}
```

### 6.3 Exec() 실행 흐름

```go
func (q *query) Exec(ctx context.Context) (logqlmodel.Result, error) {
    // 1. 트레이싱 시작
    ctx, sp := tracer.Start(ctx, "query.Exec")
    defer sp.End()

    // 2. 쿼리 로깅
    if q.logExecQuery {
        queryHash := util.HashedQuery(q.params.QueryString())
        level.Info(logutil.WithContext(ctx, q.logger)).Log(
            "msg", "executing query",
            "query", q.params.QueryString(),
            "query_hash", queryHash,
            "type", rangeType,
        )
    }

    // 3. 메트릭 기록 준비
    rangeType := GetRangeType(q.params)
    timer := prometheus.NewTimer(QueryTime.WithLabelValues(string(rangeType)))
    defer timer.ObserveDuration()

    // 4. 통계 컨텍스트 생성
    statsCtx, ctx := stats.NewContext(ctx)
    metadataCtx, ctx := metadata.NewContext(ctx)

    // 5. 실제 평가 수행
    data, err := q.Eval(ctx)

    // 6. 결과 조립 및 반환
    return logqlmodel.Result{
        Data:       data,
        Statistics: statResult,
        Headers:    metadataCtx.Headers(),
        Warnings:   metadataCtx.Warnings(),
    }, err
}
```

### 6.4 Eval() 메서드의 표현식 분기

```go
func (q *query) Eval(ctx context.Context) (promql_parser.Value, error) {
    // 1. 테넌트별 타임아웃 설정
    tenants, _ := tenant.TenantIDs(ctx)
    queryTimeout := validation.SmallestPositiveNonZeroDurationPerTenant(
        tenants, timeoutCapture)
    ctx, cancel := context.WithTimeout(ctx, queryTimeout)
    defer cancel()

    // 2. 차단 쿼리 확인
    if q.checkBlocked(ctx, tenants) {
        return nil, logqlmodel.ErrBlocked
    }

    // 3. 표현식 타입에 따른 분기
    switch e := q.params.GetExpression().(type) {
    case syntax.VariantsExpr:
        return q.evalVariants(ctx, e)

    case syntax.SampleExpr:
        return q.evalSample(ctx, e)

    case syntax.LogSelectorExpr:
        itr, err := q.evaluator.NewIterator(ctx, e, q.params)
        // 이터레이터로부터 스트림 읽기
        streams, err := readStreams(itr, q.params.Limit(),
                                    q.params.Direction(), q.params.Interval())
        return streams, err
    }
}
```

### 6.5 쿼리 실행 전체 흐름 다이어그램

```
┌────────────────────────────────────────────────────────────────────┐
│                       query.Exec()                                  │
│                                                                     │
│  ┌──────────┐   ┌───────────┐   ┌───────────┐   ┌──────────────┐  │
│  │트레이싱   │ → │쿼리 로깅   │ → │차단 체크   │ → │ q.Eval()     │  │
│  │시작       │   │           │   │           │   │              │  │
│  └──────────┘   └───────────┘   └───────────┘   └──────┬───────┘  │
│                                                         │          │
│                    ┌────────────────┬────────────────────┤          │
│                    │                │                    │          │
│              LogSelector      SampleExpr          VariantsExpr     │
│                    │                │                    │          │
│                    ▼                ▼                    ▼          │
│            NewIterator()    evalSample()         evalVariants()    │
│                    │                │                    │          │
│                    ▼                ▼                    │          │
│            readStreams()    NewStepEvaluator()           │          │
│                    │                │                    │          │
│                    ▼                ▼                    ▼          │
│              Streams          JoinSampleVector    JoinMulti...     │
│              (로그 결과)       (메트릭 결과)       (다중변형)       │
│                                                                     │
│  ┌───────────────────────────────────────────────────────────────┐  │
│  │            logqlmodel.Result 반환                               │  │
│  │  - Data (Streams / Vector / Matrix)                           │  │
│  │  - Statistics                                                 │  │
│  │  - Headers, Warnings                                          │  │
│  └───────────────────────────────────────────────────────────────┘  │
└────────────────────────────────────────────────────────────────────┘
```

### 6.6 readStreams: 로그 결과 조립

```go
func readStreams(i iter.EntryIterator, size uint32,
    dir logproto.Direction, interval time.Duration) (logqlmodel.Streams, error) {

    streams := map[string]*logproto.Stream{}
    respSize := uint32(0)
    lastEntry := lastEntryMinTime

    for respSize < size && i.Next() {
        streamLabels, entry := i.Labels(), i.At()

        // interval 기반 샘플링 (step 간격으로 출력)
        if interval == 0 || lastEntry.Unix() < 0 ||
            forwardShouldOutput || backwardShouldOutput {

            stream, ok := streams[streamLabels]
            if !ok {
                stream = &logproto.Stream{Labels: streamLabels}
                streams[streamLabels] = stream
            }
            stream.Entries = append(stream.Entries, entry)
            lastEntry = i.At().Timestamp
            respSize++
        }
    }

    result := make(logqlmodel.Streams, 0, len(streams))
    for _, stream := range streams {
        result = append(result, *stream)
    }
    sort.Sort(result)
    return result, i.Err()
}
```

---

## 7. 평가기(Evaluator)

### 7.1 StepEvaluator 인터페이스

파일: `pkg/logql/step_evaluator.go`

```go
type StepEvaluator interface {
    Next() (ok bool, ts int64, r StepResult)
    Close() error
    Error() error
    Explain(Node)
}

type StepResult interface {
    SampleVector() promql.Vector
    QuantileSketchVec() ProbabilisticQuantileVector
    CountMinSketchVec() CountMinSketchVector
}
```

`StepEvaluator`는 시계열 데이터를 한 스텝씩 생성하는 이터레이터 패턴을 구현한다. Range 쿼리에서는 각 스텝마다 벡터를 생성하고, Instant 쿼리에서는 한 번만 호출된다.

### 7.2 EvaluatorFactory

파일: `pkg/logql/evaluator.go`

```go
type EvaluatorFactory interface {
    NewIterator(context.Context, syntax.LogSelectorExpr, Params) (iter.EntryIterator, error)
    NewStepEvaluator(ctx context.Context, factory EvaluatorFactory,
        expr syntax.SampleExpr, p Params) (StepEvaluator, error)
    NewVariantsStepEvaluator(ctx context.Context, expr syntax.VariantsExpr,
        p Params) (StepEvaluator, error)
}
```

### 7.3 Querier 인터페이스

```go
type Querier interface {
    SelectLogs(context.Context, SelectLogParams) (iter.EntryIterator, error)
    SelectSamples(context.Context, SelectSampleParams) (iter.SampleIterator, error)
}
```

Querier는 실제 스토리지에서 데이터를 가져오는 역할을 한다. 엔진은 Querier를 통해 데이터를 가져오고, 평가기에서 파이프라인과 집계를 적용한다.

### 7.4 SampleExpr 평가 흐름

```go
func (q *query) evalSample(ctx context.Context, expr syntax.SampleExpr) (
    promql_parser.Value, error) {

    // 1. 리터럴/벡터 표현식 조기 반환
    if lit, ok := expr.(*syntax.LiteralExpr); ok {
        return q.evalLiteral(ctx, lit)
    }

    // 2. 최대 쿼리 범위 검증
    maxQueryInterval := validation.SmallestPositiveNonZeroDurationPerTenant(...)
    if maxQueryInterval != 0 {
        err = q.checkIntervalLimit(expr, maxQueryInterval)
    }

    // 3. 표현식 최적화
    expr, err = optimizeSampleExpr(expr)

    // 4. StepEvaluator 생성
    stepEvaluator, err := q.evaluator.NewStepEvaluator(ctx, q.evaluator, expr, q.params)
    defer stepEvaluator.Close()

    // 5. 첫 번째 스텝 평가
    next, _, r := stepEvaluator.Next()

    // 6. 결과 타입에 따른 분기
    switch vec := r.(type) {
    case SampleVector:
        return q.JoinSampleVector(ctx, next, vec, stepEvaluator, maxSeries, mfl)
    case ProbabilisticQuantileVector:
        return JoinQuantileSketchVector(...)
    case CountMinSketchVector:
        return JoinCountMinSketchVector(...)
    }
}
```

### 7.5 JoinSampleVector: 시계열 조립

Range 쿼리에서 각 스텝의 벡터를 시계열 매트릭스로 조합한다:

```go
func (q *query) JoinSampleVector(ctx context.Context, next bool, r StepResult,
    stepEvaluator StepEvaluator, maxSeries int, mergeFirstLast bool) (
    promql_parser.Value, error) {

    seriesIndex := map[uint64]promql.Series{}

    // 시리즈 개수 제한 확인
    if len(vec) > maxSeries {
        return nil, logqlmodel.NewSeriesLimitError(maxSeries)
    }

    // Instant 쿼리는 벡터를 바로 반환
    if GetRangeType(q.params) == InstantType {
        sort.Slice(vec, func(i, j int) bool {
            return labels.Compare(vec[i].Metric, vec[j].Metric) < 0
        })
        return vec, nil
    }

    // Range 쿼리는 모든 스텝을 매트릭스로 조합
    for next {
        vec = r.SampleVector()
        vectorsToSeries(vec, seriesIndex)

        if len(seriesIndex) > maxSeries {
            return nil, logqlmodel.NewSeriesLimitError(maxSeries)
        }

        next, _, r = stepEvaluator.Next()
    }

    series := make([]promql.Series, 0, len(seriesIndex))
    for _, s := range seriesIndex {
        series = append(series, s)
    }
    result := promql.Matrix(series)
    sort.Sort(result)
    return result, stepEvaluator.Error()
}
```

### 7.6 Instant vs Range 쿼리 판별

```go
func GetRangeType(q Params) QueryRangeType {
    if q.Start().Equal(q.End()) && q.Step() == 0 {
        return InstantType  // start == end && step == 0
    }
    return RangeType
}
```

---

## 8. 쿼리 샤딩

### 8.1 ShardMapper

파일: `pkg/logql/shardmapper.go`

쿼리 샤딩은 하나의 쿼리를 여러 개의 하위 쿼리로 분할하여 병렬 실행하는 기법이다.

```go
type ShardMapper struct {
    shards                   ShardingStrategy
    metrics                  *MapperMetrics
    quantileOverTimeSharding bool
    lastOverTimeSharding     bool
    firstOverTimeSharding    bool
    approxTopkSupport        bool
}
```

샤딩 가능 여부는 AST 노드의 `Shardable()` 메서드로 판단된다:

```go
func (e *MatchersExpr) Shardable(_ bool) bool  { return true }
func (e *PipelineExpr) Shardable(topLevel bool) bool {
    for _, p := range e.MultiStages {
        if !p.Shardable(topLevel) {
            return false
        }
    }
    return true
}
func (e *LineFilterExpr) Shardable(_ bool) bool { return true }
```

### 8.2 샤딩 전략

```
원본 쿼리:
    sum(rate({app="nginx"} [5m]))

샤딩 후:
    sum(
        concat(
            sum(rate({app="nginx"} [5m], shard=0of16)),
            sum(rate({app="nginx"} [5m], shard=1of16)),
            ...
            sum(rate({app="nginx"} [5m], shard=15of16))
        )
    )
```

### 8.3 샤딩 가능한 연산

| 연산 | 샤딩 전략 | 설명 |
|------|----------|------|
| `sum` | 샤드별 부분합 후 합산 | 교환법칙 성립 |
| `count` | 샤드별 카운트 후 합산 | `sum(count_per_shard)` |
| `avg` | 샤드별 `sum`/`count` 후 `sum/sum` | `avg = sum(sums) / sum(counts)` |
| `min`/`max` | 샤드별 min/max 후 전체 min/max | 교환법칙 성립 |
| `topk` | 샤드별 topk 후 병합 | 확률적 방식(`approx_topk`) 지원 |
| `rate` | 샤드별 rate 후 합산 | `sum(rate_per_shard)` |
| `count_over_time` | 샤드별 카운트 후 합산 | 교환법칙 성립 |

### 8.4 다운스트림 실행

샤딩된 쿼리의 각 샤드는 `DownstreamExpr`로 래핑되어 독립적으로 실행된다:

```go
// pkg/logql/downstream.go
type DownstreamSampleExpr struct {
    shard *ShardWithChunkRefs
    SampleExpr
}
```

### 8.5 병렬화 아키텍처

```
┌─────────────────────┐
│   원본 쿼리           │
│ sum(rate(...[5m]))   │
└──────────┬──────────┘
           │ ShardMapper.Map()
           ▼
┌─────────────────────┐
│   샤딩된 AST          │
│ sum(                 │
│   concat(            │
│     shard_0,         │
│     shard_1,         │
│     ...              │
│     shard_N          │
│   )                  │
│ )                    │
└──────────┬──────────┘
           │ 병렬 실행
     ┌─────┼─────┐
     ▼     ▼     ▼
┌───────┐ ┌───────┐ ┌───────┐
│Shard 0│ │Shard 1│ │Shard N│
│Ingester│ │Ingester│ │Ingester│
│/Store  │ │/Store  │ │/Store  │
└───┬───┘ └───┬───┘ └───┬───┘
    │         │         │
    └────┬────┘────┬────┘
         │         │
         ▼         ▼
    ┌──────────────────┐
    │  결과 병합 (sum)   │
    └──────────────────┘
```

---

## 9. 최적화

### 9.1 쿼리 최적화 (optimizeSampleExpr)

```go
func optimizeSampleExpr(expr syntax.SampleExpr) (syntax.SampleExpr, error) {
    // 파이프라인 스테이지 재정렬
    // 불필요한 라벨 추출 제거
    // 라인 필터 앞으로 이동
}
```

### 9.2 캐싱

쿼리 결과 캐시는 프론트엔드(query-frontend)에서 관리된다:

```go
type LiteralParams struct {
    // ...
    cachingOptions resultscache.CachingOptions
}
```

캐싱 수준:
1. **쿼리 결과 캐시**: query-frontend에서 시간 범위 기반 분할 후 캐싱
2. **인덱스 캐시**: 인덱스 조회 결과 캐싱
3. **청크 캐시**: 청크 데이터 캐싱 (L1 + L2 티어)

### 9.3 분할 (Splitting)

query-frontend는 큰 쿼리를 작은 시간 범위로 분할한다:

```
원본: rate({app="nginx"}[24h])  over 7 days
분할: rate({app="nginx"}[24h])  day 1
      rate({app="nginx"}[24h])  day 2
      ...
      rate({app="nginx"}[24h])  day 7
```

### 9.4 ParserHint 최적화

파서 힌트는 불필요한 라벨 추출을 건너뛰는 최적화를 제공한다:

```go
type ParserHint interface {
    NoLabels() bool                    // 라벨 추출 완전 불필요
    AllRequiredExtracted() bool        // 필요한 라벨 모두 추출됨
    PreserveError() bool               // 에러 보존 필요
    ShouldExtract(key string) bool     // 특정 키 추출 필요 여부
    ShouldExtractPrefix(prefix string) bool
}
```

`| json | status >= 400` 같은 쿼리에서 `status` 라벨만 추출하면 되므로, JSON 파서는 다른 필드를 건너뛸 수 있다:

```go
// JSONParser 내부
func (j *JSONParser) Process(_ int64, line []byte, lbs *LabelsBuilder) ([]byte, bool) {
    parserHints := lbs.ParserLabelHints()
    if parserHints.NoLabels() {
        return line, true  // 아무 라벨도 필요 없으면 파싱 건너뛰기
    }
    // ...
    if j.parserHints.AllRequiredExtracted() {
        // 필요한 라벨 모두 추출 완료 → 파싱 중단
        return errFoundAllLabels
    }
}
```

### 9.5 쿼리 차단

테넌트별로 특정 쿼리 패턴을 차단할 수 있다:

```go
func (q *query) checkBlocked(ctx context.Context, tenants []string) bool {
    blocker := newQueryBlocker(ctx, q)
    for _, tenant := range tenants {
        if blocker.isBlocked(ctx, tenant) {
            QueriesBlocked.WithLabelValues(tenant).Inc()
            return true
        }
    }
    return false
}
```

---

## 10. 내부 동작 흐름 종합

### 10.1 로그 쿼리 전체 흐름

```
1. HTTP 요청 수신
       │
2. query-frontend: 분할, 캐시 확인
       │
3. querier: ParseExpr() → AST 생성
       │
4. QueryEngine.Query(params) → query 생성
       │
5. query.Exec()
       │
6. query.Eval()
       ├── LogSelectorExpr 분기
       │     │
       │     ▼
       │   evaluator.NewIterator()
       │     │
       │     ▼
       │   Querier.SelectLogs()
       │     │
       │     ├── Ingester.Query() [인메모리 데이터]
       │     │
       │     └── Store.SelectLogs() [영속 데이터]
       │           │
       │           ├── 인덱스 조회 (매처 → 청크 참조)
       │           │
       │           └── 청크 읽기 (파이프라인 적용)
       │
       ├── 파이프라인 적용
       │     │
       │     ├── LineFilter: 라인 필터링
       │     ├── Parser: 라벨 추출
       │     ├── LabelFilter: 라벨 필터링
       │     └── Format: 라인/라벨 포맷팅
       │
       └── readStreams(): 결과 스트림 조립
```

### 10.2 메트릭 쿼리 전체 흐름

```
1. HTTP 요청 수신
       │
2. query-frontend: 분할, 샤딩, 캐시
       │
3. querier: ParseExpr() → SampleExpr AST
       │
4. ShardMapper: 쿼리 샤딩 (선택적)
       │
5. evalSample()
       │
6. optimizeSampleExpr(): 최적화
       │
7. NewStepEvaluator(): 단계별 평가기 생성
       │
8. 반복: Next() 호출
       │
       ├── 각 스텝에서:
       │     ├── Querier.SelectSamples()
       │     ├── 샘플 추출 및 집계
       │     └── Vector 반환
       │
9. JoinSampleVector(): 매트릭스 조립
       │
10. Result 반환
```

### 10.3 메트릭 수집

엔진은 다음 메트릭을 수집한다:

```go
var (
    QueryTime = promauto.NewHistogramVec(prometheus.HistogramOpts{
        Namespace: "logql",
        Name:      "query_duration_seconds",
        Help:      "LogQL query timings",
        Buckets:   prometheus.DefBuckets,
    }, []string{"query_type"})

    QueriesBlocked = promauto.NewCounterVec(prometheus.CounterOpts{
        Namespace: constants.Loki,
        Name:      "blocked_queries",
        Help:      "Count of queries blocked by per-tenant policy",
    }, []string{"user"})
)
```

### 10.4 핵심 설계 원칙 요약

| 원칙 | 구현 | 이유 |
|------|------|------|
| 파서 풀링 | `sync.Pool` 사용 | 파서 생성 비용 절감 |
| 스테이지 재정렬 | 라인 필터 앞으로 이동 | 빠른 필터링으로 불필요한 파싱 회피 |
| 파서 힌트 | 필요한 라벨만 추출 | JSON/logfmt 파싱 비용 최소화 |
| 쿼리 샤딩 | AST 변환으로 병렬화 | 대규모 데이터셋에서의 처리량 증가 |
| 스트림 캐싱 | 파이프라인 ForStream 캐시 | 동일 스트림 반복 처리 회피 |
| 차단 정책 | 테넌트별 쿼리 차단 | 위험한 쿼리로부터 시스템 보호 |
| 시리즈 제한 | maxSeries 체크 | 메모리 폭발 방지 |

---

## 참고 파일 경로

| 파일 | 설명 |
|------|------|
| `pkg/logql/engine.go` | QueryEngine, query, Exec/Eval 구현 |
| `pkg/logql/evaluator.go` | DefaultEvaluator, Params, LiteralParams |
| `pkg/logql/step_evaluator.go` | StepEvaluator, StepResult 인터페이스 |
| `pkg/logql/syntax/ast.go` | 모든 AST 노드 타입 정의 |
| `pkg/logql/syntax/lex.go` | 렉서 구현, 토큰 정의 |
| `pkg/logql/syntax/parser.go` | 파서, ParseExpr, 검증 로직 |
| `pkg/logql/syntax/syntax.y` | yacc 문법 정의 파일 |
| `pkg/logql/log/pipeline.go` | Pipeline, Stage 인터페이스 및 구현 |
| `pkg/logql/log/filter.go` | 라인 필터, Filterer 인터페이스 |
| `pkg/logql/log/parser.go` | JSONParser, RegexpParser, LogfmtParser |
| `pkg/logql/log/fmt.go` | LineFormatter, LabelsFormatter |
| `pkg/logql/log/label_filter.go` | LabelFilterer 구현 |
| `pkg/logql/shardmapper.go` | ShardMapper, 쿼리 샤딩 |
| `pkg/logql/downstream.go` | DownstreamExpr, 샤딩된 쿼리 실행 |
| `pkg/logql/optimize.go` | 쿼리 최적화 |
| `pkg/logql/rangemapper.go` | 범위 분할 매퍼 |
