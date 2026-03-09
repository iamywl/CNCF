# 20. Chunks Inspector, Migration Tools, Loki Tool — 운영 도구 Deep Dive

## 목차

1. [개요](#1-개요)
2. [Chunks Inspector 아키텍처](#2-chunks-inspector-아키텍처)
3. [청크 바이너리 포맷](#3-청크-바이너리-포맷)
4. [Chunks Inspector 파싱 엔진](#4-chunks-inspector-파싱-엔진)
5. [Chunks Inspector 출력 분석](#5-chunks-inspector-출력-분석)
6. [Migration Tool 아키텍처](#6-migration-tool-아키텍처)
7. [Migration 실행 흐름](#7-migration-실행-흐름)
8. [ChunkMover 엔진](#8-chunkmover-엔진)
9. [Loki Tool 아키텍처](#9-loki-tool-아키텍처)
10. [도구 간 관계 및 워크플로우](#10-도구-간-관계-및-워크플로우)
11. [압축/인코딩 시스템](#11-압축인코딩-시스템)
12. [운영 가이드](#12-운영-가이드)

---

## 1. 개요

Loki는 프로덕션 운영을 위한 세 가지 전문 도구를 제공한다:

| 도구 | 소스 | 용도 |
|------|------|------|
| **Chunks Inspector** | `cmd/chunks-inspect/` | 청크 파일을 디코딩하여 내부 구조를 검사 |
| **Migration Tool** | `cmd/migrate/` | 서로 다른 Loki 스토리지 간 청크 마이그레이션 |
| **Loki Tool** | `cmd/lokitool/` | 규칙 관리 및 감사(audit) CLI |

### 왜 이 도구들이 필요한가

```
┌──────────────────────────────────────────────────────────────┐
│                    Loki 운영 생명주기                          │
│                                                              │
│  [저장]         [검사]              [마이그레이션]    [관리]    │
│  Ingester ──→  Chunks Inspector    Migration Tool   lokitool │
│  ↓                ↓                    ↓               ↓     │
│  Object Store  청크 구조 검증       스토리지 간 이동   규칙 관리│
│                체크섬 확인          테넌트 간 이동     감사     │
│                데이터 무결성        백엔드 변경                 │
└──────────────────────────────────────────────────────────────┘
```

---

## 2. Chunks Inspector 아키텍처

### 소스 코드 구조

```
cmd/chunks-inspect/
├── main.go     ← 진입점, CLI 파싱, 파일 처리 루프
├── header.go   ← 청크 헤더(메타데이터) 디코딩
├── loki.go     ← Loki 청크 포맷 파싱 (블록, 엔트리)
├── labels.go   ← 레이블 직렬화/역직렬화
├── time.go     ← model.Time 타입 처리
├── go.mod      ← 독립 모듈 (별도 의존성)
└── README.md
```

**왜 독립 모듈인가?** `go.mod` 파일이 별도로 존재한다. Chunks Inspector는 Loki 본체의 `go.mod`와는 독립적인 의존성을 가진다. 이는 이 도구가 Loki 전체 의존성을 끌어오지 않고 가볍게 빌드되도록 설계되었기 때문이다.

### 실행 흐름

```go
// cmd/chunks-inspect/main.go 라인 17-26
func main() {
    blocks := flag.Bool("b", false, "print block details")
    lines := flag.Bool("l", false, "print log lines")
    storeBlocks := flag.Bool("s", false, "store blocks to files")
    flag.Parse()

    for _, f := range flag.Args() {
        printFile(f, *blocks, *lines, *storeBlocks)
    }
}
```

```
$ chunks-inspect [-b] [-l] [-s] chunk_file1 chunk_file2 ...

  -b : 블록 상세 출력 (위치, 크기, 압축률, 체크섬)
  -l : 로그 라인 출력 (각 엔트리의 타임스탬프 + 라인)
  -s : 블록 데이터를 별도 파일로 저장
```

---

## 3. 청크 바이너리 포맷

### 전체 청크 파일 구조

Chunks Inspector가 파싱하는 Loki 청크 파일은 다음 구조를 가진다:

```
┌─────────────────────────────────────────────────┐
│           Chunk File (Object Store)              │
├─────────────────────────────────────────────────┤
│  Metadata (JSON, Snappy 압축)                    │
│  ├── Fingerprint (uint64)                        │
│  ├── UserID (string)                             │
│  ├── From / Through (model.Time)                 │
│  ├── Metric (Labels)                             │
│  └── Encoding (byte)                             │
├─────────────────────────────────────────────────┤
│  Data Length (uint32, BigEndian)                  │
├─────────────────────────────────────────────────┤
│  Loki Chunk Data                                 │
│  ├── Magic Number (4B): 0x012EE56A               │
│  ├── Version (1B): v1/v2/v3/v4                   │
│  ├── Encoding (1B): gzip/snappy/lz4/zstd/...     │
│  ├── Block 1 (압축된 로그 엔트리)                  │
│  │   └── Checksum (4B, CRC32 Castagnoli)         │
│  ├── Block 2 ...                                 │
│  ├── ...                                         │
│  ├── Metadata Table (v1-v3)                      │
│  │   ├── # Blocks (uvarint)                      │
│  │   ├── Block 1 Meta                            │
│  │   │   ├── # Entries (uvarint)                 │
│  │   │   ├── MinT (varint64)                     │
│  │   │   ├── MaxT (varint64)                     │
│  │   │   ├── Offset (uvarint)                    │
│  │   │   ├── UncompSize (uvarint, v3+)           │
│  │   │   └── Length (uvarint)                    │
│  │   └── Block N Meta ...                        │
│  ├── Meta Checksum (4B, CRC32)                   │
│  └── Meta Offset (8B, BigEndian) ─→ Metadata Table│
│                                                  │
│  [v4 전용 추가 섹션]                               │
│  ├── Structured Metadata Section                 │
│  │   ├── # Symbols (uvarint)                     │
│  │   └── 압축된 심볼 목록                          │
│  └── Section Index (각 섹션의 offset + length)    │
└─────────────────────────────────────────────────┘
```

### 헤더 디코딩

`header.go`의 `DecodeHeader()` 함수(라인 34-60):

```go
func DecodeHeader(r io.Reader) (*ChunkHeader, error) {
    // 1. 메타데이터 길이 읽기 (4바이트, BigEndian)
    var metadataLen uint32
    binary.Read(r, binary.BigEndian, &metadataLen)

    // 2. 메타데이터 바이트 읽기 (Snappy 압축)
    metadataBytes := make([]byte, metadataLen-4)
    io.ReadFull(r, metadataBytes)

    // 3. JSON 디코딩 (Snappy → JSON → struct)
    var metadata ChunkHeader
    json.NewDecoder(snappy.NewReader(bytes.NewReader(metadataBytes))).Decode(&metadata)

    // 4. 데이터 길이 읽기
    var dataLen uint32
    binary.Read(r, binary.BigEndian, &dataLen)

    metadata.MetadataLength = metadataLen
    metadata.DataLength = dataLen
    return &metadata, nil
}
```

### ChunkHeader 구조체

```go
// header.go 라인 13-30
type ChunkHeader struct {
    Fingerprint uint64 `json:"fingerprint"`  // 레이블셋의 해시 (model.Fingerprint)
    UserID      string `json:"userID"`       // 테넌트 ID

    From    Time   `json:"from"`             // 시작 시간 (model.Time)
    Through Time   `json:"through"`          // 종료 시간 (model.Time)
    Metric  Labels `json:"metric"`           // 레이블셋

    Encoding byte   `json:"encoding"`        // 인코딩 방식

    MetadataLength uint32  // 메타데이터 섹션 크기
    DataLength     uint32  // 데이터 섹션 크기
}
```

---

## 4. Chunks Inspector 파싱 엔진

### Loki 청크 파싱

`loki.go`의 `parseLokiChunk()` 함수(라인 104-263)가 핵심 파싱 로직이다:

```
parseLokiChunk(header, reader)
    │
    ├─ 1. 전체 데이터를 메모리로 읽기
    │     data := make([]byte, header.DataLength)
    │
    ├─ 2. 매직 넘버 검증: 0x012EE56A
    │
    ├─ 3. 버전(format) 확인: v1/v2/v3/v4
    │
    ├─ 4. 압축 방식 확인: getCompression(format, code)
    │
    ├─ 5. 메타데이터 테이블 위치 결정
    │     ├─ v1-v3: 마지막 8바이트가 오프셋
    │     └─ v4:    readSectionLenAndOffset(1)
    │
    ├─ 6. 메타데이터 체크섬 검증 (CRC32 Castagnoli)
    │
    ├─ 7. [v4 전용] 구조화된 메타데이터 심볼 파싱
    │     ├─ 심볼 수 (uvarint)
    │     └─ 압축 해제 후 각 문자열 읽기
    │
    └─ 8. 블록별 메타데이터 파싱
          for ix := 0; ix < blocks; ix++:
              ├─ numEntries (uvarint)
              ├─ minT (varint64)
              ├─ maxT (varint64)
              ├─ dataOffset (uvarint)
              ├─ uncompSize (uvarint, v3+)
              ├─ dataLength (uvarint)
              ├─ rawData 추출
              ├─ storedChecksum 추출
              ├─ computedChecksum 계산
              └─ parseLokiBlock() 호출
```

### 블록 파싱

`parseLokiBlock()` 함수(라인 265-333)는 개별 블록을 파싱한다:

```go
func parseLokiBlock(format byte, compression Encoding, data []byte, symbols []string)
    ([]byte, []LokiEntry, error) {

    // 1. 압축 해제
    r, _ := compression.readerFn(bytes.NewReader(data))
    decompressed, _ := io.ReadAll(r)

    // 2. 엔트리 순차 읽기
    for len(decompressed) > 0 {
        timestamp := readVarint(decompressed)
        lineLength := readUvarint(decompressed)
        line := string(decompressed[:lineLength])

        // v4: 구조화된 메타데이터 읽기
        if format >= chunkFormatV4 {
            // 심볼 인덱스 기반으로 키-값 쌍 복원
            pairs := readUvarint()  // 쌍 수
            for i := 0; i < pairs; i++ {
                nameIdx := readUvarint()
                valIdx := readUvarint()
                label{name: symbols[nameIdx], val: symbols[valIdx]}
            }
        }

        entries = append(entries, LokiEntry{timestamp, line, structuredMetadata})
    }
}
```

### Varint 인코딩 이해

Loki 청크에서 사용하는 인코딩 방식:

| 타입 | 함수 | 용도 | 특징 |
|------|------|------|------|
| **uvarint** | `binary.Uvarint` | 엔트리 수, 오프셋, 길이 | 음수 없는 가변 길이 |
| **varint** | `binary.Varint` | 타임스탬프 | 음수 가능한 가변 길이 |
| **BigEndian uint32** | `binary.BigEndian.Uint32` | 체크섬, 매직 넘버 | 고정 4바이트 |
| **BigEndian uint64** | `binary.BigEndian.Uint64` | 메타 오프셋 | 고정 8바이트 |

**왜 varint를 사용하는가?** 타임스탬프는 나노초 단위로 매우 큰 값(10^18 수준)이지만, 델타 인코딩과 함께 사용하면 작은 값이 되어 varint의 압축 효과를 얻는다.

---

## 5. Chunks Inspector 출력 분석

### 기본 출력 (`-b` 없이)

```
Chunks file: chunk_001.gz
Metadata length: 156
Data length: 8192
UserID: tenant-1
From: 2024-01-15 10:00:00.000000 UTC
Through: 2024-01-15 11:00:00.000000 UTC (1h0m0s)
Labels:
     app = web-server
     env = production
Format (Version): 3
Encoding: snappy
Blocks Metadata Checksum: a1b2c3d4 OK
Found 5 block(s), use -b to show block details
Minimum time (from first block): 2024-01-15 10:00:00.000000 UTC
Maximum time (from last block): 2024-01-15 10:59:59.000000 UTC
Total size of original data: 45678 file size: 8192 ratio: 5.57
```

### 블록 상세 출력 (`-b`)

```
Block    0: position:        6, original length:   9234 (stored:   2048, ratio: 4.51),
           minT: 2024-01-15 10:00:00.000000 UTC maxT: 2024-01-15 10:12:00.000000 UTC,
           checksum: a1b2c3d4 OK
Block    0: digest compressed: ab12..., original: cd34...
```

### 로그 라인 출력 (`-l`)

```
TS(2024-01-15 10:00:01.234000 UTC) LINE(GET /api/v1/users 200 12ms) STRUCTURED_METADATA(trace_id=abc123 )
```

### 블록 저장 (`-s`)

```
chunk_001.gz.block.0           ← 압축된 원본
chunk_001.gz.original.0        ← 압축 해제된 원본
```

---

## 6. Migration Tool 아키텍처

### 소스 코드 구조

```
cmd/migrate/
├── main.go      ← 진입점, 설정 파싱, 마이그레이션 오케스트레이션
├── main_test.go ← 테스트
├── Dockerfile   ← 컨테이너 이미지
└── README.md
```

### 설계 목표

Migration Tool은 다음 시나리오를 해결한다:

1. **스토리지 백엔드 변경**: S3 → GCS, DynamoDB → BoltDB 등
2. **테넌트 간 데이터 이동**: 테넌트 A의 데이터를 테넌트 B로 복사
3. **인덱스 포맷 업그레이드**: BoltDB → TSDB 마이그레이션
4. **클러스터 간 이동**: 다른 Loki 클러스터로 데이터 복제

### 전체 아키텍처

```
┌───────────────┐     ┌──────────────────────┐     ┌───────────────┐
│ Source Store   │     │    Migration Tool     │     │  Dest Store    │
│               │     │                      │     │               │
│ ┌───────────┐ │     │  ┌────────────────┐  │     │ ┌───────────┐ │
│ │ Chunk     │ │←────│──│  ChunkMover    │──│────→│ │ Chunk     │ │
│ │ Store     │ │     │  │                │  │     │ │ Store     │ │
│ └───────────┘ │     │  │ ┌──────────┐   │  │     │ └───────────┘ │
│ ┌───────────┐ │     │  │ │ Worker 1 │   │  │     │ ┌───────────┐ │
│ │ Index     │ │←────│──│ │ Worker 2 │   │──│────→│ │ Index     │ │
│ │ Store     │ │     │  │ │ Worker 3 │   │  │     │ │ Store     │ │
│ └───────────┘ │     │  │ │ ...      │   │  │     │ └───────────┘ │
└───────────────┘     │  │ └──────────┘   │  │     └───────────────┘
                      │  └────────────────┘  │
                      │                      │
                      │  Stats Channel       │
                      │  Error Channel       │
                      │  pprof (:8080)       │
                      └──────────────────────┘
```

### CLI 파라미터

`cmd/migrate/main.go` 라인 38-53:

```go
from   := flag.String("from", "", "Start Time RFC339Nano")
to     := flag.String("to", "", "End Time RFC339Nano")
sf     := flag.String("source.config.file", "", "source datasource config")
df     := flag.String("dest.config.file", "", "dest datasource config")
source := flag.String("source.tenant", "fake", "Source tenant identifier")
dest   := flag.String("dest.tenant", "fake", "Destination tenant identifier")
match  := flag.String("match", "", "Optional label match")

batch    := flag.Int("batchLen", 500, "Chunks per batch")
shardBy  := flag.Duration("shardBy", 6*time.Hour, "Shard duration")
parallel := flag.Int("parallel", 8, "Parallel workers")
```

| 파라미터 | 기본값 | 설명 |
|----------|--------|------|
| `--from` | (필수) | 시작 시간 (RFC3339Nano) |
| `--to` | (필수) | 종료 시간 (RFC3339Nano) |
| `--source.config.file` | (필수) | 소스 Loki 설정 파일 |
| `--dest.config.file` | (필수) | 대상 Loki 설정 파일 |
| `--source.tenant` | `fake` | 소스 테넌트 ID |
| `--dest.tenant` | `fake` | 대상 테넌트 ID |
| `--match` | (없음) | 레이블 매처 (선택적 필터) |
| `--batchLen` | `500` | 배치당 청크 수 |
| `--shardBy` | `6h` | 시간 분할 단위 |
| `--parallel` | `8` | 병렬 워커 수 |

---

## 7. Migration 실행 흐름

### 초기화 단계

`main()` 함수(라인 38-240)의 초기화 순서:

```
1. CLI 플래그 파싱
     │
2. 기본 설정 로드 (defaultsConfig)
     │
3. 소스/대상 설정 파일 로드
     │  cfg.DynamicUnmarshal(&sourceConfig, ...)
     │  cfg.DynamicUnmarshal(&destConfig, ...)
     │
4. 캐시 비활성화 (메모리 절약)
     │  sourceConfig.ChunkCacheConfig.EmbeddedCache.Enabled = false
     │  destConfig.ChunkCacheConfig.EmbeddedCache.Enabled = false
     │
5. 소스 인덱스를 읽기 전용으로 설정
     │  sourceConfig.BoltDBShipperConfig.Mode = indexshipper.ModeReadOnly
     │  sourceConfig.TSDBShipperConfig.Mode = indexshipper.ModeReadOnly
     │
6. 대상 인덱스 동기화 가속
     │  destConfig.IndexCacheValidity = 1 * time.Minute
     │  destConfig.BoltDBShipperConfig.ResyncInterval = 1 * time.Minute
     │
7. Index Gateway 비활성화
     │
8. 카디널리티 제한 완화
     │  sourceConfig.LimitsConfig.CardinalityLimit = 1e9
     │  sourceConfig.LimitsConfig.MaxQueryLength = 0
     │
9. 소스/대상 Store 생성
     │
10. syncRange 계산 및 워커 시작
```

### 왜 캐시를 비활성화하는가?

소스 코드 라인 80-91의 주석:

> "This is a little brittle, if we add a new cache it may easily get missed here but it's important to disable any of the chunk caches to save on memory because we write chunks to the cache when we call Put operations on the store."

마이그레이션은 대량의 청크를 읽고 쓰는 작업이다. 캐시가 활성화되면 Put 연산 시 모든 청크가 캐시에도 기록되어 메모리가 빠르게 소진된다. 따라서 EmbeddedCache, Memcached, Redis 캐시를 모두 비활성화한다.

### syncRange 계산

```go
// cmd/migrate/main.go 라인 242-268
func calcSyncRanges(from, to int64, shardBy int64) []*syncRange {
    syncRanges := []*syncRange{}
    currentFrom := from
    currentTo := from + shardBy
    number := 0

    for currentFrom < to && currentTo <= to {
        s := &syncRange{
            number: number,
            from:   currentFrom,
            to:     currentTo,
        }
        syncRanges = append(syncRanges, s)
        number++
        currentFrom = currentTo + 1
        currentTo = currentTo + shardBy
        if currentTo > to {
            currentTo = to
        }
    }
    return syncRanges
}
```

```
전체 시간 범위: 2024-01-01 ─────────────────── 2024-01-02
                          │                        │
  shardBy=6h:            ├──6h──┤──6h──┤──6h──┤──6h──┤
                         │      │      │      │      │
  syncRange:            [0]    [1]    [2]    [3]
```

**왜 시간을 분할하는가?** 인덱스 쿼리 시 너무 넓은 시간 범위를 한 번에 요청하면 중복 청크가 많이 반환될 수 있다. `shardBy`를 적절히 설정하면 각 샤드에서 겹치는 청크가 줄어들어 효율적이다.

---

## 8. ChunkMover 엔진

### 구조체 정의

```go
// cmd/migrate/main.go 라인 275-285
type chunkMover struct {
    ctx        context.Context
    schema     config.SchemaConfig    // 대상 스키마 (청크 외부 키 결정)
    source     storage.Store          // 소스 스토어
    dest       storage.Store          // 대상 스토어
    sourceUser string                 // 소스 테넌트
    destUser   string                 // 대상 테넌트
    matchers   []*labels.Matcher      // 레이블 매처
    batch      int                    // 배치 크기
    syncRanges int                    // 총 범위 수 (로깅용)
}
```

### moveChunks 실행 루프

`moveChunks()` 메서드(라인 302-414)의 핵심 로직:

```
moveChunks(ctx, threadID, syncRangeCh, errCh, statsCh)
    │
    │  for {
    │    select {
    │    case <-ctx.Done(): return
    │    case sr := <-syncRangeCh:
    │
    ├─ 1. 인덱스에서 청크 참조 조회
    │     source.GetChunks(ctx, user, from, to, matchers)
    │     → schemaGroups[][], fetchers[]
    │
    ├─ 2. 스키마 그룹별 처리
    │     for i, f := range fetchers:
    │
    ├─ 3. 배치 단위로 슬라이싱
    │     for j := 0; j < len(chunks); j += batch:
    │
    ├─ 4. 청크 가져오기 (FetchChunks)
    │     ├─ 성공: 다음 단계
    │     └─ 실패: 개별 청크 재시도 (최대 4회)
    │
    ├─ 5. 테넌트 ID 변경 (필요 시)
    │     if sourceUser != destUser:
    │       nc := chunk.NewChunk(destUser, ...)
    │       nc.Encode()
    │
    ├─ 6. 대상 스토어에 기록 (Put)
    │     dest.Put(ctx, output)
    │     ├─ 성공: 다음 배치
    │     └─ 실패: 재시도 (최대 4회)
    │
    └─ 7. 통계 보고
          statsCh <- stats{totalChunks, totalBytes}
```

### 재시도 전략

청크 가져오기와 저장 모두 4회 재시도를 수행한다:

```go
// 개별 청크 재시도 (FetchChunks 실패 시)
for retry := 4; retry >= 0; retry-- {
    onechunk, err = f.FetchChunks(m.ctx, onechunk)
    if err != nil {
        if retry == 0 {
            log.Println(threadID, "Final error, giving up:", err)
        }
        time.Sleep(5 * time.Second)
    } else {
        break
    }
}

// Put 재시도
for retry := 4; retry >= 0; retry-- {
    err = m.dest.Put(m.ctx, output)
    if err != nil {
        if retry == 0 {
            errCh <- err
            return
        }
    } else {
        break
    }
}
```

**왜 개별 청크를 다시 시도하는가?** 배치 전체가 실패할 때, 모든 청크를 다시 가져오는 것은 낭비이다. 문제가 있는 개별 청크만 재시도하면 대부분의 데이터는 이미 성공적으로 가져온 상태를 유지할 수 있다.

### 테넌트 변경 시 재인코딩

```go
// cmd/migrate/main.go 라인 377-390
if m.sourceUser != m.destUser {
    // 청크가 이미 인코딩되어 있으므로, 사용자명을 변경하려면 새 청크를 만들어야 함
    nc := chunk.NewChunk(m.destUser, chk.FingerprintModel(),
        chk.Metric, chk.Data, chk.From, chk.Through)
    nc.Encode()
    output = append(output, nc)
} else {
    output = append(output, finalChks[i])
}
```

**왜 재인코딩이 필요한가?** Loki 청크의 외부 키(Object Store의 키)에는 테넌트 ID가 포함되어 있다. 테넌트를 변경하면 키가 달라지므로, 새로운 키로 청크를 재생성해야 한다. 데이터 자체는 동일하지만 메타데이터(UserID)가 변경된다.

### 통계 및 모니터링

```go
// cmd/migrate/main.go 라인 210-218
go func() {
    for stat := range statsChan {
        processedChunks += stat.totalChunks
        processedBytes += stat.totalBytes
    }
    log.Printf("Transferring %v chunks totalling %s in %v for an average throughput of %s/second\n",
        processedChunks, ByteCountDecimal(processedBytes), time.Since(start),
        ByteCountDecimal(uint64(float64(processedBytes)/time.Since(start).Seconds())))
}()
```

```go
// ByteCountDecimal: 바이트를 읽기 좋은 단위로 변환
func ByteCountDecimal(b uint64) string {
    const unit = 1000
    if b < unit {
        return fmt.Sprintf("%d B", b)
    }
    div, exp := uint64(unit), 0
    for n := b / unit; n >= unit; n /= unit {
        div *= unit
        exp++
    }
    return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "kMGTPE"[exp])
}
```

### 병렬 처리 아키텍처

```
                    ┌──────────────┐
                    │  Dispatcher  │ (goroutine)
                    │  Thread      │
                    └──────┬───────┘
                           │ syncChan
              ┌────────────┼────────────┐
              │            │            │
        ┌─────▼────┐ ┌────▼─────┐ ┌───▼──────┐
        │ Worker 0 │ │ Worker 1 │ │ Worker N │
        └─────┬────┘ └────┬─────┘ └───┬──────┘
              │            │            │
              └────────────┼────────────┘
                           │ statsChan / errChan
                    ┌──────▼───────┐
                    │  Stats       │
                    │  Collector   │
                    └──────────────┘
```

---

## 9. Loki Tool 아키텍처

### 소스 코드

```go
// cmd/lokitool/main.go
func main() {
    app := kingpin.New("lokitool", "A command-line tool to manage Loki.")
    ruleCommand.Register(app)    // 규칙 관리 커맨드
    auditCommand.Register(app)   // 감사 커맨드

    app.Command("version", "Get the version of the lokitool CLI").
        Action(func(_ *kingpin.ParseContext) error {
            fmt.Println(version.Print("loki"))
            return nil
        })

    kingpin.MustParse(app.Parse(os.Args[1:]))
}
```

### 커맨드 구조

```
lokitool
├── rules      ← 규칙(alerting/recording rules) 관리
│   ├── sync   ← 규칙 파일을 Loki Ruler에 동기화
│   ├── diff   ← 로컬 규칙과 원격 규칙 비교
│   └── ...
├── audit      ← Loki 설정/상태 감사
│   └── ...
└── version    ← 버전 출력
```

### RuleCommand와 AuditCommand

두 커맨드는 `pkg/tool/commands/` 패키지에 정의되어 있다:

```
pkg/tool/commands/
├── rules.go       ← RuleCommand: Loki Ruler와 규칙 동기화
└── audit.go       ← AuditCommand: 설정 감사
```

**RuleCommand**는 Cortex Tool(현 Mimir Tool)의 패턴을 따른다. YAML 파일로 정의된 알림/레코딩 규칙을 Loki Ruler API를 통해 관리한다.

**AuditCommand**는 Loki 인스턴스의 설정, 규칙, 리소스 사용량 등을 검사하여 보고서를 생성한다.

---

## 10. 도구 간 관계 및 워크플로우

### 운영 시나리오별 도구 활용

```
┌─────────────────────────────────────────────────────────────────┐
│ 시나리오 1: 데이터 무결성 검사                                    │
│                                                                 │
│  Object Store ──→ chunks-inspect ──→ 체크섬 검증                 │
│                                      블록 구조 확인              │
│                                      압축률 분석                 │
└─────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────┐
│ 시나리오 2: 스토리지 마이그레이션                                  │
│                                                                 │
│  1. chunks-inspect로 소스 데이터 확인                             │
│  2. migrate 도구로 데이터 이동                                    │
│  3. chunks-inspect로 대상 데이터 검증                             │
│  4. querytee로 양쪽 쿼리 결과 비교                               │
└─────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────┐
│ 시나리오 3: 규칙 관리                                            │
│                                                                 │
│  lokitool rules sync ──→ Loki Ruler API                         │
│  lokitool rules diff ──→ 변경 사항 확인                          │
│  lokitool audit     ──→ 설정 검사                               │
└─────────────────────────────────────────────────────────────────┘
```

---

## 11. 압축/인코딩 시스템

### 지원 인코딩

`loki.go`(라인 36-54)에서 정의한 인코딩 목록:

| 코드 | 이름 | 설명 |
|------|------|------|
| 0 | `none` | 압축 없음 |
| 1 | `gzip` | Gzip 압축 (v1 기본) |
| 2 | `dumb` | 더미 (테스트용) |
| 3 | `lz4` | LZ4 표준 |
| 4 | `snappy` | Snappy 압축 |
| 5 | `lz4-256k` | LZ4 256KB 블록 |
| 6 | `lz4-1M` | LZ4 1MB 블록 |
| 7 | `lz4-4M` | LZ4 4MB 블록 |
| 8 | `flate` | Deflate 압축 |
| 9 | `zstd` | Zstandard 압축 |

### 버전별 인코딩 결정

```go
// loki.go 라인 359-375
func getCompression(format byte, code byte) (Encoding, error) {
    if format == chunkFormatV1 {
        return encGZIP, nil  // v1은 항상 gzip
    }
    if format >= chunkFormatV2 {
        for _, e := range Encodings {
            if e.code == int(code) {
                return e, nil
            }
        }
    }
    return encNone, fmt.Errorf("unknown format: %d", format)
}
```

### 청크 포맷 버전 비교

| 버전 | 특징 | 인코딩 |
|------|------|--------|
| v1 | 기본 포맷 | gzip 고정 |
| v2 | 인코딩 선택 가능 | 코드 기반 선택 |
| v3 | 비압축 크기 메타 추가 | 동일 |
| v4 | 구조화된 메타데이터 섹션 | 동일 + 심볼 테이블 |

### CRC32 Castagnoli 체크섬

```go
// loki.go 라인 30-33
var castagnoliTable *crc32.Table

func init() {
    castagnoliTable = crc32.MakeTable(crc32.Castagnoli)
}
```

**왜 Castagnoli(CRC32C)인가?** 표준 CRC32보다 오류 검출 능력이 우수하고, 최신 CPU에서 하드웨어 가속(SSE 4.2의 CRC32C 명령어)을 지원하여 성능이 뛰어나다.

---

## 12. 운영 가이드

### Chunks Inspector 사용 예시

```bash
# 기본 정보 확인
chunks-inspect chunk_001.gz

# 블록 상세 + 로그 라인 확인
chunks-inspect -b -l chunk_001.gz

# 블록 데이터를 파일로 저장 (분석용)
chunks-inspect -s chunk_001.gz

# 여러 청크 파일 일괄 검사
chunks-inspect chunk_*.gz
```

### Migration Tool 사용 예시

```bash
# 기본 마이그레이션
migrate \
  --from="2024-01-01T00:00:00Z" \
  --to="2024-01-02T00:00:00Z" \
  --source.config.file=source-loki.yaml \
  --dest.config.file=dest-loki.yaml

# 테넌트 변경 + 레이블 필터
migrate \
  --from="2024-01-01T00:00:00Z" \
  --to="2024-01-02T00:00:00Z" \
  --source.config.file=source-loki.yaml \
  --dest.config.file=dest-loki.yaml \
  --source.tenant=old-tenant \
  --dest.tenant=new-tenant \
  --match='{app="important-service"}'

# 고성능 마이그레이션
migrate \
  --from="2024-01-01T00:00:00Z" \
  --to="2024-02-01T00:00:00Z" \
  --source.config.file=source-loki.yaml \
  --dest.config.file=dest-loki.yaml \
  --parallel=16 \
  --batchLen=1000 \
  --shardBy=1h
```

### Loki Tool 사용 예시

```bash
# 규칙 동기화
lokitool rules sync --rules-file=alert-rules.yaml

# 규칙 차이 비교
lokitool rules diff --rules-file=alert-rules.yaml

# 버전 확인
lokitool version
```

### 주의 사항

| 도구 | 주의 사항 |
|------|----------|
| Chunks Inspector | 대용량 청크 파일은 메모리를 많이 사용 (전체 데이터를 메모리에 로드) |
| Migration Tool | 캐시 비활성화 확인, pprof(:8080)로 메모리 모니터링 필수 |
| Migration Tool | `--shardBy`가 너무 작으면 중복 청크가 증가 |
| Migration Tool | 완료 후 무한 대기(`time.Sleep`)하므로 수동 종료 필요 |
| Loki Tool | Ruler API 접근 권한 필요 |

---

## 부록: 주요 소스 파일 참조

| 파일 | 설명 |
|------|------|
| `cmd/chunks-inspect/main.go` | Chunks Inspector 진입점 (136줄) |
| `cmd/chunks-inspect/header.go` | 청크 헤더 디코딩 (60줄) |
| `cmd/chunks-inspect/loki.go` | Loki 청크 파싱 엔진 (376줄) |
| `cmd/chunks-inspect/labels.go` | 레이블 직렬화 |
| `cmd/chunks-inspect/time.go` | 시간 타입 처리 |
| `cmd/migrate/main.go` | Migration Tool 전체 (438줄) |
| `cmd/lokitool/main.go` | Loki Tool 진입점 (31줄) |
| `pkg/tool/commands/` | Loki Tool 커맨드 구현 |
