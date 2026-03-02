// Cilium 네트워킹 서브시스템 시뮬레이션
//
// VXLAN 캡슐화/디캡슐화, NAT 변환, BGP 경로 광고,
// WireGuard 터널, 듀얼스택 처리를 순수 Go로 시뮬레이션한다.
//
// 실제 Cilium 코드:
//   bpf/lib/encap.h       — VXLAN/Geneve 캡슐화
//   bpf/lib/nat.h         — SNAT/DNAT 엔진
//   pkg/bgp/              — BGP 컨트롤 플레인
//   pkg/wireguard/agent/  — WireGuard Agent
//   bpf/lib/nat_46x64.h   — NAT46/64
//
// 실행: go run main.go
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"strings"
	"time"
)

// ============================================================
// 1. VXLAN 캡슐화/디캡슐화 — bpf/lib/encap.h 재현
//
//    실제 코드에서:
//    - __encap_with_nodeid4(): IPv4 터널 캡슐화
//    - ctx_set_encap_info4(): 커널에 캡슐화 메타데이터 설정
//    - get_tunnel_key(): 터널 키(VNI, Identity) 추출
//    - encap_and_redirect_with_nodeid(): 캡슐화 후 리다이렉트
// ============================================================

// VXLANHeader는 VXLAN 헤더를 나타낸다 (8바이트).
// 실제: UDP 페이로드의 첫 8바이트
type VXLANHeader struct {
	Flags uint8  // 플래그 (0x08 = VNI 유효)
	VNI   uint32 // 24비트 VXLAN Network Identifier
}

// OuterHeader는 외부 IP+UDP 헤더를 나타낸다.
// 실제: struct iphdr + struct udphdr
type OuterHeader struct {
	SrcIP   net.IP // 송신 노드 IP
	DstIP   net.IP // 수신 노드 IP (tunnel_endpoint)
	SrcPort uint16 // 소스 포트 (흐름 기반 해시)
	DstPort uint16 // VXLAN 기본 포트 (8472)
}

// InnerPacket은 원본 패킷을 나타낸다.
type InnerPacket struct {
	SrcMAC   string
	DstMAC   string
	SrcIP    string
	DstIP    string
	SrcPort  uint16
	DstPort  uint16
	Protocol string
	Payload  string
	Identity uint32 // Cilium security identity
}

// EncapsulatedPacket은 VXLAN 캡슐화된 전체 패킷이다.
type EncapsulatedPacket struct {
	Outer OuterHeader
	VXLAN VXLANHeader
	Inner InnerPacket
}

// vxlanEncapsulate는 bpf/lib/encap.h의 __encap_with_nodeid4()를 시뮬레이션한다.
// 실제 코드에서 seclabel(Identity)은 터널 키의 tunnel_id 필드에 저장된다.
func vxlanEncapsulate(inner InnerPacket, srcNodeIP, dstNodeIP net.IP, vni uint32) EncapsulatedPacket {
	// 실제: tunnel_gen_src_port_v4()에서 5-tuple 해시로 소스 포트 생성
	srcPort := hashSourcePort(inner.SrcIP, inner.DstIP, inner.SrcPort, inner.DstPort)

	return EncapsulatedPacket{
		Outer: OuterHeader{
			SrcIP:   srcNodeIP,
			DstIP:   dstNodeIP,
			SrcPort: srcPort,
			DstPort: 8472, // VXLAN 기본 포트
		},
		VXLAN: VXLANHeader{
			Flags: 0x08, // VNI 유효 플래그
			VNI:   vni,
		},
		Inner: inner,
	}
}

// vxlanDecapsulate는 bpf/bpf_overlay.c의 cil_from_overlay를 시뮬레이션한다.
// 실제: get_tunnel_key()로 VNI와 Identity를 추출하고,
//       lookup_ip4_endpoint()로 로컬 엔드포인트를 찾는다.
func vxlanDecapsulate(pkt EncapsulatedPacket) (InnerPacket, uint32, error) {
	if pkt.VXLAN.Flags&0x08 == 0 {
		return InnerPacket{}, 0, fmt.Errorf("DROP_NO_TUNNEL_KEY: VNI 플래그 없음")
	}
	inner := pkt.Inner
	return inner, pkt.VXLAN.VNI, nil
}

// hashSourcePort는 tunnel_gen_src_port_v4()를 단순화한 것이다.
// 실제: hash_from_tuple_v4()로 5-tuple 해시 → 소스 포트 생성
func hashSourcePort(srcIP, dstIP string, srcPort, dstPort uint16) uint16 {
	data := fmt.Sprintf("%s:%d->%s:%d", srcIP, srcPort, dstIP, dstPort)
	h := sha256.Sum256([]byte(data))
	port := binary.BigEndian.Uint16(h[:2])
	// 범위: 32768-65535 (에피메럴 포트)
	return 32768 + (port % 32768)
}

func demoVXLAN() {
	fmt.Println("============================================================")
	fmt.Println(" 1. VXLAN 캡슐화/디캡슐화 시뮬레이션")
	fmt.Println("    실제 코드: bpf/lib/encap.h, bpf/bpf_overlay.c")
	fmt.Println("============================================================")

	inner := InnerPacket{
		SrcMAC:   "aa:bb:cc:dd:ee:01",
		DstMAC:   "aa:bb:cc:dd:ee:02",
		SrcIP:    "10.0.1.15",
		DstIP:    "10.0.2.30",
		SrcPort:  45678,
		DstPort:  80,
		Protocol: "TCP",
		Payload:  "GET / HTTP/1.1",
		Identity: 12345, // Cilium security identity
	}

	srcNode := net.ParseIP("192.168.1.10")
	dstNode := net.ParseIP("192.168.1.20")
	vni := uint32(12345) // Identity를 VNI로 전달

	fmt.Printf("\n[캡슐화 전 — 원본 패킷]\n")
	fmt.Printf("  Src: %s:%d → Dst: %s:%d (%s)\n",
		inner.SrcIP, inner.SrcPort, inner.DstIP, inner.DstPort, inner.Protocol)
	fmt.Printf("  Identity: %d, Payload: %q\n", inner.Identity, inner.Payload)

	// 캡슐화 (encap_and_redirect_with_nodeid)
	encapped := vxlanEncapsulate(inner, srcNode, dstNode, vni)

	fmt.Printf("\n[캡슐화 후 — VXLAN 패킷]\n")
	fmt.Printf("  Outer: %s:%d → %s:%d (UDP)\n",
		encapped.Outer.SrcIP, encapped.Outer.SrcPort,
		encapped.Outer.DstIP, encapped.Outer.DstPort)
	fmt.Printf("  VXLAN: Flags=0x%02x, VNI=%d\n",
		encapped.VXLAN.Flags, encapped.VXLAN.VNI)
	fmt.Printf("  Inner: %s:%d → %s:%d\n",
		encapped.Inner.SrcIP, encapped.Inner.SrcPort,
		encapped.Inner.DstIP, encapped.Inner.DstPort)

	fmt.Printf("\n  패킷 구조:\n")
	fmt.Printf("  ┌────────────┬────────────┬──────┬────────┬────────────┬────────────┬──────┐\n")
	fmt.Printf("  │ Outer Eth  │ Outer IP   │ UDP  │ VXLAN  │ Inner Eth  │ Inner IP   │ Data │\n")
	fmt.Printf("  │            │ %s │ %d │ VNI=%d│            │ %s │      │\n",
		encapped.Outer.SrcIP, encapped.Outer.DstPort, encapped.VXLAN.VNI, encapped.Inner.SrcIP)
	fmt.Printf("  │            │ →%s│      │        │            │ →%s│      │\n",
		encapped.Outer.DstIP, encapped.Inner.DstIP)
	fmt.Printf("  └────────────┴────────────┴──────┴────────┴────────────┴────────────┴──────┘\n")

	// 디캡슐화 (cil_from_overlay → get_tunnel_key)
	decapped, extractedVNI, err := vxlanDecapsulate(encapped)
	if err != nil {
		fmt.Printf("\n  ERROR: %s\n", err)
		return
	}

	fmt.Printf("\n[디캡슐화 후 — 복원된 패킷]\n")
	fmt.Printf("  Src: %s:%d → Dst: %s:%d (%s)\n",
		decapped.SrcIP, decapped.SrcPort, decapped.DstIP, decapped.DstPort, decapped.Protocol)
	fmt.Printf("  추출된 VNI(Identity): %d\n", extractedVNI)
	fmt.Printf("  Payload: %q\n", decapped.Payload)
	fmt.Println()
}

// ============================================================
// 2. NAT 변환 시뮬레이션 — bpf/lib/nat.h 재현
//
//    실제 코드에서:
//    - snat_v4_new_mapping(): SNAT 매핑 생성
//    - set_v4_rtuple(): 역방향 튜플 설정
//    - __snat_try_keep_port(): 포트 보존 시도
//    - snat_v4_nat_handle_mapping(): NAT 처리 메인 함수
//    - cilium_snat_v4_external: BPF LRU Hash Map
// ============================================================

type NATDirection int

const (
	NAT_DIR_EGRESS  NATDirection = 0 // TUPLE_F_OUT
	NAT_DIR_INGRESS NATDirection = 1 // TUPLE_F_IN
)

func (d NATDirection) String() string {
	if d == NAT_DIR_EGRESS {
		return "EGRESS"
	}
	return "INGRESS"
}

// NATKey는 bpf/lib/nat.h의 ipv4_ct_tuple을 시뮬레이션한다.
type NATKey struct {
	SrcAddr  string
	DstAddr  string
	SrcPort  uint16
	DstPort  uint16
	Protocol uint8
	Dir      NATDirection
}

func (k NATKey) String() string {
	return fmt.Sprintf("%s:%d→%s:%d/%d [%s]",
		k.SrcAddr, k.SrcPort, k.DstAddr, k.DstPort, k.Protocol, k.Dir)
}

// NATEntry는 ipv4_nat_entry를 시뮬레이션한다.
type NATEntry struct {
	ToAddr  string // to_saddr 또는 to_daddr
	ToPort  uint16 // to_sport 또는 to_dport
	Created time.Time
}

// NATTarget은 ipv4_nat_target을 시뮬레이션한다.
type NATTarget struct {
	Addr    string // 마스커레이드 주소 (보통 노드 IP)
	MinPort uint16 // 포트 범위 시작
	MaxPort uint16 // 포트 범위 끝
}

// NATMap은 cilium_snat_v4_external (BPF_MAP_TYPE_LRU_HASH)를 시뮬레이션한다.
type NATMap struct {
	entries     map[NATKey]*NATEntry
	maxEntries  int
	nextPort    uint16
	allocations int
}

func newNATMap(maxEntries int) *NATMap {
	return &NATMap{
		entries:    make(map[NATKey]*NATEntry),
		maxEntries: maxEntries,
		nextPort:   32768,
	}
}

// snatNewMapping은 snat_v4_new_mapping()을 시뮬레이션한다.
// 포트 할당, 정방향+역방향 매핑 쌍 생성을 재현한다.
func (m *NATMap) snatNewMapping(srcAddr string, srcPort uint16,
	dstAddr string, dstPort uint16, proto uint8,
	target NATTarget) (uint16, error) {

	// __snat_try_keep_port: 원래 포트를 유지할 수 있으면 유지
	allocPort := srcPort
	if allocPort < target.MinPort || allocPort > target.MaxPort {
		allocPort = m.nextPort
		m.nextPort++
		if m.nextPort > target.MaxPort {
			m.nextPort = target.MinPort
		}
	}

	// 포트 충돌 검사 (실제: SNAT_COLLISION_RETRIES = 32회)
	for retries := 0; retries < 32; retries++ {
		revKey := NATKey{
			SrcAddr:  dstAddr,
			DstAddr:  target.Addr,
			SrcPort:  dstPort,
			DstPort:  allocPort,
			Protocol: proto,
			Dir:      NAT_DIR_INGRESS,
		}
		if _, exists := m.entries[revKey]; !exists {
			// RevSNAT 엔트리 생성 (응답 패킷 매칭용)
			m.entries[revKey] = &NATEntry{
				ToAddr:  srcAddr,
				ToPort:  srcPort,
				Created: time.Now(),
			}

			// SNAT 엔트리 생성 (나가는 패킷 매칭용)
			fwdKey := NATKey{
				SrcAddr:  srcAddr,
				DstAddr:  dstAddr,
				SrcPort:  srcPort,
				DstPort:  dstPort,
				Protocol: proto,
				Dir:      NAT_DIR_EGRESS,
			}
			m.entries[fwdKey] = &NATEntry{
				ToAddr:  target.Addr,
				ToPort:  allocPort,
				Created: time.Now(),
			}

			m.allocations++
			return allocPort, nil
		}
		allocPort = target.MinPort + uint16((int(allocPort)+1-int(target.MinPort))%(int(target.MaxPort)-int(target.MinPort)+1))
	}

	return 0, fmt.Errorf("DROP_NAT_NO_MAPPING: 포트 할당 실패 (32회 재시도 초과)")
}

// reverseLookup은 역방향 NAT 조회를 시뮬레이션한다.
func (m *NATMap) reverseLookup(srcAddr string, srcPort uint16,
	dstAddr string, dstPort uint16, proto uint8) (*NATEntry, bool) {
	key := NATKey{
		SrcAddr:  srcAddr,
		DstAddr:  dstAddr,
		SrcPort:  srcPort,
		DstPort:  dstPort,
		Protocol: proto,
		Dir:      NAT_DIR_INGRESS,
	}
	entry, ok := m.entries[key]
	return entry, ok
}

// ServiceDNAT는 서비스 DNAT를 시뮬레이션한다.
type ServiceDNAT struct {
	ServiceVIP  string
	ServicePort uint16
	Backends    []struct {
		IP   string
		Port uint16
	}
	nextBackend int
}

func (s *ServiceDNAT) translate(dstIP string, dstPort uint16) (string, uint16, bool) {
	if dstIP == s.ServiceVIP && dstPort == s.ServicePort {
		backend := s.Backends[s.nextBackend%len(s.Backends)]
		s.nextBackend++
		return backend.IP, backend.Port, true
	}
	return dstIP, dstPort, false
}

func demoNAT() {
	fmt.Println("============================================================")
	fmt.Println(" 2. NAT 변환 시뮬레이션 (SNAT/DNAT/RevNAT)")
	fmt.Println("    실제 코드: bpf/lib/nat.h, pkg/maps/nat/")
	fmt.Println("============================================================")

	natMap := newNATMap(65536)
	nodeIP := "192.168.1.10"
	target := NATTarget{
		Addr:    nodeIP,
		MinPort: 32768,
		MaxPort: 65535,
	}

	// --- SNAT (Pod → 외부) ---
	fmt.Println("\n--- SNAT (Pod → 외부) ---")
	podIP := "10.0.1.15"
	podPort := uint16(45678)
	extIP := "8.8.8.8"
	extPort := uint16(443)

	fmt.Printf("  원본: %s:%d → %s:%d (TCP)\n", podIP, podPort, extIP, extPort)

	allocPort, err := natMap.snatNewMapping(podIP, podPort, extIP, extPort, 6, target)
	if err != nil {
		fmt.Printf("  ERROR: %s\n", err)
		return
	}

	fmt.Printf("  SNAT 후: %s:%d → %s:%d\n", nodeIP, allocPort, extIP, extPort)
	fmt.Printf("  매핑 저장:\n")
	fmt.Printf("    정방향: (%s:%d→%s:%d, EGRESS) → {%s:%d}\n",
		podIP, podPort, extIP, extPort, nodeIP, allocPort)
	fmt.Printf("    역방향: (%s:%d→%s:%d, INGRESS) → {%s:%d}\n",
		extIP, extPort, nodeIP, allocPort, podIP, podPort)

	// --- 역방향 NAT (응답) ---
	fmt.Println("\n--- 역방향 NAT (외부 → Pod 응답) ---")
	fmt.Printf("  수신: %s:%d → %s:%d\n", extIP, extPort, nodeIP, allocPort)

	entry, found := natMap.reverseLookup(extIP, extPort, nodeIP, allocPort, 6)
	if found {
		fmt.Printf("  RevNAT 매칭! 원래 목적지: %s:%d\n", entry.ToAddr, entry.ToPort)
		fmt.Printf("  복원 후: %s:%d → %s:%d\n", extIP, extPort, entry.ToAddr, entry.ToPort)
	}

	// --- DNAT (서비스 접근) ---
	fmt.Println("\n--- DNAT (서비스 접근) ---")
	svc := ServiceDNAT{
		ServiceVIP:  "10.96.0.1",
		ServicePort: 80,
		Backends: []struct {
			IP   string
			Port uint16
		}{
			{"10.0.2.30", 8080},
			{"10.0.3.40", 8080},
			{"10.0.4.50", 8080},
		},
	}

	for i := 0; i < 3; i++ {
		clientPort := uint16(50000 + i)
		fmt.Printf("  요청 %d: %s:%d → %s:%d\n",
			i+1, podIP, clientPort, svc.ServiceVIP, svc.ServicePort)
		backendIP, backendPort, matched := svc.translate(svc.ServiceVIP, svc.ServicePort)
		if matched {
			fmt.Printf("    DNAT → %s:%d (Backend %d)\n", backendIP, backendPort, i+1)
		}
	}
	fmt.Println()
}

// ============================================================
// 3. BGP 경로 광고 시뮬레이션 — pkg/bgp/ 재현
//
//    실제 코드에서:
//    - GoBGPServer: GoBGP 래퍼 (pkg/bgp/gobgp/server.go)
//    - PodCIDRReconciler: Pod CIDR 광고 (reconciler/pod_cidr.go)
//    - NeighborReconciler: 피어 관리 (reconciler/neighbor.go)
//    - types.Path: 경로 객체 (types/bgp.go)
//    - types.Neighbor: 피어 객체 (types/bgp.go)
// ============================================================

type BGPFamily struct {
	AFI  string // "IPv4" 또는 "IPv6"
	SAFI string // "unicast"
}

type BGPPath struct {
	Prefix     string   // NLRI (예: "10.0.1.0/24")
	NextHop    string   // 다음 홉 IP
	ASPath     []uint32 // AS 경로
	LocalPref  uint32   // Local Preference
	Origin     string   // "IGP", "EGP", "Incomplete"
	Family     BGPFamily
	Best       bool
	ReceivedAt time.Time
}

func (p BGPPath) String() string {
	asPath := make([]string, len(p.ASPath))
	for i, as := range p.ASPath {
		asPath[i] = fmt.Sprintf("%d", as)
	}
	best := ""
	if p.Best {
		best = " *>"
	}
	return fmt.Sprintf("%s  Prefix=%-18s NextHop=%-15s ASPath=[%s] LP=%d Origin=%s",
		best, p.Prefix, p.NextHop, strings.Join(asPath, " "), p.LocalPref, p.Origin)
}

type BGPNeighbor struct {
	Address       string
	RemoteASN     uint32
	State         string // "Idle", "Connect", "Active", "OpenSent", "OpenConfirm", "Established"
	RoutesReceived int
	RoutesSent     int
}

type BGPRouter struct {
	ASN       uint32
	RouterID  string
	Neighbors map[string]*BGPNeighbor
	RIB       map[string][]BGPPath // AFI/SAFI → Paths
	LocalPrefixes []string         // 로컬에서 광고할 프리픽스
}

func newBGPRouter(asn uint32, routerID string) *BGPRouter {
	return &BGPRouter{
		ASN:       asn,
		RouterID:  routerID,
		Neighbors: make(map[string]*BGPNeighbor),
		RIB:       make(map[string][]BGPPath),
	}
}

// addNeighbor는 NeighborReconciler.Reconcile()을 시뮬레이션한다.
func (r *BGPRouter) addNeighbor(addr string, remoteASN uint32) {
	r.Neighbors[addr] = &BGPNeighbor{
		Address:   addr,
		RemoteASN: remoteASN,
		State:     "Established",
	}
}

// advertisePodCIDR는 PodCIDRReconciler를 시뮬레이션한다.
func (r *BGPRouter) advertisePodCIDR(prefix string) {
	path := BGPPath{
		Prefix:     prefix,
		NextHop:    r.RouterID,
		ASPath:     []uint32{r.ASN},
		LocalPref:  100,
		Origin:     "IGP",
		Family:     BGPFamily{AFI: "IPv4", SAFI: "unicast"},
		Best:       true,
		ReceivedAt: time.Now(),
	}
	r.RIB["ipv4-unicast"] = append(r.RIB["ipv4-unicast"], path)
	r.LocalPrefixes = append(r.LocalPrefixes, prefix)

	// 모든 피어에게 광고
	for _, neighbor := range r.Neighbors {
		neighbor.RoutesSent++
	}
}

// receiveRoute는 피어로부터 경로를 수신하는 것을 시뮬레이션한다.
func (r *BGPRouter) receiveRoute(from string, prefix, nextHop string, asPath []uint32) {
	path := BGPPath{
		Prefix:     prefix,
		NextHop:    nextHop,
		ASPath:     append(asPath, r.ASN), // 자신의 AS 추가하지 않음 (수신 시)
		LocalPref:  100,
		Origin:     "IGP",
		Family:     BGPFamily{AFI: "IPv4", SAFI: "unicast"},
		Best:       true,
		ReceivedAt: time.Now(),
	}
	// 실제: import policy 확인 (globalAllowLocalPolicyName)
	path.ASPath = asPath // 수신된 AS Path 그대로 유지
	r.RIB["ipv4-unicast"] = append(r.RIB["ipv4-unicast"], path)

	if n, ok := r.Neighbors[from]; ok {
		n.RoutesReceived++
	}
}

func demoBGP() {
	fmt.Println("============================================================")
	fmt.Println(" 3. BGP 경로 광고 시뮬레이션")
	fmt.Println("    실제 코드: pkg/bgp/gobgp/server.go, pkg/bgp/types/bgp.go")
	fmt.Println("============================================================")

	// 노드 라우터 생성 (AS 65001)
	node1 := newBGPRouter(65001, "192.168.1.10")
	node2 := newBGPRouter(65001, "192.168.1.20")
	torRouter := newBGPRouter(65000, "192.168.1.1")

	// 피어링 설정 (NeighborReconciler.Reconcile)
	node1.addNeighbor("192.168.1.1", 65000)   // node1 ↔ ToR
	node2.addNeighbor("192.168.1.1", 65000)   // node2 ↔ ToR
	torRouter.addNeighbor("192.168.1.10", 65001)
	torRouter.addNeighbor("192.168.1.20", 65001)

	fmt.Println("\n[BGP 피어링 구성]")
	fmt.Printf("  Node 1 (AS %d, Router-ID: %s)\n", node1.ASN, node1.RouterID)
	for _, n := range node1.Neighbors {
		fmt.Printf("    Neighbor: %s (AS %d) State=%s\n", n.Address, n.RemoteASN, n.State)
	}
	fmt.Printf("  Node 2 (AS %d, Router-ID: %s)\n", node2.ASN, node2.RouterID)
	for _, n := range node2.Neighbors {
		fmt.Printf("    Neighbor: %s (AS %d) State=%s\n", n.Address, n.RemoteASN, n.State)
	}
	fmt.Printf("  ToR Router (AS %d, Router-ID: %s)\n", torRouter.ASN, torRouter.RouterID)
	for _, n := range torRouter.Neighbors {
		fmt.Printf("    Neighbor: %s (AS %d) State=%s\n", n.Address, n.RemoteASN, n.State)
	}

	// PodCIDR 광고 (PodCIDRReconciler)
	node1.advertisePodCIDR("10.0.1.0/24")
	node2.advertisePodCIDR("10.0.2.0/24")

	// ToR가 경로 수신
	torRouter.receiveRoute("192.168.1.10", "10.0.1.0/24", "192.168.1.10", []uint32{65001})
	torRouter.receiveRoute("192.168.1.20", "10.0.2.0/24", "192.168.1.20", []uint32{65001})

	fmt.Println("\n[BGP RIB — ToR Router]")
	fmt.Println("  Status  Prefix              NextHop          ASPath       LP   Origin")
	fmt.Println("  ------  ------------------  ---------------  -----------  ---  ------")
	for _, paths := range torRouter.RIB {
		for _, p := range paths {
			fmt.Printf("  %s\n", p)
		}
	}

	fmt.Println("\n[BGP RIB — Node 1]")
	fmt.Println("  Status  Prefix              NextHop          ASPath       LP   Origin")
	fmt.Println("  ------  ------------------  ---------------  -----------  ---  ------")
	for _, paths := range node1.RIB {
		for _, p := range paths {
			fmt.Printf("  %s\n", p)
		}
	}

	fmt.Println()
}

// ============================================================
// 4. WireGuard 터널 시뮬레이션 — pkg/wireguard/agent/ 재현
//
//    실제 코드에서:
//    - Agent.init(): 키 페어 생성, wg 디바이스 설정
//    - Agent.Start(): IPCache/NodeManager 구독, 피어 설정
//    - updatePeer(): 피어 추가/갱신 (AllowedIPs 포함)
//    - initLocalNodeFromWireGuard(): 공개키를 CiliumNode에 게시
//
//    WireGuard는 Noise 프로토콜 (ChaCha20-Poly1305)을 사용하지만,
//    여기서는 SHA256 해시를 사용하여 암호화를 은유적으로 시뮬레이션한다.
// ============================================================

type WireGuardKey struct {
	PrivateKey string
	PublicKey  string
}

type WireGuardPeer struct {
	PublicKey  string
	Endpoint  string // IP:Port
	AllowedIPs []string
}

type WireGuardAgent struct {
	NodeName   string
	ListenPort int
	KeyPair    WireGuardKey
	Peers      map[string]*WireGuardPeer // 노드명 → 피어
}

// generateKeyPair는 loadOrGeneratePrivKey()를 시뮬레이션한다.
func generateKeyPair(seed string) WireGuardKey {
	privHash := sha256.Sum256([]byte("private:" + seed))
	pubHash := sha256.Sum256([]byte("public:" + seed))
	return WireGuardKey{
		PrivateKey: hex.EncodeToString(privHash[:16]),
		PublicKey:  hex.EncodeToString(pubHash[:16]),
	}
}

func newWireGuardAgent(nodeName string) *WireGuardAgent {
	return &WireGuardAgent{
		NodeName:   nodeName,
		ListenPort: 51871, // types.ListenPort
		KeyPair:    generateKeyPair(nodeName),
		Peers:      make(map[string]*WireGuardPeer),
	}
}

// addPeer는 Agent.updatePeer()를 시뮬레이션한다.
func (a *WireGuardAgent) addPeer(nodeName, pubKey, endpoint string, allowedIPs []string) {
	a.Peers[nodeName] = &WireGuardPeer{
		PublicKey:  pubKey,
		Endpoint:  endpoint,
		AllowedIPs: allowedIPs,
	}
}

// encrypt는 WireGuard 암호화를 은유적으로 시뮬레이션한다.
// 실제: Noise 프로토콜, ChaCha20-Poly1305 AEAD
func (a *WireGuardAgent) encrypt(data string, peerPubKey string) string {
	key := a.KeyPair.PrivateKey + peerPubKey
	h := sha256.Sum256([]byte(key + data))
	return hex.EncodeToString(h[:]) + "|" + fmt.Sprintf("len=%d", len(data))
}

// decrypt는 WireGuard 복호화를 은유적으로 시뮬레이션한다.
func (a *WireGuardAgent) decrypt(encrypted string, peerPubKey string) string {
	// 실제에서는 AEAD로 복호화. 여기서는 시뮬레이션.
	parts := strings.Split(encrypted, "|")
	if len(parts) >= 2 {
		return fmt.Sprintf("[복호화된 데이터: %s]", parts[1])
	}
	return "[복호화 실패]"
}

func demoWireGuard() {
	fmt.Println("============================================================")
	fmt.Println(" 4. WireGuard 터널 시뮬레이션")
	fmt.Println("    실제 코드: pkg/wireguard/agent/agent.go")
	fmt.Println("============================================================")

	// 두 노드의 WireGuard Agent 초기화
	agent1 := newWireGuardAgent("node-1")
	agent2 := newWireGuardAgent("node-2")

	fmt.Println("\n[키 페어 생성 — loadOrGeneratePrivKey()]")
	fmt.Printf("  Node 1: PubKey=%s...\n", agent1.KeyPair.PublicKey[:16])
	fmt.Printf("  Node 2: PubKey=%s...\n", agent2.KeyPair.PublicKey[:16])

	// 피어 교환 (CiliumNode CRD를 통해)
	agent1.addPeer("node-2", agent2.KeyPair.PublicKey,
		"192.168.1.20:51871", []string{"10.0.2.0/24"})
	agent2.addPeer("node-1", agent1.KeyPair.PublicKey,
		"192.168.1.10:51871", []string{"10.0.1.0/24"})

	fmt.Println("\n[피어 설정 — updatePeer()]")
	for name, peer := range agent1.Peers {
		fmt.Printf("  Node 1 → Peer %s:\n", name)
		fmt.Printf("    PubKey:     %s...\n", peer.PublicKey[:16])
		fmt.Printf("    Endpoint:   %s\n", peer.Endpoint)
		fmt.Printf("    AllowedIPs: %v\n", peer.AllowedIPs)
	}

	// 패킷 암호화/복호화 시뮬레이션
	plaintext := "10.0.1.15:45678→10.0.2.30:80 TCP GET /api"
	fmt.Printf("\n[암호화 — Node 1 → Node 2]\n")
	fmt.Printf("  평문: %s\n", plaintext)

	encrypted := agent1.encrypt(plaintext, agent2.KeyPair.PublicKey)
	fmt.Printf("  암호문: %s\n", encrypted[:40]+"...")

	decrypted := agent2.decrypt(encrypted, agent1.KeyPair.PublicKey)
	fmt.Printf("  복호화: %s\n", decrypted)

	fmt.Printf("\n  전체 흐름:\n")
	fmt.Printf("  Pod A → cilium_wg0 (암호화) → UDP:%d → 네트워크 → UDP:%d → cilium_wg0 (복호화) → Pod B\n",
		agent1.ListenPort, agent2.ListenPort)

	// 노드 암호화 제외(opt-out) 시뮬레이션
	fmt.Println("\n[노드 암호화 제외 — NodeEncryptionOptOutLabels]")
	fmt.Println("  노드 레이블이 opt-out 셀렉터에 매칭되면:")
	fmt.Println("    EncryptionKey = 0 → 다른 노드가 이 노드와의 통신을 암호화하지 않음")
	fmt.Println()
}

// ============================================================
// 5. 듀얼스택 IPv4/IPv6 처리 시뮬레이션
//
//    실제 코드에서:
//    - bpf/bpf_overlay.c: ETH_P_IP/ETH_P_IPV6로 분기
//    - bpf/lib/nat_46x64.h: NAT46/64 변환
//    - pkg/maps/nat/types.go: NatKey4/NatKey6
//    - pkg/datapath/tunnel/tunnel.go: UnderlayProtocol (IPv4/IPv6)
// ============================================================

type IPVersion int

const (
	IPv4Version IPVersion = 4
	IPv6Version IPVersion = 6
)

type DualStackPacket struct {
	Version  IPVersion
	SrcAddr  string
	DstAddr  string
	SrcPort  uint16
	DstPort  uint16
	Protocol string
}

func (p DualStackPacket) String() string {
	return fmt.Sprintf("IPv%d %s:%d → %s:%d (%s)",
		p.Version, p.SrcAddr, p.SrcPort, p.DstAddr, p.DstPort, p.Protocol)
}

// classifyPacket은 cil_from_overlay의 프로토콜 분기를 시뮬레이션한다.
func classifyPacket(pkt DualStackPacket) string {
	switch pkt.Version {
	case IPv4Version:
		return "CILIUM_CALL_IPV4_FROM_OVERLAY → handle_ipv4()"
	case IPv6Version:
		return "CILIUM_CALL_IPV6_FROM_OVERLAY → handle_ipv6()"
	default:
		return "DROP_UNKNOWN_L3"
	}
}

// nat46Translate는 bpf/lib/nat_46x64.h의 build_v4_in_v6_rfc6052를 시뮬레이션한다.
func nat46Translate(ipv4Addr string) string {
	// RFC 6052: 64:ff9b::/96 + IPv4 주소
	ip := net.ParseIP(ipv4Addr)
	if ip == nil {
		return ""
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return ""
	}
	return fmt.Sprintf("64:ff9b::%d.%d.%d.%d", ip4[0], ip4[1], ip4[2], ip4[3])
}

// nat64Translate는 IPv6 주소에서 IPv4를 추출한다 (build_v4_from_v6).
func nat64Translate(ipv6Addr string) string {
	// 64:ff9b:: 프리픽스 제거 후 IPv4 추출
	if strings.HasPrefix(ipv6Addr, "64:ff9b::") {
		return strings.TrimPrefix(ipv6Addr, "64:ff9b::")
	}
	// ::ffff: 프리픽스 (IPv4-mapped)
	if strings.HasPrefix(ipv6Addr, "::ffff:") {
		return strings.TrimPrefix(ipv6Addr, "::ffff:")
	}
	return ""
}

func demoDualStack() {
	fmt.Println("============================================================")
	fmt.Println(" 5. 듀얼스택 IPv4/IPv6 처리 시뮬레이션")
	fmt.Println("    실제 코드: bpf/bpf_overlay.c, bpf/lib/nat_46x64.h")
	fmt.Println("============================================================")

	packets := []DualStackPacket{
		{IPv4Version, "10.0.1.15", "10.0.2.30", 45678, 80, "TCP"},
		{IPv6Version, "fd00::1:f", "fd00::2:1e", 45678, 80, "TCP"},
		{IPv4Version, "10.0.1.20", "172.16.0.1", 55000, 443, "TCP"},
		{IPv6Version, "fd00::1:14", "fd00::3:1", 60000, 8080, "TCP"},
	}

	fmt.Println("\n[패킷 분류 — cil_from_overlay 프로토콜 분기]")
	for _, pkt := range packets {
		handler := classifyPacket(pkt)
		fmt.Printf("  %s\n    → %s\n", pkt, handler)
	}

	// NAT46/64 변환
	fmt.Println("\n[NAT46 변환 — IPv4 → IPv6 (RFC 6052)]")
	ipv4Addrs := []string{"10.0.1.15", "172.16.0.1", "8.8.8.8"}
	for _, addr := range ipv4Addrs {
		v6 := nat46Translate(addr)
		fmt.Printf("  %s → %s\n", addr, v6)
	}

	fmt.Println("\n[NAT64 변환 — IPv6 → IPv4]")
	ipv6Addrs := []string{
		"64:ff9b::10.0.1.15",
		"64:ff9b::8.8.8.8",
		"::ffff:172.16.0.1",
	}
	for _, addr := range ipv6Addrs {
		v4 := nat64Translate(addr)
		fmt.Printf("  %s → %s\n", addr, v4)
	}

	// NAT 맵 패밀리 분리
	fmt.Println("\n[NAT 맵 패밀리 분리 — pkg/maps/nat/types.go]")
	fmt.Println("  cilium_snat_v4_external (LRU Hash)")
	fmt.Println("    Key:   NatKey4 { TupleKey4Global }")
	fmt.Println("    Value: NatEntry4 { to_saddr, to_sport }")
	fmt.Println("  cilium_snat_v6_external (LRU Hash)")
	fmt.Println("    Key:   NatKey6 { TupleKey6Global }")
	fmt.Println("    Value: NatEntry6 { to_saddr, to_sport }")

	fmt.Println()
}

// ============================================================
// 메인 — 전체 시뮬레이션 실행
// ============================================================

func main() {
	fmt.Println("╔════════════════════════════════════════════════════════════╗")
	fmt.Println("║  Cilium 네트워킹 서브시스템 시뮬레이션                        ║")
	fmt.Println("║                                                           ║")
	fmt.Println("║  VXLAN, NAT, BGP, WireGuard, 듀얼스택 동작 원리를 체험한다    ║")
	fmt.Println("╚════════════════════════════════════════════════════════════╝")
	fmt.Println()

	demoVXLAN()
	demoNAT()
	demoBGP()
	demoWireGuard()
	demoDualStack()

	fmt.Println("============================================================")
	fmt.Println(" 시뮬레이션 완료")
	fmt.Println("============================================================")
	fmt.Println()
	fmt.Println("실제 Cilium 소스 코드 참조:")
	fmt.Println("  bpf/lib/encap.h       — VXLAN/Geneve 캡슐화 (__encap_with_nodeid4)")
	fmt.Println("  bpf/lib/nat.h         — NAT 엔진 (snat_v4_new_mapping)")
	fmt.Println("  bpf/bpf_overlay.c     — 오버레이 패킷 처리 (cil_from_overlay)")
	fmt.Println("  pkg/bgp/gobgp/        — GoBGP 통합 (GoBGPServer)")
	fmt.Println("  pkg/wireguard/agent/  — WireGuard Agent (키 관리, 피어 설정)")
	fmt.Println("  bpf/lib/nat_46x64.h   — NAT46/64 (build_v4_in_v6_rfc6052)")
	fmt.Println("  pkg/maps/nat/         — NAT BPF 맵 관리")
	fmt.Println("  pkg/datapath/tunnel/  — 터널 설정 (VXLAN/Geneve 프로토콜)")
}
