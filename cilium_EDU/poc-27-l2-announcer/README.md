# PoC-27: Cilium L2 Announcer 시뮬레이션

## 개요

이 PoC는 Cilium의 **L2 Announcer** 기능을 시뮬레이션한다.
L2 Announcer는 Kubernetes LoadBalancer 서비스에 할당된 외부 IP를 L2(Layer 2) 네트워크에서
도달 가능하게 만드는 메커니즘이다. 리더 선출, Gratuitous ARP(GARP) 전송, ARP 요청 응답,
IPv6 Neighbor Advertisement, 그리고 노드 장애 시 Failover까지 전체 흐름을 재현한다.

외부 의존성 없이 Go 표준 라이브러리만으로 구현되어 `go run main.go`로 즉시 실행 가능하다.

---

## 배경 지식

### L2 Announcer란

클라우드 환경에서는 LoadBalancer 타입 서비스를 생성하면 클라우드 프로바이더가 외부 IP를 할당하고
라우팅을 자동 설정한다. 그러나 베어메탈이나 온프레미스 환경에서는 이런 인프라가 없다.

Cilium L2 Announcer는 이 문제를 해결한다. LoadBalancer 서비스에 할당된 IP 주소에 대해
ARP/NDP 응답을 생성하여, 같은 L2 네트워크 세그먼트에 있는 라우터와 호스트가 해당 IP로
트래픽을 전달할 수 있게 한다. MetalLB의 L2 모드와 유사한 역할이다.

### ARP/NDP를 통한 IP 광고 원리

**ARP (Address Resolution Protocol, IPv4)**:
- 네트워크 장비가 특정 IP의 MAC 주소를 알아내기 위해 ARP Request를 브로드캐스트한다.
- L2 Announcer의 리더 노드가 서비스 IP에 대한 ARP Request를 받으면, 자신의 MAC 주소로 응답한다.
- 이후 트래픽은 해당 노드의 MAC으로 전달되고, 노드에서 서비스 엔드포인트로 분배된다.

**Gratuitous ARP (GARP)**:
- 요청 없이 자발적으로 전송하는 ARP Reply이다.
- SenderIP와 TargetIP가 동일한 것이 특징이다.
- 리더 선출 직후 또는 Failover 시 전송하여, 네트워크의 ARP 캐시를 즉시 갱신한다.

**NDP (Neighbor Discovery Protocol, IPv6)**:
- IPv6에서 ARP 대신 사용되는 프로토콜이다.
- Neighbor Solicitation(NS)은 ARP Request에, Neighbor Advertisement(NA)는 ARP Reply에 대응한다.
- Unsolicited NA는 GARP와 동일한 목적으로 사용된다.

### Kubernetes Lease 기반 리더 선출

각 서비스 IP에 대해 하나의 노드만 ARP 응답을 해야 한다. 여러 노드가 동시에 응답하면
MAC 테이블이 불안정해지는 문제(ARP flapping)가 발생한다.

Cilium은 Kubernetes Lease 오브젝트를 사용하여 서비스별 리더를 선출한다.
Lease를 획득한 노드만 해당 서비스 IP에 대해 ARP/NDP 응답을 생성한다.
리더 노드가 다운되면 Lease가 만료되고, 다른 노드가 새로운 리더로 선출된다.

---

## 시뮬레이션하는 개념

| 개념 | 실제 Cilium 코드 | 시뮬레이션 방식 |
|------|------------------|----------------|
| Leader Election | `pkg/l2announcer/` Kubernetes Lease API | 서비스별 첫 번째 alive 노드를 리더로 선출 |
| Gratuitous ARP | `pkg/datapath/linux/l2_announcer.go` raw socket | `ARPPacket`으로 GARP 패킷 구성 및 ARP 테이블 갱신 |
| ARP Reply | BPF neighbor responder + L2 announcer | 리더 노드가 서비스 IP에 대한 요청에 응답 |
| NDP NA | IPv6 Unsolicited Neighbor Advertisement | `NDPPacket`으로 Override 플래그 포함 NA 생성 |
| Failover | Lease 갱신 실패 감지 -> 재선출 | 노드 alive 플래그 해제 -> `ElectLeaders()` 재실행 -> GARP 재전송 |
| ARP Table | 네트워크 스위치/라우터의 MAC 테이블 | `ARPTable` 구조체로 IP-MAC 매핑 관리 |

---

## 아키텍처 / 흐름 다이어그램

```
+------------------+    +------------------+    +------------------+
|    worker-1      |    |    worker-2      |    |    worker-3      |
| MAC: aa:bb:cc:.. |    | MAC: dd:ee:ff:.. |    | MAC: 11:22:33:.. |
| L2 Announcer     |    | L2 Announcer     |    | L2 Announcer     |
+--------+---------+    +--------+---------+    +--------+---------+
         |                       |                       |
         v                       v                       v
   +-----------+           +-----------+           +-----------+
   | Lease     |           | Lease     |           | Lease     |
   | Holder?   |           | Holder?   |           | Holder?   |
   | YES (svc1)|           | NO        |           | NO        |
   +-----------+           +-----------+           +-----------+
         |
         v
  [1] GARP 전송: "10.0.100.1 is at aa:bb:cc:.."
         |
         v
+-------------------------------------------------+
|             L2 네트워크 (브로드캐스트 도메인)        |
|                                                 |
|  ARP Table:  10.0.100.1 -> aa:bb:cc:..          |
|              10.0.100.2 -> aa:bb:cc:..          |
+-------------------------------------------------+
         |
         v
  [2] 클라이언트 ARP Request: "who-has 10.0.100.1?"
         |
         v
  [3] worker-1 (리더) ARP Reply: "10.0.100.1 is at aa:bb:cc:.."
         |
         v
  === worker-1 장애 발생 ===
         |
         v
  [4] 리더 재선출: worker-2가 새 리더
         |
         v
  [5] worker-2 GARP 전송: "10.0.100.1 is at dd:ee:ff:.."
         |
         v
  [6] ARP 테이블 갱신 -> 트래픽이 worker-2로 전환
```

---

## 코드 해설

### 1. `ARPPacket` 구조체

```go
type ARPPacket struct {
    Operation ARPOperation
    SenderMAC net.HardwareAddr
    SenderIP  net.IP
    TargetMAC net.HardwareAddr
    TargetIP  net.IP
}
```

- **무엇**: ARP 패킷을 표현하는 구조체. Request/Reply 구분, 송수신자의 MAC/IP를 담는다.
- **어디**: 실제 Cilium에서는 raw socket이나 BPF 프로그램이 이 패킷을 직접 생성/파싱한다.
- **왜**: GARP 판별(`IsGratuitous()`)을 위해 SenderIP == TargetIP 비교가 필요하다.
  리더 노드는 자신의 MAC을 SenderMAC에 넣어 서비스 IP를 자신에게 매핑시킨다.

### 2. `L2Announcer.ElectLeaders()`

```go
func (la *L2Announcer) ElectLeaders() {
    for _, svc := range la.services {
        key := serviceKey(svc)
        elected := false
        for _, node := range la.nodes {
            if node.Alive && !elected {
                node.SetLeader(key, true)
                // ...
```

- **무엇**: 각 서비스에 대해 살아 있는 첫 번째 노드를 리더로 선출한다.
- **어디**: 실제 Cilium에서는 `pkg/l2announcer/`에서 Kubernetes Lease API를 통해 선출한다.
- **왜**: 서비스 IP당 정확히 하나의 노드만 ARP 응답을 해야 ARP flapping을 방지할 수 있다.
  서비스별 독립적인 리더를 두어 부하를 분산하는 효과도 있다.

### 3. `L2Announcer.SendGARP()`

```go
func (la *L2Announcer) SendGARP(node *Node, svc ServiceEntry) {
    garp := ARPPacket{
        Operation: ARPReply,
        SenderMAC: node.MAC,
        SenderIP:  svc.IP,
        TargetMAC: net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
        TargetIP:  svc.IP,
    }
```

- **무엇**: Gratuitous ARP를 생성하여 네트워크에 브로드캐스트하고 ARP 테이블을 갱신한다.
- **어디**: 실제 Cilium의 `pkg/datapath/linux/l2_announcer.go`에서 raw socket으로 전송한다.
- **왜**: 리더 선출/변경 직후 GARP를 전송해야 기존 ARP 캐시 만료를 기다리지 않고
  즉시 트래픽 경로를 갱신할 수 있다. TargetMAC이 브로드캐스트(`ff:ff:ff:ff:ff:ff`)이고
  SenderIP == TargetIP인 것이 GARP의 핵심 특징이다.

### 4. `L2Announcer.Failover()`

```go
func (la *L2Announcer) Failover(failedNodeName string) {
    for _, node := range la.nodes {
        if node.Name == failedNodeName {
            node.Alive = false
        }
    }
    la.ElectLeaders()
    // 새 리더가 GARP + NA 전송
```

- **무엇**: 특정 노드의 장애를 감지하고 리더를 재선출한 뒤 GARP/NA를 재전송한다.
- **어디**: 실제 Cilium에서는 Kubernetes Lease의 TTL 만료로 장애를 감지한다.
- **왜**: 리더 노드가 다운되면 서비스 IP로의 트래픽이 블랙홀에 빠진다.
  빠른 재선출과 GARP 전송으로 서비스 중단 시간을 최소화한다.

### 5. `ARPTable` 구조체

```go
type ARPTable struct {
    entries map[string]*ARPEntry
    mu      sync.RWMutex
}
```

- **무엇**: 네트워크의 ARP 캐시를 시뮬레이션한다. IP-to-MAC 매핑을 저장한다.
- **어디**: 실제 환경에서는 스위치/라우터/호스트의 ARP 캐시에 해당한다.
- **왜**: GARP 전송 전후로 ARP 테이블이 어떻게 변화하는지 시각적으로 확인하기 위해
  명시적으로 관리한다. `sync.RWMutex`로 동시 접근을 보호한다.

---

## 실행 방법 및 예상 출력

```bash
cd cilium_EDU/poc-27-l2-announcer
go run main.go
```

주요 출력 발췌:

```
=== Cilium L2 Announcer 시뮬레이션 ===

[1] 노드 등록
------------------------------------------------------------
  Node: worker-1 IP=10.0.1.10 MAC=<random>
  Node: worker-2 IP=10.0.1.11 MAC=<random>
  Node: worker-3 IP=10.0.1.12 MAC=<random>

[3] 리더 선출 (Lease 기반)
------------------------------------------------------------
  [ELECT] default/web-frontend: 리더 = worker-1 (MAC: ...)
  [ELECT] default/api-gateway: 리더 = worker-1 (MAC: ...)
  [ELECT] monitoring/monitoring: 리더 = worker-1 (MAC: ...)

[4] Gratuitous ARP 전송
------------------------------------------------------------
  [GARP]  worker-1 -> ARP-REPLY: who-has 10.0.100.1? ... (gratuitous=true)
  [NA]    worker-1 -> NA: target=fd00::100:1 ... [O]

[7] Failover 시뮬레이션
------------------------------------------------------------
  [FAIL]  노드 worker-1 장애 감지!
  [ELECT] default/web-frontend: 리더 = worker-2 (MAC: ...)
  [GARP]  worker-2 -> ARP-REPLY: who-has 10.0.100.1? ... (gratuitous=true)

[8] Failover 후 ARP 테이블
------------------------------------------------------------
    10.0.100.1 -> <worker-2의 MAC> (updated: ...)
    10.0.100.2 -> <worker-2의 MAC> (updated: ...)
```

---

## 핵심 포인트

1. **서비스별 리더 선출로 ARP flapping 방지**: 각 서비스 IP에 대해 정확히 하나의 노드만
   ARP 응답을 생성한다. 여러 노드가 동시에 응답하면 스위치의 MAC 테이블이 불안정해진다.

2. **GARP로 즉각적인 트래픽 전환**: 일반 ARP 캐시는 수 분의 TTL을 가진다.
   GARP를 보내면 네트워크 장비가 즉시 MAC 매핑을 갱신하므로 Failover 시간이 단축된다.

3. **IPv4/IPv6 듀얼 스택 지원**: ARP(IPv4)와 NDP Neighbor Advertisement(IPv6)를
   동시에 처리하여 듀얼 스택 환경에서도 서비스 IP 도달성을 보장한다.

4. **Failover 시 자동 복구**: 리더 노드 장애 감지 -> 새 리더 선출 -> GARP/NA 재전송의
   파이프라인이 자동으로 실행되어 서비스 연속성을 유지한다.

---

## 실제 Cilium과의 차이점

| 항목 | 실제 Cilium | 이 시뮬레이션 |
|------|------------|-------------|
| 리더 선출 | Kubernetes Lease API, TTL 기반 만료 | 단순 alive 플래그 + 순서 기반 선출 |
| ARP 전송 | raw socket / BPF 프로그램으로 실제 패킷 전송 | 구조체 출력으로 시뮬레이션 |
| NDP 전송 | ICMPv6 raw socket으로 실제 NA 전송 | 구조체 출력으로 시뮬레이션 |
| 장애 감지 | Lease TTL 만료, kubelet 상태 | 명시적 `Failover()` 호출 |
| 서비스 감시 | K8s informer로 Service 리소스 watch | 정적 서비스 목록 등록 |
| 정책 매칭 | `CiliumL2AnnouncementPolicy` CRD로 대상 선택 | 등록된 모든 서비스에 적용 |
| BPF 연동 | eBPF map에 서비스 IP-MAC 매핑 저장 | `ARPTable` in-memory map |
| 부하 분산 | 서비스별 리더를 다른 노드에 분산 가능 | 모든 서비스의 리더가 동일 노드 |
| 프로브 | 건강 검사로 엔드포인트 상태 반영 | 미구현 |
