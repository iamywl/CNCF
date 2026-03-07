# 07. DAG 그래프 엔진 심화

## 목차

1. [개요](#1-개요)
2. [Graph 기본 구조](#2-graph-기본-구조)
3. [AcyclicGraph 전체 구현 분석](#3-acyclicgraph-전체-구현-분석)
4. [위상 정렬 알고리즘 상세](#4-위상-정렬-알고리즘-상세)
5. [사이클 검출 알고리즘 상세](#5-사이클-검출-알고리즘-상세)
6. [이행적 축소 알고리즘 상세](#6-이행적-축소-알고리즘-상세)
7. [Walker 병렬 실행 엔진](#7-walker-병렬-실행-엔진)
8. [동시성 패턴 분석](#8-동시성-패턴-분석)
9. [Set 자료구조와 해싱](#9-set-자료구조와-해싱)
10. [Edge와 Vertex 추상화](#10-edge와-vertex-추상화)
11. [그래프 순회 알고리즘들](#11-그래프-순회-알고리즘들)
12. [왜 이렇게 설계했는가](#12-왜-이렇게-설계했는가)

---

## 1. 개요

Terraform의 DAG(Directed Acyclic Graph) 엔진은 인프라 리소스 간의 의존성을 모델링하고, 안전한 실행 순서를 결정하며, 가능한 한 많은 작업을 병렬로 수행하는 핵심 엔진이다. 모든 Terraform 작업(plan, apply, destroy)은 이 그래프 엔진 위에서 동작한다.

**소스 위치**: `internal/dag/` 디렉토리

```
internal/dag/
├── dag.go          # AcyclicGraph 구현 (Validate, Walk, TransitiveReduction 등)
├── graph.go        # Graph 기본 구조 (vertices, edges, upEdges, downEdges)
├── walk.go         # Walker 병렬 실행 엔진 (고루틴 기반)
├── tarjan.go       # Tarjan의 강연결 컴포넌트 (사이클 검출)
├── set.go          # Set 자료구조 (해시 기반)
├── edge.go         # Edge 인터페이스와 BasicEdge 구현
├── marshal.go      # DOT 형식 마샬링
└── *_test.go       # 테스트 파일들
```

### 핵심 설계 원칙

| 원칙 | 설명 |
|------|------|
| 비순환성 보장 | 모든 그래프는 사이클이 없어야 함 (DAG) |
| 병렬 실행 | 의존성이 없는 노드는 동시에 실행 |
| 이행적 축소 | 불필요한 간선을 제거하여 그래프를 단순화 |
| 동적 업데이트 | 실행 중에도 그래프를 수정 가능 |

---

## 2. Graph 기본 구조

### 2.1 Graph 구조체

`internal/dag/graph.go`에 정의된 `Graph`는 DAG 엔진의 기반이 되는 방향 그래프 구조체다.

```go
// internal/dag/graph.go

type Graph struct {
    vertices  Set
    edges     Set
    downEdges map[interface{}]Set  // source -> {target1, target2, ...}
    upEdges   map[interface{}]Set  // target -> {source1, source2, ...}
}
```

이 구조는 **인접 리스트(adjacency list)** 방식의 그래프 표현이다. `downEdges`와 `upEdges`를 동시에 유지함으로써 양방향 탐색이 O(1)에 가능하다.

```
    downEdges (의존 대상 방향)
    ┌──────────────────────────┐
    │  A ──→ {B, C}           │   A가 B와 C에 의존
    │  B ──→ {D}              │   B가 D에 의존
    │  C ──→ {D}              │   C가 D에 의존
    │  D ──→ {}               │   D는 의존 대상 없음
    └──────────────────────────┘

    upEdges (역방향, 의존하는 것들)
    ┌──────────────────────────┐
    │  A ──→ {}               │   A를 의존하는 것 없음
    │  B ──→ {A}              │   B를 의존하는 것: A
    │  C ──→ {A}              │   C를 의존하는 것: A
    │  D ──→ {B, C}           │   D를 의존하는 것: B, C
    └──────────────────────────┘
```

### 2.2 초기화 (지연 초기화 패턴)

```go
// internal/dag/graph.go

func (g *Graph) init() {
    if g.vertices == nil {
        g.vertices = make(Set)
    }
    if g.edges == nil {
        g.edges = make(Set)
    }
    if g.downEdges == nil {
        g.downEdges = make(map[interface{}]Set)
    }
    if g.upEdges == nil {
        g.upEdges = make(map[interface{}]Set)
    }
}
```

**왜 지연 초기화인가?** 그래프를 값 타입(struct)으로 임베딩할 때 zero value 상태에서도 안전하게 사용할 수 있도록 하기 위해서다. `AcyclicGraph`가 `Graph`를 임베딩하는데, 이때 별도 생성자 없이도 동작한다.

### 2.3 정점(Vertex) 추가/제거

```go
// internal/dag/graph.go

func (g *Graph) Add(v Vertex) Vertex {
    g.init()
    g.vertices.Add(v)
    return v
}

func (g *Graph) Remove(v Vertex) Vertex {
    g.vertices.Delete(v)
    // 해당 정점과 연결된 모든 간선도 함께 제거
    for _, target := range g.downEdgesNoCopy(v) {
        g.RemoveEdge(BasicEdge(v, target))
    }
    for _, source := range g.upEdgesNoCopy(v) {
        g.RemoveEdge(BasicEdge(source, v))
    }
    return nil
}
```

### 2.4 간선(Edge) 연결

`Connect` 메서드는 중복 간선 방지 로직을 포함한다.

```go
// internal/dag/graph.go

func (g *Graph) Connect(edge Edge) {
    g.init()
    source := edge.Source()
    target := edge.Target()
    sourceCode := hashcode(source)
    targetCode := hashcode(target)

    // 중복 간선 확인
    if s, ok := g.downEdges[sourceCode]; ok && s.Include(target) {
        return
    }

    g.edges.Add(edge)

    // downEdge 추가 (source -> target 방향)
    s, ok := g.downEdges[sourceCode]
    if !ok {
        s = make(Set)
        g.downEdges[sourceCode] = s
    }
    s.Add(target)

    // upEdge 추가 (target -> source 역방향)
    s, ok = g.upEdges[targetCode]
    if !ok {
        s = make(Set)
        g.upEdges[targetCode] = s
    }
    s.Add(source)
}
```

```
Connect(A → B) 수행 시:

    edges:     {(A→B)}
    downEdges: { hash(A): {B} }
    upEdges:   { hash(B): {A} }

    시간 복잡도: O(1) (해시 기반)
```

### 2.5 Replace 메서드

정점을 교체하면서 기존 간선을 보존한다.

```go
// internal/dag/graph.go

func (g *Graph) Replace(original, replacement Vertex) bool {
    if !g.vertices.Include(original) {
        return false
    }
    if original == replacement {
        return true
    }
    g.Add(replacement)
    // 기존 간선을 새 정점으로 복사
    for _, target := range g.downEdgesNoCopy(original) {
        g.Connect(BasicEdge(replacement, target))
    }
    for _, source := range g.upEdgesNoCopy(original) {
        g.Connect(BasicEdge(source, replacement))
    }
    g.Remove(original)
    return true
}
```

---

## 3. AcyclicGraph 전체 구현 분석

### 3.1 AcyclicGraph 정의

```go
// internal/dag/dag.go

type AcyclicGraph struct {
    Graph  // Graph를 임베딩 (상속이 아닌 합성)
}
```

`AcyclicGraph`는 `Graph`를 임베딩하여 기본 그래프 연산을 물려받고, DAG 특화 연산(사이클 검출, 위상 정렬, 이행적 축소 등)을 추가한다.

### 3.2 Root 찾기

```go
// internal/dag/dag.go

func (g *AcyclicGraph) Root() (Vertex, error) {
    roots := make([]Vertex, 0, 1)
    for _, v := range g.Vertices() {
        if g.upEdgesNoCopy(v).Len() == 0 {
            roots = append(roots, v)
        }
    }
    if len(roots) > 1 {
        return nil, fmt.Errorf("multiple roots: %#v", roots)
    }
    if len(roots) == 0 {
        return nil, fmt.Errorf("no roots found")
    }
    return roots[0], nil
}
```

루트는 **upEdge가 없는 정점**(아무도 의존하지 않는 정점)이다. DAG에서 루트가 하나여야 하는 이유는 Terraform 그래프가 단일 시작점(root module)에서 출발해야 하기 때문이다.

```
복잡도: O(V) — 모든 정점을 한 번씩 검사
```

### 3.3 Validate 메서드

```go
// internal/dag/dag.go

func (g *AcyclicGraph) Validate() error {
    // 1단계: 루트가 정확히 하나인지 확인
    if _, err := g.Root(); err != nil {
        return err
    }

    // 2단계: 사이클 검출 (2개 이상의 컴포넌트)
    var err error
    cycles := g.Cycles()
    if len(cycles) > 0 {
        for _, cycle := range cycles {
            cycleStr := make([]string, len(cycle))
            for j, vertex := range cycle {
                cycleStr[j] = VertexName(vertex)
            }
            err = errors.Join(err, fmt.Errorf(
                "Cycle: %s", strings.Join(cycleStr, ", ")))
        }
    }

    // 3단계: 자기 참조(self-reference) 검출
    for _, e := range g.Edges() {
        if e.Source() == e.Target() {
            err = errors.Join(err, fmt.Errorf(
                "Self reference: %s", VertexName(e.Source())))
        }
    }

    return err
}
```

검증은 3단계로 이루어진다:

```
Validate() 흐름:
┌─────────────────────────────────────────┐
│  1. Root() 호출                          │
│     └─ 루트가 0개 또는 2개 이상 → 에러    │
│                                          │
│  2. Cycles() 호출 (Tarjan 알고리즘)      │
│     └─ 강연결 컴포넌트 > 1개 → 사이클     │
│                                          │
│  3. 자기 참조 검출                        │
│     └─ edge.Source() == edge.Target()    │
└─────────────────────────────────────────┘
```

### 3.4 Ancestors / Descendants

```go
// internal/dag/dag.go

// 조상(하위 의존 대상) 집합 반환
func (g *AcyclicGraph) Ancestors(vs ...Vertex) Set {
    s := make(Set)
    memoFunc := func(v Vertex, d int) error {
        s.Add(v)
        return nil
    }
    start := make(Set)
    for _, v := range vs {
        for _, dep := range g.downEdgesNoCopy(v) {
            start.Add(dep)
        }
    }
    if err := g.DepthFirstWalk(start, memoFunc); err != nil {
        return nil
    }
    return s
}

// 자손(상위 의존자) 집합 반환
func (g *AcyclicGraph) Descendants(v Vertex) Set {
    s := make(Set)
    memoFunc := func(v Vertex, d int) error {
        s.Add(v)
        return nil
    }
    start := make(Set)
    for _, dep := range g.upEdgesNoCopy(v) {
        start.Add(dep)
    }
    g.ReverseDepthFirstWalk(start, memoFunc)
    return s
}
```

```
Ancestors(A) = A가 의존하는 모든 것 (downEdges 따라 아래로)
Descendants(D) = D에 의존하는 모든 것 (upEdges 따라 위로)

    A ──→ B ──→ D
    │           ↑
    └──→ C ─────┘

    Ancestors(A) = {B, C, D}
    Descendants(D) = {A, B, C}
```

### 3.5 Walk 메서드 (병렬 실행 진입점)

```go
// internal/dag/dag.go

func (g *AcyclicGraph) Walk(cb WalkFunc) tfdiags.Diagnostics {
    w := &Walker{Callback: cb, Reverse: true}
    w.Update(g)
    return w.Wait()
}
```

`Walk`는 **Walker를 생성하고, 그래프를 주입하고, 완료를 기다리는** 3단계로 동작한다. `Reverse: true`로 설정되어 있어 target이 source에 의존하는 방향으로 실행된다 (Terraform의 "리소스 D를 먼저 만들고, D에 의존하는 A를 나중에 만든다" 의미).

---

## 4. 위상 정렬 알고리즘 상세

### 4.1 DFS 기반 위상 정렬

```go
// internal/dag/dag.go

func (g *AcyclicGraph) topoOrder(order walkType) []Vertex {
    sorted := make([]Vertex, 0, len(g.vertices))

    // tmp: 현재 방문 중인 노드 (사이클 검출용)
    tmp := map[Vertex]bool{}
    // perm: 완료된 노드 (재귀 종료 조건)
    perm := map[Vertex]bool{}

    var visit func(v Vertex)
    visit = func(v Vertex) {
        if perm[v] {
            return  // 이미 처리 완료
        }
        if tmp[v] {
            panic("cycle found in dag")  // 사이클 발견!
        }

        tmp[v] = true
        var next Set
        switch {
        case order&downOrder != 0:
            next = g.downEdgesNoCopy(v)
        case order&upOrder != 0:
            next = g.upEdgesNoCopy(v)
        }

        for _, u := range next {
            visit(u)
        }

        tmp[v] = false
        perm[v] = true
        sorted = append(sorted, v)  // 후위 순서로 추가
    }

    for _, v := range g.Vertices() {
        visit(v)
    }
    return sorted
}
```

### 4.2 알고리즘 동작 과정

```
그래프: A → B → D, A → C → D

DFS 방문 순서 (downOrder):

visit(A):
  tmp = {A}
  visit(B):
    tmp = {A, B}
    visit(D):
      tmp = {A, B, D}
      D의 next = {} (하위 없음)
      sorted = [D]
      perm = {D}
    sorted = [D, B]
    perm = {D, B}
  visit(C):
    tmp = {A, C}
    visit(D): perm[D] = true → return
    sorted = [D, B, C]
    perm = {D, B, C}
  sorted = [D, B, C, A]
  perm = {D, B, C, A}

TopologicalOrder (upOrder) 결과: [D, B, C, A]
  → D를 먼저 생성, A를 마지막에 생성

ReverseTopologicalOrder (downOrder) 결과: [A, C, B, D]
  → A를 먼저 제거, D를 마지막에 제거
```

### 4.3 시간 복잡도

| 단계 | 복잡도 |
|------|--------|
| 모든 정점 순회 | O(V) |
| 각 정점의 간선 탐색 | O(E) |
| 전체 | O(V + E) |

---

## 5. 사이클 검출 알고리즘 상세

### 5.1 Tarjan의 강연결 컴포넌트 알고리즘

`internal/dag/tarjan.go`에 구현된 Tarjan 알고리즘은 그래프에서 **강연결 컴포넌트(Strongly Connected Components, SCC)**를 찾는다. 크기가 2 이상인 SCC는 사이클을 의미한다.

```go
// internal/dag/tarjan.go

func StronglyConnected(g *Graph) [][]Vertex {
    vs := g.Vertices()
    acct := sccAcct{
        NextIndex:   1,
        VertexIndex: make(map[Vertex]int, len(vs)),
    }
    for _, v := range vs {
        if acct.VertexIndex[v] == 0 {
            stronglyConnected(&acct, g, v)
        }
    }
    return acct.SCC
}

func stronglyConnected(acct *sccAcct, g *Graph, v Vertex) int {
    index := acct.visit(v)  // 인덱스 부여 + 스택 push
    minIdx := index

    for _, raw := range g.downEdgesNoCopy(v) {
        target := raw.(Vertex)
        targetIdx := acct.VertexIndex[target]

        if targetIdx == 0 {
            // 미방문 → 재귀
            minIdx = min(minIdx, stronglyConnected(acct, g, target))
        } else if acct.inStack(target) {
            // 스택에 있음 → 사이클 후보
            minIdx = min(minIdx, targetIdx)
        }
    }

    // 루트 정점이면 SCC를 스택에서 pop
    if index == minIdx {
        var scc []Vertex
        for {
            v2 := acct.pop()
            scc = append(scc, v2)
            if v2 == v {
                break
            }
        }
        acct.SCC = append(acct.SCC, scc)
    }
    return minIdx
}
```

### 5.2 sccAcct 보조 구조체

```go
// internal/dag/tarjan.go

type sccAcct struct {
    NextIndex   int
    VertexIndex map[Vertex]int  // 방문 순서 기록
    Stack       []Vertex         // DFS 스택
    SCC         [][]Vertex       // 발견된 SCC 목록
}
```

### 5.3 동작 예시

```
사이클이 있는 그래프: A → B → C → A

visit(A): index=1, stack=[A]
  visit(B): index=2, stack=[A,B]
    visit(C): index=3, stack=[A,B,C]
      C → A: A는 스택에 있음! minIdx = min(3, 1) = 1
      index(3) != minIdx(1) → SCC 생성 안함
    minIdx = min(2, 1) = 1
    index(2) != minIdx(1) → SCC 생성 안함
  minIdx = min(1, 1) = 1
  index(1) == minIdx(1) → SCC 생성!
  pop: [C, B, A] → SCC = [[C, B, A]]

Cycles() 메서드:
  SCC 중 len > 1인 것 = [[C, B, A]] → 사이클 발견!
```

### 5.4 사이클 vs 자기 참조

| 유형 | 검출 방법 | 예시 |
|------|-----------|------|
| 사이클 | Tarjan SCC (len > 1) | A → B → C → A |
| 자기 참조 | Edge 직접 검사 | A → A |

Tarjan 알고리즘은 자기 참조(self-loop)를 SCC 크기 1로 보고하기 때문에 별도로 검출한다.

---

## 6. 이행적 축소 알고리즘 상세

### 6.1 이행적 축소란?

이행적 축소(Transitive Reduction)는 **도달 가능성을 유지하면서 간선 수를 최소화**하는 변환이다.

```
변환 전:                  변환 후:
A ──→ B ──→ C            A ──→ B ──→ C
│           ↑
└───────────┘  (불필요한 직접 간선 제거)
```

A에서 C까지 B를 통해 도달할 수 있으므로, A→C 직접 간선은 불필요하다.

### 6.2 구현 분석

```go
// internal/dag/dag.go

func (g *AcyclicGraph) TransitiveReduction() {
    for _, u := range g.Vertices() {
        uTargets := g.downEdgesNoCopy(u)

        g.DepthFirstWalk(g.downEdgesNoCopy(u), func(v Vertex, d int) error {
            // u의 직접 자식 중, v의 자식과 겹치는 것을 찾아 제거
            shared := uTargets.Intersection(g.downEdgesNoCopy(v))
            for _, vPrime := range shared {
                g.RemoveEdge(BasicEdge(u, vPrime))
            }
            return nil
        })
    }
}
```

### 6.3 알고리즘 단계별 설명

```
그래프: A → {B, C, D},  B → {C, D},  C → {D}

u = A:
  uTargets = {B, C, D}
  DFS from {B, C, D}:
    visit B:
      B.downEdges = {C, D}
      shared = {B,C,D} ∩ {C,D} = {C, D}
      RemoveEdge(A→C), RemoveEdge(A→D)  ← A에서 C,D로 직접 갈 필요 없음!
    visit C:
      C.downEdges = {D}
      shared = {B} ∩ {D} = {}  (이미 C,D 제거됨)
    visit D:
      D.downEdges = {}

결과: A → {B},  B → {C, D},  C → {D}
  → A에서 B를 거쳐 C,D에 도달 가능하므로 직접 간선 불필요
```

### 6.4 시간 복잡도

```
각 정점 u에 대해:
  - u의 자식으로부터 DFS 수행: O(V + E)
  - 교집합 계산: O(V)

전체: O(V × (V + E)) = O(V² + VE) ≈ O(VE)
```

### 6.5 왜 이행적 축소를 수행하는가?

| 이유 | 설명 |
|------|------|
| 그래프 가독성 | `terraform graph` 출력의 시각적 복잡도 감소 |
| 디버깅 용이성 | 실제 의미 있는 의존성만 남김 |
| 실행 효율 | Walker가 추적해야 할 의존성 수 감소 |
| 사용자 이해 | 사용자가 의존성 구조를 더 쉽게 파악 |

---

## 7. Walker 병렬 실행 엔진

### 7.1 Walker 구조체

`internal/dag/walk.go`의 `Walker`는 DAG의 노드들을 **의존성 순서를 존중하면서 가능한 한 병렬로** 실행하는 엔진이다.

```go
// internal/dag/walk.go

type Walker struct {
    Callback WalkFunc    // 각 노드에서 실행할 콜백
    Reverse  bool        // true: target이 source에 의존

    changeLock sync.Mutex
    vertices   Set
    edges      Set
    vertexMap  map[Vertex]*walkerVertex

    wait sync.WaitGroup  // 모든 정점 완료 대기

    diagsMap       map[Vertex]tfdiags.Diagnostics
    upstreamFailed map[Vertex]struct{}
    diagsLock      sync.Mutex
}
```

### 7.2 walkerVertex 구조체

각 정점에 대한 실행 상태를 추적한다.

```go
// internal/dag/walk.go

type walkerVertex struct {
    DoneCh   chan struct{}    // 실행 완료 신호
    CancelCh chan struct{}    // 취소 신호

    DepsCh       chan bool       // 의존성 완료 결과 (성공/실패)
    DepsUpdateCh chan struct{}   // 의존성 변경 알림
    DepsLock     sync.Mutex

    deps         map[Vertex]chan struct{}  // 의존하는 정점 → 완료 채널
    depsCancelCh chan struct{}             // 이전 의존성 대기 취소
}
```

### 7.3 Update 메서드 (그래프 변경 반영)

```go
// internal/dag/walk.go

func (w *Walker) Update(g *AcyclicGraph) {
    w.changeLock.Lock()
    defer w.changeLock.Unlock()

    // 1. 차이 계산
    newEdges := e.Difference(w.edges)
    oldEdges := w.edges.Difference(e)
    newVerts := v.Difference(w.vertices)
    oldVerts := w.vertices.Difference(v)

    // 2. 새 정점 추가
    for _, raw := range newVerts {
        v := raw.(Vertex)
        w.wait.Add(1)
        w.vertices.Add(raw)
        info := &walkerVertex{
            DoneCh:   make(chan struct{}),
            CancelCh: make(chan struct{}),
            deps:     make(map[Vertex]chan struct{}),
        }
        w.vertexMap[v] = info
    }

    // 3. 오래된 정점 제거
    for _, raw := range oldVerts {
        v := raw.(Vertex)
        info := w.vertexMap[v]
        close(info.CancelCh)  // 취소 신호
        delete(w.vertexMap, v)
        w.vertices.Delete(raw)
    }

    // 4. 새 간선 처리: 의존성 채널 연결
    for _, raw := range newEdges {
        edge := raw.(Edge)
        waiter, dep := w.edgeParts(edge)
        waiterInfo.deps[dep] = depInfo.DoneCh
        changedDeps.Add(waiter)
    }

    // 5. 변경된 의존성에 대해 대기 고루틴 시작
    for _, raw := range changedDeps {
        go w.waitDeps(v, deps, doneCh, cancelCh)
    }

    // 6. 새 정점에 대한 워크 고루틴 시작
    for _, raw := range newVerts {
        go w.walkVertex(v, w.vertexMap[v])
    }
}
```

### 7.4 전체 실행 흐름

```
Update(graph) 호출 시:

    ┌─────────────────────────────────────────────┐
    │  각 Vertex에 대해:                            │
    │    1. walkerVertex 생성 (DoneCh, CancelCh)   │
    │    2. go walkVertex(v, info)  ← 고루틴 시작   │
    │                                               │
    │  각 Edge에 대해:                               │
    │    1. waiter.deps[dep] = dep.DoneCh           │
    │    2. go waitDeps(waiter, deps, ...)           │
    └─────────────────────────────────────────────┘

    walkVertex 동작:
    ┌─────────────────────────────────────────────┐
    │  1. 의존성 완료 대기 (depsCh에서 읽기)         │
    │  2. 의존성 성공 여부 확인                      │
    │  3. 성공 → Callback(v) 실행                   │
    │     실패 → "upstream dependencies failed"     │
    │  4. 결과를 diagsMap에 기록                     │
    │  5. close(DoneCh) → 자신에 의존하는 것들에 알림 │
    └─────────────────────────────────────────────┘
```

### 7.5 walkVertex 구현 분석

```go
// internal/dag/walk.go

func (w *Walker) walkVertex(v Vertex, info *walkerVertex) {
    defer w.wait.Done()
    defer close(info.DoneCh)

    // 의존성 대기 루프
    var depsSuccess bool
    var depsUpdateCh chan struct{}
    depsCh := make(chan bool, 1)
    depsCh <- true
    close(depsCh)

    for {
        select {
        case <-info.CancelCh:
            return  // 취소됨
        case depsSuccess = <-depsCh:
            depsCh = nil  // 완료 처리
        case <-depsUpdateCh:
            // 새 의존성 발생, 루프 계속
        }

        info.DepsLock.Lock()
        if info.DepsCh != nil {
            depsCh = info.DepsCh
            info.DepsCh = nil
        }
        if info.DepsUpdateCh != nil {
            depsUpdateCh = info.DepsUpdateCh
        }
        info.DepsLock.Unlock()

        if depsCh == nil {
            break  // 의존성 완료!
        }
    }

    // 콜백 실행 또는 업스트림 실패 기록
    var diags tfdiags.Diagnostics
    var upstreamFailed bool
    if depsSuccess {
        diags = w.Callback(v)
    } else {
        diags = diags.Append(errors.New("upstream dependencies failed"))
        upstreamFailed = true
    }

    // 결과 기록
    w.diagsLock.Lock()
    w.diagsMap[v] = diags
    if upstreamFailed {
        w.upstreamFailed[v] = struct{}{}
    }
    w.diagsLock.Unlock()
}
```

---

## 8. 동시성 패턴 분석

### 8.1 채널 기반 의존성 대기

```go
// internal/dag/walk.go

func (w *Walker) waitDeps(
    v Vertex,
    deps map[Vertex]<-chan struct{},
    doneCh chan<- bool,
    cancelCh <-chan struct{}) {

    for dep, depCh := range deps {
    DepSatisfied:
        for {
            select {
            case <-depCh:
                break DepSatisfied  // 의존성 충족!
            case <-cancelCh:
                doneCh <- false     // 취소됨
                return
            case <-time.After(time.Second * 5):
                // 5초마다 대기 상태 로그
                log.Printf("[TRACE] dag/walk: vertex %q is waiting for %q",
                    VertexName(v), VertexName(dep))
            }
        }
    }

    // 모든 의존성 충족, 에러 확인
    w.diagsLock.Lock()
    defer w.diagsLock.Unlock()
    for dep := range deps {
        if w.diagsMap[dep].HasErrors() {
            doneCh <- false  // 의존성 중 하나가 실패
            return
        }
    }
    doneCh <- true  // 모든 의존성 성공
}
```

### 8.2 동시성 패턴 요약

```
┌──────────────────────────────────────────────────────────┐
│                    동시성 패턴 매핑                        │
├──────────────────┬───────────────────────────────────────┤
│ 패턴             │ 용도                                   │
├──────────────────┼───────────────────────────────────────┤
│ sync.Mutex       │ changeLock: 그래프 변경 보호            │
│                  │ diagsLock: 진단 결과 맵 보호            │
│                  │ DepsLock: 개별 정점 의존성 보호          │
├──────────────────┼───────────────────────────────────────┤
│ chan struct{}     │ DoneCh: 정점 완료 신호 (close로 브로드캐스트) │
│                  │ CancelCh: 정점 취소 신호               │
│                  │ DepsUpdateCh: 의존성 변경 알림          │
├──────────────────┼───────────────────────────────────────┤
│ chan bool        │ DepsCh: 의존성 성공/실패 결과 전달       │
├──────────────────┼───────────────────────────────────────┤
│ sync.WaitGroup   │ wait: 모든 정점 완료 대기               │
├──────────────────┼───────────────────────────────────────┤
│ goroutine        │ walkVertex: 각 정점당 1개               │
│                  │ waitDeps: 의존성 변경 시 1개             │
└──────────────────┴───────────────────────────────────────┘
```

### 8.3 왜 Go 채널과 뮤텍스를 함께 사용하는가?

| 메커니즘 | 사용 사례 | 이유 |
|----------|-----------|------|
| 채널 | 의존성 대기, 완료 신호 | 1:N 통신 (close로 브로드캐스트) |
| 뮤텍스 | 공유 맵 보호 | 맵은 동시 쓰기에 안전하지 않음 |
| WaitGroup | 전체 완료 대기 | 총 고루틴 수를 동적으로 관리 |

Go의 일반적인 "채널을 통한 공유" 원칙과 "뮤텍스를 통한 보호" 원칙을 각 상황에 맞게 적절히 사용하고 있다.

### 8.4 고루틴 수

Walker는 `V*2` 개의 고루틴을 생성한다:
- 각 정점당 `walkVertex` 고루틴 1개
- 의존성이 있는 각 정점당 `waitDeps` 고루틴 1개

Walker 소스 주석에서도 이를 명시한다:
> "Walker will create V*2 goroutines (one for each vertex, and dependency waiter for each vertex)."

---

## 9. Set 자료구조와 해싱

### 9.1 Set 구현

```go
// internal/dag/set.go

type Set map[interface{}]interface{}

type Hashable interface {
    Hashcode() interface{}
}

func hashcode(v interface{}) interface{} {
    if h, ok := v.(Hashable); ok {
        return h.Hashcode()
    }
    return v  // Hashable이 아니면 값 자체를 키로 사용
}

func (s Set) Add(v interface{}) {
    s[hashcode(v)] = v
}

func (s Set) Delete(v interface{}) {
    delete(s, hashcode(v))
}

func (s Set) Include(v interface{}) bool {
    _, ok := s[hashcode(v)]
    return ok
}
```

### 9.2 Set 연산

```go
// Intersection: O(min(|s|, |other|))
func (s Set) Intersection(other Set) Set {
    result := make(Set)
    if other.Len() < s.Len() {
        s, other = other, s  // 작은 집합을 순회 (성능 최적화)
    }
    for _, v := range s {
        if other.Include(v) {
            result.Add(v)
        }
    }
    return result
}

// Difference: O(|s|)
func (s Set) Difference(other Set) Set {
    result := make(Set)
    for k, v := range s {
        if _, ok := other[k]; !ok {
            result.Add(v)
        }
    }
    return result
}
```

### 9.3 downEdgesNoCopy의 의미

```go
// internal/dag/graph.go

func (g *Graph) downEdgesNoCopy(v Vertex) Set {
    g.init()
    return g.downEdges[hashcode(v)]
}
```

`NoCopy`는 **방어적 복사를 하지 않고 내부 참조를 직접 반환**한다는 의미다. 성능을 위해 내부에서만 사용하며, 외부용 `DownEdges()`는 `Copy()`를 통해 안전한 복사본을 반환한다.

---

## 10. Edge와 Vertex 추상화

### 10.1 Vertex 인터페이스

```go
// internal/dag/graph.go

type Vertex interface{}  // 빈 인터페이스 — 모든 타입이 정점이 될 수 있음

type NamedVertex interface {
    Vertex
    Name() string  // 사람이 읽을 수 있는 이름
}
```

### 10.2 Edge 인터페이스

```go
// internal/dag/edge.go

type Edge interface {
    Source() Vertex
    Target() Vertex
    Hashable  // 중복 간선 방지를 위한 해시
}

type basicEdge struct {
    S, T Vertex
}

func (e *basicEdge) Hashcode() interface{} {
    return [...]interface{}{e.S, e.T}  // (source, target) 쌍을 해시코드로
}
```

`BasicEdge`의 해시코드는 `[2]interface{}{source, target}` 배열이다. Go에서 배열은 값 타입이므로 동일한 source-target 쌍은 동일한 해시코드를 가진다.

### 10.3 VertexName 함수

```go
// internal/dag/graph.go

func VertexName(raw Vertex) string {
    switch v := raw.(type) {
    case NamedVertex:
        return v.Name()
    case fmt.Stringer:
        return v.String()
    default:
        return fmt.Sprintf("%v", v)
    }
}
```

디버깅과 에러 메시지를 위해 정점 이름을 추출하는 유틸리티다. `NamedVertex`를 우선 사용하고, 없으면 `Stringer`를, 그것도 없으면 `%v` 포맷을 사용한다.

---

## 11. 그래프 순회 알고리즘들

### 11.1 범용 walk 메서드

```go
// internal/dag/dag.go

func (g *AcyclicGraph) walk(order walkType, test bool, start Set, f DepthWalkFunc) error {
    seen := make(map[Vertex]struct{})
    frontier := make([]vertexAtDepth, 0, len(start))

    for len(frontier) > 0 {
        var current vertexAtDepth

        switch {
        case order&depthFirst != 0:
            // DFS: 스택(LIFO) — 마지막 원소를 꺼냄
            n := len(frontier)
            current = frontier[n-1]
            frontier = frontier[:n-1]
        case order&breadthFirst != 0:
            // BFS: 큐(FIFO) — 첫 번째 원소를 꺼냄
            current = frontier[0]
            frontier = frontier[1:]
        }

        if _, ok := seen[current.Vertex]; ok {
            continue  // 이미 방문
        }
        seen[current.Vertex] = struct{}{}

        if err := f(current.Vertex, current.Depth); err != nil {
            switch err {
            case errStopWalk:
                return nil        // 전체 순회 중단
            case errStopWalkBranch:
                continue          // 현재 분기만 중단
            }
            return err
        }

        // 다음 방문 대상 추가
        var edges Set
        switch {
        case order&downOrder != 0:
            edges = g.downEdgesNoCopy(current.Vertex)
        case order&upOrder != 0:
            edges = g.upEdgesNoCopy(current.Vertex)
        }
        frontier = appendNext(frontier, edges, current.Depth+1)
    }
    return nil
}
```

### 11.2 4가지 순회 조합

```go
// internal/dag/dag.go

// 아래 방향 깊이 우선
func (g *AcyclicGraph) DepthFirstWalk(start Set, f DepthWalkFunc) error {
    return g.walk(depthFirst|downOrder, false, start, f)
}

// 위 방향 깊이 우선 (역방향)
func (g *AcyclicGraph) ReverseDepthFirstWalk(start Set, f DepthWalkFunc) error {
    return g.walk(depthFirst|upOrder, false, start, f)
}

// 아래 방향 너비 우선
func (g *AcyclicGraph) BreadthFirstWalk(start Set, f DepthWalkFunc) error {
    return g.walk(breadthFirst|downOrder, false, start, f)
}

// 위 방향 너비 우선 (역방향)
func (g *AcyclicGraph) ReverseBreadthFirstWalk(start Set, f DepthWalkFunc) error {
    return g.walk(breadthFirst|upOrder, false, start, f)
}
```

### 11.3 walkType 비트 플래그

```go
type walkType uint64

const (
    depthFirst   walkType = 1 << iota  // 1
    breadthFirst                        // 2
    downOrder                           // 4
    upOrder                             // 8
)
```

비트 플래그를 사용하여 순회 방식과 방향을 조합한다:

| 조합 | 값 | 의미 |
|------|----|------|
| depthFirst \| downOrder | 5 | DFS + 하향 |
| depthFirst \| upOrder | 9 | DFS + 상향 |
| breadthFirst \| downOrder | 6 | BFS + 하향 |
| breadthFirst \| upOrder | 10 | BFS + 상향 |

### 11.4 errStopWalk / errStopWalkBranch

```go
// internal/dag/dag.go

var (
    errStopWalkBranch = errors.New("stop walk branch")
    errStopWalk       = errors.New("stop walk")
)
```

순회 제어를 위한 센티널 에러:
- `errStopWalkBranch`: 현재 분기만 중단, 다른 분기 계속
- `errStopWalk`: 전체 순회 중단

이 패턴은 `FirstAncestorsWith`, `MatchAncestor` 등의 메서드에서 활용된다.

---

## 12. 왜 이렇게 설계했는가

### 12.1 왜 커스텀 DAG 구현인가?

범용 그래프 라이브러리를 사용하지 않고 자체 구현한 이유:

| 이유 | 설명 |
|------|------|
| 병렬 실행 통합 | 순회와 병렬 실행이 밀접하게 결합되어야 함 |
| 동적 그래프 수정 | 실행 중 그래프 변경 지원 필요 (DynamicExpand) |
| Terraform 특화 | 업스트림 실패 전파, 진단(Diagnostics) 통합 등 |
| 최소 의존성 | 외부 라이브러리 의존 없이 표준 라이브러리만 사용 |

### 12.2 왜 interface{} 기반 Set인가?

Go의 제네릭이 도입되기 전에 설계되었기 때문이다. `Set`은 `map[interface{}]interface{}`로 구현되어 모든 타입을 저장할 수 있지만, 타입 안전성은 런타임에 의존한다.

```go
// 현재 구현 (제네릭 이전 스타일)
type Set map[interface{}]interface{}

// 만약 제네릭으로 재작성한다면:
// type Set[T comparable] map[T]T
```

### 12.3 왜 close(chan)을 브로드캐스트로 사용하는가?

Go에서 `close(ch)`는 해당 채널에서 대기 중인 **모든 고루틴**을 깨운다. 이것은 1:N 알림에 완벽한 패턴이다:

```go
// DoneCh가 close되면, 이 정점에 의존하는 모든 정점이 깨어남
defer close(info.DoneCh)
```

뮤텍스와 조건 변수(`sync.Cond`)를 사용할 수도 있지만, 채널 close가 Go에서 더 관용적이고 간결하다.

### 12.4 왜 downEdges와 upEdges를 모두 유지하는가?

```
단방향 인접 리스트: downEdges만 있는 경우
  - "A에 의존하는 것은?" → O(V+E) 전체 탐색 필요

양방향 인접 리스트: downEdges + upEdges
  - "A에 의존하는 것은?" → O(1) upEdges[A] 조회
  - 메모리: 2배 (trade-off)
```

Terraform에서는 양방향 탐색이 빈번하다:
- `Ancestors()`: downEdges 따라 내려감
- `Descendants()`: upEdges 따라 올라감
- `Remove()`: 양쪽 모두에서 간선 제거

따라서 메모리를 약간 더 사용하는 대신 모든 방향의 탐색을 O(1)로 할 수 있는 것이 합리적인 선택이다.

### 12.5 Terraform 그래프에서의 역할

```
┌─────────────────────────────────────────────────────────────┐
│                    Terraform 실행 파이프라인                  │
│                                                              │
│  Config ──→ Graph Builder ──→ Graph Transformers ──→ DAG    │
│                                                     │        │
│                                              Validate()      │
│                                              TransitiveReduction()
│                                              Walk(callback)  │
│                                                     │        │
│                                              Provider calls  │
│                                              State updates   │
└─────────────────────────────────────────────────────────────┘
```

DAG 엔진은 Terraform의 **핵심 실행 런타임**이다. 모든 리소스 작업은 이 그래프 위에서 의존성 순서를 따라 실행된다. 이 장에서 분석한 모든 알고리즘(위상 정렬, 사이클 검출, 이행적 축소, 병렬 워크)이 합쳐져 Terraform의 안전하고 효율적인 인프라 관리를 가능하게 한다.

---

## 요약

```
┌──────────────────────────────────────────────────────────┐
│  DAG 그래프 엔진 핵심 구성요소                              │
├────────────────┬─────────────────────────────────────────┤
│ Graph          │ 방향 그래프 기본 구조 (양방향 인접 리스트)  │
│ AcyclicGraph   │ DAG 특화 연산 (위상 정렬, 사이클 검출)     │
│ Walker         │ 병렬 실행 엔진 (고루틴 + 채널)             │
│ StronglyConnected │ Tarjan SCC (사이클 검출)               │
│ Set            │ 해시 기반 집합 자료구조                    │
│ Edge/Vertex    │ 인터페이스 기반 추상화                     │
└────────────────┴─────────────────────────────────────────┘
```
