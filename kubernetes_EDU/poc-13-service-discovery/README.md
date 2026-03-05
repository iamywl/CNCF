# PoC-13: Service Discovery & 로드밸런싱 시뮬레이션

## 개요

Kubernetes Service의 핵심인 서비스 디스커버리, ClusterIP 할당, DNS 해석, 라운드 로빈 로드밸런싱을 시뮬레이션한다.

## 시뮬레이션 대상

| 컴포넌트 | 실제 소스 | 시뮬레이션 |
|----------|----------|-----------|
| Service/Endpoints 모델 | `pkg/apis/core/types.go` (Service, ServiceSpec, Endpoints, EndpointSubset) | 타입, 포트, Selector 기반 모델 |
| ClusterIP 할당 | `pkg/registry/core/service/ipallocator/range_alloc.go` | CIDR 범위 비트맵 할당 |
| NodePort 할당 | `pkg/registry/core/service/portallocator/portallocator.go` | 30000-32767 범위 순차 할당 |
| Endpoint 동기화 | `pkg/controller/endpoint/endpoints_controller.go` | Selector 매칭 → Ready/NotReady 분류 |
| DNS 해석 | CoreDNS (클러스터 애드온) | FQDN, 단축형, SRV 레코드 |
| 라운드 로빈 LB | `pkg/proxy/iptables/proxier.go` | 원자적 카운터 기반 라운드 로빈 |

## 핵심 알고리즘

```
Service Discovery 흐름:
  1. Service 생성 → ClusterIP 할당 (CIDR 범위에서 비트맵 기반)
  2. CoreDNS 등록: <svc>.<ns>.svc.cluster.local → ClusterIP
  3. EndpointController: Selector 매칭 Pod 탐색
     Ready Pod    → Endpoints.Subsets[].Addresses
     NotReady Pod → Endpoints.Subsets[].NotReadyAddresses
  4. kube-proxy: ClusterIP 트래픽 → Ready 엔드포인트 라운드 로빈 분배

Service 타입 계층:
  LoadBalancer ⊃ NodePort ⊃ ClusterIP
  - ClusterIP:    클러스터 내부 가상 IP
  - NodePort:     + 모든 노드의 고정 포트 (30000-32767)
  - LoadBalancer: + 외부 로드밸런서 IP
```

## 실행

```bash
go run main.go
```

## 데모 항목

1. **ClusterIP Service 생성**: 가상 IP 할당 및 포트 매핑
2. **DNS 해석**: FQDN, 단축형, SRV 레코드 조회
3. **Endpoint 자동 동기화**: Selector 매칭 Pod → Ready/NotReady 분류
4. **라운드 로빈 로드밸런싱**: Ready 엔드포인트 순환 분배
5. **NodePort Service**: 노드 IP:포트 접근 경로
6. **LoadBalancer Service**: 외부 IP 포함 3계층 접근
7. **Pod 변경 시 Endpoints 동적 갱신**: 추가/삭제/Ready 전환
8. **네임스페이스 격리**: 같은 이름, 다른 namespace의 독립 IP
