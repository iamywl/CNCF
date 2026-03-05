# 14. IPAM (IP Address Management) 서브시스템

## 개요

IPAM(IP Address Management)은 Cilium에서 파드에 IP 주소를 할당하고 해제하는 핵심 서브시스템이다.
Kubernetes 클러스터의 모든 워크로드가 네트워크 통신을 하려면 고유한 IP 주소가 필요하며,
IPAM은 이 주소를 효율적으로 관리하는 책임을 진다.

Cilium IPAM의 특징은 **8가지 서로 다른 IPAM 모드**를 단일 인터페이스(`Allocator`) 뒤에 통합했다는 것이다.
온프레미스 Kubernetes 클러스터부터 AWS ENI, Azure, AlibabaCloud까지, 동일한 상위 레벨 API를 통해
IP 할당/해제가 이루어진다. 이 설계는 다음 두 가지 핵심 질문에 답한다:

1. **왜 모드가 8가지나 필요한가?** --- 클라우드 환경마다 네트워크 인프라가 근본적으로 다르기 때문이다.
   AWS ENI는 네트워크 인터페이스에 보조 IP를 부착하는 방식이고, Azure는 인터페이스별 CIDR 기반이며,
   온프레미스는 노드별 PodCIDR을 사용한다. 하나의 할당 알고리즘으로는 이 차이를 흡수할 수 없다.

2. **왜 단일 인터페이스로 통합하는가?** --- 상위 레이어(CNI 플러그인, 엔드포인트 관리)가
   IPAM 모드에 관계없이 동일한 `AllocateNext()`/`ReleaseIP()` 호출로 동작해야 하기 때문이다.

### IPAM 서브시스템 전체 구조

```
+------------------------------------------------------------------+
|                        IPAM 구조체 (types.go)                      |
|  +-----------+  +-----------+  +----------+  +-----------+        |
|  | ipv4      |  | ipv6      |  | owner    |  | expiration|        |
|  | Allocator |  | Allocator |  | map      |  | timers    |        |
|  +-----+-----+  +-----+-----+  +----------+  +-----------+        |
|        |              |                                            |
+--------|--------------|--------------------------------------------+
         |              |
    +----+----+---------+----------+--------------------+
    |         |                    |                    |
+---v---+ +---v-------+  +--------v-------+  +--------v-------+
| host  | | multiPool |  | crdAllocator   |  | delegated     |
| scope | | Allocator |  | (ENI/Azure/    |  | plugin        |
| alloc | |           |  |  AlibabaCloud) |  |               |
+---+---+ +-----+-----+  +--------+-------+  +---------------+
    |           |                  |
    v           v                  v
 ipallocator  cidrPool         nodeStore
   .Range     (pool.go)        (crd.go)
              여러 CIDR          CiliumNode
              범위 관리           CR 기반
```

## IPAM 모드 비교

Cilium은 `pkg/ipam/option/option.go`에 8가지 IPAM 모드를 정의한다.

```go
// pkg/ipam/option/option.go
const (
    IPAMKubernetes     = "kubernetes"       // 호스트 스코프 할당
    IPAMClusterPool    = "cluster-pool"     // 호스트 스코프 할당
    IPAMMultiPool      = "multi-pool"       // 다중 풀 할당
    IPAMCRD            = "crd"              // CRD 기반 수동 할당
    IPAMENI            = "eni"              // AWS ENI 할당
    IPAMAzure          = "azure"            // Azure 할당
    IPAMAlibabaCloud   = "alibabacloud"     // AlibabaCloud 할당
    IPAMDelegatedPlugin = "delegated-plugin" // CNI 위임 플러그인
)
```

### 모드별 비교 테이블

| 모드 | 할당 방식 | IP 소스 | 사용 환경 | Allocator 구현체 |
|------|----------|---------|----------|-----------------|
| `kubernetes` | 노드별 PodCIDR에서 호스트 로컬 할당 | kube-controller-manager가 할당한 PodCIDR | 온프레미스, 기본 K8s | `hostScopeAllocator` |
| `cluster-pool` | 노드별 PodCIDR에서 호스트 로컬 할당 | cilium-operator가 할당한 PodCIDR | 온프레미스, 클라우드 | `hostScopeAllocator` |
| `multi-pool` | 풀별로 다수 CIDR에서 할당 | cilium-operator가 풀별로 CIDR 할당 | 멀티테넌트, 복잡한 네트워크 | `multiPoolAllocator` |
| `crd` | CiliumNode CR의 IP Pool에서 할당 | 외부 시스템이 CiliumNode에 IP 추가 | 수동 관리, 커스텀 IPAM | `crdAllocator` |
| `eni` | AWS ENI 보조 IP로 할당 | AWS ENI API | AWS EKS | `crdAllocator` |
| `azure` | Azure 인터페이스 IP로 할당 | Azure API | Azure AKS | `crdAllocator` |
| `alibabacloud` | AlibabaCloud ENI IP로 할당 | AlibabaCloud API | AlibabaCloud | `crdAllocator` |
| `delegated-plugin` | 외부 CNI 플러그인에 위임 | 외부 IPAM 플러그인 | 커스텀 CNI | N/A (외부 위임) |

### 왜 kubernetes와 cluster-pool이 분리되어 있는가?

두 모드 모두 동일한 `hostScopeAllocator`를 사용하지만 **PodCIDR의 출처**가 다르다.
`kubernetes` 모드는 Kubernetes의 기본 `kube-controller-manager`가 할당한 PodCIDR을 사용하고,
`cluster-pool` 모드는 `cilium-operator`가 `CiliumNode` CR을 통해 PodCIDR을 할당한다.
`cluster-pool`은 Cilium이 K8s 기본 IPAM에 의존하지 않고 자체적으로 CIDR를 관리할 수 있게 한다.

이 분기는 `ipam.go`의 `ConfigureAllocator()`에서 확인할 수 있다:

```go
// pkg/ipam/ipam.go (ConfigureAllocator, 라인 124-139)
func (ipam *IPAM) ConfigureAllocator() {
    switch ipam.config.IPAMMode() {
    case ipamOption.IPAMKubernetes, ipamOption.IPAMClusterPool:
        // 두 모드 모두 동일한 hostScopeAllocator 사용
        if ipam.config.IPv6Enabled() {
            ipam.ipv6Allocator = newHostScopeAllocator(
                ipam.nodeAddressing.IPv6().AllocationCIDR().IPNet)
        }
        if ipam.config.IPv4Enabled() {
            ipam.ipv4Allocator = newHostScopeAllocator(
                ipam.nodeAddressing.IPv4().AllocationCIDR().IPNet)
        }
    case ipamOption.IPAMMultiPool:
        // multiPoolAllocator 생성
        ...
```

## 핵심 인터페이스

### Allocator 인터페이스

모든 IPAM 모드의 핵심 추상화는 `Allocator` 인터페이스이다.
이 인터페이스가 IPAM 서브시스템의 Strategy Pattern을 구현하는 근간이다.

```go
// pkg/ipam/types.go (라인 66-96)
type Allocator interface {
    // 특정 IP 할당 (복원 시 사용)
    Allocate(ip net.IP, owner string, pool Pool) (*AllocationResult, error)

    // 특정 IP 할당 (upstream 동기화 없이)
    AllocateWithoutSyncUpstream(ip net.IP, owner string, pool Pool) (*AllocationResult, error)

    // IP 해제
    Release(ip net.IP, pool Pool) error

    // 다음 가용 IP 할당
    AllocateNext(owner string, pool Pool) (*AllocationResult, error)

    // 다음 가용 IP 할당 (upstream 동기화 없이)
    AllocateNextWithoutSyncUpstream(owner string, pool Pool) (*AllocationResult, error)

    // 전체 할당 현황 덤프
    Dump() (map[Pool]map[string]string, string)

    // 할당 가능 총 용량
    Capacity() uint64

    // 복원 완료 마킹
    RestoreFinished()
}
```

**왜 `WithoutSyncUpstream` 변종이 존재하는가?**

Cilium 에이전트가 재시작할 때, 이전에 할당했던 IP를 복원(restore)해야 한다.
이 복원 과정에서는 이미 CiliumNode CR에 반영된 상태를 다시 기록할 필요가 없으므로,
upstream 동기화를 건너뛰는 별도 메서드가 필요하다. 이렇게 분리함으로써
재시작 시 불필요한 API 서버 호출을 방지하고, 복원 과정의 성능을 최적화한다.

### AllocationResult 구조체

할당 결과는 단순한 IP 주소 이상의 정보를 포함한다.

```go
// pkg/ipam/types.go (라인 29-63)
type AllocationResult struct {
    IP              net.IP   // 할당된 IP
    IPPoolName      Pool     // 할당 출처 풀 이름
    CIDRs           []string // IP가 라우팅 가능한 CIDR 목록
    PrimaryMAC      string   // 주 인터페이스 MAC (ENI 모드)
    GatewayIP       string   // 게이트웨이 IP
    ExpirationUUID  string   // 만료 타이머 UUID
    InterfaceNumber string   // 인터페이스 번호 (ENI 모드)
    SkipMasquerade  bool     // 마스커레이드 건너뛰기 여부
}
```

**왜 AllocationResult에 MAC, Gateway 등 추가 정보가 포함되는가?**

AWS ENI 모드에서는 파드 IP가 특정 ENI(Elastic Network Interface)에 바인딩된다.
이 경우 CNI 플러그인이 올바른 라우팅 규칙을 설정하려면 어떤 ENI에서 할당되었는지,
해당 ENI의 MAC 주소와 게이트웨이 IP가 무엇인지 알아야 한다.
단순히 IP만 반환하면 라우팅 설정이 불가능하기 때문에 이 정보를 함께 포함한다.

### IPAM 구조체

최상위 IPAM 구조체는 IPv4/IPv6 할당기를 소유하고 메트릭, 만료 타이머, 소유자 추적을 관리한다.

```go
// pkg/ipam/types.go (라인 98-141)
type IPAM struct {
    logger         *slog.Logger
    nodeAddressing types.NodeAddressing
    config         *option.DaemonConfig
    ipv6Allocator  Allocator       // IPv6 할당기
    ipv4Allocator  Allocator       // IPv4 할당기
    metadata       Metadata        // 파드 → 풀 매핑 정보
    owner          map[Pool]map[string]string  // IP 소유자 추적
    expirationTimers map[timerKey]expirationTimer // 만료 타이머
    allocatorMutex lock.RWMutex    // 동시성 보호
    excludedIPs    map[string]string            // 제외 IP 목록
    localNodeStore *node.LocalNodeStore
    ...
}
```

### Owner 인터페이스

```go
// pkg/ipam/ipam.go (라인 44-50)
type Owner interface {
    // CiliumNode 리소스를 생성/업데이트
    // 커스텀 리소스가 생성될 때까지 블로킹
    UpdateCiliumNodeResource()
}
```

## Cluster Pool IPAM

Cluster Pool 모드는 Cilium의 **기본 IPAM 모드**이다.
`cilium-operator`가 각 노드에 PodCIDR을 할당하고, 에이전트는 해당 CIDR 내에서
로컬로 IP를 할당한다.

### 아키텍처

```
                     cilium-operator
                          |
                    CiliumNode CR에
                    PodCIDR 할당
                          |
                          v
+--------------------------------------------------+
|                    cilium-agent                    |
|                                                    |
|  ConfigureAllocator()                             |
|       |                                            |
|       v                                            |
|  newHostScopeAllocator(IPv4 AllocationCIDR)       |
|  newHostScopeAllocator(IPv6 AllocationCIDR)       |
|       |                                            |
|       v                                            |
|  +------------------------------------------+     |
|  |         hostScopeAllocator                |     |
|  |  +------------------------------------+  |     |
|  |  |  allocCIDR: 10.244.1.0/24         |  |     |
|  |  |  allocator: ipallocator.Range      |  |     |
|  |  |           (비트맵 기반 할당)        |  |     |
|  |  +------------------------------------+  |     |
|  +------------------------------------------+     |
+--------------------------------------------------+
```

### hostScopeAllocator 구현

`hostScopeAllocator`는 IPAM에서 가장 단순한 할당기 구현이다.
하나의 CIDR을 `ipallocator.Range`에 위임하여 비트맵 기반으로 IP를 할당한다.

```go
// pkg/ipam/hostscope.go (라인 15-25)
type hostScopeAllocator struct {
    allocCIDR *net.IPNet           // 할당 대상 CIDR
    allocator *ipallocator.Range   // 실제 할당 엔진
}

func newHostScopeAllocator(n *net.IPNet) Allocator {
    return &hostScopeAllocator{
        allocCIDR: n,
        allocator: ipallocator.NewCIDRRange(n),
    }
}
```

`AllocateNext` 구현은 하위 `ipallocator.Range`에 직접 위임한다:

```go
// pkg/ipam/hostscope.go (라인 48-55)
func (h *hostScopeAllocator) AllocateNext(owner string, pool Pool) (*AllocationResult, error) {
    ip, err := h.allocator.AllocateNext()
    if err != nil {
        return nil, err
    }
    return &AllocationResult{IP: ip}, nil
}
```

**핵심 특성:**
- Pool 개념 없음 --- 모든 할당은 `PoolDefault()`로 반환
- 소유자(owner) 추적 없음 --- 상위 `IPAM` 구조체의 `owner` 맵에서 관리
- `SyncUpstream` 구분 없음 --- 두 메서드가 동일 동작 (로컬 할당이므로)

### Dump 메서드의 비트맵 해석

```go
// pkg/ipam/hostscope.go (라인 66-90)
func (h *hostScopeAllocator) Dump() (map[Pool]map[string]string, string) {
    var origIP *big.Int
    alloc := map[string]string{}
    _, data, err := h.allocator.Snapshot()
    // ...
    origIP = big.NewInt(0).SetBytes(h.allocCIDR.IP.To4())
    bits := big.NewInt(0).SetBytes(data)
    for i := range bits.BitLen() {
        if bits.Bit(i) != 0 {
            ip := net.IP(big.NewInt(0).Add(origIP,
                big.NewInt(int64(uint(i+1)))).Bytes()).String()
            alloc[ip] = ""
        }
    }
    // ...
}
```

`Snapshot()`으로 비트맵 데이터를 가져온 뒤, 각 비트가 1인 위치를 IP 주소로 변환한다.
비트 인덱스 `i`에 대해 `baseIP + (i+1)`이 할당된 IP가 된다.

## Multi-Pool IPAM

Multi-Pool 모드는 Cilium 1.14에서 도입된 모드로, **하나의 노드에 여러 IP 풀**을
동시에 운용할 수 있다. 서로 다른 파드 그룹이 서로 다른 IP 대역을 사용해야 하는
멀티테넌트 환경에서 필수적이다.

### 왜 Multi-Pool이 필요한가?

기존 `cluster-pool` 모드에서는 노드당 하나의 PodCIDR만 사용한다.
그러나 다음과 같은 시나리오에서는 이것으로 충분하지 않다:

1. **멀티테넌트 격리**: 테넌트 A의 파드는 10.0.0.0/16, 테넌트 B는 172.16.0.0/16 대역 사용
2. **외부 연동**: 특정 파드 그룹은 외부 시스템과 직접 통신 가능한 대역 필요
3. **마스커레이드 분리**: 일부 풀은 마스커레이드를 건너뛰어야 함

### 아키텍처

```
                    CiliumPodIPPool CR
                    (pool 정의: CIDR, 속성)
                          |
                    cilium-operator
                    (풀별 CIDR 할당)
                          |
                    CiliumNode CR
                    (spec.ipam.pools)
                          |
                          v
+----------------------------------------------------------+
|                    cilium-agent                            |
|                                                           |
|  multiPoolAllocator (IPv4)  multiPoolAllocator (IPv6)    |
|       |                          |                        |
|       +----------+---------------+                        |
|                  |                                         |
|                  v                                         |
|       +--------------------+                              |
|       | multiPoolManager   |                              |
|       |                    |                              |
|       | pools: map[Pool]*poolPair                         |
|       |   "default" -> poolPair{v4: cidrPool, v6: cidrPool}|
|       |   "tenant-a" -> poolPair{v4: cidrPool, v6: nil}  |
|       |   "external" -> poolPair{v4: cidrPool, v6: nil}  |
|       +--------------------+                              |
|                  |                                         |
|                  v                                         |
|       +--------------------+                              |
|       |    cidrPool        |  (pool.go)                   |
|       |  ipAllocators:     |                              |
|       |    []*ipallocator  |                              |
|       |    .Range          |  10.0.1.0/24, 10.0.2.0/24   |
|       |  released: set     |  (해제된 CIDR 추적)           |
|       |  removed: set      |  (제거된 CIDR 추적)           |
|       +--------------------+                              |
+----------------------------------------------------------+
```

### MultiPoolAllocatorParams

```go
// pkg/ipam/multipool.go (라인 34-50)
type MultiPoolAllocatorParams struct {
    Logger                    *slog.Logger
    IPv4Enabled               bool
    IPv6Enabled               bool
    CiliumNodeUpdateRate      time.Duration
    PreAllocPools             map[string]string  // 사전 할당 풀 설정
    Node                      agentK8s.LocalCiliumNodeResource
    LocalNodeStore            *node.LocalNodeStore
    CNClient                  cilium_v2.CiliumNodeInterface
    JobGroup                  job.Group
    DB                        *statedb.DB
    PodIPPools                statedb.Table[podippool.LocalPodIPPool]
    OnlyMasqueradeDefaultPool bool
}
```

### multiPoolAllocator

`multiPoolAllocator`는 `Allocator` 인터페이스를 구현하되,
실제 로직은 `multiPoolManager`에 위임한다. IPv4/IPv6 각각 별도의 인스턴스가 생성된다.

```go
// pkg/ipam/multipool.go (라인 52-56)
type multiPoolAllocator struct {
    manager *multiPoolManager
    family  Family
}

// pkg/ipam/multipool.go (라인 57-92)
func newMultiPoolAllocators(p MultiPoolAllocatorParams) (Allocator, Allocator) {
    // preallocMap 파싱
    // multiPoolManager 생성
    mgr := newMultiPoolManager(...)

    // StateDB에서 모든 풀이 준비될 때까지 대기
    waitForAllPools(p.Logger, p.DB, p.PodIPPools, preallocMap)

    // 로컬 노드의 AllocCIDRs 동기화 시작
    startLocalNodeAllocCIDRsSync(...)

    return &multiPoolAllocator{manager: mgr, family: IPv4},
           &multiPoolAllocator{manager: mgr, family: IPv6}
}
```

### multiPoolManager

```go
// pkg/ipam/multipool_manager.go (라인 241-267)
type multiPoolManager struct {
    ipv4Enabled bool
    ipv6Enabled bool

    preallocatedIPsPerPool preAllocatePerPool     // 풀별 사전 할당 수
    pendingIPsPerPool      *pendingAllocationsPerPool // 대기 중 할당

    poolsMutex      lock.Mutex
    pools           map[Pool]*poolPair     // 풀 이름 → (IPv4 cidrPool, IPv6 cidrPool)
    poolsUpdated    chan struct{}           // 풀 업데이트 알림 채널
    finishedRestore map[Family]bool        // 복원 완료 상태

    nodeMutex  lock.Mutex
    node       *ciliumv2.CiliumNode       // 로컬 CiliumNode 리소스

    jobGroup   job.Group
    k8sUpdater job.Trigger                // K8s 업데이트 트리거
    cnClient   cilium_v2.CiliumNodeInterface

    localNodeUpdate   chan struct{}
    poolsFromResource ciliumv2.PoolsFromResourceFunc
    skipMasqueradeForPool SkipMasqueradeForPoolFn
}
```

### 마스커레이드 건너뛰기 로직

Multi-Pool에서는 특정 풀의 IP에 대해 마스커레이드를 건너뛰는 기능이 있다.

```go
// pkg/ipam/multipool.go (라인 126-142)
func shouldSkipMasqForPool(db *statedb.DB, podIPPools statedb.Table[...],
    onlyMasqueradeDefaultPool bool) SkipMasqueradeForPoolFn {
    return func(pool Pool) (bool, error) {
        // 플래그가 설정되면 기본 풀 외 모든 풀에서 마스커레이드 건너뜀
        if onlyMasqueradeDefaultPool && pool != PoolDefault() {
            return true, nil
        }
        // StateDB에서 풀 조회 후 어노테이션 확인
        podIPPool, _, found := podIPPools.Get(db.ReadTxn(),
            podippool.ByName(string(pool)))
        if v, ok := podIPPool.Annotations[annotation.IPAMSkipMasquerade]; ok && v == "true" {
            return true, nil
        }
        return false, nil
    }
}
```

## CIDR Set 할당 알고리즘

`cidrset.CidrSet`은 Cilium Operator에서 노드에 PodCIDR을 할당할 때 사용하는
**비트맵 기반 CIDR 할당기**이다. Kubernetes의 CIDR 할당 알고리즘을 기반으로 하며,
클러스터 CIDR을 고정 크기 서브넷으로 분할하여 관리한다.

### CidrSet 구조체

```go
// pkg/ipam/cidrset/cidr_set.go (라인 23-43)
type CidrSet struct {
    lock.Mutex
    clusterCIDR     *net.IPNet  // 클러스터 전체 CIDR (예: 10.0.0.0/16)
    clusterMaskSize int         // 클러스터 마스크 크기 (예: 16)
    nodeMask        net.IPMask  // 노드별 마스크 (예: /24)
    nodeMaskSize    int         // 노드 마스크 크기 (예: 24)
    maxCIDRs        int         // 최대 할당 가능 CIDR 수
    allocatedCIDRs  int         // 현재 할당된 CIDR 수
    nextCandidate   int         // 다음 할당 후보 인덱스
    used            big.Int     // 비트맵: 할당 상태 추적
}
```

### 비트맵 할당 알고리즘 상세

클러스터 CIDR이 `10.0.0.0/16`이고 노드 마스크가 `/24`인 경우:

```
클러스터 CIDR: 10.0.0.0/16
노드 마스크:   /24
maxCIDRs:     2^(24-16) = 256개

비트맵 (big.Int used):
+---+---+---+---+---+---+---+---+---+---+
| 0 | 1 | 2 | 3 | 4 | 5 | 6 | 7 |...|255|
+---+---+---+---+---+---+---+---+---+---+
| 1 | 1 | 0 | 1 | 0 | 0 | 0 | 0 |...| 0 |   (1=할당됨, 0=미할당)
+---+---+---+---+---+---+---+---+---+---+
  |   |       |
  |   |       +-- 10.0.3.0/24 (노드 C)
  |   +---------- 10.0.1.0/24 (노드 B)
  +-------------- 10.0.0.0/24 (노드 A)

nextCandidate: 2  (다음에 검사할 인덱스)
```

### AllocateNext 알고리즘

```go
// pkg/ipam/cidrset/cidr_set.go (라인 161-181)
func (s *CidrSet) AllocateNext() (*net.IPNet, error) {
    s.Lock()
    defer s.Unlock()

    if s.allocatedCIDRs == s.maxCIDRs {
        return nil, ErrCIDRRangeNoCIDRsRemaining
    }

    // nextCandidate부터 순환 탐색
    candidate := s.nextCandidate
    for range s.maxCIDRs {
        if s.used.Bit(candidate) == 0 {  // 미할당 슬롯 발견
            break
        }
        candidate = (candidate + 1) % s.maxCIDRs  // 순환
    }

    // 할당 마킹
    s.nextCandidate = (candidate + 1) % s.maxCIDRs
    s.used.SetBit(&s.used, candidate, 1)
    s.allocatedCIDRs++

    return s.indexToCIDRBlock(candidate), nil
}
```

**왜 순환 탐색(circular scan)을 사용하는가?**

`nextCandidate`를 유지하면서 순환 탐색하면, 최근 해제된 CIDR이 바로 재사용되지 않는다.
이는 IP 주소 재사용으로 인한 라우팅 충돌을 줄이는 효과가 있다.
또한 `big.Int.Bit()` 연산은 O(1)이므로 비트맵 탐색이 효율적이다.

### Occupy와 Release

```go
// pkg/ipam/cidrset/cidr_set.go (라인 281-298)
func (s *CidrSet) Occupy(cidr *net.IPNet) (err error) {
    begin, end, err := s.getBeginningAndEndIndices(cidr)
    // ...
    for i := begin; i <= end; i++ {
        if s.used.Bit(i) == 0 {
            s.used.SetBit(&s.used, i, 1)  // 이중 카운팅 방지
            s.allocatedCIDRs++
        }
    }
    return nil
}

// pkg/ipam/cidrset/cidr_set.go (라인 261-277)
func (s *CidrSet) Release(cidr *net.IPNet) error {
    begin, end, err := s.getBeginningAndEndIndices(cidr)
    // ...
    for i := begin; i <= end; i++ {
        if s.used.Bit(i) != 0 {
            s.used.SetBit(&s.used, i, 0)  // 이중 카운팅 방지
            s.allocatedCIDRs--
        }
    }
    return nil
}
```

**이중 카운팅 방지**: `Occupy`와 `Release` 모두 비트 변경 전에 현재 상태를 확인한다.
이미 할당된 것을 다시 할당하거나, 이미 해제된 것을 다시 해제하면 카운터가 틀어지므로
반드시 상태 변경이 발생할 때만 `allocatedCIDRs`를 조정한다.

### 인덱스를 CIDR로 변환

```go
// pkg/ipam/cidrset/cidr_set.go (라인 105-150)
func (s *CidrSet) indexToCIDRBlock(index int) *net.IPNet {
    // IPv4 예시:
    // clusterCIDR = 10.0.0.0/16, nodeMaskSize = 24
    // index = 5 → j = 5 << (32-24) = 5 << 8 = 1280
    // ipInt = 0x0A000000 | 0x00000500 = 0x0A000500 → 10.0.5.0
    j := uint32(index) << uint32(32-s.nodeMaskSize)
    ipInt := (binary.BigEndian.Uint32(s.clusterCIDR.IP)) | j
    ip = make([]byte, net.IPv4len)
    binary.BigEndian.PutUint32(ip, ipInt)
    // ...
    return &net.IPNet{IP: ip, Mask: s.nodeMask}
}
```

## CRD 기반 IPAM

CRD 기반 IPAM은 `eni`, `azure`, `alibabacloud`, `crd` 모드에서 사용된다.
이 모드들은 `CiliumNode` Custom Resource에 **IP Pool**을 정의하고,
에이전트가 이 풀에서 IP를 할당하는 방식이다.

### nodeStore 구조체

CRD 기반 IPAM의 핵심은 `nodeStore`로, 로컬 CiliumNode CR의 상태를 추적한다.

```go
// pkg/ipam/crd.go (라인 57-85)
type nodeStore struct {
    logger *slog.Logger
    mutex  lock.RWMutex

    ownNode          *ciliumv2.CiliumNode  // 로컬 노드의 CiliumNode CR
    allocators       []*crdAllocator       // 바인딩된 할당기들
    refreshTrigger   *trigger.Trigger      // API 서버 동기화 트리거
    allocationPoolSize map[Family]int      // 주소 패밀리별 풀 크기
    restoreFinished  chan struct{}          // 복원 완료 시그널
    restoreCloseOnce sync.Once

    clientset client.Clientset
    conf      *option.DaemonConfig
    mtuConfig MtuConfiguration
    sysctl    sysctl.Sysctl
}
```

### crdAllocator 구현

```go
// pkg/ipam/crd.go (라인 714-731)
type crdAllocator struct {
    store     *nodeStore            // 노드 저장소 (공유)
    mutex     lock.RWMutex
    allocated ipamTypes.AllocationMap // 할당된 IP 맵
    family    Family                 // IPv4 또는 IPv6
    conf      *option.DaemonConfig
    logger    *slog.Logger
    ipMasqAgent *ipmasq.IPMasqAgent
}
```

### 할당 흐름 (CRD 모드)

```
파드 생성 요청
     |
     v
IPAM.AllocateNext()
     |
     v
crdAllocator.AllocateNext()
     |
     +---> nodeStore.allocateNext(allocated, family, owner)
     |          |
     |          v
     |     CiliumNode.Spec.IPAM.Pool에서
     |     미할당 IP 탐색
     |     (이미 allocated에 있는 IP 제외)
     |     (릴리즈 핸드셰이크 중인 IP 제외)
     |          |
     |          v
     |     IP + AllocationIP 반환
     |
     v
crdAllocator.buildAllocationResult(ip, ipInfo)
     |
     +---> ENI 모드: ENI ID → MAC, VPC CIDRs, GatewayIP 도출
     +---> Azure 모드: Interface ID → MAC, CIDR, GatewayIP 도출
     +---> CRD 모드: 기본 AllocationResult 반환
     |
     v
markAllocated(ip, owner, ipInfo)
     |
     v
refreshTrigger.TriggerWithReason("allocation of IP ...")
     |
     v
CiliumNode.Status.IPAM.Used 업데이트 → API 서버
```

### IP 릴리즈 핸드셰이크

CRD 기반 IPAM에서는 IP 해제가 에이전트와 오퍼레이터 사이의 **핸드셰이크** 프로토콜을 따른다.

```
IP 릴리즈 핸드셰이크 상태 전이도:

오퍼레이터                        에이전트
    |                                |
    | (1) marked-for-release         |
    |------------------------------->|
    |                                |
    |    (2a) IP 사용 중             |
    |    do-not-release              |
    |<-------------------------------|
    |                                |
    |    (2b) IP 미사용              |
    |    ready-for-release           |
    |<-------------------------------|
    |                                |
    | (3) released                   |
    | (Pool에서 IP 제거)             |
    |------------------------------->|
    |                                |
    | (4) 에이전트가 release-ips에서 |
    |    해당 항목 삭제              |
    |<-------------------------------|
```

이 핸드셰이크의 상태 코드는 `pkg/ipam/option/option.go`에 정의되어 있다:

```go
// pkg/ipam/option/option.go (라인 38-43)
const (
    IPAMMarkForRelease  = "marked-for-release"   // 오퍼레이터가 릴리즈 요청
    IPAMReadyForRelease = "ready-for-release"     // 에이전트가 릴리즈 승인
    IPAMDoNotRelease    = "do-not-release"        // 에이전트가 릴리즈 거부
    IPAMReleased        = "released"              // 오퍼레이터가 실제 릴리즈 완료
)
```

**왜 핸드셰이크가 필요한가?**

오퍼레이터가 일방적으로 IP를 회수하면, 아직 그 IP를 사용 중인 파드의 네트워크가
끊어질 수 있다. 에이전트만이 특정 IP가 실제로 사용 중인지 알 수 있으므로,
에이전트의 확인(ACK/NACK)을 거친 후에만 릴리즈를 진행한다.

### CiliumNode CR 감시

```go
// pkg/ipam/crd.go (newNodeStore, 라인 89-233)
func newNodeStore(logger *slog.Logger, nodeName string, ...) *nodeStore {
    // ...
    // CiliumNode 인포머 설정 (자기 노드만 감시)
    ciliumNodeSelector := fields.ParseSelectorOrDie("metadata.name=" + nodeName)
    _, ciliumNodeInformer := informer.NewInformer(
        // ...
        cache.ResourceEventHandlerFuncs{
            AddFunc:    func(obj any) { store.updateLocalNodeResource(node) },
            UpdateFunc: func(old, new any) { store.updateLocalNodeResource(newNode) },
            DeleteFunc: func(obj any) { store.deleteLocalNodeResource() },
        },
    )

    go ciliumNodeInformer.Run(wait.NeverStop)
    // 캐시 동기화 대기
    cache.WaitForCacheSync(wait.NeverStop, ciliumNodeInformer.HasSynced)

    // 최소 IP 확보 대기
    for {
        minimumReached, _, _ := store.hasMinimumIPsInPool(localNodeStore)
        if minimumReached { break }
        time.Sleep(5 * time.Second)
    }
    // ...
}
```

## cidrPool (pool.go): 다중 CIDR 풀 관리

`cidrPool`은 Multi-Pool IPAM에서 **하나의 풀(Pool)에 여러 CIDR**을 관리하는 구조체이다.
오퍼레이터가 필요에 따라 CIDR을 추가/제거할 수 있으며, 각 CIDR은
독립적인 `ipallocator.Range`를 가진다.

```go
// pkg/ipam/pool.go (라인 36-42)
type cidrPool struct {
    logger       *slog.Logger
    mutex        lock.Mutex
    ipAllocators []*ipallocator.Range  // CIDR별 할당기 목록
    released     map[string]struct{}   // 해제 요청된 CIDR
    removed      map[string]struct{}   // 제거된 CIDR (아직 사용 중인 IP 존재)
}
```

### 할당 순서와 내부 단편화 방지

```go
// pkg/ipam/pool.go (라인 67-86)
func (p *cidrPool) allocateNext() (net.IP, error) {
    p.mutex.Lock()
    defer p.mutex.Unlock()

    // CIDR 목록 순서대로 할당 시도 → 내부 단편화 방지
    for _, ipAllocator := range p.ipAllocators {
        cidrStr := ipAllocator.CIDR().String()
        if _, removed := p.removed[cidrStr]; removed {
            continue  // 제거된 CIDR에서는 새 할당 안 함
        }
        if ipAllocator.Free() == 0 {
            continue  // 소진된 CIDR 건너뜀
        }
        return ipAllocator.AllocateNext()
    }
    return nil, errors.New("all CIDR ranges are exhausted")
}
```

**왜 순서대로 할당하는가?**

CIDR 목록을 순서대로 순회하면서 첫 번째로 여유가 있는 CIDR에서 할당하면,
IP들이 앞쪽 CIDR에 밀집된다. 이는 뒤쪽의 빈 CIDR을 조기에 릴리즈할 수 있게 하여
IP 대역을 효율적으로 회수할 수 있다.

### 초과 CIDR 릴리즈

```go
// pkg/ipam/pool.go (라인 183-213)
func (p *cidrPool) releaseExcessCIDRsMultiPool(neededIPs int) {
    p.mutex.Lock()
    defer p.mutex.Unlock()

    totalFree := 0
    for _, ipAllocator := range p.ipAllocators {
        totalFree += ipAllocator.Free()
    }

    // 역순으로 순회하여 뒤쪽 CIDR 우선 릴리즈
    retainedAllocators := []*ipallocator.Range{}
    for i := len(p.ipAllocators) - 1; i >= 0; i-- {
        ipAllocator := p.ipAllocators[i]
        free := ipAllocator.Free()
        // 사용량 0이고, 릴리즈해도 필요량 충족 시 릴리즈
        if ipAllocator.Used() == 0 && totalFree-free >= neededIPs {
            p.released[cidrStr] = struct{}{}
            totalFree -= free
        } else {
            retainedAllocators = append(retainedAllocators, ipAllocator)
        }
    }
    p.ipAllocators = retainedAllocators
}
```

### CIDR 풀 업데이트 (updatePool)

오퍼레이터가 CiliumNode CR의 CIDR 목록을 변경하면 `updatePool`이 호출된다.
이 메서드는 CIDR 추가/제거를 안전하게 처리한다.

```
updatePool 처리 흐름:

1. 새 CIDR 목록 파싱 및 중복 제거
2. released 맵에서 더 이상 존재하지 않는 CIDR 정리
3. 기존 allocator 유지/제거:
   - 새 목록에 있으면 유지
   - 새 목록에 없고 사용량 0이면 제거
   - 새 목록에 없지만 사용 중이면 removed 맵에 추가 (allocator 유지)
4. 새 CIDR에 대한 allocator 생성
5. ipAllocators 슬라이스 교체
```

```go
// pkg/ipam/pool.go (라인 215-324)
func (p *cidrPool) updatePool(CIDRs []string) {
    // ...
    // 기존 allocator 처리
    for _, ipAllocator := range p.ipAllocators {
        cidrStr := ipAllocator.CIDR().String()
        if _, ok := cidrStrSet[cidrStr]; !ok {
            if ipAllocator.Used() == 0 {
                continue  // 사용량 0이면 조용히 제거
            }
            // 사용 중인 CIDR이 제거됨 → removed 맵에 기록
            p.removed[cidrStr] = struct{}{}
        }
        newIPAllocators = append(newIPAllocators, ipAllocator)
    }
    // 새 CIDR에 대한 allocator 생성
    for _, cidrNet := range cidrNets {
        if _, ok := existingAllocators[cidrStr]; ok { continue }
        ipAllocator := ipallocator.NewCIDRRange(cidrNet)
        // ...
    }
}
```

## Node Manager

`NodeManager`는 CRD 기반 IPAM 모드에서 **오퍼레이터 측**에서 모든 노드의
IP 할당 상태를 관리하는 컴포넌트이다.

### 핵심 인터페이스

#### NodeOperations

각 클라우드 프로바이더(ENI, Azure, AlibabaCloud)가 구현해야 하는 인터페이스:

```go
// pkg/ipam/node_manager.go (라인 46-106)
type NodeOperations interface {
    UpdatedNode(obj *v2.CiliumNode)
    PopulateStatusFields(resource *v2.CiliumNode)

    // 인터페이스 생성 (ENI 생성 등)
    CreateInterface(ctx context.Context, allocation *AllocationAction,
        scopedLog *slog.Logger) (int, string, error)

    // 인터페이스/IP 동기화
    ResyncInterfacesAndIPs(ctx context.Context, scopedLog *slog.Logger) (
        ipamTypes.AllocationMap, ipamStats.InterfaceStats, error)

    // IP 할당 계획 수립
    PrepareIPAllocation(scopedLog *slog.Logger) (*AllocationAction, error)

    // 실제 IP 할당 수행
    AllocateIPs(ctx context.Context, allocation *AllocationAction) error

    // IP 릴리즈 계획 수립
    PrepareIPRelease(excessIPs int, scopedLog *slog.Logger) *ReleaseAction

    // 실제 IP 릴리즈 수행
    ReleaseIPs(ctx context.Context, release *ReleaseAction) error

    // 인스턴스 한계
    GetMaximumAllocatableIPv4() int
    GetMinimumAllocatableIPv4() int
    IsPrefixDelegated() bool
}
```

#### AllocationImplementation

노드에 종속되지 않는 전역 IPAM 구현:

```go
// pkg/ipam/node_manager.go (라인 108-136)
type AllocationImplementation interface {
    CreateNode(obj *v2.CiliumNode, node *Node) NodeOperations
    GetPoolQuota() ipamTypes.PoolQuotaMap
    Resync(ctx context.Context) time.Time
    InstanceSync(ctx context.Context, instanceID string) time.Time
    HasInstance(instanceID string) bool
    DeleteInstance(instanceID string)
}
```

### NodeManager 구조체

```go
// pkg/ipam/node_manager.go (라인 171-183)
type NodeManager struct {
    logger               *slog.Logger
    mutex                lock.RWMutex
    nodes                nodeMap                    // 노드 이름 → Node 매핑
    instancesAPI         AllocationImplementation   // 클라우드 API 구현
    k8sAPI               CiliumNodeGetterUpdater    // K8s API 클라이언트
    metricsAPI           MetricsAPI                 // 메트릭 수집
    parallelWorkers      int64                      // 병렬 워커 수
    releaseExcessIPs     bool                       // 초과 IP 릴리즈 활성화
    excessIPReleaseDelay int                        // 릴리즈 지연 (초)
    stableInstancesAPI   bool                       // API 안정성 상태
    prefixDelegation     bool                       // 프리픽스 위임 활성화
}
```

### Resync 메커니즘

NodeManager는 주기적으로 모든 노드를 순회하며 IP 부족/초과를 해결한다.

```go
// pkg/ipam/node_manager.go (라인 537-573)
func (n *NodeManager) Resync(ctx context.Context, syncTime time.Time) {
    n.mutex.Lock()
    defer n.mutex.Unlock()
    n.metricsAPI.IncResyncCount()

    stats := resyncStats{}
    sem := semaphore.NewWeighted(n.parallelWorkers)

    // IP 부족이 큰 노드 우선 처리 (정렬)
    for _, node := range n.GetNodesByIPWatermarkLocked() {
        sem.Acquire(ctx, 1)
        go func(node *Node, stats *resyncStats) {
            n.resyncNode(ctx, node, stats, syncTime)
            sem.Release(1)
        }(node, &stats)
    }

    // 모든 워커 완료 대기
    sem.Acquire(ctx, n.parallelWorkers)

    // 전체 메트릭 업데이트
    n.metricsAPI.SetAllocatedIPs("used", stats.ipv4.totalUsed)
    n.metricsAPI.SetAllocatedIPs("available", stats.ipv4.totalAvailable)
    // ...
}
```

### 워터마크 기반 우선순위

```go
// pkg/ipam/node_manager.go (라인 448-468)
func (n *NodeManager) GetNodesByIPWatermarkLocked() []*Node {
    list := make([]*Node, len(n.nodes))
    // ...
    sort.Slice(list, func(i, j int) bool {
        valuei := list[i].GetNeededAddresses()
        valuej := list[j].GetNeededAddresses()
        // 음수 = 초과 IP → 초과가 더 큰 노드 먼저 릴리즈
        if valuei < 0 && valuej < 0 {
            return valuei < valuej
        }
        // 양수 = 부족 IP → 부족이 더 큰 노드 먼저 할당
        return valuei > valuej
    })
    return list
}
```

**왜 워터마크 기반 정렬인가?**

IP 부족이 심한 노드부터 처리해야 파드 스케줄링 실패를 최소화할 수 있다.
동시에, IP 초과가 큰 노드에서 먼저 IP를 회수하면 클라우드 비용을 절약할 수 있다.

### Node 구조체

```go
// pkg/ipam/node.go (라인 64-137)
type Node struct {
    rootLogger *slog.Logger
    logger     atomic.Pointer[slog.Logger]
    mutex      lock.RWMutex

    name     string               // 노드 이름
    resource *v2.CiliumNode       // CiliumNode 리소스
    stats    Statistics            // IP 할당 통계

    instanceRunning bool          // 인스턴스 실행 상태
    ipv4Alloc       ipAllocAttrs  // IPv4 할당 속성

    resyncNeeded time.Time        // 재동기화 필요 시점
    manager      *NodeManager     // 상위 NodeManager

    poolMaintainer PoolMaintainer // IP 풀 유지 트리거
    k8sSync        *trigger.Trigger // K8s 동기화 트리거
    instanceSync   *trigger.Trigger // 인스턴스 동기화 트리거
    ops            NodeOperations   // 클라우드 API 구현
    retry          *trigger.Trigger // 재시도 트리거

    excessIPReleaseDelay time.Duration // 초과 IP 릴리즈 지연
}
```

### Statistics 구조체

```go
// pkg/ipam/node.go (라인 162-206)
type Statistics struct {
    IPv4                IPStatistics
    IPv6                IPStatistics
    EmptyInterfaceSlots int  // 추가 가능한 빈 인터페이스 슬롯
}

type IPStatistics struct {
    UsedIPs             int  // 현재 사용 중인 IP 수
    AvailableIPs        int  // 현재 할당 가능한 IP 수
    Capacity            int  // 최대 IPAM IP 용량
    NeededIPs           int  // PreAllocate 워터마크 달성에 필요한 IP 수
    ExcessIPs           int  // MaxAboveWatermark 초과 IP 수
    RemainingInterfaces int  // 남은 인터페이스 수
    InterfaceCandidates int  // IP 할당 가능한 인터페이스 수
    AssignedStaticIP    string // 할당된 정적 IP (예: AWS Elastic IP)
}
```

## 할당 흐름

### 전체 IP 할당 시퀀스

파드가 생성될 때 IP 할당이 어떻게 이루어지는지 전체 흐름을 추적한다.

```
[kubelet]                [CNI Plugin]           [cilium-agent]
    |                        |                       |
    | 파드 생성              |                       |
    |--- ADD 요청 --------->|                       |
    |                        |                       |
    |                        |--- IP 할당 요청 ---->|
    |                        |                       |
    |                        |               IPAM.AllocateNext()
    |                        |                  |
    |                        |                  v
    |                        |           allocatorMutex.Lock()
    |                        |                  |
    |                        |                  v
    |                        |           determineIPAMPool()
    |                        |           (metadata에서 풀 결정)
    |                        |                  |
    |                        |                  v
    |                        |           allocator.AllocateNext()
    |                        |           (모드별 구현 호출)
    |                        |                  |
    |                        |                  v
    |                        |           [제외 IP 확인]
    |                        |           isIPExcluded() ?
    |                        |              |      |
    |                        |           제외됨  정상
    |                        |           재시도    |
    |                        |                     v
    |                        |           registerIPOwner()
    |                        |           metrics 업데이트
    |                        |                  |
    |                        |<--- 결과 반환 ---|
    |                        |                       |
    |<--- 네트워크 설정 ---- |                       |
    |                        |                       |
```

### allocateNextFamily 상세

```go
// pkg/ipam/allocator.go (라인 131-191)
func (ipam *IPAM) allocateNextFamily(family Family, owner string,
    pool Pool, needSyncUpstream bool) (result *AllocationResult, err error) {

    // 1. 패밀리에 따라 할당기 선택
    var allocator Allocator
    switch family {
    case IPv6: allocator = ipam.ipv6Allocator
    case IPv4: allocator = ipam.ipv4Allocator
    }

    // 2. 풀이 지정되지 않으면 metadata에서 결정
    if pool == "" {
        pool, err = ipam.determineIPAMPool(owner, family)
    }

    // 3. 할당 루프 (제외 IP 스킵)
    for {
        if needSyncUpstream {
            result, err = allocator.AllocateNext(owner, pool)
        } else {
            result, err = allocator.AllocateNextWithoutSyncUpstream(owner, pool)
        }
        if err != nil { return }

        // 풀 이름 기본값 설정
        if result.IPPoolName == "" {
            result.IPPoolName = PoolDefault()
        }

        // 제외 IP가 아니면 성공
        if _, ok := ipam.isIPExcluded(result.IP, pool); !ok {
            ipam.registerIPOwner(result.IP, owner, pool)
            metrics.IPAMEvent.WithLabelValues(metricAllocate, string(family)).Inc()
            return
        }

        // 제외 IP는 "excluded" 소유자로 등록하고 다음 IP 시도
        ipam.registerIPOwner(result.IP,
            fmt.Sprintf("%s (excluded)", owner), pool)
    }
}
```

**왜 제외 IP에 대한 루프가 필요한가?**

일부 IP(예: 게이트웨이 IP, DNS 서버 IP)는 파드에 할당하면 안 된다.
`excludedIPs`에 등록된 IP가 할당되면, 해당 IP를 "excluded" 소유자로 마킹하고
다음 IP를 시도한다. 이렇게 하면 해당 IP가 다시 할당 후보에 오르지 않는다.

### IP 릴리즈 흐름

```go
// pkg/ipam/allocator.go (라인 269-316)
func (ipam *IPAM) releaseIPLocked(ip net.IP, pool Pool) error {
    // 1. 패밀리별 할당기에서 릴리즈
    family := IPv4
    if ip.To4() != nil {
        ipam.ipv4Allocator.Release(ip, pool)
        metrics.IPAMCapacity.WithLabelValues(string(family)).
            Set(float64(ipam.ipv4Allocator.Capacity()))
    } else {
        family = IPv6
        ipam.ipv6Allocator.Release(ip, pool)
        // ...
    }

    // 2. 소유자 정보 제거
    owner := ipam.releaseIPOwner(ip, pool)

    // 3. 만료 타이머가 있으면 정지 및 제거
    key := timerKey{ip: ip.String(), pool: pool}
    if t, ok := ipam.expirationTimers[key]; ok {
        close(t.stop)
        delete(ipam.expirationTimers, key)
    }

    // 4. 메트릭 업데이트
    metrics.IPAMEvent.WithLabelValues(metricRelease, string(family)).Inc()
    return nil
}
```

### 만료 타이머

IP 할당에 만료 시간을 설정하면, 외부 엔티티가 사라졌을 때 IP가 자동 회수된다.

```go
// pkg/ipam/allocator.go (StartExpirationTimer, 라인 379-438)
func (ipam *IPAM) StartExpirationTimer(ip net.IP, pool Pool,
    timeout time.Duration) (string, error) {
    allocationUUID := uuid.New().String()
    stop := make(chan struct{})
    ipam.expirationTimers[key] = expirationTimer{uuid: allocationUUID, stop: stop}

    go func(...) {
        timer := time.NewTimerWithoutMaxDelay(timeout)
        select {
        case <-stop:   // 명시적 정지
            timer.Stop()
            return
        case <-timer.C: // 타임아웃 → IP 자동 릴리즈
        }
        // ...
        ipam.releaseIPLocked(ip, pool)
    }(...)

    return allocationUUID, nil
}
```

**왜 UUID 기반 매칭인가?**

같은 IP가 릴리즈되었다가 재할당될 수 있다. 이전 할당의 만료 타이머가
새 할당의 IP를 해제하면 안 되므로, UUID로 할당 시점을 구분한다.
타임아웃 발생 시 UUID가 일치하는 경우에만 릴리즈를 수행한다.

## 왜 이 아키텍처인가?

### 1. Strategy Pattern으로 모드 분리

```
                  +-------------+
                  |  Allocator  |  (인터페이스)
                  +------+------+
                         |
          +--------------+--------------+
          |              |              |
    +-----v-----+  +----v------+  +----v---------+
    | hostScope |  | multiPool |  | crdAllocator |
    | Allocator |  | Allocator |  |              |
    +-----------+  +-----------+  +--------------+
    kubernetes     multi-pool     eni, azure,
    cluster-pool                  alibabacloud, crd
```

**왜?** 각 클라우드 환경의 IP 할당 메커니즘이 근본적으로 다르기 때문이다.

- **hostScope**: 로컬 CIDR 범위 내에서 비트맵 기반 할당. 가장 빠르고 단순.
- **multiPool**: 여러 CIDR 풀을 관리하며, 오퍼레이터와 연동하여 동적 CIDR 추가/제거.
- **crdAllocator**: 외부 시스템(오퍼레이터 + 클라우드 API)이 IP Pool을 채우고,
  에이전트는 풀에서 IP를 가져오기만 함.

이 분리 덕분에 새로운 클라우드 프로바이더를 추가할 때 `Allocator` 인터페이스만
구현하면 되고, 상위 레이어(CNI, 엔드포인트 관리)는 변경 없이 동작한다.

### 2. 에이전트-오퍼레이터 역할 분리

```
+------------------+              +------------------+
|  cilium-agent    |              | cilium-operator  |
|                  |              |                  |
| - 파드 IP 할당   |              | - 노드별 CIDR    |
| - 로컬 IP Pool   |  CiliumNode | 계획 및 할당     |
|   에서 IP 소비   |<--- CR ---->| - 클라우드 API   |
| - 사용 현황 보고 |              |   호출           |
| - 릴리즈 ACK/NACK|              | - IP 풀 보충     |
+------------------+              +------------------+
```

**왜?** 에이전트는 모든 노드에서 실행되므로 클라우드 API를 직접 호출하면
API Rate Limit에 걸릴 수 있다. 오퍼레이터가 중앙에서 클라우드 API를 호출하고,
에이전트는 CiliumNode CR을 통해 결과를 수신하면 API 호출을 최소화할 수 있다.

### 3. 비트맵 기반 CIDR/IP 할당

**왜 비트맵인가?**

- **O(1) 할당/해제**: 비트 하나를 설정/해제하는 것이므로 상수 시간
- **O(n) 탐색 최악 case**: 모든 비트를 순회해야 하지만, `nextCandidate`로 평균 탐색 거리 최소화
- **메모리 효율**: /16 클러스터에서 /24 노드는 256비트 = 32바이트만 필요
- **스냅샷 용이**: `big.Int.Bytes()`로 전체 상태를 직렬화 가능

### 4. 핸드셰이크 기반 IP 릴리즈

**왜 즉시 릴리즈하지 않는가?**

분산 시스템에서 오퍼레이터와 에이전트 사이에는 상태 불일치가 발생할 수 있다.
오퍼레이터가 IP를 회수해도 에이전트에서 아직 파드가 사용 중일 수 있다.
4단계 핸드셰이크(marked → ready/do-not → released → cleaned)는
이런 불일치를 안전하게 해소한다.

### 5. Pool 단위 관리

**왜 단일 전역 풀이 아닌 Pool 단위인가?**

Multi-Pool 모드에서 서로 다른 파드 그룹이 서로 다른 IP 대역을 사용해야 한다.
또한 풀별로 마스커레이드 정책, 라우팅 규칙을 다르게 적용할 수 있어야 한다.
Pool 추상화는 이런 유연성을 제공한다.

### 6. 트리거(Trigger) 기반 비동기 업데이트

NodeManager와 nodeStore 모두 `trigger.Trigger`를 사용하여 K8s API 서버에 대한
업데이트를 배치 처리한다.

**왜?** IP 할당/해제가 빈번하게 발생하면 매번 API 서버에 요청하면 부하가 급증한다.
트리거는 `MinInterval`을 설정하여 일정 시간 내의 여러 변경을 하나의 API 호출로
병합한다. 이는 API 서버의 Rate Limiting에 걸리지 않도록 하는 핵심 메커니즘이다.

## 참고 파일 목록

| 파일 경로 | 설명 |
|----------|------|
| `pkg/ipam/types.go` | `IPAM`, `Allocator`, `AllocationResult`, `Pool` 정의 |
| `pkg/ipam/ipam.go` | `NewIPAM()`, `ConfigureAllocator()` --- IPAM 초기화 |
| `pkg/ipam/allocator.go` | `AllocateNext()`, `ReleaseIP()`, `StartExpirationTimer()` --- 할당/해제 로직 |
| `pkg/ipam/option/option.go` | IPAM 모드 상수 (`IPAMKubernetes`, `IPAMClusterPool`, ...) |
| `pkg/ipam/hostscope.go` | `hostScopeAllocator` --- Kubernetes/ClusterPool 모드 구현 |
| `pkg/ipam/multipool.go` | `multiPoolAllocator`, `MultiPoolAllocatorParams` --- Multi-Pool 모드 |
| `pkg/ipam/multipool_manager.go` | `multiPoolManager` --- Multi-Pool 풀 관리 |
| `pkg/ipam/pool.go` | `cidrPool` --- 다중 CIDR 관리, 할당 순서, 릴리즈 |
| `pkg/ipam/cidrset/cidr_set.go` | `CidrSet` --- 비트맵 기반 CIDR 할당 알고리즘 |
| `pkg/ipam/crd.go` | `nodeStore`, `crdAllocator` --- CRD 기반 IPAM (ENI/Azure/AlibabaCloud) |
| `pkg/ipam/node_manager.go` | `NodeManager`, `NodeOperations`, `AllocationImplementation` |
| `pkg/ipam/node.go` | `Node`, `Statistics`, `IPStatistics` --- 노드별 IP 관리 |
