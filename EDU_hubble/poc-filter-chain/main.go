// SPDX-License-Identifier: Apache-2.0
// PoC: Hubble Filter Chain 패턴
//
// Hubble의 필터 시스템은 whitelist/blacklist 두 그룹으로 나뉩니다.
//   - Whitelist: OR 조건 (하나라도 매치하면 포함)
//   - Blacklist: OR 조건 (하나라도 매치하면 제외)
//   - 각 FlowFilter 내부: AND 조건 (모든 필드 매치 필요)
//
// 실행: go run main.go

package main

import (
	"fmt"
	"strings"
)

// ========================================
// 1. 데이터 타입
// ========================================

type Flow struct {
	Source      string
	Destination string
	Verdict     string
	Protocol    string
	Port        int
	Namespace   string
	Labels      []string
}

func (f Flow) String() string {
	return fmt.Sprintf("%s → %s:%d [%s] %s ns=%s",
		f.Source, f.Destination, f.Port, f.Protocol, f.Verdict, f.Namespace)
}

// ========================================
// 2. Filter 시스템 (Hubble의 filters 패키지 패턴)
// ========================================

// FilterFunc는 하나의 필터 조건입니다.
// true를 반환하면 매치, false면 불일치입니다.
//
// 실제 Hubble:
//   type FilterFunc func(ev *v1.Event) bool
type FilterFunc func(flow Flow) bool

// FlowFilter는 여러 FilterFunc의 AND 조합입니다.
// 모든 필터가 true를 반환해야 매치됩니다.
//
// CLI 예시: --source-pod frontend --verdict DROPPED
//   → SourcePod("frontend") AND Verdict("DROPPED")
//   → 두 조건 모두 매치해야 함
type FlowFilter struct {
	Name    string
	Filters []FilterFunc
}

// Match는 모든 필터가 매치하는지 확인합니다 (AND 조건).
func (ff FlowFilter) Match(flow Flow) bool {
	for _, f := range ff.Filters {
		if !f(flow) {
			return false
		}
	}
	return true
}

// ========================================
// 3. 필터 팩토리 함수들
// ========================================

// 각 함수는 CLI 플래그에 대응합니다.
// 실제 Hubble의 flows_filter.go에서 동일한 패턴으로 구현됩니다.

func ByVerdict(verdict string) FilterFunc {
	return func(flow Flow) bool {
		return strings.EqualFold(flow.Verdict, verdict)
	}
}

func BySourcePod(podPattern string) FilterFunc {
	return func(flow Flow) bool {
		return strings.Contains(flow.Source, podPattern)
	}
}

func ByDestinationPod(podPattern string) FilterFunc {
	return func(flow Flow) bool {
		return strings.Contains(flow.Destination, podPattern)
	}
}

func ByProtocol(protocol string) FilterFunc {
	return func(flow Flow) bool {
		return strings.EqualFold(flow.Protocol, protocol)
	}
}

func ByPort(port int) FilterFunc {
	return func(flow Flow) bool {
		return flow.Port == port
	}
}

func ByNamespace(ns string) FilterFunc {
	return func(flow Flow) bool {
		return flow.Namespace == ns
	}
}

func ByLabel(label string) FilterFunc {
	return func(flow Flow) bool {
		for _, l := range flow.Labels {
			if l == label {
				return true
			}
		}
		return false
	}
}

// ========================================
// 4. Whitelist/Blacklist 엔진
// ========================================

// FilterEngine은 Hubble의 전체 필터 로직을 구현합니다.
//
// 필터 로직:
//   1. Whitelist가 비어있으면 모든 Flow 통과
//   2. Whitelist가 있으면: 하나라도 매치하면 통과 (OR)
//   3. Blacklist에 하나라도 매치하면 제외 (OR)
type FilterEngine struct {
	Whitelist []FlowFilter
	Blacklist []FlowFilter
}

func (e *FilterEngine) Apply(flow Flow) (pass bool, reason string) {
	// 1. Whitelist 확인 (OR 조건)
	if len(e.Whitelist) > 0 {
		matched := false
		var matchedFilter string
		for _, wf := range e.Whitelist {
			if wf.Match(flow) {
				matched = true
				matchedFilter = wf.Name
				break
			}
		}
		if !matched {
			return false, "whitelist 불일치 (어떤 whitelist에도 매치 안 됨)"
		}
		reason = fmt.Sprintf("whitelist '%s' 매치", matchedFilter)
	} else {
		reason = "whitelist 없음 (모든 Flow 통과)"
	}

	// 2. Blacklist 확인 (OR 조건)
	for _, bf := range e.Blacklist {
		if bf.Match(flow) {
			return false, fmt.Sprintf("blacklist '%s' 매치 → 제외", bf.Name)
		}
	}

	return true, reason
}

// ========================================
// 5. 실행
// ========================================

func main() {
	fmt.Println("=== PoC: Hubble Filter Chain 패턴 ===")
	fmt.Println()
	fmt.Println("필터 로직:")
	fmt.Println("  Whitelist (OR):  filter1 OR filter2 → 하나라도 매치하면 포함")
	fmt.Println("  Blacklist (OR):  filter1 OR filter2 → 하나라도 매치하면 제외")
	fmt.Println("  FlowFilter 내부 (AND): 조건1 AND 조건2 → 모두 매치해야 함")
	fmt.Println()

	// 테스트 Flow들
	flows := []Flow{
		{Source: "frontend", Destination: "backend", Verdict: "FORWARDED", Protocol: "TCP", Port: 8080, Namespace: "default", Labels: []string{"app=frontend"}},
		{Source: "frontend", Destination: "coredns", Verdict: "FORWARDED", Protocol: "DNS", Port: 53, Namespace: "default", Labels: []string{"app=frontend"}},
		{Source: "scanner", Destination: "database", Verdict: "DROPPED", Protocol: "TCP", Port: 3306, Namespace: "untrusted", Labels: []string{"app=scanner"}},
		{Source: "backend", Destination: "cache", Verdict: "FORWARDED", Protocol: "TCP", Port: 6379, Namespace: "default", Labels: []string{"app=backend"}},
		{Source: "frontend", Destination: "external", Verdict: "DROPPED", Protocol: "TCP", Port: 443, Namespace: "default", Labels: []string{"app=frontend"}},
		{Source: "monitor", Destination: "backend", Verdict: "FORWARDED", Protocol: "HTTP", Port: 8080, Namespace: "monitoring", Labels: []string{"app=monitor"}},
	}

	// ── 시나리오 1: DROPPED만 보기 ──
	fmt.Println("━━━ 시나리오 1: hubble observe --verdict DROPPED ━━━")
	fmt.Println()

	engine1 := &FilterEngine{
		Whitelist: []FlowFilter{
			{Name: "verdict=DROPPED", Filters: []FilterFunc{ByVerdict("DROPPED")}},
		},
	}
	runFilter(engine1, flows)

	// ── 시나리오 2: frontend의 TCP만 보기 ──
	fmt.Println("━━━ 시나리오 2: hubble observe --source-pod frontend --protocol tcp ━━━")
	fmt.Println()

	engine2 := &FilterEngine{
		Whitelist: []FlowFilter{
			{Name: "src=frontend AND proto=tcp", Filters: []FilterFunc{
				BySourcePod("frontend"),
				ByProtocol("tcp"),
			}},
		},
	}
	runFilter(engine2, flows)

	// ── 시나리오 3: OR 조건 (DROPPED 또는 DNS) ──
	fmt.Println("━━━ 시나리오 3: 여러 whitelist (DROPPED OR DNS) ━━━")
	fmt.Println()

	engine3 := &FilterEngine{
		Whitelist: []FlowFilter{
			{Name: "verdict=DROPPED", Filters: []FilterFunc{ByVerdict("DROPPED")}},
			{Name: "proto=DNS", Filters: []FilterFunc{ByProtocol("DNS")}},
		},
	}
	runFilter(engine3, flows)

	// ── 시나리오 4: Whitelist + Blacklist ──
	fmt.Println("━━━ 시나리오 4: Whitelist(TCP) + Blacklist(untrusted 네임스페이스 제외) ━━━")
	fmt.Println()

	engine4 := &FilterEngine{
		Whitelist: []FlowFilter{
			{Name: "proto=TCP", Filters: []FilterFunc{ByProtocol("TCP")}},
		},
		Blacklist: []FlowFilter{
			{Name: "ns=untrusted", Filters: []FilterFunc{ByNamespace("untrusted")}},
		},
	}
	runFilter(engine4, flows)

	fmt.Println("핵심 포인트:")
	fmt.Println("  - FilterFunc: 하나의 조건 (예: verdict==DROPPED)")
	fmt.Println("  - FlowFilter: 여러 FilterFunc의 AND 조합")
	fmt.Println("  - Whitelist: 여러 FlowFilter의 OR 조합")
	fmt.Println("  - Blacklist: OR 조합으로 매치하면 제외")
	fmt.Println("  - 실제 Hubble: 50+ 종류의 FilterFunc 구현")
}

func runFilter(engine *FilterEngine, flows []Flow) {
	passed := 0
	for _, flow := range flows {
		pass, reason := engine.Apply(flow)
		mark := "✗"
		if pass {
			mark = "✓"
			passed++
		}
		fmt.Printf("  %s %s  ← %s\n", mark, flow, reason)
	}
	fmt.Printf("\n  결과: %d/%d 통과\n\n", passed, len(flows))
}
