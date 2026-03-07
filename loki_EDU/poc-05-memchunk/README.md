# PoC 05: 인메모리 청크 (MemChunk)

## 개요

Loki Ingester에서 로그 데이터를 메모리에 보관하는 핵심 자료구조인 MemChunk를 시뮬레이션한다.
Append-only 쓰기, Head Block/Sealed Block 전환, Forward/Backward Iterator 패턴을 구현한다.

## 시뮬레이션하는 Loki 컴포넌트

| 컴포넌트 | Loki 실제 위치 | 설명 |
|----------|---------------|------|
| MemChunk | `pkg/chunkenc/memchunk.go` | 메모리 내 청크 자료구조 |
| Chunk Interface | `pkg/chunkenc/interface.go` | Append/Iterator/Bounds/Size |
| HeadBlock | `pkg/chunkenc/memchunk.go` | 비압축 쓰기 버퍼 |
| EntryIterator | `pkg/iter/iterator.go` | 반복자 인터페이스 |

## MemChunk 구조

```
MemChunk
├── blocks[]     ← sealed blocks (읽기 전용)
│   ├── Block 0: [entry, entry, entry, ...]
│   ├── Block 1: [entry, entry, ...]
│   └── Block N: [entry, entry, entry, ...]
│
└── head         ← 현재 쓰기 중인 블록
    └── HeadBlock: [entry, entry, ...]
                    │
                    ▼ (크기 임계값 초과 시)
                   cut → 새 sealed Block 추가
```

## 시나리오

1. **기본 Append 및 Block Cut**: 엔트리 추가 시 head block → sealed block 전환
2. **Forward Iterator**: 시간순 정방향 순회
3. **Backward Iterator**: 역시간순 역방향 순회
4. **시간 범위 필터링**: 특정 구간의 엔트리만 조회
5. **Out-of-Order 거부**: 시간순 위반 엔트리 거부
6. **청크 크기 초과/Close**: 최대 크기 제한 및 Close 후 읽기만 가능
7. **Multi-Chunk 스트림**: 여러 청크를 연결하여 하나의 스트림 구성

## 실행 방법

```bash
go run main.go
```

## 학습 포인트

- Append-only 구조가 쓰기 성능을 극대화하는 이유
- Head Block을 별도로 유지하는 이유 (쓰기 지연 최소화)
- Iterator 패턴으로 다양한 읽기 시나리오를 통합하는 방법
- Bounds()가 O(1)인 이유 (min/max 타임스탬프 추적)
- 스트림이 청크 목록으로 구성되는 구조
- Out-of-order 엔트리를 거부하여 정렬 비용을 회피하는 설계
