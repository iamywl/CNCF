// SPDX-License-Identifier: Apache-2.0
// PoC: Hubble CIDR/IP 네트워크 필터링 패턴
//
// Hubble은 CIDR(Classless Inter-Domain Routing) 표기법으로
// IP 주소 범위를 필터링합니다:
//   - 개별 IP: --ip-source 10.244.0.5
//   - CIDR: --ip-source 10.244.0.0/24
//   - 혼합: 개별 IP와 CIDR 동시 사용
//
// Go 1.18+의 net/netip 패키지 사용 (net.IP보다 효율적)
//
// 실행: go run main.go

package main

import (
	"fmt"
	"net/netip"
	"strings"
)

// ========================================
// 1. IP/CIDR 필터 (Hubble의 filters/ip.go 패턴)
// ========================================

// IPFilter는 IP 주소와 CIDR 프리픽스로 필터링합니다.
// 실제 Hubble: pkg/hubble/filters/ip.go의 filterByIPs 함수
type IPFilter struct {
	addresses []netip.Addr   // 개별 IP 주소
	prefixes  []netip.Prefix // CIDR 프리픽스
}

// NewIPFilter는 IP 문자열 목록으로 필터를 생성합니다.
// "/"가 포함되면 CIDR, 아니면 개별 IP로 처리합니다.
func NewIPFilter(patterns []string) (*IPFilter, error) {
	f := &IPFilter{}

	for _, p := range patterns {
		if strings.Contains(p, "/") {
			// CIDR 표기법
			prefix, err := netip.ParsePrefix(p)
			if err != nil {
				return nil, fmt.Errorf("invalid CIDR %q: %w", p, err)
			}
			f.prefixes = append(f.prefixes, prefix)
		} else {
			// 개별 IP
			addr, err := netip.ParseAddr(p)
			if err != nil {
				return nil, fmt.Errorf("invalid IP %q: %w", p, err)
			}
			f.addresses = append(f.addresses, addr)
		}
	}

	return f, nil
}

// Match는 주어진 IP가 필터에 매치하는지 확인합니다.
func (f *IPFilter) Match(ipStr string) bool {
	addr, err := netip.ParseAddr(ipStr)
	if err != nil {
		return false
	}

	// 개별 IP 매치
	for _, a := range f.addresses {
		if a == addr {
			return true
		}
	}

	// CIDR 매치
	for _, prefix := range f.prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}

	return false
}

// MatchReason은 매치 결과와 이유를 반환합니다.
func (f *IPFilter) MatchReason(ipStr string) (bool, string) {
	addr, err := netip.ParseAddr(ipStr)
	if err != nil {
		return false, fmt.Sprintf("invalid IP: %s", ipStr)
	}

	for _, a := range f.addresses {
		if a == addr {
			return true, fmt.Sprintf("exact match: %s", a)
		}
	}

	for _, prefix := range f.prefixes {
		if prefix.Contains(addr) {
			return true, fmt.Sprintf("CIDR match: %s contains %s", prefix, addr)
		}
	}

	return false, "no match"
}

// ========================================
// 2. Flow 타입과 필터 적용
// ========================================

type Flow struct {
	SrcIP       string
	DstIP       string
	Source      string
	Destination string
	Verdict     string
}

// FlowFilter는 src/dst IP 필터를 조합합니다.
type FlowFilter struct {
	SrcFilter *IPFilter
	DstFilter *IPFilter
}

func (ff *FlowFilter) Match(flow Flow) (bool, string) {
	reasons := []string{}

	if ff.SrcFilter != nil {
		match, reason := ff.SrcFilter.MatchReason(flow.SrcIP)
		if !match {
			return false, fmt.Sprintf("src %s: %s", flow.SrcIP, reason)
		}
		reasons = append(reasons, fmt.Sprintf("src: %s", reason))
	}

	if ff.DstFilter != nil {
		match, reason := ff.DstFilter.MatchReason(flow.DstIP)
		if !match {
			return false, fmt.Sprintf("dst %s: %s", flow.DstIP, reason)
		}
		reasons = append(reasons, fmt.Sprintf("dst: %s", reason))
	}

	return true, strings.Join(reasons, ", ")
}

// ========================================
// 3. netip vs net.IP 비교
// ========================================

func demonstrateNetip() {
	fmt.Println("━━━ netip 패키지 기능 데모 ━━━")
	fmt.Println()

	// Addr: IP 주소 (값 타입, 비교 가능)
	addr1 := netip.MustParseAddr("10.244.0.5")
	addr2 := netip.MustParseAddr("10.244.0.5")
	addr3 := netip.MustParseAddr("fc00::1")

	fmt.Println("  [Addr] IP 주소 파싱:")
	fmt.Printf("    IPv4: %s (Is4=%t, Is6=%t)\n", addr1, addr1.Is4(), addr1.Is6())
	fmt.Printf("    IPv6: %s (Is4=%t, Is6=%t)\n", addr3, addr3.Is4(), addr3.Is6())
	fmt.Printf("    비교: %s == %s → %t\n", addr1, addr2, addr1 == addr2)
	fmt.Println()

	// Prefix: CIDR (네트워크 주소 + 마스크 비트)
	prefix1 := netip.MustParsePrefix("10.244.0.0/24")
	prefix2 := netip.MustParsePrefix("10.244.0.0/16")
	prefix3 := netip.MustParsePrefix("192.168.1.0/24")

	fmt.Println("  [Prefix] CIDR 파싱:")
	fmt.Printf("    %s → 네트워크: %s, 비트: %d\n", prefix1, prefix1.Masked().Addr(), prefix1.Bits())
	fmt.Printf("    %s → 네트워크: %s, 비트: %d\n", prefix2, prefix2.Masked().Addr(), prefix2.Bits())
	fmt.Println()

	// Contains: CIDR에 IP가 포함되는지
	testIPs := []string{"10.244.0.5", "10.244.1.100", "10.244.255.1", "192.168.1.50", "172.16.0.1"}
	fmt.Println("  [Contains] CIDR 포함 여부:")
	for _, ip := range testIPs {
		addr := netip.MustParseAddr(ip)
		fmt.Printf("    %-16s in %-18s → %t\n", ip, prefix1, prefix1.Contains(addr))
		fmt.Printf("    %-16s in %-18s → %t\n", ip, prefix2, prefix2.Contains(addr))
		fmt.Printf("    %-16s in %-18s → %t\n", ip, prefix3, prefix3.Contains(addr))
		fmt.Println()
	}

	// net/netip vs net.IP 장점
	fmt.Println("  [netip vs net.IP 비교]")
	fmt.Println("    net.IP:   슬라이스 기반, == 비교 불가, GC 부하")
	fmt.Println("    netip.Addr: 값 타입, == 비교 가능, 맵 키 사용 가능")
	fmt.Println("    netip.Prefix: CIDR 파싱+포함 검사 내장")
	fmt.Println()
}

// ========================================
// 4. 실행
// ========================================

func main() {
	fmt.Println("=== PoC: Hubble CIDR/IP 필터링 패턴 ===")
	fmt.Println()
	fmt.Println("Hubble의 IP 필터링 옵션:")
	fmt.Println("  --ip-source 10.244.0.5          (개별 IP)")
	fmt.Println("  --ip-source 10.244.0.0/24       (CIDR)")
	fmt.Println("  --ip-destination 192.168.0.0/16  (목적지 CIDR)")
	fmt.Println()

	demonstrateNetip()

	// 테스트 Flow
	flows := []Flow{
		{SrcIP: "10.244.0.5", DstIP: "10.244.0.10", Source: "frontend", Destination: "backend", Verdict: "FORWARDED"},
		{SrcIP: "10.244.0.20", DstIP: "10.96.0.10", Source: "frontend", Destination: "coredns", Verdict: "FORWARDED"},
		{SrcIP: "10.244.1.100", DstIP: "10.244.0.10", Source: "scanner", Destination: "backend", Verdict: "DROPPED"},
		{SrcIP: "192.168.1.50", DstIP: "10.244.0.10", Source: "external", Destination: "backend", Verdict: "DROPPED"},
		{SrcIP: "10.244.0.8", DstIP: "172.16.0.1", Source: "monitor", Destination: "external", Verdict: "FORWARDED"},
	}

	// ── 시나리오 1: 개별 IP 필터 ──
	fmt.Println("━━━ 시나리오 1: --ip-source 10.244.0.5 ━━━")
	fmt.Println()

	srcFilter1, _ := NewIPFilter([]string{"10.244.0.5"})
	ff1 := &FlowFilter{SrcFilter: srcFilter1}
	runFilter(ff1, flows)

	// ── 시나리오 2: CIDR 필터 ──
	fmt.Println("━━━ 시나리오 2: --ip-source 10.244.0.0/24 ━━━")
	fmt.Println()

	srcFilter2, _ := NewIPFilter([]string{"10.244.0.0/24"})
	ff2 := &FlowFilter{SrcFilter: srcFilter2}
	runFilter(ff2, flows)

	// ── 시나리오 3: 개별 IP + CIDR 혼합 ──
	fmt.Println("━━━ 시나리오 3: --ip-source 10.244.0.5,192.168.1.0/24 ━━━")
	fmt.Println()

	srcFilter3, _ := NewIPFilter([]string{"10.244.0.5", "192.168.1.0/24"})
	ff3 := &FlowFilter{SrcFilter: srcFilter3}
	runFilter(ff3, flows)

	// ── 시나리오 4: src + dst 동시 필터 ──
	fmt.Println("━━━ 시나리오 4: --ip-source 10.244.0.0/16 --ip-destination 10.244.0.10 ━━━")
	fmt.Println()

	srcFilter4, _ := NewIPFilter([]string{"10.244.0.0/16"})
	dstFilter4, _ := NewIPFilter([]string{"10.244.0.10"})
	ff4 := &FlowFilter{SrcFilter: srcFilter4, DstFilter: dstFilter4}
	runFilter(ff4, flows)

	fmt.Println("핵심 포인트:")
	fmt.Println("  - net/netip: Go 1.18+ 표준 라이브러리, 값 타입 IP/CIDR")
	fmt.Println("  - netip.Prefix.Contains(): CIDR 포함 여부 O(1) 검사")
	fmt.Println("  - 개별 IP + CIDR 혼합 필터링 지원")
	fmt.Println("  - 실제 Hubble: slices.Contains + slices.ContainsFunc 조합")
}

func runFilter(ff *FlowFilter, flows []Flow) {
	passed := 0
	for _, flow := range flows {
		match, reason := ff.Match(flow)
		mark := "✗"
		if match {
			mark = "✓"
			passed++
		}
		fmt.Printf("  %s %s(%s) → %s(%s)  ← %s\n",
			mark, flow.Source, flow.SrcIP, flow.Destination, flow.DstIP, reason)
	}
	fmt.Printf("\n  결과: %d/%d 통과\n\n", passed, len(flows))
}
