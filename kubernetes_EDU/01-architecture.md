# 01. Kubernetes 아키텍처

## 개요

Kubernetes는 **선언적 API**와 **컨트롤 루프** 패턴을 핵심으로 하는 분산 시스템이다.
사용자가 원하는 상태(desired state)를 선언하면, 컨트롤러들이 현재 상태를 원하는 상태로 수렴(reconcile)시킨다.

## 전체 아키텍처

```
┌──────────────────────────────────────────────────────────────────────┐
│                         Control Plane                                │
│                                                                      │
│  ┌─────────────────┐  ┌──────────────────┐  ┌────────────────────┐  │
│  │  kube-apiserver  │  │ kube-controller- │  │  kube-scheduler    │  │
│  │                  │  │    manager       │  │                    │  │
│  │  - REST API      │  │                  │  │  - Filter/Score    │  │
│  │  - Admission     │  │  - Deployment    │  │  - Framework       │  │
│  │  - Auth/AuthZ    │  │  - ReplicaSet    │  │  - Preemption      │  │
│  │  - Validation    │  │  - Service       │  │                    │  │
│  │  - Storage(etcd) │  │  - Node          │  │                    │  │
│  └────────┬─────────┘  │  - GC/TTL        │  └────────────────────┘  │
│           │            └──────────────────┘                          │
│           │                                                          │
│  ┌────────▼─────────┐                                                │
│  │      etcd         │  ← 유일한 상태 저장소                         │
│  │  (분산 KV store)  │                                                │
│  └──────────────────┘                                                │
└──────────────────────────────────────────────────────────────────────┘
          │              │              │
          │   Watch/API  │              │
          ▼              ▼              ▼
┌──────────────────────────────────────────────────────────────────────┐
│                          Worker Nodes                                │
│                                                                      │
│  ┌──────────────────┐  ┌───────────────┐  ┌───────────────────────┐ │
│  │     kubelet       │  │  kube-proxy    │  │  Container Runtime   │ │
│  │                   │  │                │  │  (containerd/CRI-O)  │ │
│  │  - Pod 관리       │  │  - iptables    │  │                      │ │
│  │  - CRI 호출       │  │  - IPVS        │  │  - CRI 인터페이스    │ │
│  │  - PLEG           │  │  - Service →   │  │  - 컨테이너 실행     │ │
│  │  - Volume Mount   │  │    Endpoint    │  │  - 이미지 관리       │ │
│  │  - Probe 관리     │  │    라우팅      │  │                      │ │
│  └──────────────────┘  └───────────────┘  └───────────────────────┘ │
└──────────────────────────────────────────────────────────────────────┘
```

## 핵심 컴포넌트

### Control Plane 컴포넌트

| 컴포넌트 | 역할 | 진입점 |
|----------|------|--------|
| kube-apiserver | REST API 서버, 모든 컴포넌트의 허브 | `cmd/kube-apiserver/apiserver.go` |
| kube-controller-manager | 컨트롤러 집합, 상태 조정 | `cmd/kube-controller-manager/controller-manager.go` |
| kube-scheduler | Pod를 노드에 배치 | `cmd/kube-scheduler/scheduler.go` |
| etcd | 분산 키-값 저장소 (외부 컴포넌트) | - |

### Worker Node 컴포넌트

| 컴포넌트 | 역할 | 진입점 |
|----------|------|--------|
| kubelet | 노드에서 Pod 실행·관리 | `cmd/kubelet/kubelet.go` |
| kube-proxy | Service → Pod 네트워크 라우팅 | `cmd/kube-proxy/proxy.go` |
| Container Runtime | CRI를 통한 컨테이너 실행 | 외부 (containerd, CRI-O) |

## 초기화 흐름

### 공통 패턴

모든 컴포넌트는 동일한 초기화 패턴을 따른다:

```
main() → app.NewXxxCommand() → cobra.Command.RunE → Run()
```

실제 소스코드 (`cmd/kube-apiserver/apiserver.go:32-36`):
```go
func main() {
    command := app.NewAPIServerCommand()
    code := cli.Run(command)
    os.Exit(code)
}
```

### kube-apiserver 초기화

소스: `cmd/kube-apiserver/app/server.go`

```
NewAPIServerCommand()
  ├─ options.NewServerRunOptions()        // 옵션 초기화 (line 71)
  ├─ SetupSignalContext()                 // 시그널 핸들러 (line 72)
  └─ RunE:
      ├─ s.Complete()                     // 옵션 완성 (line 104)
      ├─ completedOptions.Validate()      // 옵션 검증 (line 110)
      └─ Run(ctx, completedOptions)       // 실행 (line 117)
          ├─ NewConfig()                  // 설정 생성 (line 154)
          ├─ config.Complete()            // 설정 완성 (line 158)
          ├─ CreateServerChain()          // 서버 체인 생성 (line 162)
          │   ├─ apiExtensionsServer     // CRD 처리
          │   ├─ kubeAPIServer           // 핵심 API
          │   └─ aggregatorServer        // API 집계
          ├─ server.PrepareRun()          // 실행 준비 (line 167)
          └─ prepared.Run(ctx)            // HTTP 서버 시작 (line 172)
```

### kube-controller-manager 초기화

소스: `cmd/kube-controller-manager/app/controllermanager.go`

```
NewControllerManagerCommand()
  └─ RunE:
      ├─ s.Config()                       // 설정 생성 (line 138)
      └─ Run(ctx, c.Complete())           // 실행 (line 148)
          ├─ 리더 선출                     // Lease 기반
          ├─ CreateControllerContext()     // Informer 팩토리 생성
          ├─ StartControllers()           // 컨트롤러 시작
          └─ InformerFactory.Start()      // Watch 시작
```

### kube-scheduler 초기화

소스: `cmd/kube-scheduler/app/server.go`

```
NewSchedulerCommand()
  └─ RunE:
      ├─ Setup()                          // 스케줄러 설정
      └─ Run()                            // 실행
          ├─ 리더 선출
          ├─ InformerFactory.Start()      // Watch 시작
          └─ sched.Run(ctx)              // 스케줄링 루프
              └─ wait.UntilWithContext(ctx, sched.ScheduleOne, 0)
```

### kubelet 초기화

소스: `cmd/kubelet/app/server.go`

```
NewKubeletCommand(ctx)
  └─ RunE:
      ├─ 설정 로드 (KubeletConfiguration)
      ├─ NewMainKubelet()                 // Kubelet 구조체 초기화
      │   ├─ podManager 생성
      │   ├─ podWorkers 생성
      │   ├─ PLEG 생성
      │   ├─ statusManager 생성
      │   └─ volumeManager 생성
      └─ kubelet.Run()
          ├─ pleg.Start()                 // PLEG 시작
          ├─ statusManager.Start()        // 상태 동기화
          ├─ volumeManager.Run()          // 볼륨 관리
          └─ syncLoop()                   // 메인 동기화 루프
```

## API Server Delegation Chain

kube-apiserver는 3개의 서버가 위임(delegation) 체인으로 연결된다:

```
요청 → [Aggregator Server] → [Kube API Server] → [API Extensions Server] → 404
          │                       │                      │
          │ APIService 라우팅     │ 내장 API 처리         │ CRD 처리
          │ (extensions API)      │ (core/v1,             │ (custom
          │                       │  apps/v1, ...)        │  resources)
```

소스: `cmd/kube-apiserver/app/server.go:176-197`

```go
func CreateServerChain(config CompletedConfig) (*aggregatorapiserver.APIAggregator, error) {
    // 1. API Extensions (CRD) - 체인의 끝 (notFoundHandler로 위임)
    apiExtensionsServer, err := config.ApiExtensions.New(
        genericapiserver.NewEmptyDelegateWithCustomHandler(notFoundHandler))

    // 2. Kube API Server - API Extensions에 위임
    kubeAPIServer, err := config.KubeAPIs.New(apiExtensionsServer.GenericAPIServer)

    // 3. Aggregator - Kube API Server에 위임 (체인의 시작)
    aggregatorServer, err := controlplaneapiserver.CreateAggregatorServer(
        config.Aggregator, kubeAPIServer.ControlPlane.GenericAPIServer, ...)

    return aggregatorServer, nil
}
```

## 핵심 설계 원칙

### 1. 선언적 API (Declarative API)

```
사용자: "nginx Pod 3개 실행해줘" (Deployment.spec.replicas=3)
  ↓
시스템: 현재 0개 → 3개로 수렴
  ↓
컨트롤러: ReplicaSet 생성 → Pod 3개 생성 → 스케줄링 → 실행
```

### 2. 컨트롤 루프 (Control Loop)

모든 컨트롤러의 기본 패턴:

```
for {
    desired := getDesiredState()     // API Server에서 조회
    current := getCurrentState()     // 실제 상태 확인
    diff := desired - current        // 차이 계산
    reconcile(diff)                  // 차이 해소
}
```

### 3. Watch 기반 이벤트 드리븐

- 컨트롤러는 API Server를 폴링하지 않는다
- Watch를 통해 변경 이벤트를 실시간 수신한다
- `Reflector` → `DeltaFIFO` → `Indexer` 구조로 로컬 캐시를 유지한다

### 4. 레벨 트리거 (Level-triggered)

```
Edge-triggered: "Pod가 추가되었다" → 이벤트 1번 처리
Level-triggered: "Pod가 3개여야 한다" → 현재 상태와 비교하여 항상 올바른 결과
```

- Kubernetes는 레벨 트리거 방식: 이벤트를 놓쳐도 다음 동기화에서 복구
- 주기적 재동기화(resync)로 일관성 보장

## 컴포넌트 간 통신

```
┌────────────┐     REST/Watch     ┌─────────────────┐
│ kubectl    │ ──────────────────→ │ kube-apiserver   │
└────────────┘                    │                   │
                                  │ (유일한 etcd      │
┌────────────┐     REST/Watch     │  접근 포인트)     │
│ kubelet    │ ←────────────────→ │                   │
└────────────┘                    └────────┬──────────┘
                                           │
┌────────────┐     REST/Watch              │ etcd client
│ scheduler  │ ←────────────────→          │
└────────────┘                    ┌────────▼──────────┐
                                  │      etcd          │
┌────────────┐     REST/Watch     └───────────────────┘
│ controller │ ←────────────────→
│  manager   │
└────────────┘
```

**핵심**: 모든 컴포넌트는 **kube-apiserver를 통해서만** 통신한다.
- etcd에 직접 접근하는 것은 kube-apiserver뿐
- 컴포넌트 간 직접 통신 없음 (느슨한 결합)
- Watch 메커니즘으로 변경 사항 실시간 전파

## 확장 포인트

| 확장 포인트 | 메커니즘 | 예시 |
|------------|----------|------|
| API 확장 | CRD, API Aggregation | Istio VirtualService |
| 스케줄링 | Scheduler Framework 플러그인 | 커스텀 Filter/Score |
| 어드미션 | Webhook (Mutating/Validating) | OPA Gatekeeper |
| 네트워킹 | CNI 플러그인 | Cilium, Calico |
| 스토리지 | CSI 드라이버 | EBS, GCE PD |
| 런타임 | CRI 구현체 | containerd, CRI-O |
