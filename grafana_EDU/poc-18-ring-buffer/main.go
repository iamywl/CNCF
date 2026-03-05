package main

import (
	"fmt"
	"math"
	"math/rand"
	"strings"
	"time"
)

// ============================================================
// 시계열 링 버퍼 시뮬레이션
// 고정 크기 버퍼, 시간 범위 쿼리, 집계, 다운샘플링 구현
// ============================================================

// --- 데이터 구조 ---

// TimeSeriesPoint는 시계열 데이터의 한 점.
type TimeSeriesPoint struct {
	Timestamp time.Time
	Value     float64
}

// RingBuffer는 고정 크기 시계열 링 버퍼.
type RingBuffer struct {
	data     []TimeSeriesPoint
	head     int  // 가장 오래된 데이터의 인덱스
	count    int  // 현재 저장된 데이터 수
	capacity int  // 최대 용량
}

func NewRingBuffer(capacity int) *RingBuffer {
	return &RingBuffer{
		data:     make([]TimeSeriesPoint, capacity),
		capacity: capacity,
	}
}

// Push는 새 데이터 포인트를 추가한다.
// 버퍼가 가득 차면 가장 오래된 데이터를 덮어쓴다.
func (rb *RingBuffer) Push(point TimeSeriesPoint) {
	// 쓰기 위치: (head + count) % capacity
	writeIdx := (rb.head + rb.count) % rb.capacity

	if rb.count == rb.capacity {
		// 가득 참 → head를 앞으로 이동 (가장 오래된 데이터 삭제)
		rb.data[rb.head] = point
		rb.head = (rb.head + 1) % rb.capacity
	} else {
		rb.data[writeIdx] = point
		rb.count++
	}
}

// Get은 논리적 인덱스(0=가장 오래된)로 데이터를 반환한다.
func (rb *RingBuffer) Get(index int) (TimeSeriesPoint, bool) {
	if index < 0 || index >= rb.count {
		return TimeSeriesPoint{}, false
	}
	actualIdx := (rb.head + index) % rb.capacity
	return rb.data[actualIdx], true
}

// Len은 현재 저장된 데이터 수를 반환한다.
func (rb *RingBuffer) Len() int { return rb.count }

// Full은 버퍼가 가득 찼는지 확인한다.
func (rb *RingBuffer) Full() bool { return rb.count == rb.capacity }

// Cap은 버퍼 용량을 반환한다.
func (rb *RingBuffer) Cap() int { return rb.capacity }

// Oldest는 가장 오래된 데이터를 반환한다.
func (rb *RingBuffer) Oldest() (TimeSeriesPoint, bool) { return rb.Get(0) }

// Newest는 가장 최신 데이터를 반환한다.
func (rb *RingBuffer) Newest() (TimeSeriesPoint, bool) { return rb.Get(rb.count - 1) }

// All은 시간순으로 모든 데이터를 반환한다.
func (rb *RingBuffer) All() []TimeSeriesPoint {
	result := make([]TimeSeriesPoint, rb.count)
	for i := 0; i < rb.count; i++ {
		result[i], _ = rb.Get(i)
	}
	return result
}

// Range는 시간 범위 내의 데이터를 반환한다 (start, end 포함).
func (rb *RingBuffer) Range(start, end time.Time) []TimeSeriesPoint {
	var result []TimeSeriesPoint
	for i := 0; i < rb.count; i++ {
		p, _ := rb.Get(i)
		if (p.Timestamp.Equal(start) || p.Timestamp.After(start)) &&
			(p.Timestamp.Equal(end) || p.Timestamp.Before(end)) {
			result = append(result, p)
		}
	}
	return result
}

// --- 집계 함수 ---

// Aggregation은 데이터 집계 결과.
type Aggregation struct {
	Mean  float64
	Max   float64
	Min   float64
	Sum   float64
	Count int
}

// Aggregate는 시간 범위 내의 데이터를 집계한다.
func (rb *RingBuffer) Aggregate(start, end time.Time) Aggregation {
	points := rb.Range(start, end)
	if len(points) == 0 {
		return Aggregation{}
	}

	agg := Aggregation{
		Max:   points[0].Value,
		Min:   points[0].Value,
		Count: len(points),
	}

	for _, p := range points {
		agg.Sum += p.Value
		if p.Value > agg.Max {
			agg.Max = p.Value
		}
		if p.Value < agg.Min {
			agg.Min = p.Value
		}
	}
	agg.Mean = agg.Sum / float64(agg.Count)

	return agg
}

// AggregateAll은 전체 버퍼 데이터를 집계한다.
func (rb *RingBuffer) AggregateAll() Aggregation {
	if rb.count == 0 {
		return Aggregation{}
	}

	first, _ := rb.Get(0)
	agg := Aggregation{
		Max:   first.Value,
		Min:   first.Value,
		Count: rb.count,
	}

	for i := 0; i < rb.count; i++ {
		p, _ := rb.Get(i)
		agg.Sum += p.Value
		if p.Value > agg.Max {
			agg.Max = p.Value
		}
		if p.Value < agg.Min {
			agg.Min = p.Value
		}
	}
	agg.Mean = agg.Sum / float64(agg.Count)

	return agg
}

// --- 다운샘플링 ---

// Downsample은 지정된 간격으로 데이터를 다운샘플링한다.
// 각 간격 내 데이터의 평균값을 사용한다.
func (rb *RingBuffer) Downsample(interval time.Duration) []TimeSeriesPoint {
	if rb.count == 0 {
		return nil
	}

	all := rb.All()
	var result []TimeSeriesPoint

	bucketStart := all[0].Timestamp
	var bucketSum float64
	var bucketCount int

	for _, p := range all {
		if p.Timestamp.Sub(bucketStart) >= interval {
			// 이전 버킷 완료
			if bucketCount > 0 {
				result = append(result, TimeSeriesPoint{
					Timestamp: bucketStart,
					Value:     bucketSum / float64(bucketCount),
				})
			}
			// 새 버킷 시작
			bucketStart = p.Timestamp
			bucketSum = p.Value
			bucketCount = 1
		} else {
			bucketSum += p.Value
			bucketCount++
		}
	}

	// 마지막 버킷
	if bucketCount > 0 {
		result = append(result, TimeSeriesPoint{
			Timestamp: bucketStart,
			Value:     bucketSum / float64(bucketCount),
		})
	}

	return result
}

// --- 다중 시리즈 관리 ---

// SeriesStore는 레이블 기반 다중 시리즈 관리자.
type SeriesStore struct {
	buffers  map[string]*RingBuffer
	capacity int
}

func NewSeriesStore(capacity int) *SeriesStore {
	return &SeriesStore{
		buffers:  make(map[string]*RingBuffer),
		capacity: capacity,
	}
}

// Push는 레이블에 해당하는 시리즈에 데이터를 추가한다.
func (ss *SeriesStore) Push(label string, point TimeSeriesPoint) {
	buf, ok := ss.buffers[label]
	if !ok {
		buf = NewRingBuffer(ss.capacity)
		ss.buffers[label] = buf
	}
	buf.Push(point)
}

// GetSeries는 레이블에 해당하는 버퍼를 반환한다.
func (ss *SeriesStore) GetSeries(label string) *RingBuffer {
	return ss.buffers[label]
}

// Labels는 등록된 모든 레이블을 반환한다.
func (ss *SeriesStore) Labels() []string {
	labels := make([]string, 0, len(ss.buffers))
	for l := range ss.buffers {
		labels = append(labels, l)
	}
	return labels
}

// Stats는 전체 저장소 통계를 반환한다.
func (ss *SeriesStore) Stats() (totalPoints int, totalSeries int) {
	for _, buf := range ss.buffers {
		totalPoints += buf.Len()
		totalSeries++
	}
	return
}

// --- 출력 헬퍼 ---

func formatTime(t time.Time) string {
	return t.Format("15:04:05")
}

func printPoints(points []TimeSeriesPoint, maxShow int) {
	n := maxShow
	if n > len(points) {
		n = len(points)
	}
	for i := 0; i < n; i++ {
		p := points[i]
		fmt.Printf("    [%s] %.2f\n", formatTime(p.Timestamp), p.Value)
	}
	if len(points) > maxShow {
		fmt.Printf("    ... (%d개 더)\n", len(points)-maxShow)
	}
}

// --- 메인: 시뮬레이션 ---

func main() {
	fmt.Println("=== 시계열 링 버퍼 시뮬레이션 ===")

	rng := rand.New(rand.NewSource(42))

	// ------------------------------------------
	// 1. 기본 링 버퍼 동작
	// ------------------------------------------
	fmt.Println("\n--- 1. 기본 링 버퍼 동작 ---")
	fmt.Println()

	buf := NewRingBuffer(100)
	fmt.Printf("버퍼 생성: 용량=%d\n", buf.Cap())

	// 200개 포인트 추가 (100개 덮어쓰기 발생)
	baseTime := time.Now().Truncate(time.Minute)
	for i := 0; i < 200; i++ {
		buf.Push(TimeSeriesPoint{
			Timestamp: baseTime.Add(time.Duration(i) * time.Minute),
			Value:     float64(i) + rng.Float64()*10,
		})
	}

	fmt.Printf("\n200개 포인트 추가 후:\n")
	fmt.Printf("  버퍼 크기: %d/%d (가득 참: %v)\n", buf.Len(), buf.Cap(), buf.Full())

	oldest, _ := buf.Oldest()
	newest, _ := buf.Newest()
	fmt.Printf("  가장 오래된: %s (value=%.2f)\n", formatTime(oldest.Timestamp), oldest.Value)
	fmt.Printf("  가장 최신:   %s (value=%.2f)\n", formatTime(newest.Timestamp), newest.Value)

	// Get으로 특정 인덱스 접근
	fmt.Println("\n인덱스 접근:")
	for _, idx := range []int{0, 49, 99} {
		p, ok := buf.Get(idx)
		if ok {
			fmt.Printf("  [%d] %s = %.2f\n", idx, formatTime(p.Timestamp), p.Value)
		}
	}

	// 범위 밖 접근
	_, ok := buf.Get(100)
	fmt.Printf("  [100] 범위 밖: ok=%v\n", ok)

	// ------------------------------------------
	// 2. 시간 범위 쿼리
	// ------------------------------------------
	fmt.Println("\n--- 2. 시간 범위 쿼리 ---")
	fmt.Println()

	// 인덱스 120~140 구간 조회 (원본 시간으로는 T+120분 ~ T+140분)
	rangeStart := baseTime.Add(120 * time.Minute)
	rangeEnd := baseTime.Add(140 * time.Minute)
	rangeData := buf.Range(rangeStart, rangeEnd)

	fmt.Printf("범위: %s ~ %s\n", formatTime(rangeStart), formatTime(rangeEnd))
	fmt.Printf("결과: %d개 포인트\n\n", len(rangeData))
	printPoints(rangeData, 5)

	// 최근 10분 조회
	last10Start := baseTime.Add(190 * time.Minute)
	last10End := baseTime.Add(199 * time.Minute)
	last10 := buf.Range(last10Start, last10End)
	fmt.Printf("\n최근 10분: %s ~ %s → %d개 포인트\n",
		formatTime(last10Start), formatTime(last10End), len(last10))
	printPoints(last10, 5)

	// ------------------------------------------
	// 3. 집계 함수
	// ------------------------------------------
	fmt.Println("\n--- 3. 집계 함수 ---")
	fmt.Println()

	// 전체 집계
	allAgg := buf.AggregateAll()
	fmt.Println("전체 버퍼 집계:")
	fmt.Printf("  Mean:  %.2f\n", allAgg.Mean)
	fmt.Printf("  Max:   %.2f\n", allAgg.Max)
	fmt.Printf("  Min:   %.2f\n", allAgg.Min)
	fmt.Printf("  Sum:   %.2f\n", allAgg.Sum)
	fmt.Printf("  Count: %d\n", allAgg.Count)

	// 특정 시간 범위 집계
	rangeAgg := buf.Aggregate(rangeStart, rangeEnd)
	fmt.Printf("\n범위 집계 (%s ~ %s):\n", formatTime(rangeStart), formatTime(rangeEnd))
	fmt.Printf("  Mean:  %.2f\n", rangeAgg.Mean)
	fmt.Printf("  Max:   %.2f\n", rangeAgg.Max)
	fmt.Printf("  Min:   %.2f\n", rangeAgg.Min)
	fmt.Printf("  Count: %d\n", rangeAgg.Count)

	// ------------------------------------------
	// 4. 다운샘플링
	// ------------------------------------------
	fmt.Println("\n--- 4. 다운샘플링 ---")
	fmt.Println()

	fmt.Printf("원본: %d개 포인트 (1분 간격)\n\n", buf.Len())

	// 5분 간격으로 다운샘플링
	ds5 := buf.Downsample(5 * time.Minute)
	fmt.Printf("5분 다운샘플링: %d개 포인트\n", len(ds5))
	printPoints(ds5, 5)

	// 10분 간격으로 다운샘플링
	ds10 := buf.Downsample(10 * time.Minute)
	fmt.Printf("\n10분 다운샘플링: %d개 포인트\n", len(ds10))
	printPoints(ds10, 5)

	// 30분 간격으로 다운샘플링
	ds30 := buf.Downsample(30 * time.Minute)
	fmt.Printf("\n30분 다운샘플링: %d개 포인트\n", len(ds30))
	printPoints(ds30, 10)

	// ------------------------------------------
	// 5. 다중 시리즈 관리
	// ------------------------------------------
	fmt.Println("\n--- 5. 다중 시리즈 관리 ---")
	fmt.Println()

	store := NewSeriesStore(50)

	// 여러 메트릭 시뮬레이션
	metrics := map[string]struct{ base, variance float64 }{
		"cpu_usage":    {base: 45, variance: 20},
		"memory_usage": {base: 60, variance: 15},
		"disk_io":      {base: 30, variance: 25},
		"network_rx":   {base: 100, variance: 50},
	}

	now := time.Now().Truncate(time.Second)
	for label, m := range metrics {
		for i := 0; i < 80; i++ {
			store.Push(label, TimeSeriesPoint{
				Timestamp: now.Add(time.Duration(i) * time.Second),
				Value:     math.Max(0, m.base+(rng.Float64()-0.5)*2*m.variance),
			})
		}
	}

	totalPoints, totalSeries := store.Stats()
	fmt.Printf("저장소 통계: %d개 시리즈, %d개 포인트\n\n", totalSeries, totalPoints)

	fmt.Println(strings.Repeat("-", 65))
	fmt.Printf("%-16s %5s %8s %8s %8s %8s\n", "메트릭", "Count", "Mean", "Max", "Min", "Newest")
	fmt.Println(strings.Repeat("-", 65))

	for _, label := range store.Labels() {
		b := store.GetSeries(label)
		agg := b.AggregateAll()
		newest, _ := b.Newest()
		fmt.Printf("%-16s %5d %8.2f %8.2f %8.2f %8.2f\n",
			label, agg.Count, agg.Mean, agg.Max, agg.Min, newest.Value)
	}
	fmt.Println(strings.Repeat("-", 65))

	// ------------------------------------------
	// 6. 덮어쓰기 동작 시각화
	// ------------------------------------------
	fmt.Println("\n--- 6. 덮어쓰기 동작 시각화 ---")
	fmt.Println()

	smallBuf := NewRingBuffer(8)
	fmt.Printf("용량 8 버퍼에 12개 데이터 추가:\n\n")

	for i := 0; i < 12; i++ {
		point := TimeSeriesPoint{
			Timestamp: baseTime.Add(time.Duration(i) * time.Minute),
			Value:     float64((i + 1) * 10),
		}
		smallBuf.Push(point)

		// 버퍼 상태 시각화
		fmt.Printf("  Push D%d(%.0f): [", i, point.Value)
		for j := 0; j < smallBuf.Cap(); j++ {
			if j > 0 {
				fmt.Print(" ")
			}
			// 실제 배열의 내용 표시
			p := smallBuf.data[j]
			if p.Value == 0 && (smallBuf.count < smallBuf.capacity && j >= (smallBuf.head+smallBuf.count)%smallBuf.capacity) {
				fmt.Print("  _ ")
			} else {
				fmt.Printf("%3.0f", p.Value)
			}
		}
		fmt.Printf("]  len=%d, full=%v", smallBuf.Len(), smallBuf.Full())

		if smallBuf.Full() && i >= smallBuf.Cap() {
			fmt.Printf("  (D%d 덮어씀)", i-smallBuf.Cap()+1)
		}
		fmt.Println()
	}

	// 논리적 순서로 출력
	fmt.Printf("\n논리적 순서 (시간순):\n")
	all := smallBuf.All()
	for i, p := range all {
		fmt.Printf("  [%d] %.0f\n", i, p.Value)
	}

	// ------------------------------------------
	// 요약
	// ------------------------------------------
	fmt.Println("\n--- 시뮬레이션 요약 ---")
	fmt.Println()
	fmt.Println("링 버퍼 구성요소:")
	fmt.Println("  1. RingBuffer: 고정 크기 순환 배열 (head, count, capacity)")
	fmt.Println("  2. Push: O(1) 삽입, 가득 차면 가장 오래된 데이터 덮어쓰기")
	fmt.Println("  3. Get: O(1) 인덱스 접근 (논리적 인덱스 → 물리적 인덱스 변환)")
	fmt.Println("  4. Range: 시간 범위 쿼리 (O(n) 스캔)")
	fmt.Println("  5. Aggregate: 시간 범위 내 집계 (Mean, Max, Min, Sum, Count)")
	fmt.Println("  6. Downsample: 시간 간격별 평균으로 해상도 축소")
	fmt.Println("  7. SeriesStore: 레이블 기반 다중 시리즈 관리")
	fmt.Println()
	fmt.Println("시간 복잡도:")
	fmt.Printf("  %-16s %s\n", "Push:", "O(1)")
	fmt.Printf("  %-16s %s\n", "Get:", "O(1)")
	fmt.Printf("  %-16s %s\n", "Range:", "O(n)")
	fmt.Printf("  %-16s %s\n", "Aggregate:", "O(n)")
	fmt.Printf("  %-16s %s\n", "Downsample:", "O(n)")
	fmt.Println()
	fmt.Println("메모리 사용: capacity * sizeof(TimeSeriesPoint) — 고정, 예측 가능")
}
