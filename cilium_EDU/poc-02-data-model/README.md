# PoC-02: Cilium 핵심 데이터 모델 시뮬레이션

## 개요

Cilium의 네 가지 핵심 데이터 구조인 **Endpoint**, **Identity**, **Node**, **IPCache**를
시뮬레이션한다. 이들은 Cilium의 네트워크 정책 적용 및 라우팅의 기반이 되는 데이터 모델이다.

## 시뮬레이션하는 개념

| 데이터 구조 | 실제 코드 | 역할 |
|------------|----------|------|
| Endpoint | `pkg/endpoint/endpoint.go` | Pod의 네트워크 인터페이스 표현 (ID, IP, Identity, State) |
| Identity | `pkg/identity/identity.go` | 레이블 기반 보안 식별자 (동일 레이블 = 동일 ID) |
| Node | `pkg/node/types/node.go` | 클러스터 노드 정보 (IP, PodCIDR, HealthIP) |
| IPCache | `pkg/ipcache/ipcache.go` | IP → Identity 매핑 (BPF 맵으로 동기화) |

## 핵심 데이터 흐름

```
Pod 생성 → CNI 호출 → Endpoint 생성 → Labels에서 Identity 할당
                                      → IPCache에 IP→Identity 기록
                                      → BPF 프로그램 재생성 (정책 적용)
```

## Identity 범위

| 범위 | 용도 |
|------|------|
| 0 | Unknown |
| 1-255 | Reserved (host, world, health, init 등) |
| 256-65535 | Cluster-local (레이블 기반) |
| 16777216+ | CIDR 기반 Identity |

## 실행 방법

```bash
cd cilium_EDU/poc-02-data-model
go run main.go
```

## 핵심 포인트

- **Identity 공유**: 같은 레이블 조합을 가진 Pod들은 동일한 NumericIdentity를 공유한다.
  이로 인해 정책을 개별 IP가 아닌 Identity 단위로 적용할 수 있다.
- **IPCache**: IP → Identity 매핑은 BPF 맵(`cilium_ipcache`)으로 동기화되어
  데이터패스에서 O(1) 조회가 가능하다.
- **Endpoint 상태 머신**: Creating → WaitingForIdentity → Regenerating → Ready
  각 단계마다 필요한 작업이 완료되어야 다음 단계로 전이한다.
- **CIDR 매칭**: IPCache는 정확한 /32 매칭 후 Longest Prefix Match로 fallback한다.
