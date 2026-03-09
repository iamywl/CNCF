# 22. Setup Wizard 시스템 Deep-Dive

## 1. 개요

Jenkins Setup Wizard는 **최초 설치 시** 사용자를 안내하여 기본 보안 설정,
관리자 계정 생성, 추천 플러그인 설치, 인스턴스 URL 설정 등을 수행하는 시스템이다.

### 왜(Why) 이 서브시스템이 존재하는가?

Jenkins 2.0 이전에는 최초 설치 후 **보안이 전혀 설정되지 않은** 상태로 시작했다:
- 인증 없이 누구나 접근 가능
- CSRF 보호 없음
- 필요한 플러그인을 수동으로 검색하여 설치해야 함

이로 인해 발생한 문제:
1. **보안 사고**: 인터넷에 노출된 Jenkins가 무방비 상태
2. **사용성 저하**: 초보자가 필수 플러그인을 모름
3. **설정 누락**: 중요 설정(URL, CSRF)을 빠뜨림

Setup Wizard(Jenkins 2.0+)는 이 모든 문제를 해결한다:
- **초기 보안 강제**: 랜덤 비밀번호로 관리자 계정 자동 생성
- **가이드 설치**: 추천/선택 플러그인 설치 UI
- **필수 설정**: URL, 관리자 계정 등 핵심 설정 안내

## 2. InstallState 상태 기계

Jenkins의 설치 상태는 `InstallState` enum 계열 객체로 관리된다.

### 상태 전이 다이어그램

```
                    ┌──────────┐
                    │ UNKNOWN  │ ← 시작점
                    └────┬─────┘
                         │ initializeState()
                    ┌────▼─────┐
            ┌───────┤ 분기 로직 ├───────┐
            │       └──────────┘       │
     (신규 설치)                  (기존 설치)
            │                          │
     ┌──────▼──────┐           ┌──────▼──────┐
     │    NEW      │           │  RESTART    │
     └──────┬──────┘           └──────┬──────┘
            │                         │
     ┌──────▼──────────┐       ┌──────▼──────┐
     │ INITIAL_SECURITY│       │  UPGRADE    │
     │ _SETUP          │       └──────┬──────┘
     └──────┬──────────┘              │
            │                  ┌──────▼──────┐
     ┌──────▼──────────────┐   │  DOWNGRADE  │
     │ INITIAL_PLUGINS_    │   └──────┬──────┘
     │ INSTALLING          │          │
     └──────┬──────────────┘          │
            │                         │
     ┌──────▼──────────┐              │
     │ CREATE_ADMIN_   │              │
     │ USER            │              │
     └──────┬──────────┘              │
            │                         │
     ┌──────▼──────────┐              │
     │ CONFIGURE_      │              │
     │ INSTANCE        │              │
     └──────┬──────────┘              │
            │                         │
     ┌──────▼──────────────┐          │
     │ INITIAL_SETUP_      │          │
     │ COMPLETED           │          │
     └──────┬──────────────┘          │
            │                         │
            └────────┬────────────────┘
                     │
              ┌──────▼──────┐
              │   RUNNING   │
              └─────────────┘
```

### InstallState 상세

**경로**: `core/src/main/java/jenkins/install/InstallState.java`

```java
@StaplerAccessibleType
public class InstallState implements ExtensionPoint {
    private final transient boolean isSetupComplete;
    private final String name;

    public InstallState(String name, boolean isSetupComplete) {
        this.name = name;
        this.isSetupComplete = isSetupComplete;
    }

    // 각 상태의 초기화 로직
    public void initializeState() { }
}
```

#### 주요 상태 정의

| 상태 | isSetupComplete | 설명 |
|------|----------------|------|
| `UNKNOWN` | true | 초기 상태, 즉시 분기 |
| `NEW` | false | 신규 설치 |
| `INITIAL_SECURITY_SETUP` | false | 보안 초기화 (admin 생성) |
| `INITIAL_PLUGINS_INSTALLING` | false | 플러그인 설치 중 |
| `CREATE_ADMIN_USER` | false | 관리자 계정 생성 |
| `CONFIGURE_INSTANCE` | false | 인스턴스 URL 설정 |
| `INITIAL_SETUP_COMPLETED` | true | 설정 완료 |
| `RUNNING` | true | 정상 운영 |
| `RESTART` | true | 기존 설치 재시작 |
| `UPGRADE` | true | 업그레이드 |
| `DOWNGRADE` | true | 다운그레이드 |

**`isSetupComplete` 플래그의 의미**:
- `false`: Setup Wizard 필터가 활성화되어 모든 요청을 위자드로 리다이렉트
- `true`: 정상적인 Jenkins UI 접근 허용

#### INITIAL_SECURITY_SETUP — 최초 보안 설정

```java
private static final class InitialSecuritySetup extends InstallState {
    @Override
    public void initializeState() {
        Jenkins.get().getSetupWizard().init(true);  // 관리자 계정 생성
        InstallUtil.proceedToNextStateFrom(INITIAL_SECURITY_SETUP);
    }
}
```

#### CREATE_ADMIN_USER — 관리자 계정 생성 화면

```java
private static final class CreateAdminUser extends InstallState {
    @Override
    public void initializeState() {
        Jenkins j = Jenkins.get();
        // 보안 기본값을 사용 중이지 않으면 건너뛰기
        if (!j.getSetupWizard().isUsingSecurityDefaults()) {
            InstallUtil.proceedToNextStateFrom(this);
        }
    }
}
```

#### CONFIGURE_INSTANCE — URL 설정

```java
private static final class ConfigureInstance extends InstallState {
    @Override
    public void initializeState() {
        String url = JenkinsLocationConfiguration.getOrDie().getUrl();
        if (url != null && !url.isBlank()) {
            InstallUtil.proceedToNextStateFrom(this);  // 이미 설정됨 → 건너뛰기
        }
    }
}
```

## 3. SetupWizard 핵심 분석

**경로**: `core/src/main/java/jenkins/install/SetupWizard.java`

### 3.1 초기화 (init 메서드)

```java
@Extension
public class SetupWizard extends PageDecorator {

    void init(boolean newInstall) throws IOException, InterruptedException {
        Jenkins jenkins = Jenkins.get();

        if (newInstall) {
            FilePath iapf = getInitialAdminPasswordFile();

            if (jenkins.getSecurityRealm() == SecurityRealm.NO_AUTHENTICATION) {
                try (BulkChange bc = new BulkChange(jenkins)) {
                    // 1. HudsonPrivateSecurityRealm 설정
                    HudsonPrivateSecurityRealm securityRealm =
                        new HudsonPrivateSecurityRealm(false, false, null);
                    jenkins.setSecurityRealm(securityRealm);

                    // 2. 랜덤 비밀번호 생성
                    String randomUUID = UUID.randomUUID().toString()
                        .replace("-", "").toLowerCase(Locale.ENGLISH);

                    // 3. admin 계정 생성
                    securityRealm.createAccount("admin", randomUUID);

                    // 4. 비밀번호를 파일에 저장
                    iapf.touch(System.currentTimeMillis());
                    iapf.chmod(0640);  // 그룹 읽기 허용
                    iapf.write(randomUUID + System.lineSeparator(), "UTF-8");

                    // 5. 인증 전략 설정
                    FullControlOnceLoggedInAuthorizationStrategy authStrategy =
                        new FullControlOnceLoggedInAuthorizationStrategy();
                    authStrategy.setAllowAnonymousRead(false);
                    jenkins.setAuthorizationStrategy(authStrategy);

                    // 6. JNLP 비활성화
                    jenkins.setSlaveAgentPort(-1);

                    // 7. CSRF 보호 활성화
                    jenkins.setCrumbIssuer(
                        GlobalCrumbIssuerConfiguration.createDefaultCrumbIssuer());

                    jenkins.save();
                    bc.commit();
                }
            }

            // 8. 콘솔에 초기 비밀번호 출력
            if (iapf.exists()) {
                String setupKey = iapf.readToString().trim();
                LOGGER.info("Jenkins initial setup is required...\n" +
                    "Please use the following password:\n" + setupKey);
            }
        }

        // 9. UpdateCenter 메타데이터 갱신
        UpdateCenter.updateDefaultSite();
    }
}
```

#### 초기 비밀번호 파일

```
$JENKINS_HOME/secrets/initialAdminPassword
```

- 권한: `0640` (소유자 읽기/쓰기, 그룹 읽기)
- 내용: UUID 기반 32자 랜덤 문자열
- 위치: 콘솔 로그에도 출력됨

#### 초기 API 토큰

시스템 프로퍼티 `jenkins.install.SetupWizard.adminInitialApiToken`으로 제어:

| 값 | 동작 |
|----|------|
| `true` | 랜덤 토큰 생성 → `secrets/initialAdminApiToken`에 저장 |
| `110123...` | 지정된 값으로 고정 토큰 생성 |
| `@/path/to/file` | 파일에서 토큰 값 읽기 |
| 미설정 | API 토큰 생성 안 함 |

### 3.2 FORCE_SETUP_WIZARD_FILTER — 강제 리다이렉트

```java
private final Filter FORCE_SETUP_WIZARD_FILTER = new CompatibleFilter() {
    @Override
    public void doFilter(ServletRequest request, ServletResponse response,
                         FilterChain chain) throws IOException, ServletException {
        if (request instanceof HttpServletRequest req
            && !Jenkins.get().getInstallState().isSetupComplete()) {

            String requestURI = req.getRequestURI();

            if (req.getRequestURI().equals(req.getContextPath() + "/")) {
                // 루트 요청 → 위자드 페이지로 리다이렉트
                Jenkins.get().checkPermission(Jenkins.ADMINISTER);
                chain.doFilter(new HttpServletRequestWrapper(req) {
                    @Override
                    public String getRequestURI() {
                        return getContextPath() + "/setupWizard/";
                    }
                }, response);
                return;
            }
        }
        chain.doFilter(request, response);
    }
};
```

**핵심**: `isSetupComplete() == false`인 동안 모든 루트 URL 요청을
`/setupWizard/`로 내부 포워딩한다.

### 3.3 doCreateAdminUser — 관리자 계정 생성 API

```java
@POST
public HttpResponse doCreateAdminUser(StaplerRequest2 req, StaplerResponse2 rsp) {
    Jenkins j = Jenkins.get();
    j.checkPermission(Jenkins.ADMINISTER);

    HudsonPrivateSecurityRealm securityRealm =
        (HudsonPrivateSecurityRealm) j.getSecurityRealm();

    User admin = securityRealm.getUser("admin");

    // 기존 admin 삭제 후 새 사용자 생성
    if (admin != null) {
        initialApiTokenProperty = admin.getProperty(ApiTokenProperty.class);
        admin.delete();
    }

    User newUser = securityRealm.createAccountFromSetupWizard(req);

    // API 토큰 이전
    if (initialApiTokenProperty != null) {
        newUser.addProperty(initialApiTokenProperty);
    }

    // 초기 비밀번호 파일 삭제
    getInitialAdminPasswordFile().delete();

    // 다음 상태로 진행
    InstallUtil.proceedToNextStateFrom(InstallState.CREATE_ADMIN_USER);

    // 새 사용자로 로그인
    Authentication auth = new UsernamePasswordAuthenticationToken(
        newUser.getId(), req.getParameter("password1"));
    auth = securityRealm.getSecurityComponents().manager2.authenticate(auth);
    SecurityContextHolder.getContext().setAuthentication(auth);

    // 세션 고정 공격 방지
    HttpSession session = req.getSession(false);
    if (session != null) session.invalidate();
    HttpSession newSession = req.getSession(true);

    return HttpResponses.okJSON(data);
}
```

### 3.4 doConfigureInstance — 인스턴스 URL 설정

```java
@POST
public HttpResponse doConfigureInstance(StaplerRequest2 req,
                                        @QueryParameter String rootUrl) {
    Jenkins.get().checkPermission(Jenkins.ADMINISTER);

    Map<String, String> errors = new HashMap<>();
    checkRootUrl(errors, rootUrl);

    if (!errors.isEmpty()) {
        return HttpResponses.errorJSON("Validation errors", errors);
    }

    JenkinsLocationConfiguration.getOrDie().setUrl(rootUrl);
    InstallUtil.proceedToNextStateFrom(InstallState.CONFIGURE_INSTANCE);

    return HttpResponses.okJSON(data);
}
```

### 3.5 doPlatformPluginList — 추천 플러그인 목록

```java
public HttpResponse doPlatformPluginList() {
    if (InstallState.UPGRADE.equals(Jenkins.get().getInstallState())) {
        // 업그레이드: 새 버전에서 추가된 플러그인만
        JSONArray data = getPlatformPluginUpdates();
    } else {
        // 신규 설치: 전체 추천 플러그인 목록
        JSONArray data = getPlatformPluginList();
    }
}
```

#### 플러그인 목록 소스

1. **원격**: UpdateSite의 `suggestedPluginsUrl`에서 다운로드
   - 서명 검증 (signatureCheck)
   - 버전별 맞춤 목록 (`?version=2.xxx`)
2. **로컬 폴백**: `jenkins/install/platform-plugins.json` (클래스패스)

### 3.6 completeSetup — 설정 완료

```java
void completeSetup() throws IOException, ServletException {
    Jenkins.get().checkPermission(Jenkins.ADMINISTER);
    InstallUtil.saveLastExecVersion();
    setCurrentLevel(Jenkins.getVersion());
    InstallUtil.proceedToNextStateFrom(InstallState.INITIAL_SETUP_COMPLETED);
}
```

## 4. 업그레이드 상태 관리

### 4.1 상태 파일

```
$JENKINS_HOME/jenkins.install.UpgradeWizard.state
```

내용: 마지막으로 업그레이드된 Jenkins 버전 번호 (예: `2.462.1`)

### 4.2 UPGRADE 상태 초기화

```java
private static final class Upgrade extends InstallState {
    @Override
    public void initializeState() {
        applyForcedChanges();     // API 토큰 정책 업데이트
        reloadUpdateSiteData();    // 업데이트 사이트 갱신
        InstallUtil.saveLastExecVersion();
    }

    private void applyForcedChanges() {
        ApiTokenPropertyConfiguration config = ApiTokenPropertyConfiguration.get();
        if (!config.hasExistingConfigFile()) {
            // 레거시 토큰 비활성화
            config.setCreationOfLegacyTokenEnabled(false);
            config.setTokenGenerationOnCreationEnabled(false);
        }
    }
}
```

## 5. 보안 설계

### 5.1 초기 보안 체인

```
1. HudsonPrivateSecurityRealm 생성
2. admin 계정 + 랜덤 비밀번호 생성
3. FullControlOnceLoggedInAuthorizationStrategy 설정
4. 익명 읽기 비활성화
5. JNLP 에이전트 포트 비활성화
6. CSRF CrumbIssuer 활성화
```

### 5.2 비밀번호 파일 보안

```
$JENKINS_HOME/secrets/initialAdminPassword
  - 권한: 0640 (소유자 rw, 그룹 r)
  - 목적: 최초 로그인에만 사용
  - 수명: 관리자 계정 생성 시 삭제
```

### 5.3 세션 고정 공격 방지

```java
// doCreateAdminUser()
HttpSession session = req.getSession(false);
if (session != null) session.invalidate();   // 기존 세션 무효화
HttpSession newSession = req.getSession(true); // 새 세션 생성

UserSeedProperty userSeed = newUser.getProperty(UserSeedProperty.class);
newSession.setAttribute(UserSeedProperty.USER_SESSION_SEED, userSeed.getSeed());
```

## 6. Setup Wizard UI 흐름

```
┌─────────────────────────────────────────┐
│ Step 1: Unlock Jenkins                  │
│                                         │
│ Please copy the initial admin password  │
│ from:                                   │
│ /var/jenkins_home/secrets/              │
│   initialAdminPassword                  │
│                                         │
│ [___________________________]           │
│ [Continue]                              │
└─────────────────────────────────────────┘
            │
            ▼
┌─────────────────────────────────────────┐
│ Step 2: Customize Jenkins              │
│                                         │
│ [Install suggested plugins]             │
│ [Select plugins to install]             │
└─────────────────────────────────────────┘
            │
            ▼
┌─────────────────────────────────────────┐
│ Step 3: Installing Plugins              │
│                                         │
│ ✓ Pipeline                              │
│ ✓ Git                                   │
│ ◐ Blue Ocean (설치 중...)               │
│ ○ Docker Pipeline                       │
│ ...                                     │
└─────────────────────────────────────────┘
            │
            ▼
┌─────────────────────────────────────────┐
│ Step 4: Create First Admin User         │
│                                         │
│ Username: [__________]                  │
│ Password: [__________]                  │
│ Confirm:  [__________]                  │
│ Full name: [_________]                  │
│ Email: [_____________]                  │
│                                         │
│ [Save and Continue]                     │
└─────────────────────────────────────────┘
            │
            ▼
┌─────────────────────────────────────────┐
│ Step 5: Instance Configuration          │
│                                         │
│ Jenkins URL: [http://localhost:8080/]    │
│                                         │
│ [Save and Finish]                       │
└─────────────────────────────────────────┘
            │
            ▼
┌─────────────────────────────────────────┐
│ Jenkins is Ready!                       │
│                                         │
│ [Start using Jenkins]                   │
└─────────────────────────────────────────┘
```

## 7. 설정 스킵

### 7.1 시스템 프로퍼티로 스킵

| 프로퍼티 | 효과 |
|----------|------|
| `jenkins.install.runSetupWizard=false` | Setup Wizard 전체 스킵 |
| `hudson.Main.development=true` | 개발 모드 (DEVELOPMENT 상태) |

### 7.2 프로그래밍 방식 스킵

```groovy
// init.groovy.d/skip-wizard.groovy
import jenkins.model.*
import hudson.security.*

def instance = Jenkins.getInstance()
instance.setInstallState(InstallState.INITIAL_SETUP_COMPLETED)
```

## 8. InstallUtil — 상태 전이 유틸리티

**경로**: `core/src/main/java/jenkins/install/InstallUtil.java`

```java
public class InstallUtil {
    // 다음 상태로 진행
    public static void proceedToNextStateFrom(InstallState current) {
        // 현재 상태에서 다음 상태를 결정하고 전이
    }

    // 현재 버전을 상태 파일에 저장
    public static void saveLastExecVersion() {
        // jenkins.install.InstallUtil.lastExecVersion 파일에 기록
    }
}
```

## 9. 확장 포인트

| 확장 포인트 | 역할 | 등록 |
|-------------|------|------|
| `InstallState` | 새로운 설치 상태 추가 | `@Extension` |
| `InstallStateFilter` | 상태 전이 필터링 | `@Extension` |
| `PageDecorator` | Setup Wizard UI 수정 | `@Extension` |

## 10. 정리

Jenkins Setup Wizard의 핵심 설계 원칙:

1. **보안 우선(Security by Default)**: 최초 설치 시 랜덤 비밀번호 + 인증 강제
2. **상태 기계**: `InstallState`로 설치 과정을 명확한 단계로 모델링
3. **점진적 안내**: 단계별 UI로 초보자도 필수 설정을 빠뜨리지 않음
4. **스킵 가능**: 자동화 환경에서는 시스템 프로퍼티로 건너뛸 수 있음
5. **업그레이드 인식**: 신규 설치와 업그레이드를 구분하여 적절한 안내 제공
6. **서블릿 필터**: 설정 미완료 시 모든 요청을 위자드로 강제 리다이렉트
