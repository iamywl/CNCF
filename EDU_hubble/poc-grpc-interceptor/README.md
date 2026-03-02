# PoC: Hubble gRPC Interceptor (미들웨어) 패턴

## 관련 문서
- [02-ARCHITECTURE.md](../02-ARCHITECTURE.md) - gRPC 통신
- [05-API-REFERENCE.md](../05-API-REFERENCE.md) - gRPC API 명세

## 개요

gRPC Interceptor는 HTTP 미들웨어와 동일한 패턴으로, 요청/응답 사이에 로직을 삽입합니다:
- **Unary Interceptor**: 단일 요청/응답 (ServerStatus, GetNodes)
- **Stream Interceptor**: 스트리밍 RPC (GetFlows)
- **체이닝**: 여러 Interceptor를 순서대로 실행

Hubble에서의 사용:
- Prometheus 메트릭 수집 (요청 수, 지연시간)
- 버전 정보 메타데이터 주입
- 인증/인가

## 실행

```bash
go run main.go
```

## 시나리오

### 시나리오 1: 정상 요청
Auth → Logging → Metrics → Version → Handler 전체 체인 통과

### 시나리오 2: 인증 실패
Auth에서 체인 중단 → 이후 Interceptor/Handler 실행 안 됨

### 시나리오 3: 메트릭 누적
여러 메서드 호출 후 Interceptor 메트릭 리포트

## 핵심 학습 내용
- `grpc.UnaryServerInterceptor` / `grpc.StreamServerInterceptor` 시그니처
- `grpc.ChainUnaryInterceptor()`로 체이닝
- 역순 구성으로 실행 순서 보장
- 중간 Interceptor에서 에러 시 체인 중단
- 메타데이터(HTTP/2 헤더) 주입 패턴
