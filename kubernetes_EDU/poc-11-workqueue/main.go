package main

import (
	"container/heap"
	"fmt"
	"math"
	"sync"
	"time"
)

// =============================================================================
// Kubernetes client-go 3계층 WorkQueue 시뮬레이션
//
// 실제 구현 참조:
//   - staging/src/k8s.io/client-go/util/workqueue/queue.go (기본 큐)
//   - staging/src/k8s.io/client-go/util/workqueue/delaying_queue.go (지연 큐)
//   - staging/src/k8s.io/client-go/util/workqueue/rate_limiting_queue.go (속도제한 큐)
//   - staging/src/k8s.io/client-go/util/workqueue/default_rate_limiters.go (RateLimiter 구현체)
//
// 3계층 구조:
//   1. BasicQueue: dirty set + processing set + FIFO, 중복 제거
//   2. DelayingQueue: priority queue(min-heap)로 지연 아이템 관리
//   3. RateLimitingQueue: 지수 백오프 + 토큰 버킷으로 속도 제한
// =============================================================================

// =============================================================================
// Layer 1: BasicQueue (기본 큐)
//
// 실제 workqueue/queue.go의 Typed[T] struct에 대응한다.
//
// 핵심 설계:
//   - dirty set: 처리 대기 중인 아이템 (중복 방지)
//   - processing set: 현재 처리 중인 아이템
//   - queue: FIFO 순서 유지
//
// 중복 제거 동작:
//   - Add(x): x가 이미 dirty에 있으면 무시 (중복 추가 방지)
//   - Add(x): x가 processing에 있으면 dirty에만 표시 (queue에는 안 넣음)
//   - Done(x): x를 processing에서 제거, dirty에 있으면 다시 queue에 넣음
// =============================================================================

// BasicQueue는 dirty/processing set 기반의 기본 작업 큐이다.
type BasicQueue struct {
	cond *sync.Cond

	// queue는 FIFO 순서를 유지하는 아이템 목록
	queue []string

	// dirty는 처리 대기 중인 아이템 집합
	dirty map[string]bool

	// processing은 현재 처리 중인 아이템 집합
	processing map[string]bool

	shuttingDown bool
}

// NewBasicQueue는 새 기본 큐를 생성한다.
func NewBasicQueue() *BasicQueue {
	q := &BasicQueue{
		queue:      []string{},
		dirty:      make(map[string]bool),
		processing: make(map[string]bool),
	}
	q.cond = sync.NewCond(&sync.Mutex{})
	return q
}

// Add는 아이템을 큐에 추가한다.
// 실제 queue.go:227의 Add 메서드에 대응한다.
//
// 동작:
//   1. 셧다운 중이면 무시
//   2. dirty에 이미 있으면 무시 (중복 방지)
//   3. dirty에 추가
//   4. processing에 있으면 queue에는 안 넣음 (Done 시 재추가됨)
//   5. processing에 없으면 queue에 넣고 Signal
func (q *BasicQueue) Add(item string) {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()

	if q.shuttingDown {
		return
	}

	if q.dirty[item] {
		// 이미 dirty에 있음 → 중복 추가 방지
		return
	}

	q.dirty[item] = true

	if q.processing[item] {
		// 현재 처리 중 → Done 시 다시 queue에 들어감
		return
	}

	q.queue = append(q.queue, item)
	q.cond.Signal()
}

// Get은 큐에서 아이템을 꺼낸다. 큐가 비어있으면 블로킹한다.
// 실제 queue.go:265의 Get 메서드에 대응한다.
func (q *BasicQueue) Get() (string, bool) {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()

	for len(q.queue) == 0 && !q.shuttingDown {
		q.cond.Wait()
	}

	if len(q.queue) == 0 {
		return "", true // shutdown
	}

	item := q.queue[0]
	q.queue = q.queue[1:]

	q.processing[item] = true
	delete(q.dirty, item)

	return item, false
}

// Done은 아이템 처리 완료를 알린다.
// 실제 queue.go:289의 Done 메서드에 대응한다.
//
// 핵심: processing에서 제거 후, dirty에 있으면 다시 queue에 넣는다.
// 이는 처리 중에 같은 키가 다시 Add된 경우를 처리한다.
func (q *BasicQueue) Done(item string) {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()

	delete(q.processing, item)
	if q.dirty[item] {
		q.queue = append(q.queue, item)
		q.cond.Signal()
	}
}

// Len은 큐의 길이를 반환한다.
func (q *BasicQueue) Len() int {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()
	return len(q.queue)
}

// ShutDown은 큐를 종료한다.
func (q *BasicQueue) ShutDown() {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()
	q.shuttingDown = true
	q.cond.Broadcast()
}

// =============================================================================
// Layer 2: DelayingQueue (지연 큐)
//
// 실제 workqueue/delaying_queue.go의 delayingType에 대응한다.
//
// BasicQueue를 감싸서 AddAfter(item, duration) 기능을 추가한다.
// min-heap(priority queue)으로 지연 아이템을 관리하며,
// 별도 goroutine(waitingLoop)이 시간이 도래한 아이템을 기본 큐로 이동시킨다.
// =============================================================================

// waitFor는 지연 대기 중인 아이템 정보이다.
// 실제 delaying_queue.go:184의 waitFor에 대응한다.
type waitFor struct {
	data    string
	readyAt time.Time
	index   int // heap에서의 인덱스
}

// waitForPriorityQueue는 min-heap으로 가장 이른 readyAt이 루트에 위치한다.
// 실제 delaying_queue.go:199의 waitForPriorityQueue에 대응한다.
type waitForPriorityQueue []*waitFor

func (pq waitForPriorityQueue) Len() int            { return len(pq) }
func (pq waitForPriorityQueue) Less(i, j int) bool   { return pq[i].readyAt.Before(pq[j].readyAt) }
func (pq waitForPriorityQueue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	pq[i].index = i
	pq[j].index = j
}
func (pq *waitForPriorityQueue) Push(x interface{}) {
	n := len(*pq)
	item := x.(*waitFor)
	item.index = n
	*pq = append(*pq, item)
}
func (pq *waitForPriorityQueue) Pop() interface{} {
	old := *pq
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	item.index = -1
	*pq = old[:n-1]
	return item
}
func (pq waitForPriorityQueue) Peek() *waitFor {
	return pq[0]
}

// DelayingQueue는 지연 추가 기능이 있는 큐이다.
type DelayingQueue struct {
	*BasicQueue
	waitingForAddCh chan *waitFor
	stopCh          chan struct{}
	stopOnce        sync.Once
}

// NewDelayingQueue는 새 지연 큐를 생성한다.
func NewDelayingQueue() *DelayingQueue {
	q := &DelayingQueue{
		BasicQueue:      NewBasicQueue(),
		waitingForAddCh: make(chan *waitFor, 1000),
		stopCh:          make(chan struct{}),
	}
	go q.waitingLoop()
	return q
}

// AddAfter는 지정된 시간 후에 아이템을 큐에 추가한다.
// 실제 delaying_queue.go:249의 AddAfter에 대응한다.
func (q *DelayingQueue) AddAfter(item string, duration time.Duration) {
	if duration <= 0 {
		q.Add(item)
		return
	}

	select {
	case <-q.stopCh:
	case q.waitingForAddCh <- &waitFor{data: item, readyAt: time.Now().Add(duration)}:
	}
}

// waitingLoop는 별도 goroutine에서 실행되며, 시간이 도래한 아이템을 기본 큐로 이동시킨다.
// 실제 delaying_queue.go:276의 waitingLoop에 대응한다.
func (q *DelayingQueue) waitingLoop() {
	never := make(<-chan time.Time)
	var nextReadyAtTimer *time.Timer

	waitingForQueue := &waitForPriorityQueue{}
	heap.Init(waitingForQueue)
	waitingEntryByData := make(map[string]*waitFor)

	for {
		if q.BasicQueue.shuttingDown {
			return
		}

		now := time.Now()

		// 시간이 도래한 아이템을 기본 큐로 이동
		for waitingForQueue.Len() > 0 {
			entry := waitingForQueue.Peek()
			if entry.readyAt.After(now) {
				break
			}
			entry = heap.Pop(waitingForQueue).(*waitFor)
			q.BasicQueue.Add(entry.data)
			delete(waitingEntryByData, entry.data)
		}

		// 다음 아이템의 ready 시간에 타이머 설정
		nextReadyAt := never
		if waitingForQueue.Len() > 0 {
			if nextReadyAtTimer != nil {
				nextReadyAtTimer.Stop()
			}
			entry := waitingForQueue.Peek()
			nextReadyAtTimer = time.NewTimer(entry.readyAt.Sub(now))
			nextReadyAt = nextReadyAtTimer.C
		}

		select {
		case <-q.stopCh:
			return
		case <-nextReadyAt:
			// 타이머 만료 → 루프 재실행하여 ready 아이템 처리
		case waitEntry := <-q.waitingForAddCh:
			if waitEntry.readyAt.After(time.Now()) {
				// 아직 시간이 안 됨 → heap에 추가
				// 이미 존재하는 경우 더 이른 시간으로 업데이트
				if existing, exists := waitingEntryByData[waitEntry.data]; exists {
					if existing.readyAt.After(waitEntry.readyAt) {
						existing.readyAt = waitEntry.readyAt
						heap.Fix(waitingForQueue, existing.index)
					}
				} else {
					heap.Push(waitingForQueue, waitEntry)
					waitingEntryByData[waitEntry.data] = waitEntry
				}
			} else {
				q.BasicQueue.Add(waitEntry.data)
			}

			// 채널에 남아있는 아이템도 모두 처리
			drained := false
			for !drained {
				select {
				case waitEntry := <-q.waitingForAddCh:
					if waitEntry.readyAt.After(time.Now()) {
						if existing, exists := waitingEntryByData[waitEntry.data]; exists {
							if existing.readyAt.After(waitEntry.readyAt) {
								existing.readyAt = waitEntry.readyAt
								heap.Fix(waitingForQueue, existing.index)
							}
						} else {
							heap.Push(waitingForQueue, waitEntry)
							waitingEntryByData[waitEntry.data] = waitEntry
						}
					} else {
						q.BasicQueue.Add(waitEntry.data)
					}
				default:
					drained = true
				}
			}
		}
	}
}

// ShutDown은 지연 큐를 종료한다.
func (q *DelayingQueue) ShutDown() {
	q.stopOnce.Do(func() {
		q.BasicQueue.ShutDown()
		close(q.stopCh)
	})
}

// =============================================================================
// Layer 3: RateLimitingQueue (속도 제한 큐)
//
// 실제 workqueue/rate_limiting_queue.go의 rateLimitingType에 대응한다.
//
// DelayingQueue를 감싸서 AddRateLimited(item) 기능을 추가한다.
// RateLimiter가 각 아이템의 재시도 대기 시간을 결정한다.
// =============================================================================

// RateLimiter는 아이템별 재시도 대기 시간을 결정하는 인터페이스이다.
// 실제 default_rate_limiters.go:30의 TypedRateLimiter에 대응한다.
type RateLimiter interface {
	// When은 아이템의 대기 시간을 반환한다.
	When(item string) time.Duration
	// Forget은 아이템의 실패 이력을 초기화한다.
	Forget(item string)
	// NumRequeues는 아이템의 재시도 횟수를 반환한다.
	NumRequeues(item string) int
}

// --- ExponentialBackoffRateLimiter ---

// ExponentialBackoffRateLimiter는 아이템별 지수 백오프를 적용한다.
// 실제 default_rate_limiters.go:84의 TypedItemExponentialFailureRateLimiter에 대응한다.
// 공식: baseDelay * 2^(failures)
type ExponentialBackoffRateLimiter struct {
	mu        sync.Mutex
	failures  map[string]int
	baseDelay time.Duration
	maxDelay  time.Duration
}

func NewExponentialBackoffRateLimiter(baseDelay, maxDelay time.Duration) *ExponentialBackoffRateLimiter {
	return &ExponentialBackoffRateLimiter{
		failures:  make(map[string]int),
		baseDelay: baseDelay,
		maxDelay:  maxDelay,
	}
}

// When은 baseDelay * 2^failures를 계산한다.
// 실제 default_rate_limiters.go:116에 대응한다.
func (r *ExponentialBackoffRateLimiter) When(item string) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()

	exp := r.failures[item]
	r.failures[item]++

	// 오버플로우 방지
	backoff := float64(r.baseDelay.Nanoseconds()) * math.Pow(2, float64(exp))
	if backoff > math.MaxInt64 {
		return r.maxDelay
	}

	calculated := time.Duration(backoff)
	if calculated > r.maxDelay {
		return r.maxDelay
	}

	return calculated
}

func (r *ExponentialBackoffRateLimiter) Forget(item string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.failures, item)
}

func (r *ExponentialBackoffRateLimiter) NumRequeues(item string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failures[item]
}

// --- TokenBucketRateLimiter ---

// TokenBucketRateLimiter는 전역 속도 제한을 적용한다 (아이템 무관).
// 실제 default_rate_limiters.go:62의 TypedBucketRateLimiter에 대응한다.
// 토큰 버킷 알고리즘: rate개/초 속도로 토큰이 충전되며, burst개까지 저장 가능.
type TokenBucketRateLimiter struct {
	mu       sync.Mutex
	rate     float64   // 초당 토큰 충전 속도
	burst    int       // 최대 토큰 수
	tokens   float64   // 현재 토큰 수
	lastTime time.Time // 마지막 토큰 갱신 시간
}

func NewTokenBucketRateLimiter(ratePerSecond float64, burst int) *TokenBucketRateLimiter {
	return &TokenBucketRateLimiter{
		rate:     ratePerSecond,
		burst:    burst,
		tokens:   float64(burst),
		lastTime: time.Now(),
	}
}

func (r *TokenBucketRateLimiter) When(item string) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(r.lastTime).Seconds()
	r.lastTime = now

	// 토큰 충전
	r.tokens += elapsed * r.rate
	if r.tokens > float64(r.burst) {
		r.tokens = float64(r.burst)
	}

	if r.tokens >= 1.0 {
		r.tokens -= 1.0
		return 0
	}

	// 토큰이 부족 → 대기 시간 계산
	waitTime := (1.0 - r.tokens) / r.rate
	r.tokens = 0
	return time.Duration(waitTime * float64(time.Second))
}

func (r *TokenBucketRateLimiter) Forget(item string)      {}
func (r *TokenBucketRateLimiter) NumRequeues(item string) int { return 0 }

// --- MaxOfRateLimiter ---

// MaxOfRateLimiter는 여러 RateLimiter 중 최대 대기 시간을 반환한다.
// 실제 default_rate_limiters.go:218의 TypedMaxOfRateLimiter에 대응한다.
type MaxOfRateLimiter struct {
	limiters []RateLimiter
}

func NewMaxOfRateLimiter(limiters ...RateLimiter) *MaxOfRateLimiter {
	return &MaxOfRateLimiter{limiters: limiters}
}

func (r *MaxOfRateLimiter) When(item string) time.Duration {
	ret := time.Duration(0)
	for _, limiter := range r.limiters {
		curr := limiter.When(item)
		if curr > ret {
			ret = curr
		}
	}
	return ret
}

func (r *MaxOfRateLimiter) NumRequeues(item string) int {
	ret := 0
	for _, limiter := range r.limiters {
		curr := limiter.NumRequeues(item)
		if curr > ret {
			ret = curr
		}
	}
	return ret
}

func (r *MaxOfRateLimiter) Forget(item string) {
	for _, limiter := range r.limiters {
		limiter.Forget(item)
	}
}

// --- RateLimitingQueue ---

// RateLimitingQueue는 속도 제한이 적용된 큐이다.
// 실제 rate_limiting_queue.go:130의 rateLimitingType에 대응한다.
type RateLimitingQueue struct {
	*DelayingQueue
	rateLimiter RateLimiter
}

func NewRateLimitingQueue(limiter RateLimiter) *RateLimitingQueue {
	return &RateLimitingQueue{
		DelayingQueue: NewDelayingQueue(),
		rateLimiter:   limiter,
	}
}

// AddRateLimited는 RateLimiter가 결정한 시간 후에 아이템을 큐에 추가한다.
// 실제 rate_limiting_queue.go:137에 대응한다:
//   q.DelayingInterface.AddAfter(item, q.rateLimiter.When(item))
func (q *RateLimitingQueue) AddRateLimited(item string) {
	q.DelayingQueue.AddAfter(item, q.rateLimiter.When(item))
}

// Forget은 아이템의 실패 이력을 초기화한다.
func (q *RateLimitingQueue) Forget(item string) {
	q.rateLimiter.Forget(item)
}

// NumRequeues는 아이템의 재시도 횟수를 반환한다.
func (q *RateLimitingQueue) NumRequeues(item string) int {
	return q.rateLimiter.NumRequeues(item)
}

// --- 데모 실행 ---

func main() {
	fmt.Println("=== Kubernetes 3계층 WorkQueue 시뮬레이션 ===")
	fmt.Println()

	// -----------------------------------------------
	// 1. BasicQueue: dirty/processing 중복 제거
	// -----------------------------------------------
	fmt.Println("--- 1. BasicQueue: dirty/processing set 중복 제거 ---")

	bq := NewBasicQueue()
	bq.Add("pod-a")
	bq.Add("pod-b")
	bq.Add("pod-a") // 이미 dirty에 있음 → 무시됨
	fmt.Printf("  Add(pod-a), Add(pod-b), Add(pod-a 중복) → queue 길이: %d\n", bq.Len())

	// pod-a를 Get (processing으로 이동)
	item, _ := bq.Get()
	fmt.Printf("  Get() → %s\n", item)

	// 처리 중에 같은 아이템이 다시 Add됨
	bq.Add("pod-a") // processing에 있으므로 dirty에만 표시
	fmt.Printf("  처리 중 Add(pod-a) → queue 길이: %d (pod-b만 queue에 있음)\n", bq.Len())

	// Done 호출 → dirty에 있으므로 다시 queue에 들어감
	bq.Done("pod-a")
	fmt.Printf("  Done(pod-a) → queue 길이: %d (pod-b + pod-a 재추가됨)\n", bq.Len())

	// 남은 아이템 소비
	item, _ = bq.Get()
	fmt.Printf("  Get() → %s\n", item)
	bq.Done(item)
	item, _ = bq.Get()
	fmt.Printf("  Get() → %s (Done 후 재추가된 아이템)\n", item)
	bq.Done(item)
	bq.ShutDown()
	fmt.Println()

	// -----------------------------------------------
	// 2. DelayingQueue: AddAfter 테스트
	// -----------------------------------------------
	fmt.Println("--- 2. DelayingQueue: 지연 추가 (min-heap) ---")

	dq := NewDelayingQueue()

	// 서로 다른 지연 시간으로 아이템 추가
	fmt.Println("  AddAfter(slow, 300ms)")
	fmt.Println("  AddAfter(fast, 100ms)")
	fmt.Println("  AddAfter(medium, 200ms)")

	dq.AddAfter("slow", 300*time.Millisecond)
	dq.AddAfter("fast", 100*time.Millisecond)
	dq.AddAfter("medium", 200*time.Millisecond)

	// 즉시 추가 (delay=0)
	dq.Add("immediate")
	fmt.Println("  Add(immediate) - 즉시 추가")

	// 순서 확인: immediate → fast → medium → slow
	fmt.Println("  수신 순서:")
	for i := 0; i < 4; i++ {
		item, shutdown := dq.Get()
		if shutdown {
			break
		}
		fmt.Printf("    [%d] %s (time=%s)\n", i+1, item, time.Now().Format("15:04:05.000"))
		dq.Done(item)
	}
	dq.ShutDown()
	fmt.Println()

	// -----------------------------------------------
	// 3. ExponentialBackoff RateLimiter
	// -----------------------------------------------
	fmt.Println("--- 3. 지수 백오프 RateLimiter ---")

	expLimiter := NewExponentialBackoffRateLimiter(10*time.Millisecond, 1*time.Second)

	item3 := "failed-pod"
	for i := 0; i < 8; i++ {
		delay := expLimiter.When(item3)
		fmt.Printf("  attempt %d: delay=%v (requeues=%d)\n", i+1, delay, expLimiter.NumRequeues(item3))
	}

	fmt.Println("  Forget 호출 후:")
	expLimiter.Forget(item3)
	delay := expLimiter.When(item3)
	fmt.Printf("  attempt 1 (reset): delay=%v\n", delay)
	fmt.Println()

	// -----------------------------------------------
	// 4. RateLimitingQueue: 컨트롤러 재시도 패턴
	// -----------------------------------------------
	fmt.Println("--- 4. RateLimitingQueue: 컨트롤러 재시도 패턴 ---")

	// 실제 DefaultControllerRateLimiter와 유사한 설정
	// 지수 백오프 (10ms ~ 1s) + 토큰 버킷 (100/s, burst 50)
	limiter := NewMaxOfRateLimiter(
		NewExponentialBackoffRateLimiter(10*time.Millisecond, 500*time.Millisecond),
		NewTokenBucketRateLimiter(100, 50),
	)
	rlq := NewRateLimitingQueue(limiter)

	// 컨트롤러 패턴 시뮬레이션
	fmt.Println("  시뮬레이션: Pod 처리 중 에러 → 재시도 → 성공")

	// 작업 아이템 추가
	rlq.Add("default/flaky-pod")

	maxRetries := 5
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		for attempt := 0; attempt < maxRetries+1; attempt++ {
			item, shutdown := rlq.Get()
			if shutdown {
				return
			}

			// 처리 시뮬레이션
			success := attempt >= 2 // 3번째 시도에 성공
			if success {
				fmt.Printf("    [attempt %d] %s: 성공! Forget 호출\n", attempt+1, item)
				rlq.Forget(item)
				rlq.Done(item)
				return
			}

			retries := rlq.NumRequeues(item)
			fmt.Printf("    [attempt %d] %s: 실패 (requeues=%d), AddRateLimited으로 재시도 예약\n",
				attempt+1, item, retries)

			// Done → AddRateLimited 순서가 중요!
			// Done을 먼저 호출해야 processing에서 제거되어 AddRateLimited이 정상 동작한다.
			rlq.Done(item)
			rlq.AddRateLimited(item)
		}
	}()

	wg.Wait()
	rlq.ShutDown()
	fmt.Println()

	// -----------------------------------------------
	// 5. Done → Re-Add 패턴 상세 설명
	// -----------------------------------------------
	fmt.Println("--- 5. Done() + Re-Add 동작 원리 ---")
	fmt.Println()
	fmt.Println("  컨트롤러 재시도에서의 순서:")
	fmt.Println()
	fmt.Println("  item := queue.Get()     // processing에 추가, dirty에서 제거")
	fmt.Println("  err := processItem(item)")
	fmt.Println("  if err != nil {")
	fmt.Println("      queue.Done(item)         // processing에서 제거 (먼저!)")
	fmt.Println("      queue.AddRateLimited(item) // dirty에 추가, 지연 후 queue에 추가")
	fmt.Println("  } else {")
	fmt.Println("      queue.Forget(item)        // 실패 이력 초기화")
	fmt.Println("      queue.Done(item)           // processing에서 제거")
	fmt.Println("  }")
	fmt.Println()
	fmt.Println("  Done()을 먼저 호출하지 않으면:")
	fmt.Println("    - processing에 item이 남아있음")
	fmt.Println("    - AddRateLimited → Add 호출 시 dirty에만 추가됨")
	fmt.Println("    - 이후 Done 호출 시 dirty에 있으므로 queue에 재추가됨")
	fmt.Println("    - 결과적으로 동작은 하지만, 지연이 적용되지 않을 수 있음")
	fmt.Println()

	// -----------------------------------------------
	// 6. 3계층 아키텍처 요약
	// -----------------------------------------------
	fmt.Println("--- 6. 3계층 아키텍처 요약 ---")
	fmt.Println()
	fmt.Println("  ┌─────────────────────────────────────────────┐")
	fmt.Println("  │ RateLimitingQueue                           │")
	fmt.Println("  │  AddRateLimited(item)                       │")
	fmt.Println("  │    → rateLimiter.When(item) 으로 delay 계산  │")
	fmt.Println("  │    → DelayingQueue.AddAfter(item, delay)    │")
	fmt.Println("  ├─────────────────────────────────────────────┤")
	fmt.Println("  │ DelayingQueue                               │")
	fmt.Println("  │  AddAfter(item, duration)                   │")
	fmt.Println("  │    → min-heap에 추가                         │")
	fmt.Println("  │    → waitingLoop이 시간 도래 시               │")
	fmt.Println("  │    → BasicQueue.Add(item)                   │")
	fmt.Println("  ├─────────────────────────────────────────────┤")
	fmt.Println("  │ BasicQueue                                  │")
	fmt.Println("  │  Add(item) / Get() / Done(item)             │")
	fmt.Println("  │    → dirty set: 중복 방지                    │")
	fmt.Println("  │    → processing set: 처리 중 추적            │")
	fmt.Println("  │    → FIFO queue: 순서 보장                   │")
	fmt.Println("  └─────────────────────────────────────────────┘")
	fmt.Println()
	fmt.Println("=== 시뮬레이션 완료 ===")
	fmt.Println()
	fmt.Println("핵심 요약:")
	fmt.Println("  1. BasicQueue는 dirty/processing set으로 중복 처리를 방지한다")
	fmt.Println("  2. DelayingQueue는 min-heap으로 지연 아이템을 시간순으로 관리한다")
	fmt.Println("  3. RateLimitingQueue는 지수 백오프/토큰 버킷으로 재시도 속도를 제한한다")
	fmt.Println("  4. Done() → AddRateLimited() 순서가 올바른 재시도 패턴이다")
	fmt.Println("  5. Forget()으로 성공 시 실패 이력을 초기화해야 한다")
}
