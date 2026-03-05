package main

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// =============================================================================
// Hubble 링 버퍼 시뮬레이션
//
// 실제 구현 참조:
//   - pkg/hubble/container/ring.go: Ring, NewRing(), Write(), read(), readFrom()
//   - pkg/hubble/container/ring_reader.go: RingReader, Next(), NextFollow()
//
// 핵심 설계:
//   - 용량은 반드시 2^n - 1 (예: 1, 3, 7, 15, 31, ..., 65535)
//   - 내부 배열 크기 = 용량 + 1 (하나는 쓰기 예약 슬롯)
//   - 마스크 연산(write & mask)으로 인덱스 계산 → 나눗셈 불필요
//   - 사이클 카운터로 읽기/쓰기 위치의 순환 감지
//   - atomic 연산으로 동시성 안전한 읽기/쓰기
//   - notifyCh 채널로 reader 깨우기 (sync.Cond 대신 select 호환)
// =============================================================================

// Event는 링 버퍼에 저장되는 이벤트
type Event struct {
	Timestamp time.Time
	Message   string
}

func (e *Event) String() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("[%s] %s", e.Timestamp.Format("15:04:05.000"), e.Message)
}

// LostEvent는 이벤트 유실을 나타냄
type LostEvent struct {
	NumLost uint64
}

// -----------------------------------------------------------------------------
// Ring 구조체 (실제: pkg/hubble/container/ring.go)
//
// 핵심 필드:
//   mask      - 인덱스 마스크 (dataLen - 1)
//   write     - atomic 쓰기 위치 (단조 증가, 절대 감소하지 않음)
//   cycleExp  - 사이클 지수 (log2(dataLen))
//   cycleMask - 사이클 마스크
//   halfCycle - 전체 사이클의 절반 (reader가 writer 앞/뒤 판별에 사용)
//   data      - 이벤트 슬라이스
//   notifyCh  - reader 알림 채널
// -----------------------------------------------------------------------------

type Ring struct {
	mask      uint64
	write     atomic.Uint64
	cycleExp  uint8
	cycleMask uint64
	halfCycle uint64
	dataLen   uint64
	data      []*Event

	notifyMu sync.Mutex
	notifyCh chan struct{}
}

// MSB는 최상위 비트 위치를 반환 (1-based)
// 실제: pkg/hubble/math/math.go
func MSB(x uint64) int {
	if x == 0 {
		return 0
	}
	r := 0
	for x > 0 {
		r++
		x >>= 1
	}
	return r
}

// GetMask는 n비트 마스크를 반환
// 실제: pkg/hubble/math/math.go
func GetMask(n int) uint64 {
	if n <= 0 {
		return 0
	}
	if n >= 64 {
		return ^uint64(0)
	}
	return (1 << uint(n)) - 1
}

// NewRing은 용량 n으로 새 링 버퍼를 생성
// n은 반드시 2^i - 1 형태여야 함 (예: 7 = 2^3 - 1)
// 실제: container.NewRing(n Capacity)
func NewRing(n int) *Ring {
	// 용량 검증: n = 2^i - 1
	if n <= 0 || (n & (n + 1)) != 0 {
		panic(fmt.Sprintf("용량은 2^i - 1 형태여야 합니다: %d", n))
	}

	mask := GetMask(MSB(uint64(n)))
	dataLen := mask + 1       // 실제 배열 크기 = mask + 1
	cycleExp := uint8(MSB(mask+1)) - 1
	halfCycle := (^uint64(0) >> cycleExp) >> 1

	fmt.Printf("  링 버퍼 생성: 용량=%d, 배열크기=%d, mask=0x%x, cycleExp=%d, halfCycle=%d\n",
		n, dataLen, mask, cycleExp, halfCycle)

	return &Ring{
		mask:      mask,
		cycleExp:  cycleExp,
		cycleMask: ^uint64(0) >> cycleExp,
		halfCycle: halfCycle,
		dataLen:   dataLen,
		data:      make([]*Event, dataLen),
	}
}

// Cap은 읽을 수 있는 최대 이벤트 수를 반환 (dataLen - 1)
// 실제: func (r *Ring) Cap() uint64
func (r *Ring) Cap() uint64 {
	return r.dataLen - 1
}

// Len은 현재 저장된 이벤트 수를 반환
// 실제: func (r *Ring) Len() uint64
func (r *Ring) Len() uint64 {
	write := r.write.Load()
	if write >= r.dataLen {
		return r.Cap()
	}
	return write
}

// Write는 이벤트를 링 버퍼에 쓴다
// 실제 흐름:
//  1. notifyMu.Lock() - reader 알림과의 경쟁 방지
//  2. write.Add(1) - 쓰기 위치 원자적 증가
//  3. writeIdx = (write-1) & mask - 인덱스 계산
//  4. data[writeIdx] = entry - 이벤트 저장
//  5. notifyCh 닫기 - 대기 중인 reader 깨우기
//  6. notifyMu.Unlock()
func (r *Ring) Write(entry *Event) {
	r.notifyMu.Lock()

	write := r.write.Add(1)
	writeIdx := (write - 1) & r.mask
	r.data[writeIdx] = entry

	// 대기 중인 reader에게 알림
	if r.notifyCh != nil {
		close(r.notifyCh)
		r.notifyCh = nil
	}

	r.notifyMu.Unlock()
}

// LastWrite는 마지막으로 쓰여진 위치를 반환
// 실제: func (r *Ring) LastWrite() uint64
func (r *Ring) LastWrite() uint64 {
	return r.write.Load() - 1
}

// LastWriteParallel은 병렬 쓰기 안전한 마지막 위치를 반환
// write - 2를 반환하는 이유: write를 먼저 증가시키고 데이터를 쓰므로
// write - 1은 아직 쓰기 중일 수 있음
// 실제: func (r *Ring) LastWriteParallel() uint64
func (r *Ring) LastWriteParallel() uint64 {
	return r.write.Load() - 2
}

// OldestWrite는 가장 오래된 유효 위치를 반환
// 실제: func (r *Ring) OldestWrite() uint64
func (r *Ring) OldestWrite() uint64 {
	write := r.write.Load()
	if write > r.dataLen {
		return write - r.dataLen
	}
	return 0
}

// read는 주어진 위치에서 이벤트를 읽는다
// 사이클 비교로 유효한 읽기인지 판별:
//   - readCycle == writeCycle && readIdx < lastWriteIdx → 유효
//   - readCycle == prevWriteCycle && readIdx > lastWriteIdx → 유효
//   - reader가 writer보다 앞서면 → io.EOF
//   - reader가 writer보다 뒤처지면 → LostEvent
//
// 실제: func (r *Ring) read(read uint64) (*v1.Event, error)
func (r *Ring) read(read uint64) (*Event, error) {
	readIdx := read & r.mask
	event := r.data[readIdx]

	lastWrite := r.write.Load() - 1
	lastWriteIdx := lastWrite & r.mask

	readCycle := read >> r.cycleExp
	writeCycle := lastWrite >> r.cycleExp
	prevWriteCycle := (writeCycle - 1) & r.cycleMask
	maxWriteCycle := (writeCycle + r.halfCycle) & r.cycleMask

	switch {
	// Case 1: 현재 사이클에서 유효한 인덱스 읽기
	case readCycle == writeCycle && readIdx < lastWriteIdx:
		if event == nil {
			return nil, io.EOF
		}
		return event, nil

	// Case 2: 이전 사이클에서 유효한 인덱스 읽기 (wraparound)
	case readCycle == prevWriteCycle && readIdx > lastWriteIdx:
		if event == nil {
			// 아직 채워지지 않은 슬롯 → LostEvent
			return &Event{
				Timestamp: time.Now(),
				Message:   "[LOST] 유실된 이벤트 (미채움 슬롯)",
			}, nil
		}
		return event, nil

	// Case 3: reader가 writer보다 앞서 있음
	case readCycle >= writeCycle && readCycle < maxWriteCycle:
		return nil, io.EOF

	// Case 4: reader가 writer보다 뒤처짐 → 덮어씌워짐
	default:
		return &Event{
			Timestamp: time.Now(),
			Message:   "[LOST] 유실된 이벤트 (덮어씌워짐)",
		}, nil
	}
}

// readFrom은 컨텍스트가 취소될 때까지 계속 읽는다 (follow 모드)
// reader가 writer를 따라잡으면 notifyCh에서 대기
// 실제: func (r *Ring) readFrom(ctx context.Context, read uint64, ch chan<- *Event)
func (r *Ring) readFrom(ctx context.Context, read uint64, ch chan<- *Event) {
	for ; ; read++ {
		readIdx := read & r.mask
		event := r.data[readIdx]

		lastWrite := r.write.Load() - 1
		lastWriteIdx := lastWrite & r.mask
		writeCycle := lastWrite >> r.cycleExp
		readCycle := read >> r.cycleExp

		switch {
		// 이전 사이클의 유효한 데이터
		case event != nil && readCycle == (writeCycle-1)&r.cycleMask && readIdx > lastWriteIdx:
			select {
			case ch <- event:
				continue
			case <-ctx.Done():
				return
			}

		// 현재 사이클의 유효한 데이터
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

		// reader가 writer를 따라잡음 또는 아직 데이터 없음 → 대기
		case event == nil || readCycle >= (writeCycle+1)&r.cycleMask && readCycle < (r.halfCycle+writeCycle)&r.cycleMask:
			r.notifyMu.Lock()
			if lastWrite != r.write.Load()-1 {
				r.notifyMu.Unlock()
				read--
				continue
			}
			if r.notifyCh == nil {
				r.notifyCh = make(chan struct{})
			}
			notifyCh := r.notifyCh
			r.notifyMu.Unlock()

			select {
			case <-notifyCh:
				read--
				continue
			case <-ctx.Done():
				return
			}

		// 덮어씌워짐 → LostEvent 전송
		default:
			lostEvent := &Event{
				Timestamp: time.Now(),
				Message:   "[LOST] 유실 (writer가 reader를 추월)",
			}
			select {
			case ch <- lostEvent:
				continue
			case <-ctx.Done():
				return
			}
		}
	}
}

// -----------------------------------------------------------------------------
// RingReader (실제: pkg/hubble/container/ring_reader.go)
// Ring의 읽기 커서를 관리
// -----------------------------------------------------------------------------

type RingReader struct {
	ring       *Ring
	idx        uint64
	ctx        context.Context
	mu         sync.Mutex
	followChan chan *Event
}

func NewRingReader(ring *Ring, start uint64) *RingReader {
	return &RingReader{
		ring: ring,
		idx:  start,
	}
}

// Next는 현재 위치의 이벤트를 읽고 위치를 증가
// 실제: func (r *RingReader) Next() (*v1.Event, error)
func (r *RingReader) Next() (*Event, error) {
	e, err := r.ring.read(r.idx)
	if err != nil {
		return nil, err
	}
	r.idx++
	return e, nil
}

// Previous는 현재 위치의 이벤트를 읽고 위치를 감소
// 실제: func (r *RingReader) Previous() (*v1.Event, error)
func (r *RingReader) Previous() (*Event, error) {
	e, err := r.ring.read(r.idx)
	if err != nil {
		return nil, err
	}
	r.idx--
	return e, nil
}

// NextFollow는 follow 모드로 다음 이벤트를 기다려서 읽음
// 실제: func (r *RingReader) NextFollow(ctx context.Context) *Event
func (r *RingReader) NextFollow(ctx context.Context) *Event {
	if r.ctx != ctx {
		r.mu.Lock()
		if r.followChan == nil {
			r.followChan = make(chan *Event, 1000)
		}
		r.mu.Unlock()

		go func(ctx context.Context) {
			r.ring.readFrom(ctx, r.idx, r.followChan)
			r.mu.Lock()
			if ctx.Err() != nil && r.followChan != nil {
				close(r.followChan)
				r.followChan = nil
			}
			r.mu.Unlock()
		}(ctx)
		r.ctx = ctx
	}

	r.mu.Lock()
	followChan := r.followChan
	r.mu.Unlock()

	select {
	case e, ok := <-followChan:
		if !ok {
			return nil
		}
		r.idx++
		return e
	case <-ctx.Done():
		return nil
	}
}

// =============================================================================
// 데모 실행
// =============================================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║     Hubble 링 버퍼 시뮬레이션                               ║")
	fmt.Println("║     참조: pkg/hubble/container/ring.go                      ║")
	fmt.Println("║           pkg/hubble/container/ring_reader.go               ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")

	// --- 데모 1: 기본 쓰기/읽기 ---
	fmt.Println("\n=== 데모 1: 기본 쓰기/읽기 (용량=7, 2^3-1) ===")
	ring := NewRing(7)
	fmt.Printf("  Cap=%d, Len=%d\n", ring.Cap(), ring.Len())

	// 5개 이벤트 쓰기
	for i := 1; i <= 5; i++ {
		ring.Write(&Event{
			Timestamp: time.Now(),
			Message:   fmt.Sprintf("플로우 #%d: 10.0.0.1 -> 10.0.0.%d", i, i+1),
		})
	}
	fmt.Printf("  쓰기 후: Len=%d, LastWrite=%d, OldestWrite=%d\n",
		ring.Len(), ring.LastWrite(), ring.OldestWrite())

	// 순방향 읽기
	fmt.Println("\n  [순방향 읽기]")
	reader := NewRingReader(ring, ring.OldestWrite())
	for i := 0; i < 5; i++ {
		e, err := reader.Next()
		if err != nil {
			fmt.Printf("  읽기 종료: %v\n", err)
			break
		}
		fmt.Printf("    %s\n", e)
	}

	// --- 데모 2: 오버플로우 (순환 덮어쓰기) ---
	fmt.Println("\n=== 데모 2: 오버플로우 - 용량 초과 시 가장 오래된 데이터 덮어쓰기 ===")
	smallRing := NewRing(3) // 용량 3, 배열 크기 4

	fmt.Println("  6개 이벤트를 용량 3인 버퍼에 쓰기:")
	for i := 1; i <= 6; i++ {
		smallRing.Write(&Event{
			Timestamp: time.Now(),
			Message:   fmt.Sprintf("이벤트-%d", i),
		})
		fmt.Printf("    쓰기 #%d: Len=%d, LastWrite=%d, OldestWrite=%d\n",
			i, smallRing.Len(), smallRing.LastWrite(), smallRing.OldestWrite())
	}

	// 최신 데이터만 남아있어야 함
	fmt.Println("\n  버퍼에 남은 데이터 (최신 3개만):")
	overflowReader := NewRingReader(smallRing, smallRing.OldestWrite())
	for i := 0; i < int(smallRing.Len()); i++ {
		e, err := overflowReader.Next()
		if err != nil {
			break
		}
		fmt.Printf("    %s\n", e)
	}

	// --- 데모 3: Follow 모드 (실시간 스트리밍) ---
	fmt.Println("\n=== 데모 3: Follow 모드 - writer를 따라가며 실시간 읽기 ===")
	followRing := NewRing(15) // 용량 15

	// 초기 데이터 쓰기
	for i := 1; i <= 3; i++ {
		followRing.Write(&Event{
			Timestamp: time.Now(),
			Message:   fmt.Sprintf("초기 플로우 #%d", i),
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	followReader := NewRingReader(followRing, followRing.OldestWrite())

	// Reader goroutine (follow 모드)
	wg.Add(1)
	go func() {
		defer wg.Done()
		count := 0
		for {
			e := followReader.NextFollow(ctx)
			if e == nil {
				fmt.Printf("  [Reader] 종료 (읽은 이벤트: %d개)\n", count)
				return
			}
			count++
			fmt.Printf("  [Reader] 수신: %s\n", e)
		}
	}()

	// Writer goroutine (주기적으로 이벤트 쓰기)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 4; i <= 8; i++ {
			time.Sleep(200 * time.Millisecond)
			e := &Event{
				Timestamp: time.Now(),
				Message:   fmt.Sprintf("실시간 플로우 #%d", i),
			}
			followRing.Write(e)
			fmt.Printf("  [Writer] 전송: %s\n", e)
		}
	}()

	wg.Wait()

	// --- 데모 4: 사이클 카운터 시각화 ---
	fmt.Println("\n=== 데모 4: 사이클 카운터와 마스크 연산 시각화 ===")
	vizRing := NewRing(7) // 용량 7 = 2^3 - 1, dataLen=8

	fmt.Println("  용량: 7 (2^3 - 1)")
	fmt.Println("  배열 크기: 8 (2^3)")
	fmt.Printf("  마스크: 0x%x (이진: %08b)\n", vizRing.mask, vizRing.mask)
	fmt.Printf("  사이클 지수: %d\n", vizRing.cycleExp)
	fmt.Printf("  사이클 마스크: 0x%x\n", vizRing.cycleMask)

	fmt.Println("\n  쓰기 위치 → 인덱스 및 사이클 매핑:")
	fmt.Println("  write  |  index (write & mask)  |  cycle (write >> cycleExp)")
	fmt.Println("  -------|------------------------|---------------------------")
	for i := uint64(0); i <= 24; i++ {
		idx := i & vizRing.mask
		cycle := i >> vizRing.cycleExp
		marker := ""
		if idx == 0 && i > 0 {
			marker = " ← 사이클 전환"
		}
		fmt.Printf("  %5d  |  %5d                |  %5d%s\n", i, idx, cycle, marker)
	}

	// 다이어그램
	fmt.Println("\n" + `
┌──────────────────────────────────────────────────────────────────────┐
│                    링 버퍼 내부 구조                                  │
├──────────────────────────────────────────────────────────────────────┤
│                                                                      │
│  Ring{mask, write(atomic), cycleExp, cycleMask, halfCycle, data[]}   │
│                                                                      │
│  용량 = 2^n - 1 (예: 7 = 2^3 - 1)                                   │
│  배열 크기 = 2^n (예: 8) — 하나는 쓰기 예약 슬롯                    │
│                                                                      │
│  인덱스 계산: writeIdx = write & mask (비트 AND)                     │
│  사이클 계산: cycle = write >> cycleExp (비트 시프트)                 │
│                                                                      │
│     ┌───┬───┬───┬───┬───┬───┬───┬───┐                                │
│     │ 0 │ 1 │ 2 │ 3 │ 4 │ 5 │ 6 │ 7 │  ← data 배열               │
│     └───┴───┴───┴───┴───┴───┴───┴───┘                                │
│       ^                           ^                                  │
│       │                           │                                  │
│   OldestWrite              LastWrite                                 │
│       │                           │                                  │
│       └── 유효한 읽기 범위 ───────┘                                  │
│                                                                      │
│  Write(event):                                                       │
│    1. notifyMu.Lock()                                                │
│    2. write.Add(1)        ← atomic 증가                              │
│    3. data[idx] = event   ← 저장                                     │
│    4. close(notifyCh)     ← reader 깨우기                            │
│    5. notifyMu.Unlock()                                              │
│                                                                      │
│  read(pos) 유효성 판별:                                              │
│    readCycle vs writeCycle 비교                                       │
│    ├── 같은 사이클 + readIdx < lastWriteIdx → 유효                   │
│    ├── 이전 사이클 + readIdx > lastWriteIdx → 유효 (wraparound)      │
│    ├── reader가 writer 앞 → io.EOF                                   │
│    └── reader가 writer 뒤 → LostEvent                                │
│                                                                      │
└──────────────────────────────────────────────────────────────────────┘`)
}
