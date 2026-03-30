# PoC-10: 정책 엔진 시뮬레이션 (SelectorCache + PolicyMap)

## 개요

Cilium 정책 엔진의 핵심 흐름을 Go 표준 라이브러리만으로 시뮬레이션한다.
CiliumNetworkPolicy가 적용되면 라벨 기반 셀렉터가 Identity 집합으로 변환되고,
최종적으로 BPF PolicyMap(LPM Trie)에 삽입되어 패킷 단위 O(log n) 정책 판정이 이루어지는 전체 과정을 재현한다.

---

## 배경 지식

### Identity 기반 정책이 IP 기반보다 뛰어난 이유

전통적인 네트워크 정책은 IP 주소를 기반으로 방화벽 규칙을 작성한다.
Kubernetes 환경에서 Pod IP는 수시로 변경되므로 IP 기반 정책은 다음 한계를 가진다:

- Pod 재시작마다 IP가 바뀌어 규칙 갱신 필요
- IP 범위(CIDR)로 표현하면 의도하지 않은 Pod까지 허용/차단
- 규칙 수가 Pod 수에 비례하여 폭발적으로 증가

Cilium은 **Identity 모델**을 사용한다.
동일한 라벨 집합을 가진 모든 Pod에 하나의 NumericIdentity(숫자 ID)를 부여하고,
정책 규칙은 이 Identity를 기준으로 작성된다.
Pod가 100개여도 라벨이 같으면 Identity는 하나이므로 PolicyMap 엔트리 수가 최소화된다.

### SelectorCache의 역할

`pkg/policy/selectorcache.go`에 구현된 SelectorCache는 다음 세 가지를 캐싱한다:

1. **idCache**: 모든 알려진 Identity (NumericIdentity -> 라벨 배열)
2. **selectors**: 사용 중인 라벨 셀렉터 (셀렉터 키 -> identitySelector)
3. **cachedSelections**: 각 셀렉터가 현재 선택하는 Identity 집합

새 Pod가 배포되면 `UpdateIdentities(added, deleted)`가 호출되고,
모든 등록된 셀렉터의 선택 집합이 자동으로 재계산된다.
네임스페이스별 인덱싱(`byNamespace`)으로 불필요한 매칭을 건너뛰어 성능을 확보한다.

### PolicyMap이 BPF LPM Trie를 사용하는 이유

PolicyMap의 키는 `(TrafficDirection, Identity, Protocol, Port)` 4-tuple이다.
패킷이 도착하면 이 4-tuple 중 가장 구체적인 매치를 찾아야 하는데,
이것은 IP 라우팅의 Longest Prefix Match와 동일한 문제다.

Cilium은 `bitlpm.Trie`(비트 단위 LPM Trie)를 사용하여 O(log n) 시간에 가장 구체적인 규칙을 조회하고, specific deny가 wildcard allow를 자연스럽게 오버라이드하며, BPF 맵(`BPF_MAP_TYPE_LPM_TRIE`)으로 직접 변환할 수 있다.

---

## 시뮬레이션하는 개념

| 컴포넌트 | 실제 소스 경로 | 시뮬레이션 범위 |
|----------|---------------|----------------|
| Identity | `pkg/identity/numeric_identity.go` | 라벨 집합 -> NumericIdentity 매핑, 예약 ID |
| EndpointSelector | `pkg/policy/api/selector.go` | matchLabels 기반 라벨 매칭, 와일드카드 |
| SelectorCache | `pkg/policy/selectorcache.go` | Identity 캐시, 셀렉터 매칭, 동적 업데이트 전파 |
| Repository | `pkg/policy/repository.go` | 규칙 저장, resolvePolicyLocked() 흐름 |
| PolicyMap (LPM) | `pkg/policy/mapstate.go` | specificity 기반 LPM 매칭, Deny 우선순위 |
| L4Policy | `pkg/policy/l4.go` | L3/L4 필터 매칭, 포트/프로토콜 규칙 |

---

## 아키텍처 / 흐름 다이어그램

```
  CiliumNetworkPolicy (YAML)
         │
         ▼
  ┌──────────────────┐
  │   Repository      │  AddRule() → 규칙 저장
  └────────┬─────────┘
           │ ResolvePolicy(identity)
           ▼
  ┌──────────────────────────────────────────┐
  │       resolvePolicyLocked()              │
  │  1. Subject 매칭 → 적용 규칙 필터링       │
  │  2. Peer 셀렉터 → SelectorCache 조회      │
  │     ┌────────────────────┐               │
  │     │  SelectorCache     │               │
  │     │  idCache: NID→Label│               │
  │     │  selectors: Key→{} │               │
  │     └────────────────────┘               │
  │  3. PolicyMap 엔트리 생성                 │
  └──────────────┬───────────────────────────┘
                 ▼
  ┌──────────────────────────────────────────┐
  │         PolicyMap (LPM Trie)             │
  │  Lookup 우선순위 (높음 → 낮음):           │
  │   id+proto+port (7) > id+proto (6)       │
  │   > id (4) > proto+port (3) > proto (2)  │
  │   > wildcard (0)                         │
  └──────────────────────────────────────────┘
```

---

## 코드 해설

### 1. SelectorCache -- 양방향 매핑 캐시

`idCache`(NID->라벨)와 `selectors`(키->Identity 집합)를 관리한다.
`UpdateIdentities(added, deleted)` 호출 시 모든 셀렉터를 순회하며 `cachedSelections`를 갱신하고,
변경된 셀렉터 목록을 반환하여 정책 재계산이 필요한 엔드포인트를 식별한다.

### 2. PolicyMap.Lookup() -- LPM 매칭

6개의 후보 키를 specificity 내림차순으로 시도한다: `(id+proto+port)` -> `(id+proto)` -> `(id)` -> `(proto+port)` -> `(proto)` -> `(wildcard)`. 실제 Cilium은 `bitlpm.Trie`로 O(log n)에 수행한다.

### 3. Repository.ResolvePolicy()

주어진 Identity에 적용되는 모든 규칙을 찾아 PolicyMap을 생성한다.
(1) Subject 매칭 필터링 -> (2) default deny 삽입 -> (3) Peer 셀렉터의 SelectorCache 조회 -> (4) PolicyMap 엔트리 생성.

### 4. PolicyMapKey.specificity() -- 우선순위 비트 마스크

`Identity=4, Protocol=2, Port=1`의 가중치로 specificity를 계산한다.
Identity 지정 규칙(최소 4)이 와일드카드 규칙(최대 3)보다 항상 우선하므로,
specific deny가 wildcard allow를 자연스럽게 오버라이드한다.

---

## 실행 방법 및 예상 출력

```bash
cd cilium_EDU/poc-10-policy-engine
go run main.go
```

```
━━━ 데모 1: Identity 기반 정책 모델 ━━━
  등록된 Identity:
    ID=1000: k8s:app=web,k8s:namespace=default
    ID=2000: k8s:app=api,k8s:namespace=default  ...

━━━ 데모 2: SelectorCache 동작 ━━━
  셀렉터 → 선택된 Identity:
    app=web → [1000 1001],  app=api → [2000],  (wildcard) → [1000 1001 2000 3000 4000]
  [이벤트] 새 Identity 추가: ID=1002 → app=web 선택: [1000 1002]

━━━ 데모 4: PolicyMap LPM 매칭 ━━━
    api(2000) → web:80/TCP    → ALLOW
    cache(4000) → web:80/TCP  → DENY
    db(3000) → web:80/TCP     → DEFAULT DENY

━━━ 데모 5: 동적 Identity 업데이트와 정책 재계산 ━━━
    api-v2(2001) → web:80/TCP → ALLOW  (자동 전파)

━━━ 데모 6: Deny 우선순위와 충돌 해결 ━━━
    api(2000) → db:5432/TCP   → ALLOW (wildcard allow)
    cache(4000) → db:5432/TCP → DENY  (specific deny 우선)
```

---

## 핵심 포인트

1. **Identity 모델의 효율성**: 동일 라벨의 Pod 100개가 있어도 PolicyMap 엔트리는 1개. IP 기반 대비 규칙 수가 O(라벨 조합) 수준으로 축소된다.

2. **SelectorCache의 양방향 매핑**: 셀렉터 추가 시 기존 Identity를 스캔하고, Identity 추가 시 기존 셀렉터를 스캔한다. 어느 쪽이 먼저 등록되든 매칭이 보장된다.

3. **LPM 매칭의 자연스러운 우선순위**: specificity 비트 마스크(`Identity=4, Protocol=2, Port=1`)로 구체적인 규칙이 항상 우선한다. Deny가 specific하고 Allow가 wildcard면 Deny가 이긴다.

4. **동적 업데이트 전파**: 새 Pod 배포 -> Identity 할당 -> `UpdateIdentities()` -> 영향받는 셀렉터 식별 -> PolicyMap 재계산. 이 체인이 자동으로 동작한다.

5. **Default Deny 의미론**: 정책이 적용되는 엔드포인트는 명시적 허용 없는 트래픽을 모두 차단한다. `(Direction, 0, 0, 0) -> DENY` 엔트리로 구현된다.

---

## 실제 Cilium과의 차이점

| 항목 | 이 시뮬레이션 | 실제 Cilium |
|------|-------------|------------|
| LPM Trie | 후보 키 순차 탐색 (O(6)) | `bitlpm.Trie` + BPF `LPM_TRIE` 맵 (O(log n)) |
| Identity 할당 | 수동 지정 | kvstore(etcd) 기반 클러스터 전역 할당 |
| 네임스페이스 최적화 | 미구현 | `byNamespace` 인덱스로 매칭 범위 축소 |
| L7 정책 | 미구현 | Envoy 프록시로 HTTP/gRPC/Kafka 레벨 정책 |
| CIDR 정책 | 미구현 | `CIDRRule`로 외부 IP 대역 지정 |
| BPF 맵 동기화 | 메모리 맵만 사용 | `bpf_map_update_elem()`으로 커널 맵 직접 갱신 |
| 정책 distillery | 미구현 | `distillery.go`에서 엔드포인트별 정책 캐싱 |
| IncrementalPolicy | 미구현 | 변경분만 계산하여 BPF 맵 부분 업데이트 |
| 동시성 모델 | 단순 RWMutex | 엔드포인트별 독립 업데이트 + 배치 처리 |
