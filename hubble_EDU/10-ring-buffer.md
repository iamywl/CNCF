# 10. Hubble 링 버퍼 (Ring Buffer)

## 목차
1. [개요](#1-개요)
2. [Ring 구조체 상세 분석](#2-ring-구조체-상세-분석)
3. [용량 제약 (2^n - 1)](#3-용량-제약-2n---1)
4. [Lock-free Write 메커니즘](#4-lock-free-write-메커니즘)
5. [Read 알고리즘](#5-read-알고리즘)
6. [readFrom 연속 읽기 알고리즘](#6-readfrom-연속-읽기-알고리즘)
7. [RingReader](#7-ringreader)
8. [오버플로우 감지](#8-오버플로우-감지)
9. [알림 채널 메커니즘](#9-알림-채널-메커니즘)
10. [Lost Event 처리](#10-lost-event-처리)
11. [Atomic 연산과 메모리 안전성](#11-atomic-연산과-메모리-안전성)
12. [성능 특성 분석](#12-성능-특성-분석)
13. [운영 관점에서의 Ring Buffer](#13-운영-관점에서의-ring-buffer)

---

## 1. 개요

Hubble의 링 버퍼는 eBPF 데이터패스에서 수집된 네트워크 이벤트를 임시 저장하는
핵심 데이터 구조이다. 단일 쓰기자(writer)와 다중 읽기자(reader)를 지원하며,
lock-free 알고리즘을 사용하여 높은 처리량을 달성한다.

```
  eBPF 이벤트                                  gRPC 클라이언트
      │                                            │
      v                                            v
  ┌─────────┐     ┌────────────────────┐     ┌──────────┐
  │ Observer │────>│     Ring Buffer    │<────│ RingReader│
  │ (Writer) │     │  [0][1][2]...[N-1] │     │ (Reader)  │
  └─────────┘     └────────────────────┘     └──────────┘
                        │                         │
                  단일 쓰기자                다중 읽기자
                  (Lock 최소)              (Lock-free)
```

소스 경로:
- Ring: `cilium/pkg/hubble/container/ring.go`
- RingReader: `cilium/pkg/hubble/container/ring_reader.go`

---

## 2. Ring 구조체 상세 분석

### 2.1 구조체 정의

소스 경로: `cilium/pkg/hubble/container/ring.go`

```go
type Ring struct {
    // mask: 인덱스 계산을 위한 비트 마스크
    // write AND mask = 배열 인덱스
    mask uint64

    // write: 마지막 쓰기 위치 (원자적 증가)
    // 이 필드를 mask와 AND 연산하면 실제 data 인덱스를 얻음
    write atomic.Uint64

    // cycleExp: 2^x의 지수 (dataLen = 2^cycleExp)
    // write >> cycleExp = 현재 사이클 번호
    cycleExp uint8

    // cycleMask: 사이클 번호 계산용 마스크
    cycleMask uint64

    // halfCycle: 전체 사이클 수의 절반
    // reader가 writer보다 앞서 있는지 뒤에 있는지 판단에 사용
    halfCycle uint64

    // dataLen: 내부 버퍼의 길이 (= mask + 1)
    dataLen uint64

    // data: 이벤트를 저장하는 내부 배열
    data []*v1.Event

    // notifyMu, notifyCh: reader에게 새 쓰기를 알리는 메커니즘
    notifyMu lock.Mutex
    notifyCh chan struct{}
}
```

### 2.2 필드 간 관계

```
  예: capacity = 7 (Capacity7)

  mask      = 0b0111 = 7        (인덱스 마스크)
  dataLen   = 0b1000 = 8        (mask + 1 = 실제 배열 크기)
  cycleExp  = 3                 (log2(dataLen))
  cycleMask = ^uint64(0) >> 3   (사이클 번호 마스크)
  halfCycle = cycleMask >> 1    (사이클 절반)

  write 값:  0  1  2  3  4  5  6  7  8  9  10 11 12 13 14 15 16 ...
  인덱스:    0  1  2  3  4  5  6  7  0  1   2  3  4  5  6  7  0 ...
  사이클:    0  0  0  0  0  0  0  0  1  1   1  1  1  1  1  1  2 ...
```

### 2.3 NewRing 초기화

```go
func NewRing(n Capacity) *Ring {
    // mask 계산: MSB에서 마스크 생성
    mask := math.GetMask(math.MSB(uint64(n.Cap())))
    dataLen := uint64(mask + 1)                      // 하나의 슬롯은 쓰기용으로 예약
    cycleExp := uint8(math.MSB(mask+1)) - 1
    halfCycle := (^uint64(0) >> cycleExp) >> 1        // 전체 사이클의 절반

    return &Ring{
        mask:      mask,
        cycleExp:  cycleExp,
        cycleMask: ^uint64(0) >> cycleExp,
        halfCycle: halfCycle,
        dataLen:   dataLen,
        data:      make([]*v1.Event, dataLen),
        notifyMu:  lock.Mutex{},
        notifyCh:  nil,
    }
}
```

---

## 3. 용량 제약 (2^n - 1)

### 3.1 Capacity 타입

```go
type capacity uint16

const (
    Capacity1     capacity = 1<<(iota+1) - 1  // 1
    Capacity3                                   // 3
    Capacity7                                   // 7
    Capacity15                                  // 15
    Capacity31                                  // 31
    Capacity63                                  // 63
    Capacity127                                 // 127
    Capacity255                                 // 255
    Capacity511                                 // 511
    Capacity1023                                // 1,023
    Capacity2047                                // 2,047
    Capacity4095                                // 4,095
    Capacity8191                                // 8,191
    Capacity16383                               // 16,383
    Capacity32767                               // 32,767
    Capacity65535                                // 65,535
)
```

### 3.2 왜 2^n - 1인가?

Ring Buffer의 용량이 `2^n - 1`이어야 하는 이유:

1. **내부 배열 크기는 2^n**: 비트 마스크로 인덱스 계산이 가능하도록 2의 거듭제곱 크기가 필요
2. **1 슬롯은 쓰기용 예약**: writer가 현재 쓰고 있는 슬롯은 reader가 읽을 수 없음
3. **읽기 가능 용량 = 2^n - 1**: 따라서 사용자에게 노출되는 용량은 항상 `2^n - 1`

```
  dataLen = 8 (2^3), capacity = 7 (2^3 - 1)

  ┌───┬───┬───┬───┬───┬───┬───┬───┐
  │ 0 │ 1 │ 2 │ 3 │ 4 │ 5 │ 6 │ 7 │  ← data[8]
  └───┴───┴───┴───┴───┴───┴───┴─W─┘
                                 ^
                          쓰기 중인 슬롯 (읽기 불가)
                          읽기 가능: 7개
```

### 3.3 용량 검증

```go
func NewCapacity(n int) (Capacity, error) {
    switch {
    case n > int(^capacity(0)):   // uint16 최대값 초과 검사
        return nil, fmt.Errorf("invalid capacity: too large: %d", n)
    case n > 0:
        if n&(n+1) == 0 {         // n+1이 2의 거듭제곱인지 확인
            return capacity(n), nil
        }
    }
    return nil, fmt.Errorf("invalid capacity: must be one less than an integer power of two: %d", n)
}
```

비트 트릭 설명: `n & (n+1) == 0`은 n이 `2^k - 1` 형태인지 확인한다.

```
  n = 7  (0b0111):  7 & 8  = 0b0111 & 0b1000 = 0  → 유효
  n = 6  (0b0110):  6 & 7  = 0b0110 & 0b0111 = 6  → 무효
  n = 15 (0b1111): 15 & 16 = 0b1111 & 0b10000 = 0 → 유효
```

---

## 4. Lock-free Write 메커니즘

### 4.1 Write 메서드

```go
func (r *Ring) Write(entry *v1.Event) {
    // notification mutex를 잡고 write 갱신
    // readFrom 고루틴이 sleep하기 직전에 알림을 놓치지 않도록 함
    r.notifyMu.Lock()

    // 원자적으로 write 포인터 증가
    write := r.write.Add(1)

    // write-1 위치에 데이터 저장 (Add가 증가 후 값을 반환하므로)
    writeIdx := (write - 1) & r.mask
    r.dataStoreAtomic(writeIdx, entry)

    // 대기 중인 reader에게 알림
    if r.notifyCh != nil {
        close(r.notifyCh)    // 채널을 닫아서 모든 대기자 깨움
        r.notifyCh = nil
    }

    r.notifyMu.Unlock()
}
```

### 4.2 Write 시퀀스

```
  초기 상태: write = 5, mask = 7

  1. r.write.Add(1)       → write = 6 (원자적 증가)
  2. writeIdx = (6-1) & 7 → writeIdx = 5
  3. r.data[5] = entry    → 데이터 저장 (원자적)
  4. close(r.notifyCh)    → reader 깨우기

  write:    5 → 6
  index:    5
  data:  [_][_][_][_][_][E][_][_]
                          ^
                       새로 기록됨
```

### 4.3 왜 notifyMu가 필요한가?

`notifyMu`는 reader가 sleep하는 시점과 writer가 알림을 보내는 시점 사이의 경쟁 조건을 방지한다:

```
  notifyMu 없는 경우 (위험):
  Reader                          Writer
  ────────                        ────────
  read: 데이터 없음
  if notifyCh == nil {            write.Add(1) → 데이터 쓰기
    notifyCh = make(chan)         if notifyCh != nil {  ← notifyCh은 아직 nil!
  }                                   close(notifyCh)   ← 이 코드가 실행 안됨
  <-notifyCh                      }
  (영원히 블로킹!)

  notifyMu 사용 시 (안전):
  Reader                          Writer
  ────────                        ────────
  notifyMu.Lock()                 notifyMu.Lock() ← 대기
  if lastWrite == write-1 {       (Writer가 Lock 획득할 때는
    notifyCh = make(chan)          이미 reader가 notifyCh를 설정한 후)
  }
  notifyMu.Unlock()               write.Add(1)
  <-notifyCh                      close(notifyCh) ← 정상 알림
```

### 4.4 원자적 데이터 접근

```go
// 원자적 로드: 데이터 레이스 없이 data[dataIdx] 읽기
func (r *Ring) dataLoadAtomic(dataIdx uint64) (e *v1.Event) {
    slot := unsafe.Pointer(&r.data[dataIdx])
    return (*v1.Event)(atomic.LoadPointer((*unsafe.Pointer)(slot)))
}

// 원자적 스토어: 데이터 레이스 없이 data[dataIdx] = e 쓰기
func (r *Ring) dataStoreAtomic(dataIdx uint64, e *v1.Event) {
    slot := unsafe.Pointer(&r.data[dataIdx])
    atomic.StorePointer((*unsafe.Pointer)(slot), unsafe.Pointer(e))
}
```

`unsafe.Pointer`를 사용하는 이유: Go의 `sync/atomic` 패키지는 포인터의 원자적 로드/
스토어를 지원하지만, 제네릭 타입에 대해서는 `unsafe.Pointer`를 통해야 한다.
이를 통해 writer가 데이터를 쓰는 도중에 reader가 불완전한 포인터를 읽는 것을 방지한다.

---

## 5. Read 알고리즘

### 5.1 read 메서드

```go
func (r *Ring) read(read uint64) (*v1.Event, error) {
    readIdx := read & r.mask
    event := r.dataLoadAtomic(readIdx)

    lastWrite := r.write.Load() - 1
    lastWriteIdx := lastWrite & r.mask

    readCycle := read >> r.cycleExp
    writeCycle := lastWrite >> r.cycleExp
    prevWriteCycle := (writeCycle - 1) & r.cycleMask
    maxWriteCycle := (writeCycle + r.halfCycle) & r.cycleMask

    switch {
    // Case 1: 같은 사이클, 유효한 인덱스
    case readCycle == writeCycle && readIdx < lastWriteIdx:
        if event == nil {
            return nil, io.EOF
        }
        return event, nil

    // Case 2: 이전 사이클, 유효한 인덱스
    case readCycle == prevWriteCycle && readIdx > lastWriteIdx:
        if event == nil {
            return getLostEvent(), nil
        }
        return event, nil

    // Case 3: reader가 writer보다 앞서 있음
    case readCycle >= writeCycle && readCycle < maxWriteCycle:
        return nil, io.EOF

    // Case 4: reader가 writer보다 뒤에 있음 (오버플로우)
    default:
        return getLostEvent(), nil
    }
}
```

### 5.2 사이클 기반 유효성 검사 다이어그램

```
  mask = 0x7 (capacity = 7, dataLen = 8)
  lastWrite = 3 (사이클 0, 인덱스 3)

                  +------유효한 읽기 범위------+  +현재 쓰기 중
                  |                            |  |  +다음 쓰기 위치 (r.write)
                  V                            V  V  V
  write: f8 f9 fa fb fc fd fe ff  0  1  2  3  4  5  6  7
  index:  0  1  2  3  4  5  6  7  0  1  2  3  4  5  6  7
  cycle: 1f 1f 1f 1f 1f 1f 1f 1f  0  0  0  0  0  0  0  0

  Case 1: readCycle == writeCycle(0) && readIdx < lastWriteIdx(3)
          → 인덱스 0, 1, 2가 읽기 가능

  Case 2: readCycle == prevWriteCycle(0x1f) && readIdx > lastWriteIdx(3)
          → 인덱스 4, 5, 6, 7 (이전 사이클)이 읽기 가능

  Case 3: readCycle >= writeCycle && readCycle < maxWriteCycle
          → reader가 아직 안 쓰여진 영역을 읽으려 함 → EOF

  Case 4: 그 외
          → writer가 이미 덮어쓴 영역 → LostEvent
```

### 5.3 위치 쿼리 메서드

```go
// 마지막 쓰기 위치 (병렬 쓰기 안전)
func (r *Ring) LastWriteParallel() uint64 {
    // write - 2를 반환: write - 1은 아직 쓰기 중일 수 있음
    return r.write.Load() - 2
}

// 마지막 쓰기 위치 (직렬 쓰기)
func (r *Ring) LastWrite() uint64 {
    return r.write.Load() - 1
}

// 가장 오래된 쓰기 위치
func (r *Ring) OldestWrite() uint64 {
    write := r.write.Load()
    if write > r.dataLen {
        return write - r.dataLen  // 버퍼가 한 바퀴 이상 돌았음
    }
    return 0  // 아직 버퍼가 다 차지 않음
}

// 현재 저장된 이벤트 수
func (r *Ring) Len() uint64 {
    write := r.write.Load()
    if write >= r.dataLen {
        return r.Cap()  // 버퍼가 꽉 참
    }
    return write
}

// 최대 용량 (dataLen - 1)
func (r *Ring) Cap() uint64 {
    return r.dataLen - 1  // 1 슬롯은 쓰기용 예약
}
```

---

## 6. readFrom 연속 읽기 알고리즘

### 6.1 readFrom 메서드

`readFrom`은 `hubble observe --follow`와 같은 실시간 스트리밍에 사용되는 핵심 함수이다.
context가 취소될 때까지 계속 이벤트를 읽는다.

```go
func (r *Ring) readFrom(ctx context.Context, read uint64, ch chan<- *v1.Event) {
    for ; ; read++ {
        readIdx := read & r.mask
        event := r.dataLoadAtomic(readIdx)

        lastWrite := r.write.Load() - 1
        lastWriteIdx := lastWrite & r.mask
        writeCycle := lastWrite >> r.cycleExp
        readCycle := read >> r.cycleExp

        switch {
        // Case 1: 이전 사이클의 유효한 데이터
        case event != nil &&
             readCycle == (writeCycle-1)&r.cycleMask &&
             readIdx > lastWriteIdx:
            select {
            case ch <- event:
                continue
            case <-ctx.Done():
                return
            }

        // Case 2: 현재 사이클의 유효한 데이터
        case event != nil && readCycle == writeCycle:
            if readIdx < lastWriteIdx {
                select {
                case ch <- event:
                    continue
                case <-ctx.Done():
                    return
                }
            }
            // reader가 writer를 따라잡음 → 대기
            fallthrough

        // Case 3: 읽을 데이터 없음 → 대기
        case event == nil ||
             readCycle >= (writeCycle+1)&r.cycleMask &&
             readCycle < (r.halfCycle+writeCycle)&r.cycleMask:

            r.notifyMu.Lock()
            // 이중 확인: Lock을 잡는 동안 write가 변경됐을 수 있음
            if lastWrite != r.write.Load()-1 {
                r.notifyMu.Unlock()
                read--       // 같은 위치 재시도
                continue
            }

            // 알림 채널 설정
            if r.notifyCh == nil {
                r.notifyCh = make(chan struct{})
            }
            notifyCh := r.notifyCh
            r.notifyMu.Unlock()

            // 새 쓰기 또는 컨텍스트 취소 대기
            select {
            case <-notifyCh:
                read--       // 같은 위치 재시도
                continue
            case <-ctx.Done():
                return
            }

        // Case 4: writer가 앞질러 감 → Lost Event
        default:
            select {
            case ch <- getLostEvent():
                continue
            case <-ctx.Done():
                return
            }
        }
    }
}
```

### 6.2 readFrom 상태 다이어그램

```
              ┌──────────┐
              │ 읽기 시도  │
              └─────┬────┘
                    │
         ┌──────────┼──────────┬──────────────┐
         │          │          │              │
    이전 사이클    현재 사이클   읽을 데이터    writer가
    유효 데이터    유효 데이터   없음           앞질러 감
         │          │          │              │
         v          v          v              v
    ch <- event  ch <- event  Lock 획득     ch <- getLostEvent()
         │          │          │              │
         │          │     이중 확인            │
         │          │     write 변경?         │
         │          │     ┌──Yes──┐           │
         │          │     │       │           │
         │          │     v       v           │
         │          │   read--  notifyCh      │
         │          │   재시도    대기          │
         │          │            │            │
         └──────────┴──────┬─────┴────────────┘
                           │
                      read++ → 다음 이벤트
```

### 6.3 "이중 확인" 패턴의 중요성

readFrom에서 reader가 sleep하기 전에 `lastWrite != r.write.Load()-1`을 확인하는
이유:

```
  문제 시나리오 (이중 확인 없이):
  ─────────────────────────────
  Reader                              Writer
  ─────                               ────────
  lastWrite = r.write.Load()-1 = 5
  event = nil (읽을 데이터 없음)        write.Add(1) → write = 6
                                       data[5] = event
                                       close(notifyCh)  ← 아직 notifyCh == nil!
  r.notifyMu.Lock()
  notifyCh = make(chan struct{})
  <-notifyCh                          ← 영원히 블로킹!

  해결 (이중 확인):
  ─────────────────
  Reader                              Writer
  ─────                               ────────
  lastWrite = 5
                                       write.Add(1) → write = 6
  r.notifyMu.Lock()
  if lastWrite != r.write.Load()-1 {  ← 5 != 5? No, 5 != 5 (혹은 Yes!)
    r.notifyMu.Unlock()
    read-- ; continue                 ← write가 변경되었으면 재시도
  }
```

---

## 7. RingReader

### 7.1 RingReader 구조체

소스 경로: `cilium/pkg/hubble/container/ring_reader.go`

```go
type RingReader struct {
    ring          *Ring          // 연결된 Ring
    idx           uint64         // 현재 읽기 위치
    ctx           context.Context // 현재 follow 컨텍스트
    mutex         lock.Mutex     // followChan 보호
    followChan    chan *v1.Event  // follow 모드 이벤트 채널
    followChanLen int            // 채널 버퍼 크기 (기본 1000)
    wg            sync.WaitGroup // 고루틴 대기
}
```

### 7.2 NewRingReader

```go
// start 위치에서 읽기 시작하는 RingReader 생성
func NewRingReader(ring *Ring, start uint64) *RingReader {
    return newRingReader(ring, start, 1000)  // 기본 버퍼 1000
}

func newRingReader(ring *Ring, start uint64, bufferLen int) *RingReader {
    return &RingReader{
        ring:          ring,
        idx:           start,
        ctx:           nil,
        followChanLen: bufferLen,
    }
}
```

### 7.3 Next (단방향 읽기)

```go
func (r *RingReader) Next() (*v1.Event, error) {
    e, err := r.ring.read(r.idx)
    if err != nil {
        return nil, err  // io.EOF 또는 ErrInvalidRead
    }
    r.idx++
    return e, nil
}
```

Next()의 에러 처리:
- **io.EOF**: writer보다 앞서 있음. 데이터가 아직 없으므로 인덱스 증가하지 않음
- **ErrInvalidRead**: writer가 이미 덮어쓴 영역. 에러 전파 (따라잡기 시도는 경쟁 상태)

### 7.4 Previous (역방향 읽기)

```go
func (r *RingReader) Previous() (*v1.Event, error) {
    e, err := r.ring.read(r.idx)
    if err != nil {
        return nil, err  // ErrInvalidRead만 기대
    }
    r.idx--
    return e, nil
}
```

Previous는 `hubble observe --last N` 구현에 사용된다. Ring의 끝에서부터 역순으로
N개 이벤트를 읽는다.

### 7.5 NextFollow (실시간 스트리밍)

```go
func (r *RingReader) NextFollow(ctx context.Context) *v1.Event {
    // 컨텍스트가 변경되면 readFrom 재시작
    if r.ctx != ctx {
        r.mutex.Lock()
        if r.followChan == nil {
            r.followChan = make(chan *v1.Event, r.followChanLen)
        }
        r.mutex.Unlock()

        // 별도 고루틴에서 ring.readFrom 실행
        r.wg.Add(1)
        go func(ctx context.Context) {
            r.ring.readFrom(ctx, r.idx, r.followChan)

            // 컨텍스트 완료 시 채널 정리
            r.mutex.Lock()
            if ctx.Err() != nil && r.followChan != nil {
                close(r.followChan)
                r.followChan = nil
            }
            r.mutex.Unlock()
            r.wg.Done()
        }(ctx)
        r.ctx = ctx
    }

    // 채널에서 이벤트 수신
    select {
    case e, ok := <-followChan:
        if !ok {
            return nil  // 채널 닫힘 (컨텍스트 완료)
        }
        r.idx++
        return e
    case <-ctx.Done():
        return nil
    }
}
```

### 7.6 NextFollow의 생명주기

```
  호출 1: NextFollow(ctx1)
  ┌──────────────────────────┐
  │ ctx != ctx1 → 새 고루틴    │
  │ go ring.readFrom(ctx1)   │
  │ followChan에서 이벤트 수신  │
  └──────────────────────────┘

  호출 2: NextFollow(ctx1)
  ┌──────────────────────────┐
  │ ctx == ctx1 → 기존 고루틴  │
  │ followChan에서 이벤트 수신  │
  └──────────────────────────┘

  ctx1 취소됨 → readFrom 반환 → followChan 닫힘

  호출 3: NextFollow(ctx2)
  ┌──────────────────────────┐
  │ ctx != ctx2 → 새 고루틴    │
  │ go ring.readFrom(ctx2)   │
  │ 새 followChan에서 수신     │
  └──────────────────────────┘
```

### 7.7 Close

```go
func (r *RingReader) Close() error {
    r.wg.Wait()  // 모든 고루틴 종료 대기
    return nil
}
```

---

## 8. 오버플로우 감지

### 8.1 오버플로우란?

Ring Buffer는 원형 구조이므로, writer가 한 바퀴를 돌아 reader가 아직 읽지 않은
데이터를 덮어쓸 수 있다. 이를 오버플로우라 한다.

```
  오버플로우 발생:

  Writer (빠름):  사이클 5
  Reader (느림):  사이클 3

  ┌───┬───┬───┬───┬───┬───┬───┬───┐
  │ W │ W │ W │ W │ W │ R │ R │ R │
  │ 5 │ 5 │ 5 │ 5 │ 5 │ 3 │ 3 │ 3 │  ← 사이클
  └───┴───┴───┴───┴───┴───┴───┴───┘
                          ^
                    Reader가 읽으려는 데이터가
                    이미 덮어쓰여졌음
```

### 8.2 사이클 기반 감지

read() 함수의 default 케이스에서 오버플로우를 감지한다:

```go
// readCycle이 writeCycle의 "뒤" 절반에 있으면 → 오버플로우
// halfCycle을 사용하여 "앞"과 "뒤"를 구분
case readCycle >= writeCycle && readCycle < maxWriteCycle:
    return nil, io.EOF  // reader가 앞서 있음 (아직 안 쓰여진 영역)

default:
    return getLostEvent(), nil  // 오버플로우 (writer가 이미 덮어씀)
```

halfCycle의 역할:

```
  사이클 공간 (uint64 기반):
  0 ─────────── writeCycle ────────── ^uint64(0)
                    │
          ┌─────────┼─────────┐
          │         │         │
      "뒤" 절반   현재      "앞" 절반
    (오버플로우) (유효 읽기)  (아직 안 쓰임)
```

---

## 9. 알림 채널 메커니즘

### 9.1 알림 흐름

```
  Writer                                Reader (readFrom)
  ────────                              ──────────────────
  notifyMu.Lock()                       notifyMu.Lock()
  write.Add(1)                          // write 변경 확인
  data[idx] = event                     if r.notifyCh == nil {
  if r.notifyCh != nil {                    r.notifyCh = make(chan struct{})
      close(r.notifyCh)  ←─────────── }
      r.notifyCh = nil                  notifyCh := r.notifyCh
  }                                     notifyMu.Unlock()
  notifyMu.Unlock()
                                        select {
                                        case <-notifyCh:  ←── close()로 깨어남
                                            read-- ; continue
                                        case <-ctx.Done():
                                            return
                                        }
```

### 9.2 왜 sync.Cond가 아닌 채널인가?

소스 코드의 주석:

```go
// notifyMu, notifyCh는 writer가 새 값을 쓸 때 readFrom의 대기 중인 reader에게
// 신호를 보내는 데 사용된다.
// sync.Cond는 select 문에서 사용할 수 없으므로 채널을 사용한다.
```

`sync.Cond`는 `Wait()`를 호출할 수 있지만, `select` 문에서 context 취소와 함께
사용할 수 없다. 채널은 `select`의 case로 사용할 수 있어 `ctx.Done()`과 함께 대기할
수 있다.

### 9.3 채널 재사용 패턴

```go
// Writer: 알림 후 채널을 nil로 설정
if r.notifyCh != nil {
    close(r.notifyCh)   // 모든 대기자를 깨움
    r.notifyCh = nil     // 다음 대기를 위해 nil
}

// Reader: nil이면 새 채널 생성
if r.notifyCh == nil {
    r.notifyCh = make(chan struct{})
}
```

이 패턴은 "일회용 알림"이다. 채널은 한 번 닫히면 재사용할 수 없으므로 매번 새로
생성한다. `close()`는 모든 수신자에게 동시에 알림을 보내므로 다중 reader를 지원한다.

---

## 10. Lost Event 처리

### 10.1 getLostEvent 함수

```go
func getLostEvent() *v1.Event {
    // Prometheus 메트릭 증가
    metrics.LostEvents.WithLabelValues(
        strings.ToLower(flowpb.LostEventSource_HUBBLE_RING_BUFFER.String()),
    ).Inc()

    now := time.Now().UTC()
    return &v1.Event{
        Timestamp: &timestamppb.Timestamp{
            Seconds: now.Unix(),
            Nanos:   int32(now.Nanosecond()),
        },
        Event: &flowpb.LostEvent{
            Source:        flowpb.LostEventSource_HUBBLE_RING_BUFFER,
            NumEventsLost: 1,
            Cpu:           nil,  // Ring Buffer 손실은 CPU별이 아님
        },
    }
}
```

### 10.2 Lost Event 발생 시점

Ring Buffer에서 Lost Event가 발생하는 세 가지 경우:

| 상황 | 발생 위치 | 설명 |
|------|----------|------|
| read()에서 오버플로우 | `read()` default case | reader가 오래된 데이터를 요청 |
| readFrom()에서 오버플로우 | `readFrom()` default case | 스트리밍 중 reader가 느림 |
| 초기 nil 이벤트 | `read()` prevWriteCycle case | 버퍼가 아직 채워지지 않음 |

### 10.3 메트릭

```go
metrics.LostEvents.WithLabelValues(
    strings.ToLower(flowpb.LostEventSource_HUBBLE_RING_BUFFER.String()),
).Inc()
```

이 메트릭은 `hubble_lost_events_total{source="hubble_ring_buffer"}` 카운터로 노출되어,
운영자가 Ring Buffer 크기가 충분한지 판단할 수 있다.

---

## 11. Atomic 연산과 메모리 안전성

### 11.1 사용되는 Atomic 연산

| 연산 | 위치 | 용도 |
|------|------|------|
| `atomic.Uint64.Add(1)` | `Write()` | write 포인터 원자적 증가 |
| `atomic.Uint64.Load()` | `read()`, `readFrom()` | write 포인터 읽기 |
| `atomic.LoadPointer` | `dataLoadAtomic()` | 데이터 슬롯 원자적 읽기 |
| `atomic.StorePointer` | `dataStoreAtomic()` | 데이터 슬롯 원자적 쓰기 |

### 11.2 메모리 순서 보장

Go의 `sync/atomic` 패키지는 순차적 일관성(sequential consistency)을 보장한다.
따라서:

1. `write.Add(1)` 이후의 `dataStoreAtomic()`은 반드시 증가된 write 값 이후에
   다른 고루틴에게 보임
2. `write.Load()` 이후의 `dataLoadAtomic()`은 해당 write 시점의 데이터를 읽음

### 11.3 동시성 모델

```
  단일 Writer                  다중 Reader
  ────────────                 ────────────
  write.Add(1)  ←── 유일한 동기화 포인트
  data[idx] = event
  close(notifyCh)

                               read():
                               write.Load()
                               dataLoadAtomic()
                               사이클 기반 유효성 검사

                               readFrom():
                               write.Load()
                               dataLoadAtomic()
                               notifyCh 대기 (필요시)
```

**핵심 불변량**: writer는 `write.Add(1)`을 호출한 후 해당 인덱스에 데이터를 쓴다.
reader는 `write.Load()`를 통해 현재 write 위치를 알고, 사이클 기반으로 데이터의
유효성을 검증한다.

---

## 12. 성능 특성 분석

### 12.1 시간 복잡도

| 연산 | 시간 복잡도 | 설명 |
|------|-----------|------|
| Write | O(1) | 원자적 증가 + 원자적 저장 |
| Read | O(1) | 원자적 로드 + 사이클 비교 |
| NextFollow (데이터 있음) | O(1) | 채널 수신 |
| NextFollow (대기) | 블로킹 | 채널 대기 |
| Len() | O(1) | write 로드 |
| Cap() | O(1) | 상수 |

### 12.2 메모리 사용량

```
  Ring Buffer 메모리 = dataLen * sizeof(pointer) + 구조체 오버헤드
                     = (capacity + 1) * 8 bytes + ~100 bytes

  예시:
  Capacity4095  →  4096 * 8 = 32 KB (포인터만)
  Capacity16383 → 16384 * 8 = 128 KB (포인터만)
  Capacity65535 → 65536 * 8 = 512 KB (포인터만)

  + 각 *v1.Event 객체 크기 (Flow protobuf 메시지)
  평균 Flow 크기 ~ 500 bytes ~ 1 KB
  Capacity16383 기준: ~16 MB 실제 데이터
```

### 12.3 경합(Contention) 분석

```
  Lock 사용 패턴:

  notifyMu:
  - Writer: Write() 전체 (짧음)
  - Reader: readFrom()에서 대기 직전만 (드물게 발생)
  → 낮은 경합: reader가 데이터를 따라잡은 경우에만 Lock 필요

  데이터 접근:
  - atomic 연산만 사용 → Lock-free
  → 매우 낮은 경합: 다중 reader가 동시에 읽기 가능
```

---

## 13. 운영 관점에서의 Ring Buffer

### 13.1 용량 결정 기준

| 시나리오 | 권장 용량 | 이유 |
|---------|----------|------|
| 소규모 (1~10 파드) | Capacity4095 | 기본값, 대부분 충분 |
| 중규모 (10~100 파드) | Capacity16383 | 높은 트래픽 대응 |
| 대규모 (100+ 파드) | Capacity65535 | 최대 용량 |
| CI/CD 테스트 | Capacity1023 | 메모리 절약 |

### 13.2 Lost Event 모니터링

```bash
# Lost Event 메트릭 확인
kubectl -n kube-system exec ds/cilium -- \
  curl -s localhost:9962/metrics | \
  grep hubble_lost_events_total

# 출력 예시:
# hubble_lost_events_total{source="hubble_ring_buffer"} 0
# hubble_lost_events_total{source="perf_event_ring_buffer"} 15
# hubble_lost_events_total{source="observer_events_queue"} 3
```

### 13.3 Ring Buffer 크기 변경

```bash
# Helm으로 Ring Buffer 크기 변경
helm upgrade cilium cilium/cilium \
  --namespace kube-system \
  --reuse-values \
  --set hubble.eventBufferCapacity=16383

# ConfigMap으로 직접 변경
kubectl -n kube-system edit configmap cilium-config
# hubble-event-buffer-capacity: "16383"
```

### 13.4 상태 확인

```bash
# hubble status로 Ring Buffer 상태 확인
hubble status --port-forward

# 출력:
# Current/Max Flows: 4,095/4,095 (100.00%)  ← Ring Buffer 꽉 참 (정상)
# Current/Max Flows: 150/4,095 (3.66%)      ← 아직 채워지지 않음
```

---

## 요약

| 항목 | 내용 |
|------|------|
| 자료구조 | Lock-free Ring Buffer |
| 용량 제약 | 2^n - 1 (1 ~ 65,535) |
| Writer | 단일, atomic.Uint64.Add + atomic.StorePointer |
| Reader | 다중, atomic 읽기 + 사이클 기반 유효성 검사 |
| 알림 | 채널 기반 (close()로 broadcast) |
| 오버플로우 | LostEvent 생성 + Prometheus 메트릭 |
| 기본 버퍼 크기 | RingReader: 1,000 이벤트 |
| 핵심 파일 | `container/ring.go`, `container/ring_reader.go` |
