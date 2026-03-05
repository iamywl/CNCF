# 01. Cilium 아키텍처

## 개요

Cilium은 eBPF 기반의 Kubernetes 네트워킹, 보안, 옵저버빌리티 플랫폼이다.
4개의 핵심 바이너리(Agent, Operator, CNI Plugin, Hubble Relay)가 협력하여
클러스터 네트워킹을 구현한다. 모든 컴포넌트는 **Hive** 의존성 주입(DI) 프레임워크를
기반으로 하는 **셀(Cell) 아키텍처**로 구성된다.

## 전체 아키텍처 다이어그램

```
+------------------------------------------------------------------+
|                     Kubernetes Control Plane                      |
|              (API Server, etcd, Controller Manager)               |
+------------------------------------------------------------------+
         |                    |                      |
         | Watch/Update       | CRUD CRDs            | Leader Election
         |                    |                      |
+--------v--------+  +-------v---------+  +---------v---------+
|  cilium-agent   |  | cilium-operator |  | clustermesh-      |
|  (DaemonSet)    |  | (Deployment)    |  | apiserver         |
|                 |  |                 |  | (Deployment)      |
|  - ControlPlane |  | - Identity GC   |  |                   |
|  - Infrastructure| | - Endpoint GC   |  | - 멀티클러스터    |
|  - Datapath     |  | - IPAM          |  |   KVStore 동기화  |
|                 |  | - Gateway API   |  |                   |
+---------+-------+  | - CES Mgmt     |  +-------------------+
          |          +-----------------+
          |
  +-------v-------+     +-------------------+
  | cilium-cni    |     | hubble-relay      |
  | (Binary)      |     | (Deployment)      |
  |               |     |                   |
  | - Pod 생성 시  |     | - 다중 Agent에서  |
  |   Agent API   |     |   Flow 수집       |
  |   호출         |     | - gRPC 스트리밍   |
  +---------------+     +-------------------+
          |                      |
          v                      v
+---------------------------------------------------+
|              eBPF Datapath (Linux Kernel)          |
|                                                    |
|  bpf_lxc.c   bpf_host.c   bpf_overlay.c          |
|  bpf_xdp.c   bpf_sock.c   bpf_wireguard.c       |
|                                                    |
|  BPF Maps: ctmap, nat, policymap, lxcmap,         |
|            ipcache, lbmap, metricsmap ...          |
+---------------------------------------------------+
```

## 4대 핵심 컴포넌트

### 1. Cilium Agent (daemon)

**역할**: 각 노드에서 DaemonSet으로 실행. eBPF 프로그램 관리, 정책 적용, IPAM, 엔드포인트 관리를 담당.

**진입점** (`daemon/main.go`):
```go
func main() {
    hiveFn := func() *hive.Hive {
        return hive.New(cmd.Agent)
    }
    cmd.Execute(cmd.NewAgentCmd(hiveFn))
}
```

`hive.New(cmd.Agent)`에서 `cmd.Agent`는 `daemon/cmd/cells.go`에 정의된 최상위 셀 모듈이다:

```go
Agent = cell.Module(
    "agent",
    "Cilium Agent",

    Infrastructure,   // 외부 시스템 접근 (K8s, KVStore, API 서버)
    ControlPlane,     // 비즈니스 로직 (정책, 엔드포인트, IPAM)
    datapath.Cell,    // eBPF 데이터플레인 (맵, 프로그램 로드)
)
```

### 2. Cilium Operator

**역할**: 클러스터 수준의 싱글톤 작업 담당. 리더 선출을 통해 하나의 인스턴스만 활성.

**진입점** (`operator/main.go`):
```go
func main() {
    operatorHive := hive.New(cmd.Operator())
    cmd.Execute(cmd.NewOperatorCmd(operatorHive))
}
```

**Operator 셀 구조** (`operator/cmd/root.go`):
```go
func Operator() cell.Cell {
    Infrastructure = cell.Module("operator-infra", "Operator Infrastructure", InfrastructureCells...)
    ControlPlane = cell.Module("operator-controlplane", "Operator Control Plane",
        append(ControlPlaneCells, WithLeaderLifecycle(
            append(ControlPlaneLeaderCells, ipam.Cell())...,
        ))...,
    )
    return cell.Module("operator", "Cilium Operator", Infrastructure, ControlPlane, ...)
}
```

**주요 책임**:
- Identity GC: 사용되지 않는 보안 Identity 정리
- Endpoint GC: 고아(orphan) CiliumEndpoint 정리
- IPAM: 클라우드 IP 할당 관리 (AWS ENI, Azure)
- Gateway API/Ingress: L7 라우팅 리소스 처리
- CiliumEndpointSlice: CES 리소스 관리
- LB-IPAM: LoadBalancer 서비스에 IP 할당
- BGP: BGP 컨트롤 플레인

### 3. CNI Plugin

**역할**: kubelet이 Pod 생성/삭제 시 호출하는 CNI 바이너리. cilium-agent API를 통해 엔드포인트 생성.

**진입점** (`plugins/cilium-cni/main.go`):
```go
func main() {
    cmd.PluginMain()
}
```

**CNI 커맨드 등록** (`plugins/cilium-cni/cmd/cmd.go`):
```go
func PluginMain(opts ...Option) {
    cmd := &Cmd{logger: logger, version: "Cilium CNI plugin " + version.Version, ...}
    skel.PluginMainFuncs(
        skel.CNIFuncs{
            Add:    cmd.Add,     // Pod 생성 시
            Del:    cmd.Del,     // Pod 삭제 시
            Check:  cmd.Check,   // 상태 확인
            Status: cmd.Status,  // CNI STATUS 지원
        },
        cniVersion.PluginSupports("0.1.0", "0.2.0", "0.3.0", "0.3.1", "0.4.0", "1.0.0", "1.1.0"),
        cmd.version,
    )
}
```

### 4. Hubble Relay

**역할**: 여러 노드의 cilium-agent에서 Hubble 이벤트를 수집하여 단일 gRPC 스트림으로 제공.

**진입점** (`hubble-relay/main.go`):
```go
func main() {
    if err := cmd.New().Execute(); err != nil {
        os.Exit(1)
    }
}
```

## Hive 셀 아키텍처

Cilium의 모든 컴포넌트는 **Hive** DI 프레임워크로 구성된다. Hive는 셀(Cell) 단위로
기능을 모듈화하고, 의존성 자동 주입과 라이프사이클 관리를 제공한다.

### 왜 Hive를 사용하는가?

1. **테스트 용이성**: ControlPlane 셀을 Infrastructure 없이 단독 테스트 가능
2. **모듈화**: 기능 단위 분리로 코드 이해도 향상
3. **라이프사이클 관리**: Start/Stop 훅을 통한 순서 보장
4. **설정 검증**: 컴파일 타임에 의존성 누락 감지

### Agent의 3대 셀 모듈

```
Agent (cell.Module "agent")
├── Infrastructure (cell.Module "infra")
│   ├── pprof.Cell                    # 런타임 프로파일링
│   ├── gops.Cell                     # Go 프로세스 진단
│   ├── k8sClient.Cell                # Kubernetes 클라이언트
│   ├── kvstore.Cell                  # etcd 등 KVStore 클라이언트
│   ├── metrics.AgentCell             # Prometheus 메트릭 서버
│   ├── metricsmap.Cell               # BPF 메트릭 맵
│   ├── server.Cell                   # UNIX 소켓 REST API 서버
│   ├── store.Cell                    # KVStore 동기화
│   ├── k8sSynced.CRDSyncCell         # CRD 동기화 대기
│   └── healthz.Cell                  # 헬스 체크 엔드포인트
│
├── ControlPlane (cell.Module "controlplane")
│   ├── endpoint.Cell                 # 엔드포인트 관리
│   ├── nodeManager.Cell              # 노드 매니저
│   ├── identity.Cell                 # 보안 Identity 관리
│   ├── ipcache.Cell                  # IP-Identity 매핑
│   ├── ipamcell.Cell                 # IP 주소 관리
│   ├── policy.Cell                   # PolicyRepository
│   ├── policyK8s.Cell                # K8s 정책 워처
│   ├── clustermesh.Cell              # 멀티클러스터
│   ├── hubble.Cell                   # Hubble 서버/메트릭
│   ├── auth.Cell                     # 인증
│   ├── loadbalancer_cell.Cell        # 서비스 로드밸런싱
│   ├── proxy.Cell                    # L7 프록시 (Envoy)
│   ├── envoy.Cell                    # Envoy 컨트롤 플레인
│   ├── bgp.Cell                      # BGP 컨트롤 플레인
│   ├── signal.Cell                   # BPF 시그널 브로커
│   ├── egressgateway.Cell            # Egress Gateway
│   ├── fqdn.Cell                     # FQDN 프록시
│   ├── health.Cell                   # cilium-health
│   └── watchers.Cell                 # K8s 리소스 워처
│
└── datapath.Cell
    ├── loader/                       # BPF 프로그램 컴파일/로드
    ├── maps/                         # BPF 맵 관리 (34종+)
    └── linux/                        # Linux 네트워크 설정
```

### Infrastructure vs ControlPlane 분리의 의미

`daemon/cmd/cells.go`의 주석에서 설계 의도를 명확히 밝히고 있다:

```go
// Infrastructure provides access and services to the outside.
// A cell should live here instead of ControlPlane if it is not needed by
// integration tests, or needs to be mocked.

// ControlPlane implement the per-node control functions. These are pure
// business logic and depend on datapath or infrastructure to perform
// actions. This separation enables non-privileged integration testing of
// the control-plane.
```

**Infrastructure**는 외부 시스템(K8s API, KVStore, UNIX 소켓 서버 등)과의 연결을 담당하며,
테스트에서는 모킹 대상이다. **ControlPlane**은 순수 비즈니스 로직이므로 비특권
통합 테스트가 가능하다.

## 초기화 흐름

Agent의 전체 시작 순서는 다음과 같다:

```
1. main() → hive.New(cmd.Agent)
   └── cmd.Agent = cell.Module("agent", Infrastructure, ControlPlane, datapath.Cell)

2. cmd.Execute(cmd.NewAgentCmd(hiveFn))
   └── cobra.Command 생성 + viper 설정 바인딩
   └── option.InitConfig() → 설정 파일/환경변수/플래그 로드

3. hive.Run()
   └── 모든 Cell의 Provide/Invoke 해석
   └── 의존성 그래프 구성
   └── Lifecycle.Start() 호출 (의존 순서대로)
       ├── Infrastructure 셀 시작
       │   ├── K8s 클라이언트 연결
       │   ├── KVStore 연결
       │   └── API 서버 시작
       ├── ControlPlane 셀 시작
       │   ├── daemonCell: initAndValidateDaemonConfig()
       │   ├── Endpoint 복원
       │   ├── NodeManager 시작
       │   ├── PolicyRepository 초기화
       │   ├── K8s 워처 시작
       │   └── Hubble 서버 시작
       └── Datapath 셀 시작
           ├── BPF 맵 초기화
           └── 호스트 BPF 프로그램 로드
```

## API 서버 구성

Agent는 UNIX 소켓을 통해 REST API를 제공한다. `configureAPIServer()` 함수에서
필수 API가 비활성화되지 않았는지 검증한다:

```go
// daemon/cmd/cells.go
for _, requiredAPI := range []string{
    "GetConfig",        // CNI: IPAM 모드 감지
    "GetHealthz",       // Kubelet: 데몬 헬스 체크
    "PutEndpointID",    // CNI: 새 Pod 네트워크 프로비저닝
    "DeleteEndpointID", // CNI: 삭제된 Pod 네트워크 정리
    "PostIPAM",         // CNI: 새 Pod IP 할당
    "DeleteIPAMIP",     // CNI: 삭제된 Pod IP 해제
} { ... }
```

이 6개 API는 CNI 플러그인과 kubelet이 정상 동작하기 위한 필수 엔드포인트이다.

## 컴포넌트 간 통신

```
+---------------------+          +---------------------+
|    cilium-agent     |  gRPC    |    hubble-relay     |
|    (각 노드)        |--------->|    (Deployment)     |
|                     |  Flow    |                     |
| UNIX Socket API     |  Events  | gRPC Server         |
|  /var/run/cilium/   |          |  :4245              |
|  cilium.sock        |          +---------------------+
+----------+----------+                    |
           ^                               v
           | UNIX Socket                hubble CLI / UI
           |
+----------+----------+
|    cilium-cni       |
|    (Pod 생성 시)     |
|                     |
|  PutEndpointID      |
|  PostIPAM           |
|  DeleteEndpointID   |
|  DeleteIPAMIP       |
+---------------------+

+---------------------+          +---------------------+
|    cilium-agent     |  Watch   |   Kubernetes        |
|    (DaemonSet)      |<-------->|   API Server        |
|                     |  CRDs    |                     |
+---------------------+          +----------+----------+
                                            ^
+---------------------+                     |
|  cilium-operator    |  CRUD CRDs          |
|  (Deployment)       |-------------------->|
|                     |  Leader Election     |
+---------------------+
```

## 왜 이 아키텍처인가?

### 1. DaemonSet Agent + Singleton Operator 패턴
- **Agent(DaemonSet)**: 노드-로컬 작업(BPF 프로그램 관리, 엔드포인트 관리)은 각 노드에서 독립적으로 수행
- **Operator(Deployment)**: 클러스터 전체 작업(GC, IPAM, CRD 관리)은 단일 인스턴스로 충분
- 이 분리로 각 컴포넌트의 스케일링 특성이 다름을 반영

### 2. eBPF 데이터플레인
- iptables 체인 탐색 대신 BPF 맵의 O(1) 룩업으로 고성능 달성
- 커널 내 처리로 사용자 공간-커널 공간 전환 최소화
- 프로그램 동적 교체로 서비스 중단 없는 업데이트

### 3. Identity 기반 보안 모델
- Pod IP가 아닌 보안 Identity(라벨 기반)로 정책 적용
- IP 변경에 무관한 정책 유지
- BPF 맵에서 Identity 기반 O(1) 정책 조회

### 4. UNIX 소켓 API
- Agent와 CNI 플러그인 간 로컬 통신에 네트워크 스택 불필요
- 파일 시스템 권한으로 접근 제어
- 네트워크가 아직 구성되지 않은 Pod 생성 시점에서도 통신 가능
