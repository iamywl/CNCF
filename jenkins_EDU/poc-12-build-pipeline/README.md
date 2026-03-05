# PoC-12: Jenkins 빌드 파이프라인 (Freestyle 빌드 단계)

## 목적

Jenkins Freestyle 프로젝트의 빌드 파이프라인 실행 순서를 Go로 시뮬레이션한다.
Trigger, BuildWrapper, Builder, Publisher(Recorder/Notifier), CheckPoint, BuildStepMonitor 등
빌드 실행에 관여하는 핵심 컴포넌트의 동작 원리와 실행 순서를 재현한다.

## 핵심 개념

### 1. 빌드 파이프라인 실행 순서

```
Trigger.run()
  │
  ▼
Queue.schedule(job)
  │
  ▼
┌─────────────────────────────────────────────────────┐
│ Build.BuildExecution.doRun()                        │
│                                                     │
│  1. preBuild(Builders)      ← prebuild 검증         │
│  2. preBuild(Publishers)    ← prebuild 검증         │
│  3. BuildWrapper.setUp()    ← 환경 설정 (순차)       │
│  4. Builder.perform()       ← 빌드 실행 (순차)       │
│     └─ 하나 실패 → 즉시 중단                         │
│                                                     │
│  [CheckPoint.MAIN_COMPLETED]                        │
│                                                     │
│ Build.BuildExecution.post2()                        │
│  5. Publisher.perform()     ← 빌드 후 처리           │
│     ├─ Recorder → 결과 기록 (빌드 결과 변경 가능)     │
│     ├─ Notifier → 외부 알림 (확정된 결과로 알림)      │
│     └─ 하나 실패해도 나머지 계속 실행                  │
│                                                     │
│ Build.BuildExecution.cleanUp()                      │
│  6. Publisher(afterFinalized) ← 빌드 완료 후 Publisher│
│  7. BuildWrapper.tearDown()   ← 환경 정리 (역순)     │
│     └─ 빌드 실패해도 반드시 실행                      │
└─────────────────────────────────────────────────────┘
```

### 2. BuildStep 계층 구조

```
BuildStep (interface)
  │  prebuild(), perform(), getRequiredMonitorService()
  │
  ├── BuildStepCompatibilityLayer (abstract class)
  │     │  < 1.150 플러그인 호환성 레이어
  │     │
  │     ├── Builder (abstract)
  │     │     getRequiredMonitorService() → NONE (기본값)
  │     │     예: Shell, BatchFile, Maven
  │     │
  │     └── Publisher (abstract)
  │           │  needsToRunAfterFinalized() → false (기본값)
  │           │
  │           ├── Recorder (abstract)
  │           │     빌드 결과에 영향 (정렬 우선순위: 0)
  │           │     예: JUnitResultArchiver, JacocoPublisher
  │           │
  │           └── Notifier (abstract)
  │                 외부 알림 전송 (정렬 우선순위: 2)
  │                 예: Mailer, SlackNotifier
  │
  └── BuildWrapper (별도 클래스, BuildStep 아님)
        setUp() → Environment, tearDown()
        예: TimestamperBuildWrapper, CredentialBinding
```

### 3. BuildStepMonitor (동시 빌드 동기화)

| 수준 | 동작 | 사용 시점 |
|------|------|----------|
| `NONE` | 동기화 없이 독립 실행 | Builder 기본값 (권장) |
| `STEP` | 이전 빌드의 같은 스텝 완료 대기 | 이전 빌드 같은 스텝 결과에 의존 시 |
| `BUILD` | 이전 빌드 전체 완료 대기 | Publisher/레거시 기본값 (가장 보수적) |

```
BuildStepMonitor.STEP 동작:
  CheckPoint cp = new CheckPoint(bs.getClass().getName());
  cp.block();                  // 이전 빌드의 같은 스텝 대기
  try {
      return bs.perform(...);  // 실행
  } finally {
      cp.report();             // 완료 알림 (후속 빌드 해제)
  }
```

### 4. CheckPoint (Barrier 패턴)

```
빌드 #1                          빌드 #2
  │                                │
  ├── Builder 실행                 ├── Builder 실행
  │                                │
  ├── CheckPoint.report()  ──────→ ├── CheckPoint.block() (대기 해제)
  │                                │
  ├── Publisher 실행               ├── Publisher 실행
  │                                │
```

미리 정의된 체크포인트:
- `COMPLETED`: 빌드 완전 완료
- `MAIN_COMPLETED`: Builder 완료, Publisher 진입
- `CULPRITS_DETERMINED`: 빌드 원인 분석 완료

### 5. Publisher 정렬

Publisher.DescriptorExtensionListImpl의 classify 메서드:
```
Recorder  → 0 (먼저 실행, 빌드 결과 변경 가능)
미분류     → 1
Notifier  → 2 (나중에 실행, 확정된 결과로 알림)
```

이를 통해 Recorder가 테스트 결과를 분석하여 UNSTABLE 등으로 결과를 변경한 뒤,
Notifier가 확정된 결과를 이메일/Slack 등으로 알림.

### 6. Builder vs Publisher 실패 처리 차이

| 구분 | Builder | Publisher |
|------|---------|-----------|
| 실패 시 동작 | 즉시 중단 (나머지 스킵) | 나머지 계속 실행 |
| 소스 위치 | `Build.BuildExecution.build()` | `AbstractRunner.performAllBuildSteps()` |
| 이유 | 다음 단계가 이전 결과에 의존 | 알림/기록은 독립적으로 수행 |

## 실제 Jenkins 소스 참조

| 파일 | 역할 |
|------|------|
| `core/src/main/java/hudson/tasks/BuildStep.java` | 빌드 단계 핵심 인터페이스 (prebuild/perform) |
| `core/src/main/java/hudson/tasks/BuildStepCompatibilityLayer.java` | < 1.150 하위 호환성 레이어 |
| `core/src/main/java/hudson/tasks/Builder.java` | 실제 빌드 작업 수행 (MonitorLevel 기본값: NONE) |
| `core/src/main/java/hudson/tasks/Publisher.java` | 빌드 후 처리, 정렬 메커니즘 (classify) |
| `core/src/main/java/hudson/tasks/Recorder.java` | 결과 기록 Publisher (정렬 우선순위: 0) |
| `core/src/main/java/hudson/tasks/Notifier.java` | 외부 알림 Publisher (정렬 우선순위: 2) |
| `core/src/main/java/hudson/tasks/BuildWrapper.java` | 빌드 환경 래핑 (setUp/tearDown) |
| `core/src/main/java/hudson/tasks/BuildStepMonitor.java` | NONE/STEP/BUILD 동시성 제어 |
| `core/src/main/java/hudson/model/CheckPoint.java` | 동시 빌드 간 Barrier 동기화 |
| `core/src/main/java/hudson/triggers/Trigger.java` | cron 기반 빌드 트리거 |
| `core/src/main/java/hudson/model/Build.java` | BuildExecution.doRun/post2/cleanUp |
| `core/src/main/java/hudson/model/AbstractBuild.java` | AbstractRunner.performAllBuildSteps/perform |

## 실행 방법

```bash
cd jenkins_EDU/poc-12-build-pipeline
go run main.go
```

## 예상 출력

1. Trigger 발동 확인 (TimerTrigger, SCMTrigger)
2. 전체 빌드 파이프라인 실행:
   - Phase 1: Prebuild 검증 (Builders + Publishers)
   - Phase 2: BuildWrapper setUp (환경변수 설정)
   - Phase 3: Builder 순차 실행 (Checkout → Compile → Test → Package)
   - Phase 4: Publisher 실행 (Recorder → Notifier 순서 보장)
   - Phase 5: CleanUp (BuildWrapper tearDown 역순 실행)
3. CheckPoint Barrier 동기화 데모 (동시 빌드 #1, #2)
4. Builder 실패 시나리오 (실패 즉시 중단, Publisher/tearDown은 실행 보장)
5. Publisher 정렬 순서 확인 (Recorder가 결과 변경 → Notifier가 확정된 결과 알림)
