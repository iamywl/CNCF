# 25. Host Firewall + L2 Announcer + IP Masquerade 심층 분석

> Cilium 소스 기준: `bpf/bpf_host.c`, `pkg/l2announcer/`, `pkg/ipmasq/`
> 작성일: 2026-03-08

---

## 목차
1. [개요](#1-개요)
2. [Part A: Host Firewall](#2-part-a-host-firewall)
3. [bpf_host.c: 호스트 레벨 BPF 프로그램](#3-bpf_hostc-호스트-레벨-bpf-프로그램)
4. [호스트 정책 적용 흐름](#4-호스트-정책-적용-흐름)
5. [Part B: L2 Announcer](#5-part-b-l2-announcer)
6. [리더 선출과 서비스 선택](#6-리더-선출과-서비스-선택)
7. [정책-서비스 매칭 알고리즘](#7-정책-서비스-매칭-알고리즘)
8. [L2 Announce Table과 데이터패스 연동](#8-l2-announce-table과-데이터패스-연동)
9. [Part C: IP Masquerade Agent](#9-part-c-ip-masquerade-agent)
10. [설정 파일 감시와 BPF 맵 동기화](#10-설정-파일-감시와-bpf-맵-동기화)
11. [Non-Masquerade CIDR 관리](#11-non-masquerade-cidr-관리)
12. [설계 결정과 교훈](#12-설계-결정과-교훈)

---

## 1. 개요

이 문서는 Cilium의 **네트워크 확장 기능** 세 가지를 분석한다:

- **Host Firewall**: 호스트(노드) 자체에 대한 네트워크 정책 적용. Pod뿐 아니라 노드로 들어오는 트래픽도 제어
- **L2 Announcer**: LoadBalancer/ExternalIP 서비스의 IP를 L2(ARP/NDP)로 네트워크에 알려 트래픽 유인
- **IP Masquerade**: Pod에서 외부로 나가는 트래픽의 소스 IP를 노드 IP로 변환 (SNAT), 예외 CIDR 관리

### 왜 이 세 기능을 함께 분석하는가?

```
┌─────────────────────────────────────────────────────────────┐
│                 네트워크 확장 기능 관계도                       │
│                                                              │
│   외부 네트워크                                               │
│       │                                                      │
│       ▼ ARP "누가 10.0.0.100인가?"                            │
│   L2 Announcer ──→ "나야!" (리더 노드)                        │
│       │                                                      │
│       ▼ 트래픽 도착                                           │
│   Host Firewall ──→ 호스트 정책 검사 (BPF)                    │
│       │                                                      │
│       ▼ Pod → 외부 응답                                       │
│   IP Masquerade ──→ SNAT (Pod IP → Node IP)                  │
│       │                                                      │
│       ▼ 외부로 전송                                           │
└─────────────────────────────────────────────────────────────┘
```

세 기능은 **외부 트래픽의 수신(L2) → 필터링(Firewall) → 응답 시 NAT(Masquerade)**라는 흐름에서 연결된다.

### 소스 경로

```
bpf/bpf_host.c                     # Host Firewall BPF 프로그램

pkg/l2announcer/
├── l2announcer.go                  # L2 Announcer 컨트롤러 (1203줄)
└── l2announcer_test.go

pkg/ipmasq/
├── ipmasq.go                       # IP Masquerade Agent (294줄)
├── ipmasq_test.go
└── cell/                           # Hive cell 통합
```

---

## 2. Part A: Host Firewall

### Host Firewall란?

일반적인 Cilium 네트워크 정책은 **Pod** 단위로 적용된다. Host Firewall은 이를 확장하여 **노드(호스트) 자체**로 들어오거나 나가는 트래픽에도 정책을 적용한다.

```
┌───────────────────────────────────────────────────────────┐
│                Host Firewall 적용 범위                       │
│                                                            │
│  일반 NetworkPolicy (Pod 레벨):                             │
│  외부 → [eth0] → [BPF] → [veth] → Pod                     │
│                            ↑ 여기서 정책 검사               │
│                                                            │
│  Host Firewall (호스트 레벨):                               │
│  외부 → [eth0] → [BPF] → 호스트 프로세스 (kubelet, SSH 등)  │
│                    ↑ 여기서 정책 검사                        │
│                                                            │
│  보호 대상:                                                 │
│  - kubelet API (10250)                                     │
│  - SSH (22)                                                │
│  - etcd (2379/2380)                                        │
│  - 기타 노드 데몬                                           │
└───────────────────────────────────────────────────────────┘
```

### 왜 Host Firewall이 필요한가?

| 위협 | 설명 | Host Firewall 대응 |
|------|------|-------------------|
| kubelet 직접 접근 | Pod에서 kubelet API로 접근하여 노드 제어 | 호스트 포트 22, 10250 등 접근 제한 |
| 노드 서비스 공격 | 외부에서 노드의 etcd에 직접 접근 | 호스트 인바운드 정책으로 차단 |
| 노드 → 외부 유출 | 감염된 노드에서 C&C 서버로 통신 | 호스트 아웃바운드 정책 적용 |

---

## 3. bpf_host.c: 호스트 레벨 BPF 프로그램

### BPF 프로그램 연결 지점

```
┌──────────────────────────────────────────────────────────────┐
│              bpf_host.c 연결 지점                              │
│                                                               │
│  Ingress (외부 → 호스트):                                      │
│  ┌──────┐    ┌──────────────────────┐    ┌──────────────────┐ │
│  │ eth0 │──→│ cil_to_host          │──→│ 호스트 네트워크 스택│ │
│  │ (NIC)│    │ (bpf_host ingress)    │    │ (정책 허용 시)    │ │
│  └──────┘    └──────────────────────┘    └──────────────────┘ │
│                                                               │
│  Egress (호스트 → 외부):                                       │
│  ┌──────────────────┐    ┌──────────────────────┐    ┌──────┐ │
│  │ 호스트 프로세스   │──→│ cil_from_host          │──→│ eth0 │ │
│  │ (kubelet 등)     │    │ (bpf_host egress)      │    │ (NIC)│ │
│  └──────────────────┘    └──────────────────────┘    └──────┘ │
│                                                               │
│  Netdev (물리/가상 디바이스):                                    │
│  ┌──────┐    ┌──────────────────────┐                         │
│  │ eth0 │──→│ cil_from_netdev       │──→ Pod 또는 호스트       │
│  └──────┘    └──────────────────────┘                         │
└──────────────────────────────────────────────────────────────┘
```

### Host Firewall 정책 검사 흐름

```
패킷 도착 (eth0)
    │
    ▼
cil_from_netdev / cil_to_host
    │
    ├── 1. CT (Connection Tracking) 조회
    │   └── 기존 연결이면 → 허용
    │
    ├── 2. Policy Map 조회
    │   ├── ALLOW → 통과
    │   ├── DENY  → 드롭
    │   └── AUDIT → 로그 후 통과
    │
    ├── 3. Host Endpoint 정책
    │   └── CiliumClusterwideNetworkPolicy로 정의
    │
    └── 4. 결과: TC_ACT_OK (통과) 또는 TC_ACT_SHOT (드롭)
```

---

## 4. 호스트 정책 적용 흐름

### CiliumClusterwideNetworkPolicy 예시

```yaml
apiVersion: cilium.io/v2
kind: CiliumClusterwideNetworkPolicy
metadata:
  name: host-firewall
spec:
  nodeSelector:
    matchLabels:
      role: worker
  ingress:
  - fromEntities:
    - cluster    # 클러스터 내부에서만 허용
    toPorts:
    - ports:
      - port: "10250"    # kubelet
        protocol: TCP
  - fromEntities:
    - world      # 외부에서
    toPorts:
    - ports:
      - port: "22"       # SSH
        protocol: TCP
  egress:
  - toEntities:
    - all        # 모든 아웃바운드 허용
```

### Host Endpoint Identity

호스트는 특수한 Identity를 가진다:

| Identity | 값 | 의미 |
|----------|-----|------|
| `reserved:host` | 1 | 로컬 호스트 자체 |
| `reserved:world` | 2 | 클러스터 외부 |
| `reserved:unmanaged` | 3 | Cilium 관리 밖의 엔드포인트 |
| `reserved:kube-apiserver` | 7 | API 서버 |

---

## 5. Part B: L2 Announcer

### L2 Announcer란?

L2 Announcer는 Kubernetes LoadBalancer 또는 ExternalIP 서비스의 IP 주소를 **L2 네트워크에 알려(announce)** 트래픽을 올바른 노드로 유도한다. 이는 **MetalLB의 L2 모드**와 유사한 기능이다.

```
┌──────────────────────────────────────────────────────────────┐
│                  L2 Announcer 동작 원리                        │
│                                                               │
│  외부 클라이언트                                                │
│  "10.0.0.100에 연결하고 싶다"                                   │
│       │                                                       │
│       ▼ ARP Request: "Who has 10.0.0.100?"                    │
│                                                               │
│  ┌────────────┐  ┌────────────┐  ┌────────────┐              │
│  │ Node A     │  │ Node B     │  │ Node C     │              │
│  │ (리더)      │  │            │  │            │              │
│  │            │  │            │  │            │              │
│  │ ARP Reply: │  │ (응답 안함) │  │ (응답 안함) │              │
│  │ "나야!"    │  │            │  │            │              │
│  └────┬───────┘  └────────────┘  └────────────┘              │
│       │                                                       │
│       ▼ 트래픽이 Node A로 전송                                  │
│  Service 10.0.0.100 → Pod (kube-proxy가 라우팅)                │
└──────────────────────────────────────────────────────────────┘
```

### 왜 리더 선출이 필요한가?

L2 네트워크에서 **하나의 IP에 대해 하나의 노드만 ARP 응답**을 해야 한다. 여러 노드가 동시에 응답하면 ARP flapping이 발생하여 네트워크가 불안정해진다. 따라서 서비스별 리더를 선출하고, **리더만 해당 IP의 ARP에 응답**한다.

---

## 6. 리더 선출과 서비스 선택

### L2Announcer 구조체

```go
// 소스: pkg/l2announcer/l2announcer.go (87-105행)
type L2Announcer struct {
    params l2AnnouncerParams

    policyStore resource.Store[*cilium_api_v2alpha1.CiliumL2AnnouncementPolicy]
    localNode   *v2.CiliumNode

    scopedGroup job.ScopedGroup

    leaderChannel     chan leaderElectionEvent
    devicesUpdatedSig chan struct{}

    // 현재 노드에 매칭되는 정책
    selectedPolicies map[resource.Key]*selectedPolicy
    // 정책에 의해 선택된 서비스 (리더 선출 대상)
    selectedServices map[resource.Key]*selectedService
    // 정책과 매칭 가능한 네트워크 디바이스 목록
    devices []string
}
```

### Hive Cell 등록

```go
// 소스: pkg/l2announcer/l2announcer.go (46-53행)
var Cell = cell.Module(
    "l2-announcer",
    "L2 Announcer",

    cell.Provide(NewL2Announcer),
    cell.Provide(l2AnnouncementPolicyResource),
    cell.Invoke(func(*L2Announcer) {}),  // 시작 트리거
)
```

### 이벤트 루프

```go
// 소스: pkg/l2announcer/l2announcer.go (140-253행)
func (l2a *L2Announcer) run(ctx context.Context, health cell.Health) error {
    // 초기화: 서비스 테이블 변경 감시 시작
    svcChangeIter, _ := l2a.params.Services.Changes(wtxn)

    // 로컬 노드 정보 수신 대기
    for {
        event := <-localNodeChan
        l2a.processLocalNodeEvent(ctx, event)
        if l2a.localNode != nil { break }
    }

    // 메인 이벤트 루프
    for {
        // 서비스 변경 처리
        svcChanges, svcWatch := svcChangeIter.Next(txn)
        for event := range svcChanges {
            l2a.processSvcEvent(event)
        }

        select {
        case <-ctx.Done(): break
        case <-svcWatch:           // 서비스 변경
        case event := <-policyChan:  // 정책 변경
            l2a.processPolicyEvent(ctx, event)
        case event := <-localNodeChan:  // 노드 라벨 변경
            l2a.processLocalNodeEvent(ctx, event)
        case event := <-l2a.leaderChannel:  // 리더 선출 결과
            l2a.processLeaderEvent(event)
        case <-watchDevices:  // 디바이스 변경
            l2a.processDevicesChanged(ctx)
        }
    }
}
```

### Kubernetes Lease 기반 리더 선출

```go
// 소스: pkg/l2announcer/l2announcer.go (1137-1173행)
func (ss *selectedService) serviceLeaderElection(ctx context.Context,
    health cell.Health) error {
    defer close(ss.done)
    ss.ctx, ss.cancel = context.WithCancel(ctx)

    for {
        select {
        case <-ss.ctx.Done():
            return nil
        default:
            leaderelection.RunOrDie(ss.ctx, leaderelection.LeaderElectionConfig{
                Name:            ss.lock.LeaseMeta.Name,
                Lock:            ss.lock,
                ReleaseOnCancel: true,

                LeaseDuration: ss.leaseDuration,
                RenewDeadline: ss.renewDeadline,
                RetryPeriod:   ss.retryPeriod,

                Callbacks: leaderelection.LeaderCallbacks{
                    OnStartedLeading: func(ctx context.Context) {
                        ss.leaderChannel <- leaderElectionEvent{
                            typ:             leaderElectionLeading,
                            selectedService: ss,
                        }
                    },
                    OnStoppedLeading: func() {
                        ss.leaderChannel <- leaderElectionEvent{
                            typ:             leaderElectionStoppedLeading,
                            selectedService: ss,
                        }
                    },
                },
            })
        }
    }
}
```

### Lease 이름 규칙

```go
// 소스: pkg/l2announcer/l2announcer.go (820-827행)
const leasePrefix = "cilium-l2announce"

func (l2a *L2Announcer) newLeaseLock(svc *loadbalancer.Service) *resourcelock.LeaseLock {
    return &resourcelock.LeaseLock{
        LeaseMeta: metav1.ObjectMeta{
            Namespace: l2a.leaseNamespace(),
            Name: fmt.Sprintf("%s-%s-%s", leasePrefix,
                svc.Name.Namespace(), svc.Name.Name()),
        },
        LockConfig: resourcelock.ResourceLockConfig{
            Identity: l2a.localNode.Name,
        },
    }
}
// 예: cilium-l2announce-default-my-service
```

---

## 7. 정책-서비스 매칭 알고리즘

### 정책 Upsert 흐름

```go
// 소스: pkg/l2announcer/l2announcer.go (453-622행)
func (l2a *L2Announcer) upsertPolicy(ctx context.Context,
    policy *cilium_api_v2alpha1.CiliumL2AnnouncementPolicy) error {

    // 1. 노드 셀렉터 확인 — 이 노드에 적용되는 정책인가?
    if policy.Spec.NodeSelector != nil {
        nodeselector, _ := slim_meta_v1.LabelSelectorAsSelector(
            policy.Spec.NodeSelector)
        if !nodeselector.Matches(labels.Set(l2a.localNode.Labels)) {
            return l2a.delPolicy(key)  // 이 노드에 적용 안됨
        }
    }

    // 2. 인터페이스 정규식 매칭 — 어떤 디바이스에서 announce할 것인가?
    var selectedDevices []string
    if len(policy.Spec.Interfaces) == 0 {
        selectedDevices = l2a.devices  // 전체 디바이스
    } else {
        for _, strRegex := range policy.Spec.Interfaces {
            regex, _ := regexp.Compile(strRegex)
            for _, device := range l2a.devices {
                if regex.MatchString(device) {
                    selectedDevices = append(selectedDevices, device)
                }
            }
        }
    }

    // 3. 서비스 셀렉터 확인 — 어떤 서비스의 IP를 announce할 것인가?
    serviceSelector := labels.Everything()
    if policy.Spec.ServiceSelector != nil {
        serviceSelector, _ = slim_meta_v1.LabelSelectorAsSelector(
            policy.Spec.ServiceSelector)
    }

    // 4. 모든 서비스 스캔, 매칭되는 서비스에 정책 연결
    for svc := range l2a.params.Services.All(txn) {
        if serviceSelector.Matches(svcAndMetaLabels(svc)) {
            // IP 타입 매칭 확인
            if (policy.Spec.ExternalIPs && !noExternal) ||
                (policy.Spec.LoadBalancerIPs && !noLB) {
                l2a.addSelectedService(svc, extAddrs, lbAddrs, matchingPolicies)
            }
        }
    }
}
```

### 서비스 라벨 확장

```go
// 소스: pkg/l2announcer/l2announcer.go (1096-1110행)
const (
    serviceNamespaceLabel = "io.kubernetes.service.namespace"
    serviceNameLabel      = "io.kubernetes.service.name"
)

func svcAndMetaLabels(svc *loadbalancer.Service) labels.Set {
    labels := maps.Clone(svc.Labels.K8sStringMap())
    // 서비스 이름과 네임스페이스도 라벨로 추가
    labels[serviceNamespaceLabel] = svc.Name.Namespace()
    labels[serviceNameLabel] = svc.Name.Name()
    return labels
}
```

왜 메타 라벨을 추가하는가? Kubernetes 서비스의 이름과 네임스페이스를 셀렉터로 사용할 수 있게 하여, `io.kubernetes.service.name=my-service`로 특정 서비스를 정확히 지정할 수 있다.

---

## 8. L2 Announce Table과 데이터패스 연동

### L2 Entries Table 재계산

```go
// 소스: pkg/l2announcer/l2announcer.go (946-1058행)
func (l2a *L2Announcer) recalculateL2EntriesTableEntries(
    ss *selectedService) error {

    tbl := l2a.params.L2AnnounceTable
    txn := l2a.params.StateDB.WriteTxn(tbl)
    defer txn.Abort()

    // 리더가 아니면 → 모든 엔트리 제거
    if !ss.currentlyLeader {
        for e := range entriesIter {
            e = e.DeepCopy()
            // Origins에서 이 서비스 제거
            idx := slices.Index(e.Origins, svcKey)
            if idx != -1 {
                e.Origins = slices.Delete(e.Origins, idx, idx+1)
            }
            if len(e.Origins) == 0 {
                tbl.Delete(txn, e)  // 아무도 필요 없으면 삭제
            } else {
                tbl.Insert(txn, e)  // 다른 서비스가 필요하면 유지
            }
        }
        txn.Commit()
        return nil
    }

    // 리더이면 → 원하는 엔트리 계산 후 동기화
    desiredEntries := l2a.desiredEntries(ss)
    // ... 기존 엔트리와 비교하여 추가/삭제
    txn.Commit()
}
```

### 원하는 엔트리 계산

```go
// 소스: pkg/l2announcer/l2announcer.go (1060-1094행)
func (l2a *L2Announcer) desiredEntries(ss *selectedService) map[string]*tables.L2AnnounceEntry {
    entries := make(map[string]*tables.L2AnnounceEntry)

    for _, policyKey := range ss.byPolicies {
        selectedPolicy := l2a.selectedPolicies[policyKey]

        var IPs []netip.Addr
        if selectedPolicy.policy.Spec.LoadBalancerIPs {
            IPs = append(IPs, ss.lbAddresses...)
        }
        if selectedPolicy.policy.Spec.ExternalIPs {
            IPs = append(IPs, ss.externalAddresses...)
        }

        // IP x 디바이스 조합 생성
        for _, ip := range IPs {
            for _, iface := range selectedPolicy.selectedDevices {
                key := fmt.Sprintf("%s/%s", ip.String(), iface)
                entries[key] = &tables.L2AnnounceEntry{
                    L2AnnounceKey: tables.L2AnnounceKey{
                        IP:               ip,
                        NetworkInterface: iface,
                    },
                    Origins: []resource.Key{serviceKey(ss.svc)},
                }
            }
        }
    }
    return entries
}
```

### StateDB 기반 데이터패스 연동

```
┌──────────────────────────────────────────────────────┐
│           StateDB → BPF 맵 동기화                      │
│                                                       │
│  L2Announcer                                          │
│       │                                               │
│       ▼ Write                                         │
│  StateDB (L2AnnounceTable)                            │
│  ┌────────────────────────────────┐                   │
│  │ IP: 10.0.0.100, iface: eth0   │                   │
│  │ IP: 10.0.0.101, iface: eth0   │                   │
│  └──────────┬─────────────────────┘                   │
│             │ Watch                                    │
│             ▼                                          │
│  데이터패스 컨트롤러                                     │
│       │                                               │
│       ▼ ARP/NDP 응답 프로그래밍                          │
│  BPF 프로그램 (패킷 처리)                                │
└──────────────────────────────────────────────────────┘
```

---

## 9. Part C: IP Masquerade Agent

### IP Masquerade란?

IP Masquerade(SNAT)는 Pod에서 외부로 나가는 패킷의 **소스 IP를 노드 IP로 변환**하는 기능이다.

```
                        SNAT (Masquerade)
Pod (10.244.1.5) ──────────────────────→ 외부 (8.8.8.8)
  소스: 10.244.1.5                          소스: 192.168.1.10 (노드 IP)
  목적: 8.8.8.8                            목적: 8.8.8.8
```

### Non-Masquerade CIDR

특정 목적지는 Masquerade를 **적용하지 않아야** 한다 (Pod IP 유지):

```go
// 소스: pkg/ipmasq/ipmasq.go (27-43행)
var defaultNonMasqCIDRs = map[string]netip.Prefix{
    "10.0.0.0/8":      netip.MustParsePrefix("10.0.0.0/8"),
    "172.16.0.0/12":   netip.MustParsePrefix("172.16.0.0/12"),
    "192.168.0.0/16":  netip.MustParsePrefix("192.168.0.0/16"),
    "100.64.0.0/10":   netip.MustParsePrefix("100.64.0.0/10"),
    "192.0.0.0/24":    netip.MustParsePrefix("192.0.0.0/24"),
    "192.0.2.0/24":    netip.MustParsePrefix("192.0.2.0/24"),
    "192.88.99.0/24":  netip.MustParsePrefix("192.88.99.0/24"),
    "198.18.0.0/15":   netip.MustParsePrefix("198.18.0.0/15"),
    "198.51.100.0/24": netip.MustParsePrefix("198.51.100.0/24"),
    "203.0.113.0/24":  netip.MustParsePrefix("203.0.113.0/24"),
    "240.0.0.0/4":     netip.MustParsePrefix("240.0.0.0/4"),
}
```

왜 이 CIDR들을 제외하는가? RFC 예약 주소 (사설 네트워크, 문서용 등)로 가는 트래픽은 보통 **클러스터 내부 통신**이므로 SNAT하면 안 된다. kubernetes-sigs/ip-masq-agent와 동일한 기본 목록을 사용한다.

---

## 10. 설정 파일 감시와 BPF 맵 동기화

### IPMasqAgent 구조체

```go
// 소스: pkg/ipmasq/ipmasq.go (89-102행)
type IPMasqAgent struct {
    lock lock.Mutex

    logger                 *slog.Logger
    configPath             string
    masqLinkLocalIPv4      bool
    masqLinkLocalIPv6      bool
    nonMasqCIDRsFromConfig map[string]netip.Prefix  // 설정 파일에서 읽은 CIDR
    nonMasqCIDRsInMap      map[string]netip.Prefix  // BPF 맵에 현재 있는 CIDR
    ipMasqMap              IPMasqMap                 // BPF 맵 인터페이스
    watcher                *fsnotify.Watcher         // 파일 감시
    stop                   chan struct{}
    handlerFinished        chan struct{}
}
```

### 파일 감시 (fsnotify)

```go
// 소스: pkg/ipmasq/ipmasq.go (118-175행)
func (a *IPMasqAgent) Start() error {
    a.lock.Lock()
    defer a.lock.Unlock()

    watcher, _ := fsnotify.NewWatcher()
    a.watcher = watcher

    // 설정 파일의 디렉토리를 감시
    configDir := filepath.Dir(a.configPath)
    a.watcher.Add(configDir)

    // 초기 로드
    a.restore()  // BPF 맵에서 기존 CIDR 복원
    a.update()   // 설정 파일과 동기화

    go func() {
        for {
            select {
            case event := <-a.watcher.Events:
                switch {
                case event.Has(fsnotify.Create),
                     event.Has(fsnotify.Write),
                     event.Has(fsnotify.Chmod),
                     event.Has(fsnotify.Remove),
                     event.Has(fsnotify.Rename):
                    a.Update()  // 설정 변경 시 BPF 맵 업데이트
                }
            case err := <-a.watcher.Errors:
                a.logger.Warn("Watcher error", logfields.Error, err)
            case <-a.stop:
                close(a.handlerFinished)
                return
            }
        }
    }()

    return nil
}
```

---

## 11. Non-Masquerade CIDR 관리

### 설정 파일 형식

```yaml
# ip-masq-agent 설정 파일 (YAML)
nonMasqueradeCIDRs:
  - "10.0.0.0/8"
  - "172.16.0.0/12"
  - "192.168.0.0/16"
masqLinkLocal: false
masqLinkLocalIPv6: false
```

### BPF 맵 동기화 알고리즘

```go
// 소스: pkg/ipmasq/ipmasq.go (191-227행)
func (a *IPMasqAgent) update() error {
    // 1. 설정 파일 읽기
    isEmpty, _ := a.readConfig()

    // 2. 비어있으면 기본 CIDR 사용
    if isEmpty {
        maps.Copy(a.nonMasqCIDRsFromConfig, defaultNonMasqCIDRs)
    }

    // 3. Link-Local CIDR 처리
    if !a.masqLinkLocalIPv4 {
        a.nonMasqCIDRsFromConfig[linkLocalCIDRIPv4Str] = linkLocalCIDRIPv4
    }
    if !a.masqLinkLocalIPv6 {
        a.nonMasqCIDRsFromConfig[linkLocalCIDRIPv6Str] = linkLocalCIDRIPv6
    }

    // 4. 설정에 있고 맵에 없는 → 추가
    for cidrStr, cidr := range a.nonMasqCIDRsFromConfig {
        if _, ok := a.nonMasqCIDRsInMap[cidrStr]; !ok {
            a.ipMasqMap.Update(cidr)
            a.nonMasqCIDRsInMap[cidrStr] = cidr
        }
    }

    // 5. 맵에 있고 설정에 없는 → 삭제
    for cidrStr, cidr := range a.nonMasqCIDRsInMap {
        if _, ok := a.nonMasqCIDRsFromConfig[cidrStr]; !ok {
            a.ipMasqMap.Delete(cidr)
            delete(a.nonMasqCIDRsInMap, cidrStr)
        }
    }

    return nil
}
```

### 동기화 다이어그램

```
설정 파일                  BPF 맵
┌──────────────┐          ┌──────────────┐
│ 10.0.0.0/8   │          │ 10.0.0.0/8   │  → 유지
│ 172.16.0.0/12│          │ 172.16.0.0/12│  → 유지
│ 100.64.0.0/10│ (새로 추가)│              │  → Update()
│              │          │ 192.168.0.0/16│  → Delete() (설정에서 제거됨)
└──────────────┘          └──────────────┘

결과 BPF 맵:
┌──────────────┐
│ 10.0.0.0/8   │
│ 172.16.0.0/12│
│ 100.64.0.0/10│  ← 동기화 완료
└──────────────┘
```

---

## 12. 설계 결정과 교훈

### Host Firewall

| 설계 결정 | 이유 |
|-----------|------|
| BPF TC 훅 사용 | 호스트 네트워크 스택 진입 전 필터링, iptables보다 빠름 |
| 기존 CT/정책 맵 재사용 | Pod 정책과 동일한 인프라 사용, 중복 코드 없음 |
| CiliumClusterwideNetworkPolicy | 호스트 정책은 네임스페이스가 아닌 클러스터 범위 |

### L2 Announcer

| 설계 결정 | 이유 |
|-----------|------|
| Kubernetes Lease 기반 리더 선출 | K8s 네이티브, 추가 인프라 불필요, 장애 감지 내장 |
| 서비스별 독립 리더 선출 | 서비스 분산으로 단일 노드 과부하 방지 |
| StateDB 기반 데이터패스 연동 | 선언적 상태 관리, 리더 변경 시 자동 엔트리 정리 |
| Origins 배열로 다중 서비스 추적 | 같은 IP+iface를 여러 서비스가 사용할 수 있음 |
| 정규식 기반 인터페이스 매칭 | `^eth.*` 등 유연한 디바이스 지정 |
| Lease GC 타이머 | 비활성 서비스의 orphaned lease 자동 정리 |

### IP Masquerade

| 설계 결정 | 이유 |
|-----------|------|
| fsnotify 파일 감시 | ConfigMap 마운트 변경을 실시간 감지, 폴링 불필요 |
| BPF 맵 직접 조작 | iptables 대비 O(1) 룩업, 커널 공간에서 SNAT 결정 |
| 설정 ↔ 맵 차분 동기화 | 전체 교체 대신 추가/삭제만 수행, 최소 변경 원칙 |
| 기본 Non-Masq CIDR 목록 | k8s-sigs/ip-masq-agent와 호환, RFC 예약 범위 기본 제외 |
| restore() 시작 시 호출 | 에이전트 재시작 시 BPF 맵 상태를 인메모리로 복원 |

### 핵심 교훈

1. **서비스별 리더 선출의 확장성**: L2 Announcer에서 모든 서비스에 하나의 리더 대신, 서비스별로 독립적인 리더를 선출한다. 이로써 100개 서비스가 10개 노드에 균등 분산되어, 단일 장애점 없이 고가용성을 달성한다.

2. **StateDB의 선언적 상태 관리**: L2 Announce Table은 "현재 상태"를 선언적으로 기술한다. 리더가 바뀌면 테이블 재계산으로 자동으로 엔트리가 정리된다. 이벤트 기반으로 "추가/삭제 명령"을 보내는 방식보다 훨씬 안전하다.

3. **차분 동기화 (Diff-based Sync)**: IP Masquerade의 `update()`는 설정 파일과 BPF 맵의 차이만 계산하여 최소한의 변경을 수행한다. 이 패턴은 Kubernetes Reconciliation Loop와 동일한 원리다.

4. **fsnotify vs 폴링**: 파일 시스템 감시(inotify 기반)는 폴링 대비 즉각적인 반응과 낮은 CPU 사용을 제공한다. ConfigMap이 Pod에 마운트될 때 파일 시스템 이벤트가 발생하므로, Kubernetes 환경에서 자연스럽게 동작한다.
