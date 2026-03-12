# 10. Watch 메커니즘 Deep Dive

## 개요

etcd의 Watch는 키-값 저장소의 변경 사항을 클라이언트에게 실시간으로 전달하는 핵심 메커니즘이다. Kubernetes의 controller 패턴을 가능하게 하는 근간 기술로, etcd가 단순한 KV 스토어를 넘어 **이벤트 기반 분산 시스템의 백본**으로 기능하는 이유이기도 하다.

Watch 시스템의 핵심 설계 목표:
- **이벤트 순서 보장**: 모든 이벤트는 리비전 순서대로 전달
- **히스토리 재생**: 과거 리비전부터의 이벤트도 재생 가능
- **효율적 리소스 사용**: 수천 개의 워처를 배치 처리로 효율화
- **백프레셔 처리**: 느린 클라이언트를 위한 victim 메커니즘

소스 경로: `server/storage/mvcc/watchable_store.go`, `watcher.go`, `watcher_group.go`

---

## 1. 핵심 데이터 구조

### 1.1 watchableStore 구조체

`watchableStore`는 기본 `store`를 감싸면서 Watch 기능을 추가하는 래퍼이다.

```
경로: server/storage/mvcc/watchable_store.go (55~75행)
```

```go
type watchableStore struct {
    *store

    mu sync.RWMutex

    victims []watcherBatch
    victimc chan struct{}

    unsynced watcherGroup
    synced   watcherGroup

    stopc chan struct{}
    wg    sync.WaitGroup
}
```

필드별 역할:

| 필드 | 타입 | 역할 |
|------|------|------|
| `store` | `*store` | 임베딩된 MVCC 저장소 (키-값 데이터 보관) |
| `mu` | `sync.RWMutex` | watcher 그룹과 배치를 보호하는 뮤텍스 |
| `victims` | `[]watcherBatch` | 채널이 블록된 워처의 대기 배치 목록 |
| `victimc` | `chan struct{}` | victim 처리 루프 깨우기 시그널 |
| `unsynced` | `watcherGroup` | 과거 이벤트를 아직 따라잡지 못한 워처 그룹 |
| `synced` | `watcherGroup` | 현재 리비전에 동기화된 워처 그룹 |
| `stopc` | `chan struct{}` | 종료 시그널 |
| `wg` | `sync.WaitGroup` | 고루틴 종료 대기 |

**왜 `mu`와 `store.mu`를 분리하는가?**

`mu`는 반드시 `store.mu` 이후에 잠겨야 한다. 이 순서를 지키지 않으면 데드락이 발생한다. 주석에서도 명시적으로 경고한다:
> "It should never be locked before locking store.mu to avoid deadlock."

### 1.2 watcher 구조체

개별 Watch 요청을 나타내는 구조체이다.

```
경로: server/storage/mvcc/watchable_store.go (541~571행)
```

```go
type watcher struct {
    key      []byte      // 감시 대상 키
    end      []byte      // 범위 끝 (nil이면 단일 키)
    victim   bool        // 채널 블록으로 victim 처리 중인지
    compacted bool       // 컴팩션으로 제거되었는지
    restore  bool        // 리더 스냅샷 복원 중인지
    startRev int64       // 최초 시작 리비전
    minRev   int64       // 수신할 최소 리비전
    id       WatchID     // 워처 고유 ID
    fcs      []FilterFunc // 이벤트 필터 함수들
    ch       chan<- WatchResponse // 이벤트 전송 채널
}
```

**key/end 필드의 의미:**

```
end == nil  → 단일 키 감시: key="foo"만 감시
end != nil  → 범위 감시: [key, end) 범위의 모든 키 감시
```

**startRev vs minRev의 차이:**

```
startRev: 워처 생성 시 지정한 시작 리비전 (불변)
minRev:   현재까지 전달된 이벤트 이후의 다음 리비전 (변경됨)

예시:
  startRev=5로 생성 → minRev=5
  리비전 5~10 이벤트 전달 후 → minRev=11
```

### 1.3 WatchStream 인터페이스

```
경로: server/storage/mvcc/watcher.go (40~80행)
```

```go
type WatchStream interface {
    Watch(ctx context.Context, id WatchID, key, end []byte,
          startRev int64, fcs ...FilterFunc) (WatchID, error)
    Chan() <-chan WatchResponse
    RequestProgress(id WatchID)
    RequestProgressAll() bool
    Cancel(id WatchID) error
    Close()
    Rev() int64
}
```

`WatchStream`은 하나의 gRPC 스트림에 대응하며, 여러 워처가 **하나의 채널을 공유**한다. 이 설계 덕분에 클라이언트 하나당 스트림 하나만으로 수천 개의 키를 감시할 수 있다.

### 1.4 watchStream 구현체

```
경로: server/storage/mvcc/watcher.go (100~112행)
```

```go
type watchStream struct {
    watchable watchable
    ch        chan WatchResponse      // 버퍼 크기: chanBufLen(128)

    mu       sync.Mutex
    nextID   WatchID
    closed   bool
    cancels  map[WatchID]cancelFunc
    watchers map[WatchID]*watcher
}
```

**왜 채널 버퍼 크기가 128인가?**

```go
// server/storage/mvcc/watchable_store.go (37행)
chanBufLen = 128
```

GitHub 이슈 #11906에서 논의된 내용으로, 버퍼가 너무 작으면 이벤트 전달 지연으로 인해 워처가 victim 상태로 자주 전환된다. 128은 일반적인 워크로드에서 burst를 흡수하기에 적절한 크기이다.

### 1.5 WatchResponse 구조체

```
경로: server/storage/mvcc/watcher.go (82~98행)
```

```go
type WatchResponse struct {
    WatchID         WatchID
    Events          []mvccpb.Event
    Revision        int64
    CompactRevision int64
}
```

- `Events`가 비어 있으면 progress notification (진행 상황 알림)
- `CompactRevision`이 설정되면 컴팩션으로 인한 워처 취소 알림

---

## 2. watcherGroup: 워처 관리의 핵심

### 2.1 구조체 정의

```
경로: server/storage/mvcc/watcher_group.go (144~152행)
```

```go
type watcherGroup struct {
    keyWatchers watcherSetByKey      // 단일 키 워처
    ranges      adt.IntervalTree     // 범위 워처 (인터벌 트리)
    watchers    watcherSet           // 전체 워처 집합
}
```

**왜 두 가지 자료구조를 사용하는가?**

```
+-------------------------------------------+
|            watcherGroup                    |
|                                            |
|  keyWatchers (map[string]watcherSet)       |
|  ┌────────────────────────────────────┐    |
|  │ "foo" → {w1, w2}                  │    |
|  │ "bar" → {w3}                      │    |
|  │ "baz" → {w4, w5, w6}             │    |
|  └────────────────────────────────────┘    |
|                                            |
|  ranges (IntervalTree)                     |
|  ┌────────────────────────────────────┐    |
|  │    ["a","z") → {w7}               │    |
|  │    ["foo","foz") → {w8, w9}       │    |
|  └────────────────────────────────────┘    |
|                                            |
|  watchers (watcherSet)                     |
|  ┌────────────────────────────────────┐    |
|  │ {w1, w2, w3, w4, w5, w6, w7,     │    |
|  │  w8, w9}                          │    |
|  └────────────────────────────────────┘    |
+-------------------------------------------+
```

- **단일 키 워처**: `map` 기반 O(1) 조회
- **범위 워처**: `IntervalTree` 기반 O(log n + k) 조회 (k = 매칭된 구간 수)
- **전체 워처 집합**: 전체 워처 수 카운트, 순회용

### 2.2 watcherSetByKey

```
경로: server/storage/mvcc/watcher_group.go (118~142행)
```

```go
type watcherSetByKey map[string]watcherSet

func (w watcherSetByKey) add(wa *watcher) {
    set := w[string(wa.key)]
    if set == nil {
        set = make(watcherSet)
        w[string(wa.key)] = set
    }
    set.add(wa)
}
```

동일 키에 여러 워처가 등록 가능하다. 예를 들어 Kubernetes에서 여러 컨트롤러가 같은 리소스를 감시할 수 있다.

### 2.3 IntervalTree를 이용한 범위 워처

```
경로: server/storage/mvcc/watcher_group.go (162~181행)
```

```go
func (wg *watcherGroup) add(wa *watcher) {
    wg.watchers.add(wa)
    if wa.end == nil {
        wg.keyWatchers.add(wa)
        return
    }

    // interval already registered?
    ivl := adt.NewStringAffineInterval(string(wa.key), string(wa.end))
    if iv := wg.ranges.Find(ivl); iv != nil {
        iv.Val.(watcherSet).add(wa)
        return
    }

    // not registered, put in interval tree
    ws := make(watcherSet)
    ws.add(wa)
    wg.ranges.Insert(ivl, ws)
}
```

**왜 IntervalTree인가?**

범위 워처의 이벤트 매칭은 "이 키가 어떤 범위에 속하는가?"라는 **stabbing query**이다. IntervalTree는 이를 O(log n + k)로 해결한다. 단순 선형 탐색은 범위 워처가 많아질수록 O(n)이 되어 비효율적이다.

```
IntervalTree Stabbing Query 예시:

Tree에 등록된 범위:
  [a, d)
  [b, f)
  [e, h)

"c" 키에 대한 stabbing query:
  [a, d) ← 매칭 (a <= c < d)
  [b, f) ← 매칭 (b <= c < f)
  [e, h) ← 비매칭 (e > c)

결과: {[a,d), [b,f)}의 워처들에게 이벤트 전달
```

### 2.4 watcherSetByKey 메서드 (이벤트 매칭)

```
경로: server/storage/mvcc/watcher_group.go (268~291행)
```

```go
func (wg *watcherGroup) watcherSetByKey(key string) watcherSet {
    wkeys := wg.keyWatchers[key]
    wranges := wg.ranges.Stab(adt.NewStringAffinePoint(key))

    // zero-copy cases
    switch {
    case len(wranges) == 0:
        return wkeys
    case len(wranges) == 0 && len(wkeys) == 0:
        return nil
    case len(wranges) == 1 && len(wkeys) == 0:
        return wranges[0].Val.(watcherSet)
    }

    // copy case
    ret := make(watcherSet)
    ret.union(wg.keyWatchers[key])
    for _, item := range wranges {
        ret.union(item.Val.(watcherSet))
    }
    return ret
}
```

**최적화 포인트**: 단일 키 워처만 있거나 범위 워처가 하나뿐인 경우 새로운 `watcherSet` 할당 없이 기존 참조를 반환한다 (zero-copy).

---

## 3. synced / unsynced / victims: 3상태 모델

etcd Watch의 핵심은 워처를 세 가지 상태로 관리하는 것이다.

```
                    Watch 생성
                        │
            ┌───────────┴───────────┐
            │                       │
    startRev > currentRev    startRev <= currentRev
    (미래 리비전이므로         (과거 이벤트 따라잡아야 함)
     이미 동기화됨)                  │
            │                       ▼
            ▼               ┌──────────────┐
    ┌──────────────┐        │   UNSYNCED   │
    │    SYNCED    │        │              │
    │              │        │ syncWatchers │
    │  실시간 이벤트 │        │ Loop에 의해  │
    │  notify()로  │◄───────│ 배치 동기화   │
    │  즉시 전달    │        └──────────────┘
    └──────┬───────┘                │
           │                       │
      ch 블록됨                 ch 블록됨
      (send 실패)              (send 실패)
           │                       │
           ▼                       ▼
    ┌──────────────┐        ┌──────────────┐
    │   VICTIMS    │        │   VICTIMS    │
    │              │        │              │
    │ syncVictims  │        │ syncVictims  │
    │ Loop에 의해  │        │ Loop에 의해  │
    │ 재전송 시도   │        │ 재전송 시도   │
    └──────┬───────┘        └──────┬───────┘
           │                       │
      전송 성공              전송 성공
           │                       │
           ▼                       ▼
    ┌──────────────┐        ┌──────────────┐
    │  minRev <=   │        │  minRev <=   │
    │  curRev?     │        │  curRev?     │
    │              │        │              │
    │ Yes→UNSYNCED │        │ Yes→UNSYNCED │
    │ No →SYNCED   │        │ No →SYNCED   │
    └──────────────┘        └──────────────┘
```

### 3.1 SYNCED 상태

**정의**: `startRev > currentRev` 이거나 `startRev == 0`인 워처. 현재 스토어의 최신 리비전과 동기화되어 있다.

```go
// server/storage/mvcc/watchable_store.go (140~146행)
synced := startRev > s.store.currentRev || startRev == 0
if synced {
    wa.minRev = s.store.currentRev + 1
    if startRev > wa.minRev {
        wa.minRev = startRev
    }
    s.synced.add(wa)
}
```

SYNCED 워처는 `notify()` 메서드를 통해 **실시간으로** 이벤트를 수신한다.

### 3.2 UNSYNCED 상태

**정의**: 과거 리비전부터의 이벤트를 아직 따라잡지 못한 워처. 백엔드 스토리지에서 과거 이벤트를 읽어와야 한다.

```go
// server/storage/mvcc/watchable_store.go (147~150행)
} else {
    slowWatcherGauge.Inc()
    s.unsynced.add(wa)
}
```

UNSYNCED 워처는 `syncWatchersLoop()`이 100ms 주기로 배치 동기화한다.

### 3.3 VICTIMS 상태

**정의**: 이벤트를 전달하려 했으나 채널이 가득 차서 전달에 실패한 워처.

```go
// watcher.send() 메서드에서 채널 전송 실패 시
// server/storage/mvcc/watchable_store.go (612~617행)
select {
case w.ch <- wr:
    return true
default:
    return false  // 채널 풀 → victim 처리
}
```

**왜 victim 메커니즘이 필요한가?**

느린 클라이언트로 인해 전체 시스템이 블록되는 것을 방지한다. 채널이 가득 찬 워처를 별도 목록으로 분리하여 비동기적으로 재전송을 시도한다. 이는 **백프레셔(backpressure)** 구현의 핵심이다.

---

## 4. syncWatchersLoop(): 비동기 동기화 루프

### 4.1 전체 흐름

```
경로: server/storage/mvcc/watchable_store.go (222~254행)
```

```go
func (s *watchableStore) syncWatchersLoop() {
    defer s.wg.Done()

    delayTicker := time.NewTicker(watchResyncPeriod) // 100ms
    defer delayTicker.Stop()

    for {
        s.mu.RLock()
        lastUnsyncedWatchers := s.unsynced.size()
        s.mu.RUnlock()

        unsyncedWatchers := 0
        if lastUnsyncedWatchers > 0 {
            unsyncedWatchers = s.syncWatchers()
        }
        syncDuration := time.Since(st)

        delayTicker.Reset(watchResyncPeriod)
        // 진행이 있으면 작업 시간만큼만 대기 (공정성)
        if unsyncedWatchers != 0 && lastUnsyncedWatchers > unsyncedWatchers {
            delayTicker.Reset(syncDuration)
        }

        select {
        case <-delayTicker.C:
        case <-s.stopc:
            return
        }
    }
}
```

**공정성(Fairness) 전략:**

unsynced 워처가 많을 때 syncWatchers만 계속 실행하면 다른 스토어 연산(Put/Delete 등)이 지연된다. 그래서 동기화에 소요된 시간만큼만 대기한 후 다시 실행하여 공정성을 유지한다.

```
타이밍 예시:
  syncWatchers() 실행 시간: 20ms
  unsynced 워처가 줄어들었으면 → 20ms 후 재실행
  unsynced 워처가 안 줄었으면  → 100ms 후 재실행
  unsynced 워처가 0이면       → 100ms 후 재확인
```

### 4.2 syncWatchers(): 배치 동기화 알고리즘

```
경로: server/storage/mvcc/watchable_store.go (340~413행)
```

알고리즘 단계:

```
┌─────────────────────────────────────────────────┐
│  1. choose(): unsynced에서 최대 512개 워처 선택   │
│     → 선택된 워처 그룹(wg)과 최소 리비전(minRev)   │
│       반환                                       │
├─────────────────────────────────────────────────┤
│  2. rangeEvents(): [minRev, curRev+1) 범위의     │
│     모든 이벤트를 백엔드에서 조회                   │
├─────────────────────────────────────────────────┤
│  3. newWatcherBatch(): 이벤트를 해당 워처에        │
│     매핑하여 워처별 이벤트 배치 생성                 │
├─────────────────────────────────────────────────┤
│  4. 각 워처에 이벤트 전송:                         │
│     - 전송 성공 → synced 그룹으로 이동             │
│     - 전송 실패 → victim으로 마킹                  │
│     - moreRev 있음 → unsynced에 유지              │
└─────────────────────────────────────────────────┘
```

```go
func (s *watchableStore) syncWatchers() int {
    s.mu.Lock()
    defer s.mu.Unlock()

    curRev := s.store.currentRev
    compactionRev := s.store.compactMainRev

    // 1단계: 최대 512개 워처 선택, 최소 리비전 계산
    wg, minRev := s.unsynced.choose(maxWatchersPerSync, curRev, compactionRev)

    // 2단계: [minRev, curRev+1) 범위 이벤트 조회
    evs := rangeEvents(s.store.lg, s.store.b, minRev, curRev+1, wg)

    // 3단계: 워처별 이벤트 배치 생성
    victims := make(watcherBatch)
    wb := newWatcherBatch(wg, evs)

    // 4단계: 각 워처에 이벤트 전송
    for w := range wg.watchers {
        // ... (전송 로직)
    }
    s.addVictim(victims)

    return s.unsynced.size()
}
```

### 4.3 choose(): 워처 선택과 컴팩션 처리

```
경로: server/storage/mvcc/watcher_group.go (222~266행)
```

```go
func (wg *watcherGroup) choose(maxWatchers int, curRev, compactRev int64) (*watcherGroup, int64) {
    if len(wg.watchers) < maxWatchers {
        return wg, wg.chooseAll(curRev, compactRev)
    }
    ret := newWatcherGroup()
    for w := range wg.watchers {
        if maxWatchers <= 0 {
            break
        }
        maxWatchers--
        ret.add(w)
    }
    return &ret, ret.chooseAll(curRev, compactRev)
}
```

`maxWatchersPerSync = 512`로 제한하는 이유:
- 한 번에 너무 많은 워처를 처리하면 뮤텍스 보유 시간이 길어진다
- 512개씩 배치로 처리하여 다른 연산과 공정하게 자원을 공유한다

`chooseAll()`에서는 컴팩션된 리비전보다 오래된 워처를 **CompactRevision 응답을 보내고 제거**한다:

```go
if w.minRev < compactRev {
    select {
    case w.ch <- WatchResponse{WatchID: w.id, CompactRevision: compactRev}:
        w.compacted = true
        wg.delete(w)
    default:
        // 채널 풀 → 다음 번에 재시도
    }
    continue
}
```

---

## 5. eventBatch: 배치 크기 제한

### 5.1 watchBatchMaxRevs

```
경로: server/storage/mvcc/watcher_group.go (28행)
```

```go
var watchBatchMaxRevs = 1000
```

한 번에 워처에게 전달하는 이벤트의 최대 distinct revision 수를 1000으로 제한한다.

### 5.2 eventBatch 구조체

```
경로: server/storage/mvcc/watcher_group.go (30~64행)
```

```go
type eventBatch struct {
    evs     []mvccpb.Event   // 리비전 순서 이벤트 목록
    revs    int              // 이 배치의 고유 리비전 수
    moreRev int64            // 이 배치 이후 남은 이벤트의 첫 리비전
}

func (eb *eventBatch) add(ev mvccpb.Event) {
    if eb.revs > watchBatchMaxRevs {
        return  // 배치 크기 초과
    }

    if len(eb.evs) == 0 {
        eb.revs = 1
        eb.evs = append(eb.evs, ev)
        return
    }

    ebRev := eb.evs[len(eb.evs)-1].Kv.ModRevision
    evRev := ev.Kv.ModRevision
    if evRev > ebRev {
        eb.revs++
        if eb.revs > watchBatchMaxRevs {
            eb.moreRev = evRev  // 다음 배치의 시작 리비전 기록
            return
        }
    }

    eb.evs = append(eb.evs, ev)
}
```

**moreRev의 역할:**

```
배치 크기가 1000 리비전을 초과하면:
  현재 배치: rev 100~1099의 이벤트
  moreRev = 1100

워처는 이 배치를 받은 후:
  w.minRev = moreRev(1100)으로 설정
  unsynced에 남아서 다음 syncWatchers() 때 나머지를 받음
```

이 설계 덕분에 오래된 워처가 한 번에 수백만 개의 이벤트를 가져오는 것을 방지한다.

### 5.3 newWatcherBatch: 이벤트-워처 매핑

```
경로: server/storage/mvcc/watcher_group.go (77~94행)
```

```go
func newWatcherBatch(wg *watcherGroup, evs []mvccpb.Event) watcherBatch {
    wb := make(watcherBatch)
    for _, ev := range evs {
        for w := range wg.watcherSetByKey(string(ev.Kv.Key)) {
            if ev.Kv.ModRevision >= w.minRev {
                wb.add(w, ev)
            }
        }
    }
    return wb
}
```

`minRev` 체크는 **이중 알림 방지** 역할을 한다. 이미 전달된 이벤트를 다시 전달하지 않는다.

---

## 6. notify(): 실시간 이벤트 전달

### 6.1 호출 시점

```
경로: server/storage/mvcc/watchable_store_txn.go (22~47행)
```

```go
func (tw *watchableStoreTxnWrite) End() {
    changes := tw.Changes()
    if len(changes) == 0 {
        tw.TxnWrite.End()
        return
    }

    rev := tw.Rev() + 1
    evs := make([]mvccpb.Event, len(changes))
    for i, change := range changes {
        evs[i].Kv = &changes[i]
        if change.CreateRevision == 0 {
            evs[i].Type = mvccpb.Event_DELETE
        } else {
            evs[i].Type = mvccpb.Event_PUT
        }
    }

    tw.s.mu.Lock()
    tw.s.notify(rev, evs)
    tw.TxnWrite.End()
    tw.s.mu.Unlock()
}
```

**중요**: `notify()`와 `TxnWrite.End()`가 모두 `mu.Lock()` 안에서 실행된다. 이는 비동기 이벤트 포스팅이 현재 스토어 리비전을 확인할 때 업데이트가 원자적으로 보이게 하기 위함이다.

### 6.2 notify() 구현

```
경로: server/storage/mvcc/watchable_store.go (467~491행)
```

```go
func (s *watchableStore) notify(rev int64, evs []mvccpb.Event) {
    victim := make(watcherBatch)
    for w, eb := range newWatcherBatch(&s.synced, evs) {
        if eb.revs != 1 {
            s.store.lg.Panic(
                "unexpected multiple revisions in watch notification",
            )
        }
        if w.send(WatchResponse{WatchID: w.id, Events: eb.evs, Revision: rev}) {
            pendingEventsGauge.Add(float64(len(eb.evs)))
        } else {
            w.victim = true
            victim[w] = eb
            s.synced.delete(w)
            slowWatcherGauge.Inc()
        }
        w.minRev = rev + 1
    }
    s.addVictim(victim)
}
```

**패닉 검사 `eb.revs != 1`의 의미:**

`notify()`는 한 트랜잭션의 변경 사항을 전달하므로 이벤트는 반드시 단일 리비전이다. 만약 여러 리비전이 감지되면 로직 오류이므로 패닉을 발생시킨다.

**전송 실패 시 흐름:**

```
1. w.send() 실패 (채널 풀)
2. w.victim = true 마킹
3. synced 그룹에서 제거
4. victim 배치에 추가
5. victimc 채널로 syncVictimsLoop 깨우기
```

---

## 7. syncVictimsLoop(): Victim 재처리

### 7.1 전체 흐름

```
경로: server/storage/mvcc/watchable_store.go (256~281행)
```

```go
func (s *watchableStore) syncVictimsLoop() {
    defer s.wg.Done()

    for {
        for s.moveVictims() != 0 {
            // 모든 victim 처리될 때까지 반복
        }
        s.mu.RLock()
        isEmpty := len(s.victims) == 0
        s.mu.RUnlock()

        var tickc <-chan time.Time
        if !isEmpty {
            tickc = time.After(10 * time.Millisecond)
        }

        select {
        case <-tickc:         // 10ms 후 재시도
        case <-s.victimc:     // 새 victim 도착 시그널
        case <-s.stopc:       // 종료
            return
        }
    }
}
```

### 7.2 moveVictims(): 재전송 로직

```
경로: server/storage/mvcc/watchable_store.go (284~338행)
```

```go
func (s *watchableStore) moveVictims() (moved int) {
    s.mu.Lock()
    victims := s.victims
    s.victims = nil     // 처리 중 새 victim이 추가될 수 있으므로 분리
    s.mu.Unlock()

    var newVictim watcherBatch
    for _, wb := range victims {
        for w, eb := range wb {
            rev := w.minRev - 1
            if !w.send(WatchResponse{...}) {
                // 또 실패 → 다시 victim
                if newVictim == nil {
                    newVictim = make(watcherBatch)
                }
                newVictim[w] = eb
                continue
            }
            moved++
        }

        // 전송 성공한 워처를 적절한 그룹으로 복귀
        s.mu.Lock()
        curRev := s.store.currentRev
        for w, eb := range wb {
            if newVictim != nil && newVictim[w] != nil {
                continue
            }
            w.victim = false
            if eb.moreRev != 0 {
                w.minRev = eb.moreRev
            }
            if w.minRev <= curRev {
                s.unsynced.add(w)    // 아직 따라잡을 이벤트 있음
            } else {
                slowWatcherGauge.Dec()
                s.synced.add(w)       // 동기화 완료
            }
        }
        s.mu.Unlock()
    }

    return moved
}
```

**상태 전이 규칙:**

```
victim 재전송 성공 후:
  w.minRev <= currentRev → UNSYNCED (아직 과거 이벤트 있음)
  w.minRev > currentRev  → SYNCED   (최신 상태)
```

---

## 8. watcher.send(): 이벤트 전달과 필터링

```
경로: server/storage/mvcc/watchable_store.go (573~618행)
```

```go
func (w *watcher) send(wr WatchResponse) bool {
    progressEvent := len(wr.Events) == 0

    // 필터 적용
    if len(w.fcs) != 0 {
        ne := make([]mvccpb.Event, 0, len(wr.Events))
        for i := range wr.Events {
            filtered := false
            for _, filter := range w.fcs {
                if filter(wr.Events[i]) {
                    filtered = true
                    break
                }
            }
            if !filtered {
                ne = append(ne, wr.Events[i])
            }
        }
        wr.Events = ne
    }

    // 필터링 후 이벤트가 없고 progress도 아니면 전송 생략
    if !progressEvent && len(wr.Events) == 0 {
        return true
    }

    // 비블록킹 채널 전송
    select {
    case w.ch <- wr:
        return true
    default:
        return false
    }
}
```

**핵심 포인트:**
1. `FilterFunc`로 PUT/DELETE 타입별 필터링 가능
2. 필터 후 이벤트가 없으면 `true`를 반환 (성공으로 처리하되 전송은 안 함)
3. `select/default` 패턴으로 비블록킹 전송 → 채널이 가득 차면 즉시 `false` 반환

---

## 9. Watch 생성/취소 흐름

### 9.1 Watch 생성

```
클라이언트                 watchStream              watchableStore
    │                          │                          │
    │ Watch(key, startRev)     │                          │
    │─────────────────────────>│                          │
    │                          │                          │
    │                          │ watch(key, end,          │
    │                          │   startRev, id, ch)      │
    │                          │─────────────────────────>│
    │                          │                          │
    │                          │           ┌──────────────┤
    │                          │           │ watcher 생성  │
    │                          │           │              │
    │                          │           │ startRev >   │
    │                          │           │ currentRev?  │
    │                          │           │              │
    │                          │           │ Yes→synced   │
    │                          │           │ No →unsynced │
    │                          │           └──────────────┤
    │                          │                          │
    │                          │ (watcher, cancelFunc)    │
    │                          │<─────────────────────────│
    │                          │                          │
    │ WatchID                  │                          │
    │<─────────────────────────│                          │
    │                          │                          │
    │ <─ ch에서 이벤트 수신 ──>│                          │
```

### 9.2 Watch 취소

```
경로: server/storage/mvcc/watchable_store.go (159~204행)
```

```go
func (s *watchableStore) cancelWatcher(wa *watcher) {
    for {
        s.mu.Lock()
        if s.unsynced.delete(wa) {
            break
        } else if s.synced.delete(wa) {
            break
        } else if wa.ch == nil {
            break   // 이미 취소됨
        } else if wa.compacted {
            break   // 컴팩션으로 제거됨
        }

        if !wa.victim {
            panic("watcher not victim but not in watch groups")
        }

        // victim 배치에서 찾아서 제거
        var victimBatch watcherBatch
        for _, wb := range s.victims {
            if wb[wa] != nil {
                victimBatch = wb
                break
            }
        }
        if victimBatch != nil {
            delete(victimBatch, wa)
            break
        }

        // victim이 현재 처리 중 → 잠시 후 재시도
        s.mu.Unlock()
        time.Sleep(time.Millisecond)
    }

    wa.ch = nil
    s.mu.Unlock()
}
```

**왜 재시도 루프가 필요한가?**

워처가 victim 상태일 때 `moveVictims()`가 동시에 처리 중이면 `s.victims`에서 일시적으로 사라질 수 있다. 이 경우 1ms 대기 후 재시도한다.

---

## 10. rangeEvents(): 이벤트 조회

```
경로: server/storage/mvcc/watchable_store.go (416~436행)
```

```go
func rangeEvents(lg *zap.Logger, b backend.Backend, minRev, maxRev int64, c contains) []mvccpb.Event {
    minBytes, maxBytes := NewRevBytes(), NewRevBytes()
    minBytes = RevToBytes(Revision{Main: minRev}, minBytes)
    maxBytes = RevToBytes(Revision{Main: maxRev}, maxBytes)

    tx := b.ReadTx()
    tx.RLock()
    revs, vs := tx.UnsafeRange(schema.Key, minBytes, maxBytes, 0)
    evs := kvsToEvents(lg, c, revs, vs)
    tx.RUnlock()
    return evs
}
```

**중요 주의사항 (431~433행 주석):**

```
// Must unlock after kvsToEvents, because vs (come from boltdb memory)
// is not deep copy. We can only unlock after Unmarshal, which will do
// deep copy. Otherwise we will trigger SIGSEGV during boltdb re-mmap.
```

BoltDB의 mmap 영역에서 직접 참조하는 데이터이므로, `Unmarshal` (깊은 복사)이 완료되기 전에 트랜잭션을 해제하면 BoltDB가 re-mmap할 때 **SIGSEGV**가 발생할 수 있다.

---

## 11. Progress Notification

### 11.1 단일 워처 Progress

```
경로: server/storage/mvcc/watchable_store.go (506~511행)
```

```go
func (s *watchableStore) progress(w *watcher) {
    s.progressIfSync(map[WatchID]*watcher{w.id: w}, w.id)
}
```

### 11.2 전체 워처 Progress

```
경로: server/storage/mvcc/watchable_store.go (514~538행)
```

```go
func (s *watchableStore) progressIfSync(watchers map[WatchID]*watcher, responseWatchID WatchID) bool {
    s.mu.RLock()
    defer s.mu.RUnlock()

    rev := s.rev()
    for _, w := range watchers {
        if _, ok := s.synced.watchers[w]; !ok {
            return false    // 하나라도 unsynced면 실패
        }
    }

    // 모든 워처가 synced → 첫 번째 워처에 progress 전송
    for _, w := range watchers {
        w.send(WatchResponse{WatchID: responseWatchID, Revision: rev})
        return true
    }
    return true
}
```

Progress notification은 이벤트가 없는 `WatchResponse`로, 클라이언트에게 "현재 이 리비전까지 변경 사항이 없다"는 것을 알려준다.

---

## 12. 메트릭과 모니터링

Watch 시스템은 다양한 메트릭을 노출한다:

| 메트릭 | 설명 |
|--------|------|
| `watcherGauge` | 전체 활성 워처 수 |
| `slowWatcherGauge` | unsynced + victim 워처 수 |
| `watchStreamGauge` | 활성 WatchStream 수 |
| `pendingEventsGauge` | 전달 대기 중 이벤트 수 |

`slowWatcherGauge`가 지속적으로 높으면:
- 클라이언트가 이벤트를 충분히 빠르게 소비하지 못함
- 과거 리비전부터 많은 이벤트를 따라잡아야 하는 워처가 있음
- 네트워크 지연이나 클라이언트 처리 지연 발생

---

## 13. Restore 시 동작

```
경로: server/storage/mvcc/watchable_store.go (206~220행)
```

```go
func (s *watchableStore) Restore(b backend.Backend) error {
    s.mu.Lock()
    defer s.mu.Unlock()
    err := s.store.Restore(b)

    for wa := range s.synced.watchers {
        wa.restore = true
        s.unsynced.add(wa)
    }
    s.synced = newWatcherGroup()
    return nil
}
```

리더 스냅샷으로 복원할 때:
1. 모든 synced 워처를 `restore = true`로 마킹
2. 전부 unsynced로 이동
3. synced 그룹 초기화
4. `syncWatchersLoop()`이 과거 이벤트를 재전달

---

## 14. 설계 요약

```
┌─────────────────────────────────────────────────────────┐
│                   watchableStore                         │
│                                                          │
│  ┌──────────┐  notify()   ┌──────────┐                  │
│  │ SYNCED   │ ◄──────── │ Write     │                  │
│  │ watchers │  실시간     │ Txn End  │                  │
│  └────┬─────┘             └──────────┘                  │
│       │ ch 블록                                          │
│       ▼                                                  │
│  ┌──────────┐  syncVictimsLoop()  ┌──────────────┐      │
│  │ VICTIMS  │ ◄─────────────────│ 10ms 주기     │      │
│  │ watchers │  재전송 시도       │ 재전송 루프    │      │
│  └────┬─────┘                    └──────────────┘      │
│       │ 전송 성공                                        │
│       ▼                                                  │
│  ┌──────────┐  syncWatchersLoop()  ┌─────────────┐      │
│  │ UNSYNCED │ ◄──────────────────│ 100ms 주기   │      │
│  │ watchers │  배치 동기화        │ 512개씩 배치 │      │
│  └──────────┘  rangeEvents()     └─────────────┘      │
│                                                          │
│  ┌──────────────────────────────────────────────┐       │
│  │ watcherGroup                                  │       │
│  │  keyWatchers: map → O(1) 단일 키 조회        │       │
│  │  ranges: IntervalTree → O(log n) 범위 조회   │       │
│  └──────────────────────────────────────────────┘       │
└─────────────────────────────────────────────────────────┘
```

**핵심 설계 원칙:**
1. **3상태 모델**: synced/unsynced/victims로 워처 상태를 명확히 분리
2. **배치 처리**: 한 번에 최대 512개 워처, 1000 리비전 제한으로 자원 사용 제어
3. **비블록킹 전송**: `select/default` 패턴으로 느린 클라이언트가 전체 시스템을 블록하지 않음
4. **공정성**: 동기화 소요 시간에 비례하여 대기 시간을 조절
5. **IntervalTree**: 범위 워처의 효율적 매칭
6. **이벤트 순서 보장**: 리비전 기반 순서로 항상 일관된 이벤트 스트림 제공
