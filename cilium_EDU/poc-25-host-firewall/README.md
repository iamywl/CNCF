# PoC-25: Cilium Host Firewall 시뮬레이션

## 개요

Cilium Host Firewall은 eBPF를 사용하여 호스트 인터페이스에서 패킷을 필터링한다.
이 PoC는 BPF 정책 맵 룩업, CIDR 매칭, Connection Tracking 등 핵심 동작을 시뮬레이션한다.

## 시뮬레이션하는 개념

| 개념 | 실제 Cilium 코드 | 시뮬레이션 |
|------|------------------|-----------|
| PolicyMap | `pkg/maps/policymap/` | 우선순위 기반 규칙 매칭 |
| CIDR Match | `bpf/lib/lpm.h` (LPM trie) | net.IPNet 기반 매칭 |
| ConnTrack | `bpf/lib/conntrack.h` | 상태 기반 연결 추적 |
| Ingress/Egress | `bpf/bpf_host.c` | 방향별 필터링 |
| Default Deny | Host Firewall 모드 기본값 | 매칭 실패 시 DROP |

## 실행 방법

```bash
cd cilium_EDU/poc-25-host-firewall
go run main.go
```

## 핵심 포인트

- **BPF 맵 룩업**: 실제로는 O(1) 해시 맵이지만, PoC에서는 우선순위 순 선형 탐색으로 구현
- **Connection Tracking**: 허용된 패킷의 역방향 응답을 자동 허용하여 stateful 필터링 구현
- **Default Deny**: Host Firewall 모드에서는 명시적 허용 규칙이 없으면 기본 거부
