# 05. Terraform 핵심 컴포넌트

## 1. DAG 그래프 엔진

### 1.1 기본 그래프 구조

```go
// internal/dag/graph.go
type Graph struct {
    vertices  Set                         // 모든 정점
    edges     Set                         // 모든 간선
    downEdges map[interface{}]Set         // 정점 → 나가는 간선 (의존하는 것)
    upEdges   map[interface{}]Set         // 정점 → 들어오는 간선 (의존 받는 것)
}
```

```go
// internal/dag/dag.go
type AcyclicGraph struct {
    Graph  // 임베딩
}
```

### 1.2 핵심 연산

**위상 정렬 (Topological Sort)**

DFS 기반으로 의존성 순서를 결정한다:

```
입력 그래프:           위상 정렬 결과:
A → B → D             [A, C, B, E, D]
A → C → D             (A와 C가 먼저, B와 E 다음, D 마지막)
    C → E
```

**병렬 워크 (Parallel Walk)**

```go
// internal/dag/dag.go — Walk 메서드
func (g *AcyclicGraph) Walk(cb WalkFunc) tfdiags.Diagnostics {
    w := &Walker{Callback: cb, Reverse: true}
    w.Update(g)
    return w.Wait()
}
```

Walker는 의존성이 완료된 노드를 고루틴으로 동시 실행한다:

```
시간 →
t0: [A, C] 동시 시작 (의존성 없음)
t1: A 완료 → B ready
t2: C 완료 → E ready
t3: [B, E] 동시 시작
t4: B 완료, E 완료 → D ready
t5: D 시작
t6: D 완료
```

**이행적 축소 (Transitive Reduction)**

불필요한 간접 의존성 간선을 제거한다:

```
축소 전:              축소 후:
A → B                 A → B
A → C                 A → C
A → D  (불필요)       B → D
B → D                 C → D
C → D
```

> **왜 이행적 축소가 필요한가?** — 그래프를 시각화하거나 디버깅할 때 불필요한 간선이 없어야 구조가 명확하다. 실행 순서에는 영향이 없지만, 사용자에게 보여주는 의존성 그래프가 깔끔해진다.

### 1.3 사이클 검출

```go
func (g *AcyclicGraph) Validate() error {
    // DFS로 역방향 간선(back edge) 검출
    // 역방향 간선이 있으면 사이클 존재
}
```

사이클이 있으면 Terraform은 "Error: Cycle"과 함께 관련 리소스를 출력한다.

## 2. 그래프 빌더 & 변환기 (Graph Builder & Transformer)

### 2.1 그래프 빌더 패턴

```go
// internal/terraform/graph_builder.go
type GraphBuilder interface {
    Build(addrs.ModuleInstance) (*Graph, tfdiags.Diagnostics)
}

type BasicGraphBuilder struct {
    Steps []GraphTransformer    // 변환 단계 순서
    Name  string
}
```

### 2.2 Plan 그래프 빌더

`PlanGraphBuilder`는 다음 순서로 변환기를 적용한다:

```
1. ConfigTransformer
   └── Config의 각 리소스/데이터 소스를 그래프 노드로 추가

2. ModuleExpansionTransformer
   └── count/for_each 있는 모듈을 확장 노드로 변환

3. ProviderTransformer
   └── 각 리소스에 프로바이더 노드 연결

4. ReferenceTransformer
   └── 표현식의 참조를 분석하여 의존성 간선 추가
   └── 예: aws_instance.web의 subnet_id = aws_subnet.main.id
         → aws_instance.web → aws_subnet.main 간선

5. AttachStateTransformer
   └── 기존 State 정보를 노드에 첨부

6. TargetsTransformer
   └── -target 옵션에 해당하는 노드만 남기기

7. CloseProviderTransformer
   └── 프로바이더 종료 노드 추가

8. TransitiveReductionTransformer
   └── 불필요한 간접 의존성 제거

9. PruneUnusedValuesTransformer
   └── 사용되지 않는 변수/로컬 제거
```

### 2.3 Apply 그래프 빌더

Apply 그래프는 Plan의 Changes를 기반으로 구축한다:

```
Plan Changes                      Apply 그래프
┌─────────────────────┐          ┌─────────────────┐
│ aws_vpc.main: Create│   ─────▶ │ Create vpc.main │
│ aws_subnet.a: Create│   ─────▶ │ Create subnet.a │───depends──▶ vpc.main
│ aws_instance: Update│   ─────▶ │ Update instance │───depends──▶ subnet.a
│ aws_sg.old: Delete  │   ─────▶ │ Delete sg.old   │
└─────────────────────┘          └─────────────────┘

※ Delete 노드는 역방향 의존성 (의존하는 것이 먼저 삭제)
```

### 2.4 ReferenceTransformer 상세

HCL 표현식에서 참조를 추출하여 의존성 간선을 만든다:

```hcl
resource "aws_instance" "web" {
  subnet_id     = aws_subnet.main.id        # → aws_subnet.main 의존
  vpc_security_group_ids = [aws_sg.web.id]   # → aws_sg.web 의존
  ami           = data.aws_ami.latest.id     # → data.aws_ami.latest 의존
  tags = {
    Name = var.instance_name                  # → var.instance_name 의존
  }
}
```

결과 의존성 간선:
```
aws_instance.web → aws_subnet.main
aws_instance.web → aws_sg.web
aws_instance.web → data.aws_ami.latest
aws_instance.web → var.instance_name
```

## 3. terraform.Context

### 3.1 Context 생성 및 초기화

```go
// internal/terraform/context.go
func NewContext(opts *ContextOpts) (*Context, tfdiags.Diagnostics) {
    // opts에서:
    // - Providers: map[addrs.Provider]providers.Factory
    // - Provisioners: map[string]provisioners.Factory
    // - Hooks: []Hook
    // - Parallelism: int (기본 10)
}
```

### 3.2 Plan 메서드

```
Context.Plan(config, state, opts)
    │
    ├── 1. 프로바이더 스키마 로드
    │      └── 각 프로바이더의 GetProviderSchema() 호출
    │
    ├── 2. 설정 유효성 검사
    │      └── 변수 타입, 필수 값 확인
    │
    ├── 3. Refresh (opts.SkipRefresh가 아닌 경우)
    │      └── 모든 리소스의 현재 원격 상태 읽기
    │      └── State와 비교하여 드리프트 감지
    │
    ├── 4. Plan 그래프 구축
    │      └── PlanGraphBuilder.Build()
    │
    ├── 5. 그래프 워크
    │      └── walk(graph, walkPlan)
    │      └── 각 노드에서 PlanResourceChange() 호출
    │
    └── 6. Plan 객체 반환
           └── Changes, DriftedResources, VariableValues 포함
```

### 3.3 Apply 메서드

```
Context.Apply(plan, config, opts)
    │
    ├── 1. Plan 유효성 검증
    │
    ├── 2. Apply 그래프 구축
    │      └── ApplyGraphBuilder.Build()
    │
    ├── 3. 그래프 워크
    │      └── walk(graph, walkApply)
    │      └── 각 노드에서 ApplyResourceChange() 호출
    │      └── State 실시간 갱신
    │
    └── 4. 최종 State 반환
```

### 3.4 동시성 제어

```go
type Context struct {
    parallelSem Semaphore    // 세마포어 (기본 10)
}

// 세마포어로 동시 실행 고루틴 수 제한
// terraform plan -parallelism=20 으로 조정 가능
```

```
세마포어 = 10인 경우:

고루틴 1: [████████░░] vpc 생성
고루틴 2: [████░░░░░░] subnet 생성
고루틴 3: [██████░░░░] sg 생성
...
고루틴 10: [██░░░░░░░░] 리소스 N
고루틴 11: [대기 중...] ← 세마포어 full, 슬롯 기다림
```

## 4. EvalContext

### 4.1 인터페이스

```go
// internal/terraform/eval_context.go
type EvalContext interface {
    // 프로바이더 관리
    InitProvider(addr addrs.AbsProviderConfig) (providers.Interface, error)
    Provider(addr addrs.AbsProviderConfig) providers.Interface
    ConfigureProvider(addr addrs.AbsProviderConfig, cfg cty.Value) tfdiags.Diagnostics

    // 상태 접근
    State() *states.SyncState
    RefreshState() *states.SyncState

    // 변경 추적
    Changes() *plans.ChangesSync

    // 표현식 평가
    Evaluator() *lang.Evaluator

    // 인스턴스 확장
    InstanceExpander() *instances.Expander

    // 경로
    Path() addrs.ModuleInstance
}
```

### 4.2 ContextGraphWalker

```go
// internal/terraform/graph_walk_context.go
type ContextGraphWalker struct {
    Context          *Context
    State            *states.SyncState       // 변경 가능한 State
    Changes          *plans.ChangesSync      // 변경 추적
    Operation        walkOperation           // plan/apply/validate

    providerCache    map[string]providers.Interface  // 프로바이더 캐시
    providerLock     sync.Mutex

    instanceExpander *instances.Expander     // count/for_each 확장기
}
```

> **왜 ContextGraphWalker가 필요한가?** — 그래프의 각 노드는 프로바이더, State, Changes 등 공유 자원에 접근해야 한다. ContextGraphWalker는 이러한 공유 자원을 스레드 안전하게 관리하면서, 각 노드에 필요한 컨텍스트를 제공한다.

## 5. Provider Plugin System

### 5.1 gRPC Provider 클라이언트

```go
// internal/plugin/grpc_provider.go
type GRPCProvider struct {
    client  tfplugin5.ProviderClient   // gRPC 클라이언트 스텁
    ctx     context.Context
    schema  providers.GetProviderSchemaResponse  // 캐시된 스키마
}
```

### 5.2 Provider Interface 핵심 메서드

```
Provider 생명주기:
┌──────────────┐
│ GetSchema    │ ← 스키마 조회 (리소스 타입, 속성 정의)
└──────┬───────┘
       ▼
┌──────────────┐
│ Configure    │ ← 인증 정보 설정 (API 키, 리전 등)
└──────┬───────┘
       ▼
┌──────────────┐
│ Validate     │ ← 설정 유효성 검증
└──────┬───────┘
       ▼
┌──────────────┐     ┌──────────────┐
│ Plan         │────▶│ Apply        │ ← 리소스 CRUD
└──────────────┘     └──────────────┘
       │
       ▼
┌──────────────┐
│ Read         │ ← Refresh 시 원격 상태 읽기
└──────────────┘
       │
       ▼
┌──────────────┐
│ Import       │ ← 기존 리소스 가져오기
└──────────────┘
       │
       ▼
┌──────────────┐
│ Close        │ ← gRPC 연결 종료, 프로세스 정리
└──────────────┘
```

### 5.3 Provider 프로토콜 버전

| 프로토콜 | Protobuf | 특징 |
|---------|----------|------|
| v5 | `tfplugin5.proto` | 기존 프로바이더 대부분 |
| v6 | `tfplugin6.proto` | 새로운 기능 (에페메럴 리소스, 함수, 리스트 등) |

v6에서 추가된 메서드:
- `CallFunction()` — 프로바이더 정의 함수 호출
- `OpenEphemeralResource()` — 일시적 리소스 (시크릿 등)
- `ListResource()` — 리소스 열거

### 5.4 스키마 캐싱

```go
// internal/providers/provider.go
var SchemaCache = &schemaCache{
    schemas: make(map[addrs.Provider]providers.ProviderSchema),
}
```

스키마는 한 번 조회하면 캐시에 저장되어, 동일 프로바이더의 반복 스키마 요청을 방지한다.

## 6. State 관리

### 6.1 State 읽기/쓰기

```
State 생명주기:
┌──────────────┐
│ 백엔드에서   │
│ State 읽기   │
│ (JSON 역직렬화)│
└──────┬───────┘
       ▼
┌──────────────┐
│ SyncState    │ ← RWMutex 보호
│ 래핑         │
└──────┬───────┘
       ▼
┌──────────────┐
│ 그래프 워크  │ ← 여러 고루틴이 동시 접근
│ 중 변경      │    SetResourceInstanceCurrent()
└──────┬───────┘    RemoveResourceInstanceObject()
       ▼
┌──────────────┐
│ State 쓰기   │
│ (JSON 직렬화)│
│ → 백엔드     │
└──────────────┘
```

### 6.2 ObjectStatus

```go
// internal/states/instance_object_src.go
type ObjectStatus byte

const (
    ObjectReady   ObjectStatus = 'R'   // 정상 — 원격과 일치
    ObjectTainted ObjectStatus = 'T'   // 오염됨 — 다음 apply에서 교체
    ObjectPlanned ObjectStatus = 'P'   // 계획됨 — 아직 미적용
)
```

### 6.3 Deposed 인스턴스

`create_before_destroy`를 사용할 때, 새 인스턴스가 먼저 생성되고 기존 인스턴스는 "Deposed" 상태로 전환된다:

```
교체 과정 (create_before_destroy = true):

1단계: Current = 기존 인스턴스
        ↓
2단계: Current = 새 인스턴스 (생성)
       Deposed["abc123"] = 기존 인스턴스 (대기)
        ↓
3단계: Current = 새 인스턴스
       Deposed 삭제 (기존 인스턴스 파괴)
```

## 7. Hook 시스템

### 7.1 Hook 인터페이스

```go
// internal/terraform/hook.go
type Hook interface {
    PreApply(info *InstanceInfo, priorState, plannedNewState cty.Value) (HookAction, error)
    PostApply(info *InstanceInfo, newState cty.Value, err error) (HookAction, error)
    PreDiff(info *InstanceInfo, priorState, proposedNewState cty.Value) (HookAction, error)
    PostDiff(info *InstanceInfo, diff *InstanceDiff) (HookAction, error)
    PreProvisionInstanceStep(info *InstanceInfo, typeName string) (HookAction, error)
    PostProvisionInstanceStep(info *InstanceInfo, typeName string, err error) (HookAction, error)
    PreRefresh(info *InstanceInfo, priorState cty.Value) (HookAction, error)
    PostRefresh(info *InstanceInfo, newState cty.Value) (HookAction, error)
    PreImportState(info *InstanceInfo, importID string) (HookAction, error)
    PostImportState(info *InstanceInfo, imported []InstanceState) (HookAction, error)
    PostStateUpdate(state *states.State) (HookAction, error)
}
```

### 7.2 Hook 동작

```
그래프 워크 중:
    PreApply(aws_instance.web, prior, planned)
        ↓
    provider.ApplyResourceChange()
        ↓
    PostApply(aws_instance.web, newState, err)
        ↓
    [UI 업데이트: "aws_instance.web: Creating..."]
    [UI 업데이트: "aws_instance.web: Creation complete after 45s"]
```

HookAction 종류:
- `HookActionContinue` — 계속 진행
- `HookActionHalt` — 실행 중지

### 7.3 StopHook

`Ctrl+C` 처리를 위한 내부 훅:

```
첫 번째 Ctrl+C → Graceful Stop
    StopHook.PostApply() → HookActionHalt
    현재 실행 중인 리소스 완료 후 중지

두 번째 Ctrl+C → Force Cancel (10초 후)
    프로세스 강제 종료
```

> **소스 위치**: `internal/terraform/hook_stop.go`

## 8. 인스턴스 확장기 (Instance Expander)

### 8.1 역할

`count`와 `for_each`를 처리하여 단일 리소스 정의를 여러 인스턴스로 확장한다.

```go
// internal/instances/expander.go
type Expander struct {
    // 모듈과 리소스의 count/for_each 값 추적
}

// 확장 결과:
// resource "aws_instance" "web" { count = 3 }
//   → aws_instance.web[0]
//   → aws_instance.web[1]
//   → aws_instance.web[2]
```

### 8.2 DynamicExpand 패턴

그래프 노드가 `GraphNodeDynamicExpandable`을 구현하면, 워크 중에 동적으로 서브그래프를 생성한다:

```
nodeExpandModule("server", count=3)
    │
    ├── DynamicExpand() 호출
    │
    └── 서브그래프 생성:
        ├── module.server[0]
        │   ├── aws_instance.web
        │   └── aws_eip.web
        ├── module.server[1]
        │   ├── aws_instance.web
        │   └── aws_eip.web
        └── module.server[2]
            ├── aws_instance.web
            └── aws_eip.web
```

## 9. 진단 시스템 (tfdiags)

### 9.1 Diagnostic 구조

```go
// internal/tfdiags/diagnostic.go
type Diagnostic interface {
    Severity() Severity    // Error 또는 Warning
    Description() Description
    Source() Source         // 소스 코드 위치
}

type Diagnostics []Diagnostic   // 슬라이스
```

### 9.2 에러 누적 패턴

Terraform은 가능한 한 많은 에러를 수집하여 한 번에 보고한다:

```go
var diags tfdiags.Diagnostics

// 에러가 발생해도 즉시 중단하지 않고 누적
diags = diags.Append(validateResource(r))
diags = diags.Append(validateProvider(p))

// 치명적 에러가 있으면 중단
if diags.HasErrors() {
    return diags
}
```

> **왜 이런 패턴인가?** — 사용자가 `terraform validate`를 실행했을 때, 첫 번째 에러에서 멈추는 것보다 모든 에러를 한 번에 보여주는 것이 더 유용하다. 여러 파일의 여러 에러를 한 번에 수정할 수 있기 때문이다.

## 10. Named Values 시스템

### 10.1 역할

변수, 로컬 값, 출력 값을 추적하는 중앙 저장소:

```go
// internal/namedvals/state.go — 개념적 구조
type State struct {
    inputVariables  map[addrs.AbsInputVariableInstance]cty.Value
    localValues     map[addrs.AbsLocalValue]cty.Value
    outputValues    map[addrs.AbsOutputValue]cty.Value
}
```

### 10.2 값 해석 순서

```
1. Input Variables (terraform.tfvars, -var, 환경 변수)
    ↓
2. Local Values (locals { ... })
    ↓ (변수 참조 해석)
3. Resource/Data 표현식 평가
    ↓ (변수, 로컬, 다른 리소스 참조)
4. Output Values (output { ... })
    ↓ (리소스 속성 참조)
5. 모듈 호출자에게 출력값 전달
```
