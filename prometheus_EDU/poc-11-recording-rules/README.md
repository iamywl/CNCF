# PoC-11: Recording Rules

## 개요

Prometheus Recording Rule의 핵심 동작을 Go 표준 라이브러리만으로 구현한 PoC이다.
Recording Rule은 PromQL 표현식의 결과를 **사전 계산(pre-compute)**하여 새로운 시계열로 저장하는 메커니즘이다.

**실제 소스 참조:**
- `rules/recording.go` — `RecordingRule.Eval()`: 쿼리 실행 후 메트릭 이름/라벨 재작성
- `rules/group.go` — `Group.Eval()`: 규칙 그룹 평가, `seriesInPreviousEval`로 stale 시리즈 감지

## Recording Rules를 사용하는 이유

### 1. 카디널리티 감소

원본 메트릭이 `job`, `method`, `status`, `handler` 등 다수의 라벨을 가질 때, 대시보드에서는 보통 `job` 단위 합계만 필요하다.

```
# 원본: 12개 시리즈 (job x method x status x handler)
http_requests_total{job="api-server", method="GET", status="200", handler="/users"}
http_requests_total{job="api-server", method="GET", status="404", handler="/users"}
http_requests_total{job="api-server", method="POST", status="200", handler="/users"}
...

# Recording Rule 결과: 3개 시리즈 (job만)
job:http_requests_total:sum{job="api-server"}     = 26865
job:http_requests_total:sum{job="payment-svc"}    = 11323
job:http_requests_total:sum{job="web-frontend"}   = 154600
```

12개 시리즈 → 3개 시리즈로 **75% 카디널리티 감소**.

### 2. 쿼리 성능 향상

Recording Rule 없이 대시보드가 매번 `sum by (job) (http_requests_total)`을 실행하면:
- 12개 시리즈를 모두 읽어야 함
- 대시보드 새로고침마다 반복 연산
- 사용자가 많을수록 PromQL 엔진에 부하 집중

Recording Rule을 사용하면:
- 사전 계산된 3개 시리즈만 읽으면 됨
- 집계 연산은 평가 주기(보통 1분)에 한 번만 수행
- 대시보드는 단순 조회만 하면 됨

### 3. 네이밍 컨벤션

Prometheus 공식 네이밍 규칙: `level:metric:operations`

| 예시 | 의미 |
|------|------|
| `job:http_requests_total:sum` | job 레벨로 합산 |
| `job_method:http_requests:rate5m` | job+method 레벨로 5분 rate |
| `instance:node_cpu:ratio` | instance 레벨 CPU 비율 |

## 구현 구조

```
┌────────────────────────────────────────────────────────────┐
│                       RuleGroup                            │
│  name: "http_recording_rules"                              │
│  interval: 1m                                              │
│  seriesInPreviousEval: []map[string]Labels                 │
│                                                            │
│  ┌──────────────────────────────────────────────────────┐  │
│  │ RecordingRule 1                                      │  │
│  │  name: job:http_requests_total:sum                   │  │
│  │  expr: sum by (job) (http_requests_total)            │  │
│  │  Eval(ts):                                           │  │
│  │    1. QueryFunc 실행 → 벡터 획득                      │  │
│  │    2. __name__ 교체                                   │  │
│  │    3. 추가 라벨 병합                                   │  │
│  └──────────────────────────────────────────────────────┘  │
│  ┌──────────────────────────────────────────────────────┐  │
│  │ RecordingRule 2                                      │  │
│  │  name: job_method:http_requests:rate5m               │  │
│  │  expr: sum by (job,method) (rate(...[5m]))           │  │
│  │  labels: {env: "production"}                         │  │
│  └──────────────────────────────────────────────────────┘  │
│                                                            │
│  Group.Eval(ts):                                           │
│    for each rule:                                          │
│      vector = rule.Eval(ts)                                │
│      storage.Append(vector)                                │
│      diff = seriesInPreviousEval - seriesReturned          │
│      for disappeared series: Append(StaleNaN)              │
│      seriesInPreviousEval = seriesReturned                 │
└────────────────────────────────────────────────────────────┘
```

## Stale 시리즈 처리

Recording Rule의 핵심 메커니즘 중 하나는 **시리즈 소멸 감지**이다.

```
사이클 3: payment-svc 존재
  seriesInPreviousEval = {api-server, payment-svc, web-frontend}

사이클 4: payment-svc 종료
  seriesReturned       = {api-server, web-frontend}
  차이                  = {payment-svc}  ← 사라진 시리즈
  → payment-svc에 StaleNaN 기록

사이클 5: payment-svc 없이 정상
  seriesInPreviousEval = {api-server, web-frontend}
  → StaleNaN 추가 기록 없음 (이미 처리됨)
```

실제 코드 (`group.go:620-639`):
```go
for metric, lset := range g.seriesInPreviousEval[i] {
    if _, ok := seriesReturned[metric]; !ok {
        // 시리즈가 사라짐 → StaleNaN 마커 기록
        app.Append(0, lset, ts, math.Float64frombits(value.StaleNaN))
    }
}
```

StaleNaN은 특수한 NaN 비트 패턴(`0x7ff0000000000002`)으로, 쿼리 엔진이 이 값을 만나면 해당 시리즈를 결과에서 제외한다. 이를 통해 사라진 타겟이나 변경된 라벨로 인한 유령 시리즈(ghost series)를 방지한다.

## 평가 시각 정렬 (EvalTimestamp)

RuleGroup은 평가 시각을 interval에 정렬하여 일관된 시계열을 생성한다.

```
interval = 1분

입력: 12:00:13 → 정렬: 12:00:00
입력: 12:00:47 → 정렬: 12:00:00
입력: 12:01:05 → 정렬: 12:01:00
```

실제 구현에서는 `hash(group) % interval`로 그룹별 오프셋을 추가하여, 여러 그룹의 평가가 동시에 실행되지 않도록 분산한다.

## 실행 방법

```bash
go run main.go
```

외부 의존성 없이 Go 표준 라이브러리만 사용한다.

## 데모 시나리오

| 사이클 | 이벤트 | 결과 |
|--------|--------|------|
| 1-2 | 정상 동작 | 12개 원본 → 3개(sum), 6개(rate) 시리즈 |
| 3 | 트래픽 50% 증가 | 값 변화 반영 |
| 4 | payment-svc 종료 | StaleNaN 기록, 3→2개 시리즈 |
| 5 | payment-svc 없이 계속 | 2개 시리즈 정상 유지 |

## 핵심 학습 포인트

1. **RecordingRule.Eval()** — 쿼리 실행 후 `__name__` 교체 + 추가 라벨 병합 (`recording.go:84-122`)
2. **RuleGroup.Eval()** — 전체 규칙 평가 + 결과 저장 + stale 감지 (`group.go:504-639`)
3. **seriesInPreviousEval** — 이전/현재 시리즈 비교로 소멸 감지 (`group.go:52`)
4. **StaleNaN** — 특수 비트 패턴 NaN으로 시리즈 라이프사이클 관리
5. **EvalTimestamp** — interval 정렬 + hash 기반 오프셋으로 평가 시각 분산 (`group.go:422-445`)
