# PoC: Jaeger Span/Trace 데이터 모델

## 개요

Jaeger의 핵심 데이터 모델인 **Span**과 **Trace** 구조를 시뮬레이션한다.
Jaeger 소스코드의 `internal/uimodel/model.go`에 정의된 데이터 구조를 기반으로,
분산 추적(distributed tracing)의 기본 개념을 실제 코드로 구현한다.

## 시뮬레이션 대상

| Jaeger 소스 경로 | 시뮬레이션 내용 |
|------------------|----------------|
| `internal/uimodel/model.go` | Span, Trace, Reference, Process, KeyValue, Log 구조체 |
| `internal/uimodel/model.go` | ReferenceType (CHILD_OF, FOLLOWS_FROM) |
| `internal/uimodel/model.go` | ValueType (string, bool, int64, float64, binary) |

## 핵심 개념

### Span
분산 시스템에서 단일 작업 단위를 표현한다. 각 Span은 다음을 포함한다:
- **TraceID**: 전체 요청 흐름의 고유 식별자 (128비트)
- **SpanID**: 개별 작업의 고유 식별자 (64비트)
- **ParentSpanID**: 부모 Span의 ID (루트 Span은 비어있음)
- **OperationName**: 수행 중인 작업의 이름
- **References**: 다른 Span과의 관계 목록
- **Tags**: 키-값 쌍으로 된 메타데이터
- **Logs**: 시간 기반 이벤트 기록

### Trace
동일한 TraceID를 공유하는 Span들의 집합이다. 하나의 분산 요청 전체를 표현한다.

### Reference 유형
- **CHILD_OF**: 동기적 의존 관계. 부모가 자식의 완료를 기다림
- **FOLLOWS_FROM**: 비동기적 인과 관계. 선행 Span이 후행 Span을 유발하지만 결과를 기다리지 않음

## 데모 시나리오

전자상거래 체크아웃 흐름을 시뮬레이션한다:

```
frontend (HTTP GET /checkout)                 [루트]
├── cart-service (gRPC GetCart)               [CHILD_OF]
│   └── cart-service (Redis GET)             [CHILD_OF]
├── payment-service (gRPC ProcessPayment)    [CHILD_OF]
│   ├── payment-service (validate-card)      [CHILD_OF]
│   └── payment-gateway (HTTP POST /charge)  [CHILD_OF]
└── notification-service (async SendEmail)   [FOLLOWS_FROM]
```

## 실행 방법

```bash
cd poc-span-model
go run main.go
```

## 출력 내용

1. **Span 참조 유형 비교**: CHILD_OF와 FOLLOWS_FROM의 차이를 ASCII 다이어그램으로 설명
2. **Trace 트리 시각화**: 타임라인 바가 포함된 트리 형태의 Trace 출력
3. **개별 Span 상세**: 각 Span의 ID, 태그, 로그 등 전체 정보
4. **Trace 통계 요약**: 서비스별 Span 수, 소요시간, 참조 유형별 통계

## Jaeger 소스코드와의 대응

| PoC 코드 | Jaeger 소스 |
|----------|------------|
| `type Span struct` | `internal/uimodel/model.go:Span` |
| `type Trace struct` | `internal/uimodel/model.go:Trace` |
| `type Reference struct` | `internal/uimodel/model.go:Reference` |
| `type Process struct` | `internal/uimodel/model.go:Process` |
| `type KeyValue struct` | `internal/uimodel/model.go:KeyValue` |
| `type Log struct` | `internal/uimodel/model.go:Log` |
| `ChildOf / FollowsFrom` | `internal/uimodel/model.go:ChildOf / FollowsFrom` |
