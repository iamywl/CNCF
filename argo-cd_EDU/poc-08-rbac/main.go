// poc-08-rbac: Argo CD RBAC 시스템 시뮬레이션
//
// 실제 소스 참조:
//   - util/rbac/rbac.go: Enforcer struct, enforce(), EnforceErr(), globMatchFunc()
//   - assets/model.conf: Casbin 모델 (sub, res, act, obj, eft)
//   - assets/builtin-policy.csv: role:readonly, role:admin 내장 정책
//   - util/rbac/rbac.go:112-119: ProjectScoped 맵
//   - util/rbac/rbac.go:58-110: 리소스/액션 상수
//   - util/rbac/rbac.go:380-407: enforce() 함수 (defaultRole → claimsEnforcer → casbin)
//   - util/rbac/rbac.go:282-298: globMatchFunc()
//   - util/glob/glob.go: glob 매칭 구현
//
// go run main.go
package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ─────────────────────────────────────────────
// 리소스 및 액션 상수
// 실제: util/rbac/rbac.go:58-110
// ─────────────────────────────────────────────

const (
	// 리소스
	ResourceClusters        = "clusters"
	ResourceProjects        = "projects"
	ResourceApplications    = "applications"
	ResourceApplicationSets = "applicationsets"
	ResourceRepositories    = "repositories"
	ResourceCertificates    = "certificates"
	ResourceAccounts        = "accounts"
	ResourceGPGKeys         = "gpgkeys"
	ResourceLogs            = "logs"
	ResourceExec            = "exec"

	// 액션
	ActionGet      = "get"
	ActionCreate   = "create"
	ActionUpdate   = "update"
	ActionDelete   = "delete"
	ActionSync     = "sync"
	ActionOverride = "override"
	ActionAction   = "action"
)

// ProjectScoped는 프로젝트 범위 리소스 목록이다.
// 오브젝트 형식이 "project/resource"인 리소스들.
// 실제: util/rbac/rbac.go:112-119
var ProjectScoped = map[string]bool{
	ResourceApplications:    true,
	ResourceApplicationSets: true,
	ResourceLogs:            true,
	ResourceExec:            true,
	ResourceClusters:        true,
	ResourceRepositories:    true,
}

// ─────────────────────────────────────────────
// Casbin 모델
// 실제: assets/model.conf
//
// [request_definition]  r = sub, res, act, obj
// [policy_definition]   p = sub, res, act, obj, eft
// [role_definition]     g = _, _
// [policy_effect]       e = some(where (p.eft == allow)) && !some(where (p.eft == deny))
// [matchers]            m = g(r.sub, p.sub) && globMatch(r.res, p.res) &&
//                           globMatch(r.act, p.act) && globMatch(r.obj, p.obj)
// ─────────────────────────────────────────────

// Policy는 Casbin 정책 항목을 나타낸다
// 형식: p, sub, res, act, obj, eft
type Policy struct {
	Sub string // subject (role/user)
	Res string // resource (applications, clusters, ...)
	Act string // action (get, create, ...)
	Obj string // object (project/name or *)
	Eft string // effect (allow/deny)
}

// RoleBinding은 role inheritance를 나타낸다
// 형식: g, sub, role
type RoleBinding struct {
	Sub  string // user or role
	Role string // parent role
}

// ─────────────────────────────────────────────
// Glob 매칭
// 실제: util/rbac/rbac.go:282-298 globMatchFunc()
//       util/glob/glob.go
// ─────────────────────────────────────────────

// globMatch는 pattern과 str를 glob 패턴으로 매칭한다.
// Argo CD는 filepath.Match를 래핑한 glob 라이브러리를 사용한다.
// 실제: util/glob/glob.go → glob.Match()
func globMatch(pattern, str string) bool {
	if pattern == "*" {
		return true
	}
	// filepath.Match는 Go 표준 라이브러리의 glob 매칭
	// *, ?, [...] 지원
	matched, err := filepath.Match(pattern, str)
	if err != nil {
		return false
	}
	return matched
}

// ─────────────────────────────────────────────
// Enforcer Cache (TTL 기반)
// 실제: util/rbac/rbac.go:127-146
// gocache 라이브러리를 사용하지만 여기서는 간단히 구현
// ─────────────────────────────────────────────

type CacheEntry struct {
	Result    bool
	ExpiresAt time.Time
}

type EnforcerCache struct {
	mu      sync.Mutex
	entries map[string]*CacheEntry
	ttl     time.Duration
}

func NewEnforcerCache(ttl time.Duration) *EnforcerCache {
	return &EnforcerCache{
		entries: make(map[string]*CacheEntry),
		ttl:     ttl,
	}
}

func (c *EnforcerCache) Get(key string) (bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || time.Now().After(entry.ExpiresAt) {
		delete(c.entries, key)
		return false, false
	}
	return entry.Result, true
}

func (c *EnforcerCache) Set(key string, result bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = &CacheEntry{
		Result:    result,
		ExpiresAt: time.Now().Add(c.ttl),
	}
}

func (c *EnforcerCache) Flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]*CacheEntry)
}

// ─────────────────────────────────────────────
// Enforcer — RBAC 핵심 구현
// 실제: util/rbac/rbac.go:121-140 Enforcer struct
// ─────────────────────────────────────────────

// Enforcer는 Casbin 기반 RBAC를 구현한다
type Enforcer struct {
	mu          sync.RWMutex
	cache       *EnforcerCache
	policies    []Policy
	roleBindings []RoleBinding
	defaultRole string
}

// NewEnforcer는 내장 정책이 로드된 새 Enforcer를 반환한다
func NewEnforcer() *Enforcer {
	e := &Enforcer{
		cache: NewEnforcerCache(time.Hour),
	}
	// 내장 정책 로드 (assets/builtin-policy.csv)
	e.loadBuiltinPolicy()
	return e
}

// loadBuiltinPolicy는 role:readonly와 role:admin 내장 정책을 로드한다.
// 실제: assets/builtin-policy.csv
func (e *Enforcer) loadBuiltinPolicy() {
	// role:readonly — get 전용
	readonlyPolicies := []Policy{
		{Sub: "role:readonly", Res: "applications", Act: "get", Obj: "*/*", Eft: "allow"},
		{Sub: "role:readonly", Res: "applicationsets", Act: "get", Obj: "*/*", Eft: "allow"},
		{Sub: "role:readonly", Res: "certificates", Act: "get", Obj: "*", Eft: "allow"},
		{Sub: "role:readonly", Res: "clusters", Act: "get", Obj: "*", Eft: "allow"},
		{Sub: "role:readonly", Res: "repositories", Act: "get", Obj: "*", Eft: "allow"},
		{Sub: "role:readonly", Res: "projects", Act: "get", Obj: "*", Eft: "allow"},
		{Sub: "role:readonly", Res: "accounts", Act: "get", Obj: "*", Eft: "allow"},
		{Sub: "role:readonly", Res: "gpgkeys", Act: "get", Obj: "*", Eft: "allow"},
		{Sub: "role:readonly", Res: "logs", Act: "get", Obj: "*/*", Eft: "allow"},
	}

	// role:admin — 전체 권한
	adminPolicies := []Policy{
		{Sub: "role:admin", Res: "applications", Act: "create", Obj: "*/*", Eft: "allow"},
		{Sub: "role:admin", Res: "applications", Act: "update", Obj: "*/*", Eft: "allow"},
		{Sub: "role:admin", Res: "applications", Act: "delete", Obj: "*/*", Eft: "allow"},
		{Sub: "role:admin", Res: "applications", Act: "sync", Obj: "*/*", Eft: "allow"},
		{Sub: "role:admin", Res: "applications", Act: "override", Obj: "*/*", Eft: "allow"},
		{Sub: "role:admin", Res: "applicationsets", Act: "create", Obj: "*/*", Eft: "allow"},
		{Sub: "role:admin", Res: "applicationsets", Act: "update", Obj: "*/*", Eft: "allow"},
		{Sub: "role:admin", Res: "applicationsets", Act: "delete", Obj: "*/*", Eft: "allow"},
		{Sub: "role:admin", Res: "certificates", Act: "create", Obj: "*", Eft: "allow"},
		{Sub: "role:admin", Res: "certificates", Act: "update", Obj: "*", Eft: "allow"},
		{Sub: "role:admin", Res: "certificates", Act: "delete", Obj: "*", Eft: "allow"},
		{Sub: "role:admin", Res: "clusters", Act: "create", Obj: "*", Eft: "allow"},
		{Sub: "role:admin", Res: "clusters", Act: "update", Obj: "*", Eft: "allow"},
		{Sub: "role:admin", Res: "clusters", Act: "delete", Obj: "*", Eft: "allow"},
		{Sub: "role:admin", Res: "repositories", Act: "create", Obj: "*", Eft: "allow"},
		{Sub: "role:admin", Res: "repositories", Act: "update", Obj: "*", Eft: "allow"},
		{Sub: "role:admin", Res: "repositories", Act: "delete", Obj: "*", Eft: "allow"},
		{Sub: "role:admin", Res: "projects", Act: "create", Obj: "*", Eft: "allow"},
		{Sub: "role:admin", Res: "projects", Act: "update", Obj: "*", Eft: "allow"},
		{Sub: "role:admin", Res: "projects", Act: "delete", Obj: "*", Eft: "allow"},
		{Sub: "role:admin", Res: "accounts", Act: "update", Obj: "*", Eft: "allow"},
		{Sub: "role:admin", Res: "exec", Act: "create", Obj: "*/*", Eft: "allow"},
	}

	e.policies = append(e.policies, readonlyPolicies...)
	e.policies = append(e.policies, adminPolicies...)

	// Role inheritance: g, role:admin, role:readonly
	// role:admin은 role:readonly의 모든 권한도 가짐
	// 실제: assets/builtin-policy.csv "g, role:admin, role:readonly"
	e.roleBindings = append(e.roleBindings,
		RoleBinding{Sub: "role:admin", Role: "role:readonly"},
		// admin 사용자는 role:admin
		RoleBinding{Sub: "admin", Role: "role:admin"},
	)
}

// SetUserPolicy는 사용자 정의 정책을 추가한다 (ConfigMap의 policy.csv)
// 실제: util/rbac/rbac.go:417-423
func (e *Enforcer) SetUserPolicy(policyCSV string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	// 기존 사용자 정책 제거 (내장 정책 유지)
	var newPolicies []Policy
	var newBindings []RoleBinding
	for _, p := range e.policies {
		if strings.HasPrefix(p.Sub, "role:readonly") || strings.HasPrefix(p.Sub, "role:admin") {
			newPolicies = append(newPolicies, p)
		}
	}
	for _, b := range e.roleBindings {
		if b.Sub == "role:admin" || b.Sub == "admin" {
			newBindings = append(newBindings, b)
		}
	}
	e.policies = newPolicies
	e.roleBindings = newBindings

	// 새 정책 파싱 및 추가
	for _, line := range strings.Split(policyCSV, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := splitCSV(line)
		if len(parts) < 2 {
			continue
		}
		switch strings.TrimSpace(parts[0]) {
		case "p":
			if len(parts) >= 6 {
				e.policies = append(e.policies, Policy{
					Sub: strings.TrimSpace(parts[1]),
					Res: strings.TrimSpace(parts[2]),
					Act: strings.TrimSpace(parts[3]),
					Obj: strings.TrimSpace(parts[4]),
					Eft: strings.TrimSpace(parts[5]),
				})
			}
		case "g":
			if len(parts) >= 3 {
				e.roleBindings = append(e.roleBindings, RoleBinding{
					Sub:  strings.TrimSpace(parts[1]),
					Role: strings.TrimSpace(parts[2]),
				})
			}
		}
	}
	e.cache.Flush()
}

// SetDefaultRole는 기본 역할을 설정한다
// 실제: util/rbac/rbac.go:311-315
func (e *Enforcer) SetDefaultRole(roleName string) {
	e.defaultRole = roleName
}

// splitCSV는 CSV 라인을 파싱한다 (공백 무시)
func splitCSV(line string) []string {
	parts := strings.Split(line, ",")
	return parts
}

// ─────────────────────────────────────────────
// enforce() — 핵심 평가 로직
// 실제: util/rbac/rbac.go:380-407
// ─────────────────────────────────────────────

// Enforce는 sub, res, act, obj에 대한 접근 허용 여부를 반환한다.
// 실제: util/rbac/rbac.go:326-328
// "Enforce is a wrapper around casbin.Enforce to additionally enforce a default role"
func (e *Enforcer) Enforce(sub, res, act, obj string) bool {
	return e.enforce(sub, res, act, obj)
}

// enforce는 3단계 평가를 수행한다:
// 1. defaultRole 체크
// 2. casbin 정책 체크 (glob 매칭)
// 실제: util/rbac/rbac.go:380-407
func (e *Enforcer) enforce(sub, res, act, obj string) bool {
	// 캐시 확인
	cacheKey := fmt.Sprintf("%s|%s|%s|%s", sub, res, act, obj)
	if result, ok := e.cache.Get(cacheKey); ok {
		return result
	}

	e.mu.RLock()
	defer e.mu.RUnlock()

	// 1. defaultRole 체크 (설정된 경우)
	// 실제: "if defaultRole != "" && len(rvals) >= 2 { if ok, err := enf.Enforce(defaultRole, ...)"
	if e.defaultRole != "" && e.defaultRole != sub {
		if e.casbinEnforce(e.defaultRole, res, act, obj) {
			e.cache.Set(cacheKey, true)
			return true
		}
	}

	// 2. casbin 정책 체크
	result := e.casbinEnforce(sub, res, act, obj)
	e.cache.Set(cacheKey, result)
	return result
}

// casbinEnforce는 Casbin 정책 평가를 수행한다.
// 모델: e = some(where (p.eft == allow)) && !some(where (p.eft == deny))
// 즉, allow가 하나라도 있고 deny가 없으면 허용
func (e *Enforcer) casbinEnforce(sub, res, act, obj string) bool {
	// sub의 모든 역할(상속 포함) 수집
	roles := e.getAllRoles(sub)
	roles[sub] = true

	allowFound := false
	denyFound := false

	for _, p := range e.policies {
		// policy의 sub가 현재 sub의 역할 집합에 포함되는지 확인
		if !roles[p.Sub] {
			continue
		}
		// glob 매칭: res, act, obj
		// 실제: "globOrRegexMatch(r.res, p.res) && globOrRegexMatch(r.act, p.act) && globOrRegexMatch(r.obj, p.obj)"
		if !globMatch(p.Res, res) {
			continue
		}
		if !globMatch(p.Act, act) {
			continue
		}
		if !globMatch(p.Obj, obj) {
			continue
		}

		switch p.Eft {
		case "allow":
			allowFound = true
		case "deny":
			denyFound = true
		}
	}

	// 실제 Casbin 정책 효과: some(allow) && !some(deny)
	return allowFound && !denyFound
}

// getAllRoles는 sub의 모든 상위 역할(상속 포함)을 반환한다.
// 실제: Casbin의 g(sub, role) role inheritance
func (e *Enforcer) getAllRoles(sub string) map[string]bool {
	roles := make(map[string]bool)
	e.collectRoles(sub, roles, 0)
	return roles
}

func (e *Enforcer) collectRoles(sub string, roles map[string]bool, depth int) {
	if depth > 10 {
		return // 순환 방지
	}
	for _, binding := range e.roleBindings {
		if binding.Sub == sub && !roles[binding.Role] {
			roles[binding.Role] = true
			e.collectRoles(binding.Role, roles, depth+1)
		}
	}
}

// EnforceErr는 허용되지 않으면 상세 에러를 반환한다.
// 실제: util/rbac/rbac.go:331-358
func (e *Enforcer) EnforceErr(sub, res, act, obj string) error {
	if !e.Enforce(sub, res, act, obj) {
		return fmt.Errorf("permission denied: user=%s, resource=%s, action=%s, object=%s",
			sub, res, act, obj)
	}
	return nil
}

// EnforceRuntimePolicy는 프로젝트별 런타임 정책으로 enforce한다.
// 실제: util/rbac/rbac.go:363-366
// 프로젝트 정책은 내장/사용자 정책을 보완하되, deny가 우선함
func (e *Enforcer) EnforceRuntimePolicy(sub, res, act, obj, projectPolicy string) bool {
	if projectPolicy == "" {
		return e.Enforce(sub, res, act, obj)
	}
	// 프로젝트 정책을 임시 추가하여 평가
	tempEnforcer := &Enforcer{
		cache:        NewEnforcerCache(time.Second),
		policies:     make([]Policy, len(e.policies)),
		roleBindings: make([]RoleBinding, len(e.roleBindings)),
		defaultRole:  e.defaultRole,
	}
	copy(tempEnforcer.policies, e.policies)
	copy(tempEnforcer.roleBindings, e.roleBindings)
	tempEnforcer.SetUserPolicy(projectPolicy)
	return tempEnforcer.Enforce(sub, res, act, obj)
}

// ─────────────────────────────────────────────
// 출력 헬퍼
// ─────────────────────────────────────────────

func printResult(sub, res, act, obj string, allowed bool) {
	icon := "DENY "
	if allowed {
		icon = "ALLOW"
	}
	fmt.Printf("  [%s] sub=%-25s res=%-15s act=%-10s obj=%s\n",
		icon, sub, res, act, obj)
}

func printSection(title string) {
	fmt.Printf("\n%s\n%s\n", strings.Repeat("=", 70), title)
}

func printSubSection(title string) {
	fmt.Printf("\n  --- %s ---\n", title)
}

// ─────────────────────────────────────────────
// main — 시나리오 실행
// ─────────────────────────────────────────────

func main() {
	fmt.Println("=== Argo CD RBAC 시스템 시뮬레이션 ===")
	fmt.Println()
	fmt.Println("참조: util/rbac/rbac.go, assets/model.conf, assets/builtin-policy.csv")
	fmt.Println()
	fmt.Println("Casbin 모델:")
	fmt.Println("  [request_definition]  r = sub, res, act, obj")
	fmt.Println("  [policy_definition]   p = sub, res, act, obj, eft")
	fmt.Println("  [role_definition]     g = _, _  (역할 상속)")
	fmt.Println("  [policy_effect]       e = some(allow) && !some(deny)")
	fmt.Println("  [matchers]            g(r.sub, p.sub) && glob(r.res,p.res) && glob(r.act,p.act) && glob(r.obj,p.obj)")

	enf := NewEnforcer()

	// ─────────────────────────────────────────────
	// 시나리오 1: role:readonly 내장 권한
	// ─────────────────────────────────────────────
	printSection("시나리오 1: role:readonly 내장 권한 (get 전용)")
	fmt.Println("  assets/builtin-policy.csv: role:readonly는 모든 리소스 get만 허용")

	printSubSection("get 허용")
	readonlyGetCases := [][3]string{
		{"applications", "get", "myproject/myapp"},
		{"clusters", "get", "https://k8s.example.com"},
		{"repositories", "get", "*"},
		{"logs", "get", "myproject/myapp"},
	}
	for _, c := range readonlyGetCases {
		printResult("role:readonly", c[0], c[1], c[2], enf.Enforce("role:readonly", c[0], c[1], c[2]))
	}

	printSubSection("쓰기 거부")
	readonlyDenyCases := [][3]string{
		{"applications", "create", "myproject/myapp"},
		{"applications", "sync", "myproject/myapp"},
		{"clusters", "delete", "*"},
		{"exec", "create", "myproject/myapp"},
	}
	for _, c := range readonlyDenyCases {
		printResult("role:readonly", c[0], c[1], c[2], enf.Enforce("role:readonly", c[0], c[1], c[2]))
	}

	// ─────────────────────────────────────────────
	// 시나리오 2: role:admin 내장 권한
	// ─────────────────────────────────────────────
	printSection("시나리오 2: role:admin 내장 권한 (전체)")
	fmt.Println("  role:admin은 role:readonly를 상속 + 모든 쓰기 권한")
	fmt.Println("  g, role:admin, role:readonly  (상속)")

	adminCases := [][3]string{
		{"applications", "get", "myproject/myapp"},     // readonly 상속
		{"applications", "create", "myproject/myapp"},
		{"applications", "sync", "*/"},
		{"applications", "delete", "myproject/myapp"},
		{"clusters", "create", "*"},
		{"clusters", "delete", "*"},
		{"exec", "create", "myproject/myapp"},
		{"projects", "delete", "*"},
	}
	for _, c := range adminCases {
		printResult("role:admin", c[0], c[1], c[2], enf.Enforce("role:admin", c[0], c[1], c[2]))
	}

	// ─────────────────────────────────────────────
	// 시나리오 3: admin 사용자 (g, admin, role:admin)
	// ─────────────────────────────────────────────
	printSection("시나리오 3: admin 사용자 (g, admin, role:admin)")
	fmt.Println("  builtin-policy.csv: g, admin, role:admin")
	fmt.Println("  사용자 'admin'은 role:admin 역할을 가짐")

	printResult("admin", "applications", "get", "*/*", enf.Enforce("admin", "applications", "get", "*/*"))
	printResult("admin", "clusters", "delete", "*", enf.Enforce("admin", "clusters", "delete", "*"))
	printResult("admin", "exec", "create", "*/pod", enf.Enforce("admin", "exec", "create", "*/pod"))

	// ─────────────────────────────────────────────
	// 시나리오 4: defaultRole 설정
	// ─────────────────────────────────────────────
	printSection("시나리오 4: defaultRole (모든 인증 사용자의 기본 역할)")
	fmt.Println("  ConfigMap policy.default=role:readonly 설정 시")
	fmt.Println("  인증된 모든 사용자는 role:readonly 권한을 기본으로 가짐")
	fmt.Println("  실제: util/rbac/rbac.go:383-387 defaultRole 체크")

	enf.SetDefaultRole("role:readonly")

	// defaultRole로 인해 모든 사용자가 get 가능
	fmt.Println("\n  defaultRole=role:readonly 설정 후:")
	printResult("alice", "applications", "get", "myproject/myapp", enf.Enforce("alice", "applications", "get", "myproject/myapp"))
	printResult("alice", "applications", "sync", "myproject/myapp", enf.Enforce("alice", "applications", "sync", "myproject/myapp"))
	printResult("bob", "clusters", "get", "*", enf.Enforce("bob", "clusters", "get", "*"))

	enf.SetDefaultRole("") // 리셋

	// ─────────────────────────────────────────────
	// 시나리오 5: 사용자 정의 정책 (ConfigMap policy.csv)
	// ─────────────────────────────────────────────
	printSection("시나리오 5: 사용자 정의 정책 (ConfigMap policy.csv)")
	fmt.Println("  ConfigMap argocd-rbac-cm의 policy.csv 항목")

	userPolicy := `
# 프로젝트별 역할 정의
p, role:team-a-admin, applications, *, team-a/*, allow
p, role:team-a-admin, logs, get, team-a/*, allow
p, role:team-a-readonly, applications, get, team-a/*, allow

# 글로벌 리소스 (프로젝트 범위 없음)
p, role:cluster-viewer, clusters, get, *, allow
p, role:cert-manager, certificates, create, *, allow
p, role:cert-manager, certificates, delete, *, allow

# 사용자-역할 바인딩
g, alice, role:team-a-admin
g, bob, role:team-a-readonly
g, charlie, role:cluster-viewer
g, diana, role:cert-manager

# 특정 사용자 직접 정책
p, eve, applications, get, */*, allow
p, eve, applications, sync, myproject/*, allow
`

	enf.SetUserPolicy(userPolicy)

	fmt.Println("\n  team-a-admin (alice):")
	printResult("alice", "applications", "get", "team-a/myapp", enf.Enforce("alice", "applications", "get", "team-a/myapp"))
	printResult("alice", "applications", "sync", "team-a/myapp", enf.Enforce("alice", "applications", "sync", "team-a/myapp"))
	printResult("alice", "applications", "delete", "team-a/myapp", enf.Enforce("alice", "applications", "delete", "team-a/myapp"))
	// team-b 접근 거부
	printResult("alice", "applications", "get", "team-b/otherapp", enf.Enforce("alice", "applications", "get", "team-b/otherapp"))

	fmt.Println("\n  team-a-readonly (bob):")
	printResult("bob", "applications", "get", "team-a/myapp", enf.Enforce("bob", "applications", "get", "team-a/myapp"))
	printResult("bob", "applications", "sync", "team-a/myapp", enf.Enforce("bob", "applications", "sync", "team-a/myapp"))

	fmt.Println("\n  직접 정책 (eve):")
	printResult("eve", "applications", "get", "anyproject/anyapp", enf.Enforce("eve", "applications", "get", "anyproject/anyapp"))
	printResult("eve", "applications", "sync", "myproject/myapp", enf.Enforce("eve", "applications", "sync", "myproject/myapp"))
	printResult("eve", "applications", "sync", "otherproject/myapp", enf.Enforce("eve", "applications", "sync", "otherproject/myapp"))

	// ─────────────────────────────────────────────
	// 시나리오 6: Glob 패턴 매칭
	// ─────────────────────────────────────────────
	printSection("시나리오 6: Glob 패턴 매칭")
	fmt.Println("  실제: util/glob/glob.go, util/rbac/rbac.go:282-298 globMatchFunc()")
	fmt.Println("  지원: *, ?, [...] — filepath.Match 기반")
	fmt.Println()

	globCases := [][2]string{
		// pattern, str
		{"*/", "myproject/"},
		{"*/*", "myproject/myapp"},
		{"myproject/*", "myproject/myapp"},
		{"myproject/*", "otherproject/myapp"},
		{"*/myapp", "myproject/myapp"},
		{"*/myapp", "myproject/otherapp"},
		{"*", "anysinglevalue"},
		{"team-?/*", "team-a/myapp"},
		{"team-?/*", "team-ab/myapp"},
		{"[abc]*/*", "admin/myapp"},
		{"[abc]*/*", "dev/myapp"},
	}

	fmt.Printf("  %-25s %-25s %s\n", "Pattern", "String", "Match")
	fmt.Println("  " + strings.Repeat("-", 60))
	for _, c := range globCases {
		matched := globMatch(c[0], c[1])
		icon := "false"
		if matched {
			icon = "true "
		}
		fmt.Printf("  %-25s %-25s %s\n", c[0], c[1], icon)
	}

	// ─────────────────────────────────────────────
	// 시나리오 7: Deny 정책 (allow과 deny 동시 적용)
	// ─────────────────────────────────────────────
	printSection("시나리오 7: Deny 정책 (allow + deny)")
	fmt.Println("  정책 효과: some(allow) && !some(deny)")
	fmt.Println("  deny가 하나라도 있으면 allow가 있어도 거부됨")

	enf2 := NewEnforcer()
	denyPolicy := `
p, role:dev, applications, *, */*, allow
p, role:dev, applications, delete, */*, deny
g, frank, role:dev
`
	enf2.SetUserPolicy(denyPolicy)

	fmt.Println("\n  role:dev (frank) — delete는 deny:")
	printResult("frank", "applications", "get", "myproject/myapp", enf2.Enforce("frank", "applications", "get", "myproject/myapp"))
	printResult("frank", "applications", "sync", "myproject/myapp", enf2.Enforce("frank", "applications", "sync", "myproject/myapp"))
	printResult("frank", "applications", "delete", "myproject/myapp", enf2.Enforce("frank", "applications", "delete", "myproject/myapp"))

	// ─────────────────────────────────────────────
	// 시나리오 8: 프로젝트 런타임 정책
	// ─────────────────────────────────────────────
	printSection("시나리오 8: 프로젝트 런타임 정책 (AppProject.spec.roles)")
	fmt.Println("  AppProject의 spec.roles로 프로젝트별 RBAC 정의")
	fmt.Println("  실제: util/rbac/rbac.go:363-373 EnforceRuntimePolicy()")
	fmt.Println("  전역 정책의 deny는 프로젝트 정책보다 우선함")

	enf3 := NewEnforcer()
	// 전역 정책: 기본적으로 alice는 readonly
	globalPolicy := `
g, alice, role:readonly
`
	enf3.SetUserPolicy(globalPolicy)

	// 프로젝트 정책: myproject에서 alice에게 sync 권한
	projectPolicy := `
p, alice, applications, sync, myproject/*, allow
`

	fmt.Println("\n  전역 정책만 (readonly):")
	printResult("alice", "applications", "get", "myproject/myapp", enf3.Enforce("alice", "applications", "get", "myproject/myapp"))
	printResult("alice", "applications", "sync", "myproject/myapp", enf3.Enforce("alice", "applications", "sync", "myproject/myapp"))

	fmt.Println("\n  프로젝트 런타임 정책 추가 후:")
	printResult("alice", "applications", "sync", "myproject/myapp",
		enf3.EnforceRuntimePolicy("alice", "applications", "sync", "myproject/myapp", projectPolicy))
	printResult("alice", "applications", "sync", "otherproject/myapp",
		enf3.EnforceRuntimePolicy("alice", "applications", "sync", "otherproject/myapp", projectPolicy))

	// ─────────────────────────────────────────────
	// 시나리오 9: EnforceErr 상세 에러
	// ─────────────────────────────────────────────
	printSection("시나리오 9: EnforceErr 상세 에러 메시지")
	fmt.Println("  실제: util/rbac/rbac.go:331-358 EnforceErr()")

	enf4 := NewEnforcer()
	enf4.SetUserPolicy(`g, alice, role:readonly`)

	cases9 := []struct {
		sub, res, act, obj string
	}{
		{"alice", "applications", "get", "myproject/myapp"},
		{"alice", "applications", "delete", "myproject/myapp"},
		{"bob", "clusters", "create", "*"},
	}

	for _, c := range cases9 {
		err := enf4.EnforceErr(c.sub, c.res, c.act, c.obj)
		if err != nil {
			fmt.Printf("  ERROR: %v\n", err)
		} else {
			fmt.Printf("  OK: sub=%s, res=%s, act=%s, obj=%s\n",
				c.sub, c.res, c.act, c.obj)
		}
	}

	// ─────────────────────────────────────────────
	// 역할 상속 시각화
	// ─────────────────────────────────────────────
	printSection("역할 상속 구조 시각화")
	fmt.Println("  실제: Casbin g(_, _) role inheritance")
	fmt.Println()

	enf5 := NewEnforcer()
	enf5.SetUserPolicy(`
g, alice, role:team-a-admin
g, role:team-a-admin, role:readonly
g, bob, role:readonly
g, charlie, role:admin
`)

	type userRoleInfo struct {
		user  string
		roles []string
	}

	users := []string{"alice", "bob", "charlie", "admin"}
	var infos []userRoleInfo

	for _, user := range users {
		roles := enf5.getAllRoles(user)
		roleList := make([]string, 0, len(roles))
		for r := range roles {
			roleList = append(roleList, r)
		}
		sort.Strings(roleList)
		infos = append(infos, userRoleInfo{user: user, roles: roleList})
	}

	for _, info := range infos {
		fmt.Printf("  %-10s → 역할: %s\n", info.user, strings.Join(info.roles, ", "))
	}

	// 요약
	fmt.Println()
	fmt.Println(strings.Repeat("=", 70))
	fmt.Println("RBAC 시스템 핵심 포인트:")
	fmt.Println("  1. Casbin 모델: sub+res+act+obj+eft로 정책 정의")
	fmt.Println("  2. 정책 효과: some(allow) && !some(deny) — deny 우선")
	fmt.Println("  3. role:readonly → get 전용, role:admin → 전체 (+ readonly 상속)")
	fmt.Println("  4. g(sub, role): 역할 상속으로 계층적 권한 구조")
	fmt.Println("  5. defaultRole: 인증된 모든 사용자의 기본 역할")
	fmt.Println("  6. glob 매칭: *, ?, [...] 패턴으로 유연한 리소스 지정")
	fmt.Println("  7. 프로젝트 정책: AppProject.spec.roles로 프로젝트별 RBAC")
	fmt.Println("  8. ProjectScoped 리소스: 오브젝트 형식이 project/resource")
}
