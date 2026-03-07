package main

import (
	"container/heap"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"
)

// =============================================================================
// Loki PoC #08: 반복자 병합 - 다중 소스 로그 스트림의 정렬 병합
// =============================================================================
//
// Loki는 여러 소스(ingester, store, 각 청크)에서 로그를 읽어온 뒤,
// 시간 순서대로 정렬 병합하여 단일 스트림으로 제공한다.
// 이 패턴은 pkg/iter/ 패키지에 구현되어 있으며, Iterator 인터페이스와
// 힙 기반 병합(heap-based merge)이 핵심이다.
//
// 이 PoC는 다음을 시뮬레이션한다:
//   1. EntryIterator 인터페이스 정의
//   2. SliceIterator: 메모리 내 로그 슬라이스를 순회
//   3. HeapIterator: 여러 이터레이터를 힙으로 병합 (forward/backward)
//   4. TimeRangeIterator: 시간 범위 필터링
//   5. LimitIterator: 결과 수 제한
//
// 실행: go run main.go

// =============================================================================
// 1. 핵심 타입 정의
// =============================================================================

// Direction은 로그 정렬 방향을 나타낸다.
type Direction int

const (
	Forward  Direction = iota // 시간순 (오래된 것 먼저) - 기본
	Backward                  // 역시간순 (최신 것 먼저) - tail 쿼리
)

func (d Direction) String() string {
	if d == Forward {
		return "FORWARD"
	}
	return "BACKWARD"
}

// Entry는 하나의 로그 엔트리를 나타낸다.
// Loki 실제 코드: logproto.Entry
type Entry struct {
	Timestamp time.Time
	Line      string
	Labels    string // 소스 식별용 (예: ingester-1, store-chunk-3)
}

func (e Entry) String() string {
	return fmt.Sprintf("[%s] %s: %s",
		e.Timestamp.Format("15:04:05.000"), e.Labels, e.Line)
}

// =============================================================================
// 2. EntryIterator 인터페이스
// =============================================================================
// Loki의 pkg/iter/iterator.go에 정의된 핵심 인터페이스.
// 모든 로그 읽기 작업은 이 인터페이스를 통해 수행된다.

// EntryIterator는 로그 엔트리를 순차적으로 읽는 인터페이스이다.
type EntryIterator interface {
	// Next는 다음 엔트리가 있으면 true를 반환한다.
	Next() bool
	// Entry는 현재 엔트리를 반환한다.
	Entry() Entry
	// Labels는 현재 스트림의 레이블을 반환한다.
	StreamLabels() string
	// Close는 이터레이터 리소스를 해제한다.
	Close() error
	// Err는 반복 중 발생한 에러를 반환한다.
	Err() error
}

// =============================================================================
// 3. SliceIterator - 메모리 내 엔트리 슬라이스를 순회
// =============================================================================
// 테스트 및 인메모리 데이터 소스에 사용된다.
// Loki 실제 코드: pkg/iter/entry_iterator.go의 listEntryIterator

// SliceIterator는 엔트리 슬라이스를 순회하는 이터레이터이다.
type SliceIterator struct {
	entries []Entry
	labels  string
	pos     int
	current Entry
}

// NewSliceIterator는 엔트리 슬라이스로부터 이터레이터를 생성한다.
func NewSliceIterator(entries []Entry, labels string) *SliceIterator {
	return &SliceIterator{
		entries: entries,
		labels:  labels,
		pos:     -1,
	}
}

func (s *SliceIterator) Next() bool {
	s.pos++
	if s.pos >= len(s.entries) {
		return false
	}
	s.current = s.entries[s.pos]
	return true
}

func (s *SliceIterator) Entry() Entry        { return s.current }
func (s *SliceIterator) StreamLabels() string { return s.labels }
func (s *SliceIterator) Close() error         { return nil }
func (s *SliceIterator) Err() error           { return nil }

// =============================================================================
// 4. HeapIterator - 힙 기반 정렬 병합
// =============================================================================
// Loki의 핵심 이터레이터. 여러 소스의 이터레이터를 힙을 사용해 병합한다.
// 시간 복잡도: O(n * log(k)), n = 전체 엔트리 수, k = 소스 이터레이터 수
//
// Loki 실제 코드: pkg/iter/entry_iterator.go의 heapIterator

// heapEntry는 힙에 들어갈 엔트리를 래핑한다.
type heapEntry struct {
	entry    Entry
	iterator EntryIterator // 원본 이터레이터 (다음 엔트리를 가져오기 위해 필요)
	index    int           // 힙 인덱스 추적용
}

// entryHeap은 엔트리들의 최소/최대 힙이다.
type entryHeap struct {
	entries   []*heapEntry
	direction Direction
}

func (h *entryHeap) Len() int { return len(h.entries) }

// Less는 방향에 따라 비교 기준을 바꾼다.
// Forward: 오래된 것이 우선 (최소 힙)
// Backward: 최신 것이 우선 (최대 힙)
func (h *entryHeap) Less(i, j int) bool {
	ti := h.entries[i].entry.Timestamp
	tj := h.entries[j].entry.Timestamp
	if h.direction == Forward {
		return ti.Before(tj)
	}
	return ti.After(tj)
}

func (h *entryHeap) Swap(i, j int) {
	h.entries[i], h.entries[j] = h.entries[j], h.entries[i]
	h.entries[i].index = i
	h.entries[j].index = j
}

func (h *entryHeap) Push(x interface{}) {
	entry := x.(*heapEntry)
	entry.index = len(h.entries)
	h.entries = append(h.entries, entry)
}

func (h *entryHeap) Pop() interface{} {
	old := h.entries
	n := len(old)
	entry := old[n-1]
	old[n-1] = nil
	h.entries = old[:n-1]
	return entry
}

// HeapIterator는 여러 이터레이터를 힙으로 병합하는 이터레이터이다.
type HeapIterator struct {
	heap      *entryHeap
	current   Entry
	direction Direction
	err       error
}

// NewHeapIterator는 여러 이터레이터를 병합하는 힙 이터레이터를 생성한다.
func NewHeapIterator(iterators []EntryIterator, direction Direction) *HeapIterator {
	h := &HeapIterator{
		heap: &entryHeap{
			direction: direction,
		},
		direction: direction,
	}

	// 각 이터레이터의 첫 번째 엔트리를 힙에 추가
	for _, iter := range iterators {
		if iter.Next() {
			heap.Push(h.heap, &heapEntry{
				entry:    iter.Entry(),
				iterator: iter,
			})
		}
	}

	heap.Init(h.heap)
	return h
}

func (h *HeapIterator) Next() bool {
	if h.heap.Len() == 0 {
		return false
	}

	// 힙에서 최우선 엔트리를 꺼냄
	entry := heap.Pop(h.heap).(*heapEntry)
	h.current = entry.entry

	// 해당 이터레이터에서 다음 엔트리를 가져와 힙에 재삽입
	// 이것이 힙 병합의 핵심: 꺼낸 이터레이터의 다음 값을 즉시 보충
	if entry.iterator.Next() {
		heap.Push(h.heap, &heapEntry{
			entry:    entry.iterator.Entry(),
			iterator: entry.iterator,
		})
	}

	return true
}

func (h *HeapIterator) Entry() Entry        { return h.current }
func (h *HeapIterator) StreamLabels() string { return h.current.Labels }
func (h *HeapIterator) Err() error           { return h.err }
func (h *HeapIterator) Close() error         { return nil }

// =============================================================================
// 5. TimeRangeIterator - 시간 범위 필터링
// =============================================================================
// 지정된 시간 범위 내의 엔트리만 통과시킨다.
// Loki에서는 쿼리의 시간 범위에 해당하는 엔트리만 반환하는 데 사용된다.

// TimeRangeIterator는 시간 범위 내의 엔트리만 통과시키는 이터레이터이다.
type TimeRangeIterator struct {
	inner   EntryIterator
	start   time.Time
	end     time.Time
	current Entry
}

// NewTimeRangeIterator는 시간 범위 필터링 이터레이터를 생성한다.
func NewTimeRangeIterator(inner EntryIterator, start, end time.Time) *TimeRangeIterator {
	return &TimeRangeIterator{
		inner: inner,
		start: start,
		end:   end,
	}
}

func (t *TimeRangeIterator) Next() bool {
	for t.inner.Next() {
		entry := t.inner.Entry()
		ts := entry.Timestamp
		// 시간 범위 내의 엔트리만 통과
		if (ts.Equal(t.start) || ts.After(t.start)) && ts.Before(t.end) {
			t.current = entry
			return true
		}
	}
	return false
}

func (t *TimeRangeIterator) Entry() Entry        { return t.current }
func (t *TimeRangeIterator) StreamLabels() string { return t.current.Labels }
func (t *TimeRangeIterator) Close() error         { return t.inner.Close() }
func (t *TimeRangeIterator) Err() error           { return t.inner.Err() }

// =============================================================================
// 6. LimitIterator - 결과 수 제한
// =============================================================================

// LimitIterator는 최대 N개의 엔트리만 반환하는 이터레이터이다.
type LimitIterator struct {
	inner   EntryIterator
	limit   int
	count   int
	current Entry
}

// NewLimitIterator는 결과 수 제한 이터레이터를 생성한다.
func NewLimitIterator(inner EntryIterator, limit int) *LimitIterator {
	return &LimitIterator{
		inner: inner,
		limit: limit,
	}
}

func (l *LimitIterator) Next() bool {
	if l.count >= l.limit {
		return false
	}
	if l.inner.Next() {
		l.current = l.inner.Entry()
		l.count++
		return true
	}
	return false
}

func (l *LimitIterator) Entry() Entry        { return l.current }
func (l *LimitIterator) StreamLabels() string { return l.current.Labels }
func (l *LimitIterator) Close() error         { return l.inner.Close() }
func (l *LimitIterator) Err() error           { return l.inner.Err() }

// =============================================================================
// 7. 데이터 생성 유틸리티
// =============================================================================

// generateSourceEntries는 하나의 소스에서 생성된 로그 엔트리를 시뮬레이션한다.
func generateSourceEntries(source string, baseTime time.Time, count int, direction Direction) []Entry {
	entries := make([]Entry, count)
	r := rand.New(rand.NewSource(time.Now().UnixNano()))

	for i := 0; i < count; i++ {
		// 각 소스의 타임스탬프에 약간의 랜덤 오프셋 추가
		offset := time.Duration(i*100+r.Intn(50)) * time.Millisecond
		entries[i] = Entry{
			Timestamp: baseTime.Add(offset),
			Line:      fmt.Sprintf("log message #%d from %s", i+1, source),
			Labels:    source,
		}
	}

	// 방향에 따라 정렬 (각 소스 내부는 이미 정렬되어 있어야 함)
	if direction == Backward {
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].Timestamp.After(entries[j].Timestamp)
		})
	} else {
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].Timestamp.Before(entries[j].Timestamp)
		})
	}

	return entries
}

// drainIterator는 이터레이터의 모든 엔트리를 소비하여 슬라이스로 반환한다.
func drainIterator(iter EntryIterator) []Entry {
	var result []Entry
	for iter.Next() {
		result = append(result, iter.Entry())
	}
	return result
}

// =============================================================================
// 8. 메인 함수 - 이터레이터 병합 시연
// =============================================================================

func main() {
	fmt.Println("=== Loki PoC #08: 반복자 병합 - 다중 소스 로그 스트림의 정렬 병합 ===")
	fmt.Println()

	baseTime := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	// =========================================================================
	// 시연 1: Forward 방향 힙 병합
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("[시연 1] Forward 방향 힙 병합 (시간순, 오래된 것 먼저)")
	fmt.Println()

	// 3개의 소스에서 각각 5개의 로그 생성
	sources := []struct {
		name    string
		entries []Entry
	}{
		{"ingester-1", generateSourceEntries("ingester-1", baseTime, 5, Forward)},
		{"ingester-2", generateSourceEntries("ingester-2", baseTime.Add(30*time.Millisecond), 5, Forward)},
		{"store-chunk", generateSourceEntries("store-chunk", baseTime.Add(10*time.Millisecond), 5, Forward)},
	}

	// 각 소스의 엔트리 출력
	for _, src := range sources {
		fmt.Printf("  소스 [%s]:\n", src.name)
		for _, e := range src.entries {
			fmt.Printf("    %s\n", e)
		}
		fmt.Println()
	}

	// 이터레이터 생성 및 힙 병합
	iterators := make([]EntryIterator, len(sources))
	for i, src := range sources {
		iterators[i] = NewSliceIterator(src.entries, src.name)
	}

	merged := NewHeapIterator(iterators, Forward)
	fmt.Println("  병합 결과 (Forward - 시간순):")
	results := drainIterator(merged)
	for i, e := range results {
		fmt.Printf("    [%2d] %s\n", i+1, e)
	}
	fmt.Printf("  총 %d개 엔트리 병합 완료\n", len(results))

	// 정렬 검증
	sorted := true
	for i := 1; i < len(results); i++ {
		if results[i].Timestamp.Before(results[i-1].Timestamp) {
			sorted = false
			break
		}
	}
	fmt.Printf("  정렬 검증: %v\n", sorted)
	fmt.Println()

	// =========================================================================
	// 시연 2: Backward 방향 힙 병합
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("[시연 2] Backward 방향 힙 병합 (역시간순, 최신 것 먼저)")
	fmt.Println()

	// Backward 방향으로 정렬된 소스 생성
	backwardSources := []struct {
		name    string
		entries []Entry
	}{
		{"tail-ingester-1", generateSourceEntries("tail-ingester-1", baseTime, 4, Backward)},
		{"tail-ingester-2", generateSourceEntries("tail-ingester-2", baseTime.Add(20*time.Millisecond), 4, Backward)},
	}

	backwardIters := make([]EntryIterator, len(backwardSources))
	for i, src := range backwardSources {
		backwardIters[i] = NewSliceIterator(src.entries, src.name)
	}

	backwardMerged := NewHeapIterator(backwardIters, Backward)
	fmt.Println("  병합 결과 (Backward - 역시간순):")
	backResults := drainIterator(backwardMerged)
	for i, e := range backResults {
		fmt.Printf("    [%2d] %s\n", i+1, e)
	}

	// 역순 정렬 검증
	reverseSorted := true
	for i := 1; i < len(backResults); i++ {
		if backResults[i].Timestamp.After(backResults[i-1].Timestamp) {
			reverseSorted = false
			break
		}
	}
	fmt.Printf("  역순 정렬 검증: %v\n", reverseSorted)
	fmt.Println()

	// =========================================================================
	// 시연 3: 이터레이터 체이닝 (TimeRange + Limit)
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("[시연 3] 이터레이터 체이닝 (TimeRange + Limit)")
	fmt.Println()

	// 더 많은 엔트리 생성
	chainEntries := generateSourceEntries("app-logs", baseTime, 20, Forward)
	fmt.Println("  원본 엔트리 (20개):")
	for i, e := range chainEntries {
		fmt.Printf("    [%2d] %s\n", i+1, e)
	}
	fmt.Println()

	// 시간 범위: 처음 10개 정도 범위
	rangeStart := baseTime.Add(200 * time.Millisecond)
	rangeEnd := baseTime.Add(800 * time.Millisecond)
	fmt.Printf("  시간 범위 필터: %s ~ %s\n",
		rangeStart.Format("15:04:05.000"),
		rangeEnd.Format("15:04:05.000"))

	// 체이닝: SliceIterator → TimeRangeIterator → LimitIterator
	baseIter := NewSliceIterator(chainEntries, "app-logs")
	rangeIter := NewTimeRangeIterator(baseIter, rangeStart, rangeEnd)
	limitIter := NewLimitIterator(rangeIter, 5)

	fmt.Println("  체이닝 결과 (TimeRange + Limit 5):")
	chainResults := drainIterator(limitIter)
	for i, e := range chainResults {
		fmt.Printf("    [%2d] %s\n", i+1, e)
	}
	fmt.Printf("  필터링 후 %d개 엔트리 반환 (최대 5개 제한)\n", len(chainResults))
	fmt.Println()

	// =========================================================================
	// 시연 4: 다중 레벨 병합 (ingester + store 시뮬레이션)
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("[시연 4] 다중 레벨 병합 (Ingester + Store)")
	fmt.Println()
	fmt.Println("  Loki 쿼리 시 실제 데이터 흐름:")
	fmt.Println("  ┌─────────────────────────────────────────────┐")
	fmt.Println("  │ Querier                                     │")
	fmt.Println("  │  ┌─────────────┐   ┌──────────────────┐    │")
	fmt.Println("  │  │ Ingester    │   │ Store            │    │")
	fmt.Println("  │  │ Iterator    │   │ Iterator         │    │")
	fmt.Println("  │  │ ┌─────────┐ │   │ ┌──────────────┐ │    │")
	fmt.Println("  │  │ │ingstr-1 │ │   │ │chunk-iter-1  │ │    │")
	fmt.Println("  │  │ │ingstr-2 │ │   │ │chunk-iter-2  │ │    │")
	fmt.Println("  │  │ │ingstr-3 │ │   │ │chunk-iter-3  │ │    │")
	fmt.Println("  │  │ └─────────┘ │   │ └──────────────┘ │    │")
	fmt.Println("  │  └──────┬──────┘   └────────┬─────────┘    │")
	fmt.Println("  │         └────────┬──────────┘              │")
	fmt.Println("  │            HeapIterator (최종 병합)          │")
	fmt.Println("  └─────────────────────────────────────────────┘")
	fmt.Println()

	// Ingester 소스들 (최근 데이터, 아직 store에 플러시되지 않은 데이터)
	ingesterTime := baseTime.Add(1 * time.Second) // 최근 데이터
	ingester1 := generateSourceEntries("ingester-1", ingesterTime, 3, Forward)
	ingester2 := generateSourceEntries("ingester-2", ingesterTime.Add(50*time.Millisecond), 3, Forward)
	ingester3 := generateSourceEntries("ingester-3", ingesterTime.Add(100*time.Millisecond), 3, Forward)

	// Ingester 이터레이터를 먼저 병합
	ingesterIters := []EntryIterator{
		NewSliceIterator(ingester1, "ingester-1"),
		NewSliceIterator(ingester2, "ingester-2"),
		NewSliceIterator(ingester3, "ingester-3"),
	}
	ingesterMerged := NewHeapIterator(ingesterIters, Forward)

	// Store 소스들 (과거 데이터, 이미 청크로 저장된 데이터)
	storeTime := baseTime // 과거 데이터
	chunk1 := generateSourceEntries("store-chunk-1", storeTime, 3, Forward)
	chunk2 := generateSourceEntries("store-chunk-2", storeTime.Add(40*time.Millisecond), 3, Forward)

	// Store 이터레이터를 병합
	storeIters := []EntryIterator{
		NewSliceIterator(chunk1, "store-chunk-1"),
		NewSliceIterator(chunk2, "store-chunk-2"),
	}
	storeMerged := NewHeapIterator(storeIters, Forward)

	// 최종 병합: Ingester + Store
	finalIters := []EntryIterator{ingesterMerged, storeMerged}
	finalMerged := NewHeapIterator(finalIters, Forward)

	fmt.Println("  최종 병합 결과 (Ingester + Store, Forward):")
	finalResults := drainIterator(finalMerged)
	for i, e := range finalResults {
		// 소스 타입 표시
		srcType := "STORE"
		if strings.HasPrefix(e.Labels, "ingester") {
			srcType = "INGSTR"
		}
		fmt.Printf("    [%2d] [%s] %s\n", i+1, srcType, e)
	}
	fmt.Printf("  총 %d개 엔트리 (Ingester %d + Store %d)\n",
		len(finalResults), 9, 6)
	fmt.Println()

	// =========================================================================
	// 시연 5: 성능 특성 분석
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("[시연 5] 성능 특성 분석")
	fmt.Println()

	// 다양한 소스 수에 대한 병합 성능 측정
	sourceCounts := []int{2, 5, 10, 50, 100}
	entriesPerSource := 1000

	for _, numSources := range sourceCounts {
		iters := make([]EntryIterator, numSources)
		for i := 0; i < numSources; i++ {
			entries := make([]Entry, entriesPerSource)
			for j := 0; j < entriesPerSource; j++ {
				entries[j] = Entry{
					Timestamp: baseTime.Add(time.Duration(j*100) * time.Millisecond),
					Line:      fmt.Sprintf("msg-%d", j),
					Labels:    fmt.Sprintf("source-%d", i),
				}
			}
			iters[i] = NewSliceIterator(entries, fmt.Sprintf("source-%d", i))
		}

		start := time.Now()
		merger := NewHeapIterator(iters, Forward)
		count := 0
		for merger.Next() {
			count++
		}
		elapsed := time.Since(start)

		fmt.Printf("  소스 %3d개 x %d 엔트리 = %6d 총 엔트리 → 병합 시간: %v\n",
			numSources, entriesPerSource, count, elapsed)
	}

	fmt.Println()
	fmt.Println("  힙 병합 시간 복잡도: O(N * log(K))")
	fmt.Println("    N = 전체 엔트리 수, K = 소스 이터레이터 수")
	fmt.Println("    K가 증가해도 log(K) 비례이므로 효율적")

	// =========================================================================
	// 구조 요약
	// =========================================================================
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("=== 반복자 병합 구조 요약 ===")
	fmt.Println()
	fmt.Println("  이터레이터 계층 구조:")
	fmt.Println()
	fmt.Println("    EntryIterator (인터페이스)")
	fmt.Println("    ├── SliceIterator     : 메모리 내 엔트리 순회")
	fmt.Println("    ├── HeapIterator      : 힙 기반 다중 소스 병합")
	fmt.Println("    ├── TimeRangeIterator : 시간 범위 필터링 (데코레이터)")
	fmt.Println("    └── LimitIterator     : 결과 수 제한 (데코레이터)")
	fmt.Println()
	fmt.Println("  핵심 포인트:")
	fmt.Println("    - Iterator 패턴: 지연 평가(lazy evaluation)로 메모리 효율적")
	fmt.Println("    - 힙 병합: K개 정렬된 소스를 O(N*logK)에 병합")
	fmt.Println("    - 데코레이터 패턴: 이터레이터를 감싸서 필터링/제한 추가")
	fmt.Println("    - 방향 제어: Forward(시간순)/Backward(역시간순) 모두 지원")
	fmt.Println("    - Loki 실제 코드: pkg/iter/entry_iterator.go")
}
