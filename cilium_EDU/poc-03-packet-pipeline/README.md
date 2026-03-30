# PoC-03: Cilium BPF 패킷 처리 파이프라인 시뮬레이션

## 개요

Cilium의 데이터플레인은 리눅스 커널의 eBPF를 활용하여 패킷을 처리한다.
단일 BPF 프로그램으로 모든 로직을 담을 수 없기 때문에, **tail call 체인**으로 여러 프로그램을 연결하여
패킷 분류 → Connection Tracking → 정책 검사 → 라우팅 → 전달이라는 파이프라인을 구성한다.

이 PoC는 해당 파이프라인의 핵심 동작 원리를 Go 표준 라이브러리만으로 시뮬레이션한다.
실제 소스코드 참조: `bpf/bpf_lxc.c`, `bpf/lib/tail_call.h`, `bpf/lib/conntrack.h`, `bpf/lib/policy.h`

---

## 배경 지식

### eBPF Tail Call이란?

eBPF 프로그램은 커널 안에서 실행되므로 안전성을 위해 **프로그램 크기 제한**이 존재한다.
초기에는 4,096 인스트럭션이 상한이었고, 커널 5.2부터 100만 인스트럭션으로 확장되었다.
그러나 복잡한 네트워킹 로직(CT, 정책, NAT, 캡슐화 등)을 하나의 프로그램에 모두 담으면
verifier 통과가 어렵고 유지보수도 곤란하다.

**Tail call**은 현재 BPF 프로그램에서 다른 BPF 프로그램으로 점프하는 메커니즘이다.
일반 함수 호출과 달리 **스택이 유지되지 않고, 호출한 프로그램으로 돌아오지 않는다**.
`goto`처럼 제어 흐름이 완전히 넘어간다. 커널은 무한 루프를 방지하기 위해
**최대 33번**의 tail call만 허용한다.

### PROG_ARRAY 맵의 역할

Tail call 대상 프로그램은 `BPF_MAP_TYPE_PROG_ARRAY` 타입의 BPF 맵에 등록된다.
정수 인덱스를 키로, BPF 프로그램 파일 디스크립터를 값으로 가진다.
Cilium은 `CILIUM_CALL_*` 상수로 각 슬롯에 프로그램을 배치하고,
`tail_call_static()` 매크로로 인덱스를 지정하여 점프한다.

### Connection Tracking과 성능 최적화

Connection Tracking(CT)은 네트워크 연결의 상태를 추적하는 메커니즘이다.
Cilium은 `cilium_ct4_global` BPF 맵(LRU Hash)에 연결 정보를 저장한다.
새 연결(CT MISS)이 발생하면 정방향/역방향 두 개의 CT 엔트리를 생성하고 반드시 정책 검사를 수행한다.
그러나 **이미 확인된 연결(CT HIT)**의 후속 패킷은 정책 검사를 건너뛰고 곧바로 라우팅 단계로 넘어간다.
대부분의 트래픽은 기존 연결의 패킷이므로, CT HIT 경로가 빠를수록 전체 처리량이 향상된다.

---

## 시뮬레이션하는 개념

| 실제 개념 | 실제 코드 위치 | PoC에서의 시뮬레이션 |
|-----------|---------------|---------------------|
| Tail Call | `bpf/lib/tail_call.h` | `ProgArray` 맵 + 인덱스 기반 프로그램 점프 |
| PROG_ARRAY | `BPF_MAP_TYPE_PROG_ARRAY` | `map[TailCallIndex]BPFProgram` |
| CT Lookup | `bpf/lib/conntrack.h` `ct_lookup4()` | `CTTable` 해시맵 조회/생성 |
| Policy Check | `bpf/lib/policy.h` `policy_can_egress4()` | `PolicyMap` Identity 기반 허용/거부 |
| IPCache | `cilium_ipcache` BPF 맵 | `IPIdentityMap` (IP → Security Identity) |
| 패킷 분류 | `bpf/bpf_lxc.c` `handle_xgress()` | EtherType 기반 진입점 선택 |
| Verdict | `bpf/lib/common.h` `TC_ACT_*` | `Verdict` 열거형 (PASS/DROP/REDIRECT) |
| 최대 33 tail call | 커널 제한 | `maxTailCalls := 33` 루프 제한 |

---

## 아키텍처 / 흐름 다이어그램

```
  패킷 수신 (TC hook) → EtherType 분류
      │                     └─ ARP → arp_handler → PASS
      ▼
  ipv4_from_lxc [0]     IP → Identity 조회 (ipcache)
      │ tail call
      ▼
  ct_lookup [3]         CT 테이블 조회
      ├─ CT HIT ──────────────────────┐  (정책 검사 건너뜀)
      │  (ESTABLISHED/REPLY)          │
      └─ CT MISS (새 연결)            │
          ├─ CT 엔트리 생성           │
          │  (정방향+역방향)          │
          ▼                           │
  policy_check [4]     srcID+dstPort  │
      ├─ 허용 ─────────┐             │
      └─ 거부 → DROP   │             │
                        ▼             ▼
                   routing [6]     목적지 판별
                    ├─ 로컬 → deliver [8] → PASS
                    └─ 리모트 → encap [7] (VXLAN) → deliver [8] → PASS
```

---

## 코드 해설

### 1. `Packet` 구조체 (Line 117-134)

처리 중인 패킷을 표현한다. L2(EtherType), L3(IP), L4(Port, Protocol) 헤더 정보와 함께
Security Identity(`SrcID`, `DstID`), CT 상태, 최종 판정(Verdict), 경로 추적(Trace)을 포함한다.
`Trace` 슬라이스는 패킷이 어떤 프로그램을 거쳤는지 기록하여 디버깅에 활용된다.

### 2. `ProgArray` 구조체 (Line 186-197)

`BPF_MAP_TYPE_PROG_ARRAY`의 시뮬레이션이다.
`TailCallIndex`를 키로, `BPFProgram` 함수를 값으로 보관한다.
각 BPF 프로그램은 패킷을 받아 처리한 뒤, 다음에 호출할 프로그램의 인덱스를 반환하거나
`done=true`로 체인을 종료한다.

### 3. `CTTable` — Connection Tracking (Line 209-231)

`cilium_ct4_global` BPF 맵의 시뮬레이션이다.
5-tuple(SrcIP, DstIP, SrcPort, DstPort, Protocol)을 키로 연결 상태를 저장한다.
CT MISS시 정방향(ESTABLISHED) + 역방향(REPLY) 엔트리를 동시에 생성하여
응답 패킷도 기존 연결으로 인식되게 한다.

### 4. `PolicyMap` — Identity 기반 정책 (Line 243-266)

`cilium_policy_*` per-endpoint BPF 맵의 시뮬레이션이다.
IP 주소가 아닌 **Security Identity + 목적지 포트 + 프로토콜** 조합으로 정책을 검사한다.
Pod IP가 변경되더라도 Identity가 동일하면 정책이 그대로 적용된다.

### 5. `ProcessPacket` 메서드 (Line 453-494)

파이프라인의 실행 엔진이다. EtherType에 따라 진입 프로그램을 결정한 뒤,
`maxTailCalls(33)` 제한 내에서 tail call 체인을 순차 실행한다.
각 프로그램이 반환하는 `nextCall` 인덱스로 다음 프로그램을 찾아 호출하며,
`done=true`이면 체인을 종료하고 결과 채널로 패킷을 전달한다.

---

## 실행 방법 및 예상 출력

```bash
cd cilium_EDU/poc-03-packet-pipeline
go run main.go
```

### 테스트 시나리오

| # | 시나리오 | 경로 | 예상 결과 |
|---|---------|------|----------|
| 1 | frontend → backend:8080 (새 연결) | CT MISS → 정책 허용 → 로컬 전달 | PASS |
| 2 | frontend → backend:3306 (정책 없음) | CT MISS → 정책 거부 | DROP |
| 3 | frontend → 리모트:8080 | CT MISS → 정책 허용 → VXLAN 캡슐화 | PASS |
| 4 | frontend → backend:8080 (재전송) | CT HIT → 정책 스킵 → 로컬 전달 | PASS |
| 5 | backend → frontend (응답) | CT REPLY → 정책 스킵 → 로컬 전달 | PASS |
| 6 | backend → 8.8.8.8:443 (외부) | CT MISS → 정책 허용 → VXLAN 캡슐화 | PASS |

패킷 #4(CT HIT 경로)의 Trace에서 `ct_lookup → routing`으로 직접 넘어가며
`policy_check`를 거치지 않는 것을 확인할 수 있다:

```
[ct_lookup] CT 히트: ESTABLISHED
[tail_call] ct_lookup → routing          ← 정책 검사 건너뜀!
[routing] 로컬 Pod 대상 → 직접 전달
[deliver] 패킷 전달 완료
```

---

## 핵심 포인트

1. **Tail Call로 프로그램 분할**: 프로그램 크기 제한과 verifier 복잡도를 극복하기 위해
   기능별 독립 프로그램을 PROG_ARRAY 맵으로 체인 연결한다.
2. **CT HIT 패스트 패스**: 기존 연결(ESTABLISHED/REPLY) 패킷은 정책 검사를 건너뛰고
   곧바로 라우팅으로 넘어간다. 대부분의 트래픽이 이 경로를 타므로 지연이 크게 줄어든다.
3. **Identity 기반 정책**: IP 대신 Security Identity로 정책을 검사한다.
   Pod IP가 바뀌어도 동일 Label이면 같은 Identity가 부여되어 정책이 유지된다.
4. **최대 33번 tail call 제한**: 커널이 강제하는 제한으로 무한 루프를 원천 차단한다.
5. **양방향 CT 엔트리 생성**: 정방향(ESTABLISHED) + 역방향(REPLY)을 동시 생성하여
   응답 패킷이 정책 검사 없이 통과한다.

---

## 실제 Cilium과의 차이점

| 항목 | 실제 Cilium | 이 PoC |
|------|-----------|--------|
| 실행 환경 | 커널 eBPF VM, TC hook에 attach | Go 유저스페이스 |
| Tail call | `bpf_tail_call()` 헬퍼, 스택 공유 없음 | 함수 반환값 + 루프로 시뮬레이션 |
| PROG_ARRAY | `BPF_MAP_TYPE_PROG_ARRAY` (커널 맵) | Go `map[int]func` |
| CT 테이블 | `cilium_ct4_global` LRU Hash, 타임아웃/GC | 단순 `map`, 만료 없음 |
| 정책 맵 | per-endpoint `cilium_policy_*` 맵, 방향별 | 단일 글로벌 맵 |
| NAT | Conntrack 연계 SNAT/DNAT, 포트 할당 | 로그만 출력 |
| 캡슐화 | 실제 VXLAN/Geneve 헤더 추가, FIB lookup | 로그만 출력 |
| 패킷 파싱 | `skb`/`xdp_md`에서 직접 바이트 파싱 | 구조체 필드 직접 접근 |
| 동시성 | 커널 per-CPU 맵, RCU 동기화 | `sync.RWMutex` |
| 암호화 | IPsec/WireGuard 투명 암호화 | 미구현 |