# 30. Kubelet Resource Manager 심화

## 목차

1. [개요](#1-개요)
2. [CPU Manager](#2-cpu-manager)
3. [Memory Manager](#3-memory-manager)
4. [Device Manager](#4-device-manager)
5. [Topology Manager](#5-topology-manager)
6. [Resource Manager 통합 흐름](#6-resource-manager-통합-흐름)
7. [NUMA 토폴로지 인식 할당](#7-numa-토폴로지-인식-할당)
8. [Topology Manager 정책 비교](#8-topology-manager-정책-비교)
9. [Hint Merging 알고리즘](#9-hint-merging-알고리즘)
10. [왜 이런 설계인가](#10-왜-이런-설계인가)
11. [정리](#11-정리)

---

## 1. 개요

Kubelet의 Resource Manager는 노드의 하드웨어 리소스를 NUMA(Non-Uniform Memory Access) 토폴로지에 맞춰 최적으로 할당하기 위한 서브시스템이다. 고성능 워크로드(텔레코, ML, HPC 등)에서 CPU와 메모리가 물리적으로 가까운 NUMA 노드에 함께 배치되지 않으면 성능이 크게 저하된다. Kubernetes는 이 문제를 해결하기 위해 4개의 Resource Manager를 제공한다.

```
+------------------------------------------------------------------+
|                         Kubelet                                   |
|                                                                   |
|  +--------------------+    +---------------------+                |
|  |  Topology Manager   |    |  Container Manager  |                |
|  |  (정책 조정자)       |    |  (Linux)            |                |
|  +--------+-----------+    +----------+----------+                |
|           |                           |                           |
|    +------+-------+------+           |                           |
|    |      |       |      |           |                           |
|  +-v-+  +-v-+  +-v-+  +-v-+         |                           |
|  |CPU|  |Mem|  |Dev|  |DRA|         |                           |
|  |Mgr|  |Mgr|  |Mgr|  |Mgr|         |                           |
|  +---+  +---+  +---+  +---+         |                           |
|                                      |                           |
|  HintProvider 인터페이스              |                           |
+------------------------------------------------------------------+
```

### 4개 Resource Manager의 역할

| Manager | 소스 위치 | 관리 대상 | 정책 |
|---------|----------|----------|------|
| CPU Manager | `pkg/kubelet/cm/cpumanager/` | CPU 코어 | none, static |
| Memory Manager | `pkg/kubelet/cm/memorymanager/` | 메모리, HugePages | None, Static |
| Device Manager | `pkg/kubelet/cm/devicemanager/` | GPU, FPGA 등 디바이스 | (Device Plugin 기반) |
| Topology Manager | `pkg/kubelet/cm/topologymanager/` | 위 3개의 조정자 | none, best-effort, restricted, single-numa-node |

### 핵심 개념: HintProvider 패턴

모든 Resource Manager는 `HintProvider` 인터페이스를 구현한다. Topology Manager가 Pod Admit 시점에 각 HintProvider에게 "어떤 NUMA 노드에 리소스를 할당할 수 있는가?"를 질의하고, 이를 통합(Merge)하여 최적의 NUMA 배치를 결정한다.

```go
// pkg/kubelet/cm/topologymanager/topology_manager.go:80-96
type HintProvider interface {
    GetTopologyHints(pod *v1.Pod, container *v1.Container) map[string][]TopologyHint
    GetPodTopologyHints(pod *v1.Pod) map[string][]TopologyHint
    Allocate(pod *v1.Pod, container *v1.Container) error
}
```

---

## 2. CPU Manager

CPU Manager는 Guaranteed QoS 클래스의 컨테이너에 전용(exclusive) CPU를 할당하는 서브시스템이다. 컨테이너가 다른 워크로드와 CPU를 공유하지 않도록 보장하여 지연 시간(latency)에 민감한 워크로드의 성능을 극대화한다.

### 2.1 Manager 인터페이스

```go
// pkg/kubelet/cm/cpumanager/cpu_manager.go:56-103
type Manager interface {
    Start(ctx context.Context, activePods ActivePodsFunc, ...) error
    Allocate(pod *v1.Pod, container *v1.Container) error
    AddContainer(logger logr.Logger, p *v1.Pod, c *v1.Container, containerID string)
    RemoveContainer(logger logr.Logger, containerID string) error
    State() state.Reader
    GetTopologyHints(pod *v1.Pod, container *v1.Container) map[string][]TopologyHint
    GetExclusiveCPUs(podUID, containerName string) cpuset.CPUSet
    GetPodTopologyHints(pod *v1.Pod) map[string][]TopologyHint
    GetAllocatableCPUs() cpuset.CPUSet
    GetCPUAffinity(podUID, containerName string) cpuset.CPUSet
    GetAllCPUs() cpuset.CPUSet
}
```

### 2.2 Static Policy의 CPU 풀 구조

Static Policy는 CPU를 4개의 논리적 풀(Pool)로 관리한다. 이 설계가 CPU Manager의 핵심이다.

```
+---------------------------------------------------------+
|                     전체 CPU 집합                         |
|                                                         |
|  +------------------+  +----------------------------+   |
|  |   RESERVED       |  |        SHARED               |   |
|  | (시스템/kubelet   |  | (Burstable, BestEffort     |   |
|  |  예약 CPU)        |  |  + 비정수 Guaranteed)       |   |
|  +------------------+  +---+------------------------+   |
|                        |   |    ASSIGNABLE            |   |
|                        |   | = SHARED - RESERVED      |   |
|                        |   +----------+---------+     |   |
|                        |              | EXCLUSIVE|     |   |
|                        |              | (정수 CPU |     |   |
|                        |              | Guaranteed|    |   |
|                        |              | 전용 할당) |    |   |
|                        |              +----------+     |   |
|                        +----------------------------+   |
+---------------------------------------------------------+
```

소스코드에 명시된 풀 정의:

```go
// pkg/kubelet/cm/cpumanager/policy_static.go:78-107 (주석에서 발췌)

// SHARED: Burstable, BestEffort, 비정수 Guaranteed 컨테이너가 사용
//   초기에는 모든 CPU ID를 포함. 전용 할당이 생기면 줄어들고, 해제되면 다시 늘어남.
//   state의 default CPU set으로 저장.

// RESERVED: SHARED의 부분집합으로 전용 할당 불가 영역
//   크기 = ceil(systemreserved.cpu + kubereserved.cpu)
//   가장 낮은 물리 코어부터 토폴로지 순서로 선택.

// ASSIGNABLE: SHARED - RESERVED. 전용 할당은 이 풀에서만.

// EXCLUSIVE ALLOCATIONS: 하나의 컨테이너에 전용 할당된 CPU 집합.
//   state에 명시적 할당(assignment)으로 저장.
```

```go
// pkg/kubelet/cm/cpumanager/policy_static.go:108-128
type staticPolicy struct {
    topology             *topology.CPUTopology   // CPU 소켓/코어/스레드 토폴로지
    reservedCPUs         cpuset.CPUSet           // 시스템 예약 CPU
    reservedPhysicalCPUs cpuset.CPUSet           // 예약 CPU + SMT 형제
    affinity             topologymanager.Store   // Topology Manager 참조
    cpusToReuse          map[string]cpuset.CPUSet // init 컨테이너 CPU 재사용
    options              StaticPolicyOptions     // 정책 옵션
    cpuGroupSize         int                     // SMT 그룹 크기 (코어당 스레드 수)
}
```

### 2.3 CPU 할당 조건

Static Policy가 컨테이너에 전용 CPU를 할당하는 조건은 다음 두 가지를 **모두** 만족해야 한다:

| 조건 | 설명 |
|------|------|
| QoS = Guaranteed | Pod의 모든 컨테이너가 CPU/메모리 request == limit |
| 정수 CPU 요청 | CPU request가 양의 정수 (예: 2, 4, 8) |

### 2.4 Allocate() 핵심 흐름

```go
// pkg/kubelet/cm/cpumanager/policy_static.go:319-410
func (p *staticPolicy) Allocate(logger logr.Logger, s state.State, pod *v1.Pod, container *v1.Container) (rerr error) {
    numCPUs := p.guaranteedCPUs(logger, pod, container)
    if numCPUs == 0 {
        return nil  // 공유 풀 사용 (할당 불필요)
    }

    // FullPhysicalCPUsOnly 옵션: SMT 정렬 검증
    if p.options.FullPhysicalCPUsOnly {
        if (numCPUs % p.cpuGroupSize) != 0 {
            return SMTAlignmentError{...}  // CPU 요청이 코어당 스레드 수의 배수가 아님
        }
    }

    // 이미 할당된 경우 스킵
    if cset, ok := s.GetCPUSet(string(pod.UID), container.Name); ok {
        p.updateCPUsToReuse(pod, container, cset)
        return nil
    }

    // Topology Manager에서 NUMA affinity 힌트 조회
    hint := p.affinity.GetAffinity(string(pod.UID), container.Name)

    // NUMA affinity에 따라 CPU 할당
    cpuAllocation, err := p.allocateCPUs(logger, s, numCPUs, hint.NUMANodeAffinity, p.cpusToReuse[string(pod.UID)])

    // 상태 저장
    s.SetCPUSet(string(pod.UID), container.Name, cpuAllocation.CPUs)
    p.updateCPUsToReuse(pod, container, cpuAllocation.CPUs)
    return nil
}
```

할당 흐름을 단계별로 정리하면:

```
Allocate() 흐름:

1. guaranteedCPUs() → numCPUs 결정
   └── 0이면 공유 풀 → return nil

2. FullPhysicalCPUsOnly 검증
   └── numCPUs % cpuGroupSize != 0 → SMTAlignmentError

3. 이미 할당 확인 (멱등성)
   └── GetCPUSet() 성공 → updateCPUsToReuse() → return nil

4. Topology Manager에서 NUMA affinity 조회
   └── p.affinity.GetAffinity(podUID, containerName)

5. allocateCPUs() → cpuAccumulator로 CPU 선택
   └── takeByTopology() → NUMA/소켓/코어 순서 탐색

6. 상태 저장
   ├── s.SetCPUSet(podUID, containerName, cpus)
   └── p.updateCPUsToReuse(pod, container, cpus)
```

### 2.5 CPU State 구조

CPU Manager의 상태는 두 가지로 구성된다:

```go
// pkg/kubelet/cm/cpumanager/state/state.go:24-58
type ContainerCPUAssignments map[string]map[string]cpuset.CPUSet
// map[podUID] -> map[containerName] -> CPUSet

type Reader interface {
    GetCPUSet(podUID string, containerName string) (cpuset.CPUSet, bool)
    GetDefaultCPUSet() cpuset.CPUSet          // 공유 풀 CPU
    GetCPUSetOrDefault(podUID, containerName string) cpuset.CPUSet
    GetCPUAssignments() ContainerCPUAssignments // 전용 할당 맵
}

type writer interface {
    SetCPUSet(podUID string, containerName string, cpuset cpuset.CPUSet)
    SetDefaultCPUSet(cpuset cpuset.CPUSet)
    SetCPUAssignments(ContainerCPUAssignments)
    Delete(podUID string, containerName string)
    ClearState()
}

type State interface {
    Reader
    writer
}
```

상태 파일은 `cpu_manager_state`라는 이름으로 kubelet root 디렉토리에 저장되며, kubelet 재시작 시 복원된다.

### 2.6 cpuAccumulator: CPU 선택 알고리즘

CPU를 어떤 순서로 선택하느냐가 NUMA 지역성(locality)에 큰 영향을 미친다. `cpuAccumulator`가 이 역할을 담당한다.

```go
// pkg/kubelet/cm/cpumanager/cpu_assignment.go:259-299
type cpuAccumulator struct {
    logger        logr.Logger
    topo          *topology.CPUTopology  // 원본 토폴로지 (불변)
    details       topology.CPUDetails    // 사용 가능한 CPU (할당 시 제거됨)
    numCPUsNeeded int                    // 아직 필요한 CPU 수
    result        cpuset.CPUSet          // 지금까지 축적한 CPU
    numaOrSocketsFirst numaOrSocketsFirstFuncs  // NUMA 우선 vs 소켓 우선
    availableCPUSorter availableCPUSorter       // Packed vs Spread 정렬
}
```

#### Packed vs Spread 전략

```
12-CPU 시스템 (2 소켓, 각 6코어, HT=2):

CPU 토폴로지:
  Socket 0, NUMA 0: Core 0(CPU 0,6), Core 1(CPU 2,8), Core 2(CPU 4,10)
  Socket 1, NUMA 1: Core 3(CPU 1,7), Core 4(CPU 3,9), Core 5(CPU 5,11)

Packed 정렬 (기본값):  0, 2, 4, 6, 8, 10, 1, 3, 5, 7, 9, 11
  → 하나의 NUMA 노드 안에서 코어를 먼저 채움
  → 지역성 최대화, 단일 NUMA 워크로드에 유리

Spread 정렬 (DistributeCPUsAcrossCores 옵션):  0, 6, 2, 8, 4, 10, 1, 7, 3, 9, 5, 11
  → 코어 간에 분산하여 열/전력 밸런스 확보
  → 멀티스레드 워크로드에 유리
```

```go
// pkg/kubelet/cm/cpumanager/cpu_assignment.go:254-257
const (
    CPUSortingStrategyPacked CPUSortingStrategy = "packed"
    CPUSortingStrategySpread CPUSortingStrategy = "spread"
)
```

#### CPU 선택 우선순위

`cpuAccumulator`는 다음 순서로 CPU를 선택한다:

```
1. 전체 NUMA 노드가 가용한 경우 → 전체 NUMA 노드를 통째로 할당
2. 전체 소켓이 가용한 경우   → 전체 소켓을 통째로 할당
3. 전체 코어가 가용한 경우   → 전체 코어를 통째로 할당
4. 개별 CPU 단위              → 남은 CPU 중 정렬 순서대로 선택
```

```go
// cpuAccumulator 생성 시 NUMA vs Socket 우선순위 결정
// pkg/kubelet/cm/cpumanager/cpu_assignment.go:310-314
if topo.NumSockets >= topo.NumNUMANodes {
    acc.numaOrSocketsFirst = &numaFirst{acc}   // NUMA 우선
} else {
    acc.numaOrSocketsFirst = &socketsFirst{acc} // 소켓 우선
}
```

### 2.7 init 컨테이너 CPU 재사용

init 컨테이너가 종료되면 해당 CPU를 앱 컨테이너에 재사용할 수 있다. 단, **restartable init 컨테이너**는 앱 컨테이너와 동시에 실행되므로 재사용 대상에서 제외된다.

```go
// pkg/kubelet/cm/cpumanager/policy_static.go:300-317
func (p *staticPolicy) updateCPUsToReuse(pod *v1.Pod, container *v1.Container, cset cpuset.CPUSet) {
    for _, initContainer := range pod.Spec.InitContainers {
        if container.Name == initContainer.Name {
            if podutil.IsRestartableInitContainer(&initContainer) {
                break  // restartable init container는 재사용 불가
            }
            p.cpusToReuse[string(pod.UID)] = p.cpusToReuse[string(pod.UID)].Union(cset)
            return
        }
    }
    // 앱 컨테이너: 할당된 CPU를 재사용 풀에서 제거
    p.cpusToReuse[string(pod.UID)] = p.cpusToReuse[string(pod.UID)].Difference(cset)
}
```

### 2.8 SMT(Simultaneous Multi-Threading) 정렬

`FullPhysicalCPUsOnly` 옵션이 활성화되면, CPU 요청은 반드시 코어당 스레드 수의 배수여야 한다. 이는 하나의 물리 코어에서 일부 스레드만 전용 할당하고 나머지가 다른 워크로드와 공유되는 보안/성능 문제를 방지한다.

```go
// pkg/kubelet/cm/cpumanager/policy_static.go:50-63
type SMTAlignmentError struct {
    RequestedCPUs         int
    CpusPerCore           int
    AvailablePhysicalCPUs int
    CausedByPhysicalCPUs  bool
}
```

```
예시: Hyper-Threading 시스템 (cpuGroupSize = 2)

요청: CPU=3 → SMTAlignmentError! (3 % 2 != 0)
요청: CPU=4 → 물리 코어 2개 (각 2 스레드) 할당
```

---

## 3. Memory Manager

Memory Manager는 Guaranteed QoS Pod에 NUMA 노드별 메모리(일반 메모리 + HugePages)를 전용 할당하는 서브시스템이다.

### 3.1 Manager 인터페이스

```go
// pkg/kubelet/cm/memorymanager/memory_manager.go:58-95
type Manager interface {
    Start(ctx context.Context, activePods ActivePodsFunc, ...) error
    AddContainer(logger klog.Logger, p *v1.Pod, c *v1.Container, containerID string)
    Allocate(pod *v1.Pod, container *v1.Container) error
    RemoveContainer(logger klog.Logger, containerID string) error
    State() state.Reader
    GetTopologyHints(*v1.Pod, *v1.Container) map[string][]TopologyHint
    GetPodTopologyHints(*v1.Pod) map[string][]TopologyHint
    GetMemoryNUMANodes(logger klog.Logger, pod *v1.Pod, container *v1.Container) sets.Set[int]
    GetAllocatableMemory() []state.Block
    GetMemory(podUID, containerName string) []state.Block
}
```

### 3.2 Static Policy 구조

```go
// pkg/kubelet/cm/memorymanager/policy_static.go:46-58
type staticPolicy struct {
    machineInfo                  *cadvisorapi.MachineInfo   // 머신 메모리 정보
    systemReserved               systemReservedMemory       // 시스템 예약 메모리
    affinity                     topologymanager.Store      // Topology Manager 참조
    initContainersReusableMemory reusableMemory             // init 컨테이너 재사용 메모리
}

// systemReservedMemory: NUMA 노드별 리소스별 예약량
// pkg/kubelet/cm/memorymanager/policy_static.go:42-43
type systemReservedMemory map[int]map[v1.ResourceName]uint64
type reusableMemory map[string]map[string]map[v1.ResourceName]uint64
```

### 3.3 메모리 상태 모델

Memory Manager는 NUMA 노드별로 메모리 테이블을 관리하고, 컨테이너별로 Block 단위로 할당을 추적한다.

```go
// pkg/kubelet/cm/memorymanager/state/state.go:24-100

// MemoryTable: NUMA 노드별 메모리 정보
type MemoryTable struct {
    TotalMemSize   uint64  // 전체 메모리 크기
    SystemReserved uint64  // 시스템 예약량
    Allocatable    uint64  // 할당 가능량 = Total - SystemReserved
    Reserved       uint64  // 이미 예약된 양 (컨테이너 할당)
    Free           uint64  // 남은 양 = Allocatable - Reserved
}

// NUMANodeState: NUMA 노드 상태
type NUMANodeState struct {
    NumberOfAssignments int                           // 할당 횟수
    MemoryMap           map[v1.ResourceName]*MemoryTable // 리소스별 메모리 테이블
    Cells               []int                          // 크로스-NUMA 그룹
}

// NUMANodeMap: NUMA 노드 ID → 상태
type NUMANodeMap map[int]*NUMANodeState

// Block: 컨테이너의 메모리 할당 단위
type Block struct {
    NUMAAffinity []int           // NUMA 노드 목록
    Type         v1.ResourceName // "memory" 또는 "hugepages-2Mi" 등
    Size         uint64          // 할당 크기 (바이트)
}

// ContainerMemoryAssignments: Pod/Container → Block 매핑
type ContainerMemoryAssignments map[string]map[string][]Block
```

```
NUMA 노드별 메모리 상태 예시 (2-NUMA 시스템):

+----NUMA Node 0---------+    +----NUMA Node 1---------+
| TotalMemSize: 16 GiB   |    | TotalMemSize: 16 GiB   |
| SystemReserved: 1 GiB  |    | SystemReserved: 1 GiB  |
| Allocatable: 15 GiB    |    | Allocatable: 15 GiB    |
| Reserved: 4 GiB        |    | Reserved: 8 GiB        |
| Free: 11 GiB           |    | Free: 7 GiB            |
|                         |    |                         |
| NumberOfAssignments: 2  |    | NumberOfAssignments: 3  |
| Cells: [0]              |    | Cells: [1]              |
+-------------------------+    +-------------------------+
```

### 3.4 Allocate() 핵심 흐름

Memory Manager의 할당은 CPU Manager보다 복잡하다. Topology Manager의 힌트가 불충분할 경우 확장(extend) 로직이 동작한다.

```go
// pkg/kubelet/cm/memorymanager/policy_static.go:98-210 (요약)
func (p *staticPolicy) Allocate(logger klog.Logger, s state.State, pod *v1.Pod, container *v1.Container) (rerr error) {
    // 1. Guaranteed QoS가 아니면 스킵
    qos := v1qos.GetPodQOS(pod)
    if qos != v1.PodQOSGuaranteed {
        return nil
    }

    // 2. 이미 할당된 경우 재사용 메모리 업데이트 후 스킵
    if blocks := s.GetMemoryBlocks(podUID, container.Name); blocks != nil {
        p.updatePodReusableMemory(pod, container, blocks)
        return nil
    }

    // 3. Topology Manager에서 NUMA affinity 조회
    hint := p.affinity.GetAffinity(podUID, container.Name)
    bestHint := &hint

    // 4. 힌트가 nil이면 기본 힌트 계산
    if hint.NUMANodeAffinity == nil {
        defaultHint, err := p.getDefaultHint(machineState, pod, requestedResources)
        bestHint = defaultHint
    }

    // 5. 힌트가 요청량을 만족하지 못하면 확장
    if !isAffinitySatisfyRequest(machineState, bestHint.NUMANodeAffinity, requestedResources) {
        extendedHint, err := p.extendTopologyManagerHint(...)
        bestHint = extendedHint
    }

    // 6. Single vs Cross NUMA 할당 규칙 위반 검사
    if isAffinityViolatingNUMAAllocations(machineState, bestHint.NUMANodeAffinity) {
        return fmt.Errorf("preferred hint violates NUMA node allocation")
    }

    // 7. Block 생성 및 상태 업데이트
    for resourceName, requestedSize := range requestedResources {
        containerBlocks = append(containerBlocks, state.Block{
            NUMAAffinity: maskBits,
            Size:         requestedSize,
            Type:         resourceName,
        })
        p.updateMachineState(machineState, maskBits, resourceName, requestedSize)
    }

    s.SetMachineState(machineState)
    s.SetMemoryBlocks(podUID, container.Name, containerBlocks)
    return nil
}
```

### 3.5 Machine State 업데이트

할당 시 NUMA 노드의 메모리 상태를 업데이트하는 로직이다:

```go
// pkg/kubelet/cm/memorymanager/policy_static.go:212-219
func (p *staticPolicy) updateMachineState(machineState state.NUMANodeMap, numaAffinity []int,
    resourceName v1.ResourceName, requestedSize uint64) {
    for _, nodeID := range numaAffinity {
        machineState[nodeID].NumberOfAssignments++
        machineState[nodeID].Cells = numaAffinity  // 크로스-NUMA 그룹 기록
        if requestedSize == 0 {
            continue
        }
        // 해당 노드에서 가능한 만큼 할당
        // ...
    }
}
```

### 3.6 Single vs Cross NUMA 할당 규칙

Memory Manager는 하나의 NUMA 노드에서 single-NUMA 할당과 cross-NUMA 할당이 혼재되지 않도록 한다. 이 규칙을 위반하면 메모리 단편화와 성능 저하가 발생한다.

```
허용되는 경우:
  NUMA 0: [Pod-A (cells=[0])]       <-- single-NUMA 할당만
  NUMA 1: [Pod-B (cells=[1,2])]     <-- cross-NUMA 할당만 (NUMA 2와 함께)

금지되는 경우:
  NUMA 0: [Pod-A (cells=[0]), Pod-C (cells=[0,1])]  <-- single + cross 혼재!
```

### 3.7 Reader/Writer 상태 인터페이스

```go
// pkg/kubelet/cm/memorymanager/state/state.go:103-130
type Reader interface {
    GetMachineState() NUMANodeMap
    GetMemoryBlocks(podUID string, containerName string) []Block
    GetMemoryAssignments() ContainerMemoryAssignments
}

type writer interface {
    SetMachineState(memoryMap NUMANodeMap)
    SetMemoryBlocks(podUID string, containerName string, blocks []Block)
    SetMemoryAssignments(assignments ContainerMemoryAssignments)
    Delete(podUID string, containerName string)
    ClearState()
}

type State interface {
    Reader
    writer
}
```

---

## 4. Device Manager

Device Manager는 Device Plugin을 통해 GPU, FPGA, SR-IOV NIC 등 확장 리소스를 관리한다. Topology Manager와의 통합을 통해 디바이스의 NUMA 지역성도 고려한다.

### 4.1 ManagerImpl 구조

```go
// pkg/kubelet/cm/devicemanager/manager.go:62-116
type ManagerImpl struct {
    checkpointdir string
    endpoints     map[string]endpointInfo    // 리소스명 -> Device Plugin endpoint
    mutex         sync.Mutex
    server        plugin.Server              // gRPC 서버

    activePods    ActivePodsFunc
    sourcesReady  config.SourcesReady

    allDevices       ResourceDeviceInstances   // 등록된 모든 디바이스
    healthyDevices   map[string]sets.Set[string]  // 건강한 디바이스 ID 집합
    unhealthyDevices map[string]sets.Set[string]  // 비건강한 디바이스 ID 집합
    allocatedDevices map[string]sets.Set[string]  // 할당된 디바이스 ID 집합

    podDevices        *podDevices               // Pod -> 디바이스 매핑
    checkpointManager checkpointmanager.CheckpointManager

    numaNodes              []int                          // NUMA 노드 목록
    topologyAffinityStore  topologymanager.Store           // Topology Manager 참조
    devicesToReuse         PodReusableDevices              // init 컨테이너 디바이스 재사용
    containerMap           containermap.ContainerMap
    containerRunningSet    sets.Set[string]
}
```

### 4.2 디바이스 상태 관리

```
+-------------------+     +---------------------+     +---------------------+
| allDevices        |     | healthyDevices       |     | allocatedDevices    |
| (전체 등록)        |     | (건강한 디바이스)     |     | (이미 할당됨)       |
|                   |     |                      |     |                     |
| nvidia.com/gpu:   |     | nvidia.com/gpu:      |     | nvidia.com/gpu:     |
|   gpu-0: Healthy  |     |   {gpu-0, gpu-1,     |     |   {gpu-0}           |
|   gpu-1: Healthy  |     |    gpu-2, gpu-3}     |     |                     |
|   gpu-2: Healthy  |     |                      |     |                     |
|   gpu-3: Healthy  |     +---------------------+     +---------------------+
|   gpu-4: Unhealthy|
+-------------------+     available = healthy - allocated = {gpu-1, gpu-2, gpu-3}
```

### 4.3 Topology Hint 생성 흐름

```go
// pkg/kubelet/cm/devicemanager/topology_hints.go:33-84
func (m *ManagerImpl) GetTopologyHints(pod *v1.Pod, container *v1.Container) map[string][]TopologyHint {
    m.UpdateAllocatedDevices()  // 가비지 컬렉션

    deviceHints := make(map[string][]TopologyHint)
    accumulatedResourceRequests := m.getContainerDeviceRequest(container)

    m.mutex.Lock()
    defer m.mutex.Unlock()
    for resource, requested := range accumulatedResourceRequests {
        // 토폴로지 정보가 없는 디바이스는 nil 힌트 (어디든 OK)
        if aligned := m.deviceHasTopologyAlignment(resource); !aligned {
            deviceHints[resource] = nil
            continue
        }

        // 이미 할당된 디바이스가 있으면 해당 토폴로지로 재생성
        allocated := m.podDevices.containerDevices(string(pod.UID), container.Name, resource)
        if allocated.Len() > 0 {
            deviceHints[resource] = m.generateDeviceTopologyHints(resource, allocated, sets.Set[string]{}, requested)
            continue
        }

        // 가용한 디바이스 목록으로 힌트 생성
        available := m.getAvailableDevices(resource)
        reusable := m.devicesToReuse[string(pod.UID)][resource]
        deviceHints[resource] = m.generateDeviceTopologyHints(resource, available, reusable, requested)
    }
    return deviceHints
}
```

### 4.4 generateDeviceTopologyHints: NUMA 비트마스크 순회

```go
// pkg/kubelet/cm/devicemanager/topology_hints.go:154-219
func (m *ManagerImpl) generateDeviceTopologyHints(resource string, available sets.Set[string],
    reusable sets.Set[string], request int) []TopologyHint {

    minAffinitySize := len(m.numaNodes)
    hints := []TopologyHint{}

    // 모든 NUMA 노드 조합을 순회 (2^N - 1개 비트마스크)
    bitmask.IterateBitMasks(m.numaNodes, func(mask bitmask.BitMask) {
        // 1. 이 마스크에 속하는 전체 디바이스 수 확인 (minAffinitySize 업데이트용)
        devicesInMask := 0
        for _, device := range m.allDevices[resource] {
            if mask.AnySet(m.getNUMANodeIds(device.Topology)) {
                devicesInMask++
            }
        }
        if devicesInMask >= request && mask.Count() < minAffinitySize {
            minAffinitySize = mask.Count()
        }

        // 2. 재사용 디바이스가 이 마스크에 포함되는지 확인
        numMatching := 0
        for d := range reusable {
            if !mask.AnySet(m.getNUMANodeIds(m.allDevices[resource][d].Topology)) {
                return  // 이 조합은 불가
            }
            numMatching++
        }

        // 3. 가용한 디바이스 중 이 마스크에 해당하는 것 카운트
        for d := range available {
            if mask.AnySet(m.getNUMANodeIds(m.allDevices[resource][d].Topology)) {
                numMatching++
            }
        }

        // 4. 요청 수를 충족하면 힌트 추가 (일단 Preferred=false)
        if numMatching >= request {
            hints = append(hints, TopologyHint{
                NUMANodeAffinity: mask,
                Preferred:        false,
            })
        }
    })

    // 5. 최소 affinity 크기와 같은 힌트만 Preferred로 표시
    for i := range hints {
        if hints[i].NUMANodeAffinity.Count() == minAffinitySize {
            hints[i].Preferred = true
        }
    }
    return hints
}
```

### 4.5 Preferred 힌트의 의미

```
예시: 4-NUMA 시스템, GPU 4개 (NUMA 0에 2개, NUMA 1에 2개)
요청: GPU 2개

생성되는 힌트:
  {NUMANodeAffinity: [0],     Preferred: true}   <-- 최소 1개 NUMA로 충족
  {NUMANodeAffinity: [1],     Preferred: true}   <-- 최소 1개 NUMA로 충족
  {NUMANodeAffinity: [0,1],   Preferred: false}  <-- 2개 NUMA 필요 (불필요하게 넓음)
  {NUMANodeAffinity: [0,1,2], Preferred: false}  <-- 3개 NUMA 필요
  ...

minAffinitySize = 1 → Count() == 1인 힌트만 Preferred
```

### 4.6 토폴로지 정렬 확인

```go
// pkg/kubelet/cm/devicemanager/topology_hints.go:139-147
func (m *ManagerImpl) deviceHasTopologyAlignment(resource string) bool {
    for _, device := range m.allDevices[resource] {
        if device.Topology != nil && len(device.Topology.Nodes) > 0 {
            return true  // 하나라도 토폴로지 정보가 있으면 정렬 필요
        }
    }
    return false  // 토폴로지 정보 없음 → NUMA 무관 디바이스
}
```

---

## 5. Topology Manager

Topology Manager는 CPU, 메모리, 디바이스의 NUMA 배치를 조율하는 상위 조정자(orchestrator)이다. Pod Admit 시점에 모든 HintProvider로부터 힌트를 수집하고, 병합(Merge)하여 최적의 NUMA affinity를 결정한다.

### 5.1 핵심 데이터 구조

```go
// pkg/kubelet/cm/topologymanager/topology_manager.go:58-70
type Manager interface {
    lifecycle.PodAdmitHandler                      // Pod Admit 핸들러
    AddHintProvider(logger klog.Logger, h HintProvider) // HintProvider 등록
    AddContainer(pod *v1.Pod, container *v1.Container, containerID string)
    RemoveContainer(containerID string) error
    Store                                           // affinity 저장소
}

type manager struct {
    scope Scope  // Container Scope 또는 Pod Scope
}

// pkg/kubelet/cm/topologymanager/topology_manager.go:104-110
type TopologyHint struct {
    NUMANodeAffinity bitmask.BitMask  // NUMA 노드 비트마스크
    Preferred        bool             // 선호 힌트 여부
}
```

`TopologyHint`는 두 개의 비교 메서드를 제공한다:

```go
// pkg/kubelet/cm/topologymanager/topology_manager.go:113-131
func (th *TopologyHint) IsEqual(topologyHint TopologyHint) bool {
    if th.Preferred == topologyHint.Preferred {
        if th.NUMANodeAffinity == nil || topologyHint.NUMANodeAffinity == nil {
            return th.NUMANodeAffinity == topologyHint.NUMANodeAffinity
        }
        return th.NUMANodeAffinity.IsEqual(topologyHint.NUMANodeAffinity)
    }
    return false
}

func (th *TopologyHint) LessThan(other TopologyHint) bool {
    if th.Preferred != other.Preferred {
        return th.Preferred  // preferred < non-preferred
    }
    return th.NUMANodeAffinity.IsNarrowerThan(other.NUMANodeAffinity)
}
```

### 5.2 Store 인터페이스

```go
// pkg/kubelet/cm/topologymanager/topology_manager.go:98-102
type Store interface {
    GetAffinity(podUID string, containerName string) TopologyHint
    GetPolicy() Policy
}
```

각 Resource Manager(CPU, Memory, Device)는 `Store`를 통해 Topology Manager가 결정한 NUMA affinity를 조회한다.

### 5.3 NUMAInfo 구조

```go
// pkg/kubelet/cm/topologymanager/numa_info.go:26-31
type NUMADistances map[int][]uint64

type NUMAInfo struct {
    Nodes         []int          // NUMA 노드 ID 목록
    NUMADistances NUMADistances  // NUMA 간 거리 매트릭스
}
```

NUMAInfo는 NUMA 노드 간의 거리 정보를 포함하며, `PreferClosestNUMA` 옵션 활성화 시 가장 가까운 NUMA 노드 조합을 선호한다.

```
NUMA Distance Matrix 예시 (4-NUMA 시스템):

         NUMA 0   NUMA 1   NUMA 2   NUMA 3
NUMA 0 [  10       12       20       22   ]
NUMA 1 [  12       10       22       20   ]
NUMA 2 [  20       22       10       12   ]
NUMA 3 [  22       20       12       10   ]

NUMA 0-1은 거리 12 (같은 소켓, 가까움)
NUMA 0-2는 거리 20 (다른 소켓, 멀음)
```

### 5.4 DefaultAffinityMask과 거리 계산

```go
// pkg/kubelet/cm/topologymanager/numa_info.go:88-109
func (n NUMAInfo) DefaultAffinityMask() bitmask.BitMask {
    defaultAffinity, _ := bitmask.NewBitMask(n.Nodes...)
    return defaultAffinity  // 모든 NUMA 노드를 포함하는 마스크
}

func (d NUMADistances) CalculateAverageFor(bm bitmask.BitMask) float64 {
    var count, sum float64
    for _, node1 := range bm.GetBits() {
        for _, node2 := range bm.GetBits() {
            sum += float64(d[node1][node2])
            count++
        }
    }
    return sum / count  // 마스크 내 모든 NUMA 쌍의 평균 거리
}
```

### 5.5 Narrowest vs Closest 비교

```go
// pkg/kubelet/cm/topologymanager/numa_info.go:57-86
func (n *NUMAInfo) Narrowest(m1 bitmask.BitMask, m2 bitmask.BitMask) bitmask.BitMask {
    if m1.IsNarrowerThan(m2) { return m1 }
    return m2
}

func (n *NUMAInfo) Closest(m1 bitmask.BitMask, m2 bitmask.BitMask) bitmask.BitMask {
    if m1.Count() != m2.Count() {
        return n.Narrowest(m1, m2)  // 비트 수가 다르면 좁은 것 선호
    }
    m1Distance := n.NUMADistances.CalculateAverageFor(m1)
    m2Distance := n.NUMADistances.CalculateAverageFor(m2)
    if m1Distance == m2Distance {
        if m1.IsLessThan(m2) { return m1 }
        return m2
    }
    if m1Distance < m2Distance { return m1 }
    return m2
}
```

### 5.6 NUMA 노드 수 제한

```go
// pkg/kubelet/cm/topologymanager/topology_manager.go:33-42
const defaultMaxAllowableNUMANodes = 8
```

순열 순회의 지수적 복잡도 때문에 NUMA 노드 수를 최대 8개로 제한한다. 이를 초과하면 Topology Manager 로딩 시 에러가 발생한다.

---

## 6. Resource Manager 통합 흐름

Container Manager Linux에서 모든 Resource Manager가 초기화되고 Topology Manager에 등록되는 흐름이다.

### 6.1 초기화 순서

```go
// pkg/kubelet/cm/container_manager_linux.go:312-356 (요약)

// 1. Topology Manager 생성
cm.topologyManager, err = topologymanager.NewManager(...)

// 2. Device Manager 생성 및 등록
cm.deviceManager, err = devicemanager.NewManagerImpl(machineInfo.Topology, cm.topologyManager)
cm.topologyManager.AddHintProvider(logger, cm.deviceManager)

// 3. CPU Manager 생성 및 등록
cm.cpuManager, err = cpumanager.NewManager(logger, ...)
cm.topologyManager.AddHintProvider(logger, cm.cpuManager)

// 4. Memory Manager 생성 및 등록
cm.memoryManager, err = memorymanager.NewManager(logger, ...)
cm.topologyManager.AddHintProvider(logger, cm.memoryManager)
```

초기화 순서에서 주목할 점:
- Topology Manager가 **먼저** 생성된다
- 각 Resource Manager는 생성 시 `topologymanager.Store` 참조를 받는다
- 생성 후 `AddHintProvider()`로 Topology Manager에 등록된다
- 등록 순서: Device Manager → CPU Manager → Memory Manager

### 6.2 Pod Admit 시 전체 흐름

```
Pod Admit 요청
│
├─1. Topology Manager: Pod Admit Handler 호출
│   │
│   ├─2. 각 HintProvider에게 TopologyHint 수집
│   │   ├── CPU Manager.GetTopologyHints()
│   │   │   └── 가용 CPU의 NUMA 분포 기반 힌트
│   │   ├── Memory Manager.GetTopologyHints()
│   │   │   └── NUMA 노드별 여유 메모리 기반 힌트
│   │   └── Device Manager.GetTopologyHints()
│   │       └── 디바이스 NUMA 위치 기반 힌트
│   │
│   ├─3. 힌트 병합 (Policy.Merge())
│   │   ├── filterProvidersHints() → 리소스별 힌트 목록
│   │   ├── [single-numa-node] filterSingleNumaHints()
│   │   ├── NewHintMerger() → HintMerger 생성
│   │   ├── iterateAllProviderTopologyHints() → 순열 순회
│   │   ├── mergePermutation() → bitwise-AND 병합
│   │   └── compare() → 최적 힌트 선택
│   │
│   ├─4. 정책 기반 Admit 결정
│   │   ├── none: 항상 허용
│   │   ├── best-effort: 항상 허용 (최선 힌트 사용)
│   │   ├── restricted: Preferred 힌트 필요
│   │   └── single-numa-node: Preferred + 단일 NUMA 힌트 필요
│   │
│   └─5. Admit 성공 시 각 HintProvider.Allocate() 호출
│       ├── CPU Manager.Allocate()
│       │   └── GetAffinity() → NUMA affinity에 따라 CPU 할당
│       ├── Memory Manager.Allocate()
│       │   └── GetAffinity() → NUMA affinity에 따라 메모리 할당
│       └── Device Manager.Allocate()
│           └── GetAffinity() → NUMA affinity에 따라 디바이스 할당
│
└─ Admit 결과 반환
```

### 6.3 Scope: Container vs Pod

Topology Manager는 두 가지 Scope를 지원한다:

| Scope | 설명 | HintProvider 호출 |
|-------|------|------------------|
| container (기본) | 컨테이너 단위로 NUMA 배치 결정 | `GetTopologyHints(pod, container)` |
| pod | Pod 전체를 하나의 단위로 NUMA 배치 결정 | `GetPodTopologyHints(pod)` |

Pod Scope는 여러 컨테이너가 모두 같은 NUMA 노드에 배치되어야 하는 경우(예: sidecar 패턴)에 유용하다.

### 6.4 Reconcile Loop

CPU Manager와 Memory Manager는 주기적인 Reconcile Loop을 통해 상태를 검증하고, 실제 cgroup 설정이 원하는 상태와 일치하는지 확인한다.

```
Reconcile Loop (CPU Manager):
  1. 활성 Pod 목록 조회
  2. 각 컨테이너의 기대 CPU 집합과 실제 cgroup 설정 비교
  3. 불일치 시 UpdateContainerResources() 호출로 cgroup 업데이트
  4. 스테일 컨테이너(종료된 Pod의 할당) 정리
```

---

## 7. NUMA 토폴로지 인식 할당

### 7.1 NUMA 아키텍처 개요

현대 서버는 여러 CPU 소켓을 가지며, 각 소켓은 자체 메모리 컨트롤러와 로컬 메모리를 가진다. CPU가 자신의 로컬 메모리에 접근하는 것보다 원격 NUMA 노드의 메모리에 접근하는 것이 훨씬 느리다.

```
+==============================+    +==============================+
||  Socket 0 / NUMA Node 0     ||    ||  Socket 1 / NUMA Node 1     ||
||                              ||    ||                              ||
||  +------+------+------+     ||    ||  +------+------+------+     ||
||  |Core 0|Core 1|Core 2|     || QPI||  |Core 3|Core 4|Core 5|     ||
||  | HT0  | HT0  | HT0  |     ||<-->||  | HT0  | HT0  | HT0  |     ||
||  | HT1  | HT1  | HT1  |     ||    ||  | HT1  | HT1  | HT1  |     ||
||  +------+------+------+     ||    ||  +------+------+------+     ||
||                              ||    ||                              ||
||  +------------------------+ ||    ||  +------------------------+ ||
||  | Local Memory (16 GiB)  | ||    ||  | Local Memory (16 GiB)  | ||
||  +------------------------+ ||    ||  +------------------------+ ||
||                              ||    ||                              ||
||  +----------+               ||    ||  +----------+               ||
||  | GPU 0    |               ||    ||  | GPU 1    |               ||
||  | (PCIe)   |               ||    ||  | (PCIe)   |               ||
||  +----------+               ||    ||  +----------+               ||
+==============================+    +==============================+
```

### 7.2 NUMA 지역성의 성능 영향

| 접근 유형 | 지연 시간 | 대역폭 |
|----------|----------|--------|
| 로컬 NUMA 메모리 | ~100ns | ~50 GB/s |
| 원격 NUMA 메모리 (1-hop) | ~150-200ns | ~30 GB/s |
| 원격 NUMA 메모리 (2-hop) | ~200-300ns | ~20 GB/s |

지연 시간에 민감한 워크로드(DPDK, 5G UPF, HFT)에서는 이 차이가 처리량과 테일 레이턴시에 직접적인 영향을 미친다.

### 7.3 리소스별 NUMA Hint 생성 방식

| Manager | Hint 생성 기준 | Preferred 조건 |
|---------|---------------|---------------|
| CPU Manager | 가용 CPU가 속한 NUMA 노드 | 요청 CPU를 최소 NUMA 수로 충족 |
| Memory Manager | NUMA 노드별 여유 메모리 | 요청 메모리를 단일/최소 NUMA로 충족 |
| Device Manager | 디바이스의 물리적 NUMA 위치 | 최소 NUMA 수로 요청 디바이스 충족 |

### 7.4 NUMA Affinity 결정 과정

```
예시: Pod가 CPU=4, Memory=8GiB, GPU=1을 요청

CPU Manager 힌트:
  [NUMA 0: preferred] [NUMA 1: preferred] [NUMA 0,1: non-preferred]
  (양쪽 NUMA에 각각 4+ CPU 가용)

Memory Manager 힌트:
  [NUMA 0: preferred] [NUMA 1: preferred] [NUMA 0,1: non-preferred]
  (양쪽 NUMA에 각각 8+ GiB 가용)

Device Manager 힌트:
  [NUMA 1: preferred]  <-- GPU 1이 NUMA 1에 연결
  [NUMA 0,1: non-preferred]

Merge 결과 (순열별):
  순열 1: CPU[0] AND Mem[0] AND GPU[1] → 0b01 & 0b01 & 0b10 = 0b00 → 공집합 (불가)
  순열 2: CPU[0] AND Mem[0] AND GPU[0,1] → 0b01 & 0b01 & 0b11 = 0b01 → non-preferred
  순열 3: CPU[1] AND Mem[1] AND GPU[1] → 0b10 & 0b10 & 0b10 = 0b10 → preferred!
  순열 4: CPU[1] AND Mem[1] AND GPU[0,1] → 0b10 & 0b10 & 0b11 = 0b10 → non-preferred
  ...

  최적: NUMA [1], preferred → CPU/메모리/GPU 모두 NUMA 1에 배치
```

---

## 8. Topology Manager 정책 비교

### 8.1 정책별 동작 비교 테이블

| 정책 | Hint Merge | Admit 조건 | 실패 시 동작 | 사용 시나리오 |
|------|-----------|-----------|------------|-------------|
| `none` | 병합하지 않음 | 항상 허용 | - | NUMA 미고려, 일반 워크로드 |
| `best-effort` | 병합 수행, 최적 선택 | 항상 허용 | 비최적 배치 허용 | NUMA 고려하되 실패 방지 |
| `restricted` | 병합 수행, 최적 선택 | Preferred 필요 | Pod Reject | NUMA 정렬 보장 필요 |
| `single-numa-node` | 단일 NUMA 힌트만, 병합 | Preferred 필요 | Pod Reject | 최대 지역성 (텔레코, HPC) |

### 8.2 정책별 코드 비교

```go
// none: 가장 단순, 힌트를 아예 사용하지 않음
// pkg/kubelet/cm/topologymanager/policy_none.go
func (p *nonePolicy) Merge(logger klog.Logger, providersHints []map[string][]TopologyHint) (TopologyHint, bool) {
    return TopologyHint{}, true
}

// best-effort: 힌트 병합, 항상 admit
// pkg/kubelet/cm/topologymanager/policy_best_effort.go
func (p *bestEffortPolicy) Merge(logger klog.Logger, providersHints []map[string][]TopologyHint) (TopologyHint, bool) {
    filteredHints := filterProvidersHints(logger, providersHints)
    merger := NewHintMerger(p.numaInfo, filteredHints, p.Name(), p.opts)
    bestHint := merger.Merge()
    return bestHint, true  // canAdmitPodResult: 항상 true
}

// restricted: best-effort 상속 + Preferred 검사
// pkg/kubelet/cm/topologymanager/policy_restricted.go
type restrictedPolicy struct {
    bestEffortPolicy  // 임베딩
}
func (p *restrictedPolicy) canAdmitPodResult(hint *TopologyHint) bool {
    return hint.Preferred  // Preferred가 아니면 거부
}
func (p *restrictedPolicy) Merge(logger klog.Logger, providersHints []map[string][]TopologyHint) (TopologyHint, bool) {
    filteredHints := filterProvidersHints(logger, providersHints)
    merger := NewHintMerger(p.numaInfo, filteredHints, p.Name(), p.opts)
    bestHint := merger.Merge()
    return bestHint, bestHint.Preferred
}

// single-numa-node: 단일 NUMA 힌트만 필터링 후 병합
// pkg/kubelet/cm/topologymanager/policy_single_numa_node.go
func (p *singleNumaNodePolicy) Merge(logger klog.Logger, providersHints []map[string][]TopologyHint) (TopologyHint, bool) {
    filteredHints := filterProvidersHints(logger, providersHints)
    singleNumaHints := filterSingleNumaHints(filteredHints)  // 단일 NUMA만 필터
    merger := NewHintMerger(p.numaInfo, singleNumaHints, p.Name(), p.opts)
    bestHint := merger.Merge()
    // DefaultAffinityMask와 같으면 nil로 변환 (실질적 의미 없음)
    if bestHint.NUMANodeAffinity.IsEqual(p.numaInfo.DefaultAffinityMask()) {
        bestHint = TopologyHint{nil, bestHint.Preferred}
    }
    return bestHint, bestHint.Preferred
}
```

### 8.3 정책별 시나리오 비교

```
시나리오: 2-NUMA 시스템, Pod가 CPU=6 요청
NUMA 0: 4 CPU 가용, NUMA 1: 4 CPU 가용

CPU Manager 힌트:
  [NUMA 0: non-preferred (4 < 6)]
  [NUMA 1: non-preferred (4 < 6)]
  [NUMA 0,1: preferred (4+4=8 >= 6)]

정책별 결과:
  none           → admit (힌트 무시, NUMA affinity 없음)
  best-effort    → admit, affinity=[0,1] (최적 힌트 사용하되 거부하지 않음)
  restricted     → admit, affinity=[0,1] (preferred 힌트 존재하므로 허용)
  single-numa-node → REJECT! (단일 NUMA preferred 힌트 없음)
```

```
시나리오: 2-NUMA 시스템, Pod가 CPU=2, GPU=1 요청
NUMA 0: 4 CPU 가용, GPU 없음
NUMA 1: 4 CPU 가용, GPU 1개

CPU Manager 힌트:
  [NUMA 0: preferred] [NUMA 1: preferred] [NUMA 0,1: non-preferred]

Device Manager 힌트:
  [NUMA 1: preferred]

정책별 결과:
  none           → admit (CPU와 GPU가 다른 NUMA에 배치될 수 있음)
  best-effort    → admit, affinity=[1] (CPU+GPU 모두 NUMA 1)
  restricted     → admit, affinity=[1] (preferred 힌트 존재)
  single-numa-node → admit, affinity=[1] (단일 NUMA preferred 힌트 존재)
```

---

## 9. Hint Merging 알고리즘

Hint Merging은 Topology Manager의 가장 복잡한 부분이다. 여러 HintProvider의 힌트를 교차 조합하여 모든 리소스가 동시에 만족할 수 있는 최적의 NUMA affinity를 찾는다.

### 9.1 HintMerger 구조

```go
// pkg/kubelet/cm/topologymanager/policy.go:137-148
type HintMerger struct {
    NUMAInfo                      *NUMAInfo
    Hints                         [][]TopologyHint      // 각 리소스별 힌트 목록
    BestNonPreferredAffinityCount int                   // 비선호 힌트 최적 NUMA 수
    CompareNUMAAffinityMasks      func(*TopologyHint, *TopologyHint) *TopologyHint
}
```

### 9.2 BestNonPreferredAffinityCount의 의미

이 값은 "모든 HintProvider 중 가장 넓은 최소 NUMA 요구사항"이다. 비선호 힌트들 사이에서 어떤 것이 가장 "적절한" 크기인지를 판단하는 기준이 된다.

```go
// pkg/kubelet/cm/topologymanager/policy.go:104-135
func narrowestHint(hints []TopologyHint) *TopologyHint {
    var narrowestHint *TopologyHint
    for i := range hints {
        if hints[i].NUMANodeAffinity == nil { continue }
        if narrowestHint == nil { narrowestHint = &hints[i] }
        if hints[i].NUMANodeAffinity.IsNarrowerThan(narrowestHint.NUMANodeAffinity) {
            narrowestHint = &hints[i]
        }
    }
    return narrowestHint
}

func maxOfMinAffinityCounts(filteredHints [][]TopologyHint) int {
    maxOfMinCount := 0
    for _, resourceHints := range filteredHints {
        nh := narrowestHint(resourceHints)
        if nh != nil && nh.NUMANodeAffinity.Count() > maxOfMinCount {
            maxOfMinCount = nh.NUMANodeAffinity.Count()
        }
    }
    return maxOfMinCount
}
```

```
예시:
  CPU 힌트: narrowest = NUMA [0] (count=1)
  GPU 힌트: narrowest = NUMA [0,1] (count=2)  <-- GPU가 양쪽에 분산
  Memory 힌트: narrowest = NUMA [0] (count=1)

  BestNonPreferredAffinityCount = max(1, 2, 1) = 2
  → 비선호 힌트 평가 시 count=2에 가장 가까운 것을 선택
```

### 9.3 Merge() 알고리즘 상세

```go
// pkg/kubelet/cm/topologymanager/policy.go:303-322
func (m HintMerger) Merge() TopologyHint {
    defaultAffinity := m.NUMAInfo.DefaultAffinityMask()  // 모든 NUMA 노드

    var bestHint *TopologyHint
    iterateAllProviderTopologyHints(m.Hints, func(permutation []TopologyHint) {
        // 각 순열(permutation)을 병합
        mergedHint := mergePermutation(defaultAffinity, permutation)
        // 현재 최적과 비교하여 업데이트
        bestHint = m.compare(bestHint, &mergedHint)
    })

    if bestHint == nil {
        bestHint = &TopologyHint{defaultAffinity, false}
    }
    return *bestHint
}
```

### 9.4 mergePermutation: 비트와이즈 AND

```go
// pkg/kubelet/cm/topologymanager/policy.go:44-69
func mergePermutation(defaultAffinity bitmask.BitMask, permutation []TopologyHint) TopologyHint {
    preferred := true
    var numaAffinities []bitmask.BitMask
    for _, hint := range permutation {
        if hint.NUMANodeAffinity != nil {
            numaAffinities = append(numaAffinities, hint.NUMANodeAffinity)
            // 모든 affinity가 동일하지 않으면 non-preferred
            if !hint.NUMANodeAffinity.IsEqual(numaAffinities[0]) {
                preferred = false
            }
        }
        // 하나라도 non-preferred면 전체 non-preferred
        if !hint.Preferred {
            preferred = false
        }
    }

    // 비트와이즈 AND: 모든 리소스가 공통으로 만족하는 NUMA 노드
    mergedAffinity := bitmask.And(defaultAffinity, numaAffinities...)
    return TopologyHint{mergedAffinity, preferred}
}
```

```
순열 병합 예시 (3-NUMA 시스템):

리소스 A 힌트: NUMA [0,1]    = 0b011
리소스 B 힌트: NUMA [1,2]    = 0b110
리소스 C 힌트: NUMA [1]      = 0b010

AND 결과:  0b011 & 0b110 & 0b010 = 0b010 = NUMA [1]
preferred: A=0b011, B=0b110, C=0b010 → 서로 다름 → false

다른 순열:
리소스 A 힌트: NUMA [0]      = 0b001
리소스 B 힌트: NUMA [1,2]    = 0b110
AND 결과:  0b001 & 0b110 = 0b000 = 공집합!
→ Count() == 0 → compare()에서 무시
```

### 9.5 compare: 최적 힌트 선택 로직

`compare()` 메서드의 의사 결정 트리:

```
compare(current, candidate):
│
├── candidate.Count() == 0 → 버림 (공집합)
├── current == nil → candidate 채택
│
├── current.Preferred  vs  candidate.Preferred
│   ├── !current && candidate → candidate 채택 (preferred 우선)
│   ├── current && !candidate → current 유지
│   ├── current && candidate → CompareNUMAAffinityMasks (더 좁은 것)
│   └── !current && !candidate → non-preferred 비교 (아래)
│
└── 둘 다 non-preferred:
    ├── current.Count() > BestCount → CompareNUMAAffinityMasks
    ├── current.Count() == BestCount
    │   ├── candidate.Count() != BestCount → current 유지
    │   └── candidate.Count() == BestCount → CompareNUMAAffinityMasks
    └── current.Count() < BestCount
        ├── candidate.Count() > BestCount → current 유지 (오버슈트 방지)
        ├── candidate.Count() == BestCount → candidate 채택
        └── candidate.Count() < BestCount
            ├── candidate > current → candidate (BestCount에 가까워짐)
            ├── candidate < current → current 유지
            └── candidate == current → CompareNUMAAffinityMasks
```

### 9.6 순열 순회: 지수적 복잡도

`iterateAllProviderTopologyHints`는 모든 Provider 힌트의 데카르트 곱을 순회한다. 이는 재귀적으로 구현된 다중 중첩 루프이다.

```go
// pkg/kubelet/cm/topologymanager/policy.go:324-339 (주석에서 발췌)
// 등가 코드:
// for i := 0; i < len(providerHints[0]); i++
//     for j := 0; j < len(providerHints[1]); j++
//         for k := 0; k < len(providerHints[2]); k++
//             ...
//             callback([]TopologyHint{
//                 providerHints[0][i],
//                 providerHints[1][j],
//                 providerHints[2][k], ...
//             })
```

```
복잡도 분석:
  Provider 수: P (CPU, Memory, Device = 3)
  NUMA 노드 수: N
  각 Provider의 힌트 수: 최대 2^N - 1 (모든 비트마스크 조합)

  총 순열 수: (2^N - 1)^P
  N=2, P=3: 3^3 = 27
  N=4, P=3: 15^3 = 3,375
  N=8, P=3: 255^3 = 16,581,375

  → N=8이면 약 1,600만 순열, 허용 가능한 상한
  → N=16이면 65535^3 ≈ 2.8 * 10^14, 비실용적
```

### 9.7 filterProvidersHints: 힌트 전처리

```go
// pkg/kubelet/cm/topologymanager/policy.go:71-101
func filterProvidersHints(logger klog.Logger, providersHints []map[string][]TopologyHint) [][]TopologyHint {
    var allProviderHints [][]TopologyHint
    for _, hints := range providersHints {
        if len(hints) == 0 {
            // HintProvider가 힌트를 제공하지 않음 → "아무 NUMA나 OK" 힌트
            allProviderHints = append(allProviderHints, []TopologyHint{{nil, true}})
            continue
        }
        for resource := range hints {
            if hints[resource] == nil {
                // 해당 리소스에 NUMA 선호 없음 → "아무 NUMA나 OK"
                allProviderHints = append(allProviderHints, []TopologyHint{{nil, true}})
            } else if len(hints[resource]) == 0 {
                // NUMA 배치 불가능 → "어떤 NUMA도 안 됨"
                allProviderHints = append(allProviderHints, []TopologyHint{{nil, false}})
            } else {
                allProviderHints = append(allProviderHints, hints[resource])
            }
        }
    }
    return allProviderHints
}
```

세 가지 특수 케이스:
1. `hints == nil`: Provider가 NUMA를 전혀 신경 쓰지 않음 → `{nil, true}` (preferred any-NUMA)
2. `hints[resource] == nil`: 해당 리소스에 NUMA 선호 없음 → `{nil, true}`
3. `hints[resource]` 길이 0: 가능한 배치가 없음 → `{nil, false}` (non-preferred, 실질적 실패)

### 9.8 CompareNUMAAffinityMasks: Narrowest vs Closest

```go
// pkg/kubelet/cm/topologymanager/policy.go:150-168
func NewHintMerger(numaInfo *NUMAInfo, hints [][]TopologyHint, policyName string, opts PolicyOptions) HintMerger {
    compareNumaAffinityMasks := func(current, candidate *TopologyHint) *TopologyHint {
        if candidate.NUMANodeAffinity.IsEqual(current.NUMANodeAffinity) {
            return current  // 동일하면 기존 유지
        }
        var best bitmask.BitMask
        if (policyName != PolicySingleNumaNode) && opts.PreferClosestNUMA {
            best = numaInfo.Closest(current.NUMANodeAffinity, candidate.NUMANodeAffinity)
        } else {
            best = numaInfo.Narrowest(current.NUMANodeAffinity, candidate.NUMANodeAffinity)
        }
        if best.IsEqual(current.NUMANodeAffinity) { return current }
        return candidate
    }
    // ...
}
```

```
Narrowest (기본): NUMA 노드 수가 적은 것 선호
  [0,1] vs [0,1,2] → [0,1] 선택 (2 < 3)

Closest (PreferClosestNUMA 옵션): NUMA 간 평균 거리가 짧은 것 선호
  [0,1] vs [0,2] → NUMA 거리 비교
  dist(0,1)=12, dist(0,2)=20 → [0,1] 선택 (12 < 20)

주의: single-numa-node 정책에서는 PreferClosestNUMA가 적용되지 않음
  (이미 단일 NUMA만 허용하므로 거리 비교가 무의미)
```

---

## 10. 왜 이런 설계인가

### 10.1 왜 HintProvider 패턴을 사용하는가?

**문제**: CPU, 메모리, 디바이스는 각각 독립적인 할당 로직을 가진다. 이들의 NUMA 배치를 조율하려면 중앙 조정자가 필요하다.

**해결**: HintProvider 인터페이스로 각 Resource Manager를 추상화하고, Topology Manager가 힌트를 수집-병합하는 계층적 아키텍처를 사용한다.

```
장점:
  1. 느슨한 결합 (Loose Coupling)
     - CPU Manager가 Device Manager를 직접 알 필요 없음
     - 새로운 리소스 타입(DRA 등) 추가 시 HintProvider만 구현하면 됨

  2. 관심사 분리 (Separation of Concerns)
     - 각 Manager: "어디에 할당할 수 있는지" 결정 (힌트 생성)
     - Topology Manager: "어디에 할당해야 하는지" 결정 (힌트 병합)

  3. 정책 교체 용이
     - 정책만 바꾸면 동일한 힌트 데이터로 다른 결정 가능
     - kubelet 설정 변경만으로 none → restricted 전환 가능

  4. 확장성
     - DRA(Dynamic Resource Allocation) 등 새로운 Manager 추가가 용이
     - 기존 코드 수정 없이 AddHintProvider()만 호출
```

### 10.2 왜 비트마스크 기반 NUMA 표현인가?

```
장점:
  1. 공간 효율: 8-NUMA 시스템을 8비트(1바이트)로 표현
  2. 연산 효율: AND/OR/Count 등이 비트 연산으로 O(1)
  3. 순회 효율: IterateBitMasks로 모든 NUMA 조합을 체계적으로 순회
  4. 비교 효율: IsEqual, IsNarrowerThan 등이 O(1)

bitmask.And(0b0011, 0b0110) = 0b0010  <-- CPU 1번 연산
vs
intersect([]int{0,1}, []int{1,2}) = []int{1}  <-- 비교 연산 필요, 메모리 할당
```

### 10.3 왜 CPU Manager에 Packed/Spread 두 가지 전략이 있는가?

```
Packed (기본값):
  - 하나의 NUMA/소켓에 CPU를 집중 배치
  - 메모리 지역성 극대화
  - 적합: 단일 스레드/NUMA-bound 워크로드, 지연 시간 민감

Spread (DistributeCPUsAcrossCores):
  - 여러 코어에 분산 배치
  - 열 분산, 터보 부스트 활용
  - 적합: 멀티스레드 워크로드, 열 관리 중요

이 두 전략은 상충하는 최적화 목표를 반영한다:
  Packed  → 메모리 접근 지역성 최적화
  Spread  → 열/전력 분산 최적화
```

### 10.4 왜 restricted가 best-effort를 상속(임베딩)하는가?

```go
// pkg/kubelet/cm/topologymanager/policy_restricted.go:21-23
type restrictedPolicy struct {
    bestEffortPolicy  // 구조체 임베딩
}
```

restricted 정책은 best-effort와 **동일한 병합 로직**을 사용한다. 차이점은 `canAdmitPodResult()`에서 `hint.Preferred`를 확인하는 것 **하나**뿐이다.

```
best-effort:  Merge() → 최적 힌트 사용, 실패해도 admit (canAdmitPodResult: true)
restricted:   Merge() → 최적 힌트 사용, Preferred 아니면 reject (canAdmitPodResult: hint.Preferred)

→ best-effort를 임베딩하면 Merge 로직 중복 제거
→ canAdmitPodResult()만 오버라이드하면 됨
→ Go의 구조체 임베딩 패턴을 활용한 최소 코드 변경 설계
```

### 10.5 왜 defaultMaxAllowableNUMANodes = 8인가?

순열 순회의 지수적 복잡도 때문이다:

| NUMA 수 | 비트마스크 조합 | 3-Provider 순열 | 예상 처리 시간 |
|---------|--------------|----------------|-------------|
| 2 | 3 | 27 | < 1ms |
| 4 | 15 | 3,375 | < 1ms |
| 8 | 255 | 16,581,375 | ~수십ms |
| 16 | 65,535 | ~2.8 * 10^14 | 실용 불가 |

현실적으로 대부분의 서버는 2~4 NUMA 노드를 가지며, 8 NUMA 노드를 초과하는 시스템은 매우 드물다.

### 10.6 왜 Memory Manager에 힌트 확장(extend) 로직이 있는가?

Topology Manager가 반환한 NUMA affinity가 메모리 요청량을 충족하지 못할 수 있다. 이는 다른 Resource Manager의 힌트에 의해 좁은 NUMA 세트가 선택되었지만, 해당 NUMA 노드에 충분한 메모리가 없는 경우 발생한다.

```
예시:
  Topology Manager 결과: NUMA [0]
  Pod 요청: memory=20GiB
  NUMA 0 가용: 15GiB <-- 부족!

  Memory Manager 확장:
  NUMA [0,1]로 확장 → 15+15=30GiB >= 20GiB <-- 충족!
```

CPU Manager에는 이런 확장 로직이 없다. CPU 할당은 개수 기반이므로 Topology Manager 힌트가 CPU 수를 기준으로 생성되기 때문이다. 반면 메모리는 크기(바이트) 기반이므로 NUMA 노드별 가용량에 따라 부족할 수 있다.

### 10.7 왜 Single vs Cross NUMA 할당 규칙이 필요한가?

한 NUMA 노드에서 single-NUMA 할당(cells=[0])과 cross-NUMA 할당(cells=[0,1])이 혼재되면, single-NUMA 워크로드가 예상치 못한 메모리 경합을 겪을 수 있다.

```
문제 시나리오:
  NUMA 0에 Pod-A (cells=[0], single-NUMA, 10GiB) 할당
  NUMA 0에 Pod-B (cells=[0,1], cross-NUMA, 20GiB) 할당

  Pod-B가 NUMA 0의 메모리를 사용하면 Pod-A의 로컬 메모리 대역폭이 감소
  → Pod-A는 전용 NUMA 배치를 기대했지만 실제로는 경합 발생

해결:
  각 NUMA 노드에서 single과 cross 할당을 혼재시키지 않음
  → single-NUMA 워크로드의 예측 가능한 성능 보장
```

### 10.8 왜 각 Manager가 독립적인 State를 가지는가?

CPU, Memory, Device 각각이 자체 상태 파일을 관리한다:
- CPU: `cpu_manager_state`
- Memory: `memory_manager_state`
- Device: checkpoint 파일

이는 kubelet 재시작 시 각 Manager가 독립적으로 상태를 복원할 수 있게 한다. 하나의 Manager 상태가 손상되더라도 다른 Manager에 영향을 주지 않는다.

---

## 11. 정리

### 핵심 구성 요소 요약

```
+------------------------------------------------------------------+
|                    Topology Manager                               |
|  +-----------------------------------------------------------+   |
|  | Policy: none / best-effort / restricted / single-numa-node |   |
|  |                                                             |   |
|  | Merge():                                                    |   |
|  |   1. filterProvidersHints() → allProviderHints              |   |
|  |   2. [single-numa-node only] filterSingleNumaHints()        |   |
|  |   3. NewHintMerger() → HintMerger                          |   |
|  |   4. iterateAllProviderTopologyHints() → permutation        |   |
|  |   5. mergePermutation() → bitwise AND                      |   |
|  |   6. compare() → bestHint                                  |   |
|  |   7. canAdmitPodResult(bestHint) → admit/reject            |   |
|  +-----------------------------------------------------------+   |
|           |              |              |                         |
|  +--------v--+  +--------v--+  +--------v--+                     |
|  |CPU Manager|  |Mem Manager|  |Dev Manager|                     |
|  |           |  |           |  |           |                      |
|  |staticPol. |  |staticPol. |  |ManagerImpl|                     |
|  |cpuAccum.  |  |Block/     |  |generate   |                     |
|  |Packed/    |  |NUMANode   |  |DeviceTopo |                     |
|  |Spread     |  |State      |  |logyHints  |                     |
|  +-----------+  +-----------+  +-----------+                     |
+------------------------------------------------------------------+
```

### 정책 선택 가이드

| 워크로드 유형 | 권장 정책 | 이유 |
|-------------|----------|------|
| 일반 웹 서비스 | none | NUMA 지역성 불필요, 스케줄링 유연성 최대화 |
| 데이터 처리 (Spark, Flink) | best-effort | NUMA 고려하되 가용성 우선 |
| ML 학습 (GPU 사용) | restricted | GPU와 CPU/메모리의 NUMA 정렬 필요 |
| 텔레코 (5G, DPDK) | single-numa-node | 지연 시간 최소화, 완전한 NUMA 지역성 |
| HPC (MPI) | single-numa-node | 노드 내 통신 최적화 |

### CPU Manager 정책 선택 가이드

| 워크로드 유형 | CPU 정책 | CPU 옵션 |
|-------------|---------|---------|
| 일반 | none | - |
| 지연 시간 민감 | static | Packed (기본) |
| 보안 민감 (SMT 격리) | static | FullPhysicalCPUsOnly |
| 멀티스레드, 열 관리 | static | DistributeCPUsAcrossCores (Spread) |

### 소스코드 참조 목록

| 파일 | 핵심 내용 |
|------|----------|
| `pkg/kubelet/cm/cpumanager/cpu_manager.go` | CPU Manager 인터페이스 (56-103행) |
| `pkg/kubelet/cm/cpumanager/policy_static.go` | Static Policy 구조체 (108-128행), Allocate() (319-410행) |
| `pkg/kubelet/cm/cpumanager/cpu_assignment.go` | cpuAccumulator (259-299행), Packed/Spread (252-257행) |
| `pkg/kubelet/cm/cpumanager/state/state.go` | CPU 상태 인터페이스 (24-58행) |
| `pkg/kubelet/cm/memorymanager/memory_manager.go` | Memory Manager 인터페이스 (58-95행) |
| `pkg/kubelet/cm/memorymanager/policy_static.go` | Static Policy (46-58행), Allocate() (98-210행) |
| `pkg/kubelet/cm/memorymanager/state/state.go` | MemoryTable, NUMANodeState, Block (24-100행) |
| `pkg/kubelet/cm/devicemanager/manager.go` | ManagerImpl (62-116행) |
| `pkg/kubelet/cm/devicemanager/topology_hints.go` | GetTopologyHints() (33-84행), generateDeviceTopologyHints() (154-219행) |
| `pkg/kubelet/cm/topologymanager/topology_manager.go` | Manager (58-70행), HintProvider (80-96행), TopologyHint (105-110행) |
| `pkg/kubelet/cm/topologymanager/policy.go` | HintMerger (137-148행), Merge() (303-322행), mergePermutation() (44-69행), compare() (180-300행) |
| `pkg/kubelet/cm/topologymanager/numa_info.go` | NUMAInfo (28-31행), NUMADistances, CalculateAverageFor() (93-109행) |
| `pkg/kubelet/cm/topologymanager/policy_none.go` | nonePolicy (항상 admit) |
| `pkg/kubelet/cm/topologymanager/policy_best_effort.go` | bestEffortPolicy (병합 + 항상 admit) |
| `pkg/kubelet/cm/topologymanager/policy_restricted.go` | restrictedPolicy (bestEffortPolicy 임베딩, Preferred 검사) |
| `pkg/kubelet/cm/topologymanager/policy_single_numa_node.go` | singleNumaNodePolicy, filterSingleNumaHints() |
| `pkg/kubelet/cm/container_manager_linux.go` | Resource Manager 초기화 및 등록 (312-356행) |
