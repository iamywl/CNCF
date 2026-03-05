package main

import (
	"fmt"
	"math"
	"math/rand"
	"strings"
	"time"
)

// ============================================================
// Grafana 서버사이드 표현식(SSE) 엔진 시뮬레이션
// DAG 기반 파이프라인: 데이터소스 → 수학 → 리듀스 → 임계값
// ============================================================

// --- 데이터 구조 ---

// TimeSeriesPoint는 시계열 데이터의 한 점.
type TimeSeriesPoint struct {
	Timestamp time.Time
	Value     float64
}

// Result는 노드 실행 결과를 나타낸다.
type Result struct {
	Series []TimeSeriesPoint // 시계열 데이터 (DatasourceNode, MathNode)
	Scalar *float64          // 스칼라 값 (ReduceNode, ThresholdNode)
	Error  error
}

// HasSeries는 시계열 데이터가 있는지 확인한다.
func (r *Result) HasSeries() bool { return len(r.Series) > 0 }

// HasScalar는 스칼라 값이 있는지 확인한다.
func (r *Result) HasScalar() bool { return r.Scalar != nil }

// ScalarValue는 스칼라 값을 반환하거나, 시계열의 마지막 값을 반환한다.
func (r *Result) ScalarValue() float64 {
	if r.Scalar != nil {
		return *r.Scalar
	}
	if len(r.Series) > 0 {
		return r.Series[len(r.Series)-1].Value
	}
	return 0
}

// --- Node 인터페이스 ---

// Node는 표현식 파이프라인의 실행 단위를 나타낸다.
// 실제 구현: pkg/expr/nodes.go
type Node interface {
	ID() string              // 노드 식별자 (A, B, C, ...)
	NodeType() string        // 노드 타입 (datasource, math, reduce, threshold)
	Dependencies() []string  // 의존하는 노드 ID 목록
	Execute(vars map[string]*Result) *Result
	String() string          // 사람이 읽을 수 있는 설명
}

// --- DatasourceNode ---

// DatasourceNode는 데이터 소스 쿼리를 시뮬레이션한다.
// 실제 구현: pkg/expr/classic.go, pkg/expr/nodes.go
type DatasourceNode struct {
	id         string
	label      string
	pointCount int
	baseValue  float64
	variance   float64
}

func NewDatasourceNode(id, label string, pointCount int, base, variance float64) *DatasourceNode {
	return &DatasourceNode{
		id:         id,
		label:      label,
		pointCount: pointCount,
		baseValue:  base,
		variance:   variance,
	}
}

func (n *DatasourceNode) ID() string              { return n.id }
func (n *DatasourceNode) NodeType() string         { return "datasource" }
func (n *DatasourceNode) Dependencies() []string   { return nil }
func (n *DatasourceNode) String() string {
	return fmt.Sprintf("DatasourceNode(%s: %s, %d points, base=%.1f)",
		n.id, n.label, n.pointCount, n.baseValue)
}

func (n *DatasourceNode) Execute(vars map[string]*Result) *Result {
	rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(n.id[0])))
	now := time.Now()
	series := make([]TimeSeriesPoint, n.pointCount)

	for i := 0; i < n.pointCount; i++ {
		series[i] = TimeSeriesPoint{
			Timestamp: now.Add(time.Duration(-n.pointCount+i+1) * time.Minute),
			Value:     n.baseValue + (rng.Float64()-0.5)*2*n.variance,
		}
	}

	return &Result{Series: series}
}

// --- MathNode ---

// MathOp는 수학 연산 타입.
type MathOp int

const (
	MathAdd MathOp = iota // A + B
	MathSub               // A - B
	MathMul               // A * B
	MathDiv               // A / B
	MathAbs               // abs(A)
	MathScalarMul         // A * scalar
)

// MathNode는 수학 연산을 수행한다.
// 실제 구현: pkg/expr/mathexp/
type MathNode struct {
	id       string
	op       MathOp
	deps     []string // 입력 노드 ID (1개 또는 2개)
	scalar   float64  // MathScalarMul 시 사용
	expr     string   // 표현식 문자열 (표시용)
}

func NewMathNode(id string, op MathOp, deps []string, scalar float64, expr string) *MathNode {
	return &MathNode{id: id, op: op, deps: deps, scalar: scalar, expr: expr}
}

func (n *MathNode) ID() string              { return n.id }
func (n *MathNode) NodeType() string         { return "math" }
func (n *MathNode) Dependencies() []string   { return n.deps }
func (n *MathNode) String() string {
	return fmt.Sprintf("MathNode(%s: %s)", n.id, n.expr)
}

func (n *MathNode) Execute(vars map[string]*Result) *Result {
	aResult, ok := vars[n.deps[0]]
	if !ok || aResult.Error != nil {
		return &Result{Error: fmt.Errorf("의존 노드 %s 결과 없음", n.deps[0])}
	}

	switch n.op {
	case MathAbs:
		series := make([]TimeSeriesPoint, len(aResult.Series))
		for i, p := range aResult.Series {
			series[i] = TimeSeriesPoint{Timestamp: p.Timestamp, Value: math.Abs(p.Value)}
		}
		return &Result{Series: series}

	case MathScalarMul:
		series := make([]TimeSeriesPoint, len(aResult.Series))
		for i, p := range aResult.Series {
			series[i] = TimeSeriesPoint{Timestamp: p.Timestamp, Value: p.Value * n.scalar}
		}
		return &Result{Series: series}
	}

	// 이항 연산: 두 시리즈 필요
	if len(n.deps) < 2 {
		return &Result{Error: fmt.Errorf("이항 연산에 2개 입력 필요")}
	}

	bResult, ok := vars[n.deps[1]]
	if !ok || bResult.Error != nil {
		return &Result{Error: fmt.Errorf("의존 노드 %s 결과 없음", n.deps[1])}
	}

	// 길이가 다르면 짧은 쪽에 맞춤
	length := len(aResult.Series)
	if len(bResult.Series) < length {
		length = len(bResult.Series)
	}

	series := make([]TimeSeriesPoint, length)
	for i := 0; i < length; i++ {
		var val float64
		a := aResult.Series[i].Value
		b := bResult.Series[i].Value

		switch n.op {
		case MathAdd:
			val = a + b
		case MathSub:
			val = a - b
		case MathMul:
			val = a * b
		case MathDiv:
			if b == 0 {
				val = math.NaN()
			} else {
				val = a / b
			}
		}
		series[i] = TimeSeriesPoint{Timestamp: aResult.Series[i].Timestamp, Value: val}
	}

	return &Result{Series: series}
}

// --- ReduceNode ---

// ReduceFunc는 집계 함수 타입.
type ReduceFunc string

const (
	ReduceMean  ReduceFunc = "mean"
	ReduceMax   ReduceFunc = "max"
	ReduceMin   ReduceFunc = "min"
	ReduceSum   ReduceFunc = "sum"
	ReduceLast  ReduceFunc = "last"
	ReduceCount ReduceFunc = "count"
)

// ReduceNode는 시계열을 스칼라로 집계한다.
// 실제 구현: pkg/expr/reduce.go
type ReduceNode struct {
	id      string
	fn      ReduceFunc
	inputID string
}

func NewReduceNode(id string, fn ReduceFunc, inputID string) *ReduceNode {
	return &ReduceNode{id: id, fn: fn, inputID: inputID}
}

func (n *ReduceNode) ID() string              { return n.id }
func (n *ReduceNode) NodeType() string         { return "reduce" }
func (n *ReduceNode) Dependencies() []string   { return []string{n.inputID} }
func (n *ReduceNode) String() string {
	return fmt.Sprintf("ReduceNode(%s: %s($%s))", n.id, n.fn, n.inputID)
}

func (n *ReduceNode) Execute(vars map[string]*Result) *Result {
	input, ok := vars[n.inputID]
	if !ok || input.Error != nil {
		return &Result{Error: fmt.Errorf("의존 노드 %s 결과 없음", n.inputID)}
	}

	if len(input.Series) == 0 {
		return &Result{Error: fmt.Errorf("빈 시리즈")}
	}

	var result float64

	switch n.fn {
	case ReduceMean:
		sum := 0.0
		for _, p := range input.Series {
			sum += p.Value
		}
		result = sum / float64(len(input.Series))

	case ReduceMax:
		result = input.Series[0].Value
		for _, p := range input.Series[1:] {
			if p.Value > result {
				result = p.Value
			}
		}

	case ReduceMin:
		result = input.Series[0].Value
		for _, p := range input.Series[1:] {
			if p.Value < result {
				result = p.Value
			}
		}

	case ReduceSum:
		for _, p := range input.Series {
			result += p.Value
		}

	case ReduceLast:
		result = input.Series[len(input.Series)-1].Value

	case ReduceCount:
		result = float64(len(input.Series))
	}

	return &Result{Scalar: &result}
}

// --- ThresholdNode ---

// ThresholdOp는 임계값 비교 연산자.
type ThresholdOp string

const (
	ThresholdGT  ThresholdOp = ">"
	ThresholdLT  ThresholdOp = "<"
	ThresholdGTE ThresholdOp = ">="
	ThresholdLTE ThresholdOp = "<="
	ThresholdEQ  ThresholdOp = "=="
)

// ThresholdNode는 임계값 비교를 수행한다.
// 결과: 1 (조건 충족, firing) 또는 0 (정상).
// 실제 구현: pkg/expr/threshold.go
type ThresholdNode struct {
	id        string
	inputID   string
	op        ThresholdOp
	threshold float64
}

func NewThresholdNode(id, inputID string, op ThresholdOp, threshold float64) *ThresholdNode {
	return &ThresholdNode{id: id, inputID: inputID, op: op, threshold: threshold}
}

func (n *ThresholdNode) ID() string              { return n.id }
func (n *ThresholdNode) NodeType() string         { return "threshold" }
func (n *ThresholdNode) Dependencies() []string   { return []string{n.inputID} }
func (n *ThresholdNode) String() string {
	return fmt.Sprintf("ThresholdNode(%s: $%s %s %.1f)", n.id, n.inputID, n.op, n.threshold)
}

func (n *ThresholdNode) Execute(vars map[string]*Result) *Result {
	input, ok := vars[n.inputID]
	if !ok || input.Error != nil {
		return &Result{Error: fmt.Errorf("의존 노드 %s 결과 없음", n.inputID)}
	}

	value := input.ScalarValue()
	var firing bool

	switch n.op {
	case ThresholdGT:
		firing = value > n.threshold
	case ThresholdLT:
		firing = value < n.threshold
	case ThresholdGTE:
		firing = value >= n.threshold
	case ThresholdLTE:
		firing = value <= n.threshold
	case ThresholdEQ:
		firing = value == n.threshold
	}

	var result float64
	if firing {
		result = 1
	}
	return &Result{Scalar: &result}
}

// --- DataPipeline (DAG) ---

// DataPipeline은 노드들의 DAG 기반 실행 파이프라인.
// 실제 구현: pkg/expr/service.go
type DataPipeline struct {
	nodes    map[string]Node
	order    []string // 위상 정렬된 실행 순서
	results  map[string]*Result
}

func NewDataPipeline() *DataPipeline {
	return &DataPipeline{
		nodes:   make(map[string]Node),
		results: make(map[string]*Result),
	}
}

// AddNode는 노드를 파이프라인에 추가한다.
func (p *DataPipeline) AddNode(node Node) {
	p.nodes[node.ID()] = node
}

// TopologicalSort는 위상 정렬을 수행한다.
// Kahn의 알고리즘 사용.
func (p *DataPipeline) TopologicalSort() ([]string, error) {
	// 진입 차수(in-degree) 계산
	inDegree := make(map[string]int)
	for id := range p.nodes {
		inDegree[id] = 0
	}
	for _, node := range p.nodes {
		for _, dep := range node.Dependencies() {
			inDegree[node.ID()]++ // dep → node 간선
			_ = dep
		}
	}

	// 재계산: 각 노드의 의존성 수 = 진입 차수
	for id := range p.nodes {
		inDegree[id] = len(p.nodes[id].Dependencies())
	}

	// 진입 차수 0인 노드부터 시작
	var queue []string
	for id, deg := range inDegree {
		if deg == 0 {
			queue = append(queue, id)
		}
	}

	// 정렬 안정성을 위해 알파벳순
	sortStrings(queue)

	var order []string
	for len(queue) > 0 {
		// 큐에서 꺼냄
		current := queue[0]
		queue = queue[1:]
		order = append(order, current)

		// current에 의존하는 노드의 진입 차수 감소
		for id, node := range p.nodes {
			for _, dep := range node.Dependencies() {
				if dep == current {
					inDegree[id]--
					if inDegree[id] == 0 {
						queue = append(queue, id)
						sortStrings(queue)
					}
				}
			}
		}
	}

	if len(order) != len(p.nodes) {
		return nil, fmt.Errorf("순환 의존성 감지: 정렬된 %d개 / 전체 %d개", len(order), len(p.nodes))
	}

	p.order = order
	return order, nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// Execute는 파이프라인을 실행한다.
func (p *DataPipeline) Execute() error {
	if len(p.order) == 0 {
		if _, err := p.TopologicalSort(); err != nil {
			return err
		}
	}

	for _, nodeID := range p.order {
		node := p.nodes[nodeID]

		// 의존 노드 실패 확인
		skip := false
		for _, dep := range node.Dependencies() {
			if r, ok := p.results[dep]; ok && r.Error != nil {
				p.results[nodeID] = &Result{
					Error: fmt.Errorf("의존 노드 %s 실패로 스킵", dep),
				}
				skip = true
				break
			}
		}
		if skip {
			continue
		}

		// 노드 실행
		result := node.Execute(p.results)
		p.results[nodeID] = result
	}

	return nil
}

// GetResult는 특정 노드의 실행 결과를 반환한다.
func (p *DataPipeline) GetResult(nodeID string) *Result {
	return p.results[nodeID]
}

// --- 출력 헬퍼 ---

func formatSeries(series []TimeSeriesPoint, maxShow int) string {
	if len(series) == 0 {
		return "[]"
	}
	n := maxShow
	if n > len(series) {
		n = len(series)
	}
	var parts []string
	for i := 0; i < n; i++ {
		parts = append(parts, fmt.Sprintf("%.1f", series[i].Value))
	}
	result := "[" + strings.Join(parts, ", ")
	if len(series) > maxShow {
		result += ", ..."
	}
	result += "]"
	return result
}

// --- 메인: 시뮬레이션 ---

func main() {
	fmt.Println("=== Grafana 표현식 엔진 시뮬레이션 ===")

	// ------------------------------------------
	// 1. 파이프라인 구성
	// ------------------------------------------
	fmt.Println("\n--- 1. DAG 파이프라인 구성 ---")
	fmt.Println()

	pipeline := NewDataPipeline()

	// Node A: CPU 메트릭 (데이터소스)
	nodeA := NewDatasourceNode("A", "CPU Usage (%)", 10, 45.0, 15.0)
	pipeline.AddNode(nodeA)
	fmt.Printf("  %s\n", nodeA)

	// Node B: Memory 메트릭 (데이터소스)
	nodeB := NewDatasourceNode("B", "Memory Usage (%)", 10, 30.0, 10.0)
	pipeline.AddNode(nodeB)
	fmt.Printf("  %s\n", nodeB)

	// Node C: A + B (수학 연산)
	nodeC := NewMathNode("C", MathAdd, []string{"A", "B"}, 0, "$A + $B")
	pipeline.AddNode(nodeC)
	fmt.Printf("  %s\n", nodeC)

	// Node D: mean(C) (리듀스)
	nodeD := NewReduceNode("D", ReduceMean, "C")
	pipeline.AddNode(nodeD)
	fmt.Printf("  %s\n", nodeD)

	// Node E: D > 50 (임계값)
	nodeE := NewThresholdNode("E", "D", ThresholdGT, 50.0)
	pipeline.AddNode(nodeE)
	fmt.Printf("  %s\n", nodeE)

	// ------------------------------------------
	// 2. 위상 정렬
	// ------------------------------------------
	fmt.Println("\n--- 2. 위상 정렬 ---")
	fmt.Println()

	order, err := pipeline.TopologicalSort()
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
		return
	}

	// 의존성 그래프 출력
	fmt.Println("의존성 그래프:")
	for _, nodeID := range order {
		node := pipeline.nodes[nodeID]
		deps := node.Dependencies()
		if len(deps) == 0 {
			fmt.Printf("  %s: (입력 없음)\n", nodeID)
		} else {
			fmt.Printf("  %s: ← %s\n", nodeID, strings.Join(deps, ", "))
		}
	}

	fmt.Printf("\n실행 순서: %s\n", strings.Join(order, " → "))

	// ------------------------------------------
	// 3. 파이프라인 실행
	// ------------------------------------------
	fmt.Println("\n--- 3. 파이프라인 실행 ---")
	fmt.Println()

	err = pipeline.Execute()
	if err != nil {
		fmt.Printf("  실행 오류: %v\n", err)
		return
	}

	// 실행 결과 출력
	for _, nodeID := range order {
		result := pipeline.GetResult(nodeID)
		node := pipeline.nodes[nodeID]

		fmt.Printf("[%s] %s\n", nodeID, node.String())

		if result.Error != nil {
			fmt.Printf("     오류: %v\n", result.Error)
			continue
		}

		if result.HasSeries() {
			fmt.Printf("     시리즈: %d개 포인트 %s\n",
				len(result.Series), formatSeries(result.Series, 5))
		}

		if result.HasScalar() {
			fmt.Printf("     스칼라: %.2f\n", *result.Scalar)
			if node.NodeType() == "threshold" {
				if *result.Scalar == 1 {
					fmt.Printf("     상태: FIRING (임계값 초과)\n")
				} else {
					fmt.Printf("     상태: NORMAL (정상)\n")
				}
			}
		}
		fmt.Println()
	}

	// ------------------------------------------
	// 4. 다양한 수학 표현식
	// ------------------------------------------
	fmt.Println("--- 4. 다양한 수학 표현식 ---")
	fmt.Println()

	// A * 2
	fmt.Println("[테스트] $A * 2")
	pipeline2 := NewDataPipeline()
	pipeline2.AddNode(NewDatasourceNode("A", "Metric", 5, 10.0, 5.0))
	pipeline2.AddNode(NewMathNode("B", MathScalarMul, []string{"A"}, 2.0, "$A * 2"))
	pipeline2.TopologicalSort()
	pipeline2.Execute()
	rA := pipeline2.GetResult("A")
	rB := pipeline2.GetResult("B")
	fmt.Printf("  A: %s\n", formatSeries(rA.Series, 5))
	fmt.Printf("  B: %s\n", formatSeries(rB.Series, 5))

	// abs(A - B)
	fmt.Println("\n[테스트] abs($A - $B)")
	pipeline3 := NewDataPipeline()
	pipeline3.AddNode(NewDatasourceNode("A", "Metric A", 5, 50.0, 30.0))
	pipeline3.AddNode(NewDatasourceNode("B", "Metric B", 5, 50.0, 30.0))
	pipeline3.AddNode(NewMathNode("C", MathSub, []string{"A", "B"}, 0, "$A - $B"))
	pipeline3.AddNode(NewMathNode("D", MathAbs, []string{"C"}, 0, "abs($C)"))
	pipeline3.TopologicalSort()
	pipeline3.Execute()
	fmt.Printf("  A:     %s\n", formatSeries(pipeline3.GetResult("A").Series, 5))
	fmt.Printf("  B:     %s\n", formatSeries(pipeline3.GetResult("B").Series, 5))
	fmt.Printf("  A - B: %s\n", formatSeries(pipeline3.GetResult("C").Series, 5))
	fmt.Printf("  abs:   %s\n", formatSeries(pipeline3.GetResult("D").Series, 5))

	// ------------------------------------------
	// 5. 다양한 리듀스 함수
	// ------------------------------------------
	fmt.Println("\n--- 5. 리듀스 함수 비교 ---")
	fmt.Println()

	reduceFuncs := []ReduceFunc{ReduceMean, ReduceMax, ReduceMin, ReduceSum, ReduceLast, ReduceCount}
	pipeline4 := NewDataPipeline()
	pipeline4.AddNode(NewDatasourceNode("A", "Sample", 10, 50.0, 20.0))
	for i, fn := range reduceFuncs {
		id := string(rune('B' + i))
		pipeline4.AddNode(NewReduceNode(id, fn, "A"))
	}
	pipeline4.TopologicalSort()
	pipeline4.Execute()

	fmt.Printf("  입력 시리즈: %s\n\n", formatSeries(pipeline4.GetResult("A").Series, 10))
	for i, fn := range reduceFuncs {
		id := string(rune('B' + i))
		result := pipeline4.GetResult(id)
		if result.HasScalar() {
			fmt.Printf("  %-8s = %.2f\n", fn, *result.Scalar)
		}
	}

	// ------------------------------------------
	// 6. 실패 전파 테스트
	// ------------------------------------------
	fmt.Println("\n--- 6. 실패 전파 테스트 ---")
	fmt.Println()

	pipeline5 := NewDataPipeline()
	pipeline5.AddNode(NewDatasourceNode("A", "Valid", 5, 10.0, 5.0))
	// B는 존재하지 않는 노드 X에 의존 → 실패
	pipeline5.AddNode(NewMathNode("C", MathAdd, []string{"A", "X"}, 0, "$A + $X"))
	// D는 C에 의존 → C 실패로 D도 스킵
	pipeline5.AddNode(NewReduceNode("D", ReduceMean, "C"))

	pipeline5.TopologicalSort()
	pipeline5.Execute()

	for _, id := range []string{"A", "C", "D"} {
		result := pipeline5.GetResult(id)
		if result == nil {
			fmt.Printf("  [%s] 결과 없음\n", id)
			continue
		}
		if result.Error != nil {
			fmt.Printf("  [%s] 실패: %v\n", id, result.Error)
		} else if result.HasSeries() {
			fmt.Printf("  [%s] 성공: %d개 포인트\n", id, len(result.Series))
		} else if result.HasScalar() {
			fmt.Printf("  [%s] 성공: %.2f\n", id, *result.Scalar)
		}
	}

	// ------------------------------------------
	// 요약
	// ------------------------------------------
	fmt.Println("\n--- 시뮬레이션 요약 ---")
	fmt.Println()
	fmt.Println("표현식 엔진 구성요소:")
	fmt.Println("  1. Node 인터페이스: ID(), Dependencies(), Execute(vars)")
	fmt.Println("  2. DatasourceNode: 데이터 소스에서 시계열 데이터 쿼리")
	fmt.Println("  3. MathNode: 시계열 간 수학 연산 (+, -, *, /, abs, scalar)")
	fmt.Println("  4. ReduceNode: 시계열 → 스칼라 집계 (mean, max, min, sum, last)")
	fmt.Println("  5. ThresholdNode: 스칼라 → 0/1 (임계값 비교)")
	fmt.Println("  6. DataPipeline: DAG 기반 위상 정렬 → 순서대로 실행")
	fmt.Println()
	fmt.Println("실행 흐름:")
	fmt.Println("  Datasource → Math → Reduce → Threshold")
	fmt.Println("  각 노드는 vars map을 통해 이전 노드 결과를 참조")
	fmt.Println("  의존 노드 실패 시 하위 노드 자동 스킵")
}
