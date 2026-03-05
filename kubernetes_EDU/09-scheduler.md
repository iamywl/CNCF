# 09. 스케줄러 심화

## 목차

1. [개요](#1-개요)
2. [Scheduler 구조체](#2-scheduler-구조체)
3. [ScheduleOne 흐름](#3-scheduleone-흐름)
4. [Framework Extension Points](#4-framework-extension-points)
5. [SchedulingQueue](#5-schedulingqueue)
6. [병렬 필터 실행](#6-병렬-필터-실행)
7. [Preemption (PostFilter)](#7-preemption-postfilter)
8. [내장 플러그인](#8-내장-플러그인)
9. [스케줄링 알고리즘 상세](#9-스케줄링-알고리즘-상세)
10. [Binding Cycle](#10-binding-cycle)
11. [왜 이런 설계인가](#11-왜-이런-설계인가)
12. [정리](#12-정리)

---

## 1. 개요

Kubernetes 스케줄러는 미배정 Pod를 적절한 노드에 배치하는 컨트롤 플레인 컴포넌트다.
스케줄링은 크게 두 단계로 나뉜다:

1. **Scheduling Cycle** (동기): Pod에 적합한 노드를 찾고 선택
2. **Binding Cycle** (비동기): 선택된 노드에 Pod를 바인딩

스케줄러의 핵심 설계 원칙:

| 원칙 | 설명 |
|------|------|
| **플러그인 아키텍처** | 12개 확장 포인트, 모든 로직이 플러그인으로 구현 |
| **병렬 필터링** | 노드 필터링을 goroutine으로 병렬 실행 |
| **낙관적 바인딩** | Assume(가정) 후 비동기 바인딩 |
| **우선순위 큐** | 3개의 하위 큐로 효율적 스케줄링 순서 관리 |

**핵심 소스 파일:**

```
pkg/scheduler/
├── scheduler.go               # Scheduler 구조체, New()
├── schedule_one.go            # ScheduleOne, schedulePod, findNodesThatPassFilters
├── framework/
│   ├── interface.go           # Framework, Diagnosis
│   ├── plugins/
│   │   ├── registry.go        # NewInTreeRegistry (내장 플러그인 등록)
│   │   ├── noderesources/     # NodeResourcesFit 플러그인
│   │   ├── nodeaffinity/      # NodeAffinity 플러그인
│   │   ├── tainttoleration/   # TaintToleration 플러그인
│   │   └── ...
│   ├── runtime/               # frameworkImpl (Framework 구현)
│   └── parallelize/           # 병렬 실행 유틸리티
├── backend/
│   ├── queue/
│   │   └── scheduling_queue.go  # PriorityQueue (SchedulingQueue 구현)
│   └── cache/                 # 스케줄러 캐시
└── profile/                   # 스케줄링 프로필

staging/src/k8s.io/kube-scheduler/framework/
└── interface.go               # Plugin 인터페이스 정의 (PreFilter, Filter, Score 등)
```

---

## 2. Scheduler 구조체

### 2.1 구조체 정의

**파일:** `pkg/scheduler/scheduler.go` (68행~124행)

```go
// Scheduler watches for new unscheduled pods. It attempts to find
// nodes that they fit on and writes bindings back to the api server.
type Scheduler struct {
    // It is expected that changes made via Cache will be observed
    // by NodeLister and Algorithm.
    Cache internalcache.Cache

    Extenders []fwk.Extender

    // NextPod should be a function that blocks until the next pod
    // is available. We don't use a channel for this, because scheduling
    // a pod may take some amount of time and we don't want pods to get
    // stale while they sit in a channel.
    NextPod func(logger klog.Logger) (*framework.QueuedPodInfo, error)

    // FailureHandler is called upon a scheduling failure.
    FailureHandler FailureHandlerFn

    // SchedulePod tries to schedule the given pod to one of the nodes.
    SchedulePod func(ctx context.Context, fwk framework.Framework,
        state fwk.CycleState, podInfo *framework.QueuedPodInfo) (ScheduleResult, error)

    // Close this to shut down the scheduler.
    StopEverything <-chan struct{}

    // SchedulingQueue holds pods to be scheduled
    SchedulingQueue internalqueue.SchedulingQueue

    // APIDispatcher for async API calls (nil if feature gate disabled)
    APIDispatcher *apidispatcher.APIDispatcher

    // WorkloadManager for workload-aware scheduling
    WorkloadManager internalworkloadmanager.WorkloadManager

    // Profiles are the scheduling profiles.
    Profiles profile.Map

    client clientset.Interface

    nodeInfoSnapshot *internalcache.Snapshot

    percentageOfNodesToScore int32

    nextStartNodeIndex int

    logger klog.Logger

    registeredHandlers []cache.ResourceEventHandlerRegistration

    nominatedNodeNameForExpectationEnabled bool
}
```

### 2.2 핵심 필드 분석

| 필드 | 타입 | 역할 |
|------|------|------|
| `Cache` | `internalcache.Cache` | 노드/Pod 정보 인메모리 캐시 |
| `NextPod` | `func() (*QueuedPodInfo, error)` | 다음 스케줄링할 Pod 반환 (블로킹) |
| `SchedulePod` | `func(...)  (ScheduleResult, error)` | 스케줄링 알고리즘 실행 |
| `FailureHandler` | `FailureHandlerFn` | 스케줄링 실패 처리 |
| `SchedulingQueue` | `SchedulingQueue` | Pod 대기 큐 (PriorityQueue) |
| `Profiles` | `profile.Map` | 스케줄링 프로필별 Framework 매핑 |
| `nodeInfoSnapshot` | `*Snapshot` | 노드 정보 스냅샷 (스케줄링 사이클마다 갱신) |
| `percentageOfNodesToScore` | `int32` | 점수 계산할 노드 비율 |
| `nextStartNodeIndex` | `int` | 라운드 로빈 시작 인덱스 |
| `Extenders` | `[]fwk.Extender` | 외부 스케줄러 확장 |

### 2.3 ScheduleResult

**파일:** `pkg/scheduler/scheduler.go` (152행~163행)

```go
type ScheduleResult struct {
    // Name of the selected node.
    SuggestedHost string
    // The number of nodes the scheduler evaluated in the filtering phase.
    EvaluatedNodes int
    // The number of nodes that fit the pod.
    FeasibleNodes int
    // The nominating info for scheduling cycle.
    nominatingInfo *fwk.NominatingInfo
}
```

### 2.4 기본 옵션

**파일:** `pkg/scheduler/scheduler.go` (261행~273행)

```go
var defaultSchedulerOptions = schedulerOptions{
    clock:                             clock.RealClock{},
    percentageOfNodesToScore:          schedulerapi.DefaultPercentageOfNodesToScore,
    podInitialBackoffSeconds:          1,   // 초기 백오프: 1초
    podMaxBackoffSeconds:              10,  // 최대 백오프: 10초
    podMaxInUnschedulablePodsDuration: internalqueue.DefaultPodMaxInUnschedulablePodsDuration,
    parallelism:                       int32(parallelize.DefaultParallelism),  // 기본 16
    applyDefaultProfile:               true,
}
```

### 2.5 Scheduler 생성

**파일:** `pkg/scheduler/scheduler.go` (276행~299행)

```go
func New(ctx context.Context,
    client clientset.Interface,
    informerFactory informers.SharedInformerFactory,
    dynInformerFactory dynamicinformer.DynamicSharedInformerFactory,
    recorderFactory profile.RecorderFactory,
    opts ...Option) (*Scheduler, error) {

    logger := klog.FromContext(ctx)
    stopEverything := ctx.Done()

    options := defaultSchedulerOptions
    for _, opt := range opts {
        opt(&options)
    }

    // 기본 프로필 적용
    if options.applyDefaultProfile {
        var versionedCfg configv1.KubeSchedulerConfiguration
        scheme.Scheme.Default(&versionedCfg)
        cfg := schedulerapi.KubeSchedulerConfiguration{}
        scheme.Scheme.Convert(&versionedCfg, &cfg, nil)
        options.profiles = cfg.Profiles
    }
    // ... 이후 Framework 생성, Queue 생성, Cache 생성 등
}
```

### 2.6 기본 핸들러 설정

**파일:** `pkg/scheduler/scheduler.go` (126행~129행)

```go
func (sched *Scheduler) applyDefaultHandlers() {
    sched.SchedulePod = sched.schedulePod
    sched.FailureHandler = sched.handleSchedulingFailure
}
```

---

## 3. ScheduleOne 흐름

### 3.1 ScheduleOne 메서드

**파일:** `pkg/scheduler/schedule_one.go` (66행~95행)

```go
// ScheduleOne does the entire scheduling workflow for a single scheduling entity.
// It is serialized on the scheduling algorithm's host fitting.
func (sched *Scheduler) ScheduleOne(ctx context.Context) {
    logger := klog.FromContext(ctx)

    // 1. 다음 Pod 가져오기 (블로킹)
    podInfo, err := sched.NextPod(logger)
    if err != nil {
        utilruntime.HandleErrorWithLogger(logger, err,
            "Error while retrieving next pod from scheduling queue")
        return
    }
    if podInfo == nil || podInfo.Pod == nil {
        return
    }

    // 2. Pod Group 스케줄링 여부 확인
    if podInfo.NeedsPodGroupScheduling {
        podGroupInfo, err := sched.podGroupInfoForPod(ctx, podInfo)
        if err != nil {
            // ...
            return
        }
        sched.scheduleOnePodGroup(ctx, podGroupInfo)
    } else {
        sched.scheduleOnePod(ctx, podInfo)
    }
}
```

### 3.2 scheduleOnePod 메서드

**파일:** `pkg/scheduler/schedule_one.go` (98행~147행)

```go
func (sched *Scheduler) scheduleOnePod(ctx context.Context,
    podInfo *framework.QueuedPodInfo) {
    logger := klog.FromContext(ctx)
    pod := podInfo.Pod
    logger = klog.LoggerWithValues(logger, "pod", klog.KObj(pod))
    ctx = klog.NewContext(ctx, logger)

    // 1. 해당 Pod에 대한 Framework 조회 (프로필별)
    fwk, err := sched.frameworkForPod(pod)
    if err != nil {
        sched.SchedulingQueue.Done(pod.UID)
        return
    }

    // 2. Pod 스킵 여부 확인 (삭제 중이거나 이미 assumed)
    if sched.skipPodSchedule(ctx, fwk, pod) {
        sched.SchedulingQueue.Done(pod.UID)
        return
    }

    // 3. CycleState 초기화
    start := time.Now()
    state := framework.NewCycleState()
    // 10%만 플러그인 메트릭 샘플링
    state.SetRecordPluginMetrics(rand.Intn(100) < pluginMetricsSamplePercent)

    // PodsToActivate 초기화
    podsToActivate := framework.NewPodsToActivate()
    state.Write(framework.PodsToActivateKey, podsToActivate)

    schedulingCycleCtx, cancel := context.WithCancel(ctx)
    defer cancel()

    // 4. Scheduling Cycle 실행 (동기)
    scheduleResult, assumedPodInfo, status := sched.schedulingCycle(
        schedulingCycleCtx, state, fwk, podInfo, start, podsToActivate)
    if !status.IsSuccess() {
        sched.FailureHandler(schedulingCycleCtx, fwk, assumedPodInfo,
            status, scheduleResult.nominatingInfo, start)
        return
    }

    // 5. Binding Cycle 실행 (비동기)
    go sched.runBindingCycle(ctx, state, fwk, scheduleResult,
        assumedPodInfo, start, podsToActivate)
}
```

### 3.3 전체 흐름 다이어그램

```
┌─────────────────────────────────────────────────────────────────┐
│                      ScheduleOne()                               │
│                                                                 │
│  ┌──────────────────────────────────────────┐                   │
│  │ 1. NextPod() ← SchedulingQueue.Pop()     │  블로킹 대기      │
│  └────────────────────┬─────────────────────┘                   │
│                       │                                         │
│                       ▼                                         │
│  ┌──────────────────────────────────────────┐                   │
│  │ 2. frameworkForPod(pod)                   │  프로필 조회      │
│  │    Profiles[pod.Spec.SchedulerName]       │                   │
│  └────────────────────┬─────────────────────┘                   │
│                       │                                         │
│                       ▼                                         │
│  ┌──────────────────────────────────────────┐                   │
│  │ 3. skipPodSchedule()                      │  스킵 체크       │
│  │    - 삭제 중인 Pod                         │                   │
│  │    - 이미 assumed된 Pod                    │                   │
│  └────────────────────┬─────────────────────┘                   │
│                       │                                         │
│  ═════════════════════╪═══════════════════════                   │
│  ║  Scheduling Cycle  ║  (동기, 직렬)          ║                  │
│  ═════════════════════╪═══════════════════════                   │
│                       │                                         │
│                       ▼                                         │
│  ┌──────────────────────────────────────────┐                   │
│  │ 4. schedulingCycle()                      │                   │
│  │    │                                      │                   │
│  │    ├── schedulingAlgorithm()              │                   │
│  │    │   ├── SchedulePod()                  │                   │
│  │    │   │   ├── PreFilter                  │                   │
│  │    │   │   ├── Filter (병렬)              │                   │
│  │    │   │   ├── PreScore                   │                   │
│  │    │   │   └── Score                      │                   │
│  │    │   │                                  │                   │
│  │    │   └── [실패 시] PostFilter (선점)     │                   │
│  │    │                                      │                   │
│  │    └── prepareForBindingCycle()           │                   │
│  │        ├── Assume (캐시에 가정)           │                   │
│  │        ├── Reserve                        │                   │
│  │        └── Permit                         │                   │
│  └────────────────────┬─────────────────────┘                   │
│                       │                                         │
│  ═════════════════════╪═══════════════════════                   │
│  ║  Binding Cycle     ║  (비동기, goroutine)   ║                  │
│  ═════════════════════╪═══════════════════════                   │
│                       │                                         │
│                       ▼                                         │
│  ┌──────────────────────────────────────────┐                   │
│  │ 5. runBindingCycle() (go goroutine)       │                   │
│  │    ├── WaitOnPermit                       │                   │
│  │    ├── PreBind                            │                   │
│  │    ├── Bind                               │                   │
│  │    └── PostBind                           │                   │
│  └──────────────────────────────────────────┘                   │
└─────────────────────────────────────────────────────────────────┘
```

### 3.4 schedulingCycle

**파일:** `pkg/scheduler/schedule_one.go` (173행~193행)

```go
func (sched *Scheduler) schedulingCycle(
    ctx context.Context,
    state fwk.CycleState,
    schedFramework framework.Framework,
    podInfo *framework.QueuedPodInfo,
    start time.Time,
    podsToActivate *framework.PodsToActivate,
) (ScheduleResult, *framework.QueuedPodInfo, *fwk.Status) {

    // 1. 스케줄링 알고리즘 실행 (Filter + Score)
    scheduleResult, status := sched.schedulingAlgorithm(ctx, state,
        schedFramework, podInfo, start)
    if !status.IsSuccess() {
        return scheduleResult, podInfo, status
    }

    // 2. 바인딩 사이클 준비 (Assume + Reserve + Permit)
    assumedPodInfo, status := sched.prepareForBindingCycle(ctx, state,
        schedFramework, podInfo, podsToActivate, scheduleResult)
    if !status.IsSuccess() {
        return ScheduleResult{nominatingInfo: clearNominatedNode},
            assumedPodInfo, status
    }

    return scheduleResult, assumedPodInfo, nil
}
```

### 3.5 schedulingAlgorithm

**파일:** `pkg/scheduler/schedule_one.go` (249행~303행)

```go
func (sched *Scheduler) schedulingAlgorithm(
    ctx context.Context,
    state fwk.CycleState,
    schedFramework framework.Framework,
    podInfo *framework.QueuedPodInfo,
    start time.Time,
) (ScheduleResult, *fwk.Status) {
    defer func() {
        metrics.SchedulingAlgorithmLatency.Observe(metrics.SinceInSeconds(start))
    }()

    pod := podInfo.Pod

    // 1. SchedulePod (Filter + Score)
    scheduleResult, err := sched.SchedulePod(ctx, schedFramework, state, podInfo)
    if err != nil {
        if err == ErrNoNodesAvailable {
            status := fwk.NewStatus(fwk.UnschedulableAndUnresolvable).WithError(err)
            return ScheduleResult{nominatingInfo: clearNominatedNode}, status
        }

        fitError, ok := err.(*framework.FitError)
        if !ok {
            return ScheduleResult{nominatingInfo: clearNominatedNode}, fwk.AsStatus(err)
        }

        // 2. Filter 실패 시 PostFilter(선점) 시도
        if !schedFramework.HasPostFilterPlugins() {
            logger.V(3).Info("No PostFilter plugins are registered, " +
                "so no preemption will be performed")
            return ScheduleResult{nominatingInfo: clearNominatedNode},
                fwk.NewStatus(fwk.Unschedulable).WithError(err)
        }

        // 3. PostFilter 플러그인 실행 (선점)
        result, status := schedFramework.RunPostFilterPlugins(ctx, state, pod,
            fitError.Diagnosis.NodeToStatus)
        msg := status.Message()
        fitError.Diagnosis.PostFilterMsg = msg

        var nominatingInfo *fwk.NominatingInfo
        if result != nil {
            nominatingInfo = result.NominatingInfo
        }
        return ScheduleResult{nominatingInfo: nominatingInfo},
            fwk.NewStatus(fwk.Unschedulable).WithError(err)
    }
    return scheduleResult, nil
}
```

---

## 4. Framework Extension Points

### 4.1 12개 확장 포인트

Kubernetes 스케줄러 프레임워크는 12개의 확장 포인트를 제공한다.
각 확장 포인트는 독립적인 Plugin 인터페이스로 정의된다.

**파일:** `staging/src/k8s.io/kube-scheduler/framework/interface.go`

```
┌─────────────────────────────────────────────────────────────────┐
│                    Scheduling Framework                          │
│                                                                 │
│  ┌──────────────────────────────────────────────────┐           │
│  │              Scheduling Cycle (동기)              │           │
│  │                                                  │           │
│  │  1. QueueSort                                    │           │
│  │     │  Pod 우선순위 결정                           │           │
│  │     ▼                                            │           │
│  │  2. PreFilter                                    │           │
│  │     │  전처리 (리소스 계산, 노드 사전 필터)        │           │
│  │     ▼                                            │           │
│  │  3. Filter (병렬)                                │           │
│  │     │  각 노드에 Pod 배치 가능 여부 판단           │           │
│  │     ▼                                            │           │
│  │  4. PostFilter                                   │           │
│  │     │  Filter 실패 시 선점(Preemption) 시도        │           │
│  │     ▼                                            │           │
│  │  5. PreScore                                     │           │
│  │     │  점수 계산 전처리                            │           │
│  │     ▼                                            │           │
│  │  6. Score                                        │           │
│  │     │  각 노드에 점수 부여                         │           │
│  │     ▼                                            │           │
│  │  7. NormalizeScore                               │           │
│  │     │  점수 정규화 (0~100)                        │           │
│  │     ▼                                            │           │
│  │  8. Reserve                                      │           │
│  │     │  리소스 예약 (낙관적)                        │           │
│  │     ▼                                            │           │
│  │  9. Permit                                       │           │
│  │     │  최종 승인/대기/거부                         │           │
│  └──────────────────────────────────────────────────┘           │
│                                                                 │
│  ┌──────────────────────────────────────────────────┐           │
│  │              Binding Cycle (비동기)               │           │
│  │                                                  │           │
│  │  10. WaitOnPermit                                │           │
│  │      │  Permit에서 대기 중인 Pod 처리              │           │
│  │      ▼                                           │           │
│  │  11. PreBind                                     │           │
│  │      │  바인딩 전 준비 (볼륨 프로비저닝 등)        │           │
│  │      ▼                                           │           │
│  │  12. Bind                                        │           │
│  │      │  API Server에 바인딩 요청                   │           │
│  │      ▼                                           │           │
│  │  13. PostBind                                    │           │
│  │      │  바인딩 후 정리                             │           │
│  └──────────────────────────────────────────────────┘           │
└─────────────────────────────────────────────────────────────────┘
```

### 4.2 Plugin 인터페이스 정의

**파일:** `staging/src/k8s.io/kube-scheduler/framework/interface.go`

#### PreFilterPlugin (476행)

```go
type PreFilterPlugin interface {
    Plugin
    // PreFilter is called at the beginning of the scheduling cycle.
    // Can return a PreFilterResult to influence which nodes to evaluate.
    PreFilter(ctx context.Context, state CycleState, p *v1.Pod,
        nodes []NodeInfo) (*PreFilterResult, *Status)
    PreFilterExtensions() PreFilterExtensions
}
```

#### FilterPlugin (505행)

```go
type FilterPlugin interface {
    Plugin
    // Filter is called by the scheduling framework.
    // Must return "Success" for nodes that can run the pod.
    Filter(ctx context.Context, state CycleState, pod *v1.Pod,
        nodeInfo NodeInfo) *Status
}
```

#### PostFilterPlugin (534행)

```go
type PostFilterPlugin interface {
    Plugin
    // PostFilter is called when no node passes the filter phase.
    // Used for preemption.
    PostFilter(ctx context.Context, state CycleState, pod *v1.Pod,
        filteredNodeStatusMap NodeToStatusReader) (*PostFilterResult, *Status)
}
```

#### PreScorePlugin (561행)

```go
type PreScorePlugin interface {
    Plugin
    // PreScore is called with a list of nodes that passed filtering.
    PreScore(ctx context.Context, state CycleState, pod *v1.Pod,
        nodes []NodeInfo) *Status
}
```

#### ScorePlugin (582행)

```go
type ScorePlugin interface {
    Plugin
    // Score is called on each filtered node. Returns an integer rank.
    Score(ctx context.Context, state CycleState, p *v1.Pod,
        nodeInfo NodeInfo) (int64, *Status)
    ScoreExtensions() ScoreExtensions
}
```

#### ReservePlugin (599행)

```go
type ReservePlugin interface {
    Plugin
    // Reserve is called when the scheduler cache is updated.
    Reserve(ctx context.Context, state CycleState, p *v1.Pod,
        nodeName string) *Status
    // Unreserve is called when a reserved pod is rejected.
    Unreserve(ctx context.Context, state CycleState, p *v1.Pod,
        nodeName string)
}
```

#### PreBindPlugin (615행)

```go
type PreBindPlugin interface {
    Plugin
    PreBindPreFlight(ctx context.Context, state CycleState, p *v1.Pod,
        nodeName string) (*PreBindPreFlightResult, *Status)
    PreBind(ctx context.Context, state CycleState, p *v1.Pod,
        nodeName string) *Status
}
```

#### PostBindPlugin (632행)

```go
type PostBindPlugin interface {
    Plugin
    PostBind(ctx context.Context, state CycleState, p *v1.Pod,
        nodeName string)
}
```

#### PermitPlugin (643행)

```go
type PermitPlugin interface {
    Plugin
    // Permit is called before binding. Can approve, deny, or wait.
    Permit(ctx context.Context, state CycleState, p *v1.Pod,
        nodeName string) (*Status, time.Duration)
}
```

#### BindPlugin (656행)

```go
type BindPlugin interface {
    Plugin
    // Bind plugins are called until one handles the pod.
    // Must return Skip if not handling.
    Bind(ctx context.Context, state CycleState, p *v1.Pod,
        nodeName string) *Status
}
```

### 4.3 확장 포인트별 실패 처리

| 확장 포인트 | 실패 시 동작 |
|-------------|-------------|
| PreFilter | Pod 전체 스케줄링 실패 → PostFilter 시도 |
| Filter | 해당 노드만 제외 → 다른 노드 시도 |
| PostFilter | 선점 실패 → unschedulablePods로 이동 |
| PreScore | 전체 실패 → 에러 |
| Score | 전체 실패 → 에러 |
| Reserve | Unreserve 호출 후 실패 |
| Permit | Wait/Deny → 거부 시 Unreserve |
| PreBind | 바인딩 실패 → Unreserve + 재시도 |
| Bind | 바인딩 실패 → Unreserve + 재시도 |
| PostBind | 실패해도 계속 (정보성) |

---

## 5. SchedulingQueue

### 5.1 SchedulingQueue 인터페이스

**파일:** `pkg/scheduler/backend/queue/scheduling_queue.go` (94행~148행)

```go
type SchedulingQueue interface {
    fwk.PodNominator
    Add(logger klog.Logger, pod *v1.Pod)
    Activate(logger klog.Logger, pods map[string]*v1.Pod)
    AddUnschedulableIfNotPresent(logger klog.Logger,
        pod *framework.QueuedPodInfo, podSchedulingCycle int64) error
    SchedulingCycle() int64
    Pop(logger klog.Logger) (*framework.QueuedPodInfo, error)
    PopSpecificPod(logger klog.Logger, pod *v1.Pod) *framework.QueuedPodInfo
    Done(types.UID)
    Update(logger klog.Logger, oldPod, newPod *v1.Pod)
    Delete(pod *v1.Pod)
    MoveAllToActiveOrBackoffQueue(logger klog.Logger, event fwk.ClusterEvent,
        oldObj, newObj interface{}, preCheck PreEnqueueCheck)
    AssignedPodAdded(logger klog.Logger, pod *v1.Pod)
    AssignedPodUpdated(logger klog.Logger, oldPod, newPod *v1.Pod, event fwk.ClusterEvent)
    Close()
    Run(logger klog.Logger)
}
```

### 5.2 PriorityQueue 구조체

**파일:** `pkg/scheduler/backend/queue/scheduling_queue.go` (167행~215행)

```go
// PriorityQueue implements a scheduling queue.
// The head of PriorityQueue is the highest priority pending pod.
type PriorityQueue struct {
    *nominator

    stop  chan struct{}
    clock clock.WithTicker

    // lock takes precedence and should be taken first.
    // Correct locking order: lock > activeQueue.lock > backoffQueue.lock > nominator.nLock
    lock sync.RWMutex

    // the maximum time a pod can stay in the unschedulablePods
    podMaxInUnschedulablePodsDuration time.Duration

    activeQ  activeQueuer     // 즉시 스케줄링 대상
    backoffQ backoffQueuer    // 백오프 대기 중
    // unschedulablePods holds pods that have been tried and determined unschedulable.
    unschedulablePods *unschedulablePods

    // moveRequestCycle caches the sequence number when we received a move request.
    moveRequestCycle int64

    // preEnqueuePluginMap: profile+plugin → PreEnqueuePlugin
    preEnqueuePluginMap map[string]map[string]fwk.PreEnqueuePlugin
    // queueingHintMap: profile → QueueingHintFunction
    queueingHintMap QueueingHintMapPerProfile
    // pluginToEventsMap: plugin → interested events
    pluginToEventsMap map[string][]fwk.ClusterEvent

    nsLister listersv1.NamespaceLister

    metricsRecorder *metrics.MetricAsyncRecorder
    pluginMetricsSamplePercent int

    apiDispatcher fwk.APIDispatcher

    isSchedulingQueueHintEnabled bool
    isPopFromBackoffQEnabled     bool
    isGenericWorkloadEnabled     bool
}
```

### 5.3 3-큐 아키텍처

```
┌─────────────────────────────────────────────────────────────────┐
│                      PriorityQueue                               │
│                                                                 │
│  ┌──────────────────────────────────────────┐                   │
│  │            activeQ (힙)                   │                   │
│  │  - 즉시 스케줄링 가능한 Pod                │                   │
│  │  - QueueSort 플러그인으로 우선순위 결정     │                   │
│  │  - Pop()으로 최우선 Pod 반환               │                   │
│  │                                          │                   │
│  │  [Pod A (priority=100)]  ← Pop() 대상     │                   │
│  │  [Pod B (priority=50)]                    │                   │
│  │  [Pod C (priority=10)]                    │                   │
│  └──────────────────────────────────────────┘                   │
│                                                                 │
│  ┌──────────────────────────────────────────┐                   │
│  │            backoffQ (힙)                  │                   │
│  │  - 백오프 기간 대기 중인 Pod               │                   │
│  │  - 백오프 만료 시 activeQ로 이동           │                   │
│  │  - 초기 1초, 최대 10초 (지수 백오프)       │                   │
│  │                                          │                   │
│  │  [Pod D (backoff expires: 2s)]           │                   │
│  │  [Pod E (backoff expires: 4s)]           │                   │
│  └──────────────────────────────────────────┘                   │
│                                                                 │
│  ┌──────────────────────────────────────────┐                   │
│  │       unschedulablePods (맵)              │                   │
│  │  - 현재 스케줄링 불가능한 Pod              │                   │
│  │  - 클러스터 변경 이벤트 발생 시             │                   │
│  │    backoffQ 또는 activeQ로 이동           │                   │
│  │  - 최대 체류 시간 후 activeQ로 이동        │                   │
│  │                                          │                   │
│  │  [Pod F: 노드 리소스 부족]                │                   │
│  │  [Pod G: 볼륨 제약]                       │                   │
│  └──────────────────────────────────────────┘                   │
└─────────────────────────────────────────────────────────────────┘
```

### 5.4 Pod 이동 흐름

```
새 Pod 도착
    │
    ▼
┌──────────┐
│ activeQ  │◀──── 백오프 만료 ───── backoffQ
│          │◀──── 클러스터 변경 ─── unschedulablePods
│          │◀──── Activate() ────── unschedulablePods/backoffQ
└────┬─────┘
     │ Pop()
     ▼
스케줄링 시도
     │
     ├── 성공 → Binding Cycle → Done()
     │
     └── 실패
         │
         ├── Unschedulable
         │   └── unschedulablePods에 추가
         │       (QueueingHint가 있으면 관련 이벤트만 감시)
         │
         └── Error / BackoffQ
             └── backoffQ에 추가
                 (지수 백오프: 1s, 2s, 4s, 8s, 10s)
```

### 5.5 QueueSort 플러그인

기본 정렬 기준: Pod Priority → 생성 시간

```go
// queuesort/priority_sort.go
func (pl *PrioritySort) Less(pInfo1, pInfo2 *framework.QueuedPodInfo) bool {
    p1 := corev1helpers.PodPriority(pInfo1.Pod)
    p2 := corev1helpers.PodPriority(pInfo2.Pod)
    return (p1 > p2) || (p1 == p2 && pInfo1.Timestamp.Before(pInfo2.Timestamp))
}
```

### 5.6 MoveAllToActiveOrBackoffQueue

클러스터 상태 변경(노드 추가, Pod 삭제 등) 시 unschedulablePods의 Pod를
activeQ 또는 backoffQ로 이동시키는 메서드:

```go
MoveAllToActiveOrBackoffQueue(logger, event, oldObj, newObj, preCheck)
```

**QueueingHint**: 각 플러그인이 "이 이벤트가 내 Pod를 스케줄 가능하게 만들 수 있는가?"에 대한
힌트를 제공한다. 힌트가 "No"이면 불필요한 재시도를 방지한다.

---

## 6. 병렬 필터 실행

### 6.1 findNodesThatPassFilters

**파일:** `pkg/scheduler/schedule_one.go` (768행~851행)

```go
func (sched *Scheduler) findNodesThatPassFilters(
    ctx context.Context,
    schedFramework framework.Framework,
    state fwk.CycleState,
    pod *v1.Pod,
    diagnosis *framework.Diagnosis,
    nodes []fwk.NodeInfo) ([]fwk.NodeInfo, error) {

    numAllNodes := len(nodes)
    numNodesToFind := sched.numFeasibleNodesToFind(
        schedFramework.PercentageOfNodesToScore(), int32(numAllNodes))

    // 스코어링이 없으면 하나만 찾으면 됨
    if !sched.hasExtenderFilters() && !sched.hasScoring(schedFramework) {
        numNodesToFind = 1
    }

    // 결과 슬라이스 사전 할당
    feasibleNodes := make([]fwk.NodeInfo, numNodesToFind)

    // Filter 플러그인이 없으면 바로 반환
    if !schedFramework.HasFilterPlugins() {
        for i := range feasibleNodes {
            feasibleNodes[i] = nodes[(sched.nextStartNodeIndex+i)%numAllNodes]
        }
        return feasibleNodes, nil
    }

    errCh := parallelize.NewResultChannel[error]()
    var feasibleNodesLen int32
    ctx, cancel := context.WithCancelCause(ctx)
    defer cancel(errors.New("findNodesThatPassFilters has completed"))

    // 각 노드에 대한 필터 실행 함수
    checkNode := func(i int) {
        // 라운드 로빈: 이전 사이클의 다음 노드부터 시작
        nodeInfo := nodes[(sched.nextStartNodeIndex+i)%numAllNodes]

        // Filter 플러그인 실행 (Nominated Pod 포함)
        status := schedFramework.RunFilterPluginsWithNominatedPods(
            ctx, state, pod, nodeInfo)

        if status.Code() == fwk.Error {
            errCh.SendWithCancel(status.AsError(), func() {
                cancel(errors.New("some other Filter operation failed"))
            })
            return
        }

        if status.IsSuccess() {
            // 충분한 노드를 찾으면 context 취소로 나머지 중단
            length := atomic.AddInt32(&feasibleNodesLen, 1)
            if length > numNodesToFind {
                cancel(errors.New("enough nodes found"))
                atomic.AddInt32(&feasibleNodesLen, -1)
            } else {
                feasibleNodes[length-1] = nodeInfo
            }
        } else {
            result[i] = &nodeStatus{node: nodeInfo.Node().Name, status: status}
        }
    }

    // 병렬 실행 (기본 parallelism=16)
    schedFramework.Parallelizer().Until(ctx, numAllNodes, checkNode, metrics.Filter)
    feasibleNodes = feasibleNodes[:feasibleNodesLen]

    // 실패한 노드의 상태 기록
    for _, item := range result {
        if item == nil {
            continue
        }
        diagnosis.NodeToStatus.Set(item.node, item.status)
        diagnosis.AddPluginStatus(item.status)
    }

    return feasibleNodes, nil
}
```

### 6.2 atomic.AddInt32 기반 동시성 제어

```
┌─────────────────────────────────────────────────────────────────┐
│              병렬 필터 실행 (parallelism=16)                      │
│                                                                 │
│  goroutine-1: checkNode(0)   goroutine-2: checkNode(1)         │
│  goroutine-3: checkNode(2)   goroutine-4: checkNode(3)         │
│  ...                                                           │
│  goroutine-16: checkNode(15)                                   │
│                                                                 │
│  각 goroutine:                                                  │
│    RunFilterPlugins(pod, nodeInfo)                              │
│        │                                                       │
│        ├── Success:                                             │
│        │   length := atomic.AddInt32(&feasibleNodesLen, 1)     │
│        │   if length > numNodesToFind:                         │
│        │       cancel() ← context 취소로 나머지 goroutine 중단  │
│        │   else:                                               │
│        │       feasibleNodes[length-1] = nodeInfo              │
│        │                                                       │
│        └── Failure:                                             │
│            result[i] = {node, status}                          │
└─────────────────────────────────────────────────────────────────┘
```

### 6.3 핵심 상수

**파일:** `pkg/scheduler/schedule_one.go` (49행~62행)

```go
const (
    // pluginMetricsSamplePercent: 메트릭 샘플링 비율
    pluginMetricsSamplePercent = 10

    // minFeasibleNodesToFind: 최소 스코어링 노드 수
    minFeasibleNodesToFind = 100

    // minFeasibleNodesPercentageToFind: 최소 스코어링 노드 비율
    minFeasibleNodesPercentageToFind = 5
)
```

### 6.4 numFeasibleNodesToFind

**파일:** `pkg/scheduler/schedule_one.go` (855행~881행)

```go
func (sched *Scheduler) numFeasibleNodesToFind(
    percentageOfNodesToScore *int32, numAllNodes int32) (numNodes int32) {

    if numAllNodes < minFeasibleNodesToFind {
        return numAllNodes  // 100개 미만이면 전부 평가
    }

    var percentage int32
    if percentageOfNodesToScore != nil {
        percentage = *percentageOfNodesToScore
    } else {
        percentage = sched.percentageOfNodesToScore
    }

    // 적응형 비율: 50 - (노드 수)/125
    if percentage == 0 {
        percentage = int32(50) - numAllNodes/125
        if percentage < minFeasibleNodesPercentageToFind {
            percentage = minFeasibleNodesPercentageToFind  // 최소 5%
        }
    }

    numNodes = numAllNodes * percentage / 100
    if numNodes < minFeasibleNodesToFind {
        return minFeasibleNodesToFind  // 최소 100개
    }
    return numNodes
}
```

적응형 비율 계산:

| 전체 노드 수 | 비율 | 스코어링할 노드 수 |
|------------|------|-----------------|
| 50 | 100% (50 < 100) | 50 |
| 100 | 49% | 100 (최소) |
| 500 | 46% | 230 |
| 1,000 | 42% | 420 |
| 5,000 | 10% | 500 |
| 10,000 | 5% (최소) | 500 |

### 6.5 라운드 로빈 시작 인덱스

**파일:** `pkg/scheduler/schedule_one.go` (690행~691행)

```go
processedNodes := len(feasibleNodes) + diagnosis.NodeToStatus.Len()
sched.nextStartNodeIndex = (sched.nextStartNodeIndex + processedNodes) % len(allNodes)
```

매 스케줄링 사이클마다 시작 인덱스를 이동시켜 모든 노드가 공평하게 평가 기회를 갖도록 한다.

---

## 7. Preemption (PostFilter)

### 7.1 선점 메커니즘

Pod가 어떤 노드에도 스케줄링될 수 없을 때, PostFilter 단계에서 선점(Preemption)을 시도한다.
선점은 우선순위가 낮은 Pod를 퇴거(evict)시켜 공간을 확보하는 메커니즘이다.

**파일:** `pkg/scheduler/schedule_one.go` (281행~301행)

```go
// SchedulePod() 실패 시 PostFilter 실행
if !schedFramework.HasPostFilterPlugins() {
    logger.V(3).Info("No PostFilter plugins are registered, " +
        "so no preemption will be performed")
    return ScheduleResult{nominatingInfo: clearNominatedNode},
        fwk.NewStatus(fwk.Unschedulable).WithError(err)
}

// PostFilter 플러그인 실행 (선점)
result, status := schedFramework.RunPostFilterPlugins(ctx, state, pod,
    fitError.Diagnosis.NodeToStatus)
```

### 7.2 DefaultPreemption 플러그인

**파일:** `pkg/scheduler/framework/plugins/defaultpreemption/`

DefaultPreemption은 기본 제공 선점 플러그인으로, 다음 단계를 수행한다:

```
선점 알고리즘:
    │
    ├── 1. 후보 노드 선별
    │   └── 각 노드에서 낮은 우선순위 Pod를 제거했을 때
    │       현재 Pod가 스케줄 가능한지 시뮬레이션
    │
    ├── 2. 희생자(Victim) 선택
    │   └── PDB 위반 최소화
    │   └── 최고 우선순위 희생자의 우선순위가 가장 낮은 노드
    │   └── 희생자 수가 가장 적은 노드
    │
    ├── 3. NominatedNodeName 설정
    │   └── 선점 대상 Pod의 Status.NominatedNodeName에 기록
    │
    └── 4. 희생자 Pod 삭제 API 호출
        └── GracefulTermination으로 Pod 퇴거
```

### 7.3 선점 흐름 다이어그램

```
┌─────────────────────────────────────────────────────────────────┐
│                    Preemption 흐름                                │
│                                                                 │
│  고우선순위 Pod P (priority=1000) 스케줄링 실패                   │
│       │                                                         │
│       ▼                                                         │
│  PostFilter (DefaultPreemption) 시작                             │
│       │                                                         │
│       ├── Node A: [Pod X(p=100), Pod Y(p=200)]                 │
│       │   시뮬레이션: X,Y 제거 → P 스케줄 가능 ✓               │
│       │                                                         │
│       ├── Node B: [Pod Z(p=900)]                               │
│       │   시뮬레이션: Z 제거 → P 스케줄 가능 ✓                  │
│       │   하지만 Z의 우선순위가 너무 높음                        │
│       │                                                         │
│       └── Node C: [Pod W(p=50)]                                │
│           시뮬레이션: W 제거 → P 스케줄 가능 ✓                  │
│                                                                 │
│  후보 비교:                                                      │
│    Node A: 희생자 최고 우선순위 = 200, 희생자 수 = 2             │
│    Node C: 희생자 최고 우선순위 = 50,  희생자 수 = 1             │
│    → Node C 선택 (희생자 우선순위 더 낮고, 수도 적음)            │
│                                                                 │
│  결과:                                                          │
│    Pod P: Status.NominatedNodeName = "Node C"                  │
│    Pod W: 삭제 API 호출 (Graceful Termination)                  │
│                                                                 │
│  다음 스케줄링 사이클:                                            │
│    Pod P가 다시 스케줄링 → Node C에 배치                         │
└─────────────────────────────────────────────────────────────────┘
```

### 7.4 NominatedNodeName

선점된 Pod는 `Status.NominatedNodeName`에 대상 노드가 기록된다.
다음 스케줄링 사이클에서 이 노드를 먼저 평가한다:

**파일:** `pkg/scheduler/schedule_one.go` (664행~673행)

```go
// "NominatedNodeName" can potentially be set in a previous scheduling cycle
// as a result of preemption.
// This node is likely the only candidate that will fit the pod.
if len(pod.Status.NominatedNodeName) > 0 || len(nodeHint) > 0 {
    feasibleNodes, err := sched.evaluateNominatedNode(ctx, pod,
        schedFramework, state, nodeHint, diagnosis)
    if len(feasibleNodes) != 0 {
        return feasibleNodes, diagnosis, nodeHint, signature, nil
    }
}
```

---

## 8. 내장 플러그인

### 8.1 플러그인 레지스트리

**파일:** `pkg/scheduler/framework/plugins/registry.go` (48행~75행)

```go
func NewInTreeRegistry() runtime.Registry {
    fts := plfeature.NewSchedulerFeaturesFromGates(feature.DefaultFeatureGate)
    registry := runtime.Registry{
        dynamicresources.Name:                runtime.FactoryAdapter(fts, dynamicresources.New),
        imagelocality.Name:                   imagelocality.New,
        tainttoleration.Name:                 runtime.FactoryAdapter(fts, tainttoleration.New),
        nodename.Name:                        runtime.FactoryAdapter(fts, nodename.New),
        nodeports.Name:                       runtime.FactoryAdapter(fts, nodeports.New),
        nodeaffinity.Name:                    runtime.FactoryAdapter(fts, nodeaffinity.New),
        nodedeclaredfeatures.Name:            runtime.FactoryAdapter(fts, nodedeclaredfeatures.New),
        podtopologyspread.Name:               runtime.FactoryAdapter(fts, podtopologyspread.New),
        nodeunschedulable.Name:               runtime.FactoryAdapter(fts, nodeunschedulable.New),
        noderesources.Name:                   runtime.FactoryAdapter(fts, noderesources.NewFit),
        noderesources.BalancedAllocationName: runtime.FactoryAdapter(fts, noderesources.NewBalancedAllocation),
        volumebinding.Name:                   runtime.FactoryAdapter(fts, volumebinding.New),
        volumerestrictions.Name:              runtime.FactoryAdapter(fts, volumerestrictions.New),
        volumezone.Name:                      runtime.FactoryAdapter(fts, volumezone.New),
        nodevolumelimits.CSIName:             runtime.FactoryAdapter(fts, nodevolumelimits.NewCSI),
        interpodaffinity.Name:                runtime.FactoryAdapter(fts, interpodaffinity.New),
        queuesort.Name:                       queuesort.New,
        defaultbinder.Name:                   defaultbinder.New,
        defaultpreemption.Name:               runtime.FactoryAdapter(fts, defaultpreemption.New),
        schedulinggates.Name:                 runtime.FactoryAdapter(fts, schedulinggates.New),
        gangscheduling.Name:                  runtime.FactoryAdapter(fts, gangscheduling.New),
    }
    return registry
}
```

### 8.2 내장 플러그인 상세

| 플러그인 | 확장 포인트 | 기능 |
|----------|-----------|------|
| **NodeResourcesFit** | PreFilter, Filter, Score | CPU/메모리 리소스 적합성 검사 |
| **NodeResourcesBalancedAllocation** | Score | 리소스 균형 분배 점수 |
| **NodeName** | Filter | Pod.Spec.NodeName 일치 확인 |
| **NodePorts** | PreFilter, Filter | 호스트 포트 충돌 검사 |
| **NodeAffinity** | PreFilter, Filter, Score | 노드 어피니티 규칙 적용 |
| **NodeUnschedulable** | Filter | Unschedulable 노드 제외 |
| **TaintToleration** | Filter, PreScore, Score | Taint/Toleration 매칭 |
| **PodTopologySpread** | PreFilter, Filter, PreScore, Score | 토폴로지 분산 제약 |
| **InterPodAffinity** | PreFilter, Filter, PreScore, Score | Pod간 어피니티/안티어피니티 |
| **VolumeBinding** | PreFilter, Filter, Reserve, PreBind | 볼륨 바인딩 및 프로비저닝 |
| **VolumeRestrictions** | Filter | 볼륨 마운트 제약 검사 |
| **VolumeZone** | Filter | 볼륨 가용 영역 매칭 |
| **NodeVolumeLimits** | Filter | 노드당 볼륨 수 제한 |
| **ImageLocality** | Score | 이미지가 이미 있는 노드 선호 |
| **DynamicResources** | PreFilter, Filter, PostFilter, PreScore, Reserve, PreBind, PostBind | DRA(Dynamic Resource Allocation) |
| **DefaultPreemption** | PostFilter | 기본 선점 로직 |
| **DefaultBinder** | Bind | API Server에 바인딩 요청 |
| **QueueSort** | QueueSort | Pod 우선순위 정렬 |
| **SchedulingGates** | PreEnqueue | 게이트 조건 검사 |
| **GangScheduling** | PreFilter, PostFilter, Permit | Pod 그룹 동시 스케줄링 |
| **NodeDeclaredFeatures** | Filter | 노드 선언 기능 매칭 |

### 8.3 플러그인별 확장 포인트 매핑

```
┌────────────────────────────────────────────────────────────────────────┐
│                  플러그인 ↔ 확장 포인트 매핑                            │
│                                                                        │
│  확장 포인트       등록된 플러그인                                       │
│  ──────────       ─────────────                                        │
│  QueueSort     →  PrioritySort                                         │
│                                                                        │
│  PreFilter     →  NodeResourcesFit, NodePorts, NodeAffinity,          │
│                   PodTopologySpread, InterPodAffinity,                 │
│                   VolumeBinding, DynamicResources                      │
│                                                                        │
│  Filter        →  NodeResourcesFit, NodePorts, NodeAffinity,          │
│                   NodeName, NodeUnschedulable, TaintToleration,        │
│                   PodTopologySpread, InterPodAffinity,                 │
│                   VolumeBinding, VolumeRestrictions, VolumeZone,       │
│                   NodeVolumeLimits, DynamicResources,                  │
│                   NodeDeclaredFeatures                                 │
│                                                                        │
│  PostFilter    →  DefaultPreemption, DynamicResources                 │
│                                                                        │
│  PreScore      →  TaintToleration, PodTopologySpread,                 │
│                   InterPodAffinity, DynamicResources                   │
│                                                                        │
│  Score         →  NodeResourcesFit, NodeResourcesBalancedAllocation,  │
│                   NodeAffinity, TaintToleration,                       │
│                   PodTopologySpread, InterPodAffinity,                 │
│                   ImageLocality                                        │
│                                                                        │
│  Reserve       →  VolumeBinding, DynamicResources                     │
│                                                                        │
│  Permit        →  GangScheduling                                       │
│                                                                        │
│  PreBind       →  VolumeBinding, DynamicResources                     │
│                                                                        │
│  Bind          →  DefaultBinder                                        │
│                                                                        │
│  PostBind      →  DynamicResources                                    │
└────────────────────────────────────────────────────────────────────────┘
```

---

## 9. 스케줄링 알고리즘 상세

### 9.1 schedulePod

**파일:** `pkg/scheduler/schedule_one.go` (562행~622행)

```go
func (sched *Scheduler) schedulePod(ctx context.Context, fwk framework.Framework,
    state fwk.CycleState, podInfo *framework.QueuedPodInfo) (result ScheduleResult, err error) {

    pod := podInfo.Pod
    trace := utiltrace.New("Scheduling", ...)
    defer trace.LogIfLong(100 * time.Millisecond)

    // 1. 스냅샷 갱신
    if err := sched.Cache.UpdateSnapshot(klog.FromContext(ctx),
        sched.nodeInfoSnapshot); err != nil {
        return result, err
    }

    // 2. 노드가 없으면 에러
    if sched.nodeInfoSnapshot.NumNodes() == 0 {
        return result, ErrNoNodesAvailable
    }

    // 3. Filter 단계: 적합한 노드 찾기
    feasibleNodes, diagnosis, nodeHint, signature, err := sched.findNodesThatFitPod(
        ctx, fwk, state, podInfo.Pod)
    if err != nil {
        return result, err
    }

    // 4. 적합한 노드가 없으면 FitError
    if len(feasibleNodes) == 0 {
        return result, &framework.FitError{
            Pod:         pod,
            NumAllNodes: sched.nodeInfoSnapshot.NumNodes(),
            Diagnosis:   diagnosis,
        }
    }

    // 5. 적합한 노드가 하나면 바로 사용
    if len(feasibleNodes) == 1 {
        node := feasibleNodes[0].Node().Name
        return ScheduleResult{
            SuggestedHost:  node,
            EvaluatedNodes: 1 + diagnosis.NodeToStatus.Len(),
            FeasibleNodes:  1,
        }, nil
    }

    // 6. Score 단계: 노드에 점수 부여
    priorityList, err := prioritizeNodes(ctx, sched.Extenders, fwk, state,
        pod, feasibleNodes)
    if err != nil {
        return result, err
    }

    // 7. 최고 점수 노드 선택
    sortedPrioritizedNodes := newSortedNodeScores(priorityList)
    node := sortedPrioritizedNodes.Pop()

    return ScheduleResult{
        SuggestedHost:  node,
        EvaluatedNodes: len(feasibleNodes) + diagnosis.NodeToStatus.Len(),
        FeasibleNodes:  len(feasibleNodes),
    }, err
}
```

### 9.2 findNodesThatFitPod

**파일:** `pkg/scheduler/schedule_one.go` (626행~693행)

```go
func (sched *Scheduler) findNodesThatFitPod(ctx context.Context,
    schedFramework framework.Framework, state fwk.CycleState,
    pod *v1.Pod) ([]fwk.NodeInfo, framework.Diagnosis, string, fwk.PodSignature, error) {

    diagnosis := framework.Diagnosis{
        NodeToStatus: framework.NewDefaultNodeToStatus(),
    }
    allNodes, err := sched.nodeInfoSnapshot.NodeInfos().List()

    // 1. PreFilter 실행
    preRes, s, unscheduledPlugins := schedFramework.RunPreFilterPlugins(ctx, state, pod)
    diagnosis.UnschedulablePlugins = unscheduledPlugins
    if !s.IsSuccess() {
        if !s.IsRejected() {
            return nil, diagnosis, "", nil, s.AsError()
        }
        // PreFilter가 거부하면 모든 노드 거부
        diagnosis.NodeToStatus.SetAbsentNodesStatus(s)
        return nil, diagnosis, "", nil, nil
    }

    // 2. NominatedNode 먼저 평가 (선점 결과)
    if len(pod.Status.NominatedNodeName) > 0 || len(nodeHint) > 0 {
        feasibleNodes, err := sched.evaluateNominatedNode(ctx, pod,
            schedFramework, state, nodeHint, diagnosis)
        if len(feasibleNodes) != 0 {
            return feasibleNodes, diagnosis, nodeHint, signature, nil
        }
    }

    // 3. PreFilter 결과로 노드 목록 축소
    nodes := allNodes
    if !preRes.AllNodes() {
        nodes = make([]fwk.NodeInfo, 0, len(preRes.NodeNames))
        for nodeName := range preRes.NodeNames {
            if nodeInfo, err := sched.nodeInfoSnapshot.Get(nodeName); err == nil {
                nodes = append(nodes, nodeInfo)
            }
        }
    }

    // 4. Filter 실행 (병렬)
    feasibleNodes, err := sched.findNodesThatPassFilters(ctx, schedFramework,
        state, pod, &diagnosis, nodes)

    // 5. Extender Filter 실행 (순차)
    feasibleNodes, err = findNodesThatPassExtenders(ctx, sched.Extenders,
        pod, feasibleNodes, diagnosis.NodeToStatus)

    // 6. 라운드 로빈 인덱스 갱신
    processedNodes := len(feasibleNodes) + diagnosis.NodeToStatus.Len()
    sched.nextStartNodeIndex = (sched.nextStartNodeIndex + processedNodes) % len(allNodes)

    return feasibleNodes, diagnosis, nodeHint, signature, nil
}
```

### 9.3 전체 알고리즘 흐름

```
schedulePod()
    │
    ├── 1. Cache.UpdateSnapshot()
    │      노드/Pod 정보 스냅샷 갱신
    │
    ├── 2. findNodesThatFitPod()
    │      │
    │      ├── 2a. RunPreFilterPlugins()
    │      │       - NodeResourcesFit: 리소스 요청량 계산
    │      │       - NodePorts: 필요한 포트 목록 수집
    │      │       - InterPodAffinity: 어피니티 조건 준비
    │      │
    │      ├── 2b. evaluateNominatedNode() [선점된 노드 먼저]
    │      │
    │      ├── 2c. findNodesThatPassFilters() [병렬]
    │      │       각 노드에 대해:
    │      │       - NodeUnschedulable: Unschedulable 확인
    │      │       - NodeName: Spec.NodeName 일치
    │      │       - NodePorts: 포트 충돌
    │      │       - NodeAffinity: 어피니티 매칭
    │      │       - TaintToleration: Taint 허용
    │      │       - NodeResourcesFit: CPU/메모리 여유
    │      │       - VolumeBinding: 볼륨 바인딩 가능
    │      │       - PodTopologySpread: 분산 제약
    │      │       - InterPodAffinity: Pod간 어피니티
    │      │
    │      └── 2d. findNodesThatPassExtenders() [순차]
    │
    ├── 3. [노드 1개면 바로 반환]
    │
    └── 4. prioritizeNodes()
           │
           ├── 4a. RunPreScorePlugins()
           │       - TaintToleration: 전처리
           │       - InterPodAffinity: 점수 데이터 준비
           │
           ├── 4b. RunScorePlugins()
           │       각 노드에 대해:
           │       - NodeResourcesFit: 리소스 여유율 점수
           │       - NodeResourcesBalancedAllocation: 균형 점수
           │       - ImageLocality: 이미지 존재 점수
           │       - NodeAffinity: 어피니티 선호 점수
           │       - TaintToleration: Taint 점수
           │       - PodTopologySpread: 분산 점수
           │       - InterPodAffinity: Pod간 선호 점수
           │
           ├── 4c. NormalizeScore (0~100 정규화)
           │
           └── 4d. 최고 점수 노드 선택
```

---

## 10. Binding Cycle

### 10.1 runBindingCycle

**파일:** `pkg/scheduler/schedule_one.go` (150행~169행)

```go
func (sched *Scheduler) runBindingCycle(
    ctx context.Context,
    state fwk.CycleState,
    schedFramework framework.Framework,
    scheduleResult ScheduleResult,
    assumedPodInfo *framework.QueuedPodInfo,
    start time.Time,
    podsToActivate *framework.PodsToActivate) {

    bindingCycleCtx, cancel := context.WithCancel(ctx)
    defer cancel()

    metrics.Goroutines.WithLabelValues(metrics.Binding).Inc()
    defer metrics.Goroutines.WithLabelValues(metrics.Binding).Dec()

    status := sched.bindingCycle(bindingCycleCtx, state, schedFramework,
        scheduleResult, assumedPodInfo, start, podsToActivate)
    if !status.IsSuccess() {
        sched.handleBindingCycleError(bindingCycleCtx, state, schedFramework,
            assumedPodInfo, start, scheduleResult, status)
        return
    }
}
```

### 10.2 bindingCycle

**파일:** `pkg/scheduler/schedule_one.go` (389행~496행)

```go
func (sched *Scheduler) bindingCycle(
    ctx context.Context,
    state fwk.CycleState,
    schedFramework framework.Framework,
    scheduleResult ScheduleResult,
    assumedPodInfo *framework.QueuedPodInfo,
    start time.Time,
    podsToActivate *framework.PodsToActivate) *fwk.Status {

    assumedPod := assumedPodInfo.Pod

    // 1. WaitOnPermit: Permit 플러그인의 대기 처리
    if status := schedFramework.WaitOnPermit(ctx, assumedPod); !status.IsSuccess() {
        return status
    }

    // 2. Done: 스케줄링 큐에서 완료 표시
    sched.SchedulingQueue.Done(assumedPod.UID)

    // 3. PreBind: 바인딩 전 준비 (볼륨 프로비저닝 등)
    if status := schedFramework.RunPreBindPlugins(ctx, state, assumedPod,
        scheduleResult.SuggestedHost); !status.IsSuccess() {
        return status
    }

    // 4. Bind: API Server에 바인딩 요청
    if status := sched.bind(ctx, schedFramework, assumedPod,
        scheduleResult.SuggestedHost, state); !status.IsSuccess() {
        return status
    }

    // 5. 성공 메트릭 기록
    logger.V(2).Info("Successfully bound pod to node",
        "pod", klog.KObj(assumedPod),
        "node", scheduleResult.SuggestedHost,
        "evaluatedNodes", scheduleResult.EvaluatedNodes,
        "feasibleNodes", scheduleResult.FeasibleNodes)
    metrics.PodScheduled(schedFramework.ProfileName(), metrics.SinceInSeconds(start))
    metrics.PodSchedulingAttempts.Observe(float64(assumedPodInfo.Attempts))

    // 6. PostBind: 바인딩 후 정리 (정보성)
    schedFramework.RunPostBindPlugins(ctx, state, assumedPod,
        scheduleResult.SuggestedHost)

    // 7. 대기 중인 Pod 활성화
    if len(podsToActivate.Map) != 0 {
        sched.SchedulingQueue.Activate(logger, podsToActivate.Map)
    }

    return nil
}
```

### 10.3 Assume 메커니즘

Assume는 바인딩이 완료되기 전에 Pod를 캐시에 "이미 배치된 것처럼" 기록하는 메커니즘이다.

**파일:** `pkg/scheduler/schedule_one.go` (306행~327행)

```go
func (sched *Scheduler) assumeAndReserve(
    ctx context.Context,
    state fwk.CycleState,
    schedFramework framework.Framework,
    podInfo *framework.QueuedPodInfo,
    scheduleResult ScheduleResult,
) (*framework.QueuedPodInfo, *fwk.Status) {

    // 1. 캐시에 Pod를 "assumed" 상태로 추가
    assumedPodInfo := podInfo.DeepCopy()
    assumedPod := assumedPodInfo.Pod
    err := sched.assume(logger, assumedPodInfo, scheduleResult.SuggestedHost)

    // 2. Reserve 플러그인 실행
    if sts := schedFramework.RunReservePluginsReserve(ctx, state, assumedPod,
        scheduleResult.SuggestedHost); !sts.IsSuccess() {
        // 실패 시 Unreserve + Forget
        sched.unreserveAndForget(ctx, state, schedFramework, assumedPodInfo,
            scheduleResult.SuggestedHost)
        return assumedPodInfo, sts
    }
    return assumedPodInfo, nil
}
```

Assume의 이점:

```
전통적 방식 (Assume 없음):
  Filter → Score → Bind(동기) → 다음 Pod 스케줄링
  └─── 바인딩 대기 시간만큼 블로킹 (수십~수백 ms) ───┘

Assume 방식:
  Filter → Score → Assume(캐시 업데이트) → 다음 Pod 즉시 스케줄링 시작
                                            │
                            go Bind(비동기) ──┘
  └─── 바인딩 대기 없이 연속 스케줄링 ───┘
```

---

## 11. 왜 이런 설계인가

### 11.1 왜 플러그인 아키텍처인가?

**이전 방식 (Scheduler Framework 도입 전):**
- 하드코딩된 Predicate(필터)와 Priority(점수) 함수
- 확장하려면 소스 코드 수정 필요
- Webhook 기반 Scheduler Extender는 성능 문제

**현재 방식 (Scheduling Framework):**
- 12개 잘 정의된 확장 포인트
- in-tree와 out-of-tree 플러그인 동일한 인터페이스
- CycleState로 플러그인 간 데이터 공유
- 프로세스 내 함수 호출 → Webhook 대비 월등한 성능

| 비교 항목 | Scheduler Extender | Scheduling Framework |
|----------|-------------------|---------------------|
| 통신 방식 | HTTP/gRPC (프로세스 간) | 함수 호출 (프로세스 내) |
| 레이턴시 | ~ms | ~us |
| 확장 포인트 | Filter, Prioritize만 | 12개 |
| 데이터 공유 | 직렬화 필요 | CycleState |
| 에러 처리 | 네트워크 에러 포함 | 타입 안전 |

### 11.2 왜 병렬 필터인가?

5,000개 노드 클러스터에서 각 노드에 10개 Filter 플러그인을 순차 실행하면:

```
순차: 5,000 노드 x 10 플러그인 x 0.1ms = 5초
병렬(16 goroutine): 5초 / 16 ≈ 0.3초
+ 적응형 비율(5%): 250 노드 x 10 x 0.1ms / 16 ≈ 0.015초
```

`context.WithCancelCause` + `atomic.AddInt32`로 충분한 노드를 찾으면 나머지를 즉시 중단한다.

### 11.3 왜 3-큐 아키텍처인가?

| 큐 | 목적 | 없으면? |
|----|------|--------|
| **activeQ** | 즉시 스케줄링 가능한 Pod | 모든 Pod를 순차 시도 → 불필요한 재시도 |
| **backoffQ** | 재시도 속도 제어 | 실패한 Pod가 즉시 재시도 → CPU 낭비 |
| **unschedulablePods** | 관련 이벤트만 반응 | 모든 Pod가 매 사이클 시도 → O(n^2) |

**QueueingHint**가 핵심 최적화다. 각 플러그인이 "이 클러스터 이벤트가 나를
스케줄 가능하게 만들 수 있는가?"에 대한 힌트를 제공한다:

```
Pod가 NodeResourcesFit에서 실패 → unschedulablePods로 이동
  │
  ├── 노드 추가 이벤트 발생
  │   QueueingHint: "이 Pod에게 유효할 수 있음" → backoffQ로 이동
  │
  ├── ConfigMap 변경 이벤트 발생
  │   QueueingHint: "NodeResourcesFit과 무관" → 그대로 유지
  │
  └── 대기 시간 초과 → activeQ로 이동 (안전장치)
```

### 11.4 왜 Assume + 비동기 바인딩인가?

**문제**: 바인딩(API Server → etcd 쓰기)은 네트워크 레이턴시를 포함한다.
동기적으로 바인딩을 대기하면 스케줄링 처리량이 크게 감소한다.

**해결**: Assume(낙관적 캐시 업데이트) 후 다음 Pod를 즉시 스케줄링한다.

```
시간 →

동기 바인딩:
  [Pod A: Filter+Score] [Pod A: Bind ~~~~] [Pod B: Filter+Score] [Pod B: Bind ~~~~]
                        ← 대기 →                                 ← 대기 →

비동기 바인딩 (Assume):
  [Pod A: Filter+Score+Assume] [Pod B: Filter+Score+Assume] [Pod C: ...]
                [Pod A: Bind ~~~~]  [Pod B: Bind ~~~~]
                                    ← 병렬 진행 →
```

바인딩 실패 시 `unreserveAndForget()`으로 캐시 롤백:

```go
// schedule_one.go 498행~529행
func (sched *Scheduler) handleBindingCycleError(...) {
    // Unreserve + Forget → 캐시에서 assumed Pod 제거
    sched.unreserveAndForget(ctx, state, fwk, podInfo, scheduleResult.SuggestedHost)
    // 실패 핸들러 호출 → 재시도 큐에 추가
    sched.FailureHandler(ctx, fwk, podInfo, status, clearNominatedNode, start)
}
```

### 11.5 왜 라운드 로빈 시작 인덱스인가?

노드 목록의 시작 위치를 매번 변경하지 않으면:

- 앞쪽 노드만 계속 평가 → 뒤쪽 노드에 Pod가 안 배치됨
- `percentageOfNodesToScore`로 일부만 평가할 때 특히 문제

```go
// schedule_one.go 691행
sched.nextStartNodeIndex = (sched.nextStartNodeIndex + processedNodes) % len(allNodes)
```

이 한 줄이 모든 노드에 공평한 평가 기회를 보장한다.

---

## 12. 정리

### 스케줄링 전체 흐름 요약

```
1. SchedulingQueue.Pop()  →  Pod 꺼내기
2. frameworkForPod()      →  스케줄러 프로필 조회
3. PreFilter             →  전처리 (리소스 계산)
4. Filter (병렬)          →  노드 필터링 (atomic.AddInt32)
5. PostFilter            →  실패 시 선점 시도
6. PreScore              →  점수 전처리
7. Score + Normalize     →  노드 점수 부여 (0~100)
8. 최고 점수 노드 선택
9. Assume + Reserve      →  캐시에 낙관적 반영
10. Permit               →  최종 승인/대기
11. go Bind              →  비동기 바인딩
    ├── WaitOnPermit
    ├── PreBind
    ├── Bind (API Server)
    └── PostBind
```

### 핵심 파일 참조

| 파일 | 행 | 내용 |
|------|-----|------|
| `scheduler.go` | 68~124 | Scheduler 구조체 |
| `scheduler.go` | 126~129 | applyDefaultHandlers |
| `scheduler.go` | 152~163 | ScheduleResult 구조체 |
| `scheduler.go` | 261~273 | defaultSchedulerOptions |
| `scheduler.go` | 276~299 | New() 생성자 |
| `schedule_one.go` | 49~62 | 상수 (minFeasibleNodesToFind 등) |
| `schedule_one.go` | 66~95 | ScheduleOne 메서드 |
| `schedule_one.go` | 98~147 | scheduleOnePod 메서드 |
| `schedule_one.go` | 173~193 | schedulingCycle 메서드 |
| `schedule_one.go` | 249~303 | schedulingAlgorithm 메서드 |
| `schedule_one.go` | 306~352 | assumeAndReserve 메서드 |
| `schedule_one.go` | 389~496 | bindingCycle 메서드 |
| `schedule_one.go` | 562~622 | schedulePod 메서드 |
| `schedule_one.go` | 626~693 | findNodesThatFitPod 메서드 |
| `schedule_one.go` | 768~851 | findNodesThatPassFilters 메서드 |
| `schedule_one.go` | 855~881 | numFeasibleNodesToFind 메서드 |
| `framework/interface.go` | 476~665 | Plugin 인터페이스들 |
| `plugins/registry.go` | 48~75 | NewInTreeRegistry |
| `queue/scheduling_queue.go` | 94~148 | SchedulingQueue 인터페이스 |
| `queue/scheduling_queue.go` | 167~215 | PriorityQueue 구조체 |

### 설계 키워드

- **Scheduling Framework**: 12개 확장 포인트의 플러그인 아키텍처
- **병렬 필터링**: `atomic.AddInt32` + `context.WithCancelCause`
- **적응형 비율**: 노드 수에 따른 동적 평가 범위 조절
- **3-큐 아키텍처**: activeQ + backoffQ + unschedulablePods
- **QueueingHint**: 이벤트 기반 선택적 재큐잉
- **Assume + 비동기 Bind**: 낙관적 캐시 업데이트로 처리량 극대화
- **라운드 로빈**: 공평한 노드 평가 기회
- **CAS 선점**: NominatedNodeName으로 안전한 선점 처리
