# PoC: VirtualService/DestinationRule 라우팅

## 개요

이 PoC는 Istio의 **VirtualService**와 **DestinationRule**이 트래픽 라우팅을 결정하는 과정을 시뮬레이션한다.
Istio에서 Pilot(istiod)은 이 두 리소스를 해석하여 Envoy 프록시에 라우팅 설정을 전달하는데,
이 PoC는 그 해석 알고리즘을 Go 표준 라이브러리만으로 재현한다.

## 시뮬레이션하는 핵심 개념

### 1. VirtualService 모델
- **Host 매칭**: 요청의 Host 헤더와 VirtualService의 hosts 목록을 비교
- **HTTP Route 매칭**: 순서대로 평가, 첫 번째 일치 사용
- **매칭 조건**: URI(exact/prefix/regex), Header, Method를 AND 결합
- **가중치 기반 라우팅**: 목적지별 가중치로 트래픽 분배
- **장애 주입**: 지연(delay)과 중단(abort)을 특정 비율로 주입

### 2. DestinationRule 모델
- **Subset**: 레이블 셀렉터로 엔드포인트 그룹 정의 (예: version=v1)
- **Traffic Policy**: 로드밸런싱 알고리즘, 연결 풀, 이상 감지 설정
- **정책 상속**: 전역 정책 + subset 레벨 정책 (subset이 우선)

### 3. Route Resolution 흐름
```
요청 수신
  -> Host 매칭 (VirtualService 선택)
  -> HTTP Route 순서 평가 (첫 매칭 사용)
  -> 장애 주입 평가 (abort/delay)
  -> 가중치 기반 목적지 선택
  -> DestinationRule에서 subset 레이블 조회
  -> 엔드포인트 필터링 (레이블 매칭)
  -> 로드밸런서로 최종 엔드포인트 선택
```

## 실행 방법

```bash
cd istio_EDU/poc-traffic-routing
go run main.go
```

## 시나리오 목록

| 시나리오 | 설명 | Istio 기능 |
|---------|------|-----------|
| 1. 카나리 라우팅 | v1에 80%, v2에 20% 분배 | VirtualService weight |
| 2. 헤더 기반 라우팅 | end-user: jason -> v2 | HTTPMatchRequest headers |
| 3. URI Prefix 라우팅 | /api/v1/* -> v1, /api/v2/* -> v2 | HTTPMatchRequest uri.prefix |
| 4. 복합 매칭 | URI + Header + Method AND 결합 | 다중 매칭 조건 |
| 5. 장애 주입 | HTTP 503 중단, 5초 지연 주입 | HTTPFaultInjection |
| 6. LB 정책 | ROUND_ROBIN vs RANDOM | DestinationRule trafficPolicy |
| 7. 경계 케이스 | 호스트 불일치 | 매칭 실패 처리 |

## 예상 출력

```
==========================================================
 Istio VirtualService/DestinationRule 라우팅 시뮬레이션
==========================================================

[시나리오 1] 가중치 기반 카나리 라우팅
----------------------------------------------------------
  요청: GET /api/reviews (Host: reviews.default.svc.cluster.local)
    -> 라우트: canary-route
    -> 목적지: reviews.default.svc.cluster.local (subset: v1)
    -> 엔드포인트: 10.0.1.1:9080 (labels: map[app:reviews version:v1])

  [통계] 10회 요청 결과: v1=8회, v2=2회

[시나리오 2] 헤더 기반 라우팅 (A/B 테스트)
----------------------------------------------------------
  요청: GET /api/reviews (Host: ..., Headers: {end-user: jason})
    -> 라우트: jason-route
    -> 목적지: ... (subset: v2)

  요청: GET /api/reviews (Host: ..., Headers: {end-user: alice})
    -> 라우트: default-route
    -> 목적지: ... (subset: v1)

[시나리오 5] 장애 주입
----------------------------------------------------------
  요청: GET /api/ratings (Headers: {end-user: jason})
    -> [장애 주입] HTTP 503 중단

  요청: GET /api/ratings (Headers: {end-user: tester})
    -> [장애 주입] 5s 지연
    -> 목적지: ratings.default.svc.cluster.local (subset: v1)
```

## 참조 Istio 소스 코드

| 소스 파일 | 설명 |
|-----------|------|
| `pilot/pkg/model/virtualservice.go` | VirtualService 모델, 호스트 매칭, delegate 병합 |
| `pilot/pkg/networking/core/route/route.go` | Envoy 라우트 변환, VirtualHost 구성 |
| `networking/v1/virtual_service.pb.go` | VirtualService protobuf 정의 |
| `networking/v1/destination_rule.pb.go` | DestinationRule protobuf 정의 |

### 주요 Istio 함수 매핑

| PoC 함수/구조체 | Istio 원본 | 설명 |
|-----------------|-----------|------|
| `VirtualService` | `networking.VirtualService` | VS protobuf 모델 |
| `DestinationRule` | `networking.DestinationRule` | DR protobuf 모델 |
| `HTTPMatchRequest.Matches()` | route.go 의 매칭 로직 | HTTP 요청 매칭 |
| `RouteEngine.Resolve()` | `BuildSidecarVirtualHostWrapper()` | 라우트 해석 |
| `findVirtualService()` | `SelectVirtualServices()` | VS 호스트 매칭 |
| `selectDestination()` | envoy weighted_clusters | 가중치 기반 선택 |
| `selectEndpoint()` | envoy LB 알고리즘 | 로드밸런싱 |
| `evaluateFault()` | `HTTPFaultInjection` 처리 | 장애 주입 |

## 아키텍처 다이어그램

```
┌─────────────────────────────────────────────────────────────────┐
│                  istiod (Pilot) Control Plane                    │
│                                                                  │
│  ┌───────────────┐    ┌──────────────────┐                      │
│  │ VirtualService│    │ DestinationRule   │                      │
│  │               │    │                  │                      │
│  │ hosts: [...]  │    │ host: reviews    │                      │
│  │ http:         │    │ subsets:         │                      │
│  │   - match:    │    │   - name: v1     │                      │
│  │     uri:      │    │     labels:      │                      │
│  │     headers:  │    │       version:v1 │                      │
│  │   route:      │    │   - name: v2     │                      │
│  │     weight:80 │    │     labels:      │                      │
│  │     weight:20 │    │       version:v2 │                      │
│  └───────┬───────┘    └────────┬─────────┘                      │
│          │                     │                                 │
│          └────────┬────────────┘                                 │
│                   │ xDS (RDS/CDS/EDS)                            │
│                   ▼                                              │
│  ┌─────────────────────────────────────────────────────────┐    │
│  │           Route Translation Engine                       │    │
│  │                                                          │    │
│  │  VS + DR  ──>  Envoy Route / Cluster / Endpoint 설정     │    │
│  └──────────────────────────┬───────────────────────────────┘    │
└─────────────────────────────┼────────────────────────────────────┘
                              │
                    ┌─────────▼──────────┐
                    │   Envoy Proxy      │
                    │                    │
                    │  Route Config:     │
                    │  /api/v1/* -> v1   │
                    │  /api/v2/* -> v2   │
                    │  * -> v1 (default) │
                    └─────────┬──────────┘
                              │
              ┌───────────────┼───────────────┐
              ▼               ▼               ▼
        ┌──────────┐   ┌──────────┐   ┌──────────┐
        │ Pod v1-a │   │ Pod v2-a │   │ Pod v3-a │
        │ 10.0.1.1 │   │ 10.0.2.1 │   │ 10.0.3.1 │
        │ version: │   │ version: │   │ version: │
        │   v1     │   │   v2     │   │   v3     │
        └──────────┘   └──────────┘   └──────────┘
```

## 라우트 해석 시퀀스

```
  Incoming Request                     Route Engine
       │                                    │
       │──── Host: reviews.svc ───────────>│
       │                                    │
       │     ┌────────────────────────────┐ │
       │     │ 1. Host 매칭               │ │
       │     │    reviews.svc -> VS 선택   │ │
       │     └────────────────────────────┘ │
       │                                    │
       │     ┌────────────────────────────┐ │
       │     │ 2. HTTP Route 순서 평가     │ │
       │     │    match[0]: uri prefix    │ │
       │     │    match[1]: header exact  │ │
       │     │    첫 번째 매칭 사용        │ │
       │     └────────────────────────────┘ │
       │                                    │
       │     ┌────────────────────────────┐ │
       │     │ 3. 가중치 목적지 선택       │ │
       │     │    v1: 80%, v2: 20%        │ │
       │     │    -> v1 선택               │ │
       │     └────────────────────────────┘ │
       │                                    │
       │     ┌────────────────────────────┐ │
       │     │ 4. DR Subset 레이블 조회    │ │
       │     │    v1 -> {version: v1}     │ │
       │     └────────────────────────────┘ │
       │                                    │
       │     ┌────────────────────────────┐ │
       │     │ 5. 엔드포인트 필터 + LB     │ │
       │     │    version=v1 -> 2개 매칭   │ │
       │     │    ROUND_ROBIN -> 10.0.1.1 │ │
       │     └────────────────────────────┘ │
       │                                    │
       │<─── Route to 10.0.1.1:9080 ──────│
       │                                    │
```
