# PoC-02: B-tree 인덱스

## 핵심 개념

etcd의 treeIndex는 키에서 리비전으로의 매핑을 B-tree(google/btree 라이브러리)로 관리한다. 각 키는 `keyIndex` 구조체로 표현되며, 키의 생명주기를 `generation` 단위로 추적한다.

### keyIndex 구조

```
keyIndex {
    key:         "foo"
    modified:    5.0          ← 마지막 수정 리비전
    generations: [
        {ver:3, created:1.0, revs:[1.0, 2.0, 3.0(t)]},  ← 첫 세대
        {ver:2, created:4.0, revs:[4.0, 5.0(t)]},        ← 둘째 세대
        {empty}                                            ← 현재 (삭제 상태)
    ]
}
```

### generation (세대)

키의 한 생명주기를 나타낸다:
- 첫 번째 `put`에서 시작 (`created` 설정)
- 이후 `put`마다 리비전 추가, `ver++`
- `tombstone`으로 종료 → 새 빈 세대 추가

### 핵심 알고리즘: get(atRev)

1. `findGeneration(atRev)`: 해당 리비전이 속하는 세대를 역순 탐색
2. `generation.walk()`: 세대 내에서 역순으로 순회하며 `atRev` 이하인 첫 리비전 반환
3. 세대 간 갭(톰스톤 이후, 다음 put 이전)에서는 "키 없음" 반환

## 구현 설명

### B-tree 시뮬레이션

실제 etcd는 `google/btree.BTreeG[*keyIndex]`를 사용하지만, 이 PoC에서는 정렬된 슬라이스 + 이진 탐색(`sort.Search`)으로 동일한 O(log n) 검색 성능을 구현한다.

### Range 검색

etcd의 `treeIndex.Range(key, end, atRev)`를 구현:
- 접두사 검색: Range("app/", "app0", rev) → "app/"로 시작하는 모든 키
- 전체 검색: Range("", "", rev) → 모든 키

### Compact (컴팩션)

```
compact(2) 전: generations = [{1.0, 2.0, 3.0(t)}, {4.0, 5.0(t)}, {empty}]
compact(2) 후: generations = [{2.0, 3.0(t)}, {4.0, 5.0(t)}, {empty}]
```

오래된 리비전을 정리하되, atRev 이하에서 가장 큰 리비전 하나는 유지한다.

## 실제 소스코드 참조

| 파일 | 역할 |
|------|------|
| `server/storage/mvcc/key_index.go` | keyIndex, generation 구조체와 핵심 메서드 |
| `server/storage/mvcc/index.go` | treeIndex (google/btree 기반 B-tree) |
| `server/storage/mvcc/kvstore.go` | treeIndex를 사용하는 저장소 |

## 실행 방법

```bash
go run main.go
```

## 예상 출력

```
=== etcd B-tree 인덱스 시뮬레이션 ===

--- 시나리오 1: keyIndex 생명주기 ---
etcd 소스 주석의 예제 재현:
  put(1.0);put(2.0);tombstone(3.0);put(4.0);tombstone(5.0) on key "foo"

키 'foo' (최종수정: 5.0)
  세대[0]:   세대: 생성=1.0, 버전=3, 리비전=[1.0, 2.0, 3.0]
  세대[1]:   세대: 생성=4.0, 버전=2, 리비전=[4.0, 5.0]
  세대[2]: {비어있음}

--- 시나리오 2: 리비전별 조회 ---
  Get("foo", atRev=1) → 수정=1.0, 생성=1.0, 버전=1
  Get("foo", atRev=2) → 수정=2.0, 생성=1.0, 버전=2
  Get("foo", atRev=3) → 에러: 리비전을 찾을 수 없음
  Get("foo", atRev=4) → 수정=4.0, 생성=4.0, 버전=1
  Get("foo", atRev=5) → 에러: 리비전을 찾을 수 없음
  Get("foo", atRev=6) → 에러: 리비전을 찾을 수 없음

--- 시나리오 3: 다중 키 + Range 검색 ---
전체 키 (atRev=6):
  app/config: 수정=6.0, 생성=1.0, 버전=2
  app/name: 수정=2.0, 생성=2.0, 버전=1
  app/version: 수정=3.0, 생성=3.0, 버전=1
  db/host: 수정=4.0, 생성=4.0, 버전=1
  db/port: 수정=5.0, 생성=5.0, 버전=1

범위 검색 ["app/", "app0") (atRev=6):
  app/config: 수정=6.0, 버전=2
  app/name: 수정=2.0, 버전=1
  app/version: 수정=3.0, 버전=1
...
```
