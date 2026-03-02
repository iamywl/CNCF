# 5. Cilium 핵심 구성 요소 참조표

---

## BPF 프로그램 참조

| 프로그램 | 파일 | 부착 위치 | 역할 |
|----------|------|-----------|------|
| `cil_from_container` | `bpf_lxc.c` | TC ingress (veth) | Pod에서 나가는 패킷 처리, 정책 적용 |
| `cil_to_container` | `bpf_lxc.c` | TC egress (veth) | Pod으로 들어오는 패킷 처리 |
| `cil_from_host` | `bpf_host.c` | TC ingress (호스트) | 호스트에서 나가는 패킷 처리 |
| `cil_to_host` | `bpf_host.c` | TC egress (호스트) | 호스트로 들어오는 패킷 처리 |
| `cil_from_netdev` | `bpf_host.c` | TC ingress (물리 NIC) | 외부에서 들어오는 패킷 처리 |
| `cil_to_netdev` | `bpf_host.c` | TC egress (물리 NIC) | 외부로 나가는 패킷 처리 |
| `cil_from_overlay` | `bpf_overlay.c` | TC ingress (tunnel) | 터널에서 수신한 패킷 디캡슐화 |
| `cil_to_overlay` | `bpf_overlay.c` | TC egress (tunnel) | 패킷 캡슐화하여 터널로 전송 |
| `cil_sock4_connect` | `bpf_sock.c` | cgroup/connect4 | IPv4 connect() 시 서비스 DNAT |
| `cil_sock6_connect` | `bpf_sock.c` | cgroup/connect6 | IPv6 connect() 시 서비스 DNAT |
| `cil_xdp_entry` | `bpf_xdp.c` | XDP | 초고속 패킷 처리 (조기 드롭, LB) |
| `cil_from_wireguard` | `bpf_wireguard.c` | TC | WireGuard에서 수신한 패킷 처리 |
| `cil_to_wireguard` | `bpf_wireguard.c` | TC | WireGuard로 전송할 패킷 처리 |

---

## BPF 맵 참조

| 맵 | 패키지 | 키 | 값 | 용도 |
|----|--------|-----|-----|------|
| CT (v4/v6) | `pkg/maps/ctmap/` | 5-tuple | 상태, 통계 | 연결 추적 |
| NAT (v4/v6) | `pkg/maps/nat/` | 5-tuple | NAT'd 주소 | NAT 변환 |
| Policy | `pkg/maps/policymap/` | Identity+Port | Allow/Deny | 정책 결정 |
| IPCache | `pkg/maps/ipcache/` | IP/CIDR | Identity | IP→Identity 매핑 |
| LXC (Endpoint) | `pkg/maps/lxcmap/` | Endpoint ID | 인터페이스 정보 | Endpoint 메타데이터 |
| Service | `pkg/loadbalancer/maps/` | Frontend IP:Port | Backend 수 | 서비스 프론트엔드 |
| Backend | `pkg/loadbalancer/maps/` | Backend ID | IP:Port | 서비스 백엔드 |
| RevNAT | `pkg/loadbalancer/maps/` | RevNAT ID | Original IP:Port | 역방향 NAT |
| Metrics | `pkg/maps/metricsmap/` | Reason+Direction | Packets, Bytes | 데이터패스 통계 |
| Auth | `pkg/maps/authmap/` | Identity pair | 인증 상태 | mTLS 인증 캐시 |
| Bandwidth | `pkg/maps/bwmap/` | Endpoint ID | Rate limit | 대역폭 제한 |
| SRv6 | `pkg/maps/srv6map/` | SID | Policy | SRv6 라우팅 |

---

## BPF 라이브러리 헤더 참조 (`bpf/lib/`)

| 헤더 | 크기 | 담당 기능 |
|------|------|-----------|
| `nodeport.h` | ~84KB | NodePort, DSR, Maglev 로드밸런싱 |
| `policy.h` | ~16KB | 정책 조회 및 적용 로직 |
| `lb.h` | ~65KB | 서비스 로드밸런싱 핵심 로직 |
| `nat.h` | ~63KB | SNAT/DNAT 구현 |
| `conntrack.h` | ~38KB | 연결 추적 (CT) 로직 |
| `encap.h` | — | VXLAN/Geneve 캡슐화 |
| `identity.h` | — | Identity 조회 |
| `trace.h` | — | 패킷 추적 (디버깅) |
| `tailcall.h` | — | BPF tail call 메커니즘 |
| `ipv4.h` / `ipv6.h` | — | IP 프로토콜 처리 |
| `arp.h` | — | ARP 처리 |
| `ipsec.h` | — | IPsec 암호화 |
| `srv6.h` | — | SRv6 라우팅 |

---

## 외부 의존성 참조

| 의존성 | 패키지 | 용도 |
|--------|--------|------|
| etcd | `go.etcd.io/etcd/client/v3` | 분산 KV 저장소 (Identity, Endpoint 동기화) |
| Kubernetes | `k8s.io/client-go` | K8s API 연동 |
| cilium/ebpf | `github.com/cilium/ebpf` | eBPF 프로그램 로딩/관리 |
| Hive | `github.com/cilium/hive` | 의존성 주입 프레임워크 (코어는 외부 패키지) |
| StateDB | `github.com/cilium/statedb` | 인메모리 상태 DB (외부 패키지) |
| gRPC | `google.golang.org/grpc` | RPC 통신 |
| Envoy | `github.com/envoyproxy/go-control-plane` | L7 프록시 제어 |
| Cobra | `github.com/spf13/cobra` | CLI 프레임워크 |
| Viper | `github.com/spf13/viper` | 설정 관리 |
| Prometheus | `github.com/prometheus/client_golang` | 메트릭 수집 |

---

## CRD (Custom Resource Definition) 참조

| CRD | 약칭 | 용도 |
|-----|------|------|
| CiliumNetworkPolicy | CNP | 네임스페이스 단위 L3-L7 네트워크 정책 |
| CiliumClusterwideNetworkPolicy | CCNP | 클러스터 전체 네트워크 정책 |
| CiliumEndpoint | CEP | Endpoint 상태 (K8s에 동기화) |
| CiliumIdentity | CID | Security Identity 정보 |
| CiliumNode | CN | 노드 메타데이터 |
| CiliumExternalWorkload | CEW | 외부 워크로드 연결 |
| CiliumLocalRedirectPolicy | CLRP | 로컬 트래픽 리다이렉션 |
| CiliumEnvoyConfig | CEC | Envoy L7 프록시 설정 |
| CiliumBGPPeeringPolicy | CBGPP | BGP 피어링 설정 |
| CiliumLoadBalancerIPPool | CLBIP | LB IP 풀 관리 |
| CiliumCIDRGroup | CCG | CIDR 그룹 정의 |

---

## 핵심 구성 요소 요약표

제공하신 문서화 프레임워크에 맞춘 Cilium 프로젝트 참조:

| 구분 | 주요 내용 | 위치/도구 |
|------|-----------|-----------|
| **개요** | eBPF 기반 K8s 네트워킹/보안/관측 | `README.rst`, `Documentation/` |
| **설계** | Identity 기반 정책, BPF 데이터패스 | `bpf/`, `pkg/policy/`, `pkg/identity/` |
| **운영** | Helm 배포, DaemonSet/Deployment | `install/kubernetes/cilium/` |
| **인터페이스** | REST (OpenAPI), gRPC (protobuf) | `api/v1/` |
