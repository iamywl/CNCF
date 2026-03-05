# PoC-07: Jenkins 보안 서브시스템 (Security Subsystem)

## 목적

Jenkins 보안 모델의 4대 축인 **SecurityRealm(인증)**, **AuthorizationStrategy(인가)**, **ACL+Permission(접근 제어)**, **CrumbIssuer(CSRF 방어)**를 Go 표준 라이브러리만으로 시뮬레이션한다.

이 PoC를 통해 Jenkins의 인증/인가 분리 아키텍처, Permission 계층 트리, 매트릭스 기반 세밀한 접근 제어, CSRF 토큰 메커니즘을 이해할 수 있다.

## 핵심 개념

### 인증/인가 흐름 (전체 아키텍처)

```
HTTP 요청
    |
    v
+--------------------------------------------------+
|             Filter Chain (서블릿 필터)              |
|  1. HttpSessionContextIntegrationFilter2          |
|  2. BasicHeaderProcessor        -- Basic 인증     |
|  3. AuthenticationProcessingFilter2 -- 폼 로그인   |
|  4. RememberMeAuthenticationFilter                |
|  5. AnonymousAuthenticationFilter -- 익명 설정     |
|  6. ExceptionTranslationFilter                    |
+--------------------------------------------------+
    |
    v
+------------------+     +------------------------+
| SecurityRealm    |     | SecurityContext         |
| (인증 백엔드)     |---->| (현재 사용자 Authentication)|
|                  |     +------------------------+
| authenticate()   |              |
| loadUserByUsername()|           v
+------------------+     +------------------------+
                         | AuthorizationStrategy   |
                         | (인가 전략)              |
                         |                        |
                         | getRootACL()            |
                         | getACL(Job)             |
                         +------------------------+
                                  |
                                  v
                         +------------------------+
                         | ACL                    |
                         | (접근 제어 리스트)       |
                         |                        |
                         | hasPermission2(auth, p) |
                         +------------------------+
                                  |
                                  v
                         +------------------------+
                         | Permission             |
                         | (권한 트리)             |
                         |                        |
                         | impliedBy 체인 확인     |
                         +------------------------+
                                  |
                                  v
                           GRANTED / DENIED
```

### Permission 계층 (impliedBy 트리)

```
Hudson.Administer (God-like -- 모든 권한의 최상위)
  |-- Permission.GenericRead
  |     |-- Item.Read
  |     |-- Item.Workspace
  |-- Permission.GenericWrite
  |     |-- Permission.GenericCreate
  |     |     |-- Item.Create
  |     |-- Permission.GenericUpdate
  |     |     |-- Permission.GenericConfigure
  |     |     |     |-- Item.Configure
  |     |     |-- Item.Build
  |     |     |-- Item.Cancel
  |     |-- Permission.GenericDelete
  |           |-- Item.Delete
```

상위 권한을 가진 사용자는 하위 권한을 자동으로 보유한다. 예를 들어, `Hudson.Administer`를 가진 사용자는 `Item.Build`, `Item.Delete` 등 모든 권한을 갖는다.

### CSRF 방어 (CrumbIssuer)

```
POST 요청
    |
    v
+----------------------------+
| CrumbFilter                |
|                            |
| 1. Jenkins-Crumb 헤더 추출  |
| 2. DefaultCrumbIssuer로    |
|    토큰 검증               |
+----------------------------+
    |
    v
+----------------------------+
| DefaultCrumbIssuer         |
|                            |
| issueCrumb():              |
|   data = username + ";"    |
|        + sessionID         |
|   SHA-256(data + salt)     |
|   → hex string             |
|                            |
| validateCrumb():           |
|   MessageDigest.isEqual()  |
|   (상수 시간 비교)          |
+----------------------------+
```

## 시뮬레이션 구성

| 구성 요소 | Jenkins 실제 클래스 | PoC 구현 |
|-----------|---------------------|----------|
| Permission | `hudson.security.Permission` | `Permission` struct (Group, Name, ImpliedBy) |
| PermissionGroup | `hudson.security.PermissionGroup` | `PermissionGroup` struct |
| ACL | `hudson.security.ACL` | `ACL` interface + `MatrixACL`, `LambdaACL`, `CompositeACL` |
| SecurityRealm | `hudson.security.SecurityRealm` | `SecurityRealm` interface |
| HudsonPrivateSecurityRealm | `hudson.security.HudsonPrivateSecurityRealm` | `HudsonPrivateSecurityRealm` struct (SHA-256 해싱) |
| AuthorizationStrategy | `hudson.security.AuthorizationStrategy` | `AuthorizationStrategy` interface |
| FullControlOnceLoggedIn | `FullControlOnceLoggedInAuthorizationStrategy` | `FullControlOnceLoggedIn` struct |
| MatrixAuthorization | `GlobalMatrixAuthorizationStrategy` | `MatrixAuthorizationStrategy` struct |
| CrumbIssuer | `hudson.security.csrf.CrumbIssuer` | `CrumbIssuer` interface |
| DefaultCrumbIssuer | `hudson.security.csrf.DefaultCrumbIssuer` | `DefaultCrumbIssuer` struct (HMAC-SHA256) |
| SecurityContext | `SecurityContextHolder` (ThreadLocal) | `SecurityContext` struct (mutex) |
| Filter Chain | `SecurityRealm.createFilterImpl()` | `FilterChain` + `BasicHeaderFilter`, `AnonymousAuthFilter`, `CrumbFilter` |
| SYSTEM | `ACL.SYSTEM2` | `SYSTEM` 전역 변수 (항상 모든 권한 허가) |
| Impersonation | `ACL.impersonate2()`, `ACL.as2()` | `SecurityContext.Impersonate()` |

## 실제 소스 참조

| 파일 | 핵심 메서드/필드 |
|------|----------------|
| `core/src/main/java/hudson/security/SecurityRealm.java` | `createSecurityComponents()`, `createFilter()`, `SecurityComponents` 내부 클래스 |
| `core/src/main/java/hudson/security/HudsonPrivateSecurityRealm.java` | BCrypt/PBKDF2 패스워드 해싱, `AbstractPasswordBasedSecurityRealm` 상속 |
| `core/src/main/java/hudson/security/AuthorizationStrategy.java` | `getRootACL()`, `getACL(Job)`, `UNSECURED` 싱글턴 |
| `core/src/main/java/hudson/security/FullControlOnceLoggedInAuthorizationStrategy.java` | `AUTHENTICATED_READ`, `ANONYMOUS_READ` SparseACL, `denyAnonymousReadAccess` |
| `core/src/main/java/hudson/security/ACL.java` | `hasPermission2()`, `checkPermission()`, `SYSTEM2`, `impersonate2()`, `as2()`, `lambda2()` |
| `core/src/main/java/hudson/security/Permission.java` | `group`, `name`, `impliedBy`, `HUDSON_ADMINISTER`, `READ`, `WRITE`, `CREATE`, `UPDATE`, `DELETE`, `CONFIGURE` |
| `core/src/main/java/hudson/security/PermissionGroup.java` | `owner`, `permissions` (SortedSet), `find(name)` |
| `core/src/main/java/hudson/model/Item.java` | `Item.CREATE`, `DELETE`, `CONFIGURE`, `READ`, `BUILD`, `WORKSPACE`, `CANCEL` |
| `core/src/main/java/hudson/security/csrf/CrumbIssuer.java` | `getCrumb()`, `issueCrumb()`, `validateCrumb()`, `DEFAULT_CRUMB_NAME` |
| `core/src/main/java/hudson/security/csrf/DefaultCrumbIssuer.java` | SHA-256 + salt 기반 crumb 생성, `MessageDigest.isEqual()` 상수 시간 비교 |

## 실행 방법

```bash
cd jenkins_EDU/poc-07-security
go run main.go
```

외부 의존성 없이 Go 표준 라이브러리만 사용한다.

## 예상 출력

```
========================================================================
 Jenkins 보안 서브시스템 PoC
 SecurityRealm / AuthorizationStrategy / ACL / Permission / CrumbIssuer
========================================================================

=== [1] Permission 계층 구조 (impliedBy 트리) ===

Jenkins의 모든 권한은 트리 구조를 형성한다.
상위 권한을 가진 사용자는 하위 권한도 자동으로 갖는다.

Administer (Overall)
  |-- GenericRead (Generic)
    |-- Read (Job)
    |-- Workspace (Job)
  |-- GenericWrite (Generic)
    |-- GenericCreate (Generic)
      |-- Create (Job)
    |-- GenericUpdate (Generic)
      |-- GenericConfigure (Generic)
        |-- Configure (Job)
      |-- Build (Job)
      |-- Cancel (Job)
    |-- GenericDelete (Generic)
      |-- Delete (Job)

=== [2] SecurityRealm: 사용자 등록 및 인증 (HudsonPrivateSecurityRealm) ===

  [OK] 사용자 등록: admin (역할: [authenticated admin])
  [OK] 사용자 등록: developer (역할: [authenticated developer])
  [OK] 사용자 등록: viewer (역할: [authenticated viewer])

--- 인증 시도 ---
  [OK]   admin / admin123 → 인증 성공 (authenticated=true, authorities=[authenticated admin])
  [OK]   developer / dev456 → 인증 성공 (authenticated=true, authorities=[authenticated developer])
  [FAIL] developer / wrong → 잘못된 비밀번호입니다 (BadCredentialsException)
  [FAIL] unknown / pass → 사용자 'unknown'를 찾을 수 없습니다 (UsernameNotFoundException)

=== [3] FullControlOnceLoggedIn 인가 전략 ===

  [GRANTED] admin → Administer
  [GRANTED] admin → Item.Build
  [GRANTED] anonymous → Item.Read (허용: 익명 읽기 허용 모드)
  [DENIED]  anonymous → Item.Build (거부: 쓰기 권한 없음)
  [GRANTED] SYSTEM → Administer (항상 허가)

=== [4] MatrixAuthorizationStrategy: 프로젝트별 매트릭스 권한 ===

--- 글로벌 권한 매트릭스 ---
사용자       | Administer| Create    | Read      | Build     | Configure | Delete    |
-----------+-----------+-----------+-----------+-----------+-----------+-----------+
admin      | V         | V         | V         | V         | V         | V         |
developer  | -         | V         | V         | V         | V         | -         |
viewer     | -         | -         | V         | -         | -         | -         |

--- 권한 검사 ---
  [GRANTED] admin → Administer (글로벌)
  [GRANTED] admin → Item.Delete (Administer가 내포)
  [GRANTED] developer → Item.Build (글로벌: 직접 부여)
  [DENIED]  developer → Item.Delete (글로벌: 부여 안 됨)
  [GRANTED] developer → Item.Delete (my-project: 프로젝트별 부여)
  [GRANTED] viewer → Item.Read (글로벌: 직접 부여)
  [DENIED]  viewer → Item.Build (글로벌: 부여 안 됨)

=== [5] CrumbIssuer: CSRF 토큰 발급 및 검증 ===

  [  VALID] 올바른 토큰
  [INVALID] 잘못된 토큰
  [INVALID] 다른 세션 (세션 고정 공격 방어)
  [INVALID] 다른 사용자 (크로스사이트 공격 방어)

=== [6] Filter Chain: 요청 → 인증 → 인가 흐름 ===

  시나리오 1: Basic 인증 GET → 인증 성공
  시나리오 2: 인증 없는 GET → 익명 사용자로 설정
  시나리오 3: POST + CSRF 토큰 → 토큰 검증 성공
  시나리오 4: POST + CSRF 없음 → 요청 거부

=== [7] Impersonation: 사용자 전환 ===

  developer → SYSTEM 전환 → 모든 권한 허가 → 원래 사용자 복원

=== [8] 종합 시나리오: 전체 보안 흐름 ===

  시나리오 1: admin → my-project Build → GRANTED
  시나리오 2: developer → my-project Build → GRANTED
  시나리오 3: developer → my-project Delete → GRANTED (프로젝트별 ACL)
  시나리오 4: developer → other-project Delete → DENIED (글로벌 ACL)
  시나리오 5: viewer → my-project Read → GRANTED
  시나리오 6: viewer → my-project Build → DENIED
```

## 학습 포인트

1. **인증/인가 분리**: SecurityRealm은 "누구인가"만 결정하고, AuthorizationStrategy는 "무엇을 할 수 있는가"만 결정한다. 이 분리 덕분에 LDAP 인증 + 매트릭스 인가 같은 조합이 가능하다.

2. **Permission impliedBy 체인**: 권한이 트리 구조로 구성되어 상위 권한이 하위 권한을 내포한다. `Administer` 하나만 부여하면 모든 권한을 갖는다.

3. **SYSTEM 인증의 특수성**: `ACL.SYSTEM2`는 모든 권한 검사를 우회한다. Jenkins 내부 작업(빌드, 스케줄링)은 SYSTEM으로 실행된다.

4. **CSRF 방어**: DefaultCrumbIssuer는 사용자명 + 세션 ID + 비밀 솔트로 해시를 생성하여, 다른 사이트에서의 위조 요청을 차단한다.

5. **프로젝트별 ACL 레이어링**: MatrixAuthorizationStrategy에서 프로젝트별 ACL은 글로벌 ACL 위에 레이어링되어, 특정 프로젝트에서만 추가 권한을 부여할 수 있다.
