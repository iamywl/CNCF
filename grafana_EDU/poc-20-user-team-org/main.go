package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Grafana 사용자/팀/조직 관리 PoC
//
// 이 PoC는 Grafana의 멀티 테넌시 핵심 개념을 시뮬레이션한다:
//   1. 사용자(User), 팀(Team), 조직(Organization) CRUD
//   2. 조직 기반 데이터 격리
//   3. 역할(Role) 계층 (Viewer, Editor, Admin)
//   4. RBAC 권한 평가
//   5. 팀 기반 권한 할당
//
// 실제 소스 참조:
//   - pkg/services/user/model.go         (User 모델)
//   - pkg/services/org/model.go          (Org, OrgUser 모델)
//   - pkg/services/team/model.go         (Team, TeamMember 모델)
//   - pkg/services/accesscontrol/        (RBAC 시스템)
// =============================================================================

// --- 역할 정의 (pkg/services/org/model.go 참조) ---

type RoleType string

const (
	RoleNone   RoleType = "None"
	RoleViewer RoleType = "Viewer"
	RoleEditor RoleType = "Editor"
	RoleAdmin  RoleType = "Admin"
)

// roleHierarchy는 역할 계층을 정의한다.
// 높은 숫자가 더 높은 권한을 의미한다.
var roleHierarchy = map[RoleType]int{
	RoleNone:   0,
	RoleViewer: 1,
	RoleEditor: 2,
	RoleAdmin:  3,
}

func (r RoleType) HasRole(required RoleType) bool {
	return roleHierarchy[r] >= roleHierarchy[required]
}

// --- 사용자 모델 (pkg/services/user/model.go 참조) ---

type User struct {
	ID               int64
	UID              string
	Email            string
	Login            string
	Name             string
	IsAdmin          bool // Grafana 전체 관리자
	IsServiceAccount bool
	IsDisabled       bool
	OrgID            int64 // 현재 활성 조직
	Created          time.Time
}

// --- 조직 모델 (pkg/services/org/model.go 참조) ---

type Organization struct {
	ID      int64
	Name    string
	Created time.Time
}

// OrgUser는 사용자-조직 관계를 나타낸다.
type OrgUser struct {
	OrgID  int64
	UserID int64
	Role   RoleType
}

// --- 팀 모델 (pkg/services/team/model.go 참조) ---

type Team struct {
	ID    int64
	UID   string
	OrgID int64
	Name  string
	Email string
}

type PermissionType int

const (
	PermissionTypeMember PermissionType = 0
	PermissionTypeAdmin  PermissionType = 4
)

type TeamMember struct {
	TeamID     int64
	UserID     int64
	Permission PermissionType
}

// --- RBAC 모델 (pkg/services/accesscontrol/models.go 참조) ---

// Permission은 Action + Scope로 구성된다.
type Permission struct {
	Action string
	Scope  string
}

// Role은 RBAC 역할이다. 여러 Permission을 가진다.
type Role struct {
	Name        string
	Permissions []Permission
}

// --- 서비스 구현 ---

// MultiTenantService는 사용자/팀/조직 관리를 통합한 서비스이다.
type MultiTenantService struct {
	mu sync.RWMutex

	// 데이터 저장소 (인메모리)
	users    map[int64]*User
	orgs     map[int64]*Organization
	teams    map[int64]*Team
	orgUsers []OrgUser
	teamMembers []TeamMember

	// RBAC
	roleAssignments map[string][]Permission // "role:orgID" -> Permissions
	userPermissions map[string][]Permission // "userID:orgID" -> additional Permissions

	// 자동 증가 ID
	nextUserID int64
	nextOrgID  int64
	nextTeamID int64
}

func NewMultiTenantService() *MultiTenantService {
	s := &MultiTenantService{
		users:           make(map[int64]*User),
		orgs:            make(map[int64]*Organization),
		teams:           make(map[int64]*Team),
		roleAssignments: make(map[string][]Permission),
		userPermissions: make(map[string][]Permission),
	}

	// 기본 역할별 권한 등록 (Fixed Roles)
	s.registerDefaultPermissions()

	return s
}

// registerDefaultPermissions는 기본 역할별 권한을 등록한다.
func (s *MultiTenantService) registerDefaultPermissions() {
	// Viewer 권한
	s.roleAssignments["Viewer"] = []Permission{
		{Action: "dashboards:read", Scope: "dashboards:*"},
		{Action: "folders:read", Scope: "folders:*"},
		{Action: "explore:read", Scope: ""},
	}

	// Editor 권한 (Viewer 포함)
	s.roleAssignments["Editor"] = []Permission{
		{Action: "dashboards:read", Scope: "dashboards:*"},
		{Action: "dashboards:write", Scope: "dashboards:*"},
		{Action: "dashboards:create", Scope: "folders:*"},
		{Action: "folders:read", Scope: "folders:*"},
		{Action: "folders:write", Scope: "folders:*"},
		{Action: "explore:read", Scope: ""},
		{Action: "datasources:query", Scope: "datasources:*"},
	}

	// Admin 권한 (Editor 포함)
	s.roleAssignments["Admin"] = []Permission{
		{Action: "dashboards:read", Scope: "dashboards:*"},
		{Action: "dashboards:write", Scope: "dashboards:*"},
		{Action: "dashboards:create", Scope: "folders:*"},
		{Action: "dashboards:delete", Scope: "dashboards:*"},
		{Action: "folders:read", Scope: "folders:*"},
		{Action: "folders:write", Scope: "folders:*"},
		{Action: "explore:read", Scope: ""},
		{Action: "datasources:query", Scope: "datasources:*"},
		{Action: "datasources:read", Scope: "datasources:*"},
		{Action: "datasources:write", Scope: "datasources:*"},
		{Action: "teams:read", Scope: "teams:*"},
		{Action: "teams:write", Scope: "teams:*"},
		{Action: "org.users:read", Scope: "users:*"},
		{Action: "org.users:write", Scope: "users:*"},
	}
}

// --- 조직 관리 ---

func (s *MultiTenantService) CreateOrg(name string, creatorUserID int64) *Organization {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextOrgID++
	org := &Organization{
		ID:      s.nextOrgID,
		Name:    name,
		Created: time.Now(),
	}
	s.orgs[org.ID] = org

	// 생성자를 Admin으로 추가
	s.orgUsers = append(s.orgUsers, OrgUser{
		OrgID:  org.ID,
		UserID: creatorUserID,
		Role:   RoleAdmin,
	})

	return org
}

// --- 사용자 관리 ---

func (s *MultiTenantService) CreateUser(login, email, name string, orgID int64, role RoleType) *User {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextUserID++
	user := &User{
		ID:      s.nextUserID,
		UID:     fmt.Sprintf("user-%d", s.nextUserID),
		Email:   email,
		Login:   login,
		Name:    name,
		OrgID:   orgID,
		Created: time.Now(),
	}
	s.users[user.ID] = user

	// 조직에 사용자 추가
	s.orgUsers = append(s.orgUsers, OrgUser{
		OrgID:  orgID,
		UserID: user.ID,
		Role:   role,
	})

	return user
}

// AddUserToOrg는 기존 사용자를 다른 조직에 추가한다.
func (s *MultiTenantService) AddUserToOrg(userID, orgID int64, role RoleType) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 이미 추가되어 있는지 확인
	for _, ou := range s.orgUsers {
		if ou.OrgID == orgID && ou.UserID == userID {
			return fmt.Errorf("user already added to organization")
		}
	}

	s.orgUsers = append(s.orgUsers, OrgUser{
		OrgID:  orgID,
		UserID: userID,
		Role:   role,
	})

	return nil
}

// GetUserOrgs는 사용자가 속한 모든 조직을 반환한다.
func (s *MultiTenantService) GetUserOrgs(userID int64) []OrgUser {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []OrgUser
	for _, ou := range s.orgUsers {
		if ou.UserID == userID {
			result = append(result, ou)
		}
	}
	return result
}

// SwitchUserOrg는 사용자의 현재 활성 조직을 전환한다.
func (s *MultiTenantService) SwitchUserOrg(userID, orgID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	user, ok := s.users[userID]
	if !ok {
		return fmt.Errorf("user not found")
	}

	// 해당 조직에 속해있는지 확인
	found := false
	for _, ou := range s.orgUsers {
		if ou.UserID == userID && ou.OrgID == orgID {
			found = true
			break
		}
	}

	if !found {
		return fmt.Errorf("user is not a member of org %d", orgID)
	}

	user.OrgID = orgID
	return nil
}

// --- 팀 관리 ---

func (s *MultiTenantService) CreateTeam(name string, orgID int64) *Team {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextTeamID++
	team := &Team{
		ID:    s.nextTeamID,
		UID:   fmt.Sprintf("team-%d", s.nextTeamID),
		OrgID: orgID,
		Name:  name,
	}
	s.teams[team.ID] = team
	return team
}

func (s *MultiTenantService) AddTeamMember(teamID, userID int64, perm PermissionType) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 이미 멤버인지 확인
	for _, tm := range s.teamMembers {
		if tm.TeamID == teamID && tm.UserID == userID {
			return fmt.Errorf("user already added to team")
		}
	}

	s.teamMembers = append(s.teamMembers, TeamMember{
		TeamID:     teamID,
		UserID:     userID,
		Permission: perm,
	})
	return nil
}

func (s *MultiTenantService) GetTeamMembers(teamID int64) []TeamMember {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []TeamMember
	for _, tm := range s.teamMembers {
		if tm.TeamID == teamID {
			result = append(result, tm)
		}
	}
	return result
}

func (s *MultiTenantService) GetTeamsByUser(orgID, userID int64) []*Team {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*Team
	for _, tm := range s.teamMembers {
		if tm.UserID == userID {
			if team, ok := s.teams[tm.TeamID]; ok && team.OrgID == orgID {
				result = append(result, team)
			}
		}
	}
	return result
}

// --- RBAC 권한 평가 ---

// GetUserRole은 특정 조직에서의 사용자 역할을 반환한다.
func (s *MultiTenantService) GetUserRole(userID, orgID int64) RoleType {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, ou := range s.orgUsers {
		if ou.UserID == userID && ou.OrgID == orgID {
			return ou.Role
		}
	}
	return RoleNone
}

// GetUserPermissions는 사용자의 모든 권한을 수집한다.
func (s *MultiTenantService) GetUserPermissions(userID, orgID int64) []Permission {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []Permission

	// 1. 기본 역할에서 권한 가져오기
	role := RoleNone
	for _, ou := range s.orgUsers {
		if ou.UserID == userID && ou.OrgID == orgID {
			role = ou.Role
			break
		}
	}

	if perms, ok := s.roleAssignments[string(role)]; ok {
		result = append(result, perms...)
	}

	// 2. 사용자별 추가 권한
	key := fmt.Sprintf("%d:%d", userID, orgID)
	if perms, ok := s.userPermissions[key]; ok {
		result = append(result, perms...)
	}

	return result
}

// Evaluate는 사용자가 특정 권한을 가지는지 평가한다.
func (s *MultiTenantService) Evaluate(userID, orgID int64, requiredAction, requiredScope string) bool {
	perms := s.GetUserPermissions(userID, orgID)

	for _, p := range perms {
		if p.Action != requiredAction {
			continue
		}

		// scope가 비어있으면 Action만으로 충분
		if p.Scope == "" || requiredScope == "" {
			return true
		}

		// 와일드카드 매칭
		if p.Scope == "*" {
			return true
		}

		// 접두사 와일드카드: "dashboards:*" 는 "dashboards:uid:abc" 를 포함
		if strings.HasSuffix(p.Scope, "*") {
			prefix := p.Scope[:len(p.Scope)-1]
			if strings.HasPrefix(requiredScope, prefix) {
				return true
			}
		}

		// 정확히 일치
		if p.Scope == requiredScope {
			return true
		}
	}

	return false
}

// --- Last Admin 보호 ---

// RemoveOrgUser는 조직에서 사용자를 제거한다.
// 마지막 Admin은 제거할 수 없다.
func (s *MultiTenantService) RemoveOrgUser(userID, orgID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 현재 사용자의 역할 확인
	var targetRole RoleType
	targetIdx := -1
	for i, ou := range s.orgUsers {
		if ou.UserID == userID && ou.OrgID == orgID {
			targetRole = ou.Role
			targetIdx = i
			break
		}
	}

	if targetIdx == -1 {
		return fmt.Errorf("user not found in org")
	}

	// 마지막 Admin 보호
	if targetRole == RoleAdmin {
		adminCount := 0
		for _, ou := range s.orgUsers {
			if ou.OrgID == orgID && ou.Role == RoleAdmin {
				adminCount++
			}
		}
		if adminCount <= 1 {
			return fmt.Errorf("cannot remove last organization admin")
		}
	}

	// 제거
	s.orgUsers = append(s.orgUsers[:targetIdx], s.orgUsers[targetIdx+1:]...)
	return nil
}

// --- 메인 실행 ---

func main() {
	fmt.Println("=== Grafana 사용자/팀/조직 관리 PoC ===")
	fmt.Println()

	svc := NewMultiTenantService()

	// -------------------------------------------------------
	// 1. 조직 생성
	// -------------------------------------------------------
	fmt.Println("--- [1] 조직 생성 ---")

	// 먼저 Grafana Admin 사용자 생성 (조직 생성에 필요)
	admin := svc.CreateUser("admin", "admin@grafana.io", "Grafana Admin", 0, RoleNone)
	admin.IsAdmin = true

	org1 := svc.CreateOrg("Engineering", admin.ID)
	org2 := svc.CreateOrg("Marketing", admin.ID)

	fmt.Printf("  조직 생성: %s (ID=%d)\n", org1.Name, org1.ID)
	fmt.Printf("  조직 생성: %s (ID=%d)\n", org2.Name, org2.ID)
	fmt.Println()

	// -------------------------------------------------------
	// 2. 사용자 생성 및 조직 배정
	// -------------------------------------------------------
	fmt.Println("--- [2] 사용자 생성 ---")

	alice := svc.CreateUser("alice", "alice@company.com", "Alice Kim", org1.ID, RoleAdmin)
	bob := svc.CreateUser("bob", "bob@company.com", "Bob Lee", org1.ID, RoleEditor)
	charlie := svc.CreateUser("charlie", "charlie@company.com", "Charlie Park", org1.ID, RoleViewer)
	diana := svc.CreateUser("diana", "diana@company.com", "Diana Choi", org2.ID, RoleAdmin)

	fmt.Printf("  Alice: ID=%d, Org=%d (Admin)\n", alice.ID, alice.OrgID)
	fmt.Printf("  Bob: ID=%d, Org=%d (Editor)\n", bob.ID, bob.OrgID)
	fmt.Printf("  Charlie: ID=%d, Org=%d (Viewer)\n", charlie.ID, charlie.OrgID)
	fmt.Printf("  Diana: ID=%d, Org=%d (Admin)\n", diana.ID, diana.OrgID)
	fmt.Println()

	// Bob을 Marketing 조직에도 추가 (Viewer로)
	svc.AddUserToOrg(bob.ID, org2.ID, RoleViewer)

	// -------------------------------------------------------
	// 3. 조직 전환
	// -------------------------------------------------------
	fmt.Println("--- [3] 조직 전환 ---")

	userOrgs := svc.GetUserOrgs(bob.ID)
	fmt.Printf("  Bob의 조직 목록: %d개\n", len(userOrgs))
	for _, uo := range userOrgs {
		orgName := svc.orgs[uo.OrgID].Name
		fmt.Printf("    - %s (Role: %s)\n", orgName, uo.Role)
	}

	err := svc.SwitchUserOrg(bob.ID, org2.ID)
	if err != nil {
		fmt.Printf("  조직 전환 실패: %v\n", err)
	} else {
		fmt.Printf("  Bob 조직 전환: Engineering -> Marketing\n")
		fmt.Printf("  Bob의 Marketing 역할: %s\n", svc.GetUserRole(bob.ID, org2.ID))
	}
	fmt.Println()

	// -------------------------------------------------------
	// 4. 팀 생성 및 멤버 관리
	// -------------------------------------------------------
	fmt.Println("--- [4] 팀 관리 ---")

	backendTeam := svc.CreateTeam("Backend Team", org1.ID)
	frontendTeam := svc.CreateTeam("Frontend Team", org1.ID)

	svc.AddTeamMember(backendTeam.ID, alice.ID, PermissionTypeAdmin)
	svc.AddTeamMember(backendTeam.ID, bob.ID, PermissionTypeMember)
	svc.AddTeamMember(frontendTeam.ID, alice.ID, PermissionTypeMember)
	svc.AddTeamMember(frontendTeam.ID, charlie.ID, PermissionTypeMember)

	fmt.Printf("  팀 생성: %s (Org: %s)\n", backendTeam.Name, org1.Name)
	fmt.Printf("  팀 생성: %s (Org: %s)\n", frontendTeam.Name, org1.Name)

	members := svc.GetTeamMembers(backendTeam.ID)
	fmt.Printf("  %s 멤버:\n", backendTeam.Name)
	for _, m := range members {
		user := svc.users[m.UserID]
		permStr := "Member"
		if m.Permission == PermissionTypeAdmin {
			permStr = "Admin"
		}
		fmt.Printf("    - %s (%s)\n", user.Name, permStr)
	}

	aliceTeams := svc.GetTeamsByUser(org1.ID, alice.ID)
	fmt.Printf("  Alice의 팀: %d개\n", len(aliceTeams))
	for _, t := range aliceTeams {
		fmt.Printf("    - %s\n", t.Name)
	}
	fmt.Println()

	// -------------------------------------------------------
	// 5. RBAC 권한 평가
	// -------------------------------------------------------
	fmt.Println("--- [5] RBAC 권한 평가 ---")

	testCases := []struct {
		name     string
		userID   int64
		orgID    int64
		action   string
		scope    string
	}{
		{"Alice(Admin) 대시보드 읽기", alice.ID, org1.ID, "dashboards:read", "dashboards:uid:abc"},
		{"Alice(Admin) 대시보드 삭제", alice.ID, org1.ID, "dashboards:delete", "dashboards:uid:abc"},
		{"Bob(Editor) 대시보드 쓰기", bob.ID, org1.ID, "dashboards:write", "dashboards:uid:abc"},
		{"Bob(Editor) 대시보드 삭제", bob.ID, org1.ID, "dashboards:delete", "dashboards:uid:abc"},
		{"Charlie(Viewer) 대시보드 읽기", charlie.ID, org1.ID, "dashboards:read", "dashboards:uid:abc"},
		{"Charlie(Viewer) 대시보드 쓰기", charlie.ID, org1.ID, "dashboards:write", "dashboards:uid:abc"},
		{"Diana(Admin) Org2 팀 관리", diana.ID, org2.ID, "teams:write", "teams:id:1"},
		{"Diana Org1 접근 시도", diana.ID, org1.ID, "dashboards:read", "dashboards:uid:abc"},
	}

	for _, tc := range testCases {
		result := svc.Evaluate(tc.userID, tc.orgID, tc.action, tc.scope)
		status := "허용"
		if !result {
			status = "거부"
		}
		fmt.Printf("  %-35s → %s\n", tc.name, status)
	}
	fmt.Println()

	// -------------------------------------------------------
	// 6. 조직 격리 확인
	// -------------------------------------------------------
	fmt.Println("--- [6] 조직 격리 ---")

	// Diana(Org2 Admin)가 Org1의 리소스에 접근 시도
	canAccess := svc.Evaluate(diana.ID, org1.ID, "dashboards:read", "dashboards:uid:abc")
	fmt.Printf("  Diana(Org2 Admin)이 Org1 대시보드 접근: %v (격리됨)\n", canAccess)

	// Alice(Org1 Admin)가 Org2의 리소스에 접근 시도
	canAccess = svc.Evaluate(alice.ID, org2.ID, "teams:write", "teams:id:1")
	fmt.Printf("  Alice(Org1 Admin)가 Org2 팀 관리: %v (격리됨)\n", canAccess)
	fmt.Println()

	// -------------------------------------------------------
	// 7. Last Admin 보호
	// -------------------------------------------------------
	fmt.Println("--- [7] Last Admin 보호 ---")

	// 테스트를 위한 새 조직 생성 (admin만 Admin으로 존재)
	org3 := svc.CreateOrg("TestOrg", admin.ID)
	frank := svc.CreateUser("frank", "frank@company.com", "Frank Yoo", org3.ID, RoleEditor)
	_ = frank

	// admin 사용자는 org3의 유일한 Admin이므로 제거 불가
	err = svc.RemoveOrgUser(admin.ID, org3.ID)
	if err != nil {
		fmt.Printf("  마지막 Admin 제거 시도: %v\n", err)
	}

	// Admin을 추가한 후에는 기존 Admin을 제거할 수 있다
	eve := svc.CreateUser("eve", "eve@company.com", "Eve Jung", org3.ID, RoleAdmin)
	err = svc.RemoveOrgUser(admin.ID, org3.ID)
	if err != nil {
		fmt.Printf("  Admin 2명 상태에서 admin 제거: %v\n", err)
	} else {
		fmt.Printf("  Admin 2명 상태에서 admin 제거: 성공 (Eve가 남은 Admin)\n")
	}
	_ = eve
	fmt.Println()

	// -------------------------------------------------------
	// 8. 권한 요약 출력
	// -------------------------------------------------------
	fmt.Println("--- [8] 권한 요약 ---")

	fmt.Println("  역할별 권한 수:")
	for _, role := range []string{"Viewer", "Editor", "Admin"} {
		perms := svc.roleAssignments[role]
		fmt.Printf("    %s: %d개 권한\n", role, len(perms))
	}

	fmt.Println()
	fmt.Println("  권한 계층:")
	fmt.Println("    Grafana Admin (전체 인스턴스 관리)")
	fmt.Println("    └── Org Admin (조직 관리, 데이터소스, 팀)")
	fmt.Println("         └── Org Editor (대시보드 편집, 쿼리)")
	fmt.Println("              └── Org Viewer (대시보드 조회)")
	fmt.Println("                   └── None (접근 불가)")
	fmt.Println()

	fmt.Println("=== 사용자/팀/조직 관리 PoC 완료 ===")
}
