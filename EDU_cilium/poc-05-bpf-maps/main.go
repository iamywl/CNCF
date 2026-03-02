// Cilium BPF 맵 동작 매커니즘 시뮬레이션
//
// BPF 맵은 커널과 유저스페이스가 공유하는 키-값 저장소.
// 실제로는 bpf() 시스템콜로 접근하지만, 여기서는 Go로 동작 원리를 재현.
//
// Cilium 실제 코드:
//   pkg/maps/ctmap/     — Connection Tracking
//   pkg/maps/policymap/ — Policy (Identity+Port → Allow/Deny)
//   pkg/loadbalancer/maps/ — Service/Backend LB
//
// 실행: go run main.go
package main

import (
	"fmt"
	"strings"
	"time"
)

// -----------------------------------------------------------
// 1. Policy Map — pkg/maps/policymap/policymap.go 재현
//    BPF Hash Map: 각 Endpoint마다 하나씩 존재
// -----------------------------------------------------------
type PolicyKey struct {
	Identity uint32
	DstPort  uint16
	Protocol uint8 // 6=TCP, 17=UDP
}

type PolicyEntry struct {
	ProxyPort uint16
	Flags     uint8 // 0=deny, 1=allow
}

type PolicyMap struct {
	endpointID uint16
	entries    map[PolicyKey]PolicyEntry
	maxSize    int
}

func newPolicyMap(epID uint16, maxSize int) *PolicyMap {
	return &PolicyMap{
		endpointID: epID,
		entries:    make(map[PolicyKey]PolicyEntry),
		maxSize:    maxSize,
	}
}

func (m *PolicyMap) Allow(identity uint32, port uint16, proto uint8) {
	key := PolicyKey{Identity: identity, DstPort: port, Protocol: proto}
	m.entries[key] = PolicyEntry{Flags: 1}
}

func (m *PolicyMap) Lookup(identity uint32, port uint16, proto uint8) string {
	key := PolicyKey{Identity: identity, DstPort: port, Protocol: proto}
	if e, ok := m.entries[key]; ok && e.Flags == 1 {
		return "ALLOW"
	}
	return "DENY"
}

// -----------------------------------------------------------
// 2. CT Map (LRU) — pkg/maps/ctmap/ctmap.go 재현
//    BPF LRU Hash Map: 오래된 연결을 자동 삭제
// -----------------------------------------------------------
type CTKey struct {
	SrcAddr  string
	DstAddr  string
	SrcPort  uint16
	DstPort  uint16
	Protocol uint8
}

type CTEntry struct {
	Lifetime    time.Time
	RxPackets   uint64
	TxPackets   uint64
	Flags       uint16
	RevNATID    uint16
	SrcIdentity uint32
}

type CTMapLRU struct {
	entries map[CTKey]*CTEntry
	maxSize int
	order   []CTKey // LRU 순서 추적
}

func newCTMapLRU(maxSize int) *CTMapLRU {
	return &CTMapLRU{
		entries: make(map[CTKey]*CTEntry),
		maxSize: maxSize,
	}
}

func (m *CTMapLRU) Lookup(key CTKey) (*CTEntry, bool) {
	e, ok := m.entries[key]
	if ok {
		// LRU: 접근한 엔트리를 맨 뒤로 이동 (최근 사용)
		m.moveToBack(key)
	}
	return e, ok
}

func (m *CTMapLRU) Create(key CTKey, srcIdentity uint32) {
	// LRU: 맵이 가득 차면 가장 오래된 엔트리 제거
	if len(m.entries) >= m.maxSize {
		evicted := m.order[0]
		delete(m.entries, evicted)
		m.order = m.order[1:]
		fmt.Printf("      [LRU 제거] %s:%d → %s:%d (맵 용량 초과)\n",
			evicted.SrcAddr, evicted.SrcPort, evicted.DstAddr, evicted.DstPort)
	}

	m.entries[key] = &CTEntry{
		Lifetime:    time.Now().Add(5 * time.Minute),
		RxPackets:   1,
		Flags:       0x01, // SYN_SEEN
		SrcIdentity: srcIdentity,
	}
	m.order = append(m.order, key)
}

func (m *CTMapLRU) moveToBack(key CTKey) {
	for i, k := range m.order {
		if k == key {
			m.order = append(m.order[:i], m.order[i+1:]...)
			m.order = append(m.order, key)
			break
		}
	}
}

// -----------------------------------------------------------
// 3. Service Map — pkg/loadbalancer/maps/lbmaps.go 재현
//    BPF Hash Map: Frontend → Backend 매핑
// -----------------------------------------------------------
type ServiceKey struct {
	Address  string
	Port     uint16
	Protocol uint8
}

type ServiceValue struct {
	BackendCount uint16
	RevNATID     uint16
}

type BackendKey struct {
	ID uint32
}

type BackendValue struct {
	Address string
	Port    uint16
}

type ServiceMap struct {
	services map[ServiceKey]ServiceValue
	backends map[BackendKey]BackendValue
}

func newServiceMap() *ServiceMap {
	return &ServiceMap{
		services: make(map[ServiceKey]ServiceValue),
		backends: make(map[BackendKey]BackendValue),
	}
}

func (m *ServiceMap) AddService(vip string, port uint16, backends []BackendValue) {
	key := ServiceKey{Address: vip, Port: port, Protocol: 6}
	m.services[key] = ServiceValue{
		BackendCount: uint16(len(backends)),
		RevNATID:     uint16(len(m.services) + 1),
	}
	for i, b := range backends {
		m.backends[BackendKey{ID: uint32(len(m.backends) + i + 1)}] = b
	}
}

func (m *ServiceMap) LookupService(vip string, port uint16) ([]BackendValue, bool) {
	key := ServiceKey{Address: vip, Port: port, Protocol: 6}
	svc, ok := m.services[key]
	if !ok {
		return nil, false
	}
	var result []BackendValue
	count := 0
	for _, b := range m.backends {
		result = append(result, b)
		count++
		if count >= int(svc.BackendCount) {
			break
		}
	}
	return result, true
}

// -----------------------------------------------------------
// 메인 시뮬레이션
// -----------------------------------------------------------
func main() {
	fmt.Println("=== Cilium BPF 맵 동작 시뮬레이터 ===")
	fmt.Println()

	// ===== Policy Map =====
	fmt.Println("[1] Policy Map (Hash Map)")
	fmt.Println("    실제 코드: pkg/maps/policymap/policymap.go")
	fmt.Println("    특성: Endpoint마다 1개, 기본 최대 65536 엔트리")
	fmt.Println(strings.Repeat("─", 55))

	polMap := newPolicyMap(1234, 65536)
	polMap.Allow(48312, 80, 6)   // frontend → 80/TCP 허용
	polMap.Allow(2, 80, 6)       // world → 80/TCP 허용
	polMap.Allow(48313, 6379, 6) // backend → 6379/TCP 허용

	fmt.Println("    등록된 규칙:")
	fmt.Println("      Identity 48312 (frontend) → 80/TCP  = ALLOW")
	fmt.Println("      Identity 2 (world)        → 80/TCP  = ALLOW")
	fmt.Println("      Identity 48313 (backend)  → 6379/TCP = ALLOW")
	fmt.Println()
	fmt.Println("    조회 테스트:")
	tests := []struct{ id uint32; port uint16; proto uint8; name string }{
		{48312, 80, 6, "frontend → 80/TCP"},
		{48312, 443, 6, "frontend → 443/TCP"},
		{48313, 6379, 6, "backend → 6379/TCP"},
		{99999, 80, 6, "unknown → 80/TCP"},
	}
	for _, t := range tests {
		result := polMap.Lookup(t.id, t.port, t.proto)
		fmt.Printf("      %-25s = %s\n", t.name, result)
	}

	// ===== CT Map (LRU) =====
	fmt.Println()
	fmt.Println("[2] CT Map (LRU Hash Map)")
	fmt.Println("    실제 코드: pkg/maps/ctmap/ctmap.go")
	fmt.Println("    특성: LRU — 맵이 가득 차면 가장 오래된 엔트리 자동 제거")
	fmt.Println("    기본 최대: TCP 524288, 비-TCP 262144 엔트리")
	fmt.Println(strings.Repeat("─", 55))

	// 의도적으로 작은 맵 (maxSize=3)으로 LRU 동작을 보여줌
	ctMap := newCTMapLRU(3)
	fmt.Println("    맵 크기: 3 (LRU 동작 확인용)")
	fmt.Println()

	connections := []CTKey{
		{"10.0.1.5", "10.0.1.10", 34567, 80, 6},
		{"10.0.1.5", "10.0.1.15", 34568, 6379, 6},
		{"10.0.1.10", "10.0.1.15", 34569, 6379, 6},
	}

	fmt.Println("    3개 연결 생성:")
	for _, c := range connections {
		ctMap.Create(c, 48312)
		fmt.Printf("      + %s:%d → %s:%d\n", c.SrcAddr, c.SrcPort, c.DstAddr, c.DstPort)
	}
	fmt.Printf("    현재 맵 크기: %d/%d\n", len(ctMap.entries), ctMap.maxSize)
	fmt.Println()

	// 4번째 연결 추가 → LRU에 의해 첫 번째 제거
	fmt.Println("    4번째 연결 추가 (LRU 제거 발생):")
	newConn := CTKey{"10.0.1.20", "10.0.1.10", 34570, 80, 6}
	ctMap.Create(newConn, 48315)
	fmt.Printf("      + %s:%d → %s:%d\n", newConn.SrcAddr, newConn.SrcPort, newConn.DstAddr, newConn.DstPort)
	fmt.Printf("    현재 맵 크기: %d/%d\n", len(ctMap.entries), ctMap.maxSize)
	fmt.Println()

	// 조회 테스트
	fmt.Println("    조회 테스트:")
	if _, ok := ctMap.Lookup(connections[0]); !ok {
		fmt.Printf("      %s:%d → MISS (LRU에 의해 제거됨)\n", connections[0].SrcAddr, connections[0].SrcPort)
	}
	if e, ok := ctMap.Lookup(connections[1]); ok {
		fmt.Printf("      %s:%d → HIT (packets=%d)\n", connections[1].SrcAddr, connections[1].SrcPort, e.RxPackets)
	}

	// ===== Service Map =====
	fmt.Println()
	fmt.Println("[3] Service Map (서비스 로드밸런싱)")
	fmt.Println("    실제 코드: pkg/loadbalancer/maps/lbmaps.go")
	fmt.Println("    동작: ClusterIP(VIP) → 실제 Backend Pod으로 DNAT")
	fmt.Println(strings.Repeat("─", 55))

	svcMap := newServiceMap()
	svcMap.AddService("10.96.0.100", 80, []BackendValue{
		{"10.0.1.10", 8080},
		{"10.0.1.11", 8080},
		{"10.0.1.12", 8080},
	})

	fmt.Println("    등록된 서비스:")
	fmt.Println("      VIP 10.96.0.100:80 → 3개 Backend")
	fmt.Println()

	fmt.Println("    조회 (connect() 시점에 실행):")
	if backends, ok := svcMap.LookupService("10.96.0.100", 80); ok {
		fmt.Printf("      10.96.0.100:80 → %d개 Backend 발견\n", len(backends))
		for i, b := range backends {
			fmt.Printf("        Backend %d: %s:%d\n", i+1, b.Address, b.Port)
		}
		fmt.Println()
		fmt.Println("      Maglev/Random으로 하나 선택 → 소켓 dst 주소를 교체")
		fmt.Println("      이후 패킷은 Backend IP로 직접 전송 (매 패킷 NAT 불필요)")
	}

	fmt.Println()
	fmt.Println("[요약]")
	fmt.Println("  - Policy Map: Hash Map, Endpoint마다 1개, Identity+Port로 O(1) 조회")
	fmt.Println("  - CT Map: LRU Hash Map, 용량 초과 시 오래된 연결 자동 정리")
	fmt.Println("  - Service Map: Hash Map, VIP → Backend 변환으로 kube-proxy 대체")
	fmt.Println("  - 모든 맵은 커널과 유저스페이스가 공유 → bpf() 시스템콜로 접근")
}
