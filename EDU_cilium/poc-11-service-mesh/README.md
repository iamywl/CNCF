# PoC 11: Cilium 서비스 메시 및 프록시 서브시스템

Cilium의 서비스 메시 핵심 메커니즘을 직접 시뮬레이션한다:
xDS 프로토콜, DNS 프록시, L7 프록시 흐름, Gateway API, Per-node 프록시 모델.

---

## 구조

```
이 PoC가 재현하는 패턴:

┌──────────────────────────────────────────────────────────────────┐
│ xDS Control Plane (Demo 1)                                       │
│  Cilium Agent의 xDS 서버가 Listener/Route/Cluster/Endpoint/      │
│  Secret 리소스를 캐시에 저장하고 Envoy에 푸시하는 과정             │
│  실제 코드: pkg/envoy/xds_server.go, pkg/envoy/grpc.go          │
├──────────────────────────────────────────────────────────────────┤
│ DNS Proxy (Demo 2)                                               │
│  BPF가 DNS 쿼리를 가로채어 프록시가 패턴 매칭, 캐싱,              │
│  FQDN Identity 생성하는 과정                                      │
│  실제 코드: pkg/fqdn/cache.go, pkg/proxy/dns.go                 │
├──────────────────────────────────────────────────────────────────┤
│ L7 Proxy Flow (Demo 3)                                           │
│  패킷 → BPF 리다이렉트 → Envoy L7 정책 적용 → 포워딩/블로킹       │
│  실제 코드: pkg/proxy/proxy.go, pkg/proxy/envoyproxy.go         │
├──────────────────────────────────────────────────────────────────┤
│ Gateway API (Demo 4)                                             │
│  Gateway + HTTPRoute → CiliumEnvoyConfig(xDS) 변환               │
│  실제 코드: operator/pkg/gateway-api/gateway_reconcile.go        │
│            operator/pkg/model/translation/types.go               │
├──────────────────────────────────────────────────────────────────┤
│ Per-Node vs Sidecar (Demo 5)                                     │
│  노드당 1개 프록시 vs Pod당 사이드카 모델 비교                      │
├──────────────────────────────────────────────────────────────────┤
│ HTTP Server (Demo 6)                                             │
│  실제 HTTP 서버로 L7 프록시/DNS 프록시를 체험                       │
└──────────────────────────────────────────────────────────────────┘
```

## 실행 방법

```bash
cd EDU/poc-11-service-mesh

# PoC 실행 (모든 데모를 순차적으로 실행한 후 HTTP 서버 시작)
go run main.go
```

실행하면 6개의 데모가 순차적으로 출력된 후, HTTP 서버가 `localhost:19080`에서 시작된다.

## HTTP 엔드포인트 테스트

서버가 시작된 후 별도 터미널에서 테스트:

```bash
# L7 프록시: 허용되는 요청
curl http://localhost:19080/proxy \
  -H 'X-Target-Host: api.example.com' \
  -H 'X-Target-Path: /v1/users/1'

# L7 프록시: 차단되는 요청 (/admin 경로)
curl http://localhost:19080/proxy \
  -H 'X-Target-Host: api.example.com' \
  -H 'X-Target-Path: /admin'

# DNS 프록시: 허용된 도메인 조회
curl 'http://localhost:19080/dns?fqdn=api.example.com'

# DNS 프록시: 차단된 도메인 조회
curl 'http://localhost:19080/dns?fqdn=blocked.malware.com'

# 프록시 통계 조회
curl http://localhost:19080/stats

# xDS 캐시 요약 조회
curl http://localhost:19080/xds
```

## 핵심 메커니즘

### 1. xDS 프로토콜 (pkg/envoy/)

Cilium agent는 gRPC 기반 xDS 서버를 운영한다. 5가지 표준 리소스 타입과 2가지 Cilium 전용 타입을 관리한다:

| 타입 | 역할 |
|------|------|
| LDS (Listener) | 수신 포트, 필터 체인 정의 |
| RDS (Route) | HTTP 경로/헤더 매칭 규칙 |
| CDS (Cluster) | 업스트림 서비스 클러스터 |
| EDS (Endpoint) | 백엔드 IP:port 목록 |
| SDS (Secret) | TLS 인증서/키 |
| NPDS (NetworkPolicy) | Cilium 네트워크 정책 |
| NPHDS (NetworkPolicyHosts) | IP→Identity 매핑 |

각 타입마다 독립 캐시가 있고, 변경 시 버전이 증가하며 Envoy에 푸시된다.

### 2. DNS 프록시 (pkg/fqdn/, pkg/proxy/dns.go)

BPF 데이터패스가 Pod의 DNS 쿼리를 가로채어 DNS 프록시로 리다이렉트한다.
프록시는:
- 정책(matchPattern/matchName)에 따라 허용/거부 결정
- 허용된 응답의 IP를 캐싱 (TTL 관리)
- FQDN Identity를 생성하여 toFQDNs 정책에 활용

### 3. L7 프록시 흐름 (pkg/proxy/)

```
Pod → BPF(L3/L4 필터링) → Envoy(L7 정책) → BPF(재삽입) → Backend
```

- BPF가 L7 정책이 필요한 트래픽만 선택적으로 프록시 리다이렉트
- Envoy가 HTTP 경로, 헤더, 메서드 매칭 수행
- Cilium L7 정책 필터(cilium.l7policy)가 허용/거부 결정
- 처리 완료 후 패킷을 BPF 데이터패스에 재삽입

### 4. Gateway API (operator/pkg/gateway-api/)

Cilium operator가 Gateway API CRD를 감시하고:
1. Gateway + HTTPRoute/GRPCRoute/TLSRoute를 수집
2. 내부 Model로 변환
3. Translator가 CiliumEnvoyConfig + Service + EndpointSlice 생성
4. CEC의 xDS 리소스가 cilium-agent → Envoy로 전달

### 5. Per-Node 프록시 장점

Cilium은 노드당 1개의 공유 Envoy를 사용하여:
- 메모리 사용량 대폭 절감 (500 Pod 클러스터에서 97.5% 절약)
- 사이드카 주입/재시작 불필요
- BPF 기반 리다이렉트로 iptables 오버헤드 제거
- L7 정책 필요 트래픽만 선택적 프록시 경유

## 관련 Cilium 소스 파일

| 파일 | 역할 |
|------|------|
| `pkg/envoy/cell.go` | Envoy 프록시 Hive 모듈 |
| `pkg/envoy/xds_server.go` | XDSServer 인터페이스, xDS 캐시 초기화 |
| `pkg/envoy/grpc.go` | xDS gRPC 서비스 등록 (LDS/RDS/CDS/EDS/SDS) |
| `pkg/envoy/resources.go` | 리소스 타입 URL 상수, NPHDS 캐시 |
| `pkg/envoy/embedded_envoy.go` | 임베디드 Envoy 프로세스 관리 |
| `pkg/envoy/xds/server.go` | xDS 스트림 핸들러 |
| `pkg/envoy/xds/cache.go` | xDS 리소스 캐시 (버전 관리) |
| `pkg/proxy/proxy.go` | Proxy 매니저 (DNS/Envoy 통합) |
| `pkg/proxy/dns.go` | DNS 리다이렉트 구현 |
| `pkg/proxy/envoyproxy.go` | Envoy 리다이렉트 구현 |
| `pkg/proxy/redirect.go` | RedirectImplementation 인터페이스 |
| `pkg/proxy/crd.go` | CRD 리다이렉트 (CiliumEnvoyConfig) |
| `pkg/fqdn/doc.go` | FQDN 서브시스템 아키텍처 문서 |
| `pkg/fqdn/cache.go` | DNS 조회 캐시 |
| `pkg/k8s/apis/cilium.io/v2/cec_types.go` | CiliumEnvoyConfig CRD 타입 |
| `operator/pkg/gateway-api/cell.go` | Gateway API 모듈 |
| `operator/pkg/gateway-api/gateway_reconcile.go` | Gateway Reconcile 로직 |
| `operator/pkg/model/translation/types.go` | Translator 인터페이스 |
| `pkg/ztunnel/cell.go` | Ztunnel 모듈 (Istio Ambient 호환) |
| `pkg/ztunnel/xds/xds_server.go` | Ztunnel 전용 xDS/CA 서버 |
| `pkg/ztunnel/zds/server.go` | ZDS 서버 (Unix 소켓) |
