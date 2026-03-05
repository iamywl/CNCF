# Argo CD RBAC 및 인증 시스템 Deep-Dive

## 목차

1. [인증 체계 개요](#1-인증-체계-개요)
2. [Casbin RBAC 모델](#2-casbin-rbac-모델)
3. [Enforcer 구조체](#3-enforcer-구조체)
4. [정책 형식 및 빌트인 역할](#4-정책-형식-및-빌트인-역할)
5. [enforce() 알고리즘](#5-enforce-알고리즘)
6. [정책 동적 로딩](#6-정책-동적-로딩)
7. [SessionManager](#7-sessionmanager)
8. [로그인 보안](#8-로그인-보안)
9. [updateFailureCount() 알고리즘](#9-updatefailurecount-알고리즘)
10. [AppProject RBAC](#10-appproject-rbac)
11. [보안 패턴](#11-보안-패턴)
12. [전체 인증/인가 흐름 요약](#12-전체-인증인가-흐름-요약)

---

## 1. 인증 체계 개요

Argo CD는 크게 네 가지 인증 경로를 지원한다. 각 경로는 최종적으로 JWT 토큰을 생성하여 이후의 RBAC 검사에 사용한다.

```
┌─────────────────────────────────────────────────────────────────────────┐
│                         Argo CD 인증 체계                                │
│                                                                         │
│  ┌──────────────┐  ┌──────────────────────────────────────────────┐    │
│  │  로컬 계정   │  │              SSO / 외부 IDP                  │    │
│  │              │  │                                              │    │
│  │ admin / user │  │  ┌──────────┐  ┌──────────┐  ┌──────────┐  │    │
│  │              │  │  │   Dex    │  │  외부    │  │ 프로젝트  │  │    │
│  │ HMAC-SHA256  │  │  │ (내장)   │  │  OIDC    │  │ JWT 토큰  │  │    │
│  │   JWT 발급   │  │  │ OIDC/    │  │ 프로바   │  │          │  │    │
│  │              │  │  │ LDAP/    │  │ 이더     │  │ proj:    │  │    │
│  │ argocd-secret│  │  │ SAML/    │  │          │  │ name:    │  │    │
│  │ server.      │  │  │ GitHub/  │  │ Okta,    │  │ rolename │  │    │
│  │ secretkey로  │  │  │ Google   │  │ Azure AD │  │          │  │    │
│  │ 서명         │  │  └──────────┘  └──────────┘  └──────────┘  │    │
│  └──────────────┘  └──────────────────────────────────────────────┘    │
│         │                          │                    │               │
│         └──────────────────────────┴────────────────────┘               │
│                                    │                                    │
│                            JWT Claims 검증                              │
│                         (VerifyToken / Parse)                           │
│                                    │                                    │
│                         RBAC 검사 (Casbin)                             │
└─────────────────────────────────────────────────────────────────────────┘
```

### 1.1 로컬 계정 (Local Account)

로컬 계정은 `argocd-cm` ConfigMap의 `accounts.{name}` 키와 `argocd-secret` Secret에 저장된다.

- **비밀번호 해싱**: bcrypt (기본 cost = `bcrypt.DefaultCost = 10`)
- **JWT 서명**: HMAC-SHA256 (`jwt.SigningMethodHS256`)
- **서명 키**: `argocd-secret`의 `server.secretkey` 필드 (`ServerSignature`)
- **Issuer**: `"argocd"` (`SessionManagerClaimsIssuer` 상수)

```go
// util/session/sessionmanager.go
const (
    SessionManagerClaimsIssuer = "argocd"
)

func (mgr *SessionManager) signClaims(claims jwt.Claims) (string, error) {
    token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
    settings, err := mgr.settingsMgr.GetSettings()
    if err != nil {
        return "", err
    }
    return token.SignedString(settings.ServerSignature)
}
```

서명 키는 `argocd-secret`에서 로드된다:

```go
// util/settings/settings.go
settingServerSignatureKey = "server.secretkey"

// updateSettingsFromSecret
secretKey, ok := argoCDSecret.Data[settingServerSignatureKey]
if ok {
    settings.ServerSignature = secretKey
}
```

### 1.2 SSO: Dex (내장 OIDC 프록시)

Dex는 Argo CD와 함께 배포되는 OIDC 프록시로, 다양한 외부 IdP를 OIDC 인터페이스로 통합한다.

- **지원 커넥터**: GitHub, Google, LDAP, SAML, Microsoft (OIDC), GitLab 등
- **설정 위치**: `argocd-cm`의 `dex.config` 키
- **통신 방식**: Dex는 gRPC (포트 5557)와 HTTPS (포트 5556)로 동작

```go
// util/dex/config.go
func GenerateDexConfigYAML(argocdSettings *settings.ArgoCDSettings, disableTLS bool) ([]byte, error) {
    var dexCfg map[string]any
    err = yaml.Unmarshal([]byte(argocdSettings.DexConfig), &dexCfg)
    dexCfg["issuer"] = argocdSettings.IssuerURL()
    dexCfg["storage"] = map[string]any{"type": "memory"}
    dexCfg["grpc"] = map[string]any{"addr": "0.0.0.0:5557"}
    // ...
}
```

Dex 설정 예시 (`argocd-cm`의 `dex.config`):

```yaml
connectors:
- type: github
  id: github
  name: GitHub
  config:
    clientID: $dex.github.clientId
    clientSecret: $dex.github.clientSecret
    orgs:
    - name: my-github-org
      teams:
      - my-team
```

### 1.3 외부 OIDC 프로바이더

Dex 없이 외부 OIDC 프로바이더를 직접 연결할 수도 있다. `argocd-cm`의 `oidc.config` 키에 설정한다.

```go
// util/settings/settings.go
type OIDCConfig struct {
    Name             string   `json:"name,omitempty"`
    Issuer           string   `json:"issuer,omitempty"`
    ClientID         string   `json:"clientID,omitempty"`
    ClientSecret     string   `json:"clientSecret,omitempty"`
    CLIClientID      string   `json:"cliClientID,omitempty"`
    RequestedScopes  []string `json:"requestedScopes,omitempty"`
    EnablePKCEAuthentication bool `json:"enablePKCEAuthentication,omitempty"`
    // ...
}
```

### 1.4 프로젝트 JWT 토큰

AppProject는 자체 JWT 토큰을 발급할 수 있다. CI/CD 파이프라인에서 특정 프로젝트에 제한된 액세스를 부여하는 데 사용한다.

- **Subject 형식**: `proj:{project}:{role}` (예: `proj:myproject:deploy-role`)
- **토큰 저장**: `AppProject.Status.JWTTokensByRole` (신버전) 및 `AppProject.Spec.Roles[].JWTTokens` (구버전 호환)

```go
// pkg/apis/application/v1alpha1/types.go
type JWTToken struct {
    IssuedAt  int64  `json:"iat" protobuf:"int64,1,opt,name=iat"`
    ExpiresAt int64  `json:"exp,omitempty" protobuf:"int64,2,opt,name=exp"`
    ID        string `json:"id,omitempty" protobuf:"bytes,3,opt,name=id"`
}
```

---

## 2. Casbin RBAC 모델

Argo CD의 RBAC 엔진은 [Casbin](https://casbin.org/)을 기반으로 한다. Casbin 모델은 `assets/model.conf`에 내장되어 있으며, `assets.ModelConf`로 로드된다.

### 2.1 모델 정의 (assets/model.conf)

```ini
[request_definition]
r = sub, res, act, obj

[policy_definition]
p = sub, res, act, obj, eft

[role_definition]
g = _, _

[policy_effect]
e = some(where (p.eft == allow)) && !some(where (p.eft == deny))

[matchers]
m = g(r.sub, p.sub) && globOrRegexMatch(r.res, p.res) && globOrRegexMatch(r.act, p.act) && globOrRegexMatch(r.obj, p.obj)
```

### 2.2 각 필드 의미

| 필드 | 설명 | 예시 |
|------|------|------|
| `sub` | 요청 주체 (사용자 또는 역할) | `admin`, `role:readonly`, `proj:myproj:dev` |
| `res` | 리소스 타입 | `applications`, `clusters`, `repositories` |
| `act` | 액션 | `get`, `create`, `update`, `delete`, `sync` |
| `obj` | 리소스 객체 (경로/이름) | `*/myapp`, `myproject/*`, `*` |
| `eft` | 효과 (allow/deny) | `allow`, `deny` |

### 2.3 정책 효과 규칙

```
e = some(where (p.eft == allow)) && !some(where (p.eft == deny))
```

이 규칙의 의미:
- **allow가 하나라도 있으면 허용** — 단, deny가 없을 때
- **deny가 하나라도 있으면 거부** — allow가 있어도 deny가 우선

이는 "명시적 거부가 허용을 오버라이드" 하는 보안 모델이다.

### 2.4 Glob vs Regex 매칭

Casbin 매처에서 `globOrRegexMatch` 커스텀 함수를 사용한다.

```go
// util/rbac/rbac.go
const (
    GlobMatchMode  = "glob"
    RegexMatchMode = "regex"
)

// globMatchFunc는 glob 패턴 매칭을 수행
func globMatchFunc(args ...any) (any, error) {
    val, ok := args[0].(string)
    pattern, ok := args[1].(string)
    return glob.Match(pattern, val), nil
}
```

| 모드 | 패턴 예시 | 설명 |
|------|-----------|------|
| glob (기본) | `myproject/*` | `*`는 단일 세그먼트 와일드카드 |
| glob | `*/myapp` | 모든 프로젝트의 myapp |
| regex | `^myproject/.*$` | 정규식 전체 매칭 |

매치 모드는 `argocd-rbac-cm`의 `policy.matchMode` 키로 설정한다.

---

## 3. Enforcer 구조체

`util/rbac/rbac.go`에 정의된 `Enforcer`는 Casbin 위에 구축된 Argo CD 전용 래퍼다.

### 3.1 Enforcer 구조체 정의

```go
// util/rbac/rbac.go L.127-140
type Enforcer struct {
    lock               sync.Mutex
    enforcerCache      *gocache.Cache       // TTL 1시간 캐시
    adapter            *argocdAdapter       // 정책 어댑터
    enableLog          bool
    enabled            bool
    clientset          kubernetes.Interface
    namespace          string
    configmap          string               // "argocd-rbac-cm"
    claimsEnforcerFunc ClaimsEnforcerFunc   // JWT 클레임 검사 함수
    model              model.Model          // Casbin 모델
    defaultRole        string               // 기본 역할 (예: role:readonly)
    matchMode          string               // "glob" 또는 "regex"
}
```

### 3.2 Enforcer 캐시 구조

```go
// cachedEnforcer holds the Casbin enforcer instances and optional custom project policy
type cachedEnforcer struct {
    enforcer CasbinEnforcer
    policy   string    // 프로젝트별 런타임 정책
}
```

캐시 키는 프로젝트 이름이며, TTL은 1시간이다:

```go
func NewEnforcer(...) *Enforcer {
    return &Enforcer{
        enforcerCache: gocache.New(time.Hour, time.Hour),
        // ...
    }
}
```

### 3.3 argocdAdapter (정책 레이어 구조)

```go
// util/rbac/rbac.go
type argocdAdapter struct {
    builtinPolicy     string    // 빌트인 정책 (role:readonly, role:admin 정의)
    userDefinedPolicy string    // argocd-rbac-cm의 policy.csv
    runtimePolicy     string    // AppProject 정책 (요청 시 동적 생성)
}

func (a *argocdAdapter) LoadPolicy(model model.Model) error {
    for _, policyStr := range []string{
        a.builtinPolicy,
        a.userDefinedPolicy,
        a.runtimePolicy,
    } {
        for line := range strings.SplitSeq(policyStr, "\n") {
            if err := loadPolicyLine(strings.TrimSpace(line), model); err != nil {
                return err
            }
        }
    }
    return nil
}
```

정책 우선순위 (낮은 번호가 먼저 로드, 나중 로드가 우선):

```
┌─────────────────────────────────────────┐
│  1. builtinPolicy (최우선 deny 가능)     │
│  2. userDefinedPolicy (관리자 정의)      │
│  3. runtimePolicy (AppProject 정책)      │
└─────────────────────────────────────────┘
```

단, `policy_effect`에서 deny가 allow보다 우선하므로, 어느 레이어에서든 deny가 있으면 최종 결과는 거부다.

### 3.4 tryGetCasbinEnforcer() — 프로젝트별 Enforcer 캐싱

```go
// util/rbac/rbac.go L.167-205
func (e *Enforcer) tryGetCasbinEnforcer(project string, policy string) (CasbinEnforcer, error) {
    e.lock.Lock()
    defer e.lock.Unlock()

    // 캐시 조회: 프로젝트 이름 + 정책 내용이 일치하면 캐시 히트
    var cached *cachedEnforcer
    val, ok := e.enforcerCache.Get(project)
    if ok {
        if c, ok := val.(*cachedEnforcer); ok && c.policy == policy {
            cached = c
        }
    }
    if cached != nil {
        return cached.enforcer, nil
    }

    // 캐시 미스: 새 enforcer 생성
    matchFunc := globMatchFunc
    if e.matchMode == RegexMatchMode {
        matchFunc = util.RegexMatchFunc
    }

    var enforcer CasbinEnforcer
    if policy != "" {
        // 프로젝트 정책이 있는 경우: builtinPolicy + userDefinedPolicy + projectPolicy
        enforcer, err = newEnforcerSafe(matchFunc, e.model,
            newAdapter(e.adapter.builtinPolicy, e.adapter.userDefinedPolicy, policy))
        if err != nil {
            // 프로젝트 정책이 유효하지 않으면 기본 어댑터로 폴백
            enforcer, err = newEnforcerSafe(matchFunc, e.model, e.adapter)
        }
    } else {
        enforcer, err = newEnforcerSafe(matchFunc, e.model, e.adapter)
    }

    enforcer.AddFunction("globOrRegexMatch", matchFunc)
    enforcer.EnableLog(e.enableLog)
    enforcer.EnableEnforce(e.enabled)

    // 캐시에 저장 (TTL: 1시간)
    e.enforcerCache.SetDefault(project, &cachedEnforcer{
        enforcer: enforcer,
        policy:   policy,
    })
    return enforcer, nil
}
```

캐시 무효화(`invalidateCache`)는 정책 변경, 매치모드 변경, 로그 설정 변경 시 발생한다.

### 3.5 CasbinEnforcer 인터페이스

```go
// util/rbac/rbac.go
type CasbinEnforcer interface {
    EnableLog(bool)
    Enforce(rvals ...any) (bool, error)
    LoadPolicy() error
    EnableEnforce(bool)
    AddFunction(name string, function govaluate.ExpressionFunction)
    GetGroupingPolicy() ([][]string, error)
    GetAllRoles() ([]string, error)
    GetImplicitPermissionsForUser(user string, domain ...string) ([][]string, error)
}
```

실제 구현은 `casbin.NewCachedEnforcer`로 생성된 `casbin.CachedEnforcer`가 담당한다.

---

## 4. 정책 형식 및 빌트인 역할

### 4.1 정책 형식

**프로젝트 범위 리소스** (applications, applicationsets, logs, exec, clusters, repositories):

```
p, <주체>, <리소스>, <액션>, <프로젝트>/<객체>, <allow|deny>
```

**글로벌 리소스** (clusters, projects, repositories, certificates, accounts, gpgkeys):

```
p, <주체>, <리소스>, <액션>, <객체>, <allow|deny>
```

**역할 상속**:

```
g, <주체>, <역할>
```

### 4.2 리소스 목록

`util/rbac/rbac.go`에 정의된 리소스 상수:

```go
const (
    ResourceClusters          = "clusters"
    ResourceProjects          = "projects"
    ResourceApplications      = "applications"
    ResourceApplicationSets   = "applicationsets"
    ResourceRepositories      = "repositories"
    ResourceWriteRepositories = "write-repositories"
    ResourceCertificates      = "certificates"
    ResourceAccounts          = "accounts"
    ResourceGPGKeys           = "gpgkeys"
    ResourceLogs              = "logs"
    ResourceExec              = "exec"
    ResourceExtensions        = "extensions"
)
```

**프로젝트 범위 리소스** (`ProjectScoped` 맵):

```go
var ProjectScoped = map[string]bool{
    ResourceApplications:    true,
    ResourceApplicationSets: true,
    ResourceLogs:            true,
    ResourceExec:            true,
    ResourceClusters:        true,
    ResourceRepositories:    true,
}
```

### 4.3 액션 목록

```go
const (
    ActionGet      = "get"
    ActionCreate   = "create"
    ActionUpdate   = "update"
    ActionDelete   = "delete"
    ActionSync     = "sync"
    ActionOverride = "override"
    ActionAction   = "action"
    ActionInvoke   = "invoke"
)
```

| 액션 | 설명 | 주요 대상 리소스 |
|------|------|------------------|
| `get` | 읽기 | 모든 리소스 |
| `create` | 생성 | applications, clusters, repositories |
| `update` | 수정 | applications, clusters, repositories |
| `delete` | 삭제 | applications, clusters, repositories |
| `sync` | 동기화 | applications |
| `override` | 파라미터 오버라이드 | applications |
| `action` | 커스텀 액션 실행 | applications |
| `invoke` | 확장 기능 호출 | extensions |

### 4.4 빌트인 역할 (assets/builtin-policy.csv)

```csv
# role:readonly — get 전용
p, role:readonly, applications, get, */*, allow
p, role:readonly, applicationsets, get, */*, allow
p, role:readonly, certificates, get, *, allow
p, role:readonly, clusters, get, *, allow
p, role:readonly, repositories, get, *, allow
p, role:readonly, write-repositories, get, *, allow
p, role:readonly, projects, get, *, allow
p, role:readonly, accounts, get, *, allow
p, role:readonly, gpgkeys, get, *, allow
p, role:readonly, logs, get, */*, allow

# role:admin — 전체 권한 (role:readonly 상속 포함)
p, role:admin, applications, create, */*, allow
p, role:admin, applications, update, */*, allow
p, role:admin, applications, update/*, */*, allow
p, role:admin, applications, delete, */*, allow
p, role:admin, applications, delete/*, */*, allow
p, role:admin, applications, sync, */*, allow
p, role:admin, applications, override, */*, allow
p, role:admin, applications, action/*, */*, allow
# ... (생략)

# 역할 상속
g, role:admin, role:readonly     # admin은 readonly의 모든 권한을 포함
g, admin, role:admin             # 로컬 admin 사용자는 role:admin 역할
```

### 4.5 정책 작성 예시

```csv
# dev 팀에게 myproject의 application sync 권한 부여
p, role:dev, applications, sync, myproject/*, allow

# ops 팀에게 모든 클러스터 관리 권한 부여
p, role:ops, clusters, create, *, allow
p, role:ops, clusters, update, *, allow
p, role:ops, clusters, delete, *, allow

# OIDC 그룹을 역할에 매핑
g, my-github-org:developers, role:dev
g, my-github-org:ops, role:ops

# 특정 사용자 직접 정책
p, alice, applications, get, */*, allow
p, alice, applications, sync, production/*, deny   # production은 sync 불가

# AppProject 역할 정책 (proj: 접두사)
p, proj:myproject:deploy-role, applications, sync, myproject/*, allow
```

---

## 5. enforce() 알고리즘

`util/rbac/rbac.go`의 `enforce()` 함수는 Casbin 검사 전에 추가 로직을 수행하는 핵심 함수다.

### 5.1 enforce() 소스 코드 (L.381-407)

```go
// enforce is a helper to additionally check a default role and invoke a custom claims enforcement function
func enforce(enf CasbinEnforcer, defaultRole string, claimsEnforcerFunc ClaimsEnforcerFunc, rvals ...any) bool {
    // 1단계: defaultRole 체크
    if defaultRole != "" && len(rvals) >= 2 {
        if ok, err := enf.Enforce(append([]any{defaultRole}, rvals[1:]...)...); ok && err == nil {
            return true
        }
    }

    if len(rvals) == 0 {
        return false
    }

    // 2단계: subject 타입 분기
    sub := rvals[0]
    switch s := sub.(type) {
    case string:
        // 일반 문자열 subject: 그대로 Casbin에 전달
    case jwt.Claims:
        // JWT 클레임: claimsEnforcerFunc 실행 (프로젝트 정책 포함)
        if claimsEnforcerFunc != nil && claimsEnforcerFunc(s, rvals...) {
            return true
        }
        rvals = append([]any{""}, rvals[1:]...)  // 빈 subject로 교체
    default:
        rvals = append([]any{""}, rvals[1:]...)
    }

    // 3단계: Casbin 직접 검사
    ok, err := enf.Enforce(rvals...)
    return ok && err == nil
}
```

### 5.2 enforce() 실행 흐름

```
enforce(enf, defaultRole, claimsEnforcerFunc, claims, "applications", "sync", "myproject/myapp")
│
├─ [1] defaultRole 체크
│   └─ enf.Enforce("role:readonly", "applications", "sync", "myproject/myapp") → false
│
├─ [2] subject 타입 확인: jwt.Claims
│   └─ claimsEnforcerFunc(claims, "applications", "sync", "myproject/myapp")
│       │
│       ├─ subject 추출: "alice"
│       ├─ getProjectFromRequest → AppProject "myproject" 조회
│       ├─ proj.ProjectPoliciesString() → 프로젝트 정책 로드
│       ├─ CreateEnforcerWithRuntimePolicy("myproject", projectPolicy)
│       │
│       ├─ enf.EnforceWithCustomEnforcer(enforcer, "alice", ...) → false?
│       └─ 그룹 순회: GetScopeValues → ["my-github-org:developers"]
│           └─ enf.EnforceWithCustomEnforcer(enforcer, "my-github-org:developers", ...) → true!
│
└─ return true
```

### 5.3 EnforceErr() — 상세 에러 메시지

```go
// util/rbac/rbac.go
func (e *Enforcer) EnforceErr(rvals ...any) error {
    if !e.Enforce(rvals...) {
        errMsg := "permission denied"

        if len(rvals) > 0 {
            rvalsStrs := make([]string, len(rvals)-1)
            for i, rval := range rvals[1:] {
                rvalsStrs[i] = fmt.Sprintf("%s", rval)
            }
            // JWT 클레임에서 사용자 ID와 발급 시간 추출
            if s, ok := rvals[0].(jwt.Claims); ok {
                claims, err := jwtutil.MapClaims(s)
                if err == nil {
                    userId := jwtutil.GetUserIdentifier(claims)
                    if userId != "" {
                        rvalsStrs = append(rvalsStrs, "sub: "+userId)
                    }
                    if issuedAtTime, err := jwtutil.IssuedAtTime(claims); err == nil {
                        rvalsStrs = append(rvalsStrs, "iat: "+issuedAtTime.Format(time.RFC3339))
                    }
                }
            }
            errMsg = fmt.Sprintf("%s: %s", errMsg, strings.Join(rvalsStrs, ", "))
        }
        return status.Error(codes.PermissionDenied, errMsg)
    }
    return nil
}
```

실패 시 에러 메시지 예시:
```
permission denied: applications, sync, myproject/myapp, sub: alice, iat: 2026-03-04T10:30:00Z
```

### 5.4 RBACPolicyEnforcer.EnforceClaims() — JWT 클레임 검사

`server/rbacpolicy/rbacpolicy.go`의 `EnforceClaims`가 `ClaimsEnforcerFunc`로 등록된다:

```go
// server/rbacpolicy/rbacpolicy.go
func (p *RBACPolicyEnforcer) EnforceClaims(claims jwt.Claims, rvals ...any) bool {
    mapClaims, err := jwtutil.MapClaims(claims)
    subject := jwtutil.GetUserIdentifier(mapClaims)

    // 프로젝트 JWT 토큰 처리 (proj:myproject:role 형식)
    var runtimePolicy string
    var projName string
    proj := p.getProjectFromRequest(rvals...)
    if proj != nil {
        if IsProjectSubject(subject) {
            // proj:* subject는 별도 enforceProjectToken 호출
            return p.enforceProjectToken(subject, proj, rvals...)
        }
        runtimePolicy = proj.ProjectPoliciesString()
        projName = proj.Name
    }

    enforcer := p.enf.CreateEnforcerWithRuntimePolicy(projName, runtimePolicy)

    // 1. subject 직접 검사 (예: "admin")
    vals := append([]any{subject}, rvals[1:]...)
    if p.enf.EnforceWithCustomEnforcer(enforcer, vals...) {
        return true
    }

    // 2. OIDC 그룹 검사 (scopes: ["groups"])
    groups := jwtutil.GetScopeValues(mapClaims, scopes)
    groupingPolicies, _ := enforcer.GetGroupingPolicy()
    for gidx := range groups {
        for gpidx := range groupingPolicies {
            // 정책에 정의된 그룹과만 비교 (최적화)
            if groupingPolicies[gpidx][0] == groups[gidx] {
                vals := append([]any{groups[gidx]}, rvals[1:]...)
                if p.enf.EnforceWithCustomEnforcer(enforcer, vals...) {
                    return true
                }
                break
            }
        }
    }
    return false
}
```

---

## 6. 정책 동적 로딩

### 6.1 RunPolicyLoader() (L.436-450)

```go
// util/rbac/rbac.go
func (e *Enforcer) RunPolicyLoader(ctx context.Context, onUpdated func(cm *corev1.ConfigMap) error) error {
    // 시작 시 즉시 ConfigMap 로드
    cm, err := e.clientset.CoreV1().ConfigMaps(e.namespace).Get(ctx, e.configmap, metav1.GetOptions{})
    if err != nil {
        if !apierrors.IsNotFound(err) {
            return err
        }
    } else {
        err = e.syncUpdate(cm, onUpdated)
        if err != nil {
            return err
        }
    }
    // Informer로 변경 감시 시작
    e.runInformer(ctx, onUpdated)
    return nil
}
```

### 6.2 newInformer() — ConfigMap 감시

```go
// util/rbac/rbac.go
func (e *Enforcer) newInformer() cache.SharedIndexInformer {
    tweakConfigMap := func(options *metav1.ListOptions) {
        cmFieldSelector := fields.ParseSelectorOrDie("metadata.name=" + e.configmap)
        options.FieldSelector = cmFieldSelector.String()
    }
    indexers := cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}
    return informersv1.NewFilteredConfigMapInformer(
        e.clientset,
        e.namespace,
        defaultRBACSyncPeriod,  // 10분
        indexers,
        tweakConfigMap,
    )
}
```

### 6.3 정책 리싱크 주기

```go
const defaultRBACSyncPeriod = 10 * time.Minute
```

10분 주기로 리싱크하되, ConfigMap 변경 이벤트 발생 시 즉시 반영한다.

### 6.4 PolicyCSV() — 멀티 파일 정책 병합

```go
// util/rbac/rbac.go
func PolicyCSV(data map[string]string) string {
    var strBuilder strings.Builder

    // 1. 메인 정책 (policy.csv) 먼저 추가
    if p, ok := data[ConfigMapPolicyCSVKey]; ok {
        strBuilder.WriteString(p)
    }

    keys := make([]string, 0, len(data))
    for k := range data {
        keys = append(keys, k)
    }
    sort.Strings(keys)  // 알파벳 순서로 정렬 (결정론적 순서)

    // 2. policy.*.csv 파일 알파벳 순으로 추가
    for _, key := range keys {
        value := data[key]
        if strings.HasPrefix(key, "policy.") &&
            strings.HasSuffix(key, ".csv") &&
            key != ConfigMapPolicyCSVKey {
            strBuilder.WriteString("\n")
            strBuilder.WriteString(value)
        }
    }
    return strBuilder.String()
}
```

`argocd-rbac-cm` ConfigMap 예시:
```yaml
data:
  policy.csv: |
    p, role:staging-admin, applications, *, staging/*, allow
    g, my-org:staging-team, role:staging-admin
  policy.prod.csv: |
    p, role:prod-admin, applications, *, production/*, allow
    g, my-org:prod-team, role:prod-admin
  policy.default: role:readonly
  policy.matchMode: glob
```

병합 순서: `policy.csv` → `policy.prod.csv` (알파벳 순)

### 6.5 syncUpdate() — ConfigMap 변경 처리

```go
// util/rbac/rbac.go
func (e *Enforcer) syncUpdate(cm *corev1.ConfigMap, onUpdated func(cm *corev1.ConfigMap) error) error {
    e.SetDefaultRole(cm.Data[ConfigMapPolicyDefaultKey])  // policy.default
    e.SetMatchMode(cm.Data[ConfigMapMatchModeKey])         // policy.matchMode
    policyCSV := PolicyCSV(cm.Data)
    if err := onUpdated(cm); err != nil {
        return err
    }
    return e.SetUserPolicy(policyCSV)  // 캐시 무효화 + 정책 재로드
}
```

### 6.6 ValidatePolicy() — 정책 검증

```go
// util/rbac/rbac.go
func ValidatePolicy(policy string) error {
    casbinEnforcer, err := newEnforcerSafe(globMatchFunc, newBuiltInModel(), newAdapter("", "", policy))
    if err != nil {
        return fmt.Errorf("policy syntax error: %s", policy)
    }
    // 역할 참조 무결성 검사 (정의되지 않은 역할 참조 경고)
    if err := CheckUserDefinedRoleReferentialIntegrity(casbinEnforcer); err != nil {
        log.Warning(err.Error())
    }
    return nil
}
```

정책 유효성 검사는 `argocd-rbac-cm`에 정책을 적용하기 전에 API 서버에서 호출한다.

---

## 7. SessionManager

`util/session/sessionmanager.go`의 `SessionManager`는 JWT 토큰 생성, 파싱, 검증, 자동 갱신을 담당한다.

### 7.1 SessionManager 구조체

```go
// util/session/sessionmanager.go
type SessionManager struct {
    settingsMgr                   *settings.SettingsManager
    projectsLister                v1alpha1.AppProjectNamespaceLister
    client                        *http.Client               // OIDC 검증용 HTTP 클라이언트
    prov                          oidcutil.Provider          // OIDC 프로바이더
    storage                       UserStateStorage           // 로그인 시도 + 토큰 취소 저장소
    sleep                         func(d time.Duration)      // 테스트 주입용
    verificationDelayNoiseEnabled bool                       // 타이밍 노이즈 활성화
    failedLock                    sync.RWMutex
    metricsRegistry               MetricsRegistry
}
```

### 7.2 UserStateStorage — Redis 기반 상태 저장소

```go
// util/session/state.go
type userStateStorage struct {
    attempts            map[string]LoginAttempts
    redis               *redis.Client
    revokedTokens       map[string]bool
    recentRevokedTokens map[string]bool
    lock                sync.RWMutex
    resyncDuration      time.Duration  // 15초
}
```

취소된 토큰(revoked tokens)은 Redis에 `revoked-token|{id}` 키로 저장되고, Redis Pub/Sub(`new-revoked-token` 채널)으로 모든 API 서버 인스턴스에 즉시 전파된다.

### 7.3 Create() — JWT 토큰 생성

```go
// util/session/sessionmanager.go
func (mgr *SessionManager) Create(subject string, secondsBeforeExpiry int64, id string) (string, error) {
    now := time.Now().UTC()
    claims := jwt.RegisteredClaims{
        IssuedAt:  jwt.NewNumericDate(now),
        Issuer:    SessionManagerClaimsIssuer,   // "argocd"
        NotBefore: jwt.NewNumericDate(now),
        Subject:   subject,
        ID:        id,
    }
    if secondsBeforeExpiry > 0 {
        expires := now.Add(time.Duration(secondsBeforeExpiry) * time.Second)
        claims.ExpiresAt = jwt.NewNumericDate(expires)
    }
    return mgr.signClaims(claims)
}
```

Subject 형식:
- 로컬 로그인: `"admin"` 또는 `"alice:login"`
- API 키: `"alice:apiKey"`
- 프로젝트 JWT: `"proj:myproject:deploy-role"`

### 7.4 Parse() — JWT 파싱 및 검증 (로컬 계정)

```go
// util/session/sessionmanager.go
func (mgr *SessionManager) Parse(tokenString string) (jwt.Claims, string, error) {
    var claims jwt.MapClaims

    // 1. HMAC-SHA256 서명 검증
    token, err := jwt.ParseWithClaims(tokenString, &claims, func(token *jwt.Token) (any, error) {
        if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
            return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
        }
        return argoCDSettings.ServerSignature, nil
    })

    // 2. 프로젝트 JWT 토큰 처리
    if projName, role, ok := rbacpolicy.GetProjectRoleFromSubject(subject); ok {
        proj, err := mgr.projectsLister.Get(projName)
        _, _, err = proj.GetJWTToken(role, issuedAt.Unix(), id)
        return token.Claims, "", nil
    }

    // 3. 로컬 계정 검증
    account, err := mgr.settingsMgr.GetAccount(subject)
    if !account.Enabled {
        return nil, "", fmt.Errorf("account %s is disabled", subject)
    }
    if !account.HasCapability(capability) {
        return nil, "", fmt.Errorf("account %s does not have '%s' capability", subject, capability)
    }

    // 4. 토큰 취소 여부 확인
    if id == "" || mgr.storage.IsTokenRevoked(id) {
        return nil, "", errors.New("token is revoked, please re-login")
    }

    // 5. 비밀번호 변경 시 토큰 무효화
    if account.PasswordMtime != nil && issuedAt.Before(*account.PasswordMtime) {
        return nil, "", errors.New("account password has changed since token issued")
    }

    // 6. 토큰 자동 갱신 (만료 5분 이내이면 새 토큰 발급)
    newToken := ""
    if exp, err := jwtutil.ExpirationTime(claims); err == nil {
        tokenExpDuration := exp.Sub(issuedAt)
        remainingDuration := time.Until(exp)
        if remainingDuration < autoRegenerateTokenDuration && capability == settings.AccountCapabilityLogin {
            if uniqueId, err := uuid.NewRandom(); err == nil {
                newToken, _ = mgr.Create(
                    fmt.Sprintf("%s:%s", subject, settings.AccountCapabilityLogin),
                    int64(tokenExpDuration.Seconds()),
                    uniqueId.String(),
                )
            }
        }
    }
    return token.Claims, newToken, nil
}
```

### 7.5 VerifyToken() — Issuer 기반 분기

```go
// util/session/sessionmanager.go
func (mgr *SessionManager) VerifyToken(ctx context.Context, tokenString string) (jwt.Claims, string, error) {
    // 먼저 클레임 파싱 (서명 검증 없이)
    parser := jwt.NewParser(jwt.WithoutClaimsValidation())
    claims := jwt.MapClaims{}
    _, _, err := parser.ParseUnverified(tokenString, &claims)

    // issuer 기반 분기
    issuer, _ := claims["iss"].(string)
    switch issuer {
    case SessionManagerClaimsIssuer:   // "argocd"
        // 로컬 계정 또는 프로젝트 JWT: Parse() 호출
        return mgr.Parse(tokenString)
    default:
        // OIDC 토큰: IDP 검증
        prov, err := mgr.provider()
        idToken, err := prov.Verify(ctx, tokenString, argoSettings)
        if err != nil {
            // 만료된 OIDC 토큰: UI가 처리할 수 있도록 issuer만 포함한 클레임 반환
            if errors.As(err, &tokenExpiredError) {
                claims = jwt.MapClaims{"iss": "sso"}
                return claims, "", common.ErrTokenVerification
            }
            return nil, "", common.ErrTokenVerification
        }
        var claims jwt.MapClaims
        err = idToken.Claims(&claims)
        return claims, "", nil
    }
}
```

### 7.6 토큰 자동 갱신 흐름

```
┌─────────────────────────────────────────────────────┐
│              토큰 자동 갱신 흐름                     │
│                                                     │
│  Parse()                                            │
│  │                                                  │
│  ├─ 남은 유효시간 < 5분?                             │
│  │   예: 토큰 TTL=24h, 남은 시간=3분                 │
│  │                                                  │
│  ├─ YES → uuid.NewRandom() 생성                     │
│  │         mgr.Create(subject, 24h, newUUID)        │
│  │         newToken = 새 JWT 문자열                  │
│  │                                                  │
│  └─ return (oldClaims, newToken, nil)               │
│                                                     │
│  Authenticate() (server.go)                         │
│  │                                                  │
│  └─ newToken != "" →                               │
│      grpc.SendHeader(ctx, renewTokenKey: newToken)  │
│      → 클라이언트가 쿠키 업데이트                    │
└─────────────────────────────────────────────────────┘
```

```go
const autoRegenerateTokenDuration = time.Minute * 5
```

---

## 8. 로그인 보안

### 8.1 VerifyUsernamePassword() — 다층 보안 (L.434-491)

```go
// util/session/sessionmanager.go
func (mgr *SessionManager) VerifyUsernamePassword(username string, password string) error {
    // [보안 1] 빈 비밀번호 즉시 거부
    if password == "" {
        return status.Errorf(codes.Unauthenticated, blankPasswordError)
    }

    // [보안 2] 사용자명 길이 제한 (캐시 메모리 공격 방지)
    if len(username) > maxUsernameLength {
        return status.Errorf(codes.InvalidArgument, usernameTooLongError, maxUsernameLength)
    }

    start := time.Now()

    // [보안 3] 타이밍 노이즈 (사용자 열거 공격 방지)
    if mgr.verificationDelayNoiseEnabled {
        defer func() {
            delayNanoseconds := verificationDelayNoiseMin.Nanoseconds() +
                int64(rand.Intn(int(
                    verificationDelayNoiseMax.Nanoseconds() - verificationDelayNoiseMin.Nanoseconds())))
            delayNanoseconds = delayNanoseconds - time.Since(start).Nanoseconds()
            if delayNanoseconds > 0 {
                mgr.sleep(time.Duration(delayNanoseconds))
            }
        }()
    }

    // [보안 4] 실패 횟수 초과 확인
    attempt := mgr.getFailureCount(username)
    if mgr.exceededFailedLoginAttempts(attempt) {
        return InvalidLoginErr
    }

    account, err := mgr.settingsMgr.GetAccount(username)
    if err != nil {
        if errStatus, ok := status.FromError(err); ok && errStatus.Code() == codes.NotFound {
            mgr.updateFailureCount(username, true)
            err = InvalidLoginErr
        }
        // [보안 5] 존재하지 않는 사용자도 bcrypt 수행 (응답 시간 일관성)
        _, _ = passwordutil.HashPassword("for_consistent_response_time")
        return err
    }

    valid, _ := passwordutil.VerifyPassword(password, account.PasswordHash)
    if !valid {
        mgr.updateFailureCount(username, true)
        return InvalidLoginErr
    }

    if !account.Enabled {
        return status.Errorf(codes.Unauthenticated, accountDisabled, username)
    }
    if !account.HasCapability(settings.AccountCapabilityLogin) {
        return status.Errorf(codes.Unauthenticated, userDoesNotHaveCapability, username, settings.AccountCapabilityLogin)
    }

    mgr.updateFailureCount(username, false)  // 성공 시 카운터 초기화
    return nil
}
```

### 8.2 보안 상수

```go
const (
    maxUsernameLength           = 32               // 사용자명 최대 길이
    defaultMaxCacheSize         = 10000            // 실패 캐시 최대 크기
    defaultMaxLoginFailures     = 5                // 최대 실패 횟수
    defaultFailureWindow        = 300              // 실패 윈도우 (초, 5분)
    verificationDelayNoiseMin   = 500 * time.Millisecond  // 최소 딜레이
    verificationDelayNoiseMax   = 1000 * time.Millisecond // 최대 딜레이
)
```

환경 변수로 재정의 가능:
```
ARGOCD_SESSION_FAILURE_MAX_FAIL_COUNT   (기본: 5)
ARGOCD_SESSION_FAILURE_WINDOW_SECONDS   (기본: 300)
ARGOCD_SESSION_MAX_CACHE_SIZE           (기본: 10000)
```

### 8.3 동시 로그인 요청 제한 (브루트포스 방지)

```go
// server/server.go
maxConcurrentLoginRequestsCount = 50  // 기본값

// 환경 변수로 재정의
maxConcurrentLoginRequestsCountEnv = "ARGOCD_MAX_CONCURRENT_LOGIN_REQUESTS_COUNT"

// 레이트 리미터 생성 (semaphore 기반)
// server/session/ratelimiter.go
func NewLoginRateLimiter(maxNumber int) func() (utilio.Closer, error) {
    semaphore := semaphore.NewWeighted(int64(maxNumber))
    return func() (utilio.Closer, error) {
        if !semaphore.TryAcquire(1) {
            log.Warnf("Exceeded number of concurrent login requests")
            return nil, session.InvalidLoginErr
        }
        return utilio.NewCloser(func() error {
            defer semaphore.Release(1)
            return nil
        }), nil
    }
}
```

HA 환경에서는 레플리카 수로 나누어 각 인스턴스의 허용량을 조정한다:
```go
if replicasCount > 1 {
    maxConcurrentLoginRequestsCount = maxConcurrentLoginRequestsCount / replicasCount
}
```

### 8.4 계정 비밀번호 해싱 (bcrypt)

```go
// util/password/password.go
var preferredHashers = []PasswordHasher{
    BcryptPasswordHasher{},
}

func (h BcryptPasswordHasher) HashPassword(password string) (string, error) {
    cost := max(h.Cost, bcrypt.DefaultCost)  // 최소 bcrypt.DefaultCost (10)
    hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), cost)
    return string(hashedPassword), err
}

func (h BcryptPasswordHasher) VerifyPassword(password, hashedPassword string) bool {
    err := bcrypt.CompareHashAndPassword([]byte(hashedPassword), []byte(password))
    return err == nil
}
```

`verifyPasswordWithHashers`는 다중 해시 알고리즘을 순서대로 시도하여 이전 알고리즘과의 하위 호환성을 유지한다. 이전 알고리즘(인덱스 > 0)으로 검증된 경우 `stale=true`를 반환하여 비밀번호 해시 업그레이드를 트리거할 수 있다.

---

## 9. updateFailureCount() 알고리즘

### 9.1 소스 코드 (L.350-398)

```go
// util/session/sessionmanager.go
func (mgr *SessionManager) updateFailureCount(username string, failed bool) {
    mgr.failedLock.Lock()
    defer mgr.failedLock.Unlock()

    failures := mgr.GetLoginFailures()

    // [단계 1] 만료된 항목 제거 (failure window 기반)
    if window := getLoginFailureWindow(); window > 0 {
        count := expireOldFailedAttempts(window, failures)
        if count > 0 {
            log.Infof("Expired %d entries from session cache due to max age reached", count)
        }
    }

    // [단계 2] 캐시 크기 초과 시 무작위 비-admin 항목 제거 (DoS 방지)
    if failed && len(failures) >= getMaximumCacheSize() {
        log.Warnf("Session cache size exceeds %d entries, removing random entry", getMaximumCacheSize())
        rmUser := pickRandomNonAdminLoginFailure(failures, username)
        if rmUser != nil {
            delete(failures, *rmUser)
        }
    }

    attempt, ok := failures[username]
    if !ok {
        attempt = LoginAttempts{FailCount: 0}
    }

    // [단계 3] 카운터 업데이트
    if failed {
        attempt.FailCount++
        attempt.LastFailed = time.Now()
        failures[username] = attempt
    } else if attempt.FailCount > 0 {
        // 성공 시 항목 삭제 (단순 0으로 재설정이 아닌 완전 제거)
        delete(failures, username)
    }

    mgr.storage.SetLoginAttempts(failures)
}
```

### 9.2 expireOldFailedAttempts() — 슬라이딩 윈도우 만료

```go
func expireOldFailedAttempts(maxAge time.Duration, failures map[string]LoginAttempts) int {
    expiredCount := 0
    for key, attempt := range failures {
        if time.Since(attempt.LastFailed) > maxAge*time.Second {
            expiredCount++
            delete(failures, key)
        }
    }
    return expiredCount
}
```

`defaultFailureWindow = 300`(초)이므로, 마지막 실패 후 5분이 지나면 해당 항목이 만료된다. 이를 통해 슬라이딩 윈도우 방식으로 이전 실패 기록이 자동 정리된다.

### 9.3 pickRandomNonAdminLoginFailure() — DoS 방지

```go
func pickRandomNonAdminLoginFailure(failures map[string]LoginAttempts, username string) *string {
    idx := rand.Intn(len(failures) - 1)
    i := 0
    for key := range failures {
        if i == idx {
            // admin과 현재 사용자는 보호 (재귀 호출로 다른 항목 선택)
            if key == common.ArgoCDAdminUsername || key == username {
                return pickRandomNonAdminLoginFailure(failures, username)
            }
            return &key
        }
        i++
    }
    return nil
}
```

공격자가 대량의 가짜 사용자명으로 실패 캐시를 채워 메모리를 고갈시키려 할 때, 무작위로 비-admin 항목을 제거하여 캐시 크기를 유지한다. `admin` 계정과 현재 로그인 시도 사용자는 절대 제거되지 않는다.

### 9.4 exceededFailedLoginAttempts() — 지수 백오프 없는 단순 임계값

```go
func (mgr *SessionManager) exceededFailedLoginAttempts(attempt LoginAttempts) bool {
    maxFails := getMaxLoginFailures()
    failureWindow := getLoginFailureWindow()

    inWindow := func() bool {
        if failureWindow == 0 || time.Since(attempt.LastFailed).Seconds() <= float64(failureWindow) {
            return true
        }
        return false
    }

    if attempt.FailCount >= maxFails && inWindow() {
        return true
    }
    return false
}
```

5회 실패 후 5분 윈도우 내에 있으면 모든 로그인 시도가 즉시 거부된다. 5분이 지나면 카운터가 만료되어 다시 시도 가능하다.

### 9.5 LoginAttempts 구조체

```go
type LoginAttempts struct {
    LastFailed time.Time `json:"lastFailed"`   // 마지막 실패 시각
    FailCount  int       `json:"failCount"`     // 연속 실패 횟수
}
```

---

## 10. AppProject RBAC

### 10.1 AppProject 정책 구조

```go
// pkg/apis/application/v1alpha1/types.go
type AppProjectSpec struct {
    SourceRepos  []string              `json:"sourceRepos,omitempty"`   // 허용된 Git 저장소
    Destinations []ApplicationDestination `json:"destinations,omitempty"` // 허용된 배포 대상
    Roles        []ProjectRole         `json:"roles,omitempty"`         // 프로젝트 역할 정의
    ClusterResourceWhitelist  []ClusterResourceRestrictionItem `json:"clusterResourceWhitelist,omitempty"`
    NamespaceResourceBlacklist []metav1.GroupKind `json:"namespaceResourceBlacklist,omitempty"`
    NamespaceResourceWhitelist []metav1.GroupKind `json:"namespaceResourceWhitelist,omitempty"`
    ClusterResourceBlacklist  []ClusterResourceRestrictionItem `json:"clusterResourceBlacklist,omitempty"`
    SyncWindows SyncWindows   `json:"syncWindows,omitempty"`           // 동기화 시간 제한
}

type ProjectRole struct {
    Name        string      `json:"name"`
    Description string      `json:"description,omitempty"`
    Policies    []string    `json:"policies,omitempty"`   // Casbin 정책 문자열
    JWTTokens   []JWTToken  `json:"jwtTokens,omitempty"`  // 발급된 JWT 토큰 목록
    Groups      []string    `json:"groups,omitempty"`     // OIDC 그룹 바인딩
}
```

### 10.2 ProjectPoliciesString() — 동적 정책 생성

```go
// pkg/apis/application/v1alpha1/app_project_types.go
func (proj *AppProject) ProjectPoliciesString() string {
    var policies []string
    for _, role := range proj.Spec.Roles {
        // 프로젝트 역할에 자동으로 'get' 정책 추가
        projectPolicy := fmt.Sprintf(
            "p, proj:%s:%s, projects, get, %s, allow",
            proj.Name, role.Name, proj.Name)
        policies = append(policies, projectPolicy)

        // 역할에 정의된 커스텀 정책 추가
        policies = append(policies, role.Policies...)

        // OIDC 그룹 → 프로젝트 역할 바인딩
        for _, groupName := range role.Groups {
            policies = append(policies, fmt.Sprintf(
                "g, %s, proj:%s:%s",
                groupName, proj.Name, role.Name))
        }
    }
    return strings.Join(policies, "\n")
}
```

AppProject YAML 예시:
```yaml
apiVersion: argoproj.io/v1alpha1
kind: AppProject
metadata:
  name: myproject
spec:
  sourceRepos:
  - 'https://github.com/my-org/my-repo'
  destinations:
  - namespace: myapp-*
    server: https://kubernetes.default.svc
  roles:
  - name: deploy-role
    description: CI/CD 파이프라인용 배포 역할
    policies:
    - p, proj:myproject:deploy-role, applications, sync, myproject/*, allow
    - p, proj:myproject:deploy-role, applications, get, myproject/*, allow
    jwtTokens:
    - iat: 1741094400
      id: "abc123"
    groups:
    - my-github-org:devops-team
```

생성되는 정책 문자열:
```
p, proj:myproject:deploy-role, projects, get, myproject, allow
p, proj:myproject:deploy-role, applications, sync, myproject/*, allow
p, proj:myproject:deploy-role, applications, get, myproject/*, allow
g, my-github-org:devops-team, proj:myproject:deploy-role
```

### 10.3 SyncWindows — 시간 기반 동기화 제한

```go
// pkg/apis/application/v1alpha1/types.go
type SyncWindow struct {
    Kind         string   `json:"kind,omitempty"`         // "allow" 또는 "deny"
    Schedule     string   `json:"schedule,omitempty"`     // Cron 표현식
    Duration     string   `json:"duration,omitempty"`     // "1h", "30m" 등
    Applications []string `json:"applications,omitempty"` // 적용 앱 목록
    Namespaces   []string `json:"namespaces,omitempty"`   // 적용 네임스페이스
    Clusters     []string `json:"clusters,omitempty"`     // 적용 클러스터
    ManualSync   bool     `json:"manualSync,omitempty"`   // 수동 sync 허용 여부
    TimeZone     string   `json:"timeZone,omitempty"`     // 타임존
}
```

SyncWindow 예시 (`production` 환경에서 업무 시간에만 sync 허용):
```yaml
syncWindows:
- kind: allow
  schedule: "0 9 * * 1-5"   # 평일 오전 9시
  duration: 8h
  applications: ['*']
  manualSync: true
```

### 10.4 리소스 화이트리스트/블랙리스트

```go
// pkg/apis/application/v1alpha1/app_project_types.go
func (proj AppProject) IsGroupKindNamePermitted(gk schema.GroupKind, name string, namespaced bool) bool {
    if namespaced {
        namespaceWhitelist := proj.Spec.NamespaceResourceWhitelist
        namespaceBlacklist := proj.Spec.NamespaceResourceBlacklist

        isWhiteListed = namespaceWhitelist == nil || len(namespaceWhitelist) != 0 &&
            isResourceInList(res, namespaceWhitelist)
        isBlackListed = len(namespaceBlacklist) != 0 && isResourceInList(res, namespaceBlacklist)
        return isWhiteListed && !isBlackListed
    }

    clusterWhitelist := proj.Spec.ClusterResourceWhitelist
    clusterBlacklist := proj.Spec.ClusterResourceBlacklist

    // 클러스터 리소스: 화이트리스트에 명시적으로 있어야 허용
    isWhiteListed = len(clusterWhitelist) != 0 && isNamedResourceInList(res, name, clusterWhitelist)
    isBlackListed = len(clusterBlacklist) != 0 && isNamedResourceInList(res, name, clusterBlacklist)
    return isWhiteListed && !isBlackListed
}
```

네임스페이스 리소스는 "화이트리스트가 없으면 전체 허용, 블랙리스트에 있으면 거부"이고, 클러스터 리소스는 "화이트리스트에 명시적으로 있어야만 허용"이다.

---

## 11. 보안 패턴

### 11.1 타이밍 어택 방지 — getAppEnforceRBAC()

```go
// server/application/application.go
func (s *Server) getAppEnforceRBAC(ctx context.Context, action, project, namespace, name string,
    getApp func() (*v1alpha1.Application, error)) (*v1alpha1.Application, *v1alpha1.AppProject, error) {

    if project != "" {
        givenRBACName := security.RBACName(s.ns, project, namespace, name)
        if err := s.enf.EnforceErr(ctx.Value("claims"), rbac.ResourceApplications, action, givenRBACName); err != nil {
            // 타이밍 균등화: 접근 거부 시에도 getApp()을 실행하여 응답 시간을 일관되게 유지
            _, _ = getApp()
            return nil, nil, argocommon.PermissionDeniedAPIError
        }
    }

    a, err := getApp()
    if err != nil {
        if apierrors.IsNotFound(err) {
            if project != "" {
                return nil, nil, status.Error(codes.NotFound, ...)
            }
            // project가 없으면 404 대신 403 반환 (존재 여부 은폐)
            return nil, nil, argocommon.PermissionDeniedAPIError
        }
        return nil, nil, argocommon.PermissionDeniedAPIError
    }

    // 실제 앱의 프로젝트로 RBAC 재검사 (요청 project != 실제 project 차이 방지)
    if err := s.enf.EnforceErr(ctx.Value("claims"), rbac.ResourceApplications, action, a.RBACName(s.ns)); err != nil {
        if project != "" {
            return nil, nil, status.Error(codes.NotFound, ...)  // 존재 은폐
        }
        return nil, nil, argocommon.PermissionDeniedAPIError
    }

    // 요청 project != 실제 project: 404 반환
    if project != "" && effectiveProject != project {
        return nil, nil, status.Error(codes.NotFound, ...)
    }
    // ...
}
```

이 패턴의 목적:
1. **정보 은폐**: 접근 권한이 없는 사용자에게 앱의 존재 여부를 숨김 (403 대신 404)
2. **타이밍 균등화**: 접근 거부 시에도 DB 조회를 수행하여 응답 시간으로 앱 존재 여부를 추측하지 못하게 함

### 11.2 sensitiveMethods 로그 마스킹

```go
// server/server.go
sensitiveMethods := map[string]bool{
    "/cluster.ClusterService/Create":                               true,
    "/cluster.ClusterService/Update":                               true,
    "/session.SessionService/Create":                               true,   // 로그인 (비밀번호 포함)
    "/account.AccountService/UpdatePassword":                       true,
    "/gpgkey.GPGKeyService/CreateGnuPGPublicKey":                   true,
    "/repository.RepositoryService/Create":                         true,
    "/repository.RepositoryService/Update":                         true,
    // ... (저장소 자격증명 관련 메서드들)
    "/application.ApplicationService/PatchResource":                true,
    "/application.ApplicationService/GetManifestsWithFiles":        true,  // 크기도 크고 민감
}

// gRPC 페이로드 로깅 인터셉터에서 민감한 메서드는 제외
grpc_util.PayloadUnaryServerInterceptor(server.log, true, func(_ context.Context, c interceptors.CallMeta) bool {
    return !sensitiveMethods[c.FullMethod()]  // 민감 메서드는 false 반환 → 로깅 스킵
}),
```

### 11.3 서명 키 관리 (argocd-secret)

서명 키(`server.secretkey`)는 `argocd-secret` Kubernetes Secret에 저장된다.

```go
// util/settings/settings.go
settingServerSignatureKey = "server.secretkey"

// 키가 없으면 자동 생성 후 저장
if cdSettings.ServerSignature == nil {
    // 새 서명 키 생성
    cdSettings.ServerSignature = signature
    argoCDSecret.Data[settingServerSignatureKey] = settings.ServerSignature
}
```

키가 변경되면 모든 기존 로컬 JWT 토큰이 즉시 무효화된다.

### 11.4 RBAC 이중 검사 패턴

앱 관련 API는 두 번의 RBAC 검사를 수행한다:

```
1차 검사: 사용자가 제공한 project + name으로 사전 확인
    → 빠른 실패 (시간 균등화 포함)

2차 검사: 실제 앱의 RBACName(project/app)으로 재확인
    → 요청에 기재된 project와 실제 project가 다른 경우 방어
```

`RBACName`은 앱의 실제 프로젝트와 이름을 조합한다:
```go
// RBACName은 "{namespace}/{project}/{name}" 또는 "{project}/{name}" 형식
func (a *Application) RBACName(defaultNamespace string) string { ... }
```

### 11.5 AnonymousUser 지원

```go
// server/server.go Authenticate()
if claimsErr != nil {
    argoCDSettings, _ := server.settingsMgr.GetSettings()
    if !argoCDSettings.AnonymousUserEnabled {
        return ctx, claimsErr   // 인증 실패 시 즉시 거부
    }
    // AnonymousUser가 활성화된 경우: 빈 클레임으로 계속
    ctx = context.WithValue(ctx, "claims", "")
}
```

`AnonymousUserEnabled`가 true이면 인증 없이도 API를 호출할 수 있으며, RBAC의 `defaultRole`(예: `role:readonly`)이 적용된다.

---

## 12. 전체 인증/인가 흐름 요약

### 12.1 로그인 → 토큰 발급 흐름

```
클라이언트                  API 서버                    Dex/IDP
    │                          │                           │
    │─── POST /api/v1/session ─→│                           │
    │     {username, password}  │                           │
    │                          │                           │
    │                    [로컬 계정]                         │
    │                    VerifyUsernamePassword()           │
    │                    - bcrypt 검증                      │
    │                    - 실패 카운터 체크                  │
    │                    - 타이밍 노이즈 추가                │
    │                          │                           │
    │                    [SSO 계정]                         │
    │                    ─── OAuth2 redirect ──────────────→│
    │                          │                           │
    │←─── redirect to Dex ─────│                           │
    │                                                       │
    │──────────────── OAuth2 flow ─────────────────────────→│
    │                                                       │
    │←──────────────── ID Token (OIDC) ────────────────────│
    │                                                       │
    │─── POST /auth/callback (ID Token) ─→│               │
    │                          │                           │
    │                    VerifyToken()                      │
    │                    - issuer 확인                      │
    │                    - OIDC 검증 또는 argocd Parse()    │
    │                          │                           │
    │                    Create() JWT 발급                  │
    │                    - HS256 서명                        │
    │                    - iss: "argocd"                    │
    │                          │                           │
    │←─── JWT 토큰 (쿠키) ──────│                           │
```

### 12.2 API 요청 → RBAC 검사 흐름

```
클라이언트                  API 서버                    Casbin
    │                          │                           │
    │─── gRPC 요청 + JWT ──────→│                           │
    │                          │                           │
    │                    Authenticate() 미들웨어            │
    │                    - JWT 추출 (메타데이터)             │
    │                    - VerifyToken() 호출               │
    │                    - ctx에 claims 저장                │
    │                          │                           │
    │                    서비스 핸들러 (예: Get Application) │
    │                    │                                  │
    │                    getAppEnforceRBAC()                │
    │                    │                                  │
    │                    EnforceErr(claims, "applications", │
    │                              "get", "proj/appname")   │
    │                    │                                  │
    │                    enforce()                          │
    │                    ├─ [1] defaultRole 체크            │
    │                    ├─ [2] claimsEnforcerFunc 호출     │
    │                    │   ├─ subject 검사                │
    │                    │   ├─ AppProject 정책 로드         │
    │                    │   └─ OIDC 그룹 검사              │
    │                    └─ [3] Casbin.Enforce()────────────→│
    │                                                       │
    │                                               정책 매칭│
    │                                               g(r.sub, p.sub)
    │                                               &&      │
    │                                               globOrRegexMatch(...)
    │                                                       │
    │                                          허용/거부 반환│
    │←───────────────────────────────────────────────────── │
    │                          │                           │
    │                    응답 반환 또는 PermissionDenied     │
    │←─── 응답 ────────────────│                           │
```

### 12.3 정책 로드 흐름

```
argocd-rbac-cm ConfigMap 변경
        │
        ↓
informersv1.NewFilteredConfigMapInformer (10분 리싱크)
        │
        ↓
syncUpdate(cm, onUpdated)
        │
        ├─ SetDefaultRole(cm.Data["policy.default"])
        ├─ SetMatchMode(cm.Data["policy.matchMode"])
        ├─ PolicyCSV(cm.Data) → policy.csv + policy.*.csv 병합
        └─ SetUserPolicy(mergedCSV)
               │
               ↓
        invalidateCache()
        adapter.userDefinedPolicy = mergedCSV
               │
               ↓
        다음 enforce() 호출 시 tryGetCasbinEnforcer() → 새 enforcer 생성
```

### 12.4 컴포넌트 관계 다이어그램

```
┌────────────────────────────────────────────────────────────────────┐
│                        API Server                                   │
│                                                                    │
│  ┌──────────────┐    ┌────────────────┐    ┌─────────────────┐    │
│  │ Authenticate │    │ SessionManager │    │    Enforcer     │    │
│  │  (미들웨어)  │───→│                │    │                 │    │
│  │              │    │ VerifyToken()  │    │ enforce()       │    │
│  │ grpc_auth    │    │ Create()       │    │ EnforceErr()    │    │
│  │ interceptor  │    │ Parse()        │    │ enforcerCache   │    │
│  └──────────────┘    └───────┬────────┘    └────────┬────────┘    │
│                              │                      │             │
│                    ┌─────────┴──────────┐  ┌───────┴──────────┐  │
│                    │  UserStateStorage  │  │ argocdAdapter    │  │
│                    │  (Redis)           │  │                  │  │
│                    │ - revokedTokens   │  │ - builtinPolicy  │  │
│                    │ - loginAttempts   │  │ - userDefinedPol │  │
│                    └────────────────────┘  │ - runtimePolicy  │  │
│                                            └──────────────────┘  │
│                                                      ↑            │
│                                            ┌─────────┴──────────┐ │
│                                            │ RunPolicyLoader    │ │
│                                            │ (ConfigMap 감시)   │ │
│                                            └────────────────────┘ │
└────────────────────────────────────────────────────────────────────┘
         │                    │                    │
         ↓                    ↓                    ↓
   argocd-secret        Redis Cache          argocd-rbac-cm
  (server.secretkey)   (revoked tokens,    (policy.csv,
                        login attempts)     policy.default,
                                            policy.matchMode)
```

---

## 부록: 주요 환경 변수

| 환경 변수 | 기본값 | 설명 |
|-----------|--------|------|
| `ARGOCD_SESSION_FAILURE_MAX_FAIL_COUNT` | 5 | 로그인 실패 임계값 |
| `ARGOCD_SESSION_FAILURE_WINDOW_SECONDS` | 300 | 실패 윈도우 (초) |
| `ARGOCD_SESSION_MAX_CACHE_SIZE` | 10000 | 실패 캐시 최대 크기 |
| `ARGOCD_MAX_CONCURRENT_LOGIN_REQUESTS_COUNT` | 50 | 동시 로그인 요청 최대 수 |
| `ARGOCD_SSO_DEBUG` | "" | SSO 디버그 로깅 활성화 |

## 부록: 주요 파일 경로 참조

| 파일 | 역할 |
|------|------|
| `util/rbac/rbac.go` | Casbin 기반 RBAC 엔진 (`Enforcer`, `argocdAdapter`) |
| `util/session/sessionmanager.go` | JWT 토큰 생성/파싱/검증, 로그인 보안 |
| `util/session/state.go` | Redis 기반 상태 저장소 (취소된 토큰, 로그인 시도) |
| `util/session/ratelimiter.go` | 동시 로그인 요청 레이트 리미터 |
| `util/password/password.go` | bcrypt 비밀번호 해싱/검증 |
| `util/settings/accounts.go` | 로컬 계정 타입 정의 (`Account`, `AccountCapability`) |
| `util/settings/settings.go` | `ArgoCDSettings`, `OIDCConfig`, `ServerSignature` |
| `server/rbacpolicy/rbacpolicy.go` | `RBACPolicyEnforcer`, JWT 클레임 → RBAC 변환 |
| `server/application/application.go` | `getAppEnforceRBAC()` 보안 패턴 |
| `server/server.go` | `Authenticate()`, `sensitiveMethods`, 레이트 리미터 |
| `assets/model.conf` | Casbin 모델 정의 |
| `assets/builtin-policy.csv` | `role:readonly`, `role:admin` 빌트인 정책 |
| `pkg/apis/application/v1alpha1/app_project_types.go` | `ProjectPoliciesString()`, `GetJWTToken()` |
