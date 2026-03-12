package main

import (
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

// =============================================================================
// Prometheus stripeSeries PoC
// =============================================================================
// Prometheus TSDB의 Head 블록은 수십만~수백만 개의 활성 시리즈를 메모리에 보관한다.
// 단일 RWMutex로 보호하면 동시 scrape 시 심각한 lock contention이 발생하므로,
// Prometheus는 stripeSeries 패턴을 사용하여 lock을 분산시킨다.
//
// 실제 구현: tsdb/head.go — type stripeSeries struct
//   - DefaultStripeSize = 1 << 14 (16384)
//   - series  []map[HeadSeriesRef]*memSeries  (ref 기준 sharding)
//   - hashes  []seriesHashmap                 (label hash 기준 sharding)
//   - locks   []stripeLock                    (cache line padding 포함)
//   - stripe index = value & (size - 1)       (비트 AND로 O(1) 계산)
// =============================================================================

// MemSeries는 Prometheus memSeries의 핵심 필드만 추출한 구조체이다.
// 실제: tsdb/head.go — type memSeries struct
type MemSeries struct {
	ref            uint64 // 시리즈 고유 ID (HeadSeriesRef)
	labels         string // 레이블 문자열 (실제는 labels.Labels)
	lastAppendTime int64  // 마지막 샘플 추가 시각 (Unix nanoseconds)
}

// =============================================================================
// 1. SingleLockMap — 단일 RWMutex 기반 (비교 기준선)
// =============================================================================

type SingleLockMap struct {
	mu     sync.RWMutex
	series map[uint64]*MemSeries
}

func NewSingleLockMap() *SingleLockMap {
	return &SingleLockMap{
		series: make(map[uint64]*MemSeries),
	}
}

func (m *SingleLockMap) Set(s *MemSeries) {
	m.mu.Lock()
	m.series[s.ref] = s
	m.mu.Unlock()
}

func (m *SingleLockMap) Get(ref uint64) *MemSeries {
	m.mu.RLock()
	s := m.series[ref]
	m.mu.RUnlock()
	return s
}

func (m *SingleLockMap) Delete(ref uint64) {
	m.mu.Lock()
	delete(m.series, ref)
	m.mu.Unlock()
}

func (m *SingleLockMap) Len() int {
	m.mu.RLock()
	n := len(m.series)
	m.mu.RUnlock()
	return n
}

// =============================================================================
// 2. StripeSeries — Prometheus의 stripe 패턴 구현
// =============================================================================

const stripeSize = 512 // 2^9, PoC에서는 작은 값 사용. 실제 Prometheus는 1<<14=16384

// stripeLock은 cache line padding을 포함한 lock이다.
// 실제: tsdb/head.go — type stripeLock struct { sync.RWMutex; _ [40]byte }
// 64바이트 cache line에서 sync.RWMutex(~24바이트) + padding(40바이트)로 정렬하여
// false sharing을 방지한다.
type stripeLock struct {
	sync.RWMutex
	_ [40]byte // cache line padding
}

type StripeSeries struct {
	size   int
	series []map[uint64]*MemSeries // ref 기준 sharding
	hashes []map[uint64]*MemSeries // label hash 기준 sharding
	locks  []stripeLock
}

func NewStripeSeries(size int) *StripeSeries {
	s := &StripeSeries{
		size:   size,
		series: make([]map[uint64]*MemSeries, size),
		hashes: make([]map[uint64]*MemSeries, size),
		locks:  make([]stripeLock, size),
	}
	for i := range s.series {
		s.series[i] = make(map[uint64]*MemSeries)
	}
	for i := range s.hashes {
		s.hashes[i] = make(map[uint64]*MemSeries)
	}
	return s
}

// stripeIndex는 비트 AND 연산으로 stripe 인덱스를 계산한다.
// 실제: uint64(id) & uint64(s.size-1)
// size가 2의 거듭제곱이므로 size-1은 모든 하위 비트가 1인 마스크가 된다.
// 예: size=512 → size-1=0x1FF → 하위 9비트만 추출
func (s *StripeSeries) stripeIndex(value uint64) int {
	return int(value & uint64(s.size-1))
}

// getByID는 ref로 시리즈를 조회한다.
// 실제: tsdb/head.go — func (s *stripeSeries) getByID(id HeadSeriesRef) *memSeries
func (s *StripeSeries) getByID(ref uint64) *MemSeries {
	i := s.stripeIndex(ref)
	s.locks[i].RLock()
	series := s.series[i][ref]
	s.locks[i].RUnlock()
	return series
}

// getByHash는 label hash로 시리즈를 조회한다.
// 실제: tsdb/head.go — func (s *stripeSeries) getByHash(hash uint64, lset labels.Labels)
func (s *StripeSeries) getByHash(hash uint64) *MemSeries {
	i := s.stripeIndex(hash)
	s.locks[i].RLock()
	series := s.hashes[i][hash]
	s.locks[i].RUnlock()
	return series
}

// getOrCreateByHash는 hash 기준으로 시리즈를 조회하고, 없으면 생성한다.
// 실제: tsdb/head.go — func (s *stripeSeries) setUnlessAlreadySet(...)
// 두 개의 stripe에 lock을 잡는다: hash stripe (hashes 맵) + ref stripe (series 맵)
func (s *StripeSeries) getOrCreateByHash(hash uint64, labels string, ref uint64) (*MemSeries, bool) {
	// 1) hash stripe에서 조회/생성
	hashIdx := s.stripeIndex(hash)
	s.locks[hashIdx].Lock()
	if existing := s.hashes[hashIdx][hash]; existing != nil {
		s.locks[hashIdx].Unlock()
		return existing, false // 이미 존재
	}
	newSeries := &MemSeries{
		ref:            ref,
		labels:         labels,
		lastAppendTime: time.Now().UnixNano(),
	}
	s.hashes[hashIdx][hash] = newSeries
	s.locks[hashIdx].Unlock()

	// 2) ref stripe에 등록
	refIdx := s.stripeIndex(ref)
	s.locks[refIdx].Lock()
	s.series[refIdx][ref] = newSeries
	s.locks[refIdx].Unlock()

	return newSeries, true // 새로 생성됨
}

// gc는 모든 stripe를 순회하며 stale 시리즈를 제거한다.
// 실제: tsdb/head.go — func (s *stripeSeries) gc(mint int64, ...)
// 각 stripe의 lock을 개별적으로 잡으므로 전체 맵을 잠그지 않는다.
func (s *StripeSeries) gc(minTime int64) (deleted int) {
	for i := 0; i < s.size; i++ {
		s.locks[i].Lock()
		for ref, series := range s.series[i] {
			if series.lastAppendTime < minTime {
				delete(s.series[i], ref)
				deleted++
			}
		}
		s.locks[i].Unlock()
	}
	// hashes 맵도 정리
	for i := 0; i < s.size; i++ {
		s.locks[i].Lock()
		for hash, series := range s.hashes[i] {
			if series.lastAppendTime < minTime {
				delete(s.hashes[i], hash)
			}
		}
		s.locks[i].Unlock()
	}
	return deleted
}

// Len은 전체 시리즈 수를 반환한다.
func (s *StripeSeries) Len() int {
	total := 0
	for i := 0; i < s.size; i++ {
		s.locks[i].RLock()
		total += len(s.series[i])
		s.locks[i].RUnlock()
	}
	return total
}

// StripeDistribution은 각 stripe별 시리즈 수를 반환한다.
func (s *StripeSeries) StripeDistribution() []int {
	dist := make([]int, s.size)
	for i := 0; i < s.size; i++ {
		s.locks[i].RLock()
		dist[i] = len(s.series[i])
		s.locks[i].RUnlock()
	}
	return dist
}

// =============================================================================
// 벤치마크 함수들
// =============================================================================

// benchmarkSingleLock은 SingleLockMap의 동시 읽기/쓰기 처리량을 측정한다.
func benchmarkSingleLock(numWriters, numReaders int, duration time.Duration, totalSeries uint64) (writeOps, readOps int64) {
	m := NewSingleLockMap()
	// 초기 데이터 삽입
	for i := uint64(0); i < totalSeries; i++ {
		m.Set(&MemSeries{
			ref:            i,
			labels:         fmt.Sprintf("series_%d", i),
			lastAppendTime: time.Now().UnixNano(),
		})
	}

	var wOps, rOps atomic.Int64
	done := make(chan struct{})

	// Writers
	for w := 0; w < numWriters; w++ {
		go func(seed int) {
			r := rand.New(rand.NewSource(int64(seed)))
			for {
				select {
				case <-done:
					return
				default:
					ref := r.Uint64() % totalSeries
					m.Set(&MemSeries{
						ref:            ref,
						labels:         fmt.Sprintf("series_%d", ref),
						lastAppendTime: time.Now().UnixNano(),
					})
					wOps.Add(1)
				}
			}
		}(w)
	}

	// Readers
	for rd := 0; rd < numReaders; rd++ {
		go func(seed int) {
			r := rand.New(rand.NewSource(int64(seed + 1000)))
			for {
				select {
				case <-done:
					return
				default:
					ref := r.Uint64() % totalSeries
					_ = m.Get(ref)
					rOps.Add(1)
				}
			}
		}(rd)
	}

	time.Sleep(duration)
	close(done)
	time.Sleep(10 * time.Millisecond) // goroutine 정리 대기

	return wOps.Load(), rOps.Load()
}

// benchmarkStripeSeries은 StripeSeries의 동시 읽기/쓰기 처리량을 측정한다.
func benchmarkStripeSeries(numWriters, numReaders int, duration time.Duration, totalSeries uint64) (writeOps, readOps int64) {
	s := NewStripeSeries(stripeSize)
	// 초기 데이터 삽입
	for i := uint64(0); i < totalSeries; i++ {
		refIdx := s.stripeIndex(i)
		s.locks[refIdx].Lock()
		s.series[refIdx][i] = &MemSeries{
			ref:            i,
			labels:         fmt.Sprintf("series_%d", i),
			lastAppendTime: time.Now().UnixNano(),
		}
		s.locks[refIdx].Unlock()
	}

	var wOps, rOps atomic.Int64
	done := make(chan struct{})

	// Writers
	for w := 0; w < numWriters; w++ {
		go func(seed int) {
			r := rand.New(rand.NewSource(int64(seed)))
			for {
				select {
				case <-done:
					return
				default:
					ref := r.Uint64() % totalSeries
					idx := s.stripeIndex(ref)
					s.locks[idx].Lock()
					s.series[idx][ref] = &MemSeries{
						ref:            ref,
						labels:         fmt.Sprintf("series_%d", ref),
						lastAppendTime: time.Now().UnixNano(),
					}
					s.locks[idx].Unlock()
					wOps.Add(1)
				}
			}
		}(w)
	}

	// Readers
	for rd := 0; rd < numReaders; rd++ {
		go func(seed int) {
			r := rand.New(rand.NewSource(int64(seed + 1000)))
			for {
				select {
				case <-done:
					return
				default:
					ref := r.Uint64() % totalSeries
					_ = s.getByID(ref)
					rOps.Add(1)
				}
			}
		}(rd)
	}

	time.Sleep(duration)
	close(done)
	time.Sleep(10 * time.Millisecond)

	return wOps.Load(), rOps.Load()
}

// =============================================================================
// 메인
// =============================================================================

func main() {
	fmt.Println("=================================================================")
	fmt.Println(" Prometheus stripeSeries PoC — Lock Striping 패턴")
	fmt.Println("=================================================================")
	fmt.Println()
	fmt.Printf("Stripe size: %d (2^%d)\n", stripeSize, countBits(stripeSize))
	fmt.Printf("Bitmask: 0x%X (size-1 = %d)\n", stripeSize-1, stripeSize-1)
	fmt.Println()

	// -----------------------------------------------------------------
	// Demo 1: 100K 시리즈 삽입
	// -----------------------------------------------------------------
	fmt.Println("-----------------------------------------------------------------")
	fmt.Println(" [1] 100K 시리즈 삽입 비교")
	fmt.Println("-----------------------------------------------------------------")

	const numSeries = 100_000

	// SingleLockMap
	slm := NewSingleLockMap()
	start := time.Now()
	for i := uint64(0); i < numSeries; i++ {
		slm.Set(&MemSeries{
			ref:            i,
			labels:         fmt.Sprintf("__name__=cpu_usage,host=server_%d", i%1000),
			lastAppendTime: time.Now().UnixNano(),
		})
	}
	singleDur := time.Since(start)
	fmt.Printf("  SingleLockMap: %d series in %v\n", slm.Len(), singleDur)

	// StripeSeries
	ss := NewStripeSeries(stripeSize)
	start = time.Now()
	for i := uint64(0); i < numSeries; i++ {
		hash := i * 2654435761 // Knuth multiplicative hash (시뮬레이션)
		ss.getOrCreateByHash(hash, fmt.Sprintf("__name__=cpu_usage,host=server_%d", i%1000), i)
	}
	stripeDur := time.Since(start)
	fmt.Printf("  StripeSeries:  %d series in %v\n", ss.Len(), stripeDur)
	fmt.Println()

	// -----------------------------------------------------------------
	// Demo 2: 동시 벤치마크 — 8 writers + 8 readers
	// -----------------------------------------------------------------
	fmt.Println("-----------------------------------------------------------------")
	fmt.Println(" [2] 동시 벤치마크: 8 writers + 8 readers (2초)")
	fmt.Println("-----------------------------------------------------------------")

	const (
		numWriters  = 8
		numReaders  = 8
		benchDur    = 2 * time.Second
		benchSeries = 100_000
	)

	fmt.Println("  SingleLockMap 측정 중...")
	slmWrites, slmReads := benchmarkSingleLock(numWriters, numReaders, benchDur, benchSeries)
	slmTotal := slmWrites + slmReads
	fmt.Printf("    Writes: %s ops/sec\n", formatNumber(slmWrites/2))
	fmt.Printf("    Reads:  %s ops/sec\n", formatNumber(slmReads/2))
	fmt.Printf("    Total:  %s ops/sec\n", formatNumber(slmTotal/2))
	fmt.Println()

	fmt.Println("  StripeSeries 측정 중...")
	ssWrites, ssReads := benchmarkStripeSeries(numWriters, numReaders, benchDur, benchSeries)
	ssTotal := ssWrites + ssReads
	fmt.Printf("    Writes: %s ops/sec\n", formatNumber(ssWrites/2))
	fmt.Printf("    Reads:  %s ops/sec\n", formatNumber(ssReads/2))
	fmt.Printf("    Total:  %s ops/sec\n", formatNumber(ssTotal/2))
	fmt.Println()

	if ssTotal > slmTotal {
		speedup := float64(ssTotal) / float64(slmTotal)
		fmt.Printf("  StripeSeries가 %.1fx 더 빠름\n", speedup)
	} else {
		fmt.Println("  (참고: 시리즈 수가 적거나 CPU 코어가 적으면 차이가 작을 수 있음)")
	}
	fmt.Println()

	// -----------------------------------------------------------------
	// Demo 3: Stripe 분포 확인
	// -----------------------------------------------------------------
	fmt.Println("-----------------------------------------------------------------")
	fmt.Println(" [3] Stripe 분포 (100K 시리즈가 512개 stripe에 어떻게 분산되는가)")
	fmt.Println("-----------------------------------------------------------------")

	dist := ss.StripeDistribution()
	expected := float64(numSeries) / float64(stripeSize)

	min, max, nonEmpty := dist[0], dist[0], 0
	for _, d := range dist {
		if d < min {
			min = d
		}
		if d > max {
			max = d
		}
		if d > 0 {
			nonEmpty++
		}
	}

	fmt.Printf("  기대값 (균등 분포): %.1f series/stripe\n", expected)
	fmt.Printf("  실제 최솟값: %d\n", min)
	fmt.Printf("  실제 최댓값: %d\n", max)
	fmt.Printf("  비어있지 않은 stripe 수: %d / %d\n", nonEmpty, stripeSize)
	fmt.Println()

	// 히스토그램 출력 (버킷 10개)
	fmt.Println("  분포 히스토그램:")
	bucketSize := (max - min + 10) / 10
	if bucketSize == 0 {
		bucketSize = 1
	}
	buckets := make([]int, 10)
	for _, d := range dist {
		idx := (d - min) / bucketSize
		if idx >= 10 {
			idx = 9
		}
		buckets[idx]++
	}
	for i, count := range buckets {
		lo := min + i*bucketSize
		hi := lo + bucketSize - 1
		bar := ""
		for j := 0; j < count/2; j++ {
			bar += "#"
		}
		if count > 0 && len(bar) == 0 {
			bar = "#"
		}
		fmt.Printf("    [%3d-%3d]: %3d stripes  %s\n", lo, hi, count, bar)
	}
	fmt.Println()

	// -----------------------------------------------------------------
	// Demo 4: GC — stale 시리즈 제거
	// -----------------------------------------------------------------
	fmt.Println("-----------------------------------------------------------------")
	fmt.Println(" [4] GC: stale 시리즈 제거")
	fmt.Println("-----------------------------------------------------------------")

	// 새로운 StripeSeries 생성하여 GC 테스트
	gcMap := NewStripeSeries(stripeSize)
	now := time.Now().UnixNano()
	staleCount := 0

	for i := uint64(0); i < 10000; i++ {
		ts := now
		if i%3 == 0 {
			// 33%를 stale로 마킹 (5초 전 타임스탬프)
			ts = now - int64(5*time.Second)
			staleCount++
		}
		refIdx := gcMap.stripeIndex(i)
		gcMap.locks[refIdx].Lock()
		gcMap.series[refIdx][i] = &MemSeries{
			ref:            i,
			labels:         fmt.Sprintf("series_%d", i),
			lastAppendTime: ts,
		}
		gcMap.locks[refIdx].Unlock()
	}

	fmt.Printf("  삽입: 10,000 시리즈 (stale: %d, active: %d)\n", staleCount, 10000-staleCount)
	fmt.Printf("  GC 전 시리즈 수: %d\n", gcMap.Len())

	// GC 실행: 3초 전보다 오래된 것 제거
	cutoff := now - int64(3*time.Second)
	deleted := gcMap.gc(cutoff)

	fmt.Printf("  GC 실행 (cutoff: 3초 전)\n")
	fmt.Printf("  제거된 시리즈: %d\n", deleted)
	fmt.Printf("  GC 후 시리즈 수: %d\n", gcMap.Len())
	fmt.Println()

	if deleted == staleCount {
		fmt.Println("  [OK] stale 시리즈가 모두 정확히 제거됨")
	} else {
		fmt.Printf("  [WARN] 예상 %d개 제거, 실제 %d개 제거\n", staleCount, deleted)
	}
	fmt.Println()

	// -----------------------------------------------------------------
	// Demo 5: Stripe 인덱스 계산 원리
	// -----------------------------------------------------------------
	fmt.Println("-----------------------------------------------------------------")
	fmt.Println(" [5] Stripe 인덱스 계산 원리: ref & (size-1)")
	fmt.Println("-----------------------------------------------------------------")
	fmt.Println()
	fmt.Printf("  size = %d = 0b%b\n", stripeSize, stripeSize)
	fmt.Printf("  mask = size-1 = %d = 0b%b\n", stripeSize-1, stripeSize-1)
	fmt.Println()
	fmt.Println("  예시:")
	examples := []uint64{0, 1, 511, 512, 1023, 1024, 99999, 123456789}
	for _, v := range examples {
		idx := v & uint64(stripeSize-1)
		fmt.Printf("    ref=%-12d → %d & %d = stripe[%d]\n", v, v, stripeSize-1, idx)
	}
	fmt.Println()
	fmt.Println("  비트 AND가 modulo(%)보다 빠른 이유:")
	fmt.Println("    - modulo: CPU DIV 명령어 사용 (수십 사이클)")
	fmt.Println("    - AND:    CPU AND 명령어 사용 (1 사이클)")
	fmt.Println("    - 조건: size가 2의 거듭제곱일 때만 동일 결과")
	fmt.Println()

	fmt.Println("=================================================================")
	fmt.Println(" 완료")
	fmt.Println("=================================================================")
}

// countBits는 2의 거듭제곱에서 지수를 반환한다.
func countBits(n int) int {
	count := 0
	for n > 1 {
		n >>= 1
		count++
	}
	return count
}

// formatNumber는 숫자를 천 단위로 콤마를 찍어 반환한다.
func formatNumber(n int64) string {
	if n < 0 {
		return "-" + formatNumber(-n)
	}
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	result := ""
	for i, ch := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result += ","
		}
		result += string(ch)
	}
	return result
}
