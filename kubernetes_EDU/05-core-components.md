# 05. Kubernetes 핵심 컴포넌트

## 개요

Kubernetes의 4대 핵심 컴포넌트를 소스코드 수준에서 분석한다.

## 1. kube-apiserver

### 역할

- 클러스터의 **유일한 API 게이트웨이** — 모든 컴포넌트는 API Server를 통해서만 통신
- REST API 제공 (CRUD + Watch)
- 인증, 인가, 어드미션 컨트롤
- etcd와의 유일한 통신 채널

### 핵심 구조체

소스: `pkg/controlplane/apiserver/server.go:79`

```go
type Server struct {
    GenericAPIServer *genericapiserver.GenericAPIServer
    // 내장 API 핸들러, 스토리지 프로바이더
}
```

소스: `staging/src/k8s.io/apiserver/pkg/server/genericapiserver.go:109`

```go
type GenericAPIServer struct {
    Handler         *APIServerHandler      // HTTP 핸들러 체인
    delegationTarget DelegationTarget       // 위임 대상 서버
    Serializer      runtime.NegotiatedSerializer
    // ... healthz, livez, readyz 핸들러
}
```

### Server Chain

3개의 서버가 위임 체인으로 연결:

```
Aggregator → KubeAPIServer → APIExtensions → 404(EmptyDelegate)
```

| 서버 | 처리 대상 | 소스 |
|------|----------|------|
| Aggregator | APIService, 외부 API 서버 | `staging/src/k8s.io/kube-aggregator/` |
| KubeAPIServer | 내장 API (core/v1, apps/v1, ...) | `pkg/controlplane/` |
| APIExtensions | CRD (Custom Resource) | `staging/src/k8s.io/apiextensions-apiserver/` |

### Handler Chain (필터 순서)

소스: `staging/src/k8s.io/apiserver/pkg/server/config.go:1028-1110`

```
1. Panic Recovery
2. Request Info (API Group, Verb, Resource 파싱)
3. Priority & Fairness (Rate Limiting)
4. Authentication
5. Audit Logging
6. Impersonation
7. Authorization
→ Resource Handler (Admission + Storage)
```

### 스토리지 레이어

```
REST Handler
  ↓
Store (generic registry)
  registry/generic/registry/store.go:100
  ↓
etcd3 Store
  storage/etcd3/store.go:80
  ↓
etcd 클러스터
```

**Store.Create() 흐름** (`store.go:446`):
1. `BeginCreate` hook
2. `rest.BeforeCreate()` — Strategy 기반 검증/기본값 설정
3. 어드미션 검증 콜백 실행
4. etcd 키 생성 (`/registry/{resource}/{namespace}/{name}`)
5. `e.Storage.Create()` — etcd에 저장
6. `AfterCreate` hook

## 2. kube-scheduler

### 역할

- 미할당 Pod(`spec.nodeName == ""`)를 감시
- Filter/Score 프레임워크로 최적 노드 선택
- Pod를 노드에 바인딩

### 핵심 구조체

소스: `pkg/scheduler/scheduler.go:68-124`

```go
type Scheduler struct {
    Cache           internalcache.Cache           // 노드/Pod 스냅샷 캐시
    SchedulingQueue internalqueue.SchedulingQueue  // Pod 스케줄링 큐
    Profiles        map[string]framework.Framework // 스케줄러 프로파일
    NextPod         func() (*framework.QueuedPodInfo, error) // 큐에서 Pod 꺼냄
    FailureHandler  func(*framework.QueuedPodInfo, ...)      // 실패 처리
}
```

### 스케줄링 사이클

소스: `pkg/scheduler/schedule_one.go`

```
ScheduleOne() (line 66)
  ↓
scheduleOnePod() (line 98)
  ├─ schedulingCycle() — 동기 (line 174)
  │   ├─ findNodesThatFitPod() (line 626)
  │   │   ├─ RunPreFilterPlugins()
  │   │   ├─ findNodesThatPassFilters() — 병렬 실행 (line 768)
  │   │   │   └─ 각 노드에 대해 Filter 플러그인 실행
  │   │   │       atomic.AddInt32로 가능 노드 카운트
  │   │   └─ findNodesThatPassExtenders()
  │   ├─ prioritizeNodes() (line 604)
  │   │   ├─ RunPreScorePlugins()
  │   │   └─ RunScorePlugins() — 각 노드 점수 계산
  │   ├─ selectHost() — 최고 점수 노드 선택
  │   ├─ assume() — 캐시에 미리 반영
  │   ├─ RunReservePlugins()
  │   └─ RunPermitPlugins()
  └─ bindingCycle() — 비동기 goroutine (line 390)
      ├─ WaitOnPermit()
      ├─ RunPreBindPlugins()
      ├─ RunBindPlugins() → API Server에 Binding 생성
      └─ RunPostBindPlugins()
```

### 프레임워크 확장 포인트 (12개)

| 확장 포인트 | 시점 | 용도 |
|------------|------|------|
| PreEnqueue | 큐 진입 전 | 스케줄링 게이트 |
| QueueSort | 큐 정렬 | 우선순위 결정 |
| PreFilter | 필터링 전 | 노드 필터링 힌트 |
| Filter | 각 노드 검사 | 노드 적합성 판단 |
| PostFilter | 필터 실패 시 | Preemption (선점) |
| PreScore | 점수 계산 전 | 공통 데이터 준비 |
| Score | 각 노드 점수 | 노드 선호도 계산 |
| Reserve | 노드 선택 후 | 리소스 예약 |
| Permit | 바인딩 전 | 승인 대기 (Gang Scheduling) |
| PreBind | 바인딩 직전 | 사전 준비 (볼륨 등) |
| Bind | 바인딩 | API에 Binding 생성 |
| PostBind | 바인딩 후 | 정리/알림 |

### 주요 내장 플러그인

소스: `pkg/scheduler/framework/plugins/`

| 플러그인 | 확장 포인트 | 역할 |
|----------|-----------|------|
| NodeResourcesFit | Filter, Score | CPU/Memory 리소스 적합성 |
| NodeAffinity | Filter, Score | 노드 어피니티 규칙 |
| InterPodAffinity | PreFilter, Filter, PreScore, Score | Pod 간 어피니티 |
| TaintToleration | Filter, Score | Taint/Toleration 매칭 |
| NodePorts | PreFilter, Filter | 포트 충돌 검사 |
| VolumeBinding | PreFilter, Filter, PreBind | PVC 바인딩 |
| DefaultPreemption | PostFilter | 선점(Preemption) 처리 |

## 3. kube-controller-manager

### 역할

- 40+ 개의 컨트롤러를 하나의 프로세스에서 실행
- 각 컨트롤러는 독립적인 컨트롤 루프 (goroutine)
- 리더 선출로 HA 보장

### 컨트롤러 패턴

모든 컨트롤러의 기본 패턴:

```go
// 1. Informer 등록 (Watch)
deploymentInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
    AddFunc:    controller.addDeployment,
    UpdateFunc: controller.updateDeployment,
    DeleteFunc: controller.deleteDeployment,
})

// 2. 이벤트 핸들러에서 WorkQueue에 키 추가
func (dc *DeploymentController) addDeployment(obj interface{}) {
    key, _ := cache.MetaNamespaceKeyFunc(obj)  // "namespace/name"
    dc.queue.Add(key)
}

// 3. Worker goroutine에서 큐 처리
func (dc *DeploymentController) worker() {
    for dc.processNextWorkItem() {}
}

func (dc *DeploymentController) processNextWorkItem() bool {
    key, quit := dc.queue.Get()
    if quit { return false }
    defer dc.queue.Done(key)

    err := dc.syncHandler(key.(string))  // syncDeployment
    dc.handleErr(err, key)
    return true
}
```

### 주요 컨트롤러

소스: `pkg/controller/`

| 컨트롤러 | Watch 대상 | 조정 대상 | 파일 |
|----------|-----------|----------|------|
| Deployment | Deployment, RS, Pod | ReplicaSet 생성/스케일 | `deployment/deployment_controller.go` |
| ReplicaSet | ReplicaSet, Pod | Pod 생성/삭제 | `replicaset/replica_set.go` |
| StatefulSet | StatefulSet, Pod | Pod 순서 보장 생성/삭제 | `statefulset/stateful_set.go` |
| DaemonSet | DaemonSet, Node, Pod | 모든 노드에 Pod 배치 | `daemon/daemon_controller.go` |
| Job | Job, Pod | 완료까지 Pod 실행 | `job/job_controller.go` |
| CronJob | CronJob, Job | 크론 스케줄에 Job 생성 | `cronjob/cronjob_controllerv2.go` |
| Namespace | Namespace | 네임스페이스 삭제 시 정리 | `namespace/namespace_controller.go` |
| GarbageCollector | 모든 리소스 | OwnerReference 기반 삭제 | `garbagecollector/garbagecollector.go` |
| NodeLifecycle | Node | 노드 장애 감지, Taint 추가 | `nodelifecycle/node_lifecycle_controller.go` |
| ServiceAccount | Namespace | 기본 SA 생성 | `serviceaccount/serviceaccounts_controller.go` |
| EndpointSlice | Service, Pod, Node | EndpointSlice 관리 | `endpointslice/endpointslice_controller.go` |

### 리더 선출

소스: `cmd/kube-controller-manager/app/controllermanager.go`

```go
// Lease 기반 리더 선출 (line 785)
leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
    Lock: &resourcelock.LeaseLock{
        LeaseMeta: metav1.ObjectMeta{
            Name:      "kube-controller-manager",
            Namespace: "kube-system",
        },
    },
    LeaseDuration: 15 * time.Second,
    RenewDeadline: 10 * time.Second,
    RetryPeriod:   2 * time.Second,
    Callbacks: leaderelection.LeaderCallbacks{
        OnStartedLeading: func(ctx) {
            // 컨트롤러 시작
            run(ctx)
        },
    },
})
```

### Expectations 패턴

ReplicaSet 컨트롤러가 사용하는 동시성 제어 메커니즘:

```
1. Pod 3개 생성 요청 → Expectations: {creates: 3, deletes: 0}
2. Pod 1개 생성 이벤트 → CreationObserved() → {creates: 2}
3. Pod 1개 생성 이벤트 → CreationObserved() → {creates: 1}
4. Pod 1개 생성 이벤트 → CreationObserved() → {creates: 0}
5. Expectations 충족 → 다음 syncReplicaSet() 실행

왜 필요한가?
→ Watch 이벤트가 도착하기 전에 syncReplicaSet()이 다시 실행되면
→ "아직 Pod 0개네?" → 또 3개 생성 → 중복 생성 방지
```

## 4. kubelet

### 역할

- 각 노드에서 Pod 실행·관리
- CRI (Container Runtime Interface)를 통해 컨테이너 관리
- Pod 상태를 API Server에 보고

### 핵심 구조체

소스: `pkg/kubelet/kubelet.go:1137-1286`

```go
type Kubelet struct {
    podManager        kubepod.Manager          // 원하는 Pod 상태 (API 소스)
    podWorkers        PodWorkers               // Pod 라이프사이클 상태 머신
    pleg              pleg.PodLifecycleEventGenerator  // 컨테이너 상태 감지
    statusManager     status.Manager           // Pod 상태 API 동기화
    volumeManager     volumemanager.VolumeManager      // 볼륨 관리
    containerRuntime  kubecontainer.Runtime     // CRI 인터페이스
    probeManager      prober.Manager           // Probe 관리
    evictionManager   eviction.Manager         // 리소스 압박 시 축출
}
```

### Sync Loop

소스: `pkg/kubelet/kubelet.go:2505-2700`

```go
func (kl *Kubelet) syncLoop(ctx context.Context, updates <-chan kubetypes.PodUpdate, handler SyncHandler) {
    syncTicker := time.NewTicker(time.Second)          // 1초 주기
    housekeepingTicker := time.NewTicker(housekeepingPeriod) // 30초 주기
    plegCh := kl.pleg.Watch()                          // PLEG 이벤트

    for {
        kl.syncLoopIteration(ctx, updates, handler,
            syncTicker.C, housekeepingTicker.C, plegCh)
    }
}
```

### PLEG (Pod Lifecycle Event Generator)

소스: `pkg/kubelet/pleg/generic.go:53-85`

```go
type GenericPLEG struct {
    runtime      kubecontainer.Runtime    // CRI
    eventChannel chan *PodLifecycleEvent  // kubelet으로 전달
    podRecords   podRecords              // 이전/현재 Pod 상태
    cache        kubecontainer.Cache     // 런타임 캐시
}
```

**Relist 동작** (`generic.go:234`):
1. CRI로 모든 Pod/컨테이너 상태 조회
2. 이전 상태와 비교하여 변화 감지
3. `ContainerStarted`, `ContainerDied` 등 이벤트 생성
4. 이벤트 채널로 전송 → syncLoop에서 처리

### Pod Worker 상태 머신

소스: `pkg/kubelet/pod_workers.go:157`

```
SyncPod (실행 중)
  │
  │ 삭제 요청
  ▼
TerminatingPod (종료 중)
  │
  │ 컨테이너 모두 종료
  ▼
TerminatedPod (종료 완료)
  │
  │ 정리 완료
  ▼
[삭제]
```

### CRI (Container Runtime Interface)

소스: `staging/src/k8s.io/cri-api/pkg/apis/services.go`

```
RuntimeService (line 114-128)
  ├─ PodSandboxManager (line 69-91)
  │   ├─ RunPodSandbox()      — Pod 네트워크 네임스페이스 생성
  │   ├─ StopPodSandbox()     — Pod 샌드박스 중지
  │   ├─ RemovePodSandbox()   — 정리
  │   └─ PodSandboxStatus()   — 상태 조회
  │
  ├─ ContainerManager (line 34-65)
  │   ├─ CreateContainer()    — 컨테이너 생성
  │   ├─ StartContainer()     — 컨테이너 시작
  │   ├─ StopContainer()      — 컨테이너 중지
  │   ├─ RemoveContainer()    — 컨테이너 삭제
  │   ├─ ListContainers()     — 목록 조회
  │   └─ ContainerStatus()    — 상태 조회
  │
  └─ ContainerStatsManager (line 95-110)
      ├─ ContainerStats()     — 컨테이너 메트릭
      └─ ListContainerStats() — 전체 메트릭

ImageManagerService (line 133-146)
  ├─ PullImage()              — 이미지 다운로드
  ├─ ListImages()             — 이미지 목록
  ├─ ImageStatus()            — 이미지 상태
  └─ RemoveImage()            — 이미지 삭제
```

### SyncPod 흐름

소스: `pkg/kubelet/kubelet.go:1947`

```
SyncPod(ctx, updateType, pod, mirrorPod, podStatus)
  │
  ├─ 1. generateAPIPodStatus()     // API 상태 생성
  ├─ 2. 터미널 상태 확인 (Succeeded/Failed)
  ├─ 3. Pod 데이터 디렉토리 생성
  ├─ 4. Volume attach/mount 대기
  ├─ 5. ImagePull Secret 가져오기
  ├─ 6. containerRuntime.SyncPod() // CRI 호출
  │     ├─ Pod Sandbox 생성/확인
  │     ├─ Init Container 실행
  │     └─ 일반 Container 시작
  └─ 7. 트래픽 셰이핑 적용
```

## 컴포넌트 간 상호작용 요약

```
┌─────────────────────────────────────────────────────────────────┐
│                                                                 │
│  ┌──────────┐   Watch    ┌─────────────┐   Watch   ┌────────┐ │
│  │Controller├───────────→│             │←──────────┤Scheduler│ │
│  │ Manager  │   Create   │   API       │  Bind     │        │ │
│  │          ├───────────→│   Server    │←──────────┤        │ │
│  └──────────┘            │             │           └────────┘ │
│                          │  ┌───────┐  │                      │
│  ┌──────────┐   Watch    │  │ etcd  │  │                      │
│  │ kubelet  ├───────────→│  └───────┘  │                      │
│  │          │  Status    │             │                      │
│  │          ├───────────→│             │                      │
│  └──────────┘            └─────────────┘                      │
│       │                                                       │
│       │ CRI                                                   │
│       ▼                                                       │
│  ┌──────────┐                                                 │
│  │containerd│                                                 │
│  └──────────┘                                                 │
│                                                               │
└─────────────────────────────────────────────────────────────────┘
```

- **Controller Manager**: Deployment/RS/Pod 등의 Watch → 상태 조정 → API 업데이트
- **Scheduler**: 미할당 Pod Watch → Filter/Score → Binding 생성
- **Kubelet**: 자기 노드의 Pod Watch → CRI로 컨테이너 실행 → 상태 보고
- **API Server**: 모든 요청의 허브, etcd 유일 접근자
