# 10. 청크 인코딩 (Chunk Encoding) Deep-Dive

## 목차

1. [청크 인코딩 개요](#1-청크-인코딩-개요)
2. [Chunk 인터페이스](#2-chunk-인터페이스)
3. [XOR 인코딩 (Gorilla 논문 기반)](#3-xor-인코딩-gorilla-논문-기반)
4. [bstream (비트 스트림)](#4-bstream-비트-스트림)
5. [XOR Appender 구현](#5-xor-appender-구현)
6. [XOR Iterator 구현](#6-xor-iterator-구현)
7. [히스토그램 청크](#7-히스토그램-청크)
8. [청크 디스크 매퍼](#8-청크-디스크-매퍼)
9. [샘플 밀도 분석](#9-샘플-밀도-분석)
10. [성능 비교](#10-성능-비교)

---

## 1. 청크 인코딩 개요

### 왜 청크 기반인가

Prometheus는 시계열(time series) 데이터를 저장하는 모니터링 시스템이다. 시계열 데이터에는
뚜렷한 특성이 있다:

- **타임스탬프가 등간격**: 스크래프 간격(예: 15초)으로 수집되므로 연속된 타임스탬프 간 차이가 거의 일정하다
- **값이 점진적으로 변화**: CPU 사용률, 메모리 크기 등은 인접 샘플 간 차이가 작다
- **카운터는 단조 증가**: 요청 수, 바이트 수 등 카운터 메트릭은 값이 점진적으로 증가한다

이러한 특성을 활용하면 원시 데이터 대비 10배 이상의 압축이 가능하다.

```
시계열 데이터의 특성 (15초 간격 스크래프 예시)
─────────────────────────────────────────────

타임스탬프:  1000  1015  1030  1045  1060  1075
             │     │     │     │     │     │
delta:           15    15    15    15    15
                  │     │     │     │     │
delta-of-delta:     0     0     0     0      ← 대부분 0!

값(CPU%):   45.2  45.3  45.1  45.3  45.2  45.4
             │     │     │     │     │     │
XOR:            작음   작음   작음   작음   작음   ← 유사한 값의 XOR은 대부분 0
```

### 청크 단위 저장

Prometheus TSDB는 시계열 데이터를 **청크(Chunk)** 단위로 묶어 저장한다. 하나의 청크에는
기본 120개의 샘플이 들어간다(`DefaultSamplesPerChunk = 120`, `tsdb/head.go:210`).

```
시계열 (Series)
┌─────────────────────────────────────────────────────────┐
│  Chunk 0         Chunk 1         Chunk 2        ...     │
│ ┌──────────┐   ┌──────────┐   ┌──────────┐             │
│ │120 samples│   │120 samples│   │120 samples│            │
│ │ ~1KB max  │   │ ~1KB max  │   │ ~1KB max  │            │
│ └──────────┘   └──────────┘   └──────────┘             │
│  t0 ~ t119      t120 ~ t239    t240 ~ t359              │
└─────────────────────────────────────────────────────────┘
```

청크 기반 구조의 장점:
- **압축 효율**: 인접 샘플 간 유사성을 delta/XOR로 활용
- **메모리 효율**: 오래된 청크를 mmap으로 디스크 매핑
- **I/O 효율**: 청크 단위로 읽기/쓰기하여 디스크 접근 최소화
- **GC 부하 감소**: 개별 샘플 대신 바이트 배열([]byte)로 관리

### 인코딩 타입

소스코드 `tsdb/chunkenc/chunk.go:28-33`에 정의된 인코딩 타입:

```go
type Encoding uint8

const (
    EncNone           Encoding = iota  // 0: 없음
    EncXOR                             // 1: XOR 인코딩 (float64 값)
    EncHistogram                       // 2: 히스토그램 (정수 카운트)
    EncFloatHistogram                  // 3: 부동소수점 히스토그램
)
```

| 인코딩 | 대상 데이터 | 값 타입 | 사용 시나리오 |
|--------|------------|---------|-------------|
| `EncXOR` | Gauge, Counter 등 일반 메트릭 | `float64` | 가장 일반적 |
| `EncHistogram` | 네이티브 히스토그램 | 정수 카운트 | 정밀한 분포 데이터 |
| `EncFloatHistogram` | 부동소수점 히스토그램 | 부동소수점 카운트 | 집계된 히스토그램 |

---

## 2. Chunk 인터페이스

### 핵심 인터페이스 정의

`tsdb/chunkenc/chunk.go:69-93`에 정의된 `Chunk` 인터페이스:

```go
type Chunk interface {
    Iterable

    // Bytes: 청크의 원시 바이트 슬라이스 반환
    Bytes() []byte

    // Encoding: 인코딩 타입 반환 (EncXOR, EncHistogram, EncFloatHistogram)
    Encoding() Encoding

    // Appender: 샘플 추가용 Appender 반환
    Appender() (Appender, error)

    // NumSamples: 청크 내 샘플 수 반환
    NumSamples() int

    // Compact: 청크 완료 시 메모리 최적화
    Compact()

    // Reset: 새로운 바이트 스트림으로 리셋
    Reset(stream []byte)
}
```

### Appender 인터페이스

`tsdb/chunkenc/chunk.go:103-119`에서 정의:

```go
type Appender interface {
    // float64 샘플 추가 (st: 시작 타임스탬프, t: 타임스탬프, v: 값)
    Append(st, t int64, v float64)

    // 히스토그램 샘플 추가 (청크 재인코딩이 필요할 수 있음)
    AppendHistogram(prev *HistogramAppender, st, t int64,
        h *histogram.Histogram, appendOnly bool) (Chunk, bool, Appender, error)

    // 부동소수점 히스토그램 샘플 추가
    AppendFloatHistogram(prev *FloatHistogramAppender, st, t int64,
        h *histogram.FloatHistogram, appendOnly bool) (Chunk, bool, Appender, error)
}
```

### Iterator 인터페이스

`tsdb/chunkenc/chunk.go:122-161`에서 정의:

```go
type Iterator interface {
    Next() ValueType           // 다음 샘플로 이동, 타입 반환
    Seek(t int64) ValueType    // 특정 타임스탬프 이후 첫 샘플로 이동
    At() (int64, float64)      // 현재 (타임스탬프, 값) 반환
    AtHistogram(*histogram.Histogram) (int64, *histogram.Histogram)
    AtFloatHistogram(*histogram.FloatHistogram) (int64, *histogram.FloatHistogram)
    AtT() int64                // 현재 타임스탬프만 반환
    AtST() int64               // 시작 타임스탬프 반환
    Err() error                // 에러 반환
}
```

### ValueType과 인코딩 매핑

```go
type ValueType uint8

const (
    ValNone           ValueType = iota  // 값 없음 (이터레이터 소진)
    ValFloat                            // float64 → EncXOR
    ValHistogram                        // Histogram → EncHistogram
    ValFloatHistogram                   // FloatHistogram → EncFloatHistogram
)
```

### 청크 풀 (Pool)

`tsdb/chunkenc/chunk.go:292-368`에서 `sync.Pool`을 사용한 메모리 재사용:

```
청크 풀 구조
─────────────────────────────────────
Pool
├── xor: sync.Pool          → *XORChunk 재사용
├── histogram: sync.Pool    → *HistogramChunk 재사용
└── floatHistogram: sync.Pool → *FloatHistogramChunk 재사용

Get(encoding, bytes) → 풀에서 청크 꺼내서 Reset
Put(chunk)           → 청크를 Reset 후 풀에 반환
```

풀을 사용하는 이유:
- 청크 객체가 빈번하게 생성/소멸됨
- GC 압박을 줄이기 위해 객체 재사용
- `Reset()`으로 내부 상태를 초기화한 후 재활용

---

## 3. XOR 인코딩 (Gorilla 논문 기반)

### 배경: Facebook Gorilla 논문

Prometheus의 XOR 인코딩은 Facebook의 2015년 논문 "Gorilla: A Fast, Scalable, In-Memory
Time Series Database"에 기반한다. 원래 Damian Gryski가 Go로 구현한
`github.com/dgryski/go-tsz`를 Prometheus가 수정하여 사용한다
(`tsdb/chunkenc/xor.go` 파일 상단 라이선스 참조).

핵심 아이디어:
1. **타임스탬프**: Delta-of-Delta 인코딩 (등간격 특성 활용)
2. **값**: XOR 인코딩 (유사 값의 비트 차이가 적은 특성 활용)

### 3.1 타임스탬프 인코딩: Delta-of-Delta

#### 원리

시계열 타임스탬프는 거의 등간격이므로:
- delta(연속 타임스탬프 차이)가 거의 일정
- delta-of-delta(delta의 차이)는 대부분 0

```
타임스탬프 시퀀스 예시 (15초 간격, 밀리초 단위)
──────────────────────────────────────────────

시각:     t0=1000000  t1=1015000  t2=1030000  t3=1045000  t4=1045500

delta:          15000       15000       15000        500
                  │           │           │           │
dod:                  0           0         -14500

인코딩:
  t0: 원본 저장 (varint)
  t1: delta 저장 (uvarint) = 15000
  t2: dod=0    → "0"           (1비트!)
  t3: dod=0    → "0"           (1비트!)
  t4: dod=-14500 → "10" + 14비트  (16비트)
```

#### 인코딩 규칙 (xor.go:192-207)

| Delta-of-Delta 값 | 접두 비트 | 데이터 비트 | 총 비트 |
|-------------------|----------|-----------|--------|
| 0 | `0` | 없음 | 1 |
| -8191 ~ 8192 (14비트) | `10` | 14비트 | 16 |
| -65535 ~ 65536 (17비트) | `110` | 17비트 | 20 |
| -524287 ~ 524288 (20비트) | `1110` | 20비트 | 24 |
| 그 외 | `1111` | 64비트 | 68 |

```
Delta-of-Delta 인코딩 비트 레이아웃
────────────────────────────────────

dod = 0:
┌───┐
│ 0 │
└───┘
 1비트

dod = 5 (14비트 범위):
┌────┬──────────────────────────┐
│ 10 │  14비트 부호 있는 정수     │
└────┴──────────────────────────┘
 2비트        14비트 = 총 16비트

dod = -500 (17비트 범위):
┌─────┬────────────────────────────┐
│ 110 │  17비트 부호 있는 정수       │
└─────┴────────────────────────────┘
 3비트         17비트 = 총 20비트

dod = 100000 (20비트 범위):
┌──────┬─────────────────────────────┐
│ 1110 │  20비트 부호 있는 정수        │
└──────┴─────────────────────────────┘
 4비트          20비트 = 총 24비트

dod = 매우 큰 값:
┌──────┬─────────────────────────────────────────┐
│ 1111 │         64비트 원본 값                     │
└──────┴─────────────────────────────────────────┘
 4비트                64비트 = 총 68비트
```

#### Prometheus의 Gorilla 대비 차이점

Gorilla 논문은 초(second) 단위 해상도를 기준으로 설계했지만, Prometheus는
밀리초(millisecond) 단위를 사용한다. 따라서 비트 버킷 크기가 다르다:

| 버킷 | Gorilla (초) | Prometheus (밀리초) |
|------|-------------|-------------------|
| 1 | 7비트 | 14비트 |
| 2 | 9비트 | 17비트 |
| 3 | 12비트 | 20비트 |
| 4 | 32비트 | 64비트 |

소스코드의 TODO 주석(`xor.go:187-191`)에서도 이 점을 언급한다:
> "Gorilla has a max resolution of seconds, Prometheus milliseconds.
> Thus we use higher value range steps with larger bit size."

### 3.2 값 인코딩: XOR

#### 원리

IEEE 754 float64 값에서 유사한 두 값의 XOR 결과는 대부분의 비트가 0이다.
leading zeros와 trailing zeros가 많을수록 저장해야 할 유효 비트(significant bits)가
줄어든다.

```
XOR 인코딩 원리 (float64 = 64비트)
──────────────────────────────────

값1: 45.2 = 0 10000000100 0110100110011001100110011001100110011001100110011010
값2: 45.3 = 0 10000000100 0110100110011001100110011001100110011001100110100110
             ─────────────────────────────────────────────────────────────────
XOR:         0 00000000000 0000000000000000000000000000000000000000000000111100
                                                                      ^^^^^^
                                                       leading zeros: 58개
                                                       trailing zeros: 0개 (이 예시)
                                                       유효 비트: 6개
```

#### 인코딩 규칙 (xor.go:410-448 xorWrite 함수)

| XOR 결과 | 비트 패턴 | 설명 |
|----------|---------|------|
| 0 (동일 값) | `0` | 1비트만 사용 |
| 이전과 같은 leading/trailing | `10` + 유효비트 | 재사용 |
| 새로운 leading/trailing | `11` + 5비트(leading) + 6비트(sigbits) + 유효비트 | 전체 정보 저장 |

```
값 XOR 인코딩 비트 레이아웃
──────────────────────────

Case 1: XOR = 0 (값이 동일)
┌───┐
│ 0 │
└───┘
 1비트

Case 2: 이전 leading/trailing zero 재사용 가능
┌────┬──────────────────────┐
│ 10 │  유효 비트 (sigbits)   │
└────┴──────────────────────┘
 2비트    (64 - leading - trailing) 비트

Case 3: 새로운 leading/trailing zero
┌────┬───────────┬────────────┬──────────────────────┐
│ 11 │ leading(5)│ sigbits(6) │  유효 비트 (sigbits)   │
└────┴───────────┴────────────┴──────────────────────┘
 2비트    5비트       6비트      sigbits 비트
```

#### xorWrite 함수 분석

소스코드 `tsdb/chunkenc/xor.go:410-448`:

```go
func xorWrite(b *bstream, newValue, currentValue float64, leading, trailing *uint8) {
    delta := math.Float64bits(newValue) ^ math.Float64bits(currentValue)

    if delta == 0 {
        b.writeBit(zero)  // Case 1: 값 동일 → 1비트
        return
    }
    b.writeBit(one)

    newLeading := uint8(bits.LeadingZeros64(delta))
    newTrailing := uint8(bits.TrailingZeros64(delta))

    if newLeading >= 32 {
        newLeading = 31  // 5비트로 표현 가능한 최대값으로 클램프
    }

    if *leading != 0xff && newLeading >= *leading && newTrailing >= *trailing {
        // Case 2: 이전 leading/trailing 재사용
        b.writeBit(zero)
        b.writeBits(delta>>*trailing, 64-int(*leading)-int(*trailing))
        return
    }

    // Case 3: 새로운 leading/trailing
    *leading, *trailing = newLeading, newTrailing
    b.writeBit(one)
    b.writeBits(uint64(newLeading), 5)         // leading zero 수 (5비트)
    sigbits := 64 - newLeading - newTrailing
    b.writeBits(uint64(sigbits), 6)            // 유효 비트 수 (6비트)
    b.writeBits(delta>>newTrailing, int(sigbits)) // 실제 유효 비트
}
```

주의할 점:
- `leading >= 32`일 때 31로 클램프하는 이유: leading을 5비트(0-31)로 저장하기 때문
- `sigbits == 64`일 때 0으로 저장하고 읽을 때 복원하는 이유: 6비트로 64를 표현할 수 없음
- `*leading = 0xff`은 첫 번째 XOR 비교 시 이전 값이 없음을 나타내는 센티넬 값

### 3.3 왜 이 방식인가

```
압축 효과 분석 (120개 샘플 기준)
─────────────────────────────

원시 저장:
  타임스탬프: 120 × 8바이트 = 960바이트
  값:        120 × 8바이트 = 960바이트
  합계:      1920바이트 (16 bytes/sample)

XOR 인코딩:
  첫 번째 샘플:   ~10바이트 (varint timestamp + 8바이트 값)
  두 번째 샘플:   ~3바이트 (uvarint delta + XOR 값)
  나머지 118개:
    타임스탬프 dod=0 → 1비트 × 118 = ~15바이트
    값 XOR(case2) → ~6비트 × 118 = ~89바이트
  합계:          ~117바이트 (~1.0 bytes/sample)

압축률: 1920 / 117 ≈ 16.4배

Facebook Gorilla 논문 결과: 평균 1.37 bytes/sample
```

---

## 4. bstream (비트 스트림)

### 개요

XOR 인코딩은 비트 단위로 데이터를 읽고 써야 한다. `bstream`은 바이트 배열(`[]byte`) 위에
비트 단위 읽기/쓰기를 구현한 기반 구조체이다.

소스코드: `tsdb/chunkenc/bstream.go`

### bstream 구조체 (쓰기)

```go
// bstream.go:50-53
type bstream struct {
    stream []byte  // 데이터 바이트 배열
    count  uint8   // 현재 마지막 바이트에서 쓰기 가능한 남은 비트 수
}
```

```
bstream 내부 구조
─────────────────

stream: [0xFF] [0xA0] [0b11010000]
                              ↑
                        count = 4 (하위 4비트가 아직 쓰기 가능)

writeBit(1):
stream: [0xFF] [0xA0] [0b11011000]
                              ↑
                        count = 3
```

### writeBit (bstream.go:72-85)

```go
func (b *bstream) writeBit(bit bit) {
    if b.count == 0 {
        b.stream = append(b.stream, 0)  // 새 바이트 추가
        b.count = 8                     // 8비트 사용 가능
    }
    i := len(b.stream) - 1
    if bit {
        b.stream[i] |= 1 << (b.count - 1)  // MSB부터 채움
    }
    b.count--
}
```

```
writeBit 동작 예시
─────────────────

초기: stream=[...0b00000000], count=8

writeBit(1): stream=[...0b10000000], count=7
writeBit(0): stream=[...0b10000000], count=6
writeBit(1): stream=[...0b10100000], count=5
writeBit(1): stream=[...0b10110000], count=4

비트 순서: MSB → LSB (왼쪽에서 오른쪽으로 채움)
```

### writeByte (bstream.go:87-100)

바이트 경계에 정렬되지 않은 상태에서도 1바이트를 쓸 수 있다:

```go
func (b *bstream) writeByte(byt byte) {
    if b.count == 0 {
        b.stream = append(b.stream, byt)  // 정렬된 경우 직접 추가
        return
    }
    i := len(b.stream) - 1
    b.stream[i] |= byt >> (8 - b.count)     // 현재 바이트의 남은 비트 채움
    b.stream = append(b.stream, byt<<b.count) // 나머지를 새 바이트에
}
```

```
writeByte 비정렬 쓰기 예시
──────────────────────────

초기: stream=[...0b10100000], count=5
쓰기: writeByte(0b11001100)

1단계: 현재 바이트에 상위 5비트 채움
  stream=[...0b10111001]       ← 0b11001100 >> 3 = 0b00011001

2단계: 새 바이트에 하위 3비트 저장
  stream=[...0b10111001, 0b10000000]  ← 0b11001100 << 5 = 0b10000000
  count = 5 (새 바이트에 5비트 남음)
```

### writeBits (bstream.go:104-118)

임의 개수의 비트를 쓰는 범용 함수:

```go
func (b *bstream) writeBits(u uint64, nbits int) {
    u <<= 64 - uint(nbits)        // 왼쪽 정렬
    for nbits >= 8 {
        byt := byte(u >> 56)       // 상위 8비트 추출
        b.writeByte(byt)
        u <<= 8
        nbits -= 8
    }
    for nbits > 0 {
        b.writeBit((u >> 63) == 1) // 남은 비트 하나씩
        u <<= 1
        nbits--
    }
}
```

### bstreamReader 구조체 (읽기)

```go
// bstream.go:120-127
type bstreamReader struct {
    stream       []byte   // 원본 데이터
    streamOffset int      // 다음에 읽을 바이트 오프셋
    buffer       uint64   // 64비트 읽기 버퍼
    valid        uint8    // 버퍼에서 유효한 비트 수
    last         byte     // 마지막 바이트 복사본 (동시성 안전)
}
```

읽기 버퍼링 전략:
1. 8바이트 이상 남은 경우: `binary.BigEndian.Uint64()`로 64비트를 한 번에 로드
2. 8바이트 이하 남은 경우: 필요한 만큼만 로드
3. 마지막 바이트는 초기화 시 복사본을 만듦 (동시 쓰기로 인한 데이터 경쟁 방지)

```
bstreamReader 버퍼 동작
───────────────────────

stream: [B0][B1][B2][B3][B4][B5][B6][B7][B8][B9][B10]
                                                  ↑
                                            last = B10 (복사본)

loadNextBuffer() 호출 시 (streamOffset=0):
  buffer = B0B1B2B3B4B5B6B7 (64비트)
  valid = 64
  streamOffset = 8

readBitsFast(5):
  결과 = buffer >> 59 (상위 5비트)
  valid = 59
```

### readBitFast vs readBit

성능 최적화를 위해 두 단계로 분리:

```go
// 빠른 경로: 버퍼에 데이터가 있으면 즉시 반환
func (b *bstreamReader) readBitFast() (bit, error) {
    if b.valid == 0 {
        return false, io.EOF  // 버퍼 비어있음 → 느린 경로로
    }
    b.valid--
    bitmask := uint64(1) << b.valid
    return (b.buffer & bitmask) != 0, nil
}

// 느린 경로: 새 데이터 로드 후 읽기
func (b *bstreamReader) readBit() (bit, error) {
    if b.valid == 0 {
        if !b.loadNextBuffer(1) {
            return false, io.EOF
        }
    }
    return b.readBitFast()
}
```

이렇게 분리하는 이유: `readBitFast()`를 작은 리프 함수로 유지하여 컴파일러가
**인라이닝(inlining)** 할 수 있게 한다. 핫 경로(버퍼에 데이터 있음)에서 함수 호출
오버헤드를 제거한다.

---

## 5. XOR Appender 구현

### xorAppender 구조체

`tsdb/chunkenc/xor.go:150-159`:

```go
type xorAppender struct {
    b *bstream          // 비트 스트림 (쓰기 대상)

    t      int64        // 마지막 타임스탬프
    v      float64      // 마지막 값
    tDelta uint64       // 마지막 타임스탬프 delta

    leading  uint8      // XOR 결과의 leading zero 수
    trailing uint8      // XOR 결과의 trailing zero 수
}
```

### Append 메서드 (xor.go:161-216)

샘플 추가는 세 가지 경우로 분기한다:

```go
func (a *xorAppender) Append(_, t int64, v float64) {
    var tDelta uint64
    num := binary.BigEndian.Uint16(a.b.bytes())  // 헤더에서 샘플 수 읽기

    switch num {
    case 0:   // 첫 번째 샘플
    case 1:   // 두 번째 샘플
    default:  // 세 번째 이후 샘플
    }

    a.t = t
    a.v = v
    binary.BigEndian.PutUint16(a.b.bytes(), num+1)  // 샘플 수 증가
    a.tDelta = tDelta
}
```

#### Case 0: 첫 번째 샘플

```go
case 0:
    buf := make([]byte, binary.MaxVarintLen64)
    for _, b := range buf[:binary.PutVarint(buf, t)] {
        a.b.writeByte(b)       // 타임스탬프를 varint로 저장
    }
    a.b.writeBits(math.Float64bits(v), 64)  // 값을 64비트 원본으로 저장
```

```
첫 번째 샘플 인코딩
───────────────────

┌──────────────┬────────────┬──────────────────────────┐
│ Header (2B)  │ t (varint) │     v (64비트 raw)         │
│ num_samples  │  가변 길이   │      8바이트               │
└──────────────┴────────────┴──────────────────────────┘
```

#### Case 1: 두 번째 샘플

```go
case 1:
    tDelta = uint64(t - a.t)       // delta 계산
    buf := make([]byte, binary.MaxVarintLen64)
    for _, b := range buf[:binary.PutUvarint(buf, tDelta)] {
        a.b.writeByte(b)           // delta를 uvarint로 저장
    }
    a.writeVDelta(v)               // 값은 XOR로 저장
```

```
두 번째 샘플 인코딩
───────────────────

┌────────────────┬───────────────┐
│ tDelta(uvarint)│ v XOR 인코딩    │
│   가변 길이      │   가변 길이     │
└────────────────┴───────────────┘
```

#### Case default: 세 번째 이후 샘플

```go
default:
    tDelta = uint64(t - a.t)
    dod := int64(tDelta - a.tDelta)  // delta-of-delta 계산

    switch {
    case dod == 0:
        a.b.writeBit(zero)           // "0" (1비트)
    case bitRange(dod, 14):
        // "10" + 14비트 (총 16비트, 2바이트 최적화)
        a.b.writeByte(0b10<<6 | (uint8(dod>>8) & (1<<6 - 1)))
        a.b.writeByte(uint8(dod))
    case bitRange(dod, 17):
        a.b.writeBits(0b110, 3)      // "110" + 17비트
        a.b.writeBits(uint64(dod), 17)
    case bitRange(dod, 20):
        a.b.writeBits(0b1110, 4)     // "1110" + 20비트
        a.b.writeBits(uint64(dod), 20)
    default:
        a.b.writeBits(0b1111, 4)     // "1111" + 64비트
        a.b.writeBits(uint64(dod), 64)
    }
    a.writeVDelta(v)                 // 값은 XOR로 저장
```

### bitRange 함수 (xor.go:220-222)

부호 있는 정수가 nbits로 표현 가능한지 확인:

```go
func bitRange(x int64, nbits uint8) bool {
    return -((1<<(nbits-1))-1) <= x && x <= 1<<(nbits-1)
}
```

14비트 예시: -8191 <= x <= 8192

### Appender 생성 시 상태 복원

기존 청크에 Appender를 생성할 때는 전체 데이터를 순회하여 상태를 복원한다
(`xor.go:102-126`):

```go
func (c *XORChunk) Appender() (Appender, error) {
    if len(c.b.stream) == chunkHeaderSize {
        // 빈 청크: 초기 상태로 시작
        return &xorAppender{b: &c.b, t: math.MinInt64, leading: 0xff}, nil
    }
    // 기존 데이터가 있으면 이터레이터로 끝까지 순회
    it := c.iterator(nil)
    for it.Next() != ValNone {
    }
    // 이터레이터의 최종 상태로 appender 초기화
    a := &xorAppender{
        b: &c.b, t: it.t, v: it.val,
        tDelta: it.tDelta, leading: it.leading, trailing: it.trailing,
    }
    return a, nil
}
```

`leading: 0xff`는 센티넬 값으로, 첫 XOR 비교 시 "이전 leading/trailing이 없음"을
나타낸다. `xorWrite` 함수에서 `*leading != 0xff` 조건으로 확인한다.

---

## 6. XOR Iterator 구현

### xorIterator 구조체

`tsdb/chunkenc/xor.go:236-249`:

```go
type xorIterator struct {
    br       bstreamReader  // 비트 스트림 리더
    numTotal uint16         // 청크 내 전체 샘플 수
    numRead  uint16         // 지금까지 읽은 샘플 수

    t   int64              // 현재 타임스탬프
    val float64            // 현재 값

    leading  uint8         // XOR leading zeros
    trailing uint8         // XOR trailing zeros

    tDelta uint64          // 현재 타임스탬프 delta
    err    error           // 에러
}
```

### Next() 메서드 (xor.go:303-398)

#### 첫 번째 샘플 읽기

```go
if it.numRead == 0 {
    t, err := binary.ReadVarint(&it.br)  // varint로 타임스탬프 읽기
    v, err := it.br.readBits(64)         // 64비트 원본 값 읽기
    it.t = t
    it.val = math.Float64frombits(v)
    it.numRead++
    return ValFloat
}
```

#### 두 번째 샘플 읽기

```go
if it.numRead == 1 {
    tDelta, err := binary.ReadUvarint(&it.br)  // uvarint로 delta 읽기
    it.tDelta = tDelta
    it.t += int64(it.tDelta)                   // 타임스탬프 복원
    return it.readValue()                       // XOR로 값 읽기
}
```

#### 세 번째 이후 샘플 읽기

```go
// Delta-of-delta 접두 비트 읽기
var d byte
for range 4 {
    d <<= 1
    bit, err := it.br.readBitFast()
    if err != nil {
        bit, err = it.br.readBit()
    }
    if bit == zero {
        break      // 0을 만나면 접두 코드 완성
    }
    d |= 1
}

var sz uint8
var dod int64
switch d {
case 0b0:     // dod == 0
case 0b10:    sz = 14
case 0b110:   sz = 17
case 0b1110:  sz = 20
case 0b1111:  // 64비트 직접 읽기
    bits, _ := it.br.readBits(64)
    dod = int64(bits)
}
```

접두 비트 디코딩 과정:

```
접두 비트 읽기 상태 머신
─────────────────────────

              readBit
  시작 ──────────────→ bit=0? → d=0b0 (dod=0)
    │                    │
    │                  bit=1
    │                    ↓
    │            readBit → bit=0? → d=0b10 (14비트)
    │                    │
    │                  bit=1
    │                    ↓
    │            readBit → bit=0? → d=0b110 (17비트)
    │                    │
    │                  bit=1
    │                    ↓
    │            readBit → bit=0? → d=0b1110 (20비트)
    │                    │
    │                  bit=1 → d=0b1111 (64비트)
```

#### 부호 있는 정수 복원 (음수 처리)

```go
if sz != 0 {
    bits, _ := it.br.readBitsFast(sz)
    // 음수 복원: 상위 비트가 1이면 부호 확장
    if bits > (1 << (sz - 1)) {
        bits -= 1 << sz
    }
    dod = int64(bits)
}

it.tDelta = uint64(int64(it.tDelta) + dod)  // delta 복원
it.t += int64(it.tDelta)                     // 타임스탬프 복원
```

### xorRead 함수 (xor.go:450-516)

값 디코딩:

```go
func xorRead(br *bstreamReader, value *float64, leading, trailing *uint8) error {
    bit, _ := br.readBitFast()

    if bit == zero {
        return nil  // Case 1: 값 동일, 아무것도 하지 않음
    }

    bit, _ = br.readBitFast()
    if bit == zero {
        // Case 2: 이전 leading/trailing 재사용
        mbits = 64 - *leading - *trailing
    } else {
        // Case 3: 새 leading/trailing 읽기
        newLeading = uint8(br.readBits(5))
        mbits = uint8(br.readBits(6))
        if mbits == 0 { mbits = 64 }  // 오버플로 복원
        newTrailing = 64 - newLeading - mbits
        *leading, *trailing = newLeading, newTrailing
    }

    bits, _ := br.readBits(mbits)
    vbits := math.Float64bits(*value)
    vbits ^= bits << newTrailing       // XOR 적용하여 값 복원
    *value = math.Float64frombits(vbits)
    return nil
}
```

```
값 디코딩 흐름도
─────────────────

readBit()
  │
  ├── 0 → 값 변경 없음 (이전 값 그대로)
  │
  └── 1 → readBit()
           │
           ├── 0 → 이전 leading/trailing 재사용
           │        └→ readBits(64-leading-trailing)
           │           └→ XOR 적용하여 값 복원
           │
           └── 1 → readBits(5)  → newLeading
                    readBits(6)  → sigbits (0이면 64)
                    newTrailing = 64 - newLeading - sigbits
                    └→ readBits(sigbits)
                       └→ XOR 적용하여 값 복원
```

### Seek 메서드

```go
func (it *xorIterator) Seek(t int64) ValueType {
    for t > it.t || it.numRead == 0 {
        if it.Next() == ValNone {
            return ValNone
        }
    }
    return ValFloat
}
```

Seek은 선형 탐색이다. XOR 인코딩은 순차적으로만 디코딩할 수 있으므로
(이전 값이 있어야 다음 값을 복원할 수 있음) 랜덤 액세스가 불가능하다.
이것이 청크 크기를 제한하는 이유 중 하나이다.

---

## 7. 히스토그램 청크

### HistogramChunk 구조체

`tsdb/chunkenc/histogram.go:38-40`:

```go
type HistogramChunk struct {
    b bstream
}
```

### 히스토그램 청크 헤더

`tsdb/chunkenc/histogram.go:71-87`:

```
히스토그램 청크 레이아웃
─────────────────────────

┌──────────────────┬──────────────────┬───────────────────┐
│ num_samples (2B) │ counter_reset(1B)│ 인코딩된 샘플 데이터  │
│                  │ [2비트 플래그]      │                    │
└──────────────────┴──────────────────┴───────────────────┘
   헤더: 3바이트 (histogramHeaderSize)
```

Counter Reset 헤더 (상위 2비트):

| 비트 패턴 | 상수 | 의미 |
|----------|------|------|
| `10` | `CounterReset` | 카운터 리셋 발생 |
| `01` | `NotCounterReset` | 카운터 리셋 없음 |
| `11` | `GaugeType` | 게이지 히스토그램 (리셋 개념 없음) |
| `00` | `UnknownCounterReset` | 알 수 없음 (쿼리 시 감지 필요) |

### 샘플별 인코딩 방식

`tsdb/chunkenc/histogram.go:27-37`의 주석에 명시:

```
필드별 인코딩 전략
──────────────────────────────────────────────────────────────

field →    ts     count  zeroCount  sum    []posBuckets  []negBuckets
sample 1   raw    raw    raw        raw    []raw         []raw
sample 2   delta  delta  delta      xor    []delta       []delta
sample >2  dod    dod    dod        xor    []dod         []dod

ts:          타임스탬프 → XOR 청크와 동일한 delta-of-delta
count:       총 관측 수 → delta-of-delta (varbit 인코딩)
zeroCount:   제로 버킷 카운트 → delta-of-delta
sum:         관측값 합계 → XOR (float64이므로)
posBuckets:  양수 버킷들 → delta-of-delta (각 버킷별)
negBuckets:  음수 버킷들 → delta-of-delta (각 버킷별)
```

### 히스토그램 레이아웃 메타데이터

`tsdb/chunkenc/histogram_meta.go:22-33`의 `writeHistogramChunkLayout`:

```
히스토그램 레이아웃 저장 순서
──────────────────────────

┌─────────────────┬──────────┬──────────────┬──────────────┬───────────────┐
│ zeroThreshold   │ schema   │ positiveSpans│ negativeSpans│ customValues  │
│ (float64 특수)   │ (varbit) │ (varbit)     │ (varbit)     │ (custom only) │
└─────────────────┴──────────┴──────────────┴──────────────┴───────────────┘
```

각 Span은 `Length`(uint32)와 `Offset`(int32)으로 구성:

```go
// histogram_meta.go:72-78
func putHistogramChunkLayoutSpans(b *bstream, spans []histogram.Span) {
    putVarbitUint(b, uint64(len(spans)))  // span 개수
    for _, s := range spans {
        putVarbitUint(b, uint64(s.Length))   // 각 span의 길이
        putVarbitInt(b, int64(s.Offset))     // 각 span의 오프셋
    }
}
```

### FloatHistogramChunk

`tsdb/chunkenc/float_histogram.go:38-40`:

```go
type FloatHistogramChunk struct {
    b bstream
}
```

FloatHistogramChunk은 HistogramChunk과 구조가 동일하지만, 버킷 카운트를 정수 대신
부동소수점으로 저장한다. 히스토그램 집계(예: `rate()` 함수 적용) 시 정수에서
부동소수점으로 변환되므로, 집계 결과를 저장할 때 사용된다.

```
히스토그램 인코딩 비교
──────────────────────

HistogramChunk:
  count, zeroCount, buckets → 정수 (varbit delta-of-delta)
  sum → float64 (XOR)
  용도: 원본 히스토그램 데이터

FloatHistogramChunk:
  count, zeroCount, buckets → float64 (XOR)
  sum → float64 (XOR)
  용도: 집계/다운샘플링된 히스토그램 데이터
```

### HistogramAppender 필드

`tsdb/chunkenc/histogram.go:182-199`:

```go
type HistogramAppender struct {
    b *bstream

    // 레이아웃
    schema         int32
    zThreshold     float64
    pSpans, nSpans []histogram.Span
    customValues   []float64

    // 상태
    t                            int64
    cnt, zCnt                    uint64
    tDelta, cntDelta, zCntDelta  int64
    pBuckets, nBuckets           []int64
    pBucketsDelta, nBucketsDelta []int64

    sum      float64
    leading  uint8
    trailing uint8
}
```

히스토그램 Appender가 XOR Appender보다 훨씬 복잡한 이유:
- 버킷 레이아웃이 변경되면 **청크 재인코딩(recode)** 이 필요할 수 있음
- 카운터 리셋 감지 로직이 포함됨
- 각 버킷별로 개별 delta/dod 상태를 유지해야 함

### 청크 크기 제한

```go
// chunk.go:55-66
const (
    MaxBytesPerXORChunk           = 1024   // XOR: 최대 1KB
    TargetBytesPerHistogramChunk  = 1024   // 히스토그램: 목표 1KB
    MinSamplesPerHistogramChunk   = 10     // 히스토그램: 최소 10개 샘플
)
```

히스토그램은 샘플 하나가 1KB를 초과할 수 있으므로(버킷이 매우 많은 경우),
`MinSamplesPerHistogramChunk`으로 최소 샘플 수를 보장하여 압축 효과를 확보한다.

---

## 8. 청크 디스크 매퍼

### ChunkDiskMapper 개요

`tsdb/chunks/head_chunks.go:193-218`에 정의된 `ChunkDiskMapper`는 Head 블록의
청크를 디스크에 영속화하고 mmap으로 읽는 컴포넌트이다.

```
ChunkDiskMapper 역할
────────────────────

메모리 (Head)                           디스크
┌──────────────┐                    ┌──────────────┐
│  활성 청크     │                    │  세그먼트 파일   │
│ (쓰기 가능)    │   ──mmap 쓰기──→   │ 000001        │
│              │                    │ 000002        │
│  완료된 청크   │   ←──mmap 읽기──   │ 000003        │
│ (읽기 전용)    │                    │ ...           │
└──────────────┘                    └──────────────┘
```

### ChunkDiskMapper 구조체

```go
type ChunkDiskMapper struct {
    // Writer 관련
    dir             *os.File
    writeBufferSize int
    curFile         *os.File       // 현재 쓰기 중인 파일
    curFileSequence int            // 현재 파일 인덱스
    curFileOffset   atomic.Uint64  // 현재 파일 내 오프셋
    curFileMaxt     int64          // 크기 기반 보존용

    evtlPosMtx sync.Mutex
    evtlPos    chunkPos            // 쓰기 큐 처리 후 최종 위치

    byteBuf   [MaxHeadChunkMetaSize]byte  // 헤더 쓰기 버퍼
    chkWriter *bufio.Writer               // 버퍼링된 쓰기
    crc32     hash.Hash

    // Reader 관련
    mmappedChunkFiles map[int]*mmappedChunkFile  // mmap된 파일들
    closers           map[int]io.Closer
    readPathMtx       sync.RWMutex              // 읽기 경로 보호
    pool              chunkenc.Pool              // 청크 풀
}
```

### 세그먼트 파일 포맷

#### Head 청크 파일 헤더

```
Head 청크 세그먼트 파일 포맷
──────────────────────────

┌────────────────────────────────────────┐
│ Magic Number (4B): 0x0130BC91          │
│ Format Version (1B): 1                 │
│ Padding (3B)                           │
├────────────────────────────────────────┤  ← SegmentHeaderSize (8B)
│ Chunk 0                                │
│ Chunk 1                                │
│ Chunk 2                                │
│ ...                                    │
└────────────────────────────────────────┘
  최대 크기: 128 MiB (MaxHeadChunkFileSize)
```

#### 개별 청크 포맷

```
개별 Head 청크 레이아웃
─────────────────────────

┌──────────┬──────────┬──────────┬──────────┬──────────┬──────────┬──────┐
│ SeriesRef│ MinTime  │ MaxTime  │ Encoding │ ChunkLen │ ChunkData│ CRC  │
│  (8B)    │  (8B)    │  (8B)    │  (1B)    │ (uvarint)│ (가변)    │ (4B) │
└──────────┴──────────┴──────────┴──────────┴──────────┴──────────┴──────┘
     │          │          │          │          │          │         │
     │          │          │          │          │          │         └─ CRC32 체크섬
     │          │          │          │          │          └─ 인코딩된 청크 바이트
     │          │          │          │          └─ 청크 데이터 길이
     │          │          │          └─ EncXOR(1), EncHistogram(2), ...
     │          │          └─ 청크 내 최대 타임스탬프
     │          └─ 청크 내 최소 타임스탬프
     └─ 시리즈 참조 (어떤 시계열의 청크인지)
```

### Block 청크 파일 포맷

`tsdb/chunks/chunks.go:32-42`에 정의:

```
Block 청크 세그먼트 파일 포맷
─────────────────────────────

┌────────────────────────────────────────┐
│ Magic Number (4B): 0x85BD40DD          │
│ Format Version (1B): 1                 │
│ Padding (3B)                           │
├────────────────────────────────────────┤  ← SegmentHeaderSize (8B)
│ Chunk 0                                │
│ Chunk 1                                │
│ ...                                    │
└────────────────────────────────────────┘
  최대 크기: 512 MiB (DefaultChunkSegmentSize)
```

```go
const DefaultChunkSegmentSize = 512 * 1024 * 1024  // 512 MiB
```

### ChunkDiskMapperRef

`tsdb/chunks/head_chunks.go:78-88`:

```
ChunkDiskMapperRef (8바이트)
──────────────────────────────

┌──────────────────────┬──────────────────────┐
│   상위 4바이트          │   하위 4바이트          │
│   세그먼트 파일 인덱스    │   파일 내 바이트 오프셋   │
└──────────────────────┴──────────────────────┘

예: ref = 0x0000000300000100
    → 세그먼트 파일 #3, 오프셋 256바이트
```

### 파일 분할 결정

`tsdb/chunks/head_chunks.go:163-170`:

```go
func (f *chunkPos) shouldCutNewFile(bytesToWrite uint64) bool {
    if f.cutFile {
        return true
    }
    return f.offset == 0 ||                         // 첫 번째 파일
        f.offset+bytesToWrite > MaxHeadChunkFileSize // 128MiB 초과
}
```

### mmap 기반 읽기

완료된 청크 파일은 mmap으로 메모리에 매핑된다:

```
mmap 동작 원리
──────────────

디스크:                     메모리 (가상 주소 공간):
┌──────────┐               ┌──────────┐
│ 세그먼트 0 │ ──mmap──→    │ byte[]   │ ← OS가 페이지 폴트로 지연 로드
│ 세그먼트 1 │ ──mmap──→    │ byte[]   │
│ 세그먼트 2 │ ──mmap──→    │ byte[]   │
└──────────┘               └──────────┘

장점:
- 명시적 read() 시스템 콜 불필요
- OS의 페이지 캐시 활용
- 필요한 부분만 메모리에 로드 (demand paging)
- 여러 프로세스가 같은 데이터 공유 가능
```

### CRC32 체크섬

데이터 무결성을 위해 Castagnoli CRC32를 사용한다:

```go
// chunks.go:257-265
var castagnoliTable *crc32.Table

func init() {
    castagnoliTable = crc32.MakeTable(crc32.Castagnoli)
}
```

Castagnoli 다항식을 선택한 이유: 하드웨어 가속(SSE 4.2의 CRC32C 명령어)을 활용할 수
있어 소프트웨어 CRC32보다 수배 빠르다.

---

## 9. 샘플 밀도 분석

### 기본 설정

```go
// tsdb/head.go:209-210
// DefaultSamplesPerChunk provides a default target number of samples per chunk.
DefaultSamplesPerChunk = 120
```

### 스크래프 간격별 청크 수명

| 스크래프 간격 | 샘플/청크 | 청크 수명 | 시간당 청크 수 |
|------------|---------|---------|------------|
| 10초 | 120 | 20분 | 3 |
| 15초 | 120 | 30분 | 2 |
| 30초 | 120 | 60분 | 1 |
| 60초 | 120 | 120분 | 0.5 |

### 청크 크기 분석

```
XOR 청크 크기 분포 (일반적인 시나리오)
────────────────────────────────────

MaxBytesPerXORChunk = 1024바이트 (1KB)

일반 Gauge 메트릭 (CPU%, 메모리 등):
  120 샘플 × ~1.4 bytes/sample ≈ 168바이트

거의 일정한 값 (업타임 등):
  120 샘플 × ~0.5 bytes/sample ≈ 60바이트

급격히 변하는 값 (랜덤 등):
  120 샘플 × ~6 bytes/sample ≈ 720바이트

헤더 오버헤드: 2바이트 (샘플 수 uint16)

실제 대부분의 청크: 100~500바이트
```

### 청크 교체 조건

청크가 교체되는 조건:
1. **샘플 수 도달**: `SamplesPerChunk`(기본 120)개 도달
2. **크기 초과**: `MaxBytesPerXORChunk`(1024바이트) 초과
3. **시간 범위**: `chunkRange` 경계 초과
4. **카운터 리셋**: 히스토그램의 카운터 리셋 감지 시

```
청크 교체 판단 흐름
──────────────────

새 샘플 도착
    │
    ├── 샘플 수 >= 120? ────→ 새 청크 생성
    │
    ├── 청크 크기 >= 1KB? ──→ 새 청크 생성
    │
    ├── 시간 범위 초과? ────→ 새 청크 생성
    │
    └── 현재 청크에 추가
```

---

## 10. 성능 비교

### 원시 저장 vs XOR 인코딩

```
120개 샘플 기준 비교
──────────────────────────────────────────────────────

                      원시 float64      XOR 인코딩
─────────────────────────────────────────────────────
타임스탬프              960 bytes        ~15 bytes
                      (120 × 8B)       (dod=0: 120비트)

값                    960 bytes        ~153 bytes
                      (120 × 8B)       (avg ~10비트/sample)

헤더                  0 bytes          2 bytes
첫 번째 샘플 오버헤드    포함              ~18 bytes

합계                  1920 bytes       ~188 bytes
bytes/sample          16.0             ~1.57
압축률                 1x              ~10.2x
─────────────────────────────────────────────────────
```

### 시나리오별 압축 효율

```
시나리오별 bytes/sample 비교
─────────────────────────────────────────────────

시나리오                    원시    XOR     압축률
──────────────────────────────────────────────
상수 값 (항상 같은 값)       16.0   ~0.3    ~53x
  ts: dod=0 (1bit)
  val: XOR=0 (1bit)

등간격 + 완만한 변화         16.0   ~1.0    ~16x
  ts: dod=0 (1bit)
  val: XOR small (8bits)

등간격 + 일반 변화           16.0   ~1.4    ~11x
  ts: dod=0 (1bit)
  val: XOR medium (12bits)

비등간격 + 큰 변화           16.0   ~4.0    ~4x
  ts: dod!=0 (16-24bits)
  val: XOR large (20+bits)

완전 랜덤                  16.0   ~12.0   ~1.3x
  ts: dod large (68bits)
  val: XOR full (67bits)
──────────────────────────────────────────────
```

### 구체적 인코딩 예시

15초 간격, CPU 사용률 메트릭을 예로 들어 실제 비트 레벨 인코딩을 보여준다:

```
입력 데이터:
  t0=1700000000000, v0=45.20  (밀리초 타임스탬프)
  t1=1700000015000, v1=45.30
  t2=1700000030000, v2=45.25
  t3=1700000045000, v3=45.30

=== 샘플 0 (첫 번째) ===
  타임스탬프: varint(1700000000000) → 약 6바이트
  값: float64bits(45.20) → 64비트 원본
  총: ~14바이트

=== 샘플 1 (두 번째) ===
  tDelta = 15000
  타임스탬프: uvarint(15000) → 약 2바이트
  값 XOR:
    45.30 XOR 45.20 = 소수의 유효비트
    Case 3: 11 + 5비트(leading) + 6비트(sigbits) + N비트
  총: ~5바이트

=== 샘플 2 (세 번째) ===
  tDelta = 15000, dod = 15000 - 15000 = 0
  타임스탬프: "0" → 1비트!
  값 XOR:
    45.25 XOR 45.30 = 유사한 leading/trailing
    Case 2: 10 + 유효비트
  총: ~1.5바이트

=== 샘플 3 (네 번째) ===
  tDelta = 15000, dod = 0
  타임스탬프: "0" → 1비트!
  값 XOR:
    45.30 XOR 45.25 = 이전과 유사
    Case 2: 10 + 유효비트
  총: ~1.5바이트

4개 샘플 합계: ~22바이트 (5.5 bytes/sample)
원시 저장 시: 64바이트 (16 bytes/sample)
압축률: ~2.9x (샘플이 많을수록 향상)
```

### 메모리 사용량 추산

```
시계열 10만 개, 15초 스크래프, 2시간 Head 보존 기준
──────────────────────────────────────────────────

시계열당 청크 수: 2시간 / 30분 = 4개 활성 청크
전체 청크 수: 100,000 × 4 = 400,000개
청크당 평균 크기: ~200바이트

청크 데이터: 400,000 × 200B = ~76 MB
메타데이터 오버헤드: ~50%
총 Head 메모리: ~114 MB

원시 저장 시:
  100,000 시계열 × 480 샘플 × 16B = ~732 MB
  XOR 대비 ~6.4배 더 사용
```

### Gorilla 논문과의 비교

| 항목 | Gorilla (Facebook) | Prometheus |
|------|-------------------|------------|
| 해상도 | 초 단위 | 밀리초 단위 |
| 타임스탬프 버킷 | 7/9/12/32비트 | 14/17/20/64비트 |
| 평균 bytes/sample | 1.37 | ~1.4 (유사) |
| 값 인코딩 | XOR | XOR (동일) |
| 청크 크기 | 2시간 고정 | 샘플 수 기반 (120개) |
| 저장 매체 | 인메모리 | 인메모리 + mmap |
| 히스토그램 | 미지원 | 네이티브 히스토그램 지원 |

---

## 소스코드 참조 요약

| 파일 | 핵심 내용 |
|------|---------|
| `tsdb/chunkenc/chunk.go` | Chunk, Appender, Iterator 인터페이스, Encoding 타입, Pool |
| `tsdb/chunkenc/xor.go` | XORChunk, xorAppender, xorIterator, xorWrite, xorRead |
| `tsdb/chunkenc/bstream.go` | bstream(쓰기), bstreamReader(읽기), 비트 단위 I/O |
| `tsdb/chunkenc/histogram.go` | HistogramChunk, HistogramAppender |
| `tsdb/chunkenc/histogram_meta.go` | 히스토그램 레이아웃 직렬화/역직렬화 |
| `tsdb/chunkenc/float_histogram.go` | FloatHistogramChunk |
| `tsdb/chunks/chunks.go` | 세그먼트 파일 포맷, Writer, BlockChunkRef |
| `tsdb/chunks/head_chunks.go` | ChunkDiskMapper, ChunkDiskMapperRef, 세그먼트 관리 |
| `tsdb/head.go` | DefaultSamplesPerChunk = 120 |
