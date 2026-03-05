# PoC-03: Cilium BPF 패킷 처리 파이프라인 시뮬레이션

## 개요

Cilium은 BPF 프로그램을 **tail call 체인**으로 연결하여 패킷을 처리한다.
이 PoC는 실제 BPF 데이터패스의 패킷 처리 흐름을 Go로 시뮬레이션한다.

## 시뮬레이션하는 개념

| 개념 | 실제 코드 | 시뮬레이션 |
|------|----------|-----------|
| Tail Call | `bpf/lib/tail_call.h` | 채널 기반 프로그램 점프 |
| PROG_ARRAY | `BPF_MAP_TYPE_PROG_ARRAY` | 맵에 인덱스별 프로그램 등록 |
| CT Lookup | `bpf/lib/conntrack.h` | 연결 추적 테이블 조회 |
| Policy Check | `bpf/lib/policy.h` | Identity 기반 정책 검사 |
| IPCache | `cilium_ipcache` BPF 맵 | IP → Identity 매핑 |
| 패킷 분류 | `bpf/bpf_lxc.c` | EtherType 기반 진입점 선택 |

## 파이프라인 흐름

```
EtherType 분류 → IPv4/IPv6/ARP
  │
  ├─ IPv4: ipv4_from_lxc → ct_lookup → policy_check → routing → deliver
  │                          │                          │
  │                          ├─ CT HIT (기존 연결)     ├─ 로컬: 직접 전달
  │                          │   → 정책 검사 건너뜀    ├─ 리모트: VXLAN 캡슐화
  │                          │
  │                          └─ CT MISS (새 연결)
  │                              → CT 엔트리 생성
  │                              → 정책 검사
  │
  └─ ARP: arp_handler → 직접 응답
```

## 테스트 시나리오

| # | 시나리오 | 예상 결과 |
|---|---------|----------|
| 1 | frontend → backend:8080 | PASS (정책 허용) |
| 2 | frontend → backend:3306 | DROP (정책 없음) |
| 3 | frontend → 리모트:8080 | PASS (VXLAN 캡슐화) |
| 4 | frontend → backend:8080 (재전송) | PASS (CT HIT, 정책 건너뜀) |
| 5 | backend → frontend (응답) | PASS (CT REPLY) |
| 6 | backend → 8.8.8.8:443 | PASS (외부 접근 허용) |

## 실행 방법

```bash
cd cilium_EDU/poc-03-packet-pipeline
go run main.go
```

## 핵심 포인트

- **Tail Call 체인**: BPF 프로그램 크기 제한을 극복하기 위해 여러 프로그램으로 분할
- **CT 최적화**: 기존 연결(ESTABLISHED/REPLY)은 정책 검사를 건너뛰어 성능 향상
- **Identity 기반 정책**: IP가 아닌 Security Identity로 정책을 검사하여 Pod IP 변경에 강건
- **최대 33번의 tail call**: 커널이 강제하는 제한으로 무한 루프 방지
