# 18. SCM / View / Fingerprint 시스템

Jenkins의 소스 코드 관리(SCM) 추상화, 뷰(View) 시스템, 아티팩트 추적(Fingerprint) 시스템을 심층 분석한다.
이 세 시스템은 각각 "소스를 어떻게 가져올 것인가", "작업을 어떻게 보여줄 것인가", "빌드 산출물을 어떻게 추적할 것인가"라는 핵심 질문에 답한다.

---

## 목차

1. [SCM 추상화 계층](#1-scm-추상화-계층)
2. [SCM 핵심 메서드](#2-scm-핵심-메서드)
3. [NullSCM: 기본 SCM 구현](#3-nullscm-기본-scm-구현)
4. [SCMDescriptor: SCM 등록과 검색](#4-scmdescriptor-scm-등록과-검색)
5. [SCMRevisionState: 리비전 상태 표현](#5-scmrevisionstate-리비전-상태-표현)
6. [PollingResult: 변경 감지 결과](#6-pollingresult-변경-감지-결과)
7. [ChangeLogSet: 변경 로그 모델](#7-changelogset-변경-로그-모델)
8. [SCMTrigger: 폴링 기반 빌드 트리거](#8-scmtrigger-폴링-기반-빌드-트리거)
9. [SCM 폴링 전체 흐름](#9-scm-폴링-전체-흐름)
10. [View 시스템 아키텍처](#10-view-시스템-아키텍처)
11. [ListView: 리스트 뷰 구현](#11-listview-리스트-뷰-구현)
12. [AllView: 전체 뷰](#12-allview-전체-뷰)
13. [ViewJobFilter와 ListViewColumn: 뷰 확장](#13-viewjobfilter와-listviewcolumn-뷰-확장)
14. [Fingerprint: 아티팩트 추적 시스템](#14-fingerprint-아티팩트-추적-시스템)
15. [왜 이렇게 설계했는가](#15-왜-이렇게-설계했는가)
16. [전체 클래스 관계도](#16-전체-클래스-관계도)

---

## 1. SCM 추상화 계층

### 클래스 선언

```
파일: core/src/main/java/hudson/scm/SCM.java
```

```java
@ExportedBean
public abstract class SCM implements Describable<SCM>, ExtensionPoint {
    // ...
}
```

SCM은 Jenkins의 소스 코드 관리 시스템에 대한 **최상위 추상화**다.
`Describable<SCM>`과 `ExtensionPoint`를 동시에 구현함으로써 두 가지 핵심 설계 요소를 달성한다.

| 인터페이스 | 역할 |
|-----------|------|
| `Describable<SCM>` | Descriptor/Describable 패턴을 통한 UI 자동 생성, 폼 바인딩, 직렬화 |
| `ExtensionPoint` | 플러그인이 새로운 SCM 구현(Git, SVN, Mercurial 등)을 등록할 수 있는 확장점 |

### SCM 확장 메커니즘

```
┌──────────────────────────────────┐
│          SCM (abstract)          │
│  implements Describable,         │
│            ExtensionPoint        │
├──────────────────────────────────┤
│  + checkout()                    │
│  + calcRevisionsFromBuild()      │
│  + compareRemoteRevisionWith()   │
│  + createChangeLogParser()       │
│  + poll()                        │
│  + supportsPolling()             │
│  + requiresWorkspaceForPolling() │
│  + getEffectiveBrowser()         │
│  + getModuleRoot()               │
│  + buildEnvironment()            │
└──────┬───────────────────────────┘
       │ extends
       ├─── NullSCM         (내장: SCM 미설정)
       ├─── GitSCM           (플러그인: git-plugin)
       ├─── SubversionSCM    (플러그인: svn-plugin)
       ├─── MercurialSCM     (플러그인: mercurial-plugin)
       └─── ...              (그 외 플러그인)
```

### 권한 시스템

SCM은 자체 권한 그룹을 정의한다.

```java
// core/src/main/java/hudson/scm/SCM.java (756-761행)
public static final PermissionGroup PERMISSIONS =
    new PermissionGroup(SCM.class, Messages._SCM_Permissions_Title());

public static final Permission TAG =
    new Permission(PERMISSIONS, "Tag",
        Messages._SCM_TagPermission_Description(),
        Permission.CREATE, PermissionScope.ITEM);
```

`TAG` 권한은 SCM 태그 생성(릴리스 마킹 등)에 사용된다. `Permission.CREATE`를 부모로 가지므로, 기본적으로 `CREATE` 권한이 있는 사용자에게 허용된다.

---

## 2. SCM 핵심 메서드

### 2.1 checkout() -- 소스 코드 체크아웃

```java
// core/src/main/java/hudson/scm/SCM.java (499-535행)
public void checkout(
        @NonNull Run<?, ?> build,
        @NonNull Launcher launcher,
        @NonNull FilePath workspace,
        @NonNull TaskListener listener,
        @CheckForNull File changelogFile,
        @CheckForNull SCMRevisionState baseline)
        throws IOException, InterruptedException
```

빌드가 시작될 때 워크스페이스에 소스 코드를 가져오는 **가장 핵심적인 메서드**다.

| 파라미터 | 설명 |
|---------|------|
| `build` | 현재 실행 중인 빌드 (`Run<?,?>`) |
| `launcher` | 원격 머신에서 명령을 실행하기 위한 추상화 |
| `workspace` | 소스 코드를 체크아웃할 디렉토리 (`FilePath`는 원격 파일 시스템도 지원) |
| `listener` | 로그 출력용 리스너 |
| `changelogFile` | 변경 로그를 기록할 파일. null이면 변경 로그 미생성 |
| `baseline` | 이전 빌드의 리비전 상태. 변경 로그 생성 시 기준점 |

체크아웃은 신규 체크아웃일 수도 있고, 기존 워크스페이스의 업데이트일 수도 있다. SCM 구현체가 판단한다.

### 2.2 calcRevisionsFromBuild() -- 리비전 상태 계산

```java
// core/src/main/java/hudson/scm/SCM.java (335-341행)
public @CheckForNull SCMRevisionState calcRevisionsFromBuild(
        @NonNull Run<?, ?> build,
        @Nullable FilePath workspace,
        @Nullable Launcher launcher,
        @NonNull TaskListener listener)
        throws IOException, InterruptedException
```

빌드 완료 후 워크스페이스의 SCM 리비전 상태를 계산한다. 이 결과는 `Action`으로 빌드에 첨부되어 다음 폴링의 기준(baseline)이 된다.

**최적화 포인트**: SCM 구현체는 `checkout()` 도중에 `SCMRevisionState`를 직접 계산하여 `Action`으로 추가할 수 있다. 이 경우 `calcRevisionsFromBuild()`는 호출되지 않는다.

### 2.3 compareRemoteRevisionWith() -- 원격 변경 감지

```java
// core/src/main/java/hudson/scm/SCM.java (394-415행)
public PollingResult compareRemoteRevisionWith(
        @NonNull Job<?, ?> project,
        @Nullable Launcher launcher,
        @Nullable FilePath workspace,
        @NonNull TaskListener listener,
        @NonNull SCMRevisionState baseline)
        throws IOException, InterruptedException
```

원격 리포지토리의 현재 상태와 baseline을 비교하여 변경 사항이 있는지 판단한다.

**설계 의도**: 두 리포지토리 상태를 독립적으로 구성한 뒤 비교하는 것은 비용이 크다. 그래서 Jenkins는 (1) 리포지토리 상태 구축과 (2) 비교 행위를 하나의 메서드로 합쳤다. SCM 구현체가 더 효율적으로 구현할 수 있도록 한 것이다.

### 2.4 poll() -- 폴링 편의 메서드

```java
// core/src/main/java/hudson/scm/SCM.java (425-440행)
public final PollingResult poll(
        AbstractProject<?, ?> project,
        Launcher launcher,
        FilePath workspace,
        TaskListener listener,
        SCMRevisionState baseline)
        throws IOException, InterruptedException
```

`poll()`은 **final 메서드**로, 내부에서 API 버전 호환성을 처리한다.

```
poll() 내부 동작:
┌─────────────────────────────────────────────┐
│ 1. 1.346+ API를 지원하는가?                   │
│    ├── YES: compareRemoteRevisionWith() 호출  │
│    │   - baseline이 NONE이면                  │
│    │     calcRevisionsFromBuild()로 재계산     │
│    └── NO: 레거시 pollChanges() 호출           │
│         - true → SIGNIFICANT                 │
│         - false → NO_CHANGES                 │
└─────────────────────────────────────────────┘
```

### 2.5 createChangeLogParser() -- 변경 로그 파서

```java
// core/src/main/java/hudson/scm/SCM.java (719행)
public abstract ChangeLogParser createChangeLogParser();
```

유일한 **순수 추상 메서드**다. 모든 SCM 구현체는 자신의 변경 로그 형식을 파싱할 수 있는 `ChangeLogParser`를 제공해야 한다.

### 2.6 그 외 중요 메서드

| 메서드 | 설명 |
|--------|------|
| `supportsPolling()` | 폴링 지원 여부. 기본값 `true` |
| `requiresWorkspaceForPolling()` | 폴링에 워크스페이스가 필요한가. 기본값 `true` |
| `processWorkspaceBeforeDeletion()` | 워크스페이스 삭제 전 정리 기회 제공 |
| `buildEnvironment()` | 빌드 환경 변수 추가 (예: SVN_REVISION) |
| `getModuleRoot()` | 체크아웃된 모듈의 최상위 디렉토리 반환 |
| `getModuleRoots()` | 다중 모듈 체크아웃 시 모든 루트 반환 |
| `getEffectiveBrowser()` | 적용 가능한 RepositoryBrowser 반환 |
| `guessBrowser()` | URL 등으로 RepositoryBrowser 추측 |
| `getKey()` | SCM 설정 구분 키 (기본값: `getType()`) |

---

## 3. NullSCM: 기본 SCM 구현

```
파일: core/src/main/java/hudson/scm/NullSCM.java
```

```java
public class NullSCM extends SCM {

    @DataBoundConstructor
    public NullSCM() {}

    @Override
    public SCMRevisionState calcRevisionsFromBuild(...) {
        return null;  // 리비전 없음
    }

    @Override
    public PollingResult compareRemoteRevisionWith(...) {
        return PollingResult.NO_CHANGES;  // 변경 없음
    }

    @Override
    public void checkout(...) {
        if (changelogFile != null) {
            createEmptyChangeLog(changelogFile, listener, "log");
        }
        // 아무 코드도 체크아웃하지 않음
    }

    @Override
    public ChangeLogParser createChangeLogParser() {
        return NullChangeLogParser.INSTANCE;
    }
}
```

### NullSCM의 역할

| 동작 | 반환값/행위 |
|------|-----------|
| 리비전 계산 | `null` 반환 |
| 변경 감지 | 항상 `NO_CHANGES` |
| 체크아웃 | 빈 변경 로그만 생성 |
| 파서 | `NullChangeLogParser.INSTANCE` |

**Null Object 패턴**의 전형적인 구현이다. SCM을 설정하지 않은 프로젝트에서 `null` 체크 없이 안전하게 SCM 메서드를 호출할 수 있게 한다.

### Descriptor 등록

```java
@Extension(ordinal = Integer.MAX_VALUE) @Symbol("none")
public static class DescriptorImpl extends SCMDescriptor<NullSCM> {
    public DescriptorImpl() {
        super(null);  // RepositoryBrowser 없음
    }
}
```

`ordinal = Integer.MAX_VALUE`로 설정하여 SCM 선택 목록에서 **가장 먼저** 표시된다. `@Symbol("none")`은 Pipeline DSL에서 `scm none` 형태로 사용할 수 있게 한다.

---

## 4. SCMDescriptor: SCM 등록과 검색

```
파일: core/src/main/java/hudson/scm/SCMDescriptor.java
```

```java
public abstract class SCMDescriptor<T extends SCM> extends Descriptor<SCM> {
    public final transient Class<? extends RepositoryBrowser> repositoryBrowser;
    // ...
}
```

### 핵심 필드와 메서드

| 멤버 | 설명 |
|------|------|
| `repositoryBrowser` | 이 SCM과 호환되는 RepositoryBrowser 타입. `null`이면 브라우저 미지원 |
| `isApplicable(Job)` | 특정 프로젝트에 이 SCM을 설정할 수 있는지 여부 |
| `getBrowserDescriptors()` | 호환되는 RepositoryBrowser Descriptor 목록 |
| `getGeneration()` (deprecated) | SCM 인스턴스 생성 카운터. 캐시 무효화에 사용 |

### 적용 가능성 판단 로직

```java
// core/src/main/java/hudson/scm/SCMDescriptor.java (142-157행)
public boolean isApplicable(Job project) {
    if (project instanceof AbstractProject) {
        return isApplicable((AbstractProject) project);
    } else {
        return false;  // Pipeline 등 Job에는 기본적으로 false
    }
}
```

**중요**: `SCMDescriptor.isApplicable()`은 `AbstractProject`에 대해서만 기본적으로 `true`를 반환한다. Pipeline 작업(`Job`)에서는 `false`를 반환한다. Pipeline은 `checkout` 스텝을 통해 별도의 SCM 통합 경로를 사용하기 때문이다.

### repositoryBrowser 직렬화 보호

```java
// core/src/main/java/hudson/scm/SCMDescriptor.java (100-114행)
@Override
public void load() {
    Class<? extends RepositoryBrowser> rb = repositoryBrowser;
    super.load();
    if (repositoryBrowser != rb) {
        // XStream이 final 필드를 덮어쓸 수 있으므로 복구
        Field f = SCMDescriptor.class.getDeclaredField("repositoryBrowser");
        f.setAccessible(true);
        f.set(this, rb);
    }
}
```

XStream 역직렬화 과정에서 `final transient` 필드인 `repositoryBrowser`가 오래된 설정 파일에 의해 덮어쓰이는 버그(JENKINS-4514)를 방어한다. `load()` 후 원래 값을 리플렉션으로 복구한다.

---

## 5. SCMRevisionState: 리비전 상태 표현

```
파일: core/src/main/java/hudson/scm/SCMRevisionState.java
```

```java
public abstract class SCMRevisionState implements Action {
    public static SCMRevisionState NONE = new None();

    private static final class None extends SCMRevisionState {}

    @Override public String getIconFileName() { return null; }
    @Override public String getDisplayName() { return null; }
    @Override public String getUrlName() { return null; }
}
```

### 설계 결정

SCMRevisionState는 **의도적으로 Comparable을 구현하지 않는다**. 소스코드 주석에 그 이유가 명시되어 있다.

```
// core/src/main/java/hudson/scm/SCMRevisionState.java (43-50행)
/*
  I can't really make this comparable because comparing two revision
  states often requires non-trivial computation and conversations
  with the repository (mainly to figure out which changes are
  insignificant and which are not.)

  So instead, here we opt to a design where we tell SCM upfront
  about what we are comparing against (baseline), and have it give
  us the new state and degree of change in PollingResult.
*/
```

두 리비전을 비교하는 것은 단순 비교가 아니라 리포지토리와의 통신이 필요한 복잡한 연산이다. 따라서 비교 로직을 `compareRemoteRevisionWith()`에 위임하고, 결과를 `PollingResult`로 반환한다.

### NONE 상수

`SCMRevisionState.NONE`은 "아직 리비전을 모르는 상태"를 나타낸다. `poll()` 메서드에서 baseline이 `NONE`이면 `calcRevisionsFromBuild()`를 호출하여 실제 리비전을 계산한다.

### Action으로서의 역할

`SCMRevisionState`는 `Action`을 구현하지만 세 메서드 모두 `null`을 반환한다. 이는 UI에 표시할 필요 없이 빌드의 메타데이터로만 사용하겠다는 의미다. `Action`을 구현하는 이유는 `Run.getAction(SCMRevisionState.class)` 형태로 빌드에서 쉽게 조회하기 위함이다.

---

## 6. PollingResult: 변경 감지 결과

```
파일: core/src/main/java/hudson/scm/PollingResult.java
```

```java
public final class PollingResult implements SerializableOnlyOverRemoting {
    public final @CheckForNull SCMRevisionState baseline;
    public final @CheckForNull SCMRevisionState remote;
    public final @NonNull Change change;
}
```

### Change enum

```java
// core/src/main/java/hudson/scm/PollingResult.java (48-79행)
public enum Change {
    NONE,           // 변경 없음
    INSIGNIFICANT,  // 무시 가능한 변경
    SIGNIFICANT,    // 빌드가 필요한 변경
    INCOMPARABLE    // 비교 불가 → 즉시 빌드
}
```

| 값 | 의미 | 빌드 트리거 |
|----|------|-----------|
| `NONE` | 동일한 리비전, 변경 없음 | X |
| `INSIGNIFICANT` | 변경 있지만 무시 가능 (예: 특정 경로 제외) | X |
| `SIGNIFICANT` | 의미 있는 변경. quiet period 적용 후 빌드 | O (지연) |
| `INCOMPARABLE` | 비교 불가 (설정 변경 등). 즉시 빌드 | O (즉시) |

### hasChanges() 판정 로직

```java
// core/src/main/java/hudson/scm/PollingResult.java (92-94행)
public boolean hasChanges() {
    return change.ordinal() > Change.INSIGNIFICANT.ordinal();
}
```

`SIGNIFICANT`(ordinal=2)와 `INCOMPARABLE`(ordinal=3)만 `true`를 반환한다. `INSIGNIFICANT`는 변경이 있지만 빌드를 트리거하지 않는다.

### 미리 정의된 상수

```java
public static final PollingResult NO_CHANGES  = new PollingResult(Change.NONE);
public static final PollingResult SIGNIFICANT = new PollingResult(Change.SIGNIFICANT);
public static final PollingResult BUILD_NOW   = new PollingResult(Change.INCOMPARABLE);
```

### 3-값 구조의 의미

`PollingResult`가 `baseline`, `remote`, `change` 세 값을 분리한 이유:

1. **`change`가 `baseline`/`remote`와 독립**: `INCOMPARABLE` 상태에서는 baseline과 remote를 비교할 수 없지만 빌드는 필요하다
2. **제외 기능 지원**: SCM이 특정 변경을 무시(`INSIGNIFICANT`)하면서도 remote 상태는 업데이트해야 한다
3. **상태 전달**: `remote` 값은 다음 폴링의 `baseline`으로 전달된다

---

## 7. ChangeLogSet: 변경 로그 모델

```
파일: core/src/main/java/hudson/scm/ChangeLogSet.java
```

```java
@ExportedBean(defaultVisibility = 999)
public abstract class ChangeLogSet<T extends ChangeLogSet.Entry>
        implements Iterable<T> {
    private final Run<?, ?> run;
    private final RepositoryBrowser<?> browser;
}
```

### 클래스 구조

```
ChangeLogSet<T extends Entry>
│  implements Iterable<T>
│
├── Run<?,?> run          -- 이 변경 목록이 속한 빌드
├── RepositoryBrowser<?> browser  -- 코드 탐색 연결
│
├── isEmptySet(): boolean    -- 변경 사항 없음 여부
├── getItems(): Object[]     -- 모든 변경 항목 (REST API용)
├── getKind(): String        -- SCM 종류 식별자 ("git", "svn" 등)
│
└── Entry (abstract static inner class)
    ├── getCommitId(): String       -- 커밋 해시/리비전 번호
    ├── getTimestamp(): long         -- 커밋 시각
    ├── getMsg(): String            -- 커밋 메시지
    ├── getAuthor(): User           -- 커밋 작성자
    ├── getAffectedPaths(): Collection<String>  -- 변경된 파일 경로
    ├── getAffectedFiles(): Collection<AffectedFile>  -- 변경 파일 상세
    ├── getMsgAnnotated(): String   -- 마크업된 커밋 메시지
    └── getMsgEscaped(): String     -- HTML 이스케이프된 메시지
```

### Entry 클래스 상세

```java
// core/src/main/java/hudson/scm/ChangeLogSet.java (140-271행)
@ExportedBean(defaultVisibility = 999)
public abstract static class Entry {
    private ChangeLogSet parent;

    @Exported public String getCommitId() { return null; }
    @Exported public long getTimestamp() { return -1; }
    @Exported public abstract String getMsg();
    @Exported public abstract User getAuthor();
    @Exported public abstract Collection<String> getAffectedPaths();
}
```

| 메서드 | 기본값 | 설명 |
|--------|--------|------|
| `getCommitId()` | `null` | CVS처럼 파일별 리비전인 SCM에서는 단일 식별자가 없음 |
| `getTimestamp()` | `-1` | CVS처럼 커밋이 시간에 걸쳐 분산되는 SCM에서는 단일 타임스탬프 없음 |
| `getMsg()` | 추상 | 모든 SCM은 커밋 메시지를 제공해야 함 |
| `getAuthor()` | 추상 | 모든 SCM은 작성자를 제공해야 함 |
| `getAffectedPaths()` | 추상 | 변경된 파일 경로 목록 |

### AffectedFile 인터페이스

```java
// core/src/main/java/hudson/scm/ChangeLogSet.java (281-297행)
public interface AffectedFile {
    String getPath();
    EditType getEditType();
}
```

### EditType 상수

```java
// core/src/main/java/hudson/scm/EditType.java (57-61행)
public static final EditType ADD    = new EditType("add", "The file was added");
public static final EditType EDIT   = new EditType("edit", "The file was modified");
public static final EditType DELETE = new EditType("delete", "The file was removed");
```

### ChangeLogAnnotator와 메시지 마크업

```java
// core/src/main/java/hudson/scm/ChangeLogSet.java (249-261행)
public String getMsgAnnotated() {
    MarkupText markup = new MarkupText(getMsg());
    for (ChangeLogAnnotator a : ChangeLogAnnotator.all())
        try {
            a.annotate(parent.run, this, markup);
        } catch (RuntimeException e) { /* 로그 후 무시 */ }
    return markup.toString(false);
}
```

`ChangeLogAnnotator`는 커밋 메시지에 하이퍼링크를 추가하는 확장점이다. 예를 들어 "JIRA-123"을 JIRA 이슈 링크로, "#42"를 빌드 번호 링크로 변환한다.

### EmptyChangeLogSet

```java
// core/src/main/java/hudson/scm/ChangeLogSet.java (131-133행)
public static ChangeLogSet<? extends ChangeLogSet.Entry> createEmpty(Run build) {
    return new EmptyChangeLogSet(build);
}
```

변경 사항이 없는 빌드에서 사용할 빈 ChangeLogSet을 팩토리 메서드로 제공한다.

---

## 8. SCMTrigger: 폴링 기반 빌드 트리거

```
파일: core/src/main/java/hudson/triggers/SCMTrigger.java
```

```java
public class SCMTrigger extends Trigger<Item> {
    private boolean ignorePostCommitHooks;

    @DataBoundConstructor
    public SCMTrigger(String scmpoll_spec) {
        super(scmpoll_spec);
    }
}
```

### 핵심 설정

| 필드 | 설명 |
|------|------|
| `scmpoll_spec` | cron 표현식 (예: `H/5 * * * *` - 5분마다) |
| `ignorePostCommitHooks` | `true`이면 webhook에 의한 폴링을 무시 |

### run() 메서드 -- 폴링 실행

```java
// core/src/main/java/hudson/triggers/SCMTrigger.java (160-194행)
@Override
public void run() {
    if (job == null) return;
    run(null);
}

public void run(Action[] additionalActions) {
    DescriptorImpl d = getDescriptor();
    if (d.synchronousPolling) {
        new Runner(additionalActions).run();  // 동기 실행
    } else {
        d.queue.execute(new Runner(additionalActions));  // 비동기 큐
        d.clogCheck();  // 큐 정체 확인
    }
}
```

### DescriptorImpl -- 스레드 풀 관리

```java
// core/src/main/java/hudson/triggers/SCMTrigger.java (218-356행)
@Extension @Symbol("pollSCM")
public static class DescriptorImpl extends TriggerDescriptor
        implements PersistentDescriptor {

    private final transient SequentialExecutionQueue queue =
        new SequentialExecutionQueue(
            Executors.newSingleThreadExecutor(threadFactory()));

    public boolean synchronousPolling = false;
    private int maximumThreads = 10;

    private static final int THREADS_LOWER_BOUND = 5;
    private static final int THREADS_UPPER_BOUND = 100;
    private static final int THREADS_DEFAULT = 10;
}
```

| 설정 | 기본값 | 설명 |
|------|--------|------|
| `synchronousPolling` | `false` | `true`이면 cron 스레드에서 직접 폴링 |
| `maximumThreads` | 10 | 폴링 스레드 풀 크기 (5~100) |

### SequentialExecutionQueue의 의미

`SequentialExecutionQueue`는 동일한 프로젝트에 대한 폴링 요청을 **합쳐준다**. 즉, 이미 폴링 대기 중인 프로젝트에 새 폴링 요청이 들어오면 기존 요청에 병합된다. `Runner.equals()`가 이를 가능하게 한다.

```java
// core/src/main/java/hudson/triggers/SCMTrigger.java (690-691행)
@Override
public boolean equals(Object that) {
    return that instanceof Runner && job == ((Runner) that)._job();
}
```

### Runner 클래스 -- 실제 폴링 수행

```java
// core/src/main/java/hudson/triggers/SCMTrigger.java (555-686행)
public class Runner implements Runnable {
    private volatile long startTime;
    private Action[] additionalActions;

    private boolean runPolling() {
        StreamTaskListener listener = new StreamTaskListener(getLogFile(), ...);
        boolean result = job().poll(listener).hasChanges();
        return result;
    }

    @Override
    public void run() {
        // SCMDecisionHandler에 의한 거부권 확인
        SCMDecisionHandler veto = SCMDecisionHandler.firstShouldPollVeto(job);
        if (veto != null) {
            // 폴링 건너뜀
            return;
        }

        if (runPolling()) {
            // 변경 감지 → 빌드 스케줄링
            SCMTriggerCause cause = new SCMTriggerCause(getLogFile());
            Action[] queueActions = new Action[additionalActions.length + 1];
            queueActions[0] = new CauseAction(cause);
            // ...
            p.scheduleBuild2(p.getQuietPeriod(), queueActions);
        }
    }
}
```

### 큐 정체 모니터링

```java
// core/src/main/java/hudson/triggers/SCMTrigger.java (269-279행)
public boolean isClogged() {
    return queue.isStarving(STARVATION_THRESHOLD);
}

public void clogCheck() {
    AdministrativeMonitor.all()
        .get(AdministrativeMonitorImpl.class).on = isClogged();
}
```

폴링 큐가 정체되면 `AdministrativeMonitorImpl`이 활성화되어 관리자에게 경고한다. 스레드 풀 크기를 늘리라는 신호다.

### BuildAction과 SCMAction

SCMTrigger는 두 종류의 Action을 생성한다.

| Action | 부착 대상 | 목적 |
|--------|----------|------|
| `SCMAction` | Job | 프로젝트 수준의 폴링 로그 표시 |
| `BuildAction` | Run(Build) | 해당 빌드를 트리거한 폴링 로그 표시 |

---

## 9. SCM 폴링 전체 흐름

```
┌───────────────────────────────────────────────────────────────────┐
│                     SCM 폴링 → 빌드 트리거 흐름                    │
├───────────────────────────────────────────────────────────────────┤
│                                                                   │
│  [1] cron 스케줄러                                                │
│   │  (예: "H/5 * * * *")                                         │
│   │                                                               │
│   ▼                                                               │
│  [2] SCMTrigger.run()                                             │
│   │  - synchronousPolling 확인                                    │
│   │  - SequentialExecutionQueue에 Runner 등록                     │
│   │                                                               │
│   ▼                                                               │
│  [3] Runner.run()                                                 │
│   │  - SCMDecisionHandler 거부권 확인                              │
│   │  - runPolling() 호출                                          │
│   │                                                               │
│   ▼                                                               │
│  [4] SCMTriggerItem.poll(listener)                                │
│   │  - SCM.poll() 호출                                            │
│   │                                                               │
│   ▼                                                               │
│  [5] SCM.poll()                                                   │
│   │  ├── baseline == NONE?                                        │
│   │  │   └── YES: calcRevisionsFromBuild()로 baseline 재계산      │
│   │  └── compareRemoteRevisionWith(baseline) 호출                 │
│   │                                                               │
│   ▼                                                               │
│  [6] PollingResult 반환                                           │
│   │  - hasChanges() → SIGNIFICANT 또는 INCOMPARABLE               │
│   │                                                               │
│   ▼                                                               │
│  [7] 변경 감지 시:                                                 │
│      - SCMTriggerCause 생성 (폴링 로그 포함)                       │
│      - scheduleBuild2(quietPeriod, causeAction)                   │
│      - 빌드 큐에 추가                                              │
│                                                                   │
│   ▼                                                               │
│  [8] 빌드 실행:                                                    │
│      - SCM.checkout() → 워크스페이스에 소스 체크아웃                 │
│      - changelogFile에 변경 로그 기록                               │
│      - calcRevisionsFromBuild() → 다음 폴링 baseline 저장          │
│                                                                   │
└───────────────────────────────────────────────────────────────────┘
```

---

## 10. View 시스템 아키텍처

```
파일: core/src/main/java/hudson/model/View.java
```

```java
// core/src/main/java/hudson/model/View.java (148행)
@ExportedBean
public abstract class View extends AbstractModelObject
        implements AccessControlled, Describable<View>, ExtensionPoint,
                   Saveable, ModelObjectWithChildren,
                   DescriptorByNameOwner, HasWidgets, Badgeable {
```

### View가 구현하는 인터페이스

| 인터페이스 | 역할 |
|-----------|------|
| `AccessControlled` | 권한 검사 지원 (CREATE, DELETE, CONFIGURE, READ) |
| `Describable<View>` | Descriptor/Describable 패턴으로 UI 폼 자동 생성 |
| `ExtensionPoint` | 플러그인이 새로운 뷰 타입 등록 가능 |
| `Saveable` | XML 직렬화를 통한 영속화 |
| `ModelObjectWithChildren` | 컨텍스트 메뉴에서 자식 항목 표시 |
| `DescriptorByNameOwner` | 이름으로 Descriptor 검색 |
| `HasWidgets` | 위젯(빌드 큐, 실행자 등) 표시 |
| `Badgeable` | 배지 아이콘 표시 |

### 핵심 필드

```java
// core/src/main/java/hudson/model/View.java (150-180행)
protected /*final*/ ViewGroup owner;    // 이 뷰를 소유한 그룹
protected String name;                  // 뷰 이름
protected String description;           // 설명 (HTML)
protected boolean filterExecutors;      // 관련 실행자만 표시
protected boolean filterQueue;          // 관련 큐 항목만 표시
private volatile DescribableList<ViewProperty, ViewPropertyDescriptor> properties;
```

### 추상 메서드

```java
// 이 뷰에 포함된 아이템 목록 (핵심)
@Exported(name = "jobs")
public abstract Collection<TopLevelItem> getItems();

// 특정 아이템이 이 뷰에 포함되는지 여부
public abstract boolean contains(TopLevelItem item);
```

### View의 주요 구체 메서드

```java
// 뷰 이름 반환 (URL에서 사용)
@Exported(visibility = 2, name = "name")
public String getViewName() { return name; }

// 기본 뷰인가?
public boolean isDefault() {
    return getOwner().getPrimaryView() == this;
}

// 뷰 저장 → 실제로는 owner에게 위임
public void save() throws IOException {
    if (owner != null) owner.save();
}

// 이 뷰에 속한 빌드 목록
public RunList getBuilds() {
    return new RunList(this);
}
```

### 뷰 권한 시스템

```java
// core/src/main/java/hudson/model/View.java (1125-1132행)
public static final PermissionGroup PERMISSIONS =
    new PermissionGroup(View.class, Messages._View_Permissions_Title());

public static final Permission CREATE    = new Permission(PERMISSIONS, "Create", ...);
public static final Permission DELETE    = new Permission(PERMISSIONS, "Delete", ...);
public static final Permission CONFIGURE = new Permission(PERMISSIONS, "Configure", ...);
public static final Permission READ      = new Permission(PERMISSIONS, "Read", ...);
```

### 뷰 생성 팩토리

```java
// core/src/main/java/hudson/model/View.java (1155-1205행)
public static View create(StaplerRequest2 req, StaplerResponse2 rsp,
                           ViewGroup owner) throws ... {
    String mode = req.getParameter("mode");
    String name = req.getParameter("name");

    if ("copy".equals(mode)) {
        // 기존 뷰 복사
        v = copy(req, owner, name);
    } else {
        // ViewDescriptor를 찾아 새 인스턴스 생성
        ViewDescriptor descriptor = all().findByName(mode);
        v = descriptor.newInstance(req, submittedForm);
    }
    return v;
}
```

### 큐 필터링

```java
// core/src/main/java/hudson/model/View.java (500-538행)
private List<Queue.Item> filterQueue(List<Queue.Item> base) {
    if (!isFilterQueue()) return base;
    Collection<TopLevelItem> items = getItems();
    return base.stream()
        .filter(qi -> filterQueueItemTest(qi, items))
        .collect(Collectors.toList());
}
```

`filterQueue`가 `true`이면, 이 뷰에 속한 작업의 큐 항목만 표시한다. Pipeline의 하위 태스크도 `getOwnerTask()` 체인을 따라가며 검사한다.

---

## 11. ListView: 리스트 뷰 구현

```
파일: core/src/main/java/hudson/model/ListView.java
```

```java
public class ListView extends View implements DirectlyModifiableView {
    @GuardedBy("this")
    /*package*/ /*almost-final*/ SortedSet<String> jobNames =
        new TreeSet<>(String.CASE_INSENSITIVE_ORDER);

    private DescribableList<ViewJobFilter, Descriptor<ViewJobFilter>> jobFilters;
    private DescribableList<ListViewColumn, Descriptor<ListViewColumn>> columns;
    private String includeRegex;
    private volatile boolean recurse;
    private transient Pattern includePattern;
}
```

### 아이템 선택 메커니즘

ListView는 세 가지 방식으로 표시할 작업을 결정한다.

```
┌─────────────────────────────────────────────────┐
│              ListView 아이템 선택 흐름            │
├─────────────────────────────────────────────────┤
│                                                  │
│  [1단계] 이름 기반 선택                           │
│   │  jobNames (SortedSet<String>)               │
│   │  - 대소문자 무시 정렬                         │
│   │  - synchronized 접근                         │
│   │                                              │
│  [2단계] 정규식 패턴 매칭                         │
│   │  includeRegex → includePattern              │
│   │  - 이름과 패턴 결과를 합침 (OR 조건)          │
│   │                                              │
│  [3단계] ViewJobFilter 체인                      │
│   │  jobFilters 리스트를 순차 적용               │
│   │  - 각 필터가 목록을 수정 가능                 │
│   │  - candidates = 전체 아이템 목록              │
│   │                                              │
│  [최종] 중복 제거                                │
│      new LinkedHashSet<>(items)                  │
│                                                  │
└─────────────────────────────────────────────────┘
```

### getItems() 구현

```java
// core/src/main/java/hudson/model/ListView.java (218-272행)
private List<TopLevelItem> getItems(boolean recurse) {
    SortedSet<String> names;
    List<TopLevelItem> items = new ArrayList<>();

    synchronized (this) {
        names = new TreeSet<>(jobNames);
    }

    ItemGroup<? extends TopLevelItem> parent = getOwner().getItemGroup();

    if (recurse) {
        // ItemGroup 재귀 탐색: 이름 매칭 또는 패턴 매칭
        items.addAll(parent.getAllItems(TopLevelItem.class, item -> {
            String itemName = item.getRelativeNameFrom(parent);
            if (names.contains(itemName)) return true;
            if (includePattern != null)
                return includePattern.matcher(itemName).matches();
            return false;
        }));
    } else {
        // 비재귀: 직접 이름으로 조회
        for (String name : names) {
            TopLevelItem i = parent.getItem(name);
            if (i != null) items.add(i);
        }
        // 패턴 매칭 추가
        if (includePattern != null) {
            items.addAll(parent.getItems(item ->
                includePattern.matcher(
                    item.getRelativeNameFrom(parent)).matches()));
        }
    }

    // ViewJobFilter 체인 적용
    Collection<ViewJobFilter> jobFilters = getJobFilters();
    if (!jobFilters.isEmpty()) {
        List<TopLevelItem> candidates = recurse
            ? parent.getAllItems(TopLevelItem.class)
            : new ArrayList<>(parent.getItems());
        for (ViewJobFilter jobFilter : jobFilters) {
            items = jobFilter.filter(items, candidates, this);
        }
    }

    // 중복 제거
    items = new ArrayList<>(new LinkedHashSet<>(items));
    return items;
}
```

### 재귀(recurse) 옵션

```java
// core/src/main/java/hudson/model/ListView.java (325-335행)
@DataBoundSetter
public void setRecurse(boolean recurse) {
    this.recurse = recurse;
}
```

`recurse = true`이면 폴더(ItemGroup) 내부의 아이템도 검색한다. Jenkins의 폴더 플러그인과 함께 사용할 때 중요하다.

### 아이템 이름 변경/삭제 리스너

```java
// core/src/main/java/hudson/model/ListView.java (562-652행)
@Extension
public static final class Listener extends ItemListener {
    @Override
    public void onLocationChanged(Item item, String oldFullName,
                                   String newFullName) {
        // 모든 ListView의 jobNames에서 이름 갱신
    }

    @Override
    public void onDeleted(Item item) {
        // 모든 ListView의 jobNames에서 삭제
    }
}
```

아이템이 이동/이름변경/삭제되면 모든 ListView의 `jobNames`를 자동으로 업데이트한다. 이는 `ItemListener` 확장점을 활용한 것으로, 뷰와 아이템 사이의 정합성을 유지한다.

**동기화 전략**: `renameViewItem()`에서 `synchronized(lv)` 블록 안에서 jobNames를 변경하고, 블록 밖에서 `lv.save()`를 호출한다. 이는 save() 중 발생할 수 있는 I/O 블로킹 동안 ListView 락을 잡지 않기 위함이다.

### contains() 구현

```java
// core/src/main/java/hudson/model/ListView.java (284-286행)
@Override
public boolean contains(TopLevelItem item) {
    return getItems().contains(item);
}
```

`getItems()` 전체를 계산한 뒤 포함 여부를 확인한다. 필터 체인까지 적용된 결과에 대해 판단한다.

### DirectlyModifiableView

ListView는 `DirectlyModifiableView`를 구현하여 프로그래밍 방식으로 아이템을 추가/제거할 수 있다.

```java
@Override
public void add(TopLevelItem item) throws IOException {
    synchronized (this) {
        jobNames.add(item.getRelativeNameFrom(getOwner().getItemGroup()));
    }
    save();
}

@Override
public boolean remove(TopLevelItem item) throws IOException {
    synchronized (this) {
        String name = item.getRelativeNameFrom(getOwner().getItemGroup());
        if (!jobNames.remove(name)) return false;
    }
    save();
    return true;
}
```

### REST API 엔드포인트

| 엔드포인트 | HTTP | 설명 |
|-----------|------|------|
| `doAddJobToView` | POST | 이름으로 아이템 추가 |
| `doRemoveJobFromView` | POST | 이름으로 아이템 제거 |
| `doCreateItem` | POST | 새 아이템 생성 후 뷰에 추가 |

---

## 12. AllView: 전체 뷰

```
파일: core/src/main/java/hudson/model/AllView.java
```

```java
public class AllView extends View {
    public static final String DEFAULT_VIEW_NAME = "all";

    @Override
    public boolean contains(TopLevelItem item) {
        return true;  // 모든 아이템 포함
    }

    @Override
    public Collection<TopLevelItem> getItems() {
        return (Collection) getOwner().getItemGroup().getItems();
    }

    @Override
    public boolean isEditable() {
        return false;  // 설정 불가
    }
}
```

AllView는 모든 아이템을 무조건 포함하는 특수한 뷰다. `contains()`가 항상 `true`를 반환하고, `isEditable()`이 `false`를 반환하여 편집 불가능하다.

### 로케일 마이그레이션 (JENKINS-38606)

```java
// core/src/main/java/hudson/model/AllView.java (156-193행)
public static String migrateLegacyPrimaryAllViewLocalizedName(
        List<View> views, String primaryView) {
    // 로케일별 "All" 이름을 DEFAULT_VIEW_NAME("all")으로 통일
    for (Locale l : Locale.getAvailableLocales()) {
        if (primaryView.equals(
                Messages._Hudson_ViewName().toString(l))) {
            allView.name = DEFAULT_VIEW_NAME;
            return DEFAULT_VIEW_NAME;
        }
    }
    return primaryView;
}
```

이전 버전에서 "All" 뷰 이름이 로케일에 따라 달랐던 문제를 해결한다. 기본 뷰일 때만 안전하게 이름을 변경한다.

### Descriptor의 유일성 보장

```java
// core/src/main/java/hudson/model/AllView.java (196-205행)
@Extension @Symbol("all")
public static final class DescriptorImpl extends ViewDescriptor {
    @Override
    public boolean isApplicableIn(ViewGroup owner) {
        for (View v : owner.getViews()) {
            if (v instanceof AllView) return false;
        }
        return true;
    }
}
```

하나의 ViewGroup에 AllView는 최대 하나만 존재할 수 있다. 이미 AllView가 있으면 `isApplicableIn()`이 `false`를 반환하여 추가 생성을 방지한다.

---

## 13. ViewJobFilter와 ListViewColumn: 뷰 확장

### ViewJobFilter -- 작업 필터

```
파일: core/src/main/java/hudson/views/ViewJobFilter.java
```

```java
public abstract class ViewJobFilter
        implements ExtensionPoint, Describable<ViewJobFilter> {

    public abstract List<TopLevelItem> filter(
        List<TopLevelItem> added,
        List<TopLevelItem> all,
        View filteringView);
}
```

| 파라미터 | 설명 |
|---------|------|
| `added` | 현재까지 추가된 아이템 목록 (이전 필터 결과) |
| `all` | 가능한 모든 아이템 목록 (후보군) |
| `filteringView` | 필터링 대상 뷰 |

필터 체인은 **순차적**으로 적용된다. 각 필터는 `added` 목록에서 항목을 제거하거나, `all`에서 항목을 추가할 수 있다.

```
필터 체인 예시:

[모든 아이템: A, B, C, D, E]

→ StatusFilter(enabled=true)  → [A, B, C]     (비활성 D, E 제거)
→ RegExJobFilter(".*-prod")   → [A, B, C, E]  (패턴 매칭으로 E 추가)
→ MostRecentJobsFilter(3)     → [B, C, E]     (최근 3개만)
```

### ListViewColumn -- 테이블 열

```
파일: core/src/main/java/hudson/views/ListViewColumn.java
```

```java
public abstract class ListViewColumn
        implements ExtensionPoint, Describable<ListViewColumn> {

    @Exported
    public String getColumnCaption() {
        return getDescriptor().getDisplayName();
    }
}
```

### 기본 열 순서 상수

```java
// core/src/main/java/hudson/views/ListViewColumn.java (191-196행)
public static final double DEFAULT_COLUMNS_ORDINAL_ICON_START       = 60;
public static final double DEFAULT_COLUMNS_ORDINAL_ICON_END         = 50;
public static final double DEFAULT_COLUMNS_ORDINAL_PROPERTIES_START = 40;
public static final double DEFAULT_COLUMNS_ORDINAL_PROPERTIES_END   = 30;
public static final double DEFAULT_COLUMNS_ORDINAL_ACTIONS_START    = 20;
public static final double DEFAULT_COLUMNS_ORDINAL_ACTIONS_END      = 10;
```

열 배치 구조:

```
┌────────────────────────────────────────────────────────────────┐
│  아이콘 영역 (60~50)  │  속성 영역 (40~30)  │  액션 영역 (20~10) │
├───────────────────────┼────────────────────┼───────────────────┤
│  상태 아이콘           │  이름              │  빌드 버튼         │
│  날씨 아이콘           │  마지막 성공        │  설정 링크         │
│                       │  마지막 실패        │                    │
│                       │  마지막 실행 시간    │                    │
└───────────────────────┴────────────────────┴───────────────────┘
```

### 기본 열 목록 생성

```java
// core/src/main/java/hudson/views/ListViewColumn.java (154-177행)
private static List<ListViewColumn> createDefaultInitialColumnList(
        List<Descriptor<ListViewColumn>> descriptors) {
    ArrayList<ListViewColumn> r = new ArrayList<>();
    for (Descriptor<ListViewColumn> d : descriptors) {
        if (d instanceof ListViewColumnDescriptor ld) {
            if (!ld.shownByDefault()) continue;
        }
        ListViewColumn lvc = d.newInstance(null, emptyJSON);
        if (!lvc.shownByDefault()) continue;
        r.add(lvc);
    }
    return r;
}
```

`ListViewColumnDescriptor.shownByDefault()`가 `true`인 열만 기본 열 목록에 포함된다. 플러그인이 설치되면 해당 플러그인의 열이 자동으로 후보에 포함된다.

---

## 14. Fingerprint: 아티팩트 추적 시스템

```
파일: core/src/main/java/hudson/model/Fingerprint.java
```

```java
@ExportedBean
public class Fingerprint implements ModelObject, Saveable {
    private final @NonNull Date timestamp;
    private final @CheckForNull BuildPtr original;
    private final byte[] md5sum;
    private final String fileName;
    private Hashtable<String, RangeSet> usages = new Hashtable<>();
    PersistedList<FingerprintFacet> facets = new PersistedList<>(this);
}
```

### 핵심 개념

Fingerprint는 파일의 **MD5 해시**를 사용하여 빌드 간 아티팩트를 추적한다.

```
┌──────────────────────────────────────────────────────────┐
│                   Fingerprint 모델                        │
├──────────────────────────────────────────────────────────┤
│                                                           │
│  fileName: "myapp-1.0.jar"                               │
│  md5sum:   a1b2c3d4e5f6...                               │
│  timestamp: 2024-01-15 10:30:00                          │
│                                                           │
│  original:  ← 이 파일을 처음 만든 빌드                     │
│    BuildPtr { name="my-project", number=42 }             │
│                                                           │
│  usages:    ← 이 파일을 사용한 모든 빌드                    │
│    "my-project"      → RangeSet [42,45)                  │
│    "integration-test" → RangeSet [10,12),[15,16)         │
│    "deploy-prod"     → RangeSet [5,6)                    │
│                                                           │
│  facets:    ← 확장 가능한 메타데이터                        │
│    [FingerprintFacet, ...]                                │
│                                                           │
└──────────────────────────────────────────────────────────┘
```

### BuildPtr -- 빌드 포인터

```java
// core/src/main/java/hudson/model/Fingerprint.java (89-218행)
@ExportedBean(defaultVisibility = 2)
public static class BuildPtr {
    String name;       // Job의 fullName
    final int number;  // 빌드 번호

    public Job<?, ?> getJob() {
        return Jenkins.get().getItemByFullName(name, Job.class);
    }

    public Run getRun() {
        Job j = getJob();
        if (j == null) return null;
        return j.getBuildByNumber(number);
    }

    public boolean belongsTo(Job job) {
        // 상위 Job까지 확인 (MavenModule → MavenModuleSet)
        Item p = Jenkins.get().getItemByFullName(name);
        while (p != null) {
            if (p == job) return true;
            ItemGroup<?> parent = p.getParent();
            if (!(parent instanceof Item)) return false;
            p = (Item) parent;
        }
        return false;
    }
}
```

### Range와 RangeSet -- 빌드 번호 범위

Fingerprint는 "어떤 Job의 몇 번 빌드에서 이 파일을 사용했는가"를 `RangeSet`으로 효율적으로 저장한다.

```java
// core/src/main/java/hudson/model/Fingerprint.java (224-335행)
@ExportedBean(defaultVisibility = 4)
public static final class Range {
    final int start;  // 시작 (포함)
    final int end;    // 끝 (미포함)

    public boolean includes(int i) {
        return start <= i && i < end;
    }

    public Range combine(Range that) {
        return new Range(
            Math.min(this.start, that.start),
            Math.max(this.end, that.end));
    }
}
```

```java
// core/src/main/java/hudson/model/Fingerprint.java (341-800행)
@ExportedBean(defaultVisibility = 3)
public static final class RangeSet {
    private final List<Range> ranges;  // 정렬된 Range 목록

    public void add(int n) {
        // 인접 범위와 자동 병합
        for (int i = 0; i < ranges.size(); i++) {
            Range r = ranges.get(i);
            if (r.includes(n)) return;
            if (r.end == n) {
                ranges.set(i, r.expandRight());
                checkCollapse(i);  // 다음 범위와 병합 가능 여부
                return;
            }
            // ...
        }
    }
}
```

**RangeSet의 효율성**: 연속된 빌드 번호는 하나의 Range로 합쳐진다.

```
빌드 1, 2, 3, 5, 7, 8, 9 → RangeSet: [1,4),[5,6),[7,10)
직렬화: "1-3,5,7-9"
```

### RangeSet 직렬화

```java
// core/src/main/java/hudson/model/Fingerprint.java (751-800행)
public static final class ConverterImpl implements Converter {
    public static String serialize(RangeSet src) {
        StringBuilder buf = new StringBuilder();
        for (Range r : src.ranges) {
            if (!buf.isEmpty()) buf.append(',');
            if (r.isSingle())
                buf.append(r.start);
            else
                buf.append(r.start).append('-').append(r.end - 1);
        }
        return buf.toString();
    }

    @Override
    public Object unmarshal(HierarchicalStreamReader reader, ...) {
        if (reader.hasMoreChildren()) {
            // 구형 XML 형식 (중첩 <range> 요소)
            return new RangeSet((List<Range>) collectionConv.unmarshal(...));
        } else {
            // 신형 문자열 형식 ("1-3,5,7-9")
            return RangeSet.fromString(reader.getValue(), true);
        }
    }
}
```

### usages -- 사용 추적

```java
// core/src/main/java/hudson/model/Fingerprint.java (854행)
private Hashtable<String, RangeSet> usages = new Hashtable<>();
```

| 키 | 값 |
|----|-----|
| Job fullName (예: "folder/my-job") | 해당 Job에서 이 파일을 사용한 빌드 번호 범위 |

```java
// core/src/main/java/hudson/model/Fingerprint.java (1006-1008행)
public void addFor(@NonNull Run b) throws IOException {
    add(b.getParent().getFullName(), b.getNumber());
}

public synchronized void add(String jobFullName, int n) throws IOException {
    addWithoutSaving(jobFullName, n);
    save();
}
```

### Fingerprint 생존 판정

```java
// core/src/main/java/hudson/model/Fingerprint.java (1046-1064행)
public synchronized boolean isAlive() {
    if (original != null && original.isAlive()) return true;

    for (Map.Entry<String, RangeSet> e : usages.entrySet()) {
        Job j = Jenkins.get().getItemByFullName(e.getKey(), Job.class);
        if (j == null) continue;
        Run firstBuild = j.getFirstBuild();
        if (firstBuild == null) continue;
        int oldest = firstBuild.getNumber();
        if (!e.getValue().isSmallerThan(oldest)) return true;
    }
    return false;
}
```

original 빌드가 아직 존재하거나, usages에 기록된 빌드 중 하나라도 아직 존재하면 fingerprint는 "살아있다". 모든 참조 빌드가 삭제되면 fingerprint도 정리 대상이 된다.

### trim() -- 정리

```java
// core/src/main/java/hudson/model/Fingerprint.java (1074-1099행)
public synchronized boolean trim() throws IOException {
    boolean modified = false;
    for (Map.Entry<String, RangeSet> e :
            new Hashtable<>(usages).entrySet()) {
        Job j = Jenkins.get().getItemByFullName(e.getKey(), Job.class);
        if (j == null) {
            usages.remove(e.getKey());  // 삭제된 Job
            modified = true;
            continue;
        }
        // 삭제된 빌드에 대한 RangeSet 정리
        // ...
    }
    return modified;
}
```

### 프로젝트 이름 변경 대응

```java
// core/src/main/java/hudson/model/Fingerprint.java (806-832행)
@Extension
public static final class ProjectRenameListener extends ItemListener {
    @Override
    public void onLocationChanged(Item item, String oldName, String newName) {
        if (item instanceof Job) {
            Job p = Jenkins.get().getItemByFullName(newName, Job.class);
            for (Run build : p.getBuilds()) {
                for (Fingerprint f : build.getBuildFingerprints()) {
                    f.rename(oldName, newName);
                }
            }
        }
    }
}
```

프로젝트 이름이 변경되면 관련된 모든 Fingerprint의 `usages` 키를 업데이트한다.

### FingerprintFacet -- 확장 메타데이터

```java
PersistedList<FingerprintFacet> facets = new PersistedList<>(this);
private transient volatile List<FingerprintFacet> transientFacets = null;
```

`FingerprintFacet`은 Fingerprint에 추가 정보를 붙이는 확장점이다. 예를 들어 Docker 이미지 태그, 배포 환경 정보 등을 연결할 수 있다. `TransientFingerprintFacetFactory`로 런타임에 동적으로 생성되는 facet도 지원한다.

---

## 15. 왜 이렇게 설계했는가

### 왜 SCM이 ExtensionPoint인가?

Jenkins는 **어떤 VCS와도 통합할 수 있어야** 한다. SCM 추상화 없이 Git만 하드코딩했다면, Subversion이나 Mercurial을 지원하려면 Jenkins 코어를 수정해야 한다.

ExtensionPoint로 설계함으로써:

1. **Git Plugin**: `GitSCM extends SCM` -- 별도 플러그인으로 Git 지원
2. **SVN Plugin**: `SubversionSCM extends SCM` -- 별도 플러그인으로 SVN 지원
3. **코어 변경 없음**: 새 VCS를 지원하려면 플러그인만 추가하면 된다

### 왜 폴링(polling)과 체크아웃(checkout)이 분리되어 있는가?

폴링은 **변경 감지**이고 체크아웃은 **코드 다운로드**다. 이를 분리하면:

1. **폴링 빈도 vs 빌드 빈도**: 5분마다 폴링하지만 변경이 없으면 빌드하지 않는다
2. **워크스페이스 독립 폴링**: `requiresWorkspaceForPolling() == false`이면 워크스페이스 없이도 폴링할 수 있다. Git은 원격 참조만 비교하면 되므로 워크스페이스가 필요 없다
3. **quiet period**: 변경 감지 후 일정 시간 대기하여 연속 커밋을 하나의 빌드로 묶는다

### 왜 PollingResult에 Change enum이 4가지인가?

| 시나리오 | 적절한 Change 값 |
|---------|---------------|
| 변경 없음 | `NONE` |
| .gitignore만 변경됨 (제외 패턴에 해당) | `INSIGNIFICANT` |
| 소스 파일 변경 | `SIGNIFICANT` |
| 리포지토리 URL이 바뀜 (설정 변경) | `INCOMPARABLE` |

`INSIGNIFICANT`와 `INCOMPARABLE`이 없다면, 커밋 제외 기능이나 설정 변경 시 즉시 빌드 기능을 구현할 수 없다.

### 왜 View가 ExtensionPoint인가?

Jenkins 대시보드의 **표시 방식**은 조직마다 다르다.

| 플러그인 뷰 | 설명 |
|------------|------|
| Dashboard View | 위젯 기반 대시보드 |
| Build Pipeline View | 파이프라인 시각화 |
| Nested View | 뷰 안의 뷰 (계층 구조) |
| Sectioned View | 섹션으로 나눈 뷰 |
| Categorized Jobs View | 카테고리별 분류 |

View를 ExtensionPoint로 만들지 않았다면, 이 모든 뷰를 코어에 포함시켜야 했을 것이다.

### 왜 Fingerprint에 Hashtable을 사용하는가?

```java
private Hashtable<String, RangeSet> usages = new Hashtable<>();
```

`HashMap`이 아닌 `Hashtable`을 사용하는 것은 **레거시 코드**다. `Hashtable`은 모든 메서드가 `synchronized`이므로 thread-safe하지만, 현대 Java에서는 `ConcurrentHashMap`이 권장된다. Fingerprint 클래스 자체가 대부분의 접근을 `synchronized` 메서드로 보호하고 있으므로 이중 동기화가 되는 셈이지만, 하위 호환성을 위해 유지하고 있다.

### 왜 RangeSet으로 빌드 번호를 저장하는가?

빌드 번호를 개별적으로 저장하면 수천 번의 빌드가 쌓일 때 메모리와 디스크를 크게 소비한다. RangeSet은 연속 범위를 압축한다.

```
개별 저장:  [1, 2, 3, 4, 5, 100, 101, 102]  → 8개 정수
RangeSet:   [1,6),[100,103)                  → 4개 정수 (50% 절약)

실제 시나리오 (CI 서버에서 1000번 연속 빌드):
개별 저장:  1000개 정수
RangeSet:   [1,1001) → 2개 정수 (99.8% 절약)
```

### 왜 ListView의 jobNames가 SortedSet인가?

```java
SortedSet<String> jobNames = new TreeSet<>(String.CASE_INSENSITIVE_ORDER);
```

1. **대소문자 무시**: Windows에서 "MyJob"과 "myjob"은 같은 작업이다
2. **정렬**: UI에서 일관된 순서로 표시
3. **TreeSet**: 이름 기반 조회가 `O(log n)`으로 효율적

---

## 16. 전체 클래스 관계도

```
┌─────────────────────────────────────────────────────────────────────────┐
│                          SCM 서브시스템                                  │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                         │
│  ┌──────────────┐         ┌──────────────────┐                          │
│  │    SCM       │◄────────│  SCMDescriptor   │                          │
│  │  (abstract)  │describe │  (abstract)      │                          │
│  ├──────────────┤         ├──────────────────┤                          │
│  │ checkout()   │         │ repositoryBrowser│                          │
│  │ poll()       │         │ isApplicable()   │                          │
│  │ getKey()     │         │ getBrowserDesc() │                          │
│  └──────┬───────┘         └──────────────────┘                          │
│         │ extends                                                       │
│    ┌────┴─────┐                                                         │
│    │ NullSCM  │     ┌──────────────────┐    ┌──────────────────┐        │
│    └──────────┘     │ SCMRevisionState │    │  PollingResult   │        │
│                     │ (abstract)       │    │  (immutable)     │        │
│                     ├──────────────────┤    ├──────────────────┤        │
│                     │ NONE (sentinel)  │    │ baseline, remote │        │
│                     └──────────────────┘    │ Change enum      │        │
│                                             │ NO_CHANGES       │        │
│                                             │ SIGNIFICANT      │        │
│                                             │ BUILD_NOW        │        │
│                                             └──────────────────┘        │
│                                                                         │
│  ┌──────────────────────────┐    ┌──────────────────┐                   │
│  │ ChangeLogSet<T>          │    │  EditType         │                   │
│  │ implements Iterable<T>   │    ├──────────────────┤                   │
│  ├──────────────────────────┤    │ ADD, EDIT, DELETE │                   │
│  │ run, browser             │    └──────────────────┘                   │
│  │ isEmptySet(), getItems() │                                           │
│  │                          │    ┌──────────────────┐                   │
│  │ Entry (abstract inner)   │    │  AffectedFile    │                   │
│  │   getMsg(), getAuthor()  │────│  (interface)     │                   │
│  │   getAffectedPaths()     │    │  getPath()       │                   │
│  │   getCommitId()          │    │  getEditType()   │                   │
│  └──────────────────────────┘    └──────────────────┘                   │
│                                                                         │
│  ┌──────────────────┐                                                   │
│  │   SCMTrigger     │  extends Trigger<Item>                            │
│  ├──────────────────┤                                                   │
│  │ scmpoll_spec     │──── cron 스케줄                                    │
│  │ ignorePostCommit │                                                   │
│  │ Runner           │──── 실제 폴링 수행 Runnable                        │
│  │ SCMAction        │──── 프로젝트 레벨 폴링 로그                        │
│  │ BuildAction      │──── 빌드 레벨 폴링 로그                            │
│  │ DescriptorImpl   │──── SequentialExecutionQueue 관리                  │
│  └──────────────────┘                                                   │
│                                                                         │
├─────────────────────────────────────────────────────────────────────────┤
│                          View 서브시스템                                  │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                         │
│  ┌──────────────────────────────────────┐                               │
│  │           View (abstract)            │                               │
│  │  implements AccessControlled,        │                               │
│  │    Describable, ExtensionPoint,      │                               │
│  │    Saveable, HasWidgets, Badgeable   │                               │
│  ├──────────────────────────────────────┤                               │
│  │ owner: ViewGroup                     │                               │
│  │ name, description                    │                               │
│  │ filterExecutors, filterQueue         │                               │
│  │ properties: DescribableList          │                               │
│  │                                      │                               │
│  │ getItems(): abstract                 │                               │
│  │ contains(): abstract                 │                               │
│  │ getViewName(), isDefault()           │                               │
│  │ getBuilds(), save()                  │                               │
│  │ doConfigSubmit(), doDoDelete()       │                               │
│  │ PERMISSIONS: CREATE/DELETE/          │                               │
│  │              CONFIGURE/READ          │                               │
│  └────┬──────────────┬──────────────────┘                               │
│       │              │ extends                                          │
│  ┌────┴────┐    ┌────┴──────────────────────┐                           │
│  │ AllView │    │       ListView             │                           │
│  ├─────────┤    │ implements                 │                           │
│  │ 모든 항목│    │   DirectlyModifiableView   │                           │
│  │ 편집불가 │    ├───────────────────────────┤                           │
│  └─────────┘    │ jobNames: SortedSet       │                           │
│                 │ jobFilters: List<Filter>   │                           │
│                 │ columns: List<Column>      │                           │
│                 │ includeRegex/Pattern       │                           │
│                 │ recurse                    │                           │
│                 │                            │                           │
│                 │ add(), remove()            │                           │
│                 │ Listener (ItemListener)    │                           │
│                 └───────────────────────────┘                           │
│                                                                         │
│  ┌────────────────┐    ┌──────────────────┐                             │
│  │ ViewJobFilter  │    │ ListViewColumn   │                             │
│  │ (abstract)     │    │ (abstract)       │                             │
│  ├────────────────┤    ├──────────────────┤                             │
│  │ filter()       │    │ getColumnCaption │                             │
│  └────────────────┘    │ shownByDefault() │                             │
│                        └──────────────────┘                             │
│                                                                         │
├─────────────────────────────────────────────────────────────────────────┤
│                       Fingerprint 서브시스템                              │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                         │
│  ┌──────────────────────────────────────────┐                           │
│  │          Fingerprint                      │                           │
│  │  implements ModelObject, Saveable         │                           │
│  ├──────────────────────────────────────────┤                           │
│  │ timestamp: Date                           │                           │
│  │ original: BuildPtr (nullable)             │                           │
│  │ md5sum: byte[]                            │                           │
│  │ fileName: String                          │                           │
│  │ usages: Hashtable<String, RangeSet>       │                           │
│  │ facets: PersistedList<FingerprintFacet>   │                           │
│  │                                           │                           │
│  │ addFor(Run), trim(), isAlive()            │                           │
│  │ getRangeSet(jobFullName)                  │                           │
│  │ getHashString(), getJobs()                │                           │
│  ├──────────────────────────────────────────┤                           │
│  │ BuildPtr { name, number }                 │                           │
│  │   getJob(), getRun(), belongsTo()         │                           │
│  │ Range { start, end }                      │                           │
│  │   includes(), combine(), intersect()      │                           │
│  │ RangeSet { List<Range> }                  │                           │
│  │   add(n), retainAll(), removeAll()        │                           │
│  │   ConverterImpl (직렬화: "1-3,5,7-9")     │                           │
│  └──────────────────────────────────────────┘                           │
│                                                                         │
└─────────────────────────────────────────────────────────────────────────┘
```

---

## 참조 파일 경로

| 파일 | 설명 |
|------|------|
| `core/src/main/java/hudson/scm/SCM.java` | SCM 추상 클래스 (810행) |
| `core/src/main/java/hudson/scm/NullSCM.java` | Null Object 패턴 SCM (79행) |
| `core/src/main/java/hudson/scm/SCMDescriptor.java` | SCM Descriptor (172행) |
| `core/src/main/java/hudson/scm/SCMRevisionState.java` | 리비전 상태 추상화 (55행) |
| `core/src/main/java/hudson/scm/PollingResult.java` | 폴링 결과 불변 객체 (109행) |
| `core/src/main/java/hudson/scm/ChangeLogSet.java` | 변경 로그 모델 (298행) |
| `core/src/main/java/hudson/scm/EditType.java` | 파일 변경 유형 (62행) |
| `core/src/main/java/hudson/triggers/SCMTrigger.java` | SCM 폴링 트리거 (700+행) |
| `core/src/main/java/hudson/model/View.java` | View 추상 클래스 (1260+행) |
| `core/src/main/java/hudson/model/ListView.java` | 리스트 뷰 구현 (653행) |
| `core/src/main/java/hudson/model/AllView.java` | 전체 뷰 구현 (213행) |
| `core/src/main/java/hudson/model/Fingerprint.java` | 아티팩트 추적 (1100+행) |
| `core/src/main/java/hudson/views/ViewJobFilter.java` | 뷰 작업 필터 (64행) |
| `core/src/main/java/hudson/views/ListViewColumn.java` | 리스트 뷰 열 (197행) |
