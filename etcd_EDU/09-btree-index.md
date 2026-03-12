# 09. B-tree 인덱스 Deep-Dive

## 개요

etcd MVCC 저장소는 키에서 리비전으로의 매핑을 인메모리 B-tree 인덱스로 관리한다. 실제 키-값 데이터는 BoltDB에 리비전 바이트를 키로 저장되므로, "특정 키의 최신 리비전이 무엇인가"를 빠르게 찾으려면 인메모리 인덱스가 필수이다. 이 문서에서는 B-tree 인덱스의 핵심 자료구조(`treeIndex`, `keyIndex`, `generation`, `Revision`)와 그 동작 원리를 소스코드 수준에서 분석한다.

소스코드 경로:
- `server/storage/mvcc/index.go` - treeIndex 구조체, index 인터페이스
- `server/storage/mvcc/key_index.go` - keyIndex, generation 구조체
- `server/storage/mvcc/revision.go` - Revision, BucketKey, 바이너리 인코딩

---

## 1. Revision 구조와 바이너리 인코딩

### 1.1 Revision 구조체

```go
// server/storage/mvcc/revision.go:35-42
type Revision struct {
    Main int64    // 원자적 변경 집합의 리비전
    Sub  int64    // 집합 내 개별 변경의 순서
}
```

etcd에서 리비전은 두 수준으로 구성된다:

| 필드 | 의미 | 예시 |
|------|------|------|
| `Main` | 트랜잭션 리비전. 하나의 트랜잭션에서 발생한 모든 변경은 같은 Main을 공유 | Txn에서 Put("a"), Put("b") → Main=5 |
| `Sub` | 트랜잭션 내 순서. 같은 Main 내에서 각 변경을 구분 | Put("a") → Sub=0, Put("b") → Sub=1 |

### 1.2 GreaterThan 비교

```go
// server/storage/mvcc/revision.go:44-52
func (a Revision) GreaterThan(b Revision) bool {
    if a.Main > b.Main {
        return true
    }
    if a.Main < b.Main {
        return false
    }
    return a.Sub > b.Sub
}
```

Main을 먼저 비교하고, 같으면 Sub을 비교한다. 이 순서 관계는 B-tree에서의 키 정렬, generation 내 리비전 순서 검증 등 전체 MVCC 시스템의 기초이다.

### 1.3 바이너리 인코딩 형식

```
revBytesLen = 8 + 1 + 8 = 17바이트

인코딩 형식 (일반 리비전):
┌───────────────┬─────┬───────────────┐
│   Main (8B)   │  _  │   Sub (8B)    │
│  big-endian   │     │  big-endian   │
└───────────────┴─────┴───────────────┘

인코딩 형식 (tombstone 리비전):
┌───────────────┬─────┬───────────────┬─────┐
│   Main (8B)   │  _  │   Sub (8B)    │  t  │
│  big-endian   │     │  big-endian   │     │
└───────────────┴─────┴───────────────┴─────┘
markedRevBytesLen = 17 + 1 = 18바이트
```

**왜 big-endian인가?**

BoltDB는 키를 바이트 순서(lexicographic order)로 정렬한다. Big-endian으로 인코딩하면 바이트 순서와 숫자 순서가 일치하므로, BoltDB의 Range 연산으로 리비전 범위 조회를 효율적으로 수행할 수 있다.

### 1.4 BucketKey - 확장된 리비전

```go
// server/storage/mvcc/revision.go:62-67
type BucketKey struct {
    Revision
    tombstone bool
}
```

`BucketKey`는 BoltDB 버킷의 키로 사용되는 확장 리비전이다. `tombstone` 필드가 true이면 해당 리비전이 삭제 마커임을 나타낸다.

### 1.5 인코딩/디코딩 함수

```go
// server/storage/mvcc/revision.go:79-81
func NewRevBytes() []byte {
    return make([]byte, revBytesLen, markedRevBytesLen)
    // 길이 17, 용량 18 → tombstone 마커 추가 시 재할당 없음
}

// server/storage/mvcc/revision.go:83-96
func BucketKeyToBytes(rev BucketKey, bytes []byte) []byte {
    binary.BigEndian.PutUint64(bytes, uint64(rev.Main))
    bytes[8] = '_'
    binary.BigEndian.PutUint64(bytes[9:], uint64(rev.Sub))
    if rev.tombstone {
        switch len(bytes) {
        case revBytesLen:
            bytes = append(bytes, markTombstone)   // 't' 추가
        case markedRevBytesLen:
            bytes[markBytePosition] = markTombstone
        }
    }
    return bytes
}
```

`NewRevBytes()`의 용량 설계: `cap=18`로 생성하여 tombstone 마커를 `append`할 때 슬라이스 재할당이 발생하지 않는다. 이는 고빈도 호출 경로에서의 GC 압박을 줄이기 위한 최적화이다.

### 1.6 디코딩 및 tombstone 감지

```go
// server/storage/mvcc/revision.go:98-117
func BytesToBucketKey(bytes []byte) BucketKey {
    if (len(bytes) != revBytesLen) && (len(bytes) != markedRevBytesLen) {
        panic(fmt.Sprintf("invalid revision length: %d", len(bytes)))
    }
    if bytes[8] != '_' {
        panic(fmt.Sprintf("invalid separator in bucket key: %q", bytes[8]))
    }
    main := int64(binary.BigEndian.Uint64(bytes[0:8]))
    sub := int64(binary.BigEndian.Uint64(bytes[9:]))
    return BucketKey{
        Revision:  Revision{Main: main, Sub: sub},
        tombstone: isTombstone(bytes),
    }
}

// server/storage/mvcc/revision.go:120-122
func isTombstone(b []byte) bool {
    return len(b) == markedRevBytesLen && b[markBytePosition] == markTombstone
}
```

tombstone 감지는 단순히 바이트 길이(18)와 마지막 바이트('t')를 확인한다.

---

## 2. index 인터페이스

### 2.1 인터페이스 정의

```go
// server/storage/mvcc/index.go:24-37
type index interface {
    Get(key []byte, atRev int64) (rev, created Revision, ver int64, err error)
    Range(key, end []byte, atRev int64) ([][]byte, []Revision)
    Revisions(key, end []byte, atRev int64, limit int) ([]Revision, int)
    CountRevisions(key, end []byte, atRev int64) int
    Put(key []byte, rev Revision)
    Tombstone(key []byte, rev Revision) error
    Compact(rev int64) map[Revision]struct{}
    Keep(rev int64) map[Revision]struct{}
    Equal(b index) bool

    Insert(ki *keyIndex)
    KeyIndex(ki *keyIndex) *keyIndex
}
```

각 메서드의 역할:

| 메서드 | 호출자 | 역할 |
|--------|--------|------|
| `Get` | rangeKeys, put | 단일 키의 리비전/생성리비전/버전 조회 |
| `Range` | deleteRange, readView | 키 범위의 키 목록과 리비전 조회 |
| `Revisions` | rangeKeys | 키 범위의 리비전 목록 조회 (limit 지원) |
| `CountRevisions` | rangeKeys (Count 모드) | 키 범위의 리비전 개수 조회 |
| `Put` | storeTxnWrite.put | 키에 새 리비전 등록 |
| `Tombstone` | storeTxnWrite.delete | 키에 삭제 마커 추가 |
| `Compact` | scheduleCompaction | 컴팩션 수행, 보존할 리비전 맵 반환 |
| `Keep` | hashByRev | 컴팩션 시 보존할 리비전 맵 계산 (실제 삭제 없음) |
| `Insert` | restoreIntoIndex | 복구 시 keyIndex 직접 삽입 |
| `KeyIndex` | restoreIntoIndex | 복구 시 기존 keyIndex 조회 |

### 2.2 Revisions() vs Range() 차이

```go
// Revisions: 리비전만 반환 (키 바이트 미포함), limit과 total 지원
func (ti *treeIndex) Revisions(key, end []byte, atRev int64, limit int) (revs []Revision, total int)

// Range: 키 바이트와 리비전 모두 반환, limit 미지원
func (ti *treeIndex) Range(key, end []byte, atRev int64) (keys [][]byte, revs []Revision)
```

| 특성 | Revisions() | Range() |
|------|-------------|---------|
| 반환값 | `[]Revision, int` | `[][]byte, []Revision` |
| 키 바이트 포함 | 아니오 | 예 |
| limit 지원 | 예 | 아니오 |
| total 카운트 | 예 (limit 초과분 포함) | 아니오 |
| 주 사용처 | `rangeKeys()` (읽기 조회) | `deleteRange()` (삭제 대상 조회) |

**왜 두 가지인가?**

`rangeKeys()`는 BoltDB에서 값을 조회할 때 리비전만 필요하고 키 바이트는 BoltDB 결과에 포함된다. 반면 `deleteRange()`는 삭제할 키 목록이 필요하므로 키 바이트가 반환되어야 한다.

---

## 3. treeIndex 구조체

### 3.1 구조 정의

```go
// server/storage/mvcc/index.go:39-43
type treeIndex struct {
    sync.RWMutex
    tree *btree.BTree[*keyIndex]
    lg   *zap.Logger
}
```

`treeIndex`는 Google의 B-tree 라이브러리 (`k8s.io/utils/third_party/forked/golang/btree`)를 사용한다. B-tree의 각 노드에는 `*keyIndex`가 저장되며, 키의 바이트 순서로 정렬된다.

### 3.2 B-tree 생성

```go
// server/storage/mvcc/index.go:45-52
func newTreeIndex(lg *zap.Logger) index {
    return &treeIndex{
        tree: btree.New(32, func(aki *keyIndex, bki *keyIndex) bool {
            return aki.Less(bki)
        }),
        lg: lg,
    }
}
```

B-tree의 차수(degree)가 32인 이유:
- 차수 32 → 각 내부 노드에 최소 32개, 최대 64개의 자식
- 적당히 넓은 팬아웃으로 트리 높이를 낮게 유지
- 캐시 친화적 (노드 크기가 CPU 캐시라인에 적합)

비교 함수: `keyIndex.Less()`를 사용하여 키의 바이트 순서로 정렬

```go
// server/storage/mvcc/key_index.go:314-316
func (ki *keyIndex) Less(bki *keyIndex) bool {
    return bytes.Compare(ki.key, bki.key) == -1
}
```

### 3.3 락 전략

`treeIndex`는 자체 `sync.RWMutex`를 임베딩한다:

```
읽기 연산 (Get, Range, Revisions, CountRevisions, Keep, Equal):
    → ti.RLock() / ti.RUnlock()
    → 여러 읽기가 동시에 가능

쓰기 연산 (Put, Tombstone, Insert):
    → ti.Lock() / ti.Unlock()
    → 읽기와 쓰기 상호배제

컴팩션 (Compact):
    → 특별한 패턴 (아래 상세)
```

---

## 4. treeIndex 핵심 메서드

### 4.1 Put()

```go
// server/storage/mvcc/index.go:54-66
func (ti *treeIndex) Put(key []byte, rev Revision) {
    keyi := &keyIndex{key: key}

    ti.Lock()
    defer ti.Unlock()
    okeyi, ok := ti.tree.Get(keyi)
    if !ok {
        keyi.put(ti.lg, rev.Main, rev.Sub)
        ti.tree.ReplaceOrInsert(keyi)
        return
    }
    okeyi.put(ti.lg, rev.Main, rev.Sub)
}
```

Put 흐름:

```
Put("mykey", Revision{Main:5, Sub:0})
    │
    ├── 임시 keyIndex{key: "mykey"} 생성
    ├── ti.Lock()
    ├── tree.Get(keyi) → B-tree에서 "mykey" 검색
    │
    ├── [키 없음]
    │   ├── keyi.put(5, 0) → 새 generation 생성
    │   └── tree.ReplaceOrInsert(keyi) → B-tree에 삽입
    │
    └── [키 있음]
        └── okeyi.put(5, 0) → 기존 keyIndex에 리비전 추가
```

### 4.2 Get()

```go
// server/storage/mvcc/index.go:68-80
func (ti *treeIndex) Get(key []byte, atRev int64) (modified, created Revision, ver int64, err error) {
    ti.RLock()
    defer ti.RUnlock()
    return ti.unsafeGet(key, atRev)
}

func (ti *treeIndex) unsafeGet(key []byte, atRev int64) (modified, created Revision, ver int64, err error) {
    keyi := &keyIndex{key: key}
    if keyi = ti.keyIndex(keyi); keyi == nil {
        return Revision{}, Revision{}, 0, ErrRevisionNotFound
    }
    return keyi.get(ti.lg, atRev)
}
```

Get은 단일 키의 특정 리비전에서의 상태를 조회한다:
- `modified`: 해당 리비전 이하에서 가장 최신의 수정 리비전
- `created`: 해당 generation의 생성 리비전
- `ver`: 버전 번호

### 4.3 Tombstone()

```go
// server/storage/mvcc/index.go:179-190
func (ti *treeIndex) Tombstone(key []byte, rev Revision) error {
    keyi := &keyIndex{key: key}

    ti.Lock()
    defer ti.Unlock()
    ki, ok := ti.tree.Get(keyi)
    if !ok {
        return ErrRevisionNotFound
    }

    return ki.tombstone(ti.lg, rev.Main, rev.Sub)
}
```

### 4.4 unsafeVisit() - 범위 순회

```go
// server/storage/mvcc/index.go:95-107
func (ti *treeIndex) unsafeVisit(key, end []byte, f func(ki *keyIndex) bool) {
    keyi, endi := &keyIndex{key: key}, &keyIndex{key: end}

    ti.tree.AscendGreaterOrEqual(keyi, func(item *keyIndex) bool {
        if len(endi.key) > 0 && !item.Less(endi) {
            return false    // end에 도달하면 중단
        }
        if !f(item) {
            return false    // 콜백이 false 반환하면 중단
        }
        return true
    })
}
```

이 함수는 `Range`, `Revisions`, `CountRevisions`의 공통 기반이다. B-tree의 `AscendGreaterOrEqual` 메서드를 사용하여 `key` 이상 `end` 미만의 모든 keyIndex를 순회한다.

**B-tree 검색 최적화**: `AscendGreaterOrEqual`은 먼저 `key`의 위치를 B-tree에서 O(log n)으로 찾은 후, 그 위치부터 순차적으로 순회한다. 전체 트리를 스캔하지 않으므로 키 범위가 작을수록 빠르다.

### 4.5 Revisions()

```go
// server/storage/mvcc/index.go:112-133
func (ti *treeIndex) Revisions(key, end []byte, atRev int64, limit int) (revs []Revision, total int) {
    ti.RLock()
    defer ti.RUnlock()

    if end == nil {
        // 단일 키 조회
        rev, _, _, err := ti.unsafeGet(key, atRev)
        if err != nil {
            return nil, 0
        }
        return []Revision{rev}, 1
    }

    ti.unsafeVisit(key, end, func(ki *keyIndex) bool {
        if rev, _, _, err := ki.get(ti.lg, atRev); err == nil {
            if limit <= 0 || len(revs) < limit {
                revs = append(revs, rev)
            }
            total++
        }
        return true
    })
    return revs, total
}
```

`end == nil`인 경우: 단일 키 조회로 최적화. B-tree 순회 없이 직접 Get.

`total`과 `limit`의 분리: `total`은 limit에 관계없이 전체 매칭 키 수를 반환한다. 클라이언트가 "전체 결과가 몇 개인지" 알 수 있게 한다 (페이지네이션 지원).

### 4.6 Range()

```go
// server/storage/mvcc/index.go:158-177
func (ti *treeIndex) Range(key, end []byte, atRev int64) (keys [][]byte, revs []Revision) {
    ti.RLock()
    defer ti.RUnlock()

    if end == nil {
        rev, _, _, err := ti.unsafeGet(key, atRev)
        if err != nil {
            return nil, nil
        }
        return [][]byte{key}, []Revision{rev}
    }

    ti.unsafeVisit(key, end, func(ki *keyIndex) bool {
        if rev, _, _, err := ki.get(ti.lg, atRev); err == nil {
            revs = append(revs, rev)
            keys = append(keys, ki.key)
        }
        return true
    })
    return keys, revs
}
```

`Range`는 키 바이트도 함께 반환한다는 점에서 `Revisions`와 다르다. `deleteRange`에서 삭제할 키 목록을 알아내기 위해 사용된다.

### 4.7 Compact()

```go
// server/storage/mvcc/index.go:192-214
func (ti *treeIndex) Compact(rev int64) map[Revision]struct{} {
    available := make(map[Revision]struct{})
    ti.lg.Info("compact tree index", zap.Int64("revision", rev))

    ti.Lock()
    clone := ti.tree.Clone()
    ti.Unlock()

    clone.Ascend(func(keyi *keyIndex) bool {
        ti.Lock()
        keyi.compact(ti.lg, rev, available)
        if keyi.isEmpty() {
            _, ok := ti.tree.Delete(keyi)
            if !ok {
                ti.lg.Panic("failed to delete during compaction")
            }
        }
        ti.Unlock()
        return true
    })
    return available
}
```

Compact의 특별한 락 패턴:

```
1. ti.Lock() → tree.Clone() → ti.Unlock()
   - B-tree를 복제 (shallow clone)
   - 복제 동안만 전체 락 보유

2. clone.Ascend(func(keyi) {
       ti.Lock()
       keyi.compact(...)     // 개별 keyIndex 컴팩션
       if keyi.isEmpty() {
           ti.tree.Delete(keyi)  // 빈 keyIndex 제거
       }
       ti.Unlock()
   })
   - 복제본을 순회하면서 원본을 수정
   - 각 keyIndex 처리 시에만 락 획득/해제
```

**왜 Clone + 개별 락인가?**

전체 트리를 한 번에 락을 잡고 컴팩션하면, 그 동안 모든 읽기/쓰기가 차단된다. 수십만 개의 키가 있으면 이 시간이 수 초에 달할 수 있다. 대신:
- Clone은 트리 구조만 얕은 복사하므로 빠름
- 복제본을 순회하면서 각 keyIndex 처리 시에만 짧게 락을 잡음
- 다른 읽기/쓰기는 keyIndex 처리 사이사이에 진행 가능

**Clone이 안전한 이유**: B-tree의 Clone은 구조적 공유(structural sharing)를 사용한다. 복제본과 원본은 같은 keyIndex 포인터를 공유하므로, 복제본에서 순회한 keyIndex는 원본 트리의 것과 동일하다.

### 4.8 Keep()

```go
// server/storage/mvcc/index.go:217-226
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

`Keep`은 `Compact`와 달리 트리를 수정하지 않는다. "만약 이 리비전에서 컴팩션을 수행한다면 어떤 리비전이 보존되어야 하는가"를 계산만 한다. `hashByRev()`에서 해시 계산 시 어떤 리비전을 포함해야 하는지 결정하는 데 사용된다.

---

## 5. keyIndex 구조체

### 5.1 구조 정의

```go
// server/storage/mvcc/key_index.go:73-77
type keyIndex struct {
    key         []byte       // 키 바이트
    modified    Revision     // 마지막 수정 리비전
    generations []generation // 세대 목록
}
```

`keyIndex`는 하나의 키에 대한 모든 리비전 이력을 관리한다. 핵심 개념은 "generation(세대)"이다.

### 5.2 keyIndex 예시

소스코드 주석에 있는 예시 (`key_index.go:28-72`):

```
키 "foo"에 대해:
  put(1.0); put(2.0); tombstone(3.0); put(4.0); tombstone(5.0)

결과 keyIndex:
  key:      "foo"
  modified: 5
  generations:
    {empty}                    ← 가장 최근 세대 (삭제 후 빈 상태)
    {4.0, 5.0(t)}             ← 두 번째 세대
    {1.0, 2.0, 3.0(t)}        ← 첫 번째 세대
```

시각화:

```
Generation 0 (최초):
  ┌─────────────────────────────────┐
  │ created: {1, 0}                 │
  │ ver: 3                          │
  │ revs: [{1,0}, {2,0}, {3,0}(t)] │
  │        put     put    tombstone │
  └─────────────────────────────────┘
      ↓ tombstone 후 새 generation 생성

Generation 1:
  ┌─────────────────────────────────┐
  │ created: {4, 0}                 │
  │ ver: 2                          │
  │ revs: [{4,0}, {5,0}(t)]        │
  │        put     tombstone        │
  └─────────────────────────────────┘
      ↓ tombstone 후 새 generation 생성

Generation 2 (현재):
  ┌─────────────────────────────────┐
  │ created: {0, 0}                 │
  │ ver: 0                          │
  │ revs: []    (empty)             │
  └─────────────────────────────────┘
```

---

## 6. generation 구조체

### 6.1 구조 정의

```go
// server/storage/mvcc/key_index.go:346-350
type generation struct {
    ver     int64      // 이 세대의 버전 수 (put 횟수)
    created Revision   // 이 세대의 첫 리비전 (키 생성 시점)
    revs    []Revision // 이 세대의 모든 리비전 목록
}
```

### 6.2 generation의 생명주기

```
1. 생성 (키가 처음 put되거나 tombstone 후 다시 put될 때)
   ┌──────────────────────────┐
   │ generation{              │
   │   ver: 1,                │
   │   created: {main, sub},  │
   │   revs: [{main, sub}]    │
   │ }                        │
   └──────────────────────────┘

2. 버전 추가 (같은 키에 put할 때)
   ┌──────────────────────────┐
   │ generation{              │
   │   ver: 2,                │ ← ver 증가
   │   created: {1, 0},       │ ← created 불변
   │   revs: [{1,0}, {2,0}]   │ ← 리비전 추가
   │ }                        │
   └──────────────────────────┘

3. 톰스톤 (키가 삭제될 때)
   ┌──────────────────────────┐
   │ generation{              │
   │   ver: 3,                │ ← ver 증가
   │   created: {1, 0},       │
   │   revs: [{1,0},{2,0},    │
   │          {3,0}]          │ ← tombstone 리비전 추가
   │ }                        │
   └──────────────────────────┘
   + 새 빈 generation 추가

4. 컴팩션으로 제거
   - 모든 리비전이 컴팩션 리비전 이하이고
   - 마지막 리비전이 tombstone이면
   → generation 삭제
```

### 6.3 isEmpty()

```go
// server/storage/mvcc/key_index.go:352
func (g *generation) isEmpty() bool { return g == nil || len(g.revs) == 0 }
```

nil 체크가 포함된 이유: `findGeneration()`이 nil을 반환할 수 있고, 호출자가 nil 체크 없이 `isEmpty()`를 호출할 수 있도록 하기 위함이다.

### 6.4 walk() - 역순 순회

```go
// server/storage/mvcc/key_index.go:359-368
func (g *generation) walk(f func(rev Revision) bool) int {
    l := len(g.revs)
    for i := range g.revs {
        ok := f(g.revs[l-i-1])    // 역순 (최신 → 과거)
        if !ok {
            return l - i - 1       // 중단 위치 반환
        }
    }
    return -1                      // 전체 순회 완료
}
```

**왜 역순인가?**

`walk`은 주로 `get()`에서 사용된다. `get(atRev)`는 `atRev` 이하의 가장 최신 리비전을 찾아야 하므로, 최신부터 역순으로 탐색하면 첫 번째로 조건을 만족하는 리비전이 답이다.

```
revs: [{1,0}, {3,0}, {5,0}, {7,0}]
atRev: 4

역순 순회:
  {7,0} → 7 > 4 → continue
  {5,0} → 5 > 4 → continue
  {3,0} → 3 <= 4 → 반환! (이것이 답)
```

---

## 7. keyIndex 핵심 메서드

### 7.1 put()

```go
// server/storage/mvcc/key_index.go:80-103
func (ki *keyIndex) put(lg *zap.Logger, main int64, sub int64) {
    rev := Revision{Main: main, Sub: sub}

    if !rev.GreaterThan(ki.modified) {
        lg.Panic("'put' with an unexpected smaller revision", ...)
    }

    if len(ki.generations) == 0 {
        ki.generations = append(ki.generations, generation{})
    }

    g := &ki.generations[len(ki.generations)-1]
    if len(g.revs) == 0 {
        keysGauge.Inc()          // 새 키 카운터 증가
        g.created = rev          // 세대의 생성 리비전 설정
    }
    g.revs = append(g.revs, rev)
    g.ver++
    ki.modified = rev
}
```

`put`이 panic하는 경우: 리비전은 항상 증가해야 한다. 이전 리비전으로 put을 시도하면 데이터 손상을 의미하므로 즉시 중단한다.

빈 generation 확인: `len(g.revs) == 0`이면 이 세대가 방금 시작된 것이다 (tombstone 후 첫 put). `keysGauge`를 증가시키고 `created`를 설정한다.

### 7.2 tombstone()

```go
// server/storage/mvcc/key_index.go:131-145
func (ki *keyIndex) tombstone(lg *zap.Logger, main int64, sub int64) error {
    if ki.isEmpty() {
        lg.Panic("'tombstone' got an unexpected empty keyIndex", ...)
    }
    if ki.generations[len(ki.generations)-1].isEmpty() {
        return ErrRevisionNotFound    // 이미 삭제된 키
    }
    ki.put(lg, main, sub)                               // tombstone 리비전 추가
    ki.generations = append(ki.generations, generation{}) // 새 빈 세대 추가
    keysGauge.Dec()                                       // 키 카운터 감소
    return nil
}
```

tombstone은 두 단계로 동작한다:
1. `put`으로 현재 세대에 tombstone 리비전을 추가
2. 새 빈 generation을 추가 (다음 put을 위한 준비)

마지막 세대가 이미 비어있으면 `ErrRevisionNotFound` 반환: 이는 이미 삭제된 키를 다시 삭제하려는 시도를 나타낸다.

### 7.3 get()

```go
// server/storage/mvcc/key_index.go:149-167
func (ki *keyIndex) get(lg *zap.Logger, atRev int64) (modified, created Revision, ver int64, err error) {
    if ki.isEmpty() {
        lg.Panic("'get' got an unexpected empty keyIndex", ...)
    }

    g := ki.findGeneration(atRev)
    if g.isEmpty() {
        return Revision{}, Revision{}, 0, ErrRevisionNotFound
    }

    n := g.walk(func(rev Revision) bool { return rev.Main > atRev })
    if n != -1 {
        return g.revs[n], g.created, g.ver - int64(len(g.revs)-n-1), nil
    }

    return Revision{}, Revision{}, 0, ErrRevisionNotFound
}
```

`get` 흐름:

```
get(atRev=4)
    │
    ├── findGeneration(4)
    │   → 리비전 4를 포함하는 세대 찾기
    │   → generation{created:{1,0}, revs:[{1,0},{2,0},{3,0(t)}]}
    │      3(t)은 tombstone → 4보다 작으므로 이 세대의 범위 밖
    │   → nil (이 세대의 tombstone이 4 이하이므로 키는 삭제 상태)
    │
    └── g.isEmpty() == true → ErrRevisionNotFound

get(atRev=6)
    │
    ├── findGeneration(6)
    │   → generation{created:{4,0}, revs:[{4,0},{5,0(t)}]}
    │      tombstone {5,0}의 Main=5 ≤ 6 이지만
    │      이것이 최신(마지막) 세대가 아니므로 tomb <= atRev → nil
    │
    └── nil (키 삭제됨)

get(atRev=4, 세대: {4,0, 5,0(t)})
    │
    ├── findGeneration(4)
    │   → generation{created:{4,0}, revs:[{4,0},{5,0(t)}]}
    │
    ├── g.walk(func(rev) { return rev.Main > 4 })
    │   → {5,0}: 5 > 4 → continue
    │   → {4,0}: 4 > 4 = false → 중단, n=0
    │
    └── return g.revs[0]={4,0}, g.created={4,0},
               ver = g.ver - (len(revs)-0-1) = 2 - 1 = 1
```

버전 계산식 해석:
- `g.ver`: 세대의 총 put 횟수 (tombstone 포함)
- `len(g.revs) - n - 1`: 찾은 리비전 이후의 리비전 수
- `g.ver - (len(g.revs) - n - 1)`: 찾은 리비전 시점의 버전

### 7.4 findGeneration()

```go
// server/storage/mvcc/key_index.go:291-312
func (ki *keyIndex) findGeneration(rev int64) *generation {
    lastg := len(ki.generations) - 1
    cg := lastg

    for cg >= 0 {
        if len(ki.generations[cg].revs) == 0 {
            cg--
            continue
        }
        g := ki.generations[cg]
        if cg != lastg {
            if tomb := g.revs[len(g.revs)-1].Main; tomb <= rev {
                return nil    // 이 세대의 tombstone이 rev 이하 → 키 삭제됨
            }
        }
        if g.revs[0].Main <= rev {
            return &ki.generations[cg]    // 이 세대의 첫 리비전이 rev 이하 → 이 세대
        }
        cg--
    }
    return nil
}
```

탐색 로직:

```
generations: [{1,0, 2,0, 3,0(t)}, {4,0, 5,0(t)}, {empty}]
rev: 4

cg=2: empty → skip
cg=1: {4,0, 5,0(t)}
  - cg != lastg(2): tomb=5.0, 5 <= 4? No → continue
  - g.revs[0].Main=4, 4 <= 4? Yes → return &generations[1]

rev: 6
cg=2: empty → skip
cg=1: {4,0, 5,0(t)}
  - cg != lastg(2): tomb=5.0, 5 <= 6? Yes → return nil (삭제됨)
```

**왜 최신 세대부터 역순인가?**

대부분의 조회는 최신 리비전에 대해 수행된다. 역순 탐색으로 가장 빈번한 케이스를 빨리 처리한다.

---

## 8. 컴팩션 관련 메서드

### 8.1 compact()

```go
// server/storage/mvcc/key_index.go:215-235
func (ki *keyIndex) compact(lg *zap.Logger, atRev int64, available map[Revision]struct{}) {
    if ki.isEmpty() {
        lg.Panic("'compact' got an unexpected empty keyIndex", ...)
    }

    genIdx, revIndex := ki.doCompact(atRev, available)

    g := &ki.generations[genIdx]
    if !g.isEmpty() {
        if revIndex != -1 {
            g.revs = g.revs[revIndex:]    // 이전 리비전 제거
        }
    }

    ki.generations = ki.generations[genIdx:]   // 이전 세대 제거
}
```

### 8.2 doCompact() - 컴팩션 핵심 로직

```go
// server/storage/mvcc/key_index.go:258-282
func (ki *keyIndex) doCompact(atRev int64, available map[Revision]struct{}) (genIdx int, revIndex int) {
    f := func(rev Revision) bool {
        if rev.Main <= atRev {
            available[rev] = struct{}{}   // 보존할 리비전으로 마킹
            return false
        }
        return true
    }

    genIdx, g := 0, &ki.generations[0]
    for genIdx < len(ki.generations)-1 {
        if tomb := g.revs[len(g.revs)-1].Main; tomb >= atRev {
            break    // 이 세대의 tombstone이 atRev 이상이면 중단
        }
        genIdx++
        g = &ki.generations[genIdx]
    }

    revIndex = g.walk(f)
    return genIdx, revIndex
}
```

컴팩션 예시:

```
compact(rev=2) on:
  generations: [{1,0, 2,0, 3,0(t)}, {4,0, 5,0(t)}, {empty}]

Step 1: 세대 찾기
  genIdx=0: tomb=3.0, 3 >= 2 → break → genIdx=0

Step 2: walk
  {3,0(t)}: 3 > 2 → continue
  {2,0}: 2 <= 2 → available[{2,0}] = {} → revIndex=1

Step 3: 잘라내기
  g.revs = g.revs[1:] = [{2,0}, {3,0(t)}]
  ki.generations = ki.generations[0:] = (변화 없음)

결과:
  generations: [{2,0, 3,0(t)}, {4,0, 5,0(t)}, {empty}]
  available: {{2,0}: {}}
```

```
compact(rev=4) on:
  generations: [{2,0, 3,0(t)}, {4,0, 5,0(t)}, {empty}]

Step 1: 세대 찾기
  genIdx=0: tomb=3.0, 3 >= 4? No → genIdx++
  genIdx=1: tomb=5.0, 5 >= 4? Yes → break → genIdx=1

Step 2: walk
  {5,0(t)}: 5 > 4 → continue
  {4,0}: 4 <= 4 → available[{4,0}] = {} → revIndex=0

Step 3: 잘라내기
  g.revs = g.revs[0:] = [{4,0}, {5,0(t)}] (변화 없음)
  ki.generations = ki.generations[1:] = [{4,0, 5,0(t)}, {empty}]

결과:
  generations: [{4,0, 5,0(t)}, {empty}]
  available: {{4,0}: {}}
```

```
compact(rev=5) on:
  generations: [{4,0, 5,0(t)}, {empty}]

Step 1: genIdx=0, tomb=5.0, 5 >= 5 → break

Step 2: walk
  {5,0(t)}: 5 <= 5 → available[{5,0}] = {} → revIndex=1

Step 3:
  g.revs = g.revs[1:] = [{5,0(t)}]
  ki.generations = ki.generations[0:]

결과:
  generations: [{5,0(t)}, {empty}]
  available: {{5,0}: {}}
```

```
compact(rev=6) on:
  generations: [{5,0(t)}, {empty}]

Step 1: genIdx=0, tomb=5.0, 5 >= 6? No → genIdx=1
  genIdx=1: empty → walk returns -1

Step 3:
  g is empty → no revs to trim
  ki.generations = ki.generations[1:] = [{empty}]

결과:
  generations: [{empty}]
  → ki.isEmpty() == true → treeIndex.Compact()에서 삭제
```

### 8.3 keep()

```go
// server/storage/mvcc/key_index.go:238-256
func (ki *keyIndex) keep(atRev int64, available map[Revision]struct{}) {
    if ki.isEmpty() {
        return
    }

    genIdx, revIndex := ki.doCompact(atRev, available)
    g := &ki.generations[genIdx]
    if !g.isEmpty() {
        // tombstone이면 available에서 제거
        if revIndex == len(g.revs)-1 && genIdx != len(ki.generations)-1 {
            delete(available, g.revs[revIndex])
        }
    }
}
```

`keep`과 `compact`의 차이:

| 특성 | compact() | keep() |
|------|-----------|--------|
| 트리 수정 | 예 (리비전/세대 제거) | 아니오 |
| available 맵 | 보존할 리비전 (BoltDB에 남겨야 할 것) | 같은 의미 |
| tombstone 처리 | 보존 (BoltDB에서 참조 가능해야) | 제거 (해시 계산에서 제외) |
| 용도 | 실제 컴팩션 | 해시 계산용 시뮬레이션 |

**tombstone 제거 이유**: `keep`은 `hashByRev`에서 사용된다. 컴팩션 후의 데이터 상태를 시뮬레이션하여 해시를 계산하는데, tombstone은 컴팩션 후 "키가 삭제됨"을 나타낼 뿐 실제 데이터가 아니므로 해시에서 제외해야 일관된 해시 값을 보장한다.

### 8.4 isEmpty()

```go
// server/storage/mvcc/key_index.go:284-286
func (ki *keyIndex) isEmpty() bool {
    return len(ki.generations) == 1 && ki.generations[0].isEmpty()
}
```

keyIndex가 비어있다 = 세대가 1개이고 그 세대의 리비전이 없다. 이는 모든 세대가 컴팩션으로 제거된 후의 상태이다. `treeIndex.Compact()`에서 이 keyIndex를 B-tree에서 삭제한다.

---

## 9. 복구 관련 메서드

### 9.1 restore()

```go
// server/storage/mvcc/key_index.go:105-117
func (ki *keyIndex) restore(lg *zap.Logger, created, modified Revision, ver int64) {
    if len(ki.generations) != 0 {
        lg.Panic("'restore' got an unexpected non-empty generations", ...)
    }

    ki.modified = modified
    g := generation{created: created, ver: ver, revs: []Revision{modified}}
    ki.generations = append(ki.generations, g)
    keysGauge.Inc()
}
```

`restore`는 BoltDB에서 읽은 데이터로 keyIndex를 재구성한다. `created`와 `modified`가 별도로 전달되는 이유: BoltDB의 KeyValue protobuf에 `CreateRevision`과 `ModRevision`이 별도 필드로 저장되어 있기 때문이다.

### 9.2 restoreTombstone()

```go
// server/storage/mvcc/key_index.go:122-126
func (ki *keyIndex) restoreTombstone(lg *zap.Logger, main, sub int64) {
    ki.restore(lg, Revision{}, Revision{main, sub}, 1)
    ki.generations = append(ki.generations, generation{})
    keysGauge.Dec()
}
```

tombstone 복구 시 `created`를 빈 Revision으로 설정하는 이유: 생성 리비전이 이미 컴팩션되어 알 수 없는 경우이다. tombstone만 남은 상태에서는 생성 리비전이 필요하지 않다.

`keysGauge.Dec()`: `restore`에서 Inc했지만, tombstone은 삭제된 키이므로 즉시 Dec한다.

---

## 10. since() 메서드

```go
// server/storage/mvcc/key_index.go:172-210
func (ki *keyIndex) since(lg *zap.Logger, rev int64) []Revision {
    since := Revision{Main: rev}
    var gi int
    // 시작 세대 찾기 (역순)
    for gi = len(ki.generations) - 1; gi > 0; gi-- {
        g := ki.generations[gi]
        if g.isEmpty() {
            continue
        }
        if since.GreaterThan(g.created) {
            break
        }
    }

    var revs []Revision
    var last int64
    for ; gi < len(ki.generations); gi++ {
        for _, r := range ki.generations[gi].revs {
            if since.GreaterThan(r) {
                continue
            }
            if r.Main == last {
                // 같은 Main의 이전 Sub 리비전을 최신으로 교체
                revs[len(revs)-1] = r
                continue
            }
            revs = append(revs, r)
            last = r.Main
        }
    }
    return revs
}
```

`since`는 특정 리비전 이후의 모든 변경을 반환한다. watch에서 사용된다.

같은 Main 리비전의 여러 Sub 리비전이 있을 때 마지막 것만 유지하는 이유: 외부에서 볼 때 같은 트랜잭션 내의 중간 상태는 의미가 없고, 최종 상태만 중요하기 때문이다.

---

## 11. B-tree 라이브러리 사용 방식

### 11.1 사용되는 B-tree 메서드

| 메서드 | 사용 위치 | 설명 |
|--------|----------|------|
| `btree.New(degree, less)` | `newTreeIndex()` | B-tree 생성 |
| `tree.Get(key)` | `Put`, `Tombstone`, `keyIndex` | 키 검색 |
| `tree.ReplaceOrInsert(item)` | `Put`, `Insert` | 삽입 또는 교체 |
| `tree.Delete(item)` | `Compact` | 항목 삭제 |
| `tree.AscendGreaterOrEqual(pivot, f)` | `unsafeVisit` | 범위 순회 |
| `tree.Ascend(f)` | `Compact`, `Keep`, `Equal` | 전체 순회 |
| `tree.Clone()` | `Compact` | 얕은 복제 |
| `tree.Len()` | `Equal` | 항목 수 |

### 11.2 제네릭 B-tree

```go
tree *btree.BTree[*keyIndex]
```

이 B-tree는 제네릭 타입 매개변수 `*keyIndex`를 사용한다. 비교 함수는 생성 시 전달된다:

```go
btree.New(32, func(aki *keyIndex, bki *keyIndex) bool {
    return aki.Less(bki)
})
```

### 11.3 검색 최적화

B-tree의 AscendGreaterOrEqual을 사용한 범위 검색:

```
B-tree (degree=32, 각 노드 32~64개 자식):

                        [m, z]
                       /   |   \
              [a, f, k]  [n, r]  [...]
             / | | | \
          [a] [c,d] [f,g,h] [i,j,k] [...]

AscendGreaterOrEqual("c"):
  1. 루트에서 "c" 위치 찾기 → 왼쪽 자식
  2. [a, f, k]에서 "c" 위치 찾기 → 두 번째 자식
  3. [c, d]에서 "c" 이상 순회 시작
  4. c → d → f → g → h → ... (순서대로)

시간 복잡도:
  - 위치 찾기: O(log n) (트리 높이 * 노드 내 이진검색)
  - 순회: O(k) (결과 수)
  - 전체: O(log n + k)
```

---

## 12. 정리: B-tree 인덱스 아키텍처

```
┌──────────────────────────────────────────────────────────────┐
│                       treeIndex                               │
│  ┌─────────────────────────────────────────────────────────┐  │
│  │                    B-tree (degree=32)                    │  │
│  │                                                          │  │
│  │              키 바이트 순서로 정렬                        │  │
│  │  ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌──────────┐   │  │
│  │  │keyIndex  │ │keyIndex  │ │keyIndex  │ │keyIndex  │   │  │
│  │  │key:"a"   │ │key:"b"   │ │key:"foo" │ │key:"z"   │   │  │
│  │  └────┬─────┘ └────┬─────┘ └────┬─────┘ └────┬─────┘   │  │
│  │       │            │            │            │          │  │
│  └───────┼────────────┼────────────┼────────────┼──────────┘  │
│          │            │            │            │             │
│          ▼            ▼            ▼            ▼             │
│  ┌──────────────────────────────────────────────────────┐    │
│  │                    keyIndex 내부                      │    │
│  │                                                       │    │
│  │  modified: {Main:7, Sub:0}                            │    │
│  │                                                       │    │
│  │  generations:                                         │    │
│  │   [0] {created:{1,0}, ver:3, revs:[{1,0},{2,0},{3,0t}]}  │
│  │   [1] {created:{5,0}, ver:2, revs:[{5,0},{7,0}]}     │    │
│  │   [2] {empty - 현재 활성 세대}                         │    │
│  │                                                       │    │
│  │  각 generation:                                       │    │
│  │   ┌────────────────────────────────┐                  │    │
│  │   │ created: 첫 put 시점의 리비전   │                  │    │
│  │   │ ver: put 횟수 (tombstone 포함)  │                  │    │
│  │   │ revs: 리비전 목록 (시간순)      │                  │    │
│  │   │   마지막이 't'이면 삭제됨       │                  │    │
│  │   └────────────────────────────────┘                  │    │
│  └───────────────────────────────────────────────────────┘    │
│                                                               │
│  RWMutex: 읽기/쓰기 동시성 제어                               │
│  - RLock: Get, Range, Revisions, Keep                         │
│  - Lock: Put, Tombstone, Compact, Insert                      │
└──────────────────────────────────────────────────────────────┘

Revision 바이너리 인코딩 (BoltDB 키):
┌───────────┬───┬───────────┬─────┐
│ Main (8B) │ _ │ Sub (8B)  │ [t] │
│ big-end   │   │ big-end   │     │
└───────────┴───┴───────────┴─────┘
  17바이트 (일반) / 18바이트 (tombstone)
```

핵심 설계 원칙:
1. **인메모리 인덱스 + 디스크 저장소 분리**: 키 검색은 메모리에서, 값 저장은 BoltDB에서
2. **Generation 기반 이력 관리**: 삭제/재생성 사이클을 세대로 구분하여 깔끔한 이력 추적
3. **리비전 기반 정렬**: Big-endian 인코딩으로 BoltDB의 바이트 순서 정렬과 일치
4. **효율적 컴팩션**: Clone + 개별 락으로 컴팩션 중에도 읽기/쓰기 가능
5. **Keep vs Compact 분리**: 해시 검증을 위한 시뮬레이션과 실제 컴팩션을 분리
