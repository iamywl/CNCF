# 20. 리더 선출과 Status 관리 Deep-Dive

> istiod HA를 위한 Kubernetes Lease 기반 리더 선출과 CRD 리소스 상태(Conditions) 업데이트 시스템

---

## 1. 개요

Istio의 컨트롤 플레인(istiod)은 고가용성(HA)을 위해 여러 인스턴스로 배포될 수 있다. 이때 두 가지 핵심 서브시스템이 협력한다:

1. **리더 선출 (`pilot/pkg/leaderelection/`)**: Kubernetes Lease/ConfigMap 기반으로 리더를 선출하고, 특정 컨트롤러를 리더에서만 실행하여 중복 작업과 충돌을 방지한다.

2. **Status 관리 (`pilot/pkg/status/`)**: 여러 컨트롤러가 CRD 리소스의 상태(status)를 업데이트할 때 쓰기를 최소화하고 충돌을 방지하는 중앙 관리 시스템이다.

```
┌─────────────────────────────────────────────────────────────┐
│                   istiod HA 아키텍처                          │
│                                                             │
│  istiod-1 (리더)         istiod-2 (팔로워)                    │
│  ┌─────────────────┐    ┌─────────────────┐                │
│  │ xDS Server      │    │ xDS Server      │  ← 모두 활성    │
│  │ Config Watch    │    │ Config Watch    │  ← 모두 활성    │
│  ├─────────────────┤    ├─────────────────┤                │
│  │ Status Writer   │    │ (비활성)         │  ← 리더만       │
│  │ Webhook Patcher │    │ (비활성)         │  ← 리더만       │
│  │ Ingress Sync    │    │ (비활성)         │  ← 리더만       │
│  │ Gateway Deploy  │    │ (비활성)         │  ← 리더만       │
│  └────────┬────────┘    └─────────────────┘                │
│           │                                                 │
│           ▼                                                 │
│    Kubernetes Lease/ConfigMap (잠금)                         │
└─────────────────────────────────────────────────────────────┘
```

---

## 2. 리더 선출 아키텍처

### 2.1 선출 대상 컨트롤러

```
// pilot/pkg/leaderelection/leaderelection.go 라인 38-62

컨트롤러별 선출 이름:
  NamespaceController            = "istio-namespace-controller-election"
  ClusterTrustBundleController   = "istio-clustertrustbundle-controller-election"
  ServiceExportController        = "istio-serviceexport-controller-election"
  IngressController              = "istio-leader" (레거시 이름)
  GatewayStatusController        = "istio-gateway-status-leader"
  StatusController               = "istio-status-leader"
  AnalyzeController              = "istio-analyze-leader"
  GatewayDeploymentController    = "istio-gateway-deployment" (Lease 사용)
  InferencePoolController        = "istio-gateway-inferencepool"
  NodeUntaintController          = "istio-node-untaint"
  IPAutoallocateController       = "istio-ip-autoallocate"
```

### 2.2 LeaderElection 구조체

```
// pilot/pkg/leaderelection/leaderelection.go 라인 67-97

LeaderElection:
  - namespace: string               // Lease/ConfigMap 네임스페이스
  - name: string                    // 이 인스턴스의 이름 (호스트명 기반)
  - runFns: []func(stop <-chan struct{})  // 리더 획득 시 실행할 함수들
  - client: kubernetes.Interface    // K8s 클라이언트
  - ttl: time.Duration              // 리스 TTL (기본 30초)

  리비전 관련:
  - revision: string               // 이 인스턴스의 리비전 ("default", "canary" 등)
  - perRevision: bool              // 리비전별 개별 리더 선출 여부
  - remote: bool                   // 원격 클러스터 여부
  - defaultWatcher: revisions.DefaultWatcher  // 기본 리비전 감시

  잠금 유형:
  - useLeaseLock: bool             // true: Lease, false: ConfigMap

  상태:
  - cycle: *atomic.Int32           // 선출 주기 카운터
  - electionID: string             // 선출 ID (선출 이름)
  - le: *k8sleaderelection.LeaderElector  // 실제 선출기
  - mu: sync.RWMutex               // 동시성 보호
```

### 2.3 선출 실행 흐름

```
// pilot/pkg/leaderelection/leaderelection.go 라인 99-140

Run(stop <-chan struct{}) 흐름:

  if !enabled:
    // 단일 노드 클러스터 - 리더 선출 생략
    for _, f := range runFns: go f(stop)
    <-stop
    return

  // 리더 선출 활성화
  go defaultWatcher.Run(stop)  // 기본 리비전 감시 시작

  for {  // 선출 루프 (잠금 상실 시 재시도)
    le = create()              // LeaderElector 생성
    cycle.Inc()                // 주기 증가
    ctx = context.WithCancel() // 취소 가능한 컨텍스트
    le.Run(ctx)                // 선출 참여 (블로킹)

    select {
    case <-stop:
      return                   // 명시적 종료
    default:
      // 잠금 상실 → 재시도
      log.Infof("Leader election cycle %v lost. Trying again", cycle)
    }
  }
```

### 2.4 잠금 생성 (create)

```
// pilot/pkg/leaderelection/leaderelection.go 라인 142-204

create() 흐름:

  1. 콜백 등록:
     OnStartedLeading: 리더 획득 시 모든 runFns 고루틴으로 실행
     OnStoppedLeading: 리더 상실 로깅

  2. 잠금 유형 선택:
     if perRevision || useLeaseLock:
       LeaseLock (Coordination API v1)
     else:
       ConfigMapLock (Core API v1)

  3. 설정:
     LeaseDuration: ttl (30초)
     RenewDeadline: ttl/2 (15초)
     RetryPeriod: ttl/4 (7.5초)
     ReleaseOnCancel: true (종료 시 잠금 해제)

  4. 우선순위 비교 함수 설정 (perRevision이 아닌 경우):
     config.KeyComparison = LocationPrioritizedComparison
```

---

## 3. 우선순위 기반 리더 선출

### 3.1 리비전 우선순위

Istio는 카나리 배포 시 여러 리비전이 공존할 수 있다. 이때 "default" 리비전이 우선권을 갖는다.

```
// pilot/pkg/leaderelection/leaderelection.go 라인 206-218

LocationPrioritizedComparison(currentLeaderRevision, l) bool:
  // 현재 리더가 원격인지 확인
  currentLeaderRemote = strings.HasPrefix(currentLeaderRevision, "^")

  // 케이스 1: 내가 default 리비전이고, 현재 리더가 아닌 경우 → 항상 빼앗기
  if l.revision != currentLeaderRevision &&
     defaultRevision != "" &&
     defaultRevision == l.revision:
    return true

  // 케이스 2: 같은 리비전이지만, 내가 로컬이고 현재 리더가 원격인 경우 → 빼앗기
  return l.revision == currentLeaderRevision &&
         !l.remote && currentLeaderRemote
```

### 3.2 원격 클러스터 표기

```
// pilot/pkg/leaderelection/leaderelection.go 라인 65
remoteIstiodPrefix = "^"

저장 형식:
  로컬 istiod: key = "default"
  원격 istiod: key = "^default"

우선순위: 로컬 > 원격 (같은 리비전일 때)
```

### 3.3 리비전별 선출 (PerRevision)

```
NewPerRevisionLeaderElection:
  - 리비전마다 별도의 Lease 생성
  - electionID = "istio-gateway-deployment-canary" (리비전 접미사)
  - KeyComparison 비활성화 (빼앗기 없음)
  - 항상 Lease 잠금 사용

사용 사례:
  GatewayDeploymentController - 각 리비전이 자신의 Gateway만 관리
```

---

## 4. 리더 선출 유형

### 4.1 ConfigMap 기반 (레거시)

```
k8sresourcelock.ConfigMapLock:
  - ConfigMapMeta: {Namespace, Name}  // ConfigMap 리소스
  - Client: CoreV1Client
  - LockConfig: {Identity, Key}      // 누가 잠금을 소유하는지

장점: 하위 호환성, 기존 배포와 충돌 방지
단점: ConfigMap은 범용 리소스라 오버헤드 있음
```

### 4.2 Lease 기반 (현대적)

```
k8sresourcelock.LeaseLock:
  - LeaseMeta: {Namespace, Name}     // Lease 리소스
  - Client: CoordinationV1Client
  - LockConfig: {Identity, Key}

장점: 전용 리소스라 가벼움, Kubernetes 권장 방식
사용: GatewayDeploymentController, PerRevision 유형
```

### 4.3 선출 비활성화

```
features.EnableLeaderElection = false 일 때:
  - watcher = nil
  - enabled = false
  - Run() 호출 시 모든 runFns 즉시 실행
  - isLeader() 항상 true 반환

사용 사례: 단일 인스턴스 개발/테스트 환경
```

---

## 5. Status 관리 아키텍처

### 5.1 Manager 구조체

```
// pilot/pkg/status/manager.go 라인 33-37

Manager:
  - store: model.ConfigStore     // Istio 설정 저장소
  - workers: WorkerQueue         // 상태 업데이트 워커 풀

생성 시 등록되는 함수:
  writeFunc: store.UpdateStatus(config) 호출
  retrieveFunc: store.Get(gvk, name, namespace) 호출
```

### 5.2 Status 업데이트 흐름

```
┌──────────────────────────────────────────────────────────┐
│              Status 업데이트 파이프라인                      │
│                                                          │
│  컨트롤러 A ──→ EnqueueStatusUpdate(context, target)     │
│  컨트롤러 B ──→ EnqueueStatusUpdate(context, target)     │
│                         │                                │
│                         ▼                                │
│                   ┌────────────┐                         │
│                   │ WorkQueue  │                         │
│                   │ (병합 큐)   │                         │
│                   └─────┬──────┘                         │
│                         │                                │
│                         ▼                                │
│                   ┌────────────┐                         │
│                   │ WorkerPool │                         │
│                   │ (동적 확장) │                         │
│                   └─────┬──────┘                         │
│                         │                                │
│                    1. Get(resource) 로 현재 상태 조회      │
│                    2. Generation 일치 확인                │
│                    3. SetObservedGeneration 설정          │
│                    4. 각 컨트롤러의 UpdateFunc 순차 호출    │
│                    5. store.UpdateStatus 로 API 서버 쓰기  │
└──────────────────────────────────────────────────────────┘
```

### 5.3 Resource 식별자

```
// pilot/pkg/status/resource.go 라인 39-44

Resource:
  - GroupVersionResource  // 리소스 종류 (임베드)
  - Namespace: string     // 네임스페이스
  - Name: string          // 이름
  - Generation: string    // 세대 번호 (문자열)

변환 함수:
  ResourceFromModelConfig(config.Config) → Resource
  ResourceFromMetadata(resource.Metadata) → Resource
  ToModelKey() → config.Key(group, version, kind, name, namespace)
```

### 5.4 WorkQueue 동작

```
// pilot/pkg/status/resourcelock.go 라인 68-126

WorkQueue:
  - tasks: []lockResource            // 대기 중인 작업 목록
  - cache: map[lockResource]cacheEntry  // 리소스별 최신 상태
  - lock: sync.Mutex                 // 동시성 보호

Push(target, controller, context):
  lock.Lock()
  if target이 이미 큐에 있음:
    perControllerStatus[controller] = context  // 기존 항목 업데이트
  else:
    새 cacheEntry 생성 + tasks에 추가
  lock.Unlock()
  OnPush() 호출  // 워커 생성 트리거

Pop(exclusion):
  lock.Lock()
  현재 작업 중인 리소스를 제외한 첫 번째 작업 반환
  lock.Unlock()

핵심 최적화:
  - 같은 리소스에 대한 여러 업데이트를 병합
  - 동일 리소스는 동시에 한 워커만 처리
```

### 5.5 WorkerPool (동적 확장 워커 풀)

```
// pilot/pkg/status/resourcelock.go 라인 128-224

WorkerPool:
  - q: WorkQueue                          // 작업 큐
  - write: func(*config.Config)           // 상태 쓰기 함수
  - get: func(Resource) *config.Config    // 설정 조회 함수
  - workerCount: uint                     // 현재 워커 수
  - maxWorkers: uint                      // 최대 워커 수
  - currentlyWorking: sets.Set[lockResource]  // 현재 작업 중인 리소스

maybeAddWorker():
  if workerCount >= maxWorkers || 큐가 비어있음:
    return
  workerCount++
  go func():
    for:
      큐에서 작업 가져오기 (Pop)
      현재 작업 중 리소스에 추가
      cfg = get(target)
      if cfg != nil && generation 일치:
        sm = GetStatusManipulator(cfg.Status)
        sm.SetObservedGeneration(cfg.Generation)
        for controller, context in perControllerWork:
          controller.fn(sm, context)  // 각 컨트롤러의 상태 변환 적용
        cfg.Status = sm.Unwrap()
        write(cfg)                    // API 서버에 쓰기
      현재 작업 중 리소스에서 제거
```

---

## 6. Status Manipulator 패턴

### 6.1 Manipulator 인터페이스

```
// pilot/pkg/status/resourcelock.go 라인 226-233

Manipulator 인터페이스:
  SetObservedGeneration(int64)        // 관찰된 세대 번호 설정
  SetValidationMessages(diag.Messages) // 검증 메시지 설정
  SetInner(any)                       // 내부 상태 설정
  Unwrap() any                        // 최종 상태 반환
```

### 6.2 구현체

```
IstioGenerationProvider:
  - 대상: v1alpha1.IstioStatus
  - SetObservedGeneration → i.ObservedGeneration = in
  - SetValidationMessages → i.ValidationMessages 갱신
  - 사용: VirtualService, DestinationRule 등 일반 Istio CRD

ServiceEntryGenerationProvider:
  - 대상: networking.ServiceEntryStatus
  - 동일한 인터페이스, ServiceEntry 전용 타입

NopStatusManipulator:
  - 대상: 알 수 없는 타입
  - 모든 메서드가 no-op (안전한 폴백)
```

### 6.3 GetStatusManipulator 팩토리

```
// pilot/pkg/status/resource.go 라인 80-88

GetStatusManipulator(in any) Manipulator:
  if in은 *v1alpha1.IstioStatus:
    return IstioGenerationProvider{in}
  elif in은 *networking.ServiceEntryStatus:
    return ServiceEntryGenerationProvider{in}
  else:
    return NopStatusManipulator{in}
```

---

## 7. StatusCollections (krt 통합)

### 7.1 StatusCollections 구조체

```
// pilot/pkg/status/collections.go 라인 38-43

StatusCollections:
  - mu: sync.Mutex
  - constructors: []func(Queue) krt.HandlerRegistration  // 등록된 생성자
  - active: []krt.HandlerRegistration                    // 활성 핸들러
  - queue: Queue                                         // 현재 큐 (nil이면 비활성)
```

### 7.2 동적 활성화/비활성화

```
SetQueue(queue):
  queue를 설정하고 모든 생성자를 실행하여 핸들러 등록
  → 리더 획득 시 호출

UnsetQueue():
  queue = nil로 설정하고 모든 활성 핸들러 해제
  → 리더 상실 시 호출
```

### 7.3 RegisterStatus 제네릭 함수

```
// pilot/pkg/status/collections.go 라인 78-112

RegisterStatus[I, IS] 흐름:
  1. StatusCollection에서 이벤트 수신
  2. 현재 상태(live)와 원하는 상태(desired) 비교
  3. 동일하면 스킵 (불필요한 쓰기 방지)
  4. 리비전 소유권 확인 (tagWatcher.IsMine)
  5. 삭제 이벤트면 빈 상태로 큐잉
  6. enqueueStatus로 업데이트 큐잉
```

---

## 8. 리더 선출과 Status 관리의 통합

```
┌──────────────────────────────────────────────────────────┐
│               리더 선출 ↔ Status 관리 통합                  │
│                                                          │
│  LeaderElection("istio-status-leader")                  │
│    .AddRunFunction(func(stop) {                         │
│        StatusCollections.SetQueue(statusQueue)           │
│        <-stop                                           │
│        StatusCollections.UnsetQueue()                    │
│    })                                                   │
│    .Run(stop)                                           │
│                                                          │
│  리더 획득 → SetQueue → 상태 업데이트 활성화               │
│  리더 상실 → UnsetQueue → 모든 핸들러 해제                 │
│                                                          │
│  다른 인스턴스가 리더가 되면:                               │
│  - 그 인스턴스의 SetQueue가 호출됨                         │
│  - 동일 리소스에 대한 쓰기가 단일 인스턴스에서만 발생        │
└──────────────────────────────────────────────────────────┘
```

---

## 9. 설계 결정과 "왜(Why)"

### 9.1 왜 ConfigMap과 Lease 두 가지 잠금을 지원하는가?

```
ConfigMap (레거시):
  - Istio 초기 버전부터 사용
  - 기존 배포와의 하위 호환성 필수
  - "우선순위 기반 리더 선출"이 ConfigMap에만 구현됨

Lease (현대적):
  - Kubernetes 공식 권장 방식
  - 가벼움 (전용 API, 불필요한 데이터 없음)
  - 새로운 컨트롤러(GatewayDeployment, PerRevision)에서 사용

마이그레이션 전략:
  - 기존 컨트롤러는 ConfigMap 유지 (하위 호환)
  - 새 컨트롤러는 Lease 사용
  - PerRevision 유형은 잠금 비용이 높으므로 Lease 필수
```

### 9.2 왜 리비전 우선순위 비교가 필요한가?

```
시나리오: 카나리 업그레이드
  1. istiod-1.20 (default 리비전) 실행 중 → 리더
  2. istiod-1.21 (canary 리비전) 배포 → 팔로워
  3. istiod-1.21을 default로 승격
  4. istiod-1.21이 default 리비전이므로 istiod-1.20에서 잠금 빼앗기

이 메커니즘 없이는:
  - 기존 리더가 TTL까지 유지 → 30초 동안 구버전이 상태 쓰기
  - 리비전 간 충돌 가능
```

### 9.3 왜 Status Manager에서 쓰기를 병합하는가?

```
문제: 여러 컨트롤러가 동일 리소스의 상태를 업데이트
  - Validation Controller: ValidationMessages 설정
  - Distribution Controller: ObservedGeneration 설정

병합 없이:
  API 서버에 2번 쓰기 → 409 Conflict 가능 + 네트워크 비용 2배

병합 시:
  1번의 Get + 모든 컨트롤러의 변환 순차 적용 + 1번의 UpdateStatus
  → 쓰기 횟수 최소화, 충돌 방지
```

### 9.4 왜 동적 워커 풀을 사용하는가?

```
고정 워커 풀:
  - 부하가 낮을 때 불필요한 고루틴 유지
  - 부하가 높을 때 처리량 부족

동적 확장:
  - 큐에 작업이 들어올 때만 워커 생성
  - maxWorkers까지만 확장
  - 큐가 비면 워커 자동 종료
  - features.StatusMaxWorkers로 상한 조정 가능
```

---

## 10. Generation 기반 일관성

```
Status 업데이트 시 Generation 확인:

  cfg = get(target)
  if cfg.Generation == target.Generation:
    // Generation이 일치할 때만 상태 업데이트
    sm.SetObservedGeneration(cfg.Generation)
    // ... 상태 변환 적용 ...
    write(cfg)

이유:
  - 상태 업데이트가 큐에 있는 동안 리소스의 spec이 변경될 수 있음
  - 구버전 spec에 대한 상태를 신버전에 적용하면 잘못된 상태 표시
  - Generation 비교로 오래된 상태 업데이트를 자동 폐기
```

---

## 11. 에러 처리

### 11.1 리더 선출 에러

```
create() 실패:
  - 입력이 프로그래밍적으로 결정되므로 panic
  - "LeaderElection creation failed: " + err.Error()

잠금 갱신 실패:
  - le.Run(ctx) 반환
  - 외부 루프에서 재시도 (무한)
  - log: "Leader election cycle %v lost. Trying again"
```

### 11.2 Status 업데이트 에러

```
writeFunc 에러 처리:
  if 409 Conflict:
    scope.Debugf("warning: object has changed")
    // 자동 재시도 (큐에서 다시 처리)
  else:
    scope.Errorf("Encountered unexpected error")
    // 다음 이벤트에서 재시도
```

---

## 12. 소스 코드 경로 정리

| 파일 | 역할 |
|------|------|
| `pilot/pkg/leaderelection/leaderelection.go` | LeaderElection 구현, 선출 루프, 우선순위 비교 |
| `pilot/pkg/leaderelection/k8sleaderelection/` | Kubernetes 리더 선출 포크 (커스텀 KeyComparison) |
| `pilot/pkg/leaderelection/k8sleaderelection/k8sresourcelock/` | ConfigMapLock, LeaseLock 구현 |
| `pilot/pkg/status/manager.go` | Status Manager, Controller 팩토리 |
| `pilot/pkg/status/resource.go` | Resource 식별자, StatusManipulator 팩토리 |
| `pilot/pkg/status/resourcelock.go` | WorkQueue, WorkerPool, Manipulator 인터페이스 |
| `pilot/pkg/status/collections.go` | StatusCollections, krt 통합, RegisterStatus |

---

## 13. 관련 PoC

- **poc-leader-election**: Kubernetes Lease 기반 리더 선출 시뮬레이션 (리비전 우선순위, 로컬/원격 비교)
- **poc-status-manager**: CRD 상태 관리 워커 풀 시뮬레이션 (병합 큐, 동적 확장, Generation 검증)
