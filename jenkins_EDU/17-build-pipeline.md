# 17. 빌드 파이프라인 (Build Pipeline)

## 목차

1. [개요](#1-개요)
2. [핵심 인터페이스: BuildStep](#2-핵심-인터페이스-buildstep)
3. [BuildStepMonitor와 동시성 제어](#3-buildstepmonitor와-동시성-제어)
4. [CheckPoint: 동시 빌드 간 동기화](#4-checkpoint-동시-빌드-간-동기화)
5. [BuildStepCompatibilityLayer: 하위 호환성 레이어](#5-buildstepcompatibilitylayer-하위-호환성-레이어)
6. [Builder: 빌드 단계 실행기](#6-builder-빌드-단계-실행기)
7. [Publisher: 빌드 후 처리 프레임워크](#7-publisher-빌드-후-처리-프레임워크)
8. [Recorder vs Notifier: Publisher의 두 얼굴](#8-recorder-vs-notifier-publisher의-두-얼굴)
9. [BuildWrapper: 빌드 환경 래퍼](#9-buildwrapper-빌드-환경-래퍼)
10. [Trigger: 빌드 트리거 시스템](#10-trigger-빌드-트리거-시스템)
11. [SimpleBuildStep: 현대적 빌드 스텝 API](#11-simplebuildstep-현대적-빌드-스텝-api)
12. [빌드 실행 순서: 전체 파이프라인 흐름](#12-빌드-실행-순서-전체-파이프라인-흐름)
13. [BuildStepDescriptor와 확장점 등록](#13-buildstepdescriptor와-확장점-등록)
14. [Publisher 정렬 메커니즘](#14-publisher-정렬-메커니즘)
15. [설계 결정과 Why](#15-설계-결정과-why)

---

## 1. 개요

Jenkins의 빌드 파이프라인은 하나의 빌드가 시작부터 완료까지 거치는 일련의 단계를 정의한다. Freestyle 프로젝트 기준으로, 이 파이프라인은 크게 다섯 가지 핵심 개념으로 구성된다.

```
Trigger → BuildWrapper → Builder → Publisher(Recorder → Notifier) → tearDown
```

| 컴포넌트 | 역할 | 소스 위치 |
|---------|------|---------|
| `BuildStep` | 빌드의 한 단계를 나타내는 핵심 인터페이스 | `core/src/main/java/hudson/tasks/BuildStep.java` |
| `Builder` | 실제 빌드 작업 수행 (컴파일, 테스트 등) | `core/src/main/java/hudson/tasks/Builder.java` |
| `Publisher` | 빌드 후 처리 (결과 수집, 알림 등) | `core/src/main/java/hudson/tasks/Publisher.java` |
| `Recorder` | 빌드 결과에 영향을 주는 Publisher | `core/src/main/java/hudson/tasks/Recorder.java` |
| `Notifier` | 빌드 결과를 알리는 Publisher | `core/src/main/java/hudson/tasks/Notifier.java` |
| `BuildWrapper` | 빌드 환경 설정/정리 (setUp/tearDown) | `core/src/main/java/hudson/tasks/BuildWrapper.java` |
| `Trigger` | 빌드 시작 조건 (cron, SCM 변경 등) | `core/src/main/java/hudson/triggers/Trigger.java` |
| `BuildStepMonitor` | 동시 빌드 간 동기화 수준 지정 | `core/src/main/java/hudson/tasks/BuildStepMonitor.java` |
| `CheckPoint` | 동시 빌드 간 세밀한 동기화 지점 | `core/src/main/java/hudson/model/CheckPoint.java` |

이 모든 컴포넌트는 Jenkins의 `ExtensionPoint` 패턴을 통해 플러그인이 자유롭게 확장할 수 있다.

---

## 2. 핵심 인터페이스: BuildStep

`BuildStep`은 빌드 프로세스의 한 단계를 나타내는 최상위 인터페이스이다. `Builder`, `Publisher` 등 모든 빌드 단계가 이 인터페이스를 구현한다.

### 2.1 인터페이스 정의

```
소스: core/src/main/java/hudson/tasks/BuildStep.java
```

```java
public interface BuildStep {

    boolean prebuild(AbstractBuild<?, ?> build, BuildListener listener);

    boolean perform(AbstractBuild<?, ?> build, Launcher launcher, BuildListener listener)
        throws InterruptedException, IOException;

    @Deprecated
    Action getProjectAction(AbstractProject<?, ?> project);

    @NonNull
    Collection<? extends Action> getProjectActions(AbstractProject<?, ?> project);

    default BuildStepMonitor getRequiredMonitorService() {
        return BuildStepMonitor.BUILD;
    }
}
```

### 2.2 메서드별 상세 분석

#### prebuild(AbstractBuild, BuildListener)

빌드가 시작되기 전에 실행되는 사전 검증 단계이다.

```
+-------------------+
| prebuild() 호출    |
+-------------------+
         |
    true 반환? ──── false → 빌드 중단 (AbortException 권장)
         |
         v
  빌드 계속 진행
```

- **반환값**: `true`이면 빌드 계속, `false`이면 중단
- **권장 패턴**: `false` 반환 대신 `AbortException`을 던지는 것이 권장됨 (소스 주석에 명시)
- **용도**: 빌드 실행 전 선결 조건 검증, 파라미터 유효성 확인 등

소스 코드 주석에서 직접 확인한 내용:

```java
// BuildStep.java 88~90행
// Using the return value to indicate success/failure should
// be considered deprecated, and implementations are encouraged
// to throw AbortException to indicate a failure.
```

#### perform(AbstractBuild, Launcher, BuildListener)

빌드 단계의 핵심 실행 메서드이다. 실제 빌드 작업이 여기서 수행된다.

```java
// BuildStep.java 129행
boolean perform(AbstractBuild<?, ?> build, Launcher launcher, BuildListener listener)
    throws InterruptedException, IOException;
```

주요 매개변수:

| 매개변수 | 타입 | 설명 |
|---------|------|------|
| `build` | `AbstractBuild<?,?>` | 현재 진행 중인 빌드 객체 |
| `launcher` | `Launcher` | 프로세스 실행기 (원격 에이전트 포함) |
| `listener` | `BuildListener` | 빌드 출력 리스너 (콘솔 로그) |

**보안 관련 중요 사항** (소스 주석에서 확인):

```java
// BuildStep.java 101~107행
// When this build step needs to make permission checks to ACL,
// the implementation should check whether Jenkins.getAuthentication2()
// is ACL.SYSTEM2, and if so, replace it for the duration of this step
// with Jenkins.ANONYMOUS.
```

`perform()` 메서드가 ACL 권한 검사를 수행할 때, `QueueItemAuthenticator`가 설정되지 않은 경우 `SYSTEM` 인증 대신 `ANONYMOUS` 인증으로 대체해야 한다. 이는 보안상 중요한 설계 결정이다.

#### getProjectActions(AbstractProject)

프로젝트 페이지에 Action을 기여하는 메서드이다.

```java
// BuildStep.java 159~160행
@NonNull
Collection<? extends Action> getProjectActions(AbstractProject<?, ?> project);
```

- 프로젝트 렌더링 시 매번 호출됨
- `jobMain.jelly` 뷰를 통해 프로젝트 메인 패널에 섹션 추가 가능
- JUnit 플러그인이 테스트 리포트를 빌드 페이지에 첨부하는 것이 대표적 사례

#### getRequiredMonitorService()

동시성 제어 수준을 지정하는 메서드이다. Jenkins 1.319에서 도입되었다.

```java
// BuildStep.java 220~222행
default BuildStepMonitor getRequiredMonitorService() {
    return BuildStepMonitor.BUILD;
}
```

기본값이 `BUILD`(가장 보수적)인 이유: 1.319 이전 플러그인과의 하위 호환성을 위해서이다. 이전 버전의 Jenkins는 동일 잡의 빌드를 병렬로 실행하지 않았으므로, 기존 플러그인이 이전 빌드 결과를 참조하는 코드를 안전하게 보호한다.

### 2.3 영속성(Persistence) 모델

소스 코드 Javadoc에서 확인한 핵심 특성:

```
소스: BuildStep.java 57~75행

1. XStream으로 직렬화 — Project의 일부로 저장
2. 생성자 없이 복원 (Java 직렬화 방식과 동일)
3. 설정 데이터만 인스턴스 변수로 유지
4. 처리용 객체는 transient 마킹 필수 → null 체크 필요
5. 사용자가 잡 설정을 저장하면 인스턴스 생성 → 설정 덮어쓸 때까지 메모리에 유지
```

---

## 3. BuildStepMonitor와 동시성 제어

### 3.1 enum 정의

```
소스: core/src/main/java/hudson/tasks/BuildStepMonitor.java
```

`BuildStepMonitor`는 동시 빌드 환경에서 BuildStep의 동기화 수준을 결정하는 enum이다.

```java
public enum BuildStepMonitor {
    NONE {
        public boolean perform(BuildStep bs, AbstractBuild build,
                Launcher launcher, BuildListener listener)
                throws IOException, InterruptedException {
            return bs.perform(build, launcher, listener);
        }
    },
    STEP {
        public boolean perform(BuildStep bs, AbstractBuild build,
                Launcher launcher, BuildListener listener)
                throws InterruptedException, IOException {
            CheckPoint cp = new CheckPoint(bs.getClass().getName(), bs.getClass());
            if (bs instanceof Describable) {
                cp.block(listener, ((Describable) bs).getDescriptor().getDisplayName());
            } else {
                cp.block();
            }
            try {
                return bs.perform(build, launcher, listener);
            } finally {
                cp.report();
            }
        }
    },
    BUILD {
        public boolean perform(BuildStep bs, AbstractBuild build,
                Launcher launcher, BuildListener listener)
                throws IOException, InterruptedException {
            if (bs instanceof Describable) {
                CheckPoint.COMPLETED.block(listener,
                    ((Describable) bs).getDescriptor().getDisplayName());
            } else {
                CheckPoint.COMPLETED.block();
            }
            return bs.perform(build, launcher, listener);
        }
    };

    public abstract boolean perform(BuildStep bs, AbstractBuild build,
        Launcher launcher, BuildListener listener)
        throws IOException, InterruptedException;
}
```

### 3.2 세 가지 동기화 수준 비교

```
                    Build #1                 Build #2
                    --------                 --------

NONE:           [Step A][Step B]       [Step A][Step B]
                (독립적으로 병렬 실행)

STEP:           [Step A]...[Step B]    대기...[Step A]...[Step B]
                         ^                    ^
                  report()             block() → 이전 빌드의 같은 Step 완료 후 실행

BUILD:          [전체 빌드 완료]         대기...........................[실행 시작]
                         ^                                            ^
              CheckPoint.COMPLETED              block() → 이전 빌드 완전 완료 후
```

| 수준 | 동작 | 동시성 | 적합한 상황 |
|------|------|--------|-----------|
| `NONE` | 외부 동기화 없음 | 최고 | 이전 빌드에 의존하지 않는 스텝 (권장) |
| `STEP` | 이전 빌드의 같은 스텝 완료 대기 | 중간 | 같은 스텝의 이전 결과만 참조하는 경우 |
| `BUILD` | 이전 빌드 완전 완료 대기 | 최저 | 이전 빌드 전체 결과에 의존하는 경우 (기본값) |

### 3.3 STEP 모드의 동기화 메커니즘

`STEP` 모드의 구현을 자세히 살펴보면:

```
BuildStepMonitor.STEP.perform() 호출

1. CheckPoint cp = new CheckPoint(bs.getClass().getName(), bs.getClass())
   → BuildStep 클래스명을 identity로 사용하여 CheckPoint 생성

2. cp.block(listener, displayName)
   → 이전 빌드에서 같은 클래스의 CheckPoint가 report()될 때까지 대기

3. bs.perform(build, launcher, listener)
   → 실제 빌드 스텝 실행

4. finally { cp.report() }
   → 후속 빌드의 같은 스텝에게 "완료" 신호 전달
```

`block()` → `perform()` → `report()` 패턴은 동시 빌드 간의 안전한 데이터 교환을 보장한다.

### 3.4 마이그레이션 가이드

소스 코드 Javadoc(BuildStep.java 195~216행)에서 명시한 마이그레이션 전략:

```
1. 최소 변경: getRequiredMonitorService()를 오버라이드하지 않음
   → BUILD 모드 유지 (하위 호환, 동시 빌드 이점 없음)

2. 독립적 스텝: NONE 반환
   → Run.getPreviousBuild()를 호출하지 않는 경우

3. 자기 결과만 참조: STEP 반환
   → 이전 빌드에서 자신이 추가한 Action만 참조하는 경우

4. 복잡한 의존성: NONE 반환 + CheckPoint 직접 사용
   → block()으로 필요한 지점에서만 대기
```

---

## 4. CheckPoint: 동시 빌드 간 동기화

### 4.1 클래스 구조

```
소스: core/src/main/java/hudson/model/CheckPoint.java
```

```java
public final class CheckPoint {
    private final Object identity;
    private final String internalName;

    // 생성자 1: 커스텀 identity
    public CheckPoint(String internalName, Object identity) { ... }

    // 생성자 2: 자동 identity (new Object())
    public CheckPoint(String internalName) { ... }

    // 이 체크포인트에 도달했음을 알림
    public void report() { Run.reportCheckpoint(this); }

    // 이전 빌드의 같은 체크포인트까지 대기
    public void block() throws InterruptedException {
        Run.waitForCheckpoint(this, null, null);
    }

    // 리치 로깅 지원 block
    public void block(@NonNull BuildListener listener, @NonNull String waiter)
        throws InterruptedException { ... }
}
```

### 4.2 사전 정의된 CheckPoint

Jenkins는 세 개의 표준 CheckPoint를 정의한다:

```java
// CheckPoint.java 164~175행
public static final CheckPoint CULPRITS_DETERMINED =
    new CheckPoint("CULPRITS_DETERMINED");

public static final CheckPoint COMPLETED =
    new CheckPoint("COMPLETED");

public static final CheckPoint MAIN_COMPLETED =
    new CheckPoint("MAIN_COMPLETED");
```

| CheckPoint | 의미 | 사용 위치 |
|-----------|------|---------|
| `CULPRITS_DETERMINED` | `getCulprits()` 계산 완료 | 변경 책임자 목록이 확정된 시점 |
| `COMPLETED` | 빌드 완전 완료 (`isBuilding()==false`) | `BuildStepMonitor.BUILD`에서 사용 |
| `MAIN_COMPLETED` | Builder 실행 완료, post-build 이행 중 | FreeStyleProject의 메인 빌드 완료 시점 |

### 4.3 동기화 시나리오

소스 코드 Javadoc(CheckPoint.java 127~138행)에서 설명하는 시나리오:

```
시간 흐름 →

Build #1: [......checkout......][build][test][  JUnit report  ]
                                                    ↑ report()

Build #2: [......checkout......][build][test][ 중단(abort) ]
                                              ← report() 없이 종료

Build #3: [......checkout......][build][test][ block() 대기... ]
                                                    ↓
                                    Build #1의 report()에 의해 해제

핵심: Build #3는 "이전 진행 중 빌드(previous build in progress)"를 기준으로 대기.
      Build #2가 중단되어도 Build #1이 report()하면 Build #3가 해제된다.
      "previous build in progress" = "previous (build in progress)"
      ≠ "(previous build) if in progress"
```

이 동작 방식 덕분에, `report()`/`block()` 쌍을 try/finally 없이도 안전하게 사용할 수 있다.

### 4.4 identity 기반 동등성

```java
// CheckPoint.java 89~97행
@Override
public boolean equals(Object that) {
    if (that == null || getClass() != that.getClass()) return false;
    return identity == ((CheckPoint) that).identity;  // 참조 동등성 (==)
}

@Override
public int hashCode() {
    return identity.hashCode();
}
```

`identity` 필드로 참조 동등성(`==`)을 사용한다. 따라서 같은 CheckPoint 인스턴스를 공유해야 동기화가 작동한다. 일반적으로 `static final`로 선언하는 이유이다.

---

## 5. BuildStepCompatibilityLayer: 하위 호환성 레이어

### 5.1 클래스 구조

```
소스: core/src/main/java/hudson/tasks/BuildStepCompatibilityLayer.java
```

```java
@Deprecated
public abstract class BuildStepCompatibilityLayer implements BuildStep {
    // 새 API (>= 1.150)
    public boolean prebuild(AbstractBuild<?, ?> build, BuildListener listener) { ... }
    public boolean perform(AbstractBuild<?, ?> build, Launcher launcher,
        BuildListener listener) throws InterruptedException, IOException { ... }
    public Collection<? extends Action> getProjectActions(
        AbstractProject<?, ?> project) { ... }

    // 구 API (< 1.150)
    public boolean prebuild(Build<?, ?> build, BuildListener listener) { ... }
    public boolean perform(Build<?, ?> build, Launcher launcher,
        BuildListener listener) throws InterruptedException, IOException { ... }
    public Action getProjectAction(Project<?, ?> project) { ... }
}
```

### 5.2 호환성 브리지 패턴

이 클래스의 핵심 역할은 `AbstractBuild` API와 구형 `Build` API 간의 양방향 브리지이다.

```
신규 플러그인                  BuildStepCompatibilityLayer               구형 플러그인
(AbstractBuild 사용)                                                    (Build 사용)
       |                                                                    |
       |--- perform(AbstractBuild) --→ instanceof Build? ---→ perform(Build) ---→|
       |                                     |                                   |
       |                                     no → SimpleBuildStep 위임           |
       |                                          또는 true 반환                 |
       |                                                                         |
       |←--- perform(AbstractBuild) ←--- Util.isOverridden() 확인 ←--- perform(Build)
```

### 5.3 SimpleBuildStep 위임

`BuildStepCompatibilityLayer.perform()` 메서드에서 주목할 부분:

```java
// BuildStepCompatibilityLayer.java 72~90행
public boolean perform(AbstractBuild<?, ?> build, Launcher launcher,
        BuildListener listener) throws InterruptedException, IOException {
    if (this instanceof SimpleBuildStep step) {
        // SimpleBuildStep으로 위임
        final FilePath workspace = build.getWorkspace();
        if (step.requiresWorkspace() && workspace == null) {
            throw new AbortException("no workspace for " + build);
        }
        if (workspace != null) {
            step.perform(build, workspace, build.getEnvironment(listener),
                launcher, listener);
        } else {
            step.perform(build, build.getEnvironment(listener), listener);
        }
        return true;  // SimpleBuildStep은 항상 true 반환 또는 예외
    } else if (build instanceof Build) {
        return perform((Build) build, launcher, listener);
    } else {
        return true;
    }
}
```

이 코드가 보여주는 설계 의도:
- `SimpleBuildStep`을 구현하면 반환값 대신 예외 기반 에러 처리를 사용
- `SimpleBuildStep`이 아닌 경우 구형 `Build` 타입으로 위임
- 둘 다 아니면 안전하게 `true` 반환

---

## 6. Builder: 빌드 단계 실행기

### 6.1 클래스 계층 구조

```
소스: core/src/main/java/hudson/tasks/Builder.java
```

```
                  BuildStep (interface)
                       |
            BuildStepCompatibilityLayer (abstract)
                       |
                  Builder (abstract)
                  implements Describable<Builder>, ExtensionPoint
                       |
          +------------+-------------+
          |            |             |
    Shell.java    Maven.java    커스텀 Builder
```

### 6.2 전체 소스 코드 분석

```java
// Builder.java 전체 (80줄)
public abstract class Builder extends BuildStepCompatibilityLayer
        implements Describable<Builder>, ExtensionPoint {

    // 하위 호환 유지 (Hudson < 1.150)
    @Override
    public boolean prebuild(Build build, BuildListener listener) {
        return true;  // 기본: 아무 것도 하지 않음
    }

    // Builder는 기본적으로 NONE — 이전 빌드에 의존하지 않음
    @Override
    public BuildStepMonitor getRequiredMonitorService() {
        return BuildStepMonitor.NONE;
    }

    @Override
    public Descriptor<Builder> getDescriptor() {
        return Jenkins.get().getDescriptorOrDie(getClass());
    }

    // 등록된 모든 Builder Descriptor 목록
    public static DescriptorExtensionList<Builder, Descriptor<Builder>> all() {
        return Jenkins.get().getDescriptorList(Builder.class);
    }
}
```

### 6.3 핵심 설계 결정

**왜 Builder는 `BuildStepMonitor.NONE`이 기본값인가?**

BuildStep 인터페이스의 기본값은 `BUILD`인데, Builder는 이를 `NONE`으로 오버라이드한다.

```java
// Builder.java 63~66행
@Override
public BuildStepMonitor getRequiredMonitorService() {
    return BuildStepMonitor.NONE;
}
```

이유: Builder는 소스 코드를 컴파일하고 테스트를 실행하는 역할이다. 각 빌드는 자체 워크스페이스에서 독립적으로 작업하므로, 이전 빌드의 결과에 의존하지 않는 것이 일반적이다. 따라서 동시 빌드를 최대한 허용한다.

반면 `Publisher`(특히 이메일 알림 등)는 이전 빌드 결과를 참조하여 "이번에 실패했는데 이전에는 성공이었나?" 같은 판단을 해야 하므로, 더 보수적인 동기화가 필요하다.

### 6.4 확장점 패턴

Builder를 확장하는 플러그인의 전형적 구조:

```
MyBuilder extends Builder {
    // 1. 설정 필드 (XStream으로 직렬화됨)
    private final String targetName;

    // 2. DataBoundConstructor (Stapler 바인딩)
    @DataBoundConstructor
    public MyBuilder(String targetName) { ... }

    // 3. 핵심 빌드 로직
    @Override
    public boolean perform(AbstractBuild build, Launcher launcher,
            BuildListener listener) { ... }

    // 4. Descriptor (Extension 등록)
    @Extension
    public static class DescriptorImpl extends BuildStepDescriptor<Builder> {
        @Override
        public boolean isApplicable(Class<? extends AbstractProject> jobType) {
            return true;
        }
    }
}
```

---

## 7. Publisher: 빌드 후 처리 프레임워크

### 7.1 클래스 계층 구조

```
소스: core/src/main/java/hudson/tasks/Publisher.java
```

```
                  BuildStep (interface)
                       |
            BuildStepCompatibilityLayer (abstract)
                       |
                  Publisher (abstract)
                  implements Describable<Publisher>
                       |
          +------------+------------+
          |                         |
    Recorder (abstract)      Notifier (abstract)
    implements ExtensionPoint  implements ExtensionPoint
          |                         |
    JUnitResultArchiver       MailSender
    FindBugsPublisher         SlackNotifier
    등                         등
```

### 7.2 Publisher 핵심 API

```java
// Publisher.java 65행
public abstract class Publisher extends BuildStepCompatibilityLayer
        implements Describable<Publisher> {

    @Deprecated  // 직접 상속하지 말고 Recorder 또는 Notifier를 사용할 것
    protected Publisher() {}

    // 빌드 결과 확정 후 실행해야 하는지 여부
    public boolean needsToRunAfterFinalized() {
        return false;
    }
}
```

### 7.3 needsToRunAfterFinalized() 메서드

이 메서드는 Publisher의 실행 시점을 결정하는 핵심 메서드이다.

```
소스: Publisher.java 97~121행
```

```
needsToRunAfterFinalized() == false (기본값):
+------------------------------------------------+
|  빌드 실행 중 (isBuilding() == true)             |
|                                                  |
|  [Builder들] → [Publisher.perform()]              |
|                     ↑                            |
|               빌드 결과 변경 가능                  |
|               (FAILURE로 마킹 가능)               |
|               실행 시간이 빌드 시간에 포함          |
+------------------------------------------------+

needsToRunAfterFinalized() == true:
+------------------------------------------------+
|  빌드 완료 (isBuilding() == false)               |
+------------------------------------------------+
|  [Publisher.perform()]                           |
|       ↑                                         |
|  빌드 결과 변경 불가                               |
|  다른 빌드가 이 빌드를 "완료된 빌드"로 참조 가능    |
+------------------------------------------------+
```

`needsToRunAfterFinalized()`가 `true`를 반환하는 대표적 사례: 다른 빌드를 트리거하는 Publisher. 트리거되는 하위 빌드가 상위 빌드의 결과를 확인해야 하므로, 상위 빌드가 "완료" 상태여야 한다.

### 7.4 Publisher의 @Deprecated 생성자

```java
// Publisher.java 67~73행
@Deprecated
protected Publisher() {}
```

생성자가 `@Deprecated`인 이유: `Publisher`를 직접 상속하지 말고, 반드시 `Recorder` 또는 `Notifier` 중 하나를 상속하라는 설계 의도이다. 이는 Publisher의 실행 순서를 결정하는 정렬 메커니즘과 직결된다.

---

## 8. Recorder vs Notifier: Publisher의 두 얼굴

### 8.1 Recorder

```
소스: core/src/main/java/hudson/tasks/Recorder.java (57줄)
```

```java
public abstract class Recorder extends Publisher implements ExtensionPoint {
    @SuppressWarnings("deprecation")
    protected Recorder() {}

    @Override
    public BuildStepDescriptor getDescriptor() {
        return (BuildStepDescriptor) super.getDescriptor();
    }
}
```

Recorder의 역할 (소스 Javadoc에서 확인):

```java
// Recorder.java 34~37행
// Recorder is a kind of Publisher that collects statistics from the build,
// and can mark builds as unstable/failure. This marking ensures that builds
// are marked accordingly before notifications are sent via Notifiers.
```

- **빌드 결과에 영향을 줄 수 있다** (UNSTABLE, FAILURE로 마킹)
- **Notifier보다 먼저 실행된다** (정렬 순서에 의해 보장)
- 예: 테스트 결과 수집 (실패 시 빌드를 UNSTABLE로 마킹), 코드 커버리지 측정

### 8.2 Notifier

```
소스: core/src/main/java/hudson/tasks/Notifier.java (57줄)
```

```java
public abstract class Notifier extends Publisher implements ExtensionPoint {
    @SuppressWarnings("deprecation")
    protected Notifier() {}

    @Override
    public BuildStepDescriptor getDescriptor() {
        return (BuildStepDescriptor) super.getDescriptor();
    }
}
```

Notifier의 역할 (소스 Javadoc에서 확인):

```java
// Notifier.java 34~37행
// Notifier is a kind of Publisher that sends out the outcome of the builds
// to other systems and humans. This marking ensures that notifiers are run
// after the build result is set to its final value by other Recorders.
```

- **빌드 결과를 외부에 알린다** (이메일, Slack, IRC 등)
- **Recorder 이후에 실행된다** (빌드 결과가 확정된 후)
- `needsToRunAfterFinalized()`를 `true`로 오버라이드하면, 빌드가 완전 완료된 후에도 실행 가능

### 8.3 왜 Recorder와 Notifier를 분리했는가?

```
시나리오: JUnit 테스트 결과 수집 + 이메일 알림

[잘못된 순서]
1. MailSender: 빌드 SUCCESS → "빌드 성공" 이메일 발송
2. JUnitResultArchiver: 테스트 실패 발견 → 빌드 UNSTABLE로 마킹
→ 문제: 이메일은 이미 "성공"으로 발송됨!

[올바른 순서 — Recorder/Notifier 분리로 보장]
1. JUnitResultArchiver (Recorder): 테스트 실패 → 빌드 UNSTABLE 마킹
2. MailSender (Notifier): 빌드 UNSTABLE → "빌드 불안정" 이메일 발송
→ 정확한 상태 반영
```

이 분리가 1.286에서 도입된 이유: 초기 Jenkins는 Publisher의 실행 순서가 보장되지 않았다. 빌드 결과를 변경하는 Publisher와 결과를 알리는 Publisher가 임의 순서로 실행되어, 위와 같은 불일치가 발생했다.

---

## 9. BuildWrapper: 빌드 환경 래퍼

### 9.1 클래스 구조

```
소스: core/src/main/java/hudson/tasks/BuildWrapper.java (338줄)
```

```java
public abstract class BuildWrapper
        implements Describable<BuildWrapper>, ExtensionPoint {

    // 내부 클래스: 환경 설정/해제
    public abstract class Environment extends hudson.model.Environment {
        public boolean tearDown(AbstractBuild build, BuildListener listener)
            throws IOException, InterruptedException { ... }
    }

    // 빌드 전 환경 구성 — Environment 반환
    public Environment setUp(AbstractBuild build, Launcher launcher,
        BuildListener listener) throws IOException, InterruptedException { ... }

    // Launcher 데코레이션 (setUp보다 먼저 호출)
    public Launcher decorateLauncher(AbstractBuild build, Launcher launcher,
        BuildListener listener) throws IOException, InterruptedException { ... }

    // 로거 데코레이션 (setUp보다 먼저 호출)
    public OutputStream decorateLogger(AbstractBuild build, OutputStream logger)
        throws IOException, InterruptedException { ... }

    // SCM 체크아웃 전 실행 (decorateLauncher 이후, setUp 이전)
    public void preCheckout(AbstractBuild build, Launcher launcher,
        BuildListener listener) throws IOException, InterruptedException { ... }

    // 빌드 변수 추가
    public void makeBuildVariables(AbstractBuild build,
        Map<String, String> variables) { ... }

    // 민감한 빌드 변수 지정
    public void makeSensitiveBuildVariables(AbstractBuild build,
        Set<String> sensitiveVariables) { ... }
}
```

### 9.2 BuildWrapper 생명주기

```
BuildWrapper의 메서드 호출 순서:

1. decorateLogger()          ← 빌드 매우 초기 단계
2. decorateLauncher()        ← 빌드 매우 초기 단계
3. preCheckout()             ← SCM 체크아웃 전
4. [SCM 체크아웃]
5. setUp()                   ← 체크아웃 후, Builder 실행 전
   → Environment 객체 반환
6. [Builder들 실행]
7. [Publisher들 실행]
8. Environment.tearDown()    ← 빌드 후 정리 (실패해도 실행)
```

### 9.3 setUp()과 Environment 패턴

```java
// BuildWrapper.java 130~158행
public Environment setUp(AbstractBuild build, Launcher launcher,
        BuildListener listener) throws IOException, InterruptedException {
    if (build instanceof Build &&
        Util.isOverridden(BuildWrapper.class, getClass(),
            "setUp", Build.class, Launcher.class, BuildListener.class))
        return setUp((Build) build, launcher, listener);
    else
        throw new UnsupportedOperationException(
            "Plugin class '" + this.getClass().getName() +
            "' does not support a build of type '" +
            build.getClass().getName() + "'.");
}
```

**반환값의 의미**:
- `non-null Environment` → 빌드 계속 진행
- `null` → 에러 발생, 빌드 중단

### 9.4 Environment.tearDown()의 안전성

```java
// BuildWrapper.java 88~118행 (Environment 내부 클래스)
@Override
public boolean tearDown(AbstractBuild build, BuildListener listener)
        throws IOException, InterruptedException {
    if (build instanceof Build)
        return tearDown((Build) build, listener);
    else
        return true;
}
```

소스 주석에서 확인한 핵심:

```
tearDown()은 빌드가 실패해도 반드시 호출된다.
→ 애플리케이션 서버 중지, DB 연결 해제 등 정리 작업 보장
→ Build.getResult()가 Result.FAILURE를 반환 (1.339부터)
→ null 결과는 "지금까지 SUCCESS"를 의미 (post-build 액션이 최종 결과에 영향 가능)
```

### 9.5 Launcher 데코레이션

```java
// BuildWrapper.java 180~211행
public Launcher decorateLauncher(AbstractBuild build, Launcher launcher,
        BuildListener listener)
        throws IOException, InterruptedException, RunnerAbortedException {
    return launcher;  // 기본: 변경 없이 그대로 반환
}
```

이 메서드는 `setUp()` **이전에** 호출된다. 용도:
- sudo/pfexec/chroot 래핑
- 환경변수 주입
- 원격 빌드 에이전트에서의 실행 환경 조작

여러 BuildWrapper가 동일한 Launcher를 데코레이트할 수 있으며, 체이닝이 가능하다.

### 9.6 빌드 변수 관리

```java
// BuildWrapper.java 298~328행
// 빌드 변수 추가
public void makeBuildVariables(AbstractBuild build,
        Map<String, String> variables) {
    // noop
}

// 민감한 변수 이름 등록 (콘솔 출력에서 마스킹됨)
public void makeSensitiveBuildVariables(AbstractBuild build,
        Set<String> sensitiveVariables) {
    // noop
}
```

`makeSensitiveBuildVariables()`를 통해 등록된 변수 이름은 콘솔 로그에서 마스킹(`****`)된다. 비밀번호, API 토큰 등을 보호하는 메커니즘이다.

---

## 10. Trigger: 빌드 트리거 시스템

### 10.1 클래스 구조

```
소스: core/src/main/java/hudson/triggers/Trigger.java (378줄)
```

```java
public abstract class Trigger<J extends Item>
        implements Describable<Trigger<?>>, ExtensionPoint {

    protected final String spec;            // cron 표현식
    protected transient CronTabList tabs;   // 파싱된 cron 탭
    @CheckForNull
    protected transient J job;              // 연결된 프로젝트

    protected Trigger(@NonNull String cronTabSpec) {
        this.spec = cronTabSpec;
        this.tabs = CronTabList.create(cronTabSpec);
    }

    protected Trigger() {
        this.spec = "";
        this.tabs = new CronTabList(Collections.emptyList());
    }
}
```

### 10.2 Trigger 생명주기

```
1. 프로젝트 설정 저장 시:
   → Trigger 인스턴스 생성 (spec 파싱)
   → start(project, newInstance=true) 호출

2. Jenkins 재시작 시:
   → XStream 역직렬화 (readResolve()에서 tabs 재파싱)
   → start(project, newInstance=false) 호출

3. 매분 cron 체크:
   → Cron.doRun() → checkTriggers(cal) → trigger.run()

4. 프로젝트 설정 변경 시:
   → stop() 호출 (기존 트리거 제거)
   → 새 트리거 인스턴스로 start() 호출
```

### 10.3 start() 메서드

```java
// Trigger.java 92~107행
public void start(J project, boolean newInstance) {
    LOGGER.finer(() -> "Starting " + this + " on " + project);
    this.job = project;

    try {
        if (spec != null) {
            // 프로젝트 이름 기반 해시로 cron 탭 재파싱
            this.tabs = CronTabList.create(spec,
                Hash.from(project.getFullName()));
        } else {
            LOGGER.log(Level.WARNING,
                "The job {0} has a null crontab spec", job.getFullName());
        }
    } catch (IllegalArgumentException e) {
        LOGGER.log(Level.WARNING,
            String.format("Failed to parse crontab spec %s in job %s",
                spec, project.getFullName()), e);
    }
}
```

**왜 start()에서 cron을 다시 파싱하는가?**

`Hash.from(project.getFullName())`을 사용하여 cron 표현식을 재파싱한다. Jenkins의 cron은 `H` 토큰을 지원하는데, 이 토큰은 프로젝트 이름의 해시값으로 대체된다. 예를 들어 `H/15 * * * *`는 프로젝트마다 다른 분(minute)에 실행되어, 수백 개의 프로젝트가 동시에 폴링하는 것을 방지한다.

### 10.4 Cron 주기 검사

```java
// Trigger.java 217~250행 (Cron 내부 클래스)
@Extension @Symbol("cron")
public static class Cron extends PeriodicWork {
    private final Calendar cal = new GregorianCalendar();

    public Cron() {
        cal.set(Calendar.SECOND, 0);
        cal.set(Calendar.MILLISECOND, 0);
    }

    @Override
    public long getRecurrencePeriod() {
        return MIN;  // 1분마다 실행
    }

    @Override
    public long getInitialDelay() {
        // 분이 바뀌는 정각에 시작하도록 지연 계산
        return MIN - TimeUnit.SECONDS.toMillis(
            Calendar.getInstance().get(Calendar.SECOND));
    }

    @Override
    public void doRun() {
        while (new Date().getTime() >= cal.getTimeInMillis()) {
            LOGGER.log(Level.FINE, "cron checking {0}", cal.getTime());
            try {
                checkTriggers(cal);
            } catch (Throwable e) {
                LOGGER.log(Level.WARNING,
                    "Cron thread throw an exception", e);
            }
            cal.add(Calendar.MINUTE, 1);
        }
    }
}
```

`doRun()`의 `while` 루프는 Jenkins가 일시적으로 중단되었다가 복구된 경우, 놓친 분(minute)을 따라잡기 위한 것이다.

### 10.5 checkTriggers() 메서드 상세

```java
// Trigger.java 255~326행
public static void checkTriggers(final Calendar cal) {
    Jenkins inst = Jenkins.get();

    // 1. 동기 폴링 모드인지 확인 (SCMTrigger 전용)
    SCMTrigger.DescriptorImpl scmd =
        inst.getDescriptorByType(SCMTrigger.DescriptorImpl.class);
    if (scmd.synchronousPolling) {
        // 이전 동기 폴링이 완료되었으면 새로 시작
        if (previousSynchronousPolling == null ||
                previousSynchronousPolling.isDone()) {
            previousSynchronousPolling =
                scmd.getExecutor().submit(new DependencyRunner(...));
        }
    }

    // 2. 모든 TriggeredItem 순회
    for (TriggeredItem p : inst.allItems(TriggeredItem.class)) {
        for (Trigger t : p.getTriggers().values()) {
            // 동기 폴링 모드의 SCMTrigger는 건너뜀 (위에서 별도 처리)
            if (!(p instanceof AbstractProject && t instanceof SCMTrigger
                    && scmd.synchronousPolling)) {
                if (t.tabs.check(cal)) {
                    // cron 표현식이 현재 시간과 일치하면 실행
                    long begin_time = System.currentTimeMillis();
                    t.run();
                    long end_time = System.currentTimeMillis();

                    // 30초 이상 걸리면 경고 (CRON_THRESHOLD)
                    if (end_time - begin_time > CRON_THRESHOLD * 1000) {
                        SlowTriggerAdminMonitor.getInstance().report(...);
                    }
                }
            }
        }
    }
}
```

**CRON_THRESHOLD** (Trigger.java 334행):

```java
public static long CRON_THRESHOLD =
    SystemProperties.getLong(Trigger.class.getName() + ".CRON_THRESHOLD", 30L);
```

기본 30초. 트리거 실행이 이 시간을 초과하면 `SlowTriggerAdminMonitor`에 보고되고 관리자에게 경고를 표시한다.

### 10.6 TimerTrigger: 시간 기반 트리거

```
소스: core/src/main/java/hudson/triggers/TimerTrigger.java
```

```java
public class TimerTrigger extends Trigger<BuildableItem> {

    @DataBoundConstructor
    public TimerTrigger(@NonNull String spec) {
        super(spec);
    }

    @Override
    public void run() {
        if (job == null) {
            return;
        }
        job.scheduleBuild(0, new TimerTriggerCause());
    }
}
```

- cron 표현식에 따라 주기적으로 `scheduleBuild()` 호출
- `TimerTriggerCause`를 통해 빌드 원인 추적 가능
- `BuildableItem`에만 적용 가능

### 10.7 SCMTrigger: SCM 변경 감지 트리거

```
소스: core/src/main/java/hudson/triggers/SCMTrigger.java
```

```java
public class SCMTrigger extends Trigger<Item> {
    private boolean ignorePostCommitHooks;

    @DataBoundConstructor
    public SCMTrigger(String scmpoll_spec) {
        super(scmpoll_spec);
    }

    @Override
    public void run() {
        if (job == null) { return; }
        run(null);
    }

    public void run(Action[] additionalActions) {
        if (job == null) { return; }
        DescriptorImpl d = getDescriptor();
        // SCM 폴링 큐에 제출
        ...
    }
}
```

- cron 표현식에 따라 SCM을 폴링
- 변경 사항이 감지되면 빌드 스케줄
- `ignorePostCommitHooks`: post-commit 훅 무시 옵션 (1.493부터)
- 동기 폴링 모드: 의존성 순서대로 SCM 폴링 (`DependencyRunner` 사용)

### 10.8 Trigger의 for_() 필터링

```java
// Trigger.java 362~377행
public static List<TriggerDescriptor> for_(Item i) {
    List<TriggerDescriptor> r = new ArrayList<>();
    for (TriggerDescriptor t : all()) {
        if (!t.isApplicable(i))  continue;

        if (i instanceof TopLevelItem) {
            TopLevelItemDescriptor tld = ((TopLevelItem) i).getDescriptor();
            if (tld != null && !tld.isApplicable(t))  continue;
        }

        r.add(t);
    }
    return r;
}
```

이 메서드는 주어진 Item에 적용 가능한 트리거만 필터링한다. 양방향 적합성 검사를 수행한다:
1. `TriggerDescriptor.isApplicable(Item)` - 트리거가 이 아이템을 지원하는지
2. `TopLevelItemDescriptor.isApplicable(Descriptor)` - 아이템이 이 트리거를 허용하는지

---

## 11. SimpleBuildStep: 현대적 빌드 스텝 API

### 11.1 인터페이스 구조

```
소스: core/src/main/java/jenkins/tasks/SimpleBuildStep.java
```

```java
public interface SimpleBuildStep extends BuildStep {

    // 워크스페이스 필요 시 (기본)
    default void perform(@NonNull Run<?, ?> run,
            @NonNull FilePath workspace,
            @NonNull EnvVars env,
            @NonNull Launcher launcher,
            @NonNull TaskListener listener)
        throws InterruptedException, IOException { ... }

    // 워크스페이스 불필요 시
    default void perform(@NonNull Run<?, ?> run,
            @NonNull EnvVars env,
            @NonNull TaskListener listener)
        throws InterruptedException, IOException { ... }

    // 워크스페이스가 필요한지 여부
    default boolean requiresWorkspace() { return true; }
}
```

### 11.2 왜 SimpleBuildStep이 도입되었는가?

소스 Javadoc(SimpleBuildStep.java 61~71행)에서 확인한 설계 가이드라인:

```
SimpleBuildStep의 규칙:
1. prebuild()을 구현하지 말 것 — 특정 실행 순서를 가정하므로
2. getProjectActions()를 구현하지 말 것 — 정적 설정이 아닐 수 있으므로
   → 대신 LastBuildAction 사용
3. getRequiredMonitorService()는 NONE으로 — 빌드당 1회 실행이 아닐 수 있으므로
4. DependencyDeclarer를 구현하지 말 것 — AbstractProject에 국한되므로
5. BuildStepDescriptor.isApplicable()은 무조건 true 반환
6. Executor.currentExecutor()가 null일 수 있음을 대비
```

이 규칙들의 핵심 동기: **Pipeline(Jenkinsfile)과의 호환성**. Pipeline에서는 빌드 스텝이 임의 시점에 여러 번 호출될 수 있다. Freestyle 프로젝트의 가정(정적 설정, 빌드당 1회 실행)이 성립하지 않는다.

### 11.3 `Run` vs `AbstractBuild`

`SimpleBuildStep`은 `AbstractBuild` 대신 `Run`을 매개변수로 받는다:

```
AbstractBuild : Freestyle 프로젝트 전용 빌드 객체
     ↑
    Run : 모든 유형의 빌드 실행을 나타내는 범용 객체
     ↑
  (Pipeline에서도 사용 가능)
```

### 11.4 LastBuildAction 패턴

```java
// SimpleBuildStep.java 173~182행
interface LastBuildAction extends Action {
    Collection<? extends Action> getProjectActions();
}
```

`getProjectActions(AbstractProject)`의 대체:
- 빌드 실행 시 `Run.addAction()`으로 `LastBuildAction` 추가
- `TransientActionFactory`가 `lastSuccessfulBuild`의 `LastBuildAction`을 수집
- 프로젝트 페이지에 자동 반영

---

## 12. 빌드 실행 순서: 전체 파이프라인 흐름

### 12.1 Freestyle 빌드의 전체 실행 흐름

```
┌─────────────────────────────────────────────────────────────────┐
│                    빌드 트리거 단계                                │
│                                                                   │
│  Trigger.run() → Queue.schedule()                                │
│  (TimerTrigger: cron 매칭 시)                                     │
│  (SCMTrigger: SCM 변경 감지 시)                                   │
│  (Webhook: 외부 이벤트 시)                                        │
└──────────────────────────┬──────────────────────────────────────┘
                           │
                           v
┌─────────────────────────────────────────────────────────────────┐
│                    빌드 큐 처리                                    │
│                                                                   │
│  Queue → Executor 할당 → 워크스페이스 획득                         │
└──────────────────────────┬──────────────────────────────────────┘
                           │
                           v
┌─────────────────────────────────────────────────────────────────┐
│                BuildWrapper 초기화 단계                            │
│                                                                   │
│  1. BuildWrapper.decorateLogger()     ← 콘솔 로그 래핑            │
│  2. BuildWrapper.decorateLauncher()   ← 프로세스 실행기 래핑       │
│  3. BuildWrapper.preCheckout()        ← 체크아웃 전 준비           │
│  4. [SCM 체크아웃]                                                │
│  5. BuildWrapper.setUp()              ← Environment 객체 반환      │
│     → makeBuildVariables()            ← 빌드 변수 주입             │
│     → makeSensitiveBuildVariables()   ← 민감 변수 등록             │
└──────────────────────────┬──────────────────────────────────────┘
                           │
                           v
┌─────────────────────────────────────────────────────────────────┐
│                    빌드 실행 단계                                  │
│                                                                   │
│  for each Builder:                                                │
│    6. Builder.prebuild()              ← 빌드 전 검증               │
│    7. BuildStepMonitor.perform()      ← 동기화 제어 하에 실행      │
│       → Builder.perform()             ← 실제 빌드 작업             │
│                                                                   │
│  CheckPoint.MAIN_COMPLETED.report()   ← 메인 빌드 완료 신호       │
└──────────────────────────┬──────────────────────────────────────┘
                           │
                           v
┌─────────────────────────────────────────────────────────────────┐
│              빌드 후 처리 단계 (Publisher)                         │
│                                                                   │
│  [정렬 순서: Recorder → 미분류 → Notifier]                        │
│                                                                   │
│  --- needsToRunAfterFinalized() == false ---                      │
│  8. Recorder.perform()                ← 결과 수집, 빌드 상태 변경  │
│  9. (미분류 Publisher).perform()                                  │
│ 10. Notifier.perform()                ← 알림 발송                  │
│                                                                   │
│  --- 빌드 결과 확정 (isBuilding() = false) ---                    │
│                                                                   │
│  --- needsToRunAfterFinalized() == true ---                       │
│ 11. (finalized Publisher).perform()   ← 빌드 완료 후 처리          │
└──────────────────────────┬──────────────────────────────────────┘
                           │
                           v
┌─────────────────────────────────────────────────────────────────┐
│                    정리(tearDown) 단계                             │
│                                                                   │
│ 12. BuildWrapper.Environment.tearDown()  ← 역순으로 실행          │
│     (빌드 성공/실패 관계없이 항상 실행)                              │
│                                                                   │
│  CheckPoint.COMPLETED.report()        ← 빌드 완전 완료 신호       │
└─────────────────────────────────────────────────────────────────┘
```

### 12.2 단계별 에러 처리

| 단계 | 에러 발생 시 | 빌드 결과 |
|------|-----------|---------|
| `BuildWrapper.setUp()` | `null` 반환 또는 예외 → 빌드 즉시 중단 | FAILURE |
| `Builder.prebuild()` | `false` 반환 → 빌드 중단 | FAILURE |
| `Builder.perform()` | `false` 반환 또는 예외 → 빌드 중단 | FAILURE |
| `Recorder.perform()` | 빌드 상태 변경 가능 (UNSTABLE 등) | 변경됨 |
| `Notifier.perform()` | 빌드 상태에 영향 없음 (알림만) | 유지 |
| `Environment.tearDown()` | 에러와 무관하게 항상 실행 | 유지/변경 |

### 12.3 동시 빌드 시나리오

```
동일 잡의 Build #1과 Build #2가 동시에 실행되는 경우:

Build #1:  [setUp][Builder A][Builder B][Recorder][Notifier][tearDown]
                                          ↑ report(MAIN_COMPLETED)
                                                              ↑ report(COMPLETED)

Build #2:  [setUp][Builder A][Builder B]
                                 ↑
                          BuildStepMonitor에 따라:
                          - NONE: 즉시 실행
                          - STEP: Build #1의 Builder B 완료 대기
                          - BUILD: Build #1 완전 완료 대기
```

---

## 13. BuildStepDescriptor와 확장점 등록

### 13.1 클래스 구조

```
소스: core/src/main/java/hudson/tasks/BuildStepDescriptor.java (92줄)
```

```java
public abstract class BuildStepDescriptor<T extends BuildStep & Describable<T>>
        extends Descriptor<T> {

    // 이 BuildStep이 주어진 프로젝트 타입에 적용 가능한지
    public abstract boolean isApplicable(
        Class<? extends AbstractProject> jobType);

    // 필터링 유틸리티
    public static <T extends BuildStep & Describable<T>>
    List<Descriptor<T>> filter(List<Descriptor<T>> base,
        Class<? extends AbstractProject> type) { ... }
}
```

### 13.2 이중 필터링 메커니즘

`BuildStepDescriptor.filter()` 메서드의 동작:

```java
// BuildStepDescriptor.java 72~91행
public static <T extends BuildStep & Describable<T>>
List<Descriptor<T>> filter(List<Descriptor<T>> base,
        Class<? extends AbstractProject> type) {
    Descriptor pd = Jenkins.get().getDescriptor((Class) type);

    List<Descriptor<T>> r = new ArrayList<>(base.size());
    for (Descriptor<T> d : base) {
        // 1차 필터: 프로젝트 타입이 이 Descriptor를 허용하는지
        if (pd instanceof AbstractProjectDescriptor &&
                !((AbstractProjectDescriptor) pd).isApplicable(d))
            continue;

        // 2차 필터: BuildStep이 이 프로젝트 타입을 지원하는지
        if (d instanceof BuildStepDescriptor<T> bd) {
            if (!bd.isApplicable(type))  continue;
            r.add(bd);
        } else {
            // 1.150 이전 플러그인은 BuildStepDescriptor를 상속하지 않을 수 있음
            r.add(d);
        }
    }
    return r;
}
```

```
양방향 적합성 검사:

프로젝트 타입 ←→ BuildStep Descriptor

1. AbstractProjectDescriptor.isApplicable(Descriptor d)
   "이 프로젝트 타입이 주어진 BuildStep을 허용하는가?"

2. BuildStepDescriptor.isApplicable(Class<? extends AbstractProject> jobType)
   "이 BuildStep이 주어진 프로젝트 타입에서 작동하는가?"

양쪽 모두 true를 반환해야 UI에 표시됨
```

### 13.3 Descriptor 등록 방식의 진화

```
[초기 방식 — 1.286 이전]
BuildStep.BUILDERS.add(myDescriptor);     // 정적 리스트에 수동 등록
BuildStep.PUBLISHERS.addRecorder(myDesc); // Recorder로 등록
BuildStep.PUBLISHERS.addNotifier(myDesc); // Notifier로 등록

[현대 방식 — @Extension 어노테이션]
@Extension
public static class DescriptorImpl extends BuildStepDescriptor<Builder> {
    ...
}
→ Jenkins가 자동으로 발견하고 등록
```

소스 코드에서 확인한 하위 호환성 코드:

```java
// BuildStep.java 233~234행
@Deprecated
List<Descriptor<Builder>> BUILDERS = new DescriptorList<>(Builder.class);

// BuildStep.java 250~251행
@Deprecated
PublisherList PUBLISHERS = new PublisherList();
```

`DescriptorList`는 레거시 등록을 `@Extension` 기반 시스템으로 브리지한다.

---

## 14. Publisher 정렬 메커니즘

### 14.1 DescriptorExtensionListImpl

```
소스: Publisher.java 133~145행
```

```java
public static final class DescriptorExtensionListImpl
        extends DescriptorExtensionList<Publisher, Descriptor<Publisher>> {

    public DescriptorExtensionListImpl(Jenkins hudson) {
        super(hudson, Publisher.class);
    }

    @Override
    protected List<ExtensionComponent<Descriptor<Publisher>>>
    sort(List<ExtensionComponent<Descriptor<Publisher>>> r) {
        List<ExtensionComponent<Descriptor<Publisher>>> copy = new ArrayList<>(r);
        copy.sort(new ExtensionComponentComparator());
        return copy;
    }
}
```

Publisher의 `DescriptorExtensionList`는 `sort()` 메서드를 오버라이드하여 커스텀 정렬을 수행한다.

### 14.2 정렬 Comparator

```java
// Publisher.java 149~172행
private static final class ExtensionComponentComparator
        implements Comparator<ExtensionComponent<Descriptor<Publisher>>> {
    @Override
    public int compare(ExtensionComponent<Descriptor<Publisher>> lhs,
            ExtensionComponent<Descriptor<Publisher>> rhs) {
        int r = classify(lhs.getInstance()) - classify(rhs.getInstance());
        if (r != 0)   return r;
        return lhs.compareTo(rhs);  // 같은 분류 내에서 기본 정렬
    }

    private int classify(Descriptor<Publisher> d) {
        if (d.isSubTypeOf(Recorder.class))    return 0;  // Recorder: 최우선
        if (d.isSubTypeOf(Notifier.class))    return 2;  // Notifier: 최후순

        // 레거시 호환: 수동 등록된 Publisher의 종류 확인
        Class<? extends Publisher> kind = PublisherList.KIND.get(d);
        if (kind == Recorder.class)    return 0;
        if (kind == Notifier.class)    return 2;

        return 1;  // 미분류: 중간
    }
}
```

### 14.3 정렬 결과

```
정렬 키:  0          1          2
         Recorder → 미분류  → Notifier

실행 순서:
+-------------------+-------------------+-------------------+
| classify() == 0   | classify() == 1   | classify() == 2   |
|                   |                   |                   |
| Recorder들         | 미분류 Publisher들  | Notifier들         |
| (테스트 결과 수집)  | (레거시 플러그인)   | (이메일, Slack)    |
| (커버리지 측정)     |                   | (IRC 알림)         |
|                   |                   |                   |
| 빌드 상태 변경 가능 | 빌드 상태 변경 가능 | 빌드 상태 확정 후   |
+-------------------+-------------------+-------------------+
```

### 14.4 PublisherList.KIND: 레거시 호환

```java
// BuildStep.java 267~268행
static final WeakHashMap<Descriptor<Publisher>,
    Class<? extends Publisher>> KIND = new WeakHashMap<>();
```

`Recorder`/`Notifier` 분리(1.286) 이전에 개발된 플러그인은 `Publisher`를 직접 상속한다. 이런 플러그인이 `addRecorder()` 또는 `addNotifier()`로 수동 등록했다면, `KIND` 맵에 기록된 정보로 올바르게 분류된다.

```java
// BuildStep.java 283~286행
public void addNotifier(Descriptor<Publisher> d) {
    KIND.put(d, Notifier.class);  // 종류 기록
    core.add(d);
}

// BuildStep.java 297~300행
public void addRecorder(Descriptor<Publisher> d) {
    KIND.put(d, Recorder.class);  // 종류 기록
    core.add(d);
}
```

---

## 15. 설계 결정과 Why

### 15.1 왜 boolean 반환값에서 예외 기반으로 전환했는가?

```
초기 API:
boolean perform(...) → false 반환 시 빌드 실패

문제점:
1. false만으로는 실패 원인을 전달할 수 없다
2. 호출자가 반환값을 무시할 수 있다
3. 부분 실패(UNSTABLE)를 표현할 수 없다

현재 권장:
throw new AbortException("구체적 에러 메시지")
→ 사용자에게 명확한 에러 메시지 전달
→ 호출 스택에서 강제 처리
```

소스 코드에서 이 전환이 명시적으로 언급된다:

```java
// BuildStep.java 88~90행, 113~115행
// Using the return value to indicate success/failure should
// be considered deprecated, and implementations are encouraged
// to throw AbortException to indicate a failure.
```

### 15.2 왜 BuildWrapper가 BuildStep과 별도 계층인가?

```
BuildStep:    빌드의 "한 단계" — 순차 실행
BuildWrapper: 빌드의 "환경" — setUp/tearDown 쌍으로 빌드 전체를 감쌈

+------------ BuildWrapper.setUp() ------------+
|                                               |
|  [Builder 1] → [Builder 2] → [Publisher 1]   |
|                                               |
+------------ BuildWrapper.tearDown() ---------+

핵심 차이:
- BuildStep은 실행(perform)만 한다
- BuildWrapper는 리소스 관리(setUp + tearDown)를 한다
- tearDown은 빌드 실패와 무관하게 항상 실행된다 (try-finally 패턴)
```

BuildWrapper가 `BuildStep`을 구현하지 않는 이유: `perform()` 하나로는 "설정 → 빌드 → 정리" 패턴을 표현할 수 없다. `setUp()`이 `Environment` 객체를 반환하고, 빌드 완료 후 `Environment.tearDown()`이 호출되는 구조가 필요하다.

### 15.3 왜 Trigger.checkTriggers()에서 SCMTrigger를 특별 처리하는가?

```java
// Trigger.java 259~283행
if (scmd.synchronousPolling) {
    previousSynchronousPolling = scmd.getExecutor().submit(
        new DependencyRunner(p -> {
            for (Trigger t : p.getTriggers().values()) {
                if (t instanceof SCMTrigger) {
                    t.run();
                }
            }
        }));
}
```

SCMTrigger의 동기 폴링 모드는 프로젝트 간 **의존성 순서**를 보장한다. 예를 들어:

```
프로젝트 A → 프로젝트 B (A에 의존)

비동기 모드: A와 B가 동시에 SCM 폴링 → B가 A의 변경을 놓칠 수 있음
동기 모드:   A 폴링 완료 → B 폴링 → 의존성 순서 보장
```

`DependencyRunner`가 의존성 그래프를 따라 순서대로 트리거를 실행한다.

### 15.4 왜 BuildStepMonitor의 기본값이 BUILD인가?

이것은 "안전한 기본값(safe default)" 원칙의 적용이다.

```
Jenkins 1.319 이전: 동일 잡의 빌드가 순차 실행됨
Jenkins 1.319 이후: 동시 빌드 허용

기존 플러그인 가정:
"이전 빌드의 결과는 항상 사용 가능하다"
→ Run.getPreviousBuild()가 완료된 빌드를 반환한다고 가정

BUILD 기본값의 효과:
→ 기존 플러그인이 코드 변경 없이도 안전하게 작동
→ 성능은 떨어지지만 정확성은 보장
→ 플러그인 개발자가 명시적으로 NONE/STEP으로 변경하도록 유도
```

### 15.5 왜 Publisher 생성자가 @Deprecated인가?

```java
// Publisher.java 67~73행
@Deprecated
protected Publisher() {}
```

이것은 Java 타입 시스템의 한계를 보완하기 위한 패턴이다. `Publisher`를 `abstract`로 선언하면 직접 인스턴스화는 방지할 수 있지만, 직접 상속하는 것은 방지할 수 없다. `@Deprecated` 경고를 통해 개발자가 `Recorder` 또는 `Notifier`를 상속하도록 유도한다.

`Recorder`와 `Notifier`의 생성자에서 이 경고를 억제하는 것도 확인할 수 있다:

```java
// Recorder.java 49~50행
@SuppressWarnings("deprecation") // super only @Deprecated to discourage other subclasses
protected Recorder() {}

// Notifier.java 49~50행
@SuppressWarnings("deprecation") // super only @Deprecated to discourage other subclasses
protected Notifier() {}
```

### 15.6 영속성 모델 선택의 이유

BuildStep이 XStream 직렬화를 사용하는 이유:

```
1. 잡 설정 = XML 파일 (config.xml)
   → 사람이 읽고 수정할 수 있어야 함
   → DB가 아닌 파일 시스템 기반 저장

2. 설정 변경 빈도가 낮음
   → 사용자가 저장 버튼을 누를 때만 직렬화
   → 실행 시에는 메모리에서 참조

3. 생성자 없이 복원
   → 플러그인 업데이트 시 생성자 시그니처가 바뀌어도 기존 설정 로드 가능
   → 단, transient 필드는 null이 되므로 방어적 코딩 필요

4. 주의점:
   → "처리용 객체(parser 등)"는 반드시 transient로 마킹
   → 복원 후 null 체크 필수
   → 직렬화 대상은 "설정 데이터"만
```

---

## 요약

Jenkins 빌드 파이프라인의 핵심 아키텍처를 정리하면 다음과 같다:

```
+----------------------------------------------------------------------+
|                        Jenkins 빌드 파이프라인                          |
+----------------------------------------------------------------------+
|                                                                      |
|  [Trigger]  cron/SCM/webhook → Queue.schedule()                      |
|      |                                                               |
|      v                                                               |
|  [BuildWrapper]                                                      |
|      | decorateLogger() → decorateLauncher() → preCheckout()         |
|      | → SCM checkout → setUp() → Environment 반환                   |
|      |                                                               |
|      v                                                               |
|  [Builder]  prebuild() → perform()   ← BuildStepMonitor 제어        |
|      |      (NONE 기본값)              ← CheckPoint 동기화            |
|      |                                                               |
|      v                                                               |
|  [Publisher]                                                         |
|      | Recorder.perform()    ← classify()==0, 빌드 상태 변경 가능      |
|      | 미분류.perform()       ← classify()==1                          |
|      | Notifier.perform()    ← classify()==2, 빌드 상태 확정 후        |
|      | (finalized Publisher) ← needsToRunAfterFinalized()==true       |
|      |                                                               |
|      v                                                               |
|  [Environment.tearDown()]  항상 실행 (실패 시에도)                     |
|                                                                      |
+----------------------------------------------------------------------+

확장점 등록:
  - @Extension on Descriptor → 자동 발견
  - BuildStepDescriptor.isApplicable() → 프로젝트 타입 필터링
  - Builder.all() / Publisher.all() / Trigger.all() → 등록 목록 조회

동시성 제어:
  - BuildStepMonitor.BUILD/STEP/NONE → 외부 동기화 수준
  - CheckPoint.block()/report() → 세밀한 동기화
  - COMPLETED/MAIN_COMPLETED/CULPRITS_DETERMINED → 표준 체크포인트
```

이 파이프라인 구조는 Jenkins 초기(Hudson 시절)부터 진화해온 결과물이다. 핵심 설계 원칙은 다음과 같다:

1. **하위 호환성 우선**: `BuildStepCompatibilityLayer`, `PublisherList.KIND` 등으로 구형 플러그인과의 호환성을 보장한다.
2. **안전한 기본값**: `BuildStepMonitor.BUILD`가 기본값이어서, 기존 플러그인이 동시 빌드 환경에서도 안전하게 동작한다.
3. **관심사 분리**: `Recorder`(결과 수집) → `Notifier`(알림)의 정렬로, 빌드 결과가 확정된 후에만 알림이 발송된다.
4. **리소스 관리 보장**: `BuildWrapper`의 `setUp()`/`tearDown()` 패턴으로, 빌드 실패 시에도 정리 작업이 실행된다.
5. **점진적 현대화**: `SimpleBuildStep`으로 Pipeline 호환 API를 제공하면서, 기존 `BuildStep` API도 유지한다.
