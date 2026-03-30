# PoC-23: Cilium XDP 초고속 패킷 처리 시뮬레이션

## 개요

이 PoC는 Cilium의 XDP(eXpress Data Path) 계층에서 수행되는 초고속 패킷 처리 파이프라인을 Go 표준 라이브러리만으로 시뮬레이션한다. CIDR 프리필터를 통한 DDoS 방어, NodePort 로드밸런싱 가속, 그리고 여러 기능이 XDP 모드를 요구할 때 발생하는 충돌을 해결하는 Enabler 패턴까지 재현한다.

실제 소스 위치: `bpf/bpf_xdp.c`, `pkg/datapath/xdp/xdp.go`

## 배경 지식

### XDP란 무엇인가

XDP(eXpress Data Path)는 Linux 커널의 네트워크 스택에서 가장 이른 시점에 패킷을 처리할 수 있는 BPF 훅이다. NIC 드라이버가 패킷을 수신하자마자, `sk_buff` 구조체가 할당되기 **전에** BPF 프로그램이 실행된다. 이로 인해 커널 네트워크 스택 전체를 우회할 수 있어 극도로 낮은 레이턴시와 높은 처리량을 달성한다.

### TC 훅과의 차이점

| 항목 | XDP | TC (Traffic Control) |
|------|-----|---------------------|
| 실행 시점 | sk_buff 할당 전 (드라이버 레벨) | sk_buff 할당 후 |
| 성능 | ~14M pps (native 모드) | ~4M pps |
| 접근 가능 메타데이터 | 제한적 (raw 패킷만) | 풍부 (sk_buff 필드 전체) |
| 판정 | PASS, DROP, TX, REDIRECT | OK, SHOT, REDIRECT 등 |
| 용도 | DDoS 방어, LB 가속 | 정책 적용, 캡슐화 |

### 왜 초고속 패킷 처리가 필요한가

- **DDoS 방어**: 악의적 트래픽을 커널 스택에 도달하기 전에 차단하여 CPU 소비를 최소화
- **로드밸런싱 가속**: NodePort 트래픽을 XDP 레벨에서 DNAT 처리하여 kube-proxy 대비 수배 빠른 성능
- **자원 절약**: sk_buff 할당, 메모리 복사, 프로토콜 파싱 등 커널 스택 오버헤드를 완전 회피

## 시뮬레이션하는 개념

| 개념 | 실제 Cilium 코드 | 시뮬레이션 방식 |
|------|-----------------|----------------|
| XDP 판정 (PASS/DROP/TX/REDIRECT) | `bpf/bpf_xdp.c:cil_xdp_entry()` | `XDPVerdict` 열거형 + `ProcessPacket()` 반환값 |
| CIDR 프리필터 (Hash Map) | `BPF_MAP_TYPE_HASH` 기반 `/32` 필터 | `HashFilter` 구조체 (`map[uint32]bool`) |
| CIDR 프리필터 (LPM Trie) | `BPF_MAP_TYPE_LPM_TRIE` 기반 CIDR 필터 | `LPMTrie` 구조체 (슬라이스 순회 매칭) |
| NodePort XDP 가속 | `check_v4_lb()` → `nodeport_lb4()` | `NodePortLB` 구조체 (포트 → 백엔드 매핑) |
| Enabler 패턴 (모드 해결) | `pkg/datapath/xdp/xdp.go:newConfig()` | `ResolveMode()` 함수 (우선순위 + 충돌 감지) |
| XDP 가속 모드 | native/best-effort/generic/disabled | `AccelerationMode` 상수 |

## 아키텍처 다이어그램

```
패킷 수신 (NIC)
       │
       ▼
┌─────────────────────────────────────────────┐
│              XDP BPF 프로그램                 │
│            (cil_xdp_entry)                   │
│                                              │
│  ┌────────────────────────────┐              │
│  │     CIDR 프리필터           │              │
│  │  ┌──────────┬───────────┐  │              │
│  │  │ Hash Map │ LPM Trie  │  │              │
│  │  │  (/32)   │ (CIDR)    │  │              │
│  │  └────┬─────┴─────┬─────┘  │              │
│  │       │ 매치?     │ 매치?  │              │
│  │       ▼           ▼        │              │
│  │      XDP_DROP   XDP_DROP   │              │
│  └────────────┬───────────────┘              │
│               │ 통과                          │
│               ▼                               │
│  ┌────────────────────────────┐              │
│  │    NodePort LB 검사         │              │
│  │    (check_v4_lb)            │              │
│  │    port → backend 매핑      │              │
│  └────────┬───────┬───────────┘              │
│    매치됨 │       │ 매치 없음                 │
│           ▼       ▼                           │
│       XDP_TX   XDP_PASS                      │
│     (DNAT+반송)  (커널 스택 전달)              │
└─────────────────────────────────────────────┘
       │               │
       ▼               ▼
  동일 NIC로 반송    커널 네트워크 스택
                    (TC → bpf_host.c)
```

## 코드 해설

### 1. LPMTrie - Longest Prefix Match 트라이

```go
type LPMTrie struct {
    mu      sync.RWMutex
    entries []LPMTrieKey
}
```

CIDR 범위 기반 IP 매칭을 수행한다. 실제 Cilium에서는 커널의 `BPF_MAP_TYPE_LPM_TRIE` 맵 타입을 사용하여 O(W) 복잡도(W=비트 너비)로 가장 긴 프리픽스를 매칭한다. 시뮬레이션에서는 슬라이스를 순회하며 비트마스크 비교를 수행하는 방식으로 단순화했다. 핵심은 `(ip & mask) == (entry.Addr & mask)` 비교 로직으로, 이것이 LPM 매칭의 본질이다.

### 2. CIDRPrefilter - 이중 필터 구조

```go
type CIDRPrefilter struct {
    fixMap *HashFilter  // /32 전용 O(1) 조회
    dynMap *LPMTrie     // 가변 CIDR 범위
}
```

Cilium의 `prefilter_v4`를 모방했다. `/32` 주소는 Hash Map으로 O(1) 조회하고, 가변 길이 CIDR은 LPM Trie로 처리하는 이중 구조이다. 이렇게 분리한 이유는 정확한 IP 차단이 훨씬 빈번하므로 해시맵의 O(1) 성능을 활용하고, CIDR 범위는 상대적으로 적으므로 LPM의 약간의 오버헤드를 감수하는 전략이다.

### 3. XDPProgram.ProcessPacket - 핵심 패킷 처리 경로

```go
func (xdp *XDPProgram) ProcessPacket(pkt Packet) (XDPVerdict, string) {
```

`bpf/bpf_xdp.c`의 `cil_xdp_entry()` 함수를 시뮬레이션한다. 실제 XDP 프로그램과 동일한 순서로 동작한다: (1) 프리필터로 악성 IP 차단, (2) NodePort LB로 서비스 트래픽 가속, (3) 나머지는 커널 스택으로 전달. `atomic.Int64`로 통계를 관리하는 부분은 실제 BPF 맵의 per-CPU 카운터를 반영한 것이다.

### 4. ResolveMode - Enabler 패턴의 모드 충돌 해결

```go
func ResolveMode(enablers []XDPEnabler) (AccelerationMode, error) {
```

`pkg/datapath/xdp/xdp.go`의 `newConfig()` 로직을 재현한다. NodePort, Prefilter, DSR 등 여러 기능이 각각 XDP 가속 모드를 요구할 때, 모드 간 우선순위(native > best-effort)와 충돌(native vs generic)을 해결한다. 하나의 NIC에는 하나의 XDP 프로그램만 로드 가능하므로, 이 조율이 필수적이다.

### 5. NodePortLB - XDP 레벨 로드밸런서

```go
type NodePortLB struct {
    mu       sync.RWMutex
    services map[uint16]*NodePortService
}
```

Cilium의 `nodeport_lb4()` 로직을 단순화했다. 포트 번호로 서비스를 찾고, 백엔드 중 하나를 선택하여 DNAT를 수행한다. 실제 Cilium은 Maglev 일관성 해싱으로 백엔드를 선택하지만, 시뮬레이션에서는 랜덤 선택으로 대체했다.

## 실행 방법 및 예상 출력

```bash
cd cilium_EDU/poc-23-xdp
go run main.go
```

주요 출력 발췌:

```
[1] CIDR 프리필터 (Hash + LPM Trie)
--------------------------------------------------
  10.0.0.100         → XDP_DROP       # Hash Map 매치
  10.0.0.101         → XDP_PASS       # 차단 대상 아님
  172.16.5.1         → XDP_DROP       # LPM Trie 매치 (172.16.0.0/12)
  172.32.1.1         → XDP_PASS       # CIDR 범위 밖
  203.0.113.42       → XDP_DROP       # LPM Trie 매치 (203.0.113.0/24)
  8.8.8.8            → XDP_PASS       # 허용

[2] NodePort 가속 (XDP 레벨 LB)
--------------------------------------------------
  1.2.3.4:50000 → 192.168.1.1:30080 (TCP)
    → XDP_TX (NodePort LB → 10.244.x.x:8080)
  203.0.113.42:50002 → 192.168.1.1:30080 (TCP)
    → XDP_DROP (CIDR prefilter blocked)
  9.9.9.9:50003 → 192.168.1.1:80 (TCP)
    → XDP_PASS (passed to kernel stack)

  통계: Total=5, PASS=1, DROP=2, TX=2

[3] Enabler 패턴 (모드 충돌 해결)
--------------------------------------------------
  시나리오 1 (NodePort=native, Prefilter=native): native (err=<nil>)
  시나리오 2 (NodePort=best-effort, DSR=native): native (err=<nil>)
  시나리오 3 (NodePort=native, Testing=generic): disabled (err=충돌 에러)
  시나리오 4 (WireGuard=disabled, NodePort=native): native (err=<nil>)

[4] 처리량 벤치마크 (XDP vs 커널스택)
--------------------------------------------------
  XDP 시뮬레이션: 1000000 패킷 / ~XXXms = ~XXXXX pps
```

## 핵심 포인트

1. **sk_buff 이전 처리의 위력**: XDP는 커널이 패킷 메타데이터 구조체(sk_buff)를 할당하기 전에 실행되므로, 메모리 할당과 프로토콜 파싱 비용을 완전히 회피한다. 이것이 TC 대비 3배 이상 빠른 근본적 이유다.

2. **이중 필터 전략**: 정확한 IP(`/32`)는 Hash Map으로 O(1) 조회하고, 가변 CIDR은 LPM Trie로 처리하는 구조는 실무에서 DDoS 방어 시 블랙리스트와 CIDR 범위 차단을 동시에 효율적으로 수행하는 패턴이다.

3. **XDP 판정의 의미**: `XDP_DROP`은 패킷을 즉시 폐기(DDoS 방어), `XDP_TX`는 동일 NIC로 반송(헤어핀 LB), `XDP_REDIRECT`는 다른 NIC로 전달, `XDP_PASS`는 커널 스택으로 넘기는 것이다. 이 네 가지 판정이 XDP의 전체 동작을 결정한다.

4. **Enabler 패턴의 필요성**: Linux에서 NIC 하나당 XDP 프로그램은 하나만 로드할 수 있다. Cilium은 NodePort, Prefilter, DSR 등 여러 기능을 하나의 XDP 프로그램에 통합하되, 각 기능이 요구하는 가속 모드 간 충돌을 Enabler 패턴으로 해결한다.

5. **NodePort 가속의 원리**: kube-proxy는 iptables/IPVS를 사용하여 커널 스택 내에서 NodePort 트래픽을 처리하지만, Cilium은 XDP 레벨에서 DNAT 후 `XDP_TX`로 패킷을 즉시 반송하여 커널 스택을 완전히 우회한다.

## 실제 Cilium과의 차이점

| 항목 | 실제 Cilium | 이 시뮬레이션 |
|------|------------|-------------|
| 실행 환경 | eBPF 바이트코드, NIC 드라이버 컨텍스트 | Go 유저스페이스 |
| LPM Trie | 커널 `BPF_MAP_TYPE_LPM_TRIE` (O(W), W=32) | 슬라이스 순회 (O(N)) |
| Hash Map | 커널 `BPF_MAP_TYPE_HASH` (per-CPU, lockless) | `sync.RWMutex` + Go map |
| LB 알고리즘 | Maglev 일관성 해싱 | 랜덤 선택 |
| 패킷 데이터 | `xdp_md` 구조체 (raw 패킷 포인터) | Go `Packet` 구조체 |
| 통계 카운터 | per-CPU BPF 맵 (lockless) | `atomic.Int64` |
| NIC 통합 | 실제 NIC 드라이버에 로드 | 시뮬레이션만 수행 |
| REDIRECT | `bpf_redirect_map()`으로 다른 NIC 전달 | 미구현 |
| 메타데이터 전달 | `xdp_meta` → TC로 메타 전달 | 미구현 |
| 성능 | ~14M pps (native 모드) | Go 오버헤드로 인해 수십만 pps 수준 |
