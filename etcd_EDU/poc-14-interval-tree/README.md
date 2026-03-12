# PoC-14: IntervalTree (범위 워처 관리)

## 개요

etcd의 IntervalTree 자료구조를 시뮬레이션한다. etcd는 Red-Black 트리 기반 IntervalTree를 사용하여 범위 워치(watch)를 관리하고, 키 이벤트 발생 시 Stab 쿼리로 해당 키를 포함하는 모든 워처를 효율적으로 찾는다.

## etcd 소스 참조

- `pkg/adt/interval_tree.go` - IntervalTree 인터페이스 및 Red-Black 트리 구현
- `server/storage/mvcc/watcher_group.go` - watcherGroup에서 IntervalTree 사용

## 핵심 개념

### Interval (구간)
- `[Begin, End)` 반개방 구간
- etcd 키 범위 매칭에 사용 (예: `/users/` ~ `/users0`)

### Stab 쿼리 (Stabbing Query)
- 주어진 점을 포함하는 모든 구간 반환
- 키 PUT/DELETE 이벤트 발생 시 매칭 워처 탐색에 사용

### 구현 차이
- etcd: Red-Black 트리 기반, O(log N + K) stab
- 이 PoC: 정렬 슬라이스 기반, O(N) stab

## 실행 방법

```bash
go run main.go
```

## 데모 내용

1. Interval 기본 연산 (Contains, Overlaps)
2. Stab 점 쿼리
3. 범위 워처 시나리오 (etcd Watch 패턴)
4. 워처 삭제
5. StabRange 범위 겹침 쿼리
6. Visit 패턴 (방문자 콜백)
