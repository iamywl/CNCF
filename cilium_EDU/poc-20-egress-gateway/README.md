# PoC-20: Egress Gateway 시뮬레이션

## 개요

이 PoC는 Cilium Egress Gateway의 핵심 동작 원리를 Go 표준 라이브러리만으로 시뮬레이션한다.
Kubernetes 클러스터에서 외부로 나가는 트래픽의 소스 IP를 고정된 게이트웨이 노드의 IP로 변환(SNAT)하는
전체 흐름 -- 정책 파싱, 엔드포인트 매칭, 게이트웨이 선택, eBPF 맵 관리 -- 을 재현한다.

## 배경 지식

### Egress Gateway란?

Kubernetes Pod에서 클러스터 외부로 나가는 트래픽(egress)은 기본적으로 해당 Pod이 실행되는 노드의 IP로 SNAT된다. Pod이 스케줄링되는 노드는 유동적이므로, 외부에서 보이는 소스 IP도 계속 변한다.

이것이 문제가 되는 상황:

- **외부 방화벽 규칙**: 기업 환경에서 외부 API나 데이터베이스에 접근할 때 소스 IP 기반 화이트리스트를 사용하는 경우가 많다. Pod IP가 바뀔 때마다 방화벽 규칙을 갱신할 수 없다.
- **감사/컴플라이언스**: 특정 서비스의 외부 통신이 어떤 IP에서 나가는지 추적 가능해야 한다.
- **SaaS 연동**: 외부 SaaS가 IP 기반 인증을 요구하는 경우 고정 IP가 필수다.

Egress Gateway는 이 문제를 해결한다. 특정 레이블을 가진 Pod의 외부 트래픽을 지정된 게이트웨이 노드로 라우팅하고, 그 노드의 고정 IP로 SNAT하여 내보낸다. Cilium은 이를 `CiliumEgressGatewayPolicy` CRD와 eBPF 기반 데이터플레인으로 구현한다.

### 핵심 구성 요소

| 구성 요소 | 역할 |
|-----------|------|
| `CiliumEgressGatewayPolicy` | 어떤 Pod(endpointSelector)의 어떤 목적지(destinationCIDR) 트래픽을 어떤 노드(egressGateway)로 보낼지 정의 |
| `EgressGatewayManager` | 정책/노드/엔드포인트 변경을 감시하고 eBPF 맵을 리컨실레이션 |
| eBPF 정책 맵 | 커널 수준에서 패킷별로 SNAT 결정을 수행하는 데이터플레인 |

## 시뮬레이션하는 개념

| 개념 | 실제 Cilium 코드 | 시뮬레이션 방식 |
|------|------------------|----------------|
| CiliumEgressGatewayPolicy | `pkg/egressgateway/policy.go` - `PolicyConfig` | `PolicyConfig` 구조체로 정책 정의 (셀렉터, CIDR, 게이트웨이 설정) |
| 엔드포인트 매칭 | `PolicyConfig.matchesEndpointLabels()` | 레이블 기반 매칭 함수 `matchesLabels()` |
| 게이트웨이 설정 결정 | `PolicyConfig.regenerateGatewayConfig()` | 노드 셀렉터 매칭 후 `GatewayConfig` 생성 |
| 다중 게이트웨이 분배 | `computeEndpointHash()` + FNV 해시 | FNV-1a 해시로 엔드포인트별 게이트웨이 할당 |
| eBPF 맵 업데이트 | `Manager.updateEgressRules4()` | mark-and-sweep 패턴으로 `policyMap` 관리 |
| 리컨실레이션 루프 | `Manager.reconcileLocked()` | `Reconcile()` 메서드로 전체 재계산 |
| 제외 CIDR | `ExcludedCIDRIPv4 = 0.0.0.1` | 동일한 특수 IP 값으로 제외 표시 |

## 아키텍처 / 흐름 다이어그램

```
CiliumEgressGatewayPolicy (CRD)
         │
         ▼
┌─────────────────────────────────────────────────────────────┐
│                  EgressGatewayManager                       │
│                                                             │
│  ┌──────────┐   ┌──────────────┐   ┌──────────────────┐    │
│  │  Nodes   │   │  Endpoints   │   │  PolicyConfigs   │    │
│  │ (노드 목록)│   │ (Pod 메타데이터) │   │ (정책 내부 표현)   │    │
│  └────┬─────┘   └──────┬───────┘   └────────┬─────────┘    │
│       │                │                     │              │
│       ▼                ▼                     ▼              │
│  ┌──────────────── Reconcile() ────────────────────┐       │
│  │                                                  │       │
│  │  1. updateMatchedEndpointIDs()                   │       │
│  │     셀렉터로 Pod 매칭                               │       │
│  │                                                  │       │
│  │  2. regenerateGatewayConfig()                    │       │
│  │     노드 셀렉터로 게이트웨이 결정                     │       │
│  │                                                  │       │
│  │  3. updateEgressRules() [mark-and-sweep]         │       │
│  │     ┌─────────────────────────────────────┐      │       │
│  │     │ (a) 기존 엔트리 stale 표시            │      │       │
│  │     │ (b) forEachEndpointAndCIDR() 순회     │      │       │
│  │     │     - 다중 GW: hash % len(gw) 분배   │      │       │
│  │     │     - excluded CIDR: 0.0.0.1 마킹    │      │       │
│  │     │ (c) stale 엔트리 삭제                 │      │       │
│  │     └─────────────────────────────────────┘      │       │
│  └──────────────────────────────────────────────────┘       │
│                          │                                   │
│                          ▼                                   │
│              ┌──────────────────────┐                        │
│              │   BPF 정책 맵 (eBPF)  │                        │
│              │                      │                        │
│              │  Key: (SrcIP, DstCIDR)│                        │
│              │  Val: (EgressIP, GwIP)│                        │
│              └──────────────────────┘                        │
└─────────────────────────────────────────────────────────────┘
         │
         ▼ (커널 데이터플레인에서 패킷별 SNAT 수행)
  Pod 트래픽 → 게이트웨이 노드 → 고정 IP로 SNAT → 외부
```

## 코드 해설

### 1. `PolicyConfig` 구조체 (라인 62-71)

정책 CRD의 내부 표현이다. `EndpointSelectors`로 대상 Pod을, `DstCIDRs`/`ExcludedCIDRs`로 목적지를, `PolicyGwConfigs`로 게이트웨이를 정의한다. `MatchedEndpoints`와 `GatewayConfigs`는 리컨실레이션 시 동적으로 계산되는 런타임 상태다.

실제 Cilium의 `pkg/egressgateway/policy.go`에 정의된 `PolicyConfig`를 단순화한 것이다. 실제 코드에서는 `slimLabels.LabelSelector`를 사용하지만, 여기서는 `map[string]string`으로 대체했다.

### 2. `regenerateGatewayConfig()` 메서드 (라인 151-184)

노드 목록을 순회하며 `PolicyGatewayConfig.NodeSelector`와 매칭되는 노드를 찾아 런타임 `GatewayConfig`를 생성한다. 로컬 노드가 게이트웨이인 경우 인터페이스/EgressIP 결정 로직이 포함된다.

실제 Cilium에서는 네트워크 인터페이스에서 실제 IP를 조회하지만, 시뮬레이션에서는 노드 IP를 그대로 사용한다. 이 함수가 중요한 이유는 게이트웨이 노드가 추가/삭제될 때마다 호출되어 정책의 실제 라우팅 대상을 결정하기 때문이다.

### 3. `forEachEndpointAndCIDR()` 메서드 (라인 194-221)

모든 (매칭된 엔드포인트 IP, 대상 CIDR) 조합을 순회하며 콜백을 호출한다. 다중 게이트웨이가 있을 때 FNV-1a 해시 기반으로 엔드포인트별 게이트웨이를 결정하는 것이 핵심이다. `computeEndpointHash(ep.ID) % len(gateways)`로 일관된 분배를 보장한다.

이 패턴은 Cilium의 동일 이름 메서드를 직접 모방한 것으로, 게이트웨이를 IP 기준 정렬하여 해시 분배의 일관성을 보장하는 점까지 재현했다.

### 4. `updateEgressRules()` 메서드 (라인 300-338)

eBPF 맵 업데이트의 핵심인 mark-and-sweep 패턴을 구현한다:
- (a) 기존 모든 엔트리를 stale로 표시
- (b) 현재 정책에서 필요한 엔트리를 계산하며 stale 표시를 제거
- (c) 남은 stale 엔트리를 삭제

실제 Cilium의 `updateEgressRules4()`와 동일한 패턴이다. 이 방식은 정책/엔드포인트가 변경될 때 불필요한 규칙이 자동으로 정리되며, 변경되지 않은 엔트리는 재기록하지 않아 효율적이다.

### 5. `Reconcile()` 메서드 (라인 277-297)

리컨실레이션의 3단계 흐름을 보여준다: 엔드포인트 매칭 -> 게이트웨이 설정 재생성 -> BPF 맵 업데이트. 실제 Cilium에서는 노드/엔드포인트/정책 변경 이벤트가 발생할 때마다 이 흐름이 트리거된다. 시뮬레이션에서는 명시적으로 호출한다.

## 실행 방법 및 예상 출력

```bash
go run main.go
```

### 주요 출력 발췌

**시나리오 2 -- 단일 게이트웨이 정책 적용 후 BPF 맵 상태:**

```
  ┌─ 정책:
  │  egw-production (엔드포인트 2개 매칭, 게이트웨이 1개)
  │    GW: 10.0.0.1 (egress=10.0.0.1, iface=eth0) [LOCAL]
  ├─ BPF 정책 맵:
  │  (10.244.1.10 → 0.0.0.0/0) → egress=10.0.0.1, gw=10.0.0.1
  │  (10.244.1.10 → 10.0.0.0/8) → egress=10.0.0.1, gw=0.0.0.1 [EXCLUDED]
  │  (10.244.2.20 → 0.0.0.0/0) → egress=10.0.0.1, gw=10.0.0.1
  │  (10.244.2.20 → 10.0.0.0/8) → egress=10.0.0.1, gw=0.0.0.1 [EXCLUDED]
```

`app=backend, env=production` Pod 2개가 매칭되어 모든 외부 트래픽(0.0.0.0/0)이 worker-1(10.0.0.1)을 통해 나간다. 내부 대역(10.0.0.0/8)은 `[EXCLUDED]`로 표시되어 SNAT을 건너뛴다.

**시나리오 3 -- 다중 게이트웨이 해시 분배:**

```
  해시 기반 분배 확인:
    uid-pod-1: hash=1656814937, 게이트웨이 인덱스=1
    uid-pod-2: hash=1606482080, 게이트웨이 인덱스=0
```

FNV-1a 해시값을 게이트웨이 수(2)로 나눈 나머지로 게이트웨이가 결정된다. Pod마다 일관되게 같은 게이트웨이에 할당된다.

**시나리오 4 -- 새 엔드포인트 추가 시 증분 업데이트:**

```
    BPF 맵 적용: (10.244.3.40 → 203.0.113.0/24) → egress=198.51.100.10, gw=10.0.0.1
```

기존 엔트리는 유지되고 새 Pod에 대한 규칙만 추가된다. mark-and-sweep이 변경분만 처리하는 것을 보여준다.

## 핵심 포인트

1. **Mark-and-Sweep 패턴**: eBPF 맵을 선언적으로 관리하는 핵심 기법이다. 원하는 상태를 계산하고, 기존 상태와 비교하여 불필요한 것만 삭제한다. Kubernetes 컨트롤러 패턴의 데이터플레인 버전이라 할 수 있다.

2. **해시 기반 게이트웨이 분배**: 다중 게이트웨이 환경에서 엔드포인트 UID의 FNV-1a 해시를 사용하여 일관된 분배를 보장한다. 게이트웨이를 IP 기준으로 정렬한 후 해시하므로, 동일 엔드포인트는 항상 같은 게이트웨이에 할당된다.

3. **제외 CIDR의 특수 IP 처리**: 제외된 CIDR은 GatewayIP를 `0.0.0.1`로 설정하여 eBPF 맵에 명시적으로 "이 목적지는 SNAT하지 말라"를 기록한다. 화이트리스트가 아닌 블랙리스트 방식이다.

4. **이벤트 기반 리컨실레이션**: 노드 추가/삭제, Pod 생성/종료, 정책 변경 등 어떤 이벤트가 발생해도 동일한 리컨실레이션 흐름을 실행한다. 멱등성(idempotent)이 보장되므로 중복 실행해도 결과가 같다.

5. **로컬 노드 인식**: 게이트웨이 노드가 자기 자신인 경우를 별도로 처리한다(`LocalNodeConfiguredAsGateway`). 이 노드에서만 실제 SNAT 수행이 필요하고, 다른 노드에서는 해당 게이트웨이로 터널링만 하면 되기 때문이다.

## 실제 Cilium과의 차이점

| 항목 | 이 시뮬레이션 | 실제 Cilium |
|------|-------------|-------------|
| 데이터플레인 | Go map으로 BPF 맵 시뮬레이션 | 커널 eBPF 맵 + tc/XDP 프로그램으로 패킷 SNAT |
| 이벤트 처리 | 명시적 `Reconcile()` 호출 | K8s informer + hive 이벤트 시스템으로 자동 트리거 |
| 레이블 셀렉터 | `map[string]string` 완전 일치 | `slimLabels.LabelSelector` (matchExpressions, In/NotIn 등 지원) |
| 네트워크 인터페이스 | 노드 IP를 그대로 사용 | `netlink`로 실제 인터페이스 IP 조회, VIP 지원 |
| IP 할당 | 정적 설정만 지원 | CiliumEgressNATPolicy를 통한 IP 풀 할당 지원 |
| HA/장애복구 | 미구현 | 게이트웨이 노드 장애 시 자동 failover, health probing |
| IPv6 | IPv4만 지원 | IPv4/IPv6 듀얼 스택 지원 (`EgressPolicyKey6`) |
| 성능 | 단순 맵 순회 | `slices.BinarySearchFunc`으로 노드 검색 최적화, 배치 BPF 맵 업데이트 |
| CRD 연동 | 하드코딩된 정책 | K8s API 서버에서 CRD watch, 버전 관리 |
