# PoC-01: MVCC 저장소

## 핵심 개념

etcd의 MVCC(Multi-Version Concurrency Control) 저장소는 모든 키-값 변경을 리비전 단위로 관리한다. 데이터를 덮어쓰지 않고, 각 변경마다 새로운 리비전을 생성하여 과거 시점의 데이터를 조회할 수 있다.

### Revision 구조

```
Revision {
    Main int64  // 트랜잭션 번호 (단조 증가)
    Sub  int64  // 트랜잭션 내 변경 순서
}
```

- `Main`: 각 쓰기 트랜잭션마다 1씩 증가
- `Sub`: 하나의 트랜잭션 내에서 여러 키를 수정할 때 0, 1, 2... 순서로 증가

### BucketKey (리비전 키)

실제 etcd에서 BoltDB에 저장되는 키. 리비전 + 톰스톤 마커로 구성된다.

```
BucketKey = Revision(8바이트) + '_' + Sub(8바이트) [+ 't' (톰스톤)]
```

### 저장 구조

| 계층 | etcd 실제 | 이 PoC |
|------|----------|--------|
| 인덱스 | treeIndex (B-tree) | map[string][]int64 |
| 데이터 | BoltDB Key 버킷 | map[int64]KeyValue |
| 리비전 관리 | store.currentRev | MVCCStore.currentRev |

## 구현 설명

### Put 흐름

1. `currentRev++`로 새 리비전 생성
2. `BucketKey{Revision, Tombstone=false}` 저장
3. `KeyValue{Key, Value, CreateRevision, ModRevision, Version}` 저장
4. `keyIndex[key]`에 리비전 추가

### Get 흐름

1. `keyIndex[key]`에서 리비전 목록 조회
2. 지정된 리비전 이하에서 가장 최신 리비전 탐색
3. 톰스톤이면 "키 없음" 반환
4. 아니면 해당 리비전의 KeyValue 반환

### Delete 흐름

1. `currentRev++`로 새 리비전 생성
2. `BucketKey{Revision, Tombstone=true}` 저장 (톰스톤 마킹)
3. 실제 데이터는 삭제하지 않음 → 과거 리비전 조회 가능
4. Compaction이 오래된 리비전을 정리

## 실제 소스코드 참조

| 파일 | 역할 |
|------|------|
| `server/storage/mvcc/revision.go` | Revision, BucketKey 구조체 |
| `server/storage/mvcc/kvstore.go` | store 구조체, currentRev 관리 |
| `server/storage/mvcc/kvstore_txn.go` | put(), delete() 트랜잭션 처리 |
| `api/v3/mvccpb/kv.proto` | KeyValue protobuf 정의 |

## 실행 방법

```bash
go run main.go
```

## 예상 출력

```
=== etcd MVCC 저장소 시뮬레이션 ===

--- 시나리오 1: 기본 Put/Get ---
Put("name", "Alice") → 리비전: {Main:1, Sub:0}
Put("age", "30")     → 리비전: {Main:2, Sub:0}
Put("name", "Bob")   → 리비전: {Main:3, Sub:0}
Get("name", 최신)    → 값: "Bob", 버전: 2, 생성리비전: 1, 수정리비전: 3

--- 시나리오 2: 과거 리비전 조회 (Time Travel) ---
Get("name", rev=1) → 값: "Alice" (과거 시점)
Get("name", rev=3) → 값: "Bob" (최신 시점)
Get("age",  rev=1) → 존재: false (아직 생성 전)
Get("age",  rev=2) → 값: "30"

--- 시나리오 3: Delete (톰스톤) ---
Delete("name")       → 톰스톤 리비전: {Main:4, Sub:0}
Get("name", 최신)    → 존재: false (삭제됨)
Get("name", rev=3) → 값: "Bob" (삭제 전 시점)

--- 시나리오 4: 삭제 후 재생성 (새 세대) ---
Put("name", "Charlie") → 리비전: {Main:5, Sub:0}
Get("name", 최신)      → 값: "Charlie", 버전: 1 (새 세대, 버전 1부터 재시작)

--- 시나리오 5: 리비전 히스토리 ---
키 "name"의 전체 히스토리:
---------------------------------------------------------------------------
리비전            값          톰스톤      버전     생성Rev  수정Rev
---------------------------------------------------------------------------
{Main:1, Sub:0} Alice      false      1        1        1
{Main:3, Sub:0} Bob        false      2        1        3
{Main:4, Sub:0}            TRUE       3        1        4
{Main:5, Sub:0} Charlie    false      1        5        5

--- 시나리오 6: 다중 키 + 현재 리비전 ---
현재 리비전: 9
server/addr = "10.0.0.2" (수정리비전: 8)
server/port = "8080" (수정리비전: 7)

=== MVCC 저장소 핵심 원리 ===
1. 모든 변경은 리비전(Revision)을 생성 — 데이터를 덮어쓰지 않음
2. 삭제는 톰스톤 마킹 — 과거 데이터는 보존됨
3. 과거 리비전 조회 가능 — Time Travel 쿼리
4. 삭제 후 재생성 시 새 세대(generation) 시작
5. Compaction이 오래된 리비전을 정리할 때까지 모든 히스토리 유지
```
