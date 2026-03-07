package main

import (
	"fmt"
	"net"
	"strings"
)

// =============================================================================
// Istio Authorization Policy 평가 시뮬레이션
//
// 실제 Istio 소스 참조:
//   - pilot/pkg/security/authz/model/model.go: Model, rule, ruleList, Generate()
//   - pilot/pkg/security/authz/builder/builder.go: Builder, build(), CUSTOM/DENY/ALLOW 순서
//   - pilot/pkg/security/authz/model/generator.go: 각 조건별 generator
//   - pilot/pkg/security/authz/model/permission.go: permission AND/OR/NOT 조합
//   - pilot/pkg/security/authz/model/principal.go: principal AND/OR/NOT 조합
//
// 핵심 알고리즘:
// 1. 정책 평가 순서: CUSTOM -> DENY -> ALLOW -> 기본(deny-all 또는 allow-all)
// 2. 각 정책의 Rule 내부:
//    - Source(principals, namespaces, IPs) 조건은 OR로 결합 (from[])
//    - Operation(hosts, paths, methods) 조건은 OR로 결합 (to[])
//    - When 조건은 AND로 결합
//    - Source 내부 필드는 AND로 결합
//    - Source 필드의 개별 값은 OR로 결합
// 3. 같은 액션의 여러 정책: 하나라도 매칭되면 해당 액션 적용
// =============================================================================

// --- 정책 액션 ---
type PolicyAction int

const (
	ActionAllow  PolicyAction = 0
	ActionDeny   PolicyAction = 1
	ActionCustom PolicyAction = 2
)

func (a PolicyAction) String() string {
	switch a {
	case ActionAllow:
		return "ALLOW"
	case ActionDeny:
		return "DENY"
	case ActionCustom:
		return "CUSTOM"
	}
	return "UNKNOWN"
}

// --- 요청 컨텍스트 ---
// Envoy에서 전달받는 요청 정보
type RequestContext struct {
	// Source 정보
	SourcePrincipal string // SPIFFE ID (e.g., "cluster.local/ns/default/sa/productpage")
	SourceNamespace string
	SourceIP        string

	// Request 정보
	Host    string
	Path    string
	Method  string
	Headers map[string]string
}

func (r RequestContext) String() string {
	return fmt.Sprintf("{principal=%s, ns=%s, ip=%s, host=%s, path=%s, method=%s}",
		r.SourcePrincipal, r.SourceNamespace, r.SourceIP,
		r.Host, r.Path, r.Method)
}

// --- Source 매칭 조건 ---
// 실제 Istio의 authzpb.Rule.From.Source에 대응
type Source struct {
	Principals     []string // 허용할 principal 목록
	NotPrincipals  []string // 제외할 principal 목록
	Namespaces     []string // 허용할 네임스페이스
	NotNamespaces  []string // 제외할 네임스페이스
	IPBlocks       []string // 허용할 IP 대역 (CIDR 지원)
	NotIPBlocks    []string // 제외할 IP 대역
}

// --- Operation 매칭 조건 ---
// 실제 Istio의 authzpb.Rule.To.Operation에 대응
type Operation struct {
	Hosts      []string // 허용할 호스트
	NotHosts   []string // 제외할 호스트
	Paths      []string // 허용할 경로 (prefix/suffix 와일드카드 지원)
	NotPaths   []string // 제외할 경로
	Methods    []string // 허용할 HTTP 메서드
	NotMethods []string // 제외할 HTTP 메서드
}

// --- When 조건 ---
// 실제 Istio의 authzpb.Condition에 대응
type Condition struct {
	Key       string   // 예: "request.headers[x-token]"
	Values    []string // 매칭할 값 목록 (OR)
	NotValues []string // 제외할 값 목록
}

// --- 정책 규칙 ---
// 실제 Istio의 authzpb.Rule에 대응
// From[]: OR로 결합, To[]: OR로 결합, When[]: AND로 결합
type Rule struct {
	From []Source    // 소스 조건 (OR 결합)
	To   []Operation // 오퍼레이션 조건 (OR 결합)
	When []Condition // 추가 조건 (AND 결합)
}

// --- Authorization Policy ---
// 실제 Istio의 AuthorizationPolicy CRD에 대응
type AuthorizationPolicy struct {
	Name      string
	Namespace string
	Action    PolicyAction
	Rules     []Rule
	Provider  string // CUSTOM 액션일 때 외부 제공자
}

// =============================================================================
// 매칭 함수들 (실제 Istio의 authz/matcher 패키지 참조)
// =============================================================================

// matchString은 문자열 패턴 매칭 (prefix/suffix 와일드카드 지원)
// 실제: authz/matcher/string.go의 StringMatcher
func matchString(pattern, value string) bool {
	if pattern == "*" {
		return true
	}
	// prefix 와일드카드: "*.example.com"
	if strings.HasPrefix(pattern, "*") {
		return strings.HasSuffix(value, pattern[1:])
	}
	// suffix 와일드카드: "/api/*"
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(value, pattern[:len(pattern)-1])
	}
	return pattern == value
}

// matchIPBlock은 IP CIDR 블록 매칭
func matchIPBlock(block, ip string) bool {
	// 단일 IP
	if !strings.Contains(block, "/") {
		return block == ip
	}
	// CIDR 블록
	_, cidr, err := net.ParseCIDR(block)
	if err != nil {
		return false
	}
	return cidr.Contains(net.ParseIP(ip))
}

// matchStringList는 문자열 목록 중 하나라도 매칭되면 true (OR 로직)
func matchStringList(patterns []string, value string) bool {
	if len(patterns) == 0 {
		return true // 비어있으면 모두 매칭
	}
	for _, p := range patterns {
		if matchString(p, value) {
			return true
		}
	}
	return false
}

// matchIPBlockList는 IP 블록 목록 중 하나라도 매칭되면 true
func matchIPBlockList(blocks []string, ip string) bool {
	if len(blocks) == 0 {
		return true
	}
	for _, b := range blocks {
		if matchIPBlock(b, ip) {
			return true
		}
	}
	return false
}

// notMatchStringList는 제외 목록에 하나라도 매칭되면 false
func notMatchStringList(patterns []string, value string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		if matchString(p, value) {
			return false
		}
	}
	return true
}

// notMatchIPBlockList는 제외 IP 목록에 하나라도 매칭되면 false
func notMatchIPBlockList(blocks []string, ip string) bool {
	if len(blocks) == 0 {
		return true
	}
	for _, b := range blocks {
		if matchIPBlock(b, ip) {
			return false
		}
	}
	return true
}

// =============================================================================
// 규칙 매칭 (실제 Istio의 Model.Generate() 로직)
// =============================================================================

// matchSource는 단일 Source 조건 매칭
// 실제: model.go의 generatePrincipal() - Source 내부 필드는 AND로 결합
func matchSource(src Source, req RequestContext) bool {
	// Principals: values는 OR, notValues는 AND(모두 비매칭)
	if !matchStringList(src.Principals, req.SourcePrincipal) {
		return false
	}
	if !notMatchStringList(src.NotPrincipals, req.SourcePrincipal) {
		return false
	}

	// Namespaces
	if !matchStringList(src.Namespaces, req.SourceNamespace) {
		return false
	}
	if !notMatchStringList(src.NotNamespaces, req.SourceNamespace) {
		return false
	}

	// IP Blocks
	if !matchIPBlockList(src.IPBlocks, req.SourceIP) {
		return false
	}
	if !notMatchIPBlockList(src.NotIPBlocks, req.SourceIP) {
		return false
	}

	return true
}

// matchOperation은 단일 Operation 조건 매칭
// 실제: model.go의 generatePermission() - Operation 내부 필드는 AND로 결합
func matchOperation(op Operation, req RequestContext) bool {
	// Hosts
	if !matchStringList(op.Hosts, req.Host) {
		return false
	}
	if !notMatchStringList(op.NotHosts, req.Host) {
		return false
	}

	// Paths
	if !matchStringList(op.Paths, req.Path) {
		return false
	}
	if !notMatchStringList(op.NotPaths, req.Path) {
		return false
	}

	// Methods
	if !matchStringList(op.Methods, req.Method) {
		return false
	}
	if !notMatchStringList(op.NotMethods, req.Method) {
		return false
	}

	return true
}

// matchCondition은 When 조건 매칭
func matchCondition(cond Condition, req RequestContext) bool {
	value := ""
	// request.headers[X] 형식 파싱
	if strings.HasPrefix(cond.Key, "request.headers[") && strings.HasSuffix(cond.Key, "]") {
		headerName := cond.Key[len("request.headers[") : len(cond.Key)-1]
		value = req.Headers[strings.ToLower(headerName)]
	}

	// Values 매칭 (OR)
	if len(cond.Values) > 0 {
		matched := false
		for _, v := range cond.Values {
			if matchString(v, value) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	// NotValues 매칭
	if len(cond.NotValues) > 0 {
		for _, v := range cond.NotValues {
			if matchString(v, value) {
				return false
			}
		}
	}

	return true
}

// matchRule은 단일 Rule 매칭
// From[]: OR (하나라도 매칭되면 통과), 비어있으면 모든 소스 허용
// To[]: OR (하나라도 매칭되면 통과), 비어있으면 모든 오퍼레이션 허용
// When[]: AND (모두 매칭되어야 통과)
func matchRule(rule Rule, req RequestContext) bool {
	// From 매칭 (OR)
	if len(rule.From) > 0 {
		fromMatched := false
		for _, src := range rule.From {
			if matchSource(src, req) {
				fromMatched = true
				break
			}
		}
		if !fromMatched {
			return false
		}
	}

	// To 매칭 (OR)
	if len(rule.To) > 0 {
		toMatched := false
		for _, op := range rule.To {
			if matchOperation(op, req) {
				toMatched = true
				break
			}
		}
		if !toMatched {
			return false
		}
	}

	// When 매칭 (AND)
	for _, cond := range rule.When {
		if !matchCondition(cond, req) {
			return false
		}
	}

	return true
}

// matchPolicy는 정책의 규칙 목록 중 하나라도 매칭되면 true
// 규칙이 없는 정책(rules: [])은 실제 Istio에서 rbacPolicyMatchNever로 변환
func matchPolicy(policy AuthorizationPolicy, req RequestContext) (bool, string) {
	if len(policy.Rules) == 0 {
		return false, "규칙 없음 (match-never)"
	}
	for i, rule := range policy.Rules {
		if matchRule(rule, req) {
			return true, fmt.Sprintf("규칙 #%d 매칭", i)
		}
	}
	return false, "매칭되는 규칙 없음"
}

// --- 정책 평가 결과 ---
type EvalResult struct {
	Decision    string // "ALLOW" 또는 "DENY"
	Reason      string
	MatchedPolicy string
}

// =============================================================================
// 정책 평가 엔진 (실제 Istio Builder의 build() 함수 로직)
// =============================================================================

// PolicyEngine은 정책 평가 엔진
// 실제 Istio의 builder.go에서 정책을 CUSTOM/DENY/ALLOW 순서로 분류하고 평가
type PolicyEngine struct {
	customPolicies []AuthorizationPolicy
	denyPolicies   []AuthorizationPolicy
	allowPolicies  []AuthorizationPolicy
}

func NewPolicyEngine(policies []AuthorizationPolicy) *PolicyEngine {
	engine := &PolicyEngine{}
	for _, p := range policies {
		switch p.Action {
		case ActionCustom:
			engine.customPolicies = append(engine.customPolicies, p)
		case ActionDeny:
			engine.denyPolicies = append(engine.denyPolicies, p)
		case ActionAllow:
			engine.allowPolicies = append(engine.allowPolicies, p)
		}
	}
	return engine
}

// Evaluate는 요청에 대한 정책 평가 수행
// 실제 Istio의 Envoy RBAC 필터 체인과 동일한 순서:
// 1. CUSTOM 정책 평가 → 매칭되면 외부 인가 서비스에 위임
// 2. DENY 정책 평가 → 매칭되면 즉시 거부
// 3. ALLOW 정책 평가 → 매칭되면 허용
// 4. ALLOW 정책이 존재하지만 매칭 안 되면 거부 (default deny)
// 5. ALLOW 정책이 없으면 허용
func (e *PolicyEngine) Evaluate(req RequestContext) EvalResult {
	// 1단계: CUSTOM 정책
	for _, policy := range e.customPolicies {
		matched, reason := matchPolicy(policy, req)
		if matched {
			return EvalResult{
				Decision:    "CUSTOM",
				Reason:      fmt.Sprintf("외부 인가 서비스(%s)에 위임: %s", policy.Provider, reason),
				MatchedPolicy: policy.Name,
			}
		}
	}

	// 2단계: DENY 정책
	for _, policy := range e.denyPolicies {
		matched, reason := matchPolicy(policy, req)
		if matched {
			return EvalResult{
				Decision:    "DENY",
				Reason:      reason,
				MatchedPolicy: policy.Name,
			}
		}
	}

	// 3단계: ALLOW 정책
	if len(e.allowPolicies) > 0 {
		for _, policy := range e.allowPolicies {
			matched, reason := matchPolicy(policy, req)
			if matched {
				return EvalResult{
					Decision:    "ALLOW",
					Reason:      reason,
					MatchedPolicy: policy.Name,
				}
			}
		}
		// ALLOW 정책이 존재하지만 매칭 안 됨 -> 기본 거부
		return EvalResult{
			Decision:    "DENY",
			Reason:      "ALLOW 정책 존재하지만 매칭되는 규칙 없음 (default deny)",
			MatchedPolicy: "-",
		}
	}

	// ALLOW 정책이 없으면 기본 허용
	return EvalResult{
		Decision:    "ALLOW",
		Reason:      "ALLOW 정책 없음 (default allow)",
		MatchedPolicy: "-",
	}
}

// =============================================================================
// 시뮬레이션 실행
// =============================================================================

func main() {
	fmt.Println("=" + strings.Repeat("=", 79))
	fmt.Println("Istio Authorization Policy 평가 시뮬레이션")
	fmt.Println("=" + strings.Repeat("=", 79))

	// -----------------------------------------------------------------------
	// 정책 정의
	// -----------------------------------------------------------------------
	policies := []AuthorizationPolicy{
		// CUSTOM 정책: 외부 인가 서비스 위임
		{
			Name:      "ext-authz-oauth",
			Namespace: "default",
			Action:    ActionCustom,
			Provider:  "oauth2-proxy",
			Rules: []Rule{
				{
					To: []Operation{
						{Paths: []string{"/admin/*"}, Methods: []string{"GET", "POST"}},
					},
				},
			},
		},

		// DENY 정책 1: 특정 네임스페이스에서의 접근 차단
		{
			Name:      "deny-untrusted",
			Namespace: "default",
			Action:    ActionDeny,
			Rules: []Rule{
				{
					From: []Source{
						{Namespaces: []string{"untrusted"}},
					},
				},
			},
		},

		// DENY 정책 2: 특정 경로에 대한 외부 IP 차단
		{
			Name:      "deny-external-to-internal",
			Namespace: "default",
			Action:    ActionDeny,
			Rules: []Rule{
				{
					From: []Source{
						{NotIPBlocks: []string{"10.0.0.0/8", "172.16.0.0/12"}},
					},
					To: []Operation{
						{Paths: []string{"/internal/*"}},
					},
				},
			},
		},

		// ALLOW 정책 1: 같은 네임스페이스에서 특정 서비스 허용
		{
			Name:      "allow-productpage",
			Namespace: "default",
			Action:    ActionAllow,
			Rules: []Rule{
				{
					From: []Source{
						{
							Principals: []string{
								"cluster.local/ns/default/sa/productpage",
								"cluster.local/ns/default/sa/gateway",
							},
						},
					},
					To: []Operation{
						{Paths: []string{"/api/*", "/health"}, Methods: []string{"GET"}},
					},
				},
			},
		},

		// ALLOW 정책 2: 특정 헤더가 있는 요청 허용
		{
			Name:      "allow-with-token",
			Namespace: "default",
			Action:    ActionAllow,
			Rules: []Rule{
				{
					To: []Operation{
						{Paths: []string{"/api/*"}, Methods: []string{"POST", "PUT"}},
					},
					When: []Condition{
						{
							Key:    "request.headers[x-auth-token]",
							Values: []string{"valid-token-*"},
						},
					},
				},
			},
		},

		// ALLOW 정책 3: 헬스체크 경로는 모두 허용
		{
			Name:      "allow-health",
			Namespace: "default",
			Action:    ActionAllow,
			Rules: []Rule{
				{
					To: []Operation{
						{Paths: []string{"/healthz", "/readyz"}, Methods: []string{"GET"}},
					},
				},
			},
		},
	}

	// -----------------------------------------------------------------------
	// 정책 목록 출력
	// -----------------------------------------------------------------------
	fmt.Println("\n[1] 등록된 정책 목록")
	fmt.Println(strings.Repeat("-", 60))

	for _, p := range policies {
		fmt.Printf("  %s [%s] (%s/%s)\n", p.Action, p.Name, p.Namespace, p.Name)
		for i, rule := range p.Rules {
			fmt.Printf("    규칙 #%d:\n", i)
			if len(rule.From) > 0 {
				fmt.Printf("      From: ")
				for _, src := range rule.From {
					if len(src.Principals) > 0 {
						fmt.Printf("principals=%v ", src.Principals)
					}
					if len(src.Namespaces) > 0 {
						fmt.Printf("namespaces=%v ", src.Namespaces)
					}
					if len(src.IPBlocks) > 0 {
						fmt.Printf("ipBlocks=%v ", src.IPBlocks)
					}
					if len(src.NotIPBlocks) > 0 {
						fmt.Printf("notIpBlocks=%v ", src.NotIPBlocks)
					}
					if len(src.NotPrincipals) > 0 {
						fmt.Printf("notPrincipals=%v ", src.NotPrincipals)
					}
					if len(src.NotNamespaces) > 0 {
						fmt.Printf("notNamespaces=%v ", src.NotNamespaces)
					}
				}
				fmt.Println()
			}
			if len(rule.To) > 0 {
				fmt.Printf("      To: ")
				for _, op := range rule.To {
					if len(op.Paths) > 0 {
						fmt.Printf("paths=%v ", op.Paths)
					}
					if len(op.Methods) > 0 {
						fmt.Printf("methods=%v ", op.Methods)
					}
					if len(op.Hosts) > 0 {
						fmt.Printf("hosts=%v ", op.Hosts)
					}
				}
				fmt.Println()
			}
			if len(rule.When) > 0 {
				for _, cond := range rule.When {
					fmt.Printf("      When: key=%s values=%v\n", cond.Key, cond.Values)
				}
			}
		}
	}

	// -----------------------------------------------------------------------
	// 평가 순서 설명
	// -----------------------------------------------------------------------
	fmt.Println("\n[2] 정책 평가 순서 (Envoy RBAC 필터 체인)")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println("  1. CUSTOM 정책 평가 -> 매칭되면 외부 인가 서비스에 위임")
	fmt.Println("  2. DENY 정책 평가   -> 매칭되면 즉시 거부 (403)")
	fmt.Println("  3. ALLOW 정책 평가  -> 매칭되면 허용")
	fmt.Println("  4. ALLOW 정책 존재하나 미매칭 -> 기본 거부 (403)")
	fmt.Println("  5. ALLOW 정책 없음          -> 기본 허용")

	// -----------------------------------------------------------------------
	// 테스트 요청들
	// -----------------------------------------------------------------------
	fmt.Println("\n[3] 정책 평가 테스트")
	fmt.Println(strings.Repeat("-", 60))

	engine := NewPolicyEngine(policies)

	testRequests := []struct {
		desc string
		req  RequestContext
	}{
		{
			desc: "관리자 경로 접근 (CUSTOM 위임 예상)",
			req: RequestContext{
				SourcePrincipal: "cluster.local/ns/default/sa/admin",
				SourceNamespace: "default",
				SourceIP:        "10.1.1.1",
				Host:            "reviews.default.svc.cluster.local",
				Path:            "/admin/dashboard",
				Method:          "GET",
				Headers:         map[string]string{},
			},
		},
		{
			desc: "untrusted 네임스페이스에서 접근 (DENY 예상)",
			req: RequestContext{
				SourcePrincipal: "cluster.local/ns/untrusted/sa/hacker",
				SourceNamespace: "untrusted",
				SourceIP:        "10.2.1.1",
				Host:            "reviews.default.svc.cluster.local",
				Path:            "/api/reviews",
				Method:          "GET",
				Headers:         map[string]string{},
			},
		},
		{
			desc: "외부 IP에서 내부 경로 접근 (DENY 예상)",
			req: RequestContext{
				SourcePrincipal: "cluster.local/ns/default/sa/external",
				SourceNamespace: "default",
				SourceIP:        "203.0.113.50",
				Host:            "reviews.default.svc.cluster.local",
				Path:            "/internal/config",
				Method:          "GET",
				Headers:         map[string]string{},
			},
		},
		{
			desc: "내부 IP에서 내부 경로 접근 (DENY 규칙 미매칭, ALLOW 규칙 필요)",
			req: RequestContext{
				SourcePrincipal: "cluster.local/ns/default/sa/productpage",
				SourceNamespace: "default",
				SourceIP:        "10.1.1.5",
				Host:            "reviews.default.svc.cluster.local",
				Path:            "/internal/config",
				Method:          "GET",
				Headers:         map[string]string{},
			},
		},
		{
			desc: "productpage 서비스가 GET /api/reviews 접근 (ALLOW 예상)",
			req: RequestContext{
				SourcePrincipal: "cluster.local/ns/default/sa/productpage",
				SourceNamespace: "default",
				SourceIP:        "10.1.1.2",
				Host:            "reviews.default.svc.cluster.local",
				Path:            "/api/reviews",
				Method:          "GET",
				Headers:         map[string]string{},
			},
		},
		{
			desc: "유효한 토큰으로 POST /api/reviews (ALLOW 예상)",
			req: RequestContext{
				SourcePrincipal: "cluster.local/ns/default/sa/frontend",
				SourceNamespace: "default",
				SourceIP:        "10.1.1.3",
				Host:            "reviews.default.svc.cluster.local",
				Path:            "/api/reviews",
				Method:          "POST",
				Headers:         map[string]string{"x-auth-token": "valid-token-abc123"},
			},
		},
		{
			desc: "토큰 없이 POST /api/reviews (DENY 예상 - ALLOW 미매칭)",
			req: RequestContext{
				SourcePrincipal: "cluster.local/ns/default/sa/frontend",
				SourceNamespace: "default",
				SourceIP:        "10.1.1.3",
				Host:            "reviews.default.svc.cluster.local",
				Path:            "/api/reviews",
				Method:          "POST",
				Headers:         map[string]string{},
			},
		},
		{
			desc: "헬스체크 접근 (ALLOW 예상)",
			req: RequestContext{
				SourcePrincipal: "cluster.local/ns/kube-system/sa/kubelet",
				SourceNamespace: "kube-system",
				SourceIP:        "10.0.0.1",
				Host:            "reviews.default.svc.cluster.local",
				Path:            "/healthz",
				Method:          "GET",
				Headers:         map[string]string{},
			},
		},
		{
			desc: "허가되지 않은 서비스가 DELETE 요청 (DENY 예상 - ALLOW 미매칭)",
			req: RequestContext{
				SourcePrincipal: "cluster.local/ns/default/sa/unknown",
				SourceNamespace: "default",
				SourceIP:        "10.1.1.9",
				Host:            "reviews.default.svc.cluster.local",
				Path:            "/api/reviews/123",
				Method:          "DELETE",
				Headers:         map[string]string{},
			},
		},
		{
			desc: "gateway 서비스가 GET /health 접근 (ALLOW 예상)",
			req: RequestContext{
				SourcePrincipal: "cluster.local/ns/default/sa/gateway",
				SourceNamespace: "default",
				SourceIP:        "10.1.1.10",
				Host:            "reviews.default.svc.cluster.local",
				Path:            "/health",
				Method:          "GET",
				Headers:         map[string]string{},
			},
		},
	}

	for i, tc := range testRequests {
		fmt.Printf("\n  --- 테스트 #%d: %s ---\n", i+1, tc.desc)
		fmt.Printf("  요청: %s\n", tc.req)

		result := engine.Evaluate(tc.req)

		icon := " "
		if result.Decision == "ALLOW" {
			icon = "[허용]"
		} else if result.Decision == "DENY" {
			icon = "[거부]"
		} else {
			icon = "[위임]"
		}
		fmt.Printf("  결과: %s %s\n", icon, result.Decision)
		fmt.Printf("  정책: %s\n", result.MatchedPolicy)
		fmt.Printf("  사유: %s\n", result.Reason)
	}

	// -----------------------------------------------------------------------
	// AND/OR 로직 상세 설명
	// -----------------------------------------------------------------------
	fmt.Println("\n" + strings.Repeat("-", 60))
	fmt.Println("[4] AND/OR 로직 상세")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println(`
  정책 내부 로직 구조:

  AuthorizationPolicy
  └── Rules[] (OR - 하나라도 매칭되면 정책 적용)
      ├── From[] (OR - 하나라도 매칭되면 소스 조건 통과)
      │   └── Source 내부 필드 (AND)
      │       ├── principals[]    (OR - 값 목록 중 하나 매칭)
      │       ├── namespaces[]    (OR)
      │       ├── ipBlocks[]      (OR)
      │       ├── notPrincipals[] (AND - 모두 비매칭이어야)
      │       ├── notNamespaces[] (AND)
      │       └── notIpBlocks[]   (AND)
      ├── To[] (OR - 하나라도 매칭되면 오퍼레이션 조건 통과)
      │   └── Operation 내부 필드 (AND)
      │       ├── hosts[]    (OR)
      │       ├── paths[]    (OR)
      │       ├── methods[]  (OR)
      │       ├── notHosts[] (AND)
      │       └── notPaths[] (AND)
      └── When[] (AND - 모든 조건 매칭 필요)
          └── Condition
              ├── values[]    (OR)
              └── notValues[] (AND)

  Envoy RBAC 필터 체인 순서:
  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐
  │ CUSTOM RBAC  │->│  DENY RBAC   │->│  ALLOW RBAC  │-> 기본 허용/거부
  │ (ext_authz)  │  │  (enforce)   │  │  (enforce)   │
  └──────────────┘  └──────────────┘  └──────────────┘
`)

	// -----------------------------------------------------------------------
	// 빈 규칙 정책 테스트
	// -----------------------------------------------------------------------
	fmt.Println("[5] 특수 케이스: 빈 규칙 정책")
	fmt.Println(strings.Repeat("-", 60))

	// 빈 ALLOW 정책 = match-never (rbacPolicyMatchNever)
	emptyAllowPolicies := []AuthorizationPolicy{
		{
			Name:      "deny-all",
			Namespace: "default",
			Action:    ActionAllow,
			Rules:     []Rule{}, // 빈 규칙 -> match-never
		},
	}

	emptyEngine := NewPolicyEngine(emptyAllowPolicies)
	result := emptyEngine.Evaluate(RequestContext{
		SourcePrincipal: "cluster.local/ns/default/sa/any",
		SourceNamespace: "default",
		SourceIP:        "10.1.1.1",
		Path:            "/anything",
		Method:          "GET",
	})

	fmt.Printf("  빈 ALLOW 정책(rules: []) + 임의 요청:\n")
	fmt.Printf("  결과: %s - %s\n", result.Decision, result.Reason)
	fmt.Println("  => 빈 규칙의 ALLOW 정책은 match-never로 변환되어 모든 요청 거부")

	// DENY 정책만 있는 경우
	denyOnlyPolicies := []AuthorizationPolicy{
		{
			Name:      "deny-bad-path",
			Namespace: "default",
			Action:    ActionDeny,
			Rules: []Rule{
				{
					To: []Operation{{Paths: []string{"/evil"}}},
				},
			},
		},
	}

	denyOnlyEngine := NewPolicyEngine(denyOnlyPolicies)
	result2 := denyOnlyEngine.Evaluate(RequestContext{
		SourcePrincipal: "cluster.local/ns/default/sa/any",
		SourceNamespace: "default",
		SourceIP:        "10.1.1.1",
		Path:            "/safe",
		Method:          "GET",
	})

	fmt.Printf("\n  DENY 정책만 있고 ALLOW 정책 없음 + /safe 요청:\n")
	fmt.Printf("  결과: %s - %s\n", result2.Decision, result2.Reason)
	fmt.Println("  => ALLOW 정책이 없으면 DENY에 매칭되지 않는 요청은 기본 허용")

	fmt.Println("\n" + strings.Repeat("=", 80))
	fmt.Println("시뮬레이션 완료!")
	fmt.Println(strings.Repeat("=", 80))
}
