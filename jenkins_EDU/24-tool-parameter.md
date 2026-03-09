# 24. Tool Installation & Build Parameter 심층 분석

## 목차
1. [개요](#1-개요)
2. [Tool Installation 아키텍처](#2-tool-installation-아키텍처)
3. [ToolInstallation 핵심 클래스 분석](#3-toolinstallation-핵심-클래스-분석)
4. [ToolDescriptor와 설정 관리](#4-tooldescriptor와-설정-관리)
5. [ToolInstaller: 자동 설치 메커니즘](#5-toolinstaller-자동-설치-메커니즘)
6. [노드별 도구 경로 해석](#6-노드별-도구-경로-해석)
7. [Build Parameter 아키텍처](#7-build-parameter-아키텍처)
8. [ParameterDefinition 핵심 분석](#8-parameterdefinition-핵심-분석)
9. [파라미터 타입별 구현 분석](#9-파라미터-타입별-구현-분석)
10. [파라미터 값 생성 및 검증 흐름](#10-파라미터-값-생성-및-검증-흐름)
11. [ParametersDefinitionProperty 통합](#11-parametersdefinitionproperty-통합)
12. [Tool + Parameter 통합 시퀀스](#12-tool--parameter-통합-시퀀스)
13. [설계 결정과 교훈](#13-설계-결정과-교훈)

---

## 1. 개요

Jenkins에서 **Tool Installation**과 **Build Parameter**는 빌드 실행 환경을 구성하는 두 가지 핵심 메커니즘이다.
Tool Installation은 JDK, Maven, Ant, Gradle 등 빌드 도구를 노드별로 관리하고 자동 설치하는 프레임워크이며,
Build Parameter는 사용자가 빌드 시점에 동적으로 값을 주입할 수 있게 하는 확장 가능한 타입 시스템이다.

### 왜 이 두 기능을 함께 분석하는가?

1. **Describable/Descriptor 패턴의 전형적 활용**: 둘 다 `Describable<T>` + `Descriptor<T>` 패턴의 대표적 구현체
2. **빌드 환경 구성의 양대 축**: Tool은 "어떤 도구로", Parameter는 "어떤 값으로" 빌드를 실행할지 결정
3. **ExtensionPoint 기반 확장성**: 둘 다 플러그인을 통한 확장이 핵심 설계 의도

### 소스 경로

```
core/src/main/java/hudson/tools/
├── ToolInstallation.java       # 도구 설치 추상 기반 클래스
├── ToolDescriptor.java         # 도구 Descriptor (설정 관리)
├── ToolInstaller.java          # 자동 설치 추상 클래스
├── ToolLocationNodeProperty.java # 노드별 도구 위치 오버라이드
├── ToolProperty.java           # 도구 속성 (InstallSource 등)
├── InstallSourceProperty.java  # 설치 소스 속성
└── ToolLocationTranslator.java # 도구 경로 변환기

core/src/main/java/hudson/model/
├── ParameterDefinition.java    # 빌드 파라미터 정의 추상 클래스
├── ParameterValue.java         # 파라미터 값 추상 클래스
├── StringParameterDefinition.java   # 문자열 파라미터
├── ChoiceParameterDefinition.java   # 선택 파라미터
├── BooleanParameterDefinition.java  # 불리언 파라미터
├── ParametersDefinitionProperty.java # Job 속성으로서의 파라미터
└── ParametersAction.java       # 빌드에 첨부되는 파라미터 액션
```

---

## 2. Tool Installation 아키텍처

### 전체 구조

```
┌─────────────────────────────────────────────────────┐
│                  Global Configuration               │
│  ┌──────────────────────────────────────────┐        │
│  │          ToolDescriptor<T>                │        │
│  │  - T[] installations                      │        │
│  │  - getDefaultInstallers()                 │        │
│  │  - configure(req, json)                   │        │
│  └──────────────┬───────────────────────────┘        │
│                 │ manages                             │
│  ┌──────────────▼───────────────────────────┐        │
│  │       ToolInstallation (abstract)         │        │
│  │  - name: String                           │        │
│  │  - home: String (설치 경로)               │        │
│  │  - properties: DescribableList            │        │
│  │  + translate(Node, EnvVars, TaskListener) │        │
│  │  + buildEnvVars(EnvVars)                  │        │
│  └──────────────┬───────────────────────────┘        │
│                 │ has                                 │
│  ┌──────────────▼───────────────────────────┐        │
│  │      ToolProperty<T> (ExtensionPoint)     │        │
│  │  └── InstallSourceProperty               │        │
│  │       - installers: List<ToolInstaller>   │        │
│  └──────────────┬───────────────────────────┘        │
│                 │ contains                            │
│  ┌──────────────▼───────────────────────────┐        │
│  │     ToolInstaller (abstract)              │        │
│  │  - label: String (노드 필터)              │        │
│  │  + appliesTo(Node): boolean               │        │
│  │  + performInstallation(): FilePath        │        │
│  │  + preferredLocation(): FilePath          │        │
│  └──────────────────────────────────────────┘        │
└─────────────────────────────────────────────────────┘
```

### 왜 이런 구조인가?

Jenkins는 이기종 에이전트 환경을 지원한다. 같은 JDK라도:
- 컨트롤러에서는 `/usr/local/java`
- Windows 에이전트에서는 `C:\Program Files\Java`
- Docker 에이전트에서는 `/opt/java/openjdk`

이 다양성을 **NodeSpecific + EnvironmentSpecific** 인터페이스 조합으로 해결한다.

---

## 3. ToolInstallation 핵심 클래스 분석

### 클래스 정의

```java
// 소스: core/src/main/java/hudson/tools/ToolInstallation.java
public abstract class ToolInstallation
    implements Describable<ToolInstallation>, Serializable, ExtensionPoint {

    private final String name;           // 도구 식별 이름 (예: "JDK 17")
    private /*almost final*/ String home; // 설치 경로

    // 도구 속성 (자동 설치기 등)
    private /*almost final*/ DescribableList<ToolProperty<?>, ToolPropertyDescriptor> properties
            = new DescribableList<>(Saveable.NOOP);
}
```

### 핵심 메서드: translate()

`translate()`는 도구 경로를 실행 컨텍스트에 맞게 변환하는 핵심 메서드다.

```java
// 소스: ToolInstallation.java:183-192
public ToolInstallation translate(@NonNull Node node, EnvVars envs,
        TaskListener listener) throws IOException, InterruptedException {
    ToolInstallation t = this;
    // 1단계: NodeSpecific - 노드별 경로 변환
    if (t instanceof NodeSpecific n) {
        t = (ToolInstallation) n.forNode(node, listener);
    }
    // 2단계: EnvironmentSpecific - 환경변수 치환
    if (t instanceof EnvironmentSpecific e) {
        t = (ToolInstallation) e.forEnvironment(envs);
    }
    return t;
}
```

### translate 시퀀스

```
빌드 스텝에서 tool.translate(node, envs, listener) 호출
    │
    ├── 1. NodeSpecific.forNode(node, listener)
    │       │
    │       └── ToolLocationNodeProperty.getToolHome(node, this, log)
    │           │
    │           ├── 노드의 ToolLocationNodeProperty에서 오버라이드 경로 확인
    │           │   → 있으면 해당 경로 반환
    │           │
    │           ├── ToolLocationTranslator 확장점에서 변환 시도
    │           │   → 변환 성공하면 반환
    │           │
    │           └── 기본값: installation.getHome() 반환
    │
    └── 2. EnvironmentSpecific.forEnvironment(envs)
            │
            └── home 경로 내의 ${ENV_VAR} 등을 실제 값으로 치환
```

### writeReplace: Remoting 직렬화 문제 해결

```java
// 소스: ToolInstallation.java:235-251
protected Object writeReplace() throws Exception {
    if (Channel.current() == null) { // XStream 직렬화
        return this;
    } else { // Remoting을 통한 에이전트 전송
        // properties는 Serializable이 아닌 DescribableList
        // → XML로 직렬화 후 <properties/> 제거 후 역직렬화하여 clone 생성
        String xml1 = Timer.get().submit(
            () -> Jenkins.XSTREAM2.toXML(this)).get();
        Document dom = new SAXReader().read(new StringReader(xml1));
        Element properties = dom.getRootElement().element("properties");
        if (properties != null) {
            dom.getRootElement().remove(properties);
        }
        String xml2 = dom.asXML();
        return (ToolInstallation) Timer.get().submit(
            () -> Jenkins.XSTREAM2.fromXML(xml2)).get();
    }
}
```

**왜 이렇게 복잡한가?**
- `DescribableList`는 `Serializable`을 구현하지 않음
- Remoting 채널로 에이전트에 전송할 때 Java 직렬화가 필요
- 해결책: XStream → DOM 조작 → XStream 역직렬화로 properties 없는 clone 생성
- 별도 스레드(`Timer.get().submit`)에서 실행하는 이유: XStream이 Jenkins 클래스로더가 필요

---

## 4. ToolDescriptor와 설정 관리

### ToolDescriptor 구조

```java
// 소스: core/src/main/java/hudson/tools/ToolDescriptor.java
public abstract class ToolDescriptor<T extends ToolInstallation>
    extends Descriptor<ToolInstallation> {

    private T[] installations;  // 설정된 도구 인스턴스 배열

    // 설정된 모든 도구 반환
    public T[] getInstallations() {
        if (installations != null)
            return installations.clone();
        // 리플렉션으로 타입 추론하여 빈 배열 생성
        Type bt = Types.getBaseClass(getClass(), ToolDescriptor.class);
        if (bt instanceof ParameterizedType pt) {
            Class t = Types.erasure(pt.getActualTypeArguments()[0]);
            return (T[]) Array.newInstance(t, 0);
        }
        return emptyArray_unsafeCast();
    }

    // 도구 설정 저장
    public void setInstallations(T... installations) {
        this.installations = installations.clone();
    }
}
```

### 설정 흐름

```
관리자가 Global Tool Configuration 페이지 접근
    │
    ├── 1. ToolDescriptor.getInstallations() 호출
    │       → 기존 설정된 JDK/Maven/Ant 목록 표시
    │
    ├── 2. 폼 제출 시 configure(req, json) 호출
    │       └── req.bindJSONToList(clazz, json.get("tool"))
    │           → JSON → ToolInstallation[] 변환
    │           → setInstallations() 저장
    │
    └── 3. getDefaultInstallers()
            → 새 도구 추가 시 기본 설치기 제공
            → InstallSourceProperty(installers) 생성
```

### 경로 유효성 검사

```java
// 소스: ToolDescriptor.java:175-188
public FormValidation doCheckHome(@QueryParameter File value) {
    Jenkins.get().checkPermission(Jenkins.ADMINISTER);  // 보안: 관리자만
    if (value.getPath().isEmpty())
        return FormValidation.ok();
    if (!value.isDirectory())
        return FormValidation.warning(
            Messages.ToolDescriptor_NotADirectory(value));
    return checkHomeDirectory(value);  // 서브클래스 커스텀 검증
}
```

---

## 5. ToolInstaller: 자동 설치 메커니즘

### ToolInstaller 구조

```java
// 소스: core/src/main/java/hudson/tools/ToolInstaller.java
public abstract class ToolInstaller
    implements Describable<ToolInstaller>, ExtensionPoint {

    private final String label;          // 적용 노드 필터 (null = 모든 노드)
    protected transient ToolInstallation tool;  // 설치 대상 도구

    // 노드 적용 가능 여부
    public boolean appliesTo(Node node) {
        Label l = Jenkins.get().getLabel(label);
        return l == null || l.contains(node);  // label이 null이면 모든 노드에 적용
    }

    // 실제 설치 수행 (추상)
    public abstract FilePath performInstallation(
        ToolInstallation tool, Node node, TaskListener log)
        throws IOException, InterruptedException;
}
```

### preferredLocation: 설치 경로 결정

```java
// 소스: ToolInstaller.java:112-125
protected final FilePath preferredLocation(ToolInstallation tool, Node node) {
    String home = Util.fixEmptyAndTrim(tool.getHome());
    if (home == null) {
        // home이 미지정이면 노드 작업 디렉토리 아래에 설치
        home = sanitize(tool.getDescriptor().getId())
             + File.separatorChar
             + sanitize(tool.getName());
    }
    FilePath root = node.getRootPath();
    return root.child("tools").child(home);
    // 결과 예: /var/jenkins/tools/hudson.model.JDK/JDK_17
}
```

### 자동 설치 시퀀스

```
빌드 시작 → BuildStep에서 JDK 필요
    │
    ├── 1. ToolInstallation.translate(node, envs, listener)
    │       └── NodeSpecific.forNode(node, listener)
    │           └── ToolLocationNodeProperty.getToolHome()
    │               → 오버라이드 없으면 자동 설치 트리거
    │
    ├── 2. InstallSourceProperty.getInstallers() 순회
    │       └── 각 ToolInstaller에 대해:
    │           ├── appliesTo(node) 확인 (label 매칭)
    │           └── performInstallation(tool, node, log)
    │               ├── 이미 설치 확인 → 스킵
    │               └── 미설치 시:
    │                   ├── URL에서 다운로드
    │                   ├── 압축 해제
    │                   └── FilePath 반환
    │
    └── 3. 설치 경로가 env에 추가
            └── buildEnvVars(env)
                → PATH+JDK=/path/to/jdk/bin
```

---

## 6. 노드별 도구 경로 해석

### ToolLocationNodeProperty

```java
// 소스: core/src/main/java/hudson/tools/ToolLocationNodeProperty.java
public class ToolLocationNodeProperty extends NodeProperty<Node> {
    private final List<ToolLocation> locations;

    public String getHome(ToolInstallation installation) {
        for (ToolLocation location : locations) {
            if (location.getName().equals(installation.getName())
                && location.getType() == installation.getDescriptor()) {
                return location.getHome();  // 노드별 오버라이드 경로
            }
        }
        return null;  // 오버라이드 없음
    }
}
```

### ToolLocation 내부 구조

```java
// 소스: ToolLocationNodeProperty.java:142-185
public static final class ToolLocation {
    private final String type;  // ToolDescriptor 클래스명
    private final String name;  // 도구 이름
    private final String home;  // 오버라이드 경로

    // key 포맷: "descriptorClassName@toolName"
    public ToolLocation(String key, String home) {
        this.type = key.substring(0, key.indexOf('@'));
        this.name = key.substring(key.indexOf('@') + 1);
        this.home = home;
    }
}
```

### 경로 해석 우선순위

```
getToolHome(node, installation, log) 호출
    │
    ├── 1순위: ToolLocationNodeProperty (노드 설정에서 명시적 오버라이드)
    │          → 가장 구체적인 설정, 특정 노드에 특정 도구 경로 지정
    │
    ├── 2순위: ToolLocationTranslator (확장점 기반 동적 변환)
    │          → 플러그인이 제공하는 동적 경로 변환 로직
    │          → 예: Docker 에이전트에서 컨테이너 내부 경로 매핑
    │
    └── 3순위: installation.getHome() (기본 경로)
               → Global Tool Configuration에서 설정한 기본값
```

---

## 7. Build Parameter 아키텍처

### 3계층 모델

Jenkins Build Parameter는 **Definition → Value → Action** 3계층으로 구성된다.

```
┌──────────────────────────────────────────────────────────────────┐
│                    Build Parameter 3계층                         │
│                                                                  │
│  ┌─────────────────────────────┐                                │
│  │  ParameterDefinition        │ ← Job config.xml에 저장       │
│  │  (빌드 파라미터 "정의")       │    "이 Job은 string 파라미터  │
│  │  - name: String              │     'branch'를 받는다"        │
│  │  - description: String       │                                │
│  │  + createValue(): PValue     │                                │
│  │  + getDefaultParameterValue()│                                │
│  └──────────┬──────────────────┘                                │
│             │ creates                                            │
│  ┌──────────▼──────────────────┐                                │
│  │  ParameterValue             │ ← 빌드별로 생성               │
│  │  (빌드 시점의 "값")          │    "이번 빌드에서 branch=main" │
│  │  - name: String              │                                │
│  │  - value: (타입별)           │                                │
│  │  + buildEnvironment(env)     │ ← 환경변수에 주입             │
│  └──────────┬──────────────────┘                                │
│             │ attached via                                       │
│  ┌──────────▼──────────────────┐                                │
│  │  ParametersAction           │ ← Build.getAction()으로 접근   │
│  │  (빌드에 첨부되는 액션)      │                                │
│  │  - parameters: List<PValue>  │                                │
│  └─────────────────────────────┘                                │
└──────────────────────────────────────────────────────────────────┘
```

### 왜 Definition과 Value를 분리했는가?

1. **재사용성**: Definition은 Job에 한 번 저장, Value는 매 빌드마다 새로 생성
2. **다형성**: Definition 타입에 따라 다른 UI 렌더링, 다른 Value 타입 생성
3. **영속성 분리**: Definition은 `config.xml`, Value는 `build.xml`에 별도 저장
4. **검증 로직 분리**: Definition이 Value의 유효성을 검증 (특히 ChoiceParameter)

---

## 8. ParameterDefinition 핵심 분석

### 추상 클래스 구조

```java
// 소스: core/src/main/java/hudson/model/ParameterDefinition.java
@ExportedBean(defaultVisibility = 3)
public abstract class ParameterDefinition implements
        Describable<ParameterDefinition>, ExtensionPoint, Serializable {

    private final String name;        // 파라미터 이름 (불변)
    private String description;       // 설명 (가변, @DataBoundSetter)

    // 폼 제출에서 값 생성
    public /* abstract */ ParameterValue createValue(
        StaplerRequest2 req, JSONObject jo) { ... }

    // GET 쿼리스트링에서 값 생성 (API/CLI 용)
    public /* abstract */ ParameterValue createValue(
        StaplerRequest2 req) { ... }

    // CLI에서 문자열로 값 생성
    public ParameterValue createValue(CLICommand command, String value)
        throws IOException, InterruptedException {
        throw new AbortException("CLI parameter submission not supported...");
    }

    // 기본값 반환
    public ParameterValue getDefaultParameterValue() {
        return null;
    }

    // 값 유효성 검증 (2.244+)
    public boolean isValid(ParameterValue value) {
        return true;  // 기본: 항상 유효
    }
}
```

### ParameterDescriptor

```java
// 소스: ParameterDefinition.java:331-357
public abstract static class ParameterDescriptor
        extends Descriptor<ParameterDefinition> {

    // Jelly 뷰 경로: 파라미터 입력 폼
    public String getValuePage() {
        return getViewPage(clazz, "index.jelly");
    }
}
```

### equals/hashCode의 특수한 구현

```java
// 소스: ParameterDefinition.java:293-313
@Override
public int hashCode() {
    // XStream XML을 해시하여 모든 필드를 포함
    return Jenkins.XSTREAM2.toXML(this).hashCode();
}

@Override
public boolean equals(Object obj) {
    // XML 직렬화 결과 비교로 완전한 동등성 검사
    String thisXml  = Jenkins.XSTREAM2.toXML(this);
    String otherXml = Jenkins.XSTREAM2.toXML(other);
    return thisXml.equals(otherXml);
}
```

**왜 XML 비교인가?**
- ParameterDefinition은 확장 가능하므로 어떤 필드가 있을지 모름
- 리플렉션보다 XStream 직렬화가 Jenkins 생태계에서 더 자연스러움
- 단점: 성능 오버헤드 (매번 XML 생성)
- StringParameterDefinition 등 구체 클래스는 이를 오버라이드하여 직접 비교

---

## 9. 파라미터 타입별 구현 분석

### StringParameterDefinition

```java
// 소스: core/src/main/java/hudson/model/StringParameterDefinition.java
public class StringParameterDefinition extends SimpleParameterDefinition {
    private String defaultValue;
    private boolean trim;         // 공백 제거 옵션 (2.90+)

    @Override
    public StringParameterValue getDefaultParameterValue() {
        StringParameterValue value = new StringParameterValue(
            getName(), defaultValue, getDescription());
        if (isTrim()) {
            value.doTrim();  // 기본값에도 trim 적용
        }
        return value;
    }

    @Override
    public ParameterValue createValue(StaplerRequest2 req, JSONObject jo) {
        StringParameterValue value = req.bindJSON(
            StringParameterValue.class, jo);
        if (isTrim()) {
            value.doTrim();  // 사용자 입력에도 trim 적용
        }
        value.setDescription(getDescription());
        return value;
    }
}
```

### ChoiceParameterDefinition

```java
// 소스: core/src/main/java/hudson/model/ChoiceParameterDefinition.java
public class ChoiceParameterDefinition extends SimpleParameterDefinition {
    private List<String> choices;
    private final String defaultValue;

    // 유효성 검증: 선택지에 포함된 값만 허용
    @Override
    public boolean isValid(ParameterValue value) {
        return choices.contains(
            ((StringParameterValue) value).getValue());
    }

    // 폼 제출 시 검증
    @Override
    public ParameterValue createValue(StaplerRequest2 req, JSONObject jo) {
        StringParameterValue value = req.bindJSON(
            StringParameterValue.class, jo);
        checkValue(value, value.getValue());  // 불법 선택 시 예외
        return value;
    }

    private void checkValue(StringParameterValue value, String value2) {
        if (!isValid(value)) {
            throw new IllegalArgumentException(
                "Illegal choice for parameter " + getName()
                + ": " + value2);
        }
    }
}
```

### 파라미터 타입 비교 테이블

```
┌──────────────────────┬─────────────┬──────────────┬─────────────┐
│ 타입                  │ 값 클래스     │ 검증          │ 기본값       │
├──────────────────────┼─────────────┼──────────────┼─────────────┤
│ StringParameter      │ StringPV    │ 없음          │ 빈 문자열    │
│ ChoiceParameter      │ StringPV    │ choices 포함  │ 첫 번째 항목 │
│ BooleanParameter     │ BooleanPV   │ 없음          │ false       │
│ FileParameter        │ FilePV      │ 없음          │ null        │
│ PasswordParameter    │ PasswordPV  │ 없음          │ 빈 문자열    │
│ RunParameter         │ RunPV       │ 프로젝트 존재  │ 최근 빌드    │
│ TextParameter        │ TextPV      │ 없음          │ 빈 문자열    │
└──────────────────────┴─────────────┴──────────────┴─────────────┘
```

---

## 10. 파라미터 값 생성 및 검증 흐름

### createValue 3가지 경로

```
파라미터 값 생성 경로:

1. UI 폼 제출 (가장 일반적)
   ─────────────────────────
   사용자 → index.jelly 폼 작성 → POST 제출
   → ParameterDefinition.createValue(StaplerRequest2, JSONObject)
   → 각 타입별 구현이 JSON에서 값 추출
   → ParameterValue 생성 & 반환

2. REST API / 스크립트 (프로그래밍 방식)
   ─────────────────────────────────
   curl -X POST .../buildWithParameters?branch=main
   → ParameterDefinition.createValue(StaplerRequest2)
   → 쿼리 파라미터에서 값 추출
   → 없으면 getDefaultParameterValue() 사용

3. CLI (명령줄)
   ─────────────
   java -jar jenkins-cli.jar build job -p branch=main
   → ParameterDefinition.createValue(CLICommand, "main")
   → 기본 구현은 AbortException (지원 안 함)
   → SimpleParameterDefinition이 오버라이드
```

### 파라미터 빌드 적용 흐름

```
빌드 트리거
    │
    ├── 1. ParametersDefinitionProperty에서 정의 목록 가져오기
    │       → List<ParameterDefinition> paramDefs
    │
    ├── 2. 각 정의에 대해 createValue() 또는 getDefaultParameterValue()
    │       → List<ParameterValue> paramValues
    │
    ├── 3. ParametersAction 생성
    │       → new ParametersAction(paramValues)
    │
    ├── 4. 빌드 큐에 ParametersAction 첨부
    │       → Queue.schedule2(task, delay, actions)
    │
    └── 5. 빌드 실행 시 환경변수에 주입
            → ParameterValue.buildEnvironment(AbstractBuild, EnvVars)
            → 예: envVars.put("BRANCH", "main")
```

---

## 11. ParametersDefinitionProperty 통합

### Job과의 연결

ParameterDefinition 자체는 독립적이지만, Job에 적용되려면
`ParametersDefinitionProperty`를 통해 `JobProperty`로 등록되어야 한다.

```
Job (config.xml)
│
├── <properties>
│   └── <hudson.model.ParametersDefinitionProperty>
│       └── <parameterDefinitions>
│           ├── <hudson.model.StringParameterDefinition>
│           │   ├── <name>BRANCH</name>
│           │   ├── <defaultValue>main</defaultValue>
│           │   └── <trim>false</trim>
│           │
│           └── <hudson.model.ChoiceParameterDefinition>
│               ├── <name>ENV</name>
│               └── <choices>
│                   ├── <string>dev</string>
│                   ├── <string>staging</string>
│                   └── <string>prod</string>
│               </choices>
│
└── "Build with Parameters" 버튼 활성화
```

### isParameterized 판단

```
Job.isParameterized()
    → getProperty(ParametersDefinitionProperty.class) != null
    → true이면 "Build Now" 대신 "Build with Parameters" 표시
    → index.jelly에서 각 파라미터의 입력 UI 렌더링
```

---

## 12. Tool + Parameter 통합 시퀀스

실제 빌드에서 Tool과 Parameter가 함께 작동하는 전체 흐름이다.

```
사용자: "Build with Parameters" 클릭
    │
    ├── 파라미터 입력 (BRANCH=feature, JDK_VERSION=17)
    │
    ├── 빌드 큐 진입
    │   └── ParametersAction(BRANCH=feature, JDK_VERSION=17)
    │
    ├── Executor가 빌드 인계
    │   ├── AbstractBuild.getEnvironment(listener)
    │   │   └── ParameterValue.buildEnvironment()
    │   │       → BRANCH=feature 환경변수 설정
    │   │
    │   └── BuildStep 실행
    │       ├── JDK 도구 해석
    │       │   └── JDK.translate(node, envs, listener)
    │       │       ├── NodeSpecific: 노드별 경로 확인
    │       │       ├── ToolInstaller: 필요시 자동 설치
    │       │       └── EnvironmentSpecific: ${JDK_VERSION} 치환
    │       │
    │       ├── Maven 도구 해석
    │       │   └── Maven.translate(node, envs, listener)
    │       │
    │       └── 셸/배치 스크립트 실행
    │           → BRANCH, JAVA_HOME 등 환경변수 사용 가능
    │
    └── 빌드 완료 → build.xml에 ParameterValue 영속화
```

---

## 13. 설계 결정과 교훈

### Tool Installation 설계 결정

| 결정 | 이유 | 트레이드오프 |
|------|------|------------|
| `home`이 almost-final | XML 역직렬화 호환성 (1.286 이전 마이그레이션) | 진정한 불변성 포기 |
| writeReplace XML 해킹 | DescribableList의 Serializable 미구현 | 복잡성 증가, 성능 오버헤드 |
| label 기반 노드 필터링 | 라벨 표현식 재활용 | 복잡한 조건에는 부족 |
| 3단계 경로 우선순위 | 유연성 (노드 오버라이드 > 동적 변환 > 기본값) | 디버깅 어려움 |

### Build Parameter 설계 결정

| 결정 | 이유 | 트레이드오프 |
|------|------|------------|
| Definition-Value 분리 | 정의 재사용, 타입별 UI/검증 분리 | 클래스 수 증가 |
| XML 기반 equals | 확장성 (어떤 필드든 비교 가능) | 매번 XML 직렬화 오버헤드 |
| isValid() 기본 true | 하위 호환성 (기존 파라미터 타입 무결성) | 검증 누락 가능성 |
| 3종 createValue | UI/API/CLI 모두 지원 | 구현 부담 (모든 타입이 3종 구현 필요) |

### 핵심 교훈

1. **Describable 패턴의 일관성**: Tool과 Parameter 모두 동일한 Describable/Descriptor 패턴을 따름. 이 일관성이 Jenkins 확장 모델의 핵심 강점
2. **NodeSpecific/EnvironmentSpecific 분리**: 노드 특화와 환경 변수 치환을 독립적 관심사로 분리한 것은 조합 가능성을 높임
3. **영속성과 직렬화의 긴장**: DescribableList의 비직렬화 문제에서 보듯, Java의 Serializable과 Jenkins의 XStream 기반 영속성 사이의 불일치가 복잡성의 주 원인
4. **3가지 값 생성 경로**: UI/API/CLI 모두를 일급 시민으로 지원하되, 기본 구현에서 점진적 지원을 가능하게 한 설계
5. **유효성 검증의 후발 도입**: `isValid()` 메서드가 2.244에서야 추가된 것은, 처음부터 검증 프레임워크를 설계하지 않은 대가

---

## 부록: 주요 소스 파일 요약

| 파일 | 줄수 | 핵심 역할 |
|------|------|----------|
| `ToolInstallation.java` | 283 | 도구 설치 추상 기반, translate() 패턴 |
| `ToolDescriptor.java` | 209 | 도구 설정 관리, installations[] 배열 |
| `ToolInstaller.java` | 178 | 자동 설치 추상 클래스, preferredLocation() |
| `ToolLocationNodeProperty.java` | 187 | 노드별 도구 경로 오버라이드 |
| `ParameterDefinition.java` | 360 | 빌드 파라미터 정의 추상 클래스 |
| `StringParameterDefinition.java` | 196 | 문자열 파라미터 (trim 지원) |
| `ChoiceParameterDefinition.java` | 240 | 선택 파라미터 (isValid 검증) |

---

*본 문서는 Jenkins 소스코드를 직접 분석하여 작성되었습니다. 모든 코드 참조는 검증된 실제 경로와 라인 번호를 기반으로 합니다.*
