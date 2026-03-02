# PoC 07: Cilium eBPF 데이터패스 시뮬레이터

## 개요

Cilium의 eBPF 데이터패스 핵심 메커니즘을 순수 Go(stdlib only)로 시뮬레이션한다.
실제 BPF 프로그램 없이도 다음 개념을 이해할 수 있다:

- **BPF 맵 유형**: Hash, LRU Hash, Array, LPM Trie의 동작 원리
- **Tail Call 체인**: 진입점 -> LB -> CT -> 정책 -> NAT -> Forward/Drop
- **BPF 검증기**: 명령어 한도, bounded loop 제약
- **컴파일 파이프라인**: C -> Clang -> ELF -> cilium/ebpf 로더

## 실행

```bash
go run main.go
```

외부 의존성 없음. Go 표준 라이브러리만 사용한다.

## 시뮬레이션 구성

### Part 1: BPF 맵 유형

| 맵 유형 | 시뮬레이션 내용 | 실제 Cilium 맵 |
|---------|---------------|---------------|
| LPM Trie | CIDR 기반 longest prefix match | `cilium_ipcache`, `cilium_policy_v2` |
| LRU Hash | 용량 초과 시 자동 LRU 제거 | `cilium_ct4_global` |
| Array (Prog Array) | 인덱스 기반 O(1) 접근 | `cilium_calls` (tail call 맵) |
| Hash | Identity 기반 O(1) 조회 | `cilium_lb4_services_v2` |

### Part 2: BPF 검증기

- 프로그램 명령어 수 한도 검증 (1,000,000)
- 무한 루프 탐지
- Tail Call로 분리하여 한도 우회하는 패턴

### Part 3: Tail Call 체인 (5개 시나리오)

| 시나리오 | 흐름 |
|---------|------|
| Pod -> Pod (직접) | entry -> LB(miss) -> CT(create) -> Policy(allow) -> Forward |
| Pod -> Service | entry -> LB(DNAT) -> CT(create) -> Policy(allow) -> Forward |
| Pod -> 외부 | entry -> LB(miss) -> CT(create) -> Policy(allow) -> SNAT -> Forward |
| 정책 거부 | entry -> LB(miss) -> CT(create) -> Policy(DENY) -> Drop notify |
| 동일 연결 재전송 | entry -> LB(miss) -> CT(hit, packets++) -> Policy(allow) -> Forward |

### Part 4: Tail Call 체인 구조 시각화

Egress 방향(Pod -> 외부) tail call 체인의 ASCII 다이어그램.

### Part 5: 컴파일 파이프라인

`bpf_lxc.c`, `bpf_host.c`, `bpf_xdp.c` 각각의 컴파일 과정과
`ebpf.LoadCollectionSpec()` -> `bpf.LoadCollection()` -> TC 부착 -> 맵 핀 커밋 흐름.

## 실제 Cilium 코드 참조

| 시뮬레이션 | 실제 소스 |
|-----------|----------|
| `cilFromContainer()` | `bpf/bpf_lxc.c` - `cil_from_container()` |
| `tailHandleIPv4()` | `bpf/bpf_lxc.c` - `tail_handle_ipv4()` |
| `tailIPv4CTEgress()` | `bpf/bpf_lxc.c` - `TAIL_CT_LOOKUP4()` 매크로 |
| `tailHandleIPv4Cont()` | `bpf/bpf_lxc.c` - `handle_ipv4_from_lxc()` |
| `LRUHashMap` | `bpf/lib/conntrack_map.h` - `BPF_MAP_TYPE_LRU_HASH` |
| `LPMTrie` | `bpf/lib/policy.h` - `BPF_MAP_TYPE_LPM_TRIE` |
| `ArrayMap` (Prog Array) | `bpf/lib/tailcall.h` - `BPF_MAP_TYPE_PROG_ARRAY` |
| `BPFVerifier` | 커널 BPF 검증기 개념 |
| 컴파일 흐름 | `pkg/datapath/loader/compile.go` |

## 관련 문서

- [07-ebpf-datapath.md](../07-ebpf-datapath.md) - eBPF 데이터패스 심층 분석 문서
- [05-core-components.md](../05-core-components.md) - 핵심 구성 요소 참조표
- [poc-05-bpf-maps/](../poc-05-bpf-maps/) - BPF 맵 동작 시뮬레이터
