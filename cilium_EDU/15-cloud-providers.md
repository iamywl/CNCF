# 15. 클라우드 프로바이더 통합

## 목차

1. [개요](#1-개요)
2. [클라우드 IPAM 아키텍처](#2-클라우드-ipam-아키텍처)
3. [AWS ENI 통합](#3-aws-eni-통합)
4. [Azure 통합](#4-azure-통합)
5. [NodeOperations 인터페이스](#5-nodeoperations-인터페이스)
6. [Node Manager 오케스트레이션](#6-node-manager-오케스트레이션)
7. [CiliumNode CRD](#7-ciliumnode-crd)
8. [할당 메트릭](#8-할당-메트릭)
9. [왜 이 아키텍처인가?](#9-왜-이-아키텍처인가)
10. [참고 파일 목록](#10-참고-파일-목록)

---

## 1. 개요

Cilium은 쿠버네티스 환경에서 파드에 IP 주소를 할당하는 IPAM(IP Address Management) 기능을
제공한다. 특히 AWS, Azure 같은 퍼블릭 클라우드에서 실행될 때, 해당 클라우드의 네이티브 네트워킹
리소스를 직접 활용하여 파드 네트워킹을 구성한다.

### 클라우드 네이티브 IPAM이 필요한 이유

전통적인 오버레이 네트워크(VXLAN, Geneve)는 모든 환경에서 동작하지만, 클라우드 환경에서는
다음과 같은 한계가 있다:

| 문제 | 오버레이 방식 | 클라우드 네이티브 IPAM |
|------|-------------|---------------------|
| 성능 | 캡슐화/역캡슐화 오버헤드 | VPC 네이티브 라우팅, 오버헤드 없음 |
| 보안 그룹 | 파드 단위 적용 불가 | ENI/인터페이스 단위 보안 그룹 적용 |
| 가시성 | VPC Flow Log에서 파드 IP 미식별 | 파드 IP가 VPC 네이티브 IP로 표시 |
| 라우팅 | 별도 라우팅 테이블 관리 필요 | VPC 라우팅 테이블 자동 통합 |
| 로드밸런서 | NodePort 경유 필수 | 파드 IP 직접 타겟 등록 가능 |

### 지원하는 클라우드 프로바이더

Cilium은 다음 클라우드 프로바이더를 지원한다:

| 프로바이더 | IPAM 모드 | 리소스 유형 | 인터페이스 생성 |
|-----------|----------|-----------|--------------|
| AWS | `eni` | ENI (Elastic Network Interface) | 동적 생성 지원 |
| Azure | `azure` | NIC (Network Interface Card) | 미지원 (기존 NIC 활용) |

각 프로바이더는 동일한 추상 인터페이스(`NodeOperations`, `AllocationImplementation`)를
구현하되, 클라우드 고유의 API와 리소스 모델에 맞게 동작을 달리한다.

---

## 2. 클라우드 IPAM 아키텍처

### 전체 아키텍처 다이어그램

```
+------------------------------------------------------------------+
|                      Cilium Operator                              |
|                                                                   |
|  +-----------------------------+                                  |
|  |     Allocator (AWS/Azure)   |  AllocatorAWS / AllocatorAzure  |
|  |  - Init(): 클라우드 클라이언트 초기화                              |
|  |  - Start(): NodeManager 생성                                   |
|  +-------------|---------------+                                  |
|                |                                                  |
|                v                                                  |
|  +-----------------------------+     +-------------------------+  |
|  |       NodeManager           |---->| AllocationImplementation|  |
|  | - nodes: map[string]*Node   |     | (InstancesManager)      |  |
|  | - instancesAPI              |     | - Resync()              |  |
|  | - metricsAPI                |     | - CreateNode()          |  |
|  | - parallelWorkers           |     | - GetPoolQuota()        |  |
|  +-------------|---------------+     +-------------------------+  |
|                |                                                  |
|    +-----------+-----------+                                      |
|    |                       |                                      |
|    v                       v                                      |
| +-------+  +-------+  +-------+                                  |
| | Node  |  | Node  |  | Node  |   각 K8s 노드마다 1개              |
| | - ops |  | - ops |  | - ops |   ops = NodeOperations 구현체     |
| +---+---+  +---+---+  +---+---+                                  |
|     |          |          |                                       |
+-----|----------|----------|--------------------------------------+
      |          |          |
      v          v          v
  +------+   +------+   +------+
  |Cloud |   |Cloud |   |Cloud |   각 노드의 클라우드 인스턴스
  | API  |   | API  |   | API  |
  +------+   +------+   +------+
```

### 계층 구조

클라우드 IPAM은 4개의 명확한 계층으로 나뉜다:

```
계층 4: Allocator            AllocatorAWS, AllocatorAzure
         |                   클라우드별 초기화, 클라이언트 생성
         v
계층 3: NodeManager           pkg/ipam/node_manager.go
         |                   모든 노드 관리, 리싱크 오케스트레이션
         v
계층 2: Node (IPAM)           pkg/ipam/node.go
         |                   노드별 IP 풀 유지보수, K8s 동기화
         v
계층 1: NodeOperations        pkg/aws/eni/node.go, pkg/azure/ipam/node.go
                              클라우드별 IP 할당/해제 실제 구현
```

### 초기화 시퀀스

```
Operator 시작
    |
    v
AllocatorAWS.Init()  또는  AllocatorAzure.Init()
    |                           |
    | - AWS SDK 설정 로드         | - (Azure는 Init에서 별도 작업 없음)
    | - EC2 클라이언트 생성        |
    | - 서브넷/인스턴스 필터 설정   |
    v                           v
AllocatorAWS.Start()         AllocatorAzure.Start()
    |                           |
    | - InstancesManager 생성    | - Azure API 클라이언트 생성
    | - NodeManager 생성          | - InstancesManager 생성
    | - nodeManager.Start()      | - NodeManager 생성
    | - ENI GC 시작 (옵션)        | - nodeManager.Start()
    v                           v
NodeManager.Start()
    |
    | - instancesAPIResync() 블로킹 호출 (최초 동기화)
    | - 백그라운드 리싱크 컨트롤러 시작 (1분 간격)
    v
CiliumNode 이벤트 수신 대기
```

---

## 3. AWS ENI 통합

### 3.1 AllocatorAWS 구조체

AWS ENI 통합의 최상위 진입점은 `AllocatorAWS` 구조체다.

**소스 경로**: `pkg/ipam/allocator/aws/aws.go` (31~48행)

```go
// AllocatorAWS is an implementation of IPAM allocator interface for AWS ENI
type AllocatorAWS struct {
    AWSReleaseExcessIPs          bool
    ExcessIPReleaseDelay         int
    AWSEnablePrefixDelegation    bool
    ENITags                      map[string]string
    ENIGarbageCollectionTags     map[string]string
    ENIGarbageCollectionInterval time.Duration
    AWSUsePrimaryAddress         bool
    EC2APIEndpoint               string
    AWSMaxResultsPerCall         int32
    ParallelAllocWorkers         int64

    rootLogger *slog.Logger
    logger     *slog.Logger
    client     *ec2shim.Client
    eniGCTags  map[string]string
}
```

| 필드 | 용도 |
|------|------|
| `AWSReleaseExcessIPs` | 초과 IP 자동 해제 활성화 여부 |
| `ExcessIPReleaseDelay` | 초과 IP 해제 전 대기 시간(초) |
| `AWSEnablePrefixDelegation` | /28 프리픽스 위임 모드 활성화 |
| `ENITags` | 새로 생성되는 ENI에 부여할 태그 |
| `ENIGarbageCollectionTags` | 미사용 ENI 정리용 태그 |
| `ENIGarbageCollectionInterval` | ENI GC 실행 주기 |
| `AWSUsePrimaryAddress` | ENI 기본 IP도 파드에 할당할지 여부 |
| `EC2APIEndpoint` | 커스텀 EC2 API 엔드포인트 |
| `ParallelAllocWorkers` | 병렬 할당 워커 수 |

### 3.2 Init 메서드

`Init()`은 AWS SDK 설정을 로드하고 EC2 클라이언트를 생성한다.

**소스 경로**: `pkg/ipam/allocator/aws/aws.go` (87~129행)

```go
func (a *AllocatorAWS) Init(ctx context.Context, logger *slog.Logger, reg *metrics.Registry) error {
    // ...
    cfg, err := ec2shim.NewConfig(ctx)
    // ...
    subnetsFilters := ec2shim.NewSubnetsFilters(
        operatorOption.Config.IPAMSubnetsTags,
        operatorOption.Config.IPAMSubnetsIDs,
    )
    instancesFilters := ec2shim.NewTagsFilter(operatorOption.Config.IPAMInstanceTags)
    // ...
    a.client = ec2shim.NewClient(a.rootLogger, ec2.NewFromConfig(cfg, optionsFunc),
        aMetrics, operatorOption.Config.IPAMAPIQPSLimit,
        operatorOption.Config.IPAMAPIBurst, subnetsFilters, instancesFilters,
        eniCreationTags, a.AWSUsePrimaryAddress, a.AWSMaxResultsPerCall)
    return nil
}
```

핵심 동작:
1. AWS SDK v2 설정 로드 (`ec2shim.NewConfig`)
2. 서브넷 필터 생성 (태그 또는 ID 기반)
3. 인스턴스 필터 생성 (태그 기반)
4. ENI GC 태그 초기화 (EKS 클러스터명 자동 감지 포함)
5. EC2 클라이언트 생성 (QPS 제한, 버스트 설정 적용)

### 3.3 Start 메서드

`Start()`는 실제 ENI 할당 시스템을 기동한다.

**소스 경로**: `pkg/ipam/allocator/aws/aws.go` (134~172행)

```go
func (a *AllocatorAWS) Start(ctx context.Context, getterUpdater ipam.CiliumNodeGetterUpdater,
    reg *metrics.Registry) (allocator.NodeEventHandler, error) {
    // ...
    instances, err := eni.NewInstancesManager(ctx, a.rootLogger, a.client, imds)
    // ...
    nodeManager, err := ipam.NewNodeManager(a.logger, instances, getterUpdater, iMetrics,
        a.ParallelAllocWorkers, a.AWSReleaseExcessIPs, a.ExcessIPReleaseDelay,
        a.AWSEnablePrefixDelegation)
    // ...
    if err := nodeManager.Start(ctx); err != nil {
        return nil, err
    }
    // ENI GC 시작 (옵션)
    if a.ENIGarbageCollectionInterval > 0 {
        eni.StartENIGarbageCollector(ctx, a.rootLogger, a.client,
            eni.GarbageCollectionParams{...})
    }
    return nodeManager, nil
}
```

시작 순서:
1. IMDS(Instance Metadata Service) 클라이언트 초기화
2. `InstancesManager` 생성 (EC2 API + 메타데이터 API)
3. `NodeManager` 생성 (인스턴스 매니저를 `AllocationImplementation`으로 전달)
4. `NodeManager.Start()` 호출 (최초 리싱크 + 백그라운드 컨트롤러)
5. ENI 가비지 컬렉터 시작 (선택적)

### 3.4 ENI InstancesManager

`InstancesManager`는 AWS EC2 인스턴스와 ENI의 전체 상태를 관리한다.

**소스 경로**: `pkg/aws/eni/instances.go` (56~72행)

```go
type InstancesManager struct {
    logger     *slog.Logger
    resyncLock lock.RWMutex
    vpcID      string
    mutex          lock.RWMutex
    instances      *ipamTypes.InstanceMap
    subnets        ipamTypes.SubnetMap
    vpcs           ipamTypes.VirtualNetworkMap
    routeTables    ipamTypes.RouteTableMap
    securityGroups types.SecurityGroupMap
    ec2api         EC2API
    metadataapi    MetadataAPI
    limitsGetter   *limits.LimitsGetter
}
```

| 필드 | 용도 |
|------|------|
| `vpcID` | 오퍼레이터가 실행 중인 VPC ID (리소스 필터링용) |
| `instances` | 전체 EC2 인스턴스 맵 (인스턴스ID -> 인터페이스 정보) |
| `subnets` | VPC 서브넷 맵 (서브넷ID -> 가용 주소 수 등) |
| `vpcs` | VPC 정보 맵 |
| `routeTables` | 라우팅 테이블 맵 |
| `securityGroups` | 보안 그룹 맵 |
| `limitsGetter` | 인스턴스 타입별 ENI/IP 제한 조회기 |

### 3.5 EC2API 인터페이스

`InstancesManager`가 사용하는 EC2 API 추상화 인터페이스다.

**소스 경로**: `pkg/aws/eni/instances.go` (29~48행)

```go
type EC2API interface {
    GetInstance(ctx context.Context, vpcs ipamTypes.VirtualNetworkMap,
        subnets ipamTypes.SubnetMap, instanceID string) (*ipamTypes.Instance, error)
    GetInstances(ctx context.Context, vpcs ipamTypes.VirtualNetworkMap,
        subnets ipamTypes.SubnetMap) (*ipamTypes.InstanceMap, error)
    GetSubnets(ctx context.Context, vpcID string) (ipamTypes.SubnetMap, error)
    GetVpcs(ctx context.Context, vpcID string) (ipamTypes.VirtualNetworkMap, error)
    GetRouteTables(ctx context.Context, vpcID string) (ipamTypes.RouteTableMap, error)
    GetSecurityGroups(ctx context.Context, vpcID string) (types.SecurityGroupMap, error)

    GetDetachedNetworkInterfaces(ctx context.Context, tags ipamTypes.Tags,
        maxResults int32) ([]string, error)
    CreateNetworkInterface(ctx context.Context, toAllocate int32, subnetID, desc string,
        groups []string, allocatePrefixes bool) (string, *eniTypes.ENI, error)
    AttachNetworkInterface(ctx context.Context, index int32,
        instanceID, eniID string) (string, error)
    DeleteNetworkInterface(ctx context.Context, eniID string) error
    ModifyNetworkInterface(ctx context.Context, eniID, attachmentID string,
        deleteOnTermination bool) error
    AssignPrivateIpAddresses(ctx context.Context, eniID string,
        addresses int32) ([]string, error)
    UnassignPrivateIpAddresses(ctx context.Context, eniID string,
        addresses []string) error
    AssignENIPrefixes(ctx context.Context, eniID string, prefixes int32) error
    UnassignENIPrefixes(ctx context.Context, eniID string, prefixes []string) error
    GetInstanceTypes(context.Context) ([]ec2_types.InstanceTypeInfo, error)
    AssociateEIP(ctx context.Context, eniID string,
        eipTags ipamTypes.Tags) (string, error)
}
```

이 인터페이스는 Cilium이 EC2에 대해 수행하는 모든 작업을 정의한다:

| 카테고리 | 메서드 | 용도 |
|---------|--------|------|
| 조회 | `GetInstances`, `GetInstance` | 인스턴스 및 ENI 정보 조회 |
| 조회 | `GetSubnets`, `GetVpcs` | 네트워크 인프라 조회 |
| 조회 | `GetSecurityGroups`, `GetRouteTables` | 보안/라우팅 정보 조회 |
| ENI 생성 | `CreateNetworkInterface` | 새 ENI 생성 |
| ENI 관리 | `AttachNetworkInterface` | ENI를 인스턴스에 부착 |
| ENI 관리 | `DeleteNetworkInterface` | ENI 삭제 |
| ENI 관리 | `ModifyNetworkInterface` | ENI 속성 변경 |
| IP 할당 | `AssignPrivateIpAddresses` | ENI에 보조 IP 할당 |
| IP 해제 | `UnassignPrivateIpAddresses` | ENI에서 보조 IP 해제 |
| 프리픽스 | `AssignENIPrefixes` | ENI에 /28 프리픽스 할당 |
| 프리픽스 | `UnassignENIPrefixes` | ENI에서 /28 프리픽스 해제 |

### 3.6 ENI Node 구조체

각 쿠버네티스 노드에 대응하는 AWS ENI 관련 상태를 관리한다.

**소스 경로**: `pkg/aws/eni/node.go` (54~75행)

```go
type Node struct {
    rootLogger *slog.Logger
    logger     atomic.Pointer[slog.Logger]
    // node contains the general purpose fields of a node
    node ipamNodeActions

    // mutex protects members below this field
    mutex lock.RWMutex

    // enis is the list of ENIs attached to the node indexed by ENI ID.
    enis map[string]eniTypes.ENI

    // k8sObj is the CiliumNode custom resource representing the node
    k8sObj *v2.CiliumNode

    // manager is the EC2 node manager responsible for this node
    manager *InstancesManager

    // instanceID of the node
    instanceID string
}
```

### 3.7 ENI 할당 흐름

AWS에서 파드 IP가 할당되는 전체 흐름을 시퀀스로 표현하면 다음과 같다:

```
NodeManager         ENI Node            EC2 API          InstancesManager
    |                   |                   |                   |
    |--Resync()-------->|                   |                   |
    |                   |                   |                   |
    |  ResyncInterfaces |                   |                   |
    |------------------>|                   |                   |
    |                   |--ForeachInstance-->|                   |
    |                   |<--ENI 목록---------|                   |
    |                   |                   |                   |
    |  PrepareIPAlloc   |                   |                   |
    |------------------>|                   |                   |
    |                   |  각 ENI별 가용 IP 계산                  |
    |                   |  서브넷 가용 주소 확인                   |
    |<--AllocationAction|                   |                   |
    |                   |                   |                   |
    | [case 1: 기존 ENI에 IP 추가]                               |
    |  AllocateIPs      |                   |                   |
    |------------------>|                   |                   |
    |                   |--AssignPrivateIps->|                   |
    |                   |<--할당된 IP 목록----|                   |
    |                   |                   |                   |
    | [case 2: 새 ENI 필요]                                      |
    |  CreateInterface  |                   |                   |
    |------------------>|                   |                   |
    |                   |  findSuitableSubnet                   |
    |                   |  getSecurityGroupIDs                  |
    |                   |--CreateNetworkInterface-->|            |
    |                   |<--eniID, ENI 정보---------|            |
    |                   |--AttachNetworkInterface-->|            |
    |                   |<--attachmentID-----------|            |
    |                   |--ModifyNetworkInterface-->|            |
    |                   |  (deleteOnTermination)   |            |
    |                   |                   |                   |
    |  syncToAPIServer  |                   |                   |
    |   (CiliumNode     |                   |                   |
    |    Status 갱신)    |                   |                   |
```

### 3.8 프리픽스 위임 (Prefix Delegation)

AWS에서는 개별 IP 대신 /28 프리픽스(16개 IP)를 ENI에 할당할 수 있다. 이 모드는
`AWSEnablePrefixDelegation` 플래그로 활성화된다.

**소스 경로**: `pkg/aws/eni/node.go` (409~434행)

```go
func (n *Node) AllocateIPs(ctx context.Context, a *ipam.AllocationAction) error {
    n.mutex.RLock()
    isPrefixDelegated := n.node.Ops().IsPrefixDelegated()
    n.mutex.RUnlock()

    if isPrefixDelegated {
        numPrefixes := ip.PrefixCeil(a.IPv4.AvailableForAllocation, option.ENIPDBlockSizeIPv4)
        err := n.manager.ec2api.AssignENIPrefixes(ctx, a.InterfaceID, int32(numPrefixes))
        if !isSubnetAtPrefixCapacity(err) {
            return err
        }
        // /28 프리픽스 고갈 시 /32 개별 IP 할당으로 폴백
    }
    assignedIPs, err := n.manager.ec2api.AssignPrivateIpAddresses(
        ctx, a.InterfaceID, int32(a.IPv4.AvailableForAllocation))
    // ...
}
```

프리픽스 위임의 장점:

| 항목 | 개별 IP 모드 | 프리픽스 위임 모드 |
|------|------------|-----------------|
| ENI당 IP 수 | IPv4 제한에 의존 | IPv4 슬롯 x 16 |
| API 호출 수 | IP당 1회 | 프리픽스당 1회 |
| 웜업 속도 | 느림 | 빠름 |
| IP 고갈 시 | 즉시 실패 | /32 폴백 가능 |

### 3.9 ENI 가비지 컬렉션

Cilium Operator는 분리된(detached) ENI를 주기적으로 정리한다.

**소스 경로**: `pkg/ipam/allocator/aws/aws.go` (163~169행)

```go
if a.ENIGarbageCollectionInterval > 0 {
    eni.StartENIGarbageCollector(ctx, a.rootLogger, a.client,
        eni.GarbageCollectionParams{
            RunInterval:    a.ENIGarbageCollectionInterval,
            MaxPerInterval: defaults.ENIGarbageCollectionMaxPerInterval,
            ENITags:        a.eniGCTags,
        })
}
```

GC 태그는 다음 우선순위로 결정된다:
1. 사용자가 `--eni-gc-tags`로 지정한 태그
2. Cilium 클러스터명이 설정된 경우 해당 클러스터명 태그
3. EKS 클러스터명 자동 감지
4. 기본 Cilium 관리 태그 (다른 클러스터의 ENI도 정리할 수 있어 주의 필요)

### 3.10 AWS Resync 메커니즘

`InstancesManager.Resync()`는 전체 인프라 상태를 EC2 API에서 다시 가져온다.

**소스 경로**: `pkg/aws/eni/instances.go` (214~265행)

```go
func (m *InstancesManager) Resync(ctx context.Context) time.Time {
    m.resyncLock.Lock()
    defer m.resyncLock.Unlock()
    return m.resync(ctx, "")
}

func (m *InstancesManager) resync(ctx context.Context, instanceID string) time.Time {
    if instanceID == "" {
        // 전체 리싱크: 인프라 동기화 + 모든 인스턴스 조회
        if err := m.syncInfrastructure(ctx); err != nil { ... }
        instances, err := m.ec2api.GetInstances(ctx, vpcs, subnets)
        // ...
        m.instances = instances
    } else {
        // 단일 인스턴스 리싱크: 특정 인스턴스만 갱신
        instance, err := m.ec2api.GetInstance(ctx, vpcs, subnets, instanceID)
        // ...
    }
}
```

리싱크는 두 가지 모드로 동작한다:

```
전체 리싱크 (instanceID == "")
+----------------------------------------+
| 1. syncInfrastructure()                |
|    - GetVpcs() -> VPC 목록              |
|    - GetSubnets() -> 서브넷 목록         |
|    - GetRouteTables() -> 라우팅 테이블    |
|    - GetSecurityGroups() -> 보안 그룹    |
|                                        |
| 2. GetInstances() -> 모든 인스턴스의     |
|    ENI 정보를 일괄 조회                   |
|                                        |
| 3. m.instances = instances (전체 교체)   |
+----------------------------------------+

단일 인스턴스 리싱크 (instanceID != "")
+----------------------------------------+
| 1. GetInstance(instanceID)             |
|    - 특정 인스턴스의 ENI 정보만 조회       |
|                                        |
| 2. m.instances.Update(instanceID, ...)  |
|    - 해당 인스턴스만 갱신                  |
+----------------------------------------+
```

---

## 4. Azure 통합

### 4.1 AllocatorAzure 구조체

Azure 통합의 최상위 진입점이다.

**소스 경로**: `pkg/ipam/allocator/azure/azure.go` (22~32행)

```go
type AllocatorAzure struct {
    AzureSubscriptionID         string
    AzureResourceGroup          string
    AzureUserAssignedIdentityID string
    AzureUsePrimaryAddress      bool
    ParallelAllocWorkers        int64

    rootLogger *slog.Logger
    logger     *slog.Logger
}
```

| 필드 | 용도 |
|------|------|
| `AzureSubscriptionID` | Azure 구독 ID (미설정 시 IMS에서 자동 감지) |
| `AzureResourceGroup` | Azure 리소스 그룹 (미설정 시 IMS에서 자동 감지) |
| `AzureUserAssignedIdentityID` | 사용자 할당 관리 ID |
| `AzureUsePrimaryAddress` | NIC 기본 IP도 파드에 할당할지 여부 |
| `ParallelAllocWorkers` | 병렬 할당 워커 수 |

AWS와 비교했을 때 Azure는 구조체가 훨씬 간결하다. 이는 Azure의 NIC 모델이 AWS ENI보다
단순하기 때문이다 -- Azure는 새 NIC 생성을 지원하지 않고 기존 NIC에 IP만 추가한다.

### 4.2 Azure Init과 Start

**소스 경로**: `pkg/ipam/allocator/azure/azure.go` (35~102행)

```go
// Init in Azure implementation doesn't need to do anything
func (a *AllocatorAzure) Init(ctx context.Context, logger *slog.Logger,
    reg *metrics.Registry) error {
    a.rootLogger = logger
    a.logger = a.rootLogger.With(logfields.LogSubsys, "ipam-allocator-azure")
    return nil
}
```

Azure의 `Init()`은 로거 설정만 수행한다. 실제 클라이언트 초기화는 `Start()`에서 이루어진다.

```go
func (a *AllocatorAzure) Start(ctx context.Context,
    getterUpdater ipam.CiliumNodeGetterUpdater,
    reg *metrics.Registry) (allocator.NodeEventHandler, error) {
    // 1. Azure 클라우드 이름 감지 (IMS 경유)
    azureCloudName, err := azureAPI.GetAzureCloudName(ctx, a.rootLogger)
    // 2. 구독 ID 확인 (CLI 또는 IMS)
    subscriptionID := a.AzureSubscriptionID
    if subscriptionID == "" {
        subID, err := azureAPI.GetSubscriptionID(ctx, a.rootLogger)
        // ...
    }
    // 3. 리소스 그룹 확인 (CLI 또는 IMS)
    resourceGroupName := a.AzureResourceGroup
    if resourceGroupName == "" {
        rgName, err := azureAPI.GetResourceGroupName(ctx, a.rootLogger)
        // ...
    }
    // 4. Azure API 클라이언트 생성
    azureClient, err := azureAPI.NewClient(a.rootLogger, azureCloudName,
        subscriptionID, resourceGroupName, ...)
    // 5. InstancesManager + NodeManager 생성
    instances := azureIPAM.NewInstancesManager(a.rootLogger, azureClient)
    nodeManager, err := ipam.NewNodeManager(a.logger, instances, getterUpdater,
        iMetrics, a.ParallelAllocWorkers, false, 0, false)
    // ...
}
```

주목할 점: `NewNodeManager` 호출 시 `releaseExcessIPs=false`, `prefixDelegation=false`를
전달한다. Azure는 초과 IP 해제와 프리픽스 위임을 지원하지 않는다.

### 4.3 AzureAPI 인터페이스

**소스 경로**: `pkg/azure/ipam/instances.go` (22~37행)

```go
type AzureAPI interface {
    GetInstance(ctx context.Context, subnets ipamTypes.SubnetMap,
        instanceID string) (*ipamTypes.Instance, error)
    GetInstances(ctx context.Context,
        subnets ipamTypes.SubnetMap) (*ipamTypes.InstanceMap, error)
    GetVpcsAndSubnets(ctx context.Context) (
        ipamTypes.VirtualNetworkMap, ipamTypes.SubnetMap, error)
    GetSubnetsByIDs(ctx context.Context,
        nodeSubnetIDs []string) (ipamTypes.SubnetMap, error)
    AssignPrivateIpAddressesVM(ctx context.Context,
        subnetID, interfaceName string, addresses int) error
    AssignPrivateIpAddressesVMSS(ctx context.Context,
        instanceID, vmssName, subnetID, interfaceName string, addresses int) error
    AssignPublicIPAddressesVM(ctx context.Context, instanceID string,
        publicIpTags ipamTypes.Tags) (string, error)
    AssignPublicIPAddressesVMSS(ctx context.Context, instanceID, vmssName string,
        publicIpTags ipamTypes.Tags) (string, error)
    ListAllNetworkInterfaces(ctx context.Context) ([]*armnetwork.Interface, error)
    ParseInterfacesIntoInstanceMap(
        networkInterfaces []*armnetwork.Interface,
        subnets ipamTypes.SubnetMap) *ipamTypes.InstanceMap
    ListVMNetworkInterfaces(ctx context.Context,
        instanceID string) ([]*armnetwork.Interface, error)
    ParseInterfacesIntoInstance(
        networkInterfaces []*armnetwork.Interface,
        subnets ipamTypes.SubnetMap) *ipamTypes.Instance
}
```

AWS EC2API와 비교한 주요 차이점:

| 항목 | AWS EC2API | Azure AzureAPI |
|------|-----------|----------------|
| 인터페이스 생성 | `CreateNetworkInterface` | 없음 (미지원) |
| 인터페이스 부착 | `AttachNetworkInterface` | 없음 (이미 부착됨) |
| IP 할당 | `AssignPrivateIpAddresses` | `AssignPrivateIpAddressesVM` / `VMSS` |
| 프리픽스 위임 | `AssignENIPrefixes` / `UnassignENIPrefixes` | 없음 (미지원) |
| 인스턴스 유형 | 단일 API | VM / VMSS 분리 |
| 최적화 | 없음 | `ListAllNetworkInterfaces` + `ParseInterfacesIntoInstanceMap` |

### 4.4 Azure InstancesManager

**소스 경로**: `pkg/azure/ipam/instances.go` (39~51행)

```go
type InstancesManager struct {
    logger     *slog.Logger
    resyncLock lock.RWMutex
    mutex      lock.RWMutex
    instances  *ipamTypes.InstanceMap
    subnets    ipamTypes.SubnetMap
    api        AzureAPI
}
```

AWS `InstancesManager`와 비교하면 Azure 버전은 `vpcs`, `routeTables`, `securityGroups`,
`limitsGetter` 필드가 없다. Azure는 NIC가 이미 VM에 부착되어 있으므로 이러한 인프라 정보를
별도로 캐시할 필요가 없다.

### 4.5 Azure Node 구조체

**소스 경로**: `pkg/azure/ipam/node.go` (24~37행)

```go
type Node struct {
    // k8sObj is the CiliumNode custom resource representing the node
    k8sObj *v2.CiliumNode
    // node contains the general purpose fields of a node
    node ipamNodeActions
    // manager is the Azure node manager responsible for this node
    manager *InstancesManager
    // vmss is the Azure VM Scale Set the node belongs to (optional)
    vmss string
}
```

`vmss` 필드는 노드가 VMSS(VM Scale Set)에 속하는 경우 스케일 셋 이름을 저장한다.
이 값은 `ResyncInterfacesAndIPs` 실행 시 첫 번째 인터페이스에서 자동으로 캐시된다.

### 4.6 Azure IP 할당 흐름

```
NodeManager         Azure Node          Azure API
    |                   |                   |
    |  PrepareIPAlloc   |                   |
    |------------------>|                   |
    |                   |  ForeachInterface  |
    |                   |  인터페이스별 가용 IP 계산
    |                   |  (InterfaceAddressLimit - 현재 IP 수)
    |                   |  서브넷 가용 주소 확인
    |<--AllocationAction|                   |
    |                   |                   |
    |  AllocateIPs      |                   |
    |------------------>|                   |
    |                   |  [VMSS 노드인 경우]  |
    |                   |--AssignPrivateIpAddressesVMSS-->|
    |                   |                   |
    |                   |  [일반 VM인 경우]    |
    |                   |--AssignPrivateIpAddressesVM---->|
    |                   |                   |
    |  syncToAPIServer  |                   |
```

### 4.7 Azure의 특수 사항

**인터페이스 생성 미지원**:

**소스 경로**: `pkg/azure/ipam/node.go` (151~155행)

```go
func (n *Node) CreateInterface(ctx context.Context, allocation *ipam.AllocationAction,
    scopedLog *slog.Logger) (int, string, error) {
    return 0, "", fmt.Errorf("not implemented")
}
```

**IP 해제 미지원**:

**소스 경로**: `pkg/azure/ipam/node.go` (73~75행)

```go
func (n *Node) ReleaseIPs(ctx context.Context, r *ipam.ReleaseAction) error {
    return fmt.Errorf("not implemented")
}
```

**최대 IP 256개 고정**:

**소스 경로**: `pkg/azure/ipam/node.go` (229~233행)

```go
func (n *Node) GetMaximumAllocatableIPv4() int {
    // An Azure node can allocate up to 256 private IP addresses
    return types.InterfaceAddressLimit
}
```

**VM vs VMSS 분기**:

**소스 경로**: `pkg/azure/ipam/node.go` (131~142행)

```go
func (n *Node) AllocateIPs(ctx context.Context, a *ipam.AllocationAction) error {
    iface, ok := a.Interface.Resource.(*types.AzureInterface)
    // ...
    if iface.GetVMScaleSetName() == "" {
        return n.manager.api.AssignPrivateIpAddressesVM(ctx,
            string(a.PoolID), iface.Name, a.IPv4.AvailableForAllocation)
    } else {
        return n.manager.api.AssignPrivateIpAddressesVMSS(ctx,
            iface.GetVMID(), iface.GetVMScaleSetName(),
            string(a.PoolID), iface.Name, a.IPv4.AvailableForAllocation)
    }
}
```

### 4.8 Azure Resync 메커니즘

**소스 경로**: `pkg/azure/ipam/instances.go` (90~140행)

Azure의 리싱크도 전체/단일 인스턴스 두 가지 모드를 지원한다:

```
전체 리싱크
+--------------------------------------------------+
| 1. GetVpcsAndSubnets() -> VNet, 서브넷 정보         |
| 2. ListAllNetworkInterfaces() -> 모든 NIC 일괄 조회  |
| 3. ParseInterfacesIntoInstanceMap() -> 인스턴스맵 변환|
| 4. m.instances = instances (전체 교체)              |
+--------------------------------------------------+

단일 인스턴스 리싱크
+--------------------------------------------------+
| 1. GetInstance(instanceID) -> 인스턴스 NIC 조회      |
| 2. 서브넷 ID 추출                                    |
| 3. GetSubnetsByIDs() -> 대상 서브넷만 조회            |
| 4. GetInstance() 재호출 (서브넷 정보 포함)             |
| 5. m.instances.Update(instanceID, ...) (부분 갱신)   |
+--------------------------------------------------+
```

Azure 리싱크의 최적화: `ListAllNetworkInterfaces()`로 모든 NIC를 한 번에 가져온 후
`ParseInterfacesIntoInstanceMap()`으로 파싱한다. 이는 인스턴스별로 개별 API를 호출하는
것보다 훨씬 효율적이다.

---

## 5. NodeOperations 인터페이스

`NodeOperations`는 클라우드 IPAM의 핵심 추상화다. 이 인터페이스를 통해 NodeManager는
클라우드 프로바이더의 구현 세부 사항을 알 필요 없이 IP를 관리한다.

**소스 경로**: `pkg/ipam/node_manager.go` (39~106행)

```go
type NodeOperations interface {
    UpdatedNode(obj *v2.CiliumNode)
    PopulateStatusFields(resource *v2.CiliumNode)
    CreateInterface(ctx context.Context, allocation *AllocationAction,
        scopedLog *slog.Logger) (int, string, error)
    ResyncInterfacesAndIPs(ctx context.Context,
        scopedLog *slog.Logger) (ipamTypes.AllocationMap, ipamStats.InterfaceStats, error)
    PrepareIPAllocation(scopedLog *slog.Logger) (*AllocationAction, error)
    AllocateIPs(ctx context.Context, allocation *AllocationAction) error
    AllocateStaticIP(ctx context.Context, staticIPTags ipamTypes.Tags) (string, error)
    PrepareIPRelease(excessIPs int, scopedLog *slog.Logger) *ReleaseAction
    ReleaseIPPrefixes(ctx context.Context, release *ReleaseAction) error
    ReleaseIPs(ctx context.Context, release *ReleaseAction) error
    GetMaximumAllocatableIPv4() int
    GetMinimumAllocatableIPv4() int
    IsPrefixDelegated() bool
}
```

### 메서드별 상세 설명

```
+----------------------------------+
|        NodeOperations            |
+----------------------------------+
|                                  |
| [라이프사이클]                     |
| UpdatedNode()                    |  CiliumNode 업데이트 수신
| PopulateStatusFields()           |  CiliumNode Status에 프로바이더별 정보 채움
|                                  |
| [동기화]                          |
| ResyncInterfacesAndIPs()         |  인터페이스/IP 목록 재동기화
|                                  |
| [할당]                            |
| PrepareIPAllocation()            |  할당 가능 IP 수 계산
| AllocateIPs()                    |  실제 IP 할당 실행
| AllocateStaticIP()               |  정적 IP (EIP/Public IP) 할당
| CreateInterface()                |  새 네트워크 인터페이스 생성
|                                  |
| [해제]                            |
| PrepareIPRelease()               |  해제 가능 IP 계산
| ReleaseIPs()                     |  IP 해제 실행
| ReleaseIPPrefixes()              |  IP 프리픽스 해제 실행
|                                  |
| [제한]                            |
| GetMaximumAllocatableIPv4()      |  최대 할당 가능 IPv4 수
| GetMinimumAllocatableIPv4()      |  최소 할당 IPv4 수
| IsPrefixDelegated()              |  프리픽스 위임 지원 여부
+----------------------------------+
```

### AWS vs Azure NodeOperations 구현 비교

| 메서드 | AWS (ENI Node) | Azure (Node) |
|--------|---------------|--------------|
| `CreateInterface` | ENI 생성 + 부착 + IP 할당 | `not implemented` 반환 |
| `ResyncInterfacesAndIPs` | ENI별 IP 수집, 용량 계산 | NIC별 IP 수집, VMSS명 캐시 |
| `PrepareIPAllocation` | ENI별 가용 IP + 서브넷 잔여 | NIC별 가용 IP + 서브넷 잔여 |
| `AllocateIPs` | PD모드: 프리픽스 할당, 일반: IP 할당 | VM/VMSS에 따라 분기 |
| `PrepareIPRelease` | ENI별 미사용 IP/프리픽스 탐색 | 빈 ReleaseAction 반환 |
| `ReleaseIPs` | EC2 API로 IP 해제 | `not implemented` 반환 |
| `ReleaseIPPrefixes` | EC2 API로 프리픽스 해제 | no-op |
| `GetMaximumAllocatableIPv4` | 인스턴스 타입별 제한 조회 | 고정값 256 |
| `GetMinimumAllocatableIPv4` | 인스턴스 타입별 제한 조회 | 기본값 (PreAllocation) |
| `IsPrefixDelegated` | 설정에 따라 true/false | 항상 false |
| `PopulateStatusFields` | `Status.ENI.ENIs[]` 채움 | `Status.Azure.Interfaces[]` 채움 |

---

## 6. Node Manager 오케스트레이션

### 6.1 NodeManager 구조체

**소스 경로**: `pkg/ipam/node_manager.go` (171~183행)

```go
type NodeManager struct {
    logger               *slog.Logger
    mutex                lock.RWMutex
    nodes                nodeMap
    instancesAPI         AllocationImplementation
    k8sAPI               CiliumNodeGetterUpdater
    metricsAPI           MetricsAPI
    parallelWorkers      int64
    releaseExcessIPs     bool
    excessIPReleaseDelay int
    stableInstancesAPI   bool
    prefixDelegation     bool
}
```

| 필드 | 용도 |
|------|------|
| `nodes` | 노드명 -> IPAM Node 매핑 |
| `instancesAPI` | 클라우드별 AllocationImplementation (InstancesManager) |
| `k8sAPI` | CiliumNode CRD CRUD 인터페이스 |
| `metricsAPI` | 메트릭 수집 인터페이스 |
| `parallelWorkers` | Resync 시 병렬 처리 워커 수 |
| `releaseExcessIPs` | 초과 IP 해제 활성화 (AWS만) |
| `excessIPReleaseDelay` | 초과 IP 해제 전 대기 시간 |
| `stableInstancesAPI` | 인스턴스 API 정상 여부 |
| `prefixDelegation` | 프리픽스 위임 활성화 (AWS만) |

### 6.2 AllocationImplementation 인터페이스

**소스 경로**: `pkg/ipam/node_manager.go` (108~136행)

```go
type AllocationImplementation interface {
    CreateNode(obj *v2.CiliumNode, node *Node) NodeOperations
    GetPoolQuota() ipamTypes.PoolQuotaMap
    Resync(ctx context.Context) time.Time
    InstanceSync(ctx context.Context, instanceID string) time.Time
    HasInstance(instanceID string) bool
    DeleteInstance(instanceID string)
}
```

이 인터페이스는 `InstancesManager`가 구현한다:

| 메서드 | 용도 |
|--------|------|
| `CreateNode` | 새 노드 발견 시 `NodeOperations` 구현체 생성 |
| `GetPoolQuota` | 서브넷별 가용 IP 수 반환 |
| `Resync` | 전체 인프라 상태 재동기화 |
| `InstanceSync` | 단일 인스턴스 상태 재동기화 |
| `HasInstance` | 인스턴스 존재 여부 확인 |
| `DeleteInstance` | 인스턴스 삭제 |

### 6.3 NodeManager.Start 메서드

**소스 경로**: `pkg/ipam/node_manager.go` (228~259행)

```go
func (n *NodeManager) Start(ctx context.Context) error {
    // 블로킹 최초 리싱크
    if _, ok := n.instancesAPIResync(ctx); !ok {
        return fmt.Errorf("Initial synchronization with instances API failed")
    }

    // 백그라운드 주기적 리싱크 (1분 간격)
    go func() {
        mngr := controller.NewManager()
        mngr.UpdateController("ipam-node-interval-refresh",
            controller.ControllerParams{
                Group:       ipamNodeIntervalControllerGroup,
                RunInterval: time.Minute,
                DoFunc: func(ctx context.Context) error {
                    start := time.Now()
                    syncTime, ok := n.instancesAPIResync(ctx)
                    if ok {
                        n.metricsAPI.ObserveBackgroundSync(success, time.Since(start))
                        n.Resync(ctx, syncTime)
                    } else {
                        n.metricsAPI.ObserveBackgroundSync(failed, time.Since(start))
                    }
                    return nil
                },
            })
    }()
    return nil
}
```

### 6.4 노드 Upsert 흐름

CiliumNode CRD가 생성/변경되면 `NodeManager.Upsert()`가 호출된다.

**소스 경로**: `pkg/ipam/node_manager.go` (291~398행)

```
Upsert(CiliumNode)
    |
    +-- 기존 노드? --> Yes --> node.UpdatedResource(resource)
    |
    +-- 새 노드 -->
        |
        +-- Node 구조체 생성
        |
        +-- 인스턴스 API에 해당 인스턴스 존재 확인
        |   (없으면 InstanceSync로 단일 인스턴스 리싱크)
        |
        +-- instancesAPI.CreateNode() 호출
        |   (NodeOperations 구현체 생성)
        |
        +-- 트리거 3개 생성:
        |   +--> pool-maintainer: IP 풀 유지보수
        |   +--> pool-maintainer-retry: 재시도 (1분 간격)
        |   +--> k8s-sync: CiliumNode Status 동기화
        |   +--> instance-sync: 인스턴스 상태 동기화
        |
        +-- n.nodes[name] = node
```

트리거(trigger) 시스템은 이벤트 기반으로 동작하되, 최소 실행 간격을 보장한다:

```
이벤트 발생 --> pool-maintainer 트리거 (10ms 최소 간격)
                    |
                    v
              MaintainIPPool()
                    |
            +-------+-------+
            |               |
         성공             실패
            |               |
            v               v
       k8s-sync        pool-maintainer-retry
       트리거              트리거 (1분 후 재시도)
            |
            v
       syncToAPIServer()
       (CiliumNode Status 갱신)
```

### 6.5 Resync 오케스트레이션

**소스 경로**: `pkg/ipam/node_manager.go` (537~567행)

```go
func (n *NodeManager) Resync(ctx context.Context, syncTime time.Time) {
    n.mutex.Lock()
    defer n.mutex.Unlock()
    n.metricsAPI.IncResyncCount()

    stats := resyncStats{}
    sem := semaphore.NewWeighted(n.parallelWorkers)

    for _, node := range n.GetNodesByIPWatermarkLocked() {
        err := sem.Acquire(ctx, 1)
        // ...
        go func(node *Node, stats *resyncStats) {
            n.resyncNode(ctx, node, stats, syncTime)
            sem.Release(1)
        }(node, &stats)
    }

    // 모든 고루틴 완료 대기
    sem.Acquire(ctx, n.parallelWorkers)

    // 글로벌 메트릭 갱신
    n.metricsAPI.SetAllocatedIPs("used", stats.ipv4.totalUsed)
    n.metricsAPI.SetAllocatedIPs("available", stats.ipv4.totalAvailable)
    n.metricsAPI.SetAllocatedIPs("needed", stats.ipv4.totalNeeded)
    n.metricsAPI.SetAvailableInterfaces(stats.ipv4.remainingInterfaces)
    // ...
}
```

핵심 설계:
- **세마포어 기반 병렬 처리**: `parallelWorkers` 수만큼 동시 실행
- **워터마크 기반 우선순위**: IP가 가장 부족한 노드부터 처리
- **완료 동기화**: 전체 세마포어 획득으로 모든 고루틴 완료 보장
- **글로벌 메트릭 집계**: 모든 노드 처리 후 한 번에 갱신

---

## 7. CiliumNode CRD

### 7.1 CiliumNode와 클라우드 상태

CiliumNode CRD는 각 노드의 IP 할당 상태를 쿠버네티스에 저장한다.
`PopulateStatusFields()`가 클라우드별 정보를 채운다.

**AWS ENI Status**:

**소스 경로**: `pkg/aws/eni/node.go` (110~121행)

```go
func (n *Node) PopulateStatusFields(k8sObj *v2.CiliumNode) {
    k8sObj.Status.ENI.ENIs = map[string]eniTypes.ENI{}
    n.manager.ForeachInstance(n.node.InstanceID(),
        func(instanceID, interfaceID string, rev ipamTypes.InterfaceRevision) error {
            e, ok := rev.Resource.(*eniTypes.ENI)
            if ok {
                k8sObj.Status.ENI.ENIs[interfaceID] = *e.DeepCopy()
            }
            return nil
        })
}
```

**Azure Status**:

**소스 경로**: `pkg/azure/ipam/node.go` (46~58행)

```go
func (n *Node) PopulateStatusFields(k8sObj *v2.CiliumNode) {
    k8sObj.Status.Azure.Interfaces = []types.AzureInterface{}
    n.manager.mutex.RLock()
    defer n.manager.mutex.RUnlock()
    n.manager.instances.ForeachInterface(n.node.InstanceID(),
        func(instanceID, interfaceID string,
            interfaceObj ipamTypes.InterfaceRevision) error {
            iface, ok := interfaceObj.Resource.(*types.AzureInterface)
            if ok {
                k8sObj.Status.Azure.Interfaces = append(
                    k8sObj.Status.Azure.Interfaces, *(iface.DeepCopy()))
            }
            return nil
        })
}
```

### 7.2 CiliumNode Status 구조

```yaml
apiVersion: cilium.io/v2
kind: CiliumNode
metadata:
  name: worker-node-1
spec:
  # AWS 전용 스펙
  eni:
    vpc-id: vpc-0abc123
    availability-zone: ap-northeast-2a
    instance-type: m5.large
    first-interface-index: 1
    subnet-ids: []
    subnet-tags: {}
    security-groups: []
  # Azure 전용 스펙
  azure:
    interface-name: ""
status:
  # 공통 IPAM 상태
  ipam:
    used:
      10.0.1.10:
        owner: default/my-pod
        resource: eni-0abc123
      10.0.1.11:
        owner: default/other-pod
        resource: eni-0abc123
    operator-status:
      error: ""
  # AWS 전용 상태
  eni:
    enis:
      eni-0abc123:
        id: eni-0abc123
        ip: 10.0.1.5
        mac: "02:ab:cd:ef:12:34"
        number: 1
        subnet:
          id: subnet-xyz
          cidr: 10.0.1.0/24
        security-groups:
          - sg-123
        addresses:
          - 10.0.1.10
          - 10.0.1.11
          - 10.0.1.12
  # Azure 전용 상태
  azure:
    interfaces:
      - id: /subscriptions/.../networkInterfaces/nic-1
        name: nic-1
        mac: "00-0D-3A-12-34-56"
        state: Succeeded
        addresses:
          - ip: 10.0.1.10
            subnet: /subscriptions/.../subnets/default
            state: Succeeded
```

### 7.3 CiliumNode ↔ NodeManager 상호작용

```
K8s API Server                NodeManager              Cloud API
      |                           |                       |
      |--CiliumNode Create------->|                       |
      |                           |  Upsert()             |
      |                           |  - CreateNode()       |
      |                           |  - 트리거 생성          |
      |                           |                       |
      |                           |  MaintainIPPool()     |
      |                           |-----ResyncInterfaces->|
      |                           |<----IP/인터페이스 목록--|
      |                           |                       |
      |                           |  [IP 부족 시]          |
      |                           |-----AllocateIPs------>|
      |                           |<----할당 결과----------|
      |                           |                       |
      |<--CiliumNode Status 갱신--|                       |
      |  (PopulateStatusFields)   |                       |
      |                           |                       |
      |--CiliumNode Update------->|                       |
      |                           |  Upsert()             |
      |                           |  - UpdatedResource()  |
      |                           |  - 트리거 재발동        |
```

---

## 8. 할당 메트릭

### 8.1 MetricsAPI 인터페이스

**소스 경로**: `pkg/ipam/node_manager.go` (138~158행)

```go
type MetricsAPI interface {
    MetricsNodeAPI

    AllocationAttempt(typ, status, subnetID string, observe float64)
    ReleaseAttempt(typ, status, subnetID string, observe float64)
    IncInterfaceAllocation(subnetID string)
    AddIPAllocation(subnetID string, allocated int64)
    AddIPRelease(subnetID string, released int64)
    SetAllocatedIPs(typ string, allocated int)
    SetAvailableInterfaces(available int)
    SetInterfaceCandidates(interfaceCandidates int)
    SetEmptyInterfaceSlots(emptyInterfaceSlots int)
    SetAvailableIPsPerSubnet(subnetID string, availabilityZone string, available int)
    SetNodes(category string, nodes int)
    IncResyncCount()
    ObserveBackgroundSync(status string, duration time.Duration)
    PoolMaintainerTrigger() trigger.MetricsObserver
    K8sSyncTrigger() trigger.MetricsObserver
    ResyncTrigger() trigger.MetricsObserver
}

type MetricsNodeAPI interface {
    SetIPAvailable(node string, cap int)
    SetIPUsed(node string, used int)
    SetIPNeeded(node string, needed int)
    DeleteNode(node string)
}
```

### 8.2 Prometheus 메트릭 목록

Cilium Operator가 노출하는 IPAM 관련 Prometheus 메트릭:

| 메트릭 이름 | 타입 | 레이블 | 설명 |
|------------|------|--------|------|
| `cilium_ipam_available_ips` | Gauge | `target_node` | 노드별 가용 IP 수 |
| `cilium_ipam_used_ips` | Gauge | `target_node` | 노드별 사용 중 IP 수 |
| `cilium_ipam_needed_ips` | Gauge | `target_node` | 노드별 필요 IP 수 |
| `cilium_ipam_ips` | Gauge | `type` | 전체 IP 수 (used/available/needed) |
| `cilium_ipam_available_interfaces` | Gauge | - | 전체 가용 인터페이스 수 |
| `cilium_ipam_interface_candidates` | Gauge | - | IP 할당 가능한 인터페이스 수 |
| `cilium_ipam_empty_interface_slots` | Gauge | - | 새 인터페이스 생성 가능 슬롯 수 |
| `cilium_ipam_ip_allocation_ops` | Counter | `subnet_id` | 서브넷별 IP 할당 횟수 |
| `cilium_ipam_ip_release_ops` | Counter | `subnet_id` | 서브넷별 IP 해제 횟수 |
| `cilium_ipam_interface_creation_ops` | Counter | `subnet_id` | 서브넷별 인터페이스 생성 횟수 |
| `cilium_ipam_allocation_duration_seconds` | Histogram | `type`, `status`, `subnet_id` | IP 할당 소요 시간 |
| `cilium_ipam_release_duration_seconds` | Histogram | `type`, `status`, `subnet_id` | IP 해제 소요 시간 |
| `cilium_ipam_nodes` | Gauge | `category` | 노드 수 (total/in-deficit) |
| `cilium_ipam_resync_total` | Counter | - | 리싱크 총 횟수 |
| `cilium_ipam_available_ips_per_subnet` | Gauge | `subnet_id`, `availability_zone` | 서브넷별 가용 IP 수 |

### 8.3 메트릭 갱신 시점

```
[Resync 완료 후]
NodeManager.Resync()
    |
    +-- 각 노드 resyncNode() 완료 후 stats 집계
    |
    +-- SetAllocatedIPs("used", totalUsed)
    +-- SetAllocatedIPs("available", totalAvailable)
    +-- SetAllocatedIPs("needed", totalNeeded)
    +-- SetAvailableInterfaces(remainingInterfaces)
    +-- SetInterfaceCandidates(interfaceCandidates)
    +-- SetEmptyInterfaceSlots(emptyInterfaceSlots)
    +-- SetNodes("total", nodes)
    +-- SetNodes("in-deficit", nodesInDeficit)

[IP 할당 시]
MaintainIPPool()
    |
    +-- AllocationAttempt(typ, status, subnetID, duration)
    +-- AddIPAllocation(subnetID, allocated)
    +-- IncInterfaceAllocation(subnetID)  (인터페이스 생성 시)

[IP 해제 시]
MaintainIPPool()
    |
    +-- ReleaseAttempt(typ, status, subnetID, duration)
    +-- AddIPRelease(subnetID, released)

[백그라운드 리싱크]
NodeManager.Start() -> 1분 간격 컨트롤러
    |
    +-- ObserveBackgroundSync(status, duration)
    +-- IncResyncCount()
```

### 8.4 EC2 API 메트릭

AWS의 경우 EC2 API 호출 자체에 대한 메트릭도 수집한다:

**소스 경로**: `pkg/ipam/allocator/aws/aws.go` (99~103행)

```go
if operatorOption.Config.EnableMetrics {
    aMetrics = apiMetrics.NewPrometheusMetrics(metrics.Namespace, "ec2", reg)
} else {
    aMetrics = &apiMetrics.NoOpMetrics{}
}
```

이는 EC2 API의 호출 횟수, 지연 시간, 에러율을 모니터링하여 API 쓰로틀링 문제를
진단하는 데 사용된다.

---

## 9. 왜 이 아키텍처인가?

### 9.1 왜 인터페이스 기반 추상화인가?

**질문**: AWS와 Azure가 완전히 다른 API를 사용하는데, 왜 공통 인터페이스를 정의하는가?

**답변**: `NodeOperations`와 `AllocationImplementation` 인터페이스를 통해 NodeManager는
클라우드 프로바이더를 전혀 모른 채 동작한다. 이 설계의 이점:

```
[인터페이스 없이]                    [인터페이스 사용]
NodeManager                        NodeManager
    |                                  |
    +--if AWS:                         +--NodeOperations.AllocateIPs()
    |    ec2.AssignPrivateIps()        |
    +--if Azure:                       프로바이더 추가 = 인터페이스 구현만
    |    azure.AssignPrivateIps()      NodeManager 변경 불필요
    +--if GCP:
    |    gce.AssignAliasIpRanges()
    ...
```

새 클라우드 프로바이더를 추가할 때 `NodeOperations`와 `AllocationImplementation`만
구현하면 된다. NodeManager, 메트릭 수집, CiliumNode 동기화 로직은 재사용된다.

### 9.2 왜 Operator에서 IP를 관리하는가?

**질문**: 각 노드의 Agent가 직접 클라우드 API를 호출하지 않는 이유는?

**답변**:

| 항목 | Agent에서 관리 | Operator에서 관리 (현재 설계) |
|------|--------------|--------------------------|
| API 호출 수 | 노드 수 x API 호출 = N^2 스케일 | 1개 Operator가 일괄 관리 |
| 자격 증명 | 모든 노드에 클라우드 자격 증명 필요 | Operator에만 필요 |
| Rate Limiting | 노드 간 조율 어려움 | 중앙에서 QPS 제어 |
| 전체 상태 | 각 노드가 부분 뷰만 보유 | 전체 VPC/서브넷 상태 파악 |
| 서브넷 선택 | 노드 로컬 정보로만 판단 | 전체 서브넷 가용 IP 기반 최적 선택 |

특히 AWS의 경우 EC2 API에 엄격한 Rate Limit이 있다. 100개 노드가 각각 API를 호출하면
쉽게 쓰로틀링에 걸리지만, Operator가 중앙에서 관리하면 `IPAMAPIQPSLimit`과
`IPAMAPIBurst` 설정으로 정밀하게 제어할 수 있다.

### 9.3 왜 이벤트 기반 + 주기적 리싱크를 병행하는가?

**질문**: 이벤트 기반 트리거만으로 충분하지 않은가?

**답변**: Cilium은 두 가지 동기화 메커니즘을 병행한다:

```
[이벤트 기반 트리거]                   [주기적 리싱크]
CiliumNode 업데이트 이벤트              1분 간격 컨트롤러
    |                                     |
    v                                     v
pool-maintainer 트리거                instancesAPIResync()
    |                                     |
    v                                     v
MaintainIPPool()                     NodeManager.Resync()
```

병행하는 이유:
1. **이벤트 누락 방어**: K8s Watch가 연결 끊김 등으로 이벤트를 놓칠 수 있다
2. **외부 상태 변경 감지**: AWS 콘솔이나 다른 도구로 ENI가 변경된 경우
3. **초과 IP 해제**: 주기적으로 사용하지 않는 IP를 감지하고 해제
4. **상태 수렴 보장**: 어떤 이유로든 상태가 어긋나면 주기적으로 수정

### 9.4 왜 Azure는 인터페이스 생성을 지원하지 않는가?

**질문**: AWS는 ENI를 동적으로 생성하는데 Azure는 왜 안 하는가?

**답변**: AWS와 Azure의 네트워크 인터페이스 모델이 근본적으로 다르기 때문이다:

```
[AWS ENI 모델]
EC2 Instance
    +-- eth0 (Primary ENI)       - VPC 서브넷에 연결
    +-- eth1 (Secondary ENI)     - 런타임에 동적 생성/부착/분리 가능
    +-- eth2 (Secondary ENI)     - 각 ENI는 독립적인 보안 그룹 가능
    ...

[Azure NIC 모델]
Azure VM / VMSS
    +-- NIC-1 (Primary)          - VM 생성 시 함께 생성
    +-- NIC-2 (Secondary)        - VM 생성 시 함께 생성
    ...                          - 런타임 동적 NIC 추가 미지원
                                 - 대신 NIC당 최대 256개 IP 할당 가능
```

Azure VM은 NIC가 VM 프로비저닝 시 고정되며, 런타임에 NIC를 추가/제거할 수 없다.
대신 기존 NIC에 IP를 추가하는 것으로 충분하다 (NIC당 최대 256개).

### 9.5 왜 세마포어 기반 병렬 처리인가?

**질문**: Resync에서 고루틴 풀 대신 세마포어를 사용하는 이유는?

**답변**: `semaphore.NewWeighted`를 사용하면:

1. **동시성 제한**: 정확히 N개의 고루틴만 동시 실행
2. **완료 동기화**: `sem.Acquire(ctx, n.parallelWorkers)`로 모든 작업 완료 대기
3. **코드 단순성**: 워커 풀 관리, 채널, WaitGroup 없이 간결한 구현
4. **클라우드 API 부하 제어**: 동시 API 호출 수를 직접 제한

```go
sem := semaphore.NewWeighted(n.parallelWorkers)
for _, node := range nodes {
    sem.Acquire(ctx, 1)     // 슬롯 확보 (블로킹)
    go func() {
        resyncNode(...)     // 노드 처리 (클라우드 API 호출 포함)
        sem.Release(1)      // 슬롯 반환
    }()
}
sem.Acquire(ctx, n.parallelWorkers)  // 전부 완료 대기
```

### 9.6 왜 CiliumNode CRD에 상태를 저장하는가?

**질문**: 클라우드 API에서 직접 조회하면 되는데 왜 CRD에 저장하는가?

**답변**:

1. **Agent 독립성**: 각 노드의 Agent는 자신의 CiliumNode만 읽으면 할당된 IP를 알 수 있다.
   클라우드 API 자격 증명이 불필요하다.
2. **가시성**: `kubectl get ciliumnodes -o yaml`로 모든 노드의 IP 할당 상태를 확인할 수 있다.
3. **이벤트 기반 반응**: CiliumNode 변경은 K8s Watch로 즉시 감지되어 Agent가 빠르게 반응한다.
4. **장애 복구**: Operator가 재시작되면 CiliumNode에서 마지막 알려진 상태를 복원할 수 있다.
5. **클라우드 API 부하 감소**: Agent가 매번 클라우드 API를 호출하지 않아도 된다.

---

## 10. 참고 파일 목록

### AWS ENI 관련

| 파일 경로 | 핵심 내용 |
|----------|---------|
| `pkg/ipam/allocator/aws/aws.go` | `AllocatorAWS` 구조체, `Init()`, `Start()` |
| `pkg/aws/eni/node.go` | ENI `Node` 구조체, `CreateInterface()`, `AllocateIPs()`, `PrepareIPAllocation()`, `ResyncInterfacesAndIPs()` |
| `pkg/aws/eni/instances.go` | `InstancesManager`, `EC2API` 인터페이스, `Resync()` |
| `pkg/aws/ec2/` | EC2 API 클라이언트 구현 |
| `pkg/aws/eni/limits/` | 인스턴스 타입별 ENI/IP 제한 |
| `pkg/aws/eni/types/` | ENI 타입 정의 |
| `pkg/aws/metadata/` | IMDS(Instance Metadata Service) 클라이언트 |

### Azure 관련

| 파일 경로 | 핵심 내용 |
|----------|---------|
| `pkg/ipam/allocator/azure/azure.go` | `AllocatorAzure` 구조체, `Init()`, `Start()` |
| `pkg/azure/ipam/node.go` | Azure `Node` 구조체, `AllocateIPs()`, `PrepareIPAllocation()`, `ResyncInterfacesAndIPs()` |
| `pkg/azure/ipam/instances.go` | `InstancesManager`, `AzureAPI` 인터페이스, `Resync()` |
| `pkg/azure/api/` | Azure API 클라이언트 구현 |
| `pkg/azure/types/` | Azure 인터페이스 타입 정의 |

### 공통 IPAM 관련

| 파일 경로 | 핵심 내용 |
|----------|---------|
| `pkg/ipam/node_manager.go` | `NodeManager`, `NodeOperations` 인터페이스, `AllocationImplementation` 인터페이스, `MetricsAPI` |
| `pkg/ipam/node.go` | IPAM `Node` (각 노드의 IP 풀 유지보수) |
| `pkg/ipam/types/` | 공통 IPAM 타입 (`InstanceMap`, `SubnetMap`, `PoolQuotaMap` 등) |
| `pkg/ipam/metrics/` | Prometheus 메트릭 구현 |
| `pkg/ipam/stats/` | 인터페이스 통계 구조체 |
| `pkg/ipam/allocator/` | Allocator 인터페이스 및 레지스트리 |
| `pkg/k8s/apis/cilium.io/v2/` | CiliumNode CRD 타입 정의 |
