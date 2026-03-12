# 17. 컴팩션 & 조각 모음 (Compaction & Defragmentation)

## 개요

etcd는 MVCC(Multi-Version Concurrency Control) 모델을 사용하므로, 모든 키의 모든 수정 이력이 리비전 단위로 영구 저장된다. 시간이 지나면 오래된 리비전이 누적되어 저장 공간을 소진하고 성능이 저하된다. 컴팩션(Compaction)은 이 문제를 해결하기 위해 지정된 리비전 이전의 불필요한 데이터를 논리적으로 삭제하는 작업이고, 조각 모음(Defragmentation)은 삭제 후 남은 물리적 빈 공간을 실제로 회수하는 작업이다.

이 문서에서는 컴팩션의 전체 흐름(요청 → 인덱스 정리 → DB 삭제 → 완료), 자동 컴팩션 모드, 조각 모음의 내부 동작, Watch와의 관계를 소스코드 기반으로 분석한다.

---

## 컴팩션 아키텍처 전체 흐름

```
┌─────────────────────────────────────────────────────────────────────┐
│                     컴팩션 전체 흐름                                  │
├─────────────────────────────────────────────────────────────────────┤
│                                                                     │
│  1. 컴팩션 요청 (수동 또는 자동)                                      │
│     ┌──────────────┐   ┌──────────────────────┐                     │
│     │ etcdctl      │   │ v3compactor          │                     │
│     │ compact REV  │   │ (Periodic / Revision)│                     │
│     └──────┬───────┘   └──────────┬───────────┘                     │
│            │                      │                                  │
│            └──────────┬───────────┘                                  │
│                       ▼                                              │
│  2. Raft 합의 → 모든 노드에서 동시 실행                                │
│                       │                                              │
│                       ▼                                              │
│  3. store.Compact(rev)                                               │
│     ├── updateCompactRev(rev)   ← compactMainRev 갱신               │
│     ├── SetScheduledCompact()    ← 예약된 컴팩션 기록                  │
│     └── compact()               ← FIFO 스케줄러에 비동기 작업 등록      │
│                       │                                              │
│                       ▼                                              │
│  4. scheduleCompaction(rev, prevRev)                                 │
│     ├── kvindex.Compact(rev)    ← 트리 인덱스에서 오래된 리비전 제거    │
│     └── DB 배치 삭제             ← 1000개씩 삭제 + 10ms sleep         │
│                       │                                              │
│                       ▼                                              │
│  5. UnsafeSetFinishedCompact()  ← 완료된 컴팩션 리비전 기록            │
│     + Hash 저장                  ← 데이터 무결성 검증용                 │
│                                                                     │
└─────────────────────────────────────────────────────────────────────┘
```

---

## store.Compact() 메서드

### 엔트리 포인트

```
소스 경로: server/storage/mvcc/kvstore.go (271행)

func (s *store) Compact(trace *traceutil.Trace, rev int64) (<-chan struct{}, error) {
    s.mu.Lock()

    // 이전 컴팩션이 완료되었는지 확인
    prevCompactionCompleted := s.checkPrevCompactionCompleted()

    // compactMainRev 갱신 및 유효성 검사
    ch, prevCompactRev, err := s.updateCompactRev(rev)
    trace.Step("check and update compact revision")
    if err != nil {
        s.mu.Unlock()
        return ch, err
    }
    s.mu.Unlock()

    // FIFO 스케줄러에 비동기 작업 등록
    return s.compact(trace, rev, prevCompactRev, prevCompactionCompleted), nil
}
```

**반환값 `<-chan struct{}`의 의미:**

이 채널은 컴팩션이 완료되면 닫힌다. 호출자는 이 채널을 통해 컴팩션 완료를 비동기적으로 대기할 수 있다.

### updateCompactRev: compactMainRev 추적

```
소스 경로: server/storage/mvcc/kvstore.go (196행)

func (s *store) updateCompactRev(rev int64) (<-chan struct{}, int64, error) {
    s.revMu.Lock()

    // 1. 이미 컴팩션된 리비전보다 이전이면 에러
    if rev <= s.compactMainRev {
        ch := make(chan struct{})
        f := schedule.NewJob("kvstore_updateCompactRev_compactBarrier", ...)
        s.fifoSched.Schedule(f)
        s.revMu.Unlock()
        return ch, 0, ErrCompacted
    }

    // 2. 현재 리비전보다 미래이면 에러
    if rev > s.currentRev {
        s.revMu.Unlock()
        return nil, 0, ErrFutureRev
    }

    // 3. 이전 컴팩션 리비전 저장 후 갱신
    compactMainRev := s.compactMainRev
    s.compactMainRev = rev

    // 4. 예약된 컴팩션 리비전을 백엔드에 기록
    SetScheduledCompact(s.b.BatchTx(), rev)
    s.b.ForceCommit()   // 디스크에 즉시 반영

    s.revMu.Unlock()
    return nil, compactMainRev, nil
}
```

**compactMainRev vs currentRev:**

| 필드 | 의미 |
|------|------|
| `currentRev` | 마지막 완료된 트랜잭션의 리비전 |
| `compactMainRev` | 마지막 컴팩션 대상 리비전 |

요청된 rev가 `compactMainRev`과 `currentRev` 사이에 있어야만 유효한 컴팩션 요청이다.

**ScheduledCompact와 FinishedCompact:**

```
┌─────────────────────────────────────────────────┐
│         컴팩션 진행 상태 추적                      │
├─────────────────────────────────────────────────┤
│                                                 │
│  ScheduledCompact = 100  ← 예약됨 (시작 시 기록)  │
│  FinishedCompact  = 80   ← 완료됨 (완료 시 기록)  │
│                                                 │
│  만약 서버가 크래시되면:                            │
│  - 재시작 시 Scheduled > Finished 확인            │
│  - 미완료 컴팩션 재개                              │
│                                                 │
│  정상 완료 후:                                    │
│  ScheduledCompact = 100                          │
│  FinishedCompact  = 100                          │
│                                                 │
└─────────────────────────────────────────────────┘
```

### checkPrevCompactionCompleted

```
소스 경로: server/storage/mvcc/kvstore.go (224행)

func (s *store) checkPrevCompactionCompleted() bool {
    tx := s.b.ReadTx()
    tx.RLock()
    defer tx.RUnlock()
    scheduledCompact, scheduledCompactFound := UnsafeReadScheduledCompact(tx)
    finishedCompact, finishedCompactFound := UnsafeReadFinishedCompact(tx)
    return scheduledCompact == finishedCompact && scheduledCompactFound == finishedCompactFound
}
```

이전 컴팩션이 완료되었는지 확인하는 함수다. `scheduledCompact`와 `finishedCompact`가 일치하면 이전 컴팩션이 정상 완료된 것이다. 이 정보는 해시 저장 여부를 결정하는 데 사용된다. 이전 컴팩션이 중단되었으면 해시 체인이 깨지므로 해시를 저장하지 않는다.

---

## scheduleCompaction: 실제 삭제 로직

### FIFO 스케줄러로 비동기 실행

```
소스 경로: server/storage/mvcc/kvstore.go (233행)

func (s *store) compact(trace *traceutil.Trace, rev, prevCompactRev int64, prevCompactionCompleted bool) <-chan struct{} {
    ch := make(chan struct{})
    j := schedule.NewJob("kvstore_compact", func(ctx context.Context) {
        if ctx.Err() != nil {
            s.compactBarrier(ctx, ch)
            return
        }

        // 실제 컴팩션 수행
        hash, err := s.scheduleCompaction(rev, prevCompactRev)
        if err != nil {
            s.compactBarrier(context.TODO(), ch)
            return
        }

        // 이전 컴팩션이 완료된 경우에만 해시 저장
        if prevCompactionCompleted {
            s.hashes.Store(hash)
        }
        close(ch)   // 완료 신호
    })

    s.fifoSched.Schedule(j)
    return ch
}
```

**왜 FIFO 스케줄러인가:**

- 컴팩션은 시간이 오래 걸릴 수 있는 작업이다.
- Raft 적용 루프(apply loop)에서 동기적으로 실행하면 다른 요청 처리가 차단된다.
- FIFO 스케줄러를 통해 비동기적으로 실행하되, 순서를 보장한다.
- 여러 컴팩션 요청이 동시에 들어와도 순차적으로 처리된다.

### scheduleCompaction 상세

```
소스 경로: server/storage/mvcc/kvstore_compaction.go (28행)

func (s *store) scheduleCompaction(compactMainRev, prevCompactRev int64) (KeyValueHash, error) {
    totalStart := time.Now()

    // 1단계: 트리 인덱스 컴팩션
    keep := s.kvindex.Compact(compactMainRev)
    indexCompactionPauseMs.Observe(float64(time.Since(totalStart) / time.Millisecond))

    // 2단계: DB 배치 삭제
    totalStart = time.Now()
    keyCompactions := 0
    end := make([]byte, 8)
    binary.BigEndian.PutUint64(end, uint64(compactMainRev+1))

    batchNum := s.cfg.CompactionBatchLimit   // 기본값: 1000
    h := newKVHasher(prevCompactRev, compactMainRev, keep)
    last := make([]byte, 8+1+8)

    for {
        start := time.Now()
        tx := s.b.BatchTx()
        tx.LockOutsideApply()

        // batchNum(1000)개씩 키를 읽어서 처리
        keys, values := tx.UnsafeRange(schema.Key, last, end, int64(batchNum))

        for i := range keys {
            rev := BytesToRev(keys[i])
            if _, ok := keep[rev]; !ok {
                tx.UnsafeDelete(schema.Key, keys[i])   // 보존 대상이 아니면 삭제
                keyCompactions++
            }
            h.WriteKeyValue(keys[i], values[i])   // 해시 계산에 포함
        }

        // 마지막 배치인 경우 완료 처리
        if len(keys) < batchNum {
            UnsafeSetFinishedCompact(tx, compactMainRev)   // 완료 기록
            tx.Unlock()
            hash := h.Hash()
            s.lg.Info("finished scheduled compaction", ...)
            return hash, nil
        }

        tx.Unlock()
        last = RevToBytes(Revision{Main: rev.Main, Sub: rev.Sub + 1}, last)

        // 즉시 커밋하여 쓰기 버퍼 비우기
        s.b.ForceCommit()

        // 10ms 대기: 다른 요청에 CPU/IO 양보
        select {
        case <-time.After(s.cfg.CompactionSleepInterval):
        case <-s.stopc:
            return KeyValueHash{}, fmt.Errorf("interrupted due to stop signal")
        }
    }
}
```

### 배치 처리 설계

```
┌─────────────────────────────────────────────────────────────────┐
│                    배치 처리 파이프라인                           │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  ┌─────────┐    ┌─────────┐    ┌─────────┐    ┌─────────┐     │
│  │ Batch 1 │    │ Batch 2 │    │ Batch 3 │    │ Batch N │     │
│  │ 1000개  │    │ 1000개  │    │ 1000개  │    │ < 1000  │     │
│  └────┬────┘    └────┬────┘    └────┬────┘    └────┬────┘     │
│       │              │              │              │           │
│       ▼              ▼              ▼              ▼           │
│  ┌────────┐    ┌────────┐    ┌────────┐    ┌────────────┐     │
│  │ Delete │    │ Delete │    │ Delete │    │ Delete +    │     │
│  │ + Commit│    │ + Commit│    │ + Commit│    │ SetFinished│     │
│  └────┬────┘    └────┬────┘    └────┬────┘    └────────────┘     │
│       │              │              │                           │
│       ▼              ▼              ▼                           │
│    10ms sleep     10ms sleep     10ms sleep                     │
│                                                                 │
│  CompactionBatchLimit = 1000 (기본값)                            │
│  CompactionSleepInterval = 10ms (기본값)                         │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

**왜 1000개씩 나눠서 삭제하는가:**

1. **잠금 시간 최소화**: `tx.LockOutsideApply()`는 다른 쓰기 작업을 차단한다. 모든 키를 한번에 삭제하면 긴 시간 동안 잠금이 유지되어 서비스 지연을 초래한다.
2. **메모리 사용 제어**: 한 번에 모든 키를 메모리에 로드하면 메모리 사용량이 급증한다.
3. **ForceCommit()으로 쓰기 버퍼 비우기**: 삭제가 쓰기 버퍼에 누적되면 메모리와 디스크 모두에 부담이 된다.

**왜 10ms sleep인가:**

- 컴팩션이 CPU와 I/O를 독점하는 것을 방지
- 다른 클라이언트 요청(Range, Put 등)이 처리될 시간을 보장
- 너무 짧으면 컨텍스트 스위칭 오버헤드, 너무 길면 컴팩션이 느려짐

### StoreConfig

```
소스 경로: server/storage/mvcc/kvstore.go

var (
    defaultCompactionBatchLimit    = 1000
    defaultCompactionSleepInterval = 10 * time.Millisecond
)

type StoreConfig struct {
    CompactionBatchLimit    int
    CompactionSleepInterval time.Duration
}
```

---

## treeIndex.Compact(): 인덱스 컴팩션 알고리즘

### Compact 메서드

```
소스 경로: server/storage/mvcc/index.go (192행)

func (ti *treeIndex) Compact(rev int64) map[Revision]struct{} {
    available := make(map[Revision]struct{})
    ti.lg.Info("compact tree index", zap.Int64("revision", rev))

    // 1. 트리를 클론하여 순회 (락 분리)
    ti.Lock()
    clone := ti.tree.Clone()
    ti.Unlock()

    // 2. 클론된 트리를 순회하면서 각 키 인덱스 컴팩션
    clone.Ascend(func(keyi *keyIndex) bool {
        ti.Lock()
        keyi.compact(ti.lg, rev, available)   // 오래된 리비전 제거
        if keyi.isEmpty() {
            ti.tree.Delete(keyi)               // 빈 키 인덱스 삭제
        }
        ti.Unlock()
        return true
    })

    return available   // 보존해야 할 리비전 맵 반환
}
```

**왜 Clone 후 순회하는가:**

- 트리를 직접 순회하면서 수정하면 반복자(iterator)가 무효화될 수 있다.
- Clone은 구조만 복사하고 keyIndex 포인터는 공유하므로 메모리 효율적이다.
- 각 keyIndex를 수정할 때만 잠금을 획득하여 동시성을 보장한다.

### keyIndex.compact() 알고리즘

```
각 키에 대한 리비전 이력:

key: "/foo"
  generations:
    gen0: [rev=2, rev=5, rev=8]        ← 이전 세대
    gen1: [rev=12, rev=15, rev=20]     ← 현재 세대

Compact(rev=10) 수행 시:
  - gen0의 모든 리비전이 rev=10 이전이므로 gen0 전체 제거
  - 단, gen0에서 가장 최근 리비전(rev=8)은 available 맵에 추가
    (DB에서 삭제하지 않음 → 클라이언트가 rev=10에서 조회 시 필요)
  - gen1은 rev=10 이후이므로 유지

결과:
  available = {Revision{Main:8}: struct{}{}}
  (이 리비전은 DB에서 보존)
```

### Keep 메서드

```
소스 경로: server/storage/mvcc/index.go (217행)

func (ti *treeIndex) Keep(rev int64) map[Revision]struct{} {
    available := make(map[Revision]struct{})
    ti.RLock()
    defer ti.RUnlock()
    ti.tree.Ascend(func(keyi *keyIndex) bool {
        keyi.keep(rev, available)
        return true
    })
    return available
}
```

`Keep`은 `Compact`와 달리 인덱스를 수정하지 않고, 특정 리비전에서 보존해야 할 리비전 목록만 반환한다. 해시 계산(`hashByRev`)에서 사용된다.

---

## 자동 컴팩션: v3compactor

### Compactor 인터페이스

```
소스 경로: server/etcdserver/api/v3compactor/compactor.go

const (
    ModePeriodic = "periodic"
    ModeRevision = "revision"
)

type Compactor interface {
    Run()       // 백그라운드 루프 시작
    Stop()      // 루프 중단
    Pause()     // 일시 정지
    Resume()    // 재개
}

func New(lg *zap.Logger, mode string, retention time.Duration, rg RevGetter, c Compactable) (Compactor, error) {
    switch mode {
    case ModePeriodic:
        return newPeriodic(lg, clockwork.NewRealClock(), retention, rg, c), nil
    case ModeRevision:
        return newRevision(lg, clockwork.NewRealClock(), int64(retention), rg, c), nil
    default:
        return nil, fmt.Errorf("unsupported compaction mode %s", mode)
    }
}
```

### Periodic 모드 (시간 기반)

```
소스 경로: server/etcdserver/api/v3compactor/periodic.go

type Periodic struct {
    lg     *zap.Logger
    clock  clockwork.Clock
    period time.Duration      // 보존 기간

    rg RevGetter              // 현재 리비전 제공
    c  Compactable            // 컴팩션 실행

    revs   []int64            // 슬라이딩 윈도우 (리비전 히스토리)
    ctx    context.Context
    cancel context.CancelFunc

    mu     sync.RWMutex
    paused bool
}
```

**Periodic 컴팩션의 슬라이딩 윈도우 메커니즘:**

```
┌─────────────────────────────────────────────────────────────────┐
│              Periodic 컴팩션 슬라이딩 윈도우                     │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  period = 1시간, compactInterval = 1시간                         │
│  retryInterval = 6분 (period / 10)                               │
│  retentions = 11 (슬라이딩 윈도우 크기)                           │
│                                                                 │
│  시간  t0   t1   t2   t3   t4   ...  t10  t11  t12              │
│       +6m  +6m  +6m  +6m  +6m       +6m  +6m  +6m              │
│                                                                 │
│  revs: [100, 120, 140, 160, 180, ..., 280, 300, 320]            │
│         ▲                                     ▲                  │
│         │                                     │                  │
│      revs[0]                              revs[10]              │
│      (1시간 전 리비전)                    (현재 리비전)            │
│                                                                 │
│  컴팩션 실행: compact(revs[0]) → 리비전 100까지 컴팩션            │
│  성공 후: revs = revs[1:] → [120, 140, ..., 300, 320]           │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

**Run() 메서드 상세:**

```
소스 경로: server/etcdserver/api/v3compactor/periodic.go (100행)

func (pc *Periodic) Run() {
    compactInterval := pc.getCompactInterval()   // period > 1h이면 1h
    retryInterval := pc.getRetryInterval()       // compactInterval / 10
    retentions := pc.getRetentions()             // period / retryInterval + 1

    go func() {
        lastRevision := int64(0)
        lastSuccess := pc.clock.Now()
        baseInterval := pc.period

        for {
            // 1. 현재 리비전 기록
            pc.revs = append(pc.revs, pc.rg.Rev())
            if len(pc.revs) > retentions {
                pc.revs = pc.revs[1:]   // 슬라이딩 윈도우 유지
            }

            // 2. retryInterval 대기 (일시정지 상태면 건너뜀)
            select {
            case <-pc.ctx.Done():
                return
            case <-pc.clock.After(retryInterval):
                if pc.paused { continue }
            }

            rev := pc.revs[0]   // 가장 오래된 리비전 (보존 기간 전)

            // 3. 아직 보존 기간이 지나지 않았거나 같은 리비전이면 건너뜀
            if pc.clock.Now().Sub(lastSuccess) < baseInterval || rev == lastRevision {
                continue
            }

            // 첫 번째 컴팩션 후에는 compactInterval 사용
            if baseInterval == pc.period {
                baseInterval = compactInterval
            }

            // 4. 컴팩션 실행
            _, err := pc.c.Compact(pc.ctx, &pb.CompactionRequest{Revision: rev})
            if err == nil || errors.Is(err, mvcc.ErrCompacted) {
                lastRevision = rev
                lastSuccess = pc.clock.Now()
            }
        }
    }()
}
```

**getCompactInterval / getRetryInterval:**

```
func (pc *Periodic) getCompactInterval() time.Duration {
    itv := pc.period
    if itv > time.Hour {
        itv = time.Hour   // 최대 1시간 간격으로 컴팩션
    }
    return itv
}

func (pc *Periodic) getRetryInterval() time.Duration {
    itv := pc.period
    if itv > time.Hour { itv = time.Hour }
    return itv / retryDivisor   // 1/10 간격으로 리비전 수집
}
```

| period 설정 | compactInterval | retryInterval | retentions |
|-------------|-----------------|---------------|------------|
| 5초 | 5초 | 500ms | 11 |
| 59분 | 59분 | 5.9분 | 11 |
| 1시간 | 1시간 | 6분 | 11 |
| 24시간 | 1시간 | 6분 | 241 |

### Revision 모드 (리비전 기반)

```
소스 경로: server/etcdserver/api/v3compactor/revision.go

type Revision struct {
    lg        *zap.Logger
    clock     clockwork.Clock
    retention int64           // 보존할 리비전 수

    rg RevGetter
    c  Compactable

    ctx    context.Context
    cancel context.CancelFunc

    mu     sync.Mutex
    paused bool
}

const revInterval = 5 * time.Minute
```

**Run() 메서드:**

```
func (rc *Revision) Run() {
    prev := int64(0)
    go func() {
        for {
            select {
            case <-rc.ctx.Done():
                return
            case <-rc.clock.After(revInterval):   // 5분마다 확인
                if rc.paused { continue }
            }

            rev := rc.rg.Rev() - rc.retention   // 현재 리비전 - 보존 수
            if rev <= 0 || rev == prev {
                continue
            }

            _, err := rc.c.Compact(rc.ctx, &pb.CompactionRequest{Revision: rev})
            if err == nil || errors.Is(err, mvcc.ErrCompacted) {
                prev = rev
            }
        }
    }()
}
```

**두 모드 비교:**

```
┌──────────────────────────────────────────────────────────────────┐
│               Periodic vs Revision 모드 비교                     │
├─────────────────────┬────────────────────────────────────────────┤
│        항목          │  Periodic              │  Revision         │
├─────────────────────┼────────────────────────┼───────────────────┤
│ 보존 기준            │ 시간 (예: 1시간)        │ 리비전 수 (예: 1000)│
│ 체크 주기            │ period/10              │ 5분 고정            │
│ 컴팩션 대상 리비전    │ revs[0] (과거 기록)     │ currentRev - retention│
│ 적합한 환경          │ 쓰기 빈도 불규칙        │ 쓰기 빈도 일정       │
│ Kubernetes 사용      │ O (기본: 5분)           │ X                   │
│ 장점                │ 시간 보장               │ 리비전 수 보장       │
│ 단점                │ 리비전 수 예측 불가       │ 시간 보장 없음       │
└─────────────────────┴────────────────────────┴───────────────────┘
```

---

## Backend.Defrag(): 물리 조각 모음

### 왜 조각 모음이 필요한가

bbolt(B+ Tree)는 삭제된 페이지를 프리리스트(freelist)에 추가하지만, 운영체제에 반환하지 않는다. 컴팩션으로 많은 키를 삭제해도 DB 파일 크기는 줄어들지 않는다.

```
┌─────────────────────────────────────────────────────────────────┐
│              DB 크기 vs DB 사용 크기                               │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  컴팩션 전:                                                      │
│  ┌─────────────────────────────────────────────────┐            │
│  │ [데이터] [데이터] [데이터] [데이터] [데이터]     │            │
│  │ DB Size: 100MB    DB InUse: 100MB               │            │
│  └─────────────────────────────────────────────────┘            │
│                                                                 │
│  컴팩션 후 (논리적 삭제):                                         │
│  ┌─────────────────────────────────────────────────┐            │
│  │ [데이터] [빈공간] [데이터] [빈공간] [데이터]     │            │
│  │ DB Size: 100MB    DB InUse: 60MB                │            │
│  └─────────────────────────────────────────────────┘            │
│  ↑ 파일 크기는 변하지 않음! 빈 공간은 프리리스트에 존재             │
│                                                                 │
│  조각 모음 후 (물리적 회수):                                      │
│  ┌──────────────────────────────────────┐                       │
│  │ [데이터] [데이터] [데이터]            │                       │
│  │ DB Size: 60MB     DB InUse: 60MB    │                       │
│  └──────────────────────────────────────┘                       │
│  ↑ 파일 크기가 실제로 줄어듦                                      │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

### defrag() 구현

```
소스 경로: server/storage/backend/backend.go (476행)

func (b *backend) defrag() error {
    now := time.Now()
    isDefragActive.Set(1)
    defer isDefragActive.Set(0)

    // 1. 모든 트랜잭션 잠금 (쓰기 + 읽기 차단)
    b.batchTx.LockOutsideApply()
    defer b.batchTx.Unlock()

    b.mu.Lock()
    defer b.mu.Unlock()

    b.readTx.Lock()
    defer b.readTx.Unlock()

    // 2. 임시 파일 생성
    dir := filepath.Dir(b.db.Path())
    temp, err := os.CreateTemp(dir, "db.tmp.*")

    // 3. 새 bbolt DB를 임시 파일로 열기
    tmpdb, err := bolt.Open(tdbp, 0600, &options)

    // 4. 현재 트랜잭션 커밋 후 닫기
    b.batchTx.unsafeCommit(true)
    b.batchTx.tx = nil

    // 5. 기존 DB의 모든 데이터를 새 DB로 복사
    err = defragdb(b.db, tmpdb, defragLimit)

    // 6. 기존 DB 파일 닫기
    err = b.db.Close()

    // 7. 임시 파일을 원래 경로로 이동 (원자적 교체)
    err = os.Rename(tdbp, dbp)

    // 8. 새 DB 파일 열기
    b.db, err = bolt.Open(dbp, 0600, bopts)

    // 9. 새 트랜잭션 생성
    b.batchTx.tx = b.unsafeBegin(true)
    b.readTx.tx = b.unsafeBegin(false)

    // 10. 크기 로깅
    size2, sizeInUse2 := b.Size(), b.SizeInUse()
    b.lg.Info("finished defragmenting",
        zap.Int64("current-db-size-bytes-diff", size2-size1),
        zap.Int64("current-db-size-in-use-bytes-diff", sizeInUse2-sizeInUse1),
    )
}
```

**조각 모음 프로세스 다이어그램:**

```
┌─────────────────────────────────────────────────────────────────┐
│                   조각 모음 (Defrag) 과정                        │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  1. 잠금 획득                                                    │
│     batchTx.Lock → mu.Lock → readTx.Lock                       │
│     (모든 읽기/쓰기 차단)                                        │
│                                                                 │
│  2. 임시 DB 생성                                                 │
│     db.tmp.xxxx                                                 │
│                                                                 │
│  3. 데이터 복사 (defragdb)                                       │
│     기존 DB → 새 DB (연속적으로 배치)                             │
│     - 버킷 순회, 키-값 복사                                      │
│     - defragLimit(10000)개씩 배치 처리                            │
│                                                                 │
│  4. DB 교체                                                      │
│     기존 DB 닫기 → 임시 파일을 원래 경로로 이동 → 새 DB 열기       │
│                                                                 │
│  5. 트랜잭션 재생성                                               │
│     새 batchTx 시작 → 새 readTx 시작                             │
│                                                                 │
│  6. 잠금 해제                                                    │
│     readTx.Unlock → mu.Unlock → batchTx.Unlock                 │
│                                                                 │
│  ⚠ 전체 과정 동안 서비스 중단 (블로킹 작업)                       │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

**주의사항:**

조각 모음은 **블로킹 작업**이다. 전체 DB에 대해 읽기/쓰기 잠금을 획득하므로, 실행 중에는 모든 클라이언트 요청이 대기한다. 프로덕션 환경에서는 한 노드씩 순차적으로 수행해야 한다.

---

## 컴팩션과 Watch의 관계 (ErrCompacted)

### ErrCompacted 에러

```
소스 경로: server/storage/mvcc/kvstore.go

var (
    ErrCompacted = errors.New("mvcc: required revision has been compacted")
    ErrFutureRev = errors.New("mvcc: required revision is a future revision")
)
```

Watch 클라이언트가 이미 컴팩션된 리비전부터 감시를 요청하면 `ErrCompacted`가 반환된다.

```
Watch 시나리오:

1. 클라이언트가 rev=50부터 Watch 시작
2. 서버에서 rev=100까지 컴팩션 수행
3. 클라이언트 네트워크 일시 단절
4. 재연결 시 rev=80부터 Watch 재개 요청
5. rev=80 < compactMainRev(100) → ErrCompacted 반환
6. 클라이언트는 전체 데이터를 다시 조회해야 함

┌──────────┐         ┌──────────┐
│  Client  │  Watch  │  Server  │
│          │ rev=80  │          │
│          │ ──────→ │          │
│          │         │ rev=80 < │
│          │         │ compact  │
│          │ Error   │ rev=100  │
│          │ ←────── │          │
│          │ ErrComp │          │
│          │ acted   │          │
│          │         │          │
│  List    │         │          │
│  All     │ ──────→ │          │
│  rev=100 │         │          │
│          │ ←────── │          │
│  Watch   │         │          │
│  rev=100 │ ──────→ │          │
└──────────┘         └──────────┘
```

### hashByRev에서의 ErrCompacted

```
소스 경로: server/storage/mvcc/kvstore.go (166행)

func (s *store) hashByRev(rev int64) (hash KeyValueHash, currentRev int64, err error) {
    s.mu.RLock()
    s.revMu.RLock()
    compactRev, currentRev = s.compactMainRev, s.currentRev
    s.revMu.RUnlock()

    if rev > 0 && rev < compactRev {
        s.mu.RUnlock()
        return KeyValueHash{}, 0, ErrCompacted   // 컴팩션된 리비전 요청
    }
    // ...
}
```

---

## Kubernetes의 자동 컴팩션 사용 패턴

Kubernetes의 kube-apiserver는 etcd를 사용할 때 다음과 같은 컴팩션 설정을 권장한다:

```
etcd 기동 옵션:
  --auto-compaction-mode=periodic
  --auto-compaction-retention=5m

의미:
  - 5분 주기로 컴팩션 (Periodic 모드)
  - compactInterval = 5분 (< 1시간이므로 period 그대로)
  - retryInterval = 30초 (5분 / 10)
  - retentions = 11
```

**Kubernetes가 Periodic 모드를 선택하는 이유:**

1. **쓰기 패턴이 불규칙**: Pod 배포, ConfigMap 변경 등의 빈도가 일정하지 않다.
2. **시간 기반 보장**: "최근 5분간의 변경 이력을 보존"이라는 시간 기반 보장이 운영에 더 직관적이다.
3. **Watch 안정성**: 5분 이내에 재연결하면 ErrCompacted를 피할 수 있다.

**Kubernetes의 List-Watch 패턴과 컴팩션:**

```
┌───────────────────────────────────────────────────────────────┐
│           Kubernetes List-Watch와 컴팩션                       │
├───────────────────────────────────────────────────────────────┤
│                                                               │
│  1. kube-apiserver가 etcd Watch 시작 (rev=N)                  │
│  2. 5분마다 자동 컴팩션 실행                                    │
│  3. Watch가 끊어지면 (네트워크 문제 등):                         │
│     a. resourceVersion이 유효하면 → Watch 재개                 │
│     b. ErrCompacted → List (전체 조회) + Watch 재시작           │
│  4. informer의 reflector가 이 로직을 자동 처리                  │
│                                                               │
│  핵심: 컴팩션 주기가 너무 짧으면 ErrCompacted 빈도 증가          │
│       컴팩션 주기가 너무 길면 DB 크기 증가                       │
│       → 5분이 적절한 균형점                                     │
│                                                               │
└───────────────────────────────────────────────────────────────┘
```

---

## 복구 시 컴팩션 재개

서버 재시작 시 미완료된 컴팩션을 감지하고 재개한다.

```
소스 경로: server/storage/mvcc/kvstore.go (restore 함수 내)

// 1. FinishedCompact 복원
finishedCompact, found := UnsafeReadFinishedCompact(tx)
if found {
    s.compactMainRev = finishedCompact
}

// 2. ScheduledCompact 확인
scheduledCompact, _ := UnsafeReadScheduledCompact(tx)

// 3. scheduledCompact > finishedCompact이면 미완료 컴팩션 존재
if scheduledCompact != 0 {
    if _, err := s.compactLockfree(scheduledCompact); err != nil {
        s.lg.Warn("compaction encountered error", ...)
    } else {
        s.lg.Info("resume scheduled compaction",
            zap.Int64("scheduled-compact-revision", scheduledCompact),
        )
    }
}
```

**currentRev 보정:**

```
// 컴팩션으로 인해 최신 리비전이 삭제된 경우 보정
if s.currentRev < s.compactMainRev {
    s.currentRev = s.compactMainRev
}

// 크래시 시나리오: FinishedCompact 미기록 상태에서 복구
if s.currentRev < scheduledCompact {
    s.currentRev = scheduledCompact
}
```

이 보정은 [etcd#17780](https://github.com/etcd-io/etcd/issues/17780)에서 발견된 버그를 수정한 것이다. 최신 리비전이 tombstone이고 컴팩션 직후 크래시가 발생하면 currentRev가 역행할 수 있다.

---

## compactBarrier: 동시성 보호

```
소스 경로: server/storage/mvcc/kvstore.go (136행)

func (s *store) compactBarrier(ctx context.Context, ch chan struct{}) {
    if ctx == nil || ctx.Err() != nil {
        select {
        case <-s.stopc:
        default:
            s.mu.Lock()
            f := schedule.NewJob("kvstore_compactBarrier", func(ctx context.Context) {
                s.compactBarrier(ctx, ch)
            })
            s.fifoSched.Schedule(f)
            s.mu.Unlock()
        }
        return
    }
    close(ch)
}
```

`compactBarrier`는 FIFO 스케줄러의 작업 순서를 이용한 배리어 패턴이다. 컨텍스트가 유효해질 때까지 재스케줄링하여, 이전 컴팩션 작업이 완료된 후에야 채널을 닫는다.

---

## 핵심 정리

| 주제 | 핵심 내용 |
|------|----------|
| 컴팩션 대상 | compactMainRev 이하의 불필요한 리비전 |
| 배치 처리 | 1000개씩 삭제 + 10ms sleep (잠금 시간 최소화) |
| FIFO 스케줄러 | 비동기 실행, apply loop 차단 방지, 순서 보장 |
| treeIndex.Compact | Clone 후 순회, 키별 오래된 리비전 제거, 빈 키 삭제 |
| Periodic 모드 | 시간 기반 슬라이딩 윈도우, retryInterval = period/10 |
| Revision 모드 | 리비전 수 기반, 5분마다 확인, currentRev - retention |
| Defrag | 새 DB로 데이터 복사 → 원자적 교체, 블로킹 작업 |
| DB Size vs InUse | Size=파일크기, InUse=실제 데이터, 차이가 크면 Defrag 필요 |
| ErrCompacted | 컴팩션된 리비전 요청 시 반환, Watch 재시작 트리거 |
| 복구 | ScheduledCompact > FinishedCompact이면 컴팩션 재개 |
| K8s 패턴 | periodic 모드, 5분 보존, List-Watch + ErrCompacted 처리 |
