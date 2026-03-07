# PoC #11: 쿼리 프론트엔드 - 쿼리 분할, 큐잉, 캐싱

## 개요

Loki의 쿼리 프론트엔드는 대규모 쿼리를 효율적으로 처리하기 위한 프록시 계층이다. 쿼리 분할, 테넌트별 큐잉, 결과 캐싱, 중복 제거를 통합적으로 처리한다.

## 실제 Loki 코드와의 관계

| 이 PoC | Loki 실제 코드 |
|--------|---------------|
| `SplitByInterval` | `pkg/querier/queryrange/split_by_interval.go` |
| `TenantQueue` | `pkg/scheduler/queue/` |
| `ResultCache` | `pkg/querier/queryrange/results_cache.go` |
| `QueryDedup` | `pkg/querier/queryrange/` (Single Flight 패턴) |
| `WorkerPool` | Querier 인스턴스 풀 |

## 실행 방법

```bash
go run main.go
```

## 핵심 메커니즘

### 쿼리 분할 (Query Splitting)
```
원본: {app="api"} 00:00~24:00 (24시간)
분할: 00:00~01:00, 01:00~02:00, ..., 23:00~24:00 (24개 서브쿼리)
→ 병렬 실행 후 시간순 병합
```

### 테넌트별 큐잉 (Fair Scheduling)
```
tenant-A: [q1, q2, q3, q4, q5]
tenant-B: [q1, q2]
tenant-C: [q1, q2, q3]

Dequeue 순서 (Round-Robin):
A-q1 → B-q1 → C-q1 → A-q2 → B-q2 → C-q2 → A-q3 → C-q3 → A-q4 → A-q5
```

### 결과 캐시
- TTL 기반 캐시 만료
- 동일 쿼리+시간범위에 대한 해시 키
- 캐시 히트 시 쿼리 실행 없이 즉시 반환

### 쿼리 중복 제거 (Single Flight)
- 동시에 들어온 동일 쿼리를 1번만 실행
- 나머지 요청은 결과를 공유

## 시연 내용

1. **쿼리 분할**: 24시간 범위 → 여러 서브쿼리로 분할
2. **테넌트별 큐잉**: 라운드 로빈 공정 스케줄링
3. **최대 대기 제한**: 테넌트당 큐 오버플로 방지
4. **결과 캐시**: 캐시 미스 → 저장 → 히트 흐름
5. **중복 제거**: 5개 동시 요청 → 1번만 실행
6. **전체 통합**: 분할 + 큐잉 + 캐싱 + 중복제거 파이프라인
