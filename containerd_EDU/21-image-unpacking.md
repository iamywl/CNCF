# containerd 이미지 언팩(Unpacking) 시스템 Deep-Dive

## 1. 개요

containerd의 이미지 언팩 시스템은 OCI 이미지 레이어를 스냅샷 파일시스템으로 변환하는 핵심 서브시스템이다.
이미지 Pull 과정에서 가져온 압축된 레이어 blob을 실제 컨테이너가 사용할 수 있는 rootfs로
만드는 전체 파이프라인을 담당한다.

### 1.1 왜 별도 시스템이 필요한가

이미지 Pull과 언팩은 근본적으로 다른 작업이다:

| 단계 | 작업 | I/O 특성 |
|------|------|----------|
| Pull (Fetch) | 레지스트리에서 blob 다운로드 | 네트워크 I/O 바운드 |
| Unpack | 레이어를 스냅샷으로 적용 | 디스크 I/O + CPU 바운드 |

containerd v2는 이 두 작업을 **병렬화**하여 성능을 최적화한다. Fetch가 완료된 레이어부터
즉시 언팩을 시작할 수 있으며, 여러 레이어를 동시에 처리할 수도 있다.

### 1.2 소스 위치

```
containerd/
├── core/unpack/
│   ├── unpacker.go          # 핵심 Unpacker 구현 (750줄)
│   └── unpacker_test.go     # 테스트
├── core/diff/
│   └── apply/apply.go       # 레이어 적용 (fsApplier)
└── pkg/labels/
    └── labels.go            # LabelUncompressed 등 레이블 상수
```

## 2. 아키텍처

### 2.1 전체 흐름

```
┌──────────────────────────────────────────────────────────────┐
│                    Image Pull Pipeline                        │
│                                                              │
│  Registry ──→ Fetch Handler ──→ Content Store                │
│                    │                    │                     │
│                    ▼                    ▼                     │
│              Unpacker.Unpack()     blob 저장 완료              │
│                    │                                         │
│           ┌────────┴────────┐                                │
│           ▼                 ▼                                │
│     Config 파싱        Layer 처리                              │
│     (DiffIDs 추출)     (병렬 가능)                             │
│           │                 │                                │
│           │           ┌─────┴─────┐                          │
│           │           ▼           ▼                          │
│           │      topHalf     bottomHalf                      │
│           │    (스냅샷 준비)  (커밋/라벨링)                     │
│           │           │           │                          │
│           │           ▼           ▼                          │
│           │      Applier.Apply  Snapshotter.Commit           │
│           │      (tar 추출)    (ChainID 이름)                 │
│           │                                                  │
│           └──→ GC 참조 라벨 설정                               │
└──────────────────────────────────────────────────────────────┘
```

### 2.2 핵심 데이터 구조

```
Unpacker                        Platform
┌──────────────────────┐       ┌─────────────────────────────┐
│ unpackerConfig       │       │ Platform    Matcher          │
│   ├─ platforms []*P  │──────→│ SnapshotterKey  string      │
│   ├─ content   Store │       │ Snapshotter     Snapshotter │
│   ├─ limiter         │       │ Applier         diff.Applier│
│   └─ duplicationSup  │       │ SnapshotterCap  []string    │
│ unpacks  atomic.Int32│       │ ConfigType      string      │
│ ctx      context.Ctx │       │ LayerTypes      []string    │
│ eg       *errgroup   │       └─────────────────────────────┘
└──────────────────────┘
                                unpackConfig (이미지 설정 파싱용)
                               ┌───────────────────┐
                               │ Platform  ocispec  │
                               │ RootFS    ocispec  │
                               │   └─ DiffIDs []dig │
                               └───────────────────┘
```

실제 소스코드 (`core/unpack/unpacker.go:57-164`):

```go
type Platform struct {
    Platform   platforms.Matcher
    SnapshotterKey          string
    Snapshotter             snapshots.Snapshotter
    SnapshotOpts            []snapshots.Opt
    Applier                 diff.Applier
    ApplyOpts               []diff.ApplyOpt
    ConfigType              string
    LayerTypes              []string
    SnapshotterCapabilities []string
}

type Unpacker struct {
    unpackerConfig
    unpacks atomic.Int32
    ctx     context.Context
    eg      *errgroup.Group
}
```

## 3. ChainID 계산 알고리즘

### 3.1 왜 ChainID가 필요한가

OCI 이미지의 각 레이어는 `DiffID`(압축 해제 후 tar의 SHA256)를 가진다.
하지만 스냅샷은 레이어들이 **순서대로 적용**된 결과이므로, 단순 DiffID만으로는
어떤 레이어 스택 위에 적용되었는지 구분할 수 없다.

`ChainID`는 레이어 스택의 고유 식별자이다:

```
ChainID(L0)     = DiffID(L0)
ChainID(L0, L1) = SHA256(ChainID(L0) + " " + DiffID(L1))
ChainID(L0..Ln) = SHA256(ChainID(L0..Ln-1) + " " + DiffID(Ln))
```

### 3.2 소스에서의 ChainID 계산

`core/unpack/unpacker.go:368-370`:
```go
chainIDs := make([]digest.Digest, len(diffIDs))
copy(chainIDs, diffIDs)
chainIDs = identity.ChainIDs(chainIDs)
```

`identity.ChainIDs()`는 OCI image-spec의 표준 구현이며, 위의 재귀적 해싱을 수행한다.

### 3.3 ChainID가 스냅샷 이름으로 사용되는 과정

```
Layer 0: DiffID = sha256:aaa...
         ChainID = sha256:aaa...
         Snapshot Name = "sha256:aaa..."

Layer 1: DiffID = sha256:bbb...
         ChainID = SHA256("sha256:aaa... sha256:bbb...") = sha256:ccc...
         Snapshot Name = "sha256:ccc..."

Layer 2: DiffID = sha256:ddd...
         ChainID = SHA256("sha256:ccc... sha256:ddd...") = sha256:eee...
         Snapshot Name = "sha256:eee..."
```

## 4. 언팩 핵심 흐름 분석

### 4.1 Unpack() 메서드 - 이미지 핸들러 래핑

`Unpack()` 메서드(`core/unpack/unpacker.go:194-270`)는 기존 이미지 핸들러를 래핑하여
매니페스트와 설정을 인터셉트한다:

```
┌───────────────────────────────────────────┐
│         Unpack Handler 로직                │
│                                           │
│  매니페스트 발견?                            │
│    ├─ Yes: children을 layer/non-layer 분리 │
│    │       layers[configDigest] = layers   │
│    │       non-layer만 children으로 반환     │
│    └─ No                                  │
│                                           │
│  Config 발견?                               │
│    ├─ Yes: layers[digest] 조회              │
│    │       goroutine으로 unpack() 시작      │
│    └─ No: 그냥 통과                         │
└───────────────────────────────────────────┘
```

핵심 코드 (`core/unpack/unpacker.go:235-268`):
```go
if images.IsManifestType(desc.MediaType) {
    // 레이어와 비-레이어(config 등) 분리
    for i, child := range children {
        if images.IsLayerType(child.MediaType) || layerTypes[child.MediaType] {
            manifestLayers = append(manifestLayers, child)
        } else {
            nonLayers = append(nonLayers, child)
        }
    }
    // config digest → layers 매핑 저장
    lock.Lock()
    for _, nl := range nonLayers {
        layers[nl.Digest] = manifestLayers
    }
    lock.Unlock()
    children = nonLayers  // config만 반환 (레이어는 unpack이 직접 처리)
} else if images.IsConfigType(desc.MediaType) || configTypes[desc.MediaType] {
    // config 도착 시 unpack 시작
    u.eg.Go(func() error {
        return u.unpack(h, desc, l)
    })
}
```

### 4.2 unpack() 메서드 - 실제 언팩 수행

`unpack()` 메서드(`core/unpack/unpacker.go:301-640`)는 이미지의 모든 레이어를
스냅샷으로 변환하는 핵심 로직이다.

#### 단계 1: 이미지 설정 파싱

```go
p, err := content.ReadBlob(ctx, u.content, config)
var i unpackConfig
json.Unmarshal(p, &i)
diffIDs := i.RootFS.DiffIDs
```

#### 단계 2: 플랫폼 매칭

```go
imgPlatform := platforms.Normalize(i.Platform)
for _, up := range u.platforms {
    if up.Platform.Match(imgPlatform) {
        unpack = up
        break
    }
}
```

#### 단계 3: topHalf / bottomHalf 분리 패턴

containerd는 각 레이어의 언팩을 두 단계로 분리한다:

```
topHalf (비동기 가능):
  1. 스냅샷 Prepare (parent 위에 새 active 스냅샷)
  2. Fetch 완료 대기
  3. Applier.Apply (tar 추출 → 마운트에 적용)
  4. DiffID 검증

bottomHalf (순차적):
  1. 스냅샷 Commit (active → committed)
  2. Content Store에 uncompressed 라벨 설정
```

### 4.3 병렬 언팩 (Parallel Unpack)

`core/unpack/unpacker.go:732-741`:
```go
func (u *Unpacker) supportParallel(unpack *Platform) bool {
    if u.unpackLimiter == nil {
        return false
    }
    if !slices.Contains(unpack.SnapshotterCapabilities, "rebase") {
        return false
    }
    return true
}
```

병렬 모드가 활성화되면:
- `topHalf`는 각 레이어에 대해 독립적으로 goroutine에서 실행
- `bottomHalf`는 여전히 순차적으로 실행 (커밋 순서 보장)
- 스냅샷터가 "rebase" 기능을 지원해야 함

```
순차 모드:              병렬 모드:
L0: top → bottom      L0: top ──┐
L1: top → bottom      L1: top ──┼─→ bottom(L0) → bottom(L1) → bottom(L2)
L2: top → bottom      L2: top ──┘
```

### 4.4 Fetch와 Unpack의 동기화

`core/unpack/unpacker.go:441-458`에서 fetch 채널을 사용한 동기화:

```go
fetchErr = make([]chan error, n)
fetchC = make([]chan struct{}, n)
for i := range n {
    fetchC[i] = make(chan struct{})
    fetchErr[i] = make(chan error, 1)
}
go func(i int) {
    err := u.fetch(ctx, h, layers[i:], fetchC)
    // 에러 발생 시 모든 fetchErr 채널에 전파
}(i)
```

Apply 전에 fetch 완료를 대기:
```go
select {
case <-ctx.Done():
    // 취소
case err := <-fetchErr[i-fetchOffset]:
    // fetch 에러
case <-fetchC[i-fetchOffset]:
    // fetch 완료, Apply 진행
}
```

## 5. 중복 억제 (Duplication Suppression)

### 5.1 왜 필요한가

여러 이미지가 동일한 base 레이어를 공유할 때, 동시에 Pull하면 같은 레이어를
중복 언팩하게 된다. `KeyedLocker` 인터페이스로 이를 방지한다.

### 5.2 두 가지 잠금 키

```go
// 스냅샷 체인ID 기반 잠금 (같은 레이어 스택의 중복 언팩 방지)
func (u *Unpacker) makeChainIDKeyWithSnapshotter(chainID, snapshotter string) string {
    return fmt.Sprintf("sn://%s/%v", snapshotter, chainID)
}

// blob descriptor 기반 잠금 (같은 blob의 중복 fetch 방지)
func (u *Unpacker) makeBlobDescriptorKey(desc ocispec.Descriptor) string {
    return fmt.Sprintf("blob://%v", desc.Digest)
}
```

## 6. 스냅샷 준비와 커밋

### 6.1 Prepare: 활성 스냅샷 생성

```go
key = fmt.Sprintf(snapshots.UnpackKeyFormat, uniquePart(), chainID)
mounts, err = sn.Prepare(ctx, key, parent, opts...)
```

- `key`: 임시 이름 (타임스탬프 + 랜덤 + ChainID)
- `parent`: 이전 레이어의 ChainID (첫 레이어는 빈 문자열)
- 반환값 `mounts`: Applier가 tar를 추출할 마운트 포인트

### 6.2 Apply: 레이어 적용

```go
diff, err := a.Apply(ctx, desc, mounts, unpack.ApplyOpts...)
if diff.Digest != diffIDs[i] {
    // DiffID 불일치 → 무결성 오류
}
```

`diff.Applier.Apply()`는 `core/diff/apply/apply.go`에서 구현:
1. Content Store에서 blob 읽기
2. StreamProcessor 체인으로 압축 해제 (gzip → tar)
3. 마운트된 디렉토리에 tar 추출
4. 추출된 콘텐츠의 DiffID 계산 및 반환

### 6.3 Commit: 활성 → 커밋 스냅샷

```go
err = sn.Commit(ctx, chainID, key, opts...)
```

- 활성 스냅샷 `key`를 커밋된 스냅샷 `chainID`로 변환
- 이후 다른 스냅샷의 parent로 사용 가능
- 이미 존재하면 `AlreadyExists` 에러 → 무시 (중복 언팩 시)

### 6.4 GC 참조 라벨 설정

모든 레이어 언팩 완료 후:
```go
cinfo := content.Info{
    Digest: config.Digest,
    Labels: map[string]string{
        fmt.Sprintf("containerd.io/gc.ref.snapshot.%s", unpack.SnapshotterKey): chainID,
    },
}
cs.Update(ctx, cinfo, ...)
```

이 라벨은 GC가 이미지 설정 → 최종 스냅샷 참조를 추적하는 데 사용된다.

## 7. 에러 처리와 정리

### 7.1 abort 함수

각 레이어 언팩에는 실패 시 정리하는 abort 함수가 설정된다:

```go
abort := func(ctx context.Context) {
    if err := sn.Remove(ctx, key); err != nil {
        log.G(ctx).WithError(err).Errorf("failed to cleanup %q", key)
    }
}
```

### 7.2 bottomHalf에서의 분기

```go
bottomHalf := func(s *unpackStatus, prevErrs error) error {
    if s.err != nil {
        s.bottomF(true)   // abort: 현재 레이어 에러
        return s.err
    } else if prevErrs != nil {
        s.bottomF(true)   // abort: 이전 레이어 에러
        return fmt.Errorf("aborted")
    } else {
        return s.bottomF(false)  // commit: 정상
    }
}
```

### 7.3 AlreadyExists 처리

스냅샷이 이미 존재하는 경우 (다른 프로세스가 먼저 언팩한 경우):

```go
mounts, err = sn.Prepare(ctx, key, parent, opts...)
if err != nil {
    if errdefs.IsAlreadyExists(err) {
        if snInfo, err := sn.Stat(ctx, chainID); err != nil {
            if !errdefs.IsNotFound(err) {
                return nil, fmt.Errorf("failed to stat snapshot %s: %w", chainID, err)
            }
            // 재시도 (최대 3회)
        } else {
            return nil, nil  // 이미 완료 → 스킵
        }
    }
}
```

## 8. Limiter 인터페이스

### 8.1 두 가지 Limiter

```go
type Limiter interface {
    Acquire(context.Context, int64) error
    Release(int64)
}
```

| Limiter | 용도 | 설정 |
|---------|------|------|
| `limiter` | Fetch 동시성 제한 | `WithLimiter()` |
| `unpackLimiter` | Unpack 동시성 제한 | `WithUnpackLimiter()` |

Fetch와 Unpack의 동시성을 독립적으로 제어할 수 있다:
- 네트워크가 빠르면 Fetch limiter를 높게
- 디스크가 느리면 Unpack limiter를 낮게

## 9. uniquePart() 함수

스냅샷 키에 고유성을 부여하는 간단한 함수:

```go
func uniquePart() string {
    t := time.Now()
    var b [3]byte
    rand.Read(b[:])
    return fmt.Sprintf("%d-%s", t.Nanosecond(), base64.URLEncoding.EncodeToString(b[:]))
}
```

타임스탬프 나노초 + 3바이트 랜덤으로 충분한 고유성을 제공한다.
같은 ChainID에 대해 여러 Prepare가 동시에 시도될 수 있으므로 키가 고유해야 한다.

## 10. 성능 최적화 전략

### 10.1 파이프라인 병렬화

```
시간 ──────────────────────────────────────→

순차:  [Fetch L0] [Unpack L0] [Fetch L1] [Unpack L1] [Fetch L2] [Unpack L2]

파이프라인: [Fetch L0] [Fetch L1] [Fetch L2]
                  [Unpack L0] [Unpack L1] [Unpack L2]

완전 병렬: [Fetch L0] [Fetch L1] [Fetch L2]
           [Unpack L0]
                 [Unpack L1]
                       [Unpack L2]
           └─ bottomHalf는 순차 ─────────┘
```

### 10.2 rebase 기능

병렬 모드에서는 각 레이어가 독립적으로 준비되고, bottomHalf에서 순차적으로
커밋된다. 이때 스냅샷터의 "rebase" 기능이 필요하다:

```go
if i > 0 && parallel {
    parent = chainIDs[i-1].String()
    opts = append(opts, snapshots.WithParent(parent))
}
err = sn.Commit(ctx, chainID, key, opts...)
```

커밋 시점에 실제 parent를 지정하여 스냅샷 체인을 재구성한다.

## 11. Transfer Service와의 관계

containerd v2의 Transfer Service(`core/transfer/`)는 이미지 Pull/Push의
상위 API이다. Transfer Service 내부에서 `Unpacker`를 생성하여 사용한다:

```
TransferService.Transfer()
  └─ Pull (image source → destination)
       └─ NewUnpacker(cs, WithUnpackPlatform(...))
            └─ unpacker.Unpack(handler)  // 핸들러 래핑
            └─ unpacker.Wait()           // 완료 대기
```

## 12. 설계 결정의 이유

### 12.1 왜 topHalf/bottomHalf로 나누는가?

Apply(tar 추출)는 CPU/IO 바운드이므로 병렬화가 유리하다.
하지만 Commit은 스냅샷 체인의 순서를 보장해야 하므로 순차적이어야 한다.
이 두 단계를 분리함으로써 Apply는 병렬로, Commit은 순차로 실행할 수 있다.

### 12.2 왜 errgroup을 사용하는가?

`errgroup.Group`은 여러 goroutine을 관리하면서:
- 하나라도 에러가 발생하면 컨텍스트를 취소
- `Wait()`로 모든 goroutine 완료를 대기
- 첫 번째 에러를 반환

이는 이미지 언팩에서 하나의 레이어 실패 시 전체 언팩을 취소하는
시맨틱과 정확히 일치한다.

### 12.3 왜 레이어를 children에서 제거하는가?

Unpack 핸들러는 레이어를 children에서 제거하고, config만 남긴다.
이렇게 하면 일반 이미지 핸들러는 레이어 blob을 개별 처리하지 않고,
Unpacker가 직접 fetch 타이밍을 제어할 수 있다.

## 13. 정리

| 구성요소 | 역할 |
|---------|------|
| `Unpacker` | 언팩 파이프라인 조율자 |
| `Platform` | 플랫폼별 스냅샷터/Applier 설정 |
| `topHalf` | 스냅샷 준비 + Apply (병렬 가능) |
| `bottomHalf` | 스냅샷 커밋 (순차적) |
| `ChainID` | 레이어 스택의 고유 식별자 |
| `Limiter` | 동시성 제어 (Fetch/Unpack 독립) |
| `KeyedLocker` | 중복 억제 |
| `errgroup` | goroutine 관리 + 에러 전파 |

containerd의 이미지 언팩 시스템은 파이프라인 병렬화, 중복 억제,
세밀한 에러 처리를 통해 대규모 컨테이너 환경에서도 효율적으로 동작하도록 설계되었다.

---

*소스 참조: `core/unpack/unpacker.go`, `core/diff/apply/apply.go`*
*containerd 버전: v2.0*
