# 22. REPL (terraform console) & terraform fmt 심화

## 목차
1. [개요](#1-개요)
2. [REPL 아키텍처](#2-repl-아키텍처)
3. [Session 구조체 분석](#3-session-구조체-분석)
4. [표현식 평가 파이프라인](#4-표현식-평가-파이프라인)
5. [FormatValue - 값 포매팅 엔진](#5-formatvalue---값-포매팅-엔진)
6. [멀티라인 입력 감지](#6-멀티라인-입력-감지)
7. [terraform fmt 아키텍처](#7-terraform-fmt-아키텍처)
8. [FmtCommand 처리 흐름](#8-fmtcommand-처리-흐름)
9. [formatSourceCode - 포매팅 엔진](#9-formatsourcecode---포매팅-엔진)
10. [interpolation 언래핑](#10-interpolation-언래핑)
11. [타입 표현식 정규화](#11-타입-표현식-정규화)
12. [왜(Why) 이렇게 설계했나](#12-왜why-이렇게-설계했나)
13. [PoC 매핑](#13-poc-매핑)

---

## 1. 개요

### REPL (`terraform console`)

`terraform console`은 Terraform 표현식을 대화형으로 평가하는 도구다. 현재 State에 접근하여 리소스 속성 확인, 함수 테스트, 변수 참조 등을 즉시 실행할 수 있다.

### `terraform fmt`

HCL 설정 파일을 표준 형식으로 자동 포매팅하는 도구다. Go의 `gofmt`에서 영감을 받았으며, 코드 스타일 논쟁을 제거하고 일관된 코드베이스를 유지한다.

```
소스 경로:
├── internal/repl/              # REPL 시스템
│   ├── repl.go                 # 패키지 문서
│   ├── session.go              # Session 구조체, 평가 로직 (209줄)
│   ├── format.go               # FormatValue 함수 (180줄)
│   └── continuation.go         # 멀티라인 입력 감지 (97줄)
├── internal/command/fmt.go     # terraform fmt 구현 (620줄)
```

---

## 2. REPL 아키텍처

### 전체 흐름

```
┌────────────────────────────────────────────────────┐
│                terraform console                    │
│                                                     │
│  ┌──────────┐   ┌──────────┐   ┌────────────────┐ │
│  │ readline  │──>│ Session  │──>│ lang.Scope     │ │
│  │ (stdin)   │   │ Handle() │   │ EvalExpr()     │ │
│  └──────────┘   └──────────┘   └────────────────┘ │
│       │              │                │             │
│       │              ▼                ▼             │
│       │        ┌──────────┐   ┌────────────────┐  │
│       │        │ handleCmd│   │ cty.Value       │  │
│       │        │ (help,   │   │ → FormatValue() │  │
│       │        │  exit)   │   │ → 화면 출력     │  │
│       └────────┴──────────┘   └────────────────┘  │
└────────────────────────────────────────────────────┘
```

### 핵심 설계 원칙

| 원칙 | 구현 |
|------|------|
| 무상태(Stateless) 평가 | Session에 변수 저장 없음, State 읽기만 |
| 현재 State 반영 | lang.Scope를 통해 실제 State 접근 |
| 안전한 실험 | 인프라 변경 없이 표현식만 테스트 |
| 즉각 피드백 | 입력 즉시 평가 후 포매팅된 결과 출력 |

---

## 3. Session 구조체 분석

### `internal/repl/session.go`

```go
// Session은 단일 REPL 세션의 상태를 표현한다.
type Session struct {
    Scope *lang.Scope  // 표현식 평가 스코프
}
```

Session은 의도적으로 단순하다. `Scope`만 가지고 있으며, 이를 통해 현재 Terraform State에 있는 모든 리소스, 변수, 출력값에 접근할 수 있다.

### Handle 메서드

```go
func (s *Session) Handle(line string) (string, bool, tfdiags.Diagnostics) {
    switch {
    case strings.TrimSpace(line) == "":
        return "", false, nil          // 빈 줄: 무시
    case strings.TrimSpace(line) == "exit":
        return "", true, nil           // exit: 세션 종료
    case strings.TrimSpace(line) == "help":
        ret, diags := s.handleHelp()
        return ret, false, diags       // help: 도움말 출력
    default:
        ret, diags := s.handleEval(line)
        return ret, false, diags       // 그 외: 표현식 평가
    }
}
```

반환값 `(string, bool, Diagnostics)`:
- `string`: 출력 텍스트
- `bool`: 세션 종료 여부 (`exit` 시 true)
- `Diagnostics`: 경고/에러 메시지

---

## 4. 표현식 평가 파이프라인

### handleEval 분석

```go
func (s *Session) handleEval(line string) (string, tfdiags.Diagnostics) {
    // 1단계: HCL 표현식 파싱
    expr, parseDiags := hclsyntax.ParseExpression(
        []byte(line), "<console-input>", hcl.Pos{Line: 1, Column: 1})

    // 2단계: 현재 Scope에서 평가
    val, valDiags := s.Scope.EvalExpr(expr, cty.DynamicPseudoType)

    // 3단계: TypeType 마크 처리 (type() 함수 결과)
    if marks.Contains(val, marks.TypeType) {
        val, _ = val.UnmarkDeep()
        valType := val.EncapsulatedValue().(*cty.Type)
        return typeString(*valType), diags
    }

    // 4단계: 일반 값 포매팅
    return FormatValue(val, 0), diags
}
```

### 파이프라인 다이어그램

```
사용자 입력                      내부 처리                  출력
────────────  ──────────────────────────────────  ────────
"1 + 2"       Parse → EvalExpr → FormatValue     "3"
"aws_instance Parse → EvalExpr → FormatValue     "i-abc123"
 .foo.id"
"type(var.x)" Parse → EvalExpr → TypeType mark   "string"
               → typeString()
"tolist(null)" Parse → EvalExpr → FormatValue     "tolist(null)
                → null 분기                        /* of string */"
```

### DynamicPseudoType의 의미

`cty.DynamicPseudoType`을 기대 타입으로 전달하면 "어떤 타입이든 받겠다"는 의미다. REPL은 사용자가 어떤 타입의 표현식을 입력할지 알 수 없으므로, 타입 제약 없이 평가한다.

---

## 5. FormatValue - 값 포매팅 엔진

### `internal/repl/format.go`

값의 타입에 따라 사람이 읽기 좋은 형태로 변환한다:

```go
func FormatValue(v cty.Value, indent int) string {
    if !v.IsKnown() { return "(known after apply)" }
    if marks.Has(v, marks.Sensitive) { return "(sensitive value)" }
    if marks.Has(v, marks.Ephemeral) { return "(ephemeral value)" }

    if v.IsNull() {
        switch {
        case ty == cty.DynamicPseudoType: return "null"
        case ty == cty.String:  return "tostring(null)"
        case ty == cty.Number:  return "tonumber(null)"
        case ty == cty.Bool:    return "tobool(null)"
        case ty.IsListType():   return fmt.Sprintf("tolist(null) /* of %s */", ...)
        case ty.IsSetType():    return fmt.Sprintf("toset(null) /* of %s */", ...)
        case ty.IsMapType():    return fmt.Sprintf("tomap(null) /* of %s */", ...)
        default:                return fmt.Sprintf("null /* %s */", ...)
        }
    }

    switch {
    case ty.IsPrimitiveType():
        // String → strconv.Quote() 또는 heredoc
        // Number → BigFloat.Text('f', -1)
        // Bool → "true" / "false"
    case ty.IsObjectType():  return formatMappingValue(v, indent)
    case ty.IsTupleType():   return formatSequenceValue(v, indent)
    case ty.IsListType():    return "tolist(" + formatSequenceValue(v, indent) + ")"
    case ty.IsSetType():     return "toset(" + formatSequenceValue(v, indent) + ")"
    case ty.IsMapType():     return "tomap(" + formatMappingValue(v, indent) + ")"
    }
}
```

### 특수 값 표현

| 값 상태 | 출력 |
|---------|------|
| Unknown | `(known after apply)` |
| Sensitive | `(sensitive value)` |
| Ephemeral | `(ephemeral value)` |
| null (타입 있음) | `tostring(null)`, `tonumber(null)` 등 |
| null (타입 없음) | `null` |

### 멀티라인 문자열의 Heredoc 변환

```go
func formatMultilineString(v cty.Value, indent int) (string, bool) {
    str := v.AsString()
    lines := strings.Split(str, "\n")
    if len(lines) < 2 { return "", false }  // 단일 줄은 Heredoc 불필요

    operator := "<<"
    if indent > 0 { operator = "<<-" }     // 들여쓰기 시 <<- 사용

    delimiter := "EOT"
    // 충돌 방지: 내용에 "EOT"가 있으면 "EOT_", "EOT__" 등으로 확장
    for {
        conflict := false
        for _, line := range lines {
            if strings.TrimSpace(line) == delimiter {
                delimiter += "_"
                conflict = true
                break
            }
        }
        if !conflict { break }
    }
    // ...
}
```

**왜 Heredoc인가?** `\n`이 포함된 문자열을 `"hello\nworld"`로 출력하면 가독성이 떨어진다. Heredoc 형식은 실제 HCL 문법이므로 사용자가 복사하여 `.tf` 파일에 붙여넣을 수 있다.

---

## 6. 멀티라인 입력 감지

### `internal/repl/continuation.go`

```go
func ExpressionEntryCouldContinue(linesSoFar []string) bool {
    // 마지막 줄이 비어있으면 강제 실행
    if strings.TrimSpace(linesSoFar[len(linesSoFar)-1]) == "" {
        return false
    }

    // 토큰화하여 괄호 균형 검사
    delimStack := make([]hclsyntax.TokenType, 0, 8)
    all := strings.Join(linesSoFar, "\n") + "\n"
    toks, diags := hclsyntax.LexExpression([]byte(all), "", hcl.InitialPos)
    if diags.HasErrors() { return false }

    for _, tok := range toks {
        switch tok.Type {
        case hclsyntax.TokenOBrace, hclsyntax.TokenOBrack, hclsyntax.TokenOParen,
             hclsyntax.TokenOHeredoc, hclsyntax.TokenTemplateInterp, hclsyntax.TokenTemplateControl:
            push(tok.Type)         // 여는 괄호 → 스택에 추가
        case hclsyntax.TokenCBrace:
            if pop() != hclsyntax.TokenOBrace { return false }  // 불균형
        // ... 각 닫는 괄호 처리
        }
    }

    return len(delimStack) != 0  // 스택에 여는 괄호가 남아있으면 계속 입력
}
```

### 멀티라인 감지 예시

```hcl
# 첫 줄 입력: {           → 여는 중괄호 감지 → 계속 입력 대기
# 둘째 줄 입력: a = 1     → 스택에 { 남아있음 → 계속 대기
# 셋째 줄 입력: }         → 스택 비어짐 → 표현식 완성, 평가 실행
# 또는 셋째 줄 빈 줄      → 강제 실행 (에러 가능)
```

**왜 빈 줄을 강제 실행으로 처리하는가?** 휴리스틱이 잘못 판단하여 무한히 입력을 기다리는 상황을 방지한다. 사용자가 빈 줄을 입력하면 "지금까지 입력한 것을 평가해달라"는 의도로 해석한다.

---

## 7. terraform fmt 아키텍처

### 전체 구조

```
┌─────────────────────────────────────────────────────┐
│                  terraform fmt                       │
│                                                      │
│  ┌─────────────┐                                    │
│  │ FmtCommand  │                                    │
│  │  -list      │  플래그                             │
│  │  -write     │                                    │
│  │  -diff      │                                    │
│  │  -check     │                                    │
│  │  -recursive │                                    │
│  └──────┬──────┘                                    │
│         │                                            │
│         ▼                                            │
│  ┌──────────────┐   ┌──────────────────────────────┐│
│  │ fmt()        │──>│ processFile() / processDir() ││
│  │ 경로 순회    │   │ 파일별 처리                   ││
│  └──────────────┘   └──────────────┬───────────────┘│
│                                     │                │
│                     ┌───────────────▼───────────────┐│
│                     │ formatSourceCode()            ││
│                     │ ├── hclwrite.ParseConfig()    ││
│                     │ ├── formatBody()              ││
│                     │ │   ├── formatValueExpr()     ││
│                     │ │   └── formatTypeExpr()      ││
│                     │ └── f.Bytes()                 ││
│                     └───────────────────────────────┘│
└─────────────────────────────────────────────────────┘
```

---

## 8. FmtCommand 처리 흐름

### 지원 확장자

```go
var fmtSupportedExts = []string{
    ".tf",
    ".tfvars",
    ".tftest.hcl",
    ".tfmock.hcl",
}
```

JSON 파일(`.tf.json`, `.tfvars.json`)은 의도적으로 제외된다. JSON은 기계가 생성하는 형식이므로 포매팅이 불필요하다.

### Run 메서드 핵심 흐름

```go
func (c *FmtCommand) Run(args []string) int {
    // 플래그 파싱
    cmdFlags.BoolVar(&c.list, "list", true, "list")
    cmdFlags.BoolVar(&c.write, "write", true, "write")
    cmdFlags.BoolVar(&c.diff, "diff", false, "diff")
    cmdFlags.BoolVar(&c.check, "check", false, "check")
    cmdFlags.BoolVar(&c.recursive, "recursive", false, "recursive")

    // stdin인 경우
    if args[0] == "-" {
        c.list = false   // stdin에서는 파일 목록 불필요
        c.write = false  // stdin에서는 파일 쓰기 불가
    }

    // -check 모드: 포매팅 필요 여부만 확인
    if c.check {
        c.list = true    // 변경 필요 파일 목록 수집
        c.write = false  // 실제 변경은 하지 않음
        output = &bytes.Buffer{}  // 출력을 버퍼에 캡처
    }

    diags := c.fmt(paths, c.input, output)

    // -check 모드: 버퍼가 비어있으면 모두 포매팅 완료 (exit 0)
    if c.check {
        if buf.Len() == 0 { return 0 }
        else { return 3 }  // 포매팅 필요한 파일 존재
    }
}
```

### 종료 코드 체계

| 코드 | 의미 |
|------|------|
| 0 | 성공 (모든 파일 포매팅 완료 / check 통과) |
| 1 | CLI 인자 에러 |
| 2 | 포매팅 중 에러 (파일 읽기/쓰기 실패, 구문 에러) |
| 3 | `-check` 모드에서 포매팅 필요한 파일 존재 |

### processFile 분석

```go
func (c *FmtCommand) processFile(path string, r io.Reader, w io.Writer, isStdout bool) tfdiags.Diagnostics {
    src, _ := io.ReadAll(r)

    // 1단계: 구문 검증 (파싱 가능한지 확인)
    _, syntaxDiags := hclsyntax.ParseConfig(src, path, hcl.Pos{Line: 1, Column: 1})
    if syntaxDiags.HasErrors() {
        return diags  // 구문 에러가 있으면 포매팅 시도하지 않음
    }

    // 2단계: 포매팅 실행
    result := c.formatSourceCode(src, path)

    // 3단계: 변경 사항 처리
    if !bytes.Equal(src, result) {
        if c.list  { fmt.Fprintln(w, path) }           // 파일명 출력
        if c.write { os.WriteFile(path, result, 0644) } // 파일 덮어쓰기
        if c.diff  { diff, _ := bytesDiff(src, result, path); w.Write(diff) }
    }

    // 출력 전용 모드 (list, write, diff 모두 꺼진 경우)
    if !c.list && !c.write && !c.diff {
        w.Write(result)  // 포매팅 결과를 stdout에 출력
    }
}
```

**왜 구문 검증을 먼저 하는가?** 구문 에러가 있는 파일을 포매팅하면 원본보다 더 이상한 결과가 나올 수 있다. 소스 주석에 명시되어 있다:

> File must be parseable as HCL native syntax before we'll try to format it.
> If not, the formatter is likely to make drastic changes that would be hard
> for the user to undo.

---

## 9. formatSourceCode - 포매팅 엔진

### 핵심 로직

```go
func (c *FmtCommand) formatSourceCode(src []byte, filename string) []byte {
    f, diags := hclwrite.ParseConfig(src, filename, hcl.InitialPos)
    if diags.HasErrors() {
        return src  // 파싱 실패 시 원본 반환
    }
    c.formatBody(f.Body(), nil)
    return f.Bytes()
}
```

`hclwrite` 패키지는 AST를 수정하면서도 주석과 공백을 보존하는 특수 파서다. 일반 `hclsyntax` 파서와 달리, 토큰 레벨에서 조작이 가능하다.

### formatBody - 재귀적 포매팅

```go
func (c *FmtCommand) formatBody(body *hclwrite.Body, inBlocks []string) {
    // 속성 포매팅
    for name, attr := range body.Attributes() {
        if len(inBlocks) == 1 && inBlocks[0] == "variable" && name == "type" {
            // variable 블록의 type 속성은 특수 처리
            cleanedExprTokens := c.formatTypeExpr(attr.Expr().BuildTokens(nil))
            body.SetAttributeRaw(name, cleanedExprTokens)
        } else {
            // 일반 속성: interpolation 언래핑 처리
            cleanedExprTokens := c.formatValueExpr(attr.Expr().BuildTokens(nil))
            body.SetAttributeRaw(name, cleanedExprTokens)
        }
    }

    // 블록 포매팅
    for _, block := range body.Blocks() {
        block.SetLabels(block.Labels())  // 레이블 정규화
        c.formatBody(block.Body(), append(inBlocks, block.Type()))  // 재귀
    }
}
```

---

## 10. interpolation 언래핑

### 문제: 불필요한 보간 래핑

```hcl
# 불필요한 래핑 (Terraform 0.11 스타일)
name = "${var.name}"

# 올바른 형태 (Terraform 0.12+)
name = var.name
```

### formatValueExpr 알고리즘

```go
func (c *FmtCommand) formatValueExpr(tokens hclwrite.Tokens) hclwrite.Tokens {
    if len(tokens) < 5 { return tokens }

    // "${ ... }" 패턴 감지
    oQuote := tokens[0]          // "
    oBrace := tokens[1]          // ${
    cBrace := tokens[len(tokens)-2]  // }
    cQuote := tokens[len(tokens)-1]  // "

    // 단일 보간인지 확인 (중첩 보간 없어야 함)
    inside := tokens[2 : len(tokens)-2]
    for _, token := range inside {
        if token.Type == hclsyntax.TokenTemplateInterp ||
           token.Type == hclsyntax.TokenTemplateSeqEnd {
            return tokens  // "${foo}${bar}" 같은 복합 보간은 언래핑 불가
        }
        if token.Type == hclsyntax.TokenQuotedLit {
            return tokens  // "${foo}bar" 같은 리터럴 혼합도 언래핑 불가
        }
    }

    trimmed := c.trimNewlines(inside)

    // 멀티라인이면 괄호로 감싸기
    if isMultiLine && !(hasLeadingParen && hasTrailingParen) {
        wrapped := append([]Token{openParen}, trimmed...)
        wrapped = append(wrapped, closeParen)
        return wrapped
    }

    return trimmed  // 언래핑된 토큰 반환
}
```

### 언래핑 판단 흐름

```
입력: "${var.name}"
    │
    ├── 단일 보간? → Yes
    ├── 리터럴 혼합? → No
    ├── 멀티라인? → No
    │
    └── 결과: var.name

입력: "${var.x}-${var.y}"
    │
    ├── 내부에 }${ 발견
    │
    └── 결과: "${var.x}-${var.y}" (변경 없음)
```

---

## 11. 타입 표현식 정규화

### formatTypeExpr

```go
func (c *FmtCommand) formatTypeExpr(tokens hclwrite.Tokens) hclwrite.Tokens {
    switch len(tokens) {
    case 1:
        // 단일 키워드: list → list(any), map → map(any), set → set(any)
        switch string(kwTok.Bytes) {
        case "list", "map", "set":
            return Tokens{kwTok, openParen, anyIdent, closeParen}
        }

    case 3:
        // Terraform 0.11 레거시 형식: "string" → string
        switch string(strTok.Bytes) {
        case "string": return Tokens{stringIdent}
        case "list":   return Tokens{listIdent, openParen, stringIdent, closeParen}
        case "map":    return Tokens{mapIdent, openParen, stringIdent, closeParen}
        }
    }
}
```

### 정규화 예시

| 입력 (레거시) | 출력 (현대) | 이유 |
|-------------|-----------|------|
| `type = "string"` | `type = string` | 0.11 따옴표 형식 제거 |
| `type = "list"` | `type = list(string)` | 0.11 암시적 string 명시 |
| `type = list` | `type = list(any)` | 요소 타입 누락 시 any 명시 |
| `type = "map"` | `type = map(string)` | 0.11 암시적 string 명시 |

**왜 `string`이 기본 요소 타입인가?** Terraform 0.11에서는 모든 값이 내부적으로 문자열이었다. `list`는 문자열 리스트, `map`은 문자열-문자열 맵을 의미했다. 0.12 마이그레이션 시 이 호환성을 유지하기 위해 `string`을 기본값으로 사용한다. 반면 새로 작성하는 코드에서 `list`만 쓰면 `any`가 기본값이 되어야 의미적으로 맞다.

---

## 12. 왜(Why) 이렇게 설계했나

### Q1: REPL에서 왜 변수 할당을 지원하지 않는가?

Session 구조체에는 변수 저장 기능이 없다. 이는 의도적이다:
- REPL의 목적은 **실험과 검증**이지, 새로운 상태를 만드는 것이 아님
- 변수 할당을 허용하면 State와의 일관성 문제 발생
- 실수로 상태를 변경하는 위험 제거

### Q2: fmt가 왜 구문 에러 파일을 건드리지 않는가?

포매터는 AST를 재구성하여 토큰을 재배치한다. 구문 에러가 있는 파일은 AST가 불완전하므로:
- 포매터가 "최선의 추측"으로 재구성하면 원본보다 더 이상해질 수 있음
- 사용자가 `git diff`로 변경을 되돌리기 어려울 수 있음
- 구문 에러를 먼저 수정하도록 유도하는 것이 더 나은 워크플로우

### Q3: bytesDiff가 왜 외부 `diff` 명령을 호출하는가?

```go
func bytesDiff(b1, b2 []byte, path string) (data []byte, err error) {
    f1, _ := os.CreateTemp("", "")
    f2, _ := os.CreateTemp("", "")
    // ...
    data, err = exec.Command("diff", "--label=old/"+path,
        "--label=new/"+path, "-u", f1.Name(), f2.Name()).CombinedOutput()
}
```

Go 표준 라이브러리에 unified diff 구현이 없기 때문이다. 시스템의 `diff` 명령은 성능이 검증되었고, `--label` 플래그로 사용자 친화적 경로명을 지정할 수 있다.

### Q4: 왜 `-recursive`가 기본값이 아닌가?

소스 코드 주석에 이유가 명시되어 있다:

> We do not recurse into child directories by default because we want to
> mimic the file-reading behavior of "terraform plan", etc, operating on
> one module at a time.

Terraform은 모듈 단위로 동작한다. `terraform fmt`도 이 관례를 따라 현재 디렉토리(= 루트 모듈)만 처리한다. 하위 모듈은 별도의 `terraform fmt` 호출로 처리해야 한다.

### Q5: REPL에서 typeString이 왜 별도 구현인가?

```go
// Modified copy of TypeString from go-cty:
// https://github.com/zclconf/go-cty-debug/blob/master/ctydebug/type_string.go
```

`go-cty` 라이브러리의 디버그 패키지에서 복사한 것이다. 외부 디버그 라이브러리를 의존성으로 추가하지 않기 위해 내부에 복사했다. 이는 Terraform의 의존성 최소화 정책과 일치한다.

---

## 13. PoC 매핑

| PoC | 시뮬레이션 대상 |
|-----|---------------|
| poc-21-repl | REPL Session 평가 엔진, FormatValue, 멀티라인 감지 |
| poc-22-fmt | HCL 포매팅 엔진, interpolation 언래핑, 타입 정규화 |

---

## 참조 소스 파일

| 파일 | 줄수 | 핵심 내용 |
|------|------|----------|
| `internal/repl/session.go` | 209 | Session, Handle, handleEval, typeString |
| `internal/repl/format.go` | 180 | FormatValue, heredoc 변환, 컬렉션 포매팅 |
| `internal/repl/continuation.go` | 97 | 멀티라인 입력 감지, 괄호 스택 |
| `internal/command/fmt.go` | 620 | FmtCommand, processFile, formatBody, formatValueExpr |
