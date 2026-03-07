# Istio 교육 자료 (EDU)

> Istio 서비스 메시의 내부 구현을 소스코드 수준에서 분석한 교육 자료

## Istio란?

Istio는 마이크로서비스 간 트래픽을 투명하게 관리하는 **오픈소스 서비스 메시**다. 기존 분산 애플리케이션 위에 투명하게 레이어링되어 서비스 간 통신의 보안, 연결, 모니터링을 통합적으로 제공한다.

### 핵심 컴포넌트

| 컴포넌트 | 역할 | 구현 언어 |
|----------|------|----------|
| **Istiod** | 컨트롤 플레인 — 서비스 디스커버리, 설정 관리, 인증서 관리 | Go |
| **Envoy** | 사이드카 프록시 — L7 라우팅, 로드밸런싱, mTLS, 텔레메트리 | C++ (외부) |
| **Ztunnel** | 노드 로컬 프록시 — Ambient 메시 모드의 L4 투명 프록시 | Rust (외부) |
| **istioctl** | CLI 도구 — 설치, 분석, 디버깅 | Go |
| **CNI Plugin** | 네트워크 설정 — iptables/nftables 트래픽 인터셉트 | Go |

### 핵심 기능

- **트래픽 관리**: VirtualService/DestinationRule 기반 L7 라우팅, 가중치 분배, 서킷 브레이커
- **보안**: SPIFFE 기반 워크로드 ID, 자동 mTLS, 인증/인가 정책
- **관측성**: 분산 트레이싱, 메트릭 수집, 액세스 로깅
- **Ambient 메시**: 사이드카 없는 서비스 메시 (ztunnel + waypoint)

---

## 문서 목차

### 기본 문서

| # | 문서 | 내용 |
|---|------|------|
| 01 | [아키텍처](01-architecture.md) | 전체 아키텍처, 컴포넌트 관계, Istiod 초기화 흐름 |
| 02 | [데이터 모델](02-data-model.md) | Service, Proxy, PushContext 등 핵심 데이터 구조 |
| 03 | [시퀀스 다이어그램](03-sequence-diagrams.md) | xDS 푸시, mTLS 핸드셰이크, 사이드카 인젝션 흐름 |
| 04 | [코드 구조](04-code-structure.md) | 디렉토리 구조, 빌드 시스템, 의존성 |
| 05 | [핵심 컴포넌트](05-core-components.md) | xDS 서버, ConfigGenerator, 서비스 레지스트리 동작 원리 |
| 06 | [운영 가이드](06-operations.md) | 배포, 설정, 모니터링, 트러블슈팅 |

### 심화 문서

| # | 문서 | 내용 |
|---|------|------|
| 07 | [xDS 디스커버리 서비스](07-xds-discovery.md) | ADS/Delta XDS, 디바운싱, 푸시 큐, 커넥션 관리 |
| 08 | [Envoy 설정 생성](08-envoy-config-generation.md) | CDS/LDS/RDS/EDS 생성 파이프라인, EnvoyFilter |
| 09 | [서비스 레지스트리](09-service-registry.md) | K8s 컨트롤러, EndpointSlice, Aggregate 패턴 |
| 10 | [보안과 mTLS](10-security-mtls.md) | CA, SPIFFE, SDS, 인증서 로테이션, PeerAuthentication |
| 11 | [사이드카 인젝션과 CNI](11-sidecar-injection-cni.md) | 웹훅 인젝션, 템플릿 시스템, iptables/nftables |
| 12 | [트래픽 관리](12-traffic-management.md) | VirtualService→Route, DestinationRule→Cluster 변환 |
| 13 | [Ambient 메시](13-ambient-mesh.md) | ztunnel, HBONE, waypoint, ZDS API |
| 14 | [Pilot Agent와 SDS](14-pilot-agent-sds.md) | 사이드카 에이전트, Secret Manager, 인증서 캐시 |
| 15 | [istioctl CLI](15-istioctl.md) | proxy-status, proxy-config, analyze, kube-inject |
| 16 | [Operator와 설치](16-operator-installation.md) | IstioOperator, 프로파일, Helm 렌더링 |
| 17 | [멀티클러스터](17-multicluster.md) | 클러스터 간 서비스 디스커버리, 네트워크 게이트웨이 |
| 18 | [관측성과 텔레메트리](18-observability.md) | 메트릭, 트레이싱, 액세스 로깅 통합 |

### PoC (Proof of Concept)

| # | PoC | 핵심 개념 |
|---|-----|----------|
| 01 | [poc-xds-server](poc-xds-server/) | xDS 프로토콜 시뮬레이션 (ADS + 디바운싱) |
| 02 | [poc-envoy-config](poc-envoy-config/) | Envoy 설정 생성 파이프라인 (CDS/LDS/RDS/EDS) |
| 03 | [poc-service-registry](poc-service-registry/) | 서비스 레지스트리와 Aggregate 패턴 |
| 04 | [poc-mtls](poc-mtls/) | SPIFFE 기반 mTLS 핸드셰이크 |
| 05 | [poc-certificate-rotation](poc-certificate-rotation/) | 인증서 자동 로테이션과 SDS 푸시 |
| 06 | [poc-sidecar-injection](poc-sidecar-injection/) | 사이드카 인젝션 패치 생성 |
| 07 | [poc-traffic-routing](poc-traffic-routing/) | VirtualService/DestinationRule 라우팅 |
| 08 | [poc-circuit-breaker](poc-circuit-breaker/) | 서킷 브레이커와 아웃라이어 디텍션 |
| 09 | [poc-iptables-redirect](poc-iptables-redirect/) | iptables 트래픽 인터셉트 규칙 생성 |
| 10 | [poc-push-context](poc-push-context/) | PushContext 스냅샷과 인덱싱 |
| 11 | [poc-ambient-ztunnel](poc-ambient-ztunnel/) | ztunnel L4 프록시와 HBONE 터널링 |
| 12 | [poc-config-debounce](poc-config-debounce/) | 설정 변경 디바운싱과 병합 |
| 13 | [poc-endpoint-builder](poc-endpoint-builder/) | 엔드포인트 빌더와 로컬리티 라우팅 |
| 14 | [poc-auth-policy](poc-auth-policy/) | 인증/인가 정책 평가 엔진 |
| 15 | [poc-waypoint-proxy](poc-waypoint-proxy/) | Waypoint 프록시 L7 라우팅 |
| 16 | [poc-multicluster](poc-multicluster/) | 멀티클러스터 서비스 디스커버리 |

---

## 소스코드 참조

- **저장소**: [istio/istio](https://github.com/istio/istio) (GitHub)
- **소스 언어**: Go
- **로컬 경로**: `/Users/ywlee/sideproejct/CNCF/istio/`
- **주요 진입점**:
  - `pilot/cmd/pilot-discovery/main.go` → Istiod (컨트롤 플레인)
  - `pilot/cmd/pilot-agent/main.go` → Pilot Agent (사이드카 에이전트)
  - `istioctl/cmd/istioctl/main.go` → istioctl CLI
  - `cni/cmd/istio-cni/main.go` → CNI 플러그인
