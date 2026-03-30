# PoC-14: IPAM (IP 주소 관리)

## 개요

Cilium의 IPAM(IP Address Management) 시스템을 Go 표준 라이브러리만으로 시뮬레이션한다.
CIDR 기반 비트맵 IP 할당, 다중 풀(Multi-Pool) 관리, Pre-allocation 전략,
GC 유예 기간을 통한 conntrack 충돌 방지까지 IPAM의 핵심 메커니즘을 재현한다.

## 배경 지식

### IPAM이란?

IPAM(IP Address Management)은 네트워크에서 IP 주소의 할당, 추적, 회수를 관리하는 시스템이다.
Kubernetes 환경에서는 모든 Pod가 고유한 IP를 가져야 하므로, CNI 플러그인이 IPAM을 담당한다.
Cilium은 자체 IPAM 구현(`pkg/ipam/`)을 통해 다양한 클라우드 환경과 온프레미스를 지원한다.

### Cilium의 IPAM 모드

Cilium은 환경에 따라 8가지 IPAM 모드를 제공한다:

| 모드 | 설명 | 사용 환경 |
|------|------|----------|
| **Cluster Pool** | 클러스터 범위 CIDR에서 노드별 PodCIDR 자동 할당 | 범용 (기본값) |
| **Multi-Pool** | 여러 IP 풀을 용도별로 분리하여 관리 | 멀티테넌트, 정책 분리 |
| **AWS ENI** | AWS Elastic Network Interface 기반 할당 | AWS EKS |
| **Azure IPAM** | Azure 서브넷에서 IP 할당 | Azure AKS |
| **GKE** | Google Cloud 네트워크 연동 | GCP GKE |
| **CRD-backed** | CiliumNode CRD로 IP 범위 수동 관리 | 커스텀 환경 |
| **Kubernetes Host-scope** | K8s가 할당한 PodCIDR 사용 | 단순 환경 |
| **Delegated** | 다른 IPAM 플러그인에 위임 | 서드파티 연동 |

### Pod IP 할당 과정

1. kubelet이 Pod 생성 시 CNI 플러그인(Cilium)의 `ADD` 호출
2. Cilium Agent가 IPAM 모듈에 IP 할당 요청
3. IPAM이 해당 노드의 CIDR 풀에서 빈 IP를 비트맵 탐색으로 선택
4. 할당된 IP를 CiliumEndpoint CRD에 기록
5. Pod 삭제 시 `DEL` 호출 -> IP 해제 -> GC 유예 후 재사용 가능

### Pre-allocation 전략의 장점

- **지연 최소화**: Pod 생성 시점에 클라우드 API 호출 없이 즉시 IP 할당
- **API Rate Limit 회피**: AWS ENI 생성 등 느린 API를 미리 호출해 둠
- **burst 대응**: 갑작스러운 Pod 대량 생성에도 IP 부족 없이 대응
- **워터마크 기반**: 사전 할당 IP가 임계값 이하로 떨어지면 자동 보충

## 시뮬레이션하는 개념

| 개념 | 실제 Cilium 경로 | PoC 구현 | 설명 |
|------|------------------|----------|------|
| CIDR Set | `pkg/ipam/cidrset/cidr_set.go` | `CIDRPool` 구조체 | uint64 비트맵으로 IP 할당 상태 추적 |
| Pool Manager | `pkg/ipam/` | `PoolManager` 구조체 | 용도별 다중 풀 관리 |
| Pre-allocation | `pkg/ipam/prealloc.go` | `PreAllocate()` 메서드 | Pod 생성 전 IP 사전 확보 |
| IP 할당 | `pkg/ipam/cidrset/` | `Allocate()` 메서드 | First Fit 알고리즘으로 빈 IP 탐색 |
| IP 해제 + GC | `pkg/ipam/gc.go` | `Release()` + `GarbageCollect()` | 유예 기간 후 비트맵 비트 해제 |
| 할당 통계 | `pkg/ipam/metrics.go` | `Stats()` 메서드 | used/releasing/available/capacity 추적 |

## 아키텍처 다이어그램

```
                       PoolManager
                           │
            ┌──────────────┼──────────────┐
            ▼              ▼              ▼
       [default]      [external]    [internal-svc]
      10.10.0.0/24   10.20.0.0/24   10.30.0.0/24
            │              │              │
            ▼              ▼              ▼
        CIDRPool       CIDRPool       CIDRPool
       ┌────────┐    ┌────────┐     ┌────────┐
       │ bitmap │    │ bitmap │     │ bitmap │
       │[uint64]│    │[uint64]│     │[uint64]│
       └───┬────┘    └───┬────┘     └───┬────┘
           │             │              │
           ▼             ▼              ▼
  Allocate()  → ^word의 TrailingZeros → 첫 0비트 = 빈 IP → 비트 1 설정
  Release()   → allocated 맵에서 제거 → released 맵으로 이동 (시각 기록)
  GC()        → 유예기간 경과 확인 → 비트 0 해제 → IP 재사용 가능
```

### 비트맵 할당 상태 예시 (10.0.1.0/28, 14 IPs)

```
  IP:     .1  .2  .3  .4  .5  .6  .7  .8  .9 .10 .11 .12 .13 .14
  비트맵: [1] [1] [0] [1] [1] [0] [0] [0] [0] [0] [0] [0] [0] [0]
               ▲       ▲
          할당됨    해제 후 GC 완료 → 재사용 가능
```

## 코드 해설

### 1. `CIDRPool` -- 비트맵 기반 IP 풀

CIDR 범위를 받아 uint64 배열(비트맵)로 각 IP의 할당 상태를 추적한다.
`/24` CIDR은 254개 IP = 4개 uint64로 관리한다. `allocated` 맵은 할당 시각을,
`released` 맵은 해제 시각을 기록하여 GC 판단에 사용한다.

### 2. `Allocate()` -- First Fit 할당

비트맵 워드를 순회하며 `bits.TrailingZeros64(^word)`로 첫 번째 0 비트를 O(1)에 찾는다.
NOT 연산 후 trailing zeros가 가장 낮은 빈 비트 위치가 된다. 해당 비트를 1로 설정하고
오프셋을 IP 주소로 변환하여 반환한다.

### 3. `Release()` + `GarbageCollect()` -- 지연 해제

`Release()`는 즉시 비트맵을 해제하지 않고 IP를 `released` 맵으로 옮긴다.
`GarbageCollect()`가 유예 기간 경과 후 비트를 실제 해제한다.
conntrack 테이블의 오래된 항목이 새 Pod에 간섭하는 것을 방지하는 안전장치이다.

### 4. `PoolManager` -- 다중 풀 관리

`PoolType`(default, external, internal-svc)별로 `CIDRPool`을 관리한다.
실제 Cilium에서는 `CiliumPodIPPool` CRD로 풀을 정의하고 Pod 어노테이션으로 풀을 지정한다.

### 5. `PreAllocate()` -- 사전 할당

각 풀에서 설정된 수만큼 IP를 미리 할당한다. AWS ENI 모드에서 인터페이스 생성에
수초가 걸리므로, 미리 확보해두면 Pod 스케줄링 시 API 지연 없이 즉시 배정 가능하다.

## 실행 방법

```bash
cd cilium_EDU/poc-14-ipam
go run main.go
```

### 예상 출력 (발췌)

```
=== Cilium IPAM PoC ===
[1] 단일 CIDR Pool 테스트 (10.0.1.0/28 = 14 IPs)
  생성: CIDR=10.0.1.0/28 used=0 releasing=0 available=14 capacity=14
  할당: 10.0.1.1
  할당: 10.0.1.2
  ...
  GC 수행: 2개 IP 회수
  상태: CIDR=10.0.1.0/28 used=3 releasing=0 available=11 capacity=14

[3] Pre-Allocation (사전 할당)
  [default] 사전 할당: 10.10.0.1, 10.10.0.2, 10.10.0.3
  [external] 사전 할당: 10.20.0.1, 10.20.0.2, 10.20.0.3

[5] Garbage Collection
  [default] GC 회수: 5개 IP

[6] 비트맵 할당 상태 시각화 (10.0.1.0/28)
  비트맵: [11011000000000]
  (1=할당, 0=미할당, 3번째 IP 해제 확인)
```

실제로는 6개 섹션(단일 풀, Multi-Pool, Pre-allocation, 동적 할당/해제, GC, 비트맵 시각화)이 순서대로 출력된다.

## 핵심 포인트

1. **비트맵은 메모리 효율의 핵심이다**: /16 CIDR(65,534 IPs)도 1KB 미만의 비트맵으로 관리할 수 있다. 각 IP당 1비트만 사용하므로 대규모 클러스터에서도 메모리 부담이 없다.

2. **First Fit + TrailingZeros는 O(n/64) 탐색이다**: 64개 IP를 한 워드에서 한 번의 CPU 명령어로 검사하므로, 순차 탐색 대비 64배 빠르다.

3. **GC 유예 기간은 보안과 안정성을 위한 것이다**: 해제된 IP를 즉시 재사용하면 이전 Pod의 conntrack 항목이 새 Pod 트래픽에 간섭할 수 있다. 유예 기간은 이 위험을 제거한다.

4. **Pre-allocation은 SLA의 핵심이다**: 클라우드 API 호출(ENI 생성 등)은 수초가 걸릴 수 있으므로, Pod 스케줄링 경로에서 이를 제거하는 것이 응답 시간 보장의 핵심이다.

5. **다중 풀은 네트워크 정책 분리를 가능하게 한다**: 외부 통신용, 내부 서비스용, 기본 Pod용 IP 대역을 분리하여 방화벽 규칙과 라우팅 정책을 단순화한다.

## 실제 Cilium과의 차이점

| 항목 | 이 PoC | 실제 Cilium |
|------|--------|-------------|
| CIDR 할당 | 정적 CIDR 직접 지정 | CiliumNode CRD 또는 Operator가 노드별 PodCIDR 자동 분배 |
| 비트맵 구현 | `[]uint64` 단순 배열 | `pkg/ipam/cidrset/`에서 동일한 비트맵 사용 + IPv6 지원 |
| 다중 풀 정의 | 코드 내 하드코딩 | `CiliumPodIPPool` CRD + Pod 어노테이션으로 풀 선택 |
| Pre-allocation | 단순 카운트 기반 | 워터마크(min/max-allocate) 기반 자동 보충 + excess IP 정리 |
| GC | 단일 호출 | 주기적 타이머(`RunGC` 루프) + 메트릭 연동 |
| ENI/Azure 연동 | 미구현 | AWS/Azure API 호출로 네트워크 인터페이스 생성/삭제 |
| IPv6 | 미지원 | IPv4/IPv6 듀얼스택 완전 지원 |
| 메트릭 | `Stats()` 로컬 출력 | Prometheus 메트릭 노출 (`cilium_ipam_*`) |
| 동시성 | `sync.Mutex` | `sync.Mutex` + 채널 기반 이벤트 알림 |

## 관련 문서

- [14-ipam.md](../14-ipam.md) - Cilium IPAM 심화 문서
