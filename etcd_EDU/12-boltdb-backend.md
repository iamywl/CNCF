# 12. BoltDB 백엔드 Deep Dive

## 개요

etcd의 모든 키-값 데이터는 **BoltDB**(bbolt)를 통해 디스크에 영구 저장된다. etcd의 `backend` 패키지는 BoltDB 위에 **배치 트랜잭션**, **읽기 버퍼링**, **동시 읽기 트랜잭션** 등의 계층을 추가하여 성능과 동시성을 최적화한다.

```
etcd 스토리지 계층:

  ┌──────────────────────────────────┐
  │         MVCC 계층                │
  │   (리비전 관리, Watch, 인덱스)    │
  ├──────────────────────────────────┤
  │         Backend 계층             │  ← 이 문서의 범위
  │   (배치 TX, 읽기 버퍼, 동시성)    │
  ├──────────────────────────────────┤
  │         BoltDB (bbolt)           │
  │   (B+ 트리, 페이지 관리, mmap)    │
  ├──────────────────────────────────┤
  │         파일 시스템               │
  └──────────────────────────────────┘
```

소스 경로: `server/storage/backend/`

---

## 1. Backend 인터페이스

### 1.1 인터페이스 정의

```
경로: server/storage/backend/backend.go (49~75행)
```

```go
type Backend interface {
    ReadTx() ReadTx
    BatchTx() BatchTx
    ConcurrentReadTx() ReadTx

    Snapshot() Snapshot
    Hash(ignores func(bucketName, keyName []byte) bool) (uint32, error)
    Size() int64
    SizeInUse() int64
    OpenReadTxN() int64
    Defrag() error
    ForceCommit()
    Close() error

    SetTxPostLockInsideApplyHook(func())
}
```

| 메서드 | 역할 |
|--------|------|
| `ReadTx()` | 블록킹 읽기 트랜잭션 반환 |
| `BatchTx()` | 배치 쓰기 트랜잭션 반환 |
| `ConcurrentReadTx()` | 비블록킹 읽기 트랜잭션 반환 |
| `Snapshot()` | 현재 DB의 일관된 스냅샷 |
| `Hash()` | DB 전체의 해시값 계산 (일관성 검증) |
| `Defrag()` | DB 조각 모음 |
| `ForceCommit()` | 현재 배치를 즉시 커밋 |

### 1.2 Snapshot 인터페이스

```go
type Snapshot interface {
    Size() int64
    WriteTo(w io.Writer) (n int64, err error)
    Close() error
}
```

---

## 2. backend 구조체

### 2.1 구조체 정의

```
경로: server/storage/backend/backend.go (92~131행)
```

```go
type backend struct {
    size      int64    // 할당된 전체 크기 (atomic)
    sizeInUse int64    // 실제 사용 크기 (atomic)
    commits   int64    // 총 커밋 횟수 (atomic)
    openReadTxN int64  // 현재 열린 읽기 TX 수 (atomic)
    mlock     bool     // mlock 사용 여부

    mu    sync.RWMutex
    bopts *bolt.Options
    db    *bolt.DB

    batchInterval time.Duration  // 배치 커밋 주기 (기본 100ms)
    batchLimit    int            // 배치 커밋 임계값 (기본 10000)
    batchTx       *batchTxBuffered

    readTx *readTx
    txReadBufferCache txReadBufferCache

    stopc chan struct{}
    donec chan struct{}

    hooks Hooks
    txPostLockInsideApplyHook func()

    lg *zap.Logger
}
```

### 2.2 BackendConfig

```
경로: server/storage/backend/backend.go (133~156행)
```

```go
type BackendConfig struct {
    Path              string
    BatchInterval     time.Duration      // 기본 100ms
    BatchLimit        int                // 기본 10000
    BackendFreelistType bolt.FreelistType
    MmapSize          uint64             // 기본 10GB
    Logger            *zap.Logger
    UnsafeNoFsync     bool
    Mlock             bool
    Timeout           time.Duration
    Hooks             Hooks
}
```

**기본값:**

```go
var (
    defaultBatchLimit    = 10000
    defaultBatchInterval = 100 * time.Millisecond
    defragLimit          = 10000
    InitialMmapSize      = uint64(10 * 1024 * 1024 * 1024)  // 10GB
)
```

### 2.3 Backend 초기화

```
경로: server/storage/backend/backend.go (195~257행)
```

```go
func newBackend(bcfg BackendConfig) *backend {
    bopts := &bolt.Options{}
    bopts.InitialMmapSize = bcfg.mmapSize()
    bopts.FreelistType = bcfg.BackendFreelistType
    bopts.NoSync = bcfg.UnsafeNoFsync
    bopts.Mlock = bcfg.Mlock

    db, err := bolt.Open(bcfg.Path, 0o600, bopts)

    b := &backend{
        db:            db,
        batchInterval: bcfg.BatchInterval,
        batchLimit:    bcfg.BatchLimit,

        readTx: &readTx{
            baseReadTx: baseReadTx{
                buf: txReadBuffer{
                    txBuffer:   txBuffer{make(map[BucketID]*bucketBuffer)},
                    bufVersion: 0,
                },
                buckets: make(map[BucketID]*bolt.Bucket),
                txWg:    new(sync.WaitGroup),
                txMu:    new(sync.RWMutex),
            },
        },
        txReadBufferCache: txReadBufferCache{},
        // ...
    }

    b.batchTx = newBatchTxBuffered(b)
    b.hooks = bcfg.Hooks

    go b.run()  // 주기적 커밋 고루틴
    return b
}
```

**초기화 순서:**

```
1. BoltDB 열기 (bolt.Open)
2. readTx 초기화 (읽기 버퍼, 버킷 캐시)
3. batchTxBuffered 생성 (쓰기 버퍼)
4. 주기적 커밋 고루틴 시작 (b.run())
```

---

## 3. 주기적 배치 커밋 (b.run)

```
경로: server/storage/backend/backend.go (441~457행)
```

```go
func (b *backend) run() {
    defer close(b.donec)
    t := time.NewTimer(b.batchInterval)  // 100ms
    defer t.Stop()
    for {
        select {
        case <-t.C:
        case <-b.stopc:
            b.batchTx.CommitAndStop()
            return
        }
        if b.batchTx.safePending() != 0 {
            b.batchTx.Commit()
        }
        t.Reset(b.batchInterval)
    }
}
```

**배치 커밋 전략:**

```
┌─────────────────────────────────────────────────────────┐
│                    배치 커밋 트리거                        │
│                                                           │
│  1. 시간 기반 (batchInterval = 100ms)                    │
│     └── b.run() 고루틴이 100ms마다 확인                   │
│         └── pending > 0이면 Commit()                     │
│                                                           │
│  2. 수량 기반 (batchLimit = 10000)                        │
│     └── batchTx.Unlock() 시 확인                         │
│         └── pending >= 10000이면 commit(false)            │
│                                                           │
│  3. 삭제 연산 즉시 커밋                                    │
│     └── batchTxBuffered.Unlock() 시 확인                  │
│         └── pendingDeleteOperations > 0이면 commit(false) │
│                                                           │
│  4. 강제 커밋 (ForceCommit)                               │
│     └── 스냅샷 전, 외부 요청 시                            │
└─────────────────────────────────────────────────────────┘
```

**왜 삭제 연산은 즉시 커밋하는가?**

```
경로: server/storage/backend/batch_tx.go (308~340행) 주석 참조
```

Put 연산은 쓰기 버퍼(txWriteBuffer)에 저장되므로 커밋 전에도 읽기에서 보인다. 하지만 Delete 연산은 버퍼에서 제거만 하므로, 커밋 전까지 BoltDB에는 여전히 데이터가 남아 있다. 이로 인해 선형성(linearizability)이 깨질 수 있어 삭제 시 즉시 커밋한다.

```
Put의 경우:
  Put("key", "value") → txWriteBuffer에 저장
  다음 읽기 → txReadBuffer에서 "key"="value" 보임 ✓ (커밋 전에도)

Delete의 경우:
  Delete("key") → BoltDB에는 여전히 "key" 존재
  다음 읽기 → txReadBuffer에 없지만 BoltDB에서 보임 ✗ (불일치!)
  → 즉시 커밋으로 해결
```

---

## 4. batchTx: 쓰기 트랜잭션

### 4.1 구조체

```
경로: server/storage/backend/batch_tx.go (73~79행)
```

```go
type batchTx struct {
    sync.Mutex
    tx      *bolt.Tx
    backend *backend
    pending int       // 커밋 대기 중인 연산 수
}
```

### 4.2 BatchTx 인터페이스

```
경로: server/storage/backend/batch_tx.go (48~58행)
```

```go
type BatchTx interface {
    Lock()
    Unlock()
    Commit()
    CommitAndStop()
    LockInsideApply()
    LockOutsideApply()
    UnsafeReadWriter
}
```

### 4.3 Lock/Unlock 전략

```
경로: server/storage/backend/batch_tx.go (82~114행)
```

```go
// Lock()은 단위 테스트에서만 호출되어야 함
func (t *batchTx) Lock() {
    ValidateCalledInsideUnittest(t.backend.lg)
    t.lock()
}

// LockInsideApply: Raft Apply 경로에서 호출
func (t *batchTx) LockInsideApply() {
    t.lock()
    if t.backend.txPostLockInsideApplyHook != nil {
        ValidateCalledInsideApply(t.backend.lg)
        t.backend.txPostLockInsideApplyHook()
    }
}

// LockOutsideApply: Apply 경로 바깥에서 호출
func (t *batchTx) LockOutsideApply() {
    ValidateCalledOutSideApply(t.backend.lg)
    t.lock()
}

// Unlock 시 batchLimit 체크
func (t *batchTx) Unlock() {
    if t.pending >= t.backend.batchLimit {
        t.commit(false)
    }
    t.Mutex.Unlock()
}
```

**왜 Lock을 두 가지로 분리하는가?**

etcd는 Raft Apply 경로(요청 처리)와 그 외 경로(리더 선출, 멤버십 변경 등)를 구분한다. `txPostLockInsideApplyHook`은 Apply 경로에서만 실행되는 후크로, consistent index 업데이트 등에 사용된다.

### 4.4 UnsafePut / UnsafeSeqPut

```
경로: server/storage/backend/batch_tx.go (139~171행)
```

```go
func (t *batchTx) unsafePut(bucketType Bucket, key []byte, value []byte, seq bool) {
    bucket := t.tx.Bucket(bucketType.Name())
    if seq {
        // 순차 쓰기: fill percent를 90%로 높여 페이지 분할 지연
        bucket.FillPercent = 0.9
    }
    bucket.Put(key, value)
    t.pending++
}
```

**FillPercent 0.9의 의미:**

BoltDB는 B+ 트리를 사용한다. 노드(페이지)가 가득 차면 분할(split)된다. 기본 FillPercent(50%)에서는 노드가 반만 채워지면 분할하지만, 0.9로 설정하면 90%까지 채울 수 있다.

```
기본값 (50%):                     순차 쓰기 (90%):
┌──────┬──────┐                   ┌──────────────────┐
│ ████ │      │ (50% 채움)        │ ████████████████ │ (90% 채움)
└──────┴──────┘                   └──────────────────┘
  빈 공간 50%                        빈 공간 10%

순차 쓰기(리비전 키)는 항상 증가하므로
기존 노드에 삽입할 일이 없다 → 높은 fill%가 유리
→ 공간 효율 증가, 페이지 분할 감소
```

### 4.5 commit(): 실제 커밋

```
경로: server/storage/backend/batch_tx.go (261~288행)
```

```go
func (t *batchTx) commit(stop bool) {
    if t.tx != nil {
        if t.pending == 0 && !stop {
            return
        }

        start := time.Now()
        err := t.tx.Commit()

        // 메트릭 기록
        rebalanceSec.Observe(t.tx.Stats().RebalanceTime.Seconds())
        spillSec.Observe(t.tx.Stats().SpillTime.Seconds())
        writeSec.Observe(t.tx.Stats().WriteTime.Seconds())
        commitSec.Observe(time.Since(start).Seconds())
        atomic.AddInt64(&t.backend.commits, 1)

        t.pending = 0
    }
    if !stop {
        t.tx = t.backend.begin(true)
    }
}
```

**커밋 과정에서 측정하는 메트릭:**

| 메트릭 | 의미 |
|--------|------|
| `rebalanceSec` | B+ 트리 리밸런싱 시간 |
| `spillSec` | 더티 페이지를 디스크로 흘리는 시간 |
| `writeSec` | 실제 디스크 쓰기 시간 |
| `commitSec` | 전체 커밋 시간 |

---

## 5. batchTxBuffered: 버퍼링된 쓰기 트랜잭션

### 5.1 구조체

```
경로: server/storage/backend/batch_tx.go (290~294행)
```

```go
type batchTxBuffered struct {
    batchTx
    buf                     txWriteBuffer
    pendingDeleteOperations int
}
```

`batchTxBuffered`는 `batchTx`를 임베딩하면서 **쓰기 버퍼**를 추가한다. 이 버퍼가 있어야 커밋 전에도 최신 데이터를 읽을 수 있다.

### 5.2 Unlock(): writeback과 즉시 커밋

```
경로: server/storage/backend/batch_tx.go (308~340행)
```

```go
func (t *batchTxBuffered) Unlock() {
    if t.pending != 0 {
        t.backend.readTx.Lock()
        t.buf.writeback(&t.backend.readTx.buf)
        t.backend.readTx.Unlock()

        if t.pending >= t.backend.batchLimit || t.pendingDeleteOperations > 0 {
            t.commit(false)
        }
    }
    t.batchTx.Unlock()
}
```

**writeback 흐름:**

```
쓰기 연산 시:
  1. BoltDB TX에 기록 (batchTx.unsafePut)
  2. txWriteBuffer에 기록 (buf.put)

Unlock 시:
  3. txWriteBuffer → txReadBuffer로 writeback
     (readTx 잠금 하에 수행)

읽기 연산 시:
  4. txReadBuffer 먼저 확인 → BoltDB TX에서 확인
     (최신 데이터가 버퍼에 있으므로 커밋 전에도 보임)
```

### 5.3 unsafeCommit(): 전체 커밋 과정

```
경로: server/storage/backend/batch_tx.go (361~386행)
```

```go
func (t *batchTxBuffered) unsafeCommit(stop bool) {
    // Pre-commit hook (consistent index 등 업데이트)
    if t.backend.hooks != nil {
        t.backend.hooks.OnPreCommitUnsafe(t)
    }

    // 현재 읽기 TX가 완료될 때까지 기다린 후 롤백
    if t.backend.readTx.tx != nil {
        go func(tx *bolt.Tx, wg *sync.WaitGroup) {
            wg.Wait()
            tx.Rollback()
        }(t.backend.readTx.tx, t.backend.readTx.txWg)
        t.backend.readTx.reset()
    }

    // BoltDB TX 커밋
    t.batchTx.commit(stop)
    t.pendingDeleteOperations = 0

    // 새 읽기 TX 시작
    if !stop {
        t.backend.readTx.tx = t.backend.begin(false)
    }
}
```

**읽기 TX 교체 과정:**

```
커밋 전:
  readTx.tx = TX_A (이전 배치의 읽기 TX)
  일부 ConcurrentReadTx들이 TX_A를 사용 중

커밋 중:
  1. go func() { wg.Wait(); TX_A.Rollback() }
     → TX_A 사용하는 모든 ConcurrentReadTx 완료 대기 후 롤백
  2. readTx.reset() → 버퍼/버킷 캐시 초기화
  3. batchTx.commit(false) → 쓰기 TX 커밋 + 새 쓰기 TX 시작
  4. readTx.tx = TX_B (새 배치의 읽기 TX)

커밋 후:
  readTx.tx = TX_B
  새 ConcurrentReadTx들은 TX_B 사용
  TX_A는 마지막 사용자 완료 후 롤백됨
```

### 5.4 UnsafePut/UnsafeDelete 오버라이드

```
경로: server/storage/backend/batch_tx.go (388~406행)
```

```go
func (t *batchTxBuffered) UnsafePut(bucket Bucket, key []byte, value []byte) {
    t.batchTx.UnsafePut(bucket, key, value)  // BoltDB에 기록
    t.buf.put(bucket, key, value)             // 쓰기 버퍼에도 기록
}

func (t *batchTxBuffered) UnsafeSeqPut(bucket Bucket, key []byte, value []byte) {
    t.batchTx.UnsafeSeqPut(bucket, key, value)
    t.buf.putSeq(bucket, key, value)
}

func (t *batchTxBuffered) UnsafeDelete(bucketType Bucket, key []byte) {
    t.batchTx.UnsafeDelete(bucketType, key)
    t.pendingDeleteOperations++  // 삭제 카운트만 증가
}
```

**왜 Delete는 버퍼에 기록하지 않는가?**

삭제 시 버퍼에서 특정 키를 찾아 제거하는 것은 복잡하고 비효율적이다. 대신 `pendingDeleteOperations`를 추적하여 Unlock 시 즉시 커밋함으로써 BoltDB와 버퍼의 일관성을 유지한다.

---

## 6. ReadTx: 읽기 트랜잭션

### 6.1 ReadTx 인터페이스와 baseReadTx

```
경로: server/storage/backend/read_tx.go (28~52행)
```

```go
type ReadTx interface {
    RLock()
    RUnlock()
    UnsafeReader
}

type baseReadTx struct {
    mu      sync.RWMutex
    buf     txReadBuffer           // 쓰기 버퍼에서 writeback된 데이터
    txMu    *sync.RWMutex          // BoltDB TX 접근 보호
    tx      *bolt.Tx               // BoltDB 읽기 TX
    buckets map[BucketID]*bolt.Bucket  // 버킷 캐시
    txWg    *sync.WaitGroup        // TX 수명 추적
}
```

### 6.2 UnsafeRange: 버퍼 + BoltDB 병합 읽기

```
경로: server/storage/backend/read_tx.go (78~122행)
```

```go
func (baseReadTx *baseReadTx) UnsafeRange(bucketType Bucket, key, endKey []byte, limit int64) ([][]byte, [][]byte) {
    if endKey == nil {
        limit = 1  // 단일 키는 중복 방지
    }
    if limit > 1 && !bucketType.IsSafeRangeBucket() {
        panic("do not use unsafeRange on non-keys bucket")
    }

    // 1. 먼저 버퍼에서 조회
    keys, vals := baseReadTx.buf.Range(bucketType, key, endKey, limit)
    if int64(len(keys)) == limit {
        return keys, vals  // 버퍼에서 충분히 찾음
    }

    // 2. 버킷 캐시 확인/갱신
    bn := bucketType.ID()
    bucket, ok := baseReadTx.buckets[bn]
    if !ok {
        bucket = baseReadTx.tx.Bucket(bucketType.Name())
        baseReadTx.buckets[bn] = bucket
    }

    // 3. BoltDB에서 나머지 조회
    k2, v2 := unsafeRange(bucket.Cursor(), key, endKey, limit-int64(len(keys)))

    // 4. 결과 병합 (BoltDB 결과가 먼저, 버퍼가 나중)
    return append(k2, keys...), append(v2, vals...)
}
```

**읽기 우선순위:**

```
┌──────────────────┐    ┌──────────────────┐
│  txReadBuffer    │    │  BoltDB TX       │
│  (최신 데이터)    │    │  (커밋된 데이터)  │
└────────┬─────────┘    └────────┬─────────┘
         │                       │
         └───────────┬───────────┘
                     │
              ┌──────▼──────┐
              │  병합 결과   │
              │ BoltDB + buf │
              │ (buf 우선)   │
              └─────────────┘
```

버퍼의 데이터가 append 뒤에 오므로, 같은 키가 있으면 버퍼의 값이 우선된다 (더 최신).

### 6.3 IsSafeRangeBucket

```go
// batch_tx.go (42~46행)
type Bucket interface {
    IsSafeRangeBucket() bool
}
```

```go
// schema/bucket.go (43행)
Key = backend.Bucket(bucket{id: 1, name: keyBucketName, safeRangeBucket: true})
```

**왜 이 제한이 필요한가?**

`safeRangeBucket`이 아닌 버킷(meta, lease 등)에서 범위 쿼리를 하면 버퍼와 BoltDB에 같은 키의 다른 값이 있을 수 있다 (덮어쓰기). 이 경우 중복이 발생하므로 `limit=1`로 제한한다.

`Key` 버킷은 리비전 키를 사용하고 절대 덮어쓰지 않으므로 `safeRangeBucket = true`이다.

---

## 7. readTx vs concurrentReadTx

### 7.1 readTx

```
경로: server/storage/backend/read_tx.go (124~138행)
```

```go
type readTx struct {
    baseReadTx
}

func (rt *readTx) Lock()    { rt.mu.Lock() }
func (rt *readTx) Unlock()  { rt.mu.Unlock() }
func (rt *readTx) RLock()   { rt.mu.RLock() }
func (rt *readTx) RUnlock() { rt.mu.RUnlock() }

func (rt *readTx) reset() {
    rt.buf.reset()
    rt.buckets = make(map[BucketID]*bolt.Bucket)
    rt.tx = nil
    rt.txWg = new(sync.WaitGroup)
}
```

`readTx`는 **블록킹** 읽기 트랜잭션이다. `writeback` 시 `readTx.Lock()`이 필요하므로, 읽기 중 쓰기 버퍼 writeback이 불가능하다.

### 7.2 concurrentReadTx

```
경로: server/storage/backend/read_tx.go (140~151행)
```

```go
type concurrentReadTx struct {
    baseReadTx
}

func (rt *concurrentReadTx) Lock()   {}    // No-op
func (rt *concurrentReadTx) Unlock() {}    // No-op
func (rt *concurrentReadTx) RLock()  {}    // No-op
func (rt *concurrentReadTx) RUnlock() { rt.txWg.Done() }  // TX 완료 시그널
```

`concurrentReadTx`는 **비블록킹** 읽기 트랜잭션이다:
- Lock/Unlock이 no-op → 쓰기와 동시에 읽기 가능
- RUnlock에서 `txWg.Done()` → 이 TX가 완료됨을 알림
- 생성 시 `txReadBuffer`의 **복사본**을 가짐 → 독립적 읽기

### 7.3 ConcurrentReadTx 생성

```
경로: server/storage/backend/backend.go (279~352행)
```

```go
func (b *backend) ConcurrentReadTx() ReadTx {
    b.readTx.RLock()
    defer b.readTx.RUnlock()
    b.readTx.txWg.Add(1)  // TX 수명 추적

    // 캐시에서 읽기 버퍼 가져오기 (또는 새로 복사)
    b.txReadBufferCache.mu.Lock()

    curCache := b.txReadBufferCache.buf
    isEmptyCache := curCache == nil
    isStaleCache := curCacheVer != curBufVer

    var buf *txReadBuffer
    switch {
    case isEmptyCache:
        // 최초: 안전한 복사
        curBuf := b.readTx.buf.unsafeCopy()
        buf = &curBuf
    case isStaleCache:
        // 캐시 만료: 동시성 최대화를 위해 락 해제 후 복사
        b.txReadBufferCache.mu.Unlock()
        curBuf := b.readTx.buf.unsafeCopy()
        b.txReadBufferCache.mu.Lock()
        buf = &curBuf
    default:
        // 캐시 유효: 복사 없이 캐시 사용
        buf = curCache
    }

    b.txReadBufferCache.mu.Unlock()

    return &concurrentReadTx{
        baseReadTx: baseReadTx{
            buf:     *buf,           // 버퍼 복사본
            txMu:    b.readTx.txMu,  // 공유
            tx:      b.readTx.tx,    // 공유
            buckets: b.readTx.buckets, // 공유
            txWg:    b.readTx.txWg,  // 공유
        },
    }
}
```

**txReadBufferCache의 역할:**

```
                    txReadBuffer (원본)
                         │
                    ┌────┴────┐
                    │  복사   │ ← 비용이 큼
                    └────┬────┘
                         ▼
              txReadBufferCache (캐시)
                    ┌────┴────┐
               ┌────┤         ├────┐
               │    │         │    │
               ▼    ▼         ▼    ▼
            CRT1  CRT2     CRT3  CRT4
            (공유) (공유)   (공유) (공유)

캐시가 유효하면 여러 ConcurrentReadTx가 같은 캐시를 참조
→ 버퍼 복사 오버헤드 대폭 감소
```

---

## 8. 스키마 정의 (server/storage/schema/)

### 8.1 버킷 정의

```
경로: server/storage/schema/bucket.go (24~59행)
```

```go
var (
    Key             = bucket{id: 1,  name: "key",             safeRangeBucket: true}
    Meta            = bucket{id: 2,  name: "meta",            safeRangeBucket: false}
    Lease           = bucket{id: 3,  name: "lease",           safeRangeBucket: false}
    Alarm           = bucket{id: 4,  name: "alarm",           safeRangeBucket: false}
    Cluster         = bucket{id: 5,  name: "cluster",         safeRangeBucket: false}
    Members         = bucket{id: 10, name: "members",         safeRangeBucket: false}
    MembersRemoved  = bucket{id: 11, name: "members_removed", safeRangeBucket: false}
    Auth            = bucket{id: 20, name: "auth",            safeRangeBucket: false}
    AuthUsers       = bucket{id: 21, name: "authUsers",       safeRangeBucket: false}
    AuthRoles       = bucket{id: 22, name: "authRoles",       safeRangeBucket: false}
    Test            = bucket{id: 100, name: "test",           safeRangeBucket: false}
)

AllBuckets = []backend.Bucket{Key, Meta, Lease, Alarm, Cluster,
    Members, MembersRemoved, Auth, AuthUsers, AuthRoles}
```

**버킷 용도 정리:**

| 버킷 | 용도 | 키 형식 | 값 형식 |
|------|------|---------|---------|
| `key` | KV 데이터 | 리비전 (main.sub) | mvccpb.KeyValue |
| `meta` | 메타데이터 | 고정 키 이름 | 가변 |
| `lease` | 리스 정보 | 리스 ID | leasepb.Lease |
| `alarm` | 알람 상태 | 멤버 ID | alarmpb.AlarmMember |
| `cluster` | 클러스터 정보 | 고정 키 | 가변 |
| `members` | 멤버 정보 | 멤버 ID | 멤버 JSON |
| `members_removed` | 제거된 멤버 | 멤버 ID | 빈 값 |
| `auth` | 인증 설정 | 고정 키 | 가변 |
| `authUsers` | 사용자 정보 | 사용자 이름 | authpb.User |
| `authRoles` | 역할 정보 | 역할 이름 | authpb.Role |

### 8.2 Meta 버킷의 키

```
경로: server/storage/schema/bucket.go (72~87행)
```

```go
var (
    ScheduledCompactKeyName    = []byte("scheduledCompactRev")
    FinishedCompactKeyName     = []byte("finishedCompactRev")
    MetaConsistentIndexKeyName = []byte("consistent_index")
    AuthEnabledKeyName         = []byte("authEnabled")
    AuthRevisionKeyName        = []byte("authRevision")
    MetaTermKeyName            = []byte("term")
    MetaConfStateName          = []byte("confState")
    ClusterClusterVersionKeyName = []byte("clusterVersion")
    ClusterDowngradeKeyName    = []byte("downgrade")
    MetaStorageVersionName     = []byte("storageVersion")
)
```

### 8.3 DefaultIgnores: 해시 검증 제외 키

```go
func DefaultIgnores(bucket, key []byte) bool {
    return bytes.Equal(bucket, Meta.Name()) &&
        (bytes.Equal(key, MetaTermKeyName) ||
         bytes.Equal(key, MetaConsistentIndexKeyName) ||
         bytes.Equal(key, MetaStorageVersionName))
}
```

일관성 해시 비교 시 `term`, `consistent_index`, `storageVersion`은 노드마다 다를 수 있으므로 제외한다.

---

## 9. 읽기/쓰기 버퍼링 전략

### 9.1 txBuffer / txWriteBuffer / txReadBuffer

```
경로: server/storage/backend/tx_buffer.go
```

```go
// 기본 버퍼 (버킷별 키-값 쌍 저장)
type txBuffer struct {
    buckets map[BucketID]*bucketBuffer
}

// 쓰기 버퍼 (순차 여부 추적)
type txWriteBuffer struct {
    txBuffer
    bucket2seq map[BucketID]bool
}

// 읽기 버퍼 (버전 추적)
type txReadBuffer struct {
    txBuffer
    bufVersion uint64
}
```

### 9.2 bucketBuffer: 버킷별 KV 저장소

```
경로: server/storage/backend/tx_buffer.go (159~167행)
```

```go
type bucketBuffer struct {
    buf  []kv     // 키-값 쌍 배열
    used int      // 사용 중인 요소 수
}

const bucketBufferInitialSize = 512

func newBucketBuffer() *bucketBuffer {
    return &bucketBuffer{buf: make([]kv, bucketBufferInitialSize), used: 0}
}
```

**동적 확장:**

```go
func (bb *bucketBuffer) add(k, v []byte) {
    bb.buf[bb.used].key, bb.buf[bb.used].val = k, v
    bb.used++
    if bb.used == len(bb.buf) {
        buf := make([]kv, (3*len(bb.buf))/2)  // 1.5배 확장
        copy(buf, bb.buf)
        bb.buf = buf
    }
}
```

### 9.3 writeback: 쓰기 → 읽기 버퍼 전달

```
경로: server/storage/backend/tx_buffer.go (96~116행)
```

```go
func (txw *txWriteBuffer) writeback(txr *txReadBuffer) {
    for k, wb := range txw.buckets {
        rb, ok := txr.buckets[k]
        if !ok {
            // 읽기 버퍼에 해당 버킷이 없으면 쓰기 버퍼를 이전
            delete(txw.buckets, k)
            if seq, ok := txw.bucket2seq[k]; ok && !seq {
                wb.dedupe()
            }
            txr.buckets[k] = wb
            continue
        }
        // 순차가 아닌 쓰기는 정렬 필요
        if seq, ok := txw.bucket2seq[k]; ok && !seq && wb.used > 1 {
            sort.Sort(wb)
        }
        rb.merge(wb)
    }
    txw.reset()
    txr.bufVersion++
}
```

**순차 vs 비순차 처리:**

```
순차 쓰기 (putSeq → Key 버킷):
  키가 항상 증가 → 정렬 불필요 → merge만

비순차 쓰기 (put → Meta, Lease 등):
  키 순서 불규칙 → sort 후 merge → dedupe(중복 제거)
```

### 9.4 dedupe(): 중복 제거

```
경로: server/storage/backend/tx_buffer.go (229~242행)
```

```go
func (bb *bucketBuffer) dedupe() {
    if bb.used <= 1 {
        return
    }
    sort.Stable(bb)
    widx := 0
    for ridx := 1; ridx < bb.used; ridx++ {
        if !bytes.Equal(bb.buf[ridx].key, bb.buf[widx].key) {
            widx++
        }
        bb.buf[widx] = bb.buf[ridx]
    }
    bb.used = widx + 1
}
```

`sort.Stable` 사용으로 같은 키의 순서가 보존된다. 이 경우 마지막(최신) 값이 최종 값이 된다.

---

## 10. Defrag(): 조각 모음

```
경로: server/storage/backend/backend.go (476~614행)
```

BoltDB는 삭제된 데이터의 공간을 즉시 OS에 반환하지 않는다. Defrag는 데이터를 새 DB에 복사하여 사용하지 않는 공간을 회수한다.

**Defrag 과정:**

```
1. 모든 TX 잠금
   batchTx.LockOutsideApply()
   backend.mu.Lock()
   readTx.Lock()

2. 임시 DB 생성
   temp, _ := os.CreateTemp(dir, "db.tmp.*")
   tmpdb, _ := bolt.Open(tdbp, 0o600, options)

3. 데이터 복사
   defragdb(oldDB, tmpDB, defragLimit=10000)

4. DB 교체
   oldDB.Close()
   tmpDB.Close()
   os.Rename(tmpDB, oldDB)

5. 새 DB 열기
   bolt.Open(dbp, 0o600, bopts)

6. TX 재초기화
   batchTx.tx = begin(true)
   readTx.tx = begin(false)

7. 모든 잠금 해제
```

### 10.1 defragdb(): 데이터 복사

```
경로: server/storage/backend/backend.go (616~676행)
```

```go
func defragdb(odb, tmpdb *bolt.DB, limit int) error {
    tmptx, _ := tmpdb.Begin(true)
    tx, _ := odb.Begin(false)
    defer tx.Rollback()

    c := tx.Cursor()
    count := 0
    for next, _ := c.First(); next != nil; next, _ = c.Next() {
        b := tx.Bucket(next)
        tmpb, _ := tmptx.CreateBucketIfNotExists(next)
        tmpb.FillPercent = 0.9

        b.ForEach(func(k, v []byte) error {
            count++
            if count > limit {
                tmptx.Commit()
                tmptx, _ = tmpdb.Begin(true)
                tmpb = tmptx.Bucket(next)
                tmpb.FillPercent = 0.9
                count = 0
            }
            return tmpb.Put(k, v)
        })
    }

    return tmptx.Commit()
}
```

**defragLimit = 10000의 의미:**

한 번의 쓰기 TX에 너무 많은 키를 넣으면 메모리 사용량이 급증한다. 10000개마다 중간 커밋하여 메모리를 제어한다.

**FillPercent = 0.9:**

Defrag 시 데이터를 순차적으로 복사하므로 높은 fill percent가 최적이다. 이로 인해 defrag 후 DB 크기가 크게 줄어들 수 있다.

---

## 11. Snapshot(): DB 스냅샷

```
경로: server/storage/backend/backend.go (359~401행)
```

```go
func (b *backend) Snapshot() Snapshot {
    b.batchTx.Commit()  // 현재 배치 커밋

    b.mu.RLock()
    defer b.mu.RUnlock()
    tx, _ := b.db.Begin(false)  // 읽기 전용 TX

    stopc, donec := make(chan struct{}), make(chan struct{})
    dbBytes := tx.Size()

    go func() {
        // 전송 시간 모니터링
        warningTimeout := time.Duration(...)
        ticker := time.NewTicker(warningTimeout)
        for {
            select {
            case <-ticker.C:
                b.lg.Warn("snapshotting taking too long")
            case <-stopc:
                snapshotTransferSec.Observe(...)
                return
            }
        }
    }()

    return &snapshot{tx, stopc, donec}
}

type snapshot struct {
    *bolt.Tx
    stopc chan struct{}
    donec chan struct{}
}

func (s *snapshot) Close() error {
    close(s.stopc)
    <-s.donec
    return s.Tx.Rollback()
}
```

**스냅샷의 동작 원리:**

BoltDB의 읽기 TX는 MVCC 스냅샷 격리를 제공한다. `db.Begin(false)`로 시작된 읽기 TX는 그 시점의 DB 상태를 일관되게 볼 수 있다. `bolt.Tx.WriteTo(w)`를 통해 전체 DB를 스트림으로 출력할 수 있다.

---

## 12. ForceCommit()

```
경로: server/storage/backend/backend.go (355~357행)
```

```go
func (b *backend) ForceCommit() {
    b.batchTx.Commit()
}
```

ForceCommit은 현재 배치를 즉시 커밋한다. 주로 다음 상황에서 사용된다:
- 스냅샷 생성 전 (최신 데이터가 디스크에 있어야 함)
- 리더 전환 시
- 클러스터 구성 변경 시

---

## 13. Hash(): 일관성 검증

```
경로: server/storage/backend/backend.go (403~431행)
```

```go
func (b *backend) Hash(ignores func(bucketName, keyName []byte) bool) (uint32, error) {
    h := crc32.New(crc32.MakeTable(crc32.Castagnoli))

    b.mu.RLock()
    defer b.mu.RUnlock()
    err := b.db.View(func(tx *bolt.Tx) error {
        c := tx.Cursor()
        for next, _ := c.First(); next != nil; next, _ = c.Next() {
            b := tx.Bucket(next)
            h.Write(next)
            b.ForEach(func(k, v []byte) error {
                if ignores != nil && !ignores(next, k) {
                    h.Write(k)
                    h.Write(v)
                }
                return nil
            })
        }
        return nil
    })

    return h.Sum32(), nil
}
```

모든 버킷의 모든 키-값 쌍에 대해 CRC32를 계산한다. `ignores` 함수로 노드별로 다를 수 있는 키(`term`, `consistent_index` 등)를 제외한다. 클러스터의 모든 노드가 같은 해시값을 가지면 데이터 일관성이 보장된다.

---

## 14. 설계 요약

```
┌────────────────────────────────────────────────────────────┐
│                    Backend 아키텍처                          │
│                                                              │
│  쓰기 경로:                                                  │
│  ┌──────────┐    ┌───────────────┐    ┌──────────┐          │
│  │ MVCC     │───>│ batchTx       │───>│ BoltDB   │          │
│  │ 계층     │    │ Buffered      │    │ B+ Tree  │          │
│  └──────────┘    │               │    └──────────┘          │
│                  │ ┌───────────┐ │                           │
│                  │ │txWriteBuf │─┤── Unlock 시 writeback    │
│                  │ └───────────┘ │                           │
│                  └───────────────┘                           │
│                                                              │
│  읽기 경로:                                                  │
│  ┌──────────┐    ┌───────────────┐    ┌──────────┐          │
│  │ MVCC     │───>│ readTx /      │───>│ BoltDB   │          │
│  │ 계층     │    │ concurrentRTx │    │ (mmap)   │          │
│  └──────────┘    │               │    └──────────┘          │
│                  │ ┌───────────┐ │                           │
│                  │ │txReadBuf  │─┤── 버퍼 먼저 확인          │
│                  │ └───────────┘ │                           │
│                  └───────────────┘                           │
│                                                              │
│  커밋 전략:                                                  │
│  ┌────────────────────────────────────────────┐             │
│  │  100ms 주기   OR   10000 ops   OR   삭제   │             │
│  │  (시간 기반)      (수량 기반)     (즉시)     │             │
│  └────────────────────────────────────────────┘             │
│                                                              │
│  버퍼 계층:                                                  │
│  ┌────────────┐  writeback  ┌────────────┐  copy           │
│  │txWriteBuf  │ ──────────> │txReadBuf   │ ──────>         │
│  │(미커밋 쓰기)│             │(읽기 캐시)  │     txReadBuf  │
│  └────────────┘             └────────────┘     Cache       │
│                                                              │
│  버킷 구조:                                                  │
│  ┌─────┬──────┬───────┬───────┬─────────┐                  │
│  │ key │ meta │ lease │ alarm │ cluster │ ...               │
│  │ (1) │ (2)  │ (3)   │ (4)   │ (5)     │                  │
│  └─────┴──────┴───────┴───────┴─────────┘                  │
└────────────────────────────────────────────────────────────┘
```

**핵심 설계 원칙:**

1. **배치 커밋**: 개별 쓰기마다 커밋하지 않고 배치로 모아 처리 → fsync 오버헤드 최소화
2. **이중 버퍼링**: txWriteBuffer → txReadBuffer writeback으로 커밋 전에도 최신 데이터 읽기 가능
3. **ConcurrentReadTx**: 버퍼 복사본으로 읽기와 쓰기의 동시성 극대화
4. **삭제 즉시 커밋**: 선형성 보장을 위해 삭제 연산은 즉시 커밋
5. **버킷 캐시**: BoltDB 버킷 참조를 캐싱하여 반복 조회 비용 제거
6. **Defrag**: 순차 복사 + 높은 fill percent로 공간 효율 극대화
7. **스냅샷 격리**: BoltDB의 MVCC 읽기 TX를 활용한 일관된 스냅샷
