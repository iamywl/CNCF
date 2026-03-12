# 08. MVCC 저장소 Deep-Dive

## 개요

etcd의 KV 저장소는 MVCC(Multi-Version Concurrency Control) 기반으로 설계되어 있다. 모든 키-값 쌍의 수정은 새로운 리비전을 생성하며, 이전 리비전의 데이터는 컴팩션이 수행될 때까지 보존된다. 이를 통해 etcd는 특정 시점의 데이터 스냅샷 읽기, watch를 통한 변경 이력 추적, 그리고 트랜잭션의 충돌 감지를 지원한다.

이 문서에서는 MVCC 저장소의 핵심 구현인 `store` 구조체, 트랜잭션 처리(`storeTxnRead`, `storeTxnWrite`), 인터페이스 계층(`KV`, `TxnRead`, `TxnWrite`, `ReadView`, `WriteView`), 그리고 컴팩션/복구/해시 검증까지 소스코드 수준에서 분석한다.

소스코드 경로:
- `server/storage/mvcc/kvstore.go` - store 구조체, NewStore, restore, hash, compact
- `server/storage/mvcc/kvstore_txn.go` - storeTxnRead, storeTxnWrite, put, delete, rangeKeys
- `server/storage/mvcc/kv.go` - KV, TxnRead, TxnWrite, ReadView, WriteView 인터페이스
- `server/storage/mvcc/kv_view.go` - readView, writeView 래퍼
- `server/storage/mvcc/kvstore_compaction.go` - scheduleCompaction

---

## 1. 인터페이스 계층 구조

### 1.1 핵심 인터페이스 관계

```
                    ┌─────────────┐
                    │     KV      │
                    │  (최상위)    │
                    └──────┬──────┘
                           │ 임베딩
              ┌────────────┼────────────┐
              │            │            │
        ┌─────┴─────┐ ┌───┴────┐ ┌────┴───────────┐
        │ ReadView  │ │WriteView│ │ Read/Write/     │
        │           │ │         │ │ Compact/Commit/ │
        │           │ │         │ │ Restore/Close   │
        └─────┬─────┘ └───┬────┘ └────────────────┘
              │            │
        ┌─────┴─────┐ ┌───┴────────┐
        │ TxnRead   │ │ TxnWrite   │
        │ +End()    │ │ +TxnRead   │
        └───────────┘ │ +WriteView │
                      │ +Changes() │
                      └────────────┘
```

### 1.2 ReadView 인터페이스

```go
// server/storage/mvcc/kv.go:38-56
type ReadView interface {
    FirstRev() int64
    Rev() int64
    Range(ctx context.Context, key, end []byte, ro RangeOptions) (r *RangeResult, err error)
}
```

| 메서드 | 설명 |
|--------|------|
| `FirstRev()` | 트랜잭션 시점의 첫 KV 리비전 (컴팩션 리비전) |
| `Rev()` | 트랜잭션 시점의 현재 리비전 |
| `Range()` | 키 범위 조회, 특정 리비전 지정 가능 |

### 1.3 WriteView 인터페이스

```go
// server/storage/mvcc/kv.go:66-82
type WriteView interface {
    DeleteRange(key, end []byte) (n, rev int64)
    Put(key, value []byte, lease lease.LeaseID) (rev int64)
}
```

### 1.4 TxnRead / TxnWrite 인터페이스

```go
// server/storage/mvcc/kv.go:58-90
type TxnRead interface {
    ReadView
    End()
}

type TxnWrite interface {
    TxnRead
    WriteView
    Changes() []mvccpb.KeyValue
}
```

`TxnWrite`가 `TxnRead`를 임베딩하는 이유: 쓰기 트랜잭션 내에서도 현재 상태를 읽어야 한다. 예를 들어 `Put`에서 기존 키의 CreateRevision을 조회하거나, `DeleteRange`에서 삭제할 키 목록을 조회할 때 읽기가 필요하다.

### 1.5 KV 인터페이스

```go
// server/storage/mvcc/kv.go:112-134
type KV interface {
    ReadView
    WriteView
    Read(mode ReadTxMode, trace *traceutil.Trace) TxnRead
    Write(trace *traceutil.Trace) TxnWrite
    HashStorage() HashStorage
    Compact(trace *traceutil.Trace, rev int64) (<-chan struct{}, error)
    Commit()
    Restore(b backend.Backend) error
    Close() error
}
```

`KV` 인터페이스는 ReadView와 WriteView를 모두 임베딩한다. 이는 `store` 구조체가 단독 연산(`readView`, `writeView`를 통해)과 트랜잭션 연산(`Read`, `Write`를 통해) 모두를 지원하기 위함이다.

### 1.6 ReadTxMode

```go
// server/storage/mvcc/kv.go:103-110
type ReadTxMode uint32

const (
    ConcurrentReadTxMode = ReadTxMode(1)  // 읽기 버퍼 복사, 높은 동시성
    SharedBufReadTxMode  = ReadTxMode(2)  // 공유 버퍼, 낮은 오버헤드
)
```

**왜 두 가지 모드가 있는가?**

- `ConcurrentReadTxMode`: 읽기 전용 워크로드에서 사용. 트랜잭션 읽기 버퍼를 복사하여 진행 중인 쓰기와 독립적으로 읽기 가능. watch나 Range 요청에서 사용.
- `SharedBufReadTxMode`: 쓰기 트랜잭션 내에서의 읽기나 간단한 상태 조회에 사용. 버퍼를 복사하지 않아 오버헤드가 적지만, 진행 중인 쓰기에 의해 블로킹될 수 있다.

---

## 2. store 구조체 상세

### 2.1 구조 분석

```go
// server/storage/mvcc/kvstore.go:52-81
type store struct {
    ReadView
    WriteView

    cfg StoreConfig

    mu sync.RWMutex       // 트랜잭션(읽기 락) vs 비트랜잭션 변경(쓰기 락)

    b       backend.Backend
    kvindex index           // B-tree 기반 인메모리 인덱스

    le lease.Lessor

    revMu sync.RWMutex     // currentRev, compactMainRev 보호
    currentRev int64        // 마지막 완료된 트랜잭션의 리비전
    compactMainRev int64    // 마지막 컴팩션의 메인 리비전

    fifoSched schedule.Scheduler  // 컴팩션 작업 스케줄러

    stopc chan struct{}

    lg     *zap.Logger
    hashes HashStorage
}
```

### 2.2 2단계 락 전략 (mu와 revMu)

etcd MVCC 스토어의 동시성 제어에서 가장 중요한 설계 결정 중 하나가 두 개의 락을 사용하는 것이다.

```
┌──────────────────────────────────────────────────────────────┐
│                        mu (sync.RWMutex)                      │
│                                                               │
│  역할: 트랜잭션 동시성 제어                                    │
│  - RLock: 읽기/쓰기 트랜잭션 시작 시                           │
│  - Lock:  비트랜잭션 변경 (Restore, Compact 준비 등)           │
│                                                               │
│  특징: 읽기 트랜잭션과 쓰기 트랜잭션 모두 RLock을 사용         │
│        → 쓰기 간 상호배제는 backend.BatchTx의 Lock이 담당      │
└──────────────────────────────────────────────────────────────┘

┌──────────────────────────────────────────────────────────────┐
│                      revMu (sync.RWMutex)                     │
│                                                               │
│  역할: currentRev, compactMainRev 접근 제어                    │
│  - RLock: 읽기 트랜잭션 시작 시 (리비전 스냅샷 획득)           │
│  - Lock:  쓰기 트랜잭션 종료 시 (currentRev 증가)              │
│           컴팩션 시 (compactMainRev 업데이트)                   │
│                                                               │
│  특징: mu보다 세밀한 범위의 락                                 │
│        쓰기 트랜잭션 종료 시 잠깐만 잡고 해제                   │
└──────────────────────────────────────────────────────────────┘
```

**왜 2단계 락인가?**

단일 락으로는 다음 시나리오를 효율적으로 처리할 수 없다:

```
시나리오: 쓰기 트랜잭션 종료 시 새 읽기 트랜잭션 차단

1. 쓰기 트랜잭션이 끝나면 currentRev를 증가시킨다.
2. 이 순간 새 읽기 트랜잭션이 시작되면, 증가 전의 rev를 볼 수도 있고
   증가 후의 rev를 볼 수도 있다 (일관성 문제).
3. revMu.Lock을 잡아 currentRev 증가와 새 읽기 트랜잭션의 rev 획득을
   원자적으로 만든다.

storeTxnWrite.End()에서:
    if len(tw.changes) != 0 {
        tw.s.revMu.Lock()      // 새 읽기 트랜잭션 차단
        tw.s.currentRev++
    }
    tw.tx.Unlock()              // backend 쓰기 버퍼 해제
    if len(tw.changes) != 0 {
        tw.s.revMu.Unlock()    // 새 읽기 트랜잭션 허용
    }
    tw.s.mu.RUnlock()

store.Read()에서:
    s.mu.RLock()
    s.revMu.RLock()             // currentRev 스냅샷 획득
    // ...
    firstRev, rev := s.compactMainRev, s.currentRev
    s.revMu.RUnlock()
```

이 설계로 인해:
- 쓰기 트랜잭션이 `revMu.Lock()`을 잡는 시간은 `currentRev++`과 `tx.Unlock()` 사이의 극히 짧은 구간뿐
- 읽기 트랜잭션은 `revMu.RLock()`으로 일관된 리비전 스냅샷을 얻음
- `mu.RLock()`은 여러 읽기/쓰기 트랜잭션이 동시에 보유 가능

### 2.3 리비전 증가 메커니즘

```
트랜잭션 1: Put("a", "1"), Put("b", "2")

시작 시:
  tw.beginRev = s.currentRev  (예: 5)

Put("a", "1"):
  rev = tw.beginRev + 1 = 6
  idxRev = {Main: 6, Sub: 0}   (Sub = len(tw.changes) = 0)
  → backend에 키 6_0 으로 저장
  → kvindex에 Revision{6, 0} 등록
  tw.changes = [{key:"a", value:"1", ModRevision:6}]

Put("b", "2"):
  rev = tw.beginRev + 1 = 6    (같은 트랜잭션이므로 같은 Main rev)
  idxRev = {Main: 6, Sub: 1}   (Sub = len(tw.changes) = 1)
  → backend에 키 6_1 으로 저장
  → kvindex에 Revision{6, 1} 등록
  tw.changes = [{key:"a",...}, {key:"b", value:"2", ModRevision:6}]

End():
  s.currentRev++  → 6
```

핵심 포인트:
- 같은 트랜잭션의 모든 변경은 같은 Main 리비전을 공유
- Sub 리비전이 트랜잭션 내의 순서를 나타냄
- `currentRev`는 트랜잭션 종료 시 1 증가

---

## 3. store 생성과 초기화

### 3.1 NewStore()

```go
// server/storage/mvcc/kvstore.go:86-134
func NewStore(lg *zap.Logger, b backend.Backend, le lease.Lessor, cfg StoreConfig) *store {
    // 기본값 설정
    if cfg.CompactionBatchLimit == 0 {
        cfg.CompactionBatchLimit = defaultCompactionBatchLimit  // 1000
    }
    if cfg.CompactionSleepInterval == 0 {
        cfg.CompactionSleepInterval = defaultCompactionSleepInterval  // 10ms
    }

    s := &store{
        cfg:     cfg,
        b:       b,
        kvindex: newTreeIndex(lg),    // B-tree 인덱스 생성

        currentRev:     1,            // 초기 리비전
        compactMainRev: -1,           // 컴팩션 미수행

        fifoSched: schedule.NewFIFOScheduler(lg),
        stopc: make(chan struct{}),
        lg: lg,
    }

    s.hashes = NewHashStorage(lg, s)
    s.ReadView = &readView{s}         // ReadView 임베딩
    s.WriteView = &writeView{s}       // WriteView 임베딩

    // Lessor에 키 삭제 콜백 등록
    if s.le != nil {
        s.le.SetRangeDeleter(func() lease.TxnDelete { return s.Write(traceutil.TODO()) })
    }

    // backend 버킷 초기화
    tx := s.b.BatchTx()
    tx.LockOutsideApply()
    tx.UnsafeCreateBucket(schema.Key)
    schema.UnsafeCreateMetaBucket(tx)
    tx.Unlock()
    s.b.ForceCommit()

    // 기존 데이터 복구
    s.mu.Lock()
    defer s.mu.Unlock()
    if err := s.restore(); err != nil {
        panic("failed to recover store from backend")
    }

    return s
}
```

### 3.2 readView와 writeView

```go
// server/storage/mvcc/kv_view.go:24-56
type readView struct{ kv KV }

func (rv *readView) FirstRev() int64 {
    tr := rv.kv.Read(SharedBufReadTxMode, traceutil.TODO())
    defer tr.End()
    return tr.FirstRev()
}

func (rv *readView) Range(ctx context.Context, key, end []byte, ro RangeOptions) (r *RangeResult, err error) {
    tr := rv.kv.Read(ConcurrentReadTxMode, traceutil.TODO())
    defer tr.End()
    return tr.Range(ctx, key, end, ro)
}

type writeView struct{ kv KV }

func (wv *writeView) Put(key, value []byte, lease lease.LeaseID) (rev int64) {
    tw := wv.kv.Write(traceutil.TODO())
    defer tw.End()
    return tw.Put(key, value, lease)
}
```

`readView`와 `writeView`는 단독 연산(트랜잭션 없이 직접 호출)을 위한 편의 래퍼이다. 내부적으로 트랜잭션을 생성하고, 연산 수행 후, 트랜잭션을 종료한다.

**왜 Range에서 ConcurrentReadTxMode를 쓰는가?**

`readView.Range()`는 Lease의 RangeDeleter 콜백 등 다양한 컨텍스트에서 호출될 수 있다. `ConcurrentReadTxMode`는 읽기 버퍼를 복사하여 진행 중인 쓰기 트랜잭션과 완전히 독립적으로 동작하므로, 교착상태 없이 안전하게 읽기를 수행할 수 있다.

---

## 4. 읽기 트랜잭션 (storeTxnRead)

### 4.1 구조

```go
// server/storage/mvcc/kvstore_txn.go:30-43
type storeTxnRead struct {
    storeTxnCommon
    tx backend.ReadTx
}

type storeTxnCommon struct {
    s  *store
    tx backend.UnsafeReader

    firstRev int64    // 컴팩션 리비전 (이전 리비전은 조회 불가)
    rev      int64    // 현재 리비전 (트랜잭션 시작 시점)

    trace *traceutil.Trace
}
```

### 4.2 Read() - 읽기 트랜잭션 생성

```go
// server/storage/mvcc/kvstore_txn.go:45-63
func (s *store) Read(mode ReadTxMode, trace *traceutil.Trace) TxnRead {
    s.mu.RLock()
    s.revMu.RLock()

    var tx backend.ReadTx
    if mode == ConcurrentReadTxMode {
        tx = s.b.ConcurrentReadTx()    // 버퍼 복사
    } else {
        tx = s.b.ReadTx()              // 공유 버퍼
    }

    tx.RLock()
    firstRev, rev := s.compactMainRev, s.currentRev
    s.revMu.RUnlock()

    return newMetricsTxnRead(&storeTxnRead{
        storeTxnCommon{s, tx, firstRev, rev, trace}, tx,
    })
}
```

락 획득 순서: `mu.RLock()` → `revMu.RLock()` → `tx.RLock()` → `revMu.RUnlock()`

`revMu`를 `tx.RLock()` 후에 해제하는 이유: 리비전 스냅샷을 안전하게 획득한 후에는 `revMu`가 필요 없다. 빨리 해제하여 쓰기 트랜잭션이 `currentRev`를 업데이트할 수 있게 한다.

### 4.3 rangeKeys() - 범위 조회 구현

```go
// server/storage/mvcc/kvstore_txn.go:72-132
func (tr *storeTxnCommon) rangeKeys(ctx context.Context, key, end []byte,
    curRev int64, ro RangeOptions) (*RangeResult, error) {

    rev := ro.Rev
    if rev > curRev {
        return &RangeResult{KVs: nil, Count: -1, Rev: curRev}, ErrFutureRev
    }
    if rev <= 0 {
        rev = curRev    // 리비전 미지정 시 현재 리비전 사용
    }
    if rev < tr.s.compactMainRev {
        return &RangeResult{KVs: nil, Count: -1, Rev: 0}, ErrCompacted
    }

    // 카운트만 요청하는 경우
    if ro.Count {
        total := tr.s.kvindex.CountRevisions(key, end, rev)
        return &RangeResult{KVs: nil, Count: total, Rev: curRev}, nil
    }

    // 1단계: 인메모리 B-tree 인덱스에서 리비전 조회
    revpairs, total := tr.s.kvindex.Revisions(key, end, rev, int(ro.Limit))
    if len(revpairs) == 0 {
        return &RangeResult{KVs: nil, Count: total, Rev: curRev}, nil
    }

    // 2단계: backend(BoltDB)에서 실제 KV 데이터 조회
    limit := int(ro.Limit)
    if limit <= 0 || limit > len(revpairs) {
        limit = len(revpairs)
    }

    kvs := make([]mvccpb.KeyValue, limit)
    revBytes := NewRevBytes()
    for i, revpair := range revpairs[:len(kvs)] {
        select {
        case <-ctx.Done():
            return nil, fmt.Errorf("rangeKeys: context cancelled: %w", ctx.Err())
        default:
        }
        revBytes = RevToBytes(revpair, revBytes)
        _, vs := tr.tx.UnsafeRange(schema.Key, revBytes, nil, 0)
        if len(vs) != 1 {
            tr.s.lg.Fatal("range failed to find revision pair", ...)
        }
        kvs[i].Unmarshal(vs[0])
    }

    return &RangeResult{KVs: kvs, Count: total, Rev: curRev}, nil
}
```

Range 조회의 2단계 구조:

```
┌─────────────────────────────────────────────────────────┐
│ 1단계: B-tree 인덱스 (인메모리)                          │
│                                                          │
│   kvindex.Revisions(key, end, rev, limit)                │
│   → 키 범위에 해당하는 Revision 목록 반환                 │
│   → 이미 정렬되어 있고, limit 적용됨                      │
│                                                          │
│ 2단계: BoltDB Backend (디스크/캐시)                       │
│                                                          │
│   각 Revision을 바이트로 변환 → UnsafeRange()            │
│   → protobuf 직렬화된 KeyValue 조회                       │
│   → Unmarshal하여 결과 배열에 저장                         │
└─────────────────────────────────────────────────────────┘
```

**왜 2단계인가?**

인덱스는 메모리에 있으므로 빠르게 검색할 수 있고, 실제 값 데이터는 BoltDB에 있다. 인덱스에서 필요한 리비전만 추려내고 그것만 BoltDB에서 조회하므로, 전체 키-값 공간을 스캔할 필요가 없다. limit가 있는 경우 인덱스 단계에서 이미 결과를 제한하므로 BoltDB 조회 횟수도 최소화된다.

### 4.4 End() - 읽기 트랜잭션 종료

```go
// server/storage/mvcc/kvstore_txn.go:134-137
func (tr *storeTxnRead) End() {
    tr.tx.RUnlock()       // backend 읽기 락 해제
    tr.s.mu.RUnlock()     // store 락 해제
}
```

---

## 5. 쓰기 트랜잭션 (storeTxnWrite)

### 5.1 구조

```go
// server/storage/mvcc/kvstore_txn.go:139-145
type storeTxnWrite struct {
    storeTxnCommon
    tx backend.BatchTx
    beginRev int64                // 트랜잭션 시작 시의 currentRev
    changes  []mvccpb.KeyValue   // 트랜잭션 내 변경사항 누적
}
```

### 5.2 Write() - 쓰기 트랜잭션 생성

```go
// server/storage/mvcc/kvstore_txn.go:147-158
func (s *store) Write(trace *traceutil.Trace) TxnWrite {
    s.mu.RLock()
    tx := s.b.BatchTx()
    tx.LockInsideApply()
    tw := &storeTxnWrite{
        storeTxnCommon: storeTxnCommon{s, tx, 0, 0, trace},
        tx:             tx,
        beginRev:       s.currentRev,
        changes:        make([]mvccpb.KeyValue, 0, 4),
    }
    return newMetricsTxnWrite(tw)
}
```

`mu.RLock()`을 사용하는 이유: 쓰기 트랜잭션도 `mu`에 대해서는 읽기 락만 사용한다. 쓰기 간의 상호배제는 `backend.BatchTx`의 `LockInsideApply()`가 담당한다. 이를 통해 읽기 트랜잭션과 쓰기 트랜잭션이 동시에 존재할 수 있다.

`changes` 슬라이스의 초기 용량이 4인 이유: 대부분의 트랜잭션은 소수의 키만 수정하므로, 작은 초기 용량으로 시작하여 불필요한 메모리 할당을 방지한다.

### 5.3 Put() 구현 상세

```go
// server/storage/mvcc/kvstore_txn.go:196-264
func (tw *storeTxnWrite) put(key, value []byte, leaseID lease.LeaseID) {
    rev := tw.beginRev + 1
    c := rev                     // CreateRevision (새 키인 경우)
    oldLease := lease.NoLease

    // 기존 키 존재 여부 확인
    _, created, ver, err := tw.s.kvindex.Get(key, rev)
    if err == nil {
        c = created.Main          // 기존 키의 CreateRevision 유지
        oldLease = tw.s.le.GetLease(lease.LeaseItem{Key: string(key)})
    }

    // 리비전 바이트 생성
    ibytes := NewRevBytes()
    idxRev := Revision{Main: rev, Sub: int64(len(tw.changes))}
    ibytes = RevToBytes(idxRev, ibytes)

    ver = ver + 1
    kv := mvccpb.KeyValue{
        Key:            key,
        Value:          value,
        CreateRevision: c,
        ModRevision:    rev,
        Version:        ver,
        Lease:          int64(leaseID),
    }

    d, _ := kv.Marshal()

    // backend에 저장 (키: 리비전 바이트, 값: KV protobuf)
    tw.tx.UnsafeSeqPut(schema.Key, ibytes, d)

    // B-tree 인덱스에 등록
    tw.s.kvindex.Put(key, idxRev)

    // 변경사항 누적
    tw.changes = append(tw.changes, kv)

    // Lease 갱신
    if oldLease != leaseID {
        if oldLease != lease.NoLease {
            tw.s.le.Detach(oldLease, []lease.LeaseItem{{Key: string(key)}})
        }
        if leaseID != lease.NoLease {
            tw.s.le.Attach(leaseID, []lease.LeaseItem{{Key: string(key)}})
        }
    }
}
```

Put 흐름 다이어그램:

```
Put("mykey", "myvalue", leaseID=7)
    │
    ├── 1. rev = currentRev + 1 = 6
    │
    ├── 2. kvindex.Get("mykey", 6)
    │      ├── 성공: CreateRevision 유지, 기존 lease 조회
    │      └── 실패: 새 키 → CreateRevision = 6
    │
    ├── 3. idxRev = {Main:6, Sub:0}
    │      ibytes = "0000000000000006_0000000000000000"
    │
    ├── 4. KeyValue{
    │        Key: "mykey",
    │        Value: "myvalue",
    │        CreateRevision: 3,   (기존 키였다면)
    │        ModRevision: 6,
    │        Version: 4,          (기존 키였다면 ver+1)
    │        Lease: 7,
    │      }
    │
    ├── 5. tx.UnsafeSeqPut(Key, ibytes, marshal(kv))
    │      → BoltDB의 "key" 버킷에 저장
    │
    ├── 6. kvindex.Put("mykey", {6, 0})
    │      → B-tree 인메모리 인덱스 업데이트
    │
    ├── 7. changes = append(changes, kv)
    │      → watch 이벤트 생성용
    │
    └── 8. Lease 관리
           ├── oldLease != leaseID → Detach(old) + Attach(new)
           └── oldLease == leaseID → skip
```

### 5.4 DeleteRange() 구현

```go
// server/storage/mvcc/kvstore_txn.go:170-175
func (tw *storeTxnWrite) DeleteRange(key, end []byte) (int64, int64) {
    if n := tw.deleteRange(key, end); n != 0 || len(tw.changes) > 0 {
        return n, tw.beginRev + 1
    }
    return 0, tw.beginRev
}
```

`deleteRange`의 내부 구현:

```go
// server/storage/mvcc/kvstore_txn.go:266-279
func (tw *storeTxnWrite) deleteRange(key, end []byte) int64 {
    rrev := tw.beginRev
    if len(tw.changes) > 0 {
        rrev++    // 같은 트랜잭션에서 이미 변경이 있으면 다음 rev에서 조회
    }
    keys, _ := tw.s.kvindex.Range(key, end, rrev)
    if len(keys) == 0 {
        return 0
    }
    for _, key := range keys {
        tw.delete(key)
    }
    return int64(len(keys))
}
```

`delete` 메서드:

```go
// server/storage/mvcc/kvstore_txn.go:281-319
func (tw *storeTxnWrite) delete(key []byte) {
    ibytes := NewRevBytes()
    idxRev := newBucketKey(tw.beginRev+1, int64(len(tw.changes)), true)
    ibytes = BucketKeyToBytes(idxRev, ibytes)
    // ↑ tombstone=true → 마지막 바이트에 't' 마커

    kv := mvccpb.KeyValue{Key: key}
    d, _ := kv.Marshal()

    tw.tx.UnsafeSeqPut(schema.Key, ibytes, d)       // tombstone 기록
    tw.s.kvindex.Tombstone(key, idxRev.Revision)     // 인덱스에 tombstone

    tw.changes = append(tw.changes, kv)

    // Lease detach
    item := lease.LeaseItem{Key: string(key)}
    leaseID := tw.s.le.GetLease(item)
    if leaseID != lease.NoLease {
        tw.s.le.Detach(leaseID, []lease.LeaseItem{item})
    }
}
```

삭제는 물리적 삭제가 아니라 tombstone 마커를 추가하는 논리적 삭제이다. 실제 데이터 제거는 컴팩션 시 수행된다.

### 5.5 End() - 쓰기 트랜잭션 종료

```go
// server/storage/mvcc/kvstore_txn.go:182-194
func (tw *storeTxnWrite) End() {
    // 변경이 있는 경우만 리비전 증가
    if len(tw.changes) != 0 {
        tw.s.revMu.Lock()
        tw.s.currentRev++
    }
    tw.tx.Unlock()              // backend 쓰기 락 해제 (→ 버퍼 플러시)
    if len(tw.changes) != 0 {
        tw.s.revMu.Unlock()     // 새 읽기 트랜잭션 허용
    }
    tw.s.mu.RUnlock()
}
```

중요한 순서:
1. `revMu.Lock()` 획득 → 새 읽기 트랜잭션 차단
2. `currentRev++` → 리비전 증가
3. `tx.Unlock()` → backend 쓰기 데이터 가시화
4. `revMu.Unlock()` → 새 읽기 트랜잭션은 증가된 rev로 시작

이 순서가 보장하는 것: `currentRev == N+1`을 본 읽기 트랜잭션은 반드시 리비전 N+1의 데이터를 볼 수 있다 (`tx.Unlock()`이 먼저 수행되었으므로).

### 5.6 쓰기 트랜잭션 내의 Range

```go
// server/storage/mvcc/kvstore_txn.go:162-168
func (tw *storeTxnWrite) Range(ctx context.Context, key, end []byte, ro RangeOptions) (*RangeResult, error) {
    rev := tw.beginRev
    if len(tw.changes) > 0 {
        rev++    // 현재 트랜잭션의 변경사항 포함
    }
    return tw.rangeKeys(ctx, key, end, rev, ro)
}
```

쓰기 트랜잭션 내에서의 읽기는 "자신이 방금 쓴 데이터"를 볼 수 있다. `changes`가 비어있지 않으면 `rev`를 1 증가시켜 현재 트랜잭션의 변경사항도 조회 범위에 포함시킨다.

---

## 6. Compact() - 컴팩션

### 6.1 전체 흐름

```
Compact(rev=100)
    │
    ├── 1. s.mu.Lock()
    ├── 2. checkPrevCompactionCompleted()
    │      → 이전 컴팩션이 완료되었는지 확인
    │
    ├── 3. updateCompactRev(100)
    │      ├── s.revMu.Lock()
    │      ├── rev <= compactMainRev → ErrCompacted
    │      ├── rev > currentRev → ErrFutureRev
    │      ├── compactMainRev = 100
    │      ├── SetScheduledCompact(tx, 100)  → DB에 기록
    │      ├── s.b.ForceCommit()             → 영속화 보장
    │      └── s.revMu.Unlock()
    │
    ├── 4. s.mu.Unlock()
    │
    └── 5. compact(trace, 100, prevRev, prevCompleted)
           └── fifoSched.Schedule(job)  → 비동기 실행
               │
               └── scheduleCompaction(100, prevRev)
                   ├── kvindex.Compact(100)  → 인덱스 정리
                   └── backend 배치 삭제      → DB 정리
```

### 6.2 scheduleCompaction() 상세

```go
// server/storage/mvcc/kvstore_compaction.go:28-100
func (s *store) scheduleCompaction(compactMainRev, prevCompactRev int64) (KeyValueHash, error) {
    // 1단계: 인메모리 인덱스 컴팩션
    keep := s.kvindex.Compact(compactMainRev)
    // keep = 보존해야 할 리비전 맵

    // 2단계: backend(BoltDB) 배치 삭제
    end := make([]byte, 8)
    binary.BigEndian.PutUint64(end, uint64(compactMainRev+1))

    batchNum := s.cfg.CompactionBatchLimit  // 기본 1000
    h := newKVHasher(prevCompactRev, compactMainRev, keep)
    last := make([]byte, 8+1+8)

    for {
        tx := s.b.BatchTx()
        tx.LockOutsideApply()
        keys, values := tx.UnsafeRange(schema.Key, last, end, int64(batchNum))

        for i := range keys {
            rev := BytesToRev(keys[i])
            if _, ok := keep[rev]; !ok {
                tx.UnsafeDelete(schema.Key, keys[i])  // 불필요한 리비전 삭제
            }
            h.WriteKeyValue(keys[i], values[i])         // 해시 계산
        }

        if len(keys) < batchNum {
            UnsafeSetFinishedCompact(tx, compactMainRev)
            tx.Unlock()
            hash := h.Hash()
            return hash, nil
        }

        tx.Unlock()
        last = RevToBytes(Revision{Main: rev.Main, Sub: rev.Sub + 1}, last)
        s.b.ForceCommit()

        // 배치 간 슬립으로 다른 작업에 양보
        select {
        case <-time.After(s.cfg.CompactionSleepInterval):  // 기본 10ms
        case <-s.stopc:
            return KeyValueHash{}, fmt.Errorf("interrupted due to stop signal")
        }
    }
}
```

배치 컴팩션의 설계 의도:

```
┌──────────────────────────────────────────────────────────┐
│  왜 배치로 나누어 삭제하는가?                             │
│                                                           │
│  1. BoltDB 트랜잭션 크기 제한                             │
│     - 한 번에 수만 개의 키를 삭제하면 쓰기 버퍼가 거대해짐│
│     - 메모리 사용량 급증 및 GC 압박                       │
│                                                           │
│  2. 쓰기 지연 방지                                        │
│     - BatchTx.Lock()은 다른 쓰기를 블로킹                 │
│     - 배치 사이에 Lock을 풀어 다른 쓰기가 진행되게 함     │
│                                                           │
│  3. 슬립으로 I/O 양보                                     │
│     - CompactionSleepInterval(10ms)로 다른 I/O에 양보     │
│     - 컴팩션이 일반 요청 처리를 방해하지 않음              │
│                                                           │
│  4. 해시 계산                                              │
│     - 삭제하면서 동시에 KV 해시를 계산                     │
│     - 클러스터 노드 간 데이터 일관성 검증에 사용           │
└──────────────────────────────────────────────────────────┘
```

### 6.3 compactBarrier()

```go
// server/storage/mvcc/kvstore.go:136-153
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

`compactBarrier`는 컴팩션이 완료(또는 실패)된 후 대기자들에게 알리는 메커니즘이다. 컨텍스트가 취소된 경우(스케줄러가 셧다운 중), 재스케줄링하여 교착상태를 방지한다 (PR #11817 참조).

---

## 7. restore() - 서버 부팅 시 인덱스 복구

### 7.1 복구 흐름

```go
// server/storage/mvcc/kvstore.go:316-425
func (s *store) restore() error {
    s.setupMetricsReporter()

    min, max := NewRevBytes(), NewRevBytes()
    min = RevToBytes(Revision{Main: 1}, min)
    max = RevToBytes(Revision{Main: math.MaxInt64, Sub: math.MaxInt64}, max)

    keyToLease := make(map[string]lease.LeaseID)

    // 1. 마지막 완료된 컴팩션 리비전 복구
    tx := s.b.ReadTx()
    tx.RLock()
    finishedCompact, found := UnsafeReadFinishedCompact(tx)
    if found {
        s.compactMainRev = finishedCompact
    }

    // 2. 스케줄된 (미완료) 컴팩션 리비전 확인
    scheduledCompact, _ := UnsafeReadScheduledCompact(tx)

    // 3. B-tree 인덱스 복구 (비동기)
    rkvc, revc := restoreIntoIndex(s.lg, s.kvindex)
    for {
        keys, vals := tx.UnsafeRange(schema.Key, min, max, int64(restoreChunkKeys))
        if len(keys) == 0 {
            break
        }
        restoreChunk(s.lg, rkvc, keys, vals, keyToLease)
        if len(keys) < restoreChunkKeys {
            break
        }
        newMin := BytesToRev(keys[len(keys)-1][:revBytesLen])
        newMin.Sub++
        min = RevToBytes(newMin, min)
    }
    close(rkvc)

    // 4. currentRev 설정
    s.currentRev = <-revc
    if s.currentRev < s.compactMainRev {
        s.currentRev = s.compactMainRev
    }
    if s.currentRev < scheduledCompact {
        s.currentRev = scheduledCompact
    }

    // 5. Lease 복구
    for key, lid := range keyToLease {
        s.le.Attach(lid, []lease.LeaseItem{{Key: key}})
    }
    tx.RUnlock()

    // 6. 미완료 컴팩션 재개
    if scheduledCompact != 0 && scheduledCompact > s.compactMainRev {
        s.compactLockfree(scheduledCompact)
    }

    return nil
}
```

### 7.2 restoreIntoIndex() - 비동기 인덱스 구축

```go
// server/storage/mvcc/kvstore.go:433-490
func restoreIntoIndex(lg *zap.Logger, idx index) (chan<- revKeyValue, <-chan int64) {
    rkvc, revc := make(chan revKeyValue, restoreChunkKeys), make(chan int64, 1)
    go func() {
        currentRev := int64(1)
        defer func() { revc <- currentRev }()

        kiCache := make(map[string]*keyIndex, restoreChunkKeys)
        for rkv := range rkvc {
            ki, ok := kiCache[rkv.kstr]

            // 캐시 퍼지 (메모리 절약)
            if !ok && len(kiCache) >= restoreChunkKeys {
                i := 10
                for k := range kiCache {
                    delete(kiCache, k)
                    if i--; i == 0 { break }
                }
            }

            // 캐시 미스 시 인덱스에서 조회
            if !ok {
                ki = &keyIndex{key: rkv.kv.Key}
                if idxKey := idx.KeyIndex(ki); idxKey != nil {
                    kiCache[rkv.kstr], ki = idxKey, idxKey
                    ok = true
                }
            }

            rev := BytesToRev(rkv.key)
            currentRev = rev.Main

            if ok {
                if isTombstone(rkv.key) {
                    ki.tombstone(lg, rev.Main, rev.Sub)
                } else {
                    ki.put(lg, rev.Main, rev.Sub)
                }
            } else {
                if isTombstone(rkv.key) {
                    ki.restoreTombstone(lg, rev.Main, rev.Sub)
                } else {
                    ki.restore(lg, Revision{Main: rkv.kv.CreateRevision},
                        rev, rkv.kv.Version)
                }
                idx.Insert(ki)
                kiCache[rkv.kstr] = ki
            }
        }
    }()
    return rkvc, revc
}
```

복구 아키텍처:

```
BoltDB (디스크)
    │
    ├── UnsafeRange(Key, min, max, 10000)
    │   → keys[], vals[] (청크)
    │
    ├── restoreChunk()
    │   → revKeyValue로 변환
    │   → rkvc 채널로 전송
    │
    └── (반복)
         │
         ▼
restoreIntoIndex() 고루틴
    │
    ├── rkvc에서 수신
    ├── kiCache로 keyIndex 캐싱
    │   ├── 캐시 히트 → ki.put() 또는 ki.tombstone()
    │   └── 캐시 미스 → idx.KeyIndex() 조회
    │       ├── 인덱스에 있음 → 캐시에 추가
    │       └── 인덱스에 없음 → ki.restore() + idx.Insert()
    └── 완료 시 revc <- currentRev
```

**왜 청크 기반인가?**

`restoreChunkKeys = 10000`개씩 읽는 이유:
- 수백만 개의 키를 한 번에 메모리에 로드하면 OOM 위험
- 청크 단위로 읽어 메모리 사용량을 제한
- `rkvc` 채널의 버퍼 크기도 `restoreChunkKeys`로 설정하여 백프레셔 제공

---

## 8. hash와 hashByRev - 무결성 검증

### 8.1 hash()

```go
// server/storage/mvcc/kvstore.go:155-164
func (s *store) hash() (hash uint32, revision int64, err error) {
    start := time.Now()
    s.b.ForceCommit()
    h, err := s.b.Hash(schema.DefaultIgnores)
    hashSec.Observe(time.Since(start).Seconds())
    return h, s.currentRev, err
}
```

전체 DB의 해시를 계산한다. 클러스터 노드 간 전체 데이터 일관성을 검증하는 데 사용된다. `ForceCommit()`으로 버퍼를 플러시한 후 해시를 계산하여 정확성을 보장한다.

### 8.2 hashByRev()

```go
// server/storage/mvcc/kvstore.go:166-194
func (s *store) hashByRev(rev int64) (hash KeyValueHash, currentRev int64, err error) {
    s.mu.RLock()
    s.revMu.RLock()
    compactRev, currentRev = s.compactMainRev, s.currentRev
    s.revMu.RUnlock()

    if rev > 0 && rev < compactRev {
        return KeyValueHash{}, 0, ErrCompacted
    } else if rev > 0 && rev > currentRev {
        return KeyValueHash{}, currentRev, ErrFutureRev
    }
    if rev == 0 {
        rev = currentRev
    }

    keep := s.kvindex.Keep(rev)   // 보존할 리비전 맵

    tx := s.b.ReadTx()
    tx.RLock()
    defer tx.RUnlock()
    s.mu.RUnlock()

    hash, err = unsafeHashByRev(tx, compactRev, rev, keep)
    return hash, currentRev, err
}
```

`hashByRev`는 특정 리비전까지의 데이터 해시를 계산한다. `Keep(rev)`을 사용하여 컴팩션 후에도 존재해야 할 리비전만 해시에 포함시킨다. 이를 통해 서로 다른 시점에 컴팩션을 수행한 노드들도 동일한 해시 값을 생성할 수 있다.

---

## 9. Restore() - 전체 스토어 복구

```go
// server/storage/mvcc/kvstore.go:291-313
func (s *store) Restore(b backend.Backend) error {
    s.mu.Lock()
    defer s.mu.Unlock()

    close(s.stopc)              // 기존 컴팩션 작업 중단
    s.fifoSched.Stop()

    s.b = b                     // 새 backend으로 교체
    s.kvindex = newTreeIndex(s.lg)  // 인덱스 재생성

    s.revMu.Lock()
    s.currentRev = 1
    s.compactMainRev = -1
    s.revMu.Unlock()

    s.fifoSched = schedule.NewFIFOScheduler(s.lg)
    s.stopc = make(chan struct{})

    return s.restore()          // 새 backend에서 인덱스 복구
}
```

`Restore()`는 스냅샷 복구 시 호출된다. 인메모리 상태를 완전히 초기화하고 새 backend에서 인덱스를 재구축한다.

---

## 10. StoreConfig

```go
// server/storage/mvcc/kvstore.go:47-50
type StoreConfig struct {
    CompactionBatchLimit    int            // 배치당 삭제할 최대 키 수 (기본 1000)
    CompactionSleepInterval time.Duration  // 배치 간 슬립 (기본 10ms)
}
```

이 설정으로 컴팩션의 공격성을 조절할 수 있다:
- `CompactionBatchLimit`을 늘리면 컴팩션이 빨라지지만 BatchTx 락 보유 시간이 길어짐
- `CompactionSleepInterval`을 줄이면 컴팩션이 빨라지지만 일반 I/O에 영향

---

## 11. 메트릭과 관찰 가능성

### 11.1 setupMetricsReporter()

```go
// server/storage/mvcc/kvstore.go:516-541
func (s *store) setupMetricsReporter() {
    reportDbTotalSizeInBytes = func() float64 { return float64(b.Size()) }
    reportDbTotalSizeInUseInBytes = func() float64 { return float64(b.SizeInUse()) }
    reportDbOpenReadTxN = func() float64 { return float64(b.OpenReadTxN()) }
    reportCurrentRev = func() float64 {
        s.revMu.RLock()
        defer s.revMu.RUnlock()
        return float64(s.currentRev)
    }
    reportCompactRev = func() float64 {
        s.revMu.RLock()
        defer s.revMu.RUnlock()
        return float64(s.compactMainRev)
    }
}
```

Prometheus 메트릭으로 노출되는 항목:

| 메트릭 | 설명 |
|--------|------|
| `etcd_mvcc_db_total_size_in_bytes` | 전체 DB 파일 크기 |
| `etcd_mvcc_db_total_size_in_use_in_bytes` | 실제 사용 중인 DB 크기 |
| `etcd_mvcc_db_open_read_transactions_total` | 열린 읽기 트랜잭션 수 |
| `etcd_debugging_mvcc_current_revision` | 현재 리비전 |
| `etcd_debugging_mvcc_compact_revision` | 컴팩션 리비전 |
| `etcd_debugging_mvcc_keys_total` | 전체 키 수 |
| `etcd_mvcc_hash_duration_seconds` | 해시 계산 소요 시간 |
| `etcd_mvcc_hash_rev_duration_seconds` | 리비전별 해시 계산 소요 시간 |

---

## 12. 에러 처리

```go
// server/storage/mvcc/kvstore.go:37-39
var (
    ErrCompacted = errors.New("mvcc: required revision has been compacted")
    ErrFutureRev = errors.New("mvcc: required revision is a future revision")
)
```

| 에러 | 발생 상황 | 의미 |
|------|----------|------|
| `ErrCompacted` | Range/hashByRev에서 요청 rev < compactMainRev | 이미 컴팩션된 리비전 |
| `ErrFutureRev` | Range/hashByRev에서 요청 rev > currentRev | 아직 존재하지 않는 리비전 |

---

## 13. 정리: MVCC 저장소 아키텍처

```
┌─────────────────────────────────────────────────────────────┐
│                    KV 인터페이스                              │
│  ┌──────────┐  ┌───────────┐  ┌──────────────────────────┐  │
│  │ readView │  │ writeView │  │ Read()/Write()/Compact() │  │
│  │ (단독)    │  │ (단독)    │  │ (트랜잭션)                │  │
│  └────┬─────┘  └─────┬─────┘  └──────────┬───────────────┘  │
│       │              │                    │                   │
│       │   내부적으로 트랜잭션 생성         │                   │
│       └──────────────┼────────────────────┘                   │
│                      │                                        │
│  ┌───────────────────┴────────────────────────────────────┐  │
│  │                    store 구조체                          │  │
│  │                                                         │  │
│  │  mu (RWMutex)      │  revMu (RWMutex)                   │  │
│  │  ├── 트랜잭션 보호  │  ├── currentRev 보호                │  │
│  │  └── Restore 보호   │  └── compactMainRev 보호            │  │
│  │                     │                                    │  │
│  │  ┌────────────┐    ┌─────────────────┐                  │  │
│  │  │  kvindex   │    │  backend.Backend │                  │  │
│  │  │  (B-tree)  │    │  (BoltDB)        │                  │  │
│  │  │            │    │                   │                  │  │
│  │  │  키 → Rev  │    │  Rev → KeyValue  │                  │  │
│  │  │  인메모리   │    │  디스크 + 캐시    │                  │  │
│  │  └────────────┘    └─────────────────┘                  │  │
│  │                                                         │  │
│  │  fifoSched ← 컴팩션 비동기 실행                          │  │
│  │  hashes ← 무결성 검증용 해시 저장                         │  │
│  └─────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────┘

데이터 흐름:
  Put:    kvindex.Put() + backend.UnsafeSeqPut()
  Range:  kvindex.Revisions() → backend.UnsafeRange() → Unmarshal
  Delete: kvindex.Tombstone() + backend.UnsafeSeqPut(tombstone)
  Compact: kvindex.Compact() + backend.UnsafeDelete(non-keep)
```

핵심 설계 원칙:
1. **MVCC 리비전 기반 저장**: 모든 변경은 새 리비전 생성, 이전 데이터 보존
2. **2단계 검색**: 인메모리 인덱스(빠른 키 검색) + 디스크 backend(실제 데이터)
3. **2단계 락**: mu(트랜잭션 보호) + revMu(리비전 보호)로 세밀한 동시성 제어
4. **비동기 컴팩션**: FIFO 스케줄러로 배치 삭제, 일반 요청에 미치는 영향 최소화
5. **해시 기반 무결성**: 컴팩션 시 해시 계산으로 클러스터 노드 간 데이터 일관성 검증
