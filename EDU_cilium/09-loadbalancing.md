# 09. Cilium 로드 밸런싱 서브시스템

## 1. 개요

Cilium의 로드 밸런싱(LB)은 eBPF 기반의 고성능 L3/L4 로드 밸런서로, Kubernetes 서비스(ClusterIP, NodePort, LoadBalancer, ExternalIP)를
kube-proxy 없이 처리한다. 핵심 아키텍처는 다음과 같다:

1. **Go 컨트롤 플레인**: Kubernetes API에서 Service/Endpoint를 수집하여 BPF 맵에 기록
2. **BPF 데이터 플레인**: TC, XDP, cgroup/connect 후크에서 패킷/소켓을 인터셉트하여 백엔드 선택 수행
3. **알고리즘**: Random 또는 Maglev 일관 해싱
4. **최적화**: DSR(Direct Server Return), Socket-level LB, XDP 가속

주요 소스 위치:

| 영역 | 경로 |
|------|------|
| Go LB 패키지 | `pkg/loadbalancer/` |
| Maglev 알고리즘 | `pkg/maglev/maglev.go` |
| BPF 맵 타입/조작 | `pkg/loadbalancer/maps/` |
| BPF 리컨실러 | `pkg/loadbalancer/reconciler/bpf_reconciler.go` |
| BPF LB 헤더 | `bpf/lib/lb.h` |
| Socket-level LB | `bpf/bpf_sock.c` |
| XDP 가속 | `bpf/bpf_xdp.c` |
| NodePort/DSR 로직 | `bpf/lib/nodeport.h` |

---

## 2. BPF 맵 구조

Cilium LB는 여러 BPF 맵을 사용하여 서비스 정보를 데이터 플레인에 전달한다.

### 2.1 Service Map (`cilium_lb4_services_v2`)

서비스 프론트엔드를 저장한다. **키-값 구조**:

```
// bpf/lib/lb.h
struct lb4_key {
    __be32 address;       // 서비스 VIP (가상 IP)
    __be16 dport;         // 목적지 포트
    __u16  backend_slot;  // 백엔드 슬롯 (0=프론트엔드 마스터)
    __u8   proto;         // L4 프로토콜
    __u8   scope;         // LB_LOOKUP_SCOPE_EXT / _INT
};

struct lb4_service {
    union {
        __u32 backend_id;       // 슬롯 > 0: 백엔드 ID
        __u32 affinity_timeout; // 슬롯 0: 상위 8비트=알고리즘, 하위 24비트=타임아웃(초)
        __u32 l7_lb_proxy_port; // L7 LB 프록시 포트
    };
    __u16 count;            // 백엔드 슬롯 수
    __u16 rev_nat_index;    // RevNAT 인덱스
    __u8  flags;            // 서비스 플래그 (NodePort, LB, Affinity 등)
    __u8  flags2;           // 추가 플래그 (DSR, TwoScopes 등)
    __u16 qcount;           // 격리(quarantine)된 백엔드 수
};
```

서비스 맵은 **슬롯 기반** 구조를 사용한다:
- `backend_slot=0`: **마스터 엔트리** -- 서비스 메타데이터(백엔드 수, 플래그, 알고리즘)
- `backend_slot=1..N`: **백엔드 슬롯** -- 각 슬롯에 `backend_id` 저장

Go 측 정의: `pkg/loadbalancer/maps/types.go`

```go
// Service4MapV2Name = "cilium_lb4_services_v2"
// Service6MapV2Name = "cilium_lb6_services_v2"

type Service4Key struct {
    Address  types.IPv4
    Port     uint16
    Slot     uint16
    Proto    uint8
    Scope    uint8
    Pad      [2]uint8
}
```

### 2.2 Backend Map (`cilium_lb4_backends_v3`)

실제 백엔드(파드) 주소를 저장한다:

```c
// bpf/lib/lb.h
struct lb4_backend {
    __be32 address;    // 백엔드 IP
    __be16 port;       // 백엔드 포트
    __u8   proto;      // L4 프로토콜
    __u8   flags;      // 상태 (Active/Terminating/Quarantined/Maintenance)
    __u16  cluster_id; // 클러스터 ID (멀티클러스터 구분)
    __u8   zone;       // 토폴로지 영역
};
```

키는 단순한 `__u32 backend_id`이다. Go 측에서는 `BackendID` 타입(`uint32`)을 사용한다.

### 2.3 Reverse NAT Map (`cilium_lb4_reverse_nat`)

응답 패킷의 원래 서비스 VIP를 복원하기 위한 맵이다:

```
키: __u16 rev_nat_index
값: struct lb4_reverse_nat { __be32 address; __be16 port; }
```

SNAT 모드에서 백엔드가 응답할 때, Cilium은 이 맵을 조회하여 소스 주소를 원래 서비스 VIP로 되돌린다.

### 2.4 Affinity Map (`cilium_lb4_affinity`)

Session Affinity(세션 친화성)를 위한 LRU 해시 맵이다:

```c
struct lb4_affinity_key {
    union lb4_affinity_client_id client_id; // 클라이언트 IP 또는 netns cookie
    __u16 rev_nat_id;                       // 서비스 식별자
    __u8  netns_cookie:1;                   // socket cookie 사용 여부
};

struct lb_affinity_val {
    __u64 last_used;    // 마지막 사용 타임스탬프
    __u32 backend_id;   // 고정된 백엔드 ID
};
```

### 2.5 Maglev Map (`cilium_lb4_maglev`)

Maglev 해싱을 위한 **Hash-of-Maps** 구조이다:

```
외부 맵: BPF_MAP_TYPE_HASH_OF_MAPS
  키: __u16 (rev_nat_index)
  값: 내부 맵의 FD

내부 맵: BPF_MAP_TYPE_ARRAY
  키: __u32 (항상 0, 단일 키)
  값: __u32[M] (M = 테이블 크기, 기본 16381개의 backend_id 배열)
```

Go 측에서 `pkg/loadbalancer/maps/lbmaps.go`에서 `UpdateMaglev()`로 내부 배열 맵을 생성하고 외부 맵에 연결한다.

### 2.6 Socket RevNAT Map (`cilium_lb_sock_rev_nat4`)

Socket-level LB에서 `connect()` 시점의 원래 주소를 저장한다:

```go
// pkg/loadbalancer/maps/lbmaps.go
func (r *BPFLBMaps) UpdateSockRevNat(cookie uint64, addr net.IP, port uint16, revNatIndex uint16) error
```

### 2.7 맵 관계 다이어그램

```
[클라이언트 패킷: dst=VIP:port]
        |
        v
  Service Map 조회 (VIP:port:proto -> master entry)
        |
        +-- count, flags, rev_nat_index 획득
        |
        v
  백엔드 선택 (Random 또는 Maglev)
        |
        +-- Random:  slot = random() % count + 1
        |            Service Map 조회 (VIP:port:slot) -> backend_id
        |
        +-- Maglev:  Maglev Map 조회 (rev_nat_index -> LUT)
        |            index = hash(tuple) % M
        |            backend_id = LUT[index]
        |
        v
  Backend Map 조회 (backend_id -> backend_ip:port)
        |
        v
  RevNAT Map 기록 (rev_nat_index -> VIP:port)
        |
        v
  DNAT 수행: dst = backend_ip:port
```

---

## 3. 로드 밸런싱 알고리즘

### 3.1 Random 알고리즘

가장 단순한 방식으로, BPF의 `get_prandom_u32()` 를 사용한다:

```c
// bpf/lib/lb.h -- lb4_select_backend_id_random()
static __always_inline __u32
lb4_select_backend_id_random(struct __ctx_buff *ctx,
                             struct lb4_key *key,
                             const struct ipv4_ct_tuple *tuple,
                             const struct lb4_service *svc)
{
    // 슬롯 0은 프론트엔드이므로 1부터 시작
    __u16 slot = (get_prandom_u32() % svc->count) + 1;
    const struct lb4_service *be = lb4_lookup_backend_slot(ctx, key, slot);
    return be ? be->backend_id : 0;
}
```

**장점**: 구현이 간단하고 오버헤드가 적다
**단점**: 백엔드 추가/제거 시 기존 연결이 깨질 수 있다 (일관성 없음)

설정: `--bpf-lb-algorithm=random` (기본값)

### 3.2 Maglev 일관 해싱

Google Maglev 논문(2016)에 기반한 일관 해싱 알고리즘이다.

#### 3.2.1 알고리즘 원리

1. **룩업 테이블(LUT)**: 크기 M(소수)인 배열. 각 엔트리에 백엔드 ID 저장
2. **순열 생성**: 각 백엔드에 대해 길이 M의 순열(permutation) 생성
   - `offset = hash1(backend) % M`
   - `skip = (hash2(backend) % (M-1)) + 1`
   - `permutation[j] = (offset + j * skip) % M`
3. **테이블 채우기**: 라운드 로빈으로 각 백엔드가 자신의 순열에서 빈 슬롯을 채움
4. **조회**: `hash(5-tuple) % M` 으로 인덱스 계산, LUT에서 백엔드 ID 반환

#### 3.2.2 Go 구현 (pkg/maglev/maglev.go)

```go
// 설정
const DefaultTableSize = 16381  // 소수

// 순열 계산
func getOffsetAndSkip(addr []byte, m uint64, seed uint32) (uint64, uint64) {
    h1, h2 := murmur3.Hash128(addr, seed)
    offset := h1 % m
    skip := (h2 % (m - 1)) + 1
    return offset, skip
}

// 룩업 테이블 생성
func (ml *Maglev) GetLookupTable(backends iter.Seq[BackendInfo]) []loadbalancer.BackendID {
    // 1. 백엔드를 해시 문자열 기준으로 정렬 (클러스터 간 일관성 보장)
    // 2. 순열 계산 (병렬 처리, CPU 코어 수만큼 워커)
    // 3. 라운드 로빈으로 LUT 채우기 (가중치 고려)
    return ml.computeLookupTable()
}
```

#### 3.2.3 가중치 지원

Envoy에서 영감을 받은 가중치 구현. 각 백엔드의 턴(turn)에서 가중치를 확인하여 낮은 가중치의 백엔드는 일부 턴을 건너뛴다:

```go
// 가중치가 사용되는 경우
if weightsUsed {
    if ((n + 1) * uint64(info.Weight)) < uint64(weightCntr[i]) {
        i = (i + 1) % l  // 이 백엔드 건너뛰기
        continue
    }
    weightCntr[i] += float64(weightSum)
}
```

#### 3.2.4 BPF 데이터 플레인 조회

```c
// bpf/lib/lb.h -- lb4_select_backend_id_maglev()
maglev_lut = map_lookup_elem(&cilium_lb4_maglev, &index);  // 외부 맵 조회
backend_ids = map_lookup_elem(maglev_lut, &zero);            // 내부 맵 조회
index = __hash_from_tuple_v4(tuple, sport, dport) % LB_MAGLEV_LUT_SIZE;
return map_array_get_32(backend_ids, index, ...);
```

#### 3.2.5 Maglev의 장점

- **최소 혼란(Minimal Disruption)**: 백엔드 추가/제거 시 영향받는 엔트리가 최소화됨
- **균등 분배**: M이 충분히 크면 (M >= 100*N) 백엔드 간 최대 1% 차이
- **클러스터 간 일관성**: 모든 노드에서 동일한 해시 시드와 백엔드 정렬 순서 사용

설정: `--bpf-lb-algorithm=maglev`

지원 테이블 크기: `[251, 509, 1021, 2039, 4093, 8191, 16381, 32749, 65521, 131071]`

---

## 4. Session Affinity (세션 친화성)

### 4.1 동작 원리

`sessionAffinity: ClientIP`가 설정된 서비스에서, 동일한 클라이언트 IP가 항상 같은 백엔드로 라우팅된다.

**흐름**:

1. 첫 번째 요청: 정상적으로 백엔드 선택 -> Affinity Map에 `{clientIP, svcID} -> {backendID, timestamp}` 기록
2. 이후 요청: Affinity Map 조회 -> 타임아웃 미만이면 저장된 `backendID` 사용
3. 타임아웃 경과: Affinity Map 엔트리 무시, 새 백엔드 선택 후 갱신

**타임아웃 인코딩**:

서비스 맵의 마스터 엔트리에서 `affinity_timeout` 필드가 이중 용도로 사용됨:
- 상위 8비트: LB 알고리즘 (`1=random`, `2=maglev`)
- 하위 24비트: 세션 친화성 타임아웃 (초)

```c
#define LB_ALGORITHM_SHIFT      24
#define AFFINITY_TIMEOUT_MASK   ((1 << LB_ALGORITHM_SHIFT) - 1)
```

### 4.2 Affinity Match Map

`cilium_lb_affinity_match` 맵은 특정 `{backendID, revNatID}` 쌍이 유효한지 확인하는 데 사용된다. 백엔드가 제거될 때 이 맵에서도 삭제되어, 더 이상 유효하지 않은 친화성 엔트리가 무시된다.

```c
struct lb_affinity_match {
    __u32 backend_id;
    __u16 rev_nat_id;
    __u16 pad;
};
```

### 4.3 BPF에서의 처리

```
lb4_local() {
    if (svc에 AFFINITY 플래그 설정) {
        backend_id = lb4_affinity_backend_id_by_addr(svc, client_ip)
        if (backend_id != 0 && affinity_match 맵에 존재) {
            // 기존 백엔드 사용
        } else {
            // 새 백엔드 선택 -> affinity 맵 업데이트
        }
    }
}
```

---

## 5. DSR (Direct Server Return)

### 5.1 개념

일반(SNAT) 모드에서는 응답 패킷이 LB 노드를 다시 통과해야 하지만, DSR 모드에서는 백엔드가 직접 클라이언트에게 응답한다.

```
[SNAT 모드]
Client ---> LB Node ---> Backend
Client <--- LB Node <--- Backend   (응답도 LB 경유)

[DSR 모드]
Client ---> LB Node ---> Backend
Client <---------- Backend         (응답이 LB 우회)
```

### 5.2 DSR Dispatch 모드

세 가지 방식으로 원래 서비스 주소를 백엔드에 전달한다:

| 모드 | 설정값 | 설명 |
|------|--------|------|
| IP Option | `opt` | IPv4 옵션 또는 IPv6 확장 헤더에 원래 VIP:port 인코딩 |
| IPIP | `ipip` | IPIP 터널로 캡슐화하여 백엔드에 전달 |
| Geneve | `geneve` | Geneve 터널 옵션에 DSR 정보 포함 |

설정: `--bpf-lb-mode=dsr` 또는 `--bpf-lb-mode=hybrid` (TCP=DSR, UDP=SNAT)

### 5.3 BPF 데이터 플레인

```c
// bpf/lib/nodeport.h
// DSR 전용 플래그 확인
static __always_inline bool
nodeport_uses_dsr4(const struct lb4_service *svc)
{
    return svc->flags2 & SVC_FLAG_FWD_MODE_DSR;
}
```

DSR이 활성화된 경우:
1. LB 노드에서 패킷을 받으면 백엔드 선택
2. 원래 서비스 VIP:port를 IP 옵션/터널 헤더에 인코딩
3. 백엔드 노드에서 응답 시 소스 주소를 서비스 VIP로 설정하여 직접 클라이언트로 전송

### 5.4 Hybrid 모드

`--bpf-lb-mode=hybrid`는 TCP에는 DSR, UDP에는 SNAT를 적용한다:

```go
// pkg/loadbalancer/loadbalancer.go
func ToSVCForwardingMode(s string, proto ...uint8) SVCForwardingMode {
    switch s {
    case LBModeHybrid:
        if len(proto) > 0 && proto[0] == uint8(u8proto.TCP) {
            return SVCForwardingModeDSR
        }
        return SVCForwardingModeSNAT
    }
}
```

이유: UDP는 상태가 없어 DSR의 이점이 적고, 구현 복잡도가 높기 때문이다.

---

## 6. Socket-level LB

### 6.1 개념

일반적인 패킷 수준 LB에서는 매 패킷마다 DNAT/SNAT가 필요하다. Socket-level LB는 `connect()` 시스콜 시점에 목적지 주소를 백엔드 주소로 변환하여, 이후 패킷에서는 NAT가 불필요하다.

```
[패킷 수준 LB]
connect(VIP:80) -> 소켓의 dst=VIP:80 유지
sendmsg()       -> 매 패킷마다 DNAT(VIP->Backend) + 응답마다 SNAT(Backend->VIP)

[소켓 수준 LB]
connect(VIP:80) -> 소켓의 dst가 Backend:80으로 변경됨
sendmsg()       -> NAT 불필요, 직접 Backend:80으로 전송
```

### 6.2 BPF 구현 (bpf/bpf_sock.c)

`cgroup/connect4` 후크에 연결된 BPF 프로그램이 `connect()` 시스콜을 인터셉트한다:

```c
// bpf/bpf_sock.c
__section("cgroup/connect4")
int cil_sock4_connect(struct bpf_sock_addr *ctx)
{
    err = __sock4_xlate_fwd(ctx, ctx, false, true);
    // ...
}
```

`__sock4_xlate_fwd()` 함수 내부:
1. `ctx->user_ip4`와 `ctx->user_port`에서 목적지 추출
2. Service Map에서 서비스 조회
3. 백엔드 선택 (Random 또는 Maglev)
4. `ctx->user_ip4 = backend->address` 로 주소 변환
5. `ctx_set_port(ctx, backend->port)` 로 포트 변환
6. SockRevNAT 맵에 원래 주소 기록 (getpeername() 복원용)

```c
// 주소 변환 핵심 코드
ctx->user_ip4 = backend->address;
ctx_set_port(ctx, backend->port);
```

### 6.3 UDP sendmsg 처리

UDP는 연결 없는 프로토콜이므로 `sendmsg()` 시점에도 LB가 필요하다:

```c
__section("cgroup/sendmsg4")
int cil_sock4_sendmsg(struct bpf_sock_addr *ctx)
{
    err = __sock4_xlate_fwd(ctx, ctx, true, false);
    // ...
}

__section("cgroup/recvmsg4")
int cil_sock4_recvmsg(struct bpf_sock_addr *ctx)
{
    __sock4_xlate_rev(ctx, ctx);  // 역방향 변환 (원래 VIP로 복원)
}
```

### 6.4 장점

- **성능**: 매 패킷 NAT 제거로 CPU 오버헤드 감소
- **투명성**: 커넥션 트래킹이 불필요 (conntrack 부하 제거)
- **호환성**: 클라이언트 애플리케이션에 투명

---

## 7. 서비스 유형별 처리

### 7.1 ClusterIP

가장 기본적인 서비스 유형. 클러스터 내부에서만 접근 가능한 가상 IP를 할당한다.

- Service Map에 VIP:port -> 백엔드 슬롯 매핑
- Socket-level LB가 주 경로 (connect 시점에 변환)
- `--bpf-lb-external-clusterip=true` 로 외부 접근 허용 가능

### 7.2 NodePort

모든 노드의 특정 포트(기본 30000-32767)에서 서비스 접근 가능:

- Service Map에 `0.0.0.0:NodePort` (와일드카드 주소) 엔트리 등록
- `SVC_FLAG_NODEPORT` 플래그 설정
- TC/XDP 후크에서 처리

```go
// pkg/loadbalancer/config.go
NodePortMinDefault = 30000
NodePortMaxDefault = 32767
```

### 7.3 ExternalIP

외부 IP를 통한 서비스 접근:
- Service Map에 `ExternalIP:port` 엔트리 등록
- `SVC_FLAG_EXTERNAL_IP` 플래그 설정

### 7.4 LoadBalancer

클라우드 로드 밸런서가 할당한 IP를 통한 접근:
- Service Map에 `LoadBalancerIP:port` 엔트리 등록
- `SVC_FLAG_LOADBALANCER` 플래그 설정
- 소스 범위 제한 지원 (`loadBalancerSourceRanges`)
- SourceRange Map (`cilium_lb4_source_range`)으로 LPM Trie 기반 접근 제어

### 7.5 externalTrafficPolicy

`externalTrafficPolicy`에 따른 백엔드 범위 제어:

- **Cluster** (기본): 모든 노드의 백엔드 사용
- **Local**: 로컬 노드의 백엔드만 사용

구현은 `scope` 필드로 분리:

```c
#define LB_LOOKUP_SCOPE_EXT 0  // 외부 트래픽
#define LB_LOOKUP_SCOPE_INT 1  // 내부 트래픽
```

`SVC_FLAG_TWO_SCOPES`가 설정되면 외부/내부 트래픽에 대해 서로 다른 백엔드 셋을 사용한다.

---

## 8. XDP 가속

### 8.1 개념

XDP(eXpress Data Path)는 드라이버 레벨에서 패킷을 처리하여 커널 네트워크 스택 진입 전에 LB를 수행한다. TC 후크보다 빠르다.

### 8.2 구현 (bpf/bpf_xdp.c)

```c
// bpf/bpf_xdp.c
#ifdef ENABLE_NODEPORT_ACCELERATION
__declare_tail(CILIUM_CALL_IPV4_FROM_NETDEV)
int tail_lb_ipv4(struct __ctx_buff *ctx)
{
    // ...
    ret = nodeport_lb4(ctx, ip4, l3_off, UNKNOWN_ID,
                       &punt_to_stack, &ext_err, &is_dsr);
    // ...
}
#endif
```

### 8.3 XDP LB 처리 흐름

```
NIC 수신 -> XDP 프로그램 (tail_lb_ipv4)
  |
  +-> nodeport_lb4()
       |
       +-> Service Map 조회
       +-> 백엔드 선택 (Maglev/Random)
       +-> Backend Map 조회
       +-> DNAT 수행
       +-> DSR 헤더 추가 (DSR 모드)
       +-> XDP_TX (같은 NIC로 전송) 또는 XDP_REDIRECT
```

### 8.4 XDP vs TC

| 특성 | XDP | TC |
|------|-----|-----|
| 처리 시점 | 드라이버 직후 | 네트워크 스택 진입 후 |
| 성능 | 매우 높음 | 높음 |
| skb 유무 | 없음 (xdp_buff) | 있음 (sk_buff) |
| 기능 제한 | 일부 헤더 조작만 가능 | 전체 네트워크 스택 기능 |
| DSR Geneve | 지원 | 지원 |

XDP에서 처리할 수 없는 복잡한 경우(IP 옵션 있는 패킷 등)는 `punt_to_stack=true`로 TC로 폴백된다.

---

## 9. pkg/loadbalancer/ 패키지 구조

```
pkg/loadbalancer/
  |-- loadbalancer.go      # L3n4Addr, ServiceFlags, SVCType 등 핵심 타입
  |-- service.go            # Service 구조체 (세션 친화성, 트래픽 정책)
  |-- backend.go            # Backend, BackendParams 구조체
  |-- frontend.go           # Frontend 구조체 (StateDB 테이블)
  |-- config.go             # LB 설정 (알고리즘, DSR 모드, 맵 크기)
  |-- errors.go             # 에러 정의
  |
  |-- maps/                 # BPF 맵 타입 및 조작
  |   |-- types.go          # ServiceKey/Value, BackendKey/Value 등 BPF 맵 타입
  |   |-- lbmaps.go         # BPFLBMaps 구현 (실제 맵 CRUD)
  |   |-- cell.go           # Hive DI 셀
  |   |-- skiplb.go         # LB 건너뛰기 맵
  |   |-- test_utils.go     # FakeLBMaps (테스트용)
  |
  |-- reconciler/           # StateDB -> BPF 맵 동기화
  |   |-- bpf_reconciler.go # Frontend 리컨실러
  |   |-- id_allocator.go   # ServiceID 할당기
  |   |-- cell.go           # Hive DI 셀
  |   |-- termination.go    # 소켓 종료 처리
  |
  |-- writer/               # StateDB 테이블 쓰기
  |   |-- writer.go         # Service/Frontend/Backend 업서트/삭제
  |   |-- zones.go          # 토폴로지 인식 라우팅
  |
  |-- reflectors/           # K8s -> StateDB 변환
  |   |-- k8s.go            # K8s Service/Endpoint 리플렉터
  |   |-- conversions.go    # K8s 오브젝트 -> LB 타입 변환
  |
  |-- cell/                 # 최상위 Hive 셀
  |   |-- cell.go           # LB 서브시스템 셀 등록
  |
  |-- redirectpolicy/       # 로컬 리다이렉트 정책
  |-- healthserver/         # 헬스체크 서버
```

### 9.1 데이터 흐름

```
K8s API Server
     |
     v
reflectors/k8s.go        -- K8s Service/Endpoint 감시
     |
     v
writer/writer.go          -- StateDB에 Service/Frontend/Backend 기록
     |
     v
StateDB Tables            -- services, frontends, backends
     |
     v
reconciler/bpf_reconciler.go  -- StateDB 변경 감지 -> BPF 맵 업데이트
     |
     v
maps/lbmaps.go            -- BPF 맵 CRUD 실행
     |
     v
BPF Maps (커널)            -- cilium_lb4_services_v2, cilium_lb4_backends_v3, ...
```

---

## 10. 백엔드 상태 관리

### 10.1 상태 전이

```go
// pkg/loadbalancer/loadbalancer.go
const (
    BackendStateActive               // 정상, 트래픽 수신
    BackendStateTerminating          // 종료 중, 폴백으로 사용 가능
    BackendStateTerminatingNotServing // 종료 중, 폴백 불가
    BackendStateQuarantined          // 격리됨, 헬스체크 가능
    BackendStateMaintenance          // 유지보수, 헬스체크/트래픽 모두 차단
)
```

상태 전이:
- Active -> Terminating, Quarantined, Maintenance
- Quarantined -> Active, Terminating
- Maintenance -> Active

### 10.2 격리(Quarantine) 슬롯

서비스 맵에서 격리된 백엔드는 별도의 `qcount` 영역에 배치된다. 정상 백엔드가 모두 사라진 경우에만 폴백으로 사용된다.

---

## 11. 서비스별 알고리즘 어노테이션

`--bpf-lb-algorithm-annotation=true` 설정 시, 서비스 어노테이션으로 개별 서비스의 LB 알고리즘을 지정할 수 있다:

```yaml
apiVersion: v1
kind: Service
metadata:
  annotations:
    service.cilium.io/lb-algorithm: "maglev"  # 또는 "random"
```

BPF에서는 마스터 엔트리의 `affinity_timeout` 상위 8비트에 알고리즘이 인코딩된다:

```c
#define LB_ALGORITHM_SHIFT 24
// alg = svc->affinity_timeout >> LB_ALGORITHM_SHIFT
```

`LB_SELECTION_PER_SERVICE` 매크로가 정의되면 `lb4_select_backend_id()`에서 서비스별 알고리즘 분기가 활성화된다.

---

## 12. 정리

| 기능 | 설정 | BPF 맵 | 핵심 파일 |
|------|------|--------|----------|
| Random LB | `--bpf-lb-algorithm=random` | Service Map | `bpf/lib/lb.h` |
| Maglev LB | `--bpf-lb-algorithm=maglev` | Maglev Map | `pkg/maglev/maglev.go` |
| Session Affinity | `sessionAffinity: ClientIP` | Affinity Map, AffinityMatch Map | `bpf/lib/lb.h` |
| DSR | `--bpf-lb-mode=dsr` | Service Map (flags) | `bpf/lib/nodeport.h` |
| Socket-level LB | 자동 (KPR 활성 시) | SockRevNAT Map | `bpf/bpf_sock.c` |
| XDP 가속 | `ENABLE_NODEPORT_ACCELERATION` | 모든 LB 맵 | `bpf/bpf_xdp.c` |
| NodePort | `type: NodePort` | Service Map | `bpf/lib/nodeport.h` |
| LoadBalancer | `type: LoadBalancer` | Service Map, SourceRange Map | `bpf/lib/lb.h` |
