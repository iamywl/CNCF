# PoC: Envoy Config Generation 시뮬레이션

## 개요

Istio Pilot이 VirtualService, DestinationRule 등의 사용자 설정을
Envoy가 이해할 수 있는 xDS 리소스(CDS, RDS, LDS)로 변환하는 과정을 시뮬레이션한다.

## 시뮬레이션 대상

| 구성 요소 | 실제 소스 경로 | 시뮬레이션 내용 |
|-----------|---------------|----------------|
| ConfigGenerator | `pilot/pkg/networking/core/configgen.go` | BuildClusters, BuildHTTPRoutes, BuildListeners |
| ClusterBuilder | `pilot/pkg/networking/core/cluster.go` | 서비스별/subset별 Envoy Cluster 생성 |
| RouteBuilder | `pilot/pkg/networking/core/httproute.go` | VirtualService → Envoy Route 변환 |
| ListenerBuilder | `pilot/pkg/networking/core/listener.go` | 포트별 Envoy Listener 생성 |
| BuildSubsetKey | `pilot/pkg/model/service.go` | 클러스터 이름 규칙 |

## 핵심 알고리즘

### 1. 클러스터 이름 규칙 (BuildSubsetKey)

```
형식: "direction|port|subset|hostname"
예시: "outbound|9080|v1|reviews.default.svc.cluster.local"
```

- direction: outbound 또는 inbound
- port: 서비스 포트 번호
- subset: DestinationRule의 subset 이름 (없으면 빈 문자열)
- hostname: 서비스 FQDN

### 2. DestinationRule → Subset Cluster 생성

```
DestinationRule (reviews)
├── subset: v1 (version=v1) → outbound|9080|v1|reviews.default...
├── subset: v2 (version=v2) → outbound|9080|v2|reviews.default...
└── subset: v3 (version=v3) → outbound|9080|v3|reviews.default...
```

### 3. VirtualService → Route 변환

```
VirtualService
├── match: prefix=/api/v1 → weighted_clusters [v1(80%), v2(20%)]
├── match: header[end-user]=jason → cluster v3
└── default → cluster v1
```

### 4. 리소스 간 참조 관계

```
LDS (Listener)  →  RDS (RouteConfig)  →  CDS (Cluster)
 0.0.0.0:9080   →  reviews-route      →  outbound|9080|v1|reviews...
```

## 실행 방법

```bash
go run main.go
```

## 시나리오

1. CDS: 서비스별 기본 클러스터 + DestinationRule subset 클러스터 생성
2. 클러스터 이름 파싱 (ParseSubsetKey)
3. RDS: VirtualService HTTP 규칙을 Envoy Route로 변환 (가중치, 헤더 매칭)
4. LDS: 포트별 Listener + virtualOutbound/virtualInbound 생성
5. CDS-RDS-LDS 간 참조 관계 시각화
