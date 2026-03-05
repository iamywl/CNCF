# 07. eBPF 데이터패스 Deep Dive

## 개요

Cilium의 데이터플레인은 Linux 커널 내부에서 실행되는 eBPF(extended Berkeley Packet Filter) 프로그램으로
구성된다. 기존 iptables 기반 패킷 처리를 BPF 맵의 O(1) 룩업으로 대체하여 고성능을 달성하며,
프로그램 동적 교체로 서비스 중단 없이 정책을 업데이트할 수 있다.

이 문서에서는 BPF 프로그램의 구조, 패킷 처리 흐름, ConnTrack, 정책 적용, Tail Call 메커니즘을
소스코드 기반으로 상세히 분석한다.

## BPF 프로그램 디렉토리 구조

```
bpf/
├── bpf_lxc.c           # 컨테이너(Pod) 인그레스/이그레스 (2,670줄)
├── bpf_host.c          # 호스트 네트워크 인터페이스 (1,916줄)
├── bpf_overlay.c       # 오버레이 네트워크 (VXLAN/Geneve) (663줄)
├── bpf_xdp.c           # XDP 로드밸런싱 (370줄)
├── bpf_sock.c          # 소켓 레벨 LB (cgroup hook) (1,279줄)
├── bpf_wireguard.c     # WireGuard 암호화 (388줄)
├── bpf_sock_term.c     # 소켓 종료 (172줄)
├── bpf_probes.c        # 시스템 기능 탐지 (42줄)
├── bpf_alignchecker.c  # 구조체 정렬 검증 (116줄)
│
├── ep_config.h         # 엔드포인트별 설정 헤더
├── filter_config.h     # 필터 설정
├── netdev_config.h     # 네트워크 디바이스 설정
├── node_config.h       # 노드 전체 설정
│
└── lib/                # 공유 헤더 라이브러리 (~44개, 22,678줄)
    ├── common.h        # 공통 타입, 매크로, CT 튜플 구조체
    ├── conntrack.h     # 연결 추적 (ConnTrack) (38.4KB)
    ├── policy.h        # 정책 조회/적용 (16.4KB)
    ├── lb.h            # 로드밸런싱 로직
    ├── nodeport.h      # NodePort/DSR 구현 (84.8KB)
    ├── encap.h         # 터널 캡슐화
    ├── nat.h           # NAT 처리
    ├── tailcall.h      # Tail Call 관리 (5.1KB)
    ├── trace.h         # 패킷 트레이싱
    ├── drop.h          # 드롭 알림
    ├── events.h        # Perf 이벤트 맵
    ├── ipv4.h / ipv6.h # IP 프로토콜 처리
    ├── eth.h           # 이더넷 헤더 처리
    ├── eps.h           # 엔드포인트/IPCache 조회
    ├── auth.h          # 인증 확인
    ├── proxy.h         # L7 프록시 리다이렉트
    ├── wireguard.h     # WireGuard 리다이렉트
    ├── host_firewall.h # 호스트 방화벽
    ├── egress_gateway.h # Egress 게이트웨이
    ├── clustermesh.h   # 멀티클러스터
    └── ratelimit.h     # EDT 기반 대역폭 제한
```

**총 코드량**: 메인 프로그램 ~7,600줄 + 라이브러리 헤더 ~22,700줄 ≈ **30,000줄** C 코드

## 메인 BPF 프로그램과 진입점

### 프로그램-인터페이스 어태치 매핑

```
┌─────────────────────────────────────────────────────────────┐
│ 물리 NIC (eth0)                                              │
│  TC ingress: cil_from_netdev() ← bpf_host.c                │
│  TC egress:  cil_to_netdev()   ← bpf_host.c                │
│  XDP:        cil_xdp_entry()   ← bpf_xdp.c (선택적)       │
├─────────────────────────────────────────────────────────────┤
│ cilium_host (가상 인터페이스)                                 │
│  TC egress:  cil_from_host()   ← bpf_host.c                │
├─────────────────────────────────────────────────────────────┤
│ cilium_vxlan / cilium_geneve (터널 인터페이스)               │
│  TC ingress: cil_from_overlay() ← bpf_overlay.c            │
│  TC egress:  cil_to_overlay()   ← bpf_overlay.c            │
├─────────────────────────────────────────────────────────────┤
│ lxc-xxx / netkit (Pod veth/netkit)                           │
│  TC ingress: cil_from_container() ← bpf_lxc.c              │
│  TC egress:  cil_to_container()   ← bpf_lxc.c              │
├─────────────────────────────────────────────────────────────┤
│ cgroup v2 (소켓 레벨)                                        │
│  connect4/6: sock4_xlate_fwd() ← bpf_sock.c                │
│  sendmsg4/6: sock4_xlate_snd() ← bpf_sock.c                │
├─────────────────────────────────────────────────────────────┤
│ cilium_wg0 (WireGuard 인터페이스)                            │
│  TC ingress: cil_from_wireguard() ← bpf_wireguard.c        │
│  TC egress:  cil_to_wireguard()   ← bpf_wireguard.c        │
└─────────────────────────────────────────────────────────────┘
```

### 각 프로그램의 역할

| 프로그램 | 진입점 | 줄 번호 | 역할 |
|----------|--------|---------|------|
| `bpf_lxc.c` | `cil_from_container()` | 1750 | Pod에서 나가는 패킷 처리 (이그레스 정책) |
| `bpf_lxc.c` | `cil_lxc_policy()` | 2452 | Pod으로 들어오는 패킷 정책 적용 |
| `bpf_lxc.c` | `cil_to_container()` | 2561 | Pod 엔드포인트로 최종 배달 |
| `bpf_host.c` | `cil_from_netdev()` | 1178 | 물리 NIC에서 수신 (TC ingress) |
| `bpf_host.c` | `cil_from_host()` | 1261 | cilium_host에서 송신 |
| `bpf_host.c` | `cil_to_netdev()` | 1317 | 물리 NIC으로 송신 (TC egress) |
| `bpf_overlay.c` | `handle_ipv4/6()` | 51-149 | 오버레이 네트워크 디캡슐화 후 처리 |
| `bpf_xdp.c` | `tail_lb_ipv4()` | 98 | XDP 레벨 NodePort LB (최고 성능) |
| `bpf_sock.c` | `sock4_xlate_fwd()` | - | 소켓 레벨 서비스 해석 (connect 시점) |

## 핵심 데이터 구조체

### ConnTrack 튜플 (CT Tuple)

패킷의 5-튜플(소스/대상 IP, 소스/대상 포트, 프로토콜)을 표현한다.
CT 조회의 키로 사용된다.

```c
// bpf/lib/common.h
struct ipv4_ct_tuple {
    __be32 daddr;      // 대상 IPv4 주소 (reply 방향에서는 반전)
    __be32 saddr;      // 소스 IPv4 주소 (reply 방향에서는 반전)
    __be16 dport;      // 대상 포트
    __be16 sport;      // 소스 포트
    __u8   nexthdr;    // 프로토콜 (TCP=6, UDP=17, ICMP=1)
    __u8   flags;      // 플래그 (TUPLE_F_IN, TUPLE_F_RELATED)
};

struct ipv6_ct_tuple {
    union v6addr daddr;
    union v6addr saddr;
    __be16 dport;
    __be16 sport;
    __u8   nexthdr;
    __u8   flags;
};
```

### ConnTrack 엔트리 (CT Entry)

CT 맵에 저장되는 연결 상태 정보다. 키는 위의 CT 튜플이다.

```c
// bpf/lib/conntrack.h:85-126
struct ct_entry {
    union v6addr nat_addr;     // NAT 주소 (이그레스) 또는 예약
    __u64 backend_id;          // 서비스 백엔드 ID
    __u64 packets;             // 패킷 카운터 (atomic)
    __u64 bytes;               // 바이트 카운터 (atomic)
    __u32 lifetime;            // CT 엔트리 만료 시각

    // 비트필드 플래그들
    __u16 rx_closing:1,        // RX 방향 연결 종료 중
          tx_closing:1,        // TX 방향 연결 종료 중
          lb_loopback:1,       // 서비스 헤어핀 (자기 자신으로 LB)
          seen_non_syn:1,      // SYN 아닌 TCP 패킷 관찰
          node_port:1,         // NodePort 연결
          proxy_redirect:1,    // L7 프록시 리다이렉트
          dsr_internal:1,      // DSR 서비스
          from_l7lb:1,         // L7 LB에서 생성
          from_tunnel:1;       // 터널에서 수신

    __u16 rev_nat_index;       // 역방향 NAT 인덱스
    __be16 nat_port;           // NAT 포트
    __u8  tx_flags_seen;       // 관찰된 TCP 플래그 (TX)
    __u8  rx_flags_seen;       // 관찰된 TCP 플래그 (RX)
    __u32 src_sec_id;          // 소스 보안 Identity
    __u32 last_tx_report;      // 마지막 TX 리포트 시각
    __u32 last_rx_report;      // 마지막 RX 리포트 시각
};
```

### ct_state (함수 간 전달용)

```c
// bpf/lib/conntrack.h:41-57
struct ct_state {
    union v6addr nat_addr;     // NAT 주소
    __be16 nat_port;           // NAT 포트
    __u16  rev_nat_index;      // 역방향 NAT 인덱스

    // 비트필드 플래그들
    __u16  loopback:1,         // 서비스 헤어핀
           node_port:1,        // NodePort
           dsr_internal:1,     // DSR
           syn:1,              // TCP SYN 관찰
           proxy_redirect:1,   // 프록시 리다이렉트
           from_l7lb:1;        // L7 LB 출처

    __u32  src_sec_id;         // 소스 보안 Identity
    __u32  backend_id;         // LB 백엔드 ID
};
```

### 정책 키/엔트리 (Policy Map)

```c
// bpf/lib/policy.h:56-78
struct policy_key {
    struct bpf_lpm_trie_key lpm_key;  // LPM 접두사 (최상위 필드)
    __u32  sec_label;                  // 소스 보안 Identity
    __u8   egress:1,                   // 0=인그레스, 1=이그레스
           pad:7;
    __u8   protocol;                   // L4 프로토콜 (와일드카드 가능)
    __be16 dport;                      // 대상 포트 (부분 와일드카드)
};

struct policy_entry {
    __be16 proxy_port;                 // 프록시 리다이렉트 포트 (0=직접 통과)
    __u8   deny:1,                     // 0=허용, 1=거부
           reserved:2,
           lpm_prefix_length:5;        // LPM 접두사 길이
    __u8   auth_type:7,                // 인증 요구 사항 (SPIRE 등)
           has_explicit_auth_type:1;
    __u32  precedence;                 // 정책 우선순위 (높을수록 우선)
    __u32  cookie;                     // 정책 로그 쿠키
};
```

## Tail Call 메커니즘

### 왜 Tail Call이 필요한가?

eBPF 프로그램에는 **명령어 수 제한**(4096 → 100만 확장)과 **스택 크기 제한**(512바이트)이 있다.
Cilium의 데이터패스는 CT 조회, 정책 확인, LB, NAT 등 복잡한 로직을 처리해야 하므로
단일 프로그램으로는 불가능하다. **Tail Call**은 현재 프로그램의 스택 프레임을 재활용하면서
다른 BPF 프로그램으로 점프하는 메커니즘이다.

### Tail Call 맵과 인덱스

```c
// bpf/lib/tailcall.h
// BPF_MAP_TYPE_PROG_ARRAY 맵 — BPF 프로그램 포인터 배열
struct cilium_calls {
    __uint(type, BPF_MAP_TYPE_PROG_ARRAY);
    __uint(key_size, sizeof(__u32));
    __uint(max_entries, CILIUM_CALL_SIZE);  // 49개 슬롯
    __uint(pinning, CILIUM_PIN_REPLACE);
};
```

주요 Tail Call 인덱스 (총 49개):

| 인덱스 | 상수 | 대상 함수 | 용도 |
|--------|------|----------|------|
| 1 | `CILIUM_CALL_DROP_NOTIFY` | - | 드롭 알림 |
| 6 | `CILIUM_CALL_ARP` | `tail_handle_arp()` | ARP 응답 |
| 7 | `CILIUM_CALL_IPV4_FROM_LXC` | `tail_handle_ipv4()` | Pod 이그레스 IPv4 |
| 10 | `CILIUM_CALL_IPV6_FROM_LXC` | `tail_handle_ipv6()` | Pod 이그레스 IPv6 |
| 11 | `CILIUM_CALL_IPV4_TO_LXC_POLICY_ONLY` | `tail_ipv4_policy()` | 인그레스 정책 IPv4 |
| 12 | `CILIUM_CALL_IPV6_TO_LXC_POLICY_ONLY` | `tail_ipv6_policy()` | 인그레스 정책 IPv6 |
| 13 | `CILIUM_CALL_IPV4_TO_ENDPOINT` | - | 엔드포인트 배달 IPv4 |
| 14 | `CILIUM_CALL_IPV6_TO_ENDPOINT` | - | 엔드포인트 배달 IPv6 |
| 28 | `CILIUM_CALL_IPV4_CT_INGRESS` | - | CT 인그레스 조회 IPv4 |
| 30 | `CILIUM_CALL_IPV4_CT_EGRESS` | - | CT 이그레스 조회 IPv4 |

### Tail Call 선언과 호출

```c
// Tail Call 대상 함수 선언 (bpf_lxc.c:1046)
__declare_tail(CILIUM_CALL_IPV6_FROM_LXC)
int tail_handle_ipv6_cont(struct __ctx_buff *ctx)
{
    // IPv6 이그레스 처리 로직
    return handle_ipv6_from_lxc(ctx, &dst_sec_identity, &ext_err);
}

// Tail Call 호출 (bpf_lxc.c:1784)
tail_call_internal(ctx, CILIUM_CALL_IPV4_FROM_LXC, &ext_err);

// 호출 구현 (bpf/lib/tailcall.h:129-137)
static __always_inline int
tail_call_internal(struct __ctx_buff *ctx, const __u32 index, __s8 *ext_err)
{
    tail_call_static(ctx, cilium_calls, index);
    // Tail Call이 실패하면 여기에 도달
    if (ext_err)
        *ext_err = (__s8)index;
    return DROP_MISSED_TAIL_CALL;
}
```

### Tail Call 간 데이터 전달

Tail Call은 스택을 재활용하므로 로컬 변수를 다음 프로그램에 전달할 수 없다.
Cilium은 **per-CPU 배열 맵**을 버퍼로 사용한다:

```c
// CT 버퍼 맵 (per-CPU, 1개 엔트리)
struct cilium_tail_call_buffer4 {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, 1);
    __type(key, __u32);
    __type(value, struct ct_buffer4);
};

struct ct_buffer4 {
    struct ipv4_ct_tuple tuple;   // CT 5-튜플
    struct ct_state      ct_state; // CT 상태
    __u32                monitor;  // 모니터링 플래그
    int                  ret;      // CT 조회 결과
    int                  l4_off;   // L4 헤더 오프셋
};
```

또한 **skb->cb**(Control Block) 필드를 통해 메타데이터를 전달한다:

| CB 필드 | 용도 |
|---------|------|
| `CB_SRC_LABEL` | 소스 보안 Identity |
| `CB_FROM_HOST` | 호스트 네임스페이스 출처 |
| `CB_FROM_TUNNEL` | 터널에서 수신 |
| `CB_DELIVERY_REDIRECT` | 엔드포인트 리다이렉트 트리거 |
| `CB_PROXY_MAGIC` | L7 프록시 매직 넘버 |

## 패킷 처리 흐름: 이그레스 (Pod → 외부)

```
Pod가 패킷 전송
    │
    ▼
cil_from_container() [bpf_lxc.c:1750]
    │
    ├── validate_ethertype() — 이더넷 타입 검증
    │
    ├── IPv4이면: tail_call(CILIUM_CALL_IPV4_FROM_LXC)
    │   │
    │   ▼
    │   tail_handle_ipv4_cont() [bpf_lxc.c]
    │   │
    │   └── handle_ipv4_from_lxc() [bpf_lxc.c:1370]
    │       │
    │       ├── 1. 대상 IP로 cilium_ipcache 조회
    │       │   └── 대상 보안 Identity 획득
    │       │
    │       ├── 2. CT 조회: ct_lookup4()
    │       │   ├── CT_ESTABLISHED → 기존 연결, 정책 스킵
    │       │   ├── CT_REPLY → 응답 트래픽, 역방향 NAT 적용
    │       │   └── CT_NEW → 새 연결, 정책 확인 필요
    │       │
    │       ├── 3. [CT_NEW] policy_can_egress4()
    │       │   ├── cilium_policy_v2 맵에서 LPM 조회
    │       │   ├── (src_identity, proto, dport) → Allow/Deny
    │       │   └── proxy_port != 0 → L7 프록시 리다이렉트
    │       │
    │       ├── 4. [CT_NEW] ct_create4() — 새 CT 엔트리 생성
    │       │
    │       ├── 5. 라우팅 결정:
    │       │   ├── 로컬 엔드포인트 → redirect_ep()
    │       │   ├── 터널 필요 → encap_and_redirect()
    │       │   └── 직접 라우팅 → CTX_ACT_OK (스택으로)
    │       │
    │       └── 6. send_trace_notify() — 트레이스 이벤트 발생
    │
    ├── IPv6이면: tail_call(CILIUM_CALL_IPV6_FROM_LXC) [유사 경로]
    │
    └── ARP이면: tail_call(CILIUM_CALL_ARP)
        └── tail_handle_arp() → arp_respond()
```

## 패킷 처리 흐름: 인그레스 (외부 → Pod)

```
외부에서 패킷 수신 또는 정책 Tail Call
    │
    ▼
cil_to_container() [bpf_lxc.c:2561]
    │
    ├── IPv4이면: tail_call(CILIUM_CALL_IPV4_TO_LXC_POLICY_ONLY)
    │   │
    │   ▼
    │   tail_ipv4_policy() [bpf_lxc.c:2290]
    │   │
    │   └── ipv4_policy() [bpf_lxc.c:2118]
    │       │
    │       ├── 1. CT 버퍼에서 CT 상태 복원
    │       │
    │       ├── 2. SWITCH (CT 상태):
    │       │   │
    │       │   ├── CT_REPLY / CT_RELATED:
    │       │   │   ├── 정책 스킵 (이미 허용된 연결의 응답)
    │       │   │   ├── proxy_redirect → ctx_redirect_to_proxy4()
    │       │   │   └── rev_nat_index → lb4_rev_nat() (역방향 NAT)
    │       │   │
    │       │   └── CT_NEW / DEFAULT:
    │       │       ├── policy_can_ingress4()
    │       │       │   └── (remote_identity, proto, dport) 조회
    │       │       ├── ct_create4() — 새 CT 엔트리
    │       │       └── proxy_port > 0 → L7 프록시 리다이렉트
    │       │
    │       └── 3. 결과:
    │           ├── POLICY_ACT_PROXY_REDIRECT → 프록시로
    │           ├── CTX_ACT_OK → redirect_ep() 또는 반환
    │           └── DROP_POLICY_DENY → 드롭 + 알림
    │
    └── IPv6이면: 유사 경로
```

## 호스트 네트워크 처리 (bpf_host.c)

```
물리 NIC에서 수신
    │
    ▼
cil_from_netdev() [bpf_host.c:1178]
    │
    ├── VLAN 필터링 검증
    ├── IPsec 복호화 체크
    │
    └── do_netdev(ctx, proto, UNKNOWN_ID, FROM_NETWORK)
        │
        ├── IPv4: handle_ipv4_from_netdev()
        │   ├── NodePort LB: nodeport_lb4()
        │   ├── CT 조회 + 정책 확인
        │   └── 라우팅 결정
        │
        └── IPv6: handle_ipv6_from_netdev() [유사]


cilium_host에서 송신
    │
    ▼
cil_from_host() [bpf_host.c:1261]
    │
    ├── EDT 대역폭 제한 설정
    ├── 호스트 트래픽 Identity 상속
    └── do_netdev(ctx, proto, identity, FROM_HOST)


물리 NIC으로 송신
    │
    ▼
cil_to_netdev() [bpf_host.c:1317]
    │
    ├── Magic Mark 파싱 (호스트/오버레이/암호화 구분)
    ├── 호스트 방화벽 체크
    ├── Egress Gateway 처리
    └── 최종 라우팅/캡슐화
```

## ConnTrack (연결 추적) 상세

### CT 맵 구성

| 맵 이름 | 타입 | 용도 |
|---------|------|------|
| `cilium_ct4_global` | LRU_HASH | IPv4 TCP 연결 |
| `cilium_ct_any4_global` | LRU_HASH | IPv4 비-TCP (UDP, ICMP 등) |
| `cilium_ct6_global` | LRU_HASH | IPv6 TCP 연결 |
| `cilium_ct_any6_global` | LRU_HASH | IPv6 비-TCP |
| `cilium_per_cluster_ct_tcp4` | ARRAY_OF_MAPS | 클러스터별 IPv4 TCP |
| `cilium_per_cluster_ct_any4` | ARRAY_OF_MAPS | 클러스터별 IPv4 비-TCP |

**왜 TCP와 비-TCP를 분리하는가?**
TCP 연결은 상태 기반으로 오래 유지되고 크기가 크다. UDP/ICMP는 상대적으로 짧은 타임아웃을 가진다.
별도 맵으로 분리하면 각 특성에 맞는 크기와 LRU 정책을 적용할 수 있다.

### CT 조회 흐름 (`ct_lookup4()`)

```c
// bpf/lib/conntrack.h:1020
static __always_inline int
ct_lookup4(const void *map, struct ipv4_ct_tuple *tuple,
           struct __ctx_buff *ctx, int l4_off, int dir,
           struct ct_state *ct_state, __u32 *monitor)
{
    // 1. L4 포트 추출
    ipv4_ct_extract_l4_ports(ctx, l4_off, dir, tuple, NULL);

    // 2. 방향에 따라 튜플 반전 (SCOPE_FORWARD → reply 방향으로)
    if (scope == SCOPE_FORWARD)
        ipv4_ct_tuple_reverse(tuple);

    // 3. 맵 조회
    return __ct_lookup(map, tuple, action, dir, ct_state, ...);
}
```

### 핵심 CT 조회 (`__ct_lookup()`)

```c
// bpf/lib/conntrack.h:353
static __always_inline int
__ct_lookup(const void *map, struct __ctx_buff *ctx, const void *tuple,
            enum ct_action action, int dir, struct ct_state *ct_state, ...)
{
    struct ct_entry *entry = map_lookup_elem(map, tuple);

    if (entry) {
        // 1. 타임아웃 갱신
        *monitor = ct_update_timeout(entry, ...);

        // 2. 패킷/바이트 카운터 갱신 (atomic)
        __sync_fetch_and_add(&entry->packets, 1);
        __sync_fetch_and_add(&entry->bytes, ctx_full_len(ctx));

        // 3. 상태 복사
        ct_state->rev_nat_index = entry->rev_nat_index;
        ct_state->src_sec_id = entry->src_sec_id;
        ct_state->proxy_redirect = entry->proxy_redirect;
        // ...

        // 4. 방향에 따라 CT_ESTABLISHED 또는 CT_REPLY 반환
        if (dir == CT_INGRESS || dir == CT_EGRESS)
            return CT_ESTABLISHED;
        return CT_REPLY;
    }

    return CT_NEW;  // 새 연결
}
```

### CT 상태 반환값

| 값 | 의미 | 정책 필요? |
|----|------|-----------|
| `CT_NEW` (0) | 새 연결 | **예** — 정책 확인 후 CT 엔트리 생성 |
| `CT_ESTABLISHED` (1) | 기존 연결, 정상 트래픽 | 아니오 |
| `CT_RELATED` (2) | ICMP 에러 등 관련 트래픽 | 아니오 |
| `CT_REPLY` (3) | 응답 방향 트래픽 | 아니오 |

**왜 CT_ESTABLISHED는 정책을 스킵하는가?**
최초 연결(CT_NEW)에서 이미 정책을 검증했으므로, 이후 패킷은 CT 엔트리의 존재로 암묵적 허용이다.
이 설계는 패킷당 정책 조회 비용을 O(1) CT 조회로 대체하여 성능을 크게 향상시킨다.

### CT 타임아웃 관리

```c
// bpf/lib/conntrack.h:166-230
static __always_inline __u32
__ct_update_timeout(struct ct_entry *entry, __u32 lifetime,
                    enum ct_dir dir, ...)
{
    // 1. 현재 모노토닉 시간 획득
    __u32 now = bpf_mono_now();

    // 2. 타임아웃 갱신
    WRITE_ONCE(entry->lifetime, now + lifetime);

    // 3. TCP 플래그 누적 (OR 연산)
    if (dir == CT_TX)
        entry->tx_flags_seen |= tcp_flags;
    else
        entry->rx_flags_seen |= tcp_flags;

    // 4. 리포트 간격 체크 (Hubble 이벤트 발생 조건)
    if (now - last_report >= CT_REPORT_INTERVAL)
        return TRACE_PAYLOAD_LEN;  // 이벤트 발생

    return 0;
}
```

## 정책 적용 상세

### 정책 맵 구조

```
cilium_policy_v2 (BPF_MAP_TYPE_LPM_TRIE, 엔드포인트별)
    │
    ├── Key: (Identity, Direction, Protocol, DestPort) + LPM prefix
    │
    └── Value: (ProxyPort, Deny, AuthType, Precedence, Cookie)
```

### 정책 조회 알고리즘

```c
// bpf/lib/policy.h:218-348
int __policy_can_access(const void *map, struct __ctx_buff *ctx,
                        __u32 local_id, __u32 remote_id,
                        __be16 dport, __u8 proto, int dir, ...)
{
    // 1단계: L3+L4 조회 (Identity + Protocol + Port)
    struct policy_key key = {
        .sec_label = remote_id,
        .egress = (dir == CT_EGRESS),
        .protocol = proto,
        .dport = dport,
    };
    struct policy_entry *l3l4 = map_lookup_elem(map, &key);

    // 2단계: L4-only 조회 (와일드카드 Identity)
    key.sec_label = 0;  // Identity = 0은 "모든 Identity"
    struct policy_entry *l4only = map_lookup_elem(map, &key);

    // 3단계: 우선순위 비교
    // - 높은 precedence가 우선
    // - 같은 precedence면 긴 LPM 접두사가 우선
    // - 여전히 같으면 L3 정책(비-와일드카드)이 L4-only보다 우선
    // - deny가 allow보다 항상 우선 (같은 precedence일 때)

    // 4단계: 최종 판단
    return __policy_check(winner);
    // CTX_ACT_OK (허용)
    // DROP_POLICY_DENY (거부)
    // DROP_POLICY_AUTH_REQUIRED (인증 필요)
}
```

### 정책 매칭 타입

| 매칭 타입 | 코드 | 의미 |
|----------|------|------|
| `POLICY_MATCH_NONE` | 0 | 매칭 없음 |
| `POLICY_MATCH_L3_ONLY` | 1 | Identity만 매칭 |
| `POLICY_MATCH_L3_L4` | 2 | Identity + Protocol + Port |
| `POLICY_MATCH_L4_ONLY` | 3 | Protocol + Port (와일드카드 Identity) |
| `POLICY_MATCH_ALL` | 4 | 전체 와일드카드 |
| `POLICY_MATCH_L3_PROTO` | 5 | Identity + Protocol |
| `POLICY_MATCH_PROTO_ONLY` | 6 | Protocol만 매칭 |

### 정책 통계

```c
// bpf/lib/policy.h:109-117
struct cilium_policystats {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __type(key, struct policy_stats_key);   // endpoint_id, sec_label, proto, dport
    __type(value, struct policy_stats_value); // packets, bytes
};

// 정책 매칭 후 통계 업데이트
static __always_inline void
__policy_account(struct __ctx_buff *ctx, __u32 identity,
                 __u8 proto, __be16 dport)
{
    // LRU 맵에 패킷/바이트 카운터 증가
}
```

## BPF 맵 상호작용 종합

```
패킷 도착 → bpf_lxc.c 또는 bpf_host.c
    │
    ├──[1] cilium_ipcache (LPM Trie) 조회
    │      IP → remote_endpoint_info {sec_identity, tunnel_endpoint}
    │      → 소스/대상의 보안 Identity 확인
    │
    ├──[2] cilium_ct4_global (LRU Hash) 조회
    │      5-tuple → ct_entry {lifetime, flags, counters}
    │      ├── CT_ESTABLISHED → 정책 스킵, 빠른 경로
    │      └── CT_NEW → 정책 확인 필요
    │
    ├──[3] cilium_policy_v2_{EP_ID} (LPM Trie) 조회
    │      (Identity, Proto, Port, Dir) → policy_entry {allow/deny, proxy_port}
    │      ├── Allow → 통과
    │      ├── Deny → 드롭
    │      └── proxy_port != 0 → L7 프록시 리다이렉트
    │
    ├──[4] cilium_lb4_services_v2 (Hash) 조회 [서비스 트래픽]
    │      (VIP, Port, Proto) → lb4_service {backend_id, count, algo}
    │      └── cilium_lb4_backends_v3 → 백엔드 IP:Port
    │         └── DNAT 수행
    │
    ├──[5] cilium_lxc (Hash) 조회 [로컬 배달]
    │      IP → endpoint_info {ifindex, MAC, LXC_ID}
    │      └── redirect_ep() → veth/netkit으로 패킷 전송
    │
    └──[6] cilium_calls (Prog Array) — Tail Call
           인덱스 → 다음 BPF 프로그램으로 점프
```

## BPF 프로그램 컴파일 파이프라인

### Go 레벨 (Datapath Loader)

```go
// pkg/datapath/loader/loader.go:37
type loader struct {
    templateCache  *objectCache       // 컴파일된 오브젝트 캐시
    registry       *registry.MapRegistry // BPF 맵 레지스트리
    compilationLock datapath.CompilationLock
    configWriter   datapath.ConfigWriter
}

// pkg/datapath/loader/compile.go
const (
    compiler           = "clang"
    endpointProg       = "bpf_lxc.c"      // 엔드포인트 프로그램
    endpointObj        = "bpf_lxc.o"
    hostEndpointProg   = "bpf_host.c"
    hostEndpointObj    = "bpf_host.o"
)
```

### 컴파일 과정

```
1. 헤더 생성 (ep_config.h)
   ├── #define LXC_ID        <엔드포인트 ID>
   ├── #define SECLABEL      <보안 Identity>
   ├── #define SECLABEL_IPV4 <IPv4 Identity>
   ├── #define NODE_MAC      <노드 MAC 주소>
   └── #define ENABLE_*      <기능 플래그>

2. clang 컴파일
   └── clang --target=bpf -O2 -g -std=gnu99 \
       -I bpf/ -I bpf/include/ \
       -c bpf_lxc.c -o bpf_lxc.o

3. 오브젝트 캐시 확인 (templateCache)
   └── 같은 #define 조합 → 기존 오브젝트 재사용
   └── 다른 조합 → 새로 컴파일

4. 커널에 로드 (cilium/ebpf 라이브러리)
   └── ebpf.LoadCollectionSpec(bpf_lxc.o)
   └── 맵 핀닝 + 프로그램 어태치
```

### TC/TCX 어태치

```go
// pkg/datapath/loader/tc.go:27
func attachSKBProgram(device, prog, progName, bpffsDir, parent, tcxEnabled) {
    if tcxEnabled {
        if device.Type() == "netkit" {
            return upsertNetkitProgram(...)  // netkit 디바이스
        }
        err := upsertTCXProgram(...)         // TCX (Linux 6.6+)
        if err == nil {
            removeTCFilters(device, parent)  // 레거시 TC 정리
            return nil
        }
        if !errors.Is(err, link.ErrNotSupported) {
            return err                       // TCX 미지원이 아닌 오류
        }
    }
    return upsertTCFilter(...)               // 레거시 TC 폴백
}
```

**TCX vs TC vs netkit**:

| 어태치 방식 | 커널 버전 | 장점 |
|------------|----------|------|
| TC (레거시) | 4.x+ | 넓은 호환성 |
| TCX | 6.6+ | 원자적 교체, bpffs 핀닝, BPF 링크 기반 |
| netkit | 6.7+ | veth 대체, 최고 성능, netns 자동 연결 |

## Perf 이벤트 (모니터링/옵저버빌리티)

### 이벤트 맵

```c
// bpf/lib/events.h
struct cilium_events {
    __uint(type, BPF_MAP_TYPE_PERF_EVENT_ARRAY);
    __uint(pinning, LIBBPF_PIN_BY_NAME);
};
```

### 트레이스 알림 (`send_trace_notify()`)

패킷이 각 관찰 지점을 통과할 때 이벤트를 발생시켜 Hubble이 수집할 수 있게 한다.

```c
// bpf/lib/trace.h
send_trace_notify(ctx,
    obs_point,    // TRACE_TO_LXC, TRACE_FROM_NETWORK 등 (14개 관찰 지점)
    src,          // 소스 엔드포인트 ID
    dst,          // 대상 엔드포인트 ID
    dst_id,       // 대상 Identity 또는 프록시 포트
    ifindex,      // 네트워크 인터페이스 인덱스
    reason,       // 트레이스 이유 (정책, CT, 암호화 등)
    monitor       // 집계 레벨
);
```

### 드롭 알림 (`send_drop_notify()`)

패킷이 드롭될 때 이유와 함께 이벤트를 발생시킨다.

```c
// bpf/lib/drop.h
send_drop_notify(ctx,
    src,          // 소스
    dst,          // 대상
    dst_id,       // 대상 Identity
    reason,       // 드롭 사유 코드 (DROP_POLICY_DENY 등)
    exitcode,     // 반환 코드
    direction     // 인그레스/이그레스
);
```

### 관찰 지점 (Observation Points)

| 코드 | 관찰 지점 | 위치 |
|------|----------|------|
| 0 | `TRACE_TO_LXC` | 엔드포인트로 배달 |
| 1 | `TRACE_TO_PROXY` | L7 프록시로 리다이렉트 |
| 2 | `TRACE_TO_HOST` | 호스트 스택으로 전달 |
| 3 | `TRACE_TO_STACK` | 커널 네트워크 스택 |
| 4 | `TRACE_TO_OVERLAY` | 오버레이 터널로 캡슐화 |
| 5 | `TRACE_FROM_LXC` | 엔드포인트에서 수신 |
| 6 | `TRACE_FROM_PROXY` | 프록시에서 반환 |
| 7 | `TRACE_FROM_HOST` | 호스트에서 수신 |
| 8 | `TRACE_FROM_STACK` | 커널 스택에서 수신 |
| 9 | `TRACE_FROM_OVERLAY` | 오버레이에서 디캡슐화 |
| 10 | `TRACE_FROM_NETWORK` | 물리 네트워크에서 수신 |
| 11 | `TRACE_TO_NETWORK` | 물리 네트워크로 송신 |
| 12 | `TRACE_FROM_CRYPTO` | 복호화 후 |
| 13 | `TRACE_TO_CRYPTO` | 암호화 전 |

## 왜 이 아키텍처인가?

### 1. 왜 iptables를 대체하는가?

```
iptables 방식:
  패킷 → PREROUTING → INPUT/FORWARD → OUTPUT → POSTROUTING
  각 체인에서 선형 규칙 탐색 — O(N) where N = 규칙 수
  1000개 서비스 × 10개 엔드포인트 = 10,000+ 규칙

eBPF 방식:
  패킷 → BPF 프로그램 → BPF 맵 조회 — O(1) 해시 룩업
  서비스 수에 관계없이 일정한 조회 시간
```

### 2. 왜 프로그램별로 분리하는가?

- **관심사 분리**: 각 프로그램은 하나의 어태치 포인트(인터페이스)에 특화
- **독립 업데이트**: Pod veth의 BPF 프로그램만 교체해도 다른 프로그램에 영향 없음
- **엔드포인트별 설정**: `ep_config.h`의 `#define`으로 엔드포인트별 커스텀 컴파일
- **성능 최적화**: 각 위치에서 불필요한 로직 제거 (컴파일 타임 dead code elimination)

### 3. 왜 LRU Hash를 CT에 사용하는가?

- **자동 eviction**: 맵이 가득 차면 가장 오래된 엔트리 자동 제거
- **per-CPU 최적화**: LRU 맵은 CPU별 캐시로 잠금 경합 최소화
- **메모리 효율**: 고정 크기 맵으로 메모리 사용량 예측 가능
- TCP와 비-TCP 분리로 각 특성에 맞는 크기/타임아웃 적용

### 4. 왜 LPM Trie를 정책 맵에 사용하는가?

```
LPM (Longest Prefix Match) 활용 시나리오:

정책: "TCP/80-90 허용"
  → LPM 키: protocol=TCP, port_prefix=80/4bit (80-95 범위 매칭)

정책: "TCP/ANY 허용"
  → LPM 키: protocol=TCP, port_prefix=0/0bit (모든 포트)

더 긴 접두사(더 구체적인 규칙)가 우선 매칭됨
```

이는 포트 범위 정책을 단일 맵 엔트리로 효율적으로 표현할 수 있게 한다.

## 성능 특성

| 항목 | iptables | eBPF (Cilium) |
|------|----------|---------------|
| 서비스 조회 | O(N) 선형 | O(1) 해시 |
| 정책 조회 | O(N) 체인 | O(1) LPM Trie |
| CT 조회 | O(1) conntrack | O(1) LRU Hash |
| 프로그램 업데이트 | 전체 체인 재로드 | 개별 프로그램 원자적 교체 |
| 메트릭 수집 | 별도 도구 필요 | BPF 맵 내장 카운터 |
| L7 프록시 연동 | 복잡한 REDIRECT | BPF proxy_port 필드 |
| 멀티코어 확장성 | conntrack 잠금 경합 | per-CPU LRU 최적화 |

## 참고 파일 목록

| 파일 | 줄 수 | 핵심 내용 |
|------|-------|----------|
| `bpf/bpf_lxc.c` | 2,670 | Pod 이그레스/인그레스, 정책 적용 |
| `bpf/bpf_host.c` | 1,916 | 호스트 네트워크, NodePort |
| `bpf/bpf_overlay.c` | 663 | 터널 오버레이 디캡슐화 |
| `bpf/bpf_xdp.c` | 370 | XDP LB (최고 성능) |
| `bpf/bpf_sock.c` | 1,279 | 소켓 레벨 LB |
| `bpf/lib/conntrack.h` | ~1,200 | CT 조회/생성/타임아웃 |
| `bpf/lib/policy.h` | ~500 | 정책 조회/판단 |
| `bpf/lib/tailcall.h` | ~150 | Tail Call 인프라 |
| `bpf/lib/trace.h` | ~100 | 트레이스 이벤트 |
| `bpf/lib/common.h` | ~400 | 공통 타입/매크로 |
| `pkg/datapath/loader/loader.go` | - | Go 레벨 BPF 로더 |
| `pkg/datapath/loader/compile.go` | - | clang 컴파일 관리 |
| `pkg/datapath/loader/tc.go` | - | TC/TCX 어태치 |
