# PoC-04: Node-Computer-Executor 3계층 시스템

## 목적

Jenkins의 빌드 실행 인프라 핵심인 **Node → Computer → Executor** 3계층 구조를 Go로 재현한다.
이 3계층은 "설정(Configuration)과 런타임(Runtime)의 분리"라는 Jenkins 설계 철학의 핵심이다.

## 핵심 개념

### Node-Computer-Executor 관계

```
┌─────────────────────────────────────────────────────────────────┐
│                         Jenkins                                 │
│                                                                 │
│  Node (설정 계층)              Computer (런타임 계층)              │
│  ┌────────────────────┐       ┌─────────────────────────────┐   │
│  │ name: "agent-01"   │       │ executors[]                 │   │
│  │ numExecutors: 3    │──1:1──│ ┌──────┐┌──────┐┌──────┐   │   │
│  │ labels: [java,linux]│setNode│ │Exec#0││Exec#1││Exec#2│   │   │
│  │ mode: NORMAL       │──────→│ │IDLE  ││BUSY  ││IDLE  │   │   │
│  │ description: "..." │       │ └──────┘└──────┘└──────┘   │   │
│  └────────────────────┘       │ isOnline: true              │   │
│                               │ offlineCause: null          │   │
│  ※ 사용자가 설정 변경 시       └─────────────────────────────┘   │
│    Node 재생성,                                                  │
│    Computer는 유지                                               │
│                                                                 │
│  Queue ──WorkUnit──→ Executor.start(WorkUnit)                   │
│              │        → run() → Executable 생성 → 빌드 실행       │
│              │        → finish1() → finish2()                    │
│              │              └→ owner.removeExecutor(this)        │
│              └──라벨 매칭──→ Node.canTake(item)                   │
└─────────────────────────────────────────────────────────────────┘
```

### 3계층 역할

| 계층 | 클래스 | 역할 | 생명주기 |
|------|--------|------|----------|
| **설정** | `Node` | 에이전트 설정 정보 (이름, Executor 수, 라벨) | 설정 변경 시 재생성 |
| **런타임** | `Computer` | Node의 실행 상태, Executor 풀 관리 | Node보다 오래 존속 가능 |
| **실행** | `Executor` | Thread(goroutine) 기반 빌드 실행 단위 | 작업 단위로 생성/소멸 |

### 설정-런타임 분리 원칙

Node 설정 변경 시 Computer의 동작 (`Computer.setNumExecutors()`):

```
numExecutors 증가 (3 → 5):
  → addNewExecutorIfNecessary()
  → 새 Executor 2개 즉시 생성

numExecutors 감소 (5 → 2):
  → idle Executor에 interrupt() 전송 → 즉시 제거
  → busy Executor는 완료까지 유지 (핵심!)
  → 빌드 완료 후 finish2() → removeExecutor()
```

### 라벨 매칭

```
Node.canTake(BuildableItem):
  1. 작업에 라벨 지정 + 노드에 해당 라벨 없음 → 거부
  2. 작업에 라벨 미지정 + 노드가 EXCLUSIVE 모드 → 거부
  3. 위 검사 통과 → 허용

NORMAL 모드:  라벨 없는 작업도 실행 가능
EXCLUSIVE 모드: 라벨을 명시한 작업만 실행
```

### RetentionStrategy

```
RetentionStrategy.Always:
  → 오프라인 감지 시 자동 reconnect
  → 전용 에이전트에 적합

RetentionStrategy.Demand:
  → idleDelay 초과 유휴 시 disconnect(IdleOfflineCause)
  → inDemandDelay 초과 수요 시 connect(false)
  → 클라우드 에이전트에 적합 (비용 절감)
```

## 실제 소스 참조

| 파일 | 핵심 내용 |
|------|----------|
| `core/src/main/java/hudson/model/Node.java` | 설정 계층, `canTake()`, `getNumExecutors()`, `Mode` enum |
| `core/src/main/java/hudson/model/Computer.java` | 런타임 계층, `setNumExecutors()`, `removeExecutor()`, `addNewExecutorIfNecessary()` |
| `core/src/main/java/hudson/model/Executor.java` | 실행 계층, `run()`, `start(WorkUnit)`, `interrupt()`, `finish1()`/`finish2()` |
| `core/src/main/java/hudson/slaves/RetentionStrategy.java` | `Always`, `Demand`, `check(Computer)` |
| `core/src/main/java/hudson/model/Label.java` | `contains(Node)`, `parse(String)` |
| `core/src/main/java/hudson/model/queue/WorkUnit.java` | Queue → Executor 전달 작업 단위 |

## 시뮬레이션 항목

| # | 시나리오 | 실제 Jenkins 메서드 |
|---|---------|-------------------|
| 1 | Node-Computer-Executor 생성 | `Node.createComputer()`, `addNewExecutorIfNecessary()` |
| 2 | 라벨 매칭 + 작업 할당 | `Node.canTake()`, `Executor.start(WorkUnit)` |
| 3 | NumExecutors 동적 변경 (증가/감소) | `Computer.setNumExecutors()`, `setNode()` |
| 4 | RetentionStrategy (Always/Demand) | `RetentionStrategy.check()` |
| 5 | NORMAL vs EXCLUSIVE 모드 | `Node.Mode`, `canTake()` 로직 |

## 실행 방법

```bash
cd jenkins_EDU/poc-04-executor
go run main.go
```

## 예상 출력

```
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  데모 1: Node-Computer-Executor 3계층 생성
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

[1] Node(설정) 생성:
  master               | executors=2 | labels=[master, linux] | mode=NORMAL
  agent-linux-01       | executors=3 | labels=[linux, docker, java] | mode=NORMAL
  agent-windows-01     | executors=2 | labels=[windows, dotnet] | mode=EXCLUSIVE

[2] Computer(런타임) 생성 및 Executor 풀 초기화:

[3] 전체 노드 상태:
  master       | ONLINE  | 라벨: [master, linux] | 모드: NORMAL    | Executor: 2개
    └─ Executor #0: IDLE         작업: (없음)
    └─ Executor #1: IDLE         작업: (없음)
  agent-linux-01 | ONLINE  | 라벨: [linux, docker, java] | 모드: NORMAL    | Executor: 3개
    └─ Executor #0: IDLE         작업: (없음)
    └─ Executor #1: IDLE         작업: (없음)
    └─ Executor #2: IDLE         작업: (없음)
  ...

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  데모 2: 라벨 매칭과 작업 할당
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  ...
  작업 'dotnet-test' (라벨: 'dotnet') 매칭:
    ✗ master               → 거부: 라벨 'dotnet' 없음 (보유: [master, linux])
    ✗ agent-linux-01       → 거부: 라벨 'dotnet' 없음 (보유: [linux, docker, java])
    ✓ agent-windows-01     → 매칭 성공
      → Executor #0에 할당 완료
  ...

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  데모 3: 설정-런타임 분리 — NumExecutors 동적 변경
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  ...
  [Computer:agent-linux-01] Executor 수 감소: 5 → 2 (초과 3개 제거 시작)
  [Computer:agent-linux-01] Idle Executor 3개 제거 완료
  ...

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  데모 4: RetentionStrategy — 유휴 에이전트 자동 오프라인
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  ...
  [RetentionStrategy:Demand] 'agent-windows-01' 유휴 300ms 초과 → 오프라인 전환
  [RetentionStrategy:Always] 'master' 오프라인 감지 → 재연결 시도
  ...

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  데모 5: Node.Mode (NORMAL vs EXCLUSIVE) 동작 비교
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  노드                 | 작업           | 요구 라벨           | 결과
  ----------------------------------------------------------------------
  normal-node        | 라벨-없는-작업     | (없음)            | 허용
  exclusive-node     | 라벨-없는-작업     | (없음)            | 거부: EXCLUSIVE 모드
  exclusive-node     | java-작업      | java            | 허용
```

## Go 매핑

| Jenkins (Java) | Go 시뮬레이션 | 설명 |
|----------------|-------------|------|
| `Node` (abstract class) | `Node` struct | 설정 정보 보유 |
| `Computer` (abstract class) | `Computer` struct | 런타임 상태 + Executor 풀 |
| `Executor extends Thread` | `Executor` + goroutine | `run()` 무한루프를 for-select로 대체 |
| `WorkUnit` | `WorkUnit` struct | Queue → Executor 작업 전달 단위 |
| `Thread.start()` | `go e.run()` + channel | goroutine + channel 기반 작업 수신 |
| `Thread.interrupt()` | `stopChan <- struct{}{}` | channel 기반 인터럽트 |
| `CopyOnWriteArrayList` | `[]*Executor` + `sync.RWMutex` | 동시성 안전 컬렉션 |
| `RetentionStrategy.check()` | `RetentionStrategy.Check()` | 인터페이스 기반 전략 패턴 |
| `Node.Mode` enum | `NodeMode` const | NORMAL / EXCLUSIVE |
