// Cilium BPF 데이터패스의 패킷 처리 파이프라인 시뮬레이션
//
// 실제 BPF 코드 (C):
//   bpf/bpf_lxc.c → cil_from_container()
//   bpf/lib/conntrack.h → CT 조회/생성
//   bpf/lib/policy.h → 정책 조회
//
// 실행: go run main.go
package main

import (
	"fmt"
	"strings"
)

// -----------------------------------------------------------
// CT (Connection Tracking) 맵 — bpf/lib/conntrack.h 재현
// 실제: pkg/maps/ctmap/ctmap.go
// -----------------------------------------------------------
type CTKey struct {
	SrcIP    string
	DstIP    string
	SrcPort  uint16
	DstPort  uint16
	Protocol string
}

type CTEntry struct {
	Key        CTKey
	RxPackets  uint64
	TxPackets  uint64
	Flags      string // "SYN_SEEN", "ESTABLISHED"
	SrcIdentity uint32
}

type CTMap struct {
	entries map[CTKey]*CTEntry
}

func newCTMap() *CTMap {
	return &CTMap{entries: make(map[CTKey]*CTEntry)}
}

func (m *CTMap) Lookup(key CTKey) (*CTEntry, bool) {
	e, ok := m.entries[key]
	return e, ok
}

func (m *CTMap) Create(key CTKey, srcIdentity uint32) *CTEntry {
	entry := &CTEntry{
		Key:         key,
		RxPackets:   1,
		Flags:       "SYN_SEEN",
		SrcIdentity: srcIdentity,
	}
	m.entries[key] = entry
	return entry
}

// -----------------------------------------------------------
// Policy 맵 — bpf/lib/policy.h 재현
// 실제: pkg/maps/policymap/policymap.go
// -----------------------------------------------------------
type PolicyKey struct {
	Identity uint32
	Port     uint16
	Protocol string
}

type PolicyMap struct {
	allowed map[PolicyKey]bool
}

func newPolicyMap() *PolicyMap {
	return &PolicyMap{allowed: make(map[PolicyKey]bool)}
}

func (m *PolicyMap) Allow(identity uint32, port uint16, proto string) {
	m.allowed[PolicyKey{identity, port, proto}] = true
}

func (m *PolicyMap) Lookup(identity uint32, port uint16, proto string) bool {
	return m.allowed[PolicyKey{identity, port, proto}]
}

// -----------------------------------------------------------
// 패킷 구조체
// -----------------------------------------------------------
type Packet struct {
	SrcIP    string
	DstIP    string
	SrcPort  uint16
	DstPort  uint16
	Protocol string
	SrcIdentity uint32
	Label    string // 디버깅용 라벨
}

// -----------------------------------------------------------
// BPF 파이프라인 — cil_from_container() 재현
// -----------------------------------------------------------
type Pipeline struct {
	ctMap     *CTMap
	policyMap *PolicyMap
	step      int
}

func newPipeline(ct *CTMap, pol *PolicyMap) *Pipeline {
	return &Pipeline{ctMap: ct, policyMap: pol}
}

func (p *Pipeline) log(indent int, format string, args ...interface{}) {
	prefix := strings.Repeat("  ", indent)
	fmt.Printf("%s%s\n", prefix, fmt.Sprintf(format, args...))
}

func (p *Pipeline) Process(pkt Packet) string {
	p.step++
	fmt.Printf("\n── 패킷 #%d: %s ─────────────────────────────────\n", p.step, pkt.Label)
	p.log(1, "src: %s:%d (Identity %d)", pkt.SrcIP, pkt.SrcPort, pkt.SrcIdentity)
	p.log(1, "dst: %s:%d %s", pkt.DstIP, pkt.DstPort, pkt.Protocol)

	// Step 1: CT 조회 — conntrack.h의 ct_lookup4()
	ctKey := CTKey{
		SrcIP: pkt.SrcIP, DstIP: pkt.DstIP,
		SrcPort: pkt.SrcPort, DstPort: pkt.DstPort,
		Protocol: pkt.Protocol,
	}

	p.log(1, "")
	p.log(1, "[CT 조회] conntrack.h → ct_lookup4()")
	if entry, ok := p.ctMap.Lookup(ctKey); ok {
		// CT HIT — 기존 연결, 정책 평가 생략
		entry.RxPackets++
		entry.Flags = "ESTABLISHED"
		p.log(2, "결과: HIT (기존 연결, packets=%d)", entry.RxPackets)
		p.log(2, "→ 정책 평가 생략, 바로 전달")
		p.log(1, "[결과] FORWARDED (CT 캐시)")
		return "FORWARDED"
	}
	p.log(2, "결과: MISS (새 연결)")

	// Step 2: Policy 조회 — policy.h의 policy_can_egress4()
	p.log(1, "")
	p.log(1, "[Policy 조회] policy.h → policy_can_egress4()")
	p.log(2, "조회: Identity %d → port %d/%s", pkt.SrcIdentity, pkt.DstPort, pkt.Protocol)

	if !p.policyMap.Lookup(pkt.SrcIdentity, pkt.DstPort, pkt.Protocol) {
		// DENY — 패킷 드롭
		p.log(2, "결과: DENY")
		p.log(1, "")
		p.log(1, "[DROP] 정책 위반 — 패킷 드롭")
		p.log(2, "Hubble에 DROPPED 이벤트 기록 (drop_reason=POLICY_DENIED)")
		return "DROPPED"
	}
	p.log(2, "결과: ALLOW")

	// Step 3: CT 엔트리 생성 — conntrack.h의 ct_create4()
	p.log(1, "")
	p.log(1, "[CT 생성] conntrack.h → ct_create4()")
	p.ctMap.Create(ctKey, pkt.SrcIdentity)
	p.log(2, "새 CT 엔트리 생성 (flags=SYN_SEEN)")
	p.log(2, "이후 같은 5-tuple 패킷은 CT HIT로 빠르게 처리됨")

	// Step 4: 패킷 전달 — tail call로 to-container
	p.log(1, "")
	p.log(1, "[전달] tail_call → cil_to_container()")
	p.log(2, "Hubble에 FORWARDED 이벤트 기록")
	p.log(1, "[결과] FORWARDED")
	return "FORWARDED"
}

func main() {
	fmt.Println("=== Cilium BPF 패킷 처리 파이프라인 시뮬레이터 ===")
	fmt.Println()
	fmt.Println("실제 코드 위치:")
	fmt.Println("  bpf/bpf_lxc.c         → cil_from_container()")
	fmt.Println("  bpf/lib/conntrack.h   → ct_lookup4(), ct_create4()")
	fmt.Println("  bpf/lib/policy.h      → policy_can_egress4()")

	ct := newCTMap()
	pol := newPolicyMap()

	// 정책: frontend(48312)는 backend 80/TCP 허용
	pol.Allow(48312, 80, "TCP")
	// 정책: backend(48313)는 redis 6379/TCP 허용
	pol.Allow(48313, 6379, "TCP")

	pipeline := newPipeline(ct, pol)

	// 패킷 1: frontend → backend:80 (새 연결, ALLOW)
	pipeline.Process(Packet{
		SrcIP: "10.0.1.5", DstIP: "10.0.1.10",
		SrcPort: 34567, DstPort: 80, Protocol: "TCP",
		SrcIdentity: 48312, Label: "frontend → backend:80 (첫 패킷)",
	})

	// 패킷 2: 같은 연결의 두 번째 패킷 (CT HIT)
	pipeline.Process(Packet{
		SrcIP: "10.0.1.5", DstIP: "10.0.1.10",
		SrcPort: 34567, DstPort: 80, Protocol: "TCP",
		SrcIdentity: 48312, Label: "frontend → backend:80 (두 번째 패킷, CT HIT 예상)",
	})

	// 패킷 3: frontend → redis:6379 (정책 없음, DROP)
	pipeline.Process(Packet{
		SrcIP: "10.0.1.5", DstIP: "10.0.1.15",
		SrcPort: 45678, DstPort: 6379, Protocol: "TCP",
		SrcIdentity: 48312, Label: "frontend → redis:6379 (정책 없음, DROP 예상)",
	})

	// 패킷 4: backend → redis:6379 (정책 있음, ALLOW)
	pipeline.Process(Packet{
		SrcIP: "10.0.1.10", DstIP: "10.0.1.15",
		SrcPort: 56789, DstPort: 6379, Protocol: "TCP",
		SrcIdentity: 48313, Label: "backend → redis:6379 (정책 있음, ALLOW 예상)",
	})

	fmt.Println()
	fmt.Println("═══════════════════════════════════════════════════")
	fmt.Println("[요약]")
	fmt.Println("  1. 첫 패킷: CT MISS → Policy 조회 → CT 생성 → 전달")
	fmt.Println("  2. 후속 패킷: CT HIT → 바로 전달 (Policy 조회 생략!)")
	fmt.Println("  3. 정책 위반: CT MISS → Policy DENY → DROP")
	fmt.Println()
	fmt.Println("  이것이 Cilium이 iptables보다 빠른 이유:")
	fmt.Println("  iptables는 매 패킷마다 전체 규칙 체인을 순회하지만,")
	fmt.Println("  Cilium은 첫 패킷만 정책을 평가하고 이후는 CT 해시맵으로 O(1) 처리")
}
