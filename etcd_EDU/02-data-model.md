# etcd 데이터 모델

## 1. 개요

etcd의 데이터 모델은 **MVCC(Multi-Version Concurrency Control)** 기반이다. 모든 키-값 쌍은 리비전(Revision) 번호와 함께 저장되며, 과거 버전을 조회하거나 변경 이벤트를 추적할 수 있다. 이 모델은 Kubernetes의 ResourceVersion 메커니즘의 근간이 된다.

## 2. 핵심 개념

### 2.1 리비전 (Revision)

etcd의 리비전은 전역적으로 단조 증가하는 64비트 정수이다. 모든 트랜잭션(Put, Delete, Txn)마다 Main 리비전이 1씩 증가하고, 같은 트랜잭션 내 여러 연산은 Sub 리비전으로 구분한다.

```go
// server/storage/mvcc/revision.go
type Revision struct {
    Main int64    // 트랜잭션 번호 (전역 단조 증가)
    Sub  int64    // 트랜잭션 내 연산 순서
}
```

**바이너리 인코딩**: 17바이트 형식 `[Main(8바이트)] + ['_'] + [Sub(8바이트)]`

```
리비전 {Main:5, Sub:2}의 바이너리:
0x00 0x00 0x00 0x00 0x00 0x00 0x00 0x05 '_' 0x00 0x00 0x00 0x00 0x00 0x00 0x00 0x02

톰스톤(삭제) 마킹: 17바이트 리비전 + 't' (총 18바이트)
```

### 2.2 KeyValue 구조

`api/mvccpb/kv.proto`에 정의된 핵심 데이터 구조:

```protobuf
// api/mvccpb/kv.proto
message KeyValue {
    bytes key = 1;              // 키 (바이트 배열)
    int64 create_revision = 2;  // 최초 생성된 리비전
    int64 mod_revision = 3;     // 마지막 수정 리비전
    int64 version = 4;          // 현재 세대(generation) 내 버전
    bytes value = 5;            // 값 (바이트 배열)
    int64 lease = 6;            // 연결된 Lease ID (0이면 없음)
}
```

| 필드 | 의미 | 예시 |
|------|------|------|
| key | 키 바이트 | `/registry/pods/default/nginx` |
| create_revision | 이 키가 처음 만들어진 리비전 | 100 |
| mod_revision | 이 키가 마지막으로 수정된 리비전 | 150 |
| version | 삭제 후 재생성 시 1부터 다시 시작 | 3 (3번째 수정) |
| value | 값 바이트 | `{"apiVersion":"v1",...}` |
| lease | 연결된 Lease (만료 시 키도 삭제) | 7587848390832113682 |

### 2.3 Event 구조

```protobuf
// api/mvccpb/kv.proto
message Event {
    enum EventType {
        PUT = 0;
        DELETE = 1;
    }
    EventType type = 1;
    KeyValue kv = 2;        // 현재 KV 상태
    KeyValue prev_kv = 3;   // 변경 전 KV 상태 (옵션)
}
```

## 3. 다중 버전 관리

### 3.1 keyIndex와 generation

etcd는 각 키마다 `keyIndex` 구조로 모든 리비전 이력을 관리한다:

```go
// server/storage/mvcc/key_index.go
type keyIndex struct {
    key         []byte        // 키 바이트
    modified    Revision      // 최종 수정 리비전
    generations []generation  // 세대 배열 (삭제 후 재생성 시 새 세대)
}

type generation struct {
    ver     int64       // 이 세대 내 버전 카운터
    created Revision    // 세대 시작 리비전
    revs    []Revision  // 이 세대의 모든 리비전 목록
}
```

### 3.2 세대(Generation) 라이프사이클

```
시간 →

세대 0 (gen[0]):  Put@1 → Put@3 → Put@5 → Delete@7 (톰스톤)
                   v=1     v=2     v=3      세대 종료

세대 1 (gen[1]):  Put@10 → Put@12 → Delete@15 (톰스톤)
                   v=1      v=2       세대 종료

세대 2 (gen[2]):  Put@20 → Put@22
                   v=1      v=2    (현재 세대, 아직 살아있음)
```

**동작 원리:**
- `Put`: 현재 세대의 `revs`에 리비전 추가, `ver++`
- `Delete`: 현재 세대에 톰스톤 추가 → 새 세대 시작
- `Compact(rev)`: rev 이전의 오래된 리비전 제거, 빈 세대 삭제

### 3.3 treeIndex (B-tree)

모든 keyIndex는 B-tree에 저장된다:

```go
// server/storage/mvcc/index.go
type treeIndex struct {
    sync.RWMutex
    tree *btree.BTree[*keyIndex]  // 정렬된 B-tree
}
```

**인터페이스:**
```go
type index interface {
    Get(key []byte, atRev int64) (rev, created Revision, ver int64, err error)
    Range(key, end []byte, atRev int64) ([][]byte, []Revision)
    Put(key []byte, rev Revision)
    Tombstone(key []byte, rev Revision) error
    Compact(rev int64) map[Revision]struct{}
}
```

## 4. 저장소 계층 구조

```
┌─────────────────────────────────────────────────┐
│              MVCC KV (store)                     │
│                                                  │
│  ┌──────────────┐     ┌──────────────────────┐  │
│  │  treeIndex   │     │    Backend (BoltDB)   │  │
│  │  (B-tree)    │     │                       │  │
│  │              │     │  ┌─────────────────┐  │  │
│  │ key → [revs] │     │  │  "key" bucket   │  │  │
│  │              │     │  │  rev → KV bytes  │  │  │
│  │  O(log N)    │     │  └─────────────────┘  │  │
│  │  검색/범위    │     │                       │  │
│  └──────────────┘     │  ┌─────────────────┐  │  │
│                       │  │ "meta" bucket   │  │  │
│                       │  │ consistentIndex │  │  │
│                       │  │ scheduledCompact│  │  │
│                       │  └─────────────────┘  │  │
│                       └──────────────────────┘  │
└─────────────────────────────────────────────────┘
```

### 4.1 BoltDB 버킷 구조

etcd는 BoltDB에 다음 버킷들을 사용한다:

| 버킷 | 키 형식 | 값 형식 | 용도 |
|------|--------|--------|------|
| `key` | 리비전 바이트 (17B) | marshaled KeyValue | KV 데이터 저장 |
| `meta` | 메타 키 이름 | 바이트 | 메타데이터 (consistentIndex 등) |
| `lease` | Lease ID | marshaled Lease | Lease 영속화 |
| `auth` | 인증 키 | marshaled auth data | 인증 정보 |
| `authUsers` | 사용자 이름 | marshaled User | 사용자 정보 |
| `authRoles` | 역할 이름 | marshaled Role | 역할 정보 |
| `members` | 멤버 ID | marshaled Member | 클러스터 멤버 |
| `members_removed` | 멤버 ID | 빈 값 | 제거된 멤버 |
| `cluster` | 클러스터 키 | 클러스터 정보 | 클러스터 메타 |
| `alarm` | 알람 ID | marshaled Alarm | 알람 상태 |

### 4.2 키-리비전 매핑

BoltDB의 `key` 버킷에서 키는 **리비전 바이트**이고 값은 **직렬화된 KeyValue**이다:

```
"key" 버킷:
┌──────────────────┬─────────────────────────────────┐
│ Key (리비전 바이트) │ Value (protobuf KeyValue)        │
├──────────────────┼─────────────────────────────────┤
│ {Main:1, Sub:0}  │ {key:"/foo", value:"bar", ...}  │
│ {Main:2, Sub:0}  │ {key:"/baz", value:"qux", ...}  │
│ {Main:3, Sub:0}  │ {key:"/foo", value:"baz", ...}  │ ← /foo 수정
│ {Main:4, Sub:0}t │ {key:"/foo", value:"", ...}     │ ← /foo 삭제 (톰스톤)
└──────────────────┴─────────────────────────────────┘
```

## 5. 트랜잭션 모델

### 5.1 읽기 트랜잭션

```go
// server/storage/mvcc/kv.go
type TxnRead interface {
    Range(ctx context.Context, key, end []byte, ro RangeOptions) (*RangeResult, error)
    FirstRev() int64
    Rev() int64
    End()
}
```

**Range 처리 흐름:**

```
Range(key="/foo", end="/foz", rev=5)
  │
  ├─ treeIndex.Range("/foo", "/foz", atRev=5)
  │   → B-tree에서 /foo ~ /foz 범위의 keyIndex 탐색
  │   → 각 keyIndex에서 rev ≤ 5인 최신 리비전 반환
  │   → 결과: [rev{3,0}, rev{2,0}]
  │
  └─ backend.UnsafeRange("key", rev_bytes)
      → BoltDB에서 리비전 바이트로 KeyValue 조회
      → protobuf Unmarshal
      → RangeResult 반환
```

### 5.2 쓰기 트랜잭션

```go
// server/storage/mvcc/kv.go
type TxnWrite interface {
    TxnRead
    Put(key, value []byte, lease lease.LeaseID) int64
    DeleteRange(key, end []byte) (n, rev int64)
    Changes() []mvccpb.KeyValue
    Rev() int64
    End()
}
```

**Put 처리 흐름:**

```
Put(key="/foo", value="bar", lease=0)
  │
  ├─ currentRev++ (새 리비전 할당)
  │
  ├─ KeyValue 생성
  │   ├─ create_revision: 새 키면 currentRev, 기존이면 유지
  │   ├─ mod_revision: currentRev
  │   ├─ version: 이전 버전 + 1
  │   └─ value: "bar"
  │
  ├─ treeIndex.Put("/foo", rev{currentRev, 0})
  │   → keyIndex의 현재 generation에 리비전 추가
  │
  ├─ backend.BatchTx.UnsafePut("key", rev_bytes, kv_bytes)
  │   → BoltDB 배치에 KV 저장
  │
  └─ changes에 KeyValue 추가 (Watch 이벤트용)
```

### 5.3 Compare-And-Swap (Txn)

etcd의 트랜잭션은 조건 평가 → 분기 실행 구조이다:

```protobuf
// api/etcdserverpb/rpc.proto
message TxnRequest {
    repeated Compare compare = 1;     // 조건들 (AND)
    repeated RequestOp success = 2;   // 모든 조건 참일 때
    repeated RequestOp failure = 3;   // 하나라도 거짓일 때
}

message Compare {
    enum CompareResult { EQUAL, GREATER, LESS, NOT_EQUAL }
    enum CompareTarget { VERSION, CREATE, MOD, VALUE, LEASE }
    CompareResult result = 1;
    CompareTarget target = 2;
    bytes key = 3;
    oneof target_union {
        int64 version = 4;
        int64 create_revision = 5;
        int64 mod_revision = 6;
        bytes value = 7;
        int64 lease = 8;
    }
}
```

**예시: 분산 잠금 구현**

```
Txn(
  Compare: [create_revision("/lock") == 0]    // 잠금이 없으면
  Success: [Put("/lock", "holder-1")]         // 잠금 획득
  Failure: [Range("/lock")]                   // 현재 잠금 소유자 조회
)
```

## 6. Lease와 키 연결

### 6.1 Lease 구조

```go
// server/lease/lessor.go
type Lease struct {
    ID           LeaseID
    ttl          int64                    // 초 단위 TTL
    remainingTTL int64                    // 체크포인트된 남은 TTL
    expiry       time.Time               // 만료 시간
    itemSet      map[LeaseItem]struct{}   // 연결된 키들
}

type LeaseItem struct {
    Key string
}
```

### 6.2 키-Lease 바인딩

```
1. LeaseGrant(TTL=30s) → LeaseID=123
2. Put("/session/abc", "data", lease=123)
   → Lessor.Attach(123, LeaseItem{Key:"/session/abc"})
   → KV의 lease 필드에 123 저장
3. 30초 후 Lease 만료
   → Lessor.Revoke(123)
   → 연결된 모든 키 삭제 (/session/abc)
   → Watch 이벤트 발생 (DELETE)
```

## 7. 컴팩션

### 7.1 리비전 컴팩션

컴팩션은 지정된 리비전 이전의 오래된 버전을 삭제한다:

```
컴팩션 전 (rev=5에서 컴팩션):
  treeIndex:
    /foo: gen[{rev:1, rev:3, rev:7}]
    /bar: gen[{rev:2, rev:4}]

  BoltDB "key" 버킷:
    rev{1,0} → /foo=v1
    rev{2,0} → /bar=v1
    rev{3,0} → /foo=v2
    rev{4,0} → /bar=v2
    rev{7,0} → /foo=v3

컴팩션 후:
  treeIndex:
    /foo: gen[{rev:3, rev:7}]   ← rev:1 제거, rev:3은 보존 (rev≤5 최신)
    /bar: gen[{rev:4}]          ← rev:2 제거, rev:4는 보존

  BoltDB "key" 버킷:
    rev{3,0} → /foo=v2          ← rev≤5 시점의 최신 보존
    rev{4,0} → /bar=v2
    rev{7,0} → /foo=v3
```

### 7.2 자동 컴팩션 모드

| 모드 | 설정 | 동작 |
|------|------|------|
| periodic | `--auto-compaction-retention=1h` | 1시간마다 현재 rev - 1시간 전 rev로 컴팩션 |
| revision | `--auto-compaction-retention=1000` | 1000 리비전마다 자동 컴팩션 |

## 8. 데이터 흐름 예시

### 8.1 Put → Watch 이벤트 흐름

```
1. Put("/app/config", "v2") at rev=10
   │
   ├─ MVCC store
   │   ├─ currentRev: 9 → 10
   │   ├─ treeIndex.Put("/app/config", {10,0})
   │   └─ backend.Put(rev_bytes, KV{key:"/app/config", value:"v2", mod_rev:10})
   │
   └─ watchableStore.notify()
       ├─ synced watchers에서 "/app/config" 매칭 검색
       │   ├─ keyWatchers["/app/config"] → watcher 목록
       │   └─ ranges.Stab("/app/config") → 범위 watcher 목록
       └─ 매칭된 watcher들에게 Event{PUT, KV} 전송
```

### 8.2 Range at Revision 조회

```
Range("/app/", "/app0", rev=8)
  │
  ├─ treeIndex.Range("/app/", "/app0", atRev=8)
  │   → /app/config: rev{7,0} (rev≤8에서 최신)
  │   → /app/name:   rev{5,0}
  │   → /app/port:   rev{3,0}
  │
  └─ 각 리비전으로 BoltDB에서 KV 조회
      → [KV{/app/config@7}, KV{/app/name@5}, KV{/app/port@3}]
```

## 9. 데이터 모델 비교

### etcd vs 일반 KV 저장소

| 항목 | 일반 KV | etcd MVCC |
|------|---------|-----------|
| 버전 관리 | 최신 값만 | 모든 버전 보존 (컴팩션 전) |
| 읽기 일관성 | Eventual/Strong | Linearizable/Serializable 선택 |
| 변경 감지 | 폴링 | Watch 이벤트 스트림 |
| 삭제 | 즉시 제거 | 톰스톤 마킹 + 컴팩션 시 제거 |
| 트랜잭션 | 단순 CAS | Compare → Success/Failure 분기 |
| 만료 | TTL per key | Lease로 키 그룹 만료 |
| 순서 | 없음 | 전역 리비전 순서 보장 |

## 10. 데이터 크기 제한

| 항목 | 제한 | 설정 |
|------|------|------|
| 키 크기 | 제한 없음 (권장 < 1KB) | - |
| 값 크기 | 기본 1.5MB | `--max-request-bytes` |
| 총 DB 크기 | 기본 2GB, 최대 8GB | `--quota-backend-bytes` |
| 트랜잭션 연산 수 | 기본 128 | `--max-txn-ops` |
| Watch 수 | 제한 없음 | - |
| Lease 수 | 제한 없음 | - |

## 11. 요약

```
┌──────────────────────────────────────────────────────────┐
│                  etcd 데이터 모델 요약                      │
├──────────────────────────────────────────────────────────┤
│                                                           │
│  키-값 쌍 + 리비전 = 다중 버전 관리                          │
│                                                           │
│  리비전: {Main, Sub} → 전역 순서 보장                       │
│  세대: Put/Delete 사이클마다 새 generation                   │
│  인덱스: B-tree (키 → 리비전 목록)                           │
│  저장: BoltDB (리비전 바이트 → KeyValue protobuf)           │
│                                                           │
│  트랜잭션: Compare → Success/Failure (원자적)               │
│  Watch: 리비전 기반 이벤트 스트리밍                           │
│  Lease: TTL 기반 키 그룹 만료                               │
│  컴팩션: 오래된 리비전 정리 (최신 버전 보존)                   │
│                                                           │
└──────────────────────────────────────────────────────────┘
```
