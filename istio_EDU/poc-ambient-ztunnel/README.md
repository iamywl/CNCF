# PoC: ztunnel L4 프록시와 HBONE 터널링

## 개요

Istio Ambient 메시에서 **ztunnel**(Zero-Trust Tunnel)은 각 노드에 DaemonSet으로 배포되어
L4(TCP) 수준의 투명 프록시 역할을 수행한다. 이 PoC는 ztunnel의 핵심 동작인
**HBONE(HTTP-Based Overlay Network Encapsulation) 터널링**과 **워크로드 주소 인덱스**를
Go 표준 라이브러리만으로 시뮬레이션한다.

### Ambient 메시에서 ztunnel의 역할

기존 사이드카 모드와 달리, Ambient 메시는 Pod마다 Envoy를 주입하지 않는다.
대신 노드 단위의 ztunnel이 L4 트래픽을 투명하게 처리한다:

```
전통적 사이드카 모드:
  App → Envoy(sidecar) → Network → Envoy(sidecar) → App

Ambient 메시:
  App → ztunnel(node) → [HBONE tunnel] → ztunnel(node) → App
              L4 프록시                        L4 프록시
```

## 시뮬레이션 구성 요소

### 1. 워크로드 주소 인덱스 (WorkloadIndex)

ztunnel은 Istiod로부터 xDS를 통해 `Address` 리소스를 수신하고,
IP 주소로 워크로드의 아이덴티티(SPIFFE ID)와 터널 프로토콜을 조회한다.

- **Istio 소스**: `pkg/workloadapi/workload.proto` -- Address, Workload message
- **핵심 필드**: IP → (UID, Namespace, ServiceAccount, TunnelProtocol)
- **SPIFFE ID 형식**: `spiffe://{trust_domain}/ns/{namespace}/sa/{service_account}`

### 2. HBONE 터널 (HTTP CONNECT over TLS)

HBONE은 HTTP CONNECT 메서드를 사용하여 TCP 스트림을 HTTP/2 위에 터널링하는 프로토콜이다.

- **Istio 소스**: `pkg/hbone/dialer.go` -- `hbone()` 함수가 CONNECT 요청 생성
- **Istio 소스**: `pkg/hbone/server.go` -- `handleConnect()` 함수가 CONNECT 수신 처리
- **프로토콜 흐름**:
  1. 소스 ztunnel → 목적지 ztunnel: TLS 핸드셰이크 (mTLS)
  2. `CONNECT {dest_ip}:{port} HTTP/1.1` 요청 전송
  3. 목적지 ztunnel: `200 OK` 응답
  4. 양방향 TCP 스트림 복사 시작

### 3. 아웃바운드 프록시 (포트 15001)

iptables/nftables 규칙으로 앱의 아웃바운드 트래픽을 투명하게 가로챈다.

- 목적지 IP를 워크로드 인덱스에서 조회
- `TunnelProtocol == HBONE`이면 HBONE 터널을 통해 전달
- `TunnelProtocol == NONE`이면 직접 TCP 전달

### 4. 인바운드 프록시 (포트 15008)

원격 ztunnel로부터 HBONE 연결을 수신하고, 로컬 Pod으로 트래픽을 전달한다.

- mTLS로 소스 아이덴티티 인증
- HTTP CONNECT의 Host 헤더에서 목적지 Pod 주소 추출
- 로컬 TCP 연결로 트래픽 전달

### 5. mTLS 인증

자체 서명 CA를 사용하여 SPIFFE ID 기반 mTLS를 시뮬레이션한다.

- 실제 Istio: ztunnel이 SDS(Secret Discovery Service)로 Istiod에서 인증서 수신
- 인증서의 CommonName에 SPIFFE ID 포함

## 실행 방법

```bash
go run main.go
```

외부 의존성 없이 Go 표준 라이브러리만 사용한다.

## 예상 출력

```
============================================================
  Istio Ambient ztunnel L4 프록시 & HBONE 터널링 시뮬레이션
============================================================

[1단계] 메시 CA 생성 (mTLS 인프라)
  ...CA 생성 완료

[2단계] 워크로드 주소 인덱스 구성
  [인덱스 이벤트] ADD: client-pod → IPs=[10.0.1.10], tunnel=HBONE
  [인덱스 이벤트] ADD: server-pod → IPs=[10.0.2.20], tunnel=HBONE
  [인덱스 이벤트] ADD: external-svc → IPs=[10.0.3.30], tunnel=NONE

  [IP 조회 테스트]
    default/10.0.1.10 → client-pod (tunnel=HBONE)
    default/10.0.2.20 → server-pod (tunnel=HBONE)
    default/10.0.3.30 → external-svc (tunnel=NONE)

[3단계] 워크로드별 mTLS 인증서 생성
  소스 ztunnel 인증서: identity=spiffe://cluster.local/ns/default/sa/client-sa
  목적지 ztunnel 인증서: identity=spiffe://cluster.local/ns/default/sa/server-sa

[4단계] ztunnel 노드 및 애플리케이션 서버 시작
  [목적지 앱] server-pod-app 시작
  [목적지 ztunnel] HBONE 인바운드 서버 시작 (실제: 포트 15008)
  [소스 ztunnel] 아웃바운드 프록시 시작 (실제: 포트 15001)

[5단계] 트래픽 흐름 시뮬레이션
  --- 시나리오 1: HBONE 터널을 통한 메시 내 트래픽 ---
  [아웃바운드] 트래픽 가로챔 → 워크로드 발견 → HBONE 터널 생성
  [HBONE서버] CONNECT 수신 → 터널 성립
  [결과] HBONE 터널링 + mTLS 성공!

  --- 시나리오 2: 직접 TCP 전달 (TunnelProtocol=NONE) ---
  (비-메시 워크로드는 HBONE 터널 미사용)

[6단계~8단계] 아키텍처 다이어그램, 워크로드 인덱스, HBONE 프로토콜 상세 출력
```

## 트래픽 흐름 다이어그램

```
  ┌─────────────────────────────────────────────────────────┐
  │                    Node A (소스)                         │
  │  ┌──────────┐    iptables     ┌───────────────────┐     │
  │  │ App Pod  │ ──────────────→ │ ztunnel           │     │
  │  │(client)  │    (투명 캡처)   │  ├─ 아웃바운드:15001│    │
  │  └──────────┘                 │  └─ 인바운드:15008 │    │
  │                               └────────┬──────────┘     │
  └────────────────────────────────────────┼────────────────┘
                                           │ HBONE (mTLS)
  ┌────────────────────────────────────────┼────────────────┐
  │                    Node B (목적지)       │                │
  │                               ┌────────┴──────────┐     │
  │  ┌──────────┐                 │ ztunnel           │     │
  │  │ App Pod  │ ←────────────── │  ├─ 인바운드:15008 │    │
  │  │(server)  │    (로컬 TCP)   │  └─ 아웃바운드:15001│    │
  │  └──────────┘                 └───────────────────┘     │
  └─────────────────────────────────────────────────────────┘
```

## Istio 소스코드 참조

| 구성 요소 | 소스 파일 | 핵심 내용 |
|-----------|----------|----------|
| 워크로드 모델 | `pkg/workloadapi/workload.proto` | Address, Workload, Service, TunnelProtocol 정의 |
| HBONE 다이얼러 | `pkg/hbone/dialer.go` | `hbone()` -- HTTP CONNECT 요청 생성 및 양방향 복사 |
| HBONE 서버 | `pkg/hbone/server.go` | `handleConnect()` -- CONNECT 수신, 목적지 연결, 양방향 복사 |
| 네트워크 게이트웨이 | `pilot/pkg/model/network.go` | `NetworkGateway` -- HBONEPort 필드 |
| 투명 캡처 | `tools/istio-nftables/pkg/capture/run.go` | nftables 규칙으로 트래픽 리디렉트 |

## 핵심 학습 포인트

1. **HBONE = HTTP CONNECT + TLS**: TCP 스트림을 HTTP/2 위에 터널링하여 기존 인프라 호환성 확보
2. **워크로드 인덱스**: IP 주소 → SPIFFE 아이덴티티 매핑이 ztunnel의 핵심 데이터 구조
3. **TunnelProtocol 결정**: 워크로드의 메시 참여 여부에 따라 HBONE/NONE 결정
4. **양방향 복사 패턴**: `io.Copy` 2개를 goroutine으로 실행하여 full-duplex TCP 터널 구현
5. **L4 vs L7 분리**: ztunnel은 L4만 담당, L7 정책은 waypoint proxy가 처리
