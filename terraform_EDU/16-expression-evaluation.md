# 16. 표현식 평가 & 내장 함수 심화

## 목차

1. [개요](#1-개요)
2. [Scope 구조체](#2-scope-구조체)
3. [표현식 평가 흐름](#3-표현식-평가-흐름)
4. [EvalContext 구성](#4-evalcontext-구성)
5. [cty 값 시스템](#5-cty-값-시스템)
6. [참조 해석 상세](#6-참조-해석-상세)
7. [내장 함수 등록 시스템](#7-내장-함수-등록-시스템)
8. [함수 카테고리별 분류](#8-함수-카테고리별-분류)
9. [조건식, for 표현식, splat 연산자](#9-조건식-for-표현식-splat-연산자)
10. [Unknown 값의 중요성](#10-unknown-값의-중요성)
11. [Sensitive 값 처리](#11-sensitive-값-처리)
12. [설계 결정: 왜 이렇게 설계되었는가](#12-설계-결정-왜-이렇게-설계되었는가)
13. [정리](#13-정리)

---

## 1. 개요

Terraform의 표현식 평가 시스템은 HCL(HashiCorp Configuration Language) 표현식을 `cty.Value`로 변환하는 과정이다. 이 시스템은 변수 참조, 함수 호출, 연산자, 조건식 등 Terraform 설정 언어의 모든 동적 요소를 처리한다.

### 평가 파이프라인

```
HCL 소스코드
  "var.name"
      ↓ HCL 파서
hcl.Expression
      ↓ 참조 추출
[]*addrs.Reference
      ↓ EvalContext 구성
*hcl.EvalContext
      ↓ 표현식 평가
cty.Value
      ↓ 타입 변환
최종 cty.Value
```

### 핵심 소스 파일

| 파일 경로 | 역할 |
|-----------|------|
| `internal/lang/eval.go` | 표현식 평가 메서드들 (EvalBlock, EvalExpr 등) |
| `internal/lang/scope.go` | Scope 구조체 — 평가 컨텍스트의 최상위 |
| `internal/lang/functions.go` | 내장 함수 등록 (baseFunctions, Scope.Functions) |
| `internal/lang/funcs/` | 개별 함수 구현 |
| `internal/lang/langrefs/` | 참조 추출 |
| `internal/lang/marks/` | cty 값 마킹 (Sensitive 등) |
| `internal/lang/blocktoattr/` | v0.11 호환성 핵 |
| `internal/lang/globalref/` | 전역 참조 분석 |

---

## 2. Scope 구조체

### 2.1 정의

`internal/lang/scope.go`에 정의된 핵심 구조체:

```go
type Scope struct {
    // Data는 참조를 값으로 해석하는 데 사용
    Data Data

    // ParseRef는 HCL Traversal에서 참조를 추출하는 함수
    ParseRef langrefs.ParseRef

    // SelfAddr — "self" 객체의 별칭 주소
    SelfAddr addrs.Referenceable

    // SourceAddr — 현재 스코프의 소스 항목 주소
    SourceAddr addrs.Referenceable

    // BaseDir — 파일 시스템 함수의 기본 디렉토리
    BaseDir string

    // PureOnly — Plan 시 비순수 함수를 Unknown으로 반환
    PureOnly bool

    // ExternalFuncs — 프로바이더가 제공하는 외부 함수
    ExternalFuncs ExternalFuncs

    // FunctionResults — 함수 결과 일관성 검사용
    FunctionResults *FunctionResults

    funcs     map[string]function.Function
    funcsLock sync.Mutex

    activeExperiments experiments.Set

    // ConsoleMode — terraform console 전용 함수 활성화
    ConsoleMode bool

    // PlanTimestamp — plantimestamp() 함수용
    PlanTimestamp time.Time

    // ForProvider — 프로바이더 설정 블록 내에서의 평가
    ForProvider bool
}
```

### 2.2 Data 인터페이스

`Data`는 Scope가 참조를 값으로 해석할 때 사용하는 인터페이스:

```
Data 인터페이스
├── GetCountAttr(addr, rng)       → count.index
├── GetForEachAttr(addr, rng)     → each.key, each.value
├── GetInputVariable(addr, rng)   → var.name
├── GetLocalValue(addr, rng)      → local.name
├── GetModule(addr, rng)          → module.name.output
├── GetPathAttr(addr, rng)        → path.module, path.root
├── GetTerraformAttr(addr, rng)   → terraform.workspace
├── GetResource(addr, rng)        → resource 참조
└── GetOutput(addr, rng)          → output 값 (테스팅에서만)
```

### 2.3 ExternalFuncs

```go
type ExternalFuncs struct {
    Provider map[string]map[string]function.Function
    // 프로바이더별 함수 네임스페이스
    // provider::aws::arn_parse 등
}
```

---

## 3. 표현식 평가 흐름

### 3.1 EvalBlock — 블록 전체 평가

`internal/lang/eval.go`의 핵심 메서드:

```go
func (s *Scope) EvalBlock(body hcl.Body, schema *configschema.Block) (cty.Value, tfdiags.Diagnostics) {
    // 1. DecoderSpec 생성
    spec := schema.DecoderSpec()

    // 2. 참조 추출
    refs, diags := langrefs.ReferencesInBlock(s.ParseRef, body, schema)

    // 3. EvalContext 구성
    ctx, ctxDiags := s.EvalContext(refs)

    // 4. v0.11 호환성 핵
    body = blocktoattr.FixUpBlockAttrs(body, schema)

    // 5. HCL 디코딩 (실제 평가)
    val, evalDiags := hcldec.Decode(body, spec, ctx)

    return val, diags
}
```

### 3.2 EvalExpr — 단일 표현식 평가

```go
func (s *Scope) EvalExpr(expr hcl.Expression, wantType cty.Type) (cty.Value, tfdiags.Diagnostics) {
    // 1. 참조 추출
    refs, diags := langrefs.ReferencesInExpr(s.ParseRef, expr)

    // 2. EvalContext 구성
    ctx, ctxDiags := s.EvalContext(refs)

    // 3. 표현식 평가
    val, evalDiags := expr.Value(ctx)

    // 4. 타입 변환 (원하는 타입으로)
    if wantType != cty.DynamicPseudoType {
        val, convErr = convert.Convert(val, wantType)
        if convErr != nil {
            val = cty.UnknownVal(wantType)
            // 에러 진단 추가
        }
    }

    return val, diags
}
```

### 3.3 EvalSelfBlock — self 참조가 있는 블록

```go
func (s *Scope) EvalSelfBlock(body hcl.Body, self cty.Value,
    schema *configschema.Block, keyData instances.RepetitionData) (cty.Value, tfdiags.Diagnostics) {

    vals := make(map[string]cty.Value)
    vals["self"] = self

    if !keyData.CountIndex.IsNull() {
        vals["count"] = cty.ObjectVal(map[string]cty.Value{
            "index": keyData.CountIndex,
        })
    }
    if !keyData.EachKey.IsNull() {
        vals["each"] = cty.ObjectVal(map[string]cty.Value{
            "key": keyData.EachKey,
        })
    }

    // 참조에서 path, terraform 등 정적 값도 추가
    for _, ref := range refs {
        switch subj := ref.Subject.(type) {
        case addrs.PathAttr:
            pathAttrs[subj.Name] = s.Data.GetPathAttr(subj, ...)
        case addrs.TerraformAttr:
            terraformAttrs[subj.Name] = s.Data.GetTerraformAttr(subj, ...)
        }
    }

    ctx := &hcl.EvalContext{
        Variables: vals,
        Functions: s.Functions(),
    }

    val, decDiags := hcldec.Decode(body, schema.DecoderSpec(), ctx)
    return val, diags
}
```

### 3.4 ExpandBlock — dynamic 블록 확장

```go
func (s *Scope) ExpandBlock(body hcl.Body, schema *configschema.Block) (hcl.Body, tfdiags.Diagnostics) {
    spec := schema.DecoderSpec()
    traversals := dynblock.ExpandVariablesHCLDec(body, spec)
    refs, diags := langrefs.References(s.ParseRef, traversals)
    ctx, ctxDiags := s.EvalContext(refs)
    return dynblock.Expand(body, ctx), diags
}
```

### 3.5 평가 순서 다이어그램

```
┌────────────────────────────────────────────────────────────┐
│                   EvalBlock 전체 흐름                        │
├────────────────────────────────────────────────────────────┤
│                                                            │
│  1. Schema → DecoderSpec                                   │
│     configschema.Block → hcldec.Spec                       │
│                                                            │
│  2. Body → References 추출                                  │
│     hcl.Body ──→ hcl.Traversal[] ──→ addrs.Reference[]    │
│                                                            │
│  3. References → EvalContext                                │
│     addrs.Reference[] ──→ hcl.EvalContext{                 │
│       Variables: {"var": {...}, "local": {...}, ...}        │
│       Functions: {"length": ..., "join": ...}              │
│     }                                                      │
│                                                            │
│  4. Body + Spec + Context → Value                          │
│     hcldec.Decode(body, spec, ctx) ──→ cty.Value           │
│                                                            │
└────────────────────────────────────────────────────────────┘
```

---

## 4. EvalContext 구성

### 4.1 EvalContext의 구조

HCL의 `EvalContext`는 표현식 평가에 필요한 모든 정보를 담는다:

```go
type hcl.EvalContext struct {
    Variables map[string]cty.Value
    Functions map[string]function.Function
    Parent    *EvalContext  // 중첩 스코프
}
```

### 4.2 Variables 맵 구성

```
EvalContext.Variables = {
    "var": cty.ObjectVal({
        "name":          cty.StringVal("myapp"),
        "instance_type": cty.StringVal("t3.micro"),
    }),

    "local": cty.ObjectVal({
        "common_tags": cty.MapVal({...}),
    }),

    "module": cty.ObjectVal({
        "vpc": cty.ObjectVal({
            "subnet_id": cty.StringVal("subnet-123"),
        }),
    }),

    "data": cty.ObjectVal({
        "aws_ami": cty.ObjectVal({
            "latest": cty.ObjectVal({
                "id": cty.StringVal("ami-123"),
            }),
        }),
    }),

    "self": cty.ObjectVal({
        "private_ip": cty.StringVal("10.0.0.5"),
    }),

    "count": cty.ObjectVal({
        "index": cty.NumberIntVal(0),
    }),

    "each": cty.ObjectVal({
        "key":   cty.StringVal("us-east-1"),
        "value": cty.ObjectVal({...}),
    }),

    "path": cty.ObjectVal({
        "module": cty.StringVal("/path/to/module"),
        "root":   cty.StringVal("/path/to/root"),
        "cwd":    cty.StringVal("/current/dir"),
    }),

    "terraform": cty.ObjectVal({
        "workspace": cty.StringVal("default"),
    }),
}
```

### 4.3 참조 기반 지연 로딩

모든 변수를 미리 로드하지 않고, 표현식에서 실제로 참조하는 것만 로드한다:

```go
// EvalContext 내부:
for _, ref := range refs {
    switch addr := ref.Subject.(type) {
    case addrs.InputVariable:
        val := s.Data.GetInputVariable(addr, ref.SourceRange)
        vars["var"][addr.Name] = val

    case addrs.LocalValue:
        val := s.Data.GetLocalValue(addr, ref.SourceRange)
        vars["local"][addr.Name] = val

    case addrs.ResourceInstance:
        val := s.Data.GetResource(addr, ref.SourceRange)
        // 중첩된 맵 구조로 저장

    // ... 기타 참조 타입
    }
}
```

---

## 5. cty 값 시스템

### 5.1 cty란

`cty`(pronounced "see-tee")는 Terraform이 사용하는 **동적 타입 값 시스템**이다. Go의 정적 타입으로는 Terraform 설정 언어의 동적 특성을 표현할 수 없기 때문에 만들어졌다.

### 5.2 기본 타입

| cty 타입 | Go 대응 | 예시 |
|---------|---------|------|
| `cty.String` | `string` | `cty.StringVal("hello")` |
| `cty.Number` | `*big.Float` | `cty.NumberIntVal(42)` |
| `cty.Bool` | `bool` | `cty.True`, `cty.False` |
| `cty.List(T)` | `[]T` | `cty.ListVal([]cty.Value{...})` |
| `cty.Map(T)` | `map[string]T` | `cty.MapVal(map[string]cty.Value{...})` |
| `cty.Set(T)` | 집합 | `cty.SetVal([]cty.Value{...})` |
| `cty.Tuple(...)` | 다형 리스트 | `cty.TupleVal([]cty.Value{...})` |
| `cty.Object({...})` | 다형 맵 | `cty.ObjectVal(map[string]cty.Value{...})` |

### 5.3 특수 값: Unknown

```go
// Unknown 값 — "아직 모르는 값"
unknown := cty.UnknownVal(cty.String)
```

**Plan 시점**에서 아직 알 수 없는 값:

```hcl
resource "aws_instance" "web" {
  ami           = "ami-123"        # Known
  instance_type = "t3.micro"       # Known
}

output "public_ip" {
  value = aws_instance.web.public_ip  # Unknown at plan time
}
```

`public_ip`는 인스턴스가 실제로 생성되기 전까지 알 수 없으므로, Plan 시점에서 `cty.UnknownVal(cty.String)`이다.

### 5.4 특수 값: Null

```go
// Null 값 — "값이 없음"
null := cty.NullVal(cty.String)
```

`null`은 `optional` 속성이 설정되지 않았을 때 사용된다.

### 5.5 특수 값: DynamicVal

```go
// DynamicVal — 타입도 값도 모르는 완전 미지의 값
dynVal := cty.DynamicVal
```

타입 자체가 아직 결정되지 않은 경우. 예를 들어 `WildcardKey`의 값이 이것이다.

### 5.6 값 마크(Marks)

```go
// Sensitive 마킹
sensitiveVal := val.Mark(marks.Sensitive)

// Ephemeral 마킹
ephemeralVal := val.Mark(marks.Ephemeral)
```

마크는 값에 메타데이터를 부착하는 메커니즘이다:

| 마크 | 용도 |
|------|------|
| `marks.Sensitive` | plan 출력에서 마스킹, state에 암호화 저장 |
| `marks.Ephemeral` | 휘발성 값, state에 저장하지 않음 |

### 5.7 cty 타입 변환

```go
// 자동 변환
val, err := convert.Convert(cty.NumberIntVal(42), cty.String)
// → cty.StringVal("42")

// 변환 불가
val, err := convert.Convert(cty.StringVal("hello"), cty.Number)
// → err: cannot convert string "hello" to number
```

---

## 6. 참조 해석 상세

### 6.1 self 참조

```hcl
resource "aws_instance" "web" {
  provisioner "local-exec" {
    command = "echo ${self.private_ip}"
  }
}
```

`self`는 현재 리소스를 가리키는 특수 참조. `EvalSelfBlock`에서 처리:

```go
vals["self"] = self  // 현재 리소스의 값
```

### 6.2 each 참조

```hcl
resource "aws_instance" "web" {
  for_each = toset(["us-east-1", "us-west-2"])
  availability_zone = each.key
  tags = { Name = "web-${each.key}" }
}
```

```go
if !keyData.EachKey.IsNull() {
    vals["each"] = cty.ObjectVal(map[string]cty.Value{
        "key":   keyData.EachKey,
        "value": keyData.EachValue,
    })
}
```

### 6.3 count 참조

```hcl
resource "aws_instance" "web" {
  count = 3
  tags = { Name = "web-${count.index}" }
}
```

```go
if !keyData.CountIndex.IsNull() {
    vals["count"] = cty.ObjectVal(map[string]cty.Value{
        "index": keyData.CountIndex,
    })
}
```

### 6.4 var 참조

```hcl
variable "name" { default = "myapp" }
resource "aws_instance" "web" {
  tags = { Name = var.name }
}
```

```
parseRef("var.name")
→ Reference{Subject: InputVariable{Name: "name"}}
→ Data.GetInputVariable(InputVariable{Name: "name"})
→ cty.StringVal("myapp")
```

### 6.5 local 참조

```hcl
locals { suffix = "-prod" }
resource "aws_instance" "web" {
  tags = { Name = "app${local.suffix}" }
}
```

### 6.6 module 참조

```hcl
module "vpc" { source = "./modules/vpc" }
resource "aws_instance" "web" {
  subnet_id = module.vpc.subnet_id
}
```

```
parseRef("module.vpc.subnet_id")
→ Reference{
    Subject: ModuleCallOutput{
      Call: ModuleCall{Name: "vpc"},
      Name: "subnet_id",
    },
    Remaining: nil,
  }
```

### 6.7 data 참조

```hcl
data "aws_ami" "latest" {
  most_recent = true
  owners      = ["amazon"]
}
resource "aws_instance" "web" {
  ami = data.aws_ami.latest.id
}
```

```
parseRef("data.aws_ami.latest.id")
→ Reference{
    Subject: Resource{
      Mode: DataResourceMode,
      Type: "aws_ami",
      Name: "latest",
    },
    Remaining: [TraverseAttr{Name: "id"}],
  }
```

### 6.8 resource 참조 (암시적)

```hcl
resource "aws_instance" "web" { ... }
output "ip" {
  value = aws_instance.web.public_ip
}
```

```
parseRef("aws_instance.web.public_ip")
→ root "aws_instance" — 알려진 키워드 아님 → 암시적 리소스 참조
→ Reference{
    Subject: Resource{
      Mode: ManagedResourceMode,
      Type: "aws_instance",
      Name: "web",
    },
    Remaining: [TraverseAttr{Name: "public_ip"}],
  }
```

---

## 7. 내장 함수 등록 시스템

### 7.1 함수 등록 구조

`internal/lang/functions.go`에서:

```go
func (s *Scope) Functions() map[string]function.Function {
    s.funcsLock.Lock()
    if s.funcs == nil {
        // 1. 기본 함수 로드
        coreFuncs := baseFunctions(s.BaseDir)

        // 2. Scope별 함수 커스터마이징
        coreFuncs["file"] = funcs.MakeFileFunc(s.BaseDir, false, immutableResults(...))
        coreFuncs["templatefile"] = funcs.MakeTemplateFileFunc(...)
        coreFuncs["templatestring"] = funcs.MakeTemplateStringFunc(...)

        // 3. console 전용 함수
        if s.ConsoleMode {
            coreFuncs["type"] = funcs.TypeFunc
        }

        // 4. plan 전용 함수
        if !s.ConsoleMode {
            coreFuncs["plantimestamp"] = funcs.MakeStaticTimestampFunc(s.PlanTimestamp)
        }

        // 5. 비순수 함수 처리 (PureOnly 모드)
        if s.PureOnly {
            for _, name := range impureFunctions {
                coreFuncs[name] = function.Unpredictable(coreFuncs[name])
            }
        }

        // 6. core:: 네임스페이스 등록
        s.funcs = make(map[string]function.Function)
        for name, fn := range coreFuncs {
            fn = funcs.WithDescription(name, fn)
            s.funcs[name] = fn
            s.funcs["core::"+name] = fn  // 네임스페이스 별칭
        }

        // 7. 프로바이더 함수 등록
        for providerLocalName, funcs := range s.ExternalFuncs.Provider {
            for funcName, fn := range funcs {
                name := fmt.Sprintf("provider::%s::%s", providerLocalName, funcName)
                s.funcs[name] = fn
            }
        }
    }
    s.funcsLock.Unlock()
    return s.funcs
}
```

### 7.2 함수 네임스페이스

```
함수 네임스페이스:
├── (없음)        → 기본 함수         length(), join()
├── core::        → 내장 함수 명시적   core::length(), core::join()
├── provider::    → 프로바이더 함수    provider::aws::arn_parse()
└── module::      → 모듈 함수 (미래)   module::helper::format_name()
```

### 7.3 비순수 함수 (Impure Functions)

```go
var impureFunctions = []string{
    "bcrypt",
    "timestamp",
    "uuid",
}
```

이 함수들은 **호출할 때마다 다른 결과**를 반환한다. Plan 시점에서는 Unknown으로 처리하여 Plan과 Apply의 결과 차이를 방지한다.

```go
if s.PureOnly {
    for _, name := range impureFunctions {
        coreFuncs[name] = function.Unpredictable(coreFuncs[name])
    }
}
```

### 7.4 파일 시스템 함수의 일관성 검사

```go
var filesystemFunctions = collections.NewSetCmp[string](
    "file", "fileexists", "fileset", "filebase64",
    "filebase64sha256", "filebase64sha512",
    "filemd5", "filesha1", "filesha256", "filesha512",
    "templatefile",
)
```

파일 시스템 함수는 Plan과 Apply 사이에 파일이 변경될 수 있으므로, `FunctionResults`를 통해 결과 일관성을 검사한다. 단, `ForProvider` 모드에서는 레거시 호환을 위해 검사를 생략한다.

### 7.5 템플릿 함수의 재귀 방지

```go
funcsFunc := func() (funcs map[string]function.Function,
    fsFuncs collections.Set[string],
    templateFuncs collections.Set[string]) {
    return s.funcs, filesystemFunctions, templateFunctions
}
coreFuncs["templatefile"] = funcs.MakeTemplateFileFunc(s.BaseDir, funcsFunc, ...)
```

`templatefile`과 `templatestring`은 자신을 재귀적으로 호출할 수 없도록 보호된다.

---

## 8. 함수 카테고리별 분류

### 8.1 baseFunctions의 전체 등록

`internal/lang/functions.go`의 `baseFunctions()`:

### 문자열 함수

| 함수 | 설명 | 예시 |
|------|------|------|
| `chomp` | 끝 개행 제거 | `chomp("hello\n")` → `"hello"` |
| `format` | 포맷 문자열 | `format("Hello, %s!", "world")` |
| `formatlist` | 리스트 포맷 | `formatlist("%s-sg", var.names)` |
| `indent` | 들여쓰기 | `indent(2, "a\nb")` |
| `join` | 리스트 연결 | `join(", ", ["a", "b"])` → `"a, b"` |
| `lower` | 소문자 | `lower("HELLO")` → `"hello"` |
| `upper` | 대문자 | `upper("hello")` → `"HELLO"` |
| `replace` | 치환 | `replace("hello", "l", "L")` |
| `split` | 분리 | `split(",", "a,b")` → `["a", "b"]` |
| `substr` | 부분 문자열 | `substr("hello", 1, 3)` → `"ell"` |
| `title` | 타이틀 케이스 | `title("hello world")` |
| `trim` | 양쪽 트림 | `trim(" hello ", " ")` |
| `trimprefix` | 접두사 제거 | `trimprefix("helloworld", "hello")` |
| `trimsuffix` | 접미사 제거 | `trimsuffix("helloworld", "world")` |
| `trimspace` | 공백 트림 | `trimspace(" hello ")` |
| `regex` | 정규식 매칭 | `regex("[a-z]+", "hello123")` |
| `regexall` | 정규식 전체 매칭 | `regexall("[a-z]+", "hello 123 world")` |
| `startswith` | 접두사 확인 | `startswith("hello", "he")` → `true` |
| `endswith` | 접미사 확인 | `endswith("hello", "lo")` → `true` |
| `strcontains` | 포함 확인 | `strcontains("hello", "ell")` → `true` |
| `strrev` | 문자열 뒤집기 | `strrev("hello")` → `"olleh"` |

### 수학 함수

| 함수 | 설명 |
|------|------|
| `abs` | 절대값 |
| `ceil` | 올림 |
| `floor` | 내림 |
| `log` | 로그 |
| `max` | 최대값 |
| `min` | 최소값 |
| `parseint` | 문자열→정수 |
| `pow` | 거듭제곱 |
| `signum` | 부호 (-1, 0, 1) |

### 컬렉션 함수

| 함수 | 설명 |
|------|------|
| `length` | 길이 |
| `element` | 인덱스 접근 (순환) |
| `index` | 값으로 인덱스 찾기 |
| `flatten` | 중첩 리스트 평탄화 |
| `compact` | null/빈 문자열 제거 |
| `concat` | 리스트 연결 |
| `contains` | 포함 확인 |
| `distinct` | 중복 제거 |
| `chunklist` | 리스트 청크 분할 |
| `coalesce` | 첫 non-null 값 |
| `coalescelist` | 첫 non-empty 리스트 |
| `keys` | 맵의 키 리스트 |
| `values` | 맵의 값 리스트 |
| `lookup` | 맵 조회 (기본값) |
| `merge` | 맵 병합 |
| `reverse` | 리스트 역순 |
| `slice` | 리스트 슬라이스 |
| `sort` | 문자열 리스트 정렬 |
| `zipmap` | 키+값 → 맵 |
| `transpose` | 맵 전치 |
| `matchkeys` | 키 기반 필터 |
| `one` | 단일 요소 추출 |
| `sum` | 합계 |
| `range` | 범위 생성 |

### 집합 함수

| 함수 | 설명 |
|------|------|
| `setintersection` | 교집합 |
| `setproduct` | 카르테시안 곱 |
| `setsubtract` | 차집합 |
| `setunion` | 합집합 |
| `toset` | 집합 변환 |

### 인코딩/디코딩 함수

| 함수 | 설명 |
|------|------|
| `base64encode` | Base64 인코딩 |
| `base64decode` | Base64 디코딩 |
| `base64gzip` | Gzip + Base64 |
| `csvdecode` | CSV 파싱 |
| `jsondecode` | JSON 파싱 |
| `jsonencode` | JSON 직렬화 |
| `urlencode` | URL 인코딩 |
| `yamldecode` | YAML 파싱 |
| `yamlencode` | YAML 직렬화 |
| `textdecodebase64` | 텍스트 디코딩 |
| `textencodebase64` | 텍스트 인코딩 |

### 암호화 함수

| 함수 | 설명 |
|------|------|
| `bcrypt` | bcrypt 해시 (비순수) |
| `md5` | MD5 해시 |
| `sha1` | SHA1 해시 |
| `sha256` | SHA256 해시 |
| `sha512` | SHA512 해시 |
| `rsadecrypt` | RSA 복호화 |
| `uuid` | UUID 생성 (비순수) |
| `uuidv5` | UUID v5 생성 |

### 네트워크 함수

| 함수 | 설명 |
|------|------|
| `cidrhost` | CIDR에서 호스트 주소 |
| `cidrnetmask` | CIDR 넷마스크 |
| `cidrsubnet` | CIDR 서브넷 계산 |
| `cidrsubnets` | 다중 서브넷 계산 |

### 타입 변환 함수

| 함수 | 설명 |
|------|------|
| `tostring` | 문자열 변환 |
| `tonumber` | 숫자 변환 |
| `tobool` | 불리언 변환 |
| `tolist` | 리스트 변환 |
| `tomap` | 맵 변환 |
| `toset` | 집합 변환 |
| `convert` | 범용 타입 변환 |

### 날짜/시간 함수

| 함수 | 설명 |
|------|------|
| `formatdate` | 날짜 포맷 |
| `timeadd` | 시간 더하기 |
| `timecmp` | 시간 비교 |
| `timestamp` | 현재 시간 (비순수) |
| `plantimestamp` | Plan 시간 (고정) |

### 특수 함수

| 함수 | 설명 |
|------|------|
| `try` | 에러 없는 첫 표현식 |
| `can` | 표현식 평가 가능 여부 |
| `sensitive` | Sensitive 마킹 |
| `nonsensitive` | Sensitive 해제 |
| `issensitive` | Sensitive 여부 확인 |
| `ephemeralasnull` | Ephemeral → null 변환 |
| `type` | 타입 출력 (console 전용) |
| `alltrue` | 모든 요소 true |
| `anytrue` | 하나라도 true |

---

## 9. 조건식, for 표현식, splat 연산자

### 9.1 조건식

```hcl
# HCL 구문
value = condition ? true_val : false_val

# 예시
instance_type = var.env == "prod" ? "m5.xlarge" : "t3.micro"
```

HCL 파서가 `hcl.ConditionalExpr`로 파싱하고, 평가 시 조건을 먼저 평가한 후 해당하는 값만 평가한다.

Unknown 조건의 경우:

```
condition = unknown  →  결과도 unknown (양쪽 값의 공통 타입)
```

### 9.2 for 표현식

```hcl
# 리스트 → 리스트
upper_names = [for name in var.names : upper(name)]

# 리스트 → 맵
name_map = {for name in var.names : name => upper(name)}

# 필터링
short_names = [for name in var.names : name if length(name) < 5]

# 맵 순회
tag_list = [for k, v in var.tags : "${k}=${v}"]
```

### 9.3 for 표현식의 평가

```
[for name in var.names : upper(name) if length(name) > 3]

1. var.names 평가 → ["alice", "bob", "charlie"]
2. 각 요소에 대해:
   name = "alice"
   ├── 필터: length("alice") > 3 → true
   └── 값: upper("alice") → "ALICE"

   name = "bob"
   ├── 필터: length("bob") > 3 → false (스킵)

   name = "charlie"
   ├── 필터: length("charlie") > 3 → true
   └── 값: upper("charlie") → "CHARLIE"

3. 결과: ["ALICE", "CHARLIE"]
```

### 9.4 splat 연산자

```hcl
# 기본 splat
ids = aws_instance.web[*].id

# 동등한 for 표현식
ids = [for i in aws_instance.web : i.id]
```

splat 연산자 `[*]`는 컬렉션의 각 요소에서 속성을 추출하는 축약 문법이다.

```hcl
# 전체 접근 (full splat)
ports = aws_security_group.sg[*].ingress[*].from_port
# → 중첩 리스트의 중첩 리스트

# 레거시 (attribute-only splat)
ids = aws_instance.web.*.id
# → [*]와 동일하지만 deprecated
```

### 9.5 Unknown 값과 표현식

```
# Unknown이 포함된 for 표현식
[for s in unknown_list : s.name]
→ unknown list (원소 수를 모르므로 결과도 unknown)

# Unknown이 포함된 조건식
condition ? "a" : "b"
→ condition이 unknown이면 결과도 unknown
→ 단, 타입은 "a"와 "b"의 공통 타입 (string)

# Unknown이 포함된 함수 호출
length(unknown_list)
→ unknown number (리스트 크기를 모름)
```

---

## 10. Unknown 값의 중요성

### 10.1 Plan 시점의 Unknown

Terraform의 2단계 실행 모델에서 Unknown은 필수적이다:

```
Plan 단계                    Apply 단계
──────────                    ──────────
ami = "ami-123"              ami = "ami-123"
  → Known                      → Known

instance_id = (unknown)      instance_id = "i-abc123"
  → Unknown (아직 생성 안됨)     → Known (AWS가 반환)

public_ip = (unknown)        public_ip = "54.1.2.3"
  → Unknown                     → Known
```

### 10.2 Unknown 전파 규칙

```
# Unknown이 입력에 포함되면 출력도 Unknown
length(unknown_list)    → unknown number
join(",", unknown_list) → unknown string
upper(unknown_string)   → unknown string

# 예외: 일부 함수는 부분적으로 Known 결과를 반환
# 예: coalesce("hello", unknown) → "hello" (첫 Known 값 반환)
```

### 10.3 Unknown과 함수 평가

```go
// PureOnly 모드에서 비순수 함수:
if s.PureOnly {
    coreFuncs["timestamp"] = function.Unpredictable(coreFuncs["timestamp"])
    // timestamp()는 Plan 시점에서 Unknown 반환
    // Apply 시점에서 실제 시간 반환
}
```

### 10.4 Unknown이 중요한 이유

```
1. Plan 정확성
   Unknown을 추적하지 않으면 Plan이 실제 Apply와 다른 결과를 보여줄 수 있다.

2. 의존성 분석
   A의 출력이 B의 입력으로 들어갈 때, A가 Unknown이면
   B도 Unknown → 실행 순서 결정에 활용

3. 에러 방지
   Unknown 값에 대해 연산을 시도하면 에러 대신 Unknown을 반환
   → Plan이 실패하지 않고 "알 수 없음"으로 표시

4. 비순수 함수 제어
   timestamp(), uuid() 등은 Plan 시점에서 Unknown
   → Plan과 Apply의 결과 차이 방지
```

### 10.5 Unknown 처리 예시

```hcl
# Plan 출력:
# aws_instance.web will be created
# + resource "aws_instance" "web" {
#     + ami           = "ami-123"
#     + instance_type = "t3.micro"
#     + id            = (known after apply)    ← Unknown
#     + public_ip     = (known after apply)    ← Unknown
#   }
```

---

## 11. Sensitive 값 처리

### 11.1 Sensitive 마킹

```hcl
variable "db_password" {
  type      = string
  sensitive = true
}
```

```go
// 내부적으로:
val = val.Mark(marks.Sensitive)
```

### 11.2 Sensitive 전파

```
db_password = sensitive("secret123")
connection_string = "postgres://user:${db_password}@host/db"
  → 전체 문자열이 sensitive로 마킹
```

Sensitive 값이 표현식에 사용되면, **결과 전체**가 sensitive로 마킹된다.

### 11.3 Sensitive 함수

```go
"sensitive":    funcs.SensitiveFunc,     // 값 → sensitive 값
"nonsensitive": funcs.NonsensitiveFunc,  // sensitive → 일반 값
"issensitive":  funcs.IssensitiveFunc,   // sensitive 여부 확인
```

### 11.4 Plan에서의 표시

```
# Plan 출력에서 sensitive 값:
# ~ password = (sensitive value)
```

---

## 12. 설계 결정: 왜 이렇게 설계되었는가

### 12.1 왜 cty를 사용하는가

Go의 네이티브 타입 시스템으로는 부족한 이유:

| 필요 | Go 네이티브 | cty |
|------|-----------|-----|
| Unknown 값 | 불가 | `cty.UnknownVal()` |
| Null 값 | `nil` (타입 정보 없음) | `cty.NullVal(cty.String)` (타입 있음) |
| 동적 타입 | `interface{}` (안전하지 않음) | `cty.DynamicVal` (타입 안전) |
| 값 마킹 | 불가 | `val.Mark(marks.Sensitive)` |
| 집합 타입 | `map[...bool]` (해킹적) | `cty.Set(cty.String)` (일급 지원) |

### 12.2 왜 지연 참조 로딩인가

```go
// 모든 변수를 미리 로드하는 대신:
// ❌ ctx.Variables = loadAllVariables()

// 참조된 것만 로드:
// ✓ refs := extractReferences(expr)
//   ctx := buildContext(refs)
```

이유:
1. **성능**: 대규모 설정에서 모든 값을 미리 계산하면 불필요한 오버헤드
2. **순환 참조 방지**: 필요한 것만 로드하면 순환을 더 쉽게 감지
3. **에러 격리**: 참조되지 않는 값의 에러가 현재 평가에 영향을 주지 않음

### 12.3 왜 Scope에 Data 인터페이스가 있는가

```go
type Scope struct {
    Data Data  // 참조를 값으로 해석하는 인터페이스
}
```

Data를 인터페이스로 만든 이유:
1. **테스트**: 테스트에서 모의(mock) Data를 주입할 수 있다
2. **컨텍스트 분리**: Plan과 Apply에서 다른 Data 구현 사용
3. **확장성**: 새로운 참조 타입 추가 시 Data 인터페이스만 확장

### 12.4 왜 함수가 하드코딩인가

```go
// baseFunctions에서 모든 함수를 명시적으로 등록
fs := map[string]function.Function{
    "abs":    stdlib.AbsoluteFunc,
    "chomp":  stdlib.ChompFunc,
    // ... 80+ 함수
}
```

플러그인으로 만들지 않은 이유:
1. **안정성**: 내장 함수의 동작은 Terraform 버전에 의해 보장
2. **성능**: 프로세스 간 통신 오버헤드 없음
3. **보안**: 프로바이더 함수를 제외하면 신뢰할 수 있는 코드만 실행
4. **단순성**: 함수 추가/수정이 소스 레벨에서 즉시 가능

---

## 13. 정리

### 핵심 요약

| 개념 | 설명 |
|------|------|
| **Scope** | 평가 컨텍스트의 최상위 — Data, Functions, 옵션 관리 |
| **EvalBlock** | HCL 블록 전체를 cty.Value로 변환 |
| **EvalExpr** | 단일 HCL 표현식을 cty.Value로 변환 |
| **EvalContext** | 변수와 함수를 담는 HCL 평가 환경 |
| **cty** | 동적 타입 값 시스템 — Unknown, Null, Marks 지원 |
| **Unknown** | Plan 시점에서 아직 모르는 값 — 2단계 실행의 핵심 |
| **Sensitive** | 민감한 값 마킹 — 출력에서 마스킹 |
| **baseFunctions** | 80+개 내장 함수 — 문자열, 수학, 컬렉션, 암호화 등 |
| **core:: 네임스페이스** | 내장 함수의 명시적 네임스페이스 |
| **provider:: 네임스페이스** | 프로바이더 제공 함수 |

### 학습 포인트

1. **Unknown 전파**: 표현식 평가의 핵심 메커니즘, Plan과 Apply 일관성 보장
2. **지연 참조 로딩**: 성능과 에러 격리를 위한 설계
3. **cty의 존재 이유**: Go 타입 시스템의 한계를 보완하는 도메인 특화 타입 시스템
4. **비순수 함수 처리**: PureOnly 모드로 Plan 시점의 결정론성 보장
5. **Sensitive 전파**: 한 번 sensitive로 마킹되면 파생된 모든 값도 sensitive
