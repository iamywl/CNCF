# PoC-15: Cloud Provider IPAM (AWS ENI 시뮬레이션)

## 개요

Cilium은 클라우드 환경에서 Pod 네트워킹을 위해 클라우드 프로바이더의 네이티브 네트워크 인터페이스를 직접 활용한다.
이 PoC는 AWS ENI(Elastic Network Interface) 기반 IPAM 모드의 전체 파이프라인을 시뮬레이션한다.
인스턴스 타입별 리밋 관리, 서브넷 선택, ENI 생성/연결, Secondary IP 할당/해제까지
실제 `pkg/aws/eni/` 코드의 핵심 알고리즘을 Go 표준 라이브러리만으로 재현한다.

---

## 배경 지식

### 클라우드 프로바이더 통합이 필요한 이유

전통적인 오버레이 네트워크(VXLAN, Geneve)는 패킷 캡슐화로 인한 성능 오버헤드가 있다.
클라우드 환경에서는 프로바이더의 네이티브 네트워크 인터페이스를 활용하면
**캡슐화 없이 직접 VPC 라우팅**을 사용할 수 있어 지연 시간과 처리량 모두 개선된다.

### AWS ENI / Azure IPAM의 원리

AWS EC2 인스턴스에는 여러 ENI를 연결할 수 있고, 각 ENI에 여러 Secondary Private IP를 할당할 수 있다.
Cilium은 이 메커니즘을 활용한다:

1. **ENI를 동적으로 생성/연결** -- EC2 API(`CreateNetworkInterface`, `AttachNetworkInterface`) 호출
2. **Secondary IP를 Pod에 직접 할당** -- `AssignPrivateIpAddresses`로 IP 확보
3. **VPC 라우팅으로 직접 통신** -- 오버레이 없이 VPC 내부 라우팅으로 Pod 간 통신

### 서브넷 선택 알고리즘

Cilium은 다단계 우선순위로 서브넷을 선택한다: **SubnetIDs 직접 지정** > **SubnetTags 매칭** > **NodeSubnetID** > **같은 라우트 테이블** > **폴백**. 후보 중 **가용 IP가 가장 많은 서브넷**을 최종 선택한다.

### IP 할당/해제 사이클

- **부족 시**: `PrepareIPAllocation` -> 기존 ENI 여유 확인 -> `AllocateIPs` 또는 `CreateInterface`
- **초과 시**: `PrepareIPRelease` -> 미사용 IP가 많은 ENI 선택 -> `ReleaseIPs`

---

## 시뮬레이션하는 개념

| 개념 | 실제 Cilium 경로 | PoC 구현 |
|------|-----------------|---------|
| 인스턴스 타입 리밋 | `pkg/aws/eni/limits/limits.go` | `LimitsGetter` -- 타입별 MaxENI/MaxIPv4 캐시 |
| EC2 인프라 상태 관리 | `pkg/aws/eni/instances.go` | `InstancesManager` -- 서브넷/ENI 전체 관리 |
| 노드별 ENI 할당/해제 | `pkg/aws/eni/node.go` | `Node` -- NodeOperations 인터페이스 구현 |
| AWS EC2 API 추상화 | `pkg/aws/eni/instances.go` | `EC2API` 인터페이스 + `mockEC2API` |
| 서브넷 선택 | `FindSubnetByTags/IDs` | `FindBestSubnet` -- VPC/AZ 매칭 후 가용 IP 최대 선택 |
| IP 할당 계획/실행 | `PrepareIPAllocation/AllocateIPs` | 기존 ENI 여유 계산 후 Secondary IP 추가 |
| ENI 생성 파이프라인 | `CreateInterface` (최대 5회 재시도) | 서브넷선택 -> ENI생성 -> 연결 -> IP할당 |
| IP 해제 | `PrepareIPRelease/ReleaseIPs` | 미사용 IP 최다 ENI에서 해제 |

---

## 아키텍처 다이어그램

```
IPAM Controller (주기적 실행)
  IP 부족? -> PrepareIPAllocation -> AllocateIPs / CreateInterface
  IP 초과? -> PrepareIPRelease    -> ReleaseIPs
       |                              |
       v                              v
+-------------------+       +-------------------+
| InstancesManager  |       |  LimitsGetter     |
| - subnets{}       |       |  m5.large: 3/10   |
| - instances{}     |       |  t3.small: 3/4    |
| - FindBestSubnet  |       |  r5.4xlarge: 8/30 |
+-------------------+       +-------------------+
       |
       v
+-------------------+  EC2 API  +----------------+
|  Node (per K8s)   | -------> |  EC2API (mock)  |
|  - enis{}         |          |  - ENI 생성/연결  |
|  - usedIPs{}      |          |  - IP 할당/해제   |
|  - firstIfaceIdx  |          +----------------+
+-------------------+
```

### ENI 생성 상세 흐름

```
CreateInterface()
  +--[1] FindBestSubnet(vpcID, az) -- 가용 IP 최대 서브넷 선택
  +--[2] toAllocate = min(MaxIPsToAllocate, limits.IPv4 - 1)
  +--[3] findNextIndex(firstIfaceIndex) -- 빈 인덱스 탐색
  +--[4] CreateNetworkInterface() -- ENI + Primary/Secondary IPs 생성
  +--[5] AttachNetworkInterface() -- 최대 5회 재시도, 실패 시 cleanup
  +--[6] 매니저 + 노드에 ENI 등록
```

---

## 코드 해설

### 1. `LimitsGetter` -- 인스턴스 타입별 리밋 캐시

EC2 인스턴스 타입마다 연결 가능한 최대 ENI 수(`Adapters`)와 ENI당 최대 IPv4 수(`IPv4`)가 다르다.
실제 Cilium에서는 `DescribeInstanceTypes` API를 Trigger 패턴(최소 1분 간격)으로 호출하여 동적 갱신한다.
PoC에서는 `map[string]Limits`에 하드코딩된 값을 사용한다.

### 2. `InstancesManager` -- EC2 인프라 전체 상태 관리

서브넷, 인스턴스, ENI 매핑을 관리하는 싱글턴이다.
`FindBestSubnet(vpcID, az)`은 VPC와 가용영역이 일치하는 서브넷 중 `AvailableAddresses`가 가장 큰 것을 반환한다.
실제 구현에서는 `Resync()`(전체 동기화)와 `InstanceSync()`(증분 동기화) 두 단계로 EC2 API 결과를 캐시한다.

### 3. `Node.PrepareIPAllocation()` / `AllocateIPs()` -- IP 할당 계획/실행

`PrepareIPAllocation`은 기존 ENI를 순회하며 `effectiveLimits(= IPv4 - 1)`와 현재 주소 수의 차이를 계산한다.
`firstIfaceIndex`(보통 1) 미만의 ENI(eth0)는 건너뛴다.
여유가 있으면 `AllocateIPs`로 기존 ENI에 Secondary IP를 추가하고,
빈 슬롯이 있으면 `CreateInterface`로 새 ENI를 생성한다.

### 4. `Node.CreateInterface()` -- 새 ENI 생성 파이프라인

이 함수가 ENI IPAM의 핵심이다. 서브넷 선택 -> ENI 생성 -> 인스턴스 연결 -> 등록의 전체 흐름을 수행한다.
`AttachNetworkInterface`는 인덱스 충돌을 대비해 최대 5회 재시도하며,
모든 재시도가 실패하면 생성한 ENI를 삭제하여 리소스 누수를 방지한다.

### 5. `Node.PrepareIPRelease()` / `ReleaseIPs()` -- IP 해제

ENI를 정렬된 ID 순서로 순회하면서, 각 ENI에서 `usedIPs`에 등록되지 않은 미사용 Secondary IP를 수집한다.
미사용 IP가 가장 많은 ENI를 선택하여 해제 대상으로 지정한다.
이렇게 하면 특정 ENI에 미사용 IP를 집중시켜 향후 ENI 자체를 해제할 가능성을 높인다.

---

## 실행 방법 및 예상 출력

```bash
go run main.go
```

시뮬레이션은 8단계로 진행되며, 주요 출력은 다음과 같다:

```
[2] 노드 생성 및 인스턴스 타입별 리밋
  인스턴스       타입         MaxENI IPv4/ENI   최대할당가능
  -------------------------------------------------------
  i-node001    m5.large          3       10         18
  i-node002    t3.small          3        4          6
  i-node003    r5.4xlarge        8       30        203

[4] ENI 생성 및 IP 할당 (PrepareIPAllocation -> CreateInterface)
  --- i-node001 (m5.large) ---
    PrepareIPAllocation: 기존ENI할당가능=0, 후보ENI=0, 빈슬롯=2
    ENI 생성: eni-00000001 (index=1, attach=eni-attach-00000001, IPs=9개)

[6] IP 해제 (PrepareIPRelease -> ReleaseIPs)
  i-node001: ENI=eni-00000001에서 3개 IP 해제 대상
    해제 완료 (ec2api.UnassignPrivateIpAddresses 호출)
```

---

## 핵심 포인트

- **인스턴스 타입이 곧 제약 조건이다**: `m5.large`는 3개 ENI x 10 IPv4, `r5.4xlarge`는 8개 ENI x 30 IPv4. 인스턴스 타입 선택이 클러스터의 Pod 밀도를 결정한다.
- **firstIfaceIndex = 1**: 0번 ENI(eth0)는 노드 자체 통신용이다. Cilium은 1번 인덱스부터 관리하여 노드 네트워크를 보호한다.
- **effectiveLimits = IPv4 - 1**: ENI당 최대 IPv4에서 Primary IP를 제외한 나머지가 Pod에 할당 가능한 Secondary IP이다.
- **서브넷 선택은 가용 IP 최대 우선**: IP 고갈을 방지하기 위해 남은 주소가 가장 많은 서브넷을 선택한다.
- **실패 시 cleanup**: ENI 생성 후 연결이 실패하면 반드시 ENI를 삭제하여 리소스 누수를 방지한다.
- **IP 해제는 미사용 집중 전략**: 미사용 IP가 가장 많은 ENI에서 우선 해제하여, 해당 ENI를 완전히 비울 가능성을 높인다.

---

## 실제 Cilium과의 차이점

| 항목 | 실제 Cilium | 이 PoC |
|------|-----------|--------|
| EC2 API 호출 | 실제 AWS SDK (`aws-sdk-go-v2`) | `mockEC2API`로 시뮬레이션 |
| 리밋 갱신 | Trigger 패턴 (최소 1분 간격, 5초 타임아웃 + jitter) | 하드코딩된 정적 맵 |
| 서브넷 선택 | SubnetIDs/Tags/라우트테이블 등 다단계 우선순위 | VPC/AZ 매칭 후 가용 IP 최대만 |
| 동시성 | `resyncLock` (전체 vs 증분 동기화), CiliumNode CRD watch | 단순 `sync.RWMutex` |
| Prefix Delegation | Nitro 인스턴스에서 /28 접두사 단위 할당 지원 | 미구현 |
| 보안 그룹 | SecurityGroupTags 매칭, eth0 상속 등 | 고정 값 전달 |
| ENI 태깅/CRD | `io.cilium/*` 태깅, CiliumNode CRD 연동 | 미구현 |
| 에러 처리 | `isAttachmentIndexConflict` 등 세밀한 분류 | 단순 재시도 |
