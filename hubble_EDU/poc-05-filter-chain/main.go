package main

import (
	"fmt"
	"net"
	"strings"
	"time"
)

// =============================================================================
// Hubble 필터 체인 시뮬레이션
//
// 실제 구현 참조:
//   - pkg/hubble/filters/filters.go: FilterFunc, FilterFuncs, Apply, BuildFilterList
//   - pkg/hubble/filters/ip.go: IPFilter, filterByIPs
//   - pkg/hubble/filters/labels.go: LabelsFilter, FilterByLabelSelectors
//   - pkg/hubble/filters/verdict.go: VerdictFilter
//   - pkg/hubble/filters/port.go: PortFilter
//
// 필터 모델:
//   - Whitelist: OR 조합 (하나라도 매치하면 통과, 비어있으면 모두 통과)
//   - Blacklist: NOR 조합 (하나라도 매치하면 차단, 비어있으면 모두 통과)
//   - 최종: whitelist.MatchOne(ev) && blacklist.MatchNone(ev)
//
// FlowFilter 하나 안의 필드들은 AND 조합:
//   {SourceIP: "10.0.0.1", DestinationPort: "80"} → IP 매치 AND 포트 매치
// =============================================================================

// ── 데이터 모델 ──

type Verdict int

const (
	VerdictForwarded Verdict = 1
	VerdictDropped   Verdict = 2
)

func (v Verdict) String() string {
	switch v {
	case VerdictForwarded:
		return "FORWARDED"
	case VerdictDropped:
		return "DROPPED"
	default:
		return "UNKNOWN"
	}
}

type Endpoint struct {
	Namespace string
	PodName   string
	Labels    []string
}

type Flow struct {
	SrcIP    string
	DstIP    string
	SrcPort  uint32
	DstPort  uint32
	Protocol string
	Verdict  Verdict
	Source   *Endpoint
	Dest     *Endpoint
	NodeName string
}

type Event struct {
	Timestamp time.Time
	Event     any
}

func (ev *Event) GetFlow() *Flow {
	if ev == nil || ev.Event == nil {
		return nil
	}
	if f, ok := ev.Event.(*Flow); ok {
		return f
	}
	return nil
}

// ── FilterFunc 타입 (실제: pkg/hubble/filters/filters.go) ──
// 이벤트를 받아 매치 여부를 반환하는 함수 타입

type FilterFunc func(ev *Event) bool
type FilterFuncs []FilterFunc

// MatchAll은 모든 필터가 매치하면 true (AND)
// 실제: func (fs FilterFuncs) MatchAll(ev *v1.Event) bool
func (fs FilterFuncs) MatchAll(ev *Event) bool {
	for _, f := range fs {
		if !f(ev) {
			return false
		}
	}
	return true
}

// MatchOne은 하나라도 매치하면 true, 빈 리스트면 true (OR)
// 실제: func (fs FilterFuncs) MatchOne(ev *v1.Event) bool
func (fs FilterFuncs) MatchOne(ev *Event) bool {
	if len(fs) == 0 {
		return true
	}
	for _, f := range fs {
		if f(ev) {
			return true
		}
	}
	return false
}

// MatchNone은 어느 것도 매치하지 않으면 true, 빈 리스트면 true (NOR)
// 실제: func (fs FilterFuncs) MatchNone(ev *v1.Event) bool
func (fs FilterFuncs) MatchNone(ev *Event) bool {
	if len(fs) == 0 {
		return true
	}
	for _, f := range fs {
		if f(ev) {
			return false
		}
	}
	return true
}

// Apply는 whitelist와 blacklist를 적용
// 실제: func Apply(whitelist, blacklist FilterFuncs, ev *v1.Event) bool
// whitelist.MatchOne(ev) && blacklist.MatchNone(ev)
func Apply(whitelist, blacklist FilterFuncs, ev *Event) bool {
	return whitelist.MatchOne(ev) && blacklist.MatchNone(ev)
}

// ── FlowFilter (필터 조건 구조체) ──
// 실제: flowpb.FlowFilter

type FlowFilter struct {
	SourceIP        []string
	DestinationIP   []string
	SourcePort      []uint32
	DestinationPort []uint32
	Verdict         []Verdict
	SourceLabel     []string
	DestLabel       []string
	Protocol        []string
	NodeName        []string
}

// ── OnBuildFilter 인터페이스 ──
// 실제: filters.OnBuildFilter

type OnBuildFilter interface {
	OnBuildFilter(ff *FlowFilter) ([]FilterFunc, error)
}

// ── IP 필터 (실제: pkg/hubble/filters/ip.go) ──
// 정확한 IP 매칭과 CIDR 범위 매칭을 모두 지원

type IPFilter struct{}

func (f *IPFilter) OnBuildFilter(ff *FlowFilter) ([]FilterFunc, error) {
	var fs []FilterFunc

	if len(ff.SourceIP) > 0 {
		ipf, err := filterByIPs(ff.SourceIP, func(ev *Event) string {
			if flow := ev.GetFlow(); flow != nil {
				return flow.SrcIP
			}
			return ""
		})
		if err != nil {
			return nil, err
		}
		fs = append(fs, ipf)
	}

	if len(ff.DestinationIP) > 0 {
		ipf, err := filterByIPs(ff.DestinationIP, func(ev *Event) string {
			if flow := ev.GetFlow(); flow != nil {
				return flow.DstIP
			}
			return ""
		})
		if err != nil {
			return nil, err
		}
		fs = append(fs, ipf)
	}

	return fs, nil
}

// filterByIPs는 IP 주소/CIDR 매칭 필터를 생성
// 실제: func filterByIPs(ips []string, getIP func(*v1.Event) string) (FilterFunc, error)
func filterByIPs(ips []string, getIP func(*Event) string) (FilterFunc, error) {
	var addresses []string
	var prefixes []*net.IPNet

	for _, ip := range ips {
		if strings.Contains(ip, "/") {
			_, ipnet, err := net.ParseCIDR(ip)
			if err != nil {
				return nil, fmt.Errorf("잘못된 CIDR: %s: %w", ip, err)
			}
			prefixes = append(prefixes, ipnet)
		} else {
			if net.ParseIP(ip) == nil {
				return nil, fmt.Errorf("잘못된 IP: %s", ip)
			}
			addresses = append(addresses, ip)
		}
	}

	return func(ev *Event) bool {
		eventIP := getIP(ev)
		if eventIP == "" {
			return false
		}

		// 정확한 IP 매칭
		for _, addr := range addresses {
			if eventIP == addr {
				return true
			}
		}

		// CIDR 범위 매칭
		if len(prefixes) > 0 {
			ip := net.ParseIP(eventIP)
			if ip == nil {
				return false
			}
			for _, prefix := range prefixes {
				if prefix.Contains(ip) {
					return true
				}
			}
		}

		return false
	}, nil
}

// ── Verdict 필터 (실제: pkg/hubble/filters/verdict.go) ──

type VerdictFilter struct{}

func (f *VerdictFilter) OnBuildFilter(ff *FlowFilter) ([]FilterFunc, error) {
	if len(ff.Verdict) == 0 {
		return nil, nil
	}
	verdicts := ff.Verdict
	return []FilterFunc{
		func(ev *Event) bool {
			flow := ev.GetFlow()
			if flow == nil {
				return false
			}
			for _, v := range verdicts {
				if flow.Verdict == v {
					return true
				}
			}
			return false
		},
	}, nil
}

// ── Port 필터 (실제: pkg/hubble/filters/port.go) ──

type PortFilter struct{}

func (f *PortFilter) OnBuildFilter(ff *FlowFilter) ([]FilterFunc, error) {
	var fs []FilterFunc

	if len(ff.SourcePort) > 0 {
		ports := ff.SourcePort
		fs = append(fs, func(ev *Event) bool {
			flow := ev.GetFlow()
			if flow == nil {
				return false
			}
			for _, p := range ports {
				if flow.SrcPort == p {
					return true
				}
			}
			return false
		})
	}

	if len(ff.DestinationPort) > 0 {
		ports := ff.DestinationPort
		fs = append(fs, func(ev *Event) bool {
			flow := ev.GetFlow()
			if flow == nil {
				return false
			}
			for _, p := range ports {
				if flow.DstPort == p {
					return true
				}
			}
			return false
		})
	}

	return fs, nil
}

// ── Labels 필터 (실제: pkg/hubble/filters/labels.go) ──

type LabelsFilter struct{}

func (f *LabelsFilter) OnBuildFilter(ff *FlowFilter) ([]FilterFunc, error) {
	var fs []FilterFunc

	if len(ff.SourceLabel) > 0 {
		selectors := ff.SourceLabel
		fs = append(fs, func(ev *Event) bool {
			flow := ev.GetFlow()
			if flow == nil || flow.Source == nil {
				return false
			}
			return matchLabels(flow.Source.Labels, selectors)
		})
	}

	if len(ff.DestLabel) > 0 {
		selectors := ff.DestLabel
		fs = append(fs, func(ev *Event) bool {
			flow := ev.GetFlow()
			if flow == nil || flow.Dest == nil {
				return false
			}
			return matchLabels(flow.Dest.Labels, selectors)
		})
	}

	return fs, nil
}

// matchLabels는 라벨 셀렉터 매칭 (간소화 버전)
// 실제: k8sLabels.Selector.Matches()
func matchLabels(labels []string, selectors []string) bool {
	labelMap := make(map[string]string)
	for _, l := range labels {
		parts := strings.SplitN(l, "=", 2)
		if len(parts) == 2 {
			labelMap[parts[0]] = parts[1]
		}
	}

	for _, selector := range selectors {
		parts := strings.SplitN(selector, "=", 2)
		if len(parts) == 2 {
			if val, ok := labelMap[parts[0]]; ok && val == parts[1] {
				return true
			}
		}
	}
	return false
}

// ── Protocol 필터 ──

type ProtocolFilter struct{}

func (f *ProtocolFilter) OnBuildFilter(ff *FlowFilter) ([]FilterFunc, error) {
	if len(ff.Protocol) == 0 {
		return nil, nil
	}
	protocols := ff.Protocol
	return []FilterFunc{
		func(ev *Event) bool {
			flow := ev.GetFlow()
			if flow == nil {
				return false
			}
			for _, p := range protocols {
				if strings.EqualFold(flow.Protocol, p) {
					return true
				}
			}
			return false
		},
	}, nil
}

// ── BuildFilter / BuildFilterList (실제: filters.BuildFilter, BuildFilterList) ──

func DefaultFilters() []OnBuildFilter {
	return []OnBuildFilter{
		&IPFilter{},
		&VerdictFilter{},
		&PortFilter{},
		&LabelsFilter{},
		&ProtocolFilter{},
	}
}

// BuildFilter는 하나의 FlowFilter로부터 FilterFuncs를 생성
// 각 필터 결과는 AND로 결합됨
func BuildFilter(ff *FlowFilter, auxFilters []OnBuildFilter) (FilterFuncs, error) {
	var fs []FilterFunc

	for _, f := range auxFilters {
		fl, err := f.OnBuildFilter(ff)
		if err != nil {
			return nil, err
		}
		if fl != nil {
			fs = append(fs, fl...)
		}
	}

	return fs, nil
}

// BuildFilterList는 FlowFilter 리스트로부터 FilterFuncs를 생성
// 각 FlowFilter는 OR로 결합 (하나라도 매치하면 통과)
// 각 FlowFilter 내부의 필드는 AND로 결합 (모두 매치해야 통과)
func BuildFilterList(filters []*FlowFilter, auxFilters []OnBuildFilter) (FilterFuncs, error) {
	filterList := make([]FilterFunc, 0, len(filters))

	for _, flowFilter := range filters {
		tf, err := BuildFilter(flowFilter, auxFilters)
		if err != nil {
			return nil, err
		}

		// 각 FlowFilter의 모든 조건이 AND로 매치해야 함
		filterFunc := func(ev *Event) bool {
			return tf.MatchAll(ev)
		}

		filterList = append(filterList, filterFunc)
	}

	return filterList, nil
}

// =============================================================================
// 메인 실행
// =============================================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║     Hubble 필터 체인 시뮬레이션                              ║")
	fmt.Println("║     참조: pkg/hubble/filters/filters.go                     ║")
	fmt.Println("║           pkg/hubble/filters/ip.go                          ║")
	fmt.Println("║           pkg/hubble/filters/labels.go                      ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")

	now := time.Now()

	// 테스트용 이벤트 생성
	events := []*Event{
		{Timestamp: now, Event: &Flow{
			SrcIP: "10.244.0.5", DstIP: "10.244.1.10",
			SrcPort: 54321, DstPort: 8080, Protocol: "TCP",
			Verdict: VerdictForwarded,
			Source: &Endpoint{Namespace: "default", PodName: "frontend-abc",
				Labels: []string{"app=frontend", "tier=web"}},
			Dest: &Endpoint{Namespace: "default", PodName: "backend-xyz",
				Labels: []string{"app=backend", "tier=api"}},
			NodeName: "worker-01",
		}},
		{Timestamp: now.Add(1 * time.Millisecond), Event: &Flow{
			SrcIP: "10.244.2.15", DstIP: "10.244.1.10",
			SrcPort: 44444, DstPort: 3306, Protocol: "TCP",
			Verdict: VerdictDropped,
			Source: &Endpoint{Namespace: "untrusted", PodName: "attacker-pod",
				Labels: []string{"app=unknown"}},
			Dest: &Endpoint{Namespace: "database", PodName: "mysql-0",
				Labels: []string{"app=mysql", "tier=database"}},
			NodeName: "worker-02",
		}},
		{Timestamp: now.Add(2 * time.Millisecond), Event: &Flow{
			SrcIP: "10.244.0.5", DstIP: "10.96.0.10",
			SrcPort: 45678, DstPort: 53, Protocol: "UDP",
			Verdict: VerdictForwarded,
			Source: &Endpoint{Namespace: "default", PodName: "frontend-abc",
				Labels: []string{"app=frontend", "tier=web"}},
			Dest: &Endpoint{Namespace: "kube-system", PodName: "coredns-abc",
				Labels: []string{"app=coredns", "tier=infra"}},
			NodeName: "worker-01",
		}},
		{Timestamp: now.Add(3 * time.Millisecond), Event: &Flow{
			SrcIP: "192.168.1.100", DstIP: "10.244.0.5",
			SrcPort: 33333, DstPort: 443, Protocol: "TCP",
			Verdict: VerdictForwarded,
			Source: &Endpoint{Namespace: "", PodName: "external",
				Labels: []string{"reserved:world"}},
			Dest: &Endpoint{Namespace: "default", PodName: "frontend-abc",
				Labels: []string{"app=frontend", "tier=web"}},
			NodeName: "worker-01",
		}},
		{Timestamp: now.Add(4 * time.Millisecond), Event: &Flow{
			SrcIP: "10.244.3.20", DstIP: "10.244.3.21",
			SrcPort: 22222, DstPort: 9090, Protocol: "TCP",
			Verdict: VerdictForwarded,
			Source: &Endpoint{Namespace: "monitoring", PodName: "prometheus-0",
				Labels: []string{"app=prometheus", "tier=monitoring"}},
			Dest: &Endpoint{Namespace: "monitoring", PodName: "grafana-abc",
				Labels: []string{"app=grafana", "tier=monitoring"}},
			NodeName: "worker-03",
		}},
	}

	// 이벤트 요약 출력
	fmt.Println("\n=== 테스트 이벤트 ===")
	for i, ev := range events {
		flow := ev.GetFlow()
		fmt.Printf("  [%d] %s %s:%d -> %s:%d %s (%s -> %s)\n",
			i+1, flow.Verdict, flow.SrcIP, flow.SrcPort,
			flow.DstIP, flow.DstPort, flow.Protocol,
			flow.Source.PodName, flow.Dest.PodName)
	}

	auxFilters := DefaultFilters()

	// --- 필터 테스트 1: 소스 IP 필터 ---
	fmt.Println("\n=== 테스트 1: 소스 IP 필터 (10.244.0.5) ===")
	testFilter(events, []*FlowFilter{
		{SourceIP: []string{"10.244.0.5"}},
	}, nil, auxFilters)

	// --- 필터 테스트 2: CIDR 필터 ---
	fmt.Println("\n=== 테스트 2: CIDR 필터 (10.244.0.0/16) ===")
	testFilter(events, []*FlowFilter{
		{SourceIP: []string{"10.244.0.0/16"}},
	}, nil, auxFilters)

	// --- 필터 테스트 3: Verdict 필터 ---
	fmt.Println("\n=== 테스트 3: Verdict 필터 (DROPPED) ===")
	testFilter(events, []*FlowFilter{
		{Verdict: []Verdict{VerdictDropped}},
	}, nil, auxFilters)

	// --- 필터 테스트 4: Port 필터 ---
	fmt.Println("\n=== 테스트 4: 목적지 포트 필터 (80, 443, 8080) ===")
	testFilter(events, []*FlowFilter{
		{DestinationPort: []uint32{80, 443, 8080}},
	}, nil, auxFilters)

	// --- 필터 테스트 5: Label 필터 ---
	fmt.Println("\n=== 테스트 5: 소스 라벨 필터 (app=frontend) ===")
	testFilter(events, []*FlowFilter{
		{SourceLabel: []string{"app=frontend"}},
	}, nil, auxFilters)

	// --- 필터 테스트 6: AND 조합 (하나의 FlowFilter 내) ---
	fmt.Println("\n=== 테스트 6: AND 조합 (소스 IP=10.244.0.5 AND 목적지 포트=8080) ===")
	testFilter(events, []*FlowFilter{
		{SourceIP: []string{"10.244.0.5"}, DestinationPort: []uint32{8080}},
	}, nil, auxFilters)

	// --- 필터 테스트 7: OR 조합 (여러 FlowFilter) ---
	fmt.Println("\n=== 테스트 7: OR 조합 (포트 53 OR 포트 3306) ===")
	testFilter(events, []*FlowFilter{
		{DestinationPort: []uint32{53}},
		{DestinationPort: []uint32{3306}},
	}, nil, auxFilters)

	// --- 필터 테스트 8: Whitelist + Blacklist ---
	fmt.Println("\n=== 테스트 8: Whitelist(10.244.0.0/16) + Blacklist(verdict=DROPPED) ===")
	testFilter(events,
		[]*FlowFilter{{SourceIP: []string{"10.244.0.0/16"}}},       // whitelist
		[]*FlowFilter{{Verdict: []Verdict{VerdictDropped}}},         // blacklist
		auxFilters,
	)

	// --- 필터 테스트 9: Protocol 필터 ---
	fmt.Println("\n=== 테스트 9: 프로토콜 필터 (UDP) ===")
	testFilter(events, []*FlowFilter{
		{Protocol: []string{"UDP"}},
	}, nil, auxFilters)

	// 필터 논리 다이어그램
	fmt.Println("\n" + `
┌──────────────────────────────────────────────────────────────────────┐
│                    필터 체인 논리 구조                                │
├──────────────────────────────────────────────────────────────────────┤
│                                                                      │
│  FlowFilter #1 (AND):    FlowFilter #2 (AND):                       │
│  ┌─────────────────┐     ┌─────────────────┐                        │
│  │ srcIP=10.0.0.1  │     │ dstPort=443     │                        │
│  │ AND             │     │ AND             │                        │
│  │ dstPort=8080    │     │ verdict=DROP    │                        │
│  └────────┬────────┘     └────────┬────────┘                        │
│           │                       │                                  │
│           └───────── OR ──────────┘                                  │
│                      │                                               │
│                Whitelist Result                                       │
│                      │                                               │
│                     AND                                              │
│                      │                                               │
│  FlowFilter #3 (AND):                                               │
│  ┌─────────────────┐                                                │
│  │ srcLabel=test   │ ← NOR (매치하면 차단)                           │
│  └────────┬────────┘                                                │
│           │                                                          │
│     Blacklist Result                                                 │
│           │                                                          │
│     최종: whitelist.MatchOne(ev) && blacklist.MatchNone(ev)          │
│                                                                      │
│  핵심 코드:                                                          │
│    func Apply(wl, bl FilterFuncs, ev *Event) bool {                  │
│        return wl.MatchOne(ev) && bl.MatchNone(ev)                    │
│    }                                                                 │
│                                                                      │
└──────────────────────────────────────────────────────────────────────┘`)
}

func testFilter(events []*Event, whitelistFilters, blacklistFilters []*FlowFilter, auxFilters []OnBuildFilter) {
	whitelist, err := BuildFilterList(whitelistFilters, auxFilters)
	if err != nil {
		fmt.Printf("  whitelist 빌드 에러: %v\n", err)
		return
	}

	var blacklist FilterFuncs
	if blacklistFilters != nil {
		blacklist, err = BuildFilterList(blacklistFilters, auxFilters)
		if err != nil {
			fmt.Printf("  blacklist 빌드 에러: %v\n", err)
			return
		}
	}

	matched := 0
	for i, ev := range events {
		flow := ev.GetFlow()
		result := Apply(whitelist, blacklist, ev)
		mark := "  "
		if result {
			mark = "->"
			matched++
		}
		fmt.Printf("  %s [%d] %s %s:%d -> %s:%d %s (%s)\n",
			mark, i+1, flow.Verdict, flow.SrcIP, flow.SrcPort,
			flow.DstIP, flow.DstPort, flow.Protocol,
			flow.Source.PodName)
	}
	fmt.Printf("  결과: %d/%d 이벤트 매치\n", matched, len(events))
}
