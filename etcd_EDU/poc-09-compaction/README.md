# PoC-09: 컴팩션 (Compaction)

## 개요

etcd MVCC 스토어의 리비전 기반 히스토리 압축(컴팩션)을 시뮬레이션한다.

etcd는 모든 키 변경을 리비전 히스토리로 보관하지만, 무한히 쌓이면 저장 공간이 고갈된다. `Compact(rev)` 연산으로 지정 리비전 이하의 오래된 버전을 제거하되, 각 키의 최신 버전은 보존한다.

## 핵심 개념

| 개념 | 설명 |
|------|------|
| Revision | Main(전역 트랜잭션 ID) + Sub(트랜잭션 내 순번)로 구성 |
| Generation | 키 생성~삭제까지의 수명 주기. 삭제(tombstone) 시 새 generation 생성 |
| keyIndex | 키별 generation 목록을 관리. 컴팩션의 핵심 단위 |
| 배치 처리 | CompactionBatchLimit 개씩 삭제하며 사이에 sleep으로 부하 분산 |
| ErrCompacted | 컴팩션된 리비전 조회 시 반환되는 에러 |

## etcd 소스코드 참조

- `server/storage/mvcc/kvstore_compaction.go` — `scheduleCompaction()`: 배치 단위 old revision 삭제
- `server/storage/mvcc/key_index.go` — `compact()`, `doCompact()`: generation별 보존 리비전 결정
- `server/storage/mvcc/kvstore.go` — `Compact()`: 컴팩션 진입점, 유효성 검증
- `server/storage/mvcc/index.go` — `treeIndex.Compact()`: 전체 인덱스 순회

## 실행 방법

```bash
go run main.go
```

## 데모 시나리오

1. 리비전 10개 생성 (foo 5번, bar 3번, baz 생성+삭제)
2. 컴팩션 전 히스토리 조회 확인
3. `Compact(5)` — 리비전 5 이하 압축, 배치 처리 시뮬레이션
4. 컴팩션 후 조회 → `ErrCompacted` 확인
5. 추가 `Compact(8)` 실행
6. 에러 케이스 (이중 컴팩션, 미래 리비전)
7. Generation 기반 컴팩션 동작 확인 (put→delete→put 흐름)
