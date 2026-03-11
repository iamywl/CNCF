package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// =============================================================================
// gRPC Authorization 정책 엔진 시뮬레이션
// =============================================================================
//
// gRPC authz는 RBAC 기반 인가 정책을 인터셉터로 적용한다.
// CEL(Common Expression Language) 또는 JSON 정책 파일로 규칙을 정의한다.
//
// 핵심 개념:
//   - AuthzPolicy: JSON 기반 인가 정책
//   - RBAC Rules: 역할 기반 접근 제어
//   - Principal Matching: 인증된 사용자/서비스 식별
//   - Action: ALLOW / DENY
//
// 실제 코드 참조:
//   - authz/rbac_translator.go: 정책 변환
//   - authz/grpc_authz_server_interceptors.go: 서버 인터셉터
// =============================================================================

// --- 인가 정책 모델 ---

type Decision int

const (
	DecisionAllow Decision = iota
	DecisionDeny
	DecisionUndecided
)

func (d Decision) String() string {
	return []string{"ALLOW", "DENY", "UNDECIDED"}[d]
}

type StringMatcher struct {
	Exact    string `json:"exact,omitempty"`
	Prefix   string `json:"prefix,omitempty"`
	Suffix   string `json:"suffix,omitempty"`
	Contains string `json:"contains,omitempty"`
}

func (m StringMatcher) Match(s string) bool {
	if m.Exact != "" {
		return s == m.Exact
	}
	if m.Prefix != "" {
		return strings.HasPrefix(s, m.Prefix)
	}
	if m.Suffix != "" {
		return strings.HasSuffix(s, m.Suffix)
	}
	if m.Contains != "" {
		return strings.Contains(s, m.Contains)
	}
	return true // empty matcher = match all
}

// Principal은 요청자를 식별하는 조건이다.
type Principal struct {
	Authenticated *AuthenticatedPrincipal `json:"authenticated,omitempty"`
}

type AuthenticatedPrincipal struct {
	PrincipalName StringMatcher `json:"principalName"`
}

// Permission은 접근 대상 리소스를 식별하는 조건이다.
type Permission struct {
	Methods []StringMatcher `json:"methods,omitempty"`
	Headers []HeaderMatcher `json:"headers,omitempty"`
}

type HeaderMatcher struct {
	Name    string       `json:"name"`
	Matcher StringMatcher `json:"matcher"`
}

// RBACRule은 하나의 RBAC 규칙이다.
type RBACRule struct {
	Name        string       `json:"name"`
	Principals  []Principal  `json:"principals"`
	Permissions []Permission `json:"permissions"`
}

// RBACPolicy는 ALLOW 또는 DENY 정책이다.
type RBACPolicy struct {
	Action Decision   `json:"action"`
	Rules  []RBACRule `json:"rules"`
}

// AuthzPolicy는 전체 인가 정책이다.
type AuthzPolicy struct {
	Name        string       `json:"name"`
	DenyRules   []RBACRule   `json:"deny_rules,omitempty"`
	AllowRules  []RBACRule   `json:"allow_rules,omitempty"`
}

// --- 요청 컨텍스트 ---

type RequestContext struct {
	Method      string            // /package.Service/Method
	Principal   string            // 인증된 사용자/서비스 이름
	Headers     map[string]string // 요청 메타데이터
	SourceIP    string
	Timestamp   time.Time
}

// --- 인가 엔진 ---

type AuthzEngine struct {
	policy AuthzPolicy
	stats  AuthzStats
}

type AuthzStats struct {
	TotalRequests int
	Allowed       int
	Denied        int
}

func NewAuthzEngine(policy AuthzPolicy) *AuthzEngine {
	return &AuthzEngine{policy: policy}
}

func matchPrincipal(principal Principal, reqPrincipal string) bool {
	if principal.Authenticated != nil {
		return principal.Authenticated.PrincipalName.Match(reqPrincipal)
	}
	return true
}

func matchPermission(perm Permission, method string, headers map[string]string) bool {
	// 메서드 매칭
	if len(perm.Methods) > 0 {
		matched := false
		for _, m := range perm.Methods {
			if m.Match(method) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	// 헤더 매칭
	for _, hm := range perm.Headers {
		val, ok := headers[hm.Name]
		if !ok {
			return false
		}
		if !hm.Matcher.Match(val) {
			return false
		}
	}

	return true
}

func matchRule(rule RBACRule, ctx RequestContext) bool {
	// 모든 Principal 중 하나라도 매치되어야 함
	principalMatch := len(rule.Principals) == 0
	for _, p := range rule.Principals {
		if matchPrincipal(p, ctx.Principal) {
			principalMatch = true
			break
		}
	}
	if !principalMatch {
		return false
	}

	// 모든 Permission 중 하나라도 매치되어야 함
	permMatch := len(rule.Permissions) == 0
	for _, perm := range rule.Permissions {
		if matchPermission(perm, ctx.Method, ctx.Headers) {
			permMatch = true
			break
		}
	}
	return permMatch
}

// Evaluate는 요청에 대한 인가 결정을 반환한다.
// 평가 순서: DENY 규칙 먼저, 그 다음 ALLOW 규칙.
func (e *AuthzEngine) Evaluate(ctx RequestContext) (Decision, string) {
	e.stats.TotalRequests++

	// 1. DENY 규칙 평가 (하나라도 매치되면 거부)
	for _, rule := range e.policy.DenyRules {
		if matchRule(rule, ctx) {
			e.stats.Denied++
			return DecisionDeny, rule.Name
		}
	}

	// 2. ALLOW 규칙 평가 (하나라도 매치되면 허용)
	for _, rule := range e.policy.AllowRules {
		if matchRule(rule, ctx) {
			e.stats.Allowed++
			return DecisionAllow, rule.Name
		}
	}

	// 3. 기본: 거부
	e.stats.Denied++
	return DecisionDeny, "default-deny"
}

func main() {
	fmt.Println("=== gRPC Authorization 정책 엔진 시뮬레이션 ===")
	fmt.Println()

	// --- 정책 정의 ---
	fmt.Println("[1] 인가 정책 정의")
	fmt.Println(strings.Repeat("-", 60))

	policy := AuthzPolicy{
		Name: "my-authz-policy",
		DenyRules: []RBACRule{
			{
				Name: "deny-health-from-external",
				Principals: []Principal{
					{Authenticated: &AuthenticatedPrincipal{
						PrincipalName: StringMatcher{Prefix: "external/"},
					}},
				},
				Permissions: []Permission{
					{Methods: []StringMatcher{{Prefix: "/grpc.health"}}},
				},
			},
			{
				Name: "deny-admin-methods",
				Permissions: []Permission{
					{
						Methods: []StringMatcher{{Contains: "Admin"}},
						Headers: []HeaderMatcher{
							{Name: "x-role", Matcher: StringMatcher{Exact: "viewer"}},
						},
					},
				},
			},
		},
		AllowRules: []RBACRule{
			{
				Name: "allow-internal-services",
				Principals: []Principal{
					{Authenticated: &AuthenticatedPrincipal{
						PrincipalName: StringMatcher{Prefix: "spiffe://cluster.local/ns/"},
					}},
				},
			},
			{
				Name: "allow-admin-users",
				Principals: []Principal{
					{Authenticated: &AuthenticatedPrincipal{
						PrincipalName: StringMatcher{Suffix: "@admin.example.com"},
					}},
				},
			},
			{
				Name: "allow-read-methods",
				Permissions: []Permission{
					{Methods: []StringMatcher{
						{Prefix: "/myapp.ReadService/"},
						{Exact: "/grpc.health.v1.Health/Check"},
					}},
				},
			},
		},
	}

	policyJSON, _ := json.MarshalIndent(policy, "  ", "  ")
	fmt.Printf("  %s\n", policyJSON)
	fmt.Println()

	// --- 인가 엔진 ---
	engine := NewAuthzEngine(policy)

	// --- 요청 평가 ---
	fmt.Println("[2] 요청 인가 평가")
	fmt.Println(strings.Repeat("-", 60))

	requests := []struct {
		ctx  RequestContext
		desc string
	}{
		{
			RequestContext{"/myapp.UserService/GetUser", "spiffe://cluster.local/ns/default/sa/frontend", nil, "10.0.0.5", time.Now()},
			"내부 서비스 (SPIFFE)",
		},
		{
			RequestContext{"/grpc.health.v1.Health/Check", "external/monitoring", nil, "203.0.113.1", time.Now()},
			"외부에서 Health 체크 (deny)",
		},
		{
			RequestContext{"/grpc.health.v1.Health/Check", "anonymous", nil, "10.0.0.5", time.Now()},
			"익명 Health 체크 (allow - read)",
		},
		{
			RequestContext{"/myapp.AdminService/DeleteUser", "user1@admin.example.com", map[string]string{"x-role": "admin"}, "10.0.0.5", time.Now()},
			"관리자 Admin 메서드",
		},
		{
			RequestContext{"/myapp.AdminService/DeleteUser", "viewer@example.com", map[string]string{"x-role": "viewer"}, "10.0.0.5", time.Now()},
			"뷰어가 Admin 메서드 (deny)",
		},
		{
			RequestContext{"/myapp.ReadService/ListItems", "anonymous", nil, "10.0.0.5", time.Now()},
			"읽기 전용 메서드",
		},
		{
			RequestContext{"/myapp.WriteService/CreateItem", "random-user", nil, "10.0.0.5", time.Now()},
			"비인가 사용자 쓰기 (default deny)",
		},
		{
			RequestContext{"/myapp.UserService/UpdateProfile", "spiffe://cluster.local/ns/kube-system/sa/controller", nil, "10.0.0.5", time.Now()},
			"kube-system 내부 서비스",
		},
	}

	for _, req := range requests {
		decision, ruleName := engine.Evaluate(req.ctx)
		icon := "+"
		if decision == DecisionDeny {
			icon = "X"
		}
		fmt.Printf("\n  [%s] %s\n", icon, req.desc)
		fmt.Printf("      Method:    %s\n", req.ctx.Method)
		fmt.Printf("      Principal: %s\n", req.ctx.Principal)
		fmt.Printf("      Decision:  %s (rule: %s)\n", decision, ruleName)
	}
	fmt.Println()

	// --- 통계 ---
	fmt.Println("[3] 인가 통계")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Printf("  Total Requests: %d\n", engine.stats.TotalRequests)
	fmt.Printf("  Allowed:        %d\n", engine.stats.Allowed)
	fmt.Printf("  Denied:         %d\n", engine.stats.Denied)
	fmt.Println()

	// --- StringMatcher 테스트 ---
	fmt.Println("[4] StringMatcher 패턴 테스트")
	fmt.Println(strings.Repeat("-", 60))

	matchers := []struct {
		matcher StringMatcher
		inputs  []string
	}{
		{StringMatcher{Exact: "/admin"}, []string{"/admin", "/admin/users", "/ADMIN"}},
		{StringMatcher{Prefix: "/api/v1"}, []string{"/api/v1/users", "/api/v2/users", "/api/v1"}},
		{StringMatcher{Suffix: ".admin"}, []string{"user.admin", "admin", "super.admin"}},
		{StringMatcher{Contains: "secret"}, []string{"top-secret-data", "public-data", "my-secret"}},
	}

	for _, m := range matchers {
		mJSON, _ := json.Marshal(m.matcher)
		fmt.Printf("  Matcher: %s\n", mJSON)
		for _, input := range m.inputs {
			fmt.Printf("    %-25s -> %v\n", input, m.matcher.Match(input))
		}
	}
	fmt.Println()

	fmt.Println("=== 시뮬레이션 완료 ===")
}
