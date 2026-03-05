# PoC-14: IPAM (IP 주소 관리)

## 개요

Cilium의 IPAM(IP Address Management) 시스템을 시뮬레이션한다.
CIDR 기반 비트맵 할당, 다중 풀 관리, 사전 할당, 가비지 컬렉션을 재현한다.

## 시뮬레이션 대상

| 컴포넌트 | 실제 Cilium 경로 | PoC 구현 |
|----------|------------------|----------|
| CIDR Set | `pkg/ipam/cidrset/` | `CIDRPool` (비트맵) |
| Pool Manager | `pkg/ipam/` | `PoolManager` |
| Pre-allocation | `pkg/ipam/prealloc.go` | `PreAllocate()` |
| Garbage Collection | `pkg/ipam/gc.go` | `GarbageCollect()` |

## 핵심 개념

### 비트맵 기반 IP 할당
- **uint64 배열**로 IP 할당 상태를 추적 (1비트 = 1 IP)
- **First Fit 알고리즘**: `bits.TrailingZeros64(^bitmap)`으로 첫 빈 비트 탐색
- **/24 CIDR = 254 IP = 4개 uint64**로 효율적 관리

### 다중 풀 (Multi-Pool)
- 용도별 IP 풀 분리: default(Pod), external(외부), internal(내부 서비스)
- 실제 Cilium에서는 CiliumPodIPPool CRD로 풀을 정의

### Pre-Allocation
- Pod 생성 전에 미리 IP를 확보하여 할당 지연 최소화
- ENI 모드에서 인터페이스 생성이 느리므로 특히 중요

### GC 유예 기간 (Grace Period)
- IP 해제 후 즉시 재사용하지 않고 유예 기간을 둠
- conntrack 테이블의 오래된 항목이 새 Pod에 영향을 주는 것을 방지

## 실행 방법

```bash
go run main.go
```

## 출력 예시

- 단일 CIDR Pool: 할당, 해제, 풀 소진 테스트
- Multi-Pool: 용도별 풀 할당 및 통계
- Pre-Allocation: 사전 할당 IP 목록
- GC: 유예 기간 후 IP 회수
- 비트맵 시각화: 할당 패턴 시각적 확인

## 관련 문서

- [14-ipam.md](../14-ipam.md) - Cilium IPAM 심화 문서
