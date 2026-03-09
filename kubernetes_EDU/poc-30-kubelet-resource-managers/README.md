# PoC 30: Kubelet Resource Manager 시뮬레이션

## 개요

Kubernetes Kubelet의 4개 Resource Manager(CPU Manager, Memory Manager, Device Manager, Topology Manager)가 Pod Admission 시 NUMA 토폴로지를 고려하여 최적의 리소스 할당을 수행하는 과정을 시뮬레이션한다.

## 실행 방법

```bash
go run main.go
```

외부 의존성 없이 Go 표준 라이브러리만 사용한다.

## 시뮬레이션 내용

### 1. NUMA 토폴로지 표현

- `BitMask`: NUMA 노드 비트마스크 (비트 연산 기반)
- `NUMAInfo`: NUMA 노드 목록 및 노드 간 거리 행렬
- `CPUTopology`: 소켓/코어/스레드 토폴로지

실제 소스: `pkg/kubelet/cm/topologymanager/bitmask/`, `pkg/kubelet/cm/topologymanager/numa_info.go`

### 2. CPU 할당 (Static Policy)

- `cpuAccumulator`: CPU 선택 알고리즘
- Packed 전략: 하나의 NUMA 노드에 집중 배치 (메모리 지역성 최대화)
- Spread 전략: 코어 간 분산 배치 (열/전력 분산)
- NUMA affinity에 따른 우선 할당

실제 소스: `pkg/kubelet/cm/cpumanager/cpu_assignment.go`, `pkg/kubelet/cm/cpumanager/policy_static.go`

### 3. Topology Hint 생성 및 병합

- 각 Resource Manager가 NUMA 비트마스크 기반 TopologyHint 생성
- `mergePermutation()`: 순열 내 힌트를 비트와이즈 AND로 병합
- `HintMerger.Merge()`: 모든 순열을 순회하여 최적 힌트 선택
- `compare()`: preferred 우선, 좁은 NUMA 우선, BestNonPreferredAffinityCount 기준

실제 소스: `pkg/kubelet/cm/topologymanager/policy.go`

### 4. 정책 비교

4개 정책의 동작 차이를 동일한 시나리오에서 비교한다:

| 정책 | 힌트 병합 | Admit 조건 |
|------|----------|-----------|
| `none` | 수행하지 않음 | 항상 허용 |
| `best-effort` | 수행, 최적 선택 | 항상 허용 |
| `restricted` | 수행, 최적 선택 | Preferred 필요 |
| `single-numa-node` | 단일 NUMA만, 최적 선택 | Preferred 필요 |

실제 소스: `pkg/kubelet/cm/topologymanager/policy_none.go`, `policy_best_effort.go`, `policy_restricted.go`, `policy_single_numa_node.go`

### 5. Resource Manager 조율 흐름

CPU Manager + Memory Manager + Device Manager가 Topology Manager의 조율 하에 Pod를 할당하는 전체 흐름:

1. Resource Manager 초기화
2. Pod 요청 수신
3. 각 HintProvider에서 TopologyHint 수집
4. 정책에 따른 힌트 병합 및 Admit 결정
5. Admit 성공 시 각 Manager의 Allocate() 호출
6. 최종 상태 확인

실제 소스: `pkg/kubelet/cm/container_manager_linux.go` (312-356행)

### 6. PreferClosestNUMA 비교

4-NUMA 시스템에서 Narrowest(기본) vs Closest(PreferClosestNUMA) 전략의 차이를 시연한다.

- Narrowest: NUMA 노드 수가 적은 마스크 선호
- Closest: 평균 NUMA 간 거리가 짧은 마스크 선호

실제 소스: `pkg/kubelet/cm/topologymanager/numa_info.go`

## 핵심 알고리즘

### 비트마스크 기반 NUMA 표현

```
NUMA [0,1] = 0b11 = 3
NUMA [0]   = 0b01 = 1
NUMA [1]   = 0b10 = 2

AND 연산: [0,1] & [1] = 0b11 & 0b10 = 0b10 = [1]
```

### 순열 기반 힌트 병합

```
Provider A 힌트: [NUMA 0], [NUMA 1], [NUMA 0,1]
Provider B 힌트: [NUMA 1]
Provider C 힌트: [NUMA 0], [NUMA 1]

순열 순회:
  A[0] & B[1] & C[0] = [0] & [1] & [0] = 공집합 (무시)
  A[0] & B[1] & C[1] = [0] & [1] & [1] = 공집합 (무시)
  A[1] & B[1] & C[0] = [1] & [1] & [0] = 공집합 (무시)
  A[1] & B[1] & C[1] = [1] & [1] & [1] = [1] (preferred!)
  ...
```

## 대응 소스코드

| PoC 구조체/함수 | Kubernetes 소스 |
|----------------|----------------|
| `BitMask` | `pkg/kubelet/cm/topologymanager/bitmask/` |
| `NUMAInfo` | `pkg/kubelet/cm/topologymanager/numa_info.go` (28-31행) |
| `TopologyHint` | `pkg/kubelet/cm/topologymanager/topology_manager.go` (105-110행) |
| `cpuAccumulator` | `pkg/kubelet/cm/cpumanager/cpu_assignment.go` (259-299행) |
| `mergePermutation()` | `pkg/kubelet/cm/topologymanager/policy.go` (44-69행) |
| `HintMerger.Merge()` | `pkg/kubelet/cm/topologymanager/policy.go` (303-322행) |
| `compare()` | `pkg/kubelet/cm/topologymanager/policy.go` (180-300행) |
| `nonePolicy` | `pkg/kubelet/cm/topologymanager/policy_none.go` |
| `bestEffortPolicy` | `pkg/kubelet/cm/topologymanager/policy_best_effort.go` |
| `restrictedPolicy` | `pkg/kubelet/cm/topologymanager/policy_restricted.go` |
| `singleNumaNodePolicy` | `pkg/kubelet/cm/topologymanager/policy_single_numa_node.go` |
| `CPUResourceManager` | `pkg/kubelet/cm/cpumanager/cpu_manager.go` (56-103행) |
| `MemoryResourceManager` | `pkg/kubelet/cm/memorymanager/memory_manager.go` (58-95행) |
| `DeviceResourceManager` | `pkg/kubelet/cm/devicemanager/manager.go` (62-116행) |
