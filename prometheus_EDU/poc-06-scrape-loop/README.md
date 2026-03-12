# PoC-06: Prometheus Scrape Loop

## 개요

Prometheus의 **스크레이프 루프(Scrape Loop)** 핵심 동작을 시뮬레이션하는 PoC이다.
스크레이프 루프는 Prometheus가 모니터링 대상에서 메트릭을 수집하는 핵심 메커니즘으로,
주기적 HTTP 폴링, 텍스트 파싱, stale marker 처리를 포함한다.

## 실행 방법

```bash
go run main.go
```

외부 의존성 없이 Go 표준 라이브러리만 사용한다.

## 핵심 개념

### 1. Scrape Loop 라이프사이클

Prometheus의 scrape loop(`scrape/scrape.go`)는 다음 단계로 동작한다:

```
┌─────────────────────────────────────────────────────────┐
│                  Scrape Loop Lifecycle                   │
├─────────────────────────────────────────────────────────┤
│                                                         │
│  1. Offset 대기                                         │
│     └─ hash(target) % interval → 시작 시점 분산          │
│                                                         │
│  2. 주기적 스크레이핑 (ticker 기반)                       │
│     ┌─────────────────────────────┐                     │
│     │ HTTP GET /metrics           │                     │
│     │ → Parse text format         │◄── interval마다 반복  │
│     │ → Append to storage         │                     │
│     │ → Detect stale metrics      │                     │
│     │ → Write report metrics      │                     │
│     └─────────────────────────────┘                     │
│                                                         │
│  3. 종료 시 End-of-run Stale Markers                     │
│     └─ 2 interval + 10% 대기 후 stale marker 기록        │
│                                                         │
└─────────────────────────────────────────────────────────┘
```

### 2. Offset 기반 시간 분산

수백 개의 타겟을 동시에 스크레이핑하면 부하가 집중된다.
Prometheus는 타겟의 해시값으로 offset을 계산하여 스크레이핑 시점을 분산한다.

```
실제 코드 (scrape/target.go:155):
  base   = int64(interval) - now % int64(interval)
  offset = (t.hash() ^ offsetSeed) % uint64(interval)
  next   = base + int64(offset)
```

같은 타겟은 항상 같은 offset을 가지므로, 재시작 후에도 동일한 시점에 스크레이핑한다.

### 3. Prometheus Text Format

Prometheus의 표준 메트릭 노출 형식이다:

```
# HELP http_requests_total The total number of HTTP requests.
# TYPE http_requests_total counter
http_requests_total{method="GET",code="200"} 1027
http_requests_total{method="POST",code="200"} 342

# HELP go_goroutines Number of goroutines.
# TYPE go_goroutines gauge
go_goroutines 42
```

| 요소 | 설명 |
|------|------|
| `# HELP` | 메트릭 설명 (선택) |
| `# TYPE` | 메트릭 타입: counter, gauge, histogram, summary (선택) |
| `metric_name{labels} value` | 실제 메트릭 데이터 |
| labels | `key="value"` 쌍, 쉼표로 구분 |

### 4. Stale Markers (핵심)

Stale marker는 Prometheus가 **시계열의 소멸**을 명시적으로 표시하는 메커니즘이다.

#### 왜 필요한가?

stale marker가 없으면 사라진 메트릭의 마지막 값이 쿼리에서 계속 반환된다 (lookback delta 내).
이는 잘못된 대시보드 표시와 알림을 유발한다.

#### StaleNaN

```
일반 NaN:  0x7ff8000000000001  (quiet NaN)
StaleNaN:  0x7ff0000000000002  (signaling NaN, Prometheus 전용)
```

IEEE 754의 signaling NaN을 활용하여 일반 NaN과 구별한다.
`model/value/value.go`에 정의되어 있다.

#### 3가지 stale marker 발생 시점

| 시점 | 트리거 | 실제 코드 |
|------|--------|----------|
| **메트릭 소멸** | 이전 스크레이프에 있던 메트릭이 현재 없음 | `scrapeCache.forEachStale()` |
| **스크레이프 실패** | HTTP 에러, timeout 등 | `scrapeAndReport()` 에러 경로 |
| **루프 종료** | 타겟 제거, 설정 변경 | `endOfRunStaleness()` |

#### scrapeCache의 cur/prev 메커니즘

```
Scrape N:    seriesCur = {A, B, C}     seriesPrev = {A, B, C, D}
                                        → D가 seriesCur에 없음 → D에 StaleNaN

Scrape N+1:  seriesCur = {A, B}        seriesPrev = {A, B, C}
                                        → C가 seriesCur에 없음 → C에 StaleNaN
```

매 스크레이프 후 `iterDone()`에서 cur과 prev를 교체(swap)한다.

### 5. Report 메트릭

매 스크레이프마다 자동으로 기록되는 내부 메트릭이다 (`scrapeLoop.report()`):

| 메트릭 | 설명 |
|--------|------|
| `up` | 타겟 상태 (1=정상, 0=실패) |
| `scrape_duration_seconds` | 스크레이핑 소요 시간 |
| `scrape_samples_scraped` | 수집된 샘플 수 |
| `scrape_samples_post_metric_relabeling` | relabel 후 샘플 수 |
| `scrape_series_added` | 새로 추가된 시리즈 수 |

### 6. End-of-run Staleness

스크레이프 루프가 종료될 때 (`scrape.go:1446`):

```
1. 루프 종료
2. 다음 스크레이프 시점까지 대기 (1 interval)
3. 한 번 더 대기 (1 interval) — 타겟이 재생성될 수 있으므로
4. 추가 10% interval 대기 — 안전 마진
5. 모든 활성 시리즈에 StaleNaN 기록
```

이렇게 2+ interval을 대기하는 이유는 타겟이 다른 scrape pool로 이동(re-shard)될 수 있기 때문이다.
새 pool에서 이미 스크레이핑을 시작했다면 stale marker는 out-of-order로 무시된다.

## 데모 시나리오

| Phase | 동작 | 관찰 포인트 |
|-------|------|------------|
| Phase 1 | 정상 스크레이핑 (5개 메트릭) | offset 대기, 주기적 수집, report 메트릭 |
| Phase 2 | `temperature_celsius` 제거 | 해당 메트릭에 StaleNaN 기록 |
| Phase 3 | 루프 중지 | 모든 활성 시리즈 + report 메트릭에 StaleNaN |

## 소스코드 참조

| 파일 | 내용 |
|------|------|
| `scrape/scrape.go:822` | `scrapeLoop` 구조체 정의 |
| `scrape/scrape.go:887` | `scrapeCache` — cur/prev 기반 staleness 감지 |
| `scrape/scrape.go:1234` | `scrapeLoop.run()` — offset 대기 + ticker 루프 |
| `scrape/scrape.go:1313` | `scrapeAndReport()` — 스크레이핑 + 저장 + report |
| `scrape/scrape.go:1446` | `endOfRunStaleness()` — 종료 시 stale marker |
| `scrape/scrape.go:2076` | `report()` — up, duration, samples 등 내부 메트릭 기록 |
| `scrape/target.go:155` | `Target.offset()` — 해시 기반 시간 분산 |
| `model/value/value.go:28` | `StaleNaN = 0x7ff0000000000002` |
