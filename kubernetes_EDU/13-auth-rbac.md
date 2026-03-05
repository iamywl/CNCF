# 13. 인증·인가 심화 (Authentication & Authorization Deep-Dive)

## 목차

1. [개요](#1-개요)
2. [인증(Authentication) 아키텍처](#2-인증authentication-아키텍처)
3. [Authenticator 인터페이스](#3-authenticator-인터페이스)
4. [인증 체인 (Union Authentication)](#4-인증-체인-union-authentication)
5. [토큰 인증 유형](#5-토큰-인증-유형)
6. [인증 필터 (WithAuthentication)](#6-인증-필터-withauthentication)
7. [인가(Authorization) 아키텍처](#7-인가authorization-아키텍처)
8. [Authorizer 인터페이스와 Decision](#8-authorizer-인터페이스와-decision)
9. [인가 체인 (Union Authorization)](#9-인가-체인-union-authorization)
10. [RBAC Authorizer 심화](#10-rbac-authorizer-심화)
11. [Rule Resolver와 Subject 매칭](#11-rule-resolver와-subject-매칭)
12. [RuleAllows: 규칙 매칭 알고리즘](#12-ruleallows-규칙-매칭-알고리즘)
13. [인가 필터 (WithAuthorization)](#13-인가-필터-withauthorization)
14. [Handler Chain 통합](#14-handler-chain-통합)
15. [User/Group 모델](#15-usergroup-모델)
16. [설계 원칙: Why](#16-설계-원칙-why)
17. [정리](#17-정리)

---

## 1. 개요

Kubernetes API Server는 **모든 요청**에 대해 인증(Authentication)과 인가(Authorization)를
순차적으로 수행한다. 이 두 단계는 HTTP 핸들러 체인(filter chain)에 미들웨어 형태로 삽입되며,
각각 플러거블한 체인 구조를 가진다.

```
HTTP 요청
   |
   v
+------------------+     +-------------------+     +------------------+
| WithAuthentication| --> | WithAuthorization  | --> | Admission Control|
| (인증 필터)       |     | (인가 필터)         |     | (어드미션)        |
+------------------+     +-------------------+     +------------------+
   |                         |                         |
   | 인증 실패 → 401         | 인가 거부 → 403          | 거부 → 에러
   v                         v                         v
```

**핵심 소스 경로:**

| 구성요소 | 소스 경로 |
|---------|----------|
| Authenticator 인터페이스 | `staging/src/k8s.io/apiserver/pkg/authentication/authenticator/interfaces.go` |
| 인증 체인 (Union) | `staging/src/k8s.io/apiserver/pkg/authentication/request/union/union.go` |
| Authorizer 인터페이스 | `staging/src/k8s.io/apiserver/pkg/authorization/authorizer/interfaces.go` |
| 인가 체인 (Union) | `staging/src/k8s.io/apiserver/pkg/authorization/union/union.go` |
| RBAC Authorizer | `plugin/pkg/auth/authorizer/rbac/rbac.go` |
| Rule Resolver | `pkg/registry/rbac/validation/rule.go` |
| 인증 필터 | `staging/src/k8s.io/apiserver/pkg/endpoints/filters/authentication.go` |
| 인가 필터 | `staging/src/k8s.io/apiserver/pkg/endpoints/filters/authorization.go` |
| User 인터페이스 | `staging/src/k8s.io/apiserver/pkg/authentication/user/user.go` |

---

## 2. 인증(Authentication) 아키텍처

### 2.1 인증이란

인증은 "이 요청을 보낸 사람이 누구인가?"를 결정하는 과정이다.
Kubernetes는 단일 인증 방식을 강제하지 않고, 여러 인증기(authenticator)를
체인으로 연결하여 **하나라도 성공하면** 인증된 것으로 간주한다.

### 2.2 전체 인증 흐름

```
HTTP 요청 (Authorization 헤더, 클라이언트 인증서, 토큰 등)
   |
   v
+-------------------------------------------+
|       unionAuthRequestHandler             |
|  +------+  +------+  +------+  +------+  |
|  | X.509|  | Token|  | OIDC |  | Hook |  |
|  +------+  +------+  +------+  +------+  |
|  순서대로 시도, 첫 번째 성공 시 반환        |
+-------------------------------------------+
   |
   | 성공: (Response{User, Audiences}, true, nil)
   | 실패: (nil, false, aggregate error)
   v
Context에 user.Info 저장
```

### 2.3 인증 결과

인증이 성공하면 `authenticator.Response` 구조체가 반환된다:

```go
// staging/src/k8s.io/apiserver/pkg/authentication/authenticator/interfaces.go (58-65행)
type Response struct {
    Audiences Audiences
    User      user.Info
}
```

`User` 필드에는 사용자 이름, UID, 그룹, 추가 정보가 담긴다.
이 정보가 이후 인가 단계에서 사용된다.

---

## 3. Authenticator 인터페이스

### 3.1 Request 인터페이스

```go
// staging/src/k8s.io/apiserver/pkg/authentication/authenticator/interfaces.go (34-36행)
type Request interface {
    AuthenticateRequest(req *http.Request) (*Response, bool, error)
}
```

| 반환값 | 의미 |
|-------|------|
| `(*Response, true, nil)` | 인증 성공 — User 정보 반환 |
| `(nil, false, nil)` | 이 인증기로는 판단 불가 — 다음으로 넘김 |
| `(nil, false, error)` | 인증 시도 중 오류 발생 |

### 3.2 Token 인터페이스

```go
// staging/src/k8s.io/apiserver/pkg/authentication/authenticator/interfaces.go (28-30행)
type Token interface {
    AuthenticateToken(ctx context.Context, token string) (*Response, bool, error)
}
```

`Token` 인터페이스는 HTTP 요청이 아닌 **토큰 문자열**을 직접 받는다.
Bearer 토큰 인증기가 HTTP 요청에서 토큰을 추출한 뒤
이 인터페이스의 구현체에 위임한다.

### 3.3 함수형 어댑터

```go
// interfaces.go (39-43행)
type TokenFunc func(ctx context.Context, token string) (*Response, bool, error)

func (f TokenFunc) AuthenticateToken(ctx context.Context, token string) (*Response, bool, error) {
    return f(ctx, token)
}
```

```go
// interfaces.go (47-51행)
type RequestFunc func(req *http.Request) (*Response, bool, error)

func (f RequestFunc) AuthenticateRequest(req *http.Request) (*Response, bool, error) {
    return f(req)
}
```

함수를 직접 `Request` 또는 `Token` 인터페이스로 변환할 수 있다.
이는 Go의 함수형 어댑터 패턴으로, 테스트나 간단한 인증기 구현 시 유용하다.

---

## 4. 인증 체인 (Union Authentication)

### 4.1 핵심 구조체

```go
// staging/src/k8s.io/apiserver/pkg/authentication/request/union/union.go (27-32행)
type unionAuthRequestHandler struct {
    Handlers    []authenticator.Request
    FailOnError bool
}
```

| 필드 | 설명 |
|------|------|
| `Handlers` | 인증기 체인 (순서대로 시도) |
| `FailOnError` | true이면 에러 발생 시 즉시 중단, false이면 에러를 모아서 계속 |

### 4.2 생성자

```go
// union.go (36-41행)
func New(authRequestHandlers ...authenticator.Request) authenticator.Request {
    if len(authRequestHandlers) == 1 {
        return authRequestHandlers[0]
    }
    return &unionAuthRequestHandler{Handlers: authRequestHandlers, FailOnError: false}
}
```

핸들러가 1개뿐이면 래핑하지 않고 그대로 반환한다 (불필요한 간접 호출 제거).

### 4.3 체인 실행 로직

```go
// union.go (53-71행)
func (authHandler *unionAuthRequestHandler) AuthenticateRequest(req *http.Request) (
    *authenticator.Response, bool, error) {
    var errlist []error
    for _, currAuthRequestHandler := range authHandler.Handlers {
        resp, ok, err := currAuthRequestHandler.AuthenticateRequest(req)
        if err != nil {
            if authHandler.FailOnError {
                return resp, ok, err       // 즉시 에러 반환
            }
            errlist = append(errlist, err) // 에러 누적
            continue
        }
        if ok {
            return resp, ok, err           // 첫 번째 성공 시 반환
        }
    }
    return nil, false, utilerrors.NewAggregate(errlist)
}
```

#### 실행 흐름 다이어그램

```
AuthenticateRequest(req)
  |
  for each handler:
  |   |
  |   +-- handler.AuthenticateRequest(req)
  |   |     |
  |   |     +-- err != nil?
  |   |     |     |-- FailOnError? → 즉시 반환 (에러)
  |   |     |     +-- 아니면 errlist에 추가, continue
  |   |     |
  |   |     +-- ok == true? → 성공! 즉시 반환
  |   |     |
  |   |     +-- ok == false? → 다음 핸들러로
  |   |
  +-- 모든 핸들러 실패 → (nil, false, aggregate error)
```

### 4.4 FailOnError 변형

```go
// union.go (45-50행)
func NewFailOnError(authRequestHandlers ...authenticator.Request) authenticator.Request {
    if len(authRequestHandlers) == 1 {
        return authRequestHandlers[0]
    }
    return &unionAuthRequestHandler{Handlers: authRequestHandlers, FailOnError: true}
}
```

`NewFailOnError`는 에러 발생 시 즉시 단락(short-circuit)한다.
보안이 중요한 시나리오에서 에러를 무시하고 다음 인증기로 넘기는 것을 방지한다.

---

## 5. 토큰 인증 유형

Kubernetes는 다양한 토큰 인증 방식을 지원한다:

### 5.1 인증 유형 일람

| 인증 유형 | 방식 | 설명 |
|----------|------|------|
| **X.509 클라이언트 인증서** | TLS handshake | CN을 사용자명, O를 그룹으로 매핑 |
| **Static Token** | Bearer 토큰 | 파일 기반 정적 토큰 (--token-auth-file) |
| **Service Account Token** | JWT | Pod에 자동 마운트되는 서비스 어카운트 토큰 |
| **OIDC Token** | Bearer 토큰 | OpenID Connect 호환 IdP에서 발급 |
| **Webhook Token** | Bearer 토큰 | 외부 서비스에 인증 위임 |
| **Bootstrap Token** | Bearer 토큰 | 클러스터 부트스트랩용 단기 토큰 |
| **Request Header** | 프록시 헤더 | 인증 프록시가 설정한 X-Remote-User 등 |

### 5.2 Service Account Token 인증

Service Account 토큰은 Kubernetes에서 가장 많이 사용되는 인증 방식이다.
Pod가 생성되면 자동으로 서비스 어카운트 토큰이 마운트되며,
이 JWT를 Bearer 토큰으로 API Server에 전달한다.

```
Pod 내부:
/var/run/secrets/kubernetes.io/serviceaccount/token
   |
   v
HTTP Header: Authorization: Bearer <JWT>
   |
   v
API Server → ServiceAccount Token Authenticator
   |
   v
user.Info{
    Name:   "system:serviceaccount:<namespace>:<sa-name>",
    Groups: ["system:serviceaccounts", "system:serviceaccounts:<namespace>"],
}
```

### 5.3 OIDC 인증

OIDC(OpenID Connect) 인증은 외부 IdP(Identity Provider)와 연동한다:

```
사용자 → IdP (Google, Dex, Keycloak 등) → ID Token (JWT) 발급
   |
   v
kubectl --token=<OIDC JWT>
   |
   v
API Server → OIDC Authenticator
   |  1) JWT 서명 검증 (IdP의 JWKS 사용)
   |  2) 발급자(iss) 검증
   |  3) 클레임에서 사용자명/그룹 추출
   v
user.Info 생성
```

### 5.4 Webhook Token 인증

외부 서비스에 인증을 위임하는 패턴:

```
API Server → POST https://auth-webhook.example.com/authenticate
   |
   | 요청 본문:
   |   { "apiVersion": "authentication.k8s.io/v1",
   |     "kind": "TokenReview",
   |     "spec": { "token": "<bearer-token>" } }
   |
   v
외부 서비스 → TokenReview Status 반환
   |   authenticated: true/false
   |   user: { username, uid, groups, extra }
```

---

## 6. 인증 필터 (WithAuthentication)

### 6.1 핸들러 체인에의 통합

```go
// staging/src/k8s.io/apiserver/pkg/endpoints/filters/authentication.go (46-48행)
func WithAuthentication(handler http.Handler, auth authenticator.Request,
    failed http.Handler, apiAuds authenticator.Audiences,
    requestHeaderConfig *authenticatorfactory.RequestHeaderConfig) http.Handler {
    return withAuthentication(handler, auth, failed, apiAuds,
        requestHeaderConfig, recordAuthenticationMetrics)
}
```

### 6.2 내부 처리 로직

```go
// authentication.go (50-125행)
func withAuthentication(...) http.Handler {
    if auth == nil {
        klog.Warning("Authentication is disabled")
        return handler
    }
    // ...
    return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
        authenticationStart := time.Now()

        // Audience 설정
        if len(apiAuds) > 0 {
            req = req.WithContext(authenticator.WithAudiences(req.Context(), apiAuds))
        }

        // 인증 수행
        resp, ok, err := auth.AuthenticateRequest(req)

        // 메트릭 기록
        authenticationFinish := time.Now()
        // ...

        // 실패 처리
        if err != nil || !ok {
            failed.ServeHTTP(w, req)   // 401 Unauthorized
            return
        }

        // Audience 검증
        if !audiencesAreAcceptable(apiAuds, resp.Audiences) {
            failed.ServeHTTP(w, req)
            return
        }

        // Authorization 헤더 제거 (하위 핸들러로의 전파 방지)
        req.Header.Del("Authorization")

        // 프록시 헤더 정리
        headerrequest.ClearAuthenticationHeaders(...)

        // HTTP/2 DoS 완화 (미인증 사용자)
        if ... && isAnonymousUser(resp.User) {
            w.Header().Set("Connection", "close")
        }

        // Context에 User 정보 저장
        req = req.WithContext(genericapirequest.WithUser(req.Context(), resp.User))

        // 다음 핸들러로 전달
        handler.ServeHTTP(w, req)
    })
}
```

### 6.3 핵심 동작 정리

| 단계 | 동작 | 실패 시 |
|------|------|--------|
| 1 | `auth.AuthenticateRequest(req)` 호출 | 401 반환 |
| 2 | Audience 교차 검증 | 401 반환 |
| 3 | Authorization 헤더 삭제 | - |
| 4 | 프록시 인증 헤더 정리 | - |
| 5 | HTTP/2 anonymous user 보호 | Connection: close |
| 6 | `WithUser(ctx, resp.User)` | - |
| 7 | 다음 핸들러(인가)로 전달 | - |

### 6.4 Anonymous User 보호

```go
// authentication.go (160-170행)
func isAnonymousUser(u user.Info) bool {
    if u.GetName() == user.Anonymous {
        return true
    }
    for _, group := range u.GetGroups() {
        if group == user.AllUnauthenticated {
            return true
        }
    }
    return false
}
```

CVE-2023-44487, CVE-2023-39325 대응으로
미인증 HTTP/2 연결을 즉시 종료하여 DoS 공격을 완화한다.

---

## 7. 인가(Authorization) 아키텍처

### 7.1 인가란

인가는 "인증된 사용자가 이 작업을 수행할 권한이 있는가?"를 결정한다.
인증과 마찬가지로 여러 인가기(authorizer)를 체인으로 연결한다.

### 7.2 전체 인가 흐름

```
인증된 요청 (Context에 user.Info 포함)
   |
   v
+-------------------------------------------+
|       unionAuthzHandler                   |
|  +------+  +------+  +------+  +------+  |
|  | Node |  | ABAC |  | RBAC |  | Hook |  |
|  +------+  +------+  +------+  +------+  |
|  순서대로 시도                              |
|  Allow/Deny → 즉시 반환 (단락)             |
|  NoOpinion → 다음으로                      |
+-------------------------------------------+
   |
   | Allow → 200 (다음 핸들러)
   | Deny → 403 Forbidden
   | 모두 NoOpinion → 403 Forbidden
   v
```

### 7.3 지원하는 인가 모드

| 모드 | 설명 |
|------|------|
| **RBAC** | Role-Based Access Control (기본, 가장 많이 사용) |
| **Node** | kubelet 전용 인가 (노드가 자신의 리소스만 접근) |
| **ABAC** | Attribute-Based Access Control (정적 파일 기반, 레거시) |
| **Webhook** | 외부 서비스에 인가 위임 |
| **AlwaysAllow** | 항상 허용 (개발/테스트용) |
| **AlwaysDeny** | 항상 거부 (테스트용) |

---

## 8. Authorizer 인터페이스와 Decision

### 8.1 Authorizer 인터페이스

```go
// staging/src/k8s.io/apiserver/pkg/authorization/authorizer/interfaces.go (83-85행)
type Authorizer interface {
    Authorize(ctx context.Context, a Attributes) (authorized Decision, reason string, err error)
}
```

### 8.2 Decision 타입

```go
// interfaces.go (175-185행)
type Decision int

const (
    DecisionDeny      Decision = iota  // 0: 명시적 거부
    DecisionAllow                       // 1: 명시적 허용
    DecisionNoOpinion                   // 2: 판단 유보 (다음 인가기로)
)
```

**3가지 결정의 의미:**

| Decision | 의미 | 체인 동작 |
|----------|------|----------|
| `DecisionAllow` | 요청 허용 | 즉시 반환, 요청 진행 |
| `DecisionDeny` | 요청 거부 | 즉시 반환, 403 |
| `DecisionNoOpinion` | 판단 유보 | 다음 인가기로 넘김 |

### 8.3 Attributes 인터페이스

인가 결정에 필요한 모든 정보를 제공하는 인터페이스:

```go
// interfaces.go (31-78행)
type Attributes interface {
    GetUser() user.Info           // 인증된 사용자 정보
    GetVerb() string              // get, list, watch, create, update, patch, delete 등
    IsReadOnly() bool             // 읽기 전용 여부
    GetNamespace() string         // 네임스페이스
    GetResource() string          // 리소스 (pods, services 등)
    GetSubresource() string       // 하위 리소스 (status, log 등)
    GetName() string              // 오브젝트 이름
    GetAPIGroup() string          // API 그룹
    GetAPIVersion() string        // API 버전
    IsResourceRequest() bool      // 리소스 요청 vs 비리소스 요청
    GetPath() string              // URL 경로
    GetFieldSelector() (fields.Requirements, error)
    GetLabelSelector() (labels.Requirements, error)
}
```

### 8.4 AttributesRecord 구현

```go
// interfaces.go (105-121행)
type AttributesRecord struct {
    User            user.Info
    Verb            string
    Namespace       string
    APIGroup        string
    APIVersion      string
    Resource        string
    Subresource     string
    Name            string
    ResourceRequest bool
    Path            string
    FieldSelectorRequirements fields.Requirements
    FieldSelectorParsingErr   error
    LabelSelectorRequirements labels.Requirements
    LabelSelectorParsingErr   error
}
```

`IsReadOnly()`의 구현이 특히 주목할 만하다:

```go
// interfaces.go (131-133행)
func (a AttributesRecord) IsReadOnly() bool {
    return a.Verb == "get" || a.Verb == "list" || a.Verb == "watch"
}
```

---

## 9. 인가 체인 (Union Authorization)

### 9.1 핵심 구조

```go
// staging/src/k8s.io/apiserver/pkg/authorization/union/union.go (37-42행)
type unionAuthzHandler []authorizer.Authorizer

func New(authorizationHandlers ...authorizer.Authorizer) authorizer.Authorizer {
    return unionAuthzHandler(authorizationHandlers)
}
```

인증 체인과 달리, 인가 체인은 Go의 타입 에일리어스를 사용하여
`[]authorizer.Authorizer` 슬라이스 자체가 `Authorizer` 인터페이스를 구현한다.

### 9.2 Authorize 메서드

```go
// union.go (45-69행)
func (authzHandler unionAuthzHandler) Authorize(ctx context.Context,
    a authorizer.Attributes) (authorizer.Decision, string, error) {
    var (
        errlist    []error
        reasonlist []string
    )

    for _, currAuthzHandler := range authzHandler {
        decision, reason, err := currAuthzHandler.Authorize(ctx, a)

        if err != nil {
            errlist = append(errlist, err)
        }
        if len(reason) != 0 {
            reasonlist = append(reasonlist, reason)
        }
        switch decision {
        case authorizer.DecisionAllow, authorizer.DecisionDeny:
            return decision, reason, err    // 즉시 반환 (단락)
        case authorizer.DecisionNoOpinion:
            // 다음 인가기로 계속
        }
    }

    return authorizer.DecisionNoOpinion,
        strings.Join(reasonlist, "\n"),
        utilerrors.NewAggregate(errlist)
}
```

### 9.3 단락 평가 다이어그램

```
Authorize(ctx, attributes)
  |
  for each authorizer:
  |   |
  |   +-- authorizer.Authorize(ctx, a)
  |         |
  |         +-- DecisionAllow → 즉시 반환 (허용)
  |         |
  |         +-- DecisionDeny  → 즉시 반환 (거부)
  |         |
  |         +-- DecisionNoOpinion → 다음 인가기로
  |
  +-- 모든 인가기가 NoOpinion
       → DecisionNoOpinion 반환
       → 인가 필터에서 403 처리
```

### 9.4 RuleResolver 체인

```go
// union.go (72-106행)
type unionAuthzRulesHandler []authorizer.RuleResolver

func NewRuleResolvers(authorizationHandlers ...authorizer.RuleResolver) authorizer.RuleResolver {
    return unionAuthzRulesHandler(authorizationHandlers)
}

func (authzHandler unionAuthzRulesHandler) RulesFor(ctx context.Context,
    user user.Info, namespace string) (
    []authorizer.ResourceRuleInfo, []authorizer.NonResourceRuleInfo, bool, error) {

    // 모든 RuleResolver의 결과를 합산
    for _, currAuthzHandler := range authzHandler {
        resourceRules, nonResourceRules, incomplete, err := currAuthzHandler.RulesFor(ctx, user, namespace)
        // 결과 누적...
    }
    return resourceRulesList, nonResourceRulesList, incompleteStatus, ...
}
```

`RulesFor`는 `kubectl auth can-i --list`와 같은 명령어에서 사용된다.
모든 인가기의 규칙을 합산하여 사용자가 수행 가능한 작업 목록을 반환한다.

---

## 10. RBAC Authorizer 심화

### 10.1 RBACAuthorizer 구조체

```go
// plugin/pkg/auth/authorizer/rbac/rbac.go (50-52행)
type RBACAuthorizer struct {
    authorizationRuleResolver RequestToRuleMapper
}
```

`RBACAuthorizer`는 `RequestToRuleMapper`에 실제 규칙 해석을 위임한다.

### 10.2 RequestToRuleMapper 인터페이스

```go
// rbac.go (37-48행)
type RequestToRuleMapper interface {
    RulesFor(ctx context.Context, subject user.Info, namespace string) ([]rbacv1.PolicyRule, error)
    VisitRulesFor(ctx context.Context, user user.Info, namespace string,
        visitor func(source fmt.Stringer, rule *rbacv1.PolicyRule, err error) bool)
}
```

| 메서드 | 용도 |
|--------|------|
| `RulesFor` | 사용자에게 적용되는 모든 PolicyRule 수집 |
| `VisitRulesFor` | Visitor 패턴으로 규칙을 순회하며 단락 평가 |

### 10.3 authorizingVisitor

```go
// rbac.go (55-73행)
type authorizingVisitor struct {
    requestAttributes authorizer.Attributes
    allowed           bool
    reason            string
    errors            []error
}

func (v *authorizingVisitor) visit(source fmt.Stringer, rule *rbacv1.PolicyRule, err error) bool {
    if rule != nil && RuleAllows(v.requestAttributes, rule) {
        v.allowed = true
        v.reason = fmt.Sprintf("RBAC: allowed by %s", source.String())
        return false  // 방문 중단 (허용됨)
    }
    if err != nil {
        v.errors = append(v.errors, err)
    }
    return true       // 방문 계속
}
```

Visitor 패턴을 사용하는 핵심 이유:
- 허용되는 규칙을 찾자마자 즉시 중단 (성능 최적화)
- 오류가 있어도 나머지 규칙 평가 계속 (additive 모델)

### 10.4 Authorize 메서드

```go
// rbac.go (75-127행)
func (r *RBACAuthorizer) Authorize(ctx context.Context,
    requestAttributes authorizer.Attributes) (authorizer.Decision, string, error) {

    ruleCheckingVisitor := &authorizingVisitor{requestAttributes: requestAttributes}

    // 모든 적용 가능한 규칙을 방문
    r.authorizationRuleResolver.VisitRulesFor(ctx,
        requestAttributes.GetUser(),
        requestAttributes.GetNamespace(),
        ruleCheckingVisitor.visit)

    if ruleCheckingVisitor.allowed {
        return authorizer.DecisionAllow, ruleCheckingVisitor.reason, nil
    }

    // 거부 시 상세 로그 (V(5) 레벨)
    if klogV := klog.V(5); klogV.Enabled() {
        // ... 상세 거부 이유 로깅
    }

    reason := ""
    if len(ruleCheckingVisitor.errors) > 0 {
        reason = fmt.Sprintf("RBAC: %v", utilerrors.NewAggregate(ruleCheckingVisitor.errors))
    }
    return authorizer.DecisionNoOpinion, reason, nil  // Deny가 아닌 NoOpinion!
}
```

**중요**: RBAC는 허용하지 않는 경우 `DecisionDeny`가 아니라 `DecisionNoOpinion`을 반환한다.
이는 다른 인가기(Node, Webhook 등)가 허용할 수 있는 여지를 남긴다.
RBAC는 "이 규칙으로는 판단할 수 없다"고 말할 뿐, 명시적으로 거부하지 않는다.

### 10.5 RBAC 생성자

```go
// rbac.go (159-166행)
func New(roles rbacregistryvalidation.RoleGetter,
    roleBindings rbacregistryvalidation.RoleBindingLister,
    clusterRoles rbacregistryvalidation.ClusterRoleGetter,
    clusterRoleBindings rbacregistryvalidation.ClusterRoleBindingLister) *RBACAuthorizer {

    authorizer := &RBACAuthorizer{
        authorizationRuleResolver: rbacregistryvalidation.NewDefaultRuleResolver(
            roles, roleBindings, clusterRoles, clusterRoleBindings,
        ),
    }
    return authorizer
}
```

4가지 RBAC 리소스에 대한 접근자를 받아서 `DefaultRuleResolver`를 생성한다:

| 접근자 | RBAC 리소스 | 범위 |
|--------|------------|------|
| `RoleGetter` | Role | 네임스페이스 |
| `RoleBindingLister` | RoleBinding | 네임스페이스 |
| `ClusterRoleGetter` | ClusterRole | 클러스터 |
| `ClusterRoleBindingLister` | ClusterRoleBinding | 클러스터 |

### 10.6 Lister 구현체

```go
// rbac.go (195-225행)
type RoleGetter struct {
    Lister rbaclisters.RoleLister
}
func (g *RoleGetter) GetRole(ctx context.Context, namespace, name string) (*rbacv1.Role, error) {
    return g.Lister.Roles(namespace).Get(name)
}

type ClusterRoleGetter struct {
    Lister rbaclisters.ClusterRoleLister
}
func (g *ClusterRoleGetter) GetClusterRole(ctx context.Context, name string) (*rbacv1.ClusterRole, error) {
    return g.Lister.Get(name)
}
```

이들은 client-go의 Informer 캐시(Lister)를 래핑한다.
etcd에 직접 접근하지 않고 로컬 캐시를 사용하므로
인가 결정이 매우 빠르게 수행된다.

---

## 11. Rule Resolver와 Subject 매칭

### 11.1 DefaultRuleResolver

```go
// pkg/registry/rbac/validation/rule.go (91-100행)
type DefaultRuleResolver struct {
    roleGetter               RoleGetter
    roleBindingLister        RoleBindingLister
    clusterRoleGetter        ClusterRoleGetter
    clusterRoleBindingLister ClusterRoleBindingLister
}
```

### 11.2 VisitRulesFor: 규칙 순회 알고리즘

```go
// rule.go (179-237행)
func (r *DefaultRuleResolver) VisitRulesFor(ctx context.Context, user user.Info,
    namespace string, visitor func(source fmt.Stringer, rule *rbacv1.PolicyRule, err error) bool) {

    // 1단계: ClusterRoleBinding 검사 (클러스터 범위)
    if clusterRoleBindings, err := r.clusterRoleBindingLister.ListClusterRoleBindings(ctx); err != nil {
        if !visitor(nil, nil, err) { return }
    } else {
        for _, clusterRoleBinding := range clusterRoleBindings {
            subjectIndex, applies := appliesTo(user, clusterRoleBinding.Subjects, "")
            if !applies { continue }

            rules, err := r.GetRoleReferenceRules(ctx, clusterRoleBinding.RoleRef, "")
            if err != nil {
                if !visitor(nil, nil, err) { return }
                continue
            }
            for i := range rules {
                if !visitor(sourceDescriber, &rules[i], nil) { return }
            }
        }
    }

    // 2단계: RoleBinding 검사 (네임스페이스 범위, namespace가 있을 때만)
    if len(namespace) > 0 {
        if roleBindings, err := r.roleBindingLister.ListRoleBindings(ctx, namespace); err != nil {
            if !visitor(nil, nil, err) { return }
        } else {
            for _, roleBinding := range roleBindings {
                subjectIndex, applies := appliesTo(user, roleBinding.Subjects, namespace)
                if !applies { continue }

                rules, err := r.GetRoleReferenceRules(ctx, roleBinding.RoleRef, namespace)
                // ...
            }
        }
    }
}
```

#### 규칙 순회 흐름

```
VisitRulesFor(user, namespace)
  |
  |-- [1단계] ClusterRoleBinding 순회
  |     |
  |     for each clusterRoleBinding:
  |       |-- appliesTo(user, subjects, "") → 사용자 매칭?
  |       |     No → skip
  |       |     Yes ↓
  |       |-- GetRoleReferenceRules(roleRef, "") → ClusterRole의 규칙 로드
  |       |-- for each rule:
  |             visitor(source, rule, nil) → false면 중단 (허용됨)
  |
  |-- [2단계] RoleBinding 순회 (namespace가 있을 때만)
        |
        for each roleBinding in namespace:
          |-- appliesTo(user, subjects, namespace) → 사용자 매칭?
          |     No → skip
          |     Yes ↓
          |-- GetRoleReferenceRules(roleRef, namespace) → Role/ClusterRole 규칙 로드
          |-- for each rule:
                visitor(source, rule, nil)
```

### 11.3 Subject 매칭 (appliesTo)

```go
// rule.go (263-270행)
func appliesTo(user user.Info, bindingSubjects []rbacv1.Subject, namespace string) (int, bool) {
    for i, bindingSubject := range bindingSubjects {
        if appliesToUser(user, bindingSubject, namespace) {
            return i, true
        }
    }
    return 0, false
}
```

### 11.4 appliesToUser: 3가지 Subject Kind

```go
// rule.go (281-304행)
func appliesToUser(user user.Info, subject rbacv1.Subject, namespace string) bool {
    switch subject.Kind {
    case rbacv1.UserKind:
        return user.GetName() == subject.Name

    case rbacv1.GroupKind:
        return has(user.GetGroups(), subject.Name)

    case rbacv1.ServiceAccountKind:
        saNamespace := namespace
        if len(subject.Namespace) > 0 {
            saNamespace = subject.Namespace
        }
        if len(saNamespace) == 0 {
            return false
        }
        return serviceaccount.MatchesUsername(saNamespace, subject.Name, user.GetName())

    default:
        return false
    }
}
```

| Subject Kind | 매칭 방식 |
|-------------|----------|
| `User` | 사용자 이름 직접 비교 |
| `Group` | 사용자의 그룹 목록에서 검색 |
| `ServiceAccount` | `system:serviceaccount:<namespace>:<name>` 형식으로 변환하여 비교 |

### 11.5 GetRoleReferenceRules

```go
// rule.go (240-259행)
func (r *DefaultRuleResolver) GetRoleReferenceRules(ctx context.Context,
    roleRef rbacv1.RoleRef, bindingNamespace string) ([]rbacv1.PolicyRule, error) {
    switch roleRef.Kind {
    case "Role":
        role, err := r.roleGetter.GetRole(ctx, bindingNamespace, roleRef.Name)
        if err != nil { return nil, err }
        return role.Rules, nil

    case "ClusterRole":
        clusterRole, err := r.clusterRoleGetter.GetClusterRole(ctx, roleRef.Name)
        if err != nil { return nil, err }
        return clusterRole.Rules, nil

    default:
        return nil, fmt.Errorf("unsupported role reference kind: %q", roleRef.Kind)
    }
}
```

RoleBinding은 같은 네임스페이스의 Role만 참조할 수 있지만,
RoleBinding에서 ClusterRole을 참조할 수도 있다 (해당 네임스페이스로 범위가 제한됨).

---

## 12. RuleAllows: 규칙 매칭 알고리즘

### 12.1 RuleAllows 함수

```go
// plugin/pkg/auth/authorizer/rbac/rbac.go (178-193행)
func RuleAllows(requestAttributes authorizer.Attributes, rule *rbacv1.PolicyRule) bool {
    if requestAttributes.IsResourceRequest() {
        combinedResource := requestAttributes.GetResource()
        if len(requestAttributes.GetSubresource()) > 0 {
            combinedResource = requestAttributes.GetResource() + "/" + requestAttributes.GetSubresource()
        }

        return rbacv1helpers.VerbMatches(rule, requestAttributes.GetVerb()) &&
            rbacv1helpers.APIGroupMatches(rule, requestAttributes.GetAPIGroup()) &&
            rbacv1helpers.ResourceMatches(rule, combinedResource, requestAttributes.GetSubresource()) &&
            rbacv1helpers.ResourceNameMatches(rule, requestAttributes.GetName())
    }

    return rbacv1helpers.VerbMatches(rule, requestAttributes.GetVerb()) &&
        rbacv1helpers.NonResourceURLMatches(rule, requestAttributes.GetPath())
}
```

### 12.2 리소스 요청 매칭 조건

리소스 요청 (`IsResourceRequest() == true`)에 대해 **4가지 조건 모두** 만족해야 한다:

```
RuleAllows = VerbMatches
           AND APIGroupMatches
           AND ResourceMatches
           AND ResourceNameMatches
```

| 조건 | 매칭 대상 | 와일드카드 |
|------|----------|-----------|
| `VerbMatches` | get, list, watch, create, update, patch, delete | `*` |
| `APIGroupMatches` | "", apps, rbac.authorization.k8s.io 등 | `*` |
| `ResourceMatches` | pods, services, deployments 등 + 하위 리소스 | `*` |
| `ResourceNameMatches` | 특정 오브젝트 이름 (비어 있으면 모든 이름) | - |

### 12.3 비리소스 요청 매칭

비리소스 요청 (`/healthz`, `/api` 등)에 대해 **2가지 조건** 만족:

```
RuleAllows = VerbMatches AND NonResourceURLMatches
```

### 12.4 SubResource 처리

하위 리소스(예: `pods/log`, `pods/status`)의 경우 리소스명을 결합한다:

```go
combinedResource := requestAttributes.GetResource() + "/" + requestAttributes.GetSubresource()
// 예: "pods/log", "pods/status", "deployments/scale"
```

이를 통해 `pods/*` 규칙으로 pods의 모든 하위 리소스에 대한 접근을 허용하거나,
`pods/log` 규칙으로 log 하위 리소스만 접근을 허용할 수 있다.

### 12.5 RulesAllow (복수 규칙)

```go
// rbac.go (168-176행)
func RulesAllow(requestAttributes authorizer.Attributes, rules ...rbacv1.PolicyRule) bool {
    for i := range rules {
        if RuleAllows(requestAttributes, &rules[i]) {
            return true
        }
    }
    return false
}
```

하나라도 허용하면 전체가 허용된다 (OR 논리, additive 모델).

---

## 13. 인가 필터 (WithAuthorization)

### 13.1 WithAuthorization 함수

```go
// staging/src/k8s.io/apiserver/pkg/endpoints/filters/authorization.go (53-55행)
func WithAuthorization(hhandler http.Handler, auth authorizer.Authorizer,
    s runtime.NegotiatedSerializer) http.Handler {
    return withAuthorization(hhandler, auth, s, recordAuthorizationMetrics)
}
```

### 13.2 내부 처리 로직

```go
// authorization.go (57-99행)
func withAuthorization(handler http.Handler, a authorizer.Authorizer, ...) http.Handler {
    if a == nil {
        klog.Warning("Authorization is disabled")
        return handler
    }
    return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
        ctx := req.Context()
        authorizationStart := time.Now()

        // Context에서 요청 정보 추출하여 Attributes 생성
        attributes, err := GetAuthorizerAttributes(ctx)
        if err != nil {
            responsewriters.InternalError(w, req, err)
            return
        }

        // 인가 수행
        authorized, reason, err := a.Authorize(ctx, attributes)

        // 메트릭 기록
        authorizationFinish := time.Now()
        // ...

        // Allow 먼저 확인 (에러가 있어도 허용될 수 있음)
        if authorized == authorizer.DecisionAllow {
            audit.AddAuditAnnotations(ctx,
                decisionAnnotationKey, decisionAllow,
                reasonAnnotationKey, reason)
            handler.ServeHTTP(w, req)
            return
        }

        // 에러 처리
        if err != nil {
            audit.AddAuditAnnotation(ctx, reasonAnnotationKey, reasonError)
            responsewriters.InternalError(w, req, err)
            return
        }

        // 거부 또는 NoOpinion → 403 Forbidden
        klog.V(4).InfoS("Forbidden", "URI", req.RequestURI, "reason", reason)
        audit.AddAuditAnnotations(ctx,
            decisionAnnotationKey, decisionForbid,
            reasonAnnotationKey, reason)
        responsewriters.Forbidden(attributes, w, req, reason, s)
    })
}
```

**중요**: `authorized == authorizer.DecisionAllow`를 **에러보다 먼저** 확인한다.
RBAC 같은 인가기가 평가 중 에러를 만나도 다른 규칙에 의해 허용될 수 있기 때문이다.

### 13.3 GetAuthorizerAttributes

```go
// authorization.go (101-153행)
func GetAuthorizerAttributes(ctx context.Context) (authorizer.Attributes, error) {
    attribs := authorizer.AttributesRecord{}

    user, ok := request.UserFrom(ctx)
    if ok {
        attribs.User = user
    }

    requestInfo, found := request.RequestInfoFrom(ctx)
    if !found {
        return nil, errors.New("no RequestInfo found in the context")
    }

    attribs.ResourceRequest = requestInfo.IsResourceRequest
    attribs.Path = requestInfo.Path
    attribs.Verb = requestInfo.Verb
    attribs.APIGroup = requestInfo.APIGroup
    attribs.APIVersion = requestInfo.APIVersion
    attribs.Resource = requestInfo.Resource
    attribs.Subresource = requestInfo.Subresource
    attribs.Namespace = requestInfo.Namespace
    attribs.Name = requestInfo.Name

    // AuthorizeWithSelectors 피처 게이트가 활성화된 경우
    // 필드/라벨 셀렉터도 인가 속성에 포함
    if utilfeature.DefaultFeatureGate.Enabled(genericfeatures.AuthorizeWithSelectors) {
        // 필드 셀렉터 파싱
        // 라벨 셀렉터 파싱
    }

    return &attribs, nil
}
```

Context에서 `user.Info`와 `RequestInfo`를 추출하여
`AttributesRecord`를 구성한다.

---

## 14. Handler Chain 통합

### 14.1 전체 핸들러 체인

API Server의 HTTP 핸들러 체인에서 인증과 인가는 다음 위치에 있다:

```
HTTP 요청 도착
  |
  v
[1] WithPanicRecovery        -- 패닉 복구
  |
  v
[2] WithRequestInfo          -- URL → RequestInfo 파싱
  |
  v
[3] WithAuthentication       -- 인증 (이 문서의 6장)
  |     |
  |     +-- 실패 → 401 Unauthorized
  |     +-- 성공 → Context에 user.Info 저장
  |
  v
[4] WithAuditInit            -- 감사 로그 초기화
  |
  v
[5] WithAuthorization        -- 인가 (이 문서의 13장)
  |     |
  |     +-- Allow → 계속
  |     +-- Deny/NoOpinion → 403 Forbidden
  |
  v
[6] WithAdmission            -- 어드미션 컨트롤
  |
  v
[7] REST Handler             -- 실제 리소스 처리
```

### 14.2 인증에서 인가로의 데이터 전달

```go
// authentication.go (122행)
req = req.WithContext(genericapirequest.WithUser(req.Context(), resp.User))

// authorization.go (104행)
user, ok := request.UserFrom(ctx)
```

인증 필터는 `WithUser(ctx, user.Info)`로 Context에 사용자 정보를 저장하고,
인가 필터는 `UserFrom(ctx)`로 이를 꺼내 사용한다.
Go의 `context.Context`를 통한 데이터 전달 패턴이다.

### 14.3 감사 로그 연계

인가 결과는 감사 로그(audit log)에 어노테이션으로 기록된다:

```go
// authorization.go (40-48행)
const (
    decisionAnnotationKey = "authorization.k8s.io/decision"
    reasonAnnotationKey   = "authorization.k8s.io/reason"

    decisionAllow  = "allow"
    decisionForbid = "forbid"
    reasonError    = "internal error"
)
```

---

## 15. User/Group 모델

### 15.1 user.Info 인터페이스

```go
// staging/src/k8s.io/apiserver/pkg/authentication/user/user.go (20-42행)
type Info interface {
    GetName() string                    // 사용자 고유 이름
    GetUID() string                     // 사용자 고유 식별자
    GetGroups() []string                // 소속 그룹 목록
    GetExtra() map[string][]string      // 추가 정보 (스코프 등)
}
```

### 15.2 DefaultInfo 구현체

```go
// user.go (46-51행)
type DefaultInfo struct {
    Name   string
    UID    string
    Groups []string
    Extra  map[string][]string
}
```

### 15.3 Well-Known 사용자와 그룹

```go
// user.go (69-88행)
const (
    SystemPrivilegedGroup = "system:masters"       // 모든 권한 (슈퍼유저)
    NodesGroup            = "system:nodes"          // kubelet 노드 그룹
    MonitoringGroup       = "system:monitoring"     // 모니터링 전용
    AllUnauthenticated    = "system:unauthenticated"// 미인증 사용자 그룹
    AllAuthenticated      = "system:authenticated"  // 인증된 사용자 그룹

    Anonymous     = "system:anonymous"              // 익명 사용자
    APIServerUser = "system:apiserver"              // API Server 자체

    KubeProxy             = "system:kube-proxy"
    KubeControllerManager = "system:kube-controller-manager"
    KubeScheduler         = "system:kube-scheduler"
)
```

| 사용자/그룹 | 설명 |
|------------|------|
| `system:masters` | 모든 RBAC 검사를 우회하는 슈퍼유저 그룹 |
| `system:authenticated` | 인증된 모든 사용자에게 자동 추가 |
| `system:unauthenticated` | 미인증 요청에 자동 추가 |
| `system:anonymous` | 미인증 요청의 사용자명 |
| `system:serviceaccount:<ns>:<name>` | 서비스 어카운트 사용자명 형식 |

### 15.4 CredentialID

```go
// user.go (87-88행)
CredentialIDKey = "authentication.kubernetes.io/credential-id"
```

인증서나 토큰의 고유 식별자를 Extra 필드에 저장하여
감사 로그에서 어떤 자격 증명이 사용되었는지 추적할 수 있다.

---

## 16. 설계 원칙: Why

### 16.1 왜 플러거블 체인 구조인가?

**문제**: 조직마다 인증/인가 요구사항이 다르다.
어떤 조직은 LDAP, 어떤 조직은 OIDC, 어떤 조직은 인증서를 사용한다.

**해결**: Chain of Responsibility 패턴으로 인증기/인가기를 동적으로 조합한다.
새로운 인증/인가 방식을 추가할 때 기존 코드를 수정할 필요가 없다.

```
+--------+     +--------+     +--------+
| Auth-1 | --> | Auth-2 | --> | Auth-3 |
+--------+     +--------+     +--------+
    |               |              |
    | 성공?          | 성공?         | 성공?
    | No → 다음     | No → 다음    | No → 최종실패
    v               v              v
```

### 16.2 왜 RBAC에서 NoOpinion인가?

RBAC가 거부할 때 `DecisionDeny`가 아닌 `DecisionNoOpinion`을 반환하는 이유:

1. **다층 방어 (Defense in Depth)**:
   여러 인가기가 체인으로 연결되어 있을 때,
   RBAC에서 규칙을 찾지 못해도 다른 인가기(Node, Webhook)가 허용할 수 있다.

2. **Additive 모델**:
   RBAC 규칙은 "허용"만 명시한다. "거부" 규칙이 없다.
   규칙이 없다는 것은 "거부"가 아니라 "판단 불가"이다.

3. **보안 기본값**:
   모든 인가기가 NoOpinion이면 최종적으로 거부된다.
   명시적 허용이 없으면 기본 거부(deny by default).

### 16.3 왜 Visitor 패턴인가?

`VisitRulesFor`가 규칙 목록 반환 대신 Visitor 패턴을 사용하는 이유:

1. **조기 종료**: 허용 규칙을 찾으면 나머지 규칙을 평가할 필요 없음
2. **메모리 절약**: 모든 규칙을 슬라이스에 수집하지 않아도 됨
3. **유연성**: 동일한 순회 로직으로 다른 동작 수행 가능
   (예: 규칙 수집 vs 인가 확인)

```go
// 인가 확인용 visitor (허용 시 false 반환 = 순회 중단)
func (v *authorizingVisitor) visit(source, rule, err) bool {
    if RuleAllows(v.requestAttributes, rule) {
        v.allowed = true
        return false  // 중단
    }
    return true       // 계속
}

// 규칙 수집용 visitor (항상 true 반환 = 전부 순회)
func (r *ruleAccumulator) visit(source, rule, err) bool {
    if rule != nil {
        r.rules = append(r.rules, *rule)
    }
    return true       // 계속
}
```

### 16.4 왜 인증 후 Authorization 헤더를 삭제하는가?

```go
// authentication.go (89행)
req.Header.Del("Authorization")
```

보안 관련 이유:
1. **하위 핸들러 보호**: 인증 완료 후 토큰이 더 이상 필요 없음
2. **로깅 안전**: 실수로 토큰이 로그에 기록되는 것을 방지
3. **프록시 전파 방지**: API Server 뒤의 서비스로 토큰이 전달되는 것을 방지

### 16.5 왜 인가에서 Allow를 에러보다 먼저 확인하는가?

```go
// authorization.go (80행)
if authorized == authorizer.DecisionAllow {
    // 에러가 있어도 허용
    handler.ServeHTTP(w, req)
    return
}
```

RBAC에서 일부 규칙 해석 중 에러가 발생해도,
다른 규칙에 의해 이미 허용이 결정된 경우가 있다.
규칙은 additive이므로, 하나라도 허용하면 전체가 허용된다.

### 16.6 왜 캐시 기반 Lister를 사용하는가?

```go
// rbac.go (199-201행)
func (g *RoleGetter) GetRole(ctx context.Context, namespace, name string) (*rbacv1.Role, error) {
    return g.Lister.Roles(namespace).Get(name)
}
```

인가는 **모든 API 요청**에서 실행된다.
매번 etcd에 쿼리하면 심각한 성능 병목이 된다.
Informer 캐시(Lister)를 사용하면:

- O(1) 조회: 로컬 인메모리 캐시
- 비동기 업데이트: Watch를 통해 변경 사항만 반영
- API Server 부하 감소

트레이드오프: 캐시는 **최종 일관성(eventually consistent)**이므로,
RBAC 규칙 변경 후 약간의 지연이 있을 수 있다.

---

## 17. 정리

### 17.1 인증-인가 전체 흐름 요약

```
클라이언트 → HTTP 요청
  |
  v
[인증 체인] unionAuthRequestHandler
  |-- X.509 인증기 → 실패
  |-- Bearer Token → Service Account 인증 → 성공!
  |   → user.Info{Name: "system:serviceaccount:default:my-sa",
  |               Groups: ["system:serviceaccounts", "system:authenticated"]}
  |
  v
Context에 user.Info 저장
  |
  v
[인가 체인] unionAuthzHandler
  |-- Node Authorizer → NoOpinion (SA 요청이므로)
  |-- RBAC Authorizer
  |     |-- ClusterRoleBinding 검사
  |     |     |-- "system:authenticated" 그룹이 "system:basic-user" ClusterRole에 바인딩
  |     |     |-- 규칙: {resources: ["selfsubjectaccessreviews"], verbs: ["create"]}
  |     |     |-- RuleAllows(요청 속성, 규칙) → 매칭 확인
  |     |
  |     |-- RoleBinding 검사 (namespace "default")
  |     |     |-- "my-sa"가 "pod-reader" Role에 바인딩
  |     |     |-- 규칙: {resources: ["pods"], verbs: ["get", "list"]}
  |     |     |-- GET /api/v1/namespaces/default/pods → 매칭!
  |     |
  |     +-- DecisionAllow, "RBAC: allowed by RoleBinding ..."
  |
  v
요청 처리 계속 (Admission → Storage)
```

### 17.2 핵심 인터페이스 관계

```
authenticator.Request
       |
       +-- unionAuthRequestHandler (체인)
       |     |-- X.509 Authenticator
       |     |-- Token Authenticator
       |     |-- OIDC Authenticator
       |     +-- Webhook Authenticator
       |
       v
authenticator.Response{User: user.Info}
       |
       v
authorizer.Authorizer
       |
       +-- unionAuthzHandler (체인)
       |     |-- Node Authorizer
       |     |-- RBAC Authorizer
       |     |     |-- RequestToRuleMapper
       |     |     |     +-- DefaultRuleResolver
       |     |     |           |-- VisitRulesFor()
       |     |     |           |-- appliesTo() (Subject 매칭)
       |     |     |           +-- GetRoleReferenceRules()
       |     |     +-- RuleAllows() (규칙 매칭)
       |     +-- Webhook Authorizer
       |
       v
authorizer.Decision (Allow / Deny / NoOpinion)
```

### 17.3 RBAC 4대 리소스 관계

```
+------------------+     참조     +------------------+
| ClusterRoleBinding|----------->| ClusterRole      |
|   subjects: [...]  |            |   rules: [...]    |
+------------------+              +------------------+
      |                                  |
      | 바인딩                            | 참조 가능
      v                                  v
+------------------+     참조     +------------------+
| RoleBinding       |----------->| Role             |
| (namespace 범위)   |            | (namespace 범위)  |
|   subjects: [...]  |            |   rules: [...]    |
+------------------+              +------------------+
```

| 리소스 | 범위 | 역할 |
|--------|------|------|
| ClusterRole | 클러스터 | 클러스터/네임스페이스 리소스에 대한 권한 정의 |
| ClusterRoleBinding | 클러스터 | 사용자/그룹을 ClusterRole에 바인딩 |
| Role | 네임스페이스 | 해당 네임스페이스 리소스에 대한 권한 정의 |
| RoleBinding | 네임스페이스 | 사용자/그룹을 Role/ClusterRole에 바인딩 |
