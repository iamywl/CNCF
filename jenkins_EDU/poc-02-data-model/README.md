# PoC-02: Jenkins 핵심 데이터 모델

## 목적

Jenkins의 핵심 데이터 모델을 Go로 시뮬레이션한다.
Java의 재귀적 제네릭(F-bounded polymorphism)으로 구현된 Job/Run 계층 구조,
Action/Actionable 동적 확장 패턴, Node/Computer/Executor 3계층 빌드 인프라,
그리고 빌드 라이프사이클 상태 전이를 재현한다.

## 핵심 개념

### 1. Job/Run 상속 계층 (F-bounded Polymorphism)

Jenkins의 Job과 Run은 재귀적 제네릭으로 서로를 참조한다:

```java
// Job<JobT, RunT> — JobT와 RunT가 자기 자신과 상대를 타입 파라미터로 참조
public abstract class Job<JobT extends Job<JobT, RunT>, RunT extends Run<JobT, RunT>>
    extends AbstractItem implements ExtensionPoint, StaplerOverridable, ...

public abstract class Run<JobT extends Job<JobT, RunT>, RunT extends Run<JobT, RunT>>
    extends Actionable implements ExtensionPoint, Comparable<RunT>, ...
```

구체적인 상속 체인:

```
Job 쪽:  FreeStyleProject → Project → AbstractProject → Job → AbstractItem → Actionable
Run 쪽:  FreeStyleBuild   → Build   → AbstractBuild   → Run → Actionable
```

이 설계 덕분에 `FreeStyleProject.getLastBuild()`가 `Run`이 아닌 `FreeStyleBuild`를 반환한다.

### 2. 빌드 라이프사이클 상태 전이

Run 내부의 `State` enum이 빌드 생명주기를 관리한다:

```
NOT_STARTED → BUILDING → POST_PRODUCTION → COMPLETED
```

- **NOT_STARTED**: 빌드 생성/큐잉됨, 아직 실행 안 함
- **BUILDING**: 빌드 실행 중. `result` 값이 이 상태에서 변경될 수 있음
- **POST_PRODUCTION**: 빌드 완료, 결과 확정. Jenkins가 이 상태부터 "완료"로 간주하여 후속 빌드 트리거 가능 (JENKINS-980)
- **COMPLETED**: 모든 작업 종료, 로그 파일 닫힘

### 3. Result (빌드 결과)

`Result`는 불변 객체로, ordinal 값이 낮을수록 양호하다:

| Result | Ordinal | CompleteBuild | Color |
|--------|---------|---------------|-------|
| SUCCESS | 0 | true | blue |
| UNSTABLE | 1 | true | yellow |
| FAILURE | 2 | true | red |
| NOT_BUILT | 3 | false | notbuilt |
| ABORTED | 4 | false | aborted |

`isWorseThan()`, `combine()` 메서드로 결과를 비교/합성한다.

### 4. Action/Actionable 패턴

`Actionable`은 `CopyOnWriteArrayList<Action>`을 가진 기반 클래스로,
Run, Job, Computer 등이 모두 이를 확장한다. 플러그인은 기존 클래스를 수정하지 않고
`Action`을 동적으로 첨부하여 데이터를 추가할 수 있다.

대표적인 Action 구현체:
- `CauseAction` — 빌드 원인 기록 ("Started by user admin")
- `ParametersAction` — 빌드 파라미터 값
- `TestResultAction` — 테스트 결과 (junit 플러그인)

### 5. Node / Computer / Executor 3계층

| 계층 | 클래스 | 역할 | 수명 |
|------|--------|------|------|
| 설정 | Node | 사용자 설정(numExecutors, labels, mode) | 설정 변경 시 재생성 |
| 런타임 | Computer | 온라인/오프라인 상태, Executor 목록 관리 | Node 존재 동안 유지 |
| 실행 | Executor | Thread 기반, Queue에서 작업 할당받아 실행 | 빌드 실행 동안 BUSY |

Node와 Computer는 1:1, Computer와 Executor는 1:N 관계이다.

### 6. 빌드 번호 관리 (nextBuildNumber)

`nextBuildNumber`는 `JENKINS_HOME/jobs/{name}/nextBuildNumber` 별도 파일에 저장된다.
`config.xml`과 독립적으로 관리되므로 VCS에서 설정만 관리해도 빌드 번호가 보존된다.
`assignBuildNumber()`는 `synchronized(job)` 블록 내에서 원자적으로 실행된다.

## 실제 Jenkins 소스 참조

| 파일 | 역할 |
|------|------|
| `core/src/main/java/hudson/model/Job.java` | Job 기반 클래스. nextBuildNumber, assignBuildNumber(), getLastBuild() |
| `core/src/main/java/hudson/model/Run.java` | Run 기반 클래스. number, timestamp, duration, result, State enum |
| `core/src/main/java/hudson/model/Result.java` | 빌드 결과. SUCCESS/UNSTABLE/FAILURE/NOT_BUILT/ABORTED, ordinal 비교 |
| `core/src/main/java/hudson/model/Actionable.java` | Action 리스트 관리. CopyOnWriteArrayList<Action>, addAction(), getAllActions() |
| `core/src/main/java/hudson/model/AbstractItem.java` | Item 기반 클래스. name, fullName, description |
| `core/src/main/java/hudson/model/AbstractProject.java` | 빌드 가능 Job. scm, triggers, disabled |
| `core/src/main/java/hudson/model/Project.java` | builders, buildWrappers, publishers 관리 |
| `core/src/main/java/hudson/model/FreeStyleProject.java` | 구체적 Job 타입. `Project<FreeStyleProject, FreeStyleBuild>` 확장 |
| `core/src/main/java/hudson/model/Build.java` | 구체적 Run 타입. `AbstractBuild<P, B>` 확장 |
| `core/src/main/java/hudson/model/FreeStyleBuild.java` | `Build<FreeStyleProject, FreeStyleBuild>` 확장 |
| `core/src/main/java/hudson/model/Node.java` | 빌드 에이전트 설정. getNumExecutors(), getMode() |
| `core/src/main/java/hudson/model/Computer.java` | Node 런타임 상태. executors[], isOffline(), countBusy() |
| `core/src/main/java/hudson/model/Executor.java` | 빌드 실행 스레드. Computer.owner, executable, number |

## 실행 방법

```bash
cd jenkins_EDU/poc-02-data-model
go run main.go
```

## 예상 출력

1. Job/Run 상속 계층 구조 ASCII 다이어그램 (실제 Java 클래스 매핑)
2. Result 비교 매트릭스 (ordinal 기반 isWorseThan/combine)
3. Node/Computer/Executor 3계층 관계도 및 생성 시뮬레이션
4. Job 생성 → 빌드 3회 실행 → 상태 전이 로그 (NOT_STARTED → BUILDING → POST_PRODUCTION → COMPLETED)
5. Action/Actionable 패턴 데모 (CauseAction, ParametersAction, TestResultAction 첨부 및 조회)
6. 빌드 히스토리 테이블 (빌드 번호, 결과, 소요 시간, 상태, 원인)
7. 빌드 조회 메서드 데모 (getLastBuild, getLastSuccessfulBuild, getLastFailedBuild, getBuildByNumber)
8. nextBuildNumber 동시 할당 시뮬레이션 (Mutex 보호 검증)
9. 빌드 상태 전이 다이어그램
10. 전체 데이터 모델 관계 요약 다이어그램
