# 15. Cilium 클라우드 프로바이더 통합

## 개요

Cilium은 AWS, Azure, Alibaba Cloud 등 주요 클라우드 프로바이더와 깊은 수준의 통합을 제공한다. 이 통합의 핵심은 **클라우드 네이티브 IPAM**(IP Address Management)으로, 각 클라우드의 네트워크 인터페이스 시스템을 활용하여 Pod에 IP를 할당하는 메커니즘이다.

전통적인 오버레이 네트워크 방식과 달리, 클라우드 프로바이더 IPAM은 각 클라우드의 네이티브 네트워크 인터페이스(AWS ENI, Azure NIC, Alibaba Cloud ENI)에서 직접 IP를 할당받아 Pod에 제공한다. 이를 통해 VPC 네이티브 라우팅이 가능해지고, 캡슐화 오버헤드 없이 높은 네트워크 성능을 달성할 수 있다.

### 아키텍처 개요

```
+----------------------------------------------+
|             cilium-operator                   |
|                                               |
|  +------------------+                         |
|  | NodeManager      |                         |
|  |  - Resync Loop   |                         |
|  |  - IP Allocation |                         |
|  +--------+---------+                         |
|           |                                   |
|  +--------v---------+  +-------------------+  |
|  | InstancesManager |  | Cloud API Client  |  |
|  |  - instances      |  |  - rate limiter   |  |
|  |  - subnets        |  |  - metrics        |  |
|  |  - vpcs           |  |  - pagination     |  |
|  +-------------------+  +--------+----------+  |
|                                  |              |
+----------------------------------------------+
                                   |
                    +--------------v--------------+
                    |    Cloud Provider API        |
                    |  (EC2 / Azure / AliCloud)    |
                    +-----------------------------+
```

### 핵심 파일 경로

| 구성 요소 | 파일 경로 |
|----------|----------|
| AWS EC2 클라이언트 | `pkg/aws/ec2/ec2.go` |
| AWS ENI 노드 관리 | `pkg/aws/eni/node.go` |
| AWS ENI 인스턴스 관리 | `pkg/aws/eni/instances.go` |
| AWS 메타데이터 | `pkg/aws/metadata/metadata.go` |
| AWS ENI 타입 | `pkg/aws/eni/types/types.go` |
| AWS ENI GC | `pkg/aws/eni/eni_gc.go` |
| Azure API 클라이언트 | `pkg/azure/api/api.go` |
| Azure IPAM 노드 | `pkg/azure/ipam/node.go` |
| Azure IPAM 인스턴스 관리 | `pkg/azure/ipam/instances.go` |
| Azure 메타데이터 | `pkg/azure/api/metadata.go` |
| Azure 타입 | `pkg/azure/types/types.go` |
| Alibaba Cloud API | `pkg/alibabacloud/api/api.go` |
| Alibaba Cloud ENI 노드 | `pkg/alibabacloud/eni/node.go` |
| Alibaba Cloud 인스턴스 관리 | `pkg/alibabacloud/eni/instances.go` |
| Alibaba Cloud 메타데이터 | `pkg/alibabacloud/metadata/metadata.go` |
| Alibaba Cloud ENI 타입 | `pkg/alibabacloud/eni/types/types.go` |
| Operator AWS IPAM | `operator/pkg/ipam/aws.go` |
| Operator Azure IPAM | `operator/pkg/ipam/azure.go` |
| Operator Alibaba IPAM | `operator/pkg/ipam/alibabacloud.go` |
| AWS Allocator | `pkg/ipam/allocator/aws/aws.go` |
| Azure Allocator | `pkg/ipam/allocator/azure/azure.go` |
| Alibaba Allocator | `pkg/ipam/allocator/alibabacloud/alibabacloud.go` |

---

## 1. AWS ENI 통합

### 1.1 ENI IPAM: 인터페이스 생성/관리, 보조 IP 할당

AWS에서 Cilium은 **Elastic Network Interface (ENI)** 를 직접 관리하여 Pod IP를 할당한다. 이는 VPC CNI와 유사한 접근 방식이지만, Cilium의 고급 네트워킹 기능(eBPF 기반 정책, 서비스 메시 등)과 결합된다.

#### ENI 라이프사이클

```
1. CreateNetworkInterface  --> ENI 생성 (서브넷, 보안 그룹 지정)
2. AttachNetworkInterface  --> EC2 인스턴스에 ENI 연결
3. ModifyNetworkInterface  --> deleteOnTermination 속성 설정
4. AssignPrivateIpAddresses --> 보조 IP 할당 (Pod용)
5. UnassignPrivateIpAddresses --> 보조 IP 해제
6. DeleteNetworkInterface  --> ENI 삭제 (GC)
```

#### Node 구조체 (`pkg/aws/eni/node.go`)

`Node`는 ENI가 연결된 Kubernetes 노드를 나타내는 핵심 구조체이다:

```go
// pkg/aws/eni/node.go
type Node struct {
    rootLogger *slog.Logger
    logger     atomic.Pointer[slog.Logger]
    node       ipamNodeActions       // IPAM 노드 인터페이스
    mutex      lock.RWMutex
    enis       map[string]eniTypes.ENI  // ENI ID -> ENI 매핑
    k8sObj     *v2.CiliumNode          // CiliumNode CRD
    manager    *InstancesManager       // EC2 인스턴스 매니저
    instanceID string
}
```

#### ENI 생성 (CreateInterface)

ENI 생성은 `Node.CreateInterface()` 메서드에서 수행된다. 이 과정은 다음과 같다:

1. **인스턴스 타입 제한 확인**: 인스턴스 타입별 ENI/IP 수 제한 확인
2. **적절한 서브넷 탐색**: VPC, AZ, 서브넷 태그/ID 기반 서브넷 선택
3. **보안 그룹 결정**: 명시적 지정 > 태그 기반 탐색 > eth0의 보안 그룹 상속
4. **ENI 생성 및 부착**: EC2 API를 통한 ENI 생성 -> 인스턴스 연결 -> deleteOnTermination 설정

```go
// pkg/aws/eni/node.go - CreateInterface 핵심 로직
func (n *Node) CreateInterface(ctx context.Context, allocation *ipam.AllocationAction, scopedLog *slog.Logger) (int, string, error) {
    // 1. 인스턴스 제한 확인
    limits, limitsAvailable := n.getLimits()

    // 2. 적절한 서브넷 찾기
    subnet := n.findSuitableSubnet(resource.Spec.ENI, limits)

    // 3. 보안 그룹 ID 가져오기
    securityGroupIDs, err := n.getSecurityGroupIDs(ctx, resource.Spec.ENI)

    // 4. ENI 생성
    eniID, eni, err := n.manager.ec2api.CreateNetworkInterface(
        ctx, int32(toAllocate), subnet.ID, desc, securityGroupIDs, isPrefixDelegated)

    // 5. ENI를 인스턴스에 연결 (재시도 로직 포함)
    for range maxAttachRetries {
        attachmentID, err = n.manager.ec2api.AttachNetworkInterface(
            ctx, index, n.node.InstanceID(), eniID)
        if !isAttachmentIndexConflict(err) {
            break
        }
        index = n.findNextIndex(index + 1)
    }

    // 6. deleteOnTermination 설정
    err = n.manager.ec2api.ModifyNetworkInterface(ctx, eniID, attachmentID, true)

    return toAllocate, "", nil
}
```

#### 보조 IP 할당 (AllocateIPs)

기존 ENI에 보조 IP를 추가 할당하는 로직:

```go
// pkg/aws/eni/node.go
func (n *Node) AllocateIPs(ctx context.Context, a *ipam.AllocationAction) error {
    if isPrefixDelegated {
        // 프리픽스 위임: /28 프리픽스 단위로 할당 (16개 IP)
        numPrefixes := ip.PrefixCeil(a.IPv4.AvailableForAllocation, option.ENIPDBlockSizeIPv4)
        err := n.manager.ec2api.AssignENIPrefixes(ctx, a.InterfaceID, int32(numPrefixes))
    }
    // 개별 IP 할당
    assignedIPs, err := n.manager.ec2api.AssignPrivateIpAddresses(
        ctx, a.InterfaceID, int32(a.IPv4.AvailableForAllocation))
    n.manager.AddIPsToENI(n.node.InstanceID(), a.InterfaceID, assignedIPs)
    return nil
}
```

#### 프리픽스 위임 (Prefix Delegation)

AWS는 개별 IP 대신 `/28` 프리픽스(16개 IP)를 ENI에 할당할 수 있다. 이를 통해 API 호출 수를 줄이고 확장성을 높인다:

```go
// pkg/aws/eni/node.go
func (n *Node) IsPrefixDelegated() bool {
    if !n.isPrefixDelegationEnabled() {
        return false
    }
    limits, limitsAvailable := n.getLimitsLocked()
    if !limitsAvailable {
        return false
    }
    // Nitro 또는 베어메탈 인스턴스에서만 지원
    if limits.HypervisorType != "nitro" && !limits.IsBareMetal {
        return false
    }
    // 노드별 비활성화 설정 확인
    if n.k8sObj.Spec.ENI.DisablePrefixDelegation != nil &&
       aws.ToBool(n.k8sObj.Spec.ENI.DisablePrefixDelegation) {
        return false
    }
    return true
}
```

### 1.2 EC2 메타데이터 서비스 (IMDS) 활용

Cilium은 EC2 Instance Metadata Service (IMDS)를 통해 인스턴스의 기본 정보를 수집한다.

```go
// pkg/aws/metadata/metadata.go
type MetaDataInfo struct {
    InstanceID       string
    InstanceType     string
    AvailabilityZone string
    VPCID            string
    SubnetID         string
}

func (m *metadataClient) GetInstanceMetadata(ctx context.Context) (MetaDataInfo, error) {
    instanceID, _ := getMetadata(ctx, m.client, "instance-id")
    instanceType, _ := getMetadata(ctx, m.client, "instance-type")
    eth0MAC, _ := getMetadata(ctx, m.client, "mac")
    vpcID, _ := getMetadata(ctx, m.client,
        fmt.Sprintf("network/interfaces/macs/%s/vpc-id", eth0MAC))
    subnetID, _ := getMetadata(ctx, m.client,
        fmt.Sprintf("network/interfaces/macs/%s/subnet-id", eth0MAC))
    availabilityZone, _ := getMetadata(ctx, m.client, "placement/availability-zone")

    return MetaDataInfo{...}, nil
}
```

IMDS를 통해 수집하는 정보:
- `instance-id`: 인스턴스 고유 식별자 (예: `i-0123456789abcdef0`)
- `instance-type`: 인스턴스 타입 (예: `m5.large`) - ENI/IP 제한 결정에 사용
- `mac`: 기본 네트워크 인터페이스의 MAC 주소 - VPC/서브넷 정보 조회에 사용
- `network/interfaces/macs/{mac}/vpc-id`: VPC ID
- `network/interfaces/macs/{mac}/subnet-id`: 서브넷 ID
- `placement/availability-zone`: 가용 영역

### 1.3 aws-sdk-go-v2 사용

Cilium은 `aws-sdk-go-v2`를 사용하여 EC2 API와 통신한다.

#### 클라이언트 초기화

```go
// pkg/aws/ec2/ec2.go
func NewConfig(ctx context.Context) (aws.Config, error) {
    cfg, err := awsconfig.LoadDefaultConfig(ctx)

    // IMDS를 통한 리전 자동 감지
    metadataClient := imds.NewFromConfig(cfg)
    instance, err := metadataClient.GetInstanceIdentityDocument(ctx,
        &imds.GetInstanceIdentityDocumentInput{})
    cfg.Region = instance.Region

    // Cilium 자체 rate limiting 사용 (AWS SDK 내장 rate limiting 비활성화)
    cfg.Retryer = func() aws.Retryer {
        return retry.NewStandard(func(o *retry.StandardOptions) {
            o.RateLimiter = ratelimit.None
        })
    }
    return cfg, nil
}
```

#### Rate Limiting

EC2 API 호출에 대한 자체적인 rate limiting을 구현한다:

```go
// pkg/aws/ec2/ec2.go
type Client struct {
    logger              *slog.Logger
    ec2Client           *ec2.Client
    limiter             *helpers.APILimiter  // 자체 rate limiter
    metricsAPI          MetricsAPI           // API 호출 메트릭
    subnetsFilters      []ec2_types.Filter
    eniTagSpecification ec2_types.TagSpecification
    usePrimary          bool
    maxResultsPerCall   int32
}
```

모든 API 호출에 앞서 `c.limiter.Limit(ctx, operationName)`을 호출하여 속도 제한을 적용한다. 또한 API 호출 시간을 측정하여 메트릭으로 노출한다:

```go
c.limiter.Limit(ctx, AssignPrivateIpAddresses)
sinceStart := spanstat.Start()
output, err := c.ec2Client.AssignPrivateIpAddresses(ctx, input)
c.metricsAPI.ObserveAPICall(AssignPrivateIpAddresses, deriveStatus(err), sinceStart.Seconds())
```

#### 자동 페이지네이션 전환

대규모 환경에서 `OperationNotPermitted` 오류가 발생하면 자동으로 페이지네이션 모드로 전환한다:

```go
// pkg/aws/ec2/ec2.go
func (c *Client) switchToPagination(err error) bool {
    if !isOperationNotPermitted(err) {
        return false
    }
    if c.maxResultsPerCall > 0 {
        return false
    }
    c.maxResultsPerCall = 1000
    return true
}
```

### 1.4 pkg/aws/ 패키지 구조

```
pkg/aws/
├── ec2/
│   ├── ec2.go          # EC2 API 클라이언트 (ENI CRUD, 서브넷, VPC, 보안 그룹)
│   ├── ec2_test.go
│   └── mock/           # 테스트용 mock
├── eni/
│   ├── doc.go
│   ├── eni_gc.go       # ENI 가비지 컬렉션 (분리된 ENI 정리)
│   ├── instances.go    # InstancesManager (인스턴스/ENI 캐시 관리)
│   ├── limits/         # 인스턴스 타입별 ENI/IP 제한
│   ├── node.go         # Node (ENI 할당/해제 로직)
│   └── types/
│       └── types.go    # ENI, ENISpec, ENIStatus 타입 정의
├── metadata/
│   ├── metadata.go     # IMDS 클라이언트 (인스턴스 메타데이터 수집)
│   └── mock/
└── types/
    └── types.go        # SecurityGroup, SecurityGroupMap
```

### 1.5 ENI 가비지 컬렉션

분리된 ENI를 자동으로 정리하는 GC 메커니즘이 있다 (`pkg/aws/eni/eni_gc.go`):

```go
// 2단계 GC: 첫 번째 실행에서 마킹, 두 번째 실행에서 삭제
// - 분리된 ENI를 태그로 필터링하여 발견
// - 한 번의 GC 사이클을 기다린 후 삭제 (race condition 방지)
func StartENIGarbageCollector(ctx context.Context, logger *slog.Logger,
    api EC2API, params GarbageCollectionParams) {
    // ...
    DoFunc: func(ctx context.Context) error {
        // 이전 사이클에서 마킹된 ENI 삭제
        for _, eniID := range enisMarkedForDeletion {
            api.DeleteNetworkInterface(ctx, eniID)
        }
        // 현재 분리된 ENI를 마킹
        enisMarkedForDeletion, _ = api.GetDetachedNetworkInterfaces(
            ctx, params.ENITags, params.MaxPerInterval)
    }
}
```

---

## 2. Azure 통합

### 2.1 Azure NIC IPAM

Azure 통합은 AWS와 근본적으로 다른 접근 방식을 취한다. Azure에서는 새로운 NIC를 동적으로 생성하지 않고, **기존 NIC에 IP 구성을 추가**하는 방식으로 동작한다.

#### 핵심 차이점

```
AWS:   새 ENI 생성 -> 인스턴스에 부착 -> 보조 IP 할당
Azure: 기존 NIC에 -> IP Configuration 추가 (CreateInterface는 미구현)
```

```go
// pkg/azure/ipam/node.go
func (n *Node) CreateInterface(ctx context.Context, ...) (int, string, error) {
    return 0, "", fmt.Errorf("not implemented")  // Azure에서는 NIC 동적 생성 불가
}

func (n *Node) AllocateIPs(ctx context.Context, a *ipam.AllocationAction) error {
    iface := a.Interface.Resource.(*types.AzureInterface)
    if iface.GetVMScaleSetName() == "" {
        // 일반 VM: NIC에 직접 IP 추가
        return n.manager.api.AssignPrivateIpAddressesVM(
            ctx, string(a.PoolID), iface.Name, a.IPv4.AvailableForAllocation)
    } else {
        // VMSS VM: 스케일 셋 API를 통한 IP 추가
        return n.manager.api.AssignPrivateIpAddressesVMSS(
            ctx, iface.GetVMID(), iface.GetVMScaleSetName(),
            string(a.PoolID), iface.Name, a.IPv4.AvailableForAllocation)
    }
}
```

#### AzureInterface 타입

```go
// pkg/azure/types/types.go
type AzureInterface struct {
    ID            string          // ARM 리소스 ID
    Name          string          // 인터페이스 이름
    MAC           string
    State         string          // 프로비저닝 상태
    Addresses     []AzureAddress  // IP 주소 목록
    SecurityGroup string          // NSG (네트워크 보안 그룹)
    Gateway       string          // 서브넷 기본 게이트웨이
    CIDR          string          // 서브넷 CIDR
    vmssName      string          // VMSS 이름 (ARM ID에서 추출)
    vmID          string          // VM ID (ARM ID에서 추출)
    resourceGroup string          // 리소스 그룹
}

type AzureAddress struct {
    IP     string  // IP 주소
    Subnet string  // 서브넷 이름
    State  string  // "succeeded" 이면 사용 가능
}
```

#### 최대 IP 한도

Azure VM/NIC당 최대 256개의 IP 주소를 할당할 수 있다:

```go
// pkg/azure/types/types.go
const InterfaceAddressLimit = 256  // Azure NIC당 최대 주소 수
```

### 2.2 Azure IMDS (Instance Metadata Service)

Azure의 IMDS는 `169.254.169.254`에서 접근 가능하다:

```go
// pkg/azure/api/metadata.go
const (
    metadataURL        = "http://169.254.169.254/metadata"
    metadataAPIVersion = "2019-06-01"
)

// IMDS를 통해 수집하는 정보:
func GetSubscriptionID(ctx context.Context, logger *slog.Logger) (string, error) {
    return getMetadataString(ctx, logger, "instance/compute/subscriptionId")
}

func GetResourceGroupName(ctx context.Context, logger *slog.Logger) (string, error) {
    return getMetadataString(ctx, logger, "instance/compute/resourceGroupName")
}

func GetAzureCloudName(ctx context.Context, logger *slog.Logger) (string, error) {
    return getMetadataString(ctx, logger, "instance/compute/azEnvironment")
}
```

IMDS 요청 시 `Metadata: true` 헤더와 `api-version` 쿼리 파라미터가 필수이다:

```go
func getMetadataString(ctx context.Context, logger *slog.Logger, path string) (string, error) {
    req.Header.Add("Metadata", "true")
    query.Add("api-version", metadataAPIVersion)
    query.Add("format", "text")
}
```

### 2.3 azure-sdk-for-go 사용

Cilium은 최신 Azure SDK 트랙을 사용한다:

```go
// pkg/azure/api/api.go 의 import
import (
    "github.com/Azure/azure-sdk-for-go/sdk/azcore"
    "github.com/Azure/azure-sdk-for-go/sdk/azidentity"
    "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
    "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v8"
)
```

#### 클라이언트 구성

```go
// pkg/azure/api/api.go
type Client struct {
    logger                    *slog.Logger
    subscriptionID            string
    resourceGroup             string
    interfaces                *armnetwork.InterfacesClient
    publicIPPrefixes          *armnetwork.PublicIPPrefixesClient
    virtualNetworks           *armnetwork.VirtualNetworksClient
    virtualMachines           *armcompute.VirtualMachinesClient
    subnets                   *armnetwork.SubnetsClient
    virtualMachineScaleSetVMs *armcompute.VirtualMachineScaleSetVMsClient
    virtualMachineScaleSets   *armcompute.VirtualMachineScaleSetsClient
    limiter                   *helpers.APILimiter
    metricsAPI                MetricsAPI
    usePrimary                bool
}
```

#### 클라우드 환경 지원

```go
func newClientOptions(cloudName string) (*azcore.ClientOptions, error) {
    switch cloudName {
    case "AzurePublicCloud":
        clientOptions.Cloud = cloud.AzurePublic
    case "AzureUSGovernmentCloud":
        clientOptions.Cloud = cloud.AzureGovernment
    case "AzureChinaCloud":
        clientOptions.Cloud = cloud.AzureChina
    }
}
```

#### 인증

Azure는 Managed Identity 또는 Default Azure Credential을 지원한다:

```go
func newTokenCredential(clientOptions *azcore.ClientOptions, userAssignedIdentityID string) (azcore.TokenCredential, error) {
    if userAssignedIdentityID != "" {
        return azidentity.NewManagedIdentityCredential(...)  // User-Assigned Managed Identity
    }
    return azidentity.NewDefaultAzureCredential(...)  // 기본 인증 체인
}
```

### 2.4 3단계 리싱크 최적화

Azure의 `InstancesManager.Resync()`는 3단계 전략으로 최적화되어 있다:

```go
// pkg/azure/ipam/instances.go
func (m *InstancesManager) resyncInstances(ctx context.Context) time.Time {
    // Phase 1: 네트워크 인터페이스를 Azure API에서 한 번만 가져옴
    networkInterfaces, _ := m.api.ListAllNetworkInterfaces(ctx)

    // Phase 2: 빈 서브넷 맵으로 파싱하여 사용 중인 서브넷 ID 파악
    instances := m.api.ParseInterfacesIntoInstanceMap(networkInterfaces, ipamTypes.SubnetMap{})
    subnetIDs := m.extractSubnetIDs(instances)

    // Phase 3: 실제 사용 중인 서브넷만 쿼리
    subnets, _ := m.api.GetSubnetsByIDs(ctx, subnetIDs)

    // Phase 4: 동일한 네트워크 인터페이스 데이터를 서브넷 정보와 함께 재파싱
    instances = m.api.ParseInterfacesIntoInstanceMap(networkInterfaces, subnets)
}
```

이 방식의 장점:
- Azure API 호출 횟수 최소화 (네트워크 인터페이스를 한 번만 조회)
- 사용 중인 서브넷만 선택적으로 쿼리
- 메모리 내 파싱은 빠르므로 두 번 수행해도 부담 없음

### 2.5 pkg/azure/ 패키지 구조

```
pkg/azure/
├── api/
│   ├── api.go         # Azure API 클라이언트 (NIC, VM, VMSS, VNet, Subnet)
│   ├── metadata.go    # Azure IMDS (subscriptionId, resourceGroup, cloudName)
│   └── mock/          # 테스트용 mock
├── ipam/
│   ├── doc.go
│   ├── instances.go   # InstancesManager (3단계 리싱크 최적화)
│   └── node.go        # Node (IP 할당, VM/VMSS 분기 처리)
└── types/
    └── types.go       # AzureInterface, AzureAddress, AzureSpec
```

---

## 3. Alibaba Cloud 통합

### 3.1 ENI IPAM

Alibaba Cloud의 ENI IPAM은 AWS와 유사한 패턴을 따르지만, 몇 가지 중요한 차이점이 있다.

#### 핵심 차이점

| 항목 | AWS | Alibaba Cloud |
|------|-----|---------------|
| 서브넷 | Subnet | VSwitch |
| 프리픽스 위임 | 지원 | 미지원 |
| ENI 타입 구분 | 인덱스 기반 | Primary/Secondary 타입 필드 |
| 최초 IP 할당 | 제한 없음 | `maxENIIPCreate = 10` |
| ENI 인덱스 관리 | 자동 인덱스 | 태그 기반 인덱스 |
| ENI 부착 확인 | 즉시 | 폴링 기반 (`WaitENIAttached`) |

#### ENI 타입

```go
// pkg/alibabacloud/eni/types/types.go
const (
    ENITypePrimary   string = "Primary"    // 기본 ENI (eth0)
    ENITypeSecondary string = "Secondary"  // Cilium이 관리하는 보조 ENI
)

type ENI struct {
    NetworkInterfaceID string
    MACAddress         string
    Type               string           // Primary 또는 Secondary
    InstanceID         string
    SecurityGroupIDs   []string
    VPC                VPC
    ZoneID             string
    VSwitch            VSwitch
    PrimaryIPAddress   string
    PrivateIPSets      []PrivateIPSet   // 모든 IP (Primary IP 포함)
    Tags               map[string]string
}
```

#### ENI 생성 로직

```go
// pkg/alibabacloud/eni/node.go
func (n *Node) CreateInterface(ctx context.Context, allocation *ipam.AllocationAction,
    scopedLog *slog.Logger) (int, string, error) {

    // 인스턴스 제한 확인
    l, limitsAvailable := n.getLimits()

    // 할당할 IP 수 결정 (최초 생성 시 최대 10개)
    toAllocate := min(allocation.IPv4.MaxIPsToAllocate, l.IPv4)
    toAllocate = min(maxENIIPCreate, toAllocate)

    // 적합한 VSwitch 탐색
    bestSubnet := n.manager.FindOneVSwitch(resource.Spec.AlibabaCloud, toAllocate)

    // ENI 인덱스 할당 (태그 기반)
    index, err := n.allocENIIndex()

    // ENI 생성 (toAllocate-1: primary IP 제외)
    eniID, eni, err := n.manager.api.CreateNetworkInterface(
        ctx, toAllocate-1, bestSubnet.ID, securityGroupIDs,
        utils.FillTagWithENIIndex(map[string]string{}, index))

    // ENI 부착 및 부착 완료 대기
    err = n.manager.api.AttachNetworkInterface(ctx, instanceID, eniID)
    _, err = n.manager.api.WaitENIAttached(ctx, eniID)
}
```

#### WaitENIAttached 메커니즘

AWS와 달리 Alibaba Cloud는 ENI 부착을 비동기로 처리하므로, 폴링으로 완료를 확인한다:

```go
// pkg/alibabacloud/api/api.go
func (c *Client) WaitENIAttached(ctx context.Context, eniID string) (string, error) {
    err := wait.ExponentialBackoffWithContext(ctx, maxAttachRetries,
        func(ctx context.Context) (done bool, err error) {
            eni, err := c.DescribeNetworkInterface(ctx, eniID)
            if eni.Status == "InUse" {
                instanceID = eni.InstanceId
                return true, nil
            }
            return false, nil
        })
    return instanceID, nil
}

var maxAttachRetries = wait.Backoff{
    Duration: 2500 * time.Millisecond,
    Factor:   1,
    Jitter:   0.1,
    Steps:    6,
}
```

### 3.2 alibaba-cloud-sdk-go

```go
// pkg/alibabacloud/api/api.go
import (
    httperr "github.com/aliyun/alibaba-cloud-sdk-go/sdk/errors"
    "github.com/aliyun/alibaba-cloud-sdk-go/services/ecs"
    "github.com/aliyun/alibaba-cloud-sdk-go/services/vpc"
)

type Client struct {
    vpcClient        *vpc.Client
    ecsClient        *ecs.Client
    limiter          *helpers.APILimiter
    metricsAPI       MetricsAPI
    instancesFilters map[string]string
}
```

#### VPC 내부 네트워크 엔드포인트 사용

퍼블릭 인터넷 접근 없이 API를 호출할 수 있도록 VPC 내부 엔드포인트를 사용한다:

```go
// pkg/ipam/allocator/alibabacloud/alibabacloud.go
vpcClient.Network = "vpc"    // vpc 내부 엔드포인트 사용
ecsClient.Network = "vpc"
vpcClient.GetConfig().WithScheme("HTTPS")
ecsClient.GetConfig().WithScheme("HTTPS")
```

#### 인스턴스 태그 기반 필터링 최적화

대규모 클러스터에서의 성능을 위해 3단계 병렬 조회를 수행한다:

```
1. ListTagResources  --> 태그로 필터링된 인스턴스 ID 목록
2. DescribeInstances --> 인스턴스별 ENI ID 수집 (100개씩 병렬)
3. DescribeNetworkInterfaces --> ENI 상세 정보 (100개씩 병렬)
```

### 3.3 pkg/alibabacloud/ 패키지 구조

```
pkg/alibabacloud/
├── api/
│   ├── api.go         # ECS/VPC API 클라이언트
│   └── mock/          # 테스트용 mock
├── eni/
│   ├── instances.go   # InstancesManager (VSwitch, 보안 그룹 관리)
│   ├── limits/
│   │   └── limits.go  # 인스턴스 타입별 ENI/IP 제한
│   ├── node.go        # Node (ENI 생성/부착, IP 할당/해제)
│   └── types/
│       └── types.go   # ENI, Spec, VPC, VSwitch 타입
├── metadata/
│   └── metadata.go    # 인스턴스 메타데이터 (100.100.100.200)
├── types/
│   └── types.go       # SecurityGroup
└── utils/
    └── utils.go       # ENI 인덱스 태그 유틸리티
```

### 3.4 메타데이터 서비스

Alibaba Cloud의 메타데이터 서비스는 `100.100.100.200`에서 접근한다:

```go
// pkg/alibabacloud/metadata/metadata.go
const metadataURL = "http://100.100.100.200/latest/meta-data"

func GetInstanceID(ctx context.Context) (string, error) {
    return getMetadata(ctx, "instance-id")
}
func GetInstanceType(ctx context.Context) (string, error) {
    return getMetadata(ctx, "instance/instance-type")
}
func GetRegionID(ctx context.Context) (string, error) {
    return getMetadata(ctx, "region-id")
}
func GetZoneID(ctx context.Context) (string, error) {
    return getMetadata(ctx, "zone-id")
}
func GetVPCID(ctx context.Context) (string, error) {
    return getMetadata(ctx, "vpc-id")
}
```

---

## 4. 클라우드별 IPAM 동작 차이

### 4.1 전체 비교

| 기능 | AWS ENI | Azure NIC | Alibaba Cloud ENI |
|------|---------|-----------|-------------------|
| **IPAM 모드** | `eni` | `azure` | `alibabacloud` |
| **인터페이스 생성** | 동적 ENI 생성 | 미지원 (기존 NIC 사용) | 동적 ENI 생성 |
| **IP 할당** | 보조 IP / 프리픽스 | IP Configuration 추가 | 보조 IP |
| **프리픽스 위임** | 지원 (/28) | 미지원 | 미지원 |
| **IP 해제** | 지원 | 미지원 | 지원 |
| **최대 IP/NIC** | 인스턴스 타입 의존 | 256 | 인스턴스 타입 의존 |
| **메타데이터 URL** | `169.254.169.254` | `169.254.169.254` | `100.100.100.200` |
| **SDK** | aws-sdk-go-v2 | azure-sdk-for-go | alibaba-cloud-sdk-go |
| **인증** | IAM Role/IMDS | Managed Identity | Provider Chain |
| **ENI GC** | 지원 | 없음 | 없음 |
| **VMSS 지원** | 해당 없음 | 지원 (VM + VMSS) | 해당 없음 |
| **인스턴스 타입 조회** | DescribeInstanceTypes | Azure IMDS | DescribeInstanceTypes |

### 4.2 IPAM 할당 흐름 비교

#### AWS

```
PrepareIPAllocation --> 기존 ENI에서 가용 IP 확인
                    --> 새 ENI 필요 시 CreateInterface 호출
AllocateIPs        --> AssignPrivateIpAddresses 또는 AssignENIPrefixes

ReleaseIPs         --> UnassignPrivateIpAddresses
                    --> UnassignENIPrefixes (프리픽스 위임 시)
```

#### Azure

```
PrepareIPAllocation --> 지정된 NIC에서 가용 IP 확인 (InterfaceAddressLimit - 현재 수)
                    --> 서브넷 가용 주소 확인
AllocateIPs        --> AssignPrivateIpAddressesVM 또는 AssignPrivateIpAddressesVMSS

ReleaseIPs         --> 미구현 (fmt.Errorf("not implemented"))
```

#### Alibaba Cloud

```
PrepareIPAllocation --> Secondary ENI에서 가용 IP 확인
                    --> 새 ENI 필요 시 CreateInterface 호출
AllocateIPs        --> AssignPrivateIPAddresses

ReleaseIPs         --> UnassignPrivateIPAddresses
```

### 4.3 인스턴스 리싱크 전략 비교

세 프로바이더 모두 `Resync()` (전체 동기화)와 `InstanceSync()` (단일 인스턴스 동기화)를 지원한다:

- **Resync()**: `resyncLock.Lock()` - 전체 API 리싱크, 모든 증분 리싱크를 차단
- **InstanceSync()**: `resyncLock.RLock()` - 증분 리싱크, 다른 증분 리싱크와 병렬 실행 가능

---

## 5. 메타데이터 서비스 활용 패턴

### 5.1 메타데이터 서비스 비교

| 항목 | AWS IMDS | Azure IMDS | Alibaba Cloud |
|------|----------|------------|---------------|
| **URL** | `169.254.169.254` | `169.254.169.254` | `100.100.100.200` |
| **SDK 활용** | aws-sdk-go-v2 imds 패키지 | 직접 HTTP 클라이언트 | 직접 HTTP 클라이언트 |
| **필수 헤더** | SDK가 처리 | `Metadata: true` | 없음 |
| **API 버전** | SDK가 처리 | `api-version=2019-06-01` | 없음 |
| **수집 정보** | instance-id, type, AZ, VPC, subnet | subscriptionId, resourceGroup, cloudName | instance-id, type, region, zone, vpc-id |

### 5.2 활용 시점

메타데이터 서비스는 주로 **operator 초기화** 시점에 사용된다:

```
Operator 시작
    |
    +-- AWS: metadata.GetInstanceMetadata() --> VPC ID 확인
    |       ec2.NewConfig() --> 리전 자동 감지
    |
    +-- Azure: GetSubscriptionID() --> 구독 ID (CLI 미지정 시)
    |          GetResourceGroupName() --> 리소스 그룹 (CLI 미지정 시)
    |          GetAzureCloudName() --> 클라우드 환경 (Public/Gov/China)
    |
    +-- Alibaba: metadata.GetRegionID() --> API 클라이언트 초기화용
```

---

## 6. 보안 그룹/네트워크 보안 그룹 연동

### 6.1 AWS 보안 그룹

ENI 생성 시 보안 그룹을 결정하는 3단계 폴백 전략:

```go
// pkg/aws/eni/node.go
func (n *Node) getSecurityGroupIDs(ctx context.Context, eniSpec eniTypes.ENISpec) ([]string, error) {
    // 1순위: CiliumNode에 명시적으로 지정된 보안 그룹
    if len(eniSpec.SecurityGroups) > 0 {
        return eniSpec.SecurityGroups, nil
    }

    // 2순위: 태그 기반 보안 그룹 탐색
    if len(eniSpec.SecurityGroupTags) > 0 {
        securityGroups := n.manager.FindSecurityGroupByTags(
            eniSpec.VpcID, eniSpec.SecurityGroupTags)
        if len(securityGroups) > 0 {
            return groups, nil
        }
    }

    // 3순위: eth0(기본 ENI)의 보안 그룹 상속
    n.manager.ForeachInstance(n.node.InstanceID(),
        func(instanceID, interfaceID string, rev ipamTypes.InterfaceRevision) error {
            e, ok := rev.Resource.(*eniTypes.ENI)
            if ok && e.Number == 0 {  // eth0
                securityGroups = e.SecurityGroups
            }
            return nil
        })
    return securityGroups, nil
}
```

### 6.2 Azure 네트워크 보안 그룹 (NSG)

Azure에서 NSG는 NIC 또는 서브넷 수준에서 연결된다. Cilium은 `AzureInterface.SecurityGroup` 필드로 NSG를 추적한다:

```go
// pkg/azure/types/types.go
type AzureInterface struct {
    SecurityGroup string  // NSG 연결
    // ...
}
```

### 6.3 Alibaba Cloud 보안 그룹

AWS와 동일한 3단계 폴백 패턴을 사용하지만, Primary ENI 타입 기반으로 구분한다:

```go
// pkg/alibabacloud/eni/node.go
func (n *Node) getSecurityGroupIDs(ctx context.Context, eniSpec eniTypes.Spec) ([]string, error) {
    // 1순위: 명시적 지정
    if len(eniSpec.SecurityGroups) > 0 {
        return eniSpec.SecurityGroups, nil
    }
    // 2순위: 태그 기반 탐색
    if len(eniSpec.SecurityGroupTags) > 0 { ... }
    // 3순위: Primary ENI의 보안 그룹 상속
    n.manager.ForeachInstance(n.node.InstanceID(),
        func(instanceID, interfaceID string, rev ipamTypes.InterfaceRevision) error {
            e, ok := rev.Resource.(*eniTypes.ENI)
            if ok && e.Type == eniTypes.ENITypePrimary {  // Primary 타입으로 구분
                securityGroups = e.SecurityGroupIDs
            }
            return nil
        })
}
```

---

## 7. Operator 통합

### 7.1 Hive Cell 기반 모듈화

각 클라우드 프로바이더의 IPAM allocator는 Hive Cell 모듈로 구현된다:

```go
// operator/pkg/ipam/aws.go
func init() {
    allocators = append(allocators, cell.Module(
        "aws-ipam-allocator",
        "AWS IP Allocator",
        cell.Config(awsDefaultConfig),
        cell.Invoke(startAWSAllocator),
    ))
}

// operator/pkg/ipam/azure.go
func init() {
    allocators = append(allocators, cell.Module(
        "azure-ipam-allocator",
        "Azure IP Allocator",
        cell.Config(azureDefaultConfig),
        cell.Invoke(startAzureAllocator),
    ))
}

// operator/pkg/ipam/alibabacloud.go
func init() {
    allocators = append(allocators, cell.Module(
        "alibabacloud-ipam-allocator",
        "Alibaba Cloud IP Allocator",
        cell.Config(defaultAlibabaCloudConfig),
        cell.Invoke(startAlibabaAllocator),
    ))
}
```

### 7.2 빌드 태그 기반 컴파일

각 프로바이더는 빌드 태그로 조건부 컴파일된다:

```go
//go:build ipam_provider_aws         // operator/pkg/ipam/aws.go
//go:build ipam_provider_azure       // operator/pkg/ipam/azure.go
//go:build ipam_provider_alibabacloud // operator/pkg/ipam/alibabacloud.go
```

### 7.3 Allocator 초기화 흐름

```
startXXXAllocator()
    |
    +-- allocator.Init()   --> SDK 클라이언트 초기화, 메타데이터 수집
    |
    +-- allocator.Start()  --> InstancesManager 생성
                           --> NodeManager 생성 및 시작
                           --> NodeWatcher 작업 등록
```

---

## 8. 요약

Cilium의 클라우드 프로바이더 통합은 다음과 같은 설계 원칙을 따른다:

1. **추상화 계층**: `ipam.NodeOperations` 인터페이스를 통해 프로바이더별 구현을 추상화
2. **Rate Limiting**: 모든 클라우드 API 호출에 자체적인 속도 제한 적용
3. **점진적 동기화**: 전체 리싱크와 인스턴스별 증분 리싱크의 병렬 실행 지원
4. **메트릭 통합**: API 호출 지연 시간 및 상태를 Prometheus 메트릭으로 노출
5. **안전한 동시성**: `lock.RWMutex`를 활용한 읽기/쓰기 분리
6. **보안 그룹 폴백**: 명시적 지정 > 태그 기반 탐색 > 기본 인터페이스 상속
7. **ENI 가비지 컬렉션**: 분리된 ENI를 자동으로 정리 (AWS)
8. **빌드 태그**: 각 프로바이더를 독립적으로 컴파일 가능
