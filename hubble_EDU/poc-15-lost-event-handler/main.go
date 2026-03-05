package main

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

// =============================================================================
// Hubble 유실 이벤트 핸들러 시뮬레이션
//
// 실제 구현 참조:
//   cilium/pkg/hubble/observer/local_observer.go - GetFlows()의 lostEventCounter
//   cilium/pkg/hubble/container/ring_reader.go   - Next(), NextFollow()
//   cilium/pkg/hubble/container/ring.go          - Write(), read()
//
// 핵심 개념:
//   1. 링 버퍼 오버플로우: writer가 reader를 추월 (ErrInvalidRead)
//   2. LostEvent 생성: 추월 감지 시 LostEvent 타입 이벤트 삽입
//   3. Rate Limiting: IntervalRangeCounter로 10초 간격 배치 보고
//   4. 모니터링 카운터: 유실 이벤트 수 추적
// =============================================================================

// --- 이벤트 타입 ---

type EventType int

const (
	EventTypeFlow EventType = iota
	EventTypeLostEvent
)

type LostEventSource string

const (
	LostSourceHubbleRingBuffer LostEventSource = "HUBBLE_RING_BUFFER"
	LostSourcePerfEventBuffer  LostEventSource = "PERF_EVENT_RING_BUFFER"
)

// Flow는 네트워크 이벤트
type Flow struct {
	Time    time.Time
	Source  string
	Dest    string
	Verdict string
}

// LostEvent는 이벤트 유실을 알리는 메타 이벤트
// 실제: flowpb.LostEvent
type LostEvent struct {
	Source        LostEventSource
	NumEventsLost uint64
	First         time.Time // 유실 시작 시간
	Last          time.Time // 유실 종료 시간
}

// Event는 Flow 또는 LostEvent를 래핑
type Event struct {
	Timestamp time.Time
	Event     interface{}
}

func (e *Event) GetLostEvent() *LostEvent {
	if le, ok := e.Event.(*LostEvent); ok {
		return le
	}
	return nil
}

// --- 링 버퍼 ---
// 실제: container.Ring

type Ring struct {
	mu      sync.RWMutex
	data    []*Event
	write   uint64
	cap     uint64
	notify  []chan struct{}
	metrics RingMetrics
}

type RingMetrics struct {
	totalWrites    atomic.Uint64
	totalOverflows atomic.Uint64
}

func NewRing(capacity uint64) *Ring {
	return &Ring{
		data: make([]*Event, capacity),
		cap:  capacity,
	}
}

// Write는 이벤트를 링 버퍼에 쓴다
// 오버플로우 시: 가장 오래된 이벤트가 덮어씌워지고
// LostEvent가 해당 위치에 삽입될 수 있다
func (r *Ring) Write(ev *Event) {
	r.mu.Lock()

	// 오버플로우 감지: 버퍼가 가득 찬 상태에서 새 쓰기
	if r.write >= r.cap {
		r.metrics.totalOverflows.Add(1)
	}

	idx := r.write % r.cap
	r.data[idx] = ev
	r.write++
	r.metrics.totalWrites.Add(1)

	// 알림
	for _, ch := range r.notify {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	r.mu.Unlock()
}

// read는 지정된 인덱스의 이벤트를 읽는다
// 실제: container.Ring.read()
func (r *Ring) read(idx uint64) (*Event, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.write == 0 {
		return nil, fmt.Errorf("EOF: 버퍼 비어있음")
	}
	if idx >= r.write {
		return nil, fmt.Errorf("EOF: reader가 writer보다 앞에 있음")
	}

	// ErrInvalidRead: writer가 reader를 추월
	oldest := uint64(0)
	if r.write > r.cap {
		oldest = r.write - r.cap
	}
	if idx < oldest {
		return nil, fmt.Errorf("ErrInvalidRead: 위치 %d는 이미 덮어씌워짐 (oldest=%d, write=%d)",
			idx, oldest, r.write)
	}

	return r.data[idx%r.cap], nil
}

func (r *Ring) LastWrite() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.write == 0 {
		return 0
	}
	return r.write - 1
}

func (r *Ring) OldestWrite() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.write <= r.cap {
		return 0
	}
	return r.write - r.cap
}

func (r *Ring) RegisterNotify() chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	ch := make(chan struct{}, 1)
	r.notify = append(r.notify, ch)
	return ch
}

// --- RingReader ---
// 실제: container.RingReader

type RingReader struct {
	ring *Ring
	idx  uint64
}

func NewRingReader(ring *Ring, start uint64) *RingReader {
	return &RingReader{ring: ring, idx: start}
}

// Next는 다음 이벤트를 읽는다
// ErrInvalidRead 발생 시: writer가 reader를 추월
// → 이것이 LostEvent를 생성해야 하는 시점
func (r *RingReader) Next() (*Event, error) {
	ev, err := r.ring.read(r.idx)
	if err != nil {
		return nil, err
	}
	r.idx++
	return ev, nil
}

// NextFollow는 새 이벤트가 올 때까지 블로킹
func (r *RingReader) NextFollow(ctx context.Context) *Event {
	ev, err := r.ring.read(r.idx)
	if err == nil {
		r.idx++
		return ev
	}

	notify := r.ring.RegisterNotify()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-notify:
			ev, err := r.ring.read(r.idx)
			if err == nil {
				r.idx++
				return ev
			}
		}
	}
}

// --- IntervalRangeCounter ---
// 실제: counter.IntervalRangeCounter
// 유실 이벤트를 rate-limiting하는 카운터

type IntervalRange struct {
	Count uint64
	First time.Time
	Last  time.Time
}

type IntervalRangeCounter struct {
	mu          sync.Mutex
	interval    time.Duration
	count       uint64
	first       time.Time
	last        time.Time
	lastElapsed time.Time
}

func NewIntervalRangeCounter(interval time.Duration) *IntervalRangeCounter {
	return &IntervalRangeCounter{
		interval: interval,
	}
}

// Increment는 카운터를 증가시킨다
func (c *IntervalRangeCounter) Increment(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.count == 0 {
		c.first = now
	}
	c.count++
	c.last = now
}

// IsElapsed는 interval이 경과했는지 확인
// 카운터가 비어있으면 항상 false 반환
func (c *IntervalRangeCounter) IsElapsed(now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.count == 0 {
		return false
	}
	return now.Sub(c.lastElapsed) >= c.interval
}

// Clear는 카운터를 리셋하고 누적값을 반환
func (c *IntervalRangeCounter) Clear() IntervalRange {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := IntervalRange{
		Count: c.count,
		First: c.first,
		Last:  c.last,
	}
	c.count = 0
	c.first = time.Time{}
	c.last = time.Time{}
	c.lastElapsed = time.Now()
	return r
}

// --- GetFlows에서의 LostEvent 처리 ---
// 실제: LocalObserverServer.GetFlows()의 LostEvent 처리 로직

type GetFlowsResponse struct {
	Flow       *Flow
	LostEvents *LostEvent
	Time       time.Time
	NodeName   string
}

// SimulateGetFlows는 GetFlows RPC를 시뮬레이션
func SimulateGetFlows(
	ctx context.Context,
	ring *Ring,
	follow bool,
	lostEventInterval time.Duration,
	sendFn func(*GetFlowsResponse) error,
) error {
	reader := NewRingReader(ring, ring.OldestWrite())

	// 유실 이벤트 rate-limiter
	// 실제: counter.NewIntervalRangeCounter(s.opts.LostEventSendInterval)
	lostEventCounter := NewIntervalRangeCounter(lostEventInterval)

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		now := time.Now()

		// rate-limited LostEvent 전송
		// 실제: GetFlows의 lostEventCounter.IsElapsed(now) 처리
		if lostEventCounter.IsElapsed(now) {
			count := lostEventCounter.Clear()
			resp := &GetFlowsResponse{
				Time:     now,
				NodeName: "local-node",
				LostEvents: &LostEvent{
					Source:        LostSourceHubbleRingBuffer,
					NumEventsLost: count.Count,
					First:         count.First,
					Last:          count.Last,
				},
			}
			if err := sendFn(resp); err != nil {
				return err
			}
		}

		// 이벤트 읽기
		var ev *Event
		if follow {
			ev = reader.NextFollow(ctx)
			if ev == nil {
				return nil
			}
		} else {
			var err error
			ev, err = reader.Next()
			if err != nil {
				return nil // EOF 또는 에러
			}
		}

		// LostEvent 타입 분기
		// 실제: GetFlows의 switch ev := e.Event.(type) 처리
		switch typedEv := ev.Event.(type) {
		case *Flow:
			resp := &GetFlowsResponse{
				Flow:     typedEv,
				Time:     ev.Timestamp,
				NodeName: "local-node",
			}
			if err := sendFn(resp); err != nil {
				return err
			}

		case *LostEvent:
			// Hubble 링 버퍼 유실: rate-limiting 적용
			// 다른 소스의 유실: 즉시 전달
			switch typedEv.Source {
			case LostSourceHubbleRingBuffer:
				lostEventCounter.Increment(now)
			default:
				resp := &GetFlowsResponse{
					Time:       ev.Timestamp,
					NodeName:   "local-node",
					LostEvents: typedEv,
				}
				if err := sendFn(resp); err != nil {
					return err
				}
			}
		}
	}
}

func main() {
	fmt.Println("=== Hubble 유실 이벤트 핸들러 시뮬레이션 ===")
	fmt.Println()
	fmt.Println("참조: cilium/pkg/hubble/observer/local_observer.go - GetFlows() lostEventCounter")
	fmt.Println("참조: cilium/pkg/hubble/container/ring_reader.go   - Next(), ErrInvalidRead")
	fmt.Println("참조: cilium/pkg/hubble/container/ring.go          - Write(), read()")
	fmt.Println()

	// === 테스트 1: 링 버퍼 오버플로우 감지 ===
	fmt.Println("--- 테스트 1: 링 버퍼 오버플로우 감지 ---")
	fmt.Println("  (버퍼 크기: 5, 10개 이벤트 쓰기)")
	fmt.Println()

	ring := NewRing(5)

	// 10개 이벤트 쓰기 → 5개는 덮어씌워짐
	for i := 0; i < 10; i++ {
		ring.Write(&Event{
			Timestamp: time.Now(),
			Event: &Flow{
				Time:    time.Now(),
				Source:  fmt.Sprintf("pod-%d", i),
				Dest:    "backend",
				Verdict: "FORWARDED",
			},
		})
	}

	fmt.Printf("  총 쓰기: %d, 오버플로우: %d\n",
		ring.metrics.totalWrites.Load(), ring.metrics.totalOverflows.Load())
	fmt.Printf("  oldest=%d, lastWrite=%d\n", ring.OldestWrite(), ring.LastWrite())

	// 오래된 인덱스 읽기 시도 → ErrInvalidRead
	_, err := ring.read(2)
	fmt.Printf("  read(2) 결과: %v\n", err)

	// 유효한 인덱스 읽기
	ev, err := ring.read(5)
	if err == nil {
		fmt.Printf("  read(5) 결과: 성공 (source=%s)\n", ev.Event.(*Flow).Source)
	}
	fmt.Println()

	// === 테스트 2: LostEvent 삽입 ===
	fmt.Println("--- 테스트 2: LostEvent 삽입 (writer 추월 시) ---")
	fmt.Println()

	lostRing := NewRing(5)

	// 정상 이벤트 5개
	for i := 0; i < 5; i++ {
		lostRing.Write(&Event{
			Timestamp: time.Now(),
			Event:     &Flow{Time: time.Now(), Source: fmt.Sprintf("flow-%d", i)},
		})
	}

	// 오버플로우 발생 - LostEvent 삽입
	lostRing.Write(&Event{
		Timestamp: time.Now(),
		Event: &LostEvent{
			Source:        LostSourceHubbleRingBuffer,
			NumEventsLost: 3,
			First:         time.Now().Add(-1 * time.Second),
			Last:          time.Now(),
		},
	})

	// 추가 이벤트
	for i := 5; i < 8; i++ {
		lostRing.Write(&Event{
			Timestamp: time.Now(),
			Event:     &Flow{Time: time.Now(), Source: fmt.Sprintf("flow-%d", i)},
		})
	}

	// 읽기
	reader := NewRingReader(lostRing, lostRing.OldestWrite())
	fmt.Printf("  링 버퍼 내용 (oldest=%d ~ lastWrite=%d):\n", lostRing.OldestWrite(), lostRing.LastWrite())
	for {
		ev, err := reader.Next()
		if err != nil {
			break
		}
		switch typedEv := ev.Event.(type) {
		case *Flow:
			fmt.Printf("    [Flow] source=%s\n", typedEv.Source)
		case *LostEvent:
			fmt.Printf("    [LostEvent] source=%s, lost=%d\n", typedEv.Source, typedEv.NumEventsLost)
		}
	}
	fmt.Println()

	// === 테스트 3: IntervalRangeCounter (Rate Limiting) ===
	fmt.Println("--- 테스트 3: IntervalRangeCounter (유실 이벤트 배치 보고) ---")
	fmt.Println("  (실제: 10초 간격, 여기서는 1초 간격)")
	fmt.Println()

	counter := NewIntervalRangeCounter(1 * time.Second)

	// 빠르게 유실 이벤트 누적
	now := time.Now()
	for i := 0; i < 5; i++ {
		counter.Increment(now.Add(time.Duration(i*100) * time.Millisecond))
	}
	fmt.Printf("  5개 유실 이벤트 누적\n")
	fmt.Printf("  IsElapsed (즉시): %v (아직 1초 미경과)\n", counter.IsElapsed(now))

	// 1초 후 확인
	time.Sleep(1100 * time.Millisecond)
	elapsed := counter.IsElapsed(time.Now())
	fmt.Printf("  IsElapsed (1.1초 후): %v\n", elapsed)

	if elapsed {
		result := counter.Clear()
		fmt.Printf("  배치 보고: count=%d, first=%s, last=%s\n",
			result.Count,
			result.First.Format("15:04:05.000"),
			result.Last.Format("15:04:05.000"),
		)
	}
	fmt.Println()

	// === 테스트 4: GetFlows에서의 LostEvent 처리 ===
	fmt.Println("--- 테스트 4: GetFlows 스트림에서 LostEvent 처리 ---")
	fmt.Println("  (follow 모드, rate-limit 500ms)")
	fmt.Println()

	streamRing := NewRing(100)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// 이벤트 생산자: Flow와 LostEvent를 섞어서 생성
	go func() {
		for i := 0; ; i++ {
			select {
			case <-ctx.Done():
				return
			default:
			}

			if rand.Float32() < 0.2 {
				// 20% 확률로 LostEvent 삽입
				streamRing.Write(&Event{
					Timestamp: time.Now(),
					Event: &LostEvent{
						Source:        LostSourceHubbleRingBuffer,
						NumEventsLost: uint64(1 + rand.Intn(10)),
						First:         time.Now(),
						Last:          time.Now(),
					},
				})
			} else {
				streamRing.Write(&Event{
					Timestamp: time.Now(),
					Event: &Flow{
						Time:    time.Now(),
						Source:  fmt.Sprintf("pod-%d", rand.Intn(5)),
						Dest:    fmt.Sprintf("svc-%d", rand.Intn(3)),
						Verdict: []string{"FORWARDED", "DROPPED"}[rand.Intn(2)],
					},
				})
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()

	// 약간 대기하여 이벤트 축적
	time.Sleep(200 * time.Millisecond)

	flowCount := 0
	lostBatchCount := 0
	totalLostEvents := uint64(0)

	err = SimulateGetFlows(ctx, streamRing, true, 500*time.Millisecond,
		func(resp *GetFlowsResponse) error {
			if resp.Flow != nil {
				flowCount++
				if flowCount <= 5 {
					fmt.Printf("  [수신] Flow #%d: %s -> %s [%s]\n",
						flowCount, resp.Flow.Source, resp.Flow.Dest, resp.Flow.Verdict)
				}
			}
			if resp.LostEvents != nil {
				lostBatchCount++
				totalLostEvents += resp.LostEvents.NumEventsLost
				fmt.Printf("  [수신] LostEvent 배치 #%d: %d개 유실 (source=%s, %s~%s)\n",
					lostBatchCount,
					resp.LostEvents.NumEventsLost,
					resp.LostEvents.Source,
					resp.LostEvents.First.Format("15:04:05.000"),
					resp.LostEvents.Last.Format("15:04:05.000"),
				)
			}
			return nil
		},
	)

	fmt.Printf("\n  요약: Flow=%d개, LostEvent 배치=%d회, 총 유실=%d개\n",
		flowCount, lostBatchCount, totalLostEvents)
	fmt.Println()

	// === 테스트 5: 다양한 유실 소스 구분 ===
	fmt.Println("--- 테스트 5: 유실 이벤트 소스 구분 ---")
	fmt.Println()

	fmt.Println("  Hubble은 두 가지 유실 소스를 구분합니다:")
	fmt.Println()
	fmt.Println("  1. HUBBLE_RING_BUFFER: Hubble 내부 링 버퍼 오버플로우")
	fmt.Println("     → Rate-limited: IntervalRangeCounter로 배치 보고")
	fmt.Println("     → 원인: 관찰자가 Flow를 처리하는 속도보다 빠르게 생성")
	fmt.Println()
	fmt.Println("  2. PERF_EVENT_RING_BUFFER: 커널 perf 이벤트 버퍼 오버플로우")
	fmt.Println("     → 즉시 전달 (rate-limiting 없음)")
	fmt.Println("     → 원인: eBPF 프로그램이 이벤트를 커널에서 사용자 공간으로")
	fmt.Println("       전달할 때 perf 버퍼가 가득 참")
	fmt.Println()

	// 소스별 처리 시연
	mixedRing := NewRing(10)
	mixedRing.Write(&Event{
		Timestamp: time.Now(), Event: &Flow{Time: time.Now(), Source: "pod-1", Dest: "pod-2", Verdict: "FORWARDED"},
	})
	mixedRing.Write(&Event{
		Timestamp: time.Now(), Event: &LostEvent{Source: LostSourceHubbleRingBuffer, NumEventsLost: 5},
	})
	mixedRing.Write(&Event{
		Timestamp: time.Now(), Event: &LostEvent{Source: LostSourcePerfEventBuffer, NumEventsLost: 3},
	})
	mixedRing.Write(&Event{
		Timestamp: time.Now(), Event: &Flow{Time: time.Now(), Source: "pod-3", Dest: "pod-4", Verdict: "DROPPED"},
	})

	mixedReader := NewRingReader(mixedRing, 0)
	for {
		ev, err := mixedReader.Next()
		if err != nil {
			break
		}
		switch typedEv := ev.Event.(type) {
		case *Flow:
			fmt.Printf("  [Flow] %s -> %s [%s]\n", typedEv.Source, typedEv.Dest, typedEv.Verdict)
		case *LostEvent:
			rateLimit := "즉시 전달"
			if typedEv.Source == LostSourceHubbleRingBuffer {
				rateLimit = "rate-limited 배치 보고"
			}
			fmt.Printf("  [Lost] source=%s, lost=%d → %s\n",
				typedEv.Source, typedEv.NumEventsLost, rateLimit)
		}
	}
	fmt.Println()

	fmt.Println("=== 시뮬레이션 완료 ===")
	fmt.Println()
	fmt.Println("핵심 패턴 요약:")
	fmt.Println("  1. 링 버퍼 오버플로우: writer가 reader 추월 시 ErrInvalidRead")
	fmt.Println("  2. LostEvent: 유실된 이벤트 수와 시간 범위를 기록하는 메타 이벤트")
	fmt.Println("  3. IntervalRangeCounter: 유실 이벤트를 배치로 묶어 rate-limiting")
	fmt.Println("     (실제: 10초 간격, opts.LostEventSendInterval)")
	fmt.Println("  4. 소스별 처리: HUBBLE_RING_BUFFER는 rate-limit, 나머지는 즉시 전달")
	fmt.Println("  5. 배치 보고: count/first/last 포함하여 유실 규모와 기간 파악 가능")
}
