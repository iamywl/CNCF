// SPDX-License-Identifier: Apache-2.0
// PoC: Hubble FQDN 패턴 매칭
//
// Hubble은 DNS 이름(FQDN)을 와일드카드 패턴으로 필터링합니다:
//   - 와일드카드: *.google.com → maps.google.com, api.google.com
//   - 정확한 매치: google.com → google.com만
//   - 복합 패턴: *.*.svc.cluster.local → 2단계 서브도메인 매치
//   - 노드 이름: cluster/node 형식 지원
//
// 와일드카드를 정규식으로 변환하여 매칭합니다.
//
// 실행: go run main.go

package main

import (
	"fmt"
	"regexp"
	"strings"
)

// ========================================
// 1. FQDN 패턴 컴파일 (Hubble의 patterns.go)
// ========================================

// FQDNMatcher는 FQDN 패턴을 정규식으로 변환하여 매칭합니다.
// 실제 Hubble: pkg/hubble/filters/fqdn.go
type FQDNMatcher struct {
	patterns []string
	regex    *regexp.Regexp
}

// NewFQDNMatcher는 FQDN 패턴 목록을 컴파일합니다.
// 여러 패턴은 OR로 결합됩니다.
func NewFQDNMatcher(patterns []string) (*FQDNMatcher, error) {
	var sb strings.Builder
	sb.WriteString(`\A(?:`) // 시작 앵커

	for i, pattern := range patterns {
		if i > 0 {
			sb.WriteByte('|') // OR
		}
		if err := appendFQDNPatternRegexp(&sb, pattern); err != nil {
			return nil, fmt.Errorf("invalid pattern %q: %w", pattern, err)
		}
	}

	sb.WriteString(`)\z`) // 끝 앵커

	regexStr := sb.String()
	regex, err := regexp.Compile(regexStr)
	if err != nil {
		return nil, fmt.Errorf("failed to compile regex: %w", err)
	}

	return &FQDNMatcher{
		patterns: patterns,
		regex:    regex,
	}, nil
}

// appendFQDNPatternRegexp는 FQDN 패턴을 정규식으로 변환합니다.
// 실제 Hubble: pkg/hubble/filters/patterns.go
//
// 변환 규칙:
//   * → [-.0-9a-z]*  (와일드카드)
//   . → \.           (리터럴 점)
//   기타 → 그대로
func appendFQDNPatternRegexp(sb *strings.Builder, pattern string) error {
	// 정규화: 후행 점 제거, 소문자 변환
	pattern = strings.TrimSuffix(strings.ToLower(pattern), ".")

	for _, r := range pattern {
		switch {
		case r == '*':
			sb.WriteString(`[-.0-9a-z]*`)
		case r == '.':
			sb.WriteString(`\.`)
		case ('0' <= r && r <= '9') || ('a' <= r && r <= 'z') || r == '-' || r == '_':
			sb.WriteRune(r)
		default:
			return fmt.Errorf("invalid character %q in FQDN pattern", r)
		}
	}
	return nil
}

// Match는 FQDN이 패턴에 매치하는지 확인합니다.
func (m *FQDNMatcher) Match(fqdn string) bool {
	// 정규화
	fqdn = strings.TrimSuffix(strings.ToLower(fqdn), ".")
	return m.regex.MatchString(fqdn)
}

// RegexString은 컴파일된 정규식 문자열을 반환합니다.
func (m *FQDNMatcher) RegexString() string {
	return m.regex.String()
}

// ========================================
// 2. DNS Query 필터 (L7 DNS 필터)
// ========================================

// DNSQueryFilter는 DNS 질의를 정규식으로 필터링합니다.
// 실제 Hubble: --dns-query 플래그
type DNSQueryFilter struct {
	patterns []*regexp.Regexp
}

func NewDNSQueryFilter(patterns []string) (*DNSQueryFilter, error) {
	var compiled []*regexp.Regexp
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("invalid DNS query pattern %q: %w", p, err)
		}
		compiled = append(compiled, re)
	}
	return &DNSQueryFilter{patterns: compiled}, nil
}

func (f *DNSQueryFilter) Match(query string) bool {
	for _, re := range f.patterns {
		if re.MatchString(query) {
			return true
		}
	}
	return false
}

// ========================================
// 3. 노드 이름 패턴 (cluster/node 형식)
// ========================================

// NodeNameMatcher는 "cluster/node" 형식의 노드 이름을 매칭합니다.
// 실제 Hubble: --node-name 플래그
type NodeNameMatcher struct {
	clusterPattern *regexp.Regexp
	nodePattern    *regexp.Regexp
}

func NewNodeNameMatcher(pattern string) (*NodeNameMatcher, error) {
	parts := strings.SplitN(pattern, "/", 2)

	var clusterPat, nodePat string
	switch len(parts) {
	case 1:
		nodePat = parts[0]
	case 2:
		clusterPat = parts[0]
		nodePat = parts[1]
	}

	m := &NodeNameMatcher{}

	if clusterPat != "" {
		re, err := compileWildcard(clusterPat)
		if err != nil {
			return nil, err
		}
		m.clusterPattern = re
	}

	re, err := compileWildcard(nodePat)
	if err != nil {
		return nil, err
	}
	m.nodePattern = re

	return m, nil
}

func compileWildcard(pattern string) (*regexp.Regexp, error) {
	var sb strings.Builder
	sb.WriteString(`\A`)
	for _, r := range pattern {
		switch r {
		case '*':
			sb.WriteString(`[-.0-9a-z]*`)
		case '.':
			sb.WriteString(`\.`)
		default:
			sb.WriteRune(r)
		}
	}
	sb.WriteString(`\z`)
	return regexp.Compile(sb.String())
}

func (m *NodeNameMatcher) Match(cluster, node string) bool {
	if m.clusterPattern != nil && !m.clusterPattern.MatchString(cluster) {
		return false
	}
	return m.nodePattern.MatchString(node)
}

// ========================================
// 4. 실행
// ========================================

func main() {
	fmt.Println("=== PoC: Hubble FQDN 패턴 매칭 ===")
	fmt.Println()
	fmt.Println("FQDN 와일드카드 변환 규칙:")
	fmt.Println("  *  → [-.0-9a-z]*  (0개 이상 문자)")
	fmt.Println("  .  → \\.           (리터럴 점)")
	fmt.Println("  여러 패턴은 | (OR)로 결합")
	fmt.Println("  앵커: \\A...\\z (전체 매치)")
	fmt.Println()

	// ── 시나리오 1: 와일드카드 매칭 ──
	fmt.Println("━━━ 시나리오 1: 와일드카드 매칭 ━━━")
	fmt.Println()

	patterns1 := []string{"*.google.com"}
	matcher1, _ := NewFQDNMatcher(patterns1)
	fmt.Printf("  패턴: %v\n", patterns1)
	fmt.Printf("  정규식: %s\n\n", matcher1.RegexString())

	testFQDNs1 := []string{
		"maps.google.com",
		"api.google.com",
		"google.com",
		"evil.google.com.attacker.com",
		"maps.google.co.kr",
	}

	for _, fqdn := range testFQDNs1 {
		match := matcher1.Match(fqdn)
		mark := "✗"
		if match {
			mark = "✓"
		}
		fmt.Printf("  %s %-40s\n", mark, fqdn)
	}
	fmt.Println()

	// ── 시나리오 2: 서비스 디스커버리 패턴 ──
	fmt.Println("━━━ 시나리오 2: K8s 서비스 패턴 ━━━")
	fmt.Println()

	patterns2 := []string{"*.*.svc.cluster.local"}
	matcher2, _ := NewFQDNMatcher(patterns2)
	fmt.Printf("  패턴: %v\n", patterns2)
	fmt.Printf("  정규식: %s\n\n", matcher2.RegexString())

	testFQDNs2 := []string{
		"backend-api.default.svc.cluster.local",
		"coredns.kube-system.svc.cluster.local",
		"my-service.svc.cluster.local",
		"external.api.com",
	}

	for _, fqdn := range testFQDNs2 {
		match := matcher2.Match(fqdn)
		mark := "✗"
		if match {
			mark = "✓"
		}
		fmt.Printf("  %s %-50s\n", mark, fqdn)
	}
	fmt.Println()

	// ── 시나리오 3: 복합 패턴 (OR) ──
	fmt.Println("━━━ 시나리오 3: 복합 패턴 (OR 결합) ━━━")
	fmt.Println()

	patterns3 := []string{"*.google.com", "*.github.com", "api.example.com"}
	matcher3, _ := NewFQDNMatcher(patterns3)
	fmt.Printf("  패턴: %v\n", patterns3)
	fmt.Printf("  정규식: %s\n\n", matcher3.RegexString())

	testFQDNs3 := []string{
		"maps.google.com",
		"api.github.com",
		"api.example.com",
		"www.example.com",
		"docs.github.com",
	}

	for _, fqdn := range testFQDNs3 {
		match := matcher3.Match(fqdn)
		mark := "✗"
		if match {
			mark = "✓"
		}
		fmt.Printf("  %s %-40s\n", mark, fqdn)
	}
	fmt.Println()

	// ── 시나리오 4: DNS Query 필터 (정규식) ──
	fmt.Println("━━━ 시나리오 4: DNS Query 정규식 필터 ━━━")
	fmt.Println()

	dnsFilter, _ := NewDNSQueryFilter([]string{
		`.*\.google\.com$`,
		`^api\.`,
	})

	queries := []string{
		"maps.google.com",
		"api.backend.svc.cluster.local",
		"www.example.com",
		"cdn.google.com",
		"api.github.com",
	}

	for _, q := range queries {
		match := dnsFilter.Match(q)
		mark := "✗"
		if match {
			mark = "✓"
		}
		fmt.Printf("  %s DNS query: %-40s\n", mark, q)
	}
	fmt.Println()

	// ── 시나리오 5: 노드 이름 패턴 ──
	fmt.Println("━━━ 시나리오 5: 노드 이름 패턴 (cluster/node) ━━━")
	fmt.Println()

	nodeMatcher, _ := NewNodeNameMatcher("prod-*/k8s-node-*")
	fmt.Println("  패턴: prod-*/k8s-node-*")
	fmt.Println()

	nodes := []struct {
		cluster string
		node    string
	}{
		{"prod-us", "k8s-node-1"},
		{"prod-eu", "k8s-node-2"},
		{"staging", "k8s-node-1"},
		{"prod-ap", "worker-3"},
	}

	for _, n := range nodes {
		match := nodeMatcher.Match(n.cluster, n.node)
		mark := "✗"
		if match {
			mark = "✓"
		}
		fmt.Printf("  %s %s/%s\n", mark, n.cluster, n.node)
	}

	fmt.Println()
	fmt.Println("핵심 포인트:")
	fmt.Println("  - 와일드카드 → 정규식 변환: * → [-.0-9a-z]*")
	fmt.Println("  - 앵커(\\A, \\z)로 부분 매치 방지")
	fmt.Println("  - 여러 패턴을 | (OR)로 결합하여 단일 정규식으로 컴파일")
	fmt.Println("  - 실제 Hubble: --fqdn, --dns-query, --node-name 플래그")
	fmt.Println("  - 보안: evil.google.com.attacker.com 같은 서브도메인 위장 방지")
}
