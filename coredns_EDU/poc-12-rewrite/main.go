package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// =============================================================================
// PoC 12: Rewrite 규칙 (Rewrite Rules)
// =============================================================================
// CoreDNS의 rewrite 플러그인이 DNS 요청/응답을 재작성하는 메커니즘을 시뮬레이션한다.
// 쿼리 이름의 exact/prefix/suffix/substring/regex 매칭과 응답 재작성을 구현한다.
//
// 참조: coredns/plugin/rewrite/rewrite.go
//       - Rewrite 구조체: Rules []Rule, ServeDNS()에서 규칙 순차 적용
//       - Rule 인터페이스: Rewrite(), Mode() (stop/continue)
//       coredns/plugin/rewrite/name.go
//       - exactNameRule, prefixNameRule, suffixNameRule, regexNameRule
//       - stringRewriter: regexStringRewriter, remapStringRewriter
// =============================================================================

// =============================================================================
// 결과 타입 및 처리 모드
// =============================================================================

// RewriteResult는 재작성 결과를 나타낸다.
type RewriteResult int

const (
	// RewriteIgnored는 규칙이 적용되지 않았음을 나타낸다.
	RewriteIgnored RewriteResult = iota
	// RewriteDone은 재작성이 수행되었음을 나타낸다.
	RewriteDone
)

// 처리 모드: CoreDNS의 rewrite.go에서 정의된 것과 동일
const (
	ModeStop     = "stop"     // 이 규칙 적용 후 중단
	ModeContinue = "continue" // 다음 규칙도 계속 적용
)

// =============================================================================
// DNS 요청/응답 구조
// =============================================================================

// DNSQuestion은 DNS 질문 섹션을 나타낸다.
type DNSQuestion struct {
	Name  string // 쿼리 이름 (예: www.example.com.)
	QType string // 쿼리 타입 (예: A, AAAA, CNAME)
}

// DNSRecord는 DNS 응답 레코드를 나타낸다.
type DNSRecord struct {
	Name  string // 레코드 이름
	Type  string // 레코드 타입
	TTL   uint32
	Value string // 레코드 값 (IP 주소, CNAME 대상 등)
}

// DNSRequest는 DNS 요청을 나타낸다.
type DNSRequest struct {
	Question DNSQuestion
}

// DNSResponse는 DNS 응답을 나타낸다.
type DNSResponse struct {
	Question DNSQuestion
	Answers  []DNSRecord
}

// =============================================================================
// Rule 인터페이스 및 구현
// CoreDNS의 Rule 인터페이스를 재현한다.
// =============================================================================

// Rule은 재작성 규칙 인터페이스이다.
type Rule interface {
	// Rewrite는 요청을 재작성한다.
	Rewrite(req *DNSRequest) RewriteResult
	// RewriteResponse는 응답을 재작성한다.
	RewriteResponse(resp *DNSResponse)
	// Mode는 처리 모드(stop/continue)를 반환한다.
	Mode() string
	// String은 규칙의 문자열 표현을 반환한다.
	String() string
}

// =============================================================================
// Exact Match 규칙
// CoreDNS의 exactNameRule에 해당한다.
// =============================================================================

type exactRule struct {
	mode        string
	from        string
	to          string
	autoReverse bool
}

func NewExactRule(mode, from, to string) Rule {
	return &exactRule{
		mode:        mode,
		from:        normalize(from),
		to:          normalize(to),
		autoReverse: true,
	}
}

func (r *exactRule) Rewrite(req *DNSRequest) RewriteResult {
	if req.Question.Name == r.from {
		req.Question.Name = r.to
		return RewriteDone
	}
	return RewriteIgnored
}

func (r *exactRule) RewriteResponse(resp *DNSResponse) {
	if !r.autoReverse {
		return
	}
	// 응답에서 이름을 원래대로 되돌림
	for i := range resp.Answers {
		if resp.Answers[i].Name == r.to {
			resp.Answers[i].Name = r.from
		}
		if resp.Answers[i].Value == r.to {
			resp.Answers[i].Value = r.from
		}
	}
}

func (r *exactRule) Mode() string  { return r.mode }
func (r *exactRule) String() string { return fmt.Sprintf("exact %s → %s [%s]", r.from, r.to, r.mode) }

// =============================================================================
// Prefix Match 규칙
// CoreDNS의 prefixNameRule에 해당한다.
// =============================================================================

type prefixRule struct {
	mode   string
	prefix string
	to     string
}

func NewPrefixRule(mode, prefix, replacement string) Rule {
	return &prefixRule{
		mode:   mode,
		prefix: prefix,
		to:     replacement,
	}
}

func (r *prefixRule) Rewrite(req *DNSRequest) RewriteResult {
	if after, ok := strings.CutPrefix(req.Question.Name, r.prefix); ok {
		req.Question.Name = r.to + after
		return RewriteDone
	}
	return RewriteIgnored
}

func (r *prefixRule) RewriteResponse(resp *DNSResponse) {
	// 응답에서 prefix 되돌림
	for i := range resp.Answers {
		if after, ok := strings.CutPrefix(resp.Answers[i].Name, r.to); ok {
			resp.Answers[i].Name = r.prefix + after
		}
	}
}

func (r *prefixRule) Mode() string { return r.mode }
func (r *prefixRule) String() string {
	return fmt.Sprintf("prefix %s → %s [%s]", r.prefix, r.to, r.mode)
}

// =============================================================================
// Suffix Match 규칙
// CoreDNS의 suffixNameRule에 해당한다.
// =============================================================================

type suffixRule struct {
	mode   string
	suffix string
	to     string
}

func NewSuffixRule(mode, suffix, replacement string) Rule {
	return &suffixRule{
		mode:   mode,
		suffix: suffix,
		to:     replacement,
	}
}

func (r *suffixRule) Rewrite(req *DNSRequest) RewriteResult {
	if before, ok := strings.CutSuffix(req.Question.Name, r.suffix); ok {
		req.Question.Name = before + r.to
		return RewriteDone
	}
	return RewriteIgnored
}

func (r *suffixRule) RewriteResponse(resp *DNSResponse) {
	for i := range resp.Answers {
		if before, ok := strings.CutSuffix(resp.Answers[i].Name, r.to); ok {
			resp.Answers[i].Name = before + r.suffix
		}
	}
}

func (r *suffixRule) Mode() string { return r.mode }
func (r *suffixRule) String() string {
	return fmt.Sprintf("suffix %s → %s [%s]", r.suffix, r.to, r.mode)
}

// =============================================================================
// Regex Match 규칙
// CoreDNS의 regexNameRule에 해당한다.
// =============================================================================

type regexRule struct {
	mode        string
	pattern     *regexp.Regexp
	replacement string
}

func NewRegexRule(mode, pattern, replacement string) (Rule, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("정규식 컴파일 오류: %v", err)
	}
	return &regexRule{
		mode:        mode,
		pattern:     re,
		replacement: replacement,
	}, nil
}

func (r *regexRule) Rewrite(req *DNSRequest) RewriteResult {
	groups := r.pattern.FindStringSubmatch(req.Question.Name)
	if len(groups) == 0 {
		return RewriteIgnored
	}

	// CoreDNS의 regexNameRule.Rewrite()와 동일한 치환 로직
	// {0}=전체매치, {1}=첫번째 그룹, ...
	result := r.replacement
	for i, g := range groups {
		placeholder := "{" + strconv.Itoa(i) + "}"
		result = strings.ReplaceAll(result, placeholder, g)
	}
	req.Question.Name = result
	return RewriteDone
}

func (r *regexRule) RewriteResponse(resp *DNSResponse) {
	// 정규식 역방향 재작성은 복잡하므로 간단히 처리
}

func (r *regexRule) Mode() string { return r.mode }
func (r *regexRule) String() string {
	return fmt.Sprintf("regex /%s/ → %s [%s]", r.pattern.String(), r.replacement, r.mode)
}

// =============================================================================
// Substring Match 규칙
// CoreDNS의 substringNameRule에 해당한다.
// =============================================================================

type substringRule struct {
	mode      string
	substring string
	to        string
}

func NewSubstringRule(mode, substring, replacement string) Rule {
	return &substringRule{
		mode:      mode,
		substring: substring,
		to:        replacement,
	}
}

func (r *substringRule) Rewrite(req *DNSRequest) RewriteResult {
	if strings.Contains(req.Question.Name, r.substring) {
		req.Question.Name = strings.ReplaceAll(req.Question.Name, r.substring, r.to)
		return RewriteDone
	}
	return RewriteIgnored
}

func (r *substringRule) RewriteResponse(resp *DNSResponse) {
	for i := range resp.Answers {
		resp.Answers[i].Name = strings.ReplaceAll(resp.Answers[i].Name, r.to, r.substring)
	}
}

func (r *substringRule) Mode() string { return r.mode }
func (r *substringRule) String() string {
	return fmt.Sprintf("substring %s → %s [%s]", r.substring, r.to, r.mode)
}

// =============================================================================
// Rewrite 엔진
// CoreDNS의 Rewrite.ServeDNS()의 규칙 적용 로직을 재현한다.
// =============================================================================

// RewriteEngine은 규칙 체인을 관리하고 적용하는 엔진이다.
type RewriteEngine struct {
	Rules []Rule
}

// NewRewriteEngine은 새로운 Rewrite 엔진을 생성한다.
func NewRewriteEngine() *RewriteEngine {
	return &RewriteEngine{}
}

// AddRule은 규칙을 엔진에 추가한다.
func (e *RewriteEngine) AddRule(rule Rule) {
	e.Rules = append(e.Rules, rule)
}

// ProcessRequest는 요청에 대해 모든 규칙을 적용한다.
// CoreDNS의 ServeDNS()에서 규칙을 순차 적용하는 로직과 동일하다.
func (e *RewriteEngine) ProcessRequest(req *DNSRequest) (appliedRules []Rule) {
	for _, rule := range e.Rules {
		result := rule.Rewrite(req)
		if result == RewriteDone {
			appliedRules = append(appliedRules, rule)
			// stop 모드면 즉시 중단
			if rule.Mode() == ModeStop {
				break
			}
			// continue 모드면 다음 규칙도 적용
		}
	}
	return appliedRules
}

// ProcessResponse는 응답에 대해 적용된 규칙의 역방향 재작성을 수행한다.
func (e *RewriteEngine) ProcessResponse(resp *DNSResponse, appliedRules []Rule) {
	// 역순으로 응답 재작성 (마지막에 적용된 규칙부터)
	for i := len(appliedRules) - 1; i >= 0; i-- {
		appliedRules[i].RewriteResponse(resp)
	}
}

// =============================================================================
// 헬퍼 함수
// =============================================================================

func normalize(name string) string {
	if name != "" && !strings.HasSuffix(name, ".") {
		return name + "."
	}
	return name
}

// =============================================================================
// 메인
// =============================================================================

func main() {
	fmt.Println("=== CoreDNS Rewrite 규칙 (Rewrite Rules) PoC ===")
	fmt.Println()

	// =========================================================================
	// 1. Exact Match 재작성
	// =========================================================================
	fmt.Println("--- 1. Exact Match 재작성 ---")

	engine := NewRewriteEngine()
	engine.AddRule(NewExactRule(ModeStop,
		"old-app.example.com.", "new-app.example.com."))

	testQueries := []string{
		"old-app.example.com.",
		"other.example.com.",
		"old-app.example.org.",
	}

	for _, q := range testQueries {
		req := &DNSRequest{Question: DNSQuestion{Name: q, QType: "A"}}
		original := req.Question.Name
		applied := engine.ProcessRequest(req)
		if len(applied) > 0 {
			fmt.Printf("  [재작성] %s → %s (규칙: %s)\n", original, req.Question.Name, applied[0])
		} else {
			fmt.Printf("  [무시됨] %s (변경 없음)\n", original)
		}
	}

	// =========================================================================
	// 2. Prefix Match 재작성
	// =========================================================================
	fmt.Println()
	fmt.Println("--- 2. Prefix Match 재작성 ---")

	engine = NewRewriteEngine()
	engine.AddRule(NewPrefixRule(ModeStop, "staging-", "prod-"))

	prefixQueries := []string{
		"staging-api.example.com.",
		"staging-web.example.com.",
		"prod-api.example.com.",
	}

	for _, q := range prefixQueries {
		req := &DNSRequest{Question: DNSQuestion{Name: q, QType: "A"}}
		original := req.Question.Name
		applied := engine.ProcessRequest(req)
		if len(applied) > 0 {
			fmt.Printf("  [재작성] %s → %s\n", original, req.Question.Name)
		} else {
			fmt.Printf("  [무시됨] %s\n", original)
		}
	}

	// =========================================================================
	// 3. Suffix Match 재작성
	// =========================================================================
	fmt.Println()
	fmt.Println("--- 3. Suffix Match 재작성 ---")

	engine = NewRewriteEngine()
	engine.AddRule(NewSuffixRule(ModeStop,
		".internal.example.com.", ".example.com."))

	suffixQueries := []string{
		"api.internal.example.com.",
		"db.internal.example.com.",
		"web.example.com.",
	}

	for _, q := range suffixQueries {
		req := &DNSRequest{Question: DNSQuestion{Name: q, QType: "A"}}
		original := req.Question.Name
		applied := engine.ProcessRequest(req)
		if len(applied) > 0 {
			fmt.Printf("  [재작성] %s → %s\n", original, req.Question.Name)
		} else {
			fmt.Printf("  [무시됨] %s\n", original)
		}
	}

	// =========================================================================
	// 4. Regex Match 재작성
	// =========================================================================
	fmt.Println()
	fmt.Println("--- 4. Regex Match 재작성 ---")

	engine = NewRewriteEngine()

	// {1}은 첫 번째 캡처 그룹
	regexR, err := NewRegexRule(ModeStop,
		`^(.+)\.us-west-2\.example\.com\.$`,
		`{1}.us-east-1.example.com.`)
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
		return
	}
	engine.AddRule(regexR)

	regexQueries := []string{
		"api.us-west-2.example.com.",
		"db.us-west-2.example.com.",
		"web.us-east-1.example.com.",
		"app.eu-west-1.example.com.",
	}

	for _, q := range regexQueries {
		req := &DNSRequest{Question: DNSQuestion{Name: q, QType: "A"}}
		original := req.Question.Name
		applied := engine.ProcessRequest(req)
		if len(applied) > 0 {
			fmt.Printf("  [재작성] %s\n           → %s\n", original, req.Question.Name)
		} else {
			fmt.Printf("  [무시됨] %s\n", original)
		}
	}

	// =========================================================================
	// 5. Substring Match 재작성
	// =========================================================================
	fmt.Println()
	fmt.Println("--- 5. Substring Match 재작성 ---")

	engine = NewRewriteEngine()
	engine.AddRule(NewSubstringRule(ModeStop, ".legacy.", ".modern."))

	substringQueries := []string{
		"api.legacy.corp.com.",
		"db.legacy.corp.com.",
		"web.modern.corp.com.",
	}

	for _, q := range substringQueries {
		req := &DNSRequest{Question: DNSQuestion{Name: q, QType: "A"}}
		original := req.Question.Name
		applied := engine.ProcessRequest(req)
		if len(applied) > 0 {
			fmt.Printf("  [재작성] %s → %s\n", original, req.Question.Name)
		} else {
			fmt.Printf("  [무시됨] %s\n", original)
		}
	}

	// =========================================================================
	// 6. 규칙 체인 (Continue 모드)
	// =========================================================================
	fmt.Println()
	fmt.Println("--- 6. 규칙 체인 (Continue 모드) ---")
	fmt.Println("  규칙 체인 (순서대로 적용):")

	engine = NewRewriteEngine()

	// 규칙 1: prefix 변환 (continue → 다음 규칙도 적용)
	rule1 := NewPrefixRule(ModeContinue, "staging-", "prod-")
	engine.AddRule(rule1)
	fmt.Printf("    1. %s\n", rule1)

	// 규칙 2: suffix 변환 (continue)
	rule2 := NewSuffixRule(ModeContinue, ".internal.corp.", ".corp.")
	engine.AddRule(rule2)
	fmt.Printf("    2. %s\n", rule2)

	// 규칙 3: substring 변환 (stop → 여기서 중단)
	rule3 := NewSubstringRule(ModeStop, ".corp.", ".example.com.")
	engine.AddRule(rule3)
	fmt.Printf("    3. %s\n", rule3)

	fmt.Println()

	chainQueries := []string{
		"staging-api.internal.corp.",
		"staging-web.internal.corp.",
		"prod-db.corp.",
		"other.example.org.",
	}

	for _, q := range chainQueries {
		req := &DNSRequest{Question: DNSQuestion{Name: q, QType: "A"}}
		original := req.Question.Name
		applied := engine.ProcessRequest(req)
		if len(applied) > 0 {
			fmt.Printf("  입력: %s\n", original)
			for i, r := range applied {
				fmt.Printf("    규칙 %d 적용: %s\n", i+1, r)
			}
			fmt.Printf("    최종: %s\n", req.Question.Name)
		} else {
			fmt.Printf("  입력: %s → 매칭 규칙 없음\n", original)
		}
		fmt.Println()
	}

	// =========================================================================
	// 7. 응답 재작성 (Response Rewrite)
	// =========================================================================
	fmt.Println("--- 7. 응답 재작성 (Response Rewrite) ---")

	engine = NewRewriteEngine()
	engine.AddRule(NewExactRule(ModeStop,
		"alias.example.com.", "real-server.backend.com."))

	// 요청 재작성
	req := &DNSRequest{Question: DNSQuestion{Name: "alias.example.com.", QType: "A"}}
	originalName := req.Question.Name
	applied := engine.ProcessRequest(req)
	fmt.Printf("  요청 재작성: %s → %s\n", originalName, req.Question.Name)

	// 백엔드에서 응답이 왔다고 가정
	resp := &DNSResponse{
		Question: DNSQuestion{Name: req.Question.Name, QType: "A"},
		Answers: []DNSRecord{
			{Name: "real-server.backend.com.", Type: "A", TTL: 300, Value: "10.0.1.5"},
			{Name: "real-server.backend.com.", Type: "A", TTL: 300, Value: "10.0.1.6"},
		},
	}

	fmt.Println("  백엔드 응답 (재작성 전):")
	for _, a := range resp.Answers {
		fmt.Printf("    %s %d IN %s %s\n", a.Name, a.TTL, a.Type, a.Value)
	}

	// 응답 재작성 (이름을 원래대로 되돌림)
	engine.ProcessResponse(resp, applied)

	fmt.Println("  클라이언트 응답 (재작성 후):")
	for _, a := range resp.Answers {
		fmt.Printf("    %s %d IN %s %s\n", a.Name, a.TTL, a.Type, a.Value)
	}

	// =========================================================================
	// 8. Stop vs Continue 동작 비교
	// =========================================================================
	fmt.Println()
	fmt.Println("--- 8. Stop vs Continue 동작 비교 ---")

	// Stop 모드: 첫 번째 매칭 규칙에서 중단
	fmt.Println("  [Stop 모드]")
	stopEngine := NewRewriteEngine()
	stopEngine.AddRule(NewPrefixRule(ModeStop, "test-", "dev-"))
	stopEngine.AddRule(NewSuffixRule(ModeStop, ".local.", ".prod."))

	stopReq := &DNSRequest{Question: DNSQuestion{Name: "test-api.local.", QType: "A"}}
	stopApplied := stopEngine.ProcessRequest(stopReq)
	fmt.Printf("    입력: test-api.local.\n")
	fmt.Printf("    적용된 규칙 수: %d\n", len(stopApplied))
	fmt.Printf("    결과: %s\n", stopReq.Question.Name)

	// Continue 모드: 모든 매칭 규칙 적용
	fmt.Println("  [Continue 모드]")
	contEngine := NewRewriteEngine()
	contEngine.AddRule(NewPrefixRule(ModeContinue, "test-", "dev-"))
	contEngine.AddRule(NewSuffixRule(ModeStop, ".local.", ".prod."))

	contReq := &DNSRequest{Question: DNSQuestion{Name: "test-api.local.", QType: "A"}}
	contApplied := contEngine.ProcessRequest(contReq)
	fmt.Printf("    입력: test-api.local.\n")
	fmt.Printf("    적용된 규칙 수: %d\n", len(contApplied))
	fmt.Printf("    결과: %s\n", contReq.Question.Name)
}
