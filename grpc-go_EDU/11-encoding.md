# 11. 인코딩 및 압축 서브시스템 (Encoding & Compression)

## 목차

1. [개요](#1-개요)
2. [아키텍처 전체 흐름](#2-아키텍처-전체-흐름)
3. [Codec 인터페이스 체계](#3-codec-인터페이스-체계)
4. [Compressor 인터페이스](#4-compressor-인터페이스)
5. [레지스트리 시스템](#5-레지스트리-시스템)
6. [Protocol Buffers 코덱 구현 상세](#6-protocol-buffers-코덱-구현-상세)
7. [gzip 압축기 구현 상세](#7-gzip-압축기-구현-상세)
8. [메시지 프레이밍](#8-메시지-프레이밍)
9. [직렬화 흐름 (송신)](#9-직렬화-흐름-송신)
10. [역직렬화 흐름 (수신)](#10-역직렬화-흐름-수신)
11. [Content-Type 협상](#11-content-type-협상)
12. [V1 vs V2 Codec 차이와 브리지 패턴](#12-v1-vs-v2-codec-차이와-브리지-패턴)
13. [커스텀 코덱 및 압축기 작성 방법](#13-커스텀-코덱-및-압축기-작성-방법)
14. [성능 최적화 전략](#14-성능-최적화-전략)
15. [정리](#15-정리)

---

## 1. 개요

gRPC-Go의 인코딩/압축 서브시스템은 사용자의 Go 구조체(주로 protobuf 메시지)를 네트워크 전송 가능한 바이트 스트림으로 변환하고, 수신 측에서 다시 원래 구조체로 복원하는 전체 파이프라인을 담당한다.

이 서브시스템이 해결하는 핵심 문제는 다음과 같다:

| 문제 | 해결 방식 |
|------|-----------|
| 직렬화 포맷의 교체 가능성 | Codec 인터페이스 + 레지스트리 패턴 |
| 압축 알고리즘의 교체 가능성 | Compressor 인터페이스 + 레지스트리 패턴 |
| 고성능 메모리 관리 | mem.BufferSlice + Buffer Pool |
| 하위 호환성 유지 | V0/V1/V2 브리지 패턴 |
| 클라이언트-서버 간 인코딩 협상 | Content-Type 헤더 + grpc-encoding 헤더 |

### 핵심 소스 파일

| 파일 경로 | 역할 |
|-----------|------|
| `encoding/encoding.go` | Codec V1, Compressor 인터페이스 정의 및 레지스트리 |
| `encoding/encoding_v2.go` | CodecV2 인터페이스 정의 (mem.BufferSlice 기반) |
| `encoding/proto/proto.go` | Protocol Buffers 코덱 (기본 코덱) |
| `encoding/gzip/gzip.go` | gzip 압축기 구현 |
| `rpc_util.go` | encode, compress, decompress, recvAndDecompress, prepareMsg 함수 |
| `codec.go` | 레거시 Codec(V0), baseCodec, 브리지 패턴 |
| `internal/grpcutil/method.go` | Content-Type 파싱/생성 |
| `internal/grpcutil/compressor.go` | 등록된 압축기 이름 관리 |

---

## 2. 아키텍처 전체 흐름

### 송신 측 (클라이언트 또는 서버)

```
사용자 메시지 (proto.Message)
        |
        v
  +-------------+
  |   encode()   |  ← Codec.Marshal() 호출
  +-------------+
        |
        v
  직렬화된 데이터 (mem.BufferSlice)
        |
        v
  +-------------+
  |  compress()  |  ← Compressor.Compress() 호출 (선택적)
  +-------------+
        |
        v
  압축된 데이터 (mem.BufferSlice)
        |
        v
  +-------------+
  | msgHeader()  |  ← 5바이트 헤더 생성 (1 flag + 4 length)
  +-------------+
        |
        v
  [헤더][페이로드] → HTTP/2 DATA 프레임으로 전송
```

### 수신 측 (클라이언트 또는 서버)

```
  HTTP/2 DATA 프레임 수신
        |
        v
  +-----------------+
  | parser.recvMsg()|  ← 5바이트 헤더 파싱 → payloadFormat + 데이터
  +-----------------+
        |
        v
  +---------------------+
  | recvAndDecompress() |  ← 압축 플래그 확인 → decompress() 호출
  +---------------------+
        |
        v
  +-------------+
  |    recv()    |  ← Codec.Unmarshal() 호출
  +-------------+
        |
        v
  사용자 메시지 (proto.Message)
```

---

## 3. Codec 인터페이스 체계

gRPC-Go는 역사적으로 세 가지 버전의 Codec 인터페이스를 가지고 있다.

### 3.1 V0 — 레거시 grpc.Codec (Deprecated)

```go
// 소스: codec.go:92-105
type Codec interface {
    Marshal(v any) ([]byte, error)
    Unmarshal(data []byte, v any) error
    String() string  // ← Name()이 아닌 String()
}
```

V0는 gRPC-Go 초기 버전에서 사용되었다. `String()` 메서드가 `Name()` 대신 사용되었고, `[]byte` 기반이어서 메모리 재사용이 불가능했다.

**왜 아직 남아있는가?** 기존 사용자 코드와의 하위 호환성을 위해 `CallCustomCodec()` 옵션을 통해 여전히 사용할 수 있지만, Deprecated 상태이다.

### 3.2 V1 — encoding.Codec

```go
// 소스: encoding/encoding.go:99-111
type Codec interface {
    Marshal(v any) ([]byte, error)
    Unmarshal(data []byte, v any) error
    Name() string
}
```

V1은 V0에서 `String()`을 `Name()`으로 변경하고, `encoding` 패키지로 이동한 버전이다. `[]byte` 기반이므로 매번 새로운 슬라이스를 할당해야 한다.

### 3.3 V2 — encoding.CodecV2 (최신)

```go
// 소스: encoding/encoding_v2.go:27-44
type CodecV2 interface {
    Marshal(v any) (out mem.BufferSlice, err error)
    Unmarshal(data mem.BufferSlice, v any) error
    Name() string
}
```

**왜 V2가 필요한가?** `[]byte` 대신 `mem.BufferSlice`를 사용함으로써 다음을 달성한다:

| 측면 | V1 ([]byte) | V2 (mem.BufferSlice) |
|------|-------------|----------------------|
| 메모리 할당 | 매번 새 슬라이스 할당 | Buffer Pool에서 재사용 |
| 참조 카운팅 | 없음 | Ref()/Free()로 생명주기 관리 |
| Zero-copy | 불가 | 가능 (청크 단위) |
| 대용량 메시지 | GC 압박 증가 | Pool 기반으로 GC 부담 감소 |

`mem.BufferSlice`의 핵심 구조:

```go
// 소스: mem/buffer_slice.go:45
type BufferSlice []Buffer
```

`BufferSlice`는 여러 `Buffer` 인스턴스에 걸친 데이터를 표현하는 불변(immutable) 슬라이스다. `Ref()`, `Free()`, `Len()`, `Materialize()` 등의 메서드를 제공한다.

### 3.4 baseCodec — 내부 통합 인터페이스

```go
// 소스: codec.go:27-34
type baseCodec interface {
    Marshal(v any) (mem.BufferSlice, error)
    Unmarshal(data mem.BufferSlice, v any) error
}
```

`baseCodec`은 `Name()` 메서드를 제외한 CodecV2의 핵심 기능만 추출한 인터페이스다. **왜 Name()을 뺐는가?** 이름은 레지스트리에 등록할 때만 필요하고, 실제 직렬화/역직렬화 수행 시에는 필요 없다. 이 설계로 V0, V1, V2 코덱을 모두 `baseCodec`으로 통일하여 `encode()`, `recv()` 등의 함수에서 단일 인터페이스로 처리할 수 있다.

---

## 4. Compressor 인터페이스

### 4.1 현재 인터페이스 — encoding.Compressor

```go
// 소스: encoding/encoding.go:59-74
type Compressor interface {
    Compress(w io.Writer) (io.WriteCloser, error)
    Decompress(r io.Reader) (io.Reader, error)
    Name() string
}
```

**왜 io.Writer/io.Reader 기반인가?**

압축은 본질적으로 스트리밍 연산이다. `[]byte → []byte` 방식은 전체 데이터를 메모리에 올려야 하지만, `io.Writer/io.Reader` 기반은 청크 단위로 처리할 수 있어 대용량 메시지에 효율적이다.

```
Compress 호출 흐름:
  io.Writer (출력 대상)
       |
       v
  Compress(w) → io.WriteCloser (압축 파이프)
       |
       v
  데이터를 WriteCloser에 기록 → w에 압축된 바이트 출력
       |
       v
  Close() → 압축 마무리 (flush)
```

### 4.2 레거시 인터페이스 — grpc.Compressor / grpc.Decompressor

```go
// 소스: rpc_util.go:50-58
type Compressor interface {          // Deprecated
    Do(w io.Writer, p []byte) error
    Type() string
}

// 소스: rpc_util.go:109-117
type Decompressor interface {        // Deprecated
    Do(r io.Reader) ([]byte, error)
    Type() string
}
```

레거시 인터페이스는 압축과 해제가 별도 인터페이스로 분리되어 있었다. 현재 `encoding.Compressor`는 이를 단일 인터페이스로 통합했다.

---

## 5. 레지스트리 시스템

### 5.1 코덱 레지스트리

gRPC-Go는 코덱과 압축기를 전역 맵(map)에 등록하고, content-subtype 또는 이름으로 조회한다.

```go
// 소스: encoding/encoding.go:113
var registeredCodecs = make(map[string]any)
```

**왜 `map[string]any`인가?** V1 `Codec`과 V2 `CodecV2`를 동일한 맵에 저장하기 위해서이다. 조회 시 타입 단언(type assertion)으로 구분한다.

```
등록 흐름:

  RegisterCodec(codec)        → registeredCodecs["proto"] = Codec(V1)
  RegisterCodecV2(codec)      → registeredCodecs["proto"] = CodecV2(V2)
                                                   ↑
                                            같은 키면 V2가 덮어씀
```

```go
// 소스: encoding/encoding.go:129-138
func RegisterCodec(codec Codec) {
    if codec == nil {
        panic("cannot register a nil Codec")
    }
    if codec.Name() == "" {
        panic("cannot register Codec with empty string result for Name()")
    }
    contentSubtype := strings.ToLower(codec.Name())
    registeredCodecs[contentSubtype] = codec
}

// 소스: encoding/encoding_v2.go:63-72
func RegisterCodecV2(codec CodecV2) {
    if codec == nil {
        panic("cannot register a nil CodecV2")
    }
    if codec.Name() == "" {
        panic("cannot register CodecV2 with empty string result for Name()")
    }
    contentSubtype := strings.ToLower(codec.Name())
    registeredCodecs[contentSubtype] = codec
}
```

조회는 V1과 V2를 각각의 타입 단언으로 시도한다:

```go
// 소스: encoding/encoding.go:144-147
func GetCodec(contentSubtype string) Codec {
    c, _ := registeredCodecs[contentSubtype].(Codec)
    return c
}

// 소스: encoding/encoding_v2.go:78-80
func GetCodecV2(contentSubtype string) CodecV2 {
    c, _ := registeredCodecs[contentSubtype].(CodecV2)
    return c
}
```

**내부 조회 우선순위 (codec.go:41-47):**

```go
// 소스: codec.go:41-47
func getCodec(name string) encoding.CodecV2 {
    if codecV1 := encoding.GetCodec(name); codecV1 != nil {
        return newCodecV1Bridge(codecV1)
    }
    return encoding.GetCodecV2(name)
}
```

주의할 점: 내부 `getCodec()` 함수는 V1을 먼저 확인한다. V1이 있으면 브리지로 감싸서 반환하고, 없으면 V2를 조회한다. 이 순서는 하위 호환성을 보장한다.

### 5.2 압축기 레지스트리

```go
// 소스: encoding/encoding.go:76
var registeredCompressor = make(map[string]Compressor)
```

```go
// 소스: encoding/encoding.go:87-92
func RegisterCompressor(c Compressor) {
    registeredCompressor[c.Name()] = c
    if !grpcutil.IsCompressorNameRegistered(c.Name()) {
        grpcutil.RegisteredCompressorNames = append(grpcutil.RegisteredCompressorNames, c.Name())
    }
}
```

**이중 등록의 이유:** `registeredCompressor` 맵 외에 `grpcutil.RegisteredCompressorNames` 슬라이스에도 이름을 등록한다. 이 슬라이스는 HTTP/2 헤더에 `grpc-accept-encoding` 값으로 포함되어, 클라이언트가 지원하는 압축 알고리즘을 서버에 알린다.

```go
// 소스: internal/grpcutil/compressor.go:25-42
var RegisteredCompressorNames []string

func IsCompressorNameRegistered(name string) bool {
    for _, compressor := range RegisteredCompressorNames {
        if compressor == name {
            return true
        }
    }
    return false
}

func RegisteredCompressors() string {
    return strings.Join(RegisteredCompressorNames, ",")
}
```

### 5.3 등록 타이밍 제약

```
모든 등록은 init() 함수에서 수행해야 한다.
            ↓
  이유: 레지스트리가 thread-safe하지 않기 때문
            ↓
  init()은 Go 런타임이 단일 고루틴에서 순차적으로 실행
            ↓
  프로그램 시작 후에는 읽기만 하므로 안전
```

**왜 sync.Map이나 mutex를 사용하지 않는가?** 등록은 프로그램 시작 시 한 번만 발생하고, 이후에는 읽기만 하므로 동기화 비용을 피하는 것이 합리적이다. gRPC-Go는 성능에 민감한 라이브러리이므로 불필요한 오버헤드를 제거한다.

---

## 6. Protocol Buffers 코덱 구현 상세

### 6.1 등록

```go
// 소스: encoding/proto/proto.go:33-37
const Name = "proto"

func init() {
    encoding.RegisterCodecV2(&codecV2{})
}
```

proto 코덱은 `import _ "google.golang.org/grpc/encoding/proto"` 를 통해 자동 등록된다. `codec.go`에서 이 사이드 이펙트 임포트가 수행된다:

```go
// 소스: codec.go:23
import _ "google.golang.org/grpc/encoding/proto" // to register the Codec for "proto"
```

### 6.2 Marshal — 크기 기반 분기

```go
// 소스: encoding/proto/proto.go:43-83
func (c *codecV2) Marshal(v any) (data mem.BufferSlice, err error) {
    vv := messageV2Of(v)
    if vv == nil {
        return nil, fmt.Errorf("proto: failed to marshal, message is %T, want proto.Message", v)
    }

    size := proto.Size(vv)

    marshalOptions := proto.MarshalOptions{UseCachedSize: true}

    if mem.IsBelowBufferPoolingThreshold(size) {
        buf, err := marshalOptions.Marshal(vv)
        if err != nil {
            return nil, err
        }
        data = append(data, mem.SliceBuffer(buf))
    } else {
        pool := mem.DefaultBufferPool()
        buf := pool.Get(size)
        if _, err := marshalOptions.MarshalAppend((*buf)[:0], vv); err != nil {
            pool.Put(buf)
            return nil, err
        }
        data = append(data, mem.NewBuffer(buf, pool))
    }

    return data, nil
}
```

이 코드에는 핵심적인 성능 최적화가 있다:

**1. UseCachedSize 최적화:**

```
proto.Size(vv)                       ← 1차: 크기 계산
marshalOptions.MarshalAppend(...)    ← 2차: 직렬화 (크기 재계산 건너뜀)
                                        UseCachedSize: true
```

`proto.Size()`를 먼저 호출하면 protobuf 내부에 크기가 캐싱된다. 이후 `MarshalAppend()` 호출 시 `UseCachedSize: true`를 설정하면 크기를 다시 계산하지 않고 캐시된 값을 사용한다. 이로써 직렬화 성능이 크게 향상된다.

**2. 크기 기반 풀링 분기:**

```
  size = proto.Size(vv)
         |
    size <= 1024 바이트?  (bufferPoolingThreshold)
     /            \
    Yes            No
     |              |
  일반 Marshal    Pool에서 버퍼 할당
  (SliceBuffer)   (NewBuffer + pool)
     |              |
  GC가 관리      Pool이 관리 (재사용)
```

**왜 이런 분기가 필요한가?** 작은 메시지(1KB 이하)는 풀링 오버헤드(Get/Put)가 할당 비용보다 클 수 있다. 반면 큰 메시지는 GC 부담이 크므로 풀에서 버퍼를 재사용하는 것이 유리하다.

### 6.3 Unmarshal

```go
// 소스: encoding/proto/proto.go:85-97
func (c *codecV2) Unmarshal(data mem.BufferSlice, v any) (err error) {
    vv := messageV2Of(v)
    if vv == nil {
        return fmt.Errorf("failed to unmarshal, message is %T, want proto.Message", v)
    }

    buf := data.MaterializeToBuffer(mem.DefaultBufferPool())
    defer buf.Free()
    return proto.Unmarshal(buf.ReadOnlyData(), vv)
}
```

`Unmarshal`에서는 `BufferSlice`를 단일 연속 바이트 배열로 결합(`MaterializeToBuffer`)한 후 `proto.Unmarshal()`에 전달한다.

**왜 zero-copy가 아닌가?** 코드 주석(93-95행)에서 언급하듯이, `proto.Unmarshal`은 아직 `mem.BufferSlice`를 직접 지원하지 않는다. 이는 향후 vtprotobuf 같은 라이브러리를 통해 개선될 수 있다.

### 6.4 protobuf 메시지 버전 호환

```go
// 소스: encoding/proto/proto.go:99-108
func messageV2Of(v any) proto.Message {
    switch v := v.(type) {
    case protoadapt.MessageV1:
        return protoadapt.MessageV2Of(v)
    case protoadapt.MessageV2:
        return v
    }
    return nil
}
```

`messageV2Of()` 함수는 protobuf V1 메시지(구 `github.com/golang/protobuf` 패키지)와 V2 메시지(`google.golang.org/protobuf`)를 모두 처리한다. `protoadapt` 패키지를 통해 V1 → V2 변환을 수행한다.

---

## 7. gzip 압축기 구현 상세

### 7.1 구조

```go
// 소스: encoding/gzip/gzip.go:117-120
type compressor struct {
    poolCompressor   sync.Pool
    poolDecompressor sync.Pool
}
```

gzip 압축기는 두 개의 `sync.Pool`을 유지한다:
- `poolCompressor`: gzip.Writer 인스턴스를 풀링
- `poolDecompressor`: gzip.Reader 인스턴스를 풀링

**왜 sync.Pool인가?** gzip.Writer/Reader 생성은 비용이 높다(내부 해시 테이블, 허프만 트리 초기화). `sync.Pool`로 재사용하면 GC 압박을 줄이고 할당 비용을 아낀다.

### 7.2 등록 및 writer 래핑

```go
// 소스: encoding/gzip/gzip.go:40-46
func init() {
    c := &compressor{}
    c.poolCompressor.New = func() any {
        return &writer{Writer: gzip.NewWriter(io.Discard), pool: &c.poolCompressor}
    }
    encoding.RegisterCompressor(c)
}

// 소스: encoding/gzip/gzip.go:48-51
type writer struct {
    *gzip.Writer
    pool *sync.Pool
}
```

`writer` 구조체는 `gzip.Writer`를 임베딩하면서 자신이 속한 풀의 참조를 유지한다. 이를 통해 `Close()` 호출 시 자동으로 풀에 반환된다.

### 7.3 Compress 흐름

```go
// 소스: encoding/gzip/gzip.go:73-77
func (c *compressor) Compress(w io.Writer) (io.WriteCloser, error) {
    z := c.poolCompressor.Get().(*writer)
    z.Writer.Reset(w)
    return z, nil
}
```

```
  Pool에서 writer 가져오기
        |
        v
  Reset(w) → 기존 상태 초기화, 새 출력 대상 설정
        |
        v
  WriteCloser 반환
        |
        v
  호출자가 Write() → 압축 데이터가 w에 기록
        |
        v
  Close() 호출 시:
    +--> gzip flush/finalize
    +--> pool.Put(z) → 풀에 반환
```

```go
// 소스: encoding/gzip/gzip.go:79-82
func (z *writer) Close() error {
    defer z.pool.Put(z)   // 풀에 반환
    return z.Writer.Close()
}
```

**왜 `io.Discard`로 초기화하는가?** `gzip.NewWriter()`는 반드시 `io.Writer`를 받아야 한다. 풀에 보관 중인 writer는 아직 실제 출력 대상이 없으므로 `io.Discard`(아무것도 하지 않는 writer)로 초기화한다. 실제 사용 시 `Reset()`으로 실제 출력 대상으로 교체한다.

### 7.4 Decompress 흐름

```go
// 소스: encoding/gzip/gzip.go:89-103
func (c *compressor) Decompress(r io.Reader) (io.Reader, error) {
    z, inPool := c.poolDecompressor.Get().(*reader)
    if !inPool {
        newZ, err := gzip.NewReader(r)
        if err != nil {
            return nil, err
        }
        return &reader{Reader: newZ, pool: &c.poolDecompressor}, nil
    }
    if err := z.Reset(r); err != nil {
        c.poolDecompressor.Put(z)
        return nil, err
    }
    return z, nil
}
```

```
  Pool에서 reader 가져오기
       |
  풀에 있었는가?
   /         \
  No          Yes
   |           |
  새 gzip.NewReader    z.Reset(r)
  생성 및 래핑           |
       \              /
        v            v
      reader 반환 (io.Reader)
```

### 7.5 reader의 자동 반환 메커니즘

```go
// 소스: encoding/gzip/gzip.go:105-111
func (z *reader) Read(p []byte) (n int, err error) {
    n, err = z.Reader.Read(p)
    if err == io.EOF {
        z.pool.Put(z)  // EOF 도달 시 자동으로 풀에 반환
    }
    return n, err
}
```

**왜 Read()에서 반환하는가?** `Decompress()`가 반환하는 `io.Reader`의 호출자는 데이터를 다 읽으면 더 이상 reader를 사용하지 않는다. EOF 시점에 자동으로 풀에 반환함으로써, 호출자가 별도로 `Close()`를 호출하지 않아도 리소스가 회수된다. 이는 `io.Reader` 인터페이스만 반환하므로 `Close()`를 강제할 수 없는 상황에서의 우아한 해결책이다.

### 7.6 압축 레벨 설정

```go
// 소스: encoding/gzip/gzip.go:58-71
func SetLevel(level int) error {
    if level < gzip.DefaultCompression || level > gzip.BestCompression {
        return fmt.Errorf("grpc: invalid gzip compression level: %d", level)
    }
    c := encoding.GetCompressor(Name).(*compressor)
    c.poolCompressor.New = func() any {
        w, err := gzip.NewWriterLevel(io.Discard, level)
        if err != nil {
            panic(err)
        }
        return &writer{Writer: w, pool: &c.poolCompressor}
    }
    return nil
}
```

`SetLevel()`은 이미 등록된 compressor의 `poolCompressor.New` 함수를 교체한다. 이후 풀에서 새로 생성되는 writer들은 변경된 압축 레벨을 사용한다.

| 상수 | 값 | 설명 |
|------|----|------|
| `gzip.DefaultCompression` | -1 | 기본 압축 레벨 (보통 6) |
| `gzip.NoCompression` | 0 | 압축 없음 |
| `gzip.BestSpeed` | 1 | 가장 빠른 압축 |
| `gzip.BestCompression` | 9 | 가장 높은 압축률 |

---

## 8. 메시지 프레이밍

gRPC는 HTTP/2 위에서 자체적인 메시지 프레이밍을 수행한다. 각 메시지는 5바이트 헤더 + 페이로드로 구성된다.

### 8.1 프레임 구조

```
+--+--+--+--+--+--+--+--+--+--+--+--+--+--+--+--+
|  Compressed Flag  |     Message Length (4 bytes)  |
|     (1 byte)      |       (Big Endian uint32)     |
+--+--+--+--+--+--+--+--+--+--+--+--+--+--+--+--+
|                                                   |
|              Message Payload                      |
|           (Length 바이트만큼)                       |
|                                                   |
+--+--+--+--+--+--+--+--+--+--+--+--+--+--+--+--+

  바이트 0:     압축 플래그
                  0 = 비압축 (compressionNone)
                  1 = 압축됨 (compressionMade)

  바이트 1-4:   페이로드 길이 (Big Endian)
                  최대 2^32 - 1 바이트 ≈ 4GB
```

### 8.2 상수 정의

```go
// 소스: rpc_util.go:857-861
const (
    payloadLen = 1       // 압축 플래그 크기
    sizeLen    = 4       // 메시지 길이 크기
    headerLen  = payloadLen + sizeLen  // = 5
)
```

### 8.3 payloadFormat 타입

```go
// 소스: rpc_util.go:714-723
type payloadFormat uint8

const (
    compressionNone payloadFormat = 0  // 비압축
    compressionMade payloadFormat = 1  // 압축됨
)

func (pf payloadFormat) isCompressed() bool {
    return pf == compressionMade
}
```

### 8.4 헤더 생성 — msgHeader()

```go
// 소스: rpc_util.go:865-881
func msgHeader(data, compData mem.BufferSlice, pf payloadFormat) (hdr []byte, payload mem.BufferSlice) {
    hdr = make([]byte, headerLen)
    hdr[0] = byte(pf)

    var length uint32
    if pf.isCompressed() {
        length = uint32(compData.Len())
        payload = compData
    } else {
        length = uint32(data.Len())
        payload = data
    }

    binary.BigEndian.PutUint32(hdr[payloadLen:], length)
    return hdr, payload
}
```

**왜 Big Endian인가?** gRPC 프로토콜 스펙(PROTOCOL-HTTP2.md)에서 네트워크 바이트 순서(Big Endian)를 명시하고 있다. 이는 플랫폼 독립적인 바이트 순서를 보장한다.

### 8.5 헤더 파싱 — parser.recvMsg()

```go
// 소스: rpc_util.go:771-795
func (p *parser) recvMsg(maxReceiveMessageSize int) (payloadFormat, mem.BufferSlice, error) {
    err := p.r.ReadMessageHeader(p.header[:])
    if err != nil {
        return 0, nil, err
    }

    pf := payloadFormat(p.header[0])
    length := binary.BigEndian.Uint32(p.header[1:])

    if int64(length) > int64(maxInt) {
        return 0, nil, status.Errorf(codes.ResourceExhausted, ...)
    }
    if int(length) > maxReceiveMessageSize {
        return 0, nil, status.Errorf(codes.ResourceExhausted, ...)
    }

    data, err := p.r.Read(int(length))
    if err != nil {
        if err == io.EOF {
            err = io.ErrUnexpectedEOF
        }
        return 0, nil, err
    }
    return pf, data, nil
}
```

```
수신 파싱 흐름:

  1. ReadMessageHeader(5바이트)
       |
  2. header[0] → payloadFormat 추출
       |
  3. header[1:5] → Big Endian uint32 → 메시지 길이
       |
  4. 길이 검증:
       - 머신 최대값 초과? → ResourceExhausted
       - 수신 최대값 초과? → ResourceExhausted
       |
  5. Read(length) → 페이로드 데이터 읽기
       |
  6. EOF가 중간에 발생? → io.ErrUnexpectedEOF (불완전한 메시지)
```

---

## 9. 직렬화 흐름 (송신)

### 9.1 전체 흐름: prepareMsg()

`prepareMsg()`는 송신 시 메시지를 직렬화하고 압축하여 전송 준비를 완료하는 핵심 함수다.

```go
// 소스: stream.go:1886-1903
func prepareMsg(m any, codec baseCodec, cp Compressor, comp encoding.Compressor,
    pool mem.BufferPool) (hdr []byte, data, payload mem.BufferSlice, pf payloadFormat, err error) {
    if preparedMsg, ok := m.(*PreparedMsg); ok {
        return preparedMsg.hdr, preparedMsg.encodedData, preparedMsg.payload, preparedMsg.pf, nil
    }
    data, err = encode(codec, m)
    if err != nil {
        return nil, nil, nil, 0, err
    }
    compData, pf, err := compress(data, cp, comp, pool)
    if err != nil {
        data.Free()
        return nil, nil, nil, 0, err
    }
    hdr, payload = msgHeader(data, compData, pf)
    return hdr, data, payload, pf, nil
}
```

```
prepareMsg() 전체 흐름:

  m (사용자 메시지)
    |
    +--> PreparedMsg인가? --Yes--> 사전 준비된 데이터 반환 (캐시)
    |
    No
    |
    v
  encode(codec, m)
    |                   ← codec.Marshal(m) 호출
    v
  data (mem.BufferSlice, 직렬화된 원본 데이터)
    |
    v
  compress(data, cp, comp, pool)
    |                   ← compressor.Compress() 호출
    v
  compData (mem.BufferSlice, 압축된 데이터) + payloadFormat
    |
    v
  msgHeader(data, compData, pf)
    |                   ← 5바이트 헤더 생성
    v
  (hdr, data, payload, pf) 반환
```

### 9.2 encode() — 직렬화

```go
// 소스: rpc_util.go:800-813
func encode(c baseCodec, msg any) (mem.BufferSlice, error) {
    if msg == nil {
        return nil, nil
    }
    b, err := c.Marshal(msg)
    if err != nil {
        return nil, status.Errorf(codes.Internal, "grpc: error while marshaling: %v", err.Error())
    }
    if bufSize := uint(b.Len()); bufSize > math.MaxUint32 {
        b.Free()
        return nil, status.Errorf(codes.ResourceExhausted, "grpc: message too large (%d bytes)", bufSize)
    }
    return b, nil
}
```

직렬화 후 크기가 `math.MaxUint32`(약 4GB)를 초과하면 에러를 반환한다. 이는 메시지 프레임 헤더의 길이 필드가 4바이트(uint32)이기 때문이다.

### 9.3 compress() — 압축

```go
// 소스: rpc_util.go:820-855
func compress(in mem.BufferSlice, cp Compressor, compressor encoding.Compressor,
    pool mem.BufferPool) (mem.BufferSlice, payloadFormat, error) {
    if (compressor == nil && cp == nil) || in.Len() == 0 {
        return nil, compressionNone, nil
    }
    var out mem.BufferSlice
    w := mem.NewWriter(&out, pool)
    // ... 압축 수행 ...
    return out, compressionMade, nil
}
```

```
compress() 분기:

  compressor == nil && cp == nil?
  또는 in.Len() == 0?
     |
    Yes → compressionNone 반환 (압축 안 함)
     |
    No
     |
  compressor (encoding.Compressor) != nil?
   /         \
  Yes         No → cp (레거시 grpc.Compressor) 사용
   |                  |
  z = comp.Compress(w)  cp.Do(w, materialized_bytes)
   |                  |
  각 버퍼 청크를 z.Write()  전체 데이터를 한번에 처리
   |                  |
  z.Close()           |
   |                /
    v              v
  (out, compressionMade, nil) 반환
```

**레거시 vs 현재 압축기의 차이:** 현재 `encoding.Compressor`는 `BufferSlice`의 각 청크를 순차적으로 `Write()`하므로 전체 데이터를 메모리에 올릴 필요가 없다. 반면 레거시 `grpc.Compressor`는 `Do(w, []byte)`를 받으므로 `MaterializeToBuffer()`로 전체 데이터를 하나의 `[]byte`로 변환해야 한다.

### 9.4 SendMsg() 호출 흐름 (클라이언트 예시)

```go
// 소스: stream.go:938-997 (핵심 부분)
func (cs *clientStream) SendMsg(m any) (err error) {
    // ...
    hdr, data, payload, pf, err := prepareMsg(m, cs.codec, cs.compressorV0,
        cs.compressorV1, cs.cc.dopts.copts.BufferPool)
    if err != nil {
        return err
    }

    defer func() {
        data.Free()
        if pf.isCompressed() {
            payload.Free()
        }
    }()

    dataLen := data.Len()
    payloadLen := payload.Len()
    if payloadLen > *cs.callInfo.maxSendMessageSize {
        return status.Errorf(codes.ResourceExhausted, ...)
    }

    payload.Ref()  // retry를 위한 추가 참조
    op := func(a *csAttempt) error {
        return a.sendMsg(m, hdr, payload, dataLen, payloadLen)
    }
    // ...
}
```

**왜 payload.Ref()를 호출하는가?** 재시도 시 같은 payload를 다시 전송해야 할 수 있다. `Ref()`로 참조 카운트를 증가시켜 `defer`에서 `Free()`해도 실제 버퍼가 해제되지 않도록 한다.

---

## 10. 역직렬화 흐름 (수신)

### 10.1 recv() — 최상위 수신 함수

```go
// 소스: rpc_util.go:1023-1038
func recv(p *parser, c baseCodec, s recvCompressor, dc Decompressor, m any,
    maxReceiveMessageSize int, payInfo *payloadInfo, compressor encoding.Compressor,
    isServer bool) error {
    data, err := recvAndDecompress(p, s, dc, maxReceiveMessageSize, payInfo, compressor, isServer)
    if err != nil {
        return err
    }
    defer data.Free()

    if err := c.Unmarshal(data, m); err != nil {
        return status.Errorf(codes.Internal, "grpc: failed to unmarshal the received message: %v", err)
    }
    return nil
}
```

### 10.2 recvAndDecompress() — 수신 + 해제

```go
// 소스: rpc_util.go:930-962
func recvAndDecompress(p *parser, s recvCompressor, dc Decompressor,
    maxReceiveMessageSize int, payInfo *payloadInfo,
    compressor encoding.Compressor, isServer bool) (out mem.BufferSlice, err error) {
    pf, compressed, err := p.recvMsg(maxReceiveMessageSize)
    if err != nil {
        return nil, err
    }

    compressedLength := compressed.Len()

    if st := checkRecvPayload(pf, s.RecvCompress(), compressor != nil || dc != nil, isServer); st != nil {
        compressed.Free()
        return nil, st.Err()
    }

    if pf.isCompressed() {
        defer compressed.Free()
        out, err = decompress(compressor, compressed, dc, maxReceiveMessageSize, p.bufferPool)
        if err != nil {
            return nil, err
        }
    } else {
        out = compressed
    }
    // ...
    return out, nil
}
```

```
recvAndDecompress() 전체 흐름:

  parser.recvMsg()
    |
    v
  (payloadFormat, compressed data)
    |
    v
  checkRecvPayload()
    |                  ← 압축 플래그 검증
    v
  압축됨?
   /      \
  Yes      No
   |        |
  decompress()   compressed → out (그대로 반환)
   |
   v
  out (해제된 데이터)
```

### 10.3 checkRecvPayload() — 수신 페이로드 검증

```go
// 소스: rpc_util.go:894-911
func checkRecvPayload(pf payloadFormat, recvCompress string,
    haveCompressor bool, isServer bool) *status.Status {
    switch pf {
    case compressionNone:
        // 비압축 — 문제 없음
    case compressionMade:
        if recvCompress == "" || recvCompress == encoding.Identity {
            return status.New(codes.Internal,
                "grpc: compressed flag set with identity or empty encoding")
        }
        if !haveCompressor {
            if isServer {
                return status.Newf(codes.Unimplemented, ...)
            }
            return status.Newf(codes.Internal, ...)
        }
    default:
        return status.Newf(codes.Internal,
            "grpc: received unexpected payload format %d", pf)
    }
    return nil
}
```

**왜 서버와 클라이언트에서 에러 코드가 다른가?**
- 서버: `codes.Unimplemented` — "이 압축을 지원하지 않는다"
- 클라이언트: `codes.Internal` — "서버가 잘못된 압축을 보냈다" (클라이언트가 grpc-accept-encoding에 명시한 것만 서버가 보내야 하므로)

### 10.4 decompress() — 해제

```go
// 소스: rpc_util.go:971-1014
func decompress(compressor encoding.Compressor, d mem.BufferSlice, dc Decompressor,
    maxReceiveMessageSize int, pool mem.BufferPool) (mem.BufferSlice, error) {
    if dc != nil {
        // 레거시 Decompressor 사용
        r := d.Reader()
        uncompressed, err := dc.Do(r)
        // ... 크기 검증 ...
        return mem.BufferSlice{mem.SliceBuffer(uncompressed)}, nil
    }
    if compressor != nil {
        // 현재 encoding.Compressor 사용
        r := d.Reader()
        dcReader, err := compressor.Decompress(r)
        // ... 크기 제한 적용 ...
        if limit := int64(maxReceiveMessageSize); limit < math.MaxInt64 {
            dcReader = io.LimitReader(dcReader, limit+1)
        }
        out, err := mem.ReadAll(dcReader, pool)
        // ... 크기 검증 ...
        return out, nil
    }
    return nil, status.Errorf(codes.Internal, "grpc: no decompressor available")
}
```

**왜 `limit+1`을 사용하는가?** `io.LimitReader(dcReader, limit+1)`로 제한을 건 후, 실제로 읽힌 바이트가 `limit`을 초과하면 에러를 반환한다. `+1` 덕분에 "정확히 제한 크기"인 유효한 메시지를 거부하지 않으면서도, 제한을 초과하는 메시지를 잡아낼 수 있다.

---

## 11. Content-Type 협상

### 11.1 Content-Type 헤더

gRPC는 HTTP/2의 `content-type` 헤더를 통해 직렬화 포맷을 협상한다.

```
기본 Content-Type:    application/grpc
Proto 사용 시:        application/grpc+proto
JSON 사용 시:         application/grpc+json
커스텀 사용 시:       application/grpc+<content-subtype>
```

```go
// 소스: internal/grpcutil/method.go:46
const baseContentType = "application/grpc"

// 소스: internal/grpcutil/method.go:83-88
func ContentType(contentSubtype string) string {
    if contentSubtype == "" {
        return baseContentType
    }
    return baseContentType + "+" + contentSubtype
}
```

클라이언트가 HTTP/2 요청을 보낼 때 content-type을 설정한다:

```go
// 소스: internal/transport/http2_client.go:575
headerFields = append(headerFields, hpack.HeaderField{
    Name: "content-type",
    Value: grpcutil.ContentType(callHdr.ContentSubtype),
})
```

서버는 수신한 content-type에서 content-subtype을 추출한다:

```go
// 소스: internal/grpcutil/method.go:61-78
func ContentSubtype(contentType string) (string, bool) {
    if contentType == baseContentType {
        return "", true
    }
    if !strings.HasPrefix(contentType, baseContentType) {
        return "", false
    }
    switch contentType[len(baseContentType)] {
    case '+', ';':
        return contentType[len(baseContentType)+1:], true
    default:
        return "", false
    }
}
```

### 11.2 grpc-encoding 헤더

`grpc-encoding`은 메시지 본문에 적용된 압축 알고리즘을 나타낸다.

```
클라이언트 → 서버 (요청):
  grpc-encoding: gzip          ← 요청 메시지가 gzip으로 압축됨

서버 → 클라이언트 (응답):
  grpc-encoding: gzip          ← 응답 메시지가 gzip으로 압축됨
```

```go
// 소스: internal/transport/http2_client.go:582-583
if callHdr.SendCompress != "" {
    headerFields = append(headerFields, hpack.HeaderField{
        Name: "grpc-encoding", Value: callHdr.SendCompress,
    })
}
```

서버 측에서 수신한 grpc-encoding 값을 파싱:

```go
// 소스: internal/transport/http2_server.go:439-440
case "grpc-encoding":
    s.recvCompress = hf.Value
```

### 11.3 grpc-accept-encoding 헤더

클라이언트가 지원하는 압축 알고리즘 목록을 서버에 알린다.

```
클라이언트 → 서버:
  grpc-accept-encoding: gzip,snappy,zstd
                         ↑
                  등록된 모든 압축기 이름
```

```go
// 소스: internal/transport/http2_client.go:595-597
if registeredCompressors != "" {
    headerFields = append(headerFields, hpack.HeaderField{
        Name: "grpc-accept-encoding", Value: registeredCompressors,
    })
}
```

서버 측에서 이를 파싱하여 저장:

```go
// 소스: internal/transport/http2_server.go:429-438
case "grpc-accept-encoding":
    mdata[hf.Name] = append(mdata[hf.Name], hf.Value)
    if hf.Value == "" {
        continue
    }
    compressors := hf.Value
    if s.clientAdvertisedCompressors != "" {
        compressors = s.clientAdvertisedCompressors + "," + compressors
    }
    s.clientAdvertisedCompressors = compressors
```

### 11.4 서버의 응답 압축 선택

```go
// 소스: server.go:1355-1368
if s.opts.cp != nil {
    cp = s.opts.cp
    sendCompressorName = cp.Type()
} else if rc := stream.RecvCompress(); rc != "" && rc != encoding.Identity {
    // 클라이언트가 사용한 압축 알고리즘으로 응답도 압축
    comp = encoding.GetCompressor(rc)
    if comp != nil {
        sendCompressorName = comp.Name()
    }
}
```

**기본 동작:** 서버는 클라이언트의 요청 압축과 동일한 압축을 응답에 사용한다. 이는 "클라이언트가 해당 압축을 지원한다"는 암묵적 신호이기 때문이다.

서버 핸들러에서 응답 압축을 동적으로 변경할 수도 있다:

```go
// 소스: server.go:2127-2137
func SetSendCompressor(ctx context.Context, name string) error {
    stream, ok := ServerTransportStreamFromContext(ctx).(*transport.ServerStream)
    if !ok || stream == nil {
        return fmt.Errorf("failed to fetch the stream from the given context")
    }
    if err := validateSendCompressor(name, stream.ClientAdvertisedCompressors()); err != nil {
        return fmt.Errorf("unable to set send compressor: %w", err)
    }
    return stream.SetSendCompress(name)
}
```

### 11.5 코덱 선택 (서버)

```go
// 소스: server.go:2001-2014
func (s *Server) getCodec(contentSubtype string) baseCodec {
    if s.opts.codec != nil {
        return s.opts.codec          // 서버 옵션으로 강제 지정
    }
    if contentSubtype == "" {
        return getCodec(proto.Name)  // 기본: proto
    }
    codec := getCodec(contentSubtype)
    if codec == nil {
        logger.Warningf("Unsupported codec %q. Defaulting to %q ...", contentSubtype, proto.Name)
        return getCodec(proto.Name)  // 미등록 코덱 → proto 폴백
    }
    return codec
}
```

### 11.6 코덱 선택 (클라이언트)

```go
// 소스: rpc_util.go:1130-1158
func setCallInfoCodec(c *callInfo) error {
    if c.codec != nil {
        // ForceCodec/ForceCodecV2로 이미 설정된 경우
        if c.contentSubtype == "" {
            if ec, ok := c.codec.(encoding.CodecV2); ok {
                c.contentSubtype = strings.ToLower(ec.Name())
            }
        }
        return nil
    }
    if c.contentSubtype == "" {
        c.codec = getCodec(proto.Name)  // 기본: proto
        return nil
    }
    // CallContentSubtype로 지정된 경우
    c.codec = getCodec(c.contentSubtype)
    if c.codec == nil {
        return status.Errorf(codes.Internal, "no codec registered for content-subtype %s", c.contentSubtype)
    }
    return nil
}
```

```
코덱 선택 우선순위:

  1. ForceCodec / ForceCodecV2    ← CallOption으로 직접 지정
       |
  2. CallContentSubtype           ← content-subtype으로 레지스트리 조회
       |
  3. 기본값: "proto"              ← 아무것도 설정하지 않은 경우
```

---

## 12. V1 vs V2 Codec 차이와 브리지 패턴

### 12.1 브리지 패턴 개요

gRPC-Go는 V0(grpc.Codec), V1(encoding.Codec), V2(encoding.CodecV2) 세 가지 코덱 인터페이스를 모두 지원해야 한다. 이를 위해 **브리지(Bridge) 패턴**을 사용하여 구 버전 코덱을 새 버전 인터페이스로 래핑한다.

```
  grpc.Codec (V0)         encoding.Codec (V1)       encoding.CodecV2 (V2)
  +-----------+           +-----------+              +-----------+
  | Marshal   |           | Marshal   |              | Marshal   |
  | ([]byte)  |           | ([]byte)  |              | (BufferSlice)|
  | Unmarshal |           | Unmarshal |              | Unmarshal |
  | ([]byte)  |           | ([]byte)  |              | (BufferSlice)|
  | String()  |           | Name()    |              | Name()    |
  +-----------+           +-----------+              +-----------+
       |                       |                          |
       v                       v                          |
  codecV0Bridge           codecV1Bridge                   |
       |                  (includes V0Bridge)             |
       |                       |                          |
       +---------- baseCodec ---------+                   |
                   (Marshal/Unmarshal                     |
                    with BufferSlice)                     |
                        |                                 |
                        +------------ 동일 ---------------+
```

### 12.2 codecV0Bridge

```go
// 소스: codec.go:49-79
func newCodecV0Bridge(c Codec) baseCodec {
    return codecV0Bridge{codec: c}
}

type codecV0Bridge struct {
    codec interface {
        Marshal(v any) ([]byte, error)
        Unmarshal(data []byte, v any) error
    }
}

func (c codecV0Bridge) Marshal(v any) (mem.BufferSlice, error) {
    data, err := c.codec.Marshal(v)
    if err != nil {
        return nil, err
    }
    return mem.BufferSlice{mem.SliceBuffer(data)}, nil
}

func (c codecV0Bridge) Unmarshal(data mem.BufferSlice, v any) (err error) {
    return c.codec.Unmarshal(data.Materialize(), v)
}
```

V0/V1 코덱의 `[]byte` 입출력을 `mem.BufferSlice`로 래핑한다:
- `Marshal`: `[]byte` → `mem.SliceBuffer` → `mem.BufferSlice`
- `Unmarshal`: `mem.BufferSlice` → `Materialize()` → `[]byte`

`mem.SliceBuffer`는 풀링되지 않는 단순 바이트 슬라이스 래퍼로, `Free()`가 no-op이다. 이를 통해 V0/V1 코덱의 `[]byte`를 안전하게 `BufferSlice`에 넣을 수 있다.

### 12.3 codecV1Bridge

```go
// 소스: codec.go:81-90
type codecV1Bridge struct {
    codecV0Bridge        // V0 브리지를 포함 (Marshal/Unmarshal)
    name string          // V1의 Name() 값
}

func (c codecV1Bridge) Name() string {
    return c.name
}
```

V1 브리지는 V0 브리지를 임베딩하고 `Name()` 메서드만 추가한다. 이로써 `encoding.CodecV2` 인터페이스를 만족한다.

### 12.4 내부 getCodec() 조회 전략

```go
// 소스: codec.go:41-47
func getCodec(name string) encoding.CodecV2 {
    if codecV1 := encoding.GetCodec(name); codecV1 != nil {
        return newCodecV1Bridge(codecV1)
    }
    return encoding.GetCodecV2(name)
}
```

**왜 V1을 먼저 확인하는가?** 만약 사용자가 `encoding.RegisterCodec()`로 V1 코덱을 등록했다면, 이를 우선적으로 사용해야 한다. V2가 같은 이름으로 등록되어 있더라도, `registeredCodecs` 맵에서 V1은 `Codec` 타입이므로 `GetCodecV2()`의 타입 단언 `.(CodecV2)`에 실패한다. 따라서 V1을 먼저 체크해야 올바른 코덱을 사용할 수 있다.

단, `RegisterCodecV2()`로 등록한 경우 같은 키에 `CodecV2` 타입이 저장되므로, `GetCodec()` (V1 조회)의 `.(Codec)` 단언이 실패하고, `GetCodecV2()`에서 성공한다.

---

## 13. 커스텀 코덱 및 압축기 작성 방법

### 13.1 커스텀 코덱 (V2 방식, 권장)

```go
package jsoncodec

import (
    "encoding/json"
    "google.golang.org/grpc/encoding"
    "google.golang.org/grpc/mem"
)

func init() {
    encoding.RegisterCodecV2(&jsonCodecV2{})
}

type jsonCodecV2 struct{}

func (c *jsonCodecV2) Marshal(v any) (mem.BufferSlice, error) {
    data, err := json.Marshal(v)
    if err != nil {
        return nil, err
    }
    return mem.BufferSlice{mem.SliceBuffer(data)}, nil
}

func (c *jsonCodecV2) Unmarshal(data mem.BufferSlice, v any) error {
    return json.Unmarshal(data.Materialize(), v)
}

func (c *jsonCodecV2) Name() string {
    return "json"
}
```

사용법:
```go
import _ "path/to/jsoncodec"  // init()으로 자동 등록

// 방법 1: CallContentSubtype
conn.Invoke(ctx, method, req, resp, grpc.CallContentSubtype("json"))

// 방법 2: ForceCodecV2
conn.Invoke(ctx, method, req, resp, grpc.ForceCodecV2(&jsonCodecV2{}))
```

### 13.2 커스텀 코덱 (V1 방식)

```go
package jsoncodec

import (
    "encoding/json"
    "google.golang.org/grpc/encoding"
)

func init() {
    encoding.RegisterCodec(&jsonCodec{})
}

type jsonCodec struct{}

func (c *jsonCodec) Marshal(v any) ([]byte, error) {
    return json.Marshal(v)
}

func (c *jsonCodec) Unmarshal(data []byte, v any) error {
    return json.Unmarshal(data, v)
}

func (c *jsonCodec) Name() string {
    return "json"
}
```

### 13.3 커스텀 압축기

```go
package snappy

import (
    "io"
    "sync"

    snappylib "github.com/golang/snappy"
    "google.golang.org/grpc/encoding"
)

func init() {
    encoding.RegisterCompressor(&compressor{})
}

type compressor struct {
    writerPool sync.Pool
}

type snappyWriter struct {
    *snappylib.Writer
    pool *sync.Pool
}

func (c *compressor) Compress(w io.Writer) (io.WriteCloser, error) {
    sw, ok := c.writerPool.Get().(*snappyWriter)
    if !ok {
        sw = &snappyWriter{
            Writer: snappylib.NewBufferedWriter(w),
            pool:   &c.writerPool,
        }
    } else {
        sw.Reset(w)
    }
    return sw, nil
}

func (sw *snappyWriter) Close() error {
    defer sw.pool.Put(sw)
    return sw.Writer.Close()
}

func (c *compressor) Decompress(r io.Reader) (io.Reader, error) {
    return snappylib.NewReader(r), nil
}

func (c *compressor) Name() string {
    return "snappy"
}
```

사용법:
```go
import _ "path/to/snappy"  // init()으로 자동 등록

// 클라이언트 측: 송신 압축 지정
conn.Invoke(ctx, method, req, resp, grpc.UseCompressor("snappy"))
```

### 13.4 커스텀 코덱/압축기 작성 시 주의사항

| 항목 | 설명 |
|------|------|
| Thread safety | `Marshal`/`Unmarshal`은 여러 고루틴에서 동시 호출됨 |
| Name() 불변성 | 호출마다 동일한 문자열을 반환해야 함 |
| 등록 타이밍 | 반드시 `init()` 함수에서 등록 |
| 에러 처리 | nil 반환, 빈 Name() → panic |
| 메모리 관리 (V2) | `Marshal` 반환값의 Buffer는 최소 참조 카운트 1을 가져야 함 |
| Unmarshal 데이터 수명 (V2) | `Unmarshal`에 전달된 `data`는 함수 반환 즉시 Free됨. 데이터를 보존하려면 자체 Ref() 필요 |

---

## 14. 성능 최적화 전략

### 14.1 sync.Pool 활용 정리

| 풀링 대상 | 위치 | 효과 |
|-----------|------|------|
| gzip.Writer | `encoding/gzip/gzip.go:42` | 압축기 초기화 비용 회피 |
| gzip.Reader | `encoding/gzip/gzip.go:89` | 해제기 초기화 비용 회피 |
| protobuf 직렬화 버퍼 | `encoding/proto/proto.go:73-79` | 대용량 메시지 GC 부담 감소 |
| buffer 객체 | `mem/buffers.go:64` | Buffer 구조체 자체의 할당 최적화 |

### 14.2 mem.BufferSlice의 이점

```
기존 []byte 방식:
  Marshal → 새 []byte 할당 → 네트워크 전송 → GC 회수
                ↓
          매번 새로운 할당 + GC 부담

BufferSlice 방식:
  Marshal → Pool에서 버퍼 획득 → 네트워크 전송 → Free() → Pool로 반환
                                                    ↓
                                              재사용 가능
```

### 14.3 UseCachedSize 최적화

protobuf 직렬화에서 크기 계산은 전체 메시지 트리를 순회해야 하므로 비용이 높다. `proto.Size()`를 먼저 호출하여 크기를 캐시한 뒤, `MarshalAppend()`에서 재활용하면 순회를 한 번 줄일 수 있다.

### 14.4 PreparedMsg

```go
// 소스: stream.go:1887-1888
if preparedMsg, ok := m.(*PreparedMsg); ok {
    return preparedMsg.hdr, preparedMsg.encodedData, preparedMsg.payload, preparedMsg.pf, nil
}
```

`PreparedMsg`는 동일한 메시지를 여러 번 전송할 때 직렬화와 압축을 한 번만 수행하고 결과를 캐시한다. 스트리밍 RPC에서 동일한 메시지를 반복 전송할 때 유용하다.

### 14.5 크기 기반 풀링 임계값

```go
// 소스: mem/buffers.go:62
var bufferPoolingThreshold = 1 << 10  // 1024 bytes
```

1KB 이하의 버퍼는 풀링하지 않고 일반 할당을 사용한다. 이는 작은 버퍼의 풀링 오버헤드(Get/Put 호출, 원자적 연산)가 할당 비용보다 클 수 있기 때문이다.

---

## 15. 정리

### 인코딩/압축 서브시스템의 설계 원칙

| 원칙 | 구현 방식 |
|------|-----------|
| 플러그인 확장성 | Codec/Compressor 인터페이스 + 전역 레지스트리 |
| 하위 호환성 | V0 → V1 → V2 브리지 패턴 |
| 제로 카피 | mem.BufferSlice + 참조 카운팅 |
| 메모리 효율 | sync.Pool + 크기 기반 풀링 분기 |
| 프로토콜 준수 | 5바이트 프레이밍, Content-Type/grpc-encoding 헤더 |
| 자동 협상 | 서버가 클라이언트 압축에 맞춰 응답 |
| 안전한 등록 | init() 시점에만 등록, 이후 읽기 전용 |

### 주요 파일 요약

```
encoding/
├── encoding.go          ← Codec(V1), Compressor, 레지스트리
├── encoding_v2.go       ← CodecV2 (mem.BufferSlice 기반)
├── internal/
│   └── internal.go      ← 테스트용 압축기 등록/해제
├── proto/
│   └── proto.go         ← protobuf 코덱 (기본 코덱, CodecV2 구현)
└── gzip/
    └── gzip.go          ← gzip 압축기 (sync.Pool 활용)

codec.go                 ← 레거시 Codec(V0), baseCodec, 브리지 패턴
rpc_util.go              ← encode, compress, decompress, recv, prepareMsg
stream.go                ← SendMsg, RecvMsg에서 파이프라인 호출
server.go                ← 서버 측 코덱/압축기 선택 로직
internal/grpcutil/
├── method.go            ← Content-Type 파싱/생성
└── compressor.go        ← 등록된 압축기 이름 관리
internal/transport/
├── http2_client.go      ← 클라이언트 HTTP/2 헤더 설정
└── http2_server.go      ← 서버 HTTP/2 헤더 파싱
```

### 데이터 흐름 종합 다이어그램

```
[사용자 코드]
     |
     v
  SendMsg(m)
     |
     v
  prepareMsg(m, codec, cp, comp, pool)
     |
     +--→ encode(codec, m)
     |        |
     |        v
     |    codec.Marshal(m) → data (mem.BufferSlice)
     |
     +--→ compress(data, cp, comp, pool)
     |        |
     |        v
     |    compressor.Compress(w) → z.Write(data) → compData
     |
     +--→ msgHeader(data, compData, pf)
              |
              v
          hdr[0] = pf (0 or 1)
          hdr[1:5] = BigEndian(len(payload))
     |
     v
  Write(hdr, payload) → HTTP/2 DATA 프레임
     |
     v
  ===================== 네트워크 =====================
     |
     v
  ReadMessageHeader(5 bytes) → parser.recvMsg()
     |
     v
  recvAndDecompress()
     |
     +--→ checkRecvPayload(pf, recvCompress, ...)
     |
     +--→ decompress(compressor, compressed, dc, maxSize, pool)
     |        |
     |        v
     |    compressor.Decompress(r) → io.Reader → mem.ReadAll()
     |
     v
  recv()
     |
     v
  codec.Unmarshal(data, m)
     |
     v
  [사용자 메시지 복원]
```
