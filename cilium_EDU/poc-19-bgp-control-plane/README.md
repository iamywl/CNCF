# PoC-19: Cilium BGP 제어플레인 시뮬레이션

## 개요

이 PoC는 Cilium의 BGP 제어플레인(BGP Control Plane) 핵심 아키텍처를 Go 표준 라이브러리만으로 시뮬레이션한다.
BGPRouterManager가 여러 BGP 인스턴스의 생명주기를 관리하고, ConfigReconciler 파이프라인이 우선순위 기반으로
피어 연결, Pod CIDR 광고, 서비스 VIP 광고를 수행하는 과정을 재현한다.

## 배경 지식

### BGP 제어플레인이란

BGP(Border Gateway Protocol)는 인터넷에서 자율 시스템(AS) 간 라우팅 정보를 교환하는 프로토콜이다.
Kubernetes 환경에서 BGP 제어플레인은 클러스터 내부의 네트워크 정보를 외부 네트워크 장비(ToR 스위치, 라우터)에
알리는 역할을 한다.

### Cilium에서 BGP가 필요한 이유

Kubernetes 클러스터에서 Pod은 내부 IP(예: 10.244.x.x)를 사용하는데, 이 IP는 클러스터 외부에서 직접 접근할 수 없다.
Cilium BGP 제어플레인은 다음 문제를 해결한다:

- **Pod CIDR 광고**: 각 노드의 Pod CIDR을 외부 라우터에 광고하여 외부에서 Pod으로 직접 통신 가능
- **서비스 VIP 광고**: LoadBalancer 타입 서비스의 External IP를 BGP로 광고하여 외부 트래픽 유입
- **ECMP 로드밸런싱**: 여러 노드가 동일 서비스 VIP를 광고하면 외부 라우터가 자동으로 트래픽 분산
- **MetalLB 대체**: 별도 로드밸런서 없이 BGP만으로 베어메탈 환경에서 서비스 노출 가능

### 실무 적용 사례

데이터센터 내 Kubernetes 클러스터를 운영할 때, 각 워커 노드가 ToR(Top-of-Rack) 스위치와 BGP 피어링을 맺고
자신의 Pod CIDR과 서비스 IP를 광고한다. 이를 통해 별도의 오버레이 네트워크나 외부 로드밸런서 없이
순수 L3 라우팅으로 네트워크를 구성할 수 있다.

## 시뮬레이션하는 개념

| 개념 | 실제 Cilium 코드 | 시뮬레이션 방식 |
|------|-----------------|----------------|
| BGP 라우터 인스턴스 | `pkg/bgp/types/bgp.go` (Router 인터페이스) | `SimBGPRouter` 구조체로 피어/경로 관리 |
| 인스턴스 매니저 | `pkg/bgp/manager/manager.go` (BGPRouterManager) | `BGPRouterManager`로 인스턴스 생명주기 관리 |
| Reconcile Diff | `pkg/bgp/manager/reconcileDiff` | `ReconcileDiff`로 register/withdraw/reconcile 분류 |
| Config Reconciler | `pkg/bgp/manager/reconciler/reconcilers.go` | `ConfigReconciler` 인터페이스 + 3개 구현체 |
| Neighbor Reconciler | `reconciler.NeighborReconciler` | `NeighborReconciler`로 피어 추가/제거 |
| PodCIDR Reconciler | `reconciler.PodCIDRReconciler` | `PodCIDRReconciler`로 Pod CIDR 경로 광고/철회 |
| Service Reconciler | `reconciler.ServiceReconciler` | `ServiceReconciler`로 서비스 VIP 경로 광고/철회 |
| 이벤트 기반 Controller | `pkg/bgp/agent/controller.go` | `Controller`가 시그널 채널로 리컨실레이션 트리거 |
| CiliumBGPNodeConfig CR | Kubernetes CRD | `BGPNodeConfig` 구조체로 선언적 설정 표현 |

## 아키텍처 다이어그램

```
                    +-----------------------+
                    |  SetNodeConfig()      |
                    |  (CiliumBGPNodeConfig)|
                    +----------+------------+
                               |
                               v
                    +----------+------------+
                    |     Controller        |
                    |  signalCh <- struct{} |
                    +----------+------------+
                               |
                               v
                    +----------+------------+
                    |  BGPRouterManager     |
                    |  ReconcileInstances() |
                    +----------+------------+
                               |
                    +----------+------------+
                    |    ReconcileDiff      |
                    |  +-register (신규)    |
                    |  +-withdraw (제거)    |
                    |  +-reconcile (갱신)   |
                    +----------+------------+
                               |
              +----------------+----------------+
              |                |                |
              v                v                v
     +--------+------+ +------+-------+ +------+--------+
     | PodCIDR       | | Service      | | Neighbor      |
     | Reconciler    | | Reconciler   | | Reconciler    |
     | (priority=30) | | (priority=40)| | (priority=60) |
     +--------+------+ +------+-------+ +------+--------+
              |                |                |
              v                v                v
     +--------+----------------+----------------+--------+
     |                 SimBGPRouter                       |
     |  AdvertisePath()  AddNeighbor()  WithdrawPath()   |
     +---------------------------------------------------+
              |                                |
              v                                v
     +-----------------+            +------------------+
     | 경로 테이블      |            | 피어 테이블       |
     | 10.244.1.0/24   |            | 10.0.0.254:65000 |
     | 10.244.2.0/24   |            | 10.0.0.253:65000 |
     | 192.168.100.10  |            +------------------+
     | 192.168.100.20  |
     +-----------------+
```

## 코드 해설

### 1. ReconcileDiff - 선언적 상태 비교 엔진

```go
type ReconcileDiff struct {
    register  []string    // 새로 생성할 인스턴스
    withdraw  []string    // 제거할 인스턴스
    reconcile []string    // 업데이트할 인스턴스
}
```

**무엇을 하는 코드인가**: 현재 실행 중인 BGP 인스턴스 목록과 원하는(desired) 설정을 비교하여,
어떤 인스턴스를 생성(register)하고, 제거(withdraw)하고, 갱신(reconcile)할지 분류한다.

**실제 Cilium 참고**: `pkg/bgp/manager/manager.go`의 `reconcileDiff` 구조체를 모방했다.
Cilium은 CiliumBGPNodeConfig CRD의 변경을 감지할 때마다 이 diff를 계산한다.

**왜 이렇게 구현했는가**: Kubernetes의 선언적(declarative) 패턴을 따른다. "현재 상태"와 "원하는 상태"의
차이만 계산하여 최소한의 변경만 수행하므로, 불필요한 피어 재연결이나 경로 재광고를 방지한다.

### 2. BGPRouterManager - 인스턴스 생명주기 관리자

```go
func (m *BGPRouterManager) ReconcileInstances(ctx context.Context, nodeConfig *BGPNodeConfig) error
```

**무엇을 하는 코드인가**: ReconcileDiff의 결과에 따라 BGP 인스턴스를 생성, 제거, 갱신하는 핵심 오케스트레이터이다.
인스턴스 생성 시 라우터를 만들고 모든 리컨실러를 초기화한 뒤 초기 리컨실레이션을 실행한다.

**실제 Cilium 참고**: `pkg/bgp/manager/manager.go`의 `BGPRouterManager.ConfigReconciler()`를 모방했다.
실제 코드에서는 `hive.Cell`로 의존성 주입을 받아 생성된다.

**왜 이렇게 구현했는가**: 하나의 노드에서 여러 ASN의 BGP 인스턴스를 동시에 운영할 수 있어야 하므로,
인스턴스를 맵으로 관리하고 각각 독립적으로 리컨실레이션을 수행한다.

### 3. ConfigReconciler 파이프라인 - 우선순위 기반 리컨실러 체인

```go
sort.Slice(reconcilers, func(i, j int) bool {
    return reconcilers[i].Priority() < reconcilers[j].Priority()
})
```

**무엇을 하는 코드인가**: PodCIDR(30) -> Service(40) -> Neighbor(60) 순서로 리컨실러를 정렬하여
실행한다. 경로를 먼저 광고한 뒤 피어를 연결하는 순서를 보장한다.

**실제 Cilium 참고**: `pkg/bgp/manager/reconciler/reconcilers.go`의 `ConfigReconciler` 인터페이스와
`GetActiveReconcilers()` 함수를 모방했다. 실제 Cilium에는 PreflightReconciler, RoutePolicyReconciler 등
더 많은 리컨실러가 존재한다.

**왜 이렇게 구현했는가**: 리컨실러 간 의존성을 우선순위로 해결한다. 경로가 준비된 상태에서 피어를 연결해야
피어 설정 직후 경로가 즉시 광고될 수 있다. 새 리컨실러 추가 시 우선순위만 지정하면 되므로 확장성이 높다.

### 4. Controller - 이벤트 기반 리컨실레이션 루프

```go
func (c *Controller) Run(ctx context.Context) {
    for {
        select {
        case <-ctx.Done(): return
        case <-c.signalCh:
            c.manager.ReconcileInstances(ctx, c.nodeConfig)
        }
    }
}
```

**무엇을 하는 코드인가**: 설정 변경 시그널을 채널로 수신하여 리컨실레이션을 트리거하는 이벤트 루프이다.
버퍼 크기 1인 채널을 사용하여 중복 시그널을 자동으로 병합(coalescing)한다.

**실제 Cilium 참고**: `pkg/bgp/agent/controller.go`의 Controller를 모방했다. 실제 코드에서는
Kubernetes Informer의 이벤트와 Cilium 내부 이벤트 버스에서 시그널을 수신한다.

**왜 이렇게 구현했는가**: 폴링 방식 대신 이벤트 기반으로 동작하여 불필요한 리컨실레이션을 방지한다.
채널 버퍼가 1이므로 빠르게 연속된 변경이 발생해도 리컨실레이션은 한 번만 수행된다.

## 실행 방법 및 예상 출력

```bash
go run main.go
```

시뮬레이션은 4개의 시나리오를 순서대로 실행한다:

**시나리오 1 - 초기 BGP 인스턴스 생성**: ASN 65001 인스턴스를 만들고 ToR 스위치 2대와 피어링
```
▶ BGP 인스턴스 등록: instance-65001 (ASN=65001, RouterID=10.0.0.1)
  ├─ [PodCIDR] (우선순위=30) 실행
  [Router ASN=65001] 경로 광고: 10.244.1.0/24 via 10.0.0.1 (LP:0, AS-Path:[65001])
  [Router ASN=65001] 경로 광고: 10.244.2.0/24 via 10.0.0.1 (LP:0, AS-Path:[65001])
  ├─ [Service] (우선순위=40) 실행
  [Router ASN=65001] 경로 광고: 192.168.100.10/32 via 10.0.0.1 (LP:0, AS-Path:[65001])
  ├─ [Neighbor] (우선순위=60) 실행
  [Router ASN=65001] 피어 추가: 10.0.0.254 (ASN=65000) → Established
```

**시나리오 2 - 두 번째 인스턴스 추가**: ASN 65002 인스턴스를 추가하고 spine 스위치와 피어링.
기존 instance-65001은 변경 없이 유지됨 (리컨실러 실행되지만 변경사항 없음)

**시나리오 3 - 첫 번째 인스턴스 제거**: instance-65001을 withdraw하여 라우터를 중지하고,
instance-65002만 남김

**시나리오 4 - 전체 제거**: NodeConfig를 nil로 설정하여 모든 인스턴스 제거
```
▶ BGP 인스턴스 제거: instance-65002
  [Router ASN=65002] 라우터 중지
━━━ 현재 BGP 상태 ━━━
  (인스턴스 없음)
```

## 핵심 포인트

1. **선언적 리컨실레이션 패턴**: "원하는 상태"만 선언하면 시스템이 현재 상태와 비교하여 최소한의 변경만 수행한다.
   ReconcileDiff가 register/withdraw/reconcile을 분류하고, 각 리컨실러가 자신의 도메인(피어, 경로)에서
   동일한 패턴을 반복한다.

2. **우선순위 기반 리컨실러 파이프라인**: 리컨실러 간 암묵적 의존성을 Priority 값으로 해결한다.
   PodCIDR(30) -> Service(40) -> Neighbor(60) 순서로 실행되어, 경로가 먼저 준비된 상태에서
   피어를 연결한다. 새 리컨실러를 추가할 때 기존 코드 변경이 불필요하다.

3. **이벤트 기반 루프와 시그널 병합**: 버퍼 크기 1인 채널을 사용하여, 빠르게 연속된 설정 변경이
   발생해도 리컨실레이션은 한 번만 수행된다. Kubernetes Controller의 workqueue와 유사한 개념이다.

4. **멀티 인스턴스 관리**: 하나의 노드에서 여러 ASN의 BGP 인스턴스를 독립적으로 운영할 수 있다.
   각 인스턴스는 자체 라우터, 피어 테이블, 경로 테이블을 가진다.

5. **Desired-Current 비교 패턴**: 모든 리컨실러가 동일한 패턴을 사용한다. desired 맵을 구성하고,
   current에 없는 것은 추가하고, desired에 없는 것은 제거한다. 이 패턴은 멱등성을 자연스럽게 보장한다.

## 실제 Cilium과의 차이점

| 항목 | 이 시뮬레이션 | 실제 Cilium |
|------|-------------|-------------|
| BGP 구현 | 메모리 내 맵으로 시뮬레이션 | GoBGP 라이브러리 사용, 실제 TCP/179 BGP 세션 |
| 피어 상태 | 즉시 Established 처리 | FSM(Idle->Connect->Active->OpenSent->OpenConfirm->Established) 전이 |
| 설정 소스 | 함수 호출로 직접 전달 | CiliumBGPNodeConfig CRD를 Kubernetes Informer로 감시 |
| 리컨실러 수 | 3개 (PodCIDR, Service, Neighbor) | 7개 이상 (Preflight, Neighbor, PodCIDR, Service, RoutePolicy, LBService 등) |
| 경로 정책 | RoutePolicy 구조체만 정의 | GoBGP의 RoutingPolicy API를 통한 import/export 정책 적용 |
| 의존성 주입 | 생성자에서 직접 주입 | Hive Cell 프레임워크를 통한 의존성 주입 |
| 에러 처리 | 단순 에러 반환 | 재시도 로직, 백오프, 메트릭 리포팅 |
| 동시성 | 단일 고루틴 리컨실레이션 | 인스턴스별 병렬 리컨실레이션, 세밀한 락 관리 |
| 모니터링 | fmt.Printf 출력 | Prometheus 메트릭, 이벤트 로깅, Hubble 연동 |
