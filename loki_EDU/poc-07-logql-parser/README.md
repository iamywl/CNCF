# PoC #07: LogQL 파서 - LogQL 쿼리 파싱 및 AST 생성

## 개요

Loki의 LogQL은 로그 데이터를 쿼리하기 위한 전용 언어이다. 이 PoC는 LogQL의 핵심 파싱 파이프라인을 구현한다:

1. **Lexer (토크나이저)**: 입력 문자열을 토큰 스트림으로 변환
2. **Parser (재귀 하강 파서)**: 토큰 스트림을 AST(Abstract Syntax Tree)로 변환
3. **Evaluator (평가기)**: AST를 기반으로 로그 엔트리 필터링

## 실제 Loki 코드와의 관계

| 이 PoC | Loki 실제 코드 |
|--------|---------------|
| `Lexer` | `pkg/logql/syntax/lexer.go` |
| `Parser` | `pkg/logql/syntax/parser.go` |
| `AST 노드` | `pkg/logql/syntax/ast.go` |
| `Evaluator` | `pkg/logql/engine.go` + `pkg/logql/syntax/visit.go` |

## 지원 문법

```
logQuery       = streamSelector { pipelineStage }
streamSelector = "{" matcher { "," matcher } "}"
matcher        = IDENT ("=" | "!=" | "=~" | "!~") STRING
pipelineStage  = lineFilter | "|" (parserStage | labelFilter)
lineFilter     = ("|=" | "!=" | "|~" | "!~") STRING
parserStage    = "json" | "logfmt" | "regexp" STRING | "line_format" STRING
labelFilter    = IDENT ("=" | "!=" | ">" | ">=" | "<" | "<=") (STRING | NUMBER)
```

## 지원 LogQL 예시

```logql
{app="api"}
{app="api", env="prod"} |= "error"
{app="api"} |= "error" != "debug" | json | level="error"
{namespace=~"prod.*"} |~ "HTTP/1\\.[01]" | logfmt | status>="400"
{app="nginx"} | regexp "(?P<ip>\\d+)" | ip!="127.0.0.1"
```

## 실행 방법

```bash
go run main.go
```

## 핵심 개념

### Lexer (토크나이저)
- 입력 문자열을 한 문자씩 읽으며 토큰으로 분류
- 다중 문자 연산자 처리 (`|=`, `!=`, `=~`, `>=` 등)
- 따옴표 문자열, 식별자, 숫자를 구분

### Recursive Descent Parser
- 각 문법 규칙을 하나의 함수로 구현
- LL(1) 파싱: 현재 토큰과 다음 토큰(peek)만으로 파싱 결정
- 에러 수집 및 보고 기능

### AST (Abstract Syntax Tree)
- `LogQueryExpr`: 최상위 노드
  - `StreamSelector`: 스트림 셀렉터 (`{app="api"}`)
  - `Pipeline`: 파이프라인 스테이지 목록
    - `LineFilter`: 라인 필터 (`|= "error"`)
    - `ParserStage`: 파서 (`| json`, `| logfmt`)
    - `LabelFilter`: 레이블 필터 (`| level="error"`)

## 출력 예시

```
쿼리: {app="api", env="prod"} |= "error"

[1] 토큰화 결과:
    토큰[0]: {                   (위치: 0)
    토큰[1]: IDENT(app)          (위치: 1)
    토큰[2]: =                   (위치: 4)
    토큰[3]: STRING(api)         (위치: 5)
    ...

[2] AST:
    LogQueryExpr:
      StreamSelector:
        Matcher: app = "api"
        Matcher: env = "prod"
      Pipeline:
        LineFilter: |= "error"
```
