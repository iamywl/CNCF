# PoC-11: 서비스 메시 시뮬레이션 (L7 프록시 + xDS)

## 개요

Cilium 서비스 메시의 핵심 메커니즘인 **xDS 프로토콜 기반 동적 설정 관리**와 **BPF 기반 L7 프록시 리다이렉트**를 시뮬레이션한다. Envoy 프록시와 Cilium 에이전트 간의 리소스 디스커버리 흐름(LDS/RDS/CDS/EDS), ACK/NACK 프로토콜, 캐시 기반 원자적 업데이트, 그리고 tc BPF에서 Envoy로 패킷을 전달하는 L7 리다이렉트 패턴을 Go 표준 라이브러리만으로 재현한다.

## 배경 지식

### 서비스 메시란 무엇인가

서비스 메시는 마이크로서비스 간 통신을 인프라 계층에서 관리하는 아키텍처 패턴이다. 트래픽 라우팅, 로드밸런싱, 인증/인가, 관측성(observability) 등을 애플리케이션 코드 변경 없이 제공한다. 전통적으로 Istio/Linkerd 등은 각 Pod에 사이드카 프록시(Envoy)를 주입하여 모든 트래픽을 가로챈다.

### Cilium의 사이드카리스(Sidecarless) 접근 방식

Cilium은 eBPF를 활용하여 **사이드카 없이** 서비스 메시 기능을 제공한다. 핵심 차이점은 다음과 같다:

- **L3/L4 처리**: eBPF 프로그램이 커널 레벨에서 직접 처리하므로 프록시가 불필요하다
- **L7 처리**: L7 정책이 필요한 트래픽만 선택적으로 Envoy 프록시로 리다이렉트한다
- **노드당 하나의 Envoy**: Pod마다 사이드카를 배치하는 대신 노드당 공유 Envoy 인스턴스를 사용한다
- 이 방식은 메모리 사용량과 레이턴시를 크게 줄인다

### Envoy 프록시와의 통합 - xDS 프로토콜

Cilium 에이전트는 xDS(eXtensible Discovery Service) gRPC 서버를 내장하고 있으며, Envoy에 동적 설정을 전달한다. xDS는 네 가지 핵심 리소스 타입으로 구성된다:

| 프로토콜 | 리소스 타입 | 역할 |
|---------|-----------|------|
| **LDS** (Listener Discovery) | Listener | 수신 포트, 필터 체인 정의 |
| **RDS** (Route Discovery) | RouteConfiguration | HTTP 라우팅 규칙 (경로 → 클러스터 매핑) |
| **CDS** (Cluster Discovery) | Cluster | 업스트림 서비스 그룹, LB 정책 |
| **EDS** (Endpoint Discovery) | ClusterLoadAssignment | 클러스터의 실제 백엔드 IP:Port 목록 |

Cilium이 NetworkPolicy나 Service 변경을 감지하면 xDS Cache를 업데이트하고, Envoy가 gRPC 스트림을 통해 변경된 설정을 수신한다. Envoy는 설정 적용 성공 시 ACK, 실패 시 NACK를 보낸다.

### L7 프록시 리다이렉트의 원리

BPF tc(traffic control) 프로그램이 패킷을 Envoy로 전달하는 흐름은 다음과 같다:

1. **tc ingress BPF**: 수신 패킷의 identity와 포트를 검사하여 L7 정책이 적용되면 TPROXY로 Envoy 프록시 포트로 리다이렉트
2. **Envoy L7 프록시**: HTTP 파싱, 라우트 매칭, NetworkPolicy 기반 허용/거부 판정
3. **tc egress BPF**: Envoy에서 나온 패킷을 원래 목적지(백엔드)로 DNAT하여 전달

이 매핑 정보는 BPF 맵에 `(identity, port) → proxyPort` 형태로 저장되어 O(1) 조회가 가능하다.

## 시뮬레이션하는 개념

| 실제 Cilium 컴포넌트 | 소스 경로 | PoC 시뮬레이션 |
|---------------------|----------|---------------|
| xDS 리소스 캐시 | `pkg/envoy/xds/cache.go` | `Cache` 구조체 - 리소스 저장, version 관리, TX() 원자적 업데이트 |
| xDS gRPC 서버 | `pkg/envoy/xds/server.go` | `XDSServer` - DiscoveryRequest/Response, ACK/NACK 처리 |
| 리소스 타입 정의 | `pkg/envoy/resources.go` | LDS/RDS/CDS/EDS TypeURL 상수 매핑 |
| 고수준 xDS 인터페이스 | `pkg/envoy/xds_server.go` | `CiliumXDSServer` - AddListener, AddRoute, UpdateEndpoint |
| L7 프록시 리다이렉트 | `pkg/proxy/`, BPF tc | `ProxyEngine` - tc→Envoy→tc 패킷 흐름 및 HTTP 라우트 매칭 |
| 리소스 변경 감시 | `pkg/envoy/xds/watcher.go` | `ResourceObserver` 인터페이스 - 캐시 변경 시 옵저버 통지 |

## 아키텍처 다이어그램

### xDS 리소스 디스커버리 흐름

```
  Envoy                              Cilium xDS Server
    |                                      |
    |--- DiscoveryRequest(v=0) ----------→ |
    |                                      | Cache에서 리소스 조회
    |←-- DiscoveryResponse(v=1,nonce=1) -- |
    |                                      |
    |--- ACK(v=1, nonce=1) -------------→  | 적용 성공 확인
    |                                      |
    |--- NACK(v=0, nonce=1, error) -----→  | 적용 실패, 이전 설정 유지
    |                                      |
    |   (Cache 업데이트 → version bump)     |
    |                                      |
    |←-- DiscoveryResponse(v=2,nonce=2) -- | 변경된 리소스 push
```

### L7 프록시 리다이렉트 (tc→Envoy→tc 패턴)

```
  Client Pod                Node                          Backend Pod
      |                                                       |
      |── packet ──→ [tc ingress BPF] ── TPROXY ──→ [Envoy L7 Proxy]
      |               identity+port 검사              HTTP 파싱+라우트 매칭
      |                                               NetworkPolicy 검사
      |                                                    |
      |              [tc egress BPF] ←── 허용된 트래픽 ──┘
      |               DNAT → 백엔드 주소                      |
      |                       └──── packet ──────────────→    |
```

## 코드 해설

### Cache (xDS 리소스 캐시)

`pkg/envoy/xds/cache.go`의 핵심을 재현한 구조체이다. 리소스를 `(typeURL, name)` 키로 저장하고 `TX()` 메서드로 원자적 업데이트(upsert + delete)를 수행한다. 변경이 발생하면 version을 bump하고 등록된 옵저버에게 통지한다. `sync.RWMutex`로 동시성을 보장한다.

### XDSServer (xDS 스트림 핸들러)

`pkg/envoy/xds/server.go`의 `HandleRequestStream()`을 재현한다. Envoy의 `DiscoveryRequest`를 받아 ACK/NACK를 판별한다. `ResponseNonce`가 유효하고 `ErrorDetail`이 비어있으면 ACK로 처리하여 해당 리소스 타입의 acked version을 갱신한다. 요청된 version이 캐시의 현재 version보다 낮으면 새 리소스를 담은 `DiscoveryResponse`를 반환한다.

### ProxyEngine (L7 프록시)

`pkg/proxy/` 패키지의 L7 리다이렉트 로직을 시뮬레이션한다. `ProcessPacket()`은 수신 패킷의 Host와 Path를 기반으로 xDS Cache의 Route 리소스와 매칭하여 대상 클러스터를 결정한다. 실제 Cilium에서는 BPF tc 프로그램이 TPROXY로 패킷을 전달하고, Envoy가 이 HTTP 라우팅을 수행한다.

### CiliumXDSServer (고수준 인터페이스)

`pkg/envoy/xds_server.go`의 `XDSServer` 인터페이스를 재현한다. `AddListener()`는 Envoy 리스너를 생성하여 xDS Cache에 등록하고 동시에 프록시 리다이렉트 포트를 할당한다. `UpdateEndpoint()`는 엔드포인트를 변경하여 핫 리로드(zero-downtime 업데이트)를 시뮬레이션한다.

### Resource + DiscoveryRequest/Response (xDS 프로토콜 메시지)

Envoy의 protobuf 메시지(`DiscoveryRequest`, `DiscoveryResponse`)를 Go 구조체로 단순화하여 재현한다. `Resource`는 `TypeURL`, `Name`, `Value`, `Version` 필드를 가지며, 실제로는 `proto.Message` 타입인 `Value`를 `interface{}`로 대체했다.

## 실행 방법 및 예상 출력

```bash
cd cilium_EDU/poc-11-service-mesh
go run main.go
```

### 예상 출력 (요약)

```
╔══════════════════════════════════════════════════════════════╗
║  Cilium 서비스 메시 시뮬레이션 (L7 프록시 + xDS)            ║
╚══════════════════════════════════════════════════════════════╝

━━━ 데모 1: xDS 리소스 디스커버리 프로토콜 (LDS/RDS/CDS/EDS) ━━━
  LDS ← type.googleapis.com/envoy.config.listener.v3.Listener
  RDS ← type.googleapis.com/envoy.config.route.v3.RouteConfiguration
  ...

━━━ 데모 2: 리스너, 라우트, 클러스터, 엔드포인트 설정 ━━━
  [LDS] 리스너 추가: ingress-http:80 (proxyPort=10000)
  [RDS] 라우트 추가: /api/ → api-cluster, /static/ → static-cluster

━━━ 데모 3: xDS 스트리밍 — 요청/응답/ACK 흐름 ━━━
  [Cilium] LDS 응답: version=6, nonce=1, resources=1
  [Cilium] 이미 최신 — 대기 (long-poll)

━━━ 데모 4: L7 프록시 리다이렉트 (tc→proxy→tc 패턴) ━━━
  GET /api/users     → cluster=api-cluster [ALLOW]
  GET /static/style  → cluster=static-cluster [ALLOW]

━━━ 데모 5: 핫 리로드 — 엔드포인트 동적 업데이트 ━━━
  캐시 버전: 6 → 7 (자동 bump), 새 포드 추가 + unhealthy 감지

━━━ 데모 7: L7 라우팅 부하 시뮬레이션 ━━━
  1000개 요청 라우팅 분포: /api/ ~500건, /static/ ~170건, / ~330건
```

## 핵심 포인트

1. **Cache.TX()의 원자적 업데이트**: upsert와 delete를 하나의 트랜잭션으로 처리하여 Envoy가 중간 상태를 보는 것을 방지한다. 변경이 실제로 발생한 경우에만 version을 bump하여 불필요한 push를 방지한다.

2. **Nonce 기반 ACK/NACK**: 서버가 여러 응답을 연속으로 보낼 수 있으므로, nonce를 사용하여 어떤 응답에 대한 ACK/NACK인지 식별한다. NACK 시 Envoy는 이전 설정을 유지하며 서버는 에러를 기록한다.

3. **선택적 L7 리다이렉트**: 모든 트래픽이 프록시를 거치는 것이 아니라, L7 정책이 필요한 트래픽만 BPF가 판별하여 Envoy로 보낸다. L3/L4만으로 처리 가능한 트래픽은 커널에서 직접 처리한다.

4. **핫 리로드 메커니즘**: 엔드포인트 변경(스케일링, 헬스체크 실패 등)은 EDS 업데이트만으로 처리되며, 리스너나 라우트를 재생성할 필요가 없다. 이는 xDS의 계층적 리소스 모델 덕분이다.

5. **옵저버 패턴의 활용**: Cache가 변경되면 등록된 옵저버에게 즉시 통지하여 long-poll 중인 xDS 스트림이 새 응답을 보낼 수 있게 한다.

## 실제 Cilium과의 차이점

| 항목 | 실제 Cilium | PoC 시뮬레이션 |
|------|-----------|---------------|
| xDS 전송 | gRPC 양방향 스트리밍 (`StreamListeners` 등) | 함수 호출로 요청/응답 처리 |
| 리소스 직렬화 | Protocol Buffers (`proto.Message`) | Go `interface{}` 타입 |
| L7 프록시 | 실제 Envoy 프로세스 (embedded 또는 standalone) | `ProxyEngine` 구조체의 인메모리 라우트 매칭 |
| BPF 리다이렉트 | tc ingress/egress BPF 프로그램 + TPROXY | 프록시 포트 매핑만 시뮬레이션 |
| 리소스 동일성 비교 | `proto.Equal()`로 기존/신규 리소스 비교 후 변경 시만 업데이트 | 무조건 업데이트 (비교 로직 생략) |
| 네트워크 정책 | `NetworkPolicy` CRD → BPF 맵 + Envoy 정책 동시 적용 | 라우트 매칭만 수행, 정책 거부 미구현 |
| mTLS | Cilium이 SPIFFE 기반 인증서를 Envoy에 주입 | 미구현 |
| 헬스체크 | Envoy의 능동적 헬스체크 + EDS 헬스 상태 | `Healthy` 플래그 표시만 |
| 동시성 | 다수의 Envoy 인스턴스가 동시에 xDS 스트림 유지 | 단일 스트림만 시뮬레이션 |
