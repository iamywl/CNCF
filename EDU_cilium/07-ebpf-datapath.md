# 7. Cilium eBPF 데이터패스 심층 분석

---

## 개요

Cilium의 데이터패스는 리눅스 커널 내 eBPF 프로그램으로 구현된다. 모든 패킷 처리 -- 정책 적용, NAT, 로드밸런싱, 연결 추적 -- 가 커널 공간에서 수행되므로 유저스페이스 왕복 없이 고성능 네트워킹을 달성한다. 이 문서에서는 eBPF 프로그램 유형, BPF 맵, tail call 체인, 커널 기능 활용, cilium/ebpf 라이브러리 사용 패턴, 그리고 BPF 코드 컴파일 파이프라인을 심층적으로 다룬다.

---

## 1. eBPF 프로그램 유형

Cilium은 패킷이 커널 네트워크 스택의 어느 지점을 통과하느냐에 따라 서로 다른 eBPF 프로그램 유형을 사용한다.

### 1.1 XDP (eXpress Data Path)

| 항목 | 설명 |
|------|------|
| **부착 위치** | NIC 드라이버 레벨 (가장 빠른 지점) |
| **소스 파일** | `bpf/bpf_xdp.c` |
| **진입점 함수** | `cil_xdp_entry` |
| **반환값** | `XDP_PASS`, `XDP_DROP`, `XDP_TX`, `XDP_REDIRECT` |
| **주요 용도** | DDoS 방어, NodePort 가속, 조기 패킷 드롭 |

XDP는 패킷이 커널 네트워크 스택에 진입하기 전에 실행된다. `skb` 할당 전이므로 메모리 오버헤드가 최소화된다.

```c
// bpf/bpf_xdp.c
__declare_tail(CILIUM_CALL_IPV4_FROM_NETDEV)
int tail_lb_ipv4(struct __ctx_buff *ctx)
{
    bool punt_to_stack = false;
    // NodePort LB 처리...
}
```

XDP에서도 LPM Trie 맵을 사용하여 CIDR 기반 필터링을 수행한다:

```c
// bpf/bpf_xdp.c
struct {
    __uint(type, BPF_MAP_TYPE_LPM_TRIE);
    __type(key, struct lpm_v4_key);
    __type(value, struct lpm_val);
    __uint(max_entries, CIDR4_LMAP_ELEMS);
} cilium_cidr_v4_dyn __section_maps_btf;
```

### 1.2 TC (Traffic Control) ingress/egress

| 항목 | 설명 |
|------|------|
| **부착 위치** | 네트워크 디바이스의 TC 훅 (ingress/egress) |
| **소스 파일** | `bpf/bpf_lxc.c`, `bpf/bpf_host.c`, `bpf/bpf_overlay.c` |
| **반환값** | `TC_ACT_OK`, `TC_ACT_SHOT`, `TC_ACT_REDIRECT` |
| **주요 용도** | 핵심 패킷 처리의 대부분 |

TC는 Cilium 데이터패스의 중추이다. 패킷의 방향(ingress/egress)과 인터페이스에 따라 다른 프로그램이 부착된다:

| 프로그램 | 파일 | 인터페이스 | 방향 |
|----------|------|-----------|------|
| `cil_from_container` | `bpf_lxc.c` | Pod veth | TC ingress |
| `cil_to_container` | `bpf_lxc.c` | Pod veth | TC egress |
| `cil_from_host` | `bpf_host.c` | cilium_host | TC ingress |
| `cil_to_host` | `bpf_host.c` | cilium_host | TC egress |
| `cil_from_netdev` | `bpf_host.c` | 물리 NIC | TC ingress |
| `cil_to_netdev` | `bpf_host.c` | 물리 NIC | TC egress |
| `cil_from_overlay` | `bpf_overlay.c` | cilium_vxlan/geneve | TC ingress |
| `cil_to_overlay` | `bpf_overlay.c` | cilium_vxlan/geneve | TC egress |

```c
// bpf/bpf_lxc.c - Pod에서 나가는 패킷의 진입점
__section_entry
int cil_from_container(struct __ctx_buff *ctx)
{
    __be16 proto = 0;
    __u32 sec_label = SECLABEL;

    switch (proto) {
    case bpf_htons(ETH_P_IP):
        // IPv4 처리를 위한 tail call
        ret = tail_call_internal(ctx, CILIUM_CALL_IPV4_FROM_LXC, &ext_err);
        break;
    case bpf_htons(ETH_P_IPV6):
        ret = tail_call_internal(ctx, CILIUM_CALL_IPV6_FROM_LXC, &ext_err);
        break;
    case bpf_htons(ETH_P_ARP):
        ret = tail_call_internal(ctx, CILIUM_CALL_ARP, &ext_err);
        break;
    }
}
```

### 1.3 Socket/cgroup 프로그램

| 항목 | 설명 |
|------|------|
| **부착 위치** | cgroup에 부착, 소켓 시스템콜 훅 |
| **소스 파일** | `bpf/bpf_sock.c` |
| **주요 용도** | 소켓 레벨 로드밸런싱 (connect/sendmsg 인터셉트) |

소켓 레벨에서 서비스 DNAT를 수행하면 패킷 단위 NAT가 불필요해진다:

| 프로그램 | 훅 | 역할 |
|----------|-----|------|
| `cil_sock4_connect` | `cgroup/connect4` | IPv4 `connect()` 시 ClusterIP를 Backend IP로 변환 |
| `cil_sock6_connect` | `cgroup/connect6` | IPv6 `connect()` 시 서비스 DNAT |
| `cil_sock4_sendmsg` | `cgroup/sendmsg4` | UDP `sendmsg()` 시 서비스 DNAT |
| `cil_sock4_recvmsg` | `cgroup/recvmsg4` | `recvmsg()` 시 역방향 NAT |
| `cil_sock4_getpeername` | `cgroup/getpeername4` | `getpeername()` 시 원본 주소 반환 |

```c
// bpf/bpf_sock.c
static __always_inline __maybe_unused bool is_v4_loopback(__be32 daddr)
{
    return (daddr & bpf_htonl(0xff000000)) == bpf_htonl(0x7f000000);
}
```

소켓 LB의 장점:
- **한 번만 변환**: `connect()` 시점에 목적지를 변환하면 이후 패킷은 직접 Backend로 전송
- **커널 스택 최적화**: 패킷마다 NAT 테이블을 조회할 필요 없음
- **투명한 동작**: 애플리케이션은 서비스 IP로 연결했다고 인식

---

## 2. BPF 맵 유형

BPF 맵은 커널 BPF 프로그램과 유저스페이스가 공유하는 데이터 구조이다. Cilium은 용도에 따라 다양한 맵 유형을 사용한다.

### 2.1 Hash Map

```
키 → 해시 → 버킷 → 값 (O(1) 조회)
```

| 용도 | 맵 이름 | 소스 |
|------|---------|------|
| 서비스 Frontend | `cilium_lb4_services_v2` | `bpf/lib/lb.h` |
| 서비스 Backend | `cilium_lb4_backends_v3` | `bpf/lib/lb.h` |
| RevNAT | `cilium_lb4_reverse_nat` | `bpf/lib/lb.h` |
| Endpoint 정보 | `cilium_lxc` | `bpf/lib/eps.h` |

```c
// bpf/lib/lb.h
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __type(key, struct lb4_key);
    __type(value, struct lb4_service);
    __uint(pinning, LIBBPF_PIN_BY_NAME);
    __uint(max_entries, CILIUM_LB_SERVICE_MAP_MAX_ENTRIES);
} cilium_lb4_services_v2 __section_maps_btf;
```

### 2.2 LRU Hash Map

가장 오래 사용되지 않은 엔트리를 자동으로 제거한다. Connection Tracking과 NAT에 이상적이다.

| 용도 | 맵 이름 | 기본 크기 | 소스 |
|------|---------|-----------|------|
| CT (TCP, IPv4) | `cilium_ct4_global` | 524,288 | `bpf/lib/conntrack_map.h` |
| CT (Any, IPv4) | `cilium_ct_any4_global` | 262,144 | `bpf/lib/conntrack_map.h` |
| NAT (IPv4) | `cilium_snat_v4_external` | - | `bpf/lib/nat.h` |
| LB Health | `cilium_lb4_health` | - | `bpf/lib/lb.h` |

```c
// bpf/lib/conntrack_map.h
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __type(key, struct ipv4_ct_tuple);   // 5-tuple (src/dst IP, port, proto)
    __type(value, struct ct_entry);       // 연결 상태, 패킷/바이트 카운터
    __uint(max_entries, CT_MAP_SIZE_TCP);
    __uint(map_flags, LRU_MEM_FLAVOR);
} cilium_ct4_global __section_maps_btf;
```

CT 엔트리의 구조(`bpf/lib/conntrack.h`):

```c
struct ct_entry {
    __u64 packets;
    __u64 bytes;
    __u32 lifetime;
    __u16 rx_closing:1,
          tx_closing:1,
          lb_loopback:1,
          seen_non_syn:1,
          node_port:1,
          proxy_redirect:1,
          dsr_internal:1,
          from_l7lb:1,
          from_tunnel:1;
    __u16 rev_nat_index;
    __be16 nat_port;
    __u8  tx_flags_seen;
    __u8  rx_flags_seen;
    __u32 src_sec_id;
};
```

### 2.3 Array Map

고정 크기 배열. 인덱스로 O(1) 접근. 주로 설정값과 통계에 사용된다.

| 용도 | 맵 이름 | 소스 |
|------|---------|------|
| Tail call 프로그램 배열 | `cilium_calls` | `bpf/lib/tailcall.h` |
| CT 임시 버퍼 | `cilium_tail_call_buffer4` | `bpf/bpf_host.c` |
| 메트릭스 | `cilium_metrics` | `bpf/lib/metrics.h` |

```c
// bpf/lib/tailcall.h
struct {
    __uint(type, BPF_MAP_TYPE_PROG_ARRAY);   // Array의 특수한 형태
    __uint(key_size, sizeof(__u32));
    __uint(max_entries, CILIUM_CALL_SIZE);    // 49개 슬롯
    __uint(pinning, CILIUM_PIN_REPLACE);
} cilium_calls __section_maps_btf;
```

```c
// bpf/bpf_host.c - Per-CPU Array로 tail call 간 데이터 전달
struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __type(key, __u32);
    __type(value, struct ct_buffer4);
    __uint(max_entries, 1);
} cilium_tail_call_buffer4 __section_maps_btf;
```

### 2.4 LPM Trie (Longest Prefix Match)

CIDR 기반 매칭에 사용된다. 가장 긴 프리픽스가 우선한다.

| 용도 | 맵 이름 | 소스 |
|------|---------|------|
| 정책 (Identity+Port) | `cilium_policy_v2` | `bpf/lib/policy.h` |
| CIDR 필터링 | `cilium_cidr_v4_dyn` | `bpf/bpf_xdp.c` |
| IPCache (IP->Identity) | `cilium_ipcache` | `bpf/lib/eps.h` |
| Egress Gateway | `cilium_egress_gw_policy_v4` | `bpf/lib/egress_gateway.h` |
| SRv6 정책 | `cilium_srv6_policy_v4` | `bpf/lib/srv6.h` |

```c
// bpf/lib/policy.h - 정책 맵 (LPM Trie)
struct policy_key {
    struct bpf_lpm_trie_key lpm_key;
    __u32 sec_label;         // Security Identity
    __u8  egress:1, pad:7;   // 방향
    __u8  protocol;          // 프로토콜 (와일드카드 가능)
    __be16 dport;            // 포트 (부분 와일드카드 가능)
};

struct {
    __uint(type, BPF_MAP_TYPE_LPM_TRIE);
    __type(key, struct policy_key);
    __type(value, struct policy_entry);
    __uint(max_entries, POLICY_MAP_SIZE);
} cilium_policy_v2 __section_maps_btf;
```

LPM을 사용하면 프로토콜과 포트를 계층적으로 와일드카드 처리할 수 있다:
- 전체 매칭: Identity + Protocol + Port
- 프로토콜만: Identity + Protocol (Port 와일드카드)
- Identity만: Identity (Protocol + Port 와일드카드)

### 2.5 Perf Event Array / Ring Buffer

이벤트 스트리밍을 위한 맵. 패킷 추적, 드롭 알림, 정책 판정 로그 등을 유저스페이스로 전달한다.

```c
// bpf/lib/events.h
struct {
    __uint(type, BPF_MAP_TYPE_PERF_EVENT_ARRAY);
    __uint(key_size, sizeof(__u32));
    __uint(value_size, sizeof(__u32));
    __uint(pinning, LIBBPF_PIN_BY_NAME);
} cilium_events __section_maps_btf;
```

Hubble은 이 이벤트 맵을 통해 실시간 네트워크 가시성을 제공한다.

### 2.6 Hash of Maps / Array of Maps

맵의 맵. 동적으로 내부 맵을 교체할 수 있다.

| 용도 | 맵 이름 | 내부 맵 유형 | 소스 |
|------|---------|-------------|------|
| Maglev LUT | `cilium_lb4_maglev` | Array | `bpf/lib/lb.h` |
| Per-Cluster CT | `cilium_per_cluster_ct_tcp4` | LRU Hash | `bpf/lib/conntrack_map.h` |
| 멀티캐스트 구독 | `cilium_mcast_subscribers` | Hash | `bpf/lib/mcast.h` |

```c
// bpf/lib/lb.h - Maglev 일관성 해싱 테이블
struct {
    __uint(type, BPF_MAP_TYPE_HASH_OF_MAPS);
    __type(key, __u16);          // RevNAT ID
    __type(value, __u32);        // 내부 맵 FD
    __uint(max_entries, CILIUM_LB_MAGLEV_MAP_MAX_ENTRIES);
    __array(values, struct {
        __uint(type, BPF_MAP_TYPE_ARRAY);
        __uint(key_size, sizeof(__u32));
        __uint(value_size, sizeof(__u32) * LB_MAGLEV_LUT_SIZE);
        __uint(max_entries, 1);
    });
} cilium_lb4_maglev __section_maps_btf;
```

```c
// bpf/lib/conntrack_map.h - 클러스터별 CT 맵
struct {
    __uint(type, BPF_MAP_TYPE_ARRAY_OF_MAPS);
    __type(key, __u32);          // Cluster ID
    __type(value, __u32);        // 내부 맵 FD
    __uint(max_entries, 256);
    __array(values, struct {
        __uint(type, BPF_MAP_TYPE_LRU_HASH);
        __type(key, struct ipv4_ct_tuple);
        __type(value, struct ct_entry);
        __uint(max_entries, CT_MAP_SIZE_TCP);
    });
} cilium_per_cluster_ct_tcp4 __section_maps_btf;
```

---

## 3. Tail Call 체인

### 3.1 Tail Call이 필요한 이유

BPF 검증기(verifier)는 프로그램의 명령어 수를 제한한다 (최대 100만 명령어, 커널 5.2+). Cilium의 데이터패스 로직은 이 한도를 초과하므로 여러 프로그램으로 분리하고 tail call로 연결한다.

Tail call의 특성:
- **같은 스택 프레임** 재사용 (함수 호출이 아닌 점프)
- **반환 불가**: tail call 후에는 호출자로 돌아오지 않음
- **최대 33회** 중첩 가능
- `BPF_MAP_TYPE_PROG_ARRAY` 맵에서 인덱스로 대상 프로그램 선택

### 3.2 Cilium의 Tail Call 인덱스

`bpf/lib/tailcall.h`에 정의된 주요 tail call 인덱스:

```c
#define CILIUM_CALL_DROP_NOTIFY          1
#define CILIUM_CALL_ERROR_NOTIFY         2
#define CILIUM_CALL_HANDLE_ICMP6_NS      4
#define CILIUM_CALL_ARP                  6
#define CILIUM_CALL_IPV4_FROM_LXC        7   // IPv4 egress 처리 시작
#define CILIUM_CALL_IPV6_FROM_LXC        10  // IPv6 egress 처리 시작
#define CILIUM_CALL_IPV4_TO_LXC_POLICY_ONLY  11  // 정책 전용 검사
#define CILIUM_CALL_IPV4_TO_ENDPOINT     13  // Endpoint로 전달
#define CILIUM_CALL_IPV4_NODEPORT_NAT_EGRESS 15  // NodePort NAT
#define CILIUM_CALL_IPV4_NODEPORT_REVNAT 17  // NodePort 역방향 NAT
#define CILIUM_CALL_IPV4_FROM_LXC_CONT   26  // LXC egress 계속
#define CILIUM_CALL_IPV4_CT_EGRESS       30  // CT egress 조회
#define CILIUM_CALL_IPV4_CT_INGRESS      28  // CT ingress 조회
#define CILIUM_CALL_SIZE                 49  // 전체 슬롯 수
```

### 3.3 Egress Tail Call 체인 (Pod -> 외부)

Pod에서 나가는 IPv4 패킷의 처리 흐름:

```
cil_from_container (진입점)
    │
    ├─ tail_call → CILIUM_CALL_IPV4_FROM_LXC
    │   │
    │   ├─ 패킷 유효성 검사 (src IP, fragments)
    │   ├─ Per-packet LB 수행 (ENABLE_PER_PACKET_LB인 경우)
    │   │   └─ lb4_lookup_service() → lb4_local() → lb4_dnat_request()
    │   │
    │   └─ tail_call → CILIUM_CALL_IPV4_CT_EGRESS
    │       │
    │       ├─ CT 조회 (ct_lookup4)
    │       ├─ CT 엔트리 생성/업데이트
    │       │
    │       └─ tail_call → CILIUM_CALL_IPV4_FROM_LXC_CONT
    │           │
    │           ├─ handle_ipv4_from_lxc()
    │           ├─ 정책 검사 (policy_can_egress4)
    │           ├─ NAT (SNAT/masquerade)
    │           ├─ 터널 캡슐화 (필요한 경우)
    │           └─ FIB lookup → redirect
```

실제 코드 흐름:

```c
// 1단계: bpf/bpf_lxc.c - 진입점
__section_entry
int cil_from_container(struct __ctx_buff *ctx) {
    // 프로토콜에 따라 tail call
    ret = tail_call_internal(ctx, CILIUM_CALL_IPV4_FROM_LXC, &ext_err);
}

// 2단계: bpf/bpf_lxc.c - IPv4 처리
__declare_tail(CILIUM_CALL_IPV4_FROM_LXC)
int tail_handle_ipv4(struct __ctx_buff *ctx) {
    // Per-packet LB인 경우:
    return __per_packet_lb_svc_xlate_4(ctx, ip4, ext_err);
    // LB 처리 후 CT egress로 tail call
    // → tail_call_internal(ctx, CILIUM_CALL_IPV4_CT_EGRESS, ...)
}

// 3단계: CT 조회 (TAIL_CT_LOOKUP4 매크로로 생성)
TAIL_CT_LOOKUP4(CILIUM_CALL_IPV4_CT_EGRESS, tail_ipv4_ct_egress,
    CT_EGRESS, ..., CILIUM_CALL_IPV4_FROM_LXC_CONT, tail_handle_ipv4_cont)

// 4단계: 핵심 처리 계속
__declare_tail(CILIUM_CALL_IPV4_FROM_LXC_CONT)
int tail_handle_ipv4_cont(struct __ctx_buff *ctx) {
    return handle_ipv4_from_lxc(ctx, &dst_sec_identity, &ext_err);
    // → 정책 검사, NAT, 라우팅 결정, redirect
}
```

### 3.4 Ingress Tail Call 체인 (외부 -> Pod)

```
cil_to_container (진입점) 또는 다른 프로그램에서 redirect
    │
    ├─ tail_call → CILIUM_CALL_IPV4_CT_INGRESS
    │   │
    │   ├─ CT 조회 (ingress 방향)
    │   │
    │   └─ tail_call → CILIUM_CALL_IPV4_TO_ENDPOINT
    │       │
    │       ├─ Identity → 정책 맵 조회
    │       ├─ 정책 적용 (ALLOW/DENY)
    │       ├─ 프록시 리다이렉트 (필요한 경우)
    │       └─ 패킷 전달 또는 드롭
```

### 3.5 정책 전용 Tail Call (cilium_call_policy)

각 Endpoint마다 전용 정책 프로그램이 `BPF_MAP_TYPE_PROG_ARRAY`에 등록된다:

```c
// bpf/lib/local_delivery.h
struct {
    __uint(type, BPF_MAP_TYPE_PROG_ARRAY);
    __type(key, __u32);
    __type(value, __u32);
    __uint(pinning, LIBBPF_PIN_BY_NAME);
    __uint(max_entries, POLICY_PROG_MAP_SIZE);
} cilium_call_policy __section_maps_btf;

static __always_inline __must_check int
tail_call_policy(struct __ctx_buff *ctx, __u16 endpoint_id)
{
    tail_call_dynamic(ctx, &cilium_call_policy, endpoint_id);
    return DROP_EP_NOT_READY;
}
```

이 구조를 통해 Endpoint마다 독립적인 정책을 적용할 수 있다. Endpoint가 생성/삭제될 때 해당 슬롯만 업데이트하면 된다.

### 3.6 Tail Call 간 상태 전달

tail call은 같은 `ctx`(skb)를 공유하지만 스택 변수는 재초기화된다. 상태 전달을 위해 두 가지 메커니즘을 사용한다:

**방법 1: ctx metadata (CB_ 슬롯)**
```c
// 상태 저장
ctx_store_meta(ctx, CB_CT_STATE, (__u32)state->rev_nat_index << 16 | state->loopback);
ctx_store_meta(ctx, CB_PROXY_MAGIC, (__u32)proxy_port << 16);

// 상태 복원 (다음 tail call에서)
__u32 meta = ctx_load_and_clear_meta(ctx, CB_CT_STATE);
state->rev_nat_index = meta >> 16;
state->loopback = meta & 1;
```

**방법 2: Per-CPU Array 맵**
```c
// bpf/bpf_host.c
struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __type(key, __u32);
    __type(value, struct ct_buffer4);  // CT tuple + state + monitor
    __uint(max_entries, 1);
} cilium_tail_call_buffer4 __section_maps_btf;
```

---

## 4. 커널 기능 활용

### 4.1 Conntrack (연결 추적)

Cilium은 커널의 netfilter conntrack을 사용하지 않고 **자체 BPF 기반 CT**를 구현한다.

소스 위치:
- BPF 측: `bpf/lib/conntrack.h`, `bpf/lib/conntrack_map.h`
- Go 측: `pkg/maps/ctmap/`

CT 엔트리 생명주기:
1. **SYN 수신**: `ACTION_CREATE` -> 새 엔트리 생성, `SYN` 플래그 기록
2. **데이터 전송**: `ACTION_UNSPEC` -> 패킷/바이트 카운터 업데이트, lifetime 갱신
3. **FIN/RST 수신**: `ACTION_CLOSE` -> `rx_closing` 또는 `tx_closing` 설정
4. **만료**: GC(유저스페이스)가 주기적으로 만료된 엔트리 삭제

```c
// bpf/lib/conntrack.h
static __always_inline enum ct_action ct_tcp_select_action(union tcp_flags flags)
{
    if (unlikely(flags.value & (TCP_FLAG_RST | TCP_FLAG_FIN)))
        return ACTION_CLOSE;
    if (unlikely((flags.value & TCP_FLAG_SYN) && !(flags.value & TCP_FLAG_ACK)))
        return ACTION_CREATE;
    return ACTION_UNSPEC;
}
```

### 4.2 FIB Lookup

커널의 FIB(Forwarding Information Base)를 BPF 헬퍼 함수로 조회하여 라우팅 결정을 수행한다.

소스: `bpf/lib/fib.h`

```c
// 커널 FIB 조회 → DMAC/SMAC 자동 해석 → redirect
static __always_inline int
fib_do_redirect(struct __ctx_buff *ctx, bool needs_l2_check,
                struct bpf_fib_lookup *fib_params, ...)
{
    // bpf_fib_lookup() 헬퍼로 next-hop 조회
    // bpf_redirect_neigh()로 이웃 해석 포함 리다이렉트
    // 또는 수동으로 DMAC 설정 후 ctx_redirect()
}
```

### 4.3 Netlink

유저스페이스에서 BPF 프로그램을 네트워크 디바이스에 부착하기 위해 netlink을 사용한다.

소스: `pkg/datapath/loader/netlink.go`, `pkg/datapath/loader/tcx.go`

```go
// pkg/datapath/loader/tcx.go
// TCX (TC + Express) 또는 기존 TC로 프로그램 부착
func attachTCProgram(link netlink.Link, prog *ebpf.Program,
    progName, bpffsDir string, direction string) error {
    // netlink를 통해 TC qdisc 생성 및 필터 추가
}
```

### 4.4 XFRM (IPsec)

IPsec 암호화/복호화를 위해 커널 XFRM 프레임워크를 활용한다.

소스: `bpf/lib/ipsec.h`, `bpf/lib/encrypt.h`

BPF 프로그램에서 `skb->mark`에 암호화 관련 메타데이터를 설정하면 커널의 XFRM 계층이 실제 암호화를 처리한다.

---

## 5. cilium/ebpf 라이브러리 사용 패턴

Cilium은 `github.com/cilium/ebpf` 라이브러리를 사용하여 BPF 프로그램과 맵을 관리한다.

### 5.1 ELF 로딩 및 Collection 관리

```go
// pkg/datapath/loader/overlay.go
spec, err := ebpf.LoadCollectionSpec(overlayObj)  // ELF 파일 파싱
// spec에서 맵/프로그램 메타데이터 추출

// pkg/datapath/loader/loader_test.go
coll, commit, err := bpf.LoadCollection(logger, spec, &bpf.CollectionOptions{
    // 맵 핀닝, 프로그램 로딩 옵션
})
defer commit()  // 맵 핀 커밋
```

### 5.2 맵 핀닝 (Pinning)

BPF 맵은 `/sys/fs/bpf/` 아래에 핀(pin)되어 프로세스 간 공유된다:

```
/sys/fs/bpf/tc/globals/cilium_ct4_global      (CT 맵)
/sys/fs/bpf/tc/globals/cilium_lb4_services_v2  (서비스 맵)
/sys/fs/bpf/tc/globals/cilium_policy_v2        (정책 맵)
/sys/fs/bpf/tc/globals/cilium_ipcache          (IPCache 맵)
```

핀닝 모드:
- `LIBBPF_PIN_BY_NAME`: 맵 이름으로 핀 (전역 공유)
- `CILIUM_PIN_REPLACE`: 프로그램 로드 시 기존 핀 교체 (Endpoint별 tail call 맵)

### 5.3 프로그램 부착 흐름

```
1. ebpf.LoadCollectionSpec(objPath)   // ELF → CollectionSpec
2. 맵 이름 리매핑 (Endpoint별 맵 이름 변환)
3. bpf.LoadCollection(spec, opts)     // 커널에 프로그램/맵 로드
4. TCX/TC/XDP로 프로그램 부착         // netlink
5. 맵 핀 커밋                         // bpffs에 핀 생성
```

---

## 6. BPF 코드 컴파일 과정

### 6.1 파이프라인 개요

```
┌──────────┐     ┌──────────┐     ┌──────────────┐     ┌──────────┐
│ C 소스   │────>│ Clang    │────>│ ELF 오브젝트 │────>│ 커널     │
│ bpf_lxc.c│     │ -O2 -g   │     │ bpf_lxc.o    │     │ BPF 로더 │
│ + headers│     │--target= │     │ (BTF 포함)   │     │          │
│          │     │  bpf     │     │              │     │          │
└──────────┘     └──────────┘     └──────────────┘     └──────────┘
       │                                                      │
       │  #include "lib/..."                                  │
       │  #define ENABLE_...                                  │
       └── 노드/Endpoint별 설정 헤더 ──────────────────────────┘
```

### 6.2 컴파일러 설정

소스: `pkg/datapath/loader/compile.go`

```go
var StandardCFlags = []string{
    "-O2",                              // 최적화 레벨 2
    "--target=bpf",                     // BPF 타겟
    "-std=gnu99",                       // GNU C99 표준
    "-nostdinc",                        // 시스템 헤더 제외
    "-Wall", "-Wextra", "-Werror",      // 엄격한 경고
    "-Wno-address-of-packed-member",    // packed 구조체 허용
    "-g",                               // BTF 디버그 정보
}
```

BPF CPU 아키텍처 선택:
```go
// BPF ISA 버전에 따라 CPU 선택
func getBPFCPU(logger *slog.Logger) string {
    // v3: 커널 5.x+ (32비트 점프, atomics)
    // v2: 커널 4.14+ (JMP32)
    // v1: 기본 폴백
}
```

### 6.3 컴파일 단위

| 오브젝트 | 소스 | 용도 |
|----------|------|------|
| `bpf_lxc.o` | `bpf_lxc.c` | Endpoint (Pod) 데이터패스 |
| `bpf_host.o` | `bpf_host.c` | 호스트/네트워크 디바이스 |
| `bpf_overlay.o` | `bpf_overlay.c` | 터널 디바이스 |
| `bpf_xdp.o` | `bpf_xdp.c` | XDP 가속 |
| `bpf_network.o` | `bpf_network.c` | 네트워크 디바이스 |
| `bpf_wireguard.o` | `bpf_wireguard.c` | WireGuard 인터페이스 |

### 6.4 템플릿 캐시

Cilium은 동일한 설정을 가진 Endpoint들의 컴파일 결과를 캐시하여 재컴파일을 방지한다.

소스: `pkg/datapath/loader/cache.go`

```go
// loader 구조체의 templateCache
type loader struct {
    templateCache *objectCache  // 컴파일된 BPF 오브젝트 캐시
}
```

캐시 키는 Endpoint 설정의 해시값이다. 동일한 설정을 가진 Endpoint들은 같은 컴파일 결과를 공유한다.

### 6.5 노드/Endpoint별 설정 주입

컴파일 시 C 전처리기 매크로로 설정을 주입한다:

```
node_config.h    — 노드 전체 설정 (IP, MAC, 터널 모드 등)
endpoint_config.h — Endpoint별 설정 (LXC_ID, SECLABEL 등)
```

예시:
```c
#define LXC_ID 1234                        // Endpoint ID
#define SECLABEL 48312                     // Security Identity
#define ENABLE_IPV4 1                      // IPv4 활성화
#define ENABLE_NODEPORT 1                  // NodePort 활성화
#define CT_MAP_SIZE_TCP 524288             // CT 맵 크기
```

---

## 7. 실제 소스 파일 위치 참조

### BPF C 소스

| 파일 | 경로 | 역할 |
|------|------|------|
| 핵심 프로그램 | `bpf/bpf_lxc.c` | Pod 데이터패스 (가장 복잡) |
| 호스트 프로그램 | `bpf/bpf_host.c` | 호스트 네트워킹 |
| 오버레이 | `bpf/bpf_overlay.c` | VXLAN/Geneve 터널 |
| XDP | `bpf/bpf_xdp.c` | XDP 가속 |
| 소켓 LB | `bpf/bpf_sock.c` | cgroup 소켓 훅 |
| WireGuard | `bpf/bpf_wireguard.c` | WireGuard 통합 |

### BPF 라이브러리 헤더

| 헤더 | 경로 | 역할 |
|------|------|------|
| Conntrack | `bpf/lib/conntrack.h` | 연결 추적 로직 |
| CT 맵 정의 | `bpf/lib/conntrack_map.h` | CT 맵 선언 |
| NAT | `bpf/lib/nat.h` | SNAT/DNAT 구현 |
| LB | `bpf/lib/lb.h` | 로드밸런싱 핵심 |
| NodePort | `bpf/lib/nodeport.h` | NodePort/DSR/Maglev |
| Policy | `bpf/lib/policy.h` | 정책 조회/적용 |
| Tail Call | `bpf/lib/tailcall.h` | Tail call 인프라 |
| Local Delivery | `bpf/lib/local_delivery.h` | Endpoint 전달 및 정책 tail call |
| FIB | `bpf/lib/fib.h` | 커널 FIB 조회 |
| Encap | `bpf/lib/encap.h` | 터널 캡슐화 |
| Identity | `bpf/lib/identity.h` | Security Identity 조회 |
| Trace | `bpf/lib/trace.h` | 패킷 추적 |

### Go 유저스페이스

| 패키지 | 경로 | 역할 |
|--------|------|------|
| 로더 | `pkg/datapath/loader/` | BPF 프로그램 컴파일/로드 |
| 컴파일러 | `pkg/datapath/loader/compile.go` | Clang 호출 |
| Endpoint 로딩 | `pkg/datapath/loader/endpoint.go` | Endpoint BPF 설정 |
| TC 부착 | `pkg/datapath/loader/tcx.go` | TC/TCX 프로그램 부착 |
| CT 맵 | `pkg/maps/ctmap/` | CT 맵 유저스페이스 관리 |
| NAT 맵 | `pkg/maps/nat/` | NAT 맵 관리 |
| Policy 맵 | `pkg/maps/policymap/` | 정책 맵 관리 |
| IPCache 맵 | `pkg/maps/ipcache/` | IPCache 관리 |
| Calls 맵 | `pkg/maps/callsmap/` | Tail call 맵 관리 |

---

## 8. 전체 아키텍처 요약

```
                          유저스페이스
    ┌──────────────────────────────────────────────────┐
    │  cilium-agent                                    │
    │  ├─ pkg/datapath/loader/ (컴파일 + 로딩)         │
    │  ├─ pkg/maps/           (맵 관리)                │
    │  └─ cilium/ebpf         (BPF 시스콜 추상화)      │
    └─────────────────────┬────────────────────────────┘
                          │ bpf() syscall
    ──────────────────────┼────────────────────────────
                          │ 커널
    ┌─────────────────────┴────────────────────────────┐
    │                                                   │
    │  XDP ──── TC ingress ──── TC egress               │
    │   │         │                 │                    │
    │   │    ┌────┴─────┐    ┌─────┴─────┐              │
    │   │    │ bpf_lxc  │    │ bpf_host  │              │
    │   │    │ bpf_host │    │ bpf_lxc   │              │
    │   │    └────┬─────┘    └─────┬─────┘              │
    │   │         │                │                    │
    │   │    tail calls (cilium_calls)                  │
    │   │    ┌──────────────────────┐                   │
    │   │    │ LB → CT → Policy    │                   │
    │   │    │ → NAT → Forward     │                   │
    │   │    └──────────────────────┘                   │
    │   │              │                                │
    │   │    BPF 맵 (공유 데이터)                       │
    │   │    ┌──────────────────────┐                   │
    │   │    │ CT  NAT  Policy     │                   │
    │   │    │ LB  IPCache Events  │                   │
    │   │    └──────────────────────┘                   │
    │   │                                               │
    │   cgroup/connect (bpf_sock) ── 소켓 LB            │
    │                                                   │
    └───────────────────────────────────────────────────┘
```

---

## 요약표

| 구분 | 핵심 내용 |
|------|-----------|
| **프로그램 유형** | XDP (NIC 레벨), TC (패킷 처리 핵심), Socket/cgroup (소켓 LB) |
| **맵 유형** | Hash (O(1)), LRU Hash (자동 정리), Array (인덱스), LPM Trie (CIDR), Hash-of-Maps (동적) |
| **Tail Call** | 검증기 한도 우회, `PROG_ARRAY` 맵으로 프로그램 체이닝, 최대 33회 중첩 |
| **상태 전달** | ctx metadata (CB_ 슬롯), Per-CPU Array 맵 |
| **커널 기능** | 자체 BPF CT, FIB lookup, netlink, XFRM (IPsec) |
| **컴파일** | C → Clang (-O2 --target=bpf) → ELF (.o) → cilium/ebpf 로더 → 커널 |
| **핀닝** | `/sys/fs/bpf/tc/globals/` 아래 전역 맵 공유 |
