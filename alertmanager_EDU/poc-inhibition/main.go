// Alertmanager Inhibition PoC
//
// Alertmanager의 Inhibition(억제) 시스템을 시뮬레이션한다.
// Source Alert가 있을 때 Target Alert를 억제하는 메커니즘을 재현한다.
//
// 핵심 개념:
//   - InhibitRule: SourceMatchers, TargetMatchers, Equal
//   - Source Alert 캐시 (scache)
//   - Equal 레이블 비교
//   - 자기 자신 억제 방지
//
// 실행: go run main.go

package main

import (
	"fmt"
	"sort"
	"strings"
)

// LabelSet은 레이블 집합이다.
type LabelSet map[string]string

// Fingerprint는 LabelSet의 고유 식별자를 생성한다.
func Fingerprint(ls LabelSet) string {
	keys := make([]string, 0, len(ls))
	for k := range ls {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, k+"="+ls[k])
	}
	return strings.Join(parts, ",")
}

// Matcher는 레이블 매칭 조건이다.
type Matcher struct {
	Name  string
	Value string
}

func (m *Matcher) Matches(val string) bool {
	return val == m.Value
}

// Alert는 활성 Alert이다.
type Alert struct {
	Labels      LabelSet
	Fingerprint string
}

// InhibitRule은 억제 규칙이다.
type InhibitRule struct {
	Name           string
	SourceMatchers []*Matcher // Source Alert 조건
	TargetMatchers []*Matcher // Target Alert 조건
	Equal          []string   // Source와 Target에서 동일해야 할 레이블

	// Source Alert 캐시: Equal 레이블 값 → Source Alert
	scache map[string][]*Alert
}

// NewInhibitRule은 새 억제 규칙을 생성한다.
func NewInhibitRule(name string, source, target []*Matcher, equal []string) *InhibitRule {
	return &InhibitRule{
		Name:           name,
		SourceMatchers: source,
		TargetMatchers: target,
		Equal:          equal,
		scache:         make(map[string][]*Alert),
	}
}

// matchAll은 모든 Matcher가 LabelSet과 일치하는지 확인한다.
func matchAll(matchers []*Matcher, lset LabelSet) bool {
	for _, m := range matchers {
		val, ok := lset[m.Name]
		if !ok || !m.Matches(val) {
			return false
		}
	}
	return true
}

// equalKey는 Equal 레이블들의 값으로 키를 생성한다.
func (r *InhibitRule) equalKey(lset LabelSet) string {
	var parts []string
	for _, label := range r.Equal {
		parts = append(parts, label+"="+lset[label])
	}
	return strings.Join(parts, ",")
}

// AddSource는 Source Alert를 캐시에 추가한다.
func (r *InhibitRule) AddSource(alert *Alert) bool {
	if !matchAll(r.SourceMatchers, alert.Labels) {
		return false
	}

	key := r.equalKey(alert.Labels)
	r.scache[key] = append(r.scache[key], alert)
	return true
}

// Mutes는 Target Alert가 억제되는지 확인한다.
func (r *InhibitRule) Mutes(target *Alert) (bool, string) {
	// Target Matchers 확인
	if !matchAll(r.TargetMatchers, target.Labels) {
		return false, ""
	}

	// Equal 레이블로 Source 조회
	key := r.equalKey(target.Labels)
	sources, ok := r.scache[key]
	if !ok || len(sources) == 0 {
		return false, ""
	}

	// 자기 자신 억제 방지
	for _, src := range sources {
		if src.Fingerprint != target.Fingerprint {
			return true, src.Fingerprint
		}
	}

	return false, ""
}

// Inhibitor는 여러 InhibitRule을 관리한다.
type Inhibitor struct {
	rules []*InhibitRule
}

// NewInhibitor는 새 Inhibitor를 생성한다.
func NewInhibitor(rules []*InhibitRule) *Inhibitor {
	return &Inhibitor{rules: rules}
}

// UpdateSources는 활성 Alert 목록으로 Source 캐시를 업데이트한다.
func (inh *Inhibitor) UpdateSources(alerts []*Alert) {
	// 캐시 초기화
	for _, rule := range inh.rules {
		rule.scache = make(map[string][]*Alert)
	}

	// 각 Alert를 Source로 등록
	for _, alert := range alerts {
		for _, rule := range inh.rules {
			if rule.AddSource(alert) {
				fmt.Printf("  [Source 등록] Rule %q: Alert %v → key=%q\n",
					rule.Name, alert.Labels, rule.equalKey(alert.Labels))
			}
		}
	}
}

// Mutes는 Target Alert가 억제되는지 확인한다.
func (inh *Inhibitor) Mutes(target *Alert) (bool, string, string) {
	for _, rule := range inh.rules {
		if muted, sourceID := rule.Mutes(target); muted {
			return true, rule.Name, sourceID
		}
	}
	return false, "", ""
}

func main() {
	fmt.Println("=== Alertmanager Inhibition PoC ===")
	fmt.Println()

	// 규칙 1: critical이 있으면 같은 alertname의 warning 억제
	rule1 := NewInhibitRule(
		"critical-inhibits-warning",
		[]*Matcher{{Name: "severity", Value: "critical"}},   // Source
		[]*Matcher{{Name: "severity", Value: "warning"}},    // Target
		[]string{"alertname"},                                 // Equal
	)

	// 규칙 2: critical이 있으면 같은 instance의 info 억제
	rule2 := NewInhibitRule(
		"critical-inhibits-info",
		[]*Matcher{{Name: "severity", Value: "critical"}},
		[]*Matcher{{Name: "severity", Value: "info"}},
		[]string{"instance"},
	)

	inhibitor := NewInhibitor([]*InhibitRule{rule1, rule2})

	// 현재 활성 Alert
	alerts := []*Alert{
		{
			Labels:      LabelSet{"alertname": "HighCPU", "severity": "critical", "instance": "node-1"},
			Fingerprint: "fp-1",
		},
		{
			Labels:      LabelSet{"alertname": "HighCPU", "severity": "warning", "instance": "node-1"},
			Fingerprint: "fp-2",
		},
		{
			Labels:      LabelSet{"alertname": "HighMemory", "severity": "warning", "instance": "node-2"},
			Fingerprint: "fp-3",
		},
		{
			Labels:      LabelSet{"alertname": "DiskFull", "severity": "critical", "instance": "node-2"},
			Fingerprint: "fp-4",
		},
		{
			Labels:      LabelSet{"alertname": "HealthCheck", "severity": "info", "instance": "node-1"},
			Fingerprint: "fp-5",
		},
		{
			Labels:      LabelSet{"alertname": "HealthCheck", "severity": "info", "instance": "node-3"},
			Fingerprint: "fp-6",
		},
	}

	fmt.Println("활성 Alert:")
	for _, a := range alerts {
		fmt.Printf("  [%s] %v\n", a.Fingerprint, a.Labels)
	}
	fmt.Println()

	// Source 캐시 업데이트
	fmt.Println("--- Source 캐시 업데이트 ---")
	inhibitor.UpdateSources(alerts)
	fmt.Println()

	// 각 Alert에 대해 억제 확인
	fmt.Println("--- 억제 판정 ---")
	for _, alert := range alerts {
		muted, ruleName, sourceID := inhibitor.Mutes(alert)
		if muted {
			fmt.Printf("  [INHIBITED] %v\n", alert.Labels)
			fmt.Printf("    Rule: %q, Source: %s\n", ruleName, sourceID)
		} else {
			fmt.Printf("  [ACTIVE]    %v\n", alert.Labels)
		}
	}

	fmt.Println()
	fmt.Println("=== 동작 원리 요약 ===")
	fmt.Println()
	fmt.Println("규칙 1: critical → warning 억제 (Equal: alertname)")
	fmt.Println("  HighCPU:critical 존재 → HighCPU:warning 억제됨")
	fmt.Println("  HighMemory:warning은 HighMemory:critical 없으므로 통과")
	fmt.Println()
	fmt.Println("규칙 2: critical → info 억제 (Equal: instance)")
	fmt.Println("  node-1:critical 존재 → node-1:info 억제됨")
	fmt.Println("  node-3:info는 node-3:critical 없으므로 통과")
	fmt.Println()
	fmt.Println("핵심: Equal 레이블이 같은 Source가 있을 때만 Target 억제")
	fmt.Println("자기 자신은 억제하지 않음 (Fingerprint 비교)")
}
