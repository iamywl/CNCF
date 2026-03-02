# 8. Cilium 네트워킹 서브시스템

---

## 개요

Cilium의 네트워킹 서브시스템은 eBPF 기반 데이터패스 위에 구축된다.
터널링(VXLAN/Geneve), 암호화(WireGuard/IPsec), 동적 라우팅(BGP), NAT,
그리고 듀얼스택(IPv4/IPv6)을 모두 커널 수준에서 처리하여 기존 iptables/kube-proxy를
완전히 대체한다.

```
Pod A (Node 1)                                   Pod B (Node 2)
  │                                                 ▲
  ▼                                                 │
┌──────────────────────┐                ┌──────────────────────┐
│  cil_from_container  │                │  cil_to_container    │
│  (bpf_lxc.c)        │                │  (bpf_lxc.c)        │
├──────────────────────┤                ├──────────────────────┤
│  Identity 조회        │                │  Policy 검사          │
│  Policy 검사          │                │  CT 조회/업데이트     │
│  CT 조회/생성         │                │  RevNAT 적용         │
│  NAT (SNAT/DNAT)     │                │                      │
├──────────────────────┤                ├──────────────────────┤
│  라우팅 결정           │                │  디캡슐화             │
│  ├─ 같은 노드? → 직접 전달           │  │  (VXLAN/Geneve)      │
│  ├─ 터널 모드? → 캡슐화              │  │  Identity 복원       │
│  └─ 네이티브? → FIB lookup           │  │                      │
├──────────────────────┤                ├──────────────────────┤
│  cil_to_overlay /    │ ── 네트워크 ──▶│  cil_from_overlay    │
│  cil_to_netdev       │                │  (bpf_overlay.c)     │
│  (bpf_overlay.c /    │                │                      │
│   bpf_host.c)        │                │                      │
└──────────────────────┘                └──────────────────────┘
```

---

## 1. 터널 프로토콜: VXLAN, Geneve, VTEP

### 1.1 캡슐화 프로토콜 선택

Cilium은 두 가지 오버레이 캡슐화 프로토콜을 지원한다.

| 프로토콜 | 기본 포트 | 헤더 크기 | 특징 |
|---------|----------|----------|------|
| VXLAN | 8472 | 50B (Outer IP+UDP+VXLAN) | 가장 널리 사용, 하드웨어 오프로드 지원 |
| Geneve | 6081 | 50B+ (가변 옵션) | 확장 가능한 TLV 옵션, DSR 정보 전달 가능 |

프로토콜 설정은 `pkg/datapath/tunnel/tunnel.go`에서 관리한다:

```go
// pkg/datapath/tunnel/tunnel.go
type EncapProtocol string

const (
    VXLAN   EncapProtocol = "vxlan"
    Geneve  EncapProtocol = "geneve"
    Disabled EncapProtocol = ""
)

// BPF 데이터패스에서 사용하는 프로토콜 ID
const (
    TUNNEL_PROTOCOL_NONE   BPFEncapProtocol = 0
    TUNNEL_PROTOCOL_VXLAN  BPFEncapProtocol = 1
    TUNNEL_PROTOCOL_GENEVE BPFEncapProtocol = 2
)
```

### 1.2 캡슐화 과정 (Encapsulation)

패킷이 다른 노드의 Pod으로 향할 때, BPF 프로그램이 터널 헤더를 추가한다.

**핵심 코드**: `bpf/lib/encap.h`

```
원본 패킷:
┌──────────┬──────────┬─────────────┐
│ Ethernet │ Inner IP │ TCP/UDP ... │
└──────────┴──────────┴─────────────┘

캡슐화 후 (VXLAN):
┌──────────┬──────────┬─────┬──────┬──────────┬──────────┬─────────────┐
│ Outer    │ Outer IP │ UDP │ VXLAN│ Ethernet │ Inner IP │ TCP/UDP ... │
│ Ethernet │ (노드간) │ 8472│ VNI  │ (원본)   │ (원본)   │             │
└──────────┴──────────┴─────┴──────┴──────────┴──────────┴─────────────┘
```

`__encap_with_nodeid4()` 함수가 IPv4 터널 캡슐화를 수행한다:

```c
// bpf/lib/encap.h — IPv4 캡슐화 핵심 함수
static __always_inline int
__encap_with_nodeid4(struct __ctx_buff *ctx, __u32 src_ip, __be16 src_port,
                     __be32 tunnel_endpoint,
                     __u32 seclabel, __u32 dstid, __u32 vni,
                     enum trace_reason ct_reason, __u32 monitor,
                     int *ifindex, __be16 proto)
{
    // 로컬 호스트에서 온 패킷은 LOCAL_NODE_ID로 표시
    if (seclabel == HOST_ID)
        seclabel = LOCAL_NODE_ID;

    // 터널 인터페이스 설정 후 메타데이터(VNI, seclabel) 포함하여 캡슐화
    return ctx_set_encap_info4(ctx, src_ip, src_port,
                                tunnel_endpoint, seclabel, vni,
                                NULL, 0);
}
```

IPv6 터널 엔드포인트도 지원된다 (`__encap_with_nodeid6()`).
터널 언더레이 프로토콜(IPv4/IPv6)은 `pkg/datapath/tunnel/tunnel.go`의 `UnderlayProtocol` 설정으로 결정된다.

### 1.3 디캡슐화 과정 (Decapsulation)

수신 노드에서는 `bpf_overlay.c`의 `cil_from_overlay` 프로그램이 터널 패킷을 처리한다:

```c
// bpf/bpf_overlay.c — 오버레이에서 수신한 패킷 처리 흐름
// 1. get_tunnel_key()로 터널 메타데이터(VNI, Identity) 추출
// 2. Identity 기반으로 보안 정책 확인
// 3. lookup_ip4_endpoint()로 로컬 엔드포인트 검색
// 4. 로컬 Pod이면 직접 전달, 아니면 호스트로 전달
```

`get_tunnel_key()` 함수 (`bpf/lib/encap.h`)가 터널 키를 추출한다:

```c
// bpf/lib/encap.h
static __always_inline int
get_tunnel_key(struct __ctx_buff *ctx, struct bpf_tunnel_key *key)
{
    // IPv4로 먼저 시도, 실패하면 IPv6로 시도
    ret = ctx_get_tunnel_key(ctx, key, key_size, 0);            // IPv4
    ret = ctx_get_tunnel_key(ctx, key, key_size, BPF_F_TUNINFO_IPV6); // IPv6
}
```

### 1.4 VTEP (VXLAN Tunnel Endpoint)

VTEP는 외부 VXLAN 게이트웨이와의 통합을 위한 기능이다.
BPF 맵 `cilium_vtep_map`이 외부 VTEP의 IP, MAC, 터널 엔드포인트를 매핑한다.

**핵심 코드**: `bpf/lib/vtep.h`

```c
// bpf/lib/vtep.h
struct vtep_key {
    __u32 vtep_ip;          // VTEP의 내부 IP
};

struct vtep_value {
    __u64 vtep_mac;         // VTEP의 MAC 주소
    __u32 tunnel_endpoint;  // 실제 터널 엔드포인트 IP
};

// BPF 해시맵으로 VTEP 정보 관리
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __type(key, struct vtep_key);
    __type(value, struct vtep_value);
} cilium_vtep_map;
```

VTEP로 향하는 패킷은 `encap_and_redirect_with_nodeid()`에서 특별한 VNI(`NOT_VTEP_DST` 아닌 실제 VNI)를 사용하여 캡슐화된다.

### 1.5 Geneve DSR (Direct Server Return)

Geneve 프로토콜의 확장 가능한 옵션 필드를 활용하여 DSR 정보를 전달한다.

```c
// bpf/lib/encap.h — DSR 옵션이 포함된 Geneve 캡슐화
struct geneve_dsr_opt4 {
    struct geneve_opt_hdr hdr;  // 옵션 클래스, 타입, 길이
    __be32 addr;                // 원본 클라이언트 IP
    __be16 port;                // 원본 클라이언트 포트
};

static __always_inline void
set_geneve_dsr_opt4(__be16 port, __be32 addr, struct geneve_dsr_opt4 *gopt)
{
    gopt->hdr.opt_class = bpf_htons(DSR_GENEVE_OPT_CLASS);
    gopt->hdr.type = DSR_GENEVE_OPT_TYPE;
    gopt->hdr.length = DSR_IPV4_GENEVE_OPT_LEN;
    gopt->addr = addr;
    gopt->port = port;
}
```

---

## 2. 암호화

### 2.1 WireGuard

WireGuard는 Cilium의 기본 노드간 암호화 방식이다. Noise 프로토콜 기반의 현대적 VPN으로,
커널 내장 모듈을 활용한다.

**핵심 코드**: `pkg/wireguard/agent/agent.go`

```go
// pkg/wireguard/agent/agent.go
type Agent struct {
    lock.RWMutex

    privKey  wgtypes.Key           // 노드의 개인키
    wgClient wireguardClient       // wgctrl 클라이언트

    peerByNodeName   map[string]*peerConfig    // 노드명 → 피어 설정
    nodeNameByNodeIP map[string]string          // 노드 IP → 노드명
    nodeNameByPubKey map[wgtypes.Key]string     // 공개키 → 노드명
}
```

WireGuard 동작 흐름:

```
Node A                                         Node B
  │                                              │
  │ 1. 시작 시 키 페어 생성/로드                    │
  │    loadOrGeneratePrivKey()                    │
  │                                              │
  │ 2. 공개키를 CiliumNode CRD에 게시              │
  │    initLocalNodeFromWireGuard()               │
  │                                              │
  │ 3. 다른 노드의 공개키 수신 (NodeManager)       │
  │    updatePeer()                               │
  │                                              │
  │ 4. WireGuard 인터페이스(cilium_wg0) 설정       │
  │    AllowedIPs: 상대 노드의 Pod CIDR            │
  │                                              │
  │ ◄═══════ 암호화된 UDP 터널 (51871) ═══════► │
```

Agent의 시작 과정 (`Start()`):

```go
// pkg/wireguard/agent/agent.go
func (a *Agent) Start(cell.HookContext) error {
    // 1. WireGuard 인터페이스 초기화
    a.init()  // 키 로드, wg 디바이스 생성

    // 2. 로컬 노드에 WireGuard 공개키 등록
    a.localNode.Update(func(ln *node.LocalNode) {
        a.initLocalNodeFromWireGuard(ln, sel)
    })

    // 3. IPCache 이벤트 구독 (네이티브 라우팅 모드에서)
    if a.needsIPCache() {
        a.ipCache.AddListener(a)
    }

    // 4. 노드 이벤트 구독 (피어 추가/제거)
    a.nodeManager.Subscribe(a)

    // 5. MTU 조정, 만료 피어 정리 작업 등록
    a.jobGroup.Add(
        job.OneShot("mtu-reconciler", a.mtuReconciler),
        job.OneShot("peer-gc", a.peerGarbageCollector, ...),
    )
}
```

노드별 암호화 선택 해제(opt-out)도 지원된다.
`NodeEncryptionOptOutLabels` 레이블 셀렉터에 매칭되는 노드는 `EncryptionKey`를 0으로 설정하여
다른 노드에게 암호화하지 말 것을 알린다.

### 2.2 IPsec (XFRM)

IPsec은 Linux 커널의 XFRM(Transform) 프레임워크를 활용한다.
AES-GCM-128/256 또는 AES-CBC+HMAC-SHA256 알고리즘을 지원한다.

**핵심 코드**: `pkg/datapath/linux/ipsec/` 디렉토리

```
IPsec 아키텍처:
┌──────────────────────────────┐
│  cilium-agent                │
│  ├─ ipsec.Agent              │  키 관리, XFRM 정책/상태 설정
│  │  (pkg/datapath/linux/     │
│  │   ipsec/cell.go)          │
│  ├─ linuxNodeHandler         │  노드별 XFRM SA/SP 설정
│  │  (pkg/datapath/linux/     │
│  │   ipsec.go)               │
│  └─ XFRMCollector            │  XFRM 에러 메트릭 수집
│     (xfrm_collector.go)      │
└──────────────┬───────────────┘
               │ netlink (XFRM)
               ▼
┌──────────────────────────────┐
│  Linux Kernel                │
│  ├─ XFRM State (SA)         │  암호화 키, 알고리즘, SPI
│  ├─ XFRM Policy (SP)        │  어떤 트래픽을 암호화할지
│  └─ ESP 처리                  │  실제 패킷 암호화/복호화
└──────────────────────────────┘
```

IPsec Cell 등록 (`pkg/datapath/linux/ipsec/cell.go`):

```go
var Cell = cell.Module(
    "ipsec-agent",
    "Handles initial key setup and knows the key size",
    cell.Config(defaultUserConfig),
    cell.Provide(newIPsecAgent, newIPsecConfig),
)
```

XFRM 에러 모니터링 (`xfrm_collector.go`):

```go
// pkg/datapath/linux/ipsec/xfrm_collector.go
// XFRM 에러 유형: no_state, state_expired, no_policy 등
type xfrmCollector struct {
    xfrmErrorDesc    *prometheus.Desc  // XFRM 에러 카운터
    nbKeysDesc       *prometheus.Desc  // 사용 중인 키 수
    nbXFRMStatesDesc *prometheus.Desc  // XFRM SA 수
    nbXFRMPolsDesc   *prometheus.Desc  // XFRM SP 수
}
```

### 2.3 WireGuard vs IPsec 비교

| 항목 | WireGuard | IPsec |
|------|-----------|-------|
| 키 관리 | 자동 (Noise 프로토콜) | 수동 (키 로테이션 필요) |
| 성능 | 높음 (ChaCha20-Poly1305) | 중간 (AES-GCM, 하드웨어 오프로드 가능) |
| 설정 복잡도 | 낮음 | 높음 (XFRM SA/SP) |
| 커널 요구사항 | 5.6+ (내장) | 모든 커널 |
| 코드 위치 | `pkg/wireguard/` | `pkg/datapath/linux/ipsec/` |
| BPF 프로그램 | `bpf_wireguard.c` | `bpf/lib/ipsec.h` |

---

## 3. 라우팅

### 3.1 BGP (Border Gateway Protocol)

Cilium은 GoBGP 라이브러리를 통합하여 BGP 컨트롤 플레인을 제공한다.
Pod CIDR, 서비스 VIP, Pod IP 등을 외부 라우터에 광고할 수 있다.

**핵심 코드**: `pkg/bgp/` 디렉토리

```
BGP 컨트롤 플레인 아키텍처:
┌─────────────────────────────────────────────┐
│  BGP Manager (pkg/bgp/manager/manager.go)   │
│  ├─ Reconciler들을 조율                       │
│  │                                           │
│  ├─ NeighborReconciler   → 피어 관계 관리     │
│  ├─ PodCIDRReconciler    → Pod CIDR 광고      │
│  ├─ ServiceReconciler    → 서비스 VIP 광고    │
│  ├─ PodIPPoolReconciler  → Pod IP 풀 광고     │
│  └─ PolicyReconciler     → 라우트 정책 관리    │
│                                               │
│  ┌───────────────────────────────────────┐   │
│  │  BGP Instance                         │   │
│  │  (pkg/bgp/manager/instance/)          │   │
│  │  └─ GoBGPServer                       │   │
│  │     (pkg/bgp/gobgp/server.go)         │   │
│  │     └─ server.BgpServer (GoBGP)       │   │
│  └───────────────────────────────────────┘   │
└─────────────────────────────────────────────┘
          │
          │ BGP (TCP 179)
          ▼
    ┌─────────────┐
    │ 외부 라우터   │
    │ (ToR, Spine) │
    └─────────────┘
```

GoBGP 서버 래퍼 (`pkg/bgp/gobgp/server.go`):

```go
// pkg/bgp/gobgp/server.go
type GoBGPServer struct {
    asn    uint32             // 로컬 AS 번호
    server *server.BgpServer  // GoBGP 서버 인스턴스
}

// IPv4/IPv6 패밀리 지원
var (
    GoBGPIPv4Family = &gobgp.Family{
        Afi:  gobgp.Family_AFI_IP,
        Safi: gobgp.Family_SAFI_UNICAST,
    }
    GoBGPIPv6Family = &gobgp.Family{
        Afi:  gobgp.Family_AFI_IP6,
        Safi: gobgp.Family_SAFI_UNICAST,
    }
)
```

BGP 경로 타입 (`pkg/bgp/types/bgp.go`):

```go
// pkg/bgp/types/bgp.go
type Path struct {
    NLRI           bgp.AddrPrefixInterface    // 프리픽스 (예: 10.0.0.0/24)
    PathAttributes []bgp.PathAttributeInterface // AS Path, Next Hop 등
    Family         Family                       // IPv4/IPv6
    Best           bool                         // 최적 경로 여부
}

type Neighbor struct {
    Address  netip.Addr  // 피어 IP 주소
    ASN      uint32      // 피어 AS 번호
    Timers   *NeighborTimers
    AfiSafis []*Family
}
```

PodCIDR 광고 Reconciler (`pkg/bgp/manager/reconciler/pod_cidr.go`):
CiliumBGPVirtualRouter CRD에 정의된 광고 정책에 따라 Pod CIDR을 BGP 경로로 광고한다.

### 3.2 SRv6 (Segment Routing over IPv6)

SRv6는 IPv6 확장 헤더를 사용하여 패킷의 경로를 소스에서 지정하는 기술이다.

**핵심 코드**: `bpf/lib/srv6.h`

```
SRv6 BPF 맵 구조:
┌─────────────────────────────────────────────┐
│  cilium_srv6_vrf_v4/v6  (LPM Trie)         │
│  Key: VRF ID + IP prefix                    │
│  Value: VRF ID                              │
│  → 패킷이 속한 VRF 결정                       │
├─────────────────────────────────────────────┤
│  cilium_srv6_policy_v4/v6  (LPM Trie)      │
│  Key: VRF ID + IP prefix                    │
│  Value: SRv6 SID (IPv6 주소)                 │
│  → 목적지에 대한 SRv6 정책(SID) 조회           │
├─────────────────────────────────────────────┤
│  cilium_srv6_sid  (Hash Map)                │
│  Key: SRv6 SID (IPv6 주소)                   │
│  Value: VRF ID                              │
│  → 수신한 SRv6 패킷의 SID로 VRF 결정          │
└─────────────────────────────────────────────┘
```

SRv6 SRH(Segment Routing Header) 구조:

```c
// bpf/lib/srv6.h
struct srv6_srh {
    struct ipv6_rt_hdr rthdr;     // IPv6 라우팅 헤더
    __u8 first_segment;            // 첫 번째 세그먼트 인덱스
    __u8 flags;
    __u16 reserved;
    struct in6_addr segments[0];   // 세그먼트 리스트 (가변 길이)
};
```

### 3.3 네이티브 라우팅 (Native Routing)

터널 오버헤드 없이 리눅스 커널 라우팅 테이블을 직접 활용하는 모드.
클라우드 환경(AWS ENI, Azure, GKE)에서 주로 사용된다.

```go
// pkg/datapath/linux/node.go — 노드 핸들러
type linuxNodeHandler struct {
    nodeConfig     datapath.LocalNodeConfiguration
    datapathConfig DatapathConfiguration
    nodes          map[nodeTypes.Identity]*nodeTypes.Node

    // 캡슐화 필요 여부 판단 함수
    enableEncapsulation func(node *nodeTypes.Node) bool
}
```

네이티브 라우팅에서 패킷 흐름:

```
Pod → cil_from_container → FIB lookup → 물리 NIC → 라우터 → 대상 노드
                                         (터널 헤더 없음)
```

라우트 테이블 관리 (`pkg/datapath/tables/route.go`):

```go
// pkg/datapath/tables/route.go — StateDB 기반 라우트 테이블
type Route struct {
    Table     RouteTable
    LinkIndex int
    Dst       netip.Prefix
    Src       netip.Addr
    Gw        netip.Addr
}
```

---

## 4. 프로토콜 지원

### 4.1 IPv4/IPv6 듀얼스택

Cilium은 `ENABLE_IPV4`와 `ENABLE_IPV6` 플래그로 듀얼스택을 지원한다.
BPF 코드에서 조건부 컴파일로 두 프로토콜을 모두 처리한다.

```c
// bpf/bpf_overlay.c — 듀얼스택 패킷 분기
__section("tc")
int cil_from_overlay(struct __ctx_buff *ctx)
{
    // 이더넷 프로토콜 확인
    switch (proto) {
#ifdef ENABLE_IPV6
    case bpf_htons(ETH_P_IPV6):
        invoke_tailcall_if(CILIUM_CALL_IPV6_FROM_OVERLAY, ...);
#endif
#ifdef ENABLE_IPV4
    case bpf_htons(ETH_P_IP):
        invoke_tailcall_if(CILIUM_CALL_IPV4_FROM_OVERLAY, ...);
#endif
    }
}
```

NAT 맵도 IP 패밀리별로 분리된다 (`pkg/maps/nat/types.go`):

```go
// pkg/maps/nat/types.go
type IPFamily bool
const (
    IPv4 = IPFamily(true)
    IPv6 = IPFamily(false)
)

// IPv4 NAT 키: 4-tuple (src/dst addr + src/dst port)
type NatKey4 struct { tuple.TupleKey4Global }

// IPv6 NAT 키: 4-tuple (src/dst addr + src/dst port)
type NatKey6 struct { tuple.TupleKey6Global }
```

### 4.2 ARP/NDP

BPF 프로그램이 ARP 요청을 직접 처리하여 Pod의 MAC 주소를 응답한다.

**핵심 코드**: `bpf/lib/arp.h`

```c
// bpf/lib/arp.h
struct arp_eth {
    unsigned char ar_sha[ETH_ALEN];  // 송신자 MAC
    __be32        ar_sip;             // 송신자 IP
    unsigned char ar_tha[ETH_ALEN];  // 대상 MAC
    __be32        ar_tip;             // 대상 IP
};

// ARP 요청 검증
static __always_inline bool
arp_check(struct ethhdr *eth, const struct arphdr *arp, union macaddr *mac)
{
    return arp->ar_op == bpf_htons(ARPOP_REQUEST) &&
           arp->ar_hrd == bpf_htons(ARPHRD_ETHER) &&
           (eth_is_bcast(dmac) || !eth_addrcmp(dmac, mac));
}

// ARP 응답 생성
static __always_inline int
arp_prepare_response(struct __ctx_buff *ctx, union macaddr *smac, __be32 sip,
                     union macaddr *dmac, __be32 tip)
{
    // Ethernet, ARP 헤더를 응답용으로 덮어쓰기
    // 송신자/대상 MAC, IP를 교환
}
```

### 4.3 IGMP / Multicast

Cilium은 BPF에서 직접 IGMP 멤버십 리포트를 처리하여 멀티캐스트 그룹을 관리한다.

**핵심 코드**: `bpf/lib/mcast.h`

```c
// bpf/lib/mcast.h
struct mcast_subscriber_v4 {
    __be32 saddr;     // 구독자의 소스 주소
    __u32 ifindex;    // 로컬 인터페이스 또는 원격 출구 인터페이스
    __u8 flags;       // MCAST_SUB_F_REMOTE = 원격 구독자
};

// 멀티캐스트 그룹 맵: Hash-of-Maps
// Outer: 멀티캐스트 그룹 주소 → Inner Map
// Inner: 구독자 소스 주소 → 구독자 정보
```

---

## 5. NAT (Network Address Translation)

### 5.1 SNAT (Source NAT)

Pod에서 외부로 나가는 트래픽의 소스 주소를 노드 IP로 변환한다.

**핵심 코드**: `bpf/lib/nat.h`

```c
// bpf/lib/nat.h — NAT 엔트리 구조
struct ipv4_nat_entry {
    struct nat_entry common;  // 생성 시간, 플래그
    union {
        struct {
            __be32 to_saddr;  // 변환된 소스 주소
            __be16 to_sport;  // 변환된 소스 포트
        };
        struct {
            __be32 to_daddr;  // 변환된 목적지 주소 (RevNAT)
            __be16 to_dport;  // 변환된 목적지 포트 (RevNAT)
        };
    };
};

// NAT 타겟 설정
struct ipv4_nat_target {
    __be32 addr;               // 마스커레이드 주소 (보통 노드 IP)
    __u16  min_port;           // 포트 할당 범위 시작
    __u16  max_port;           // 포트 할당 범위 끝
    bool   egress_gateway;     // Egress Gateway 정책 여부
};
```

SNAT 매핑 생성 흐름:

```
1. 패킷 도착 (Pod → 외부)
2. snat_v4_nat_handle_mapping() 호출
   ├── __snat_lookup(): 기존 매핑 확인
   ├── 없으면 snat_v4_new_mapping() 호출
   │   ├── 가용 포트 선택 (try_keep_port → clamp_port_range)
   │   ├── RevSNAT 엔트리 생성 (응답 패킷 매칭용)
   │   ├── SNAT 엔트리 생성 (나가는 패킷 매칭용)
   │   └── 포트 충돌 시 최대 32회 재시도
   └── NAT 변환 적용 (소스 IP/포트 변경)
```

BPF 맵 구조:

```c
// cilium_snat_v4_external (LRU Hash Map)
// Key: ipv4_ct_tuple (5-tuple)
// Value: ipv4_nat_entry
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __type(key, struct ipv4_ct_tuple);
    __type(value, struct ipv4_nat_entry);
    __uint(max_entries, SNAT_MAPPING_IPV4_SIZE);
} cilium_snat_v4_external;
```

Go 측 NAT 맵 관리 (`pkg/maps/nat/nat.go`):

```go
// pkg/maps/nat/nat.go
const (
    MapNameSnat4Global = "cilium_snat_v4_external"
    MapNameSnat6Global = "cilium_snat_v6_external"
)

type Map struct {
    bpf.Map
    family IPFamily
}
```

### 5.2 DNAT (Destination NAT) — 서비스 접근

서비스 VIP:Port를 실제 백엔드 Pod의 IP:Port로 변환한다.
서비스 LB 맵(`cilium_lb4_services_v2`)에서 백엔드를 선택한 후,
NAT 맵에 역방향 매핑(RevNAT)을 기록한다.

```
클라이언트 요청:  10.96.0.1:80  (Service VIP)
          │
          ▼  DNAT
백엔드 전달:     10.0.2.15:8080 (Pod IP)
          │
          ▼  응답 시 RevNAT
클라이언트 응답:  10.96.0.1:80  (원본 복원)
```

### 5.3 NAT46/64

IPv4-only 클라이언트가 IPv6-only 서비스에 접근하거나 그 반대를 지원한다.

**핵심 코드**: `bpf/lib/nat_46x64.h`

```c
// bpf/lib/nat_46x64.h
// IPv4 주소를 IPv6에 매핑하는 두 가지 방식:

// 1. ::FFFF:<IPv4> (IPv4-mapped IPv6)
static __always_inline void
build_v4_in_v6(union v6addr *daddr, __be32 v4)
{
    memset(daddr, 0, sizeof(*daddr));
    daddr->addr[10] = 0xff;
    daddr->addr[11] = 0xff;
    daddr->p4 = v4;
}

// 2. RFC 6052 well-known prefix (64:ff9b::/96)
static __always_inline void
build_v4_in_v6_rfc6052(union v6addr *daddr, __be32 v4)
{
    memset(daddr, 0, sizeof(*daddr));
    daddr->addr[0] = NAT_46X64_PREFIX_0;  // 0x00
    daddr->addr[1] = NAT_46X64_PREFIX_1;  // 0x64
    daddr->addr[2] = NAT_46X64_PREFIX_2;  // 0xff
    daddr->addr[3] = NAT_46X64_PREFIX_3;  // 0x9b
    daddr->p4 = v4;
}

// IPv6 주소에서 IPv4 추출
static __always_inline void
build_v4_from_v6(const union v6addr *v6, __be32 *daddr)
{
    *daddr = v6->p4;
}
```

### 5.4 역방향 NAT (Reverse NAT)

응답 패킷이 돌아올 때 원래 주소를 복원하는 과정:

```c
// bpf/lib/nat.h — 역방향 튜플 설정
static __always_inline void
set_v4_rtuple(const struct ipv4_ct_tuple *otuple,
              const struct ipv4_nat_entry *ostate,
              struct ipv4_ct_tuple *rtuple)
{
    rtuple->flags = TUPLE_F_IN;          // 인그레스 방향
    rtuple->nexthdr = otuple->nexthdr;
    rtuple->saddr = otuple->daddr;       // 원본의 목적지 → 역방향 소스
    rtuple->daddr = ostate->to_saddr;    // NAT된 소스 → 역방향 목적지
    rtuple->sport = otuple->dport;
    rtuple->dport = ostate->to_sport;
}
```

NAT 맵에는 항상 정방향(SNAT) + 역방향(RevSNAT) 쌍이 함께 생성된다:

```
정방향 (Egress):
  Key:   (PodIP:PodPort → ExtIP:ExtPort, OUT)
  Value: (NodeIP:AllocPort)

역방향 (Ingress):
  Key:   (ExtIP:ExtPort → NodeIP:AllocPort, IN)
  Value: (PodIP:PodPort)
```

---

## 6. 네트워크 모드 비교

### 6.1 Tunnel 모드 vs Native Routing vs DSR

| 항목 | Tunnel (VXLAN/Geneve) | Native Routing | DSR (Direct Server Return) |
|------|-----------------------|----------------|----------------------------|
| **캡슐화** | VXLAN 또는 Geneve | 없음 | 요청만 캡슐화 (옵션) |
| **MTU 오버헤드** | 50+ 바이트 | 없음 | 최소 |
| **네트워크 요구사항** | L2/L3 무관 | L3 라우팅 필요 (BGP 또는 클라우드 라우팅) | L2 직접 접근 또는 Geneve |
| **Identity 전달** | 터널 VNI에 포함 | IPCache + 노드 매핑 | Geneve 옵션 또는 IP 옵션 |
| **성능** | 중간 (캡슐화 오버헤드) | 높음 | 매우 높음 (응답 직접 전달) |
| **설정** | `--tunnel-protocol=vxlan` | `--routing-mode=native` | `--bpf-lb-dsr-dispatch=...` |
| **코드 위치** | `bpf/bpf_overlay.c` | `bpf/bpf_host.c` | `bpf/lib/nodeport.h` |

### 6.2 터널 모드의 패킷 흐름

```
[Pod A] → cil_from_container → 라우팅 결정 → 터널 필요
    → encap_and_redirect_with_nodeid() → VXLAN 헤더 추가
    → cil_to_overlay (bpf_overlay.c) → 물리 NIC → 네트워크
    → 수신 노드 물리 NIC → cil_from_overlay
    → get_tunnel_key() → Identity 복원
    → lookup_ip4_endpoint() → 로컬 Pod 탐색
    → ipv4_local_delivery() → cil_to_container → [Pod B]
```

### 6.3 네이티브 라우팅 모드의 패킷 흐름

```
[Pod A] → cil_from_container → 라우팅 결정 → 네이티브 라우팅
    → FIB lookup (fib_lookup()) → 물리 NIC의 다음 홉 결정
    → cil_to_netdev (bpf_host.c) → SNAT (필요 시) → 물리 NIC
    → 네트워크 라우터 (BGP로 학습된 경로 사용)
    → 수신 노드 물리 NIC → cil_from_netdev
    → IPCache에서 Identity 조회 → Pod 전달
```

### 6.4 DSR 모드의 패킷 흐름

```
[Client] → NodePort 요청 → [Node A (LB)]
    → DNAT: Service VIP → Backend Pod (Node B)
    → DSR 정보 인코딩 (Geneve 옵션 또는 IP Option)
    → [Node B]
    → DSR 정보 디코딩 → 원본 클라이언트 주소 복원
    → 응답을 클라이언트에게 직접 전송 (Node A 경유 안 함!)
    → [Client]
```

---

## 7. 소스 파일 참조

### BPF 코드 (C)

| 파일 | 역할 |
|------|------|
| `bpf/bpf_overlay.c` | 오버레이 터널 수신/송신 처리 |
| `bpf/bpf_host.c` | 호스트/네트워크 인터페이스 패킷 처리 |
| `bpf/bpf_lxc.c` | Pod (컨테이너) 인터페이스 패킷 처리 |
| `bpf/bpf_wireguard.c` | WireGuard 인터페이스 패킷 처리 |
| `bpf/lib/encap.h` | VXLAN/Geneve 캡슐화/디캡슐화 |
| `bpf/lib/nat.h` | SNAT/DNAT/RevNAT 엔진 (~63KB) |
| `bpf/lib/nat_46x64.h` | NAT46/64 변환 |
| `bpf/lib/srv6.h` | SRv6 세그먼트 라우팅 |
| `bpf/lib/vtep.h` | VXLAN Tunnel Endpoint 맵 |
| `bpf/lib/arp.h` | ARP 요청/응답 처리 |
| `bpf/lib/mcast.h` | 멀티캐스트/IGMP 처리 |
| `bpf/lib/ipsec.h` | IPsec 마크 처리 |
| `bpf/lib/nodeport.h` | NodePort, DSR, LB 처리 (~84KB) |

### Go 코드 (유저스페이스)

| 패키지 | 역할 |
|--------|------|
| `pkg/datapath/tunnel/` | 터널 설정 관리 (프로토콜, 포트, 언더레이) |
| `pkg/datapath/linux/node.go` | 노드 핸들러 (라우트, 캡슐화 결정) |
| `pkg/datapath/linux/ipsec.go` | IPsec XFRM 설정 |
| `pkg/datapath/linux/ipsec/` | IPsec Agent, XFRM 모니터링 |
| `pkg/wireguard/agent/agent.go` | WireGuard Agent (키 관리, 피어 설정) |
| `pkg/wireguard/types/` | WireGuard 설정 타입 |
| `pkg/bgp/gobgp/server.go` | GoBGP 서버 래퍼 |
| `pkg/bgp/types/bgp.go` | BGP 타입 (Path, Neighbor, Family) |
| `pkg/bgp/manager/manager.go` | BGP 매니저 (Reconciler 조율) |
| `pkg/bgp/manager/reconciler/` | BGP Reconciler들 (Neighbor, PodCIDR, Service) |
| `pkg/maps/nat/` | NAT BPF 맵 Go 래퍼 |
| `pkg/maps/nat/types.go` | NAT 키/값 타입 정의 |
| `pkg/datapath/tables/route.go` | StateDB 기반 라우트 테이블 |
| `pkg/datapath/tables/node_address.go` | 노드 주소 관리 |

---

## 8. 주요 BPF 맵 (네트워킹 관련)

| 맵 이름 | 타입 | 키 | 값 | 용도 |
|---------|------|-----|-----|------|
| `cilium_snat_v4_external` | LRU Hash | 5-tuple | NAT 엔트리 | IPv4 SNAT 매핑 |
| `cilium_snat_v6_external` | LRU Hash | 5-tuple | NAT 엔트리 | IPv6 SNAT 매핑 |
| `cilium_vtep_map` | Hash | VTEP IP | MAC+Tunnel EP | VTEP 매핑 |
| `cilium_ipmasq_v4` | LPM Trie | IP/prefix | 마스커레이드 값 | IP 마스커레이드 예외 |
| `cilium_srv6_vrf_v4/v6` | LPM Trie | VRF+IP | VRF ID | SRv6 VRF 매핑 |
| `cilium_srv6_policy_v4/v6` | LPM Trie | VRF+IP | SRv6 SID | SRv6 정책 |
| `cilium_srv6_sid` | Hash | SID (IPv6) | VRF ID | SRv6 SID 역매핑 |
| `cilium_mcast_group_v4_outer` | Hash-of-Maps | 그룹 IP | Inner Map | 멀티캐스트 그룹 |
| `cilium_tunnel_map` | Hash | IP | Tunnel EP | 터널 엔드포인트 매핑 |
| `cilium_encrypt_state` | Array | 키 인덱스 | 암호화 상태 | IPsec 키 상태 |
