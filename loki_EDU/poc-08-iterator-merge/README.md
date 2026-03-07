# PoC #08: 반복자 병합 - 다중 소스 로그 스트림의 정렬 병합

## 개요

Loki는 여러 소스(ingester, store, 각 청크)에서 로그를 읽어온 뒤, 시간 순서대로 정렬 병합하여 단일 스트림으로 제공한다. 이 PoC는 힙 기반 병합 이터레이터 패턴을 구현한다.

## 실제 Loki 코드와의 관계

| 이 PoC | Loki 실제 코드 |
|--------|---------------|
| `EntryIterator` 인터페이스 | `pkg/iter/iterator.go` |
| `SliceIterator` | `pkg/iter/entry_iterator.go` (listEntryIterator) |
| `HeapIterator` | `pkg/iter/entry_iterator.go` (heapIterator) |
| `TimeRangeIterator` | `pkg/iter/entry_iterator.go` 내 시간 필터링 |
| `LimitIterator` | `pkg/iter/entry_iterator.go` 내 제한 로직 |

## 구현된 이터레이터

### SliceIterator
메모리 내 엔트리 슬라이스를 순차적으로 순회하는 기본 이터레이터.

### HeapIterator
`container/heap`을 사용하여 K개의 정렬된 이터레이터를 병합한다.
- **Forward**: 최소 힙 (오래된 것 먼저)
- **Backward**: 최대 힙 (최신 것 먼저)
- 시간 복잡도: O(N * log(K))

### TimeRangeIterator
내부 이터레이터를 감싸서 지정된 시간 범위 내의 엔트리만 통과시킨다.

### LimitIterator
내부 이터레이터를 감싸서 최대 N개의 엔트리만 반환한다.

## 실행 방법

```bash
go run main.go
```

## 핵심 개념

### 힙 기반 병합 알고리즘
```
소스1: [t1, t4, t7]
소스2: [t2, t5, t8]     →  병합 결과: [t1, t2, t3, t4, t5, t6, t7, t8, t9]
소스3: [t3, t6, t9]

힙 동작:
1. 각 소스의 첫 번째 엔트리를 힙에 삽입
2. 힙에서 최소(Forward) / 최대(Backward) 꺼냄
3. 꺼낸 엔트리의 소스에서 다음 엔트리를 힙에 삽입
4. 반복
```

### 이터레이터 체이닝 (데코레이터 패턴)
```
SliceIterator → TimeRangeIterator → LimitIterator
                (시간 필터)          (수량 제한)
```

### Loki 쿼리 시 실제 데이터 흐름
```
Querier
├── Ingester Iterator (최근 데이터)
│   ├── ingester-1 iterator
│   ├── ingester-2 iterator
│   └── ingester-3 iterator
│   └── HeapIterator (병합)
├── Store Iterator (과거 데이터)
│   ├── chunk-iter-1
│   ├── chunk-iter-2
│   └── HeapIterator (병합)
└── HeapIterator (최종 병합)
```
