# 08. containerd Content Store Deep-Dive

## 목차

1. [개요](#1-개요)
2. [Content-Addressable Storage 원리](#2-content-addressable-storage-원리)
3. [Store 인터페이스 계층 구조](#3-store-인터페이스-계층-구조)
4. [Info와 Status: 메타데이터 구조](#4-info와-status-메타데이터-구조)
5. [Writer 생명주기](#5-writer-생명주기)
6. [Local Store 구현: 파일시스템 레이아웃](#6-local-store-구현-파일시스템-레이아웃)
7. [Writer 구현 상세](#7-writer-구현-상세)
8. [Digest 검증 메커니즘](#8-digest-검증-메커니즘)
9. [MRSW와 동시성 제어](#9-mrsw와-동시성-제어)
10. [Ingest 흐름: 데이터 쓰기 과정](#10-ingest-흐름-데이터-쓰기-과정)
11. [Label Store 활용](#11-label-store-활용)
12. [Helper 함수들: Copy, WriteBlob, ReadBlob](#12-helper-함수들-copy-writeblob-readblob)
13. [Walk와 필터링](#13-walk와-필터링)
14. [설계 철학과 GC 연동](#14-설계-철학과-gc-연동)

---

## 1. 개요

Content Store는 containerd에서 이미지 레이어, 매니페스트, 설정 등 모든 **불변 블롭(blob)**을 저장하는 핵심 저장소이다. OCI 이미지의 모든 구성요소가 Content Store에 저장되며, SHA256 다이제스트로 주소를 지정하는 CAS(Content-Addressable Storage) 패턴을 따른다.

```
소스 위치:
  core/content/content.go          -- 인터페이스 정의
  core/content/helpers.go          -- Copy, WriteBlob, ReadBlob 등
  plugins/content/local/store.go   -- 파일시스템 기반 구현
  plugins/content/local/writer.go  -- Writer 구현
```

---

## 2. Content-Addressable Storage 원리

### CAS란 무엇인가

```
전통적 저장소:                    CAS (Content-Addressable Storage):

  이름 → 데이터                    다이제스트 → 데이터
  +---------+                     +---------------------------+
  | file.txt| → "hello"          | sha256:2cf24... | → "hello"
  | copy.txt| → "hello"          +---------------------------+
  +---------+
  (같은 데이터 2번 저장)            (동일 내용은 1번만 저장)
```

CAS의 핵심 특성:
1. **중복 제거** -- 동일 콘텐츠는 같은 다이제스트를 가지므로 한 번만 저장
2. **무결성 보장** -- 다이제스트로 검증 가능. 비트 플립이나 손상 즉시 감지
3. **불변성** -- 다이제스트가 콘텐츠를 결정하므로 수정 불가 (수정 = 새 다이제스트)
4. **공유 안전성** -- 어떤 참조가 삭제되어도 다른 참조에 영향 없음

### 왜 containerd에 CAS인가

OCI 이미지의 구조가 CAS와 완벽하게 일치한다:

```
Image Index
  |
  +-- Manifest (sha256:aaa...)
       |
       +-- Config (sha256:bbb...)
       +-- Layer 1 (sha256:ccc...)
       +-- Layer 2 (sha256:ddd...)
```

각 컴포넌트가 다이제스트로 참조되므로, Content Store는 이 구조를 자연스럽게 저장하고 검증할 수 있다.

---

## 3. Store 인터페이스 계층 구조

### 인터페이스 합성 계층도

```go
// 소스: core/content/content.go:41-46

type Store interface {
    Manager         // Info, Update, Walk, Delete
    Provider        // ReaderAt
    IngestManager   // Status, ListStatuses, Abort
    Ingester        // Writer
}
```

```
Store (최상위 합성 인터페이스)
  |
  +-- Manager (읽기/삭제 관리)
  |     |
  |     +-- InfoProvider: Info(dgst) -> Info
  |     +-- Update(info, fieldpaths...) -> Info
  |     +-- Walk(fn, filters...) -> error
  |     +-- Delete(dgst) -> error
  |
  +-- Provider (읽기 전용)
  |     +-- ReaderAt(desc) -> ReaderAt
  |
  +-- IngestManager (진행 중 쓰기 관리)
  |     +-- Status(ref) -> Status
  |     +-- ListStatuses(filters...) -> []Status
  |     +-- Abort(ref) -> error
  |
  +-- Ingester (쓰기 시작)
        +-- Writer(opts...) -> Writer
```

### 왜 이렇게 나누는가

각 인터페이스는 독립적으로 사용될 수 있다:

| 인터페이스 | 사용 예 |
|-----------|---------|
| Provider | 이미지 레이어 읽기만 필요한 경우 |
| Ingester | 새 블롭을 쓰기만 하는 경우 |
| Manager | 블롭 메타데이터 관리 (라벨 업데이트) |
| IngestManager | 진행 중인 다운로드 상태 확인 |
| Store | 전체 기능이 필요한 경우 |

### Provider 인터페이스

```go
// 소스: core/content/content.go:56-61

type Provider interface {
    ReaderAt(ctx context.Context, desc ocispec.Descriptor) (ReaderAt, error)
}

type ReaderAt interface {
    io.ReaderAt
    io.Closer
    Size() int64
}
```

`ReaderAt`은 표준 `io.ReaderAt`을 확장하여 `Size()`와 `Close()`를 추가한다. `io.ReaderAt`은 임의 위치 읽기를 지원하므로 대용량 레이어의 부분 읽기에 적합하다.

### Ingester와 Writer 인터페이스

```go
// 소스: core/content/content.go:64-71

type Ingester interface {
    Writer(ctx context.Context, opts ...WriterOpt) (Writer, error)
}

// 소스: core/content/content.go:146-166

type Writer interface {
    io.WriteCloser
    Digest() digest.Digest
    Commit(ctx context.Context, size int64, expected digest.Digest, opts ...Opt) error
    Status() (Status, error)
    Truncate(size int64) error
}
```

Writer는 **트랜잭션적** 쓰기를 제공한다:
1. `Writer()`로 트랜잭션 시작 (ref로 식별)
2. `Write()`로 데이터 기록
3. `Commit()`으로 확정 (다이제스트 검증 포함)
4. 실패 시 `Close()` 후 재개 가능, 또는 `Abort()`로 취소

---

## 4. Info와 Status: 메타데이터 구조

### Info: 커밋된 블롭의 메타데이터

```go
// 소스: core/content/content.go:90-96

type Info struct {
    Digest    digest.Digest
    Size      int64
    CreatedAt time.Time
    UpdatedAt time.Time
    Labels    map[string]string
}
```

| 필드 | 설명 |
|------|------|
| Digest | SHA256 다이제스트 (블롭의 고유 식별자) |
| Size | 바이트 크기 |
| CreatedAt | 최초 커밋 시각 (파일 수정 시각) |
| UpdatedAt | 마지막 업데이트 시각 (파일 접근 시각) |
| Labels | 가변 메타데이터 (GC 참조, 미디어 타입 등) |

### Status: 진행 중인 쓰기의 상태

```go
// 소스: core/content/content.go:99-106

type Status struct {
    Ref       string
    Offset    int64
    Total     int64
    Expected  digest.Digest
    StartedAt time.Time
    UpdatedAt time.Time
}
```

| 필드 | 설명 |
|------|------|
| Ref | 쓰기 트랜잭션의 고유 참조 키 |
| Offset | 현재까지 기록된 바이트 수 |
| Total | 예상 총 크기 (0이면 미정) |
| Expected | 예상 다이제스트 (빈 값이면 미정) |
| StartedAt | 쓰기 시작 시각 |
| UpdatedAt | 마지막 쓰기 시각 |

---

## 5. Writer 생명주기

```
Writer 생명주기 상태 다이어그램:

  Writer()
    |
    v
  [Active] ---Write()---> [Active] ---Write()---> ...
    |                        |
    |  Close()               |  Commit(size, digest)
    v                        v
  [Suspended]            [Committed]
    |                        |
    |  Writer(same ref)      |  (블롭이 Content Store에 영구 저장)
    v                        |
  [Active] (재개)            v
    |                    [Available via Provider]
    |  Abort(ref)
    v
  [Aborted] (모든 임시 데이터 삭제)
```

### 상태 전이 설명

1. **Active**: `Writer()`로 시작. 데이터를 `Write()`로 기록 중
2. **Suspended**: `Close()`로 일시 중단. 같은 ref로 `Writer()`를 다시 호출하면 재개
3. **Committed**: `Commit()`으로 확정. 블롭이 blobs/ 디렉토리로 이동
4. **Aborted**: `Abort(ref)`로 완전 취소. 임시 데이터 삭제

### 재개(Resume) 메커니즘

containerd의 Writer는 네트워크 중단 등의 상황에서 **이어쓰기(resume)**를 지원한다. 같은 ref로 `Writer()`를 호출하면 기존 진행 상태를 복원하고, 이전에 기록된 데이터의 다이제스트를 재계산한 후 이어서 기록할 수 있다.

```go
// 소스: plugins/content/local/store.go:501-529 (resumeStatus)

func (s *store) resumeStatus(ref string, total int64, digester digest.Digester) (content.Status, error) {
    path, _, data := s.ingestPaths(ref)
    status, err := s.status(path)
    // ...
    // 기존 데이터 파일을 읽어 다이제스트 재계산
    fp, err := os.Open(data)
    p := bufPool.Get().(*[]byte)
    status.Offset, err = io.CopyBuffer(digester.Hash(), fp, *p)
    bufPool.Put(p)
    fp.Close()
    return status, err
}
```

---

## 6. Local Store 구현: 파일시스템 레이아웃

### store 구조체

```go
// 소스: plugins/content/local/store.go:68-76

type store struct {
    root               string
    ls                 LabelStore
    integritySupported bool
    locksMu              sync.Mutex
    locks                map[string]*lock
    ensureIngestRootOnce func() error
}
```

### 파일시스템 디렉토리 구조

```
{root}/
  |
  +-- blobs/
  |     |
  |     +-- sha256/
  |           |
  |           +-- 2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824
  |           +-- a1b2c3d4e5f6...
  |           +-- ...
  |
  +-- ingest/
        |
        +-- {encoded-ref-hash-1}/
        |     +-- ref          (원본 ref 문자열)
        |     +-- data         (쓰기 중인 데이터)
        |     +-- startedat    (시작 시각)
        |     +-- updatedat    (갱신 시각)
        |     +-- total        (예상 총 크기, 선택적)
        |
        +-- {encoded-ref-hash-2}/
              +-- ...
```

### 경로 계산 함수들

```go
// 소스: plugins/content/local/store.go:641-669

// 블롭 경로: {root}/blobs/{algorithm}/{encoded-hash}
func (s *store) blobPath(dgst digest.Digest) (string, error) {
    if err := dgst.Validate(); err != nil {
        return "", fmt.Errorf("cannot calculate blob path from invalid digest: %v: %w",
            err, errdefs.ErrInvalidArgument)
    }
    return filepath.Join(s.root, "blobs",
        dgst.Algorithm().String(), dgst.Encoded()), nil
}

// ingest 루트: ref를 해시하여 일정 길이의 디렉토리명 생성
func (s *store) ingestRoot(ref string) string {
    dgst := digest.FromString(ref)
    return filepath.Join(s.root, "ingest", dgst.Encoded())
}

// ingest 경로들: root, ref 파일, data 파일
func (s *store) ingestPaths(ref string) (string, string, string) {
    var (
        fp = s.ingestRoot(ref)
        rp = filepath.Join(fp, "ref")
        dp = filepath.Join(fp, "data")
    )
    return fp, rp, dp
}
```

**왜 ref를 해시하는가:**
ref 문자열은 임의의 길이와 문자를 가질 수 있다. 파일시스템의 경로 길이 제한과 특수 문자 문제를 피하기 위해, ref의 SHA256 해시를 디렉토리명으로 사용한다.

---

## 7. Writer 구현 상세

### writer 구조체

```go
// 소스: plugins/content/local/writer.go:38-48

type writer struct {
    s         *store           // 부모 store 참조
    fp        *os.File         // 열린 data 파일
    path      string           // ingest 디렉토리 경로
    ref       string           // 참조 키
    offset    int64            // 현재 쓰기 위치
    total     int64            // 예상 총 크기
    digester  digest.Digester  // 실시간 해시 계산기
    startedAt time.Time
    updatedAt time.Time
}
```

### Write: 이중 기록

```go
// 소스: plugins/content/local/writer.go:71-77

func (w *writer) Write(p []byte) (n int, err error) {
    n, err = w.fp.Write(p)          // 1. 파일에 기록
    w.digester.Hash().Write(p[:n])  // 2. 해시에도 기록
    w.offset += int64(len(p))
    w.updatedAt = time.Now()
    return n, err
}
```

모든 `Write()` 호출마다 데이터가 **파일과 해시 계산기 양쪽**에 동시에 기록된다. 이 "이중 기록" 패턴 덕분에 별도의 검증 단계 없이 `Commit()` 시점에 다이제스트를 즉시 확인할 수 있다.

### Commit: 원자적 확정

```go
// 소스: plugins/content/local/writer.go:79-187 (핵심 흐름)

func (w *writer) Commit(ctx context.Context, size int64, expected digest.Digest, opts ...content.Opt) error {
    defer w.s.unlock(w.ref)  // 반드시 락 해제

    // 1. 파일 sync 후 닫기
    fp.Sync()
    fi, err := fp.Stat()
    fp.Close()

    // 2. 크기 검증
    if size > 0 && size != fi.Size() {
        return fmt.Errorf("unexpected commit size %d, expected %d: %w",
            fi.Size(), size, errdefs.ErrFailedPrecondition)
    }

    // 3. 다이제스트 검증
    dgst := w.digester.Digest()
    if expected != "" && expected != dgst {
        return fmt.Errorf("unexpected commit digest %s, expected %s: %w",
            dgst, expected, errdefs.ErrFailedPrecondition)
    }

    // 4. 중복 확인
    target, _ := w.s.blobPath(dgst)
    if _, err := os.Stat(target); err == nil {
        os.RemoveAll(w.path)
        return fmt.Errorf("content %v: %w", dgst, errdefs.ErrAlreadyExists)
    }

    // 5. 원자적 이동: ingest/data → blobs/sha256/{digest}
    os.MkdirAll(filepath.Dir(target), 0755)
    os.Rename(ingest, target)

    // 6. fsverity 활성화 (지원 시)
    if w.s.integritySupported {
        fsverity.Enable(target)
    }

    // 7. 읽기 전용으로 권한 변경
    os.Chmod(target, (fi.Mode()&os.ModePerm)&^0333)

    // 8. 정리: ingest 디렉토리 삭제
    os.RemoveAll(w.path)

    // 9. 라벨 저장 (있는 경우)
    if w.s.ls != nil && base.Labels != nil {
        w.s.ls.Set(dgst, base.Labels)
    }

    return nil
}
```

### Commit 흐름도

```
Commit(size=1024, expected="sha256:2cf24...")
  |
  v
[1] fp.Sync() + fp.Close()
  |
  v
[2] fi.Size() == 1024? --> No: ErrFailedPrecondition
  |
  v (Yes)
[3] digester.Digest() == "sha256:2cf24..."? --> No: ErrFailedPrecondition
  |
  v (Yes)
[4] blobs/sha256/2cf24... 이미 존재? --> Yes: ErrAlreadyExists
  |
  v (No)
[5] os.Rename(ingest/data, blobs/sha256/2cf24...)  <-- 원자적!
  |
  v
[6] fsverity.Enable() (선택적)
  |
  v
[7] chmod 0444 (읽기 전용)
  |
  v
[8] os.RemoveAll(ingest/{ref-hash}/)
  |
  v
[9] LabelStore.Set(digest, labels) (선택적)
  |
  v
SUCCESS
```

**왜 os.Rename인가:**
`os.Rename()`은 같은 파일시스템 내에서 **원자적(atomic)** 연산이다. ingest 데이터가 blobs 디렉토리로 이동하는 과정에서 중간 상태가 존재하지 않는다. 시스템 크래시가 발생해도 파일이 완전히 이동되었거나 전혀 이동되지 않은 상태만 존재한다.

---

## 8. Digest 검증 메커니즘

### 실시간 해시 계산

```
Writer.Write(data_chunk_1)
  |
  +---> fp.Write(chunk_1)         --> 디스크에 기록
  +---> digester.Hash().Write(chunk_1) --> SHA256 상태 갱신

Writer.Write(data_chunk_2)
  |
  +---> fp.Write(chunk_2)
  +---> digester.Hash().Write(chunk_2)

Writer.Commit(size, expected_digest)
  |
  +---> dgst = digester.Digest()  --> 최종 SHA256 계산
  +---> dgst == expected_digest?  --> 불일치 시 거부
```

이 패턴의 장점:
1. **별도의 검증 패스 불필요** -- 데이터를 다시 읽을 필요 없음
2. **실시간 감지** -- 네트워크 전송 중 변조가 Commit 시 즉시 감지
3. **효율적 재개** -- 재개 시 기존 데이터만 다시 해시하면 됨

### 재개 시 다이제스트 재계산

```go
// 소스: plugins/content/local/store.go:523-527

fp, err := os.Open(data)
p := bufPool.Get().(*[]byte)
status.Offset, err = io.CopyBuffer(digester.Hash(), fp, *p)
bufPool.Put(p)
fp.Close()
```

재개(resume) 시 기존에 기록된 데이터를 전부 읽어 해시를 재계산한다. 소스 코드 주석에서도 "slow slow slow!!"라고 표현되어 있으며, 향후 "resumable hashes" 도입이 검토되고 있다.

---

## 9. MRSW와 동시성 제어

### Multi-Reader Single-Writer

Content Store는 **MRSW(Multi-Reader Single-Writer)** 모델을 따른다:
- 커밋된 블롭은 **여러 goroutine이 동시에 읽기** 가능 (ReaderAt)
- 진행 중인 쓰기(ingest)는 **하나의 Writer만** 활성화 가능

### 락 메커니즘

```go
// 소스: plugins/content/local/store.go:73-74

locksMu sync.Mutex
locks   map[string]*lock
```

```go
// Writer 생성 시 락 획득
func (s *store) Writer(ctx context.Context, opts ...content.WriterOpt) (content.Writer, error) {
    // ...
    if err := s.tryLock(wOpts.Ref); err != nil {
        return nil, err  // 이미 다른 Writer가 활성
    }

    w, err := s.writer(ctx, wOpts.Ref, wOpts.Desc.Size, wOpts.Desc.Digest)
    if err != nil {
        s.unlock(wOpts.Ref)  // 실패 시 즉시 해제
        return nil, err
    }
    return w, nil  // 성공: 락은 Writer가 보유
}
```

Writer의 `Commit()` 또는 `Close()`에서 `s.unlock(wOpts.Ref)`가 호출되어 락이 해제된다.

### 왜 파일 락이 아닌 메모리 락인가

- **성능** -- 파일 락(flock)보다 빠름
- **정리** -- 프로세스 크래시 시 파일 락은 잔존할 수 있지만, 메모리 락은 자동 정리
- **범위** -- 같은 containerd 프로세스 내에서만 동시성 보호 필요 (외부 프로세스는 Store API를 사용)

### bufPool: 버퍼 풀

```go
// 소스: plugins/content/local/store.go:43-47

var bufPool = sync.Pool{
    New: func() interface{} {
        buffer := make([]byte, 1<<20)  // 1MB 버퍼
        return &buffer
    },
}
```

1MB 크기의 버퍼를 `sync.Pool`로 재사용한다. 대용량 블롭 복사 시 매번 버퍼를 할당하는 GC 부담을 줄인다.

---

## 10. Ingest 흐름: 데이터 쓰기 과정

### 새 Writer 생성 흐름

```go
// 소스: plugins/content/local/store.go:533-624 (writer 내부 함수)

func (s *store) writer(ctx context.Context, ref string, total int64, expected digest.Digest) (content.Writer, error) {
    // 1. 이미 커밋된 콘텐츠인지 확인
    if expected != "" {
        p, _ := s.blobPath(expected)
        if _, err := os.Stat(p); err == nil {
            return nil, errdefs.ErrAlreadyExists  // 이미 존재
        }
    }

    // 2. ingest 디렉토리 생성
    path, refp, data := s.ingestPaths(ref)
    digester := digest.Canonical.Digester()

    if err := os.Mkdir(path, 0755); err != nil {
        if os.IsExist(err) {
            // 기존 ingest가 있으면 재개 시도
            status, err := s.resumeStatus(ref, total, digester)
            // ...
        }
    }

    // 3. 새 ingest인 경우 메타파일 생성
    os.WriteFile(refp, []byte(ref), 0666)            // ref 파일
    writeTimestampFile(filepath.Join(path, "startedat"), startedAt)
    writeTimestampFile(filepath.Join(path, "updatedat"), startedAt)
    if total > 0 {
        os.WriteFile(filepath.Join(path, "total"), []byte(fmt.Sprint(total)), 0666)
    }

    // 4. 데이터 파일 열기
    fp, _ := os.OpenFile(data, os.O_WRONLY|os.O_CREATE, 0666)
    fp.Seek(offset, io.SeekStart)

    // 5. Writer 반환
    return &writer{
        s: s, fp: fp, ref: ref, path: path,
        offset: offset, total: total, digester: digester,
        startedAt: startedAt, updatedAt: updatedAt,
    }, nil
}
```

### 전체 쓰기 시퀀스

```
클라이언트                     Content Store (local)
  |                              |
  |  Writer(WithRef("my-layer")) |
  |----------------------------->|
  |                              |  tryLock("my-layer")
  |                              |  mkdir ingest/{ref-hash}/
  |                              |  write ref, startedat, updatedat
  |                              |  open ingest/{ref-hash}/data
  |  <writer>                    |
  |<-----------------------------|
  |                              |
  |  Write(chunk1)               |
  |----------------------------->|  fp.Write(chunk1)
  |                              |  digester.Hash().Write(chunk1)
  |  Write(chunk2)               |
  |----------------------------->|  fp.Write(chunk2)
  |                              |  digester.Hash().Write(chunk2)
  |                              |
  |  Commit(size, digest)        |
  |----------------------------->|  fp.Sync() + Close()
  |                              |  크기 검증
  |                              |  다이제스트 검증
  |                              |  Rename(data -> blobs/sha256/{dgst})
  |                              |  Chmod(0444)
  |                              |  RemoveAll(ingest/{ref-hash}/)
  |                              |  unlock("my-layer")
  |  nil (성공)                   |
  |<-----------------------------|
```

---

## 11. Label Store 활용

### LabelStore 인터페이스

```go
// 소스: plugins/content/local/store.go:51-61

type LabelStore interface {
    Get(digest.Digest) (map[string]string, error)
    Set(digest.Digest, map[string]string) error
    Update(digest.Digest, map[string]string) (map[string]string, error)
}
```

라벨은 블롭의 **가변 메타데이터**이다. 블롭 자체(파일)는 불변이지만, 라벨은 변경 가능하다.

### 주요 라벨 용도

| 라벨 키 | 용도 |
|---------|------|
| `containerd.io/gc.ref.content.config` | GC 참조: 설정 블롭 |
| `containerd.io/gc.ref.content.l.*` | GC 참조: 레이어 블롭 |
| `containerd.io/gc.ref.content.m.*` | GC 참조: 매니페스트 |
| `containerd.io/gc.ref.snapshot.{snapshotter}` | GC 참조: 스냅샷 |
| `containerd.io/uncompressed` | 비압축 레이어의 diffID |
| `containerd.io/distribution.source.*` | 레지스트리 출처 |

### Update 메서드의 필드 경로 기반 업데이트

```go
// 소스: plugins/content/local/store.go:181-246

func (s *store) Update(ctx context.Context, info content.Info, fieldpaths ...string) (content.Info, error) {
    // fieldpaths가 비어있으면 전체 라벨 교체
    // "labels.key" 형태면 특정 키만 업데이트
    // "labels" 면 전체 라벨 교체
    for _, path := range fieldpaths {
        if strings.HasPrefix(path, "labels.") {
            key := strings.TrimPrefix(path, "labels.")
            labels[key] = info.Labels[key]
        }
        switch path {
        case "labels":
            all = true
            labels = info.Labels
        }
    }
}
```

필드 경로(fieldpath) 기반의 부분 업데이트를 지원한다. 이는 GC가 라벨을 통해 참조를 관리할 때 레이스 컨디션 없이 특정 라벨만 업데이트할 수 있게 한다.

---

## 12. Helper 함수들: Copy, WriteBlob, ReadBlob

### ReadBlob: 작은 블롭 전체 읽기

```go
// 소스: core/content/helpers.go:84-106

func ReadBlob(ctx context.Context, provider Provider, desc ocispec.Descriptor) ([]byte, error) {
    // 디스크립터에 data 필드가 있으면 바로 반환 (인라인 데이터)
    if int64(len(desc.Data)) == desc.Size && digest.FromBytes(desc.Data) == desc.Digest {
        return desc.Data, nil
    }
    // Content Store에서 읽기
    ra, err := provider.ReaderAt(ctx, desc)
    defer ra.Close()
    p := make([]byte, ra.Size())
    n, err := ra.ReadAt(p, 0)
    return p, err
}
```

매니페스트, 설정 등 작은 블롭을 읽을 때 사용한다. 레이어 같은 대용량 블롭에는 부적합하다.

### WriteBlob: 다이제스트 기반 쓰기

```go
// 소스: core/content/helpers.go:115-127

func WriteBlob(ctx context.Context, cs Ingester, ref string, r io.Reader, desc ocispec.Descriptor, opts ...Opt) error {
    cw, err := OpenWriter(ctx, cs, WithRef(ref), WithDescriptor(desc))
    if err != nil {
        if !errdefs.IsAlreadyExists(err) {
            return err
        }
        return nil  // 이미 존재하면 성공으로 처리
    }
    defer cw.Close()
    return Copy(ctx, cw, r, desc.Size, desc.Digest, opts...)
}
```

### Copy: 버퍼링된 복사 + 커밋

```go
// 소스: core/content/helpers.go:173-215

func Copy(ctx context.Context, cw Writer, or io.Reader, size int64, expected digest.Digest, opts ...Opt) error {
    r := or
    for i := 0; ; i++ {
        ws, err := cw.Status()
        // 재개 시 offset 위치로 reader 이동
        if ws.Offset > 0 || i > 0 {
            r, err = seekReader(or, ws.Offset, size)
        }
        copied, err := copyWithBuffer(cw, r)
        if errors.Is(err, ErrReset) {
            continue  // Reset 에러면 처음부터 재시도
        }
        // 크기 검증
        if size != 0 && copied < size-ws.Offset {
            return io.ErrUnexpectedEOF
        }
        // 커밋
        if err := cw.Commit(ctx, size, expected, opts...); err != nil {
            if errors.Is(err, ErrReset) {
                continue  // Reset 에러면 재시도
            }
        }
        return nil
    }
}
```

### OpenWriter: 재시도 로직

```go
// 소스: core/content/helpers.go:131-164

func OpenWriter(ctx context.Context, cs Ingester, opts ...WriterOpt) (Writer, error) {
    var retry = 16
    for {
        cw, err = cs.Writer(ctx, opts...)
        if err != nil {
            if !errdefs.IsUnavailable(err) {
                return nil, err
            }
            // 다른 Writer가 활성 중이면 지수 백오프로 재시도
            select {
            case <-time.After(time.Millisecond * time.Duration(randutil.Intn(retry))):
                if retry < 2048 {
                    retry = retry << 1  // 지수 증가
                }
                continue
            case <-ctx.Done():
                return nil, err
            }
        }
        break
    }
    return cw, err
}
```

**지수 백오프(Exponential Backoff):** 초기 16ms에서 최대 2048ms까지 대기 시간을 두 배씩 증가시킨다. 여러 goroutine이 같은 ref에 Writer를 요청할 때 경쟁을 완화한다.

### copyWithBuffer: 최적화된 복사

```go
// 소스: core/content/helpers.go:303-342

func copyWithBuffer(dst io.Writer, src io.Reader) (written int64, err error) {
    // WriterTo/ReaderFrom 인터페이스가 있으면 활용 (zero-copy 가능)
    if wt, ok := src.(io.WriterTo); ok {
        return wt.WriteTo(dst)
    }
    if rt, ok := dst.(io.ReaderFrom); ok {
        return rt.ReadFrom(src)
    }
    // 1MB 풀 버퍼 사용
    bufRef := bufPool.Get().(*[]byte)
    defer bufPool.Put(bufRef)
    buf := *bufRef
    for {
        // ReadAtLeast로 버퍼를 최대한 채운 후 한 번에 쓰기
        nr, er := io.ReadAtLeast(src, buf, len(buf))
        if nr > 0 {
            nw, ew := dst.Write(buf[0:nr])
            // ...
        }
    }
}
```

`io.ReadAtLeast`를 사용하여 **버퍼를 최대한 채운 후** 쓰기한다. 이는 작은 단위의 Write 호출을 줄여 Writer 측의 오버헤드(해시 계산, 파일 I/O)를 최소화한다.

---

## 13. Walk와 필터링

### Walk: 전체 블롭 순회

```go
// 소스: plugins/content/local/store.go:248-306

func (s *store) Walk(ctx context.Context, fn content.WalkFunc, fs ...string) error {
    root := filepath.Join(s.root, "blobs")
    filter, err := filters.ParseAll(fs...)

    var alg digest.Algorithm
    return filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
        // 1단계: blobs/ 직하의 디렉토리명 = 알고리즘 (sha256 등)
        if filepath.Dir(path) == root {
            alg = digest.Algorithm(filepath.Base(path))
            if !alg.Available() {
                return filepath.SkipDir  // 미지원 알고리즘은 건너뜀
            }
            return nil
        }
        // 2단계: 알고리즘 디렉토리 내 파일명 = 인코딩된 해시
        dgst := digest.NewDigestFromEncoded(alg, filepath.Base(path))
        // 라벨 조회 및 Info 구성
        var labels map[string]string
        if s.ls != nil {
            labels, _ = s.ls.Get(dgst)
        }
        info := s.info(dgst, fi, labels)
        // 필터 적용
        if !filter.Match(content.AdaptInfo(info)) {
            return nil
        }
        return fn(info)
    })
}
```

파일시스템 순회로 구현되므로 블롭이 많을수록 느려진다. GC에서 주로 사용되며, 일반적인 블롭 접근은 다이제스트 기반 직접 접근을 사용한다.

---

## 14. 설계 철학과 GC 연동

### Content Store와 GC의 관계

```
Image A --> Config (sha256:aaa)
        --> Layer1 (sha256:bbb)
        --> Layer2 (sha256:ccc)

Image B --> Config (sha256:ddd)
        --> Layer1 (sha256:bbb)  <-- Image A와 공유!
        --> Layer3 (sha256:eee)

Image A 삭제 시:
  - sha256:aaa 삭제 가능 (A만 참조)
  - sha256:bbb 삭제 불가 (B도 참조)
  - sha256:ccc 삭제 가능 (A만 참조)
```

Content Store 자체는 참조 카운팅을 하지 않는다. 대신:
1. **라벨**로 블롭 간 참조 관계를 표현
2. **GC 플러그인**이 라벨을 기반으로 도달 가능성(reachability) 분석
3. 도달 불가능한 블롭을 `Delete()`로 삭제

### 핵심 설계 원칙

1. **단순한 저장소** -- Content Store는 블롭을 저장/읽기만 한다. 참조 관리는 외부(GC, 라벨)에 위임
2. **원자적 커밋** -- Rename 기반으로 부분 기록이 보이지 않음
3. **재개 가능성** -- 네트워크 중단에 강건한 이미지 pull
4. **검증 내장** -- 모든 쓰기에 해시 검증이 내재
5. **불변 데이터 + 가변 메타데이터** -- 블롭은 불변, 라벨은 가변으로 분리

이 설계는 OCI 이미지 스펙의 CAS 기반 참조 모델과 완벽하게 일치하며, containerd가 다양한 스토리지 백엔드를 지원하면서도 일관된 무결성을 보장할 수 있게 한다.
