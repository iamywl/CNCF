# 15. 주소(Address) 체계 심화

## 목차

1. [개요](#1-개요)
2. [Module 주소](#2-module-주소)
3. [Resource 주소](#3-resource-주소)
4. [Provider 주소](#4-provider-주소)
5. [InstanceKey: NoKey, IntKey, StringKey](#5-instancekey-nokey-intkey-stringkey)
6. [절대 주소와 상대 주소](#6-절대-주소와-상대-주소)
7. [참조 파싱 (parse_ref.go)](#7-참조-파싱-parse_refgo)
8. [Target 주소 해석](#8-target-주소-해석)
9. [Move 엔드포인트 주소](#9-move-엔드포인트-주소)
10. [주소 문자열 ↔ 구조체 변환](#10-주소-문자열--구조체-변환)
11. [Traversal 기반 주소 해석](#11-traversal-기반-주소-해석)
12. [설계 결정: 왜 주소 체계가 복잡한가](#12-설계-결정-왜-주소-체계가-복잡한가)
13. [정리](#13-정리)

---

## 1. 개요

Terraform의 주소(Address) 시스템은 설정, State, 실행 계획에서 **모든 객체를 고유하게 식별**하는 체계이다. 리소스, 모듈, 프로바이더, 데이터 소스, 변수 등 Terraform이 관리하는 모든 것은 고유한 주소를 가진다.

### 왜 주소 시스템이 필요한가

| 용도 | 예시 |
|------|------|
| State에서 리소스 식별 | `module.vpc.aws_subnet.public[0]` |
| -target 으로 범위 한정 | `terraform apply -target=aws_instance.web` |
| moved 블록에서 이동 지점 | `moved { from = aws_instance.old to = aws_instance.new }` |
| 의존성 그래프 노드 | `module.db.aws_rds_instance.main` |
| 참조 해석 | `var.name`, `local.tags`, `module.vpc.subnet_id` |

### 핵심 소스 파일

| 파일 경로 | 역할 |
|-----------|------|
| `internal/addrs/module.go` | Module, ModuleInstance |
| `internal/addrs/resource.go` | Resource, ResourceInstance, AbsResource |
| `internal/addrs/provider.go` | Provider 주소 |
| `internal/addrs/instance_key.go` | InstanceKey (NoKey, IntKey, StringKey) |
| `internal/addrs/parse_ref.go` | 참조 파싱 (Reference) |
| `internal/addrs/move_endpoint.go` | moved 블록 주소 |
| `internal/addrs/parse_target.go` | -target 주소 해석 |
| `internal/addrs/module_instance.go` | ModuleInstance (동적) |

---

## 2. Module 주소

### 2.1 Module (정적 주소)

`internal/addrs/module.go`에 정의:

```go
// Module is an address for a module call within configuration. This is
// the static counterpart of ModuleInstance, representing a traversal through
// the static module call tree in configuration and does not take into account
// the potentially-multiple instances of a module that might be created by
// "count" and "for_each" arguments within those calls.
type Module []string
```

Module은 단순한 `[]string`이다. 이 단순한 타입이 모든 모듈 중첩을 표현한다.

### 2.2 모듈 경로 예시

| HCL 설정 | Module 주소 | Go 표현 |
|----------|------------|---------|
| 루트 모듈 | (빈 경로) | `Module{}` 또는 `RootModule` |
| `module "vpc" {}` | `module.vpc` | `Module{"vpc"}` |
| `module "vpc" { module "subnet" {} }` | `module.vpc.module.subnet` | `Module{"vpc", "subnet"}` |

### 2.3 RootModule

```go
// RootModule is the module address representing the root of the static module
// call tree, which is also the zero value of Module.
var RootModule Module
```

`nil` 슬라이스가 루트 모듈을 의미한다. Go의 제로 값이 유효한 상태인 관용적 설계.

### 2.4 String() 메서드

```go
func (m Module) String() string {
    if len(m) == 0 {
        return ""
    }
    buf := strings.Builder{}
    buf.Grow(l + len(m)*8)  // "module." = 8 chars
    sep := ""
    for _, step := range m {
        buf.WriteString(sep)
        buf.WriteString("module.")
        buf.WriteString(step)
        sep = "."
    }
    return buf.String()
}
```

결과: `Module{"vpc", "subnet"}` → `"module.vpc.module.subnet"`

### 2.5 핵심 메서드

```go
// Child — 자식 모듈 주소 생성
func (m Module) Child(name string) Module {
    ret := make(Module, 0, len(m)+1)
    ret = append(ret, m...)
    return append(ret, name)
}

// Parent — 부모 모듈 주소
func (m Module) Parent() Module {
    if len(m) == 0 { return m }
    return m[:len(m)-1]
}

// IsRoot — 루트 모듈 여부
func (m Module) IsRoot() bool {
    return len(m) == 0
}

// Call — 마지막 모듈 호출과 호출자 분리
func (m Module) Call() (Module, ModuleCall) {
    caller, callName := m[:len(m)-1], m[len(m)-1]
    return caller, ModuleCall{Name: callName}
}

// Ancestors — 루트부터 자신까지의 모든 조상
func (m Module) Ancestors() []Module {
    ret := make([]Module, 0, len(m)+1)
    for i := 0; i <= len(m); i++ {
        ret = append(ret, m[:i])
    }
    return ret
}
```

### 2.6 TargetContains: 타겟 포함 관계

```go
func (m Module) TargetContains(other Targetable) bool {
    switch to := other.(type) {
    case Module:
        // 접두사 매칭
        for i, ourStep := range m {
            if ourStep != to[i] { return false }
        }
        return true
    case ModuleInstance:
        return m.TargetContains(to.Module())
    case ConfigResource:
        return m.TargetContains(to.Module)
    case AbsResource:
        return m.TargetContains(to.Module)
    case AbsResourceInstance:
        return m.TargetContains(to.Module)
    }
    return false
}
```

`-target=module.vpc`를 지정하면 `module.vpc` 아래의 모든 리소스가 포함된다. 이것이 `TargetContains`의 역할이다.

### 2.7 Module vs ModuleInstance

```
Module (정적)                    ModuleInstance (동적)
"module.vpc"                     "module.vpc"
                                 "module.vpc[0]"
                                 "module.vpc[\"us-east-1\"]"

Module{"vpc"}                    ModuleInstance{
                                   {Name:"vpc", InstanceKey: NoKey},
                                 }
```

`Module`은 설정 파일의 정적 구조를, `ModuleInstance`는 실행 시 count/for_each로 생성된 인스턴스를 나타낸다.

---

## 3. Resource 주소

### 3.1 Resource 구조체 (상대 주소)

```go
// Resource is an address for a resource block within configuration.
type Resource struct {
    referenceable
    Mode ResourceMode
    Type string
    Name string
}
```

### 3.2 ResourceMode

```go
const (
    ManagedResourceMode   ResourceMode  // resource "aws_instance" "web"
    DataResourceMode      ResourceMode  // data "aws_ami" "latest"
    EphemeralResourceMode ResourceMode  // ephemeral 리소스 (실험적)
    ListResourceMode      ResourceMode  // list 리소스 (실험적)
)
```

### 3.3 String() 출력

```go
func (r Resource) String() string {
    switch r.Mode {
    case ManagedResourceMode:
        return fmt.Sprintf("%s.%s", r.Type, r.Name)
        // → "aws_instance.web"
    case DataResourceMode:
        return fmt.Sprintf("data.%s.%s", r.Type, r.Name)
        // → "data.aws_ami.latest"
    case EphemeralResourceMode:
        return fmt.Sprintf("ephemeral.%s.%s", r.Type, r.Name)
    case ListResourceMode:
        return fmt.Sprintf("list.%s.%s", r.Type, r.Name)
    }
}
```

### 3.4 ImpliedProvider

```go
// ImpliedProvider returns the implied provider type name
func (r Resource) ImpliedProvider() string {
    typeName := r.Type
    if under := strings.Index(typeName, "_"); under != -1 {
        typeName = typeName[:under]
    }
    return typeName
}
```

`aws_instance` → `"aws"`, `google_compute_instance` → `"google"`

이 규칙이 Terraform에서 프로바이더를 자동으로 추론하는 기반이다.

### 3.5 ResourceInstance

```go
type ResourceInstance struct {
    referenceable
    Resource Resource
    Key      InstanceKey
}

func (r ResourceInstance) String() string {
    if r.Key == NoKey {
        return r.Resource.String()         // "aws_instance.web"
    }
    return r.Resource.String() + r.Key.String()  // "aws_instance.web[0]"
}
```

### 3.6 주소 타입 계층

```
Resource (상대)
├── "aws_instance.web"
│
├── ResourceInstance (상대 + 인스턴스 키)
│   ├── "aws_instance.web"        (NoKey)
│   ├── "aws_instance.web[0]"     (IntKey(0))
│   └── "aws_instance.web[\"a\"]" (StringKey("a"))
│
AbsResource (절대)
├── "aws_instance.web"                      (루트 모듈)
├── "module.vpc.aws_instance.web"           (모듈 내)
│
AbsResourceInstance (절대 + 인스턴스 키)
├── "module.vpc.aws_instance.web[0]"
│
ConfigResource (설정 레벨)
├── "module.vpc.aws_instance.web"           (인스턴스 키 없음)
```

### 3.7 AbsResource 구조체

```go
type AbsResource struct {
    targetable
    Module   ModuleInstance
    Resource Resource
}

// Instance — 인스턴스 키 추가
func (r AbsResource) Instance(key InstanceKey) AbsResourceInstance {
    return AbsResourceInstance{
        Module:   r.Module,
        Resource: r.Resource.Instance(key),
    }
}

// Config — ConfigResource로 변환 (인스턴스 정보 제거)
func (r AbsResource) Config() ConfigResource {
    return ConfigResource{
        Module:   r.Module.Module(),  // ModuleInstance → Module
        Resource: r.Resource,
    }
}
```

### 3.8 AbsResourceInstance

```go
type AbsResourceInstance struct {
    targetable
    Module   ModuleInstance
    Resource ResourceInstance
}
```

Terraform State의 각 항목은 `AbsResourceInstance`로 식별된다. 이것이 State의 "주민등록번호"이다.

---

## 4. Provider 주소

### 4.1 Provider 타입

```go
// Provider는 tfaddr.Provider의 별칭
type Provider = tfaddr.Provider
```

실제 구조는 3-부분 주소:

```
hostname / namespace / type
registry.terraform.io / hashicorp / aws
```

### 4.2 주요 상수

```go
const DefaultProviderRegistryHost = tfaddr.DefaultProviderRegistryHost
// → "registry.terraform.io"

const BuiltInProviderHost = tfaddr.BuiltInProviderHost
// → "terraform.io"

const BuiltInProviderNamespace = tfaddr.BuiltInProviderNamespace
// → "builtin"

const LegacyProviderNamespace = tfaddr.LegacyProviderNamespace
// → "-"
```

### 4.3 기본 프로바이더

```go
func NewDefaultProvider(name string) Provider {
    return tfaddr.Provider{
        Type:      MustParseProviderPart(name),
        Namespace: "hashicorp",
        Hostname:  DefaultProviderRegistryHost,
    }
}
// NewDefaultProvider("aws") → "registry.terraform.io/hashicorp/aws"
```

### 4.4 내장 프로바이더

```go
func NewBuiltInProvider(name string) Provider {
    return tfaddr.Provider{
        Type:      MustParseProviderPart(name),
        Namespace: BuiltInProviderNamespace,
        Hostname:  BuiltInProviderHost,
    }
}
// NewBuiltInProvider("terraform") → "terraform.io/builtin/terraform"
```

### 4.5 ImpliedProviderForUnqualifiedType

```go
func ImpliedProviderForUnqualifiedType(typeName string) Provider {
    switch typeName {
    case "terraform":
        return NewBuiltInProvider(typeName)
    default:
        return NewDefaultProvider(typeName)
    }
}
```

| 입력 | 결과 |
|------|------|
| `"aws"` | `registry.terraform.io/hashicorp/aws` |
| `"google"` | `registry.terraform.io/hashicorp/google` |
| `"terraform"` | `terraform.io/builtin/terraform` (특수 케이스) |

### 4.6 IsDefaultProvider

```go
func IsDefaultProvider(addr Provider) bool {
    return addr.Hostname == DefaultProviderRegistryHost &&
           addr.Namespace == "hashicorp"
}
```

### 4.7 프로바이더 주소 전체 스펙트럼

```
┌─────────────────────────────────────────────────────────────┐
│  Provider 주소 체계                                          │
├─────────────────────────────────────────────────────────────┤
│                                                             │
│  registry.terraform.io/hashicorp/aws        ← Default      │
│  registry.terraform.io/hashicorp/google     ← Default      │
│  registry.terraform.io/custom-ns/custom     ← 커스텀        │
│  terraform.io/builtin/terraform             ← 내장          │
│  app.terraform.io/my-org/my-provider        ← 사설 레지스트리 │
│  registry.terraform.io/-/legacy             ← 레거시 (비권장) │
│                                                             │
│  hostname / namespace / type                                │
│  ──────── / ───────── / ────                                │
└─────────────────────────────────────────────────────────────┘
```

---

## 5. InstanceKey: NoKey, IntKey, StringKey

### 5.1 InstanceKey 인터페이스

`internal/addrs/instance_key.go`에 정의:

```go
type InstanceKey interface {
    instanceKeySigil()
    String() string
    Value() cty.Value
}
```

### 5.2 세 가지 구현

```go
// NoKey — count/for_each 없는 단일 인스턴스
var NoKey InstanceKey  // nil

// IntKey — count 사용 시 (정수 인덱스)
type IntKey int
func (k IntKey) String() string { return fmt.Sprintf("[%d]", int(k)) }
func (k IntKey) Value() cty.Value { return cty.NumberIntVal(int64(k)) }

// StringKey — for_each 사용 시 (문자열 키)
type StringKey string
func (k StringKey) String() string {
    return fmt.Sprintf("[%s]", toHCLQuotedString(string(k)))
}
func (k StringKey) Value() cty.Value { return cty.StringVal(string(k)) }
```

### 5.3 WildcardKey (실험적)

```go
// WildcardKey represents the "unknown" value of an InstanceKey.
// Used within the deferral logic to express absolute addresses
// that are not known at the time of planning.
var WildcardKey InstanceKey = &wildcardKey{}

type wildcardKey struct{}
func (w *wildcardKey) String() string { return "[*]" }
func (w *wildcardKey) Value() cty.Value { return cty.DynamicVal }
```

### 5.4 ParseInstanceKey

```go
func ParseInstanceKey(key cty.Value) (InstanceKey, error) {
    switch key.Type() {
    case cty.String:
        return StringKey(key.AsString()), nil
    case cty.Number:
        var idx int
        err := gocty.FromCtyValue(key, &idx)
        return IntKey(idx), err
    default:
        return NoKey, fmt.Errorf("either a string or an integer is required")
    }
}
```

### 5.5 정렬 순서

```go
func InstanceKeyLess(i, j InstanceKey) bool {
    // 정렬 순서: NoKey < IntKey < StringKey
    // 같은 타입 내: 숫자 순 또는 문자열 순
}
```

```
정렬 예시:
aws_instance.web          (NoKey)    ← 최소
aws_instance.web[0]       (IntKey)
aws_instance.web[1]       (IntKey)
aws_instance.web["a"]     (StringKey)
aws_instance.web["b"]     (StringKey)  ← 최대
```

### 5.6 InstanceKeyType

```go
type InstanceKeyType rune

const (
    NoKeyType     InstanceKeyType = 0
    IntKeyType    InstanceKeyType = 'I'
    StringKeyType InstanceKeyType = 'S'
    UnknownKeyType InstanceKeyType = '?'  // 아직 모르는 키 타입
)
```

### 5.7 HCL 문자열 인코딩

```go
func toHCLQuotedString(s string) string {
    buf.WriteByte('"')
    for i, r := range s {
        switch r {
        case '\n': buf.WriteString(`\n`)
        case '\r': buf.WriteString(`\r`)
        case '\t': buf.WriteString(`\t`)
        case '"':  buf.WriteString(`\"`)
        case '\\': buf.WriteString(`\\`)
        case '$', '%':
            buf.WriteRune(r)
            // "${" → "$${"로 이스케이프
            if len(remain) > 0 && remain[0] == '{' {
                buf.WriteRune(r)
            }
        }
    }
    buf.WriteByte('"')
}
```

StringKey의 String() 출력이 HCL 파서로 다시 파싱 가능하도록 보장한다.

---

## 6. 절대 주소와 상대 주소

### 6.1 주소 타입 분류

```
┌─────────────────────────────────────────────────────────┐
│                  주소 타입 분류                           │
├─────────────────────────────────────────────────────────┤
│                                                         │
│  상대 주소 (Relative) — 모듈 내에서만 유효              │
│  ├── Resource          "aws_instance.web"               │
│  ├── ResourceInstance  "aws_instance.web[0]"            │
│  ├── ModuleCall        "module.vpc"                     │
│  └── InputVariable     "var.name"                       │
│                                                         │
│  절대 주소 (Absolute) — 전체 트리에서 유일               │
│  ├── AbsResource              "module.vpc.aws_instance.web"       │
│  ├── AbsResourceInstance      "module.vpc.aws_instance.web[0]"    │
│  ├── ModuleInstance           "module.vpc[0]"                     │
│  └── AbsProviderConfig        "provider[\"registry.../aws\"]"    │
│                                                         │
│  설정 주소 (Config) — 인스턴스 키 없는 정적 주소         │
│  ├── ConfigResource    "module.vpc.aws_instance.web"    │
│  └── Module            "module.vpc"                     │
│                                                         │
└─────────────────────────────────────────────────────────┘
```

### 6.2 상대 → 절대 변환

```go
// Resource → AbsResource
func (r Resource) Absolute(module ModuleInstance) AbsResource {
    return AbsResource{
        Module:   module,
        Resource: r,
    }
}

// Resource → ConfigResource
func (r Resource) InModule(module Module) ConfigResource {
    return ConfigResource{
        Module:   module,
        Resource: r,
    }
}

// ResourceInstance → AbsResourceInstance
func (r ResourceInstance) Absolute(module ModuleInstance) AbsResourceInstance {
    return AbsResourceInstance{
        Module:   module,
        Resource: r,
    }
}
```

### 6.3 절대 → 설정 변환

```go
// AbsResource → ConfigResource
func (r AbsResource) Config() ConfigResource {
    return ConfigResource{
        Module:   r.Module.Module(),  // 인스턴스 키 제거
        Resource: r.Resource,
    }
}
```

### 6.4 변환 관계도

```
Resource                    AbsResource                ConfigResource
"aws_instance.web"    ───→  "module.vpc.              "module.vpc.
                             aws_instance.web"         aws_instance.web"
    │                           │                          ↑
    │ .Instance(IntKey(0))      │ .Config()                │
    ↓                           ↓                          │
ResourceInstance           AbsResourceInstance              │
"aws_instance.web[0]" ───→ "module.vpc.               .ContainingResource()
                            aws_instance.web[0]"  ────→    │
                                │
                                │ .ContainingResource()
                                ↓
                            AbsResource
                            "module.vpc.aws_instance.web"
```

---

## 7. 참조 파싱 (parse_ref.go)

### 7.1 Reference 구조체

```go
type Reference struct {
    Subject     Referenceable
    SourceRange tfdiags.SourceRange
    Remaining   hcl.Traversal
}
```

Reference는 HCL 표현식에서 추출된 참조를 나타낸다. `Subject`는 참조 대상, `Remaining`은 아직 해석되지 않은 속성 접근이다.

### 7.2 ParseRef 함수

```go
func ParseRef(traversal hcl.Traversal) (*Reference, tfdiags.Diagnostics) {
    ref, diags := parseRef(traversal)
    if ref != nil {
        if len(ref.Remaining) == 0 {
            ref.Remaining = nil
        }
    }
    return ref, diags
}
```

### 7.3 parseRef 내부 로직

```
parseRef(traversal) 흐름:

입력: hcl.Traversal ["var", "name"]
    ↓
root := traversal[0].Name  // "var"
    ↓
switch root {
case "var":
    → InputVariable{Name: "name"}
case "local":
    → LocalValue{Name: "name"}
case "module":
    → Module call 주소 파싱
case "data":
    → DataResourceMode Resource 파싱
case "resource":
    → ManagedResourceMode Resource 파싱
case "self":
    → Self 참조
case "each":
    → ForEachAttr
case "count":
    → CountAttr
case "path":
    → PathAttr
case "terraform":
    → TerraformAttr
default:
    → 암시적 리소스 참조 (타입.이름)
}
```

### 7.4 참조 타입 전체 목록

| 접두사 | Subject 타입 | 예시 |
|--------|-------------|------|
| `var.` | `InputVariable` | `var.instance_type` |
| `local.` | `LocalValue` | `local.common_tags` |
| `module.` | `ModuleCallOutput` | `module.vpc.subnet_id` |
| `data.` | `Resource (DataMode)` | `data.aws_ami.latest.id` |
| `resource.` | `Resource (ManagedMode)` | `resource.aws_instance.web.id` |
| `self` | `Self` | `self.private_ip` |
| `each.` | `ForEachAttr` | `each.key`, `each.value` |
| `count.` | `CountAttr` | `count.index` |
| `path.` | `PathAttr` | `path.module`, `path.root`, `path.cwd` |
| `terraform.` | `TerraformAttr` | `terraform.workspace` |
| (암시적) | `Resource` | `aws_instance.web.id` |

### 7.5 암시적 리소스 참조

```hcl
# 명시적 참조
resource "aws_instance" "web" {
  ami = data.aws_ami.latest.id
}

# 암시적 참조 (resource. 접두사 생략)
output "ip" {
  value = aws_instance.web.public_ip
}
```

파서는 root 이름이 알려진 키워드가 아니면 암시적으로 리소스 참조로 해석한다:

```
traversal: ["aws_instance", "web", "public_ip"]
    ↓
root "aws_instance" — 알려진 키워드 아님
    ↓
Resource{Mode: ManagedResourceMode, Type: "aws_instance", Name: "web"}
Remaining: ["public_ip"]
```

### 7.6 DisplayString

```go
func (r *Reference) DisplayString() string {
    if len(r.Remaining) == 0 {
        return r.Subject.String()
    }
    var ret strings.Builder
    ret.WriteString(r.Subject.String())
    for _, step := range r.Remaining {
        switch tStep := step.(type) {
        case hcl.TraverseAttr:
            ret.WriteByte('.')
            ret.WriteString(tStep.Name)
        case hcl.TraverseIndex:
            ret.WriteByte('[')
            // ... 인덱스 출력
            ret.WriteByte(']')
        }
    }
    return ret.String()
}
```

### 7.7 테스팅 스코프 확장

```go
func ParseRefFromTestingScope(traversal hcl.Traversal) (*Reference, tfdiags.Diagnostics) {
    switch root {
    case "output":
        // 테스트에서는 output을 직접 참조 가능
        return &Reference{
            Subject: OutputValue{Name: name},
            // ...
        }, diags
    case "check":
        // 테스트에서는 check 블록 참조 가능
        // ...
    }
}
```

---

## 8. Target 주소 해석

### 8.1 Targetable 인터페이스

```go
type Targetable interface {
    targetableSigil()
    String() string
    TargetContains(other Targetable) bool
    AddrType() TargetableAddrType
}
```

`-target` 플래그로 지정할 수 있는 모든 주소 타입이 이 인터페이스를 구현한다.

### 8.2 타겟 가능한 타입들

```go
type TargetableAddrType int

const (
    ModuleAddrType               TargetableAddrType = iota
    ModuleInstanceAddrType
    AbsResourceAddrType
    AbsResourceInstanceAddrType
    ConfigResourceAddrType
)
```

### 8.3 TargetContains 동작

```
-target=module.vpc
    ├── module.vpc                         ✓ (직접 매칭)
    ├── module.vpc.aws_instance.web        ✓ (하위 리소스)
    ├── module.vpc.aws_instance.web[0]     ✓ (하위 리소스 인스턴스)
    ├── module.vpc.module.subnet           ✓ (하위 모듈)
    └── module.other.aws_instance.web      ✗ (다른 모듈)

-target=aws_instance.web
    ├── aws_instance.web                   ✓ (직접 매칭)
    ├── aws_instance.web[0]                ✓ (인스턴스)
    ├── aws_instance.web["a"]              ✓ (인스턴스)
    └── aws_instance.other                 ✗ (다른 리소스)

-target=aws_instance.web[0]
    ├── aws_instance.web[0]                ✓ (직접 매칭)
    ├── aws_instance.web                   ✗ (더 넓은 범위)
    └── aws_instance.web[1]                ✗ (다른 인스턴스)
```

### 8.4 AbsResource.TargetContains 구현

```go
func (r AbsResource) TargetContains(other Targetable) bool {
    switch to := other.(type) {
    case AbsResource:
        return to.String() == r.String()
    case ConfigResource:
        return to.String() == r.String()
    case AbsResourceInstance:
        return r.TargetContains(to.ContainingResource())
    default:
        return false
    }
}
```

### 8.5 ParseTarget 흐름

```
-target=module.vpc.aws_instance.web[0]
    ↓
HCL Traversal:
    [TraverseRoot("module"), TraverseAttr("vpc"),
     TraverseAttr("aws_instance"), TraverseAttr("web"),
     TraverseIndex(0)]
    ↓
1. parseModuleInstancePrefix → ModuleInstance{{"vpc", NoKey}}
    ↓
2. 남은 traversal: [TraverseRoot("aws_instance"), TraverseAttr("web"), TraverseIndex(0)]
    ↓
3. Resource 파싱: Resource{Mode: Managed, Type: "aws_instance", Name: "web"}
    ↓
4. InstanceKey 파싱: IntKey(0)
    ↓
결과: AbsResourceInstance{
    Module: ModuleInstance{{"vpc", NoKey}},
    Resource: ResourceInstance{
        Resource: Resource{Managed, "aws_instance", "web"},
        Key: IntKey(0),
    },
}
```

---

## 9. Move 엔드포인트 주소

### 9.1 MoveEndpoint 구조체

`internal/addrs/move_endpoint.go`에 정의:

```go
type MoveEndpoint struct {
    SourceRange tfdiags.SourceRange
    relSubject  AbsMoveable  // 내부적으로 AbsMoveable로 상대 주소 표현
}
```

### 9.2 왜 relSubject가 AbsMoveable인가

주석이 이 설계를 설명한다:

> We (ab)use AbsMoveable as the representation of our relative address, even though everywhere else in Terraform AbsMoveable always represents a fully-absolute address.

`MoveEndpoint`는 **상대 주소**인데, 내부적으로 **절대 주소** 타입을 "오용"한다. 이는 상대 주소만을 위한 별도 타입을 만들지 않기 위한 실용적 결정이다.

### 9.3 MoveEndpointKind

```go
type MoveEndpointKind int

const (
    MoveEndpointModule   MoveEndpointKind  // 모듈 이동
    MoveEndpointResource MoveEndpointKind  // 리소스 이동
)
```

### 9.4 UnifyMoveEndpoints

moved 블록의 from과 to는 같은 종류여야 한다:

```hcl
# 유효: 리소스 → 리소스
moved {
  from = aws_instance.old
  to   = aws_instance.new
}

# 유효: 모듈 → 모듈
moved {
  from = module.old
  to   = module.new
}

# 무효: 리소스 → 모듈 (불일치)
moved {
  from = aws_instance.old
  to   = module.new
}
```

```go
func UnifyMoveEndpoints(module Module, from, to *MoveEndpoint) (*MoveEndpointInModule, *MoveEndpointInModule) {
    // 두 엔드포인트의 종류가 일치하는지 확인
    // module 주소를 기준으로 절대 주소로 변환
}
```

### 9.5 MightUnifyWith

```go
func (e *MoveEndpoint) MightUnifyWith(other *MoveEndpoint) bool {
    // 초기 정적 검증: 명백히 잘못된 조합 감지
    // 예: 모듈 주소 ↔ 리소스 주소
}
```

---

## 10. 주소 문자열 ↔ 구조체 변환

### 10.1 구조체 → 문자열

```
주소 구조체                    문자열 표현
────────────                    ──────────
Module{"vpc"}                → "module.vpc"
Module{"vpc","subnet"}       → "module.vpc.module.subnet"

Resource{Managed,"aws_instance","web"}
                             → "aws_instance.web"
Resource{Data,"aws_ami","latest"}
                             → "data.aws_ami.latest"

ResourceInstance{..., IntKey(0)}
                             → "aws_instance.web[0]"
ResourceInstance{..., StringKey("a")}
                             → "aws_instance.web[\"a\"]"

AbsResource{
  Module: ModuleInstance{{"vpc",NoKey}},
  Resource: Resource{Managed,"aws_instance","web"},
}                            → "module.vpc.aws_instance.web"

AbsResourceInstance{
  Module: ModuleInstance{{"vpc",IntKey(0)}},
  Resource: ResourceInstance{
    Resource{Managed,"aws_instance","web"},
    IntKey(1)},
}                            → "module.vpc[0].aws_instance.web[1]"
```

### 10.2 문자열 → 구조체 (HCL Traversal 경유)

문자열을 직접 파싱하지 않고, HCL Traversal을 거친다:

```
"module.vpc.aws_instance.web[0]"
    ↓ HCL 파서
hcl.Traversal{
    TraverseRoot{Name: "module"},
    TraverseAttr{Name: "vpc"},
    TraverseAttr{Name: "aws_instance"},
    TraverseAttr{Name: "web"},
    TraverseIndex{Key: cty.NumberIntVal(0)},
}
    ↓ parseModuleInstancePrefix()
ModuleInstance: {{"vpc", NoKey}}
Remaining: {TraverseRoot{"aws_instance"}, TraverseAttr{"web"}, TraverseIndex{0}}
    ↓ 리소스 파싱
Resource: {Managed, "aws_instance", "web"}
InstanceKey: IntKey(0)
    ↓
AbsResourceInstance{
    Module:   ModuleInstance{{"vpc", NoKey}},
    Resource: ResourceInstance{
        Resource: {Managed, "aws_instance", "web"},
        Key:      IntKey(0),
    },
}
```

### 10.3 UniqueKey

```go
func (r Resource) UniqueKey() UniqueKey {
    return r  // Resource 자체가 UniqueKey
}

type moduleKey string
func (m Module) UniqueKey() UniqueKey {
    return moduleKey(m.String())
}
```

`UniqueKey`는 맵의 키로 사용할 수 있는 비교 가능한 타입이다. Go의 슬라이스는 맵 키로 쓸 수 없으므로, `Module`은 문자열로 변환하여 키를 생성한다.

---

## 11. Traversal 기반 주소 해석

### 11.1 HCL Traversal이란

HCL(HashiCorp Configuration Language)에서 `module.vpc.aws_instance.web`은 "순회(traversal)"로 표현된다:

```
module.vpc.aws_instance.web
^root  ^attr ^attr       ^attr
```

각 요소가 `hcl.TraverseRoot`, `hcl.TraverseAttr`, `hcl.TraverseIndex` 중 하나이다.

### 11.2 parseModulePrefix

```go
func parseModulePrefix(traversal hcl.Traversal) (Module, hcl.Traversal, tfdiags.Diagnostics) {
    remain := traversal
    var mod Module

    for len(remain) > 0 {
        var next string
        switch tt := remain[0].(type) {
        case hcl.TraverseRoot:
            next = tt.Name
        case hcl.TraverseAttr:
            next = tt.Name
        }

        if next != "module" {
            break  // "module"이 아니면 모듈 접두사 끝
        }

        remain = remain[1:]
        // 다음 요소에서 모듈 이름 추출
        switch tt := remain[0].(type) {
        case hcl.TraverseAttr:
            moduleName = tt.Name
        }
        remain = remain[1:]

        // 인스턴스 키 확인 (Module은 키 불허)
        if _, ok := remain[0].(hcl.TraverseIndex); ok {
            // 에러: "Module instance keys not allowed"
        }

        mod = append(mod, moduleName)
    }

    return mod, remain, diags
}
```

### 11.3 파싱 단계 시각화

```
입력 Traversal:
[Root("module"), Attr("vpc"), Attr("module"), Attr("subnet"), Attr("aws_instance"), Attr("web")]

단계 1: next = "module" → remain 진행
단계 2: moduleName = "vpc" → mod = ["vpc"]
단계 3: next = "module" → remain 진행
단계 4: moduleName = "subnet" → mod = ["vpc", "subnet"]
단계 5: next = "aws_instance" ≠ "module" → BREAK

결과:
  mod = Module{"vpc", "subnet"}
  remain = [Root("aws_instance"), Attr("web")]
```

### 11.4 TraverseRoot 정규화

```go
// parseModulePrefix의 반환값 처리:
if tt, ok := retRemain[0].(hcl.TraverseAttr); ok {
    retRemain[0] = hcl.TraverseRoot{
        Name:     tt.Name,
        SrcRange: tt.SrcRange,
    }
}
```

남은 traversal의 첫 요소가 `TraverseAttr`일 수 있는데, 호출자의 편의를 위해 항상 `TraverseRoot`로 정규화한다.

---

## 12. 설계 결정: 왜 주소 체계가 복잡한가

### 12.1 복잡성의 원인

```
1. 모듈 중첩
   module.a.module.b.module.c.aws_instance.web
   → 임의 깊이의 모듈 중첩 지원

2. 인스턴스 키
   module.vpc[0].aws_instance.web["us-east-1"]
   → count, for_each로 인한 다중 인스턴스

3. 정적 vs 동적
   Module vs ModuleInstance
   ConfigResource vs AbsResourceInstance
   → 설정 시점과 실행 시점의 주소가 다름

4. 다양한 리소스 모드
   resource, data, ephemeral, list
   → 각각 다른 접두사와 동작

5. 프로바이더 주소
   registry.terraform.io/hashicorp/aws
   → 호스트, 네임스페이스, 타입의 3-부분 구조

6. 이전 버전 호환
   LegacyProviderNamespace = "-"
   → 0.12 이전의 주소 체계 호환
```

### 12.2 왜 타입을 이렇게 많이 나누었는가

| 타입 | 필요한 이유 |
|------|-----------|
| `Resource` | 모듈 내에서의 상대적 식별 |
| `AbsResource` | 전체 트리에서의 절대 식별 |
| `ConfigResource` | 설정 파일 수준 (인스턴스 키 불필요) |
| `AbsResourceInstance` | State의 최소 단위 (인스턴스 키 포함) |

각 타입이 자신만의 `TargetContains`, `String`, `UniqueKey` 메서드를 가지므로, 타입 안전성이 보장된다.

### 12.3 왜 문자열이 아닌 구조체인가

```
문자열: "module.vpc.aws_instance.web[0]"
  - 파싱 오버헤드
  - 오타 가능
  - 부분 비교 어려움 (모듈 경로만 비교하려면?)

구조체: AbsResourceInstance{Module: ..., Resource: ...}
  - 타입 안전
  - 컴파일 타임 검증
  - 효율적인 부분 비교 (.Module 필드만 비교)
  - 메서드 바인딩 (TargetContains, Config, Instance 등)
```

### 12.4 Trade-off 인정

```go
// move_endpoint.go 주석:
// Internally we (ab)use AbsMoveable as the representation of our
// relative address, even though everywhere else in Terraform
// AbsMoveable always represents a fully-absolute address.
```

완벽한 타입 시스템을 추구하면 타입 수가 폭발하고, 너무 느슨하면 런타임 에러가 발생한다. Terraform은 실용적 중간점을 선택했다.

---

## 13. 정리

### 주소 타입 전체 맵

```
                    ┌─────────────────────┐
                    │   Targetable        │
                    │   (타겟 가능)        │
                    └─────────┬───────────┘
              ┌───────────────┼───────────────┐
              ↓               ↓               ↓
        ┌──────────┐   ┌──────────┐    ┌──────────────┐
        │  Module  │   │  AbsRes  │    │ AbsResInst   │
        │          │   │  ource   │    │ ance         │
        └──────────┘   └──────────┘    └──────────────┘
              │               │               │
              ↓               ↓               ↓
        ┌──────────┐   ┌──────────┐    ┌──────────────┐
        │ Module   │   │  Config  │    │              │
        │ Instance │   │ Resource │    │              │
        └──────────┘   └──────────┘    └──────────────┘
```

### 핵심 요약

| 개념 | 설명 |
|------|------|
| **Module** | `[]string` — 정적 모듈 경로 |
| **ModuleInstance** | 모듈 + InstanceKey — 동적 인스턴스 |
| **Resource** | Mode + Type + Name — 상대 리소스 주소 |
| **AbsResourceInstance** | Module + Resource + Key — State의 최소 단위 |
| **Provider** | Host + Namespace + Type — 3-부분 프로바이더 식별자 |
| **InstanceKey** | NoKey, IntKey, StringKey — count/for_each 인덱스 |
| **Reference** | Subject + Remaining — HCL 표현식의 참조 |
| **MoveEndpoint** | 상대 주소 — moved 블록 전용 |
| **Traversal** | HCL의 속성 접근 체인 — 주소 파싱의 기반 |

### 학습 포인트

1. **타입 안전성**: 문자열 대신 구조체로 주소를 표현하여 컴파일 타임 검증
2. **정적 vs 동적 분리**: Module과 ModuleInstance의 구분
3. **ImpliedProvider 규칙**: 리소스 타입의 접두사로 프로바이더 추론
4. **TargetContains**: 계층적 포함 관계로 -target 범위 결정
5. **HCL Traversal**: 주소 파싱이 HCL 표현식 파싱과 통합
