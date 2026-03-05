# PoC-11: 서비스 메시 시뮬레이션 (L7 프록시 + xDS)

## 개요

Cilium 서비스 메시의 핵심 메커니즘을 시뮬레이션한다:
- xDS 프로토콜 (리소스 디스커버리)
- L7 프록시 리다이렉트 (tc→proxy→tc 패턴)
- 리스너, 라우트, 클러스터, 엔드포인트 모델
- 핫 리로드 설정 업데이트

## 시뮬레이션 대상

| 컴포넌트 | 실제 소스 | 시뮬레이션 |
|----------|----------|-----------|
| xDS Cache | `pkg/envoy/xds/cache.go` | 리소스 저장, version 관리, TX() |
| xDS Server | `pkg/envoy/xds/server.go` | DiscoveryRequest/Response, ACK/NACK |
| 리소스 타입 | `pkg/envoy/resources.go` | LDS/RDS/CDS/EDS TypeURL |
| XDSServer | `pkg/envoy/xds_server.go` | AddListener, UpdateNetworkPolicy |
| 프록시 리다이렉트 | `pkg/proxy/`, BPF tc | tc→proxy→tc 패턴 |

## xDS 프로토콜 흐름

```
Envoy                         Cilium xDS Server
  |                                |
  |--- DiscoveryRequest(v=0) ---→  |
  |                                | (resources from Cache)
  |←-- DiscoveryResponse(v=1) ---  |
  |                                |
  |--- ACK(v=1, nonce=1) ------→  | (적용 성공)
  |                                |
  |--- NACK(v=0, error="...") -→  | (적용 실패)
  |                                |
  (Cache 업데이트 → version bump)
  |                                |
  |←-- DiscoveryResponse(v=2) ---  | (push)
```

## 실행

```bash
go run main.go
```

## 데모 항목

1. **xDS 리소스 타입**: LDS/RDS/CDS/EDS TypeURL 매핑
2. **리소스 설정**: 리스너/라우트/클러스터/엔드포인트 생성
3. **xDS 스트리밍**: 요청/응답/ACK/NACK 흐름
4. **L7 프록시 리다이렉트**: tc→Envoy→tc 패킷 흐름, HTTP 라우팅
5. **핫 리로드**: 엔드포인트 동적 업데이트와 version bump
6. **리스너 관리**: 추가/제거
7. **부하 시뮬레이션**: L7 라우팅 분포
