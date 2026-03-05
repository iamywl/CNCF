# PoC-01: Jenkins WAR 디스패처 + 초기화 시퀀스

## 목적

Jenkins의 단일 WAR 아키텍처와 부팅 시퀀스를 Go로 시뮬레이션한다.
하나의 바이너리(WAR)에 웹 서버, 플러그인 관리, 빌드 엔진 등 여러 컴포넌트가 포함되는 구조와,
Reactor 기반 DAG 위상정렬을 통한 초기화 과정을 재현한다.

## 핵심 개념

### 1. 단일 WAR 구조
Jenkins는 `jenkins.war` 하나로 전체 시스템이 실행된다.
내장 Jetty(Winstone)가 웹 서버를 제공하고, `web.xml`에 등록된
`WebAppMain(ServletContextListener)`이 초기화를 시작한다.

### 2. 초기화 흐름
```
java -jar jenkins.war
  → Main 클래스 (Winstone 시작)
    → WebAppMain.contextInitialized()
      → 초기화 스레드(Thread) 시작
        → Jenkins 싱글톤 생성
          → Reactor 실행 (DAG 위상정렬)
            → InitMilestone 순서대로 초기화
```

### 3. InitMilestone 순서
```
STARTED → PLUGINS_LISTED → PLUGINS_PREPARED → PLUGINS_STARTED
  → EXTENSIONS_AUGMENTED → SYSTEM_CONFIG_LOADED → SYSTEM_CONFIG_ADAPTED
    → JOB_LOADED → JOB_CONFIG_ADAPTED → COMPLETED
```

### 4. Reactor 패턴
- 각 초기화 작업(Task)에 `requires`(선행 마일스톤)와 `attains`(달성 마일스톤)을 선언
- `InitMilestone.ordering()`으로 마일스톤 간 순서를 강제하는 NOOP 작업 삽입
- Kahn's Algorithm으로 위상정렬하여 실행 순서 결정

### 5. Stapler URL 라우팅
- URL 경로를 Java 객체 트리에 매핑하는 프레임워크
- `/job/my-app/42/console` → `Jenkins.getItem("my-app").getBuild(42).doConsole()`

### 6. 플러그인 의존성 해석
- 플러그인 간 의존성을 DAG로 모델링
- 위상정렬로 로드 순서 결정 (의존 대상을 먼저 로드)
- 각 플러그인에 독립적인 ClassLoader 할당

## 실제 Jenkins 소스 참조

| 파일 | 역할 |
|------|------|
| `core/src/main/java/hudson/init/InitMilestone.java` | 초기화 마일스톤 enum, ordering() 메서드 |
| `core/src/main/java/hudson/WebAppMain.java` | ServletContextListener, 초기화 스레드 |
| `core/src/main/java/jenkins/model/Jenkins.java` | 메인 싱글톤, 모든 서브시스템 관리 |
| `core/src/main/java/hudson/PluginManager.java` | 플러그인 스캔, 의존성 해석, ClassLoader |

## 실행 방법

```bash
cd jenkins_EDU/poc-01-architecture
go run main.go
```

## 예상 출력

1. WAR 파일 내부 구조 시각화
2. JENKINS_HOME 파일시스템 구조
3. InitMilestone DAG 다이어그램
4. 서브시스템 아키텍처 다이어그램
5. ClassLoader 계층 구조
6. Kahn's Algorithm 위상정렬 과정 (step-by-step)
7. Stapler URL 라우팅 규칙
8. 부팅 시퀀스 실행 (10개 마일스톤 순차 달성)
9. HTTP 디스패처 시뮬레이션 (가상 요청/응답)
