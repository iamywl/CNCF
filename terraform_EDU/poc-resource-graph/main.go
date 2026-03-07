package main

import (
	"fmt"
	"strings"
)

// =============================================================================
// Terraform 리소스 의존성 그래프 빌더 시뮬레이션
// =============================================================================
//
// Terraform은 리소스 정의에서 참조(Reference)를 분석하여
// 자동으로 의존성 그래프를 구축한다.
//
// 실제 Terraform 소스:
//   - internal/terraform/transform_reference.go: ReferenceTransformer
//   - internal/terraform/transform_transitive_reduction.go: TransitiveReductionTransformer
//   - internal/addrs/referenceable.go: 참조 가능한 주소 체계
//   - internal/lang/references.go: HCL 표현식에서 참조 추출
//
// 이 PoC에서 구현하는 핵심 개념:
//   1. 리소스 정의에서 참조(Reference) 추출
//   2. 참조를 기반으로 의존성 간선(Edge) 생성
//   3. ReferenceTransformer 패턴
//   4. 이행적 축소(Transitive Reduction)

// =============================================================================
// 1. 리소스 주소 체계 (Addressing)
// =============================================================================

// ResourceAddr은 리소스의 주소를 나타낸다.
// Terraform에서는 addrs.Resource, addrs.ResourceInstance 등으로 구현된다.
//
// 예: aws_vpc.main → ResourceAddr{Type: "aws_vpc", Name: "main"}
type ResourceAddr struct {
	Type string // 리소스 타입 (aws_vpc, aws_subnet, ...)
	Name string // 리소스 이름 (main, web, ...)
}

func (a ResourceAddr) String() string {
	return fmt.Sprintf("%s.%s", a.Type, a.Name)
}

// =============================================================================
// 2. 리소스 설정 (Configuration)
// =============================================================================

// Attribute는 리소스의 속성 하나를 나타낸다.
type Attribute struct {
	Key   string
	Value string // 문자열 값 (참조 표현식 포함 가능)
}

// ResourceConfig는 하나의 리소스 설정을 나타낸다.
// Terraform의 configs.Resource에 대응한다.
type ResourceConfig struct {
	Addr       ResourceAddr
	Attributes []Attribute
}

// =============================================================================
// 3. 참조 추출기 (Reference Extractor)
// =============================================================================

// Reference는 하나의 참조를 나타낸다.
// 예: aws_vpc.main.id → Reference{Addr: {Type: "aws_vpc", Name: "main"}, Attr: "id"}
type Reference struct {
	Addr ResourceAddr
	Attr string // 참조 속성 (id, arn, ...)
}

func (r Reference) String() string {
	if r.Attr != "" {
		return fmt.Sprintf("%s.%s", r.Addr, r.Attr)
	}
	return r.Addr.String()
}

// ExtractReferences는 속성 값에서 리소스 참조를 추출한다.
// Terraform에서는 internal/lang/references.go의 References() 함수가
// HCL 표현식 트리를 순회하며 참조를 추출한다.
//
// 실제 구현에서는 hcl.Expression을 분석하지만,
// 여기서는 문자열 패턴 매칭으로 간단히 구현한다.
func ExtractReferences(value string, knownResources map[string]bool) []Reference {
	var refs []Reference

	// 알려진 리소스 타입.이름 패턴 검색
	for addr := range knownResources {
		if strings.Contains(value, addr) {
			parts := strings.SplitN(addr, ".", 2)
			if len(parts) == 2 {
				ref := Reference{
					Addr: ResourceAddr{Type: parts[0], Name: parts[1]},
				}
				// .id, .arn 등 속성 추출
				afterAddr := value[strings.Index(value, addr)+len(addr):]
				if len(afterAddr) > 0 && afterAddr[0] == '.' {
					attrEnd := strings.IndexAny(afterAddr[1:], " ,})\n\"")
					if attrEnd == -1 {
						ref.Attr = afterAddr[1:]
					} else {
						ref.Attr = afterAddr[1 : attrEnd+1]
					}
				}
				refs = append(refs, ref)
			}
		}
	}

	return refs
}

// =============================================================================
// 4. 의존성 그래프 (Dependency Graph)
// =============================================================================

// Edge는 의존성 간선을 나타낸다.
type Edge struct {
	From ResourceAddr // 의존하는 리소스
	To   ResourceAddr // 의존 대상 리소스
	Refs []Reference  // 이 의존성의 원인이 되는 참조들
}

// DependencyGraph는 리소스 의존성 그래프이다.
type DependencyGraph struct {
	Resources []ResourceConfig
	Edges     []Edge
	AdjList   map[string][]string // 인접 리스트 (From → []To)
}

// NewDependencyGraph는 새로운 의존성 그래프를 생성한다.
func NewDependencyGraph() *DependencyGraph {
	return &DependencyGraph{
		AdjList: make(map[string][]string),
	}
}

// AddResource는 리소스를 그래프에 추가한다.
func (g *DependencyGraph) AddResource(rc ResourceConfig) {
	g.Resources = append(g.Resources, rc)
}

// =============================================================================
// 5. ReferenceTransformer
// =============================================================================

// ReferenceTransformer는 리소스 간 참조를 분석하여 의존성 간선을 생성한다.
// Terraform의 internal/terraform/transform_reference.go에 대응한다.
//
// 동작 원리:
//   1. 모든 리소스의 속성을 순회
//   2. 각 속성 값에서 다른 리소스에 대한 참조를 추출
//   3. 참조가 발견되면 의존성 간선을 추가
func (g *DependencyGraph) ApplyReferenceTransformer() {
	fmt.Println("  [ReferenceTransformer] 리소스 참조 분석 시작...")
	fmt.Println()

	// 알려진 리소스 주소 집합 구축
	knownResources := make(map[string]bool)
	for _, rc := range g.Resources {
		knownResources[rc.Addr.String()] = true
	}

	// 각 리소스의 속성에서 참조 추출
	for _, rc := range g.Resources {
		for _, attr := range rc.Attributes {
			refs := ExtractReferences(attr.Value, knownResources)
			for _, ref := range refs {
				// 자기 참조 제외
				if ref.Addr.String() == rc.Addr.String() {
					continue
				}

				edge := Edge{
					From: rc.Addr,
					To:   ref.Addr,
					Refs: []Reference{ref},
				}
				g.Edges = append(g.Edges, edge)

				fromStr := rc.Addr.String()
				toStr := ref.Addr.String()
				// 중복 간선 방지
				alreadyExists := false
				for _, existing := range g.AdjList[fromStr] {
					if existing == toStr {
						alreadyExists = true
						break
					}
				}
				if !alreadyExists {
					g.AdjList[fromStr] = append(g.AdjList[fromStr], toStr)
				}

				fmt.Printf("    발견: %s.%s = %s\n", rc.Addr, attr.Key, attr.Value)
				fmt.Printf("         → %s 는 %s 에 의존\n", rc.Addr, ref.Addr)
				fmt.Println()
			}
		}
	}
}

// =============================================================================
// 6. 이행적 축소 (Transitive Reduction)
// =============================================================================

// ApplyTransitiveReduction은 이행적 축소를 수행한다.
// Terraform의 internal/terraform/transform_transitive_reduction.go에 대응한다.
//
// 이행적 축소란:
//   A → B, B → C, A → C 에서 A → C를 제거하는 것이다.
//   A는 B를 통해 이미 C에 간접 의존하므로, 직접 간선은 불필요하다.
//
// 이를 통해 그래프를 최소화하여 불필요한 동기화를 줄인다.
func (g *DependencyGraph) ApplyTransitiveReduction() {
	fmt.Println("  [TransitiveReduction] 이행적 축소 수행...")
	fmt.Println()

	// 각 노드에서 도달 가능한 노드 계산 (BFS)
	reachable := make(map[string]map[string]bool)
	for _, rc := range g.Resources {
		addr := rc.Addr.String()
		reachable[addr] = make(map[string]bool)
		g.bfsReachable(addr, reachable[addr])
	}

	// 이행적 간선 식별 및 제거
	var removedEdges []Edge
	newAdjList := make(map[string][]string)

	for from, targets := range g.AdjList {
		for _, to := range targets {
			// from → to가 다른 경로를 통해 도달 가능한지 확인
			isTransitive := false
			for _, mid := range targets {
				if mid == to {
					continue
				}
				// mid를 통해 to에 도달 가능한가?
				if reachable[mid][to] {
					isTransitive = true
					removedEdges = append(removedEdges, Edge{
						From: parseAddr(from),
						To:   parseAddr(to),
					})
					break
				}
			}

			if !isTransitive {
				newAdjList[from] = append(newAdjList[from], to)
			}
		}
	}

	if len(removedEdges) > 0 {
		for _, e := range removedEdges {
			fmt.Printf("    제거: %s → %s (이행적 간선)\n", e.From, e.To)
		}
		g.AdjList = newAdjList
	} else {
		fmt.Println("    제거할 이행적 간선 없음")
	}
	fmt.Println()
}

// bfsReachable은 시작 노드에서 도달 가능한 모든 노드를 찾는다.
func (g *DependencyGraph) bfsReachable(start string, visited map[string]bool) {
	queue := g.AdjList[start]
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if visited[current] {
			continue
		}
		visited[current] = true
		queue = append(queue, g.AdjList[current]...)
	}
}

func parseAddr(s string) ResourceAddr {
	parts := strings.SplitN(s, ".", 2)
	if len(parts) == 2 {
		return ResourceAddr{Type: parts[0], Name: parts[1]}
	}
	return ResourceAddr{Name: s}
}

// =============================================================================
// 7. 위상 정렬 (실행 순서 결정)
// =============================================================================

func (g *DependencyGraph) TopologicalSort() []ResourceAddr {
	visited := make(map[string]bool)
	var result []ResourceAddr

	var dfs func(name string)
	dfs = func(name string) {
		if visited[name] {
			return
		}
		visited[name] = true
		for _, dep := range g.AdjList[name] {
			dfs(dep)
		}
		result = append(result, parseAddr(name))
	}

	for _, rc := range g.Resources {
		dfs(rc.Addr.String())
	}

	return result
}

// =============================================================================
// 8. 그래프 시각화
// =============================================================================

func (g *DependencyGraph) Visualize(title string) {
	fmt.Printf("  ┌─── %s ────────────────────────────┐\n", title)
	fmt.Println("  │")

	// 노드 출력
	for _, rc := range g.Resources {
		deps := g.AdjList[rc.Addr.String()]
		if len(deps) == 0 {
			fmt.Printf("  │  [%s]  (루트 노드)\n", rc.Addr)
		} else {
			fmt.Printf("  │  [%s]\n", rc.Addr)
			for _, dep := range deps {
				fmt.Printf("  │    └──▶ %s\n", dep)
			}
		}
	}

	fmt.Println("  │")
	fmt.Println("  └──────────────────────────────────────────┘")
}

// =============================================================================
// 메인 함수
// =============================================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════╗")
	fmt.Println("║   Terraform 리소스 의존성 그래프 시뮬레이션                  ║")
	fmt.Println("║   실제 코드: internal/terraform/transform_reference.go   ║")
	fmt.Println("╚══════════════════════════════════════════════════════════╝")
	fmt.Println()

	// =========================================================================
	// 리소스 정의 (HCL 설정에서 파싱된 결과를 시뮬레이션)
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  데모: AWS 인프라 의존성 그래프 구축")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	graph := NewDependencyGraph()

	// 리소스 정의 (Terraform 설정 시뮬레이션)
	vpc := ResourceConfig{
		Addr: ResourceAddr{Type: "aws_vpc", Name: "main"},
		Attributes: []Attribute{
			{Key: "cidr_block", Value: "10.0.0.0/16"},
		},
	}

	igw := ResourceConfig{
		Addr: ResourceAddr{Type: "aws_internet_gateway", Name: "main"},
		Attributes: []Attribute{
			{Key: "vpc_id", Value: "aws_vpc.main.id"},
		},
	}

	subnet := ResourceConfig{
		Addr: ResourceAddr{Type: "aws_subnet", Name: "public"},
		Attributes: []Attribute{
			{Key: "vpc_id", Value: "aws_vpc.main.id"},
			{Key: "cidr_block", Value: "10.0.1.0/24"},
		},
	}

	sg := ResourceConfig{
		Addr: ResourceAddr{Type: "aws_security_group", Name: "web"},
		Attributes: []Attribute{
			{Key: "vpc_id", Value: "aws_vpc.main.id"},
			{Key: "name", Value: "web-sg"},
		},
	}

	ec2 := ResourceConfig{
		Addr: ResourceAddr{Type: "aws_instance", Name: "web"},
		Attributes: []Attribute{
			{Key: "subnet_id", Value: "aws_subnet.public.id"},
			{Key: "vpc_security_group_ids", Value: "[aws_security_group.web.id]"},
			{Key: "ami", Value: "ami-12345"},
		},
	}

	eip := ResourceConfig{
		Addr: ResourceAddr{Type: "aws_eip", Name: "web"},
		Attributes: []Attribute{
			{Key: "instance", Value: "aws_instance.web.id"},
			{Key: "vpc", Value: "true"},
		},
	}

	// 리소스 추가
	graph.AddResource(vpc)
	graph.AddResource(igw)
	graph.AddResource(subnet)
	graph.AddResource(sg)
	graph.AddResource(ec2)
	graph.AddResource(eip)

	// 리소스 설정 출력
	fmt.Println("  입력 리소스 설정:")
	fmt.Println()
	for _, rc := range graph.Resources {
		fmt.Printf("    resource \"%s\" \"%s\" {\n", rc.Addr.Type, rc.Addr.Name)
		for _, attr := range rc.Attributes {
			fmt.Printf("      %-30s = \"%s\"\n", attr.Key, attr.Value)
		}
		fmt.Println("    }")
		fmt.Println()
	}

	// =========================================================================
	// 단계 1: ReferenceTransformer 적용
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  단계 1: ReferenceTransformer - 참조 분석")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	graph.ApplyReferenceTransformer()

	graph.Visualize("참조 변환 후 그래프")
	fmt.Println()

	// =========================================================================
	// 단계 2: 이행적 축소 전/후 비교
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  단계 2: TransitiveReduction - 이행적 축소")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	// 이행적 축소 데모를 위한 별도 그래프
	demoGraph := NewDependencyGraph()
	demoGraph.AddResource(ResourceConfig{Addr: ResourceAddr{Type: "A", Name: "1"}, Attributes: nil})
	demoGraph.AddResource(ResourceConfig{Addr: ResourceAddr{Type: "B", Name: "1"}, Attributes: nil})
	demoGraph.AddResource(ResourceConfig{Addr: ResourceAddr{Type: "C", Name: "1"}, Attributes: nil})

	demoGraph.AdjList["A.1"] = []string{"B.1", "C.1"} // A→B, A→C
	demoGraph.AdjList["B.1"] = []string{"C.1"}         // B→C

	fmt.Println("  이행적 축소 예시:")
	fmt.Println()
	fmt.Println("    축소 전:  A → B → C")
	fmt.Println("              A ──────→ C  (이행적 간선, 불필요)")
	fmt.Println()
	fmt.Println("    축소 후:  A → B → C")
	fmt.Println("              (A→C 간선 제거, B를 통해 이미 도달 가능)")
	fmt.Println()

	demoGraph.ApplyTransitiveReduction()
	demoGraph.Visualize("축소 후 데모 그래프")
	fmt.Println()

	// 원래 그래프에 이행적 축소 적용
	fmt.Println("  원래 인프라 그래프에 이행적 축소 적용:")
	fmt.Println()
	graph.ApplyTransitiveReduction()

	graph.Visualize("최종 의존성 그래프")
	fmt.Println()

	// =========================================================================
	// 단계 3: 위상 정렬 (실행 순서)
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  단계 3: 실행 순서 (위상 정렬)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	order := graph.TopologicalSort()
	for i, addr := range order {
		deps := graph.AdjList[addr.String()]
		depStr := "(루트 - 의존성 없음)"
		if len(deps) > 0 {
			depStr = fmt.Sprintf("← %s", strings.Join(deps, ", "))
		}
		fmt.Printf("    %d. %-30s %s\n", i+1, addr, depStr)
	}

	// =========================================================================
	// 의존 관계 다이어그램
	// =========================================================================
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  최종 의존 관계 다이어그램")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()
	fmt.Println("                     aws_eip.web")
	fmt.Println("                         │")
	fmt.Println("                         ▼")
	fmt.Println("                   aws_instance.web")
	fmt.Println("                    /           \\")
	fmt.Println("                   ▼             ▼")
	fmt.Println("         aws_subnet.public  aws_security_group.web")
	fmt.Println("                   \\             /")
	fmt.Println("                    ▼           ▼")
	fmt.Println("      aws_internet_gateway.main")
	fmt.Println("                   \\")
	fmt.Println("                    ▼")
	fmt.Println("                 aws_vpc.main")
	fmt.Println()

	// =========================================================================
	// 핵심 포인트
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  핵심 포인트 정리")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()
	fmt.Println("  1. ReferenceTransformer: 속성 값에서 참조를 추출하여 자동 의존성 생성")
	fmt.Println("  2. 사용자가 depends_on을 명시하지 않아도 참조만으로 의존 관계 파악")
	fmt.Println("  3. TransitiveReduction: 불필요한 간선을 제거하여 그래프 최적화")
	fmt.Println("  4. 이행적 축소로 병렬 실행 기회를 극대화한다")
	fmt.Println("  5. 최종 그래프를 위상 정렬하여 안전한 실행 순서를 결정한다")
}
