# PoC-07: eBPF 데이터패스 시뮬레이션

## 개요

Cilium의 eBPF 데이터패스는 리눅스 커널 내부에서 패킷을 분류하고, 연결을 추적하며,
보안 정책을 적용하고, 라우팅을 결정하는 파이프라인이다. 이 PoC는 실제 Cilium에서
BPF tail call 체인으로 구현된 5단계 패킷 처리 흐름을 Go 함수 체이닝으로 재현한다.

7개 시나리오를 통해 커널 수준 네트워킹의 핵심 개념을 확인할 수 있다.

---

## 배경 지식

### eBPF 데이터패스란

전통적인 리눅스 네트워킹에서 iptables는 규칙이 선형 리스트(O(n))로 관리되어
Kubernetes의 수천 개 서비스 환경에서 심각한 병목이 된다. Cilium은 eBPF를 활용하여
커널 네트워크 스택 **이전** 단계(TC 훅 포인트)에서 패킷을 가로채, 커널 스택을
전혀 거치지 않고 분류/정책검사/라우팅을 완료한다.

### 핵심 BPF 프로그램 파일

- `bpf/bpf_lxc.c` -- 컨테이너 veth에 부착되는 진입점. `handle_xgress()`가 모든 트래픽을 받는다
- `bpf/bpf_host.c` -- 호스트 인터페이스에 부착. 노드 간 트래픽, NodePort 서비스 처리
- `bpf/lib/conntrack.h` -- CT 조회/생성 (`ct_lookup4()`, `ct_create4()`)
- `bpf/lib/policy.h` -- identity+port+protocol 기반 정책 검사
- `bpf/lib/drop.h` -- 드롭 이벤트 알림 (`cilium monitor`에서 관찰)

### iptables 대비 성능 이점

| 항목 | iptables | Cilium eBPF |
|------|----------|-------------|
| 규칙 조회 | O(n) 선형 탐색 | O(1) BPF hash map |
| conntrack | 커널 nf_conntrack 모듈 | BPF LRU hash map (lock-free) |
| 정책 업데이트 | 전체 규칙 체인 재구성 | 개별 map entry 원자적 갱신 |
| 컨텍스트 스위칭 | 커널 스택 전체 통과 | TC 훅에서 바로 처리 |
| 확장성 | 서비스 1000개 이상에서 성능 저하 | 수만 개 서비스에서도 일정한 성능 |

---

## 시뮬레이션하는 개념

| 실제 Cilium 개념 | PoC 구현 | 설명 |
|------------------|----------|------|
| BPF tail call chain | `DatapathPipeline.ProcessPacket()` | 5단계 함수 체이닝으로 tail call 시뮬레이션 |
| `cilium_ct4_global` LRU hash map | `CTTable` (Go map) | 5-tuple 키 기반 연결 추적 테이블 |
| `cilium_policy_<ep_id>` BPF map | `PolicyMap` (슬라이스) | deny-first 원칙의 정책 맵 |
| `struct ipv4_ct_tuple` | `Tuple5` 구조체 | SrcIP/DstIP/SrcPort/DstPort/Protocol |
| Security Identity (레이블 기반) | `SecurityIdentity` 상수 | World=2, Host=1, App=1000 등 |
| `validate_ethertype()` | `classifyPacket()` | EtherType 분류 (IPv4/IPv6/ARP) |
| `ct_lookup4()` / `ct_create4()` | `ctLookup()` | CT 조회 + 정/역방향 매칭 |
| `policy_can_access_ingress()` | `policyCheck()` | identity/port/proto 기반 정책 판정 |
| `ipv4_local_delivery()` / `encap_and_redirect()` | `routingDecision()` | 로컬 전달 vs 터널 캡슐화 |
| Drop notification | `DropNotification` | 정책 거부 시 드롭 이벤트 기록 |

---

## 아키텍처 다이어그램

### 데이터패스 파이프라인 (Tail Call Chain)

```
  패킷 도착 (TC ingress hook)
       │
       ▼
  ┌─────────────────┐
  │ classify_packet  │  EtherType 분류
  │ (validate_       │  IPv4 → 다음 단계
  │  ethertype)      │  ARP  → 커널 스택
  └────────┬────────┘  기타  → DROP
           │
           ▼
  ┌─────────────────┐
  │ extract_tuple    │  5-tuple 추출
  │ (ct_extract_     │  SrcIP:SrcPort →
  │  tuple4)         │  DstIP:DstPort/Proto
  └────────┬────────┘
           │
           ▼
  ┌─────────────────┐
  │ ct_lookup        │  Conntrack 조회
  │ (ct_lookup4)     │  NEW → 엔트리 생성
  │                  │  EST → 통계 업데이트
  └────────┬────────┘  REPLY → 역방향 매칭
           │
           ▼
  ┌─────────────────┐
  │ policy_check     │  정책 검사
  │ (policy_can_     │  EST/REPLY → 건너뜀
  │  access_ingress) │  NEW → policymap 조회
  └────────┬────────┘  DENY → DROP + 알림
           │
           ▼
  ┌─────────────────┐
  │ routing_decision │  라우팅 결정
  │ (ipv4_local_     │  로컬 → 직접 전달
  │  delivery /      │  원격 → VXLAN 터널
  │  encap_redirect) │  프록시 → L7 리다이렉트
  └─────────────────┘
```

---

## 코드 해설

### 1. `Tuple5` 구조체 — 연결 식별자

5-tuple(SrcIP, DstIP, SrcPort, DstPort, Protocol)로 네트워크 연결을 고유하게 식별한다.
Cilium의 `struct ipv4_ct_tuple`(bpf/lib/conntrack.h)에 대응한다. `ReverseTuple()`
메서드로 역방향 튜플을 생성하여 응답 패킷을 매칭한다.

### 2. `CTTable` — Conntrack 테이블

`cilium_ct4_global` BPF LRU hash map을 Go map으로 시뮬레이션한다. `Lookup()`은
정방향 키를 먼저 조회하고, 실패하면 역방향 키로 재조회하여 REPLY 상태를 감지한다.
확립된 연결은 정책 재검사를 건너뛰므로 이 CT가 성능 최적화의 핵심이다.

### 3. `PolicyMap.Check()` — 정책 판정 엔진

Cilium의 deny-first 원칙을 구현한다. DENY 규칙을 먼저 전체 순회하고, 매칭되면
즉시 거부한다. `DstPort=0` 또는 `Protocol=0`은 와일드카드로 동작한다.
`VerdictRedirectProxy` 판정 시 Envoy L7 프록시 포트 번호도 함께 반환한다.

### 4. `DatapathPipeline.ProcessPacket()` — Tail Call 체인

실제 Cilium에서는 각 단계가 독립 BPF 프로그램이며 `bpf_tail_call()`로 호출된다.
tail call은 스택을 재사용하여 BPF 명령어 수 제한을 우회한다. 이 PoC에서는
`[]struct{name, fn}` 슬라이스를 순회하며, 에러 발생 시 체인을 중단한다.

### 5. `DatapathContext` — BPF 프로그램 간 공유 컨텍스트

실제 Cilium에서는 `__ctx_buff`(skb/xdp) 메타데이터와 per-CPU 변수로 단계 간
상태를 전달한다. 이 PoC에서는 패킷 정보, tuple, CT 상태, 정책 판정, 라우팅 액션,
트레이스 이벤트를 하나의 구조체에 담아 전달한다.

---

## 실행 방법 및 예상 출력

```bash
cd cilium_EDU/poc-07-ebpf-datapath
go run main.go
```

7개 시나리오가 순차 실행되며, 각 시나리오의 주요 결과는 다음과 같다.

| 시나리오 | 트래픽 | CT 상태 | 정책 | 최종 액션 |
|----------|--------|---------|------|----------|
| 1 | Frontend -> App:80 | NEW | ALLOW | LOCAL_DELIVERY |
| 2 | Frontend -> App:80 (재전송) | ESTABLISHED | CT hit (건너뜀) | LOCAL_DELIVERY |
| 3 | App -> Frontend (응답) | REPLY | CT hit (건너뜀) | LOCAL_DELIVERY |
| 4 | World -> App:80 | NEW | DENY | DROP |
| 5 | Frontend -> App:8080 | NEW | REDIRECT_PROXY | LOCAL_DELIVERY (proxy:15001) |
| 6 | App -> RemoteDB:3306 | NEW | ALLOW | TUNNEL_ENCAP |
| 7 | ARP 패킷 | - | - | KERNEL_STACK |

---

## 핵심 포인트

1. **커널 스택 우회**: TC 훅에서 패킷을 가로채 iptables를 완전히 우회하여, 수만 개 서비스에서도 일정한 성능을 유지한다.
2. **Tail call 체인**: BPF 명령어 수 제한을 우회하기 위해 스택을 재사용하며 별도 프로그램을 호출, 복잡한 로직을 단계별로 분할한다.
3. **Conntrack 최적화**: ESTABLISHED/REPLY 패킷은 정책 재검사를 건너뛴다. 대부분의 트래픽이 기존 연결의 후속 패킷이므로 성능 영향이 크다.
4. **Deny-first 정책**: DENY가 ALLOW보다 항상 우선 평가되어, 과도한 허용 규칙이 있어도 명시적 거부가 보호벽 역할을 한다.
5. **Identity 기반 보안**: IP 대신 Pod 레이블 기반 숫자 ID를 사용하여, Pod 재시작으로 IP가 바뀌어도 동일 정책이 적용된다.

---

## 실제 Cilium과의 차이점

| 항목 | 실제 Cilium | 이 PoC |
|------|------------|--------|
| 실행 환경 | 리눅스 커널 내 eBPF VM | Go 유저스페이스 |
| 데이터 구조 | BPF hash/LRU map (커널 메모리, lock-free) | Go map/slice (힙 메모리) |
| tail call | `bpf_tail_call()`로 BPF 프로그램 간 점프 | 함수 슬라이스 순차 호출 |
| 정책 맵 | 엔드포인트별 BPF map, 에이전트가 동적 갱신 | 하드코딩된 슬라이스 |
| NAT/LB | `cilium_lb4_services` map 기반 DNAT/SNAT | 미구현 |
| VXLAN/XDP | 커널 VXLAN + NIC 레벨 XDP 처리 | 라우팅 판정만 시뮬레이션 |
| IPv6/메트릭 | 완전 지원, per-CPU 카운터 | IPv4만, TraceEvent 로그만 |
