// Alertmanager Rate Limiter PoC
//
// Alertmanager의 용량 제한 메커니즘을 시뮬레이션한다.
// limit/bucket.go의 힙 기반 Bucket과 API의 동시성 제한을 재현한다.
//
// 핵심 개념:
//   - limit.Bucket: Alert 수 제한 (힙 기반 LRU)
//   - API limitHandler: 동시 요청 수 제한
//   - MaxSilences: Silence 수 제한
//   - MaxSilenceSizeBytes: Silence 크기 제한
//
// 실행: go run main.go

package main

import (
	"container/heap"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// === 1. Alert 용량 제한 (limit.Bucket) ===

// AlertEntry는 Bucket의 항목이다.
type AlertEntry struct {
	fingerprint uint64
	insertTime  time.Time
	index       int // 힙 인덱스
}

// AlertHeap은 삽입 시간 기준 최소 힙이다 (가장 오래된 것이 루트).
type AlertHeap []*AlertEntry

func (h AlertHeap) Len() int           { return len(h) }
func (h AlertHeap) Less(i, j int) bool { return h[i].insertTime.Before(h[j].insertTime) }
func (h AlertHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}
func (h *AlertHeap) Push(x any) {
	entry := x.(*AlertEntry)
	entry.index = len(*h)
	*h = append(*h, entry)
}
func (h *AlertHeap) Pop() any {
	old := *h
	n := len(old)
	entry := old[n-1]
	old[n-1] = nil
	entry.index = -1
	*h = old[:n-1]
	return entry
}

// Bucket은 Alert 용량 제한 컨테이너이다.
type Bucket struct {
	capacity int
	mu       sync.Mutex
	entries  map[uint64]*AlertEntry
	heap     AlertHeap
}

// NewBucket은 새 Bucket을 생성한다.
func NewBucket(capacity int) *Bucket {
	b := &Bucket{
		capacity: capacity,
		entries:  make(map[uint64]*AlertEntry),
	}
	heap.Init(&b.heap)
	return b
}

// Add는 Alert를 추가한다. 용량 초과 시 가장 오래된 것을 제거한다.
func (b *Bucket) Add(fingerprint uint64) (evicted *uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// 이미 존재하면 시간 갱신
	if entry, ok := b.entries[fingerprint]; ok {
		entry.insertTime = time.Now()
		heap.Fix(&b.heap, entry.index)
		return nil
	}

	// 용량 초과 시 가장 오래된 제거
	if len(b.entries) >= b.capacity {
		oldest := heap.Pop(&b.heap).(*AlertEntry)
		delete(b.entries, oldest.fingerprint)
		evicted = &oldest.fingerprint
	}

	// 새 항목 추가
	entry := &AlertEntry{
		fingerprint: fingerprint,
		insertTime:  time.Now(),
	}
	b.entries[fingerprint] = entry
	heap.Push(&b.heap, entry)

	return evicted
}

// Count는 현재 항목 수를 반환한다.
func (b *Bucket) Count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.entries)
}

// === 2. API 동시성 제한 (limitHandler) ===

// ConcurrencyLimiter는 동시 요청 수를 제한한다.
type ConcurrencyLimiter struct {
	limit    int
	current  atomic.Int32
	rejected atomic.Int32
}

// NewConcurrencyLimiter는 새 ConcurrencyLimiter를 생성한다.
func NewConcurrencyLimiter(limit int) *ConcurrencyLimiter {
	return &ConcurrencyLimiter{limit: limit}
}

// Acquire는 슬롯을 획득한다. 실패하면 false를 반환한다.
func (cl *ConcurrencyLimiter) Acquire() bool {
	current := cl.current.Add(1)
	if int(current) > cl.limit {
		cl.current.Add(-1)
		cl.rejected.Add(1)
		return false
	}
	return true
}

// Release는 슬롯을 반환한다.
func (cl *ConcurrencyLimiter) Release() {
	cl.current.Add(-1)
}

// Stats는 현재 상태를 반환한다.
func (cl *ConcurrencyLimiter) Stats() (current int32, rejected int32) {
	return cl.current.Load(), cl.rejected.Load()
}

// === 3. Silence 제한 ===

// SilenceLimiter는 Silence 수와 크기를 제한한다.
type SilenceLimiter struct {
	maxSilences     int
	maxSizeBytes    int
	currentCount    int
	mu              sync.Mutex
}

// NewSilenceLimiter는 새 SilenceLimiter를 생성한다.
func NewSilenceLimiter(maxCount, maxSize int) *SilenceLimiter {
	return &SilenceLimiter{
		maxSilences:  maxCount,
		maxSizeBytes: maxSize,
	}
}

// CanAdd는 새 Silence를 추가할 수 있는지 확인한다.
func (sl *SilenceLimiter) CanAdd(sizeBytes int) error {
	sl.mu.Lock()
	defer sl.mu.Unlock()

	if sl.currentCount >= sl.maxSilences {
		return fmt.Errorf("Silence 수 제한 초과 (현재: %d, 최대: %d)", sl.currentCount, sl.maxSilences)
	}

	if sizeBytes > sl.maxSizeBytes {
		return fmt.Errorf("Silence 크기 제한 초과 (크기: %d bytes, 최대: %d bytes)", sizeBytes, sl.maxSizeBytes)
	}

	sl.currentCount++
	return nil
}

func main() {
	fmt.Println("=== Alertmanager Rate Limiter PoC ===")
	fmt.Println()

	// === Part 1: Alert 용량 제한 ===
	fmt.Println("=== Part 1: Alert Bucket (용량 제한) ===")
	bucket := NewBucket(5) // 최대 5개

	fmt.Println("최대 용량: 5")
	fmt.Println()

	// 5개 추가 (용량 내)
	for i := uint64(1); i <= 5; i++ {
		evicted := bucket.Add(i)
		if evicted != nil {
			fmt.Printf("  fp=%d 추가 → fp=%d 제거됨\n", i, *evicted)
		} else {
			fmt.Printf("  fp=%d 추가\n", i)
		}
	}
	fmt.Printf("  현재: %d/%d\n\n", bucket.Count(), bucket.capacity)

	// 6번째 추가 (용량 초과 → 가장 오래된 제거)
	fmt.Println("용량 초과 테스트:")
	for i := uint64(6); i <= 8; i++ {
		time.Sleep(1 * time.Millisecond) // 시간 순서 보장
		evicted := bucket.Add(i)
		if evicted != nil {
			fmt.Printf("  fp=%d 추가 → fp=%d 제거됨 (LRU)\n", i, *evicted)
		}
	}
	fmt.Printf("  현재: %d/%d\n\n", bucket.Count(), bucket.capacity)

	// 기존 항목 갱신 (제거 안 됨)
	fmt.Println("기존 항목 갱신:")
	evicted := bucket.Add(4) // 이미 존재
	if evicted == nil {
		fmt.Printf("  fp=4 갱신 (시간 업데이트, 제거 없음)\n")
	}
	fmt.Println()

	// === Part 2: API 동시성 제한 ===
	fmt.Println("=== Part 2: API 동시성 제한 ===")
	limiter := NewConcurrencyLimiter(3) // 최대 3개 동시 요청

	fmt.Println("최대 동시 요청: 3")
	fmt.Println()

	var wg sync.WaitGroup
	results := make([]string, 6)

	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			if limiter.Acquire() {
				results[id] = fmt.Sprintf("  요청 %d: 허용됨", id)
				time.Sleep(50 * time.Millisecond) // 처리 시뮬레이션
				limiter.Release()
			} else {
				results[id] = fmt.Sprintf("  요청 %d: 거부됨 (503 Too Many Requests)", id)
			}
		}(i)
		time.Sleep(10 * time.Millisecond) // 순차적 시작
	}

	wg.Wait()
	for _, r := range results {
		fmt.Println(r)
	}

	current, rejected := limiter.Stats()
	fmt.Printf("\n  현재 진행: %d, 총 거부: %d\n\n", current, rejected)

	// === Part 3: Silence 제한 ===
	fmt.Println("=== Part 3: Silence 제한 ===")
	silLimiter := NewSilenceLimiter(3, 1024) // 최대 3개, 최대 1KB

	fmt.Println("최대 Silence 수: 3, 최대 크기: 1024 bytes")
	fmt.Println()

	testSilences := []struct {
		name string
		size int
	}{
		{"alertname=HighCPU", 200},
		{"severity=warning", 300},
		{"instance=~'node-.*'", 500},
		{"team=backend", 100},     // 수 초과
		{"huge-silence", 2000},     // 크기 초과
	}

	for _, ts := range testSilences {
		err := silLimiter.CanAdd(ts.size)
		if err != nil {
			fmt.Printf("  %s (%d bytes) → 거부: %v\n", ts.name, ts.size, err)
		} else {
			fmt.Printf("  %s (%d bytes) → 허용\n", ts.name, ts.size)
		}
	}

	fmt.Println()
	fmt.Println("=== 제한 메커니즘 요약 ===")
	fmt.Println("1. Alert Bucket: 힙 기반 LRU, 용량 초과 시 가장 오래된 Alert 제거")
	fmt.Println("2. API 동시성: atomic counter, 초과 시 503 반환")
	fmt.Println("3. Silence 수/크기: MaxSilences, MaxSilenceSizeBytes 제한")
	fmt.Println("4. 모든 제한은 --web.max-* 또는 --silences.max-* 플래그로 설정")
}
