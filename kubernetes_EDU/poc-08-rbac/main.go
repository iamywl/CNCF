package main

import (
	"fmt"
	"strings"
)

// =============================================================================
// Kubernetes RBAC 인가(Authorization) 시뮬레이션
//
// 실제 구현 참조:
//   - plugin/pkg/auth/authorizer/rbac/rbac.go (RBACAuthorizer, RuleAllows)
//   - staging/src/k8s.io/apiserver/pkg/authorization/authorizer (Authorizer interface)
//   - k8s.io/api/rbac/v1 (Role, ClusterRole, RoleBinding, ClusterRoleBinding, PolicyRule)
//
// 핵심 개념:
//   1. PolicyRule: verb + apiGroup + resource + resourceName 조합의 허가 규칙
//   2. Role/ClusterRole: PolicyRule의 집합 (네임스페이스/클러스터 범위)
//   3. RoleBinding/ClusterRoleBinding: Subject와 Role을 연결
//   4. Subject: User, Group, ServiceAccount
//   5. 인가 체인: Allow / Deny / NoOpinion 3단계 판정
// =============================================================================

// --- 인가 결정 ---

// Decision은 인가 판정 결과를 나타낸다.
// 실제 authorizer.Decision에 대응한다.
type Decision int

const (
	// DecisionNoOpinion: 이 인가자가 판단할 수 없음 (다음 인가자에게 위임)
	DecisionNoOpinion Decision = iota
	// DecisionAllow: 요청을 허가
	DecisionAllow
	// DecisionDeny: 요청을 명시적으로 거부 (체인 즉시 중단)
	DecisionDeny
)

func (d Decision) String() string {
	switch d {
	case DecisionAllow:
		return "Allow"
	case DecisionDeny:
		return "Deny"
	default:
		return "NoOpinion"
	}
}

// --- Subject (요청 주체) ---

// SubjectKind는 인가 주체의 종류를 나타낸다.
type SubjectKind string

const (
	UserKind           SubjectKind = "User"
	GroupKind          SubjectKind = "Group"
	ServiceAccountKind SubjectKind = "ServiceAccount"
)

// Subject는 RBAC에서 권한을 부여받는 주체이다.
// 실제 rbacv1.Subject에 대응한다.
type Subject struct {
	Kind      SubjectKind
	Name      string
	Namespace string // ServiceAccount 전용
}

// UserInfo는 인증된 사용자 정보를 나타낸다.
// 실제 user.Info 인터페이스에 대응한다.
type UserInfo struct {
	Username string
	Groups   []string
	// ServiceAccount인 경우: system:serviceaccount:<namespace>:<name>
}

// --- PolicyRule ---

// PolicyRule은 하나의 권한 규칙을 정의한다.
// 실제 rbacv1.PolicyRule에 대응한다.
//
// 매칭 규칙:
//   - "*"는 와일드카드로 모든 값에 매칭된다
//   - 빈 슬라이스는 "아무것도 매칭되지 않음"을 의미한다
//   - ResourceNames가 비어있으면 모든 리소스 이름에 매칭된다
type PolicyRule struct {
	Verbs         []string // get, list, watch, create, update, delete, patch
	APIGroups     []string // "", "apps", "batch" 등
	Resources     []string // pods, deployments, services 등
	ResourceNames []string // 특정 리소스 이름 (비어있으면 모든 이름)
}

// --- Role / ClusterRole ---

// Role은 네임스페이스 범위의 역할을 정의한다.
type Role struct {
	Name      string
	Namespace string
	Rules     []PolicyRule
}

// ClusterRole은 클러스터 범위의 역할을 정의한다.
type ClusterRole struct {
	Name  string
	Rules []PolicyRule
}

// --- Binding ---

// RoleBinding은 네임스페이스 범위에서 Subject와 Role/ClusterRole을 연결한다.
type RoleBinding struct {
	Name      string
	Namespace string
	Subjects  []Subject
	RoleRef   RoleRef
}

// ClusterRoleBinding은 클러스터 범위에서 Subject와 ClusterRole을 연결한다.
type ClusterRoleBinding struct {
	Name     string
	Subjects []Subject
	RoleRef  RoleRef
}

// RoleRef는 바인딩이 참조하는 역할을 지정한다.
type RoleRef struct {
	Kind string // "Role" 또는 "ClusterRole"
	Name string
}

// --- 요청 속성 ---

// RequestAttributes는 인가 요청의 속성을 나타낸다.
// 실제 authorizer.Attributes 인터페이스에 대응한다.
type RequestAttributes struct {
	User              UserInfo
	Verb              string // get, list, create, update, delete, watch, patch
	Namespace         string // 비어있으면 클러스터 범위
	APIGroup          string // 리소스의 API 그룹
	Resource          string // 리소스 타입
	Name              string // 특정 리소스 이름
	IsResourceRequest bool   // true면 리소스 요청, false면 non-resource URL
	Path              string // non-resource URL 경로
}

// --- Authorizer 인터페이스 ---

// Authorizer는 인가 판정을 수행하는 인터페이스이다.
type Authorizer interface {
	Authorize(req RequestAttributes) (Decision, string)
}

// --- RBAC Authorizer ---

// RBACAuthorizer는 RBAC 기반 인가를 수행한다.
// 실제 plugin/pkg/auth/authorizer/rbac/rbac.go의 RBACAuthorizer에 대응한다.
//
// 동작 원리:
//   1. 요청의 User/Group에 매칭되는 모든 RoleBinding/ClusterRoleBinding을 찾는다
//   2. 각 바인딩이 참조하는 Role/ClusterRole의 PolicyRule을 가져온다
//   3. 각 PolicyRule이 요청의 verb/apiGroup/resource/name에 매칭되는지 확인한다
//   4. 하나라도 매칭되면 Allow, 아니면 NoOpinion을 반환한다
type RBACAuthorizer struct {
	roles               map[string]map[string]*Role  // namespace → name → Role
	clusterRoles        map[string]*ClusterRole      // name → ClusterRole
	roleBindings        map[string][]*RoleBinding    // namespace → RoleBindings
	clusterRoleBindings []*ClusterRoleBinding
}

// NewRBACAuthorizer는 새 RBAC 인가자를 생성한다.
func NewRBACAuthorizer() *RBACAuthorizer {
	return &RBACAuthorizer{
		roles:        make(map[string]map[string]*Role),
		clusterRoles: make(map[string]*ClusterRole),
		roleBindings: make(map[string][]*RoleBinding),
	}
}

// AddRole은 네임스페이스 범위 Role을 추가한다.
func (a *RBACAuthorizer) AddRole(role *Role) {
	if a.roles[role.Namespace] == nil {
		a.roles[role.Namespace] = make(map[string]*Role)
	}
	a.roles[role.Namespace][role.Name] = role
}

// AddClusterRole은 클러스터 범위 ClusterRole을 추가한다.
func (a *RBACAuthorizer) AddClusterRole(cr *ClusterRole) {
	a.clusterRoles[cr.Name] = cr
}

// AddRoleBinding은 네임스페이스 범위 RoleBinding을 추가한다.
func (a *RBACAuthorizer) AddRoleBinding(rb *RoleBinding) {
	a.roleBindings[rb.Namespace] = append(a.roleBindings[rb.Namespace], rb)
}

// AddClusterRoleBinding은 클러스터 범위 ClusterRoleBinding을 추가한다.
func (a *RBACAuthorizer) AddClusterRoleBinding(crb *ClusterRoleBinding) {
	a.clusterRoleBindings = append(a.clusterRoleBindings, crb)
}

// Authorize는 RBAC 규칙에 따라 요청을 인가한다.
// 실제 rbac.go의 Authorize() 메서드에 대응한다.
//
// 실제 코드의 핵심 로직:
//   ruleCheckingVisitor := &authorizingVisitor{requestAttributes: requestAttributes}
//   r.authorizationRuleResolver.VisitRulesFor(ctx, user, namespace, ruleCheckingVisitor.visit)
//   if ruleCheckingVisitor.allowed { return DecisionAllow }
//   return DecisionNoOpinion
func (a *RBACAuthorizer) Authorize(req RequestAttributes) (Decision, string) {
	// 1. ClusterRoleBinding 검사 (클러스터 범위 - 모든 네임스페이스에 적용)
	for _, crb := range a.clusterRoleBindings {
		if !subjectMatches(crb.Subjects, req.User) {
			continue
		}
		cr, ok := a.clusterRoles[crb.RoleRef.Name]
		if !ok {
			continue
		}
		for _, rule := range cr.Rules {
			if ruleAllows(req, rule) {
				return DecisionAllow, fmt.Sprintf("RBAC: allowed by ClusterRoleBinding %q → ClusterRole %q",
					crb.Name, cr.Name)
			}
		}
	}

	// 2. RoleBinding 검사 (네임스페이스 범위)
	if req.Namespace != "" {
		for _, rb := range a.roleBindings[req.Namespace] {
			if !subjectMatches(rb.Subjects, req.User) {
				continue
			}

			var rules []PolicyRule
			switch rb.RoleRef.Kind {
			case "Role":
				if nsRoles, ok := a.roles[rb.Namespace]; ok {
					if role, ok := nsRoles[rb.RoleRef.Name]; ok {
						rules = role.Rules
					}
				}
			case "ClusterRole":
				if cr, ok := a.clusterRoles[rb.RoleRef.Name]; ok {
					rules = cr.Rules
				}
			}

			for _, rule := range rules {
				if ruleAllows(req, rule) {
					return DecisionAllow, fmt.Sprintf("RBAC: allowed by RoleBinding %q/%q → %s %q",
						rb.Namespace, rb.Name, rb.RoleRef.Kind, rb.RoleRef.Name)
				}
			}
		}
	}

	// 아무 규칙도 매칭되지 않음
	return DecisionNoOpinion, "RBAC: no rules matched"
}

// --- 매칭 함수들 ---

// subjectMatches는 바인딩의 Subject 목록에 사용자가 포함되는지 확인한다.
func subjectMatches(subjects []Subject, user UserInfo) bool {
	for _, s := range subjects {
		switch s.Kind {
		case UserKind:
			if s.Name == user.Username {
				return true
			}
		case GroupKind:
			for _, g := range user.Groups {
				if s.Name == g {
					return true
				}
			}
		case ServiceAccountKind:
			// ServiceAccount는 "system:serviceaccount:<namespace>:<name>" 형식
			expected := fmt.Sprintf("system:serviceaccount:%s:%s", s.Namespace, s.Name)
			if expected == user.Username {
				return true
			}
		}
	}
	return false
}

// ruleAllows는 PolicyRule이 요청에 매칭되는지 확인한다.
// 실제 rbac.go의 RuleAllows() 함수에 대응한다:
//
//	return VerbMatches(rule, verb) &&
//	       APIGroupMatches(rule, apiGroup) &&
//	       ResourceMatches(rule, resource, subresource) &&
//	       ResourceNameMatches(rule, name)
func ruleAllows(req RequestAttributes, rule PolicyRule) bool {
	return verbMatches(rule.Verbs, req.Verb) &&
		apiGroupMatches(rule.APIGroups, req.APIGroup) &&
		resourceMatches(rule.Resources, req.Resource) &&
		resourceNameMatches(rule.ResourceNames, req.Name)
}

// verbMatches는 요청 verb가 규칙에 매칭되는지 확인한다.
func verbMatches(ruleVerbs []string, requestVerb string) bool {
	for _, v := range ruleVerbs {
		if v == "*" || v == requestVerb {
			return true
		}
	}
	return false
}

// apiGroupMatches는 요청 API 그룹이 규칙에 매칭되는지 확인한다.
func apiGroupMatches(ruleGroups []string, requestGroup string) bool {
	for _, g := range ruleGroups {
		if g == "*" || g == requestGroup {
			return true
		}
	}
	return false
}

// resourceMatches는 요청 리소스가 규칙에 매칭되는지 확인한다.
func resourceMatches(ruleResources []string, requestResource string) bool {
	for _, r := range ruleResources {
		if r == "*" || r == requestResource {
			return true
		}
		// 서브리소스 매칭: "pods/*"는 "pods/log", "pods/exec" 등에 매칭
		if strings.HasSuffix(r, "/*") {
			prefix := strings.TrimSuffix(r, "/*")
			if strings.HasPrefix(requestResource, prefix+"/") {
				return true
			}
		}
	}
	return false
}

// resourceNameMatches는 요청의 리소스 이름이 규칙에 매칭되는지 확인한다.
// ResourceNames가 비어있으면 모든 이름에 매칭된다.
func resourceNameMatches(ruleNames []string, requestName string) bool {
	if len(ruleNames) == 0 {
		return true // 비어있으면 모든 이름 매칭
	}
	for _, n := range ruleNames {
		if n == requestName {
			return true
		}
	}
	return false
}

// --- 인가 체인 ---

// AuthorizationChain은 여러 Authorizer를 순차적으로 실행한다.
// 실제 Kubernetes API 서버에서 Node → RBAC → Webhook 순으로 체인을 구성하는 방식.
//
// 판정 규칙:
//   - Allow: 즉시 허가 (체인 중단)
//   - Deny: 즉시 거부 (체인 중단)
//   - NoOpinion: 다음 인가자에게 위임
//   - 모든 인가자가 NoOpinion이면 최종적으로 거부
type AuthorizationChain struct {
	authorizers []namedAuthorizer
}

type namedAuthorizer struct {
	name       string
	authorizer Authorizer
}

func NewAuthorizationChain() *AuthorizationChain {
	return &AuthorizationChain{}
}

func (c *AuthorizationChain) Add(name string, a Authorizer) {
	c.authorizers = append(c.authorizers, namedAuthorizer{name: name, authorizer: a})
}

func (c *AuthorizationChain) Authorize(req RequestAttributes) (Decision, string) {
	for _, na := range c.authorizers {
		decision, reason := na.authorizer.Authorize(req)
		switch decision {
		case DecisionAllow:
			return DecisionAllow, fmt.Sprintf("[%s] %s", na.name, reason)
		case DecisionDeny:
			return DecisionDeny, fmt.Sprintf("[%s] %s", na.name, reason)
		}
		// NoOpinion → 다음 인가자 계속
	}
	return DecisionDeny, "authorization denied: no authorizer allowed the request"
}

// --- AlwaysDeny Authorizer (테스트용) ---

type AlwaysDenyAuthorizer struct{}

func (a *AlwaysDenyAuthorizer) Authorize(req RequestAttributes) (Decision, string) {
	return DecisionDeny, "always deny"
}

// --- Node Authorizer (간단한 시뮬레이션) ---

// NodeAuthorizer는 kubelet의 Node 인가를 간략히 시뮬레이션한다.
// 실제로는 system:nodes 그룹 + 특정 리소스 접근 패턴을 검사한다.
type NodeAuthorizer struct{}

func (a *NodeAuthorizer) Authorize(req RequestAttributes) (Decision, string) {
	for _, g := range req.User.Groups {
		if g == "system:nodes" {
			// Node는 자기 자신의 정보와 Pod 관련 리소스에 접근 가능
			if req.Resource == "nodes" || req.Resource == "pods" || req.Resource == "secrets" || req.Resource == "configmaps" {
				return DecisionAllow, "Node authorizer: allowed for node identity"
			}
		}
	}
	return DecisionNoOpinion, ""
}

// --- 데모 실행 ---

func main() {
	fmt.Println("=== Kubernetes RBAC 인가 시뮬레이션 ===")
	fmt.Println()

	rbac := NewRBACAuthorizer()

	// -----------------------------------------------
	// RBAC 규칙 설정
	// -----------------------------------------------

	// ClusterRole: cluster-admin (모든 권한)
	rbac.AddClusterRole(&ClusterRole{
		Name: "cluster-admin",
		Rules: []PolicyRule{
			{Verbs: []string{"*"}, APIGroups: []string{"*"}, Resources: []string{"*"}},
		},
	})

	// ClusterRole: view (읽기 전용)
	rbac.AddClusterRole(&ClusterRole{
		Name: "view",
		Rules: []PolicyRule{
			{Verbs: []string{"get", "list", "watch"}, APIGroups: []string{""}, Resources: []string{"pods", "services", "configmaps"}},
			{Verbs: []string{"get", "list", "watch"}, APIGroups: []string{"apps"}, Resources: []string{"deployments", "replicasets"}},
		},
	})

	// ClusterRole: pod-manager (Pod 관리)
	rbac.AddClusterRole(&ClusterRole{
		Name: "pod-manager",
		Rules: []PolicyRule{
			{Verbs: []string{"get", "list", "watch", "create", "update", "delete"}, APIGroups: []string{""}, Resources: []string{"pods", "pods/log"}},
		},
	})

	// Role: secret-reader (default 네임스페이스의 시크릿 읽기)
	rbac.AddRole(&Role{
		Name:      "secret-reader",
		Namespace: "default",
		Rules: []PolicyRule{
			{Verbs: []string{"get", "list"}, APIGroups: []string{""}, Resources: []string{"secrets"}},
		},
	})

	// Role: configmap-editor (특정 ConfigMap만 수정 가능)
	rbac.AddRole(&Role{
		Name:      "configmap-editor",
		Namespace: "production",
		Rules: []PolicyRule{
			{Verbs: []string{"get", "update"}, APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{"app-config", "feature-flags"}},
		},
	})

	// ClusterRoleBinding: admin → cluster-admin
	rbac.AddClusterRoleBinding(&ClusterRoleBinding{
		Name: "admin-binding",
		Subjects: []Subject{
			{Kind: UserKind, Name: "admin"},
			{Kind: GroupKind, Name: "system:masters"},
		},
		RoleRef: RoleRef{Kind: "ClusterRole", Name: "cluster-admin"},
	})

	// ClusterRoleBinding: everyone → view (모든 인증된 사용자에게 읽기 권한)
	rbac.AddClusterRoleBinding(&ClusterRoleBinding{
		Name: "authenticated-view",
		Subjects: []Subject{
			{Kind: GroupKind, Name: "system:authenticated"},
		},
		RoleRef: RoleRef{Kind: "ClusterRole", Name: "view"},
	})

	// RoleBinding: dev-team → pod-manager (default 네임스페이스)
	rbac.AddRoleBinding(&RoleBinding{
		Name:      "dev-pod-access",
		Namespace: "default",
		Subjects: []Subject{
			{Kind: GroupKind, Name: "dev-team"},
			{Kind: UserKind, Name: "alice"},
		},
		RoleRef: RoleRef{Kind: "ClusterRole", Name: "pod-manager"},
	})

	// RoleBinding: bob → secret-reader (default 네임스페이스)
	rbac.AddRoleBinding(&RoleBinding{
		Name:      "bob-secret-access",
		Namespace: "default",
		Subjects: []Subject{
			{Kind: UserKind, Name: "bob"},
		},
		RoleRef: RoleRef{Kind: "Role", Name: "secret-reader"},
	})

	// RoleBinding: deploy-sa → configmap-editor (production 네임스페이스)
	rbac.AddRoleBinding(&RoleBinding{
		Name:      "deploy-configmap-access",
		Namespace: "production",
		Subjects: []Subject{
			{Kind: ServiceAccountKind, Name: "deploy-bot", Namespace: "production"},
		},
		RoleRef: RoleRef{Kind: "Role", Name: "configmap-editor"},
	})

	// -----------------------------------------------
	// 테스트 시나리오
	// -----------------------------------------------

	fmt.Println("--- 1. 사용자별 권한 테스트 ---")
	fmt.Println()

	testCases := []struct {
		desc string
		req  RequestAttributes
	}{
		{
			desc: "admin이 노드 삭제",
			req: RequestAttributes{
				User:              UserInfo{Username: "admin", Groups: []string{"system:masters"}},
				Verb:              "delete",
				Resource:          "nodes",
				APIGroup:          "",
				IsResourceRequest: true,
			},
		},
		{
			desc: "alice가 default에서 Pod 생성",
			req: RequestAttributes{
				User:              UserInfo{Username: "alice", Groups: []string{"system:authenticated"}},
				Verb:              "create",
				Namespace:         "default",
				Resource:          "pods",
				APIGroup:          "",
				IsResourceRequest: true,
			},
		},
		{
			desc: "alice가 production에서 Pod 생성 (권한 없음)",
			req: RequestAttributes{
				User:              UserInfo{Username: "alice", Groups: []string{"system:authenticated"}},
				Verb:              "create",
				Namespace:         "production",
				Resource:          "pods",
				APIGroup:          "",
				IsResourceRequest: true,
			},
		},
		{
			desc: "bob이 default에서 시크릿 조회",
			req: RequestAttributes{
				User:              UserInfo{Username: "bob", Groups: []string{"system:authenticated"}},
				Verb:              "get",
				Namespace:         "default",
				Resource:          "secrets",
				APIGroup:          "",
				Name:              "db-password",
				IsResourceRequest: true,
			},
		},
		{
			desc: "bob이 default에서 시크릿 삭제 (권한 없음)",
			req: RequestAttributes{
				User:              UserInfo{Username: "bob", Groups: []string{"system:authenticated"}},
				Verb:              "delete",
				Namespace:         "default",
				Resource:          "secrets",
				APIGroup:          "",
				IsResourceRequest: true,
			},
		},
		{
			desc: "dev-team 그룹 사용자가 Pod 로그 조회",
			req: RequestAttributes{
				User:              UserInfo{Username: "charlie", Groups: []string{"dev-team", "system:authenticated"}},
				Verb:              "get",
				Namespace:         "default",
				Resource:          "pods/log",
				APIGroup:          "",
				IsResourceRequest: true,
			},
		},
		{
			desc: "인증된 사용자가 Deployment 조회 (view 권한)",
			req: RequestAttributes{
				User:              UserInfo{Username: "viewer", Groups: []string{"system:authenticated"}},
				Verb:              "list",
				Namespace:         "default",
				Resource:          "deployments",
				APIGroup:          "apps",
				IsResourceRequest: true,
			},
		},
		{
			desc: "ServiceAccount가 production에서 app-config 수정",
			req: RequestAttributes{
				User:              UserInfo{Username: "system:serviceaccount:production:deploy-bot"},
				Verb:              "update",
				Namespace:         "production",
				Resource:          "configmaps",
				APIGroup:          "",
				Name:              "app-config",
				IsResourceRequest: true,
			},
		},
		{
			desc: "ServiceAccount가 production에서 다른 ConfigMap 수정 (ResourceName 제한)",
			req: RequestAttributes{
				User:              UserInfo{Username: "system:serviceaccount:production:deploy-bot"},
				Verb:              "update",
				Namespace:         "production",
				Resource:          "configmaps",
				APIGroup:          "",
				Name:              "other-config",
				IsResourceRequest: true,
			},
		},
	}

	for _, tc := range testCases {
		decision, reason := rbac.Authorize(tc.req)
		status := "ALLOWED"
		if decision != DecisionAllow {
			status = "DENIED"
		}
		fmt.Printf("  [%s] %s\n", status, tc.desc)
		fmt.Printf("    사유: %s\n", reason)
		fmt.Println()
	}

	// -----------------------------------------------
	// 2. 인가 체인 테스트
	// -----------------------------------------------
	fmt.Println("--- 2. 인가 체인 (Node → RBAC) ---")
	fmt.Println()

	chain := NewAuthorizationChain()
	chain.Add("Node", &NodeAuthorizer{})
	chain.Add("RBAC", rbac)

	chainTests := []struct {
		desc string
		req  RequestAttributes
	}{
		{
			desc: "kubelet(node)이 Pod 조회 → Node 인가자가 허가",
			req: RequestAttributes{
				User:              UserInfo{Username: "system:node:worker-1", Groups: []string{"system:nodes"}},
				Verb:              "get",
				Namespace:         "default",
				Resource:          "pods",
				APIGroup:          "",
				IsResourceRequest: true,
			},
		},
		{
			desc: "kubelet(node)이 Deployment 조회 → Node: NoOpinion → RBAC: NoOpinion → 거부",
			req: RequestAttributes{
				User:              UserInfo{Username: "system:node:worker-1", Groups: []string{"system:nodes"}},
				Verb:              "list",
				Namespace:         "default",
				Resource:          "deployments",
				APIGroup:          "apps",
				IsResourceRequest: true,
			},
		},
		{
			desc: "일반 사용자 → Node: NoOpinion → RBAC: view로 허가",
			req: RequestAttributes{
				User:              UserInfo{Username: "viewer", Groups: []string{"system:authenticated"}},
				Verb:              "get",
				Namespace:         "default",
				Resource:          "pods",
				APIGroup:          "",
				IsResourceRequest: true,
			},
		},
	}

	for _, tc := range chainTests {
		decision, reason := chain.Authorize(tc.req)
		status := "ALLOWED"
		if decision != DecisionAllow {
			status = "DENIED"
		}
		fmt.Printf("  [%s] %s\n", status, tc.desc)
		fmt.Printf("    사유: %s\n", reason)
		fmt.Println()
	}

	// -----------------------------------------------
	// 3. 규칙 매칭 상세 테스트
	// -----------------------------------------------
	fmt.Println("--- 3. PolicyRule 매칭 상세 ---")
	fmt.Println()

	rules := []struct {
		desc string
		rule PolicyRule
		req  RequestAttributes
	}{
		{
			desc: "와일드카드 verb (*) 매칭",
			rule: PolicyRule{Verbs: []string{"*"}, APIGroups: []string{""}, Resources: []string{"pods"}},
			req:  RequestAttributes{Verb: "delete", APIGroup: "", Resource: "pods"},
		},
		{
			desc: "와일드카드 리소스 (*) 매칭",
			rule: PolicyRule{Verbs: []string{"get"}, APIGroups: []string{"*"}, Resources: []string{"*"}},
			req:  RequestAttributes{Verb: "get", APIGroup: "apps", Resource: "deployments"},
		},
		{
			desc: "ResourceName 제한 - 매칭됨",
			rule: PolicyRule{Verbs: []string{"get"}, APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{"my-config"}},
			req:  RequestAttributes{Verb: "get", APIGroup: "", Resource: "configmaps", Name: "my-config"},
		},
		{
			desc: "ResourceName 제한 - 매칭 안됨",
			rule: PolicyRule{Verbs: []string{"get"}, APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{"my-config"}},
			req:  RequestAttributes{Verb: "get", APIGroup: "", Resource: "configmaps", Name: "other-config"},
		},
		{
			desc: "서브리소스 매칭 (pods/log)",
			rule: PolicyRule{Verbs: []string{"get"}, APIGroups: []string{""}, Resources: []string{"pods/log"}},
			req:  RequestAttributes{Verb: "get", APIGroup: "", Resource: "pods/log"},
		},
	}

	for _, tc := range rules {
		result := ruleAllows(tc.req, tc.rule)
		status := "MATCH"
		if !result {
			status = "NO MATCH"
		}
		fmt.Printf("  [%s] %s\n", status, tc.desc)
		fmt.Printf("    Rule: verbs=%v, apiGroups=%v, resources=%v, names=%v\n",
			tc.rule.Verbs, tc.rule.APIGroups, tc.rule.Resources, tc.rule.ResourceNames)
		fmt.Printf("    Req:  verb=%s, apiGroup=%s, resource=%s, name=%s\n",
			tc.req.Verb, tc.req.APIGroup, tc.req.Resource, tc.req.Name)
		fmt.Println()
	}

	fmt.Println("=== 시뮬레이션 완료 ===")
	fmt.Println()
	fmt.Println("핵심 요약:")
	fmt.Println("  1. RBAC는 기본적으로 deny-by-default이다 (규칙이 없으면 거부)")
	fmt.Println("  2. Role은 네임스페이스 범위, ClusterRole은 클러스터 범위이다")
	fmt.Println("  3. RoleBinding으로 Subject(User/Group/SA)와 Role을 연결한다")
	fmt.Println("  4. PolicyRule은 verb + apiGroup + resource + resourceName으로 매칭한다")
	fmt.Println("  5. 인가 체인에서 Allow/Deny는 즉시 결정, NoOpinion은 다음 인가자로 위임한다")
}
