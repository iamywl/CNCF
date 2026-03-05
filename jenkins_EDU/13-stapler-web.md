# 13. Stapler 웹 프레임워크와 Jenkins 통합

## 개요

Stapler는 Jenkins의 핵심 웹 프레임워크로, URL 경로를 Java 객체 트리에 직접 매핑하는 독특한 방식을 사용한다. 전통적인 웹 프레임워크가 URL을 컨트롤러 메서드에 매핑하는 반면, Stapler는 URL의 각 세그먼트를 getter 메서드 호출로 변환하여 객체 그래프를 탐색한다. 이 설계 덕분에 Jenkins의 복잡한 계층 구조(Jenkins -> Job -> Build -> Action)를 자연스러운 URL 체계로 노출할 수 있다.

이 문서에서는 Jenkins 소스코드를 직접 분석하여 Stapler의 URL 라우팅, REST API, 뷰 시스템, 보안 통합의 실제 구현을 심층적으로 다룬다.

---

## 1. Stapler URL-to-Object 매핑 원리

### 1.1 핵심 개념

Stapler의 URL 라우팅은 다음과 같은 원리로 동작한다:

```
URL: /job/my-project/42/console

매핑 과정:
  / → Jenkins (루트 객체)
  /job/my-project → Jenkins.getItem("my-project") → AbstractProject
  /42 → Job.getBuild("42") 또는 getBuildByNumber(42) → Run
  /console → Run의 console.jelly 뷰 렌더링
```

URL의 각 세그먼트(`/`로 구분된 부분)를 현재 객체의 getter 메서드로 해석하여, Java 객체 트리를 따라 내려간다. 마지막 세그먼트에 도달하면 해당 객체의 뷰(Jelly/Groovy)를 렌더링하거나 `do{Action}` 메서드를 호출한다.

### 1.2 getter 메서드 컨벤션

Stapler가 URL 세그먼트를 해석할 때 시도하는 메서드 순서:

| 우선순위 | 메서드 패턴 | 예시 URL 세그먼트 | 메서드 호출 |
|---------|-----------|-----------------|-----------|
| 1 | `get{Name}()` | `/pluginManager` | `getPluginManager()` |
| 2 | `get{Name}(String)` | `/job/my-project` | `getItem("my-project")` 또는 `getJob("my-project")` |
| 3 | `get{Name}(int)` | `/42` | `getBuildByNumber(42)` |
| 4 | `getDynamic(String, ...)` | 기타 동적 경로 | `getDynamic(token, req, rsp)` |
| 5 | `do{Action}(...)` | `/build` | `doBuild(req, rsp)` |
| 6 | 뷰 파일 | `/configure` | `configure.jelly` 렌더링 |
| 7 | public 필드 | `/bean` | `this.bean` 접근 |

### 1.3 Jenkins 루트 객체의 URL 매핑

`Jenkins` 클래스는 Stapler의 루트 객체로, 모든 URL 라우팅의 시작점이다.

소스 파일: `core/src/main/java/jenkins/model/Jenkins.java`

```java
@ExportedBean
public class Jenkins extends AbstractCIBase
    implements DirectlyModifiableTopLevelItemGroup, StaplerProxy, StaplerFallback,
               ModifiableViewGroup, AccessControlled, DescriptorByNameOwner,
               ModelObjectWithContextMenu, ModelObjectWithChildren, OnMaster, Loadable {
```

Jenkins가 구현하는 주요 Stapler 관련 인터페이스:
- **`StaplerProxy`**: 요청 처리 전 권한 확인을 위한 프록시 패턴
- **`StaplerFallback`**: URL 매핑 실패 시 대체 객체 반환
- **`ModelObjectWithContextMenu`**: 컨텍스트 메뉴 지원
- **`DescriptorByNameOwner`**: Descriptor 이름으로 접근 지원

Jenkins 루트에서 접근 가능한 주요 URL 경로:

```
/                        → Jenkins (루트, StaplerFallback으로 primaryView)
/job/{name}              → Jenkins.getItem(name) → TopLevelItem
/computer                → Jenkins.getComputer() → ComputerSet
/computer/{name}         → Jenkins.getComputer(name) → Computer
/view/{name}             → Jenkins.getView(name) → View
/pluginManager           → Jenkins.getPluginManager() → PluginManager
/securityRealm           → Jenkins.getSecurityRealm() → SecurityRealm
/manage                  → manage.jelly 뷰 렌더링
/api                     → Jenkins.getApi() → Api
```

### 1.4 객체 트리 탐색 실제 코드

Jenkins에서 아이템(Job)을 조회하는 코드:

```java
// core/src/main/java/jenkins/model/Jenkins.java (라인 3021)
@Override public TopLevelItem getItem(String name) throws AccessDeniedException {
    if (name == null)    return null;
    TopLevelItem item = items.get(name);
    if (item == null)
        return null;
    if (!item.hasPermission(Item.READ)) {
        if (item.hasPermission(Item.DISCOVER)) {
            throw new AccessDeniedException("Please login to access job " + name);
        }
        return null;
    }
    return item;
}
```

이 메서드는 URL `/job/{name}`에서 `{name}` 부분을 받아 해당 아이템을 반환한다. 주목할 점은 **권한 확인이 getter 내부에서 수행**된다는 것이다. `Item.READ` 권한이 없으면 `null`을 반환하고, `Item.DISCOVER` 권한만 있으면 로그인 페이지로 유도하기 위해 `AccessDeniedException`을 던진다.

Job에서 빌드를 조회하는 코드:

```java
// core/src/main/java/hudson/model/Job.java (라인 822)
public RunT getBuild(String id) {
    for (RunT r : _getRuns().values()) {
        if (r.getId().equals(id))
            return r;
    }
    return null;
}
```

### 1.5 getDynamic: 동적 URL 라우팅

정적 getter로 해결되지 않는 URL 세그먼트는 `getDynamic()` 메서드로 처리된다. Jenkins의 `DescriptorImpl` 내부에서 이를 활용하는 예:

```java
// core/src/main/java/jenkins/model/Jenkins.java (라인 2332)
// Jenkins.DescriptorImpl 내부
public Object getDynamic(String token) {
    return Jenkins.get().getDescriptor(token);
}
```

이 패턴을 통해 `/descriptor/{FQCN}/xxx` 형태의 URL이 해당 Descriptor 객체로 라우팅된다. 예를 들어 `/descriptor/hudson.tasks.Shell/help`는 Shell Descriptor의 도움말 페이지를 반환한다.

---

## 2. StaplerProxy 인터페이스

### 2.1 개념과 목적

`StaplerProxy`는 Stapler의 URL 라우팅 과정에서 **프록시 패턴**을 적용하기 위한 인터페이스다. `getTarget()` 메서드를 통해 실제 요청을 처리할 객체를 반환하되, 그 전에 권한 확인이나 리다이렉트 등의 전처리를 수행할 수 있다.

```
요청 수신 → StaplerProxy.getTarget() 호출 → 반환된 객체로 URL 라우팅 계속
```

### 2.2 Jenkins의 getTarget() 구현

```java
// core/src/main/java/jenkins/model/Jenkins.java (라인 5217)
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

이 구현의 핵심 로직:

1. `Jenkins.READ` 권한을 확인한다
2. 권한이 없으면 `AccessDeniedException`이 발생한다
3. 그러나 요청 경로가 "의무 읽기 권한 확인 대상이 아닌" 경우(`isSubjectToMandatoryReadPermissionCheck()`가 `false`), 예외를 삼키고 `this`를 반환하여 접근을 허용한다
4. 권한이 있으면 `this`를 반환하여 정상 라우팅을 계속한다

### 2.3 의무 읽기 권한 확인 면제 경로

인증 없이도 접근 가능해야 하는 경로가 있다. 예를 들어 로그인 페이지는 인증 전에 접근 가능해야 한다.

```java
// core/src/main/java/jenkins/model/Jenkins.java (라인 5876)
private static final Set<String> ALWAYS_READABLE_PATHS = new HashSet<>(Arrays.asList(
    "404",                    // Web method
    "_404",                   // .jelly
    "_404_simple",            // .jelly
    "login",                  // .jelly
    "loginError",             // .jelly
    "logout",                 // #doLogout
    "accessDenied",           // .jelly
    "adjuncts",               // #getAdjuncts
    "error",                  // AbstractModelObject/error.jelly
    "oops",                   // .jelly
    "signup",                 // #doSignup
    "tcpSlaveAgentListener",  // #getTcpSlaveAgentListener
    "federatedLoginService",  // #getFederatedLoginService
    "securityRealm"           // #getSecurityRealm
));
```

`isSubjectToMandatoryReadPermissionCheck()` 메서드는 이 목록과 `UnprotectedRootAction`으로 등록된 경로를 확인한다:

```java
// core/src/main/java/jenkins/model/Jenkins.java (라인 5236)
public boolean isSubjectToMandatoryReadPermissionCheck(String restOfPath) {
    for (String name : ALWAYS_READABLE_PATHS) {
        if (restOfPath.startsWith("/" + name + "/") || restOfPath.equals("/" + name)) {
            return false;
        }
    }

    for (String name : getUnprotectedRootActions()) {
        if (restOfPath.startsWith("/" + name + "/") || restOfPath.equals("/" + name)) {
            return false;
        }
    }

    // 에이전트 JNLP 경로 특수 처리
    if ((isAgentJnlpPath(restOfPath, "jenkins") || isAgentJnlpPath(restOfPath, "slave"))
        && "true".equals(Stapler.getCurrentRequest2().getParameter("encrypt"))) {
        return false;
    }

    return true;
}
```

### 2.4 AbstractItem의 getTarget()

개별 아이템(Job, Folder 등)도 `StaplerProxy`를 구현한다:

```java
// core/src/main/java/hudson/model/AbstractItem.java (라인 990)
@Override
@Restricted(NoExternalUse.class)
public Object getTarget() {
    if (!SKIP_PERMISSION_CHECK) {
        if (!hasPermission(Item.DISCOVER)) {
            return null;
        }
        checkPermission(Item.READ);
    }
    return this;
}
```

`Item.DISCOVER` 권한이 없으면 `null`을 반환하여 404 응답을 생성하고, `Item.READ` 권한이 없으면 예외를 던져 403 응답을 생성한다. 이를 통해 권한이 없는 사용자에게는 해당 아이템의 존재 자체가 노출되지 않는다.

### 2.5 StaplerProxy 흐름 다이어그램

```
클라이언트 요청: GET /job/secret-project/configure

  ┌──────────────────────────────────────────────────────┐
  │ 1. Stapler가 URL "/" 에서 Jenkins 루트 객체 탐색      │
  │                                                      │
  │ 2. Jenkins.getTarget() 호출                          │
  │    ├─ checkPermission(READ) → 성공                    │
  │    └─ return this (Jenkins 객체)                      │
  │                                                      │
  │ 3. "/job/secret-project" → Jenkins.getItem(...)      │
  │    ├─ items.get("secret-project") → AbstractProject  │
  │    └─ hasPermission(Item.READ) → 성공                 │
  │                                                      │
  │ 4. AbstractItem.getTarget() 호출                     │
  │    ├─ hasPermission(Item.DISCOVER) → true            │
  │    ├─ checkPermission(Item.READ) → 성공               │
  │    └─ return this (AbstractProject 객체)              │
  │                                                      │
  │ 5. "/configure" → configure.jelly 뷰 렌더링           │
  │    └─ permission="${it.EXTENDED_READ}" 확인            │
  └──────────────────────────────────────────────────────┘
```

---

## 3. StaplerFallback 인터페이스

### 3.1 개념과 목적

`StaplerFallback`은 Stapler가 URL 세그먼트를 현재 객체에서 해석하지 못했을 때, 대체 객체를 반환하여 라우팅을 계속할 수 있게 하는 인터페이스다.

### 3.2 Jenkins의 getStaplerFallback() 구현

```java
// core/src/main/java/jenkins/model/Jenkins.java (라인 5287)
@Override
public View getStaplerFallback() {
    return getPrimaryView();
}
```

이 구현은 매우 간결하지만 중요한 의미를 가진다:

- 사용자가 Jenkins 루트 URL(`/`)에 접근하면, Stapler는 `Jenkins` 객체에서 `index.jelly` 같은 뷰를 찾는다
- 매칭되지 않으면 `getStaplerFallback()`을 호출하여 **기본 뷰(Primary View)** 객체를 반환한다
- 결과적으로 Jenkins 대시보드에 접근하면 "All" 뷰(또는 사용자가 설정한 기본 뷰)가 표시된다

### 3.3 Fallback 동작 순서

```
URL: / (Jenkins 루트)

1. Jenkins 객체에서 index.jelly 탐색 → 없음
2. getStaplerFallback() 호출 → getPrimaryView() → View("All")
3. View 객체의 index.jelly 렌더링 → 대시보드 표시
```

이 패턴이 없었다면 Jenkins 루트 URL에 별도의 `index.jelly`를 작성해야 했을 것이다. 대신 View 객체에 라우팅을 위임함으로써, 뷰 전환이 자연스럽게 동작한다.

---

## 4. REST API (hudson.model.Api)

### 4.1 Api 클래스 구조

소스 파일: `core/src/main/java/hudson/model/Api.java`

```java
// core/src/main/java/hudson/model/Api.java (라인 81)
public class Api extends AbstractModelObject {
    /**
     * Model object to be exposed as XML/JSON/etc.
     */
    public final Object bean;

    public Api(Object bean) {
        this.bean = bean;
    }
}
```

`Api` 클래스는 `AbstractModelObject`을 상속하며, 직렬화 대상 객체(`bean`)를 래핑한다. 모든 모델 객체는 `getApi()` 메서드를 통해 자신의 REST API를 노출한다.

### 4.2 API 접근 URL 체계

Jenkins의 모든 모델 객체는 자신의 URL 뒤에 `/api`를 붙여 REST API에 접근할 수 있다:

| URL | 설명 |
|-----|------|
| `/api/xml` | XML 형식 |
| `/api/json` | JSON 형식 |
| `/api/python` | Python 리터럴 형식 |
| `/api/schema` | XML 스키마 |
| `/job/{name}/api/json` | 특정 Job의 JSON API |
| `/job/{name}/{number}/api/json` | 특정 빌드의 JSON API |

### 4.3 Jenkins 루트의 getApi()

```java
// core/src/main/java/jenkins/model/Jenkins.java (라인 1368)
public Api getApi() {
    /* Do not show "REST API" link in footer when on 404 error page */
    final StaplerRequest2 req = Stapler.getCurrentRequest2();
    if (req != null) {
        final Object attribute = req.getAttribute("jakarta.servlet.error.message");
        if (attribute != null) {
            return null;
        }
    }
    return new Api(this);
}
```

404 에러 페이지에서는 REST API 링크를 표시하지 않기 위해 `null`을 반환한다.

### 4.4 XML API (doXml)

```java
// core/src/main/java/hudson/model/Api.java (라인 104)
public void doXml(StaplerRequest2 req, StaplerResponse2 rsp,
                  @QueryParameter String xpath,
                  @QueryParameter String wrapper,
                  @QueryParameter String tree,
                  @QueryParameter int depth) throws IOException, ServletException {
    setHeaders(rsp);

    String[] excludes = req.getParameterValues("exclude");

    if (xpath == null && excludes == null) {
        // serve the whole thing
        rsp.serveExposedBean(req, bean, Flavor.XML);
        return;
    }

    StringWriter sw = new StringWriter();

    // first write to String
    Model p = MODEL_BUILDER.get(bean.getClass());
    TreePruner pruner = tree != null
        ? new NamedPathPruner(tree)
        : new ByDepth(1 - depth);
    p.writeTo(bean, pruner, Flavor.XML.createDataWriter(bean, sw));

    // apply XPath
    FilteredFunctionContext functionContext = new FilteredFunctionContext();
    Object result;
    Document dom = new SAXReader().read(new StringReader(sw.toString()));

    // apply exclusions
    if (excludes != null) {
        for (String exclude : excludes) {
            XPath xExclude = dom.createXPath(exclude);
            xExclude.setFunctionContext(functionContext);
            List<org.dom4j.Node> list = xExclude.selectNodes(dom);
            for (org.dom4j.Node n : list) {
                Element parent = n.getParent();
                if (parent != null)
                    parent.remove(n);
            }
        }
    }
    // ... xpath 처리 및 wrapper 처리 ...
}
```

XML API 처리 흐름:

```
요청: GET /api/xml?tree=jobs[name]&xpath=//job&wrapper=jobs

1. setHeaders(rsp)  → X-Jenkins, X-Content-Type-Options 등 보안 헤더 설정
2. tree 파라미터 → NamedPathPruner로 응답 필터링
3. ModelBuilder로 bean을 XML로 직렬화
4. exclude 파라미터 → XPath로 노드 제거
5. xpath 파라미터 → XPath로 결과 추출
6. wrapper 파라미터 → 여러 결과를 래핑 엘리먼트로 감싸기
```

### 4.5 JSON API (doJson)

```java
// core/src/main/java/hudson/model/Api.java (라인 256)
private void doJsonImpl(StaplerRequest2 req, StaplerResponse2 rsp)
        throws IOException, ServletException {
    if (req.getParameter("jsonp") == null || permit(req)) {
        setHeaders(rsp);
        rsp.serveExposedBean(req, bean,
            req.getParameter("jsonp") == null ? Flavor.JSON : Flavor.JSONP);
    } else {
        rsp.sendError(HttpURLConnection.HTTP_FORBIDDEN,
            "jsonp forbidden; implement jenkins.security.SecureRequester");
    }
}
```

JSONP는 기본적으로 금지되며, `SecureRequester`를 구현한 경우에만 허용된다. 이는 CSRF 공격을 방지하기 위한 보안 조치다.

### 4.6 @Exported / @ExportedBean 어노테이션

REST API에 노출할 데이터를 결정하는 것은 `@Exported`와 `@ExportedBean` 어노테이션이다.

```java
// core/src/main/java/jenkins/model/Jenkins.java (라인 354)
@ExportedBean
public class Jenkins extends AbstractCIBase ...
```

```java
// core/src/main/java/hudson/model/Job.java 에서 실제 사용 예
@Exported
public boolean isInQueue() {
    return false;
}

@Exported
public Queue.Item getQueueItem() {
    return null;
}

@Exported
public boolean isKeepDependencies() {
    return keepDependencies;
}

@Exported
public int getNextBuildNumber() {
    return nextBuildNumber;
}

@Exported(name = "property", inline = true)
public List<JobProperty<? super JobT>> getAllProperties() {
    return properties.getView();
}

@Exported(name = "allBuilds", visibility = -2)
@WithBridgeMethods(List.class)
public RunList<RunT> getBuilds() {
    return RunList.fromRuns(_getRuns().values());
}

@Exported(name = "builds")
public RunList<RunT> getNewBuilds() {
    return getBuilds().limit(100);
}
```

`@Exported` 어노테이션의 주요 속성:

| 속성 | 설명 | 예시 |
|------|------|------|
| `name` | JSON/XML에서 사용할 이름 | `@Exported(name = "builds")` |
| `visibility` | depth 기반 노출 제어 (기본 1) | `@Exported(visibility = -2)` |
| `inline` | 중첩 객체를 인라인으로 펼침 | `@Exported(inline = true)` |

`visibility = -2`로 설정된 `allBuilds`는 기본 depth에서는 노출되지 않고, `tree` 파라미터로 명시적으로 요청해야만 반환된다. 이는 성능 최적화를 위한 것으로, 모든 빌드를 로드하는 것은 비용이 크기 때문이다.

### 4.7 tree 파라미터를 이용한 응답 필터링

`tree` 파라미터는 `NamedPathPruner`에 의해 처리되며, 필요한 필드만 선택적으로 가져올 수 있다:

```
# 모든 Job의 이름만 가져오기
GET /api/json?tree=jobs[name]

# Job의 이름과 최근 빌드 정보
GET /api/json?tree=jobs[name,lastBuild[number,result]]

# 범위 지정: 처음 10개 Job
GET /api/json?tree=jobs[name]{0,10}
```

### 4.8 보안 헤더 설정

모든 API 응답에는 보안 헤더가 추가된다:

```java
// core/src/main/java/hudson/model/Api.java (라인 308)
@Restricted(NoExternalUse.class)
protected void setHeaders(StaplerResponse2 rsp) {
    rsp.setHeader("X-Jenkins", Jenkins.VERSION);
    rsp.setHeader("X-Jenkins-Session", Jenkins.SESSION_HASH);
    rsp.setHeader("X-Content-Type-Options", "nosniff");
    rsp.setHeader("X-Frame-Options", "deny");
}
```

| 헤더 | 목적 |
|------|------|
| `X-Jenkins` | Jenkins 버전 정보 |
| `X-Jenkins-Session` | 현재 세션 해시 (재시작 감지) |
| `X-Content-Type-Options: nosniff` | MIME 타입 스니핑 방지 |
| `X-Frame-Options: deny` | iframe 임베딩 방지 (클릭재킹 방어) |

---

## 5. HTTP 메서드 컨벤션

### 5.1 do{Action} 핸들러

Stapler는 `do{Action}` 이름 패턴의 메서드를 HTTP 요청 핸들러로 인식한다. 이 메서드들은 주로 POST 요청을 처리하며, 부수 효과(side effect)가 있는 작업을 수행한다.

```java
// core/src/main/java/hudson/model/AbstractProject.java (라인 775)
@Override
@POST
public void doConfigSubmit(StaplerRequest2 req, StaplerResponse2 rsp)
        throws IOException, ServletException, FormException {
    super.doConfigSubmit(req, rsp);
    updateTransientActions();
    Jenkins.get().getQueue().scheduleMaintenance();
    Jenkins.get().rebuildDependencyGraphAsync();
}
```

```java
// core/src/main/java/jenkins/model/Jenkins.java (라인 4032)
@POST
public synchronized void doConfigSubmit(StaplerRequest2 req, StaplerResponse2 rsp)
        throws IOException, ServletException, FormException {
    try (BulkChange bc = new BulkChange(this)) {
        checkPermission(MANAGE);
        JSONObject json = req.getSubmittedForm();
        // ... 설정 처리 ...
    }
}
```

Jenkins의 루트 설정 페이지 제출(`/configSubmit`)은 `@POST` 어노테이션으로 POST 요청만 허용하며, `MANAGE` 권한을 확인한다.

### 5.2 doCheck{Field}: AJAX 폼 검증

`Descriptor` 클래스에서 폼 필드의 유효성 검증을 위한 컨벤션:

```java
// core/src/main/java/jenkins/model/Jenkins.java (라인 2327)
// Jenkins.DescriptorImpl 내부
public FormValidation doCheckNumExecutors(@QueryParameter String value) {
    return FormValidation.validateNonNegativeInteger(value);
}
```

Descriptor에서 검증 메서드를 탐색하는 로직:

```java
// core/src/main/java/hudson/model/Descriptor.java (라인 413)
public CheckMethod getCheckMethod(String fieldName) {
    CheckMethod method = checkMethods.get(fieldName);
    if (method == null) {
        method = new CheckMethod(this, fieldName);
        checkMethods.put(fieldName, method);
    }
    return method;
}
```

`doCheck{FieldName}` 패턴의 메서드는 `@QueryParameter`로 값을 받아 `FormValidation` 객체를 반환한다:

```
필드명: numExecutors
→ 검증 URL: /descriptor/{FQCN}/checkNumExecutors?value=...
→ 메서드: doCheckNumExecutors(@QueryParameter String value)
→ 반환: FormValidation.ok() / FormValidation.warning(...) / FormValidation.error(...)
```

### 5.3 doFill{Field}Items: 드롭다운 채우기

드롭다운 목록을 동적으로 채우기 위한 컨벤션:

```java
// core/src/main/java/jenkins/model/Jenkins.java (라인 1839)
@Restricted(NoExternalUse.class)
public ComboBoxModel doFillJobNameItems() {
    return new ComboBoxModel(getJobNames());
}
```

Descriptor에서 이 메서드를 자동으로 탐색하는 로직:

```java
// core/src/main/java/hudson/model/Descriptor.java (라인 428)
public void calcFillSettings(String field, Map<String, Object> attributes) {
    String capitalizedFieldName = field == null || field.isEmpty()
        ? field
        : Character.toTitleCase(field.charAt(0)) + field.substring(1);
    String methodName = "doFill" + capitalizedFieldName + "Items";
    Method method = ReflectionUtils.getPublicMethodNamed(getClass(), methodName);
    if (method == null)
        throw new IllegalStateException(String.format(
            "%s doesn't have the %s method for filling a drop-down list",
            getClass(), methodName));

    // build query parameter line
    List<String> depends = buildFillDependencies(method, new ArrayList<>());

    if (!depends.isEmpty())
        attributes.put("fillDependsOn", String.join(" ", depends));
    attributes.put("fillUrl", String.format(
        "%s/%s/fill%sItems",
        getCurrentDescriptorByNameUrl(), getDescriptorUrl(), capitalizedFieldName));
}
```

이 메서드는 다음과 같이 동작한다:

```
필드명: jobName
→ 메서드명 계산: doFill + JobName + Items = doFillJobNameItems
→ URL 생성: /descriptor/{FQCN}/fillJobNameItems
→ 의존성 필드: fillDependsOn 속성으로 HTML에 주입
→ 결과: ListBoxModel 또는 ComboBoxModel 반환
```

### 5.4 doAutoComplete: 자동 완성

```java
// core/src/main/java/hudson/model/AbstractProject.java (라인 2014)
public AutoCompletionCandidates doAutoCompleteUpstreamProjects(
        @QueryParameter String value) {
    AutoCompletionCandidates candidates = new AutoCompletionCandidates();
    List<TopLevelItem> jobs = Jenkins.get().getItems(
        j -> j instanceof Job && j.getFullName().startsWith(value));
    for (TopLevelItem job : jobs) {
        candidates.add(job.getFullName());
    }
    return candidates;
}
```

### 5.5 @WebMethod 어노테이션

URL과 메서드명이 다를 때 `@WebMethod`으로 매핑을 명시할 수 있다:

```java
// core/src/main/java/jenkins/model/Jenkins.java (라인 4552)
@WebMethod(name = "404")
@Restricted(NoExternalUse.class)
public void generateNotFoundResponse(StaplerRequest2 req, StaplerResponse2 rsp)
        throws ServletException, IOException {
    if (ResourceDomainConfiguration.isResourceRequest(req)) {
        rsp.forward(this, "_404_simple", req);
    } else {
        // ...
    }
}
```

`/404` URL은 Java 메서드명으로 사용할 수 없으므로(`404`는 숫자로 시작) `@WebMethod(name = "404")`로 매핑한다.

---

## 6. Jelly/Groovy 뷰 시스템

### 6.1 뷰 파일 위치 규칙

Stapler는 Java 클래스의 패키지 경로를 기반으로 뷰 파일을 찾는다:

```
Java 클래스: hudson.model.Job
뷰 파일 위치: src/main/resources/hudson/model/Job/

이 디렉토리 내의 파일들:
  index.jelly          → 메인 페이지 (URL: /job/{name}/)
  configure.jelly      → 설정 페이지 (URL: /job/{name}/configure)
  main.jelly           → 메인 영역 (index.jelly에서 include)
  sidepanel.jelly      → 사이드 패널
  _api.jelly           → API 페이지에 추가되는 문서
  buildTimeTrend.jelly → 빌드 시간 트렌드
```

실제 디렉토리 구조 (jenkins 소스코드에서 확인):

```
core/src/main/resources/
├── hudson/model/
│   ├── Job/
│   │   ├── index.jelly
│   │   ├── configure.jelly
│   │   ├── configure-entries.jelly
│   │   ├── main.jelly
│   │   ├── _api.jelly
│   │   ├── buildTimeTrend.jelly
│   │   ├── permalinks.jelly
│   │   └── ...
│   ├── AbstractProject/
│   │   ├── main.jelly
│   │   ├── sidepanel.jelly
│   │   ├── configure-common.jelly
│   │   ├── _api.jelly
│   │   └── ...
│   ├── Api/
│   │   └── index.jelly        → /api/ 접근 시 표시되는 REST API 문서
│   └── Descriptor/
│       └── newInstanceDetail.jelly
```

### 6.2 ${it} 바인딩: 현재 객체 참조

Jelly 뷰에서 `${it}`은 해당 뷰가 바인딩된 Java 객체를 참조한다:

```xml
<!-- core/src/main/resources/hudson/model/Job/index.jelly -->
<l:layout title="${it.displayName}${not empty it.parent.fullDisplayName
    ?' ['+it.parent.fullDisplayName+']':''}">
  <st:include page="sidepanel.jelly" />
  <l:main-panel>
    <div class="jenkins-app-bar">
      <div class="jenkins-app-bar__content jenkins-build-caption">
        <j:set var="lastBuild" value="${it.lastBuild}" />
        <j:if test="${lastBuild != null}">
          <a href="${rootURL + '/' + lastBuild.url}">
            <l:icon src="symbol-status-${lastBuild.iconColor.iconName}"
                     tooltip="${lastBuild.iconColor.description}"/>
          </a>
        </j:if>
        <h1 class="job-index-headline page-headline">
          <l:breakable value="${it.displayName}"/>
        </h1>
      </div>
    </div>
    <!-- ... -->
    <st:include page="main.jelly" />
    <st:include page="permalinks.jelly" />
  </l:main-panel>
</l:layout>
```

여기서 `${it.displayName}`은 `Job.getDisplayName()`을 호출하고, `${it.lastBuild}`는 `Job.getLastBuild()`를 호출한다. Stapler는 이러한 EL 표현식을 Java getter 호출로 자동 변환한다.

### 6.3 configure.jelly: Descriptor 폼

설정 페이지는 `configure.jelly`에서 `<f:form>` 태그를 사용하여 구현된다:

```xml
<!-- core/src/main/resources/hudson/model/Job/configure.jelly -->
<l:layout permission="${it.EXTENDED_READ}"
         title="${%Config(it.displayName)}">
  <j:set var="readOnlyMode" value="${!it.hasPermission(it.CONFIGURE)}" />

  <l:main-panel>
    <f:form method="post" class="jenkins-form"
            action="configSubmit" name="config">
      <l:app-bar title="${%General}" headingLevel="h2">
        <p:config-disableBuild />
      </l:app-bar>

      <j:set var="descriptor" value="${it.descriptor}" />
      <j:set var="instance" value="${it}" />

      <f:entry title="${%Description}"
               help="${app.markupFormatter.helpUrl}">
        <f:textarea name="description" value="${it.description}" />
      </f:entry>

      <f:descriptorList field="properties"
          descriptors="${h.getJobPropertyDescriptors(it)}" />

      <!-- 파생 클래스의 추가 설정 -->
      <st:include page="configure-entries.jelly" />

      <f:saveApplyBar/>
    </f:form>
  </l:main-panel>
</l:layout>
```

주목할 점:
- `permission="${it.EXTENDED_READ}"`: 이 페이지에 접근하려면 최소 `EXTENDED_READ` 권한이 필요
- `readOnlyMode`: `CONFIGURE` 권한이 없으면 읽기 전용 모드
- `action="configSubmit"`: 폼 제출 시 `doConfigSubmit()` 메서드 호출
- `<st:include page="configure-entries.jelly" />`: 하위 클래스에서 추가 설정 항목을 오버라이드

### 6.4 sidepanel.jelly: 사이드 패널 메뉴

```xml
<!-- core/src/main/resources/hudson/model/AbstractProject/sidepanel.jelly -->
<l:side-panel>
  <l:tasks>
    <j:set var="url" value="${h.getNearestAncestorUrl(request2,it)}"/>
    <l:task contextMenu="false" href="${url}/" icon="symbol-details"
            title="${%Status}"/>
    <l:task href="${url}/changes" icon="symbol-changes"
            title="${%Changes}"/>
    <l:task icon="symbol-folder" href="${url}/ws/"
            title="${%Workspace}" permission="${it.WORKSPACE}">
      <l:task confirmationMessage="${%wipe.out.confirm}"
              href="${url}/doWipeOutWorkspace"
              icon="symbol-trash"
              permission="${h.isWipeOutPermissionEnabled()
                  ? it.WIPEOUT : it.BUILD}"
              post="true" requiresConfirmation="true"
              title="${%Wipe Out Workspace}"/>
    </l:task>
    <j:if test="${it.configurable}">
      <p:configurable/>
    </j:if>
    <st:include page="actions.jelly" />
  </l:tasks>
</l:side-panel>
```

사이드 패널의 각 `<l:task>` 태그는:
- `href`: 링크 URL
- `icon`: 아이콘 (Jenkins Symbol 또는 이미지 경로)
- `permission`: 표시 조건 (해당 권한이 있어야 메뉴 표시)
- `post="true"`: 클릭 시 POST 요청 전송
- `requiresConfirmation="true"`: 확인 대화상자 표시

### 6.5 _api.jelly: REST API 문서 확장

각 모델 객체는 `_api.jelly`를 통해 REST API 문서 페이지에 추가 정보를 제공할 수 있다:

```xml
<!-- core/src/main/resources/hudson/model/Job/_api.jelly -->
<st:include page="/hudson/model/AbstractItem/_api.jelly"/>

<h2>Retrieving all builds</h2>
<p>
  To prevent Jenkins from having to load all builds from disk
  when someone accesses the job API, the <code>builds</code>
  tree only contains the 100 newest builds.
  If you really need to get all builds, access the
  <code>allBuilds</code> tree.
</p>

<h2>Perform a build</h2>
<p>
  To programmatically schedule a new build, post to
  <a href="../build?delay=0sec">this URL</a>.
  If the build has parameters, post to
  <a href="../buildWithParameters">this URL</a>.
</p>
```

이 파일은 `/api/` 페이지의 `Api/index.jelly`에서 `<st:include it="${it.bean}" page="_api.jelly" optional="true" />`로 포함된다.

### 6.6 Descriptor의 config.jelly 탐색

Descriptor는 자신의 config 페이지를 클래스 계층을 따라 탐색한다:

```java
// core/src/main/java/hudson/model/Descriptor.java (라인 889)
public String getConfigPage() {
    return getViewPage(clazz, getPossibleViewNames("config"), "config.jelly");
}

// 라인 947
protected List<String> getPossibleViewNames(String baseName) {
    List<String> names = new ArrayList<>();
    for (Facet f : WebApp.get(Jenkins.get().getServletContext()).facets) {
        if (f instanceof JellyCompatibleFacet jcf) {
            for (String ext : jcf.getScriptExtensions())
                names.add(baseName + ext);
        }
    }
    return names;
}
```

`getPossibleViewNames("config")`는 설치된 Facet에 따라 다음과 같은 이름 목록을 생성한다:
- `config.jelly` (기본 Jelly)
- `config.groovy` (Groovy 뷰)

```java
// 라인 926
private String getViewPage(Class<?> clazz, Collection<String> pageNames,
        String defaultValue) {
    while (clazz != Object.class && clazz != null) {
        for (String pageName : pageNames) {
            String name = clazz.getName().replace('.', '/').replace('$', '/')
                + "/" + pageName;
            if (clazz.getClassLoader().getResource(name) != null)
                return '/' + name;
        }
        clazz = clazz.getSuperclass();
    }
    return defaultValue;
}
```

이 메서드는 클래스 계층을 거슬러 올라가면서 config.jelly 파일을 찾는다. 따라서 하위 클래스가 config.jelly를 제공하지 않으면 부모 클래스의 것이 사용된다.

---

## 7. Action의 URL 기여

### 7.1 Action 인터페이스

소스 파일: `core/src/main/java/hudson/model/Action.java`

```java
// core/src/main/java/hudson/model/Action.java (라인 79)
public interface Action extends ModelObject {
    @CheckForNull String getIconFileName();
    @Override @CheckForNull String getDisplayName();
    @CheckForNull String getUrlName();
}
```

Action의 세 가지 핵심 메서드:

| 메서드 | 역할 | null 반환 시 |
|--------|------|-------------|
| `getIconFileName()` | 사이드 패널 아이콘 | 메뉴에서 숨김 |
| `getDisplayName()` | 메뉴에 표시할 텍스트 | 메뉴에서 숨김 |
| `getUrlName()` | URL 경로명 | 웹 바인딩 없음 |

### 7.2 URL 바인딩 규칙

`getUrlName()`이 반환하는 값에 따라 URL이 결정된다:

```java
// Action.java Javadoc에서 (라인 117-141)
/**
 * Gets the URL path name.
 *
 * For example, if this method returns "xyz", and if the parent object
 * (that this action is associated with) is bound to /foo/bar/zot,
 * then this action object will be exposed to /foo/bar/zot/xyz.
 *
 * The returned string can be an absolute URL, like "http://www.sun.com/",
 * which is useful for directly connecting to external systems.
 *
 * If the returned string starts with '/', like '/foo', then it's assumed
 * to be relative to the context path of the Jenkins webapp.
 */
@CheckForNull String getUrlName();
```

예시:
- `return "testReport"` → `/job/my-project/42/testReport`
- `return "http://sonarqube.example.com/project/123"` → 외부 URL로 링크
- `return "/manage"` → Jenkins 컨텍스트 루트 기준 절대 경로
- `return null` → URL 바인딩 없음 (floatingBox.jelly로만 기여)

### 7.3 Action의 뷰

Action도 Jelly/Groovy 뷰를 가질 수 있다:

```
Action JavaDoc에서:
- floatingBox.jelly: 대상 ModelObject의 상단 페이지에 플로팅 박스로 표시
  (예: JUnit 테스트 결과 트렌드 그래프)
- action.jelly: 사이드 패널에서의 렌더링 커스터마이즈
  (기본 동작을 오버라이드하여 중첩 메뉴 등 구현)
```

### 7.4 Actionable: Action의 컨테이너

`Actionable` 클래스는 `Action` 목록을 관리한다:

```java
// core/src/main/java/hudson/model/Actionable.java (라인 54)
@ExportedBean
public abstract class Actionable extends AbstractModelObject
        implements ModelObjectWithContextMenu {
    private volatile CopyOnWriteArrayList<Action> actions;

    @Deprecated
    @NonNull
    public List<Action> getActions() {
        if (actions == null) {
            synchronized (this) {
                if (actions == null) {
                    // ...
                }
            }
        }
        return actions;
    }
}
```

`CopyOnWriteArrayList`를 사용하여 동시 접근 안전성을 보장한다. 이 리스트는 영속적(persistent) Action만 포함하며, 일시적(transient) Action은 `TransientActionFactory`를 통해 동적으로 생성된다.

### 7.5 RootAction: Jenkins 루트의 Action

```java
// core/src/main/java/hudson/model/RootAction.java (라인 42)
public interface RootAction extends Action, ExtensionPoint {
    // ...
}
```

`@Extension` 어노테이션과 함께 `RootAction`을 구현하면, Jenkins 루트 URL 아래에 자동으로 등록된다. 예를 들어 `getUrlName()`이 `"myPage"`를 반환하면 `/myPage` URL로 접근할 수 있다.

### 7.6 UnprotectedRootAction: 인증 없이 접근 가능한 Action

```java
// core/src/main/java/hudson/model/UnprotectedRootAction.java (라인 35)
public interface UnprotectedRootAction extends RootAction, ExtensionPoint {
}
```

`UnprotectedRootAction`은 `Jenkins.getTarget()`의 읽기 권한 확인에서 면제된다. 웹훅 수신이나 에이전트 연결처럼 인증 없이 접근해야 하는 엔드포인트에 사용된다.

```java
// core/src/main/java/jenkins/model/Jenkins.java (라인 5269)
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

---

## 8. TransientActionFactory: 동적 Action 생성

### 8.1 구조

소스 파일: `core/src/main/java/jenkins/model/TransientActionFactory.java`

```java
// core/src/main/java/jenkins/model/TransientActionFactory.java (라인 49)
public abstract class TransientActionFactory<T> implements ExtensionPoint {

    public abstract Class<T> type();

    public /* abstract */ Class<? extends Action> actionType() {
        return Action.class;
    }

    public abstract @NonNull Collection<? extends Action> createFor(@NonNull T target);
}
```

### 8.2 왜 TransientActionFactory가 필요한가

영속적 Action(`Actionable.actions` 리스트에 저장)과 달리, TransientActionFactory는:
- **디스크에 저장되지 않는** 일시적 Action을 생성한다
- **조건부로** Action을 표시/숨김할 수 있다 (예: 특정 권한이 있을 때만)
- **플러그인 간 느슨한 결합**을 가능하게 한다

### 8.3 실제 구현 예: RenameAction

```java
// core/src/main/java/jenkins/model/RenameAction.java (라인 55)
@Extension
public static class TransientActionFactoryImpl
        extends TransientActionFactory<AbstractItem> {

    @Override
    public Class<AbstractItem> type() {
        return AbstractItem.class;
    }

    @Override
    public Collection<? extends Action> createFor(AbstractItem target) {
        if (target.isNameEditable()) {
            return Set.of(new RenameAction());
        } else {
            return Collections.emptyList();
        }
    }
}
```

이 팩토리는:
1. `AbstractItem` 타입의 모든 객체에 대해 동작한다
2. 해당 아이템의 이름이 변경 가능한 경우에만 `RenameAction`을 반환한다
3. 이름 변경이 불가한 경우 빈 리스트를 반환하여 "Rename" 메뉴를 숨긴다

### 8.4 캐싱 메커니즘

```java
// core/src/main/java/jenkins/model/TransientActionFactory.java (라인 83)
@Extension
public static final class Cache extends ExtensionListListener {
    private ExtensionList<TransientActionFactory> allFactories;
    private ClassValue<ClassValue<List<TransientActionFactory<?>>>> cache;

    private synchronized ClassValue<ClassValue<List<TransientActionFactory<?>>>> cache() {
        if (allFactories == null) {
            allFactories = ExtensionList.lookup(TransientActionFactory.class);
            allFactories.addListener(this);
        }
        if (cache == null) {
            cache = new ClassValue<>() {
                @Override
                protected ClassValue<List<TransientActionFactory<?>>> computeValue(
                        Class<?> type) {
                    return new ClassValue<>() {
                        @Override
                        protected List<TransientActionFactory<?>> computeValue(
                                Class<?> actionType) {
                            List<TransientActionFactory<?>> factories = new ArrayList<>();
                            for (TransientActionFactory<?> taf : allFactories) {
                                if (taf.type().isAssignableFrom(type) &&
                                    (actionType.isAssignableFrom(taf.actionType()) ||
                                     taf.actionType().isAssignableFrom(actionType))) {
                                    factories.add(taf);
                                }
                            }
                            return factories;
                        }
                    };
                }
            };
        }
        return cache;
    }

    @Override
    public synchronized void onChange() {
        cache = null;  // 플러그인 변경 시 캐시 무효화
    }
}
```

`ClassValue`를 이중으로 사용한 2차원 캐시:
- 1차 키: 대상 객체 타입 (예: `AbstractItem`)
- 2차 키: 액션 타입 (예: `Action.class`)
- 값: 해당하는 `TransientActionFactory` 목록

`ExtensionListListener`를 통해 플러그인이 추가/제거되면 캐시를 무효화한다.

---

## 9. 보안 통합

### 9.1 @RequirePOST / @POST

상태 변경을 수행하는 메서드에 POST 요청을 강제하는 어노테이션:

```java
// core/src/main/java/hudson/model/AbstractProject.java (라인 1933)
@RequirePOST
public HttpResponse doDoWipeOutWorkspace() throws IOException, InterruptedException {
    checkPermission(Functions.isWipeOutPermissionEnabled() ? WIPEOUT : BUILD);
    // ...
}
```

```java
// core/src/main/java/jenkins/model/Jenkins.java (라인 4032)
@POST
public synchronized void doConfigSubmit(StaplerRequest2 req, StaplerResponse2 rsp)
        throws IOException, ServletException, FormException {
    // ...
}
```

`@RequirePOST`는 레거시 방식이고, `@POST`(org.kohsuke.stapler.verb.POST)가 현재 권장 방식이다. GET 요청으로 이 메서드에 접근하면 405 Method Not Allowed 응답이 반환된다.

`AbstractModelObject`에도 레거시 방식의 POST 강제 메서드가 있다:

```java
// core/src/main/java/hudson/model/AbstractModelObject.java (라인 123)
@Deprecated
protected final void requirePOST() throws ServletException {
    StaplerRequest2 req = Stapler.getCurrentRequest2();
    if (req == null)  return;
    String method = req.getMethod();
    if (!method.equalsIgnoreCase("POST"))
        throw new ServletException("Must be POST, Can't be " + method);
}
```

### 9.2 @StaplerNotDispatchable

특정 메서드를 Stapler 라우팅에서 제외하는 어노테이션:

```java
// core/src/main/java/jenkins/security/stapler/StaplerNotDispatchable.java
@Target({ElementType.FIELD, ElementType.METHOD})
@Retention(RetentionPolicy.RUNTIME)
@Documented
public @interface StaplerNotDispatchable {}
```

실제 사용 예:

```java
// core/src/main/java/hudson/model/Api.java (라인 247)
@Deprecated
@StaplerNotDispatchable
public void doJson(StaplerRequest req, StaplerResponse rsp)
        throws IOException, javax.servlet.ServletException {
    // deprecated 버전 — 직접 라우팅되지 않음
}
```

이 어노테이션의 목적: deprecated된 오버로드 메서드가 Stapler에 의해 잘못 호출되는 것을 방지한다. 새로운 `StaplerRequest2`/`StaplerResponse2` 기반 메서드만 라우팅 대상이 된다.

### 9.3 @StaplerDispatchable

반대로 명시적으로 Stapler 라우팅을 허용하는 어노테이션:

```java
// core/src/main/java/jenkins/security/stapler/StaplerDispatchable.java
@Target({ElementType.FIELD, ElementType.METHOD})
@Retention(RetentionPolicy.RUNTIME)
@Documented
public @interface StaplerDispatchable {}
```

### 9.4 TypedFilter: Stapler 라우팅 보안 필터

소스 파일: `core/src/main/java/jenkins/security/stapler/TypedFilter.java`

`TypedFilter`는 Stapler가 URL을 통해 접근할 수 있는 메서드와 필드를 필터링하는 핵심 보안 컴포넌트다:

```java
// core/src/main/java/jenkins/security/stapler/TypedFilter.java (라인 23)
@Restricted(NoExternalUse.class)
public class TypedFilter implements FieldRef.Filter, FunctionList.Filter {

    // 클래스가 Stapler 라우팅 대상인지 판단
    private boolean isClassAcceptable(Class<?> clazz) {
        if (clazz.isArray()) {
            Class<?> elementClazz = clazz.getComponentType();
            if (isClassAcceptable(elementClazz)) {
                return true;
            }
            return false;
        }
        return SKIP_TYPE_CHECK || isStaplerRelevant.get(clazz);
    }
}
```

클래스의 Stapler 관련성을 판단하는 기준:

```java
// 라인 68
private static boolean isSpecificClassStaplerRelevant(@NonNull Class<?> clazz) {
    // 1. @StaplerAccessibleType 어노테이션
    if (clazz.isAnnotationPresent(StaplerAccessibleType.class)) {
        return true;
    }

    // 2. StaplerProxy 구현
    if (StaplerProxy.class.isAssignableFrom(clazz)) {
        return true;
    }

    // 3. StaplerFallback 구현
    if (StaplerFallback.class.isAssignableFrom(clazz)) {
        return true;
    }

    // 4. StaplerOverridable 구현
    if (StaplerOverridable.class.isAssignableFrom(clazz)) {
        return true;
    }

    // 5. 라우팅 가능한 메서드 보유
    for (Method m : clazz.getMethods()) {
        if (isRoutableMethod(m)) {
            return true;
        }
    }

    return false;
}
```

메서드의 라우팅 가능성 판단:

```java
// 라인 93
private static boolean isRoutableMethod(@NonNull Method m) {
    // 1. @WebMethod 등 웹 메서드 어노테이션 확인
    for (Annotation a : m.getDeclaredAnnotations()) {
        if (WebMethodConstants.WEB_METHOD_ANNOTATION_NAMES
                .contains(a.annotationType().getName())) {
            return true;
        }
        // 2. Stapler 인터셉터 어노테이션 (RequirePOST, JsonResponse 등)
        if (a.annotationType().isAnnotationPresent(InterceptorAnnotation.class)) {
            return true;
        }
    }

    // 3. @QueryParameter 등 웹 메서드 파라미터 어노테이션
    for (Annotation[] set : m.getParameterAnnotations()) {
        for (Annotation a : set) {
            if (WebMethodConstants.WEB_METHOD_PARAMETER_ANNOTATION_NAMES
                    .contains(a.annotationType().getName())) {
                return true;
            }
        }
    }

    // 4. StaplerRequest/StaplerResponse 파라미터 타입
    for (Class<?> parameterType : m.getParameterTypes()) {
        if (WebMethodConstants.WEB_METHOD_PARAMETERS_NAMES
                .contains(parameterType.getName())) {
            return true;
        }
    }

    return WebApp.getCurrent().getFilterForDoActions().keep(
        new Function.InstanceFunction(m));
}
```

### 9.5 특수 메서드 필터링

TypedFilter는 특정 메서드명을 특별히 처리한다:

```java
// 라인 228
if (function.getName().equals("getDynamic")) {
    Class[] parameterTypes = function.getParameterTypes();
    if (parameterTypes.length > 0 && parameterTypes[0] == String.class) {
        // getDynamic은 특별한 디스패치 메커니즘이므로
        // 일반 getter로는 취급하지 않음
        return false;
    }
}

if (function.getName().equals("getStaplerFallback")
        && function.getParameterTypes().length == 0) {
    // StaplerFallback 인터페이스의 특수 동작이므로
    // 일반 getter로 취급하지 않음
    return false;
}

if (function.getName().equals("getTarget")
        && function.getParameterTypes().length == 0) {
    // StaplerProxy 인터페이스의 특수 동작이므로
    // 일반 getter로 취급하지 않음
    return false;
}
```

이 필터링은 보안상 중요하다. `getTarget()`이 일반 getter로 라우팅되면 `StaplerProxy`의 권한 확인을 우회할 수 있기 때문이다.

### 9.6 RoutingDecisionProvider: 화이트리스트/블랙리스트

```java
// TypedFilter.java에서 (라인 137-163)
ExtensionList<RoutingDecisionProvider> decisionProviders =
    ExtensionList.lookup(RoutingDecisionProvider.class);
if (!decisionProviders.isEmpty()) {
    for (RoutingDecisionProvider provider : decisionProviders) {
        RoutingDecisionProvider.Decision fieldDecision = provider.decide(signature);
        if (fieldDecision == RoutingDecisionProvider.Decision.ACCEPTED) {
            return true;
        }
        if (fieldDecision == RoutingDecisionProvider.Decision.REJECTED) {
            return false;
        }
    }
}
```

`RoutingDecisionProvider`를 통해 플러그인이 특정 메서드/필드의 Stapler 접근 허용/거부를 결정할 수 있다. 이는 보안 취약점이 발견되었을 때 핫픽스 없이도 특정 엔드포인트를 차단할 수 있게 한다.

### 9.7 정적 멤버 접근 금지

```java
// TypedFilter.java (라인 165)
if (PROHIBIT_STATIC_ACCESS && fieldRef.isStatic()) {
    return false;
}
```

기본적으로 static 필드/메서드에 대한 Stapler 라우팅을 금지한다. static 멤버를 통한 접근은 인스턴스 기반 권한 확인을 우회할 수 있기 때문이다.

---

## 10. 전체 요청 처리 흐름

### 10.1 엔드투엔드 흐름 다이어그램

```
요청: POST /job/my-project/build?delay=0sec

  ┌─────────────────────────────────────────────────────────────┐
  │ 1. 서블릿 필터 체인                                          │
  │    ├─ 인증 필터 (SecurityRealm)                              │
  │    ├─ CSRF 방지 필터 (crumb)                                 │
  │    └─ Stapler 서블릿 디스패치                                 │
  │                                                             │
  │ 2. URL 파싱: "/" → "job" → "my-project" → "build"           │
  │                                                             │
  │ 3. Jenkins.getTarget() [StaplerProxy]                       │
  │    └─ checkPermission(READ) → OK                            │
  │                                                             │
  │ 4. URL "job" → 뷰/getter 탐색 실패                          │
  │    (Jenkins에 "getJob()" 없음)                               │
  │    → "job"이 라우팅 경로의 일부로 처리                         │
  │    → Jenkins.getItem("my-project") 호출                     │
  │                                                             │
  │ 5. AbstractItem.getTarget() [StaplerProxy]                  │
  │    ├─ hasPermission(Item.DISCOVER) → true                   │
  │    ├─ checkPermission(Item.READ) → OK                       │
  │    └─ return AbstractProject                                │
  │                                                             │
  │ 6. URL "build" → AbstractProject.doBuild(...) 탐색          │
  │    ├─ @POST 어노테이션 확인 → POST 요청 → OK                 │
  │    ├─ TypedFilter.keep() → true (웹 메서드 파라미터 확인)     │
  │    └─ 메서드 호출                                            │
  │                                                             │
  │ 7. doBuild() 실행                                           │
  │    ├─ 권한 확인: checkPermission(BUILD)                      │
  │    ├─ 빌드 큐에 등록                                         │
  │    └─ 201 Created + Location 헤더 응답                       │
  └─────────────────────────────────────────────────────────────┘
```

### 10.2 REST API 요청 흐름

```
요청: GET /job/my-project/api/json?tree=name,lastBuild[number,result]

  ┌─────────────────────────────────────────────────────────────┐
  │ 1. Jenkins.getTarget() → 권한 확인 통과                      │
  │                                                             │
  │ 2. Jenkins.getItem("my-project") → AbstractProject 반환     │
  │                                                             │
  │ 3. AbstractItem.getTarget() → 권한 확인 통과                 │
  │                                                             │
  │ 4. AbstractProject.getApi() → new Api(this) 반환            │
  │                                                             │
  │ 5. Api.doJson(req, rsp) 호출                                │
  │    ├─ setHeaders(rsp) → 보안 헤더 추가                       │
  │    ├─ tree 파라미터 파싱 → NamedPathPruner 생성              │
  │    └─ rsp.serveExposedBean(req, bean, Flavor.JSON) 호출     │
  │                                                             │
  │ 6. Stapler의 ExposedBean 직렬화                             │
  │    ├─ ModelBuilder.get(AbstractProject.class)                │
  │    ├─ @ExportedBean, @Exported 어노테이션 스캔               │
  │    ├─ TreePruner로 요청된 필드만 직렬화                       │
  │    └─ JSON 출력: {"name":"my-project",                      │
  │       "lastBuild":{"number":42,"result":"SUCCESS"}}         │
  └─────────────────────────────────────────────────────────────┘
```

---

## 11. AbstractModelObject: 공통 기반

### 11.1 구조

소스 파일: `core/src/main/java/hudson/model/AbstractModelObject.java`

```java
// core/src/main/java/hudson/model/AbstractModelObject.java (라인 47)
public abstract class AbstractModelObject implements SearchableModelObject {

    protected final void sendError(Exception e, StaplerRequest2 req,
            StaplerResponse2 rsp) throws ServletException, IOException {
        req.setAttribute("exception", e);
        sendError(e.getMessage(), req, rsp);
    }

    protected final void sendError(String message, StaplerRequest2 req,
            StaplerResponse2 rsp) throws ServletException, IOException {
        req.setAttribute("message", message);
        rsp.forward(this, "error", req);
    }
}
```

`sendError()` 메서드는 에러 메시지를 `error.jelly` 뷰로 포워드한다. 이 뷰는 `ALWAYS_READABLE_PATHS`에 "error"가 포함되어 있어 인증 없이도 접근 가능하다.

### 11.2 검색 통합

```java
// 라인 134
protected SearchIndexBuilder makeSearchIndex() {
    return new SearchIndexBuilder().addAllAnnotations(this);
}

@Override
public final SearchIndex getSearchIndex() {
    return makeSearchIndex().make();
}

@Override
public Search getSearch() {
    for (SearchFactory sf : SearchFactory.all()) {
        Search s = sf.createFor(this);
        if (s != null)
            return s;
    }
    return new Search();
}
```

`AbstractModelObject`은 Jenkins의 검색 기능과도 통합된다. `SearchableModelObject` 인터페이스를 구현하여, 각 모델 객체가 Jenkins 검색 결과에 나타날 수 있도록 한다.

---

## 12. 클래스 계층과 Stapler 통합 전체도

```
                    ModelObject (인터페이스)
                         │
                    SearchableModelObject
                         │
                  AbstractModelObject
                    ├─ getSearch()
                    ├─ sendError()
                    └─ requirePOST() [deprecated]
                         │
              ┌──────────┴──────────┐
              │                     │
          Actionable                Api
    ├─ CopyOnWriteArrayList      ├─ doXml()
    │  <Action> actions           ├─ doJson()
    ├─ getAllActions()            ├─ doPython()
    └─ addAction()               └─ doSchema()
              │
         AbstractItem
    ├─ implements StaplerProxy
    ├─ getTarget() → 권한 확인
    ├─ doConfigDotXml()
    └─ doSubmitDescription()
              │
            Job<JobT, RunT>
    ├─ implements StaplerOverridable
    ├─ getBuild(String)
    ├─ getApi() → new Api(this)
    ├─ @Exported getNextBuildNumber()
    └─ @Exported isInQueue()
              │
       AbstractProject<P, R>
    ├─ doBuild()
    ├─ doConfigSubmit()
    ├─ doDoWipeOutWorkspace()
    └─ DescriptorImpl
        ├─ doCheckNumExecutors()
        ├─ doAutoCompleteUpstreamProjects()
        └─ doAutoCompleteLabel()


    Jenkins (루트 객체)
    ├─ implements StaplerProxy     → getTarget()
    ├─ implements StaplerFallback  → getStaplerFallback()
    ├─ getItem(name) → Job 라우팅
    ├─ getComputer(name) → Node 라우팅
    ├─ getView(name) → View 라우팅
    ├─ getPluginManager() → 플러그인 관리
    ├─ getSecurityRealm() → 보안
    ├─ getApi() → REST API
    ├─ doConfigSubmit() → 설정 저장
    ├─ @WebMethod("404") → 404 페이지
    └─ DescriptorImpl
        ├─ getDynamic(token) → Descriptor 동적 라우팅
        ├─ doCheckNumExecutors()
        └─ doFillJobNameItems()
```

---

## 13. Stapler 설계의 장단점 분석

### 13.1 왜 이 설계를 선택했는가

**URL = 객체 그래프**: Jenkins의 데이터 모델은 본질적으로 트리 구조다 (Jenkins → Folder → Job → Build → Action). Stapler는 이 구조를 URL에 1:1로 대응시킴으로써:

1. **별도의 라우팅 테이블이 불필요**: 객체를 추가하면 URL이 자동으로 생성된다
2. **플러그인 확장이 자연스럽다**: 새로운 Action이나 View를 추가하면 URL 경로가 자동으로 확장된다
3. **REST API가 무료**: `@ExportedBean`/`@Exported` 어노테이션만 추가하면 `getApi()`를 통해 자동으로 REST API가 제공된다

**보안 모델 통합**: 각 객체의 `getTarget()`에서 권한을 확인하므로, URL 깊이에 관계없이 일관된 접근 제어가 가능하다.

### 13.2 주의할 점

1. **의도치 않은 메서드 노출**: 모든 public getter가 잠재적 URL 엔드포인트이므로, `@StaplerNotDispatchable`이나 TypedFilter로 접근을 제한해야 한다
2. **getter 이름 변경 = URL 변경**: 메서드명을 리팩토링하면 URL이 깨질 수 있다
3. **디버깅 복잡성**: 어떤 메서드가 어떤 URL에 매핑되는지 추적하기 어려울 수 있다
4. **deprecated 메서드의 오버로드**: StaplerRequest/StaplerRequest2 마이그레이션 시 `@StaplerNotDispatchable`로 레거시 오버로드를 라우팅에서 제외해야 한다

---

## 요약

| 핵심 개념 | 구현 위치 | 역할 |
|----------|---------|------|
| URL-to-Object 매핑 | Stapler 프레임워크 | URL 세그먼트를 getter 호출로 변환 |
| StaplerProxy | `Jenkins.getTarget()`, `AbstractItem.getTarget()` | 요청 전 권한 확인 |
| StaplerFallback | `Jenkins.getStaplerFallback()` | URL 매핑 실패 시 기본 View로 폴백 |
| REST API | `Api.doXml/doJson/doPython` | XML/JSON/Python 형식의 데이터 노출 |
| @Exported/@ExportedBean | 각 모델 클래스의 getter | API에 노출할 필드 지정 |
| do{Action} | 각 모델 클래스 | HTTP 요청 핸들러 |
| doCheck{Field} | Descriptor 클래스 | AJAX 폼 검증 |
| doFill{Field}Items | Descriptor 클래스 | 드롭다운 동적 채우기 |
| Jelly/Groovy 뷰 | `src/main/resources/{패키지}/{클래스}/` | HTML 렌더링 |
| ${it} | Jelly 뷰 | 현재 바인딩된 Java 객체 참조 |
| Action | `hudson.model.Action` | URL 경로, 아이콘, 표시명으로 UI 기여 |
| TransientActionFactory | `jenkins.model.TransientActionFactory` | 조건부 동적 Action 생성 |
| TypedFilter | `jenkins.security.stapler.TypedFilter` | 라우팅 보안 필터링 |
| @StaplerNotDispatchable | 보안 어노테이션 | 특정 메서드 라우팅 금지 |
| @RequirePOST / @POST | 보안 어노테이션 | POST 메서드 강제 |
| ALWAYS_READABLE_PATHS | `Jenkins.java` | 인증 없이 접근 가능한 경로 목록 |
