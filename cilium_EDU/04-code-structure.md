# 04. Cilium 코드 구조

## 개요

Cilium은 Go와 C로 작성된 대규모 프로젝트이다. Go는 컨트롤플레인(에이전트, 오퍼레이터, CLI)을,
C는 eBPF 데이터플레인 프로그램을 담당한다. 이 문서에서는 디렉토리 구조, 빌드 시스템,
핵심 패키지를 분석한다.

## 최상위 디렉토리 구조

```
cilium/
├── api/                          # API 정의 (Swagger, go-swagger 생성 코드)
│   └── v1/
│       ├── models/               # 자동 생성 API 모델
│       ├── server/               # API 서버 코드 (go-swagger)
│       ├── health/               # cilium-health API
│       └── operator/             # Operator API
│
├── bpf/                          # eBPF C 프로그램 (데이터플레인)
│   ├── bpf_lxc.c                 # 엔드포인트 프로그램 (Pod veth)
│   ├── bpf_host.c                # 호스트 프로그램
│   ├── bpf_overlay.c             # 오버레이 터널 프로그램
│   ├── bpf_xdp.c                 # XDP 프로그램
│   ├── bpf_sock.c                # 소켓 프로그램
│   ├── bpf_wireguard.c           # WireGuard 프로그램
│   ├── bpf_sock_term.c           # 소켓 종료 프로그램
│   ├── bpf_alignchecker.c        # 구조체 정렬 검사
│   ├── bpf_probes.c              # 커널 기능 프로브
│   ├── lib/                      # BPF 라이브러리 헤더 (84개)
│   ├── tests/                    # BPF 단위 테스트
│   └── Makefile                  # BPF 빌드
│
├── daemon/                       # cilium-agent 바이너리
│   ├── main.go                   # 진입점
│   ├── cmd/
│   │   ├── cells.go              # Agent/Infrastructure/ControlPlane 셀 정의
│   │   ├── daemon.go             # initAndValidateDaemonConfig()
│   │   └── ...
│   ├── restapi/                  # REST API 핸들러 라이프사이클
│   ├── k8s/                      # Agent K8s 리소스
│   └── healthz/                  # 헬스체크 엔드포인트
│
├── operator/                     # cilium-operator 바이너리
│   ├── main.go                   # 진입점
│   ├── cmd/
│   │   └── root.go               # Operator() 셀, 리더 선출
│   ├── pkg/                      # Operator 전용 패키지
│   │   ├── bgp/                  # BGP
│   │   ├── gateway-api/          # Gateway API 컨트롤러
│   │   ├── ingress/              # Ingress 컨트롤러
│   │   ├── ipam/                 # IPAM (클라우드 IP 관리)
│   │   ├── lbipam/               # LB-IPAM
│   │   └── ...
│   ├── identitygc/               # Identity GC
│   ├── endpointgc/               # Endpoint GC
│   └── watchers/                 # K8s 리소스 워처
│
├── plugins/                      # 플러그인
│   └── cilium-cni/               # CNI 플러그인
│       ├── main.go               # 진입점
│       ├── cmd/
│       │   └── cmd.go            # PluginMain(), Add(), Del()
│       ├── chaining/             # CNI 체이닝 모드 (AWS, Azure, Flannel)
│       └── lib/                  # CNI 유틸리티
│
├── hubble-relay/                 # Hubble Relay 바이너리
│   ├── main.go                   # 진입점
│   └── cmd/                      # Relay 커맨드
│
├── hubble/                       # Hubble CLI (내장)
│
├── cilium-dbg/                   # cilium-dbg CLI (디버그 도구, 151개 커맨드)
│   └── cmd/                      # 디버그 커맨드 구현
│
├── cilium-cli/                   # cilium CLI (클러스터 관리)
│
├── cilium-health/                # cilium-health 바이너리
│   └── cmd/                      # 노드 간 연결성 테스트
│
├── bugtool/                      # 버그 리포트 수집 도구
│
├── clustermesh-apiserver/        # ClusterMesh API 서버
│
├── standalone-dns-proxy/         # 독립 DNS 프록시
│
├── pkg/                          # 핵심 Go 패키지 (공유 라이브러리)
│
├── install/                      # Helm 차트
│   └── kubernetes/
│       └── cilium/               # Cilium Helm 차트
│
├── Documentation/                # 문서 (RST)
├── examples/                     # 예제 설정
├── test/                         # E2E 테스트
├── tools/                        # 유틸리티 도구
├── contrib/                      # 기여 관련 스크립트
├── hack/                         # 개발 스크립트
├── images/                       # 컨테이너 이미지 빌드
├── vendor/                       # Go 벤더 디렉토리
│
├── go.mod                        # Go 모듈 (github.com/cilium/cilium, Go 1.25.0)
├── Makefile                      # 최상위 빌드
└── Makefile.defs                 # 빌드 변수 정의
```

## pkg/ 핵심 패키지

pkg/는 모든 바이너리가 공유하는 핵심 로직 라이브러리이다.

### 네트워킹/데이터플레인

| 패키지 | 설명 |
|--------|------|
| `pkg/datapath/` | 데이터플레인 추상화 (loader, config, maps) |
| `pkg/datapath/loader/` | BPF 프로그램 컴파일/로드 (compile.go, tc.go, netkit.go) |
| `pkg/datapath/linux/` | Linux 네트워크 설정 (ipsec, route, sysctl, bandwidth) |
| `pkg/datapath/tables/` | StateDB 테이블 (Device, Route, NodeAddress) |
| `pkg/datapath/types/` | 데이터플레인 인터페이스 정의 |
| `pkg/proxy/` | L7 프록시 (Envoy, DNS) 관리 |
| `pkg/envoy/` | Envoy 프록시 컨트롤 플레인 |

### 정책/보안

| 패키지 | 설명 |
|--------|------|
| `pkg/policy/` | PolicyRepository, SelectorCache, Distillery |
| `pkg/policy/api/` | 정책 API 타입 정의 |
| `pkg/policy/k8s/` | K8s CiliumNetworkPolicy 워처 |
| `pkg/policy/types/` | PolicyEntry, PolicyMetrics |
| `pkg/identity/` | 보안 Identity 관리 |
| `pkg/auth/` | Mutual Authentication |

### 엔드포인트/노드

| 패키지 | 설명 |
|--------|------|
| `pkg/endpoint/` | Endpoint 구조체 및 관리 |
| `pkg/endpointmanager/` | EndpointManager (CRUD, GC) |
| `pkg/node/` | LocalNodeStore, NodeManager |
| `pkg/node/types/` | Node 구조체 |
| `pkg/nodediscovery/` | 노드 디스커버리 |

### 로드밸런싱

| 패키지 | 설명 |
|--------|------|
| `pkg/loadbalancer/` | Service, Frontend, Backend 데이터 모델 |
| `pkg/loadbalancer/cell/` | LB 컨트롤 플레인 셀 |
| `pkg/maglev/` | Maglev 해시 테이블 |

### IPAM

| 패키지 | 설명 |
|--------|------|
| `pkg/ipam/` | IP Address Management |
| `pkg/ipam/cell/` | IPAM 셀 |
| `pkg/ipam/option/` | IPAM 모드 (cluster-pool, eni, azure, multi-pool) |
| `pkg/ipcache/` | IP-Identity 매핑 캐시 |

### BPF 맵 (34종)

| 패키지 | 설명 |
|--------|------|
| `pkg/maps/ctmap/` | ConnTrack 맵 |
| `pkg/maps/nat/` | NAT 맵 |
| `pkg/maps/policymap/` | 정책 맵 |
| `pkg/maps/lxcmap/` | 엔드포인트 맵 |
| `pkg/maps/ipcache/` | IPCache 맵 |
| `pkg/maps/metricsmap/` | 데이터플레인 메트릭 맵 |
| `pkg/maps/callsmap/` | Tail Call 맵 |
| `pkg/maps/signalmap/` | 시그널 맵 |
| `pkg/maps/configmap/` | 설정 맵 |
| `pkg/maps/bwmap/` | 대역폭 관리 맵 |
| `pkg/maps/egressmap/` | Egress 맵 |
| `pkg/maps/fragmap/` | IP 단편화 맵 |
| `pkg/maps/nodemap/` | 노드 맵 |
| `pkg/maps/encrypt/` | 암호화 맵 |
| `pkg/maps/srv6map/` | SRv6 맵 |
| `pkg/maps/vtep/` | VTEP 맵 |
| `pkg/maps/authmap/` | 인증 맵 |
| 기타 | act, cidrmap, eventsmap, l2respondermap, multicast, neighborsmap, netdev, ratelimitmap, subnet, timestamp 등 |

### K8s 통합

| 패키지 | 설명 |
|--------|------|
| `pkg/k8s/` | K8s 클라이언트, 리소스 |
| `pkg/k8s/client/` | Clientset 셀 |
| `pkg/k8s/apis/` | CRD API 정의 (cilium.io/v2, v2alpha1) |
| `pkg/k8s/watchers/` | K8s 리소스 워처 |
| `pkg/k8s/synced/` | K8s 캐시 동기화 |

### 클러스터/인프라

| 패키지 | 설명 |
|--------|------|
| `pkg/clustermesh/` | 멀티클러스터 (ClusterMesh) |
| `pkg/kvstore/` | KVStore 추상화 (etcd) |
| `pkg/hubble/` | Hubble 서버/메트릭 |
| `pkg/metrics/` | Prometheus 메트릭 |
| `pkg/option/` | DaemonConfig, 설정 관리 |
| `pkg/hive/` | Hive DI 프레임워크 래퍼 |
| `pkg/bgp/` | BGP 컨트롤 플레인 |
| `pkg/egressgateway/` | Egress Gateway |

## bpf/ 디렉토리 구조

### BPF 프로그램 (.c 파일, 9개)

| 파일 | 용도 | 어태치 포인트 |
|------|------|--------------|
| `bpf_lxc.c` | 엔드포인트(Pod) 트래픽 | TC ingress/egress on veth |
| `bpf_host.c` | 호스트 트래픽 | TC on host device |
| `bpf_overlay.c` | 오버레이 터널 | TC on tunnel device |
| `bpf_xdp.c` | XDP 프리필터 | XDP on physical NIC |
| `bpf_sock.c` | 소켓 레벨 LB | cgroup/connect4,6 등 |
| `bpf_wireguard.c` | WireGuard 트래픽 | TC on wg device |
| `bpf_sock_term.c` | 소켓 종료 | 소켓 ops |
| `bpf_alignchecker.c` | 구조체 정렬 검증 | 빌드 시 검증용 |
| `bpf_probes.c` | 커널 기능 프로빙 | 기능 감지용 |

### BPF 라이브러리 헤더 (lib/, 84개)

주요 헤더 파일:

| 파일 | 역할 |
|------|------|
| `lib/common.h` | 공통 정의, 매크로 |
| `lib/conntrack.h` | ConnTrack 조회/생성 |
| `lib/policy.h` | 정책 확인 (policymap 조회) |
| `lib/lb.h` | 로드밸런싱 (서비스 조회, 백엔드 선택) |
| `lib/nat.h` | NAT 처리 (SNAT/DNAT) |
| `lib/fib.h` | FIB 룩업 (다음 홉 결정) |
| `lib/encap.h` | 터널 캡슐화 (VXLAN/Geneve) |
| `lib/tailcall.h` | Tail Call 번호 정의 |
| `lib/ipv4.h` / `lib/ipv6.h` | IPv4/IPv6 처리 |
| `lib/nodeport.h` | NodePort 처리 |
| `lib/identity.h` | Identity 해석 |
| `lib/drop.h` | 드롭 사유 코드 |
| `lib/trace.h` | 트레이싱 |
| `lib/eps.h` | 엔드포인트 조회 |
| `lib/local_delivery.h` | 로컬 배달 |
| `lib/auth.h` | 인증 |

### BPF 설정 헤더

| 파일 | 역할 |
|------|------|
| `ep_config.h` | 엔드포인트별 설정 (컴파일 시 주입) |
| `node_config.h` | 노드 전체 설정 |
| `filter_config.h` | 프리필터 설정 |
| `netdev_config.h` | 네트워크 디바이스 설정 |

## 빌드 시스템

### Go 빌드

```makefile
# Makefile
all: precheck build postcheck

# 빌드 대상 서브디렉토리
SUBDIRS_CILIUM_CONTAINER := cilium-dbg daemon cilium-health bugtool \
    hubble tools/mount tools/sysctlfix plugins/cilium-cni
SUBDIR_OPERATOR_CONTAINER := operator
SUBDIR_RELAY_CONTAINER := hubble-relay
SUBDIR_CLUSTERMESH_APISERVER_CONTAINER := clustermesh-apiserver
SUBDIR_STANDALONE_DNS_PROXY_CONTAINER := standalone-dns-proxy
```

**모듈**: `go.mod`
- 모듈명: `github.com/cilium/cilium`
- Go 버전: 1.25.0
- 벤더링: `vendor/` 디렉토리 사용

### BPF 빌드

BPF 프로그램은 `clang`으로 컴파일된다. 런타임에 Agent가 엔드포인트별로 컴파일한다.

```go
// pkg/datapath/loader/compile.go
const (
    compiler = "clang"

    endpointPrefix = "bpf_lxc"
    endpointProg   = endpointPrefix + ".c"    // bpf_lxc.c
    endpointObj    = endpointPrefix + ".o"     // bpf_lxc.o

    hostEndpointPrefix = "bpf_host"
    hostEndpointProg   = hostEndpointPrefix + ".c"
    hostEndpointObj    = hostEndpointPrefix + ".o"

    xdpPrefix = "bpf_xdp"
    overlayPrefix = "bpf_overlay"
    wireguardPrefix = "bpf_wireguard"
)
```

### 컨테이너 이미지 빌드

`images/` 디렉토리에 각 바이너리의 Dockerfile이 있다:
- Cilium Agent 이미지: daemon, cilium-dbg, cilium-health, bugtool, plugins/cilium-cni 포함
- Operator 이미지: operator
- Hubble Relay 이미지: hubble-relay
- ClusterMesh API Server 이미지: clustermesh-apiserver

## Helm 차트

**디렉토리**: `install/kubernetes/cilium/`

Cilium의 표준 설치 방법은 Helm 차트이다. 주요 값:

```
install/kubernetes/cilium/
├── Chart.yaml
├── values.yaml           # 기본 설정 값
├── templates/
│   ├── cilium-agent/     # DaemonSet, ConfigMap, RBAC
│   ├── cilium-operator/  # Deployment, RBAC
│   ├── hubble/           # Hubble UI, Relay
│   └── ...
└── crds/                 # CRD 정의
```

## 의존성 관리

### 외부 Go 의존성 (주요)

| 의존성 | 용도 |
|--------|------|
| `github.com/cilium/hive` | DI 프레임워크 |
| `github.com/cilium/statedb` | 상태 데이터베이스 |
| `github.com/cilium/ebpf` | eBPF Go 라이브러리 |
| `github.com/cilium/proxy` | Envoy 프록시 통합 |
| `k8s.io/client-go` | K8s 클라이언트 |
| `k8s.io/apimachinery` | K8s API 기계 |
| `google.golang.org/grpc` | gRPC (Hubble, KVStore) |
| `github.com/spf13/cobra` | CLI 프레임워크 |
| `github.com/spf13/viper` | 설정 관리 |
| `github.com/prometheus/client_golang` | Prometheus 메트릭 |
| `github.com/vishvananda/netlink` | Linux netlink |
| `go.etcd.io/etcd/client/v3` | etcd 클라이언트 |

### 내부 Cilium 프로젝트 의존성

| 프로젝트 | 용도 |
|----------|------|
| `github.com/cilium/hive` | Hive DI 프레임워크 (셀, 라이프사이클) |
| `github.com/cilium/statedb` | StateDB (인메모리 상태 저장소) |
| `github.com/cilium/ebpf` | eBPF 맵/프로그램 Go 바인딩 |
| `github.com/cilium/proxy` | Envoy 프록시 Go 바인딩 |
| `github.com/cilium/dns` | DNS 파싱 |

## 코드 규모 참고

| 영역 | 추정 규모 |
|------|----------|
| Go 소스 코드 | ~100만+ 라인 |
| BPF C 소스 (bpf/) | ~3만+ 라인 |
| BPF 라이브러리 헤더 | 84개 파일 |
| BPF 맵 종류 | 34종 |
| cilium-dbg 커맨드 | 151개 파일 |
| API 모델 | `api/v1/models/` 다수 |
| pkg/ 패키지 | 100+ 하위 패키지 |
