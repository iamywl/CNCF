# 12. Grafana 인가(Authorization)와 RBAC 심화

## 목차

1. [접근 제어 아키텍처 개요](#1-접근-제어-아키텍처-개요)
2. [권한 모델: Permission](#2-권한-모델-permission)
3. [스코프(Scope) 시스템](#3-스코프scope-시스템)
4. [평가기(Evaluator) 패턴](#4-평가기evaluator-패턴)
5. [역할(Role) 체계](#5-역할role-체계)
6. [Fixed Role 정의와 등록](#6-fixed-role-정의와-등록)
7. [리소스별 권한 서비스](#7-리소스별-권한-서비스)
8. [미들웨어 통합](#8-미들웨어-통합)
9. [AuthorizeInOrg 미들웨어](#9-authorizeinorg-미들웨어)
10. [스코프 리졸버](#10-스코프-리졸버)
11. [권한 평가 흐름 상세](#11-권한-평가-흐름-상세)
12. [멀티테넌시와 조직 모델](#12-멀티테넌시와-조직-모델)
13. [레거시 역할 기반 접근 제어](#13-레거시-역할-기반-접근-제어)
14. [LegacyAccessClient와 K8s 통합](#14-legacyaccessclient와-k8s-통합)
15. [Action과 Scope 전체 목록](#15-action과-scope-전체-목록)
16. [권한 캐싱과 최적화](#16-권한-캐싱과-최적화)
17. [권한 평가 예제 시나리오](#17-권한-평가-예제-시나리오)
18. [설계 원칙과 트레이드오프](#18-설계-원칙과-트레이드오프)

---

## 1. 접근 제어 아키텍처 개요

Grafana의 인가(Authorization) 시스템은 **RBAC(Role-Based Access Control)**을 기반으로 하며, `pkg/services/accesscontrol/` 패키지에 구현되어 있다.

### 핵심 소스 위치

```
pkg/services/accesscontrol/
  accesscontrol.go    -- AccessControl/Service 인터페이스, 헬퍼 함수
  evaluator.go        -- Evaluator 인터페이스 (EvalPermission, EvalAll, EvalAny)
  models.go           -- Permission, Role, RoleDTO, ResourcePermission 모델
  middleware.go        -- HTTP 미들웨어 (Middleware, AuthorizeInOrgMiddleware)
  scope.go            -- Scope 빌더, ScopeProvider, 와일드카드 처리
  resolvers.go        -- ScopeAttributeResolver, 캐시 기반 스코프 해석
  roles.go            -- Fixed Role 정의, Basic Role 빌더
  authorizer.go       -- LegacyAccessClient (K8s 통합)
  checker.go          -- Checker 함수 (리소스 필터링)
  filter.go           -- SQL 필터 생성
  metadata.go         -- 메타데이터 수집
  errors.go           -- 에러 정의
```

### AccessControl 인터페이스

```go
// pkg/services/accesscontrol/accesscontrol.go
type AccessControl interface {
    // Evaluate는 주어진 리소스에 대한 접근을 평가한다
    Evaluate(ctx context.Context, user identity.Requester, evaluator Evaluator) (bool, error)
    // RegisterScopeAttributeResolver는 특정 스코프 접두사에 대한 리졸버를 등록한다
    RegisterScopeAttributeResolver(prefix string, resolver ScopeAttributeResolver)
    // WithoutResolvers는 리졸버 없는 AccessControl 복사본을 반환한다
    WithoutResolvers() AccessControl
    // InvalidateResolverCache는 스코프 해석 캐시를 무효화한다
    InvalidateResolverCache(orgID int64, scope string)
}
```

### Service 인터페이스

```go
type Service interface {
    registry.ProvidesUsageStats
    GetRoleByName(ctx context.Context, orgID int64, roleName string) (*RoleDTO, error)
    GetUserPermissions(ctx context.Context, user identity.Requester, options Options) ([]Permission, error)
    SearchUsersPermissions(ctx context.Context, user identity.Requester, options SearchOptions) (map[int64][]Permission, error)
    ClearUserPermissionCache(user identity.Requester)
    SearchUserPermissions(ctx context.Context, orgID int64, filterOptions SearchOptions) ([]Permission, error)
    DeleteUserPermissions(ctx context.Context, orgID, userID int64) error
    DeleteTeamPermissions(ctx context.Context, orgID, teamID int64) error
    DeclareFixedRoles(registrations ...RoleRegistration) error
    SaveExternalServiceRole(ctx context.Context, cmd SaveExternalServiceRoleCommand) error
    DeleteExternalServiceRole(ctx context.Context, externalServiceID string) error
    SyncUserRoles(ctx context.Context, orgID int64, cmd SyncUserRolesCommand) error
    GetStaticRoles(ctx context.Context) map[string]*RoleDTO
}
```

### 전체 인가 흐름

```
HTTP 요청
    |
    v
[인증 단계] → Identity 반환 (Permissions 포함)
    |
    v
[미들웨어] Middleware(ac)(evaluator)
    |
    v
+---> 토큰 에러 확인 (LookupTokenErr)
|     +---> TokenRevokedError → tokenRevoked()
|     +---> 기타 에러 → unauthorized()
|
+---> authorize(c, ac, user, evaluator)
      |
      +---> evaluator.MutateScopes() → URL 파라미터 주입
      |
      +---> ac.Evaluate(ctx, user, injected)
            |
            +---> 사용자 권한에서 리졸버로 스코프 해석
            +---> evaluator.Evaluate(permissions) → bool
            |
            +---> true → 요청 진행
            +---> false → deny() → 403 Forbidden
```

---

## 2. 권한 모델: Permission

### Permission 구조체

```go
// pkg/services/accesscontrol/models.go
type Permission struct {
    ID         int64     `json:"-" xorm:"pk autoincr 'id'"`
    RoleID     int64     `json:"-" xorm:"role_id"`
    Action     string    `json:"action"`
    Scope      string    `json:"scope"`
    Kind       string    `json:"-"`
    Attribute  string    `json:"-"`
    Identifier string    `json:"-"`
    Updated    time.Time `json:"updated"`
    Created    time.Time `json:"created"`
}
```

Permission은 **Action + Scope** 쌍으로 구성된다:

| 구성 요소 | 설명 | 예시 |
|----------|------|------|
| **Action** | 수행할 수 있는 작업 | `dashboards:read`, `datasources:write` |
| **Scope** | 작업 대상 리소스 범위 | `dashboards:uid:abc`, `datasources:*` |

### Action 네이밍 컨벤션

```
<리소스>:<동작>

리소스: dashboards, datasources, teams, users, orgs, alert.rules 등
동작: read, write, create, delete

예시:
  dashboards:read      → 대시보드 읽기
  datasources:write    → 데이터소스 수정
  teams:create         → 팀 생성
  users:delete         → 사용자 삭제
  alert.rules:read     → 알림 규칙 읽기
```

하위 리소스는 점(.)으로 구분한다:

```
users.password:write         → 사용자 비밀번호 변경
users.authtoken:read         → 사용자 인증 토큰 조회
teams.permissions:write      → 팀 권한 변경
alert.notifications.receivers:read → 알림 수신자 조회
```

### Scope 구조

Scope는 `kind:attribute:identifier` 형태의 3단계 구조를 가진다:

```
dashboards:uid:my-dashboard   → kind=dashboards, attribute=uid, identifier=my-dashboard
datasources:id:42             → kind=datasources, attribute=id, identifier=42
teams:*                       → kind=teams, 와일드카드 (모든 팀)
*                             → 전역 와일드카드 (모든 리소스)
```

`SplitScope` 함수가 스코프를 파싱한다:

```go
// pkg/services/accesscontrol/scope.go
func SplitScope(scope string) (string, string, string) {
    fragments := strings.Split(scope, ":")
    switch l := len(fragments); l {
    case 1:  // "*" → ("*", "*", "*")
        return fragments[0], fragments[0], fragments[0]
    case 2:  // "dashboards:*" → ("dashboards", "*", "*")
        return fragments[0], fragments[1], fragments[1]
    default: // "dashboards:uid:test" → ("dashboards", "uid", "test")
        return fragments[0], fragments[1], strings.Join(fragments[2:], ":")
    }
}
```

---

## 3. 스코프(Scope) 시스템

### ScopeProvider

`ScopeProvider`는 특정 리소스 타입에 대한 스코프를 생성하는 팩토리다:

```go
// pkg/services/accesscontrol/scope.go
type ScopeProvider interface {
    GetResourceScope(resourceID string) string      // "dashboards:id:123"
    GetResourceScopeUID(resourceID string) string    // "dashboards:uid:abc"
    GetResourceScopeName(resourceID string) string   // "dashboards:name:test"
    GetResourceScopeType(typeName string) string     // "dashboards:type:dash-db"
    GetResourceAllScope() string                     // "dashboards:*"
    GetResourceAllIDScope() string                   // "dashboards:id:*"
}
```

사용 예시:

```go
ScopeAnnotationsProvider = NewScopeProvider("annotations")
ScopeAnnotationsAll = ScopeAnnotationsProvider.GetResourceAllScope()  // "annotations:*"
ScopeAnnotationsID = Scope("annotations", "id", Parameter(":annotationId"))
```

### Scope 빌더 함수

```go
// Scope는 부분들을 ':'로 결합한다
func Scope(parts ...string) string  // Scope("users", "id", "42") → "users:id:42"

// Parameter는 URL 파라미터를 Go 템플릿으로 변환한다
func Parameter(key string) string   // Parameter(":id") → `{{ index .URLParams ":id" }}`

// Field는 요청 컨텍스트 필드를 참조한다
func Field(key string) string       // Field("OrgID") → `{{ .OrgID }}`
```

### 와일드카드 매칭

와일드카드 스코프는 **접두사 매칭** 방식으로 동작한다:

```
사용자 스코프        | 대상 스코프            | 매칭 여부
--------------------|-----------------------|----------
*                   | dashboards:uid:abc    | O (전역 와일드카드)
dashboards:*        | dashboards:uid:abc    | O (kind 레벨 와일드카드)
dashboards:uid:*    | dashboards:uid:abc    | O (attribute 레벨 와일드카드)
dashboards:uid:abc  | dashboards:uid:abc    | O (정확한 매칭)
dashboards:uid:abc  | dashboards:uid:def    | X
datasources:*       | dashboards:uid:abc    | X (다른 kind)
```

와일드카드 생성 로직:

```go
// pkg/services/accesscontrol/scope.go
func WildcardsFromPrefixes(prefixes []string) Wildcards {
    wildcards := Wildcards{"*"}
    for _, prefix := range prefixes {
        parts := strings.Split(prefix, ":")
        for _, p := range parts {
            if p == "" { continue }
            b.WriteString(p)
            b.WriteRune(':')
            wildcards = append(wildcards, b.String()+"*")
        }
    }
    return wildcards
}

// 예: "datasources:uid:" → ["*", "datasources:*", "datasources:uid:*"]
```

### ScopePrefix

```go
func ScopePrefix(scope string) string {
    parts := strings.Split(scope, ":")
    if len(parts) > maxPrefixParts {  // maxPrefixParts = 2
        parts = append(parts[:maxPrefixParts], "")
    }
    return strings.Join(parts, ":")
}

// "datasources:uid:abc" → "datasources:uid:"
// "teams:*" → "teams:"
```

---

## 4. 평가기(Evaluator) 패턴

평가기는 **Composite 패턴**으로 구현되어, 복잡한 권한 조건을 트리 구조로 표현한다.

### Evaluator 인터페이스

```go
// pkg/services/accesscontrol/evaluator.go
type Evaluator interface {
    // Evaluate는 액션별로 그룹화된 권한을 평가한다
    Evaluate(permissions map[string][]string) bool
    // EvaluateCustom은 커스텀 체커 함수로 평가한다
    EvaluateCustom(fn CheckerFn) (bool, error)
    // MutateScopes는 스코프에 파라미터를 주입한 새 Evaluator를 반환한다
    MutateScopes(ctx context.Context, mutate ScopeAttributeMutator) (Evaluator, error)
    fmt.Stringer     // 문자열 표현
    fmt.GoStringer   // Go 문자열 표현
}
```

### EvalPermission (단일 권한 평가)

```go
func EvalPermission(action string, scopes ...string) Evaluator {
    return permissionEvaluator{Action: action, Scopes: scopes}
}
```

동작 로직:

```go
func (p permissionEvaluator) Evaluate(permissions map[string][]string) bool {
    userScopes, ok := permissions[p.Action]
    if !ok {
        return false  // 액션 자체가 없으면 거부
    }
    if len(p.Scopes) == 0 {
        return true   // 스코프 없으면 액션만 확인
    }
    for _, target := range p.Scopes {
        for _, scope := range userScopes {
            if match(scope, target) {
                return true  // 하나라도 매칭되면 허용
            }
        }
    }
    return false
}
```

### match 함수 (스코프 매칭)

```go
func match(scope, target string) bool {
    if scope == "" { return false }
    if !ValidateScope(scope) { return false }  // 메타문자 검증

    prefix, last := scope[:len(scope)-1], scope[len(scope)-1]
    if last == '*' {
        if strings.HasPrefix(target, prefix) {
            return true  // 와일드카드 접두사 매칭
        }
    }
    return scope == target  // 정확한 매칭
}
```

### ValidateScope (스코프 검증)

```go
func ValidateScope(scope string) bool {
    prefix, last := scope[:len(scope)-1], scope[len(scope)-1]
    if len(prefix) > 0 && last == '*' {
        lastChar := prefix[len(prefix)-1]
        if lastChar != ':' && lastChar != '/' {
            return false  // '*'는 ':' 또는 '/' 뒤에만 허용
        }
    }
    return !strings.ContainsAny(prefix, "*?")  // 접두사에 메타문자 불허
}

// 유효: "dashboards:*", "dashboards:uid:*", "*"
// 무효: "dash*boards:uid:abc", "dashboards:u*id:abc"
```

### EvalAll (AND 논리)

```go
func EvalAll(allOf ...Evaluator) Evaluator {
    return allEvaluator{allOf: allOf}
}

func (a allEvaluator) Evaluate(permissions map[string][]string) bool {
    for _, e := range a.allOf {
        if !e.Evaluate(permissions) {
            return false  // 하나라도 실패하면 거부
        }
    }
    return true
}
```

### EvalAny (OR 논리)

```go
func EvalAny(anyOf ...Evaluator) Evaluator {
    return anyEvaluator{anyOf: anyOf}
}

func (a anyEvaluator) Evaluate(permissions map[string][]string) bool {
    for _, e := range a.anyOf {
        if e.Evaluate(permissions) {
            return true  // 하나라도 성공하면 허용
        }
    }
    return false
}
```

### Evaluator 트리 구조 예시

실제 코드에 정의된 `TeamsAccessEvaluator`:

```go
var TeamsAccessEvaluator = EvalAny(
    EvalPermission(ActionTeamsCreate),
    EvalAll(
        EvalPermission(ActionTeamsRead),
        EvalAny(
            EvalPermission(ActionTeamsWrite),
            EvalPermission(ActionTeamsPermissionsWrite),
            EvalPermission(ActionTeamsPermissionsRead),
        ),
    ),
)
```

이를 트리로 표현하면:

```
              EvalAny (OR)
              /          \
   EvalPermission    EvalAll (AND)
   (teams:create)    /          \
                EvalPermission  EvalAny (OR)
                (teams:read)    /     |      \
                          teams:write  teams.permissions:write  teams.permissions:read
```

이 Evaluator는 다음 조건 중 하나를 만족하면 접근을 허용한다:
1. `teams:create` 권한이 있거나
2. `teams:read` AND (`teams:write` OR `teams.permissions:write` OR `teams.permissions:read`)

### MutateScopes (스코프 파라미터 주입)

URL 파라미터를 스코프에 주입하는 데 사용된다:

```go
func (p permissionEvaluator) MutateScopes(ctx context.Context, mutate ScopeAttributeMutator) (Evaluator, error) {
    scopes := make([]string, 0, len(p.Scopes))
    for _, scope := range p.Scopes {
        mutated, err := mutate(ctx, scope)
        scopes = append(scopes, mutated...)
    }
    return EvalPermission(p.Action, scopes...), nil
}
```

예시:

```
정의: EvalPermission("teams:read", "teams:id:{{ index .URLParams \":teamId\" }}")
요청: GET /api/teams/42
주입 후: EvalPermission("teams:read", "teams:id:42")
```

---

## 5. 역할(Role) 체계

Grafana의 역할 체계는 **4단계 계층**으로 구성된다.

### 역할 종류

| 종류 | 접두사 | 설명 |
|------|--------|------|
| Basic | `basic:` | 기본 역할 (None, Viewer, Editor, Admin, Grafana Admin) |
| Fixed | `fixed:` | 서비스가 선언한 고정 역할 |
| Managed | `managed:` | 사용자/팀별 리소스 권한 |
| Plugin | `plugins:` | 플러그인이 정의한 역할 |
| ExternalService | `extsvc:` | 외부 서비스 역할 |

### Basic Role 정의

```go
// pkg/services/accesscontrol/roles.go
func BuildBasicRoleDefinitions() map[string]*RoleDTO {
    return map[string]*RoleDTO{
        string(org.RoleAdmin): {
            Name: "basic:admin",    UID: "basic_admin",
            OrgID: GlobalOrgID,     DisplayName: "Admin",
        },
        string(org.RoleEditor): {
            Name: "basic:editor",   UID: "basic_editor",
            OrgID: GlobalOrgID,     DisplayName: "Editor",
        },
        string(org.RoleViewer): {
            Name: "basic:viewer",   UID: "basic_viewer",
            OrgID: GlobalOrgID,     DisplayName: "Viewer",
        },
        string(org.RoleNone): {
            Name: "basic:none",     UID: "basic_none",
            OrgID: GlobalOrgID,     DisplayName: "None",
        },
        RoleGrafanaAdmin: {
            Name: "basic:grafana_admin", UID: "basic_grafana_admin",
            OrgID: GlobalOrgID,          DisplayName: "Grafana Admin",
        },
    }
}
```

### 역할 상속 관계

```
Grafana Admin (서버 전역 관리자)
    |
    +---> 모든 조직에서 최고 권한
    |
    v
Admin (조직 관리자)
    |
    +---> Editor 권한 포함
    |
    v
Editor (편집자)
    |
    +---> Viewer 권한 포함
    |
    v
Viewer (조회자)
    |
    +---> 최소 읽기 권한
    |
    v
None (무권한)
    |
    +---> 권한 없음 (RBAC 권한만으로 접근)
```

`GetOrgRoles`는 상속을 포함한 역할 목록을 반환한다:

```go
func GetOrgRoles(user identity.Requester) []string {
    roles := []string{string(user.GetOrgRole())}
    if user.GetIsGrafanaAdmin() {
        if user.GetOrgID() == GlobalOrgID {
            return []string{RoleGrafanaAdmin, string(org.RoleAdmin)}
        }
        roles = append(roles, RoleGrafanaAdmin)
    }
    return roles
}
```

### Role 모델

```go
type Role struct {
    ID          int64     `xorm:"pk autoincr 'id'"`
    OrgID       int64     `xorm:"org_id"`
    Version     int64
    UID         string    `xorm:"uid"`
    Name        string
    DisplayName string
    Group       string    `xorm:"group_name"`
    Description string
    Hidden      bool
    Updated     time.Time
    Created     time.Time
}
```

### RoleDTO (Permissions 포함)

```go
type RoleDTO struct {
    Version     int64
    UID         string
    Name        string
    DisplayName string
    Description string
    Group       string
    Permissions []Permission  // 이 역할에 부여된 권한 목록
    Delegatable *bool
    Mapped      bool
    Hidden      bool
    ID          int64
    OrgID       int64
    Updated     time.Time
    Created     time.Time
}
```

### 역할 타입 판별 메서드

```go
func (r *RoleDTO) IsManaged() bool        { return strings.HasPrefix(r.Name, ManagedRolePrefix) }
func (r *RoleDTO) IsFixed() bool          { return strings.HasPrefix(r.Name, FixedRolePrefix) }
func (r *RoleDTO) IsPlugin() bool         { return strings.HasPrefix(r.Name, PluginRolePrefix) }
func (r *RoleDTO) IsBasic() bool          { return strings.HasPrefix(r.Name, BasicRolePrefix) }
func (r *RoleDTO) IsExternalService() bool { return strings.HasPrefix(r.Name, ExternalServiceRolePrefix) }
func (r *RoleDTO) Global() bool           { return r.OrgID == GlobalOrgID }
```

---

## 6. Fixed Role 정의와 등록

Fixed Role은 서비스가 시작 시 선언하는 **불변 역할**이다.

### RoleRegistration

```go
type RoleRegistration struct {
    Role    RoleDTO     // 역할 정의 (이름, 설명, 권한 목록)
    Grants  []string    // 이 역할을 부여받을 기본 역할 ("Viewer", "Editor", "Admin", "Grafana Admin")
    Exclude []string    // 제외할 기본 역할
}
```

### OSS Fixed Role 등록

```go
// pkg/services/accesscontrol/roles.go
func DeclareFixedRoles(service Service, cfg *setting.Cfg) error {
    ldapReader := RoleRegistration{
        Role:   ldapReaderRole,           // fixed:ldap:reader
        Grants: []string{RoleGrafanaAdmin},
    }
    orgUsersReader := RoleRegistration{
        Role:   orgUsersReaderRole,       // fixed:org.users:reader
        Grants: []string{RoleGrafanaAdmin, string(org.RoleAdmin)},
    }
    usersReader := RoleRegistration{
        Role:   usersReaderRole,          // fixed:users:reader
        Grants: []string{RoleGrafanaAdmin},
    }
    // ... 기타 역할

    return service.DeclareFixedRoles(
        ldapReader, ldapWriter, orgUsersReader, orgUsersWriter,
        settingsReader, statsReader, usersReader, usersWriter,
        authenticationConfigWriter, generalAuthConfigWriter, usageStatsReader,
    )
}
```

### Fixed Role 예시

```go
var ldapReaderRole = RoleDTO{
    Name:        "fixed:ldap:reader",
    DisplayName: "Reader",
    Description: "Read LDAP configuration and status.",
    Group:       "LDAP",
    Permissions: []Permission{
        {Action: ActionLDAPUsersRead},
        {Action: ActionLDAPStatusRead},
    },
}

var orgUsersWriterRole = RoleDTO{
    Name:        "fixed:org.users:writer",
    DisplayName: "Writer (organizational)",
    Description: "Within a single organization, add, read, remove users or change roles.",
    Group:       "User administration",
    Permissions: ConcatPermissions(orgUsersReaderRole.Permissions, []Permission{
        {Action: ActionOrgUsersAdd, Scope: ScopeUsersAll},
        {Action: ActionOrgUsersWrite, Scope: ScopeUsersAll},
        {Action: ActionOrgUsersRemove, Scope: ScopeUsersAll},
    }),
}
```

### 역할 UID 생성

```go
func PrefixedRoleUID(roleName string) string {
    prefix := strings.Split(roleName, ":")[0] + "_"
    hasher := sha1.New()
    hasher.Write([]byte(roleName))
    return fmt.Sprintf("%s%s", prefix, base64.RawURLEncoding.EncodeToString(hasher.Sum(nil)))
}

// "fixed:ldap:reader" → "fixed_" + base64(sha1("fixed:ldap:reader"))
```

### RegistrationList (동시성 안전)

```go
type RegistrationList struct {
    mx            sync.RWMutex
    registrations []RoleRegistration
}

func (m *RegistrationList) Append(regs ...RoleRegistration) {
    m.mx.Lock()
    defer m.mx.Unlock()
    m.registrations = append(m.registrations, regs...)
}

func (m *RegistrationList) Range(f func(registration RoleRegistration) bool) {
    m.mx.RLock()
    defer m.mx.RUnlock()
    for _, registration := range m.registrations {
        if ok := f(registration); !ok { return }
    }
}
```

---

## 7. 리소스별 권한 서비스

Grafana는 리소스 타입별로 전용 권한 서비스를 제공한다.

### PermissionsService 인터페이스

```go
// pkg/services/accesscontrol/accesscontrol.go
type PermissionsService interface {
    GetPermissions(ctx context.Context, user identity.Requester, resourceID string) ([]ResourcePermission, error)
    SetUserPermission(ctx context.Context, orgID int64, user User, resourceID, permission string) (*ResourcePermission, error)
    SetTeamPermission(ctx context.Context, orgID, teamID int64, resourceID, permission string) (*ResourcePermission, error)
    SetBuiltInRolePermission(ctx context.Context, orgID int64, builtInRole string, resourceID string, permission string) (*ResourcePermission, error)
    SetPermissions(ctx context.Context, orgID int64, resourceID string, commands ...SetResourcePermissionCommand) ([]ResourcePermission, error)
    MapActions(permission ResourcePermission) string
    DeleteResourcePermissions(ctx context.Context, orgID int64, resourceID string) error
}
```

### 리소스별 서비스 타입

```go
type FolderPermissionsService interface {
    PermissionsService
}

type DashboardPermissionsService interface {
    PermissionsService
}

type DatasourcePermissionsService interface {
    PermissionsService
}

type ServiceAccountPermissionsService interface {
    PermissionsService
}

type ReceiverPermissionsService interface {
    PermissionsService
    SetDefaultPermissions(ctx context.Context, orgID int64, user identity.Requester, uid string)
    CopyPermissions(ctx context.Context, orgID int64, user identity.Requester, oldUID, newUID string) (int, error)
}

type TeamPermissionsService interface {
    GetPermissions(ctx context.Context, user identity.Requester, resourceID string) ([]ResourcePermission, error)
    SetUserPermission(ctx context.Context, orgID int64, user User, resourceID, permission string) (*ResourcePermission, error)
    SetPermissions(ctx context.Context, orgID int64, resourceID string, commands ...SetResourcePermissionCommand) ([]ResourcePermission, error)
}
```

### ResourcePermission 모델

```go
type ResourcePermission struct {
    ID               int64
    RoleName         string
    Actions          []string     // 허용된 액션 목록
    Scope            string       // 리소스 스코프
    UserID           int64        // 사용자 ID (사용자 권한인 경우)
    UserUID          string
    UserLogin        string
    UserEmail        string
    TeamID           int64        // 팀 ID (팀 권한인 경우)
    TeamUID          string
    TeamEmail        string
    Team             string
    BuiltInRole      string       // 기본 역할 (기본 역할 권한인 경우)
    IsManaged        bool
    IsInherited      bool         // 상위 폴더에서 상속된 권한
    IsServiceAccount bool
    Created          time.Time
    Updated          time.Time
}
```

### SetResourcePermissionCommand

```go
type SetResourcePermissionCommand struct {
    UserID      int64  `json:"userId,omitempty"`
    TeamID      int64  `json:"teamId,omitempty"`
    BuiltinRole string `json:"builtInRole,omitempty"`
    Permission  string `json:"permission"`  // "View", "Edit", "Admin" 등
}
```

### Managed Role 이름 생성

```go
func ManagedUserRoleName(userID int64) string {
    return fmt.Sprintf("managed:users:%d:permissions", userID)
}

func ManagedTeamRoleName(teamID int64) string {
    return fmt.Sprintf("managed:teams:%d:permissions", teamID)
}

func ManagedBuiltInRoleName(builtInRole string) string {
    return fmt.Sprintf("managed:builtins:%s:permissions", strings.ToLower(builtInRole))
}
```

---

## 8. 미들웨어 통합

### Middleware 함수

```go
// pkg/services/accesscontrol/middleware.go
func Middleware(ac AccessControl) func(Evaluator) web.Handler {
    return func(evaluator Evaluator) web.Handler {
        return func(c *contextmodel.ReqContext) {
            // 1. 익명 접근 + forceLogin 처리
            if c.AllowAnonymous {
                forceLogin, _ := strconv.ParseBool(c.Req.URL.Query().Get("forceLogin"))
                orgID, err := strconv.ParseInt(c.Req.URL.Query().Get("orgId"), 10, 64)
                if err == nil && orgID > 0 && orgID != c.GetOrgID() {
                    forceLogin = true
                }
                if !c.IsSignedIn && forceLogin {
                    unauthorized(c)
                    return
                }
            }

            // 2. 토큰 에러 처리
            if c.LookupTokenErr != nil {
                var revokedErr *usertoken.TokenRevokedError
                if errors.As(c.LookupTokenErr, &revokedErr) {
                    tokenRevoked(c, revokedErr)
                    return
                }
                unauthorized(c)
                return
            }

            // 3. 인가 수행
            authorize(c, ac, c.SignedInUser, evaluator)
        }
    }
}
```

### authorize 함수

```go
func authorize(c *contextmodel.ReqContext, ac AccessControl, user identity.Requester, evaluator Evaluator) {
    // 1. 스코프에 URL 파라미터 주입
    injected, err := evaluator.MutateScopes(ctx, scopeInjector(scopeParams{
        OrgID:     user.GetOrgID(),
        URLParams: web.Params(c.Req),
    }))

    // 2. 권한 평가
    hasAccess, err := ac.Evaluate(ctx, user, injected)

    // 3. 거부 시 처리
    if !hasAccess || err != nil {
        deny(c, injected, err)
    }
}
```

### deny 함수 (거부 응답)

```go
func deny(c *contextmodel.ReqContext, evaluator Evaluator, err error) {
    id := newID()  // "ACE" + 10자리 숫자

    if !c.IsApiRequest() {
        // 웹 브라우저: 리다이렉트
        writeRedirectCookie(c)
        c.Redirect(setting.AppSubUrl + "/")
        return
    }

    // API: 403 JSON 응답
    c.JSON(http.StatusForbidden, map[string]string{
        "title":         "Access denied",
        "message":       fmt.Sprintf("You'll need additional permissions to perform this action. Permissions needed: %s", message),
        "accessErrorId": id,
    })
}
```

### 레거시 미들웨어 상수

```go
var ReqSignedIn = func(c *contextmodel.ReqContext) bool {
    return c.IsSignedIn
}

var ReqGrafanaAdmin = func(c *contextmodel.ReqContext) bool {
    return c.GetIsGrafanaAdmin()
}

func ReqHasRole(role org.RoleType) func(c *contextmodel.ReqContext) bool {
    return func(c *contextmodel.ReqContext) bool { return c.HasRole(role) }
}
```

### 라우트 등록 패턴

API 라우트에서 미들웨어를 사용하는 전형적인 패턴:

```go
// authorize = ac.Middleware(hs.AccessControl)
// authorizeInOrg = ac.AuthorizeInOrgMiddleware()

// 단순 권한 체크
r.Get("/api/teams", authorize(
    ac.EvalPermission(ac.ActionTeamsRead),
), hs.SearchTeams)

// 복합 권한 체크
r.Get("/api/teams/:teamId", authorize(
    ac.EvalPermission(ac.ActionTeamsRead, ac.ScopeTeamsID),
), hs.GetTeamByID)

// 레거시 역할 체크
r.Get("/api/admin/stats", ReqGrafanaAdmin, hs.AdminGetStats)
```

---

## 9. AuthorizeInOrg 미들웨어

`AuthorizeInOrgMiddleware`는 **요청 대상 조직에서의 권한**을 평가한다. 사용자의 현재 조직이 아닌 다른 조직의 리소스에 접근할 때 사용된다.

```go
func AuthorizeInOrgMiddleware(ac AccessControl, authnService authn.Service) func(OrgIDGetter, Evaluator) web.Handler {
    return func(getTargetOrg OrgIDGetter, evaluator Evaluator) web.Handler {
        return func(c *contextmodel.ReqContext) {
            // 1. 대상 조직 ID 결정
            targetOrgID, err := getTargetOrg(c)

            // 2. 대상 조직에서 사용자 Identity 해석
            var orgUser identity.Requester = c.SignedInUser
            if targetOrgID != c.GetOrgID() {
                orgUser, err = authnService.ResolveIdentity(c.Req.Context(), targetOrgID, c.GetID())
                if err == nil && orgUser.GetOrgID() == NoOrgID {
                    // 대상 조직 멤버가 아니면 글로벌 권한만 사용
                    orgUser, _ = authnService.ResolveIdentity(c.Req.Context(), GlobalOrgID, c.GetID())
                }
            }

            // 3. 해당 조직에서 인가 수행
            authorize(c, ac, orgUser, evaluator)

            // 4. 권한 캐시
            c.Permissions[orgUser.GetOrgID()] = orgUser.GetPermissions()
        }
    }
}
```

### OrgIDGetter 유형

| 함수 | 용도 |
|------|------|
| `UseOrgFromContextParams` | URL 파라미터 `:orgId` |
| `UseGlobalOrg` | 항상 GlobalOrgID (0) |
| `UseGlobalOrSingleOrg(cfg)` | Single Org 설정이면 현재 조직, 아니면 글로벌 |
| `UseOrgFromRequestData` | 요청 본문의 `orgId` 필드 |
| `UseGlobalOrgFromRequestData(cfg)` | 요청 본문의 `global` 플래그 |
| `UseGlobalOrgFromRequestParams(cfg)` | 쿼리 파라미터의 `global` 플래그 |

---

## 10. 스코프 리졸버

스코프 리졸버는 **스코프 변환**을 수행한다. 예를 들어, 대시보드 이름을 UID로, 폴더 ID를 UID로 변환한다.

### ScopeAttributeResolver 인터페이스

```go
// pkg/services/accesscontrol/resolvers.go
type ScopeAttributeResolver interface {
    Resolve(ctx context.Context, orgID int64, scope string) ([]string, error)
}

// 함수 어댑터
type ScopeAttributeResolverFunc func(ctx context.Context, orgID int64, scope string) ([]string, error)
```

### Resolvers 구조체

```go
type Resolvers struct {
    log                log.Logger
    cache              *localcache.CacheService  // 30초 TTL, 2분 정리 간격
    attributeResolvers map[string]ScopeAttributeResolver
}
```

### 리졸버 등록

```go
func (s *Resolvers) AddScopeAttributeResolver(prefix string, resolver ScopeAttributeResolver)

// 예: dashboards:name: 접두사로 요청이 오면 UID로 변환
// "dashboards:name:My Dashboard" → ["dashboards:uid:abc123"]
```

### 리졸버 캐싱

```go
func (s *Resolvers) GetScopeAttributeMutator(orgID int64) ScopeAttributeMutator {
    return func(ctx context.Context, scope string) ([]string, error) {
        key := getScopeCacheKey(orgID, scope)  // "scope-orgID"

        // 캐시 확인
        if cachedScope, ok := s.cache.Get(key); ok {
            return cachedScope.([]string), nil
        }

        // 접두사로 리졸버 조회
        prefix := ScopePrefix(scope)
        if resolver, ok := s.attributeResolvers[prefix]; ok {
            scopes, err := resolver.Resolve(ctx, orgID, scope)
            s.cache.Set(key, scopes, ttl)  // 30초 캐시
            return scopes, nil
        }

        return nil, ErrResolverNotFound
    }
}
```

### ActionResolver (액션 셋 확장)

```go
type ActionResolver interface {
    ExpandActionSets(permissions []Permission) []Permission
    ExpandActionSetsWithFilter(permissions []Permission, actionMatcher func(action string) bool) []Permission
    ResolveAction(action string) []string
    ResolveActionPrefix(prefix string) []string
}
```

Action Set은 여러 액션을 하나로 묶는 메커니즘이다. 예를 들어 `dashboards:view` 액션 셋은 `dashboards:read`, `dashboards.insights:read` 등 여러 개별 액션으로 확장된다.

---

## 11. 권한 평가 흐름 상세

### 전체 흐름 다이어그램

```
1. 미들웨어 진입
   |
   v
2. Evaluator에 URL 파라미터 주입 (MutateScopes)
   |
   EvalPermission("teams:read", "teams:id:{{ .URLParams.teamId }}")
   → EvalPermission("teams:read", "teams:id:42")
   |
   v
3. AccessControl.Evaluate(ctx, user, evaluator)
   |
   v
4. 사용자 권한 로드 (Identity.Permissions)
   |
   권한 맵: map[string][]string {
       "teams:read":  ["teams:id:*"],
       "teams:write": ["teams:id:42"],
   }
   |
   v
5. 스코프 리졸버 실행 (필요 시)
   |
   "dashboards:name:My Dashboard" → ["dashboards:uid:abc", "folders:uid:general"]
   |
   v
6. Evaluator.Evaluate(permissions)
   |
   permissionEvaluator: action="teams:read", scopes=["teams:id:42"]
   |
   +---> permissions["teams:read"] = ["teams:id:*"]
   +---> match("teams:id:*", "teams:id:42")
   +---> prefix="teams:id:", last='*'
   +---> HasPrefix("teams:id:42", "teams:id:") → true
   |
   v
7. 결과: true (허용)
```

### GroupScopesByAction (최적화된 권한 그룹화)

```go
func GroupScopesByActionContext(ctx context.Context, permissions []Permission) map[string][]string {
    // 1단계: 액션에 인덱스 할당 (uint16으로 메모리 절약)
    actionIndex := make(map[string]uint16, 256)
    indices := make([]uint16, len(permissions))

    for i := range permissions {
        action := permissions[i].Action
        if idx, ok := actionIndex[action]; ok {
            indices[i] = idx
        } else {
            idx := uint16(len(actionIndex))
            actionIndex[action] = idx
            indices[i] = idx
        }
    }

    // 2단계: 액션별 스코프 카운트
    actionCounts := make([]int, numActions)
    for _, idx := range indices {
        actionCounts[idx]++
    }

    // 3단계: 단일 연속 배킹 배열 할당 (GC 부담 감소)
    backingArray := make([]string, len(permissions))

    // 4단계: 배킹 배열 분할하여 슬라이스 생성
    scopes := make([][]string, numActions)
    offset := 0
    for i, count := range actionCounts {
        scopes[i] = backingArray[offset : offset : offset+count]
        offset += count
    }

    // 5단계: 캐시된 인덱스로 스코프 추가 (맵 조회 없음!)
    for i := range permissions {
        idx := indices[i]
        scopes[idx] = append(scopes[idx], permissions[i].Scope)
    }

    // 6단계: 결과 맵 생성
    m := make(map[string][]string, numActions)
    for action, idx := range actionIndex {
        m[action] = scopes[idx]
    }
    return m
}
```

이 함수의 최적화 포인트:
- **uint16 인덱스**: 8바이트 대신 2바이트로 메모리 절약
- **단일 배킹 배열**: 여러 작은 할당 대신 하나의 큰 할당으로 GC 부담 감소
- **인덱스 캐싱**: 두 번째 패스에서 맵 조회 없이 슬라이스 인덱스 사용

### Reduce (권한 축소)

```go
func Reduce(ps []Permission) map[string][]string {
    // 1. 스코프 없는 권한, 와일드카드, 특정 스코프 분류
    // 2. 와일드카드 축소 (하위 포함하는 와일드카드 제거)
    //    예: "dashboards:*"가 있으면 "dashboards:uid:*" 불필요
    // 3. 와일드카드에 포함되는 특정 스코프 제거
    //    예: "dashboards:*"가 있으면 "dashboards:uid:abc" 불필요
}
```

---

## 12. 멀티테넌시와 조직 모델

### Organization 모델

Grafana는 **멀티테넌시**를 조직(Organization) 모델로 구현한다:

```
Grafana 인스턴스
    |
    +---> Organization 1 (Main Org.)
    |     |
    |     +---> Users (각각 Admin/Editor/Viewer 역할)
    |     +---> Dashboards
    |     +---> Datasources
    |     +---> Teams
    |
    +---> Organization 2
    |     |
    |     +---> Users (동일 사용자가 다른 역할)
    |     +---> Dashboards (독립적)
    |     +---> Datasources (독립적)
    |
    +---> Global (Org 0)
          |
          +---> Grafana Admin 권한
          +---> 서버 설정
```

### 사용자-조직 관계

한 사용자가 여러 조직에 속할 수 있으며, 각 조직에서 다른 역할을 가질 수 있다:

```
Identity.OrgRoles = map[int64]org.RoleType{
    1: org.RoleAdmin,   // 조직 1에서 Admin
    2: org.RoleViewer,  // 조직 2에서 Viewer
    3: org.RoleEditor,  // 조직 3에서 Editor
}
```

### 조직 격리

권한은 조직별로 격리된다:

```go
// Identity.Permissions 구조
map[int64]map[string][]string{
    0: {  // Global (모든 조직에서 유효)
        "users:read": ["global.users:*"],
    },
    1: {  // Organization 1
        "dashboards:read": ["dashboards:uid:abc"],
        "datasources:read": ["datasources:*"],
    },
    2: {  // Organization 2
        "dashboards:read": ["dashboards:*"],
    },
}
```

```go
func (i *Identity) GetPermissions() map[string][]string {
    return i.Permissions[i.GetOrgID()]  // 현재 활성 조직의 권한만 반환
}

func (i *Identity) GetGlobalPermissions() map[string][]string {
    return i.Permissions[GlobalOrgID]  // 글로벌 권한 반환
}
```

### HasGlobalAccess

글로벌 스코프에서 권한을 확인하는 헬퍼:

```go
func HasGlobalAccess(ac AccessControl, authnService authn.Service, c *contextmodel.ReqContext) func(evaluator Evaluator) bool {
    return func(evaluator Evaluator) bool {
        var targetOrgID int64 = GlobalOrgID
        orgUser, err := authnService.ResolveIdentity(c.Req.Context(), targetOrgID, c.GetID())
        if err != nil {
            return false
        }
        hasAccess, err := ac.Evaluate(c.Req.Context(), orgUser, evaluator)
        c.Permissions[orgUser.GetOrgID()] = orgUser.GetPermissions()
        return hasAccess
    }
}
```

---

## 13. 레거시 역할 기반 접근 제어

RBAC 이전의 레거시 접근 제어는 단순한 역할 비교로 동작한다.

### ReqSignedIn

```go
var ReqSignedIn = func(c *contextmodel.ReqContext) bool {
    return c.IsSignedIn
}
```

### ReqGrafanaAdmin

```go
var ReqGrafanaAdmin = func(c *contextmodel.ReqContext) bool {
    return c.GetIsGrafanaAdmin()
}
```

### ReqHasRole

```go
func ReqHasRole(role org.RoleType) func(c *contextmodel.ReqContext) bool {
    return func(c *contextmodel.ReqContext) bool { return c.HasRole(role) }
}
```

`HasRole`은 역할 상속을 포함한다. `Admin` 역할은 `Editor`와 `Viewer` 권한을 포함한다.

### HasAccess

```go
func HasAccess(ac AccessControl, c *contextmodel.ReqContext) func(evaluator Evaluator) bool {
    return func(evaluator Evaluator) bool {
        hasAccess, err := ac.Evaluate(c.Req.Context(), c.SignedInUser, evaluator)
        if err != nil {
            c.Logger.Error("Error from access control system", "error", err)
            return false
        }
        return hasAccess
    }
}
```

---

## 14. LegacyAccessClient와 K8s 통합

`pkg/services/accesscontrol/authorizer.go`의 `LegacyAccessClient`는 Kubernetes API 스타일의 인가 요청을 Grafana RBAC로 변환한다.

### ResourceAuthorizerOptions

```go
type ResourceAuthorizerOptions struct {
    Resource string              // 리소스 이름 (복수형)
    Unchecked map[string]bool    // 체크 건너뛸 동사
    Attr string                  // 스코프 속성 ('id' 또는 'uid')
    Mapping map[string]string    // K8s 동사 → RBAC 액션 매핑
    Resolver ResourceResolver    // 리소스 이름 → 스코프 변환
}
```

### 기본 동사 매핑

```go
defaultMapping := func(r string) map[string]string {
    return map[string]string{
        "get":              fmt.Sprintf("%s:read", r),
        "list":             fmt.Sprintf("%s:read", r),
        "watch":            fmt.Sprintf("%s:read", r),
        "create":           fmt.Sprintf("%s:create", r),
        "update":           fmt.Sprintf("%s:write", r),
        "patch":            fmt.Sprintf("%s:write", r),
        "delete":           fmt.Sprintf("%s:delete", r),
        "deletecollection": fmt.Sprintf("%s:delete", r),
        "getpermissions":   fmt.Sprintf("%s.permissions:read", r),
        "setpermissions":   fmt.Sprintf("%s.permissions:write", r),
    }
}
```

### Check 메서드

```go
func (c *LegacyAccessClient) Check(ctx context.Context, id claims.AuthInfo, req claims.CheckRequest, folder string) (claims.CheckResponse, error) {
    ident, ok := id.(identity.Requester)

    // 1. 리소스 옵션 조회
    opts, ok := c.opts[req.Resource]
    if !ok {
        // 옵션 없으면 Grafana Admin만 허용
        if ident.GetIsGrafanaAdmin() {
            return claims.CheckResponse{Allowed: true}, nil
        }
        return claims.CheckResponse{}, nil
    }

    // 2. 체크 건너뛰기 확인
    if opts.Unchecked[req.Verb] {
        return claims.CheckResponse{Allowed: true}, nil
    }

    // 3. 동사 → 액션 매핑
    action := opts.Mapping[req.Verb]

    // 4. Evaluator 생성
    if req.Name != "" {
        if opts.Resolver != nil {
            scopes, _ := opts.Resolver.Resolve(ctx, ns, req.Name)
            eval = EvalPermission(action, scopes...)
        } else {
            eval = EvalPermission(action, fmt.Sprintf("%s:%s:%s", opts.Resource, opts.Attr, req.Name))
        }
    } else if req.Verb == "list" || req.Verb == "create" {
        eval = EvalPermission(action)  // 스코프 없이 액션만 확인
    }

    // 5. 평가
    allowed, _ := c.ac.Evaluate(ctx, ident, eval)
    return claims.CheckResponse{Allowed: allowed}, nil
}
```

### Compile 메서드 (리스트 필터링)

```go
func (c *LegacyAccessClient) Compile(ctx context.Context, id claims.AuthInfo, req claims.ListRequest) (claims.ItemChecker, claims.Zookie, error) {
    check := Checker(ident, action)
    return func(name, _ string) bool {
        return check(fmt.Sprintf("%s:%s:%s", opts.Resource, opts.Attr, name))
    }, claims.NoopZookie{}, nil
}
```

이 메서드는 리스트 요청에서 **개별 항목 필터링 함수**를 반환한다. Kubernetes API 서버가 리스트 결과를 필터링할 때 사용한다.

---

## 15. Action과 Scope 전체 목록

### 사용자 관련 Action

| Action | Scope | 설명 |
|--------|-------|------|
| `users:read` | `global.users:*` | 사용자 정보 읽기 |
| `users:write` | `global.users:*` | 사용자 정보 수정 |
| `users:create` | - | 사용자 생성 |
| `users:delete` | `global.users:*` | 사용자 삭제 |
| `users:enable` | `global.users:*` | 사용자 활성화 |
| `users:disable` | `global.users:*` | 사용자 비활성화 |
| `users:logout` | `global.users:*` | 강제 로그아웃 |
| `users.password:write` | `global.users:*` | 비밀번호 변경 |
| `users.authtoken:read` | `global.users:*` | 인증 토큰 조회 |
| `users.authtoken:write` | `global.users:*` | 인증 토큰 수정 |
| `users.permissions:write` | `global.users:*` | 권한 변경 |
| `users.permissions:read` | `users:*` | 권한 조회 |
| `users.quotas:read` | `global.users:*` | 쿼터 조회 |
| `users.quotas:write` | `global.users:*` | 쿼터 수정 |

### 조직 관련 Action

| Action | 설명 |
|--------|------|
| `orgs:read` | 조직 읽기 |
| `orgs:write` | 조직 수정 |
| `orgs:create` | 조직 생성 |
| `orgs:delete` | 조직 삭제 |
| `orgs.preferences:read` | 조직 설정 읽기 |
| `orgs.preferences:write` | 조직 설정 수정 |
| `orgs.quotas:read` | 조직 쿼터 읽기 |
| `orgs.quotas:write` | 조직 쿼터 수정 |
| `org.users:read` | 조직 사용자 읽기 |
| `org.users:add` | 조직 사용자 추가 |
| `org.users:write` | 조직 사용자 역할 변경 |
| `org.users:remove` | 조직 사용자 제거 |

### 팀 관련 Action

| Action | 설명 |
|--------|------|
| `teams:create` | 팀 생성 |
| `teams:delete` | 팀 삭제 |
| `teams:read` | 팀 읽기 |
| `teams:write` | 팀 수정 |
| `teams.permissions:read` | 팀 권한 읽기 |
| `teams.permissions:write` | 팀 권한 수정 |

### 알림(Alerting) 관련 Action

| Action | 설명 |
|--------|------|
| `alert.rules:create` | 알림 규칙 생성 |
| `alert.rules:read` | 알림 규칙 읽기 |
| `alert.rules:write` | 알림 규칙 수정 |
| `alert.rules:delete` | 알림 규칙 삭제 |
| `alert.instances:create` | 알림 인스턴스 생성 |
| `alert.instances:read` | 알림 인스턴스 읽기 |
| `alert.instances:write` | 알림 인스턴스 수정 |
| `alert.silences:read` | 음소거 읽기 |
| `alert.silences:create` | 음소거 생성 |
| `alert.silences:write` | 음소거 수정 |
| `alert.notifications.receivers:read` | 수신자 읽기 |
| `alert.notifications.receivers:create` | 수신자 생성 |
| `alert.notifications.receivers:write` | 수신자 수정 |
| `alert.notifications.receivers:delete` | 수신자 삭제 |
| `alert.notifications.routes:read` | 라우팅 정책 읽기 |
| `alert.notifications.routes:write` | 라우팅 정책 수정 |

### Scope 패턴

| Scope 패턴 | 설명 |
|-----------|------|
| `*` | 전역 와일드카드 (모든 리소스) |
| `dashboards:*` | 모든 대시보드 |
| `dashboards:uid:*` | 모든 대시보드 (UID 기준) |
| `dashboards:uid:abc123` | 특정 대시보드 |
| `datasources:*` | 모든 데이터소스 |
| `datasources:id:42` | 특정 데이터소스 (ID 기준) |
| `teams:*` | 모든 팀 |
| `teams:id:7` | 특정 팀 |
| `users:*` | 조직 내 모든 사용자 |
| `users:id:1` | 특정 사용자 |
| `global.users:*` | 전체 인스턴스 모든 사용자 |
| `folders:uid:general` | 특정 폴더 |
| `settings:*` | 모든 설정 |
| `settings:auth.saml:*` | SAML 설정 |
| `annotations:*` | 모든 주석 |
| `annotations:type:dashboard` | 대시보드 주석 |

---

## 16. 권한 캐싱과 최적화

### Store 인터페이스

```go
type Store interface {
    GetUserPermissions(ctx context.Context, query GetUserPermissionsQuery) ([]Permission, error)
    GetBasicRolesPermissions(ctx context.Context, query GetUserPermissionsQuery) ([]Permission, error)
    GetTeamsPermissions(ctx context.Context, query GetUserPermissionsQuery) (map[int64][]Permission, error)
    SearchUsersPermissions(ctx context.Context, orgID int64, options SearchOptions) (map[int64][]Permission, error)
    GetUsersBasicRoles(ctx context.Context, userFilter []int64, orgID int64) (map[int64][]string, error)
    DeleteUserPermissions(ctx context.Context, orgID, userID int64) error
    DeleteTeamPermissions(ctx context.Context, orgID, teamID int64) error
}
```

### GetUserPermissionsQuery

```go
type GetUserPermissionsQuery struct {
    OrgID        int64
    UserID       int64
    Roles        []string     // 기본 역할 목록
    TeamIDs      []int64      // 소속 팀 ID 목록
    RolePrefixes []string     // 역할 접두사 필터
    ExcludeRedundantManagedPermissions bool  // 중복 관리 권한 제외
}
```

`ExcludeRedundantManagedPermissions`가 `true`이면, Action Set이 활성화된 경우 개별 대시보드/폴더 액션 권한을 SQL 쿼리에서 제외한다. Action Set이 메모리에서 개별 액션으로 확장되기 때문에, SQL에서 미리 로드하면 중복이 된다. 이 옵션은 **대규모 인스턴스에서 로드되는 행 수를 크게 줄인다**.

### 스코프 리졸버 캐시

```go
const (
    ttl           = 30 * time.Second     // 캐시 TTL
    cleanInterval = 2 * time.Minute      // 정리 간격
)
```

스코프 리졸버 결과는 30초 동안 캐시된다. `InvalidateResolverCache`로 특정 스코프의 캐시를 수동으로 무효화할 수 있다.

### 권한 캐시 무효화

```go
// Service 인터페이스
ClearUserPermissionCache(user identity.Requester)
```

사용자 권한이 변경될 때 호출하여 캐시를 갱신한다.

### SearchOptions

```go
type SearchOptions struct {
    ActionPrefix string    // 액션 접두사 필터 (예: "dashboards:")
    Action       string    // 정확한 액션 매칭
    ActionSets   []string  // 확장할 액션 셋
    Scope        string    // 스코프 필터
    UserID       int64     // 사용자 ID 필터
    wildcards    Wildcards // 계산된 와일드카드 (private)
    RolePrefixes []string  // 역할 접두사 필터
}
```

---

## 17. 권한 평가 예제 시나리오

### 시나리오 1: 대시보드 읽기

```
요청: GET /api/dashboards/uid/my-dashboard
사용자 권한: {
    "dashboards:read": ["dashboards:uid:my-dashboard", "folders:uid:general"]
}

Evaluator: EvalPermission("dashboards:read", "dashboards:uid:my-dashboard")

평가:
  1. permissions["dashboards:read"] = ["dashboards:uid:my-dashboard", "folders:uid:general"]
  2. match("dashboards:uid:my-dashboard", "dashboards:uid:my-dashboard") → true (정확한 매칭)
  3. 결과: 허용
```

### 시나리오 2: 와일드카드 매칭

```
요청: GET /api/datasources/42
사용자 권한: {
    "datasources:read": ["datasources:*"]
}

Evaluator: EvalPermission("datasources:read", "datasources:id:42")

평가:
  1. permissions["datasources:read"] = ["datasources:*"]
  2. match("datasources:*", "datasources:id:42")
     → prefix="datasources:", last='*'
     → HasPrefix("datasources:id:42", "datasources:") → true
  3. 결과: 허용
```

### 시나리오 3: 복합 조건 (Teams 페이지)

```
사용자 권한: {
    "teams:read": ["teams:*"],
    "teams.permissions:read": ["teams:*"]
}

Evaluator: TeamsAccessEvaluator = EvalAny(
    EvalPermission(ActionTeamsCreate),
    EvalAll(
        EvalPermission(ActionTeamsRead),
        EvalAny(
            EvalPermission(ActionTeamsWrite),
            EvalPermission(ActionTeamsPermissionsWrite),
            EvalPermission(ActionTeamsPermissionsRead),
        ),
    ),
)

평가:
  1. EvalAny 시작
  2. EvalPermission("teams:create") → permissions에 없음 → false
  3. EvalAll 시작
     a. EvalPermission("teams:read") → permissions["teams:read"]=["teams:*"] → true
     b. EvalAny 시작
        - EvalPermission("teams:write") → 없음 → false
        - EvalPermission("teams.permissions:write") → 없음 → false
        - EvalPermission("teams.permissions:read") → 있음 → true
     c. EvalAny 결과: true
  4. EvalAll 결과: true (a AND b 모두 true)
  5. EvalAny 결과: true
  6. 최종 결과: 허용
```

### 시나리오 4: 권한 거부

```
요청: DELETE /api/teams/5
사용자 권한: {
    "teams:read": ["teams:*"]
}

Evaluator: EvalPermission("teams:delete", "teams:id:5")

평가:
  1. permissions["teams:delete"] → 존재하지 않음
  2. 결과: 거부

응답: 403 Forbidden
{
    "title": "Access denied",
    "message": "You'll need additional permissions to perform this action. Permissions needed: teams:delete",
    "accessErrorId": "ACE1234567890"
}
```

---

## 18. 설계 원칙과 트레이드오프

### 설계 원칙

| 원칙 | 구현 |
|------|------|
| **세분화된 권한** | Action + Scope로 리소스 수준 접근 제어 |
| **역할 기반** | Basic → Fixed → Managed 3단계 역할 |
| **조직 격리** | 조직별 독립 권한 공간 |
| **하위 호환** | 레거시 역할과 RBAC 공존 |
| **확장 가능** | Fixed Role 동적 등록, 스코프 리졸버 |
| **성능 최적화** | 캐싱, 배칭 배열, 중복 제거 |

### 트레이드오프

1. **복잡성 vs 세분화**: RBAC은 단순한 역할 체크보다 복잡하지만, 리소스 수준의 정밀한 접근 제어가 가능하다.

2. **메모리 vs 속도**: 권한 맵을 메모리에 유지하여 매 요청 DB 조회를 피하지만, 대규모 인스턴스에서 메모리 사용량이 증가한다.

3. **캐시 일관성**: 스코프 리졸버의 30초 TTL 캐시는 성능은 좋지만, 권한 변경 후 최대 30초의 지연이 발생할 수 있다.

4. **K8s 통합**: LegacyAccessClient가 K8s 동사를 Grafana 액션으로 매핑하는 브릿지 역할을 하지만, 두 시스템의 의미론적 차이로 완벽한 매핑이 어려울 수 있다.

### 핵심 소스 파일 요약

| 파일 | 역할 |
|------|------|
| `pkg/services/accesscontrol/accesscontrol.go` | AccessControl/Service 인터페이스 |
| `pkg/services/accesscontrol/evaluator.go` | EvalPermission, EvalAll, EvalAny |
| `pkg/services/accesscontrol/models.go` | Permission, Role, ResourcePermission |
| `pkg/services/accesscontrol/middleware.go` | Middleware, AuthorizeInOrgMiddleware |
| `pkg/services/accesscontrol/scope.go` | Scope 빌더, ScopeProvider, 와일드카드 |
| `pkg/services/accesscontrol/resolvers.go` | ScopeAttributeResolver, 캐시 |
| `pkg/services/accesscontrol/roles.go` | Fixed Role 정의, Basic Role |
| `pkg/services/accesscontrol/authorizer.go` | LegacyAccessClient (K8s 통합) |
| `pkg/services/accesscontrol/checker.go` | 리소스 필터링 Checker |
| `pkg/services/accesscontrol/filter.go` | SQL 권한 필터 생성 |
