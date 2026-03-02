# PoC 15: Cilium 클라우드 프로바이더 통합 시뮬레이션

## 개요

이 PoC는 Cilium의 클라우드 프로바이더 통합 메커니즘을 순수 Go 표준 라이브러리만으로 시뮬레이션한다. AWS ENI, Azure NIC, Alibaba Cloud ENI의 IPAM(IP Address Management) 동작을 실제 코드베이스의 구조를 반영하여 구현하였다.

## 실행 방법

```bash
cd EDU/poc-15-cloud-provider
go run main.go
```

외부 의존성이 없으므로 `go mod`나 추가 패키지 설치가 필요하지 않다.

## 시뮬레이션 항목

### 1. AWS ENI 라이프사이클

실제 Cilium 코드 `pkg/aws/eni/node.go`의 `CreateInterface()` 흐름을 재현한다:

- **ENI 생성**: `CreateNetworkInterface` API를 통해 새 ENI 생성
- **인스턴스 부착**: `AttachNetworkInterface` API로 EC2 인스턴스에 연결 (인덱스 충돌 재시도 포함)
- **보조 IP 할당**: `AssignPrivateIpAddresses` API로 Pod용 IP 할당
- **보안 그룹 결정**: 3단계 폴백 전략 (명시 지정 > 태그 탐색 > eth0 상속)
- **서브넷 선택**: 가용 IP가 가장 많은 서브넷 자동 선택
- **IP 해제**: `UnassignPrivateIpAddresses`로 불필요 IP 반환

### 2. AWS 프리픽스 위임

Nitro 인스턴스에서의 `/28` 프리픽스 위임을 시뮬레이션한다:

- 개별 IP 대신 16개 IP 블록 단위 할당
- API 호출 수 절감 효과 시연

### 3. Azure NIC IPAM

실제 Cilium 코드 `pkg/azure/ipam/node.go`의 동작을 재현한다:

- **NIC 동적 생성 불가**: `CreateInterface`가 미구현인 특성
- **기존 NIC에 IP 추가**: `AssignPrivateIpAddresses` 방식
- **VM vs VMSS 분기**: 일반 VM과 Virtual Machine Scale Set에 따른 API 경로 차이
- **3단계 리싱크 최적화**: 네트워크 인터페이스를 1회만 조회하고 메모리에서 재파싱
- **256 IP 제한**: NIC당 최대 256개 IP 주소

### 4. Alibaba Cloud ENI IPAM

실제 Cilium 코드 `pkg/alibabacloud/eni/node.go`의 동작을 재현한다:

- **ENI 생성**: Primary/Secondary 타입 구분
- **비동기 부착 + 폴링**: `WaitENIAttached`로 지수 백오프 대기
- **ENI 인덱스 태그**: 태그 기반 ENI 인덱스 관리
- **초기 IP 제한**: `maxENIIPCreate = 10`

### 5. 클라우드 메타데이터 서비스

각 클라우드의 Instance Metadata Service를 시뮬레이션한다:

| 프로바이더 | 엔드포인트 | 수집 정보 |
|-----------|-----------|----------|
| AWS IMDS | `169.254.169.254` | instance-id, type, AZ, VPC, subnet |
| Azure IMDS | `169.254.169.254` | subscriptionId, resourceGroup, cloudName |
| Alibaba | `100.100.100.200` | instance-id, type, region, zone, vpc-id |

### 6. Rate Limiting

토큰 버킷 알고리즘 기반의 API 속도 제한기를 구현한다:

- 초당 호출 횟수 제한
- 버스트 허용
- API 호출 메트릭 수집 (호출 수, 지연 시간, 에러 수)
- 스로틀링 횟수 추적

### 7. 프로바이더간 비교

세 프로바이더의 기능 차이를 표 형태로 비교하고, rate limiting 동작과 인스턴스 타입별 Pod IP 용량을 수치로 비교한다.

## 실제 코드 참조

| 시뮬레이션 컴포넌트 | 실제 Cilium 코드 |
|-------------------|-----------------|
| `AWSENIManager` | `pkg/aws/eni/instances.go` - `InstancesManager` |
| `AWSENI` | `pkg/aws/eni/types/types.go` - `ENI` |
| `CreateNetworkInterface` | `pkg/aws/ec2/ec2.go` - `Client.CreateNetworkInterface()` |
| `AttachNetworkInterface` | `pkg/aws/ec2/ec2.go` - `Client.AttachNetworkInterface()` |
| ENI 부착 재시도 | `pkg/aws/eni/node.go` - `Node.CreateInterface()` |
| 보안 그룹 폴백 | `pkg/aws/eni/node.go` - `Node.getSecurityGroupIDs()` |
| 서브넷 선택 | `pkg/aws/eni/node.go` - `Node.findSuitableSubnet()` |
| 프리픽스 위임 | `pkg/aws/eni/node.go` - `Node.IsPrefixDelegated()` |
| `AzureNICManager` | `pkg/azure/ipam/instances.go` - `InstancesManager` |
| Azure 3단계 리싱크 | `pkg/azure/ipam/instances.go` - `resyncInstances()` |
| Azure IP 할당 | `pkg/azure/ipam/node.go` - `Node.AllocateIPs()` |
| `AlibabaENIManager` | `pkg/alibabacloud/eni/instances.go` - `InstancesManager` |
| `WaitENIAttached` | `pkg/alibabacloud/api/api.go` - `Client.WaitENIAttached()` |
| ENI 인덱스 태그 | `pkg/alibabacloud/eni/node.go` - `Node.allocENIIndex()` |
| `MetadataService` | `pkg/aws/metadata/metadata.go`, `pkg/azure/api/metadata.go`, `pkg/alibabacloud/metadata/metadata.go` |
| `APILimiter` | `pkg/api/helpers/rate_limiter.go` |

## 아키텍처 흐름

```
main()
 |
 +-- AWS ENI 라이프사이클
 |    +-- 메타데이터 수집 (IMDS)
 |    +-- 보안 그룹 결정 (3단계 폴백)
 |    +-- 서브넷 선택 (가용 IP 기반)
 |    +-- ENI 생성 -> 부착 -> IP 할당 -> Pod 할당 -> IP 해제
 |
 +-- AWS 프리픽스 위임
 |    +-- /28 프리픽스 단위 할당
 |
 +-- Azure NIC (일반 VM)
 |    +-- 3단계 리싱크 최적화
 |    +-- 기존 NIC에 IP Configuration 추가
 |
 +-- Azure NIC (VMSS)
 |    +-- VMSS API를 통한 IP 추가
 |
 +-- Alibaba Cloud ENI
 |    +-- ENI 생성 -> 비동기 부착 -> 폴링 대기 -> IP 할당
 |
 +-- 프로바이더간 비교
      +-- 기능 비교표
      +-- Rate Limiting 벤치마크
      +-- 메타데이터 서비스 비교
      +-- 인스턴스 타입별 Pod IP 용량
```

## 학습 포인트

1. **프로바이더별 차이 이해**: AWS는 ENI 동적 생성, Azure는 기존 NIC 활용, Alibaba는 비동기 부착
2. **Rate Limiting 필요성**: 클라우드 API 스로틀링을 방지하기 위한 자체 속도 제한
3. **메타데이터 활용**: 인스턴스 정보를 IMDS에서 자동 수집하여 수동 설정 최소화
4. **보안 그룹 관리**: 3단계 폴백으로 유연한 보안 그룹 결정
5. **프리픽스 위임**: API 호출 최소화를 위한 대량 IP 할당 전략
6. **3단계 리싱크**: Azure의 API 호출 최적화를 위한 페치-파싱 분리 전략
