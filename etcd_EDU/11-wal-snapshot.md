# 11. WAL & 스냅샷 Deep Dive

## 개요

etcd의 내구성(durability)은 **WAL(Write-Ahead Log)**과 **스냅샷(Snapshot)** 두 메커니즘에 의해 보장된다. 모든 상태 변경은 먼저 WAL에 기록된 후 메모리와 BoltDB에 반영된다. 스냅샷은 특정 시점의 전체 상태를 캡처하여 WAL 파일이 무한히 증가하는 것을 방지한다.

```
쓰기 요청 흐름:

  클라이언트 요청
       │
       ▼
  Raft 합의 (리더 → 팔로워)
       │
       ▼
  ┌──────────┐     ┌──────────┐
  │   WAL    │────>│ BoltDB   │
  │ (디스크)  │     │ (디스크)  │
  └──────────┘     └──────────┘
       │
       ▼
  클라이언트 응답

  장애 복구:

  ┌──────────┐     ┌──────────┐
  │ Snapshot  │ +  │   WAL    │ → 전체 상태 복원
  │ (기준점)  │     │ (이후 로그)│
  └──────────┘     └──────────┘
```

소스 경로: `server/storage/wal/`, `server/etcdserver/api/snap/`

---

## 1. WAL 구조체

### 1.1 핵심 구조체 정의

```
경로: server/storage/wal/wal.go (72~95행)
```

```go
type WAL struct {
    lg *zap.Logger

    dir     string          // WAL 파일 디렉토리
    dirFile *os.File        // 디렉토리 fd (Rename 시 sync용)

    metadata []byte           // 각 WAL 파일 헤더에 기록되는 메타데이터
    state    raftpb.HardState // WAL 헤더에 기록되는 하드 스테이트

    start     walpb.Snapshot  // 읽기 시작 지점
    decoder   Decoder         // 레코드 디코더 (읽기 모드)
    readClose func() error    // 디코더 리더 닫기 함수

    unsafeNoSync bool         // 테스트용: fsync 생략

    mu      sync.Mutex
    enti    uint64            // 마지막으로 저장된 엔트리 인덱스
    encoder *encoder          // 레코드 인코더 (쓰기 모드)

    locks []*fileutil.LockedFile  // 잠긴 WAL 파일들 (이름 오름차순)
    fp    *filePipeline           // 비동기 파일 사전 할당
}
```

**읽기 모드 vs 쓰기 모드:**

WAL은 동시에 읽기와 쓰기 모드를 가질 수 없다. 새로 생성된 WAL은 쓰기 모드이고, 열린 WAL은 읽기 모드이다. 모든 이전 레코드를 읽은 후에야 쓰기 모드로 전환된다.

```
WAL 라이프사이클:

  Create() → 쓰기 모드 (encoder 활성)
  Open()   → 읽기 모드 (decoder 활성)
  ReadAll() → 모든 레코드 읽기 → 쓰기 모드 전환
```

### 1.2 주요 상수와 변수

```go
// server/storage/wal/wal.go (38~65행)
const (
    MetadataType int64 = iota + 1  // 1: 메타데이터
    EntryType                       // 2: Raft 엔트리
    StateType                       // 3: HardState
    CrcType                         // 4: CRC 체크섬
    SnapshotType                    // 5: 스냅샷 마커

    warnSyncDuration = time.Second  // fsync 경고 임계값
)

var (
    SegmentSizeBytes int64 = 64 * 1000 * 1000  // 64MB
    crcTable = crc32.MakeTable(crc32.Castagnoli)
)
```

---

## 2. WAL 레코드 타입

### 2.1 5가지 레코드 타입

```
WAL 파일 내부 구조:

┌──────────────────────────────────────────────────┐
│  CrcType       │ 이전 파일의 CRC 체인 연결        │
├──────────────────────────────────────────────────┤
│  MetadataType  │ 클러스터/노드 메타데이터          │
├──────────────────────────────────────────────────┤
│  SnapshotType  │ 스냅샷 인덱스/텀 마커            │
├──────────────────────────────────────────────────┤
│  EntryType     │ Raft 로그 엔트리 (실제 데이터)   │
│  EntryType     │                                  │
│  EntryType     │                                  │
│  ...           │                                  │
├──────────────────────────────────────────────────┤
│  StateType     │ HardState (term, vote, commit)   │
├──────────────────────────────────────────────────┤
│  EntryType     │ 새로운 엔트리들                   │
│  ...           │                                  │
├──────────────────────────────────────────────────┤
│  StateType     │ 업데이트된 HardState             │
├──────────────────────────────────────────────────┤
│  (빈 공간)     │ 사전 할당된 미사용 영역           │
└──────────────────────────────────────────────────┘
```

| 타입 | 값 | 용도 | 기록 시점 |
|------|-----|------|----------|
| MetadataType | 1 | 클러스터 ID, 노드 ID 등 | WAL 파일 생성/세그먼트 절단 시 |
| EntryType | 2 | Raft 로그 엔트리 (제안, 설정변경) | Save() 호출 시 |
| StateType | 3 | HardState (term, vote, commit) | Save() 호출 시 |
| CrcType | 4 | 이전 파일의 CRC 체인 | 새 세그먼트 시작 시 |
| SnapshotType | 5 | 스냅샷 인덱스/텀 참조 | SaveSnapshot() 호출 시 |

### 2.2 레코드 형식

각 레코드는 `walpb.Record` protobuf로 직렬화된다:

```
레코드 디스크 레이아웃:

┌──────────────┬──────────────────────────────────┐
│ 8 bytes      │ 가변 길이                         │
│              │                                    │
│ lenField     │ Record (protobuf 직렬화)          │
│ (데이터 길이  │ + 패딩 (8바이트 정렬)             │
│  + 패딩 정보) │                                   │
└──────────────┴──────────────────────────────────┘

lenField 구조 (8 bytes):
  하위 56비트: 데이터 바이트 수
  상위 8비트:  패딩 정보 (MSB=1이면 패딩 있음, 하위 3비트=패딩 크기)
```

**왜 8바이트 정렬인가?**

디스크 섹터 경계를 고려한 것이다. lenField가 torn write(불완전 쓰기)로 손상되는 것을 방지한다. 8바이트 정렬이면 lenField 자체가 한 번의 원자적 쓰기에 포함된다.

---

## 3. Create(): WAL 생성

```
경로: server/storage/wal/wal.go (100~235행)
```

WAL 생성 과정은 원자성을 보장하기 위해 **임시 디렉토리 패턴**을 사용한다:

```
WAL 생성 흐름:

1. 임시 디렉토리 생성: {dirpath}.tmp/
2. 첫 WAL 파일 생성:  {dirpath}.tmp/0000000000000000-0000000000000000.wal
3. 파일 사전 할당:     64MB
4. 초기 레코드 기록:
   - CrcType (prevCrc=0)
   - MetadataType (metadata)
   - SnapshotType (index=0, term=0)
5. 임시 → 최종 디렉토리 rename (원자적)
6. 부모 디렉토리 fsync

┌──────────────────────────────────────┐
│  {dir}.tmp/                           │
│  ┌──────────────────────────┐        │
│  │ 0000...0000.wal          │        │
│  │  CRC(0)                  │        │
│  │  Metadata(clusterID)     │        │
│  │  Snapshot(index=0,term=0)│        │
│  └──────────────────────────┘        │
└──────────────┬───────────────────────┘
               │ os.Rename (원자적)
               ▼
┌──────────────────────────────────────┐
│  {dir}/                               │
│  ┌──────────────────────────┐        │
│  │ 0000...0000.wal          │        │
│  └──────────────────────────┘        │
└──────────────────────────────────────┘
```

**왜 임시 디렉토리를 사용하는가?**

WAL 초기화 중 프로세스가 크래시하면 불완전한 WAL이 남을 수 있다. 임시 디렉토리에서 작업한 후 원자적으로 rename하면, WAL 디렉토리는 항상 **완전하거나 존재하지 않거나** 둘 중 하나이다.

---

## 4. Save(): 엔트리 저장

```
경로: server/storage/wal/wal.go (956~992행)
```

```go
func (w *WAL) Save(st raftpb.HardState, ents []raftpb.Entry) error {
    w.mu.Lock()
    defer w.mu.Unlock()

    // 변경 없으면 sync 생략
    if raft.IsEmptyHardState(st) && len(ents) == 0 {
        return nil
    }

    mustSync := raft.MustSync(st, w.state, len(ents))

    // 엔트리 저장
    for i := range ents {
        if err := w.saveEntry(&ents[i]); err != nil {
            return err
        }
    }
    // HardState 저장
    if err := w.saveState(&st); err != nil {
        return err
    }

    // 현재 파일 오프셋 확인
    curOff, err := w.tail().Seek(0, io.SeekCurrent)
    if err != nil {
        return err
    }

    if curOff < SegmentSizeBytes {
        if mustSync {
            return w.sync()
        }
        return nil
    }

    // 세그먼트 크기 초과 → 새 세그먼트 생성
    return w.cut()
}
```

**Save 흐름도:**

```
Save(HardState, []Entry)
    │
    ├── 빈 데이터? → return nil
    │
    ├── mustSync 판단
    │   (term 변경, vote 변경, 새 엔트리 → true)
    │
    ├── 각 엔트리 → saveEntry()
    │   └── protobuf 직렬화 → encoder.encode()
    │
    ├── HardState → saveState()
    │   └── 빈 상태가 아니면 직렬화 → encoder.encode()
    │
    └── 현재 파일 크기 확인
        │
        ├── < 64MB
        │   ├── mustSync? → w.sync() (fdatasync)
        │   └── !mustSync → return nil
        │
        └── >= 64MB → w.cut() (새 세그먼트)
```

**mustSync의 의미:**

Raft 프로토콜에서는 특정 상황에서 반드시 디스크에 기록이 완료되어야 한다:
- Term이 변경됨 (리더 선출)
- Vote가 변경됨 (투표)
- 새로운 엔트리가 있음

이외의 경우 (예: commit 인덱스만 업데이트)에는 fsync를 생략하여 성능을 향상시킨다.

---

## 5. cut(): 세그먼트 절단

```
경로: server/storage/wal/wal.go (746~828행)
```

```go
func (w *WAL) cut() error {
    // 1. 현재 파일 truncate (미사용 공간 제거)
    off, serr := w.tail().Seek(0, io.SeekCurrent)
    w.tail().Truncate(off)
    w.sync()

    // 2. 새 파일 이름 결정
    fpath := filepath.Join(w.dir, walName(w.seq()+1, w.enti+1))

    // 3. filePipeline에서 사전 할당된 파일 가져오기
    newTail, err := w.fp.Open()

    // 4. 새 파일에 헤더 기록
    w.locks = append(w.locks, newTail)
    prevCrc := w.encoder.crc.Sum32()
    w.encoder = newFileEncoder(w.tail().File, prevCrc)

    w.saveCrc(prevCrc)                        // CRC 체인
    w.encoder.encode(MetadataType, w.metadata) // 메타데이터
    w.saveState(&w.state)                      // HardState
    w.sync()

    // 5. 임시 파일을 최종 이름으로 rename
    os.Rename(newTail.Name(), fpath)
    fileutil.Fsync(w.dirFile)

    // 6. 새 이름으로 파일 재열기
    newTail.Close()
    newTail = fileutil.LockFile(fpath, os.O_WRONLY, ...)
    w.locks[len(w.locks)-1] = newTail

    return nil
}
```

**세그먼트 파일 명명 규칙:**

```
형식: {seq:016x}-{index:016x}.wal

예시:
  0000000000000000-0000000000000000.wal  (첫 번째 세그먼트)
  0000000000000001-0000000000001234.wal  (seq=1, 시작 index=0x1234)
  0000000000000002-000000000000abcd.wal  (seq=2, 시작 index=0xabcd)

seq:   세그먼트 순서 번호 (단조 증가)
index: 이 세그먼트에 포함된 첫 엔트리의 인덱스
```

**CRC 체인의 의미:**

```
파일 1:                    파일 2:
┌────────────────────┐    ┌────────────────────┐
│ CrcType(prevCrc=0) │    │ CrcType(prevCrc=X) │ ← 파일1의 최종 CRC
│ 레코드들...        │    │ 레코드들...        │
│ 최종 CRC: X        │───>│ ...               │
└────────────────────┘    └────────────────────┘
```

이전 파일의 최종 CRC를 다음 파일의 시작에 기록함으로써, 파일 경계를 넘는 데이터 무결성 체인을 형성한다.

---

## 6. 파일 파이프라인 (file_pipeline.go)

### 6.1 구조체

```
경로: server/storage/wal/file_pipeline.go (28~41행)
```

```go
type filePipeline struct {
    lg    *zap.Logger
    dir   string
    size  int64       // 사전 할당 크기 (64MB)
    count int         // 생성된 파일 수

    filec chan *fileutil.LockedFile  // 준비된 파일 전달 채널
    errc  chan error                  // 에러 전달 채널
    donec chan struct{}               // 종료 시그널
}
```

### 6.2 동작 원리

```
경로: server/storage/wal/file_pipeline.go (90~106행)
```

```go
func (fp *filePipeline) run() {
    defer close(fp.errc)
    for {
        f, err := fp.alloc()  // 64MB 파일 사전 할당
        if err != nil {
            fp.errc <- err
            return
        }
        select {
        case fp.filec <- f:   // 준비된 파일 전달
        case <-fp.donec:      // 종료
            os.Remove(f.Name())
            f.Close()
            return
        }
    }
}
```

**왜 파일 파이프라인이 필요한가?**

```
파이프라인 없이:
  cut() 호출 → 새 파일 생성(64MB 할당) → 쓰기 시작
  (파일 생성/할당에 수십~수백 ms 소요 → 쓰기 지연)

파이프라인 사용:
  백그라운드: 미리 64MB 파일 생성해두기
  cut() 호출 → 채널에서 준비된 파일 수령 → 즉시 쓰기 시작
  (파일 생성 오버헤드 제거)
```

### 6.3 alloc(): 파일 사전 할당

```
경로: server/storage/wal/file_pipeline.go (75~88행)
```

```go
func (fp *filePipeline) alloc() (f *fileutil.LockedFile, err error) {
    fpath := filepath.Join(fp.dir, fmt.Sprintf("%d.tmp", fp.count%2))
    if f, err = createNewWALFile[*fileutil.LockedFile](fpath, false); err != nil {
        return nil, err
    }
    if err = fileutil.Preallocate(f.File, fp.size, true); err != nil {
        f.Close()
        return nil, err
    }
    fp.count++
    return f, nil
}
```

**`fp.count%2`의 의미:**

임시 파일 이름을 `0.tmp`와 `1.tmp` 두 개만 번갈아 사용한다. 이전 임시 파일이 rename되기 전에 새 임시 파일을 만들면 충돌하므로 두 개를 교대로 사용한다.

---

## 7. Encoder: CRC32 체크섬과 레코드 인코딩

### 7.1 encoder 구조체

```
경로: server/storage/wal/encoder.go (36~52행)
```

```go
type encoder struct {
    mu sync.Mutex
    bw *ioutil.PageWriter

    crc       hash.Hash32      // CRC32-Castagnoli
    buf       []byte            // 1MB 직렬화 버퍼
    uint64buf []byte            // 8바이트 길이 필드 버퍼
}

func newEncoder(w io.Writer, prevCrc uint32, pageOffset int) *encoder {
    return &encoder{
        bw:        ioutil.NewPageWriter(w, walPageBytes, pageOffset),
        crc:       crc.New(prevCrc, crcTable),
        buf:       make([]byte, 1024*1024),  // 1MB
        uint64buf: make([]byte, 8),
    }
}
```

### 7.2 encode() 메서드

```
경로: server/storage/wal/encoder.go (64~96행)
```

```go
func (e *encoder) encode(rec *walpb.Record) error {
    e.mu.Lock()
    defer e.mu.Unlock()

    // CRC 계산
    e.crc.Write(rec.Data)
    rec.Crc = new(e.crc.Sum32())

    // 직렬화 (1MB 미만은 사전 할당 버퍼 사용)
    var data []byte
    if rec.Size() > len(e.buf) {
        data, _ = rec.Marshal()
    } else {
        n, _ := rec.MarshalTo(e.buf)
        data = e.buf[:n]
    }

    // 8바이트 정렬 패딩
    data, lenField := prepareDataWithPadding(data)

    return write(e.bw, e.uint64buf, data, lenField)
}
```

**PageWriter의 역할:**

```
walPageBytes = 8 * minSectorSize = 8 * 512 = 4096 bytes
```

`PageWriter`는 4KB 페이지 경계에 맞춰 쓰기를 수행한다. 이는 디스크 섹터와 정렬하여 torn write를 방지하고 I/O 효율성을 높인다.

### 7.3 8바이트 정렬과 lenField 인코딩

```
경로: server/storage/wal/encoder.go (98~106행)
```

```go
func encodeFrameSize(dataBytes int) (lenField uint64, padBytes int) {
    lenField = uint64(dataBytes)
    padBytes = (8 - (dataBytes % 8)) % 8
    if padBytes != 0 {
        lenField |= uint64(0x80|padBytes) << 56
    }
    return lenField, padBytes
}
```

```
lenField 비트 레이아웃 (64비트):

 63  62  61  60  59  58  57  56  55 ... 0
┌───┬───┬───┬───┬───┬───┬───┬───┬────────────┐
│pad│ 0 │ 0 │ 0 │ 0 │p2 │p1 │p0 │  dataBytes │
│flg│   │   │   │   │   │   │   │  (56비트)   │
└───┴───┴───┴───┴───┴───┴───┴───┴────────────┘

pad flag = 1: 패딩 있음
p2:p1:p0: 패딩 바이트 수 (1~7)
dataBytes: 실제 데이터 크기
```

---

## 8. Decoder: 레코드 디코딩

### 8.1 decoder 구조체

```
경로: server/storage/wal/decoder.go (44~56행)
```

```go
type decoder struct {
    mu             sync.Mutex
    brs            []*fileutil.FileBufReader  // 여러 WAL 파일 리더

    lastValidOff   int64       // 마지막 유효 레코드 이후 오프셋
    crc            hash.Hash32 // CRC 검증용

    continueOnCrcError bool    // 도구용: CRC 에러 시 계속 진행
}
```

### 8.2 decodeRecord() 메서드

```
경로: server/storage/wal/decoder.go (86~153행)
```

디코딩 흐름:

```
decodeRecord()
    │
    ├── brs가 비어있으면 → EOF
    │
    ├── readInt64() → lenField 읽기
    │   ├── EOF → 다음 파일로 전환
    │   └── lenField == 0 → 사전 할당 영역 도달 → 다음 파일
    │
    ├── decodeFrameSize(lenField) → recBytes, padBytes
    │
    ├── 크기 검증: recBytes > 파일 남은 크기?
    │   └── Yes → ErrUnexpectedEOF
    │
    ├── io.ReadFull(data) → 레코드 + 패딩 읽기
    │
    ├── rec.Unmarshal(data[:recBytes])
    │   └── 실패 시 isTornEntry() 체크
    │
    ├── CRC 검증 (CrcType이 아닌 경우)
    │   ├── d.crc.Write(rec.Data)
    │   └── rec.Validate(d.crc.Sum32())
    │       └── 실패 시 isTornEntry() 체크
    │
    └── lastValidOff 업데이트
```

### 8.3 Torn Write 감지

```
경로: server/storage/wal/decoder.go (167~201행)
```

```go
func (d *decoder) isTornEntry(data []byte) bool {
    if len(d.brs) != 1 {
        return false  // 마지막 파일에서만 torn write 가능
    }

    // 데이터를 섹터 경계로 분할
    for curOff < len(data) {
        chunkLen := int(minSectorSize - (fileOff % minSectorSize))
        chunks = append(chunks, data[curOff:curOff+chunkLen])
        // ...
    }

    // 섹터 청크 중 하나라도 전부 0이면 torn write
    for _, sect := range chunks {
        isZero := true
        for _, v := range sect {
            if v != 0 {
                isZero = false
                break
            }
        }
        if isZero {
            return true
        }
    }
    return false
}
```

**Torn Write란?**

```
정상 쓰기:
섹터1: [데이터A] [데이터B]
섹터2: [데이터C] [데이터D]

Torn Write (정전 중 발생):
섹터1: [데이터A] [데이터B]  ← 이건 기록됨
섹터2: [00000000] [00000000] ← 이건 기록 안 됨

감지: 섹터 청크 중 하나가 전부 0 → torn write로 판단
처리: io.ErrUnexpectedEOF 반환 → Repair()로 복구 가능
```

---

## 9. ReadAll(): WAL 전체 읽기

```
경로: server/storage/wal/wal.go (470~592행)
```

ReadAll은 WAL의 모든 레코드를 읽어서 반환한다:

```go
func (w *WAL) ReadAll() (metadata []byte, state raftpb.HardState,
                          ents []raftpb.Entry, err error) {
    w.mu.Lock()
    defer w.mu.Unlock()

    rec := &walpb.Record{}
    decoder := w.decoder

    var match bool
    for err = decoder.Decode(rec); err == nil; err = decoder.Decode(rec) {
        switch rec.GetType() {
        case EntryType:
            e := MustUnmarshalEntry(rec.Data)
            if e.Index > w.start.GetIndex() {
                offset := e.Index - w.start.GetIndex() - 1
                ents = append(ents[:offset], e)  // 오버라이드 가능!
            }
            w.enti = e.Index

        case StateType:
            state = MustUnmarshalState(rec.Data)

        case MetadataType:
            if metadata != nil && !bytes.Equal(metadata, rec.Data) {
                return nil, state, nil, ErrMetadataConflict
            }
            metadata = rec.Data

        case CrcType:
            crc := decoder.LastCRC()
            if crc != 0 && rec.Validate(crc) != nil {
                return nil, state, nil, ErrCRCMismatch
            }
            decoder.UpdateCRC(rec.GetCrc())

        case SnapshotType:
            var snap walpb.Snapshot
            if snap.GetIndex() == w.start.GetIndex() {
                if snap.GetTerm() != w.start.GetTerm() {
                    return nil, state, nil, ErrSnapshotMismatch
                }
                match = true
            }
        }
    }
    // ...
}
```

**엔트리 오버라이드의 의미 (ents = append(ents[:offset], e)):**

Raft 논문 Figure 7에서 설명하는 시나리오이다. 리더 교체 후 새 리더가 이전 리더의 일부 uncommitted 엔트리를 덮어쓸 수 있다. WAL에는 두 버전 모두 기록되지만, ReadAll은 같은 인덱스의 최신 엔트리만 유지한다.

```
WAL 내용:
  Entry{Index:5, Term:2, Data:"A"}  ← 이전 리더가 기록
  Entry{Index:6, Term:2, Data:"B"}
  Entry{Index:5, Term:3, Data:"C"}  ← 새 리더가 기록 (Index:5 덮어씀)

ReadAll 결과:
  ents[0] = Entry{Index:5, Term:3, Data:"C"}  ← 최신 버전만 유지
```

---

## 10. sync(): fsync와 성능

```
경로: server/storage/wal/wal.go (830~855행)
```

```go
func (w *WAL) sync() error {
    if w.encoder != nil {
        if err := w.encoder.flush(); err != nil {
            return err
        }
    }

    if w.unsafeNoSync {
        return nil
    }

    start := time.Now()
    err := fileutil.Fdatasync(w.tail().File)

    took := time.Since(start)
    if took > warnSyncDuration {  // 1초 초과 시 경고
        w.lg.Warn("slow fdatasync",
            zap.Duration("took", took),
            zap.Duration("expected-duration", warnSyncDuration),
        )
    }
    walFsyncSec.Observe(took.Seconds())

    return err
}
```

**fdatasync vs fsync:**

- `fsync`: 데이터 + 메타데이터(파일 크기, 수정 시간 등) 모두 디스크에 동기화
- `fdatasync`: 데이터만 동기화 (메타데이터 변경이 데이터 접근에 필요 없으면 생략)
- etcd는 `fdatasync`를 사용하여 불필요한 메타데이터 동기화 오버헤드를 줄인다

---

## 11. Snapshotter: 스냅샷 관리

### 11.1 구조체

```
경로: server/etcdserver/api/snap/snapshotter.go (53~56행)
```

```go
type Snapshotter struct {
    lg  *zap.Logger
    dir string
}
```

### 11.2 스냅샷 저장

```
경로: server/etcdserver/api/snap/snapshotter.go (75~105행)
```

```go
func (s *Snapshotter) save(snapshot *raftpb.Snapshot) error {
    start := time.Now()

    // 파일명: {term:016x}-{index:016x}.snap
    fname := fmt.Sprintf("%016x-%016x%s",
        snapshot.Metadata.Term, snapshot.Metadata.Index, snapSuffix)

    // raftpb.Snapshot → protobuf 직렬화
    b := pbutil.MustMarshal(snapshot)

    // CRC32 계산
    crc := crc32.Update(0, crcTable, b)

    // snappb.Snapshot으로 래핑 (CRC + Data)
    snap := snappb.Snapshot{Crc: &crc, Data: b}
    d, _ := snap.Marshal()

    // 파일 쓰기 + fsync
    spath := filepath.Join(s.dir, fname)
    err = pioutil.WriteAndSyncFile(spath, d, 0o666)

    return nil
}
```

**스냅샷 파일 형식:**

```
┌─────────────────────────────────────┐
│  snappb.Snapshot (protobuf)          │
│  ┌──────────┬───────────────────┐   │
│  │ Crc      │ CRC32-Castagnoli  │   │
│  ├──────────┼───────────────────┤   │
│  │ Data     │ raftpb.Snapshot   │   │
│  │          │ ┌───────────────┐ │   │
│  │          │ │ Metadata:     │ │   │
│  │          │ │  ConfState    │ │   │
│  │          │ │  Index, Term  │ │   │
│  │          │ ├───────────────┤ │   │
│  │          │ │ Data:         │ │   │
│  │          │ │  (상태 데이터) │ │   │
│  │          │ └───────────────┘ │   │
│  └──────────┴───────────────────┘   │
└─────────────────────────────────────┘
```

### 11.3 스냅샷 로드

```
경로: server/etcdserver/api/snap/snapshotter.go (156~196행)
```

```go
func Read(lg *zap.Logger, snapname string) (*raftpb.Snapshot, error) {
    b, _ := os.ReadFile(snapname)

    var serializedSnap snappb.Snapshot
    serializedSnap.Unmarshal(b)

    // CRC 검증
    crc := crc32.Update(0, crcTable, serializedSnap.Data)
    if crc != serializedSnap.GetCrc() {
        return nil, ErrCRCMismatch
    }

    var snap raftpb.Snapshot
    snap.Unmarshal(serializedSnap.Data)
    return &snap, nil
}
```

### 11.4 스냅샷 파일 이름과 정렬

```
경로: server/etcdserver/api/snap/snapshotter.go (200~220행)
```

스냅샷 파일명은 `{term}-{index}.snap` 형식이므로 사전순 정렬하면 자동으로 시간순 정렬이 된다. `snapNames()`는 **역순 정렬** (최신 먼저)을 반환한다.

```go
sort.Sort(sort.Reverse(sort.StringSlice(snaps)))
```

이렇게 하면 `Load()`가 가장 최신 스냅샷부터 시도하고, 손상된 경우 이전 스냅샷으로 폴백한다.

---

## 12. WAL + 스냅샷 기반 장애 복구

### 12.1 복구 흐름

```
프로세스 시작
    │
    ▼
┌──────────────────────────────┐
│ 1. 스냅샷 디렉토리에서        │
│    최신 유효 스냅샷 로드       │
│    (CRC 검증 포함)            │
└──────────────┬───────────────┘
               │
               ▼
┌──────────────────────────────┐
│ 2. WAL에서 유효한 스냅샷     │
│    마커 목록 조회             │
│    ValidSnapshotEntries()    │
└──────────────┬───────────────┘
               │
               ▼
┌──────────────────────────────┐
│ 3. 스냅샷 인덱스 이후의      │
│    WAL 파일 선택             │
│    selectWALFiles()          │
└──────────────┬───────────────┘
               │
               ▼
┌──────────────────────────────┐
│ 4. WAL.Open() + ReadAll()    │
│    스냅샷 이후의 모든 엔트리  │
│    + HardState 복원          │
└──────────────┬───────────────┘
               │
               ▼
┌──────────────────────────────┐
│ 5. 스냅샷 상태 복원 +        │
│    WAL 엔트리 재적용         │
│    → 전체 상태 복원 완료      │
└──────────────────────────────┘
```

### 12.2 타임라인 예시

```
시간축:

  rev 1    rev 100   rev 200   rev 300   rev 350
  │        │         │         │         │
  ▼        ▼         ▼         ▼         ▼
  ├────────┤─────────┤─────────┤─────────┤
  │ WAL    │  WAL    │  WAL    │  WAL    │
  │ seg 0  │  seg 1  │  seg 2  │  seg 3  │
  └────────┴─────────┴─────────┴─────────┘
                      │
                      ▼
                  스냅샷 생성
                  (rev 200 시점)

  장애 발생 시점: rev 350

  복구:
  1. 스냅샷 로드 (rev 200까지의 전체 상태)
  2. WAL seg 2부터 읽기 (rev 200 이후의 엔트리)
  3. rev 201~350의 엔트리를 스냅샷 위에 재적용
  4. WAL seg 0, 1은 이제 불필요 (ReleaseLockTo()로 해제)
```

---

## 13. Repair(): 손상된 WAL 복구

```
경로: server/storage/wal/repair.go (32~106행)
```

```go
func Repair(lg *zap.Logger, dirpath string) bool {
    f, err := openLast(lg, dirpath)  // 마지막 WAL 파일만 열기

    rec := &walpb.Record{}
    decoder := NewDecoder(fileutil.NewFileReader(f.File))
    for {
        lastOffset := decoder.LastOffset()
        err := decoder.Decode(rec)
        switch {
        case err == nil:
            // CRC 타입이면 디코더 CRC 업데이트
            continue

        case errors.Is(err, io.EOF):
            // 정상 종료
            return true

        case errors.Is(err, io.ErrUnexpectedEOF):
            // 불완전 레코드 발견!
            // 1. 원본 파일 백업 (.broken)
            brokenName := f.Name() + ".broken"
            bf, _ = createNewWALFile[*os.File](brokenName, true)
            io.Copy(bf, f)

            // 2. 마지막 유효 오프셋에서 truncate
            f.Truncate(lastOffset)
            fileutil.Fsync(f.File)
            return true

        default:
            // 복구 불가능한 오류
            return false
        }
    }
}
```

**Repair 전략:**

```
손상된 WAL:
┌─────────────────────────────────────────┐
│ Record1 ✓ │ Record2 ✓ │ Record3 ✗ │ ... │
└──────────────────────────┬──────────────┘
                          lastValidOff

복구 후:
원본 → {파일}.broken (백업)
WAL  → truncate at lastValidOff

┌──────────────────────────┐
│ Record1 ✓ │ Record2 ✓ │  │  ← 유효한 부분만 보존
└──────────────────────────┘
```

**제한사항:**
- 마지막 WAL 파일만 복구 가능 (이전 파일이 손상되면 복구 불가)
- 잘려나간 레코드는 영구적으로 손실됨
- `.broken` 백업 파일은 수동 분석용

---

## 14. ReleaseLockTo(): WAL 파일 정리

```
경로: server/storage/wal/wal.go (865~906행)
```

```go
func (w *WAL) ReleaseLockTo(index uint64) error {
    w.mu.Lock()
    defer w.mu.Unlock()

    var smaller int
    for i, l := range w.locks {
        _, lockIndex, _ := parseWALName(filepath.Base(l.Name()))
        if lockIndex >= index {
            smaller = i - 1
            break
        }
    }

    for i := 0; i < smaller; i++ {
        w.locks[i].Close()
    }
    w.locks = w.locks[smaller:]
    return nil
}
```

스냅샷이 생성된 후 해당 스냅샷 이전의 WAL 파일은 더 이상 필요 없다. `ReleaseLockTo()`는 이러한 오래된 WAL 파일의 잠금을 해제하여 디스크 공간을 확보한다.

**주의**: 해당 인덱스보다 작은 파일 중 가장 큰 것은 보존한다. 이는 스냅샷 인덱스를 포함하는 WAL 파일이 복구에 필요할 수 있기 때문이다.

---

## 15. 설계 요약

```
┌────────────────────────────────────────────────────────────┐
│                    WAL & 스냅샷 시스템                       │
│                                                              │
│  쓰기 경로:                                                  │
│  ┌──────────┐    ┌───────────┐    ┌─────────────┐          │
│  │ Raft     │───>│ WAL.Save  │───>│ fdatasync   │          │
│  │ 제안     │    │ (직렬화)   │    │ (디스크 동기)│          │
│  └──────────┘    └───────────┘    └─────────────┘          │
│                       │                                      │
│                       │ curOff >= 64MB                       │
│                       ▼                                      │
│                  ┌───────────┐    ┌──────────────┐          │
│                  │ cut()     │◄───│ filePipeline │          │
│                  │ (세그먼트) │    │ (사전 할당)   │          │
│                  └───────────┘    └──────────────┘          │
│                                                              │
│  스냅샷 경로:                                                │
│  ┌──────────┐    ┌───────────────┐    ┌──────────┐         │
│  │ 주기적   │───>│ Snapshotter   │───>│ .snap    │         │
│  │ 트리거   │    │ .save()       │    │ 파일     │         │
│  └──────────┘    └───────────────┘    └──────────┘         │
│                                                              │
│  복구 경로:                                                  │
│  ┌──────────┐    ┌───────────┐    ┌──────────────┐         │
│  │ .snap    │ + │ WAL       │ = │ 전체 상태    │         │
│  │ (기준점)  │   │ (이후 로그)│   │ 복원         │         │
│  └──────────┘    └───────────┘    └──────────────┘         │
│                                                              │
│  데이터 무결성:                                              │
│  ┌──────────┐    ┌───────────┐    ┌──────────────┐         │
│  │ CRC32    │    │ 8바이트   │    │ Torn Write   │         │
│  │ 체인     │    │ 정렬      │    │ 감지         │         │
│  └──────────┘    └───────────┘    └──────────────┘         │
└────────────────────────────────────────────────────────────┘
```

**핵심 설계 원칙:**

1. **Write-Ahead**: 모든 상태 변경은 먼저 WAL에 기록된 후 적용
2. **원자적 초기화**: 임시 디렉토리 + rename으로 원자성 보장
3. **CRC 체인**: 파일 경계를 넘는 데이터 무결성 검증
4. **사전 할당**: filePipeline으로 세그먼트 절단 지연 최소화
5. **점진적 복구**: 스냅샷 + WAL 조합으로 효율적 장애 복구
6. **Torn Write 감지**: 섹터 경계 기반 0-채움 패턴 감지
