# PoC-03: Content-Addressable Storage

## 목적

containerd의 Content Store를 시뮬레이션한다. Content Store는 OCI 이미지의 모든 구성 요소(manifest, config, layers)를 SHA256 digest를 키로 저장하는 불변 blob 저장소이다.

1. **Content Store 파일시스템 구조** — `blobs/sha256/{digest}` + `ingest/{hash(ref)}/`
2. **Writer 생명주기** — `Writer(ref)` -> `Write(data)` -> `Commit(size, expected)`
3. **SHA256 digest 계산 및 검증** — 실시간 해시 누적, Commit 시 검증
4. **ReaderAt** — digest로 blob 직접 읽기
5. **중복 제거** — 같은 digest는 한 번만 저장
6. **동시성 제어** — ref 기반 잠금으로 동시 쓰기 방지

## 핵심 개념

### Content-Addressable Storage

데이터의 내용(content)이 곧 주소(address)가 되는 저장 방식:
- 데이터 -> SHA256 해시 -> `sha256:{hex}` 형식의 digest
- 같은 데이터는 항상 같은 digest -> 자동 중복 제거
- digest로 무결성 검증 내장 (전송 중 변조 감지)

### Writer 생명주기 (핵심 알고리즘)

```
1. Writer(ref) 호출
   - tryLock(ref): 동시 쓰기 방지
   - expected digest가 이미 존재하면 즉시 거부 (ErrAlreadyExists)
   - ingest/{hash(ref)}/ 디렉토리 생성
   - ref, startedat, updatedat, total 파일 작성
   - data 파일 열기

2. Write(data) 반복 호출
   - fp.Write(data): 파일에 쓰기
   - digester.Hash().Write(data): SHA256 해시 누적 (별도 패스 불필요!)
   - offset 갱신

3. Commit(size, expected) 호출
   - fp.Sync(): 디스크 플러시
   - 크기 검증: size > 0 && size != actual → 에러
   - digest 검증: expected != "" && expected != computed → 에러
   - os.Stat(target): 이미 존재하면 ErrAlreadyExists (중복 제거)
   - os.Rename(ingest/data → blobs/sha256/{digest}): 원자적 이동
   - os.Chmod(0444): 읽기 전용
   - os.RemoveAll(ingest/): 정리
```

### 파일시스템 구조

```
root/
├── blobs/
│   └── sha256/
│       ├── a1b2c3...   ← 커밋된 blob (0444, readonly)
│       ├── d4e5f6...
│       └── ...
└── ingest/
    └── {hash(ref)}/    ← 진행 중인 쓰기
        ├── ref         ← 참조 이름 문자열
        ├── data        ← 실제 데이터
        ├── startedat   ← RFC3339 타임스탬프
        ├── updatedat
        └── total       ← 예상 크기 (선택)
```

## 실제 소스 참조

| PoC 구현 | 실제 소스 경로 | 설명 |
|----------|---------------|------|
| `Store` 구조체 | `plugins/content/local/store.go:68` | `store{root, ls, integritySupported, locks}` |
| `NewStore()` | `plugins/content/local/store.go:79` | root 디렉토리 생성, fsverity 지원 확인 |
| `blobPath()` | `plugins/content/local/store.go:641` | `root/blobs/{algorithm}/{encoded}` |
| `ingestRoot()` | `plugins/content/local/store.go:649` | `root/ingest/{digest(ref).Encoded()}` |
| `ingestPaths()` | `plugins/content/local/store.go:661` | root, ref, data 경로 반환 |
| `Writer()` | `plugins/content/local/store.go:475` | ref 잠금 -> writer 생성 -> ingest 디렉토리 |
| `writer.Write()` | `plugins/content/local/writer.go:71` | `fp.Write + digester.Hash().Write + offset` |
| `writer.Commit()` | `plugins/content/local/writer.go:79` | Sync -> 크기검증 -> digest검증 -> Rename -> Chmod |
| `writer.Digest()` | `plugins/content/local/writer.go:63` | `digester.Digest()` 현재 해시 |
| `writer.Close()` | `plugins/content/local/writer.go:198` | Sync + Close + unlock (resume 가능) |
| `ReaderAt()` | `plugins/content/local/store.go:146` | `OpenReader(blobPath(desc.Digest))` |
| `Info()` | `plugins/content/local/store.go:111` | `os.Stat(blobPath) -> Info` |
| `Walk()` | `plugins/content/local/store.go:248` | `filepath.Walk(blobs/)` |
| `Delete()` | `plugins/content/local/store.go:164` | `os.RemoveAll(blobPath)` |
| `Abort()` | `plugins/content/local/store.go:628` | `os.RemoveAll(ingestRoot)` |
| `ListStatuses()` | `plugins/content/local/store.go:312` | ingest 디렉토리 순회 |
| `content.Store` 인터페이스 | `core/content/content.go:41` | Manager + Provider + IngestManager + Ingester |
| `content.Writer` 인터페이스 | `core/content/content.go:146` | WriteCloser + Digest + Commit + Status + Truncate |
| `content.ReaderAt` 인터페이스 | `core/content/content.go:49` | io.ReaderAt + io.Closer + Size |

## 실행 방법

```bash
cd containerd_EDU/poc-03-content-store
go run main.go
```

## 예상 출력

```
======================================================================
containerd Content-Addressable Storage 시뮬레이션
======================================================================

[1] Content Store 파일시스템 구조
  root: /tmp/containerd-content-xxx
  ...

[2] Blob 쓰기 — Writer 생명주기
  [2a] Config blob 쓰기 (ref='config-upload')
    Writer 상태 (쓰기 전): offset=0, total=91
    Writer 상태 (쓰기 후): offset=91, digest=sha256:...
    Committed: sha256:... (91 bytes)
  [2b] Layer blob 쓰기 ...
  [2c] Manifest blob 쓰기 ...

[3] Blob 읽기 — ReaderAt
  Config: size=91, digest=sha256:...
  Layer: size=84, digest=sha256:...
  Manifest: size=xxx, digest=sha256:...

[6] 중복 제거 (Deduplication)
  → Writer 생성 거부: content sha256:...: already exists

[7] Digest 검증 실패 시나리오
  Commit 거부: digest 불일치: expected=sha256:000..., actual=sha256:...

[11] 동시 쓰기 잠금 (ref 기반)
  두 번째 Writer 거부: ref "concurrent-ref" is already locked
  잠금 해제 후 재시도: 성공
```
