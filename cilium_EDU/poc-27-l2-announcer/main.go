package main

import (
	"encoding/hex"
	"fmt"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Cilium L2 Announcer 시뮬레이션
// =============================================================================
//
// Cilium L2 Announcer는 LoadBalancer 서비스 IP에 대해 ARP(IPv4)/NDP(IPv6)
// 응답을 생성하여 L2 네트워크에서 서비스 IP로의 트래픽을 수신할 수 있게 한다.
//
// 핵심 동작:
//   - 리더 선출: 각 서비스 IP에 대해 하나의 노드만 ARP 응답
//   - GARP: Gratuitous ARP로 즉시 MAC 테이블 업데이트
//   - Failover: 리더 노드 다운 시 다른 노드가 인계
//   - NDP: IPv6에서는 Neighbor Advertisement로 동일 동작
//
// 실제 코드 참조:
//   - pkg/l2announcer/: L2 announcer 로직
//   - pkg/datapath/linux/l2_announcer.go: datapath 통합
// =============================================================================

// --- MAC 주소 생성 ---

func randomMAC(r *rand.Rand) net.HardwareAddr {
	mac := make([]byte, 6)
	for i := range mac {
		mac[i] = byte(r.Intn(256))
	}
	mac[0] = (mac[0] | 0x02) & 0xFE // locally administered, unicast
	return mac
}

// --- ARP 패킷 구조 ---

type ARPOperation uint16

const (
	ARPRequest ARPOperation = 1
	ARPReply   ARPOperation = 2
)

func (op ARPOperation) String() string {
	if op == ARPRequest {
		return "ARP-REQUEST"
	}
	return "ARP-REPLY"
}

// ARPPacket은 ARP 패킷을 시뮬레이션한다.
type ARPPacket struct {
	Operation ARPOperation
	SenderMAC net.HardwareAddr
	SenderIP  net.IP
	TargetMAC net.HardwareAddr
	TargetIP  net.IP
}

func (p ARPPacket) String() string {
	targetMAC := p.TargetMAC.String()
	if p.Operation == ARPRequest {
		targetMAC = "ff:ff:ff:ff:ff:ff"
	}
	return fmt.Sprintf("%s: who-has %s? tell %s (src-mac: %s, dst-mac: %s)",
		p.Operation, p.TargetIP, p.SenderIP, p.SenderMAC, targetMAC)
}

// IsGratuitous는 GARP인지 검사한다.
func (p ARPPacket) IsGratuitous() bool {
	return p.SenderIP.Equal(p.TargetIP)
}

// --- NDP (IPv6 Neighbor Discovery) ---

type NDPType int

const (
	NeighborSolicitation  NDPType = 135
	NeighborAdvertisement NDPType = 136
)

func (t NDPType) String() string {
	if t == NeighborSolicitation {
		return "NS"
	}
	return "NA"
}

type NDPPacket struct {
	Type      NDPType
	SrcMAC    net.HardwareAddr
	SrcIP     net.IP
	TargetIP  net.IP
	Override  bool // NA에서 기존 캐시 덮어쓸지 여부
	Solicited bool
}

func (p NDPPacket) String() string {
	flags := ""
	if p.Type == NeighborAdvertisement {
		if p.Override {
			flags += "O"
		}
		if p.Solicited {
			flags += "S"
		}
		if flags != "" {
			flags = " [" + flags + "]"
		}
	}
	return fmt.Sprintf("%s: target=%s src=%s mac=%s%s",
		p.Type, p.TargetIP, p.SrcIP, p.SrcMAC, flags)
}

// --- L2 Announcer 노드 ---

// ServiceEntry는 LoadBalancer 서비스를 표현한다.
type ServiceEntry struct {
	Name      string
	Namespace string
	IP        net.IP
	IPv6      net.IP
}

// Node는 L2 Announcer가 실행되는 노드이다.
type Node struct {
	Name    string
	MAC     net.HardwareAddr
	IP      net.IP
	Alive   bool
	isLeader map[string]bool // serviceKey -> isLeader
	mu       sync.Mutex
}

func (n *Node) SetLeader(serviceKey string, leader bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.isLeader[serviceKey] = leader
}

func (n *Node) IsLeader(serviceKey string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.isLeader[serviceKey]
}

// --- ARP 테이블 ---

type ARPEntry struct {
	IP      net.IP
	MAC     net.HardwareAddr
	Updated time.Time
}

type ARPTable struct {
	entries map[string]*ARPEntry
	mu      sync.RWMutex
}

func NewARPTable() *ARPTable {
	return &ARPTable{entries: make(map[string]*ARPEntry)}
}

func (t *ARPTable) Update(ip net.IP, mac net.HardwareAddr) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries[ip.String()] = &ARPEntry{
		IP:      ip,
		MAC:     mac,
		Updated: time.Now(),
	}
}

func (t *ARPTable) Lookup(ip net.IP) *ARPEntry {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.entries[ip.String()]
}

func (t *ARPTable) Print() {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, entry := range t.entries {
		fmt.Printf("    %s -> %s (updated: %s)\n",
			entry.IP, entry.MAC, entry.Updated.Format("15:04:05"))
	}
}

// --- L2 Announcer ---

type L2Announcer struct {
	nodes    []*Node
	services []ServiceEntry
	arpTable *ARPTable
	r        *rand.Rand
}

func NewL2Announcer() *L2Announcer {
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	return &L2Announcer{
		arpTable: NewARPTable(),
		r:        r,
	}
}

func (la *L2Announcer) AddNode(name string, ip net.IP) *Node {
	node := &Node{
		Name:     name,
		MAC:      randomMAC(la.r),
		IP:       ip,
		Alive:    true,
		isLeader: make(map[string]bool),
	}
	la.nodes = append(la.nodes, node)
	return node
}

func (la *L2Announcer) AddService(svc ServiceEntry) {
	la.services = append(la.services, svc)
}

func serviceKey(svc ServiceEntry) string {
	return svc.Namespace + "/" + svc.Name
}

// ElectLeaders는 각 서비스 IP에 대해 리더를 선출한다.
// Cilium은 Kubernetes Lease 기반 리더 선출을 사용한다.
func (la *L2Announcer) ElectLeaders() {
	for _, svc := range la.services {
		key := serviceKey(svc)
		elected := false
		for _, node := range la.nodes {
			if node.Alive && !elected {
				node.SetLeader(key, true)
				elected = true
				fmt.Printf("  [ELECT] %s: 리더 = %s (MAC: %s)\n", key, node.Name, node.MAC)
			} else {
				node.SetLeader(key, false)
			}
		}
	}
}

// SendGARP는 Gratuitous ARP를 전송한다.
// 리더 선출 후 즉시 GARP를 보내 네트워크의 MAC 테이블을 업데이트한다.
func (la *L2Announcer) SendGARP(node *Node, svc ServiceEntry) {
	garp := ARPPacket{
		Operation: ARPReply,
		SenderMAC: node.MAC,
		SenderIP:  svc.IP,
		TargetMAC: net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		TargetIP:  svc.IP,
	}
	fmt.Printf("  [GARP]  %s -> %s (gratuitous=%v)\n", node.Name, garp, garp.IsGratuitous())
	la.arpTable.Update(svc.IP, node.MAC)
}

// HandleARPRequest는 ARP 요청을 처리한다.
func (la *L2Announcer) HandleARPRequest(pkt ARPPacket) {
	fmt.Printf("  [REQ]   %s\n", pkt)

	for _, svc := range la.services {
		if !svc.IP.Equal(pkt.TargetIP) {
			continue
		}
		key := serviceKey(svc)
		for _, node := range la.nodes {
			if node.Alive && node.IsLeader(key) {
				reply := ARPPacket{
					Operation: ARPReply,
					SenderMAC: node.MAC,
					SenderIP:  svc.IP,
					TargetMAC: pkt.SenderMAC,
					TargetIP:  pkt.SenderIP,
				}
				fmt.Printf("  [REPLY] %s responds: %s\n", node.Name, reply)
				la.arpTable.Update(svc.IP, node.MAC)
				return
			}
		}
	}
	fmt.Printf("  [MISS]  No leader for %s\n", pkt.TargetIP)
}

// SendUnsolicited NA는 IPv6 Unsolicited Neighbor Advertisement를 전송한다.
func (la *L2Announcer) SendUnsolicitedNA(node *Node, svc ServiceEntry) {
	if svc.IPv6 == nil {
		return
	}
	na := NDPPacket{
		Type:      NeighborAdvertisement,
		SrcMAC:    node.MAC,
		SrcIP:     svc.IPv6,
		TargetIP:  svc.IPv6,
		Override:  true,
		Solicited: false,
	}
	fmt.Printf("  [NA]    %s -> %s\n", node.Name, na)
}

// Failover는 노드 장애 시 리더를 재선출한다.
func (la *L2Announcer) Failover(failedNodeName string) {
	fmt.Printf("\n  [FAIL]  노드 %s 장애 감지!\n", failedNodeName)
	for _, node := range la.nodes {
		if node.Name == failedNodeName {
			node.Alive = false
		}
	}
	// 재선출
	la.ElectLeaders()
	// 새 리더가 GARP 전송
	for _, svc := range la.services {
		key := serviceKey(svc)
		for _, node := range la.nodes {
			if node.Alive && node.IsLeader(key) {
				la.SendGARP(node, svc)
				la.SendUnsolicitedNA(node, svc)
			}
		}
	}
}

func main() {
	fmt.Println("=== Cilium L2 Announcer 시뮬레이션 ===")
	fmt.Println()

	la := NewL2Announcer()

	// --- 노드 등록 ---
	fmt.Println("[1] 노드 등록")
	fmt.Println(strings.Repeat("-", 60))
	n1 := la.AddNode("worker-1", net.ParseIP("10.0.1.10"))
	n2 := la.AddNode("worker-2", net.ParseIP("10.0.1.11"))
	n3 := la.AddNode("worker-3", net.ParseIP("10.0.1.12"))
	for _, n := range []*Node{n1, n2, n3} {
		fmt.Printf("  Node: %s IP=%s MAC=%s\n", n.Name, n.IP, n.MAC)
	}
	fmt.Println()

	// --- 서비스 등록 ---
	fmt.Println("[2] LoadBalancer 서비스 등록")
	fmt.Println(strings.Repeat("-", 60))
	services := []ServiceEntry{
		{Name: "web-frontend", Namespace: "default", IP: net.ParseIP("10.0.100.1"), IPv6: net.ParseIP("fd00::100:1")},
		{Name: "api-gateway", Namespace: "default", IP: net.ParseIP("10.0.100.2"), IPv6: net.ParseIP("fd00::100:2")},
		{Name: "monitoring", Namespace: "monitoring", IP: net.ParseIP("10.0.100.3"), IPv6: nil},
	}
	for _, svc := range services {
		la.AddService(svc)
		v6str := "none"
		if svc.IPv6 != nil {
			v6str = svc.IPv6.String()
		}
		fmt.Printf("  Service: %s/%s IP=%s IPv6=%s\n", svc.Namespace, svc.Name, svc.IP, v6str)
	}
	fmt.Println()

	// --- 리더 선출 ---
	fmt.Println("[3] 리더 선출 (Lease 기반)")
	fmt.Println(strings.Repeat("-", 60))
	la.ElectLeaders()
	fmt.Println()

	// --- GARP 전송 ---
	fmt.Println("[4] Gratuitous ARP 전송")
	fmt.Println(strings.Repeat("-", 60))
	for _, svc := range la.services {
		key := serviceKey(svc)
		for _, node := range la.nodes {
			if node.IsLeader(key) {
				la.SendGARP(node, svc)
				la.SendUnsolicitedNA(node, svc)
			}
		}
	}
	fmt.Println()

	// --- ARP 요청 처리 ---
	fmt.Println("[5] ARP 요청 처리")
	fmt.Println(strings.Repeat("-", 60))
	clientMAC, _ := hex.DecodeString("aabbccddeeff")
	requests := []ARPPacket{
		{ARPRequest, clientMAC, net.ParseIP("10.0.1.50"), nil, net.ParseIP("10.0.100.1")},
		{ARPRequest, clientMAC, net.ParseIP("10.0.1.51"), nil, net.ParseIP("10.0.100.2")},
		{ARPRequest, clientMAC, net.ParseIP("10.0.1.52"), nil, net.ParseIP("10.0.100.3")},
		{ARPRequest, clientMAC, net.ParseIP("10.0.1.53"), nil, net.ParseIP("10.0.200.1")}, // unknown IP
	}
	for _, req := range requests {
		la.HandleARPRequest(req)
	}
	fmt.Println()

	// --- ARP 테이블 ---
	fmt.Println("[6] ARP 테이블 상태")
	fmt.Println(strings.Repeat("-", 60))
	la.arpTable.Print()
	fmt.Println()

	// --- Failover 시뮬레이션 ---
	fmt.Println("[7] Failover 시뮬레이션")
	fmt.Println(strings.Repeat("-", 60))
	la.Failover("worker-1")
	fmt.Println()

	// --- Failover 후 ARP 테이블 ---
	fmt.Println("[8] Failover 후 ARP 테이블")
	fmt.Println(strings.Repeat("-", 60))
	la.arpTable.Print()
	fmt.Println()

	// --- Failover 후 ARP 요청 재처리 ---
	fmt.Println("[9] Failover 후 ARP 요청 재처리")
	fmt.Println(strings.Repeat("-", 60))
	la.HandleARPRequest(ARPPacket{ARPRequest, clientMAC, net.ParseIP("10.0.1.50"), nil, net.ParseIP("10.0.100.1")})
	fmt.Println()

	fmt.Println("=== 시뮬레이션 완료 ===")
}
