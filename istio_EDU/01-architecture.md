# 01. Istio 아키텍처

## 1. 전체 아키텍처 개요

Istio는 **컨트롤 플레인(Istiod)**과 **데이터 플레인(Envoy/Ztunnel)**으로 구성된 서비스 메시다.

```
┌─────────────────────────────────────────────────────────────────┐
│                    컨트롤 플레인 (Istiod)                         │
│  ┌──────────┐  ┌──────────────┐  ┌────────┐  ┌──────────────┐  │
│  │ Pilot    │  │ Config       │  │  CA    │  │ Sidecar      │  │
│  │ (xDS)    │  │ Controller   │  │(인증서)│  │ Injector     │  │
│  └────┬─────┘  └──────┬───────┘  └───┬────┘  └──────┬───────┘  │
│       │               │              │               │          │
│  ┌────┴───────────────┴──────────────┴───────────────┴──────┐   │
│  │              PushContext (설정 스냅샷)                      │   │
│  └──────────────────────┬───────────────────────────────────┘   │
└─────────────────────────┼───────────────────────────────────────┘
                          │ gRPC (xDS)
          ┌───────────────┼───────────────┐
          │               │               │
          ▼               ▼               ▼
    ┌───────────┐   ┌───────────┐   ┌───────────┐
    │  Envoy    │   │  Envoy    │   │  Ztunnel   │
    │ (Sidecar) │   │ (Gateway) │   │ (Ambient)  │
    └─────┬─────┘   └─────┬─────┘   └─────┬─────┘
          │               │               │
    ┌─────┴─────┐   ┌─────┴─────┐   ┌─────┴─────┐
    │  App Pod  │   │  Ingress  │   │  App Pod  │
    │  (사이드카)│   │  Gateway  │   │ (Ambient) │
    └───────────┘   └───────────┘   └───────────┘
```

## 2. Istiod 내부 구조

Istiod는 단일 바이너리에 여러 서브시스템을 통합한 컨트롤 플레인이다.

### 2.1 핵심 서브시스템

```
Istiod (pilot-discovery)
├── Pilot (xDS Server)
│   ├── DiscoveryServer — ADS/Delta XDS gRPC 스트림 관리
│   ├── ConfigGenerator — CDS/LDS/RDS/EDS 리소스 생성
│   └── PushQueue — 프록시별 설정 푸시 큐
├── Config Controller
│   ├── CRD Controller — VirtualService, DestinationRule 등 CRD 감시
│   ├── ServiceEntry Controller — 외부 서비스 등록
│   └── File/MCP Controller — 파일/MCP 기반 설정 소스
├── Service Registry
│   ├── Kubernetes Controller — Service, EndpointSlice, Pod 감시
│   ├── Aggregate Controller — 다중 레지스트리 통합
│   └── Ambient Index — Ambient 메시 워크로드 인덱싱
├── Certificate Authority (CA)
│   ├── Self-Signed CA — 자체 서명 루트 CA
│   ├── Plugged-In CA — 외부 CA 연동
│   └── CA Server — CSR 서명 gRPC 서비스
├── Sidecar Injector
│   ├── Webhook — MutatingWebhookConfiguration
│   └── Template Engine — Go 템플릿 기반 패치 생성
└── Validation Webhook
    └── 설정 리소스 검증
```

### 2.2 주요 포트

| 포트 | 프로토콜 | 용도 |
|------|---------|------|
| 8080 | HTTP | 디버그, readiness, gRPC 멀티플렉싱 |
| 15010 | gRPC | xDS 평문 (테스트용) |
| 15012 | gRPC+TLS | xDS 보안 채널 (기본) |
| 15014 | HTTP | Prometheus 메트릭 |
| 15017 | HTTPS | 웹훅 (인젝션, 검증) |

## 3. Istiod 초기화 흐름

`pilot/cmd/pilot-discovery/main.go` → `app.NewRootCommand()` → `bootstrap.NewServer()` → `Start()`

### 3.1 NewServer() 초기화 순서

```
NewServer(args)
│
├─ Phase 1: 기반 설정
│  ├─ model.Environment 생성 (Aggregate ServiceDiscovery)
│  ├─ xds.NewDiscoveryServer() — xDS 서버 인스턴스
│  └─ core.NewConfigGenerator() — Envoy 설정 생성기
│
├─ Phase 2: 서버 초기화
│  ├─ initReadinessProbes() — /ready 엔드포인트
│  ├─ initServers() — gRPC, HTTP, 모니터링 서버
│  └─ serveHTTP() — HTTP 리스너 시작
│
├─ Phase 3: Kubernetes 연동
│  ├─ initKubeClient() — K8s REST 클라이언트
│  └─ NamespaceFilter — 네임스페이스 필터링
│
├─ Phase 4: 설정 로딩
│  ├─ initMeshConfiguration() — mesh config (파일/ConfigMap)
│  ├─ initMeshNetworks() — 네트워크 설정
│  └─ initMeshHandlers() — 변경 시 xDS 전체 푸시 트리거
│
├─ Phase 5: 인증서 & 트러스트
│  ├─ maybeCreateCA() — CA/RA 생성
│  ├─ initControllers() — Config/Service 컨트롤러
│  ├─ InitGenerators() — xDS 제너레이터 등록
│  └─ initWorkloadTrustBundle() — 멀티루트 트러스트
│
├─ Phase 6: 보안 채널
│  ├─ initIstiodCerts() — Istiod TLS 인증서
│  └─ initSecureDiscoveryService() — 보안 gRPC (15012)
│
├─ Phase 7: 웹훅
│  ├─ initSidecarInjector() — 사이드카 인젝션 웹훅
│  └─ initConfigValidation() — 설정 검증 웹훅
│
├─ Phase 8: 이벤트 핸들러
│  └─ initRegistryEventHandlers() — 서비스/설정 변경 → xDS 푸시
│
├─ Phase 9: 인증
│  └─ 인증기 등록 (ClientCert, JWT, XFCC)
│
└─ Phase 10: CA 서버 시작
   └─ startCA() — CSR 서명 gRPC 서비스
```

**소스 참조**: `pilot/pkg/bootstrap/server.go:230-433`

### 3.2 Start() 실행 순서

```go
// pilot/pkg/bootstrap/server.go:466-528
func (s *Server) Start(stop <-chan struct{}) error {
    // 1. 등록된 모든 컴포넌트 시작
    s.server.Start(stop)

    // 2. 캐시 동기화 대기
    //    - Multicluster Controller 동기화
    //    - Service Controller 동기화
    //    - Config Controller 동기화

    // 3. XDSServer.CachesSynced() — 연결 수락 시작

    // 4. 보안 gRPC 리스너 시작 (15012)
    // 5. 평문 gRPC 리스너 시작 (15010)
    // 6. HTTPS 웹훅 리스너 시작 (15017)

    // 7. waitForShutdown(stop) — 종료 시그널 대기
}
```

## 4. 데이터 플레인 아키텍처

### 4.1 사이드카 모드

```
┌──────────────────────────────────────┐
│  Application Pod                     │
│                                      │
│  ┌──────────┐    ┌────────────────┐  │
│  │ App      │    │ istio-proxy    │  │
│  │ Container│    │ (Envoy)        │  │
│  │          │    │                │  │
│  │  :8080 ──┼────┼→ :15001 (out) │  │
│  │          │    │  :15006 (in)  │  │
│  └──────────┘    │  :15090 (prom)│  │
│                  └───────┬────────┘  │
│                          │           │
│  ┌───────────────────────┴────────┐  │
│  │ pilot-agent                    │  │
│  │ ├─ SDS Server (인증서 제공)     │  │
│  │ ├─ xDS Proxy (설정 프록싱)     │  │
│  │ └─ Health Check               │  │
│  └────────────────────────────────┘  │
│                                      │
│  iptables/nftables 규칙:             │
│  ├─ PREROUTING → 15006 (인바운드)    │
│  └─ OUTPUT → 15001 (아웃바운드)      │
│      (UID 1337 제외 = Envoy 자체)    │
└──────────────────────────────────────┘
```

### 4.2 Ambient 모드

```
┌─────────────────────────────────────────────────┐
│  Kubernetes Node                                │
│                                                 │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐     │
│  │ App Pod  │  │ App Pod  │  │ App Pod  │     │
│  │ (no      │  │ (no      │  │ (no      │     │
│  │  sidecar)│  │  sidecar)│  │  sidecar)│     │
│  └─────┬────┘  └─────┬────┘  └─────┬────┘     │
│        │             │             │            │
│  ┌─────┴─────────────┴─────────────┴────────┐  │
│  │  Ztunnel (DaemonSet)                     │  │
│  │  ├─ L4 투명 프록시 (Rust)                 │  │
│  │  ├─ mTLS 암호화                           │  │
│  │  ├─ :15001 (아웃바운드)                    │  │
│  │  ├─ :15006 (인바운드 패스스루)              │  │
│  │  └─ :15008 (인바운드 HBONE)               │  │
│  └──────────────────────────────────────────┘  │
│                                                 │
│  (선택적) Waypoint Proxy:                       │
│  ┌──────────────────────────────────────────┐  │
│  │  Waypoint (Envoy)                        │  │
│  │  └─ L7 라우팅/정책 처리                    │  │
│  └──────────────────────────────────────────┘  │
└─────────────────────────────────────────────────┘
```

## 5. xDS 프로토콜과 설정 배포

### 5.1 xDS 리소스 타입

| 타입 | 약어 | 설명 | Envoy 적용 |
|------|------|------|-----------|
| Cluster Discovery Service | CDS | 업스트림 클러스터 정의 | 로드밸런서, 연결풀 |
| Endpoint Discovery Service | EDS | 클러스터 멤버 엔드포인트 | 실제 Pod IP:Port |
| Listener Discovery Service | LDS | 네트워크 리스너 | 포트 바인딩, 필터 체인 |
| Route Discovery Service | RDS | HTTP/TCP 라우트 | 경로 매칭, 가중치 |
| Secret Discovery Service | SDS | TLS 인증서 | mTLS 설정 |

### 5.2 설정 배포 파이프라인

```
┌─────────────────────────────────────────────────────────┐
│  설정 수집 (Config Ingestion)                            │
│  ├─ K8s CRD (VirtualService, DestinationRule, ...)      │
│  ├─ K8s 리소스 (Service, EndpointSlice, Pod)            │
│  └─ 파일/MCP 소스                                       │
└────────────────────┬────────────────────────────────────┘
                     │ ConfigUpdate → pushChannel
                     ▼
┌─────────────────────────────────────────────────────────┐
│  설정 변환 (Config Translation)                          │
│  ├─ 디바운싱 (100ms 조용 기간, 최대 10s)                  │
│  ├─ PushContext 재구성 (불변 스냅샷)                      │
│  └─ ConfigGenerator — CDS/LDS/RDS/EDS 생성               │
└────────────────────┬────────────────────────────────────┘
                     │ PushRequest → PushQueue
                     ▼
┌─────────────────────────────────────────────────────────┐
│  설정 서빙 (Config Serving)                              │
│  ├─ PushQueue.Dequeue() — 프록시별 순서대로               │
│  ├─ pushConnection() — CDS→EDS→LDS→RDS→SDS 순서          │
│  ├─ SotW (전체 상태) 또는 Delta (변경분만)                │
│  └─ gRPC 스트림으로 Envoy에 전송                         │
└─────────────────────────────────────────────────────────┘
```

## 6. 두 가지 메시 모드 비교

| 특성 | 사이드카 모드 | Ambient 모드 |
|------|-------------|-------------|
| **프록시 배치** | Pod마다 Envoy 사이드카 | 노드당 ztunnel + 선택적 waypoint |
| **Pod 수정** | 필요 (인젝션) | 불필요 |
| **리소스 오버헤드** | Pod당 ~100MB | 노드당 ~50MB |
| **기능 범위** | 전체 L7 | L4(ztunnel) + 선택적 L7(waypoint) |
| **설정 프로토콜** | 표준 xDS | 커스텀 xDS (Address, Authorization) |
| **네트워크 설정** | init 컨테이너 (NET_ADMIN) | CNI 플러그인 |
| **지원 정책** | 전체 Istio 정책 | L4 정책(ztunnel), L7 정책(waypoint) |

## 7. 핵심 설계 원칙

1. **투명성**: 애플리케이션 코드 변경 없이 트래픽 관리 — iptables 인터셉트와 IP 스푸핑
2. **제로 트러스트**: 모든 워크로드 간 통신 암호화 — SPIFFE 기반 mTLS
3. **점진적 도입**: 네임스페이스/Pod 단위 선택적 메시 참여
4. **확장성**: 디바운싱, 부분 푸시, 캐싱으로 대규모 클러스터 지원
5. **모듈성**: L4(ztunnel)와 L7(waypoint/sidecar) 분리, 필요에 따라 조합
6. **관측 가능성**: 표준 메트릭, 트레이싱, 인증서 로테이션 가시성
