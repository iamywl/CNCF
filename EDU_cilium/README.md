# Cilium 프로젝트 문서화

Cilium 프로젝트의 아키텍처, 데이터 모델, 주요 흐름, 코드 구조를 계층별로 정리한 문서입니다.

---

## 목차

### 상위 수준 (Architecture & Design)

| 문서 | 설명 |
|------|------|
| [01-architecture.md](01-architecture.md) | 시스템 아키텍처 다이어그램 — 컴포넌트 구성과 연결 관계 |
| [02-data-model.md](02-data-model.md) | 데이터 모델 (ERD) — 핵심 데이터 구조와 관계 |
| [03-sequence-diagrams.md](03-sequence-diagrams.md) | 시퀀스 다이어그램 — 패킷 처리, 정책 적용, 서비스 로드밸런싱 흐름 |

### 하위 수준 (Code Level)

| 문서 | 설명 |
|------|------|
| [04-code-structure.md](04-code-structure.md) | 코드 구조 — 디렉토리, 패키지, API 명세 |
| [05-core-components.md](05-core-components.md) | 핵심 구성 요소 참조표 — BPF 프로그램, 맵, 주요 패키지 |
| [06-operations.md](06-operations.md) | 운영 가이드 — 설정, 배포, 트러블슈팅 |

### 기술 심화 (Deep Dive)

| 문서 | 설명 |
|------|------|
| [07-ebpf-datapath.md](07-ebpf-datapath.md) | eBPF 데이터패스 — 프로그램 유형, 맵 유형, Tail Call 체인, 컴파일 과정 |
| [08-networking.md](08-networking.md) | 네트워킹 — 터널(VXLAN/Geneve), 암호화, BGP, NAT, 듀얼스택 |
| [09-loadbalancing.md](09-loadbalancing.md) | 로드밸런싱 — Maglev, DSR, Session Affinity, Socket-level LB |
| [10-policy-engine.md](10-policy-engine.md) | 정책 엔진 — L3/L4/L7, FQDN, CIDR Group, 정책 평가 흐름 |
| [11-service-mesh.md](11-service-mesh.md) | 서비스 메시 — Envoy xDS, DNS Proxy, Gateway API, Ztunnel |
| [12-multicluster.md](12-multicluster.md) | 멀티클러스터 — ClusterMesh, MCS API, 크로스 클러스터 동기화 |
| [13-observability.md](13-observability.md) | 관측 — Hubble Observer/Relay, Prometheus, OpenTelemetry |
| [14-ipam.md](14-ipam.md) | IPAM — cluster-pool, ENI, Azure, multi-pool, 프리얼로케이션 |
| [15-cloud-providers.md](15-cloud-providers.md) | 클라우드 프로바이더 — AWS ENI, Azure NIC, Alibaba Cloud ENI |
| [16-auth-encryption.md](16-auth-encryption.md) | 인증/암호화 — WireGuard, IPsec, mTLS, SPIFFE/SPIRE |
| [17-crd-k8s.md](17-crd-k8s.md) | CRD/K8s 통합 — 20+ CRD, Informer/Watcher, controller-runtime |
| [18-build-codegen.md](18-build-codegen.md) | 빌드/코드생성 — Makefile, protobuf, deepcopy-gen, CI/CD |

### PoC (실행하며 체험)

각 문서의 개념을 직접 실행하여 동작 매커니즘을 확인할 수 있는 Go 프로젝트.

| PoC | 체험 내용 | 실행 |
|-----|-----------|------|
| [poc-01-architecture/](poc-01-architecture/) | gRPC 스트리밍(Hubble), UNIX 소켓 REST(daemon) 통신 패턴 | `go run grpc_server.go` / `go run grpc_client.go` |
| [poc-02-data-model/](poc-02-data-model/) | Label→Identity 매핑, IPCache, 정책 평가 전체 과정 | `go run main.go` |
| [poc-03-packet-pipeline/](poc-03-packet-pipeline/) | CT 조회→정책 평가→CT 생성→전달/드롭 파이프라인 | `go run main.go` |
| [poc-04-hive-di/](poc-04-hive-di/) | Hive DI 의존성 해결, 생명주기 관리 (Start/Stop 순서) | `go run main.go` |
| [poc-05-bpf-maps/](poc-05-bpf-maps/) | Policy Map, CT Map(LRU), Service Map 동작 | `go run main.go` |
| [poc-06-operations/](poc-06-operations/) | 설정 우선순위 체계, 트러블슈팅 시뮬레이션 | `go run main.go --simulate-trouble` |
| [poc-07-ebpf-datapath/](poc-07-ebpf-datapath/) | Tail Call 체인, LPM Trie, BPF 검증기, 컴파일 파이프라인 | `go run main.go` |
| [poc-08-networking/](poc-08-networking/) | VXLAN 캡슐화, NAT 변환, BGP 경로 광고, WireGuard 터널 | `go run main.go` |
| [poc-09-loadbalancing/](poc-09-loadbalancing/) | Maglev 일관 해싱, DSR, Session Affinity, Socket-level LB | `go run main.go` |
| [poc-10-policy-engine/](poc-10-policy-engine/) | 정책 평가, FQDN 정책, L7 프록시, Default Deny | `go run main.go` |
| [poc-11-service-mesh/](poc-11-service-mesh/) | xDS 프로토콜, DNS Proxy, L7 프록시 흐름, Gateway API | `go run main.go` |
| [poc-12-multicluster/](poc-12-multicluster/) | ClusterMesh 동기화, 글로벌 서비스, 크로스 클러스터 Identity | `go run main.go` |
| [poc-13-observability/](poc-13-observability/) | Hubble 파이프라인, Relay 집계, Prometheus 메트릭, 서비스 맵 | `go run main.go` |
| [poc-14-ipam/](poc-14-ipam/) | Cluster-Pool, Multi-Pool, ENI-style, 프리얼로케이션, 듀얼스택 | `go run main.go` |
| [poc-15-cloud-provider/](poc-15-cloud-provider/) | AWS ENI, Azure NIC, Alibaba Cloud ENI, 메타데이터 서비스 | `go run main.go` |
| [poc-16-auth-encryption/](poc-16-auth-encryption/) | WireGuard, IPsec SA, SPIFFE, mTLS 핸드셰이크, 키 로테이션 | `go run main.go` |
| [poc-17-crd-k8s/](poc-17-crd-k8s/) | Informer/Watcher, CRD 라이프사이클, Reconciler, GC | `go run main.go` |
| [poc-18-build-codegen/](poc-18-build-codegen/) | Protobuf 코드생성, DeepCopy, CRD YAML, 빌드 의존성 그래프 | `go run main.go` |

---

## Cilium 한 줄 요약

Cilium은 **eBPF** 기반의 Kubernetes 네트워킹, 보안, 관측 플랫폼이다.
커널 수준에서 패킷을 처리하여 높은 성능을 달성하고, Identity 기반 정책으로 보안을 강화한다.

## 기술 스택

### 언어 및 빌드

| 구분 | 기술 |
|------|------|
| 언어 | Go 1.25, C (BPF datapath) |
| 빌드 | Make, Go Build, Helm 3, Docker (멀티스테이지) |
| 코드 생성 | protobuf, go-swagger, deepcopy-gen, controller-gen, client-gen |
| 테스트 | Ginkgo, Testify, Gomega, BPF 통합 테스트 (150+ C 테스트) |
| CI/CD | GitHub Actions (88+ 워크플로우) |

### 커널 및 데이터패스

| 구분 | 기술 |
|------|------|
| eBPF 프로그램 | XDP, TC (ingress/egress), Socket (cgroup/connect), cgroup |
| BPF 맵 | Hash, LRU Hash, Array, LPM Trie, RingBuf, HashOfMaps |
| 커널 기능 | conntrack, netfilter, FIB, netlink, XFRM (IPsec) |
| BPF 라이브러리 | cilium/ebpf |

### 네트워킹

| 구분 | 기술 |
|------|------|
| 터널 프로토콜 | VXLAN, Geneve, VTEP |
| 암호화 | WireGuard, IPsec (AES), mTLS (SPIFFE/SPIRE) |
| 라우팅 | BGP (GoBGP), SRv6, Native Routing |
| 프로토콜 | IPv4/IPv6 (듀얼스택), ARP/NDP, IGMP, Multicast |
| LB 알고리즘 | Maglev (일관 해싱), Random, Session Affinity, DSR |
| NAT | SNAT/DNAT, NAT46/64, 역방향 NAT |

### 오케스트레이션 및 API

| 구분 | 기술 |
|------|------|
| 오케스트레이터 | Kubernetes (client-go, controller-runtime) |
| CNI/CRI | CNI Plugin, Docker Plugin, containerd |
| API 게이트웨이 | Gateway API (v1.4), Kubernetes Ingress |
| 서비스 메시 | Per-node 프록시 (사이드카 없음), Ztunnel (Istio Ambient 호환) |
| 멀티클러스터 | ClusterMesh, MCS API (Multi-Cluster Service) |
| CRD | CNP, CCNP, CEP, CID, CN, CEC, CBGP 등 20+ 종 |

### 프록시 및 정책

| 구분 | 기술 |
|------|------|
| L7 프록시 | Envoy (xDS 제어), DNS Proxy (standalone 포함) |
| 정책 계층 | L3 (CIDR/Identity), L4 (Port/Protocol), L7 (HTTP/gRPC/DNS/Kafka) |
| 정책 유형 | FQDN, CIDR Group, Egress Gateway, Local Redirect |

### 데이터 저장 및 상태

| 구분 | 기술 |
|------|------|
| 분산 KV | etcd (go.etcd.io/etcd/client/v3) |
| 인메모리 DB | StateDB (cilium/statedb) |
| K8s 저장소 | CRD 기반 (Identity, Endpoint, Node, Policy) |
| DI 프레임워크 | Hive (cilium/hive) |

### 관측 및 모니터링

| 구분 | 기술 |
|------|------|
| Flow 관측 | Hubble (Observer, Relay, UI, CLI) |
| 메트릭 | Prometheus (client_golang) |
| 트레이싱 | OpenTelemetry (OTLP) |
| 디버깅 | gops, pprof, BPF 패킷 트레이스 |

### 클라우드 프로바이더

| 구분 | 기술 |
|------|------|
| AWS | ENI IPAM, EC2 메타데이터 (aws-sdk-go-v2) |
| Azure | NIC IPAM, IMDS (azure-sdk-for-go) |
| Alibaba Cloud | ENI IPAM (alibaba-cloud-sdk-go) |

### IPAM 모드

| 모드 | 설명 |
|------|------|
| cluster-pool | 클러스터 범위 IP 풀 할당 (기본값) |
| kubernetes | K8s 네이티브 IPAM 위임 |
| eni | AWS ENI 기반 할당 |
| azure | Azure NIC 기반 할당 |
| alibaba-cloud | Alibaba Cloud ENI 기반 할당 |
| multi-pool | CiliumPodIPPool CRD 기반 다중 풀 |

### CLI 및 유틸리티

| 구분 | 기술 |
|------|------|
| CLI | Cobra, Pflag, Viper |
| 직렬화 | protobuf, JSON (json-iterator), YAML, CBOR |
| 해싱 | xxhash, murmur3 |
| 표현식 | CEL (Common Expression Language) |
| 로깅 | logrus, zap |
