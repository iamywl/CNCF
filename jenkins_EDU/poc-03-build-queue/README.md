# PoC-03: Jenkins 빌드 큐 상태 머신 (Build Queue State Machine)

## 목적

Jenkins 빌드 큐(`Queue.java`, 약 3,252줄)의 4단계 상태 머신을 Go 표준 라이브러리만으로 시뮬레이션한다.
작업이 스케줄링되어 큐에 진입한 후, quiet period 대기 → 차단/빌드가능 판별 → Executor 할당 → 실행 시작까지의 전체 라이프사이클을 재현한다.

## 핵심 개념

### 1. 4개 컬렉션 기반 상태 머신

Jenkins Queue는 아이템이 속한 **컬렉션**으로 상태를 결정한다. 별도의 `state` 필드가 아니라, 어느 리스트에 들어있는지가 곧 상태이다.

```
                    ┌──────────────┐
                    │    enter     │  schedule2() 호출
                    └──────┬───────┘
                           │
                           v
                    ┌──────────────┐
                    │ waitingList  │  TreeSet<WaitingItem>
                    │ (quiet period│  timestamp 순 자동 정렬
                    │   대기)      │
                    └──────┬───────┘
                           │ quiet period 만료
              ┌────────────┴────────────┐
              │                         │
              v                         v
       ┌──────────────┐         ┌──────────────┐
       │blockedProjects│        │  buildables   │
       │ (차단 조건    │<─────>│ (즉시 빌드    │
       │   존재)      │ 양방향 │   가능)       │
       └──────────────┘  전이   └──────┬───────┘
       ItemList<BlockedItem>           │ LoadBalancer.map()
                                       v
                              ┌──────────────┐
                              │   pendings   │  ItemList<BuildableItem>
                              │ (Executor    │  isPending = true
                              │   할당됨)    │
                              └──────┬───────┘
                                     │ onStartExecuting()
                                     v
                              ┌──────────────┐
                              │  leftItems   │  Cache<Long, LeftItem>
                              │ (완료/취소)   │  5분 TTL
                              └──────────────┘
```

### 2. maintain() 5단계 루프

`maintain()`은 Queue의 심장으로, 모든 상태 전이가 여기서 실행된다:

| Phase | 설명 | 상태 전이 |
|-------|------|----------|
| Phase 1 | parked Executor 수집, lost pending 복구 | pending → buildable (Executor 소실 시) |
| Phase 2 | blocked → buildable 전이 | 차단 해제 확인, fatal이면 취소 |
| Phase 3 | waitingList → buildable/blocked 전이 | quiet period 만료 확인 |
| Phase 4 | QueueSorter 적용 | buildables 우선순위 정렬 |
| Phase 5 | buildables → Executor 할당 | LoadBalancer.map() → pending |

### 3. LoadBalancer

빌드 가능 아이템을 어떤 노드의 어떤 Executor에서 실행할지 결정하는 전략 패턴:
- `CONSISTENT_HASH`: Task의 affinityKey를 해싱하여 일관된 노드에 할당
- 같은 작업은 가능하면 같은 노드에서 실행 (빌드 캐시 활용)
- `MappingWorksheet`로 작업-Executor 매핑 문제를 정의하고 `assignGreedily()`로 탐욕적 할당

### 4. QueueSorter

빌드 가능 항목의 실행 순서를 결정하는 확장점:
- `sortBuildableItems()`: 우선순위 기반 정렬
- `DEFAULT_BLOCKED_ITEM_COMPARATOR`: `inQueueSince` 기준 정렬
- 플러그인(Priority Sorter Plugin 등)이 구현 가능

### 5. Snapshot 패턴

잠금 없이 일관된 큐 상태를 읽기 위한 패턴:
- 쓰기: `lock` 안에서 상태 변경 후 `updateSnapshot()` → 새 `Snapshot` 생성 → `volatile` 쓰기
- 읽기: `this.snapshot` (`volatile` 읽기) → 시점이 고정된 일관된 스냅샷 조회
- `try { try { ... } finally { updateSnapshot(); } } finally { lock.unlock(); }` 중첩 패턴

### 6. CauseOfBlockage

작업이 차단되는 4가지 원인:
1. `Task.getCauseOfBlockage()` -- 태스크 자체가 차단 보고
2. `ResourceActivity` -- 필요한 리소스가 다른 빌드에 점유됨
3. `QueueTaskDispatcher.canRun(Item)` -- 플러그인이 거부권 행사
4. 동시 빌드 불허 시 이미 buildables/pendings에 동일 Task 존재

## 실제 Jenkins 소스 참조

| 컴포넌트 | 실제 파일 | 설명 |
|----------|-----------|------|
| Queue | `core/src/main/java/hudson/model/Queue.java` | 빌드 큐 핵심 (3,252줄) |
| LoadBalancer | `core/src/main/java/hudson/model/LoadBalancer.java` | Executor 배치 전략 (CONSISTENT_HASH) |
| QueueSorter | `core/src/main/java/hudson/model/queue/QueueSorter.java` | 빌드 가능 항목 정렬 |
| MappingWorksheet | `core/src/main/java/hudson/model/queue/MappingWorksheet.java` | 작업-Executor 매핑 문제 정의 |
| QueueListener | `core/src/main/java/hudson/model/queue/QueueListener.java` | 큐 이벤트 리스너 |
| ScheduleResult | `core/src/main/java/hudson/model/queue/ScheduleResult.java` | 스케줄링 결과 (Created/Existing/Refused) |
| WorkUnitContext | `core/src/main/java/hudson/model/queue/WorkUnitContext.java` | WorkUnit 간 공유 컨텍스트 |
| Snapshot | `Queue.java` 3085~3103줄 | 4개 컬렉션의 불변 스냅샷 |

## 시뮬레이션 시나리오

| 시나리오 | 설명 |
|----------|------|
| 1 | 상태 머신 다이어그램 출력 (4단계 전이 + maintain 5단계) |
| 2 | 기본 상태 전이 (waiting → buildable → pending → left) |
| 3 | 차단과 해제 (CauseOfBlockage → blocked → 해제 → buildable) |
| 4 | QueueSorter 우선순위 정렬 (Phase 4) |
| 5 | 레이블 기반 Executor 매칭 (LoadBalancer + MappingWorksheet) |
| 6 | 중복 스케줄링 감지 & 취소 (scheduleInternal + cancel) |
| 7 | Snapshot 패턴 - 잠금 없는 동시 읽기 |
| 8 | 전체 라이프사이클 (maintain 루프 + 빌드 완료 + 차단 해제) |

## 실행 방법

```bash
cd jenkins_EDU/poc-03-build-queue
go run main.go
```

## 예상 출력

1. 상태 머신 다이어그램 (4개 컬렉션 + 전이 규칙)
2. maintain() 5단계 다이어그램
3. 작업 스케줄링 → quiet period 대기 → buildable 전이
4. Executor 할당 → pending → 실행 시작(left)
5. 차단 조건 설정/해제에 따른 상태 전이
6. 우선순위 기반 Executor 할당 순서
7. 레이블 매칭 성공/실패 (gpu 레이블 매칭 불가)
8. 중복 스케줄링 감지 및 작업 취소
9. 여러 goroutine에서의 동시 스냅샷 읽기
10. 전체 라이프사이클: maintain 루프 4라운드 실행

## 핵심 구현 매핑

| 이 PoC | 실제 Jenkins |
|--------|-------------|
| `QueueItem` | `Queue.Item` (abstract), `WaitingItem`, `BlockedItem`, `BuildableItem`, `LeftItem` |
| `Queue.waitingList` (slice, 정렬) | `TreeSet<WaitingItem>` (timestamp 순) |
| `Queue.blockedProjects` (slice) | `ItemList<BlockedItem>` (ArrayList 확장) |
| `Queue.buildables` (slice) | `ItemList<BuildableItem>` |
| `Queue.pendings` (slice) | `ItemList<BuildableItem>` (isPending=true) |
| `Queue.Maintain()` | `Queue.maintain()` (Phase 1~5) |
| `Queue.Schedule()` | `Queue.schedule2()` → `scheduleInternal()` |
| `Queue.Cancel()` | `Queue.cancel(Task)` (비트 OR 패턴) |
| `Queue.GetSnapshot()` | `Queue.getItems()` (volatile Snapshot 읽기) |
| `ConsistentHashLoadBalancer` | `LoadBalancer.CONSISTENT_HASH` |
| `PriorityQueueSorter` | `QueueSorter` (ExtensionPoint) |
| `QueueListener` | `QueueListener` (onEnterWaiting 등 7개 이벤트) |
| `CauseOfBlockage` | `CauseOfBlockage` (차단 원인 추상 클래스) |
| `ScheduleResult` | `ScheduleResult` (Created/Existing/Refused) |
| `sync.Mutex` | `ReentrantLock` |
| `atomic.Value` | `volatile Snapshot` |
