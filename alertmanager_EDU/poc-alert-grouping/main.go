// Alertmanager Alert Grouping PoC
//
// Alertmanager의 Alert 그룹핑 메커니즘을 시뮬레이션한다.
// dispatch/dispatch.go의 groupAlert()과 AggregationGroup을 재현한다.
//
// 핵심 개념:
//   - group_by 레이블로 그룹 키 생성
//   - group_by: [...] (특정 레이블) vs group_by: ['...'] (전체 레이블)
//   - 그룹 내 Alert 수집 및 일괄 전송
//   - AlertGroup 데이터 구조
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

// Alert는 수신된 알림이다.
type Alert struct {
	Labels LabelSet
	Status string
}

func (a *Alert) String() string {
	var parts []string
	keys := make([]string, 0, len(a.Labels))
	for k := range a.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%q", k, a.Labels[k]))
	}
	return fmt.Sprintf("{%s}", strings.Join(parts, ", "))
}

// AlertGroup은 그룹핑된 Alert 집합이다.
type AlertGroup struct {
	GroupKey    string
	GroupLabels LabelSet
	Alerts     []*Alert
	Receiver   string
}

// GroupingConfig는 그룹핑 설정이다.
type GroupingConfig struct {
	GroupBy    []string // 그룹핑 레이블
	GroupByAll bool     // '...'으로 모든 레이블로 그룹핑
}

// Grouper는 Alert 그룹핑을 수행한다.
type Grouper struct {
	config GroupingConfig
	groups map[string]*AlertGroup
}

// NewGrouper는 새 Grouper를 생성한다.
func NewGrouper(config GroupingConfig) *Grouper {
	return &Grouper{
		config: config,
		groups: make(map[string]*AlertGroup),
	}
}

// makeGroupKey는 Alert의 레이블에서 그룹 키를 생성한다.
func (g *Grouper) makeGroupKey(labels LabelSet) (string, LabelSet) {
	groupLabels := make(LabelSet)

	if g.config.GroupByAll {
		// group_by: ['...'] → 모든 레이블로 그룹핑
		for k, v := range labels {
			groupLabels[k] = v
		}
	} else {
		// group_by: [label1, label2] → 특정 레이블만
		for _, key := range g.config.GroupBy {
			if val, ok := labels[key]; ok {
				groupLabels[key] = val
			}
		}
	}

	// 정렬된 키로 안정적인 그룹 키 생성
	keys := make([]string, 0, len(groupLabels))
	for k := range groupLabels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%q", k, groupLabels[k]))
	}
	return strings.Join(parts, ","), groupLabels
}

// AddAlert는 Alert를 적절한 그룹에 추가한다.
func (g *Grouper) AddAlert(alert *Alert, receiver string) {
	key, groupLabels := g.makeGroupKey(alert.Labels)

	group, exists := g.groups[key]
	if !exists {
		group = &AlertGroup{
			GroupKey:    key,
			GroupLabels: groupLabels,
			Receiver:   receiver,
		}
		g.groups[key] = group
	}

	group.Alerts = append(group.Alerts, alert)
}

// GetGroups는 모든 그룹을 반환한다.
func (g *Grouper) GetGroups() []*AlertGroup {
	result := make([]*AlertGroup, 0, len(g.groups))
	for _, group := range g.groups {
		result = append(result, group)
	}
	// 그룹 키로 정렬
	sort.Slice(result, func(i, j int) bool {
		return result[i].GroupKey < result[j].GroupKey
	})
	return result
}

func main() {
	fmt.Println("=== Alertmanager Alert Grouping PoC ===")
	fmt.Println()

	// 테스트 Alert
	alerts := []*Alert{
		{Labels: LabelSet{"alertname": "HighCPU", "severity": "critical", "instance": "node-1", "cluster": "prod"}},
		{Labels: LabelSet{"alertname": "HighCPU", "severity": "warning", "instance": "node-2", "cluster": "prod"}},
		{Labels: LabelSet{"alertname": "HighCPU", "severity": "critical", "instance": "node-3", "cluster": "staging"}},
		{Labels: LabelSet{"alertname": "HighMemory", "severity": "critical", "instance": "node-1", "cluster": "prod"}},
		{Labels: LabelSet{"alertname": "HighMemory", "severity": "warning", "instance": "node-4", "cluster": "prod"}},
		{Labels: LabelSet{"alertname": "DiskFull", "severity": "critical", "instance": "node-2", "cluster": "prod"}},
	}

	fmt.Println("입력 Alert:")
	for _, a := range alerts {
		fmt.Printf("  %s\n", a)
	}
	fmt.Println()

	// 시나리오 1: group_by: [alertname]
	fmt.Println("--- 시나리오 1: group_by: [alertname] ---")
	grouper1 := NewGrouper(GroupingConfig{GroupBy: []string{"alertname"}})
	for _, a := range alerts {
		grouper1.AddAlert(a, "default")
	}
	printGroups(grouper1.GetGroups())

	// 시나리오 2: group_by: [alertname, cluster]
	fmt.Println("--- 시나리오 2: group_by: [alertname, cluster] ---")
	grouper2 := NewGrouper(GroupingConfig{GroupBy: []string{"alertname", "cluster"}})
	for _, a := range alerts {
		grouper2.AddAlert(a, "default")
	}
	printGroups(grouper2.GetGroups())

	// 시나리오 3: group_by: [severity]
	fmt.Println("--- 시나리오 3: group_by: [severity] ---")
	grouper3 := NewGrouper(GroupingConfig{GroupBy: []string{"severity"}})
	for _, a := range alerts {
		grouper3.AddAlert(a, "default")
	}
	printGroups(grouper3.GetGroups())

	// 시나리오 4: group_by: ['...'] (모든 레이블)
	fmt.Println("--- 시나리오 4: group_by: ['...'] (GroupByAll) ---")
	grouper4 := NewGrouper(GroupingConfig{GroupByAll: true})
	for _, a := range alerts {
		grouper4.AddAlert(a, "default")
	}
	printGroups(grouper4.GetGroups())

	// 시나리오 5: group_by: [] (빈 그룹핑 → 모든 Alert 하나의 그룹)
	fmt.Println("--- 시나리오 5: group_by: [] (모든 Alert 하나의 그룹) ---")
	grouper5 := NewGrouper(GroupingConfig{GroupBy: []string{}})
	for _, a := range alerts {
		grouper5.AddAlert(a, "default")
	}
	printGroups(grouper5.GetGroups())

	fmt.Println("=== 동작 원리 요약 ===")
	fmt.Println("1. group_by 레이블의 값으로 그룹 키 생성")
	fmt.Println("2. 같은 그룹 키 → 같은 AggregationGroup")
	fmt.Println("3. group_by: ['...'] → 모든 레이블로 그룹핑 (각 Alert가 별도 그룹)")
	fmt.Println("4. group_by: [] → 모든 Alert가 하나의 그룹")
	fmt.Println("5. 그룹 단위로 알림 전송 (하나의 Slack 메시지에 그룹의 모든 Alert)")
}

func printGroups(groups []*AlertGroup) {
	fmt.Printf("그룹 수: %d\n", len(groups))
	for _, g := range groups {
		fmt.Printf("\n  그룹 키: %q\n", g.GroupKey)
		fmt.Printf("  그룹 레이블: %v\n", g.GroupLabels)
		fmt.Printf("  Alert %d개:\n", len(g.Alerts))
		for _, a := range g.Alerts {
			fmt.Printf("    %s\n", a)
		}
	}
	fmt.Println()
}
