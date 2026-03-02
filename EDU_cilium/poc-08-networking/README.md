# PoC 08: 네트워킹 서브시스템 체험

Cilium의 네트워킹 서브시스템(VXLAN, NAT, BGP, WireGuard, 듀얼스택)의
동작 원리를 순수 Go로 시뮬레이션한다.

---

## 핵심 메커니즘

Cilium은 eBPF 데이터패스에서 터널링, NAT, 암호화를 모두 처리한다.

```
Pod A (Node 1)                                  Pod B (Node 2)
  │                                               ▲
  ▼                                               │
┌───────────────────┐                 ┌───────────────────┐
│ cil_from_container│                 │ cil_to_container  │
│ ├─ Policy 검사    │                 │ ├─ RevNAT 복원    │
│ ├─ NAT (SNAT)     │                 │ └─ Policy 검사    │
│ └─ 라우팅 결정     │                 │                   │
├───────────────────┤                 ├───────────────────┤
│ 터널? → VXLAN 캡슐화│  ───────────▶  │ VXLAN 디캡슐화    │
│ 암호화? → WireGuard │               │ Identity 복원     │
│ 네이티브? → FIB     │               │ 엔드포인트 전달    │
└───────────────────┘                 └───────────────────┘
```

## 시뮬레이션 항목

| # | 항목 | 실제 코드 | 시뮬레이션 내용 |
|---|------|-----------|----------------|
| 1 | VXLAN | `bpf/lib/encap.h` | 캡슐화/디캡슐화, VNI(Identity) 전달 |
| 2 | NAT | `bpf/lib/nat.h` | SNAT(이그레스), DNAT(서비스), RevNAT |
| 3 | BGP | `pkg/bgp/gobgp/server.go` | 피어링, PodCIDR 광고, RIB |
| 4 | WireGuard | `pkg/wireguard/agent/agent.go` | 키 교환, 암호화/복호화, AllowedIPs |
| 5 | 듀얼스택 | `bpf/lib/nat_46x64.h` | IPv4/IPv6 분기, NAT46/64 변환 |

## 실행 방법

```bash
cd EDU/poc-08-networking
go run main.go
```

외부 의존성이 없으며, Go 표준 라이브러리만으로 실행된다.

## 예상 출력

### 1. VXLAN 캡슐화/디캡슐화

원본 패킷에 VXLAN 헤더(Outer IP + UDP + VNI)를 추가하고,
수신 측에서 이를 제거하여 원본 패킷을 복원한다.

```
[캡슐화 전]  10.0.1.15:45678 → 10.0.2.30:80 (TCP)
[캡슐화 후]  192.168.1.10:41256 → 192.168.1.20:8472 (UDP), VNI=12345
[디캡슐화]   10.0.1.15:45678 → 10.0.2.30:80 (TCP) — Identity 복원
```

### 2. NAT 변환

SNAT(Pod→외부), DNAT(서비스 접근), RevNAT(응답 복원)을 시뮬레이션한다.
정방향+역방향 매핑이 쌍으로 생성되는 것을 확인할 수 있다.

```
SNAT: 10.0.1.15:45678 → 192.168.1.10:45678 (노드 IP로 변환)
RevNAT: 192.168.1.10:45678 → 10.0.1.15:45678 (원본 복원)
DNAT: 10.96.0.1:80 → 10.0.2.30:8080 (백엔드로 변환)
```

### 3. BGP 경로 광고

두 노드가 ToR 라우터와 iBGP 피어링을 맺고,
각 노드의 PodCIDR을 광고하는 과정을 시뮬레이션한다.

### 4. WireGuard 터널

키 페어 생성, 피어 교환, 패킷 암호화/복호화를 시뮬레이션한다.
실제 WireGuard는 Noise 프로토콜(ChaCha20-Poly1305)을 사용하지만,
여기서는 SHA256 해시로 은유적으로 표현한다.

### 5. 듀얼스택

IPv4/IPv6 패킷 분류와 NAT46/64 변환(RFC 6052)을 시뮬레이션한다.

## 관련 소스 파일

### BPF (C)

- `bpf/lib/encap.h` -- VXLAN/Geneve 캡슐화 (`__encap_with_nodeid4`)
- `bpf/lib/nat.h` -- SNAT/DNAT 엔진 (`snat_v4_new_mapping`)
- `bpf/lib/nat_46x64.h` -- NAT46/64 변환 (`build_v4_in_v6_rfc6052`)
- `bpf/lib/vtep.h` -- VTEP 맵 (`cilium_vtep_map`)
- `bpf/lib/srv6.h` -- SRv6 세그먼트 라우팅
- `bpf/lib/arp.h` -- ARP 처리 (`arp_prepare_response`)
- `bpf/lib/mcast.h` -- 멀티캐스트/IGMP
- `bpf/bpf_overlay.c` -- 오버레이 패킷 처리 (`cil_from_overlay`)

### Go (유저스페이스)

- `pkg/datapath/tunnel/tunnel.go` -- 터널 설정 (VXLAN/Geneve 선택)
- `pkg/wireguard/agent/agent.go` -- WireGuard Agent
- `pkg/bgp/gobgp/server.go` -- GoBGP 서버 래퍼
- `pkg/bgp/types/bgp.go` -- BGP 타입 (Path, Neighbor)
- `pkg/bgp/manager/reconciler/` -- BGP Reconciler들
- `pkg/maps/nat/nat.go` -- NAT BPF 맵 관리
- `pkg/maps/nat/types.go` -- NAT 키/값 타입
- `pkg/datapath/linux/ipsec/` -- IPsec/XFRM 관리
- `pkg/datapath/linux/node.go` -- 노드 핸들러 (라우팅 결정)
- `pkg/datapath/tables/route.go` -- StateDB 라우트 테이블
