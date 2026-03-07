# PoC 04: 청크 인코딩 (Chunk Encoding)

## 개요

Loki의 로그 청크 압축/인코딩 메커니즘을 시뮬레이션한다.
로그 데이터를 청크 단위로 묶고, 블록 기반 압축과 CRC32 체크섬으로 저장하는 과정을 구현한다.

## 시뮬레이션하는 Loki 컴포넌트

| 컴포넌트 | Loki 실제 위치 | 설명 |
|----------|---------------|------|
| MemChunk | `pkg/chunkenc/memchunk.go` | 메모리 내 청크 |
| HeadBlock | `pkg/chunkenc/memchunk.go` | 비압축 쓰기 버퍼 |
| Block | `pkg/chunkenc/memchunk.go` | 압축된 로그 블록 |
| Chunk Interface | `pkg/chunkenc/interface.go` | 청크 인터페이스 |

## 청크 바이너리 포맷

```
Offset  Size    Field
──────  ──────  ────────────────────────
0       4       Magic Number (0x4C4F4B49 = "LOKI")
4       1       Version (1)
5       1       Encoding (0=none, 1=gzip)
6       var     Block 0 data (compressed)
        var     Block 1 data (compressed)
        ...     Block N data (compressed)
        4       Number of blocks
        N*32    Block metadata (offset, length, entries, mint, maxt, checksum)
        4       Metadata section offset
        4       Total CRC32 checksum
```

## 시나리오

1. **청크 생성**: 로그 엔트리를 추가하며 head block → 압축 block 전환
2. **바이너리 포맷**: 직렬화된 청크의 헤더/풋터/메타데이터 분석
3. **역직렬화**: 바이너리에서 청크 복원, 블록별 체크섬 검증
4. **압축 효율**: 원시 vs 압축 크기 비교
5. **변조 탐지**: 1바이트 변조 시 CRC32 불일치 감지

## 실행 방법

```bash
go run main.go
```

## 학습 포인트

- Head Block이 쓰기 성능을 보장하는 이유 (비압축 상태로 빠르게 append)
- 블록 크기 임계값에 따른 압축 전환(cut) 메커니즘
- CRC32 체크섬을 블록 단위와 전체 청크 단위로 이중 검증하는 이유
- 메타데이터 섹션을 통한 개별 블록 O(1) 접근
- gzip 압축이 로그 데이터에 효과적인 이유 (반복 패턴이 많아서)
