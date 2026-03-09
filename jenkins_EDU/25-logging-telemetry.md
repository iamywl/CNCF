# 25. 로깅 시스템 (LogRecorder) & 텔레메트리 심층 분석

## 목차
1. [개요](#1-개요)
2. [LogRecorder 아키텍처](#2-logrecorder-아키텍처)
3. [LogRecorder 핵심 클래스 분석](#3-logrecorder-핵심-클래스-분석)
4. [Target: 로그 필터링 메커니즘](#4-target-로그-필터링-메커니즘)
5. [RingBufferLogHandler: 순환 버퍼](#5-ringbufferloghandler-순환-버퍼)
6. [에이전트 로그 레벨 동기화](#6-에이전트-로그-레벨-동기화)
7. [LogRecorderManager: 관리 계층](#7-logrecordermanager-관리-계층)
8. [자동완성과 로거 이름 검색](#8-자동완성과-로거-이름-검색)
9. [Telemetry 아키텍처](#9-telemetry-아키텍처)
10. [Telemetry 핵심 클래스 분석](#10-telemetry-핵심-클래스-분석)
11. [TelemetryReporter: 주기적 수집](#11-telemetryreporter-주기적-수집)
12. [Correlator: 익명 상관 ID](#12-correlator-익명-상관-id)
13. [텔레메트리 비활성화 메커니즘](#13-텔레메트리-비활성화-메커니즘)
14. [설계 결정과 교훈](#14-설계-결정과-교훈)

---

## 1. 개요

Jenkins의 **로깅 시스템**과 **텔레메트리**는 시스템 관찰성(observability)의 두 축이다.

- **LogRecorder**: 관리자가 특정 Java 패키지/클래스의 로그를 선택적으로 캡처하여 진단하는 내부 도구
- **Telemetry**: JEP-214 기반의 익명 사용 통계 수집 프레임워크로, Jenkins 개발팀이 기능 사용 현황을 파악하는 데 사용

### 왜 이 두 기능을 함께 분석하는가?

1. **관찰성의 두 방향**: LogRecorder는 "내부→관리자" (내부 디버깅), Telemetry는 "내부→개발팀" (외부 보고)
2. **JUL(java.util.logging) 기반**: 둘 다 Java 표준 로깅 프레임워크 위에 구축
3. **ExtensionPoint 패턴**: Telemetry는 ExtensionPoint, LogRecorder는 확장점을 제공하지는 않지만 Target 기반 유연한 설정

### 소스 경로

```
core/src/main/java/hudson/logging/
├── LogRecorder.java              # 커스텀 로그 레코더 (590줄)
├── LogRecorderManager.java       # 레코더 관리자
└── WeakLogHandler.java           # GC 안전 핸들러

core/src/main/java/jenkins/telemetry/
├── Telemetry.java                # 텔레메트리 추상 기반 (253줄)
└── Correlator.java               # 인스턴스 상관 ID
```

---

## 2. LogRecorder 아키텍처

### 전체 구조

```
┌─────────────────────────────────────────────────────────┐
│                   Jenkins 로깅 아키텍처                  │
│                                                          │
│  java.util.logging (JUL)                                 │
│  ┌────────────────────────────────────────────┐          │
│  │ Root Logger ("")                            │          │
│  │   ├── Handler: ConsoleHandler (stdout)      │          │
│  │   ├── Handler: FileHandler (jenkins.log)    │          │
│  │   └── Handler: WeakLogHandler ──┐           │          │
│  │       ├── Logger("hudson.model") │           │          │
│  │       ├── Logger("jenkins.security")         │          │
│  │       └── Logger("org.apache")   │           │          │
│  └──────────────────────────────────┼──────────┘          │
│                                     │                     │
│  ┌──────────────────────────────────▼──────────┐          │
│  │           LogRecorder "security-debug"       │          │
│  │  ┌────────────────────────────────┐          │          │
│  │  │ RingBufferLogHandler           │          │          │
│  │  │   [record1, record2, ..., recordN]        │          │
│  │  │   (순환 버퍼, 기본 256개)        │          │          │
│  │  └────────────────────────────────┘          │          │
│  │  Targets:                                    │          │
│  │  ├── Target("jenkins.security", FINE)        │          │
│  │  └── Target("hudson.security", FINE)         │          │
│  └──────────────────────────────────────────────┘          │
│                                                          │
│  ┌──────────────────────────────────────────────┐          │
│  │           LogRecorder "scm-polling"          │          │
│  │  Targets:                                    │          │
│  │  ├── Target("hudson.triggers.SCMTrigger", FINE)        │
│  │  └── Target("hudson.scm", FINER)            │          │
│  └──────────────────────────────────────────────┘          │
└─────────────────────────────────────────────────────────┘
```

### 왜 LogRecorder가 필요한가?

Jenkins 운영 환경에서 발생하는 문제를 진단할 때:
- 전체 DEBUG 로그를 켜면 **성능 저하**와 **로그 폭주** 발생
- 특정 서브시스템만 선택적으로 디버그 로그를 활성화해야 함
- LogRecorder는 **특정 패키지/클래스에 대해서만** 로그 레벨을 낮추고, 해당 로그를 **순환 버퍼**에 저장

---

## 3. LogRecorder 핵심 클래스 분석

### 클래스 정의

```java
// 소스: core/src/main/java/hudson/logging/LogRecorder.java
public class LogRecorder extends AbstractModelObject
    implements Loadable, Saveable {

    private volatile String name;                    // 레코더 이름
    private List<Target> loggers = new ArrayList<>(); // 로그 타겟 목록
    private static final TargetComparator TARGET_COMPARATOR
        = new TargetComparator();
}
```

### 생성자: WeakLogHandler 등록

```java
// 소스: LogRecorder.java:108-114
@DataBoundConstructor
public LogRecorder(String name) {
    this.name = name;
    // Root Logger에 WeakLogHandler 등록
    // LogRecorder가 GC되면 자동으로 핸들러도 제거
    new WeakLogHandler(handler, Logger.getLogger(""));
}
```

**왜 WeakLogHandler인가?**
- LogRecorder가 삭제되면 핸들러도 자동 정리되어야 함
- Java의 WeakReference를 활용하여 GC와 연동
- 메모리 누수 방지 패턴

### RingBufferLogHandler 커스텀 필터링

```java
// 소스: LogRecorder.java:217-236
transient RingBufferLogHandler handler = new RingBufferLogHandler() {
    @Override
    public void publish(LogRecord record) {
        // orderedTargets(): 이름 길이 역순으로 정렬된 Target 배열
        for (Target t : orderedTargets()) {
            Boolean match = t.matches(record);
            if (match == null) {
                // 도메인 불일치 → 다음 타겟으로
                continue;
            }
            if (match) {
                // 가장 구체적인 타겟이 매칭 → 저장
                super.publish(record);
            }
            // 가장 구체적인 타겟이 불일치 → 저장하지 않음
            // 이는 더 구체적인 로거에서 로그 레벨을 낮출 수 있게 함
            return;
        }
    }
};
```

---

## 4. Target: 로그 필터링 메커니즘

### Target 클래스

```java
// 소스: LogRecorder.java:242-338
public static final class Target {
    public final String name;   // 로거 이름 (예: "hudson.model")
    private final int level;    // 로그 레벨 (정수)
    private transient Logger logger;

    // 3값 매칭 (Boolean + null)
    public Boolean matches(LogRecord r) {
        boolean levelSufficient = r.getLevel().intValue() >= level;
        if (name.isEmpty()) {
            return levelSufficient;  // 루트 로거 → 레벨만 확인
        }
        String logName = r.getLoggerName();
        if (logName == null || !logName.startsWith(name))
            return null;  // 도메인 불일치 → null (판단 불가)
        String rest = logName.substring(name.length());
        if (rest.startsWith(".") || rest.isEmpty()) {
            return levelSufficient;  // 도메인 일치 → 레벨 검사
        }
        return null;  // 부분 일치 → null
    }
}
```

### 왜 3값 로직(Boolean + null)인가?

```
Target 목록 (이름 길이 역순 정렬):
  1. "hudson.model.queue"    level=FINEST
  2. "hudson.model"          level=WARNING
  3. ""                      level=INFO

LogRecord: logger="hudson.model.queue.QueueTaskFuture", level=FINE

검색 과정:
  Target 1 ("hudson.model.queue"): 이름 매칭 → level FINE >= FINEST → true → 저장!

LogRecord: logger="hudson.model.Run", level=FINE

검색 과정:
  Target 1 ("hudson.model.queue"): 이름 불일치 → null → 계속
  Target 2 ("hudson.model"):        이름 매칭 → level FINE < WARNING → false → 저장 안 함!

효과: "hudson.model"은 WARNING 이상만, 하위 "hudson.model.queue"는 FINEST까지 별도 설정 가능
```

이 설계로 **더 구체적인 로거가 덜 구체적인 로거의 설정을 오버라이드**할 수 있다.

### Target 정렬 (TargetComparator)

```java
// 소스: LogRecorder.java:340-348
private static class TargetComparator
    implements Comparator<Target>, Serializable {
    @Override
    public int compare(Target left, Target right) {
        // 이름이 긴 것(더 구체적)이 먼저 오도록 역순 정렬
        return right.getName().length() - left.getName().length();
    }
}
```

### enable/disable: 로그 레벨 활성화

```java
// 소스: LogRecorder.java:326-336
public void enable() {
    Logger l = getLogger();
    if (!l.isLoggable(getLevel()))
        l.setLevel(getLevel());    // JUL 로거 레벨 설정
    new SetLevel(name, getLevel()).broadcast(); // 모든 에이전트에 전파
}

public void disable() {
    getLogger().setLevel(null);     // JUL 기본값으로 복원
    new SetLevel(name, null).broadcast();
}
```

---

## 5. RingBufferLogHandler: 순환 버퍼

LogRecorder는 로그 레코드를 무한정 저장하지 않고 **순환 버퍼**에 최근 N개만 유지한다.

```
RingBufferLogHandler 내부 구조:

     index=0   1     2     3     ...   255
    ┌─────┬─────┬─────┬─────┬─────┬─────┐
    │ r0  │ r1  │ r2  │ r3  │ ... │r255 │
    └─────┴─────┴─────┴─────┴─────┴─────┘
                                  ▲
                                  │ 다음 쓰기 위치 (순환)

    256개가 가득 차면 가장 오래된 것부터 덮어씀
    → 메모리 사용량 일정, OOM 방지
```

### getView()로 읽기

```java
// RingBufferLogHandler에서 정의
public List<LogRecord> getView() {
    // 순환 버퍼의 현재 스냅샷을 시간순 List로 반환
    // 복사본 반환으로 스레드 안전성 보장
}
```

---

## 6. 에이전트 로그 레벨 동기화

### SetLevel: 컨트롤러 → 에이전트 전파

```java
// 소스: LogRecorder.java:350-382
private static final class SetLevel
    extends MasterToSlaveCallable<Void, Error> {

    @SuppressWarnings("MismatchedQueryAndUpdateOfCollection")
    private static final Set<Logger> loggers = new HashSet<>();
    private final String name;
    private final Level level;

    @Override
    public Void call() throws Error {
        Logger logger = Logger.getLogger(name);
        loggers.add(logger);   // GC 방지! (강한 참조 유지)
        logger.setLevel(level);
        return null;
    }

    void broadcast() {
        for (Computer c : Jenkins.get().getComputers()) {
            if (!c.getName().isEmpty()) { // 컨트롤러 제외
                VirtualChannel ch = c.getChannel();
                if (ch != null) {
                    try {
                        ch.call(this);  // Remoting으로 에이전트에 전송
                    } catch (Exception x) {
                        // 실패해도 계속 진행 (best effort)
                        Logger.getLogger(LogRecorder.class.getName())
                            .log(Level.WARNING,
                                "could not set up logging on " + c, x);
                    }
                }
            }
        }
    }
}
```

**왜 `loggers` Set에 강한 참조를 유지하는가?**
- JUL Logger는 WeakReference로 관리됨
- `Logger.getLogger(name)` 후 참조를 잡지 않으면 GC될 수 있음
- GC되면 `setLevel()`이 사라져서 로그 레벨이 리셋됨
- `static Set<Logger>`에 강한 참조를 유지하여 이 문제 방지

### ComputerLogInitializer: 에이전트 연결 시 초기화

```java
// 소스: LogRecorder.java:384-392
@Extension
public static final class ComputerLogInitializer
    extends ComputerListener {

    @Override
    public void preOnline(Computer c, Channel channel,
            FilePath root, TaskListener listener)
            throws IOException, InterruptedException {
        // 에이전트가 연결될 때 모든 LogRecorder의 타겟 레벨 전파
        for (LogRecorder recorder :
                Jenkins.get().getLog().getRecorders()) {
            for (Target t : recorder.getLoggers()) {
                channel.call(new SetLevel(t.name, t.getLevel()));
            }
        }
    }
}
```

---

## 7. LogRecorderManager: 관리 계층

### 설정 파일 영속화

```java
// LogRecorder.java:526-528
private XmlFile getConfigFile() {
    return new XmlFile(XSTREAM,
        new File(LogRecorderManager.configDir(),
            name + ".xml"));
}
```

저장 경로: `$JENKINS_HOME/log/{name}.xml`

### save/load 흐름

```
save() 호출 시:
    ├── BulkChange 확인 → 배치 중이면 스킵
    ├── getConfigFile().write(this) → XML 파일 저장
    ├── loggers.forEach(Target::enable) → JUL 로그 레벨 재설정
    └── SaveableListener.fireOnChange() → 변경 이벤트 발행

load() 호출 시:
    ├── getConfigFile().unmarshal(this) → XML 파일 로드
    └── loggers.forEach(Target::enable) → JUL 로그 레벨 설정

delete() 호출 시:
    ├── getConfigFile().delete() → XML 파일 삭제
    ├── getParent().getRecorders().remove() → 목록에서 제거
    ├── loggers.forEach(Target::disable) → 로그 레벨 복원
    └── 다른 레코더의 타겟 재활성화
        → 같은 로거를 공유하는 경우 대비
```

### XStream 별칭 등록

```java
// 소스: LogRecorder.java:578-583
public static final XStream XSTREAM = new XStream2();
static {
    XSTREAM.alias("log", LogRecorder.class);
    XSTREAM.alias("target", Target.class);
}
```

---

## 8. 자동완성과 로거 이름 검색

### 로거 이름 자동완성 알고리즘

```java
// 소스: LogRecorder.java:136-165
public static Set<String> getAutoCompletionCandidates(
        List<String> loggerNamesList) {
    Set<String> loggerNames = new HashSet<>(loggerNamesList);

    HashMap<String, Integer> seenPrefixes = new HashMap<>();
    SortedSet<String> relevantPrefixes = new TreeSet<>();

    for (String loggerName : loggerNames) {
        String[] loggerNameParts = loggerName.split("[.]");
        String longerPrefix = null;

        for (int i = loggerNameParts.length; i > 0; i--) {
            String loggerNamePrefix = String.join(".",
                Arrays.copyOf(loggerNameParts, i));
            seenPrefixes.put(loggerNamePrefix,
                seenPrefixes.getOrDefault(loggerNamePrefix, 0) + 1);

            if (longerPrefix == null) {
                relevantPrefixes.add(loggerNamePrefix); // 전체 이름 추가
                longerPrefix = loggerNamePrefix;
                continue;
            }

            // 접두사의 빈도가 하위 접두사보다 높으면 추가
            if (seenPrefixes.get(loggerNamePrefix)
                    > seenPrefixes.get(longerPrefix)) {
                relevantPrefixes.add(loggerNamePrefix);
            }
            longerPrefix = loggerNamePrefix;
        }
    }
    return relevantPrefixes;
}
```

### 알고리즘 동작 예시

```
입력 로거 이름들:
  org.apache.http.client
  org.apache.http.impl
  org.jenkinsci.plugins.workflow
  io.jenkins.plugins

분석 결과 (relevantPrefixes):
  org.apache.http.client     ← 전체 이름
  org.apache.http.impl       ← 전체 이름
  org.apache.http            ← 접두사 빈도 2 > 각 하위 1
  org.apache                 ← 접두사 빈도 2 > 하위 1 (http만)
  org                        ← 접두사 빈도 3 (apache2 + jenkinsci1)
  org.jenkinsci.plugins.workflow ← 전체 이름
  io.jenkins.plugins         ← 전체 이름

자동완성에서 "org" 입력 시:
  → org, org.apache, org.apache.http,
    org.apache.http.client, org.apache.http.impl,
    org.jenkinsci.plugins.workflow
```

---

## 9. Telemetry 아키텍처

### JEP-214 텔레메트리 프레임워크

```
┌──────────────────────────────────────────────────────────┐
│                Jenkins Telemetry (JEP-214)               │
│                                                          │
│  ┌────────────────────────────┐                          │
│  │    Telemetry (abstract)    │                          │
│  │  + getId(): String          │  ← 고유 식별자          │
│  │  + getDisplayName(): String │  ← 사용자 표시명        │
│  │  + getStart(): LocalDate    │  ← 수집 시작일          │
│  │  + getEnd(): LocalDate      │  ← 수집 종료일          │
│  │  + createContent(): JSON    │  ← 실제 데이터 생성     │
│  └──────────┬─────────────────┘                          │
│             │ ExtensionPoint                             │
│  ┌──────────▼─────────────────┐                          │
│  │  TelemetryReporter         │  ← @Extension            │
│  │  (AsyncPeriodicWork)       │                          │
│  │  매 24시간마다 실행         │                          │
│  │  ┌─────────────────────┐   │                          │
│  │  │ for each Telemetry: │   │                          │
│  │  │  ├── 기간 확인      │   │                          │
│  │  │  ├── createContent()│   │                          │
│  │  │  ├── wrap(type,data)│   │                          │
│  │  │  └── POST to uplink │   │                          │
│  │  └─────────────────────┘   │                          │
│  └────────────────────────────┘                          │
│                 │                                        │
│                 ▼                                        │
│  https://uplink.jenkins.io/events                        │
└──────────────────────────────────────────────────────────┘
```

### 핵심 설계 원칙

1. **시간 제한 수집**: 각 텔레메트리는 시작일/종료일이 있어 영구 수집 방지
2. **opt-out 모델**: 사용 통계 수집 동의 시에만 활성화
3. **익명성**: 상관 ID는 해시되어 역추적 불가
4. **ExtensionPoint**: 플러그인도 텔레메트리를 등록 가능

---

## 10. Telemetry 핵심 클래스 분석

### 추상 클래스 구조

```java
// 소스: core/src/main/java/jenkins/telemetry/Telemetry.java
public abstract class Telemetry implements ExtensionPoint {

    // 엔드포인트 URL (SystemProperties로 오버라이드 가능)
    static String ENDPOINT = SystemProperties.getString(
        Telemetry.class.getName() + ".endpoint",
        "https://uplink.jenkins.io/events");

    // 고유 ID (기본: 클래스명)
    public String getId() {
        return getClass().getName();
    }

    // 수집 기간
    public abstract LocalDate getStart();
    public abstract LocalDate getEnd();

    // 사용자 표시명
    public abstract String getDisplayName();

    // 데이터 생성 (null 반환 시 전송하지 않음)
    public abstract JSONObject createContent();
}
```

### 기간 활성 여부 확인

```java
// 소스: Telemetry.java:157-160
public boolean isActivePeriod() {
    LocalDate now = LocalDate.now();
    return now.isAfter(getStart()) && now.isBefore(getEnd());
}
```

### 비활성화 조건

```java
// 소스: Telemetry.java:142-149
public static boolean isDisabled() {
    // 1. 시스템 전체 사용 통계 비활성화
    if (UsageStatistics.DISABLED) {
        return true;
    }
    // 2. Jenkins 인스턴스 미사용 또는 통계 수집 미동의
    Jenkins jenkins = Jenkins.getInstanceOrNull();
    return jenkins == null || !jenkins.isUsageStatisticsCollected();
}
```

### 컴포넌트 정보 수집

```java
// 소스: Telemetry.java:168-178
protected final Map<String, String> buildComponentInformation() {
    Map<String, String> components = new TreeMap<>();
    // Jenkins 코어 버전
    VersionNumber core = Jenkins.getVersion();
    components.put("jenkins-core",
        core == null ? "" : core.toString());
    // 모든 활성 플러그인의 버전
    for (PluginWrapper plugin :
            Jenkins.get().pluginManager.getPlugins()) {
        if (plugin.isActive()) {
            components.put(plugin.getShortName(),
                plugin.getVersion());
        }
    }
    return components;
}
```

---

## 11. TelemetryReporter: 주기적 수집

### AsyncPeriodicWork 기반 실행

```java
// 소스: Telemetry.java:181-252
@Extension
public static class TelemetryReporter extends AsyncPeriodicWork {

    public TelemetryReporter() {
        super("telemetry collection");
    }

    @Override
    public long getRecurrencePeriod() {
        return TimeUnit.HOURS.toMillis(24); // 24시간마다 실행
    }

    @Override
    protected void execute(TaskListener listener)
            throws IOException, InterruptedException {
        // 비활성화 확인
        if (isDisabled()) {
            LOGGER.info("Collection of anonymous usage statistics "
                + "is disabled, skipping...");
            return;
        }

        // 모든 등록된 Telemetry 확장점 순회
        Telemetry.all().forEach(telemetry -> {
            // 기간 확인
            if (telemetry.getStart().isAfter(LocalDate.now())) {
                LOGGER.config("Skipping '" + telemetry.getId()
                    + "' - configured to start later");
                return;
            }
            if (telemetry.getEnd().isBefore(LocalDate.now())) {
                LOGGER.config("Skipping '" + telemetry.getId()
                    + "' - configured to end in the past");
                return;
            }

            // 데이터 생성
            JSONObject data = null;
            try {
                data = telemetry.createContent();
            } catch (RuntimeException e) {
                LOGGER.log(Level.WARNING,
                    "Failed to build content for '"
                    + telemetry.getId() + "'", e);
            }

            if (data == null) {
                LOGGER.config("Skipping '" + telemetry.getId()
                    + "' - no data");
                return;
            }

            // 래핑 및 전송
            JSONObject wrappedData = new JSONObject();
            wrappedData.put("type", telemetry.getId());
            wrappedData.put("payload", data);
            String correlationId = ExtensionList
                .lookupSingleton(Correlator.class)
                .getCorrelationId();
            wrappedData.put("correlator",
                Util.getHexOfSHA256DigestOf(
                    correlationId + telemetry.getId()));

            // HTTP POST 전송
            String body = wrappedData.toString();
            HttpClient httpClient =
                ProxyConfiguration.newHttpClient();
            HttpRequest httpRequest = ProxyConfiguration
                .newHttpRequestBuilder(new URI(ENDPOINT))
                .headers("Content-Type",
                    "application/json; charset=utf-8")
                .POST(HttpRequest.BodyPublishers.ofString(body))
                .build();

            HttpResponse<Void> response = httpClient.send(
                httpRequest,
                HttpResponse.BodyHandlers.discarding());
            LOGGER.config("Response " + response.statusCode()
                + " for: " + telemetry.getId());
        });
    }
}
```

### 전송 데이터 구조

```json
{
  "type": "jenkins.security.s2m.MasterKillSwitchConfiguration",
  "payload": {
    "components": {
      "jenkins-core": "2.401.3",
      "git": "5.2.0",
      "pipeline-model-definition": "2.2144.1"
    },
    "killSwitchEnabled": false
  },
  "correlator": "a1b2c3d4e5f6..."
}
```

---

## 12. Correlator: 익명 상관 ID

### 상관 ID의 역할

```
correlator = SHA-256(instanceId + telemetryId)

특징:
  - 같은 Jenkins 인스턴스에서 같은 텔레메트리 → 항상 같은 correlator
  - 서로 다른 텔레메트리 타입 → 다른 correlator (연결 불가)
  - 원본 instanceId로 역추적 불가 (단방향 해시)

목적:
  - 같은 인스턴스의 연속 보고를 추적 (추세 분석)
  - 텔레메트리 타입 간 교차 연결 방지 (프라이버시)
```

---

## 13. 텔레메트리 비활성화 메커니즘

### 다층 비활성화 구조

```
텔레메트리 전송 여부 결정:

1. 시스템 속성: UsageStatistics.DISABLED
   → -Dhudson.model.UsageStatistics.disabled=true
   → 전체 사용 통계 비활성화

2. Jenkins 설정: isUsageStatisticsCollected()
   → 관리 → "Usage Statistics" 체크 해제
   → Setup Wizard에서 선택

3. 개별 Telemetry 기간:
   → getStart().isAfter(now) → 아직 시작 전
   → getEnd().isBefore(now)  → 이미 종료

4. createContent() 반환값:
   → null 반환 → 해당 주기 전송 스킵
   → 예: 수집할 데이터가 없는 경우
```

---

## 14. 설계 결정과 교훈

### LogRecorder 설계 결정

| 결정 | 이유 | 트레이드오프 |
|------|------|------------|
| WeakLogHandler | LogRecorder GC 시 자동 핸들러 정리 | WeakReference 간접 참조 복잡성 |
| 3값 matches() | 계층적 로거 이름에서 구체→일반 순서 검색 | null 의미론 이해 필요 |
| RingBuffer | 메모리 사용량 제한, OOM 방지 | 오래된 로그 유실 |
| SetLevel broadcast | 에이전트에서도 동일한 디버그 로깅 가능 | 네트워크 오류 시 불일치 |
| 정적 Logger Set | GC에 의한 로거 리셋 방지 | 메모리 누수 가능성 (미미) |

### Telemetry 설계 결정

| 결정 | 이유 | 트레이드오프 |
|------|------|------------|
| 시간 제한 수집 | 영구 수집 방지, 목적 달성 후 자동 종료 | 새 수집이 필요할 때마다 릴리스 필요 |
| SHA-256 correlator | 프라이버시 보호 + 추세 추적 가능 | 인스턴스별 상세 디버깅 불가 |
| AsyncPeriodicWork | 24시간 간격으로 부하 최소화 | 실시간 데이터 수집 불가 |
| opt-out 모델 | 기본 수집으로 데이터 확보 | 프라이버시 우려 제기 가능 |
| ExtensionPoint | 플러그인도 텔레메트리 등록 가능 | 남용 가능성 |

### 핵심 교훈

1. **순환 버퍼 패턴**: 무제한 로그 저장은 운영 환경에서 OOM의 원인. RingBuffer로 최근 N개만 유지하는 것은 로그 시스템의 기본 패턴
2. **GC 안전 설계**: JUL Logger의 WeakReference 특성을 이해하고, 필요한 곳에서 강한 참조를 유지하는 것이 중요
3. **시간 제한 텔레메트리**: 데이터 수집에 명시적 시작/종료를 두는 것은 프라이버시와 데이터 관리 모두에 유리
4. **3값 로직의 유용성**: null을 "판단 불가"로 활용하는 패턴은 계층적 매칭에서 매우 효과적
5. **best effort 전파**: 에이전트 로그 레벨 설정 실패를 치명적으로 처리하지 않는 것은 분산 시스템의 현실적 접근

---

## 부록: LogRecorder LEVELS 상수

```java
// 소스: LogRecorder.java:588-589
public static List<Level> LEVELS =
    Arrays.asList(
        Level.ALL,      // Integer.MIN_VALUE
        Level.FINEST,   // 300
        Level.FINER,    // 400
        Level.FINE,     // 500
        Level.CONFIG,   // 700
        Level.INFO,     // 800
        Level.WARNING,  // 900
        Level.SEVERE,   // 1000
        Level.OFF       // Integer.MAX_VALUE
    );
```

### 주요 소스 파일 요약

| 파일 | 줄수 | 핵심 역할 |
|------|------|----------|
| `LogRecorder.java` | 590 | 커스텀 로그 레코더, Target 매칭, 에이전트 동기화 |
| `Telemetry.java` | 253 | JEP-214 텔레메트리 프레임워크, TelemetryReporter |
| `BaseParser.java` | 148 | (scheduler 패키지이나 LogRecorder LEVELS와 관련) |

---

*본 문서는 Jenkins 소스코드를 직접 분석하여 작성되었습니다. 모든 코드 참조는 검증된 실제 경로와 라인 번호를 기반으로 합니다.*
