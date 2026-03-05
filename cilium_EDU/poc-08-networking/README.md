# PoC-08: 네트워킹 시뮬레이션 (VXLAN + Direct Routing)

## 개요

Cilium의 두 가지 네트워킹 모드를 시뮬레이션한다:
1. **VXLAN 터널 모드**: 오버레이 네트워크로 인프라 독립적 동작
2. **Direct Routing 모드**: 네이티브 라우팅으로 최고 성능

## 핵심 개념

### VXLAN 캡슐화 구조
```
[Outer Eth 14B][Outer IP 20B][Outer UDP 8B][VXLAN 8B][Inner Eth 14B][Inner IP 20B][Payload]
                                              ↑
                                   VNI = Security Identity
```

Cilium은 표준 VXLAN의 VNI(24비트) 필드에 Security Identity를 인코딩한다.
이를 통해 수신 노드에서 패킷의 출처 identity를 즉시 파악하고 정책 검사를 수행한다.

### FIB Lookup
커널의 `bpf_fib_lookup()` 헬퍼를 사용하여 라우팅 결정을 BPF 내에서 수행한다.
이를 통해 패킷이 커널 스택을 거치지 않고도 올바른 경로로 전송된다.

### 시나리오

| 시나리오 | 설명 |
|---------|------|
| 1 | Node A → Node B: VXLAN 캡슐화 과정 |
| 2 | Node B에서 수신: VXLAN 역캡슐화 및 identity 추출 |
| 3 | Direct Routing: 터널 없이 FIB 기반 라우팅 |
| 4 | 패킷 크기 비교: VXLAN 50바이트 오버헤드 분석 |

## 실행

```bash
go run main.go
```

## 관련 소스 코드

- `bpf/lib/encap.h` — VXLAN 캡슐화 (`__encap_and_redirect_with_nodeid`)
- `bpf/lib/nodeport.h` — FIB 조회 및 라우팅
- `pkg/datapath/linux/config/` — 네트워킹 모드 설정
- `pkg/maps/tunnel/tunnel.go` — 터널 맵 관리

## 학습 포인트

- VXLAN VNI 필드에 Security Identity를 인코딩하는 것이 Cilium의 핵심 설계
- Direct Routing은 50바이트 오버헤드를 절약하지만 네트워크 인프라 지원 필요
- FIB Lookup은 BPF 내에서 커널 라우팅 테이블을 직접 조회하는 메커니즘
- ECMP 분산을 위해 내부 패킷 해시 기반 소스 포트를 사용한다
