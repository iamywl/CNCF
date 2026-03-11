# PoC-27: Cilium L2 Announcer 시뮬레이션

## 개요

Cilium L2 Announcer는 LoadBalancer 서비스 IP에 대해 ARP/NDP 응답을 생성하여
L2 네트워크에서 트래픽을 수신한다. 이 PoC는 리더 선출, GARP, Failover를 시뮬레이션한다.

## 시뮬레이션하는 개념

| 개념 | 실제 Cilium 코드 | 시뮬레이션 |
|------|------------------|-----------|
| Leader Election | Kubernetes Lease | 서비스별 리더 노드 선출 |
| GARP | `pkg/datapath/linux/l2_announcer.go` | Gratuitous ARP 전송 |
| ARP Reply | BPF neighbor responder | ARP 요청에 대한 응답 |
| NDP NA | IPv6 Neighbor Advertisement | Unsolicited NA 전송 |
| Failover | Lease 갱신 실패 감지 | 리더 재선출 + GARP |

## 실행 방법

```bash
cd cilium_EDU/poc-27-l2-announcer
go run main.go
```

## 핵심 포인트

- **리더 선출**: 각 서비스 IP당 하나의 노드만 ARP 응답 (중복 응답 방지)
- **GARP**: 리더 변경 시 Gratuitous ARP로 네트워크의 ARP 캐시를 즉시 업데이트
- **Failover**: 노드 장애 시 다른 노드가 리더를 인계받아 서비스 연속성 보장
