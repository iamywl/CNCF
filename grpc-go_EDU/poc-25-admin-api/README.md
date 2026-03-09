# PoC: gRPC-Go Admin API (Channelz + CSDS) 시뮬레이션

## 개요

gRPC-Go의 Admin API를 시뮬레이션한다.
Admin API는 Channelz(채널 계측)와 CSDS(xDS 설정 상태)를 하나의 진입점으로 통합한다.

## 대응하는 gRPC-Go 소스코드

| 이 PoC | gRPC-Go 소스 | 설명 |
|--------|-------------|------|
| `AddService()` | `internal/admin/admin.go` | 서비스 플러그인 등록 |
| `Register()` | `admin/admin.go` | Admin 서비스 일괄 등록 |
| `ChannelzService` | `channelz/service/service.go` | Channelz gRPC 서비스 |
| `Channel` | `internal/channelz/types.go` | 채널 데이터 모델 |
| `CSDSService` | xDS 패키지 내 CSDS 구현 | xDS 설정 덤프 |

## 구현 내용

### 1. Admin 서비스 등록 패턴
- AddService로 플러그인 방식 서비스 등록
- init()에서 등록하여 순환 의존 회피

### 2. Channelz
- 채널 계층 구조: TopChannel → SubChannel → Socket
- RPC 메트릭 (calls started/succeeded/failed)
- 소켓 상세 (주소, 스트림 수, 메시지 수)

### 3. CSDS
- xDS 설정 상태 덤프 (Listener, Cluster 등)
- ACK/NACK 상태 추적

## 실행 방법

```bash
go run main.go
```

## 핵심 포인트

- AddService 패턴으로 admin 패키지가 xDS를 import하지 않아 순환 의존을 피한다
- Channelz는 gRPC 내부 상태를 실시간으로 관찰할 수 있는 진단 도구다
- CSDS는 xDS 환경에서 설정 디버깅에 사용한다
