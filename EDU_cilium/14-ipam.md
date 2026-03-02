# 14. Cilium IPAM (IP Address Management) 심층 분석

## 목차

1. [개요](#1-개요)
2. [IPAM 모드 전체 비교](#2-ipam-모드-전체-비교)
3. [핵심 아키텍처](#3-핵심-아키텍처)
4. [IP 할당/해제 과정](#4-ip-할당해제-과정)
5. [CiliumNode CRD와 IP 풀 관리](#5-ciliumnode-crd와-ip-풀-관리)
6. [프리얼로케이션 (Pre-allocation) 메커니즘](#6-프리얼로케이션-pre-allocation-메커니즘)
7. [pkg/ipam/ 패키지 구조](#7-pkgipam-패키지-구조)
8. [듀얼스택 (IPv4+IPv6) 지원](#8-듀얼스택-ipv4ipv6-지원)
9. [IP 풀 고갈 처리](#9-ip-풀-고갈-처리)
10. [Multi-Pool IPAM](#10-multi-pool-ipam)
11. [클라우드 프로바이더 통합](#11-클라우드-프로바이더-통합)
12. [운영 관련 고려사항](#12-운영-관련-고려사항)

---

## 1. 개요

Cilium의 IPAM(IP Address Management)은 클러스터 내 파드에 IP 주소를 할당하고 관리하는 서브시스템이다. Cilium은 단일 IPAM 구현이 아닌 **다양한 IPAM 모드**를 제공하여 온프레미스부터 AWS, Azure, AlibabaCloud까지 다양한 환경을 지원한다.

### 핵심 설계 원칙

- **모드 기반 추상화**: `Allocator` 인터페이스를 통해 각 모드가 동일한 API로 IP를 할당/해제
- **선제적 할당 (Pre-allocation)**: IP가 필요해지기 전에 미리 풀을 확보하여 파드 기동 지연 최소화
- **CRD 기반 상태 관리**: CiliumNode CRD를 통해 노드별 IP 할당 상태를 Kubernetes-native 방식으로 관리
- **Operator-Agent 분리**: Operator가 클라우드 API와 통신하여 IP를 확보하고, Agent가 로컬에서 파드에 할당

### 소스 코드 위치

| 경로 | 설명 |
|------|------|
| `pkg/ipam/ipam.go` | IPAM 메인 구조체 및 NewIPAM, ConfigureAllocator |
| `pkg/ipam/allocator.go` | IP 할당/해제 로직 (AllocateNext, ReleaseIP 등) |
| `pkg/ipam/types.go` | Allocator 인터페이스, IPAM 구조체, AllocationResult 정의 |
| `pkg/ipam/option/option.go` | IPAM 모드 상수 정의 |
| `pkg/ipam/hostscope.go` | cluster-pool, kubernetes 모드의 호스트 스코프 할당기 |
| `pkg/ipam/crd.go` | CRD 기반 할당기 (ENI, Azure, AlibabaCloud) |
| `pkg/ipam/multipool.go` | Multi-pool IPAM 할당기 |
| `pkg/ipam/multipool_manager.go` | Multi-pool 관리자 (풀 생성, CIDR 관리) |
| `pkg/ipam/pool.go` | cidrPool - 다중 CIDR 기반 IP 할당 풀 |
| `pkg/ipam/node_manager.go` | NodeManager - CRD 기반 모드 노드 관리 |
| `pkg/ipam/node.go` | Node - 노드별 IP 할당 상태 및 워터마크 계산 |
| `pkg/ipam/allocator/clusterpool/` | cluster-pool 모드 Operator 측 구현 |
| `pkg/ipam/allocator/aws/aws.go` | AWS ENI 할당기 |
| `pkg/ipam/allocator/azure/azure.go` | Azure NIC 할당기 |
| `pkg/ipam/allocator/alibabacloud/` | AlibabaCloud ENI 할당기 |
| `operator/pkg/ipam/` | Operator 측 IPAM 통합 |

---

## 2. IPAM 모드 전체 비교

Cilium은 `--ipam` 옵션으로 IPAM 모드를 설정한다. 모드 상수는 `pkg/ipam/option/option.go`에 정의되어 있다:

```go
// pkg/ipam/option/option.go
const (
    IPAMKubernetes      = "kubernetes"
    IPAMCRD             = "crd"
    IPAMENI             = "eni"
    IPAMAzure           = "azure"
    IPAMClusterPool     = "cluster-pool"
    IPAMMultiPool       = "multi-pool"
    IPAMAlibabaCloud    = "alibabacloud"
    IPAMDelegatedPlugin = "delegated-plugin"
)
```

### 2.1 cluster-pool (기본값)

**개요**: Cilium Operator가 클러스터 전체 CIDR 풀에서 각 노드에 PodCIDR을 할당한다. 각 노드의 Agent는 할당받은 CIDR 내에서 파드에 IP를 부여한다.

**동작 흐름**:
1. Operator가 `--cluster-pool-ipv4-cidr`에서 노드 단위 CIDR을 잘라서 각 노드에 할당
2. Agent의 `hostScopeAllocator`가 해당 CIDR 내에서 순차적으로 IP 할당
3. CiliumNode CRD의 `spec.ipam.podCIDRs` 필드로 CIDR 정보 전달

```go
// pkg/ipam/ipam.go - ConfigureAllocator()
case ipamOption.IPAMKubernetes, ipamOption.IPAMClusterPool:
    if ipam.config.IPv4Enabled() {
        ipam.ipv4Allocator = newHostScopeAllocator(
            ipam.nodeAddressing.IPv4().AllocationCIDR().IPNet)
    }
```

**Operator 측 구현**: `pkg/ipam/allocator/clusterpool/clusterpool.go`

```go
// AllocatorOperator - 클러스터 풀에서 노드 CIDR 분배
type AllocatorOperator struct {
    v4CIDRSet, v6CIDRSet []cidralloc.CIDRAllocator
}
```

**장점**: 클라우드 독립적, 간단한 설정, 기본 모드
**단점**: 클라우드 네이티브 라우팅 활용 불가

### 2.2 kubernetes

**개요**: Kubernetes 자체의 PodCIDR 할당 메커니즘에 위임한다. `kube-controller-manager`가 노드에 PodCIDR을 할당하고, Cilium Agent가 해당 CIDR에서 IP를 할당한다.

**동작 흐름**:
1. `kube-controller-manager --allocate-node-cidrs`가 Node 오브젝트의 `spec.podCIDRs`를 설정
2. Cilium Agent가 노드의 `spec.podCIDRs`를 읽어 `hostScopeAllocator` 초기화
3. cluster-pool과 동일한 `hostScopeAllocator` 사용

**구현**: cluster-pool과 동일한 코드 경로 (`ConfigureAllocator`에서 같은 case)

**장점**: Kubernetes 표준 방식 활용
**단점**: Cilium Operator 미사용, 유연성 제한

### 2.3 eni (AWS ENI)

**개요**: AWS의 Elastic Network Interface(ENI)를 활용하여 VPC 네이티브 IP를 파드에 직접 할당한다. 각 EC2 인스턴스에 여러 ENI를 부착하고, 각 ENI에 보조(secondary) IP를 할당한다.

**동작 흐름**:
1. Operator의 `eni.InstancesManager`가 AWS EC2 API로 ENI 정보 동기화
2. `NodeManager`가 각 노드의 IP 수요를 계산 (프리얼로케이션 워터마크 기반)
3. IP 부족 시 기존 ENI에 보조 IP 추가 또는 새 ENI 생성
4. CiliumNode CRD의 `spec.ipam.pool`에 할당 가능한 IP 목록 기록
5. Agent의 `crdAllocator`가 풀에서 파드에 IP 할당

```go
// pkg/ipam/allocator/aws/aws.go
func (a *AllocatorAWS) Start(ctx context.Context, ...) (allocator.NodeEventHandler, error) {
    instances, _ := eni.NewInstancesManager(ctx, a.rootLogger, a.client, imds)
    nodeManager, _ := ipam.NewNodeManager(a.logger, instances, getterUpdater, iMetrics,
        a.ParallelAllocWorkers, a.AWSReleaseExcessIPs, a.ExcessIPReleaseDelay,
        a.AWSEnablePrefixDelegation)
    nodeManager.Start(ctx)
    return nodeManager, nil
}
```

**AllocationResult 구성** (Agent 측 `crd.go`):
```go
// ENI 모드에서 할당 결과에 VPC 정보 포함
case ipamOption.IPAMENI:
    result.PrimaryMAC = eni.MAC
    result.CIDRs = []string{eni.VPC.PrimaryCIDR}
    result.GatewayIP = deriveGatewayIP(eni.Subnet.CIDR, 1)
    result.InterfaceNumber = strconv.Itoa(eni.Number)
```

**장점**: VPC 네이티브 라우팅, 오버레이 불필요
**단점**: AWS 전용, 인스턴스 타입별 ENI/IP 제한

### 2.4 azure

**개요**: Azure NIC(Network Interface Controller)를 활용하여 Azure VNet 네이티브 IP를 할당한다.

**동작 흐름**: ENI 모드와 유사하나 Azure API 사용
- `azureIPAM.InstancesManager`가 Azure API로 NIC 정보 동기화
- `NodeManager`가 동일한 프리얼로케이션/워터마크 로직 사용
- CiliumNode CRD를 통한 Operator-Agent 통신

```go
// pkg/ipam/allocator/azure/azure.go
func (a *AllocatorAzure) Start(ctx context.Context, ...) {
    instances := azureIPAM.NewInstancesManager(a.rootLogger, azureClient)
    nodeManager, _ := ipam.NewNodeManager(a.logger, instances, getterUpdater, iMetrics,
        a.ParallelAllocWorkers, false, 0, false)
}
```

**장점**: Azure VNet 네이티브 라우팅
**단점**: Azure 전용

### 2.5 alibabacloud

**개요**: Alibaba Cloud ENI를 활용한 VPC 네이티브 IP 할당이다.

```go
// pkg/ipam/allocator/alibabacloud/alibabacloud.go
func (a *AllocatorAlibabaCloud) Start(ctx context.Context, ...) {
    instances := eni.NewInstancesManager(a.rootLogger, a.client)
    nodeManager, _ := ipam.NewNodeManager(a.logger, instances, getterUpdater, iMetrics,
        a.ParallelAllocWorkers, a.AlibabaCloudReleaseExcessIPs, 0, false)
}
```

**장점**: Alibaba Cloud VPC 네이티브
**단점**: Alibaba Cloud 전용

### 2.6 multi-pool

**개요**: `CiliumPodIPPool` CRD를 통해 여러 IP 풀을 정의하고, 파드의 annotation 또는 label을 기반으로 특정 풀에서 IP를 할당한다.

**핵심 차별점**:
- 하나의 노드에 여러 CIDR 풀을 동시에 할당 가능
- 파드마다 다른 풀에서 IP 할당 가능
- `ipam.cilium.io/ip-pool` annotation으로 풀 선택

```go
// pkg/ipam/multipool_manager.go
type multiPoolManager struct {
    pools                  map[Pool]*poolPair  // 풀 이름 -> v4/v6 CIDR 풀
    preallocatedIPsPerPool preAllocatePerPool   // 풀별 사전 할당 수
    pendingIPsPerPool      *pendingAllocationsPerPool
}
```

**풀 선택 메커니즘** (`pkg/ipam/metadata/manager.go`):
1. 파드의 `ipam.cilium.io/ip-pool` annotation 확인
2. 없으면 `CiliumPodIPPool`의 `podSelector` 및 `namespaceSelector`로 매칭
3. 매칭되는 풀에서 IP 할당

**장점**: 네트워크 분리, 멀티테넌트 지원, 유연한 IP 정책
**단점**: 설정 복잡도 증가

### 2.7 delegated-plugin

**개요**: Cilium CNI가 다른 CNI 플러그인에 IPAM을 위임한다. Cilium 자체는 IPAM을 수행하지 않는다.

```go
// pkg/ipam/ipam.go
case ipamOption.IPAMDelegatedPlugin:
    ipam.ipv6Allocator = &noOpAllocator{}
    ipam.ipv4Allocator = &noOpAllocator{}
```

### 모드 비교 요약표

| 모드 | 환경 | IP 소스 | Operator 필요 | 프리얼로케이션 | 듀얼스택 |
|------|------|---------|--------------|---------------|---------|
| cluster-pool | 범용 | Cilium CIDR 풀 | O | O (Operator 측) | O |
| kubernetes | 범용 | K8s PodCIDR | X | X | O |
| eni | AWS | VPC ENI 보조 IP | O | O (워터마크) | 제한적 |
| azure | Azure | VNet NIC IP | O | O (워터마크) | 제한적 |
| alibabacloud | Alibaba | VPC ENI IP | O | O (워터마크) | 제한적 |
| multi-pool | 범용 | 다중 CIDR 풀 | O | O (풀별) | O |
| delegated-plugin | 범용 | 외부 CNI | X | X | 외부 의존 |

---

## 3. 핵심 아키텍처

### 3.1 Allocator 인터페이스

모든 IPAM 모드는 `Allocator` 인터페이스를 구현한다:

```go
// pkg/ipam/types.go
type Allocator interface {
    Allocate(ip net.IP, owner string, pool Pool) (*AllocationResult, error)
    AllocateWithoutSyncUpstream(ip net.IP, owner string, pool Pool) (*AllocationResult, error)
    Release(ip net.IP, pool Pool) error
    AllocateNext(owner string, pool Pool) (*AllocationResult, error)
    AllocateNextWithoutSyncUpstream(owner string, pool Pool) (*AllocationResult, error)
    Dump() (map[Pool]map[string]string, string)
    Capacity() uint64
    RestoreFinished()
}
```

### 3.2 IPAM 구조체

```go
// pkg/ipam/types.go
type IPAM struct {
    ipv6Allocator Allocator  // IPv6 할당기
    ipv4Allocator Allocator  // IPv4 할당기
    owner         map[Pool]map[string]string  // 풀별 IP->소유자 매핑
    excludedIPs   map[string]string           // 제외된 IP
    expirationTimers map[timerKey]expirationTimer // 만료 타이머
}
```

### 3.3 AllocationResult

```go
// pkg/ipam/types.go
type AllocationResult struct {
    IP              net.IP   // 할당된 IP
    IPPoolName      Pool     // IP 풀 이름
    CIDRs           []string // 직접 접근 가능한 CIDR 목록 (VPC 모드)
    PrimaryMAC      string   // 주 인터페이스 MAC (ENI 모드)
    GatewayIP       string   // 게이트웨이 IP (VPC 모드)
    InterfaceNumber string   // 인터페이스 번호 (ENI 모드)
    SkipMasquerade  bool     // 마스커레이드 건너뛰기 (multi-pool)
    ExpirationUUID  string   // 만료 타이머 UUID
}
```

### 3.4 Allocator 구현체 매핑

```
ConfigureAllocator()
├── cluster-pool / kubernetes → hostScopeAllocator (pkg/ipam/hostscope.go)
├── eni / azure / alibabacloud → crdAllocator (pkg/ipam/crd.go)
├── multi-pool → multiPoolAllocator (pkg/ipam/multipool.go)
└── delegated-plugin → noOpAllocator (pkg/ipam/noop_allocator.go)
```

---

## 4. IP 할당/해제 과정

### 4.1 IP 할당 (AllocateNext)

`pkg/ipam/allocator.go`에서 IP 할당의 전체 흐름:

```
AllocateNext(family, owner, pool)
    ├── ipv6Result = AllocateNextFamily(IPv6, owner, pool)
    └── ipv4Result = AllocateNextFamily(IPv4, owner, pool)
            ├── allocatorMutex.Lock()
            ├── pool = determineIPAMPool(owner, family)  // multi-pool인 경우
            ├── loop:
            │   ├── result = allocator.AllocateNext(owner, pool)
            │   ├── if isIPExcluded(result.IP, pool): continue
            │   └── registerIPOwner(result.IP, owner, pool)
            └── metrics.IPAMEvent.Inc()
```

**핵심 포인트**:
- `allocatorMutex`로 동시 할당 방지
- 제외된 IP는 자동으로 건너뛰기
- 할당 실패 시 이미 할당된 다른 패밀리 IP 롤백 (듀얼스택)

### 4.2 특정 IP 할당 (AllocateIP)

```go
// pkg/ipam/allocator.go
func (ipam *IPAM) allocateIP(ip net.IP, owner string, pool Pool, needSyncUpstream bool) {
    // 1. 풀 이름 필수 확인
    // 2. 제외 IP 확인
    // 3. IPv4/IPv6에 따라 적절한 allocator 호출
    // 4. 풀 이름이 비어있으면 기본 풀로 설정
    // 5. IP 소유자 등록
}
```

### 4.3 IP 해제 (ReleaseIP)

```go
// pkg/ipam/allocator.go
func (ipam *IPAM) releaseIPLocked(ip net.IP, pool Pool) error {
    // 1. 풀 이름 필수 확인
    // 2. 적절한 allocator의 Release 호출
    // 3. 소유자 정보 삭제
    // 4. 만료 타이머가 있으면 정리
    // 5. 메트릭 업데이트
}
```

### 4.4 만료 타이머

할당된 IP에 만료 타이머를 설정하여, 일정 시간 내에 사용되지 않으면 자동 해제할 수 있다:

```go
// pkg/ipam/allocator.go
func (ipam *IPAM) StartExpirationTimer(ip net.IP, pool Pool, timeout time.Duration) (string, error) {
    // UUID 생성 → 고루틴으로 타이머 시작 → 만료 시 releaseIPLocked 호출
}
```

이는 `AllocateNextWithExpiration`에서 사용되며, 외부 엔티티가 IP를 사용하기 전에 사라질 수 있는 시나리오를 처리한다.

---

## 5. CiliumNode CRD와 IP 풀 관리

### 5.1 CRD 기반 모드의 아키텍처

ENI, Azure, AlibabaCloud 모드에서는 CiliumNode CRD가 Operator와 Agent 사이의 통신 채널 역할을 한다:

```
[Operator]                    [CiliumNode CRD]                [Agent]
    │                              │                              │
    │ ──── spec.ipam.pool 업데이트 ──→ │                              │
    │     (할당 가능한 IP 목록)       │ ──── 변경 감지 ──────────────→ │
    │                              │                              │ ← 파드에 IP 할당
    │                              │ ←── status.ipam.used 업데이트 ─ │
    │ ←── 사용 중 IP 확인 ────────── │                              │
    │                              │                              │
```

### 5.2 nodeStore (Agent 측)

`pkg/ipam/crd.go`의 `nodeStore`는 로컬 CiliumNode 리소스를 관리한다:

```go
type nodeStore struct {
    ownNode            *ciliumv2.CiliumNode  // 자신의 CiliumNode
    allocators         []*crdAllocator       // IPv4/IPv6 할당기
    allocationPoolSize map[Family]int         // 패밀리별 풀 크기
    refreshTrigger     *trigger.Trigger       // CRD 업데이트 트리거
    restoreFinished    chan struct{}          // 복구 완료 신호
}
```

**풀 업데이트 흐름** (`updateLocalNodeResource`):
1. CiliumNode 업데이트 수신
2. `spec.ipam.pool`에서 IP 목록 갱신
3. `status.ipam.releaseIPs` 핸드셰이크 처리
4. 풀 크기(IPv4/IPv6 별) 재계산

### 5.3 crdAllocator (Agent 측)

```go
// pkg/ipam/crd.go
type crdAllocator struct {
    store     *nodeStore                  // 공유 노드 저장소
    allocated ipamTypes.AllocationMap      // 할당된 IP 맵
    family    Family                       // 주소 패밀리
}
```

할당 과정:
1. `allocateNext` → nodeStore의 `spec.ipam.pool`에서 미사용 IP 검색
2. `markAllocated` → 로컬 `allocated` 맵에 기록
3. `refreshTrigger` → CiliumNode `status.ipam.used` 업데이트

### 5.4 NodeManager (Operator 측)

`pkg/ipam/node_manager.go`의 `NodeManager`는 모든 CRD 기반 모드에서 공통으로 사용된다:

```go
type NodeManager struct {
    nodes            nodeMap                    // 노드 이름 → Node
    instancesAPI     AllocationImplementation   // 클라우드별 구현
    k8sAPI           CiliumNodeGetterUpdater    // K8s API
    metricsAPI       MetricsAPI                 // 메트릭
    parallelWorkers  int64                      // 병렬 워커 수
    releaseExcessIPs bool                       // 초과 IP 해제 여부
}
```

**Resync 과정**:
1. `instancesAPIResync` → 클라우드 API와 동기화
2. `GetNodesByIPWatermarkLocked` → IP 부족/과잉 노드 정렬
3. 각 노드에 대해 `resyncNode` → `recalculate` → 워터마크 기반 할당/해제 결정

---

## 6. 프리얼로케이션 (Pre-allocation) 메커니즘

### 6.1 워터마크 기반 프리얼로케이션

CRD 기반 모드(ENI, Azure, AlibabaCloud)에서는 워터마크를 통해 IP를 선제적으로 확보한다:

```go
// pkg/ipam/node.go
func calculateNeededIPs(availableIPs, usedIPs, preAllocate, minAllocate, maxAllocate int) int {
    neededIPs = preAllocate - (availableIPs - usedIPs)
    if minAllocate > 0 {
        neededIPs = max(neededIPs, minAllocate - availableIPs)
    }
    if maxAllocate > 0 && (availableIPs + neededIPs) > maxAllocate {
        neededIPs = maxAllocate - availableIPs
    }
    return max(neededIPs, 0)
}
```

**파라미터**:
- `preAllocate`: 항상 유지해야 할 여유 IP 수 (기본값: `defaults.IPAMPreAllocation`)
- `minAllocate`: 노드에 할당해야 할 최소 IP 수
- `maxAllocate`: 노드에 할당할 수 있는 최대 IP 수
- `maxAboveWatermark`: 워터마크 위로 추가 할당할 수 있는 최대 IP 수

### 6.2 초과 IP 계산

```go
// pkg/ipam/node.go
func calculateExcessIPs(availableIPs, usedIPs, preAllocate, minAllocate, maxAboveWatermark int) int {
    // minAllocate + maxAboveWatermark 이하 사용 시: 해당 한도 초과분만 반환
    // 그 이상 사용 시: availableIPs - usedIPs - preAllocate - maxAboveWatermark
}
```

### 6.3 Multi-Pool 프리얼로케이션

Multi-pool 모드에서는 풀별로 독립적인 프리얼로케이션을 수행한다:

```go
// pkg/ipam/multipool_manager.go
func neededIPCeil(numIP int, preAlloc int) int {
    // 예: preAlloc=16
    //   numIP  0 -> 16
    //   numIP  1 -> 32
    //   numIP 16 -> 32
    //   numIP 17 -> 48
    // 항상 최소 preAlloc 만큼의 여유 확보
    quotient := numIP / preAlloc
    rem := numIP % preAlloc
    if rem > 0 {
        return (quotient + 2) * preAlloc
    }
    return (quotient + 1) * preAlloc
}

func (m *multiPoolManager) computeNeededIPsPerPoolLocked() map[Pool]types.IPAMPoolDemand {
    // 각 풀별로: inUseIPs + pendingIPs를 neededIPCeil로 올림
}
```

### 6.4 Pending Allocation 추적

IP 풀이 일시적으로 비어 있는 경우, 대기 중인 할당 요청을 추적한다:

```go
// pkg/ipam/multipool_manager.go
type pendingAllocationsPerPool struct {
    pools map[Pool]pendingAllocationsPerOwner
    clock func() time.Time
}
```

- 할당 실패 시 `upsertPendingAllocation` → 대기 요청 등록
- 성공 시 `markAsAllocated` → 대기 요청 제거
- `pendingAllocationTTL` (5분) 경과 시 자동 만료

---

## 7. pkg/ipam/ 패키지 구조

### 7.1 핵심 파일

```
pkg/ipam/
├── ipam.go                 # IPAM 생성자, ConfigureAllocator, 유틸리티
├── allocator.go            # AllocateNext, ReleaseIP, 만료 타이머
├── types.go                # Allocator 인터페이스, IPAM 구조체, AllocationResult
├── hostscope.go            # hostScopeAllocator (cluster-pool, kubernetes)
├── crd.go                  # crdAllocator + nodeStore (ENI/Azure/AlibabaCloud)
├── crd_eni.go              # ENI 모드 전용 디바이스 설정
├── multipool.go            # multiPoolAllocator
├── multipool_manager.go    # multiPoolManager (CIDR 풀 관리, 프리얼로케이션)
├── pool.go                 # cidrPool (다중 CIDR 기반 IP 풀)
├── node_manager.go         # NodeManager (CRD 기반 모드 노드 관리)
├── node.go                 # Node (노드별 상태, 워터마크, IP 해제 핸드셰이크)
├── noop_allocator.go       # noOpAllocator (delegated-plugin용)
├── doc.go                  # 패키지 문서
├── stats/                  # IP 통계
├── metrics/                # IPAM 메트릭
├── metadata/               # Pod IP Pool 메타데이터 관리
├── podippool/              # CiliumPodIPPool 관련
├── option/                 # IPAM 모드 상수
├── types/                  # 공통 타입 정의
├── cell/                   # Hive DI 셀
├── cidrset/                # CIDR 세트 관리
├── service/ipallocator/    # IP 범위 할당기
├── api/                    # REST API
├── allocator/              # Operator 측 할당기 구현
│   ├── provider.go         # NodeEventHandler 인터페이스
│   ├── clusterpool/        # cluster-pool Operator 구현
│   │   ├── clusterpool.go
│   │   └── cidralloc/      # CIDR 할당기
│   ├── podcidr/            # PodCIDR 관리 (NodesPodCIDRManager)
│   ├── aws/aws.go          # AWS ENI Operator 구현
│   ├── azure/azure.go      # Azure Operator 구현
│   └── alibabacloud/       # AlibabaCloud Operator 구현
└── *_test.go               # 테스트 파일
```

### 7.2 핵심 인터페이스 계층

```
Allocator (pkg/ipam/types.go)
├── hostScopeAllocator (hostscope.go)     - ipallocator.Range 래핑
├── crdAllocator (crd.go)                  - nodeStore 기반
├── multiPoolAllocator (multipool.go)      - multiPoolManager → cidrPool
└── noOpAllocator (noop_allocator.go)      - 아무것도 안 함

AllocationImplementation (node_manager.go)
├── eni.InstancesManager (AWS)
├── azureIPAM.InstancesManager (Azure)
└── alibabacloud.InstancesManager (AlibabaCloud)

NodeOperations (node_manager.go)
├── CreateInterface       - 새 ENI/NIC 생성
├── PrepareIPAllocation   - 할당 액션 계산
├── AllocateIPs           - 클라우드 API로 IP 할당
├── PrepareIPRelease      - 해제 액션 계산
└── ReleaseIPs            - 클라우드 API로 IP 해제
```

---

## 8. 듀얼스택 (IPv4+IPv6) 지원

### 8.1 패밀리 구분

```go
// pkg/ipam/ipam.go
type Family string
const (
    IPv6 Family = "ipv6"
    IPv4 Family = "ipv4"
)

func DeriveFamily(ip net.IP) Family {
    if ip.To4() == nil { return IPv6 }
    return IPv4
}
```

### 8.2 듀얼스택 할당

`AllocateNext` 함수에서 `family == ""`이면 IPv4와 IPv6 모두 할당한다:

```go
// pkg/ipam/allocator.go
func (ipam *IPAM) AllocateNext(family, owner string, pool Pool) (ipv4Result, ipv6Result *AllocationResult, err error) {
    if (family == "ipv6" || family == "") && ipam.ipv6Allocator != nil {
        ipv6Result, err = ipam.AllocateNextFamily(IPv6, owner, pool)
    }
    if (family == "ipv4" || family == "") && ipam.ipv4Allocator != nil {
        ipv4Result, err = ipam.AllocateNextFamily(IPv4, owner, pool)
        if err != nil && ipv6Result != nil {
            ipam.ReleaseIP(ipv6Result.IP, ipv6Result.IPPoolName)  // 롤백
        }
    }
}
```

### 8.3 모드별 듀얼스택 지원

- **cluster-pool / kubernetes**: `ConfigureAllocator`에서 IPv4, IPv6 각각 `hostScopeAllocator` 생성
- **multi-pool**: `multiPoolAllocator`가 패밀리별로 생성되며, `poolPair`가 v4/v6 cidrPool을 보유
- **CRD 기반 (ENI 등)**: `crdAllocator`가 패밀리별로 생성, 동일한 nodeStore 공유

```go
// pkg/ipam/multipool.go - poolPair
type poolPair struct {
    v4 *cidrPool
    v6 *cidrPool
}
```

---

## 9. IP 풀 고갈 처리

### 9.1 고갈 감지

**cluster-pool / kubernetes 모드**:
- `hostScopeAllocator.AllocateNext()`가 에러 반환 (ipallocator.Range 고갈)

**CRD 기반 모드**:
- `crdAllocator.allocateNext()`에서 미사용 IP가 없으면 에러:
  ```
  "no IPs currently available on the node, allocation will be retried once Cilium Operator allocates more IPs"
  ```
- Operator가 자동으로 새 IP를 프로비저닝 시도

**Multi-pool 모드**:
- `cidrPool.allocateNext()`에서 "all CIDR ranges are exhausted" 에러
- Pending allocation 등록 → Operator에 추가 CIDR 요청

### 9.2 복구 메커니즘

**CRD 기반 모드의 자동 복구**:
1. `NodeManager.Resync` (1분 주기) → 모든 노드 재계산
2. 부족 감지 시 `poolMaintainer.Trigger()` → `MaintainIPPool` 호출
3. 인터페이스에 여유 슬롯 있으면 보조 IP 추가
4. 인터페이스 슬롯 없으면 새 인터페이스 생성 시도

**Multi-pool 모드의 CIDR 확장**:
1. `computeNeededIPsPerPoolLocked` → 풀별 수요 계산
2. `updateLocalNode` → CiliumNode CRD의 `spec.ipam.pools.requested` 업데이트
3. Operator가 요청을 확인하고 새 CIDR 할당
4. CiliumNode CRD의 `spec.ipam.pools.allocated` 업데이트
5. Agent가 새 CIDR을 `cidrPool.updatePool`으로 반영

### 9.3 IP 해제 핸드셰이크 (CRD 기반 모드)

IP 해제는 안전한 4단계 핸드셰이크로 진행된다:

```
[Operator]                                          [Agent]
    │                                                   │
    │ ── marked-for-release (status.ipam.releaseIPs) ──→ │
    │                                                   │ 사용 중 확인
    │ ←── ready-for-release 또는 do-not-release ──────── │
    │                                                   │
    │ (ready면) IP 해제 → released ─────────────────────→ │
    │                                                   │ spec.ipam.pool에서 제거 확인 후
    │ ←── 항목 삭제 ───────────────────────────────────── │
```

```go
// pkg/ipam/option/option.go
const (
    IPAMMarkForRelease  = "marked-for-release"   // Operator → Agent
    IPAMReadyForRelease = "ready-for-release"     // Agent → Operator (안전)
    IPAMDoNotRelease    = "do-not-release"         // Agent → Operator (거부)
    IPAMReleased        = "released"               // Operator → Agent (완료)
)
```

### 9.4 초과 IP 지연 해제

```go
// pkg/ipam/node.go
// excessIPReleaseDelay 동안 대기 후에만 해제 진행
for markedIP, ts := range n.ipv4Alloc.ipsMarkedForRelease {
    if time.Since(ts) > n.excessIPReleaseDelay {
        ipsToMark = append(ipsToMark, markedIP)
    }
}
```

---

## 10. Multi-Pool IPAM

### 10.1 CiliumPodIPPool CRD

Multi-pool 모드에서는 `CiliumPodIPPool` CRD로 IP 풀을 정의한다:

```yaml
apiVersion: cilium.io/v2alpha1
kind: CiliumPodIPPool
metadata:
  name: production
spec:
  ipv4:
    cidrs:
      - cidr: "10.10.0.0/16"
        maskSize: 24
  ipv6:
    cidrs:
      - cidr: "fd00:10:10::/48"
        maskSize: 64
  podSelector:
    matchLabels:
      env: production
```

### 10.2 풀 선택 로직

`pkg/ipam/metadata/manager.go`에서 파드에 맞는 풀을 선택한다:

1. 파드에 `ipam.cilium.io/ip-pool` annotation이 있으면 해당 풀 사용
2. 없으면 `CiliumPodIPPool`의 `podSelector` 및 `namespaceSelector`로 매칭

### 10.3 cidrPool 동작

`pkg/ipam/pool.go`의 `cidrPool`은 다중 CIDR을 관리한다:

```go
type cidrPool struct {
    ipAllocators []*ipallocator.Range  // CIDR별 할당기
    released     map[string]struct{}    // 해제 대기 CIDR
    removed      map[string]struct{}    // 제거된 CIDR (아직 사용 중)
}
```

- `allocateNext()`: CIDR 순서대로 할당 시도 (내부 단편화 방지)
- `updatePool()`: Operator가 할당한 새 CIDR 추가, 기존 CIDR 유지
- `releaseExcessCIDRsMultiPool()`: 미사용 CIDR 해제 (역순으로 해제하여 최신 CIDR 우선 해제)

---

## 11. 클라우드 프로바이더 통합

### 11.1 공통 구조

모든 클라우드 IPAM은 동일한 패턴을 따른다:

```
CloudAllocator.Init()  → 클라우드 API 클라이언트 초기화
CloudAllocator.Start() → InstancesManager + NodeManager 생성 및 시작
    ├── InstancesManager: 클라우드 인스턴스/인터페이스 상태 동기화
    └── NodeManager: 워터마크 기반 IP 할당/해제 오케스트레이션
```

### 11.2 NodeOperations 인터페이스

각 클라우드 프로바이더는 `NodeOperations`를 구현한다:

```go
// pkg/ipam/node_manager.go
type NodeOperations interface {
    UpdatedNode(obj *v2.CiliumNode)
    PopulateStatusFields(resource *v2.CiliumNode)
    CreateInterface(ctx context.Context, allocation *AllocationAction, ...) (int, string, error)
    ResyncInterfacesAndIPs(ctx context.Context, ...) (AllocationMap, InterfaceStats, error)
    PrepareIPAllocation(scopedLog *slog.Logger) (*AllocationAction, error)
    AllocateIPs(ctx context.Context, allocation *AllocationAction) error
    PrepareIPRelease(excessIPs int, ...) *ReleaseAction
    ReleaseIPs(ctx context.Context, release *ReleaseAction) error
    GetMaximumAllocatableIPv4() int
    GetMinimumAllocatableIPv4() int
    IsPrefixDelegated() bool
}
```

### 11.3 AWS ENI 특화 기능

- **Prefix Delegation**: `/28` 프리픽스 단위 할당 (16 IP/프리픽스)
- **ENI Garbage Collection**: 오래된 미사용 ENI 자동 정리
- **Surge Allocation**: 대기 중인 Pod 수에 따라 추가 IP 선제 할당

---

## 12. 운영 관련 고려사항

### 12.1 주요 설정 파라미터

| 파라미터 | 설명 | 기본값 |
|---------|------|--------|
| `--ipam` | IPAM 모드 | cluster-pool |
| `--cluster-pool-ipv4-cidr` | 클러스터 IPv4 CIDR | 10.0.0.0/8 |
| `--cluster-pool-ipv6-cidr` | 클러스터 IPv6 CIDR | fd00::/104 |
| `spec.ipam.pre-allocate` | 노드별 프리얼로케이션 수 | 8 |
| `spec.ipam.min-allocate` | 노드별 최소 할당 수 | 0 |
| `spec.ipam.max-allocate` | 노드별 최대 할당 수 | 0 (무제한) |
| `spec.ipam.max-above-watermark` | 워터마크 위 최대 추가 할당 | 0 |
| `--excess-ip-release-delay` | 초과 IP 해제 지연 시간 | 0s |

### 12.2 모니터링 메트릭

- `cilium_ipam_available`: 사용 가능한 IP 수
- `cilium_ipam_used`: 사용 중인 IP 수
- `cilium_ipam_needed`: 추가 필요한 IP 수
- `cilium_ipam_capacity`: 총 IPAM 용량
- `cilium_ipam_allocation_ops`: 할당 작업 수
- `cilium_ipam_release_ops`: 해제 작업 수
- `cilium_ipam_interface_creation_ops`: 인터페이스 생성 작업 수

### 12.3 트러블슈팅

**IP 고갈 시**:
1. `cilium status`에서 IPAM 상태 확인
2. `kubectl get ciliumnodes -o yaml`로 노드별 풀 상태 확인
3. cluster-pool: 클러스터 CIDR 크기 확인
4. ENI: 인스턴스 타입별 ENI/IP 제한 확인
5. multi-pool: CiliumPodIPPool 리소스 및 annotation 확인

**프리얼로케이션 조정**:
- IP 고갈 빈번: `pre-allocate` 값 증가
- IP 낭비: `pre-allocate` 값 감소, `release-excess-ips` 활성화
- 빠른 스케일아웃 필요: `max-above-watermark` 증가
