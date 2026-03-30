# PoC-25: Cilium Host Firewall 시뮬레이션

## 개요

이 PoC는 Cilium의 **Host Firewall** 기능을 순수 Go 표준 라이브러리만으로 시뮬레이션한다.
Host Firewall은 쿠버네티스 노드(호스트) 자체에 대한 네트워크 정책을 eBPF 프로그램으로
적용하는 기능으로, Pod 네트워크 정책과는 별도로 호스트 인터페이스(eth0 등)의 TC 훅에서
패킷을 필터링한다. 이 PoC는 PolicyMap 기반 규칙 룩업, CIDR 매칭, 방향별(Ingress/Egress)
필터링, Connection Tracking을 포함한 전체 파이프라인을 재현한다.

## 배경 지식

### Host Firewall이 필요한 이유

쿠버네티스 NetworkPolicy는 Pod 간 트래픽만 제어한다. 그러나 실제 클러스터에서는
호스트 수준의 보안이 반드시 필요하다:

- **노드 SSH 접근 제한**: 특정 관리 네트워크에서만 SSH(22번 포트) 허용
- **호스트 프로세스 보호**: kubelet(10250), etcd(2379) 등 호스트에서 직접 실행되는
  데몬에 대한 접근 제어
- **외부 DB 접속 차단**: 노드에서 외부 데이터베이스로의 무단 이그레스 방지
- **컴플라이언스 요구사항**: 호스트 레벨 방화벽 정책 강제 적용

전통적으로 iptables/nftables로 해결했지만, 규칙 수가 늘어나면 O(n) 순회로 인해
성능이 급격히 저하된다.

### BPF TC 훅 기반 구현의 장점

Cilium은 iptables 대신 eBPF 프로그램을 TC(Traffic Control) 훅에 부착하여 동작한다:

- **O(1) 룩업**: BPF 해시 맵/LPM trie로 정책을 저장하여 규칙 수에 무관한 상수 시간 매칭
- **커널 우회 없음**: 커널 네트워크 스택 내부에서 직접 동작하므로 컨텍스트 스위칭 없음
- **원자적 업데이트**: BPF 맵 업데이트는 lock-free로 트래픽 중단 없이 정책 변경 가능
- **Conntrack 통합**: BPF CT 맵으로 상태 기반 필터링을 커널 공간에서 직접 수행

## 시뮬레이션하는 개념

| 개념 | 실제 Cilium 코드 | 시뮬레이션 방식 |
|------|------------------|----------------|
| PolicyMap (BPF 맵) | `pkg/maps/policymap/` | 우선순위 기반 규칙 리스트 순회 |
| CIDR Match (LPM trie) | `bpf/lib/lpm.h` | `net.IPNet.Contains()` 기반 접두사 매칭 |
| Connection Tracking | `bpf/lib/conntrack.h` | 역방향 키 검색 + 만료 시간 기반 CT 테이블 |
| Ingress/Egress 훅 | `bpf/bpf_host.c` tc_ingress/tc_egress | `Direction` 상수로 방향별 분기 처리 |
| Default Deny 정책 | Host Firewall 모드 기본값 | `Lookup()` 매칭 실패 시 `VerdictDrop` 반환 |
| CT Garbage Collection | `pkg/maps/ctmap/gc.go` | `ConnTrack.GC()`로 만료 엔트리 정리 |
| 방화벽 통계 | `pkg/metrics/` eBPF metrics | `FirewallStats` 구조체로 허용/거부 카운터 |

## 아키텍처 / 흐름 다이어그램

```
                         호스트 인터페이스 (eth0)
                                │
               ┌────────────────┴────────────────┐
               │                                 │
          [TC Ingress]                      [TC Egress]
               │                                 │
               ▼                                 ▼
   ┌───────────────────┐             ┌───────────────────┐
   │  1. ConnTrack     │             │  1. ConnTrack     │
   │     검사           │             │     검사           │
   │  (역방향 키 조회)   │             │  (역방향 키 조회)   │
   └────────┬──────────┘             └────────┬──────────┘
            │                                 │
     established?                      established?
      ├── YES → ALLOW                  ├── YES → ALLOW
      │                                │
      ▼ NO                             ▼ NO
   ┌───────────────────┐             ┌───────────────────┐
   │  2. PolicyMap     │             │  2. PolicyMap     │
   │     룩업           │             │     룩업           │
   │  (우선순위순 매칭)  │             │  (우선순위순 매칭)  │
   └────────┬──────────┘             └────────┬──────────┘
            │                                 │
     ALLOW? ──YES──┐                   ALLOW? ──YES──┐
            │      ▼                          │      ▼
            │  3. CT 엔트리                    │  3. CT 엔트리
            │     생성                         │     생성
            │      │                          │      │
            ▼      ▼                          ▼      ▼
   ┌───────────────────┐             ┌───────────────────┐
   │  4. 통계 업데이트   │             │  4. 통계 업데이트   │
   │  (허용/거부 카운터) │             │  (허용/거부 카운터) │
   └───────────────────┘             └───────────────────┘
```

패킷 처리 순서 요약:

```
패킷 도착 → CT 검사 (established?) → PolicyMap 룩업 → CT 기록 → 통계 → 판정
```

## 코드 해설

### 1. PolicyRule -- 방화벽 규칙 단위

```go
type PolicyRule struct {
    Name      string
    Direction Direction
    SrcCIDR   *CIDRRule
    DstCIDR   *CIDRRule
    DstPort   uint16
    Protocol  Protocol
    Verdict   Verdict
    Priority  int
}
```

- **무엇**: 하나의 방화벽 규칙을 표현하는 구조체
- **어디**: 실제 Cilium에서는 `pkg/policy/` 엔진이 이를 BPF 맵 엔트리(`PolicyEntry`)로 변환
- **왜**: 방향, CIDR, 포트, 프로토콜, 판정을 하나의 단위로 묶어야 우선순위 기반 매칭이 가능.
  `Priority` 필드가 낮을수록 먼저 평가되어, "내부 SSH 허용(10) > 외부 SSH 거부(20)" 같은
  세밀한 정책 순서를 표현한다.

### 2. PolicyMap -- BPF 정책 맵 시뮬레이션

```go
func (pm *PolicyMap) Lookup(pkt Packet, dir Direction) (Verdict, string) {
    for _, rule := range pm.rules {
        if rule.Match(pkt, dir) {
            return rule.Verdict, rule.Name
        }
    }
    return VerdictDrop, "default-deny"
}
```

- **무엇**: 패킷에 대해 매칭되는 첫 번째 규칙의 판정을 반환하는 룩업 함수
- **어디**: 실제 Cilium에서는 `bpf_map_lookup_elem()`으로 O(1) 해시 맵 조회를 수행
- **왜**: 실제 BPF 맵은 해시 기반 O(1)이지만, 이 시뮬레이션에서는 우선순위 기반
  first-match 의미론을 보여주기 위해 정렬된 리스트를 순회한다.
  매칭 실패 시 `VerdictDrop`을 반환하여 Host Firewall의 default-deny 정책을 구현한다.

### 3. ConnTrack -- 상태 기반 연결 추적

```go
func (ct *ConnTrack) IsEstablished(pkt Packet) bool {
    reverseKey := connTrackKey(pkt.DstIP.String(), pkt.SrcIP.String(),
                               pkt.DstPort, pkt.SrcPort, pkt.Protocol)
    entry, ok := ct.entries[reverseKey]
    if !ok { return false }
    return time.Now().Before(entry.Expires)
}
```

- **무엇**: 수신 패킷이 기존 허용된 연결의 응답인지 역방향 키로 검사
- **어디**: 실제 Cilium에서는 `bpf/lib/conntrack.h`의 `ct_lookup4()`가 CT BPF 맵에서 조회
- **왜**: 상태 기반 필터링의 핵심. 한번 허용된 연결의 응답 패킷은 별도 정책 룩업 없이
  즉시 허용하여 성능을 높이고, 응답 패킷에 대한 명시적 규칙 없이도 양방향 통신을 보장한다.

### 4. HostFirewall.ProcessPacket -- 전체 파이프라인

```go
func (hf *HostFirewall) ProcessPacket(pkt Packet, dir Direction) Verdict
```

- **무엇**: TC 훅(tc_ingress/tc_egress)의 전체 패킷 처리 흐름을 하나의 함수로 구현
- **어디**: 실제 Cilium에서는 `bpf/bpf_host.c`의 `handle_xgress()` 함수가 이 역할을 수행
- **왜**: CT 검사 -> PolicyMap 룩업 -> CT 기록 -> 통계 업데이트의 4단계 파이프라인을
  순서대로 실행하여, eBPF 프로그램의 결정론적 처리 흐름을 보여준다.

### 5. ConnTrack.GC -- CT 가비지 컬렉션

```go
func (ct *ConnTrack) GC() int {
    removed := 0
    for key, entry := range ct.entries {
        if now.After(entry.Expires) {
            delete(ct.entries, key)
            removed++
        }
    }
    return removed
}
```

- **무엇**: 만료된 CT 엔트리를 정리하는 가비지 컬렉터
- **어디**: 실제 Cilium에서는 `pkg/maps/ctmap/gc.go`의 `GC()`가 주기적으로 BPF CT 맵을 순회
- **왜**: CT 테이블이 무한히 커지는 것을 방지. 실제 Cilium은 별도 고루틴에서 주기적으로
  실행하며, TCP 상태(FIN, RST)에 따른 타임아웃 차등 적용도 수행한다.

## 실행 방법 및 예상 출력

```bash
cd cilium_EDU/poc-25-host-firewall
go run main.go
```

주요 출력 발췌:

```
=== Cilium Host Firewall 시뮬레이션 ===

[1] 정책 규칙 등록
----------------------------------------------------------------------
  + [INGRESS] allow-ssh-internal src=10.0.0.0/24 dst=any port=22 proto=TCP -> ALLOW
  + [INGRESS] deny-ssh-external src=any dst=any port=22 proto=TCP -> DENY
  + [INGRESS] allow-http src=any dst=any port=80 proto=TCP -> ALLOW
  + [EGRESS] allow-dns-egress src=any dst=any port=53 proto=UDP -> ALLOW
  ...

[2] 패킷 필터링 시뮬레이션
----------------------------------------------------------------------
  >> 내부 SSH 접속
  [POLICY] INGRESS 10.0.0.5:54321 -> 10.0.0.1:22 [TCP] -> ALLOW (rule: allow-ssh-internal)

  >> 외부 SSH 접속 시도
  [POLICY] INGRESS 203.0.113.1:54322 -> 10.0.0.1:22 [TCP] -> DENY (rule: deny-ssh-external)

  >> 외부 비허용 포트
  [POLICY] INGRESS 203.0.113.3:54324 -> 10.0.0.1:8080 [TCP] -> DROP (rule: default-deny)

[3] Connection Tracking (응답 패킷)
----------------------------------------------------------------------
  >> HTTP 응답 패킷 (established 연결)
  [CT-HIT]  EGRESS 10.0.0.1:80 -> 203.0.113.2:54323 [TCP] -> ALLOW (established)

  >> DNS 응답 패킷 (established 연결)
  [CT-HIT]  INGRESS 8.8.8.8:53 -> 10.0.0.1:12345 [UDP] -> ALLOW (established)

[5] 방화벽 통계
----------------------------------------------------------------------
  Ingress Allowed: ...
  Ingress Denied:  ...
  Egress Allowed:  ...
  Egress Denied:   ...
  CT Hits:         ...
```

출력에서 주목할 점:
- `[POLICY]`는 PolicyMap 룩업으로 판정된 패킷, `[CT-HIT]`는 CT 테이블 히트로 판정된 패킷
- 같은 포트 22라도 소스 IP가 `10.0.0.0/24`이면 ALLOW, 외부이면 DENY
- 매칭되는 규칙이 없는 포트 8080은 default-deny로 DROP 처리

## 핵심 포인트

1. **Default Deny 원칙**: Host Firewall 모드에서는 명시적 허용 규칙이 없는 모든
   트래픽을 거부한다. 이것은 보안의 기본 원칙인 "최소 권한(Least Privilege)"의 구현이다.

2. **우선순위 기반 First-Match**: 규칙은 Priority가 낮은 것부터 평가되며, 처음 매칭된
   규칙의 판정이 최종 결과가 된다. 이를 통해 "10.0.0.0/24에서 SSH 허용(10) →
   나머지에서 SSH 거부(20)"처럼 세밀한 정책 표현이 가능하다.

3. **Stateful Filtering via CT**: Connection Tracking으로 한번 허용된 연결의 응답
   패킷은 정책 룩업 없이 즉시 허용한다. 이것은 성능 최적화이자, 응답 패킷에 대한
   별도 규칙 작성 부담을 제거한다.

4. **방향별 독립 정책**: Ingress와 Egress를 독립적으로 제어하여, 유입 트래픽과
   유출 트래픽에 서로 다른 보안 정책을 적용할 수 있다.

## 실제 Cilium과의 차이점

| 항목 | 실제 Cilium | 이 PoC |
|------|------------|--------|
| 정책 저장소 | BPF 해시 맵 (O(1) 룩업) | Go 슬라이스 순회 (O(n)) |
| CIDR 매칭 | BPF LPM trie (longest-prefix match) | `net.IPNet.Contains()` 단순 매칭 |
| 실행 공간 | 커널 공간 (eBPF 프로그램) | 유저 공간 (Go 프로세스) |
| TC 훅 부착 | `tc filter add dev eth0 bpf` | 함수 호출로 시뮬레이션 |
| CT 상태 머신 | TCP 상태(SYN/FIN/RST) 추적 | 단순 타이머 기반 만료 |
| CT GC | 별도 고루틴에서 주기적 실행 | 수동 호출 1회 |
| Identity 기반 정책 | 보안 Identity 라벨로 매칭 | CIDR/포트 기반만 지원 |
| 정책 업데이트 | lock-free BPF 맵 원자적 업데이트 | 슬라이스 삽입 (비동시성) |
| Metrics | Prometheus 메트릭 노출 | 구조체 카운터 출력 |
| 멀티 인터페이스 | 여러 NIC에 독립 BPF 프로그램 부착 | 단일 HostFirewall 인스턴스 |
