# etcd 핵심 컴포넌트

## 1. 개요

etcd의 핵심 컴포넌트는 5개이다: **Raft 합의 엔진**, **MVCC 저장소**, **Watch 시스템**, **Lease 관리자**, **BoltDB 백엔드**. 이들이 협력하여 분산 일관성, 다중 버전 관리, 실시간 이벤트 알림, TTL 기반 만료, 영속 저장을 구현한다.

## 2. Raft 합의 엔진

### 2.1 역할

Raft는 etcd의 모든 상태 변경(쓰기)이 클러스터 전체에 일관되게 복제되도록 보장한다. `go.etcd.io/raft/v3` 라이브러리를 사용하며, etcd 서버는 이를 `raftNode`로 감싸서 사용한다.

### 2.2 raftNode 구조

```go
// server/etcdserver/raft.go
type raftNode struct {
    raftNodeConfig
    msgSnapC   chan raftpb.Message    // 스냅샷 메시지
    applyc     chan toApply           // 적용할 항목
    readStateC chan raft.ReadState    // ReadIndex 응답
    ticker     *time.Ticker          // Raft 틱
}
```

### 2.3 Raft 처리 루프

`raftNode.start()`는 별도 고루틴에서 Raft의 심장 역할을 한다:

```go
func (r *raftNode) start(rh *raftReadyHandler) {
    go func() {
        for {
            select {
            case <-r.ticker.C:
                r.tick()  // 선거/하트비트 타이머 구동
            case rd := <-r.Ready():
                // 1. SoftState 변경 처리 (리더 변경)
                if rd.SoftState != nil {
                    rh.updateLead(rd.SoftState.Lead)
                }
                // 2. ReadState 전달 (ReadIndex 프로토콜)
                if len(rd.ReadStates) > 0 {
                    r.readStateC <- rd.ReadStates[last]
                }
                // 3. WAL에 엔트리 저장
                r.storage.Save(rd.HardState, rd.Entries)
                // 4. 스냅샷 저장 (있으면)
                if !raft.IsEmptySnap(rd.Snapshot) {
                    r.storage.SaveSnap(rd.Snapshot)
                }
                // 5. Raft 메시지 전송
                r.transport.Send(rd.Messages)
                // 6. 적용할 항목 전달
                r.applyc <- toApply{entries, snapshot}
                // 7. Raft 진행
                r.Advance()
            }
        }
    }()
}
```

### 2.4 쓰기 경로: Propose → Apply

```
1. 클라이언트 Put("/foo", "bar")
2. EtcdServer.raftRequest()
   → w.Register(id) - 완료 대기 등록
   → r.Propose(ctx, marshaled_data)
3. Raft 리더가 엔트리를 로그에 추가
4. 팔로워들에게 AppendEntries 전송
5. 과반수 ACK → 커밋
6. Ready() → WAL 저장 → applyc로 전달
7. EtcdServer.run()에서 applyAll() 호출
8. UberApplier가 MVCC store에 적용
9. w.Trigger(id, result) → 클라이언트에 응답
```

### 2.5 리더 선거

```
TickMs × ElectionTicks = 선거 타임아웃 (기본 100ms × 10 = 1초)

팔로워 → 하트비트 타임아웃 → Candidate 전환
Candidate → RequestVote 전송 → 과반수 투표 → Leader
Leader → 하트비트 전송 (TickMs 주기)
```

## 3. MVCC 저장소

### 3.1 역할

MVCC(Multi-Version Concurrency Control) 저장소는 키-값 데이터를 리비전 단위로 관리한다. 과거 버전 조회, Watch 이벤트 생성, 트랜잭션 처리를 담당한다.

### 3.2 store 구조

```go
// server/storage/mvcc/kvstore.go
type store struct {
    b       backend.Backend     // 실제 데이터 저장 (BoltDB)
    kvindex index               // 인메모리 B-tree 인덱스

    currentRev     int64        // 현재 리비전 (단조 증가)
    compactMainRev int64        // 마지막 컴팩션 리비전

    fifoSched schedule.Scheduler // 비동기 컴팩션
}
```

### 3.3 2계층 구조

```
┌──────────────────────────────────────────────┐
│            treeIndex (인메모리)                 │
│                                               │
│  B-tree: key → keyIndex{generations[revs]}    │
│  역할: "이 키의 rev≤N 최신 리비전은?" 응답       │
│  복잡도: O(log N)                              │
└──────────────────┬───────────────────────────┘
                   │ 리비전 바이트
┌──────────────────▼───────────────────────────┐
│            Backend (BoltDB)                    │
│                                               │
│  "key" 버킷: rev_bytes → KeyValue protobuf    │
│  역할: 실제 키-값 데이터 영속 저장               │
│  복잡도: O(log N) B+tree                       │
└──────────────────────────────────────────────┘
```

### 3.4 Put 동작

```go
// server/storage/mvcc/kvstore_txn.go (요약)
func (tw *storeTxnWrite) put(key, value []byte, leaseID lease.LeaseID) {
    rev := tw.beginRev + 1

    // 1. 기존 키 확인
    _, created, ver, err := tw.s.kvindex.Get(key, rev)
    if err != nil {
        // 새 키: create_revision = rev, version = 1
        c = rev; ver = 1
    } else {
        // 기존 키: version++
        ver++
    }

    // 2. KeyValue 생성
    kv := mvccpb.KeyValue{
        Key:            key,
        Value:          value,
        CreateRevision: c,
        ModRevision:    rev,
        Version:        ver,
        Lease:          int64(leaseID),
    }

    // 3. BoltDB에 저장
    d, _ := kv.Marshal()
    tw.tx.UnsafePut(schema.Key, revBytes, d)

    // 4. 인덱스 업데이트
    tw.s.kvindex.Put(key, Revision{Main: rev})

    // 5. 변경 기록 (Watch 이벤트용)
    tw.changes = append(tw.changes, kv)
}
```

### 3.5 Range 동작

```go
// server/storage/mvcc/kvstore_txn.go (요약)
func (tr *storeTxnRead) rangeKeys(key, end []byte, curRev int64, ro RangeOptions) (*RangeResult, error) {
    rev := ro.Rev
    if rev == 0 { rev = curRev }
    if rev < tr.s.compactMainRev { return nil, ErrCompacted }

    // 1. 인덱스에서 리비전 조회
    revpairs, total := tr.s.kvindex.Revisions(key, end, rev, ro.Count)

    // 2. BoltDB에서 KV 조회
    kvs := make([]mvccpb.KeyValue, 0, len(revpairs))
    for _, revpair := range revpairs {
        revBytes := revisionToBytes(revpair)
        _, vs := tr.tx.UnsafeRange(schema.Key, revBytes, nil, 0)
        var kv mvccpb.KeyValue
        kv.Unmarshal(vs[0])
        kvs = append(kvs, kv)
    }

    return &RangeResult{KVs: kvs, Count: total, Rev: curRev}, nil
}
```

## 4. Watch 시스템

### 4.1 역할

Watch는 키 변경 이벤트를 실시간으로 클라이언트에 스트리밍한다. etcd의 핵심 차별점으로, Kubernetes의 Informer/Controller 메커니즘의 기반이다.

### 4.2 watchableStore 구조

```go
// server/storage/mvcc/watchable_store.go
type watchableStore struct {
    *store                          // MVCC store 임베딩

    unsynced watcherGroup           // 과거 이벤트 따라잡기 필요
    synced   watcherGroup           // 현재와 동기화됨
    victims  []watcherBatch         // 채널 막힌 워처들
    victimc  chan struct{}           // 피해자 발생 알림
}
```

### 4.3 워처 상태 전이

```
┌──────────────────────────────────────────────────┐
│                                                   │
│   Watch 생성                                      │
│   (startRev <= currentRev)                        │
│         │                                         │
│         ▼                                         │
│   ┌──────────┐   동기화 완료    ┌──────────┐       │
│   │ unsynced │──────────────→│  synced  │       │
│   └──────────┘               └────┬─────┘       │
│         ▲                         │              │
│         │                    채널 막힘             │
│    재동기화                        │              │
│         │                    ┌────▼─────┐       │
│         └────────────────────│ victims  │       │
│              채널 해제        └──────────┘       │
│                                                   │
│   Watch 생성                                      │
│   (startRev > currentRev)                         │
│         │                                         │
│         └──────────────────→ synced에 바로 추가     │
└──────────────────────────────────────────────────┘
```

### 4.4 watcherGroup (IntervalTree)

```go
// server/storage/mvcc/watcher_group.go
type watcherGroup struct {
    keyWatchers watcherSetByKey    // 단일 키 워처: map[key]watcherSet
    ranges      adt.IntervalTree   // 범위 워처: IntervalTree
    watchers    watcherSet         // 전체 워처 집합
}
```

**단일 키 Watch**: `keyWatchers`에서 O(1) 조회
**범위 Watch**: IntervalTree에서 O(log N + k) stabbing query

```
IntervalTree 예시:
  Watch ["/app/", "/app0")  ← /app/ 프리픽스 전체
  Watch ["/db/", "/db0")   ← /db/ 프리픽스 전체

  이벤트: Put("/app/config") 발생
  → tree.Stab("/app/config")
  → ["/app/", "/app0") 범위에 매칭
  → 해당 워처에 이벤트 전달
```

### 4.5 동기화 루프

```go
// syncWatchersLoop: 100ms 주기
func (s *watchableStore) syncWatchersLoop() {
    for {
        s.mu.RLock()
        st := time.Now()
        lastUnsyncedWatchers := s.unsynced.size()
        s.mu.RUnlock()

        unsyncedWatchers := 0
        if lastUnsyncedWatchers > 0 {
            unsyncedWatchers = s.syncWatchers()
        }
        // 100ms 대기 후 반복
        time.Sleep(100 * time.Millisecond)
    }
}

// syncVictimsLoop: 10ms 주기
func (s *watchableStore) syncVictimsLoop() {
    for {
        // 막힌 워처들에게 이벤트 재전송 시도
        // 성공 → synced로 이동
        // 실패 → victims에 유지
    }
}
```

## 5. Lease 관리자

### 5.1 역할

Lease는 TTL(Time-To-Live) 기반으로 키의 생존 기간을 관리한다. Lease가 만료되면 연결된 모든 키가 자동 삭제된다. 분산 잠금, 서비스 등록, 세션 관리에 사용된다.

### 5.2 lessor 구조

```go
// server/lease/lessor.go
type lessor struct {
    leaseMap             map[LeaseID]*Lease
    leaseExpiredNotifier *LeaseExpiredNotifier  // 만료 알림 (힙)
    leaseCheckpointHeap  LeaseQueue             // 체크포인트 힙
    itemMap              map[LeaseItem]LeaseID  // 키 → Lease 매핑

    demotec chan struct{}    // nil이면 Secondary, non-nil이면 Primary
    expiredC chan []*Lease   // 만료된 Lease 채널

    rd RangeDeleter          // 만료 시 키 삭제 위임
    cp Checkpointer          // TTL 체크포인트
}
```

### 5.3 Primary/Secondary 모델

```
Raft 리더 = Lease Primary
  → 만료 타이머 실행
  → 만료된 Lease를 Raft에 Revoke 제안
  → KeepAlive 처리

Raft 팔로워 = Lease Secondary
  → 만료 타이머 정지 (demotec = nil)
  → Raft Apply로만 Lease 변경 적용
  → KeepAlive 리더에 전달
```

### 5.4 Lease 생명주기

```
Grant(id, ttl=30s)
  → leaseMap[id] = Lease{ttl:30, expiry:now+30}
  → leaseExpiredNotifier에 등록

Attach(id, items=["/session/abc"])
  → lease.itemSet["/session/abc"] = {}
  → itemMap["/session/abc"] = id

Renew(id)  (KeepAlive)
  → lease.expiry = now + ttl (만료 시간 갱신)

만료 감지 (Primary만)
  → expiredLeaseC ← [만료된 Lease]
  → EtcdServer.revokeExpiredLeases()
  → Raft Propose(LeaseRevoke)

Revoke(id)
  → 연결된 키 삭제 (rd.DeleteRange)
  → leaseMap에서 제거
  → Watch DELETE 이벤트 발생
```

### 5.5 체크포인트

리더가 변경되면 Lease의 남은 TTL이 초기화될 수 있다. 이를 방지하기 위해 **체크포인트** 메커니즘이 있다:

```
리더가 주기적으로 (checkpointInterval 마다)
  → 각 Lease의 remainingTTL = expiry - now
  → Raft에 LeaseCheckpoint 제안
  → 전체 클러스터에 남은 TTL 동기화

리더 변경 시
  → Promote(extend) 호출
  → remainingTTL > 0이면 그 값으로 만료 시간 설정
  → TTL 리셋 방지
```

## 6. BoltDB 백엔드

### 6.1 역할

BoltDB(bbolt)는 etcd의 영속 저장소이다. 모든 키-값 데이터, Lease, 인증 정보, 클러스터 메타데이터가 하나의 BoltDB 파일에 저장된다.

### 6.2 backend 구조

```go
// server/storage/backend/backend.go
type backend struct {
    db    *bolt.DB             // BoltDB 인스턴스

    batchInterval time.Duration // 배치 커밋 주기 (기본 100ms)
    batchLimit    int           // 배치 크기 제한 (기본 10,000)
    batchTx       *batchTxBuffered  // 배치 쓰기
    readTx        *readTx           // 공유 읽기
}
```

### 6.3 배치 쓰기 최적화

etcd는 BoltDB의 쓰기 성능을 극대화하기 위해 **배치 쓰기**를 사용한다:

```
개별 쓰기 (느림):
  Put1 → Commit → fsync
  Put2 → Commit → fsync
  Put3 → Commit → fsync

배치 쓰기 (빠름):
  Put1 ─┐
  Put2 ─┤→ 배치 → Commit → fsync (100ms마다 또는 10,000개마다)
  Put3 ─┘
```

### 6.4 읽기 트랜잭션 최적화

```
ReadTx (공유 버퍼):
  → 모든 읽기가 같은 버퍼 공유
  → 오버헤드 낮음
  → 쓰기와 동시 접근 시 블로킹 가능

ConcurrentReadTx (개별 복사):
  → 버퍼를 복사하여 독립적 읽기
  → 오버헤드 높지만 비블로킹
  → Watch의 이벤트 조회에 사용
```

### 6.5 Defrag (조각 모음)

BoltDB는 삭제해도 파일 크기가 줄지 않는다 (freelist에 빈 페이지 추가). Defrag는 새 DB 파일에 데이터를 복사하여 크기를 줄인다:

```
Defrag 과정:
  1. 새 BoltDB 파일 생성 (temp)
  2. 기존 DB에서 모든 키-값 복사
  3. 기존 파일 교체
  4. freelist 초기화

주의: Defrag 중 서버가 일시 중단됨
```

## 7. 컴포넌트 간 상호작용

### 7.1 Put 요청의 전체 경로

```
gRPC kvServer.Put()
  │
  ▼
EtcdServer.Put()
  │ raftRequest()
  ▼
raftNode.Propose()
  │ Raft 합의
  ▼
WAL.Save()           ← 내구성 보장
  │
  ▼
applyc ← toApply
  │
  ▼
UberApplier.Apply()
  │
  ├─ MVCC store.Put()
  │   ├─ treeIndex.Put()    ← 인메모리 인덱스
  │   └─ backend.Put()      ← BoltDB 영속화
  │
  ├─ Lessor.Attach()        ← Lease 연결 (있으면)
  │
  └─ watchableStore.notify()
      └─ synced 워처에 Event 전송  ← Watch 이벤트
```

### 7.2 Range의 두 가지 경로

```
Linearizable 읽기:
  gRPC → EtcdServer → ReadIndex → Raft 리더 확인 → MVCC Range

Serializable 읽기:
  gRPC → EtcdServer → MVCC Range (바로 로컬 조회)
```

### 7.3 Lease 만료 → 키 삭제 체인

```
lessor.expiredC ← 만료된 Lease
  │
  ▼
EtcdServer.revokeExpiredLeases()
  │ raftRequest(LeaseRevoke)
  ▼
Raft 합의 → Apply
  │
  ▼
lessor.Revoke(id)
  │
  ├─ lease.Keys() → 연결된 키 목록
  ├─ RangeDeleter.DeleteRange() → MVCC 삭제
  │   └─ Watch DELETE 이벤트 발생
  └─ leaseMap에서 제거
```

## 8. 성능 특성

| 컴포넌트 | 읽기 | 쓰기 | 주요 병목 |
|---------|------|------|----------|
| Raft | - | O(N) 합의 | 네트워크 RTT, 디스크 fsync |
| MVCC Index | O(log N) | O(log N) | 메모리 (B-tree 크기) |
| BoltDB | O(log N) | 배치 O(1) amortized | 디스크 I/O, fsync |
| Watch | O(log N + k) | - | 워처 수, 이벤트 빈도 |
| WAL | - | O(1) append | 디스크 fsync |

## 9. 요약

```
┌──────────────────────────────────────────────────────┐
│               etcd 핵심 컴포넌트 요약                   │
├──────────────────────────────────────────────────────┤
│                                                       │
│  Raft    : 쓰기 합의 + 리더 선거 + 로그 복제            │
│  MVCC    : 리비전 기반 다중 버전 + B-tree 인덱스         │
│  Watch   : synced/unsynced/victims 3상태 워처 관리     │
│  Lease   : TTL 기반 키 만료 + Primary/Secondary 모델   │
│  Backend : BoltDB 배치 쓰기 + 읽기 최적화              │
│                                                       │
│  WAL     : Raft 로그 영속화 (장애 복구 보장)             │
│  Snapshot: 주기적 상태 캡처 (빠른 복구)                  │
│                                                       │
└──────────────────────────────────────────────────────┘
```
