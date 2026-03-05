# PoC-10: Jenkins InitMilestone 및 Reactor 위상 정렬

## 목적

Jenkins의 초기화 시스템을 Go로 시뮬레이션한다.
Reactor 패턴 기반의 DAG 위상정렬을 통해 InitMilestone 순서를 보장하면서
독립 작업은 병렬 실행하는 메커니즘을 재현한다.

## 핵심 개념

### 1. InitMilestone (10단계)
```
STARTED → PLUGINS_LISTED → PLUGINS_PREPARED → PLUGINS_STARTED
  → EXTENSIONS_AUGMENTED → SYSTEM_CONFIG_LOADED → SYSTEM_CONFIG_ADAPTED
    → JOB_LOADED → JOB_CONFIG_ADAPTED → COMPLETED
```

각 마일스톤은 초기화 과정의 체크포인트이며, 다음 마일스톤으로 진행하기 전에
해당 마일스톤에 등록된 모든 작업이 완료되어야 한다.

### 2. @Initializer 어노테이션
초기화 메서드에 의존성을 선언:
- `after`: 이 마일스톤 이후에 실행
- `before`: 이 마일스톤 이전에 실행
- `requires`: 선행 마일스톤 (= after)
- `attains`: 달성 마일스톤 (= before의 다음)
- `fatal`: true이면 실패 시 부팅 중단

### 3. Reactor 패턴
- TaskGraphBuilder로 작업 간 의존성 DAG 구성
- InitMilestone.ordering()이 마일스톤 간 순서를 강제하는 NOOP 작업 삽입
- Kahn's Algorithm으로 위상정렬
- 의존성이 충족된 작업은 병렬 실행 (스레드풀)

### 4. InitReactorRunner
- Reactor를 실제로 실행하는 러너
- 스레드풀 관리, 진행률 보고
- 마일스톤 달성 시 콜백 호출

## 실제 Jenkins 소스 참조

| 파일 | 역할 |
|------|------|
| `core/src/main/java/hudson/init/InitMilestone.java` | 10단계 마일스톤 열거형, ordering() |
| `core/src/main/java/hudson/init/Initializer.java` | @Initializer 어노테이션 정의 |
| `core/src/main/java/hudson/init/InitReactorRunner.java` | Reactor 실행, 스레드풀, 진행률 |
| `core/src/main/java/hudson/init/InitStrategy.java` | 초기화 전략 (작업 발견) |
| `core/src/main/java/hudson/init/TaskMethodFinder.java` | @Initializer 메서드 탐색 |

## 실행 방법

```bash
cd jenkins_EDU/poc-10-init-milestone
go run main.go
```

## 예상 출력

1. InitMilestone 10단계 목록 및 의존성 다이어그램
2. @Initializer 작업 등록 및 DAG 구성
3. Kahn's Algorithm 위상정렬 과정 (step-by-step)
4. 병렬 실행: 독립 작업은 동시에, 의존 작업은 순서대로
5. 마일스톤 달성 이벤트 로그
6. 전체 초기화 완료 시간 통계
