# 23. XDP 초고속 패킷 처리 (eXpress Data Path)

> Cilium 소스 기준: `bpf/bpf_xdp.c`, `pkg/datapath/xdp/xdp.go`, `pkg/datapath/xdp/cell.go`

---

## 1. 개요

### 1.1 XDP란?

XDP(eXpress Data Path)는 Linux 커널의 네트워크 드라이버 레벨에서 패킷을 처리하는 eBPF 프레임워크이다. 패킷이 커널 네트워크 스택에 도달하기 **전에** 처리하므로 극도로 낮은 레이턴시와 높은 처리량을 달성한다.

```
패킷 처리 계층 비교:
┌─────────────────────────────────────────────────────┐
│                                                       │
│  NIC 하드웨어 → DMA 버퍼                              │
│       │                                               │
│       ▼                                               │
│  ┌──────────┐   ← XDP (드라이버 레벨, 가장 빠름)     │
│  │ XDP Hook │      - 소프트웨어 인터럽트 전           │
│  └────┬─────┘      - sk_buff 할당 전                  │
│       │                                               │
│       ▼                                               │
│  ┌──────────┐   ← TC (Traffic Control, 두 번째)       │
│  │ TC Hook  │      - sk_buff 할당 후                  │
│  └────┬─────┘      - 네트워크 스택 입구               │
│       │                                               │
│       ▼                                               │
│  ┌──────────┐   ← iptables/nftables (가장 느림)       │
│  │ Netfilter│      - 전체 네트워크 스택 통과           │
│  └──────────┘                                         │
│                                                       │
└─────────────────────────────────────────────────────┘
```

### 1.2 Cilium에서 XDP의 역할

Cilium은 XDP를 두 가지 용도로 사용한다:

1. **NodePort 가속 (NodePort Acceleration)**: 외부에서 들어오는 NodePort/LoadBalancer 트래픽의 로드밸런싱을 XDP 레벨에서 수행
2. **CIDR 프리필터 (Prefilter)**: 블랙리스트 기반 IP 차단을 NIC 드라이버 레벨에서 즉시 드롭

### 1.3 아키텍처 개요

```
┌──────────────────────────────────────────────────────┐
│                 XDP 데이터패스                         │
│                                                        │
│  NIC 수신                                              │
│    │                                                    │
│    ▼                                                    │
│  cil_xdp_entry()  [bpf/bpf_xdp.c:364]                │
│    │                                                    │
│    ├── validate_ethertype()  이더넷 타입 검증            │
│    │                                                    │
│    ├── xdp_early_hook()  사전 훅 (커스텀)               │
│    │                                                    │
│    ├── [IPv4] prefilter_v4()  CIDR 블랙리스트 검사      │
│    │     ├── cilium_cidr_v4_fix (Hash Map)             │
│    │     └── cilium_cidr_v4_dyn (LPM Trie)             │
│    │                                                    │
│    ├── [IPv4] check_v4_lb()  NodePort LB               │
│    │     └── tail_lb_ipv4() → nodeport_lb4()           │
│    │                                                    │
│    ├── [IPv6] prefilter_v6()  CIDR 블랙리스트 검사      │
│    │     ├── cilium_cidr_v6_fix (Hash Map)             │
│    │     └── cilium_cidr_v6_dyn (LPM Trie)             │
│    │                                                    │
│    └── [IPv6] check_v6_lb()  NodePort LB               │
│          └── tail_lb_ipv6() → nodeport_lb6()           │
│                                                        │
│  반환값:                                                │
│    XDP_PASS   → 커널 스택으로 전달                      │
│    XDP_DROP   → 패킷 즉시 폐기                         │
│    XDP_TX     → 동일 인터페이스로 반송                   │
│    XDP_REDIRECT → 다른 인터페이스로 리다이렉트           │
└──────────────────────────────────────────────────────┘
```

---

## 2. 소스 코드 분석

### 2.1 Go 구성 계층 (`pkg/datapath/xdp/xdp.go`)

#### AccelerationMode

XDP 가속 모드를 정의하는 열거형 (`pkg/datapath/xdp/xdp.go:16`):

```go
type AccelerationMode string

const (
    AccelerationModeNative     AccelerationMode = "native"
    AccelerationModeBestEffort AccelerationMode = "best-effort"
    AccelerationModeGeneric    AccelerationMode = "testing-only"
    AccelerationModeDisabled   AccelerationMode = "disabled"
)
```

| 모드 | 커널 모드 | 성능 | 설명 |
|------|----------|------|------|
| `native` | `xdpdrv` | 최고 | NIC 드라이버가 직접 XDP 지원 (10Gbps+) |
| `best-effort` | `xdpdrv` (폴백: `xdpgeneric`) | 높음 | native 시도, 실패 시 generic 폴백 |
| `testing-only` | `xdpgeneric` | 보통 | 소프트웨어 에뮬레이션, 테스트용 |
| `disabled` | - | - | XDP 비활성화 |

#### Config 구조체

```go
type Config struct {
    mode AccelerationMode
}

// Mode()는 실제 커널에 전달할 XDP 모드를 반환
func (cfg Config) Mode() Mode {
    switch cfg.mode {
    case AccelerationModeNative, AccelerationModeBestEffort:
        return ModeLinkDriver    // "xdpdrv"
    case AccelerationModeGeneric:
        return ModeLinkGeneric   // "xdpgeneric"
    }
    return ModeLinkNone
}
```

#### Enabler 패턴 (Hive DI)

XDP 모드는 여러 기능이 요청할 수 있으며, `newConfig()`에서 충돌을 해결한다:

```go
type newConfigIn struct {
    cell.In
    Enablers []enabler `group:"request-xdp-mode"`
}

func newConfig(in newConfigIn) (Config, error) {
    // 우선순위: Native > BestEffort > Generic > Disabled
    // 충돌 시 에러 반환
}
```

**왜 Enabler 패턴을 사용하는가?**

NodePort 가속, 프리필터, DSR 등 여러 기능이 각각 XDP를 요청할 수 있다. Hive DI의 그룹 주입을 통해 모든 요청을 수집하고, 가장 강력한 모드를 선택하되 충돌 시 에러를 반환한다.

### 2.2 BPF 프로그램 (`bpf/bpf_xdp.c`)

#### 진입점

```c
// bpf/bpf_xdp.c:364
__section_entry
int cil_xdp_entry(struct __ctx_buff *ctx)
{
    check_and_store_ip_trace_id(ctx);
    return check_filters(ctx);
}
```

#### 필터 체인

```c
// bpf/bpf_xdp.c:318
static __always_inline int check_filters(struct __ctx_buff *ctx)
{
    int ret = CTX_ACT_OK;
    __be16 proto;

    if (!validate_ethertype(ctx, &proto))
        return CTX_ACT_OK;  // 비-IP 패킷은 통과

    ctx_store_meta(ctx, XFER_MARKER, 0);
    ctx_skip_nodeport_clear(ctx);

    // 커스텀 사전 훅
    ret = xdp_early_hook(ctx, proto);
    if (ret != CTX_ACT_OK)
        return ret;

    switch (proto) {
    case bpf_htons(ETH_P_IP):
        // IPv4 프리필터
        if (CONFIG(enable_xdp_prefilter)) {
            ret = prefilter_v4(ctx);
            if (ret == CTX_ACT_DROP)
                return ret;
        }
        // NodePort LB
        ret = check_v4_lb(ctx);
        break;

    case bpf_htons(ETH_P_IPV6):
        // IPv6 프리필터
        if (CONFIG(enable_xdp_prefilter)) {
            ret = prefilter_v6(ctx);
            if (ret == CTX_ACT_DROP)
                return ret;
        }
        // NodePort LB
        ret = check_v6_lb(ctx);
        break;
    }

    return bpf_xdp_exit(ctx, ret);
}
```

#### CIDR 프리필터

```c
// bpf/bpf_xdp.c:215
static __always_inline int prefilter_v4(struct __ctx_buff *ctx)
{
    struct iphdr *ipv4_hdr = data + sizeof(struct ethhdr);
    struct lpm_v4_key pfx;

    // 소스 IP를 LPM 키로 구성
    memcpy(pfx.lpm.data, &ipv4_hdr->saddr, sizeof(pfx.addr));
    pfx.lpm.prefixlen = 32;

    // 1. 동적 LPM Trie 검사
    if (map_lookup_elem(&cilium_cidr_v4_dyn, &pfx))
        return CTX_ACT_DROP;

    // 2. 고정 Hash Map 검사
    if (map_lookup_elem(&cilium_cidr_v4_fix, &pfx))
        return CTX_ACT_DROP;

    return 0;
}
```

**왜 두 종류의 맵을 사용하는가?**

| 맵 타입 | 용도 | 장점 |
|---------|------|------|
| `BPF_MAP_TYPE_HASH` (fix) | 고정 블랙리스트 (/32 정확한 IP) | O(1) 조회, 정확한 매치 |
| `BPF_MAP_TYPE_LPM_TRIE` (dyn) | 동적 CIDR 범위 (/16, /24 등) | 프리픽스 매칭, 범위 차단 |

고정 맵은 정확한 IP 차단에 빠르고, LPM Trie는 CIDR 범위 차단에 적합하다.

#### NodePort 가속 (Tail Call)

```c
// bpf/bpf_xdp.c:98
__declare_tail(CILIUM_CALL_IPV4_FROM_NETDEV)
int tail_lb_ipv4(struct __ctx_buff *ctx)
{
    bool punt_to_stack = false;
    int ret = CTX_ACT_OK;

    if (!ctx_skip_nodeport(ctx)) {
        struct iphdr *ip4;
        bool is_dsr = false;

        // DSR + Geneve 터널 처리 (있을 경우)
        // ...

        // NodePort 로드밸런싱 수행
        ret = nodeport_lb4(ctx, ip4, l3_off, UNKNOWN_ID,
                           &punt_to_stack, &ext_err, &is_dsr);
    }

    return bpf_xdp_exit(ctx, ret);
}
```

### 2.3 BPF 맵 정의

```c
// bpf/bpf_xdp.c:50-84
// IPv4 고정 CIDR 맵 (Hash)
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __type(key, struct lpm_v4_key);
    __type(value, struct lpm_val);
    __uint(max_entries, CIDR4_HMAP_ELEMS);
    __uint(map_flags, BPF_F_NO_PREALLOC | BPF_F_RDONLY_PROG_COND);
} cilium_cidr_v4_fix __section_maps_btf;

// IPv4 동적 CIDR 맵 (LPM Trie)
struct {
    __uint(type, BPF_MAP_TYPE_LPM_TRIE);
    __type(key, struct lpm_v4_key);
    __type(value, struct lpm_val);
    __uint(max_entries, CIDR4_LMAP_ELEMS);
    __uint(map_flags, BPF_F_NO_PREALLOC | BPF_F_RDONLY_PROG_COND);
} cilium_cidr_v4_dyn __section_maps_btf;
```

`BPF_F_RDONLY_PROG_COND` 플래그는 BPF 프로그램에서 읽기만 가능하게 제한한다. 쓰기는 유저스페이스(cilium-agent)에서만 수행한다.

---

## 3. XDP 부착 모드

### 3.1 커널 모드 비교

```
┌────────────────────────────────────────────────────────┐
│                  XDP 부착 모드                           │
├──────────┬─────────────┬──────────────────────────────┤
│ 모드      │ 처리 위치    │ 성능                         │
├──────────┼─────────────┼──────────────────────────────┤
│ xdpdrv   │ NIC 드라이버 │ 최고 (14M+ pps)              │
│ (native) │ NAPI poll   │ sk_buff 할당 전               │
│          │ 루프 안       │ 제로카피 가능                 │
├──────────┼─────────────┼──────────────────────────────┤
│ xdpgeneric│ 네트워크     │ 보통 (TC 수준)               │
│ (generic) │ 스택 진입 후 │ sk_buff 이미 할당             │
│           │             │ 테스트/호환성용                │
├──────────┼─────────────┼──────────────────────────────┤
│ xdpoffload│ NIC 하드웨어│ 극한 (CPU 무부하)             │
│ (offload) │ 자체에서     │ 매우 제한된 NIC만 지원        │
│           │ 실행         │ (Netronome 등)               │
└──────────┴─────────────┴──────────────────────────────┘
```

### 3.2 부착 플래그

```go
// pkg/datapath/xdp/xdp.go:145
func (cfg Config) GetAttachFlags() link.XDPAttachFlags {
    switch cfg.mode {
    case AccelerationModeNative, AccelerationModeBestEffort:
        return link.XDPDriverMode
    case AccelerationModeGeneric:
        return link.XDPGenericMode
    }
    return 0
}
```

---

## 4. NodePort 가속 상세

### 4.1 XDP에서 NodePort 처리 흐름

```
외부 트래픽 → NIC 수신
    │
    ▼
cil_xdp_entry()
    │
    ├── IPv4: tail_lb_ipv4()
    │     │
    │     ├── DSR Geneve 터널 패킷 감지
    │     │   (UDP dport == tunnel_port이고 체크섬 0이면)
    │     │   → 내부 패킷으로 L3 오프셋 조정
    │     │
    │     └── nodeport_lb4()
    │           │
    │           ├── NodePort 서비스 룩업
    │           │   (목적지 IP:Port로 LB 맵 조회)
    │           │
    │           ├── 백엔드 선택
    │           │   (Maglev 해시 또는 랜덤)
    │           │
    │           ├── DNAT 수행
    │           │   (목적지 → 백엔드 주소로 변경)
    │           │
    │           └── XDP_TX 또는 XDP_REDIRECT
    │               (같은 NIC로 반송 또는 다른 NIC로 전달)
    │
    └── IPv6: tail_lb_ipv6()
          └── nodeport_lb6() (동일 로직)
```

### 4.2 DSR (Direct Server Return) 처리

XDP에서 DSR 모드를 지원하면 응답 패킷이 원래 클라이언트에게 직접 전송된다:

```
일반 DNAT:
  Client → LB Node → Backend → LB Node → Client
  (응답이 LB를 다시 경유)

DSR:
  Client → LB Node → Backend → Client
  (응답이 직접 클라이언트로)

DSR + Geneve 터널:
  XDP에서 Geneve 헤더 안의 내부 패킷을 파싱하여
  터널 오버헤드 없이 직접 로드밸런싱
```

---

## 5. CIDR 프리필터 상세

### 5.1 프리필터 동작

```
프리필터 결정 트리:
┌──────────────────────┐
│ enable_xdp_prefilter │
│ 설정이 켜져 있는가?   │
└──────┬───────────────┘
       │ Yes
       ▼
┌──────────────────────┐     ┌───────────┐
│ cilium_cidr_v4_dyn   │ ──> │ LPM Match │ → DROP
│ (LPM Trie 조회)      │     │ (CIDR)    │
└──────┬───────────────┘     └───────────┘
       │ No Match
       ▼
┌──────────────────────┐     ┌───────────┐
│ cilium_cidr_v4_fix   │ ──> │ 정확 Match│ → DROP
│ (Hash Map 조회)       │     │ (/32)    │
└──────┬───────────────┘     └───────────┘
       │ No Match
       ▼
     PASS (커널 스택으로)
```

### 5.2 사용 사례

| 사례 | CIDR | 맵 |
|------|------|----|
| 알려진 공격 IP 차단 | `1.2.3.4/32` | cilium_cidr_v4_fix |
| 국가별 IP 대역 차단 | `203.0.0.0/8` | cilium_cidr_v4_dyn |
| 스푸핑 방지 | `0.0.0.0/8` | cilium_cidr_v4_dyn |
| 보고네트 차단 | `240.0.0.0/4` | cilium_cidr_v4_dyn |

---

## 6. XDP 컨텍스트와 TC 전환

### 6.1 XDP → TC 메타데이터 전달

```c
// bpf/bpf_xdp.c:87
static __always_inline int
bpf_xdp_exit(struct __ctx_buff *ctx, const int verdict)
{
    if (verdict == CTX_ACT_OK)
        ctx_move_xfer(ctx);  // 메타데이터를 TC로 전달
    return verdict;
}
```

`ctx_move_xfer()`는 XDP 컨텍스트의 메타데이터를 TC(Traffic Control) BPF 프로그램으로 전달한다. XDP에서 패킷을 PASS하면 TC에서 추가 처리(정책 검사 등)를 수행할 수 있다.

### 6.2 XDP_PASS와 커널 스택

```
XDP_PASS 이후:
XDP → sk_buff 할당 → TC ingress → Netfilter → 로컬 프로세스/라우팅
                       │
                       └── bpf_host.c (TC 프로그램)
                           → 호스트 방화벽
                           → 정책 검사
                           → 포워딩
```

---

## 7. 성능 특성

### 7.1 벤치마크 비교

```
처리량 비교 (단일 코어, 64바이트 패킷):
┌──────────────┬─────────────┬──────────┐
│ 처리 방식     │ Packets/sec │ 레이턴시  │
├──────────────┼─────────────┼──────────┤
│ XDP native   │ ~14M pps    │ ~100ns   │
│ XDP generic  │ ~3M pps     │ ~500ns   │
│ TC BPF       │ ~3M pps     │ ~500ns   │
│ iptables     │ ~1M pps     │ ~2μs     │
│ userspace LB │ ~200K pps   │ ~10μs    │
└──────────────┴─────────────┴──────────┘

* 실제 성능은 NIC, CPU, 패킷 크기에 따라 상이
```

### 7.2 왜 XDP가 빠른가?

1. **sk_buff 할당 생략**: 가장 큰 비용인 소켓 버퍼 할당을 건너뜀
2. **제로카피**: NIC DMA 버퍼에서 직접 패킷 데이터에 접근
3. **배치 처리**: NAPI poll 루프 안에서 여러 패킷을 연속 처리
4. **캐시 친화적**: 패킷 데이터가 L1/L2 캐시에 남아 있는 상태에서 처리
5. **커널 우회**: TCP/IP 스택의 복잡한 처리를 완전히 건너뜀

---

## 8. Validator 패턴

### 8.1 XDP 모드 검증

```go
// pkg/datapath/xdp/xdp.go:175
type Validator func(AccelerationMode, Mode) error

func WithValidator(validator Validator) enablerOpt {
    return func(te *enabler) {
        te.validators = append(te.validators, validator)
    }
}

func WithEnforceXDPDisabled(reason string) enablerOpt {
    return func(te *enabler) {
        te.validators = append(te.validators,
            func(m AccelerationMode, _ Mode) error {
                if m != AccelerationModeDisabled {
                    return fmt.Errorf("XDP must be disabled: %s", reason)
                }
                return nil
            },
        )
    }
}
```

**사용 예**: WireGuard와 XDP는 특정 조합에서 호환되지 않으므로, WireGuard 기능이 `WithEnforceXDPDisabled`를 사용하여 XDP 비활성화를 강제할 수 있다.

---

## 9. 제한 사항

| 항목 | 설명 |
|------|------|
| NIC 지원 | native 모드는 XDP를 지원하는 NIC 드라이버 필요 |
| 멀티버퍼 | 점보 프레임(MTU > 페이지 크기) 시 XDP 멀티버퍼 필요 |
| 본딩 | 본딩/팀 디바이스에서 XDP 지원 제한적 |
| L7 정책 | XDP에서는 L7(HTTP, DNS) 정책 적용 불가 |
| 호스트 방화벽 | XDP PASS 후 TC에서 별도 처리 필요 |
| VLAN | 일부 NIC에서 VLAN 태그된 XDP 패킷 처리 이슈 |

---

## 10. 설계 결정의 이유 (Why)

### Q1: 왜 TC 대신 XDP를 사용하는가?

NodePort 로드밸런싱은 패킷의 목적지만 변경하면 되는 단순 작업이다. 이런 작업에 전체 네트워크 스택을 통과시키는 것은 낭비이다. XDP는 드라이버 레벨에서 처리하므로 4-10배 빠르다.

### Q2: 왜 XDP에서 일부 패킷만 처리하고 나머지는 TC로 넘기는가?

XDP는 BPF 명령어 수 제한이 있고, 복잡한 정책(L7, ConnTrack 등)을 처리하기 어렵다. 단순한 작업(LB, 프리필터)만 XDP에서 처리하고, 복잡한 작업은 TC(bpf_host.c)에서 처리하는 것이 최적이다.

### Q3: 왜 프리필터에 Hash맵과 LPM Trie를 모두 사용하는가?

- Hash맵: 정확한 /32 매치는 O(1)로 가장 빠름
- LPM Trie: CIDR 범위 매치(/16, /24 등)가 필요할 때만 사용
- 두 맵을 분리하여 각각의 강점을 활용

---

*검증 기준: Cilium 소스코드 `bpf/bpf_xdp.c`, `pkg/datapath/xdp/` 직접 분석*
