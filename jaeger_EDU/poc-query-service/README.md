# PoC 6: Query Service HTTP API 시뮬레이션

## 개요

Jaeger의 **Query Service**가 제공하는 HTTP API를 시뮬레이션한다. Jaeger UI는 이 API를 통해 트레이스 데이터를 검색하고 시각화한다.

## 실제 Jaeger 소스 참조

| 파일 | 핵심 구조체/함수 | 역할 |
|------|-----------------|------|
| `cmd/jaeger/internal/extension/jaegerquery/internal/http_handler.go` | `APIHandler`, `RegisterRoutes()` | HTTP 라우트 등록 및 핸들러 |
| `cmd/jaeger/internal/extension/jaegerquery/internal/http_handler.go` | `structuredResponse` | 통일 API 응답 포맷 |
| `cmd/jaeger/internal/extension/jaegerquery/querysvc/` | `QueryService` | 비즈니스 로직 계층 |

## API 엔드포인트

| 메서드 | 경로 | 설명 | 핸들러 |
|--------|------|------|--------|
| GET | `/api/services` | 서비스 목록 | `getServices()` |
| GET | `/api/operations?service=X` | 오퍼레이션 목록 | `getOperations()` |
| GET | `/api/traces?service=X&limit=N` | 트레이스 검색 | `search()` |
| GET | `/api/traces/{traceID}` | 특정 트레이스 조회 | `getTrace()` |

## 응답 포맷

Jaeger API는 모든 응답을 `structuredResponse` 포맷으로 반환한다:

```json
{
  "data": [...],
  "total": 10,
  "limit": 20,
  "offset": 0,
  "errors": []
}
```

## 시뮬레이션 내용

1. **인메모리 스토리지**: 50개의 샘플 트레이스 사전 생성
2. **HTTP 서버**: Go 표준 `net/http`로 실제 Jaeger 포트(16686)에서 실행
3. **API 요청 시뮬레이션**: 모든 엔드포인트에 자동 요청 전송
4. **에러 처리**: 존재하지 않는 traceID 조회 시 404 응답

## 실행 방법

```bash
cd poc-query-service
go run main.go
```

## 쿼리 흐름

```
HTTP Request
  → APIHandler (라우트 매칭)
    → queryParser (파라미터 파싱)
      → QueryService (비즈니스 로직)
        → Storage Backend (데이터 조회)
          → uiconv.FromDomain() (UI 모델 변환)
            → structuredResponse (JSON 응답)
```

## 핵심 학습 포인트

- Jaeger Query Service의 HTTP API 설계 패턴
- `structuredResponse`를 통한 일관된 API 응답 구조
- 스토리지 백엔드 추상화 계층의 역할
- 트레이스 데이터의 UI 모델(UITrace/UISpan) 구조
