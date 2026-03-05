package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"strings"
)

// =============================================================================
// Cilium 네트워킹 시뮬레이션: VXLAN 터널링 + Direct Routing
// =============================================================================
// Cilium은 두 가지 네트워킹 모드를 지원한다:
// 1. VXLAN/Geneve 터널 모드: 오버레이 네트워크로 모든 인프라에서 동작
// 2. Direct Routing 모드: 네이티브 라우팅으로 최고 성능 달성
//
// 이 PoC는 VXLAN 캡슐화/역캡슐화와 FIB 조회를 시뮬레이션한다.
//
// VXLAN 패킷 구조:
//   [Outer Eth][Outer IP][Outer UDP:4789][VXLAN Header][Inner Eth][Inner IP][Payload]
//   VXLAN VNI 필드에 Cilium Security Identity를 인코딩한다.
// =============================================================================

// --- 네트워크 주소 타입 ---

// NodeInfo는 Cilium 노드 정보를 나타낸다
type NodeInfo struct {
	Name     string
	NodeIP   net.IP   // 노드의 외부 IP (터널 소스/목적지)
	PodCIDR  net.IPNet // 이 노드에 할당된 Pod CIDR
	MAC      net.HardwareAddr
}

// EndpointInfo는 Pod 엔드포인트 정보를 나타낸다
type EndpointInfo struct {
	PodIP    net.IP
	NodeName string
	Identity uint32
}

// --- VXLAN 헤더 구조체 ---

// VXLANHeader는 VXLAN 캡슐화 헤더를 나타낸다
// RFC 7348: 8바이트 고정 길이
type VXLANHeader struct {
	Flags uint8  // VXLAN 플래그 (0x08: VNI 유효)
	VNI   uint32 // 24비트 VXLAN Network Identifier
	// Cilium은 VNI 필드에 Security Identity를 인코딩한다
	// 이를 통해 수신 측에서 패킷의 출처 identity를 즉시 알 수 있다
}

// SecurityIdentity는 VNI에서 보안 ID를 추출한다
func (v VXLANHeader) SecurityIdentity() uint32 {
	return v.VNI // Cilium은 VNI의 24비트를 identity로 사용
}

func (v VXLANHeader) String() string {
	return fmt.Sprintf("VXLAN{flags=0x%02x, vni=%d(identity=%d)}", v.Flags, v.VNI, v.SecurityIdentity())
}

// --- IP/UDP 헤더 ---

type IPHeader struct {
	SrcIP    net.IP
	DstIP    net.IP
	Protocol uint8
	TTL      uint8
}

type UDPHeader struct {
	SrcPort uint16
	DstPort uint16
	Length  uint16
}

type EthHeader struct {
	DstMAC    net.HardwareAddr
	SrcMAC    net.HardwareAddr
	EtherType uint16
}

// --- 캡슐화된 패킷 구조 ---

// InnerPacket은 원본 패킷 (Pod 간 통신)
type InnerPacket struct {
	Eth     EthHeader
	IP      IPHeader
	Payload string
}

func (p InnerPacket) String() string {
	return fmt.Sprintf("%s → %s [%s]", p.IP.SrcIP, p.IP.DstIP, p.Payload)
}

// VXLANPacket은 VXLAN 캡슐화된 패킷 전체 구조
type VXLANPacket struct {
	OuterEth  EthHeader
	OuterIP   IPHeader
	OuterUDP  UDPHeader
	VXLAN     VXLANHeader
	Inner     InnerPacket
}

func (p VXLANPacket) String() string {
	return fmt.Sprintf(
		"VXLAN Packet:\n"+
			"  Outer: %s → %s (UDP:%d)\n"+
			"  VXLAN: VNI=%d (Security Identity=%d)\n"+
			"  Inner: %s",
		p.OuterIP.SrcIP, p.OuterIP.DstIP, p.OuterUDP.DstPort,
		p.VXLAN.VNI, p.VXLAN.SecurityIdentity(),
		p.Inner,
	)
}

// --- FIB (Forwarding Information Base) ---

// FIBEntry는 라우팅 테이블 엔트리이다
// 실제 Cilium: bpf_fib_lookup() 커널 헬퍼로 FIB 조회
type FIBEntry struct {
	Destination net.IPNet        // 목적지 네트워크
	NextHop     net.IP           // 다음 홉 IP
	Interface   string           // 출력 인터페이스
	NextHopMAC  net.HardwareAddr // 다음 홉 MAC (ARP 결과)
}

// FIBTable은 노드의 라우팅 테이블을 시뮬레이션한다
type FIBTable struct {
	entries []FIBEntry
}

// Lookup은 목적지 IP에 대한 FIB 조회를 수행한다
// 가장 긴 접두사 매칭 (Longest Prefix Match)
func (f *FIBTable) Lookup(dstIP net.IP) (*FIBEntry, bool) {
	var bestMatch *FIBEntry
	bestLen := -1

	for i, entry := range f.entries {
		if entry.Destination.Contains(dstIP) {
			ones, _ := entry.Destination.Mask.Size()
			if ones > bestLen {
				bestLen = ones
				bestMatch = &f.entries[i]
			}
		}
	}
	return bestMatch, bestMatch != nil
}

// --- Tunnel Map ---

// TunnelMap은 BPF 터널 맵을 시뮬레이션한다
// 실제 Cilium: cilium_tunnel_map (Pod CIDR → 원격 노드 IP 매핑)
type TunnelMap struct {
	entries map[string]net.IP // Pod CIDR → 원격 노드 IP
}

func NewTunnelMap() *TunnelMap {
	return &TunnelMap{entries: make(map[string]net.IP)}
}

// Lookup은 목적지 Pod IP의 터널 엔드포인트를 조회한다
func (tm *TunnelMap) Lookup(podIP net.IP) (net.IP, bool) {
	for cidr, nodeIP := range tm.entries {
		_, network, _ := net.ParseCIDR(cidr)
		if network.Contains(podIP) {
			return nodeIP, true
		}
	}
	return nil, false
}

// --- VXLAN 캡슐화/역캡슐화 엔진 ---

const VXLANPort = 4789

// TunnelEngine은 VXLAN 터널링 엔진이다
type TunnelEngine struct {
	localNode NodeInfo
	tunnelMap *TunnelMap
	fib       *FIBTable
	nodes     map[string]NodeInfo // 노드명 → 노드 정보
}

// Encapsulate는 패킷을 VXLAN으로 캡슐화한다
// 실제 Cilium: __encap_and_redirect_with_nodeid() in bpf/lib/encap.h
func (te *TunnelEngine) Encapsulate(inner InnerPacket, identity uint32) (*VXLANPacket, error) {
	// 1. 터널 맵에서 목적지 노드 IP 조회
	remoteNodeIP, found := te.tunnelMap.Lookup(inner.IP.DstIP)
	if !found {
		return nil, fmt.Errorf("터널 맵에서 목적지 %s를 찾을 수 없음", inner.IP.DstIP)
	}

	// 2. FIB 조회로 외부 패킷의 다음 홉 결정
	fibEntry, found := te.fib.Lookup(remoteNodeIP)
	if !found {
		return nil, fmt.Errorf("FIB에서 %s에 대한 경로를 찾을 수 없음", remoteNodeIP)
	}

	// 3. 소스 포트: 내부 패킷 해시 기반 (ECMP 분산을 위해)
	srcPort := hashPacket(inner)

	// 4. VXLAN 패킷 조립
	vxlanPkt := &VXLANPacket{
		OuterEth: EthHeader{
			DstMAC:    fibEntry.NextHopMAC,
			SrcMAC:    te.localNode.MAC,
			EtherType: 0x0800,
		},
		OuterIP: IPHeader{
			SrcIP:    te.localNode.NodeIP,
			DstIP:    remoteNodeIP,
			Protocol: 17, // UDP
			TTL:      64,
		},
		OuterUDP: UDPHeader{
			SrcPort: srcPort,
			DstPort: VXLANPort,
			Length:  0, // 단순화
		},
		VXLAN: VXLANHeader{
			Flags: 0x08,     // VNI 유효 플래그
			VNI:   identity, // Cilium: VNI에 Security Identity 인코딩
		},
		Inner: inner,
	}

	return vxlanPkt, nil
}

// Decapsulate는 VXLAN 패킷을 역캡슐화한다
// 실제 Cilium: handle_xgress() → VXLAN 인터페이스에서 수신 시
func (te *TunnelEngine) Decapsulate(vxlanPkt *VXLANPacket) (InnerPacket, uint32, error) {
	// 1. 외부 목적지가 자신인지 확인
	if !vxlanPkt.OuterIP.DstIP.Equal(te.localNode.NodeIP) {
		return InnerPacket{}, 0, fmt.Errorf(
			"외부 목적지 %s가 로컬 노드 %s가 아님",
			vxlanPkt.OuterIP.DstIP, te.localNode.NodeIP)
	}

	// 2. VXLAN 플래그 확인
	if vxlanPkt.VXLAN.Flags&0x08 == 0 {
		return InnerPacket{}, 0, fmt.Errorf("VXLAN VNI 플래그가 설정되지 않음")
	}

	// 3. Security Identity 추출 (VNI에서)
	identity := vxlanPkt.VXLAN.SecurityIdentity()

	// 4. 내부 패킷 반환
	return vxlanPkt.Inner, identity, nil
}

// hashPacket은 내부 패킷의 해시값으로 소스 포트를 생성한다
// ECMP 환경에서 동일 흐름이 같은 경로를 선택하도록 보장
func hashPacket(pkt InnerPacket) uint16 {
	h := uint32(0)
	for _, b := range pkt.IP.SrcIP.To4() {
		h = h*31 + uint32(b)
	}
	for _, b := range pkt.IP.DstIP.To4() {
		h = h*31 + uint32(b)
	}
	// 소스 포트 범위: 49152-65535 (ephemeral range)
	return uint16(49152 + (h % 16383))
}

// --- Direct Routing 엔진 ---

// DirectRoutingEngine은 네이티브 라우팅 모드를 시뮬레이션한다
// 터널 오버헤드 없이 직접 라우팅
type DirectRoutingEngine struct {
	localNode NodeInfo
	fib       *FIBTable
}

// Route는 직접 라우팅 모드에서 패킷을 전달한다
// 실제 Cilium: ipv4_host_policy_egress() → fib_redirect_v4()
func (dr *DirectRoutingEngine) Route(pkt InnerPacket) (*FIBEntry, error) {
	fibEntry, found := dr.fib.Lookup(pkt.IP.DstIP)
	if !found {
		return nil, fmt.Errorf("FIB에서 %s에 대한 경로를 찾을 수 없음", pkt.IP.DstIP)
	}
	return fibEntry, nil
}

// --- 헬퍼 함수 ---

func makeMAC(suffix byte) net.HardwareAddr {
	return net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, suffix}
}

func mustParseCIDR(s string) net.IPNet {
	_, network, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return *network
}

func printSection(title string) {
	fmt.Println()
	fmt.Println(strings.Repeat("═", 70))
	fmt.Printf("  %s\n", title)
	fmt.Println(strings.Repeat("═", 70))
}

func printPacketDiagram(inner InnerPacket) {
	fmt.Println("  ┌──────────────────────────────────────────────────┐")
	fmt.Println("  │             Inner Packet (원본)                  │")
	fmt.Printf("  │  Src: %-15s  →  Dst: %-15s  │\n", inner.IP.SrcIP, inner.IP.DstIP)
	fmt.Printf("  │  Payload: %-38s │\n", inner.Payload)
	fmt.Println("  └──────────────────────────────────────────────────┘")
}

func printVXLANDiagram(pkt *VXLANPacket) {
	identity := pkt.VXLAN.SecurityIdentity()
	fmt.Println("  ┌──────────────────────────────────────────────────────────────┐")
	fmt.Println("  │                    VXLAN 캡슐화된 패킷                       │")
	fmt.Println("  ├──────────────────────────────────────────────────────────────┤")
	fmt.Printf("  │  Outer Eth: %s → %s          │\n", pkt.OuterEth.SrcMAC, pkt.OuterEth.DstMAC)
	fmt.Printf("  │  Outer IP:  %-15s → %-15s          │\n", pkt.OuterIP.SrcIP, pkt.OuterIP.DstIP)
	fmt.Printf("  │  Outer UDP: src=%d dst=%d (VXLAN)                     │\n", pkt.OuterUDP.SrcPort, pkt.OuterUDP.DstPort)
	fmt.Println("  ├──────────────────────────────────────────────────────────────┤")
	fmt.Printf("  │  VXLAN: VNI=%d (Security Identity=%d)                       │\n", pkt.VXLAN.VNI, identity)
	fmt.Println("  ├──────────────────────────────────────────────────────────────┤")
	fmt.Printf("  │  Inner IP:  %-15s → %-15s          │\n", pkt.Inner.IP.SrcIP, pkt.Inner.IP.DstIP)
	fmt.Printf("  │  Payload: %-50s │\n", pkt.Inner.Payload)
	fmt.Println("  └──────────────────────────────────────────────────────────────┘")
}

// =============================================================================
// main
// =============================================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║        Cilium 네트워킹 시뮬레이션                                   ║")
	fmt.Println("║  VXLAN 캡슐화/역캡슐화 + FIB Lookup + Direct Routing               ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════════╝")

	// --- 클러스터 토폴로지 설정 ---
	// 3개 노드, 각 노드에 Pod CIDR 할당
	nodeA := NodeInfo{
		Name:    "node-a",
		NodeIP:  net.ParseIP("192.168.1.10"),
		PodCIDR: mustParseCIDR("10.0.0.0/24"),
		MAC:     makeMAC(0x0A),
	}
	nodeB := NodeInfo{
		Name:    "node-b",
		NodeIP:  net.ParseIP("192.168.1.20"),
		PodCIDR: mustParseCIDR("10.0.1.0/24"),
		MAC:     makeMAC(0x0B),
	}
	nodeC := NodeInfo{
		Name:    "node-c",
		NodeIP:  net.ParseIP("192.168.1.30"),
		PodCIDR: mustParseCIDR("10.0.2.0/24"),
		MAC:     makeMAC(0x0C),
	}

	// --- 터널 맵 설정 ---
	tunnelMap := NewTunnelMap()
	tunnelMap.entries["10.0.0.0/24"] = nodeA.NodeIP
	tunnelMap.entries["10.0.1.0/24"] = nodeB.NodeIP
	tunnelMap.entries["10.0.2.0/24"] = nodeC.NodeIP

	// --- FIB 테이블 설정 (Node A 기준) ---
	fib := &FIBTable{
		entries: []FIBEntry{
			{Destination: mustParseCIDR("192.168.1.0/24"), NextHop: nil,
				Interface: "eth0", NextHopMAC: makeMAC(0xFF)}, // 직접 연결
			{Destination: mustParseCIDR("10.0.0.0/24"), NextHop: nil,
				Interface: "cilium_host", NextHopMAC: makeMAC(0xC0)}, // 로컬 Pod
			{Destination: mustParseCIDR("10.0.1.0/24"), NextHop: nodeB.NodeIP,
				Interface: "eth0", NextHopMAC: nodeB.MAC}, // Node B Pod
			{Destination: mustParseCIDR("10.0.2.0/24"), NextHop: nodeC.NodeIP,
				Interface: "eth0", NextHopMAC: nodeC.MAC}, // Node C Pod
		},
	}

	// --- VXLAN 터널 엔진 (Node A) ---
	tunnelEngine := &TunnelEngine{
		localNode: nodeA,
		tunnelMap: tunnelMap,
		fib:       fib,
		nodes: map[string]NodeInfo{
			"node-a": nodeA, "node-b": nodeB, "node-c": nodeC,
		},
	}

	// --- Direct Routing 엔진 (Node A) ---
	directEngine := &DirectRoutingEngine{
		localNode: nodeA,
		fib:       fib,
	}

	// =====================================================
	// 시나리오 1: VXLAN 캡슐화 (Node A → Node B)
	// =====================================================
	printSection("시나리오 1: VXLAN 캡슐화 (Node A Pod → Node B Pod)")

	innerPkt := InnerPacket{
		Eth: EthHeader{
			DstMAC: makeMAC(0xBB), SrcMAC: makeMAC(0xAA), EtherType: 0x0800,
		},
		IP: IPHeader{
			SrcIP: net.ParseIP("10.0.0.5"), DstIP: net.ParseIP("10.0.1.15"),
			Protocol: 6, TTL: 64,
		},
		Payload: "HTTP GET /api/v1/pods",
	}

	fmt.Println("\n  [1] 원본 패킷:")
	printPacketDiagram(innerPkt)

	secIdentity := uint32(12345)
	vxlanPkt, err := tunnelEngine.Encapsulate(innerPkt, secIdentity)
	if err != nil {
		fmt.Printf("  캡슐화 실패: %v\n", err)
	} else {
		fmt.Println("\n  [2] VXLAN 캡슐화 결과:")
		printVXLANDiagram(vxlanPkt)
		fmt.Println("\n  [3] 캡슐화 과정:")
		fmt.Printf("      a. 터널 맵 조회: %s → Node %s (%s)\n",
			innerPkt.IP.DstIP, "B", "192.168.1.20")
		fmt.Printf("      b. FIB 조회: next-hop=%s, interface=%s\n",
			fib.entries[2].NextHop, fib.entries[2].Interface)
		fmt.Printf("      c. VNI에 Security Identity 인코딩: %d\n", secIdentity)
		srcPort := hashPacket(innerPkt)
		fmt.Printf("      d. 소스 포트 (내부 해시): %d (ECMP 분산용)\n", srcPort)
	}

	// =====================================================
	// 시나리오 2: VXLAN 역캡슐화 (Node B에서 수신)
	// =====================================================
	printSection("시나리오 2: VXLAN 역캡슐화 (Node B에서 수신)")

	// Node B의 터널 엔진 생성
	tunnelEngineB := &TunnelEngine{
		localNode: nodeB,
		tunnelMap: tunnelMap,
		fib:       fib,
	}

	if vxlanPkt != nil {
		fmt.Println("\n  [1] 수신된 VXLAN 패킷:")
		printVXLANDiagram(vxlanPkt)

		innerDecap, identity, err := tunnelEngineB.Decapsulate(vxlanPkt)
		if err != nil {
			fmt.Printf("  역캡슐화 실패: %v\n", err)
		} else {
			fmt.Println("\n  [2] 역캡슐화 결과:")
			printPacketDiagram(innerDecap)
			fmt.Printf("\n  [3] 추출된 Security Identity: %d\n", identity)
			fmt.Println("      → 이 identity로 수신 측 정책 검사 수행")
		}
	}

	// =====================================================
	// 시나리오 3: Direct Routing 모드
	// =====================================================
	printSection("시나리오 3: Direct Routing 모드 (터널 오버헤드 없음)")

	directPkt := InnerPacket{
		Eth: EthHeader{
			DstMAC: makeMAC(0xBB), SrcMAC: makeMAC(0xAA), EtherType: 0x0800,
		},
		IP: IPHeader{
			SrcIP: net.ParseIP("10.0.0.5"), DstIP: net.ParseIP("10.0.2.30"),
			Protocol: 6, TTL: 64,
		},
		Payload: "gRPC call",
	}

	fmt.Println("\n  [1] 원본 패킷:")
	printPacketDiagram(directPkt)

	fibEntry, err := directEngine.Route(directPkt)
	if err != nil {
		fmt.Printf("  라우팅 실패: %v\n", err)
	} else {
		fmt.Println("\n  [2] FIB 조회 결과:")
		fmt.Printf("      목적지: %s\n", fibEntry.Destination.String())
		fmt.Printf("      다음 홉: %s\n", fibEntry.NextHop)
		fmt.Printf("      인터페이스: %s\n", fibEntry.Interface)
		fmt.Printf("      다음 홉 MAC: %s\n", fibEntry.NextHopMAC)
		fmt.Println("\n  [3] Direct Routing은 VXLAN 헤더를 추가하지 않는다")
		fmt.Println("      → MTU 오버헤드 없음 (VXLAN은 50바이트 추가)")
		fmt.Println("      → 단, 네트워크 인프라가 Pod CIDR 라우팅을 지원해야 함")
	}

	// =====================================================
	// 시나리오 4: 패킷 크기 비교
	// =====================================================
	printSection("시나리오 4: VXLAN vs Direct Routing 패킷 크기 비교")

	// 바이트 크기 계산
	outerEthSize := 14
	outerIPSize := 20
	outerUDPSize := 8
	vxlanHeaderSize := 8
	innerEthSize := 14
	innerIPSize := 20
	innerTCPSize := 20 // TCP 헤더 최소
	payloadSize := 1400

	totalVXLAN := outerEthSize + outerIPSize + outerUDPSize + vxlanHeaderSize +
		innerEthSize + innerIPSize + innerTCPSize + payloadSize
	totalDirect := innerEthSize + innerIPSize + innerTCPSize + payloadSize
	overhead := totalVXLAN - totalDirect

	vxlanBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(vxlanBytes, uint32(totalVXLAN))
	directBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(directBytes, uint32(totalDirect))

	fmt.Printf("\n  %-30s %6s\n", "구성 요소", "크기")
	fmt.Println("  " + strings.Repeat("─", 40))
	fmt.Printf("  %-30s %4d B\n", "Outer Ethernet Header", outerEthSize)
	fmt.Printf("  %-30s %4d B\n", "Outer IP Header", outerIPSize)
	fmt.Printf("  %-30s %4d B\n", "Outer UDP Header", outerUDPSize)
	fmt.Printf("  %-30s %4d B\n", "VXLAN Header", vxlanHeaderSize)
	fmt.Println("  " + strings.Repeat("─", 40))
	fmt.Printf("  %-30s %4d B\n", "VXLAN 오버헤드 합계", overhead)
	fmt.Println("  " + strings.Repeat("─", 40))
	fmt.Printf("  %-30s %4d B\n", "Inner Ethernet Header", innerEthSize)
	fmt.Printf("  %-30s %4d B\n", "Inner IP Header", innerIPSize)
	fmt.Printf("  %-30s %4d B\n", "TCP Header", innerTCPSize)
	fmt.Printf("  %-30s %4d B\n", "Payload", payloadSize)
	fmt.Println("  " + strings.Repeat("─", 40))
	fmt.Printf("  %-30s %4d B\n", "VXLAN 모드 전체 크기", totalVXLAN)
	fmt.Printf("  %-30s %4d B\n", "Direct Routing 전체 크기", totalDirect)

	fmt.Println("\n  * VXLAN MTU 고려:")
	fmt.Println("    - 표준 MTU 1500에서 VXLAN 오버헤드 50B 차감")
	fmt.Println("    - Inner MTU = 1450B (또는 Jumbo Frame 사용)")
	fmt.Println()
}
