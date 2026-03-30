# PoC-02: Cilium 핵심 데이터 모델 시뮬레이션

## 개요

Cilium의 네 가지 핵심 데이터 구조인 **Endpoint**, **Identity**, **Node**, **IPCache**를
순수 Go 표준 라이브러리만으로 시뮬레이션한다. 이 데이터 모델들은 Cilium이 eBPF 기반
네트워크 정책을 적용하고, 패킷을 라우팅하며, 클러스터 전체의 보안을 관리하는 핵심 기반이다.

Pod 생성부터 Identity 할당, IPCache 등록, 엔드포인트 삭제까지의 전체 라이프사이클을
단계별로 재현하며, 동일 레이블을 가진 Pod가 Identity를 공유하는 메커니즘을 확인할 수 있다.

---

## 배경 지식

### 전통적 네트워크 보안의 한계

전통적인 방화벽과 보안 그룹은 **IP 주소** 기반으로 정책을 적용한다.
그러나 Kubernetes 환경에서 Pod IP는 수시로 변경되고, 스케일 아웃/인 시마다 새로운 IP가
할당된다. IP 기반 보안 정책은 이 동적 환경에서 다음과 같은 문제를 일으킨다:

- Pod가 재시작될 때마다 방화벽 규칙을 갱신해야 한다
- IP 범위 기반 정책은 지나치게 넓거나 좁은 접근 권한을 부여한다
- 수천 개의 Pod에 대해 개별 IP 규칙을 관리하는 것은 비현실적이다

### Cilium의 Identity 기반 보안 모델

Cilium은 이 문제를 **Identity(보안 식별자)** 개념으로 해결한다. IP 대신 **레이블 조합**으로
워크로드를 식별하며, 동일한 레이블을 가진 모든 Pod는 하나의 NumericIdentity를 공유한다.

이 접근법의 혁신성:
- **IP 독립적**: Pod IP가 변경되어도 레이블이 같으면 같은 Identity를 유지한다
- **스케일 효율**: 1,000개의 Pod도 레이블이 같으면 하나의 Identity로 관리된다
- **BPF 최적화**: Identity는 정수값이므로 eBPF 맵에서 O(1) 조회가 가능하다
- **클러스터 전역**: Identity는 노드 로컬이 아닌 클러스터 전역으로 일관성을 보장한다

### 네 가지 핵심 데이터 모델의 역할

| 데이터 모델 | 역할 | 왜 중요한가 |
|------------|------|------------|
| **Identity** | 레이블 기반 보안 식별자 | 네트워크 정책의 기본 단위. IP가 아닌 Identity로 허용/차단 결정 |
| **Endpoint** | Pod의 네트워크 인터페이스 | BPF 프로그램이 부착되는 지점. Identity와 1:N 관계 |
| **IPCache** | IP -> Identity 매핑 테이블 | 데이터패스에서 패킷의 출발지/목적지 Identity를 실시간 조회 |
| **Node** | 클러스터 노드 정보 | Pod CIDR 할당, 노드 간 터널링, 헬스 체크의 기반 |

---

## 시뮬레이션하는 개념

| 개념 | 실제 코드 위치 | PoC에서의 구현 |
|------|--------------|---------------|
| NumericIdentity | `pkg/identity/numeric_identity.go` | 예약 범위(0-255)와 클러스터 로컬(256+) 할당 |
| Identity 할당기 | `pkg/identity/cache/allocator.go` | 레이블 해시 기반 중복 검출 및 재사용 로직 |
| Endpoint 상태 머신 | `pkg/endpoint/endpoint.go` | Creating -> WaitingForIdentity -> Regenerating -> Ready 전이 |
| EndpointManager | `pkg/endpoint/manager.go` | 엔드포인트 CRUD 및 Identity/IPCache 연동 |
| IPCache LPM 조회 | `pkg/ipcache/ipcache.go` | /32 정확 매칭 + Longest Prefix Match fallback |
| Node 모델 | `pkg/node/types/node.go` | 노드 IP, PodCIDR, HealthIP 등 메타데이터 |

### Identity 범위

| 범위 | 용도 | 예시 |
|------|------|------|
| 0 | Unknown | 미식별 트래픽 |
| 1-7 | Reserved | host(1), world(2), health(4), kube-apiserver(7) |
| 256-65535 | Cluster-local | 레이블 기반 할당 (`app=frontend` 등) |
| 16777216+ | CIDR 기반 | 외부 CIDR에 대한 Identity |

---

## 아키텍처 / 흐름 다이어그램

### Pod 생성 시 데이터 모델 연동 흐름

```
  kubelet                Cilium Agent              데이터 모델
  ───────               ────────────              ──────────────
     │                       │                         │
     │  CNI ADD 호출         │                         │
     │──────────────────────>│                         │
     │                       │                         │
     │                       │  1) Endpoint 생성       │
     │                       │────────────────────────>│ Endpoint{State: Creating}
     │                       │                         │
     │                       │  2) Labels 추출         │
     │                       │────────────────────────>│ Endpoint{State: WaitingForIdentity}
     │                       │                         │
     │                       │  3) Identity 할당       │
     │                       │────────────────────────>│ IdentityAllocator
     │                       │                         │  ├─ labelsKey() 정규화
     │                       │                         │  ├─ 기존 ID 조회 (재사용)
     │                       │                         │  └─ 없으면 새 ID 할당
     │                       │                         │
     │                       │  4) IPCache 등록        │
     │                       │────────────────────────>│ IPCache.Upsert()
     │                       │                         │  IP/32 → Identity 매핑
     │                       │                         │
     │                       │  5) BPF 재생성          │
     │                       │────────────────────────>│ Endpoint{State: Regenerating}
     │                       │                         │
     │                       │  6) 완료                │
     │                       │────────────────────────>│ Endpoint{State: Ready}
     │  CNI 응답             │                         │
     │<──────────────────────│                         │
```

### Identity 공유 구조

```
  +-------------------+     +-------------------+
  | Pod: frontend-abc |     | Pod: frontend-def |
  | IP: 10.244.0.10   |     | IP: 10.244.0.11   |
  | Labels:           |     | Labels:           |
  |   app=frontend    |     |   app=frontend    |
  +--------+----------+     +--------+----------+
           |                         |
           +----------+--------------+
                      |
                      v
              +-------+--------+
              | Identity: 256  |
              | Labels:        |
              |  app=frontend  |
              +-------+--------+
                      |
            +---------+---------+
            |                   |
            v                   v
  +---------+------+  +---------+------+
  | IPCache Entry  |  | IPCache Entry  |
  | 10.244.0.10/32 |  | 10.244.0.11/32 |
  | → Identity 256 |  | → Identity 256 |
  +----------------+  +----------------+
```

---

## 코드 해설

### 1. IdentityAllocator - 레이블 기반 Identity 할당

`IdentityAllocator`는 레이블 조합을 정규화하여 키를 만들고, 동일한 키가 이미 존재하면
기존 Identity를 재사용한다. 이것이 "같은 레이블 = 같은 Identity" 원칙의 구현부이다.

```go
type IdentityAllocator struct {
    nextID     NumericIdentity
    identities map[NumericIdentity]*Identity
    labelIndex map[string]NumericIdentity  // 레이블 해시 → ID
}
```

`labelsKey()` 함수가 레이블을 정렬 후 `key=value;key=value` 형태로 정규화하여,
레이블 순서에 관계없이 동일한 조합은 항상 같은 키를 생성한다.

### 2. Endpoint 상태 머신

Endpoint는 생성부터 준비 완료까지 명확한 상태 전이를 거친다. 각 단계에서 필요한 작업이
완료되어야만 다음 상태로 전이할 수 있다.

```
Creating → WaitingForIdentity → Regenerating → Ready
                                                  ↓ (삭제 시)
                                    Disconnecting → Disconnected
```

실제 Cilium에서 `Regenerating` 단계는 eBPF 프로그램을 컴파일하고 커널에 로드하는
가장 무거운 작업이다. PoC에서는 상태 전이만 시뮬레이션한다.

### 3. IPCache의 Longest Prefix Match

`IPCache.Lookup()`은 먼저 `/32` 정확 매칭을 시도하고, 실패하면 등록된 모든 CIDR 중
가장 긴 프리픽스를 가진 엔트리를 반환한다. 이는 실제 BPF 맵의 LPM trie 자료구조를
순수 Go로 재현한 것이다.

```go
func (c *IPCache) Lookup(ip string) (*IPCacheEntry, bool) {
    // 1단계: /32 정확 매칭
    // 2단계: 모든 CIDR에 대해 Contains 검사 후 최장 프리픽스 선택
}
```

예를 들어, `0.0.0.0/0`(world)과 `10.244.0.10/32`(특정 Pod)가 모두 등록된 상태에서
`10.244.0.10`을 조회하면 /32가 우선한다. Pod 삭제 후에는 /0으로 fallback된다.

### 4. EndpointManager의 생성/삭제 흐름

`CreateEndpoint()`는 Endpoint 생성, Identity 할당, IPCache 등록을 하나의 트랜잭션처럼
처리한다. `DeleteEndpoint()`는 역순으로 IPCache에서 제거 후 Endpoint를 삭제한다.

### 5. Identity 구조체와 NumericIdentity

`NumericIdentity`는 `uint32` 타입으로, 예약 범위(0-255)의 상수가 미리 정의되어 있다.
`Identity` 구조체는 이 숫자 ID에 레이블, 네임스페이스, 소스 정보를 함께 담는다.

---

## 실행 방법 및 예상 출력

```bash
cd cilium_EDU/poc-02-data-model
go run main.go
```

### 예상 출력 (주요 부분)

```
╔══════════════════════════════════════════════════════╗
║  Cilium 핵심 데이터 모델 시뮬레이션                 ║
║  Endpoint, Identity, Node, IPCache                  ║
╚══════════════════════════════════════════════════════╝

=== 1. 노드 정보 ===
  Node{Name:worker-1, IPv4:10.0.0.1, PodCIDRs:[10.244.0.0/24]}

=== 2. 예약된 Identity ===
  ID: 1   → reserved:host
  ID: 2   → reserved:world
  ID: 4   → reserved:health
  ID: 5   → reserved:init

=== 3. 엔드포인트 생성 (Pod 배포 시뮬레이션) ===
  [endpoint-manager] 엔드포인트 생성 시작: default/frontend-abc (ID: 1001)
  [allocator] 새 Identity 할당: 256 (레이블: k8s:app=frontend;k8s:io.kubernetes.pod.namespace=default)
  ...
  [allocator] 기존 Identity 재사용: 256 (레이블: k8s:app=frontend;...)
  [allocator] 새 Identity 할당: 257 (레이블: k8s:app=backend;...)

=== 4. Identity 공유 확인 ===
  frontend-abc Identity: 256
  frontend-def Identity: 256    ← 동일 레이블이므로 같은 Identity
  backend-xyz  Identity: 257
  frontend 공유 여부: true

=== 7. IPCache Lookup 테스트 ===
  10.244.0.10 → Identity identity:256 (source: k8s)
  10.244.0.20 → Identity identity:257 (source: k8s)
  8.8.8.8     → Identity reserved:world (source: reserved)  ← 0.0.0.0/0 fallback
  10.0.0.1    → Identity reserved:host (source: reserved)
```

---

## 핵심 포인트

1. **Identity 공유**: 같은 레이블 조합(`app=frontend`)을 가진 Pod들은 동일한
   NumericIdentity(256)를 공유한다. 1,000개의 frontend Pod가 있어도 Identity는 하나이며,
   네트워크 정책도 하나만 정의하면 된다.

2. **IPCache와 BPF 동기화**: IPCache의 IP -> Identity 매핑은 실제로는 BPF 맵
   (`cilium_ipcache`)으로 커널에 동기화된다. 데이터패스에서 패킷을 처리할 때
   eBPF 프로그램이 이 맵을 O(1)로 조회하여 출발지/목적지의 Identity를 결정한다.

3. **Endpoint 상태 머신**: 각 상태 전이는 특정 조건이 충족되어야 진행된다.
   Identity 할당 없이는 BPF 재생성으로 넘어갈 수 없고, BPF 재생성 없이는
   Ready 상태가 될 수 없다. 이 순서가 보안 정책의 일관성을 보장한다.

4. **CIDR 기반 Longest Prefix Match**: IPCache는 정확한 /32 매칭을 우선하고,
   없으면 가장 긴 프리픽스로 fallback한다. 외부 IP(8.8.8.8)는 `0.0.0.0/0`과
   매칭되어 `reserved:world` Identity를 받는다.

5. **레이블 키 정규화**: Identity 할당 시 레이블 순서에 무관하게 동일한 키를 생성한다.
   `{a=1, b=2}`와 `{b=2, a=1}`은 같은 Identity를 받는다.

---

## 실제 Cilium과의 차이점

| 항목 | 실제 Cilium | 이 PoC |
|------|-----------|--------|
| Identity 할당 | kvstore(etcd) 기반 분산 할당 + CRD 캐시 | 로컬 인메모리 카운터 |
| IPCache 저장소 | BPF LPM trie 맵 (`cilium_ipcache`) | Go map + 순차 탐색 |
| Endpoint BPF 재생성 | clang으로 eBPF C 코드 컴파일 후 커널 로드 | 상태 전이만 시뮬레이션 |
| 동시성 | 이벤트 기반 비동기 처리 + 세밀한 잠금 | sync.Mutex 기본 잠금 |
| Identity GC | 참조 카운트 기반, 사용하지 않는 Identity 해제 | 미구현 (할당만) |
| Node 관리 | K8s watcher + kvstore 동기화 | 정적 구조체 생성만 |
| 멀티 클러스터 | ClusterMesh를 통한 크로스 클러스터 Identity | 단일 클러스터만 |
| CIDR Identity | 16777216+ 범위로 외부 CIDR 전용 Identity 할당 | 미구현 |
