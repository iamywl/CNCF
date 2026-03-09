// NetworkPolicy 시뮬레이터
//
// Kubernetes NetworkPolicy의 핵심 개념을 Go 표준 라이브러리만으로 시뮬레이션한다.
// 구현 내용:
//   1. NetworkPolicy 타입 시스템 (Ingress/Egress 규칙)
//   2. Pod/Namespace 셀렉터 매칭
//   3. IPBlock CIDR 매칭 (Except 포함)
//   4. 포트 범위 매칭 (Port ~ EndPort)
//   5. 정책 합산(Additive) 규칙
//   6. 트래픽 판정 엔진
//
// 실행: go run main.go
package main

import (
	"fmt"
	"net"
	"strings"
)

// ============================================================================
// 1. 타입 정의 -- Kubernetes NetworkPolicy 타입 시스템 재현
// ============================================================================

// Protocol은 네트워크 프로토콜을 나타낸다.
type Protocol string

const (
	ProtocolTCP  Protocol = "TCP"
	ProtocolUDP  Protocol = "UDP"
	ProtocolSCTP Protocol = "SCTP"
)

// PolicyType은 정책이 제어하는 트래픽 방향이다.
type PolicyType string

const (
	PolicyTypeIngress PolicyType = "Ingress"
	PolicyTypeEgress  PolicyType = "Egress"
)

// Labels는 Kubernetes 라벨을 표현한다.
type Labels map[string]string

// LabelSelector는 라벨 기반 셀렉터이다.
// nil이면 "미지정", 빈 맵이면 "모든 대상 선택".
type LabelSelector struct {
	MatchLabels Labels
	IsPresent   bool // true이면 "빈 셀렉터 {}"를 명시적으로 지정한 것
}

// NetworkPolicyPort는 허용할 포트를 정의한다.
type NetworkPolicyPort struct {
	Protocol *Protocol
	Port     *int
	EndPort  *int
}

// IPBlock은 CIDR 기반 IP 대역을 정의한다.
type IPBlock struct {
	CIDR   string
	Except []string
}

// NetworkPolicyPeer는 트래픽의 소스/대상을 정의한다.
type NetworkPolicyPeer struct {
	PodSelector       *LabelSelector
	NamespaceSelector *LabelSelector
	IPBlock           *IPBlock
}

// NetworkPolicyIngressRule은 인바운드 트래픽 규칙이다.
type NetworkPolicyIngressRule struct {
	Ports []NetworkPolicyPort
	From  []NetworkPolicyPeer
}

// NetworkPolicyEgressRule은 아웃바운드 트래픽 규칙이다.
type NetworkPolicyEgressRule struct {
	Ports []NetworkPolicyPort
	To    []NetworkPolicyPeer
}

// NetworkPolicySpec은 정책 명세이다.
type NetworkPolicySpec struct {
	PodSelector LabelSelector
	Ingress     []NetworkPolicyIngressRule
	Egress      []NetworkPolicyEgressRule
	PolicyTypes []PolicyType
}

// NetworkPolicy는 Kubernetes NetworkPolicy 리소스를 재현한다.
type NetworkPolicy struct {
	Name      string
	Namespace string
	Spec      NetworkPolicySpec
}

// ============================================================================
// 2. 클러스터 상태 모델
// ============================================================================

// Pod는 클러스터 내의 Pod를 나타낸다.
type Pod struct {
	Name      string
	Namespace string
	Labels    Labels
	IP        string
}

// Namespace는 클러스터 내의 네임스페이스를 나타낸다.
type Namespace struct {
	Name   string
	Labels Labels
}

// TrafficDirection은 트래픽 방향이다.
type TrafficDirection string

const (
	DirectionIngress TrafficDirection = "Ingress"
	DirectionEgress  TrafficDirection = "Egress"
)

// Traffic은 네트워크 트래픽을 나타낸다.
type Traffic struct {
	SourcePod      *Pod
	SourceIP       string
	DestPod        *Pod
	DestIP         string
	DestPort       int
	Protocol       Protocol
	Direction      TrafficDirection
	SourceExternal bool // 클러스터 외부 소스 여부
	DestExternal   bool // 클러스터 외부 대상 여부
}

// Cluster는 시뮬레이션 클러스터 상태를 보관한다.
type Cluster struct {
	Namespaces []Namespace
	Pods       []Pod
	Policies   []NetworkPolicy
}

// ============================================================================
// 3. 라벨 셀렉터 매칭
// ============================================================================

// matchLabels는 라벨이 셀렉터에 매칭되는지 확인한다.
// 빈 셀렉터({})는 모든 라벨에 매칭된다.
func matchLabels(selector *LabelSelector, labels Labels) bool {
	if selector == nil {
		return false // nil 셀렉터 = "미지정" = 매칭하지 않음
	}
	if !selector.IsPresent && len(selector.MatchLabels) == 0 {
		return false // 명시적 빈 셀렉터가 아님
	}
	// 빈 셀렉터 {} = 모든 대상 선택
	if selector.IsPresent && len(selector.MatchLabels) == 0 {
		return true
	}
	// 셀렉터의 모든 라벨이 대상에 존재해야 함 (AND)
	for k, v := range selector.MatchLabels {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// ============================================================================
// 4. CIDR 매칭 (IPBlock)
// ============================================================================

// matchCIDR은 IP가 CIDR 범위에 포함되는지 확인한다.
func matchCIDR(ip string, cidr string) bool {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return false
	}
	parsedIP := net.ParseIP(ip)
	if parsedIP == nil {
		return false
	}
	return network.Contains(parsedIP)
}

// matchIPBlock은 IP가 IPBlock에 매칭되는지 확인한다.
// CIDR에 포함되면서 Except에는 포함되지 않아야 한다.
func matchIPBlock(ip string, block *IPBlock) bool {
	if block == nil {
		return false
	}
	// CIDR 매칭
	if !matchCIDR(ip, block.CIDR) {
		return false
	}
	// Except 확인 -- 제외 대역에 포함되면 매칭 실패
	for _, except := range block.Except {
		if matchCIDR(ip, except) {
			return false
		}
	}
	return true
}

// ============================================================================
// 5. 포트 매칭
// ============================================================================

// matchPort는 트래픽 포트가 NetworkPolicyPort에 매칭되는지 확인한다.
func matchPort(trafficPort int, trafficProto Protocol, policyPort NetworkPolicyPort) bool {
	// 프로토콜 확인 (nil이면 TCP 기본값)
	proto := ProtocolTCP
	if policyPort.Protocol != nil {
		proto = *policyPort.Protocol
	}
	if proto != trafficProto {
		return false
	}
	// Port 미지정이면 모든 포트 매칭
	if policyPort.Port == nil {
		return true
	}
	// EndPort가 있으면 범위 확인
	if policyPort.EndPort != nil {
		return trafficPort >= *policyPort.Port && trafficPort <= *policyPort.EndPort
	}
	// 단일 포트 매칭
	return trafficPort == *policyPort.Port
}

// matchPorts는 트래픽이 포트 목록 중 하나라도 매칭되는지 확인한다 (OR).
// 빈 목록이면 모든 포트 허용.
func matchPorts(trafficPort int, trafficProto Protocol, ports []NetworkPolicyPort) bool {
	if len(ports) == 0 {
		return true // 빈 목록 = 모든 포트 허용
	}
	for _, p := range ports {
		if matchPort(trafficPort, trafficProto, p) {
			return true
		}
	}
	return false
}

// ============================================================================
// 6. Peer 매칭
// ============================================================================

// matchPeer는 트래픽 소스/대상이 NetworkPolicyPeer에 매칭되는지 확인한다.
func matchPeer(peer NetworkPolicyPeer, pod *Pod, ip string, isExternal bool,
	policyNamespace string, cluster *Cluster) bool {

	// IPBlock 매칭
	if peer.IPBlock != nil {
		return matchIPBlock(ip, peer.IPBlock)
	}

	// 클러스터 외부 트래픽은 PodSelector/NamespaceSelector로 매칭 불가
	if isExternal || pod == nil {
		return false
	}

	// NamespaceSelector + PodSelector 조합
	if peer.PodSelector != nil && peer.NamespaceSelector != nil {
		// AND 관계: 네임스페이스 매칭 AND Pod 매칭
		nsMatched := false
		for _, ns := range cluster.Namespaces {
			if ns.Name == pod.Namespace && matchLabels(peer.NamespaceSelector, ns.Labels) {
				nsMatched = true
				break
			}
		}
		return nsMatched && matchLabels(peer.PodSelector, pod.Labels)
	}

	// NamespaceSelector만
	if peer.NamespaceSelector != nil {
		for _, ns := range cluster.Namespaces {
			if ns.Name == pod.Namespace && matchLabels(peer.NamespaceSelector, ns.Labels) {
				return true
			}
		}
		return false
	}

	// PodSelector만 -- 같은 네임스페이스 내에서만 매칭
	if peer.PodSelector != nil {
		if pod.Namespace != policyNamespace {
			return false
		}
		return matchLabels(peer.PodSelector, pod.Labels)
	}

	return false
}

// matchPeers는 트래픽이 peer 목록 중 하나라도 매칭되는지 확인한다 (OR).
// 빈 목록이면 모든 소스/대상 허용.
func matchPeers(peers []NetworkPolicyPeer, pod *Pod, ip string, isExternal bool,
	policyNamespace string, cluster *Cluster) bool {
	if len(peers) == 0 {
		return true // 빈 목록 = 모든 소스/대상 허용
	}
	for _, p := range peers {
		if matchPeer(p, pod, ip, isExternal, policyNamespace, cluster) {
			return true
		}
	}
	return false
}

// ============================================================================
// 7. 트래픽 판정 엔진
// ============================================================================

// hasPolicyType은 PolicyTypes에 특정 타입이 포함되는지 확인한다.
func hasPolicyType(types []PolicyType, target PolicyType) bool {
	for _, t := range types {
		if t == target {
			return true
		}
	}
	return false
}

// effectivePolicyTypes는 PolicyTypes의 기본값을 추론한다.
// 미지정 시: Ingress는 항상 포함, Egress 규칙이 있으면 Egress도 포함.
func effectivePolicyTypes(policy *NetworkPolicy) []PolicyType {
	if len(policy.Spec.PolicyTypes) > 0 {
		return policy.Spec.PolicyTypes
	}
	types := []PolicyType{PolicyTypeIngress}
	if len(policy.Spec.Egress) > 0 {
		types = append(types, PolicyTypeEgress)
	}
	return types
}

// EvaluateTraffic은 트래픽이 허용되는지 판정한다.
// 반환값: (허용 여부, 판정 이유)
func EvaluateTraffic(traffic Traffic, cluster *Cluster) (bool, string) {
	var targetPod *Pod
	var direction TrafficDirection

	// 트래픽 방향에 따라 대상 Pod 결정
	if traffic.Direction == DirectionIngress {
		targetPod = traffic.DestPod
		direction = DirectionIngress
	} else {
		targetPod = traffic.SourcePod
		direction = DirectionEgress
	}

	if targetPod == nil {
		return true, "대상 Pod 없음 -- 정책 평가 불가, 기본 허용"
	}

	// 1단계: 이 Pod에 적용되는 정책이 있는지 확인
	var applicablePolicies []NetworkPolicy
	for _, policy := range cluster.Policies {
		if policy.Namespace != targetPod.Namespace {
			continue
		}
		if matchLabels(&policy.Spec.PodSelector, targetPod.Labels) {
			// PolicyTypes에 해당 방향이 포함되는지 확인
			effectiveTypes := effectivePolicyTypes(&policy)
			if direction == DirectionIngress && hasPolicyType(effectiveTypes, PolicyTypeIngress) {
				applicablePolicies = append(applicablePolicies, policy)
			} else if direction == DirectionEgress && hasPolicyType(effectiveTypes, PolicyTypeEgress) {
				applicablePolicies = append(applicablePolicies, policy)
			}
		}
	}

	// 적용되는 정책이 없으면 기본 허용
	if len(applicablePolicies) == 0 {
		return true, "적용되는 NetworkPolicy 없음 -- 기본 허용"
	}

	// 2단계: 정책의 규칙 중 하나라도 매칭되면 허용 (Additive)
	var matchedPolicies []string
	for _, policy := range applicablePolicies {
		if direction == DirectionIngress {
			for _, rule := range policy.Spec.Ingress {
				peerMatched := matchPeers(rule.From, traffic.SourcePod, traffic.SourceIP,
					traffic.SourceExternal, policy.Namespace, cluster)
				portMatched := matchPorts(traffic.DestPort, traffic.Protocol, rule.Ports)
				if peerMatched && portMatched {
					matchedPolicies = append(matchedPolicies, policy.Name)
					break
				}
			}
		} else {
			for _, rule := range policy.Spec.Egress {
				peerMatched := matchPeers(rule.To, traffic.DestPod, traffic.DestIP,
					traffic.DestExternal, policy.Namespace, cluster)
				portMatched := matchPorts(traffic.DestPort, traffic.Protocol, rule.Ports)
				if peerMatched && portMatched {
					matchedPolicies = append(matchedPolicies, policy.Name)
					break
				}
			}
		}
	}

	if len(matchedPolicies) > 0 {
		return true, fmt.Sprintf("허용 -- 매칭된 정책: [%s]", strings.Join(matchedPolicies, ", "))
	}

	// 3단계: 어떤 규칙도 매칭되지 않으면 차단
	policyNames := make([]string, len(applicablePolicies))
	for i, p := range applicablePolicies {
		policyNames[i] = p.Name
	}
	return false, fmt.Sprintf("차단 -- 적용된 정책 [%s]에서 허용 규칙 없음", strings.Join(policyNames, ", "))
}

// ============================================================================
// 8. 검증 로직
// ============================================================================

// ValidateNetworkPolicyPort는 NetworkPolicyPort의 유효성을 검증한다.
func ValidateNetworkPolicyPort(port NetworkPolicyPort) []string {
	var errors []string

	// Protocol 검증
	if port.Protocol != nil {
		switch *port.Protocol {
		case ProtocolTCP, ProtocolUDP, ProtocolSCTP:
			// 유효
		default:
			errors = append(errors, fmt.Sprintf("지원하지 않는 프로토콜: %s (TCP, UDP, SCTP만 허용)", *port.Protocol))
		}
	}

	// Port 검증
	if port.Port != nil {
		if *port.Port < 1 || *port.Port > 65535 {
			errors = append(errors, fmt.Sprintf("유효하지 않은 포트 번호: %d (1-65535 범위)", *port.Port))
		}
		// EndPort 검증
		if port.EndPort != nil {
			if *port.EndPort < *port.Port {
				errors = append(errors, fmt.Sprintf("endPort(%d)는 port(%d) 이상이어야 함", *port.EndPort, *port.Port))
			}
			if *port.EndPort < 1 || *port.EndPort > 65535 {
				errors = append(errors, fmt.Sprintf("유효하지 않은 endPort: %d (1-65535 범위)", *port.EndPort))
			}
		}
	} else {
		// Port 미지정 시 EndPort도 사용 불가
		if port.EndPort != nil {
			errors = append(errors, "port가 지정되지 않으면 endPort를 사용할 수 없음")
		}
	}

	return errors
}

// ValidateNetworkPolicyPeer는 NetworkPolicyPeer의 유효성을 검증한다.
func ValidateNetworkPolicyPeer(peer NetworkPolicyPeer) []string {
	var errors []string
	numPeers := 0

	if peer.PodSelector != nil {
		numPeers++
	}
	if peer.NamespaceSelector != nil {
		numPeers++
	}
	if peer.IPBlock != nil {
		numPeers++
	}

	if numPeers == 0 {
		errors = append(errors, "최소 하나의 peer를 지정해야 함")
	} else if numPeers > 1 && peer.IPBlock != nil {
		errors = append(errors, "ipBlock은 podSelector/namespaceSelector와 함께 사용할 수 없음")
	}

	// IPBlock 검증
	if peer.IPBlock != nil {
		if peer.IPBlock.CIDR == "" {
			errors = append(errors, "ipBlock.cidr은 필수 필드")
		} else {
			_, cidrNet, err := net.ParseCIDR(peer.IPBlock.CIDR)
			if err != nil {
				errors = append(errors, fmt.Sprintf("유효하지 않은 CIDR: %s", peer.IPBlock.CIDR))
			} else {
				for _, except := range peer.IPBlock.Except {
					_, exceptNet, err := net.ParseCIDR(except)
					if err != nil {
						errors = append(errors, fmt.Sprintf("유효하지 않은 except CIDR: %s", except))
						continue
					}
					cidrMaskLen, _ := cidrNet.Mask.Size()
					exceptMaskLen, _ := exceptNet.Mask.Size()
					if !cidrNet.Contains(exceptNet.IP) || cidrMaskLen >= exceptMaskLen {
						errors = append(errors, fmt.Sprintf("except %s는 cidr %s의 엄격한 부분집합이어야 함",
							except, peer.IPBlock.CIDR))
					}
				}
			}
		}
	}

	return errors
}

// ============================================================================
// 9. 헬퍼 함수
// ============================================================================

func intPtr(v int) *int             { return &v }
func protoPtr(p Protocol) *Protocol { return &p }

func allSelector() *LabelSelector {
	return &LabelSelector{MatchLabels: Labels{}, IsPresent: true}
}

func labelSelector(labels Labels) *LabelSelector {
	return &LabelSelector{MatchLabels: labels, IsPresent: true}
}

func printResult(name string, allowed bool, reason string) {
	status := "ALLOW"
	if !allowed {
		status = "DENY "
	}
	fmt.Printf("  [%s] %-45s -- %s\n", status, name, reason)
}

func formatErrors(errors []string) string {
	if len(errors) == 0 {
		return "OK (유효)"
	}
	return "ERROR -- " + strings.Join(errors, "; ")
}

// ============================================================================
// 10. 시뮬레이션
// ============================================================================

func main() {
	fmt.Println("=============================================================")
	fmt.Println(" Kubernetes NetworkPolicy 시뮬레이터")
	fmt.Println("=============================================================")
	fmt.Println()

	// ------------------------------------------------------------------
	// 클러스터 구성
	// ------------------------------------------------------------------
	cluster := &Cluster{
		Namespaces: []Namespace{
			{Name: "production", Labels: Labels{"env": "production"}},
			{Name: "monitoring", Labels: Labels{"env": "monitoring", "name": "monitoring"}},
			{Name: "development", Labels: Labels{"env": "development"}},
		},
		Pods: []Pod{
			{Name: "frontend-1", Namespace: "production", Labels: Labels{"app": "frontend", "tier": "web"}, IP: "10.0.1.10"},
			{Name: "backend-1", Namespace: "production", Labels: Labels{"app": "backend", "tier": "api"}, IP: "10.0.1.20"},
			{Name: "database-1", Namespace: "production", Labels: Labels{"app": "database", "tier": "db"}, IP: "10.0.1.30"},
			{Name: "prometheus", Namespace: "monitoring", Labels: Labels{"app": "prometheus"}, IP: "10.0.2.10"},
			{Name: "dev-app", Namespace: "development", Labels: Labels{"app": "dev-app"}, IP: "10.0.3.10"},
		},
	}

	frontend := &cluster.Pods[0]
	backend := &cluster.Pods[1]
	database := &cluster.Pods[2]
	prometheus := &cluster.Pods[3]
	devApp := &cluster.Pods[4]

	tcp := ProtocolTCP

	// ------------------------------------------------------------------
	// 시나리오 1: 정책 없음 -- 모든 트래픽 허용
	// ------------------------------------------------------------------
	fmt.Println("--- 시나리오 1: 정책 없음 (기본 허용) ---")
	fmt.Println()

	allowed, reason := EvaluateTraffic(Traffic{
		SourcePod: frontend, SourceIP: frontend.IP,
		DestPod: backend, DestIP: backend.IP,
		DestPort: 80, Protocol: tcp, Direction: DirectionIngress,
	}, cluster)
	printResult("frontend -> backend:80", allowed, reason)

	allowed, reason = EvaluateTraffic(Traffic{
		SourcePod: devApp, SourceIP: devApp.IP,
		DestPod: database, DestIP: database.IP,
		DestPort: 5432, Protocol: tcp, Direction: DirectionIngress,
	}, cluster)
	printResult("dev-app -> database:5432", allowed, reason)
	fmt.Println()

	// ------------------------------------------------------------------
	// 시나리오 2: Default Deny Ingress
	// ------------------------------------------------------------------
	fmt.Println("--- 시나리오 2: Default Deny Ingress ---")
	fmt.Println()

	cluster.Policies = []NetworkPolicy{
		{
			Name:      "default-deny-ingress",
			Namespace: "production",
			Spec: NetworkPolicySpec{
				PodSelector: LabelSelector{MatchLabels: Labels{}, IsPresent: true}, // 모든 Pod
				PolicyTypes: []PolicyType{PolicyTypeIngress},
				// Ingress 규칙 없음 = 모든 인바운드 차단
			},
		},
	}

	allowed, reason = EvaluateTraffic(Traffic{
		SourcePod: frontend, SourceIP: frontend.IP,
		DestPod: backend, DestIP: backend.IP,
		DestPort: 80, Protocol: tcp, Direction: DirectionIngress,
	}, cluster)
	printResult("frontend -> backend:80", allowed, reason)

	allowed, reason = EvaluateTraffic(Traffic{
		SourcePod: devApp, SourceIP: devApp.IP,
		DestPod: database, DestIP: database.IP,
		DestPort: 5432, Protocol: tcp, Direction: DirectionIngress,
	}, cluster)
	printResult("dev-app -> database:5432", allowed, reason)
	fmt.Println()

	// ------------------------------------------------------------------
	// 시나리오 3: 특정 Pod 간 트래픽 허용 (Additive)
	// ------------------------------------------------------------------
	fmt.Println("--- 시나리오 3: Additive 정책 (frontend -> backend 허용 추가) ---")
	fmt.Println()

	cluster.Policies = append(cluster.Policies, NetworkPolicy{
		Name:      "allow-frontend-to-backend",
		Namespace: "production",
		Spec: NetworkPolicySpec{
			PodSelector: *labelSelector(Labels{"app": "backend"}),
			PolicyTypes: []PolicyType{PolicyTypeIngress},
			Ingress: []NetworkPolicyIngressRule{
				{
					From: []NetworkPolicyPeer{
						{PodSelector: labelSelector(Labels{"app": "frontend"})},
					},
					Ports: []NetworkPolicyPort{
						{Protocol: &tcp, Port: intPtr(80)},
						{Protocol: &tcp, Port: intPtr(443)},
					},
				},
			},
		},
	})

	allowed, reason = EvaluateTraffic(Traffic{
		SourcePod: frontend, SourceIP: frontend.IP,
		DestPod: backend, DestIP: backend.IP,
		DestPort: 80, Protocol: tcp, Direction: DirectionIngress,
	}, cluster)
	printResult("frontend -> backend:80", allowed, reason)

	allowed, reason = EvaluateTraffic(Traffic{
		SourcePod: frontend, SourceIP: frontend.IP,
		DestPod: backend, DestIP: backend.IP,
		DestPort: 443, Protocol: tcp, Direction: DirectionIngress,
	}, cluster)
	printResult("frontend -> backend:443", allowed, reason)

	allowed, reason = EvaluateTraffic(Traffic{
		SourcePod: devApp, SourceIP: devApp.IP,
		DestPod: backend, DestIP: backend.IP,
		DestPort: 80, Protocol: tcp, Direction: DirectionIngress,
	}, cluster)
	printResult("dev-app -> backend:80", allowed, reason)

	allowed, reason = EvaluateTraffic(Traffic{
		SourcePod: frontend, SourceIP: frontend.IP,
		DestPod: backend, DestIP: backend.IP,
		DestPort: 8080, Protocol: tcp, Direction: DirectionIngress,
	}, cluster)
	printResult("frontend -> backend:8080", allowed, reason)
	fmt.Println()

	// ------------------------------------------------------------------
	// 시나리오 4: NamespaceSelector -- 다른 네임스페이스에서의 접근
	// ------------------------------------------------------------------
	fmt.Println("--- 시나리오 4: NamespaceSelector (monitoring NS의 prometheus 허용) ---")
	fmt.Println()

	cluster.Policies = append(cluster.Policies, NetworkPolicy{
		Name:      "allow-monitoring",
		Namespace: "production",
		Spec: NetworkPolicySpec{
			PodSelector: *allSelector(), // 모든 Pod
			PolicyTypes: []PolicyType{PolicyTypeIngress},
			Ingress: []NetworkPolicyIngressRule{
				{
					From: []NetworkPolicyPeer{
						{
							NamespaceSelector: labelSelector(Labels{"name": "monitoring"}),
							PodSelector:       labelSelector(Labels{"app": "prometheus"}),
						},
					},
					Ports: []NetworkPolicyPort{
						{Protocol: &tcp, Port: intPtr(9090)},
					},
				},
			},
		},
	})

	allowed, reason = EvaluateTraffic(Traffic{
		SourcePod: prometheus, SourceIP: prometheus.IP,
		DestPod: backend, DestIP: backend.IP,
		DestPort: 9090, Protocol: tcp, Direction: DirectionIngress,
	}, cluster)
	printResult("prometheus(monitoring) -> backend:9090", allowed, reason)

	allowed, reason = EvaluateTraffic(Traffic{
		SourcePod: prometheus, SourceIP: prometheus.IP,
		DestPod: backend, DestIP: backend.IP,
		DestPort: 80, Protocol: tcp, Direction: DirectionIngress,
	}, cluster)
	printResult("prometheus(monitoring) -> backend:80", allowed, reason)

	allowed, reason = EvaluateTraffic(Traffic{
		SourcePod: devApp, SourceIP: devApp.IP,
		DestPod: backend, DestIP: backend.IP,
		DestPort: 9090, Protocol: tcp, Direction: DirectionIngress,
	}, cluster)
	printResult("dev-app(development) -> backend:9090", allowed, reason)
	fmt.Println()

	// ------------------------------------------------------------------
	// 시나리오 5: IPBlock + Except
	// ------------------------------------------------------------------
	fmt.Println("--- 시나리오 5: IPBlock CIDR 매칭 (Except 포함) ---")
	fmt.Println()

	cluster.Policies = append(cluster.Policies, NetworkPolicy{
		Name:      "allow-external-cidr",
		Namespace: "production",
		Spec: NetworkPolicySpec{
			PodSelector: *labelSelector(Labels{"app": "frontend"}),
			PolicyTypes: []PolicyType{PolicyTypeIngress},
			Ingress: []NetworkPolicyIngressRule{
				{
					From: []NetworkPolicyPeer{
						{
							IPBlock: &IPBlock{
								CIDR:   "203.0.113.0/24",
								Except: []string{"203.0.113.128/25"},
							},
						},
					},
					Ports: []NetworkPolicyPort{
						{Protocol: &tcp, Port: intPtr(443)},
					},
				},
			},
		},
	})

	// 203.0.113.10 -- CIDR에 포함, Except에 미포함 -> 허용
	allowed, reason = EvaluateTraffic(Traffic{
		SourceIP: "203.0.113.10", SourceExternal: true,
		DestPod: frontend, DestIP: frontend.IP,
		DestPort: 443, Protocol: tcp, Direction: DirectionIngress,
	}, cluster)
	printResult("203.0.113.10 -> frontend:443", allowed, reason)

	// 203.0.113.200 -- CIDR에 포함, Except에도 포함 -> 차단
	allowed, reason = EvaluateTraffic(Traffic{
		SourceIP: "203.0.113.200", SourceExternal: true,
		DestPod: frontend, DestIP: frontend.IP,
		DestPort: 443, Protocol: tcp, Direction: DirectionIngress,
	}, cluster)
	printResult("203.0.113.200 -> frontend:443", allowed, reason)

	// 198.51.100.5 -- CIDR 범위 밖 -> 차단
	allowed, reason = EvaluateTraffic(Traffic{
		SourceIP: "198.51.100.5", SourceExternal: true,
		DestPod: frontend, DestIP: frontend.IP,
		DestPort: 443, Protocol: tcp, Direction: DirectionIngress,
	}, cluster)
	printResult("198.51.100.5 -> frontend:443", allowed, reason)
	fmt.Println()

	// ------------------------------------------------------------------
	// 시나리오 6: 포트 범위 (Port ~ EndPort)
	// ------------------------------------------------------------------
	fmt.Println("--- 시나리오 6: 포트 범위 매칭 (Port ~ EndPort) ---")
	fmt.Println()

	cluster.Policies = append(cluster.Policies, NetworkPolicy{
		Name:      "allow-high-ports",
		Namespace: "production",
		Spec: NetworkPolicySpec{
			PodSelector: *labelSelector(Labels{"app": "backend"}),
			PolicyTypes: []PolicyType{PolicyTypeIngress},
			Ingress: []NetworkPolicyIngressRule{
				{
					From: []NetworkPolicyPeer{
						{PodSelector: allSelector()}, // 같은 NS의 모든 Pod
					},
					Ports: []NetworkPolicyPort{
						{Protocol: &tcp, Port: intPtr(8000), EndPort: intPtr(9000)},
					},
				},
			},
		},
	})

	allowed, reason = EvaluateTraffic(Traffic{
		SourcePod: frontend, SourceIP: frontend.IP,
		DestPod: backend, DestIP: backend.IP,
		DestPort: 8080, Protocol: tcp, Direction: DirectionIngress,
	}, cluster)
	printResult("frontend -> backend:8080 (범위 내)", allowed, reason)

	allowed, reason = EvaluateTraffic(Traffic{
		SourcePod: frontend, SourceIP: frontend.IP,
		DestPod: backend, DestIP: backend.IP,
		DestPort: 8500, Protocol: tcp, Direction: DirectionIngress,
	}, cluster)
	printResult("frontend -> backend:8500 (범위 내)", allowed, reason)

	allowed, reason = EvaluateTraffic(Traffic{
		SourcePod: frontend, SourceIP: frontend.IP,
		DestPod: backend, DestIP: backend.IP,
		DestPort: 7999, Protocol: tcp, Direction: DirectionIngress,
	}, cluster)
	printResult("frontend -> backend:7999 (범위 밖)", allowed, reason)

	allowed, reason = EvaluateTraffic(Traffic{
		SourcePod: frontend, SourceIP: frontend.IP,
		DestPod: backend, DestIP: backend.IP,
		DestPort: 9001, Protocol: tcp, Direction: DirectionIngress,
	}, cluster)
	printResult("frontend -> backend:9001 (범위 밖)", allowed, reason)
	fmt.Println()

	// ------------------------------------------------------------------
	// 시나리오 7: Egress 정책
	// ------------------------------------------------------------------
	fmt.Println("--- 시나리오 7: Egress 정책 ---")
	fmt.Println()

	egressCluster := &Cluster{
		Namespaces: cluster.Namespaces,
		Pods:       cluster.Pods,
		Policies: []NetworkPolicy{
			{
				Name:      "restrict-backend-egress",
				Namespace: "production",
				Spec: NetworkPolicySpec{
					PodSelector: *labelSelector(Labels{"app": "backend"}),
					PolicyTypes: []PolicyType{PolicyTypeEgress},
					Egress: []NetworkPolicyEgressRule{
						{
							To: []NetworkPolicyPeer{
								{PodSelector: labelSelector(Labels{"app": "database"})},
							},
							Ports: []NetworkPolicyPort{
								{Protocol: &tcp, Port: intPtr(5432)},
							},
						},
						{
							To: []NetworkPolicyPeer{
								{IPBlock: &IPBlock{CIDR: "0.0.0.0/0"}},
							},
							Ports: []NetworkPolicyPort{
								{Protocol: &tcp, Port: intPtr(53)},
								{Protocol: protoPtr(ProtocolUDP), Port: intPtr(53)},
							},
						},
					},
				},
			},
		},
	}

	allowed, reason = EvaluateTraffic(Traffic{
		SourcePod: backend, SourceIP: backend.IP,
		DestPod: database, DestIP: database.IP,
		DestPort: 5432, Protocol: tcp, Direction: DirectionEgress,
	}, egressCluster)
	printResult("backend -> database:5432 (Egress)", allowed, reason)

	allowed, reason = EvaluateTraffic(Traffic{
		SourcePod: backend, SourceIP: backend.IP,
		DestIP: "8.8.8.8", DestExternal: true,
		DestPort: 53, Protocol: ProtocolUDP, Direction: DirectionEgress,
	}, egressCluster)
	printResult("backend -> 8.8.8.8:53/UDP (Egress)", allowed, reason)

	allowed, reason = EvaluateTraffic(Traffic{
		SourcePod: backend, SourceIP: backend.IP,
		DestIP: "1.2.3.4", DestExternal: true,
		DestPort: 443, Protocol: tcp, Direction: DirectionEgress,
	}, egressCluster)
	printResult("backend -> 1.2.3.4:443 (Egress)", allowed, reason)
	fmt.Println()

	// ------------------------------------------------------------------
	// 시나리오 8: 검증 로직
	// ------------------------------------------------------------------
	fmt.Println("--- 시나리오 8: 검증 로직 ---")
	fmt.Println()

	// 유효한 포트
	errors := ValidateNetworkPolicyPort(NetworkPolicyPort{
		Protocol: &tcp, Port: intPtr(80),
	})
	fmt.Printf("  Port{TCP, 80}:        %s\n", formatErrors(errors))

	// 유효한 포트 범위
	errors = ValidateNetworkPolicyPort(NetworkPolicyPort{
		Protocol: &tcp, Port: intPtr(8000), EndPort: intPtr(9000),
	})
	fmt.Printf("  Port{TCP, 8000-9000}: %s\n", formatErrors(errors))

	// EndPort < Port (오류)
	errors = ValidateNetworkPolicyPort(NetworkPolicyPort{
		Protocol: &tcp, Port: intPtr(9000), EndPort: intPtr(8000),
	})
	fmt.Printf("  Port{TCP, 9000-8000}: %s\n", formatErrors(errors))

	// Port 미지정 + EndPort (오류)
	errors = ValidateNetworkPolicyPort(NetworkPolicyPort{
		Protocol: &tcp, EndPort: intPtr(8000),
	})
	fmt.Printf("  Port{TCP, nil-8000}:  %s\n", formatErrors(errors))

	fmt.Println()

	// IPBlock + PodSelector (오류)
	errors = ValidateNetworkPolicyPeer(NetworkPolicyPeer{
		IPBlock:     &IPBlock{CIDR: "10.0.0.0/8"},
		PodSelector: allSelector(),
	})
	fmt.Printf("  Peer{IPBlock + PodSelector}:  %s\n", formatErrors(errors))

	// IPBlock 단독 (유효)
	errors = ValidateNetworkPolicyPeer(NetworkPolicyPeer{
		IPBlock: &IPBlock{CIDR: "10.0.0.0/8"},
	})
	fmt.Printf("  Peer{IPBlock 단독}:           %s\n", formatErrors(errors))

	// Except가 CIDR 범위 밖 (오류)
	errors = ValidateNetworkPolicyPeer(NetworkPolicyPeer{
		IPBlock: &IPBlock{CIDR: "10.0.0.0/24", Except: []string{"192.168.0.0/24"}},
	})
	fmt.Printf("  Peer{CIDR 범위 밖 Except}:    %s\n", formatErrors(errors))

	// Peer 미지정 (오류)
	errors = ValidateNetworkPolicyPeer(NetworkPolicyPeer{})
	fmt.Printf("  Peer{빈 값}:                  %s\n", formatErrors(errors))

	fmt.Println()
	fmt.Println("=============================================================")
	fmt.Println(" 시뮬레이션 완료")
	fmt.Println("=============================================================")
}
