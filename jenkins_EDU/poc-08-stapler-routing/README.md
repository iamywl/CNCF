# PoC-08: Jenkins Stapler 웹 프레임워크 URL-to-Object 라우팅

## 개요

Jenkins의 Stapler 웹 프레임워크 핵심 메커니즘을 Go 표준 라이브러리만으로 시뮬레이션한다.
Stapler는 URL 경로의 각 세그먼트를 Java 객체 트리의 getter 메서드 호출로 변환하여
객체 그래프를 탐색하는 독특한 라우팅 방식을 사용한다. 이 PoC는 그 URL-to-Object 매핑
알고리즘, 메서드 우선순위 규칙, 액션 메서드 디스패치, 뷰 렌더링을 재현한다.

## 실행

```bash
go run main.go
```

## 핵심 개념: URL 라우팅 흐름

```
URL: /job/my-app/42/console

  / ──── Jenkins (루트 객체, StaplerProxy + StaplerFallback)
  |
  /job ── getItem(String) 매칭 (문자열 파라미터 getter)
  |       Jenkins.getItem("my-app")
  |
  /my-app ── → Job("my-app") 반환
  |
  /42 ──── getDynamic("42") → Integer.parseInt(42) → getBuildByNumber(42)
  |         Job.getDynamic에서 빌드 번호로 파싱 시도
  |
  ────── → Run(#42) 반환
  |
  /console ── 뷰 파일 탐색: {Run}/console.jelly
               클래스 계층을 따라가며 Jelly 템플릿 검색
```

## Stapler URL 라우팅 우선순위 규칙

```
URL 세그먼트 수신
       |
       v
+------+------+
| StaplerProxy |  getTarget() → 권한 확인
|              |  null이면 403 Forbidden
+------+------+
       |
       v
[1] getXxx()           파라미터 없는 getter
    예: /pluginManager  → getPluginManager()
       |
       v (실패)
[2] getXxx(String)     문자열 파라미터 getter
    예: /job/my-app    → getItem("my-app")
       |
       v (실패)
[3] getXxx(int)        숫자 파라미터 getter
    예: /42            → getBuildByNumber(42)
       |
       v (실패)
[4] getDynamic(String)  동적 라우트 (Action, Permalink 등)
    예: /lastBuild → Permalink 해석
       |
       v (실패)
[5] do{Action}()       액션 메서드 (POST 처리)
    예: /build → doBuild(req, rsp)
    패턴: ^do[^a-z].*  (DoActionFilter)
       |
       v (실패)
[6] 뷰 파일 탐색       Jelly/Groovy 템플릿
    예: /console → {Run}/console.jelly
       |
       v (실패)
[7] StaplerFallback    대체 객체 반환
    예: Jenkins → getPrimaryView()
       |
       v (실패)
    404 Not Found
```

## Jenkins 소스 참조

| 컴포넌트 | 실제 파일 | 핵심 메서드 | 설명 |
|----------|-----------|------------|------|
| Jenkins (루트) | `jenkins/model/Jenkins.java:355` | `getItem(name)`, `getView(name)`, `getComputer()`, `getDynamic(token)` | 루트 객체, StaplerProxy + StaplerFallback 구현 |
| Job | `hudson/model/Job.java:900` | `getDynamic(token, req, rsp)` | 빌드 번호 파싱 → Widget → Permalink → super.getDynamic |
| Run | `hudson/model/Run.java:2606` | `getDynamic(token, req, rsp)`, `getTarget()` | transient Action 검색, StaplerProxy 권한 확인 |
| View | `hudson/model/View.java:605` | `getDynamic(token)` | Action urlName 매칭 |
| Actionable | `hudson/model/Actionable.java:348` | `getDynamic(token, req, rsp)` | getAllActions()에서 urlName 매칭 |
| DoActionFilter | `jenkins/security/stapler/DoActionFilter.java:50` | `keep(Function m)` | `^do[^a-z].*` 패턴으로 웹 메서드 필터 |
| WebMethodConstants | `jenkins/security/stapler/WebMethodConstants.java:53` | - | 웹 메서드 파라미터/어노테이션 상수 정의 |
| StaplerFallback | `jenkins/model/Jenkins.java:5287` | `getStaplerFallback()` | URL 매핑 실패 시 primaryView로 폴백 |

## 시뮬레이션 시나리오

### 시나리오 1: 기본 객체 탐색
- `/job/my-app` → getItem(String) 매칭
- `/job/my-app/42` → getBuildByNumber(int) 매칭
- `/job/my-app/42/console` → 뷰 렌더링 (console.jelly)

### 시나리오 2: 깊은 객체 그래프 탐색
- `/job/my-app/42/artifact` → Run.getArtifacts() (파라미터 없는 getter)
- `/job/my-app/42/testReport` → getDynamic으로 Action 검색

### 시나리오 3: View/Computer 경로
- `/view/all` → Jenkins.getView(String) 매칭
- `/computer` → Jenkins.getComputer() (파라미터 없는 getter)
- `/computer/master/0` → Computer → getChildByInt(0) → Executor

### 시나리오 4: 파라미터 없는 getter
- `/pluginManager` → Jenkins.getPluginManager()
- `/pluginManager/installed` → PluginManager의 installed.jelly 뷰

### 시나리오 5: 액션 메서드 (do{Action})
- `/job/my-app/build` → doBuild() 빌드 트리거
- `/job/my-app/configSubmit` → doConfigSubmit() 설정 저장
- `/job/my-app/42/doDelete` → doDoDelete() 빌드 삭제

### 시나리오 6: Permalink (getDynamic)
- `/job/my-app/lastBuild` → Permalink 해석으로 최신 Run 반환
- `/job/my-app/lastSuccessfulBuild` → 최근 성공 빌드 반환

### 시나리오 7: getDynamic으로 Action 찾기
- `/manage` → Jenkins.getDynamic("manage") → ManagementLink Action
- `/job/my-app/ws` → Job.getDynamic("ws") → Workspace Action

### 시나리오 8: 뷰 렌더링
- `/job/my-app/configure` → Job/configure.jelly
- `/job/my-app/42/changes` → Run의 changes Action 또는 뷰

### 시나리오 9: 실패 케이스 (404)
- `/job/nonexistent` → 존재하지 않는 Job
- `/job/my-app/999` → 존재하지 않는 빌드 번호
- `/unknown/path` → StaplerFallback 후에도 실패

## 객체 그래프 구조

```
Jenkins (루트, StaplerProxy + StaplerFallback)
  |
  +-- /job/{name}  [getItem(String)]
  |   +-- my-app  (Job)
  |   |   +-- #41  (FAILURE)
  |   |   +-- #42  (SUCCESS) [2 artifacts]
  |   |   |   +-- artifact/ → ArtifactList
  |   |   |   |   +-- app.jar
  |   |   |   |   +-- README.md
  |   |   |   +-- testReport → Action
  |   |   +-- #43  (SUCCESS)
  |   +-- backend-api  (Job)
  |       +-- #1  (SUCCESS)
  |
  +-- /view/{name}  [getView(String)]
  |   +-- all  (아이템: my-app, backend-api)
  |   +-- frontend  (아이템: my-app)
  |
  +-- /computer  [getComputer()]
  |   +-- master  (온라인, Executor 2개)
  |   |   +-- Executor #0 (busy)
  |   |   +-- Executor #1 (idle)
  |   +-- agent-01  (온라인, Executor 1개)
  |       +-- Executor #0 (idle)
  |
  +-- /pluginManager  [getPluginManager()]
  |
  +-- StaplerFallback → primaryView: all
```

## 핵심 구조

```
StaplerObject (기본 인터페이스)
    |
    +-- StaplerGetterNoArg    → getXxx() 파라미터 없는 getter
    +-- StaplerGetterString   → getXxx(String) 문자열 getter
    +-- StaplerGetterInt      → getXxx(int) 숫자 getter
    +-- StaplerDynamic        → getDynamic(String) 동적 라우트
    +-- StaplerAction         → do{Action}() 액션 메서드
    +-- StaplerViewable       → 뷰 렌더링 (Jelly/Groovy)
    +-- StaplerProxy          → getTarget() 권한 확인
    +-- StaplerFallback       → 대체 객체 반환
    |
StaplerRouter
    |
    +-- Route(url) → 세그먼트별 탐색 → RouteResult
         우선순위: NoArg → String → Int → Dynamic → Action → View → Fallback
```

## 예상 출력

```
================================================================
  Jenkins Stapler URL-to-Object 라우팅 시뮬레이션
  참조: jenkins.model.Jenkins, org.kohsuke.stapler.Stapler
================================================================

=== Stapler URL 라우팅 우선순위 규칙 ===
...

=== Jenkins 객체 그래프 ===
Jenkins (루트, StaplerProxy + StaplerFallback)
  |
  +-- /job/{name}  [getItem(String)]
  |   +-- my-app  (Job)
  |   |   +-- #41  (FAILURE)
  |   |   +-- #42  (SUCCESS) [2 artifacts]
  |   |   +-- #43  (SUCCESS)
  ...

=== URL 라우팅 테스트 ===

--- Job 조회 (getItem(String)) ---
[URL] /job/my-app
----------------------------------------------------------------------
  [O] /                     root                            루트 객체: Jenkins (Jenkins)
  -> [O] job/my-app          getJob("my-app")                → my-app (Job)

  => 결과: 라우팅 성공 → my-app (Job)

--- 빌드 콘솔 뷰 (getItem → getBuildByNumber → console.jelly) ---
[URL] /job/my-app/42/console
----------------------------------------------------------------------
  [O] /                     root                            루트 객체: Jenkins (Jenkins)
  -> [O] job/my-app          getJob("my-app")                → my-app (Job)
    -> [O] 42                getBuildByNumber(42)            → my-app #42 (Run)
      -> [V] console         Run/console.jelly               [Console 뷰] ...

  => 결과: 라우팅 성공 → my-app #42 (Run)

--- 빌드 트리거 (doBuild()) ---
[URL] /job/my-app/build
----------------------------------------------------------------------
  [O] /                     root                            루트 객체: Jenkins (Jenkins)
  -> [O] job/my-app          getJob("my-app")                → my-app (Job)
    -> [A] build             doBuild(req, rsp)               [ACTION] Job 'my-app' 빌드 트리거

  => 결과: 라우팅 성공 → my-app (Job)

--- 존재하지 않는 Job (404) ---
[URL] /job/nonexistent
----------------------------------------------------------------------
  [O] /                     root                            루트 객체: Jenkins (Jenkins)
  -> [O] job                StaplerFallback                  폴백 → all (View)
    -> [X] job               N/A                             404 Not Found

  => 결과: 라우팅 실패 (404 Not Found)
```

## Stapler 설계 핵심 원리

1. **URL = 객체 그래프 경로**: URL의 각 `/`가 getter 메서드 호출에 대응. 전통적인 Controller 기반 라우팅과 근본적으로 다름.
2. **Convention over Configuration**: `get{Name}()`/`do{Action}()` 네이밍 규칙만으로 자동 라우팅. 별도의 라우트 등록 불필요.
3. **보안이 객체 레벨에 내장**: `StaplerProxy.getTarget()`으로 접근 전 권한 확인. getter 내부에서 `Item.READ`/`DISCOVER` 권한 체크.
4. **확장성**: 플러그인이 Action을 추가하면 `getDynamic()`이 urlName으로 자동 매칭하여 URL 노출.
5. **StaplerFallback**: Jenkins 루트에서 매핑 실패 시 primaryView로 우아하게 폴백.
