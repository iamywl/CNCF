# PoC-21: Cilium Gateway API 시뮬레이션

## 개요

이 PoC는 Cilium이 Kubernetes Gateway API를 구현하는 핵심 메커니즘을 시뮬레이션한다.
GatewayClass, Gateway, HTTPRoute 리소스의 계층 구조와 리컨실레이션 파이프라인을 재현하며,
Gateway API 리소스가 내부 모델로 변환(Ingestion)된 후 CiliumEnvoyConfig와 LoadBalancer Service로
번역(Translation)되는 전체 과정을 보여준다. 크로스 네임스페이스 참조 시 ReferenceGrant를 통한
보안 검증도 포함한다.

## 배경 지식

### Gateway API란?

Gateway API는 Kubernetes의 차세대 인그레스 표준으로, 기존 Ingress 리소스의 한계를 극복하기 위해
SIG-Network에서 설계했다. 역할 기반 리소스 분리(GatewayClass/Gateway/Route)를 통해
인프라 관리자, 클러스터 운영자, 애플리케이션 개발자가 각자의 관심사를 독립적으로 관리할 수 있다.

### 기존 Ingress와의 차이점

| 항목 | Ingress | Gateway API |
|------|---------|-------------|
| 리소스 계층 | 단일 Ingress 리소스 | GatewayClass -> Gateway -> xRoute 3계층 |
| 역할 분리 | 없음 (모든 설정이 하나에 혼합) | 인프라/운영자/개발자 역할별 리소스 분리 |
| 프로토콜 지원 | HTTP/HTTPS만 | HTTP, gRPC, TCP, UDP, TLS 등 다양 |
| 크로스 네임스페이스 | 제한적 | ReferenceGrant를 통한 명시적 허용 |
| 트래픽 분할 | 별도 어노테이션 필요 | Weight 기반 네이티브 지원 |
| 헤더 매칭 | 불가능 | HTTPRouteMatch로 네이티브 지원 |

### Cilium의 Gateway API 구현 방식

Cilium은 Kubernetes Operator 내에서 Gateway API 컨트롤러를 실행한다.
Gateway API 리소스를 감시하다가 변경이 감지되면 리컨실레이션이 트리거된다.
핵심 흐름은 두 단계로 나뉜다:

1. **Ingestion**: Gateway API 리소스(Gateway + HTTPRoute)를 Cilium 내부 모델(`model.Model`)로 변환
2. **Translation**: 내부 모델을 CiliumEnvoyConfig(Envoy 프록시 설정)와 LoadBalancer Service로 변환

최종적으로 Envoy 프록시가 실제 트래픽 라우팅을 수행한다.

## 시뮬레이션하는 개념

| 개념 | 실제 Cilium 코드 | 시뮬레이션 방식 |
|------|-----------------|----------------|
| GatewayClass 리컨실 | `operator/pkg/gateway-api/gatewayclass_reconcile.go` | `ReconcileGatewayClass()` - 컨트롤러 이름 매칭 후 Accepted 설정 |
| Gateway 리컨실 파이프라인 | `operator/pkg/gateway-api/gateway_reconcile.go` | `ReconcileGateway()` - 6단계 파이프라인 (GatewayClass 확인 -> Route 수집 -> Ingestion -> Translation -> 리소스 생성 -> 상태 업데이트) |
| Ingestion (리소스 변환) | `operator/pkg/gateway-api/ingestion/` | `ingest()` - Gateway + HTTPRoute를 HTTPListener 내부 모델로 변환 |
| Translation (Envoy 설정 생성) | `operator/pkg/gateway-api/translation/` | `translate()` - 내부 모델을 CiliumEnvoyConfig + LBService로 변환 |
| ReferenceGrant 검증 | `operator/pkg/gateway-api/helpers.go` | `isReferenceGranted()` - 크로스 네임스페이스 참조 허용 여부 확인 |
| Route 필터링 | `operator/pkg/gateway-api/gateway_reconcile.go` | `filterHTTPRoutesByGateway()` - ParentRef 매칭 + AllowedRoutes 정책 적용 |

## 아키텍처 다이어그램

```
┌──────────────────────────────────────────────────────────────┐
│                  Gateway API 리컨실레이션 파이프라인              │
│                                                              │
│  [GatewayClass]     [Gateway]       [HTTPRoute]              │
│       │                │                │                    │
│       ▼                ▼                ▼                    │
│  ┌──────────┐    ┌──────────────────────────┐               │
│  │ Accepted? │    │  ReconcileGateway()       │               │
│  │ 컨트롤러  │    │                          │               │
│  │ 이름 매칭 │    │  Step 1: GatewayClass 확인│               │
│  └──────────┘    │  Step 2: Route 수집/필터  │               │
│                  │  Step 3: Ingestion        │               │
│                  │  Step 4: Translation      │               │
│                  │  Step 5: 리소스 생성       │               │
│                  │  Step 6: 상태 업데이트     │               │
│                  └────────────┬─────────────┘               │
│                               │                              │
│              ┌────────────────┼────────────────┐             │
│              ▼                ▼                ▼             │
│     ┌──────────────┐  ┌───────────┐  ┌──────────────┐      │
│     │ HTTPListener  │  │           │  │              │      │
│     │ (내부 모델)   │→│ Translate │→│ CiliumEnvoy  │      │
│     │              │  │           │  │ Config       │      │
│     └──────────────┘  └───────────┘  └──────────────┘      │
│                                             │                │
│                                      ┌──────┴──────┐        │
│                                      ▼             ▼        │
│                              ┌────────────┐ ┌──────────┐    │
│                              │ Envoy      │ │ LB       │    │
│                              │ Listeners  │ │ Service  │    │
│                              │ Routes     │ │ (ports)  │    │
│                              │ Clusters   │ │          │    │
│                              └────────────┘ └──────────┘    │
└──────────────────────────────────────────────────────────────┘

크로스 네임스페이스 참조 흐름:

  [other-ns/HTTPRoute] ──ParentRef──▶ [default/Gateway]
          │                                   ▲
          │ 참조 허용?                         │
          ▼                                   │
  [default/ReferenceGrant]                    │
    From: other-ns/HTTPRoute ────허용────────┘
    To: Gateway
```

## 코드 해설

### 1. GatewayAPIController 구조체

```go
type GatewayAPIController struct {
    gatewayClasses  map[string]*GatewayClass
    gateways        map[string]*Gateway
    httpRoutes      map[string]*HTTPRoute
    referenceGrants map[string]*ReferenceGrant
    cecStore        map[string]*CiliumEnvoyConfig
    svcStore        map[string]*LBService
}
```

Gateway API 컨트롤러의 전체 상태를 관리하는 중심 구조체다. 실제 Cilium에서는 Kubernetes informer/cache를
통해 리소스를 관리하지만, 이 시뮬레이션에서는 map으로 단순화했다. 입력 리소스(GatewayClass, Gateway,
HTTPRoute, ReferenceGrant)와 출력 리소스(CiliumEnvoyConfig, LBService)를 모두 보유하여
리컨실레이션 전체 사이클을 한 곳에서 추적할 수 있게 했다.

### 2. ReconcileGateway - 6단계 리컨실 파이프라인

`ReconcileGateway()`는 실제 Cilium의 `gateway_reconcile.go`에 있는 리컨실 로직을 재현한다.
GatewayClass 확인 -> Route 수집 -> Ingestion -> Translation -> 리소스 생성 -> 상태 업데이트의
6단계를 순차적으로 실행한다. 실제 Cilium도 동일한 순서로 처리하며, 각 단계에서 실패하면
Gateway의 상태 조건(Accepted, Programmed)을 적절히 설정하고 조기 반환한다.

### 3. filterHTTPRoutesByGateway - Route 매칭과 보안 검증

이 함수는 두 가지 핵심 검증을 수행한다. 첫째, HTTPRoute의 ParentRef가 현재 Gateway를
가리키는지 확인한다. 둘째, 크로스 네임스페이스 참조인 경우 ReferenceGrant가 존재하는지,
Gateway의 AllowedRoutes 정책(All/Same/Selector)이 허용하는지 확인한다.
이 이중 검증은 Gateway API의 보안 모델의 핵심이며, 실제 Cilium에서도 동일한 패턴을 사용한다.

### 4. ingest - Gateway API에서 내부 모델로 변환

`ingest()`는 Cilium의 Ingestion 레이어를 재현한다. Gateway의 Listener와 매칭된 HTTPRoute를
결합하여 `HTTPListener` 내부 모델을 생성한다. 이 단계에서 Kubernetes API 리소스의 복잡한
구조가 Cilium이 처리하기 쉬운 단순한 형태로 정규화된다. 실제 Cilium에서는
`operator/pkg/gateway-api/ingestion/` 패키지가 이 역할을 담당한다.

### 5. translate - 내부 모델에서 Envoy 설정 생성

`translate()`는 내부 모델을 CiliumEnvoyConfig(Envoy 리스너, 라우트, 클러스터)와
LoadBalancer Service로 변환한다. Envoy의 용어로 매핑하면:
Listener는 포트/프로토콜 바인딩, Route는 도메인+경로 매칭 규칙,
Cluster는 실제 백엔드 서비스 엔드포인트다. `cilium-gateway-{name}` 네이밍 컨벤션도
실제 Cilium과 동일하다.

## 실행 방법 및 예상 출력

```bash
go run main.go
```

주요 출력 발췌:

```
━━━ 시나리오 1: GatewayClass 등록 ━━━
  [GatewayClass] 리컨실: cilium (controller=io.cilium/gateway-controller)
    → Accepted: true (Cilium 컨트롤러 매칭)
  [GatewayClass] 리컨실: nginx (controller=k8s.io/nginx-controller)
    → Accepted: false (컨트롤러 불일치)

━━━ 시나리오 2: Gateway + HTTPRoute 리컨실레이션 ━━━
  [Gateway] 리컨실: default/my-gateway
    매칭된 HTTPRoute: 1개
    Ingestion 결과: HTTPListener 2개
    CiliumEnvoyConfig 생성: default/cilium-gateway-my-gateway
    LoadBalancer Service 생성: default/cilium-gateway-my-gateway (ports: http/80, https/443)
    → Accepted: true, Programmed: true
    → Address: 198.51.100.1

━━━ 시나리오 3: 크로스 네임스페이스 Route (ReferenceGrant 없음) ━━━
    Route other-ns/cross-route: ReferenceGrant 없음 → 건너뜀

━━━ 시나리오 4: ReferenceGrant 추가 후 재리컨실 ━━━
    매칭된 HTTPRoute: 2개
```

시나리오 3에서 크로스 네임스페이스 Route가 ReferenceGrant 없이 거부되었다가,
시나리오 4에서 ReferenceGrant를 추가한 후 성공적으로 매칭되는 흐름을 확인할 수 있다.

## 핵심 포인트

1. **역할 기반 리소스 분리**: GatewayClass(인프라) -> Gateway(운영자) -> HTTPRoute(개발자)의
   3계층 구조는 각 역할이 자신의 관심사만 관리할 수 있게 하며, 이것이 기존 Ingress 대비
   Gateway API의 가장 큰 장점이다.

2. **Ingestion-Translation 2단계 파이프라인**: Cilium은 Gateway API 리소스를 바로 Envoy 설정으로
   변환하지 않고, 중간에 내부 모델을 거친다. 이 설계 덕분에 다양한 입력(Ingress, Gateway API)을
   동일한 Translation 레이어로 처리할 수 있다.

3. **ReferenceGrant 보안 모델**: 크로스 네임스페이스 참조는 명시적 허가 없이는 차단된다.
   대상 네임스페이스에 ReferenceGrant가 존재해야만 다른 네임스페이스의 Route가 Gateway에
   연결될 수 있다. 이는 멀티테넌트 환경에서의 보안을 보장한다.

4. **CiliumEnvoyConfig를 통한 Envoy 제어**: Gateway API 리소스의 최종 산출물은
   CiliumEnvoyConfig CRD다. Cilium은 이 CRD를 통해 Envoy 프록시의 리스너, 라우트,
   클러스터를 선언적으로 관리한다.

5. **Weight 기반 트래픽 분할**: HTTPRoute의 BackendRef에 Weight를 지정하여 여러 백엔드 간
   트래픽을 비율로 분배할 수 있다. 카나리 배포나 A/B 테스트에 활용된다.

## 실제 Cilium과의 차이점

| 항목 | 이 시뮬레이션 | 실제 Cilium |
|------|-------------|------------|
| 리소스 감시 | map에 직접 저장 | Kubernetes informer/cache 기반 watch |
| 동시성 | 순차 실행 | controller-runtime의 work queue 기반 동시 리컨실 |
| Envoy 설정 | 구조체 출력만 | CiliumEnvoyConfig CRD를 생성하고 Envoy xDS 서버가 프록시에 전달 |
| TLS 처리 | TLSConfig 구조체만 존재 | Secret에서 인증서 로드, Envoy SDS(Secret Discovery Service) 연동 |
| 상태 관리 | 단순 bool 플래그 | Kubernetes status subresource에 조건(Condition) 상세 기록 |
| LB-IPAM | 하드코딩된 IP | Cilium LB-IPAM이 IP 풀에서 동적 할당 |
| 리스너 격리 | 모든 Route가 모든 리스너에 매칭 | 리스너별 hostname, port, protocol에 따른 정밀 매칭 |
| 에러 처리 | 조기 반환만 | 상세한 이벤트 기록, 메트릭, 조건별 상태 메시지 |
| GRPCRoute/TLSRoute | 미구현 | HTTPRoute 외 GRPCRoute, TLSRoute, TCPRoute 등 지원 |
