# PoC-08: Cilium 네트워킹 시뮬레이션 (VXLAN 터널링 + Direct Routing)

## 개요

Cilium은 쿠버네티스 클러스터에서 Pod 간 통신을 위해 두 가지 네트워킹 모드를 지원한다.
이 PoC는 **VXLAN 터널 모드**와 **Direct Routing 모드**의 핵심 동작 원리를
Go 표준 라이브러리만으로 시뮬레이션한다. VXLAN 캡슐화/역캡슐화, FIB(Forwarding Information Base) 조회,
터널 맵 기반 노드 탐색, 그리고 패킷 크기 오버헤드 비교까지 다룬다.

## 배경 지식

### VXLAN 터널 모드 vs Direct Routing 모드

쿠버네티스에서 Pod IP는 노드마다 별도 CIDR 대역을 할당받는다.
서로 다른 노드의 Pod끼리 통신하려면 물리 네트워크를 넘어야 하는데, 이때 두 가지 방식이 존재한다.

**VXLAN 터널 모드 (오버레이 네트워크)**
- 원본 패킷을 외부 IP/UDP/VXLAN 헤더로 감싸서(캡슐화) 노드 간 전송한다.
- 물리 네트워크는 노드 IP만 알면 되므로 **인프라 독립적**으로 동작한다.
- Cilium은 VNI(Virtual Network Identifier, 24비트) 필드에 **Security Identity**를 인코딩한다.
  수신 측에서 별도 조회 없이 패킷 출처의 보안 ID를 즉시 파악할 수 있다.

**Direct Routing 모드 (네이티브 라우팅)**
- 캡슐화 없이 원본 패킷을 그대로 전송한다. 물리 네트워크(BGP 등)가 Pod CIDR 경로를 알아야 한다.
- 50바이트 오버헤드가 없으므로 **최대 성능**을 달성할 수 있다.

**오버레이가 필요한 이유**: 클라우드/온프레미스의 물리 네트워크는 Pod IP(10.x.x.x)를 인식하지 못한다.
오버레이는 노드 IP로 감싸서 전송함으로써 인프라 변경 없이 Pod 간 통신을 가능하게 한다.

### FIB 룩업

FIB(Forwarding Information Base)는 커널의 라우팅 결정 테이블이다.
Cilium은 BPF 내에서 `bpf_fib_lookup()` 헬퍼를 호출하여 커널 스택을 거치지 않고
다음 홉과 출력 인터페이스를 결정한다. **Longest Prefix Match(LPM)**으로 가장 구체적인 경로를 선택한다.

### 모드 선택 기준

| 기준 | VXLAN 터널 | Direct Routing |
|------|-----------|----------------|
| 인프라 요구사항 | 없음 (어디서든 동작) | BGP/라우터 설정 필요 |
| 성능 | 50B 오버헤드, 캡슐화 CPU 비용 | 네이티브 성능 |
| MTU | 1450B (1500 - 50) | 1500B (표준) |
| 운영 복잡도 | 낮음 | 높음 (네트워크 팀 협조) |
| 대표 환경 | 퍼블릭 클라우드, 멀티테넌트 | 베어메탈, 고성능 요구 |

## 시뮬레이션하는 개념

| 개념 | 구현 | 실제 Cilium 대응 |
|------|------|-----------------|
| VXLAN 캡슐화 | `TunnelEngine.Encapsulate()` | `__encap_and_redirect_with_nodeid()` (bpf/lib/encap.h) |
| VXLAN 역캡슐화 | `TunnelEngine.Decapsulate()` | `handle_xgress()` VXLAN 인터페이스 수신 처리 |
| VNI에 Identity 인코딩 | `VXLANHeader.VNI` 필드 사용 | VNI 24비트를 Security Identity로 활용 |
| FIB 조회 (LPM) | `FIBTable.Lookup()` | `bpf_fib_lookup()` 커널 헬퍼 |
| 터널 맵 | `TunnelMap.Lookup()` | `cilium_tunnel_map` BPF 맵 |
| Direct Routing | `DirectRoutingEngine.Route()` | `fib_redirect_v4()` (bpf/lib/nodeport.h) |
| ECMP 해시 | `hashPacket()` | 내부 5-tuple 해시 기반 소스 포트 생성 |

## 아키텍처/흐름 다이어그램

### VXLAN 터널 모드 패킷 흐름

```
  Node A (192.168.1.10)                    Node B (192.168.1.20)
  ┌─────────────────────┐                  ┌─────────────────────┐
  │ Pod-A (10.0.0.5)    │                  │ Pod-B (10.0.1.15)   │
  │   │ 원본 패킷       │                  │   ▲ 원본 패킷       │
  │   ▼                 │                  │   │                 │
  │ [BPF 프로그램]      │                  │ [BPF 프로그램]      │
  │   │                 │                  │   ▲                 │
  │   ├─ TunnelMap 조회 │                  │   ├─ VNI→Identity   │
  │   ├─ FIB 조회       │                  │   ├─ 역캡슐화       │
  │   ├─ VXLAN 캡슐화   │                  │   ├─ 정책 검사      │
  │   ▼                 │                  │   │                 │
  │ [VXLAN 패킷]        │  ──── 물리 ────> │ [VXLAN 패킷]        │
  │ Outer: 192.168.1.10 │    네트워크      │ UDP:4789 수신       │
  │  → 192.168.1.20     │                  │                     │
  └─────────────────────┘                  └─────────────────────┘
```

### VXLAN 패킷 구조 (50바이트 오버헤드)

```
  ┌──────────────┬──────────────┬──────────────┬──────────────┐
  │ Outer Eth    │ Outer IP     │ Outer UDP    │ VXLAN Header │
  │ (14B)        │ (20B)        │ (8B)         │ (8B)         │
  │ Dst/Src MAC  │ Node IP 간   │ dst=4789     │ VNI=Identity │
  ├──────────────┴──────────────┴──────────────┴──────────────┤
  │              원본 패킷 (Inner Packet)                      │
  │ [Inner Eth 14B][Inner IP 20B][TCP/UDP][Payload]           │
  └───────────────────────────────────────────────────────────┘
```

### Direct Routing 모드

```
  Node A                                   Node C
  Pod-A → [BPF: FIB 조회] → eth0 ──물리 네트워크(BGP)──> eth0 → Pod-C
          (캡슐화 없음, 원본 패킷 그대로 전송)
```

## 코드 해설

### 1. `VXLANHeader` - VXLAN 헤더와 Security Identity

RFC 7348 기반 8바이트 헤더. `Flags`의 `0x08` 비트가 VNI 유효를 나타내며,
**VNI 필드에 Security Identity를 인코딩**하는 Cilium 핵심 설계를 구현한다.

### 2. `TunnelMap` - BPF 터널 맵 시뮬레이션

실제 `cilium_tunnel_map`에 대응. Pod CIDR → 원격 노드 IP 매핑을 저장하고,
`Lookup(podIP)`로 목적지 Pod가 어느 노드에 있는지 찾는다.
(실제는 LPM trie O(log n), 이 PoC는 순회 O(n))

### 3. `FIBTable.Lookup()` - Longest Prefix Match 라우팅

커널 FIB의 LPM을 시뮬레이션. 목적지 IP를 포함하는 네트워크 중
**가장 긴 접두사**를 선택하여 다음 홉 IP, 출력 인터페이스, MAC을 반환한다.

### 4. `TunnelEngine.Encapsulate()` - VXLAN 캡슐화

터널 맵 조회 → FIB 조회 → 내부 패킷 해시로 소스 포트 생성(ECMP) →
Outer Eth/IP/UDP + VXLAN + Inner 조립. Cilium의 `__encap_and_redirect_with_nodeid()`에 대응.

### 5. `DirectRoutingEngine.Route()` - 네이티브 라우팅

캡슐화 없이 FIB 조회만으로 패킷 전달. Cilium의 `fib_redirect_v4()`에 대응.

## 실행 방법 및 예상 출력

```bash
cd cilium_EDU/poc-08-networking
go run main.go
```

### 예상 출력 (요약)

4개 시나리오가 순서대로 실행된다.

```
시나리오 1: VXLAN 캡슐화 (Node A Pod → Node B Pod)
  원본: 10.0.0.5 → 10.0.1.15
  캡슐화: Outer 192.168.1.10 → 192.168.1.20, VNI=12345, UDP:4789
  과정: 터널맵 조회 → FIB 조회 → VNI에 Identity 인코딩 → 소스포트 해시

시나리오 2: VXLAN 역캡슐화 (Node B에서 수신)
  수신 VXLAN 패킷 → 역캡슐화 → 원본 복원
  추출된 Security Identity: 12345 → 정책 검사 수행

시나리오 3: Direct Routing 모드
  원본: 10.0.0.5 → 10.0.2.30
  FIB 조회 → 다음 홉: 192.168.1.30, 인터페이스: eth0
  캡슐화 없이 원본 그대로 전송 (50B 오버헤드 없음)

시나리오 4: 패킷 크기 비교
  VXLAN 오버헤드: 50B (Outer Eth 14 + IP 20 + UDP 8 + VXLAN 8)
  VXLAN 전체: 1504B / Direct Routing 전체: 1454B
```

## 핵심 포인트

1. **VNI = Security Identity**: VNI 필드를 보안 식별자로 재활용하여 수신 측에서 별도 조회 없이 출처 identity를 즉시 파악, 정책 적용 지연을 최소화한다.
2. **BPF 내부 FIB 조회**: `bpf_fib_lookup()`으로 커널 스택을 우회하면서 라우팅 테이블 결과를 활용한다.
3. **ECMP 소스 포트 해시**: 내부 패킷 IP 해시로 외부 UDP 소스 포트를 생성하여 동일 흐름이 같은 경로를 선택하도록 보장한다.
4. **50바이트 오버헤드**: Outer Eth(14)+IP(20)+UDP(8)+VXLAN(8) = 50B. MTU 1500 기준 Inner MTU 1450으로 줄어든다.

## 실제 Cilium과의 차이점

| 항목 | 이 PoC | 실제 Cilium |
|------|--------|-------------|
| 실행 위치 | Go 유저스페이스 | eBPF TC/XDP 커널 내부 |
| 터널 맵 | Go map + CIDR 순회 | BPF LPM trie (`cilium_tunnel_map`) |
| FIB 조회 | 배열 순회 LPM | `bpf_fib_lookup()` 커널 헬퍼 |
| VNI | uint32 (단순화) | 24비트 (최대 16,777,215) |
| 터널 프로토콜 | VXLAN만 | VXLAN + Geneve |
| Identity 전파 | VNI 필드만 | VXLAN, ICMP, WireGuard 등 다중 경로 |
| Direct Routing | FIB 조회만 | BGP, AWS ENI, GKE 네이티브 등 연동 |
| 패킷 전송 | 없음 (출력만) | 커널 통해 실제 송수신 |
