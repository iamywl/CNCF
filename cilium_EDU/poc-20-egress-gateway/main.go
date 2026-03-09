// Egress Gateway 시뮬레이션
//
// 이 PoC는 Cilium Egress Gateway의 핵심 개념을 시뮬레이션한다:
// 1. CiliumEgressGatewayPolicy 파싱 및 내부 표현
// 2. 엔드포인트/노드 매칭 로직
// 3. 게이트웨이 노드 선택 및 설정 결정
// 4. 다중 게이트웨이 해시 기반 분배
// 5. eBPF 정책 맵 관리 (mark-and-sweep 패턴)
// 6. 이벤트 기반 리컨실레이션
//
// 실제 Cilium 구현을 참고:
// - pkg/egressgateway/manager.go (Manager)
// - pkg/egressgateway/policy.go (PolicyConfig, gatewayConfig)
// - pkg/egressgateway/endpoint.go (endpointMetadata)
//
// 실행: go run main.go

package main

import (
	"fmt"
	"hash/fnv"
	"net/netip"
	"sort"
	"strings"
	"sync"
)

// ─── 데이터 구조 ───

// EndpointMetadata는 Pod 엔드포인트 메타데이터 (Cilium endpointMetadata 참고)
type EndpointMetadata struct {
	ID     string            // UID
	Labels map[string]string // 레이블
	IPs    []netip.Addr      // Pod IP 목록
	NodeIP string            // 호스트 노드 IP
}

// Node는 Kubernetes 노드
type Node struct {
	Name   string
	IP     netip.Addr
	Labels map[string]string
}

// PolicyGatewayConfig는 CRD에서 파싱된 게이트웨이 설정 (Cilium policyGatewayConfig 참고)
type PolicyGatewayConfig struct {
	NodeSelector map[string]string // 게이트웨이 노드 셀렉터
	Interface    string            // 이그레스 인터페이스
	EgressIP     netip.Addr        // 이그레스 IP
}

// GatewayConfig는 런타임에 결정된 게이트웨이 설정 (Cilium gatewayConfig 참고)
type GatewayConfig struct {
	IfaceName                   string
	EgressIP                    netip.Addr
	GatewayIP                   netip.Addr
	LocalNodeConfiguredAsGateway bool
}

// PolicyConfig는 정책의 내부 표현 (Cilium PolicyConfig 참고)
type PolicyConfig struct {
	Name              string
	EndpointSelectors []map[string]string   // Pod 셀렉터 목록
	NodeSelectors     []map[string]string   // 노드 셀렉터 목록
	DstCIDRs          []netip.Prefix        // 대상 CIDR
	ExcludedCIDRs     []netip.Prefix        // 제외 CIDR
	PolicyGwConfigs   []PolicyGatewayConfig // 게이트웨이 설정
	GatewayConfigs    []GatewayConfig       // 런타임 게이트웨이 설정
	MatchedEndpoints  map[string]*EndpointMetadata // 매칭된 엔드포인트
}

// EgressPolicyKey는 eBPF 맵 키 (Cilium egressmap.EgressPolicyKey4 참고)
type EgressPolicyKey struct {
	SourceIP netip.Addr
	DestCIDR netip.Prefix
}

func (k EgressPolicyKey) String() string {
	return fmt.Sprintf("(%s → %s)", k.SourceIP, k.DestCIDR)
}

// EgressPolicyVal은 eBPF 맵 값 (Cilium egressmap.EgressPolicyVal4 참고)
type EgressPolicyVal struct {
	EgressIP  netip.Addr
	GatewayIP netip.Addr
}

func (v EgressPolicyVal) String() string {
	return fmt.Sprintf("egress=%s, gw=%s", v.EgressIP, v.GatewayIP)
}

// ─── 특수 IP 값 (Cilium manager.go 참고) ───

var (
	GatewayNotFoundIPv4 = netip.IPv4Unspecified()                 // 0.0.0.0
	ExcludedCIDRIPv4    = netip.MustParseAddr("0.0.0.1")          // 제외 CIDR 표시
	EgressIPNotFoundIPv4 = netip.IPv4Unspecified()                // 0.0.0.0
)

// ─── 레이블 매칭 함수 ───

func matchesLabels(selector map[string]string, labels map[string]string) bool {
	if len(selector) == 0 {
		return true // 빈 셀렉터는 모든 것과 매칭
	}
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// ─── PolicyConfig 메서드 ───

// matchesEndpointLabels는 엔드포인트가 정책과 매칭되는지 확인한다 (Cilium PolicyConfig.matchesEndpointLabels 참고)
func (pc *PolicyConfig) matchesEndpointLabels(ep *EndpointMetadata) bool {
	for _, selector := range pc.EndpointSelectors {
		if matchesLabels(selector, ep.Labels) {
			return true
		}
	}
	return false
}

// matchesNodeLabels는 노드 레이블이 정책과 매칭되는지 확인한다 (Cilium PolicyConfig.matchesNodeLabels 참고)
func (pc *PolicyConfig) matchesNodeLabels(nodeLabels map[string]string) bool {
	if len(pc.NodeSelectors) == 0 {
		return true
	}
	for _, selector := range pc.NodeSelectors {
		if matchesLabels(selector, nodeLabels) {
			return true
		}
	}
	return false
}

// updateMatchedEndpointIDs는 매칭된 엔드포인트를 업데이트한다 (Cilium PolicyConfig.updateMatchedEndpointIDs 참고)
func (pc *PolicyConfig) updateMatchedEndpointIDs(epStore map[string]*EndpointMetadata, nodeAddr2Labels map[string]map[string]string) {
	pc.MatchedEndpoints = make(map[string]*EndpointMetadata)
	for _, ep := range epStore {
		if pc.matchesEndpointLabels(ep) && pc.matchesNodeLabels(nodeAddr2Labels[ep.NodeIP]) {
			pc.MatchedEndpoints[ep.ID] = ep
		}
	}
}

// regenerateGatewayConfig는 런타임 게이트웨이 설정을 결정한다 (Cilium PolicyConfig.regenerateGatewayConfig 참고)
func (pc *PolicyConfig) regenerateGatewayConfig(nodes []Node, localNodeIP string) {
	pc.GatewayConfigs = make([]GatewayConfig, 0, len(pc.PolicyGwConfigs))

	for _, pgwc := range pc.PolicyGwConfigs {
		gwc := GatewayConfig{
			EgressIP:  EgressIPNotFoundIPv4,
			GatewayIP: GatewayNotFoundIPv4,
		}

		for _, node := range nodes {
			if !matchesLabels(pgwc.NodeSelector, node.Labels) {
				continue
			}
			gwc.GatewayIP = node.IP

			// 로컬 노드인 경우 egress IP 결정
			if node.IP.String() == localNodeIP {
				gwc.LocalNodeConfiguredAsGateway = true
				if pgwc.Interface != "" {
					gwc.IfaceName = pgwc.Interface
					gwc.EgressIP = node.IP // 시뮬레이션: 인터페이스 IP = 노드 IP
				} else if pgwc.EgressIP.IsValid() {
					gwc.EgressIP = pgwc.EgressIP
				} else {
					gwc.EgressIP = node.IP
					gwc.IfaceName = "eth0"
				}
			}
			break // 첫 번째 매칭 노드 사용
		}

		pc.GatewayConfigs = append(pc.GatewayConfigs, gwc)
	}
}

// computeEndpointHash는 엔드포인트 UID의 해시를 계산한다 (Cilium computeEndpointHash 참고)
func computeEndpointHash(endpointUID string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(endpointUID))
	return h.Sum32()
}

// forEachEndpointAndCIDR는 각 (엔드포인트, CIDR) 조합에 콜백을 호출한다 (Cilium PolicyConfig.forEachEndpointAndCIDR 참고)
func (pc *PolicyConfig) forEachEndpointAndCIDR(f func(netip.Addr, netip.Prefix, bool, *GatewayConfig)) {
	// 게이트웨이를 IP 기준 정렬 (일관된 할당 보장)
	sort.Slice(pc.GatewayConfigs, func(i, j int) bool {
		return pc.GatewayConfigs[i].GatewayIP.Less(pc.GatewayConfigs[j].GatewayIP)
	})

	for _, ep := range pc.MatchedEndpoints {
		var gw *GatewayConfig
		if len(pc.GatewayConfigs) > 1 {
			// 다중 게이트웨이: 해시 기반 분배
			idx := computeEndpointHash(ep.ID) % uint32(len(pc.GatewayConfigs))
			gw = &pc.GatewayConfigs[idx]
		} else if len(pc.GatewayConfigs) == 1 {
			gw = &pc.GatewayConfigs[0]
		} else {
			continue
		}

		for _, epIP := range ep.IPs {
			for _, dstCIDR := range pc.DstCIDRs {
				f(epIP, dstCIDR, false, gw)
			}
			for _, excludedCIDR := range pc.ExcludedCIDRs {
				f(epIP, excludedCIDR, true, gw)
			}
		}
	}
}

// ─── Egress Gateway Manager (Cilium Manager 참고) ───

type EgressGatewayManager struct {
	mu              sync.Mutex
	nodes           []Node
	nodeAddr2Labels map[string]map[string]string
	policyConfigs   map[string]*PolicyConfig
	epDataStore     map[string]*EndpointMetadata
	policyMap       map[EgressPolicyKey]EgressPolicyVal // eBPF 맵 시뮬레이션
	localNodeIP     string
}

func NewEgressGatewayManager(localNodeIP string) *EgressGatewayManager {
	return &EgressGatewayManager{
		nodes:           make([]Node, 0),
		nodeAddr2Labels: make(map[string]map[string]string),
		policyConfigs:   make(map[string]*PolicyConfig),
		epDataStore:     make(map[string]*EndpointMetadata),
		policyMap:       make(map[EgressPolicyKey]EgressPolicyVal),
		localNodeIP:     localNodeIP,
	}
}

func (m *EgressGatewayManager) AddNode(node Node) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 이름순 정렬 삽입 (Cilium은 slices.BinarySearchFunc 사용)
	idx := sort.Search(len(m.nodes), func(i int) bool {
		return m.nodes[i].Name >= node.Name
	})
	if idx < len(m.nodes) && m.nodes[idx].Name == node.Name {
		m.nodes[idx] = node // 업데이트
	} else {
		m.nodes = append(m.nodes, Node{})
		copy(m.nodes[idx+1:], m.nodes[idx:])
		m.nodes[idx] = node
	}
	m.nodeAddr2Labels[node.IP.String()] = node.Labels
}

func (m *EgressGatewayManager) AddEndpoint(ep *EndpointMetadata) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.epDataStore[ep.ID] = ep
}

func (m *EgressGatewayManager) AddPolicy(pc *PolicyConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.policyConfigs[pc.Name] = pc
}

// Reconcile은 리컨실레이션을 수행한다 (Cilium Manager.reconcileLocked 참고)
func (m *EgressGatewayManager) Reconcile() {
	m.mu.Lock()
	defer m.mu.Unlock()

	fmt.Println("  [리컨실레이션 시작]")

	// 1. 매칭된 엔드포인트 업데이트
	for _, pc := range m.policyConfigs {
		pc.updateMatchedEndpointIDs(m.epDataStore, m.nodeAddr2Labels)
	}

	// 2. 게이트웨이 설정 재생성
	for _, pc := range m.policyConfigs {
		pc.regenerateGatewayConfig(m.nodes, m.localNodeIP)
	}

	// 3. BPF 맵 업데이트 (mark-and-sweep)
	m.updateEgressRules()

	fmt.Println("  [리컨실레이션 완료]")
}

// updateEgressRules는 eBPF 정책 맵을 업데이트한다 (Cilium Manager.updateEgressRules4 참고)
func (m *EgressGatewayManager) updateEgressRules() {
	// 현재 모든 엔트리를 stale로 표시
	stale := make(map[EgressPolicyKey]bool)
	for k := range m.policyMap {
		stale[k] = true
	}

	addEgressRule := func(endpointIP netip.Addr, dstCIDR netip.Prefix, excludedCIDR bool, gwc *GatewayConfig) {
		key := EgressPolicyKey{SourceIP: endpointIP, DestCIDR: dstCIDR}
		delete(stale, key) // 아직 필요한 엔트리

		gatewayIP := gwc.GatewayIP
		if excludedCIDR {
			gatewayIP = ExcludedCIDRIPv4
		}

		newVal := EgressPolicyVal{EgressIP: gwc.EgressIP, GatewayIP: gatewayIP}
		if val, exists := m.policyMap[key]; exists && val == newVal {
			return // 변경 없음
		}

		m.policyMap[key] = newVal
		action := "적용"
		if excludedCIDR {
			action = "제외"
		}
		fmt.Printf("    BPF 맵 %s: %s → %s\n", action, key, newVal)
	}

	for _, pc := range m.policyConfigs {
		pc.forEachEndpointAndCIDR(addEgressRule)
	}

	// stale 엔트리 삭제
	for k := range stale {
		delete(m.policyMap, k)
		fmt.Printf("    BPF 맵 삭제: %s\n", k)
	}
}

func (m *EgressGatewayManager) PrintStatus() {
	m.mu.Lock()
	defer m.mu.Unlock()

	fmt.Println("  ┌─ 정책:")
	for name, pc := range m.policyConfigs {
		fmt.Printf("  │  %s (엔드포인트 %d개 매칭, 게이트웨이 %d개)\n",
			name, len(pc.MatchedEndpoints), len(pc.GatewayConfigs))
		for _, gwc := range pc.GatewayConfigs {
			local := ""
			if gwc.LocalNodeConfiguredAsGateway {
				local = " [LOCAL]"
			}
			fmt.Printf("  │    GW: %s (egress=%s, iface=%s)%s\n",
				gwc.GatewayIP, gwc.EgressIP, gwc.IfaceName, local)
		}
	}
	fmt.Println("  ├─ BPF 정책 맵:")
	if len(m.policyMap) == 0 {
		fmt.Println("  │  (비어있음)")
	}
	for k, v := range m.policyMap {
		excluded := ""
		if v.GatewayIP == ExcludedCIDRIPv4 {
			excluded = " [EXCLUDED]"
		}
		fmt.Printf("  │  %s → %s%s\n", k, v, excluded)
	}
	fmt.Println("  └─")
}

// ─── 메인 ───

func main() {
	fmt.Println("╔════════════════════════════════════════════════════════╗")
	fmt.Println("║     Cilium Egress Gateway 시뮬레이션                    ║")
	fmt.Println("╚════════════════════════════════════════════════════════╝")

	// 로컬 노드 IP 설정 (시뮬레이션)
	localNodeIP := "10.0.0.1"
	manager := NewEgressGatewayManager(localNodeIP)

	// ─── 시나리오 1: 노드 및 엔드포인트 등록 ───
	fmt.Println("\n━━━ 시나리오 1: 노드 및 엔드포인트 등록 ━━━")

	// 노드 추가
	manager.AddNode(Node{
		Name: "worker-1",
		IP:   netip.MustParseAddr("10.0.0.1"),
		Labels: map[string]string{
			"kubernetes.io/hostname": "worker-1",
			"egress-gateway":        "true",
		},
	})
	manager.AddNode(Node{
		Name: "worker-2",
		IP:   netip.MustParseAddr("10.0.0.2"),
		Labels: map[string]string{
			"kubernetes.io/hostname": "worker-2",
		},
	})
	manager.AddNode(Node{
		Name: "worker-3",
		IP:   netip.MustParseAddr("10.0.0.3"),
		Labels: map[string]string{
			"kubernetes.io/hostname": "worker-3",
			"egress-gateway":        "true",
		},
	})

	// 엔드포인트 추가 (Pod)
	manager.AddEndpoint(&EndpointMetadata{
		ID:     "uid-pod-1",
		Labels: map[string]string{"app": "backend", "env": "production"},
		IPs:    []netip.Addr{netip.MustParseAddr("10.244.1.10")},
		NodeIP: "10.0.0.1",
	})
	manager.AddEndpoint(&EndpointMetadata{
		ID:     "uid-pod-2",
		Labels: map[string]string{"app": "backend", "env": "production"},
		IPs:    []netip.Addr{netip.MustParseAddr("10.244.2.20")},
		NodeIP: "10.0.0.2",
	})
	manager.AddEndpoint(&EndpointMetadata{
		ID:     "uid-pod-3",
		Labels: map[string]string{"app": "frontend", "env": "production"},
		IPs:    []netip.Addr{netip.MustParseAddr("10.244.1.30")},
		NodeIP: "10.0.0.1",
	})

	fmt.Println("  노드 3개, 엔드포인트 3개 등록됨")

	// ─── 시나리오 2: 단일 게이트웨이 정책 ───
	fmt.Println("\n━━━ 시나리오 2: 단일 게이트웨이 정책 적용 ━━━")

	manager.AddPolicy(&PolicyConfig{
		Name: "egw-production",
		EndpointSelectors: []map[string]string{
			{"app": "backend", "env": "production"},
		},
		DstCIDRs: []netip.Prefix{
			netip.MustParsePrefix("0.0.0.0/0"),
		},
		ExcludedCIDRs: []netip.Prefix{
			netip.MustParsePrefix("10.0.0.0/8"),
		},
		PolicyGwConfigs: []PolicyGatewayConfig{
			{
				NodeSelector: map[string]string{"egress-gateway": "true"},
				Interface:    "eth0",
			},
		},
	})

	manager.Reconcile()
	fmt.Println("\n  현재 상태:")
	manager.PrintStatus()

	// ─── 시나리오 3: 다중 게이트웨이 정책 ───
	fmt.Println("\n━━━ 시나리오 3: 다중 게이트웨이 정책 (해시 기반 분배) ━━━")

	// 기존 정책 제거
	delete(manager.policyConfigs, "egw-production")

	manager.AddPolicy(&PolicyConfig{
		Name: "egw-multi-gw",
		EndpointSelectors: []map[string]string{
			{"app": "backend"},
		},
		DstCIDRs: []netip.Prefix{
			netip.MustParsePrefix("203.0.113.0/24"),
		},
		PolicyGwConfigs: []PolicyGatewayConfig{
			{
				NodeSelector: map[string]string{"kubernetes.io/hostname": "worker-1"},
				EgressIP:     netip.MustParseAddr("198.51.100.10"),
			},
			{
				NodeSelector: map[string]string{"kubernetes.io/hostname": "worker-3"},
				EgressIP:     netip.MustParseAddr("198.51.100.20"),
			},
		},
	})

	manager.Reconcile()

	fmt.Println("\n  현재 상태:")
	manager.PrintStatus()

	// 해시 분배 확인
	fmt.Println("\n  해시 기반 분배 확인:")
	for _, ep := range []string{"uid-pod-1", "uid-pod-2"} {
		hash := computeEndpointHash(ep)
		idx := hash % 2
		fmt.Printf("    %s: hash=%d, 게이트웨이 인덱스=%d\n", ep, hash, idx)
	}

	// ─── 시나리오 4: 엔드포인트 추가/삭제에 따른 동적 업데이트 ───
	fmt.Println("\n━━━ 시나리오 4: 새 엔드포인트 추가 ━━━")

	manager.AddEndpoint(&EndpointMetadata{
		ID:     "uid-pod-4",
		Labels: map[string]string{"app": "backend", "env": "staging"},
		IPs:    []netip.Addr{netip.MustParseAddr("10.244.3.40")},
		NodeIP: "10.0.0.3",
	})

	manager.Reconcile()
	fmt.Println("\n  현재 상태:")
	manager.PrintStatus()

	// ─── 시나리오 5: 특수 IP 값 확인 ───
	fmt.Println("\n━━━ 시나리오 5: 특수 IP 값 의미 ━━━")
	fmt.Println("  GatewayNotFoundIPv4:", GatewayNotFoundIPv4, "→ 게이트웨이 미발견")
	fmt.Println("  ExcludedCIDRIPv4:   ", ExcludedCIDRIPv4, "  → 제외된 CIDR")
	fmt.Println("  EgressIPNotFoundIPv4:", EgressIPNotFoundIPv4, "→ Egress IP 미발견")

	// ─── 정리 ───
	fmt.Println()
	fmt.Println(strings.Repeat("─", 56))
	fmt.Printf("최종 BPF 맵 엔트리 수: %d\n", len(manager.policyMap))

	fmt.Println("\n╔════════════════════════════════════════════════════════╗")
	fmt.Println("║     시뮬레이션 완료                                     ║")
	fmt.Println("╚════════════════════════════════════════════════════════╝")
}
