# containerd Diff Service Deep-Dive

## 1. 개요

containerd의 Diff Service는 컨테이너 파일시스템 레이어 간의 **차이(diff)**를 계산하고 적용하는 서브시스템이다.
이 서비스는 두 가지 핵심 기능을 제공한다:

1. **Compare (비교)**: 두 마운트 세트 간의 파일시스템 차이를 OCI 레이어 tar로 생성
2. **Apply (적용)**: OCI 레이어 tar를 마운트된 파일시스템 위에 적용

### 1.1 왜 Diff Service가 필요한가

| 유즈케이스 | 사용하는 기능 |
|-----------|-------------|
| 이미지 Pull → rootfs 생성 | Apply (레이어 tar → 스냅샷) |
| 컨테이너 변경 사항 커밋 | Compare (lower vs upper → new layer) |
| 체크포인트의 RW 레이어 저장 | Compare (원본 vs 변경된 rootfs) |
| 이미지 빌드 | Compare (각 빌드 단계 간 diff) |

### 1.2 소스 위치

```
containerd/
├── core/diff/
│   ├── diff.go              # Comparer, Applier 인터페이스 (163줄)
│   ├── stream.go            # StreamProcessor, Handler (192줄)
│   ├── stream_unix.go       # BinaryProcessor (Unix) (165줄)
│   ├── stream_windows.go    # BinaryProcessor (Windows)
│   ├── apply/
│   │   └── apply.go         # fsApplier 구현 (182줄)
│   └── proxy/
│       └── differ.go        # gRPC 프록시
├── plugins/diff/
│   ├── walking/
│   │   ├── differ.go        # WalkingDiff (Compare 구현, 216줄)
│   │   └── plugin/plugin.go
│   ├── erofs/               # EROFS 포맷 diff
│   └── windows/             # Windows diff
└── pkg/archive/
    └── changes.go           # 파일시스템 차이 계산 (WriteDiff)
```

## 2. 아키텍처

### 2.1 핵심 인터페이스

```
┌─────────────────────────────────────────────────────┐
│                  Diff Service                        │
│                                                     │
│  ┌────────────────┐      ┌────────────────────┐     │
│  │   Comparer      │      │     Applier         │     │
│  │                │      │                    │     │
│  │  Compare(       │      │  Apply(             │     │
│  │    lower,       │      │    desc,            │     │
│  │    upper,       │      │    mounts,          │     │
│  │    opts...      │      │    opts...          │     │
│  │  ) Descriptor   │      │  ) Descriptor       │     │
│  └────────┬───────┘      └────────┬───────────┘     │
│           │                       │                  │
│           ▼                       ▼                  │
│  ┌────────────────┐      ┌────────────────────┐     │
│  │  WalkingDiff    │      │   fsApplier         │     │
│  │  (plugins/diff/ │      │   (core/diff/       │     │
│  │   walking/)     │      │    apply/)          │     │
│  └────────────────┘      └────────────────────┘     │
└─────────────────────────────────────────────────────┘
```

`core/diff/diff.go`에서 정의된 인터페이스:

```go
// Comparer는 두 마운트 세트 간의 차이를 계산한다
type Comparer interface {
    Compare(ctx context.Context, lower, upper []mount.Mount, opts ...Opt) (ocispec.Descriptor, error)
}

// Applier는 diff를 마운트 위에 적용한다
type Applier interface {
    Apply(ctx context.Context, desc ocispec.Descriptor, mount []mount.Mount, opts ...ApplyOpt) (ocispec.Descriptor, error)
}
```

### 2.2 Config 구조

```go
type Config struct {
    MediaType       string                                              // diff 출력 포맷
    Reference       string                                              // 콘텐츠 업로드 참조
    Labels          map[string]string                                   // 콘텐츠 라벨
    Compressor      func(dest io.Writer, mediaType string) (io.WriteCloser, error) // 커스텀 압축
    SourceDateEpoch *time.Time                                         // 재현 가능 빌드용 타임스탬프
}
```

### 2.3 ApplyConfig 구조

```go
type ApplyConfig struct {
    ProcessorPayloads map[string]typeurl.Any   // 스트림 프로세서 페이로드 (예: 복호화 키)
    SyncFs            bool                      // 파일시스템 sync 호출 여부
    Progress          func(int64)               // 진행률 콜백
}
```

## 3. Compare 동작 원리 (WalkingDiff)

### 3.1 WalkingDiff 알고리즘

`plugins/diff/walking/differ.go`의 `WalkingDiff`는 가장 범용적인 Compare 구현이다.
어떤 파일시스템이든 동작하며, 두 디렉토리를 동시에 순회하며 차이를 계산한다.

```
Compare(lower, upper) 흐름:

1. lower 마운트 → 임시 디렉토리에 마운트
2. upper 마운트 → 임시 디렉토리에 마운트 (읽기 전용)
3. Content Store Writer 생성
4. 압축 스트림 설정 (gzip/zstd/none)
5. archive.WriteDiff(lower_root, upper_root) → tar 생성
6. Content Store에 커밋
7. Descriptor 반환

         lower mount          upper mount
             │                     │
             ▼                     ▼
        /tmp/lower            /tmp/upper
             │                     │
             └─────────┬──────────┘
                       │
                       ▼
              archive.WriteDiff()
                       │
                       ▼
              ┌────────────────┐
              │ Content Writer  │
              │  (gzip 압축)    │
              └────────┬───────┘
                       │
                       ▼
              Content Store 커밋
                       │
                       ▼
              OCI Descriptor 반환
```

### 3.2 지원하는 압축 포맷

```go
switch config.MediaType {
case ocispec.MediaTypeImageLayer:
    compressionType = compression.Uncompressed
case ocispec.MediaTypeImageLayerGzip:
    compressionType = compression.Gzip          // 기본값
case ocispec.MediaTypeImageLayerZstd:
    compressionType = compression.Zstd
default:
    // 커스텀 Compressor 필요
}
```

### 3.3 재현 가능 빌드 (SOURCE_DATE_EPOCH)

`WithSourceDateEpoch` 옵션으로 타임스탬프를 고정할 수 있다:

```go
func WithSourceDateEpoch(tm *time.Time) Opt {
    return func(c *Config) error {
        c.SourceDateEpoch = tm
        return nil
    }
}
```

이를 통해 같은 파일시스템 변경에 대해 항상 동일한 diff 결과를 생성하여
재현 가능한 이미지 빌드를 지원한다.

## 4. Apply 동작 원리 (fsApplier)

### 4.1 fsApplier 흐름

`core/diff/apply/apply.go`의 `fsApplier`는 콘텐츠 스토어에서 레이어를 읽어
마운트된 파일시스템에 적용한다.

```
Apply(desc, mounts) 흐름:

1. Content Store에서 blob ReaderAt 획득
2. StreamProcessor 체인 구성:
   ┌──────────────┐   ┌──────────────────┐   ┌────────────────┐
   │ processorChain │→│ compressedProcessor│→│ stdProcessor    │
   │ (원본 스트림)   │   │ (gzip 해제)       │   │ (최종 tar)     │
   └──────────────┘   └──────────────────┘   └────────────────┘
3. digest 검증용 TeeReader 설정
4. 마운트에 tar 추출
5. 후행 데이터 읽기
6. DiffID (uncompressed digest) 반환
```

핵심 코드 (`core/diff/apply/apply.go:50-125`):

```go
func (s *fsApplier) Apply(ctx context.Context, desc ocispec.Descriptor,
    mounts []mount.Mount, opts ...diff.ApplyOpt) (d ocispec.Descriptor, err error) {

    // Content Store에서 blob 읽기
    ra, err := s.store.ReaderAt(ctx, desc)
    var r io.ReadCloser
    if config.Progress != nil {
        r = newProgressReader(ra, config.Progress)
    } else {
        r = newReadCloser(ra)
    }

    // StreamProcessor 체인 구성
    processor := diff.NewProcessorChain(desc.MediaType, r)
    for {
        processor, err = diff.GetProcessor(ctx, processor, config.ProcessorPayloads)
        if processor.MediaType() == ocispec.MediaTypeImageLayer {
            break  // 최종 tar 스트림 도달
        }
    }

    // 무결성 검증 + tar 추출
    digester := digest.Canonical.Digester()
    rc := &readCounter{r: io.TeeReader(processor, digester.Hash())}
    apply(ctx, mounts, rc, config.SyncFs)

    return ocispec.Descriptor{
        MediaType: ocispec.MediaTypeImageLayer,
        Size:      rc.c,
        Digest:    digester.Digest(),  // DiffID
    }, nil
}
```

### 4.2 Progress 추적

`progressReader`는 읽기 작업마다 콜백을 호출하여 진행률을 보고한다:

```go
type progressReader struct {
    rc *readCounter
    c  io.Closer
    p  func(int64)
}

func (pr *progressReader) Read(p []byte) (n int, err error) {
    pr.p(pr.rc.c)    // 현재까지 읽은 바이트 수 보고
    n, err = pr.rc.Read(p)
    return
}
```

## 5. StreamProcessor 체인

### 5.1 설계 원리

StreamProcessor는 콘텐츠 스트림을 변환하는 파이프라인을 구현한다.
이 설계를 통해 암호화, 압축, 커스텀 포맷 등 다양한 변환을 체인으로 연결할 수 있다.

```go
type StreamProcessor interface {
    io.ReadCloser
    MediaType() string
}
```

### 5.2 기본 프로세서 체인

```
입력 스트림 (gzip+tar)
       │
       ▼
processorChain
  MediaType: "application/vnd.oci.image.layer.v1.tar+gzip"
       │
       ▼
compressedProcessor (gzip 해제)
  MediaType: "application/vnd.oci.image.layer.v1.tar"
       │
       ▼
최종 tar 스트림 (Apply에서 추출)
```

### 5.3 Handler 등록 메커니즘

```go
// stream.go의 init()에서 기본 압축 핸들러 등록
func init() {
    RegisterProcessor(compressedHandler)
}

// GetProcessor는 등록된 핸들러를 역순으로 탐색 (사용자 핸들러 우선)
func GetProcessor(ctx context.Context, stream StreamProcessor,
    payloads map[string]typeurl.Any) (StreamProcessor, error) {
    for i := len(handlers) - 1; i >= 0; i-- {
        processor, ok := handlers[i](ctx, stream.MediaType())
        if ok {
            return processor(ctx, stream, payloads)
        }
    }
    return nil, ErrNoProcessor
}
```

### 5.4 BinaryProcessor (외부 프로세서)

`core/diff/stream_unix.go`의 `BinaryProcessor`는 외부 바이너리를 사용하여
스트림을 처리한다. 예를 들어 암호화된 레이어를 복호화하는 데 사용된다:

```go
func BinaryHandler(id, returnsMediaType string, mediaTypes []string,
    path string, args, env []string) Handler {
    // 지정된 mediaType에 대해 외부 바이너리를 실행하는 핸들러 생성
}
```

동작 방식:
1. 외부 바이너리를 `exec.CommandContext`로 시작
2. 입력 스트림을 stdin에 연결
3. stdout을 출력 스트림으로 사용
4. 페이로드는 ExtraFiles(fd 3)로 전달
5. 환경변수 `STREAM_PROCESSOR_MEDIATYPE`으로 입력 미디어 타입 전달

```
┌─────────────┐  stdin   ┌────────────────┐  stdout  ┌─────────────┐
│ 입력 스트림    │────────→│ 외부 바이너리      │────────→│ 출력 스트림    │
│ (암호화 tar)  │         │ (복호화 처리)     │         │ (평문 tar)   │
└─────────────┘  fd=3    │                │         └─────────────┘
                 ────────→│ (페이로드/키)    │
                          └────────────────┘
```

## 6. Diff 옵션 시스템

### 6.1 Compare 옵션 (Opt)

| 옵션 | 용도 |
|------|------|
| `WithMediaType(m)` | diff 출력 포맷 지정 (gzip/zstd/none) |
| `WithReference(ref)` | 콘텐츠 업로드 참조 지정 (추적용) |
| `WithLabels(labels)` | 콘텐츠 라벨 설정 |
| `WithCompressor(f)` | 커스텀 압축 함수 사용 |
| `WithSourceDateEpoch(tm)` | 재현 가능 빌드용 타임스탬프 |

### 6.2 Apply 옵션 (ApplyOpt)

| 옵션 | 용도 |
|------|------|
| `WithPayloads(p)` | 프로세서 페이로드 (예: 복호화 키) |
| `WithSyncFs(sync)` | 적용 후 fsync 호출 여부 |
| `WithProgress(f)` | 진행률 콜백 설정 |

## 7. Walking Diff의 세부 구현

### 7.1 에러 처리 패턴

WalkingDiff는 `errOpen` 변수로 Content Writer가 열려 있는 동안의 에러를 추적한다:

```go
var errOpen error
defer func() {
    if errOpen != nil {
        cw.Close()
        if newReference {
            s.store.Abort(ctx, config.Reference)
        }
    }
}()
```

이 패턴으로 에러 시 자동으로 미완성 콘텐츠를 정리한다.

### 7.2 Uncompressed Label 설정

```go
config.Labels[labels.LabelUncompressed] = dgstr.Digest().String()
```

압축된 diff와 비압축 diff의 관계를 라벨로 기록하여,
이후 Apply 시 DiffID 검증에 사용한다.

### 7.3 uniqueRef() 함수

```go
func uniqueRef() string {
    t := time.Now()
    var b [3]byte
    rand.Read(b[:])
    return fmt.Sprintf("%d-%s", t.UnixNano(), base64.URLEncoding.EncodeToString(b[:]))
}
```

Content Writer 참조에 고유성을 부여하여 동시 Compare 작업 시 충돌을 방지한다.

## 8. 플러그인 구조

### 8.1 Walking Diff 플러그인

```go
// plugins/diff/walking/plugin/plugin.go
registry.Register(&plugin.Registration{
    Type: plugins.DiffPlugin,
    ID:   "walking",
    Requires: []plugin.Type{
        plugins.MetadataPlugin,
    },
    InitFn: func(ic *plugin.InitContext) (interface{}, error) {
        md, err := ic.GetSingle(plugins.MetadataPlugin)
        db := md.(*metadata.DB)
        // Content Store + Applier 결합
        return walking.NewWalkingDiff(db.ContentStore()), nil
    },
})
```

### 8.2 EROFS Diff 플러그인

EROFS(Enhanced Read-Only File System)는 읽기 전용 파일시스템으로,
이미지 레이어를 EROFS 포맷으로 저장하는 특수 diff 플러그인이다.
`plugins/diff/erofs/`에 구현되어 있다.

## 9. Diff Service의 사용처

### 9.1 이미지 언팩 파이프라인

```
Unpacker.unpack()
  └─ Applier.Apply(ctx, layerDesc, mounts)
       └─ fsApplier.Apply()
            └─ StreamProcessor 체인 → tar 추출
```

### 9.2 컨테이너 체크포인트

```
WithCheckpointRW()
  └─ rootfs.CreateDiff()
       └─ Comparer.Compare(lower, upper)
            └─ WalkingDiff.Compare()
                 └─ archive.WriteDiff() → tar 생성
```

### 9.3 컨테이너 복원

```
WithRestoreRW()
  └─ DiffService().Apply(ctx, rw, mounts)
       └─ fsApplier.Apply()
```

## 10. 성능 고려사항

### 10.1 메모리 효율성

`fsApplier`는 콘텐츠 스토어에서 `ReaderAt`을 사용하여 필요한 부분만 읽는다.
전체 blob을 메모리에 로드하지 않고 스트리밍 방식으로 처리한다.

### 10.2 I/O 최적화

`readCounter`로 읽은 바이트 수를 추적하면서, `io.TeeReader`로 동시에
해시를 계산한다. 데이터를 한 번만 읽어 두 가지 작업을 수행한다:

```go
digester := digest.Canonical.Digester()
rc := &readCounter{
    r: io.TeeReader(processor, digester.Hash()),
}
```

### 10.3 SyncFs 옵션

```go
type ApplyConfig struct {
    SyncFs bool  // true이면 Apply 후 fsync 호출
}
```

- `SyncFs=false` (기본값): 성능 우선, OS 캐시에 의존
- `SyncFs=true`: 데이터 안전 보장, 성능 저하

## 11. 설계 결정의 이유

### 11.1 왜 Comparer와 Applier를 분리하는가?

- **Comparer**는 파일시스템 순회 로직에 의존 (Walking, EROFS 등)
- **Applier**는 tar 추출 로직에 의존 (OS별 다름)
- 두 인터페이스를 분리하여 독립적으로 교체/확장 가능

### 11.2 왜 StreamProcessor 체인을 사용하는가?

직접적인 if-else 분기 대신 체인 패턴을 사용하면:
- 새로운 미디어 타입/변환을 플러그인으로 추가 가능
- 암호화 + 압축 같은 다단계 변환을 자연스럽게 표현
- 외부 바이너리를 통한 확장 지원

### 11.3 왜 역순 탐색하는가?

```go
for i := len(handlers) - 1; i >= 0; i-- {
    processor, ok := handlers[i](ctx, stream.MediaType())
    // ...
}
```

나중에 등록된 핸들러가 우선 적용된다.
기본 핸들러(`init()`에서 등록)보다 사용자 핸들러가 우선하도록 하기 위함이다.

## 12. 정리

| 구성요소 | 역할 | 소스 위치 |
|---------|------|----------|
| Comparer | 두 파일시스템 간 diff 계산 | `core/diff/diff.go` |
| Applier | diff를 파일시스템에 적용 | `core/diff/diff.go` |
| WalkingDiff | 범용 Compare 구현체 | `plugins/diff/walking/` |
| fsApplier | 범용 Apply 구현체 | `core/diff/apply/` |
| StreamProcessor | 스트림 변환 체인 | `core/diff/stream.go` |
| BinaryProcessor | 외부 프로세서 연동 | `core/diff/stream_unix.go` |
| Config/ApplyConfig | 동작 설정 | `core/diff/diff.go` |

containerd의 Diff Service는 인터페이스 분리, 스트림 프로세서 체인, 플러그인 아키텍처를 통해
다양한 이미지 포맷과 변환을 유연하게 지원하면서도, 핵심 로직은 단순하게 유지한다.

---

*소스 참조: `core/diff/diff.go`, `core/diff/stream.go`, `core/diff/apply/apply.go`, `plugins/diff/walking/differ.go`*
*containerd 버전: v2.0*
