# 11. Kubelet 심화

## 목차

1. [개요](#1-개요)
2. [Kubelet 구조체](#2-kubelet-구조체)
3. [NewMainKubelet 초기화 흐름](#3-newmainkubelet-초기화-흐름)
4. [Run과 syncLoop](#4-run과-syncloop)
5. [syncLoopIteration: 5개의 채널](#5-synclooiteration-5개의-채널)
6. [PLEG (Pod Lifecycle Event Generator)](#6-pleg-pod-lifecycle-event-generator)
7. [Pod Workers 상태 머신](#7-pod-workers-상태-머신)
8. [SyncPod 흐름](#8-syncpod-흐름)
9. [CRI 인터페이스](#9-cri-인터페이스)
10. [Container Manager와 QoS Cgroups](#10-container-manager와-qos-cgroups)
11. [Why: 이벤트 기반 + 주기적 재조정](#11-why-이벤트-기반--주기적-재조정)
12. [정리](#12-정리)

---

## 1. 개요

Kubelet은 Kubernetes 클러스터의 모든 노드에서 실행되는 에이전트로, 각 노드에서
Pod의 전체 생명주기를 관리한다. API 서버, 컨테이너 런타임, 볼륨, 네트워크 등
다양한 서브시스템을 조율하여 Pod가 spec대로 실행되도록 보장한다.

### 핵심 책임

| 역할 | 설명 |
|------|------|
| Pod 생명주기 관리 | 생성, 실행, 종료, 정리 |
| 컨테이너 런타임 관리 | CRI를 통한 컨테이너 조작 |
| 볼륨 마운트/언마운트 | Volume Manager를 통한 관리 |
| 노드 상태 보고 | Node Status, Lease를 API 서버에 보고 |
| 리소스 관리 | cgroups, eviction, QoS 관리 |
| 헬스 체크 | liveness, readiness, startup probe 실행 |

### 소스 코드 위치

```
pkg/kubelet/
├── kubelet.go              # 핵심: Kubelet 구조체, Run(), SyncPod(), syncLoop()
├── pod_workers.go          # Pod Workers 상태 머신
├── pleg/
│   ├── pleg.go             # PLEG 인터페이스
│   └── generic.go          # GenericPLEG 구현, Relist()
├── cm/
│   ├── container_manager.go # ContainerManager 인터페이스
│   └── qos_container_manager_linux.go
├── config/                 # Pod 소스 설정 (api, file, http)
├── container/              # 컨테이너 추상화
├── eviction/               # Eviction Manager
├── images/                 # Image GC
├── prober/                 # Health Check Prober
├── status/                 # Status Manager
└── volumemanager/          # Volume Manager
staging/src/k8s.io/cri-api/
└── pkg/apis/
    └── services.go         # CRI 인터페이스 정의
```

---

## 2. Kubelet 구조체

### 2.1 핵심 필드

`pkg/kubelet/kubelet.go` (라인 1137 이후)에 정의된 Kubelet 구조체의 주요 필드:

```go
type Kubelet struct {
    // === 설정 및 기본 정보 ===
    kubeletConfiguration kubeletconfiginternal.KubeletConfiguration
    hostname             string
    nodeName             types.NodeName

    // === API 클라이언트 ===
    kubeClient      clientset.Interface     // 일반 API 호출
    heartbeatClient clientset.Interface     // Node 상태/Lease 전용
    mirrorPodClient kubepod.MirrorClient    // Static Pod의 미러 Pod 관리

    // === 핵심 매니저들 ===
    podManager       kubepod.Manager        // 원하는 Pod 집합 관리
    podWorkers       PodWorkers             // Pod 생명주기 상태 머신
    evictionManager  eviction.Manager       // 리소스 압박 시 Pod 축출
    probeManager     prober.Manager         // liveness/readiness/startup 프로브
    volumeManager    volumemanager.VolumeManager
    statusManager    status.Manager         // API 서버에 Pod 상태 보고
    allocationManager allocation.Manager    // 리소스 할당 관리

    // === 캐시 및 시크릿 ===
    secretManager    secret.Manager         // Secret 캐싱
    configMapManager configmap.Manager      // ConfigMap 캐싱

    // === 컨테이너 런타임 ===
    containerRuntime kubecontainer.Runtime   // CRI 기반 런타임
    runtimeService   internalapi.RuntimeService
    runtimeCache     kubecontainer.RuntimeCache

    // === PLEG ===
    pleg        pleg.PodLifecycleEventGenerator
    eventedPleg pleg.PodLifecycleEventGenerator  // EventedPLEG (선택적)
    podCache    kubecontainer.Cache

    // === 프로브 매니저 ===
    livenessManager  proberesults.Manager
    readinessManager proberesults.Manager
    startupManager   proberesults.Manager

    // === 노드 관리 ===
    containerManager      cm.ContainerManager  // QoS cgroups, 리소스 관리
    nodeLeaseController   lease.Controller
    cgroupsPerQOS         bool
    nodeStatusUpdateFrequency time.Duration

    // === GC ===
    containerGC   kubecontainer.GC
    imageManager  images.ImageGCManager

    // === 기타 ===
    recorder      record.EventRecorderLogger
    sourcesReady  config.SourcesReady
    clock         clock.WithTicker
}
```

### 2.2 매니저 간 관계도

```
                        API Server
                            │
                ┌───────────┼───────────┐
                ▼           ▼           ▼
          ┌──────────┐ ┌────────┐ ┌──────────┐
          │ Pod      │ │ Node   │ │ Status   │
          │ Config   │ │ Lease  │ │ Manager  │
          │ (source) │ │ Ctrl   │ │          │
          └────┬─────┘ └────────┘ └────▲─────┘
               │                       │
               ▼                       │
          ┌──────────┐            ┌────┴─────┐
          │ Pod      │            │ Pod      │
          │ Manager  │◄──────────│ Workers  │
          │ (desired)│            │ (actual) │
          └────┬─────┘            └────┬─────┘
               │                       │
               │    ┌──────────────┐   │
               │    │ syncLoop     │   │
               └───►│ Iteration    │───┘
                    └──────┬───────┘
                           │
              ┌────────────┼────────────┐
              ▼            ▼            ▼
         ┌────────┐  ┌─────────┐  ┌─────────┐
         │ PLEG   │  │ Volume  │  │ Probe   │
         │        │  │ Manager │  │ Manager │
         └────┬───┘  └─────────┘  └─────────┘
              │
              ▼
         ┌──────────────┐
         │ Container    │
         │ Runtime(CRI) │
         └──────────────┘
```

---

## 3. NewMainKubelet 초기화 흐름

### 3.1 함수 시그니처

`pkg/kubelet/kubelet.go` (라인 420-446):

```go
func NewMainKubelet(ctx context.Context,
    kubeCfg *kubeletconfiginternal.KubeletConfiguration,
    kubeDeps *Dependencies,
    crOptions *kubeletconfig.ContainerRuntimeOptions,
    hostname string,
    nodeName types.NodeName,
    nodeIPs []net.IP,
    ...
    seccompDefault bool,
) (*Kubelet, error)
```

### 3.2 초기화 순서

```
NewMainKubelet()
│
├─ 1. 기본 검증
│     ├─ rootDirectory 비어있으면 에러
│     ├─ SyncFrequency > 0 검증
│     └─ cloudProvider 검증
│
├─ 2. Node Informer 설정
│     ├─ NodeInformer (자기 노드만 watch)
│     └─ kubeInformers.Start()
│
├─ 3. PodConfig 생성 (소스별 Pod 수신 채널)
│     makePodSourceConfig(kubeCfg, kubeDeps, nodeName)
│     → API Server, File, HTTP 소스로부터 Pod 변경 수신
│
├─ 4. GC 정책 설정
│     containerGCPolicy, imageGCPolicy
│
├─ 5. 핵심 Kubelet 구조체 생성
│     klet := &Kubelet{...}
│
├─ 6. Container Runtime 초기화
│     ├─ CRI 클라이언트 연결
│     └─ kuberuntime.NewKubeGenericRuntimeManager()
│
├─ 7. PLEG 생성
│     ├─ GenericPLEG (기본)
│     └─ EventedPLEG (feature gate에 따라)
│
├─ 8. 서브시스템 매니저 초기화
│     ├─ secretManager, configMapManager
│     ├─ podManager (kubepod.NewBasicPodManager)
│     ├─ statusManager
│     ├─ probeManager (liveness, readiness, startup)
│     ├─ volumeManager
│     ├─ evictionManager
│     └─ podWorkers (newPodWorkers)
│
├─ 9. Node Lease Controller
│     lease.NewController(...)
│
└─ 10. Kubelet 반환
```

### 3.3 Dependencies 구조체

`pkg/kubelet/kubelet.go` (라인 309-340):

```go
type Dependencies struct {
    Options []Option

    Auth                      server.AuthInterface
    CAdvisorInterface         cadvisor.Interface
    ContainerManager          cm.ContainerManager
    EventClient               v1core.EventsGetter
    HeartbeatClient           clientset.Interface
    KubeClient                clientset.Interface
    Mounter                   mount.Interface
    OOMAdjuster               *oom.OOMAdjuster
    OSInterface               kubecontainer.OSInterface
    PodConfig                 *config.PodConfig
    ProbeManager              prober.Manager
    Recorder                  record.EventRecorderLogger
    RemoteRuntimeService      internalapi.RuntimeService
    RemoteImageService        internalapi.ImageManagerService
    PodStartupLatencyTracker  util.PodStartupLatencyTracker
}
```

이 구조체는 Kubelet이 필요로 하는 외부 의존성을 캡슐화한다.
테스트 시 모킹이 용이하도록 인터페이스로 정의되어 있다.

---

## 4. Run과 syncLoop

### 4.1 Run() 메서드

`pkg/kubelet/kubelet.go` (라인 1780-1896):

```
kl.Run(ctx, updates)
│
├─ 1. 모듈 초기화
│     kl.initializeModules(ctx)
│
├─ 2. Allocation Manager 시작
│     kl.allocationManager.Run(ctx)
│
├─ 3. Volume Manager 시작
│     go kl.volumeManager.Run(ctx, kl.sourcesReady)
│
├─ 4. Node Status 동기화 (API 연결 시)
│     go wait.JitterUntil(kl.syncNodeStatus, nodeStatusUpdateFrequency, 0.04)
│     go kl.fastStatusUpdateOnce()        // 초기 빠른 업데이트
│     go kl.nodeLeaseController.Run(ctx)  // Lease 갱신
│     go kl.fastStaticPodsRegistration(ctx)
│
├─ 5. Runtime 상태 업데이트
│     go wait.UntilWithContext(ctx, kl.updateRuntimeUp, 5s)
│
├─ 6. iptables 유틸리티 체인 설정
│     kl.initNetworkUtil(logger)
│
├─ 7. Status Manager 시작
│     kl.statusManager.Start(ctx)
│
├─ 8. PLEG 시작
│     kl.pleg.Start()
│     if EventedPLEG: kl.eventedPleg.Start()
│
└─ 9. syncLoop 진입 (메인 루프)
      kl.syncLoop(ctx, updates, kl)
```

### 4.2 syncLoop

`pkg/kubelet/kubelet.go` (라인 2505-2546):

```go
func (kl *Kubelet) syncLoop(ctx context.Context,
    updates <-chan kubetypes.PodUpdate, handler SyncHandler) {

    syncTicker := time.NewTicker(time.Second)
    housekeepingTicker := time.NewTicker(housekeepingPeriod)  // 2초
    plegCh := kl.pleg.Watch()

    for {
        // 런타임 에러 시 지수 백오프
        if err := kl.runtimeState.runtimeErrors(); err != nil {
            time.Sleep(duration)  // 100ms ~ 5s 지수 증가
            continue
        }
        duration = base  // 성공 시 리셋

        kl.syncLoopMonitor.Store(kl.clock.Now())
        if !kl.syncLoopIteration(ctx, updates, handler,
            syncTicker.C, housekeepingTicker.C, plegCh) {
            break
        }
        kl.syncLoopMonitor.Store(kl.clock.Now())
    }
}
```

**상수 정의** (라인 148-239):

| 상수 | 값 | 설명 |
|------|-----|------|
| `housekeepingPeriod` | 2s | 하우스키핑 주기 |
| `housekeepingWarningDuration` | 1s | 하우스키핑이 느릴 때 경고 |
| `plegChannelCapacity` | 1000 | PLEG 이벤트 채널 버퍼 크기 |
| `genericPlegRelistPeriod` | 1s | PLEG relist 주기 |
| `genericPlegRelistThreshold` | 3m | PLEG 건강 체크 임계값 |
| `backOffPeriod` | 10s | Pod 동기화 실패 시 백오프 |
| `ContainerGCPeriod` | 1m | 컨테이너 GC 주기 |
| `ImageGCPeriod` | 5m | 이미지 GC 주기 |

---

## 5. syncLoopIteration: 5개의 채널

### 5.1 채널 아키텍처

`syncLoopIteration()` (라인 2580-2655)은 다섯 개의 채널에서 이벤트를 수신한다:

```
syncLoopIteration(ctx, configCh, handler, syncCh, housekeepingCh, plegCh)
│
├─ select:
│
├─ case u := <-configCh:           ① 설정 변경 채널
│     switch u.Op:
│       ADD      → handler.HandlePodAdditions(u.Pods)
│       UPDATE   → handler.HandlePodUpdates(u.Pods)
│       REMOVE   → handler.HandlePodRemoves(u.Pods)
│       RECONCILE→ handler.HandlePodReconcile(u.Pods)
│       DELETE   → handler.HandlePodUpdates(u.Pods)  // graceful delete
│
├─ case e := <-plegCh:             ② PLEG 이벤트 채널
│     if isSyncPodWorthy(e):
│       pod := kl.podManager.GetPodByUID(e.ID)
│       handler.HandlePodSyncs([]*v1.Pod{pod})
│     if e.Type == ContainerDied:
│       kl.cleanUpContainersInPod(e.ID, containerID)
│
├─ case <-syncCh:                  ③ 주기적 동기화 (1초)
│     podsToSync := kl.getPodsToSync()
│     handler.HandlePodSyncs(podsToSync)
│
├─ case update := <-kl.livenessManager.Updates():   ④ Liveness 프로브
│     if update.Result == Failure:
│       handleProbeSync(kl, update, handler, "liveness")
│
├─ case update := <-kl.readinessManager.Updates():  ④ Readiness 프로브
│     kl.statusManager.SetContainerReadiness(...)
│
├─ case update := <-kl.startupManager.Updates():    ④ Startup 프로브
│     kl.statusManager.SetContainerStartup(...)
│
└─ case <-housekeepingCh:          ⑤ 하우스키핑 (2초)
      handler.HandlePodCleanups(ctx)
```

### 5.2 채널별 상세 설명

| 번호 | 채널 | 주기 | 역할 |
|------|------|------|------|
| 1 | configCh | 이벤트 기반 | API/파일/HTTP 소스에서 Pod 설정 변경 수신 |
| 2 | plegCh | 1초 (relist) | 컨테이너 런타임 상태 변화 감지 |
| 3 | syncCh | 1초 | 백오프 만료된 Pod 재동기화 |
| 4 | health managers | 이벤트 기반 | 프로브 결과에 따른 컨테이너 재시작/상태 업데이트 |
| 5 | housekeepingCh | 2초 | 고아 Pod 정리, 종료된 컨테이너 GC |

### 5.3 SyncHandler 인터페이스

```go
// kubelet.go:283-290
type SyncHandler interface {
    HandlePodAdditions(ctx context.Context, pods []*v1.Pod)
    HandlePodUpdates(ctx context.Context, pods []*v1.Pod)
    HandlePodRemoves(ctx context.Context, pods []*v1.Pod)
    HandlePodReconcile(ctx context.Context, pods []*v1.Pod)
    HandlePodSyncs(ctx context.Context, pods []*v1.Pod)
    HandlePodCleanups(ctx context.Context) error
}
```

Kubelet 자체가 이 인터페이스를 구현한다: `kl.syncLoop(ctx, updates, kl)`.

### 5.4 이벤트 흐름 다이어그램

```
API Server ──Watch──► PodConfig ──configCh──┐
                                             │
Static Files ──Watch──► PodConfig ──────────┤
                                             │
                                             ▼
Container   ──PLEG──► plegCh ──────────► syncLoop
Runtime                                   Iteration
                                             ▲
                                             │
Probe Results ──► livenessManager ──────────┤
                  readinessManager ─────────┤
                  startupManager ───────────┤
                                             │
Timer(1s) ──────► syncCh ──────────────────┤
Timer(2s) ──────► housekeepingCh ──────────┘
```

---

## 6. PLEG (Pod Lifecycle Event Generator)

### 6.1 개요

PLEG(Pod Lifecycle Event Generator)는 컨테이너 런타임의 실제 상태를 주기적으로
조회(polling)하여, 컨테이너 생명주기 이벤트를 생성하는 서브시스템이다.

### 6.2 인터페이스 정의

`pkg/kubelet/pleg/pleg.go` (라인 67-75):

```go
type PodLifecycleEventGenerator interface {
    Start()
    Watch() chan *PodLifecycleEvent
    Healthy() (bool, error)
    SetPodWatchCondition(podUID types.UID, conditionKey string,
        condition WatchCondition)
}
```

### 6.3 이벤트 타입

`pkg/kubelet/pleg/pleg.go` (라인 38-52):

```go
const (
    ContainerStarted  PodLifeCycleEventType = "ContainerStarted"
    ContainerDied     PodLifeCycleEventType = "ContainerDied"
    ContainerRemoved  PodLifeCycleEventType = "ContainerRemoved"
    PodSync           PodLifeCycleEventType = "PodSync"
    ContainerChanged  PodLifeCycleEventType = "ContainerChanged"
    ConditionMet      PodLifeCycleEventType = "ConditionMet"
)
```

### 6.4 GenericPLEG 구조체

`pkg/kubelet/pleg/generic.go` (라인 53-85):

```go
type GenericPLEG struct {
    runtime         kubecontainer.Runtime   // CRI 런타임
    eventChannel    chan *PodLifecycleEvent  // 이벤트 출력 채널
    podRecords      podRecords              // 이전/현재 Pod 상태 기록
    relistTime      atomic.Value            // 마지막 relist 시간
    cache           kubecontainer.Cache     // Pod 상태 캐시
    clock           clock.Clock
    podsToReinspect map[types.UID]*kubecontainer.Pod
    stopCh          chan struct{}
    relistLock      sync.Mutex
    isRunning       bool
    relistDuration  *RelistDuration
    watchConditions map[types.UID]map[string]versionedWatchCondition
}
```

### 6.5 Relist 동작 원리

`Relist()` (라인 234-369)는 PLEG의 핵심이다:

```
Relist()
│
├─ 1. Lock 획득 (동시 relist 방지)
│     g.relistLock.Lock()
│
├─ 2. 컨테이너 런타임에서 모든 Pod 조회
│     podList, err := g.runtime.GetPods(ctx, true)
│     pods := kubecontainer.Pods(podList)
│
├─ 3. 현재 상태를 podRecords에 기록
│     g.podRecords.setCurrent(pods)
│
├─ 4. 각 Pod에 대해 이전/현재 상태 비교
│     for pid := range g.podRecords:
│         oldPod := g.podRecords.getOld(pid)
│         newPod := g.podRecords.getCurrent(pid)
│
│         // 모든 컨테이너의 상태 변화 감지
│         allContainers := getContainersFromPods(oldPod, newPod)
│         for _, container := range allContainers:
│             events += computeEvents(oldPod, newPod, container.ID)
│
├─ 5. 캐시 업데이트 및 상태 검사
│     status, updated, err := g.updateCache(ctx, pod, pid)
│     if err: needsReinspection[pid] = pod
│
├─ 6. Watch Condition 확인
│     for _, condition := range watchConditions:
│         if condition.condition(status):
│             completedConditions = append(...)
│     events += ConditionMet 이벤트
│
├─ 7. 이벤트 발송
│     for event := range events:
│         if event.Type != ContainerChanged:  // 필터링
│             g.eventChannel <- event
│
└─ 8. 캐시 타임스탬프 업데이트
      g.cache.UpdateTime(timestamp)
```

### 6.6 상태 전이 테이블

`generateEvents()` (라인 194-218)에서 이벤트를 생성하는 로직:

```
┌─────────────────┬────────────────┬──────────────────────────────┐
│  이전 상태      │  현재 상태     │  생성 이벤트                 │
├─────────────────┼────────────────┼──────────────────────────────┤
│  *              │  running       │  ContainerStarted            │
│  *              │  exited        │  ContainerDied               │
│  *              │  unknown       │  ContainerChanged            │
│  exited         │  non-existent  │  ContainerRemoved            │
│  *              │  non-existent  │  ContainerDied+Removed       │
│  *              │  (동일)        │  (없음)                      │
└─────────────────┴────────────────┴──────────────────────────────┘

상태 매핑 (plegContainerState):
  running      ← ContainerStateRunning
  exited       ← ContainerStateExited
  unknown      ← ContainerStateCreated, ContainerStateUnknown
  non-existent ← (컨테이너가 목록에 없음)
```

### 6.7 PLEG 건강 체크

```go
// generic.go:180-192
func (g *GenericPLEG) Healthy() (bool, error) {
    relistTime := g.getRelistTime()
    if relistTime.IsZero() {
        return false, fmt.Errorf("pleg has yet to be successful")
    }
    metrics.PLEGLastSeen.Set(float64(relistTime.Unix()))
    elapsed := g.clock.Since(relistTime)
    if elapsed > g.relistDuration.RelistThreshold {  // 기본 3분
        return false, fmt.Errorf("pleg was last seen active %v ago", elapsed)
    }
    return true, nil
}
```

**Why**: PLEG가 멈추면 kubelet이 컨테이너 상태 변화를 감지하지 못한다.
3분(RelistThreshold) 이내에 relist가 실행되지 않으면 PLEG unhealthy로
판단하고, kubelet의 /healthz 엔드포인트가 실패를 반환한다.
이로 인해 노드가 NotReady 상태가 될 수 있다.

---

## 7. Pod Workers 상태 머신

### 7.1 상태 정의

`pkg/kubelet/pod_workers.go` (라인 107-118):

```go
type PodWorkerState int

const (
    SyncPod        PodWorkerState = iota  // Pod 실행 중 (설정 중)
    TerminatingPod                         // 컨테이너 종료 중
    TerminatedPod                          // 정리 완료
)
```

### 7.2 상태 전이 다이어그램

```
                    ┌──────────────────────────────┐
                    │         UpdatePod()           │
                    │  (새 Pod 또는 업데이트 수신)   │
                    └──────────────┬───────────────┘
                                   │
                                   ▼
                    ┌──────────────────────────────┐
                    │         SyncPod              │
                    │  (Pod 실행, 컨테이너 관리)    │
                    │                              │
                    │  syncPod() 반복 호출          │
                    │  - 볼륨 마운트               │
                    │  - 이미지 풀                 │
                    │  - CRI 호출                  │
                    │  - 상태 업데이트             │
                    └──────────┬───────────────────┘
                               │
                    ┌──────────┼────────────┐
                    │          │            │
                    ▼          ▼            ▼
              isTerminal   DELETE       Eviction
              (Succeeded/  요청         요청
               Failed)
                    │          │            │
                    └──────────┼────────────┘
                               │
                               ▼
                    ┌──────────────────────────────┐
                    │      TerminatingPod           │
                    │  (컨테이너 종료 중)            │
                    │                              │
                    │  syncTerminatingPod() 호출    │
                    │  - gracePeriod 적용          │
                    │  - 컨테이너 stop             │
                    │  - 에러 시 재시도            │
                    └──────────────┬───────────────┘
                                   │
                              모든 컨테이너 종료
                                   │
                                   ▼
                    ┌──────────────────────────────┐
                    │       TerminatedPod           │
                    │  (리소스 정리)                │
                    │                              │
                    │  syncTerminatedPod() 호출     │
                    │  - 볼륨 언마운트             │
                    │  - cgroup 정리               │
                    │  - 로그 정리                 │
                    └──────────────┬───────────────┘
                                   │
                              정리 완료
                                   │
                                   ▼
                         SyncKnownPods에서
                         일정 시간 후 제거
```

### 7.3 PodWorkers 인터페이스

`pkg/kubelet/pod_workers.go` (라인 157-256):

```go
type PodWorkers interface {
    // 핵심 메서드
    UpdatePod(options UpdatePodOptions)
    SyncKnownPods(desiredPods []*v1.Pod) (knownPods map[types.UID]PodWorkerSync)

    // 상태 조회 메서드
    IsPodKnownTerminated(uid types.UID) bool
    CouldHaveRunningContainers(uid types.UID) bool
    ShouldPodBeFinished(uid types.UID) bool
    IsPodTerminationRequested(uid types.UID) bool
    ShouldPodContainersBeTerminating(uid types.UID) bool
    ShouldPodRuntimeBeRemoved(uid types.UID) bool
    ShouldPodContentBeRemoved(uid types.UID) bool
}
```

### 7.4 podSyncer 인터페이스

`pkg/kubelet/pod_workers.go` (라인 264-285):

```go
type podSyncer interface {
    // Pod 실행 상태 동기화 (생성, 업데이트)
    SyncPod(ctx, updateType, pod, mirrorPod, podStatus) (bool, error)

    // Pod 컨테이너 종료 (graceful termination)
    SyncTerminatingPod(ctx, pod, podStatus, gracePeriod, podStatusFn) error

    // 알 수 없는 런타임 Pod 종료
    SyncTerminatingRuntimePod(ctx, runningPod) error

    // Pod 리소스 정리 (볼륨, cgroup 등)
    SyncTerminatedPod(ctx, pod, podStatus) error
}
```

### 7.5 UpdatePod 요청 구조

```go
// pod_workers.go:82-103
type UpdatePodOptions struct {
    UpdateType     kubetypes.SyncPodType  // create, update, sync, kill
    StartTime      time.Time
    Pod            *v1.Pod
    MirrorPod      *v1.Pod
    RunningPod     *kubecontainer.Pod     // 설정 없는 런타임 Pod
    KillPodOptions *KillPodOptions
}

type KillPodOptions struct {
    CompletedCh     chan<- struct{}        // 종료 완료 시그널
    Evict           bool                  // 축출 여부
    PodStatusFunc   PodStatusFunc         // 상태 오버라이드
    PodTerminationGracePeriodSecondsOverride *int64
}
```

---

## 8. SyncPod 흐름

### 8.1 SyncPod 개요

`pkg/kubelet/kubelet.go` (라인 1898-1947)의 주석에서 SyncPod의 워크플로우를
명시적으로 설명한다:

```
SyncPod 워크플로우:
1. Pod 생성 시, pod worker 시작 레이턴시 기록
2. generateAPIPodStatus()로 PodStatus 생성
3. 처음 Running으로 보일 때 시작 레이턴시 기록
4. Status Manager에 상태 업데이트
5. Soft admission 실패 시 컨테이너 중지
6. 실행 가능한 Pod의 백그라운드 추적 시작
7. Static Pod면 미러 Pod 생성
8. Pod 데이터 디렉토리 생성
9. 볼륨 attach/mount 대기
10. Pull secrets 조회
11. 컨테이너 런타임의 SyncPod 콜백 호출
12. 트래픽 쉐이핑 업데이트
```

### 8.2 SyncPod 코드 흐름 상세

`SyncPod()` (라인 1947 이후):

```
SyncPod(ctx, updateType, pod, mirrorPod, podStatus)
│
├─ 1. OpenTelemetry 스팬 시작
│
├─ 2. 레이턴시 측정
│     if updateType == SyncPodCreate && firstSeenTime != zero:
│         metrics.PodWorkerStartDuration.Observe(...)
│
├─ 3. PodStatus 생성
│     apiPodStatus := kl.generateAPIPodStatus(pod, podStatus, false)
│
├─ 4. Terminal 상태 확인
│     if phase == Succeeded || phase == Failed:
│         kl.statusManager.SetPodStatus(pod, apiPodStatus)
│         return isTerminal=true
│
├─ 5. Running 시작 레이턴시 기록
│     if 이전 Pending && 현재 Running:
│         metrics.PodStartDuration.Observe(...)
│
├─ 6. 상태 업데이트
│     kl.statusManager.SetPodStatus(pod, apiPodStatus)
│
├─ 7. 네트워크 상태 확인
│     if kl.runtimeState.networkErrors() && !IsHostNetworkPod:
│         return error (NetworkNotReady)
│
├─ 8. Secret/ConfigMap 등록
│     kl.secretManager.RegisterPod(pod)
│     kl.configMapManager.RegisterPod(pod)
│
├─ 9. Cgroup 생성 (cgroups-per-qos)
│     pcm := kl.containerManager.NewPodContainerManager()
│     if !pcm.Exists(pod) && !firstSync:
│         kl.killPod()  // 기존 컨테이너 종료 후 재생성
│     pcm.EnsureExists(pod)
│
├─ 10. Static Pod 미러 생성
│     if IsStaticPod(pod):
│         kl.mirrorPodClient.CreateMirrorPod(pod)
│
├─ 11. 데이터 디렉토리 생성
│     kl.makePodDataDirs(pod)
│
├─ 12. 볼륨 마운트 대기
│     kl.volumeManager.WaitForAttachAndMount(ctx, pod)
│
├─ 13. Pull Secrets 조회
│     pullSecrets := kl.getPullSecretsForPod(pod)
│
├─ 14. CRI를 통한 컨테이너 동기화
│     result := kl.containerRuntime.SyncPod(ctx, pod, podStatus, pullSecrets, backOff)
│
└─ 15. 에러 처리
      에러 발생 시 event 기록, 다음 SyncPod에서 재시도
```

### 8.3 볼륨 마운트 대기

```
WaitForAttachAndMount(pod)
│
├─ Pod에 필요한 볼륨 목록 조회
├─ 각 볼륨에 대해:
│     ├─ Attach 확인 (CSI driver → node에 디바이스 연결)
│     ├─ Mount 확인 (디바이스 → 파일시스템 마운트)
│     └─ 준비 완료 대기
└─ 모든 볼륨 준비 완료 시 반환
   (타임아웃 시 에러 반환 → 다음 SyncPod에서 재시도)
```

---

## 9. CRI 인터페이스

### 9.1 CRI 개요

CRI(Container Runtime Interface)는 kubelet과 컨테이너 런타임 사이의 표준화된
gRPC 인터페이스이다. 이를 통해 kubelet은 containerd, CRI-O 등 다양한 런타임과
통신할 수 있다.

### 9.2 핵심 인터페이스

`staging/src/k8s.io/cri-api/pkg/apis/services.go`:

**RuntimeService** (라인 114):

```go
type RuntimeService interface {
    RuntimeVersioner
    ContainerManager
    PodSandboxManager
    ContainerStatsManager

    // Lifecycle
    UpdateRuntimeConfig(ctx, runtimeConfig) error
    Status(ctx, verbose) (*runtimeapi.StatusResponse, error)
    RuntimeConfig(ctx) (*runtimeapi.RuntimeConfigResponse, error)
}
```

**ContainerManager** (라인 34-65):

```go
type ContainerManager interface {
    CreateContainer(ctx, podSandboxID, config, sandboxConfig) (string, error)
    StartContainer(ctx, containerID) error
    StopContainer(ctx, containerID, timeout) error
    RemoveContainer(ctx, containerID) error
    ListContainers(ctx, filter) ([]*runtimeapi.Container, error)
    ContainerStatus(ctx, containerID, verbose) (*runtimeapi.ContainerStatusResponse, error)
    UpdateContainerResources(ctx, containerID, resources) error
    ExecSync(ctx, containerID, cmd, timeout) (stdout, stderr, error)
    Exec(ctx, request) (*runtimeapi.ExecResponse, error)
    Attach(ctx, request) (*runtimeapi.AttachResponse, error)
    ReopenContainerLog(ctx, containerID) error
    GetContainerEvents(ctx, ch, callback) error
}
```

**PodSandboxManager** (라인 69-91):

```go
type PodSandboxManager interface {
    RunPodSandbox(ctx, config, runtimeHandler) (string, error)
    StopPodSandbox(ctx, podSandboxID) error
    RemovePodSandbox(ctx, podSandboxID) error
    PodSandboxStatus(ctx, podSandboxID, verbose) (*runtimeapi.PodSandboxStatusResponse, error)
    ListPodSandbox(ctx, filter) ([]*runtimeapi.PodSandbox, error)
    PortForward(ctx, request) (*runtimeapi.PortForwardResponse, error)
}
```

### 9.3 CRI 호출 흐름 (Pod 생성)

```
kubelet.SyncPod()
    │
    ▼
containerRuntime.SyncPod(pod, podStatus, pullSecrets, backOff)
    │
    ├─ 1. computePodActions(pod, podStatus)
    │     → 어떤 컨테이너를 생성/재시작/유지할지 결정
    │
    ├─ 2. Sandbox 생성 (필요한 경우)
    │     └─ CRI: RunPodSandbox(config, runtimeHandler)
    │        → pause 컨테이너 생성 (네트워크 네임스페이스 설정)
    │
    ├─ 3. Init 컨테이너 실행 (순차)
    │     for initContainer := range pod.Spec.InitContainers:
    │         ├─ CRI: CreateContainer(sandboxID, config, sandboxConfig)
    │         ├─ CRI: StartContainer(containerID)
    │         └─ 완료 대기
    │
    └─ 4. 일반 컨테이너 실행 (병렬)
          for container := range pod.Spec.Containers:
              ├─ Image Pull (필요 시)
              ├─ CRI: CreateContainer(sandboxID, config, sandboxConfig)
              └─ CRI: StartContainer(containerID)
```

### 9.4 Sandbox (Pause 컨테이너)의 역할

```
┌─────────────────────────────────────────┐
│  Pod Sandbox (pause container)          │
│                                         │
│  - Network Namespace 소유              │
│  - IPC Namespace 소유                  │
│  - Pod의 IP 주소 보유                  │
│                                         │
│  ┌──────────┐ ┌──────────┐ ┌─────────┐ │
│  │Container │ │Container │ │Container│ │
│  │    A     │ │    B     │ │    C    │ │
│  │(app)     │ │(sidecar) │ │(log)    │ │
│  └──────────┘ └──────────┘ └─────────┘ │
│                                         │
│  공유: Network, IPC, PID(선택)         │
│  격리: Mount, UTS                      │
└─────────────────────────────────────────┘
```

---

## 10. Container Manager와 QoS Cgroups

### 10.1 ContainerManager 인터페이스

`pkg/kubelet/cm/container_manager.go` (라인 66 이후):

```go
type ContainerManager interface {
    Start(ctx, node, ActivePodsFunc, ...) error
    SystemCgroupsLimit() v1.ResourceList
    NewPodContainerManager() PodContainerManager
    GetMountedSubsystems() *CgroupSubsystems
    GetQOSContainersInfo() QOSContainersInfo
    GetNodeAllocatableReservation() v1.ResourceList
    GetCapacity(localStorageCapacityIsolation bool) v1.ResourceList
    UpdateQOSCgroups(logger) error
    GetResources(ctx, pod, container) (*kubecontainer.RunContainerOptions, error)
    InternalContainerLifecycle() InternalContainerLifecycle
}
```

### 10.2 QoS 클래스 분류

Kubernetes는 Pod의 리소스 requests/limits 설정에 따라 세 가지 QoS 클래스를
부여한다:

| QoS 클래스 | 조건 | 우선순위 |
|------------|------|----------|
| Guaranteed | 모든 컨테이너: requests == limits (CPU, Memory) | 최고 |
| Burstable | 최소 하나의 컨테이너: requests < limits | 중간 |
| BestEffort | 모든 컨테이너: requests와 limits 모두 미설정 | 최저 |

### 10.3 QoS Cgroup 계층 구조

```
                    /sys/fs/cgroup/
                          │
                          ▼
                    kubepods (Node Allocatable)
                    │
                    ├── kubepods-burstable
                    │   ├── pod-uid-1
                    │   │   ├── container-id-1
                    │   │   └── container-id-2
                    │   └── pod-uid-2
                    │       └── container-id-3
                    │
                    ├── kubepods-besteffort
                    │   └── pod-uid-3
                    │       └── container-id-4
                    │
                    └── pod-uid-4  (Guaranteed Pod는 직접 kubepods 하위)
                        ├── container-id-5
                        └── container-id-6

참고: cgroups-per-qos=true일 때의 구조 (기본값)
```

### 10.4 리소스 관리 시 Cgroup 설정

```
SyncPod() 과정에서:
│
├─ Pod QoS 클래스 결정
│   v1qos.GetPodQOS(pod) → Guaranteed | Burstable | BestEffort
│
├─ Pod Cgroup 생성
│   pcm := kl.containerManager.NewPodContainerManager()
│   pcm.EnsureExists(pod)
│   → /sys/fs/cgroup/kubepods[-burstable|-besteffort]/pod-{uid}/
│
├─ 컨테이너별 Cgroup 설정 (CRI가 처리)
│   CreateContainer() 시 resources 지정:
│   - cpu.shares, cpu.cfs_period_us, cpu.cfs_quota_us
│   - memory.limit_in_bytes
│   - pids.max
│
└─ QoS Cgroup 업데이트
    kl.containerManager.UpdateQOSCgroups()
    → 각 QoS 레벨의 총 리소스 한도 재계산
```

### 10.5 Eviction과 QoS 관계

리소스 압박 시 eviction 우선순위:

```
높은 축출 우선순위 ──────────────────────── 낮은 축출 우선순위

BestEffort          Burstable              Guaranteed
(requests 없음)     (부분 requests)         (requests==limits)
│                   │                       │
│ 먼저 축출         │ 다음 축출             │ 마지막 축출
│                   │                       │
│ memory.oom_score  │ memory.oom_score      │ memory.oom_score
│ = 1000            │ = 2~999               │ = -997
│ (가장 높음)       │ (사용량 비례)         │ (가장 낮음)
```

---

## 11. Why: 이벤트 기반 + 주기적 재조정

### 11.1 하이브리드 설계

Kubelet은 순수한 이벤트 기반도, 순수한 폴링 기반도 아닌 하이브리드 설계를 채택한다.

```
이벤트 기반 (저지연):
- configCh: API 서버로부터 Pod 변경 즉시 수신
- plegCh: 컨테이너 상태 변화 1초 내 감지
- probe updates: 헬스 체크 결과 즉시 반영

주기적 재조정 (안전성):
- syncCh (1초): 백오프 만료된 Pod 재동기화
- housekeepingCh (2초): 고아 리소스 정리
- PLEG relist (1초): 전체 컨테이너 상태 재조회
- Node status (기본 10초): 노드 상태 보고
```

### 11.2 왜 이벤트만으로 충분하지 않은가

| 시나리오 | 이벤트만 사용 시 문제 | 재조정이 해결 |
|----------|---------------------|-------------|
| CRI 이벤트 유실 | 컨테이너 상태 불일치 | PLEG relist가 보정 |
| 네트워크 일시 단절 | API 이벤트 누락 | syncCh가 재시도 |
| Kubelet 재시작 | 진행 중 작업 유실 | 전체 Pod 재동기화 |
| 볼륨 마운트 실패 | 재시도 불가 | backoff 후 syncCh가 재시도 |
| OOM kill | 프로세스 갑작스런 종료 | PLEG가 상태 변화 감지 |

### 11.3 왜 폴링만으로 충분하지 않은가

| 시나리오 | 폴링만 사용 시 문제 | 이벤트가 해결 |
|----------|---------------------|-------------|
| 새 Pod 배포 | 최대 polling interval만큼 지연 | configCh로 즉시 반응 |
| 컨테이너 crash | 감지 지연 | PLEG 1초 내 감지 |
| 프로브 실패 | 다음 폴링까지 대기 | probe update로 즉시 반응 |
| 스케일 요청 | 반응 지연 | API watch로 즉시 반응 |

### 11.4 PLEG의 폴링 주기 선택 (1초)

```
1초 relist의 비용:
- GetPods() CRI 호출 1회
- 컨테이너 수 비례 비교 연산
- 평균 10~100ms 소요

더 짧은 주기 (100ms)의 문제:
- CRI 호출 10배 증가 → 런타임 부하
- CPU 사용량 증가
- 배터리(edge) 문제

더 긴 주기 (10초)의 문제:
- 컨테이너 crash 감지 10초 지연
- 프로브 결과 반영 지연
- Pod 시작 레이턴시 증가

결론: 1초는 "적당한 감지 지연"과 "합리적인 리소스 사용" 사이의 균형점이다.
EventedPLEG(feature gate)는 CRI 이벤트 스트리밍을 사용하여
이 폴링의 필요성을 줄이려는 시도이다.
```

---

## 12. 정리

### 12.1 Kubelet 전체 아키텍처 요약

```
┌─────────────────────────────────────────────────────────┐
│                        Kubelet                           │
│                                                          │
│  ┌────────────────────────────────────────────────────┐  │
│  │                    syncLoop                         │  │
│  │                                                     │  │
│  │   configCh  plegCh  syncCh  probes  housekeeping   │  │
│  │      │        │       │       │         │          │  │
│  │      └────────┴───────┴───────┴─────────┘          │  │
│  │                       │                             │  │
│  │               syncLoopIteration                     │  │
│  │                       │                             │  │
│  │              HandlePodAdditions                      │  │
│  │              HandlePodUpdates                        │  │
│  │              HandlePodSyncs                          │  │
│  │              HandlePodCleanups                       │  │
│  └───────────────────────┬────────────────────────────┘  │
│                          │                                │
│                   ┌──────┴──────┐                         │
│                   │ Pod Workers │                         │
│                   │             │                         │
│                   │ SyncPod     │                         │
│                   │ Terminating │                         │
│                   │ Terminated  │                         │
│                   └──────┬──────┘                         │
│                          │                                │
│  ┌───────────┬───────────┼──────────┬───────────────┐    │
│  │           │           │          │               │    │
│  ▼           ▼           ▼          ▼               ▼    │
│ Status    Volume     Container   Probe          Eviction │
│ Manager   Manager    Runtime     Manager        Manager  │
│                      (CRI)                               │
│                        │                                  │
│                        ▼                                  │
│              ┌──────────────────┐                         │
│              │ containerd/CRI-O │                         │
│              └──────────────────┘                         │
│                                                          │
│  ┌──────────────────────┐  ┌────────────────────────┐   │
│  │ PLEG (1s relist)     │  │ Node Status (10s)      │   │
│  │ → runtime 상태 감시   │  │ → API 서버에 보고      │   │
│  └──────────────────────┘  │ Lease 갱신 (10s)       │   │
│                             └────────────────────────┘   │
└─────────────────────────────────────────────────────────┘
```

### 12.2 핵심 교훈 요약

| 항목 | 핵심 내용 |
|------|-----------|
| Kubelet 구조체 | 약 40개 이상의 매니저/서브시스템을 조율하는 메가 구조체 |
| 초기화 | NewMainKubelet에서 CRI 연결, PLEG, Pod Workers 등 순차 초기화 |
| syncLoop | 5개 채널(config, PLEG, sync, probe, housekeeping)에서 이벤트 수신 |
| PLEG | 1초 주기 relist로 컨테이너 상태 변화 감지, 3분 임계값 건강 체크 |
| Pod Workers | SyncPod → TerminatingPod → TerminatedPod 3단계 상태 머신 |
| SyncPod | 상태 생성 → 볼륨 마운트 → CRI SyncPod 순서로 실행 |
| CRI | RuntimeService + ContainerManager + PodSandboxManager gRPC 인터페이스 |
| QoS Cgroups | Guaranteed/Burstable/BestEffort 계층별 cgroup 관리 |
| 설계 원칙 | 이벤트 기반(저지연) + 주기적 재조정(안전성) 하이브리드 |

### 12.3 소스 코드 참조 요약

| 파일 | 핵심 함수/구조체 | 라인 |
|------|------------------|------|
| `pkg/kubelet/kubelet.go` | `type Kubelet struct` | 1137 |
| `pkg/kubelet/kubelet.go` | `NewMainKubelet()` | 420 |
| `pkg/kubelet/kubelet.go` | `Run()` | 1780 |
| `pkg/kubelet/kubelet.go` | `SyncPod()` | 1947 |
| `pkg/kubelet/kubelet.go` | `syncLoop()` | 2505 |
| `pkg/kubelet/kubelet.go` | `syncLoopIteration()` | 2580 |
| `pkg/kubelet/kubelet.go` | `SyncHandler` 인터페이스 | 283 |
| `pkg/kubelet/pod_workers.go` | `PodWorkerState` (SyncPod/Terminating/Terminated) | 107 |
| `pkg/kubelet/pod_workers.go` | `PodWorkers` 인터페이스 | 157 |
| `pkg/kubelet/pod_workers.go` | `podSyncer` 인터페이스 | 264 |
| `pkg/kubelet/pleg/pleg.go` | `PodLifecycleEventGenerator` 인터페이스 | 67 |
| `pkg/kubelet/pleg/pleg.go` | `PodLifeCycleEventType` 상수 | 38 |
| `pkg/kubelet/pleg/generic.go` | `GenericPLEG` | 53 |
| `pkg/kubelet/pleg/generic.go` | `Relist()` | 234 |
| `pkg/kubelet/pleg/generic.go` | `generateEvents()` | 194 |
| `pkg/kubelet/cm/container_manager.go` | `ContainerManager` 인터페이스 | 66 |
| `staging/src/k8s.io/cri-api/pkg/apis/services.go` | `RuntimeService` 인터페이스 | 114 |
| `staging/src/k8s.io/cri-api/pkg/apis/services.go` | `ContainerManager` (CRI) | 34 |
| `staging/src/k8s.io/cri-api/pkg/apis/services.go` | `PodSandboxManager` | 69 |
