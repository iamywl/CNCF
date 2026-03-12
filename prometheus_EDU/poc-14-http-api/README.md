# PoC-14: Prometheus HTTP API Server

## 개요

Prometheus HTTP API 서버(`web/api/v1/api.go`)의 핵심 구조를 재현하는 PoC이다. 실제 Prometheus가 외부 클라이언트(Grafana, curl, API 호출)에게 데이터를 제공하는 HTTP API 계층의 설계 패턴을 학습한다.

## 학습 목표

1. **Response 봉투(Envelope) 패턴**: 모든 API 응답이 동일한 JSON 구조로 감싸지는 방식
2. **apiFunc 핸들러 패턴**: 비즈니스 로직과 HTTP 직렬화를 분리하는 설계
3. **에러 타입 분류 체계**: `bad_data`, `execution`, `timeout` 등 구조화된 에러 응답
4. **주요 쿼리 엔드포인트**: instant query, range query, series, labels 등의 동작 원리

## 실제 Prometheus 코드 참조

### Response 구조 (`web/api/v1/api.go`)

```go
type Response struct {
    Status    status   `json:"status"`          // "success" 또는 "error"
    Data      any      `json:"data,omitempty"`   // 성공 시 결과 데이터
    ErrorType string   `json:"errorType,omitempty"` // 에러 시 분류
    Error     string   `json:"error,omitempty"`     // 에러 시 메시지
    Warnings  []string `json:"warnings,omitempty"`  // 경고 목록
}
```

모든 Prometheus API 응답은 이 봉투 형식을 따른다. 클라이언트는 먼저 `status` 필드를 확인하고, `"success"`이면 `data`를, `"error"`이면 `errorType`과 `error`를 처리한다.

### apiFunc 패턴

```go
type apiFunc func(r *http.Request) apiFuncResult

type apiFuncResult struct {
    data      any
    err       *apiError
    warnings  annotations.Annotations
    finalizer func()
}
```

각 엔드포인트 핸들러(`query`, `queryRange`, `series` 등)는 `apiFunc` 시그니처를 따른다. `Register()` 메서드의 `wrap()` 함수가 이들을 `http.HandlerFunc`로 변환하며, 공통적인 JSON 직렬화, 에러 처리, CORS 설정을 담당한다.

### 에러 타입 분류

| errorType | 의미 | HTTP 상태 코드 |
|-----------|------|----------------|
| `bad_data` | 잘못된 파라미터 | 400 |
| `execution` | 쿼리 실행 에러 | 422 |
| `timeout` | 쿼리 타임아웃 | 422 |
| `canceled` | 쿼리 취소 | 422 |
| `internal` | 내부 서버 에러 | 500 |
| `unavailable` | 서비스 불가 | 503 |
| `not_found` | 리소스 없음 | 404 |

### 주요 엔드포인트 라우팅 (`Register()`)

```
GET  /api/v1/query           → 즉시 쿼리 (instant query)
GET  /api/v1/query_range     → 범위 쿼리 (range query)
GET  /api/v1/series          → 시리즈 메타데이터 조회
GET  /api/v1/labels          → 레이블 이름 목록
GET  /api/v1/label/:name/values → 특정 레이블의 값 목록
GET  /-/healthy              → 헬스 체크
GET  /-/ready                → 레디니스 체크
```

### 응답 예시

**성공 응답 (instant query)**:
```json
{
  "status": "success",
  "data": {
    "resultType": "vector",
    "result": [
      {
        "metric": {"__name__": "http_requests_total", "code": "200"},
        "value": [1678900000, "1234.5"]
      }
    ]
  }
}
```

**성공 응답 (range query)**:
```json
{
  "status": "success",
  "data": {
    "resultType": "matrix",
    "result": [
      {
        "metric": {"__name__": "go_memstats_alloc_bytes"},
        "values": [[1678900000, "52428800"], [1678900060, "53477376"]]
      }
    ]
  }
}
```

**에러 응답**:
```json
{
  "status": "error",
  "errorType": "bad_data",
  "error": "invalid parameter \"query\": empty query"
}
```

## 핵심 설계 패턴

### 1. wrap() 함수의 관심사 분리

`Register()` 안의 `wrap()` 함수가 핵심이다. 각 핸들러는 비즈니스 로직만 처리하고 `(data, error)`를 반환하면, `wrap()`이 다음을 일괄 처리한다:
- CORS 헤더 설정
- JSON 직렬화
- 에러 시 적절한 HTTP 상태 코드 매핑
- 압축 핸들러 적용
- readiness 체크

### 2. ready 가드

API 엔드포인트는 `api.ready()` 래퍼를 통해 Prometheus가 아직 초기화 중이면 503을 반환한다. 단, `/-/healthy`는 ready 여부와 무관하게 항상 200을 반환한다. 이는 쿠버네티스의 livenessProbe/readinessProbe 패턴과 일치한다.

### 3. 쿼리 결과 타입

- **vector**: instant query 결과 (특정 시점의 값 배열)
- **matrix**: range query 결과 (시간 범위의 시계열 배열)
- **scalar**: 단일 스칼라 값
- **string**: 문자열 값

값은 항상 `[timestamp, "string_value"]` 형태의 2-튜플로 반환된다. 숫자 값도 문자열로 전달하는 이유는 JSON의 부동소수점 정밀도 손실을 방지하기 위함이다.

## 실행 방법

```bash
go run main.go
```

## 예상 출력

```
=== Prometheus HTTP API Server PoC ===

서버 시작: http://127.0.0.1:xxxxx

--- 1. 헬스 체크: GET /-/healthy ---
  HTTP 200: Prometheus PoC is Healthy.

--- 2. 레디니스 체크: GET /-/ready ---
  HTTP 200: Prometheus PoC is Ready.

--- 3. Instant Query: GET /api/v1/query ---
    쿼리: http_requests_total{code="200",method="GET"}
  HTTP 200
  {
    "data": {
      "result": [...],
      "resultType": "vector"
    },
    "status": "success"
  }

--- 4. Range Query: GET /api/v1/query_range ---
  HTTP 200
  {
    "data": {
      "result": [...],
      "resultType": "matrix"
    },
    "status": "success"
  }

--- 9. 에러 응답: 빈 쿼리 ---
  HTTP 400
  {
    "error": "invalid parameter \"query\": empty query",
    "errorType": "bad_data",
    "status": "error"
  }
```

## 아키텍처 다이어그램

```
클라이언트 (Grafana, curl, API)
        │
        ▼
  ┌─────────────────────────────────────────┐
  │          HTTP Server (net/http)          │
  │                                         │
  │  /-/healthy  ──→ healthy()              │
  │  /-/ready    ──→ readyCheck()           │
  │                                         │
  │  /api/v1/*   ──→ ready 가드 체크         │
  │                   │                      │
  │                   ▼                      │
  │            ┌─────────────┐              │
  │            │   wrap()    │ ← JSON 직렬화 │
  │            │  공통 래퍼   │   에러 처리    │
  │            └──────┬──────┘              │
  │                   │                      │
  │        ┌──────────┼──────────┐          │
  │        ▼          ▼          ▼          │
  │   query()   queryRange()  series()     │
  │   labels()  labelValues()              │
  │        │          │          │          │
  │        └──────────┼──────────┘          │
  │                   ▼                      │
  │          ┌────────────────┐             │
  │          │    Storage     │             │
  │          │ (인메모리 TSDB) │             │
  │          └────────────────┘             │
  └─────────────────────────────────────────┘
```
