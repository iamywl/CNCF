# 02. Terraform 데이터 모델

## 1. 개요

Terraform의 데이터 모델은 크게 세 가지 핵심 도메인으로 나뉜다:
1. **Config** — 사용자가 작성한 HCL 설정
2. **State** — 실제 인프라의 현재 상태
3. **Plan** — Config와 State 간의 차이(변경 계획)

이 세 가지가 상호작용하여 인프라 변경을 안전하게 수행한다.

```
┌──────────┐     비교      ┌──────────┐
│  Config  │ ──────────── │  State   │
│ (원하는  │              │ (현재    │
│  상태)   │              │  상태)   │
└────┬─────┘              └────┬─────┘
     │                        │
     └────────┬───────────────┘
              ▼
        ┌──────────┐
        │   Plan   │
        │ (변경    │
        │  계획)   │
        └──────────┘
```

## 2. Config 데이터 모델

### 2.1 Config 트리

```go
// internal/configs/config.go
type Config struct {
    Root     *Config                  // 루트 설정 (자기 참조)
    Parent   *Config                  // 부모 모듈
    Path     addrs.Module             // 루트로부터의 경로
    Children map[string]*Config       // 자식 모듈
    Module   *Module                  // 파싱된 모듈 내용

    SourceAddr  addrs.ModuleSource    // 모듈 소스 주소
    Version     *version.Version      // 해석된 버전
    CallRange   hcl.Range             // 소스 위치
}
```

Config는 모듈 호출에 따라 **트리 구조**를 형성한다:

```
Config Tree
├── Root (.)
│   ├── Module: {Resources, Variables, Outputs, Locals...}
│   └── Children:
│       ├── "network" → Config
│       │   ├── Module: {Resources...}
│       │   └── Children:
│       │       └── "subnet" → Config
│       └── "compute" → Config
│           └── Module: {Resources...}
```

### 2.2 Module 구조체

```go
// internal/configs/module.go (개념적 구조)
type Module struct {
    SourceDir string

    // 핵심 블록
    ManagedResources map[string]*Resource      // resource 블록
    DataResources    map[string]*Resource      // data 블록
    ModuleCalls      map[string]*ModuleCall    // module 블록

    // 값
    Variables   map[string]*Variable           // variable 블록
    Outputs     map[string]*Output             // output 블록
    LocalValues map[string]*Local              // locals 블록

    // 프로바이더
    ProviderConfigs  map[string]*Provider       // provider 블록
    RequiredProviders *RequiredProviders        // required_providers

    // 기타
    Backend     *Backend                       // backend 블록
    Checks      []*Check                       // check 블록
    Moved       []*Moved                       // moved 블록
    Removed     []*Removed                     // removed 블록
}
```

### 2.3 Resource 구조체

```go
// internal/configs/resource.go (개념적 구조)
type Resource struct {
    Mode    addrs.ResourceMode          // Managed 또는 Data
    Name    string                      // 리소스 이름
    Type    string                      // 리소스 타입 (예: "aws_instance")

    Config  hcl.Body                    // HCL 본문 (속성 값)
    Count   hcl.Expression             // count 표현식
    ForEach hcl.Expression             // for_each 표현식

    ProviderConfigRef *ProviderConfigRef // 프로바이더 참조

    DependsOn []hcl.Traversal          // explicit depends_on

    // 라이프사이클
    Managed *ManagedResource            // lifecycle, provisioner 등
}

type ManagedResource struct {
    CreateBeforeDestroy bool
    PreventDestroy      bool
    IgnoreChanges       []hcl.Traversal
    ReplaceTriggeredBy  []hcl.Expression
}
```

## 3. State 데이터 모델

### 3.1 State 전체 구조

```go
// internal/states/state.go
type State struct {
    Modules          map[string]*Module      // 모듈별 상태
    RootOutputValues map[string]*OutputValue  // 루트 출력값
    CheckResults     *CheckResults            // 체크 결과
}
```

### 3.2 Module 상태

```go
// internal/states/module.go
type Module struct {
    Addr         addrs.ModuleInstance         // 모듈 인스턴스 주소
    Resources    map[string]*Resource         // 리소스 상태
    OutputValues map[string]*OutputValue      // 출력값
}
```

### 3.3 Resource 상태

```go
// internal/states/resource.go (개념적 구조)
type Resource struct {
    Addr           addrs.AbsResource          // 절대 리소스 주소
    Instances      map[addrs.InstanceKey]*ResourceInstance
    ProviderConfig addrs.AbsProviderConfig    // 프로바이더 설정
}

type ResourceInstance struct {
    Current *ResourceInstanceObjectSrc        // 현재 객체
    Deposed map[DeposedKey]*ResourceInstanceObjectSrc  // 교체 대기 객체
}

type ResourceInstanceObjectSrc struct {
    Status             ObjectStatus            // Ready, Tainted, ...
    SchemaVersion      uint64                  // 스키마 버전
    AttrsJSON          []byte                  // 속성 JSON
    AttrsFlat          map[string]string       // 평탄화된 속성
    AttrSensitivePaths []cty.Path             // 민감 속성 경로
    Private            []byte                  // 프로바이더 전용 데이터
    Dependencies       []addrs.ConfigResource  // 의존성
    CreateBeforeDestroy bool                   // CBD 플래그
}
```

### 3.4 State 계층 구조 다이어그램

```
State
├── Modules[""]  (루트 모듈)
│   ├── Addr: ""
│   ├── Resources["aws_vpc.main"]
│   │   ├── Addr: aws_vpc.main
│   │   ├── ProviderConfig: provider["registry.terraform.io/hashicorp/aws"]
│   │   └── Instances[NoKey]
│   │       └── Current:
│   │           ├── Status: Ready
│   │           ├── SchemaVersion: 1
│   │           ├── AttrsJSON: {"id":"vpc-123","cidr_block":"10.0.0.0/16",...}
│   │           └── Dependencies: []
│   │
│   ├── Resources["aws_instance.web"]
│   │   ├── Instances[IntKey(0)]  ← count = 2
│   │   │   └── Current: {Status: Ready, ...}
│   │   └── Instances[IntKey(1)]
│   │       └── Current: {Status: Ready, ...}
│   │
│   └── OutputValues["vpc_id"]
│       └── Value: cty.StringVal("vpc-123")
│
└── Modules["module.network"]
    ├── Addr: module.network
    └── Resources["aws_subnet.private"]
        └── Instances[StringKey("us-east-1a")]  ← for_each
            └── Current: {Status: Ready, ...}
```

### 3.5 SyncState (스레드 안전 래퍼)

```go
// internal/states/sync.go
type SyncState struct {
    state *State
    lock  sync.RWMutex
}

// 주요 메서드 (모두 lock 보호)
func (s *SyncState) SetResourceInstanceCurrent(addr, obj, provider)
func (s *SyncState) RemoveResourceInstanceObject(addr, key)
func (s *SyncState) SetOutputValue(addr, value)
func (s *SyncState) Module(addr) *Module
```

> **왜 SyncState인가?** — DAG 워크 중 여러 고루틴이 동시에 State를 읽고 쓰기 때문에, `sync.RWMutex`로 보호하는 래퍼가 필수적이다.

## 4. Plan 데이터 모델

### 4.1 Plan 구조체

```go
// internal/plans/plan.go
type Plan struct {
    UIMode   Mode                              // Normal, Destroy, RefreshOnly

    // 변경 사항
    Changes          *ChangesSrc                // 리소스/출력 변경
    DriftedResources []*ResourceInstanceChangeSrc  // 드리프트 감지

    // 변수
    VariableValues    map[string]DynamicValue    // 입력 변수 값
    VariableMarks     map[string][]cty.PathValueMarks
    ApplyTimeVariables collections.Set[string]   // Apply 시점 변수

    // 타겟팅
    TargetAddrs       []addrs.Targetable         // -target 주소
    ForceReplaceAddrs []addrs.AbsResourceInstance // -replace 주소

    // 상태
    Backend    *Backend                         // 백엔드 설정
    StateStore *StateStore                      // 상태 저장소

    // 상태 플래그
    Complete  bool                              // 모든 인스턴스에 액션 존재
    Applyable bool                              // 유효하고 변경 있음
}
```

### 4.2 Changes 구조체

```go
// internal/plans/changes.go
type Changes struct {
    Resources         []*ResourceInstanceChange   // 리소스 변경
    Outputs           []*OutputChange              // 출력 변경
    Queries           []*QueryInstance             // 쿼리 인스턴스
    ActionInvocations ActionInvocationInstances   // 액션 호출
}

type ResourceInstanceChange struct {
    Addr         addrs.AbsResourceInstance
    PrevRunAddr  addrs.AbsResourceInstance  // moved 시 이전 주소
    ProviderAddr addrs.AbsProviderConfig

    DeposedKey   DeposedKey

    Change                                  // 실제 변경 내용
}

type Change struct {
    Action Action                           // 변경 액션
    Before cty.Value                        // 변경 전 값
    After  cty.Value                        // 변경 후 값

    Importing    *ImportingSrc              // import 시
    GeneratedConfig string                  // 생성된 설정
}
```

### 4.3 변경 액션 (Action)

```go
// internal/plans/action.go
type Action rune

const (
    NoOp               Action = 'N'    // 변경 없음
    Create             Action = 'C'    // 새로 생성
    Read               Action = 'R'    // 데이터 소스 읽기
    Update             Action = 'U'    // 기존 수정
    Delete             Action = 'D'    // 삭제
    DeleteThenCreate   Action = 'P'    // 삭제 후 생성 (교체)
    CreateThenDelete   Action = 'A'    // 생성 후 삭제 (CBD 교체)
    Forget             Action = 'F'    // State에서 제거 (실제 삭제 안 함)
)
```

변경 액션별 의미:

| 액션 | 심볼 | 설명 | 예시 |
|------|------|------|------|
| NoOp | N | 변경 불필요 | 속성이 동일 |
| Create | C | 새 리소스 생성 | State에 없는 리소스 |
| Read | R | 데이터 소스 읽기 | `data "aws_ami" {}` |
| Update | U | 기존 리소스 수정 | 태그 변경 |
| Delete | D | 리소스 삭제 | Config에서 제거됨 |
| DeleteThenCreate | P | 교체 (삭제 우선) | 변경 불가 속성 수정 |
| CreateThenDelete | A | 교체 (생성 우선) | `create_before_destroy = true` |
| Forget | F | State에서만 제거 | `removed` 블록 사용 |

## 5. 주소(Address) 체계

### 5.1 핵심 주소 타입

```go
// internal/addrs/ 패키지
type Module      []string                     // ["network", "subnet"]
type ModuleInstance struct {                   // module.network[0]
    Module Module
    Key    InstanceKey
}

type Resource struct {                        // aws_instance.web
    Mode ResourceMode                         // Managed 또는 Data
    Type string                               // "aws_instance"
    Name string                               // "web"
}

type AbsResource struct {                     // module.network.aws_subnet.main
    Module   ModuleInstance
    Resource Resource
}

type ResourceInstance struct {                // aws_instance.web[0]
    Resource Resource
    Key      InstanceKey                     // IntKey(0), StringKey("a"), NoKey
}

type AbsResourceInstance struct {             // module.network.aws_instance.web[0]
    Module   ModuleInstance
    Resource ResourceInstance
}
```

### 5.2 주소 해석 예시

```
"module.network.aws_subnet.private[\"us-east-1a\"]"
 ├── ModuleInstance: {Module: ["network"], Key: NoKey}
 └── ResourceInstance:
     ├── Resource: {Mode: Managed, Type: "aws_subnet", Name: "private"}
     └── Key: StringKey("us-east-1a")
```

### 5.3 InstanceKey 종류

| 키 타입 | 소스 | 예시 |
|---------|------|------|
| `NoKey` | count/for_each 없음 | `aws_vpc.main` |
| `IntKey(n)` | `count = N` | `aws_instance.web[0]` |
| `StringKey(s)` | `for_each = {...}` | `aws_subnet.az["us-east-1a"]` |

## 6. 프로바이더 데이터 모델

### 6.1 Provider 주소

```go
// internal/addrs/provider.go
type Provider struct {
    Hostname  svchost.Hostname    // "registry.terraform.io"
    Namespace string              // "hashicorp"
    Type      string              // "aws"
}
// 전체 주소: registry.terraform.io/hashicorp/aws
```

### 6.2 Provider 인터페이스 요청/응답

```go
// internal/providers/provider.go — 주요 요청/응답 쌍

// Plan
type PlanResourceChangeRequest struct {
    TypeName         string
    PriorState       cty.Value      // 현재 State
    ProposedNewState cty.Value      // Config에서 계산된 목표 상태
    Config           cty.Value      // 원시 Config
    PriorPrivate     []byte         // 프로바이더 전용 메타데이터
}
type PlanResourceChangeResponse struct {
    PlannedState     cty.Value      // 계획된 최종 상태
    PlannedPrivate   []byte
    RequiresReplace  []cty.Path     // 교체 필요 속성
    Diagnostics      tfdiags.Diagnostics
}

// Apply
type ApplyResourceChangeRequest struct {
    TypeName       string
    PriorState     cty.Value       // 변경 전
    PlannedState   cty.Value       // Plan에서 계산된 목표
    Config         cty.Value       // 원시 Config
    PlannedPrivate []byte
}
type ApplyResourceChangeResponse struct {
    NewState    cty.Value          // 실제 적용 후 상태
    Private     []byte
    Diagnostics tfdiags.Diagnostics
}
```

## 7. 그래프 노드 데이터 모델

### 7.1 핵심 노드 인터페이스

```go
// internal/terraform/ — 주요 노드 인터페이스
type GraphNodeExecutable interface {
    Execute(EvalContext, walkOperation) tfdiags.Diagnostics
}

type GraphNodeDynamicExpandable interface {
    DynamicExpand(EvalContext) (*Graph, error)
}

type GraphNodeProvider interface {
    ProviderAddr() addrs.AbsProviderConfig
}

type GraphNodeConfigResource interface {
    ResourceAddr() addrs.ConfigResource
}

type GraphNodeModuleInstance interface {
    Path() addrs.ModuleInstance
}
```

### 7.2 주요 노드 타입

| 노드 | 역할 | 실행 내용 |
|------|------|----------|
| `nodeExpandModule` | 모듈 확장 | count/for_each로 모듈 인스턴스 생성 |
| `NodeAbstractResource` | 리소스 기반 | 리소스 노드의 공통 기반 |
| `NodePlannableResource` | Plan 리소스 | `PlanResourceChange()` 호출 |
| `NodeApplyableResource` | Apply 리소스 | `ApplyResourceChange()` 호출 |
| `NodeDestroyResource` | 삭제 리소스 | 리소스 삭제 실행 |
| `nodeExpandProvider` | 프로바이더 확장 | 프로바이더 초기화 및 설정 |
| `NodeApplyableOutput` | 출력 평가 | output 표현식 계산 |
| `nodeLocalValue` | 로컬 평가 | local 표현식 계산 |

## 8. cty 값 시스템

Terraform은 `cty`(Custom Types) 라이브러리를 사용하여 HCL 값을 타입 안전하게 처리한다:

```
cty.Value
├── Primitive Types
│   ├── cty.String  → cty.StringVal("hello")
│   ├── cty.Number  → cty.NumberIntVal(42)
│   └── cty.Bool    → cty.True / cty.False
├── Collection Types
│   ├── cty.List(cty.String)  → cty.ListVal([...])
│   ├── cty.Map(cty.String)   → cty.MapVal({...})
│   └── cty.Set(cty.String)   → cty.SetVal([...])
├── Structural Types
│   ├── cty.Object({...})     → 이름있는 속성
│   └── cty.Tuple([...])      → 순서있는 원소
├── Special Values
│   ├── cty.NullVal(type)     → null
│   ├── cty.UnknownVal(type)  → 아직 모르는 값 (Plan 시)
│   └── cty.DynamicVal        → 타입도 모르는 값
└── Mark System
    └── Sensitive 마킹 → 출력에서 마스킹
```

> **왜 cty인가?** — HCL은 JSON과 달리 `unknown` 값(Plan 시점에 아직 모르는 값, 예: 생성 후 할당되는 ID)을 표현해야 한다. cty는 이를 위해 `UnknownVal`을 제공하며, null과 unknown을 명확히 구분한다.

## 9. 데이터 흐름 요약

```
┌─────────────┐         ┌─────────────┐
│  .tf 파일   │         │  State 파일  │
│  (HCL)      │         │  (JSON)      │
└──────┬──────┘         └──────┬──────┘
       │ 파싱                   │ 로드
       ▼                       ▼
┌─────────────┐         ┌─────────────┐
│   Config    │         │    State    │
│ (원하는상태)│         │ (현재상태)  │
└──────┬──────┘         └──────┬──────┘
       │                       │
       └───────┬───────────────┘
               ▼
        ┌─────────────┐
        │  Plan 계산  │  ← provider.PlanResourceChange()
        │  (Diff)     │
        └──────┬──────┘
               │
               ▼
        ┌─────────────┐
        │   Apply     │  ← provider.ApplyResourceChange()
        └──────┬──────┘
               │
               ▼
        ┌─────────────┐
        │  새 State   │  → 백엔드에 저장
        └─────────────┘
```
