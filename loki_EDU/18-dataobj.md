# 18. DataObj: 컬럼나 데이터 포맷 Deep-Dive

## 목차

1. [개요](#1-개요)
2. [왜 컬럼나 포맷인가](#2-왜-컬럼나-포맷인가)
3. [파일 포맷 구조](#3-파일-포맷-구조)
4. [섹션 아키텍처](#4-섹션-아키텍처)
5. [인코딩 파이프라인](#5-인코딩-파이프라인)
6. [디코딩 파이프라인](#6-디코딩-파이프라인)
7. [섹션 타입별 상세 분석](#7-섹션-타입별-상세-분석)
8. [DataObj Consumer](#8-dataobj-consumer)
9. [Builder 아키텍처](#9-builder-아키텍처)
10. [Uploader와 Object Storage 경로](#10-uploader와-object-storage-경로)
11. [Metastore와 인덱스 계층](#11-metastore와-인덱스-계층)
12. [Explorer와 Inspect 도구](#12-explorer와-inspect-도구)
13. [청크 기반 스토리지와의 비교](#13-청크-기반-스토리지와의-비교)
14. [스키마 진화와 호환성](#14-스키마-진화와-호환성)
15. [운영 가이드](#15-운영-가이드)

---

## 1. 개요

DataObj는 Grafana Loki의 차세대 컬럼나(columnar) 컨테이너 포맷이다. 기존의 청크 기반 스토리지를 대체하기 위해 설계되었으며, Object Storage에 저장되는 자기 완결적(self-contained) 파일 형태로 로그 데이터를 컬럼나 형식으로 관리한다.

### 핵심 설계 목표

```
+------------------------------------------------------------------+
|                    DataObj 설계 목표                                |
+------------------------------------------------------------------+
|                                                                    |
|  1. 컬럼나 스토리지        로그 필드별 독립적 접근                    |
|  2. 멀티 테넌트            하나의 파일에 여러 테넌트 데이터            |
|  3. 섹션 기반 확장성       새로운 데이터 유형 추가 용이                |
|  4. 효율적 I/O             메타데이터 프리페치로 최소 라운드트립        |
|  5. Object Storage 최적화  순차 읽기, 범위 읽기 활용                  |
|                                                                    |
+------------------------------------------------------------------+
```

### 소스코드 위치

DataObj 패키지의 핵심 디렉토리 구조:

```
pkg/dataobj/
  dataobj.go          -- Object 구조체, FromBucket(), FromReaderAt()
  encoder.go          -- 인코더, 매직 바이트 "DOBJ"
  decoder.go          -- 디코더, 메타데이터 읽기
  builder.go          -- Builder (섹션 조립)
  section.go          -- Section, SectionType, SectionReader 인터페이스
  sections/
    logs/             -- 로그 섹션 (stream_id, timestamp, metadata, message)
    streams/          -- 스트림 섹션 (stream_id, min/max timestamp, labels)
    pointers/         -- 포인터 섹션 (인덱스 오브젝트 → 데이터 오브젝트 참조)
    indexpointers/    -- 인덱스 포인터 섹션 (ToC → 인덱스 오브젝트 참조)
  consumer/           -- Kafka 소비자 (레코드 → DataObj 빌드)
  uploader/           -- Object Storage 업로드
  metastore/          -- 메타스토어 (인덱스 관리, 쿼리 라우팅)
  index/              -- 인덱스 빌더 (포인터/블룸 필터 생성)
  explorer/           -- HTTP 기반 탐색 서비스
  tools/              -- Inspect 도구
```

**소스 참조**: `pkg/dataobj/dataobj.go` 패키지 주석 (라인 1-74):
```go
// Package dataobj holds utilities for working with data objects.
//
// Data objects are a container format for storing data intended to be
// retrieved from object storage. Each data object is composed of one or more
// "sections," each of which contains a specific type of data, such as logs
// stored in a columnar format.
//
// Sections are further split into two "regions": the section data and the
// section metadata. Section metadata is intended to be a lightweight payload
// per section (usually protobuf) which aids in reading smaller portions of the
// data region.
```

---

## 2. 왜 컬럼나 포맷인가

### 기존 청크 기반 스토리지의 한계

Loki의 기존 스토리지는 Ingester가 메모리에 로그를 버퍼링한 후 청크(chunk) 단위로 압축하여 Object Storage에 저장하는 구조였다. 이 접근 방식에는 여러 한계가 있다:

```
기존 청크 기반 스토리지:
+-------------------+     +-------------------+
|    Chunk A        |     |    Chunk B        |
| stream1 + logs    |     | stream1 + logs    |
| (gzip 압축)       |     | (gzip 압축)       |
+-------------------+     +-------------------+

문제점:
1. 전체 청크를 읽어야 특정 필드 접근 가능
2. 스트림별로 청크가 분산 → 많은 Object Storage 요청
3. 인덱스(BoltDB/TSDB)와 데이터가 분리 → 2단계 조회
4. 작은 청크 파일 → Object Storage 비효율
5. 메타데이터와 로그 데이터가 혼합 → 필터링 비효율
```

### 컬럼나 포맷의 장점

DataObj의 컬럼나 포맷은 데이터를 필드(컬럼)별로 분리하여 저장한다:

```
DataObj 컬럼나 스토리지:
+----------------------------------------------------------+
|  DataObj 파일 (예: objects/ab/cd1234...)                   |
+----------------------------------------------------------+
|  Logs Section                                             |
|  +--------+  +-----------+  +----------+  +-----------+  |
|  |stream_ |  |timestamp  |  |metadata  |  |message    |  |
|  |id 컬럼 |  |컬럼       |  |컬럼      |  |컬럼       |  |
|  |        |  |           |  |          |  |           |  |
|  |[page1] |  |[page1]    |  |[page1]   |  |[page1]    |  |
|  |[page2] |  |[page2]    |  |[page2]   |  |[page2]    |  |
|  |[page3] |  |[page3]    |  |[page3]   |  |[page3]    |  |
|  +--------+  +-----------+  +----------+  +-----------+  |
+----------------------------------------------------------+
|  Streams Section                                          |
|  +--------+  +---------+  +---------+  +--------+       |
|  |stream_ |  |min_ts   |  |max_ts   |  |labels  |       |
|  |id 컬럼 |  |컬럼     |  |컬럼     |  |컬럼    |       |
|  +--------+  +---------+  +---------+  +--------+       |
+----------------------------------------------------------+

장점:
1. 특정 컬럼만 선택적 읽기 (Projection Pushdown)
2. 페이지 단위 통계로 필터링 (Predicate Pushdown)
3. 하나의 큰 파일 → Object Storage 효율
4. 컬럼별 최적 압축 (delta, zstd 등)
5. 메타데이터 분리 → 빠른 스캔
```

### 핵심 이점 요약

| 관점 | 청크 기반 | DataObj 컬럼나 |
|------|----------|---------------|
| I/O 패턴 | 전체 청크 읽기 필수 | 필요한 컬럼/페이지만 읽기 |
| 파일 수 | 스트림당 다수 | 시간 윈도우당 1개 |
| 압축 효율 | 행 기반 gzip | 컬럼별 최적 인코딩 |
| 필터링 | 전체 디코딩 후 | 페이지 통계 기반 스킵 |
| 메타데이터 | 별도 인덱스 DB | 파일 내 자체 포함 |
| 확장성 | 고정 스키마 | 섹션 기반 확장 |

---

## 3. 파일 포맷 구조

### 전체 레이아웃

DataObj 파일은 **헤더 → 바디 → 테일러** 구조를 따른다. 현재 두 가지 포맷이 존재한다:

**소스 참조**: `pkg/dataobj/encoder.go` (라인 15-25):
```go
var (
    magic = []byte("DOBJ")

    // legacyMagic is the magic bytes used in the original dataobj format where
    // file metadata was kept at the bottom of the file.
    legacyMagic = []byte("THOR")
)

const (
    fileFormatVersion = 0x1
)
```

#### 새로운 포맷 (DOBJ)

```
+-----------------------------------------------------------+
|  Header                                                    |
|  [0x00] "DOBJ" (4 bytes, 매직 바이트)                       |
|  [0x04] 메타데이터 크기 (uint32, little-endian)              |
|  [0x08] 파일 포맷 버전 (uvarint, 현재 0x1)                  |
|  [0x09~] 파일 메타데이터 (protobuf 인코딩)                   |
+-----------------------------------------------------------+
|  Body                                                      |
|  [메타데이터 리전들]  섹션 1 메타데이터                       |
|                      섹션 2 메타데이터                       |
|                      ...                                   |
|                      섹션 N 메타데이터                       |
|  [데이터 리전들]      섹션 1 데이터                          |
|                      섹션 2 데이터                          |
|                      ...                                   |
|                      섹션 N 데이터                          |
+-----------------------------------------------------------+
|  Tailer                                                    |
|  "DOBJ" (4 bytes, 매직 바이트)                              |
+-----------------------------------------------------------+
```

**소스 참조**: `pkg/dataobj/encoder.go` `Flush()` 메서드 (라인 235-297)에서 이 구조를 확인할 수 있다:
```go
func (enc *encoder) Flush() (*snapshot, error) {
    // The overall structure is:
    //
    // header:
    //  [magic]
    //  [file metadata + version size (32 bits)]
    //  [file metadata version]
    //  [file metadata]
    // body:
    //  [metadata regions]
    //  [data regions]
    // tailer:
    //  [magic]
    ...
}
```

#### 레거시 포맷 (THOR)

레거시 포맷은 메타데이터가 파일 끝에 위치한다:

```
+-----------------------------------------------------------+
|  Header                                                    |
|  [0x00] "THOR" (4 bytes, 매직 바이트)                       |
+-----------------------------------------------------------+
|  Body                                                      |
|  [섹션 데이터 리전들]                                        |
|  [섹션 메타데이터 리전들]                                     |
+-----------------------------------------------------------+
|  Footer                                                    |
|  파일 포맷 버전 (uvarint)                                   |
|  파일 메타데이터 (protobuf)                                  |
|  메타데이터 크기 (uint32, little-endian)                     |
|  "THOR" (4 bytes, 매직 바이트)                              |
+-----------------------------------------------------------+
```

### 왜 헤더 기반으로 전환했는가

레거시 포맷(THOR)은 메타데이터가 파일 끝에 있어 읽기 시 파일의 마지막 부분을 먼저 읽어야 했다. 새 포맷(DOBJ)은 메타데이터를 파일 시작 부분에 배치하여 다음과 같은 이점을 제공한다:

1. **프리페치 효율성**: 파일 시작 16KB를 읽으면 메타데이터와 함께 섹션 메타데이터 리전도 함께 프리페치된다
2. **한 번의 라운드트립**: 헤더만 읽으면 전체 파일 구조를 파악할 수 있다
3. **스트리밍 호환**: 메타데이터를 먼저 쓰면 순차 쓰기가 가능하다

**소스 참조**: `pkg/dataobj/encoder.go` (라인 274-285):
```go
// We encode metadata regions first to allow a single prefetch of the start
// of the file to also prefetch metadata regions.
regions := make([]sectionRegion, 0, len(enc.sections)*2)
for _, sec := range enc.sections {
    regions = append(regions, sec.Metadata)
}
for _, sec := range enc.sections {
    regions = append(regions, sec.Data)
}
```

### 파일 메타데이터 구조 (Protobuf)

파일 메타데이터는 protobuf로 인코딩되며 다음 정보를 포함한다:

```
filemd.Metadata:
+---------------------------------------------------+
|  Dictionary: ["", "github.com/grafana/loki",      |
|               "logs", "streams", "tenant-1", ...]  |
+---------------------------------------------------+
|  Types: [                                          |
|    {invalid},                                      |
|    {NameRef:{Namespace:1, Kind:2}, Version:3},     |
|    {NameRef:{Namespace:1, Kind:3}, Version:3},     |
|  ]                                                 |
+---------------------------------------------------+
|  Sections: [                                       |
|    {TypeRef:1, Layout:{                            |
|      Metadata:{Offset:0, Length:1024},              |
|      Data:{Offset:2048, Length:50000}               |
|    }, TenantRef:4, ExtensionData:...},              |
|    ...                                             |
|  ]                                                 |
+---------------------------------------------------+
```

**소스 참조**: `pkg/dataobj/encoder.go` `Metadata()` 메서드 (라인 149-200):
```go
func (enc *encoder) Metadata() (*filemd.Metadata, error) {
    enc.initDictionary()
    relativeOffset := 0

    sections := make([]*filemd.SectionInfo, len(enc.sections))
    // Determine metadata region location.
    for i, info := range enc.sections {
        sections[i] = &filemd.SectionInfo{
            TypeRef: enc.getTypeRef(info.Type),
            Layout: &filemd.SectionLayout{
                Metadata: &filemd.Region{
                    Offset: uint64(relativeOffset),
                    Length: uint64(info.Metadata.Size),
                },
            },
            ExtensionData: info.ExtensionData,
            TenantRef:     enc.getDictionaryKey(info.Tenant),
        }
        relativeOffset += info.Metadata.Size
    }
    // Determine data region locations.
    for i, info := range enc.sections {
        sections[i].Layout.Data = &filemd.Region{
            Offset: uint64(relativeOffset),
            Length: uint64(info.Data.Size),
        }
        relativeOffset += info.Data.Size
    }
    md := &filemd.Metadata{
        Sections:   sections,
        Dictionary: enc.dictionary,
        Types:      enc.rawTypes,
    }
    return md, nil
}
```

### 딕셔너리 인코딩

문자열 중복을 제거하기 위해 딕셔너리 기법을 사용한다. 네임스페이스, 종류(kind), 테넌트 ID 등 반복되는 문자열은 딕셔너리에 한 번만 저장하고, 참조 인덱스로 대체한다:

```
Dictionary:     ["", "github.com/grafana/loki", "logs", "streams", "tenant-a"]
                 [0]  [1]                        [2]     [3]         [4]

SectionType:    {NamespaceRef: 1, KindRef: 2}  → "github.com/grafana/loki/logs"
TenantRef:      4                               → "tenant-a"
```

**소스 참조**: `pkg/dataobj/encoder.go` (라인 122-147):
```go
func (enc *encoder) initDictionary() {
    // Reserve the zero index in the dictionary for an invalid entry.
    enc.dictionary = []string{""}
    enc.dictionaryLookup = map[string]uint32{"": 0}
}

func (enc *encoder) getDictionaryKey(text string) uint32 {
    enc.initDictionary()
    key, ok := enc.dictionaryLookup[text]
    if ok {
        return key
    }
    key = uint32(len(enc.dictionary))
    enc.dictionary = append(enc.dictionary, text)
    enc.dictionaryLookup[text] = key
    return key
}
```

---

## 4. 섹션 아키텍처

### Section 핵심 타입

DataObj의 모든 데이터는 **섹션(Section)** 단위로 조직된다. 각 섹션은 타입, 리더, 테넌트 정보를 포함한다.

**소스 참조**: `pkg/dataobj/section.go` (라인 39-48):
```go
type Section struct {
    Type   SectionType   // 섹션의 데이터 종류
    Reader SectionReader  // 저수준 리더
    Tenant string         // 소유 테넌트
}
```

### SectionType 식별

각 섹션 타입은 네임스페이스, 종류, 버전의 조합으로 고유하게 식별된다:

**소스 참조**: `pkg/dataobj/section.go` (라인 51-58):
```go
type SectionType struct {
    Namespace string // "github.com/grafana/loki"
    Kind      string // "logs", "streams", "pointers", "indexpointers"
    Version   uint32 // 인코딩 버전
}
```

### 섹션 리전: 데이터와 메타데이터 분리

각 섹션은 두 개의 리전으로 구성된다:

```
+-----------------------------------+
|  Section                          |
|  +-------------+  +-------------+ |
|  | Metadata    |  | Data        | |
|  | Region      |  | Region      | |
|  |             |  |             | |
|  | (protobuf)  |  | (컬럼나    | |
|  | - 컬럼 정보 |  |  페이지들)  | |
|  | - 통계      |  |             | |
|  | - 페이지맵  |  |             | |
|  +-------------+  +-------------+ |
+-----------------------------------+
```

이 분리의 핵심 이유는 **메타데이터만 읽어도 데이터 리전의 어떤 부분을 읽어야 하는지 판단**할 수 있기 때문이다.

**소스 참조**: `pkg/dataobj/section.go` (라인 77-111) - SectionReader 인터페이스:
```go
type SectionReader interface {
    ExtensionData() []byte
    DataRange(ctx context.Context, offset, length int64) (io.ReadCloser, error)
    MetadataRange(ctx context.Context, offset, length int64) (io.ReadCloser, error)
    DataSize() int64
    MetadataSize() int64
}
```

### SectionBuilder와 SectionWriter

섹션을 생성하는 빌더 인터페이스:

**소스 참조**: `pkg/dataobj/section.go` (라인 113-183):
```go
type SectionBuilder interface {
    Type() SectionType
    Flush(w SectionWriter) (n int64, err error)
    Reset()
}

type SectionWriter interface {
    WriteSection(opts *WriteSectionOptions, data, metadata []byte) (n int64, err error)
}

type WriteSectionOptions struct {
    Tenant        string
    ExtensionData []byte
}
```

### 섹션 필터링 API

Object에서 특정 타입의 섹션만 효율적으로 찾기 위한 이터레이터 패턴:

**소스 참조**: `pkg/dataobj/section.go` (라인 10-37):
```go
type Sections []*Section

func (s Sections) Filter(predicate func(*Section) bool) iter.Seq2[int, *Section] {
    return func(yield func(int, *Section) bool) {
        var matches int
        for _, sec := range s {
            if !predicate(sec) {
                continue
            } else if !yield(matches, sec) {
                return
            }
            matches++
        }
    }
}

func (s Sections) Count(predicate func(*Section) bool) int {
    var count int
    for range s.Filter(predicate) {
        count++
    }
    return count
}
```

---

## 5. 인코딩 파이프라인

### 전체 인코딩 흐름

```
로그 레코드
    |
    v
+------------------+
| logs.Builder     |  1. 레코드 버퍼링
| - Append(Record) |  2. 버퍼 가득 → Stripe 생성
| - Flush()        |  3. Stripe 정렬 & 압축
+--------+---------+
         |
         | WriteSection(data, metadata)
         v
+------------------+
| dataobj.Builder  |  4. 섹션 데이터/메타데이터 저장
| - Append(sb)     |  5. scratch.Store에 버퍼링
| - Flush()        |
+--------+---------+
         |
         | encoder.Flush()
         v
+------------------+
| snapshot         |  6. 헤더 + 리전들 + 테일러 조립
| - Size()         |  7. io.ReaderAt 인터페이스 제공
| - ReadAt()       |
+--------+---------+
         |
         v
+------------------+
| uploader.Upload  |  8. SHA 기반 경로 생성
|                  |  9. Object Storage 업로드
+------------------+
```

### encoder 내부 구조

**소스 참조**: `pkg/dataobj/encoder.go` (라인 28-53):
```go
type encoder struct {
    totalBytes int

    store    scratch.Store       // 섹션 데이터를 임시 저장
    sections []sectionInfo       // 버퍼링된 섹션 정보

    typesReady       bool
    dictionary       []string           // 문자열 딕셔너리
    dictionaryLookup map[string]uint32  // 딕셔너리 역방향 조회
    rawTypes         []*filemd.SectionType
    typeRefLookup    map[SectionType]uint32
}

type sectionInfo struct {
    Type     SectionType
    Data     sectionRegion
    Metadata sectionRegion
    Tenant   string
    ExtensionData []byte
}
```

### AppendSection 과정

섹션 빌더가 Flush하면 데이터와 메타데이터 바이트가 scratch.Store에 저장된다:

**소스 참조**: `pkg/dataobj/encoder.go` (라인 65-84):
```go
func (enc *encoder) AppendSection(typ SectionType, opts *WriteSectionOptions, data, metadata []byte) {
    var (
        dataHandle     = enc.store.Put(data)      // 스크래치 스토어에 저장
        metadataHandle = enc.store.Put(metadata)   // 스크래치 스토어에 저장
    )

    si := sectionInfo{
        Type:     typ,
        Data:     sectionRegion{Handle: dataHandle, Size: len(data)},
        Metadata: sectionRegion{Handle: metadataHandle, Size: len(metadata)},
    }
    if opts != nil {
        si.Tenant = opts.Tenant
        si.ExtensionData = slices.Clone(opts.ExtensionData)
    }

    enc.sections = append(enc.sections, si)
    enc.totalBytes += len(data) + len(metadata)
}
```

### scratch.Store: 메모리 관리

scratch.Store는 섹션 데이터를 임시 보관하는 추상 인터페이스이다. 메모리 기반(`scratch.NewMemory()`) 또는 디스크 기반 구현이 가능하여, 대용량 DataObj 빌드 시 메모리 사용량을 제어할 수 있다.

**소스 참조**: `pkg/dataobj/builder.go` (라인 29-37):
```go
func NewBuilder(scratchStore scratch.Store) *Builder {
    if scratchStore == nil {
        scratchStore = scratch.NewMemory()
    }

    return &Builder{
        encoder: newEncoder(scratchStore),
    }
}
```

### 압축 전략

DataObj는 다단계 압축 전략을 사용한다:

```
인코딩 단계별 압축:

1. Stripe 생성 (중간 버퍼):
   - zstd SpeedFastest (빠른 버퍼링 우선)
   - 정렬된 레코드를 빠르게 압축

2. 섹션 최종 인코딩:
   - zstd SpeedDefault (높은 압축률)
   - 컬럼별 페이지 단위 압축

3. 값 인코딩 방식:
   - Plain:  원본 값 그대로 저장
   - Delta:  연속 값 간의 차이 저장 (정렬된 타임스탬프에 효과적)
   - Bitmap: 반복 레벨 (null/non-null 표시)
```

---

## 6. 디코딩 파이프라인

### 전체 디코딩 흐름

```
Object Storage
    |
    | ReadRange(0, 16KB)
    v
+---------------------+
| decoder.Metadata()  |  1. 첫 16KB 프리페치
| - header()          |  2. 매직 바이트 확인 ("DOBJ" or "THOR")
| - decodeHeader()    |  3. 메타데이터 크기 파싱
+----------+----------+
           |
           v
+---------------------+
| Object.init()       |  4. 섹션 목록 구성
| - getSectionType()  |  5. 딕셔너리로 타입 해석
| - SectionReader()   |  6. 테넌트별 분류
+----------+----------+
           |
           | Section 열기
           v
+---------------------+
| logs.Open()         |  7. 섹션 메타데이터 디코딩
| - columnar.Open()   |  8. 컬럼 목록 확인
| - Columns()         |  9. 프레디킷 매핑
+----------+----------+
           |
           | 페이지 읽기
           v
+---------------------+
| logs.Reader.Read()  |  10. 페이지 단위 디코딩
| - 프레디킷 적용     |  11. 프레디킷 필터링
| - Arrow RecordBatch |  12. Arrow 레코드 변환
+---------------------+
```

### decoder 핵심 로직

**소스 참조**: `pkg/dataobj/decoder.go` (라인 19-28):
```go
const minimumPrefetchBytes int64 = 16 * 1024  // 16KB

type decoder struct {
    rr            rangeReader
    size          int64
    startOff      int64        // 바디 시작 오프셋
    prefetchBytes int64

    prefetchedRangeReader rangeReader  // 프리페치된 데이터 캐시
}
```

### Metadata 읽기 최적화

디코더는 파일 시작 부분을 한 번에 읽어 메타데이터를 파싱한다. Object Size는 별도 고루틴에서 병렬로 가져온다:

**소스 참조**: `pkg/dataobj/decoder.go` (라인 30-97):
```go
func (d *decoder) Metadata(ctx context.Context) (*filemd.Metadata, error) {
    prefetchBytes := d.effectivePrefetchBytes()
    buf := make([]byte, prefetchBytes)

    g, gctx := errgroup.WithContext(ctx)

    // 객체 크기를 백그라운드에서 병렬 조회
    if d.size == 0 {
        g.Go(func() error {
            if _, err := d.objectSize(gctx); err != nil {
                return fmt.Errorf("fetching object size: %w", err)
            }
            return nil
        })
    }

    // 첫 N 바이트 읽기
    g.Go(func() error {
        n, err := d.readFirstBytes(gctx, prefetchBytes, buf)
        if err != nil {
            return fmt.Errorf("reading first %d bytes: %w", prefetchBytes, err)
        }
        buf = buf[:n]
        return nil
    })

    if err := g.Wait(); err != nil {
        return nil, err
    }

    header, err := d.header(buf)
    if err != nil && errors.Is(err, errLegacyMagic) {
        // 레거시 포맷으로 폴백
        buf = buf[:0]
        return d.legacyMetadata(ctx, buf)
    }

    d.setPrefetchedBytes(0, buf)
    d.startOff = int64(8) + int64(header.MetadataSize)

    if header.MetadataSize+8 <= uint64(len(buf)) {
        // 낙관적 읽기 성공: 버퍼에서 바로 디코딩
        rc := bytes.NewReader(buf[8:])
        return decodeFileMetadata(rc)
    }

    // 낙관적 읽기 실패: 메타데이터 전체 재읽기
    rc, err := d.rr.ReadRange(ctx, int64(8), int64(header.MetadataSize))
    ...
}
```

### 프리페치 최적화

디코더는 처음 읽은 바이트를 `prefetchedRangeReader`에 캐시한다. 이후 섹션 메타데이터를 읽을 때 이미 프리페치된 범위에 해당하면 추가 I/O 없이 바로 반환한다:

**소스 참조**: `pkg/dataobj/decoder.go` (라인 248-258):
```go
func (d *decoder) setPrefetchedBytes(offset int64, data []byte) {
    if len(data) == 0 {
        return
    }
    d.prefetchedRangeReader = &prefetchedRangeReader{
        inner:          d.rr,
        prefetchOffset: offset,
        prefetched:     data,
    }
}
```

### 레거시 포맷 호환

THOR 매직 바이트를 감지하면 파일 끝에서 메타데이터를 읽는 레거시 로직으로 자동 전환된다:

**소스 참조**: `pkg/dataobj/decoder.go` (라인 115-155):
```go
func (d *decoder) legacyMetadata(ctx context.Context, buf []byte) (*filemd.Metadata, error) {
    objectSize, err := d.objectSize(ctx)
    ...
    readSize := min(objectSize, d.effectivePrefetchBytes())
    ...
    n, err := d.readLastBytes(ctx, readSize, buf) // 파일 끝에서 읽기
    ...
    tailer, err := d.tailer(ctx, buf)
    ...
    if tailer.MetadataSize+8 <= uint64(len(buf)) {
        rc := bytes.NewReader(buf[len(buf)-int(tailer.MetadataSize)-8 : len(buf)-8])
        return decodeFileMetadata(rc)
    }
    ...
}
```

### Object 초기화

Object가 열리면 메타데이터를 파싱하여 섹션 목록과 테넌트 목록을 구성한다:

**소스 참조**: `pkg/dataobj/dataobj.go` (라인 123-155):
```go
func (o *Object) init(ctx context.Context) error {
    metadata, err := o.dec.Metadata(ctx)
    if err != nil {
        return fmt.Errorf("reading metadata: %w", err)
    }

    sections := make([]*Section, 0, len(metadata.Sections))
    tenants := make(map[string]struct{})

    for i, sec := range metadata.Sections {
        typ, err := getSectionType(metadata, sec)
        ...
        tenant := metadata.Dictionary[sec.TenantRef]
        sections = append(sections, &Section{
            Type:   typ,
            Reader: o.dec.SectionReader(metadata, sec, sec.ExtensionData),
            Tenant: tenant,
        })
        tenants[tenant] = struct{}{}
    }

    o.metadata = metadata
    o.sections = sections
    o.tenants = make([]string, 0, len(tenants))
    for tenant := range tenants {
        o.tenants = append(o.tenants, tenant)
    }
    return nil
}
```

### 읽기 최적화 기법

DataObj 디코더는 다음과 같은 최적화 기법을 적용한다:

```
+-----------------------------------------------------------+
|  읽기 최적화 기법                                           |
+-----------------------------------------------------------+
|                                                            |
|  1. Range Coalescing (범위 병합)                            |
|     - 인접한 페이지를 하나의 요청으로 통합                   |
|     - Object Storage API 호출 수 감소                       |
|                                                            |
|  2. Parallel Reads (병렬 읽기)                              |
|     - 여러 페이지를 동시에 가져오기                          |
|     - errgroup 활용                                        |
|                                                            |
|  3. Prefetching (프리페치)                                  |
|     - 현재 배치 처리 중 다음 페이지 미리 가져오기             |
|     - minimumPrefetchBytes = 16KB                          |
|                                                            |
|  4. Predicate Pushdown (프레디킷 푸시다운)                   |
|     - 페이지 통계(min/max) 기반 필터링                       |
|     - 불필요한 페이지 전체 스킵                              |
|                                                            |
+-----------------------------------------------------------+
```

---

## 7. 섹션 타입별 상세 분석

### 7.1 Logs Section

로그 레코드를 컬럼나 형식으로 저장하는 핵심 섹션이다.

**소스 참조**: `pkg/dataobj/sections/logs/logs.go` (라인 14-18):
```go
var sectionType = dataobj.SectionType{
    Namespace: "github.com/grafana/loki",
    Kind:      "logs",
    Version:   columnar.FormatVersion,
}
```

#### 컬럼 구성

```
Logs Section 컬럼 구조:
+-------------+---------+------------------------------------------+
| 컬럼        | 타입    | 설명                                      |
+-------------+---------+------------------------------------------+
| stream_id   | int64   | 스트림 식별자                              |
| timestamp   | int64   | 나노초 타임스탬프                          |
| metadata.*  | binary  | 구조화된 메타데이터 (키당 하나의 컬럼)       |
| message     | binary  | 로그 라인 본문                             |
+-------------+---------+------------------------------------------+

각 컬럼은 페이지 단위로 분할:
+--------+  +--------+  +--------+
| Page 1 |  | Page 2 |  | Page 3 |
| 256KB  |  | 256KB  |  | 128KB  |
| zstd   |  | zstd   |  | zstd   |
+--------+  +--------+  +--------+
```

#### 정렬 순서

Logs Section은 두 가지 정렬 순서를 지원한다:

```
1. stream-asc (기본값):
   PRIMARY:   stream_id ASC
   SECONDARY: timestamp DESC

2. timestamp-desc:
   PRIMARY:   timestamp DESC
   SECONDARY: stream_id ASC
```

#### 페이지 내부 구조

각 페이지는 독립적으로 압축되며 다음 정보를 메타데이터에 포함한다:
- 오프셋과 크기
- 행 수 (row count)
- 압축 방식
- 컬럼 통계 (min/max 값)

### 7.2 Streams Section

데이터 오브젝트에 포함된 스트림의 메타데이터와 통계 정보를 저장한다.

**소스 참조**: `pkg/dataobj/sections/streams/streams.go` (라인 13-17):
```go
var sectionType = dataobj.SectionType{
    Namespace: "github.com/grafana/loki",
    Kind:      "streams",
    Version:   columnar.FormatVersion,
}
```

#### 컬럼 구성

```
Streams Section 컬럼 구조:
+-------------------+---------+--------------------------------------+
| 컬럼              | 타입    | 설명                                  |
+-------------------+---------+--------------------------------------+
| stream_id         | int64   | 고유 스트림 식별자                     |
| min_timestamp     | int64   | 스트림 내 최소 타임스탬프               |
| max_timestamp     | int64   | 스트림 내 최대 타임스탬프               |
| labels.*          | binary  | 레이블 키-값 (키당 하나의 컬럼)         |
| rows              | uint64  | 로그 레코드 수                        |
| uncompressed_size | uint64  | 비압축 데이터 크기                     |
+-------------------+---------+--------------------------------------+
```

### 7.3 Pointers Section

인덱스 오브젝트에서 데이터 오브젝트의 특정 섹션을 참조하기 위한 포인터 정보를 저장한다.

**소스 참조**: `pkg/dataobj/sections/pointers/pointers.go` (라인 15-19):
```go
var sectionType = dataobj.SectionType{
    Namespace: "github.com/grafana/loki",
    Kind:      "pointers",
    Version:   columnar.FormatVersion,
}
```

#### 포인터 종류

```
Pointers Section - 두 가지 포인터 유형:

1. Stream Pointer:
   +-----------------------------------------------------------+
   | path           | 데이터 오브젝트 Object Storage 경로       |
   | section        | 참조하는 섹션 번호                        |
   | stream_id      | 이 인덱스 오브젝트의 스트림 ID            |
   | stream_id_ref  | 대상 데이터 오브젝트의 스트림 ID           |
   | min_timestamp  | 해당 스트림의 최소 타임스탬프              |
   | max_timestamp  | 해당 스트림의 최대 타임스탬프              |
   | row_count      | 해당 스트림의 행 수                       |
   | uncompressed_size | 해당 스트림의 비압축 크기               |
   +-----------------------------------------------------------+

2. Column Pointer:
   +-----------------------------------------------------------+
   | path              | 데이터 오브젝트 경로                   |
   | section           | 참조하는 섹션 번호                     |
   | column_name       | 참조 컬럼 이름                         |
   | column_index      | 참조 컬럼 인덱스 번호                   |
   | values_bloom_filter| 고유 값 블룸 필터                      |
   +-----------------------------------------------------------+
```

### 7.4 IndexPointers Section

Table of Contents(ToC) 오브젝트에서 인덱스 오브젝트를 참조하기 위한 포인터이다.

```
IndexPointers Section 컬럼:
+-------------------+---------+--------------------------------------+
| 컬럼              | 타입    | 설명                                  |
+-------------------+---------+--------------------------------------+
| path              | binary  | 인덱스 파일 경로                       |
| min_time          | int64   | 커버하는 최소 시간                     |
| max_time          | int64   | 커버하는 최대 시간                     |
+-------------------+---------+--------------------------------------+
```

### 오브젝트 계층 구조

```
Table of Contents (ToC)
    |
    | IndexPointers Section
    | (path, min_time, max_time)
    v
Index Object
    |
    | Streams Section (인덱스된 스트림 메타데이터)
    | Pointers Section (데이터 오브젝트 참조)
    v
Data Object (Logs + Streams)
    |
    | Logs Section (실제 로그 데이터)
    | Streams Section (스트림 통계)
    v
로그 레코드
```

---

## 8. DataObj Consumer

### 개요

DataObj Consumer는 Kafka에서 로그 레코드를 소비하여 DataObj를 빌드하고 Object Storage에 업로드하는 서비스이다. Distributor가 Kafka에 기록한 레코드를 읽어 컬럼나 DataObj로 변환하는 역할을 한다.

### Service 구조

**소스 참조**: `pkg/dataobj/consumer/service.go` (라인 37-52):
```go
type Service struct {
    services.Service
    cfg                         Config
    metastoreEvents             *kgo.Client             // Metastore 이벤트 발행
    lifecycler                  *ring.Lifecycler         // Ring 라이프사이클
    partitionInstanceLifecycler *ring.PartitionInstanceLifecycler
    consumer                    *kafkav2.SinglePartitionConsumer
    offsetReader                *kafkav2.OffsetReader
    partition                   int32                    // 담당 파티션
    processor                   *processor
    flusher                     *flusherImpl
    downscalePermitted          downscalePermittedFunc
    watcher                     *services.FailureWatcher
    logger                      log.Logger
    reg                         prometheus.Registerer
}
```

### 초기화 흐름

**소스 참조**: `pkg/dataobj/consumer/service.go` `New()` (라인 54-187):

```
DataObj Consumer 초기화 흐름:

1. Metastore Events 클라이언트 생성
   - Topic: "loki.metastore-events"
   - 업로드 완료 이벤트 발행용

2. Ring Lifecycler 설정
   - Key: "dataobj-consumer"
   - 인스턴스 ID에서 파티션 ID 추출

3. Partition Ring 설정
   - 파티션 활성/비활성 상태 관리
   - Distributor가 활성 파티션만 사용

4. Kafka Reader Client 설정
   - Topic: cfg.Topic (DataObj용 전용 토픽)
   - SinglePartitionConsumer 생성

5. Processor 파이프라인 구성
   - BuilderFactory → Builder 생성
   - Sorter → 정렬 (CopyAndSort)
   - Flusher → 업로드
   - FlushManager → 커밋 조율
   - Processor → 레코드 처리
```

핵심 코드:

```go
// Kafka로부터의 레코드 채널 생성
records := make(chan *kgo.Record)
s.consumer = kafkav2.NewSinglePartitionConsumer(
    readerClient,
    cfg.Topic,
    partitionID,
    kafkav2.OffsetStart,
    records,
    logger, ...)

// Uploader와 Sorter 설정
uploader := dataobj_uploader.New(cfg.UploaderConfig, bucket, logger)
builderFactory := logsobj.NewBuilderFactory(cfg.BuilderConfig, scratchStore)
sorter := logsobj.NewSorter(builderFactory, reg)
s.flusher = newFlusher(sorter, uploader, logger, reg)

// Processor 생성
s.processor = newProcessor(
    builder, records, flushManager,
    cfg.IdleFlushTimeout, cfg.MaxBuilderAge,
    logger, wrapped,
)
```

### 서비스 라이프사이클

**소스 참조**: `pkg/dataobj/consumer/service.go` (라인 190-235):

```
+------------------+
|    starting()    |
+--------+---------+
         |
         v
+------------------+     +---------------------------+
|  initResumeOffset|---->| Kafka에서 마지막 커밋     |
|  (backoff 3회)   |     | 오프셋 조회               |
+--------+---------+     +---------------------------+
         |
         v
+------------------+
|  lifecycler 시작 |  Ring에 등록
+--------+---------+
         |
         v
+------------------+
| partitionInstance |  파티션을 Active로 선언
| Lifecycler 시작  |
+--------+---------+
         |
         v
+------------------+
|  processor 시작  |  레코드 처리 시작
+--------+---------+
         |
         v
+------------------+
|  consumer 시작   |  Kafka 소비 시작
+--------+---------+
         |
         v
+------------------+
|    running()     |  ctx.Done() 대기
+------------------+
```

### Processor: 레코드 처리 핵심 루프

**소스 참조**: `pkg/dataobj/consumer/processor.go` (라인 38-78):
```go
type processor struct {
    *services.BasicService
    builder      builder
    decoder      *kafka.Decoder
    records      chan *kgo.Record
    flushManager flushManager

    lastOffset       int64
    idleFlushTimeout time.Duration
    maxBuilderAge    time.Duration
    firstAppend      time.Time
    lastAppend       time.Time
    earliestRecordTime time.Time

    metrics *metrics
    logger  log.Logger
}
```

#### Run 루프

**소스 참조**: `pkg/dataobj/consumer/processor.go` (라인 122-149):
```go
func (p *processor) Run(ctx context.Context) error {
    for {
        select {
        case <-ctx.Done():
            return nil
        case rec, ok := <-p.records:
            if !ok {
                return nil
            }
            if err := p.processRecord(ctx, rec); err != nil {
                level.Error(p.logger).Log("msg", "failed to process record", "err", err)
                p.observeRecordErr(rec)
            }
        case <-time.After(p.idleFlushTimeout):
            // 파티션이 유휴 상태 → 플러시
            p.metrics.setConsumptionLag(0)
            if _, err := p.idleFlush(ctx); err != nil {
                level.Error(p.logger).Log("msg", "failed to idle flush", "err", err)
            }
        }
    }
}
```

#### processRecord: 레코드 처리 로직

```
레코드 처리 흐름:

                +------------------+
                | Kafka Record     |
                | Key: tenant      |
                | Value: stream    |
                +--------+---------+
                         |
                         v
                +------------------+
                | decoder.Decode   |  스트림 디코딩
                | WithoutLabels()  |
                +--------+---------+
                         |
                         v
               +--------------------+
               | maxAge 검사        |  firstAppend + maxBuilderAge
               | → 초과시 flush     |
               +--------+-----------+
                         |
                         v
               +--------------------+
               | builder.Append     |  빌더에 추가
               | (tenant, stream)   |
               +--------+-----------+
                   |          |
              +----+          +-----+
              |                     |
         [성공]                [ErrBuilderFull]
              |                     |
              v                     v
    +------------------+   +------------------+
    | lastOffset 갱신  |   | flush() 실행     |
    | firstAppend 기록 |   | 재시도 Append    |
    +------------------+   +------------------+
```

**소스 참조**: `pkg/dataobj/consumer/processor.go` (라인 151-193):
```go
func (p *processor) processRecord(ctx context.Context, rec *kgo.Record) error {
    now := time.Now()
    p.observeRecord(rec, now)

    tenant := string(rec.Key)
    stream, err := p.decoder.DecodeWithoutLabels(rec.Value)
    if err != nil {
        return fmt.Errorf("failed to decode stream: %w", err)
    }

    if p.shouldFlushDueToMaxAge() {
        if err := p.flush(ctx, flushReasonMaxAge); err != nil {
            return fmt.Errorf("failed to flush: %w", err)
        }
    }

    if err := p.builder.Append(tenant, stream); err != nil {
        if !errors.Is(err, logsobj.ErrBuilderFull) {
            return fmt.Errorf("failed to append stream: %w", err)
        }
        // 빌더가 가득 → 플러시 후 재시도
        if err := p.flush(ctx, flushReasonBuilderFull); err != nil {
            return fmt.Errorf("failed to flush and commit: %w", err)
        }
        if err := p.builder.Append(tenant, stream); err != nil {
            return fmt.Errorf("failed to append stream after flushing: %w", err)
        }
    }
    ...
}
```

### 플러시 트리거 조건

```
+-----------------------------------------------------------+
|  플러시 트리거 조건 (3가지)                                 |
+-----------------------------------------------------------+
|                                                            |
|  1. builderFull                                            |
|     - currentSizeEstimate > TargetObjectSize               |
|     - builder.Append()가 ErrBuilderFull 반환               |
|                                                            |
|  2. maxAge                                                 |
|     - time.Since(firstAppend) > maxBuilderAge              |
|     - 파티션이 충분한 데이터를 받지 못할 때 강제 플러시      |
|     - 작은 DataObj라도 쿼리 가능하도록                      |
|                                                            |
|  3. idle                                                   |
|     - time.Since(lastAppend) > idleFlushTimeout             |
|     - 파티션 비활성화(스케일다운 준비) 시 발생               |
|     - Consumption lag을 0으로 초기화                        |
|                                                            |
+-----------------------------------------------------------+
```

---

## 9. Builder 아키텍처

### logsobj.Builder: 로그 오브젝트 빌더

DataObj Consumer가 사용하는 고수준 빌더이다. 멀티 테넌트를 지원하며, 테넌트별로 별도의 logs/streams 빌더를 관리한다.

**소스 참조**: `pkg/dataobj/consumer/logsobj/builder.go` (라인 174-197):
```go
type Builder struct {
    cfg     BuilderConfig
    metrics *builderMetrics

    labelCache *lru.Cache[string, labels.Labels]  // 레이블 파싱 캐시 (5000 엔트리)

    currentSizeEstimate int

    builder *dataobj.Builder              // 내부 dataobj.Builder
    streams map[string]*streams.Builder   // 테넌트별 스트림 빌더
    logs    map[string]*logs.Builder      // 테넌트별 로그 빌더

    state builderState                    // empty / dirty
}
```

### 빌더 설정 값

**소스 참조**: `pkg/dataobj/consumer/logsobj/builder.go` (라인 127-137):
```go
func (cfg *BuilderConfig) RegisterFlagsWithPrefix(prefix string, f *flag.FlagSet) {
    _ = cfg.TargetPageSize.Set("2MB")          // 페이지 크기 2MB
    _ = cfg.TargetObjectSize.Set("1GB")        // 오브젝트 크기 1GB
    _ = cfg.BufferSize.Set("16MB")             // 버퍼 크기 16MB
    _ = cfg.TargetSectionSize.Set("128MB")     // 섹션 크기 128MB
    ...
}
```

### Append 흐름

```
logsobj.Builder.Append(tenant, stream):

1. 레이블 파싱 (LRU 캐시 활용)
   |
2. 크기 초과 검사
   currentSizeEstimate + labelsEstimate + streamSizeEstimate > TargetObjectSize
   → ErrBuilderFull 반환
   |
3. 테넌트별 빌더 초기화 (initBuilder)
   |
4. 엔트리별 처리:
   for entry in stream.Entries:
     a. streams.Builder.Record(labels, timestamp, size)
        → streamID 반환
     b. logs.Builder.Append(Record{StreamID, Timestamp, Metadata, Line})
     c. TargetSectionSize 초과 시 → 내부 dataobj.Builder.Append(lb)
   |
5. currentSizeEstimate 갱신
   state = builderStateDirty
```

**소스 참조**: `pkg/dataobj/consumer/logsobj/builder.go` (라인 255-307):
```go
func (b *Builder) Append(tenant string, stream logproto.Stream) error {
    ls, err := b.parseLabels(stream.Labels)
    ...
    if b.state != builderStateEmpty &&
       b.currentSizeEstimate+labelsEstimate(ls)+streamSizeEstimate(stream) > int(b.cfg.TargetObjectSize) {
        return ErrBuilderFull
    }

    b.initBuilder(tenant)
    sb, lb := b.streams[tenant], b.logs[tenant]

    for _, entry := range stream.Entries {
        sz := int64(len(entry.Line))
        for _, md := range entry.StructuredMetadata {
            sz += int64(len(md.Value))
        }

        streamID := sb.Record(ls, entry.Timestamp, sz)
        lb.Append(logs.Record{
            StreamID:  streamID,
            Timestamp: entry.Timestamp,
            Metadata:  convertMetadata(entry.StructuredMetadata),
            Line:      []byte(entry.Line),
        })

        // 섹션 크기 제한 → 중간 플러시
        if lb.UncompressedSize() > int(b.cfg.TargetSectionSize) {
            if err := b.builder.Append(lb); err != nil {
                return err
            }
            lb.SetTenant(tenant)
        }
    }
    ...
}
```

### 크기 추정 로직

빌더는 메모리 사용량을 제어하기 위해 압축 비율을 추정한다:

**소스 참조**: `pkg/dataobj/consumer/logsobj/builder.go` (라인 324-353):
```go
// 레이블 크기 추정: 키는 그대로, 값은 2x 압축률 가정
func labelsEstimate(ls labels.Labels) int {
    var keysSize, valuesSize int
    ls.Range(func(l labels.Label) {
        keysSize += len(l.Name)
        valuesSize += len(l.Value)
    })
    return keysSize + valuesSize/2
}

// 스트림 크기 추정: 라인은 2x 압축률, 타임스탬프/ID는 무시
func streamSizeEstimate(stream logproto.Stream) int {
    var size int
    for _, entry := range stream.Entries {
        size += len(entry.Line) / 2
        for _, md := range entry.StructuredMetadata {
            size += len(md.Name) + len(md.Value)/2
        }
    }
    return size
}
```

### CopyAndSort: 오브젝트 재정렬

빌드된 DataObj는 여러 섹션의 로그가 전역적으로 정렬되지 않을 수 있다. CopyAndSort는 기존 오브젝트를 읽어 로그를 전역 정렬된 새 오브젝트로 재구성한다:

**소스 참조**: `pkg/dataobj/consumer/logsobj/builder.go` (라인 440-556):
```go
func (b *Builder) CopyAndSort(ctx context.Context, obj *dataobj.Object) (*dataobj.Object, io.Closer, error) {
    defer b.Reset()

    sort := parseSortOrder(b.cfg.DataobjSortOrder)
    ...
    // 테넌트별 처리 (자연 정렬)
    tenants := obj.Tenants()
    natsort.Sort(tenants)

    for _, tenant := range tenants {
        // 1. Streams 섹션 복사
        for _, sec := range obj.Sections().Filter(...streams...) {
            ...
        }

        // 2. Logs 섹션을 정렬-병합 이터레이터로 재구성
        iter, err := sortMergeIterator(ctx, sections, sort)
        ...
        for rec := range iter {
            val, err := rec.Value()
            lb.Append(val)

            if lb.UncompressedSize() > int(b.cfg.TargetSectionSize) {
                b.builder.Append(lb)
                lb.Reset()
                lb.SetTenant(tenant)
            }
        }
        b.builder.Append(lb)
    }

    return b.builder.Flush()
}
```

### Sorter 서비스

**소스 참조**: `pkg/dataobj/consumer/logsobj/sorter.go` (라인 14-46):
```go
type Sorter struct {
    factory  *BuilderFactory
    duration prometheus.Histogram  // loki_dataobj_sort_duration_seconds
}

func (s *Sorter) Sort(ctx context.Context, obj *dataobj.Object) (*dataobj.Object, io.Closer, error) {
    b, err := s.factory.NewBuilder(nil)
    ...
    t := prometheus.NewTimer(s.duration)
    defer t.ObserveDuration()
    return b.CopyAndSort(ctx, obj)
}
```

---

## 10. Uploader와 Object Storage 경로

### Uploader 구조

**소스 참조**: `pkg/dataobj/uploader/uploader.go` (라인 38-54):
```go
type Uploader struct {
    SHAPrefixSize int           // SHA 프리픽스 크기 (기본값 2)
    bucket        objstore.Bucket
    metrics       *metrics
    logger        log.Logger
}

func New(cfg Config, bucket objstore.Bucket, logger log.Logger) *Uploader {
    return &Uploader{
        SHAPrefixSize: cfg.SHAPrefixSize,
        bucket:        bucket,
        metrics:       newMetrics(cfg.SHAPrefixSize),
        logger:        logger,
    }
}
```

### SHA 기반 경로 생성

DataObj의 Object Storage 경로는 파일 내용의 SHA-224 해시를 기반으로 생성된다:

**소스 참조**: `pkg/dataobj/uploader/uploader.go` (라인 65-83):
```go
func (d *Uploader) getKey(ctx context.Context, object *dataobj.Object) (string, error) {
    hash := sha256.New224()

    reader, err := object.Reader(ctx)
    ...
    if _, err := io.Copy(hash, reader); err != nil {
        return "", err
    }

    var sumBytes [sha256.Size224]byte
    sum := hash.Sum(sumBytes[:0])
    sumStr := hex.EncodeToString(sum)

    return fmt.Sprintf("objects/%s/%s", sumStr[:d.SHAPrefixSize], sumStr[d.SHAPrefixSize:]), nil
}
```

경로 예시:
```
SHA-224: "ab1234567890..."
SHAPrefixSize: 2

경로: objects/ab/1234567890...

디렉토리 구조:
objects/
  ab/
    1234567890abcdef...
    9876543210fedcba...
  cd/
    aabbccddee112233...
```

SHA 프리픽스로 디렉토리를 분할하는 이유:
- Object Storage의 파티션 분산 (핫스팟 방지)
- 같은 내용의 오브젝트는 같은 경로 → 중복 업로드 자연 방지 (content-addressable)

### 업로드 재시도 전략

**소스 참조**: `pkg/dataobj/uploader/uploader.go` (라인 86-138):
```go
func (d *Uploader) Upload(ctx context.Context, object *dataobj.Object) (key string, err error) {
    ...
    backoff := backoff.New(ctx, backoff.Config{
        MinBackoff: 100 * time.Millisecond,   // 최소 대기
        MaxBackoff: 10 * time.Second,         // 최대 대기
        MaxRetries: 20,                       // 최대 재시도 20회
    })

    for backoff.Ongoing() {
        err = func() error {
            reader, err := object.Reader(ctx)
            ...
            return d.bucket.Upload(ctx, objectPath, reader)
        }()
        if err == nil {
            break
        }
        backoff.Wait()
    }
    ...
}
```

```
업로드 재시도 타이밍:

시도 1: 즉시
시도 2: +100ms
시도 3: +200ms
시도 4: +400ms
...
시도 N: +min(100ms * 2^(N-2), 10s)
최대 20회
```

---

## 11. Metastore와 인덱스 계층

### 3계층 오브젝트 구조

DataObj 시스템은 3계층 오브젝트 구조를 형성한다:

```
+-----------------------------------------------------------+
|  Layer 1: Table of Contents (ToC)                          |
|                                                            |
|  저장 위치: tocs/                                          |
|  내용: IndexPointers Section                               |
|  역할: 시간 범위 → 인덱스 오브젝트 매핑                      |
|                                                            |
|  +-------+  +-------+  +-------+                          |
|  |ToC A  |  |ToC B  |  |ToC C  |                          |
|  |12시간 |  |12시간 |  |12시간 |                          |
|  +---+---+  +---+---+  +---+---+                          |
|      |          |          |                               |
+------|----------|----------|-------------------------------+
       v          v          v
+-----------------------------------------------------------+
|  Layer 2: Index Objects                                    |
|                                                            |
|  내용: Streams Section + Pointers Section                  |
|  역할: 스트림/레이블 → 데이터 오브젝트 매핑                   |
|  블룸 필터: 컬럼 값 빠른 존재 확인                           |
|                                                            |
|  +----------+  +----------+  +----------+                 |
|  |Index X   |  |Index Y   |  |Index Z   |                 |
|  |streams   |  |streams   |  |streams   |                 |
|  |pointers  |  |pointers  |  |pointers  |                 |
|  +----+-----+  +----+-----+  +----+-----+                 |
|       |              |              |                      |
+-------|--------------|--------------|----------------------+
        v              v              v
+-----------------------------------------------------------+
|  Layer 3: Data Objects                                     |
|                                                            |
|  저장 위치: objects/{sha_prefix}/{sha_rest}                 |
|  내용: Logs Section + Streams Section                      |
|  역할: 실제 로그 데이터 저장                                 |
|                                                            |
|  +------------+  +------------+  +------------+           |
|  |DataObj 1   |  |DataObj 2   |  |DataObj 3   |           |
|  |logs        |  |logs        |  |logs        |           |
|  |streams     |  |streams     |  |streams     |           |
|  +------------+  +------------+  +------------+           |
+-----------------------------------------------------------+
```

### Metastore 인터페이스

**소스 참조**: `pkg/dataobj/metastore/metastore.go` (라인 11-24):
```go
type Metastore interface {
    Sections(ctx context.Context, req SectionsRequest) (SectionsResponse, error)
    GetIndexes(ctx context.Context, req GetIndexesRequest) (GetIndexesResponse, error)
    IndexSectionsReader(ctx context.Context, req IndexSectionsReaderRequest) (IndexSectionsReaderResponse, error)
    CollectSections(ctx context.Context, req CollectSectionsRequest) (CollectSectionsResponse, error)
    Labels(ctx context.Context, start, end time.Time, matchers ...*labels.Matcher) ([]string, error)
    Values(ctx context.Context, start, end time.Time, matchers ...*labels.Matcher) ([]string, error)
}
```

### ObjectMetastore: 구현체

**소스 참조**: `pkg/dataobj/metastore/object.go` (라인 43-58):
```go
const metastoreWindowSize = 12 * time.Hour
const TocPrefix = "tocs/"

type ObjectMetastore struct {
    bucket      objstore.Bucket
    parallelism int
    logger      log.Logger
    metrics     *ObjectMetastoreMetrics
}
```

### 쿼리 해석 흐름

```
쿼리: {app="nginx"} | json | level="error"
시간 범위: [T1, T2]

1. ToC 탐색
   - tocs/ 디렉토리에서 [T1, T2] 범위에 해당하는 ToC 파일 탐색
   - 12시간 윈도우 단위로 파티셔닝

2. 인덱스 해석
   - ToC의 IndexPointers에서 관련 인덱스 오브젝트 경로 확인
   - 인덱스 오브젝트의 Streams Section에서 {app="nginx"} 매칭
   - Pointers Section에서 해당 스트림의 데이터 오브젝트 위치 확인

3. 데이터 읽기
   - 데이터 오브젝트의 Logs Section을 열어 로그 읽기
   - Predicate Pushdown으로 불필요한 페이지 스킵
   - Arrow RecordBatch로 변환하여 반환
```

### DataobjSectionDescriptor

Metastore가 반환하는 섹션 디스크립터:

**소스 참조**: `pkg/dataobj/metastore/object.go` (라인 67-78):
```go
type DataobjSectionDescriptor struct {
    SectionKey

    StreamIDs []int64
    RowCount  int
    Size      int64
    Start     time.Time
    End       time.Time

    AmbiguousPredicatesByStream map[int64][]string
}
```

### Table of Contents Writer

ToC를 생성하고 업로드하는 역할:

**소스 참조**: `pkg/dataobj/metastore/toc_writer.go` (라인 26-36):
```go
var tocBuilderCfg = logsobj.BuilderBaseConfig{
    TargetObjectSize:  32 * 1024 * 1024,   // 32MB
    TargetPageSize:    4 * 1024 * 1024,    // 4MB
    BufferSize:        32 * 1024 * 1024,   // 32MB
    TargetSectionSize: 4 * 1024 * 1024,    // 4MB
    SectionStripeMergeLimit: 2,
}
```

### Index Builder

인덱스 오브젝트를 생성하는 빌더:

**소스 참조**: `pkg/dataobj/index/builder.go` (라인 29-35):
```go
type triggerType string

const (
    triggerTypeAppend  triggerType = "append"   // 이벤트 누적
    triggerTypeMaxIdle triggerType = "max-idle" // 유휴 타임아웃
)
```

인덱스 빌더는 Metastore Events Kafka 토픽에서 `ObjectWrittenEvent`를 소비하여 인덱스를 구성한다. 데이터 오브젝트가 업로드될 때마다 이벤트가 발생하고, 인덱스 빌더가 해당 오브젝트의 스트림 정보와 포인터를 인덱스 오브젝트에 기록한다.

---

## 12. Explorer와 Inspect 도구

### Explorer Service

DataObj를 웹 UI로 탐색할 수 있는 HTTP 서비스이다.

**소스 참조**: `pkg/dataobj/explorer/service.go` (라인 14-47):
```go
type Service struct {
    *services.BasicService
    bucket objstore.Bucket
    logger log.Logger
}

func (s *Service) Handler() (string, http.Handler) {
    mux := http.NewServeMux()

    mux.HandleFunc("/dataobj/api/v1/list", s.handleList)
    mux.HandleFunc("/dataobj/api/v1/inspect", s.handleInspect)
    mux.HandleFunc("/dataobj/api/v1/download", s.handleDownload)
    mux.HandleFunc("/dataobj/api/v1/provider", s.handleProvider)

    return "/dataobj", mux
}
```

### API 엔드포인트

```
DataObj Explorer API:

GET /dataobj/api/v1/list
  - Object Storage의 DataObj 파일 목록 반환
  - 버킷 내 objects/ 디렉토리 탐색

GET /dataobj/api/v1/inspect?path=objects/ab/cd123...
  - 특정 DataObj의 구조 분석
  - 섹션 목록, 컬럼 정보, 통계 반환

GET /dataobj/api/v1/download?path=objects/ab/cd123...
  - DataObj 파일 다운로드

GET /dataobj/api/v1/provider
  - Object Storage 프로바이더 정보 반환
  - 예: {"provider": "S3"}, {"provider": "GCS"}
```

### Inspect 도구

CLI 기반으로 DataObj 파일의 내부 구조를 분석하는 도구이다.

**소스 참조**: `pkg/dataobj/tools/inspect.go` (라인 16-42):
```go
func Inspect(r io.ReaderAt, size int64) {
    obj, err := dataobj.FromReaderAt(r, size)
    if err != nil {
        log.Printf("failed to open object: %v", err)
        return
    }

    for _, section := range obj.Sections() {
        switch {
        case streams.CheckSection(section):
            streamsSection, err := streams.Open(context.Background(), section)
            ...
            printStreamInfo(streamsSection)

        case logs.CheckSection(section):
            logsSection, err := logs.Open(context.Background(), section)
            ...
            printLogsInfo(logsSection)
        }
    }
}
```

#### Inspect 출력 예시

```
---- Streams Section ----
streams/stream_id[]; 1523 populated rows; 12 kB compressed (zstd); 24 kB uncompressed
streams/min_timestamp[]; 1523 populated rows; 8 kB compressed (zstd); 12 kB uncompressed
streams/max_timestamp[]; 1523 populated rows; 8 kB compressed (zstd); 12 kB uncompressed
streams/labels[app]; 1523 populated rows; 3 kB compressed (zstd); 15 kB uncompressed
streams/labels[namespace]; 1523 populated rows; 1 kB compressed (zstd); 8 kB uncompressed
streams/rows[]; 1523 populated rows; 4 kB compressed (zstd); 12 kB uncompressed
streams/uncompressed_size[]; 1523 populated rows; 6 kB compressed (zstd); 12 kB uncompressed

Streams Section Summary: 7 columns; compressed size: 42 kB; uncompressed size 95 kB

---- Logs Section ----
logs/stream_id[]; 523000 populated rows; 1.2 MB compressed (zstd); 4.0 MB uncompressed
logs/timestamp[]; 523000 populated rows; 800 kB compressed (zstd); 4.0 MB uncompressed
logs/metadata[level]; 523000 populated rows; 120 kB compressed (zstd); 2.1 MB uncompressed
logs/message[]; 523000 populated rows; 45 MB compressed (zstd); 180 MB uncompressed

Logs Section Summary: 4 columns; compressed size: 47.1 MB; uncompressed size 190.1 MB
```

**소스 참조**: `pkg/dataobj/tools/inspect.go` (라인 44-72):
```go
func printStreamInfo(sec *streams.Section) {
    fmt.Println("---- Streams Section ----")
    stats, err := streams.ReadStats(context.Background(), sec)
    ...
    for _, col := range stats.Columns {
        fmt.Printf("%v[%v]; %d populated rows; %v compressed (%v); %v uncompressed\n",
            col.Type[12:], col.Name, col.ValuesCount,
            humanize.Bytes(col.CompressedSize), col.Compression[17:],
            humanize.Bytes(col.UncompressedSize))
    }
    fmt.Printf("Streams Section Summary: %d columns; compressed size: %v; uncompressed size %v\n",
        len(stats.Columns), humanize.Bytes(stats.CompressedSize), humanize.Bytes(stats.UncompressedSize))
}
```

---

## 13. 청크 기반 스토리지와의 비교

### 아키텍처 비교

```
+-----------------------------------------------------------+
|  청크 기반 스토리지 (기존)                                   |
+-----------------------------------------------------------+
|                                                            |
|  Distributor → Ingester → Chunk Store → Object Storage     |
|                    |                                       |
|                    v                                       |
|              BoltDB/TSDB                                   |
|              (인덱스 DB)                                   |
|                                                            |
|  특징:                                                     |
|  - Ingester가 메모리에 로그 버퍼링                          |
|  - 청크 단위 gzip 압축                                     |
|  - 인덱스는 별도 DB (BoltDB → TSDB)                        |
|  - 스트림당 다수의 작은 청크 파일                            |
+-----------------------------------------------------------+

+-----------------------------------------------------------+
|  DataObj 기반 스토리지 (신규)                                |
+-----------------------------------------------------------+
|                                                            |
|  Distributor → Kafka → DataObj Consumer → Object Storage   |
|                                  |                         |
|                                  v                         |
|                        Index Builder → Metastore           |
|                                                            |
|  특징:                                                     |
|  - Kafka가 버퍼 역할 (Ingester 대체 가능)                   |
|  - 컬럼나 섹션별 zstd 압축                                  |
|  - 인덱스가 DataObj 포맷 내 자체 포함                        |
|  - 시간 윈도우당 소수의 큰 DataObj 파일                      |
+-----------------------------------------------------------+
```

### 상세 비교표

| 측면 | 청크 기반 | DataObj |
|------|----------|---------|
| **데이터 구조** | 행 기반 (로그 라인 순차) | 컬럼나 (필드별 분리) |
| **파일 크기** | 수 MB (청크) | 수백 MB ~ 1GB |
| **파일 수** | 매우 많음 | 상대적으로 적음 |
| **인덱스** | 외부 DB (BoltDB/TSDB) | 자체 포함 (Index Object) |
| **압축** | gzip (행 단위) | zstd (컬럼/페이지 단위) |
| **필터링** | 전체 디코딩 후 | Predicate Pushdown |
| **쿼리 엔진** | LogQL → Iterator | LogQL → Arrow RecordBatch |
| **버퍼링** | Ingester (상태유지) | Kafka (상태비의존) |
| **멀티 테넌트** | 테넌트별 분리 저장 | 하나의 파일에 멀티 테넌트 |
| **스키마 진화** | 마이그레이션 필요 | 섹션 버전으로 자연 진화 |
| **Object Storage 비용** | API 호출 많음 | API 호출 적음 |

### I/O 패턴 비교

```
청크 기반 쿼리 I/O:
  1. 인덱스 DB에서 청크 참조 조회        → 1 round trip
  2. 각 청크 파일 전체 다운로드            → N round trips
  3. 전체 청크를 메모리에서 디코딩          → CPU 집약
  총: 1 + N round trips, 전체 데이터 전송

DataObj 쿼리 I/O:
  1. ToC에서 인덱스 오브젝트 조회          → 1 round trip
  2. 인덱스 오브젝트에서 데이터 위치 조회   → 1 round trip
  3. DataObj 메타데이터 읽기 (16KB)        → 1 round trip
  4. 필요한 컬럼의 필요한 페이지만 읽기     → M round trips (M << N)
  총: 3 + M round trips, 선택적 데이터 전송
```

---

## 14. 스키마 진화와 호환성

### SectionType 버전 관리

섹션 타입은 Namespace, Kind, Version의 조합으로 식별된다. Version 필드를 통해 동일한 종류의 섹션이라도 인코딩 버전을 구분할 수 있다:

```go
// 현재 Logs 섹션
SectionType{
    Namespace: "github.com/grafana/loki",
    Kind:      "logs",
    Version:   3,  // columnar.FormatVersion
}

// 미래의 Logs 섹션 (새로운 인코딩)
SectionType{
    Namespace: "github.com/grafana/loki",
    Kind:      "logs",
    Version:   4,
}
```

### Equals 메서드: 버전 무관 비교

`CheckSection()` 함수는 `Equals()`를 사용하여 Namespace와 Kind만 비교하고, 실제 열기 시 Version을 검증한다:

**소스 참조**: `pkg/dataobj/section.go` (라인 62-64):
```go
func (ty SectionType) Equals(o SectionType) bool {
    return ty.Namespace == o.Namespace && ty.Kind == o.Kind
}
```

이를 통해 새로운 버전의 섹션을 인식하되, 지원하지 않는 버전은 에러로 처리한다:

```go
// logs.Open() 예시
func Open(ctx context.Context, section *dataobj.Section) (*Section, error) {
    if !CheckSection(section) {
        return nil, fmt.Errorf("section type mismatch")
    } else if section.Type.Version != columnar.FormatVersion {
        return nil, fmt.Errorf("unsupported section version: got=%d want=%d",
            section.Type.Version, columnar.FormatVersion)
    }
    ...
}
```

### 매직 바이트 진화

"THOR" → "DOBJ" 전환은 디코더의 자동 감지로 투명하게 처리된다:

```
파일 열기 시:
1. 첫 4바이트 읽기
2. "DOBJ" → 새 포맷 (헤더에 메타데이터)
3. "THOR" → 레거시 포맷 (끝에 메타데이터) → legacyMetadata() 호출
4. 그 외 → 에러
```

### 인식되지 않는 컬럼 처리

미래 버전에서 추가된 컬럼은 현재 코드에서 안전하게 무시된다:

**소스 참조**: `pkg/dataobj/sections/logs/logs.go` (라인 56-75):
```go
func (s *Section) init() error {
    for _, col := range s.inner.Columns() {
        colType, err := ParseColumnType(col.Type.Logical)
        if err != nil {
            // Skip over unrecognized columns; probably come from a newer
            // version of the code.
            continue
        }
        s.columns = append(s.columns, &Column{...})
    }
    return nil
}
```

### 새로운 섹션 타입 추가 가이드

**소스 참조**: `pkg/dataobj/dataobj.go` 패키지 주석 (라인 24-59):

```
새 섹션 타입 추가 절차:

1. 새 패키지 생성
   pkg/dataobj/sections/mysection/

2. SectionType 정의
   var sectionType = dataobj.SectionType{
       Namespace: "github.com/grafana/loki",
       Kind:      "mysection",
       Version:   1,
   }

3. SectionBuilder 구현
   type Builder struct { ... }
   func (b *Builder) Type() SectionType { ... }
   func (b *Builder) Flush(w SectionWriter) (int64, error) { ... }
   func (b *Builder) Reset() { ... }

4. 고수준 읽기 API 구현
   func Open(ctx context.Context, sec *dataobj.Section) (*Section, error) { ... }
   func CheckSection(sec *dataobj.Section) bool { ... }

5. 선택적 유틸리티
   - EstimateSize() 함수
   - ReadStats() 함수
   - Section 래퍼 타입
```

### 딕셔너리 확장성

딕셔너리는 파일별로 독립적이므로, 새로운 네임스페이스나 종류를 추가해도 기존 파일에 영향이 없다. 이는 서로 다른 조직이 독자적인 섹션 타입을 정의할 수 있게 한다:

```
예시:

// Grafana Loki 기본 섹션
{Namespace: "github.com/grafana/loki", Kind: "logs"}
{Namespace: "github.com/grafana/loki", Kind: "streams"}

// 사용자 정의 섹션 (충돌 없음)
{Namespace: "example.com/custom", Kind: "audit-logs"}
{Namespace: "example.com/custom", Kind: "metrics-summary"}
```

---

## 15. 운영 가이드

### 주요 설정 파라미터

```yaml
# DataObj Consumer 설정
dataobj_consumer:
  topic: "loki.dataobj"                    # DataObj 전용 Kafka 토픽
  idle_flush_timeout: 30s                  # 유휴 플러시 타임아웃
  max_builder_age: 5m                      # 빌더 최대 수명

  # Builder 설정
  builder:
    target_page_size: 2MB                  # 페이지 크기
    target_object_size: 1GB                # 오브젝트 크기
    buffer_size: 16MB                      # 버퍼 크기
    target_section_size: 128MB             # 섹션 크기
    section_stripe_merge_limit: 2          # 스트라이프 병합 한계
    dataobj_sort_order: "stream-asc"       # 정렬 순서

  # Uploader 설정
  uploader:
    sha_prefix_size: 2                     # SHA 프리픽스 크기

# Metastore 설정
metastore:
  partition_ratio: 1                       # 파티션 비율
```

### 모니터링 메트릭

```
+-----------------------------------------------------------+
|  DataObj Consumer 메트릭                                    |
+-----------------------------------------------------------+
|                                                            |
|  loki_dataobj_consumer_records_total                       |
|    - 처리된 레코드 총 수                                    |
|                                                            |
|  loki_dataobj_consumer_received_bytes_total                |
|    - 수신된 바이트 총량                                     |
|                                                            |
|  loki_dataobj_consumer_record_failures_total               |
|    - 처리 실패 레코드 수                                    |
|                                                            |
|  loki_dataobj_consumer_discarded_bytes_total               |
|    - 버려진 바이트 총량                                     |
|                                                            |
|  loki_dataobj_sort_duration_seconds                        |
|    - 오브젝트 정렬 소요 시간                                |
|                                                            |
+-----------------------------------------------------------+
|  Uploader 메트릭                                           |
+-----------------------------------------------------------+
|                                                            |
|  loki_dataobj_upload_total                                 |
|    - 업로드 시도 총 수                                     |
|                                                            |
|  loki_dataobj_upload_failures_total                        |
|    - 업로드 실패 수                                        |
|                                                            |
|  loki_dataobj_upload_time_seconds                          |
|    - 업로드 소요 시간                                      |
|                                                            |
|  loki_dataobj_upload_size_bytes{status="success|failure"}  |
|    - 업로드 오브젝트 크기                                   |
|                                                            |
+-----------------------------------------------------------+
|  Builder 메트릭                                            |
+-----------------------------------------------------------+
|                                                            |
|  loki_dataobj_builder_appends_total                        |
|    - Append 호출 수                                        |
|                                                            |
|  loki_dataobj_builder_append_time_seconds                  |
|    - Append 소요 시간                                      |
|                                                            |
|  loki_dataobj_builder_flush_failures_total                 |
|    - 플러시 실패 수                                        |
|                                                            |
|  loki_dataobj_builder_built_size_bytes                     |
|    - 빌드된 오브젝트 크기                                   |
|                                                            |
|  loki_dataobj_builder_size_estimate                        |
|    - 현재 빌더 크기 추정치 (gauge)                          |
|                                                            |
+-----------------------------------------------------------+
```

### 트러블슈팅 가이드

#### 증상: Consumer가 레코드를 처리하지 못함

```
원인 진단:
1. record_failures_total 메트릭 확인
   - 디코딩 실패 → Kafka 메시지 포맷 불일치
   - Append 실패 → Builder 설정 문제

2. consumption_lag 메트릭 확인
   - 증가 추세 → Consumer 처리 속도 부족
   - 해결: 파티션 수 증가 + Consumer 인스턴스 추가

3. 로그 확인
   "failed to decode stream" → 디코더 호환성 문제
   "failed to append stream after flushing" → 단일 스트림이 TargetObjectSize 초과
```

#### 증상: 업로드 실패가 지속됨

```
원인 진단:
1. upload_failures_total 메트릭 확인
   - 20회 재시도 후 실패 → Object Storage 연결 문제

2. upload_time_seconds 히스토그램 확인
   - p99 증가 → Object Storage 지연

3. 대응:
   - Object Storage 엔드포인트 연결 상태 확인
   - 버킷 권한 확인
   - 네트워크 정책 확인
```

#### 증상: 쿼리 성능 저하

```
원인 진단:
1. 오브젝트 크기 확인 (built_size_bytes)
   - 너무 작은 오브젝트가 많음 → maxBuilderAge 조정
   - 너무 큰 오브젝트 → TargetObjectSize 축소

2. Inspect 도구로 오브젝트 분석
   - 섹션 수 확인 (너무 많은 섹션 = 비효율)
   - 압축 비율 확인 (낮은 비율 = 데이터 특성 문제)
   - 페이지 크기 확인 (너무 큰 페이지 = 세밀한 필터링 불가)

3. 정렬 순서 확인
   - 쿼리 패턴에 맞는 정렬 순서 선택
   - stream-asc: 특정 스트림 쿼리에 유리
   - timestamp-desc: 최근 로그 쿼리에 유리
```

### 스케일링 전략

```
+-----------------------------------------------------------+
|  DataObj Consumer 스케일링                                   |
+-----------------------------------------------------------+
|                                                            |
|  수평 확장:                                                |
|  - Kafka 파티션 수 = Consumer 인스턴스 수                   |
|  - 파티션당 정확히 1개의 Consumer 인스턴스                   |
|  - Partition Ring으로 파티션 할당 관리                       |
|                                                            |
|  수직 확장:                                                |
|  - TargetObjectSize 조정 (메모리 사용량 결정)               |
|  - BufferSize 조정 (정렬 버퍼 크기)                         |
|  - scratch.Store를 디스크 기반으로 전환                      |
|                                                            |
|  스케일다운 안전성:                                         |
|  - Partition Ring에서 파티션 비활성화                        |
|  - 유휴 플러시 타임아웃으로 잔여 데이터 처리                  |
|  - 오프셋 커밋 확인 후 종료                                 |
|                                                            |
+-----------------------------------------------------------+
```

---

## 요약

DataObj는 Grafana Loki의 스토리지 진화를 대표하는 컬럼나 컨테이너 포맷이다:

1. **컬럼나 섹션 구조**: Logs, Streams, Pointers, IndexPointers 섹션으로 데이터를 유형별 분리 저장
2. **자체 완결 포맷**: 매직 바이트, 딕셔너리, 프로토버프 메타데이터로 외부 의존성 없는 파일 구조
3. **3계층 인덱싱**: ToC → Index Object → Data Object 계층으로 효율적 쿼리 라우팅
4. **Kafka 기반 파이프라인**: Consumer가 Kafka 레코드를 소비하여 DataObj를 빌드, 정렬, 업로드
5. **읽기 최적화**: 프리페치, Predicate Pushdown, Range Coalescing으로 Object Storage I/O 최소화
6. **스키마 진화**: 섹션 버전 관리, 인식되지 않는 컬럼 안전 무시, 레거시 포맷 자동 감지

이 설계는 기존 청크 기반 스토리지 대비 Object Storage API 호출 감소, 선택적 컬럼 읽기, 페이지 단위 필터링 등의 이점을 제공하며, Loki의 대규모 로그 분석 워크로드에 최적화되어 있다.
