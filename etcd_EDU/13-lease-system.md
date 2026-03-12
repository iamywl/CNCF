# 13. Lease 시스템

## 개요

etcd의 Lease 시스템은 키에 TTL(Time-To-Live)을 부여하는 메커니즘이다. Lease가 만료되면 해당 Lease에 연결된 모든 키가 자동으로 삭제된다. 이 시스템은 서비스 디스커버리, 분산 잠금, 세션 관리 등에서 핵심적인 역할을 한다.

Lease 시스템의 설계는 Raft 합의 프로토콜 위에서 동작해야 하는 제약 조건을 가진다. TTL은 시간 기반이지만, 분산 시스템에서 시간은 노드마다 다를 수 있다. etcd는 이 문제를 **Primary/Secondary 모델**과 **체크포인트 메커니즘**으로 해결한다.

---

## 핵심 데이터 구조

### Lease 구조체

`server/lease/lease.go`에 정의된 `Lease` 구조체는 개별 Lease를 표현한다.

```
Lease 구조체 필드:
├── ID           LeaseID        // Lease 고유 식별자 (int64)
├── ttl          int64          // TTL (초 단위)
├── remainingTTL int64          // 체크포인트된 남은 TTL
├── expiryMu     sync.RWMutex   // expiry 동시 접근 보호
├── expiry       time.Time      // 만료 시각 (zero value = 영원)
├── mu           sync.RWMutex   // itemSet 동시 접근 보호
├── itemSet      map[LeaseItem]struct{}  // 연결된 키 집합
└── revokec      chan struct{}  // 리보크 완료 알림 채널
```

**왜 expiry와 itemSet에 별도의 뮤텍스를 사용하는가?**

`expiry`와 `itemSet`은 독립적인 경로에서 접근된다. 만료 시간은 Renew나 Promote 시 갱신되고, itemSet은 키의 Put/Delete 시 변경된다. 별도 뮤텍스를 사용하면 두 작업이 서로를 블로킹하지 않는다.

### LeaseItem 구조체

```go
type LeaseItem struct {
    Key string
}
```

`LeaseItem`은 Lease에 연결된 키를 나타내는 단순한 구조체이다. 키 문자열만 포함하며, `itemSet`에 `map[LeaseItem]struct{}` 형태로 저장된다.

### LeaseID 타입

```go
type LeaseID int64
```

`LeaseID`는 int64 기반 타입으로, 클라이언트가 직접 지정하거나 서버가 자동 생성할 수 있다. `NoLease = LeaseID(0)`은 Lease가 없음을 나타내는 특수 값이다.

---

## Lessor 인터페이스

`server/lease/lessor.go`에 정의된 `Lessor` 인터페이스는 Lease 관리의 전체 계약을 정의한다.

```
Lessor 인터페이스:
├── Grant(id, ttl)       → Lease 생성
├── Revoke(id)           → Lease 삭제 (연결된 키도 삭제)
├── Renew(id)            → TTL 갱신
├── Checkpoint(id, ttl)  → 남은 TTL 기록
├── Attach(id, items)    → 키를 Lease에 연결
├── Detach(id, items)    → 키를 Lease에서 분리
├── GetLease(item)       → 키의 Lease 조회
├── Lookup(id)           → Lease 조회
├── Leases()             → 전체 Lease 목록
├── Promote(extend)      → Primary로 승격
├── Demote()             → Secondary로 강등
├── Recover(b, rd)       → 백엔드에서 복원
├── ExpiredLeasesC()     → 만료 알림 채널
└── Stop()               → 종료
```

---

## lessor 구조체 (구현체)

```
lessor 구조체:
├── mu                    sync.RWMutex     // 전체 상태 보호
├── demotec               chan struct{}     // Primary 상태 표시 (nil이면 Secondary)
├── leaseMap              map[LeaseID]*Lease
├── leaseExpiredNotifier  *LeaseExpiredNotifier  // 만료 힙
├── leaseCheckpointHeap   LeaseQueue       // 체크포인트 힙
├── itemMap               map[LeaseItem]LeaseID  // 키→Lease 역매핑
├── rd                    RangeDeleter     // 키 삭제 콜백
├── cp                    Checkpointer     // 체크포인트 콜백
├── b                     backend.Backend  // 영속 저장소
├── minLeaseTTL           int64            // 최소 TTL
├── leaseRevokeRate       int              // 초당 리보크 제한
├── expiredC              chan []*Lease     // 만료 Lease 알림 (버퍼 16)
├── stopC                 chan struct{}     // 종료 신호
├── doneC                 chan struct{}     // 종료 완료 신호
├── checkpointInterval    time.Duration    // 체크포인트 주기 (기본 5분)
├── expiredLeaseRetryInterval time.Duration // 만료 재확인 주기 (기본 3초)
└── checkpointPersist     bool             // 체크포인트 영속화 여부
```

### 왜 leaseMap과 itemMap 두 개의 맵을 유지하는가?

`leaseMap`은 LeaseID로 Lease를 빠르게 조회하기 위한 것이고, `itemMap`은 키로부터 해당 키가 속한 Lease를 빠르게 찾기 위한 역색인(reverse index)이다. 키에 대한 Put 요청이 들어올 때, 해당 키가 이미 어떤 Lease에 연결되어 있는지 O(1)로 확인해야 하기 때문이다.

---

## Grant(): Lease 생성

`Grant()` 메서드는 새로운 Lease를 생성한다.

```
Grant(id, ttl) 흐름:
1. id == NoLease(0)이면 ErrLeaseNotFound 반환
2. ttl > MaxLeaseTTL(9,000,000,000)이면 ErrLeaseTTLTooLarge 반환
3. NewLease(id, ttl) 호출하여 Lease 객체 생성
4. mu.Lock() 획득
5. 중복 ID 확인 → ErrLeaseExists
6. ttl < minLeaseTTL이면 minLeaseTTL로 상향 조정
7. Primary인 경우:
   └── l.refresh(0) → 현재 시각 + TTL로 만료 시각 설정
8. Secondary인 경우:
   └── l.forever() → 만료를 infinity로 설정
9. leaseMap[id] = l
10. l.persistTo(b) → 백엔드에 영속화
11. Primary인 경우:
    ├── leaseExpiredNotifier에 등록
    └── 체크포인트 스케줄링
```

**왜 Secondary에서는 expiry를 forever로 설정하는가?**

만료 관리는 오직 Primary(Raft 리더)만 담당한다. Secondary(팔로워)가 만료를 감지하면 리보크 요청을 합의 없이 처리하게 되어 일관성이 깨진다. Secondary는 Lease 데이터만 보관하고, Primary로 승격될 때 적절한 만료 시각을 설정한다.

### Lease의 refresh() 메서드

```go
func (l *Lease) refresh(extend time.Duration) {
    newExpiry := time.Now().Add(extend + time.Duration(l.getRemainingTTL())*time.Second)
    l.expiryMu.Lock()
    defer l.expiryMu.Unlock()
    l.expiry = newExpiry
}
```

`getRemainingTTL()`은 체크포인트된 `remainingTTL`이 있으면 그 값을 사용하고, 없으면 원래 `ttl` 값을 사용한다. 이것이 체크포인트 메커니즘의 핵심이다.

### Lease의 persistTo() 메서드

```go
func (l *Lease) persistTo(b backend.Backend) {
    lpb := leasepb.Lease{ID: int64(l.ID), TTL: l.ttl, RemainingTTL: l.remainingTTL}
    tx := b.BatchTx()
    tx.LockInsideApply()
    defer tx.Unlock()
    schema.MustUnsafePutLease(tx, &lpb)
}
```

Lease는 protobuf 형태로 백엔드(BoltDB)에 저장된다. ID, TTL, remainingTTL 세 필드만 영속화하며, 연결된 키 목록(`itemSet`)은 저장하지 않는다. 키 목록은 KV 스토어를 순회하여 복원한다.

---

## Revoke(): Lease 삭제

```
Revoke(id) 흐름:
1. mu.Lock()
2. leaseMap[id] 조회 → nil이면 ErrLeaseNotFound
3. defer close(l.revokec) → 대기 중인 Renew에 알림
4. mu.Unlock() (외부 작업 전에 잠금 해제)
5. RangeDeleter로 트랜잭션 시작
6. Lease에 연결된 키를 정렬 후 삭제
   └── 정렬 이유: 모든 멤버에서 동일한 순서로 삭제하여 백엔드 해시 일관성 보장
7. mu.Lock()
8. leaseMap에서 삭제
9. 백엔드에서 Lease 레코드 삭제
10. txn.End() → 트랜잭션 커밋
```

**왜 키 삭제를 정렬 순서로 수행하는가?**

etcd는 모든 멤버의 백엔드 해시가 동일해야 데이터 무결성을 검증할 수 있다. 키 삭제 순서가 멤버마다 다르면 B-tree의 리밸런싱이 달라져 해시가 불일치할 수 있다. 따라서 정렬하여 결정론적 순서를 보장한다.

**왜 키 삭제와 Lease 삭제를 같은 트랜잭션에서 수행하는가?**

소스코드 주석에 명시적으로 설명되어 있다:
> lease deletion needs to be in the same backend transaction with the kv deletion. Or we might end up with not executing the revoke or not deleting the keys if etcdserver fails in between.

중간에 서버가 실패하면 키는 삭제되었지만 Lease는 남아 있거나, 그 반대의 불일치 상태가 발생할 수 있다.

### revokec 채널의 역할

`revokec`는 `make(chan struct{})` 타입이며, Revoke 시 `close(l.revokec)`로 닫힌다. `Renew()` 메서드는 만료된 Lease의 리보크 완료를 대기할 때 이 채널을 사용한다:

```go
// Renew()에서의 사용
case <-l.revokec:
    return -1, ErrLeaseNotFound
```

---

## Renew(): TTL 갱신

```
Renew(id) 흐름:
1. mu.RLock()
2. Primary가 아니면 ErrNotPrimary → 클라이언트는 리더로 재시도
3. demotec 채널 저장 (나중에 demote 감지용)
4. leaseMap[id] 조회 → nil이면 ErrLeaseNotFound
5. remainingTTL > 0이면 clearRemainingTTL 플래그 설정
6. mu.RUnlock()
7. l.expired() 확인:
   └── 만료된 경우 select로 대기:
       ├── <-l.revokec → ErrLeaseNotFound (리보크 완료)
       ├── <-demotec   → ErrNotPrimary (리더 변경)
       └── <-le.stopC  → ErrNotPrimary (서버 종료)
8. clearRemainingTTL이면 Checkpoint(ID, 0) 호출
   └── RAFT 엔트리를 통해 remainingTTL 초기화
9. mu.Lock()
10. leaseMap[id] 재확인 (리보크 경쟁 조건 방지)
11. l.refresh(0) → 만료 시각 갱신
12. leaseExpiredNotifier 업데이트
13. mu.Unlock()
14. l.ttl 반환
```

**왜 Renew 시 remainingTTL을 0으로 초기화하는가?**

체크포인트된 remainingTTL이 남아 있으면, 다음 Renew나 Promote 시 이 값이 사용되어 전체 TTL보다 짧은 시간만 부여된다. Renew는 TTL을 완전히 새로 시작하는 것이므로, remainingTTL을 0으로 초기화해야 한다. 이를 RAFT를 통해 전파하여 모든 노드에서 일관되게 적용한다.

**왜 Renew 시 RAFT 엔트리 수를 제한하는가?**

소스코드 주석:
> By applying a RAFT entry only when the remainingTTL is already set, we limit the number of RAFT entries written per lease to a max of 2 per checkpoint interval.

체크포인트 간격(기본 5분) 동안 최대 2개의 RAFT 엔트리만 생성하여, 빈번한 Renew가 RAFT 로그를 과도하게 증가시키는 것을 방지한다.

---

## Checkpoint(): 남은 TTL 기록

체크포인트는 리더 변경 시 Lease의 실제 남은 TTL을 보존하기 위한 메커니즘이다.

```
Checkpoint(id, remainingTTL) 흐름:
1. mu.Lock()
2. leaseMap[id] 조회
3. l.remainingTTL = remainingTTL (로컬 상태 업데이트)
4. shouldPersistCheckpoints()이면 l.persistTo(b)로 영속화
5. Primary이면 다음 체크포인트 스케줄링
6. mu.Unlock()
```

### 체크포인트가 필요한 이유

```
체크포인트 없는 경우의 문제:

시간 0s: Lease TTL=100s 생성
시간 50s: 리더 A에서 남은 TTL=50s
시간 50s: 리더 변경 → B가 새 리더
시간 50s: B의 Promote에서 TTL=100s로 재설정! (원래는 50s만 남아야 함)
시간 150s: Lease 만료 (원래 100s에 만료되어야 했음)

체크포인트가 있는 경우:

시간 0s: Lease TTL=100s 생성
시간 45s: 체크포인트 → remainingTTL=55s 기록 (RAFT 통해 전파)
시간 50s: 리더 변경 → B가 새 리더
시간 50s: B의 Promote에서 remainingTTL=55s 사용 → 만료시각 = 50s + 55s = 105s
시간 105s: Lease 만료 (거의 정확)
```

### shouldPersistCheckpoints()

```go
func (le *lessor) shouldPersistCheckpoints() bool {
    cv := le.cluster.Version()
    return le.checkpointPersist || (cv != nil && greaterOrEqual(*cv, version.V3_6))
}
```

v3.6부터는 체크포인트가 항상 백엔드에 영속화된다. 이전 버전에서는 `checkpointPersist` 설정에 따라 결정된다.

---

## Primary/Secondary 모델

### Promote(): Primary로 승격

노드가 Raft 리더가 되면 `Promote(extend)`가 호출된다.

```
Promote(extend) 흐름:
1. mu.Lock()
2. demotec = make(chan struct{}) → Primary 상태 표시
3. 모든 Lease 순회:
   ├── l.refresh(extend) → 만료 시각 = now + extend + getRemainingTTL()
   ├── leaseExpiredNotifier에 등록
   └── 체크포인트 스케줄링
4. 만약 Lease 수 < leaseRevokeRate:
   └── 조기 반환 (pile-up 가능성 없음)
5. Lease를 만료 시간순으로 정렬
6. pile-up 방지를 위한 만료 시간 분산:
   └── targetExpiresPerSecond = (3 * leaseRevokeRate) / 4
```

**extend 파라미터의 의미:**

리더 선출 직후에는 네트워크가 불안정할 수 있다. `extend`는 추가 시간을 부여하여 선출 직후 Lease가 대량 만료되는 것을 방지한다.

**왜 만료 시간을 분산하는가?**

리더 변경 후 모든 Lease의 만료 시각이 비슷하면, 짧은 시간에 수천 개의 Lease가 동시 만료된다. 이는 시스템에 과부하를 준다. `targetExpiresPerSecond = (3 * leaseRevokeRate) / 4`로 설정하여 리보크 레이트의 75%만 사용하고, 나머지 25%는 일반적인 만료 처리에 예약한다.

```
만료 분산 알고리즘:

1. Lease를 만료 시간순 정렬
2. 1초 윈도우 내의 Lease 수를 카운트
3. targetExpiresPerSecond 초과 시:
   ├── rateDelay 계산
   └── l.refresh(delay + extend)로 만료 시간 연장
4. 결과: 초당 만료 Lease 수가 targetExpiresPerSecond 이하로 유지
```

### Demote(): Secondary로 강등

```
Demote() 흐름:
1. mu.Lock()
2. 모든 Lease의 expiry를 forever로 설정
3. 체크포인트 힙 초기화
4. 만료 알림 힙 초기화
5. demotec 채널 close → 대기 중인 Renew에 알림
6. demotec = nil → Secondary 상태 표시
```

### isPrimary() 판별

```go
func (le *lessor) isPrimary() bool {
    return le.demotec != nil
}
```

`demotec` 채널이 nil이 아니면 Primary 상태이다. 이 채널은 Promote에서 생성되고 Demote에서 닫힌다.

---

## LeaseExpiredNotifier: 만료 힙 기반 알림

`server/lease/lease_queue.go`에 정의된 `LeaseExpiredNotifier`는 최소 힙(min-heap)으로 가장 빨리 만료되는 Lease를 효율적으로 찾는다.

### LeaseWithTime 구조체

```go
type LeaseWithTime struct {
    id    LeaseID
    time  time.Time  // 만료 또는 체크포인트 시각
    index int        // 힙 내 위치 (heap.Fix에 필요)
}
```

### LeaseExpiredNotifier 구조체

```
LeaseExpiredNotifier:
├── m     map[LeaseID]*LeaseWithTime  // ID → 힙 항목 매핑
└── queue LeaseQueue                   // 최소 힙
```

### 핵심 연산

| 연산 | 시간 복잡도 | 설명 |
|------|-----------|------|
| RegisterOrUpdate | O(log N) | 새 항목 추가 또는 기존 항목의 시간 갱신 |
| Unregister | O(log N) | 힙 최상위 항목 제거 |
| Peek | O(1) | 가장 빨리 만료되는 항목 조회 |
| Len | O(1) | 항목 수 조회 |

**왜 map과 heap을 함께 사용하는가?**

힙만으로는 특정 ID의 항목을 O(1)로 찾을 수 없다. 맵을 통해 ID로 항목을 빠르게 찾고, `heap.Fix()`로 힙 속성을 유지한다. `RegisterOrUpdate()`에서 기존 항목이 있으면 시간만 갱신하고 Fix를 호출하여 O(log N) 재배치한다.

---

## LeaseQueue: 체크포인트 힙

`LeaseQueue`는 `[]*LeaseWithTime` 슬라이스 기반의 최소 힙으로, `container/heap` 인터페이스를 구현한다.

```go
type LeaseQueue []*LeaseWithTime

func (pq LeaseQueue) Less(i, j int) bool {
    return pq[i].time.Before(pq[j].time)  // 시간 기준 정렬
}
```

이 힙은 두 가지 용도로 사용된다:
1. **LeaseExpiredNotifier 내부**: 만료 시각 기준 정렬
2. **leaseCheckpointHeap**: 다음 체크포인트 시각 기준 정렬

---

## itemMap: 키 → Lease 매핑

```go
itemMap map[LeaseItem]LeaseID
```

`itemMap`은 역색인으로, 키가 어떤 Lease에 속하는지 O(1)로 조회한다.

### Attach(): 키를 Lease에 연결

```go
func (le *lessor) Attach(id LeaseID, items []LeaseItem) error {
    le.mu.Lock()
    defer le.mu.Unlock()
    l := le.leaseMap[id]
    if l == nil {
        return ErrLeaseNotFound
    }
    l.mu.Lock()
    for _, it := range items {
        l.itemSet[it] = struct{}{}
        le.itemMap[it] = id
    }
    l.mu.Unlock()
    return nil
}
```

Lease의 `itemSet`과 Lessor의 `itemMap` 양쪽에 동시에 추가한다.

### Detach(): 키를 Lease에서 분리

```go
func (le *lessor) Detach(id LeaseID, items []LeaseItem) error {
    // Attach의 역연산: itemSet과 itemMap에서 동시 삭제
}
```

### GetLease(): 키의 Lease 조회

```go
func (le *lessor) GetLease(item LeaseItem) LeaseID {
    le.mu.RLock()
    id := le.itemMap[item]
    le.mu.RUnlock()
    return id
}
```

---

## 만료 감지 루프와 expiredC 채널

### runLoop(): 메인 루프

```go
func (le *lessor) runLoop() {
    defer close(le.doneC)
    delayTicker := time.NewTicker(500 * time.Millisecond)
    defer delayTicker.Stop()
    for {
        le.revokeExpiredLeases()
        le.checkpointScheduledLeases()
        select {
        case <-delayTicker.C:
        case <-le.stopC:
            return
        }
    }
}
```

500ms마다 두 가지 작업을 수행한다:
1. 만료된 Lease 검출 및 채널 전송
2. 예정된 체크포인트 실행

### revokeExpiredLeases(): 만료 Lease 검출

```
revokeExpiredLeases() 흐름:
1. revokeLimit = leaseRevokeRate / 2
   └── 500ms 주기이므로 초당 레이트의 절반
2. mu.RLock()
3. Primary인 경우에만 findExpiredLeases(revokeLimit) 호출
4. mu.RUnlock()
5. 만료된 Lease가 있으면:
   └── select:
       ├── le.expiredC <- ls  → 성공적으로 전송
       └── default            → 수신자가 바쁘면 다음 주기에 재시도
```

### findExpiredLeases(): 만료 Lease 탐색

```
findExpiredLeases(limit) 흐름:
반복:
1. expireExists() 호출:
   ├── 힙이 비어있으면 → (nil, false) → 루프 종료
   ├── 힙 최상위 항목의 Lease가 없으면 → Unregister → (nil, true) → 계속
   ├── 현재 시각이 만료 시각 이전이면 → (nil, false) → 루프 종료
   └── 만료됨:
       ├── 재확인 시간 설정 (now + expiredLeaseRetryInterval)
       └── (l, false) 반환
2. l.expired() 확인
3. 만료된 Lease를 결과에 추가
4. limit에 도달하면 중단
```

**왜 expireExists()에서 재확인 시간을 설정하는가?**

만료가 감지된 Lease가 바로 리보크되지 않을 수 있다 (expiredC 채널이 가득 찬 경우). `expiredLeaseRetryInterval`(기본 3초) 후에 다시 확인하여, 같은 Lease를 매 500ms마다 불필요하게 검출하지 않도록 한다.

### expiredC 채널

```go
expiredC: make(chan []*Lease, 16)
```

버퍼 크기 16의 채널이다. etcdserver의 만료 처리 루프가 이 채널에서 Lease를 수신하여 RAFT를 통해 리보크를 합의한다. 버퍼가 있어서 Lessor의 검출 루프와 서버의 처리 루프가 디커플링된다.

---

## 체크포인트 메커니즘

### scheduleCheckpointIfNeeded()

```go
func (le *lessor) scheduleCheckpointIfNeeded(lease *Lease) {
    if le.cp == nil {
        return
    }
    if lease.getRemainingTTL() > int64(le.checkpointInterval.Seconds()) {
        heap.Push(&le.leaseCheckpointHeap, &LeaseWithTime{
            id:   lease.ID,
            time: time.Now().Add(le.checkpointInterval),
        })
    }
}
```

남은 TTL이 체크포인트 간격(기본 5분)보다 길 때만 스케줄링한다. TTL이 5분 이하인 Lease는 체크포인트 비용 대비 이득이 적다.

### checkpointScheduledLeases()

```
checkpointScheduledLeases() 흐름:
rate limit: leaseCheckpointRate / 2 (500ms 주기이므로 절반)
반복:
1. mu.Lock()
2. Primary인 경우 findDueScheduledCheckpoints(maxBatchSize=1000) 호출
3. mu.Unlock()
4. cp(ctx, LeaseCheckpointRequest{Checkpoints: cps}) 호출
   └── RAFT를 통해 체크포인트 전파
5. 배치가 maxBatchSize 미만이면 완료
```

### findDueScheduledCheckpoints()

```
findDueScheduledCheckpoints(limit) 흐름:
힙에서 반복:
1. 최상위 항목의 시간이 현재 이후이면 → 완료
2. 힙에서 Pop
3. Lease 조회 → 없으면 스킵
4. 이미 만료된 Lease → 스킵
5. remainingTTL = ceil(expiry - now)
6. remainingTTL >= ttl이면 스킵 (체크포인트 불필요)
7. 체크포인트 항목 추가: {ID, Remaining_TTL}
```

**왜 remainingTTL >= ttl이면 스킵하는가?**

최근에 Renew되어 전체 TTL에 가까운 남은 시간을 가진 Lease는 체크포인트할 필요가 없다. 리더 변경 시 전체 TTL을 사용해도 큰 오차가 없기 때문이다.

---

## Recover(): 백엔드에서 Lease 복원

```
Recover(b, rd) 흐름:
1. mu.Lock()
2. 백엔드와 RangeDeleter 설정
3. leaseMap과 itemMap 초기화
4. initAndRecover() 호출
5. mu.Unlock()
```

### initAndRecover()

```
initAndRecover() 흐름:
1. BatchTx().LockOutsideApply()
2. Lease 버킷 생성 (없으면)
3. 모든 Lease 레코드 로드: MustUnsafeGetAllLeases(tx)
4. tx.Unlock()
5. 각 Lease 레코드에 대해:
   ├── TTL < minLeaseTTL이면 상향 조정
   └── Lease 객체 생성:
       ├── ID, ttl 설정
       ├── itemSet = empty map (나중에 KV 순회로 채움)
       ├── expiry = forever (Promote에서 갱신)
       ├── revokec = make(chan struct{})
       └── remainingTTL 설정 (체크포인트된 값)
6. leaseExpiredNotifier.Init()
7. heap.Init(&leaseCheckpointHeap)
8. b.ForceCommit()
```

**왜 itemSet은 비어있는 상태로 복원하는가?**

Lease에 연결된 키 목록은 백엔드에 저장하지 않는다. 서버 시작 시 KV 스토어의 모든 키를 순회하면서 Lease가 연결된 키를 찾아 `Attach()`한다. 이렇게 하면 Lease 영속화 크기를 최소화할 수 있다.

---

## leaseRevokeRate: 초당 리보크 제한

```go
defaultLeaseRevokeRate = 1000  // 초당 최대 1000개 리보크
```

### 레이트 제한의 적용

| 위치 | 제한값 | 설명 |
|------|--------|------|
| `revokeExpiredLeases()` | `leaseRevokeRate / 2` | 500ms 주기이므로 절반 |
| `Promote()` pile-up 방지 | `(3 * leaseRevokeRate) / 4` | 전체의 75%만 사용 |
| `checkpointScheduledLeases()` | `leaseCheckpointRate / 2` | 체크포인트도 레이트 제한 |

**왜 레이트 제한이 필요한가?**

수만 개의 Lease가 동시에 만료되면, 리보크 처리가 RAFT 로그를 폭발적으로 증가시키고 시스템 전체에 부하를 준다. 레이트 제한으로 초당 처리량을 제어하여 시스템 안정성을 유지한다.

---

## 주요 상수와 기본값

| 상수 | 값 | 설명 |
|------|-----|------|
| `NoLease` | 0 | Lease 없음 표시 |
| `MaxLeaseTTL` | 9,000,000,000 (~285년) | 최대 허용 TTL |
| `defaultLeaseRevokeRate` | 1,000 | 초당 리보크 수 |
| `leaseCheckpointRate` | 1,000 | 초당 체크포인트 수 |
| `defaultLeaseCheckpointInterval` | 5분 | 체크포인트 주기 |
| `maxLeaseCheckpointBatchSize` | 1,000 | 배치당 체크포인트 수 |
| `defaultExpiredleaseRetryInterval` | 3초 | 만료 재확인 주기 |
| `expiredC` 버퍼 크기 | 16 | 만료 채널 버퍼 |

---

## 전체 Lease 생명주기 다이어그램

```
┌─────────┐    Grant()     ┌──────────┐   클라이언트   ┌──────────┐
│ 클라이언트│──────────────→│  lessor   │◄──KeepAlive──│  클라이언트│
└─────────┘               └────┬─────┘              └──────────┘
                               │
                    ┌──────────┼──────────┐
                    │          │          │
               ┌────▼───┐ ┌───▼────┐ ┌───▼────┐
               │leaseMap│ │itemMap │ │expiredC│
               │(저장)  │ │(역색인)│ │(알림)  │
               └────┬───┘ └───┬────┘ └───┬────┘
                    │         │          │
               ┌────▼─────────▼──┐  ┌────▼────────┐
               │  Backend(BoltDB)│  │  etcdserver  │
               │  (영속 저장)     │  │  (RAFT 리보크)│
               └─────────────────┘  └──────────────┘

Lease 상태 전이:
  생성 ──→ 활성 ──→ 만료감지 ──→ RAFT제안 ──→ 리보크 ──→ 삭제
           │  ↑
           │  │
           └──┘
         Renew(갱신)
```

---

## Lease와 Raft의 상호작용

```
┌──────────────────────────────────────────────────────┐
│                     Lease 연산과 RAFT                  │
├──────────────┬────────────────┬───────────────────────┤
│ 연산          │ RAFT 통과 여부  │ 이유                  │
├──────────────┼────────────────┼───────────────────────┤
│ Grant        │ O (합의)       │ 모든 노드에 Lease 생성 │
│ Revoke       │ O (합의)       │ 키 삭제과 함께 원자적  │
│ Checkpoint   │ O (합의)       │ remainingTTL 전파     │
│ Renew        │ X (리더 로컬)  │ 시간 기반, 합의 불필요 │
│ 만료 감지     │ X (리더 로컬)  │ 리더만 감지           │
│ 만료→리보크   │ O (합의)       │ 키 삭제 필요          │
└──────────────┴────────────────┴───────────────────────┘
```

**왜 Renew는 RAFT를 통과하지 않는가?**

Renew는 단순히 로컬의 만료 시각만 갱신하면 된다. 만료 관리는 Primary만 하므로, Primary가 로컬에서 갱신하면 충분하다. RAFT를 통과시키면 모든 Renew마다 합의가 필요해서 성능이 크게 저하된다. 체크포인트 메커니즘이 주기적으로 남은 TTL을 전파하여 보완한다.

---

## FakeLessor: 테스트 지원

소스코드에는 `FakeLessor` 구현체가 포함되어 있다. 모든 메서드가 no-op이거나 간단한 맵 연산만 수행하며, 단위 테스트에서 Lessor 의존성을 제거하는 데 사용된다.

```go
type FakeLessor struct {
    LeaseSet map[LeaseID]struct{}
}
```

---

## 소스 파일 참조

| 파일 경로 | 핵심 내용 |
|----------|----------|
| `server/lease/lessor.go` | Lessor 인터페이스, lessor 구현체, Grant/Revoke/Renew/Promote/Demote |
| `server/lease/lease.go` | Lease 구조체, LeaseItem, refresh/forever/expired |
| `server/lease/lease_queue.go` | LeaseQueue, LeaseWithTime, LeaseExpiredNotifier |
| `server/lease/leasepb/` | Lease protobuf 정의 (영속화 형식) |
| `server/storage/schema/` | Lease 버킷 스키마, UnsafePutLease, UnsafeDeleteLease |

---

## 정리

etcd Lease 시스템의 핵심 설계 원칙:

1. **Primary/Secondary 분리**: 만료 관리는 오직 Raft 리더(Primary)만 담당하여 일관성 보장
2. **체크포인트로 TTL 보존**: 주기적으로 남은 TTL을 RAFT를 통해 전파하여 리더 변경 시에도 정확한 만료 시간 유지
3. **레이트 제한**: 대량 만료 시 시스템 보호를 위한 초당 리보크 수 제한
4. **힙 기반 효율적 만료 감지**: O(log N) 삽입/삭제, O(1) 최소값 조회
5. **원자적 리보크**: 키 삭제와 Lease 삭제를 같은 트랜잭션에서 수행
6. **최소 영속화**: ID, TTL, remainingTTL만 저장하고 키 목록은 KV 순회로 복원
