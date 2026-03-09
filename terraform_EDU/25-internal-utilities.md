# 25. 내부 유틸리티: JSON 출력, Promising, Named Values, Instance Expander 심화

## 목차
1. [개요](#1-개요)
2. [JSON 출력 형식 시스템](#2-json-출력-형식-시스템)
3. [JSON Plan 구조](#3-json-plan-구조)
4. [JSON State & Config](#4-json-state--config)
5. [Promising - 데드락 프리 동시성](#5-promising---데드락-프리-동시성)
6. [Promise 생명주기](#6-promise-생명주기)
7. [Task 시스템](#7-task-시스템)
8. [자기 의존성 감지](#8-자기-의존성-감지)
9. [Once - 단일 실행 보장](#9-once---단일-실행-보장)
10. [Named Values - 값 스토어](#10-named-values---값-스토어)
11. [Placeholder 결과 시스템](#11-placeholder-결과-시스템)
12. [Instance Expander - count/for_each 확장](#12-instance-expander---countfor_each-확장)
13. [Expansion Mode 시스템](#13-expansion-mode-시스템)
14. [왜(Why) 이렇게 설계했나](#14-왜why-이렇게-설계했나)
15. [PoC 매핑](#15-poc-매핑)

---

## 1. 개요

이 문서는 Terraform 내부의 4가지 유틸리티 서브시스템을 다룬다:

| 서브시스템 | 위치 | 역할 |
|-----------|------|------|
| JSON 출력 형식 | `internal/command/json*` | Plan/State를 JSON으로 직렬화 |
| Promising | `internal/promising/` | 데드락-프리 비동기 Promise 패턴 |
| Named Values | `internal/namedvals/` | 변수/로컬/출력 값의 그래프 워크 저장소 |
| Instance Expander | `internal/instances/` | count/for_each 인스턴스 열거 |

---

## 2. JSON 출력 형식 시스템

### 패키지 구조

```
internal/command/
├── jsonplan/        # terraform show -json (plan)
│   ├── plan.go      # plan 구조체, FormatVersion
│   ├── values.go    # 값 직렬화
│   ├── resource.go  # 리소스 변경 직렬화
│   └── module.go    # 모듈 직렬화
├── jsonstate/       # terraform show -json (state)
│   └── state.go     # 상태 JSON 직렬화
├── jsonconfig/      # 설정 JSON 직렬화
│   ├── config.go    # 설정 직렬화
│   └── expression.go # 표현식 직렬화
├── jsonformat/      # 사람이 읽기 좋은 JSON diff 출력
│   ├── plan.go      # Plan 렌더링
│   ├── state.go     # State 렌더링
│   └── renderer.go  # 범용 렌더러
├── jsonfunction/    # 함수 서명 직렬화
│   ├── function.go
│   └── parameter.go
├── jsonprovider/    # 프로바이더 스키마 직렬화
│   ├── provider.go
│   ├── schema.go
│   └── attribute.go
└── jsonchecks/      # check 결과 직렬화
    ├── checks.go
    └── status.go
```

---

## 3. JSON Plan 구조

### `internal/command/jsonplan/plan.go`

```go
const FormatVersion = "1.2"

type plan struct {
    FormatVersion    string      `json:"format_version,omitempty"`
    TerraformVersion string      `json:"terraform_version,omitempty"`
    Variables        variables   `json:"variables,omitempty"`
    PlannedValues    stateValues `json:"planned_values,omitempty"`
    ResourceDrift    []ResourceChange `json:"resource_drift,omitempty"`
    ResourceChanges  []ResourceChange `json:"resource_changes,omitempty"`
    DeferredChanges  []DeferredResourceChange `json:"deferred_changes,omitempty"`
    OutputChanges    map[string]Change `json:"output_changes,omitempty"`
    PriorState       json.RawMessage `json:"prior_state,omitempty"`
    Config           json.RawMessage `json:"configuration,omitempty"`
    RelevantAttributes []ResourceAttr `json:"relevant_attributes,omitempty"`
    Checks           json.RawMessage `json:"checks,omitempty"`
    Timestamp        string      `json:"timestamp,omitempty"`
    Applyable        bool        `json:"applyable"`
    Complete         bool        `json:"complete"`
    Errored          bool        `json:"errored"`
}
```

### 리소스 변경 사유 상수

```go
const (
    ResourceInstanceReplaceBecauseCannotUpdate    = "replace_because_cannot_update"
    ResourceInstanceReplaceBecauseTainted         = "replace_because_tainted"
    ResourceInstanceReplaceByRequest              = "replace_by_request"
    ResourceInstanceReplaceByTriggers             = "replace_by_triggers"
    ResourceInstanceDeleteBecauseNoResourceConfig = "delete_because_no_resource_config"
    ResourceInstanceDeleteBecauseWrongRepetition  = "delete_because_wrong_repetition"
    ResourceInstanceDeleteBecauseCountIndex       = "delete_because_count_index"
    ResourceInstanceDeleteBecauseEachKey          = "delete_because_each_key"
    ResourceInstanceDeleteBecauseNoModule         = "delete_because_no_module"
    ResourceInstanceDeleteBecauseNoMoveTarget     = "delete_because_no_move_target"
    ResourceInstanceReadBecauseConfigUnknown      = "read_because_config_unknown"
    ResourceInstanceReadBecauseDependencyPending  = "read_because_dependency_pending"
)
```

### FormatVersion의 의미

`"1.2"`는 JSON 스키마의 버전이다. Terraform 버전과 독립적이며, JSON 구조가 변경될 때만 증가한다. 이를 통해 소비자(CI/CD 도구, Sentinel 등)가 호환성을 확인할 수 있다.

### Change 구조체

```go
type Change struct {
    Actions         []string        `json:"actions"`
    Before          json.RawMessage `json:"before"`
    After           json.RawMessage `json:"after"`
    AfterUnknown    json.RawMessage `json:"after_unknown"`
    BeforeSensitive json.RawMessage `json:"before_sensitive"`
    AfterSensitive  json.RawMessage `json:"after_sensitive"`
}
```

`json.RawMessage`를 사용하는 이유: 리소스 속성의 구조가 프로바이더마다 다르므로, 스키마에 묶이지 않는 raw JSON으로 표현한다.

---

## 4. JSON State & Config

### jsonstate

State를 JSON으로 직렬화하며, `terraform show -json` 명령의 State 부분을 담당한다.

### jsonconfig

HCL 설정을 JSON으로 직렬화하며, 표현식을 별도로 처리한다:

```
HCL 표현식 "var.name" → JSON:
{
  "references": ["var.name"],
  "constant_value": null
}

HCL 리터럴 "hello" → JSON:
{
  "references": [],
  "constant_value": "hello"
}
```

### jsonformat

JSON diff를 사람이 읽기 좋은 형태로 렌더링한다. `terraform plan`의 `~`, `+`, `-` 출력을 JSON 데이터에서 생성한다.

---

## 5. Promising - 데드락 프리 동시성

### 패키지 문서

```
internal/promising/
├── promise.go           # 핵심 Promise 구현 (327줄)
├── promise_resolver.go  # PromiseResolver (73줄)
├── task.go              # MainTask, AsyncTask (152줄)
├── once.go              # sync.Once 대체 (92줄)
├── errors.go            # ErrUnresolved, ErrSelfDependent (29줄)
├── promise_container.go # PromiseContainer 인터페이스
├── ptr_set.go           # 포인터 셋 유틸
└── doc.go               # 패키지 문서
```

### 핵심 개념

```
┌──────────────────────────────────────────────────────────┐
│                    Promising 시스템                       │
│                                                          │
│  ┌────────────┐ resolve  ┌─────────────┐ get  ┌───────┐ │
│  │   Task A   │─────────>│   Promise   │<─────│Task B │ │
│  │ (생산자)   │          │ (값 저장소)  │      │(소비자)│ │
│  │            │          │             │      │       │ │
│  │ responsible│          │ result: T   │      │awaiting│ │
│  └────────────┘          └─────────────┘      └───────┘ │
│                                                          │
│  자기 의존성 감지:                                        │
│  Task A → awaiting Promise X → responsible: Task B      │
│  Task B → awaiting Promise Y → responsible: Task A      │
│  = 순환! → ErrSelfDependent                              │
└──────────────────────────────────────────────────────────┘
```

### promise 구조체

```go
type promise struct {
    name        string
    responsible atomic.Pointer[task]    // 해결 책임이 있는 Task
    result      atomic.Pointer[promiseResult]  // 결과 저장
    traceSpan   trace.Span             // OpenTelemetry 트레이싱

    waiting     []chan<- struct{}       // 대기 중인 채널 목록
    waitingMu   sync.Mutex             // waiting 보호
}

type promiseResult struct {
    val    any     // 결과 값
    err    error   // 에러
    forced bool    // 내부 강제 해결 (자기 의존성 등)
}
```

---

## 6. Promise 생명주기

### NewPromise

```go
func NewPromise[T any](ctx context.Context, name string) (PromiseResolver[T], PromiseGet[T]) {
    initialResponsible := mustTaskFromContext(ctx)
    p := &promise{name: name}
    p.responsible.Store(initialResponsible)
    initialResponsible.responsible[p] = struct{}{}

    resolver := PromiseResolver[T]{p}
    getter := PromiseGet[T](func(ctx context.Context) (T, error) {
        // 대기 로직...
    })

    return resolver, getter
}
```

### 생명주기 다이어그램

```
1. NewPromise(ctx, "data") → (resolver, getter)
   └── 현재 Task가 responsible로 설정됨

2. AsyncTask(ctx, resolver, func(ctx, resolver) {
       result := compute()
       resolver.Resolve(ctx, result, nil)  // 해결
   })
   └── 책임이 새 Task로 이전됨

3. 다른 Task에서: val, err := getter(ctx)
   └── 결과가 있으면 즉시 반환
   └── 없으면 채널에서 대기

4. Promise 해결 시:
   └── 모든 대기 채널 close → 대기자들 깨어남
```

### 결과 해결 (resolvePromise)

```go
func resolvePromise(p *promise, v any, err error) {
    p.waitingMu.Lock()
    defer p.waitingMu.Unlock()

    // 책임 Task 정리
    respT := p.responsible.Load()
    p.responsible.Store(nil)
    respT.responsible.Remove(p)

    // 결과 저장 (CAS로 중복 해결 방지)
    ok := p.result.CompareAndSwap(nil, &promiseResult{val: v, err: err})
    if !ok {
        r := p.result.Load()
        if r != nil && r.forced { return }  // 내부 강제 해결과 경합 → 무시
        panic("promise resolved more than once")
    }

    // 모든 대기자에게 알림
    for _, waitingCh := range p.waiting {
        close(waitingCh)
    }
    p.waiting = nil
}
```

---

## 7. Task 시스템

### MainTask

```go
func MainTask[T any](ctx context.Context, impl func(ctx context.Context) (T, error)) (T, error) {
    mainT := &task{responsible: make(promiseSet)}
    ctx = contextWithTask(ctx, mainT)
    v, err := impl(ctx)

    // 미해결 Promise 정리
    for unresolved := range mainT.responsible {
        oneErr := ErrUnresolved{unresolved.promiseID()}
        resolvePromise(unresolved, nil, oneErr)
    }
    return v, err
}
```

### AsyncTask

```go
func AsyncTask[P PromiseContainer](ctx context.Context, promises P,
    impl func(ctx context.Context, promises P)) {

    callerT := mustTaskFromContext(ctx)
    newT := &task{responsible: make(promiseSet)}

    // Promise 책임 이전: 호출자 → 새 Task
    promises.AnnounceContainedPromises(func(apr AnyPromiseResolver) {
        p := apr.promise()
        newT.responsible.Add(p)
        callerT.responsible.Remove(p)
        p.responsible.Store(newT)
    })

    go func() {
        ctx = contextWithTask(ctx, newT)
        impl(ctx, promises)

        // 미해결 Promise 정리
        for unresolved := range newT.responsible {
            err := ErrUnresolved{unresolved.promiseID()}
            resolvePromise(unresolved, nil, err)
        }
    }()
}
```

---

## 8. 자기 의존성 감지

### 감지 알고리즘

```go
// Promise getter 내부 로직 (promise.go)
// Task A가 Promise P를 기다리려 할 때:

checkP := p
checkT := p.responsible.Load()  // P를 해결할 책임이 있는 Task
steps := 1

for checkT != reqT {  // reqT = 현재 Task (A)
    if checkT == nil { break }
    nextCheckP := checkT.awaiting.Load()  // 그 Task가 기다리는 Promise
    if nextCheckP == nil { break }
    checkP = nextCheckP
    checkT = checkP.responsible.Load()  // 그 Promise의 책임 Task
}

if checkT == reqT {
    // 순환 발견! → ErrSelfDependent
    // 체인 내 모든 Promise를 강제 해결
    for _, affected := range affectedPromises {
        resolvePromiseInternalFailure(affected, err)
    }
}
```

### 감지 시나리오

```
Task A ─── responsible ──► Promise X
Task B ─── responsible ──► Promise Y
Task A ─── awaiting ──► Promise Y (B가 해결해야 함)
Task B ─── awaiting ──► Promise X (A가 해결해야 함)

감지 체인:
Task A wants Promise Y
→ Y.responsible = Task B
→ B.awaiting = Promise X
→ X.responsible = Task A
→ Task A == Task A → 순환!
```

**왜 표준 sync.Once 대신 이것을 만들었는가?**

`sync.Once`는 자기 의존성 시 데드락이 발생한다. Promising의 `Once`는 자기 의존성을 감지하여 에러를 반환하므로 데드락 대신 명확한 에러 메시지를 받을 수 있다.

---

## 9. Once - 단일 실행 보장

### `internal/promising/once.go`

```go
type Once[T any] struct {
    get       PromiseGet[T]
    promiseID PromiseID
    mu        sync.Mutex
}

func (o *Once[T]) Do(ctx context.Context, name string,
    f func(ctx context.Context) (T, error)) (T, error) {

    o.mu.Lock()
    if o.get == nil {
        // 첫 번째 호출: Promise 생성 + AsyncTask 시작
        resolver, get := NewPromise[T](ctx, name)
        o.get = get
        o.mu.Unlock()

        AsyncTask(ctx, resolver,
            func(ctx context.Context, resolver PromiseResolver[T]) {
                v, err := f(ctx)
                resolver.Resolve(ctx, v, err)
            })
    } else {
        o.mu.Unlock()
    }

    // 모든 호출자가 동일한 getter를 사용하여 결과 대기
    return o.get(ctx)
}
```

---

## 10. Named Values - 값 스토어

### State 구조체

```go
type State struct {
    mu        sync.Mutex
    variables inputVariableValues   // 입력 변수
    locals    localValues           // 로컬 값
    outputs   outputValues          // 출력 값
}
```

### values 제네릭 구조체

```go
type values[LocalType namedValueAddr, AbsType namedValueAddr] struct {
    exact       addrs.Map[AbsType, cty.Value]       // 정확한 인스턴스 값
    placeholder addrs.Map[addrs.Module, addrs.Map[  // 플레이스홀더 값
                    addrs.InPartialExpandedModule[LocalType], cty.Value]]
}
```

### 값 설정/조회

```go
func (v *values[L, A]) SetExactResult(addr A, val cty.Value) {
    if v.exact.Has(addr) {
        panic(fmt.Sprintf("value for %s was already set", addr))
    }
    v.exact.Put(addr, val)
}

func (v *values[L, A]) GetExactResult(addr A) cty.Value {
    if !v.exact.Has(addr) {
        panic(fmt.Sprintf("value for %s was requested before it was provided", addr))
    }
    return v.exact.Get(addr)
}
```

**왜 panic인가?** Set/Get 순서가 잘못되면 그래프 워커의 의존성 관리에 버그가 있다는 의미다. 이는 런타임에 복구 불가능한 프로그래밍 에러이므로 panic이 적절하다.

---

## 11. Placeholder 결과 시스템

### 미확장 모듈의 Placeholder

모듈의 `count`나 `for_each`가 아직 평가되지 않았을 때, 하위 값들의 "플레이스홀더"를 제공한다.

```go
func (v *values[L, A]) GetPlaceholderResult(
    addr addrs.InPartialExpandedModule[L]) cty.Value {

    modAddr := addr.Module.Module()
    if !v.placeholder.Has(modAddr) {
        return cty.DynamicVal  // 플레이스홀더가 없으면 완전 미지
    }

    placeholders := v.placeholder.Get(modAddr)

    // 가장 구체적인 (longest prefix) 플레이스홀더 선택
    longestVal := cty.DynamicVal
    longestLen := -1

    for _, elem := range placeholders.Elems {
        candidate := elem.Key
        lenKnown := candidate.ModuleLevelsKnown()
        if lenKnown < longestLen { continue }
        if !addrs.Equivalent(candidate.Local, addr.Local) { continue }
        if !candidate.Module.MatchesPartial(addr.Module) { continue }
        longestVal = elem.Value
        longestLen = lenKnown
    }

    return longestVal
}
```

### Longest Prefix 매칭

```
모듈 경로: module.a.module.b.module.c

플레이스홀더 후보:
1. module.a → prefix length 1
2. module.a.module.b → prefix length 2  ← 선택 (더 구체적)

결과: 2번의 플레이스홀더 값 사용
```

---

## 12. Instance Expander - count/for_each 확장

### Expander 구조체

```go
type Expander struct {
    mu   sync.Mutex
    exps *expanderModule  // 재귀적 모듈 트리
}
```

### expansion 인터페이스

```go
type expansion interface {
    instanceKeys() (keyType addrs.InstanceKeyType,
                    keys []addrs.InstanceKey,
                    keysUnknown bool)
    repetitionData(addrs.InstanceKey) RepetitionData
}
```

### 확장 모드

| 모드 | 타입 | 인스턴스 키 예시 |
|------|------|---------------|
| 단일 | `expansionSingle` | `NoKey` (키 없음) |
| count | `expansionCount` | `[0]`, `[1]`, `[2]` |
| for_each | `expansionForEach` | `["a"]`, `["b"]` |
| deferred | `expansionDeferred` | 미확정 (Unknown) |

### expansionCount

```go
type expansionCount int

func (e expansionCount) instanceKeys() (addrs.InstanceKeyType, []addrs.InstanceKey, bool) {
    ret := make([]addrs.InstanceKey, int(e))
    for i := range ret {
        ret[i] = addrs.IntKey(i)  // 0, 1, 2, ...
    }
    return addrs.IntKeyType, ret, false
}

func (e expansionCount) repetitionData(key addrs.InstanceKey) RepetitionData {
    i := int(key.(addrs.IntKey))
    return RepetitionData{
        CountIndex: cty.NumberIntVal(int64(i)),  // count.index 제공
    }
}
```

### expansionForEach

```go
type expansionForEach map[string]cty.Value

func (e expansionForEach) instanceKeys() (addrs.InstanceKeyType, []addrs.InstanceKey, bool) {
    ret := make([]addrs.InstanceKey, 0, len(e))
    for k := range e {
        ret = append(ret, addrs.StringKey(k))
    }
    sort.Slice(ret, func(i, j int) bool {
        return ret[i].(addrs.StringKey) < ret[j].(addrs.StringKey)
    })
    return addrs.StringKeyType, ret, false
}

func (e expansionForEach) repetitionData(key addrs.InstanceKey) RepetitionData {
    k := string(key.(addrs.StringKey))
    v := e[k]
    return RepetitionData{
        EachKey:   cty.StringVal(k),    // each.key 제공
        EachValue: v,                    // each.value 제공
    }
}
```

### expansionDeferred

```go
type expansionDeferred rune

const expansionDeferredIntKey = expansionDeferred(addrs.IntKeyType)
const expansionDeferredStringKey = expansionDeferred(addrs.StringKeyType)

func (e expansionDeferred) instanceKeys() (addrs.InstanceKeyType, []addrs.InstanceKey, bool) {
    return addrs.InstanceKeyType(e), nil, true  // keysUnknown = true
}
```

Deferred expansion은 `count`나 `for_each`의 값이 아직 알려지지 않은 경우에 사용된다. 이는 Terraform 1.9의 deferred actions 기능과 연관된다.

---

## 13. Expansion Mode 시스템

### Set (읽기 전용 뷰)

```go
type Set struct {
    exp *Expander  // Expander의 읽기 전용 래퍼
}

func (s Set) HasModuleInstance(want addrs.ModuleInstance) bool {
    return s.exp.knowsModuleInstance(want)
}

func (s Set) HasResourceInstance(want addrs.AbsResourceInstance) bool {
    return s.exp.knowsResourceInstance(want)
}

func (s Set) InstancesForModule(modAddr addrs.Module, ...) []addrs.ModuleInstance {
    return s.exp.expandModule(modAddr, true, includeDirectOverrides)
}
```

**왜 Set을 별도로 만들었는가?** `Expander`는 쓰기 가능한 객체이지만, `AllInstances()`로 얻은 스냅샷은 읽기만 가능해야 한다. Set은 Expander의 API 중 조회 메서드만 노출하여, 결과 소비자가 실수로 확장 상태를 변경하는 것을 방지한다.

### 재귀적 확장

```
module.vpc (count = 2)
├── [0] module.vpc[0]
│   └── aws_subnet.main (for_each = {a, b})
│       ├── module.vpc[0].aws_subnet.main["a"]
│       └── module.vpc[0].aws_subnet.main["b"]
└── [1] module.vpc[1]
    └── aws_subnet.main (for_each = {a, b})
        ├── module.vpc[1].aws_subnet.main["a"]
        └── module.vpc[1].aws_subnet.main["b"]
```

Expander 주석에 명시:

> Because resources belong to modules and modules can nest inside other
> modules, module expansion in particular has a recursive effect that can
> cause deep objects to expand exponentially.

---

## 14. 왜(Why) 이렇게 설계했나

### Q1: JSON 형식에 왜 FormatVersion이 있는가?

Terraform 버전과 JSON 스키마 버전은 독립적이다. Terraform 1.5에서 추가한 JSON 필드는 Terraform 1.4로 생성된 JSON에는 없다. FormatVersion으로 소비자가 파싱 전략을 결정한다.

### Q2: Promising에서 왜 일반 채널 대신 Promise를 만들었는가?

채널은 자기 의존성(deadlock)을 감지할 수 없다. Promising은 `awaiting → responsible` 체인을 추적하여 순환 의존성을 즉시 감지하고 에러로 변환한다.

### Q3: Named Values에서 왜 panic을 사용하는가?

Get/Set 순서 위반은 DAG 워커의 의존성 관리 버그를 의미한다. 이는:
- 정상적으로 복구 불가능
- 잘못된 값으로 인프라를 변경하면 치명적 결과 초래
- 빠른 실패(fail-fast)가 최선의 전략

### Q4: Instance Expander의 for_each 키가 왜 정렬되는가?

```go
sort.Slice(ret, func(i, j int) bool {
    return ret[i].(addrs.StringKey) < ret[j].(addrs.StringKey)
})
```

Go의 map 순회는 비결정적이다. Plan 출력의 일관성을 위해 키를 정렬하여 동일한 설정에서 항상 같은 순서로 인스턴스를 나열한다.

### Q5: expansionDeferred는 왜 필요한가?

Terraform 1.9부터 "deferred actions" 기능이 도입되었다. `count = var.x`에서 `var.x`가 다른 리소스의 출력에 의존하면, 첫 번째 Plan에서는 인스턴스 수를 알 수 없다. `expansionDeferred`는 이 "아직 모르는" 상태를 표현한다.

### Q6: 왜 Placeholder는 Longest Prefix 매칭을 사용하는가?

```
module.a.module.b의 variable "x" 값을 모름
module.a 수준의 placeholder: x = "unknown"
module.a.module.b 수준의 placeholder: x = "partially_known"

→ module.a.module.b 수준이 더 구체적이므로 선택
```

모듈이 부분적으로 확장된 상태에서, 더 깊은 수준의 플레이스홀더가 더 정확한 정보를 제공한다.

---

## 15. PoC 매핑

| PoC | 시뮬레이션 대상 |
|-----|---------------|
| poc-26-json-output | JSON Plan/State 직렬화, FormatVersion, Change 구조 |
| poc-27-promising | Promise/Task 시스템, 자기 의존성 감지, Once |
| poc-28-namedvals | Named Values 스토어, Placeholder, Longest Prefix |
| poc-29-instance-expander | count/for_each 확장, 재귀적 모듈 확장, Deferred |

---

## 참조 소스 파일

| 파일 | 줄수 | 핵심 내용 |
|------|------|----------|
| `internal/command/jsonplan/plan.go` | 500+ | plan 구조체, Change, FormatVersion |
| `internal/promising/promise.go` | 327 | Promise 생명주기, 자기 의존성 감지 |
| `internal/promising/task.go` | 152 | MainTask, AsyncTask, 책임 이전 |
| `internal/promising/once.go` | 92 | Once.Do, sync.Once 대체 |
| `internal/promising/errors.go` | 29 | ErrUnresolved, ErrSelfDependent |
| `internal/namedvals/state.go` | 127 | State, Get/Set 메서드 |
| `internal/namedvals/values.go` | 142 | 제네릭 values, Placeholder |
| `internal/instances/expander.go` | 1000+ | Expander, 재귀적 확장 |
| `internal/instances/expansion_mode.go` | 125 | expansion 인터페이스, Single/Count/ForEach/Deferred |
| `internal/instances/set.go` | 55 | Set (읽기 전용 뷰) |
