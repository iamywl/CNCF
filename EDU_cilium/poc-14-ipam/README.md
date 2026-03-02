# PoC 14: Cilium IPAM (IP Address Management) 시뮬레이션

## 개요

이 PoC는 Cilium의 IPAM(IP Address Management) 서브시스템의 핵심 메커니즘을 순수 Go 표준 라이브러리만으로 시뮬레이션합니다. 실제 Cilium 코드 구조와 알고리즘을 참고하여, 다양한 IPAM 모드의 동작 원리를 이해할 수 있습니다.

## 실행 방법

```bash
cd EDU/poc-14-ipam
go run main.go
```

외부 의존성이 없으므로 `go mod`나 추가 패키지 설치가 필요하지 않습니다.

## 시뮬레이션 내용

### 데모 1: Cluster-Pool IPAM (hostScopeAllocator)

Cilium의 기본 IPAM 모드인 cluster-pool을 시뮬레이션합니다. Operator가 노드에 할당한 CIDR 블록(예: `10.244.1.0/28`)에서 순차적으로 파드에 IP를 할당하고 해제합니다.

**시뮬레이션 항목:**
- CIDR 기반 IP 순차 할당
- IP 해제 후 재활용
- 풀 상태 덤프 (할당/총용량)

**참조 소스:**
- `pkg/ipam/hostscope.go` - `hostScopeAllocator`
- `pkg/ipam/ipam.go` - `ConfigureAllocator()`

### 데모 2: Multi-Pool IPAM

`CiliumPodIPPool` CRD 기반 다중 풀 IPAM을 시뮬레이션합니다. 여러 IP 풀(production, staging, monitoring)을 생성하고 파드의 annotation에 따라 적절한 풀에서 IP를 할당합니다.

**시뮬레이션 항목:**
- 풀 생성 (IPv4/IPv6 듀얼스택 포함)
- Annotation 기반 풀 선택
- 풀별 IP 수요 계산 (`neededIPCeil` 알고리즘)
- 풀별 프리얼로케이션 목표 계산

**참조 소스:**
- `pkg/ipam/multipool.go` - `multiPoolAllocator`
- `pkg/ipam/multipool_manager.go` - `multiPoolManager`, `neededIPCeil()`
- `pkg/ipam/pool.go` - `cidrPool`
- `pkg/ipam/metadata/manager.go` - 풀 선택 로직

### 데모 3: ENI-Style IPAM (인터페이스 기반)

AWS ENI 모드를 시뮬레이션합니다. EC2 인스턴스에 ENI(Elastic Network Interface)를 부착하고, 각 ENI에 보조(secondary) IP를 할당한 후 파드에 배분하는 과정을 보여줍니다.

**시뮬레이션 항목:**
- ENI 생성 및 보조 IP 할당
- CiliumNode CRD의 `spec.ipam.pool`을 통한 IP 목록 관리
- 파드에 IP 할당 시 MAC, 게이트웨이, VPC CIDR 정보 포함
- IP 소진 시 새 인터페이스 자동 생성 (poolMaintainer)
- 워터마크 기반 IP 수요/초과 계산

**참조 소스:**
- `pkg/ipam/crd.go` - `crdAllocator`, `nodeStore`
- `pkg/ipam/node_manager.go` - `NodeManager`
- `pkg/ipam/node.go` - `Node`, `MaintainIPPool()`, `createInterface()`
- `pkg/ipam/allocator/aws/aws.go` - `AllocatorAWS`

### 데모 4: Pre-allocation (워터마크 기반 자동 보충)

Cilium의 핵심 프리얼로케이션 알고리즘을 시연합니다. `calculateNeededIPs`와 `calculateExcessIPs` 함수의 동작을 다양한 파라미터 조합으로 보여줍니다.

**시뮬레이션 항목:**
- `calculateNeededIPs()`: preAllocate, minAllocate, maxAllocate 기반 필요 IP 계산
- `calculateExcessIPs()`: maxAboveWatermark 기반 초과 IP 감지
- 워터마크 파라미터 변화에 따른 결과 테이블

**참조 소스:**
- `pkg/ipam/node.go` - `calculateNeededIPs()`, `calculateExcessIPs()`

### 데모 5: IP 풀 고갈 및 복구

작은 CIDR(`/30`, 2개 IP만 사용 가능)에서 IP를 모두 소진시킨 후, Operator가 새 CIDR을 할당하여 복구하는 과정을 시뮬레이션합니다.

**시뮬레이션 항목:**
- IP 풀 고갈 (모든 IP 소진)
- Operator의 새 CIDR 할당으로 복구
- 해제된 IP 재활용

**참조 소스:**
- `pkg/ipam/pool.go` - `cidrPool.allocateNext()`, `cidrPool.updatePool()`

### 데모 6: 듀얼스택 (IPv4 + IPv6 동시 할당)

하나의 파드에 IPv4와 IPv6 주소를 동시에 할당하는 듀얼스택 동작을 시뮬레이션합니다. IPv6 풀 고갈 시 이미 할당된 IPv4 주소를 자동 롤백하는 안전 메커니즘도 포함합니다.

**시뮬레이션 항목:**
- 듀얼스택 동시 할당 (`family=""`)
- IPv4만 또는 IPv6만 할당
- IPv6 고갈 시 IPv4 자동 롤백
- 패밀리별 해제

**참조 소스:**
- `pkg/ipam/allocator.go` - `AllocateNext()`, `AllocateNextFamily()`

## 코드 구조

```
main.go
├── 공통 타입 (Family, Pool, AllocationResult, Allocator)
├── CIDRAllocator        - 비트맵 기반 IP 할당기 (ipallocator.Range 시뮬레이션)
├── HostScopeAllocator   - cluster-pool 모드 (hostScopeAllocator 시뮬레이션)
├── CIDRPool             - 다중 CIDR 풀 (cidrPool 시뮬레이션)
├── MultiPoolManager     - 다중 풀 관리 (multiPoolManager 시뮬레이션)
├── ENINode              - ENI 기반 노드 (Node + crdAllocator 시뮬레이션)
├── DualStackIPAM        - 듀얼스택 관리 (IPAM 구조체 시뮬레이션)
├── PreAllocManager      - 프리얼로케이션 관리
├── calculateNeededIPs() - 워터마크 기반 필요 IP 계산
├── calculateExcessIPs() - 초과 IP 계산
├── neededIPCeil()       - Multi-pool 수요 올림 계산
└── 6개 데모 함수
```

## Cilium 실제 코드와의 대응

| PoC 구성요소 | Cilium 실제 파일 | 설명 |
|-------------|-----------------|------|
| `CIDRAllocator` | `pkg/ipam/service/ipallocator/` | CIDR 범위 내 IP 비트맵 할당 |
| `HostScopeAllocator` | `pkg/ipam/hostscope.go` | cluster-pool/kubernetes 모드 |
| `CIDRPool` | `pkg/ipam/pool.go` | 다중 CIDR 관리 풀 |
| `MultiPoolManager` | `pkg/ipam/multipool_manager.go` | multi-pool 모드 관리자 |
| `ENINode` | `pkg/ipam/node.go` + `pkg/ipam/crd.go` | CRD 기반 모드 노드 |
| `DualStackIPAM` | `pkg/ipam/allocator.go` | 듀얼스택 할당 로직 |
| `calculateNeededIPs` | `pkg/ipam/node.go` | 워터마크 계산 (동일 알고리즘) |
| `calculateExcessIPs` | `pkg/ipam/node.go` | 초과 계산 (동일 알고리즘) |
| `neededIPCeil` | `pkg/ipam/multipool_manager.go` | 풀별 수요 올림 (동일 알고리즘) |

## 핵심 알고리즘 요약

### calculateNeededIPs

```
neededIPs = preAllocate - (availableIPs - usedIPs)
neededIPs = max(neededIPs, minAllocate - availableIPs)
if maxAllocate > 0: neededIPs = min(neededIPs, maxAllocate - availableIPs)
neededIPs = max(neededIPs, 0)
```

### neededIPCeil (multi-pool)

```
neededIPCeil(numIP, preAlloc) = ((numIP / preAlloc) + 1 + (1 if numIP%preAlloc > 0)) * preAlloc
```

항상 최소 `preAlloc` 만큼의 여유 IP를 확보하도록 올림합니다.

### IP 해제 핸드셰이크 (CRD 기반 모드)

```
Operator: marked-for-release  ->  Agent: ready-for-release / do-not-release
Operator: released (IP 제거)  ->  Agent: 핸드셰이크 항목 삭제
```

## 관련 문서

- `EDU/14-ipam.md` - IPAM 서브시스템 종합 분석 문서 (한국어)
