# PoC-07: eBPF 데이터패스 시뮬레이션

## 개요

Cilium의 eBPF 데이터패스 패킷 처리 흐름을 Go로 시뮬레이션한다.
실제 Cilium에서는 BPF tail call 체인으로 구현된 각 단계를 함수 체이닝으로 재현한다.

## 핵심 개념

### 데이터패스 파이프라인 (Tail Call Chain)
```
패킷 도착 → classify_packet → extract_tuple → ct_lookup → policy_check → routing_decision
```

각 단계는 실제 Cilium에서 별도의 BPF 프로그램이며, `bpf_tail_call()`로 호출된다.
tail call은 스택을 소비하지 않으므로 복잡한 처리를 단계별로 분리할 수 있다.

### 주요 컴포넌트

| 컴포넌트 | 실제 Cilium | 시뮬레이션 |
|----------|------------|-----------|
| Conntrack | `cilium_ct4_global` BPF map | `CTTable` (Go map) |
| Policy Map | `cilium_policy_<ep_id>` BPF map | `PolicyMap` (슬라이스) |
| 5-Tuple | `struct ipv4_ct_tuple` | `Tuple5` 구조체 |
| Security Identity | 숫자형 ID (레이블 기반 할당) | `SecurityIdentity` 상수 |

### 시나리오

1. **새 연결 (ALLOW)**: Frontend → App:80/TCP, 정책 허용, CT 엔트리 생성
2. **후속 패킷**: 동일 연결의 다음 패킷, CT established 상태
3. **응답 패킷**: 역방향 매칭으로 CT reply 감지
4. **거부**: World → App 접근 시 policy deny + drop notification
5. **L7 프록시 리다이렉트**: 특정 포트를 Envoy로 리다이렉트
6. **터널 전송**: 원격 노드의 Pod으로 VXLAN 터널 경유
7. **ARP**: 비-IP 패킷은 커널 스택으로 전달

## 실행

```bash
go run main.go
```

## 관련 소스 코드

- `bpf/bpf_lxc.c` — 엔드포인트의 eBPF 진입점 (handle_xgress)
- `bpf/lib/conntrack.h` — conntrack 조회/생성
- `bpf/lib/policy.h` — 정책 검사 로직
- `bpf/lib/drop.h` — 드롭 알림 생성

## 학습 포인트

- eBPF 데이터패스는 커널 네트워크 스택 이전에 실행되어 높은 성능을 달성한다
- Conntrack은 상태 기반 방화벽의 핵심으로, 확립된 연결은 정책 재검사를 건너뛴다
- Deny 규칙이 Allow보다 항상 우선한다 (deny-first 원칙)
- Tail call 체인은 BPF 프로그램의 복잡도를 관리하는 핵심 패턴이다
