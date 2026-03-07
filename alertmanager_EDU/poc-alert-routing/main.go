// Alertmanager Alert Routing PoC
//
// Alertmanager의 Route 트리 기반 Alert 라우팅을 시뮬레이션한다.
// 실제 dispatch/route.go의 Match() DFS 알고리즘을 재현한다.
//
// 핵심 개념:
//   - Route 트리 구조 (부모 → 자식 → 손자)
//   - DFS(깊이 우선 탐색) 매칭
//   - Continue 옵션 (다음 형제도 계속 매칭)
//   - 옵션 상속 (GroupWait, GroupInterval 등)
//
// 실행: go run main.go

package main

import (
	"fmt"
	"strings"
	"time"
)

// LabelSet은 Alert의 레이블 집합이다.
type LabelSet map[string]string

// Matcher는 단일 레이블 매칭 조건이다.
type Matcher struct {
	Name  string // 레이블 이름
	Value string // 매칭 값
	Equal bool   // true: 일치, false: 불일치
}

// Matches는 주어진 값이 Matcher 조건에 맞는지 확인한다.
func (m *Matcher) Matches(val string) bool {
	if m.Equal {
		return val == m.Value
	}
	return val != m.Value
}

// RouteOpts는 Route의 설정 옵션이다.
type RouteOpts struct {
	Receiver      string
	GroupBy       []string
	GroupWait     time.Duration
	GroupInterval time.Duration
}

// Route는 라우팅 트리의 노드이다.
type Route struct {
	parent   *Route
	Matchers []*Matcher // 매칭 조건 (AND)
	Continue bool       // 다음 형제도 매칭 시도
	Routes   []*Route   // 자식 Route
	Opts     RouteOpts  // 옵션
	Idx      int        // 고유 인덱스
}

// NewRoute는 설정으로부터 Route를 생성하며, 부모의 옵션을 상속한다.
func NewRoute(parent *Route, matchers []*Matcher, receiver string, cont bool, idx int) *Route {
	r := &Route{
		parent:   parent,
		Matchers: matchers,
		Continue: cont,
		Idx:      idx,
		Opts: RouteOpts{
			Receiver: receiver,
		},
	}

	// 부모로부터 옵션 상속
	if parent != nil {
		if r.Opts.GroupWait == 0 {
			r.Opts.GroupWait = parent.Opts.GroupWait
		}
		if r.Opts.GroupInterval == 0 {
			r.Opts.GroupInterval = parent.Opts.GroupInterval
		}
		if len(r.Opts.GroupBy) == 0 {
			r.Opts.GroupBy = parent.Opts.GroupBy
		}
	}

	return r
}

// Match는 DFS로 Route 트리를 탐색하여 매칭되는 Route를 반환한다.
// 실제 alertmanager dispatch/route.go의 Match() 알고리즘을 재현한다.
func (r *Route) Match(lset LabelSet) []*Route {
	// 현재 Route의 Matchers 확인 (AND 로직)
	if !r.matchAll(lset) {
		return nil
	}

	// 자식 Route 탐색 (DFS)
	var all []*Route
	for _, cr := range r.Routes {
		matches := cr.Match(lset)
		all = append(all, matches...)

		// 매칭되고 Continue가 false면 탐색 중단
		if matches != nil && !cr.Continue {
			break
		}
	}

	// 자식에서 매칭된 것이 없으면 자기 자신 반환
	if len(all) == 0 {
		all = append(all, r)
	}

	return all
}

// matchAll은 모든 Matcher가 레이블셋과 일치하는지 확인한다 (AND).
func (r *Route) matchAll(lset LabelSet) bool {
	for _, m := range r.Matchers {
		val := lset[m.Name]
		if !m.Matches(val) {
			return false
		}
	}
	return true
}

// String은 Route의 문자열 표현을 반환한다.
func (r *Route) String() string {
	var parts []string
	for _, m := range r.Matchers {
		op := "="
		if !m.Equal {
			op = "!="
		}
		parts = append(parts, fmt.Sprintf("%s%s%q", m.Name, op, m.Value))
	}
	matchStr := "{" + strings.Join(parts, ", ") + "}"
	if len(r.Matchers) == 0 {
		matchStr = "{} (root)"
	}
	return fmt.Sprintf("Route[%s → %s]", matchStr, r.Opts.Receiver)
}

// buildRouteTree는 테스트용 Route 트리를 구축한다.
//
// 트리 구조:
//
//	root (receiver=default, group_by=[alertname])
//	├── severity="critical" → pager
//	│   └── team="infra" → infra-pager
//	├── severity="warning" → slack (continue=true)
//	├── team="backend" → backend-slack
//	└── severity!="info" → catch-all
func buildRouteTree() *Route {
	// 루트 Route
	root := &Route{
		Idx: 0,
		Opts: RouteOpts{
			Receiver:      "default",
			GroupBy:       []string{"alertname"},
			GroupWait:     30 * time.Second,
			GroupInterval: 5 * time.Minute,
		},
	}

	// 자식 1: severity="critical" → pager
	child1 := NewRoute(root,
		[]*Matcher{{Name: "severity", Value: "critical", Equal: true}},
		"pager", false, 1)

	// 손자: team="infra" → infra-pager
	grandchild := NewRoute(child1,
		[]*Matcher{{Name: "team", Value: "infra", Equal: true}},
		"infra-pager", false, 2)
	child1.Routes = []*Route{grandchild}

	// 자식 2: severity="warning" → slack (Continue=true)
	child2 := NewRoute(root,
		[]*Matcher{{Name: "severity", Value: "warning", Equal: true}},
		"slack", true, 3)

	// 자식 3: team="backend" → backend-slack
	child3 := NewRoute(root,
		[]*Matcher{{Name: "team", Value: "backend", Equal: true}},
		"backend-slack", false, 4)

	// 자식 4: severity!="info" → catch-all
	child4 := NewRoute(root,
		[]*Matcher{{Name: "severity", Value: "info", Equal: false}},
		"catch-all", false, 5)

	root.Routes = []*Route{child1, child2, child3, child4}
	return root
}

func main() {
	fmt.Println("=== Alertmanager Route 매칭 PoC ===")
	fmt.Println()

	root := buildRouteTree()

	// Route 트리 출력
	fmt.Println("Route 트리:")
	printTree(root, 0)
	fmt.Println()

	// 테스트 케이스
	testCases := []struct {
		name   string
		labels LabelSet
	}{
		{
			name:   "Critical + Infra → infra-pager (DFS 깊은 매칭)",
			labels: LabelSet{"alertname": "HighCPU", "severity": "critical", "team": "infra"},
		},
		{
			name:   "Critical + Backend → pager (infra 미매칭)",
			labels: LabelSet{"alertname": "HighCPU", "severity": "critical", "team": "backend"},
		},
		{
			name:   "Warning + Backend → slack + backend-slack (Continue)",
			labels: LabelSet{"alertname": "HighLatency", "severity": "warning", "team": "backend"},
		},
		{
			name:   "Warning + Frontend → slack + catch-all (Continue + severity!=info)",
			labels: LabelSet{"alertname": "SlowResponse", "severity": "warning", "team": "frontend"},
		},
		{
			name:   "Info → default (모든 자식 미매칭, 루트 반환)",
			labels: LabelSet{"alertname": "HealthCheck", "severity": "info"},
		},
		{
			name:   "Error + Backend → backend-slack (첫 매칭에서 중단)",
			labels: LabelSet{"alertname": "DBError", "severity": "error", "team": "backend"},
		},
	}

	for i, tc := range testCases {
		fmt.Printf("--- 테스트 %d: %s ---\n", i+1, tc.name)
		fmt.Printf("Labels: %v\n", tc.labels)

		matches := root.Match(tc.labels)
		fmt.Printf("매칭 결과 (%d개):\n", len(matches))
		for _, m := range matches {
			fmt.Printf("  → %s\n", m)
		}
		fmt.Println()
	}

	// Continue 동작 설명
	fmt.Println("=== Continue 동작 원리 ===")
	fmt.Println("Continue=false (기본): 첫 매칭된 자식에서 탐색 중단")
	fmt.Println("Continue=true: 매칭 후에도 다음 형제 Route 계속 탐색")
	fmt.Println()
	fmt.Println("예: severity=warning(Continue=true) 매칭 후 →")
	fmt.Println("    다음 형제인 team=backend도 확인 → 추가 매칭 가능")
}

// printTree는 Route 트리를 들여쓰기로 출력한다.
func printTree(r *Route, depth int) {
	indent := strings.Repeat("  ", depth)
	cont := ""
	if r.Continue {
		cont = " [Continue]"
	}
	fmt.Printf("%s%s%s\n", indent, r, cont)
	for _, child := range r.Routes {
		printTree(child, depth+1)
	}
}
