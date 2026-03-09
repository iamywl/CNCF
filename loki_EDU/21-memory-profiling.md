# 21. 메모리 관리 & 프로파일링/트레이싱 — 성능 인프라 Deep Dive

## 목차

1. [개요](#1-개요)
2. [Arena 메모리 할당기](#2-arena-메모리-할당기)
3. [Region 메모리 모델](#3-region-메모리-모델)
4. [Bitmap 자료구조](#4-bitmap-자료구조)
5. [Buffer 제네릭 구현](#5-buffer-제네릭-구현)
6. [메모리 할당 전략](#6-메모리-할당-전략)
7. [Parent-Child 할당기 계층](#7-parent-child-할당기-계층)
8. [트레이싱 설정 시스템](#8-트레이싱-설정-시스템)
9. [OpenTelemetry 통합](#9-opentelemetry-통합)
10. [메모리 정렬 최적화](#10-메모리-정렬-최적화)
11. [동시성 안전성](#11-동시성-안전성)
12. [성능 분석 및 운영 가이드](#12-성능-분석-및-운영-가이드)

---

## 1. 개요

Loki의 성능 인프라는 두 축으로 구성된다:

| 영역 | 패키지 | 목적 |
|------|--------|------|
| **메모리 관리** | `pkg/memory/` | Arena 스타일 메모리 할당기, GC 부담 경감 |
| **프로파일링/트레이싱** | `pkg/tracing/` | 분산 트레이싱 설정, OpenTelemetry 통합 |

### 왜 커스텀 메모리 관리가 필요한가

Go의 가비지 컬렉터(GC)는 범용적이지만, Loki처럼 **대량의 임시 버퍼를 반복적으로 할당/해제**하는 워크로드에서는 GC 오버헤드가 심각해질 수 있다.

```
[일반 Go 패턴]
요청마다 → make([]byte, N) → GC가 나중에 회수
  ↓ 문제:
  - GC 일시 정지(STW) 증가
  - 힙 파편화
  - 할당/해제 빈도에 비례하는 CPU 소비

[Arena 패턴 (pkg/memory)]
초기화 → 큰 Region 미리 할당
요청마다 → Region에서 슬라이스 → 완료 후 Reclaim
  ↓ 장점:
  - GC가 추적할 객체 수 감소
  - 메모리 재사용으로 할당 비용 제거
  - 64바이트 정렬로 캐시 효율성
```

### 패키지 구조

```
pkg/memory/
├── memory.go     ← Region 타입 정의 (패키지 문서)
├── allocator.go  ← Allocator (arena 스타일 할당기)
├── bitmap.go     ← Bitmap (비트 패킹 불리언 배열)
├── buffer.go     ← Buffer[T] (제네릭 타입 버퍼)
└── internal/
    ├── memalign/  ← 64바이트 정렬 유틸리티
    └── unsafecast/ ← unsafe 타입 변환

pkg/tracing/
├── config.go     ← 트레이싱 설정 (Config 구조체)
└── otel_kv.go    ← OpenTelemetry 키-값 변환 유틸리티
```

---

## 2. Arena 메모리 할당기

### Allocator 구조체

`pkg/memory/allocator.go` 라인 28-38:

```go
type Allocator struct {
    locked atomic.Bool      // 동시 사용 감지 (CAS)

    parent *Allocator       // 부모 할당기 (nil이면 루트)

    regions []*Region       // 소유한 메모리 영역 목록
    avail   Bitmap          // 사용 가능한 영역 추적 (1=가용, 0=사용중)
    used    Bitmap          // 최근 사용된 영역 추적 (1=사용됨, 0=미사용)
    empty   Bitmap          // nil 슬롯 추적 (1=nil, 0=비nil)
}
```

### 핵심 설계 결정

**왜 세 개의 Bitmap인가?**

| Bitmap | 의미 | 용도 |
|--------|------|------|
| `avail` | 현재 할당 가능 | Allocate() 시 빈 영역 찾기 |
| `used` | 마지막 Reclaim 이후 사용됨 | Trim() 시 미사용 영역 해제 |
| `empty` | regions 배열의 nil 슬롯 | addRegion() 시 빈 슬롯 재활용 |

이 세 가지 상태를 분리함으로써:
- `Reclaim()`은 모든 영역을 "가용"으로 표시만 하면 됨 (O(N) 비트 연산)
- `Trim()`은 "미사용" 영역만 선택적으로 해제 가능
- `addRegion()`은 nil 슬롯을 빠르게 찾아 재활용

### Allocate 흐름

```go
// allocator.go 라인 53-79
func (alloc *Allocator) Allocate(size int) *Region {
    alloc.lock()
    defer alloc.unlock()

    // 1. avail 비트맵에서 사용 가능한 영역 검색
    for i := range alloc.avail.IterValues(true) {
        region := alloc.regions[i]
        if region != nil && cap(region.data) >= size {
            alloc.avail.Set(i, false)   // 더 이상 가용 아님
            alloc.used.Set(i, true)     // 사용 중으로 표시
            return region
        }
    }

    // 2. 부모 할당기에서 빌려오기
    if alloc.parent != nil {
        region := alloc.parent.Allocate(size)
        alloc.addRegion(region, false)
        return region
    }

    // 3. 새 영역 할당 (런타임)
    region := &Region{data: allocBytes(size)}
    alloc.addRegion(region, false)
    return region
}
```

```
Allocate(1024)
    │
    ├─ avail 비트맵 검색 ──→ region[2] cap=2048 ≥ 1024? ──→ 반환
    │                                                      │
    │  (없으면)                                             │
    ├─ parent.Allocate(1024) ──→ 부모에서 영역 가져옴        │
    │                                                      │
    │  (부모 없으면)                                         │
    └─ allocBytes(1024) ──→ 새 메모리 할당 (64바이트 정렬)    │
                                                            ▼
                                                     Region 반환
```

### addRegion: nil 슬롯 재활용

```go
// allocator.go 라인 95-118
func (alloc *Allocator) addRegion(region *Region, free bool) {
    // nil 슬롯 찾기 (Trim 후 생긴 빈 자리)
    freeSlot := -1
    for i := range alloc.empty.IterValues(true) {
        freeSlot = i
        break
    }

    if freeSlot == -1 {
        freeSlot = len(alloc.regions)
        alloc.regions = append(alloc.regions, region)
    } else {
        alloc.regions[freeSlot] = region
    }

    // 세 비트맵 모두 크기 동기화
    alloc.avail.Resize(len(alloc.regions))
    alloc.used.Resize(len(alloc.regions))
    alloc.empty.Resize(len(alloc.regions))

    alloc.avail.Set(freeSlot, free)
    alloc.used.Set(freeSlot, !free)
    alloc.empty.Set(freeSlot, false)
}
```

---

## 3. Region 메모리 모델

### Region 구조체

`pkg/memory/memory.go`에서 정의:

```go
// Region은 Allocator가 소유하는 연속 메모리 영역이다.
type Region struct {
    data []byte  // 원시 데이터
}

func (m *Region) Data() []byte { return m.data }
```

**왜 Region을 별도 타입으로 만드는가?**

1. **소유권 추적**: Region은 항상 하나의 Allocator에 의해 소유된다
2. **수명 관리**: Reclaim 후에는 Region을 사용하면 안 된다는 것을 타입으로 표현
3. **확장 가능성**: 향후 메타데이터(크기, 정렬, 참조 카운트)를 추가할 수 있음

### 메모리 수명 주기

```
┌───────┐  Allocate  ┌───────────┐  사용 중   ┌───────────┐
│ Free  │──────────→ │ In-Use    │──────────→│ Reclaimable│
│ Pool  │            │           │           │           │
└───┬───┘            └───────────┘           └─────┬─────┘
    │                                              │
    │                Reclaim()                      │
    │←─────────────────────────────────────────────┘
    │
    │  Trim() (미사용)
    ▼
┌───────────┐
│  Released  │ → parent 반환 또는 GC
└───────────┘
```

### Reset vs Free

```go
// Reset = Trim + Reclaim (순서 주의)
func (alloc *Allocator) Reset() {
    alloc.Trim()     // 먼저 미사용 영역 해제
    alloc.Reclaim()  // 그 다음 모든 영역을 가용으로 전환
}

// Free = Reclaim + Trim (순서 주의)
func (alloc *Allocator) Free() {
    alloc.Reclaim()  // 먼저 모든 영역을 가용으로 전환
    alloc.Trim()     // 그 다음 전부 해제 (모든 영역이 미사용이므로)
}
```

**Reset vs Free의 차이**:
- `Reset()`: 최근 사용하지 않은 영역만 해제 → 자주 사용하는 영역은 보존 → **반복 사용에 최적**
- `Free()`: 모든 영역을 해제 → **완전 정리**

---

## 4. Bitmap 자료구조

### 구조체 정의

`pkg/memory/bitmap.go` 라인 18-30:

```go
type Bitmap struct {
    alloc *Allocator  // 선택적 메모리 할당기

    data []uint8      // 비트 패킹된 데이터 (LSB 순서)
    len  int          // 비트 수 (오프셋 포함)
    off  int          // 첫 번째 워드 내 오프셋
}
```

### LSB (Least Significant Bit) 순서

Apache Arrow와 호환되는 LSB 비트 순서를 사용한다:

```
바이트 0: [b7 b6 b5 b4 b3 b2 b1 b0]  ← b0이 인덱스 0
바이트 1: [b15 b14 b13 b12 b11 b10 b9 b8]

예: data = [0b00000101] → 인덱스 0=true, 인덱스 1=false, 인덱스 2=true
```

### 핵심 연산

| 연산 | 복잡도 | 설명 |
|------|--------|------|
| `Set(i, value)` | O(1) | 비트 하나 설정 |
| `Get(i)` | O(1) | 비트 하나 읽기 |
| `SetRange(from, to, value)` | O(N/8) | 범위 설정 |
| `Append(value)` | 분할 O(1) | 끝에 추가 |
| `IterValues(true)` | O(N/8) | 설정된 비트 순회 |
| `SetCount()` | O(N/8) | 설정된 비트 수 |
| `Resize(n)` | O(1) 또는 O(N) | 크기 변경 |

### IterValues: 비트 순회 최적화

```go
// bitmap.go 라인 264-295
func (bmap *Bitmap) IterValues(value bool) iter.Seq[int] {
    return func(yield func(int) bool) {
        var start int
        offset := bmap.off

        for i, word := range bmap.data {
            rem := word
            if !value {
                rem = ^rem  // NOT으로 0인 비트 찾기
            }

            if i == 0 && offset != 0 {
                rem &= ^uint8(0) << offset  // 오프셋 이전 비트 제거
            }

            for rem != 0 {
                firstSet := bits.TrailingZeros8(rem)  // 가장 낮은 설정 비트
                index := start + firstSet - offset
                if index >= bmap.len {
                    return
                } else if !yield(index) {
                    return
                }
                rem ^= 1 << firstSet  // 처리한 비트 제거
            }
            start += 8
        }
    }
}
```

**왜 이 방식이 빠른가?**

1. **바이트 단위 처리**: 8개 비트를 한 번에 검사
2. **`bits.TrailingZeros8`**: CPU의 CTZ(Count Trailing Zeros) 명령어로 하드웨어 가속
3. **조기 종료**: `rem != 0` 검사로 빈 워드 건너뛰기
4. **iter.Seq 패턴**: Go 1.23의 반복자 패턴으로 메모리 할당 없는 순회

### Slice: 오프셋 기반 부분 뷰

```go
// bitmap.go 라인 240-260
func (bmap *Bitmap) Slice(i, j int) *Bitmap {
    var (
        startWord = (bmap.off + i) / 8
        endWord   = ((bmap.off + j) / 8) + 1
        off       = (bmap.off + i) % 8
        newLen    = j - i
    )

    return &Bitmap{
        alloc: bmap.alloc,
        data:  bmap.data[startWord:endWord:endWord],
        len:   newLen,
        off:   off,  // 워드 내 시작 위치
    }
}
```

슬라이스된 비트맵은 원본 메모리를 공유하되, `off` 필드로 워드 내 시작 위치를 추적한다. 이는 복사 없이 부분 범위를 표현하는 방법이다.

---

## 5. Buffer 제네릭 구현

### Buffer[T] 구조체

`pkg/memory/buffer.go` 라인 14-17:

```go
type Buffer[T any] struct {
    alloc *Allocator
    data  []T
}
```

Go 1.18의 제네릭을 활용하여 임의 타입의 메모리 관리 버퍼를 제공한다.

### Grow: 용량 확장

```go
// buffer.go 라인 36-56
func (buf *Buffer[T]) Grow(n int) {
    if len(buf.data)+n <= cap(buf.data) {
        return
    }

    // 최소 2배 확장 전략
    newCap := max(len(buf.data)+n, 2*cap(buf.data))
    newBytes := newCap * int(unsafecast.Sizeof[T]())

    // Allocator에서 메모리 할당
    newMem := buf.alloc.Allocate(memalign.Align(newBytes))

    // unsafe 타입 캐스팅으로 []byte → []T 변환
    newData := castMemory[T](newMem)[:len(buf.data)]
    copy(newData, buf.data)
    buf.data = newData
}
```

**왜 기존 메모리를 해제하지 않는가?**

주석(라인 43-46)에서 설명:

> "This also keeps the old memory alive in the allocator, marked as used until [Allocator.Reclaim] is called. (We don't want to mark the old memory as free since there may still be other references to it based on copies/slices of this buffer)."

기존 `data` 슬라이스를 참조하는 다른 코드가 있을 수 있으므로, 명시적인 `Reclaim()` 호출 전까지는 기존 메모리를 유지한다. 이것이 Arena 패턴의 핵심이다.

### castMemory: unsafe 타입 변환

```go
// buffer.go 라인 148-160
func castMemory[To any](mem *Region) []To {
    orig := mem.Data()

    var (
        toSize = int(unsafecast.Sizeof[To]())
        toLen  = len(orig) / toSize
        toCap  = cap(orig) / toSize
    )

    outPointer := (*To)(unsafe.Pointer(unsafe.SliceData(orig)))
    return unsafe.Slice(outPointer, toCap)[:toLen]
}
```

`[]byte` → `[]T` 변환을 unsafe 포인터 연산으로 수행한다. 메모리 복사 없이 동일 메모리를 다른 타입으로 해석한다.

### Serialize: Arrow 호환 직렬화

```go
// buffer.go 라인 132-145
func (buf *Buffer[T]) Serialize() []byte {
    if buf.data == nil {
        return nil
    }

    out := unsafecast.Slice[T, byte](buf.data)
    alignedLen := memalign.Align(len(out))
    clear(out[len(out):alignedLen])  // 패딩 영역 0으로 초기화
    return out[:alignedLen]
}
```

**왜 64바이트 정렬이 필요한가?** Apache Arrow 사양에서 배열 데이터를 64바이트 경계에 정렬할 것을 권장한다. 이는 SIMD(벡터 연산) 최적화와 캐시라인 정렬을 위한 것이다.

---

## 6. 메모리 할당 전략

### allocBytes: 정렬된 메모리 할당

```go
// allocator.go 라인 218-231
func allocBytes(size int) []byte {
    const alignmentPadding = 64

    buf := make([]byte, size+alignmentPadding)

    addr := uint64(uintptr(unsafe.Pointer(&buf[0])))
    alignedAddr := memalign.Align64(addr)

    if alignedAddr != addr {
        offset := int(alignedAddr - addr)
        return buf[offset : offset+size : offset+size]
    }
    return buf[:size:size]
}
```

```
할당된 원시 버퍼 (size + 64):
┌─────────────────────────────────────────────────────────────┐
│ 패딩(0~63B) │          유효 데이터 (size B)                    │
└──────┬──────┴───────────────────────────────────────────────┘
       │
   64바이트 정렬 경계

반환되는 슬라이스: buf[offset:offset+size:offset+size]
  - len = size
  - cap = size (추가 확장 방지)
```

**왜 cap을 size로 제한하는가?** `buf[offset:offset+size:offset+size]`에서 세 번째 인자가 capacity를 제한한다. 이렇게 하면 슬라이스가 정렬 경계를 넘어 확장되는 것을 방지한다.

### AllocatedBytes / FreeBytes

```go
func (alloc *Allocator) AllocatedBytes() int {
    alloc.lock()
    defer alloc.unlock()
    var sum int
    for _, region := range alloc.regions {
        if region != nil {
            sum += cap(region.data)
        }
    }
    return sum
}

func (alloc *Allocator) FreeBytes() int {
    alloc.lock()
    defer alloc.unlock()
    var sum int
    for i := range alloc.avail.IterValues(true) {
        region := alloc.regions[i]
        if region != nil {
            sum += cap(region.data)
        }
    }
    return sum
}
```

이 두 메서드를 통해 할당기의 메모리 사용 상태를 모니터링할 수 있다:
- `AllocatedBytes()`: 할당기가 소유한 총 메모리
- `FreeBytes()`: 재사용 가능한 메모리
- `AllocatedBytes() - FreeBytes()`: 현재 사용 중인 메모리

---

## 7. Parent-Child 할당기 계층

### 계층 구조

```go
// allocator.go 라인 46-48
func NewAllocator(parent *Allocator) *Allocator {
    return &Allocator{parent: parent}
}
```

```
┌──────────────────────┐
│    Root Allocator     │ ← parent == nil
│  (대량 Region 풀)     │
├──────────────────────┤
│  ┌────────────────┐  │
│  │ Child A        │  │ ← 요청 처리 A용
│  │ (parent=Root)  │  │
│  └────────────────┘  │
│  ┌────────────────┐  │
│  │ Child B        │  │ ← 요청 처리 B용
│  │ (parent=Root)  │  │
│  └────────────────┘  │
└──────────────────────┘
```

### 영역 반환 메커니즘

```go
// allocator.go 라인 135-153
func (alloc *Allocator) Trim() {
    alloc.lock()
    defer alloc.unlock()

    for i := range alloc.used.IterValues(false) {  // 미사용 영역
        region := alloc.regions[i]
        if region == nil {
            continue
        }

        alloc.regions[i] = nil
        alloc.empty.Set(i, true)

        if alloc.parent != nil {
            alloc.parent.returnRegion(region)  // 부모에게 반환
        }
        // parent 없으면 GC가 회수
    }
}
```

### 수명 관리 시나리오

```
[요청 처리 시작]
child := NewAllocator(rootAllocator)

[데이터 처리]
region := child.Allocate(4096)
// ... region.Data() 사용 ...

[요청 처리 완료]
child.Reset()    // 미사용 영역을 부모에게 반환, 나머지 재활용
// 또는
child.Free()     // 모든 영역을 부모에게 반환

[다음 요청]
child.Allocate(...)  // 필요하면 부모에서 다시 빌려옴
```

**왜 계층 구조인가?**

1. **격리**: 각 요청(또는 고루틴)이 독립적인 할당기를 가지므로, 요청 간 간섭 없음
2. **풀링**: 부모 할당기가 큰 Region을 보유하고, 자식에게 빌려줌으로써 실제 `make()` 호출 최소화
3. **안전한 해제**: 자식의 `Free()`가 부모에게 영역을 반환하므로, 메모리 누수 방지

---

## 8. 트레이싱 설정 시스템

### Config 구조체

`pkg/tracing/config.go`:

```go
type Config struct {
    Enabled bool `yaml:"enabled"`
}

func (cfg *Config) RegisterFlags(f *flag.FlagSet) {
    f.BoolVar(&cfg.Enabled, "tracing.enabled", true, "Set to false to disable tracing.")
}

func (cfg *Config) RegisterFlagsWithPrefix(prefix string, f *flag.FlagSet) {
    f.BoolVar(&cfg.Enabled, prefix+"tracing.enabled", true, "Set to false to disable tracing.")
}
```

### 설계 원칙

1. **기본값 true**: 트레이싱은 기본적으로 활성화됨 (관찰 가능성 우선)
2. **dskit 패턴 준수**: `RegisterFlags`/`RegisterFlagsWithPrefix` 메서드 제공
3. **YAML/CLI 호환**: 설정 파일과 CLI 플래그 모두 지원

### Loki 서비스별 트레이싱 플래그

```yaml
# loki.yaml
tracing:
  enabled: true

# 또는 CLI
loki --tracing.enabled=true

# Query Tee에서의 사용 (cmd/querytee/main.go)
cfg.Tracing.RegisterFlags(flag.CommandLine)
```

---

## 9. OpenTelemetry 통합

### KeyValue 변환

`pkg/tracing/otel_kv.go`:

```go
func KeyValuesToOTelAttributes(kvps ...any) []attribute.KeyValue {
    attrs := make([]attribute.KeyValue, 0, len(kvps)/2)
    for i := 0; i < len(kvps); i += 2 {
        if i+1 < len(kvps) {
            key, ok := kvps[i].(string)
            if !ok {
                key = fmt.Sprintf("not_string_key:%v", kvps[i])
            }
            attrs = append(attrs, tracing.KeyValueToOTelAttribute(key, kvps[i+1]))
        }
    }
    return attrs
}
```

### 변환 흐름

```
Go Kit 로깅 스타일:
  "key1", value1, "key2", value2, ...
       │
       ▼
KeyValuesToOTelAttributes()
       │
       ├─ kvps[0] → key (string)
       ├─ kvps[1] → value (any)
       ├─ kvps[2] → key (string)
       ├─ kvps[3] → value (any)
       └─ ...
       │
       ▼
OpenTelemetry 어트리뷰트:
  []attribute.KeyValue{
      {Key: "key1", Value: ...},
      {Key: "key2", Value: ...},
  }
```

**왜 이 변환이 필요한가?** Loki 내부에서는 Go Kit 로깅 스타일(`"key", value` 쌍)을 광범위하게 사용한다. OpenTelemetry는 `attribute.KeyValue` 타입을 사용한다. 이 함수가 두 세계를 연결한다.

### 비정상 키 처리

```go
key, ok := kvps[i].(string)
if !ok {
    key = fmt.Sprintf("not_string_key:%v", kvps[i])
}
```

키가 문자열이 아닌 경우에도 패닉하지 않고, `not_string_key:` 접두사로 변환한다. 이는 방어적 프로그래밍으로, 런타임 오류를 방지한다.

### Query Tee에서의 트레이싱 사용

```go
// cmd/querytee/main.go 라인 43-57
if cfg.Tracing.Enabled {
    trace, err := tracing.NewOTelOrJaegerFromEnv("loki-querytee", util_log.Logger)
    if err != nil {
        level.Error(util_log.Logger).Log("msg", "error initializing tracing", "err", err)
        exit(1)
    }
    defer func() {
        if trace != nil {
            trace.Close()
        }
    }()
}
```

`NewOTelOrJaegerFromEnv`는 dskit 패키지에서 제공하며, 환경 변수를 통해 OpenTelemetry 또는 Jaeger 트레이서를 자동 선택한다.

---

## 10. 메모리 정렬 최적화

### 64바이트 정렬의 이유

현대 CPU의 캐시라인은 일반적으로 64바이트이다:

```
캐시라인 0:  [  0 ~  63 바이트]
캐시라인 1:  [ 64 ~ 127 바이트]
캐시라인 2:  [128 ~ 191 바이트]
...

[정렬되지 않은 접근]
데이터 시작: 바이트 40
┌──── 캐시라인 0 ────┐┌──── 캐시라인 1 ────┐
│        ############││##                  │
└────────────────────┘└────────────────────┘
  → 2개 캐시라인 접근 필요

[정렬된 접근]
데이터 시작: 바이트 64
                      ┌──── 캐시라인 1 ────┐
                      │##############      │
                      └────────────────────┘
  → 1개 캐시라인 접근
```

### memalign.Align 함수

`internal/memalign/` 패키지에서 정렬 유틸리티를 제공:

```go
// 값을 64바이트 경계로 올림
func Align(n int) int {
    return (n + 63) &^ 63  // 비트 마스킹으로 64의 배수로 올림
}

// 주소를 64바이트 경계로 올림
func Align64(addr uint64) uint64 {
    return (addr + 63) &^ 63
}
```

### Bitmap에서의 정렬

```go
// bitmap.go 라인 148-157
func allocBitmapData(alloc *Allocator, minSize int) []uint8 {
    size := memalign.Align(minSize)  // 64바이트 정렬

    if alloc != nil {
        mem := alloc.Allocate(size)
        return mem.Data()
    }
    return make([]uint8, size)
}
```

---

## 11. 동시성 안전성

### CAS 기반 락

```go
// allocator.go 라인 81-91
func (alloc *Allocator) lock() {
    if !alloc.locked.CompareAndSwap(false, true) {
        panic(errConcurrentUse)
    }
}

func (alloc *Allocator) unlock() {
    if !alloc.locked.CompareAndSwap(true, false) {
        panic(errConcurrentUse)
    }
}
```

**왜 mutex 대신 CAS + panic인가?**

Allocator는 **의도적으로 goroutine-safe하지 않다**. 패키지 문서에서 명시:

> "Allocators are not goroutine safe. If an Allocator methods are called concurrently, the method will panic."

이는 설계 결정이다:
1. **성능**: mutex 대신 CAS는 uncontended 경우 더 빠름
2. **버그 감지**: 동시 접근은 설계 오류이므로 panic으로 즉시 감지
3. **단순성**: 각 goroutine이 자체 할당기를 사용하도록 강제

### 올바른 사용 패턴

```go
// 올바른: 각 goroutine에 별도 할당기
root := memory.NewAllocator(nil)

go func() {
    child := memory.NewAllocator(root)  // 자체 할당기
    region := child.Allocate(1024)
    // ... 사용 ...
    child.Free()
}()

go func() {
    child := memory.NewAllocator(root)  // 자체 할당기
    region := child.Allocate(2048)
    // ... 사용 ...
    child.Free()
}()
```

```go
// 잘못된: 동일 할당기를 여러 goroutine에서 사용
alloc := memory.NewAllocator(nil)

go func() {
    alloc.Allocate(1024)  // PANIC: concurrent use
}()

go func() {
    alloc.Allocate(2048)  // PANIC: concurrent use
}()
```

---

## 12. 성능 분석 및 운영 가이드

### 메모리 할당기 모니터링

```go
alloc := memory.NewAllocator(nil)

// 사용 통계
total := alloc.AllocatedBytes()   // 총 소유 메모리
free := alloc.FreeBytes()         // 재사용 가능 메모리
used := total - free              // 실제 사용 중

// 효율성 지표
utilization := float64(used) / float64(total)  // 메모리 활용률
```

### DataObj에서의 실제 사용

`pkg/memory/memory.go` 패키지 문서에서 명시:

> "Memory is EXPERIMENTAL and is currently only intended for use by [github.com/grafana/loki/v3/pkg/dataobj]."

DataObj(컬럼나 데이터 포맷)는 대량의 메모리 버퍼를 사용하므로, arena 스타일 할당이 GC 부담을 크게 줄인다.

```
[DataObj 빌더에서의 사용 패턴]
1. 루트 할당기 생성
2. 각 컬럼 빌더가 자식 할당기 사용
3. 컬럼 데이터를 Buffer[T]로 관리
4. Bitmap으로 null 마스크 관리
5. 빌드 완료 후 Serialize()로 Arrow 호환 출력
6. Free()로 전체 메모리 반환
```

### 트레이싱 운영 체크리스트

| 항목 | 설정 | 권장 |
|------|------|------|
| 프로덕션 트레이싱 | `tracing.enabled=true` | 항상 활성화 |
| 샘플링 | 환경 변수로 설정 | 1% ~ 10% |
| 백엔드 | Jaeger / Tempo | Grafana Tempo 권장 |
| 서비스 이름 | 자동 설정 | 각 컴포넌트별 (loki, querytee 등) |

### 성능 튜닝 가이드

| 시나리오 | 문제 | 해결 |
|----------|------|------|
| GC 과다 | 짧은 수명의 대량 버퍼 | Arena 할당기 사용 |
| 캐시 미스 | 비정렬 메모리 접근 | 64바이트 정렬 보장 |
| 메모리 누수 | Reclaim 미호출 | defer alloc.Free() 패턴 |
| 동시성 버그 | 공유 할당기 | goroutine별 자식 할당기 |
| 메모리 과다 사용 | Trim 미호출 | Reset() 주기적 호출 |

---

## 부록: 주요 소스 파일 참조

| 파일 | 설명 |
|------|------|
| `pkg/memory/memory.go` | Region 타입, 패키지 문서 (27줄) |
| `pkg/memory/allocator.go` | Arena Allocator (232줄) |
| `pkg/memory/bitmap.go` | LSB Bitmap (296줄) |
| `pkg/memory/buffer.go` | Buffer[T] 제네릭 버퍼 (161줄) |
| `pkg/memory/internal/memalign/` | 64바이트 정렬 유틸리티 |
| `pkg/memory/internal/unsafecast/` | unsafe 타입 변환 |
| `pkg/tracing/config.go` | 트레이싱 설정 (18줄) |
| `pkg/tracing/otel_kv.go` | OTel 키-값 변환 (23줄) |
