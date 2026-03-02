// SPDX-License-Identifier: Apache-2.0
// PoC: Hubble Getter 인터페이스 패턴
//
// Hubble Parser는 K8s 메타데이터에 직접 접근하지 않고
// Getter 인터페이스를 통해 접근합니다.
//
// 왜 이 패턴인가?
//   - 테스트 용이성: Mock으로 교체하여 단위 테스트 가능
//   - 관심사 분리: Parser는 파싱에만 집중, 메타데이터는 Getter 담당
//   - 느슨한 결합: K8s API가 바뀌어도 Parser 코드 불변
//
// 실행: go run main.go

package main

import (
	"fmt"
	"net/netip"
	"strings"
)

// ========================================
// 1. Getter 인터페이스 (Hubble의 getters.go 패턴)
// ========================================

// EndpointGetter는 IP 주소로 Pod 정보를 조회합니다.
type EndpointGetter interface {
	GetEndpointInfo(ip netip.Addr) (EndpointInfo, bool)
}

// DNSGetter는 IP 주소의 DNS 이름을 조회합니다.
type DNSGetter interface {
	GetNamesOf(sourceEpID uint32, ip netip.Addr) []string
}

// ServiceGetter는 IP:Port로 서비스를 조회합니다.
type ServiceGetter interface {
	GetServiceByAddr(ip netip.Addr, port uint16) *Service
}

// IdentityGetter는 보안 아이덴티티를 조회합니다.
type IdentityGetter interface {
	GetIdentity(id uint32) (*Identity, error)
}

// ========================================
// 2. 데이터 타입
// ========================================

type EndpointInfo struct {
	ID        uint32
	PodName   string
	Namespace string
	Labels    []string
}

type Service struct {
	Name      string
	Namespace string
}

type Identity struct {
	ID     uint32
	Labels []string
}

// ========================================
// 3. 실제 구현 (프로덕션 - K8s API 연동)
// ========================================

// K8sEndpointGetter는 실제 K8s API를 통해 엔드포인트를 조회합니다.
// (여기서는 인메모리 맵으로 시뮬레이션)
type K8sEndpointGetter struct {
	endpoints map[string]EndpointInfo // IP → EndpointInfo
}

func (k *K8sEndpointGetter) GetEndpointInfo(ip netip.Addr) (EndpointInfo, bool) {
	ep, ok := k.endpoints[ip.String()]
	return ep, ok
}

type K8sDNSGetter struct {
	dnsCache map[string][]string // IP → DNS names
}

func (k *K8sDNSGetter) GetNamesOf(sourceEpID uint32, ip netip.Addr) []string {
	return k.dnsCache[ip.String()]
}

type K8sServiceGetter struct {
	services map[string]*Service // "IP:Port" → Service
}

func (k *K8sServiceGetter) GetServiceByAddr(ip netip.Addr, port uint16) *Service {
	key := fmt.Sprintf("%s:%d", ip.String(), port)
	return k.services[key]
}

type K8sIdentityGetter struct {
	identities map[uint32]*Identity
}

func (k *K8sIdentityGetter) GetIdentity(id uint32) (*Identity, error) {
	if ident, ok := k.identities[id]; ok {
		return ident, nil
	}
	return nil, fmt.Errorf("identity %d not found", id)
}

// ========================================
// 4. Mock 구현 (테스트용)
// ========================================

// MockEndpointGetter는 테스트에서 사용하는 Mock입니다.
// 실제 K8s 클러스터 없이도 Parser를 테스트할 수 있습니다.
type MockEndpointGetter struct {
	returnInfo EndpointInfo
	returnOk   bool
}

func (m *MockEndpointGetter) GetEndpointInfo(ip netip.Addr) (EndpointInfo, bool) {
	return m.returnInfo, m.returnOk
}

type MockDNSGetter struct {
	returnNames []string
}

func (m *MockDNSGetter) GetNamesOf(sourceEpID uint32, ip netip.Addr) []string {
	return m.returnNames
}

// ========================================
// 5. Parser (Getter 인터페이스 소비자)
// ========================================

// Parser는 raw 패킷 데이터를 Flow로 변환합니다.
// K8s 메타데이터를 직접 조회하지 않고 Getter 인터페이스를 통해 접근합니다.
type Parser struct {
	endpoints  EndpointGetter
	dns        DNSGetter
	services   ServiceGetter
	identities IdentityGetter
}

type RawPacket struct {
	SrcIP   string
	DstIP   string
	DstPort uint16
	SrcID   uint32
	DstID   uint32
}

type EnrichedFlow struct {
	SrcIP        string
	DstIP        string
	SrcPod       string
	DstPod       string
	SrcNamespace string
	DstNamespace string
	SrcLabels    []string
	DstLabels    []string
	DstDNSNames  []string
	ServiceName  string
	SrcIdentity  string
}

// Parse는 raw 패킷을 enriched Flow로 변환합니다.
//
// 실제 Hubble threefour.Parser.Decode()의 핵심 로직:
//   1. IP 헤더에서 src/dst 추출
//   2. EndpointGetter로 Pod 메타데이터 조회
//   3. DNSGetter로 DNS 이름 조회
//   4. ServiceGetter로 서비스 이름 조회
//   5. IdentityGetter로 보안 아이덴티티 조회
func (p *Parser) Parse(pkt RawPacket) EnrichedFlow {
	flow := EnrichedFlow{
		SrcIP: pkt.SrcIP,
		DstIP: pkt.DstIP,
	}

	srcAddr, _ := netip.ParseAddr(pkt.SrcIP)
	dstAddr, _ := netip.ParseAddr(pkt.DstIP)

	// Endpoint 메타데이터 enrichment
	if ep, ok := p.endpoints.GetEndpointInfo(srcAddr); ok {
		flow.SrcPod = ep.PodName
		flow.SrcNamespace = ep.Namespace
		flow.SrcLabels = ep.Labels
	}

	if ep, ok := p.endpoints.GetEndpointInfo(dstAddr); ok {
		flow.DstPod = ep.PodName
		flow.DstNamespace = ep.Namespace
		flow.DstLabels = ep.Labels
	}

	// DNS enrichment
	flow.DstDNSNames = p.dns.GetNamesOf(0, dstAddr)

	// Service enrichment
	if svc := p.services.GetServiceByAddr(dstAddr, pkt.DstPort); svc != nil {
		flow.ServiceName = fmt.Sprintf("%s/%s", svc.Namespace, svc.Name)
	}

	// Identity enrichment
	if ident, err := p.identities.GetIdentity(pkt.SrcID); err == nil {
		flow.SrcIdentity = fmt.Sprintf("ID:%d [%s]", ident.ID, strings.Join(ident.Labels, ", "))
	}

	return flow
}

// ========================================
// 6. 실행
// ========================================

func main() {
	fmt.Println("=== PoC: Hubble Getter 인터페이스 패턴 ===")
	fmt.Println()
	fmt.Println("Parser는 K8s 메타데이터를 인터페이스로 접근합니다:")
	fmt.Println("  EndpointGetter: IP → Pod/Namespace/Labels")
	fmt.Println("  DNSGetter:      IP → DNS 이름")
	fmt.Println("  ServiceGetter:  IP:Port → Service 이름")
	fmt.Println("  IdentityGetter: Security ID → Identity 레이블")
	fmt.Println()

	// ── 시나리오 1: 프로덕션 (실제 K8s 데이터) ──
	fmt.Println("━━━ 시나리오 1: 프로덕션 환경 (K8s 데이터 사용) ━━━")
	fmt.Println()

	prodParser := &Parser{
		endpoints: &K8sEndpointGetter{
			endpoints: map[string]EndpointInfo{
				"10.244.0.5":  {ID: 1234, PodName: "frontend-abc12", Namespace: "default", Labels: []string{"app=frontend"}},
				"10.244.0.10": {ID: 5678, PodName: "backend-xyz89", Namespace: "default", Labels: []string{"app=backend", "tier=api"}},
			},
		},
		dns: &K8sDNSGetter{
			dnsCache: map[string][]string{
				"10.244.0.10": {"backend-api.default.svc.cluster.local"},
			},
		},
		services: &K8sServiceGetter{
			services: map[string]*Service{
				"10.244.0.10:8080": {Name: "backend-api", Namespace: "default"},
			},
		},
		identities: &K8sIdentityGetter{
			identities: map[uint32]*Identity{
				56789: {ID: 56789, Labels: []string{"app=frontend", "ns=default"}},
			},
		},
	}

	packet := RawPacket{
		SrcIP: "10.244.0.5", DstIP: "10.244.0.10",
		DstPort: 8080, SrcID: 56789, DstID: 12345,
	}

	fmt.Printf("  Raw 패킷: %s → %s:%d\n", packet.SrcIP, packet.DstIP, packet.DstPort)
	fmt.Println()

	flow := prodParser.Parse(packet)
	printFlow("  ", flow)

	// ── 시나리오 2: 테스트 (Mock 데이터) ──
	fmt.Println()
	fmt.Println("━━━ 시나리오 2: 단위 테스트 (Mock 사용) ━━━")
	fmt.Println()

	testParser := &Parser{
		endpoints: &MockEndpointGetter{
			returnInfo: EndpointInfo{
				ID: 9999, PodName: "test-pod", Namespace: "test-ns",
				Labels: []string{"test=true"},
			},
			returnOk: true,
		},
		dns: &MockDNSGetter{
			returnNames: []string{"test-service.test-ns.svc.cluster.local"},
		},
		services: &K8sServiceGetter{services: map[string]*Service{}},
		identities: &K8sIdentityGetter{
			identities: map[uint32]*Identity{
				1: {ID: 1, Labels: []string{"test=true"}},
			},
		},
	}

	testPacket := RawPacket{
		SrcIP: "192.168.1.1", DstIP: "192.168.1.2",
		DstPort: 80, SrcID: 1,
	}

	fmt.Printf("  Raw 패킷: %s → %s:%d\n", testPacket.SrcIP, testPacket.DstIP, testPacket.DstPort)
	fmt.Println()

	testFlow := testParser.Parse(testPacket)
	printFlow("  ", testFlow)

	fmt.Println()
	fmt.Println("━━━ 비교: 같은 Parser 코드, 다른 Getter 구현 ━━━")
	fmt.Println()
	fmt.Println("  프로덕션: K8sEndpointGetter (실제 K8s API 캐시)")
	fmt.Println("  테스트:   MockEndpointGetter (미리 정의된 값)")
	fmt.Println("  → Parser.Parse() 코드는 완전히 동일!")
	fmt.Println()
	fmt.Println("핵심 포인트:")
	fmt.Println("  - 인터페이스 분리: 각 메타데이터 소스가 독립적인 인터페이스")
	fmt.Println("  - 테스트 용이: K8s 클러스터 없이 Parser 단위 테스트 가능")
	fmt.Println("  - 느슨한 결합: K8s API 변경 시 Getter만 수정, Parser 불변")
	fmt.Println("  - 실제 Hubble: 6개 Getter 인터페이스 (Endpoint, DNS, IP, Service, Identity, Link)")
}

func printFlow(prefix string, f EnrichedFlow) {
	fmt.Printf("%sEnriched Flow:\n", prefix)
	fmt.Printf("%s  Source:      %s (%s/%s) %v\n", prefix, f.SrcIP, f.SrcNamespace, f.SrcPod, f.SrcLabels)
	fmt.Printf("%s  Destination: %s (%s/%s) %v\n", prefix, f.DstIP, f.DstNamespace, f.DstPod, f.DstLabels)
	if len(f.DstDNSNames) > 0 {
		fmt.Printf("%s  DNS Names:   %v\n", prefix, f.DstDNSNames)
	}
	if f.ServiceName != "" {
		fmt.Printf("%s  Service:     %s\n", prefix, f.ServiceName)
	}
	if f.SrcIdentity != "" {
		fmt.Printf("%s  Identity:    %s\n", prefix, f.SrcIdentity)
	}
}
