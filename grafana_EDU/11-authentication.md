# 11. Grafana 인증 시스템 심화

## 목차

1. [인증 아키텍처 개요](#1-인증-아키텍처-개요)
2. [Identity 모델](#2-identity-모델)
3. [ClientParams와 동기화 힌트](#3-clientparams와-동기화-힌트)
4. [인증 클라이언트 인터페이스 체계](#4-인증-클라이언트-인터페이스-체계)
5. [클라이언트 우선순위와 요청 라우팅](#5-클라이언트-우선순위와-요청-라우팅)
6. [Basic Auth 클라이언트](#6-basic-auth-클라이언트)
7. [API Key 클라이언트](#7-api-key-클라이언트)
8. [JWT 클라이언트](#8-jwt-클라이언트)
9. [OAuth 클라이언트](#9-oauth-클라이언트)
10. [LDAP 클라이언트](#10-ldap-클라이언트)
11. [Session 클라이언트](#11-session-클라이언트)
12. [Auth Proxy 클라이언트](#12-auth-proxy-클라이언트)
13. [Form 클라이언트](#13-form-클라이언트)
14. [Render 클라이언트](#14-render-클라이언트)
15. [세션 관리와 토큰 로테이션](#15-세션-관리와-토큰-로테이션)
16. [OAuth PKCE와 State 검증](#16-oauth-pkce와-state-검증)
17. [인증 설정](#17-인증-설정)
18. [인증 미들웨어와 후크 시스템](#18-인증-미들웨어와-후크-시스템)

---

## 1. 인증 아키텍처 개요

Grafana의 인증 시스템은 **Pluggable Client 기반 아키텍처**로 설계되어 있다. 모든 인증 방식이 동일한 `Client` 인터페이스를 구현하며, 요청이 들어오면 등록된 클라이언트들이 우선순위에 따라 순서대로 시도된다.

### 핵심 소스 위치

```
pkg/services/authn/
  authn.go          -- Service/Client 인터페이스, Request/Redirect 모델
  identity.go       -- Identity 구조체 (인증된 엔티티 표현)
  error.go          -- 인증 관련 에러 정의
  clients/
    render.go       -- Render Key 인증 (Priority: 10)
    jwt.go          -- JWT/OIDC 인증 (Priority: 20)
    api_key.go      -- API Key 인증 (Priority: 30)
    basic.go        -- HTTP Basic 인증 (Priority: 40)
    proxy.go        -- Auth Proxy 인증 (Priority: 50)
    session.go      -- 세션 쿠키 인증 (Priority: 60)
    ext_jwt.go      -- Extended JWT 인증
    oauth.go        -- OAuth 인증 (Redirect-based)
    ldap.go         -- LDAP 인증
    form.go         -- 로그인 폼 인증
    password.go     -- 비밀번호 인증
    constants.go    -- 공통 상수
```

### 인증 서비스 인터페이스

`pkg/services/authn/authn.go`에 정의된 `Service` 인터페이스가 전체 인증 시스템의 진입점이다:

```go
type Service interface {
    Authenticator
    RegisterPostAuthHook(hook PostAuthHookFn, priority uint)
    Login(ctx context.Context, client string, r *Request) (*Identity, error)
    RegisterPostLoginHook(hook PostLoginHookFn, priority uint)
    RedirectURL(ctx context.Context, client string, r *Request) (*Redirect, error)
    Logout(ctx context.Context, user identity.Requester, sessionToken *usertoken.UserToken) (*Redirect, error)
    RegisterPreLogoutHook(hook PreLogoutHookFn, priority uint)
    ResolveIdentity(ctx context.Context, orgID int64, typedID string) (*Identity, error)
    RegisterClient(c Client)
    IsClientEnabled(client string) bool
    GetClientConfig(client string) (SSOClientConfig, bool)
}
```

핵심 메서드의 역할:

| 메서드 | 역할 |
|--------|------|
| `Authenticate` | 요청을 인증하고 Identity 반환 |
| `Login` | 인증 후 세션 생성 |
| `RedirectURL` | OAuth 등 리다이렉트 URL 생성 |
| `Logout` | 세션 해제 + 클라이언트별 추가 로직 |
| `RegisterClient` | 새 인증 클라이언트 등록 |
| `ResolveIdentity` | orgID와 typedID로 Identity 해석 |

### 전체 인증 흐름

```
HTTP 요청
    |
    v
+-------------------+
| authn.Service     |
| .Authenticate()   |
+-------------------+
    |
    | 등록된 ContextAwareClient를 Priority 순으로 정렬
    |
    v
+-----------+    +-----------+    +-----------+    +-----------+
| Render    |    | JWT       |    | APIKey    |    | Basic     |
| P=10      |--->| P=20      |--->| P=30      |--->| P=40      |
| .Test()   |    | .Test()   |    | .Test()   |    | .Test()   |
+-----------+    +-----------+    +-----------+    +-----------+
    |                                                    |
    | Test()=true 인 첫 번째 클라이언트의                  |
    | .Authenticate() 호출                               v
    |                                           +-----------+
    |                                           | Proxy     |
    +------------------------------------------>| P=50      |
    |                                           +-----------+
    v                                                |
+-----------+                                   +-----------+
| Identity  |<----------------------------------| Session   |
| 반환      |                                   | P=60      |
+-----------+                                   +-----------+
    |
    v
+-------------------+
| PostAuthHook 실행 |
| (SyncUser 등)     |
+-------------------+
    |
    v
+-------------------+
| 미들웨어 →        |
| 인가(RBAC) 단계   |
+-------------------+
```

---

## 2. Identity 모델

`pkg/services/authn/identity.go`에 정의된 `Identity` 구조체는 인증된 엔티티의 모든 정보를 담는 핵심 데이터 모델이다.

### Identity 구조체 필드

```go
type Identity struct {
    ID              string                        // DB 내 고유 식별자
    UID             string                        // Grafana DB의 UID (없을 수 있음)
    Type            claims.IdentityType           // user, api-key, service-account, anonymous 등
    OrgID           int64                         // 활성 조직 ID
    OrgName         string                        // 활성 조직 이름
    OrgRoles        map[int64]org.RoleType        // 조직별 역할 매핑
    Login           string                        // 로그인 이름 (유일해야 함)
    Name            string                        // 표시 이름
    Email           string                        // 이메일 (유일해야 함)
    EmailVerified   bool                          // 이메일 인증 여부
    IsGrafanaAdmin  *bool                         // Grafana 전역 관리자 여부
    AuthenticatedBy string                        // 인증에 사용된 모듈 이름
    AuthID          string                        // 외부 시스템의 고유 식별자
    Namespace       string                        // 네임스페이스
    IsDisabled      bool                          // 비활성화 여부
    HelpFlags1      user.HelpFlags1               // 도움말 플래그
    LastSeenAt      time.Time                     // 마지막 접근 시간
    Teams           []int64                       // 소속 팀 ID 목록
    Groups          []string                      // IdP 그룹 목록
    OAuthToken      *oauth2.Token                 // OAuth 토큰
    SAMLSession     *login.SAMLSession            // SAML 세션 정보
    SessionToken    *usertoken.UserToken          // 세션 토큰
    ClientParams    ClientParams                  // 인증 클라이언트가 설정한 동기화 힌트
    Permissions     map[int64]map[string][]string  // 조직별 권한 맵
    IDToken         string                        // 플러그인/외부 서비스용 서명된 토큰
    ExternalUID     string                        // 외부 시스템 UID
}
```

### IdentityType 종류

| Type | 설명 |
|------|------|
| `TypeUser` | 일반 사용자 |
| `TypeAPIKey` | API 키 (레거시) |
| `TypeServiceAccount` | 서비스 계정 |
| `TypeAnonymous` | 익명 사용자 |
| `TypeRenderService` | 렌더링 서비스 |

### 주요 메서드

`Identity`는 `identity.Requester` 인터페이스를 구현한다:

```go
var _ identity.Requester = (*Identity)(nil)
```

핵심 메서드들:

```go
func (i *Identity) GetOrgRole() org.RoleType     // 현재 조직에서의 역할
func (i *Identity) HasRole(role org.RoleType) bool // 역할 포함 여부 (GrafanaAdmin이면 항상 true)
func (i *Identity) GetPermissions() map[string][]string // 현재 조직의 권한
func (i *Identity) HasUniqueId() bool              // User/APIKey/ServiceAccount만 true
func (i *Identity) SignedInUser() *user.SignedInUser // 레거시 모델 변환
```

`HasRole` 메서드는 **Grafana Admin이면 모든 역할을 포함**하도록 동작한다:

```go
func (i *Identity) HasRole(role org.RoleType) bool {
    if i.GetIsGrafanaAdmin() {
        return true
    }
    return i.GetOrgRole().Includes(role)
}
```

`GetOrgRole`은 현재 활성 조직의 역할을 반환한다. `OrgRoles` 맵에서 현재 `OrgID`에 해당하는 역할을 조회하고, 없으면 `org.RoleNone`을 반환한다.

### Permissions 구조

`Permissions` 필드는 **조직 ID -> 액션 -> 스코프 목록**의 3중 맵이다:

```
map[int64]map[string][]string

예시:
{
    1: {                                    // Org ID 1
        "dashboards:read": [                // Action
            "dashboards:uid:abc",           // Scope
            "dashboards:uid:def",
            "folders:uid:general"
        ],
        "datasources:write": [
            "datasources:*"
        ]
    },
    0: {                                    // Global Org (org 0)
        "users:read": ["global.users:*"]
    }
}
```

---

## 3. ClientParams와 동기화 힌트

`ClientParams`는 인증 클라이언트가 인증 서비스에 전달하는 **동기화 힌트**이다. 인증 성공 후 어떤 후처리를 수행할지 결정한다.

```go
type ClientParams struct {
    SyncUser            bool                    // 내부 DB의 사용자 정보 업데이트
    AllowSignUp         bool                    // DB에 없으면 새 사용자 생성 (SyncUser 필요)
    EnableUser          bool                    // 비활성화된 사용자 활성화 (SyncUser 필요)
    FetchSyncedUser     bool                    // 필수 정보가 Identity에 추가되도록 보장
    SyncTeams           bool                    // IdP 그룹 → Grafana 팀 동기화
    SyncOrgRoles        bool                    // IdP 역할 → Grafana 조직 역할 동기화
    CacheAuthProxyKey   string                  // Auth Proxy 캐시 키
    LookUpParams        login.UserLookupParams  // DB 사용자 조회 파라미터
    SyncPermissions     bool                    // DB에서 권한 로드하여 Identity에 추가
    FetchPermissionsParams FetchPermissionsParams // 권한 조회 옵션
    AllowGlobalOrg      bool                    // 글로벌 스코프(Org 0) 인증 허용
}
```

### 클라이언트별 ClientParams 설정 비교

| 클라이언트 | SyncUser | AllowSignUp | FetchSyncedUser | SyncPermissions | SyncTeams | SyncOrgRoles |
|-----------|----------|-------------|-----------------|-----------------|-----------|--------------|
| Session | - | - | O | O | - | - |
| API Key | - | - | O | O | - | - |
| JWT | O | 설정 | O | O | 설정 | !SkipSync |
| OAuth | O | 설정 | O | O | O | 역할존재시 |
| LDAP | O | 설정 | O | O | O | !SkipSync |
| Proxy | - | - | O | O | - | - |
| Render | - | - | O (User) | O | - | - |
| Form | (Basic에 위임) | - | - | - | - | - |

---

## 4. 인증 클라이언트 인터페이스 체계

Grafana의 인증 클라이언트는 **기본 인터페이스 + 선택적 인터페이스** 패턴으로 설계되었다.

### 기본 인터페이스

```go
type Client interface {
    Authenticator                           // Authenticate(ctx, r) → Identity
    Name() string                           // 클라이언트 고유 이름
    IsEnabled() bool                        // 활성화 여부
}
```

### 선택적 인터페이스

| 인터페이스 | 메서드 | 용도 |
|-----------|--------|------|
| `ContextAwareClient` | `Test(ctx, r) bool`, `Priority() uint` | 자동 요청 라우팅 |
| `HookClient` | `Hook(ctx, identity, r) error` | 인증 후 클라이언트별 후처리 |
| `RedirectClient` | `RedirectURL(ctx, r) → Redirect` | OAuth 리다이렉트 URL 생성 |
| `LogoutClient` | `Logout(ctx, user, token) → Redirect` | 로그아웃 시 추가 작업 |
| `PasswordClient` | `AuthenticatePassword(ctx, r, user, pass) → Identity` | 비밀번호 인증 |
| `ProxyClient` | `AuthenticateProxy(ctx, r, user, additional) → Identity` | 프록시 인증 |
| `IdentityResolverClient` | `ResolveIdentity(ctx, orgID, type, id) → Identity` | ID → Identity 해석 |
| `UsageStatClient` | `UsageStatFn(ctx) → map` | 사용 통계 수집 |
| `SSOSettingsAwareClient` | `GetConfig() SSOClientConfig` | SSO 설정 조회 |

### 왜 이런 설계인가?

이 설계의 핵심은 **관심사의 분리**이다. 모든 인증 클라이언트가 리다이렉트나 로그아웃을 지원할 필요는 없다. Basic Auth는 리다이렉트가 필요 없고, API Key는 로그아웃이 필요 없다. 선택적 인터페이스 패턴을 사용하면:

1. **최소 구현**: Client만 구현하면 인증 클라이언트 등록 가능
2. **점진적 확장**: 필요한 기능만 추가 구현
3. **타입 안전성**: 런타임에 인터페이스 체크로 기능 지원 여부 판단
4. **독립적 진화**: 새 인터페이스 추가가 기존 클라이언트에 영향 없음

---

## 5. 클라이언트 우선순위와 요청 라우팅

`ContextAwareClient`를 구현한 클라이언트들은 **Priority 값에 따라 정렬**되어 순서대로 시도된다. 낮은 숫자가 높은 우선순위를 의미한다.

### 우선순위 테이블

| Priority | 클라이언트 | 소스 파일 | 판별 기준 |
|----------|-----------|----------|----------|
| 10 | Render | `render.go` | `renderKey` 쿠키 존재 |
| 20 | JWT | `jwt.go` | 설정된 헤더에 JWT 토큰 + sub 클레임 존재 |
| 30 | API Key | `api_key.go` | Authorization 헤더에 Bearer/Basic(api_key) 토큰 |
| 40 | Basic | `basic.go` | HTTP Basic Auth 헤더 |
| 50 | Proxy | `proxy.go` | 설정된 프록시 헤더에 값 존재 |
| 60 | Session | `session.go` | `grafana_session` 쿠키 존재 |

### Test 메서드의 역할

각 클라이언트의 `Test` 메서드는 **해당 요청을 처리할 수 있는지** 빠르게 판단한다. 실제 인증은 수행하지 않는다:

```go
// Render: renderKey 쿠키 확인
func (c *Render) Test(ctx context.Context, r *authn.Request) bool {
    if r.HTTPRequest == nil { return false }
    return getRenderKey(r) != ""
}

// JWT: 헤더에서 JWT 토큰 추출 + sub 클레임 확인
func (s *JWT) Test(ctx context.Context, r *authn.Request) bool {
    if !s.cfg.JWTAuth.Enabled || s.cfg.JWTAuth.HeaderName == "" { return false }
    jwtToken := s.retrieveToken(r.HTTPRequest)
    if jwtToken == "" { return false }
    return authJWT.HasSubClaim(jwtToken)
}

// Basic: HTTP Basic Auth 헤더 존재 확인
func (c *Basic) Test(ctx context.Context, r *authn.Request) bool {
    if r.HTTPRequest == nil { return false }
    return looksLikeBasicAuthRequest(r)
}

// Session: grafana_session 쿠키 존재 확인
func (s *Session) Test(ctx context.Context, r *authn.Request) bool {
    if s.cfg.LoginCookieName == "" { return false }
    if _, err := r.HTTPRequest.Cookie(s.cfg.LoginCookieName); err != nil {
        return false
    }
    return true
}
```

### 우선순위 설계의 의도

1. **Render(10)**: 렌더링 서비스는 내부 전용이며, 다른 인증 방식과 충돌 없이 가장 먼저 처리
2. **JWT(20)**: 토큰 기반 인증은 상태가 없으므로 빠르게 처리
3. **API Key(30)**: Authorization 헤더의 Bearer 토큰이 JWT와 겹칠 수 있으므로 JWT 이후에 시도
4. **Basic(40)**: HTTP Basic Auth는 API Key와 Authorization 헤더를 공유할 수 있으므로 나중에
5. **Proxy(50)**: 외부 프록시 헌트를 기반으로 하므로 일반 인증 이후
6. **Session(60)**: 쿠키 기반 세션은 가장 일반적이므로 마지막 순위

---

## 6. Basic Auth 클라이언트

`pkg/services/authn/clients/basic.go`에 구현된 Basic Auth 클라이언트는 HTTP Basic 인증을 처리한다.

### 구조와 동작

```go
type Basic struct {
    client authn.PasswordClient  // 실제 비밀번호 검증을 위임
}

func (c *Basic) Authenticate(ctx context.Context, r *authn.Request) (*authn.Identity, error) {
    username, password, ok := getBasicAuthFromRequest(r)
    if !ok {
        return nil, errDecodingBasicAuthHeader.Errorf("failed to decode basic auth header")
    }
    return c.client.AuthenticatePassword(ctx, r, username, password)
}
```

Basic Auth는 직접 비밀번호를 검증하지 않고 `PasswordClient` 인터페이스에 위임한다. 이를 통해 Grafana DB 비밀번호 검증과 LDAP 비밀번호 검증을 동일한 Basic Auth 진입점에서 처리할 수 있다.

### PasswordClient 체인

```
Basic Auth 요청
    |
    v
Basic.Authenticate()
    |
    v
PasswordClient.AuthenticatePassword(username, password)
    |
    +---> Grafana DB 검증 (password.go)
    |        |
    |        +---> bcrypt 해시 비교
    |
    +---> LDAP 검증 (ldap.go)
             |
             +---> LDAP bind 인증
```

---

## 7. API Key 클라이언트

`pkg/services/authn/clients/api_key.go`에 구현된 API Key 클라이언트는 두 가지 API 키 포맷을 지원한다.

### 키 포맷 분류

```go
func (s *APIKey) getAPIKey(ctx context.Context, token string) (*apikey.APIKey, error) {
    fn := s.getFromToken
    if !strings.HasPrefix(token, satokengen.GrafanaPrefix) {
        fn = s.getFromTokenLegacy
    }
    apiKey, err := fn(ctx, token)
    return apiKey, err
}
```

| 포맷 | 접두사 | 검증 방식 | 소스 |
|------|--------|----------|------|
| 신규 포맷 | `glsa_` (GrafanaPrefix) | `satokengen.Decode` → Hash → DB 조회 | `getFromToken` |
| 레거시 포맷 | 이름:해시 형태 | `apikeygen.Decode` → 이름으로 DB 조회 → HMAC 검증 | `getFromTokenLegacy` |

### 토큰 추출

```go
func getTokenFromRequest(r *authn.Request) string {
    header := r.HTTPRequest.Header.Get("Authorization")
    if strings.HasPrefix(header, bearerPrefix) {
        return strings.TrimPrefix(header, bearerPrefix)
    }
    if strings.HasPrefix(header, basicPrefix) {
        username, password, err := util.DecodeBasicAuthHeader(header)
        if err == nil && username == "api_key" {
            return password
        }
    }
    return ""
}
```

API 키는 두 가지 방법으로 전달할 수 있다:
- `Authorization: Bearer <token>` 헤더
- `Authorization: Basic api_key:<token>` (Base64 인코딩)

### 검증 흐름

```
Authorization 헤더
    |
    v
토큰 추출 (Bearer 또는 Basic)
    |
    v
접두사 확인 (glsa_ 여부)
    |
    +---> 신규: satokengen.Decode → Hash → GetAPIKeyByHash
    |
    +---> 레거시: apikeygen.Decode → GetApiKeyByName → IsValid(HMAC)
    |
    v
validateApiKey()
    |
    +---> 만료 확인: key.Expires <= time.Now()
    +---> 취소 확인: key.IsRevoked == true
    +---> 조직 확인: r.OrgID == key.OrgID
    +---> SA 확인: key.ServiceAccountId 존재 (필수)
    |
    v
newServiceAccountIdentity(key)
    → Type: TypeServiceAccount
    → AuthenticatedBy: APIKeyAuthModule
    → ClientParams: FetchSyncedUser=true, SyncPermissions=true
```

### HookClient 구현

API Key 클라이언트는 `HookClient` 인터페이스도 구현한다. 인증 성공 후 **LastUsedAt을 비동기로 업데이트**한다:

```go
func (s *APIKey) Hook(ctx context.Context, identity *authn.Identity, r *authn.Request) error {
    if r.GetMeta(metaKeySkipLastUsed) != "" {
        return nil
    }
    go func(keyID string) {
        // 비동기로 LastUsedAt 업데이트
        s.apiKeyService.UpdateAPIKeyLastUsedDate(context.Background(), id)
    }(r.GetMeta(metaKeyID))
    return nil
}
```

마지막 사용 시간이 5분 이내이면 업데이트를 건너뛴다:

```go
func shouldUpdateLastUsedAt(key *apikey.APIKey) bool {
    return key.LastUsedAt == nil || time.Since(*key.LastUsedAt) > 5*time.Minute
}
```

---

## 8. JWT 클라이언트

`pkg/services/authn/clients/jwt.go`에 구현된 JWT 클라이언트는 OIDC/JWT 토큰을 검증하고 클레임에서 사용자 정보를 추출한다.

### 토큰 추출

```go
func (s *JWT) retrieveToken(httpRequest *http.Request) string {
    jwtToken := httpRequest.Header.Get(s.cfg.JWTAuth.HeaderName)
    if jwtToken == "" && s.cfg.JWTAuth.URLLogin {
        jwtToken = httpRequest.URL.Query().Get("auth_token")
    }
    return strings.TrimPrefix(jwtToken, "Bearer ")
}
```

JWT 토큰은 **설정된 헤더 이름**에서 추출한다 (기본값은 보통 `X-JWT-Assertion` 또는 `Authorization`). URL 로그인이 활성화되면 `auth_token` 쿼리 파라미터에서도 추출한다.

### 클레임 추출 설정

```go
// sub 클레임 (필수)
sub, _ := claims["sub"].(string)

// 사용자명: username_claim 또는 username_attribute_path (JMESPath)
if key := s.cfg.JWTAuth.UsernameClaim; key != "" {
    id.Login, _ = claims[key].(string)
} else if key := s.cfg.JWTAuth.UsernameAttributePath; key != "" {
    id.Login, _ = util.SearchJSONForStringAttr(key, claims)
}

// 이메일: email_claim 또는 email_attribute_path (JMESPath)
if key := s.cfg.JWTAuth.EmailClaim; key != "" {
    id.Email, _ = claims[key].(string)
} else if key := s.cfg.JWTAuth.EmailAttributePath; key != "" {
    id.Email, _ = util.SearchJSONForStringAttr(key, claims)
}
```

### 역할 추출

```go
func (s *JWT) extractRoleAndAdmin(claims map[string]any) (org.RoleType, bool) {
    if s.cfg.JWTAuth.RoleAttributePath == "" {
        return "", false
    }
    role, err := util.SearchJSONForStringAttr(s.cfg.JWTAuth.RoleAttributePath, claims)
    if role == "GrafanaAdmin" {
        return org.RoleAdmin, true  // GrafanaAdmin → Admin 역할 + isGrafanaAdmin=true
    }
    return org.RoleType(role), false
}
```

`role_attribute_path`는 JMESPath 표현식을 사용한다. 예를 들어 `contains(groups[*], 'admin') && 'Admin' || 'Viewer'`와 같은 복잡한 조건 매핑이 가능하다.

### 그룹과 조직 매핑

```go
// 그룹 추출
func (s *JWT) extractGroups(claims map[string]any) ([]string, error) {
    return util.SearchJSONForStringSliceAttr(s.cfg.JWTAuth.GroupsAttributePath, claims)
}

// 조직 매핑
func (s *JWT) extractOrgs(claims map[string]any) ([]string, error) {
    return util.SearchJSONForStringSliceAttr(s.cfg.JWTAuth.OrgAttributePath, claims)
}

// OrgRoleMapper로 조직-역할 매핑
id.OrgRoles = s.orgRoleMapper.MapOrgRoles(s.orgMappingCfg, externalOrgs, role)
```

---

## 9. OAuth 클라이언트

`pkg/services/authn/clients/oauth.go`에 구현된 OAuth 클라이언트는 OAuth 2.0 Authorization Code Flow를 처리한다.

### 지원 프로바이더

OAuth 클라이언트는 프로바이더별로 인스턴스가 생성된다:

```go
func ProvideOAuth(name string, cfg *setting.Cfg, ...) *OAuth {
    providerName := strings.TrimPrefix(name, "auth.client.")
    return &OAuth{
        name, fmt.Sprintf("oauth_%s", providerName), providerName,
        ...
    }
}
```

- GitHub (`auth.client.github`)
- GitLab (`auth.client.gitlab`)
- Google (`auth.client.google`)
- Azure AD (`auth.client.azuread`)
- Okta (`auth.client.okta`)
- Generic OAuth (`auth.client.generic_oauth`)

### 인증 흐름 (Authorization Code Exchange)

```
1. RedirectURL() 호출 → 리다이렉트 생성
    |
    +---> PKCE 코드 생성 (UsePKCE=true인 경우)
    +---> State 생성 (랜덤 32바이트 + SHA256 해시)
    +---> AuthCodeURL 생성
    |
2. 사용자가 IdP에서 인증 후 콜백
    |
3. Authenticate() 호출
    |
    +---> State 검증 (쿠키 값 vs 쿼리 파라미터 해시)
    +---> PKCE 검증 (쿠키에서 code_verifier 추출)
    +---> Authorization Code → Token Exchange
    +---> UserInfo 조회
    +---> Identity 생성
```

### State 검증 상세

```go
func (c *OAuth) Authenticate(ctx context.Context, r *authn.Request) (*authn.Identity, error) {
    // 쿠키에 저장된 해시값 조회
    stateCookie, err := r.HTTPRequest.Cookie(oauthStateCookieName)

    // IdP가 반환한 state를 해시하여 비교
    stateQuery := hashOAuthState(
        r.HTTPRequest.URL.Query().Get(oauthStateQueryName),
        c.cfg.SecretKey,
        oauthCfg.ClientSecret,
    )

    if stateQuery != stateCookie.Value {
        return nil, errOAuthInvalidState.Errorf("provided state did not match stored state")
    }
    ...
}
```

### Workload Identity 지원

OAuth 클라이언트는 Workload Identity Federation도 지원한다:

```go
if oauthCfg.ClientAuthentication == social.WorkloadIdentity {
    federatedToken, err := os.ReadFile(oauthCfg.WorkloadIdentityTokenFile)
    opts = append(opts,
        oauth2.SetAuthURLParam("client_id", oauthCfg.ClientId),
        oauth2.SetAuthURLParam("client_assertion", strings.TrimSpace(string(federatedToken))),
        oauth2.SetAuthURLParam("client_assertion_type", "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"),
    )
}
```

### LogoutClient 구현

OAuth 클라이언트는 `LogoutClient`도 구현하여 OIDC Single Logout을 지원한다:

```go
func (c *OAuth) Logout(ctx context.Context, user identity.Requester, sessionToken *auth.UserToken) (*authn.Redirect, bool) {
    // 1. OAuth 토큰 무효화
    c.oauthService.InvalidateOAuthTokens(ctx, user, ...)

    // 2. SignoutRedirectUrl 확인
    redirectURL := getOAuthSignoutRedirectURL(c.cfg, oauthCfg)

    // 3. OIDC 로그아웃이면 id_token_hint 추가
    if isOIDCLogout(redirectURL) && token != nil && token.Valid() {
        if idToken, ok := token.Extra("id_token").(string); ok {
            redirectURL = withIDTokenHint(redirectURL, idToken)
        }
    }

    return &authn.Redirect{URL: redirectURL}, true
}
```

---

## 10. LDAP 클라이언트

`pkg/services/authn/clients/ldap.go`에 구현된 LDAP 클라이언트는 `PasswordClient`와 `ProxyClient` 인터페이스를 모두 구현한다.

### 이중 역할

```go
var _ authn.ProxyClient = new(LDAP)
var _ authn.PasswordClient = new(LDAP)
```

- **PasswordClient**: Basic Auth나 Form 로그인에서 비밀번호 검증
- **ProxyClient**: Auth Proxy에서 사용자명으로 LDAP 조회

### 비밀번호 인증

```go
func (c *LDAP) AuthenticatePassword(ctx context.Context, r *authn.Request, username, password string) (*authn.Identity, error) {
    info, err := c.service.Login(&login.LoginUserQuery{
        Username: username,
        Password: password,
    })

    if errors.Is(err, multildap.ErrCouldNotFindUser) {
        return c.disableUser(ctx, username)  // LDAP에 없으면 비활성화
    }

    if errors.Is(err, multildap.ErrInvalidCredentials) {
        return nil, errInvalidPassword.Errorf("invalid password: %w", err)
    }

    return c.identityFromLDAPInfo(r.OrgID, info), nil
}
```

### 사용자 비활성화 로직

LDAP에서 사용자를 찾을 수 없을 때, 이전에 LDAP으로 로그인한 이력이 있으면 Grafana에서 해당 사용자를 **자동 비활성화**한다:

```go
func (c *LDAP) disableUser(ctx context.Context, username string) (*authn.Identity, error) {
    // 1. Grafana DB에서 로그인 이름으로 사용자 조회
    dbUser, _ := c.userService.GetByLogin(ctx, &user.GetUserByLoginQuery{LoginOrEmail: username})

    // 2. LDAP 인증 모듈로 로그인한 이력 확인
    query := &login.GetAuthInfoQuery{UserId: dbUser.ID, AuthModule: login.LDAPAuthModule}
    authinfo, _ := c.authInfoService.GetAuthInfo(ctx, query)

    // 3. 사용자 비활성화
    isDisabled := true
    c.userService.Update(ctx, &user.UpdateUserCommand{UserID: dbUser.ID, IsDisabled: &isDisabled})

    return nil, retErr
}
```

이 설계의 의도는 **LDAP 디렉토리에서 제거된 사용자가 Grafana에 계속 접근하는 것을 방지**하기 위한 것이다.

---

## 11. Session 클라이언트

`pkg/services/authn/clients/session.go`에 구현된 Session 클라이언트는 쿠키 기반 세션 인증을 처리한다.

### 인증 로직

```go
func (s *Session) Authenticate(ctx context.Context, r *authn.Request) (*authn.Identity, error) {
    // 1. 쿠키에서 세션 토큰 추출
    unescapedCookie, err := r.HTTPRequest.Cookie(s.cfg.LoginCookieName)
    rawSessionToken, _ := url.QueryUnescape(unescapedCookie.Value)

    // 2. 토큰 조회
    token, err := s.sessionService.LookupToken(ctx, rawSessionToken)

    // 3. 토큰 로테이션 필요 여부 확인
    if token.NeedsRotation(time.Duration(s.cfg.TokenRotationIntervalMinutes) * time.Minute) {
        return nil, authn.NewTokenNeedsRotationError(token.UserId)
    }

    // 4. Identity 생성
    ident := &authn.Identity{
        ID:           strconv.FormatInt(token.UserId, 10),
        Type:         claims.TypeUser,
        SessionToken: token,
        ClientParams: authn.ClientParams{
            FetchSyncedUser: true,
            SyncPermissions: true,
        },
    }

    // 5. 인증 정보 조회 (선택)
    info, err := s.authInfoService.GetAuthInfo(ctx, &login.GetAuthInfoQuery{UserId: token.UserId})
    if err == nil {
        ident.AuthID = info.AuthId
        ident.AuthenticatedBy = info.AuthModule
    }

    return ident, nil
}
```

### 토큰 로테이션 에러

`TokenNeedsRotationError`가 반환되면 미들웨어가 이를 감지하여 `/user/auth-tokens/rotate` 엔드포인트로 리다이렉트한다. 이를 통해 세션 토큰이 주기적으로 갱신된다.

---

## 12. Auth Proxy 클라이언트

`pkg/services/authn/clients/proxy.go`에 구현된 Auth Proxy 클라이언트는 리버스 프록시가 설정한 HTTP 헤더를 신뢰하여 인증한다.

### IP 화이트리스트

```go
func (c *Proxy) isAllowedIP(r *authn.Request) bool {
    if len(c.acceptedIPs) == 0 {
        return true  // 화이트리스트가 없으면 모두 허용
    }
    host, _, _ := net.SplitHostPort(r.HTTPRequest.RemoteAddr)
    ip := net.ParseIP(host)
    for _, v := range c.acceptedIPs {
        if v.Contains(ip) {
            return true
        }
    }
    return false
}
```

### 프록시 헤더 추출

```go
const (
    proxyFieldName   = "Name"
    proxyFieldEmail  = "Email"
    proxyFieldLogin  = "Login"
    proxyFieldRole   = "Role"
    proxyFieldGroups = "Groups"
)
```

기본 헤더인 `X-WEBAUTH-USER`에서 사용자명을 추출하고, 추가 헤더에서 이메일, 이름, 역할, 그룹 정보를 추출한다.

### 캐싱 메커니즘

Auth Proxy는 **RemoteCache를 이용한 캐싱**을 구현한다. 프록시 인증은 매 요청마다 수행되므로, 동일한 헤더 조합에 대해 사용자 ID를 캐시하여 성능을 최적화한다.

```
요청 → IP 확인 → 헤더 추출 → 캐시 키 생성 (FNV-128a 해시)
    |
    +---> 캐시 히트 → 캐시된 사용자 ID로 Identity 생성
    |
    +---> 캐시 미스 → ProxyClient 체인으로 인증
                         |
                         +---> LDAP.AuthenticateProxy()
                         +---> Grafana.AuthenticateProxy()
                         |
                         v
                    Hook()에서 캐시 저장
```

캐시 키는 사용자명 + 모든 추가 헤더 값을 결합한 FNV-128a 해시이다:

```go
func getProxyCacheKey(username string, additional map[string]string) (string, bool) {
    key := strings.Builder{}
    key.WriteString(username)
    for _, k := range proxyFields {
        if v, ok := additional[k]; ok {
            key.WriteString(v)
        }
    }
    hash := fnv.New128a()
    hash.Write([]byte(key.String()))
    return strings.Join([]string{proxyCachePrefix, hex.EncodeToString(hash.Sum(nil))}, ":"), true
}
```

---

## 13. Form 클라이언트

`pkg/services/authn/clients/form.go`에 구현된 Form 클라이언트는 로그인 폼 제출을 처리한다.

```go
type loginForm struct {
    Username string `json:"user" binding:"Required"`
    Password string `json:"password" binding:"Required"`
}

func (c *Form) Authenticate(ctx context.Context, r *authn.Request) (*authn.Identity, error) {
    form := loginForm{}
    if err := web.Bind(r.HTTPRequest, &form); err != nil {
        return nil, errBadForm.Errorf("failed to parse request: %w", err)
    }
    return c.client.AuthenticatePassword(ctx, r, form.Username, form.Password)
}
```

Form 클라이언트는 `ContextAwareClient`를 구현하지 않는다. 즉, 자동 라우팅 대상이 아니며, `authn.Service.Login(ctx, authn.ClientForm, r)` 형태로 명시적으로 호출된다.

---

## 14. Render 클라이언트

`pkg/services/authn/clients/render.go`에 구현된 Render 클라이언트는 **Grafana 이미지 렌더링 서비스**를 인증한다.

```go
func (c *Render) Authenticate(ctx context.Context, r *authn.Request) (*authn.Identity, error) {
    key := getRenderKey(r)
    renderUsr, ok := c.renderService.GetRenderUser(ctx, key)
    if !ok {
        return nil, errInvalidRenderKey.Errorf("found no render user for key: %s", key)
    }

    if renderUsr.UserID <= 0 {
        identityType := claims.TypeAnonymous
        if org.RoleType(renderUsr.OrgRole) == org.RoleAdmin {
            identityType = claims.TypeRenderService
        }
        return &authn.Identity{
            ID: "0", UID: "0",
            Type:    identityType,
            OrgID:   renderUsr.OrgID,
            OrgRoles: map[int64]org.RoleType{renderUsr.OrgID: org.RoleType(renderUsr.OrgRole)},
            ...
        }, nil
    }

    return &authn.Identity{
        ID:   strconv.FormatInt(renderUsr.UserID, 10),
        Type: claims.TypeUser,
        ...
    }, nil
}
```

Render Key는 `renderKey` 쿠키에서 추출되며, 렌더링 서비스가 생성한 임시 키다. 가장 높은 우선순위(Priority: 10)를 가진다.

---

## 15. 세션 관리와 토큰 로테이션

### 세션 쿠키 쌍

Grafana는 인증된 세션을 위해 **두 개의 쿠키**를 사용한다:

| 쿠키 | 용도 | HttpOnly |
|------|------|----------|
| `grafana_session` (LoginCookieName) | 세션 토큰 (해시되지 않은 값) | Yes |
| `grafana_session_expiry` | 다음 로테이션 시간 (Unix timestamp) | No |

```go
func WriteSessionCookie(w http.ResponseWriter, cfg *setting.Cfg, token *usertoken.UserToken) {
    maxAge := int(cfg.LoginMaxLifetime.Seconds())
    if cfg.LoginMaxLifetime <= 0 {
        maxAge = -1
    }
    cookies.WriteCookie(w, cfg.LoginCookieName, url.QueryEscape(token.UnhashedToken), maxAge, nil)
    expiry := token.NextRotation(time.Duration(cfg.TokenRotationIntervalMinutes) * time.Minute)
    cookies.WriteCookie(w, sessionExpiryCookie, url.QueryEscape(strconv.FormatInt(expiry.Unix(), 10)), maxAge, ...)
}
```

### 토큰 로테이션

토큰 로테이션은 세션 하이재킹 위험을 줄이기 위한 메커니즘이다:

```
세션 토큰 생성 (로그인 시)
    |
    v
[token_rotation_interval_minutes] (기본 10분) 경과
    |
    v
Session.Authenticate()에서 NeedsRotation 감지
    |
    v
TokenNeedsRotationError 반환
    |
    v
미들웨어가 /user/auth-tokens/rotate로 리다이렉트
    |
    v
새 토큰 발급 + 쿠키 갱신
```

### 세션 수명 설정

| 설정 | 기본값 | 설명 |
|------|--------|------|
| `login_maximum_lifetime_duration` | 30일 | 세션 최대 수명 |
| `login_maximum_inactive_lifetime_duration` | 7일 | 최대 비활성 시간 |
| `token_rotation_interval_minutes` | 10분 | 토큰 로테이션 간격 |

### MaxConcurrentSessions

Grafana는 동시 세션 수를 제한할 수 있다. `TokenRevokedError`에는 `MaxConcurrentSessions` 필드가 포함되어, 클라이언트가 적절한 에러 메시지를 표시할 수 있다:

```go
if errors.As(c.LookupTokenErr, &revokedErr) {
    tokenRevoked(c, revokedErr)
    return
}
```

---

## 16. OAuth PKCE와 State 검증

### PKCE (Proof Key for Code Exchange)

`pkg/services/authn/clients/oauth.go`에서 PKCE는 RFC 7636을 따른다:

```go
func genPKCECodeVerifier() (string, error) {
    // 96 랜덤 바이트 생성
    raw := make([]byte, 96)
    _, err := rand.Read(raw)
    if err != nil {
        return "", err
    }
    // base64url 인코딩 → 128 문자
    ascii := make([]byte, 128)
    base64.RawURLEncoding.Encode(ascii, raw)
    return string(ascii), nil
}
```

PKCE 흐름:

```
1. 리다이렉트 단계:
   code_verifier 생성 (96 bytes → base64url → 128 chars)
   code_challenge = SHA256(code_verifier) → base64url
   oauth_code_verifier 쿠키에 code_verifier 저장
   AuthCodeURL에 code_challenge + code_challenge_method=S256 추가

2. 콜백 단계:
   oauth_code_verifier 쿠키에서 code_verifier 추출
   Token Exchange에 code_verifier 파라미터 추가
   IdP가 SHA256(code_verifier) == 저장된 code_challenge 검증
```

### State 검증

State 파라미터는 CSRF 방지를 위해 사용된다:

```go
func genOAuthState(secret, seed string) (string, string, error) {
    // 32 바이트 랜덤 → base64url 인코딩 = state
    rnd := make([]byte, 32)
    rand.Read(rnd)
    state := base64.URLEncoding.EncodeToString(rnd)
    // state + SecretKey + ClientSecret → SHA256 = hashedState
    return state, hashOAuthState(state, secret, seed), nil
}

func hashOAuthState(state, secret, seed string) string {
    hashBytes := sha256.Sum256([]byte(state + secret + seed))
    return hex.EncodeToString(hashBytes[:])
}
```

검증 흐름:

```
1. 리다이렉트 단계:
   state = base64(random 32 bytes)
   hashedState = SHA256(state + SecretKey + ClientSecret)
   oauth_state 쿠키에 hashedState 저장
   AuthCodeURL에 state 파라미터 추가

2. 콜백 단계:
   IdP가 반환한 state를 다시 해시
   queryHash = SHA256(returnedState + SecretKey + ClientSecret)
   쿠키의 hashedState와 queryHash 비교
   일치하면 검증 성공
```

### 쿠키 저장소

| 쿠키 이름 | 저장 값 | 용도 |
|-----------|--------|------|
| `oauth_state` | SHA256(state + SecretKey + ClientSecret) | CSRF 방지 state 검증 |
| `oauth_code_verifier` | 원본 code_verifier (128 chars) | PKCE code exchange |

---

## 17. 인증 설정

Grafana의 인증 설정은 `conf/defaults.ini`의 여러 섹션에 분산되어 있다.

### [auth] 기본 설정

| 설정 키 | 기본값 | 설명 |
|---------|--------|------|
| `login_cookie_name` | `grafana_session` | 세션 쿠키 이름 |
| `login_maximum_inactive_lifetime_duration` | 7d | 최대 비활성 시간 |
| `login_maximum_lifetime_duration` | 30d | 세션 최대 수명 |
| `token_rotation_interval_minutes` | 10 | 토큰 로테이션 간격 |
| `disable_login_form` | false | 로그인 폼 비활성화 |
| `disable_signout_menu` | false | 로그아웃 메뉴 숨김 |
| `signout_redirect_url` | (빈 값) | 로그아웃 후 리다이렉트 URL |
| `oauth_auto_login` | false | OAuth 자동 로그인 |
| `oauth_allow_insecure_email_lookup` | false | 안전하지 않은 이메일 조회 허용 |

### [auth.basic] 설정

| 설정 키 | 기본값 | 설명 |
|---------|--------|------|
| `enabled` | true | Basic Auth 활성화 |

### [auth.proxy] 설정

| 설정 키 | 기본값 | 설명 |
|---------|--------|------|
| `enabled` | false | Auth Proxy 활성화 |
| `header_name` | `X-WEBAUTH-USER` | 사용자명 헤더 |
| `header_property` | username | 헤더 속성 타입 |
| `auto_sign_up` | true | 자동 회원가입 |
| `sync_ttl` | 60 | 캐시 TTL (분) |
| `whitelist` | (빈 값) | IP 화이트리스트 |
| `headers` | (빈 값) | 추가 헤더 매핑 |

### [auth.jwt] 설정

| 설정 키 | 기본값 | 설명 |
|---------|--------|------|
| `enabled` | false | JWT 인증 활성화 |
| `header_name` | (필수) | JWT 토큰 헤더 이름 |
| `email_claim` | (빈 값) | 이메일 클레임 키 |
| `username_claim` | (빈 값) | 사용자명 클레임 키 |
| `jwk_set_url` | (빈 값) | JWK Set URL |
| `jwk_set_file` | (빈 값) | 로컬 JWK 파일 |
| `role_attribute_path` | (빈 값) | 역할 JMESPath |
| `role_attribute_strict` | false | 엄격한 역할 검증 |
| `auto_sign_up` | false | 자동 회원가입 |
| `url_login` | false | URL 파라미터 JWT 허용 |
| `groups_attribute_path` | (빈 값) | 그룹 JMESPath |
| `org_mapping` | (빈 값) | 조직 매핑 설정 |
| `skip_org_role_sync` | false | 조직 역할 동기화 건너뜀 |

### [auth.ldap] 설정

| 설정 키 | 기본값 | 설명 |
|---------|--------|------|
| `enabled` | false | LDAP 활성화 |
| `config_file` | `/etc/grafana/ldap.toml` | LDAP 설정 파일 경로 |
| `allow_sign_up` | true | LDAP 사용자 자동 생성 |
| `skip_org_role_sync` | false | 조직 역할 동기화 건너뜀 |

### [auth.anonymous] 설정

| 설정 키 | 기본값 | 설명 |
|---------|--------|------|
| `enabled` | false | 익명 접근 활성화 |
| `org_name` | `Main Org.` | 익명 사용자 조직 |
| `org_role` | `Viewer` | 익명 사용자 역할 |

### OAuth 프로바이더별 설정

각 OAuth 프로바이더는 `[auth.{provider}]` 섹션을 가진다 (예: `[auth.github]`, `[auth.google]`):

| 공통 설정 키 | 설명 |
|-------------|------|
| `enabled` | 프로바이더 활성화 |
| `client_id` | OAuth 클라이언트 ID |
| `client_secret` | OAuth 클라이언트 시크릿 |
| `scopes` | 요청 스코프 |
| `auth_url` | 인증 엔드포인트 URL |
| `token_url` | 토큰 엔드포인트 URL |
| `api_url` | 사용자 정보 API URL |
| `allowed_domains` | 허용 이메일 도메인 |
| `allow_sign_up` | 자동 회원가입 허용 |
| `role_attribute_path` | 역할 매핑 JMESPath |
| `use_pkce` | PKCE 활성화 |
| `use_refresh_token` | 리프레시 토큰 사용 |

---

## 18. 인증 미들웨어와 후크 시스템

### PostAuthHook

인증 성공 후 실행되는 후크이다. 우선순위가 있으며 낮은 숫자가 먼저 실행된다:

```go
type PostAuthHookFn func(ctx context.Context, identity *Identity, r *Request) error

// 등록
service.RegisterPostAuthHook(hook PostAuthHookFn, priority uint)
```

주요 PostAuthHook 동작:

```
인증 성공 → Identity 반환
    |
    v
[PostAuthHook 체인 (우선순위 순)]
    |
    +---> SyncUser: 외부 Identity → Grafana DB 사용자 동기화
    |     (Identity.ClientParams.SyncUser == true인 경우)
    |
    +---> SyncOrgRoles: IdP 역할 → Grafana 조직 역할 매핑
    |     (Identity.ClientParams.SyncOrgRoles == true인 경우)
    |
    +---> SyncTeams: IdP 그룹 → Grafana 팀 동기화
    |     (Identity.ClientParams.SyncTeams == true인 경우)
    |
    +---> SyncPermissions: DB에서 권한 로드
    |     (Identity.ClientParams.SyncPermissions == true인 경우)
    |
    +---> FetchSyncedUser: 동기화된 사용자 정보 가져오기
    |     (Identity.ClientParams.FetchSyncedUser == true인 경우)
    |
    +---> HookClient.Hook(): 클라이언트별 후처리
          (API Key: LastUsedAt 업데이트, Proxy: 캐시 저장 등)
```

### PostLoginHook

로그인 요청 후 실행되는 후크이다. 인증 성공/실패 모두에서 호출된다:

```go
type PostLoginHookFn func(ctx context.Context, identity *Identity, r *Request, err error)
```

### PreLogoutHook

로그아웃 전에 실행되는 후크이다:

```go
type PreLogoutHookFn func(ctx context.Context, requester identity.Requester, sessionToken *usertoken.UserToken) error
```

### 후크 시스템의 설계 의도

후크 시스템은 **인증과 후처리의 분리**를 가능하게 한다:

1. **인증 클라이언트**는 자격 증명 검증에만 집중
2. **후크**가 사용자 동기화, 권한 로드, 팀 매핑 등을 담당
3. 후크는 **우선순위 기반**으로 순서가 보장됨
4. 새로운 후처리 로직을 추가할 때 기존 인증 클라이언트를 수정할 필요 없음

### 로그인 응답 처리

`authn.go`에 정의된 유틸리티 함수들이 로그인 후 응답을 처리한다:

```go
// JSON 응답 반환 (API 호출)
func HandleLoginResponse(r, w, cfg, identity, validator, features) *response.NormalResponse

// HTTP 리다이렉트 (브라우저 로그인)
func HandleLoginRedirect(r, w, cfg, identity, validator, features)

// 내부 공통 로직
func handleLogin(r, w, cfg, identity, validator, features, redirectToCookieName) string {
    // 1. 세션 쿠키 기록
    WriteSessionCookie(w, cfg, identity.SessionToken)

    // 2. redirect_to 쿠키 확인 (로그인 전 방문 URL)
    // 3. 검증 후 리다이렉트 URL 반환
}
```

---

## 요약

Grafana의 인증 시스템은 다음과 같은 설계 원칙을 따른다:

| 원칙 | 구현 |
|------|------|
| **Pluggable** | `Client` 인터페이스 + `RegisterClient` |
| **우선순위 기반** | `ContextAwareClient.Priority()` |
| **관심사 분리** | 인증 vs 동기화(후크) vs 인가(RBAC) |
| **선택적 기능** | `HookClient`, `RedirectClient`, `LogoutClient` 등 |
| **보안** | PKCE, State 검증, 토큰 로테이션, IP 화이트리스트 |
| **확장성** | 새 클라이언트 추가가 기존 코드에 영향 없음 |

핵심 소스 파일:

| 파일 | 역할 |
|------|------|
| `pkg/services/authn/authn.go` | Service/Client 인터페이스 정의 |
| `pkg/services/authn/identity.go` | Identity 구조체 |
| `pkg/services/authn/clients/basic.go` | Basic Auth (Priority: 40) |
| `pkg/services/authn/clients/api_key.go` | API Key (Priority: 30) |
| `pkg/services/authn/clients/jwt.go` | JWT/OIDC (Priority: 20) |
| `pkg/services/authn/clients/oauth.go` | OAuth (Redirect-based) |
| `pkg/services/authn/clients/ldap.go` | LDAP (Password + Proxy) |
| `pkg/services/authn/clients/session.go` | Session Cookie (Priority: 60) |
| `pkg/services/authn/clients/proxy.go` | Auth Proxy (Priority: 50) |
| `pkg/services/authn/clients/form.go` | Form Login |
| `pkg/services/authn/clients/render.go` | Render Service (Priority: 10) |
