package main

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// =============================================================================
// Cilium XDP 초고속 패킷 처리 시뮬레이션
//
// 실제 소스: bpf/bpf_xdp.c, pkg/datapath/xdp/xdp.go
//
// 핵심 개념:
// 1. XDP 가속 모드 (native, best-effort, generic, disabled)
// 2. CIDR 프리필터 (Hash Map + LPM Trie)
// 3. NodePort 가속 (XDP 레벨 LB)
// 4. Enabler 패턴 (모드 충돌 해결)
// 5. XDP 판정 (PASS, DROP, TX, REDIRECT)
// =============================================================================

// --- XDP 판정 ---
type XDPVerdict int

const (
	XDP_PASS     XDPVerdict = iota // 커널 스택으로 전달
	XDP_DROP                       // 패킷 즉시 폐기
	XDP_TX                         // 동일 인터페이스로 반송
	XDP_REDIRECT                   // 다른 인터페이스로 리다이렉트
)

func (v XDPVerdict) String() string {
	switch v {
	case XDP_PASS:
		return "XDP_PASS"
	case XDP_DROP:
		return "XDP_DROP"
	case XDP_TX:
		return "XDP_TX"
	case XDP_REDIRECT:
		return "XDP_REDIRECT"
	}
	return "UNKNOWN"
}

// --- CIDR 프리필터 ---

// LPMTrieKey는 LPM 트라이 키 (Cilium: struct lpm_v4_key)
type LPMTrieKey struct {
	PrefixLen int
	Addr      uint32
}

// LPMTrie는 Longest Prefix Match 트라이 (Cilium: BPF_MAP_TYPE_LPM_TRIE)
type LPMTrie struct {
	mu      sync.RWMutex
	entries []LPMTrieKey
}

func NewLPMTrie() *LPMTrie {
	return &LPMTrie{}
}

func (t *LPMTrie) Insert(cidr string) error {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return err
	}

	ones, _ := ipNet.Mask.Size()
	ip := ipToUint32(ipNet.IP.To4())

	t.mu.Lock()
	t.entries = append(t.entries, LPMTrieKey{PrefixLen: ones, Addr: ip})
	t.mu.Unlock()
	return nil
}

func (t *LPMTrie) Lookup(ip uint32) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()

	for _, entry := range t.entries {
		mask := uint32(0xFFFFFFFF) << (32 - entry.PrefixLen)
		if (ip & mask) == (entry.Addr & mask) {
			return true
		}
	}
	return false
}

// HashFilter는 정확한 IP 매치 필터 (Cilium: BPF_MAP_TYPE_HASH)
type HashFilter struct {
	mu      sync.RWMutex
	blocked map[uint32]bool // IP → blocked
}

func NewHashFilter() *HashFilter {
	return &HashFilter{blocked: make(map[uint32]bool)}
}

func (h *HashFilter) Insert(ip string) {
	h.mu.Lock()
	h.blocked[ipToUint32(net.ParseIP(ip).To4())] = true
	h.mu.Unlock()
}

func (h *HashFilter) Lookup(ip uint32) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.blocked[ip]
}

// CIDRPrefilter는 XDP CIDR 프리필터 (Cilium: prefilter_v4)
type CIDRPrefilter struct {
	fixMap *HashFilter // 고정 /32 필터 (O(1))
	dynMap *LPMTrie    // 동적 CIDR 필터 (LPM)
}

func NewCIDRPrefilter() *CIDRPrefilter {
	return &CIDRPrefilter{
		fixMap: NewHashFilter(),
		dynMap: NewLPMTrie(),
	}
}

func (pf *CIDRPrefilter) Check(srcIP uint32) XDPVerdict {
	// 1. 동적 LPM Trie 검사 (CIDR 범위)
	if pf.dynMap.Lookup(srcIP) {
		return XDP_DROP
	}
	// 2. 고정 Hash Map 검사 (정확한 IP)
	if pf.fixMap.Lookup(srcIP) {
		return XDP_DROP
	}
	return XDP_PASS
}

// --- NodePort LB ---

// NodePortService는 NodePort 서비스
type NodePortService struct {
	Port     uint16
	Backends []Backend
}

// Backend은 서비스 백엔드
type Backend struct {
	IP   net.IP
	Port uint16
}

// NodePortLB는 XDP 레벨 NodePort 로드밸런서
type NodePortLB struct {
	mu       sync.RWMutex
	services map[uint16]*NodePortService // port → service
}

func NewNodePortLB() *NodePortLB {
	return &NodePortLB{services: make(map[uint16]*NodePortService)}
}

func (lb *NodePortLB) AddService(svc *NodePortService) {
	lb.mu.Lock()
	lb.services[svc.Port] = svc
	lb.mu.Unlock()
}

func (lb *NodePortLB) Lookup(dstPort uint16) (*Backend, bool) {
	lb.mu.RLock()
	defer lb.mu.RUnlock()

	svc, ok := lb.services[dstPort]
	if !ok || len(svc.Backends) == 0 {
		return nil, false
	}

	// 간단한 랜덤 선택 (실제: Maglev 해시)
	idx := rand.Intn(len(svc.Backends))
	return &svc.Backends[idx], true
}

// --- XDP 프로그램 시뮬레이션 ---

// Packet은 시뮬레이션 패킷
type Packet struct {
	SrcIP   net.IP
	DstIP   net.IP
	SrcPort uint16
	DstPort uint16
	Proto   uint8 // 6=TCP, 17=UDP
}

func (p Packet) String() string {
	proto := "TCP"
	if p.Proto == 17 {
		proto = "UDP"
	}
	return fmt.Sprintf("%s:%d → %s:%d (%s)",
		p.SrcIP, p.SrcPort, p.DstIP, p.DstPort, proto)
}

// XDPProgram은 XDP BPF 프로그램 시뮬레이션 (Cilium: cil_xdp_entry)
type XDPProgram struct {
	prefilter *CIDRPrefilter
	lb        *NodePortLB
	enabled   bool

	// 통계
	totalPackets atomic.Int64
	passPackets  atomic.Int64
	dropPackets  atomic.Int64
	txPackets    atomic.Int64
}

func NewXDPProgram(prefilter *CIDRPrefilter, lb *NodePortLB) *XDPProgram {
	return &XDPProgram{
		prefilter: prefilter,
		lb:        lb,
		enabled:   true,
	}
}

// ProcessPacket은 cil_xdp_entry() 시뮬레이션
func (xdp *XDPProgram) ProcessPacket(pkt Packet) (XDPVerdict, string) {
	xdp.totalPackets.Add(1)

	if !xdp.enabled {
		xdp.passPackets.Add(1)
		return XDP_PASS, "XDP disabled"
	}

	// 1. 프리필터 검사 (prefilter_v4)
	srcIP := ipToUint32(pkt.SrcIP.To4())
	verdict := xdp.prefilter.Check(srcIP)
	if verdict == XDP_DROP {
		xdp.dropPackets.Add(1)
		return XDP_DROP, "CIDR prefilter blocked"
	}

	// 2. NodePort LB 검사 (check_v4_lb → nodeport_lb4)
	backend, found := xdp.lb.Lookup(pkt.DstPort)
	if found {
		xdp.txPackets.Add(1)
		return XDP_TX, fmt.Sprintf("NodePort LB → %s:%d", backend.IP, backend.Port)
	}

	// 3. 매칭 없으면 커널 스택으로 전달
	xdp.passPackets.Add(1)
	return XDP_PASS, "passed to kernel stack"
}

// --- Enabler 패턴 ---

// AccelerationMode (Cilium: pkg/datapath/xdp/xdp.go:16)
type AccelerationMode string

const (
	ModeNative     AccelerationMode = "native"
	ModeBestEffort AccelerationMode = "best-effort"
	ModeGeneric    AccelerationMode = "testing-only"
	ModeDisabled   AccelerationMode = "disabled"
)

type XDPEnabler struct {
	Feature string
	Mode    AccelerationMode
}

// ResolveMode는 여러 Enabler의 모드 충돌을 해결 (Cilium: newConfig)
func ResolveMode(enablers []XDPEnabler) (AccelerationMode, error) {
	result := ModeDisabled

	for _, e := range enablers {
		if e.Mode == ModeDisabled {
			continue
		}

		// Native이 BestEffort보다 우선
		if e.Mode == ModeBestEffort && result == ModeNative {
			continue
		}
		if result == ModeBestEffort && e.Mode == ModeNative {
			result = e.Mode
			continue
		}

		// 충돌 검사
		if result != ModeDisabled && result != e.Mode {
			return ModeDisabled, fmt.Errorf(
				"XDP mode conflict: %s requests %s but %s already set",
				e.Feature, e.Mode, result)
		}

		result = e.Mode
	}

	return result, nil
}

// --- 헬퍼 ---

func ipToUint32(ip net.IP) uint32 {
	if ip == nil {
		return 0
	}
	return binary.BigEndian.Uint32(ip.To4())
}

func main() {
	fmt.Println("=" + strings.Repeat("=", 70))
	fmt.Println(" Cilium XDP 초고속 패킷 처리 시뮬레이션")
	fmt.Println(" 소스: bpf/bpf_xdp.c, pkg/datapath/xdp/xdp.go")
	fmt.Println("=" + strings.Repeat("=", 70))

	// --- 1. CIDR 프리필터 ---
	fmt.Println("\n[1] CIDR 프리필터 (Hash + LPM Trie)")
	fmt.Println(strings.Repeat("-", 50))

	prefilter := NewCIDRPrefilter()

	// 고정 IP 차단 (/32)
	prefilter.fixMap.Insert("10.0.0.100")
	prefilter.fixMap.Insert("192.168.1.99")

	// CIDR 범위 차단
	prefilter.dynMap.Insert("172.16.0.0/12")
	prefilter.dynMap.Insert("203.0.113.0/24")

	testIPs := []string{
		"10.0.0.100",   // 고정 차단
		"10.0.0.101",   // 허용
		"172.16.5.1",   // CIDR 차단 (172.16.0.0/12)
		"172.32.1.1",   // 허용 (범위 밖)
		"203.0.113.42", // CIDR 차단
		"8.8.8.8",      // 허용
	}

	for _, ipStr := range testIPs {
		ip := ipToUint32(net.ParseIP(ipStr).To4())
		verdict := prefilter.Check(ip)
		fmt.Printf("  %-18s → %s\n", ipStr, verdict)
	}

	// --- 2. NodePort LB ---
	fmt.Println("\n[2] NodePort 가속 (XDP 레벨 LB)")
	fmt.Println(strings.Repeat("-", 50))

	lb := NewNodePortLB()
	lb.AddService(&NodePortService{
		Port: 30080,
		Backends: []Backend{
			{IP: net.ParseIP("10.244.1.5"), Port: 8080},
			{IP: net.ParseIP("10.244.2.10"), Port: 8080},
			{IP: net.ParseIP("10.244.3.15"), Port: 8080},
		},
	})
	lb.AddService(&NodePortService{
		Port: 30443,
		Backends: []Backend{
			{IP: net.ParseIP("10.244.1.20"), Port: 443},
			{IP: net.ParseIP("10.244.2.25"), Port: 443},
		},
	})

	xdp := NewXDPProgram(prefilter, lb)

	// 테스트 패킷들
	packets := []Packet{
		{SrcIP: net.ParseIP("1.2.3.4"), DstIP: net.ParseIP("192.168.1.1"), SrcPort: 50000, DstPort: 30080, Proto: 6},
		{SrcIP: net.ParseIP("5.6.7.8"), DstIP: net.ParseIP("192.168.1.1"), SrcPort: 50001, DstPort: 30443, Proto: 6},
		{SrcIP: net.ParseIP("203.0.113.42"), DstIP: net.ParseIP("192.168.1.1"), SrcPort: 50002, DstPort: 30080, Proto: 6},
		{SrcIP: net.ParseIP("9.9.9.9"), DstIP: net.ParseIP("192.168.1.1"), SrcPort: 50003, DstPort: 80, Proto: 6},
		{SrcIP: net.ParseIP("172.16.100.1"), DstIP: net.ParseIP("192.168.1.1"), SrcPort: 50004, DstPort: 30080, Proto: 6},
	}

	for _, pkt := range packets {
		verdict, reason := xdp.ProcessPacket(pkt)
		fmt.Printf("  %s\n    → %s (%s)\n", pkt, verdict, reason)
	}

	fmt.Printf("\n  통계: Total=%d, PASS=%d, DROP=%d, TX=%d\n",
		xdp.totalPackets.Load(), xdp.passPackets.Load(),
		xdp.dropPackets.Load(), xdp.txPackets.Load())

	// --- 3. Enabler 패턴 ---
	fmt.Println("\n[3] Enabler 패턴 (모드 충돌 해결)")
	fmt.Println(strings.Repeat("-", 50))

	// 시나리오 1: 정상 (같은 모드)
	mode, err := ResolveMode([]XDPEnabler{
		{Feature: "NodePort", Mode: ModeNative},
		{Feature: "Prefilter", Mode: ModeNative},
	})
	fmt.Printf("  시나리오 1 (NodePort=native, Prefilter=native): %s (err=%v)\n", mode, err)

	// 시나리오 2: Native이 BestEffort보다 우선
	mode, err = ResolveMode([]XDPEnabler{
		{Feature: "NodePort", Mode: ModeBestEffort},
		{Feature: "DSR", Mode: ModeNative},
	})
	fmt.Printf("  시나리오 2 (NodePort=best-effort, DSR=native): %s (err=%v)\n", mode, err)

	// 시나리오 3: 충돌
	mode, err = ResolveMode([]XDPEnabler{
		{Feature: "NodePort", Mode: ModeNative},
		{Feature: "Testing", Mode: ModeGeneric},
	})
	fmt.Printf("  시나리오 3 (NodePort=native, Testing=generic): %s (err=%v)\n", mode, err)

	// 시나리오 4: Disabled는 무시
	mode, err = ResolveMode([]XDPEnabler{
		{Feature: "WireGuard", Mode: ModeDisabled},
		{Feature: "NodePort", Mode: ModeNative},
	})
	fmt.Printf("  시나리오 4 (WireGuard=disabled, NodePort=native): %s (err=%v)\n", mode, err)

	// --- 4. 처리량 벤치마크 ---
	fmt.Println("\n[4] 처리량 벤치마크 (XDP vs 커널스택)")
	fmt.Println(strings.Repeat("-", 50))

	benchXDP := NewXDPProgram(NewCIDRPrefilter(), NewNodePortLB())

	iterations := 1000000
	start := time.Now()
	for i := 0; i < iterations; i++ {
		pkt := Packet{
			SrcIP:   net.IPv4(byte(rand.Intn(256)), byte(rand.Intn(256)), byte(rand.Intn(256)), byte(rand.Intn(256))),
			DstIP:   net.IPv4(192, 168, 1, 1),
			SrcPort: uint16(rand.Intn(65535)),
			DstPort: uint16(rand.Intn(65535)),
			Proto:   6,
		}
		benchXDP.ProcessPacket(pkt)
	}
	xdpDuration := time.Since(start)
	xdpPPS := float64(iterations) / xdpDuration.Seconds()

	fmt.Printf("  XDP 시뮬레이션: %d 패킷 / %v = %.0f pps\n", iterations, xdpDuration, xdpPPS)
	fmt.Printf("  (실제 XDP native: ~14M pps, 이 시뮬레이션은 Go 오버헤드 포함)\n")

	// --- 요약 ---
	fmt.Println("\n" + strings.Repeat("=", 71))
	fmt.Println(" 시뮬레이션 완료")
	fmt.Println()
	fmt.Println(" XDP 핵심 동작:")
	fmt.Println("   1. NIC 드라이버 레벨에서 패킷 처리 (sk_buff 할당 전)")
	fmt.Println("   2. CIDR 프리필터: Hash(O(1)) + LPM Trie(CIDR 매치)")
	fmt.Println("   3. NodePort 가속: XDP에서 DNAT → XDP_TX/REDIRECT")
	fmt.Println("   4. Enabler 패턴으로 모드 충돌 해결")
	fmt.Println("   5. XDP_PASS 시 TC(bpf_host.c)로 메타데이터 전달")
	fmt.Println(strings.Repeat("=", 71))
}
