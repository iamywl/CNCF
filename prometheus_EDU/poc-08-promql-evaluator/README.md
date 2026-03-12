# PoC-08: PromQL Evaluator — 쿼리 파싱 및 평가 파이프라인

## 개요

Prometheus의 **PromQL 엔진**(쿼리 파서 + 평가기)의 핵심 구조를 Go 표준 라이브러리만으로 시뮬레이션한다.

PromQL(Prometheus Query Language)은 시계열 데이터를 선택, 필터링, 집계, 변환하기 위한 함수형 쿼리 언어이다. 이 PoC는 **쿼리 문자열이 결과 벡터로 변환되는 전체 과정** — Lexer(토큰화), Parser(AST 생성), Evaluator(평가) — 을 재현한다.

1. **Lexer** — PromQL 문자열을 토큰(식별자, 숫자, 연산자, 키워드 등)으로 분해
2. **AST** — VectorSelector, BinaryExpr, AggregateExpr, FunctionCall, NumberLiteral, MatrixSelector 노드
3. **Parser** — 재귀 하강 파서로 토큰 스트림에서 AST 구축
4. **Evaluator** — AST를 재귀 순회하며 Storage 조회 및 연산 적용

## 실제 소스코드 참조

| 구성 요소 | 실제 파일 | 설명 |
|-----------|----------|------|
| Lexer | `promql/parser/lex.go` | `stateFn` 기반 상태 머신, `Item{Typ, Pos, Val}` 토큰 |
| Token Types | `promql/parser/lex.go` | `ItemType` 정의, `key` 맵(키워드→타입 매핑) |
| AST Nodes | `promql/parser/ast.go` | `Node`/`Expr` 인터페이스, VectorSelector, BinaryExpr 등 |
| Parser | `promql/parser/parse.go` | yacc 생성 파서(`generated_parser.y.go`) |
| Engine | `promql/engine.go` | `evaluator.eval()` — switch로 노드 타입별 평가 |
| Functions | `promql/functions.go` | `funcRate` → `extrapolatedRate()` |
| Value Types | `promql/value.go` | `Vector`, `Matrix`, `Scalar`, `Series` |
| LabelMatcher | `model/labels/matcher.go` | `Equal`, `NotEqual`, `RegexMatch`, `NotRegexMatch` |

## 핵심 동작 원리

### PromQL 평가 파이프라인

```
┌─────────────┐    ┌──────────┐    ┌─────────┐    ┌───────────┐    ┌────────┐
│ Query String │───>│  Lexer   │───>│ Parser  │───>│ Evaluator │───>│ Result │
│              │    │(토큰화)  │    │(AST생성)│    │ (평가)    │    │(Vector)│
└─────────────┘    └──────────┘    └─────────┘    └───────────┘    └────────┘
```

### 1단계: Lexer (토큰화)

실제 Prometheus의 Lexer(`promql/parser/lex.go`)는 **stateFn 패턴**을 사용한다:

```go
// 실제 코드 구조
type stateFn func(*Lexer) stateFn

type Lexer struct {
    input string
    state stateFn
    pos   Pos
    start Pos
    ...
}
```

상태 전이 흐름:
```
lexStatements ──────┬──> lexInsideBraces ('{' 내부)
    │               ├──> lexNumberOrDuration (숫자/기간)
    │               ├──> lexKeywordOrIdentifier (키워드/식별자)
    │               └──> lexString (문자열 리터럴)
    │
    ├── 연산자: +, -, *, /, >, <, ==, !=
    ├── 구분자: (, ), {, }, [, ], ,
    └── 키워드: sum, avg, count, max, min, by, without, rate, ...
```

주요 토큰 타입(`ItemType`):
- `IDENTIFIER` / `METRIC_IDENTIFIER` — 메트릭 이름, 레이블 이름
- `NUMBER` / `DURATION` — 숫자 리터럴, 기간(5m, 1h)
- 연산자: `ADD(+)`, `SUB(-)`, `MUL(*)`, `DIV(/)`, `GTR(>)`, `LSS(<)`, `EQLC(==)`
- 집계: `SUM`, `AVG`, `COUNT`, `MAX`, `MIN`
- 키워드: `BY`, `WITHOUT`, `OFFSET`

### 2단계: AST 구조

실제 `promql/parser/ast.go`의 주요 노드:

```
Expr (인터페이스)
 ├── VectorSelector     메트릭 선택: Name + LabelMatchers
 │     실제 필드: Name, LabelMatchers []*labels.Matcher, Series, Offset, Timestamp
 │
 ├── NumberLiteral      숫자 상수: Val float64
 │
 ├── BinaryExpr         이항 연산: Op + LHS + RHS + VectorMatching
 │     VectorMatching: Card(1:1, N:1, 1:N), MatchingLabels, On/Ignoring
 │
 ├── AggregateExpr      집계: Op + Expr + Grouping []string + Without bool
 │     Op: SUM, AVG, COUNT, MAX, MIN, TOPK, BOTTOMK, QUANTILE
 │
 ├── Call (함수호출)    Func *Function + Args []Expr
 │     rate(), irate(), increase(), histogram_quantile() 등
 │
 └── MatrixSelector     범위 벡터: VectorSelector + Range time.Duration
       rate(metric[5m])에서 metric[5m] 부분
```

### 3단계: Evaluator (평가)

실제 `promql/engine.go`의 `evaluator.eval()` 메서드는 큰 switch 문이다:

```go
// 실제 코드 구조 (engine.go:1905)
func (ev *evaluator) eval(ctx context.Context, expr parser.Expr) (parser.Value, annotations.Annotations) {
    switch e := expr.(type) {
    case *parser.AggregateExpr:
        // 1. grouping 레이블 정렬
        // 2. eval(e.Expr)로 내부 표현식 평가
        // 3. rangeEvalAgg()로 그룹별 집계
    case *parser.Call:
        // FunctionCalls[e.Func.Name]() 호출
    case *parser.BinaryExpr:
        // eval(LHS), eval(RHS) 후 연산 적용
    case *parser.VectorSelector:
        // Storage에서 시계열 조회 + lookbackDelta 내 최신 샘플
    case *parser.NumberLiteral:
        // Scalar{V: e.Val} 반환
    ...
    }
}
```

#### VectorSelector 평가

```
VectorSelector 평가 흐름:
  1. checkAndExpandSeriesSet(ctx, sel)
     └── storage.Select()로 매칭 시계열 조회
  2. evalSeries(ctx, series, offset, ...)
     └── 각 시계열에서 timestamp - lookbackDelta ~ timestamp 범위의 최신 샘플 선택
```

**LookbackDelta** (기본 5분): 정확한 평가 시점에 데이터가 없으면, `lookbackDelta` 이내의 가장 최근 데이터를 사용한다. `engine.go`에서 `defaultLookbackDelta = 5 * time.Minute`로 정의.

#### BinaryExpr 평가

```
BinaryExpr 평가:
  ├── Scalar op Scalar → 단순 연산
  ├── Vector op Scalar → 각 샘플에 연산 적용
  ├── Scalar op Vector → 각 샘플에 연산 적용
  └── Vector op Vector → VectorMatching으로 레이블 매칭 후 연산
       └── one-to-one: __name__ 제외 레이블이 동일한 쌍 매칭
       └── 비교 연산자(>, <, ==): 조건 불만족 시 필터링
```

#### AggregateExpr 평가

```
AggregateExpr 평가:
  1. eval(Expr) → 내부 표현식을 평가하여 Vector 획득
  2. generateGroupingKey(metric, grouping, without)
     ├── by (label, ...):  지정 레이블만으로 그룹 키 생성
     └── without (label, ...): 지정 레이블 제외하고 그룹 키 생성
  3. 그룹별 집계:
     ├── sum:   Σ values
     ├── avg:   Σ values / count
     ├── count: len(values)
     ├── max:   max(values)
     └── min:   min(values)
```

#### rate() 함수 평가

```
rate() 평가 (promql/functions.go):
  1. MatrixSelector로 범위 내 모든 샘플 조회
  2. extrapolatedRate():
     ├── resultValue = lastValue - firstValue (counter 가정)
     ├── 외삽(extrapolation) 적용:
     │    sampledInterval = lastT - firstT
     │    averageDurationBetweenSamples = sampledInterval / (numSamples - 1)
     │    extrapolateToInterval = range + averageDurationBetweenSamples
     │    extrapolationCorrection = extrapolateToInterval / sampledInterval
     │    resultValue *= extrapolationCorrection
     └── resultValue / rangeDuration 반환
```

## AST 예시

### `sum by (method) (http_requests_total)` 의 AST:

```
AggregateExpr: sum by (method)
  └── VectorSelector: http_requests_total
        Matchers: [__name__="http_requests_total"]
```

### `rate(http_requests_total[5m])` 의 AST:

```
FunctionCall: rate
  └── MatrixSelector: range=5m
        └── VectorSelector: http_requests_total
              Matchers: [__name__="http_requests_total"]
```

### `http_requests_total > 100` 의 AST:

```
BinaryExpr: op=>
  ├── VectorSelector: http_requests_total
  │     Matchers: [__name__="http_requests_total"]
  └── NumberLiteral: 100
```

## 실행 방법

```bash
go run main.go
```

외부 의존성 없이 Go 표준 라이브러리만 사용한다.

## 데모 쿼리

| 쿼리 | 설명 | 평가 방식 |
|------|------|-----------|
| `http_requests_total{method="GET"}` | 레이블 매처로 시계열 필터링 | VectorSelector + LabelMatcher |
| `http_requests_total > 100` | 비교 연산으로 필터링 | BinaryExpr (Vector > Scalar) |
| `sum by (method) (http_requests_total)` | method별 합계 | AggregateExpr + grouping |
| `avg by (method) (http_requests_total)` | method별 평균 | AggregateExpr + grouping |
| `count by (method) (http_requests_total)` | method별 시계열 수 | AggregateExpr + grouping |
| `rate(http_requests_total[5m])` | 초당 증가율 계산 | FunctionCall + MatrixSelector |
| `http_requests_total * 2` | 벡터-스칼라 곱셈 | BinaryExpr (Vector * Scalar) |

## 실제 Prometheus와의 차이

| 항목 | 실제 Prometheus | 이 PoC |
|------|----------------|--------|
| Lexer | stateFn 상태 머신 | 루프 기반 간소화 렉서 |
| Parser | yacc 생성 파서 (LALR) | 재귀 하강 파서 |
| 연산자 우선순위 | precedence climbing | 단순 좌→우 |
| VectorMatching | on/ignoring/group_left/group_right | one-to-one만 지원 |
| rate() | extrapolatedRate (외삽 보정) | 단순 (last-first)/duration |
| 평가 모드 | instant + range query | instant query만 |
| 함수 | 80+ 내장 함수 | rate()만 구현 |
| LookbackDelta | MemoizedIterator로 효율적 탐색 | 단순 선형 탐색 |
