package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// =============================================================================
// Jaeger 서비스 의존성 DAG 분석 시뮬레이터
// =============================================================================
//
// Jaeger는 수집된 트레이스 데이터로부터 서비스 간 의존성 그래프(DAG)를 구축한다.
// 이 PoC는 다음을 시뮬레이션한다:
//
// 1. 트레이스 데이터에서 서비스 의존성 그래프 구축
// 2. 서비스 간 호출 횟수 추적 (parent -> child)
// 3. 순환 의존성(circular dependency) 감지
// 4. 크리티컬 패스(최장 지연 경로) 탐색
// 5. 의존성 그래프 ASCII 시각화
// 6. 서비스별 통계 (인입/발신 호출 수, 에러율)
//
// Jaeger 소스에서 dependency store는 spark-dependencies 또는
// jaeger-query의 /api/dependencies 엔드포인트를 통해 제공된다.
// 내부적으로 DependencyLink 구조체를 사용하여 parent-child 관계를 표현한다.
// =============================================================================

// --- 데이터 모델 ---

// Span은 단일 작업 단위를 나타낸다.
type Span struct {
	TraceID     string
	SpanID      string
	ParentID    string // 루트 스팬이면 빈 문자열
	ServiceName string
	Operation   string
	StartTime   time.Time
	Duration    time.Duration
	HasError    bool
}

// DependencyLink는 두 서비스 간의 의존성 관계를 나타낸다.
// Jaeger의 model.DependencyLink에 대응한다.
type DependencyLink struct {
	Parent    string // 호출하는 서비스
	Child     string // 호출받는 서비스
	CallCount int64  // 호출 횟수
	ErrorRate float64 // 에러율 (0.0 ~ 1.0)
	// 내부 추적용
	errorCount int64
}

// ServiceStats는 서비스별 통계 정보를 저장한다.
type ServiceStats struct {
	ServiceName   string
	IncomingCalls int64   // 이 서비스를 호출한 횟수
	OutgoingCalls int64   // 이 서비스가 호출한 횟수
	ErrorRate     float64 // 평균 에러율
	AvgLatency    time.Duration
	// 내부 추적용
	totalLatency time.Duration
	spanCount    int64
	errorCount   int64
}

// DependencyGraph는 서비스 의존성 DAG를 나타낸다.
type DependencyGraph struct {
	// adjacency: parent -> [child -> DependencyLink]
	adjacency map[string]map[string]*DependencyLink
	// services: 모든 서비스 목록
	services map[string]*ServiceStats
	// latencyMatrix: (parent, child) -> 평균 지연시간
	latencyMatrix map[string]map[string]time.Duration
}

// NewDependencyGraph는 새로운 의존성 그래프를 생성한다.
func NewDependencyGraph() *DependencyGraph {
	return &DependencyGraph{
		adjacency:     make(map[string]map[string]*DependencyLink),
		services:      make(map[string]*ServiceStats),
		latencyMatrix: make(map[string]map[string]time.Duration),
	}
}

// --- 의존성 그래프 구축 ---

// BuildFromTraces는 트레이스 데이터로부터 의존성 그래프를 구축한다.
// Jaeger의 BuildDependencies 함수에 대응한다.
// 핵심 로직: 같은 트레이스 내에서 parent span과 child span의 서비스가 다르면
// 의존성 관계로 기록한다.
func (g *DependencyGraph) BuildFromTraces(traces map[string][]Span) {
	for _, spans := range traces {
		// spanID -> Span 맵 구축
		spanMap := make(map[string]*Span)
		for i := range spans {
			spanMap[spans[i].SpanID] = &spans[i]
		}

		// 각 스팬에 대해 부모-자식 관계 확인
		for _, span := range spans {
			g.ensureService(span.ServiceName)
			g.updateServiceStats(&span)

			if span.ParentID == "" {
				continue
			}

			parentSpan, exists := spanMap[span.ParentID]
			if !exists {
				continue
			}

			// 같은 서비스 내 호출은 의존성이 아님
			if parentSpan.ServiceName == span.ServiceName {
				continue
			}

			g.addDependency(parentSpan.ServiceName, span.ServiceName, span.HasError, span.Duration)
		}
	}

	// 최종 통계 계산
	g.finalizeStats()
}

// ensureService는 서비스가 통계 맵에 있는지 확인하고 없으면 추가한다.
func (g *DependencyGraph) ensureService(name string) {
	if _, exists := g.services[name]; !exists {
		g.services[name] = &ServiceStats{ServiceName: name}
	}
}

// updateServiceStats는 스팬 정보를 바탕으로 서비스 통계를 업데이트한다.
func (g *DependencyGraph) updateServiceStats(span *Span) {
	stats := g.services[span.ServiceName]
	stats.totalLatency += span.Duration
	stats.spanCount++
	if span.HasError {
		stats.errorCount++
	}
}

// addDependency는 의존성 관계를 그래프에 추가한다.
func (g *DependencyGraph) addDependency(parent, child string, hasError bool, latency time.Duration) {
	if _, exists := g.adjacency[parent]; !exists {
		g.adjacency[parent] = make(map[string]*DependencyLink)
	}

	link, exists := g.adjacency[parent][child]
	if !exists {
		link = &DependencyLink{
			Parent: parent,
			Child:  child,
		}
		g.adjacency[parent][child] = link
	}

	link.CallCount++
	if hasError {
		link.errorCount++
	}

	// 지연시간 추적
	if _, exists := g.latencyMatrix[parent]; !exists {
		g.latencyMatrix[parent] = make(map[string]time.Duration)
	}
	// 간단히 마지막 값을 누적하여 평균 계산용으로 사용
	existing := g.latencyMatrix[parent][child]
	g.latencyMatrix[parent][child] = (existing*time.Duration(link.CallCount-1) + latency) / time.Duration(link.CallCount)

	// 서비스 호출 횟수 업데이트
	g.services[parent].OutgoingCalls++
	g.services[child].IncomingCalls++
}

// finalizeStats는 모든 통계를 최종 계산한다.
func (g *DependencyGraph) finalizeStats() {
	// 에러율 계산
	for _, links := range g.adjacency {
		for _, link := range links {
			if link.CallCount > 0 {
				link.ErrorRate = float64(link.errorCount) / float64(link.CallCount)
			}
		}
	}

	// 서비스별 평균 지연시간과 에러율
	for _, stats := range g.services {
		if stats.spanCount > 0 {
			stats.AvgLatency = stats.totalLatency / time.Duration(stats.spanCount)
			stats.ErrorRate = float64(stats.errorCount) / float64(stats.spanCount)
		}
	}
}

// --- 순환 의존성 감지 ---

// CycleInfo는 감지된 순환 의존성 정보를 담는다.
type CycleInfo struct {
	Path []string // 순환 경로의 서비스 목록
}

// DetectCycles는 DFS를 사용하여 순환 의존성을 감지한다.
// 그래프에서 back edge를 찾아 순환을 식별한다.
func (g *DependencyGraph) DetectCycles() []CycleInfo {
	var cycles []CycleInfo
	visited := make(map[string]bool)
	inStack := make(map[string]bool)
	path := make([]string, 0)

	var dfs func(node string)
	dfs = func(node string) {
		visited[node] = true
		inStack[node] = true
		path = append(path, node)

		if children, exists := g.adjacency[node]; exists {
			for child := range children {
				if !visited[child] {
					dfs(child)
				} else if inStack[child] {
					// 순환 감지: child가 현재 스택에 있음
					cycleStart := -1
					for i, n := range path {
						if n == child {
							cycleStart = i
							break
						}
					}
					if cycleStart >= 0 {
						cyclePath := make([]string, len(path[cycleStart:]))
						copy(cyclePath, path[cycleStart:])
						cyclePath = append(cyclePath, child) // 순환 완성
						cycles = append(cycles, CycleInfo{Path: cyclePath})
					}
				}
			}
		}

		path = path[:len(path)-1]
		inStack[node] = false
	}

	for service := range g.services {
		if !visited[service] {
			dfs(service)
		}
	}

	return cycles
}

// --- 크리티컬 패스 분석 ---

// CriticalPathResult는 크리티컬 패스 분석 결과를 담는다.
type CriticalPathResult struct {
	Path         []string      // 경로상의 서비스 목록
	TotalLatency time.Duration // 경로의 총 지연시간
}

// FindCriticalPath는 가장 긴 지연시간을 가진 경로를 찾는다.
// DAG에서의 최장 경로 문제 — 위상 정렬 후 DP로 해결한다.
func (g *DependencyGraph) FindCriticalPath() CriticalPathResult {
	// 진입 차수 계산
	inDegree := make(map[string]int)
	for service := range g.services {
		inDegree[service] = 0
	}
	for _, children := range g.adjacency {
		for child := range children {
			inDegree[child]++
		}
	}

	// 위상 정렬 (Kahn's algorithm)
	var queue []string
	for service, degree := range inDegree {
		if degree == 0 {
			queue = append(queue, service)
		}
	}

	var topoOrder []string
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		topoOrder = append(topoOrder, node)

		if children, exists := g.adjacency[node]; exists {
			for child := range children {
				inDegree[child]--
				if inDegree[child] == 0 {
					queue = append(queue, child)
				}
			}
		}
	}

	// 순환이 있으면 위상 정렬이 불완전 — 가능한 범위만 분석
	if len(topoOrder) < len(g.services) {
		fmt.Println("  [경고] 순환 의존성으로 인해 일부 서비스가 위상 정렬에 포함되지 않음")
	}

	// DP: 각 노드까지의 최장 경로 계산
	dist := make(map[string]time.Duration)
	prev := make(map[string]string) // 역추적용

	for _, service := range topoOrder {
		if children, exists := g.adjacency[service]; exists {
			for child, link := range children {
				edgeLatency := g.latencyMatrix[link.Parent][link.Child]
				newDist := dist[service] + edgeLatency
				if newDist > dist[child] {
					dist[child] = newDist
					prev[child] = service
				}
			}
		}
	}

	// 최장 거리를 가진 노드 찾기
	var maxNode string
	var maxDist time.Duration
	for node, d := range dist {
		if d > maxDist {
			maxDist = d
			maxNode = node
		}
	}

	// 경로 역추적
	var path []string
	current := maxNode
	for current != "" {
		path = append([]string{current}, path...)
		current = prev[current]
	}

	return CriticalPathResult{
		Path:         path,
		TotalLatency: maxDist,
	}
}

// --- ASCII 시각화 ---

// VisualizeASCII는 의존성 그래프를 ASCII 아트로 시각화한다.
func (g *DependencyGraph) VisualizeASCII() string {
	var sb strings.Builder

	// 서비스를 정렬하여 일관된 출력
	services := make([]string, 0, len(g.services))
	for s := range g.services {
		services = append(services, s)
	}
	sort.Strings(services)

	// 인접 행렬 형태로 출력
	sb.WriteString("\n=== 서비스 의존성 그래프 (인접 행렬) ===\n\n")

	// 서비스 이름 최대 길이
	maxLen := 0
	for _, s := range services {
		if len(s) > maxLen {
			maxLen = len(s)
		}
	}
	if maxLen < 8 {
		maxLen = 8
	}

	// 헤더
	sb.WriteString(strings.Repeat(" ", maxLen+2))
	for _, s := range services {
		sb.WriteString(fmt.Sprintf("%-*s", maxLen+2, s))
	}
	sb.WriteString("\n")

	// 구분선
	totalWidth := (maxLen + 2) * (len(services) + 1)
	sb.WriteString(strings.Repeat("-", totalWidth))
	sb.WriteString("\n")

	// 각 행 (parent)
	for _, parent := range services {
		sb.WriteString(fmt.Sprintf("%-*s", maxLen+2, parent))
		for _, child := range services {
			if children, exists := g.adjacency[parent]; exists {
				if link, exists := children[child]; exists {
					sb.WriteString(fmt.Sprintf("%-*s", maxLen+2, fmt.Sprintf("%d", link.CallCount)))
				} else {
					sb.WriteString(fmt.Sprintf("%-*s", maxLen+2, "."))
				}
			} else {
				sb.WriteString(fmt.Sprintf("%-*s", maxLen+2, "."))
			}
		}
		sb.WriteString("\n")
	}

	// 트리 형태 시각화
	sb.WriteString("\n=== 서비스 의존성 트리 ===\n\n")

	// 루트 서비스 찾기 (인입 호출이 없는 서비스)
	roots := make([]string, 0)
	for _, s := range services {
		if g.services[s].IncomingCalls == 0 {
			roots = append(roots, s)
		}
	}

	if len(roots) == 0 {
		// 순환만 있는 경우 첫 서비스를 루트로
		if len(services) > 0 {
			roots = append(roots, services[0])
		}
	}

	visited := make(map[string]bool)
	for _, root := range roots {
		g.printTree(&sb, root, "", true, visited)
	}

	return sb.String()
}

// printTree는 재귀적으로 트리를 출력한다.
func (g *DependencyGraph) printTree(sb *strings.Builder, node, prefix string, isLast bool, visited map[string]bool) {
	connector := "+-- "
	if !isLast {
		connector = "|-- "
	}

	if prefix == "" {
		sb.WriteString(fmt.Sprintf("[%s]\n", node))
	} else {
		sb.WriteString(fmt.Sprintf("%s%s[%s]\n", prefix, connector, node))
	}

	if visited[node] {
		childPrefix := prefix
		if isLast {
			childPrefix += "    "
		} else {
			childPrefix += "|   "
		}
		sb.WriteString(fmt.Sprintf("%s    (순환 참조)\n", childPrefix))
		return
	}
	visited[node] = true

	children, exists := g.adjacency[node]
	if !exists {
		return
	}

	childNodes := make([]string, 0, len(children))
	for child := range children {
		childNodes = append(childNodes, child)
	}
	sort.Strings(childNodes)

	childPrefix := prefix
	if prefix != "" {
		if isLast {
			childPrefix += "    "
		} else {
			childPrefix += "|   "
		}
	}

	for i, child := range childNodes {
		_ = children[child] // link 참조 (향후 호출 횟수 표시에 활용 가능)
		isLastChild := i == len(childNodes)-1
		g.printTree(sb, child, childPrefix, isLastChild, visited)
	}
}

// --- 통계 출력 ---

// PrintServiceStats는 서비스별 통계를 출력한다.
func (g *DependencyGraph) PrintServiceStats() {
	services := make([]string, 0, len(g.services))
	for s := range g.services {
		services = append(services, s)
	}
	sort.Strings(services)

	fmt.Println("\n=== 서비스별 통계 ===")
	fmt.Println()
	fmt.Printf("%-18s %8s %8s %10s %12s\n",
		"서비스", "인입", "발신", "에러율", "평균지연")
	fmt.Println(strings.Repeat("-", 62))

	for _, name := range services {
		stats := g.services[name]
		errPct := stats.ErrorRate * 100
		fmt.Printf("%-18s %8d %8d %9.1f%% %12s\n",
			name,
			stats.IncomingCalls,
			stats.OutgoingCalls,
			errPct,
			formatDuration(stats.AvgLatency))
	}
}

// PrintDependencyLinks는 의존성 링크 목록을 출력한다.
func (g *DependencyGraph) PrintDependencyLinks() {
	fmt.Println("\n=== 의존성 링크 목록 ===")
	fmt.Println()
	fmt.Printf("%-18s  -->  %-18s %8s %10s %12s\n",
		"부모 서비스", "자식 서비스", "호출수", "에러율", "평균지연")
	fmt.Println(strings.Repeat("-", 76))

	parents := make([]string, 0, len(g.adjacency))
	for p := range g.adjacency {
		parents = append(parents, p)
	}
	sort.Strings(parents)

	for _, parent := range parents {
		children := make([]string, 0, len(g.adjacency[parent]))
		for c := range g.adjacency[parent] {
			children = append(children, c)
		}
		sort.Strings(children)

		for _, child := range children {
			link := g.adjacency[parent][child]
			latency := g.latencyMatrix[parent][child]
			errPct := link.ErrorRate * 100
			fmt.Printf("%-18s  -->  %-18s %8d %9.1f%% %12s\n",
				link.Parent, link.Child, link.CallCount, errPct, formatDuration(latency))
		}
	}
}

func formatDuration(d time.Duration) string {
	if d < time.Millisecond {
		return fmt.Sprintf("%.0fus", float64(d)/float64(time.Microsecond))
	}
	return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond))
}

// =============================================================================
// 테스트 시나리오 생성
// =============================================================================

func generateNormalTraces() map[string][]Span {
	traces := make(map[string][]Span)
	now := time.Now()

	// 트레이스 1: 정상적인 마이크로서비스 호출 체인
	// frontend -> api-gateway -> user-service -> database
	traces["trace-001"] = []Span{
		{TraceID: "trace-001", SpanID: "s1", ParentID: "", ServiceName: "frontend",
			Operation: "GET /dashboard", StartTime: now, Duration: 250 * time.Millisecond},
		{TraceID: "trace-001", SpanID: "s2", ParentID: "s1", ServiceName: "api-gateway",
			Operation: "route", StartTime: now.Add(5 * time.Millisecond), Duration: 200 * time.Millisecond},
		{TraceID: "trace-001", SpanID: "s3", ParentID: "s2", ServiceName: "user-service",
			Operation: "getUser", StartTime: now.Add(10 * time.Millisecond), Duration: 150 * time.Millisecond},
		{TraceID: "trace-001", SpanID: "s4", ParentID: "s3", ServiceName: "database",
			Operation: "SELECT", StartTime: now.Add(15 * time.Millisecond), Duration: 50 * time.Millisecond},
	}

	// 트레이스 2: 같은 경로, 약간의 에러
	traces["trace-002"] = []Span{
		{TraceID: "trace-002", SpanID: "s5", ParentID: "", ServiceName: "frontend",
			Operation: "GET /profile", StartTime: now.Add(time.Second), Duration: 300 * time.Millisecond},
		{TraceID: "trace-002", SpanID: "s6", ParentID: "s5", ServiceName: "api-gateway",
			Operation: "route", StartTime: now.Add(time.Second + 3*time.Millisecond), Duration: 250 * time.Millisecond},
		{TraceID: "trace-002", SpanID: "s7", ParentID: "s6", ServiceName: "user-service",
			Operation: "getUser", StartTime: now.Add(time.Second + 8*time.Millisecond), Duration: 180 * time.Millisecond, HasError: true},
		{TraceID: "trace-002", SpanID: "s8", ParentID: "s7", ServiceName: "database",
			Operation: "SELECT", StartTime: now.Add(time.Second + 12*time.Millisecond), Duration: 80 * time.Millisecond},
	}

	// 트레이스 3: 분기 호출 (api-gateway -> user-service, api-gateway -> order-service)
	traces["trace-003"] = []Span{
		{TraceID: "trace-003", SpanID: "s9", ParentID: "", ServiceName: "frontend",
			Operation: "GET /checkout", StartTime: now.Add(2 * time.Second), Duration: 500 * time.Millisecond},
		{TraceID: "trace-003", SpanID: "s10", ParentID: "s9", ServiceName: "api-gateway",
			Operation: "route", StartTime: now.Add(2*time.Second + 5*time.Millisecond), Duration: 450 * time.Millisecond},
		{TraceID: "trace-003", SpanID: "s11", ParentID: "s10", ServiceName: "user-service",
			Operation: "getUser", StartTime: now.Add(2*time.Second + 10*time.Millisecond), Duration: 100 * time.Millisecond},
		{TraceID: "trace-003", SpanID: "s12", ParentID: "s10", ServiceName: "order-service",
			Operation: "createOrder", StartTime: now.Add(2*time.Second + 10*time.Millisecond), Duration: 350 * time.Millisecond},
		{TraceID: "trace-003", SpanID: "s13", ParentID: "s12", ServiceName: "database",
			Operation: "INSERT", StartTime: now.Add(2*time.Second + 20*time.Millisecond), Duration: 120 * time.Millisecond},
		{TraceID: "trace-003", SpanID: "s14", ParentID: "s12", ServiceName: "payment-service",
			Operation: "charge", StartTime: now.Add(2*time.Second + 150*time.Millisecond), Duration: 200 * time.Millisecond, HasError: true},
	}

	// 트레이스 4-8: 반복 호출 (통계 축적용)
	for i := 4; i <= 8; i++ {
		traceID := fmt.Sprintf("trace-%03d", i)
		offset := time.Duration(i) * time.Second
		hasErr := i%3 == 0
		traces[traceID] = []Span{
			{TraceID: traceID, SpanID: fmt.Sprintf("s%d0", i), ParentID: "", ServiceName: "frontend",
				Operation: "GET /api", StartTime: now.Add(offset), Duration: time.Duration(200+i*20) * time.Millisecond},
			{TraceID: traceID, SpanID: fmt.Sprintf("s%d1", i), ParentID: fmt.Sprintf("s%d0", i), ServiceName: "api-gateway",
				Operation: "route", StartTime: now.Add(offset + 5*time.Millisecond), Duration: time.Duration(150+i*15) * time.Millisecond},
			{TraceID: traceID, SpanID: fmt.Sprintf("s%d2", i), ParentID: fmt.Sprintf("s%d1", i), ServiceName: "user-service",
				Operation: "getUser", StartTime: now.Add(offset + 10*time.Millisecond), Duration: time.Duration(80+i*10) * time.Millisecond, HasError: hasErr},
			{TraceID: traceID, SpanID: fmt.Sprintf("s%d3", i), ParentID: fmt.Sprintf("s%d2", i), ServiceName: "database",
				Operation: "SELECT", StartTime: now.Add(offset + 15*time.Millisecond), Duration: time.Duration(30+i*5) * time.Millisecond},
		}
	}

	return traces
}

func generateCyclicTraces() map[string][]Span {
	traces := make(map[string][]Span)
	now := time.Now()

	// 순환 의존성이 포함된 트레이스
	// service-A -> service-B -> service-C -> service-A (순환!)
	traces["cycle-001"] = []Span{
		{TraceID: "cycle-001", SpanID: "c1", ParentID: "", ServiceName: "service-A",
			Operation: "handleRequest", StartTime: now, Duration: 400 * time.Millisecond},
		{TraceID: "cycle-001", SpanID: "c2", ParentID: "c1", ServiceName: "service-B",
			Operation: "process", StartTime: now.Add(10 * time.Millisecond), Duration: 300 * time.Millisecond},
		{TraceID: "cycle-001", SpanID: "c3", ParentID: "c2", ServiceName: "service-C",
			Operation: "validate", StartTime: now.Add(20 * time.Millisecond), Duration: 200 * time.Millisecond},
	}

	// 다른 트레이스에서 service-C -> service-A 호출 발생 (순환 완성)
	traces["cycle-002"] = []Span{
		{TraceID: "cycle-002", SpanID: "c4", ParentID: "", ServiceName: "service-C",
			Operation: "callback", StartTime: now.Add(time.Second), Duration: 150 * time.Millisecond},
		{TraceID: "cycle-002", SpanID: "c5", ParentID: "c4", ServiceName: "service-A",
			Operation: "handleCallback", StartTime: now.Add(time.Second + 10*time.Millisecond), Duration: 100 * time.Millisecond},
	}

	// 추가: service-B -> service-D (비순환 분기)
	traces["cycle-003"] = []Span{
		{TraceID: "cycle-003", SpanID: "c6", ParentID: "", ServiceName: "service-A",
			Operation: "handleRequest", StartTime: now.Add(2 * time.Second), Duration: 350 * time.Millisecond},
		{TraceID: "cycle-003", SpanID: "c7", ParentID: "c6", ServiceName: "service-B",
			Operation: "process", StartTime: now.Add(2*time.Second + 10*time.Millisecond), Duration: 250 * time.Millisecond},
		{TraceID: "cycle-003", SpanID: "c8", ParentID: "c7", ServiceName: "service-D",
			Operation: "store", StartTime: now.Add(2*time.Second + 20*time.Millisecond), Duration: 100 * time.Millisecond},
	}

	return traces
}

// =============================================================================
// 메인
// =============================================================================

func main() {
	fmt.Println("================================================================")
	fmt.Println(" Jaeger 서비스 의존성 DAG 분석 시뮬레이터")
	fmt.Println("================================================================")

	// ── 시나리오 1: 정상적인 마이크로서비스 아키텍처 ──
	fmt.Println("\n\n########################################################")
	fmt.Println("# 시나리오 1: 정상적인 마이크로서비스 아키텍처")
	fmt.Println("########################################################")
	fmt.Println()
	fmt.Println("구성: frontend -> api-gateway -> user-service -> database")
	fmt.Println("      api-gateway -> order-service -> database")
	fmt.Println("      order-service -> payment-service")

	graph1 := NewDependencyGraph()
	traces1 := generateNormalTraces()
	graph1.BuildFromTraces(traces1)

	fmt.Printf("\n총 트레이스 수: %d\n", len(traces1))

	// 의존성 링크 출력
	graph1.PrintDependencyLinks()

	// 서비스별 통계
	graph1.PrintServiceStats()

	// 순환 의존성 감지
	fmt.Println("\n=== 순환 의존성 감지 ===")
	cycles1 := graph1.DetectCycles()
	if len(cycles1) == 0 {
		fmt.Println("  순환 의존성 없음 (정상)")
	} else {
		for i, c := range cycles1 {
			fmt.Printf("  순환 %d: %s\n", i+1, strings.Join(c.Path, " -> "))
		}
	}

	// 크리티컬 패스
	fmt.Println("\n=== 크리티컬 패스 (최장 지연 경로) ===")
	cp1 := graph1.FindCriticalPath()
	if len(cp1.Path) > 0 {
		fmt.Printf("  경로: %s\n", strings.Join(cp1.Path, " -> "))
		fmt.Printf("  총 지연시간: %s\n", formatDuration(cp1.TotalLatency))

		// 경로 상세
		fmt.Println("\n  경로 상세:")
		for i := 0; i < len(cp1.Path)-1; i++ {
			parent := cp1.Path[i]
			child := cp1.Path[i+1]
			latency := graph1.latencyMatrix[parent][child]
			fmt.Printf("    %s -> %s : %s\n", parent, child, formatDuration(latency))
		}
	}

	// ASCII 시각화
	fmt.Print(graph1.VisualizeASCII())

	// ── 시나리오 2: 순환 의존성이 포함된 아키텍처 ──
	fmt.Println("\n\n########################################################")
	fmt.Println("# 시나리오 2: 순환 의존성이 포함된 아키텍처")
	fmt.Println("########################################################")
	fmt.Println()
	fmt.Println("구성: service-A -> service-B -> service-C -> service-A (순환!)")
	fmt.Println("      service-B -> service-D")

	graph2 := NewDependencyGraph()
	traces2 := generateCyclicTraces()
	graph2.BuildFromTraces(traces2)

	fmt.Printf("\n총 트레이스 수: %d\n", len(traces2))

	// 의존성 링크
	graph2.PrintDependencyLinks()

	// 서비스별 통계
	graph2.PrintServiceStats()

	// 순환 의존성 감지
	fmt.Println("\n=== 순환 의존성 감지 ===")
	cycles2 := graph2.DetectCycles()
	if len(cycles2) == 0 {
		fmt.Println("  순환 의존성 없음")
	} else {
		for i, c := range cycles2 {
			fmt.Printf("  [순환 %d] %s\n", i+1, strings.Join(c.Path, " -> "))
		}
		fmt.Println()
		fmt.Println("  [경고] 순환 의존성은 마이크로서비스 아키텍처의 안티패턴입니다.")
		fmt.Println("  서비스 간 결합도를 높이고 장애 전파를 유발할 수 있습니다.")
	}

	// 크리티컬 패스 (순환이 있을 경우)
	fmt.Println("\n=== 크리티컬 패스 (최장 지연 경로) ===")
	cp2 := graph2.FindCriticalPath()
	if len(cp2.Path) > 0 {
		fmt.Printf("  경로: %s\n", strings.Join(cp2.Path, " -> "))
		fmt.Printf("  총 지연시간: %s\n", formatDuration(cp2.TotalLatency))
	}

	// ASCII 시각화
	fmt.Print(graph2.VisualizeASCII())

	// ── 시나리오 3: 대규모 의존성 그래프 통계 ──
	fmt.Println("\n\n########################################################")
	fmt.Println("# 시나리오 3: 대규모 의존성 그래프 시뮬레이션")
	fmt.Println("########################################################")

	graph3 := NewDependencyGraph()
	traces3 := generateLargeTraces()
	graph3.BuildFromTraces(traces3)

	fmt.Printf("\n총 트레이스 수: %d\n", len(traces3))
	fmt.Printf("총 서비스 수: %d\n", len(graph3.services))

	totalEdges := 0
	for _, children := range graph3.adjacency {
		totalEdges += len(children)
	}
	fmt.Printf("총 의존성 링크 수: %d\n", totalEdges)

	graph3.PrintDependencyLinks()
	graph3.PrintServiceStats()

	cycles3 := graph3.DetectCycles()
	fmt.Println("\n=== 순환 의존성 감지 ===")
	if len(cycles3) == 0 {
		fmt.Println("  순환 의존성 없음")
	} else {
		for i, c := range cycles3 {
			fmt.Printf("  [순환 %d] %s\n", i+1, strings.Join(c.Path, " -> "))
		}
	}

	cp3 := graph3.FindCriticalPath()
	fmt.Println("\n=== 크리티컬 패스 ===")
	if len(cp3.Path) > 0 {
		fmt.Printf("  경로: %s\n", strings.Join(cp3.Path, " -> "))
		fmt.Printf("  총 지연시간: %s\n", formatDuration(cp3.TotalLatency))
		fmt.Printf("  홉 수: %d\n", len(cp3.Path)-1)
	}

	fmt.Print(graph3.VisualizeASCII())

	fmt.Println("\n================================================================")
	fmt.Println(" 시뮬레이션 완료")
	fmt.Println("================================================================")

	// 핵심 설계 포인트 설명
	fmt.Println()
	fmt.Println("=== Jaeger DAG 의존성 분석 핵심 설계 포인트 ===")
	fmt.Println()
	fmt.Println("1. 의존성 추출: 동일 트레이스 내 parent-child 스팬의 서비스가")
	fmt.Println("   다를 때만 의존성으로 기록 (같은 서비스 내 호출은 제외)")
	fmt.Println()
	fmt.Println("2. 집계 방식: DependencyLink에 호출 횟수와 에러 횟수를 누적하여")
	fmt.Println("   시간 범위별 의존성 통계를 제공")
	fmt.Println()
	fmt.Println("3. 순환 감지: DFS 기반 back-edge 탐지로 순환 의존성을 식별")
	fmt.Println("   마이크로서비스 아키텍처에서 순환은 안티패턴")
	fmt.Println()
	fmt.Println("4. 크리티컬 패스: 위상 정렬 + DP로 최장 지연 경로를 계산")
	fmt.Println("   성능 병목 지점을 찾는 데 활용")

	_ = math.MaxInt64 // math 패키지 사용 확인
}

// generateLargeTraces는 더 복잡한 마이크로서비스 아키텍처를 시뮬레이션한다.
func generateLargeTraces() map[string][]Span {
	traces := make(map[string][]Span)
	now := time.Now()

	// 10개 서비스의 마이크로서비스 아키텍처
	// gateway -> auth, gateway -> product-svc, gateway -> order-svc
	// product-svc -> inventory-svc, product-svc -> cache
	// order-svc -> payment-svc, order-svc -> notification-svc
	// payment-svc -> bank-api
	// notification-svc -> email-svc

	for i := 0; i < 20; i++ {
		traceID := fmt.Sprintf("large-%03d", i)
		offset := time.Duration(i) * 500 * time.Millisecond
		hasErr := i%5 == 0

		spans := []Span{
			{TraceID: traceID, SpanID: fmt.Sprintf("lg%d-0", i), ParentID: "",
				ServiceName: "gateway", Operation: "handleRequest",
				StartTime: now.Add(offset), Duration: 600 * time.Millisecond},
			{TraceID: traceID, SpanID: fmt.Sprintf("lg%d-1", i), ParentID: fmt.Sprintf("lg%d-0", i),
				ServiceName: "auth", Operation: "validateToken",
				StartTime: now.Add(offset + 5*time.Millisecond), Duration: 30 * time.Millisecond},
		}

		if i%2 == 0 {
			// 상품 조회 경로
			spans = append(spans,
				Span{TraceID: traceID, SpanID: fmt.Sprintf("lg%d-2", i), ParentID: fmt.Sprintf("lg%d-0", i),
					ServiceName: "product-svc", Operation: "getProduct",
					StartTime: now.Add(offset + 40*time.Millisecond), Duration: 200 * time.Millisecond},
				Span{TraceID: traceID, SpanID: fmt.Sprintf("lg%d-3", i), ParentID: fmt.Sprintf("lg%d-2", i),
					ServiceName: "inventory-svc", Operation: "checkStock",
					StartTime: now.Add(offset + 50*time.Millisecond), Duration: 80 * time.Millisecond},
				Span{TraceID: traceID, SpanID: fmt.Sprintf("lg%d-4", i), ParentID: fmt.Sprintf("lg%d-2", i),
					ServiceName: "cache", Operation: "GET",
					StartTime: now.Add(offset + 45*time.Millisecond), Duration: 5 * time.Millisecond},
			)
		}

		if i%3 == 0 {
			// 주문 경로
			spans = append(spans,
				Span{TraceID: traceID, SpanID: fmt.Sprintf("lg%d-5", i), ParentID: fmt.Sprintf("lg%d-0", i),
					ServiceName: "order-svc", Operation: "createOrder",
					StartTime: now.Add(offset + 40*time.Millisecond), Duration: 400 * time.Millisecond, HasError: hasErr},
				Span{TraceID: traceID, SpanID: fmt.Sprintf("lg%d-6", i), ParentID: fmt.Sprintf("lg%d-5", i),
					ServiceName: "payment-svc", Operation: "processPayment",
					StartTime: now.Add(offset + 50*time.Millisecond), Duration: 250 * time.Millisecond, HasError: hasErr},
				Span{TraceID: traceID, SpanID: fmt.Sprintf("lg%d-7", i), ParentID: fmt.Sprintf("lg%d-6", i),
					ServiceName: "bank-api", Operation: "charge",
					StartTime: now.Add(offset + 60*time.Millisecond), Duration: 200 * time.Millisecond},
				Span{TraceID: traceID, SpanID: fmt.Sprintf("lg%d-8", i), ParentID: fmt.Sprintf("lg%d-5", i),
					ServiceName: "notification-svc", Operation: "sendNotification",
					StartTime: now.Add(offset + 300*time.Millisecond), Duration: 100 * time.Millisecond},
				Span{TraceID: traceID, SpanID: fmt.Sprintf("lg%d-9", i), ParentID: fmt.Sprintf("lg%d-8", i),
					ServiceName: "email-svc", Operation: "send",
					StartTime: now.Add(offset + 310*time.Millisecond), Duration: 80 * time.Millisecond},
			)
		}

		traces[traceID] = spans
	}

	return traces
}
