# 3. Cilium 시퀀스 다이어그램 — 주요 흐름

---

## 1) Pod 생성 → Endpoint 등록 흐름

Pod가 생성될 때 Cilium이 네트워크를 설정하는 전체 과정.

```
kubelet        CNI Plugin      cilium-daemon     etcd          BPF Datapath
  │                │                │               │               │
  │ Pod 생성        │                │               │               │
  │──ADD 호출──────▶│                │               │               │
  │                │──REST API─────▶│               │               │
  │                │  (Endpoint     │               │               │
  │                │   생성 요청)    │               │               │
  │                │                │               │               │
  │                │                │──Identity     │               │
  │                │                │  할당 요청────▶│               │
  │                │                │               │               │
  │                │                │◀─Identity ID──│               │
  │                │                │  (라벨 기반)   │               │
  │                │                │               │               │
  │                │                │──Policy 계산   │               │
  │                │                │  (어떤 트래픽을 │               │
  │                │                │   허용할지)    │               │
  │                │                │               │               │
  │                │                │──BPF 프로그램──────────────────▶│
  │                │                │  컴파일 & 로딩  │               │
  │                │                │               │               │
  │                │                │──PolicyMap 업데이트────────────▶│
  │                │                │               │               │
  │                │                │──IPCache 업데이트──────────────▶│
  │                │                │               │               │
  │                │◀─IP 주소 반환──│               │               │
  │◀─네트워크 준비──│                │               │               │
  │   완료          │                │               │               │
```

**Why 이 순서?** — Identity가 먼저 할당되어야 Policy를 계산할 수 있고,
Policy가 결정되어야 BPF 맵에 올바른 규칙을 넣을 수 있다.

---

## 2) 패킷 처리 흐름 (Pod → Pod, 같은 노드)

같은 노드의 Pod A에서 Pod B로 패킷이 전달되는 과정.

```
Pod A           bpf_lxc (A의 TC)     CT Map      Policy Map    bpf_lxc (B의 TC)    Pod B
  │                  │                  │             │               │               │
  │──패킷 송신──────▶│                  │             │               │               │
  │                  │                  │             │               │               │
  │                  │──CT 조회────────▶│             │               │               │
  │                  │◀─MISS (새 연결)──│             │               │               │
  │                  │                  │             │               │               │
  │                  │──정책 조회───────────────────▶│               │               │
  │                  │◀─ALLOW─────────────────────── │               │               │
  │                  │                  │             │               │               │
  │                  │──CT 엔트리 생성─▶│             │               │               │
  │                  │                  │             │               │               │
  │                  │──패킷 리다이렉트 (tail call)──▶│               │               │
  │                  │                  │             │               │               │
  │                  │                  │             │  ──CT 조회───▶│               │
  │                  │                  │             │  ◀─HIT────── │               │
  │                  │                  │             │               │               │
  │                  │                  │             │               │──패킷 전달───▶│
  │                  │                  │             │               │               │
```

**Why tail call?** — 초기 BPF는 프로그램당 명령어 수가 4096개로 제한되었다.
현재 커널(5.2+)에서는 100만 개로 확대되었으나, Cilium은 코드 모듈화와
기능별 분리를 위해 여전히 tail call 구조를 유지한다.

---

## 3) 패킷 처리 흐름 (Pod → 외부, 다른 노드)

다른 노드의 Pod으로 패킷이 전달되는 과정 (VXLAN 오버레이 모드).

```
Pod A       bpf_lxc     CT/NAT Map    bpf_overlay    ──네트워크──    bpf_overlay    bpf_lxc    Pod B
(Node 1)      │            │              │                            │              │       (Node 2)
  │           │            │              │                            │              │          │
  │──패킷────▶│            │              │                            │              │          │
  │           │──CT 조회──▶│              │                            │              │          │
  │           │◀─MISS──── │              │                            │              │          │
  │           │            │              │                            │              │          │
  │           │──Policy OK │              │                            │              │          │
  │           │──CT 생성──▶│              │                            │              │          │
  │           │            │              │                            │              │          │
  │           │──IPCache에서 dst 노드 IP 조회                         │              │          │
  │           │──VXLAN 캡슐화─────────────▶│                            │              │          │
  │           │            │              │──VXLAN 터널───────────────▶│              │          │
  │           │            │              │            (물리 네트워크)   │              │          │
  │           │            │              │                            │──디캡슐화────▶│          │
  │           │            │              │                            │              │──전달───▶│
  │           │            │              │                            │              │          │
```

---

## 4) 서비스 로드밸런싱 흐름 (kube-proxy 대체)

ClusterIP 서비스로 접근할 때 BPF가 DNAT를 수행하는 과정.

```
Pod A       bpf_sock (connect)    Service Map    Backend Map    RevNAT Map    Pod B (backend)
  │               │                   │              │              │              │
  │──connect()───▶│                   │              │              │              │
  │  (svc IP:port)│                   │              │              │              │
  │               │──서비스 조회─────▶│              │              │              │
  │               │◀─backend count───│              │              │              │
  │               │                   │              │              │              │
  │               │──백엔드 선택──────────────────── ▶│              │              │
  │               │  (Maglev hash                    │              │              │
  │               │   또는 Random)                   │              │              │
  │               │◀─backend IP:port─────────────────│              │              │
  │               │                   │              │              │              │
  │               │──소켓 dst 주소를   │              │              │              │
  │               │  backend로 교체   │              │              │              │
  │               │                   │              │              │              │
  │◀─connect()   │                   │              │              │              │
  │  (실제로는     │                   │              │              │              │
  │   backend에    │                   │              │              │              │
  │   직접 연결)   │                   │              │              │              │
  │               │                   │              │              │              │
  │──데이터 전송 (backend IP:port로 직접)────────────────────────────────────────▶│
  │               │                   │              │              │              │
```

**Why 소켓 레벨?** — 기존 kube-proxy는 iptables DNAT로 매 패킷마다 NAT를 수행하지만,
Cilium은 `connect()` 시스템콜 시점에 한 번만 주소를 교체한다. 이후 패킷은 NAT 없이 직접 전달되므로 성능이 크게 향상된다.

---

## 5) 정책 업데이트 흐름

CiliumNetworkPolicy가 생성/변경될 때 적용되는 과정.

```
사용자         K8s API       cilium-daemon              Endpoint          BPF PolicyMap
  │              │               │                        │                    │
  │──kubectl     │               │                        │                    │
  │  apply CNP──▶│               │                        │                    │
  │              │──Watch 이벤트─▶│                        │                    │
  │              │               │                        │                    │
  │              │               │──PolicyRepository에     │                    │
  │              │               │  규칙 추가              │                    │
  │              │               │                        │                    │
  │              │               │──영향받는 Endpoint      │                    │
  │              │               │  목록 계산              │                    │
  │              │               │                        │                    │
  │              │               │──각 Endpoint에 대해:    │                    │
  │              │               │  허용 Identity 계산────▶│                    │
  │              │               │                        │──BPF 맵 업데이트──▶│
  │              │               │                        │  (allow/deny 엔트리)│
  │              │               │                        │                    │
  │              │               │──Hubble에 정책 변경     │                    │
  │              │               │  이벤트 전파            │                    │
  │              │               │                        │                    │
```

---

## 6) Hubble Flow 수집 흐름

BPF 데이터패스에서 발생한 이벤트가 사용자에게 전달되는 과정.

```
BPF Datapath     perf ring buffer    Hubble Observer    Hubble Relay     hubble CLI
     │                  │                  │                 │               │
     │──이벤트 발생─────▶│                  │                 │               │
     │  (패킷 전달/드롭  │                  │                 │               │
     │   정책 verdict)  │                  │                 │               │
     │                  │──perf event─────▶│                 │               │
     │                  │                  │                 │               │
     │                  │                  │──Flow 파싱       │               │
     │                  │                  │  (IP, Identity,  │               │
     │                  │                  │   Endpoint 정보  │               │
     │                  │                  │   매핑)          │               │
     │                  │                  │                  │               │
     │                  │                  │──링 버퍼에 저장   │               │
     │                  │                  │                  │               │
     │                  │                  │◀─GetFlows gRPC──│               │
     │                  │                  │──Flow 스트림────▶│               │
     │                  │                  │                  │               │
     │                  │                  │                  │◀─GetFlows────│
     │                  │                  │                  │──Flow 스트림─▶│
     │                  │                  │                  │  (여러 노드    │
     │                  │                  │                  │   통합)        │
```
