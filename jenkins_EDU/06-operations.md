# Jenkins 운영 가이드

## 목차

1. [배포 방식](#1-배포-방식)
2. [JENKINS_HOME 구조](#2-jenkins_home-구조)
3. [설정 관리](#3-설정-관리)
4. [보안 설정](#4-보안-설정)
5. [플러그인 관리](#5-플러그인-관리)
6. [모니터링](#6-모니터링)
7. [트러블슈팅](#7-트러블슈팅)
8. [백업/복구](#8-백업복구)
9. [성능 튜닝](#9-성능-튜닝)

---

## 1. 배포 방식

Jenkins는 다양한 방식으로 배포할 수 있다. WAR 파일 독립 실행, Docker 컨테이너, Kubernetes Helm chart, 전통적 서블릿 컨테이너 배포 등 환경에 맞는 방식을 선택한다.

### 1.1 WAR 파일 독립 실행 (java -jar jenkins.war)

가장 기본적인 배포 방식이다. Jenkins WAR 파일에는 Winstone(Jetty 기반) 서블릿 컨테이너가 내장되어 있어 별도의 웹 서버 없이 독립 실행할 수 있다.

```bash
java -jar jenkins.war
```

#### 시작 흐름 (executable.Main.main())

`executable.Main` 클래스가 진입점이다. 소스코드 경로: `war/src/main/java/executable/Main.java`

```
executable.Main.main()
  ├── 1. Java 버전 검증 (verifyJavaVersion)
  │     └── SUPPORTED_JAVA_VERSIONS = [21, 25]
  │         최소 Java 21 필요, --enable-future-java 플래그로 미지원 버전 허용 가능
  ├── 2. --paramsFromStdIn 처리 (민감 파라미터를 stdin으로 전달)
  ├── 3. --version 처리 (MANIFEST.MF에서 Jenkins-Version 읽기)
  ├── 4. 옵션 처리
  │     ├── --extractedFilesFolder: WAR 추출 디렉토리
  │     ├── --pluginroot: 플러그인 작업 디렉토리 (hudson.PluginManager.workDir)
  │     └── --webroot: WAR 전개 디렉토리 (기본값: $JENKINS_HOME/war)
  ├── 5. java.awt.headless=true 설정 (데몬 실행 시 JFreeChart 호환)
  ├── 6. Winstone JAR 추출 및 ClassLoader 구성
  │     └── URLClassLoader("Jenkins Main ClassLoader", [winstone.jar])
  ├── 7. 세션 쿠키 이름 설정
  │     └── JSESSIONID.{랜덤8자} (동일 호스트 다중 인스턴스 충돌 방지)
  └── 8. winstone.Launcher.main() 위임
```

이 클래스는 의도적으로 매우 얇은 래퍼로 설계되었다. 소스코드 주석에 명시된 설계 원칙:

> "This class is intended to be a very thin wrapper whose primary purpose is to extract Winstone and delegate to Winstone's own initialization mechanism."

Winstone 시작 후, `WebAppMain.contextInitialized()`가 호출되어 실제 Jenkins 초기화가 진행된다.

#### WebAppMain 초기화 흐름

소스코드 경로: `core/src/main/java/hudson/WebAppMain.java`

```
WebAppMain.contextInitialized()
  ├── 1. JenkinsJVM 마킹 (_setJenkinsJVM(true))
  ├── 2. Locale 프로바이더 설정
  ├── 3. 권한/환경 검증
  │     ├── SecurityException 체크 (JENKINS-719)
  │     ├── SunPKCS11 프로바이더 제거 (Solaris 호환)
  │     └── Temp 디렉토리 존재 확인
  ├── 4. RingBufferLogHandler 설치 (순환 로그 버퍼)
  ├── 5. JENKINS_HOME 결정 (getHomeDir)
  │     ├── SystemProperties: JENKINS_HOME, HUDSON_HOME
  │     ├── 환경변수: JENKINS_HOME, HUDSON_HOME
  │     ├── WEB-INF/workspace (레거시)
  │     ├── ~/.hudson (레거시)
  │     └── ~/.jenkins (기본값)
  ├── 6. 부팅 시도 기록 (recordBootAttempt → failed-boot-attempts.txt)
  ├── 7. 호환성 검증
  │     ├── XStream ReflectionProvider 확인
  │     ├── Servlet 2.4+ 확인
  │     ├── Ant 1.7+ 확인
  │     └── AWT 기능 확인 (JFreeChart 의존)
  ├── 8. context.setAttribute("app", new HudsonIsLoading())
  ├── 9. 세션 트래킹 모드: COOKIE only
  └── 10. 초기화 스레드 시작
        └── new Hudson(_home, context) → Jenkins 인스턴스 생성
            성공 시: context.setAttribute("app", instance)
                     failed-boot-attempts.txt 삭제
                     lifecycle.onReady()
            실패 시: HudsonFailedToLoad 게시
```

#### 주요 CLI 옵션

| 옵션 | 설명 | 기본값 |
|------|------|--------|
| `--httpPort` | HTTP 포트 | 8080 |
| `--httpsPort` | HTTPS 포트 | - |
| `--webroot` | WAR 전개 디렉토리 | `$JENKINS_HOME/war` |
| `--pluginroot` | 플러그인 디렉토리 | `$JENKINS_HOME/plugins` |
| `--prefix` | URL 접두사 | `/` |
| `--enable-future-java` | 미지원 Java 버전 허용 | false |
| `--paramsFromStdIn` | stdin에서 파라미터 읽기 | false |
| `--extractedFilesFolder` | 추출 파일 폴더 | 시스템 temp |

### 1.2 Docker 배포

공식 Docker 이미지를 사용한 배포 방식이다.

```bash
# LTS 버전 실행
docker run -d \
  -p 8080:8080 \
  -p 50000:50000 \
  -v jenkins_home:/var/jenkins_home \
  jenkins/jenkins:lts

# 특정 버전 지정
docker run -d jenkins/jenkins:2.440.3-lts
```

#### Docker 배포 시 고려사항

```
Docker 배포 아키텍처
┌─────────────────────────────────────────┐
│  Docker Host                            │
│  ┌───────────────────────────────────┐  │
│  │  jenkins/jenkins:lts 컨테이너     │  │
│  │                                   │  │
│  │  ┌─────────────┐  ┌───────────┐  │  │
│  │  │ Winstone/   │  │ Jenkins   │  │  │
│  │  │ Jetty       │  │ Core      │  │  │
│  │  │ :8080       │  │           │  │  │
│  │  └──────┬──────┘  └─────┬─────┘  │  │
│  │         │               │         │  │
│  │         └───────┬───────┘         │  │
│  │                 │                 │  │
│  │  ┌──────────────▼──────────────┐  │  │
│  │  │ /var/jenkins_home (Volume)  │  │  │
│  │  │  config.xml, jobs/, plugins/│  │  │
│  │  └─────────────────────────────┘  │  │
│  │                                   │  │
│  │  Port 50000: 에이전트 연결 포트   │  │
│  └───────────────────────────────────┘  │
└─────────────────────────────────────────┘
```

- **볼륨 마운트 필수**: `/var/jenkins_home`을 호스트 볼륨에 마운트하여 데이터 영속성 확보
- **포트 50000**: `TcpSlaveAgentListener`가 에이전트 연결을 수신하는 포트
- **UID/GID**: 컨테이너 내 `jenkins` 사용자(UID 1000)로 실행됨. 볼륨 권한 주의

### 1.3 Kubernetes Helm Chart 배포

Helm chart를 사용한 Kubernetes 배포이다.

```bash
# Helm 레포지토리 추가
helm repo add jenkins https://charts.jenkins.io
helm repo update

# 기본 설치
helm install jenkins jenkins/jenkins \
  --namespace jenkins \
  --create-namespace

# 커스텀 values.yaml로 설치
helm install jenkins jenkins/jenkins \
  -f values.yaml \
  --namespace jenkins
```

#### Kubernetes 배포 주요 values.yaml 설정

```yaml
controller:
  image: jenkins/jenkins
  tag: lts
  resources:
    requests:
      cpu: "500m"
      memory: "512Mi"
    limits:
      cpu: "2000m"
      memory: "4096Mi"
  javaOpts: "-Xmx2g -Xms512m"
  jenkinsUrl: "https://jenkins.example.com"
  installPlugins:
    - kubernetes:latest
    - workflow-aggregator:latest
    - configuration-as-code:latest

persistence:
  enabled: true
  size: "50Gi"
  storageClass: "standard"

agent:
  enabled: true
  image: jenkins/inbound-agent
```

### 1.4 전통적 서블릿 컨테이너 배포 (Tomcat 등)

Tomcat, Jetty 등 외부 서블릿 컨테이너에 WAR 파일을 배포하는 방식이다.

```bash
# Tomcat 예시
cp jenkins.war /opt/tomcat/webapps/

# 또는 ROOT.war로 배포 (루트 컨텍스트)
cp jenkins.war /opt/tomcat/webapps/ROOT.war
```

> **주의**: `WebAppMain`의 `FORCE_SESSION_TRACKING_BY_COOKIE_PROP` 시스템 속성 확인 필요. Tomcat의 기본 세션 트래킹은 COOKIE+URL인데, URL 트래킹이 활성화되면 세션 하이재킹에 취약할 수 있다. 소스코드 주석에서 이를 명시적으로 경고하고 있다.

---

## 2. JENKINS_HOME 구조

`JENKINS_HOME`은 Jenkins의 모든 상태를 보관하는 핵심 디렉토리이다. 파일시스템 기반의 영속화 전략으로, 별도의 데이터베이스 없이 XML 직렬화를 통해 모든 설정과 데이터를 관리한다.

### 2.1 전체 디렉토리 구조

```
JENKINS_HOME/
├── config.xml                    # [핵심] 시스템 설정 (XStream 직렬화)
├── credentials.xml               # 암호화된 자격증명
├── queue.xml                     # 큐 상태 영속화
├── nodeMonitors.xml              # 노드 모니터링 설정
├── hudson.model.UpdateCenter.xml # 업데이트 센터 설정
├── secret.key                    # 마스터 암호화 키
├── secret.key.not-so-secret      # 레거시 비밀 키
├── identity.key.enc              # 인스턴스 식별 키 (암호화)
│
├── failed-boot-attempts.txt      # 부팅 실패 추적 파일
│                                   (성공 시 삭제됨)
│
├── jobs/                         # Job 정의 및 빌드 이력
│   └── {job-name}/
│       ├── config.xml            # Job 설정
│       ├── nextBuildNumber        # 다음 빌드 번호 (텍스트)
│       └── builds/
│           └── {build-number}/
│               ├── build.xml     # 빌드 메타데이터
│               ├── log           # 빌드 콘솔 로그
│               ├── changelog.xml # 변경 이력
│               └── archive/      # 아카이브된 아티팩트
│
├── nodes/                        # 에이전트 노드 설정
│   └── {node-name}/
│       └── config.xml            # 노드 설정
│
├── users/                        # 사용자 데이터
│   └── {user-id}/
│       └── config.xml            # 사용자 설정 (이메일, API 토큰 등)
│
├── plugins/                      # 플러그인
│   ├── {plugin-name}.jpi         # 플러그인 아카이브
│   ├── {plugin-name}/            # 전개된 플러그인
│   │   ├── META-INF/MANIFEST.MF  # 플러그인 메타데이터
│   │   └── WEB-INF/
│   │       ├── classes/
│   │       └── lib/
│   └── {plugin-name}.jpi.disabled # 비활성화 마커 파일
│
├── war/                          # WAR 전개 디렉토리
│   └── (Winstone이 전개한 웹앱 파일)
│
├── logs/                         # 시스템 로그
│   └── tasks/                    # PeriodicWork 로그
│
├── fingerprints/                 # 아티팩트 추적 해시
│   └── {xx}/{yy}/{hash}.xml
│
├── userContent/                  # 사용자 정적 콘텐츠
│                                   (/userContent/... URL로 접근)
│
├── secrets/                      # 암호화 관련
│   ├── master.key               # 마스터 암호화 키
│   └── hudson.util.Secret        # Secret 암호화용 키
│
├── init.groovy.d/               # 초기화 Groovy 스크립트
│                                   (GroovyHookScript로 실행)
│
└── updates/                     # 업데이트 센터 캐시
    └── default.json              # 플러그인 메타데이터 캐시
```

### 2.2 config.xml — 시스템 설정

`config.xml`은 Jenkins 인스턴스의 전체 시스템 설정을 담는 파일이다. `XmlFile` 클래스(`core/src/main/java/hudson/XmlFile.java`)를 통해 XStream으로 직렬화/역직렬화된다.

```xml
<?xml version='1.1' encoding='UTF-8'?>
<hudson>
  <numExecutors>2</numExecutors>
  <mode>NORMAL</mode>
  <useSecurity>true</useSecurity>
  <authorizationStrategy class="...MatrixAuthorizationStrategy">
    ...
  </authorizationStrategy>
  <securityRealm class="...HudsonPrivateSecurityRealm">
    ...
  </securityRealm>
  <quietPeriod>5</quietPeriod>
  <scmCheckoutRetryCount>0</scmCheckoutRetryCount>
  <views>
    <hudson.model.AllView>
      <name>all</name>
    </hudson.model.AllView>
  </views>
  <primaryView>all</primaryView>
  <nodeProperties/>
  <globalNodeProperties/>
</hudson>
```

Jenkins 인스턴스의 `save()` 메서드가 호출될 때마다 이 파일이 갱신된다. `Jenkins.java`에서:

```java
// Jenkins 클래스 필드 (jenkins/model/Jenkins.java)
private int numExecutors = 2;       // 내장 노드 Executor 수
/*package*/ Integer quietPeriod;     // 빌드 대기 시간 (기본 5초)

// 설정 변경 시 자동 저장
public void setQuietPeriod(Integer quietPeriod) throws IOException {
    this.quietPeriod = quietPeriod;
    save();  // → config.xml 갱신
}
```

> **version 필드**: Jenkins는 `config.xml`을 저장할 때마다 현재 버전을 기록한다. 이를 통해 업그레이드 시 마이그레이션이 필요한지 감지한다.

### 2.3 queue.xml — 큐 상태 영속화

빌드 큐의 상태를 `queue.xml`에 영속화한다. Jenkins 재시작 시에도 대기 중인 빌드를 복원하기 위함이다.

소스코드 경로: `core/src/main/java/hudson/model/Queue.java`

```java
// Queue 클래스의 큐 파일 결정 로직
/*package*/ File getXMLQueueFile() {
    String id = SystemProperties.getString(Queue.class.getName() + ".id");
    if (id != null) {
        return new File(Jenkins.get().getRootDir(), "queue/" + id + ".xml");
    }
    return new File(Jenkins.get().getRootDir(), "queue.xml");
}
```

### 2.4 failed-boot-attempts.txt — 부팅 실패 추적

`BootFailure` 클래스(`core/src/main/java/hudson/util/BootFailure.java`)가 관리하는 파일이다.

```java
// BootFailure.java
public static File getBootFailureFile(File home) {
    return new File(home, "failed-boot-attempts.txt");
}
```

동작 원리:
1. 부팅 시도마다 `WebAppMain.recordBootAttempt()`가 타임스탬프를 파일에 추가
2. 부팅 성공 시 파일 삭제: `Files.deleteIfExists(BootFailure.getBootFailureFile(_home).toPath())`
3. 부팅 실패 시 `boot-failure.groovy` 훅 스크립트 실행 (GroovyHookScript)

---

## 3. 설정 관리

### 3.1 시스템 설정: config.xml과 XmlFile

Jenkins의 모든 설정은 XML 파일로 직렬화된다. 핵심 클래스는 `XmlFile`이다.

소스코드 경로: `core/src/main/java/hudson/XmlFile.java`

```java
// XmlFile.java — XML 기반 영속화의 핵심
public final class XmlFile {
    private final XStream xs;       // XStream 직렬화 엔진
    private final File file;        // 대상 파일
    private final boolean force;    // 강제 쓰기 여부
}
```

XStream을 사용한 직렬화 장점:
- **스키마리스**: 별도 스키마 정의 없이 Java 객체를 직접 XML로 변환
- **하위 호환성**: 필드 추가/삭제 시에도 기존 파일 로드 가능
- **가독성**: 사람이 읽고 편집할 수 있는 XML 형식

### 3.2 환경변수

Jenkins 홈 디렉토리 결정에 사용되는 환경변수이다. `executable.Main`과 `WebAppMain` 모두 동일한 우선순위를 따른다.

```
우선순위 (높은 순):
1. 시스템 프로퍼티 JENKINS_HOME  (-DJENKINS_HOME=...)
2. 시스템 프로퍼티 HUDSON_HOME   (-DHUDSON_HOME=...)
3. 환경변수 JENKINS_HOME
4. 환경변수 HUDSON_HOME
5. $user.home/.hudson (레거시, 존재하는 경우)
6. $user.home/.jenkins (기본값)
```

소스코드에서 확인한 HOME_NAMES 배열:

```java
// executable/Main.java
private static final String[] HOME_NAMES = {"JENKINS_HOME", "HUDSON_HOME"};

// WebAppMain.java — 동일한 우선순위
private static final String[] HOME_NAMES = {"JENKINS_HOME", "HUDSON_HOME"};
```

`WebAppMain.getHomeDir()`에는 추가로 서블릿 컨텍스트의 `WEB-INF/workspace` 경로도 확인한다 (레거시 지원).

### 3.3 SystemProperties — 세부 동작 제어

`jenkins.util.SystemProperties`(`core/src/main/java/jenkins/util/SystemProperties.java`)는 Jenkins 전용 시스템 속성 관리 클래스이다. Java의 `System.getProperty()` 외에 서블릿 컨텍스트의 init parameter도 함께 조회한다.

```
SystemProperties 조회 순서:
1. Java 시스템 프로퍼티 (-D 플래그)
2. 서블릿 컨텍스트 init parameter (web.xml)
3. 환경변수 (일부 프로퍼티)
```

주요 시스템 프로퍼티:

| 프로퍼티 | 설명 | 기본값 |
|---------|------|--------|
| `JENKINS_HOME` | 홈 디렉토리 | `~/.jenkins` |
| `hudson.lifecycle` | Lifecycle 구현 클래스 FQCN | 자동 감지 |
| `hudson.model.PeriodicWork.recurrencePeriod` | 주기적 작업 간격 조정 | - |
| `hudson.PluginManager.workDir` | 플러그인 작업 디렉토리 | `$JENKINS_HOME/plugins` |
| `executable-war` | WAR 파일 위치 (자동 설정) | - |
| `hudson.util.RingBufferLogHandler.defaultSize` | 로그 버퍼 크기 | 256 |
| `executableWar.jetty.sessionIdCookieName` | 세션 쿠키 이름 | `JSESSIONID.{랜덤}` |
| `executableWar.jetty.disableCustomSessionIdCookieName` | 커스텀 쿠키 이름 비활성화 | false |

### 3.4 Jenkins Configuration as Code (CasC)

CasC 플러그인을 통해 YAML 기반의 선언적 설정을 지원한다. Jenkins의 `InitMilestone.SYSTEM_CONFIG_ADAPTED` 단계에서 적용된다.

```
Jenkins 초기화 마일스톤 (InitMilestone.java):
  STARTED
  → PLUGINS_LISTED       : 플러그인 메타데이터 로드 완료
  → PLUGINS_PREPARED     : 플러그인 클래스로더 설정 완료
  → PLUGINS_STARTED      : 플러그인 실행 시작
  → EXTENSIONS_AUGMENTED : 확장점 등록 완료
  → SYSTEM_CONFIG_LOADED : 시스템 설정(config.xml) 로드 완료
  → SYSTEM_CONFIG_ADAPTED: CasC 등이 설정 적용 완료 ← CasC 적용 시점
  → JOB_LOADED           : Job 로드 완료
  → JOB_CONFIG_ADAPTED   : Job 설정 업데이트 완료
  → COMPLETED            : 초기화 완료
```

CasC YAML 예시:

```yaml
jenkins:
  systemMessage: "Jenkins Configuration as Code로 관리됨"
  numExecutors: 4
  mode: NORMAL
  quietPeriod: 5

  securityRealm:
    local:
      allowsSignup: false
      users:
        - id: "admin"
          password: "${JENKINS_ADMIN_PASSWORD}"

  authorizationStrategy:
    loggedInUsersCanDoAnything:
      allowAnonymousRead: false

  nodes:
    - permanent:
        name: "build-agent-1"
        remoteFS: "/home/jenkins"
        launcher:
          ssh:
            host: "agent1.example.com"
            credentialsId: "ssh-key"

unclassified:
  location:
    url: "https://jenkins.example.com/"
    adminAddress: "admin@example.com"
```

### 3.5 Groovy 초기화 스크립트

`GroovyHookScript`(`core/src/main/java/jenkins/util/groovy/GroovyHookScript.java`)를 통해 다양한 훅 포인트에서 Groovy 스크립트를 실행할 수 있다.

```
Groovy 훅 스크립트 검색 경로:
1. /WEB-INF/{hook}.groovy       (OEM 배포용)
2. /WEB-INF/{hook}.groovy.d/    (OEM 배포용, 디렉토리)
3. $JENKINS_HOME/{hook}.groovy  (설치 로컬)
4. $JENKINS_HOME/{hook}.groovy.d/*.groovy  (설치 로컬, 디렉토리)
```

주요 훅 포인트:
- `init.groovy.d/`: 초기화 시 실행
- `boot-failure.groovy.d/`: 부팅 실패 시 실행 (BootFailure에서 호출)

---

## 4. 보안 설정

Jenkins의 보안 아키텍처는 **인증(Authentication)**과 **인가(Authorization)**를 분리한 플러그인 확장 가능한 구조이다.

### 4.1 SecurityRealm — 인증 체계

`SecurityRealm`(`core/src/main/java/hudson/security/SecurityRealm.java`)은 사용자 인증을 담당하는 추상 클래스이다.

```
SecurityRealm (인증)
├── HudsonPrivateSecurityRealm  — 내장 사용자 DB
├── LDAPSecurityRealm           — LDAP/Active Directory (플러그인)
├── PAMSecurityRealm            — Unix PAM (플러그인)
├── SecurityRealm.None          — 인증 없음
└── (OIDC, SAML 등 플러그인)
```

#### HudsonPrivateSecurityRealm — 내장 사용자 데이터베이스

소스코드 경로: `core/src/main/java/hudson/security/HudsonPrivateSecurityRealm.java`

```java
// HudsonPrivateSecurityRealm.java
public class HudsonPrivateSecurityRealm
    extends AbstractPasswordBasedSecurityRealm
    implements ModelObject, AccessControlled {

    private static final int FIPS_PASSWORD_LENGTH = 14;
    // FIPS 모드에서 최소 비밀번호 길이 14자 요구
}
```

사용자 데이터는 `$JENKINS_HOME/users/{user-id}/config.xml`에 저장된다. 비밀번호는 PBKDF2-HMAC-SHA512로 해시된다.

#### SecurityRealm의 핵심 메서드

```java
public abstract class SecurityRealm implements Describable<SecurityRealm>, ExtensionPoint {
    // 인증 매니저 생성 — 실제 인증 로직을 담당
    public abstract SecurityComponents createSecurityComponents();

    // 사용자 ID 전략 (대소문자 처리)
    public IdStrategy getUserIdStrategy() {
        return IdStrategy.CASE_INSENSITIVE;  // 기본: 대소문자 무시
    }
}
```

### 4.2 AuthorizationStrategy — 인가 전략

`AuthorizationStrategy`(`core/src/main/java/hudson/security/AuthorizationStrategy.java`)는 인증된 사용자에게 어떤 권한을 부여할지 결정한다.

```
AuthorizationStrategy (인가)
├── AuthorizationStrategy.Unsecured     — 모든 사용자 전체 권한
├── FullControlOnceLoggedInAuthorizationStrategy — 로그인 사용자 전체 권한
├── GlobalMatrixAuthorizationStrategy   — 글로벌 매트릭스 (플러그인)
├── ProjectMatrixAuthorizationStrategy  — 프로젝트별 매트릭스 (플러그인)
└── RoleBasedAuthorizationStrategy      — 역할 기반 (플러그인)
```

```java
// AuthorizationStrategy.java — UNSECURED 싱글톤
public static final AuthorizationStrategy UNSECURED = new Unsecured();

public static final class Unsecured extends AuthorizationStrategy {
    @Override
    public @NonNull ACL getRootACL() {
        return UNSECURED_ACL;  // 모든 권한 허용
    }
    private static final ACL UNSECURED_ACL = ACL.lambda2((a, p) -> true);
}
```

### 4.3 CSRF 보호 — CrumbIssuer

`CrumbIssuer`(`core/src/main/java/hudson/security/csrf/CrumbIssuer.java`)는 CSRF(Cross-Site Request Forgery) 공격을 방어하기 위한 crumb(토큰)을 발급한다.

```java
// CrumbIssuer.java
public abstract class CrumbIssuer implements Describable<CrumbIssuer>, ExtensionPoint {
    public static final String DEFAULT_CRUMB_NAME = "Jenkins-Crumb";
}
```

동작 원리:
1. 클라이언트가 crumb 요청: `GET /crumbIssuer/api/json`
2. 서버가 crumb 발급 (세션/사용자 기반)
3. 클라이언트가 POST 요청 시 `Jenkins-Crumb` 헤더에 crumb 포함
4. 서버가 crumb 유효성 검증

```bash
# crumb 획득 및 API 호출 예시
CRUMB=$(curl -s 'http://jenkins/crumbIssuer/api/xml?xpath=concat(//crumbRequestField,":",//crumb)' \
  -u admin:password)

curl -X POST 'http://jenkins/job/my-job/build' \
  -u admin:password \
  -H "$CRUMB"
```

### 4.4 에이전트 프로토콜 — TcpSlaveAgentListener

`TcpSlaveAgentListener`(`core/src/main/java/hudson/TcpSlaveAgentListener.java`)는 에이전트(슬레이브)가 컨트롤러에 연결하기 위한 TCP 리스너이다.

```java
// TcpSlaveAgentListener.java
public final class TcpSlaveAgentListener extends Thread {
    private ServerSocketChannel serverSocket;
    private volatile boolean shuttingDown;
    // 기본 포트: 50000 (Docker) 또는 랜덤
}
```

에이전트 연결 방식:

```
에이전트 연결 방식
┌────────────┐                    ┌────────────────┐
│  Agent     │───── TCP/JNLP ───→│  Controller    │
│  (Inbound) │     Port 50000    │  TcpSlave-     │
│            │                    │  AgentListener │
└────────────┘                    └────────────────┘

┌────────────┐                    ┌────────────────┐
│  Agent     │←──── SSH ─────────│  Controller    │
│  (SSH)     │     Port 22       │  SSHLauncher   │
│            │                    │  (플러그인)     │
└────────────┘                    └────────────────┘
```

### 4.5 보안 모범 사례

```
보안 체크리스트
┌─────────────────────────────────────────────────┐
│ 1. SecurityRealm 설정                            │
│    □ HudsonPrivateSecurityRealm 또는 LDAP 사용   │
│    □ 셀프 가입(allowsSignup) 비활성화            │
│    □ FIPS 모드 시 14자 이상 비밀번호             │
│                                                   │
│ 2. AuthorizationStrategy 설정                    │
│    □ GlobalMatrix 또는 ProjectMatrix 사용         │
│    □ 익명 사용자 권한 최소화                      │
│    □ 관리자 권한 최소 인원에게만 부여             │
│                                                   │
│ 3. CSRF 보호                                     │
│    □ CrumbIssuer 활성화 (기본 활성)              │
│    □ API 호출 시 crumb 또는 API 토큰 사용         │
│                                                   │
│ 4. 에이전트 보안                                  │
│    □ 인바운드 에이전트 시크릿 관리                │
│    □ SSH 키 기반 인증 사용                        │
│    □ 에이전트 → 컨트롤러 접근 제한               │
│                                                   │
│ 5. 스크립트 콘솔 접근 제한                        │
│    □ ADMINISTER 권한만 /script 접근 가능          │
│    □ 네트워크 레벨에서도 접근 제한                │
└─────────────────────────────────────────────────┘
```

---

## 5. 플러그인 관리

Jenkins의 기능 확장은 플러그인 시스템에 의존한다. `PluginManager`(`core/src/main/java/hudson/PluginManager.java`)가 플러그인의 설치, 로드, 의존성 해석을 관리한다.

### 5.1 플러그인 설치

```
플러그인 설치 경로
┌──────────────────────────────────────────────────────┐
│ 1. UI: Manage Jenkins → Plugin Manager               │
│    └── Available 탭에서 검색/설치                     │
│                                                       │
│ 2. CLI: jenkins-cli.jar install-plugin                │
│    $ java -jar jenkins-cli.jar -s URL install-plugin  │
│      {plugin-name}                                    │
│                                                       │
│ 3. API: REST API                                     │
│    POST /pluginManager/installNecessaryPlugins         │
│                                                       │
│ 4. 사전 설치: $JENKINS_HOME/plugins/에 .jpi 복사     │
│    (Docker 이미지 빌드 시 jenkins-plugin-cli 사용)    │
└──────────────────────────────────────────────────────┘
```

### 5.2 플러그인 로드 과정

Jenkins 초기화 시 플러그인 로드는 `InitMilestone`에 따라 단계적으로 진행된다.

```
플러그인 로드 흐름 (InitMilestone 기반)
STARTED
  │
  ▼
PLUGINS_LISTED
  │  PluginManager가 plugins/ 디렉토리 스캔
  │  각 .jpi/.hpi 파일의 MANIFEST.MF 읽기
  │  의존성 그래프 구축
  ▼
PLUGINS_PREPARED
  │  의존성 순서에 따라 ClassLoader 설정
  │  CyclicGraphDetector로 순환 의존성 검사
  ▼
PLUGINS_STARTED
  │  각 플러그인의 Plugin.start() 호출
  │  @Extension 어노테이션 스캔
  │  Descriptor 인스턴스화 및 로드
  ▼
EXTENSIONS_AUGMENTED
  │  프로그래밍 방식으로 추가된 확장점 등록
  ▼
(이후 시스템 설정 로드, Job 로드 등)
```

### 5.3 의존성 해석

`PluginManager`는 플러그인 간 의존성을 자동으로 해석한다.

```java
// PluginWrapper.Dependency — 의존성 정보
// MANIFEST.MF의 Plugin-Dependencies 헤더에서 파싱
// 형식: "plugin-name:version[;resolution:=optional]"
```

의존성 해석 규칙:
1. 필수 의존성(mandatory): 해당 플러그인이 없으면 설치 실패
2. 선택적 의존성(optional): `resolution:=optional` 표시, 없어도 동작
3. 순환 의존성 검출: `CyclicGraphDetector`가 DAG 검증

### 5.4 동적 로딩 vs 재시작 필요

```java
// PluginManager.dynamicLoad() — 재시작 없이 플러그인 로드
public void dynamicLoad(File arc)
    throws IOException, InterruptedException, RestartRequiredException {
    dynamicLoad(arc, false, null);
}
```

동적 로딩이 가능한 경우:
- 새로운 플러그인 설치 (기존 플러그인에 영향 없음)
- `Lifecycle.supportsDynamicLoad()`가 true를 반환

동적 로딩이 불가능한 경우 (재시작 필요):
- 기존 플러그인 업데이트
- 클래스로더에 이미 로드된 클래스 변경
- `RestartRequiredException` 발생

```
동적 로딩 판단 흐름
┌────────────────────┐
│ 플러그인 설치 요청  │
└────────┬───────────┘
         │
    ┌────▼────┐
    │ 신규?   │──── Yes ───→ dynamicLoad() 시도
    └────┬────┘              │
         │                   ├── 성공 → 즉시 활성화
         No                  └── RestartRequiredException → 재시작 필요
         │
    ┌────▼─────────┐
    │ 업데이트/변경 │──→ 재시작 필요 (RestartRequiredException)
    └──────────────┘
```

### 5.5 업데이트 센터 (UpdateCenter)

소스코드 경로: `core/src/main/java/hudson/model/UpdateCenter.java`

```java
// UpdateCenter.java
public class UpdateCenter extends AbstractModelObject
    implements Loadable, Saveable, OnMaster, StaplerProxy {
    // 기본 업데이트 센터 URL
    // https://updates.jenkins.io/
}
```

`UpdateCenter`는 다음을 관리한다:
- 사용 가능한 플러그인 목록 (`update-center.json`)
- 플러그인 다운로드 및 설치 작업 큐 (`UpdateCenterJob`)
- 업데이트 사이트 설정 (`UpdateSite`)
- 프록시 설정을 통한 다운로드 (`ProxyConfiguration`)

### 5.6 플러그인 비활성화/제거

```
플러그인 상태 관리
$JENKINS_HOME/plugins/
├── git.jpi                  # 활성 플러그인 아카이브
├── git/                     # 전개된 플러그인 디렉토리
├── git.jpi.disabled         # 이 파일 존재 시 비활성화
├── git.jpi.pinned           # 이 파일 존재 시 업데이트 고정
└── git.bak                  # 이전 버전 백업 (업데이트 시)
```

비활성화: `{plugin}.jpi.disabled` 파일 생성 (빈 파일)
제거: `{plugin}.jpi` 및 전개 디렉토리 삭제 → 재시작 필요

---

## 6. 모니터링

### 6.1 관리 대시보드 (/manage)

Jenkins의 `/manage` URL은 시스템 관리 대시보드를 제공한다. `ManagementLink` 확장점을 통해 관리 메뉴를 추가할 수 있다.

주요 관리 메뉴:

| URL 경로 | 기능 | 클래스 |
|----------|------|--------|
| `/manage` | 관리 대시보드 | `ManageJenkinsAction` |
| `/manage/configure` | 시스템 설정 | `Jenkins` |
| `/manage/configureSecurity` | 보안 설정 | `Jenkins` |
| `/manage/pluginManager` | 플러그인 관리 | `PluginManager` |
| `/manage/computer` | 노드 관리 | `ComputerSet` |
| `/manage/log` | 로그 관리 | `LogRecorderManager` |
| `/manage/credentials` | 자격증명 관리 | (플러그인) |

### 6.2 시스템 정보 (/systemInfo)

시스템 속성, 환경변수, 플러그인 목록 등의 전체 시스템 정보를 표시한다.

```
/systemInfo 표시 내용
├── System Properties    : Java 시스템 프로퍼티
├── Environment Variables: OS 환경변수
├── Plugins              : 설치된 플러그인 및 버전
├── Memory Usage         : JVM 메모리 사용량
└── Thread Information   : 스레드 상태
```

### 6.3 스레드 덤프 (/threadDump)

JVM의 모든 스레드 상태를 덤프한다. 성능 이슈 분석이나 데드락 감지에 유용하다.

### 6.4 AdministrativeMonitor — 관리 경고

`AdministrativeMonitor`(`core/src/main/java/hudson/model/AdministrativeMonitor.java`)는 시스템 상태 이상을 UI에 경고로 표시하는 확장점이다.

```java
// AdministrativeMonitor.java
public abstract class AdministrativeMonitor
    extends AbstractModelObject
    implements ExtensionPoint, StaplerProxy {
    // 모니터 활성화 여부
    public abstract boolean isActivated();
}
```

내장 모니터 예시:
- **JavaVersionRecommendationAdminMonitor**: Java 버전 호환성 경고
- **PluginManager.PluginUpdateMonitor**: 플러그인 업데이트 알림
- **OldDataMonitor**: 레거시 데이터 형식 감지
- **ReverseProxySetupMonitor**: 리버스 프록시 설정 문제 감지

### 6.5 PeriodicWork — 주기적 관리 작업

`PeriodicWork`(`core/src/main/java/hudson/model/PeriodicWork.java`)는 백그라운드에서 주기적으로 실행되는 관리 작업의 확장점이다.

```java
// PeriodicWork.java
public abstract class PeriodicWork extends SafeTimerTask implements ExtensionPoint {
    // 실행 주기 (밀리초)
    public abstract long getRecurrencePeriod();

    // 시간 상수
    protected static final long MIN  = 1000 * 60;
    protected static final long HOUR = 60 * MIN;
    protected static final long DAY  = 24 * HOUR;

    // 초기 지연: 0 ~ recurrencePeriod 범위의 랜덤 값
    public long getInitialDelay() {
        return Math.abs(RANDOM.nextLong()) % getRecurrencePeriod();
    }
}
```

`PeriodicWork.init()`에서 `Timer.get().scheduleAtFixedRate()`로 스케줄링된다:

```java
@Initializer(after = JOB_CONFIG_ADAPTED)
public static void init() {
    ExtensionList<PeriodicWork> extensionList = all();
    for (PeriodicWork p : extensionList) {
        Timer.get().scheduleAtFixedRate(p,
            p.getInitialDelay(),
            p.getRecurrencePeriod(),
            TimeUnit.MILLISECONDS);
    }
}
```

주요 내장 PeriodicWork:

| 클래스 | 주기 | 기능 |
|--------|------|------|
| `FingerprintCleanupThread` | DAY (24시간) | 오래된 Fingerprint 정리 |
| `WorkspaceCleanupThread` | HOUR (기본) | 미사용 워크스페이스 정리 |
| `DownloadService.Downloadable` | - | 업데이트 센터 메타데이터 갱신 |

```java
// FingerprintCleanupThread.java
@Extension @Symbol("fingerprintCleanup")
public class FingerprintCleanupThread extends AsyncPeriodicWork {
    @Override
    public long getRecurrencePeriod() {
        return DAY;  // 24시간마다 실행
    }
}

// WorkspaceCleanupThread.java
@Extension @Symbol("workspaceCleanup")
public class WorkspaceCleanupThread extends AsyncPeriodicWork {
    @Override
    public long getRecurrencePeriod() {
        return recurrencePeriodHours * HOUR;  // 기본 1시간
    }
    // 기본 보존 기간: 30일 (retentionInDays * DAY)
}
```

### 6.6 로그 관리 — RingBufferLogHandler

소스코드 경로: `core/src/main/java/hudson/util/RingBufferLogHandler.java`

```java
// RingBufferLogHandler.java — 순환 로그 버퍼
public class RingBufferLogHandler extends Handler {
    private static final int DEFAULT_RING_BUFFER_SIZE =
        Integer.getInteger(RingBufferLogHandler.class.getName() + ".defaultSize", 256);

    private int start = 0;
    private final LogRecord[] records;  // 링 버퍼 배열
    private int size;

    @Override
    public void publish(LogRecord record) {
        synchronized (this) {
            int len = records.length;
            records[(start + size) % len] = record;  // 순환 삽입
            if (size == len) {
                start = (start + 1) % len;  // 버퍼 가득 차면 가장 오래된 것 덮어쓰기
            } else {
                size++;
            }
        }
    }
}
```

`WebAppMain`에서 이 핸들러를 설치한다:

```java
// WebAppMain.java
private final RingBufferLogHandler handler =
    new RingBufferLogHandler(WebAppMain.getDefaultRingBufferSize()) {
        @Override
        public synchronized void publish(LogRecord record) {
            if (record.getLevel().intValue() >= Level.INFO.intValue()) {
                super.publish(record);  // INFO 이상만 버퍼에 저장
            }
        }
    };

private void installLogger() {
    Jenkins.logRecords = handler.getView();  // 역순 조회용 뷰
    Logger.getLogger("").addHandler(handler);
}
```

링 버퍼의 특징:
- **고정 크기**: 기본 256개 레코드 (시스템 프로퍼티로 조정 가능)
- **순환 덮어쓰기**: 가득 차면 가장 오래된 레코드부터 덮어쓰기
- **역순 조회**: `getView()`가 최신 레코드를 먼저 반환하는 뷰 제공
- **INFO 이상만**: `WebAppMain`의 오버라이드로 INFO 미만은 필터링

### 6.7 커스텀 로그 레코더

`/manage/log`에서 커스텀 로그 레코더를 생성하여 특정 패키지/클래스의 로그를 세밀하게 모니터링할 수 있다.

```
커스텀 로그 레코더 설정 예시
┌────────────────────────────────────┐
│ 이름: plugin-debug                  │
│                                     │
│ 로거:                               │
│   hudson.plugins.git → FINE         │
│   org.jenkinsci.plugins → FINE      │
│   hudson.model.Queue → FINE         │
│                                     │
│ 출력: /manage/log/plugin-debug     │
└────────────────────────────────────┘
```

---

## 7. 트러블슈팅

### 7.1 BootFailure — 부팅 실패 추적

소스코드 경로: `core/src/main/java/hudson/util/BootFailure.java`

Jenkins는 부팅 실패를 체계적으로 추적하고 복구를 돕는 메커니즘을 갖추고 있다.

```java
// BootFailure.java — 부팅 실패 처리
public abstract class BootFailure extends ErrorObject {
    public void publish(ServletContext context, @CheckForNull File home) {
        LOGGER.log(Level.SEVERE, "Failed to initialize Jenkins", this);

        // 1. 에러 페이지 설정
        WebApp.get(context).setApp(this);

        // 2. boot-failure Groovy 훅 스크립트 실행
        if (home != null) {
            new GroovyHookScript("boot-failure", context, home,
                    BootFailure.class.getClassLoader())
                .bind("exception", this)
                .bind("home", home)
                .bind("servletContext", context)
                .bind("attempts", loadAttempts(home))
                .run();
        }

        // 3. Lifecycle에 실패 알림
        Jenkins.get().getLifecycle().onBootFailure(this);
    }
}
```

부팅 실패 추적 흐름:

```
부팅 실패 추적 흐름
┌─────────────────┐
│ Jenkins 시작     │
│ WebAppMain       │
│ .contextInit()   │
└───────┬─────────┘
        │
        ▼
┌─────────────────────────────────────────┐
│ recordBootAttempt(home)                  │
│ → failed-boot-attempts.txt에 타임스탬프 │
│   추가 (append)                          │
└───────┬─────────────────────────────────┘
        │
   ┌────▼────┐
   │ 초기화   │
   │ 성공?    │
   └────┬────┘
        │
   ┌────┼────┐
   │         │
  Yes       No
   │         │
   ▼         ▼
┌───────┐ ┌──────────────────────────────────┐
│ 파일   │ │ BootFailure.publish()             │
│ 삭제   │ │ ├── 에러 페이지 표시              │
│        │ │ ├── boot-failure.groovy 실행      │
│        │ │ │   바인딩: exception, home,       │
│        │ │ │          attempts(실패 이력)     │
│        │ │ └── lifecycle.onBootFailure()     │
└───────┘ └──────────────────────────────────┘
```

BootFailure 하위 클래스들:

| 클래스 | 원인 |
|--------|------|
| `HudsonFailedToLoad` | 일반적인 초기화 실패 |
| `NoHomeDir` | JENKINS_HOME 디렉토리 생성 실패 |
| `NoTempDir` | 임시 디렉토리 접근 불가 |
| `IncompatibleVMDetected` | 호환되지 않는 JVM |
| `IncompatibleServletVersionDetected` | 서블릿 버전 불일치 |
| `IncompatibleAntVersionDetected` | Ant 버전 불일치 |
| `AWTProblem` | AWT 서브시스템 문제 |
| `InsufficientPermissionDetected` | 권한 부족 |

### 7.2 HudsonIsLoading — 초기화 중 표시

소스코드 경로: `core/src/main/java/hudson/util/HudsonIsLoading.java`

```java
// HudsonIsLoading.java
public class HudsonIsLoading {
    public void doDynamic(StaplerRequest2 req, StaplerResponse2 rsp)
        throws IOException, ServletException, InterruptedException {
        rsp.setStatus(SC_SERVICE_UNAVAILABLE);  // HTTP 503 반환
        req.getView(this, "index.jelly").forward(req, rsp);
    }
}
```

초기화 중(`context.setAttribute("app", new HudsonIsLoading())`)에는 모든 HTTP 요청에 대해 503 Service Unavailable과 함께 로딩 페이지를 표시한다.

### 7.3 HudsonFailedToLoad — 로드 실패 시 에러 페이지

```java
// HudsonFailedToLoad.java
public class HudsonFailedToLoad extends BootFailure {
    public final Throwable exception;

    public HudsonFailedToLoad(Throwable exception) {
        super(exception);
        this.exception = exception;
    }
}
```

로드 실패 시 에러 페이지가 표시된다. `index.jelly`에 의해 스택 트레이스를 포함한 에러 정보가 렌더링된다.

### 7.4 Safe Restart — 안전한 재시작

`safeRestart()`는 실행 중인 빌드가 모두 완료된 후에 재시작을 수행한다.

소스코드 경로: `core/src/main/java/jenkins/model/Jenkins.java`

```java
// Jenkins.java
public void safeRestart(String message) throws RestartNotSupportedException {
    final Lifecycle lifecycle = restartableLifecycle();
    // 1. Quiet Down: 새 빌드 시작 중단
    quietDownInfo = new QuietDownInfo(message, true);

    new Thread("safe-restart thread") {
        @Override
        public void run() {
            try (ACLContext ctx = ACL.as2(ACL.SYSTEM2)) {
                // 2. 모든 활성 Executor가 완료될 때까지 대기
                doQuietDown(true, 0, message, true);

                // 3. 여전히 Quieting Down 상태인지 확인
                //    (사용자가 취소하지 않았는지)
                if (isQuietingDown()) {
                    // 4. "재시작 중" 페이지 표시
                    getServletContext().setAttribute("app",
                        new HudsonIsRestarting(true));

                    // 5. 브라우저가 페이지를 로드할 시간 확보
                    lifecycle.onStatusUpdate("Restart in 10 seconds");
                    Thread.sleep(TimeUnit.SECONDS.toMillis(10));

                    // 6. 종료 알림 및 실제 재시작
                    lifecycle.onStop(exitUser, null);
                    Listeners.notify(RestartListener.class, true,
                        RestartListener::onRestart);
                    lifecycle.restart();
                } else {
                    lifecycle.onStatusUpdate("Safe-restart mode cancelled");
                }
            }
        }
    }.start();
}
```

Safe Restart와 일반 Restart의 차이:

```
┌────────────────────────────────────────────────────────────┐
│          일반 Restart                Safe Restart           │
│                                                             │
│  1. 즉시 재시작             1. Quiet Down 모드 진입        │
│  2. 실행 중 빌드 중단       2. 새 빌드 시작 차단           │
│  3. HudsonIsRestarting     3. 실행 중 빌드 완료 대기       │
│  4. 5초 후 restart()       4. 전부 완료 후 재시작          │
│                             5. HudsonIsRestarting(true)     │
│                             6. 10초 후 restart()            │
│                             7. 사용자가 취소 가능           │
└────────────────────────────────────────────────────────────┘
```

### 7.5 Lifecycle — 재시작 메커니즘

`Lifecycle`(`core/src/main/java/hudson/lifecycle/Lifecycle.java`)은 Jenkins 프로세스의 시작/중지/재시작을 관리하는 추상 클래스이다.

```java
// Lifecycle.java — 환경별 자동 감지
public static synchronized Lifecycle get() {
    if (INSTANCE == null) {
        String p = SystemProperties.getString("hudson.lifecycle");
        if (p != null) {
            // 시스템 프로퍼티로 지정된 클래스 로드
            instance = (Lifecycle) cl.loadClass(p)
                .getDeclaredConstructor().newInstance();
        } else {
            if (Functions.isWindows()) {
                // Windows: 재시작 미지원
            } else if (System.getenv("SMF_FMRI") != null) {
                instance = new SolarisSMFLifecycle();  // Solaris SMF
            } else if (System.getenv("NOTIFY_SOCKET") != null) {
                instance = new SystemdLifecycle();     // systemd
            } else {
                instance = new UnixLifecycle();        // Unix/Linux
            }
        }
    }
    return INSTANCE;
}
```

Lifecycle 구현체별 재시작 동작:

| 구현체 | 환경 | 재시작 방법 |
|--------|------|-------------|
| `UnixLifecycle` | Unix/Linux | exec 시스템 호출로 프로세스 교체 |
| `SystemdLifecycle` | systemd | sd_notify + 프로세스 재시작 |
| `SolarisSMFLifecycle` | Solaris SMF | SMF 서비스 재시작 |
| `ExitLifecycle` | Docker 등 | 프로세스 종료 (외부에서 재시작) |
| (Windows) | Windows | 재시작 미지원 |

### 7.6 RestartListener — 재시작 제어

```java
// RestartListener.java
public abstract class RestartListener implements ExtensionPoint {
    // safe restart 중 주기적 호출
    // false 반환 시 재시작 차단
    public abstract boolean isReadyToRestart()
        throws IOException, InterruptedException;

    // 재시작 직전 호출
    public void onRestart() {}
}
```

플러그인이 `RestartListener`를 구현하여 재시작 시점을 제어할 수 있다. 예를 들어, 에이전트와의 연결 해제를 먼저 수행한 후 재시작을 허용하는 식이다.

### 7.7 스크립트 콘솔 (/script)

`/script`는 Groovy 스크립트를 서버에서 직접 실행하는 강력한 디버깅 도구이다.

```java
// Jenkins.java
public void doScript(StaplerRequest2 req, StaplerResponse2 rsp)
    throws IOException, ServletException {
    _doScript(req, rsp, req.getView(this, "_script.jelly"),
              FilePath.localChannel, getACL());
}

public static void _doScript(StaplerRequest2 req, StaplerResponse2 rsp,
    RequestDispatcher view, VirtualChannel channel, ACL acl)
    throws IOException, ServletException {
    // ADMINISTER 권한 필수
    acl.checkPermission(ADMINISTER);
    // ...
}
```

유용한 Groovy 스크립트 예시:

```groovy
// 실행 중인 빌드 목록
Jenkins.instance.getAllItems(Job.class).each { job ->
    job.builds.each { build ->
        if (build.isBuilding()) {
            println "${job.fullName} #${build.number} - ${build.description}"
        }
    }
}

// 비활성 에이전트 목록
Jenkins.instance.computers.each { c ->
    if (c.isOffline()) {
        println "${c.name}: ${c.offlineCauseReason}"
    }
}

// 플러그인 목록
Jenkins.instance.pluginManager.plugins.each { p ->
    println "${p.shortName}: ${p.version} (${p.isActive() ? 'active' : 'inactive'})"
}

// 큐 정리
Jenkins.instance.queue.clear()

// 시스템 프로퍼티 확인
System.getProperties().each { k, v ->
    if (k.toString().startsWith("jenkins") || k.toString().startsWith("hudson"))
        println "$k = $v"
}
```

> **보안 주의**: 스크립트 콘솔은 `ADMINISTER` 권한이 필요하며, 서버에서 임의의 코드를 실행할 수 있으므로 접근을 엄격히 제한해야 한다.

### 7.8 일반적인 문제 해결

```
┌────────────────────────────────────────────────────────────┐
│ 증상                  │ 원인 및 해결                        │
├───────────────────────┼────────────────────────────────────┤
│ 503 로딩 페이지 지속  │ 초기화 스레드 블록                  │
│                       │ → 스레드 덤프 확인 (/threadDump)    │
│                       │ → 플러그인 호환성 확인               │
├───────────────────────┼────────────────────────────────────┤
│ 부팅 실패 반복        │ failed-boot-attempts.txt 확인       │
│                       │ → boot-failure.groovy.d/ 스크립트   │
│                       │ → 문제 플러그인 비활성화            │
│                       │   (.jpi.disabled 파일 생성)          │
├───────────────────────┼────────────────────────────────────┤
│ OOM (OutOfMemory)     │ JVM 힙 부족                         │
│                       │ → -Xmx 증가                         │
│                       │ → 빌드 이력 정리                    │
│                       │ → 힙 덤프 분석                      │
├───────────────────────┼────────────────────────────────────┤
│ 빌드 큐 정체          │ Executor 부족 또는 라벨 불일치       │
│                       │ → numExecutors 확인                  │
│                       │ → 에이전트 상태 확인                │
│                       │ → /script에서 큐 분석                │
├───────────────────────┼────────────────────────────────────┤
│ 플러그인 호환성 오류  │ 의존성 버전 불일치                   │
│                       │ → 안전모드 부팅                      │
│                       │ → 문제 플러그인 비활성화            │
│                       │ → Jenkins 및 플러그인 업그레이드     │
├───────────────────────┼────────────────────────────────────┤
│ config.xml 손상       │ XStream 역직렬화 실패               │
│                       │ → 백업에서 복원                      │
│                       │ → XML 수동 수정 (문법 오류 확인)    │
├───────────────────────┼────────────────────────────────────┤
│ 에이전트 연결 불가    │ 네트워크/인증 문제                   │
│                       │ → TcpSlaveAgentListener 포트 확인   │
│                       │ → 에이전트 시크릿 재발급            │
│                       │ → 방화벽 규칙 확인                  │
└───────────────────────┴────────────────────────────────────┘
```

---

## 8. 백업/복구

### 8.1 JENKINS_HOME 전체 백업

Jenkins의 모든 상태는 `JENKINS_HOME` 디렉토리에 파일로 저장되므로, 파일시스템 수준의 백업이 가장 확실한 방법이다.

```bash
# 전체 백업 (Jenkins 중지 후 권장)
tar czf jenkins_backup_$(date +%Y%m%d).tar.gz \
  -C $JENKINS_HOME .

# 핵심 데이터만 백업 (빌드 로그 제외)
tar czf jenkins_config_backup_$(date +%Y%m%d).tar.gz \
  -C $JENKINS_HOME \
  config.xml \
  credentials.xml \
  secret.key \
  secrets/ \
  users/ \
  nodes/ \
  plugins/*.jpi \
  jobs/*/config.xml

# rsync를 사용한 증분 백업
rsync -avz --delete \
  $JENKINS_HOME/ \
  /backup/jenkins/
```

### 8.2 백업 대상 우선순위

```
백업 우선순위 (중요도 순)
┌────────────────────────────────────────────────────────┐
│ [필수] 암호화 키                                       │
│   secret.key, secrets/, identity.key.enc               │
│   → 이것이 없으면 credentials.xml 복호화 불가          │
│                                                         │
│ [필수] 시스템 설정                                      │
│   config.xml, credentials.xml, nodeMonitors.xml         │
│   hudson.model.UpdateCenter.xml                         │
│                                                         │
│ [필수] Job 설정                                         │
│   jobs/*/config.xml                                     │
│   → Job 정의 (파이프라인, 설정)                        │
│                                                         │
│ [중요] 사용자 및 노드                                  │
│   users/*/config.xml, nodes/*/config.xml                │
│                                                         │
│ [중요] 플러그인                                         │
│   plugins/*.jpi                                         │
│   → 재다운로드 가능하지만 버전 고정을 위해 백업         │
│                                                         │
│ [선택] 빌드 이력                                        │
│   jobs/*/builds/                                        │
│   → 용량이 크지만 이력 보존이 필요하면 백업             │
│                                                         │
│ [선택] 핑거프린트, 로그                                │
│   fingerprints/, logs/                                  │
│   → 재생성 가능한 데이터                                │
│                                                         │
│ [불필요] WAR 전개, 캐시                                │
│   war/, updates/                                        │
│   → 시작 시 자동 재생성                                 │
└────────────────────────────────────────────────────────┘
```

### 8.3 Thin Backup 플러그인

Thin Backup 플러그인을 사용하면 Jenkins UI에서 백업을 스케줄링할 수 있다.

주요 기능:
- 전체 백업 및 차등 백업 지원
- cron 식 스케줄링
- 백업 보존 정책 (최대 보관 개수)
- 빌드 결과 포함/제외 옵션

### 8.4 복원

```bash
# 1. Jenkins 중지
systemctl stop jenkins  # 또는 다른 방법

# 2. 기존 JENKINS_HOME 백업 (안전 조치)
mv $JENKINS_HOME ${JENKINS_HOME}.old

# 3. 백업 복원
mkdir -p $JENKINS_HOME
tar xzf jenkins_backup_20260304.tar.gz -C $JENKINS_HOME

# 4. 권한 복원 (필요 시)
chown -R jenkins:jenkins $JENKINS_HOME

# 5. Jenkins 시작
systemctl start jenkins
```

### 8.5 마이그레이션

서버 간 마이그레이션은 기본적으로 `JENKINS_HOME` 디렉토리를 복사하는 것이다.

```
마이그레이션 절차
┌────────────┐         ┌────────────┐
│  Old Server │         │  New Server │
│             │         │             │
│  JENKINS_  │  copy   │  JENKINS_  │
│  HOME/     │ ──────→ │  HOME/     │
│            │         │             │
│  Jenkins   │         │  Jenkins   │
│  v2.440    │         │  v2.440    │
└────────────┘         └────────────┘

단계:
1. Old: Jenkins 중지
2. Old: JENKINS_HOME 전체 복사 (rsync/scp/tar)
3. New: 동일 버전 Jenkins 설치
4. New: JENKINS_HOME 경로 설정 (환경변수)
5. New: Jenkins 시작
6. 확인: 설정, Job, 빌드 이력, 자격증명 검증
```

마이그레이션 시 주의사항:
- **Java 버전**: 동일하거나 호환되는 Java 버전 사용
- **Jenkins 버전**: 동일한 버전 권장 (다운그레이드 비지원)
- **OS 차이**: 절대 경로를 사용하는 빌드 스크립트 수정 필요
- **에이전트 재연결**: 에이전트의 컨트롤러 URL 업데이트 필요
- **인증서/키**: `secrets/` 디렉토리 반드시 포함 (없으면 자격증명 복호화 불가)

---

## 9. 성능 튜닝

### 9.1 Executor 수 조정

Jenkins 컨트롤러의 Executor 수는 동시에 실행할 수 있는 빌드 수를 결정한다.

```java
// Jenkins.java
private int numExecutors = 2;  // 기본값: 2

@Override
public int getNumExecutors() {
    return numExecutors;
}

public void setNumExecutors(int n) throws IOException {
    if (n < 0) {
        throw new IllegalArgumentException(
            "Incorrect field \"# of executors\": " + n);
    }
    if (this.numExecutors != n) {
        this.numExecutors = n;
        updateComputers(this);  // 컴퓨터 풀 재구성
        save();
    }
}
```

Executor 설정 가이드:

```
Executor 수 결정 기준
┌──────────────────────────────────────────────────────┐
│ 컨트롤러 (Built-in Node)                             │
│                                                       │
│ 권장: 0 ~ 2개                                        │
│                                                       │
│ 이유:                                                 │
│ - 컨트롤러에서 빌드 실행은 보안/성능상 비권장         │
│ - 빌드 부하는 에이전트에 위임                         │
│ - 0으로 설정하면 컨트롤러에서 빌드 실행 불가          │
│ - Pipeline의 lightweight checkout에만 사용            │
│                                                       │
│ 에이전트 (Agent Node)                                │
│                                                       │
│ 권장: CPU 코어 수 x 1~2                              │
│                                                       │
│ - I/O 바운드 빌드: 코어 수 x 2                       │
│ - CPU 바운드 빌드: 코어 수 x 1                       │
│ - 메모리 제한 고려: 빌드당 필요 메모리 x executor 수  │
└──────────────────────────────────────────────────────┘
```

### 9.2 빌드 이력 정리 — BuildDiscarder / LogRotator

과도한 빌드 이력은 디스크 공간과 Jenkins 성능에 영향을 미친다. `BuildDiscarder`(`core/src/main/java/jenkins/model/BuildDiscarder.java`)의 기본 구현인 `LogRotator`가 빌드 이력 정리를 담당한다.

소스코드 경로: `core/src/main/java/hudson/tasks/LogRotator.java`

```java
// LogRotator.java — 기본 BuildDiscarder 구현
public class LogRotator extends BuildDiscarder {
    // 최대 빌드 보관 기간 (일)
    private int daysToKeep;
    // 최대 빌드 보관 개수
    private int numToKeep;
    // 아티팩트 보관 기간 (일)
    private int artifactDaysToKeep;
    // 아티팩트 보관 개수
    private int artifactNumToKeep;
}
```

Job 설정에서 BuildDiscarder 구성:

```xml
<!-- Job config.xml 예시 -->
<project>
  <properties>
    <jenkins.model.BuildDiscarderProperty>
      <strategy class="hudson.tasks.LogRotator">
        <daysToKeep>30</daysToKeep>
        <numToKeep>100</numToKeep>
        <artifactDaysToKeep>7</artifactDaysToKeep>
        <artifactNumToKeep>10</artifactNumToKeep>
      </strategy>
    </jenkins.model.BuildDiscarderProperty>
  </properties>
</project>
```

Pipeline에서 `buildDiscarder` 설정:

```groovy
pipeline {
    options {
        buildDiscarder(logRotator(
            daysToKeepStr: '30',
            numToKeepStr: '100',
            artifactDaysToKeepStr: '7',
            artifactNumToKeepStr: '10'
        ))
    }
    // ...
}
```

### 9.3 큐 관리

```java
// Jenkins.java — quietPeriod (빌드 대기 시간)
/*package*/ Integer quietPeriod;  // 기본값 5초

public int getQuietPeriod() {
    return quietPeriod != null ? quietPeriod : 5;
}
```

`quietPeriod`는 빌드 트리거 후 실제 빌드 시작까지의 대기 시간이다. 이 기간 동안 동일한 빌드 요청이 들어오면 하나로 합쳐진다(coalescence).

```
큐 관리 파라미터
┌────────────────────────────────────────────────────────┐
│ quietPeriod (기본 5초)                                  │
│                                                         │
│ 트리거 ──→ [5초 대기] ──→ 빌드 시작                    │
│                  ↑                                      │
│         동일 트리거 시                                   │
│         대기 시간 리셋                                   │
│                                                         │
│ 용도:                                                   │
│ - SCM 변경 빈번한 경우 불필요한 빌드 방지               │
│ - 0으로 설정하면 즉시 빌드 시작                         │
│                                                         │
│ 설정:                                                   │
│ - 글로벌: Jenkins 시스템 설정                            │
│ - Job별: Job 설정에서 개별 지정                         │
│ - API: Jenkins.get().setQuietPeriod(n)                  │
└────────────────────────────────────────────────────────┘
```

### 9.4 JVM 옵션

Jenkins는 Java 애플리케이션이므로 JVM 옵션이 성능에 직접적인 영향을 미친다.

```bash
# 권장 JVM 옵션 (환경별 조정 필요)
JAVA_OPTS="\
  -Xms2g \
  -Xmx4g \
  -XX:+UseG1GC \
  -XX:+ParallelRefProcEnabled \
  -XX:+UseStringDeduplication \
  -XX:MaxMetaspaceSize=512m \
  -Djava.awt.headless=true \
  -Djenkins.model.Jenkins.crumbIssuerProxyCompatibility=true"

java $JAVA_OPTS -jar jenkins.war
```

JVM 옵션 가이드:

| 옵션 | 설명 | 권장값 |
|------|------|--------|
| `-Xms` | 초기 힙 크기 | 1~2G |
| `-Xmx` | 최대 힙 크기 | 2~8G (규모에 따라) |
| `-XX:+UseG1GC` | G1 가비지 컬렉터 사용 | 활성화 |
| `-XX:MaxMetaspaceSize` | Metaspace 최대 크기 | 256~512M |
| `-XX:+UseStringDeduplication` | 문자열 중복 제거 (G1 전용) | 활성화 |
| `-XX:+ParallelRefProcEnabled` | 참조 처리 병렬화 | 활성화 |

### 9.5 디스크 I/O 최적화

Jenkins는 파일시스템에 크게 의존하므로 디스크 성능이 전체 성능에 큰 영향을 미친다.

```
디스크 I/O 최적화 전략
┌────────────────────────────────────────────────────────┐
│ 1. SSD 사용                                            │
│    → JENKINS_HOME을 SSD에 배치                         │
│    → 특히 jobs/ 디렉토리가 많은 I/O 발생              │
│                                                         │
│ 2. 워크스페이스 분리                                    │
│    → 빌드 워크스페이스를 별도 디스크에 배치             │
│    → -Djenkins.model.Jenkins.workspacesDir 설정        │
│                                                         │
│ 3. 빌드 로그 관리                                      │
│    → BuildDiscarder로 오래된 빌드 자동 삭제             │
│    → 아티팩트는 별도 저장소에 보관 (Artifactory 등)     │
│                                                         │
│ 4. tmpdir 설정                                          │
│    → -Djava.io.tmpdir을 빠른 디스크에 설정             │
│    → 빌드 중 임시 파일 생성이 빈번                     │
│                                                         │
│ 5. Fingerprint 정리                                     │
│    → FingerprintCleanupThread가 매일 실행              │
│    → 오래된 fingerprint 자동 삭제                      │
└────────────────────────────────────────────────────────┘
```

### 9.6 에이전트 활용 전략

컨트롤러의 부하를 줄이고 확장성을 확보하기 위한 에이전트 활용 전략이다.

```
에이전트 아키텍처 권장 구성
┌──────────────────────────────────────────────────────────┐
│                                                           │
│  ┌─────────────────┐                                     │
│  │  Controller      │  numExecutors = 0                  │
│  │  (Master)        │  빌드 실행 안 함                   │
│  │                  │  설정/스케줄링만 담당               │
│  └────────┬─────────┘                                     │
│           │                                               │
│     ┌─────┼─────────────────────────┐                    │
│     │     │                          │                    │
│     ▼     ▼                          ▼                    │
│  ┌──────┐ ┌──────┐              ┌──────────┐            │
│  │Agent │ │Agent │    ...       │Agent     │            │
│  │Java  │ │.NET  │              │Docker    │            │
│  │빌드  │ │빌드  │              │빌드      │            │
│  │x4 exec│ │x2 exec│           │동적 생성 │            │
│  └──────┘ └──────┘              └──────────┘            │
│                                                           │
│  라벨 기반 라우팅:                                        │
│  - java-build → Agent Java                               │
│  - dotnet-build → Agent .NET                             │
│  - docker → Agent Docker                                 │
│                                                           │
│  Kubernetes 환경:                                         │
│  - Pod Template으로 에이전트 동적 프로비저닝              │
│  - 빌드 완료 후 자동 삭제                                │
└──────────────────────────────────────────────────────────┘
```

### 9.7 성능 모니터링 항목

Jenkins 운영 시 지속적으로 모니터링해야 할 항목이다.

```
핵심 모니터링 메트릭
┌────────────────────────────────────────────────────────┐
│ 시스템 레벨                                            │
│ ├── JVM 힙 사용량 (Current/Max)                        │
│ ├── GC 빈도 및 소요 시간                              │
│ ├── CPU 사용률                                         │
│ ├── 디스크 사용량 (JENKINS_HOME)                       │
│ └── 열린 파일 디스크립터 수                            │
│                                                         │
│ Jenkins 레벨                                           │
│ ├── 큐 길이 (대기 중인 빌드 수)                       │
│ ├── 큐 대기 시간 (빌드 시작까지 소요 시간)            │
│ ├── Executor 사용률 (사용 중 / 전체)                  │
│ ├── 빌드 실패율                                        │
│ ├── 에이전트 연결 상태                                 │
│ └── 플러그인 로드 시간                                 │
│                                                         │
│ 도구                                                    │
│ ├── Prometheus + Grafana (metrics 플러그인)             │
│ ├── JavaMelody (monitoring 플러그인)                    │
│ ├── /threadDump (스레드 분석)                           │
│ └── /systemInfo (시스템 정보)                           │
└────────────────────────────────────────────────────────┘
```

### 9.8 대규모 환경 운영 팁

```
대규모 Jenkins 운영 체크리스트
┌────────────────────────────────────────────────────────┐
│ □ 컨트롤러 Executor 0으로 설정                         │
│ □ 에이전트 풀 적절히 확보 (Kubernetes 동적 스케일링)   │
│ □ 빌드 이력 정리 정책 적용 (모든 Job에 BuildDiscarder) │
│ □ 큐 모니터링 및 알림 설정                             │
│ □ JVM 힙 적절히 설정 (4~8G)                            │
│ □ SSD 사용 (JENKINS_HOME)                              │
│ □ 정기 백업 자동화                                     │
│ □ 플러그인 최소화 (필요한 것만 설치)                   │
│ □ CasC로 설정 관리 (코드로 선언적 관리)                │
│ □ 보안 설정 강화 (SecurityRealm, AuthorizationStrategy)│
│ □ 모니터링 대시보드 구축 (Prometheus/Grafana)           │
│ □ 재해 복구 계획 수립 (백업/복원 테스트)               │
└────────────────────────────────────────────────────────┘
```

---

## 소스코드 참조 요약

| 파일 | 경로 | 역할 |
|------|------|------|
| `Main.java` | `war/src/main/java/executable/Main.java` | WAR 실행 진입점, Java 검증, Winstone 위임 |
| `WebAppMain.java` | `core/src/main/java/hudson/WebAppMain.java` | 서블릿 초기화, JENKINS_HOME 결정, Jenkins 인스턴스 생성 |
| `Jenkins.java` | `core/src/main/java/jenkins/model/Jenkins.java` | Jenkins 핵심 모델, 시스템 설정, 재시작 로직 |
| `BootFailure.java` | `core/src/main/java/hudson/util/BootFailure.java` | 부팅 실패 추적/복구 |
| `RingBufferLogHandler.java` | `core/src/main/java/hudson/util/RingBufferLogHandler.java` | 순환 로그 버퍼 |
| `PluginManager.java` | `core/src/main/java/hudson/PluginManager.java` | 플러그인 관리 |
| `SecurityRealm.java` | `core/src/main/java/hudson/security/SecurityRealm.java` | 인증 체계 추상 클래스 |
| `AuthorizationStrategy.java` | `core/src/main/java/hudson/security/AuthorizationStrategy.java` | 인가 전략 |
| `CrumbIssuer.java` | `core/src/main/java/hudson/security/csrf/CrumbIssuer.java` | CSRF 보호 |
| `TcpSlaveAgentListener.java` | `core/src/main/java/hudson/TcpSlaveAgentListener.java` | 에이전트 TCP 리스너 |
| `PeriodicWork.java` | `core/src/main/java/hudson/model/PeriodicWork.java` | 주기적 관리 작업 |
| `Lifecycle.java` | `core/src/main/java/hudson/lifecycle/Lifecycle.java` | 프로세스 라이프사이클 관리 |
| `XmlFile.java` | `core/src/main/java/hudson/XmlFile.java` | XStream XML 영속화 |
| `SystemProperties.java` | `core/src/main/java/jenkins/util/SystemProperties.java` | 시스템 속성 관리 |
| `InitMilestone.java` | `core/src/main/java/hudson/init/InitMilestone.java` | 초기화 단계 정의 |
| `LogRotator.java` | `core/src/main/java/hudson/tasks/LogRotator.java` | 빌드 이력 정리 |
| `Queue.java` | `core/src/main/java/hudson/model/Queue.java` | 빌드 큐 관리 |
| `GroovyHookScript.java` | `core/src/main/java/jenkins/util/groovy/GroovyHookScript.java` | Groovy 훅 스크립트 |
| `HudsonPrivateSecurityRealm.java` | `core/src/main/java/hudson/security/HudsonPrivateSecurityRealm.java` | 내장 사용자 DB |
| `RestartListener.java` | `core/src/main/java/hudson/model/RestartListener.java` | 재시작 제어 확장점 |
