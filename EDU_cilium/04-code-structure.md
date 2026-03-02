# 4. Cilium 코드 구조

---

## 최상위 디렉토리

```
cilium/
├── api/                 # API 정의 (OpenAPI, protobuf)
│   └── v1/
│       ├── flow/        #   Flow 메시지 정의 (.proto)
│       ├── observer/    #   Hubble Observer gRPC 서비스
│       ├── peer/        #   Peer 통신 서비스
│       ├── relay/       #   Relay 통신 서비스
│       └── server/      #   REST API (go-swagger 자동 생성)
│
├── bpf/                 # BPF/eBPF C 소스코드 (데이터패스)
│   ├── bpf_lxc.c        #   컨테이너 TC 프로그램
│   ├── bpf_host.c       #   호스트 TC 프로그램
│   ├── bpf_sock.c       #   소켓 레벨 LB 프로그램
│   ├── bpf_overlay.c    #   VXLAN/Geneve 오버레이
│   ├── bpf_xdp.c        #   XDP 프로그램
│   ├── bpf_wireguard.c  #   WireGuard 암호화
│   ├── bpf_network.c    #   네트워크 디바이스
│   ├── lib/             #   BPF 라이브러리 헤더 (.h)
│   └── tests/           #   BPF 유닛 테스트
│
├── daemon/              # cilium-daemon (노드 에이전트)
│   ├── main.go             # main() 함수 (실제 엔트리포인트)
│   ├── cmd/
│   │   ├── daemon_main.go  # 초기화 로직
│   │   └── cells.go        # Hive 모듈 구성
│   └── restapi/            # REST API 핸들러
│
├── operator/            # cilium-operator (클러스터 오퍼레이터)
│   ├── cmd/
│   │   └── root.go         # 메인 엔트리포인트
│   └── watchers/           # K8s 리소스 감시자
│
├── hubble/              # hubble CLI 도구
│   └── cmd/
│       └── observe/
│           └── observe.go  # hubble observe 명령
│
├── hubble-relay/        # hubble-relay 서버
│   └── cmd/
│
├── cilium-cli/          # cilium CLI 도구
│   └── cmd/
│
├── cilium-dbg/          # cilium 디버그 CLI
│
├── cilium-health/       # 노드 간 헬스 체크
│
├── clustermesh-apiserver/  # 멀티클러스터 API 서버
│
├── pkg/                 # 공유 패키지 (핵심 로직)
│   ├── ... (아래 상세)
│
├── install/             # Helm 차트, 매니페스트
│   └── kubernetes/
│       └── cilium/         # Helm 차트
│
├── test/                # 통합/E2E 테스트
├── examples/            # 사용 예제
├── vendor/              # Go 의존성 벤더링
└── tools/               # 빌드/개발 도구
```

---

## pkg/ 패키지 구조

### 제어 평면 (Control Plane)

| 패키지 | 역할 | 핵심 파일 |
|--------|------|-----------|
| `pkg/endpoint/` | Endpoint 생명주기 관리 | `endpoint.go`, `bpf.go` |
| `pkg/endpointmanager/` | Endpoint 동기화 및 조회 | `manager.go` |
| `pkg/policy/` | 정책 파싱, 컴파일, 저장소 | `repository.go`, `rule.go` |
| `pkg/identity/` | Security Identity 할당/관리 | `identity.go`, `numericidentity.go`, `cache/` |
| `pkg/ipcache/` | IP → Identity 매핑 캐시 | `ipcache.go` |
| `pkg/loadbalancer/` | 서비스 로드밸런싱 | `frontend.go`, `config.go`, `maps/lbmaps.go` |
| `pkg/kvstore/` | etcd 통합, 분산 상태 | `etcd.go`, `store/` |
| `pkg/k8s/` | Kubernetes API 연동 | `client/`, `watchers/` |
| `pkg/node/` | 노드 표현 및 관리 | `node.go`, `manager.go` |
| `pkg/clustermesh/` | 멀티클러스터 연결 | `remote_cluster.go` |

### 데이터 평면 (Data Plane)

| 패키지 | 역할 | 핵심 파일 |
|--------|------|-----------|
| `pkg/datapath/` | 데이터패스 추상화 레이어 | `types/config.go` |
| `pkg/datapath/linux/` | Linux 구현 | `config/` |
| `pkg/datapath/loader/` | BPF 프로그램 컴파일/로딩 | `cache.go`, `loader.go` |
| `pkg/maps/` | BPF 맵 정의 | 하위 패키지들 |
| `pkg/bpf/` | BPF 유틸리티 | `bpf.go` |

### 관측 (Observability)

| 패키지 | 역할 | 핵심 파일 |
|--------|------|-----------|
| `pkg/hubble/` | Flow 수집/처리 | `parser/`, `recorder/` |
| `pkg/monitor/` | 패킷 모니터링 | `datapath_drop.go` |

### 인프라 / 프레임워크

| 패키지 | 역할 | 핵심 파일 |
|--------|------|-----------|
| `pkg/hive/` | 의존성 주입 래퍼 (코어: `github.com/cilium/hive`) | 로컬 래퍼 + 외부 의존성 |
| `pkg/option/` | 전역 설정 관리 | `config.go` (~400개 옵션) |
| `pkg/proxy/` | L7 프록시 추상화 | `proxy.go` |
| `pkg/envoy/` | Envoy 통합 | `server.go` |
| `pkg/auth/` | 인증 (SPIFFE/mTLS) | `auth.go` |
| `pkg/bgp/` | BGP 제어 평면 | `speaker/` |
| `pkg/health/` | 헬스 체크 | `server/`, `client/` |

---

## API 명세

### REST API (cilium-daemon)

`api/v1/openapi.yaml` 에서 자동 생성 (go-swagger).

| 엔드포인트 | 메서드 | 설명 |
|------------|--------|------|
| `/endpoint` | GET | Endpoint 목록 조회 |
| `/endpoint/{id}` | GET/PUT/DELETE | Endpoint CRUD |
| `/identity` | GET | Identity 목록 조회 |
| `/policy` | GET/PUT/DELETE | 정책 관리 |
| `/service` | GET | 서비스 목록 |
| `/ipam` | POST | IP 할당 |
| `/healthz` | GET | 헬스 체크 |
| `/config` | GET/PATCH | 런타임 설정 |
| `/debuginfo` | GET | 디버그 정보 |
| `/map` | GET | BPF 맵 조회 |

접근 방식: UNIX 소켓 (`/var/run/cilium/cilium.sock`)

### gRPC API (Hubble)

`api/v1/observer/observer.proto` 정의.

```protobuf
service Observer {
    rpc GetFlows(GetFlowsRequest)
        returns (stream GetFlowsResponse);

    rpc GetAgentEvents(GetAgentEventsRequest)
        returns (stream GetAgentEventsResponse);

    rpc GetDebugEvents(GetDebugEventsRequest)
        returns (stream GetDebugEventsResponse);

    rpc GetNodes(GetNodesRequest)
        returns (GetNodesResponse);

    rpc GetNamespaces(GetNamespacesRequest)
        returns (GetNamespacesResponse);

    rpc ServerStatus(ServerStatusRequest)
        returns (ServerStatusResponse);
}
```

### gRPC API (Peer)

`api/v1/peer/peer.proto` — 노드 간 피어 정보 교환.

```protobuf
service Peer {
    rpc Notify(NotifyRequest) returns (stream ChangeNotification);
}
```

---

## Hive 의존성 주입 구조

Cilium daemon의 모듈 구성 (`daemon/cmd/cells.go`):

```
Agent (루트)
│
├── Infrastructure
│   ├── Kubernetes Client
│   ├── KVStore (etcd)
│   ├── Metrics Server
│   ├── gRPC Server
│   ├── Health Probes
│   └── Shell Socket
│
├── ControlPlane
│   ├── Endpoint Manager
│   ├── Policy Engine
│   ├── Identity Allocator
│   ├── Load Balancer
│   ├── Proxy (Envoy)
│   ├── Health Connectivity
│   ├── BGP Agent
│   ├── Hubble Observer
│   └── ClusterMesh
│
└── Datapath
    ├── BPF Loader
    ├── Device Manager
    └── Network Config
```

각 Cell은:
- **Constructor** — 의존성을 받아 컴포넌트 생성
- **Lifecycle Hook** — Start/Stop 함수
- **Health Check** — 상태 보고
- **Metrics** — Prometheus 메트릭 제공
