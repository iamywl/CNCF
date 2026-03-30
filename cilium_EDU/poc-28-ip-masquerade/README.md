# PoC-28: Cilium IP Masquerade (SNAT) 시뮬레이션

## 개요

이 PoC는 Cilium의 eBPF 기반 IP Masquerade(SNAT) 메커니즘을 Go 표준 라이브러리만으로 시뮬레이션한다.
Pod에서 클러스터 외부로 나가는 트래픽의 소스 IP를 노드 IP로 변환하는 전체 과정을 재현하며,
역방향 DNAT를 통한 응답 복원, Non-masquerade CIDR 기반 제외 판정, 포트 할당,
NAT 테이블 관리 및 GC까지 아우른다.

## 배경 지식

### IP Masquerade(SNAT)란?

IP Masquerade는 Network Address Translation(NAT)의 한 형태로, 내부 네트워크의 사설 IP 주소를
외부에서 라우팅 가능한 공인 IP 주소로 변환하는 기법이다. Kubernetes 환경에서 Pod은
클러스터 내부에서만 유효한 가상 IP(예: `172.16.0.5`)를 할당받는데, 이 IP로는 외부 인터넷과
직접 통신할 수 없다. Pod이 외부 서비스(예: DNS `8.8.8.8`, HTTPS API 등)에 접근하려면
패킷의 소스 IP를 노드의 실제 IP로 변환(Source NAT)해야 한다.

### 왜 Pod egress에 SNAT가 필요한가?

1. **라우팅 가능성**: Pod CIDR(예: `172.16.0.0/12`)은 클러스터 외부에서 라우팅되지 않는다.
   외부 서버가 Pod IP로 응답을 보내면 경로를 찾지 못해 패킷이 유실된다.
2. **응답 복원**: SNAT 시 NAT 테이블에 원본 정보를 기록해두면, 외부에서 돌아오는 응답 패킷의
   목적지를 원래 Pod IP:Port로 복원(Reverse DNAT)할 수 있다.
3. **보안**: 외부에 클러스터 내부 IP 구조를 노출하지 않는다.

### BPF 맵 기반 구현의 장점 (vs iptables)

| 항목 | iptables 방식 | Cilium eBPF 방식 |
|------|--------------|-----------------|
| 규칙 평가 | 선형 탐색 O(n) | 해시맵 O(1) 조회 |
| Conntrack | 커널 nf_conntrack 모듈 | BPF CT/NAT 맵 직접 관리 |
| 성능 영향 | 규칙 수 증가 시 지연 증가 | 규칙 수와 무관하게 일정 |
| 업데이트 | 전체 체인 재구성 | 개별 맵 엔트리 원자적 갱신 |
| 가시성 | 디버깅 어려움 | `cilium bpf nat list`로 직접 조회 |

## 시뮬레이션하는 개념

| 개념 | 실제 Cilium 코드 | 시뮬레이션 방식 |
|------|------------------|----------------|
| SNAT (egress) | `bpf/lib/nat.h` — `snat_v4_process()` | `ProcessEgress()`: Pod IP:Port를 Node IP:TransPort로 변환 |
| Reverse DNAT (ingress) | `bpf/lib/nat.h` — 역방향 NAT 조회 | `ProcessIngress()`: 응답 패킷의 DstIP를 원래 Pod IP로 복원 |
| Non-masquerade CIDR | `pkg/ip/masq.go` — ip-masq-agent ConfigMap | `NonMasqConfig`: CIDR 목록으로 내부 대역 SNAT 제외 판정 |
| 포트 할당 | BPF NAT 포트 풀 (1024-65535) | `PortAllocator`: 랜덤 시작점 기반 충돌 회피 포트 할당 |
| NAT 테이블 | BPF CT/NAT 맵 (해시맵) | `natTable` (Go map): 정방향/역방향 키로 이중 인덱싱 |
| NAT GC | CT/NAT GC 루프 | `CleanupExpired()`: 만료 시간 기반 엔트리 정리 및 포트 반환 |

## 아키텍처 / 흐름 다이어그램

### Egress (Pod → 외부) SNAT 흐름

```
Pod (172.16.0.5:45000)            Node (10.0.1.10)            외부 서버 (8.8.8.8:53)
        |                               |                               |
        |  SrcIP=172.16.0.5:45000       |                               |
        |  DstIP=8.8.8.8:53             |                               |
        |------------------------------>|                               |
        |                               |                               |
        |              [MasqueradeEngine.ProcessEgress()]               |
        |              1. NonMasqConfig.ShouldMasquerade(8.8.8.8)       |
        |                 → 외부 대역 → masquerade 필요                  |
        |              2. PortAllocator.Allocate() → 32800              |
        |              3. NATEntry 생성 + natTable 저장                  |
        |              4. 패킷 변환: Src → 10.0.1.10:32800              |
        |                               |                               |
        |                               |  SrcIP=10.0.1.10:32800       |
        |                               |  DstIP=8.8.8.8:53            |
        |                               |------------------------------>|
        |                               |                               |
```

### Ingress (외부 → Pod) Reverse DNAT 흐름

```
외부 서버 (8.8.8.8:53)            Node (10.0.1.10)            Pod (172.16.0.5:45000)
        |                               |                               |
        |  SrcIP=8.8.8.8:53             |                               |
        |  DstIP=10.0.1.10:32800        |                               |
        |------------------------------>|                               |
        |                               |                               |
        |              [MasqueradeEngine.ProcessIngress()]              |
        |              1. reverseNatKey로 natTable 조회                  |
        |              2. NATEntry에서 원본 정보 복원                     |
        |              3. 패킷 변환: Dst → 172.16.0.5:45000             |
        |                               |                               |
        |                               |  SrcIP=8.8.8.8:53            |
        |                               |  DstIP=172.16.0.5:45000      |
        |                               |------------------------------>|
        |                               |                               |
```

### Non-Masquerade CIDR 판정 흐름

```
 패킷 도착
     |
     v
 ┌──────────────────────────┐
 │ DstIP가 Non-masq CIDR에  │
 │ 포함되는가?               │
 └──────────┬───────────────┘
       Yes  │         No
            v              v
     ┌──────────┐   ┌─────────────┐
     │ [SKIP]   │   │ [SNAT 수행] │
     │ 원본 그대로│   │ Pod→Node IP │
     │ 전달      │   │ 변환 후 전달 │
     └──────────┘   └─────────────┘
```

## 코드 해설

### 1. `NATEntry` — NAT 변환 레코드

- **무엇**: 하나의 SNAT 변환에 대한 전체 상태를 기록하는 구조체
- **실제 코드**: Cilium BPF CT/NAT 맵의 `struct nat_entry` 에 대응
- **왜 필요한가**: egress에서 생성한 변환 정보를 ingress(응답) 시 역조회하려면
  원본 소스 IP/Port, 변환된 IP/Port, 목적지 정보를 모두 보존해야 한다.
  `Created` 필드는 GC에서 만료 판정에 사용된다.

### 2. `PortAllocator` — SNAT 포트 할당기

- **무엇**: 32768-60999 범위에서 충돌 없이 SNAT용 소스 포트를 할당하는 컴포넌트
- **실제 코드**: Cilium은 BPF 맵 내에서 해시 기반으로 포트를 선택한다
- **왜 랜덤 시작점인가**: 순차 할당은 동시 다발적 연결에서 앞쪽 포트에 충돌이 집중된다.
  랜덤 시작점에서 선형 탐색하면 해시 충돌을 분산시키면서도 빈 포트를 확실히 찾는다.

### 3. `NonMasqConfig.ShouldMasquerade()` — SNAT 제외 판정

- **무엇**: 목적지 IP가 Non-masquerade CIDR 목록에 포함되는지 검사하는 함수
- **실제 코드**: `pkg/ip/masq.go`의 ip-masq-agent 로직, ConfigMap으로 CIDR 목록 관리
- **왜 필요한가**: 클러스터 내부 통신(Pod-to-Pod, Pod-to-Service)까지 SNAT를 적용하면
  불필요한 NAT 오버헤드가 발생하고, 소스 IP 추적이 불가능해진다.
  내부 대역(`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`)은 SNAT를 건너뛴다.

### 4. `MasqueradeEngine.ProcessEgress()` — Egress SNAT 처리

- **무엇**: Pod에서 나가는 패킷에 대해 SNAT를 수행하는 핵심 함수
- **실제 코드**: `bpf/lib/nat.h`의 `snat_v4_process()` — tc egress 훅에서 실행
- **왜 이 순서인가**: (1) Non-masq CIDR 검사 → (2) 이미 노드 IP인지 확인 → (3) 포트 할당
  → (4) NAT 엔트리 저장 → (5) 패킷 변환. 가장 저렴한 검사를 먼저 수행하여
  불필요한 포트 할당과 맵 쓰기를 회피하는 fast-path 최적화 패턴이다.

### 5. `MasqueradeEngine.ProcessIngress()` — Ingress Reverse DNAT

- **무엇**: 외부에서 돌아오는 응답 패킷의 목적지를 원래 Pod IP:Port로 복원하는 함수
- **실제 코드**: BPF ingress 경로에서 NAT 맵 역방향 조회
- **왜 이중 키가 필요한가**: `natTable`에 정방향 키(`origSrc:dst`)와 역방향 키(`rev:dst:trans`)를
  모두 저장한다. egress에서는 정방향 키로 중복 검사를, ingress에서는 역방향 키로
  O(1) 조회를 수행하기 위해서이다.

## 실행 방법 및 예상 출력

```bash
cd cilium_EDU/poc-28-ip-masquerade
go run main.go
```

### 주요 출력 발췌

```
=== Cilium IP Masquerade (SNAT) 시뮬레이션 ===

[1] Masquerade 엔진 초기화
-----------------------------------------------------------------
  Node IP: 10.0.1.10
  Non-masquerade CIDRs:
    - 10.0.0.0/8
    - 172.16.0.0/12
    - 192.168.0.0/16

[2] Egress 패킷 처리 (Pod → 외부)
-----------------------------------------------------------------

  >> Pod→DNS (외부)
  [SNAT]   172.16.0.5:45000 -> 8.8.8.8:53 [UDP] => 10.0.1.10:xxxxx -> 8.8.8.8:53 [UDP]

  >> Pod→HTTPS (외부)
  [SNAT]   172.16.0.5:45001 -> 203.0.113.1:443 [TCP] => 10.0.1.10:xxxxx -> 203.0.113.1:443 [TCP]

  >> Pod→내부 (skip)
  [SKIP]   172.16.0.5:45003 -> 10.0.2.20:8080 [TCP] (non-masquerade CIDR)

  >> Pod→서비스 (skip)
  [SKIP]   172.16.0.5:45004 -> 192.168.1.100:3306 [TCP] (non-masquerade CIDR)

[3] Ingress 응답 패킷 처리 (외부 → Pod)
-----------------------------------------------------------------
  [DNAT]   8.8.8.8:53 -> 10.0.1.10:xxxxx [UDP] => 8.8.8.8:53 -> 172.16.0.5:45000 [UDP]

[6] Masquerade 통계
-----------------------------------------------------------------
  Masqueraded:    104
  Skipped:        2
  De-NATed:       4
  Port Exhausted: 0
  NAT Entries:    208
  Cleaned up:     208 entries

=== 시뮬레이션 완료 ===
```

(포트 번호는 랜덤 할당으로 실행마다 다르며, 대량 시뮬레이션의 정확한 수치도 달라질 수 있다)

## 핵심 포인트

1. **SNAT는 외부 통신의 전제 조건이다**: Pod의 가상 IP는 클러스터 밖에서 라우팅 불가능하므로,
   외부 통신 시 반드시 노드 IP로 변환해야 응답이 돌아올 수 있다.

2. **Non-masquerade CIDR로 내부 트래픽을 보호한다**: 클러스터 내부 대역 간 통신은 SNAT를
   건너뛰어 불필요한 NAT 오버헤드를 제거하고, 소스 IP 기반 네트워크 정책 적용을 가능하게 한다.

3. **NAT 테이블의 이중 인덱싱이 양방향 O(1) 조회를 보장한다**: 정방향 키(egress 중복 검사)와
   역방향 키(ingress 응답 복원)를 동시에 저장하여, 어느 방향의 패킷이든 해시맵 한 번 조회로 처리한다.

4. **포트 풀 관리가 SNAT 확장성의 병목이다**: 노드당 할당 가능한 포트 수(약 28,000개)가
   동시 외부 연결 수의 상한이 된다. 포트 고갈 시 새로운 연결이 불가능해지므로
   GC를 통한 적시 회수가 중요하다.

## 실제 Cilium과의 차이점

| 항목 | 실제 Cilium | 이 시뮬레이션 |
|------|------------|-------------|
| 실행 계층 | eBPF (커널 내 tc 훅) | Go 유저스페이스 |
| NAT 맵 | BPF 해시맵 (`cilium_snat_v4_external`) | Go `map[string]*NATEntry` |
| 포트 할당 | BPF 맵 내 해시 기반 직접 선택 | `PortAllocator`로 랜덤 시작 선형 탐색 |
| Conntrack 연동 | CT 맵과 NAT 맵이 연계되어 상태 추적 | NAT 엔트리만 단독 관리 |
| GC | 전용 BPF GC 루프 + signal 기반 회수 | `CleanupExpired()`로 단순 시간 기반 정리 |
| IPv6 지원 | `snat_v6_process()`로 완전 지원 | IPv4만 시뮬레이션 |
| 동시성 | BPF 맵의 per-CPU 해시맵으로 락 프리 | `sync.RWMutex` 기반 락 |
| Non-masq 설정 | ConfigMap + ip-masq-agent 데몬 | 정적 CIDR 리스트 |
| 패킷 처리 | 실제 네트워크 패킷 헤더 수정 | `Packet` 구조체 필드 교체 |
