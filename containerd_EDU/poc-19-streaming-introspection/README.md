# PoC-19: containerd gRPC Streaming + Introspection 시뮬레이션

## 개요

containerd는 gRPC 기반 API로 클라이언트와 통신한다. 이 PoC는 이벤트 스트리밍(server-side streaming)과
인트로스펙션(플러그인/서비스 상태 조회)을 시뮬레이션한다.

## 시뮬레이션하는 개념

| 개념 | 실제 containerd 코드 | 시뮬레이션 |
|------|---------------------|-----------|
| Event Streaming | `services/events/service.go` | Pub/Sub 이벤트 스트리밍 |
| Introspection | `services/introspection/service.go` | 플러그인 목록/상태 조회 |
| gRPC Services | `api/services/` | 서비스/메서드 레지스트리 |
| Event Filter | 토픽/네임스페이스 필터 | 구독 시 필터 적용 |

## 실행 방법

```bash
cd containerd_EDU/poc-19-streaming-introspection
go run main.go
```
