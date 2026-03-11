package main

import (
	"fmt"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Cilium IP Masquerade (SNAT) 시뮬레이션
// =============================================================================
//
// Cilium의 IP Masquerade는 Pod에서 외부로 나가는 트래픽의 소스 IP를
// 노드 IP로 변환(SNAT)한다. eBPF로 구현되어 iptables보다 효율적이다.
//
// 핵심 개념:
//   - Masquerade: Pod IP → Node IP (egress SNAT)
//   - Non-masquerade CIDR: 내부 대역은 SNAT 제외
//   - ip-masq-agent: ConfigMap으로 제외 대역 관리
//   - Port Allocation: 충돌 없는 소스 포트 할당
//   - Conntrack: 역방향 패킷의 DNAT (응답 복원)
//
// 실제 코드 참조:
//   - bpf/lib/nat.h: BPF NAT 구현
//   - pkg/datapath/linux/snat.go: SNAT 설정
//   - pkg/ip/masq.go: masquerade 로직
// =============================================================================

// --- 네트워크 구조체 ---

type Packet struct {
	SrcIP   net.IP
	DstIP   net.IP
	SrcPort uint16
	DstPort uint16
	Proto   string
}

func (p Packet) String() string {
	return fmt.Sprintf("%s:%d -> %s:%d [%s]", p.SrcIP, p.SrcPort, p.DstIP, p.DstPort, p.Proto)
}

// --- NAT 엔트리 ---

// NATEntry는 하나의 NAT 변환 레코드이다.
// BPF의 CT/NAT 맵 엔트리에 대응한다.
type NATEntry struct {
	OrigSrcIP   net.IP
	OrigSrcPort uint16
	TransIP     net.IP
	TransPort   uint16
	DstIP       net.IP
	DstPort     uint16
	Proto       string
	Created     time.Time
}

func (e NATEntry) String() string {
	return fmt.Sprintf("%s:%d -> %s:%d (orig: %s:%d)",
		e.TransIP, e.TransPort, e.DstIP, e.DstPort, e.OrigSrcIP, e.OrigSrcPort)
}

// --- 포트 할당기 ---

// PortAllocator는 SNAT 포트를 관리한다.
// Cilium은 1024-65535 범위에서 포트를 할당한다.
type PortAllocator struct {
	mu       sync.Mutex
	used     map[uint16]bool
	minPort  uint16
	maxPort  uint16
	r        *rand.Rand
}

func NewPortAllocator(min, max uint16) *PortAllocator {
	return &PortAllocator{
		used:    make(map[uint16]bool),
		minPort: min,
		maxPort: max,
		r:       rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (pa *PortAllocator) Allocate() (uint16, error) {
	pa.mu.Lock()
	defer pa.mu.Unlock()

	// 랜덤 시작점에서 포트 탐색 (hash collision 최소화)
	rangeSize := int(pa.maxPort - pa.minPort + 1)
	start := pa.r.Intn(rangeSize)
	for i := 0; i < rangeSize; i++ {
		port := pa.minPort + uint16((start+i)%rangeSize)
		if !pa.used[port] {
			pa.used[port] = true
			return port, nil
		}
	}
	return 0, fmt.Errorf("no available ports")
}

func (pa *PortAllocator) Release(port uint16) {
	pa.mu.Lock()
	defer pa.mu.Unlock()
	delete(pa.used, port)
}

// --- Non-Masquerade CIDR ---

// NonMasqConfig는 ip-masq-agent의 ConfigMap을 시뮬레이션한다.
// 이 대역으로의 트래픽은 SNAT를 적용하지 않는다.
type NonMasqConfig struct {
	CIDRs []*net.IPNet
}

func NewNonMasqConfig(cidrs []string) *NonMasqConfig {
	config := &NonMasqConfig{}
	for _, cidr := range cidrs {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			panic(fmt.Sprintf("invalid CIDR: %s", cidr))
		}
		config.CIDRs = append(config.CIDRs, network)
	}
	return config
}

func (c *NonMasqConfig) ShouldMasquerade(dstIP net.IP) bool {
	for _, cidr := range c.CIDRs {
		if cidr.Contains(dstIP) {
			return false // 내부 대역이므로 masquerade 불필요
		}
	}
	return true // 외부 대역이므로 masquerade 필요
}

// --- Masquerade Engine ---

type MasqueradeEngine struct {
	nodeIP    net.IP
	portAlloc *PortAllocator
	nonMasq   *NonMasqConfig
	natTable  map[string]*NATEntry // key: origSrc:origPort:dst:dstPort:proto
	mu        sync.RWMutex
	stats     MasqStats
}

type MasqStats struct {
	Masqueraded   int
	Skipped       int
	DeNATed       int
	PortExhausted int
}

func NewMasqueradeEngine(nodeIP string, nonMasqCIDRs []string) *MasqueradeEngine {
	return &MasqueradeEngine{
		nodeIP:    net.ParseIP(nodeIP),
		portAlloc: NewPortAllocator(32768, 60999),
		nonMasq:   NewNonMasqConfig(nonMasqCIDRs),
		natTable:  make(map[string]*NATEntry),
	}
}

func natKey(srcIP net.IP, srcPort uint16, dstIP net.IP, dstPort uint16, proto string) string {
	return fmt.Sprintf("%s:%d:%s:%d:%s", srcIP, srcPort, dstIP, dstPort, proto)
}

func reverseNatKey(transIP net.IP, transPort uint16, srcIP net.IP, srcPort uint16, proto string) string {
	return fmt.Sprintf("rev:%s:%d:%s:%d:%s", srcIP, srcPort, transIP, transPort, proto)
}

// ProcessEgress는 Pod에서 나가는 패킷을 처리한다.
// eBPF tc egress 훅에서 실행되는 snat_v4_process()를 시뮬레이션한다.
func (me *MasqueradeEngine) ProcessEgress(pkt Packet) (Packet, bool) {
	// 1. Non-masquerade CIDR 검사
	if !me.nonMasq.ShouldMasquerade(pkt.DstIP) {
		me.stats.Skipped++
		fmt.Printf("  [SKIP]   %s (non-masquerade CIDR)\n", pkt)
		return pkt, false
	}

	// 2. 이미 노드 IP인 경우 skip
	if pkt.SrcIP.Equal(me.nodeIP) {
		me.stats.Skipped++
		fmt.Printf("  [SKIP]   %s (already node IP)\n", pkt)
		return pkt, false
	}

	// 3. SNAT 포트 할당
	transPort, err := me.portAlloc.Allocate()
	if err != nil {
		me.stats.PortExhausted++
		fmt.Printf("  [ERROR]  %s (port exhausted)\n", pkt)
		return pkt, false
	}

	// 4. NAT 엔트리 생성
	entry := &NATEntry{
		OrigSrcIP:   pkt.SrcIP,
		OrigSrcPort: pkt.SrcPort,
		TransIP:     me.nodeIP,
		TransPort:   transPort,
		DstIP:       pkt.DstIP,
		DstPort:     pkt.DstPort,
		Proto:       pkt.Proto,
		Created:     time.Now(),
	}

	key := natKey(pkt.SrcIP, pkt.SrcPort, pkt.DstIP, pkt.DstPort, pkt.Proto)
	revKey := reverseNatKey(me.nodeIP, transPort, pkt.DstIP, pkt.DstPort, pkt.Proto)

	me.mu.Lock()
	me.natTable[key] = entry
	me.natTable[revKey] = entry
	me.mu.Unlock()

	// 5. 패킷 변환
	translated := Packet{
		SrcIP:   me.nodeIP,
		DstIP:   pkt.DstIP,
		SrcPort: transPort,
		DstPort: pkt.DstPort,
		Proto:   pkt.Proto,
	}

	me.stats.Masqueraded++
	fmt.Printf("  [SNAT]   %s => %s\n", pkt, translated)
	return translated, true
}

// ProcessIngress는 외부에서 들어오는 응답 패킷을 처리한다.
// 역방향 DNAT로 원래 Pod IP:Port로 복원한다.
func (me *MasqueradeEngine) ProcessIngress(pkt Packet) (Packet, bool) {
	revKey := reverseNatKey(pkt.DstIP, pkt.DstPort, pkt.SrcIP, pkt.SrcPort, pkt.Proto)

	me.mu.RLock()
	entry, ok := me.natTable[revKey]
	me.mu.RUnlock()

	if !ok {
		fmt.Printf("  [PASS]   %s (no NAT entry)\n", pkt)
		return pkt, false
	}

	// DNAT: 목적지를 원래 Pod IP:Port로 복원
	restored := Packet{
		SrcIP:   pkt.SrcIP,
		DstIP:   entry.OrigSrcIP,
		SrcPort: pkt.SrcPort,
		DstPort: entry.OrigSrcPort,
		Proto:   pkt.Proto,
	}

	me.stats.DeNATed++
	fmt.Printf("  [DNAT]   %s => %s\n", pkt, restored)
	return restored, true
}

// CleanupExpired는 오래된 NAT 엔트리를 정리한다.
func (me *MasqueradeEngine) CleanupExpired(maxAge time.Duration) int {
	me.mu.Lock()
	defer me.mu.Unlock()

	removed := 0
	now := time.Now()
	for key, entry := range me.natTable {
		if now.Sub(entry.Created) > maxAge {
			me.portAlloc.Release(entry.TransPort)
			delete(me.natTable, key)
			removed++
		}
	}
	return removed
}

func main() {
	fmt.Println("=== Cilium IP Masquerade (SNAT) 시뮬레이션 ===")
	fmt.Println()

	// --- 엔진 초기화 ---
	fmt.Println("[1] Masquerade 엔진 초기화")
	fmt.Println(strings.Repeat("-", 65))
	nonMasqCIDRs := []string{
		"10.0.0.0/8",      // 클러스터 내부
		"172.16.0.0/12",   // Pod CIDR
		"192.168.0.0/16",  // 서비스 CIDR
	}
	engine := NewMasqueradeEngine("10.0.1.10", nonMasqCIDRs)
	fmt.Printf("  Node IP: %s\n", engine.nodeIP)
	fmt.Println("  Non-masquerade CIDRs:")
	for _, cidr := range nonMasqCIDRs {
		fmt.Printf("    - %s\n", cidr)
	}
	fmt.Println()

	// --- Egress 패킷 처리 ---
	fmt.Println("[2] Egress 패킷 처리 (Pod → 외부)")
	fmt.Println(strings.Repeat("-", 65))

	egressPackets := []struct {
		pkt  Packet
		desc string
	}{
		{Packet{net.ParseIP("172.16.0.5"), net.ParseIP("8.8.8.8"), 45000, 53, "UDP"}, "Pod→DNS (외부)"},
		{Packet{net.ParseIP("172.16.0.5"), net.ParseIP("203.0.113.1"), 45001, 443, "TCP"}, "Pod→HTTPS (외부)"},
		{Packet{net.ParseIP("172.16.0.10"), net.ParseIP("93.184.216.34"), 45002, 80, "TCP"}, "Pod→HTTP (외부)"},
		{Packet{net.ParseIP("172.16.0.5"), net.ParseIP("10.0.2.20"), 45003, 8080, "TCP"}, "Pod→내부 (skip)"},
		{Packet{net.ParseIP("172.16.0.5"), net.ParseIP("192.168.1.100"), 45004, 3306, "TCP"}, "Pod→서비스 (skip)"},
		{Packet{net.ParseIP("172.16.0.15"), net.ParseIP("1.1.1.1"), 45005, 443, "TCP"}, "Pod→외부 CDN"},
	}

	translatedPackets := make([]Packet, 0)
	for _, tc := range egressPackets {
		fmt.Printf("\n  >> %s\n", tc.desc)
		translated, masqed := engine.ProcessEgress(tc.pkt)
		if masqed {
			translatedPackets = append(translatedPackets, translated)
		}
	}
	fmt.Println()

	// --- Ingress 응답 처리 ---
	fmt.Println("[3] Ingress 응답 패킷 처리 (외부 → Pod)")
	fmt.Println(strings.Repeat("-", 65))

	for _, tPkt := range translatedPackets {
		// 응답 패킷 생성 (src/dst 반전)
		response := Packet{
			SrcIP:   tPkt.DstIP,
			DstIP:   tPkt.SrcIP,
			SrcPort: tPkt.DstPort,
			DstPort: tPkt.SrcPort,
			Proto:   tPkt.Proto,
		}
		fmt.Printf("\n  >> 응답: %s\n", response)
		engine.ProcessIngress(response)
	}

	// 매칭되지 않는 패킷
	unknownPkt := Packet{net.ParseIP("1.2.3.4"), net.ParseIP("10.0.1.10"), 80, 65000, "TCP"}
	fmt.Printf("\n  >> 알 수 없는 패킷: %s\n", unknownPkt)
	engine.ProcessIngress(unknownPkt)
	fmt.Println()

	// --- NAT 테이블 ---
	fmt.Println("[4] NAT 테이블 현황")
	fmt.Println(strings.Repeat("-", 65))
	engine.mu.RLock()
	count := 0
	for key, entry := range engine.natTable {
		if !strings.HasPrefix(key, "rev:") {
			fmt.Printf("  %s\n", entry)
			count++
		}
	}
	engine.mu.RUnlock()
	fmt.Printf("  총 엔트리: %d\n", count)
	fmt.Println()

	// --- 대량 트래픽 시뮬레이션 ---
	fmt.Println("[5] 대량 트래픽 시뮬레이션 (100 패킷)")
	fmt.Println(strings.Repeat("-", 65))
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	podIPs := []string{"172.16.0.5", "172.16.0.10", "172.16.0.15", "172.16.0.20"}
	extIPs := []string{"8.8.8.8", "1.1.1.1", "203.0.113.1", "93.184.216.34"}
	for i := 0; i < 100; i++ {
		pkt := Packet{
			SrcIP:   net.ParseIP(podIPs[r.Intn(len(podIPs))]),
			DstIP:   net.ParseIP(extIPs[r.Intn(len(extIPs))]),
			SrcPort: uint16(30000 + r.Intn(30000)),
			DstPort: uint16([]int{80, 443, 53, 8080}[r.Intn(4)]),
			Proto:   "TCP",
		}
		engine.ProcessEgress(pkt)
	}
	fmt.Println()

	// --- 통계 ---
	fmt.Println("[6] Masquerade 통계")
	fmt.Println(strings.Repeat("-", 65))
	fmt.Printf("  Masqueraded:    %d\n", engine.stats.Masqueraded)
	fmt.Printf("  Skipped:        %d\n", engine.stats.Skipped)
	fmt.Printf("  De-NATed:       %d\n", engine.stats.DeNATed)
	fmt.Printf("  Port Exhausted: %d\n", engine.stats.PortExhausted)

	engine.mu.RLock()
	totalEntries := len(engine.natTable)
	engine.mu.RUnlock()
	fmt.Printf("  NAT Entries:    %d\n", totalEntries)

	// Cleanup
	removed := engine.CleanupExpired(0) // 즉시 만료로 테스트
	fmt.Printf("  Cleaned up:     %d entries\n", removed)
	fmt.Println()

	fmt.Println("=== 시뮬레이션 완료 ===")
}
