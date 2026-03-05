# 11. Descriptor 패턴: Jenkins 확장성의 핵심 설계

## 목차

1. [개요](#1-개요)
2. [Describable 인터페이스](#2-describable-인터페이스)
3. [Descriptor 클래스](#3-descriptor-클래스)
4. [Descriptor-Describable 관계 모델](#4-descriptor-describable-관계-모델)
5. [인스턴스 생성과 폼 바인딩](#5-인스턴스-생성과-폼-바인딩)
6. [전역 설정과 영속성](#6-전역-설정과-영속성)
7. [폼 검증 시스템](#7-폼-검증-시스템)
8. [Jelly/Groovy 템플릿 시스템](#8-jellygroovy-템플릿-시스템)
9. [도움말 파일 시스템](#9-도움말-파일-시스템)
10. [DescriptorExtensionList](#10-descriptorextensionlist)
11. [명명 규칙과 컨벤션](#11-명명-규칙과-컨벤션)
12. [실전 활용 예시: Shell 빌드 스텝](#12-실전-활용-예시-shell-빌드-스텝)
13. [Descriptor 패턴의 확장 계층](#13-descriptor-패턴의-확장-계층)
14. [왜 이런 패턴인가](#14-왜-이런-패턴인가)

---

## 1. 개요

Jenkins의 Descriptor 패턴은 프레임워크 전체의 확장성을 지탱하는 가장 근본적인 설계 패턴이다. 빌드 스텝, SCM, 트리거, 보안 설정 등 Jenkins에서 "구성 가능한(configurable)" 모든 것이 이 패턴을 기반으로 동작한다.

핵심 아이디어는 단순하다: **인스턴스의 설정 데이터(Describable)와 그 타입에 대한 메타데이터(Descriptor)를 분리**한다.

```
소스 파일:
  core/src/main/java/hudson/model/Describable.java      (51줄)
  core/src/main/java/hudson/model/Descriptor.java        (1334줄)
  core/src/main/java/hudson/DescriptorExtensionList.java (251줄)
```

이 패턴은 Java의 `Object`/`Class` 관계와 유사하다. 모든 Java 객체(`Object`)가 자신의 클래스 정보(`Class`)를 가지듯, 모든 `Describable` 인스턴스는 자신의 타입 정보를 담은 `Descriptor` 싱글턴을 가진다.

```
+------------------+        +------------------+
|   Describable    |        |    Descriptor     |
|   (인스턴스)       | -----> |    (싱글턴)        |
|                  |        |                  |
| - 구성 데이터      |        | - 메타데이터       |
| - 직렬화 대상      |        | - 팩토리 메서드     |
| - 런타임 동작      |        | - 폼 렌더링       |
|                  |        | - 폼 검증         |
+------------------+        | - 전역 설정       |
    (N개 존재)               +------------------+
                                (1개 존재)
```

---

## 2. Describable 인터페이스

### 2.1 전체 소스코드

`Describable`은 Jenkins에서 가장 간결하면서도 가장 중요한 인터페이스이다. 전체 코드가 51줄에 불과하다.

```java
// core/src/main/java/hudson/model/Describable.java

public interface Describable<T extends Describable<T>> {
    /**
     * Gets the descriptor for this instance.
     *
     * Descriptor is a singleton for every concrete Describable
     * implementation, so if a.getClass() == b.getClass() then by default
     * a.getDescriptor() == b.getDescriptor() as well.
     * (In rare cases a single implementation class may be used
     * for instances with distinct descriptors.)
     *
     * By default looks for a nested class (conventionally named
     * DescriptorImpl) implementing Descriptor and marked with @Extension.
     */
    default Descriptor<T> getDescriptor() {
        return Jenkins.get().getDescriptorOrDie(getClass());
    }
}
```

### 2.2 자기 참조 제네릭(Self-referential Generic)

`T extends Describable<T>`라는 자기 참조 제네릭(CRTP, Curiously Recurring Template Pattern) 선언이 핵심이다. 이것은 다음을 보장한다:

- `Describable` 구현체가 자신의 타입을 `Descriptor`에 전달할 수 있다
- 타입 안전성: `Builder`의 `Descriptor`는 `Descriptor<Builder>`로 한정된다
- 컴파일 타임에 잘못된 Descriptor-Describable 쌍을 잡아낸다

```
Builder extends Describable<Builder>
  └─> getDescriptor()가 Descriptor<Builder>를 반환

SCM extends Describable<SCM>
  └─> getDescriptor()가 Descriptor<SCM>를 반환
```

### 2.3 getDescriptor()의 default 구현

Java 8부터 지원되는 `default` 메서드를 활용해, 모든 Describable 구현체는 `getDescriptor()`를 직접 구현하지 않아도 된다. 내부적으로 `Jenkins.get().getDescriptorOrDie(getClass())`를 호출한다.

```java
// core/src/main/java/jenkins/model/Jenkins.java (1557행)
@NonNull
public Descriptor getDescriptorOrDie(Class<? extends Describable> type) {
    Descriptor d = getDescriptor(type);
    if (d == null)
        throw new AssertionError(type + " is missing its descriptor");
    return d;
}
```

Jenkins 인스턴스는 모든 `@Extension` 어노테이션이 붙은 `Descriptor` 싱글턴을 관리하고 있으므로, 클래스 이름으로 조회하면 해당 Descriptor를 찾을 수 있다.

### 2.4 AbstractDescribableImpl (Deprecated)

과거에는 `AbstractDescribableImpl<T>`라는 편의 추상 클래스가 있었으나, `Describable` 인터페이스에 `default` 메서드가 추가되면서 더 이상 필요 없어져 2.505부터 deprecated 되었다.

```java
// core/src/main/java/hudson/model/AbstractDescribableImpl.java
@Deprecated(since = "2.505")
public abstract class AbstractDescribableImpl<T extends AbstractDescribableImpl<T>>
    implements Describable<T> {}
```

---

## 3. Descriptor 클래스

### 3.1 클래스 선언과 인터페이스

```java
// core/src/main/java/hudson/model/Descriptor.java (152행)
public abstract class Descriptor<T extends Describable<T>>
    implements Loadable, Saveable, OnMaster {
```

`Descriptor`는 세 가지 인터페이스를 구현한다:

| 인터페이스 | 역할 |
|-----------|------|
| `Loadable` | `load()` 메서드 제공 -- 디스크에서 설정 로드 |
| `Saveable` | `save()` 메서드 제공 -- 디스크에 설정 저장 |
| `OnMaster` | 이 객체가 컨트롤러(마스터) 노드에서만 존재함을 표시 |

### 3.2 핵심 필드

```java
// Descriptor.java (156행)
public final transient Class<? extends T> clazz;

// 폼 검증 메서드 캐시 (158행)
private final transient Map<String, CheckMethod> checkMethods
    = new ConcurrentHashMap<>(2);

// Describable 클래스의 프로퍼티 타입 정보 (163행)
private transient volatile Map<String, PropertyType> propertyTypes,
                                                     globalPropertyTypes;

// 도움말 파일 리다이렉트 맵 (254행)
private final transient Map<String, HelpRedirect> helpRedirect
    = new HashMap<>(2);
```

`clazz` 필드는 이 Descriptor가 설명하는 Describable 클래스를 가리킨다. `transient`이므로 XML 직렬화에 포함되지 않는다.

### 3.3 생성자

Descriptor에는 두 가지 생성자가 있다:

**명시적 클래스 지정 생성자:**
```java
// Descriptor.java (277행)
protected Descriptor(Class<? extends T> clazz) {
    if (clazz == self())
        clazz = (Class) getClass();
    this.clazz = clazz;
}
```

**관례 기반 자동 추론 생성자 (중첩 클래스용):**
```java
// Descriptor.java (293행)
protected Descriptor() {
    this.clazz = (Class<T>) getClass().getEnclosingClass();
    if (clazz == null)
        throw new AssertionError(getClass()
            + " doesn't have an outer class. "
            + "Use the constructor that takes the Class object explicitly.");

    // 타입 파라미터 검증
    Type bt = Types.getBaseClass(getClass(), Descriptor.class);
    if (bt instanceof ParameterizedType pt) {
        Class t = Types.erasure(pt.getActualTypeArguments()[0]);
        if (!t.isAssignableFrom(clazz))
            throw new AssertionError("Outer class " + clazz
                + " of " + getClass()
                + " is not assignable to " + t
                + ". Perhaps wrong outer class?");
    }

    // getDescriptor() 반환 타입 검증
    try {
        Method getd = clazz.getMethod("getDescriptor");
        if (!getd.getReturnType().isAssignableFrom(getClass())) {
            throw new AssertionError(getClass()
                + " must be assignable to " + getd.getReturnType());
        }
    } catch (NoSuchMethodException e) {
        throw new AssertionError(getClass()
            + " is missing getDescriptor method.", e);
    }
}
```

이 무인자 생성자는 **세 가지 안전 검사**를 수행한다:
1. 외부(enclosing) 클래스가 존재하는지 확인
2. 외부 클래스가 Descriptor의 타입 파라미터 T에 할당 가능한지 확인
3. `getDescriptor()` 반환 타입과 이 Descriptor 클래스가 호환되는지 확인

### 3.4 핵심 메서드 요약

| 메서드 | 위치 | 역할 |
|--------|------|------|
| `getDisplayName()` | 328행 | UI에 표시할 이름 반환 (기본값: `clazz.getSimpleName()`) |
| `getId()` | 348행 | 고유 식별자 (기본값: `clazz.getName()`) |
| `getDescriptorUrl()` | 372행 | `"descriptorByName/" + getId()` URL |
| `newInstance(StaplerRequest2, JSONObject)` | 590행 | 폼 데이터로 인스턴스 생성 |
| `configure(StaplerRequest2, JSONObject)` | 869행 | 전역 설정 저장 |
| `getConfigPage()` | 889행 | config.jelly 템플릿 경로 |
| `getGlobalConfigPage()` | 893행 | global.jelly 템플릿 경로 |
| `save()` | 963행 | 설정을 XML 파일로 저장 |
| `load()` | 982행 | XML 파일에서 설정 로드 |
| `getHelpFile(String)` | 794행 | 필드별 도움말 경로 |
| `getCheckMethod(String)` | 413행 | 폼 검증 메서드 모델 |
| `calcFillSettings(String, Map)` | 428행 | 드롭다운 채우기 설정 |
| `getPropertyType(String)` | 513행 | 프로퍼티 타입 정보 |

---

## 4. Descriptor-Describable 관계 모델

### 4.1 1:1 싱글턴 관계

모든 구체적인 `Describable` 클래스에 대해 정확히 하나의 `Descriptor` 싱글턴이 존재한다. 이것은 `Object`와 `Class`의 관계와 동일하다.

```
                    Descriptor (싱글턴)
                    ┌──────────────────┐
                    │ Shell.Descriptor │
                    │   DescriptorImpl │
                    └────────┬─────────┘
                             │
            ┌────────────────┼────────────────┐
            │                │                │
     ┌──────┴──────┐  ┌─────┴──────┐  ┌──────┴──────┐
     │  Shell #1   │  │  Shell #2  │  │  Shell #3   │
     │ cmd="make"  │  │ cmd="test" │  │ cmd="deploy"│
     └─────────────┘  └────────────┘  └─────────────┘
     Describable 인스턴스 (N개)
```

### 4.2 타입 계층 관계

```
Describable<T>                    Descriptor<T>
    │                                 │
    ├── Builder                       ├── BuildStepDescriptor<Builder>
    │     └── Shell                   │     └── Shell.DescriptorImpl
    │                                 │
    ├── Publisher                     ├── BuildStepDescriptor<Publisher>
    │     ├── Recorder                │     ├── (Recorder 하위)
    │     └── Notifier                │     └── (Notifier 하위)
    │                                 │
    ├── SCM                          ├── SCMDescriptor<SCM>
    │                                 │
    ├── Trigger<?>                   ├── TriggerDescriptor
    │                                 │
    ├── SecurityRealm                ├── Descriptor<SecurityRealm>
    │                                 │
    └── GlobalConfiguration          └── (자기 자신이 Descriptor)
```

### 4.3 Self-Describing 패턴: GlobalConfiguration

`GlobalConfiguration`은 특별한 경우로, **자기 자신이 Describable이면서 동시에 Descriptor**이다.

```java
// core/src/main/java/jenkins/model/GlobalConfiguration.java (47행)
public abstract class GlobalConfiguration
    extends Descriptor<GlobalConfiguration>
    implements ExtensionPoint, Describable<GlobalConfiguration> {

    protected GlobalConfiguration() {
        super(self());  // 자기 자신을 describe
    }

    @Override
    public final Descriptor<GlobalConfiguration> getDescriptor() {
        return this;  // 자기 자신이 Descriptor
    }
}
```

`self()` 메서드는 `Descriptor.Self` 라는 특수 마커 클래스를 반환하며, 생성자에서 이를 감지해 `getClass()` 자체를 `clazz`로 설정한다.

```java
// Descriptor.java (1331행)
public static final class Self {}
protected static Class self() { return Self.class; }
```

---

## 5. 인스턴스 생성과 폼 바인딩

### 5.1 newInstance() 메서드

`newInstance()`는 웹 폼 제출 데이터로부터 `Describable` 인스턴스를 생성하는 팩토리 메서드이다.

```java
// Descriptor.java (590행)
public T newInstance(@Nullable StaplerRequest2 req,
                     @NonNull JSONObject formData) throws FormException {
    if (Util.isOverridden(Descriptor.class, getClass(),
            "newInstance", StaplerRequest.class, JSONObject.class)) {
        return newInstance(
            req != null ? StaplerRequest.fromStaplerRequest2(req) : null,
            formData);
    } else {
        return newInstanceImpl(req, formData);
    }
}
```

내부 구현(`newInstanceImpl`)은 다음 순서로 인스턴스를 생성한다:

```java
// Descriptor.java (607행)
private T newInstanceImpl(@Nullable StaplerRequest2 req,
                          @NonNull JSONObject formData) throws FormException {
    try {
        Method m = getClass().getMethod("newInstance", StaplerRequest.class);
        if (!Modifier.isAbstract(m.getDeclaringClass().getModifiers())) {
            // 하위 클래스가 newInstance(StaplerRequest)를 오버라이드한 경우
            return verifyNewInstance(
                newInstance(StaplerRequest.fromStaplerRequest2(req)));
        } else {
            if (req == null) {
                // null 요청 처리 (호환성)
                return verifyNewInstance(
                    clazz.getDeclaredConstructor().newInstance());
            }
            // 기본: bindJSON으로 데이터 바인딩
            return verifyNewInstance(bindJSON(req, clazz, formData, true));
        }
    } catch (...) {
        throw new LinkageError(
            "Failed to instantiate " + clazz + " from " +
            RedactSecretJsonInErrorMessageSanitizer.INSTANCE
                .sanitize(formData), e);
    }
}
```

### 5.2 인스턴스 생성 흐름

```
사용자가 폼 제출
    │
    ▼
StaplerRequest2 + JSONObject
    │
    ▼
Descriptor.newInstance(req, formData)
    │
    ├── (1) 하위 클래스가 오버라이드? → 그 메서드 호출
    │
    ├── (2) req == null? → clazz.newInstance() (기본 생성자)
    │
    └── (3) 기본: bindJSON(req, clazz, formData, true)
              │
              ├── NewInstanceBindInterceptor 설정
              │   (중첩 Describable도 newInstance() 경유하도록)
              │
              └── req.bindJSON(type, src)
                  │
                  └── @DataBoundConstructor가 붙은 생성자 호출
                      + @DataBoundSetter로 추가 필드 설정
```

### 5.3 NewInstanceBindInterceptor

중첩된 Describable 객체도 올바르게 `newInstance()`를 통해 생성되도록 하는 핵심 인터셉터이다.

```java
// Descriptor.java (676행)
private static class NewInstanceBindInterceptor extends BindInterceptor {
    private final BindInterceptor oldInterceptor;
    private final IdentityHashMap<JSONObject, Boolean> processed
        = new IdentityHashMap<>();

    private boolean isApplicable(Class type, JSONObject json) {
        if (Modifier.isAbstract(type.getModifiers()))
            return false;  // 추상 클래스 무시
        if (!Describable.class.isAssignableFrom(type))
            return false;  // Describable이 아니면 무시
        if (Boolean.TRUE.equals(processed.put(json, true)))
            return false;  // 이미 처리된 JSON이면 무시
        return true;
    }

    @Override
    public Object instantiate(Class actualType, JSONObject json) {
        if (isApplicable(actualType, json)) {
            // Descriptor.newInstance()를 통해 생성
            final Descriptor descriptor =
                Jenkins.get().getDescriptor(actualType);
            if (descriptor != null) {
                return descriptor.newInstance(
                    Stapler.getCurrentRequest2(), json);
            }
        }
        return oldInterceptor.instantiate(actualType, json);
    }
}
```

이 인터셉터가 없으면, 중첩된 Describable 객체는 Stapler의 기본 리플렉션 바인딩으로 생성되어 `newInstance()`를 거치지 않게 된다.

### 5.4 verifyNewInstance()

생성된 인스턴스의 무결성을 검증한다:

```java
// Descriptor.java (749행)
private T verifyNewInstance(T t) {
    if (t != null && t.getDescriptor() != this) {
        LOGGER.warning("Father of " + t
            + " and its getDescriptor() points to two different instances. "
            + "Probably misplaced @Extension.");
    }
    return t;
}
```

### 5.5 Hetero List에서의 인스턴스 생성

`<f:hetero-list>` 태그로 제출된 데이터에서 다양한 타입의 Describable 인스턴스 목록을 생성하는 메서드:

```java
// Descriptor.java (1174행)
public static <T extends Describable<T>>
List<T> newInstancesFromHeteroList(StaplerRequest2 req, Object formData,
            Collection<? extends Descriptor<T>> descriptors)
            throws FormException {

    List<T> items = new ArrayList<>();
    if (formData != null) {
        for (Object o : JSONArray.fromObject(formData)) {
            JSONObject jo = (JSONObject) o;
            Descriptor<T> d = null;

            // 'kind'로 먼저 검색 (Descriptor.getId() 매칭)
            String kind = jo.optString("kind", null);
            if (kind != null) {
                d = findById(descriptors, kind);
            }

            // '$class'로 검색 (Describable 클래스명 매칭)
            if (d == null) {
                kind = jo.optString("$class");
                if (kind != null) {
                    d = findByDescribableClassName(descriptors, kind);
                    if (d == null) {
                        // 호환성: Descriptor 클래스명으로도 검색
                        d = findByClassName(descriptors, kind);
                    }
                }
            }

            if (d != null) {
                items.add(d.newInstance(req, jo));
            }
        }
    }
    return items;
}
```

---

## 6. 전역 설정과 영속성

### 6.1 configure() 메서드

Jenkins 전역 설정 페이지가 제출되면 각 Descriptor의 `configure()` 메서드가 호출된다.

```java
// Descriptor.java (869행)
public boolean configure(StaplerRequest2 req, JSONObject json)
        throws FormException {
    if (Util.isOverridden(Descriptor.class, getClass(),
            "configure", StaplerRequest.class, JSONObject.class)) {
        return configure(StaplerRequest.fromStaplerRequest2(req), json);
    } else if (Util.isOverridden(Descriptor.class, getClass(),
            "configure", StaplerRequest.class)) {
        return configure(StaplerRequest.fromStaplerRequest2(req));
    } else {
        return true;
    }
}
```

Shell의 `configure()` 구현 예시:

```java
// core/src/main/java/hudson/tasks/Shell.java (237행)
@Override
public boolean configure(StaplerRequest2 req, JSONObject data)
        throws FormException {
    req.bindJSON(this, data);
    return super.configure(req, data);
}
```

### 6.2 save()와 load()

Descriptor의 설정은 `$JENKINS_HOME/{descriptorId}.xml` 파일에 저장된다.

```java
// Descriptor.java (963행)
@Override
public synchronized void save() {
    if (BulkChange.contains(this))  return;
    try {
        getConfigFile().write(this);
        SaveableListener.fireOnChange(this, getConfigFile());
    } catch (IOException e) {
        LOGGER.log(Level.WARNING, "Failed to save " + getConfigFile(), e);
    }
}

// Descriptor.java (982행)
@Override
public synchronized void load() {
    XmlFile file = getConfigFile();
    if (!file.exists())
        return;
    try {
        file.unmarshal(this);
    } catch (IOException e) {
        LOGGER.log(Level.WARNING, "Failed to load " + file, e);
    }
}

// Descriptor.java (994행)
protected XmlFile getConfigFile() {
    return new XmlFile(
        new File(Jenkins.get().getRootDir(), getId() + ".xml"));
}
```

저장 파일 경로 예시:
```
$JENKINS_HOME/hudson.tasks.Shell.xml
$JENKINS_HOME/hudson.security.csrf.DefaultCrumbIssuer.xml
```

### 6.3 PersistentDescriptor 인터페이스

`@PostConstruct`를 통해 `load()`가 자동 호출되도록 하는 마커 인터페이스:

```java
// core/src/main/java/hudson/model/PersistentDescriptor.java
public interface PersistentDescriptor extends Loadable, Saveable {
    @PostConstruct
    @Override
    void load();
}
```

Shell.DescriptorImpl이 이를 구현하는 예:

```java
// Shell.java (151행)
@Extension @Symbol("shell")
public static class DescriptorImpl extends BuildStepDescriptor<Builder>
        implements PersistentDescriptor {
    private String shell;
    // ...
}
```

`PersistentDescriptor`를 구현하면 Jenkins가 Descriptor를 인스턴스화할 때 자동으로 `load()`를 호출하여 이전에 저장된 설정을 복원한다.

### 6.4 BulkChange

대량 변경 시 매번 save()가 호출되는 것을 방지하는 메커니즘:

```java
// save() 내부
if (BulkChange.contains(this))  return;
```

`BulkChange`는 try-with-resources 패턴으로 사용되며, 범위가 끝날 때 한 번만 save()를 호출한다.

---

## 7. 폼 검증 시스템

### 7.1 doCheck 메서드 컨벤션

Descriptor에서 `doCheck{FieldName}(@QueryParameter String value)` 형태의 메서드를 정의하면, Jenkins UI가 해당 필드에 AJAX 검증을 자동 연결한다.

Shell.DescriptorImpl의 예시:

```java
// Shell.java (216행)
public FormValidation doCheckUnstableReturn(
        @QueryParameter String value) {
    value = Util.fixEmptyAndTrim(value);
    if (value == null) {
        return FormValidation.ok();
    }
    long unstableReturn;
    try {
        unstableReturn = Long.parseLong(value);
    } catch (NumberFormatException e) {
        return FormValidation.error(
            hudson.model.Messages.Hudson_NotANumber());
    }
    if (unstableReturn == 0) {
        return FormValidation.warning(
            Messages.Shell_invalid_exit_code_zero());
    }
    if (unstableReturn < 1 || unstableReturn > 255) {
        return FormValidation.error(
            Messages.Shell_invalid_exit_code_range(unstableReturn));
    }
    return FormValidation.ok();
}

// Shell.java (245행)
public FormValidation doCheckShell(@QueryParameter String value) {
    return FormValidation.validateExecutable(value);
}
```

### 7.2 getCheckMethod()

Jelly 태그 라이브러리가 이 메서드를 호출하여 폼 검증 URL을 알아낸다:

```java
// Descriptor.java (413행)
public CheckMethod getCheckMethod(String fieldName) {
    CheckMethod method = checkMethods.get(fieldName);
    if (method == null) {
        method = new CheckMethod(this, fieldName);
        checkMethods.put(fieldName, method);
    }
    return method;
}
```

### 7.3 AJAX 폼 검증 흐름

```
사용자가 필드 입력
    │
    ▼
JavaScript: 입력 변경 감지
    │
    ▼
AJAX 요청: GET /descriptorByName/{id}/checkFieldName?value=xxx
    │
    ▼
Stapler 라우팅 → Descriptor.doCheckFieldName()
    │
    ▼
FormValidation 반환 (ok / warning / error)
    │
    ▼
UI에 검증 결과 표시 (초록/노랑/빨강 아이콘 + 메시지)
```

### 7.4 doFill 메서드 (드롭다운 채우기)

드롭다운 목록의 항목을 동적으로 채우는 `doFill{FieldName}Items()` 메서드도 유사한 컨벤션을 따른다.

```java
// Descriptor.java (428행)
public void calcFillSettings(String field,
        Map<String, Object> attributes) {
    String capitalizedFieldName = ...;
    String methodName = "doFill" + capitalizedFieldName + "Items";
    Method method = ReflectionUtils.getPublicMethodNamed(
        getClass(), methodName);

    // 의존 필드 분석
    List<String> depends =
        buildFillDependencies(method, new ArrayList<>());

    if (!depends.isEmpty())
        attributes.put("fillDependsOn", String.join(" ", depends));
    attributes.put("fillUrl", String.format(
        "%s/%s/fill%sItems",
        getCurrentDescriptorByNameUrl(),
        getDescriptorUrl(), capitalizedFieldName));
}
```

`@QueryParameter` 어노테이션과 `@RelativePath`를 분석하여, 드롭다운이 어떤 다른 필드에 의존하는지 자동으로 파악한다.

### 7.5 doAutoComplete 메서드

자동완성도 유사한 컨벤션:

```java
// Descriptor.java (470행)
public void calcAutoCompleteSettings(String field,
        Map<String, Object> attributes) {
    String methodName = "doAutoComplete" + capitalizedFieldName;
    Method method = ReflectionUtils.getPublicMethodNamed(
        getClass(), methodName);
    if (method == null)
        return;  // 자동완성 없음
    // ...
    attributes.put("autoCompleteUrl", String.format(
        "%s/%s/autoComplete%s",
        getCurrentDescriptorByNameUrl(),
        getDescriptorUrl(), capitalizedFieldName));
}
```

### 7.6 Stapler 컨벤션 메서드 요약

| 메서드 패턴 | 용도 | 반환 타입 |
|------------|------|----------|
| `doCheck{Field}(@QueryParameter)` | 필드 검증 | `FormValidation` |
| `doFill{Field}Items(@QueryParameter)` | 드롭다운 채우기 | `ListBoxModel` |
| `doAutoComplete{Field}(@QueryParameter)` | 자동완성 | `AutoCompletionCandidates` |

---

## 8. Jelly/Groovy 템플릿 시스템

### 8.1 설정 페이지 경로 규칙

`getConfigPage()` 메서드가 Jelly 템플릿 경로를 결정한다:

```java
// Descriptor.java (889행)
public String getConfigPage() {
    return getViewPage(clazz,
        getPossibleViewNames("config"), "config.jelly");
}

public String getGlobalConfigPage() {
    return getViewPage(clazz,
        getPossibleViewNames("global"), null);
}
```

`getViewPage()` 내부에서는 클래스 계층을 타고 올라가며 템플릿을 찾는다:

```java
// Descriptor.java (926행)
private String getViewPage(Class<?> clazz,
        Collection<String> pageNames, String defaultValue) {
    while (clazz != Object.class && clazz != null) {
        for (String pageName : pageNames) {
            String name = clazz.getName()
                .replace('.', '/')
                .replace('$', '/')
                + "/" + pageName;
            if (clazz.getClassLoader().getResource(name) != null)
                return '/' + name;
        }
        clazz = clazz.getSuperclass();
    }
    return defaultValue;
}
```

### 8.2 템플릿 탐색 순서

`hudson.tasks.Shell` 클래스의 `config.jelly`를 찾는 과정:

```
1. hudson/tasks/Shell/config.jelly       ← 먼저 찾음
2. hudson/tasks/Shell/config.groovy      ← 대안
3. hudson/tasks/CommandInterpreter/config.jelly  ← 상위 클래스
4. hudson/tasks/Builder/config.jelly     ← 더 상위
5. ...
```

### 8.3 config.jelly/config.groovy 예시

실제 Shell의 config.groovy를 보면:

```groovy
// core/src/main/resources/hudson/tasks/Shell/config.groovy
package hudson.tasks.Shell
f=namespace(lib.FormTagLib)

f.entry(title:_("Command"),
        description:_("description",rootURL)) {
    f.textarea(name: "command", value: instance?.command,
        class: "fixed-width",
        'codemirror-mode': 'shell',
        'codemirror-config': '"mode": "text/x-sh"')
}

f.advanced() {
    f.entry(title:_("Exit code to set build unstable"),
            field: "unstableReturn") {
        f.number(clazz:"positive-number",
            value: instance?.unstableReturn,
            min:1, max:255, step:1)
    }

    if (instance?.configuredLocalRules
            || descriptor.applicableLocalRules) {
        f.entry(title: _("filterRules")) {
            f.hetero_list(
                name: "configuredLocalRules",
                hasHeader: true,
                oneEach: true,
                descriptors: descriptor.applicableLocalRules,
                items: instance?.configuredLocalRules,
                addCaption: _("addFilterRule")
            )
        }
    }
}
```

### 8.4 global.jelly 예시

Shell의 전역 설정 템플릿:

```xml
<!-- core/src/main/resources/hudson/tasks/Shell/global.jelly -->
<?jelly escape-by-default='true'?>
<j:jelly xmlns:j="jelly:core" xmlns:f="/lib/form">
    <f:section title="${%Shell}">
        <f:entry title="${%Shell executable}" field="shell">
            <f:textbox />
        </f:entry>
    </f:section>
</j:jelly>
```

### 8.5 템플릿 변수 바인딩

Jelly/Groovy 템플릿에서 사용 가능한 주요 변수:

| 변수 | 의미 | 설명 |
|------|------|------|
| `${it}` 또는 `descriptor` | Descriptor 인스턴스 | 메타데이터, 전역 설정 접근 |
| `${instance}` | Describable 인스턴스 | 현재 편집 중인 인스턴스 (null 가능) |
| `${rootURL}` | Jenkins 루트 URL | 리소스 참조용 |
| `${%문자열}` | 국제화 문자열 | .properties 파일에서 로드 |

### 8.6 config vs global

```
config.jelly / config.groovy
├── 인스턴스별 설정 (개별 빌드 스텝 구성 등)
├── ${instance} = 편집 중인 Describable 인스턴스
├── 프로젝트 설정 페이지에 표시
└── newInstance()로 인스턴스 생성 시 사용

global.jelly / global.groovy
├── Descriptor 전체의 전역 설정
├── ${it} = Descriptor 싱글턴 자체
├── Jenkins 시스템 설정 페이지에 표시
└── configure()로 전역 설정 저장 시 사용
```

### 8.7 주요 폼 태그

Jenkins Jelly 태그 라이브러리(`/lib/form`)가 제공하는 폼 태그:

| 태그 | 용도 |
|------|------|
| `f:entry` | 폼 항목 래퍼 (제목, 도움말 연결) |
| `f:textbox` | 텍스트 입력 |
| `f:textarea` | 여러 줄 텍스트 입력 |
| `f:number` | 숫자 입력 |
| `f:checkbox` | 체크박스 |
| `f:select` | 드롭다운 선택 |
| `f:password` | 비밀번호 입력 |
| `f:radio` | 라디오 버튼 |
| `f:section` | 섹션 구분 |
| `f:advanced` | "고급" 접이식 영역 |
| `f:hetero-list` | 이기종 Describable 목록 |
| `f:dropdownDescriptorSelector` | Descriptor 드롭다운 선택 |
| `f:repeatableProperty` | 반복 가능한 프로퍼티 |

`field` 속성은 자동으로 다음과 연결된다:
- `doCheck{Field}()` 검증 메서드
- `doFill{Field}Items()` 드롭다운 채우기
- `help-{field}.html` 도움말 파일

---

## 9. 도움말 파일 시스템

### 9.1 도움말 파일 탐색

`getHelpFile(String fieldName)` 메서드는 클래스 계층을 따라 올라가며 도움말 파일을 찾는다:

```java
// Descriptor.java (799행)
public String getHelpFile(Klass<?> clazz, String fieldName) {
    HelpRedirect r = helpRedirect.get(fieldName);
    if (r != null)  return r.resolve();

    for (Klass<?> c : clazz.getAncestors()) {
        String page = "/descriptor/" + getId() + "/help";
        String suffix;
        if (fieldName == null) {
            suffix = "";
        } else {
            page += '/' + fieldName;
            suffix = '-' + fieldName;
        }

        try {
            if (Stapler.getCurrentRequest2()
                    .getView(c, "help" + suffix) != null)
                return page;
        } catch (IOException e) {
            throw new Error(e);
        }

        if (getStaticHelpUrl(
                Stapler.getCurrentRequest2(), c, suffix) != null)
            return page;
    }
    return null;
}
```

### 9.2 도움말 파일 명명 규칙

```
src/main/resources/{패키지경로}/{클래스명}/
├── help.html                    # Describable 전체 도움말
├── help_ja.html                 # 일본어 도움말
├── help_ko.html                 # 한국어 도움말
├── help-{필드명}.html            # 필드별 도움말
├── help-{필드명}_ja.html         # 필드별 일본어 도움말
└── help-{필드명}_ko.html         # 필드별 한국어 도움말
```

Shell 빌더의 실제 도움말 파일 구조:

```
core/src/main/resources/hudson/tasks/Shell/
├── help.html                         # Shell 빌더 전체 도움말
├── help_de.html
├── help_fr.html
├── help_ja.html
├── help-shell.html                   # 'shell' 필드 도움말
├── help-shell_de.html
├── help-shell_ja.html
├── help-unstableReturn.html          # 'unstableReturn' 필드 도움말
└── help-unstableReturn_bg.html
```

### 9.3 로케일 해석 순서

`getStaticHelpUrl()` 메서드는 다음 순서로 로케일별 도움말 파일을 탐색한다:

```java
// Descriptor.java (1075행)
public static URL getStaticHelpUrl(StaplerRequest2 req,
        Klass<?> c, String suffix) {
    String base = "help" + suffix;
    Enumeration<Locale> locales = req.getLocales();
    while (locales.hasMoreElements()) {
        Locale locale = locales.nextElement();
        // 1. help-field_ko_KR_variant.html
        url = c.getResource(base + '_' + locale.getLanguage()
            + '_' + locale.getCountry()
            + '_' + locale.getVariant() + ".html");
        // 2. help-field_ko_KR.html
        url = c.getResource(base + '_' + locale.getLanguage()
            + '_' + locale.getCountry() + ".html");
        // 3. help-field_ko.html
        url = c.getResource(base + '_'
            + locale.getLanguage() + ".html");
        // 4. 영어인 경우: help-field.html (기본)
        if (locale.getLanguage().equals("en")) {
            url = c.getResource(base + ".html");
        }
    }
    // 5. 최종 폴백: help-field.html
    return c.getResource(base + ".html");
}
```

### 9.4 도움말 리다이렉트

한 필드의 도움말을 다른 Describable의 필드 도움말로 리다이렉트할 수 있다:

```java
// Descriptor.java (830행)
protected void addHelpFileRedirect(String fieldName,
        Class<? extends Describable> owner,
        String fieldNameToRedirectTo) {
    helpRedirect.put(fieldName,
        new HelpRedirect(owner, fieldNameToRedirectTo));
}
```

### 9.5 도움말 파일 서빙: doHelp()

```java
// Descriptor.java (1036행)
private void doHelpImpl(StaplerRequest2 req, StaplerResponse2 rsp)
        throws IOException, ServletException {
    String path = req.getRestOfPath();
    if (path.contains(".."))
        throw new ServletException("Illegal path: " + path);
    path = path.replace('/', '-');

    // 플러그인 정보 헤더 추가
    PluginWrapper pw = getPlugin();
    if (pw != null) {
        rsp.setHeader("X-Plugin-Short-Name", pw.getShortName());
        rsp.setHeader("X-Plugin-Long-Name", pw.getLongName());
        rsp.setHeader("X-Plugin-From", Messages.Descriptor_From(
            pw.getLongName(), pw.getUrl()));
    }

    // 클래스 계층을 따라 올라가며 도움말 검색
    for (Klass<?> c = getKlass(); c != null; c = c.getSuperClass()) {
        // 1. 템플릿 기반 도움말
        RequestDispatcher rd =
            Stapler.getCurrentRequest2().getView(c, "help" + path);
        if (rd != null) {
            rd.forward(req, rsp);
            return;
        }
        // 2. 정적 HTML 도움말
        URL url = getStaticHelpUrl(
            Stapler.getCurrentRequest2(), c, path);
        if (url != null) {
            rsp.setContentType("text/html;charset=UTF-8");
            try (InputStream in = url.openStream()) {
                String literal = IOUtils.toString(in, UTF_8);
                rsp.getWriter().println(Util.replaceMacro(
                    literal, Map.of("rootURL", req.getContextPath())));
            }
            return;
        }
    }
    rsp.sendError(SC_NOT_FOUND);
}
```

---

## 10. DescriptorExtensionList

### 10.1 개요

`DescriptorExtensionList`는 특정 `Describable` 타입에 해당하는 `Descriptor`만 필터링하여 보관하는 특화된 `ExtensionList`이다.

```java
// core/src/main/java/hudson/DescriptorExtensionList.java (65행)
public class DescriptorExtensionList<T extends Describable<T>,
                                     D extends Descriptor<T>>
    extends ExtensionList<D> {

    private final Class<T> describableType;

    protected DescriptorExtensionList(Jenkins jenkins,
            Class<T> describableType) {
        super(jenkins, (Class) Descriptor.class);
        this.describableType = describableType;
    }
}
```

### 10.2 팩토리 메서드

```java
// DescriptorExtensionList.java (70행)
public static <T extends Describable<T>, D extends Descriptor<T>>
DescriptorExtensionList<T, D> createDescriptorList(
        Jenkins jenkins, Class<T> describableType) {
    if (describableType == Publisher.class) {
        // Publisher는 특별한 정렬이 필요
        return (DescriptorExtensionList)
            new Publisher.DescriptorExtensionListImpl(jenkins);
    }
    return new DescriptorExtensionList<>(jenkins, describableType);
}
```

`Publisher` 타입은 Recorder > Unknown > Notifier 순으로 정렬하는 특수 로직이 필요하여 별도의 서브타입을 사용한다.

### 10.3 핵심 메서드

**findByName() -- ID로 Descriptor 검색:**
```java
// DescriptorExtensionList.java (168행)
public @CheckForNull D findByName(String id) {
    for (D d : this)
        if (d.getId().equals(id))
            return d;
    return null;
}
```

**find(Class) -- Describable 클래스로 검색:**
```java
// DescriptorExtensionList.java (123행)
public D find(Class<? extends T> type) {
    for (D d : this)
        if (d.clazz == type)
            return d;
    return null;
}
```

**newInstanceFromRadioList() -- 라디오 버튼 그룹에서 인스턴스 생성:**
```java
// DescriptorExtensionList.java (139행)
@CheckForNull
public T newInstanceFromRadioList(JSONObject config) throws FormException {
    if (config.isNullObject())
        return null;    // 아무것도 선택되지 않음
    int idx = config.getInt("value");
    return get(idx).newInstance(
        Stapler.getCurrentRequest2(), config);
}
```

### 10.4 지연 로딩과 필터링

`DescriptorExtensionList`는 마스터 `ExtensionList<Descriptor>`에서 해당 타입의 Descriptor만 필터링하여 로드한다:

```java
// DescriptorExtensionList.java (216행)
private List<ExtensionComponent<D>> _load(
        Iterable<ExtensionComponent<Descriptor>> set) {
    List<ExtensionComponent<D>> r = new ArrayList<>();
    for (ExtensionComponent<Descriptor> c : set) {
        Descriptor d = c.getInstance();
        try {
            if (d.getT() == describableType)
                r.add((ExtensionComponent) c);
        } catch (IllegalStateException e) {
            LOGGER.log(Level.SEVERE,
                d.getClass()
                + " doesn't extend Descriptor with a type parameter",
                e);
        }
    }
    return r;
}
```

`d.getT()`는 Descriptor의 타입 파라미터 T를 리플렉션으로 추출하여 `describableType`과 비교한다.

### 10.5 동기화와 데드락 방지

```java
// DescriptorExtensionList.java (193행)
@Override
protected Object getLoadLock() {
    // 싱글턴 확장 리스트의 락을 사용하여 데드락 방지 (JENKINS-55361)
    return getDescriptorExtensionList().getLoadLock();
}
```

### 10.6 사용 예시

각 Describable 타입은 `all()` 정적 메서드를 제공하여 해당 타입의 모든 Descriptor를 조회할 수 있게 한다:

```java
// Builder.java (77행)
public static DescriptorExtensionList<Builder, Descriptor<Builder>> all() {
    return Jenkins.get().getDescriptorList(Builder.class);
}

// Publisher.java (178행)
public static DescriptorExtensionList<Publisher, Descriptor<Publisher>> all() {
    return Jenkins.get().getDescriptorList(Publisher.class);
}
```

---

## 11. 명명 규칙과 컨벤션

### 11.1 Descriptor 클래스 명명

Jenkins에서 Descriptor 클래스는 관례적으로 `DescriptorImpl`이라 명명하며, Describable 클래스의 **중첩 static 클래스**로 정의한다.

```java
public class MyBuilder extends Builder {
    // Describable 구현 ...

    @Extension  // 반드시 @Extension 어노테이션
    public static class DescriptorImpl
            extends BuildStepDescriptor<Builder> {
        // Descriptor 구현 ...
    }
}
```

이 컨벤션을 따르면:
- `Descriptor()`의 무인자 생성자가 `getEnclosingClass()`로 자동으로 Describable 클래스를 찾는다
- 개발자가 명시적으로 클래스를 전달할 필요가 없다

### 11.2 리소스 파일 배치

```
src/main/resources/
└── {패키지를 /로 변환}/{클래스명}/
    ├── config.jelly           # 인스턴스 설정 폼
    ├── config.groovy          # 또는 Groovy 뷰
    ├── config.properties      # 국제화 문자열
    ├── config_ko.properties   # 한국어 국제화
    ├── global.jelly           # 전역 설정 폼
    ├── help.html              # 전체 도움말
    ├── help-{필드}.html       # 필드별 도움말
    └── help-{필드}_ko.html    # 필드별 한국어 도움말
```

실제 Shell 빌더의 리소스 구조:

```
core/src/main/resources/hudson/tasks/Shell/
├── config.groovy              # 인스턴스 설정 (Groovy 뷰)
├── config.properties          # 국제화 문자열
├── global.jelly               # 전역 설정 (shell 경로)
├── help.html                  # 전체 도움말
├── help_de.html               # 독일어
├── help_fr.html               # 프랑스어
├── help_ja.html               # 일본어
├── help-shell.html            # 'shell' 필드 도움말
├── help-shell_bg.html         # 불가리아어
├── help-shell_de.html         # 독일어
├── help-unstableReturn.html   # 'unstableReturn' 필드 도움말
└── help-unstableReturn_bg.html
```

### 11.3 @Extension과 @Symbol

```java
@Extension       // Jenkins가 이 Descriptor를 자동 발견
@Symbol("shell") // Pipeline DSL에서 사용할 심볼 이름
public static class DescriptorImpl extends BuildStepDescriptor<Builder>
        implements PersistentDescriptor {
    // ...
}
```

- `@Extension`: Jenkins의 확장점 발견 메커니즘에 등록
- `@Symbol`: Pipeline 스크립트에서 `shell { ... }` 형태로 사용 가능

### 11.4 @DataBoundConstructor와 @DataBoundSetter

Describable 클래스 쪽의 컨벤션:

```java
public class Shell extends CommandInterpreter {
    @DataBoundConstructor           // 필수 파라미터
    public Shell(String command) {
        super(command);
    }

    @DataBoundSetter                // 선택적 파라미터
    public void setUnstableReturn(Integer unstableReturn) {
        this.unstableReturn = unstableReturn;
    }

    @DataBoundSetter
    public void setConfiguredLocalRules(
            List<EnvVarsFilterLocalRule> configuredLocalRules) {
        this.configuredLocalRules = configuredLocalRules;
    }
}
```

---

## 12. 실전 활용 예시: Shell 빌드 스텝

### 12.1 전체 구조

Shell 빌드 스텝은 Descriptor 패턴의 전형적인 활용 사례이다.

```
                  Describable<Builder>
                       │
                  Builder (추상)
                       │
              CommandInterpreter (추상)
                       │
                     Shell  ◄── Describable 인스턴스
                       │
                       │  getDescriptor()
                       ▼
              Shell.DescriptorImpl  ◄── Descriptor 싱글턴
                 extends BuildStepDescriptor<Builder>
                 implements PersistentDescriptor
```

### 12.2 Describable 측 (Shell)

```java
// core/src/main/java/hudson/tasks/Shell.java
public class Shell extends CommandInterpreter {

    @DataBoundConstructor
    public Shell(String command) {
        super(LineEndingConversion.convertEOL(
            command, LineEndingConversion.EOLType.Unix));
    }

    private Integer unstableReturn;

    @DataBoundSetter
    public void setUnstableReturn(Integer unstableReturn) {
        this.unstableReturn = unstableReturn;
    }

    @Override
    public DescriptorImpl getDescriptor() {
        return (DescriptorImpl) super.getDescriptor();
    }
}
```

### 12.3 Descriptor 측 (Shell.DescriptorImpl)

```java
// Shell.java (150행)
@Extension @Symbol("shell")
public static class DescriptorImpl extends BuildStepDescriptor<Builder>
        implements PersistentDescriptor {

    // 전역 설정: shell 실행 파일 경로
    private String shell;

    // 적용 가능 여부 (모든 프로젝트 타입에 적용)
    @Override
    public boolean isApplicable(Class<? extends AbstractProject> jobType) {
        return true;
    }

    // UI 표시 이름
    @NonNull @Override
    public String getDisplayName() {
        return Messages.Shell_DisplayName();
    }

    // 전역 설정 저장
    @Override
    public boolean configure(StaplerRequest2 req, JSONObject data)
            throws FormException {
        req.bindJSON(this, data);
        return super.configure(req, data);
    }

    // 폼 검증: unstableReturn 필드
    public FormValidation doCheckUnstableReturn(
            @QueryParameter String value) {
        // ...검증 로직...
    }

    // 폼 검증: shell 필드
    public FormValidation doCheckShell(@QueryParameter String value) {
        return FormValidation.validateExecutable(value);
    }

    // 전역 설정 getter/setter
    public String getShell() { return shell; }
    public void setShell(String shell) {
        this.shell = Util.fixEmptyAndTrim(shell);
        save();  // 변경 시 즉시 저장
    }
}
```

### 12.4 데이터 흐름 전체 그림

```
[사용자] -- 빌드 설정 페이지 열기 -->

[Jenkins UI]
    │
    ├── config.groovy 렌더링
    │   ├── ${instance} = 기존 Shell 인스턴스 (또는 null)
    │   └── ${descriptor} = Shell.DescriptorImpl 싱글턴
    │
    ├── 사용자가 필드 입력
    │   └── AJAX: doCheckUnstableReturn() 호출 → FormValidation 반환
    │
    └── 폼 제출
        │
        ▼
[Stapler] -- JSONObject 생성 -->
    │
    ▼
[Shell.DescriptorImpl.newInstance(req, json)]
    │
    ├── bindJSON → @DataBoundConstructor 호출
    │              new Shell(command)
    │
    ├── @DataBoundSetter 호출
    │   setUnstableReturn(...)
    │   setConfiguredLocalRules(...)
    │
    └── Shell 인스턴스 반환
        │
        ▼
[프로젝트 XML로 직렬화/저장]


[관리자] -- 시스템 설정 페이지 열기 -->

[Jenkins UI]
    │
    ├── global.jelly 렌더링
    │   └── ${it} = Shell.DescriptorImpl 싱글턴
    │
    ├── 사용자가 shell 경로 입력
    │   └── AJAX: doCheckShell() 호출
    │
    └── 폼 제출
        │
        ▼
[Shell.DescriptorImpl.configure(req, json)]
    │
    ├── req.bindJSON(this, data)  → setShell() 호출
    └── save()  → $JENKINS_HOME/hudson.tasks.Shell.xml 저장
```

---

## 13. Descriptor 패턴의 확장 계층

### 13.1 Descriptor 서브타입 계층

Jenkins의 주요 확장점은 Descriptor의 서브타입을 정의하여 추가적인 메타데이터를 제공한다:

```
Descriptor<T>
│
├── BuildStepDescriptor<T extends BuildStep & Describable<T>>
│   ├── isApplicable(Class<? extends AbstractProject> jobType)
│   │   └── 프로젝트 타입별 표시 여부 제어
│   └── filter(descriptors, projectType) 정적 메서드
│
├── SCMDescriptor<T extends SCM>
│   └── SCM 관련 추가 메타데이터
│
├── TriggerDescriptor
│   └── Trigger 관련 추가 메타데이터
│
├── ViewDescriptor
│   └── View 관련 추가 메타데이터
│
├── NodeDescriptor
│   └── Node 관련 추가 메타데이터
│
└── GlobalConfiguration (자기 자신이 Describable)
    └── 시스템 설정 전용 Descriptor
```

### 13.2 BuildStepDescriptor

빌드 스텝에 특화된 Descriptor로, `isApplicable()` 메서드를 통해 어떤 프로젝트 타입에서 사용 가능한지 제어한다:

```java
// core/src/main/java/hudson/tasks/BuildStepDescriptor.java (44행)
public abstract class BuildStepDescriptor<T extends BuildStep & Describable<T>>
    extends Descriptor<T> {

    public abstract boolean isApplicable(
        Class<? extends AbstractProject> jobType);

    // 프로젝트 타입에 맞는 Descriptor만 필터링
    public static <T extends BuildStep & Describable<T>>
    List<Descriptor<T>> filter(List<Descriptor<T>> base,
            Class<? extends AbstractProject> type) {
        Descriptor pd = Jenkins.get().getDescriptor((Class) type);
        List<Descriptor<T>> r = new ArrayList<>(base.size());
        for (Descriptor<T> d : base) {
            if (pd instanceof AbstractProjectDescriptor
                    && !((AbstractProjectDescriptor) pd).isApplicable(d))
                continue;
            if (d instanceof BuildStepDescriptor<T> bd) {
                if (!bd.isApplicable(type))  continue;
                r.add(bd);
            } else {
                r.add(d);  // 1.150 이전 호환
            }
        }
        return r;
    }
}
```

### 13.3 주요 Describable 활용 사례

| Describable 타입 | 용도 | Descriptor 서브타입 |
|-----------------|------|-------------------|
| `Builder` | 빌드 단계 (Shell, Maven 등) | `BuildStepDescriptor<Builder>` |
| `Publisher` | 빌드 후 작업 (통지, 리포트 등) | `BuildStepDescriptor<Publisher>` |
| `Recorder` | Publisher 하위, 결과 기록 | `BuildStepDescriptor<Publisher>` |
| `Notifier` | Publisher 하위, 알림 전송 | `BuildStepDescriptor<Publisher>` |
| `SCM` | 소스 코드 관리 (Git, SVN 등) | `SCMDescriptor<SCM>` |
| `Trigger<?>` | 빌드 트리거 (cron, webhook 등) | `TriggerDescriptor` |
| `SecurityRealm` | 인증 (LDAP, Active Directory 등) | `Descriptor<SecurityRealm>` |
| `AuthorizationStrategy` | 인가 (Matrix, Project-based) | `Descriptor<AuthorizationStrategy>` |
| `Cloud` | 클라우드 에이전트 (Kubernetes, EC2) | `Descriptor<Cloud>` |
| `NodeProperty<?>` | 노드 속성 | `NodePropertyDescriptor` |
| `JobProperty<?>` | 잡 속성 | `JobPropertyDescriptor` |
| `View` | 대시보드 뷰 (ListView 등) | `ViewDescriptor` |
| `GlobalConfiguration` | 전역 플러그인 설정 | 자기 자신 |

### 13.4 DescriptorVisibilityFilter

특정 컨텍스트에서 Descriptor의 가시성을 제어하는 필터:

```java
// core/src/main/java/hudson/model/DescriptorVisibilityFilter.java
public abstract class DescriptorVisibilityFilter implements ExtensionPoint {
    /**
     * contextClass: 어디서 평가되는지 (예: FreeStyleProject)
     * descriptor: 가시성을 평가할 Descriptor
     * 반환: true면 표시, false면 숨김
     */
    public abstract boolean filter(
        @CheckForNull Object context,
        @NonNull Descriptor descriptor);
}
```

### 13.5 PropertyType과 리플렉션 향상

Descriptor는 Describable 클래스의 프로퍼티 타입 정보를 리플렉션으로 분석하여 Jelly 템플릿에서 사용할 수 있게 한다:

```java
// Descriptor.java (168행)
public static final class PropertyType {
    public final Class clazz;
    public final Type type;
    public final String displayName;

    // 컬렉션/배열 타입의 요소 타입
    public Class getItemType() { ... }

    // 요소 타입의 Descriptor
    public Descriptor getItemTypeDescriptor() { ... }

    // 해당 타입에 적용 가능한 모든 Descriptor
    public List<? extends Descriptor> getApplicableDescriptors() {
        return Jenkins.get().getDescriptorList(clazz);
    }

    // 컬렉션 요소 타입에 적용 가능한 모든 Descriptor
    public List<? extends Descriptor> getApplicableItemDescriptors() {
        return Jenkins.get().getDescriptorList(getItemType());
    }
}
```

이 정보는 Jelly 태그가 `f:hetero-list`, `f:dropdownDescriptorSelector` 등에서 자동으로 사용 가능한 Descriptor 목록을 렌더링할 때 활용된다.

---

## 14. 왜 이런 패턴인가

### 14.1 인스턴스와 메타데이터 분리의 필요성

Jenkins에서 하나의 프로젝트에 Shell 빌드 스텝이 3개 있을 수 있다. 각각의 Shell 인스턴스는 서로 다른 `command` 값을 가진다. 하지만 "이 빌드 스텝의 이름은 'Execute shell'이다", "설정 폼은 config.groovy에 있다", "shell 경로의 전역 기본값은 /bin/sh이다" 같은 정보는 **모든 Shell 인스턴스가 공유**한다.

```
인스턴스 데이터 (Describable)        메타데이터 (Descriptor)
─────────────────────────          ─────────────────────
command = "make test"              displayName = "Execute shell"
unstableReturn = 99                configPage = config.groovy
                                   globalConfigPage = global.jelly
command = "npm install"            shell = "/bin/sh"
unstableReturn = null              isApplicable() = true
                                   doCheckUnstableReturn()
command = "deploy.sh"              doCheckShell()
unstableReturn = 1
```

이 분리가 없으면:
- 모든 Shell 인스턴스가 메타데이터를 중복 보유해야 한다
- 전역 설정 변경 시 모든 인스턴스를 찾아서 업데이트해야 한다
- 인스턴스 직렬화에 불필요한 메타데이터가 포함된다

### 14.2 플러그인 확장성

Descriptor 패턴은 플러그인이 Jenkins의 UI와 무관하게 동작할 수 있게 한다:

```java
// 프로그래밍 방식으로 Shell 인스턴스 생성 (UI 없이)
Shell shell = new Shell("echo hello");
shell.setUnstableReturn(1);
// Descriptor 없이도 인스턴스 사용 가능
```

동시에 플러그인이 UI를 제공하고 싶으면:
- `DescriptorImpl`을 정의하고 `@Extension`을 붙이면 된다
- config.jelly를 작성하면 폼이 자동 생성된다
- `doCheck*()` 메서드를 추가하면 검증이 자동 연결된다

### 14.3 Convention over Configuration

Jenkins의 Descriptor 패턴은 "관례에 의한 설정(Convention over Configuration)" 원칙을 철저히 따른다:

| 관례 | 효과 |
|------|------|
| 중첩 클래스 `DescriptorImpl` | 자동 타입 추론 |
| `@Extension` | 자동 등록 |
| `doCheck{Field}()` | 자동 검증 연결 |
| `doFill{Field}Items()` | 자동 드롭다운 연결 |
| `help-{field}.html` | 자동 도움말 연결 |
| `config.jelly` 위치 | 자동 폼 발견 |
| `@DataBoundConstructor` | 자동 데이터 바인딩 |
| `@DataBoundSetter` | 자동 선택적 필드 바인딩 |

개발자는 이 관례를 따르기만 하면 Jenkins가 나머지를 자동으로 처리한다.

### 14.4 타입 안전성과 컴파일 타임 검증

자기 참조 제네릭(`T extends Describable<T>`)과 생성자의 검증 로직이 결합되어, **잘못된 Descriptor-Describable 쌍은 런타임 초기에 즉시 감지**된다:

```java
// 생성자에서의 세 가지 검증:
// 1. 외부 클래스 존재 확인
if (clazz == null)
    throw new AssertionError("doesn't have an outer class");

// 2. 타입 파라미터 호환성 확인
if (!t.isAssignableFrom(clazz))
    throw new AssertionError("wrong outer class");

// 3. getDescriptor() 반환 타입 확인
if (!getd.getReturnType().isAssignableFrom(getClass()))
    throw new AssertionError("must be assignable");
```

### 14.5 영속성 분리

Describable 인스턴스는 프로젝트 XML에 직렬화되고, Descriptor의 전역 설정은 별도의 XML 파일에 저장된다. 이 분리 덕분에:

- 프로젝트를 복사해도 전역 설정은 공유된다
- 전역 설정을 변경해도 기존 프로젝트 설정에 영향을 주지 않는다
- 백업/복구가 독립적이다

```
$JENKINS_HOME/
├── jobs/
│   └── my-project/
│       └── config.xml           ← Shell 인스턴스들의 설정
│           <builders>
│             <hudson.tasks.Shell>
│               <command>make</command>
│             </hudson.tasks.Shell>
│           </builders>
│
├── hudson.tasks.Shell.xml       ← Shell Descriptor의 전역 설정
│   <hudson.tasks.Shell_-DescriptorImpl>
│     <shell>/usr/local/bin/bash</shell>
│   </hudson.tasks.Shell_-DescriptorImpl>
```

### 14.6 Object/Class 비유의 확장

```
Java 세계                    Jenkins Descriptor 세계
────────────                ─────────────────────────
Object                      Describable (인스턴스)
Class                       Descriptor (메타데이터)
instanceof                  Descriptor.isInstance()
Class.newInstance()         Descriptor.newInstance()
Class.getName()            Descriptor.getDisplayName()
Class.getFields()          Descriptor.getPropertyType()
(없음)                      Descriptor.getConfigPage() ← UI 생성
(없음)                      Descriptor.doCheck*()      ← 폼 검증
(없음)                      Descriptor.save()/load()   ← 영속성
```

Jenkins의 Descriptor는 Java의 Class가 해주지 못하는 것들을 추가로 제공한다: UI 렌더링, 폼 검증, 전역 설정 관리, 영속성. 이것이 단순히 리플렉션에 의존하지 않고 별도의 Descriptor 객체를 두는 이유이다.

### 14.7 설계 트레이드오프

**장점:**
- 강력한 확장성 -- 새로운 빌드 스텝/SCM/트리거를 추가하는 데 일관된 방법 제공
- UI와 로직 분리 -- Describable은 UI 없이 동작 가능
- 자동 발견 -- `@Extension`으로 등록만 하면 Jenkins가 자동으로 찾아서 사용
- 관례 기반 개발 -- 보일러플레이트 최소화

**단점:**
- 학습 곡선 -- 패턴을 이해하지 못하면 플러그인 개발이 어렵다
- 간접 참조 -- 인스턴스에서 메타데이터를 얻으려면 항상 `getDescriptor()` 경유
- 리플렉션 의존 -- 타입 파라미터 추론, 메서드 이름 규칙 등이 런타임 리플렉션에 의존
- 중첩 클래스 강제 -- 관례를 따르려면 반드시 중첩 static 클래스로 정의해야 함

그럼에도 이 패턴은 15년 이상 Jenkins의 2,000개 이상의 플러그인 생태계를 성공적으로 지탱해 왔으며, Jenkins의 "모든 것이 플러그인" 철학의 기술적 토대이다.

---

## 참조 소스 파일

| 파일 | 줄 수 | 역할 |
|------|------|------|
| `core/src/main/java/hudson/model/Describable.java` | 51 | Describable 인터페이스 |
| `core/src/main/java/hudson/model/Descriptor.java` | 1334 | Descriptor 추상 클래스 |
| `core/src/main/java/hudson/DescriptorExtensionList.java` | 251 | Descriptor 특화 ExtensionList |
| `core/src/main/java/hudson/model/AbstractDescribableImpl.java` | 34 | Describable 기본 구현 (Deprecated) |
| `core/src/main/java/hudson/model/PersistentDescriptor.java` | 19 | 자동 load() 마커 인터페이스 |
| `core/src/main/java/hudson/tasks/Builder.java` | 80 | 빌드 스텝 기본 클래스 |
| `core/src/main/java/hudson/tasks/Publisher.java` | 181 | 빌드 후 작업 기본 클래스 |
| `core/src/main/java/hudson/tasks/BuildStepDescriptor.java` | 92 | 빌드 스텝 Descriptor 기본 클래스 |
| `core/src/main/java/hudson/tasks/Shell.java` | 263 | Shell 빌드 스텝 (실전 예시) |
| `core/src/main/java/jenkins/model/GlobalConfiguration.java` | 80+ | 전역 설정 Self-Describing 패턴 |
| `core/src/main/resources/hudson/tasks/Shell/config.groovy` | 49 | Shell 인스턴스 설정 폼 |
| `core/src/main/resources/hudson/tasks/Shell/global.jelly` | 33 | Shell 전역 설정 폼 |
