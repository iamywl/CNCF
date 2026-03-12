package main

import (
	"fmt"
	"net"
	"strings"
)

// =============================================================================
// PoC 09: 역방향 DNS (Reverse DNS)
// =============================================================================
// CoreDNS의 plugin/pkg/dnsutil/reverse.go에서 구현된 역방향 DNS 조회를 시뮬레이션한다.
// PTR 레코드는 IP 주소를 도메인 이름으로 변환하며, in-addr.arpa (IPv4)와
// ip6.arpa (IPv6) 형식을 사용한다.
//
// 참조: coredns/plugin/pkg/dnsutil/reverse.go
//       - ExtractAddressFromReverse(): 역방향 이름에서 IP 주소 추출
//       - IsReverse(): 역방향 zone 여부 판단
//       - reverse(), reverse6(): 세그먼트 역순 변환
// =============================================================================

const (
	// IPv4 역방향 DNS 접미사
	IP4arpa = ".in-addr.arpa."
	// IPv6 역방향 DNS 접미사
	IP6arpa = ".ip6.arpa."
)

// PTRRecord는 역방향 DNS PTR 레코드를 나타낸다.
type PTRRecord struct {
	ReverseName string // 역방향 이름 (예: 54.119.58.176.in-addr.arpa.)
	Domain      string // 대응하는 도메인 (예: server1.example.com.)
	TTL         uint32
}

// ReverseZone은 역방향 DNS Zone을 관리한다.
type ReverseZone struct {
	Name    string                // Zone 이름 (예: 168.192.in-addr.arpa.)
	Records map[string]*PTRRecord // 역방향이름 → PTR 레코드
}

// NewReverseZone은 새로운 역방향 Zone을 생성한다.
func NewReverseZone(name string) *ReverseZone {
	return &ReverseZone{
		Name:    name,
		Records: make(map[string]*PTRRecord),
	}
}

// AddRecord는 역방향 Zone에 PTR 레코드를 추가한다.
func (z *ReverseZone) AddRecord(ip, domain string, ttl uint32) error {
	reverseName, err := IPToReverseName(ip)
	if err != nil {
		return fmt.Errorf("IP→역방향 변환 실패: %v", err)
	}

	// Zone에 속하는지 확인
	if !strings.HasSuffix(reverseName, z.Name) {
		return fmt.Errorf("IP %s의 역방향 이름 %s이(가) zone %s에 속하지 않음", ip, reverseName, z.Name)
	}

	z.Records[reverseName] = &PTRRecord{
		ReverseName: reverseName,
		Domain:      domain,
		TTL:         ttl,
	}
	return nil
}

// Lookup은 역방향 이름으로 PTR 레코드를 조회한다.
func (z *ReverseZone) Lookup(reverseName string) (*PTRRecord, bool) {
	rec, ok := z.Records[reverseName]
	return rec, ok
}

// =============================================================================
// IP ↔ 역방향 이름 변환 함수
// CoreDNS의 reverse.go에서 구현된 핵심 알고리즘을 재현한다.
// =============================================================================

// IPToReverseName은 IP 주소를 역방향 DNS 이름으로 변환한다.
// IPv4: 192.168.1.10 → 10.1.168.192.in-addr.arpa.
// IPv6: 2001:db8::567:89ab → b.a.9.8.7.6.5.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2.ip6.arpa.
func IPToReverseName(ipStr string) (string, error) {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return "", fmt.Errorf("유효하지 않은 IP 주소: %s", ipStr)
	}

	// IPv4 처리
	if ip4 := ip.To4(); ip4 != nil {
		// 옥텟을 역순으로 배치
		return fmt.Sprintf("%d.%d.%d.%d%s", ip4[3], ip4[2], ip4[1], ip4[0], IP4arpa), nil
	}

	// IPv6 처리: 16바이트를 각 니블(4비트)로 분해하여 역순 배치
	ip16 := ip.To16()
	if ip16 == nil {
		return "", fmt.Errorf("IPv6 변환 실패: %s", ipStr)
	}

	var parts []string
	for i := len(ip16) - 1; i >= 0; i-- {
		b := ip16[i]
		parts = append(parts, fmt.Sprintf("%x", b&0x0f))   // 하위 니블
		parts = append(parts, fmt.Sprintf("%x", b>>4&0x0f)) // 상위 니블
	}
	return strings.Join(parts, ".") + IP6arpa, nil
}

// ExtractAddressFromReverse는 역방향 DNS 이름에서 IP 주소를 추출한다.
// CoreDNS의 dnsutil.ExtractAddressFromReverse()와 동일한 로직이다.
//
// IPv4: 54.119.58.176.in-addr.arpa. → 176.58.119.54
// IPv6: b.a.9.8.7.6.5.0...ip6.arpa. → 2001:db8::567:89ab
func ExtractAddressFromReverse(reverseName string) string {
	var search string
	var reverseFunc func([]string) string

	switch {
	case strings.HasSuffix(reverseName, IP4arpa):
		search = strings.TrimSuffix(reverseName, IP4arpa)
		reverseFunc = reverseIPv4
	case strings.HasSuffix(reverseName, IP6arpa):
		search = strings.TrimSuffix(reverseName, IP6arpa)
		reverseFunc = reverseIPv6
	default:
		return ""
	}

	parts := strings.Split(search, ".")
	return reverseFunc(parts)
}

// reverseIPv4는 IPv4 세그먼트를 역순으로 조합하여 IP 주소를 반환한다.
func reverseIPv4(parts []string) string {
	// IPv4는 정확히 4개의 옥텟이어야 한다
	if len(parts) != 4 {
		return ""
	}

	// 역순 배치
	for i := 0; i < len(parts)/2; i++ {
		j := len(parts) - 1 - i
		parts[i], parts[j] = parts[j], parts[i]
	}

	ip := net.ParseIP(strings.Join(parts, "."))
	if ip == nil || ip.To4() == nil {
		return ""
	}
	return ip.String()
}

// reverseIPv6는 IPv6 니블을 역순으로 조합하여 IP 주소를 반환한다.
// CoreDNS의 reverse6()와 동일한 알고리즘이다.
func reverseIPv6(parts []string) string {
	// IPv6는 정확히 32개의 니블이어야 한다
	if len(parts) != 32 {
		return ""
	}

	// 역순 배치
	for i := 0; i < len(parts)/2; i++ {
		j := len(parts) - 1 - i
		parts[i], parts[j] = parts[j], parts[i]
	}

	// 4개씩 그룹화하여 16비트 세그먼트 생성
	segments := make([]string, 0, 8)
	for i := 0; i < len(parts)/4; i++ {
		segments = append(segments, strings.Join(parts[i*4:i*4+4], ""))
	}

	ip := net.ParseIP(strings.Join(segments, ":"))
	if ip == nil || ip.To16() == nil {
		return ""
	}
	return ip.String()
}

// IsReverse는 이름이 역방향 zone에 속하는지 판단한다.
// 반환값: 0=역방향 아님, 1=IPv4 역방향, 2=IPv6 역방향
func IsReverse(name string) int {
	if strings.HasSuffix(name, IP4arpa) {
		return 1
	}
	if strings.HasSuffix(name, IP6arpa) {
		return 2
	}
	return 0
}

// ReverseServer는 역방향 DNS 조회를 처리하는 서버를 시뮬레이션한다.
type ReverseServer struct {
	Zones map[string]*ReverseZone // zone이름 → ReverseZone
}

// NewReverseServer는 새로운 역방향 DNS 서버를 생성한다.
func NewReverseServer() *ReverseServer {
	return &ReverseServer{
		Zones: make(map[string]*ReverseZone),
	}
}

// AddZone은 역방향 Zone을 서버에 추가한다.
func (s *ReverseServer) AddZone(zone *ReverseZone) {
	s.Zones[zone.Name] = zone
}

// Query는 역방향 DNS 쿼리를 처리한다.
func (s *ReverseServer) Query(reverseName string) (string, error) {
	// 역방향 zone 여부 확인
	reverseType := IsReverse(reverseName)
	if reverseType == 0 {
		return "", fmt.Errorf("역방향 DNS 이름이 아닙니다: %s", reverseName)
	}

	// 매칭되는 zone 찾기 (가장 긴 매치)
	var bestZone *ReverseZone
	bestLen := 0
	for zoneName, zone := range s.Zones {
		if strings.HasSuffix(reverseName, zoneName) && len(zoneName) > bestLen {
			bestZone = zone
			bestLen = len(zoneName)
		}
	}

	if bestZone == nil {
		return "", fmt.Errorf("매칭되는 역방향 zone을 찾을 수 없음: %s", reverseName)
	}

	// PTR 레코드 조회
	rec, ok := bestZone.Lookup(reverseName)
	if !ok {
		return "", fmt.Errorf("PTR 레코드 없음: %s", reverseName)
	}

	return rec.Domain, nil
}

func main() {
	fmt.Println("=== CoreDNS 역방향 DNS (Reverse DNS) PoC ===")
	fmt.Println()

	// =========================================================================
	// 1. IP → 역방향 이름 변환
	// =========================================================================
	fmt.Println("--- 1. IP → 역방향 이름 변환 ---")

	ipv4Tests := []string{"192.168.1.10", "10.0.0.1", "176.58.119.54"}
	for _, ip := range ipv4Tests {
		rev, err := IPToReverseName(ip)
		if err != nil {
			fmt.Printf("  오류: %v\n", err)
			continue
		}
		fmt.Printf("  IPv4: %-18s → %s\n", ip, rev)
	}

	fmt.Println()

	ipv6Tests := []string{"2001:db8::567:89ab", "::1", "fe80::1"}
	for _, ip := range ipv6Tests {
		rev, err := IPToReverseName(ip)
		if err != nil {
			fmt.Printf("  오류: %v\n", err)
			continue
		}
		fmt.Printf("  IPv6: %-22s → %s\n", ip, rev)
	}

	// =========================================================================
	// 2. 역방향 이름 → IP 추출 (ExtractAddressFromReverse)
	// =========================================================================
	fmt.Println()
	fmt.Println("--- 2. 역방향 이름 → IP 추출 ---")

	reverseTests := []struct {
		name     string
		expected string
	}{
		{"54.119.58.176.in-addr.arpa.", "176.58.119.54"},
		{"10.1.168.192.in-addr.arpa.", "192.168.1.10"},
		{"b.a.9.8.7.6.5.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2.ip6.arpa.", "2001:db8::567:89ab"},
		{".58.176.in-addr.arpa.", ""},           // 유효하지 않음
		{"d.0.1.0.0.2.ip6.arpa.", ""},           // 니블 수 부족
		{"example.com.", ""},                     // 역방향 아님
	}

	for _, tc := range reverseTests {
		result := ExtractAddressFromReverse(tc.name)
		status := "OK"
		if result != tc.expected {
			status = "FAIL"
		}
		if result == "" {
			result = "(빈 문자열 - 변환 불가)"
		}
		fmt.Printf("  [%s] %-60s → %s\n", status, tc.name, result)
	}

	// =========================================================================
	// 3. IsReverse - 역방향 zone 여부 판단
	// =========================================================================
	fmt.Println()
	fmt.Println("--- 3. IsReverse - 역방향 zone 여부 판단 ---")

	isReverseTests := []struct {
		name     string
		expected int
	}{
		{"10.1.168.192.in-addr.arpa.", 1},
		{"b.a.9.8.7.6.5.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2.ip6.arpa.", 2},
		{"example.com.", 0},
		{"in-addr.arpa.example.com.", 0},
	}

	typeNames := map[int]string{0: "일반", 1: "IPv4 역방향", 2: "IPv6 역방향"}
	for _, tc := range isReverseTests {
		result := IsReverse(tc.name)
		status := "OK"
		if result != tc.expected {
			status = "FAIL"
		}
		fmt.Printf("  [%s] %-65s → %d (%s)\n", status, tc.name, result, typeNames[result])
	}

	// =========================================================================
	// 4. 역방향 Zone 데이터 관리 및 PTR 조회
	// =========================================================================
	fmt.Println()
	fmt.Println("--- 4. 역방향 Zone 관리 및 PTR 조회 ---")

	server := NewReverseServer()

	// IPv4 역방향 Zone 생성
	zone4 := NewReverseZone("168.192.in-addr.arpa.")
	zone4.AddRecord("192.168.1.10", "web-server.example.com.", 3600)
	zone4.AddRecord("192.168.1.20", "db-server.example.com.", 3600)
	zone4.AddRecord("192.168.1.30", "cache-server.example.com.", 3600)
	zone4.AddRecord("192.168.2.1", "gateway.example.com.", 3600)
	server.AddZone(zone4)

	// IPv6 역방향 Zone 생성
	zone6 := NewReverseZone("8.b.d.0.1.0.0.2.ip6.arpa.")
	zone6.AddRecord("2001:db8::1", "ns1.example.com.", 3600)
	zone6.AddRecord("2001:db8::567:89ab", "app-server.example.com.", 3600)
	server.AddZone(zone6)

	fmt.Printf("  등록된 Zone 수: %d\n", len(server.Zones))
	for zoneName, zone := range server.Zones {
		fmt.Printf("  Zone: %-35s (레코드 %d개)\n", zoneName, len(zone.Records))
	}

	// =========================================================================
	// 5. PTR 조회 데모
	// =========================================================================
	fmt.Println()
	fmt.Println("--- 5. PTR 레코드 조회 데모 ---")

	queries := []string{
		"10.1.168.192.in-addr.arpa.",                                                         // web-server
		"20.1.168.192.in-addr.arpa.",                                                         // db-server
		"1.2.168.192.in-addr.arpa.",                                                          // gateway
		"99.1.168.192.in-addr.arpa.",                                                         // 없는 레코드
		"1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2.ip6.arpa.",       // ns1
		"b.a.9.8.7.6.5.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2.ip6.arpa.",       // app-server
		"example.com.",                                                                        // 비역방향
	}

	for _, q := range queries {
		domain, err := server.Query(q)
		if err != nil {
			fmt.Printf("  쿼리: %-65s → 오류: %s\n", q, err)
		} else {
			// IP도 추출하여 전체 흐름 보여줌
			ip := ExtractAddressFromReverse(q)
			fmt.Printf("  쿼리: %-65s\n", q)
			fmt.Printf("    IP: %-18s → PTR: %s\n", ip, domain)
		}
	}

	// =========================================================================
	// 6. 왕복 변환 검증 (IP → 역방향 → IP)
	// =========================================================================
	fmt.Println()
	fmt.Println("--- 6. 왕복 변환 검증 (IP → 역방향이름 → IP) ---")

	roundTripIPs := []string{
		"192.168.1.10",
		"10.0.0.1",
		"2001:db8::567:89ab",
		"::1",
		"fe80::1",
	}

	for _, ip := range roundTripIPs {
		rev, err := IPToReverseName(ip)
		if err != nil {
			fmt.Printf("  오류: %v\n", err)
			continue
		}
		extracted := ExtractAddressFromReverse(rev)
		// net.ParseIP로 정규화한 값과 비교
		original := net.ParseIP(ip).String()
		match := "OK"
		if extracted != original {
			match = "FAIL"
		}
		fmt.Printf("  [%s] %s → %s → %s\n", match, ip, rev, extracted)
	}
}
