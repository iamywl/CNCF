# PoC-15: Cloud Provider IPAM (AWS ENI)

## 개요

AWS ENI(Elastic Network Interface) 기반 IPAM을 시뮬레이션한다.
Cilium이 EC2 인스턴스의 ENI와 Secondary IP를 관리하여 Pod에 IP를 할당하는
전체 파이프라인을 재현한다.

## 시뮬레이션 대상

| 컴포넌트 | 실제 Cilium 경로 | PoC 구현 |
|---------|-----------------|---------|
| LimitsGetter | `pkg/aws/eni/limits/limits.go` | 인스턴스 타입별 리밋 캐시 |
| InstancesManager | `pkg/aws/eni/instances.go` | EC2 인프라 상태 관리 |
| Node (NodeOperations) | `pkg/aws/eni/node.go` | ENI 할당/해제 구현 |
| EC2API | `pkg/aws/eni/instances.go` | AWS EC2 API 추상화 |

## 핵심 개념

1. **인스턴스 타입별 리밋**: Adapters(최대 ENI 수), IPv4(ENI당 최대 IP) 제약
2. **NodeOperations 인터페이스**: PrepareIPAllocation, AllocateIPs, CreateInterface 등
3. **서브넷 선택**: VPC/AZ 매칭 후 가용 주소 최대 서브넷 우선
4. **ENI 생성 파이프라인**: 서브넷선택 -> 보안그룹 -> ENI생성 -> 연결 -> IP할당
5. **IP 해제**: 사용되지 않는 IP가 가장 많은 ENI에서 해제

## 실행

```bash
go run main.go
```

## 주요 흐름

```
PrepareIPAllocation()
  └─ 기존 ENI에 여유가 있으면 → AllocateIPs()
  └─ ENI 슬롯이 남아있으면 → CreateInterface()
       1. findSuitableSubnet()
       2. getSecurityGroupIDs()
       3. CreateNetworkInterface()
       4. AttachNetworkInterface() (최대 5회 재시도)
       5. ModifyNetworkInterface() (DeleteOnTermination)

PrepareIPRelease()
  └─ 미사용 IP가 가장 많은 ENI 선택 → ReleaseIPs()
```
