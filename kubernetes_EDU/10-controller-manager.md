# 10. 컨트롤러 매니저 심화

## 목차

1. [개요](#1-개요)
2. [컨트롤러 매니저 기동 흐름](#2-컨트롤러-매니저-기동-흐름)
3. [리더 선출 메커니즘](#3-리더-선출-메커니즘)
4. [컨트롤러 패턴: Informer - EventHandler - WorkQueue - Worker](#4-컨트롤러-패턴-informer---eventhandler---workqueue---worker)
5. [DeploymentController 심화](#5-deploymentcontroller-심화)
6. [ReplicaSetController 심화](#6-replicasetcontroller-심화)
7. [GarbageCollector 심화](#7-garbagecollector-심화)
8. [SlowStartBatch 메커니즘](#8-slowstartbatch-메커니즘)
9. [Why: Level-Triggered Reconciliation](#9-why-level-triggered-reconciliation)
10. [정리](#10-정리)

---

## 1. 개요

kube-controller-manager는 Kubernetes 클러스터의 핵심 제어 루프(Control Loop)들을 하나의 바이너리로
묶어 실행하는 데몬이다. 각 컨트롤러는 API 서버를 통해 클러스터의 공유 상태(Shared State)를 관찰하고,
현재 상태(Current State)를 원하는 상태(Desired State)로 수렴시키려는 시도를 반복한다.

### 핵심 설계 철학

| 원칙 | 설명 |
|------|------|
| Level-Triggered | 엣지가 아닌 "현재 상태"를 기준으로 동작 -- 이벤트를 놓쳐도 복구 가능 |
| 단일 책임 | 각 컨트롤러는 하나의 리소스 종류에 집중 |
| 선언적 수렴 | spec과 status의 차이를 좁히는 방향으로만 동작 |
| Shared Informer | 모든 컨트롤러가 동일한 캐시를 공유하여 API 서버 부하 최소화 |

### 소스 코드 위치

```
cmd/kube-controller-manager/
├── app/
│   ├── controllermanager.go    # 진입점, Run(), CreateControllerContext()
│   ├── core.go                 # 핵심 컨트롤러 등록 (Deployment, RS, GC 등)
│   └── config/                 # 설정 구조체
pkg/controller/
├── deployment/                 # DeploymentController
│   ├── deployment_controller.go
│   ├── rolling.go              # Rolling Update 로직
│   └── util/                   # 유틸리티 함수
├── replicaset/                 # ReplicaSetController
│   └── replica_set.go
├── garbagecollector/           # GarbageCollector
│   ├── garbagecollector.go
│   ├── graph.go                # 의존성 그래프 노드
│   └── graph_builder.go        # GraphBuilder
└── controller_utils.go         # 공통 유틸 (SlowStartBatch 등)
```

---

## 2. 컨트롤러 매니저 기동 흐름

### 2.1 명령줄 진입점

`cmd/kube-controller-manager/app/controllermanager.go` (라인 102-173)에서
`NewControllerManagerCommand()`가 cobra 명령을 생성한다.

```
NewControllerManagerCommand()
  └─ RunE:
       ├─ s.Config(ctx, KnownControllers(), ...)   // 설정 구성
       └─ Run(ctx, c.Complete())                    // 실제 실행
```

### 2.2 Run() 함수의 핵심 흐름

`Run()` 함수(라인 185-405)는 다음 순서로 동작한다:

```
Run(ctx, config)
│
├─ 1. 이벤트 처리 파이프라인 시작
│     c.EventBroadcaster.StartStructuredLogging(0)
│     c.EventBroadcaster.StartRecordingToSink(...)
│
├─ 2. Health Check 설정
│     electionChecker = leaderelection.NewLeaderHealthzAdaptor(20s)
│
├─ 3. HTTP 서버 시작 (메트릭, 디버그, healthz)
│     c.SecureServing.Serve(handler, ...)
│
├─ 4. Client Builder 생성
│     createClientBuilders(c) → rootClientBuilder, clientBuilder
│
├─ 5. run 클로저 정의 (컨트롤러 실행 로직)
│     ├─ CreateControllerContext()
│     ├─ BuildControllers()
│     ├─ InformerFactory.Start()
│     └─ RunControllers()
│
├─ 6. 리더 선출 여부 분기
│     if !LeaderElect → run(ctx, controllers) 직접 실행
│     else → leaderElectAndRun(ctx, ...) 로 리더 획득 후 실행
│
└─ 7. <-stopCh 대기
```

### 2.3 CreateControllerContext()

`CreateControllerContext()` (라인 475-558)는 모든 컨트롤러가 공유하는 컨텍스트를 생성한다.

```go
// cmd/kube-controller-manager/app/controllermanager.go:475
func CreateControllerContext(ctx context.Context, s *config.CompletedConfig, ...) (ControllerContext, error) {
    // ManagedFields를 메모리에서 제거하여 효율성 확보
    trim := func(obj interface{}) (interface{}, error) {
        if accessor, err := meta.Accessor(obj); err == nil {
            if accessor.GetManagedFields() != nil {
                accessor.SetManagedFields(nil)
            }
        }
        return obj, nil
    }

    sharedInformers := informers.NewSharedInformerFactoryWithOptions(
        versionedClient, ResyncPeriod(s)(),
        informers.WithTransform(trim),      // <-- ManagedFields 제거
    )
    ...
}
```

**ControllerContext 구조체** (라인 408-443):

| 필드 | 타입 | 설명 |
|------|------|------|
| ClientBuilder | ControllerClientBuilder | 컨트롤러별 API 클라이언트 생성 |
| InformerFactory | SharedInformerFactory | 공유 Informer 팩토리 |
| ObjectOrMetadataInformerFactory | InformerFactory | 메타데이터 전용 Informer |
| ComponentConfig | KubeControllerManagerConfiguration | 설정 값 |
| RESTMapper | DeferredDiscoveryRESTMapper | GVK ↔ GVR 매핑 |
| InformersStarted | chan struct{} | Informer 시작 완료 시그널 |
| ResyncPeriod | func() time.Duration | 랜덤화된 재동기 주기 |
| GraphBuilder | *garbagecollector.GraphBuilder | GC 의존성 그래프 빌더 |

### 2.4 ResyncPeriod의 랜덤화

```go
// cmd/kube-controller-manager/app/controllermanager.go:177
func ResyncPeriod(c *config.CompletedConfig) func() time.Duration {
    return func() time.Duration {
        factor := rand.Float64() + 1       // 1.0 ~ 2.0
        return time.Duration(float64(c.ComponentConfig.Generic.MinResyncPeriod.Nanoseconds()) * factor)
    }
}
```

**Why**: 여러 컨트롤러가 동시에 API 서버에 List 요청을 보내는 "thundering herd" 현상을 방지한다.
각 컨트롤러의 resync 주기가 `MinResyncPeriod`의 1~2배 사이에서 랜덤하게 결정되므로
요청이 시간축에서 분산된다.

### 2.5 BuildControllers와 RunControllers

**BuildControllers()** (라인 567-648):

```
BuildControllers()
├─ SA Token Controller를 먼저 빌드 (다른 컨트롤러의 인증에 필요)
├─ 나머지 컨트롤러 순회
│   ├─ IsControllerEnabled() 확인
│   ├─ BuildController() → Controller 인스턴스 생성
│   └─ HealthChecker 등록
└─ 모든 체크를 healthzHandler에 등록
```

**RunControllers()** (라인 655-751):
- 각 컨트롤러를 별도 goroutine에서 시작
- Jitter를 적용하여 동시 시작 방지: `wait.Jitter(startInterval, 1.0)`
- context 취소 시 shutdownTimeout 동안 종료 대기

```
RunControllers(ctx, controllers, jitter=1.0, timeout)
├─ for controller := range controllers:
│     go func() {
│         time.Sleep(Jitter(startInterval, 1.0))  // 동시 시작 방지
│         controller.Run(ctx)
│     }()
├─ <-ctx.Done()  // 종료 시그널
└─ select:
     case <-terminatedCh: return true    // 정상 종료
     case <-time.After(timeout): return false  // 타임아웃
```

---

## 3. 리더 선출 메커니즘

### 3.1 왜 리더 선출이 필요한가

kube-controller-manager는 고가용성(HA)을 위해 여러 인스턴스가 동시에 실행될 수 있다.
그러나 동일한 컨트롤러가 여러 인스턴스에서 동시에 동작하면 충돌이 발생한다.
따라서 **한 시점에 하나의 인스턴스만 활성화**되어야 한다.

### 3.2 Lease 기반 리더 선출

`leaderElectAndRun()` (라인 785-813):

```go
func leaderElectAndRun(ctx context.Context, c *config.CompletedConfig,
    lockIdentity string, ..., callbacks leaderelection.LeaderCallbacks) {

    rl, _ := resourcelock.NewFromKubeconfig(
        resourceLock,                    // "leases" (Lease 오브젝트 사용)
        c.ComponentConfig.Generic.LeaderElection.ResourceNamespace,
        leaseName,
        resourcelock.ResourceLockConfig{
            Identity:      lockIdentity,  // hostname_UUID
            EventRecorder: c.EventRecorder,
        },
        c.Kubeconfig,
        c.ComponentConfig.Generic.LeaderElection.RenewDeadline.Duration,
    )

    leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
        Lock:          rl,
        LeaseDuration: 15s,  // 기본값
        RenewDeadline: 10s,  // 기본값
        RetryPeriod:   2s,   // 기본값
        Callbacks:     callbacks,
    })
}
```

### 3.3 리더 선출 타이밍 다이어그램

```
     인스턴스 A                    인스턴스 B
     ─────────                    ─────────
         │                            │
         ├─ Lease 획득 시도 ──────────┤
         │  (lockIdentity: A_uuid)   │
         │                            ├─ Lease 획득 시도
         ├─ 리더 획득 성공!           │  (lockIdentity: B_uuid)
         │  OnStartedLeading()       │
         │  → run(ctx, controllers)  ├─ 획득 실패, RetryPeriod(2s) 후 재시도
         │                            │
    ┌────┴─────────────────────┐      │
    │  RenewDeadline(10s)마다   │      │
    │  Lease 갱신               │      ├─ Lease 점유 확인, 대기
    └────┬─────────────────────┘      │
         │                            │
         ├─ 장애 발생! ────────────┐  │
         │  (Lease 갱신 실패)      │  │
         │                         │  │
         │  LeaseDuration(15s)     │  │
         │  만료 대기              │  ├─ Lease 만료 감지
         │                         │  ├─ 리더 획득 성공!
         ├─ OnStoppedLeading()     │  │  OnStartedLeading()
         │  → process exit         │  │  → run(ctx, controllers)
         │                         │  │
```

### 3.4 Lock Identity 구성

```go
// controllermanager.go:287-293
id, err := os.Hostname()
id = id + "_" + string(uuid.NewUUID())
```

hostname만 사용하면 같은 호스트에서 두 프로세스가 실행될 때 충돌한다.
UUID를 추가하여 고유성을 보장한다.

### 3.5 Coordinated Leader Election (새 기능)

```go
// controllermanager.go:325-351
if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.CoordinatedLeaderElection) {
    leaseCandidate, waitForSync, err := leaderelection.NewCandidate(
        c.Client, "kube-system", id, kubeControllerManager,
        binaryVersion, emulationVersion,
        coordinationv1.OldestEmulationVersion,
    )
    go leaseCandidate.Run(ctx)
}
```

이 기능은 버전 간 업그레이드 시 가장 오래된(=안정적인) emulation version을 가진
인스턴스가 리더로 선택되도록 한다.

---

## 4. 컨트롤러 패턴: Informer - EventHandler - WorkQueue - Worker

모든 Kubernetes 컨트롤러는 동일한 패턴을 따른다. 이 패턴은 "controller pattern"이라
불리며, 다음 네 단계로 구성된다.

### 4.1 전체 아키텍처

```
                          API Server
                              │
                    ┌─────────┴─────────┐
                    │                   │
              List/Watch           Update/Create/Delete
                    │                   ▲
                    ▼                   │
            ┌───────────────┐   ┌──────────────┐
            │  Reflector    │   │   Worker(s)   │
            │  (List+Watch) │   │  processNext  │
            └───────┬───────┘   │  WorkItem()   │
                    │           └──────┬───────┘
                    ▼                  ▲
            ┌───────────────┐          │
            │  DeltaFIFO    │          │
            └───────┬───────┘          │
                    │                  │
                    ▼                  │
            ┌───────────────┐   ┌──────┴───────┐
            │  Indexer/     │   │  WorkQueue    │
            │  Store(Cache) │   │  (Rate-       │
            └───────┬───────┘   │   Limiting)   │
                    │           └──────▲───────┘
                    ▼                  │
            ┌───────────────┐          │
            │ EventHandler  │──────────┘
            │ Add/Update/   │  enqueue(key)
            │ Delete        │
            └───────────────┘
```

### 4.2 각 단계의 역할

| 단계 | 구성요소 | 역할 |
|------|----------|------|
| 1. Watch | Reflector + DeltaFIFO | API 서버에서 변경사항 수신 |
| 2. Cache | Indexer/Store | 오브젝트의 최신 상태를 메모리에 캐시 |
| 3. Dispatch | EventHandler | 변경 이벤트를 받아 WorkQueue에 키를 추가 |
| 4. Process | Worker + syncHandler | 큐에서 키를 꺼내 비즈니스 로직 실행 |

### 4.3 WorkQueue의 특성

```go
// DeploymentController 큐 생성 (deployment_controller.go:111-116)
queue: workqueue.NewTypedRateLimitingQueueWithConfig(
    workqueue.DefaultTypedControllerRateLimiter[string](),
    workqueue.TypedRateLimitingQueueConfig[string]{
        Name: "deployment",
    },
),
```

WorkQueue의 세 가지 핵심 보장:

1. **중복 제거**: 같은 키가 여러 번 추가되어도 한 번만 처리
2. **Rate Limiting**: 실패 시 지수 백오프로 재시도 (5ms * 2^retry)
3. **Fair**: 키 단위 순차 처리 보장 (같은 키는 동시 처리 불가)

### 4.4 Worker 루프

모든 컨트롤러의 worker 패턴은 동일하다:

```go
// deployment_controller.go:496-512
func (dc *DeploymentController) worker(ctx context.Context) {
    for dc.processNextWorkItem(ctx) {
    }
}

func (dc *DeploymentController) processNextWorkItem(ctx context.Context) bool {
    key, quit := dc.queue.Get()        // 블로킹 대기
    if quit { return false }
    defer dc.queue.Done(key)           // 처리 완료 마킹

    err := dc.syncHandler(ctx, key)    // 비즈니스 로직
    dc.handleErr(ctx, err, key)        // 에러 처리
    return true
}
```

**에러 처리 전략** (라인 514-534):

```go
func (dc *DeploymentController) handleErr(ctx context.Context, err error, key string) {
    if err == nil || errors.HasStatusCause(err, v1.NamespaceTerminatingCause) {
        dc.queue.Forget(key)    // 성공: 재시도 카운터 리셋
        return
    }
    if dc.queue.NumRequeues(key) < maxRetries {  // maxRetries = 15
        dc.queue.AddRateLimited(key)             // 재시도 큐에 추가
        return
    }
    dc.queue.Forget(key)  // 15회 초과: 포기
}
```

---

## 5. DeploymentController 심화

### 5.1 구조체 정의

`pkg/controller/deployment/deployment_controller.go` (라인 67-101):

```go
type DeploymentController struct {
    rsControl     controller.RSControlInterface  // ReplicaSet 조작
    client        clientset.Interface            // API 클라이언트

    syncHandler   func(ctx context.Context, dKey string) error
    enqueueDeployment func(deployment *apps.Deployment)

    dLister       appslisters.DeploymentLister   // Deployment 캐시 리스터
    rsLister      appslisters.ReplicaSetLister   // ReplicaSet 캐시 리스터
    podLister     corelisters.PodLister          // Pod 캐시 리스터
    podIndexer    cache.Indexer                  // ControllerRef UID 기반 Pod 인덱스

    dListerSynced  cache.InformerSynced
    rsListerSynced cache.InformerSynced
    podListerSynced cache.InformerSynced

    queue workqueue.TypedRateLimitingInterface[string]
}
```

### 5.2 Informer 이벤트 핸들러 등록

`NewDeploymentController()` (라인 104-168)에서 세 가지 Informer에 핸들러를 등록한다:

```
DeploymentController
├─ dInformer (Deployment)
│   ├─ Add:    dc.addDeployment(obj)     → enqueue(Deployment)
│   ├─ Update: dc.updateDeployment(old, new) → enqueue(Deployment)
│   └─ Delete: dc.deleteDeployment(obj)  → enqueue(Deployment)
│
├─ rsInformer (ReplicaSet)
│   ├─ Add:    dc.addReplicaSet(obj)
│   │          → controllerRef로 owning Deployment 찾기
│   │          → enqueue(owning Deployment)
│   ├─ Update: dc.updateReplicaSet(old, new)
│   │          → 변경된 ControllerRef의 old/new Deployment 모두 enqueue
│   └─ Delete: dc.deleteReplicaSet(obj)
│              → controllerRef로 owning Deployment enqueue
│
└─ podInformer (Pod)
    └─ Delete: dc.deletePod(obj)
               → Recreate 전략에서만 사용
               → Pod 수가 0이면 Deployment enqueue
```

**핵심 설계**: ReplicaSet이나 Pod에 변화가 생기면, 직접 처리하지 않고 소유자
Deployment를 큐에 넣는다. Deployment의 syncHandler가 전체 상태를 재조정한다.

### 5.3 syncDeployment 핵심 흐름

`syncDeployment()` (라인 589-679)은 Deployment 컨트롤러의 핵심 비즈니스 로직이다:

```
syncDeployment(ctx, "default/my-deploy")
│
├─ 1. Lister에서 Deployment 조회
│     dc.dLister.Deployments(ns).Get(name)
│
├─ 2. DeepCopy (캐시 보호)
│     d = deployment.DeepCopy()
│
├─ 3. Selector 검증 (빈 셀렉터 거부)
│
├─ 4. 소유한 ReplicaSet 목록 조회 + ControllerRef 조정
│     dc.getReplicaSetsForDeployment(ctx, d)
│     → ClaimReplicaSets(): adopt orphans / orphan non-matching
│
├─ 5. Pod 맵 조회 (RS UID → Pod 목록)
│     dc.getPodMapForDeployment(d, rsList)
│
├─ 6. 삭제 중인 경우 → 상태만 동기화
│     if d.DeletionTimestamp != nil → dc.syncStatusOnly()
│
├─ 7. Paused 상태 확인
│     if d.Spec.Paused → dc.sync()  (스케일링만 수행)
│
├─ 8. Rollback 확인
│     if getRollbackTo(d) != nil → dc.rollback()
│
├─ 9. 스케일 이벤트 확인
│     if dc.isScalingEvent() → dc.sync()
│
└─ 10. 전략별 분기
      switch d.Spec.Strategy.Type:
        case Recreate:     dc.rolloutRecreate()
        case RollingUpdate: dc.rolloutRolling()
```

### 5.4 Rolling Update 상세

`pkg/controller/deployment/rolling.go` (라인 31-66):

```go
func (dc *DeploymentController) rolloutRolling(ctx, d, rsList) error {
    newRS, oldRSs, err := dc.getAllReplicaSetsAndSyncRevision(ctx, d, rsList, true)
    allRSs := append(oldRSs, newRS)

    // 1단계: 새 ReplicaSet 스케일 업
    scaledUp, err := dc.reconcileNewReplicaSet(ctx, allRSs, newRS, d)
    if scaledUp {
        return dc.syncRolloutStatus(ctx, allRSs, newRS, d)
    }

    // 2단계: 이전 ReplicaSet 스케일 다운
    scaledDown, err := dc.reconcileOldReplicaSets(ctx, allRSs, oldRSs, newRS, d)
    if scaledDown {
        return dc.syncRolloutStatus(ctx, allRSs, newRS, d)
    }

    // 3단계: 완료 확인
    if DeploymentComplete(d, &d.Status) {
        dc.cleanupDeployment(ctx, oldRSs, d)
    }
    return dc.syncRolloutStatus(ctx, allRSs, newRS, d)
}
```

### 5.5 Rolling Update 수학적 모델

```
Deployment: replicas=10, maxSurge=3, maxUnavailable=2

maxReplicas   = replicas + maxSurge       = 10 + 3 = 13
minAvailable  = replicas - maxUnavailable = 10 - 2 = 8

reconcileNewReplicaSet:
  newReplicasCount = min(newRS.Replicas + maxSurge, maxReplicas - currentTotal + newRS.Replicas)

reconcileOldReplicaSets:
  maxScaledDown = allPodsCount - minAvailable - newRSUnavailablePodCount

단계별 예시:
┌──────────┬──────────┬──────────┬──────────┐
│   단계   │ newRS    │ oldRS    │ 총 Pod   │
├──────────┼──────────┼──────────┼──────────┤
│ 초기     │ 0        │ 10       │ 10       │
│ 1차 up   │ 3(surge) │ 10       │ 13       │
│ 1차 down │ 3        │ 5(-5)    │ 8(min)   │
│ 2차 up   │ 8(+5)    │ 5        │ 13       │
│ 2차 down │ 8        │ 0(-5)    │ 8        │
│ 3차 up   │ 10(+2)   │ 0        │ 10       │
│ 완료     │ 10       │ 0        │ 10       │
└──────────┴──────────┴──────────┴──────────┘
```

### 5.6 proportional scaling (비례 스케일링)

Rolling Update 중에 Deployment의 replicas가 변경되면, 새/이전 ReplicaSet에
비례적으로 분배해야 한다. 이를 "proportional scaling"이라 한다.

```
예: replicas=10 → 15로 변경 (maxSurge=25%)
현재 상태: newRS=5, oldRS=5, 총=10

새 replicas=15, maxSurge=4(=ceil(15*0.25))
maxReplicas = 15 + 4 = 19

추가 필요 = 19 - 10 = 9
newRS 비율 = 5/10 = 50%, oldRS 비율 = 5/10 = 50%
newRS에 +5(=ceil(9*0.5)), oldRS에 +4

결과: newRS=10, oldRS=9, 총=19
```

---

## 6. ReplicaSetController 심화

### 6.1 구조체 정의

`pkg/controller/replicaset/replica_set.go` (라인 97-140):

```go
type ReplicaSetController struct {
    schema.GroupVersionKind              // RS 또는 RC를 처리할 수 있음

    kubeClient   clientset.Interface
    podControl   controller.PodControlInterface
    podIndexer   cache.Indexer

    burstReplicas int                    // 한 번에 생성/삭제할 최대 Pod 수 (기본 500)

    syncHandler  func(ctx, rsKey string) error

    expectations *controller.UIDTrackingControllerExpectations

    rsLister     appslisters.ReplicaSetLister
    podLister    corelisters.PodLister

    queue        workqueue.TypedRateLimitingInterface[string]

    consistencyStore consistencyutil.ConsistencyStore
}
```

### 6.2 Expectations 메커니즘

Expectations는 ReplicaSetController의 핵심 최적화 메커니즘이다.

**문제**: Pod를 생성/삭제한 후 API 서버를 거쳐 Informer 캐시에 반영되기까지 지연이 있다.
이 사이에 syncReplicaSet이 다시 실행되면 중복 생성/삭제가 발생할 수 있다.

**해결**: "기대값(Expectations)"을 설정하여 아직 관찰되지 않은 변경을 추적한다.

```
Expectations 상태 머신:
─────────────────────────────────────────────────────

1. Pod 생성 요청 시:
   expectations.ExpectCreations(rsKey, count)
   → 내부적으로 (adds=count, dels=0) 설정

2. Pod가 생성되어 Informer에 관찰될 때:
   expectations.CreationObserved(rsKey)
   → adds--

3. syncReplicaSet 진입 시:
   rsNeedsSync := expectations.SatisfiedExpectations(rsKey)
   → adds==0 && dels==0 이면 true (기대가 충족됨)
   → TTL(5분) 초과 시에도 true (안전장치)
   → false이면 manageReplicas 스킵
```

```
시간축 예시:

t0: syncRS → diff=-3 (3개 부족)
    → ExpectCreations(rsKey, 3)
    → CreatePod x3

t1: syncRS 재진입 (Update 이벤트)
    → SatisfiedExpectations = false (adds=3, 아직 0개 관찰)
    → manageReplicas 스킵!  ← 중복 생성 방지

t2: Pod1 관찰 → CreationObserved (adds=2)
t3: Pod2 관찰 → CreationObserved (adds=1)
t4: Pod3 관찰 → CreationObserved (adds=0)

t5: syncRS 재진입
    → SatisfiedExpectations = true
    → manageReplicas 실행 (정상 경로)
```

### 6.3 syncReplicaSet 핵심 흐름

`syncReplicaSet()` (라인 755-857):

```
syncReplicaSet(ctx, "default/my-rs")
│
├─ 1. ConsistencyStore 확인 (최신 캐시 보장)
│     rsc.consistencyStore.EnsureReady(rsNamespacedName)
│
├─ 2. RS 조회
│     rsc.rsLister.ReplicaSets(ns).Get(name)
│
├─ 3. Expectations 확인
│     rsNeedsSync := rsc.expectations.SatisfiedExpectations(key)
│
├─ 4. Pod 조회 (인덱서 사용)
│     allRSPods := controller.FilterPodsByOwner(rsc.podIndexer, ...)
│     activePods := controller.FilterActivePods(allRSPods)
│     activePods = rsc.claimPods(ctx, rs, selector, activePods)
│
├─ 5. Pod 수 조정 (Expectations 충족 시)
│     if rsNeedsSync && rs.DeletionTimestamp == nil:
│         rsc.manageReplicas(ctx, activePods, rs)
│
├─ 6. 상태 업데이트
│     newStatus = calculateStatus(rs, activePods, ...)
│     updateReplicaSetStatus(rs, newStatus)
│
└─ 7. MinReadySeconds 후 재큐잉 (필요 시)
      rsc.queue.AddAfter(key, duration)
```

### 6.4 manageReplicas 상세

`manageReplicas()` (라인 649-750):

```go
func (rsc *ReplicaSetController) manageReplicas(ctx, activePods, rs) error {
    diff := len(activePods) - int(*(rs.Spec.Replicas))

    if diff < 0 {
        // Pod 부족 → 생성
        diff *= -1
        if diff > rsc.burstReplicas { diff = rsc.burstReplicas }  // 최대 500개
        rsc.expectations.ExpectCreations(rsKey, diff)

        // SlowStartBatch로 점진적 생성
        successfulCreations, err := slowStartBatch(diff,
            controller.SlowStartInitialBatchSize, func() error {
                return rsc.podControl.CreatePods(ctx, rs.Namespace,
                    &rs.Spec.Template, rs, metav1.NewControllerRef(rs, rsc.GVK))
            })

        // 스킵된 Pod에 대해 Expectations 보정
        if skippedPods := diff - successfulCreations; skippedPods > 0 {
            for i := 0; i < skippedPods; i++ {
                rsc.expectations.CreationObserved(rsKey)
            }
        }

    } else if diff > 0 {
        // Pod 초과 → 삭제
        if diff > rsc.burstReplicas { diff = rsc.burstReplicas }

        relatedPods, _ := rsc.getIndirectlyRelatedPods(rs)
        podsToDelete := getPodsToDelete(activePods, relatedPods, diff)

        rsc.expectations.ExpectDeletions(rsKey, getPodKeys(podsToDelete))

        // 삭제는 병렬 실행
        var wg sync.WaitGroup
        for _, pod := range podsToDelete {
            go func(targetPod *v1.Pod) {
                defer wg.Done()
                rsc.podControl.DeletePod(ctx, rs.Namespace, targetPod.Name, rs)
            }(pod)
        }
        wg.Wait()
    }
    return nil
}
```

### 6.5 Pod 삭제 우선순위

`getPodsToDelete()` 함수는 삭제할 Pod를 지능적으로 선택한다:

```
삭제 우선순위 (높은 것부터):
1. Unscheduled (아직 노드에 할당 안 된 Pod)
2. Pending (실행 대기 중)
3. Not Ready (아직 준비 안 됨)
4. 같은 노드에 관련 Pod가 많은 것 (분산 유지)
5. 최근 생성된 것 (오래된 것 보호)
6. 컨테이너 재시작 횟수가 많은 것

이 로직은 controller.ActivePodsWithRanks의 Less() 메서드로 구현된다.
```

---

## 7. GarbageCollector 심화

### 7.1 개요

GarbageCollector(GC)는 OwnerReference 기반의 계단식 삭제(Cascading Deletion)를
담당한다. 부모 오브젝트가 삭제되면 자식 오브젝트도 자동으로 정리한다.

### 7.2 핵심 구조체

**GarbageCollector** (`pkg/controller/garbagecollector/garbagecollector.go`, 라인 64-77):

```go
type GarbageCollector struct {
    restMapper     meta.ResettableRESTMapper
    metadataClient metadata.Interface

    attemptToDelete workqueue.TypedRateLimitingInterface[*node]
    attemptToOrphan workqueue.TypedRateLimitingInterface[*node]

    dependencyGraphBuilder *GraphBuilder
    absentOwnerCache       *ReferenceCache

    kubeClient       clientset.Interface
    eventBroadcaster record.EventBroadcaster
}
```

**GraphBuilder** (`pkg/controller/garbagecollector/graph_builder.go`, 라인 81-120):

```go
type GraphBuilder struct {
    restMapper meta.RESTMapper

    monitors    monitors                     // GVR → monitor 매핑
    monitorLock sync.RWMutex

    graphChanges workqueue.TypedRateLimitingInterface[*event]
    uidToNode    *concurrentUIDToNode        // UID → node 매핑

    attemptToDelete workqueue.TypedRateLimitingInterface[*node]
    attemptToOrphan workqueue.TypedRateLimitingInterface[*node]

    absentOwnerCache *ReferenceCache
    sharedInformers  informerfactory.InformerFactory
    ignoredResources map[schema.GroupResource]struct{}
}
```

**node** (의존성 그래프 노드, `graph.go`, 라인 63-84):

```go
type node struct {
    identity           objectReference       // GVK + Namespace + Name + UID
    dependentsLock     sync.RWMutex
    dependents         map[*node]struct{}     // 이 노드를 owner로 참조하는 노드들
    deletingDependents bool                  // foreground 삭제 중
    beingDeleted       bool                  // 삭제 진행 중
    virtual            bool                  // Informer에서 아직 미관찰
    owners             []metav1.OwnerReference
}
```

### 7.3 의존성 그래프 구조

```
                    ┌─────────────┐
                    │ Deployment  │ ← owner
                    │ (uid: abc)  │
                    └──────┬──────┘
                           │ ownerRef
                    ┌──────┴──────┐
                    │ ReplicaSet  │ ← owner
                    │ (uid: def)  │
                    └──────┬──────┘
                           │ ownerRef
              ┌────────────┼────────────┐
              ▼            ▼            ▼
         ┌────────┐  ┌────────┐  ┌────────┐
         │ Pod-1  │  │ Pod-2  │  │ Pod-3  │
         │(uid:x) │  │(uid:y) │  │(uid:z) │
         └────────┘  └────────┘  └────────┘

그래프 노드 관계:
- Deployment.dependents = {ReplicaSet}
- ReplicaSet.dependents = {Pod-1, Pod-2, Pod-3}
- ReplicaSet.owners = [{Deployment, uid:abc}]
- Pod-1.owners = [{ReplicaSet, uid:def}]
```

### 7.4 GC 동작 흐름

```
GarbageCollector.Run()
│
├─ 1. GraphBuilder.Run()  (단일 스레드)
│     └─ processGraphChanges() 루프
│         ├─ graphChanges 큐에서 이벤트 꺼냄
│         ├─ Add: 노드 생성, owner들의 dependents에 추가
│         ├─ Update: owner 변경 감지, 의존 관계 재조정
│         └─ Delete: 노드 제거, dependents에 있는 노드 → attemptToDelete 큐
│
├─ 2. attemptToDelete Workers (다중 스레드)
│     └─ attemptToDeleteItem(node)
│         ├─ owner가 아직 존재하는지 확인
│         │   ├─ 모든 owner 존재 → 삭제 불필요 (forget)
│         │   ├─ 일부 owner 없음 → ownerRef 패치로 참조 정리
│         │   └─ 모든 owner 없음 → API로 DELETE 전송
│         └─ owner가 foreground 삭제 중이고 BlockOwnerDeletion이면 대기
│
└─ 3. attemptToOrphan Workers (다중 스레드)
      └─ orphanDependents(owner, dependents)
          ├─ 각 dependent의 ownerReferences에서 해당 owner 제거
          └─ owner의 finalizer 제거
```

### 7.5 삭제 전파 모드

| 모드 | propagationPolicy | 동작 |
|------|-------------------|------|
| Foreground | foreground | owner에 finalizer 추가 → dependents 먼저 삭제 → owner 삭제 |
| Background | background | owner 즉시 삭제 → GC가 나중에 orphan dependents 삭제 |
| Orphan | orphan | owner 삭제, dependents의 ownerRef 정리 (고아화) |

### 7.6 Foreground 삭제 상세 흐름

```
사용자: DELETE Deployment (propagationPolicy=Foreground)
│
├─ API Server:
│   ├─ Deployment.metadata.deletionTimestamp 설정
│   ├─ Deployment.metadata.finalizers += "foregroundDeletion"
│   └─ Deployment 상태 = "deletingDependents"
│
├─ GraphBuilder.processGraphChanges():
│   ├─ Deployment 노드에 deletingDependents=true 설정
│   └─ 모든 dependents (ReplicaSet)을 attemptToDelete 큐에 추가
│
├─ GC attemptToDelete Workers:
│   ├─ ReplicaSet 삭제 시도
│   │   └─ ReplicaSet도 dependents(Pods)가 있으면 같은 과정 반복
│   └─ BlockOwnerDeletion=true인 dependents가 모두 삭제될 때까지 반복
│
└─ 모든 blocking dependents 삭제 완료:
    ├─ Deployment의 finalizer 제거
    └─ API Server가 Deployment 최종 삭제
```

### 7.7 Virtual Node과 안전성

GraphBuilder는 ownerReference에 참조된 owner가 아직 Informer에 관찰되지 않은
경우 "virtual node"를 생성한다.

```go
// graph.go:78-79
virtual     bool       // Informer에서 아직 미관찰
virtualLock sync.RWMutex
```

virtual node는 실제 오브젝트가 관찰되면 `markObserved()`로 실체화된다.
만약 virtual node인 상태에서 삭제 시도가 일어나면, API 서버에서 실제 존재 여부를
확인한 후에만 삭제를 진행한다.

---

## 8. SlowStartBatch 메커니즘

### 8.1 동작 원리

`pkg/controller/replicaset/replica_set.go` (라인 887-911):

```go
func slowStartBatch(count int, initialBatchSize int, fn func() error) (int, error) {
    remaining := count
    successes := 0
    for batchSize := min(remaining, initialBatchSize); batchSize > 0;
        batchSize = min(2*batchSize, remaining) {

        errCh := make(chan error, batchSize)
        var wg sync.WaitGroup
        wg.Add(batchSize)
        for i := 0; i < batchSize; i++ {
            go func() {
                defer wg.Done()
                if err := fn(); err != nil {
                    errCh <- err
                }
            }()
        }
        wg.Wait()

        curSuccesses := batchSize - len(errCh)
        successes += curSuccesses
        if len(errCh) > 0 {
            return successes, <-errCh  // 에러 발생 시 즉시 중단
        }
        remaining -= batchSize
    }
    return successes, nil
}
```

### 8.2 배치 크기 변화

```
initialBatchSize = 1 (SlowStartInitialBatchSize)

count = 20일 때:
┌────────┬────────────┬────────────┬───────────┐
│  배치  │ 배치 크기  │ 누적 생성  │  남은 수  │
├────────┼────────────┼────────────┼───────────┤
│   1    │     1      │     1      │    19     │
│   2    │     2      │     3      │    17     │
│   3    │     4      │     7      │    13     │
│   4    │     8      │    15      │     5     │
│   5    │     5      │    20      │     0     │
└────────┴────────────┴────────────┴───────────┘

만약 3번째 배치에서 에러 발생:
→ 성공한 것만 카운트 (예: 6)
→ 나머지 14개는 다음 sync에서 재시도
```

### 8.3 Why: SlowStartBatch를 사용하는 이유

1. **Quota 고갈 방지**: 프로젝트 quota가 부족한 상태에서 100개 Pod를 한번에 생성하면
   모두 실패하고 API 서버에 불필요한 부하가 발생한다. 첫 배치(1개)에서 실패하면
   즉시 중단한다.

2. **이벤트 스팸 방지**: 실패한 Pod 생성마다 Event가 기록된다. 대량 실패 시
   Event 기록 자체가 부하가 된다.

3. **점진적 확인**: 첫 1개가 성공하면 2, 4, 8...개로 점차 늘려간다.
   이는 시스템이 안정적인지 점진적으로 확인하는 것이다.

---

## 9. Why: Level-Triggered Reconciliation

### 9.1 Edge-Triggered vs Level-Triggered

```
Edge-Triggered (이벤트 기반):
"ReplicaSet의 replicas가 3→5로 변경됨"
→ "2개를 추가로 생성하라"

Level-Triggered (상태 기반):
"ReplicaSet의 spec.replicas는 5이고, 현재 활성 Pod는 3개이다"
→ "2개가 부족하므로 생성하라"
```

### 9.2 Level-Triggered가 안전한 이유

| 시나리오 | Edge-Triggered | Level-Triggered |
|----------|----------------|-----------------|
| 이벤트 유실 | 영영 처리 못함 | 다음 sync에서 복구 |
| 중복 이벤트 | 중복 처리 위험 | 멱등성 보장 (diff 기반) |
| 컨트롤러 재시작 | 진행 중 작업 유실 | 전체 상태 재조정 |
| 부분 실패 | 복구 복잡 | 다음 sync가 나머지 처리 |

### 9.3 코드에서의 증거

```go
// replica_set.go에서 syncReplicaSet의 핵심:
diff := len(activePods) - int(*(rs.Spec.Replicas))
// → "현재 상태"와 "원하는 상태"의 차이만 계산
// → 이전에 무슨 이벤트가 있었는지는 전혀 관심 없음
```

```go
// deployment_controller.go에서:
// ReplicaSet 변경 시 → Deployment를 enqueue
// → syncDeployment에서 전체 RS 목록을 다시 조회
// → 어떤 RS가 변경되었는지가 아니라, 현재 전체 상태를 기준으로 판단
```

### 9.4 Periodic Resync

Level-Triggered 모델을 강화하기 위해 Informer는 주기적으로 전체 캐시를
재동기화(resync)한다.

```go
// controllermanager.go:177-182
func ResyncPeriod(c *config.CompletedConfig) func() time.Duration {
    return func() time.Duration {
        factor := rand.Float64() + 1
        return time.Duration(float64(MinResyncPeriod.Nanoseconds()) * factor)
    }
}
```

Resync 시 모든 오브젝트에 대해 Update 이벤트가 발생하며, 이는 모든 컨트롤러의
syncHandler를 다시 트리거한다. 이를 통해:

- Watch 이벤트가 유실된 경우 복구
- 외부 변경(직접 etcd 수정 등)이 반영
- 시간 기반 로직(timeout 체크 등)이 실행

---

## 10. 정리

### 10.1 컨트롤러 매니저의 핵심 구성요소 관계

```
┌─────────────────────────────────────────────────────────────┐
│                  kube-controller-manager                     │
│                                                              │
│  ┌──────────────┐     ┌──────────────────────────────────┐  │
│  │ Leader       │     │ SharedInformerFactory             │  │
│  │ Election     │     │                                  │  │
│  │              │     │  Deployment Informer ──┐         │  │
│  │ Lease 기반   │     │  ReplicaSet Informer ──┤         │  │
│  │ 15s/10s/2s  │     │  Pod Informer ─────────┤         │  │
│  └──────────────┘     │  Service Informer ─────┤ ...     │  │
│                       │  Node Informer ────────┘         │  │
│                       └──────────────┬───────────────────┘  │
│                                      │                       │
│  ┌───────────────────────────────────┼───────────────────┐  │
│  │              Controllers          │                    │  │
│  │                                   ▼                    │  │
│  │  ┌──────────────┐  ┌──────────────┐  ┌────────────┐  │  │
│  │  │ Deployment   │  │ ReplicaSet   │  │ Garbage    │  │  │
│  │  │ Controller   │  │ Controller   │  │ Collector  │  │  │
│  │  ├──────────────┤  ├──────────────┤  ├────────────┤  │  │
│  │  │ EventHandler │  │ EventHandler │  │ GraphBlder │  │  │
│  │  │ WorkQueue    │  │ WorkQueue    │  │ WorkQueues │  │  │
│  │  │ Workers      │  │ Workers +    │  │ Workers    │  │  │
│  │  │ syncDeploy   │  │ Expectations │  │ OwnerRef   │  │  │
│  │  └──────────────┘  └──────────────┘  │ Graph      │  │  │
│  │                                       └────────────┘  │  │
│  │  ┌──────────────┐  ┌──────────────┐  ┌────────────┐  │  │
│  │  │ Namespace    │  │ ServiceAcct  │  │ Job        │  │  │
│  │  │ Controller   │  │ Controller   │  │ Controller │  │  │
│  │  └──────────────┘  └──────────────┘  └────────────┘  │  │
│  │                    ... 30+ 컨트롤러 ...               │  │
│  └───────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────┘
```

### 10.2 핵심 교훈 요약

| 항목 | 핵심 내용 |
|------|-----------|
| 기동 | Run() → LeaderElection → CreateControllerContext → BuildControllers → RunControllers |
| 리더 선출 | Lease 오브젝트 기반, LeaseDuration=15s, RenewDeadline=10s, RetryPeriod=2s |
| 컨트롤러 패턴 | Informer → EventHandler → WorkQueue → Worker(syncHandler) |
| DeploymentController | syncDeployment에서 전략(Rolling/Recreate) 분기, 비례 스케일링 |
| ReplicaSetController | Expectations로 중복 생성/삭제 방지, SlowStartBatch로 점진적 Pod 생성 |
| GarbageCollector | OwnerReference 기반 의존성 그래프, attemptToDelete/attemptToOrphan 이중 큐 |
| 설계 원칙 | Level-Triggered Reconciliation -- 현재 상태 기준 동작, 이벤트 유실에 안전 |

### 10.3 소스 코드 참조 요약

| 파일 | 핵심 함수/구조체 | 라인 |
|------|------------------|------|
| `cmd/kube-controller-manager/app/controllermanager.go` | `Run()`, `CreateControllerContext()` | 185, 475 |
| `cmd/kube-controller-manager/app/controllermanager.go` | `BuildControllers()`, `RunControllers()` | 571, 655 |
| `cmd/kube-controller-manager/app/controllermanager.go` | `leaderElectAndRun()` | 785 |
| `pkg/controller/deployment/deployment_controller.go` | `DeploymentController`, `syncDeployment()` | 67, 589 |
| `pkg/controller/deployment/deployment_controller.go` | `NewDeploymentController()` | 104 |
| `pkg/controller/deployment/rolling.go` | `rolloutRolling()`, `reconcileOldReplicaSets()` | 31, 86 |
| `pkg/controller/replicaset/replica_set.go` | `ReplicaSetController`, `syncReplicaSet()` | 97, 755 |
| `pkg/controller/replicaset/replica_set.go` | `manageReplicas()`, `slowStartBatch()` | 649, 887 |
| `pkg/controller/garbagecollector/garbagecollector.go` | `GarbageCollector`, `Run()` | 64, 132 |
| `pkg/controller/garbagecollector/graph.go` | `node` | 63 |
| `pkg/controller/garbagecollector/graph_builder.go` | `GraphBuilder` | 81 |
