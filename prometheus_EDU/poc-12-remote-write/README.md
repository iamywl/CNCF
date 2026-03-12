# PoC-12: Remote Write 파이프라인

## 개요

Prometheus의 Remote Write 파이프라인을 Go 표준 라이브러리만으로 시뮬레이션합니다.
실제 구현체인 `storage/remote/queue_manager.go`의 핵심 알고리즘을 재현합니다.

## 핵심 개념

### Remote Write란?

Prometheus가 수집한 메트릭 데이터를 외부 저장소(Thanos, Cortex, Mimir 등)로 전송하는 프로토콜입니다.
WAL(Write-Ahead Log)에서 읽은 샘플을 QueueManager가 배치로 묶어 HTTP POST로 전송합니다.

### 아키텍처

```
Scrape Engine → WAL → WAL Watcher → QueueManager → Remote Endpoint
                                         │
                                    ┌────┼────┐
                                    ▼    ▼    ▼
                                  Shard0 Shard1 ... ShardN
                                    │    │         │
                                    └────┴────┬────┘
                                              ▼
                                         HTTP POST
                                    (protobuf + snappy)
```

## 구현 상세

### 1. 샤드 기반 병렬 전송

실제 Prometheus에서 `shards.enqueue()`는 시리즈의 `HeadSeriesRef`를 샤드 수로 나눈 나머지로 샤드를 결정합니다:

```go
// storage/remote/queue_manager.go:1356
func (s *shards) enqueue(ref chunks.HeadSeriesRef, data timeSeries) bool {
    shard := uint64(ref) % uint64(len(s.queues))
    appended := s.queues[shard].Append(data)
    ...
}
```

PoC에서는 `fnv64a(labelsKey) % numShards`로 동일한 해싱 전략을 구현합니다.
같은 시리즈는 항상 같은 샤드로 라우팅되어 순서 보장이 됩니다.

### 2. 배치 전송 (runShard)

각 샤드는 독립적인 goroutine에서 실행되며, 두 가지 조건으로 배치를 전송합니다:

- **MaxSamplesPerSend 도달**: 배치가 가득 차면 즉시 전송
- **BatchSendDeadline 경과**: 타이머 만료 시 모인 만큼만 전송

```go
// storage/remote/queue_manager.go:1550-1553
maxCount = s.qm.cfg.MaxSamplesPerSend
...
timer := time.NewTimer(time.Duration(s.qm.cfg.BatchSendDeadline))
```

### 3. 지수 백오프 재시도

`sendWriteRequestWithBackoff()`는 recoverable 오류(5xx)에 대해 지수 백오프로 재시도합니다:

```go
// storage/remote/queue_manager.go:2021-2080
backoff := t.cfg.MinBackoff
for {
    err := attempt(try)
    if err == nil { return nil }

    // RecoverableError만 재시도
    var backoffErr RecoverableError
    if !errors.As(err, &backoffErr) { return err }

    sleepDuration = backoff
    // Retry-After 헤더 지원
    if backoffErr.retryAfter > 0 {
        sleepDuration = backoffErr.retryAfter
    }
    time.Sleep(sleepDuration)
    backoff = min(sleepDuration*2, t.cfg.MaxBackoff)
}
```

**재시도 정책:**
- 5xx 응답: recoverable → 지수 백오프 후 재시도
- 4xx 응답: non-recoverable → 즉시 폐기
- 네트워크 오류: recoverable → 재시도
- `Retry-After` 헤더: 서버가 지정한 대기 시간 우선 적용

### 4. 동적 Resharding (calculateDesiredShards)

EWMA(Exponentially Weighted Moving Average) 기반으로 최적 샤드 수를 실시간 계산합니다:

```go
// storage/remote/queue_manager.go:1153-1213
timePerSample = dataOutDuration / dataOutRate
backlogCatchup = 0.05 * dataPending
desiredShards = timePerSample * (dataInRate*dataKeptRatio + backlogCatchup)
```

**핵심 변수:**

| 변수 | 의미 |
|------|------|
| `dataInRate` | 초당 수집 속도 (EWMA) |
| `dataOutRate` | 초당 전송 속도 (EWMA) |
| `timePerSample` | 샘플 1개 전송 소요 시간 |
| `dataPending` | 밀린 데이터 양 (delay * inRate) |
| `backlogCatchup` | 밀린 양의 5%를 매초 따라잡기 |

**Tolerance 메커니즘:**
- 현재 샤드 수의 +-30% 이내면 변경하지 않음 (불필요한 resharding 방지)
- `MinShards` ~ `MaxShards` 범위 내에서만 조정

### 5. EWMA Rate 추적

```
ewmaWeight = 0.2
rate = 0.2 * instantRate + 0.8 * previousRate
```

최근 값에 20% 가중치를 두어 급격한 변동을 완화하면서도 추세를 반영합니다.
`shardUpdateDuration` (기본 10초)마다 tick()을 호출하여 rate를 갱신합니다.

## 실행

```bash
go run main.go
```

## 데모 시나리오

| 단계 | 내용 |
|------|------|
| 1 | Remote Write 수신 서버 시작 (HTTP) |
| 2 | QueueManager 시작 (4 샤드) |
| 3 | 1000개 샘플 전송 (50 시리즈 x 20 샘플) |
| 4 | 샤드별 분포 확인 (해시 기반 균등 분배) |
| 5 | 수신 서버에서 모든 샘플 수신 확인 |
| 6 | 서버 장애(503) 시뮬레이션 → 지수 백오프 재시도 → 복구 확인 |
| 7 | 동적 resharding (4 → 8 샤드) + 재분배 확인 |

## 실제 Prometheus와의 차이

| 항목 | 실제 Prometheus | PoC |
|------|----------------|-----|
| 직렬화 | protobuf + snappy 압축 | JSON |
| 샤드 키 | HeadSeriesRef (정수) | fnv64a(labels) |
| WAL 연동 | WAL Watcher가 데이터 공급 | 직접 Append() 호출 |
| 메트릭 | Prometheus 메트릭 등록 | atomic 카운터 |
| Resharding | 기존 샤드 drain 후 새 샤드 시작 | 잔여 데이터 재분배 |
| Proto 버전 | v1 (WriteRequest) + v2 지원 | 단일 JSON 형식 |

## 소스 참조

- `storage/remote/queue_manager.go` — QueueManager, shards, calculateDesiredShards
- `storage/remote/queue_manager.go:1356` — enqueue (샤드 할당)
- `storage/remote/queue_manager.go:1540` — runShard (배치 수집/전송)
- `storage/remote/queue_manager.go:2021` — sendWriteRequestWithBackoff (재시도)
- `storage/remote/queue_manager.go:1153` — calculateDesiredShards (동적 샤딩)
