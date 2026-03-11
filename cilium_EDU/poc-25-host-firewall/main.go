package main

import (
	"fmt"
	"math/rand"
	"net"
	"strings"
	"time"
)

// =============================================================================
// Cilium Host Firewall 시뮬레이션
// =============================================================================
//
// Cilium의 Host Firewall은 eBPF 프로그램을 통해 호스트 인터페이스에서
// 패킷을 필터링한다. 이 PoC는 핵심 동작을 시뮬레이션한다:
//   - PolicyMap: BPF 맵 기반 정책 룩업 시뮬레이션
//   - 방향별 필터링: Ingress/Egress 구분
//   - CIDR 매칭: IP 접두사 기반 규칙 매칭
//   - Port 매칭: L4 포트 기반 필터링
//   - Connection Tracking: conntrack 테이블로 상태 기반 필터링
//
// 실제 Cilium 코드 참조:
//   - bpf/bpf_host.c: 호스트 인터페이스 eBPF 프로그램
//   - pkg/policy/: 정책 엔진
//   - pkg/maps/policymap/: BPF 정책 맵
// =============================================================================

// --- 프로토콜 / 방향 상수 ---

type Protocol uint8

const (
	TCP Protocol = 6
	UDP Protocol = 17
)

func (p Protocol) String() string {
	switch p {
	case TCP:
		return "TCP"
	case UDP:
		return "UDP"
	default:
		return fmt.Sprintf("PROTO(%d)", p)
	}
}

type Direction int

const (
	Ingress Direction = iota
	Egress
)

func (d Direction) String() string {
	if d == Ingress {
		return "INGRESS"
	}
	return "EGRESS"
}

type Verdict int

const (
	VerdictAllow Verdict = iota
	VerdictDeny
	VerdictDrop
)

func (v Verdict) String() string {
	switch v {
	case VerdictAllow:
		return "ALLOW"
	case VerdictDeny:
		return "DENY"
	default:
		return "DROP"
	}
}

// --- 패킷 구조 ---

// Packet은 네트워크 패킷을 표현한다.
type Packet struct {
	SrcIP    net.IP
	DstIP    net.IP
	SrcPort  uint16
	DstPort  uint16
	Protocol Protocol
}

func (p Packet) String() string {
	return fmt.Sprintf("%s:%d -> %s:%d [%s]",
		p.SrcIP, p.SrcPort, p.DstIP, p.DstPort, p.Protocol)
}

// --- CIDR 매칭 ---

// CIDRRule은 IP 접두사 기반 매칭 규칙이다.
// Cilium은 longest-prefix match를 BPF LPM trie 맵으로 구현한다.
type CIDRRule struct {
	Network *net.IPNet
}

func parseCIDR(cidr string) CIDRRule {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		panic(fmt.Sprintf("invalid CIDR: %s", cidr))
	}
	return CIDRRule{Network: network}
}

func (c CIDRRule) Match(ip net.IP) bool {
	return c.Network.Contains(ip)
}

// --- 정책 규칙 ---

// PolicyRule은 하나의 방화벽 규칙이다.
// Cilium에서는 이것이 BPF 맵 엔트리로 변환된다.
type PolicyRule struct {
	Name      string
	Direction Direction
	SrcCIDR   *CIDRRule
	DstCIDR   *CIDRRule
	DstPort   uint16 // 0이면 모든 포트
	Protocol  Protocol
	Verdict   Verdict
	Priority  int // 낮을수록 우선
}

func (r PolicyRule) String() string {
	src := "any"
	if r.SrcCIDR != nil {
		src = r.SrcCIDR.Network.String()
	}
	dst := "any"
	if r.DstCIDR != nil {
		dst = r.DstCIDR.Network.String()
	}
	port := "any"
	if r.DstPort != 0 {
		port = fmt.Sprintf("%d", r.DstPort)
	}
	return fmt.Sprintf("[%s] %s src=%s dst=%s port=%s proto=%s -> %s",
		r.Direction, r.Name, src, dst, port, r.Protocol, r.Verdict)
}

// Match는 패킷이 이 규칙에 매치되는지 검사한다.
func (r PolicyRule) Match(pkt Packet, dir Direction) bool {
	if r.Direction != dir {
		return false
	}
	if r.Protocol != pkt.Protocol {
		return false
	}
	if r.SrcCIDR != nil && !r.SrcCIDR.Match(pkt.SrcIP) {
		return false
	}
	if r.DstCIDR != nil && !r.DstCIDR.Match(pkt.DstIP) {
		return false
	}
	if r.DstPort != 0 && r.DstPort != pkt.DstPort {
		return false
	}
	return true
}

// --- PolicyMap (BPF 맵 시뮬레이션) ---

// PolicyMap은 Cilium의 BPF 정책 맵을 시뮬레이션한다.
// 실제로는 bpf_map_lookup_elem()으로 O(1) 룩업하지만,
// 여기서는 규칙 리스트를 우선순위 순으로 순회한다.
type PolicyMap struct {
	rules []PolicyRule
}

func NewPolicyMap() *PolicyMap {
	return &PolicyMap{}
}

func (pm *PolicyMap) AddRule(rule PolicyRule) {
	// 우선순위 순으로 삽입
	inserted := false
	for i, r := range pm.rules {
		if rule.Priority < r.Priority {
			pm.rules = append(pm.rules[:i+1], pm.rules[i:]...)
			pm.rules[i] = rule
			inserted = true
			break
		}
	}
	if !inserted {
		pm.rules = append(pm.rules, rule)
	}
}

// Lookup은 패킷에 대한 판정을 반환한다.
func (pm *PolicyMap) Lookup(pkt Packet, dir Direction) (Verdict, string) {
	for _, rule := range pm.rules {
		if rule.Match(pkt, dir) {
			return rule.Verdict, rule.Name
		}
	}
	// 기본 정책: DROP (Host Firewall 모드에서는 기본 거부)
	return VerdictDrop, "default-deny"
}

// --- Connection Tracking ---

// ConnTrackEntry는 연결 추적 엔트리이다.
// Cilium은 CT 맵(bpf/lib/conntrack.h)으로 상태 기반 필터링을 한다.
type ConnTrackEntry struct {
	SrcIP    string
	DstIP    string
	SrcPort  uint16
	DstPort  uint16
	Protocol Protocol
	State    string
	Expires  time.Time
}

type ConnTrack struct {
	entries map[string]*ConnTrackEntry
	timeout time.Duration
}

func NewConnTrack(timeout time.Duration) *ConnTrack {
	return &ConnTrack{
		entries: make(map[string]*ConnTrackEntry),
		timeout: timeout,
	}
}

func connTrackKey(srcIP, dstIP string, srcPort, dstPort uint16, proto Protocol) string {
	return fmt.Sprintf("%s:%d-%s:%d-%d", srcIP, srcPort, dstIP, dstPort, proto)
}

// Track은 허용된 패킷의 연결을 기록한다.
func (ct *ConnTrack) Track(pkt Packet) {
	key := connTrackKey(pkt.SrcIP.String(), pkt.DstIP.String(), pkt.SrcPort, pkt.DstPort, pkt.Protocol)
	ct.entries[key] = &ConnTrackEntry{
		SrcIP:    pkt.SrcIP.String(),
		DstIP:    pkt.DstIP.String(),
		SrcPort:  pkt.SrcPort,
		DstPort:  pkt.DstPort,
		Protocol: pkt.Protocol,
		State:    "ESTABLISHED",
		Expires:  time.Now().Add(ct.timeout),
	}
}

// IsEstablished는 역방향 패킷이 기존 연결의 응답인지 검사한다.
func (ct *ConnTrack) IsEstablished(pkt Packet) bool {
	// 역방향 키로 검색
	reverseKey := connTrackKey(pkt.DstIP.String(), pkt.SrcIP.String(), pkt.DstPort, pkt.SrcPort, pkt.Protocol)
	entry, ok := ct.entries[reverseKey]
	if !ok {
		return false
	}
	return time.Now().Before(entry.Expires)
}

// GC는 만료된 엔트리를 정리한다 (Cilium CT GC 시뮬레이션).
func (ct *ConnTrack) GC() int {
	removed := 0
	now := time.Now()
	for key, entry := range ct.entries {
		if now.After(entry.Expires) {
			delete(ct.entries, key)
			removed++
		}
	}
	return removed
}

// --- Host Firewall Engine ---

// HostFirewall은 Cilium Host Firewall의 전체 파이프라인을 시뮬레이션한다.
type HostFirewall struct {
	policyMap *PolicyMap
	connTrack *ConnTrack
	stats     FirewallStats
	hostIP    net.IP
}

type FirewallStats struct {
	IngressAllowed int
	IngressDenied  int
	EgressAllowed  int
	EgressDenied   int
	CTHits         int
}

func NewHostFirewall(hostIP string) *HostFirewall {
	return &HostFirewall{
		policyMap: NewPolicyMap(),
		connTrack: NewConnTrack(30 * time.Second),
		hostIP:    net.ParseIP(hostIP),
	}
}

// ProcessPacket은 eBPF의 tc_ingress/tc_egress 훅을 시뮬레이션한다.
func (hf *HostFirewall) ProcessPacket(pkt Packet, dir Direction) Verdict {
	// 1단계: Connection Tracking 검사 (established 연결은 바로 허용)
	if hf.connTrack.IsEstablished(pkt) {
		hf.stats.CTHits++
		if dir == Ingress {
			hf.stats.IngressAllowed++
		} else {
			hf.stats.EgressAllowed++
		}
		fmt.Printf("  [CT-HIT]  %s %s -> ALLOW (established)\n", dir, pkt)
		return VerdictAllow
	}

	// 2단계: PolicyMap 룩업
	verdict, ruleName := hf.policyMap.Lookup(pkt, dir)

	// 3단계: 허용된 패킷은 CT 엔트리 생성
	if verdict == VerdictAllow {
		hf.connTrack.Track(pkt)
	}

	// 4단계: 통계 업데이트
	switch dir {
	case Ingress:
		if verdict == VerdictAllow {
			hf.stats.IngressAllowed++
		} else {
			hf.stats.IngressDenied++
		}
	case Egress:
		if verdict == VerdictAllow {
			hf.stats.EgressAllowed++
		} else {
			hf.stats.EgressDenied++
		}
	}

	fmt.Printf("  [POLICY] %s %s -> %s (rule: %s)\n", dir, pkt, verdict, ruleName)
	return verdict
}

func main() {
	fmt.Println("=== Cilium Host Firewall 시뮬레이션 ===")
	fmt.Println()

	// --- 방화벽 초기화 ---
	hf := NewHostFirewall("10.0.0.1")

	// --- 정책 규칙 등록 ---
	fmt.Println("[1] 정책 규칙 등록")
	fmt.Println(strings.Repeat("-", 70))

	sshCIDR := parseCIDR("10.0.0.0/24")
	anyCIDR := parseCIDR("0.0.0.0/0")
	externalCIDR := parseCIDR("192.168.1.0/24")

	rules := []PolicyRule{
		{Name: "allow-ssh-internal", Direction: Ingress, SrcCIDR: &sshCIDR, DstPort: 22, Protocol: TCP, Verdict: VerdictAllow, Priority: 10},
		{Name: "deny-ssh-external", Direction: Ingress, DstPort: 22, Protocol: TCP, Verdict: VerdictDeny, Priority: 20},
		{Name: "allow-http", Direction: Ingress, DstPort: 80, Protocol: TCP, Verdict: VerdictAllow, Priority: 30},
		{Name: "allow-https", Direction: Ingress, DstPort: 443, Protocol: TCP, Verdict: VerdictAllow, Priority: 30},
		{Name: "allow-dns-egress", Direction: Egress, DstPort: 53, Protocol: UDP, Verdict: VerdictAllow, Priority: 10},
		{Name: "allow-egress-internal", Direction: Egress, DstCIDR: &sshCIDR, Protocol: TCP, Verdict: VerdictAllow, Priority: 20},
		{Name: "deny-egress-external-db", Direction: Egress, DstCIDR: &externalCIDR, DstPort: 3306, Protocol: TCP, Verdict: VerdictDeny, Priority: 15},
		{Name: "allow-egress-http", Direction: Egress, DstPort: 80, Protocol: TCP, Verdict: VerdictAllow, Priority: 30},
		{Name: "allow-egress-https", Direction: Egress, DstPort: 443, Protocol: TCP, Verdict: VerdictAllow, Priority: 30},
		{Name: "deny-all-ingress", Direction: Ingress, SrcCIDR: &anyCIDR, Protocol: TCP, Verdict: VerdictDrop, Priority: 100},
	}

	for _, rule := range rules {
		hf.policyMap.AddRule(rule)
		fmt.Printf("  + %s\n", rule)
	}
	fmt.Println()

	// --- 패킷 처리 시뮬레이션 ---
	fmt.Println("[2] 패킷 필터링 시뮬레이션")
	fmt.Println(strings.Repeat("-", 70))

	testPackets := []struct {
		pkt Packet
		dir Direction
		desc string
	}{
		{Packet{net.ParseIP("10.0.0.5"), net.ParseIP("10.0.0.1"), 54321, 22, TCP}, Ingress, "내부 SSH 접속"},
		{Packet{net.ParseIP("203.0.113.1"), net.ParseIP("10.0.0.1"), 54322, 22, TCP}, Ingress, "외부 SSH 접속 시도"},
		{Packet{net.ParseIP("203.0.113.2"), net.ParseIP("10.0.0.1"), 54323, 80, TCP}, Ingress, "외부 HTTP 접속"},
		{Packet{net.ParseIP("203.0.113.3"), net.ParseIP("10.0.0.1"), 54324, 8080, TCP}, Ingress, "외부 비허용 포트"},
		{Packet{net.ParseIP("10.0.0.1"), net.ParseIP("8.8.8.8"), 12345, 53, UDP}, Egress, "DNS 쿼리"},
		{Packet{net.ParseIP("10.0.0.1"), net.ParseIP("192.168.1.10"), 12346, 3306, TCP}, Egress, "외부 DB 접속 차단"},
		{Packet{net.ParseIP("10.0.0.1"), net.ParseIP("10.0.0.5"), 12347, 8080, TCP}, Egress, "내부 통신"},
	}

	for _, tc := range testPackets {
		fmt.Printf("\n  >> %s\n", tc.desc)
		hf.ProcessPacket(tc.pkt, tc.dir)
	}
	fmt.Println()

	// --- Connection Tracking 시뮬레이션 ---
	fmt.Println("[3] Connection Tracking (응답 패킷)")
	fmt.Println(strings.Repeat("-", 70))

	// HTTP 요청의 응답 패킷 (역방향)
	responsePkt := Packet{net.ParseIP("10.0.0.1"), net.ParseIP("203.0.113.2"), 80, 54323, TCP}
	fmt.Printf("\n  >> HTTP 응답 패킷 (established 연결)\n")
	hf.ProcessPacket(responsePkt, Egress)

	// DNS 응답
	dnsResponse := Packet{net.ParseIP("8.8.8.8"), net.ParseIP("10.0.0.1"), 53, 12345, UDP}
	fmt.Printf("\n  >> DNS 응답 패킷 (established 연결)\n")
	hf.ProcessPacket(dnsResponse, Ingress)
	fmt.Println()

	// --- 트래픽 시뮬레이션 ---
	fmt.Println("[4] 무작위 트래픽 시뮬레이션 (20 패킷)")
	fmt.Println(strings.Repeat("-", 70))

	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	ports := []uint16{22, 80, 443, 3306, 8080, 53}
	srcIPs := []string{"10.0.0.5", "10.0.0.10", "203.0.113.1", "192.168.1.5"}
	dstIPs := []string{"10.0.0.1", "10.0.0.2", "192.168.1.10", "8.8.8.8"}

	for i := 0; i < 20; i++ {
		srcIP := srcIPs[r.Intn(len(srcIPs))]
		dstIP := dstIPs[r.Intn(len(dstIPs))]
		port := ports[r.Intn(len(ports))]
		proto := TCP
		if port == 53 {
			proto = UDP
		}
		dir := Ingress
		if r.Intn(2) == 1 {
			dir = Egress
		}
		pkt := Packet{net.ParseIP(srcIP), net.ParseIP(dstIP), uint16(r.Intn(65535)), port, proto}
		hf.ProcessPacket(pkt, dir)
	}
	fmt.Println()

	// --- 통계 출력 ---
	fmt.Println("[5] 방화벽 통계")
	fmt.Println(strings.Repeat("-", 70))
	fmt.Printf("  Ingress Allowed: %d\n", hf.stats.IngressAllowed)
	fmt.Printf("  Ingress Denied:  %d\n", hf.stats.IngressDenied)
	fmt.Printf("  Egress Allowed:  %d\n", hf.stats.EgressAllowed)
	fmt.Printf("  Egress Denied:   %d\n", hf.stats.EgressDenied)
	fmt.Printf("  CT Hits:         %d\n", hf.stats.CTHits)
	fmt.Printf("  CT Entries:      %d\n", len(hf.connTrack.entries))

	// CT GC 실행
	removed := hf.connTrack.GC()
	fmt.Printf("  CT GC Removed:   %d\n", removed)
	fmt.Println()
	fmt.Println("=== 시뮬레이션 완료 ===")
}
