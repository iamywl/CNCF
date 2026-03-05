# PoC 07: HTTP 미들웨어 체인

## 개요

Grafana 백엔드 서버의 HTTP 미들웨어 체인 패턴을 시뮬레이션한다.
Grafana는 모든 HTTP 요청이 여러 미들웨어를 순서대로 통과하도록 설계되어 있으며,
각 미들웨어는 요청 전처리, 후처리, 에러 복구 등의 역할을 담당한다.

## Grafana 실제 구조

Grafana의 미들웨어 체인은 `pkg/middleware/` 디렉토리에 구현되어 있으며,
`pkg/api/api.go`에서 라우트 등록 시 미들웨어를 조합한다.

주요 미들웨어:
- **RequestMetadata**: 요청 ID, 시작 시간 등 메타데이터 설정
- **RequestTracing**: OpenTelemetry 기반 트레이싱 스팬 생성
- **RequestMetrics**: Prometheus 메트릭 수집 (요청 수, 응답 시간)
- **Logger**: 요청/응답 로깅
- **Recovery**: 패닉 복구 및 500 에러 응답
- **Auth**: 세션/토큰 기반 인증 처리
- **RBAC**: 역할 기반 접근 제어

## 시뮬레이션 내용

1. **Handler/Middleware 타입 정의**: 함수 기반 체인 패턴
2. **Context 구조체**: 요청 상태를 전달하는 컨텍스트
3. **7개 미들웨어 구현**: 실행 순서 추적 포함
4. **체인 빌더**: 미들웨어를 역순으로 감싸는 Build 함수
5. **패닉 복구 시연**: Recovery 미들웨어 동작 확인
6. **타이밍 메트릭 수집**: 각 미들웨어의 실행 시간 기록

## 핵심 패턴

```
요청 → RequestMetadata → Tracing → Metrics → Logger → Recovery → Auth → RBAC → Handler
응답 ← RequestMetadata ← Tracing ← Metrics ← Logger ← Recovery ← Auth ← RBAC ← Handler
```

미들웨어는 "양파 껍질" 구조로, 요청 시 바깥에서 안으로, 응답 시 안에서 바깥으로 실행된다.

## 실행

```bash
go run main.go
```

## 학습 포인트

- 함수형 미들웨어 체인 패턴 (데코레이터 패턴)
- 요청 컨텍스트를 통한 상태 전달
- 패닉 복구를 통한 서버 안정성 확보
- 미들웨어 실행 순서의 중요성 (인증 전에 Recovery가 와야 하는 이유)
