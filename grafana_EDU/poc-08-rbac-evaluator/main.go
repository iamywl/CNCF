package main

import (
	"fmt"
	"strings"
)

// =============================================================================
// Grafana RBAC 평가기 시뮬레이션
//
// Grafana의 RBAC 시스템은 pkg/services/accesscontrol/ 에 구현되어 있다.
// Permission은 Action + Scope의 조합으로 표현되며,
// Evaluator 인터페이스를 통해 권한을 평가한다.
// =============================================================================

// OrgRole은 조직 내 기본 역할이다.
type OrgRole string

const (
	RoleNone         OrgRole = "None"
	RoleViewer       OrgRole = "Viewer"
	RoleEditor       OrgRole = "Editor"
	RoleAdmin        OrgRole = "Admin"
	RoleGrafanaAdmin OrgRole = "GrafanaAdmin"
)

// Permission은 Action + Scope 조합의 단일 권한이다.
// Grafana: pkg/services/accesscontrol/models.go
type Permission struct {
	Action string // e.g., "dashboards:read", "datasources:write"
	Scope  string // e.g., "dashboards:uid:abc123", "dashboards:*", "*"
}

func (p Permission) String() string {
	if p.Scope == "" {
		return p.Action
	}
	return fmt.Sprintf("%s @ %s", p.Action, p.Scope)
}

// User는 인증된 사용자 정보이다.
type User struct {
	ID          int64
	Login       string
	OrgID       int64
	OrgRole     OrgRole
	Teams       []int64
	Permissions []Permission // 사용자에게 부여된 세분화된 권한
}

// Evaluator는 권한 평가 인터페이스이다.
// Grafana: pkg/services/accesscontrol/evaluator/evaluator.go
type Evaluator interface {
	// Evaluate는 사용자가 해당 권한을 가지고 있는지 평가한다.
	Evaluate(user *User) (bool, string)
	// String은 평가기의 문자열 표현을 반환한다.
	String() string
}

// =============================================================================
// EvalPermission - 단일 권한 평가
// =============================================================================

// permissionEvaluator는 단일 Action + Scope 매칭을 수행한다.
type permissionEvaluator struct {
	action string
	scopes []string
}

// EvalPermission은 단일 권한 평가기를 생성한다.
// scopes가 비어있으면 Action만 확인한다.
// scopes가 있으면 하나라도 매칭되면 통과한다.
func EvalPermission(action string, scopes ...string) Evaluator {
	return &permissionEvaluator{action: action, scopes: scopes}
}

func (e *permissionEvaluator) Evaluate(user *User) (bool, string) {
	for _, perm := range user.Permissions {
		if perm.Action != e.action {
			continue
		}

		// Scope 조건이 없으면 Action만 일치하면 통과
		if len(e.scopes) == 0 {
			return true, fmt.Sprintf("matched action '%s' (no scope required)", e.action)
		}

		// Scope가 있으면 하나라도 매칭되면 통과
		for _, requiredScope := range e.scopes {
			if matchScope(perm.Scope, requiredScope) {
				return true, fmt.Sprintf("matched action '%s', scope '%s' covers '%s'", e.action, perm.Scope, requiredScope)
			}
		}
	}

	if len(e.scopes) == 0 {
		return false, fmt.Sprintf("no permission with action '%s'", e.action)
	}
	return false, fmt.Sprintf("no permission with action '%s' covering scopes %v", e.action, e.scopes)
}

func (e *permissionEvaluator) String() string {
	if len(e.scopes) == 0 {
		return fmt.Sprintf("Perm(%s)", e.action)
	}
	return fmt.Sprintf("Perm(%s, %v)", e.action, e.scopes)
}

// matchScope는 가진 권한의 scope가 요구되는 scope를 커버하는지 확인한다.
// Grafana: pkg/services/accesscontrol/scope.go
func matchScope(haveScope, requiredScope string) bool {
	// 완전 일치
	if haveScope == requiredScope {
		return true
	}

	// 글로벌 와일드카드
	if haveScope == "*" {
		return true
	}

	// 와일드카드 매칭: "dashboards:*" matches "dashboards:uid:abc"
	if strings.HasSuffix(haveScope, ":*") {
		prefix := strings.TrimSuffix(haveScope, "*")
		if strings.HasPrefix(requiredScope, prefix) {
			return true
		}
	}

	// 세그먼트별 와일드카드: "dashboards:uid:*" matches "dashboards:uid:abc123"
	haveParts := strings.Split(haveScope, ":")
	reqParts := strings.Split(requiredScope, ":")

	if len(haveParts) != len(reqParts) {
		// 길이가 다르면 마지막이 *일 때만 매칭
		if len(haveParts) > 0 && haveParts[len(haveParts)-1] == "*" {
			prefix := strings.Join(haveParts[:len(haveParts)-1], ":")
			reqPrefix := strings.Join(reqParts[:len(haveParts)-1], ":")
			return prefix == reqPrefix
		}
		return false
	}

	for i, part := range haveParts {
		if part == "*" {
			continue
		}
		if part != reqParts[i] {
			return false
		}
	}
	return true
}

// =============================================================================
// EvalAll - AND 조합
// =============================================================================

type allEvaluator struct {
	evaluators []Evaluator
}

// EvalAll은 모든 평가기가 통과해야 하는 AND 조합 평가기를 생성한다.
func EvalAll(evaluators ...Evaluator) Evaluator {
	return &allEvaluator{evaluators: evaluators}
}

func (e *allEvaluator) Evaluate(user *User) (bool, string) {
	var reasons []string
	for _, eval := range e.evaluators {
		ok, reason := eval.Evaluate(user)
		reasons = append(reasons, fmt.Sprintf("  %s: %v (%s)", eval.String(), ok, reason))
		if !ok {
			return false, fmt.Sprintf("ALL failed - %s denied:\n%s", eval.String(), strings.Join(reasons, "\n"))
		}
	}
	return true, fmt.Sprintf("ALL passed:\n%s", strings.Join(reasons, "\n"))
}

func (e *allEvaluator) String() string {
	parts := make([]string, len(e.evaluators))
	for i, eval := range e.evaluators {
		parts[i] = eval.String()
	}
	return fmt.Sprintf("All(%s)", strings.Join(parts, " AND "))
}

// =============================================================================
// EvalAny - OR 조합
// =============================================================================

type anyEvaluator struct {
	evaluators []Evaluator
}

// EvalAny는 하나라도 통과하면 되는 OR 조합 평가기를 생성한다.
func EvalAny(evaluators ...Evaluator) Evaluator {
	return &anyEvaluator{evaluators: evaluators}
}

func (e *anyEvaluator) Evaluate(user *User) (bool, string) {
	var reasons []string
	for _, eval := range e.evaluators {
		ok, reason := eval.Evaluate(user)
		reasons = append(reasons, fmt.Sprintf("  %s: %v (%s)", eval.String(), ok, reason))
		if ok {
			return true, fmt.Sprintf("ANY passed - %s granted:\n%s", eval.String(), strings.Join(reasons, "\n"))
		}
	}
	return false, fmt.Sprintf("ANY failed - none granted:\n%s", strings.Join(reasons, "\n"))
}

func (e *anyEvaluator) String() string {
	parts := make([]string, len(e.evaluators))
	for i, eval := range e.evaluators {
		parts[i] = eval.String()
	}
	return fmt.Sprintf("Any(%s)", strings.Join(parts, " OR "))
}

// =============================================================================
// ScopeAttributeResolver - UID를 리소스 속성으로 변환
// =============================================================================

// ResourceInfo는 리소스의 속성 정보이다.
type ResourceInfo struct {
	UID       string
	Name      string
	FolderUID string
	OrgID     int64
	OwnerID   int64
}

// ScopeAttributeResolver는 scope에서 리소스 정보를 조회한다.
type ScopeAttributeResolver struct {
	dashboards  map[string]*ResourceInfo
	datasources map[string]*ResourceInfo
	folders     map[string]*ResourceInfo
}

func NewScopeAttributeResolver() *ScopeAttributeResolver {
	return &ScopeAttributeResolver{
		dashboards: map[string]*ResourceInfo{
			"abc123": {UID: "abc123", Name: "System Overview", FolderUID: "infra", OrgID: 1, OwnerID: 1},
			"def456": {UID: "def456", Name: "App Metrics", FolderUID: "dev", OrgID: 1, OwnerID: 3},
			"ghi789": {UID: "ghi789", Name: "Security Audit", FolderUID: "security", OrgID: 1, OwnerID: 1},
		},
		datasources: map[string]*ResourceInfo{
			"prometheus": {UID: "prometheus", Name: "Prometheus", OrgID: 1, OwnerID: 1},
			"loki":       {UID: "loki", Name: "Loki", OrgID: 1, OwnerID: 1},
		},
		folders: map[string]*ResourceInfo{
			"infra":    {UID: "infra", Name: "Infrastructure", OrgID: 1, OwnerID: 1},
			"dev":      {UID: "dev", Name: "Development", OrgID: 1, OwnerID: 2},
			"security": {UID: "security", Name: "Security", OrgID: 1, OwnerID: 1},
		},
	}
}

func (r *ScopeAttributeResolver) Resolve(scope string) (*ResourceInfo, bool) {
	parts := strings.Split(scope, ":")
	if len(parts) < 3 {
		return nil, false
	}

	resourceType := parts[0]
	uid := parts[2]

	switch resourceType {
	case "dashboards":
		info, ok := r.dashboards[uid]
		return info, ok
	case "datasources":
		info, ok := r.datasources[uid]
		return info, ok
	case "folders":
		info, ok := r.folders[uid]
		return info, ok
	}
	return nil, false
}

// =============================================================================
// 기본 역할별 권한 세트
// =============================================================================

// RolePermissions는 기본 역할별 권한 세트를 반환한다.
func RolePermissions(role OrgRole) []Permission {
	switch role {
	case RoleViewer:
		return []Permission{
			{Action: "dashboards:read", Scope: "dashboards:*"},
			{Action: "datasources:read", Scope: "datasources:*"},
			{Action: "folders:read", Scope: "folders:*"},
		}
	case RoleEditor:
		return []Permission{
			{Action: "dashboards:read", Scope: "dashboards:*"},
			{Action: "dashboards:write", Scope: "dashboards:*"},
			{Action: "dashboards:create", Scope: "folders:*"},
			{Action: "datasources:read", Scope: "datasources:*"},
			{Action: "datasources:explore", Scope: "datasources:*"},
			{Action: "folders:read", Scope: "folders:*"},
			{Action: "folders:write", Scope: "folders:*"},
		}
	case RoleAdmin:
		return []Permission{
			{Action: "dashboards:read", Scope: "dashboards:*"},
			{Action: "dashboards:write", Scope: "dashboards:*"},
			{Action: "dashboards:create", Scope: "folders:*"},
			{Action: "dashboards:delete", Scope: "dashboards:*"},
			{Action: "dashboards:permissions:read", Scope: "dashboards:*"},
			{Action: "dashboards:permissions:write", Scope: "dashboards:*"},
			{Action: "datasources:read", Scope: "datasources:*"},
			{Action: "datasources:write", Scope: "datasources:*"},
			{Action: "datasources:create", Scope: "*"},
			{Action: "datasources:delete", Scope: "datasources:*"},
			{Action: "folders:read", Scope: "folders:*"},
			{Action: "folders:write", Scope: "folders:*"},
			{Action: "folders:create", Scope: "*"},
			{Action: "folders:delete", Scope: "folders:*"},
			{Action: "users:read", Scope: "users:*"},
			{Action: "orgs:read", Scope: "*"},
		}
	case RoleGrafanaAdmin:
		return []Permission{
			{Action: "*", Scope: "*"},
		}
	default:
		return nil
	}
}

// =============================================================================
// 팀 기반 권한
// =============================================================================

// TeamPermissions는 팀별 추가 권한을 반환한다.
func TeamPermissions(teamID int64) []Permission {
	teamPerms := map[int64][]Permission{
		1: { // Infrastructure Team
			{Action: "dashboards:write", Scope: "folders:uid:infra"},
			{Action: "dashboards:delete", Scope: "folders:uid:infra"},
			{Action: "datasources:write", Scope: "datasources:uid:prometheus"},
		},
		2: { // Dev Team
			{Action: "dashboards:write", Scope: "folders:uid:dev"},
			{Action: "datasources:read", Scope: "datasources:uid:loki"},
		},
		3: { // Security Team
			{Action: "dashboards:read", Scope: "folders:uid:security"},
			{Action: "dashboards:write", Scope: "folders:uid:security"},
		},
	}
	return teamPerms[teamID]
}

// BuildUserPermissions는 역할 + 팀 권한을 합산한다.
func BuildUserPermissions(user *User) {
	// 기본 역할 권한
	user.Permissions = RolePermissions(user.OrgRole)

	// 팀 권한 추가
	for _, teamID := range user.Teams {
		user.Permissions = append(user.Permissions, TeamPermissions(teamID)...)
	}
}

// =============================================================================
// 메인
// =============================================================================

func main() {
	fmt.Println("=== Grafana RBAC 평가기 시뮬레이션 ===")
	fmt.Println()

	resolver := NewScopeAttributeResolver()

	// ─── 사용자 생성 ───
	adminUser := &User{
		ID: 1, Login: "admin", OrgID: 1, OrgRole: RoleAdmin,
		Teams: []int64{1, 3},
	}
	BuildUserPermissions(adminUser)

	viewerUser := &User{
		ID: 2, Login: "viewer", OrgID: 1, OrgRole: RoleViewer,
		Teams: []int64{2},
	}
	BuildUserPermissions(viewerUser)

	editorUser := &User{
		ID: 3, Login: "editor", OrgID: 1, OrgRole: RoleEditor,
		Teams: []int64{1, 2},
	}
	BuildUserPermissions(editorUser)

	grafanaAdminUser := &User{
		ID: 4, Login: "grafana-admin", OrgID: 1, OrgRole: RoleGrafanaAdmin,
		Teams: []int64{},
	}
	BuildUserPermissions(grafanaAdminUser)

	// ─── 사용자 권한 출력 ───
	users := []*User{adminUser, viewerUser, editorUser, grafanaAdminUser}
	for _, u := range users {
		fmt.Printf("━━━ 사용자: %s (역할: %s, 팀: %v) ━━━\n", u.Login, u.OrgRole, u.Teams)
		fmt.Printf("  권한 수: %d\n", len(u.Permissions))
		for _, p := range u.Permissions {
			fmt.Printf("    - %s\n", p)
		}
		fmt.Println()
	}

	// ─── 시나리오 1: Admin이 대시보드 편집 ───
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("시나리오 1: Admin이 아무 대시보드나 편집할 수 있는가?")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	eval1 := EvalPermission("dashboards:write", "dashboards:uid:abc123")
	ok, reason := eval1.Evaluate(adminUser)
	fmt.Printf("  평가기: %s\n", eval1)
	fmt.Printf("  결과: %v\n", ok)
	fmt.Printf("  이유: %s\n\n", reason)

	// ─── 시나리오 2: Viewer는 읽기만 가능 ───
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("시나리오 2: Viewer가 대시보드를 읽을 수 있는가? 쓸 수 있는가?")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	evalRead := EvalPermission("dashboards:read", "dashboards:uid:abc123")
	ok, reason = evalRead.Evaluate(viewerUser)
	fmt.Printf("  [읽기] 평가기: %s\n", evalRead)
	fmt.Printf("  결과: %v\n", ok)
	fmt.Printf("  이유: %s\n\n", reason)

	evalWrite := EvalPermission("dashboards:write", "dashboards:uid:abc123")
	ok, reason = evalWrite.Evaluate(viewerUser)
	fmt.Printf("  [쓰기] 평가기: %s\n", evalWrite)
	fmt.Printf("  결과: %v\n", ok)
	fmt.Printf("  이유: %s\n\n", reason)

	// ─── 시나리오 3: Editor + 특정 폴더 권한 (팀 기반) ───
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("시나리오 3: Editor(팀 1,2)의 폴더별 권한 차이")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	// Editor는 기본적으로 모든 대시보드에 write 가능 (dashboards:write + dashboards:*)
	evalInfra := EvalPermission("dashboards:write", "folders:uid:infra")
	ok, reason = evalInfra.Evaluate(editorUser)
	fmt.Printf("  [infra 폴더 쓰기] 평가기: %s\n", evalInfra)
	fmt.Printf("  결과: %v\n", ok)
	fmt.Printf("  이유: %s\n\n", reason)

	// Editor는 기본적으로 delete 권한이 없지만, 팀 1로부터 infra 폴더 삭제 권한을 받음
	evalDeleteInfra := EvalPermission("dashboards:delete", "folders:uid:infra")
	ok, reason = evalDeleteInfra.Evaluate(editorUser)
	fmt.Printf("  [infra 폴더 삭제 - 팀 권한] 평가기: %s\n", evalDeleteInfra)
	fmt.Printf("  결과: %v\n", ok)
	fmt.Printf("  이유: %s\n\n", reason)

	evalDeleteSecurity := EvalPermission("dashboards:delete", "folders:uid:security")
	ok, reason = evalDeleteSecurity.Evaluate(editorUser)
	fmt.Printf("  [security 폴더 삭제 - 팀 없음] 평가기: %s\n", evalDeleteSecurity)
	fmt.Printf("  결과: %v\n", ok)
	fmt.Printf("  이유: %s\n\n", reason)

	// ─── 시나리오 4: EvalAll (AND 조합) ───
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("시나리오 4: AND 조합 - 대시보드 읽기 + 데이터소스 읽기 동시 필요")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	evalAll := EvalAll(
		EvalPermission("dashboards:read", "dashboards:uid:abc123"),
		EvalPermission("datasources:read", "datasources:uid:prometheus"),
	)

	for _, u := range users {
		ok, reason = evalAll.Evaluate(u)
		fmt.Printf("  [%s] %v\n", u.Login, ok)
		fmt.Printf("    %s\n\n", strings.ReplaceAll(reason, "\n", "\n    "))
	}

	// ─── 시나리오 5: EvalAny (OR 조합) ───
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("시나리오 5: OR 조합 - Admin이거나 폴더 소유자")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	evalAny := EvalAny(
		EvalPermission("folders:delete", "folders:uid:infra"),
		EvalPermission("orgs:read"),
	)

	for _, u := range users {
		ok, reason = evalAny.Evaluate(u)
		fmt.Printf("  [%s] %v\n", u.Login, ok)
		fmt.Printf("    %s\n\n", strings.ReplaceAll(reason, "\n", "\n    "))
	}

	// ─── 시나리오 6: 복잡한 중첩 평가 ───
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("시나리오 6: 복잡한 중첩 - (읽기 AND 쓰기) OR GrafanaAdmin")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	evalComplex := EvalAny(
		EvalAll(
			EvalPermission("dashboards:read", "dashboards:uid:ghi789"),
			EvalPermission("dashboards:write", "dashboards:uid:ghi789"),
		),
		EvalPermission("*"), // GrafanaAdmin의 글로벌 권한
	)
	fmt.Printf("  평가기: %s\n\n", evalComplex)

	for _, u := range users {
		ok, reason = evalComplex.Evaluate(u)
		fmt.Printf("  [%s (role=%s)] %v\n", u.Login, u.OrgRole, ok)
		fmt.Printf("    %s\n\n", strings.ReplaceAll(reason, "\n", "\n    "))
	}

	// ─── ScopeAttributeResolver 데모 ───
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("ScopeAttributeResolver - UID로 리소스 정보 조회")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	scopes := []string{
		"dashboards:uid:abc123",
		"dashboards:uid:def456",
		"datasources:uid:prometheus",
		"folders:uid:infra",
		"dashboards:uid:unknown",
	}

	for _, scope := range scopes {
		info, found := resolver.Resolve(scope)
		if found {
			fmt.Printf("  %s\n", scope)
			fmt.Printf("    → 이름: %s, 폴더: %s, 소유자ID: %d\n\n", info.Name, info.FolderUID, info.OwnerID)
		} else {
			fmt.Printf("  %s\n", scope)
			fmt.Printf("    → 리소스 없음\n\n")
		}
	}

	// ─── Scope 와일드카드 매칭 데모 ───
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("Scope 와일드카드 매칭 테스트")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	matchTests := []struct {
		have     string
		required string
	}{
		{"dashboards:*", "dashboards:uid:abc123"},
		{"dashboards:uid:*", "dashboards:uid:abc123"},
		{"dashboards:uid:abc123", "dashboards:uid:abc123"},
		{"dashboards:uid:abc123", "dashboards:uid:xyz"},
		{"*", "dashboards:uid:abc123"},
		{"folders:uid:infra", "folders:uid:dev"},
		{"datasources:*", "datasources:uid:prometheus"},
	}

	fmt.Printf("  %-30s %-30s %s\n", "보유 Scope", "요구 Scope", "매칭?")
	fmt.Println("  " + strings.Repeat("-", 75))
	for _, t := range matchTests {
		result := matchScope(t.have, t.required)
		mark := "X"
		if result {
			mark = "O"
		}
		fmt.Printf("  %-30s %-30s %s\n", t.have, t.required, mark)
	}

	fmt.Println()
	fmt.Println("=== RBAC 평가 완료 ===")
}
