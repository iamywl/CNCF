# Jenkins 코드 구조

이 문서는 Jenkins 프로젝트(`jenkins/`)의 디렉토리 구조, Maven 멀티모듈 체계, 빌드 시스템,
핵심 외부 의존성, 그리고 패키지 명명 규칙을 분석한다.

> 소스 경로: `/Users/ywlee/CNCF/jenkins/`
> 버전: 2.553-SNAPSHOT (groupId: `org.jenkins-ci.main`, artifactId: `jenkins-parent`)

---

## 목차

1. [프로젝트 최상위 구조](#1-프로젝트-최상위-구조)
2. [Maven 멀티모듈 아키텍처](#2-maven-멀티모듈-아키텍처)
3. [core 모듈 — 핵심 로직](#3-core-모듈--핵심-로직)
4. [war 모듈 — WAR 패키징 및 독립 실행](#4-war-모듈--war-패키징-및-독립-실행)
5. [cli 모듈 — CLI 클라이언트](#5-cli-모듈--cli-클라이언트)
6. [test 모듈 — 통합 테스트](#6-test-모듈--통합-테스트)
7. [websocket 모듈 — WebSocket SPI/구현](#7-websocket-모듈--websocket-spi구현)
8. [bom 모듈 — 의존성 버전 관리](#8-bom-모듈--의존성-버전-관리)
9. [coverage 모듈 — 코드 커버리지 집계](#9-coverage-모듈--코드-커버리지-집계)
10. [프론트엔드 빌드 시스템](#10-프론트엔드-빌드-시스템)
11. [빌드 시스템 상세](#11-빌드-시스템-상세)
12. [핵심 외부 의존성](#12-핵심-외부-의존성)
13. [주요 파일 크기 분석](#13-주요-파일-크기-분석)
14. [패키지 명명 규칙과 이중 패키지 구조](#14-패키지-명명-규칙과-이중-패키지-구조)
15. [리소스 구조 — Jelly 템플릿과 i18n](#15-리소스-구조--jelly-템플릿과-i18n)

---

## 1. 프로젝트 최상위 구조

```
jenkins/
├── pom.xml                  # Parent POM (멀티모듈 정의)
├── bom/                     # BOM(Bill of Materials) — 의존성 버전 중앙 관리
├── core/                    # 핵심 로직 (Java 1,236개 파일, ~212K줄)
├── war/                     # WAR 패키징 + 독립 실행 진입점
├── cli/                     # CLI 클라이언트 (원격 명령 실행)
├── test/                    # 통합 테스트 (484개 테스트 파일)
├── coverage/                # JaCoCo 코드 커버리지 집계
├── websocket/
│   ├── spi/                 # WebSocket SPI(Service Provider Interface)
│   └── jetty12-ee9/         # Jetty 12 EE9 WebSocket 구현체
├── src/
│   ├── main/js/             # 프론트엔드 JavaScript (80개 파일)
│   ├── main/scss/           # 프론트엔드 SCSS (74개 파일)
│   └── checkstyle/          # Checkstyle 설정
├── docs/                    # 프로젝트 문서
├── webpack.config.js        # Webpack 번들링 설정
├── package.json             # npm/yarn 의존성
├── eslint.config.cjs        # ESLint 설정
├── postcss.config.js        # PostCSS 설정
├── Jenkinsfile              # CI 파이프라인 정의
├── licenseCompleter.groovy  # 라이선스 완성 스크립트
└── dummy.keystore           # 개발용 더미 키스토어
```

### 규모 요약

| 항목 | 수치 |
|------|------|
| core Java 소스 파일 | 1,236개 |
| core Java 코드 줄 수 | ~212,000줄 |
| Jelly 템플릿 파일 | 725개 |
| Properties(i18n) 파일 | 7,465개 |
| 통합 테스트 파일 | 484개 |
| 프론트엔드 JS 파일 | 80개 |
| 프론트엔드 SCSS 파일 | 74개 |

---

## 2. Maven 멀티모듈 아키텍처

### Parent POM 구조

```
파일: jenkins/pom.xml
```

```xml
<groupId>org.jenkins-ci.main</groupId>
<artifactId>jenkins-parent</artifactId>
<version>${revision}${changelist}</version>   <!-- 2.553-SNAPSHOT -->
<packaging>pom</packaging>

<parent>
    <groupId>org.jenkins-ci</groupId>
    <artifactId>jenkins</artifactId>           <!-- Jenkins 조직 상위 POM -->
    <version>2.1328.vfd10e2339a_19</version>
</parent>

<modules>
    <module>bom</module>
    <module>websocket/spi</module>
    <module>websocket/jetty12-ee9</module>
    <module>core</module>
    <module>war</module>
    <module>test</module>
    <module>cli</module>
    <module>coverage</module>
</modules>
```

### 모듈 간 의존성 관계

```
+------------------+
|   jenkins-parent |  (POM, 최상위)
+------------------+
        |
        +--- bom (jenkins-bom)           의존성 버전 정의
        |
        +--- websocket/spi              WebSocket SPI 인터페이스
        |       |
        +--- websocket/jetty12-ee9      WebSocket Jetty 구현 (spi 의존)
        |
        +--- cli                         CLI 클라이언트 라이브러리
        |       |
        +--- core (jenkins-core)         핵심 로직 (cli, websocket-spi, remoting 의존)
        |       |
        +--- war (jenkins-war)           WAR 패키징 (core, cli, remoting 의존)
        |       |
        +--- test (jenkins-test)         통합 테스트 (core, war 의존)
        |
        +--- coverage                    커버리지 집계 (core, cli, test 의존)
```

**왜 이런 구조인가?**

- `bom` 분리: 플러그인 개발자가 `jenkins-bom`을 import하면 Jenkins 코어와 동일한 의존성 버전을 자동으로 사용할 수 있다.
  수천 개의 플러그인이 존재하므로 버전 충돌 방지가 핵심이다.
- `cli` 분리: CLI 클라이언트는 서버 없이도 독립 배포 가능해야 한다. `core`에서 `cli`를 의존하지만
  `cli`는 `core`를 의존하지 않는다.
- `websocket` 분리: SPI와 구현을 분리하여 서블릿 컨테이너별 WebSocket 구현을 교체할 수 있다.
- `test` 분리: 통합 테스트는 실제 Jenkins 인스턴스를 띄우므로 별도 모듈로 분리한다.

---

## 3. core 모듈 -- 핵심 로직

```
파일: jenkins/core/pom.xml
artifactId: jenkins-core
```

Jenkins의 모든 핵심 기능이 이 모듈에 집중되어 있다.
Java 소스 파일 1,236개, 약 212,000줄의 코드로 구성된다.

### 3.1 소스 디렉토리 구조

```
core/src/main/java/
├── hudson/                   # 레거시 패키지 (746개 Java 파일)
│   ├── model/                # 212개 — 도메인 모델 (Job, Run, Queue, Computer, Executor 등)
│   ├── util/                 # 121개 — 유틸리티 (XStream2, FormValidation, Secret 등)
│   ├── cli/                  #  67개 — CLICommand 기반 명령 프레임워크
│   ├── security/             #  45개 — SecurityRealm, AuthorizationStrategy, ACL, Permission
│   ├── slaves/               #  33개 — SlaveComputer, ComputerLauncher, RetentionStrategy
│   ├── tasks/                #  22개 — BuildStep, Builder, Publisher, Recorder, Notifier
│   ├── scm/                  #  18개 — SCM, ChangeLogSet, RepositoryBrowser
│   ├── console/              #  14개 — ConsoleAnnotator, ConsoleNote, HyperlinkNote
│   ├── init/                 #  10개 — InitMilestone, Initializer, InitReactorRunner
│   ├── triggers/             #   8개 — Trigger, TimerTrigger, SCMTrigger
│   ├── tools/                #       — ToolInstallation, ToolDescriptor
│   ├── views/                #       — ViewsTabBar, DefaultViewsTabBar
│   ├── widgets/              #       — Widget, HistoryWidget
│   ├── search/               #       — Search, SearchItem, SuggestedItem
│   ├── lifecycle/            #       — Lifecycle, RestartListener
│   ├── logging/              #       — LogRecorder, LogRecorderManager
│   ├── markup/               #       — MarkupFormatter, RawHtmlMarkupFormatter
│   ├── node_monitors/        #       — NodeMonitor, DiskSpaceMonitor
│   ├── diagnosis/            #       — MemoryUsageMonitor, OldDataMonitor
│   ├── scheduler/            #       — CronTab 파서
│   ├── os/                   #       — OS별 유틸리티
│   └── *.java                #  50개 — PluginManager, ExtensionFinder, FilePath 등 루트 클래스
│
└── jenkins/                  # 현대 패키지 (439개 Java 파일)
    ├── model/                # 109개 — Jenkins(싱글턴), GlobalConfiguration
    ├── security/             # 103개 — CSRF, ResourceDomainFilter, apitoken, seed
    ├── util/                 #  52개 — SystemProperties, Timer, JenkinsJVM
    ├── install/              #   4개 — SetupWizard, InstallState
    ├── plugins/              #   1개 — DetachedPluginsUtil
    ├── agents/               #       — 에이전트 관련 현대 API
    ├── appearance/           #       — UI 외관 설정
    ├── cli/                  #       — CLI 현대 API
    ├── console/              #       — 콘솔 출력 현대 API
    ├── diagnostics/          #       — 진단 모니터
    ├── fingerprints/         #       — 핑거프린트 현대 API
    ├── health/               #       — HealthCheck 관련
    ├── job/                  #       — DecoratedRun 등
    ├── management/           #       — ManagementLink, AdministrativeMonitor
    ├── monitor/              #       — JavaLevelAdminMonitor
    ├── scm/                  #       — SCM 현대 API
    ├── search/               #       — 검색 현대 API
    ├── slaves/               #       — DeprecatedAgentProtocolMonitor
    ├── tasks/                #       — 빌드 태스크 현대 API
    ├── telemetry/            #       — Telemetry 수집
    ├── tools/                #       — GlobalToolConfiguration
    ├── triggers/             #       — 트리거 현대 API
    ├── views/                #       — 뷰 현대 API
    ├── websocket/            #       — WebSocket 연결 처리
    └── widgets/              #       — 위젯 현대 API
```

### 3.2 hudson/model/ — 도메인 모델 (212개 파일)

Jenkins의 핵심 도메인 객체가 모두 이 패키지에 있다.
전체 코드의 약 36%(~75,000줄)를 차지하는 가장 큰 패키지이다.

#### 주요 클래스 계층

```
                    ModelObject
                        |
              +---------+----------+
              |                    |
        AbstractItem           View
              |                    |
         +----+-----+         AllView
         |          |         ListView
        Job     AbstractProject  MyView
         |          |
     FreeStyleProject  ...
         |
        Build
         |
    AbstractBuild
         |
       Run
```

#### 핵심 클래스 목록

| 클래스 | 줄 수 | 역할 |
|--------|-------|------|
| `Queue.java` | 3,252 | 빌드 대기열 관리, 스케줄링, 부하 분산 |
| `Run.java` | 2,698 | 단일 빌드 실행 기록 (로그, 결과, 아티팩트) |
| `AbstractProject.java` | 2,163 | 빌드 가능한 프로젝트의 추상 기반 클래스 |
| `Computer.java` | 1,801 | 빌드 노드(에이전트)의 런타임 상태 |
| `Job.java` | 1,731 | 빌드 이력을 가진 모든 프로젝트의 기반 클래스 |
| `Descriptor.java` | 1,334 | 설정 가능한 객체의 메타데이터 및 팩토리 |
| `Executor.java` | 992 | 빌드 실행 스레드 관리 |
| `Fingerprint.java` | ~800 | 아티팩트 추적용 핑거프린트 |
| `Node.java` | ~600 | 빌드 실행 노드 추상 클래스 |
| `Action.java` | ~50 | 확장 포인트 인터페이스 (모든 곳에 부착 가능) |

#### 하위 패키지

```
hudson/model/
├── labels/          # Label, LabelAtom, LabelExpression — 노드 레이블 시스템
├── queue/           # QueueTaskFuture, WorkUnit — 큐 내부 구현
├── lazy/            # LazyBuildMixIn — 빌드 이력 지연 로딩
├── details/         # 모델 상세 정보
└── *.java           # 174개의 루트 레벨 클래스
```

### 3.3 hudson/security/ — 보안 프레임워크 (45+ 파일)

```
hudson/security/
├── SecurityRealm.java                    # 인증 제공자 추상 클래스
├── AuthorizationStrategy.java            # 인가 전략 추상 클래스
├── ACL.java                              # Access Control List
├── Permission.java                       # 권한 정의
├── PermissionGroup.java                  # 권한 그룹
├── PermissionScope.java                  # 권한 범위 (JENKINS, ITEM, RUN 등)
├── HudsonPrivateSecurityRealm.java       # 내장 사용자 DB 인증
├── FullControlOnceLoggedInAuthorizationStrategy.java  # 로그인 후 전체 권한
├── LegacyAuthorizationStrategy.java      # 레거시 인가
├── HudsonFilter.java                     # 서블릿 필터 체인
├── csrf/                                 # CSRF 방지
│   └── CrumbIssuer.java                  # Crumb 발급기
├── captcha/                              # CAPTCHA 관련
└── *.java                                # 인증 필터, 토큰 처리 등
```

**왜 Spring Security인가?**

Jenkins는 2.x부터 Spring Security(구 Acegi Security)를 인증/인가 프레임워크로 사용한다.
`SecurityRealm`이 Spring Security의 `AuthenticationManager`를 래핑하고,
`AuthorizationStrategy`가 ACL 기반 인가를 제공한다.
이 래핑 계층은 플러그인이 Spring Security의 내부 구현 변경에 영향받지 않도록 보호한다.

### 3.4 hudson/tasks/ — 빌드 스텝 (22개 파일)

```
hudson/tasks/
├── BuildStep.java                  # 빌드 단계 인터페이스
├── Builder.java                    # 빌드 실행 단계 추상 클래스
├── Publisher.java                  # 빌드 후 단계 추상 클래스
├── Recorder.java                   # Publisher 하위 — 기록 유형
├── Notifier.java                   # Publisher 하위 — 알림 유형
├── BuildStepDescriptor.java        # BuildStep의 Descriptor
├── BuildStepMonitor.java           # 빌드 잠금 모니터
├── BuildStepCompatibilityLayer.java # 호환성 레이어
├── BuildTrigger.java               # 하류 빌드 트리거
├── BuildWrapper.java               # 빌드 환경 래퍼
├── ArtifactArchiver.java           # 아티팩트 보관
├── Fingerprinter.java              # 아티팩트 핑거프린팅
├── Shell.java                      # 셸 스크립트 실행
├── BatchFile.java                  # Windows 배치 파일 실행
├── CommandInterpreter.java         # Shell/BatchFile 공통 추상 클래스
├── Maven.java                      # Maven 빌드 스텝
├── LogRotator.java                 # 빌드 이력 삭제 정책
└── _maven/                         # Maven 내부 구현
```

**확장점 계층 구조:**

```
BuildStep (인터페이스)
├── Builder (추상)           # 빌드 단계: 컴파일, 테스트 등
│   ├── Shell               # 유닉스 셸 실행
│   ├── BatchFile           # Windows 배치 실행
│   └── Maven               # Maven 실행
└── Publisher (추상)          # 빌드 후 단계
    ├── Recorder (추상)      # 결과 기록: 테스트 결과, 코드 커버리지
    │   ├── ArtifactArchiver
    │   └── Fingerprinter
    └── Notifier (추상)      # 결과 알림: 이메일, 슬랙 등
```

### 3.5 hudson/cli/ — CLI 명령 프레임워크 (67개 파일)

서버 측 CLI 명령을 정의하는 패키지이다 (5절의 CLI 클라이언트 모듈과 다름).

```
hudson/cli/
├── CLICommand.java                  # 모든 CLI 명령의 추상 기반 클래스
├── CLIAction.java                   # CLI 요청 라우팅 Action
├── BuildCommand.java                # jenkins-cli build 명령
├── CreateJobCommand.java            # jenkins-cli create-job 명령
├── DeleteJobCommand.java            # jenkins-cli delete-job 명령
├── CopyJobCommand.java              # jenkins-cli copy-job 명령
├── GetJobCommand.java               # jenkins-cli get-job 명령
├── UpdateJobCommand.java            # jenkins-cli update-job 명령
├── ListJobsCommand.java             # jenkins-cli list-jobs 명령
├── InstallPluginCommand.java        # jenkins-cli install-plugin 명령
├── ListPluginsCommand.java          # jenkins-cli list-plugins 명령
├── EnablePluginCommand.java         # 플러그인 활성화
├── DisablePluginCommand.java        # 플러그인 비활성화
├── ConnectNodeCommand.java          # 노드 연결
├── DisconnectNodeCommand.java       # 노드 연결 해제
├── OfflineNodeCommand.java          # 노드 오프라인 전환
├── OnlineNodeCommand.java           # 노드 온라인 전환
├── GroovyCommand.java               # Groovy 스크립트 실행
├── GroovyshCommand.java             # Groovy 셸
├── WhoAmICommand.java               # 현재 사용자 확인
├── ReloadConfigurationCommand.java  # 설정 리로드
├── declarative/                     # 선언적 CLI 지원
├── handlers/                        # CLI 핸들러
└── *.java                           # 기타 명령
```

**CLI 명령 등록 메커니즘:**

모든 CLI 명령은 `CLICommand`를 확장하고 `@Extension` 어노테이션을 붙인다.
Jenkins의 확장점 검색 메커니즘(SezPoz)이 자동으로 명령을 발견하고 등록한다.

```java
// 예시: hudson/cli/VersionCommand.java
@Extension
public class VersionCommand extends CLICommand {
    @Override
    public String getShortDescription() {
        return "Outputs the current version.";
    }

    @Override
    protected int run() throws Exception {
        stdout.println(Jenkins.VERSION);
        return 0;
    }
}
```

### 3.6 hudson/init/ — 초기화 프레임워크 (10개 파일)

```
hudson/init/
├── InitMilestone.java         # 초기화 마일스톤 열거형
├── Initializer.java           # @Initializer 어노테이션
├── InitializerFinder.java     # 초기화 메서드 탐색
├── InitReactorListener.java   # 초기화 이벤트 리스너
├── InitStrategy.java          # 초기화 전략 인터페이스
├── TaskMethodFinder.java      # 태스크 메서드 탐색
├── Terminator.java            # @Terminator 어노테이션
├── TerminatorFinder.java      # 종료 메서드 탐색
├── TermMilestone.java         # 종료 마일스톤
└── impl/                      # 내부 구현
```

**InitMilestone 순서:**

```
STARTED
    ↓
PLUGINS_LISTED          # 플러그인 목록 확인
    ↓
PLUGINS_PREPARED        # 플러그인 의존성 해결
    ↓
PLUGINS_STARTED         # 플러그인 시작
    ↓
EXTENSIONS_AUGMENTED    # ExtensionList 초기화
    ↓
SYSTEM_CONFIG_LOADED    # config.xml 로드
    ↓
SYSTEM_CONFIG_ADAPTED   # 설정 마이그레이션
    ↓
JOB_CONFIG_ADAPTED      # Job 설정 마이그레이션
    ↓
JOB_LOADED              # Job 로드 완료
    ↓
COMPLETED               # 초기화 완료 → 요청 수신 시작
```

### 3.7 hudson/util/ — 유틸리티 (121개 파일)

| 클래스 | 줄 수 | 역할 |
|--------|-------|------|
| `FormValidation.java` | 726 | 설정 폼 유효성 검증 프레임워크 |
| `XStream2.java` | 660 | XStream 커스터마이징 (config.xml 직렬화) |
| `ConsistentHash.java` | 376 | 일관된 해싱 (노드 선택) |
| `Secret.java` | 345 | 암호화된 비밀 값 관리 |
| `DescribableList.java` | 330 | Describable 객체의 설정 가능한 리스트 |
| `TextFile.java` | 195 | 텍스트 파일 I/O 유틸리티 |

기타 주요 클래스:
- `AtomicFileWriter` — 원자적 파일 쓰기 (설정 저장 시 데이터 손실 방지)
- `CopyOnWriteList` — 동시성 안전한 리스트 (플러그인 목록 등)
- `CopyOnWriteMap` — 동시성 안전한 맵
- `DaemonThreadFactory` — 데몬 스레드 팩토리
- `ConsistentHash` — 일관된 해싱 알고리즘
- `ProcessTree` — 프로세스 트리 관리
- `StreamTaskListener` — 스트림 기반 태스크 리스너

### 3.8 jenkins/model/ — 현대 모델 (109개 파일)

```
jenkins/model/
├── Jenkins.java                      # 5,990줄 — Jenkins 싱글턴 (전체 시스템의 루트 객체)
├── GlobalConfiguration.java          # 전역 설정 확장점
├── GlobalConfigurationCategory.java  # 설정 카테고리
├── Nodes.java                        # 노드 목록 관리
├── BuildDiscarder.java               # 빌드 삭제 전략
├── ArtifactManager.java              # 아티팩트 저장소 추상화
├── PeepholePermalink.java            # 빌드 퍼머링크 최적화
├── ParameterizedJobMixIn.java        # 매개변수화된 Job 믹스인
├── TransientActionFactory.java       # 동적 Action 생성
├── IdStrategy.java                   # 사용자/그룹 ID 비교 전략
├── identity/                         # Jenkins 인스턴스 ID
├── labels/                           # 레이블 관련
├── lazy/                             # 지연 로딩
├── queue/                            # 큐 관련
├── experimentalflags/                # 실험적 기능 플래그
├── item_category/                    # 아이템 카테고리
├── navigation/                       # 네비게이션
└── details/                          # 상세 정보
```

**Jenkins.java가 5,990줄인 이유:**

`Jenkins.java`는 전체 시스템의 루트 싱글턴으로, Stapler 프레임워크에서 URL의 시작점이다.
`/` URL이 `Jenkins` 객체에 매핑되고, 그 아래의 URL 경로가 메서드/프로퍼티로 라우팅된다.
이 클래스는 다음을 모두 담당한다:

- 전역 설정 로드/저장 (`config.xml`)
- 플러그인 매니저 초기화
- 노드(에이전트) 관리
- 보안 설정
- Job/View 계층 구조의 루트
- 시스템 초기화/종료 시퀀스
- URL 라우팅 진입점

이는 "God Object" 안티패턴에 해당하지만, Stapler의 URL-to-Object 매핑 설계상
루트 객체가 많은 책임을 가질 수밖에 없다. 점진적으로 기능을 `GlobalConfiguration`,
`@RootAction` 등으로 분리하는 리팩토링이 진행 중이다.

### 3.9 jenkins/security/ — 현대 보안 (103개 파일)

```
jenkins/security/
├── csrf/                           # CSRF 방지
│   └── CrumbIssuer.java           # Crumb 기반 CSRF 토큰
├── apitoken/                       # API 토큰 관리
│   ├── ApiTokenProperty.java      # 사용자별 API 토큰
│   └── ApiTokenStore.java         # 토큰 저장소
├── csp/                            # Content-Security-Policy
├── seed/                           # 사용자별 보안 시드
├── stapler/                        # Stapler 보안 관련
├── s2m/                            # Agent→Controller 통신 보안
├── ConfidentialKey.java            # 기밀 키 추상 클래스
├── ConfidentialStore.java          # 기밀 저장소
├── CryptoConfidentialKey.java      # 암호화 키
├── HMACConfidentialKey.java        # HMAC 키
├── RSAConfidentialKey.java         # RSA 키
├── ResourceDomainFilter.java       # 리소스 도메인 필터링
├── ClassFilterImpl.java            # 역직렬화 필터
├── FIPS140.java                    # FIPS 140 준수 모드
├── SecurityListener.java           # 보안 이벤트 리스너
├── QueueItemAuthenticator.java     # 큐 아이템별 인증
└── ChannelConfigurator.java        # 에이전트 채널 보안
```

### 3.10 루트 수준 핵심 클래스 (hudson/*.java, 50개)

`hudson` 패키지의 루트에는 플러그인 시스템, 확장점, 파일 시스템 관련 핵심 클래스가 있다.

| 클래스 | 줄 수 | 역할 |
|--------|-------|------|
| `FilePath.java` | 3,899 | 원격 파일 시스템 접근 (에이전트 투명 지원) |
| `Functions.java` | 2,721 | Jelly 템플릿용 유틸리티 함수 모음 |
| `PluginManager.java` | 2,697 | 플러그인 로드, 설치, 업데이트, 의존성 관리 |
| `Launcher.java` | 1,499 | 프로세스 실행기 (로컬/원격 투명 지원) |
| `PluginWrapper.java` | 1,440 | 개별 플러그인의 런타임 래퍼 |
| `ExtensionFinder.java` | 801 | @Extension 어노테이션 스캔 및 인스턴스 생성 |
| `ClassicPluginStrategy.java` | 699 | HPI/JPI 플러그인 로딩 전략 |
| `ProxyConfiguration.java` | 642 | HTTP 프록시 설정 |
| `ExtensionList.java` | 496 | 확장점 인스턴스의 동적 리스트 |
| `Extension.java` | ~80 | @Extension 어노테이션 정의 |
| `BulkChange.java` | ~100 | 대량 변경 시 save() 호출 최적화 |
| `EnvVars.java` | ~300 | 환경 변수 맵 (대소문자 처리 등) |

---

## 4. war 모듈 -- WAR 패키징 및 독립 실행

```
파일: jenkins/war/pom.xml
artifactId: jenkins-war
packaging: war
```

### 디렉토리 구조

```
war/
├── pom.xml
└── src/
    ├── main/
    │   ├── java/executable/
    │   │   └── Main.java          # 514줄 — java -jar jenkins.war 진입점
    │   ├── webapp/
    │   │   ├── WEB-INF/
    │   │   │   ├── web.xml        # 최소 서블릿 설정 (display-name과 description만)
    │   │   │   ├── hudson/        # 정적 리소스 (Jelly 접근용)
    │   │   │   ├── ibm-web-bnd.xmi    # IBM WebSphere 바인딩
    │   │   │   ├── jboss-web.xml      # JBoss 설정
    │   │   │   └── jboss-deployment-structure.xml  # JBoss 모듈 격리
    │   │   ├── css/               # CSS 파일
    │   │   ├── images/            # 아이콘, SVG 심볼
    │   │   ├── scripts/           # 레거시 JavaScript
    │   │   ├── favicon.ico        # 파비콘
    │   │   ├── favicon.svg        # SVG 파비콘
    │   │   └── robots.txt         # 검색엔진 크롤링 설정
    │   └── resources/
    │       └── images/symbols/    # SVG 아이콘 심볼 세트
    └── test/
        └── java/executable/
            └── MainTest.java      # Main 클래스 테스트
```

### Main.java — 독립 실행 진입점 (514줄)

`java -jar jenkins.war`로 실행할 때의 진입점이다.

```java
// war/src/main/java/executable/Main.java

public class Main {
    private static final NavigableSet<Integer> SUPPORTED_JAVA_VERSIONS =
        new TreeSet<>(List.of(21, 25));

    public static void main(String[] args) throws IllegalAccessException {
        // 1. Java 버전 검증
        verifyJavaVersion(Runtime.version().feature(), isFutureJavaEnabled(args));

        // 2. 인자 파싱 (--paramsFromStdIn, --version 등)

        // 3. WAR 파일에서 Winstone(Jetty) JAR 추출
        File tmpJar = extractFromJar("winstone.jar", "winstone", ".jar", ...);

        // 4. Winstone ClassLoader 구성 → 위임
        ClassLoader cl = new URLClassLoader(new URL[]{tmpJar.toURI().toURL()});
        Class<?> winstoneLauncher = cl.loadClass("winstone.Launcher");
        Method mainMethod = winstoneLauncher.getMethod("main", String[].class);
        mainMethod.invoke(null, new Object[]{arguments.toArray(new String[0])});
    }
}
```

**실행 흐름:**

```
java -jar jenkins.war
    ↓
executable.Main.main()
    ↓
Java 버전 검증 (21, 25 지원)
    ↓
WAR에서 winstone.jar 추출
    ↓
Winstone(내장 Jetty) 시작
    ↓
Jenkins WebApp 초기화 (WebAppMain.contextInitialized)
    ↓
Jenkins.java 싱글턴 생성 → InitMilestone 순서대로 초기화
```

### web.xml — 최소 설정

```xml
<!-- war/src/main/webapp/WEB-INF/web.xml -->
<web-app version="3.1">
  <display-name>Jenkins v${project.version}</display-name>
  <description>Build management system</description>
</web-app>
```

**왜 web.xml이 이렇게 비어 있는가?**

Jenkins는 서블릿 표준의 `web.xml` 대신 Stapler 프레임워크를 통해 URL 라우팅을 처리한다.
`WebAppMain`이 `ServletContextListener`로 등록되어 초기화를 담당하고,
필터와 서블릿은 프로그래밍 방식으로 등록된다.
이 설계 덕분에 플러그인이 동적으로 URL 핸들러를 추가할 수 있다.

---

## 5. cli 모듈 -- CLI 클라이언트

```
파일: jenkins/cli/pom.xml
artifactId: cli
```

Jenkins 서버에 원격으로 명령을 전송하는 클라이언트 라이브러리이다.
`java -jar jenkins-cli.jar` 형태로 독립 사용된다.

### 소스 구조

```
cli/src/main/java/
├── hudson/cli/
│   ├── CLI.java                     # 572줄 — CLI 클라이언트 메인 클래스
│   ├── CLIConnectionFactory.java    # 서버 연결 팩토리
│   ├── PlainCLIProtocol.java        # 평문 CLI 프로토콜 구현
│   ├── SSHCLI.java                  # SSH 기반 CLI 연결
│   ├── FullDuplexHttpStream.java    # HTTP 기반 전이중 스트림
│   ├── PrivateKeyProvider.java      # SSH 키 인증
│   ├── FlightRecorderInputStream.java # 디버깅용 입력 스트림 녹화
│   ├── HexDump.java                 # 16진수 덤프 유틸리티
│   ├── NoCheckTrustManager.java     # 인증서 검증 무시 (개발용)
│   └── DiagnosedStreamCorruptionException.java  # 스트림 손상 진단
└── hudson/util/
    └── QuotedStringTokenizer.java   # 인용 문자열 토큰화
```

**CLI 통신 프로토콜:**

```
┌──────────────┐         ┌──────────────────┐
│  CLI Client  │         │  Jenkins Server   │
│  (cli모듈)   │         │  (core 모듈)      │
├──────────────┤         ├──────────────────┤
│  CLI.java    │◄──────►│  CLIAction.java   │
│              │  HTTP   │  CLICommand.java  │
│  SSHCLI.java │◄──────►│  SSHD (플러그인)   │
│              │  SSH    │                   │
│  WebSocket   │◄──────►│  WebSocket        │
└──────────────┘         └──────────────────┘
```

세 가지 전송 방식이 지원된다:
1. **WebSocket** (기본, 권장) — 양방향 스트리밍
2. **HTTP** — 폴링 기반 (`FullDuplexHttpStream`)
3. **SSH** — sshd-module 플러그인 필요

---

## 6. test 모듈 -- 통합 테스트

```
파일: jenkins/test/pom.xml
artifactId: jenkins-test
```

### 구조

```
test/src/test/
├── java/
│   ├── hudson/              # hudson.* 패키지 테스트
│   │   ├── model/           # 모델 클래스 테스트
│   │   ├── tasks/           # 빌드 스텝 테스트
│   │   ├── tools/           # 도구 설치 테스트
│   │   ├── security/        # 보안 테스트
│   │   ├── util/            # 유틸리티 테스트
│   │   ├── cli/             # CLI 명령 테스트
│   │   ├── init/            # 초기화 테스트
│   │   └── *.java           # 루트 레벨 테스트
│   ├── jenkins/             # jenkins.* 패키지 테스트
│   ├── lib/                 # UI 라이브러리 테스트
│   ├── org/                 # 조직별 테스트
│   ├── scripts/             # 스크립트 테스트
│   └── test/                # 공통 테스트 유틸리티
└── resources/
    ├── hudson/              # 테스트 리소스
    ├── jenkins/             # 테스트 리소스
    ├── plugins/             # 테스트용 플러그인
    └── scripts/             # 테스트 스크립트
```

**테스트 프레임워크:**

Jenkins 통합 테스트는 `jenkins-test-harness` 라이브러리의 `JenkinsRule`을 사용한다.
`JenkinsRule`은 각 테스트 메서드마다 임시 Jenkins 인스턴스를 시작/종료한다.

```java
// 테스트 예시 패턴
@Rule
public JenkinsRule j = new JenkinsRule();

@Test
public void testFreestyleProject() throws Exception {
    FreeStyleProject p = j.createFreeStyleProject();
    p.getBuildersList().add(new Shell("echo hello"));
    FreeStyleBuild b = j.buildAndAssertSuccess(p);
    j.assertLogContains("hello", b);
}
```

---

## 7. websocket 모듈 -- WebSocket SPI/구현

### websocket/spi — Service Provider Interface

```
파일: jenkins/websocket/spi/pom.xml
artifactId: websocket-spi
```

```java
// websocket/spi/src/main/java/jenkins/websocket/Provider.java
package jenkins.websocket;

// 서블릿 컨테이너의 WebSocket 지원을 추상화하는 SPI
// core 모듈이 이 인터페이스에만 의존하고, 구현체는 런타임에 결정
```

### websocket/jetty12-ee9 — Jetty 12 구현

```
파일: jenkins/websocket/jetty12-ee9/pom.xml
artifactId: websocket-jetty12-ee9
```

```java
// websocket/jetty12-ee9/src/main/java/jenkins/websocket/Jetty12EE9Provider.java
package jenkins.websocket;

// Jetty 12 EE9 환경에서의 WebSocket Provider 구현
// Winstone(내장 Jetty)와 함께 사용
```

**왜 SPI 패턴인가?**

Jenkins는 다양한 서블릿 컨테이너(Jetty, Tomcat, JBoss 등)에서 실행될 수 있다.
WebSocket 구현은 컨테이너마다 다르므로, SPI로 추상화하여 core 모듈이
특정 컨테이너에 종속되지 않도록 한다.

---

## 8. bom 모듈 -- 의존성 버전 관리

```
파일: jenkins/bom/pom.xml
artifactId: jenkins-bom
packaging: pom
```

Jenkins 코어와 플러그인이 공유하는 모든 라이브러리의 버전을 중앙에서 관리한다.

### 주요 관리 버전

| 의존성 | 버전 | 용도 |
|--------|------|------|
| Guice BOM | 6.0.0 | 의존성 주입 |
| SLF4J BOM | 2.0.17 | 로깅 파사드 |
| Spring Framework BOM | 6.2.16 | 스프링 코어 |
| Spring Security BOM | 6.5.8 | 인증/인가 |
| args4j | 2.37 | CLI 인자 파싱 |
| Guava | 33.5.0-jre | 유틸리티 컬렉션 |
| XStream | 1.4.21 | XML 직렬화 |
| Groovy | 2.4.21 | 스크립트 콘솔 |
| Stapler | 2076.v1b_a_c12445eb_e | URL-to-Object 웹 프레임워크 |
| Jelly | 1.1-jenkins-20250731 | XML 템플릿 엔진 |
| SezPoz | 1.13 | @Extension 인덱싱 |
| Remoting | 3355.v388858a_47b_33 | 에이전트 통신 |
| JNA | 5.18.1 | Native 라이브러리 접근 |
| Ant | 1.10.15 | 파일 패턴/글로빙 |
| JFreeChart | 1.0.19 | 차트 생성 |
| ANTLR4 | 4.13.2 | 문법 파서 (cron 표현식 등) |

**왜 BOM을 별도 모듈로 분리하는가?**

플러그인 개발자가 자신의 `pom.xml`에서 `jenkins-bom`을 import하면:

```xml
<dependencyManagement>
    <dependencies>
        <dependency>
            <groupId>org.jenkins-ci.main</groupId>
            <artifactId>jenkins-bom</artifactId>
            <version>2.553-SNAPSHOT</version>
            <type>pom</type>
            <scope>import</scope>
        </dependency>
    </dependencies>
</dependencyManagement>
```

Jenkins 코어와 동일한 버전의 라이브러리를 자동으로 사용하게 된다.
이것이 2,000개 이상의 플러그인 생태계에서 "클래스패스 지옥"을 방지하는 핵심 메커니즘이다.

---

## 9. coverage 모듈 -- 코드 커버리지 집계

```
파일: jenkins/coverage/pom.xml
artifactId: jenkins-coverage
packaging: pom
```

`core`, `cli`, `test` 모듈의 JaCoCo 커버리지 보고서를 하나로 집계하는 유틸리티 모듈이다.
소스 코드가 없고, Maven 의존성 설정만 포함한다.

---

## 10. 프론트엔드 빌드 시스템

Jenkins의 UI는 서버 측 Jelly 템플릿과 클라이언트 측 JavaScript/CSS로 구성된다.

### 디렉토리 구조

```
jenkins/
├── src/main/js/                   # JavaScript 소스 (80개 파일)
│   ├── pluginSetupWizard.js       # 초기 설정 마법사 UI
│   ├── plugin-manager-ui.js       # 플러그인 관리 UI
│   ├── add-item.js                # 새 아이템 생성 UI
│   ├── pages/                     # 페이지별 JS
│   │   ├── computer-set/          # 컴퓨터(노드) 관리 페이지
│   │   ├── dashboard/             # 대시보드 페이지
│   │   └── manage-jenkins/        # 관리 페이지
│   ├── util/                      # 유틸리티
│   │   ├── jenkins.js             # Jenkins 전역 유틸리티
│   │   ├── security.js            # 보안 관련 유틸리티
│   │   ├── dom.js                 # DOM 조작
│   │   ├── page.js                # 페이지 유틸리티
│   │   └── i18n.js                # 국제화
│   └── handlebars-helpers/        # Handlebars 헬퍼
├── src/main/scss/                 # SCSS 소스 (74개 파일)
├── webpack.config.js              # Webpack 번들링 설정
├── package.json                   # npm 의존성 (jenkins-ui)
├── eslint.config.cjs              # ESLint 코드 스타일
├── postcss.config.js              # PostCSS 처리
└── yarn.lock                      # Yarn 잠금 파일
```

### Webpack 설정

```javascript
// webpack.config.js (요약)
module.exports = {
  entry: {
    pluginSetupWizard: ["src/main/js/pluginSetupWizard.js", "...scss"],
    "plugin-manager-ui": ["src/main/js/plugin-manager-ui.js"],
    "add-item": ["src/main/js/add-item.js", "...scss"],
    "pages/computer-set": ["src/main/js/pages/computer-set"],
    "pages/dashboard": ["src/main/js/pages/dashboard"],
    "pages/manage-jenkins/system-information": ["..."],
  }
};
```

### npm 스크립트

```json
{
  "scripts": {
    "dev": "webpack --config webpack.config.js",
    "prod": "webpack --config webpack.config.js --mode=production",
    "build": "yarn prod",
    "start": "yarn dev --watch",
    "lint:js": "eslint . && prettier --check .",
    "lint:css": "stylelint src/main/scss",
    "lint:fix": "eslint --fix . && prettier --write . && stylelint ... --fix"
  }
}
```

---

## 11. 빌드 시스템 상세

### 11.1 Maven 빌드 파이프라인

```
validate  → checkstyle, spotless(코드 포매팅)
compile   → Java 컴파일, ANTLR4 파서 생성, Localizer 메시지 생성
                bridge-method-injector, access-modifier-checker
process   → license-maven-plugin (라이선스 정보)
test      → JUnit 5 + jenkins-test-harness
package   → WAR 패키징 (maven-war-plugin)
verify    → SpotBugs 정적 분석, JaCoCo 커버리지
install   → 로컬 저장소 설치
deploy    → Jenkins Maven 저장소 배포
```

### 11.2 핵심 Maven 플러그인

| 플러그인 | 역할 |
|----------|------|
| `maven-compiler-plugin` | Java 컴파일 (JDK 21+) |
| `maven-war-plugin` | WAR 파일 생성 |
| `maven-hpi-plugin` | Jenkins 플러그인 전용 빌드 도구 |
| `access-modifier-checker` | `@Restricted` 어노테이션 기반 API 가시성 강제 |
| `bridge-method-injector` | Java 이진 호환성 유지용 브릿지 메서드 자동 생성 |
| `spotbugs-maven-plugin` | 정적 분석 (버그 패턴 탐지) |
| `jacoco-maven-plugin` | 코드 커버리지 측정 (v0.8.14) |
| `maven-checkstyle-plugin` | 코드 스타일 검증 (Checkstyle 13.2.0) |
| `spotless-maven-plugin` | 코드 포매팅 (import 정렬, 들여쓰기, 줄바꿈) |
| `localizer-maven-plugin` | `Messages.properties`에서 타입 안전 메시지 클래스 생성 |
| `antlr4-maven-plugin` | ANTLR4 문법에서 파서 코드 생성 (cron 표현식 등) |
| `maven-enforcer-plugin` | 금지된 의존성 차단 (Jackson, BouncyCastle 등) |
| `maven-jarsigner-plugin` | JAR 서명 (릴리스 시) |
| `license-maven-plugin` | 라이선스 정보 생성 |
| `build-helper-maven-plugin` | 생성된 소스 디렉토리 등록 |

### 11.3 access-modifier-checker — API 가시성 강제

Jenkins는 `@Restricted` 어노테이션으로 API 가시성을 관리한다.
Java의 `public` 접근 제어자는 플러그인 호환성 때문에 변경할 수 없으므로,
`@Restricted`로 "public이지만 외부 사용 금지"를 표현한다.

```java
@Restricted(NoExternalUse.class)   // 코어 내부 전용
public void internalMethod() { ... }

@Restricted(Beta.class)            // 베타 API — 변경 가능
public void betaFeature() { ... }

@Restricted(DoNotUse.class)        // 사용 금지 — 향후 제거 예정
@Deprecated
public void legacyMethod() { ... }
```

빌드 시 `access-modifier-checker` 플러그인이 이 규칙을 컴파일 타임에 검증한다.

### 11.4 bridge-method-injector — 이진 호환성

Java 제네릭의 타입 이레이저(type erasure)로 인해, 메서드 시그니처를 변경하면
기존에 컴파일된 플러그인이 `NoSuchMethodError`로 실패할 수 있다.

```java
// 원래 메서드
public AbstractProject getProject() { ... }

// 변경 후 — 반환 타입을 좁힘
@WithBridgeMethods(AbstractProject.class)
public FreeStyleProject getProject() { ... }

// bridge-method-injector가 자동으로 브릿지 메서드 생성:
// public AbstractProject getProject() { return this.getProject(); }
```

이 도구 덕분에 코어 개발자는 API를 진화시키면서도 기존 플러그인의 이진 호환성을 유지할 수 있다.

### 11.5 금지된 의존성 (Banned Dependencies)

`maven-enforcer-plugin`이 코어에 포함되면 안 되는 라이브러리를 차단한다.
이들은 별도의 "라이브러리 플러그인"으로 제공되어, 버전 충돌 없이 플러그인 간 공유된다.

```
차단 목록 (일부):
- com.fasterxml.jackson.*      → jackson2-api 플러그인
- org.apache.httpcomponents     → apache-httpcomponents-client-4-api 플러그인
- org.bouncycastle              → bouncycastle-api 플러그인
- org.yaml:snakeyaml            → snakeyaml-api 플러그인
- org.json:json                 → json-api 플러그인
- org.apache.commons:commons-lang3 → commons-lang3-api 플러그인
- org.ow2.asm                   → asm-api 플러그인
```

### 11.6 CI 파이프라인 (Jenkinsfile)

```groovy
// Jenkinsfile (요약)
def axes = [
  platforms: ['linux', 'windows'],
  jdks: [21, 25],
]

// 매트릭스 빌드: Linux/Windows x JDK 21/25
// Launchable 통합으로 테스트 선택 최적화
```

---

## 12. 핵심 외부 의존성

### 12.1 웹 프레임워크 계층

| 라이브러리 | 버전 | 용도 |
|-----------|------|------|
| **Stapler** | 2076.v1b_a_c12445eb_e | URL-to-Object 웹 프레임워크 |
| **Jelly** | 1.1-jenkins-20250731 | XML 기반 뷰 템플릿 엔진 |
| **Winstone** | 8.1033.v23d2f156e821 | 내장 Jetty 서블릿 컨테이너 |
| Jakarta Servlet API | 5.0.0 | 서블릿 표준 |

**Stapler의 핵심 역할:**

Stapler는 Jenkins의 독특한 웹 프레임워크로, URL 경로를 객체 그래프에 직접 매핑한다.

```
URL: /job/my-project/42/console

매핑 과정:
  Jenkins.getItem("my-project")     → Job 객체
      .getBuildByNumber(42)         → Run 객체
          .doConsole(req, rsp)      → 콘솔 출력 핸들러
```

이 설계 덕분에 플러그인이 새로운 URL 핸들러를 추가하려면
`@Extension`으로 `RootAction`이나 `Action`을 구현하기만 하면 된다.
`web.xml` 수정이나 URL 매핑 등록이 불필요하다.

### 12.2 직렬화/영속화

| 라이브러리 | 버전 | 용도 |
|-----------|------|------|
| **XStream** | 1.4.21 | Java 객체 ↔ XML 변환 (config.xml 등) |
| txw2 | 20110809 | XML 쓰기 |
| jaxen | 2.0.0 | XPath 처리 |

**XStream2 — Jenkins의 영속화 엔진:**

Jenkins는 모든 설정을 XML 파일(`config.xml`)로 저장한다.
`XStream2`는 XStream을 커스터마이징한 것으로, 다음을 추가 지원한다:

- 클래스 리네이밍 호환성 (`hudson.` → `jenkins.` 전환)
- 필드 추가/제거 시 자동 마이그레이션
- `Secret` 필드 자동 암호화/복호화
- 플러그인 ClassLoader와의 통합

### 12.3 보안

| 라이브러리 | 버전 | 용도 |
|-----------|------|------|
| **Spring Security** | 6.5.8 | 인증/인가 프레임워크 |
| Spring Framework | 6.2.16 | Spring Security 기반 인프라 |

### 12.4 의존성 주입

| 라이브러리 | 버전 | 용도 |
|-----------|------|------|
| **Guice** | 6.0.0 | 의존성 주입 (ExtensionFinder에서 사용) |
| **SezPoz** | 1.13 | @Extension 어노테이션 인덱싱 |
| annotation-indexer | 1.213 | 어노테이션 스캔 |

**확장점 검색 메커니즘:**

```
컴파일 타임:
  SezPoz → @Extension 어노테이션 스캔 → META-INF/annotations/ 인덱스 생성

런타임:
  ExtensionFinder
      → GuiceFinder (Guice 기반)  ← 기본 확장점 검색
      → SezPozFinder (레거시)     ← 호환성 유지
      → 검색 결과 → ExtensionList에 등록
```

### 12.5 스크립팅/파싱

| 라이브러리 | 버전 | 용도 |
|-----------|------|------|
| **Groovy** | 2.4.21 | 스크립트 콘솔, 파이프라인 DSL |
| **ANTLR4** | 4.13.2 | cron 표현식 파서 등 |
| **args4j** | 2.37 | CLI 인자 파싱 |

### 12.6 통신

| 라이브러리 | 버전 | 용도 |
|-----------|------|------|
| **Remoting** | 3355.v388858a_47b_33 | Controller↔Agent 통신 (Channel 기반) |
| JNA | 5.18.1 | Native 라이브러리 접근 (프로세스 관리) |
| winp | - | Windows 프로세스 관리 |

### 12.7 유틸리티

| 라이브러리 | 버전 | 용도 |
|-----------|------|------|
| **Guava** | 33.5.0-jre | 컬렉션, 캐싱, 함수형 유틸리티 |
| commons-io | 2.21.0 | 파일 I/O |
| commons-codec | 1.21.0 | 인코딩/디코딩 |
| commons-beanutils | 1.11.0 | Java Bean 리플렉션 |
| commons-lang | 2.6 | 문자열/객체 유틸리티 |
| commons-collections | 3.2.2 | 컬렉션 유틸리티 |
| JFreeChart | 1.0.19 | 빌드 트렌드 차트 생성 |
| SLF4J | 2.0.17 | 로깅 파사드 |

---

## 13. 주요 파일 크기 분석

### 13.1 가장 큰 소스 파일 Top 15

| 순위 | 파일 | 줄 수 | 위치 |
|------|------|-------|------|
| 1 | `Jenkins.java` | 5,990 | `jenkins/model/` |
| 2 | `FilePath.java` | 3,899 | `hudson/` |
| 3 | `Queue.java` | 3,252 | `hudson/model/` |
| 4 | `Functions.java` | 2,721 | `hudson/` |
| 5 | `Run.java` | 2,698 | `hudson/model/` |
| 6 | `PluginManager.java` | 2,697 | `hudson/` |
| 7 | `AbstractProject.java` | 2,163 | `hudson/model/` |
| 8 | `Computer.java` | 1,801 | `hudson/model/` |
| 9 | `Job.java` | 1,731 | `hudson/model/` |
| 10 | `Launcher.java` | 1,499 | `hudson/` |
| 11 | `PluginWrapper.java` | 1,440 | `hudson/` |
| 12 | `Descriptor.java` | 1,334 | `hudson/model/` |
| 13 | `Executor.java` | 992 | `hudson/model/` |
| 14 | `ExtensionFinder.java` | 801 | `hudson/` |
| 15 | `FormValidation.java` | 726 | `hudson/util/` |

### 13.2 파일 크기가 큰 이유 분석

```
5,000줄+  │ Jenkins.java
          │   → 시스템 루트 싱글턴, Stapler URL 루트
          │   → 모든 전역 기능의 진입점
          │
3,000줄+  │ FilePath.java, Queue.java
          │   → 로컬/원격 투명한 파일 조작
          │   → 복잡한 스케줄링 알고리즘
          │
2,000줄+  │ Functions.java, Run.java, PluginManager.java
          │   → Jelly 템플릿 헬퍼 함수 모음
          │   → 빌드 라이프사이클 관리
          │   → 플러그인 설치/업데이트/의존성
          │
1,000줄+  │ Computer, Job, Launcher, PluginWrapper, Descriptor
          │   → 핵심 도메인 객체들
          │   → 20년간의 기능 축적
```

**왜 이렇게 큰 파일이 많은가?**

1. **이진 호환성 제약** — 수천 개의 플러그인이 이 클래스들에 직접 의존하므로
   메서드를 다른 클래스로 이동하면 `NoSuchMethodError`가 발생한다.
2. **Stapler URL 매핑** — URL 경로가 객체의 메서드에 매핑되므로
   한 객체에 많은 웹 핸들러가 집중된다.
3. **20년 역사** — Hudson(2004)에서 시작하여 지속적으로 기능이 추가되었다.
4. **호환성 우선 문화** — 리팩토링보다 호환성 유지가 항상 우선이다.

---

## 14. 패키지 명명 규칙과 이중 패키지 구조

### 14.1 두 패키지가 공존하는 이유

```
hudson.*   → Hudson 시절(2004~2011)부터의 레거시 패키지
              746개 Java 파일 (전체의 60%)

jenkins.*  → Jenkins 리브랜딩(2011~) 이후의 현대 패키지
              439개 Java 파일 (전체의 36%)
```

2011년에 "Hudson"이 "Jenkins"로 리브랜딩되었지만,
이미 수백 개의 플러그인이 `hudson.*` 패키지의 클래스를 직접 참조하고 있었다.
패키지명을 일괄 변경하면 모든 플러그인이 깨지므로, 새로운 코드는 `jenkins.*`에 작성하되
기존 `hudson.*`는 그대로 유지하는 전략을 택했다.

### 14.2 패키지 간 관계

```
hudson.model.Job                     ← 레거시 (유지)
    ↑ 상속
jenkins.model.ParameterizedJobMixIn  ← 현대 확장

hudson.security.SecurityRealm        ← 레거시 (유지)
    ↑ 사용
jenkins.security.SecurityListener    ← 현대 이벤트 시스템

hudson.model.Jenkins (X)             ← 리브랜딩 시 이동됨
jenkins.model.Jenkins                ← 현재 위치
```

### 14.3 새 코드 작성 시 규칙

| 상황 | 패키지 선택 |
|------|------------|
| 완전히 새로운 기능 | `jenkins.*` |
| 기존 클래스의 하위 클래스 | 부모와 같은 패키지 |
| 기존 API의 현대적 대안 | `jenkins.*` (기존은 `@Deprecated`) |
| 버그 수정 | 기존 패키지 유지 |
| 내부 구현 변경 | 기존 패키지 유지 |

---

## 15. 리소스 구조 -- Jelly 템플릿과 i18n

### 15.1 Jelly 템플릿 (725개 파일)

Jenkins의 UI는 주로 Jelly XML 템플릿으로 렌더링된다.

```
core/src/main/resources/
├── hudson/
│   ├── model/
│   │   ├── AbstractProject/
│   │   │   ├── configure.jelly       # 프로젝트 설정 폼
│   │   │   ├── sidepanel.jelly       # 사이드 패널
│   │   │   └── index.jelly           # 메인 페이지
│   │   ├── Run/
│   │   │   ├── console.jelly         # 콘솔 출력 뷰
│   │   │   └── configure.jelly       # 빌드 설정
│   │   ├── View/
│   │   │   └── main.jelly            # 뷰 메인 페이지
│   │   └── Queue/
│   │       └── items.jelly           # 큐 아이템 목록
│   ├── tasks/
│   │   ├── ArtifactArchiver/
│   │   │   └── config.jelly          # 아티팩트 보관 설정
│   │   └── Shell/
│   │       └── global.jelly          # 셸 전역 설정
│   ├── security/                     # 보안 설정 화면
│   ├── slaves/                       # 에이전트 설정 화면
│   ├── PluginManager/                # 플러그인 관리 화면
│   └── PluginWrapper/                # 개별 플러그인 정보
├── jenkins/
│   ├── model/Jenkins/                # Jenkins 메인 설정
│   ├── security/                     # 보안 현대 화면
│   └── install/SetupWizard/          # 초기 설정 마법사
└── lib/                              # 공통 UI 컴포넌트 (태그 라이브러리)
```

**Jelly 템플릿 컨벤션:**

```
{ClassName}/
├── config.jelly        # 설정 폼 (Descriptor.configure에 대응)
├── global.jelly        # 전역 설정 폼
├── index.jelly         # 기본 뷰 페이지
├── sidepanel.jelly     # 사이드바 메뉴
├── help.jelly          # 도움말 텍스트
├── help-{field}.html   # 필드별 인라인 도움말
└── message.jelly       # AdminMonitor 메시지
```

Stapler는 Java 클래스의 패키지 경로와 리소스 디렉토리 경로를 자동으로 매칭한다.
예를 들어 `hudson.tasks.Shell` 클래스의 `global.jelly`는
`core/src/main/resources/hudson/tasks/Shell/global.jelly`에 위치한다.

### 15.2 국제화 (i18n) — Properties 파일 (7,465개)

```
core/src/main/resources/hudson/model/Run/
├── Messages.properties               # 영어 (기본)
├── Messages_ja.properties            # 일본어
├── Messages_de.properties            # 독일어
├── Messages_fr.properties            # 프랑스어
├── Messages_zh_CN.properties         # 중국어 간체
├── Messages_zh_TW.properties         # 중국어 번체
├── Messages_ko.properties            # 한국어
├── Messages_pt_BR.properties         # 브라질 포르투갈어
└── Messages_ru.properties            # 러시아어
```

**Localizer Maven 플러그인:**

`localizer-maven-plugin`은 `Messages.properties` 파일에서
타입 안전한 Java 클래스를 자동 생성한다.

```properties
# Messages.properties
Run.Summary.BrokenSinceRun=Broken since {0}
```

```java
// 자동 생성된 Messages.java
public class Messages {
    public static String Run_Summary_BrokenSinceRun(Object arg0) {
        return holder.format("Run.Summary.BrokenSinceRun", arg0);
    }
}
```

---

## 부록: 전체 아키텍처 맵

```
+------------------------------------------------------------------+
|                        jenkins.war                                |
|                                                                    |
|  ┌─────────────────────────────────────────────────────────────┐  |
|  │  executable.Main                                             │  |
|  │  → Java 버전 검증 → Winstone(Jetty) 추출 → 위임             │  |
|  └─────────────────────────────────────────────────────────────┘  |
|                              ↓                                     |
|  ┌─────────────────────────────────────────────────────────────┐  |
|  │  Winstone (내장 Jetty 서블릿 컨테이너)                       │  |
|  │  → WebAppMain.contextInitialized()                           │  |
|  └─────────────────────────────────────────────────────────────┘  |
|                              ↓                                     |
|  ┌─────────────────────────────────────────────────────────────┐  |
|  │  jenkins-core                                                │  |
|  │                                                               │  |
|  │  jenkins.model.Jenkins (싱글턴, 5,990줄)                     │  |
|  │      ↕ Stapler URL 라우팅                                    │  |
|  │  hudson.model.* (도메인 모델: Job, Run, Queue, Computer)     │  |
|  │      ↕ XStream2 직렬화                                       │  |
|  │  JENKINS_HOME/config.xml, jobs/*/config.xml                  │  |
|  │                                                               │  |
|  │  hudson.PluginManager                                         │  |
|  │      ↕ ClassLoader 격리                                      │  |
|  │  plugins/*.hpi (2,000+ 확장 가능)                            │  |
|  │                                                               │  |
|  │  hudson.security.* + jenkins.security.*                      │  |
|  │      ↕ Spring Security                                       │  |
|  │  인증/인가/CSRF 방지                                         │  |
|  │                                                               │  |
|  │  hudson.slaves.* + Remoting                                  │  |
|  │      ↕ Channel (TCP/WebSocket)                               │  |
|  │  빌드 에이전트 통신                                           │  |
|  └─────────────────────────────────────────────────────────────┘  |
|                                                                    |
|  ┌────────────┐  ┌──────────────┐  ┌───────────────────────┐     |
|  │  cli 모듈   │  │ websocket-spi │  │ websocket-jetty12-ee9 │     |
|  │  (클라이언트)│  │  (인터페이스)  │  │  (구현체)              │     |
|  └────────────┘  └──────────────┘  └───────────────────────┘     |
+------------------------------------------------------------------+
```

---

## 요약

Jenkins의 코드 구조는 다음과 같은 특징을 가진다:

1. **Maven 멀티모듈 구조** — `bom`, `core`, `war`, `cli`, `test`, `websocket/*`, `coverage`의
   7개 모듈이 명확한 책임을 분담한다.

2. **이중 패키지 구조 (`hudson.*` + `jenkins.*`)** — 2004년부터의 레거시와 2011년 이후의 현대 코드가
   이진 호환성을 위해 공존한다. 새로운 기능은 `jenkins.*`에, 기존 API는 `hudson.*`에 유지된다.

3. **확장점 기반 아키텍처** — `@Extension` + SezPoz/Guice로 플러그인이 코어의 모든 부분을
   동적으로 확장할 수 있다. `CLICommand`, `Builder`, `Publisher`, `SecurityRealm` 등
   수십 개의 확장점이 존재한다.

4. **Stapler URL-to-Object 매핑** — 전통적인 서블릿 매핑 대신 객체 그래프에 URL을 직접 매핑한다.
   이것이 `web.xml`이 비어 있고 `Jenkins.java`가 5,990줄인 이유이다.

5. **BOM 기반 의존성 관리** — 2,000개 이상의 플러그인 생태계와의 라이브러리 버전 일관성을
   `jenkins-bom` 모듈이 보장한다.

6. **이진 호환성 도구** — `bridge-method-injector`와 `access-modifier-checker`가
   API 진화와 하위 호환성을 동시에 가능하게 한다.

7. **복합 빌드 시스템** — 서버 측은 Maven + Java, 클라이언트 측은 Webpack + JavaScript/SCSS로
   독립적으로 빌드된다.
