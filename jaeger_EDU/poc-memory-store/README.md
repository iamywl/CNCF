# PoC: Jaeger In-Memory Store

## 개요

Jaeger의 **인메모리 스토리지**(`internal/storage/v2/memory`)를 시뮬레이션한다.
링 버퍼 기반의 Trace 저장, LRU 퇴거, 서비스/오퍼레이션 인덱싱, 쿼리 기능을 구현한다.

## 시뮬레이션 대상

| Jaeger 소스 경로 | 시뮬레이션 내용 |
|------------------|----------------|
| `internal/storage/v2/memory/tenant.go` | Tenant 구조체 (링 버퍼, 인덱스, storeTraces, findTraceAndIds) |
| `internal/storage/v2/memory/memory.go` | Store 구조체 (GetServices, GetOperations, FindTraces) |
| `internal/storage/v2/memory/config.go` | Configuration (MaxTraces) |

## 핵심 개념

### 링 버퍼 기반 저장

실제 Jaeger의 `Tenant` 구조체는 고정 크기의 `traces[]` 배열과 `mostRecent` 인덱스를 사용하여
링 버퍼를 구현한다:

```
traces[] = [T0, T1, T2, T3, T4]    MaxTraces = 5
                          ↑
                      mostRecent = 3

새 Trace T5 삽입:
  mostRecent = (3 + 1) % 5 = 4
  T4가 퇴거되고 T5가 위치 4에 저장

traces[] = [T0, T1, T2, T3, T5]
                              ↑
                          mostRecent = 4

또 다른 Trace T6 삽입:
  mostRecent = (4 + 1) % 5 = 0
  T0가 퇴거되고 T6이 위치 0에 저장

traces[] = [T6, T1, T2, T3, T5]
            ↑
        mostRecent = 0
```

실제 Jaeger 소스(`tenant.go`):
```go
t.mostRecent = (t.mostRecent + 1) % t.config.MaxTraces
if !t.traces[t.mostRecent].id.IsEmpty() {
    delete(t.ids, t.traces[t.mostRecent].id)  // 퇴거
}
t.ids[traceId] = t.mostRecent
t.traces[t.mostRecent] = traceAndId{...}
```

### 서비스/오퍼레이션 인덱싱

```
services: {"frontend", "user-service", "order-service"}
operations: {
    "frontend": {"HTTP GET /": {}, "HTTP POST /api/orders": {}},
    "user-service": {"gRPC GetUser": {}, "DB SELECT": {}},
}
```

### FindTraces 역순 탐색

`mostRecent`에서 시작하여 역순으로 탐색하여 최신 Trace부터 반환한다:

```
탐색 순서: mostRecent → mostRecent-1 → ... → 0 → MaxTraces-1 → ...
조건 검사: validTrace() → 각 Span에 대해 validSpan() 호출
종료 조건: SearchDepth 도달 또는 빈 슬롯 만남
```

### Thread-Safety

`sync.RWMutex`로 동시성을 보장한다:
- **읽기 (GetTrace, FindTraces, GetServices)**: `RLock()` - 여러 고루틴이 동시에 읽기 가능
- **쓰기 (WriteSpan)**: `Lock()` - 하나의 고루틴만 쓰기 가능

## 데모 시나리오

1. **링 버퍼 퇴거 시연**: MaxTraces=5인 스토어에 8개 Trace 저장, 퇴거 동작 확인
2. **서비스/오퍼레이션 인덱싱**: GetServices(), GetOperations() 결과 확인
3. **FindTraces 쿼리 테스트**: 서비스, 오퍼레이션, 시간 범위, SearchDepth 필터
4. **동시성 테스트**: 10개 고루틴이 동시에 쓰기
5. **동일 TraceID 누적**: 같은 TraceID로 여러 Span 추가

## 실행 방법

```bash
cd poc-memory-store
go run main.go
```

## 출력 내용

1. **링 버퍼 구조 설명**: ASCII 다이어그램으로 동작 원리 설명
2. **퇴거 시연**: 8개 Trace 삽입 시 링 버퍼 상태 변화
3. **인덱싱 결과**: 서비스별 오퍼레이션 목록
4. **쿼리 결과**: 다양한 조건의 FindTraces 결과
5. **동시성 통계**: 동시 쓰기 후 에러/퇴거 통계
6. **Trace 누적**: 동일 TraceID에 Span이 누적되는 동작 확인

## Jaeger 소스코드와의 대응

| PoC 코드 | Jaeger 소스 |
|----------|------------|
| `MemoryStore` 구조체 | `internal/storage/v2/memory/tenant.go:Tenant` |
| `traceRecord` 구조체 | `internal/storage/v2/memory/tenant.go:traceAndId` |
| `Configuration` 구조체 | `internal/storage/v2/memory/config.go:Configuration` |
| `WriteSpan()` 메서드 | `tenant.go:storeTraces()` |
| `FindTraces()` 메서드 | `tenant.go:findTraceAndIds()` |
| `validTrace()` / `validSpan()` | `tenant.go:validTrace()` / `validSpan()` |
| `GetServices()` | `memory.go:GetServices()` |
| `GetOperations()` | `memory.go:GetOperations()` |
| 링 버퍼 순환 인덱스 | `(t.mostRecent + 1) % t.config.MaxTraces` |
| LRU 퇴거 | `delete(t.ids, t.traces[t.mostRecent].id)` |
