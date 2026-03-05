# 12. Jenkins 보안 서브시스템 (Security Subsystem)

## 개요

Jenkins 보안 서브시스템은 **인증(Authentication)**과 **인가(Authorization)**를 분리된 확장 가능한
아키텍처로 구현한다. 핵심 설계 원칙은 다음과 같다.

1. `SecurityRealm`: "이 사용자가 누구인가?"를 결정 (인증)
2. `AuthorizationStrategy`: "이 사용자가 무엇을 할 수 있는가?"를 결정 (인가)
3. `ACL` + `Permission`: 세밀한 접근 제어의 실제 검사 수행
4. `CrumbIssuer`: CSRF 공격 방어

이 네 가지 축이 Jenkins의 보안 모델 전체를 구성하며, 모두 `ExtensionPoint`를 통해
플러그인으로 교체 가능하다.

```
소스 경로: core/src/main/java/hudson/security/
```

---

## 1. SecurityRealm -- 인증 (Authentication)

### 1.1 클래스 구조

```
소스: core/src/main/java/hudson/security/SecurityRealm.java (942줄)
```

```java
public abstract class SecurityRealm
    implements Describable<SecurityRealm>, ExtensionPoint {

    // 핵심 추상 메서드 -- 인증 컴포넌트 생성
    public abstract SecurityComponents createSecurityComponents();

    // 서블릿 필터 체인 생성 (기본 구현 제공)
    public Filter createFilter(FilterConfig filterConfig) { ... }
}
```

`SecurityRealm`은 Jenkins의 **인증 백엔드**를 추상화한다. `Describable`을 구현하므로
Jenkins 시스템 설정 UI에서 선택 가능하고, `ExtensionPoint`이므로 플러그인이 새로운
인증 방식을 등록할 수 있다.

### 1.2 두 가지 구현 방식

Jenkins JavaDoc에 명시된 두 가지 구현 패턴이 있다.

#### 방식 1: createSecurityComponents() 오버라이드

```
"표준" 방식 -- 대부분의 SecurityRealm 구현이 이 패턴을 사용
```

`createSecurityComponents()`를 오버라이드하여 `AuthenticationManager`와
`UserDetailsService`를 제공한다. 기본 `createFilter(FilterConfig)` 구현이
이 컴포넌트들을 표준 Spring Security 필터 체인으로 조립한다.

```java
// SecurityRealm.SecurityComponents -- 튜플 클래스
public static final class SecurityComponents {
    public final AuthenticationManager manager2;
    public final UserDetailsService userDetails2;
    public final RememberMeServices rememberMe2;

    // 생성자 오버로드
    public SecurityComponents(AuthenticationManager manager,
                              UserDetailsService userDetails) {
        this(manager, userDetails, createRememberMeService(userDetails));
    }

    private static RememberMeServices createRememberMeService(UserDetailsService uds) {
        TokenBasedRememberMeServices2 rms = new TokenBasedRememberMeServices2(uds);
        rms.setParameter("remember_me"); // login.jelly의 폼 필드 이름
        return rms;
    }
}
```

**왜 튜플인가?** `AuthenticationManager`, `UserDetailsService`, `RememberMeServices`는
서로 의존 관계가 있다. 예를 들어 `RememberMeServices`는 `UserDetailsService`를 필요로 한다.
이들을 개별 메서드로 반환하면 초기화 순서 문제가 발생할 수 있으므로 하나의 튜플로 묶어
한 번에 반환한다.

#### 방식 2: createFilter(FilterConfig) 직접 오버라이드

```
"비표준" 방식 -- 독자적인 필터 체인이 필요한 경우
```

`createSecurityComponents()`를 무시하고 (기본 생성자로 빈 `SecurityComponents` 반환)
`createFilter(FilterConfig)`를 직접 오버라이드한다. 필터 체인 끝에서
`SecurityContext.getAuthentication()`이 올바른 `Authentication` 객체를 가지고 있으면
Jenkins는 Spring Security의 표준 모델을 따를 필요가 없다.

소스 코드의 JavaDoc 원문:

> This model is for those "weird" implementations.

### 1.3 기본 필터 체인 구성

`createFilter()`의 기본 구현이 생성하는 필터 체인 순서:

```
소스: SecurityRealm.java 645~694행 createFilterImpl() 메서드
```

```
+-----------------------------------------------------------+
| 1. HttpSessionContextIntegrationFilter2                   |
|    - HttpSession에서 SecurityContext 복원                   |
|    - allowSessionCreation = false                         |
+-----------------------------------------------------------+
          |
          v
+-----------------------------------------------------------+
| 2. BasicHeaderProcessor                                   |
|    - "Authorization: Basic xxx:yyy" 헤더 처리             |
|    - 실패 시 401 + Basic Auth 요청 (리다이렉트 대신)       |
|    - realmName = "Jenkins"                                |
+-----------------------------------------------------------+
          |
          v
+-----------------------------------------------------------+
| 3. AuthenticationProcessingFilter2                        |
|    - 폼 기반 로그인 처리                                   |
|    - URL: j_spring_security_check (getAuthenticationGatewayUrl)|
|    - SessionFixationProtectionStrategy 적용               |
|    - 성공: AuthenticationSuccessHandler (from 파라미터)    |
|    - 실패: /loginError로 리다이렉트                        |
+-----------------------------------------------------------+
          |
          v
+-----------------------------------------------------------+
| 4. RememberMeAuthenticationFilter                         |
|    - "remember_me" 쿠키 기반 자동 로그인                   |
+-----------------------------------------------------------+
          |
          v
+-----------------------------------------------------------+
| 5. AnonymousAuthenticationFilter                          |
|    - 인증되지 않은 요청에 "anonymous" 인증 부여             |
|    - key="anonymous", principal="anonymous"                |
|    - authority: SimpleGrantedAuthority("anonymous")        |
+-----------------------------------------------------------+
          |
          v
+-----------------------------------------------------------+
| 6. ExceptionTranslationFilter                             |
|    - AuthenticationException → 로그인 페이지로 리다이렉트   |
|    - AccessDeniedException → AccessDeniedHandlerImpl       |
+-----------------------------------------------------------+
          |
          v
+-----------------------------------------------------------+
| 7. UnwrapSecurityExceptionFilter                          |
+-----------------------------------------------------------+
          |
          v
+-----------------------------------------------------------+
| 8. AcegiSecurityExceptionFilter                           |
+-----------------------------------------------------------+
```

이 필터들은 `ChainedServletFilter2`로 묶여 하나의 필터처럼 동작한다.

**왜 BasicAuthenticationEntryPoint의 realmName이 "Jenkins"인가?**
HTTP Basic Auth에서 401 응답 시 `WWW-Authenticate: Basic realm="Jenkins"` 헤더가
전송된다. 프로그래밍 방식의 API 호출(CLI, REST 등)에서 Basic Auth가 실패하면 사람이 아닌
프로그램에게 로그인 폼 리다이렉트 대신 명확한 401 오류를 반환하기 위함이다.

### 1.4 로그아웃 처리

```
소스: SecurityRealm.java 372~427행 doLogoutImpl() 메서드
```

로그아웃 시 수행되는 작업:

```java
void doLogoutImpl(StaplerRequest2 req, StaplerResponse2 rsp)
    throws IOException, ServletException {
    // 1. 세션 무효화
    HttpSession session = req.getSession(false);
    if (session != null) session.invalidate();

    // 2. SecurityContext에서 인증 정보 가져온 후 컨텍스트 클리어
    Authentication auth = SecurityContextHolder.getContext().getAuthentication();
    SecurityContextHolder.clearContext();

    // 3. Remember-Me 쿠키 삭제
    resetRememberMeCookie(req, rsp, contextPath);

    // 4. 모든 JSESSIONID.* 쿠키 삭제
    clearStaleSessionCookies(req, rsp, contextPath);

    // 5. 로그아웃 후 URL로 리다이렉트
    rsp.sendRedirect2(getPostLogOutUrl2(req, auth));
}
```

**왜 JSESSIONID.* 를 모두 삭제하는가?** Jenkins가 다른 설정으로 재시작되었을 수 있고,
이전 incarnation에서 생성된 오래된 세션 쿠키가 남아있을 수 있다. 사용자가 로그아웃하는
이유 중 하나가 세션 정리이므로, 모든 `JSESSIONID.` 접두사 쿠키를 무조건 삭제한다.

### 1.5 NO_AUTHENTICATION 싱글턴

```java
// SecurityRealm.java 757~803행
public static final SecurityRealm NO_AUTHENTICATION = new None();

private static class None extends SecurityRealm {
    @Override
    public SecurityComponents createSecurityComponents() {
        return new SecurityComponents(
            // 모든 인증을 그대로 통과시키는 AuthenticationManager
            (AuthenticationManager) authentication -> authentication,
            // 항상 UsernameNotFoundException을 던지는 UserDetailsService
            username -> { throw new UsernameNotFoundException(username); }
        );
    }

    @Override
    public Filter createFilter(FilterConfig filterConfig) {
        return new ChainedServletFilter2();  // 빈 필터 체인
    }

    // 싱글턴 역직렬화 유지
    private Object readResolve() {
        return NO_AUTHENTICATION;
    }
}
```

보안이 비활성화된 상태에서 사용되는 특수 구현이다. 모든 인증 요청을 그대로 통과시키고,
필터 체인도 비어 있다.

### 1.6 ID 전략 (IdStrategy)

```java
// SecurityRealm.java 185~201행
public IdStrategy getUserIdStrategy() {
    return IdStrategy.CASE_INSENSITIVE;  // 기본값
}

public IdStrategy getGroupIdStrategy() {
    return getUserIdStrategy();  // 기본: 사용자 전략과 동일
}
```

사용자 이름의 대소문자 처리 전략을 결정한다. 기본값은 대소문자 무시(`CaseInsensitive`)이지만,
LDAP 등 외부 시스템 연동 시 `CaseSensitive` 또는 `CaseSensitiveEmailAddress`가
필요할 수 있다.

---

## 2. HudsonPrivateSecurityRealm -- 내장 사용자 DB

### 2.1 클래스 구조

```
소스: core/src/main/java/hudson/security/HudsonPrivateSecurityRealm.java (1188줄)
```

```java
public class HudsonPrivateSecurityRealm
    extends AbstractPasswordBasedSecurityRealm
    implements ModelObject, AccessControlled {

    private final boolean disableSignup;  // true면 가입 불허
    private final boolean enableCaptcha;  // true면 가입 시 CAPTCHA 요구
}
```

Jenkins 자체 사용자 데이터베이스를 사용하는 `SecurityRealm` 구현이다.
`$JENKINS_HOME/users/` 디렉토리에 사용자 정보를 XML로 저장한다.

### 2.2 인증 흐름

```java
// HudsonPrivateSecurityRealm.java 216~229행
@Override
protected UserDetails authenticate2(String username, String password)
    throws AuthenticationException {
    Details u;
    try {
        u = load(username);
    } catch (UsernameNotFoundException ex) {
        // 타이밍 공격 방지: 존재하지 않는 사용자도 비밀번호 비교를 수행
        PASSWORD_ENCODER.matches(password, ENCODED_INVALID_USER_PASSWORD);
        throw ex;
    }
    if (!u.isPasswordCorrect(password)) {
        throw new BadCredentialsException("Bad credentials");
    }
    return u.asUserDetails();
}
```

**왜 타이밍 공격을 방지하는가?** 사용자가 존재하지 않을 때 즉시 예외를 던지면, 공격자는
응답 시간 차이로 유효한 사용자 이름을 알아낼 수 있다. `ENCODED_INVALID_USER_PASSWORD`와
비교하는 무의미한 연산을 추가하여 응답 시간을 균일하게 만든다.

### 2.3 비밀번호 해싱

```
소스: HudsonPrivateSecurityRealm.java 925~1125행
```

Jenkins는 두 가지 비밀번호 해싱 알고리즘을 지원한다.

#### jBCrypt (기본)

```java
// HudsonPrivateSecurityRealm.java 928~969행
static class JBCryptEncoder extends BCryptPasswordEncoder
    implements PasswordHashEncoder {

    private static int MAXIMUM_BCRYPT_LOG_ROUND =
        SystemProperties.getInteger(
            HudsonPrivateSecurityRealm.class.getName()
            + ".maximumBCryptLogRound", 18);

    private static final Pattern BCRYPT_PATTERN =
        Pattern.compile("^\\$2a\\$([0-9]{2})\\$.{53}$");
}
```

- 해시 접두사: `#jbcrypt:`
- 최대 라운드: 18 (기본 BCrypt 라운드에서 18 이하로 제한)
- 비밀번호 최대 길이: 72바이트 (BCrypt 제한)

#### PBKDF2 (FIPS-140 모드)

```java
// HudsonPrivateSecurityRealm.java 971~1060행
static class PBKDF2PasswordEncoder implements PasswordHashEncoder {
    private static final int KEY_LENGTH_BITS = 512;
    private static final int SALT_LENGTH_BYTES = 16;
    private static final int ITTERATIONS = 210_000;
    private static final String PBKDF2_ALGORITHM = "PBKDF2WithHmacSHA512";
}
```

- 해시 접두사: `$PBKDF2`
- 알고리즘: PBKDF2WithHmacSHA512
- 반복 횟수: 210,000 (OWASP 권장 값 기반)
- 솔트: 16바이트 SecureRandom
- 최소 비밀번호 길이: 14자 (FIPS 요구사항)

#### MultiPasswordEncoder -- 어댑터

```java
// HudsonPrivateSecurityRealm.java 1081~1123행
static class MultiPasswordEncoder implements PasswordEncoder {
    @Override
    public String encode(CharSequence rawPassword) {
        return getPasswordHeader() + PASSWORD_HASH_ENCODER.encode(rawPassword);
    }

    @Override
    public boolean matches(CharSequence rawPassword, String encPass) {
        if (isPasswordHashed(encPass)) {
            return PASSWORD_HASH_ENCODER.matches(
                rawPassword, encPass.substring(getPasswordHeader().length()));
        }
        return false;
    }
}
```

`#jbcrypt:` 또는 `$PBKDF2` 접두사로 해시 알고리즘을 구분하며, FIPS-140 모드 여부에 따라
적절한 인코더를 선택한다.

```java
// 인코더 선택 로직
static final PasswordHashEncoder PASSWORD_HASH_ENCODER =
    FIPS140.useCompliantAlgorithms()
        ? new PBKDF2PasswordEncoder()
        : new JBCryptEncoder();
```

### 2.4 첫 번째 관리자 생성

```java
// HudsonPrivateSecurityRealm.java 143~157행
@DataBoundConstructor
public HudsonPrivateSecurityRealm(boolean allowsSignup,
    boolean enableCaptcha, CaptchaSupport captchaSupport) {
    this.disableSignup = !allowsSignup;
    this.enableCaptcha = enableCaptcha;
    setCaptchaSupport(captchaSupport);
    if (!allowsSignup && !hasSomeUser()) {
        // 사용자가 없으면 첫 번째 사용자 생성 필터 추가
        PluginServletFilter.addFilter(CREATE_FIRST_USER_FILTER);
    }
}
```

**왜 첫 번째 사용자 생성 필터가 필요한가?** 보안을 활성화했는데 사용자 계정이 하나도 없으면
아무도 로그인할 수 없는 잠금(lock-out) 상태가 된다. `CREATE_FIRST_USER_FILTER`는
루트 URL(`/`) 또는 `/manage` 접근 시 `/securityRealm/firstUser`로 리다이렉트하여
관리자 계정 생성을 유도한다.

```java
// HudsonPrivateSecurityRealm.java 385~392행
private void tryToMakeAdmin(User u) {
    AuthorizationStrategy as = Jenkins.get().getAuthorizationStrategy();
    for (PermissionAdder adder : ExtensionList.lookup(PermissionAdder.class)) {
        if (adder.add(as, u, Jenkins.ADMINISTER)) {
            return;
        }
    }
}
```

첫 번째 사용자에게 `Jenkins.ADMINISTER` 권한을 부여하기 위해 `PermissionAdder`
확장 포인트를 사용한다.

---

## 3. AuthorizationStrategy -- 인가 (Authorization)

### 3.1 클래스 구조

```
소스: core/src/main/java/hudson/security/AuthorizationStrategy.java (265줄)
```

```java
public abstract class AuthorizationStrategy
    implements Describable<AuthorizationStrategy>, ExtensionPoint {

    // 루트 ACL 반환 -- 모든 다른 ACL의 최종 위임 대상
    public abstract @NonNull ACL getRootACL();

    // 리소스 유형별 ACL (기본값: getRootACL() 위임)
    public @NonNull ACL getACL(@NonNull Job<?, ?> project)     { return getRootACL(); }
    public @NonNull ACL getACL(@NonNull AbstractItem item)     { return getRootACL(); }
    public @NonNull ACL getACL(@NonNull User user)             { return getRootACL(); }
    public @NonNull ACL getACL(@NonNull Computer computer)     { return getACL(computer.getNode()); }
    public @NonNull ACL getACL(@NonNull IComputer computer)    { ... }
    public @NonNull ACL getACL(@NonNull Cloud cloud)           { return getRootACL(); }
    public @NonNull ACL getACL(@NonNull Node node)             { return getRootACL(); }

    // 이 전략에서 사용하는 모든 그룹/역할 이름
    public abstract @NonNull Collection<String> getGroups();
}
```

### 3.2 ACL 위임 계층

```
                        getRootACL()
                            |
         +------------------+------------------+
         |                  |                  |
    getACL(Job)       getACL(User)      getACL(Node)
                                              |
                                        getACL(Computer)
                                              |
                                        getACL(IComputer)
```

기본 구현은 모든 `getACL()` 메서드가 `getRootACL()`로 위임한다. 플러그인이 필요에 따라
특정 리소스 유형에 대해 더 세밀한 ACL을 제공할 수 있다.

### 3.3 View ACL의 특수 로직

```java
// AuthorizationStrategy.java 105~116행
public @NonNull ACL getACL(final @NonNull View item) {
    return ACL.lambda2((a, permission) -> {
        ACL base = item.getOwner().getACL();

        boolean hasPermission = base.hasPermission2(a, permission);
        if (!hasPermission && permission == View.READ) {
            // View.READ 권한이 없더라도:
            // 1. View.CONFIGURE 권한이 있거나
            // 2. View에 아이템이 하나라도 있으면 읽기 허용
            return base.hasPermission2(a, View.CONFIGURE)
                || !item.getItems().isEmpty();
        }

        return hasPermission;
    });
}
```

**왜 View에 특별한 로직이 필요한가?** View는 여러 Job을 그룹화하는 컨테이너이다.
사용자가 View 자체의 READ 권한은 없지만 View에 포함된 Job 중 하나라도 볼 수 있으면,
View도 보여야 한다. 이 로직이 없으면 사용자가 자신이 접근할 수 있는 Job이 어느
View에 속하는지 알 수 없게 된다.

### 3.4 UNSECURED -- 무보안 기본값

```java
// AuthorizationStrategy.java 229~264행
public static final AuthorizationStrategy UNSECURED = new Unsecured();

public static final class Unsecured extends AuthorizationStrategy
    implements Serializable {

    @Override
    public @NonNull ACL getRootACL() {
        return UNSECURED_ACL;
    }

    private static final ACL UNSECURED_ACL = ACL.lambda2((a, p) -> true);
}
```

모든 권한 요청에 `true`를 반환하는 ACL이다. Jenkins 초기 설치 시 또는 보안이
비활성화된 상태에서 사용된다.

---

## 4. ACL -- 접근 제어 목록 (Access Control List)

### 4.1 클래스 구조

```
소스: core/src/main/java/hudson/security/ACL.java (533줄)
```

```java
public abstract class ACL {
    // 핵심 추상 메서드
    public boolean hasPermission2(@NonNull Authentication a,
                                  @NonNull Permission permission) { ... }

    // 편의 메서드
    public final void checkPermission(@NonNull Permission p) { ... }
    public final boolean hasPermission(@NonNull Permission p) { ... }
    public final void checkAnyPermission(@NonNull Permission... permissions) { ... }
    public final boolean hasAnyPermission(@NonNull Permission... permissions) { ... }

    // 정적 팩토리
    public static ACL lambda2(
        BiFunction<Authentication, Permission, Boolean> impl) { ... }

    // 권한 전환
    public static ACLContext as2(@NonNull Authentication auth) { ... }
    public static ACLContext as(@CheckForNull User user) { ... }
}
```

### 4.2 권한 확인 흐름

`checkPermission(Permission)`은 Jenkins 보안의 **가장 빈번하게 호출되는 메서드**이다.

```java
// ACL.java 70~81행
public final void checkPermission(@NonNull Permission p) {
    Authentication a = Jenkins.getAuthentication2();

    // SYSTEM 사용자는 항상 통과
    if (a.equals(SYSTEM2)) {
        return;
    }

    // hasPermission2 호출 -- 핵심 검사
    if (!hasPermission2(a, p)) {
        // 비활성화된 권한이면 impliedBy 체인을 따라감
        while (!p.enabled && p.impliedBy != null) {
            p = p.impliedBy;
        }
        throw new AccessDeniedException3(a, p);
    }
}
```

전체 흐름을 시퀀스 다이어그램으로 표현하면:

```
사용자 요청                AccessControlled           ACL              Permission
    |                          |                      |                    |
    |-- 리소스 접근 시도 ------>|                      |                    |
    |                          |-- checkPermission(p)->|                    |
    |                          |                      |-- getAuthentication2()
    |                          |                      |                    |
    |                          |                      |-- SYSTEM2 확인     |
    |                          |                      |   (맞으면 즉시 통과)|
    |                          |                      |                    |
    |                          |                      |-- hasPermission2(a, p)
    |                          |                      |                    |
    |                          |                      |   [권한 있음] ----->|
    |                          |<-- return ------------|                    |
    |<-- 접근 허용 ------------|                      |                    |
    |                          |                      |                    |
    |                          |                      |   [권한 없음]      |
    |                          |                      |-- p.enabled 확인   |
    |                          |                      |-- p.impliedBy 순회 |
    |                          |                      |-- throw AccessDeniedException3
    |<-- 403 Forbidden --------|<-- 예외 전파 ---------|                    |
```

### 4.3 hasPermission vs checkPermission

| 메서드 | 반환 타입 | 실패 시 동작 |
|--------|----------|-------------|
| `hasPermission(Permission)` | `boolean` | `false` 반환 |
| `checkPermission(Permission)` | `void` | `AccessDeniedException3` 예외 발생 |
| `hasAnyPermission(Permission...)` | `boolean` | 하나라도 있으면 `true` |
| `checkAnyPermission(Permission...)` | `void` | 모두 없으면 예외 발생 |

### 4.4 lambda2() 팩토리 메서드

```java
// ACL.java 197~204행
public static ACL lambda2(
    final BiFunction<Authentication, Permission, Boolean> impl) {
    return new ACL() {
        @Override
        public boolean hasPermission2(Authentication a, Permission permission) {
            return impl.apply(a, permission);
        }
    };
}
```

`ACL`은 `abstract` 클래스이지만 사실상 Single Abstract Method(`hasPermission2`)만
구현하면 된다. `lambda2()`는 이를 람다식으로 간결하게 생성할 수 있게 해준다.

사용 예:

```java
// 모든 권한 허용
ACL unsecured = ACL.lambda2((a, p) -> true);

// View의 특수 ACL
ACL viewAcl = ACL.lambda2((a, permission) -> {
    ACL base = item.getOwner().getACL();
    boolean has = base.hasPermission2(a, permission);
    if (!has && permission == View.READ) {
        return base.hasPermission2(a, View.CONFIGURE)
            || !item.getItems().isEmpty();
    }
    return has;
});
```

### 4.5 SYSTEM2 -- 시스템 인증

```java
// ACL.java 374행
public static final Authentication SYSTEM2 =
    new UsernamePasswordAuthenticationToken(SYSTEM_USERNAME, "SYSTEM");
```

Jenkins가 사용자 대신이 아닌 **자기 자신을 위해** 작업할 때(예: 빌드 실행, 스케줄러 동작)
사용되는 특별한 `Authentication` 객체이다. `SYSTEM2`는 모든 권한 검사를 무조건
통과한다.

```java
// ACL.java 70~74행 -- checkPermission에서
if (a.equals(SYSTEM2)) {
    return;  // 항상 통과
}

// AccessControlled.java 48~51행 -- 인터페이스 기본 구현에서
default void checkPermission(@NonNull Permission permission)
    throws AccessDeniedException {
    if (Jenkins.getAuthentication2().equals(ACL.SYSTEM2)) {
        return;  // 이중 체크
    }
    getACL().checkPermission(permission);
}
```

`SYSTEM2` 검사는 `ACL.checkPermission()`과 `AccessControlled.checkPermission()` 양쪽에서
수행된다. **왜 이중으로 검사하는가?** 코드 주석에 "perhaps redundant given check in
AccessControlled"라고 되어 있듯이, 방어적 프로그래밍이다. `ACL.checkPermission()`이
`AccessControlled`를 경유하지 않고 직접 호출될 수도 있기 때문이다.

### 4.6 ANONYMOUS 및 EVERYONE

```java
// ACL.java 349~358행
public static final String ANONYMOUS_USERNAME = "anonymous";
public static final Sid ANONYMOUS = new PrincipalSid(ANONYMOUS_USERNAME);
public static final Sid EVERYONE = new Sid() {
    @Override
    public String toString() {
        return "EVERYONE";
    }
};
```

| Sid | 의미 | 포함 대상 |
|-----|------|----------|
| `EVERYONE` | 모든 사용자 | 익명 사용자 포함 |
| `ANONYMOUS` | 미인증 사용자 | 로그인하지 않은 사용자만 |

### 4.7 ACLContext -- try-with-resources 권한 전환

```
소스: core/src/main/java/hudson/security/ACLContext.java (75줄)
```

```java
public class ACLContext implements AutoCloseable {
    @NonNull
    private final SecurityContext previousContext;

    ACLContext(@NonNull SecurityContext previousContext) {
        this.previousContext = previousContext;
    }

    @Override
    public void close() {
        SecurityContextHolder.setContext(previousContext);
    }
}
```

사용 패턴:

```java
// ACL.java 476~481행
public static ACLContext as2(@NonNull Authentication auth) {
    final ACLContext context =
        new ACLContext(SecurityContextHolder.getContext());
    SecurityContextHolder.setContext(
        new NonSerializableSecurityContext(auth));
    return context;
}
```

```java
// 사용 예: 시스템 권한으로 임시 작업 수행
try (ACLContext ctx = ACL.as2(ACL.SYSTEM2)) {
    // 이 블록 안에서는 SYSTEM2 권한으로 동작
    jenkins.doSomethingPrivileged();
}
// 블록을 벗어나면 이전 SecurityContext가 자동 복원됨
```

**왜 try-with-resources인가?** 이전에는 `impersonate2(Authentication)` 메서드를 사용하고
`finally` 블록에서 직접 `SecurityContextHolder.setContext(old)`를 호출해야 했다.
이 패턴은 실수로 복원을 빠뜨릴 위험이 있었다. `ACLContext`의 `AutoCloseable` 구현으로
컨텍스트 복원이 보장된다.

```java
// ACL.java 510~512행
public static ACLContext as(@CheckForNull User user) {
    return as2(user == null
        ? Jenkins.ANONYMOUS2
        : user.impersonate2());
}
```

`User` 객체를 직접 전달할 수도 있다. `null`이면 익명 사용자로 간주된다.

---

## 5. Permission -- 권한

### 5.1 클래스 구조

```
소스: core/src/main/java/hudson/security/Permission.java (359줄)
```

```java
public final class Permission {
    public final @NonNull Class owner;              // 소유 클래스
    public final @NonNull PermissionGroup group;    // 소속 그룹
    public final @NonNull String name;              // 식별 이름
    public final @CheckForNull Localizable description;  // 설명
    public final @CheckForNull Permission impliedBy;     // 암시적 상위 권한
    public boolean enabled;                         // 활성화 여부
    private final @NonNull Set<PermissionScope> scopes;  // 적용 범위
    private final @CheckForNull String id;          // "owner.name" 형식 ID
}
```

### 5.2 권한 ID 체계

```java
// Permission.java 142~158행 생성자
public Permission(@NonNull PermissionGroup group, @NonNull String name,
        @CheckForNull Localizable description,
        @CheckForNull Permission impliedBy,
        boolean enable,
        @NonNull PermissionScope[] scopes) {
    if (!JSONUtils.isJavaIdentifier(name))
        throw new IllegalArgumentException(name + " is not a Java identifier");
    this.owner = group.owner;
    this.group = group;
    this.name = name;
    this.description = description;
    this.impliedBy = impliedBy;
    this.enabled = enable;
    this.scopes = Set.of(scopes);
    this.id = owner.getName() + '.' + name;

    group.add(this);      // PermissionGroup에 등록
    ALL.add(this);        // 전역 목록에 등록
}
```

**ID 형식**: `"패키지명.클래스명.권한이름"` (예: `hudson.model.Hudson.Administer`)

```java
// Permission.java 240~253행 fromId()
public static @CheckForNull Permission fromId(@NonNull String id) {
    int idx = id.lastIndexOf('.');
    if (idx < 0) return null;

    try {
        Class cl = Class.forName(id.substring(0, idx), true,
            Jenkins.get().getPluginManager().uberClassLoader);
        PermissionGroup g = PermissionGroup.get(cl);
        if (g == null) return null;
        return g.find(id.substring(idx + 1));
    } catch (ClassNotFoundException e) {
        return null;
    }
}
```

`fromId()`는 문자열 ID를 다시 `Permission` 객체로 변환한다. **왜 `uberClassLoader`를
사용하는가?** 플러그인이 정의한 권한도 찾을 수 있어야 하기 때문이다. `uberClassLoader`는
Jenkins 코어와 모든 플러그인의 클래스를 로드할 수 있는 통합 클래스로더이다.

### 5.3 impliedBy -- 암시적 권한 관계

```
Permission.java 84~97행 impliedBy 필드 주석:

"This allows us to organize permissions in a hierarchy, so that
for example we can say 'view workspace' permission is implied by
the (broader) 'read' permission."
```

`impliedBy`는 권한의 **계층 구조**를 정의한다. 상위 권한이 부여되면 하위 권한은
자동으로 부여된 것으로 간주된다.

```
Jenkins.ADMINISTER
    |
    +-- Permission.FULL_CONTROL
    |
    +-- Permission.READ (GenericRead)
    |
    +-- Permission.WRITE (GenericWrite)
          |
          +-- Permission.CREATE (GenericCreate)
          |
          +-- Permission.UPDATE (GenericUpdate)
          |     |
          |     +-- Permission.CONFIGURE (GenericConfigure)
          |
          +-- Permission.DELETE (GenericDelete)
```

소스 코드의 실제 정의:

```java
// Permission.java 290~358행
public static final PermissionGroup HUDSON_PERMISSIONS =
    new PermissionGroup(Hudson.class,
        hudson.model.Messages._Hudson_Permissions_Title());

public static final Permission HUDSON_ADMINISTER =
    new Permission(HUDSON_PERMISSIONS, "Administer",
        hudson.model.Messages._Hudson_AdministerPermission_Description(),
        null);  // impliedBy = null (최상위)

public static final PermissionGroup GROUP =
    new PermissionGroup(Permission.class,
        Messages._Permission_Permissions_Title());

public static final Permission FULL_CONTROL =
    new Permission(GROUP, "FullControl", null, HUDSON_ADMINISTER);

public static final Permission READ =
    new Permission(GROUP, "GenericRead", null, HUDSON_ADMINISTER);

public static final Permission WRITE =
    new Permission(GROUP, "GenericWrite", null, HUDSON_ADMINISTER);

public static final Permission CREATE =
    new Permission(GROUP, "GenericCreate", null, WRITE);

public static final Permission UPDATE =
    new Permission(GROUP, "GenericUpdate", null, WRITE);

public static final Permission DELETE =
    new Permission(GROUP, "GenericDelete", null, WRITE);

public static final Permission CONFIGURE =
    new Permission(GROUP, "GenericConfigure", null, UPDATE);
```

### 5.4 enabled -- 동적 비활성화

```java
// Permission.java 99~110행
public boolean enabled;
```

`enabled` 필드를 `false`로 설정하면 해당 권한이 UI 매트릭스에서 숨겨진다.
그러나 **impliedBy 체인은 여전히 동작**한다.

```java
// ACL.java 76~78행 checkPermission에서
if (!hasPermission2(a, p)) {
    while (!p.enabled && p.impliedBy != null) {
        p = p.impliedBy;
    }
    throw new AccessDeniedException3(a, p);
}
```

비활성화된 권한으로 인해 접근이 거부되면, `impliedBy` 체인을 따라 활성화된 상위 권한을
찾아 에러 메시지에 표시한다. 이는 사용자에게 "어떤 권한이 필요한지"를 정확히 알려주기
위함이다.

### 5.5 PermissionScope -- 권한의 적용 범위

```
소스: core/src/main/java/hudson/security/PermissionScope.java (117줄)
```

```java
public final class PermissionScope {
    public final Class<? extends ModelObject> modelClass;
    private final Set<PermissionScope> containers;

    // 내장 스코프
    public static final PermissionScope JENKINS  = new PermissionScope(Jenkins.class);
    public static final PermissionScope ITEM_GROUP = new PermissionScope(ItemGroup.class, JENKINS);
    public static final PermissionScope ITEM     = new PermissionScope(Item.class, ITEM_GROUP);
    public static final PermissionScope RUN      = new PermissionScope(Run.class, ITEM);
    public static final PermissionScope COMPUTER = new PermissionScope(Computer.class, JENKINS);
}
```

스코프 포함 관계:

```
JENKINS
  |
  +-- ITEM_GROUP
  |     |
  |     +-- ITEM
  |           |
  |           +-- RUN
  |
  +-- COMPUTER
```

```java
// PermissionScope.java 80~87행
public boolean isContainedBy(PermissionScope s) {
    if (this == s) return true;
    for (PermissionScope c : containers) {
        if (c.isContainedBy(s))
            return true;
    }
    return false;
}
```

**왜 스코프가 필요한가?** "Item 생성" 권한은 `ITEM_GROUP` 스코프에 속한다.
이 권한을 `RUN` 레벨에서 설정하는 것은 의미가 없다. `PermissionScope`는
어떤 권한이 어떤 수준에서 설정 가능한지를 제어하여, 권한 매트릭스 UI가 불필요하게
복잡해지는 것을 방지한다.

---

## 6. PermissionGroup -- 권한 그룹

### 6.1 클래스 구조

```
소스: core/src/main/java/hudson/security/PermissionGroup.java (195줄)
```

```java
public final class PermissionGroup
    implements Iterable<Permission>, Comparable<PermissionGroup> {

    private final SortedSet<Permission> permissions =
        new TreeSet<>(Permission.ID_COMPARATOR);

    @NonNull
    public final Class owner;
    public final Localizable title;
    private final String id;
}
```

### 6.2 전역 레지스트리

```java
// PermissionGroup.java 162~194행
private static final SortedSet<PermissionGroup> PERMISSIONS = new TreeSet<>();

private static synchronized void register(PermissionGroup g) {
    if (!PERMISSIONS.add(g)) {
        throw new IllegalStateException(
            "attempt to register a second PermissionGroup for "
            + g.getOwnerClassName());
    }
}

public static synchronized List<PermissionGroup> getAll() {
    return new ArrayList<>(PERMISSIONS);
}

public static synchronized @CheckForNull PermissionGroup get(Class owner) {
    for (PermissionGroup g : PERMISSIONS) {
        if (g.owner == owner) {
            return g;
        }
    }
    return null;
}
```

`PermissionGroup`은 `TreeSet`으로 관리되며, 정렬 기준은 `compareTo()`이다:

```java
// PermissionGroup.java 130~144행
@Override
public int compareTo(PermissionGroup that) {
    int r = this.compareOrder() - that.compareOrder();
    if (r != 0) return r;
    return getOwnerClassName().compareTo(that.getOwnerClassName());
}

private int compareOrder() {
    if (owner == Hudson.class) return 0;  // Hudson 권한이 항상 최상위
    return 1;
}
```

**왜 Hudson.class가 항상 먼저인가?** 권한 매트릭스 UI에서 `Jenkins.ADMINISTER`를 포함한
시스템 전역 권한이 최상단에 표시되어야 직관적이기 때문이다.

### 6.3 Permission과의 관계

```
PermissionGroup                  Permission
+-----------+                   +-----------+
| owner     |<--owner---------- | owner     |
| title     |                   | group     |----> PermissionGroup
| id        |                   | name      |
| permissions|--add(p)--------> | impliedBy |
+-----------+                   | enabled   |
     |                          | scopes    |
     |  getAll()                +-----------+
     v                               |
[전역 레지스트리                      | getAll()
 SortedSet<PermissionGroup>]          v
                                [전역 레지스트리
                                 List<Permission>]
```

`Permission` 생성자가 호출되면:
1. `group.add(this)` -- 소속 `PermissionGroup`에 등록
2. `ALL.add(this)` -- 전역 `Permission` 목록에 등록

이 두 레지스트리를 통해 Jenkins는 런타임에 모든 권한과 권한 그룹을 열거할 수 있다.

---

## 7. AccessDeniedException3 -- 권한 거부 예외

```
소스: core/src/main/java/hudson/security/AccessDeniedException3.java (92줄)
```

```java
public class AccessDeniedException3 extends AccessDeniedException {
    public final Authentication authentication;
    public final Permission permission;

    public AccessDeniedException3(Authentication authentication,
                                  Permission permission) {
        super(Messages.AccessDeniedException2_MissingPermission(
            authentication.getName(),
            permission.group.title + "/" + permission.name));
        this.authentication = authentication;
        this.permission = permission;
    }
}
```

### 7.1 디버깅 정보 제공

```java
// AccessDeniedException3.java 47~72행
public void reportAsHeaders(HttpServletResponse rsp) {
    rsp.addHeader("X-You-Are-Authenticated-As", authentication.getName());
    if (REPORT_GROUP_HEADERS) {
        for (GrantedAuthority auth : authentication.getAuthorities()) {
            rsp.addHeader("X-You-Are-In-Group", auth.getAuthority());
        }
    } else {
        rsp.addHeader("X-You-Are-In-Group-Disabled",
            "JENKINS-39402: use -D...REPORT_GROUP_HEADERS=true "
            + "or use /whoAmI to diagnose");
    }
    rsp.addHeader("X-Required-Permission", permission.getId());
    for (Permission p = permission.impliedBy; p != null; p = p.impliedBy) {
        rsp.addHeader("X-Permission-Implied-By", p.getId());
    }
}
```

HTTP 응답 헤더에 포함되는 디버깅 정보:

| 헤더 | 내용 |
|------|------|
| `X-You-Are-Authenticated-As` | 현재 인증된 사용자 이름 |
| `X-You-Are-In-Group` | 사용자가 속한 그룹 (기본 비활성) |
| `X-Required-Permission` | 필요한 권한 ID |
| `X-Permission-Implied-By` | impliedBy 체인의 각 상위 권한 |

**왜 X-You-Are-In-Group이 기본 비활성인가?** JENKINS-39402 이슈에 따르면,
그룹 정보를 HTTP 헤더에 노출하면 보안 위험이 있다. 대신 `/whoAmI` 페이지를
사용하여 진단할 것을 권장한다.

---

## 8. AccessControlled 인터페이스

```
소스: core/src/main/java/hudson/security/AccessControlled.java (105줄)
```

```java
public interface AccessControlled {
    @NonNull ACL getACL();

    default void checkPermission(@NonNull Permission permission)
        throws AccessDeniedException {
        if (Jenkins.getAuthentication2().equals(ACL.SYSTEM2)) {
            return;
        }
        getACL().checkPermission(permission);
    }

    default boolean hasPermission(@NonNull Permission permission) {
        if (Jenkins.getAuthentication2().equals(ACL.SYSTEM2)) {
            return true;
        }
        return getACL().hasPermission(permission);
    }

    default void checkAnyPermission(@NonNull Permission... permission)
        throws AccessDeniedException {
        getACL().checkAnyPermission(permission);
    }

    default boolean hasAnyPermission(@NonNull Permission... permission) {
        return getACL().hasAnyPermission(permission);
    }

    default boolean hasPermission2(@NonNull Authentication a,
                                   @NonNull Permission permission) {
        if (a.equals(ACL.SYSTEM2)) {
            return true;
        }
        return getACL().hasPermission2(a, permission);
    }
}
```

Jenkins의 모든 보안 대상 모델 객체(`Jenkins`, `Job`, `View`, `Node`, `Computer` 등)가
이 인터페이스를 구현한다. `getACL()`만 구현하면 나머지 권한 검사 메서드는 기본 구현이
제공된다.

---

## 9. Spring Security 통합

### 9.1 SecurityContextHolder

Jenkins는 Spring Security의 `SecurityContextHolder`를 사용하여 현재 스레드의
인증 정보를 관리한다.

```java
// SecurityRealm.java 376~377행 (로그아웃 시)
Authentication auth = SecurityContextHolder.getContext().getAuthentication();
SecurityContextHolder.clearContext();

// ACL.java 478~480행 (권한 전환 시)
SecurityContextHolder.setContext(new NonSerializableSecurityContext(auth));
```

### 9.2 Jenkins.getAuthentication2()

`ACL` 클래스의 여러 메서드에서 현재 인증 정보를 얻을 때
`Jenkins.getAuthentication2()`를 호출한다.

```java
// ACL.java 71행
Authentication a = Jenkins.getAuthentication2();
```

이 메서드는 `SecurityContextHolder.getContext().getAuthentication()`을 래핑하며,
인증 정보가 없으면 `Jenkins.ANONYMOUS2`를 반환한다.

### 9.3 NonSerializableSecurityContext

```java
// ACL.java 479행
SecurityContextHolder.setContext(new NonSerializableSecurityContext(auth));
```

**왜 NonSerializableSecurityContext인가?** Spring Security의 기본
`SecurityContextImpl`은 `Serializable`이다. 그러나 같은 세션의 동시 요청이
같은 `SecurityContext` 객체를 공유하므로, `impersonate`로 인증을 변경하면
다른 요청에도 영향을 줄 수 있다. `NonSerializableSecurityContext`는 **새로운
객체**를 생성하여 이 문제를 방지한다. JavaDoc 원문:

> We need to create a new SecurityContext instead of
> SecurityContext.setAuthentication() because the same SecurityContext
> object is reused for all the concurrent requests from the same session.

---

## 10. CSRF 보호 (CrumbIssuer)

### 10.1 CrumbIssuer 추상 클래스

```
소스: core/src/main/java/hudson/security/csrf/CrumbIssuer.java (306줄)
```

```java
public abstract class CrumbIssuer
    implements Describable<CrumbIssuer>, ExtensionPoint {

    // Crumb 필드 이름 (기본: "Jenkins-Crumb")
    public static final String DEFAULT_CRUMB_NAME = "Jenkins-Crumb";

    // Crumb 발급
    public String getCrumb(ServletRequest request) { ... }

    // Crumb 생성 (서브클래스 구현)
    protected String issueCrumb(ServletRequest request, String salt) { ... }

    // Crumb 검증
    public boolean validateCrumb(ServletRequest request, String salt,
                                 String crumb) { ... }
}
```

### 10.2 Crumb 발급 흐름

```java
// CrumbIssuer.java 78~95행
public String getCrumb(ServletRequest request) {
    String crumb = null;
    if (request != null) {
        // 1. 요청 속성에서 캐시된 crumb 확인
        crumb = (String) request.getAttribute(CRUMB_ATTRIBUTE);
    }
    if (crumb == null) {
        // 2. 없으면 새로 생성
        crumb = issueCrumb(request, getDescriptor().getCrumbSalt());
        if (request != null) {
            if (crumb != null && !crumb.isEmpty()) {
                // 3. 요청 속성에 캐시
                request.setAttribute(CRUMB_ATTRIBUTE, crumb);
            } else {
                request.removeAttribute(CRUMB_ATTRIBUTE);
            }
        }
    }
    return crumb;
}
```

**왜 요청 속성에 캐시하는가?** 같은 요청 내에서 crumb이 여러 번 필요할 수 있다
(예: 폼에 여러 개의 숨겨진 필드). 해시 계산을 반복하지 않기 위해 캐시한다.

### 10.3 DefaultCrumbIssuer -- 기본 구현

```
소스: core/src/main/java/hudson/security/csrf/DefaultCrumbIssuer.java (198줄)
```

```java
public class DefaultCrumbIssuer extends CrumbIssuer {
    @Override
    protected synchronized String issueCrumb(ServletRequest request, String salt) {
        if (request instanceof HttpServletRequest req) {
            if (md != null) {
                StringBuilder buffer = new StringBuilder();
                Authentication a = Jenkins.getAuthentication2();

                // 1. 사용자 이름 포함
                buffer.append(a.getName());

                // 2. 세션 ID 포함 (EXCLUDE_SESSION_ID가 아닌 경우)
                if (!EXCLUDE_SESSION_ID) {
                    buffer.append(';');
                    buffer.append(req.getSession().getId());
                }

                // 3. SHA-256 해시 생성
                md.update(buffer.toString().getBytes(StandardCharsets.UTF_8));
                return Util.toHexString(
                    md.digest(salt.getBytes(StandardCharsets.US_ASCII)));
            }
        }
        return null;
    }
}
```

Crumb = `SHA-256(사용자이름 + ";" + 세션ID, salt)`

### 10.4 Crumb 검증

```java
// DefaultCrumbIssuer.java 123~134행
@Override
public boolean validateCrumb(ServletRequest request, String salt, String crumb) {
    if (request instanceof HttpServletRequest) {
        String newCrumb = issueCrumb(request, salt);
        if (newCrumb != null && crumb != null) {
            // 상수 시간 비교 (타이밍 공격 방지)
            return MessageDigest.isEqual(
                newCrumb.getBytes(StandardCharsets.US_ASCII),
                crumb.getBytes(StandardCharsets.US_ASCII));
        }
    }
    return false;
}
```

**왜 `String.equals()` 대신 `MessageDigest.isEqual()`인가?** 코드 주석에 명시:

> String.equals() is not constant-time, but this is

`String.equals()`는 첫 번째 불일치 문자에서 즉시 반환하므로, 공격자가 응답 시간 차이로
올바른 crumb을 한 글자씩 알아낼 수 있다. `MessageDigest.isEqual()`은 항상 전체
바이트를 비교하여 타이밍 공격을 방지한다.

### 10.5 솔트(Salt)

```java
// DefaultCrumbIssuer.DescriptorImpl 137~143행
public static final class DescriptorImpl extends CrumbIssuerDescriptor<DefaultCrumbIssuer>
    implements ModelObject, PersistentDescriptor {

    private static final HexStringConfidentialKey CRUMB_SALT =
        new HexStringConfidentialKey(Jenkins.class, "crumbSalt", 16);

    public DescriptorImpl() {
        super(CRUMB_SALT.get(),
              SystemProperties.getString(
                  "hudson.security.csrf.requestfield",
                  CrumbIssuer.DEFAULT_CRUMB_NAME));
    }
}
```

솔트는 `HexStringConfidentialKey`로 생성되며, `$JENKINS_HOME/secrets/` 디렉토리에
안전하게 저장된다. 16바이트 랜덤 16진수 문자열이다.

### 10.6 Stapler 통합

```java
// CrumbIssuer.java 246~265행
@Initializer
public static void initStaplerCrumbIssuer() {
    WebApp.get(Jenkins.get().getServletContext()).setCrumbIssuer(
        new org.kohsuke.stapler.CrumbIssuer() {
            @Override
            public String issueCrumb(StaplerRequest2 request) {
                CrumbIssuer ci = Jenkins.get().getCrumbIssuer();
                return ci != null
                    ? ci.getCrumb(request)
                    : DEFAULT.issueCrumb(request);
            }

            @Override
            public void validateCrumb(StaplerRequest2 request,
                                      String submittedCrumb) {
                CrumbIssuer ci = Jenkins.get().getCrumbIssuer();
                if (ci == null) {
                    DEFAULT.validateCrumb(request, submittedCrumb);
                } else {
                    if (!ci.validateCrumb(request,
                            ci.getDescriptor().getCrumbSalt(),
                            submittedCrumb))
                        throw new SecurityException("Crumb didn't match");
                }
            }
        });
}
```

Jenkins 초기화 시 Stapler 웹 프레임워크에 `CrumbIssuer`를 등록한다. 이후
Stapler가 처리하는 모든 POST 요청에서 자동으로 crumb 검증이 수행된다.

---

## 11. 권한 확인 전체 흐름 -- 종합 시퀀스

사용자가 Job의 빌드 버튼을 클릭했을 때의 전체 보안 흐름:

```
[브라우저]                     [Jenkins 서버]
    |
    |-- POST /job/my-job/build  (Jenkins-Crumb: abc123)
    |
    |                           [CrumbFilter]
    |                              |
    |                              |-- validateCrumb(request, salt, "abc123")
    |                              |   SHA-256(username + ";" + sessionId, salt)
    |                              |   == "abc123" ? (MessageDigest.isEqual)
    |                              |
    |                              |-- [통과]
    |                              v
    |                           [SecurityRealm 필터 체인]
    |                              |
    |                              |-- HttpSessionContextIntegrationFilter2
    |                              |   HttpSession에서 SecurityContext 복원
    |                              |
    |                              |-- BasicHeaderProcessor (해당 없으면 통과)
    |                              |
    |                              |-- AuthenticationProcessingFilter2 (해당 없으면 통과)
    |                              |
    |                              |-- RememberMeAuthenticationFilter
    |                              |
    |                              |-- AnonymousAuthenticationFilter
    |                              |   (이미 인증됨 → 통과)
    |                              |
    |                              |-- ExceptionTranslationFilter
    |                              v
    |                           [Stapler 라우팅]
    |                              |
    |                              |-- Job.doBuild()
    |                              |
    |                              v
    |                           [Job.checkPermission(Job.BUILD)]
    |                              |
    |                              |-- AccessControlled.checkPermission()
    |                              |   |
    |                              |   |-- SYSTEM2 확인 → 아니면 계속
    |                              |   |
    |                              |   |-- getACL().checkPermission(Job.BUILD)
    |                              |      |
    |                              |      |-- Jenkins.getAuthentication2() → "alice"
    |                              |      |
    |                              |      |-- SYSTEM2 확인 → 아님
    |                              |      |
    |                              |      |-- hasPermission2("alice", Job.BUILD)
    |                              |      |   |
    |                              |      |   |-- AuthorizationStrategy
    |                              |      |   |   .getACL(job)
    |                              |      |   |   .hasPermission2("alice", Job.BUILD)
    |                              |      |   |
    |                              |      |   |-- [권한 있음] → return true
    |                              |      |   |-- [권한 없음] → return false
    |                              |      |
    |                              |      |-- [true] → return (접근 허용)
    |                              |      |-- [false] → throw AccessDeniedException3
    |                              |
    |                              v
    |<-- 200 OK (빌드 시작)  또는  403 Forbidden
```

---

## 12. 보안 설계 원칙 요약

### 12.1 관심사의 분리 (Separation of Concerns)

| 계층 | 클래스 | 역할 |
|------|--------|------|
| 인증 (Authentication) | `SecurityRealm` | "누구인가?" |
| 인가 (Authorization) | `AuthorizationStrategy` | "무엇을 할 수 있는가?" |
| 접근 제어 (Access Control) | `ACL` | "이 요청을 허용하는가?" |
| 권한 정의 (Permission) | `Permission`, `PermissionGroup` | "어떤 작업이 가능한가?" |
| CSRF 방어 | `CrumbIssuer` | "이 요청이 정당한가?" |

### 12.2 확장성 (Extensibility)

모든 핵심 클래스가 `ExtensionPoint`를 구현하므로 플러그인으로 교체 가능하다.

| 확장 포인트 | 대표 플러그인 |
|-------------|-------------|
| `SecurityRealm` | LDAP, Active Directory, OIDC, SAML |
| `AuthorizationStrategy` | Matrix Authorization, Role Strategy |
| `CrumbIssuer` | Strict Crumb Issuer |

### 12.3 방어적 프로그래밍 패턴

Jenkins 보안 코드에서 반복적으로 나타나는 방어적 패턴들:

1. **SYSTEM2 이중 체크**: `AccessControlled`와 `ACL` 양쪽에서 검사
2. **타이밍 공격 방지**: `MessageDigest.isEqual()` 사용, 존재하지 않는 사용자도 비밀번호 비교 수행
3. **세션 고정 공격 방지**: `SessionFixationProtectionStrategy` 적용, 로그인 시 세션 무효화 후 재생성
4. **Remember-Me 쿠키 보안**: `HttpOnly`, `Secure` 플래그 설정
5. **impliedBy 체인 에러 메시지**: 비활성화된 권한의 경우 활성화된 상위 권한을 찾아 표시

### 12.4 Acegi에서 Spring Security로의 마이그레이션

소스 코드에서 `@Deprecated` 메서드가 매우 많은 이유는 Jenkins가 **Acegi Security
(구 Spring Security의 전신)**에서 **Spring Security**로 점진적으로 마이그레이션했기
때문이다. 호환성을 위해 양쪽 API를 모두 유지하며, 새 코드는 `*2` 접미사 메서드
(예: `hasPermission2`, `getAuthentication2`, `as2`)를 사용한다.

```java
// 예: ACL.java의 패턴
public boolean hasPermission2(@NonNull Authentication a, @NonNull Permission permission) {
    // Spring Security API
}

@Deprecated
public boolean hasPermission(@NonNull org.acegisecurity.Authentication a,
    @NonNull Permission permission) {
    return hasPermission2(a.toSpring(), permission);
    // Acegi API → Spring Security로 위임
}
```

이 마이그레이션 전략 덕분에 수천 개의 플러그인이 점진적으로 새 API로 전환할 수 있다.

---

## 참조 파일 목록

| 파일 경로 | 줄 수 | 역할 |
|-----------|------|------|
| `core/src/main/java/hudson/security/SecurityRealm.java` | 942 | 인증 백엔드 추상화 |
| `core/src/main/java/hudson/security/AuthorizationStrategy.java` | 265 | 인가 전략 추상화 |
| `core/src/main/java/hudson/security/ACL.java` | 533 | 접근 제어 목록 |
| `core/src/main/java/hudson/security/Permission.java` | 359 | 권한 정의 |
| `core/src/main/java/hudson/security/PermissionGroup.java` | 195 | 권한 그룹 |
| `core/src/main/java/hudson/security/PermissionScope.java` | 117 | 권한 적용 범위 |
| `core/src/main/java/hudson/security/ACLContext.java` | 75 | try-with-resources 권한 전환 |
| `core/src/main/java/hudson/security/AccessDeniedException3.java` | 92 | 상세 접근 거부 예외 |
| `core/src/main/java/hudson/security/AccessControlled.java` | 105 | 보안 대상 인터페이스 |
| `core/src/main/java/hudson/security/HudsonPrivateSecurityRealm.java` | 1188 | 내장 사용자 DB |
| `core/src/main/java/hudson/security/csrf/CrumbIssuer.java` | 306 | CSRF 보호 추상화 |
| `core/src/main/java/hudson/security/csrf/DefaultCrumbIssuer.java` | 198 | 기본 CSRF 구현 |
