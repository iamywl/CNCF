# 1. Cilium 시스템 아키텍처

---

## 전체 구조

```
┌──────────────────────────────────────────────────────────────────────┐
│                      Kubernetes Control Plane                        │
│                  (API Server, etcd, Scheduler)                       │
└─────────┬───────────────────────────────────┬────────────────────────┘
          │ K8s API (Watch/List)              │ K8s API (Watch/List)
          ▼                                   ▼
┌─────────────────┐                 ┌─────────────────────────────────┐
│  cilium-operator │                │  cilium-daemon (각 노드마다 1개)   │
│  (클러스터당 1개)  │                │                                   │
│                  │                │  ┌───────────┐  ┌─────────────┐ │
│ - Identity GC    │                │  │  Policy    │  │  Endpoint   │ │
│ - IPAM 할당       │◄──── etcd ────▶│  │  Engine    │  │  Manager    │ │
│ - CRD 관리        │                │  └───────────┘  └─────────────┘ │
│ - 인그레스 관리    │                │  ┌───────────┐  ┌─────────────┐ │
└─────────────────┘                │  │  IPCache   │  │  KVStore    │ │
                                   │  │            │  │  Client     │ │
                                   │  └───────────┘  └─────────────┘ │
                                   │  ┌───────────┐  ┌─────────────┐ │
                                   │  │  LoadBal.  │  │  Hubble     │ │
                                   │  │  (kube-    │  │  Observer   │ │
                                   │  │   proxy    │  │             │ │
                                   │  │   대체)     │  │             │ │
                                   │  └───────────┘  └──────┬──────┘ │
                                   │          │              │        │
                                   │  ┌───────▼──────────────▼──────┐ │
                                   │  │     BPF Datapath (커널)      │ │
                                   │  │  bpf_lxc · bpf_host ·       │ │
                                   │  │  bpf_sock · bpf_overlay ·   │ │
                                   │  │  bpf_xdp · bpf_wireguard   │ │
                                   │  └─────────────────────────────┘ │
                                   └──────────────────┬───────────────┘
                                                      │ gRPC
                                                      ▼
                                            ┌──────────────────┐
                                            │   hubble-relay    │
                                            │  (클러스터당 1개)   │
                                            └────────┬─────────┘
                                                     │ gRPC
                                                     ▼
                                            ┌──────────────────┐
                                            │   hubble CLI      │
                                            │  (사용자 도구)      │
                                            └──────────────────┘
```

---

## 주요 컴포넌트 설명

### cilium-daemon (에이전트)

각 Kubernetes 노드에서 실행되는 핵심 에이전트.

| 항목 | 설명 |
|------|------|
| 엔트리포인트 | `daemon/main.go` (main 함수) → `daemon/cmd/daemon_main.go` (초기화 로직) |
| DI 구성 | `daemon/cmd/cells.go` (Hive 모듈 정의) |
| 역할 | Endpoint 관리, 정책 컴파일/적용, BPF 프로그램 로딩, IP 할당 |
| 통신 | REST API (UNIX 소켓), gRPC (Hubble) |
| 상태 저장 | etcd (분산), StateDB (로컬 인메모리) |

**Why Hive?** — Cilium의 컴포넌트가 수십 개에 달하므로, 의존성 주입 프레임워크(Hive)로
컴포넌트 생명주기(Start/Stop)와 의존 관계를 선언적으로 관리한다.

### cilium-operator (오퍼레이터)

클러스터 전체를 관리하는 싱글톤 컴포넌트.

| 항목 | 설명 |
|------|------|
| 엔트리포인트 | `operator/cmd/root.go` |
| 역할 | Identity GC, IPAM 할당, CiliumNetworkPolicy 관리, 인그레스 컨트롤러 |
| 통신 | K8s API (Watch), etcd |

**Why 별도 프로세스?** — Identity GC, IPAM 할당 같은 작업은 클러스터 전체 상태를 봐야 하므로,
노드별 에이전트가 아닌 중앙 오퍼레이터에서 처리해야 충돌을 피할 수 있다.

### hubble (관측 시스템)

네트워크 흐름(Flow)을 수집·분석하는 관측 플랫폼.

```
Pod A ──패킷──▶ BPF Datapath ──이벤트──▶ Hubble Observer ──gRPC──▶ Hubble Relay ──gRPC──▶ Hubble CLI
                (커널 내)                 (daemon 내장)            (중앙 집계)              (사용자)
```

| 컴포넌트 | 위치 | 역할 |
|----------|------|------|
| Hubble Observer | daemon 내 | BPF 이벤트를 Flow로 변환, 링 버퍼에 저장 |
| hubble-relay | 별도 Pod | 여러 노드의 Flow를 집계, gRPC 스트리밍 |
| hubble CLI | 사용자 머신 | Flow 조회, 필터링 (`hubble observe`) |

### clustermesh-apiserver (멀티클러스터)

```
┌──────────────┐          ┌──────────────┐
│  Cluster A   │          │  Cluster B   │
│  cilium-     │◄──etcd──▶│  cilium-     │
│  daemon      │          │  daemon      │
└──────┬───────┘          └──────┬───────┘
       │                         │
       └────▶ clustermesh ◄──────┘
              apiserver
              (etcd 동기화)
```

서로 다른 Kubernetes 클러스터의 Endpoint, Service, Identity 정보를 etcd를 통해 동기화한다.

---

## 컴포넌트 간 통신 프로토콜

| 출발 | 도착 | 프로토콜 | 용도 |
|------|------|----------|------|
| daemon | K8s API | HTTPS (Watch/List) | Pod, Service, Policy 감시 |
| daemon | etcd | gRPC | 분산 상태 (Identity, Endpoint) |
| daemon | BPF | syscall (bpf()) | 프로그램 로딩, 맵 읽기/쓰기 |
| operator | K8s API | HTTPS (Watch/List) | CRD 관리, IPAM |
| operator | etcd | gRPC | Identity GC, 할당 |
| hubble observer | hubble-relay | gRPC | Flow 스트리밍 |
| hubble-relay | hubble CLI | gRPC | Flow 조회 |
| daemon | Envoy | gRPC (xDS) | L7 프록시 설정 |
| cilium-cli | daemon | REST (UNIX socket) | 상태 조회, 디버깅 |

---

## 배포 토폴로지

```
Kubernetes Cluster
├── kube-system namespace
│   ├── cilium (DaemonSet)           ← 모든 노드에 1개씩
│   ├── cilium-operator (Deployment) ← 클러스터에 1~2개 (HA)
│   ├── hubble-relay (Deployment)    ← 클러스터에 1개
│   └── hubble-ui (Deployment)       ← 웹 UI (선택)
└── 사용자 namespace
    └── Pod → cilium이 관리하는 Endpoint
```
