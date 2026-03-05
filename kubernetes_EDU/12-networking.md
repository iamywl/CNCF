# 12. 네트워킹 심화

## 목차

1. [개요](#1-개요)
2. [Kubernetes 네트워킹 모델](#2-kubernetes-네트워킹-모델)
3. [Service 타입별 동작](#3-service-타입별-동작)
4. [kube-proxy 아키텍처](#4-kube-proxy-아키텍처)
5. [iptables 모드 상세](#5-iptables-모드-상세)
6. [IPVS 모드 상세](#6-ipvs-모드-상세)
7. [nftables 모드](#7-nftables-모드)
8. [EndpointSlice Controller](#8-endpointslice-controller)
9. [DNS (CoreDNS 통합)](#9-dns-coredns-통합)
10. [CNI 플러그인 인터페이스](#10-cni-플러그인-인터페이스)
11. [Service에서 Pod까지의 트래픽 흐름](#11-service에서-pod까지의-트래픽-흐름)
12. [Why: 플랫 네트워크 모델](#12-why-플랫-네트워크-모델)
13. [정리](#13-정리)

---

## 1. 개요

Kubernetes 네트워킹은 세 가지 네트워크 계층과 이를 연결하는 여러 컴포넌트로
구성된다. 모든 Pod는 고유한 IP 주소를 가지며, NAT 없이 다른 모든 Pod와
직접 통신할 수 있다.

### 소스 코드 위치

```
cmd/kube-proxy/
├── app/
│   ├── server.go           # ProxyServer, newProxyServer()
│   └── server_linux.go     # Linux 특화 createProxier()
pkg/proxy/
├── types.go                # Provider 인터페이스
├── iptables/
│   └── proxier.go          # iptables Proxier 구현
├── ipvs/
│   └── proxier.go          # IPVS Proxier 구현
├── nftables/
│   └── proxier.go          # nftables Proxier 구현 (새로운)
├── config/                 # Service/EndpointSlice 이벤트 핸들러
├── endpointschangetracker.go
├── servicechangetracker.go
├── endpointslicecache.go
└── healthcheck/
pkg/controller/endpointslice/
└── endpointslice_controller.go  # EndpointSlice Controller
```

---

## 2. Kubernetes 네트워킹 모델

### 2.1 세 가지 네트워크

```
┌─────────────────────────────────────────────────────────┐
│                    Kubernetes Cluster                     │
│                                                          │
│  ┌────────────────────────────────────────────────────┐  │
│  │              Node Network (물리/VM)                │  │
│  │              예: 192.168.1.0/24                    │  │
│  │                                                    │  │
│  │  ┌─────────────────────────────────────────────┐   │  │
│  │  │         Service Network (가상)              │   │  │
│  │  │         예: 10.96.0.0/12                    │   │  │
│  │  │         (ClusterIP 범위)                    │   │  │
│  │  └─────────────────────────────────────────────┘   │  │
│  │                                                    │  │
│  │  ┌─────────────────────────────────────────────┐   │  │
│  │  │         Pod Network (가상)                  │   │  │
│  │  │         예: 10.244.0.0/16                   │   │  │
│  │  │         (CNI가 관리)                        │   │  │
│  │  └─────────────────────────────────────────────┘   │  │
│  └────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────┘
```

### 2.2 네트워킹의 기본 규칙

| 규칙 | 설명 |
|------|------|
| Pod-to-Pod | 모든 Pod는 NAT 없이 다른 모든 Pod와 통신 가능 |
| Node-to-Pod | 모든 노드는 NAT 없이 모든 Pod와 통신 가능 |
| Pod 자기 IP | Pod가 보는 자신의 IP == 다른 Pod가 보는 해당 Pod의 IP |
| Service IP | 가상 IP (VIP), kube-proxy가 실제 Pod IP로 DNAT |

### 2.3 통신 경로별 메커니즘

```
┌─────────────────────────────────────────────────────┐
│                  통신 경로 요약                       │
├─────────────────┬───────────────────────────────────┤
│ 같은 노드 내    │ veth pair → bridge (또는 직접     │
│ Pod-to-Pod      │ 라우팅), 네트워크 플러그인이 처리 │
├─────────────────┼───────────────────────────────────┤
│ 다른 노드       │ CNI가 설정한 오버레이/라우팅     │
│ Pod-to-Pod      │ (VXLAN, BGP, eBPF 등)            │
├─────────────────┼───────────────────────────────────┤
│ Pod-to-Service  │ kube-proxy (iptables/IPVS/nftables)│
│                 │ 가 ClusterIP를 Pod IP로 DNAT     │
├─────────────────┼───────────────────────────────────┤
│ 외부-to-Service │ NodePort → kube-proxy → Pod      │
│                 │ 또는 LoadBalancer → NodePort      │
├─────────────────┼───────────────────────────────────┤
│ Pod-to-외부     │ SNAT (masquerade) → Node IP로     │
│                 │ 나가기                             │
└─────────────────┴───────────────────────────────────┘
```

---

## 3. Service 타입별 동작

### 3.1 Service 타입 개요

| 타입 | IP | 접근 범위 | 동작 방식 |
|------|-----|----------|-----------|
| ClusterIP | 가상 IP (VIP) | 클러스터 내부 | kube-proxy가 DNAT |
| NodePort | ClusterIP + NodePort | 클러스터 외부 | Node의 포트로 접근 |
| LoadBalancer | ClusterIP + NodePort + LB IP | 클러스터 외부 | 클라우드 LB 연동 |
| ExternalName | 없음 | DNS 별칭 | CNAME 레코드 반환 |

### 3.2 ClusterIP Service

```
Client Pod (10.244.1.5)
    │
    │ dst: 10.96.0.100:80 (Service ClusterIP)
    │
    ▼
kube-proxy (iptables/IPVS)
    │
    │ DNAT: 10.96.0.100:80 → 10.244.2.3:8080 (Backend Pod)
    │
    ▼
Backend Pod (10.244.2.3:8080)
    │
    │ 응답: src=10.244.2.3 → Client Pod
    │ (DNAT 역변환은 conntrack이 처리)
    │
    ▼
Client Pod (10.244.1.5)
```

### 3.3 NodePort Service

```
외부 클라이언트
    │
    │ dst: NodeIP:30080 (NodePort)
    │
    ▼
Node (192.168.1.10:30080)
    │
    │ KUBE-NODEPORTS 체인에서 처리
    │ DNAT: NodeIP:30080 → PodIP:8080
    │ SNAT: src → NodeIP (필요 시, externalTrafficPolicy에 따라)
    │
    ▼
Backend Pod (10.244.2.3:8080)

NodePort 범위: 30000-32767 (기본값)
```

### 3.4 LoadBalancer Service

```
인터넷 트래픽
    │
    │ dst: LB_IP:80 (External IP)
    │
    ▼
Cloud Load Balancer
    │
    │ 트래픽을 NodePort로 분산
    │ dst: NodeIP:30080
    │
    ▼
Node의 kube-proxy
    │
    │ DNAT → PodIP:8080
    │
    ▼
Backend Pod

LoadBalancer = ClusterIP + NodePort + 클라우드 LB 프로비저닝
```

### 3.5 ExternalName Service

```yaml
apiVersion: v1
kind: Service
metadata:
  name: my-service
spec:
  type: ExternalName
  externalName: my.database.example.com
```

```
Pod → DNS 조회: my-service.default.svc.cluster.local
    → CoreDNS 응답: CNAME my.database.example.com
    → 추가 DNS 조회로 실제 IP 획득
    → 직접 연결 (kube-proxy 관여 없음)
```

### 3.6 externalTrafficPolicy

| 정책 | 동작 | 장점 | 단점 |
|------|------|------|------|
| Cluster (기본) | 모든 노드의 Pod로 분산 | 균등 분산 | 추가 hop, 클라이언트 IP 손실 (SNAT) |
| Local | 로컬 노드의 Pod로만 전달 | 클라이언트 IP 보존 | 불균등 분산, 로컬 Pod 없으면 드롭 |

```
externalTrafficPolicy: Cluster
─────────────────────────────
외부 → Node A (NodePort) → DNAT → Node B의 Pod
                           ↑ SNAT: src → Node A IP
                           (클라이언트 IP 손실)

externalTrafficPolicy: Local
─────────────────────────────
외부 → Node A (NodePort) → DNAT → Node A의 Pod만
                           (클라이언트 IP 보존)
       Node B (NodePort) → 로컬 Pod 없으면 DROP
```

---

## 4. kube-proxy 아키텍처

### 4.1 ProxyServer 구조체

`cmd/kube-proxy/app/server.go` (라인 161-178):

```go
type ProxyServer struct {
    Config          *kubeproxyconfig.KubeProxyConfiguration
    Client          clientset.Interface
    Broadcaster     events.EventBroadcaster
    Recorder        events.EventRecorder
    NodeRef         *v1.ObjectReference
    HealthzServer   *healthcheck.ProxyHealthServer
    NodeName        string
    PrimaryIPFamily v1.IPFamily
    NodeIPs         map[v1.IPFamily]net.IP
    podCIDRs        []string
    NodeManager     *proxy.NodeManager
    Proxier         proxy.Provider      // 핵심: 실제 프록시 구현체
}
```

### 4.2 Provider 인터페이스

`pkg/proxy/types.go` (라인 28-40):

```go
type Provider interface {
    config.EndpointSliceHandler   // EndpointSlice 변경 처리
    config.ServiceHandler         // Service 변경 처리
    config.NodeTopologyHandler    // 노드 토폴로지 변경 처리
    config.ServiceCIDRHandler     // ServiceCIDR 변경 처리

    Sync()                        // 즉시 동기화
    SyncLoop()                    // 주기적 동기화 루프
}
```

### 4.3 kube-proxy 기동 흐름

```
NewProxyCommand()
    │
    ├─ opts.Complete()
    ├─ opts.Validate()
    └─ opts.Run(ctx)
         │
         └─ newProxyServer(ctx, config, ...)
              │
              ├─ 1. Client 생성
              ├─ 2. NodeManager 생성 (자기 노드 정보)
              ├─ 3. NodeIPs, PrimaryIPFamily 감지
              ├─ 4. Proxier 생성 (mode에 따라)
              │     s.createProxier(ctx, config, dualStackSupported, initOnly)
              │     → iptables.NewDualStackProxier() 또는
              │     → ipvs.NewDualStackProxier() 또는
              │     → nftables.NewDualStackProxier()
              └─ 5. ProxyServer 반환

ProxyServer.Run(ctx):
    │
    ├─ Informer 생성 및 시작
    │     ServiceInformer, EndpointSliceInformer, NodeInformer
    │
    ├─ 이벤트 핸들러 등록
    │     config.NewServiceConfig(serviceInformer, ...)
    │     → Proxier.OnServiceAdd/Update/Delete/Synced
    │     config.NewEndpointSliceConfig(endpointSliceInformer, ...)
    │     → Proxier.OnEndpointSliceAdd/Update/Delete/Synced
    │
    ├─ Informer 동기화 대기
    │
    ├─ HealthzServer 시작
    │
    └─ Proxier.SyncLoop() (메인 루프)
         └─ 주기적으로 syncProxyRules() 호출
```

### 4.4 프록시 모드 비교

| 항목 | iptables | IPVS | nftables |
|------|----------|------|----------|
| 구현 위치 | `pkg/proxy/iptables/` | `pkg/proxy/ipvs/` | `pkg/proxy/nftables/` |
| 커널 기능 | netfilter/iptables | IPVS + iptables | nftables |
| 규칙 수 | O(N*M) | O(N) + O(M) | O(N*M) 최적화 |
| 로드밸런싱 | 랜덤(확률) | RR, WRR, LC, DH 등 | 랜덤(확률) |
| 대규모 | 느림(수천 서비스) | 빠름(해시 테이블) | 중간 |
| 상태 | 기본 모드 | 선택적 | 신규(beta) |

---

## 5. iptables 모드 상세

### 5.1 Proxier 구조체

`pkg/proxy/iptables/proxier.go` (라인 133 이후):

```go
type Proxier struct {
    ipFamily         v1.IPFamily
    endpointsChanges *proxy.EndpointsChangeTracker
    serviceChanges   *proxy.ServiceChangeTracker

    mu               sync.Mutex
    svcPortMap       proxy.ServicePortMap     // Service → ServicePort 매핑
    endpointsMap     proxy.EndpointsMap        // Service → Endpoints 매핑

    iptables         utiliptables.Interface    // iptables 조작 인터페이스
    masqueradeAll    bool
    masqueradeMark   string
    conntrack        conntrack.Interface

    syncPeriod       time.Duration            // 동기화 주기
    minSyncPeriod    time.Duration

    nodePortAddresses proxyutil.NodePortAddresses
    networkInterfacer proxyutil.NetworkInterfacer
}
```

### 5.2 iptables 체인 구조

`pkg/proxy/iptables/proxier.go` (라인 53-87)에 정의된 체인들:

```go
const (
    kubeServicesChain         = "KUBE-SERVICES"
    kubeExternalServicesChain = "KUBE-EXTERNAL-SERVICES"
    kubeNodePortsChain        = "KUBE-NODEPORTS"
    kubePostroutingChain      = "KUBE-POSTROUTING"
    kubeMarkMasqChain         = "KUBE-MARK-MASQ"
    kubeForwardChain          = "KUBE-FORWARD"
    kubeProxyFirewallChain    = "KUBE-PROXY-FIREWALL"
)
```

### 5.3 iptables 체인 흐름도

```
패킷 입력
    │
    ▼
┌─────────────────────────────────────────────────────────────┐
│ nat 테이블                                                   │
│                                                              │
│ PREROUTING ──► KUBE-SERVICES                                │
│                    │                                         │
│                    ├─ ClusterIP 매칭                         │
│                    │    └─► KUBE-SVC-XXXXX (서비스별 체인)   │
│                    │         ├─ 확률 1/3 → KUBE-SEP-AAAA    │
│                    │         ├─ 확률 1/2 → KUBE-SEP-BBBB    │
│                    │         └─ 확률 1/1 → KUBE-SEP-CCCC    │
│                    │                                         │
│                    ├─ NodePort 매칭                          │
│                    │    └─► KUBE-NODEPORTS                   │
│                    │         └─► KUBE-SVC-XXXXX              │
│                    │                                         │
│                    └─ ExternalIP 매칭                        │
│                         └─► KUBE-EXT-XXXXX                   │
│                              └─► KUBE-SVC-XXXXX              │
│                                                              │
│ KUBE-SEP-AAAA:                                              │
│   -j KUBE-MARK-MASQ (src=Pod이면 마스커레이드 마킹)        │
│   -j DNAT --to-destination 10.244.1.5:8080                  │
│                                                              │
│ OUTPUT ──► KUBE-SERVICES (로컬 프로세스에서 나가는 패킷)   │
│                                                              │
│ POSTROUTING ──► KUBE-POSTROUTING                            │
│                   └─ 마킹된 패킷에 MASQUERADE 적용          │
│                                                              │
└─────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────┐
│ filter 테이블                                                │
│                                                              │
│ FORWARD ──► KUBE-FORWARD                                    │
│              └─ mark 매칭 패킷 ACCEPT                       │
│                                                              │
│ INPUT ──► KUBE-EXTERNAL-SERVICES                            │
│            └─ 외부 서비스 트래픽 필터링                     │
│                                                              │
│          ──► KUBE-PROXY-FIREWALL                            │
│               └─ 방화벽 규칙 적용                           │
└─────────────────────────────────────────────────────────────┘
```

### 5.4 syncProxyRules 핵심 흐름

`pkg/proxy/iptables/proxier.go` (라인 638 이후):

```
syncProxyRules()
│
├─ 1. Lock 획득
│     proxier.mu.Lock()
│
├─ 2. 초기화 확인
│     if !proxier.isInitialized(): return
│
├─ 3. Service/Endpoints 변경 적용
│     proxier.svcPortMap.Update(proxier.serviceChanges)
│     proxier.endpointsMap.Update(proxier.endpointsChanges)
│
├─ 4. Full Sync 여부 결정
│     doFullSync := proxier.needFullSync ||
│                   time.Since(lastFullSync) > FullSyncPeriod
│
├─ 5. Full Sync 시: 기본 체인 및 점프 규칙 보장
│     for _, jump := range iptablesJumpChains:
│         iptables.EnsureChain(jump.table, jump.dstChain)
│         iptables.EnsureRule(Prepend, jump.table, jump.srcChain, ...)
│
├─ 6. iptables-restore 데이터 구성
│     for svcPortName, svcPort := range proxier.svcPortMap:
│         ├─ KUBE-SVC-XXXXX 체인 생성
│         ├─ ClusterIP 규칙 추가
│         ├─ ExternalIP, LoadBalancerIP 규칙 추가
│         ├─ NodePort 규칙 추가
│         │
│         └─ Endpoints 순회:
│             for i, endpoint := range endpoints:
│                 ├─ KUBE-SEP-XXXXX 체인 생성
│                 ├─ DNAT 규칙 추가 (확률 기반)
│                 └─ Masquerade 규칙 추가
│
├─ 7. iptables-restore 일괄 적용
│     proxier.iptables.RestoreAll(data, ...)
│
├─ 8. 메트릭 업데이트
│     SyncProxyRulesLatency.Observe(...)
│
└─ 9. conntrack 정리
      deleteConntrackStaleEntries(...)
```

### 5.5 확률 기반 로드밸런싱

```
3개의 Endpoint가 있는 경우:

KUBE-SVC-XXXXX:
  -m statistic --mode random --probability 0.33333 -j KUBE-SEP-AAAA
  -m statistic --mode random --probability 0.50000 -j KUBE-SEP-BBBB
  -j KUBE-SEP-CCCC  (나머지 전부)

확률 계산:
  Endpoint 1: 1/3 = 0.33333
  Endpoint 2: 1/2 = 0.50000 (남은 2개 중 1개)
  Endpoint 3: 1/1 = 1.00000 (나머지 전부 → -j로 직접)

실제 분산 확률:
  EP1: 33.3%
  EP2: (1-33.3%) × 50% = 33.3%
  EP3: (1-33.3%) × (1-50%) = 33.3%
  → 균등 분산!
```

### 5.6 대규모 클러스터 최적화

```go
// proxier.go:86-87
const largeClusterEndpointsThreshold = 1000
```

1000개 이상의 엔드포인트가 있으면 "대규모 클러스터 모드"로 전환:
- 서비스별 체인 이름을 짧게 생성 (디버깅 정보 축소)
- 불필요한 주석 제거
- iptables-restore 데이터 크기 최소화

---

## 6. IPVS 모드 상세

### 6.1 IPVS 개요

IPVS(IP Virtual Server)는 Linux 커널의 L4 로드밸런서이다.
해시 테이블 기반으로 동작하여 서비스/엔드포인트 수에 관계없이 O(1) 룩업을 제공한다.

### 6.2 IPVS Proxier

`pkg/proxy/ipvs/proxier.go` (라인 161):

```go
type Proxier struct {
    ipFamily          v1.IPFamily
    endpointsChanges  *proxy.EndpointsChangeTracker
    serviceChanges    *proxy.ServiceChangeTracker

    mu                sync.Mutex
    svcPortMap        proxy.ServicePortMap
    endpointsMap      proxy.EndpointsMap

    ipvs              utilipvs.Interface        // IPVS 조작
    iptables          utiliptables.Interface     // 보조 iptables 규칙
    ipset             utilipset.Interface        // IP set 관리

    schedulerList     map[string]bool            // 사용 가능한 스케줄러
    syncPeriod        time.Duration
    minSyncPeriod     time.Duration

    excludeCIDRs      []string                   // IPVS에서 제외할 CIDR
    networkInterfacer proxyutil.NetworkInterfacer
}
```

### 6.3 IPVS 로드밸런싱 알고리즘

| 알고리즘 | 플래그 | 설명 |
|----------|--------|------|
| Round Robin | rr | 순차 분산 (기본값) |
| Least Connection | lc | 연결 수 최소인 서버 선택 |
| Destination Hashing | dh | 목적지 IP 기반 해싱 |
| Source Hashing | sh | 소스 IP 기반 해싱 (세션 어피니티) |
| Shortest Expected Delay | sed | 예상 지연 최소인 서버 |
| Never Queue | nq | SED의 변형, 활성 연결 0인 서버 우선 |
| Weighted Round Robin | wrr | 가중 순차 분산 |
| Weighted Least Connection | wlc | 가중 최소 연결 |

### 6.4 IPVS 동작 방식

```
IPVS 모드의 규칙 구조:

1. 가상 서버 생성 (Service)
   ipvsadm -A -t 10.96.0.100:80 -s rr

2. 실제 서버 추가 (Endpoints)
   ipvsadm -a -t 10.96.0.100:80 -r 10.244.1.5:8080 -m
   ipvsadm -a -t 10.96.0.100:80 -r 10.244.2.3:8080 -m
   ipvsadm -a -t 10.96.0.100:80 -r 10.244.3.7:8080 -m

3. kube-ipvs0 더미 인터페이스에 VIP 바인딩
   ip addr add 10.96.0.100/32 dev kube-ipvs0

패킷 흐름:
Client → dst=10.96.0.100:80
  → kube-ipvs0에서 수신 (로컬 주소이므로)
  → IPVS가 커널에서 DNAT 수행
  → dst=10.244.1.5:8080 (선택된 백엔드)
  → 패킷 포워딩
```

### 6.5 IPVS가 iptables도 사용하는 이유

IPVS는 L4 로드밸런싱만 담당한다. 다음 기능에는 여전히 iptables가 필요하다:

```
IPVS로 처리:
✓ ClusterIP → Pod 로드밸런싱
✓ NodePort → Pod 로드밸런싱
✓ LoadBalancer IP → Pod 로드밸런싱

iptables로 보충:
✓ SNAT/Masquerade (POSTROUTING)
✓ NodePort 패킷 마킹
✓ 방화벽 규칙 (KUBE-PROXY-FIREWALL)
✓ 소스 범위 제한 (loadBalancerSourceRanges)
```

### 6.6 IPVS vs iptables 성능

```
서비스 수에 따른 규칙 업데이트 시간 비교:

서비스 수 │  iptables  │   IPVS
──────────┼────────────┼──────────
    100   │   ~50ms    │   ~10ms
   1,000  │   ~500ms   │   ~20ms
   5,000  │   ~3s      │   ~30ms
  10,000  │   ~10s     │   ~50ms
  50,000  │   ~60s+    │   ~100ms

이유:
- iptables: 선형 규칙 매칭 O(n), 전체 규칙 교체
- IPVS: 해시 테이블 기반 O(1) 룩업, 증분 업데이트
```

---

## 7. nftables 모드

### 7.1 개요

nftables는 iptables의 후속 기술로, Kubernetes v1.29에서 alpha로 도입되었다.
`pkg/proxy/nftables/proxier.go`에 구현되어 있다.

### 7.2 nftables의 장점

| 항목 | iptables | nftables |
|------|----------|----------|
| 원자적 규칙 교체 | iptables-restore (부분적) | nft -f (완전 원자적) |
| 성능 | 선형 매칭 | 맵/세트 기반 O(1) 룩업 |
| 규칙 표현력 | 체인별 순차 | 맵, 세트, 연결 지원 |
| 듀얼 스택 | IPv4/IPv6 별도 규칙 | 단일 규칙으로 처리 가능 |
| 커널 버전 | 2.x+ | 3.13+ |

### 7.3 iptables 대체 예시

```
iptables (3개 규칙):
  -A KUBE-SVC-XXX -m statistic --probability 0.333 -j KUBE-SEP-A
  -A KUBE-SVC-XXX -m statistic --probability 0.500 -j KUBE-SEP-B
  -A KUBE-SVC-XXX -j KUBE-SEP-C

nftables (1개 규칙 + 맵):
  nft add map ip kube-proxy svc-endpoints-XXX { type uint32 : verdict }
  nft add element ip kube-proxy svc-endpoints-XXX {
    0 : goto sep-A, 1 : goto sep-B, 2 : goto sep-C
  }
  nft add rule ip kube-proxy svc-XXX numgen random mod 3 vmap @svc-endpoints-XXX
```

---

## 8. EndpointSlice Controller

### 8.1 개요

EndpointSlice Controller는 Service에 매칭되는 Pod의 IP:Port 정보를 EndpointSlice
리소스로 관리한다. kube-proxy는 이 EndpointSlice를 watch하여 프록시 규칙을 구성한다.

### 8.2 Controller 생성

`pkg/controller/endpointslice/endpointslice_controller.go` (라인 84-150):

```go
func NewController(ctx context.Context,
    podInformer, serviceInformer, nodeInformer, endpointSliceInformer,
    maxEndpointsPerSlice int32, client clientset.Interface,
    endpointUpdatesBatchPeriod time.Duration) *Controller {

    c := &Controller{
        client: client,
        serviceQueue: workqueue.NewTypedRateLimitingQueueWithConfig(
            workqueue.NewTypedMaxOfRateLimiter(
                workqueue.NewTypedItemExponentialFailureRateLimiter[string](
                    defaultSyncBackOff,  // 1초
                    maxSyncBackOff,      // 1000초
                ),
                &workqueue.TypedBucketRateLimiter[string]{
                    Limiter: rate.NewLimiter(rate.Limit(10), 100),
                },
            ),
            ...
        ),
        workerLoopPeriod: time.Second,
    }

    // Informer 이벤트 핸들러 등록
    serviceInformer.Informer().AddEventHandler(
        Add:    c.onServiceUpdate,
        Update: c.onServiceUpdate,
        Delete: c.onServiceDelete,
    )
    podInformer.Informer().AddEventHandler(
        Add:    func(obj) { c.onPodUpdate(nil, obj) },
        Update: c.onPodUpdate,
        Delete: func(obj) { c.onPodUpdate(obj, nil) },
    )
    endpointSliceInformer.Informer().AddEventHandler(
        Add:    c.onEndpointSliceAdd,
        Update: c.onEndpointSliceUpdate,
        Delete: c.onEndpointSliceDelete,
    )
}
```

### 8.3 EndpointSlice 동작 원리

```
Service (selector: app=web)
    │
    │ EndpointSlice Controller가 매칭하는 Pod 탐색
    │
    ▼
EndpointSlice 생성/업데이트:
┌────────────────────────────────────────────┐
│ EndpointSlice "web-abc12"                  │
│                                            │
│ addressType: IPv4                          │
│ endpoints:                                 │
│   - addresses: [10.244.1.5]               │
│     conditions:                            │
│       ready: true                          │
│       serving: true                        │
│       terminating: false                   │
│     nodeName: node-1                       │
│     targetRef: {kind: Pod, name: web-0}   │
│                                            │
│   - addresses: [10.244.2.3]               │
│     conditions:                            │
│       ready: true                          │
│     nodeName: node-2                       │
│     targetRef: {kind: Pod, name: web-1}   │
│                                            │
│ ports:                                     │
│   - port: 8080                            │
│     protocol: TCP                          │
│     name: http                            │
└────────────────────────────────────────────┘
```

### 8.4 EndpointSlice vs Endpoints

| 항목 | Endpoints (구) | EndpointSlice (신) |
|------|---------------|-------------------|
| 엔드포인트 수 제한 | 없음 (거대 객체 가능) | 최대 100개/slice |
| 업데이트 | 전체 교체 | 변경된 slice만 |
| 토폴로지 | 미지원 | 노드별 힌트 지원 |
| 듀얼 스택 | IPv4만 | IPv4/IPv6 별도 slice |
| API 그룹 | v1 | discovery.k8s.io/v1 |

### 8.5 EndpointSlice 분할 전략

```
Service에 300개의 Pod가 있는 경우:

maxEndpointsPerSlice = 100 (기본값)

EndpointSlice "svc-abc12" (100개 endpoints)
EndpointSlice "svc-def34" (100개 endpoints)
EndpointSlice "svc-ghi56" (100개 endpoints)

Pod가 1개 제거되면:
→ 해당 Pod가 속한 slice만 업데이트
→ 나머지 2개 slice는 변경 없음
→ kube-proxy는 변경된 1개 slice의 watch event만 수신
```

---

## 9. DNS (CoreDNS 통합)

### 9.1 CoreDNS 역할

CoreDNS는 Kubernetes 클러스터의 기본 DNS 서버로, 다음 레코드를 자동 관리한다:

| 레코드 | 형식 | 예시 |
|--------|------|------|
| Service A | `<svc>.<ns>.svc.cluster.local` | `my-svc.default.svc.cluster.local → 10.96.0.100` |
| Pod A | `<pod-ip-dashed>.<ns>.pod.cluster.local` | `10-244-1-5.default.pod.cluster.local → 10.244.1.5` |
| SRV | `_<port>._<proto>.<svc>.<ns>.svc.cluster.local` | `_http._tcp.my-svc.default.svc.cluster.local` |
| Headless | `<svc>.<ns>.svc.cluster.local` | Pod IP 목록 직접 반환 (ClusterIP: None) |

### 9.2 DNS 조회 흐름

```
Pod 내부에서 my-svc.default.svc.cluster.local 조회:
│
├─ 1. Pod의 /etc/resolv.conf 확인
│     nameserver 10.96.0.10  (CoreDNS ClusterIP)
│     search default.svc.cluster.local svc.cluster.local cluster.local
│     ndots:5
│
├─ 2. CoreDNS로 DNS 쿼리 전송
│     dst: 10.96.0.10:53
│
├─ 3. kube-proxy가 DNAT
│     10.96.0.10:53 → CoreDNS Pod IP:53
│
├─ 4. CoreDNS가 kubernetes 플러그인으로 처리
│     ├─ API 서버에서 Service/EndpointSlice 캐시
│     ├─ 매칭되는 Service 찾기
│     └─ ClusterIP 또는 Pod IP 반환
│
└─ 5. 응답: 10.96.0.100 (ClusterIP)
```

### 9.3 Headless Service DNS

```
Headless Service (clusterIP: None):
│
├─ 일반 DNS 조회:
│   my-svc.default.svc.cluster.local
│   → 10.244.1.5, 10.244.2.3, 10.244.3.7 (모든 Pod IP)
│
└─ 특정 Pod DNS (StatefulSet):
    web-0.my-svc.default.svc.cluster.local → 10.244.1.5
    web-1.my-svc.default.svc.cluster.local → 10.244.2.3
    web-2.my-svc.default.svc.cluster.local → 10.244.3.7
```

### 9.4 ndots 설정의 의미

```
ndots:5 (기본값)의 동작:

my-svc 조회 시 (dots=0, 0 < 5이므로 search 도메인 먼저):
  1. my-svc.default.svc.cluster.local  → 성공! (여기서 멈춤)
  2. my-svc.svc.cluster.local          → (위에서 성공하면 불필요)
  3. my-svc.cluster.local
  4. my-svc                            (absolute query)

google.com 조회 시 (dots=1, 1 < 5이므로 search 도메인 먼저):
  1. google.com.default.svc.cluster.local  → NXDOMAIN
  2. google.com.svc.cluster.local          → NXDOMAIN
  3. google.com.cluster.local              → NXDOMAIN
  4. google.com                            → 성공!

문제: 외부 도메인 조회 시 불필요한 4번의 DNS 쿼리 발생
해결: 외부 도메인에는 FQDN (끝에 . 추가): google.com.
```

---

## 10. CNI 플러그인 인터페이스

### 10.1 CNI 개요

CNI(Container Network Interface)는 컨테이너의 네트워크 네임스페이스를 설정하고
해제하는 표준 인터페이스이다. Kubernetes는 CNI를 통해 다양한 네트워크 플러그인과
통합된다.

### 10.2 CNI 호출 흐름

```
kubelet → CRI (containerd/CRI-O) → CNI 플러그인

Pod Sandbox 생성 시:
1. kubelet: RunPodSandbox() 호출
2. CRI 런타임: pause 컨테이너 생성 + 네트워크 네임스페이스 생성
3. CRI 런타임: CNI ADD 호출
   → /opt/cni/bin/<plugin> < config.json
   → IP 주소 할당 + veth pair 생성 + 라우팅 설정
4. 결과: Pod가 네트워크 연결됨

Pod Sandbox 삭제 시:
1. CRI 런타임: CNI DEL 호출
   → IP 주소 반환 + 네트워크 리소스 정리
```

### 10.3 CNI 설정 파일 예시

```json
// /etc/cni/net.d/10-flannel.conflist
{
  "name": "cbr0",
  "cniVersion": "0.3.1",
  "plugins": [
    {
      "type": "flannel",
      "delegate": {
        "hairpinMode": true,
        "isDefaultGateway": true
      }
    },
    {
      "type": "portmap",
      "capabilities": {
        "portMappings": true
      }
    }
  ]
}
```

### 10.4 주요 CNI 플러그인 비교

| 플러그인 | 네트워크 모델 | 오버레이 | 암호화 | 정책 엔진 |
|----------|-------------|---------|--------|-----------|
| Flannel | VXLAN/host-gw | O | X | X |
| Calico | BGP/VXLAN/eBPF | 선택 | WireGuard | O |
| Cilium | eBPF | 선택 | WireGuard | O (L7) |
| Weave | VXLAN | O | O | O |
| AWS VPC CNI | 네이티브 VPC | X | X | X (SG) |

### 10.5 네트워크 네임스페이스 구조

```
┌──────────────────────────────────────────────────────┐
│ 호스트 네트워크 네임스페이스 (Node)                  │
│                                                      │
│  eth0: 192.168.1.10                                 │
│  cni0 (bridge): 10.244.0.1                          │
│                                                      │
│  ┌────────┐  ┌────────┐  ┌────────┐                │
│  │ vethA  │  │ vethB  │  │ vethC  │                │
│  └────┬───┘  └────┬───┘  └────┬───┘                │
│       │           │           │                      │
│ ──────┼───────────┼───────────┼──── (veth pair) ──  │
│       │           │           │                      │
│  ┌────┴───┐  ┌────┴───┐  ┌────┴───┐                │
│  │Pod NS-A│  │Pod NS-B│  │Pod NS-C│                │
│  │        │  │        │  │        │                │
│  │eth0:   │  │eth0:   │  │eth0:   │                │
│  │10.244  │  │10.244  │  │10.244  │                │
│  │.0.2    │  │.0.3    │  │.0.4    │                │
│  └────────┘  └────────┘  └────────┘                │
└──────────────────────────────────────────────────────┘
```

---

## 11. Service에서 Pod까지의 트래픽 흐름

### 11.1 ClusterIP 트래픽 흐름 (iptables 모드)

```
Client Pod (10.244.1.5, Node A)
    │
    │ 1. 앱에서 my-svc:80 으로 요청
    │    DNS 조회: my-svc.default.svc.cluster.local → 10.96.0.100
    │    connect(10.96.0.100:80)
    │
    │ 2. 패킷 생성
    │    src=10.244.1.5  dst=10.96.0.100:80
    │
    ▼
Node A의 netfilter (OUTPUT 체인 → nat 테이블)
    │
    │ 3. KUBE-SERVICES 체인 매칭
    │    -d 10.96.0.100/32 -p tcp --dport 80 -j KUBE-SVC-XXXXX
    │
    │ 4. KUBE-SVC-XXXXX에서 확률 기반 선택
    │    0.333 확률 → KUBE-SEP-AAAA (10.244.2.3:8080)  ← 선택됨!
    │    0.500 확률 → KUBE-SEP-BBBB (10.244.3.7:8080)
    │    나머지  → KUBE-SEP-CCCC (10.244.1.8:8080)
    │
    │ 5. KUBE-SEP-AAAA에서 DNAT
    │    -j DNAT --to-destination 10.244.2.3:8080
    │    패킷: src=10.244.1.5  dst=10.244.2.3:8080
    │
    │ 6. conntrack 테이블에 기록
    │    (10.244.1.5:port → 10.96.0.100:80) → (10.244.2.3:8080)
    │
    ▼
Node A → 라우팅 → CNI 오버레이/라우팅 → Node B
    │
    ▼
Node B의 Pod (10.244.2.3:8080) 에서 처리
    │
    │ 7. 응답 패킷
    │    src=10.244.2.3:8080  dst=10.244.1.5:port
    │
    ▼
Node A의 conntrack이 역DNAT 적용
    │    src=10.96.0.100:80  dst=10.244.1.5:port (원래 주소로 복원)
    │
    ▼
Client Pod에 응답 도착
```

### 11.2 NodePort 트래픽 흐름 (iptables 모드)

```
외부 클라이언트 (203.0.113.50)
    │
    │ 1. NodeIP:30080 으로 요청
    │    dst=192.168.1.10:30080
    │
    ▼
Node A의 netfilter (PREROUTING 체인)
    │
    │ 2. KUBE-SERVICES → KUBE-NODEPORTS 체인
    │    -p tcp --dport 30080 -j KUBE-SVC-XXXXX
    │
    │ 3. externalTrafficPolicy=Cluster인 경우:
    │    └─ KUBE-MARK-MASQ (마스커레이드 마킹)
    │    └─ KUBE-SVC-XXXXX → 확률 선택 → KUBE-SEP-AAAA
    │       DNAT: dst → 10.244.2.3:8080
    │
    │ 4. POSTROUTING 체인
    │    KUBE-POSTROUTING: 마킹된 패킷 MASQUERADE
    │    src → 192.168.1.10 (Node A IP)
    │
    │    패킷: src=192.168.1.10  dst=10.244.2.3:8080
    │
    ▼
Node B의 Pod (10.244.2.3:8080)
    │
    │ 5. 응답
    │    src=10.244.2.3:8080  dst=192.168.1.10 (Node A로)
    │
    ▼
Node A에서 conntrack 역변환
    │    src=192.168.1.10:30080  dst=203.0.113.50 (역 SNAT)
    │    src 복원 후 → dst=203.0.113.50:port  src=192.168.1.10:30080
    │
    ▼
외부 클라이언트에 응답 도착
```

### 11.3 IPVS 모드에서의 차이

```
IPVS 모드에서는 DNAT이 커널 IPVS 모듈이 직접 처리:

1. 패킷 도착: dst=10.96.0.100:80
2. kube-ipvs0 인터페이스에서 수신 (VIP 바인딩)
3. IPVS 커널 모듈이 가상 서버 테이블에서 매칭
4. 선택된 알고리즘(rr, lc 등)으로 백엔드 선택
5. 커널 수준에서 DNAT + 포워딩
   → iptables 체인 순회 불필요 (더 빠름)
```

---

## 12. Why: 플랫 네트워크 모델

### 12.1 플랫 네트워크란

Kubernetes의 네트워킹 모델은 "모든 Pod가 고유한 IP를 가지고, NAT 없이 서로
직접 통신할 수 있어야 한다"는 단순한 규칙을 기반으로 한다.

### 12.2 왜 NAT 없는 플랫 네트워크인가

```
NAT 기반 네트워크의 문제점:
────────────────────────────
1. 포트 충돌: 여러 컨테이너가 같은 포트를 사용하려면 포트 매핑 필요
2. 서비스 디스커버리 복잡: 실제 IP:Port가 아닌 매핑된 주소 관리 필요
3. 디버깅 어려움: 패킷의 실제 소스/목적지 추적이 복잡
4. 기존 소프트웨어 호환성: 자신의 IP를 알 수 없거나 잘못 알게 됨

플랫 네트워크의 장점:
────────────────────
1. 단순성: Pod IP == 실제 통신 IP
2. 포트 자유: 모든 Pod가 원하는 포트를 사용 가능
3. 기존 앱 호환: 네트워크 인식 앱이 수정 없이 동작
4. 디버깅 용이: tcpdump, wireshark로 직접 추적 가능
5. 성능: NAT 오버헤드 없음 (Pod-to-Pod 직접 통신)
```

### 12.3 Service VIP가 필요한 이유

```
플랫 네트워크에서 Pod IP로 직접 통신하면?

문제 1: Pod는 임시적 (ephemeral)
  Pod이 재시작되면 IP가 변경됨
  → 클라이언트가 항상 최신 IP를 알아야 함

문제 2: 스케일링
  Pod이 3개에서 5개로 늘어나면?
  → 클라이언트가 새 Pod의 IP를 알아야 함

문제 3: 로드밸런싱
  여러 Pod에 트래픽을 분산하려면?
  → 클라이언트에 로드밸런서 로직이 필요

해결: Service (안정적 VIP + 자동 로드밸런싱)
  → 고정 IP (ClusterIP)
  → EndpointSlice로 자동 백엔드 관리
  → kube-proxy가 투명하게 DNAT 수행
```

### 12.4 kube-proxy가 userspace에 없는 이유

```
초기 kube-proxy는 userspace 프록시였다:

userspace 모드 (과거):
  패킷 → 커널 → kube-proxy 프로세스 → 커널 → 백엔드
  → context switch 2회 + 메모리 복사 2회
  → 레이턴시 높음, CPU 사용량 높음

iptables/IPVS 모드 (현재):
  패킷 → 커널(netfilter/IPVS에서 바로 DNAT) → 백엔드
  → context switch 0회
  → 순수 커널 수준 처리
  → 훨씬 낮은 레이턴시와 CPU 사용량
```

### 12.5 왜 Pod마다 고유 IP인가

```
대안 1: 호스트 네트워크 공유
  → 포트 충돌 문제 (두 Pod가 80번 포트를 못 씀)
  → Pod 간 네트워크 격리 불가

대안 2: NAT + 포트 매핑 (Docker 기본)
  → 복잡한 포트 관리
  → 자기 IP를 모름 (NAT 뒤)
  → 멀티호스트 통신 추가 복잡도

선택: Pod마다 고유 IP (네트워크 네임스페이스)
  → CNI가 IP 할당 및 라우팅 관리
  → Pod 내 컨테이너들은 localhost로 통신
  → Pod 간에는 고유 IP로 직접 통신
  → 깔끔한 추상화
```

---

## 13. 정리

### 13.1 네트워킹 전체 아키텍처

```
┌─────────────────────────────────────────────────────────────┐
│                   Kubernetes Networking                       │
│                                                              │
│  ┌─────────────────────────────────────────────────────┐    │
│  │                Control Plane                         │    │
│  │                                                      │    │
│  │  ┌─────────────────┐   ┌──────────────────────┐    │    │
│  │  │ EndpointSlice   │   │ Service Controller   │    │    │
│  │  │ Controller      │   │ (IP 할당)            │    │    │
│  │  │ (Pod→EP 매핑)   │   │                      │    │    │
│  │  └────────┬────────┘   └──────────┬───────────┘    │    │
│  │           │                       │                 │    │
│  │           └───────────┬───────────┘                 │    │
│  │                       │ API Server                  │    │
│  │                       ▼                             │    │
│  └───────────────────────┼─────────────────────────────┘    │
│                          │                                   │
│  ┌───────────────────────┼─────────────────────────────┐    │
│  │                Data Plane (각 노드)                  │    │
│  │                       │                              │    │
│  │  ┌──────────┐  ┌─────┴──────┐  ┌───────────────┐   │    │
│  │  │ CoreDNS  │  │ kube-proxy │  │ CNI Plugin    │   │    │
│  │  │          │  │            │  │               │   │    │
│  │  │ DNS 해석 │  │ iptables/  │  │ Pod 네트워크  │   │    │
│  │  │ Service  │  │ IPVS/      │  │ 설정          │   │    │
│  │  │ → IP     │  │ nftables   │  │ veth + route  │   │    │
│  │  └──────────┘  │            │  │               │   │    │
│  │                │ Service    │  │ Pod IP 할당   │   │    │
│  │                │ → Pod      │  │ 오버레이/     │   │    │
│  │                │ DNAT 규칙  │  │ 라우팅 설정   │   │    │
│  │                └────────────┘  └───────────────┘   │    │
│  └─────────────────────────────────────────────────────┘    │
└─────────────────────────────────────────────────────────────┘
```

### 13.2 핵심 교훈 요약

| 항목 | 핵심 내용 |
|------|-----------|
| 네트워크 모델 | Pod Network + Service Network + Node Network, 모든 Pod에 고유 IP |
| Service 타입 | ClusterIP(내부), NodePort(외부), LoadBalancer(LB), ExternalName(DNS) |
| kube-proxy | iptables/IPVS/nftables 모드, Service→Pod DNAT 규칙 관리 |
| iptables 모드 | 확률 기반 LB, O(n) 규칙 매칭, 기본 모드 |
| IPVS 모드 | 해시 테이블 O(1) 룩업, 다양한 LB 알고리즘, 대규모 클러스터용 |
| nftables 모드 | 원자적 규칙 교체, 맵/세트 기반, beta |
| EndpointSlice | 100개/slice로 분할, 증분 업데이트, 토폴로지 힌트 |
| DNS | CoreDNS가 Service→IP 해석, Headless Service는 Pod IP 직접 반환 |
| CNI | Pod 네트워크 설정 표준 인터페이스, 다양한 플러그인 지원 |
| 설계 원칙 | 플랫 네트워크, NAT 없는 Pod 통신, Service VIP로 안정적 접근 |

### 13.3 소스 코드 참조 요약

| 파일 | 핵심 함수/구조체 | 라인 |
|------|------------------|------|
| `cmd/kube-proxy/app/server.go` | `ProxyServer` | 161 |
| `cmd/kube-proxy/app/server.go` | `newProxyServer()` | 181 |
| `pkg/proxy/types.go` | `Provider` 인터페이스 | 28 |
| `pkg/proxy/types.go` | `ServicePortName` | 44 |
| `pkg/proxy/iptables/proxier.go` | `Proxier` (iptables) | 133 |
| `pkg/proxy/iptables/proxier.go` | iptables 체인 상수 (KUBE-SERVICES 등) | 53-87 |
| `pkg/proxy/iptables/proxier.go` | `syncProxyRules()` | 638 |
| `pkg/proxy/iptables/proxier.go` | `NewDualStackProxier()` | 93 |
| `pkg/proxy/iptables/proxier.go` | `largeClusterEndpointsThreshold` | 86 |
| `pkg/proxy/ipvs/proxier.go` | `Proxier` (IPVS) | 161 |
| `pkg/controller/endpointslice/endpointslice_controller.go` | `NewController()` | 85 |
| `pkg/controller/endpointslice/endpointslice_controller.go` | 상수 (maxRetries, ControllerName) | 54-82 |
