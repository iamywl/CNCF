# 07. Jenkins 싱글턴 — Jenkins.java 심층 분석

## 목차

1. [개요](#1-개요)
2. [클래스 선언과 상속 계층](#2-클래스-선언과-상속-계층)
3. [핵심 필드 분석](#3-핵심-필드-분석)
4. [싱글턴 패턴](#4-싱글턴-패턴)
5. [초기화 흐름](#5-초기화-흐름)
6. [InitMilestone과 Reactor 기반 초기화](#6-initmilestone과-reactor-기반-초기화)
7. [설정 저장과 로드](#7-설정-저장과-로드)
8. [Stapler 루트 역할](#8-stapler-루트-역할)
9. [Item 관리](#9-item-관리)
10. [보안 통합](#10-보안-통합)
11. [권한 모델](#11-권한-모델)
12. [Extension 시스템 통합](#12-extension-시스템-통합)
13. [종료(cleanUp) 흐름](#13-종료cleanup-흐름)
14. [설정 리로드](#14-설정-리로드)
15. [설계 분석 — 왜 이렇게 만들었는가](#15-설계-분석--왜-이렇게-만들었는가)

---

## 1. 개요

`Jenkins.java`는 Jenkins 시스템 전체의 **루트 객체(Root Object)**이다. 약 5,990줄에 달하는 이 단일 클래스가 Jenkins의 핵심을 구성한다.

```
소스 경로: core/src/main/java/jenkins/model/Jenkins.java
```

이 클래스가 담당하는 역할을 요약하면 다음과 같다:

| 역할 | 설명 |
|------|------|
| 싱글턴 | JVM 내에서 유일한 Jenkins 인스턴스 |
| 웹 루트 | Stapler 프레임워크의 URL 매핑 진입점 |
| Item 컨테이너 | 모든 Job/프로젝트를 관리하는 최상위 ItemGroup |
| 보안 허브 | SecurityRealm, AuthorizationStrategy 통합 |
| 플러그인 호스트 | PluginManager, ExtensionList 관리 |
| 설정 저장소 | config.xml 직렬화/역직렬화 |
| 노드 관리자 | 빌트인 노드 + 에이전트 노드 관리 |
| 빌드 큐 | Queue 인스턴스 소유 |

Javadoc의 첫 줄이 이 클래스의 본질을 정확히 설명한다:

```java
/**
 * Root object of the system.
 *
 * @author Kohsuke Kawaguchi
 */
```

---

## 2. 클래스 선언과 상속 계층

### 2.1 클래스 시그니처

```java
// core/src/main/java/jenkins/model/Jenkins.java:354-357
@ExportedBean
public class Jenkins extends AbstractCIBase implements DirectlyModifiableTopLevelItemGroup,
        StaplerProxy, StaplerFallback,
        ModifiableViewGroup, AccessControlled, DescriptorByNameOwner,
        ModelObjectWithContextMenu, ModelObjectWithChildren, OnMaster, Loadable {
```

### 2.2 상속 계층도

```
java.lang.Object
  └── hudson.model.Node                    // 노드(실행 환경) 기본 클래스
       └── hudson.model.AbstractCIBase     // CI 시스템 공통 기능
            └── jenkins.model.Jenkins      // 실제 구현
```

### 2.3 구현 인터페이스 분석

Jenkins 클래스는 10개의 인터페이스를 구현한다. 각 인터페이스가 부여하는 역할은 다음과 같다:

```
+-----------------------------------+----------------------------------------------+
| 인터페이스                          | 역할                                          |
+-----------------------------------+----------------------------------------------+
| DirectlyModifiableTopLevelItemGroup| Job 추가/삭제가 가능한 최상위 ItemGroup         |
| StaplerProxy                       | 요청 전에 getTarget()으로 접근 제어            |
| StaplerFallback                    | URL 매핑 실패 시 대체 객체(View) 반환          |
| ModifiableViewGroup                | View 추가/삭제가 가능한 그룹                   |
| AccessControlled                   | ACL 기반 권한 확인                             |
| DescriptorByNameOwner              | 이름으로 Descriptor 검색                       |
| ModelObjectWithContextMenu         | 컨텍스트 메뉴 제공                             |
| ModelObjectWithChildren            | 자식 객체의 컨텍스트 메뉴                      |
| OnMaster                           | 컨트롤러 노드에서만 실행되는 마커               |
| Loadable                           | load() 메서드로 설정 로딩 가능                  |
+-----------------------------------+----------------------------------------------+
```

`@ExportedBean` 어노테이션은 Stapler의 JSON/XML export 기능을 활성화한다. `@Exported`가 붙은 필드/메서드가 REST API를 통해 노출된다.

### 2.4 AbstractCIBase의 기여

`AbstractCIBase`는 `Node`를 상속하며, Jenkins를 "하나의 노드"이자 "CI 시스템 전체"로 동시에 작동하게 한다. Jenkins 빌트인 노드(built-in node)가 곧 Jenkins 인스턴스 자체인 이유가 여기에 있다.

---

## 3. 핵심 필드 분석

### 3.1 빌드 큐

```java
// 358행
private final transient Queue queue;
```

`Queue`는 빌드 대기열이다. `transient`이므로 config.xml에 저장되지 않으며, 생성자에서 초기화된다:

```java
// 923행 (생성자 내부)
queue = new Queue(LoadBalancer.CONSISTENT_HASH);
```

`LoadBalancer.CONSISTENT_HASH`는 일관된 해싱으로 빌드를 노드에 분배하는 전략이다.

### 3.2 버전 정보

```java
// 380행
private String version = "1.0";
```

`config.xml`에 저장될 때마다 현재 Jenkins 버전으로 갱신된다. "1.0"은 1.301 이전 버전과의 하위 호환을 위한 초기값이다. `save()` 메서드에서 업데이트된다:

```java
// 3582-3584행 (save() 내부)
if (currentMilestone == InitMilestone.COMPLETED) {
    version = VERSION;
}
```

### 3.3 실행기 수와 모드

```java
// 399행
private int numExecutors = 2;

// 404행
private Mode mode = Mode.NORMAL;
```

| 필드 | 기본값 | 설명 |
|------|--------|------|
| `numExecutors` | 2 | 빌트인 노드의 동시 빌드 수 |
| `mode` | `NORMAL` | NORMAL=모든 빌드 수용, EXCLUSIVE=레이블 매칭 빌드만 |

### 3.4 보안 필드

```java
// 424행
private volatile AuthorizationStrategy authorizationStrategy = AuthorizationStrategy.UNSECURED;

// 441행
private volatile SecurityRealm securityRealm = SecurityRealm.NO_AUTHENTICATION;
```

두 필드 모두 `volatile`로 선언되어 있다. 이유는 보안 설정이 런타임에 변경될 수 있고, 변경 즉시 모든 스레드가 새 값을 보아야 하기 때문이다.

### 3.5 플러그인 매니저

```java
// 636행
public final transient PluginManager pluginManager;
```

`final`이므로 생성자에서 한 번만 할당된다:

```java
// 955-957행 (생성자 내부)
if (pluginManager == null)
    pluginManager = PluginManager.createDefault(this);
this.pluginManager = pluginManager;
```

### 3.6 View 목록

```java
// 609행
private final CopyOnWriteArrayList<View> views = new CopyOnWriteArrayList<>();

// 617행
private volatile String primaryView;
```

`CopyOnWriteArrayList`는 읽기가 잦고 쓰기가 드문 View 목록에 적합한 자료구조다. View 변경 시 전체 배열을 복사하므로 읽기 중 락이 불필요하다.

### 3.7 Item 맵

```java
// 489행
/*package*/ final transient Map<String, TopLevelItem> items =
    new CopyOnWriteMap.Tree<>(String.CASE_INSENSITIVE_ORDER);
```

핵심 설계 결정:
- `CopyOnWriteMap.Tree`: 읽기 최적화. Job 조회는 매우 빈번하고, 추가/삭제는 상대적으로 드물다.
- `String.CASE_INSENSITIVE_ORDER`: Job 이름을 대소문자 구분 없이 검색한다. Windows 환경과의 호환성을 위한 결정이다.

### 3.8 싱글턴 인스턴스

```java
// 494행
private static Jenkins theInstance;
```

`volatile`이 아닌 이유는 아래 [싱글턴 패턴](#4-싱글턴-패턴) 절에서 설명한다.

### 3.9 초기화 레벨

```java
// 484행
private transient volatile InitMilestone initLevel = InitMilestone.STARTED;
```

현재 초기화가 어디까지 진행되었는지 추적한다. `volatile`로 선언되어 다른 스레드에서 즉시 확인 가능하다.

### 3.10 기타 핵심 필드

```java
// 479행
public final transient File root;                    // JENKINS_HOME 디렉토리

// 592행
private final transient Nodes nodes = new Nodes(this); // 에이전트 노드 관리

// 551행
public final Hudson.CloudList clouds = new Hudson.CloudList(this); // 클라우드 인스턴스

// 535행
private final transient Map<Class, ExtensionList> extensionLists = new ConcurrentHashMap<>();

// 541행
private final transient Map<Class, DescriptorExtensionList> descriptorLists = new ConcurrentHashMap<>();

// 546행
protected final transient ConcurrentMap<Node, Computer> computers = new ConcurrentHashMap<>();

// 631행
private final transient FingerprintMap fingerprintMap = new FingerprintMap();

// 673행
private volatile CrumbIssuer crumbIssuer = GlobalCrumbIssuerConfiguration.createDefaultCrumbIssuer();

// 858행
private final transient UpdateCenter updateCenter = UpdateCenter.createUpdateCenter(null);
```

### 3.11 필드 분류표

| 분류 | 필드 | 직렬화 | volatile |
|------|------|--------|----------|
| 빌드 | queue | transient | X |
| 메타 | version, initLevel | O/transient | X/O |
| 보안 | authorizationStrategy, securityRealm | O | O |
| 노드 | numExecutors, mode, nodes, computers | O/transient | X |
| 플러그인 | pluginManager, extensionLists | transient | X |
| UI | views, primaryView | O | X/O |
| Item | items | transient | X |
| 시스템 | root, servletContext | transient | X |

`transient` 필드는 config.xml에 저장되지 않고, JVM이 재시작될 때마다 새로 생성된다.

---

## 4. 싱글턴 패턴

### 4.1 인스턴스 저장 구조

Jenkins의 싱글턴 패턴은 전통적인 GoF 싱글턴과 다르다. 생성자가 `protected`이며, 외부에서 직접 호출하는 것이 아니라 `WebAppMain`이 서블릿 컨테이너 초기화 시 생성한다.

```java
// 494행
private static Jenkins theInstance;

// 789-794행
static JenkinsHolder HOLDER = new JenkinsHolder() {
    @Override
    public @CheckForNull Jenkins getInstance() {
        return theInstance;
    }
};
```

`JenkinsHolder` 인터페이스는 테스트 하네스가 `Jenkins.get()`을 가로챌 수 있도록 설계된 확장점이다:

```java
// 785-787행
public interface JenkinsHolder {
    @CheckForNull Jenkins getInstance();
}
```

### 4.2 접근 메서드 3종

```java
// 802-809행
@NonNull
public static Jenkins get() throws IllegalStateException {
    Jenkins instance = getInstanceOrNull();
    if (instance == null) {
        throw new IllegalStateException(
            "Jenkins.instance is missing. Read the documentation of "
            + "Jenkins.getInstanceOrNull to see what you are doing wrong.");
    }
    return instance;
}

// 838-840행
@CLIResolver
@CheckForNull
public static Jenkins getInstanceOrNull() {
    return HOLDER.getInstance();
}

// 847-849행 (deprecated)
@Nullable
@Deprecated
public static Jenkins getInstance() {
    return getInstanceOrNull();
}
```

세 메서드의 차이:

```
+---------------------+--------+---------------------+-------------------------------+
| 메서드               | null?  | 용도                 | 비고                          |
+---------------------+--------+---------------------+-------------------------------+
| get()               | 불가   | 일반적인 접근          | null이면 IllegalStateException |
| getInstanceOrNull() | 가능   | 시작/종료 시 안전 접근  | since 1.653                   |
| getInstance()       | 가능   | 레거시 호환            | @Deprecated                   |
+---------------------+--------+---------------------+-------------------------------+
```

### 4.3 스레드 안전성

`theInstance` 필드가 `volatile`이 아닌 것이 의아할 수 있다. 그 이유는:

1. **생성자 내부에서 할당**: `theInstance = this;` (910행)
2. **생성자는 단일 스레드에서 실행**: `WebAppMain`의 초기화 스레드
3. **할당 후 서블릿 컨텍스트에 등록**: `context.setAttribute(APP, instance)` (WebAppMain.java:255행)
4. **서블릿 컨테이너의 메모리 가시성 보장**: 서블릿 요청 처리 스레드가 시작되기 전에 컨텍스트 초기화가 완료됨

또한 생성자에서 이중 생성을 방지한다:

```java
// 910-911행 (생성자 내부)
if (theInstance != null)
    throw new IllegalStateException("second instance");
theInstance = this;
```

종료 시에는 `cleanUp()` 메서드에서 `theInstance = null`로 설정한다 (3677행).

### 4.4 싱글턴 생명주기

```
WebAppMain.contextInitialized()
    │
    ├─ new HudsonIsLoading() → context.setAttribute(APP, ...)
    │
    ├─ new Thread("Jenkins initialization thread")
    │   │
    │   ├─ new Hudson(_home, context)     ← Jenkins 생성자 호출
    │   │   └─ theInstance = this          ← 싱글턴 할당
    │   │
    │   └─ context.setAttribute(APP, instance)  ← 로딩 완료
    │
    └─ Jenkins.get().getLifecycle().onReady()   ← 서비스 시작

    ... (운영 중) ...

WebAppMain.contextDestroyed()
    │
    └─ Jenkins.cleanUp()
        └─ theInstance = null              ← 싱글턴 해제
```

---

## 5. 초기화 흐름

### 5.1 생성자 전체 구조

생성자는 약 150줄에 달하는 대규모 초기화 로직을 포함한다:

```java
// 899행
protected Jenkins(File root, ServletContext context, PluginManager pluginManager)
    throws IOException, InterruptedException, ReactorException {
```

### 5.2 초기화 단계별 분석

```
┌─────────────────────────────────────────────────────────────────┐
│                    Jenkins 생성자 초기화 흐름                      │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  1. JVM 설정                                                     │
│     ├─ JenkinsJVM 플래그 설정 (901행)                             │
│     └─ STARTUP_MARKER_FILE 생성 (903행)                          │
│                                                                 │
│  2. 보안 컨텍스트 설정                                             │
│     └─ ACL.as2(ACL.SYSTEM2) → 전체 권한으로 실행 (905행)           │
│                                                                 │
│  3. 기본 필드 초기화                                               │
│     ├─ root, servletContext 할당 (906-908행)                      │
│     ├─ 버전 계산 (909행)                                          │
│     ├─ 싱글턴 중복 검사 및 할당 (910-912행)                        │
│     └─ 워크스페이스 디렉토리 설정 (914-917행)                      │
│                                                                 │
│  4. 핵심 컴포넌트 생성                                             │
│     ├─ Trigger.timer 생성 (922행)                                 │
│     ├─ Queue 생성 (923행)                                         │
│     ├─ DependencyGraph 초기화 (926행)                             │
│     └─ 시크릿 키 로드 또는 생성 (935-947행)                        │
│                                                                 │
│  5. 플러그인 초기화                                                │
│     ├─ PluginManager 생성 (955-957행)                             │
│     ├─ WebApp 클래스로더 설정 (958-960행)                          │
│     ├─ Stapler 필터 설정 (963-974행)                              │
│     └─ AdjunctManager 생성 (976행)                                │
│                                                                 │
│  6. Reactor 실행 (핵심!)                                          │
│     └─ executeReactor(is,                                        │
│            pluginManager.initTasks(is),                           │
│            loadTasks(),                                           │
│            InitMilestone.ordering())  (985-988행)                 │
│                                                                 │
│  7. 후속 초기화                                                    │
│     ├─ save() (1006행)                                            │
│     ├─ TCP Agent 리스너 시작 (1008행)                              │
│     ├─ 라벨 정리 타이머 시작 (1010-1015행)                         │
│     ├─ 컴퓨터 목록 갱신 (1017행)                                   │
│     ├─ ComputerListener 알림 (1019-1033행)                        │
│     ├─ ItemListener.onLoaded() 호출 (1035-1045행)                 │
│     └─ STARTUP_MARKER_FILE.on() (1051행)                          │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

### 5.3 시크릿 키 관리

```java
// 935-947행
TextFile secretFile = new TextFile(new File(getRootDir(), "secret.key"));
if (secretFile.exists()) {
    secretKey = secretFile.readTrim();
} else {
    byte[] random = new byte[32];
    RANDOM.nextBytes(random);
    secretKey = Util.toHexString(random);
    secretFile.write(secretKey);

    // SECURITY-49: 새로 생성된 키임을 표시
    new FileBoolean(new File(root, "secret.key.not-so-secret")).on();
}
```

`secret.key`는 32바이트 랜덤 값을 16진수 문자열로 변환하여 `$JENKINS_HOME/secret.key`에 저장한다. 이 키는 API 토큰, 빌드 시크릿 등 다양한 곳에서 사용된다.

### 5.4 ACL.as2(ACL.SYSTEM2) 패턴

생성자 전체가 `try (ACLContext ctx = ACL.as2(ACL.SYSTEM2))` 블록 안에서 실행된다 (905행). 이는 초기화 과정에서 모든 권한 검사를 우회하기 위한 것이다. 플러그인 로딩, Job 로딩 등이 보안 제약 없이 진행되어야 하기 때문이다.

---

## 6. InitMilestone과 Reactor 기반 초기화

### 6.1 InitMilestone 열거형

```java
// core/src/main/java/hudson/init/InitMilestone.java
public enum InitMilestone implements Milestone {
    STARTED("Started initialization"),
    PLUGINS_LISTED("Listed all plugins"),
    PLUGINS_PREPARED("Prepared all plugins"),
    PLUGINS_STARTED("Started all plugins"),
    EXTENSIONS_AUGMENTED("Augmented all extensions"),
    SYSTEM_CONFIG_LOADED("System config loaded"),
    SYSTEM_CONFIG_ADAPTED("System config adapted"),
    JOB_LOADED("Loaded all jobs"),
    JOB_CONFIG_ADAPTED("Configuration for all jobs updated"),
    COMPLETED("Completed initialization");
}
```

### 6.2 마일스톤 진행 순서와 각 단계의 의미

```
STARTED
   │   초기화 시작, 아무것도 하지 않음
   ▼
PLUGINS_LISTED
   │   모든 플러그인 메타데이터 검사, 의존성 파악 완료
   ▼
PLUGINS_PREPARED
   │   모든 플러그인 메타데이터 로드, 클래스로더 설정 완료
   ▼
PLUGINS_STARTED
   │   모든 플러그인 실행 시작, 확장점 로드, Descriptor 인스턴스화
   ▼
EXTENSIONS_AUGMENTED
   │   프로그래밍 방식으로 추가된 확장점 구현 등록 완료
   ▼
SYSTEM_CONFIG_LOADED
   │   config.xml에서 시스템 설정 로드 완료
   ▼
SYSTEM_CONFIG_ADAPTED
   │   플러그인(예: CasC)이 설정 파일을 갱신할 수 있는 시점
   ▼
JOB_LOADED
   │   모든 Job과 빌드 기록이 디스크에서 로드 완료
   ▼
JOB_CONFIG_ADAPTED
   │   Job 설정이 필요시 갱신된 시점 (GroovyInitScript 전)
   ▼
COMPLETED
       초기화 완료, GroovyInitScript 포함 모든 실행 완료
```

### 6.3 executeReactor 메서드

```java
// 1134행
private void executeReactor(final InitStrategy is, TaskBuilder... builders)
    throws IOException, InterruptedException, ReactorException {

    Reactor reactor = new Reactor(builders) {
        @Override
        protected void runTask(Task task) throws Exception {
            if (is != null && is.skipInitTask(task))  return;

            String taskName = InitReactorRunner.getDisplayName(task);
            Thread t = Thread.currentThread();
            String name = t.getName();
            if (taskName != null)
                t.setName(taskName);
            try (ACLContext ctx = ACL.as2(ACL.SYSTEM2)) {
                long start = System.currentTimeMillis();
                super.runTask(task);
                if (LOG_STARTUP_PERFORMANCE)
                    LOGGER.info(String.format("Took %dms for %s by %s",
                            System.currentTimeMillis() - start, taskName, name));
            } catch (Exception | Error x) {
                if (containsLinkageError(x)) {
                    LOGGER.log(Level.WARNING,
                        taskName + " failed perhaps due to plugin dependency issues", x);
                } else {
                    throw x;
                }
            } finally {
                t.setName(name);
            }
        }
    };

    new InitReactorRunner() {
        @Override
        protected void onInitMilestoneAttained(InitMilestone milestone) {
            initLevel = milestone;
            getLifecycle().onExtendTimeout(EXTEND_TIMEOUT_SECONDS, TimeUnit.SECONDS);
            if (milestone == PLUGINS_PREPARED) {
                // Guice 주입을 가능한 빨리 설정
                ExtensionList.lookup(ExtensionFinder.class).getComponents();
            }
        }
    }.run(reactor);
}
```

Reactor 패턴의 핵심 설계:

1. **TaskBuilder 합성**: `pluginManager.initTasks()`, `loadTasks()`, `InitMilestone.ordering()`을 하나의 Reactor에 합친다.
2. **LinkageError 내성**: 플러그인 의존성 문제로 인한 `LinkageError`는 경고만 기록하고 계속 진행한다.
3. **스레드 이름 설정**: 디버깅을 위해 현재 실행 중인 태스크명을 스레드 이름에 반영한다.
4. **마일스톤 콜백**: 마일스톤 도달 시 `initLevel`을 갱신하고, 서블릿 컨테이너의 타임아웃을 연장한다.

### 6.4 loadTasks() — Job 로딩 태스크 그래프

```java
// 3463행
private synchronized TaskBuilder loadTasks() throws IOException {
    File projectsDir = new File(root, "jobs");
    // ...
    TaskGraphBuilder g = new TaskGraphBuilder();

    // 1단계: 글로벌 설정 로드
    Handle loadJenkins = g.requires(EXTENSIONS_AUGMENTED)
        .attains(SYSTEM_CONFIG_LOADED)
        .add("Loading global config", session -> {
            load();                          // config.xml 로드
            if (slaves != null && !slaves.isEmpty() && nodes.isLegacy()) {
                nodes.setNodes(slaves);      // 레거시 데이터 마이그레이션
                slaves = null;
            } else {
                nodes.load();                // 노드 설정 로드
            }
        });

    // 2단계: 각 Job 병렬 로드
    List<Handle> loadJobs = new ArrayList<>();
    for (final File subdir : subdirs) {
        loadJobs.add(g.requires(loadJenkins)
            .requires(SYSTEM_CONFIG_ADAPTED)
            .attains(JOB_LOADED)
            .notFatal()                      // Job 하나 실패해도 전체 중단하지 않음
            .add("Loading item " + subdir.getName(), session -> {
                if (!Items.getConfigFile(subdir).exists()) return;
                TopLevelItem item = (TopLevelItem) Items.load(Jenkins.this, subdir);
                items.put(item.getName(), item);
                loadedNames.add(item.getName());
            }));
    }

    // 3단계: 디스크에서 삭제된 Item 정리
    g.requires(loadJobs.toArray(new Handle[0]))
        .attains(JOB_LOADED)
        .add("Cleaning up obsolete items deleted from the disk", reactor -> {
            for (String name : items.keySet()) {
                if (!loadedNames.contains(name))
                    items.remove(name);
            }
        });

    // 4단계: 최종 설정
    g.requires(JOB_CONFIG_ADAPTED)
        .attains(COMPLETED)
        .add("Finalizing set up", session -> {
            rebuildDependencyGraph();
            // 보안 설정 마이그레이션
            // RootAction 등록
            // SetupWizard 초기화
        });

    return g;
}
```

태스크 그래프 의존성:

```
EXTENSIONS_AUGMENTED
        │
        ▼
[Loading global config] ──attains──▶ SYSTEM_CONFIG_LOADED
        │
        ▼
SYSTEM_CONFIG_ADAPTED
        │
        ├──▶ [Loading item job1] ─┐
        ├──▶ [Loading item job2] ─┤
        ├──▶ [Loading item job3] ─┤──attains──▶ JOB_LOADED
        └──▶ [Loading item jobN] ─┘
                                    │
                                    ▼
                         [Cleaning up obsolete items]
                                    │
                                    ▼
                            JOB_CONFIG_ADAPTED
                                    │
                                    ▼
                         [Finalizing set up] ──attains──▶ COMPLETED
```

`notFatal()` 호출이 중요하다: 개별 Job 로딩 실패가 전체 Jenkins 시작을 중단시키지 않는다. 수백 개의 Job 중 하나가 손상되어도 나머지는 정상 로드된다.

---

## 7. 설정 저장과 로드

### 7.1 save() 메서드

```java
// 3561행
@Override
public synchronized void save() throws IOException {
    InitMilestone currentMilestone = initLevel;

    // 설정이 로드되기 전에 저장 시도하면 거부
    if (!configLoaded) {
        LOGGER.log(Level.SEVERE,
            "An attempt to save Jenkins' global configuration before it has been loaded...");
        throw new IllegalStateException(
            "An attempt to save the global configuration was made before it was loaded");
    }

    // BulkChange 중이면 저장 지연
    if (BulkChange.contains(this)) {
        return;
    }

    // 초기화 완료 시에만 버전 갱신
    if (currentMilestone == InitMilestone.COMPLETED) {
        version = VERSION;
    }

    // nodeRenameMigrationNeeded 처리
    if (nodeRenameMigrationNeeded == null) {
        nodeRenameMigrationNeeded = false;
    }

    // 실제 저장
    getConfigFile().write(this);
    SaveableListener.fireOnChange(this, getConfigFile());
}
```

핵심 안전장치:

| 보호 메커니즘 | 목적 |
|--------------|------|
| `synchronized` | 동시 저장 방지 |
| `configLoaded` 검사 | 초기화 전 저장 시도 차단 (데이터 손실 방지) |
| `BulkChange` 검사 | 대량 변경 중 중간 저장 방지 |
| 마일스톤 검사 | 초기화 중 버전 필드 조기 갱신 방지 |

### 7.2 getConfigFile()

```java
// 3278행
protected XmlFile getConfigFile() {
    return new XmlFile(XSTREAM, new File(root, "config.xml"));
}
```

`XSTREAM`은 Jenkins 전용으로 설정된 XStream2 인스턴스다:

```java
// 5929-5940행 (static 블록)
XSTREAM = XSTREAM2 = new XStream2();
XSTREAM.alias("jenkins", Jenkins.class);
XSTREAM.alias("slave", DumbSlave.class);
XSTREAM.alias("jdk", JDK.class);
XSTREAM.alias("view", ListView.class);
XSTREAM.alias("listView", ListView.class);
XSTREAM2.addCriticalField(Jenkins.class, "securityRealm");
XSTREAM2.addCriticalField(Jenkins.class, "authorizationStrategy");
```

`addCriticalField`는 역직렬화 실패 시 기본값으로 대체하지 않고 예외를 발생시키도록 한다. 보안 관련 필드가 잘못 로드되면 전체 시스템의 보안이 무력화될 수 있기 때문이다.

### 7.3 load() 메서드

```java
// 3348행
@Override
public void load() throws IOException {
    XmlFile cfg = getConfigFile();
    if (cfg.exists()) {
        // 기존 값을 백업하여 실패 시 복원
        String originalPrimaryView = primaryView;
        List<View> originalViews = new ArrayList<>(views);
        primaryView = null;
        views.clear();
        try {
            cfg.unmarshal(Jenkins.this);   // config.xml → 현재 객체에 역직렬화
        } catch (IOException | RuntimeException x) {
            // 실패 시 원본 복원
            primaryView = originalPrimaryView;
            views.clear();
            views.addAll(originalViews);
            throw x;
        }
    }

    // 기본 View 보장
    if (views.isEmpty() || primaryView == null) {
        View v = new AllView(AllView.DEFAULT_VIEW_NAME);
        setViewOwner(v);
        views.addFirst(v);
        primaryView = v.getViewName();
    }

    // 후속 처리
    primaryView = AllView.migrateLegacyPrimaryAllViewLocalizedName(views, primaryView);
    clouds.setOwner(this);
    configLoaded = true;                   // 로드 완료 플래그
    // ...
    resetFilter(securityRealm, null);       // 보안 필터 재설정
    updateComputers(this);                  // 컴퓨터 목록 갱신
}
```

로드 과정의 안전장치:
1. **원본 백업**: View 목록과 primaryView를 백업한 후 역직렬화를 시도한다.
2. **실패 시 복원**: 역직렬화 실패 시 백업된 원본을 복원한다.
3. **기본 View 보장**: View가 비어있으면 AllView를 자동 생성한다.
4. **configLoaded 플래그**: `save()` 메서드의 조기 저장 방지 플래그를 true로 설정한다.

### 7.4 config.xml 구조 (개념)

```xml
<?xml version='1.1' encoding='UTF-8'?>
<jenkins>
  <version>2.xxx</version>
  <numExecutors>2</numExecutors>
  <mode>NORMAL</mode>
  <useSecurity>true</useSecurity>
  <authorizationStrategy class="...">
    <!-- 권한 설정 -->
  </authorizationStrategy>
  <securityRealm class="...">
    <!-- 인증 설정 -->
  </securityRealm>
  <projectNamingStrategy class="..."/>
  <workspaceDir>...</workspaceDir>
  <buildsDir>...</buildsDir>
  <views>
    <hudson.model.AllView>
      <name>all</name>
    </hudson.model.AllView>
  </views>
  <primaryView>all</primaryView>
  <slaveAgentPort>-1</slaveAgentPort>
  <label></label>
  <crumbIssuer class="..."/>
  <nodeProperties/>
  <globalNodeProperties/>
  <clouds/>
</jenkins>
```

---

## 8. Stapler 루트 역할

### 8.1 Stapler 프레임워크와 Jenkins

Stapler는 URL 경로를 Java 객체 트리에 매핑하는 웹 프레임워크다. Jenkins 인스턴스가 URL 공간의 루트(`/`)에 바인딩된다.

```
HTTP 요청: GET /job/my-project/build
    │
    ▼
Jenkins (루트 객체, "/")
    │
    ├─ getItem("my-project")  →  "/job/my-project"
    │       │
    │       └─ doBuild()      →  "/job/my-project/build"
    │
    ├─ getView("All")        →  "/"  (StaplerFallback)
    ├─ getComputer("agent1") →  "/computer/agent1"
    └─ getUser("admin")      →  "/user/admin"
```

### 8.2 StaplerProxy — getTarget()

```java
// 5217행
@Override
public Object getTarget() {
    try {
        checkPermission(READ);
    } catch (AccessDeniedException e) {
        if (!isSubjectToMandatoryReadPermissionCheck(
                Stapler.getCurrentRequest2().getRestOfPath())) {
            return this;
        }
        throw e;
    }
    return this;
}
```

`StaplerProxy`의 `getTarget()`은 Stapler가 요청을 처리하기 전에 호출하는 일종의 게이트키퍼다:

1. `READ` 권한 확인
2. 권한 없으면 → 현재 URL이 보호 면제 경로인지 확인
3. 면제 경로(`ALWAYS_READABLE_PATHS`)면 `this` 반환 (접근 허용)
4. 아니면 `AccessDeniedException` 전파

### 8.3 항상 접근 가능한 경로

```java
// 5876-5891행
private static final Set<String> ALWAYS_READABLE_PATHS = new HashSet<>(Arrays.asList(
    "404",              // 404 페이지
    "_404",             // 404 Jelly 뷰
    "_404_simple",      // 간단한 404 뷰
    "login",            // 로그인 페이지
    "loginError",       // 로그인 오류 페이지
    "logout",           // 로그아웃
    "accessDenied",     // 접근 거부 페이지
    "adjuncts",         // 정적 리소스
    "error",            // 오류 페이지
    "oops",             // 시스템 오류 페이지
    "signup",           // 회원가입
    "tcpSlaveAgentListener",   // 에이전트 연결
    "federatedLoginService",   // 연합 로그인
    "securityRealm"            // 보안 Realm 접근
));
```

시스템 프로퍼티 `jenkins.model.Jenkins.additionalReadablePaths`로 추가 경로를 설정할 수 있다 (5894-5898행).

`UnprotectedRootAction` 인터페이스를 구현하는 플러그인의 액션도 보호 면제 대상에 포함된다:

```java
// 5269행
public Collection<String> getUnprotectedRootActions() {
    Set<String> names = new TreeSet<>();
    names.add("jnlpJars");
    for (Action a : getActions()) {
        if (a instanceof UnprotectedRootAction) {
            String url = a.getUrlName();
            if (url == null) continue;
            names.add(url);
        }
    }
    return names;
}
```

### 8.4 StaplerFallback — getStaplerFallback()

```java
// 5287행
@Override
public View getStaplerFallback() {
    return getPrimaryView();
}
```

URL 매핑에서 일치하는 것이 없을 때 기본 View(보통 "All" 뷰)가 반환된다. 사용자가 Jenkins 루트 URL(`/`)에 접속하면 대시보드가 표시되는 이유가 바로 이것이다.

---

## 9. Item 관리

### 9.1 Item 조회

#### getItem(String name) — 이름으로 직접 조회

```java
// 3021행
@Override
public TopLevelItem getItem(String name) throws AccessDeniedException {
    if (name == null)    return null;
    TopLevelItem item = items.get(name);   // CopyOnWriteMap에서 조회
    if (item == null)
        return null;
    if (!item.hasPermission(Item.READ)) {
        if (item.hasPermission(Item.DISCOVER)) {
            throw new AccessDeniedException("Please login to access job " + name);
        }
        return null;                        // DISCOVER 권한도 없으면 존재 자체를 숨김
    }
    return item;
}
```

보안 로직이 중요하다:

```
items.get(name)
    │
    ├─ null ────────────────────────▶ null 반환
    │
    └─ item 존재
        │
        ├─ Item.READ 권한 있음 ─────▶ item 반환
        │
        └─ Item.READ 권한 없음
            │
            ├─ Item.DISCOVER 권한 있음 ──▶ AccessDeniedException
            │                            ("로그인 필요")
            │
            └─ Item.DISCOVER 권한 없음 ──▶ null 반환
                                          (존재 자체를 숨김)
```

`DISCOVER` 권한은 "이 항목이 존재한다"는 사실만 알 수 있는 권한이다. DISCOVER 권한이 있는 사용자에게는 "로그인하라"는 메시지를, 없는 사용자에게는 아예 존재하지 않는 것처럼 `null`을 반환한다.

#### getItem(String pathName, ItemGroup context) — 경로 기반 조회

```java
// 3050행
public Item getItem(String pathName, ItemGroup context) {
    if (context == null)  context = this;
    if (pathName == null) return null;

    if (pathName.startsWith("/"))   // 절대 경로
        return getItemByFullName(pathName);

    // 상대 경로: 파일 시스템과 유사한 탐색
    Object ctx = context;
    StringTokenizer tokens = new StringTokenizer(pathName, "/");
    while (tokens.hasMoreTokens()) {
        String s = tokens.nextToken();
        if (s.equals("..")) {
            if (ctx instanceof Item)
                ctx = ((Item) ctx).getParent();
            // ...
        }
        if (ctx instanceof ItemGroup g) {
            Item i = g.getItem(s);
            // ...
            ctx = i;
        }
    }
    // ...
    return getItemByFullName(pathName);  // 폴백: fullName으로 시도
}
```

파일 시스템의 경로 탐색과 동일한 패턴이다. `..`으로 부모 이동, `.`으로 현재 위치 유지, `/`로 시작하면 절대 경로로 해석한다. Jenkins Folder 플러그인과 함께 사용하면 `/folder1/folder2/my-job`처럼 중첩 경로를 지원한다.

#### getItemByFullName(String fullName, Class<T> type) — 전체 이름으로 조회

```java
// 3128행
public @CheckForNull <T extends Item> T getItemByFullName(
        @NonNull String fullName, Class<T> type) throws AccessDeniedException {
    StringTokenizer tokens = new StringTokenizer(fullName, "/");
    ItemGroup parent = this;

    if (!tokens.hasMoreTokens()) return null;

    while (true) {
        Item item = parent.getItem(tokens.nextToken());
        if (!tokens.hasMoreTokens()) {
            if (type.isInstance(item))
                return type.cast(item);
            else
                return null;
        }
        if (!(item instanceof ItemGroup))
            return null;            // 중간 경로가 ItemGroup이 아니면 탐색 불가
        if (!item.hasPermission(Item.READ))
            return null;
        parent = (ItemGroup) item;  // 다음 레벨로 진입
    }
}
```

### 9.2 Item 생성

```java
// 3174행
@NonNull
@Override
public synchronized TopLevelItem createProject(
        @NonNull TopLevelItemDescriptor type,
        @NonNull String name,
        boolean notify) throws IOException {
    return itemGroupMixIn.createProject(type, name, notify);
}
```

실제 생성은 `ItemGroupMixIn`에 위임한다. `itemGroupMixIn`은 Jenkins 생성자에서 초기화되는 내부 헬퍼다:

```java
// 767행
private final transient ItemGroupMixIn itemGroupMixIn = new ItemGroupMixIn(this, this) {
    @Override
    protected void add(TopLevelItem item) {
        items.put(item.getName(), item);
    }

    @Override
    protected File getRootDirFor(String name) {
        return Jenkins.this.getRootDirFor(name);
    }
};
```

XML에서 생성하는 API도 제공한다:

```java
// 4239행
@Override
public TopLevelItem createProjectFromXML(String name, InputStream xml) throws IOException {
    return itemGroupMixIn.createProjectFromXML(name, xml);
}
```

### 9.3 Item 삭제

```java
// 3229행
@Override
public void onDeleted(TopLevelItem item) throws IOException {
    ItemListener.fireOnDeleted(item);      // 리스너에게 알림

    items.remove(item.getName());          // 맵에서 제거

    // 하위 호환: 모든 View에서 제거
    for (View v : views)
        v.onJobRenamed(item, item.getName(), null);  // null = 삭제됨
}
```

### 9.4 Item 이름 변경

```java
// 3216행
@Override
public void onRenamed(TopLevelItem job, String oldName, String newName) throws IOException {
    items.remove(oldName);
    items.put(newName, job);

    // 하위 호환: 모든 View에 알림
    for (View v : views)
        v.onJobRenamed(job, oldName, newName);
}
```

### 9.5 이름 유효성 검사

```java
// 4268행
public static void checkGoodName(String name) throws Failure {
    if (name == null || name.isEmpty())
        throw new Failure(Messages.Hudson_NoName());

    if (".".equals(name.trim()))
        throw new Failure(Messages.Jenkins_NotAllowedName("."));
    if ("..".equals(name.trim()))
        throw new Failure(Messages.Jenkins_NotAllowedName(".."));

    for (int i = 0; i < name.length(); i++) {
        char ch = name.charAt(i);
        if (Character.isISOControl(ch)) {
            throw new Failure(Messages.Hudson_ControlCodeNotAllowed(toPrintableName(name)));
        }
        if ("?*/\\%!@#$^&|<>[]:;".indexOf(ch) != -1)
            throw new Failure(Messages.Hudson_UnsafeChar(ch));
    }

    // SECURITY-2424: Windows에서 후행 점 사용 시 모호성 방지
    if (SystemProperties.getBoolean(NAME_VALIDATION_REJECTS_TRAILING_DOT_PROP, true)) {
        if (name.trim().endsWith(".")) {
            throw new Failure(Messages.Hudson_TrailingDot());
        }
    }
}
```

금지 문자: `? * / \ % ! @ # $ ^ & | < > [ ] : ;`

이 제한은 Job 이름이 파일 시스템의 디렉토리명으로 사용되기 때문이다. `$JENKINS_HOME/jobs/{job_name}/` 디렉토리가 생성되므로, 파일 시스템에서 문제를 일으킬 수 있는 문자를 모두 금지한다.

---

## 10. 보안 통합

### 10.1 SecurityRealm 설정

```java
// 2729행
public void setSecurityRealm(@CheckForNull SecurityRealm securityRealm) {
    if (securityRealm == null)
        securityRealm = SecurityRealm.NO_AUTHENTICATION;
    this.useSecurity = true;

    // UserIdStrategy 변경 감지 (User 디렉토리 재키잉에 필요)
    IdStrategy oldUserIdStrategy = this.securityRealm == null
            ? securityRealm.getUserIdStrategy()
            : this.securityRealm.getUserIdStrategy();

    this.securityRealm = securityRealm;
    resetFilter(securityRealm, oldUserIdStrategy);   // 서블릿 필터 재설정
    saveQuietly();                                    // 설정 저장
}
```

`resetFilter`는 `HudsonFilter`를 재설정하여 새 `SecurityRealm`의 인증 필터 체인을 활성화한다:

```java
// 2746행
private void resetFilter(@CheckForNull SecurityRealm securityRealm,
                          @CheckForNull IdStrategy oldUserIdStrategy) {
    try {
        HudsonFilter filter = HudsonFilter.get(getServletContext());
        if (filter == null) {
            // JENKINS-3069: 서블릿 초기화 순서 문제
            LOGGER.fine("HudsonFilter has not yet been initialized");
        } else {
            filter.reset(securityRealm);
        }
        // UserIdStrategy가 변경되면 User 디렉토리 재키잉
        if (oldUserIdStrategy != null && this.securityRealm != null
                && !oldUserIdStrategy.equals(this.securityRealm.getUserIdStrategy())) {
            User.rekey();
        }
    } catch (ServletException e) {
        throw new RuntimeException("Failed to configure filter", e) {};
    }
}
```

### 10.2 AuthorizationStrategy 설정

```java
// 2772행
public void setAuthorizationStrategy(@CheckForNull AuthorizationStrategy a) {
    if (a == null)
        a = AuthorizationStrategy.UNSECURED;
    useSecurity = true;
    authorizationStrategy = a;
    saveQuietly();
}
```

### 10.3 ACL 루트

```java
// 2920행
@NonNull
@Override
public ACL getACL() {
    return authorizationStrategy.getRootACL();
}
```

Jenkins의 ACL 체계에서 `Jenkins.getACL()`이 최상위 ACL이다. 모든 하위 객체의 권한 확인이 궁극적으로 이 ACL에 의존한다.

### 10.4 현재 인증 정보 가져오기

```java
// 4806행
public static @NonNull Authentication getAuthentication2() {
    Authentication a = SecurityContextHolder.getContext().getAuthentication();
    // Tomcat에서 로그인 페이지 서빙 시 null일 수 있음
    if (a == null)
        a = ANONYMOUS2;
    return a;
}
```

Spring Security의 `SecurityContextHolder`에서 현재 스레드의 인증 정보를 가져온다. `null`이면 익명 인증으로 대체한다.

```java
// 5908-5912행
public static final Authentication ANONYMOUS2 =
        new AnonymousAuthenticationToken(
                "anonymous",
                "anonymous",
                Set.of(new SimpleGrantedAuthority("anonymous")));
```

### 10.5 보안 모드 확인

```java
// 2705행
public SecurityMode getSecurity() {
    SecurityRealm realm = securityRealm;  // volatile 읽기를 한 번만 수행
    if (realm == SecurityRealm.NO_AUTHENTICATION)
        return SecurityMode.UNSECURED;
    if (realm instanceof LegacySecurityRealm)
        return SecurityMode.LEGACY;
    return SecurityMode.SECURED;
}
```

`volatile` 필드를 지역 변수에 복사하는 패턴은 동시성 코드에서 흔한 최적화다. 같은 값을 두 번 읽을 때 사이에 다른 스레드가 값을 변경하는 문제를 방지한다.

### 10.6 보안 비활성화 (비상 탈출)

```java
// 2788행
public void disableSecurity() {
    useSecurity = null;
    setSecurityRealm(SecurityRealm.NO_AUTHENTICATION);
    authorizationStrategy = AuthorizationStrategy.UNSECURED;
}
```

관리자가 자기 자신을 잠가버린 경우의 비상 탈출구다. `loadTasks()`의 최종 단계에서도 이 로직이 활용된다:

```java
// 3522-3526행 (loadTasks 내부, Finalizing set up)
if (useSecurity != null && !useSecurity) {
    // 비보안 모드로 강제 리셋 — 잠긴 사용자를 위한 탈출구
    authorizationStrategy = AuthorizationStrategy.UNSECURED;
    setSecurityRealm(SecurityRealm.NO_AUTHENTICATION);
}
```

---

## 11. 권한 모델

### 11.1 권한 상수

```java
// 5830행
public static final PermissionGroup PERMISSIONS = Permission.HUDSON_PERMISSIONS;
public static final Permission ADMINISTER = Permission.HUDSON_ADMINISTER;

// 5844행
public static final Permission MANAGE = new Permission(PERMISSIONS, "Manage",
        Messages._Jenkins_Manage_Description(),
        ADMINISTER,
        true,
        new PermissionScope[]{PermissionScope.JENKINS});

// 5856행
public static final Permission SYSTEM_READ = new Permission(PERMISSIONS, "SystemRead",
        Messages._Jenkins_SystemRead_Description(),
        ADMINISTER,
        SystemProperties.getBoolean("jenkins.security.SystemReadPermission"),
        new PermissionScope[]{PermissionScope.JENKINS});

// 5866행
public static final Permission READ = new Permission(PERMISSIONS, "Read",
        Messages._Hudson_ReadPermission_Description(),
        Permission.READ, PermissionScope.JENKINS);
```

### 11.2 권한 계층도

```
Jenkins.ADMINISTER
    │
    ├── Jenkins.MANAGE
    │       │
    │       └── (시스템 설정 중 안전한 부분만 변경 가능)
    │
    ├── Jenkins.SYSTEM_READ
    │       │
    │       └── (시스템 설정 읽기 전용)
    │
    └── Jenkins.READ
            │
            └── (Jenkins UI 접근, 대시보드 보기)
```

### 11.3 권한별 사용처

코드에서 `checkPermission()`이 호출되는 패턴:

| 권한 | 사용처 | 코드 위치 |
|------|--------|----------|
| `ADMINISTER` | Groovy 스크립트 실행, GC 강제, OOM 시뮬레이션, 종료/재시작 | 4469, 4511, 4732, 4766행 |
| `MANAGE` | 설정 리로드, 재시작, Quiet Down, 설정 페이지 | 4035, 4147, 4167, 4401, 4534행 |
| `SYSTEM_READ` | 시스템 설정 읽기 | 2665행 |
| `READ` | 기본 UI 접근 (getTarget에서 확인) | 5219행 |

`MANAGE`와 `ADMINISTER`의 구분은 Jenkins 2.222에서 도입되었다. 스크립트 실행이나 에이전트 경로 설정처럼 보안에 직접 영향을 미치는 기능은 반드시 `ADMINISTER`를 요구한다.

---

## 12. Extension 시스템 통합

### 12.1 ExtensionList 관리

```java
// 535행
private final transient Map<Class, ExtensionList> extensionLists = new ConcurrentHashMap<>();

// 2819행
public <T> ExtensionList<T> getExtensionList(Class<T> extensionType) {
    ExtensionList<T> extensionList = extensionLists.get(extensionType);
    return extensionList != null
        ? extensionList
        : extensionLists.computeIfAbsent(extensionType,
            key -> ExtensionList.create(this, key));
}
```

`ConcurrentHashMap.computeIfAbsent`를 사용하여 스레드 안전하게 지연 생성한다.

### 12.2 DescriptorExtensionList 관리

```java
// 541행
private final transient Map<Class, DescriptorExtensionList> descriptorLists = new ConcurrentHashMap<>();

// 2845행
public @NonNull <T extends Describable<T>, D extends Descriptor<T>>
        DescriptorExtensionList<T, D> getDescriptorList(Class<T> type) {
    return descriptorLists.computeIfAbsent(type,
        key -> DescriptorExtensionList.createDescriptorList(this, key));
}
```

### 12.3 확장점 새로고침 (플러그인 동적 로딩)

```java
// 2854행
public void refreshExtensions() throws ExtensionRefreshException {
    ExtensionList<ExtensionFinder> finders = getExtensionList(ExtensionFinder.class);
    for (ExtensionFinder ef : finders) {
        if (!ef.isRefreshable())
            throw new ExtensionRefreshException(ef + " doesn't support refresh");
    }

    List<ExtensionComponentSet> fragments = new ArrayList<>();
    for (ExtensionFinder ef : finders) {
        fragments.add(ef.refresh());
    }
    ExtensionComponentSet delta = ExtensionComponentSet.union(fragments).filtered();

    // 새 ExtensionFinder가 발견되면 재귀적으로 탐색
    List<ExtensionComponent<ExtensionFinder>> newFinders =
        new ArrayList<>(delta.find(ExtensionFinder.class));
    while (!newFinders.isEmpty()) {
        ExtensionFinder f = newFinders.removeLast().getInstance();
        ExtensionComponentSet ecs = ExtensionComponentSet.allOf(f).filtered();
        newFinders.addAll(ecs.find(ExtensionFinder.class));
        delta = ExtensionComponentSet.union(delta, ecs);
    }

    // 모든 ExtensionList 새로고침
    List<ExtensionList> listsToFireOnChangeListeners = new ArrayList<>();
    for (ExtensionList el : extensionLists.values()) {
        if (el.refresh(delta)) {
            listsToFireOnChangeListeners.add(el);
        }
    }
    for (ExtensionList el : descriptorLists.values()) {
        if (el.refresh(delta)) {
            listsToFireOnChangeListeners.add(el);
        }
    }

    // 모든 리스트 새로고침 후 리스너 발화 (중복 등록 방지)
    for (var el : listsToFireOnChangeListeners) {
        el.fireOnChangeListeners();
    }

    // RootAction 추가
    for (ExtensionComponent<RootAction> ea : delta.find(RootAction.class)) {
        Action a = ea.getInstance();
        if (!actions.contains(a)) actions.add(a);
    }
}
```

이 메서드는 `PluginManager.dynamicLoad()`에서 호출되어 런타임에 플러그인을 로드할 수 있게 한다.

### 12.4 Guice 의존성 주입

```java
// 2811행
public @CheckForNull Injector getInjector() {
    return lookup(Injector.class);
}
```

Jenkins는 Google Guice를 사용하여 의존성 주입을 수행한다. `@Inject` 어노테이션이 붙은 필드가 자동으로 주입되며, `executeReactor()`에서 `PLUGINS_PREPARED` 마일스톤 도달 시 Guice가 설정된다:

```java
// 1180-1184행 (executeReactor 내부)
if (milestone == PLUGINS_PREPARED) {
    // Guice 주입을 가능한 빨리 설정
    ExtensionList.lookup(ExtensionFinder.class).getComponents();
}
```

---

## 13. 종료(cleanUp) 흐름

### 13.1 cleanUp() 메서드 구조

```java
// 3612행
public void cleanUp() {
    // 싱글턴 검증
    if (theInstance != this && theInstance != null) {
        LOGGER.log(Level.WARNING,
            "This instance is no longer the singleton, ignoring cleanUp()");
        return;
    }
    // 중복 호출 방지
    synchronized (Jenkins.class) {
        if (cleanUpStarted) {
            LOGGER.log(Level.WARNING,
                "Jenkins.cleanUp() already started, ignoring repeated cleanUp()");
            return;
        }
        cleanUpStarted = true;
    }

    try {
        // ... 정리 작업 ...
    } finally {
        theInstance = null;               // 싱글턴 해제
        if (JenkinsJVM.isJenkinsJVM()) {
            JenkinsJVMAccess._setJenkinsJVM(oldJenkinsJVM);
        }
        ClassFilterImpl.unregister();
    }
}
```

### 13.2 정리 순서

```
cleanUp() 시작
    │
    ├─ 1. fireBeforeShutdown(errors)            ← ItemListener.onBeforeShutdown()
    │
    ├─ 2. _cleanUpRunTerminators(errors)        ← @Terminator 어노테이션 실행
    │
    ├─ 3. terminating = true                    ← 종료 플래그 설정
    │
    ├─ 4. _cleanUpDisconnectComputers(errors)   ← 모든 에이전트 연결 해제
    │
    ├─ 5. _cleanUpCancelDependencyGraphCalculation()
    │
    ├─ 6. _cleanUpInterruptReloadThread(errors) ← 리로드 스레드 중단
    │
    ├─ 7. _cleanUpShutdownTriggers(errors)      ← Trigger 타이머 종료
    │
    ├─ 8. _cleanUpShutdownTimer(errors)         ← Jenkins 타이머 종료
    │
    ├─ 9. _cleanUpShutdownTcpSlaveAgent(errors) ← TCP 에이전트 리스너 종료
    │
    ├─ 10. _cleanUpShutdownPluginManager(errors) ← 플러그인 매니저 종료
    │
    ├─ 11. _cleanUpPersistQueue(errors)          ← 빌드 큐 저장
    │
    ├─ 12. _cleanUpShutdownThreadPoolForLoad(errors)
    │
    ├─ 13. _cleanUpAwaitDisconnects(errors, pending) ← 연결 해제 대기
    │
    ├─ 14. _cleanUpPluginServletFilters(errors)  ← 서블릿 필터 정리
    │
    ├─ 15. _cleanUpReleaseAllLoggers(errors)     ← 로거 해제
    │
    └─ finally: theInstance = null               ← 싱글턴 해제
```

### 13.3 에이전트 연결 해제

```java
// 3750행
private Set<Future<?>> _cleanUpDisconnectComputers(final List<Throwable> errors) {
    final Set<Future<?>> pending = new HashSet<>();
    // JENKINS-28840: 모든 Computer를 중단할 것이므로 Queue 락을 한 번만 획득
    Queue.withLock(() -> {
        for (Computer c : getComputersCollection()) {
            try {
                c.interrupt();
                c.setNumExecutors(0);
                pending.add(c.disconnect(null));
            } catch (Throwable e) {
                errors.add(e);
            }
        }
    });
    return pending;
}
```

`Queue.withLock()`으로 전체 반복을 한 번의 락 획득으로 처리하는 것은 성능 최적화다. 각 `disconnect()`마다 락을 획득/해제하면 성능이 급격히 저하된다.

### 13.4 에러 수집 패턴

cleanUp의 모든 단계에서 발생하는 에러는 `List<Throwable> errors`에 수집된다. 하나의 단계 실패가 다른 단계의 실행을 방지하지 않는다. 모든 정리 작업이 끝난 후 수집된 에러를 하나의 `RuntimeException`으로 합쳐 전파한다:

```java
// 3661-3675행
if (!errors.isEmpty()) {
    StringBuilder message = new StringBuilder("Unexpected issues encountered during cleanUp: ");
    Iterator<Throwable> iterator = errors.iterator();
    message.append(iterator.next().getMessage());
    while (iterator.hasNext()) {
        message.append("; ");
        message.append(iterator.next().getMessage());
    }
    iterator = errors.iterator();
    RuntimeException exception = new RuntimeException(message.toString(), iterator.next());
    while (iterator.hasNext()) {
        exception.addSuppressed(iterator.next());
    }
    throw exception;
}
```

---

## 14. 설정 리로드

### 14.1 doReload() — HTTP 엔드포인트

```java
// 4400행
@RequirePOST
public synchronized HttpResponse doReload() throws IOException {
    checkPermission(MANAGE);
    getLifecycle().onReload(getAuthentication2().getName(), null);

    // "loading ..." UI 표시
    WebApp.get(getServletContext()).setApp(new HudsonIsLoading());

    // 별도 스레드에서 실제 리로드 수행
    new Thread("Jenkins config reload thread") {
        @Override
        public void run() {
            try (ACLContext ctx = ACL.as2(ACL.SYSTEM2)) {
                reload();
                getLifecycle().onReady();
            } catch (Exception e) {
                LOGGER.log(SEVERE, "Failed to reload Jenkins config", e);
                new JenkinsReloadFailed(e).publish(getServletContext(), root);
            }
        }
    }.start();

    return HttpResponses.redirectViaContextPath("/");
}
```

### 14.2 reload() — 동기 리로드

```java
// 4427행
public void reload() throws IOException, InterruptedException, ReactorException {
    queue.save();                          // 현재 큐 상태 저장
    executeReactor(null, loadTasks());     // 전체 loadTasks 재실행
    // ...
    User.reload();
    queue.load();                          // 큐 복원
    WebApp.get(getServletContext()).setApp(this);  // "loading" UI 해제
}
```

리로드는 `loadTasks()`를 재실행하여 config.xml, 노드, Job을 모두 다시 로드한다. 단, Javadoc에 명시된 것처럼 `ItemListener.onLoaded()`와 `@Initializer`는 호출되지 않는다.

---

## 15. 설계 분석 -- 왜 이렇게 만들었는가

### 15.1 왜 싱글턴인가?

Jenkins의 싱글턴 설계는 다음과 같은 이유에서 비롯된다:

1. **서블릿 컨텍스트와 1:1 매핑**: 하나의 서블릿 컨테이너에 하나의 Jenkins만 존재한다.
2. **URL 루트 바인딩**: Stapler가 루트 객체를 기준으로 URL을 라우팅하므로, 유일한 루트가 필요하다.
3. **전역 상태 관리**: 빌드 큐, 노드 목록, 플러그인 등은 본질적으로 전역 상태다.
4. **레거시 호환**: 수많은 플러그인이 `Jenkins.get()`에 의존하므로, 이 패턴을 변경하면 생태계 전체가 깨진다.

### 15.2 왜 5,990줄인가?

이 클래스가 이렇게 큰 이유는:

1. **God Object 안티패턴**: Jenkins의 역사적 진화 과정에서 점진적으로 기능이 추가되었다.
2. **하위 호환성**: deprecated 메서드를 제거하지 않고 유지한다. `getInstance()` → `getInstanceOrNull()` → `get()` 체인이 그 예다.
3. **Stapler 바인딩**: `doXxx()`, `getXxx()` 메서드가 URL에 직접 매핑되므로, 많은 HTTP 엔드포인트가 이 클래스에 존재한다.
4. **위임 패턴 사용**: `itemGroupMixIn`, `viewGroupMixIn` 등으로 위임하지만, 위임 메서드 자체는 Jenkins 클래스에 남아야 한다.

### 15.3 왜 CopyOnWrite 자료구조를 광범위하게 사용하는가?

```java
CopyOnWriteArrayList<View> views      // View 목록
CopyOnWriteMap.Tree<String, TopLevelItem> items   // Job 맵
CopyOnWriteList<SCMListener> scmListeners         // SCM 리스너
```

Jenkins는 **읽기 >> 쓰기** 비율이 압도적이다:
- 읽기: 모든 HTTP 요청마다 Job 조회, View 렌더링
- 쓰기: Job 생성/삭제/이름변경은 드물게 발생

CopyOnWrite는 읽기에 락이 불필요하므로, 이 패턴에 최적이다. 쓰기 시 전체 복사 비용은 쓰기 빈도가 낮으므로 문제되지 않는다.

### 15.4 왜 volatile과 synchronized를 혼용하는가?

| 필드/메서드 | 동기화 방식 | 이유 |
|------------|------------|------|
| `authorizationStrategy` | volatile | 읽기만 필요, 원자적 참조 교체 |
| `securityRealm` | volatile | 동일 이유 |
| `initLevel` | volatile | 다른 스레드에서 진행 상태 확인 |
| `save()` | synchronized | config.xml 직렬화는 원자적이어야 함 |
| `createProject()` | synchronized | items 맵 변경의 일관성 |
| `loadTasks()` | synchronized | 로드 중 중복 실행 방지 |
| `theInstance` | 없음 | 서블릿 컨테이너의 happens-before 보장에 의존 |

### 15.5 왜 XStream으로 직렬화하는가?

Jenkins는 2004년에 XStream을 선택했다. 그 이유는:
1. Java 객체를 XML로 변환하는 가장 간단한 방법이었다.
2. 어노테이션 없이 기존 클래스를 직렬화할 수 있다.
3. 하위 호환성이 우수하다 (필드 추가/제거에 유연).

`XSTREAM2.addCriticalField()`로 `securityRealm`과 `authorizationStrategy`를 보호하는 것은, 이 필드의 역직렬화 실패가 보안 구멍을 만들 수 있기 때문이다. 예를 들어 `securityRealm` 역직렬화가 실패하여 `null`로 남으면, 인증 없이 Jenkins에 접근 가능해진다.

### 15.6 readResolve()의 역할

```java
// 1058행
protected Object readResolve() {
    if (jdks == null) {
        jdks = new ArrayList<>();
    }
    if (SLAVE_AGENT_PORT_ENFORCE) {
        slaveAgentPort = getSlaveAgentPortInitialValue(slaveAgentPort);
    }
    installStateName = null;
    if (nodeRenameMigrationNeeded == null) {
        nodeRenameMigrationNeeded = true;  // 역직렬화 시 마이그레이션 필요
    }
    _setLabelString(label);
    return this;
}
```

XStream이 `config.xml`에서 Jenkins 객체를 역직렬화한 후 호출되는 콜백이다. 이전 버전의 config.xml에서 누락된 필드를 기본값으로 초기화하는 역할을 한다.

### 15.7 성능 시사점

1. **items 맵 조회 O(log n)**: `CopyOnWriteMap.Tree`는 내부적으로 `TreeMap`을 사용하므로 O(log n)이다. 수천 개의 Job이 있어도 빠르다.
2. **extensionLists/descriptorLists ConcurrentHashMap**: O(1) 평균 접근 시간, 락 분할로 동시성 우수.
3. **View 순회**: `CopyOnWriteArrayList` 순회는 스냅샷 기반이므로 동시 수정에 안전하다.

---

## 요약

Jenkins.java는 약 6,000줄의 God Object이지만, 그 안에 Jenkins 시스템의 핵심 설계 결정이 모두 담겨 있다:

| 측면 | 설계 결정 |
|------|----------|
| 수명주기 | 서블릿 컨테이너가 관리하는 싱글턴 |
| 초기화 | Reactor 패턴으로 TaskGraph 기반 단계별 초기화 |
| 직렬화 | XStream2 기반 XML 직렬화, Critical Field 보호 |
| 동시성 | CopyOnWrite + volatile + synchronized 혼합 |
| 보안 | SecurityRealm(인증) + AuthorizationStrategy(인가) 분리 |
| 확장성 | ExtensionList + Guice DI로 플러그인 확장 |
| URL 매핑 | Stapler 프레임워크의 루트 객체 |
| 종료 | 에러를 수집하면서 모든 정리 단계를 순서대로 실행 |

이 클래스를 이해하면 Jenkins의 모든 하위 시스템이 어떻게 연결되는지 파악할 수 있다.
