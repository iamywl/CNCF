# 2. Cilium 데이터 모델 (ERD)

---

## 핵심 개념 관계도

Cilium의 핵심 데이터는 IP 주소가 아닌 **Identity(보안 식별자)** 중심으로 동작한다.

```
┌────────────┐       ┌──────────────┐       ┌──────────────┐
│   Label    │N────1 │   Identity   │1────N │   Endpoint   │
│────────────│       │──────────────│       │──────────────│
│ key        │       │ id (uint32)  │       │ id (uint16)  │
│ value      │       │ labels[]     │       │ identity_id  │
│ source     │       │ labelsSHA    │       │ ipv4         │
│            │       │              │       │ ipv6         │
└────────────┘       └──────┬───────┘       │ mac          │
                            │               │ ifIndex      │
                            │               │ state        │
                     ┌──────▼───────┐       │ policyMap    │
                     │   IPCache    │       └──────┬───────┘
                     │──────────────│              │
                     │ ip/cidr (PK) │              │ 적용
                     │ identity_id  │              ▼
                     │ hostIP       │       ┌──────────────┐
                     │ encryptKey   │       │    Policy     │
                     └──────────────┘       │──────────────│
                                            │ name         │
                                            │ namespace    │
                                            │ ingressRules │
                                            │ egressRules  │
                                            └──────┬───────┘
                                                   │
                                            ┌──────▼───────┐
                                            │  L4Filter    │
                                            │──────────────│
                                            │ port         │
                                            │ protocol     │
                                            │ l7Rules[]    │
                                            │ allowsIdent[]│
                                            └──────────────┘
```

---

## Endpoint (엔드포인트)

Cilium이 관리하는 네트워크 단위. 보통 하나의 Pod에 대응된다.

| 필드 | 타입 | 설명 |
|------|------|------|
| id | uint16 | 노드 내 고유 ID |
| containerID | string | Docker/containerd 컨테이너 ID |
| identity | Identity | 이 Endpoint에 할당된 보안 식별자 |
| ipv4 | net.IP | IPv4 주소 |
| ipv6 | net.IP | IPv6 주소 |
| mac | MAC | 가상 인터페이스 MAC 주소 |
| ifIndex | int | 커널 네트워크 인터페이스 인덱스 |
| state | State | 생성중/준비완료/연결해제 등 |
| policyMap | *policymap.PolicyMap | 이 Endpoint 전용 BPF 정책 맵 |

소스 위치: `pkg/endpoint/endpoint.go`

**Why uint16?** — Endpoint ID는 BPF 맵의 키로 사용되므로, BPF 맵 크기를 효율적으로 유지하기 위해 16비트로 제한한다.

---

## Identity (보안 식별자)

Label 집합에 대해 클러스터 전체에서 고유한 숫자 ID.

| 필드 | 타입 | 설명 |
|------|------|------|
| id | NumericIdentity (uint32) | 클러스터 고유 숫자 ID |
| labels | Labels | 이 Identity를 구성하는 라벨 집합 |
| labelsSHA256 | string | 라벨의 SHA256 해시 (중복 검사) |

소스 위치: `pkg/identity/numericidentity.go`, `pkg/identity/identity.go`

### 예약된 Identity

```
ID 0   = unknown           (알 수 없음)
ID 1   = host              (노드 자신)
ID 2   = world             (클러스터 외부)
ID 3   = unmanaged         (Cilium이 관리하지 않는 Endpoint)
ID 4   = health            (헬스 체크)
ID 5   = init              (초기화 중)
ID 6   = remote-node       (다른 노드)
ID 7   = kube-apiserver
ID 8   = ingress           (Ingress 게이트웨이)
ID 9   = world-ipv4        (클러스터 외부, IPv4 전용)
ID 10  = world-ipv6        (클러스터 외부, IPv6 전용)
ID 11  = encrypted-overlay (암호화된 오버레이)
```

**Why Identity 기반?** — IP는 Pod 재시작시 바뀌지만, Label 기반 Identity는 변하지 않는다.
"app=frontend"인 Pod는 IP가 바뀌어도 같은 Identity를 갖는다. 정책이 안정적으로 유지된다.

---

## IPCache (IP → Identity 매핑)

모든 알려진 IP 주소를 Identity에 매핑하는 캐시.

| 필드 | 타입 | 설명 |
|------|------|------|
| ip/cidr | string (PK) | IP 주소 또는 CIDR 블록 |
| identity | NumericIdentity | 매핑된 Identity ID |
| hostIP | net.IP | 해당 IP가 속한 노드의 IP |
| encryptKey | uint8 | IPsec 암호화 키 인덱스 |
| tunnelPeer | net.IP | VXLAN/Geneve 터널 피어 |

소스 위치: `pkg/ipcache/ipcache.go`

```
IPCache 예시:
10.0.1.5/32   → Identity 12345 (app=frontend)
10.0.2.0/24   → Identity 2     (world)
10.0.3.10/32  → Identity 7     (kube-apiserver)
```

---

## Policy (네트워크 정책)

```
┌─────────────────────┐
│   PolicyRepository   │
│─────────────────────│
│                     │          ┌──────────────────┐
│  rules[]  ──────────┼────────▶│      Rule         │
│                     │          │──────────────────│
│                     │          │ endpointSelector  │  ← 이 정책의 대상
│                     │          │ ingress[]         │  ← 인그레스 규칙
│                     │          │ egressRules[]     │  ← 이그레스 규칙
│                     │          │ labels            │  ← 정책 자체의 라벨
└─────────────────────┘          └────────┬─────────┘
                                          │
                                 ┌────────▼─────────┐
                                 │   IngressRule     │
                                 │──────────────────│
                                 │ fromEndpoints[]   │  ← 허용할 소스 Identity
                                 │ fromCIDR[]        │  ← 허용할 소스 CIDR
                                 │ toPorts[]         │  ← L4 포트/프로토콜
                                 └────────┬─────────┘
                                          │
                                 ┌────────▼─────────┐
                                 │    L4Filter       │
                                 │──────────────────│
                                 │ port (uint16)     │
                                 │ protocol (u8proto)│
                                 │ l7Rules           │  ← HTTP, Kafka 등 L7 규칙
                                 │ allowedIdentities │  ← 허용된 Identity 목록
                                 └──────────────────┘
```

소스 위치: `pkg/policy/repository.go`, `pkg/policy/rule.go`

---

## Service / LoadBalancer (서비스 로드밸런싱)

```
┌────────────────┐       ┌────────────────┐
│    Service      │1────N │    Backend      │
│────────────────│       │────────────────│
│ frontend IP    │       │ ip             │
│ frontend Port  │       │ port           │
│ type (ClstrIP, │       │ nodePort       │
│   NodePort,    │       │ weight         │
│   LoadBal.)    │       │ state          │
│ sessionAffinity│       │ preferred      │
│ natPolicy      │       │               │
└────────────────┘       └────────────────┘
```

소스 위치: `pkg/loadbalancer/`

### BPF 맵 구조

```
서비스 맵 (Service Map):
  Key:   { frontend_ip, frontend_port, proto, scope }
  Value: { backend_count, rev_nat_id, flags }

백엔드 맵 (Backend Map):
  Key:   { backend_id }
  Value: { ip, port, proto, flags }

역방향 NAT 맵 (RevNAT Map):
  Key:   { rev_nat_id }
  Value: { original_ip, original_port }
```

---

## Connection Tracking (연결 추적)

```
┌─────────────────────────────┐
│      CT Entry                │
│─────────────────────────────│
│ src_ip, src_port             │  ← 연결의 소스
│ dst_ip, dst_port             │  ← 연결의 목적지
│ protocol                     │
│ lifetime                     │  ← TTL
│ rx_packets, rx_bytes         │  ← 수신 통계
│ tx_packets, tx_bytes         │  ← 송신 통계
│ flags                        │  ← SYN_SEEN, ESTABLISHED 등
│ rev_nat_id                   │  ← 역방향 NAT 매핑
│ src_identity                 │  ← 소스의 Security Identity
└─────────────────────────────┘
```

소스 위치: `pkg/maps/ctmap/ctmap.go`

**Why 커널 CT가 아닌 자체 CT?** — Cilium은 Identity 기반 정책을 적용해야 하므로,
커널의 기본 conntrack 대신 BPF 맵으로 자체 연결 추적을 구현하여 Identity 정보를 함께 저장한다.

---

## Flow (Hubble 관측 데이터)

Hubble이 수집하는 네트워크 이벤트 단위.

| 필드 | 타입 | 설명 |
|------|------|------|
| time | Timestamp | 이벤트 발생 시각 |
| verdict | Verdict | FORWARDED / DROPPED / ERROR / AUDIT / REDIRECTED / TRACED / TRANSLATED |
| drop_reason | uint32 | DROP인 경우 사유 코드 |
| ethernet | Ethernet | L2 정보 (src/dst MAC) |
| IP | IP | L3 정보 (src/dst IP, ipVersion) |
| l4 | Layer4 | TCP/UDP/ICMPv4/ICMPv6 |
| source | Endpoint | 소스 Endpoint 정보 |
| destination | Endpoint | 목적지 Endpoint 정보 |
| Type | FlowType | L3_L4 / L7 |
| l7 | Layer7 | HTTP, DNS, Kafka 등 L7 정보 |
| is_reply | bool | 응답 패킷인지 여부 |
| traffic_direction | Direction | INGRESS / EGRESS |

소스 위치 (protobuf): `api/v1/flow/flow.proto`
