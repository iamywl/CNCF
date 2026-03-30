# PoC-24: Socket 기반 로드밸런싱 시뮬레이션

## 개요

이 PoC는 Cilium의 **Socket 레벨 로드밸런싱(Socket LB)** 메커니즘을 시뮬레이션한다.
Cilium은 cgroup eBPF 프로그램을 통해 `connect()`, `sendmsg()`, `recvmsg()`, `getpeername()`, `bind()` 등의
소켓 시스템 콜을 가로채어, 커널 네트워크 스택(iptables/conntrack)을 거치지 않고 소켓 수준에서 직접
서비스 VIP를 백엔드 IP로 변환한다. 이 PoC는 해당 동작 원리를 Go 표준 라이브러리만으로 재현한다.

실제 소스: `pkg/socketlb/socketlb.go`, `pkg/socketlb/cgroup.go`

## 배경 지식

### 기존 iptables/TC 기반 로드밸런싱의 한계

전통적인 Kubernetes 환경에서 kube-proxy는 iptables 규칙을 사용하여 서비스 VIP를 백엔드 Pod IP로 변환한다.
이 방식은 패킷이 커널 네트워크 스택의 netfilter 체인을 순차적으로 통과해야 하며, 각 연결마다
conntrack(CT) 엔트리를 생성/조회해야 한다. 서비스 수가 많아지면 iptables 규칙 수가 O(n)으로 증가하고,
conntrack 테이블 경합이 발생하여 성능이 크게 저하된다.

### Socket LB의 원리

Socket LB는 이 문제를 근본적으로 다른 계층에서 해결한다. cgroup에 eBPF 프로그램을 부착하여
소켓 시스템 콜 자체를 가로채는 방식이다.

```
[기존 iptables 방식]
  App → connect(VIP:80) → TCP SYN → netfilter/iptables → DNAT → CT 생성 → 백엔드

[Socket LB 방식]
  App → connect(VIP:80) → cgroup BPF 훅 → VIP→백엔드 변환 → connect(백엔드:8080)
                          (커널 네트워크 스택 우회)
```

핵심 차이점:
- **패킷 변환 불필요**: 패킷이 생성되기 전에 소켓 수준에서 목적지 주소를 변경
- **conntrack 불필요**: DNAT가 없으므로 CT 엔트리 생성/조회 오버헤드 제거
- **O(1) 조회**: eBPF 맵 기반 서비스 룩업으로 서비스 수에 무관한 성능

### cgroup BPF 훅 종류

Cilium은 13개의 cgroup BPF 프로그램을 정의한다 (`socketlb.go:27-39`):

| 훅 | 용도 | 프로토콜 |
|----|------|---------|
| `cil_sock4_connect` | TCP/UDP connect() 시 VIP→백엔드 변환 | IPv4 |
| `cil_sock4_sendmsg` | UDP sendmsg() 시 매 패킷 VIP→백엔드 변환 | IPv4 |
| `cil_sock4_recvmsg` | UDP recvmsg() 시 백엔드→VIP 역변환 | IPv4 |
| `cil_sock4_getpeername` | getpeername() 시 원래 VIP 반환 | IPv4 |
| `cil_sock4_post_bind` | NodePort 범위 포트 바인드 차단 | IPv4 |
| `cil_sock4_pre_bind` | Health 데이터패스용 바인드 훅 | IPv4 |
| `cil_sock_release` | 소켓 해제 시 LB 상태 정리 | 공통 |

IPv6용(`cil_sock6_*`) 프로그램도 동일한 구조로 6개가 추가로 존재한다.

### 부착 방식: bpf_link vs PROG_ATTACH

커널 5.7 이상에서는 `bpf_link`를 사용하여 프로그램을 cgroup에 부착한다 (`cgroup.go:8-19`).
이 방식은 링크를 bpffs에 핀하여 프로세스 종료 후에도 유지되며, `link.Update()`로 원자적 교체가 가능하다.
이전 버전에서는 `PROG_ATTACH` API를 사용하며, 업그레이드 시 호환성을 위해 기존 방식을 유지한다.

## 시뮬레이션하는 개념

| 개념 | 실제 Cilium 코드 | 시뮬레이션 방식 |
|------|-----------------|---------------|
| 13개 cgroup BPF 프로그램 | `socketlb.go:27-39` cgroupProgs 상수 | `CgroupProgram` 구조체 + Enable() 활성화 |
| connect() 훅 DNAT | `bpf_sock.c` sock4_xlate_fwd | `Connect()` - 서비스 맵 룩업 후 백엔드 반환 |
| UDP sendmsg/recvmsg 변환 | `bpf_sock.c` sock4_xlate_snd/rcv | `SendMsg()`/`RecvMsg()` - 매 패킷 변환/역변환 |
| getpeername 역변환 | `bpf_sock.c` sock4_getpeername | `GetPeerName()` - LB 상태에서 원래 VIP 복원 |
| NodePort 바인드 보호 | `socketlb.go:120-121` PostBind4 | `PostBind()` - 포트 범위 검사 후 차단 |
| bpf_link 부착 | `cgroup.go:59` attachCgroup | `Enable()` - Mode="bpf_link" 설정 |
| 서비스 맵 | eBPF LB4 서비스 맵 | `ServiceMap` - Go map 기반 VIP→백엔드 매핑 |
| 소켓 LB 상태 | eBPF sock_cookie 기반 상태 | `sockStates` map[fd]*SockLBState |

## 아키텍처 다이어그램

```
+------------------+     +-----------------+     +------------------+
|   Application    |     |   ServiceMap    |     |    Backends      |
|                  |     |  (eBPF Map)     |     |                  |
|  connect(VIP:80) |     | VIP:80 →        |     | 10.244.1.5:8080  |
|  sendmsg(VIP:53) |     |   [B1, B2, B3]  |     | 10.244.2.10:8080 |
|  getpeername(fd) |     | VIP:53 →        |     | 10.244.3.15:8080 |
+--------+---------+     |   [B4, B5]      |     | 10.244.0.100:53  |
         |               +--------+--------+     | 10.244.0.101:53  |
         v                        |               +------------------+
+--------+---------+              |
|  cgroup BPF Hook |              |
|                  |   Lookup     |
|  connect4 -------+------------->+
|  sendmsg4 ------+               |
|  recvmsg4 <-----+ (역변환)      |
|  getpeername4 <--+ (VIP 복원)   |
|  post_bind4 -----> NodePort 검사|
|  sock_release ---> 상태 정리    |
+----------------------------------+

TCP 흐름:                         UDP 흐름:
  connect(VIP) → 백엔드 변환       sendmsg(VIP) → 백엔드 변환 (매 패킷)
  이후 모든 패킷 자동 라우팅        recvmsg(백엔드) → VIP 역변환 (매 패킷)
  getpeername() → VIP 반환         (연결 상태 없으므로 매번 변환 필요)
```

## 코드 해설

### 1. `SocketLB` 구조체 (92-113행)

Socket LB 엔진의 핵심 상태를 관리하는 구조체다. `services`는 eBPF 서비스 맵을 대체하며,
`sockStates`는 소켓 fd별 LB 상태(원래 VIP 정보)를 저장한다. `programs`는 13개 cgroup BPF
프로그램의 활성화/부착 상태를 추적한다.

실제 Cilium에서는 이 상태들이 eBPF 맵(`lb4_services_v2`, `lb4_backends_v3`)과
커널 cgroup 프로그램으로 존재한다. PoC에서는 Go의 `sync.Mutex` + map으로 동시성 안전한
in-memory 구현을 사용한다.

### 2. `Enable()` 메서드 (145-183행)

`pkg/socketlb/socketlb.go`의 `Enable()` 함수(68행)를 모방한다.
IPv4/IPv6 설정, Peer 지원, 바인드 보호 옵션에 따라 필요한 프로그램만 선택적으로 활성화한다.
실제 코드의 `enabled` 맵 패턴(106-151행)을 그대로 따르며, 활성화된 프로그램은
`attachCgroup()`으로 cgroup에 부착된다. PoC에서는 `Attached=true`, `Mode="bpf_link"`
플래그 설정으로 이를 표현한다.

### 3. `Connect()` 메서드 (186-212행)

`cil_sock4_connect` BPF 프로그램의 동작을 시뮬레이션한다. 목적지 IP:Port로 서비스 맵을
조회하고, 서비스가 존재하면 백엔드 중 하나를 선택하여 반환한다. 동시에 `sockStates`에
원래 VIP 정보를 저장하여, 이후 `getpeername()` 호출 시 역변환할 수 있게 한다.

실제 Cilium에서는 Maglev 해싱으로 백엔드를 선택하지만, PoC에서는 `rand.Intn()`으로
단순화했다. 핵심은 "패킷 생성 전에 소켓 수준에서 주소를 변환한다"는 원리의 시연이다.

### 4. `SendMsg()`/`RecvMsg()` 메서드 (215-247행)

UDP 트래픽을 위한 훅이다. TCP는 `connect()` 시점에 한 번만 변환하면 이후 모든 패킷이
자동으로 백엔드로 향하지만, UDP는 비연결형이므로 `sendmsg()` 호출마다 매번 변환해야 한다.
`recvmsg()`는 반대 방향으로, 백엔드 IP를 서비스 VIP로 역변환하여 애플리케이션이
일관된 소스 주소를 보게 한다. 이 TCP/UDP 처리 차이가 Socket LB 설계의 핵심 포인트다.

### 5. `PostBind()` 메서드 (265-275행)

NodePort 포트 범위(기본 30000-32767) 보호 기능을 시뮬레이션한다.
사용자 애플리케이션이 NodePort 범위의 포트에 `bind()`를 시도하면 차단하여,
Kubernetes NodePort 서비스와의 포트 충돌을 방지한다.
실제 Cilium에서는 `KubeProxyReplacement && NodePortBindProtection` 옵션이
모두 활성화된 경우에만 `PostBind4`/`PostBind6` 프로그램이 부착된다 (`socketlb.go:120-121`).

## 실행 방법 및 예상 출력

```bash
cd cilium_EDU/poc-24-socket-lb
go run main.go
```

주요 출력 발췌:

```
[1] cgroup BPF 프로그램 활성화
--------------------------------------------------
  cil_sock4_connect              enabled (bpf_link)
  cil_sock4_sendmsg              enabled (bpf_link)
  cil_sock4_recvmsg              enabled (bpf_link)
  cil_sock4_getpeername          enabled (bpf_link)
  ...

[2] TCP connect() - 서비스 VIP -> 백엔드 변환
--------------------------------------------------
  fd=100: connect(10.96.0.1:80) -> connect(10.244.2.10:8080)
  fd=101: connect(10.96.0.1:80) -> connect(10.244.1.5:8080)
  ...
  fd=200: connect(8.8.8.8:443) -> connect(8.8.8.8:443) (서비스 아님, 변환 없음)

[3] getpeername() - 백엔드 IP -> 서비스 VIP 역변환
--------------------------------------------------
  fd=100: getpeername() -> 10.96.0.1:80 (원래 서비스 VIP)

[4] UDP sendmsg/recvmsg - 매 패킷 변환
--------------------------------------------------
  sendmsg #1: 10.96.0.10:53 -> 10.244.0.100:53
  recvmsg #1: 10.244.0.100:53 -> 10.96.0.10:53 (앱이 보는 소스)

[5] NodePort 바인드 보호 (post_bind)
--------------------------------------------------
  bind(:8080)  -> ALLOWED
  bind(:30080) -> BLOCKED: port 30080 is in NodePort range [30000-32767]
  bind(:9090)  -> ALLOWED

[7] 성능 비교: Socket LB vs iptables 시뮬레이션
--------------------------------------------------
  Socket LB:  500000 ops / ~XXms
  iptables:   500000 ops / ~XXms (CT + DNAT 오버헤드)
```

## 핵심 포인트

1. **소켓 수준 DNAT의 원리**: 패킷이 커널 네트워크 스택에 진입하기 전에 소켓의 목적지 주소를
   직접 변경한다. 이로써 netfilter/iptables 규칙 순회와 conntrack 엔트리 생성이 완전히
   불필요해지며, 서비스 수에 무관한 O(1) 성능을 달성한다.

2. **TCP와 UDP의 처리 차이**: TCP는 연결 지향적이므로 `connect()` 시점에 한 번만 변환하면
   되지만, UDP는 비연결형이므로 `sendmsg()`/`recvmsg()` 매 호출마다 변환/역변환이 필요하다.
   이 차이가 별도의 BPF 훅이 필요한 이유다.

3. **getpeername() 투명성**: 애플리케이션이 `getpeername()`을 호출하면 실제 백엔드 IP가 아닌
   원래 서비스 VIP를 반환한다. 이를 통해 애플리케이션은 로드밸런싱이 일어났다는 사실을
   전혀 인식하지 못하며, 서비스 디스커버리 로직에 영향을 주지 않는다.

4. **NodePort 바인드 보호**: Kubernetes NodePort 범위(30000-32767)에 대한 사용자 바인드를
   커널 수준에서 차단하여 포트 충돌을 원천 방지한다. kube-proxy 없이 Cilium이 NodePort를
   완전히 대체할 때 필수적인 안전장치다.

5. **13개 프로그램의 선택적 활성화**: IPv4/IPv6, Peer 지원, 바인드 보호, Health 데이터패스
   등 설정 조합에 따라 필요한 프로그램만 활성화한다. 불필요한 훅은 부착하지 않아
   오버헤드를 최소화한다.

## 실제 Cilium과의 차이점

| 항목 | 실제 Cilium | 이 PoC |
|------|-----------|--------|
| BPF 프로그램 | C로 작성된 eBPF 프로그램이 커널에서 실행 | Go 함수로 시뮬레이션 |
| 서비스 맵 | eBPF 맵 (`lb4_services_v2`, `lb4_backends_v3`) | Go map 기반 `ServiceMap` |
| 백엔드 선택 | Maglev 일관적 해싱 | `rand.Intn()` 랜덤 선택 |
| cgroup 부착 | `bpf_link` 또는 `PROG_ATTACH` API로 커널에 부착 | `Attached=true` 플래그 설정 |
| 소켓 상태 추적 | `sock_cookie` 기반 eBPF 맵 | `map[int]*SockLBState` (fd 키) |
| 세션 어피니티 | 클라이언트 IP 기반 sticky session 지원 | 미구현 |
| Health 체크 | 백엔드 health 상태에 따른 자동 제거 | 미구현 (항상 모든 백엔드 사용) |
| IPv6 처리 | IPv4/IPv6 별도 BPF 프로그램 실제 실행 | 프로그램 이름만 등록, 실제 IPv6 로직 없음 |
| conntrack 연동 | CT 맵과의 연동으로 역변환 수행 | `sockStates` 맵으로 단순화 |
| 성능 비교 | 실제 netfilter 우회로 측정 가능한 성능 차이 | 인위적 오버헤드 시뮬레이션 (정확한 비교 아님) |
