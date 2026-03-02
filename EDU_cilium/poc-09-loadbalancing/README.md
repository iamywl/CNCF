# PoC 09: Cilium 로드 밸런싱 서브시스템

## 개요

이 PoC는 Cilium의 로드 밸런싱 서브시스템의 핵심 메커니즘을 순수 Go(표준 라이브러리만 사용)로 시뮬레이션한다.

## 실행 방법

```bash
cd EDU/poc-09-loadbalancing
go run main.go
```

외부 의존성이 없으므로 `go mod init`이나 별도 패키지 설치가 필요 없다.

## 시뮬레이션 항목

### 1. Maglev 일관 해싱

Cilium 소스: `pkg/maglev/maglev.go`

- 소수 크기 M의 룩업 테이블(LUT)을 구성한다
- 각 백엔드에 대해 `offset`, `skip` 기반 순열을 생성한다
- 라운드 로빈으로 테이블을 채워 균등한 분배를 달성한다
- `hash(5-tuple) % M`으로 인덱스를 계산하여 백엔드를 선택한다

**검증 항목**: 5개 백엔드에서 각 백엔드가 약 20%의 슬롯을 차지하는지 확인한다.

### 2. Maglev 최소 혼란 (Minimal Disruption)

Cilium 소스: `pkg/maglev/maglev.go`

- 백엔드 추가 시: 새 백엔드의 몫(~1/N)만큼만 슬롯이 변경된다
- 백엔드 제거 시: 제거된 백엔드의 몫(~1/N)만큼만 슬롯이 재배치된다
- 기존 연결의 대부분이 유지되는 것을 수치로 검증한다

### 3. Random 백엔드 선택

Cilium 소스: `bpf/lib/lb.h`의 `lb4_select_backend_id_random()`

- `get_prandom_u32() % count + 1`로 슬롯을 선택한다
- Quarantined/Maintenance 상태의 백엔드는 제외된다
- 10,000회 시행으로 균등 분배를 확인한다

### 4. Session Affinity

Cilium 소스: `bpf/lib/lb.h`의 Affinity Map, `pkg/loadbalancer/maps/types.go`

- 클라이언트 IP + 서비스 ID로 키를 구성한다
- 첫 요청 시 백엔드를 할당하고 Affinity Map에 기록한다
- 이후 요청은 저장된 백엔드를 반환한다
- 타임아웃(2초) 경과 후 재할당되는 것을 확인한다

### 5. DSR vs SNAT 패킷 흐름

Cilium 소스: `bpf/lib/nodeport.h`, `bpf/lib/lb.h`

- **SNAT**: 요청/응답 모두 LB 노드를 경유한다 (4번의 패킷 처리)
- **DSR**: 요청만 LB 노드를 경유하고, 응답은 백엔드에서 직접 클라이언트로 전송한다 (2번의 패킷 처리)
- DSR 모드에서는 IP 옵션/IPIP/Geneve 헤더에 원래 VIP 정보를 인코딩한다

### 6. Socket-level LB

Cilium 소스: `bpf/bpf_sock.c`의 `cil_sock4_connect()`, `__sock4_xlate_fwd()`

- `connect()` 시스콜 시점에 목적지 주소를 VIP에서 백엔드 주소로 변환한다
- 이후 `send()`/`recv()`에서는 NAT가 필요 없다
- `getpeername()`은 SockRevNAT 맵을 통해 원래 VIP를 반환한다
- conntrack 부하를 완전히 제거하는 것이 핵심 이점이다

### 7. 서비스 유형별 BPF 맵 구성

Cilium 소스: `pkg/loadbalancer/loadbalancer.go`, `bpf/lib/lb.h`

- **ClusterIP**: 클러스터 내부 전용 VIP
- **NodePort**: `0.0.0.0:NodePort` 와일드카드 엔트리 + `SVC_FLAG_NODEPORT`
- **LoadBalancer**: 외부 IP + `SVC_FLAG_LOADBALANCER` + DSR 지원
- **ExternalIP**: 지정 IP + `SVC_FLAG_EXTERNAL_IP`

## 실제 Cilium 코드와의 대응

| PoC 구현 | Cilium 실제 코드 |
|----------|-----------------|
| `BuildMaglevTable()` | `pkg/maglev/maglev.go: GetLookupTable()` |
| `getOffsetAndSkip()` | `pkg/maglev/maglev.go: getOffsetAndSkip()` |
| `MaglevTable.Lookup()` | `bpf/lib/lb.h: lb4_select_backend_id_maglev()` |
| `RandomSelect()` | `bpf/lib/lb.h: lb4_select_backend_id_random()` |
| `AffinityMap` | `bpf/lib/lb.h: cilium_lb4_affinity` BPF 맵 |
| `SocketLB.Connect()` | `bpf/bpf_sock.c: __sock4_xlate_fwd()` |
| `SimulateDSR()` | `bpf/lib/nodeport.h: nodeport_lb4()` DSR 경로 |
| `ServiceMapEntry` | `bpf/lib/lb.h: struct lb4_service` |
| `BackendMap` | `bpf/lib/lb.h: cilium_lb4_backends_v3` BPF 맵 |
| `RevNatMap` | `bpf/lib/lb.h: cilium_lb4_reverse_nat` BPF 맵 |

## 참고 문서

- `EDU/09-loadbalancing.md` - Cilium LB 서브시스템 상세 문서
- [Maglev 논문 (Google, 2016)](https://research.google/pubs/pub44824/)
- [Cilium 공식 문서: Load Balancing](https://docs.cilium.io/en/stable/network/lb/)
