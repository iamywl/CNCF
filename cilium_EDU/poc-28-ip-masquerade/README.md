# PoC-28: Cilium IP Masquerade (SNAT) 시뮬레이션

## 개요

Cilium은 eBPF 기반 IP Masquerade로 Pod egress 트래픽의 소스 IP를 노드 IP로 변환한다.
이 PoC는 SNAT, 역방향 DNAT, Non-masquerade CIDR, 포트 할당을 시뮬레이션한다.

## 시뮬레이션하는 개념

| 개념 | 실제 Cilium 코드 | 시뮬레이션 |
|------|------------------|-----------|
| SNAT | `bpf/lib/nat.h` | Pod IP:Port → Node IP:Port 변환 |
| Reverse DNAT | `bpf/lib/nat.h` | 응답 패킷의 목적지 복원 |
| Non-masq CIDR | `pkg/ip/masq.go` | 내부 대역 SNAT 제외 |
| Port Allocation | NAT 포트 풀 | 충돌 없는 랜덤 포트 할당 |
| NAT Table GC | CT/NAT GC | 만료된 엔트리 정리 |

## 실행 방법

```bash
cd cilium_EDU/poc-28-ip-masquerade
go run main.go
```

## 핵심 포인트

- **SNAT**: 외부로 나가는 Pod 트래픽의 소스를 노드 IP로 변환하여 라우팅 가능하게 함
- **Non-masquerade CIDR**: 클러스터 내부 통신은 SNAT를 건너뛰어 성능 확보
- **Conntrack**: NAT 테이블로 응답 패킷을 원래 Pod IP:Port로 정확히 복원
