# 11. Plan & Apply Engine Deep-Dive

## 목차

1. [개요](#1-개요)
2. [Context - 실행 엔진의 중심](#2-context---실행-엔진의-중심)
3. [Plan 흐름 상세](#3-plan-흐름-상세)
4. [Apply 흐름 상세](#4-apply-흐름-상세)
5. [PlanGraphBuilder - Plan 그래프 변환 체인](#5-plangraphbuilder---plan-그래프-변환-체인)
6. [ApplyGraphBuilder - Apply 그래프 변환 체인](#6-applygraphbuilder---apply-그래프-변환-체인)
7. [ReferenceTransformer - 의존성 연결](#7-referencetransformer---의존성-연결)
8. [주요 GraphTransformer 분석](#8-주요-graphtransformer-분석)
9. [Graph Walk와 노드 실행](#9-graph-walk와-노드-실행)
10. [Unknown 값 처리](#10-unknown-값-처리)
11. [Create Before Destroy 처리](#11-create-before-destroy-처리)
12. [설계 철학과 Why](#12-설계-철학과-why)
13. [요약](#13-요약)

---

## 1. 개요

Terraform의 Plan & Apply Engine은 사용자의 설정과 현재 인프라 상태를 비교하여 변경 계획(Plan)을 수립하고, 이를 실제 인프라에 적용(Apply)하는 핵심 실행 엔진이다.

```
terraform plan 명령                  terraform apply 명령
      |                                    |
      v                                    v
  Context.Plan()                     Context.Apply()
      |                                    |
      v                                    v
  PlanGraphBuilder                   ApplyGraphBuilder
      |                                    |
      v                                    v
  GraphTransformer 체인               GraphTransformer 체인
  (20+ 변환기)                       (20+ 변환기)
      |                                    |
      v                                    v
  DAG 그래프 생성                    DAG 그래프 생성
      |                                    |
      v                                    v
  Walker.Walk()                      Walker.Walk()
  (병렬 노드 실행)                   (병렬 노드 실행)
      |                                    |
      v                                    v
  Plan 객체 반환                     State 객체 반환
```

**핵심 소스 파일 위치**:
- `internal/terraform/context.go` - Context 구조체
- `internal/terraform/context_plan.go` - PlanOpts, Plan 관련 메서드
- `internal/terraform/context_apply.go` - ApplyOpts, Apply 관련 메서드
- `internal/terraform/graph_builder_plan.go` - PlanGraphBuilder
- `internal/terraform/graph_builder_apply.go` - ApplyGraphBuilder
- `internal/terraform/transform_reference.go` - ReferenceTransformer

---

## 2. Context - 실행 엔진의 중심

### 2.1 Context 구조체

```go
// internal/terraform/context.go
type Context struct {
    meta    *ContextMeta         // 작업 디렉토리, 환경 등 메타 정보
    plugins *contextPlugins      // Provider/Provisioner 팩토리

    hooks     []Hook             // 이벤트 후크 (UI 업데이트 등)
    sh        *stopHook          // 중지 요청 감지용 후크
    uiInput   UIInput            // 사용자 입력 인터페이스
    graphOpts *ContextGraphOpts  // 그래프 옵션

    l                   sync.Mutex       // 작업 잠금
    parallelSem         Semaphore        // 병렬 처리 세마포어
    providerInputConfig map[string]map[string]cty.Value
    runCond             *sync.Cond       // 실행 상태 조건 변수
    runContext          context.Context  // 실행 컨텍스트
    runContextCancel    context.CancelFunc
}
```

### 2.2 ContextOpts와 NewContext

```go
// internal/terraform/context.go
type ContextOpts struct {
    Meta         *ContextMeta
    Hooks        []Hook
    Parallelism  int                              // 병렬 처리 수
    Providers    map[addrs.Provider]providers.Factory
    Provisioners map[string]provisioners.Factory
    PreloadedProviderSchemas map[addrs.Provider]providers.ProviderSchema
    UIInput      UIInput
}
```

### 2.3 실행 상태 관리

```go
// acquireRun은 실행 잠금을 획득하고 컨텍스트를 설정
func (c *Context) acquireRun(phase string) func() {
    // 1. 다른 작업이 실행 중이면 대기
    // 2. runContext 설정 (취소 가능한 컨텍스트)
    // 3. stopHook 초기화
    // 4. 해제 함수 반환
}

// Stop은 현재 실행 중인 작업을 중지 요청
func (c *Context) Stop() {
    // runContextCancel() 호출 → 실행 중인 Walker에 전파
}
```

**동시 실행 방지**:

```
Context는 한 번에 하나의 작업만 실행:

  Plan 실행 중  → Apply 요청 → 대기 (runCond.Wait)
  Plan 완료     → Apply 시작  (runCond.Broadcast)

이 패턴은 Go의 sync.Cond를 사용하여 구현.
```

---

## 3. Plan 흐름 상세

### 3.1 PlanOpts 구조체

```go
// internal/terraform/context_plan.go
type PlanOpts struct {
    Mode             plans.Mode         // Normal, Destroy, RefreshOnly
    SkipRefresh      bool               // Refresh 건너뛰기
    PreDestroyRefresh bool              // Destroy 전 Refresh
    SetVariables     InputValues         // 루트 변수 값
    Targets          []addrs.Targetable  // 대상 리소스 제한
    ActionTargets    []addrs.Targetable  // 액션 대상
    ForceReplace     []addrs.AbsResourceInstance  // 강제 교체
    DeferralAllowed  bool               // 지연 변경 허용
    ExternalReferences []*addrs.Reference  // 외부 참조
    Overrides        *mocking.Overrides   // 테스트 오버라이드
    ImportTargets    []*ImportTarget      // import 대상
    GenerateConfigPath string            // 설정 자동 생성 경로
}
```

### 3.2 plans.Mode

```
Plan 모드:

Normal (기본):
  - Refresh → Diff 계산 → Plan 생성
  - 설정과 상태의 차이를 반영

Destroy:
  - 모든 리소스의 파괴 계획
  - 의존성 역순으로 파괴

RefreshOnly:
  - Refresh만 수행, 변경 계획 없음
  - 상태를 실제 인프라와 동기화만
```

### 3.3 Plan 실행 단계

```
Context.Plan(config, state, opts)
  |
  |  1. acquireRun("plan") - 실행 잠금 획득
  |
  |  2. Refactoring 처리
  |     - moved 블록 해석
  |     - removed 블록 해석
  |
  |  3. PlanGraphBuilder 구성
  |     - Config, State, Plugins 설정
  |     - Operation 모드 설정
  |
  |  4. GraphBuilder.Build()
  |     - 20+ GraphTransformer 순차 적용
  |     - DAG 구축
  |     - 유효성 검증
  |
  |  5. Walker.Walk()
  |     - 위상 정렬 순서로 노드 실행
  |     - 병렬 처리 (의존성 충족된 노드)
  |     - 각 노드: Refresh → Plan 계산
  |
  |  6. Plan 객체 조립
  |     - 변경 목록, 상태 스냅샷, 메타데이터
  |
  v  Plan 반환
```

---

## 4. Apply 흐름 상세

### 4.1 ApplyOpts 구조체

```go
// internal/terraform/context_apply.go
type ApplyOpts struct {
    ExternalProviders map[addrs.RootProviderConfig]providers.Interface
    SetVariables      InputValues
    AllowRootEphemeralOutputs bool
}
```

### 4.2 Apply 실행

```go
// internal/terraform/context_apply.go
func (c *Context) Apply(plan *plans.Plan, config *configs.Config,
    opts *ApplyOpts) (*states.State, tfdiags.Diagnostics) {
    state, _, diags := c.ApplyAndEval(plan, config, opts)
    return state, diags
}

func (c *Context) ApplyAndEval(plan *plans.Plan, config *configs.Config,
    opts *ApplyOpts) (*states.State, *lang.Scope, tfdiags.Diagnostics) {

    defer c.acquireRun("apply")()

    // 1. Plan 검증
    // 2. ApplyGraphBuilder 구성
    // 3. Graph 빌드
    // 4. Walker.Walk()
    // 5. State 반환
}
```

### 4.3 Plan vs Apply 차이

```
                    Plan                        Apply
+-----------+-------------------------------+---------------------------+
| 입력      | Config + State + PlanOpts     | Plan + Config + ApplyOpts |
| 그래프 빌더| PlanGraphBuilder              | ApplyGraphBuilder         |
| 노드 타입 | nodeExpandPlannableResource   | NodeApplyableResourceInst |
| Provider  | PlanResourceChange 호출       | ApplyResourceChange 호출  |
| 출력      | Plan 객체 (변경 목록)         | 업데이트된 State           |
| Refresh   | ReadResource 호출             | 이미 Plan에서 완료        |
| Unknown   | 생성 가능 (computed 속성)      | 허용되지 않음 (실제 값)   |
+-----------+-------------------------------+---------------------------+
```

---

## 5. PlanGraphBuilder - Plan 그래프 변환 체인

### 5.1 PlanGraphBuilder 구조체

```go
// internal/terraform/graph_builder_plan.go
type PlanGraphBuilder struct {
    Config               *configs.Config
    State                *states.State
    RootVariableValues   InputValues
    ExternalProviderConfigs map[addrs.RootProviderConfig]providers.Interface
    Plugins              *contextPlugins
    Targets              []addrs.Targetable
    ForceReplace         []addrs.AbsResourceInstance
    skipRefresh          bool
    skipPlanChanges      bool
    Operation            walkOperation
    ExternalReferences   []*addrs.Reference
    Overrides            *mocking.Overrides
    ImportTargets        []*ImportTarget
    ...
}
```

### 5.2 Steps() - 변환 체인 전체

```go
// internal/terraform/graph_builder_plan.go
func (b *PlanGraphBuilder) Steps() []GraphTransformer {
    return []GraphTransformer{
        // 1. 리소스 노드 생성
        &ConfigTransformer{Concrete: b.ConcreteResource, Config: b.Config},

        // 2. Action 노드
        &ActionTriggerConfigTransformer{...},
        &ActionInvokePlanTransformer{...},

        // 3. 동적 값 노드
        &RootVariableTransformer{Config: b.Config, RawValues: b.RootVariableValues},
        &ModuleVariableTransformer{Config: b.Config},
        &variableValidationTransformer{},
        &LocalTransformer{Config: b.Config},
        &OutputTransformer{Config: b.Config},

        // 4. Check 블록
        &checkTransformer{Config: b.Config},

        // 5. Orphan (설정에서 제거된 리소스)
        &OrphanResourceInstanceTransformer{State: b.State, Config: b.Config},

        // 6. Deposed 인스턴스
        &StateTransformer{State: b.State},

        // 7. 상태 첨부
        &AttachStateTransformer{State: b.State},

        // 8. Orphan 출력
        &OrphanOutputTransformer{Config: b.Config, State: b.State},

        // 9. 리소스 설정 첨부
        &AttachResourceConfigTransformer{Config: b.Config},

        // 10. Provider 추가
        transformProviders(b.ConcreteProvider, b.Config, b.ExternalProviderConfigs),

        // 11. 제거된 모듈
        &RemovedModuleTransformer{Config: b.Config, State: b.State},

        // 12. 스키마 첨부
        &AttachSchemaTransformer{Plugins: b.Plugins, Config: b.Config},

        // 13. 모듈 확장
        &ModuleExpansionTransformer{Config: b.Config},

        // 14. 외부 참조
        &ExternalReferenceTransformer{ExternalReferences: b.ExternalReferences},

        // 15. 참조 기반 의존성 연결
        &ReferenceTransformer{},
        &AttachDependenciesTransformer{},

        // 16. 데이터 소스 depends_on
        &attachDataResourceDependsOnTransformer{},

        // 17. 파괴 순서
        &DestroyEdgeTransformer{Operation: b.Operation},

        // 18. 미사용 노드 제거 (Destroy 모드)
        &pruneUnusedNodesTransformer{skip: b.Operation != walkPlanDestroy},

        // 19. 대상 필터링
        &TargetsTransformer{Targets: b.Targets},

        // 20. 쿼리 필터
        &QueryTransformer{queryPlan: b.queryPlan},

        // 21. Create Before Destroy 감지
        &ForcedCBDTransformer{},

        // 22. Ephemeral 리소스 종료
        &ephemeralResourceCloseTransformer{},

        // 23. Provider 종료
        &CloseProviderTransformer{},

        // 24. 루트 모듈 종료
        &CloseRootModuleTransformer{},

        // 25. 전이적 축소
        &TransitiveReductionTransformer{},
    }
}
```

### 5.3 Operation별 노드 팩토리

```go
// internal/terraform/graph_builder_plan.go
func (b *PlanGraphBuilder) initPlan() {
    b.ConcreteResource = func(a *NodeAbstractResource) dag.Vertex {
        return &nodeExpandPlannableResource{
            NodeAbstractResource: a,
            skipRefresh:          b.skipRefresh,
            skipPlanChanges:      b.skipPlanChanges,
            forceReplace:         b.ForceReplace,
        }
    }

    b.ConcreteResourceOrphan = func(a *NodeAbstractResourceInstance) dag.Vertex {
        return &NodePlannableResourceInstanceOrphan{...}
    }
}

func (b *PlanGraphBuilder) initDestroy() {
    b.initPlan()  // Plan 기반에 추가
    b.ConcreteResourceInstance = func(a *NodeAbstractResourceInstance) dag.Vertex {
        return &NodePlanDestroyableResourceInstance{...}
    }
}

func (b *PlanGraphBuilder) initValidate() {
    b.ConcreteResource = func(a *NodeAbstractResource) dag.Vertex {
        return &NodeValidatableResource{...}
    }
}
```

---

## 6. ApplyGraphBuilder - Apply 그래프 변환 체인

### 6.1 ApplyGraphBuilder Steps

```go
// internal/terraform/graph_builder_apply.go
func (b *ApplyGraphBuilder) Steps() []GraphTransformer {
    return []GraphTransformer{
        // 1. 리소스 메타데이터 (설정에서)
        &ConfigTransformer{Concrete: concreteResource, Config: b.Config},

        // 2. 동적 값
        &RootVariableTransformer{...},
        &ModuleVariableTransformer{...},
        &variableValidationTransformer{},
        &LocalTransformer{...},
        &OutputTransformer{...},

        // 3. Plan의 변경 사항에서 리소스 인스턴스 노드 생성
        &DiffTransformer{
            Concrete: concreteResourceInstance,
            State:    b.State,
            Changes:  b.Changes,      // Plan에서 계산된 변경 목록
            Config:   b.Config,
        },

        // 4. Action 관련
        &ActionTriggerConfigTransformer{...},
        &ActionInvokeApplyTransformer{...},
        &ActionDiffTransformer{...},

        // 5. Deferred 변경
        &DeferredTransformer{DeferredChanges: b.DeferredChanges},

        // 6. Check 블록
        &checkTransformer{Config: b.Config},

        // 7. 상태 첨부
        &AttachStateTransformer{State: b.State},

        // 8. Orphan 출력
        &OrphanOutputTransformer{Config: b.Config, State: b.State},

        // 9. 설정 첨부
        &AttachResourceConfigTransformer{Config: b.Config},

        // 10. Provider
        transformProviders(concreteProvider, b.Config, b.ExternalProviderConfigs),

        // 11. 제거된 모듈
        &RemovedModuleTransformer{...},

        // 12. 스키마 첨부
        &AttachSchemaTransformer{Plugins: b.Plugins},

        // 13. 모듈 확장
        &ModuleExpansionTransformer{Config: b.Config},

        // 14. 참조 기반 의존성
        &ReferenceTransformer{},
        &AttachDependenciesTransformer{},

        // 15. Check 시작 순서
        &checkStartTransformer{Config: b.Config},

        // 16. Create Before Destroy
        &ForcedCBDTransformer{},

        // 17. 파괴 순서
        &DestroyEdgeTransformer{Changes: b.Changes, Operation: b.Operation},
        &CBDEdgeTransformer{Config: b.Config, State: b.State},

        // 18. Destroy 모드 미사용 노드 제거
        &pruneUnusedNodesTransformer{skip: b.Operation != walkDestroy},

        // 19. 대상 필터링
        &TargetsTransformer{Targets: b.Targets},

        // 20. Ephemeral 리소스 종료
        &ephemeralResourceCloseTransformer{},

        // 21. Provider 종료
        &CloseProviderTransformer{},

        // 22. 루트 모듈 종료
        &CloseRootModuleTransformer{},

        // 23. 전이적 축소
        &TransitiveReductionTransformer{},
    }
}
```

### 6.2 Plan vs Apply GraphBuilder 비교

```
+----------------------------+----------------------------+
|     PlanGraphBuilder       |    ApplyGraphBuilder       |
+----------------------------+----------------------------+
| ConfigTransformer          | ConfigTransformer          |
| (리소스 설정에서 노드)      | (리소스 메타데이터용)       |
+----------------------------+----------------------------+
| OrphanResourceInstance     |                            |
| Transformer                | DiffTransformer            |
| (상태에 있지만 설정에 없는)  | (Plan 변경 목록에서 노드)   |
+----------------------------+----------------------------+
| StateTransformer           | DeferredTransformer        |
| (Deposed 인스턴스)          | (지연된 변경)               |
+----------------------------+----------------------------+
| ForcedCBDTransformer       | ForcedCBDTransformer       |
| (CBD 감지)                 | (CBD 감지)                  |
+----------------------------+----------------------------+
|                            | CBDEdgeTransformer         |
|                            | (CBD 에지 추가)             |
+----------------------------+----------------------------+
| TargetsTransformer         | TargetsTransformer         |
| TransitiveReduction        | TransitiveReduction        |
+----------------------------+----------------------------+
```

---

## 7. ReferenceTransformer - 의존성 연결

### 7.1 핵심 인터페이스

```go
// internal/terraform/transform_reference.go

// 참조될 수 있는 노드
type GraphNodeReferenceable interface {
    GraphNodeModulePath
    ReferenceableAddrs() []addrs.Referenceable
}

// 다른 노드를 참조하는 노드
type GraphNodeReferencer interface {
    GraphNodeModulePath
    References() []*addrs.Reference
}

// depends_on을 노출하는 노드
type graphNodeDependsOn interface {
    GraphNodeReferencer
    DependsOn() []*addrs.Reference
}
```

### 7.2 ReferenceTransformer 동작

```go
// internal/terraform/transform_reference.go
func (t *ReferenceTransformer) Transform(g *Graph) error {
    // 1. 모든 정점에서 ReferenceMap 구축
    vs := g.Vertices()
    m := NewReferenceMap(vs)

    // 2. 참조하는 노드와 참조되는 노드를 연결
    for _, v := range vs {
        if _, ok := v.(GraphNodeDestroyer); ok {
            continue  // 파괴 노드는 건너뛰기
        }

        refs, _ := v.(GraphNodeReferencer)
        if refs == nil {
            continue
        }

        // 이 노드의 참조들에 대해 대상 노드를 찾아 에지 연결
        for _, ref := range refs.References() {
            targets := m.References(v, ref)
            for _, target := range targets {
                g.Connect(dag.BasicEdge(v, target))
                // v가 target에 의존 → target이 먼저 실행
            }
        }
    }
    return nil
}
```

### 7.3 참조 해석 예시

```
HCL 설정:

resource "aws_vpc" "main" {
    cidr_block = "10.0.0.0/16"
}

resource "aws_subnet" "web" {
    vpc_id = aws_vpc.main.id     ← 참조!
}

그래프 변환:

1. ConfigTransformer:
   [aws_vpc.main] [aws_subnet.web]  (독립적 노드)

2. ReferenceTransformer:
   aws_subnet.web의 References() → [aws_vpc.main]
   aws_vpc.main의 ReferenceableAddrs() → [aws_vpc.main]

   에지 추가:
   aws_subnet.web ──depends_on──→ aws_vpc.main

3. 실행 순서:
   aws_vpc.main 먼저 → aws_subnet.web 다음
```

### 7.4 ReferenceOutside - 모듈 경계 참조

```go
// 모듈 입력 변수의 경우:
// 표현식은 호출 모듈에서 평가되지만, 참조는 자신의 모듈에서
type GraphNodeReferenceOutside interface {
    ReferenceOutside() (selfPath, referencePath addrs.Module)
}
```

```
예시:

module "vpc" {
    source = "./vpc"
    cidr   = var.network_cidr   ← 루트 모듈의 변수 참조
}

module "vpc" 내 variable "cidr":
  selfPath = ["vpc"]       ← 자신은 vpc 모듈에 속함
  referencePath = []       ← 참조는 루트 모듈에서 해석
```

---

## 8. 주요 GraphTransformer 분석

### 8.1 ConfigTransformer

```
역할: 설정 파일의 리소스를 그래프 노드로 변환

Config의 각 리소스에 대해:
  concrete(NodeAbstractResource) → dag.Vertex

Plan:  nodeExpandPlannableResource
Apply: nodeExpandApplyableResource
```

### 8.2 DiffTransformer (Apply 전용)

```
역할: Plan 결과의 변경 목록에서 리소스 인스턴스 노드 생성

Plan.Changes의 각 변경에 대해:
  concrete(NodeAbstractResourceInstance) → dag.Vertex

변경 종류별 동작:
  Create: 새 노드 생성, Apply 시 리소스 생성
  Update: 기존 노드, Apply 시 리소스 업데이트
  Delete: 파괴 노드, Apply 시 리소스 삭제
  Replace: CBD 여부에 따라 생성+파괴 노드
```

### 8.3 AttachSchemaTransformer

```
역할: 각 리소스 노드에 Provider 스키마를 첨부

왜 필요한가?
  - ReferenceTransformer가 참조를 분석하려면 스키마 필요
  - 설정 Body를 디코딩하려면 Provider 스키마의 ImpliedType 필요
  - Plan/Apply 시 속성 비교에 스키마 정보 필요

순서가 중요:
  AttachSchemaTransformer → 반드시 ReferenceTransformer보다 먼저
```

### 8.4 ModuleExpansionTransformer

```
역할: 모듈 호출의 count/for_each를 처리하여 확장 노드 생성

module "vpc" { count = 3 }
  → nodeExpandModule["vpc"] 노드 생성
  → Graph Walk 중 DynamicExpand로 3개 인스턴스 확장

DynamicExpand:
  nodeExpandModule["vpc"]
    → module.vpc[0] (하위 그래프)
    → module.vpc[1] (하위 그래프)
    → module.vpc[2] (하위 그래프)
```

### 8.5 DestroyEdgeTransformer

```
역할: 파괴 노드 간 올바른 순서 보장

일반 의존성:        A → B    (A가 B에 의존)
파괴 시 순서:       B → A    (A를 먼저 파괴, B를 나중에)

이유: A가 B에 의존하면, A가 살아있는 상태에서 B를 파괴하면
      A가 참조 에러를 발생시킬 수 있음.
      따라서 A를 먼저 파괴한 후 B를 파괴해야 함.
```

### 8.6 TargetsTransformer

```
역할: -target 옵션으로 지정된 리소스와 그 의존성만 남기고 나머지 제거

terraform plan -target=aws_instance.web
  → aws_instance.web와 그 의존 리소스(VPC, Subnet 등)만 그래프에 유지
  → 나머지 리소스 노드는 그래프에서 제거

주의: Targets는 예외적 사용을 위한 것이며, 일상적 사용은 권장하지 않음
```

### 8.7 TransitiveReductionTransformer

```
역할: 그래프에서 불필요한 간접 에지 제거

전:    A → B, B → C, A → C
                      ^^^^^ A→B→C 경로가 있으므로 불필요
후:    A → B, B → C

효과: 그래프가 더 이해하기 쉬워지고, 디버깅 시 시각화 개선
성능: 실행 순서에는 영향 없음 (위상 정렬 결과 동일)
```

---

## 9. Graph Walk와 노드 실행

### 9.1 Build → Walk 흐름

```go
// internal/terraform/graph_builder_plan.go
func (b *PlanGraphBuilder) Build(path addrs.ModuleInstance) (*Graph, tfdiags.Diagnostics) {
    return (&BasicGraphBuilder{
        Steps:               b.Steps(),
        Name:                "PlanGraphBuilder",
        SkipGraphValidation: b.SkipGraphValidation,
    }).Build(path)
}
```

```
BasicGraphBuilder.Build():
  for each step in Steps:
    step.Transform(graph)  // 그래프 변환

  graph.Validate()         // 순환 감지, 유효성 검증

  return graph
```

### 9.2 Walker 병렬 실행

```
Graph Walk 중 노드 실행:

  Walker.Walk(graph)
    |
    +-- 위상 정렬
    |
    +-- 각 노드에 대해:
    |     의존성이 모두 완료되었는가?
    |       예 → goroutine으로 실행
    |       아니오 → 대기
    |
    +-- 모든 노드 완료까지 반복

병렬 실행 예시:

  시간 →

  goroutine 1: [aws_vpc.main]──────────────────────────[완료]
  goroutine 2:                [aws_subnet.a]──────[완료]
  goroutine 3:                [aws_subnet.b]──────[완료]
  goroutine 4:                                    [aws_instance.web]──[완료]

  vpc 완료 후 → subnet a, b 병렬 시작
  subnet 완료 후 → instance 시작
```

### 9.3 Parallelism 세마포어

```go
// Context의 parallelSem이 동시 실행 수 제한
// terraform apply -parallelism=5
//   → 최대 5개 노드 동시 실행

// 세마포어 동작:
sem.Acquire()   // 슬롯 획득 (가용 없으면 대기)
doWork()        // 노드 실행 (Provider RPC 등)
sem.Release()   // 슬롯 반환
```

---

## 10. Unknown 값 처리

### 10.1 Unknown이란?

```
Plan 시점에 아직 알 수 없는 값:

resource "aws_instance" "web" {
    ami           = "ami-123"        ← 알려진 값
    instance_type = "t2.micro"       ← 알려진 값
    # id, public_ip 등은 생성 후에야 알 수 있음
}

Plan 출력:
  + aws_instance.web
      ami           = "ami-123"
      instance_type = "t2.micro"
      id            = (known after apply)    ← Unknown!
      public_ip     = (known after apply)    ← Unknown!
```

### 10.2 Unknown 값 전파

```
resource "aws_instance" "web" { ... }

resource "aws_security_group_rule" "allow" {
    source_security_group_id = aws_instance.web.vpc_security_group_ids[0]
                               ^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^
                               Plan 시점에 Unknown
}

전파:
  aws_instance.web.id = Unknown
  → aws_security_group_rule.allow 전체 설정에 Unknown 포함
  → Plan 결과에서 해당 속성도 (known after apply)

Apply 시점:
  aws_instance.web 생성 → id = "i-abc123"
  → aws_security_group_rule.allow에 실제 값 전달
  → ApplyResourceChange 호출
```

### 10.3 Unknown과 Plan 정확도

```
Unknown 값이 Plan의 정확도에 미치는 영향:

정확한 Plan:
  resource "aws_instance" "web" {
      ami           = "ami-123"       ← 정적 값
      instance_type = "t2.micro"      ← 정적 값
  }
  → Plan에서 정확한 변경 예측 가능

불확실한 Plan:
  resource "aws_instance" "web" {
      ami           = data.aws_ami.latest.id   ← data source 조회 필요
      instance_type = var.size                  ← 변수 값에 의존
      count         = length(var.zones)         ← Unknown이면 Plan 불가
  }

count/for_each가 Unknown인 경우:
  - DeferralAllowed = true: 해당 리소스 Plan 지연
  - DeferralAllowed = false: 에러 발생
```

---

## 11. Create Before Destroy 처리

### 11.1 ForcedCBDTransformer

```
역할: 의존성 에지에 의한 순환을 방지하기 위해 CBD를 강제

문제 시나리오:
  A depends_on B (일반)
  B를 교체할 때:
    destroy B → create B'

  일반 순서: destroy B → create B'
  하지만 A가 B에 의존하므로:
    A의 update → B'의 create → B의 destroy → A의 update ???
    → 순환!

해결:
  B에 create_before_destroy를 강제 적용
  → create B' → update A → destroy B
  → 순환 없음
```

### 11.2 CBDEdgeTransformer (Apply 전용)

```
역할: CBD 리소스의 파괴 노드에 적절한 에지 추가

CBD 교체 시 그래프:

  create B' ──→ update A ──→ destroy B
  (새 리소스)   (A 업데이트)  (이전 리소스)

에지 규칙:
  - create B'는 destroy B보다 먼저
  - A는 B'가 준비된 후 업데이트
  - destroy B는 A 업데이트 후 실행
```

### 11.3 CBD 전체 흐름

```
시간 →

Non-CBD (기본):
  1. [destroy old] → 2. [create new]
  서비스 중단 구간: ====|===========|

CBD:
  1. [create new] → 2. [update deps] → 3. [destroy old]
  서비스 중단 구간: 없음 (새것이 먼저 준비)

상태 변화:
  1. Current → Deposed (기존 객체를 Depose)
  2. null → Current (새 객체를 Current에)
  3. Deposed → 삭제 (이전 객체 파괴)
```

---

## 12. 설계 철학과 Why

### 12.1 왜 Plan과 Apply를 분리하는가?

```
Plan-Apply 분리의 장점:

1. 안전성:
   - Plan으로 변경 내용을 먼저 확인
   - 승인 후에만 Apply
   - CI/CD에서 리뷰 게이트 가능

2. 재현성:
   - Plan 결과를 파일로 저장 (-out 옵션)
   - 동일한 Plan을 나중에 Apply
   - Plan과 Apply 사이에 인프라 변경 감지

3. 팀 워크플로:
   - 개발자가 Plan
   - 관리자가 리뷰 후 Apply
   - Plan 결과를 코드 리뷰에 첨부 가능
```

### 12.2 왜 GraphTransformer 체인인가?

```
단일 그래프 빌더 vs 변환 체인:

단일 빌더:
  - 모든 로직이 하나의 거대 함수
  - 순서 변경이 어려움
  - 테스트하기 어려움

변환 체인 (Terraform 방식):
  - 각 변환기가 독립적 관심사 담당
  - 순서 변경이 용이 (배열 재배치)
  - 개별 변환기 단위 테스트 가능
  - 새 기능 추가 시 새 변환기만 삽입

변환기 간 암묵적 규칙:
  AttachSchemaTransformer → ReferenceTransformer 순서 필수
  (스키마 없이는 참조 분석 불가)
```

### 12.3 왜 Plan에서 Unknown 값을 허용하는가?

```
대안: Plan 시 모든 값을 확정
  - 불가능: Computed 속성 (id, arn 등)은 생성 후에야 알 수 있음
  - AWS EC2 인스턴스의 public_ip는 EC2 API가 할당
  - RDS 엔드포인트는 프로비저닝 완료 후 결정

Unknown 허용의 설계 트레이드오프:
  장점: 현실적으로 가능한 수준의 Plan 제공
  단점: Plan이 100% 정확하지 않을 수 있음
        (Unknown 값에 의존하는 다른 리소스의 변경이 불확실)

Terraform의 접근:
  "최선의 노력(best effort)" Plan
  - 알 수 있는 것은 정확하게
  - 알 수 없는 것은 Unknown으로 명시
  - 사용자가 판단하도록
```

### 12.4 왜 Plan과 Apply의 GraphBuilder가 다른가?

```
PlanGraphBuilder:
  - 설정과 상태를 비교하여 변경 감지
  - Refresh (ReadResource) 수행
  - Orphan 리소스 감지
  - 결과: 무엇을 변경할지 (Plan 객체)

ApplyGraphBuilder:
  - Plan의 변경 목록을 실행
  - DiffTransformer로 변경별 노드 생성
  - CBD 에지 추가 (CBDEdgeTransformer)
  - 결과: 변경된 상태 (State 객체)

분리 이유:
  Plan은 "무엇을" 결정, Apply는 "어떻게" 실행
  각각 다른 노드 타입, 다른 변환 체인 필요
  - Plan 전용: OrphanResourceInstanceTransformer, StateTransformer
  - Apply 전용: DiffTransformer, DeferredTransformer, CBDEdgeTransformer
```

### 12.5 왜 Context는 단일 실행만 허용하는가?

```
Context.acquireRun() 패턴:

동시 Plan+Apply가 위험한 이유:
  1. 공유 상태(State)에 대한 동시 접근
  2. Provider 연결이 Plan/Apply 간 다를 수 있음
  3. 같은 리소스에 대한 동시 변경 → 예측 불가

해결:
  sync.Mutex로 한 번에 하나의 작업만 실행
  runCond로 완료 대기
  Stop()으로 실행 중인 작업 취소 가능
```

---

## 13. 요약

### Plan & Apply 전체 아키텍처

```
terraform plan                         terraform apply
     |                                      |
     v                                      v
+----------+                         +----------+
| PlanOpts |                         | ApplyOpts|
+----+-----+                         +-----+----+
     |                                      |
     v                                      v
+----+---------+                     +------+--------+
|  Context     |                     |   Context     |
|  .Plan()     |                     |   .Apply()    |
+----+---------+                     +------+--------+
     |                                      |
     v                                      v
+----+-----------+                   +------+----------+
| PlanGraph      |                   | ApplyGraph      |
| Builder        |                   | Builder         |
| 25 Transformers|                   | 23 Transformers |
+----+-----------+                   +------+----------+
     |                                      |
     v                                      v
+----+-----------+                   +------+----------+
| DAG Graph      |                   | DAG Graph       |
| (validated)    |                   | (validated)      |
+----+-----------+                   +------+----------+
     |                                      |
     v                                      v
+----+-----------+                   +------+----------+
| Walker.Walk()  |                   | Walker.Walk()   |
| (parallel)     |                   | (parallel)      |
+----+-----------+                   +------+----------+
     |                                      |
     v                                      v
  Plan 객체                           업데이트된 State
```

### 핵심 설계 결정 요약

| 결정 | 이유 |
|------|------|
| Plan/Apply 분리 | 안전성, 재현성, 팀 워크플로 |
| GraphTransformer 체인 | 모듈성, 테스트 용이, 확장성 |
| Unknown 값 허용 | Computed 속성의 현실적 처리 |
| Plan/Apply 별도 GraphBuilder | 다른 관심사, 다른 노드 타입 |
| Context 단일 실행 | 상태 동시성 안전 |
| Parallelism 세마포어 | 제어된 병렬 처리 |
| ForcedCBD | 의존성 순환 자동 해소 |
| TransitiveReduction | 그래프 가독성 향상 |
| ReferenceTransformer | 자동 의존성 감지 (명시적 depends_on 불필요) |
