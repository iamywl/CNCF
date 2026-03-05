# 11. 이미지 관리 시스템 Deep-Dive

## 목차

1. [개요](#1-개요)
2. [OCI 이미지 스펙 구조](#2-oci-이미지-스펙-구조)
3. [Image 데이터 모델](#3-image-데이터-모델)
4. [Image Store 인터페이스](#4-image-store-인터페이스)
5. [미디어 타입 시스템](#5-미디어-타입-시스템)
6. [핸들러와 Walk 패턴](#6-핸들러와-walk-패턴)
7. [Manifest 해석과 Platform 필터링](#7-manifest-해석과-platform-필터링)
8. [Image Unpack 흐름](#8-image-unpack-흐름)
9. [GC 연계와 라벨 시스템](#9-gc-연계와-라벨-시스템)
10. [Docker-OCI 호환성](#10-docker-oci-호환성)
11. [클라이언트 Image 인터페이스](#11-클라이언트-image-인터페이스)
12. [설계 철학과 핵심 교훈](#12-설계-철학과-핵심-교훈)

---

## 1. 개요

containerd에서 "이미지"는 컨테이너를 실행하기 위해 필요한 파일시스템 레이어, 설정, 메타데이터의
집합이다. 하지만 containerd 내부에서 이미지는 단순한 파일 묶음이 아니라 **Content-Addressable
Storage(CAS) 위에 구축된 Merkle DAG**로 표현된다. 이미지의 모든 구성 요소(Index, Manifest,
Config, Layer)는 Content Store에 독립적인 blob으로 저장되고, Descriptor(digest + mediaType +
size)를 통해 참조 관계가 형성된다.

```
                          Image Store
                    (이름 → Descriptor 매핑)
                              │
                              ▼
                    ┌──────────────────┐
                    │   Image Record   │
                    │  Name: "nginx"   │
                    │  Target: desc    │──── Descriptor {digest, mediaType, size}
                    │  Labels: {...}   │
                    │  CreatedAt       │
                    └──────────────────┘
                              │
                              ▼
                       Content Store
                    (digest → blob 매핑)
                              │
                   ┌──────────┴──────────┐
                   ▼                     ▼
            ┌──────────┐          ┌──────────┐
            │  Index   │          │ Manifest │  (단일 플랫폼)
            │(multi-   │          │          │
            │ platform)│          │          │
            └─────┬────┘          └────┬─────┘
                  │                    │
          ┌───────┼───────┐      ┌─────┼─────┐
          ▼       ▼       ▼      ▼           ▼
       Manifest Manifest ...   Config     Layers
       (amd64) (arm64)         (JSON)   (tar.gz...)
```

### 왜 이 구조인가?

1. **Content-Addressable**: 동일 내용은 한 번만 저장 → 디스크 효율
2. **Merkle DAG**: 어떤 노드든 수정되면 root digest가 바뀜 → 무결성 보장
3. **이름과 내용의 분리**: Image Store는 이름→digest 매핑만, 실제 데이터는 Content Store에
4. **플랫폼 독립**: Index를 통해 하나의 이미지 이름으로 여러 아키텍처 지원

### 핵심 소스 파일

| 파일 | 역할 |
|------|------|
| `core/images/image.go` | Image struct, Store 인터페이스, Manifest/Config/RootFS/Children 함수 |
| `core/images/mediatypes.go` | 미디어 타입 상수와 분류 함수 |
| `core/images/handlers.go` | Handler 인터페이스, Walk/Dispatch, 필터링 핸들러 |
| `core/images/annotations.go` | containerd 전용 어노테이션 상수 |
| `client/image.go` | 클라이언트 측 Image 인터페이스와 Unpack 구현 |

---

## 2. OCI 이미지 스펙 구조

containerd는 OCI Image Specification을 기본 이미지 포맷으로 사용한다.
OCI 이미지는 4개의 핵심 구성 요소로 이루어진다.

### 2.1 Descriptor (기본 참조 단위)

OCI 이미지의 모든 참조는 Descriptor를 통해 이루어진다.

```
┌──────────────────────────────────────────────┐
│              ocispec.Descriptor              │
├──────────────┬───────────────────────────────┤
│ MediaType    │ "application/vnd.oci.image.." │  ← 내용의 타입
│ Digest       │ "sha256:abc123..."            │  ← 내용의 해시
│ Size         │ 1234                          │  ← 바이트 크기
│ Platform     │ {OS: "linux", Arch: "amd64"}  │  ← (선택) 플랫폼
│ Annotations  │ {"key": "value"}              │  ← (선택) 메타데이터
└──────────────┴───────────────────────────────┘
```

**왜 Descriptor인가?** 단순한 URL이나 경로 대신 content-addressable한 참조를 사용함으로써:
- 데이터 무결성을 digest로 검증 가능
- 전송 전에 크기를 알 수 있어 저장소 할당 가능
- MediaType으로 파서를 결정할 수 있음

### 2.2 Index (멀티 플랫폼 이미지)

```json
{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.index.v1+json",
  "manifests": [
    {
      "mediaType": "application/vnd.oci.image.manifest.v1+json",
      "digest": "sha256:aaa...",
      "size": 528,
      "platform": { "architecture": "amd64", "os": "linux" }
    },
    {
      "mediaType": "application/vnd.oci.image.manifest.v1+json",
      "digest": "sha256:bbb...",
      "size": 528,
      "platform": { "architecture": "arm64", "os": "linux" }
    }
  ]
}
```

### 2.3 Manifest (단일 플랫폼 이미지)

```json
{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.manifest.v1+json",
  "config": {
    "mediaType": "application/vnd.oci.image.config.v1+json",
    "digest": "sha256:ccc...",
    "size": 1024
  },
  "layers": [
    {
      "mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
      "digest": "sha256:ddd...",
      "size": 32654
    },
    {
      "mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
      "digest": "sha256:eee...",
      "size": 16724
    }
  ]
}
```

### 2.4 Config (이미지 설정)

Config에는 컨테이너 실행에 필요한 메타데이터와 RootFS의 DiffID 목록이 포함된다.

```json
{
  "architecture": "amd64",
  "os": "linux",
  "config": {
    "Env": ["PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin"],
    "Cmd": ["/bin/sh"],
    "WorkingDir": "/"
  },
  "rootfs": {
    "type": "layers",
    "diff_ids": [
      "sha256:fff...",
      "sha256:ggg..."
    ]
  }
}
```

### OCI 이미지의 Merkle DAG 구조

```
Index (sha256:idx)
  │
  ├── Manifest[amd64] (sha256:mfst-amd64)
  │     ├── Config (sha256:cfg-amd64)
  │     │     └── rootfs.diff_ids: [sha256:ddd-uncompressed, sha256:eee-uncompressed]
  │     ├── Layer 0 (sha256:ddd-compressed)
  │     └── Layer 1 (sha256:eee-compressed)
  │
  └── Manifest[arm64] (sha256:mfst-arm64)
        ├── Config (sha256:cfg-arm64)
        ├── Layer 0 (sha256:fff-compressed)
        └── Layer 1 (sha256:ggg-compressed)
```

**핵심 구분**: Layer의 digest는 *압축된* 상태의 해시이고, Config의 diff_id는 *압축 해제된* 상태의 해시이다.
이 이중 해시 구조는 전송 효율(압축)과 내용 무결성(비압축 해시) 모두를 보장한다.

---

## 3. Image 데이터 모델

### 3.1 Image 구조체

`core/images/image.go`에 정의된 Image 구조체:

```go
// 소스: core/images/image.go

type Image struct {
    Name      string                 // 이미지 이름 (참조 호환)
    Labels    map[string]string      // 런타임 레이블
    Target    ocispec.Descriptor     // 루트 콘텐츠 디스크립터
    CreatedAt, UpdatedAt time.Time   // 타임스탬프
}
```

Image 구조체에서 주목할 점:

| 필드 | 설명 | 왜 이렇게 설계했나 |
|------|------|-------------------|
| `Name` | `docker.io/library/nginx:latest` 형태 | 레지스트리 resolver와 호환 |
| `Labels` | 사용자 정의 메타데이터 | 정적 메타데이터 장식용, 런타임 동작에 영향 없음 |
| `Target` | Descriptor (digest+mediaType+size) | 실제 데이터는 Content Store에 분리 저장 |
| `CreatedAt` | 이미지 레코드 생성 시간 | OCI 이미지의 생성 시간과 다름 (메타데이터 수준) |

**Target이 핵심이다.** Image 레코드는 이름과 Target Descriptor만 가지며, 실제 이미지 데이터
(Manifest, Config, Layer)는 모두 Content Store에 별도 저장된다. 이 분리 덕분에:

- 같은 이미지 데이터를 다른 이름으로 참조 가능 (tag)
- 이미지 삭제 시 실제 blob 삭제 여부는 GC가 결정
- Content Store와 Image Store가 독립적으로 진화 가능

### 3.2 DeleteOptions

```go
// 소스: core/images/image.go

type DeleteOptions struct {
    Synchronous bool                // 동기 삭제 (GC 완료까지 대기)
    Target      *ocispec.Descriptor // 기대되는 Target 값 (CAS 보호)
}
```

`DeleteTarget` 옵션은 이미지의 현재 Target이 기대한 값과 다르면 삭제를 거부한다.
이는 **낙관적 동시성 제어(optimistic concurrency control)**의 일종으로,
다른 클라이언트가 이미지를 업데이트한 경우 실수로 삭제하는 것을 방지한다.

---

## 4. Image Store 인터페이스

### 4.1 Store 인터페이스

```go
// 소스: core/images/image.go

type Store interface {
    Get(ctx context.Context, name string) (Image, error)
    List(ctx context.Context, filters ...string) ([]Image, error)
    Create(ctx context.Context, image Image) (Image, error)
    Update(ctx context.Context, image Image, fieldpaths ...string) (Image, error)
    Delete(ctx context.Context, name string, opts ...DeleteOpt) error
}
```

### 4.2 CRUD 패턴 분석

```
클라이언트                  Image Store              Content Store
    │                           │                         │
    │  Create(image)            │                         │
    ├──────────────────────────►│                         │
    │   Name + Target 저장      │                         │
    │   (Content는 이미 존재)    │                         │
    │◄──────────────────────────┤                         │
    │                           │                         │
    │  Get("nginx:latest")      │                         │
    ├──────────────────────────►│                         │
    │  Image{Target: desc}      │                         │
    │◄──────────────────────────┤                         │
    │                           │                         │
    │  desc로 Content 읽기      │                         │
    ├─────────────────────────────────────────────────────►│
    │  Manifest/Config blob     │                         │
    │◄─────────────────────────────────────────────────────┤
```

**왜 Image Store에 Content가 없는가?**

이것이 containerd 이미지 관리의 핵심 설계 결정이다:

1. **관심사의 분리**: Image Store는 "이름 → 무엇" 매핑, Content Store는 "무엇 → 데이터"
2. **중복 제거**: 여러 이미지가 동일 레이어를 공유 가능
3. **GC 친화적**: 참조 관계를 라벨로 추적, 사용하지 않는 blob만 GC가 정리

### 4.3 Update와 fieldpaths

```go
Update(ctx context.Context, image Image, fieldpaths ...string) (Image, error)
```

`fieldpaths`는 부분 업데이트를 지원한다. 예를 들어:

- `Update(image, "labels")`: 레이블만 업데이트
- `Update(image)`: 전체 필드 업데이트

이 설계는 동시 접근 시 다른 필드를 실수로 덮어쓰는 것을 방지한다.

---

## 5. 미디어 타입 시스템

### 5.1 미디어 타입 상수 정의

`core/images/mediatypes.go`에 정의된 미디어 타입 상수들:

```go
// 소스: core/images/mediatypes.go

// Docker 미디어 타입
const (
    MediaTypeDockerSchema2Layer            = "application/vnd.docker.image.rootfs.diff.tar"
    MediaTypeDockerSchema2LayerGzip        = "application/vnd.docker.image.rootfs.diff.tar.gzip"
    MediaTypeDockerSchema2LayerZstd        = "application/vnd.docker.image.rootfs.diff.tar.zstd"
    MediaTypeDockerSchema2LayerForeign     = "application/vnd.docker.image.rootfs.foreign.diff.tar"
    MediaTypeDockerSchema2LayerForeignGzip = "application/vnd.docker.image.rootfs.foreign.diff.tar.gzip"
    MediaTypeDockerSchema2Config           = "application/vnd.docker.container.image.v1+json"
    MediaTypeDockerSchema2Manifest         = "application/vnd.docker.distribution.manifest.v2+json"
    MediaTypeDockerSchema2ManifestList     = "application/vnd.docker.distribution.manifest.list.v2+json"
)

// Checkpoint/Restore 미디어 타입
const (
    MediaTypeContainerd1Checkpoint        = "application/vnd.containerd.container.criu.checkpoint.criu.tar"
    MediaTypeContainerd1CheckpointConfig  = "application/vnd.containerd.container.checkpoint.config.v1+proto"
    // ...기타 체크포인트 타입들
)

// 암호화 미디어 타입
const (
    MediaTypeImageLayerEncrypted     = ocispec.MediaTypeImageLayer + "+encrypted"
    MediaTypeImageLayerGzipEncrypted = ocispec.MediaTypeImageLayerGzip + "+encrypted"
)

// EROFS 미디어 타입
const MediaTypeErofsLayer = "application/vnd.erofs.layer.v1"

// In-toto 증명
const MediaTypeInToto = "application/vnd.in-toto+json"
```

### 5.2 미디어 타입 분류 체계

containerd는 미디어 타입을 역할 기반으로 분류하는 함수들을 제공한다:

```
미디어 타입 분류 트리
├── IsManifestType()       ← Manifest 또는 Docker Manifest v2
├── IsIndexType()          ← OCI Index 또는 Docker ManifestList
├── IsLayerType()          ← 레이어 (OCI/Docker/EROFS)
├── IsConfigType()         ← 이미지 설정 (OCI/Docker)
├── IsKnownConfig()        ← 설정 + 체크포인트 설정
├── IsAttestationType()    ← In-toto 증명
├── IsDockerType()         ← Docker 네임스페이스 프리픽스
└── IsNonDistributable()   ← 재배포 불가 레이어
```

### 5.3 IsManifestType / IsIndexType의 구현

```go
// 소스: core/images/mediatypes.go

func IsManifestType(mt string) bool {
    switch mt {
    case MediaTypeDockerSchema2Manifest, ocispec.MediaTypeImageManifest:
        return true
    default:
        return false
    }
}

func IsIndexType(mt string) bool {
    switch mt {
    case ocispec.MediaTypeImageIndex, MediaTypeDockerSchema2ManifestList:
        return true
    default:
        return false
    }
}
```

**왜 switch 문인가?** 정해진 미디어 타입 집합만 허용함으로써:
- Schema 1은 의도적으로 제외 (IsManifestType에서 지원하지 않음)
- 알 수 없는 타입의 잘못된 처리 방지
- Docker와 OCI 양쪽 타입을 동일하게 취급

### 5.4 IsLayerType의 확장 로직

```go
// 소스: core/images/mediatypes.go

func IsLayerType(mt string) bool {
    if strings.HasPrefix(mt, "application/vnd.oci.image.layer.") {
        return true
    }
    switch base, _ := parseMediaTypes(mt); base {
    case MediaTypeDockerSchema2Layer, MediaTypeDockerSchema2LayerGzip,
        MediaTypeDockerSchema2LayerForeign, MediaTypeDockerSchema2LayerForeignGzip,
        MediaTypeDockerSchema2LayerZstd:
        return true
    case MediaTypeErofsLayer:
        return true
    }
    return false
}
```

`parseMediaTypes`는 미디어 타입을 base와 suffix로 분리한다:

```
"application/vnd.oci.image.layer.v1.tar+gzip+encrypted"
                                         │
                           parseMediaTypes()
                                         │
                       ┌─────────────────┴─────────────────┐
                       ▼                                   ▼
base: "application/vnd.oci.image.layer.v1.tar"    suffixes: ["encrypted", "gzip"]
```

이 파싱 덕분에 `+encrypted`, `+gzip` 같은 래핑 suffix를 제거하고 base 타입으로 분류할 수 있다.

### 5.5 DiffCompression 함수

```go
// 소스: core/images/mediatypes.go

func DiffCompression(ctx context.Context, mediaType string) (string, error) {
    base, ext := parseMediaTypes(mediaType)
    switch base {
    case MediaTypeDockerSchema2Layer, MediaTypeDockerSchema2LayerForeign:
        if len(ext) > 0 { return "", nil }  // 래핑됨
        return "unknown", nil               // 압축 여부 불명
    case MediaTypeDockerSchema2LayerGzip, MediaTypeDockerSchema2LayerForeignGzip:
        if len(ext) > 0 { return "", nil }
        return "gzip", nil
    case MediaTypeDockerSchema2LayerZstd:
        if len(ext) > 0 { return "", nil }
        return "zstd", nil
    case ocispec.MediaTypeImageLayer, ocispec.MediaTypeImageLayerNonDistributable:
        if len(ext) > 0 {
            switch ext[len(ext)-1] {
            case "gzip": return "gzip", nil
            case "zstd": return "zstd", nil
            }
        }
        return "", nil  // 비압축
    default:
        return "", fmt.Errorf("unrecognised mediatype %s: %w", mediaType, errdefs.ErrNotImplemented)
    }
}
```

Docker 미디어 타입의 "unknown" 반환값이 흥미롭다. Docker schema2에서 비압축 레이어 미디어 타입이
실제로는 gzip 압축된 경우가 있어, 런타임에서 실제 압축 방식을 감지해야 한다.

---

## 6. 핸들러와 Walk 패턴

### 6.1 Handler 인터페이스

containerd의 이미지 처리는 **Handler 체인 패턴**을 사용한다:

```go
// 소스: core/images/handlers.go

type Handler interface {
    Handle(ctx context.Context, desc ocispec.Descriptor) (subdescs []ocispec.Descriptor, err error)
}

type HandlerFunc func(ctx context.Context, desc ocispec.Descriptor) (subdescs []ocispec.Descriptor, err error)
```

Handler는 Descriptor를 입력으로 받아:
1. 해당 Descriptor에 대한 처리를 수행
2. 하위 Descriptor(children) 목록을 반환

반환된 children은 재귀적으로 처리된다. 이것이 이미지 Merkle DAG 순회의 기반이다.

### 6.2 Walk (동기 순회)

```go
// 소스: core/images/handlers.go

func Walk(ctx context.Context, handler Handler, descs ...ocispec.Descriptor) error {
    for _, desc := range descs {
        children, err := handler.Handle(ctx, desc)
        if err != nil {
            if errors.Is(err, ErrSkipDesc) {
                continue // 이 노드와 하위 건너뛰기
            }
            return err
        }
        if len(children) > 0 {
            if err := Walk(ctx, handler, children...); err != nil {
                return err
            }
        }
    }
    return nil
}
```

Walk는 **DFS(깊이 우선 탐색)**으로 이미지 트리를 순회한다:

```
Walk 실행 흐름 (Index → Manifest → Config + Layers)

Walk(handler, Index)
  │
  ├─ handler.Handle(Index)
  │    └─ children: [Manifest-amd64]  (플랫폼 필터 후)
  │
  └─ Walk(handler, Manifest-amd64)
       │
       ├─ handler.Handle(Manifest-amd64)
       │    └─ children: [Config, Layer0, Layer1]
       │
       └─ Walk(handler, Config, Layer0, Layer1)
            ├─ handler.Handle(Config)   → children: []
            ├─ handler.Handle(Layer0)   → children: []
            └─ handler.Handle(Layer1)   → children: []
```

### 6.3 Dispatch (병렬 순회)

```go
// 소스: core/images/handlers.go

func Dispatch(ctx context.Context, handler Handler, limiter *semaphore.Weighted, descs ...ocispec.Descriptor) error {
    eg, ctx2 := errgroup.WithContext(ctx)
    for _, desc := range descs {
        if limiter != nil {
            if err := limiter.Acquire(ctx, 1); err != nil {
                return err
            }
        }
        eg.Go(func() error {
            children, err := handler.Handle(ctx2, desc)
            if limiter != nil {
                limiter.Release(1)
            }
            if err != nil {
                if errors.Is(err, ErrSkipDesc) {
                    return nil
                }
                return err
            }
            if len(children) > 0 {
                return Dispatch(ctx2, handler, limiter, children...)
            }
            return nil
        })
    }
    return eg.Wait()
}
```

**Walk vs Dispatch**:

| 항목 | Walk | Dispatch |
|------|------|----------|
| 순회 방식 | 동기/순차 | 비동기/병렬 |
| 사용 시점 | 크기 계산, Manifest 해석 | 이미지 Fetch, 레이어 다운로드 |
| 에러 전파 | 즉시 중단 | errgroup으로 수집 |
| 동시성 제어 | 불필요 | semaphore.Weighted |

### 6.4 제어 에러

```go
// 소스: core/images/handlers.go

var (
    ErrSkipDesc    = errors.New("skip descriptor")
    ErrStopHandler = errors.New("stop handler")
    ErrEmptyWalk   = errors.New("image might be filtered out")
)
```

- `ErrSkipDesc`: 해당 Descriptor와 모든 하위 노드 건너뛰기
- `ErrStopHandler`: 핸들러 체인에서 이후 핸들러 실행 중단 (형제는 계속)
- `ErrEmptyWalk`: WalkNotEmpty에서 children이 모두 필터링된 경우

### 6.5 Handlers (체인 핸들러)

```go
// 소스: core/images/handlers.go

func Handlers(handlers ...Handler) HandlerFunc {
    return func(ctx context.Context, desc ocispec.Descriptor) (subdescs []ocispec.Descriptor, err error) {
        var children []ocispec.Descriptor
        for _, handler := range handlers {
            ch, err := handler.Handle(ctx, desc)
            if err != nil {
                if errors.Is(err, ErrStopHandler) {
                    break
                }
                return nil, err
            }
            children = append(children, ch...)
        }
        return children, nil
    }
}
```

여러 핸들러를 순서대로 실행하고 children을 합산한다. 이는 **미들웨어 패턴**과 유사하다.

### 6.6 ChildrenHandler

```go
// 소스: core/images/handlers.go

func ChildrenHandler(provider content.Provider) HandlerFunc {
    return func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
        return Children(ctx, provider, desc)
    }
}
```

ChildrenHandler는 Descriptor의 MediaType에 따라 children을 해석한다:

```go
// 소스: core/images/image.go

func Children(ctx context.Context, provider content.Provider, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
    if IsManifestType(desc.MediaType) {
        // Manifest → [Config] + Layers
        var manifest ocispec.Manifest
        // ... unmarshal
        return append([]ocispec.Descriptor{manifest.Config}, manifest.Layers...), nil
    } else if IsIndexType(desc.MediaType) {
        // Index → Manifests
        var index ocispec.Index
        // ... unmarshal
        return append([]ocispec.Descriptor{}, index.Manifests...), nil
    }
    // 기타 타입 (Config, Layer 등)은 leaf 노드 → children 없음
    return nil, nil
}
```

---

## 7. Manifest 해석과 Platform 필터링

### 7.1 Manifest 함수의 전체 흐름

`Manifest()` 함수는 이미지 Descriptor에서 특정 플랫폼의 Manifest를 찾아 반환한다:

```go
// 소스: core/images/image.go

func Manifest(ctx context.Context, provider content.Provider, image ocispec.Descriptor,
    platform platforms.MatchComparer) (ocispec.Manifest, error) {
    // ...
}
```

```
Manifest() 해석 흐름

Input: Image Descriptor + Platform Matcher
                │
                ▼
    ┌───────────────────────┐
    │ Walk(HandlerFunc, desc)│
    └───────────┬───────────┘
                │
       desc.MediaType?
                │
    ┌───────────┴───────────┐
    │                       │
    ▼                       ▼
IsManifestType          IsIndexType
    │                       │
    ▼                       ▼
1. ReadBlob             1. ReadBlob
2. validateMediaType    2. validateMediaType
3. Unmarshal Manifest   3. Unmarshal Index
4. 플랫폼 매치 확인     4. 플랫폼으로 필터링
5. m에 추가             5. 정렬 + limit 적용
                        6. children으로 반환 (재귀)
                │
                ▼
    결과: m[0].m (가장 적합한 Manifest)
```

### 7.2 플랫폼 매치 전략

Index에서 Manifest를 선택할 때:

```go
// 소스: core/images/image.go (Manifest 함수 내부)

// Index의 Manifests에서 플랫폼 필터링
var descs []ocispec.Descriptor
for _, d := range idx.Manifests {
    if d.Platform == nil || platform.Match(*d.Platform) {
        descs = append(descs, d)
    }
}

// 가장 적합한 플랫폼 순으로 정렬
sort.SliceStable(descs, func(i, j int) bool {
    if descs[i].Platform == nil { return false }
    if descs[j].Platform == nil { return true }
    return platform.Less(*descs[i].Platform, *descs[j].Platform)
})
```

정렬 후 `limit`(기본값 1)만큼 잘라서 처리한다. `platform.Less()`는 플랫폼 매치 정확도를 비교한다.

### 7.3 Manifest에서 플랫폼 정보가 없는 경우

```go
// 소스: core/images/image.go (Manifest 함수 내부)

if desc.Platform == nil {
    imagePlatform, err := ConfigPlatform(ctx, provider, manifest.Config)
    if err != nil {
        return nil, err
    }
    if !platform.Match(imagePlatform) {
        return nil, nil  // 플랫폼 불일치
    }
}
```

Descriptor에 Platform 정보가 없으면 Config를 읽어서 플랫폼을 확인한다:

```go
// 소스: core/images/image.go

func ConfigPlatform(ctx context.Context, provider content.Provider,
    configDesc ocispec.Descriptor) (ocispec.Platform, error) {
    p, err := content.ReadBlob(ctx, provider, configDesc)
    // ...
    var imagePlatform ocispec.Platform
    json.Unmarshal(p, &imagePlatform)
    return platforms.Normalize(imagePlatform), nil
}
```

### 7.4 validateMediaType

```go
// 소스: core/images/image.go

func validateMediaType(b []byte, mt string) error {
    var doc unknownDocument
    json.Unmarshal(b, &doc)
    if len(doc.FSLayers) != 0 {
        return fmt.Errorf("media-type: schema 1 not supported")
    }
    if IsManifestType(mt) && (len(doc.Manifests) != 0 || IsIndexType(doc.MediaType)) {
        return fmt.Errorf("media-type: expected manifest but found index (%s)", mt)
    } else if IsIndexType(mt) && (len(doc.Config) != 0 || len(doc.Layers) != 0 || IsManifestType(doc.MediaType)) {
        return fmt.Errorf("media-type: expected index but found manifest (%s)", mt)
    }
    return nil
}
```

이 함수의 역할:
1. **Schema 1 거부**: `FSLayers` 필드가 있으면 Docker v1 스키마 → 지원 안 함
2. **타입 불일치 감지**: Descriptor의 MediaType과 실제 내용이 다른 경우 감지
   - Manifest라고 했는데 Manifests 필드가 있으면 → 실제로는 Index
   - Index라고 했는데 Config/Layers가 있으면 → 실제로는 Manifest

### 7.5 FilterPlatforms와 LimitManifests

```go
// 소스: core/images/handlers.go

func FilterPlatforms(f HandlerFunc, m platforms.Matcher) HandlerFunc {
    return func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
        children, err := f(ctx, desc)
        // ...
        var descs []ocispec.Descriptor
        for _, d := range children {
            if d.Platform == nil || m.Match(*d.Platform) {
                descs = append(descs, d)
            }
        }
        return descs, nil
    }
}

func LimitManifests(f HandlerFunc, m platforms.MatchComparer, n int) HandlerFunc {
    return func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
        children, err := f(ctx, desc)
        // ...
        if IsIndexType(desc.MediaType) {
            sort.SliceStable(children, func(i, j int) bool {
                // platform.Less로 정렬
                return m.Less(*children[i].Platform, *children[j].Platform)
            })
            if n > 0 && len(children) > n {
                children = children[:n]
            }
        }
        return children, nil
    }
}
```

이 두 핸들러 래퍼는 조합되어 사용된다:

```go
// Image.Size()에서의 사용 예
// 소스: core/images/image.go

func (image *Image) Size(ctx context.Context, provider content.Provider,
    platform platforms.MatchComparer) (int64, error) {
    var size int64
    return size, Walk(ctx, Handlers(
        HandlerFunc(func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
            size += desc.Size
            return nil, nil
        }),
        LimitManifests(FilterPlatforms(ChildrenHandler(provider), platform), platform, 1),
    ), image.Target)
}
```

핸들러 체인 구조:

```
Handlers([
    크기 누산 핸들러,          ← 모든 Descriptor의 Size 합산
    LimitManifests(            ← Index에서 최대 1개 Manifest만
        FilterPlatforms(       ← 현재 플랫폼에 맞는 것만
            ChildrenHandler()  ← Descriptor → children 해석
        )
    )
])
```

---

## 8. Image Unpack 흐름

### 8.1 Unpack 개요

이미지 Unpack은 Content Store의 레이어 blob을 Snapshot으로 풀어서 컨테이너가 사용할 수 있는
파일시스템을 준비하는 과정이다.

```
Content Store                    Snapshot Store
(압축된 레이어 blob)              (풀린 파일시스템)

Layer 0 (tar.gz) ──Unpack──► Snapshot 0 (Committed)
                                    │
Layer 1 (tar.gz) ──Unpack──► Snapshot 1 (Committed, parent=0)
                                    │
Layer 2 (tar.gz) ──Unpack──► Snapshot 2 (Committed, parent=1)
                                    │
                              ChainID = identity(diff_id[0], diff_id[1], diff_id[2])
```

### 8.2 클라이언트 Unpack 구현

```go
// 소스: client/image.go

func (i *image) Unpack(ctx context.Context, snapshotterName string, opts ...UnpackOpt) error {
    ctx, done, err := i.client.WithLease(ctx)  // ← Lease 획득 (GC 보호)
    defer done(ctx)

    // 1. 설정 파싱
    var config UnpackConfig
    for _, o := range opts { o(ctx, &config) }

    // 2. Manifest 가져오기
    manifest, err := i.getManifest(ctx, i.platform)

    // 3. 레이어 정보 구성
    layers, err := i.getLayers(ctx, manifest)

    // 4. Snapshotter와 DiffService 준비
    a := i.client.DiffService()
    cs := i.client.ContentStore()
    sn, err := i.client.getSnapshotter(ctx, snapshotterName)

    // 5. 플랫폼 지원 검증 (선택)
    if config.CheckPlatformSupported {
        i.checkSnapshotterSupport(ctx, snapshotterName, manifest)
    }

    // 6. 레이어별 Apply
    var chain []digest.Digest
    for _, layer := range layers {
        unpacked, err = rootfs.ApplyLayerWithOpts(ctx, layer, chain, sn, a,
            config.SnapshotOpts, config.ApplyOpts)

        if unpacked {
            // 비압축 digest 라벨 설정
            cinfo := content.Info{
                Digest: layer.Blob.Digest,
                Labels: map[string]string{
                    labels.LabelUncompressed: layer.Diff.Digest.String(),
                },
            }
            cs.Update(ctx, cinfo, "labels."+labels.LabelUncompressed)
        }
        chain = append(chain, layer.Diff.Digest)
    }

    // 7. GC 참조 라벨 설정
    rootFS := identity.ChainID(chain).String()
    cinfo := content.Info{
        Digest: desc.Digest,
        Labels: map[string]string{
            fmt.Sprintf("containerd.io/gc.ref.snapshot.%s", snapshotterName): rootFS,
        },
    }
    cs.Update(ctx, cinfo, ...)
}
```

### 8.3 단계별 상세 분석

#### (1) Lease 획득

```go
ctx, done, err := i.client.WithLease(ctx)
defer done(ctx)
```

Unpack 도중 GC가 레이어나 스냅샷을 삭제하지 못하도록 Lease를 설정한다.
Lease는 Content와 Snapshot 리소스에 대한 임시 참조를 생성한다.

#### (2) getLayers - 레이어 매핑

```go
// 소스: client/image.go

func (i *image) getLayers(ctx context.Context, manifest ocispec.Manifest) ([]rootfs.Layer, error) {
    diffIDs, err := i.RootFS(ctx)  // Config에서 diff_ids 가져오기

    // OCI artifact 레이어 제외, 이미지 레이어만 추출
    imageLayers := []ocispec.Descriptor{}
    for _, ociLayer := range manifest.Layers {
        if images.IsLayerType(ociLayer.MediaType) {
            imageLayers = append(imageLayers, ociLayer)
        }
    }

    if len(diffIDs) != len(imageLayers) {
        return nil, errors.New("mismatched image rootfs and manifest layers")
    }

    layers := make([]rootfs.Layer, len(diffIDs))
    for i := range diffIDs {
        layers[i].Diff = ocispec.Descriptor{
            MediaType: ocispec.MediaTypeImageLayer,
            Digest:    diffIDs[i],               // 비압축 digest
        }
        layers[i].Blob = imageLayers[i]          // 압축된 레이어
    }
    return layers, nil
}
```

각 Layer는 두 가지 Descriptor를 가진다:
- `Diff`: 비압축 상태의 digest (Config의 diff_id에서 옴) → ChainID 계산에 사용
- `Blob`: 압축 상태의 Descriptor (Manifest의 layers에서 옴) → Content Store에서 읽기에 사용

#### (3) ApplyLayerWithOpts

각 레이어를 순서대로 Snapshot에 Apply:

```
Layer 0 Apply:
  Snapshot.Prepare("extracting-0", "") → Active snapshot
  DiffService.Apply(layer0.Blob, mount) → 레이어 내용 적용
  Snapshot.Commit(chainID(diff0), "extracting-0") → Committed

Layer 1 Apply:
  Snapshot.Prepare("extracting-1", chainID(diff0)) → Active (parent=committed-0)
  DiffService.Apply(layer1.Blob, mount) → 레이어 내용 적용
  Snapshot.Commit(chainID(diff0, diff1), "extracting-1") → Committed
```

#### (4) GC 참조 라벨 설정

```go
// Config에 스냅샷 참조 라벨 추가
cinfo := content.Info{
    Digest: desc.Digest,  // Config의 digest
    Labels: map[string]string{
        "containerd.io/gc.ref.snapshot.overlayfs": rootFS,
    },
}
```

이 라벨은 Config blob → 최종 스냅샷의 참조 관계를 기록한다.
GC는 이 라벨을 따라 스냅샷이 여전히 사용 중인지 판단한다.

### 8.4 UnpackConfig 옵션

```go
// 소스: client/image.go

type UnpackConfig struct {
    ApplyOpts              []diff.ApplyOpt       // diff apply 옵션
    SnapshotOpts           []snapshots.Opt       // 스냅샷 생성 옵션
    CheckPlatformSupported bool                  // 스냅샷터 플랫폼 지원 검증
    DuplicationSuppressor  kmutex.KeyedLocker    // 중복 unpack 방지
    Limiter                *semaphore.Weighted   // 동시 unpack 제한
}
```

`DuplicationSuppressor`는 동일 이미지의 동시 unpack 요청을 직렬화하여 불필요한 중복 작업을 방지한다.

---

## 9. GC 연계와 라벨 시스템

### 9.1 GC 라벨의 역할

containerd의 GC는 Content Store와 Snapshot Store에서 참조되지 않는 리소스를 정리한다.
이미지 관리에서 GC 라벨은 **참조 그래프의 엣지**를 표현한다.

```
Image Record ──(name→digest)──► Index blob
                                   │
                          gc.ref.content.m.0
                                   │
                                   ▼
                              Manifest blob
                              ┌────┴────┐
                    gc.ref.    │         │   gc.ref.
                    content.   │         │   content.
                    config     ▼         ▼   l.0, l.1
                          Config blob  Layer blobs
                              │
                    gc.ref.snapshot.overlayfs
                              │
                              ▼
                         Snapshot (ChainID)
```

### 9.2 ChildGCLabels 함수

```go
// 소스: core/images/mediatypes.go

func ChildGCLabels(desc ocispec.Descriptor) []string {
    // Subject 참조 (OCI Referrer)
    if _, ok := desc.Annotations[AnnotationManifestSubject]; ok {
        return []string{"containerd.io/gc.ref.content.referrer.sha256."}
    }
    mt := desc.MediaType
    if IsKnownConfig(mt) {
        return []string{"containerd.io/gc.ref.content.config"}
    }
    switch mt {
    case MediaTypeDockerSchema2Manifest, ocispec.MediaTypeImageManifest:
        return []string{"containerd.io/gc.ref.content.m."}
    }
    if IsLayerType(mt) {
        return []string{"containerd.io/gc.ref.content.l."}
    }
    return []string{"containerd.io/gc.ref.content."}
}
```

라벨 키 패턴:

| 미디어 타입 | GC 라벨 키 | 설명 |
|------------|-----------|------|
| Config | `gc.ref.content.config` | 고유 (인덱스 없음) |
| Manifest | `gc.ref.content.m.0`, `.1`, ... | 인덱스 자동 부여 |
| Layer | `gc.ref.content.l.0`, `.1`, ... | 인덱스 자동 부여 |
| Referrer | `gc.ref.content.referrer.sha256.<hex12>` | digest 기반 키 |
| 기타 | `gc.ref.content.0`, ... | 범용 |

### 9.3 SetChildrenMappedLabels

```go
// 소스: core/images/handlers.go

func SetChildrenMappedLabels(manager content.Manager, f HandlerFunc,
    labelMap func(ocispec.Descriptor) []string) HandlerFunc {
    return func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
        children, err := f(ctx, desc)
        if len(children) > 0 {
            info := content.Info{
                Digest: desc.Digest,
                Labels: map[string]string{},
            }
            keys := map[string]uint{}
            for _, ch := range children {
                for _, key := range labelMap(ch) {
                    idx := keys[key]
                    keys[key] = idx + 1
                    if strings.HasSuffix(key, ".sha256.") {
                        key = fmt.Sprintf("%s%s", key, ch.Digest.Hex()[:12])
                    } else if idx > 0 || key[len(key)-1] == '.' {
                        key = fmt.Sprintf("%s%d", key, idx)
                    }
                    info.Labels[key] = ch.Digest.String()
                    fields = append(fields, "labels."+key)
                }
            }
            manager.Update(ctx, info, fields...)
        }
        return children, err
    }
}
```

이 핸들러는 Walk/Dispatch 중에 부모 blob에 children을 가리키는 GC 라벨을 자동 설정한다.

### 9.4 ChildGCLabelsFilterLayers

```go
// 소스: core/images/mediatypes.go

func ChildGCLabelsFilterLayers(desc ocispec.Descriptor) []string {
    if IsLayerType(desc.MediaType) {
        return nil  // 레이어는 라벨 설정 안 함
    }
    return ChildGCLabels(desc)
}
```

레이어를 제외하는 변형이 있는 이유: 특정 시나리오에서 레이어 blob은 GC 라벨 없이도
별도의 참조 관리 메커니즘으로 보호될 수 있기 때문이다.

### 9.5 SetReferrers

```go
// 소스: core/images/handlers.go

func SetReferrers(refProvider content.ReferrersProvider, f HandlerFunc) HandlerFunc {
    return func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
        children, err := f(ctx, desc)
        if !IsManifestType(desc.MediaType) && !IsIndexType(desc.MediaType) {
            return children, nil
        }
        refs, err := refProvider.Referrers(ctx, desc)
        for _, ref := range refs {
            ref.Annotations[AnnotationManifestSubject] = desc.Digest.String()
            children = append(children, ref)
        }
        return children, nil
    }
}
```

OCI Referrer API 지원: Manifest/Index에 대한 referrer(서명, SBOM 등)를 children에 추가한다.
어노테이션 `io.containerd.manifest.subject`로 subject 관계를 표시한다.

---

## 10. Docker-OCI 호환성

### 10.1 Docker와 OCI 미디어 타입 매핑

containerd는 Docker 이미지와 OCI 이미지를 동일한 코드 경로로 처리한다.
이것이 가능한 이유는 분류 함수들이 양쪽 미디어 타입을 동등하게 취급하기 때문이다:

```
Docker Media Type                           OCI Media Type
─────────────────                           ──────────────
application/vnd.docker.                     application/vnd.oci.
distribution.manifest.v2+json              image.manifest.v1+json
          │                                         │
          └──────────── IsManifestType() ────────────┘
                        → true (둘 다)

application/vnd.docker.                     application/vnd.oci.
distribution.manifest.list.v2+json         image.index.v1+json
          │                                         │
          └──────────── IsIndexType() ──────────────┘
                        → true (둘 다)

application/vnd.docker.                     application/vnd.oci.
container.image.v1+json                    image.config.v1+json
          │                                         │
          └──────────── IsConfigType() ─────────────┘
                        → true (둘 다)
```

### 10.2 레이어 미디어 타입의 호환성

Docker와 OCI 레이어 타입은 구조가 다르지만 `IsLayerType()`이 모두 처리:

```
Docker:
  application/vnd.docker.image.rootfs.diff.tar           (비압축)
  application/vnd.docker.image.rootfs.diff.tar.gzip      (gzip)
  application/vnd.docker.image.rootfs.diff.tar.zstd      (zstd)
  application/vnd.docker.image.rootfs.foreign.diff.tar   (foreign/비압축)

OCI:
  application/vnd.oci.image.layer.v1.tar                 (비압축)
  application/vnd.oci.image.layer.v1.tar+gzip            (gzip)
  application/vnd.oci.image.layer.v1.tar+zstd            (zstd)

확장:
  application/vnd.oci.image.layer.v1.tar+gzip+encrypted  (암호화)
  application/vnd.erofs.layer.v1                          (EROFS)
```

**핵심 차이**: OCI는 `+suffix` 패턴으로 압축/암호화를 표현하고,
Docker는 미디어 타입 자체에 압축 정보를 포함한다.
containerd의 `parseMediaTypes()`가 이 차이를 추상화한다.

### 10.3 Schema 1 거부

```go
// 소스: core/images/image.go

type unknownDocument struct {
    MediaType string          `json:"mediaType,omitempty"`
    Config    json.RawMessage `json:"config,omitempty"`
    Layers    json.RawMessage `json:"layers,omitempty"`
    Manifests json.RawMessage `json:"manifests,omitempty"`
    FSLayers  json.RawMessage `json:"fsLayers,omitempty"` // schema 1
}

func validateMediaType(b []byte, mt string) error {
    var doc unknownDocument
    json.Unmarshal(b, &doc)
    if len(doc.FSLayers) != 0 {
        return fmt.Errorf("media-type: schema 1 not supported")
    }
    // ...
}
```

Docker Schema 1은 `FSLayers` 필드를 사용했다. containerd는 이를 명시적으로 거부한다.
Schema 1은 서명 방식, 레이어 순서, 메타데이터 구조 등이 완전히 다르기 때문에
호환 코드를 유지하는 것보다 거부하는 것이 안전하다.

### 10.4 Docker MediaType 감지

```go
// 소스: core/images/mediatypes.go

func IsDockerType(mt string) bool {
    return strings.HasPrefix(mt, "application/vnd.docker.")
}
```

이 함수는 Docker 전용 처리가 필요한 경우(예: 레지스트리 호환성)에 사용된다.

---

## 11. 클라이언트 Image 인터페이스

### 11.1 Image 인터페이스

`client/image.go`에 정의된 클라이언트 측 Image 인터페이스:

```go
// 소스: client/image.go

type Image interface {
    Name() string                            // 이미지 이름
    Target() ocispec.Descriptor              // 루트 Descriptor
    Labels() map[string]string               // 레이블
    Unpack(context.Context, string, ...UnpackOpt) error  // 스냅샷으로 풀기
    RootFS(ctx context.Context) ([]digest.Digest, error) // DiffID 목록
    Size(ctx context.Context) (int64, error)             // 전체 크기
    Usage(context.Context, ...UsageOpt) (int64, error)   // 사용량
    Config(ctx context.Context) (ocispec.Descriptor, error) // Config Descriptor
    IsUnpacked(context.Context, string) (bool, error)    // Unpack 여부
    ContentStore() content.Store                          // Content Store
    Metadata() images.Image                               // 메타데이터
    Platform() platforms.MatchComparer                    // 플랫폼 매처
    Spec(ctx context.Context) (ocispec.Image, error)     // OCI Image 스펙
}
```

### 11.2 image 구현체

```go
// 소스: client/image.go

type image struct {
    client   *Client
    i        images.Image             // 코어 Image 메타데이터
    platform platforms.MatchComparer  // 플랫폼 매처
    diffIDs  []digest.Digest          // 캐시된 DiffID 목록
    mu       sync.Mutex               // diffIDs 보호
}
```

`diffIDs`는 뮤텍스로 보호되는 캐시이다. RootFS()가 처음 호출될 때 Config를 읽어 DiffID를
파싱하고 결과를 캐시한다:

```go
// 소스: client/image.go

func (i *image) RootFS(ctx context.Context) ([]digest.Digest, error) {
    i.mu.Lock()
    defer i.mu.Unlock()
    if i.diffIDs != nil {
        return i.diffIDs, nil  // 캐시 히트
    }
    diffIDs, err := i.i.RootFS(ctx, provider, i.platform)
    i.diffIDs = diffIDs
    return diffIDs, nil
}
```

### 11.3 IsUnpacked 검증

```go
// 소스: client/image.go

func (i *image) IsUnpacked(ctx context.Context, snapshotterName string) (bool, error) {
    sn, err := i.client.getSnapshotter(ctx, snapshotterName)
    diffs, err := i.RootFS(ctx)

    // ChainID로 스냅샷이 존재하는지 확인
    if _, err := sn.Stat(ctx, identity.ChainID(diffs).String()); err != nil {
        if errdefs.IsNotFound(err) {
            return false, nil  // 아직 unpack 안 됨
        }
        return false, err
    }
    return true, nil
}
```

ChainID는 DiffID 목록의 연쇄 해시로, 전체 레이어 스택의 고유 식별자이다:

```
ChainID(d0)           = d0
ChainID(d0, d1)       = sha256(d0 + " " + d1)
ChainID(d0, d1, d2)   = sha256(ChainID(d0, d1) + " " + d2)
```

### 11.4 Spec 메서드

```go
// 소스: client/image.go

func (i *image) Spec(ctx context.Context) (ocispec.Image, error) {
    desc, err := i.Config(ctx)
    blob, err := content.ReadBlob(ctx, i.ContentStore(), desc)
    var ociImage ocispec.Image
    json.Unmarshal(blob, &ociImage)
    return ociImage, nil
}
```

Config Descriptor를 통해 Content Store에서 blob을 읽고 OCI Image 구조체로 파싱한다.
이 구조체에는 `architecture`, `os`, `config`(Env, Cmd, WorkingDir 등), `rootfs` 정보가 포함된다.

### 11.5 Usage와 Size의 차이

```go
// Size: 단일 플랫폼, 매니페스트 기준
func (i *image) Size(ctx context.Context) (int64, error) {
    return usage.CalculateImageUsage(ctx, i.i, i.client.ContentStore(),
        usage.WithManifestLimit(i.platform, 1),
        usage.WithManifestUsage())
}

// Usage: 다양한 옵션 지원 (스냅샷 포함, 다중 매니페스트 등)
func (i *image) Usage(ctx context.Context, opts ...UsageOpt) (int64, error) {
    // ...옵션에 따라 다른 계산
}
```

| 메서드 | 범위 | 스냅샷 포함 | 용도 |
|--------|------|------------|------|
| `Size()` | 단일 플랫폼 | 아니오 | 이미지 packed 크기 |
| `Usage()` | 설정 가능 | 옵션 | 디스크 사용량 분석 |

### 11.6 checkSnapshotterSupport

```go
// 소스: client/image.go

func (i *image) checkSnapshotterSupport(ctx context.Context, snapshotterName string,
    manifest ocispec.Manifest) error {
    snapshotterPlatformMatcher, err := i.client.GetSnapshotterSupportedPlatforms(ctx, snapshotterName)
    manifestPlatform, err := images.ConfigPlatform(ctx, i.ContentStore(), manifest.Config)
    if snapshotterPlatformMatcher.Match(manifestPlatform) {
        return nil
    }
    return fmt.Errorf("snapshotter %s does not support platform %s for image %s",
        snapshotterName, manifestPlatform, manifest.Config.Digest)
}
```

스냅샷터가 이미지의 플랫폼을 지원하는지 확인한다.
예: Windows 이미지를 Linux overlay 스냅샷터로 unpack하려는 시도를 방지.

---

## 12. 설계 철학과 핵심 교훈

### 12.1 Content-Addressable 분리 아키텍처

containerd 이미지 관리의 가장 핵심적인 설계 결정은 **이름(Name)과 내용(Content)의 분리**이다:

```
┌─────────────┐           ┌──────────────┐           ┌─────────────┐
│ Image Store │ ──────►   │ Content Store│ ◄──────   │  Snapshot   │
│ (이름 매핑) │  Target   │ (blob 저장)  │  Unpack   │   Store     │
│             │  Desc.    │              │           │ (파일시스템) │
└─────────────┘           └──────────────┘           └─────────────┘
   관심사:                    관심사:                    관심사:
   이름 → digest             digest → 데이터            레이어 → 마운트
```

이 3-tier 분리의 이점:
1. 같은 blob을 여러 이미지가 공유 (디스크 절약)
2. 이미지 삭제가 즉시 blob 삭제를 의미하지 않음 (GC가 결정)
3. 각 Store를 독립적으로 교체/최적화 가능

### 12.2 Handler 체인 = Unix 파이프라인

이미지 처리의 Handler 체인은 Unix 파이프라인 철학을 따른다:

```
ChildrenHandler | FilterPlatforms | LimitManifests | SetChildrenLabels
     (해석)         (필터링)          (제한)           (라벨 설정)
```

각 핸들러는 하나의 일만 하고, 조합을 통해 복잡한 동작을 구성한다.
이 패턴의 장점:
- 새로운 처리 단계를 쉽게 추가
- 기존 핸들러의 재사용
- 테스트 용이성 (각 핸들러를 독립적으로 테스트)

### 12.3 이중 해시(Dual Hash) 전략

| 용도 | 해시 대상 | 사용 위치 |
|------|----------|----------|
| Content 식별 | 압축된 blob | Manifest의 layers[].digest |
| 내용 무결성 | 비압축 tar | Config의 rootfs.diff_ids[] |
| 스냅샷 식별 | DiffID 체인 | ChainID (identity.ChainID) |

이 이중 해시는 전송 효율(압축)과 내용 무결성(비압축 해시) 모두를 보장한다.
ChainID는 레이어 스택 전체의 고유 식별자로, 같은 레이어 조합을 가진 이미지가
동일 스냅샷을 공유할 수 있게 한다.

### 12.4 미디어 타입의 확장성

containerd의 미디어 타입 시스템은 확장에 열려 있다:

```
기본:     application/vnd.oci.image.layer.v1.tar
압축:     application/vnd.oci.image.layer.v1.tar+gzip
암호화:   application/vnd.oci.image.layer.v1.tar+gzip+encrypted
EROFS:   application/vnd.erofs.layer.v1
증명:     application/vnd.in-toto+json
```

`parseMediaTypes()`로 suffix를 분리하여 base 타입으로 분류하는 설계 덕분에,
새로운 압축/래핑 형식이 추가되어도 기존 분류 로직이 깨지지 않는다.

### 12.5 GC 라벨 = 참조 카운팅의 대안

전통적인 참조 카운팅 대신 GC 라벨을 사용하는 이유:

1. **원자성**: 라벨 설정은 blob 메타데이터의 원자적 업데이트
2. **관찰 가능성**: 라벨을 조회하면 참조 관계를 즉시 파악
3. **유연성**: 다양한 참조 유형(content, snapshot, referrer)을 통일된 메커니즘으로 표현
4. **안정성**: 라벨이 손실되면 최악의 경우 불필요한 blob이 남음 (데이터 손실이 아닌 공간 낭비)

### 12.6 Check 함수 - 이미지 완결성 검증

```go
// 소스: core/images/image.go

func Check(ctx context.Context, provider content.Provider, image ocispec.Descriptor,
    platform platforms.MatchComparer) (available bool, required, present, missing []ocispec.Descriptor, err error) {
    mfst, err := Manifest(ctx, provider, image, platform)
    required = append([]ocispec.Descriptor{mfst.Config}, mfst.Layers...)
    for _, desc := range required {
        ra, err := provider.ReaderAt(ctx, desc)
        if err != nil {
            if errdefs.IsNotFound(err) {
                missing = append(missing, desc)
                continue
            }
        }
        ra.Close()
        present = append(present, desc)
    }
    return true, required, present, missing, nil
}
```

Check는 이미지의 모든 구성 요소가 Content Store에 존재하는지 검증한다.
반환값이 4개(available, required, present, missing)인 이유:
- 호출자가 누락된 blob만 선택적으로 가져올 수 있음
- 부분 가용성 상태를 정확하게 보고 가능

### 12.7 정리

containerd의 이미지 관리 시스템은 **OCI 표준 위에 구축된 Content-Addressable DAG**라는
근본 원리로부터 자연스럽게 도출되는 설계이다. Image Store는 이름 공간, Content Store는
데이터 공간, Snapshot Store는 실행 공간을 각각 담당하며, 이 3개 Store 사이의 참조 관계는
GC 라벨과 Handler 체인을 통해 관리된다. Docker와 OCI 양쪽 포맷을 분류 함수의 동등 취급으로
통합 처리하고, 미디어 타입의 suffix 파싱으로 확장성을 확보한 점은 표준 기반 시스템의
모범적인 구현이다.
