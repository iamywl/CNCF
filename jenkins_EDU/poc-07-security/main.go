// poc-07-security: Jenkins 보안 서브시스템 (SecurityRealm, AuthorizationStrategy, ACL, Permission, CrumbIssuer) 시뮬레이션
//
// Jenkins 보안 모델의 4대 축을 Go 표준 라이브러리만으로 재현한다:
//   1. SecurityRealm   -- 인증(Authentication): 사용자 신원 확인
//   2. AuthorizationStrategy -- 인가(Authorization): 권한 부여 전략
//   3. ACL + Permission -- 접근 제어: 세밀한 권한 검사
//   4. CrumbIssuer     -- CSRF 방어: 요청 위조 방지 토큰
//
// 실제 Jenkins 소스 참조:
//   - core/src/main/java/hudson/security/SecurityRealm.java
//     - abstract createSecurityComponents() → SecurityComponents(AuthenticationManager, UserDetailsService)
//     - createFilter(FilterConfig) → 표준 Spring Security 필터 체인 조립
//     - SecurityComponents: manager2(AuthenticationManager), userDetails2(UserDetailsService), rememberMe2
//     - 내부 None 클래스: SecurityRealm.NO_AUTHENTICATION (인증 없음 싱글턴)
//   - core/src/main/java/hudson/security/HudsonPrivateSecurityRealm.java
//     - Jenkins 내장 사용자 DB, BCrypt/PBKDF2 기반 패스워드 해싱
//     - AbstractPasswordBasedSecurityRealm 상속
//   - core/src/main/java/hudson/security/AuthorizationStrategy.java
//     - abstract getRootACL() → 최상위 ACL
//     - getACL(Job), getACL(View), getACL(AbstractItem), getACL(User) 등 리소스별 ACL
//     - UNSECURED: 모든 권한 허가 (Unsecured 내부 클래스)
//   - core/src/main/java/hudson/security/FullControlOnceLoggedInAuthorizationStrategy.java
//     - 로그인 사용자에게 모든 권한, 익명 사용자에게는 READ만 (또는 거부)
//     - AUTHENTICATED_READ / ANONYMOUS_READ 두 가지 SparseACL 사용
//   - core/src/main/java/hudson/security/ACL.java
//     - hasPermission2(Authentication, Permission) → boolean (핵심 검사 메서드)
//     - checkPermission(Permission): hasPermission2 실패 시 AccessDeniedException3 던짐
//     - SYSTEM2: UsernamePasswordAuthenticationToken("SYSTEM", "SYSTEM") -- 모든 권한 자동 허가
//     - impersonate2(Authentication): 다른 사용자로 전환 (SecurityContext 교체)
//     - as2(Authentication): try-with-resources용 impersonation (ACLContext 반환)
//     - lambda2(BiFunction): 람다로 간단한 ACL 생성
//   - core/src/main/java/hudson/security/Permission.java
//     - group(PermissionGroup), name(String), impliedBy(Permission) -- 권한 트리 구조
//     - HUDSON_ADMINISTER: God-like 권한 (모든 권한의 최상위)
//     - Permission.READ, WRITE, CREATE, UPDATE, DELETE, CONFIGURE -- 제네릭 루트 권한
//   - core/src/main/java/hudson/security/PermissionGroup.java
//     - owner(Class), permissions(SortedSet<Permission>) -- 권한 그룹
//   - core/src/main/java/hudson/model/Item.java
//     - Item.CREATE, DELETE, CONFIGURE, READ, BUILD, WORKSPACE, CANCEL -- Job 레벨 권한
//     - 각각 Permission.CREATE, DELETE, CONFIGURE, READ, UPDATE 등을 impliedBy로 참조
//   - core/src/main/java/hudson/security/csrf/CrumbIssuer.java
//     - getCrumb(ServletRequest) → issueCrumb(request, salt) 호출
//     - validateCrumb(request, salt, crumb) → 검증
//   - core/src/main/java/hudson/security/csrf/DefaultCrumbIssuer.java
//     - issueCrumb(): Authentication.getName() + ";" + session.getId() → SHA-256 + salt → hex
//     - validateCrumb(): MessageDigest.isEqual()로 상수 시간 비교
//
// 실행: go run main.go

package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// 1. Permission 계층: 권한 트리 구조
// =============================================================================
// Jenkins의 hudson.security.Permission은 그룹(PermissionGroup), 이름(name),
// 상위 권한(impliedBy) 필드로 트리 구조를 형성한다.
//
// 실제 코드 (Permission.java):
//   public final PermissionGroup group;
//   public final String name;
//   public final Permission impliedBy;
//
// impliedBy 체인을 통해 상위 권한이 하위 권한을 "내포(imply)"한다.
// 예: Hudson.ADMINISTER → Permission.WRITE → Permission.UPDATE → Item.BUILD

// PermissionGroup 은 같은 owner를 공유하는 Permission들의 그룹이다.
// Jenkins의 hudson.security.PermissionGroup에 대응한다.
//
// 실제 코드 (PermissionGroup.java):
//   public final Class owner;
//   public final Localizable title;
//   private final SortedSet<Permission> permissions = new TreeSet<>()
type PermissionGroup struct {
	Owner       string        // Jenkins에서는 Class 타입 (예: "hudson.model.Hudson")
	Title       string        // 사람이 읽을 수 있는 그룹 제목
	Permissions []*Permission // 이 그룹에 속한 권한 목록
}

// Permission 은 보안 권한을 나타내는 불변 객체이다.
// Jenkins의 hudson.security.Permission에 대응한다.
//
// 실제 코드 (Permission.java):
//   public Permission(PermissionGroup group, String name,
//       Localizable description, Permission impliedBy, boolean enable,
//       PermissionScope[] scopes)
//   - group에 자동 등록: group.add(this)
//   - 전역 목록에 등록: ALL.add(this)
//   - ID는 "owner.name" 형식: this.id = owner.getName() + '.' + name
type Permission struct {
	Group     *PermissionGroup // 소속 그룹
	Name      string           // 권한 이름 (예: "Administer", "Build")
	ImpliedBy *Permission      // 이 권한을 내포하는 상위 권한 (nil이면 최상위)
	Enabled   bool             // 활성 여부
}

// ID 는 Permission의 고유 식별자를 반환한다.
// Jenkins에서는 "owner.name" 형식이다. 예: "hudson.model.Hudson.Administer"
func (p *Permission) ID() string {
	return p.Group.Owner + "." + p.Name
}

// Implies 는 이 권한이 target 권한을 내포하는지 확인한다.
// impliedBy 체인을 거슬러 올라가면서 확인한다.
//
// Jenkins ACL.checkPermission() 내부 로직:
//   while (!p.enabled && p.impliedBy != null) { p = p.impliedBy; }
func (p *Permission) Implies(target *Permission) bool {
	if p == target {
		return true
	}
	// target의 impliedBy 체인을 올라가면서 p와 일치하는지 확인
	current := target
	for current != nil {
		if current == p {
			return true
		}
		current = current.ImpliedBy
	}
	return false
}

// allPermissions 는 전역 Permission 레지스트리이다.
// Jenkins의 Permission.ALL (CopyOnWriteArrayList)에 대응한다.
var allPermissions []*Permission

// NewPermission 은 새로운 Permission을 생성하고 그룹에 등록한다.
func NewPermission(group *PermissionGroup, name string, impliedBy *Permission) *Permission {
	p := &Permission{
		Group:     group,
		Name:      name,
		ImpliedBy: impliedBy,
		Enabled:   true,
	}
	group.Permissions = append(group.Permissions, p)
	allPermissions = append(allPermissions, p)
	return p
}

// -- 전역 PermissionGroup 및 Permission 정의 --
// Jenkins의 Permission 계층 구조를 재현한다.
//
// 실제 Jenkins 권한 트리:
//   Hudson.ADMINISTER (God-like, 모든 권한의 최상위)
//     ├── Permission.FULL_CONTROL (deprecated, ADMINISTER에 매핑)
//     ├── Permission.READ (GenericRead)
//     │     ├── Item.READ
//     │     ├── Item.DISCOVER (impliedBy: Item.READ)
//     │     └── Item.WORKSPACE
//     ├── Permission.WRITE (GenericWrite)
//     │     ├── Permission.CREATE (GenericCreate)
//     │     │     └── Item.CREATE
//     │     ├── Permission.UPDATE (GenericUpdate)
//     │     │     ├── Permission.CONFIGURE (GenericConfigure)
//     │     │     │     └── Item.CONFIGURE
//     │     │     ├── Item.BUILD
//     │     │     └── Item.CANCEL
//     │     └── Permission.DELETE (GenericDelete)
//     │           └── Item.DELETE
//     └── ...

var (
	// Hudson PermissionGroup -- Jenkins의 Permission.HUDSON_PERMISSIONS
	HudsonGroup = &PermissionGroup{Owner: "hudson.model.Hudson", Title: "Overall"}

	// Hudson.ADMINISTER -- 모든 권한의 최상위
	// 실제: Permission.java에서 HUDSON_ADMINISTER = new Permission(HUDSON_PERMISSIONS, "Administer", null)
	HudsonAdminister = NewPermission(HudsonGroup, "Administer", nil)

	// 제네릭 루트 권한 -- Permission.java의 GROUP
	GenericGroup = &PermissionGroup{Owner: "hudson.security.Permission", Title: "Generic"}

	// Permission.READ, WRITE, CREATE, UPDATE, DELETE, CONFIGURE
	// 모두 HUDSON_ADMINISTER를 impliedBy로 참조
	GenericRead      = NewPermission(GenericGroup, "GenericRead", HudsonAdminister)
	GenericWrite     = NewPermission(GenericGroup, "GenericWrite", HudsonAdminister)
	GenericCreate    = NewPermission(GenericGroup, "GenericCreate", GenericWrite)
	GenericUpdate    = NewPermission(GenericGroup, "GenericUpdate", GenericWrite)
	GenericDelete    = NewPermission(GenericGroup, "GenericDelete", GenericWrite)
	GenericConfigure = NewPermission(GenericGroup, "GenericConfigure", GenericUpdate)

	// Item(Job) PermissionGroup -- Item.java의 PERMISSIONS
	ItemGroup = &PermissionGroup{Owner: "hudson.model.Item", Title: "Job"}

	// Item 레벨 권한 -- 각각 제네릭 루트 권한을 impliedBy로 참조
	// 실제 (Item.java):
	//   Item.CREATE = new Permission(PERMISSIONS, "Create", Permission.CREATE, PermissionScope.ITEM_GROUP)
	//   Item.READ = new Permission(PERMISSIONS, "Read", Permission.READ, PermissionScope.ITEM)
	//   Item.BUILD = new Permission(PERMISSIONS, "Build", Permission.UPDATE, PermissionScope.ITEM)
	ItemCreate    = NewPermission(ItemGroup, "Create", GenericCreate)
	ItemDelete    = NewPermission(ItemGroup, "Delete", GenericDelete)
	ItemConfigure = NewPermission(ItemGroup, "Configure", GenericConfigure)
	ItemRead      = NewPermission(ItemGroup, "Read", GenericRead)
	ItemBuild     = NewPermission(ItemGroup, "Build", GenericUpdate)
	ItemCancel    = NewPermission(ItemGroup, "Cancel", GenericUpdate)
	ItemWorkspace = NewPermission(ItemGroup, "Workspace", GenericRead)
)

// =============================================================================
// 2. Authentication (인증 결과)
// =============================================================================
// Jenkins는 Spring Security의 Authentication 인터페이스를 사용한다.
// 핵심 메서드: getName(), getAuthorities(), isAuthenticated()
//
// ACL.java에서의 특수 인증:
//   SYSTEM2 = new UsernamePasswordAuthenticationToken("SYSTEM", "SYSTEM")
//   → 모든 권한 자동 허가

// Authentication 은 인증된 사용자 정보를 나타낸다.
// Spring Security의 org.springframework.security.core.Authentication에 대응한다.
type Authentication struct {
	Name          string   // 사용자 이름
	Authorities   []string // 부여된 역할 (GrantedAuthority)
	Authenticated bool     // 인증 완료 여부
}

// 특수 인증 상수
var (
	// SYSTEM2 는 Jenkins 자체의 인증이다. 모든 권한이 자동 허가된다.
	// 실제: ACL.java - SYSTEM2 = new UsernamePasswordAuthenticationToken("SYSTEM", "SYSTEM")
	SYSTEM = &Authentication{Name: "SYSTEM", Authorities: []string{"SYSTEM"}, Authenticated: true}

	// ANONYMOUS 는 익명 사용자의 인증이다.
	// 실제: ACL.java - ANONYMOUS_USERNAME = "anonymous"
	ANONYMOUS = &Authentication{Name: "anonymous", Authorities: []string{"anonymous"}, Authenticated: false}
)

// =============================================================================
// 3. ACL (접근 제어 리스트)
// =============================================================================
// Jenkins의 hudson.security.ACL은 권한 검사의 핵심 추상 클래스이다.
//
// 핵심 메서드 (ACL.java):
//   hasPermission2(Authentication a, Permission p) → boolean
//   checkPermission(Permission p):
//     Authentication a = Jenkins.getAuthentication2();
//     if (a.equals(SYSTEM2)) return;  // SYSTEM은 항상 허가
//     if (!hasPermission2(a, p)) throw AccessDeniedException3
//   hasPermission(Permission p):
//     Authentication a = Jenkins.getAuthentication2();
//     if (a.equals(SYSTEM2)) return true;
//     return hasPermission2(a, p);

// ACL 은 접근 제어 리스트 인터페이스이다.
// Jenkins의 hudson.security.ACL에 대응한다.
type ACL interface {
	// HasPermission 은 주어진 인증에 대해 권한이 있는지 확인한다.
	// Jenkins의 hasPermission2(Authentication, Permission)에 대응한다.
	HasPermission(auth *Authentication, perm *Permission) bool
}

// CheckPermission 은 권한 검사를 수행하고, 실패 시 에러를 반환한다.
// Jenkins의 ACL.checkPermission(Permission)에 대응한다.
//
// 실제 코드 (ACL.java):
//   public final void checkPermission(Permission p) {
//       Authentication a = Jenkins.getAuthentication2();
//       if (a.equals(SYSTEM2)) return;  // SYSTEM은 항상 통과
//       if (!hasPermission2(a, p)) {
//           while (!p.enabled && p.impliedBy != null) { p = p.impliedBy; }
//           throw new AccessDeniedException3(a, p);
//       }
//   }
func CheckPermission(acl ACL, auth *Authentication, perm *Permission) error {
	// SYSTEM 인증은 항상 허가 -- Jenkins의 핵심 패턴
	if auth == SYSTEM {
		return nil
	}
	if !acl.HasPermission(auth, perm) {
		return fmt.Errorf("[ACCESS DENIED] 사용자 '%s'에게 '%s' 권한이 없습니다", auth.Name, perm.ID())
	}
	return nil
}

// LambdaACL 은 함수로 정의된 ACL이다.
// Jenkins의 ACL.lambda2(BiFunction)에 대응한다.
//
// 실제 코드 (ACL.java):
//   public static ACL lambda2(final BiFunction<Authentication, Permission, Boolean> impl) {
//       return new ACL() { ... };
//   }
type LambdaACL struct {
	Fn func(auth *Authentication, perm *Permission) bool
}

func (a *LambdaACL) HasPermission(auth *Authentication, perm *Permission) bool {
	return a.Fn(auth, perm)
}

// =============================================================================
// 4. MatrixACL -- 프로젝트 매트릭스 권한 ACL
// =============================================================================
// Jenkins의 ProjectMatrixAuthorizationStrategy에서 사용하는 매트릭스 기반 ACL.
// 사용자/그룹별로 Permission을 개별 부여한다.
//
// GlobalMatrixAuthorizationStrategy 플러그인에서 구현:
//   - 사용자별 Permission Set 관리
//   - impliedBy 체인을 통한 상위 권한 확인

// MatrixACL 은 사용자별 권한 매트릭스를 관리하는 ACL이다.
type MatrixACL struct {
	mu          sync.RWMutex
	Permissions map[string]map[string]bool // 사용자 이름 → Permission ID → 부여 여부
}

// NewMatrixACL 은 새로운 MatrixACL을 생성한다.
func NewMatrixACL() *MatrixACL {
	return &MatrixACL{
		Permissions: make(map[string]map[string]bool),
	}
}

// Grant 는 사용자에게 특정 권한을 부여한다.
func (m *MatrixACL) Grant(username string, perm *Permission) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Permissions[username] == nil {
		m.Permissions[username] = make(map[string]bool)
	}
	m.Permissions[username][perm.ID()] = true
}

// Revoke 는 사용자에게서 특정 권한을 제거한다.
func (m *MatrixACL) Revoke(username string, perm *Permission) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Permissions[username] != nil {
		delete(m.Permissions[username], perm.ID())
	}
}

// HasPermission 은 매트릭스에서 권한을 확인한다.
// 직접 부여된 권한뿐 아니라, impliedBy 체인을 따라 상위 권한도 확인한다.
//
// 예: 사용자에게 HudsonAdminister가 있으면 ItemBuild도 허가
// (ItemBuild.impliedBy → GenericUpdate.impliedBy → GenericWrite.impliedBy → HudsonAdminister)
func (m *MatrixACL) HasPermission(auth *Authentication, perm *Permission) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	userPerms := m.Permissions[auth.Name]
	if userPerms == nil {
		return false
	}

	// 직접 부여된 권한 확인
	if userPerms[perm.ID()] {
		return true
	}

	// impliedBy 체인을 따라 상위 권한 확인
	// 사용자에게 부여된 각 권한이 target 권한을 내포하는지 검사
	for permID := range userPerms {
		for _, registered := range allPermissions {
			if registered.ID() == permID {
				if registered.Implies(perm) {
					return true
				}
				break
			}
		}
	}
	return false
}

// =============================================================================
// 5. AuthorizationStrategy (인가 전략)
// =============================================================================
// Jenkins의 hudson.security.AuthorizationStrategy에 대응한다.
//
// 실제 코드 (AuthorizationStrategy.java):
//   public abstract ACL getRootACL();
//   public ACL getACL(Job<?,?> project) { return getRootACL(); }
//   public ACL getACL(View item) { ... }  // 뷰별 ACL
//   public abstract Collection<String> getGroups();
//   public static final AuthorizationStrategy UNSECURED = new Unsecured();

// AuthorizationStrategy 는 인가 전략 인터페이스이다.
type AuthorizationStrategy interface {
	// GetRootACL 은 최상위 ACL을 반환한다.
	// Jenkins의 getRootACL()에 대응한다.
	GetRootACL() ACL

	// GetACL 은 리소스별 ACL을 반환한다.
	// Jenkins의 getACL(Job), getACL(AbstractItem) 등에 대응한다.
	// 기본 구현은 getRootACL()을 반환한다.
	GetACL(resource string) ACL

	// GetGroups 는 이 전략에서 사용하는 그룹/역할 이름을 반환한다.
	GetGroups() []string
}

// FullControlOnceLoggedIn 은 로그인한 사용자에게 모든 권한을 부여하는 전략이다.
// Jenkins의 FullControlOnceLoggedInAuthorizationStrategy에 대응한다.
//
// 실제 코드 (FullControlOnceLoggedInAuthorizationStrategy.java):
//   private boolean denyAnonymousReadAccess = false;
//   public ACL getRootACL() {
//       return denyAnonymousReadAccess ? AUTHENTICATED_READ : ANONYMOUS_READ;
//   }
//   static {
//       ANONYMOUS_READ.add(ACL.EVERYONE, Jenkins.ADMINISTER, true);
//       ANONYMOUS_READ.add(ACL.ANONYMOUS, Jenkins.ADMINISTER, false);
//       ANONYMOUS_READ.add(ACL.ANONYMOUS, Permission.READ, true);
//       AUTHENTICATED_READ.add(ACL.EVERYONE, Jenkins.ADMINISTER, true);
//       AUTHENTICATED_READ.add(ACL.ANONYMOUS, Jenkins.ADMINISTER, false);
//   }
type FullControlOnceLoggedIn struct {
	DenyAnonymousReadAccess bool
}

func (f *FullControlOnceLoggedIn) GetRootACL() ACL {
	return &LambdaACL{
		Fn: func(auth *Authentication, perm *Permission) bool {
			// 인증된 사용자 → 모든 권한 허가
			if auth.Authenticated {
				return true
			}
			// 익명 사용자
			if f.DenyAnonymousReadAccess {
				return false // 익명 읽기 접근 거부
			}
			// 익명 읽기 허용: READ 계열만 허가
			return perm == GenericRead || perm == ItemRead
		},
	}
}

func (f *FullControlOnceLoggedIn) GetACL(resource string) ACL {
	return f.GetRootACL()
}

func (f *FullControlOnceLoggedIn) GetGroups() []string {
	return nil
}

// MatrixAuthorizationStrategy 는 프로젝트별 매트릭스 권한 전략이다.
// Jenkins의 GlobalMatrixAuthorizationStrategy / ProjectMatrixAuthorizationStrategy에 대응한다.
type MatrixAuthorizationStrategy struct {
	GlobalACL    *MatrixACL
	ProjectACLs  map[string]*MatrixACL // 프로젝트 이름 → 프로젝트별 ACL
}

func NewMatrixAuthorizationStrategy() *MatrixAuthorizationStrategy {
	return &MatrixAuthorizationStrategy{
		GlobalACL:   NewMatrixACL(),
		ProjectACLs: make(map[string]*MatrixACL),
	}
}

func (m *MatrixAuthorizationStrategy) GetRootACL() ACL {
	return m.GlobalACL
}

// GetACL 은 리소스별 ACL을 반환한다.
// 프로젝트별 ACL이 있으면 글로벌 ACL과 조합한 CompositeACL을 반환한다.
// Jenkins에서도 프로젝트별 ACL이 글로벌 ACL 위에 레이어링된다.
func (m *MatrixAuthorizationStrategy) GetACL(resource string) ACL {
	if projectACL, ok := m.ProjectACLs[resource]; ok {
		// 프로젝트별 ACL + 글로벌 ACL 조합
		return &CompositeACL{Primary: projectACL, Fallback: m.GlobalACL}
	}
	return m.GlobalACL // 프로젝트별 ACL이 없으면 글로벌 ACL 사용
}

// CompositeACL 은 두 개의 ACL을 조합한다.
// Primary에서 먼저 확인하고, 없으면 Fallback에서 확인한다.
// Jenkins의 프로젝트별 ACL이 글로벌 ACL 위에 레이어링되는 패턴을 재현한다.
type CompositeACL struct {
	Primary  ACL
	Fallback ACL
}

func (c *CompositeACL) HasPermission(auth *Authentication, perm *Permission) bool {
	if c.Primary.HasPermission(auth, perm) {
		return true
	}
	return c.Fallback.HasPermission(auth, perm)
}

func (m *MatrixAuthorizationStrategy) GetGroups() []string {
	return []string{"admin", "developer", "viewer"}
}

// =============================================================================
// 6. SecurityRealm (인증 영역)
// =============================================================================
// Jenkins의 hudson.security.SecurityRealm에 대응한다.
//
// 실제 코드 (SecurityRealm.java):
//   public abstract SecurityComponents createSecurityComponents();
//   public synchronized SecurityComponents getSecurityComponents() {
//       if (this.securityComponents == null)
//           this.securityComponents = this.createSecurityComponents();
//       return this.securityComponents;
//   }

// UserDetails 는 사용자 상세 정보를 나타낸다.
// Spring Security의 UserDetails에 대응한다.
type UserDetails struct {
	Username     string
	PasswordHash string // 실제 Jenkins: BCrypt 또는 PBKDF2 해시
	Authorities  []string
}

// SecurityRealm 은 인증 백엔드 인터페이스이다.
// Jenkins의 hudson.security.SecurityRealm에 대응한다.
type SecurityRealm interface {
	// Authenticate 는 사용자 이름과 비밀번호로 인증을 수행한다.
	// Jenkins에서는 AuthenticationManager.authenticate(UsernamePasswordAuthenticationToken)
	Authenticate(username, password string) (*Authentication, error)

	// LoadUserByUsername 은 사용자 상세 정보를 로드한다.
	// Jenkins의 SecurityRealm.loadUserByUsername2(String)에 대응한다.
	LoadUserByUsername(username string) (*UserDetails, error)

	// AllowsSignup 은 사용자 등록을 지원하는지 반환한다.
	AllowsSignup() bool
}

// HudsonPrivateSecurityRealm 은 Jenkins 내장 사용자 DB 기반 인증이다.
// 실제: core/src/main/java/hudson/security/HudsonPrivateSecurityRealm.java
//
// 특징:
//   - User 객체에 패스워드 해시를 저장
//   - BCryptPasswordEncoder 또는 PBKDF2 사용 (FIPS 모드에 따라)
//   - createSecurityComponents()에서 AuthenticationManager와 UserDetailsService 생성
type HudsonPrivateSecurityRealm struct {
	mu    sync.RWMutex
	Users map[string]*UserDetails // username → UserDetails
}

func NewHudsonPrivateSecurityRealm() *HudsonPrivateSecurityRealm {
	return &HudsonPrivateSecurityRealm{
		Users: make(map[string]*UserDetails),
	}
}

// CreateUser 는 새 사용자를 등록한다.
// 실제 Jenkins에서는 HudsonPrivateSecurityRealm.doCreateAccount()를 통해 처리된다.
// 패스워드는 SHA-256 해시로 저장 (실제 Jenkins는 BCrypt 사용).
func (r *HudsonPrivateSecurityRealm) CreateUser(username, password string, authorities []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.Users[username]; exists {
		return fmt.Errorf("사용자 '%s'가 이미 존재합니다", username)
	}

	hash := sha256.Sum256([]byte(password))
	r.Users[username] = &UserDetails{
		Username:     username,
		PasswordHash: hex.EncodeToString(hash[:]),
		Authorities:  authorities,
	}
	return nil
}

// Authenticate 는 사용자 이름과 비밀번호로 인증을 수행한다.
//
// 실제 Jenkins 인증 흐름:
//   요청 → Filter Chain → AuthenticationProcessingFilter2
//   → AuthenticationManager.authenticate(UsernamePasswordAuthenticationToken)
//   → HudsonPrivateSecurityRealm 내부의 doAuthenticate()
//   → User.getProperty(Details.class).isPasswordCorrect(password)
//   → SecurityContext에 Authentication 설정
func (r *HudsonPrivateSecurityRealm) Authenticate(username, password string) (*Authentication, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	user, exists := r.Users[username]
	if !exists {
		return nil, fmt.Errorf("사용자 '%s'를 찾을 수 없습니다 (UsernameNotFoundException)", username)
	}

	// 패스워드 검증 (실제 Jenkins: BCryptPasswordEncoder.matches())
	hash := sha256.Sum256([]byte(password))
	if hex.EncodeToString(hash[:]) != user.PasswordHash {
		return nil, fmt.Errorf("잘못된 비밀번호입니다 (BadCredentialsException)")
	}

	return &Authentication{
		Name:          username,
		Authorities:   user.Authorities,
		Authenticated: true,
	}, nil
}

func (r *HudsonPrivateSecurityRealm) LoadUserByUsername(username string) (*UserDetails, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	user, exists := r.Users[username]
	if !exists {
		return nil, fmt.Errorf("사용자 '%s'를 찾을 수 없습니다", username)
	}
	return user, nil
}

func (r *HudsonPrivateSecurityRealm) AllowsSignup() bool {
	return true // Jenkins 내장 보안 영역은 가입을 지원한다
}

// =============================================================================
// 7. SecurityContext -- 현재 스레드의 인증 정보
// =============================================================================
// Jenkins는 Spring Security의 SecurityContextHolder를 사용하여
// 현재 스레드(요청)의 인증 정보를 관리한다.
//
// 실제 코드 (ACL.java):
//   public static SecurityContext impersonate2(Authentication auth) {
//       SecurityContext old = SecurityContextHolder.getContext();
//       SecurityContextHolder.setContext(new NonSerializableSecurityContext(auth));
//       return old;
//   }
//
//   public static ACLContext as2(Authentication auth) {
//       final ACLContext context = new ACLContext(SecurityContextHolder.getContext());
//       SecurityContextHolder.setContext(new NonSerializableSecurityContext(auth));
//       return context;
//   }

// SecurityContext 는 현재 사용자의 인증 정보를 저장하는 컨텍스트이다.
// Spring Security의 SecurityContextHolder (ThreadLocal 패턴)에 대응한다.
type SecurityContext struct {
	mu   sync.RWMutex
	auth *Authentication
}

var globalSecurityContext = &SecurityContext{auth: ANONYMOUS}

// SetAuthentication 은 현재 컨텍스트의 인증 정보를 설정한다.
func (ctx *SecurityContext) SetAuthentication(auth *Authentication) {
	ctx.mu.Lock()
	defer ctx.mu.Unlock()
	ctx.auth = auth
}

// GetAuthentication 은 현재 컨텍스트의 인증 정보를 반환한다.
func (ctx *SecurityContext) GetAuthentication() *Authentication {
	ctx.mu.RLock()
	defer ctx.mu.RUnlock()
	return ctx.auth
}

// Impersonate 는 다른 사용자로 전환하고 이전 인증을 반환한다.
// Jenkins의 ACL.impersonate2(Authentication)에 대응한다.
func (ctx *SecurityContext) Impersonate(auth *Authentication) *Authentication {
	ctx.mu.Lock()
	defer ctx.mu.Unlock()
	old := ctx.auth
	ctx.auth = auth
	return old
}

// =============================================================================
// 8. CrumbIssuer -- CSRF 방어
// =============================================================================
// Jenkins의 hudson.security.csrf.CrumbIssuer에 대응한다.
//
// CSRF(Cross-Site Request Forgery) 공격을 방어하기 위해 요청에 nonce 토큰(crumb)을
// 포함시킨다. Jenkins의 모든 상태 변경 요청(POST)에는 crumb이 필요하다.
//
// 실제 코드 (CrumbIssuer.java):
//   public static final String DEFAULT_CRUMB_NAME = "Jenkins-Crumb";
//   public String getCrumb(ServletRequest request) {
//       crumb = issueCrumb(request, getDescriptor().getCrumbSalt());
//       request.setAttribute(CRUMB_ATTRIBUTE, crumb);
//       return crumb;
//   }
//   protected abstract String issueCrumb(ServletRequest request, String salt);
//   public boolean validateCrumb(ServletRequest request, String salt, String crumb);

// CrumbIssuer 는 CSRF 토큰 발급/검증 인터페이스이다.
type CrumbIssuer interface {
	// IssueCrumb 는 CSRF 토큰을 발급한다.
	IssueCrumb(username, sessionID string) string

	// ValidateCrumb 는 CSRF 토큰을 검증한다.
	ValidateCrumb(username, sessionID, crumb string) bool

	// GetCrumbRequestField 는 크럼 필드 이름을 반환한다.
	GetCrumbRequestField() string
}

// DefaultCrumbIssuer 는 세션 ID와 사용자명 기반의 기본 CSRF 토큰 발급기이다.
// Jenkins의 hudson.security.csrf.DefaultCrumbIssuer에 대응한다.
//
// 실제 코드 (DefaultCrumbIssuer.java):
//   protected synchronized String issueCrumb(ServletRequest request, String salt) {
//       StringBuilder buffer = new StringBuilder();
//       Authentication a = Jenkins.getAuthentication2();
//       buffer.append(a.getName());
//       if (!EXCLUDE_SESSION_ID) {
//           buffer.append(';');
//           buffer.append(req.getSession().getId());
//       }
//       md.update(buffer.toString().getBytes(UTF_8));
//       return Util.toHexString(md.digest(salt.getBytes(US_ASCII)));
//   }
//
//   public boolean validateCrumb(ServletRequest request, String salt, String crumb) {
//       String newCrumb = issueCrumb(request, salt);
//       return MessageDigest.isEqual(newCrumb.getBytes(), crumb.getBytes());
//   }
type DefaultCrumbIssuer struct {
	Salt string // 비밀 솔트 값 (실제: HexStringConfidentialKey로 생성)
}

func NewDefaultCrumbIssuer(salt string) *DefaultCrumbIssuer {
	return &DefaultCrumbIssuer{Salt: salt}
}

func (c *DefaultCrumbIssuer) IssueCrumb(username, sessionID string) string {
	// 실제 DefaultCrumbIssuer.issueCrumb() 알고리즘 재현:
	// 1. username + ";" + sessionID로 버퍼 구성
	// 2. SHA-256(버퍼 + salt)로 해시 생성
	// 3. hex 문자열로 반환
	data := username + ";" + sessionID
	mac := hmac.New(sha256.New, []byte(c.Salt))
	mac.Write([]byte(data))
	return hex.EncodeToString(mac.Sum(nil))
}

func (c *DefaultCrumbIssuer) ValidateCrumb(username, sessionID, crumb string) bool {
	// 상수 시간 비교 -- 실제: MessageDigest.isEqual() 사용
	expected := c.IssueCrumb(username, sessionID)
	return hmac.Equal([]byte(expected), []byte(crumb))
}

func (c *DefaultCrumbIssuer) GetCrumbRequestField() string {
	return "Jenkins-Crumb" // 실제: CrumbIssuer.DEFAULT_CRUMB_NAME
}

// =============================================================================
// 9. FilterChain -- 서블릿 필터 체인 시뮬레이션
// =============================================================================
// Jenkins의 SecurityRealm.createFilterImpl()에서 조립하는 필터 체인을 시뮬레이션한다.
//
// 실제 필터 순서 (SecurityRealm.java createFilterImpl):
//   1. HttpSessionContextIntegrationFilter2 -- 세션에서 SecurityContext 복원
//   2. BasicHeaderProcessor -- "Authorization: Basic xxx:yyy" 처리
//   3. AuthenticationProcessingFilter2 -- 폼 로그인 처리 (/j_spring_security_check)
//   4. RememberMeAuthenticationFilter -- Remember-Me 쿠키 처리
//   5. AnonymousAuthenticationFilter -- 이전 필터에서 인증되지 않으면 익명으로 설정
//   6. ExceptionTranslationFilter -- 인증 예외 → 로그인 페이지 리다이렉트
//   7. UnwrapSecurityExceptionFilter -- 래핑된 보안 예외 언래핑
//   8. AcegiSecurityExceptionFilter -- Acegi 보안 예외 처리

// Filter 는 서블릿 필터를 시뮬레이션한다.
type Filter interface {
	DoFilter(req *Request, chain *FilterChain)
}

// Request 는 HTTP 요청을 시뮬레이션한다.
type Request struct {
	Method     string
	Path       string
	Headers    map[string]string
	SessionID  string
	CrumbToken string // CSRF 토큰
}

// FilterChain 은 필터 체인을 시뮬레이션한다.
type FilterChain struct {
	filters []Filter
	index   int
}

func NewFilterChain(filters ...Filter) *FilterChain {
	return &FilterChain{filters: filters, index: 0}
}

func (fc *FilterChain) DoFilter(req *Request) {
	if fc.index < len(fc.filters) {
		current := fc.filters[fc.index]
		fc.index++
		current.DoFilter(req, fc)
	}
}

// BasicHeaderFilter 는 Basic 인증 헤더를 처리하는 필터이다.
// Jenkins의 jenkins.security.BasicHeaderProcessor에 대응한다.
type BasicHeaderFilter struct {
	Realm SecurityRealm
}

func (f *BasicHeaderFilter) DoFilter(req *Request, chain *FilterChain) {
	if authHeader, ok := req.Headers["Authorization"]; ok && strings.HasPrefix(authHeader, "Basic ") {
		// "Basic username:password" 형식 파싱 (실제는 Base64)
		credentials := strings.TrimPrefix(authHeader, "Basic ")
		parts := strings.SplitN(credentials, ":", 2)
		if len(parts) == 2 {
			auth, err := f.Realm.Authenticate(parts[0], parts[1])
			if err != nil {
				fmt.Printf("    [BasicHeaderFilter] 인증 실패: %v\n", err)
			} else {
				fmt.Printf("    [BasicHeaderFilter] Basic 인증 성공: %s\n", auth.Name)
				globalSecurityContext.SetAuthentication(auth)
			}
		}
	}
	chain.DoFilter(req)
}

// AnonymousAuthFilter 는 인증되지 않은 요청에 익명 인증을 설정하는 필터이다.
// Jenkins의 AnonymousAuthenticationFilter에 대응한다.
//
// 실제 코드 (SecurityRealm.java commonFilters):
//   AnonymousAuthenticationFilter apf = new AnonymousAuthenticationFilter(
//       "anonymous", "anonymous",
//       List.of(new SimpleGrantedAuthority("anonymous")));
type AnonymousAuthFilter struct{}

func (f *AnonymousAuthFilter) DoFilter(req *Request, chain *FilterChain) {
	if !globalSecurityContext.GetAuthentication().Authenticated {
		fmt.Println("    [AnonymousAuthFilter] 인증되지 않은 요청 → 익명(anonymous)으로 설정")
		globalSecurityContext.SetAuthentication(ANONYMOUS)
	}
	chain.DoFilter(req)
}

// CrumbFilter 는 CSRF 토큰을 검증하는 필터이다.
// Jenkins의 CrumbFilter (jenkins.security.csrf)에 대응한다.
type CrumbFilter struct {
	Issuer CrumbIssuer
}

func (f *CrumbFilter) DoFilter(req *Request, chain *FilterChain) {
	// GET 요청은 CSRF 검증 생략 (상태 변경이 아니므로)
	if req.Method == "GET" {
		chain.DoFilter(req)
		return
	}

	// POST 요청은 CSRF 토큰 검증 필수
	auth := globalSecurityContext.GetAuthentication()
	if req.CrumbToken == "" {
		fmt.Printf("    [CrumbFilter] CSRF 토큰 없음 → 요청 거부 (%s %s)\n", req.Method, req.Path)
		return
	}

	if !f.Issuer.ValidateCrumb(auth.Name, req.SessionID, req.CrumbToken) {
		fmt.Printf("    [CrumbFilter] CSRF 토큰 불일치 → 요청 거부 (%s %s)\n", req.Method, req.Path)
		return
	}

	fmt.Printf("    [CrumbFilter] CSRF 토큰 검증 성공 (%s %s)\n", req.Method, req.Path)
	chain.DoFilter(req)
}

// =============================================================================
// 10. 헬퍼 함수 -- Permission 트리 시각화
// =============================================================================

// PrintPermissionTree 는 Permission 계층 구조를 트리 형태로 출력한다.
func PrintPermissionTree(perm *Permission, indent int) {
	prefix := strings.Repeat("  ", indent)
	connector := ""
	if indent > 0 {
		connector = "|-- "
	}
	fmt.Printf("%s%s%s (%s)\n", prefix, connector, perm.Name, perm.Group.Title)

	// 이 권한을 impliedBy로 참조하는 하위 권한들 찾기
	for _, p := range allPermissions {
		if p.ImpliedBy == perm {
			PrintPermissionTree(p, indent+1)
		}
	}
}

// PrintPermissionMatrix 는 사용자별 권한 매트릭스를 테이블로 출력한다.
func PrintPermissionMatrix(acl *MatrixACL, users []string, perms []*Permission) {
	// 헤더
	fmt.Printf("%-12s", "사용자")
	for _, p := range perms {
		fmt.Printf("| %-10s", p.Name)
	}
	fmt.Println("|")

	// 구분선
	fmt.Print(strings.Repeat("-", 12))
	for range perms {
		fmt.Print("+" + strings.Repeat("-", 11))
	}
	fmt.Println("+")

	// 데이터
	for _, username := range users {
		fmt.Printf("%-12s", username)
		for _, p := range perms {
			auth := &Authentication{Name: username, Authenticated: true}
			if acl.HasPermission(auth, p) {
				fmt.Printf("| %-10s", "V")
			} else {
				fmt.Printf("| %-10s", "-")
			}
		}
		fmt.Println("|")
	}
}

// =============================================================================
// main -- 데모 실행
// =============================================================================

func main() {
	fmt.Println("========================================================================")
	fmt.Println(" Jenkins 보안 서브시스템 PoC")
	fmt.Println(" SecurityRealm / AuthorizationStrategy / ACL / Permission / CrumbIssuer")
	fmt.Println("========================================================================")
	fmt.Println()

	// =========================================================================
	// [1] Permission 트리 구조 시각화
	// =========================================================================
	fmt.Println("=== [1] Permission 계층 구조 (impliedBy 트리) ===")
	fmt.Println()
	fmt.Println("Jenkins의 모든 권한은 트리 구조를 형성한다.")
	fmt.Println("상위 권한을 가진 사용자는 하위 권한도 자동으로 갖는다.")
	fmt.Println()
	PrintPermissionTree(HudsonAdminister, 0)
	fmt.Println()

	// =========================================================================
	// [2] SecurityRealm: 사용자 등록 및 인증
	// =========================================================================
	fmt.Println("=== [2] SecurityRealm: 사용자 등록 및 인증 (HudsonPrivateSecurityRealm) ===")
	fmt.Println()

	realm := NewHudsonPrivateSecurityRealm()

	// 사용자 등록
	users := []struct {
		name     string
		password string
		roles    []string
	}{
		{"admin", "admin123", []string{"authenticated", "admin"}},
		{"developer", "dev456", []string{"authenticated", "developer"}},
		{"viewer", "view789", []string{"authenticated", "viewer"}},
	}

	for _, u := range users {
		err := realm.CreateUser(u.name, u.password, u.roles)
		if err != nil {
			fmt.Printf("  [ERROR] 사용자 등록 실패: %v\n", err)
		} else {
			fmt.Printf("  [OK] 사용자 등록: %s (역할: %v)\n", u.name, u.roles)
		}
	}
	fmt.Println()

	// 인증 시도
	fmt.Println("--- 인증 시도 ---")
	testAuth := []struct {
		name     string
		password string
	}{
		{"admin", "admin123"},      // 성공
		{"developer", "dev456"},    // 성공
		{"developer", "wrong"},     // 실패 -- 잘못된 비밀번호
		{"unknown", "pass"},        // 실패 -- 존재하지 않는 사용자
	}

	for _, t := range testAuth {
		auth, err := realm.Authenticate(t.name, t.password)
		if err != nil {
			fmt.Printf("  [FAIL] %s / %s → %v\n", t.name, t.password, err)
		} else {
			fmt.Printf("  [OK]   %s / %s → 인증 성공 (authenticated=%v, authorities=%v)\n",
				t.name, t.password, auth.Authenticated, auth.Authorities)
		}
	}
	fmt.Println()

	// =========================================================================
	// [3] FullControlOnceLoggedInAuthorizationStrategy
	// =========================================================================
	fmt.Println("=== [3] FullControlOnceLoggedIn 인가 전략 ===")
	fmt.Println()
	fmt.Println("로그인한 사용자에게 모든 권한을 부여하는 가장 단순한 전략이다.")
	fmt.Println("Jenkins 초기 설정 시 기본 선택되는 전략이기도 하다.")
	fmt.Println()

	strategy := &FullControlOnceLoggedIn{DenyAnonymousReadAccess: false}
	acl := strategy.GetRootACL()

	adminAuth, _ := realm.Authenticate("admin", "admin123")

	permChecks := []struct {
		auth *Authentication
		perm *Permission
		desc string
	}{
		{adminAuth, HudsonAdminister, "admin → Administer"},
		{adminAuth, ItemBuild, "admin → Item.Build"},
		{ANONYMOUS, ItemRead, "anonymous → Item.Read (허용: 익명 읽기 허용 모드)"},
		{ANONYMOUS, ItemBuild, "anonymous → Item.Build (거부: 쓰기 권한 없음)"},
		{SYSTEM, HudsonAdminister, "SYSTEM → Administer (항상 허가)"},
	}

	for _, tc := range permChecks {
		err := CheckPermission(acl, tc.auth, tc.perm)
		if err != nil {
			fmt.Printf("  [DENIED]  %s\n            → %v\n", tc.desc, err)
		} else {
			fmt.Printf("  [GRANTED] %s\n", tc.desc)
		}
	}
	fmt.Println()

	// 익명 읽기 거부 모드 테스트
	fmt.Println("--- DenyAnonymousReadAccess = true ---")
	strategy2 := &FullControlOnceLoggedIn{DenyAnonymousReadAccess: true}
	acl2 := strategy2.GetRootACL()
	err := CheckPermission(acl2, ANONYMOUS, ItemRead)
	if err != nil {
		fmt.Printf("  [DENIED]  anonymous → Item.Read (거부: 익명 읽기 차단 모드)\n")
	}
	fmt.Println()

	// =========================================================================
	// [4] MatrixAuthorizationStrategy: 프로젝트별 매트릭스 권한
	// =========================================================================
	fmt.Println("=== [4] MatrixAuthorizationStrategy: 프로젝트별 매트릭스 권한 ===")
	fmt.Println()

	matrixStrategy := NewMatrixAuthorizationStrategy()

	// 글로벌 권한 설정
	// admin: 모든 권한 (Administer 부여 → 모든 하위 권한 내포)
	matrixStrategy.GlobalACL.Grant("admin", HudsonAdminister)

	// developer: Item 레벨 권한 (읽기, 빌드, 설정, 생성)
	matrixStrategy.GlobalACL.Grant("developer", ItemRead)
	matrixStrategy.GlobalACL.Grant("developer", ItemBuild)
	matrixStrategy.GlobalACL.Grant("developer", ItemConfigure)
	matrixStrategy.GlobalACL.Grant("developer", ItemCreate)

	// viewer: 읽기만
	matrixStrategy.GlobalACL.Grant("viewer", ItemRead)

	// 프로젝트별 ACL 설정
	projectACL := NewMatrixACL()
	projectACL.Grant("developer", ItemDelete) // 'my-project'에서만 삭제 권한 추가
	matrixStrategy.ProjectACLs["my-project"] = projectACL

	fmt.Println("--- 글로벌 권한 매트릭스 ---")
	PrintPermissionMatrix(matrixStrategy.GlobalACL,
		[]string{"admin", "developer", "viewer"},
		[]*Permission{HudsonAdminister, ItemCreate, ItemRead, ItemBuild, ItemConfigure, ItemDelete})
	fmt.Println()

	// 권한 검사 테스트
	fmt.Println("--- 권한 검사 ---")
	devAuth, _ := realm.Authenticate("developer", "dev456")
	viewerAuth, _ := realm.Authenticate("viewer", "view789")

	matrixChecks := []struct {
		auth     *Authentication
		perm     *Permission
		resource string
		desc     string
	}{
		{adminAuth, HudsonAdminister, "", "admin → Administer (글로벌)"},
		{adminAuth, ItemDelete, "", "admin → Item.Delete (Administer가 내포)"},
		{devAuth, ItemBuild, "", "developer → Item.Build (글로벌: 직접 부여)"},
		{devAuth, ItemDelete, "", "developer → Item.Delete (글로벌: 부여 안 됨)"},
		{devAuth, ItemDelete, "my-project", "developer → Item.Delete (my-project: 프로젝트별 부여)"},
		{viewerAuth, ItemRead, "", "viewer → Item.Read (글로벌: 직접 부여)"},
		{viewerAuth, ItemBuild, "", "viewer → Item.Build (글로벌: 부여 안 됨)"},
	}

	for _, tc := range matrixChecks {
		var targetACL ACL
		if tc.resource != "" {
			targetACL = matrixStrategy.GetACL(tc.resource)
		} else {
			targetACL = matrixStrategy.GetRootACL()
		}
		err := CheckPermission(targetACL, tc.auth, tc.perm)
		if err != nil {
			fmt.Printf("  [DENIED]  %s\n", tc.desc)
		} else {
			fmt.Printf("  [GRANTED] %s\n", tc.desc)
		}
	}
	fmt.Println()

	// =========================================================================
	// [5] CrumbIssuer: CSRF 토큰 발급 및 검증
	// =========================================================================
	fmt.Println("=== [5] CrumbIssuer: CSRF 토큰 발급 및 검증 ===")
	fmt.Println()

	crumbIssuer := NewDefaultCrumbIssuer("secret-salt-key-2024")
	sessionID := fmt.Sprintf("session-%d", time.Now().UnixNano())

	// 토큰 발급
	crumb := crumbIssuer.IssueCrumb("admin", sessionID)
	fmt.Printf("  CSRF 토큰 발급:\n")
	fmt.Printf("    사용자:     admin\n")
	fmt.Printf("    세션 ID:    %s\n", sessionID)
	fmt.Printf("    필드 이름:  %s\n", crumbIssuer.GetCrumbRequestField())
	fmt.Printf("    크럼 값:    %s\n", crumb)
	fmt.Println()

	// 토큰 검증
	fmt.Println("--- CSRF 토큰 검증 ---")
	crumbTests := []struct {
		username  string
		session   string
		token     string
		desc      string
	}{
		{"admin", sessionID, crumb, "올바른 토큰"},
		{"admin", sessionID, "invalid-crumb", "잘못된 토큰"},
		{"admin", "different-session", crumb, "다른 세션 (세션 고정 공격 방어)"},
		{"hacker", sessionID, crumb, "다른 사용자 (크로스사이트 공격 방어)"},
	}

	for _, tc := range crumbTests {
		valid := crumbIssuer.ValidateCrumb(tc.username, tc.session, tc.token)
		status := "INVALID"
		if valid {
			status = "VALID"
		}
		sessionPreview := tc.session
		if len(sessionPreview) > 20 {
			sessionPreview = sessionPreview[:20] + "..."
		}
		fmt.Printf("  [%7s] %s (user=%s, session=%s)\n", status, tc.desc, tc.username, sessionPreview)
	}
	fmt.Println()

	// =========================================================================
	// [6] Filter Chain: 인증 흐름 시뮬레이션
	// =========================================================================
	fmt.Println("=== [6] Filter Chain: 요청 → 인증 → 인가 흐름 ===")
	fmt.Println()
	fmt.Println("Jenkins의 모든 HTTP 요청은 SecurityRealm이 생성한 필터 체인을 통과한다.")
	fmt.Println("필터 순서: BasicHeaderFilter → AnonymousAuthFilter → CrumbFilter")
	fmt.Println()

	// 시나리오 1: Basic 인증 GET 요청
	fmt.Println("--- 시나리오 1: Basic 인증 GET 요청 ---")
	globalSecurityContext.SetAuthentication(ANONYMOUS) // 초기화

	chain1 := NewFilterChain(
		&BasicHeaderFilter{Realm: realm},
		&AnonymousAuthFilter{},
		&CrumbFilter{Issuer: crumbIssuer},
	)
	req1 := &Request{
		Method:  "GET",
		Path:    "/job/my-project",
		Headers: map[string]string{"Authorization": "Basic admin:admin123"},
	}
	fmt.Printf("  요청: %s %s (Authorization: Basic admin:***)\n", req1.Method, req1.Path)
	chain1.DoFilter(req1)
	fmt.Printf("  결과: 인증된 사용자 = %s\n", globalSecurityContext.GetAuthentication().Name)
	fmt.Println()

	// 시나리오 2: 인증 없는 GET 요청
	fmt.Println("--- 시나리오 2: 인증 없는 GET 요청 ---")
	globalSecurityContext.SetAuthentication(&Authentication{Name: "", Authenticated: false})

	chain2 := NewFilterChain(
		&BasicHeaderFilter{Realm: realm},
		&AnonymousAuthFilter{},
		&CrumbFilter{Issuer: crumbIssuer},
	)
	req2 := &Request{
		Method:  "GET",
		Path:    "/",
		Headers: map[string]string{},
	}
	fmt.Printf("  요청: %s %s (인증 헤더 없음)\n", req2.Method, req2.Path)
	chain2.DoFilter(req2)
	fmt.Printf("  결과: 인증된 사용자 = %s (익명)\n", globalSecurityContext.GetAuthentication().Name)
	fmt.Println()

	// 시나리오 3: POST 요청 + CSRF 토큰
	fmt.Println("--- 시나리오 3: POST 요청 + 올바른 CSRF 토큰 ---")
	globalSecurityContext.SetAuthentication(adminAuth)
	postSessionID := "session-post-12345"
	postCrumb := crumbIssuer.IssueCrumb("admin", postSessionID)

	chain3 := NewFilterChain(
		&BasicHeaderFilter{Realm: realm},
		&AnonymousAuthFilter{},
		&CrumbFilter{Issuer: crumbIssuer},
	)
	req3 := &Request{
		Method:     "POST",
		Path:       "/job/my-project/build",
		Headers:    map[string]string{},
		SessionID:  postSessionID,
		CrumbToken: postCrumb,
	}
	fmt.Printf("  요청: %s %s (CSRF 토큰 포함)\n", req3.Method, req3.Path)
	chain3.DoFilter(req3)
	fmt.Println()

	// 시나리오 4: POST 요청 + CSRF 토큰 없음
	fmt.Println("--- 시나리오 4: POST 요청 + CSRF 토큰 없음 ---")
	chain4 := NewFilterChain(
		&BasicHeaderFilter{Realm: realm},
		&AnonymousAuthFilter{},
		&CrumbFilter{Issuer: crumbIssuer},
	)
	req4 := &Request{
		Method:  "POST",
		Path:    "/job/my-project/build",
		Headers: map[string]string{},
	}
	fmt.Printf("  요청: %s %s (CSRF 토큰 없음)\n", req4.Method, req4.Path)
	chain4.DoFilter(req4)
	fmt.Println()

	// =========================================================================
	// [7] Impersonation: 사용자 전환
	// =========================================================================
	fmt.Println("=== [7] Impersonation: 사용자 전환 (ACL.as2/impersonate2) ===")
	fmt.Println()
	fmt.Println("Jenkins는 내부적으로 SYSTEM 인증으로 전환하여 권한 검사를 우회한다.")
	fmt.Println("예: 빌드 실행 시 SYSTEM으로 전환하여 모든 리소스에 접근한다.")
	fmt.Println()

	globalSecurityContext.SetAuthentication(devAuth)
	fmt.Printf("  현재 사용자: %s\n", globalSecurityContext.GetAuthentication().Name)

	// Administer 권한 확인 (developer는 없음)
	matrixACL := matrixStrategy.GetRootACL()
	err = CheckPermission(matrixACL, globalSecurityContext.GetAuthentication(), HudsonAdminister)
	fmt.Printf("  developer → Administer: %v\n", err)

	// SYSTEM으로 전환
	old := globalSecurityContext.Impersonate(SYSTEM)
	fmt.Printf("\n  [impersonate] SYSTEM으로 전환\n")
	fmt.Printf("  현재 사용자: %s\n", globalSecurityContext.GetAuthentication().Name)

	err = CheckPermission(matrixACL, globalSecurityContext.GetAuthentication(), HudsonAdminister)
	if err == nil {
		fmt.Printf("  SYSTEM → Administer: 허가 (SYSTEM은 항상 모든 권한 보유)\n")
	}

	// 원래 사용자로 복원
	globalSecurityContext.SetAuthentication(old)
	fmt.Printf("\n  [restore] 원래 사용자로 복원\n")
	fmt.Printf("  현재 사용자: %s\n", globalSecurityContext.GetAuthentication().Name)
	fmt.Println()

	// =========================================================================
	// [8] 종합 시나리오: 전체 보안 흐름
	// =========================================================================
	fmt.Println("=== [8] 종합 시나리오: 전체 보안 흐름 ===")
	fmt.Println()
	fmt.Println("  요청 → Filter Chain → SecurityRealm.authenticate()")
	fmt.Println("  → SecurityContext 설정 → AuthorizationStrategy.getACL()")
	fmt.Println("  → ACL.hasPermission2(auth, permission) → 허가/거부")
	fmt.Println()

	scenarios := []struct {
		user     string
		password string
		resource string
		perm     *Permission
		desc     string
	}{
		{"admin", "admin123", "my-project", ItemBuild, "관리자가 프로젝트 빌드"},
		{"developer", "dev456", "my-project", ItemBuild, "개발자가 프로젝트 빌드"},
		{"developer", "dev456", "my-project", ItemDelete, "개발자가 프로젝트 삭제 (프로젝트별 ACL)"},
		{"developer", "dev456", "other-project", ItemDelete, "개발자가 다른 프로젝트 삭제 (글로벌 ACL)"},
		{"viewer", "view789", "my-project", ItemRead, "뷰어가 프로젝트 읽기"},
		{"viewer", "view789", "my-project", ItemBuild, "뷰어가 프로젝트 빌드 시도"},
	}

	for i, s := range scenarios {
		fmt.Printf("  [시나리오 %d] %s\n", i+1, s.desc)

		// 1. 인증
		auth, authErr := realm.Authenticate(s.user, s.password)
		if authErr != nil {
			fmt.Printf("    인증 실패: %v\n\n", authErr)
			continue
		}
		fmt.Printf("    1) 인증: %s → 성공\n", s.user)

		// 2. ACL 조회
		targetACL := matrixStrategy.GetACL(s.resource)
		aclType := "글로벌"
		if _, ok := matrixStrategy.ProjectACLs[s.resource]; ok {
			aclType = "프로젝트별"
		}
		fmt.Printf("    2) ACL 조회: %s (%s ACL)\n", s.resource, aclType)

		// 3. 권한 검사
		permErr := CheckPermission(targetACL, auth, s.perm)
		if permErr != nil {
			fmt.Printf("    3) 권한 검사: %s → DENIED\n", s.perm.ID())
		} else {
			fmt.Printf("    3) 권한 검사: %s → GRANTED\n", s.perm.ID())
		}
		fmt.Println()
	}

	fmt.Println("========================================================================")
	fmt.Println(" PoC 완료")
	fmt.Println("========================================================================")
}
