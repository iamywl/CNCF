# 18. gRPC-Go xDS (서비스 메시) 심화

## 개요

xDS(x Discovery Service)는 **Envoy 프록시**에서 시작된 서비스 발견 프로토콜로,
gRPC가 이를 네이티브 지원하여 **프록시 없는 서비스 메시(proxyless service mesh)**를
구현할 수 있다. gRPC 클라이언트가 xDS 서버(예: Istiod)에서 직접 서비스 설정을 받아
라우팅, 로드 밸런싱, 보안 정책을 적용한다.

**소스코드**:
- `xds/` — xDS 지원 (별도 Go 모듈: `google.golang.org/grpc/xds`)
- `xds/xds.go` — xDS 초기화 진입점
- `xds/bootstrap/` — 부트스트랩 설정
- `xds/server.go` — xDS 지원 gRPC 서버
- `xds/csds/` — Client Status Discovery Service

---

## 1. xDS 프로토콜 개요

### 왜 xDS인가?

```
전통적 서비스 발견:
  Client → DNS → IP 목록 → 직접 연결

프록시 기반 메시 (Envoy):
  Client → Envoy Sidecar → Backend
    └── Envoy가 xDS 서버에서 설정 받음

프록시리스 메시 (gRPC xDS):
  Client → (xDS 서버에서 설정 받음) → Backend 직접 연결
    └── 프록시 없이 gRPC가 직접 xDS 프로토콜 구현
```

**프록시리스의 장점:**
- 네트워크 홉 감소 (프록시 거치지 않음) → 지연 감소
- 리소스 절약 (사이드카 프록시 불필요)
- gRPC 네이티브 통합 (HTTP/2 인식 LB)

---

## 2. xDS 리소스 타입

xDS 프로토콜은 여러 종류의 "리소스"를 정의하며, 각각 다른 설정을 제공한다.

### 리소스 계층 구조

```
┌──────────────────────────────────────────────────────┐
│                    LDS (Listener)                      │
│  "어떤 주소에서 리슨할 것인가?"                         │
│  → 리스너 설정, 필터 체인                               │
└──────────────────────┬───────────────────────────────┘
                       │
┌──────────────────────▼───────────────────────────────┐
│                    RDS (Route)                         │
│  "어떤 클러스터로 라우팅할 것인가?"                     │
│  → 호스트/경로 기반 라우팅 규칙                         │
└──────────────────────┬───────────────────────────────┘
                       │
┌──────────────────────▼───────────────────────────────┐
│                    CDS (Cluster)                       │
│  "클러스터의 LB 정책/설정은 무엇인가?"                  │
│  → 로드 밸런싱 정책, 서킷 브레이커                      │
└──────────────────────┬───────────────────────────────┘
                       │
┌──────────────────────▼───────────────────────────────┐
│                    EDS (Endpoint)                      │
│  "어떤 엔드포인트(IP:포트)가 있는가?"                   │
│  → 엔드포인트 목록, 가중치, 로컬리티                    │
└──────────────────────────────────────────────────────┘
```

### 리소스 타입 상세

| 약어 | 전체 이름 | 역할 | 클라이언트/서버 |
|------|----------|------|----------------|
| **LDS** | Listener Discovery Service | 리스너 설정, 필터 체인 | 양쪽 |
| **RDS** | Route Discovery Service | 라우팅 규칙 | 클라이언트 |
| **CDS** | Cluster Discovery Service | LB 정책, 커넥션 설정 | 클라이언트 |
| **EDS** | Endpoint Discovery Service | 엔드포인트 목록, 가중치 | 클라이언트 |
| **SDS** | Secret Discovery Service | TLS 인증서/키 | 양쪽 |

---

## 3. gRPC xDS 아키텍처

```
┌──────────────────────────────────────────────────────────┐
│                     xDS 서버 (Istiod)                     │
│                                                          │
│  ┌──────┐  ┌──────┐  ┌──────┐  ┌──────┐  ┌──────┐     │
│  │ LDS  │  │ RDS  │  │ CDS  │  │ EDS  │  │ SDS  │     │
│  └──┬───┘  └──┬───┘  └──┬───┘  └──┬───┘  └──┬───┘     │
│     └──────────┴────┬────┴─────────┴──────────┘         │
│                     │  ADS (Aggregated DS)               │
└─────────────────────┼────────────────────────────────────┘
                      │
              gRPC 양방향 스트리밍
              (DiscoveryRequest/
               DiscoveryResponse)
                      │
┌─────────────────────┼────────────────────────────────────┐
│                     │   gRPC 클라이언트                    │
│              ┌──────▼──────┐                              │
│              │  xDS Client │  (xds.NewClient)             │
│              │  (ADS 스트림)│                              │
│              └──────┬──────┘                              │
│                     │                                     │
│          ┌──────────┼──────────┐                          │
│          ▼          ▼          ▼                          │
│    ┌──────────┐ ┌────────┐ ┌──────────┐                 │
│    │xDS       │ │xDS     │ │xDS       │                 │
│    │Resolver  │ │Balancer│ │Credentials│                │
│    │(CDS/EDS) │ │(CDS LB)│ │(SDS TLS) │                │
│    └─────┬────┘ └───┬────┘ └──────────┘                 │
│          │          │                                     │
│    ┌─────▼──────────▼────┐                               │
│    │    ClientConn        │                               │
│    │  (표준 gRPC 채널)    │                               │
│    └─────────────────────┘                               │
└──────────────────────────────────────────────────────────┘
```

---

## 4. 부트스트랩 설정 (`xds/bootstrap/`)

xDS 클라이언트가 시작할 때 **부트스트랩 설정**을 읽어 xDS 서버 위치와
인증 정보를 알아낸다.

### 부트스트랩 JSON 형식

```json
{
  "xds_servers": [
    {
      "server_uri": "xds-server.example.com:443",
      "channel_creds": [
        {"type": "google_default"}
      ],
      "server_features": ["xds_v3"]
    }
  ],
  "node": {
    "id": "sidecar~10.0.0.1~my-pod.my-ns~my-ns.svc.cluster.local",
    "metadata": {
      "INSTANCE_IPS": "10.0.0.1"
    },
    "locality": {
      "region": "us-central1",
      "zone": "us-central1-a"
    }
  },
  "certificate_providers": {
    "default": {
      "plugin_name": "file_watcher",
      "config": {
        "certificate_file": "/certs/tls.crt",
        "private_key_file": "/certs/tls.key",
        "ca_certificate_file": "/certs/ca.crt",
        "refresh_interval": "600s"
      }
    }
  },
  "server_listener_resource_name_template": "grpc/server?xds.resource.listening_address=%s"
}
```

### 부트스트랩 설정 방법

```bash
# 방법 1: 파일 경로
export GRPC_XDS_BOOTSTRAP=/etc/xds/bootstrap.json

# 방법 2: JSON 직접 지정
export GRPC_XDS_BOOTSTRAP_CONFIG='{"xds_servers":[...]}'
```

### 주요 필드

| 필드 | 필수 | 설명 |
|------|------|------|
| `xds_servers` | 예 | xDS 서버 목록 |
| `node` | 예 | 이 gRPC 인스턴스의 식별자 |
| `certificate_providers` | 아니오 | TLS 인증서 제공자 |
| `server_listener_resource_name_template` | 아니오 | 서버 측 LDS 리소스 이름 템플릿 |
| `authorities` | 아니오 | 멀티 xDS 서버 지원 |

---

## 5. xDS 리졸버

gRPC xDS는 표준 리졸버 프레임워크를 활용한다. `xds:///service-name` 스킴을
사용하면 xDS 리졸버가 활성화된다.

### 사용법

```go
import _ "google.golang.org/grpc/xds"  // xDS 리졸버 등록

conn, err := grpc.NewClient("xds:///my-service.my-ns.svc.cluster.local",
    grpc.WithTransportCredentials(insecure.NewCredentials()),
)
```

### xDS 리졸버 동작

```
1. "xds:///my-service" 파싱
2. 부트스트랩에서 xDS 서버 정보 로드
3. xDS 서버에 ADS 스트림 연결
4. LDS 리소스 요청 (리스너 설정)
5. RDS 리소스 요청 (라우팅 규칙)
6. CDS 리소스 요청 (클러스터 설정)
7. EDS 리소스 요청 (엔드포인트 목록)
8. 결과를 resolver.State로 변환
9. 밸런서에 전달 → SubConn 생성 → 연결
```

---

## 6. xDS 밸런서

xDS에서는 클러스터 설정에 따라 적절한 밸런서가 자동 선택된다.

### 밸런서 계층 구조

```
xDS Cluster Manager (최상위)
├── Cluster 1: "cluster-a"
│   └── weighted_target
│       ├── Locality 1 (us-central1-a): weight=80
│       │   └── round_robin
│       │       ├── endpoint 10.0.0.1:8080
│       │       └── endpoint 10.0.0.2:8080
│       └── Locality 2 (us-central1-b): weight=20
│           └── round_robin
│               └── endpoint 10.0.1.1:8080
│
└── Cluster 2: "cluster-b"
    └── ring_hash
        ├── endpoint 10.1.0.1:8080
        └── endpoint 10.1.0.2:8080
```

### xDS에서 사용되는 밸런서

| 밸런서 | xDS 매핑 | 용도 |
|--------|----------|------|
| `cluster_resolver` | CDS/EDS | 클러스터 엔드포인트 해석 |
| `cluster_impl` | CDS | 클러스터별 설정 적용 |
| `weighted_target` | EDS locality | 로컬리티 가중치 분배 |
| `priority` | EDS priority | 우선순위 기반 페일오버 |
| `round_robin` | 기본 | 엔드포인트 간 순환 |
| `ring_hash` | CDS 설정 | 일관된 해싱 (스티키 세션) |
| `least_request` | CDS 설정 | 최소 요청 |

---

## 7. xDS 서버 (`xds/server.go`)

gRPC-Go는 **서버 측 xDS**도 지원한다. 서버가 xDS 서버에서
리스너 설정과 TLS 인증서를 동적으로 받을 수 있다.

### 사용법

```go
import "google.golang.org/grpc/xds"

// xDS 지원 서버 생성
s, err := xds.NewGRPCServer()
if err != nil {
    log.Fatal(err)
}

// 서비스 등록 (일반 gRPC와 동일)
pb.RegisterMyServiceServer(s, &myService{})

// 서빙 (xDS에서 리스너 설정 수신)
lis, _ := net.Listen("tcp", ":8080")
s.Serve(lis)
```

### 서버 xDS 동작

```
1. 부트스트랩 로드
2. xDS 서버에 LDS 요청
   → server_listener_resource_name_template에서 리소스 이름 생성
3. LDS 응답: 필터 체인, TLS 설정
4. SDS로 인증서 로드 (필요시)
5. 서빙 모드 결정:
   - xDS 설정 수신 전: NOT_SERVING
   - 설정 수신 후: SERVING
6. 클라이언트 연결 시 xDS 설정 적용
   - mTLS 적용
   - RBAC 정책 적용
```

---

## 8. ADS (Aggregated Discovery Service)

### 왜 ADS인가?

개별 xDS 서비스(LDS, RDS, CDS, EDS)를 별도 스트림으로 요청하면
리소스 간 순서 보장이 안 된다. 예를 들어 새 CDS 설정이 먼저 도착하고
해당 EDS가 나중에 오면, 잠시 트래픽이 끊길 수 있다.

ADS는 **단일 gRPC 양방향 스트림**에서 모든 리소스를 교환하여
순서를 보장한다.

```
gRPC 클라이언트 ←→ xDS 서버

ADS 스트림 (단일):
  → DiscoveryRequest{type_url: LDS, resource_names: [...]}
  ← DiscoveryResponse{type_url: LDS, resources: [...]}
  → DiscoveryRequest{type_url: LDS, response_nonce: "..."}  // ACK

  → DiscoveryRequest{type_url: RDS, resource_names: [...]}
  ← DiscoveryResponse{type_url: RDS, resources: [...]}
  → DiscoveryRequest{type_url: RDS, response_nonce: "..."}  // ACK

  → DiscoveryRequest{type_url: CDS, resource_names: [...]}
  ...
```

### ACK/NACK 메커니즘

```
xDS 서버 → DiscoveryResponse (version_info: "v1")
  ├── 클라이언트 적용 성공
  │   → DiscoveryRequest (version_info: "v1", response_nonce: "n1")  // ACK
  │
  └── 클라이언트 적용 실패
      → DiscoveryRequest (version_info: "v0", error_detail: "...")   // NACK
        → 이전 버전 유지
```

---

## 9. xDS와 Istio 통합

### Istio 서비스 메시에서의 프록시리스 gRPC

```
┌─────────────────────────────────────────────────────┐
│                 Kubernetes 클러스터                    │
│                                                      │
│  ┌────────────┐     ┌────────────┐                  │
│  │ Istiod     │     │ Istiod     │                  │
│  │ (xDS 서버) │     │ (백업)     │                  │
│  └──────┬─────┘     └────────────┘                  │
│         │                                            │
│         │ ADS 스트림                                 │
│         │                                            │
│  ┌──────▼──────┐     ┌─────────────┐                │
│  │ gRPC Client │────▶│ gRPC Server │                │
│  │ Pod (no     │     │ Pod (no     │                │
│  │  sidecar!)  │     │  sidecar!)  │                │
│  └─────────────┘     └─────────────┘                │
│                                                      │
│  비교: 기존 Istio (프록시 방식)                       │
│  ┌─────────────┐     ┌─────────────┐                │
│  │ gRPC Client │     │ gRPC Server │                │
│  │ ┌─────────┐ │     │ ┌─────────┐ │                │
│  │ │Envoy    │ │────▶│ │Envoy    │ │                │
│  │ │Sidecar  │ │     │ │Sidecar  │ │                │
│  │ └─────────┘ │     │ └─────────┘ │                │
│  └─────────────┘     └─────────────┘                │
└──────────────────────────────────────────────────────┘
```

### Istio 설정 예시

```yaml
# DestinationRule로 프록시리스 gRPC 활성화
apiVersion: networking.istio.io/v1
kind: DestinationRule
metadata:
  name: my-service
spec:
  host: my-service.my-ns.svc.cluster.local
  trafficPolicy:
    loadBalancer:
      simple: ROUND_ROBIN

# 프록시리스 워크로드 레이블
apiVersion: apps/v1
kind: Deployment
spec:
  template:
    metadata:
      labels:
        app: my-client
      annotations:
        inject.istio.io/templates: grpc-agent
        proxy.istio.io/config: '{"holdApplicationUntilProxyStarts": true}'
```

---

## 10. CSDS (Client Status Discovery Service)

xDS 클라이언트의 현재 설정 상태를 조회할 수 있는 서비스이다.

```go
import "google.golang.org/grpc/xds/csds"

// CSDS 서비스 등록
csds.RegisterClientStatusDiscoveryServer(adminServer)
```

```bash
# grpcdebug로 xDS 상태 확인
grpcdebug localhost:8080 xds status

# 출력 예시:
# Name                   Status   Version  Type   LastUpdated
# my-service-listener    ACKed    v3       LDS    2024-01-01 12:00:00
# my-service-route       ACKed    v3       RDS    2024-01-01 12:00:01
# cluster-a              ACKed    v3       CDS    2024-01-01 12:00:02
# cluster-a-endpoints    ACKed    v3       EDS    2024-01-01 12:00:03
```

---

## 11. xDS 기능 지원 현황

| 기능 | 지원 | gRPC-Go 구현 |
|------|------|-------------|
| LDS (Listener) | 예 | 클라이언트/서버 |
| RDS (Route) | 예 | 경로 기반 라우팅 |
| CDS (Cluster) | 예 | LB 정책, 서킷 브레이커 |
| EDS (Endpoint) | 예 | 엔드포인트, 가중치 |
| SDS (Secret) | 예 | TLS 인증서 자동 로테이션 |
| ADS | 예 | 단일 스트림 리소스 교환 |
| RBAC | 예 | 서버 측 인가 정책 |
| Fault Injection | 예 | 테스트용 장애 주입 |
| Retry Policy | 예 | xDS 재시도 설정 |
| Weighted Clusters | 예 | 트래픽 분배 |
| Ring Hash LB | 예 | 일관된 해싱 |
| Outlier Detection | 예 | 비정상 엔드포인트 제거 |
| Custom LB | 예 | WRR, Least Request |

---

## 12. xDS 보안 (mTLS)

### 자동 mTLS

xDS를 통해 **자동 mTLS**를 구성할 수 있다. xDS 서버가 SDS로
인증서를 제공하고, LDS 설정에서 mTLS를 요구하면,
gRPC 클라이언트/서버가 자동으로 mTLS를 적용한다.

```
xDS 서버 ──(SDS)──▶ 인증서/키 제공
    │
    ├── LDS: "이 리스너에서 mTLS 사용"
    ├── CDS: "이 클러스터에 mTLS로 연결"
    │
    ▼
gRPC 클라이언트/서버: 자동 mTLS 적용
    └── 인증서 자동 로테이션 (SDS 워치)
```

### Certificate Provider 플러그인

```json
{
  "certificate_providers": {
    "default": {
      "plugin_name": "file_watcher",
      "config": {
        "certificate_file": "/certs/tls.crt",
        "private_key_file": "/certs/tls.key",
        "ca_certificate_file": "/certs/ca.crt",
        "refresh_interval": "600s"
      }
    }
  }
}
```

---

## 13. 트러블슈팅

### xDS 연결 문제

```bash
# 1. 부트스트랩 설정 확인
echo $GRPC_XDS_BOOTSTRAP
cat $GRPC_XDS_BOOTSTRAP

# 2. xDS 서버 연결 확인
grpcdebug localhost:8080 xds status
# → "NACKed" 상태면 설정 오류

# 3. 로깅 활성화
export GRPC_GO_LOG_SEVERITY_LEVEL=info
export GRPC_GO_LOG_VERBOSITY_LEVEL=99

# 4. CSDS로 상세 상태 확인
grpcurl -plaintext localhost:8080 \
  envoy.service.status.v3.ClientStatusDiscoveryService/FetchClientStatus
```

### 일반적인 문제

| 증상 | 원인 | 해결 |
|------|------|------|
| "xds: bootstrap env vars not set" | 부트스트랩 미설정 | GRPC_XDS_BOOTSTRAP 설정 |
| 리소스 NACK | xDS 설정 파싱 실패 | xDS 서버 설정 확인 |
| 엔드포인트 없음 | EDS 미수신 | CDS 클러스터 이름 확인 |
| mTLS 실패 | 인증서 만료/불일치 | SDS/Certificate Provider 확인 |
| 라우팅 실패 | RDS 규칙 불일치 | 호스트/경로 패턴 확인 |

---

## 14. 종합: xDS vs 전통적 방식

```
┌────────────────────┬──────────────┬──────────────┬──────────────┐
│     기능            │  DNS 기반    │  Envoy 프록시 │  gRPC xDS    │
├────────────────────┼──────────────┼──────────────┼──────────────┤
│ 서비스 발견         │ DNS SRV/A   │ EDS          │ EDS          │
│ 로드 밸런싱         │ 클라이언트   │ Envoy        │ 클라이언트   │
│ 라우팅              │ 없음        │ RDS          │ RDS          │
│ mTLS               │ 수동 설정    │ SDS          │ SDS          │
│ 트래픽 관리         │ 제한적      │ 풍부         │ 풍부         │
│ 네트워크 홉         │ 0           │ +2 (양쪽)    │ 0            │
│ 리소스 사용         │ 최소        │ 사이드카 필요 │ 최소         │
│ 설정 동적 변경      │ DNS TTL     │ 실시간       │ 실시간       │
│ 복잡도              │ 낮음        │ 높음         │ 중간         │
│ 관측성              │ 제한적      │ 풍부         │ Channelz/CSDS│
└────────────────────┴──────────────┴──────────────┴──────────────┘
```
