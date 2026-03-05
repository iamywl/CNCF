# Cilium EDU - 교육 자료

## 프로젝트 소개

**Cilium**은 eBPF(extended Berkeley Packet Filter) 기반의 네트워킹, 보안, 옵저버빌리티 솔루션이다.
Linux 커널의 eBPF 기술을 활용하여 고성능 데이터플레인을 구현하며, Kubernetes 환경에서
Pod 네트워킹, 로드밸런싱, 네트워크 정책 적용, 트래픽 관찰을 담당한다.

- **CNCF Graduated 프로젝트** (2023년 10월 졸업)
- **소스코드 언어**: Go (컨트롤플레인), C (eBPF 데이터플레인)
- **라이선스**: Apache 2.0 (Go), GPL-2.0 / BSD-2-Clause (BPF C)
- **GitHub**: https://github.com/cilium/cilium

## 핵심 특징

| 특징 | 설명 |
|------|------|
| eBPF 데이터플레인 | 커널 내부에서 패킷 처리, iptables 대체 |
| Identity 기반 보안 | IP 주소가 아닌 보안 Identity로 정책 적용 |
| kube-proxy 대체 | BPF 기반 서비스 로드밸런싱 (ClusterIP, NodePort, LoadBalancer) |
| Hubble 옵저버빌리티 | L3/L4/L7 수준 네트워크 플로우 관찰 |
| ClusterMesh | 멀티클러스터 연결 및 서비스 디스커버리 |
| WireGuard/IPsec | 투명한 노드 간 트래픽 암호화 |
| Gateway API/Ingress | L7 트래픽 라우팅 (Envoy 기반) |
| BGP 지원 | BGP 피어링을 통한 외부 네트워크 통합 |

## EDU 문서 목차

### 기본 문서 (01~06)

| 번호 | 문서 | 내용 |
|------|------|------|
| 01 | [아키텍처](01-architecture.md) | 전체 아키텍처, Hive 셀 구조, 4대 컴포넌트 |
| 02 | [데이터 모델](02-data-model.md) | Endpoint, Node, Identity, Service/Frontend/Backend |
| 03 | [시퀀스 다이어그램](03-sequence-diagrams.md) | Pod 생성, 패킷 처리, 정책 업데이트 흐름 |
| 04 | [코드 구조](04-code-structure.md) | 디렉토리 구조, 빌드 시스템, 의존성 |
| 05 | [핵심 컴포넌트](05-core-components.md) | Daemon, EndpointManager, Datapath Loader, Policy, BPF Maps |
| 06 | [운영](06-operations.md) | 설치, 디버깅, 모니터링, 설정 |

### 심화 문서 (07~18)

| 번호 | 문서 | 내용 |
|------|------|------|
| 07 | eBPF 데이터패스 | bpf_lxc.c, bpf_host.c, TC/XDP 프로그램 |
| 08 | 네트워킹 | VXLAN/Geneve 터널, 다이렉트 라우팅, 네트워크 디바이스 |
| 09 | 로드밸런싱 | Maglev, DSR, SNAT, SessionAffinity, HealthCheck |
| 10 | 정책 엔진 | SelectorCache, Distillery, L3/L4/L7 정책, FQDN |
| 11 | 서비스 메시 | Envoy 통합, Gateway API, Ingress, L7 정책 |
| 12 | 멀티클러스터 | ClusterMesh, KVStore 동기화, 서비스 디스커버리 |
| 13 | 옵저버빌리티 | Hubble Server/Relay, 플로우 이벤트, Prometheus 메트릭 |
| 14 | IPAM | 클러스터풀, AWS ENI, Azure IPAM, 멀티풀 |
| 15 | 클라우드 프로바이더 | AWS, Azure, GCP 통합 |
| 16 | 인증/암호화 | Mutual Auth, WireGuard, IPsec, SPIFFE |
| 17 | CRD/K8s 통합 | CiliumNetworkPolicy, CiliumNode, CiliumEndpoint |
| 18 | 빌드/코드생성 | Makefile, go-swagger, BPF 컴파일 파이프라인 |

### PoC (Proof of Concept)

| 번호 | PoC | 시뮬레이션 대상 |
|------|-----|----------------|
| poc-01 | 아키텍처 | Hive DI 프레임워크, Cell/Module 패턴 |
| poc-02 | 데이터 모델 | Endpoint/Identity/Node 데이터 구조 |
| poc-03 | 패킷 파이프라인 | BPF 프로그램 체이닝 (tail call 시뮬레이션) |
| poc-04 | Hive DI | 의존성 주입 컨테이너, 셀 라이프사이클 |
| poc-05 | BPF 맵 | LRU/Hash/Array 맵 시뮬레이션 |
| poc-06 | 운영 | 설정 로드, 상태 체크, 메트릭 수집 |
| poc-07 | eBPF 데이터패스 | 패킷 분류, CT 조회, 정책 확인 |
| poc-08 | 네트워킹 | VXLAN 캡슐화/디캡슐화, FIB 룩업 |
| poc-09 | 로드밸런싱 | Maglev 해싱, 백엔드 선택, 세션 어피니티 |
| poc-10 | 정책 엔진 | SelectorCache, 정책 매칭, BPF policymap |
| poc-11 | 서비스 메시 | L7 프록시 리다이렉트, Envoy xDS |
| poc-12 | 멀티클러스터 | KVStore 동기화, 원격 서비스 디스커버리 |
| poc-13 | 옵저버빌리티 | 플로우 이벤트 수집, 링버퍼, 메트릭 집계 |
| poc-14 | IPAM | CIDR 할당, 풀 관리, IP 릴리스 |
| poc-15 | 클라우드 프로바이더 | ENI 할당, 서브넷 관리 |
| poc-16 | 인증/암호화 | Mutual Auth 핸드셰이크, 키 로테이션 |
| poc-17 | CRD/K8s | CRD 워처, 리소스 동기화 |
| poc-18 | 빌드/코드생성 | 코드 생성 파이프라인, 템플릿 컴파일 |

## 소스코드 위치

- **소스코드**: `/Users/ywlee/CNCF/cilium/`
- **EDU 디렉토리**: `/Users/ywlee/CNCF/cilium_EDU/`

## 참고 자료

- [Cilium 공식 문서](https://docs.cilium.io/)
- [Cilium GitHub](https://github.com/cilium/cilium)
- [eBPF.io](https://ebpf.io/)
- [Cilium Architecture Guide](https://docs.cilium.io/en/latest/overview/component-overview/)
