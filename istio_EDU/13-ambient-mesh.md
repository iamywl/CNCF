# 13. Ambient Mesh Deep-Dive

## 목차
1. [Ambient 메시 개요](#1-ambient-메시-개요)
2. [Ztunnel 아키텍처](#2-ztunnel-아키텍처)
3. [HBONE 프로토콜](#3-hbone-프로토콜)
4. [트래픽 경로](#4-트래픽-경로)
5. [Waypoint 프록시](#5-waypoint-프록시)
6. [Workload API](#6-workload-api)
7. [Authorization API](#7-authorization-api)
8. [ZDS API](#8-zds-api)
9. [PeerAuthentication 변환](#9-peerauthentication-변환)
10. [Ztunnel 롤링 리스타트](#10-ztunnel-롤링-리스타트)
11. [Pod 시작/종료 흐름](#11-pod-시작종료-흐름)

---

## 1. Ambient 메시 개요

### 1.1 사이드카 모델의 한계

전통적인 Istio 서비스 메시는 각 워크로드 Pod에 Envoy 사이드카 프록시를 주입하는 방식으로 동작한다. 이 모델은 다음과 같은 문제를 가진다:

| 문제점 | 설명 |
|--------|------|
| 리소스 오버헤드 | Pod마다 Envoy 컨테이너가 추가되어 CPU/메모리 사용량 증가 |
| 올-오어-낫씽 | 사이드카가 없으면 메시 기능을 전혀 사용할 수 없음 |
| Pod 시작 순서 문제 | 사이드카와 애플리케이션 간의 시작 순서 경쟁 (Istio의 [top issue](https://github.com/istio/istio/issues/11130)) |
| 운영 복잡성 | 사이드카 업그레이드 시 Pod 재시작 필요 |
| 프로토콜 호환성 | L7 프록시가 server-first 프로토콜, UDP 등과 충돌 가능 |

### 1.2 Ambient 메시의 설계 목표

Ambient 메시는 사이드카 없이 서비스 메시 기능을 제공하기 위해 설계되었다. 소스코드의 아키텍처 문서(`architecture/ambient/ztunnel.md`)에 명시된 핵심 목표는 다음과 같다:

1. **사용자를 방해하지 않을 것** -- Ztunnel 배포가 기존 Kubernetes 동작을 유지해야 한다. UDP, 비표준 HTTP, server-first 프로토콜, StatefulSet, 외부 서비스 등 모든 기존 기능이 작동해야 한다.
2. **메시 워크로드 간 트래픽을 Istio 아이덴티티로 암호화** -- mTLS를 자동으로 적용한다.
3. **경량화** -- 기존 사이드카보다 훨씬 작은 CPU, 메모리, 지연시간, 처리량 예산으로 동작해야 한다.

### 1.3 사이드카 vs Ambient 아키텍처 비교

```
사이드카 모델:
+------------------------------------------+
| Node                                     |
|  +-------------+    +-------------+      |
|  | Pod A       |    | Pod B       |      |
|  | +---------+ |    | +---------+ |      |
|  | |App      | |    | |App      | |      |
|  | +---------+ |    | +---------+ |      |
|  | |Envoy    |<---->| |Envoy    | |      |
|  | |Sidecar  | |    | |Sidecar  | |      |
|  | +---------+ |    | +---------+ |      |
|  +-------------+    +-------------+      |
+------------------------------------------+

Ambient 모델:
+------------------------------------------+
| Node                                     |
|  +----------+  +----------+              |
|  | Pod A    |  | Pod B    |              |
|  | +------+ |  | +------+ |             |
|  | | App  | |  | | App  | |             |
|  | +------+ |  | +------+ |             |
|  +----+-----+  +----+-----+             |
|       |              |                   |
|  +----+--------------+-----+             |
|  |      Ztunnel (DaemonSet)|             |
|  |      L4 노드 프록시     |             |
|  +----------+---------------+            |
|             |                            |
+------------------------------------------+
              |
     +--------+--------+
     | Waypoint Proxy   |
     | (선택적, L7)     |
     +------------------+
```

### 1.4 2계층 아키텍처

Ambient 메시는 의도적으로 L4와 L7 기능을 분리하는 2계층 아키텍처를 채택한다:

| 계층 | 컴포넌트 | 역할 | 배포 방식 |
|------|----------|------|-----------|
| L4 (기본) | Ztunnel | mTLS, L4 인가, 텔레메트리 | DaemonSet (노드당 1개) |
| L7 (선택) | Waypoint | HTTP 라우팅, L7 인가, 트래픽 관리 | Deployment (네임스페이스/서비스별) |

이 분리의 핵심 이유는: ztunnel은 풍부한 데이터 플레인이 되도록 설계된 것이 아니라, **공격적으로 작은 기능 세트**가 ztunnel을 실현 가능하게 만드는 핵심 특성이다. ztunnel은 트래픽을 안전하게 waypoint에 전달하는 메커니즘이며, 서비스 메시의 풍부한 기능은 waypoint에 위임된다.

---

## 2. Ztunnel 아키텍처

### 2.1 구현 언어 선택: Rust

ztunnel의 초기 구현은 3가지 방식으로 시도되었다:
- Rust 전용 구현
- Go 전용 구현
- Envoy 기반 구현

최종적으로 **Rust 구현**이 선택되었다. 그 이유는:
- 성능 이점이 너무 커서 포기할 수 없었음
- 특정 요구사항에 맞춰 튜닝할 수 있는 기회 제공
- L4 전용이라는 극도로 좁은 목표에 최적화 가능

> 소스 참조: `architecture/ambient/ztunnel.md` -- "the decision was to move forward with a Rust implementation. This offered performance benefits that were too large to leave on the table"

### 2.2 Ztunnel의 핵심 설계 원칙

```
+-------------------------------------------------------+
|                    Ztunnel 설계 원칙                    |
+-------------------------------------------------------+
|                                                       |
|  [의도적으로 하지 않는 것]                               |
|  - L7 (HTTP) 기능 없음                                 |
|  - 범용 프록시 기능 없음                                |
|  - 복잡한 라우팅 로직 없음                              |
|                                                       |
|  [핵심 기능]                                           |
|  - 투명한 L4 프록시                                    |
|  - mTLS 암호화/복호화                                   |
|  - L4 RBAC 정책 적용                                   |
|  - Istio 표준 TCP 메트릭 4종 발행                       |
|  - 인증서 관리 (워크로드 아이덴티티)                      |
|  - 트래픽을 Waypoint으로 전달                           |
|                                                       |
+-------------------------------------------------------+
```

### 2.3 설정 프로토콜: 커스텀 xDS

ztunnel은 xDS **전송 프로토콜**은 사용하지만 xDS **리소스 타입**(Cluster, Listener 등)은 사용하지 않는다. 대신 Istio 고유의 커스텀 리소스 타입을 사용한다.

범용 Envoy xDS 타입 vs 커스텀 타입 비교:

```
Envoy xDS로 Istio mTLS 설정:          커스텀 타입으로 동일 설정:
~50줄의 JSON/Protobuf               단일 필드:
(Cluster, Listener, Route,          ExpectedTLSIdentity:
 Secret, TransportSocket 등)          "spiffe://foo.bar"
```

테스트 결과 커스텀 타입은 가장 관대한 기준으로도 크기, 할당, CPU 시간에서 Envoy 타입 대비 **10배 효율**을 보인다.

ztunnel이 지원하는 xDS 리소스는 단 2종류:
1. **`Address`** -- 워크로드 및 서비스 정보 (`pkg/workloadapi/workload.proto`)
2. **`Authorization`** -- L4 인가 정책 (`pkg/workloadapi/security/authorization.proto`)

### 2.4 인증서 관리

ztunnel은 SPIFFE 형식의 인증서를 관리한다: `spiffe://<trust domain>/ns/<ns>/sa/<sa>`

핵심 특성:
- 인증서의 아이덴티티는 ztunnel 자체가 아니라 **실제 사용자 워크로드의 아이덴티티**
- 노드에서 실행 중인 각 고유 아이덴티티(ServiceAccount)별로 별도의 인증서 보유
- ztunnel은 자체 아이덴티티로 CA에 인증하되, 다른 워크로드의 아이덴티티를 요청
- CA는 ztunnel이 해당 아이덴티티를 요청할 권한이 있는지 반드시 검증 (노드에서 실행 중이지 않은 아이덴티티 요청은 거부)

```
인증서 페칭 흐름:

Ztunnel                          Istio CA
   |                                |
   |-- 자체 SA JWT 토큰으로 인증 --->|
   |-- "Pod X의 아이덴티티 요청" --->|
   |                                |-- Pod X가 이 노드에 있는지 확인
   |<-- spiffe://...ns/X/sa/Y ------|
   |                                |
   |-- "Pod Z의 아이덴티티 요청" --->|
   |                                |-- Pod Z가 이 노드에 없음!
   |<-- 거부 -----------------------|
```

- 새로운 아이덴티티 발견 시 낮은 우선순위로 선제 페칭 (최적화)
- 요청 처리 중 필요한 인증서가 없으면 즉시 요청
- 인증서 만료 전 자동 갱신 (일반적으로 24시간 만료)

### 2.5 텔레메트리

ztunnel은 [Istio 표준 메트릭](https://istio.io/latest/docs/reference/config/metrics/)의 TCP 메트릭 4종을 발행한다:
- `istio_tcp_connections_opened_total`
- `istio_tcp_connections_closed_total`
- `istio_tcp_sent_bytes_total`
- `istio_tcp_received_bytes_total`

---

## 3. HBONE 프로토콜

### 3.1 프로토콜 정의

HBONE(HTTP-Based Overlay Network)은 Istio 메시 내 워크로드 간 통신에 사용하는 프로토콜이다. 새로운 프로토콜이 아니라 표준 기술의 조합에 대한 명명이다.

> 소스 참조: `pkg/hbone/README.md` -- "HTTP Based Overlay Network (HBONE) is the protocol used by Istio for communication between workloads in the mesh."

```
HBONE = HTTP/2 CONNECT 터널 + mTLS(SPIFFE 인증서) + 포트 15008
```

### 3.2 프로토콜 스택

```
+--------------------------------------------------+
| 사용자 TCP 연결 (원본 데이터)                      |
+--------------------------------------------------+
| HTTP/2 CONNECT 터널                               |
|   :method   = CONNECT                             |
|   :authority = <목적지 IP:포트>                     |
|   forwarded  = <원본 소스 IP>                       |
+--------------------------------------------------+
| mTLS (SPIFFE 인증서)                               |
|   Client: spiffe://td/ns/src-ns/sa/src-sa         |
|   Server: spiffe://td/ns/dst-ns/sa/dst-sa         |
+--------------------------------------------------+
| TCP (포트 15008)                                   |
+--------------------------------------------------+
```

### 3.3 SNI와 목적지 결정

SNI(Server Name Indication)는 ztunnel에서 설정하지도, 사용하지도 않는다. 이유는:
- IP 주소를 SNI에 사용하는 것은 표준 위반
- 필요한 정보를 표현할 표준 형식이 없음

대신 **리다이렉션 메커니즘**을 활용한다:

```
요청이 DestinationPod:15008로 전송 → 리다이렉션으로 ztunnel이 수신
→ SO_ORIGINAL_DST로 원래 목적지 IP 추출 → 해당 워크로드의 인증서 사용
```

이 접근 방식은 추가적으로 클라이언트가 목적지의 ztunnel 주소를 알 필요가 없다는 이점을 제공한다.

### 3.4 연결 풀링

사용자 연결은 공유된 HBONE 연결 위에 HTTP/2 표준 멀티플렉싱으로 다중화된다. 풀링 키는 다음 3가지 조합이다:

```
풀링 키 = {소스 아이덴티티, 목적지 아이덴티티, 목적지 IP}
```

### 3.5 HBONE 헤더

| 헤더 | 용도 |
|------|------|
| `:authority` | CONNECT 필수 헤더, 대상 목적지 주소 |
| `Forwarded` | 아웃바운드: 원본 소스 IP. 인바운드: waypoint에서 온 트래픽의 소스 IP (waypoint는 IP 스푸핑 불가) |
| `Baggage` | (실험적) 소스/목적지 워크로드 메타데이터 (텔레메트리 용도) |
| `Traceparent` | (실험적) *연결* 수준의 추적 정보. 사용자 HTTP 요청의 추적과는 별개 |

### 3.6 Go 라이브러리 사용 예시

> 소스 참조: `pkg/hbone/README.md`

클라이언트:
```go
d := hbone.NewDialer(hbone.Config{
    ProxyAddress: "1.2.3.4:15008",
    Headers: map[string][]string{
        "some-additional-metadata": {"test-value"},
    },
    TLS: nil, // 실제 환경에서는 TLS 필수
})
client, _ := d.Dial("tcp", targetAddr)
client.Write([]byte("hello world"))
```

서버:
```go
s := hbone.NewServer()
l, _ := net.Listen("tcp", "0.0.0.0:15008")
s.Serve(l)
```

---

## 4. 트래픽 경로

### 4.1 리다이렉션 요구사항

ztunnel은 투명하게 사용자 트래픽을 암호화하고 라우팅해야 하므로, 메시 Pod에 출입하는 모든 트래픽을 캡처해야 한다. 이는 보안상 매우 중요하다: ztunnel을 우회할 수 있다면 인가 정책도 우회할 수 있기 때문이다.

리다이렉션 규칙:

| 방향 | 포트 | 설명 |
|------|------|------|
| Egress (Pod에서 나가는) | → 15001 | 노드 로컬 ztunnel의 아웃바운드 포트로 리다이렉션. **Service IP 보존 필수** |
| Ingress HBONE (15008) | → 15008 | HBONE으로 들어오는 트래픽은 ztunnel 15008로 리다이렉션 |
| Ingress 기타 | → 15006 | HBONE이 아닌 인바운드 트래픽은 ztunnel 15006으로 리다이렉션 |

### 4.2 아웃바운드 경로 (포트 15001)

Pod에서 나가는 요청은 아웃바운드 코드 경로를 거친다. ztunnel 로직의 대부분이 여기에 집중되어 있다.

```
애플리케이션이 요청 전송
        |
        v
   iptables/eBPF가 트래픽을 ztunnel:15001로 리다이렉션
        |
        v
   SO_ORIGINAL_DST로 원래 목적지 IP/포트 복원
        |
        v
   Address xDS에서 목적지 IP 조회
        |
        +--[알 수 없는 주소 or 비메시 워크로드]----> 원본 소스 IP 스푸핑 후 패스스루
        |                                          (splice 사용하여 효율 향상)
        |
        +--[메시 워크로드, waypoint 있음]----------> waypoint에 HBONE으로 전송
        |                                          Service IP 보존
        |
        +--[메시 워크로드, 같은 노드]--------------> 인바운드로 "패스트 패스" 변환
        |                                          (네트워크 왕복 절약)
        |
        +--[메시 워크로드, 다른 노드]--------------> HBONE으로 대상 ztunnel에 전송
                                                    Service → Pod IP 해석
```

아웃바운드의 핵심 동작:
1. ztunnel은 L4에서 동작하므로 목적지 IP/포트(SO_ORIGINAL_DST)만 사용 가능
2. 목적지가 Service IP일 수도, Pod IP일 수도, 클러스터 외부 IP일 수도 있음
3. **모든 경우에 원본 소스 IP를 스푸핑** -- 투명성 보장

### 4.3 인바운드 패스스루 경로 (포트 15006)

HBONE이 아닌 트래픽(목적지 포트 != 15008)이 Pod로 들어올 때 처리된다.

```
외부 트래픽 → Pod (비-HBONE)
        |
        v
   ztunnel:15006으로 리다이렉션
        |
        v
   RBAC 정책 검사
        |
        +--[STRICT 모드에서 평문 트래픽]---> 거부
        |
        +--[허용됨]------------------------> 목적지로 포워드
        |
        +--[목적지에 waypoint 있음]---------> 헤어핀 처리 (논의 중)
```

### 4.4 인바운드 HBONE 경로 (포트 15008)

HBONE으로 들어오는 트래픽의 처리 과정이다. 요청에는 여러 레이어가 있다: TLS가 HTTP CONNECT를 감싸고, HTTP CONNECT가 사용자 연결을 감싼다.

```
HBONE 요청 수신 (포트 15008)
        |
        v
   1단계: TLS 종료
   - 목적지 IP 기반으로 올바른 인증서 선택
   - 피어가 유효한 메시 아이덴티티를 가지는지 확인
   - (어떤 아이덴티티인지는 아직 검증하지 않음)
        |
        v
   2단계: CONNECT 종료
   - 헤더에서 대상 목적지 확인
        |
        +--[목적지에 waypoint 있음]
        |   → 요청이 해당 waypoint에서 왔는지 검증
        |   → 아니면 거부
        |
        +--[waypoint 없음]
        |   → ztunnel이 직접 RBAC 정책 적용
        |
        v
   3단계: 대상에 연결
   - 소스 IP 스푸핑 (Forwarded 헤더 or 수신 IP)
   - 200 HTTP 응답 반환
   - 양방향 데이터 복사 (터널 ↔ 목적지)
```

### 4.5 전체 트래픽 흐름 다이어그램

```
                    Client Node                          Server Node
              +-------------------+               +-------------------+
              | +-------+         |               |         +-------+ |
              | | App A |         |               |         | App B | |
              | +---+---+         |               |         +---+---+ |
              |     | 평문        |               |       평문 ^     |
              |     v             |               |             |     |
              | +---------+       |    HBONE      |       +---------+ |
              | | Ztunnel |------ | ------------->| ----->| Ztunnel | |
              | |  :15001 |  mTLS |   포트 15008  |  mTLS|  :15008 | |
              | +---------+       |               |       +---------+ |
              +-------------------+               +-------------------+

포트 요약:
  15001 = 아웃바운드 (egress 리다이렉션 대상)
  15006 = 인바운드 패스스루 (비-HBONE ingress)
  15008 = 인바운드 HBONE (mTLS 터널)
```

---

## 5. Waypoint 프록시

### 5.1 Waypoint의 역할

Waypoint은 Ambient 메시의 L7 기능을 담당하는 선택적 프록시이다. ztunnel이 의도적으로 제공하지 않는 기능들을 구현한다:

- HTTP 라우팅 (VirtualService)
- L7 인가 정책 (AuthorizationPolicy의 HTTP 속성)
- 트래픽 관리 (리트라이, 타임아웃, 서킷 브레이킹)
- 헤더 조작
- 고급 로드 밸런싱

### 5.2 Waypoint 구현 구조

> 소스 참조: `pilot/pkg/serviceregistry/kube/controller/ambient/waypoints.go`

Waypoint은 Kubernetes Gateway API를 기반으로 정의된다:

```go
// waypoints.go
type Waypoint struct {
    krt.Named

    // Waypoint에 도달할 수 있는 주소. 일반적으로 호스트네임.
    Address *workloadapi.GatewayAddress

    // 인바운드 ztunnel이 waypoint에 연결할 때 사용하는 기본 바인딩
    DefaultBinding *InboundBinding

    // Service 또는 Workload가 이 waypoint을 참조할 수 있는지 제어
    // "all", "service", "workload" 중 하나
    TrafficType string

    // waypoint 인스턴스의 ServiceAccount 목록
    ServiceAccounts []string
    AllowedRoutes   WaypointSelector
}
```

### 5.3 Waypoint 리소너 코드 구조

> 소스 참조: `pilot/pkg/networking/core/waypoint.go`

waypoint 설정에서 사용하는 핵심 상수들:

```go
// waypoint.go
const (
    ConnectTerminate   = "connect_terminate"       // HTTP CONNECT 종료
    MainInternalName   = "main_internal"           // 메인 내부 리스너
    ConnectOriginate   = "connect_originate"       // HTTP CONNECT 시작
    ForwardInnerConnect = "forward_inner_connect"  // 내부 CONNECT 포워딩
    EncapClusterName   = "encap"                   // connect_originate 리스너용 클러스터
)
```

`findWaypointResources` 함수는 waypoint 프록시에 연결된 워크로드와 서비스를 찾는다:

```go
func findWaypointResources(node *model.Proxy, push *model.PushContext) (
    []model.WorkloadInfo, *waypointServices) {

    var key model.WaypointKey
    if isAmbientEastWestGateway(node) {
        key = model.WaypointKeyForNetworkGatewayProxy(node)
    } else {
        key = model.WaypointKeyForProxy(node)
    }

    workloads := push.WorkloadsForWaypoint(key)
    serviceInfos := push.ServicesForWaypoint(key)
    // ...
}
```

### 5.4 GatewayAddress와 주소 해석

> 소스 참조: `waypoints.go`의 `getGatewayAddress` 함수

Waypoint의 주소는 Gateway 리소스의 Status에서 추출된다. 호스트네임이 IP보다 우선한다:

```go
func getGatewayAddress(gw *gatewayv1.Gateway, netw network.ID) *workloadapi.GatewayAddress {
    // 1순위: 호스트네임 (더 안정적인 조회 키)
    for _, addr := range gw.Status.Addresses {
        if addr.Type != nil && *addr.Type == gatewayv1.HostnameAddressType {
            return &workloadapi.GatewayAddress{
                Destination: &workloadapi.GatewayAddress_Hostname{
                    Hostname: &workloadapi.NamespacedHostname{
                        Namespace: gw.Namespace,
                        Hostname:  addr.Value,
                    },
                },
                HboneMtlsPort: 15008,
            }
        }
    }
    // 2순위: IP 주소 (폴백)
    for _, addr := range gw.Status.Addresses {
        if addr.Type != nil && *addr.Type == gatewayv1.IPAddressType {
            // ...
        }
    }
    return nil
}
```

호스트네임을 우선하는 이유:
- 호스트네임은 이미 서비스의 고유 키
- IP는 재할당될 수 있음
- 목적지가 여러 IP를 가질 수 있어 처리가 복잡해짐

### 5.5 Waypoint 트래픽 유형 제어

> 소스 참조: `waypoints.go`의 `fetchWaypointForService`, `fetchWaypointForWorkload`

Waypoint은 `istio.io/waypoint-for` 레이블로 트래픽 유형을 제어한다:

```
TrafficType = "service"  → 서비스만 이 waypoint 사용 가능
TrafficType = "workload" → 워크로드만 이 waypoint 사용 가능
TrafficType = "all"      → 서비스와 워크로드 모두 가능
```

Waypoint 할당은 레이블 `istio.io/use-waypoint`으로 지정한다. 특수 값 `"none"`은 명시적으로 waypoint 사용을 해제한다. 조회 우선순위:
1. 오브젝트 자체의 `use-waypoint` 레이블
2. 네임스페이스의 `use-waypoint` 레이블
3. 없으면 waypoint 없음

### 5.6 Waypoint을 통한 트래픽 흐름

```
인증된 요청의 흐름 (목적지에 waypoint이 있는 경우):

src Pod --> src ztunnel:15001
                |
                | HBONE (mTLS)
                v
        destination waypoint:15008
        (L7 정책 전체 적용)
                |
                | HBONE (mTLS)
                v
        dst ztunnel:15008
                |
                | 평문 (호스트 네트워크)
                v
           dst Pod
```

인증되지 않은 요청 (PERMISSIVE 모드):

```
src Pod --> ztunnel (L4 정책 적용)
                |
                | TLS (mTLS 아님)
                v
           waypoint
                |
                | mTLS
                v
           ztunnel --> dst Pod
```

> 소스 참조: `architecture/ambient/peer-authentication.md` -- "it is absolutely critical that the waypoint proxy not assume any identity from incoming connections, even if the ztunnel is hairpinning. In other words, all traffic over TLS HBONE tunnels must be considered to be untrusted."

---

## 6. Workload API

### 6.1 Address 리소스 개요

> 소스 참조: `pkg/workloadapi/workload.proto`

`Address`는 ztunnel이 소비하는 기본 설정 리소스이다. IP 주소에 대한 역방향 DNS 조회와 유사하게, "이 IP 주소는 무엇인가?"라는 질문에 답한다.

```protobuf
// Address는 고유한 주소를 나타낸다.
// Workload와 Service 두 하위 리소스를 결합하여 IP 주소로 조회를 지원한다.
message Address {
  oneof type {
    Workload workload = 1;  // 개별 워크로드 (Pod, VM 등)
    Service service = 2;    // 서비스 (워크로드 그룹)
  }
}
```

설계 목표:
1. **온디맨드 조회 지원** -- 대규모 클러스터(100만 엔드포인트)에서 전체 복제가 불가능할 때 필요
2. **클라이언트 비특정** -- 캐싱을 위해 모든 참조가 완전 정규화됨 (IP는 네트워크 ID 포함, 노드 이름은 클러스터 ID 포함)

### 6.2 Workload 메시지

```protobuf
message Workload {
  string uid = 20;                // 전역 고유 불투명 식별자
  string name = 1;                // Pod 이름 (디버깅용)
  string namespace = 2;           // 네임스페이스 (디버깅용)
  repeated bytes addresses = 3;   // IPv4/IPv6 주소
  string hostname = 21;           // DNS 해석용 호스트네임
  string network = 4;             // 네트워크 ID
  TunnelProtocol tunnel_protocol = 5;   // NONE, HBONE, LEGACY_ISTIO_MTLS
  string trust_domain = 6;        // SPIFFE 트러스트 도메인
  string service_account = 7;     // ServiceAccount
  GatewayAddress waypoint = 8;    // 이 워크로드의 waypoint
  string node = 9;                // 실행 중인 노드 이름
  bool native_tunnel = 14;        // 터널 트래픽 직접 수신 여부
  map<string, PortList> services = 22;  // 이 워크로드가 속한 서비스
  repeated string authorization_policies = 16;  // 적용되는 인가 정책
  WorkloadStatus status = 17;     // HEALTHY / UNHEALTHY
  string cluster_id = 18;         // 클러스터 ID
  Locality locality = 24;         // 지역 정보 (region/zone/subzone)
  NetworkMode network_mode = 25;  // STANDARD / HOST_NETWORK
}
```

핵심 설계 결정 -- **인가 정책을 워크로드에 나열**:

Istio의 AuthorizationPolicy는 레이블 셀렉터를 사용한다. 워크로드 크기를 줄이기 위해 레이블을 전송하지 않으므로, 셀렉터 기반 정책을 워크로드와 연결해야 한다. 두 가지 접근법이 있다:

| 접근법 | 장단점 |
|--------|--------|
| 정책에 선택된 워크로드 목록 포함 | 워크로드가 변경될 때마다(빈번) 정책 업데이트 필요 |
| **워크로드에 적용되는 정책 목록 포함** (채택) | 정책 변경(드뭄)에만 정책 업데이트, 워크로드 변경에는 워크로드만 업데이트 |

> 소스 참조: `architecture/ambient/ztunnel.md` -- "each workload will list the policies that select it. This works out to be more efficient in common cases where policies change much less often than workloads."

### 6.3 Service 메시지

```protobuf
message Service {
  string name = 1;                     // 서비스 이름
  string namespace = 2;                // 네임스페이스
  string hostname = 3;                 // FQDN (ex: foo.bar.svc.cluster.local)
  repeated NetworkAddress addresses = 4;  // 서비스 주소 (다중 네트워크, 듀얼 스택)
  repeated Port ports = 5;             // 서비스 포트 (service_port → target_port 매핑)
  GatewayAddress waypoint = 7;         // 이 서비스의 waypoint
  LoadBalancing load_balancing = 8;    // 로드 밸런싱 정책
  IPFamilies ip_families = 9;         // IPv4, IPv6, Dual
}
```

### 6.4 GatewayAddress 메시지

```protobuf
message GatewayAddress {
  oneof destination {
    NamespacedHostname hostname = 1;  // 호스트네임 기반
    NetworkAddress address = 2;       // IP 기반
  }
  uint32 hbone_mtls_port = 3;        // mTLS HBONE 포트 (일반적으로 15008)
}
```

### 6.5 TunnelProtocol

```protobuf
enum TunnelProtocol {
  NONE = 0;              // 터널링 없이 그대로 전달
  HBONE = 1;             // HTTP 기반 터널링
  LEGACY_ISTIO_MTLS = 2; // 레거시 Istio mTLS (ztunnel은 전송 미지원)
}
```

### 6.6 Ambient Index: 컨트롤 플레인 측 구현

> 소스 참조: `pilot/pkg/serviceregistry/kube/controller/ambient/ambientindex.go`

Istiod는 `ambient.Index` 인터페이스를 구현하여 ztunnel에 전달할 xDS 데이터를 관리한다:

```go
type Index interface {
    Lookup(key string) []model.AddressInfo
    All() []model.AddressInfo
    WorkloadsForWaypoint(key model.WaypointKey) []model.WorkloadInfo
    ServicesForWaypoint(key model.WaypointKey) []model.ServiceInfo
    Run(stop <-chan struct{})
    HasSynced() bool
    model.AmbientIndexes
}
```

내부적으로 `index` 구조체는 KRT(Kubernetes Runtime Toolkit) 프레임워크를 사용하여 리액티브 데이터 파이프라인을 구성한다:

```go
type index struct {
    services  servicesCollection    // 서비스 컬렉션 + 인덱스
    workloads workloadsCollection   // 워크로드 컬렉션 + 인덱스
    waypoints waypointsCollection   // waypoint 컬렉션
    networks  NetworkCollections    // 네트워크 정보

    authorizationPolicies krt.Collection[model.WorkloadAuthorization]
    // ...
}
```

워크로드 컬렉션은 4가지 인덱스를 유지한다:

```go
type workloadsCollection struct {
    krt.Collection[model.WorkloadInfo]
    ByAddress                krt.Index[networkAddress, model.WorkloadInfo]
    ByServiceKey             krt.Index[string, model.WorkloadInfo]
    ByOwningWaypointHostname krt.Index[NamespaceHostname, model.WorkloadInfo]
    ByOwningWaypointIP       krt.Index[networkAddress, model.WorkloadInfo]
}
```

### 6.7 Workload 조회 흐름

`Lookup` 함수는 3단계 폴백 전략을 사용한다:

```
Lookup(key) 호출
    |
    +-- 1. 워크로드 UID로 직접 조회
    |
    +-- 2. network/IP 형식으로 워크로드 IP 인덱스 조회
    |
    +-- 3. 서비스 키(namespace/hostname 또는 network/IP)로 서비스 조회
         + 해당 서비스에 속하는 모든 워크로드 반환
```

---

## 7. Authorization API

### 7.1 Authorization 리소스

> 소스 참조: `pkg/workloadapi/security/authorization.proto`

ztunnel이 사용하는 인가 정책의 Protobuf 정의이다. L4 속성만 지원한다.

```protobuf
message Authorization {
  string name = 1;
  string namespace = 2;
  Scope scope = 3;         // GLOBAL, NAMESPACE, WORKLOAD_SELECTOR
  Action action = 4;       // ALLOW, DENY
  repeated Group groups = 5;  // OR 관계
  bool dry_run = 6;        // 드라이 런 (로그만, 적용 안 함)
}
```

### 7.2 정책 평가 로직

```
Groups는 OR 관계:  Group1 || Group2 || ...
Rules는 AND 관계:  Rule1 && Rule2 && ...
Matches는 OR 관계: Match1 || Match2 || ...
Match 내부 필드는 AND 관계

예시:
Authorization(DENY):
  Group1:                    # OR
    Rule1:                   # AND
      Match: port=80         # OR (같은 타입 내)
      Match: port=443
    Rule2:                   # AND
      Match: ns=foo

→ (port==80 OR port==443) AND (ns==foo) → 이 조건이면 DENY
```

### 7.3 Match 메시지 (L4 속성만)

```protobuf
message Match {
  repeated StringMatch namespaces = 1;        // 소스 네임스페이스
  repeated StringMatch not_namespaces = 2;
  repeated StringMatch principals = 3;        // 소스 프린시펄 (SPIFFE ID)
  repeated StringMatch not_principals = 4;
  repeated Address source_ips = 5;            // 소스 IP
  repeated Address not_source_ips = 6;
  repeated Address destination_ips = 7;       // 목적지 IP
  repeated Address not_destination_ips = 8;
  repeated uint32 destination_ports = 9;      // 목적지 포트
  repeated uint32 not_destination_ports = 10;
}
```

### 7.4 Scope 체계

```protobuf
enum Scope {
  GLOBAL = 0;              // 메시 전체 (루트 네임스페이스)
  NAMESPACE = 1;           // 특정 네임스페이스
  WORKLOAD_SELECTOR = 2;   // 레이블 셀렉터로 선택된 워크로드
}
```

### 7.5 AuthorizationPolicy 변환

> 소스 참조: `pilot/pkg/serviceregistry/kube/controller/ambient/authorization.go`의 `convertAuthorizationPolicy`

Istio AuthorizationPolicy가 ztunnel용 `security.Authorization`으로 변환될 때:

1. `targetRef`가 있으면 ztunnel용이 아님 (waypoint용) -- `nil` 반환
2. 셀렉터가 없으면 `NAMESPACE` 또는 `GLOBAL` 스코프
3. **ALLOW/DENY만 지원** -- CUSTOM 액션은 미지원
4. HTTP 속성(hosts, methods, paths, requestPrincipals 등)이 있으면 경고 발생

```go
// ALLOW 정책에서 HTTP 규칙이 발견되면 해당 규칙 자체를 생략 (더 제한적)
// DENY 정책에서 HTTP 규칙이 발견되면 HTTP 부분 무시하고 L4만 적용 (더 제한적)
if action == security.Action_ALLOW && l7RuleFound {
    rules = nil  // L7 정책은 ALLOW에서 절대 매치하지 않음
}
```

L4 `when` 속성만 허용:

```go
var l4WhenAttributes = sets.New(
    "source.ip",
    "source.namespace",
    "source.principal",
    "destination.ip",
    "destination.port",
)
```

### 7.6 정책 컬렉션 구성

> 소스 참조: `pilot/pkg/serviceregistry/kube/controller/ambient/policies.go`

`PolicyCollections` 함수는 4가지 소스에서 정책을 수집한다:

```go
func PolicyCollections(...) (...) {
    // 1. AuthorizationPolicy에서 변환된 정책
    AuthzDerivedPolicies := krt.NewCollection(authzPolicies, ...)

    // 2. PeerAuthentication에서 변환된 정책
    PeerAuthDerivedPolicies := krt.NewCollection(peerAuths, ...)

    // 3. Waypoint 암시적 ALLOW 정책
    ImplicitWaypointPolicies := krt.NewCollection(waypoints, ...)

    // 4. 정적 STRICT 정책 (PeerAuth가 있을 때만)
    DefaultPolicy := krt.NewSingleton[model.WorkloadAuthorization](...)

    // 모두 결합
    Policies := krt.JoinCollection([]krt.Collection[model.WorkloadAuthorization]{
        AuthzDerivedPolicies,
        PeerAuthDerivedPolicies,
        DefaultPolicy.AsCollection(),
        ImplicitWaypointPolicies,
    }, ...)
}
```

정적 STRICT 정책은 PeerAuthentication이 하나라도 존재하면 항상 생성된다:

```go
// 정적 STRICT 정책: 인증되지 않은 모든 트래픽 거부
Authorization{
    Name:   "istio_converted_static_strict",
    Scope:  WORKLOAD_SELECTOR,
    Action: DENY,
    Groups: [{
        Rules: [{
            Matches: [{
                NotPrincipals: [{Presence: {}}]  // 프린시펄이 없으면 거부
            }]
        }]
    }]
}
```

---

## 8. ZDS API

### 8.1 ZDS 프로토콜 개요

> 소스 참조: `pkg/zdsapi/zds.proto`

ZDS(Ztunnel Discovery Service)는 CNI 에이전트와 ztunnel 간의 통신 프로토콜이다. Pod의 네트워크 네임스페이스를 ztunnel에 전달하는 핵심 역할을 한다.

```protobuf
// CNI → ztunnel 방향
message WorkloadRequest {
  oneof payload {
    AddWorkload add = 1;       // 워크로드 추가
    KeepWorkload keep = 5;     // 기존 워크로드 유지 (스냅샷 전)
    DelWorkload del = 2;       // 워크로드 삭제
    SnapshotSent snapshot_sent = 3;  // 스냅샷 완료 신호
  }
}

// ztunnel → CNI 방향
message WorkloadResponse {
  oneof payload {
    Ack ack = 1;               // 확인 응답 (에러 시 error 필드 포함)
  }
}
```

### 8.2 메시지 상세

```protobuf
message AddWorkload {
  string uid = 1;              // 워크로드 UID
  WorkloadInfo workload_info = 2;  // 이름, 네임스페이스, SA 정보
  // + ancillary data: 네트워크 네임스페이스 파일 디스크립터
}

message WorkloadInfo {
  string name = 1;
  string namespace = 2;
  string service_account = 3;
}

message DelWorkload {
  string uid = 2;              // 삭제할 워크로드 UID
}

message SnapshotSent {}        // 현재 캐시의 전체 스냅샷 전송 완료

message Ack {
  string error = 1;            // 비어있으면 성공, 아니면 에러 메시지
}
```

### 8.3 ZDS 프로토콜 시퀀스

```
ztunnel이 CNI에 새로 연결할 때:

   CNI Agent                          Ztunnel
      |                                  |
      |                                  |
      |--- ZdsHello(version=V1) -------->|
      |                                  |
      |--- AddWorkload(pod-A, fd=ns1) -->|
      |<-- Ack(ok) ----------------------|
      |                                  |
      |--- AddWorkload(pod-B, fd=ns2) -->|
      |<-- Ack(ok) ----------------------|
      |                                  |
      |--- KeepWorkload(pod-C) -------->|  (fd 없이 유지 신호)
      |<-- Ack(ok) ----------------------|
      |                                  |
      |--- SnapshotSent() ------------->|
      |      ztunnel은 스냅샷에 없는     |
      |      기존 항목을 삭제             |
      |<-- Ack(ok) ----------------------|
      |                                  |
      |  [이후: 실시간 이벤트]            |
      |                                  |
      |--- AddWorkload(pod-D, fd=ns3) -->|  (새 Pod)
      |<-- Ack(ok) ----------------------|
      |                                  |
      |--- DelWorkload(pod-A) --------->|  (Pod 삭제)
      |<-- Ack(ok) ----------------------|
```

핵심 특성:
- **동기적**: CNI는 각 메시지에 대해 ztunnel의 Ack를 기다린 후 다음 메시지 전송
- **스냅샷 기반 동기화**: 연결 시 전체 상태 전송 후 SnapshotSent로 완료 신호
- **파일 디스크립터 전달**: AddWorkload와 함께 Pod 네트워크 네임스페이스 fd 전달 (ancillary data)
- `KeepWorkload`는 SnapshotSent 전에만 유효 -- fd 캐시에 없지만 유지해야 하는 워크로드용

### 8.4 컴포넌트 관계도

```
+------------------+
| CNI Plugin       |  (바이너리, 컨테이너 런타임이 호출)
| (Pod 생성 시)     |
+--------+---------+
         |
         | HTTP (pluginevent.sock)
         v
+------------------+
| CNI Agent        |  (데몬, Pod 이벤트 감시)
| - 네트워크 규칙   |
|   프로그래밍      |
| - API 서버 감시   |
+--------+---------+
         |
         | ZDS (유닉스 소켓 + fd 전달)
         v
+------------------+
| Ztunnel          |  (DaemonSet)
| - Pod netns에     |
|   리스너 생성     |
| - mTLS/RBAC 처리  |
+------------------+
```

---

## 9. PeerAuthentication 변환

### 9.1 변환 개요

> 소스 참조: `architecture/ambient/peer-authentication.md`

ztunnel은 PeerAuthentication 리소스를 직접 수신하지 않는다. 대신 istiod가 PeerAuthentication을 감지하면 해당 워크로드의 유효 정책을 계산하여 `Authorization` 리소스로 변환해 전송한다.

### 9.2 변환 예시

원본 PeerAuthentication:

```yaml
apiVersion: security.istio.io/v1
kind: PeerAuthentication
metadata:
  name: strict-and-permissive-mtls
spec:
  selector:
    matchLabels:
      app: a
  mtls:
    mode: STRICT
  portLevelMtls:
    9090:
      mode: PERMISSIVE
```

변환된 Authorization:

```yaml
action: DENY
groups:
- rules:
  - matches:
    - notPrincipals:          # 프린시펄이 없으면(평문이면) 거부
      - presence: {}
- rules:
  - matches:
    - notDestinationPorts:    # 9090 포트가 아니면 거부
      - 9090
name: converted_peer_authentication_strict-and-permissive-mtls
scope: WORKLOAD_SELECTOR
```

의미: "인증되지 않은 트래픽을 거부하되, 포트 9090은 예외로 허용"

### 9.3 변환 로직 상세

> 소스 참조: `pilot/pkg/serviceregistry/kube/controller/ambient/authorization.go`의 `convertPeerAuthentication`

변환이 수행되는 조건 (모두 충족해야 함):
1. 워크로드 셀렉터가 있어야 함 (`spec.selector`)
2. 루트 네임스페이스가 아니어야 함
3. 포트 수준 mTLS 설정이 있어야 함 (`spec.portLevelMtls`)

```go
func convertPeerAuthentication(rootNamespace string,
    cfg, nsCfg, rootCfg *securityclient.PeerAuthentication) *security.Authorization {

    // 조건 위반 시 nil 반환 (변환 불필요)
    if cfg.Namespace == rootNamespace ||
       pa.Selector == nil ||
       len(pa.PortLevelMtls) == 0 {
        return nil
    }
    // ...
}
```

### 9.4 정책 상속과 병합

PeerAuthentication은 3단계 상속 구조를 가진다:

```
메시 수준 (루트 네임스페이스) → 네임스페이스 수준 → 워크로드 수준
```

> 소스 참조: `authorization.go`의 `convertedSelectorPeerAuthentications`

유효 정책 결정 로직:

```go
func convertedSelectorPeerAuthentications(rootNamespace string,
    configs []*securityclient.PeerAuthentication) []string {

    var meshCfg, namespaceCfg, workloadCfg *securityclient.PeerAuthentication
    // 각 수준에서 가장 오래된 정책 선택 (충돌 시)

    var isEffectiveStrictPolicy bool

    // 메시 수준
    if meshCfg != nil {
        isEffectiveStrictPolicy = isMtlsModeStrict(meshCfg.Spec.Mtls)
    }

    // 네임스페이스 수준 (UNSET이 아니면 오버라이드)
    if namespaceCfg != nil && !isMtlsModeUnset(namespaceCfg.Spec.Mtls) {
        isEffectiveStrictPolicy = isMtlsModeStrict(namespaceCfg.Spec.Mtls)
    }

    // 워크로드 수준 + 포트 수준 병합...
}
```

### 9.5 병합 시나리오 테이블

| 메시 | 네임스페이스 | 워크로드 | 포트 | 유효 정책 |
|------|------------|---------|------|-----------|
| STRICT | - | - | - | 정적 STRICT 정책 참조 |
| PERMISSIVE | STRICT | - | - | 정적 STRICT 정책 참조 |
| - | - | STRICT | 9090:PERMISSIVE | 변환된 정책 (STRICT + 포트 예외) |
| STRICT | - | UNSET | 9090:PERMISSIVE | 병합된 정책 (정적 STRICT 생략, 변환 정책에 STRICT 포함) |
| PERMISSIVE | - | PERMISSIVE | 8080:STRICT | 포트 수준 STRICT 정책만 변환 |

### 9.6 정적 STRICT 정책과의 관계

유효 정책이 STRICT이고 포트 수준 예외가 없으면, 워크로드는 `istio_converted_static_strict`를 참조한다. 이는 항상 존재하는 단일 정책이다.

포트 수준 예외가 있으면, 정적 정책 대신 **변환된 워크로드 정책**을 사용한다. 이는 DENY 정책의 특성 때문이다: 두 개의 DENY 정책이 있으면 어느 하나라도 매치하면 트래픽이 차단되므로, "STRICT + 예외"를 두 개의 별도 정책으로 표현할 수 없다.

```
[잘못된 접근: 2개 정책]
정책1: DENY if !mTLS (정적 STRICT)
정책2: DENY if !mTLS AND port != 9090

→ 포트 9090의 평문 트래픽은 정책1에 의해 여전히 차단됨!

[올바른 접근: 1개 병합 정책]
정책: DENY if (!mTLS) OR (!mTLS AND port != 9090)
→ Groups를 사용하여 OR 조건으로 표현
→ 포트 9090의 평문 트래픽은 두 번째 Group에 매치되지 않고,
   첫 번째 Group도 제거하여 통과 가능
```

---

## 10. Ztunnel 롤링 리스타트

### 10.1 문제 정의

ztunnel은 L4에서만 동작하므로 리스타트 시 까다로운 상황에 놓인다:

| 제약 | 영향 |
|------|------|
| TCP는 상태 유지 | L3처럼 새 프로세스로 패킷을 넘길 수 없음 |
| L7 아닌 L4 | 애플리케이션에 재연결 신호를 보낼 수 없음 |

따라서 달성 가능한 최선의 목표는:
1. **어떤 시점에서든 새 연결이 성공할 것** -- 새 연결이 드롭되는 기간 없음
2. **기존 연결을 위한 드레인 기간 제공**

### 10.2 SO_REUSEPORT 기반 핸드오프

> 소스 참조: `architecture/ambient/ztunnel-cni-lifecycle.md`

핵심 메커니즘은 `SO_REUSEPORT`이다. ztunnel은 기본적으로 모든 리스너에 이 옵션을 설정한다. 이를 통해 같은 UID의 여러 프로세스가 동일 포트에 바인딩할 수 있다.

### 10.3 롤링 리스타트 시퀀스

```
시간 →

Phase 1: ztunnel-new 시작
+--------------------------------------------------+
| ztunnel-old: 리스닝 + 기존 연결 처리              |
| ztunnel-new: 시작, CNI 연결                       |
+--------------------------------------------------+

Phase 2: ztunnel-new가 준비 완료
+--------------------------------------------------+
| ztunnel-old: 리스닝 + 기존 연결 처리              |
| ztunnel-new: 리스닝 (ready)                       |
| [두 프로세스 모두 리스닝, 새 연결은 둘 중 하나로]   |
+--------------------------------------------------+

Phase 3: ztunnel-old에 SIGTERM
+--------------------------------------------------+
| ztunnel-old: 리스너 종료 + 기존 연결만 처리        |
| ztunnel-new: 리스닝 (새 연결 전담)                 |
| [항상 최소 하나의 ztunnel이 리스닝]                |
+--------------------------------------------------+

Phase 4: 드레인 기간 종료
+--------------------------------------------------+
| ztunnel-old: 강제 종료                             |
| ztunnel-new: 정상 운영                             |
+--------------------------------------------------+
```

상세 단계:

1. `ztunnel-new`가 시작되고, CNI에 연결한다.
2. CNI는 노드의 모든 Pod 상태를 전송한다. ztunnel-new는 각 Pod의 네트워크 네임스페이스에 리스너를 설정하고 "ready" 표시.
3. 이 시점에서 **두 ztunnel 모두 리스닝**. 새 연결은 둘 중 하나에 할당된다 (먼저 accept하는 쪽).
4. Kubernetes가 `ztunnel-old`에 `SIGTERM` 전송. ztunnel-old는 "드레이닝" 시작.
5. 즉시 리스너를 닫아 새 연결 수신을 중단. 이제 `ztunnel-new`만 리스닝. **핵심: 항상 하나 이상의 ztunnel이 리스닝 상태를 유지**.
6. ztunnel-old는 기존 연결을 계속 처리.
7. `drain period` 후 ztunnel-old가 미처리 연결을 강제 종료.

### 10.4 terminationGracePeriodSeconds 고려사항

Kubernetes는 `terminationGracePeriodSeconds` 이후 `SIGQUIT`를 보내 프로세스를 강제 종료한다. 이상적으로는:

```
drain period < terminationGracePeriodSeconds
```

이를 만족해야 ztunnel이 TLS `close_notify`, HTTP/2 `GOAWAY` 등을 정상적으로 전송할 수 있다.

---

## 11. Pod 시작/종료 흐름

### 11.1 Pod 시작 요구사항

> 소스 참조: `architecture/ambient/ztunnel-cni-lifecycle.md`

**핵심 제약**: 네트워크가 애플리케이션이 시작하는 순간(initContainers 포함!) 준비되어야 한다. 이 문제는 사이드카 모델에서 [가장 많이 보고된 이슈](https://github.com/istio/istio/issues/11130)였다.

### 11.2 Pod 시작 구현

```
컨테이너 런타임이 Pod 샌드박스 생성
        |
        v
CNI 플러그인 호출 (동기적, Pod 프로세스 시작 전)
        |
        v
CNI 플러그인 → CNI 에이전트 (HTTP, pluginevent.sock)
        |
        v
CNI 에이전트: Pod/호스트 네트워크 네임스페이스에 규칙 설정
        |
        v
CNI 에이전트 → Ztunnel (ZDS: AddWorkload + 네임스페이스 fd)
        |
        v
Ztunnel: Pod 네임스페이스에 리스너 생성 (15001, 15006, 15008)
        |
        v
Ztunnel: accept() 준비 완료
        |
        v
CNI 플러그인 성공 반환 → 컨테이너 시작 허용
        |
        v
애플리케이션 시작 (네트워크 이미 준비됨)
```

핵심 메커니즘: **CNI 플러그인이 Pod 스케줄링을 차단**한다. 네트워크 설정과 ztunnel 준비가 모두 완료되어야 CNI 플러그인이 성공을 반환한다. 실패 시 CNI 플러그인이 계속 재시도되어 Pod 시작을 차단한다.

### 11.3 Lazy Loading 최적화

ztunnel이 `accept()`를 준비하더라도 요청을 완전히 처리하려면 추가 정보가 필요하다:
- CA에서 인증서
- xDS 서버에서 워크로드 정보

이를 **지연 로딩(Lazy Loading)**으로 처리한다:

```
연결 수락 (즉시)
    |
    v
인증서/워크로드 정보가 있는가?
    |
    +--[있음]--> 즉시 포워딩
    |
    +--[없음]--> 대기 (CA/xDS 서버에서 페칭)
                   |
                   v
               정보 수신 후 포워딩 시작
```

이 최적화의 근거:
- **대부분의 애플리케이션**: 시작 직후 아웃바운드 트래픽을 즉시 전송하지 않음 → 시작 시간에 영향 없음
- **즉시 전송하는 애플리케이션**: 약간의 지연 증가 발생. 그러나 실패한 연결보다 지연된 연결이 훨씬 처리하기 쉬움

### 11.4 실행 중인 Pod의 편입

CNI 플러그인 흐름 외에 **이미 실행 중인 Pod**를 Ambient 모드에 편입하는 대안 경로가 있다:

```
CNI 에이전트가 API 서버에서 Pod 이벤트 감시
        |
        v
Pod가 Ambient 모드로 라벨링됨 감지
        |
        v
네트워크 규칙 설정 (Pod 실행 중에 수행)
        |
        v
ZDS: AddWorkload → Ztunnel
```

차이점: CNI 플러그인 흐름은 Pod 시작 전에 수행되지만, 이 경로는 Pod가 이미 실행 중일 때 수행된다.

### 11.5 Pod 종료 요구사항

**반드시 충족해야 할 조건**:
1. Pod 내 모든 컨테이너가 종료될 때까지 트래픽 처리 유지
2. 종료 중에도 새로운 인바운드 트래픽 수신 (NotReady 전파의 최종 일관성으로 인해)

**해야 하는 조건**:
- HBONE 피어에게 종료 신호 전송 (GOAWAY)

### 11.6 Pod 종료 구현

```
Pod 삭제 or 터미널 페이즈 진입
        |
        v
모든 컨테이너 완전 종료
        |
        v
CNI 에이전트: ZDS DelWorkload 전송
        |
        v
Ztunnel: 해당 Pod의 프록시 종료
```

핵심 포인트:
- `DelWorkload`는 Pod와 모든 컨테이너가 **완전히 종료된 후**에만 전송됨
- 따라서 드레이닝 시간이 필요 없음 -- 즉시 종료 가능
- 그러나 Pod가 이미 삭제되어 `veth`가 해체되므로 GOAWAY 전송 불가

```
최적 시나리오가 아닌 부분:

[현재] Pod 삭제 → veth 해체 → DelWorkload → 종료 (GOAWAY 전송 불가)

[이상적] DelWorkload → GOAWAY 전송 → 기존/신규 연결 유지 → Pod 삭제
                        ^--- 아직 미구현
```

### 11.7 ztunnel의 노드 수준 아키텍처

ztunnel은 단일 공유 바이너리로 노드에서 실행되지만, 각 Pod은 자체 네트워크 네임스페이스 내에 고유한 리스너 세트를 가진다:

```
+-------------------------------------------+
| Node                                      |
|                                           |
| +---------+  +---------+  +---------+     |
| |Pod A    |  |Pod B    |  |Pod C    |     |
| |netns-A  |  |netns-B  |  |netns-C  |     |
| |:15001   |  |:15001   |  |:15001   |     |
| |:15006   |  |:15006   |  |:15006   |     |
| |:15008   |  |:15008   |  |:15008   |     |
| +---------+  +---------+  +---------+     |
|      \           |           /            |
|       \          |          /             |
|        +---------+---------+              |
|        |     Ztunnel       |              |
|        | (단일 프로세스)    |              |
|        | - 인증서 관리      |              |
|        | - xDS 수신        |              |
|        | - RBAC 적용       |              |
|        +-------------------+              |
+-------------------------------------------+
```

---

## 요약

Ambient 메시는 Istio의 아키텍처를 근본적으로 재설계한 결과물이다. 핵심 설계 결정들을 정리하면:

| 설계 결정 | 이유 |
|-----------|------|
| L4/L7 분리 (ztunnel + waypoint) | 경량 보안 계층과 풍부한 기능 계층의 독립적 확장 |
| Rust로 ztunnel 구현 | 노드당 공유 프록시의 성능 예산이 극도로 제한적 |
| 커스텀 xDS 타입 | Envoy xDS 대비 10배 효율 달성 |
| 정책을 워크로드에 나열 | 정책(드물게 변경)이 아닌 워크로드(자주 변경) 업데이트에 최적화 |
| CNI 플러그인으로 동기 설정 | Pod 시작 전 네트워크 준비 보장 (사이드카 모델의 #1 이슈 해결) |
| SO_REUSEPORT 핸드오프 | 연결 끊김 없는 ztunnel 롤링 업데이트 |
| Lazy Loading | Pod 시작 시간 최적화 (대부분의 앱은 즉시 트래픽 전송하지 않음) |
| PeerAuth → Authorization 변환 | ztunnel의 2종 xDS 리소스 제한 유지 |

### 소스 코드 참조

| 파일 | 역할 |
|------|------|
| `architecture/ambient/ztunnel.md` | Ztunnel 아키텍처 설계 문서 |
| `architecture/ambient/ztunnel-cni-lifecycle.md` | Pod/ztunnel 라이프사이클 문서 |
| `architecture/ambient/peer-authentication.md` | PeerAuth 구현 문서 |
| `pkg/workloadapi/workload.proto` | Address, Workload, Service protobuf |
| `pkg/workloadapi/security/authorization.proto` | Authorization protobuf |
| `pkg/zdsapi/zds.proto` | ZDS (CNI↔ztunnel) 프로토콜 |
| `pkg/hbone/README.md` | HBONE 프로토콜 문서 |
| `pilot/pkg/networking/core/waypoint.go` | Waypoint 설정 로직 |
| `pilot/pkg/serviceregistry/kube/controller/ambient/ambientindex.go` | Ambient 인덱스 핵심 |
| `pilot/pkg/serviceregistry/kube/controller/ambient/waypoints.go` | Waypoint 컬렉션 구현 |
| `pilot/pkg/serviceregistry/kube/controller/ambient/authorization.go` | 인가 정책 변환 |
| `pilot/pkg/serviceregistry/kube/controller/ambient/workloads.go` | 워크로드 컬렉션 구현 |
| `pilot/pkg/serviceregistry/kube/controller/ambient/services.go` | 서비스 컬렉션 구현 |
| `pilot/pkg/serviceregistry/kube/controller/ambient/policies.go` | 정책 컬렉션 구성 |
