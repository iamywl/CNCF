// poc-35-proxy-nftables: nftables 프록시 모드 및 Topology Aware Routing 시뮬레이션
//
// 시뮬레이션 내용:
//   1. nftables 테이블/체인/규칙 구조 모델링
//   2. Service→Endpoint DNAT 매핑 및 numgen 로드밸런싱
//   3. Topology-Aware 엔드포인트 선택 (Zone/Node hint)
//   4. CategorizeEndpoints 알고리즘 재현
//   5. 증분 동기화(nftElementStorage) 시뮬레이션
//
// 실행: go run main.go
// 외부 의존성 없이 Go 표준 라이브러리만 사용

package main

import (
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"math/rand"
	"net"
	"sort"
	"strconv"
	"strings"
)

// =============================================================================
// 1. nftables 테이블/체인/규칙 구조 모델링
// =============================================================================

// NFTTable은 nftables 테이블을 모델링한다.
// 실제 kube-proxy는 "kube-proxy" 단일 테이블을 사용한다.
type NFTTable struct {
	Name   string
	Chains map[string]*NFTChain
	Maps   map[string]*NFTMap
	Sets   map[string]*NFTSet
}

// NFTChain은 base chain 또는 regular chain을 모델링한다.
type NFTChain struct {
	Name     string
	Type     string // "filter" 또는 "nat"
	Hook     string // "prerouting", "input", "forward", "output", "postrouting"
	Priority int    // -110, -100, 0, 100
	Rules    []*NFTRule
	IsBase   bool
}

// NFTRule은 nftables 규칙을 모델링한다.
type NFTRule struct {
	Match   string // 매칭 조건
	Action  string // 액션 (jump, goto, dnat, masquerade 등)
	Comment string
}

// NFTMap은 verdict map을 모델링한다.
// 실제: ip . proto . port -> verdict
type NFTMap struct {
	Name     string
	Elements map[string]string // key -> verdict
}

// NFTSet은 nftables set을 모델링한다.
type NFTSet struct {
	Name     string
	Elements map[string]bool
}

func newNFTTable(name string) *NFTTable {
	return &NFTTable{
		Name:   name,
		Chains: make(map[string]*NFTChain),
		Maps:   make(map[string]*NFTMap),
		Sets:   make(map[string]*NFTSet),
	}
}

func (t *NFTTable) addBaseChain(name, chainType, hook string, priority int) *NFTChain {
	chain := &NFTChain{
		Name:     name,
		Type:     chainType,
		Hook:     hook,
		Priority: priority,
		IsBase:   true,
	}
	t.Chains[name] = chain
	return chain
}

func (t *NFTTable) addRegularChain(name string) *NFTChain {
	chain := &NFTChain{
		Name: name,
	}
	t.Chains[name] = chain
	return chain
}

func (c *NFTChain) addRule(match, action, comment string) {
	c.Rules = append(c.Rules, &NFTRule{
		Match:   match,
		Action:  action,
		Comment: comment,
	})
}

func (t *NFTTable) addMap(name string) *NFTMap {
	m := &NFTMap{
		Name:     name,
		Elements: make(map[string]string),
	}
	t.Maps[name] = m
	return m
}

func (t *NFTTable) addSet(name string) *NFTSet {
	s := &NFTSet{
		Name:     name,
		Elements: make(map[string]bool),
	}
	t.Sets[name] = s
	return s
}

// printTable은 테이블 구조를 출력한다.
func (t *NFTTable) printTable() {
	fmt.Printf("table inet %s {\n", t.Name)

	// Sets 출력
	setNames := make([]string, 0, len(t.Sets))
	for n := range t.Sets {
		setNames = append(setNames, n)
	}
	sort.Strings(setNames)
	for _, name := range setNames {
		s := t.Sets[name]
		fmt.Printf("  set %s {\n", s.Name)
		fmt.Printf("    type ipv4_addr\n")
		if len(s.Elements) > 0 {
			elems := make([]string, 0, len(s.Elements))
			for e := range s.Elements {
				elems = append(elems, e)
			}
			sort.Strings(elems)
			fmt.Printf("    elements = { %s }\n", strings.Join(elems, ", "))
		}
		fmt.Printf("  }\n\n")
	}

	// Maps 출력
	mapNames := make([]string, 0, len(t.Maps))
	for n := range t.Maps {
		mapNames = append(mapNames, n)
	}
	sort.Strings(mapNames)
	for _, name := range mapNames {
		m := t.Maps[name]
		fmt.Printf("  map %s {\n", m.Name)
		fmt.Printf("    type ipv4_addr . inet_proto . inet_service : verdict\n")
		if len(m.Elements) > 0 {
			elems := make([]string, 0, len(m.Elements))
			for k, v := range m.Elements {
				elems = append(elems, fmt.Sprintf("      %s : %s", k, v))
			}
			sort.Strings(elems)
			fmt.Printf("    elements = {\n%s\n    }\n", strings.Join(elems, ",\n"))
		}
		fmt.Printf("  }\n\n")
	}

	// Chains 출력 (base chains 먼저)
	baseChains := make([]*NFTChain, 0)
	regularChains := make([]*NFTChain, 0)
	for _, c := range t.Chains {
		if c.IsBase {
			baseChains = append(baseChains, c)
		} else {
			regularChains = append(regularChains, c)
		}
	}
	sort.Slice(baseChains, func(i, j int) bool { return baseChains[i].Priority < baseChains[j].Priority })
	sort.Slice(regularChains, func(i, j int) bool { return regularChains[i].Name < regularChains[j].Name })

	for _, c := range baseChains {
		fmt.Printf("  chain %s {\n", c.Name)
		fmt.Printf("    type %s hook %s priority %d\n", c.Type, c.Hook, c.Priority)
		for _, r := range c.Rules {
			if r.Comment != "" {
				fmt.Printf("    %s %s  # %s\n", r.Match, r.Action, r.Comment)
			} else {
				fmt.Printf("    %s %s\n", r.Match, r.Action)
			}
		}
		fmt.Printf("  }\n\n")
	}

	for _, c := range regularChains {
		fmt.Printf("  chain %s {\n", c.Name)
		for _, r := range c.Rules {
			if r.Comment != "" {
				fmt.Printf("    %s %s  # %s\n", r.Match, r.Action, r.Comment)
			} else {
				fmt.Printf("    %s %s\n", r.Match, r.Action)
			}
		}
		fmt.Printf("  }\n\n")
	}

	fmt.Println("}")
}

// =============================================================================
// 2. Service/Endpoint 모델링
// =============================================================================

// ServicePort는 서비스의 포트 정보를 모델링한다.
type ServicePort struct {
	Namespace            string
	Name                 string
	Protocol             string
	Port                 int
	ClusterIP            string
	ExternalTrafficLocal bool // externalTrafficPolicy: Local
	InternalTrafficLocal bool // internalTrafficPolicy: Local
}

func (sp *ServicePort) String() string {
	return fmt.Sprintf("%s/%s:%s:%d", sp.Namespace, sp.Name, sp.Protocol, sp.Port)
}

func (sp *ServicePort) UsesClusterEndpoints() bool {
	return !sp.InternalTrafficLocal || !sp.ExternalTrafficLocal
}

func (sp *ServicePort) UsesLocalEndpoints() bool {
	return sp.ExternalTrafficLocal || sp.InternalTrafficLocal
}

// Endpoint는 kube-proxy의 Endpoint 인터페이스를 모델링한다.
// pkg/proxy/endpoint.go의 BaseEndpointInfo에 대응
type Endpoint struct {
	IP          string
	Port        int
	NodeName    string
	Zone        string
	IsLocalNode bool
	Ready       bool
	Serving     bool
	Terminating bool
	ZoneHints   map[string]bool // ForZones에서 추출
	NodeHints   map[string]bool // ForNodes에서 추출
}

func (ep *Endpoint) String() string {
	return net.JoinHostPort(ep.IP, strconv.Itoa(ep.Port))
}

func (ep *Endpoint) IsReady() bool    { return ep.Ready }
func (ep *Endpoint) IsServing() bool  { return ep.Serving }
func (ep *Endpoint) IsLocal() bool    { return ep.IsLocalNode }
func (ep *Endpoint) HasZoneHint(zone string) bool {
	return ep.ZoneHints != nil && ep.ZoneHints[zone]
}
func (ep *Endpoint) HasNodeHint(node string) bool {
	return ep.NodeHints != nil && ep.NodeHints[node]
}

// =============================================================================
// 3. 체인 이름 생성 (hashAndTruncate 시뮬레이션)
// =============================================================================

const chainNameBaseLengthMax = 240

func hashAndTruncate(name string) string {
	hash := sha256.Sum256([]byte(name))
	encoded := base32.StdEncoding.EncodeToString(hash[:])
	result := encoded[:8] + "-" + name
	if len(result) > chainNameBaseLengthMax {
		result = result[:chainNameBaseLengthMax-3] + "..."
	}
	return result
}

func serviceChainName(sp *ServicePort) string {
	base := fmt.Sprintf("%s/%s/%s/%d", sp.Namespace, sp.Name, sp.Protocol, sp.Port)
	return "service-" + hashAndTruncate(base)
}

func endpointChainName(sp *ServicePort, ep *Endpoint) string {
	base := fmt.Sprintf("%s/%s/%s/%d__%s/%d",
		sp.Namespace, sp.Name, sp.Protocol, sp.Port,
		ep.IP, ep.Port)
	return "endpoint-" + hashAndTruncate(base)
}

// =============================================================================
// 4. Topology Aware Routing -- topologyModeFromHints 재현
// =============================================================================

const (
	TopologyModeNone           = ""
	TopologyModePreferSameZone = "PreferSameZone"
	TopologyModePreferSameNode = "PreferSameNode"
)

// topologyModeFromHints는 pkg/proxy/topology.go의 동일 함수를 재현한다.
// preferSameNodeEnabled는 PreferSameTrafficDistribution feature gate를 시뮬레이션한다.
func topologyModeFromHints(endpoints []*Endpoint, nodeName, zone string, preferSameNodeEnabled bool) string {
	hasReadyEndpoints := false
	hasEndpointForNode := false
	allEndpointsHaveNodeHints := true
	hasEndpointForZone := false
	allEndpointsHaveZoneHints := true

	for _, ep := range endpoints {
		if !ep.IsReady() {
			continue
		}
		hasReadyEndpoints = true

		// Node hint 검사
		if len(ep.NodeHints) == 0 {
			allEndpointsHaveNodeHints = false
		} else if ep.HasNodeHint(nodeName) {
			hasEndpointForNode = true
		}

		// Zone hint 검사
		if len(ep.ZoneHints) == 0 {
			allEndpointsHaveZoneHints = false
		} else if ep.HasZoneHint(zone) {
			hasEndpointForZone = true
		}
	}

	if !hasReadyEndpoints {
		return TopologyModeNone
	}

	// 1차: PreferSameNode (feature gate 필요)
	if preferSameNodeEnabled {
		if allEndpointsHaveNodeHints {
			if hasEndpointForNode {
				return TopologyModePreferSameNode
			}
			fmt.Printf("    [hint] 이 노드(%s)에 대한 node hint가 없어 무시\n", nodeName)
		} else {
			fmt.Printf("    [hint] 일부 EP에 node hint가 없어 무시\n")
		}
	}

	// 2차: PreferSameZone
	if allEndpointsHaveZoneHints {
		if hasEndpointForZone {
			return TopologyModePreferSameZone
		}
		if zone == "" {
			fmt.Printf("    [hint] 노드에 zone 라벨이 없어 무시\n")
		} else {
			fmt.Printf("    [hint] 이 zone(%s)에 대한 zone hint가 없어 무시\n", zone)
		}
	} else {
		fmt.Printf("    [hint] 일부 EP에 zone hint가 없어 무시\n")
	}

	return TopologyModeNone
}

// availableForTopology는 pkg/proxy/topology.go의 동일 함수를 재현한다.
func availableForTopology(ep *Endpoint, mode, nodeName, zone string) bool {
	switch mode {
	case TopologyModeNone:
		return true
	case TopologyModePreferSameNode:
		return ep.HasNodeHint(nodeName)
	case TopologyModePreferSameZone:
		return ep.HasZoneHint(zone)
	default:
		return false
	}
}

// =============================================================================
// 5. CategorizeEndpoints 알고리즘 재현
// =============================================================================

// CategorizeEndpoints는 pkg/proxy/topology.go의 핵심 알고리즘을 재현한다.
func CategorizeEndpoints(
	endpoints []*Endpoint,
	svcPort *ServicePort,
	nodeName string,
	zone string,
	preferSameNodeEnabled bool,
) (clusterEPs, localEPs, allReachableEPs []*Endpoint, hasAnyEndpoints bool) {

	if len(endpoints) == 0 {
		return
	}

	var topologyMode string

	// Cluster 트래픽 정책 처리
	if svcPort.UsesClusterEndpoints() {
		topologyMode = topologyModeFromHints(endpoints, nodeName, zone, preferSameNodeEnabled)
		fmt.Printf("    토폴로지 모드: %q\n", topologyMode)

		// Ready + 토폴로지 필터링
		for _, ep := range endpoints {
			if ep.IsReady() && availableForTopology(ep, topologyMode, nodeName, zone) {
				clusterEPs = append(clusterEPs, ep)
			}
		}

		// Fallback: Ready EP 없으면 Serving+Terminating 사용
		if len(clusterEPs) == 0 {
			for _, ep := range endpoints {
				if ep.IsServing() && ep.Terminating {
					clusterEPs = append(clusterEPs, ep)
				}
			}
			if len(clusterEPs) > 0 {
				fmt.Printf("    [fallback] Serving+Terminating EP %d개 사용\n", len(clusterEPs))
			}
		}

		if len(clusterEPs) > 0 {
			hasAnyEndpoints = true
		}
	}

	// Local 트래픽 정책 불필요 시 조기 반환
	if !svcPort.UsesLocalEndpoints() {
		allReachableEPs = clusterEPs
		return
	}

	// Local 트래픽 정책 처리
	var hasLocalReady, hasLocalServingTerminating bool
	for _, ep := range endpoints {
		if ep.IsReady() {
			hasAnyEndpoints = true
			if ep.IsLocal() {
				hasLocalReady = true
			}
		} else if ep.IsServing() && ep.Terminating {
			hasAnyEndpoints = true
			if ep.IsLocal() {
				hasLocalServingTerminating = true
			}
		}
	}

	useServingTerminating := false
	if hasLocalReady {
		for _, ep := range endpoints {
			if ep.IsLocal() && ep.IsReady() {
				localEPs = append(localEPs, ep)
			}
		}
	} else if hasLocalServingTerminating {
		useServingTerminating = true
		for _, ep := range endpoints {
			if ep.IsLocal() && ep.IsServing() && ep.Terminating {
				localEPs = append(localEPs, ep)
			}
		}
	}

	// allReachableEndpoints 결정
	if !svcPort.UsesClusterEndpoints() {
		allReachableEPs = localEPs
		return
	}

	if topologyMode == "" && !useServingTerminating {
		// clusterEndpoints는 모든 Ready EP의 상위 집합
		allReachableEPs = clusterEPs
		return
	}

	// Cluster + Local 합집합
	epMap := make(map[string]*Endpoint)
	for _, ep := range clusterEPs {
		epMap[ep.String()] = ep
	}
	for _, ep := range localEPs {
		epMap[ep.String()] = ep
	}
	for _, ep := range epMap {
		allReachableEPs = append(allReachableEPs, ep)
	}
	sort.Slice(allReachableEPs, func(i, j int) bool {
		return allReachableEPs[i].String() < allReachableEPs[j].String()
	})

	return
}

// =============================================================================
// 6. nftElementStorage 시뮬레이션 (증분 동기화)
// =============================================================================

type NFTElementStorage struct {
	elements      map[string]string // key -> value
	leftoverKeys  map[string]bool
	containerName string
}

func newNFTElementStorage(name string) *NFTElementStorage {
	return &NFTElementStorage{
		elements:      make(map[string]string),
		leftoverKeys:  make(map[string]bool),
		containerName: name,
	}
}

func (s *NFTElementStorage) resetLeftoverKeys() {
	s.leftoverKeys = make(map[string]bool)
	for k := range s.elements {
		s.leftoverKeys[k] = true
	}
}

func (s *NFTElementStorage) ensureElem(key, value string) string {
	existing, exists := s.elements[key]
	if exists {
		if existing != value {
			s.elements[key] = value
			delete(s.leftoverKeys, key)
			return "UPDATED"
		}
		delete(s.leftoverKeys, key)
		return "UNCHANGED"
	}
	s.elements[key] = value
	return "ADDED"
}

func (s *NFTElementStorage) cleanupLeftoverKeys() []string {
	var removed []string
	for key := range s.leftoverKeys {
		removed = append(removed, key)
		delete(s.elements, key)
	}
	s.leftoverKeys = make(map[string]bool)
	sort.Strings(removed)
	return removed
}

// =============================================================================
// 7. DNAT 로드밸런싱 시뮬레이션 (numgen random mod N)
// =============================================================================

func simulateLoadBalancing(endpoints []*Endpoint, numRequests int) map[string]int {
	counts := make(map[string]int)
	for i := 0; i < numRequests; i++ {
		// numgen random mod N -- 커널 의사난수와 동등
		idx := rand.Intn(len(endpoints))
		ep := endpoints[idx]
		counts[ep.String()]++
	}
	return counts
}

// =============================================================================
// 8. nftables 테이블 구조 구축 (setupNFTables 시뮬레이션)
// =============================================================================

func setupKubeProxyTable() *NFTTable {
	t := newNFTTable("kube-proxy")

	// Base chains (priority 순서대로)
	t.addBaseChain("filter-prerouting-pre-dnat", "filter", "prerouting", -110)
	t.addBaseChain("filter-output-pre-dnat", "filter", "output", -110)
	t.addBaseChain("nat-prerouting", "nat", "prerouting", -100)
	t.addBaseChain("nat-output", "nat", "output", -100)
	t.addBaseChain("filter-input", "filter", "input", 0)
	t.addBaseChain("filter-forward", "filter", "forward", 0)
	t.addBaseChain("filter-output", "filter", "output", 0)
	t.addBaseChain("nat-postrouting", "nat", "postrouting", 100)

	// Regular chains
	services := t.addRegularChain("services")
	_ = t.addRegularChain("masquerading")
	_ = t.addRegularChain("service-endpoints-check")
	_ = t.addRegularChain("nodeport-endpoints-check")
	_ = t.addRegularChain("firewall-check")
	_ = t.addRegularChain("cluster-ips-check")
	_ = t.addRegularChain("reject-chain")

	// Jump rules
	t.Chains["filter-prerouting-pre-dnat"].addRule("ct state new", "jump firewall-check", "")
	t.Chains["filter-output-pre-dnat"].addRule("ct state new", "jump firewall-check", "")
	t.Chains["nat-prerouting"].addRule("", "jump services", "")
	t.Chains["nat-output"].addRule("", "jump services", "")
	t.Chains["nat-postrouting"].addRule("", "jump masquerading", "")
	t.Chains["filter-input"].addRule("ct state new", "jump service-endpoints-check", "")
	t.Chains["filter-input"].addRule("ct state new", "jump nodeport-endpoints-check", "")
	t.Chains["filter-forward"].addRule("ct state new", "jump service-endpoints-check", "")
	t.Chains["filter-forward"].addRule("ct state new", "jump cluster-ips-check", "")
	t.Chains["filter-output"].addRule("ct state new", "jump service-endpoints-check", "")
	t.Chains["filter-output"].addRule("ct state new", "jump cluster-ips-check", "")

	// Masquerading rule
	t.Chains["masquerading"].addRule(
		"mark and 0x00004000 != 0",
		"mark set mark xor 0x00004000 masquerade fully-random",
		"",
	)

	// Reject chain
	t.Chains["reject-chain"].addRule("", "reject", "")

	// Maps and Sets
	t.addMap("service-ips")
	t.addMap("service-nodeports")
	t.addMap("no-endpoint-services")
	t.addMap("no-endpoint-nodeports")
	t.addMap("firewall-ips")
	t.addSet("cluster-ips")
	t.addSet("nodeport-ips")

	// services 체인에 Map 기반 디스패치 규칙 추가
	services.addRule("ip daddr . meta l4proto . th dport", "vmap @service-ips", "")
	services.addRule("fib daddr type local meta l4proto . th dport", "vmap @service-nodeports", "")

	return t
}

// =============================================================================
// 메인 실행
// =============================================================================

func main() {
	fmt.Println("=" + strings.Repeat("=", 79))
	fmt.Println(" PoC 35: nftables 프록시 모드 및 Topology Aware Routing 시뮬레이션")
	fmt.Println("=" + strings.Repeat("=", 79))

	// =========================================================================
	// 데모 1: nftables 테이블/체인 구조
	// =========================================================================
	fmt.Println("\n" + strings.Repeat("-", 80))
	fmt.Println("[데모 1] nftables kube-proxy 테이블 구조")
	fmt.Println(strings.Repeat("-", 80))

	table := setupKubeProxyTable()

	fmt.Println("\nBase Chains (netfilter 훅에 직접 연결):")
	fmt.Println("  filter-prerouting-pre-dnat  : priority -110 (DNAT 이전 방화벽)")
	fmt.Println("  filter-output-pre-dnat      : priority -110")
	fmt.Println("  nat-prerouting              : priority -100 (DNAT)")
	fmt.Println("  nat-output                  : priority -100")
	fmt.Println("  filter-input                : priority   0  (엔드포인트 검사)")
	fmt.Println("  filter-forward              : priority   0")
	fmt.Println("  filter-output               : priority   0")
	fmt.Println("  nat-postrouting             : priority  100 (SNAT/masquerade)")

	fmt.Println("\nRegular Chains (jump으로 호출):")
	fmt.Println("  services                    : 서비스 IP/NodePort Map 기반 디스패치")
	fmt.Println("  masquerading                : SNAT 마스커레이딩")
	fmt.Println("  service-endpoints-check     : 엔드포인트 없는 서비스 필터링")
	fmt.Println("  firewall-check              : LoadBalancerSourceRanges")
	fmt.Println("  cluster-ips-check           : 잘못된 ClusterIP 차단")

	fmt.Println("\nMaps/Sets:")
	fmt.Println("  service-ips (Map)           : ClusterIP.proto.port -> goto service chain")
	fmt.Println("  service-nodeports (Map)     : proto.port -> goto external chain")
	fmt.Println("  cluster-ips (Set)           : 활성 ClusterIP 목록")
	fmt.Println("  no-endpoint-services (Map)  : EP 없는 서비스 -> reject/drop")
	fmt.Println("  firewall-ips (Map)          : LB IP -> goto firewall chain")

	// =========================================================================
	// 데모 2: Service->Endpoint DNAT 매핑
	// =========================================================================
	fmt.Println("\n" + strings.Repeat("-", 80))
	fmt.Println("[데모 2] Service -> Endpoint DNAT 매핑 및 numgen 로드밸런싱")
	fmt.Println(strings.Repeat("-", 80))

	svc := &ServicePort{
		Namespace: "default",
		Name:      "web-service",
		Protocol:  "tcp",
		Port:      80,
		ClusterIP: "10.96.0.100",
	}

	endpoints := []*Endpoint{
		{IP: "10.244.1.10", Port: 8080, NodeName: "node-1", Zone: "us-east-1a",
			Ready: true, Serving: true, IsLocalNode: true},
		{IP: "10.244.2.20", Port: 8080, NodeName: "node-2", Zone: "us-east-1a",
			Ready: true, Serving: true},
		{IP: "10.244.3.30", Port: 8080, NodeName: "node-3", Zone: "us-east-1b",
			Ready: true, Serving: true},
	}

	// 체인 이름 생성
	svcChain := serviceChainName(svc)
	fmt.Printf("\n서비스: %s (ClusterIP: %s)\n", svc.String(), svc.ClusterIP)
	fmt.Printf("서비스 체인: %s\n", svcChain)

	// service-ips Map에 등록
	svcIPsMap := table.Maps["service-ips"]
	mapKey := fmt.Sprintf("%s . %s . %d", svc.ClusterIP, svc.Protocol, svc.Port)
	svcIPsMap.Elements[mapKey] = fmt.Sprintf("goto %s", svcChain)
	fmt.Printf("\nservice-ips Map 등록:\n")
	fmt.Printf("  %s -> %s\n", mapKey, svcIPsMap.Elements[mapKey])

	// cluster-ips Set에 등록
	table.Sets["cluster-ips"].Elements[svc.ClusterIP] = true
	fmt.Printf("\ncluster-ips Set 등록:\n")
	fmt.Printf("  %s\n", svc.ClusterIP)

	// 서비스 체인 + 엔드포인트 체인 생성
	svcRegChain := table.addRegularChain(svcChain)
	fmt.Printf("\n엔드포인트 체인 생성:\n")
	var epChainNames []string
	for _, ep := range endpoints {
		epChainName := endpointChainName(svc, ep)
		epChainNames = append(epChainNames, epChainName)
		epChain := table.addRegularChain(epChainName)
		fmt.Printf("  %s -> %s\n", ep.String(), epChainName)

		// 엔드포인트 체인 규칙
		epChain.addRule(
			fmt.Sprintf("ip saddr %s", ep.IP),
			"mark set mark or 0x00004000",
			"masquerade hairpin",
		)
		epChain.addRule(
			fmt.Sprintf("meta l4proto %s", svc.Protocol),
			fmt.Sprintf("dnat to %s", ep.String()),
			"DNAT",
		)
	}

	// numgen random mod N vmap 규칙
	var vmapElements []string
	for i, name := range epChainNames {
		vmapElements = append(vmapElements, fmt.Sprintf("%d : goto %s", i, name))
	}
	vmapRule := fmt.Sprintf("numgen random mod %d vmap { %s }",
		len(endpoints), strings.Join(vmapElements, ", "))
	svcRegChain.addRule("", vmapRule, "load balancing")
	fmt.Printf("\n로드밸런싱 규칙:\n  %s\n", vmapRule)

	// 로드밸런싱 시뮬레이션
	fmt.Printf("\nnumgen random mod %d 로드밸런싱 시뮬레이션 (10,000 요청):\n", len(endpoints))
	counts := simulateLoadBalancing(endpoints, 10000)
	for _, ep := range endpoints {
		count := counts[ep.String()]
		pct := float64(count) / 100.0
		bar := strings.Repeat("#", int(pct/2))
		fmt.Printf("  %s (%s): %5d (%.1f%%) %s\n", ep.String(), ep.NodeName, count, pct, bar)
	}

	// =========================================================================
	// 데모 3: Topology Aware Routing -- Zone 기반
	// =========================================================================
	fmt.Println("\n" + strings.Repeat("-", 80))
	fmt.Println("[데모 3] Topology Aware Routing -- Zone Hint 기반")
	fmt.Println(strings.Repeat("-", 80))

	// 모든 EP에 zone hint 설정
	endpointsWithZoneHints := []*Endpoint{
		{IP: "10.244.1.10", Port: 8080, NodeName: "node-1", Zone: "us-east-1a",
			Ready: true, Serving: true, IsLocalNode: true,
			ZoneHints: map[string]bool{"us-east-1a": true}},
		{IP: "10.244.2.20", Port: 8080, NodeName: "node-2", Zone: "us-east-1a",
			Ready: true, Serving: true,
			ZoneHints: map[string]bool{"us-east-1a": true}},
		{IP: "10.244.3.30", Port: 8080, NodeName: "node-3", Zone: "us-east-1b",
			Ready: true, Serving: true,
			ZoneHints: map[string]bool{"us-east-1b": true}},
		{IP: "10.244.4.40", Port: 8080, NodeName: "node-4", Zone: "us-east-1b",
			Ready: true, Serving: true,
			ZoneHints: map[string]bool{"us-east-1b": true}},
	}

	fmt.Printf("\n현재 노드: node-1 (zone: us-east-1a)\n")
	fmt.Printf("전체 엔드포인트:\n")
	for _, ep := range endpointsWithZoneHints {
		zones := make([]string, 0)
		for z := range ep.ZoneHints {
			zones = append(zones, z)
		}
		fmt.Printf("  %s (node=%s, zone=%s, zoneHints=%v)\n",
			ep.String(), ep.NodeName, ep.Zone, zones)
	}

	svcZone := &ServicePort{
		Namespace: "default", Name: "web-zone", Protocol: "tcp",
		Port: 80, ClusterIP: "10.96.0.200",
	}

	fmt.Printf("\nCategorizeEndpoints 실행:\n")
	clusterEPs, localEPs, allReachable, hasAny := CategorizeEndpoints(
		endpointsWithZoneHints, svcZone, "node-1", "us-east-1a", false)

	fmt.Printf("\n  결과:\n")
	fmt.Printf("  hasAnyEndpoints: %v\n", hasAny)
	fmt.Printf("  clusterEndpoints (%d개):\n", len(clusterEPs))
	for _, ep := range clusterEPs {
		fmt.Printf("    - %s (node=%s, zone=%s)\n", ep.String(), ep.NodeName, ep.Zone)
	}
	if localEPs != nil {
		fmt.Printf("  localEndpoints (%d개):\n", len(localEPs))
		for _, ep := range localEPs {
			fmt.Printf("    - %s (node=%s)\n", ep.String(), ep.NodeName)
		}
	} else {
		fmt.Printf("  localEndpoints: nil (Local 정책 미사용)\n")
	}
	fmt.Printf("  allReachableEndpoints (%d개):\n", len(allReachable))
	for _, ep := range allReachable {
		fmt.Printf("    - %s (node=%s)\n", ep.String(), ep.NodeName)
	}

	// =========================================================================
	// 데모 4: Topology Aware Routing -- Node Hint 기반
	// =========================================================================
	fmt.Println("\n" + strings.Repeat("-", 80))
	fmt.Println("[데모 4] Topology Aware Routing -- Node Hint 기반 (PreferSameNode)")
	fmt.Println(strings.Repeat("-", 80))

	endpointsWithNodeHints := []*Endpoint{
		{IP: "10.244.1.10", Port: 8080, NodeName: "node-1", Zone: "us-east-1a",
			Ready: true, Serving: true, IsLocalNode: true,
			ZoneHints: map[string]bool{"us-east-1a": true},
			NodeHints: map[string]bool{"node-1": true}},
		{IP: "10.244.1.11", Port: 8080, NodeName: "node-1", Zone: "us-east-1a",
			Ready: true, Serving: true, IsLocalNode: true,
			ZoneHints: map[string]bool{"us-east-1a": true},
			NodeHints: map[string]bool{"node-1": true}},
		{IP: "10.244.2.20", Port: 8080, NodeName: "node-2", Zone: "us-east-1a",
			Ready: true, Serving: true,
			ZoneHints: map[string]bool{"us-east-1a": true},
			NodeHints: map[string]bool{"node-2": true}},
		{IP: "10.244.3.30", Port: 8080, NodeName: "node-3", Zone: "us-east-1b",
			Ready: true, Serving: true,
			ZoneHints: map[string]bool{"us-east-1b": true},
			NodeHints: map[string]bool{"node-3": true}},
	}

	fmt.Printf("\n현재 노드: node-1 (zone: us-east-1a)\n")
	fmt.Printf("PreferSameTrafficDistribution feature gate: 활성\n")
	fmt.Printf("전체 엔드포인트:\n")
	for _, ep := range endpointsWithNodeHints {
		nodes := make([]string, 0)
		for n := range ep.NodeHints {
			nodes = append(nodes, n)
		}
		fmt.Printf("  %s (node=%s, nodeHints=%v)\n", ep.String(), ep.NodeName, nodes)
	}

	svcNode := &ServicePort{
		Namespace: "default", Name: "web-node", Protocol: "tcp",
		Port: 80, ClusterIP: "10.96.0.201",
	}

	fmt.Printf("\nCategorizeEndpoints 실행 (PreferSameNode 활성):\n")
	clusterEPs, _, allReachable, hasAny = CategorizeEndpoints(
		endpointsWithNodeHints, svcNode, "node-1", "us-east-1a", true)

	fmt.Printf("\n  결과:\n")
	fmt.Printf("  hasAnyEndpoints: %v\n", hasAny)
	fmt.Printf("  clusterEndpoints (%d개) -- 같은 노드의 EP만 선택됨:\n", len(clusterEPs))
	for _, ep := range clusterEPs {
		fmt.Printf("    - %s (node=%s)\n", ep.String(), ep.NodeName)
	}

	// =========================================================================
	// 데모 5: Fallback 시나리오 -- 일부 EP에 hint 누락
	// =========================================================================
	fmt.Println("\n" + strings.Repeat("-", 80))
	fmt.Println("[데모 5] Fallback 시나리오 -- 일부 EP에 hint 누락")
	fmt.Println(strings.Repeat("-", 80))

	endpointsPartialHints := []*Endpoint{
		{IP: "10.244.1.10", Port: 8080, NodeName: "node-1", Zone: "us-east-1a",
			Ready: true, Serving: true, IsLocalNode: true,
			ZoneHints: map[string]bool{"us-east-1a": true}},
		{IP: "10.244.2.20", Port: 8080, NodeName: "node-2", Zone: "us-east-1a",
			Ready: true, Serving: true},
		// 이 EP에는 zone hint 없음!
		{IP: "10.244.3.30", Port: 8080, NodeName: "node-3", Zone: "us-east-1b",
			Ready: true, Serving: true,
			ZoneHints: map[string]bool{"us-east-1b": true}},
	}

	fmt.Printf("\n현재 노드: node-1 (zone: us-east-1a)\n")
	fmt.Printf("전체 엔드포인트:\n")
	for _, ep := range endpointsPartialHints {
		zones := make([]string, 0)
		for z := range ep.ZoneHints {
			zones = append(zones, z)
		}
		hintStr := "없음"
		if len(zones) > 0 {
			hintStr = strings.Join(zones, ",")
		}
		fmt.Printf("  %s (node=%s, zoneHints=%s)\n", ep.String(), ep.NodeName, hintStr)
	}

	fmt.Printf("\nCategorizeEndpoints 실행 (일부 hint 누락):\n")
	clusterEPs, _, allReachable, hasAny = CategorizeEndpoints(
		endpointsPartialHints, svcZone, "node-1", "us-east-1a", false)

	fmt.Printf("\n  결과 -- 토폴로지 무시, 모든 Ready EP 사용:\n")
	fmt.Printf("  hasAnyEndpoints: %v\n", hasAny)
	fmt.Printf("  clusterEndpoints (%d개):\n", len(clusterEPs))
	for _, ep := range clusterEPs {
		fmt.Printf("    - %s (node=%s, zone=%s)\n", ep.String(), ep.NodeName, ep.Zone)
	}

	// =========================================================================
	// 데모 6: Fallback -- Ready EP 없음 -> Serving+Terminating
	// =========================================================================
	fmt.Println("\n" + strings.Repeat("-", 80))
	fmt.Println("[데모 6] Fallback -- Ready EP 없음, Serving+Terminating 사용")
	fmt.Println(strings.Repeat("-", 80))

	endpointsTerminating := []*Endpoint{
		{IP: "10.244.1.10", Port: 8080, NodeName: "node-1", Zone: "us-east-1a",
			Ready: false, Serving: true, Terminating: true, IsLocalNode: true},
		{IP: "10.244.2.20", Port: 8080, NodeName: "node-2", Zone: "us-east-1a",
			Ready: false, Serving: true, Terminating: true},
		{IP: "10.244.3.30", Port: 8080, NodeName: "node-3", Zone: "us-east-1b",
			Ready: false, Serving: false, Terminating: true},
		// 이 EP는 Serving=false이므로 fallback 대상이 아님
	}

	fmt.Printf("\n전체 엔드포인트 (모두 Terminating 상태):\n")
	for _, ep := range endpointsTerminating {
		fmt.Printf("  %s (ready=%v, serving=%v, terminating=%v)\n",
			ep.String(), ep.Ready, ep.Serving, ep.Terminating)
	}

	fmt.Printf("\nCategorizeEndpoints 실행:\n")
	clusterEPs, _, _, hasAny = CategorizeEndpoints(
		endpointsTerminating, svcZone, "node-1", "us-east-1a", false)

	fmt.Printf("\n  결과:\n")
	fmt.Printf("  hasAnyEndpoints: %v\n", hasAny)
	fmt.Printf("  clusterEndpoints (%d개) -- Serving+Terminating만 선택:\n", len(clusterEPs))
	for _, ep := range clusterEPs {
		fmt.Printf("    - %s (serving=%v, terminating=%v)\n",
			ep.String(), ep.Serving, ep.Terminating)
	}

	// =========================================================================
	// 데모 7: nftElementStorage 증분 동기화
	// =========================================================================
	fmt.Println("\n" + strings.Repeat("-", 80))
	fmt.Println("[데모 7] nftElementStorage -- 증분 동기화 시뮬레이션")
	fmt.Println(strings.Repeat("-", 80))

	storage := newNFTElementStorage("service-ips")

	// 1차 동기화: 초기 서비스 3개 등록
	fmt.Println("\n[1차 동기화] 서비스 3개 초기 등록:")
	storage.resetLeftoverKeys()
	services := map[string]string{
		"10.96.0.1 . tcp . 443":  "goto service-HASH-kubernetes",
		"10.96.0.10 . tcp . 53":  "goto service-HASH-kube-dns-tcp",
		"10.96.0.10 . udp . 53":  "goto service-HASH-kube-dns-udp",
	}
	svcKeys := make([]string, 0, len(services))
	for k := range services {
		svcKeys = append(svcKeys, k)
	}
	sort.Strings(svcKeys)
	for _, k := range svcKeys {
		result := storage.ensureElem(k, services[k])
		fmt.Printf("  %s -> %s [%s]\n", k, services[k], result)
	}
	removed := storage.cleanupLeftoverKeys()
	fmt.Printf("  정리된 키: %v\n", removed)
	fmt.Printf("  현재 요소 수: %d\n", len(storage.elements))

	// 2차 동기화: 서비스 1개 추가, 1개 변경, 1개 삭제
	fmt.Println("\n[2차 동기화] 서비스 추가/변경/삭제:")
	storage.resetLeftoverKeys()
	services2 := map[string]string{
		"10.96.0.1 . tcp . 443":   "goto service-HASH-kubernetes",      // 변경 없음
		"10.96.0.10 . tcp . 53":   "goto service-HASH-kube-dns-tcp-v2", // 값 변경
		"10.96.0.100 . tcp . 80":  "goto service-HASH-web-new",         // 새로 추가
		// kube-dns-udp 누락 -> 삭제됨
	}
	svcKeys2 := make([]string, 0, len(services2))
	for k := range services2 {
		svcKeys2 = append(svcKeys2, k)
	}
	sort.Strings(svcKeys2)
	for _, k := range svcKeys2 {
		result := storage.ensureElem(k, services2[k])
		fmt.Printf("  %s -> %s [%s]\n", k, services2[k], result)
	}
	removed = storage.cleanupLeftoverKeys()
	fmt.Printf("  정리된 키: %v\n", removed)
	fmt.Printf("  현재 요소 수: %d\n", len(storage.elements))

	fmt.Printf("\n최종 Map 상태:\n")
	keys := make([]string, 0, len(storage.elements))
	for k := range storage.elements {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %s -> %s\n", k, storage.elements[k])
	}

	// =========================================================================
	// 데모 8: nftables 테이블 구조 출력 (간소화)
	// =========================================================================
	fmt.Println("\n" + strings.Repeat("-", 80))
	fmt.Println("[데모 8] 생성된 nftables 테이블 구조 (간소화)")
	fmt.Println(strings.Repeat("-", 80))
	fmt.Println()
	table.printTable()

	// =========================================================================
	// 데모 9: Local 트래픽 정책 + Topology 조합
	// =========================================================================
	fmt.Println("\n" + strings.Repeat("-", 80))
	fmt.Println("[데모 9] externalTrafficPolicy: Local + Topology Aware Routing 조합")
	fmt.Println(strings.Repeat("-", 80))

	svcLocal := &ServicePort{
		Namespace: "default", Name: "web-local", Protocol: "tcp",
		Port: 80, ClusterIP: "10.96.0.202",
		ExternalTrafficLocal: true,
	}

	endpointsLocal := []*Endpoint{
		{IP: "10.244.1.10", Port: 8080, NodeName: "node-1", Zone: "us-east-1a",
			Ready: true, Serving: true, IsLocalNode: true,
			ZoneHints: map[string]bool{"us-east-1a": true}},
		{IP: "10.244.2.20", Port: 8080, NodeName: "node-2", Zone: "us-east-1a",
			Ready: true, Serving: true,
			ZoneHints: map[string]bool{"us-east-1a": true}},
		{IP: "10.244.3.30", Port: 8080, NodeName: "node-3", Zone: "us-east-1b",
			Ready: true, Serving: true,
			ZoneHints: map[string]bool{"us-east-1b": true}},
	}

	fmt.Printf("\n현재 노드: node-1 (zone: us-east-1a)\n")
	fmt.Printf("서비스: externalTrafficPolicy=Local, UsesCluster=%v, UsesLocal=%v\n",
		svcLocal.UsesClusterEndpoints(), svcLocal.UsesLocalEndpoints())

	fmt.Printf("\nCategorizeEndpoints 실행:\n")
	clusterEPs, localEPs, allReachable, hasAny = CategorizeEndpoints(
		endpointsLocal, svcLocal, "node-1", "us-east-1a", false)

	fmt.Printf("\n  결과:\n")
	fmt.Printf("  hasAnyEndpoints: %v\n", hasAny)
	fmt.Printf("  clusterEndpoints (%d개) -- Zone 필터링 적용:\n", len(clusterEPs))
	for _, ep := range clusterEPs {
		fmt.Printf("    - %s (node=%s, zone=%s)\n", ep.String(), ep.NodeName, ep.Zone)
	}
	fmt.Printf("  localEndpoints (%d개) -- 로컬 노드만:\n", len(localEPs))
	for _, ep := range localEPs {
		fmt.Printf("    - %s (node=%s, local=%v)\n", ep.String(), ep.NodeName, ep.IsLocalNode)
	}
	fmt.Printf("  allReachableEndpoints (%d개) -- Cluster + Local 합집합:\n", len(allReachable))
	for _, ep := range allReachable {
		fmt.Printf("    - %s (node=%s)\n", ep.String(), ep.NodeName)
	}

	// =========================================================================
	// 요약
	// =========================================================================
	fmt.Println("\n" + strings.Repeat("=", 80))
	fmt.Println(" 시뮬레이션 요약")
	fmt.Println(strings.Repeat("=", 80))
	fmt.Println(`
  1. nftables 테이블 구조
     - 8개 Base Chain (filter 5 + nat 3)
     - 7개 Regular Chain + 동적 서비스/엔드포인트 체인
     - Map 기반 O(1) 서비스 디스패치

  2. DNAT 로드밸런싱
     - numgen random mod N vmap: 단일 규칙으로 N-way 균등 분배
     - iptables의 -m statistic --probability 체이닝 대비 규칙 수 N -> 1

  3. Topology Aware Routing
     - PreferSameNode > PreferSameZone > 없음 (우선순위)
     - "전부 또는 전무": 모든 Ready EP에 hint 필요
     - Zone hint 기반: 같은 zone EP만 선택
     - Node hint 기반: 같은 node EP만 선택 (feature gate 필요)

  4. CategorizeEndpoints 알고리즘
     - Cluster 정책: Ready + 토폴로지 필터링
     - Local 정책: 로컬 Ready EP
     - Fallback: Ready 없으면 Serving+Terminating 사용
     - allReachable = union(cluster, local)

  5. 증분 동기화 (nftElementStorage)
     - 현재 커널 상태 캐싱 -> 변경분만 트랜잭션에 포함
     - leftoverKeys로 삭제된 서비스 자동 감지/정리`)
	fmt.Println()
}
