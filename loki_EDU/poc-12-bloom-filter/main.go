package main

import (
	"fmt"
	"hash/fnv"
	"math"
	"math/rand"
	"strings"
	"time"
)

// =============================================================================
// Loki PoC #12: 블룸 필터 - 블룸 필터 기반 로그 검색 가속
// =============================================================================
//
// 블룸 필터는 확률적 데이터 구조로, "이 원소가 집합에 존재하는가?"를
// 매우 효율적으로 (O(k), k=해시 함수 수) 답할 수 있다.
//
// 특성:
//   - False Positive 가능: "있다"고 했지만 실제로 없을 수 있음
//   - False Negative 불가: "없다"고 하면 반드시 없음
//   - 메모리 효율적: 원소 자체를 저장하지 않고 비트만 사용
//
// Loki에서의 활용:
//   청크에 특정 검색어가 포함되어 있는지를 블룸 필터로 빠르게 판별하여,
//   불필요한 청크 읽기를 건너뛴다 (I/O 절약).
//
// 실제 Loki 코드: pkg/storage/bloom/v1/
//
// 실행: go run main.go

// =============================================================================
// 1. 기본 블룸 필터
// =============================================================================
// 블룸 필터의 핵심 구조:
//   - 비트 배열 (size m bits)
//   - k개의 해시 함수
//   - 삽입: 원소를 k개 해시 → 해당 비트 위치를 1로 설정
//   - 조회: 원소를 k개 해시 → 모든 비트가 1이면 "아마도 있음"
//
// 최적 파라미터:
//   n = 예상 원소 수
//   p = 목표 false positive 확률
//   m = -(n * ln(p)) / (ln(2))^2      (최적 비트 수)
//   k = (m/n) * ln(2)                   (최적 해시 함수 수)

// BloomFilter는 기본 블룸 필터이다.
type BloomFilter struct {
	bits       []bool  // 비트 배열
	size       uint    // 비트 배열 크기 (m)
	hashCount  uint    // 해시 함수 수 (k)
	count      uint    // 삽입된 원소 수
}

// NewBloomFilter는 예상 원소 수와 목표 false positive 확률로 블룸 필터를 생성한다.
func NewBloomFilter(expectedItems uint, fpRate float64) *BloomFilter {
	// 최적 비트 수 계산: m = -(n * ln(p)) / (ln(2))^2
	m := uint(math.Ceil(-float64(expectedItems) * math.Log(fpRate) / (math.Ln2 * math.Ln2)))
	if m == 0 {
		m = 1
	}

	// 최적 해시 함수 수 계산: k = (m/n) * ln(2)
	k := uint(math.Round(float64(m) / float64(expectedItems) * math.Ln2))
	if k == 0 {
		k = 1
	}

	return &BloomFilter{
		bits:      make([]bool, m),
		size:      m,
		hashCount: k,
	}
}

// NewBloomFilterWithParams는 직접 파라미터를 지정하여 블룸 필터를 생성한다.
func NewBloomFilterWithParams(size, hashCount uint) *BloomFilter {
	return &BloomFilter{
		bits:      make([]bool, size),
		size:      size,
		hashCount: hashCount,
	}
}

// hashValues는 원소에 대해 k개의 해시 값을 생성한다.
// Double Hashing 기법: h(i) = h1 + i*h2 (두 개의 해시로 k개 생성)
func (bf *BloomFilter) hashValues(data []byte) []uint {
	// FNV-1a 해시 두 개로 double hashing
	h1 := fnv.New64a()
	h1.Write(data)
	hash1 := h1.Sum64()

	h2 := fnv.New64()
	h2.Write(data)
	hash2 := h2.Sum64()

	positions := make([]uint, bf.hashCount)
	for i := uint(0); i < bf.hashCount; i++ {
		// h(i) = (h1 + i * h2) mod m
		combined := hash1 + uint64(i)*hash2
		positions[i] = uint(combined % uint64(bf.size))
	}

	return positions
}

// Add는 원소를 블룸 필터에 추가한다.
func (bf *BloomFilter) Add(data []byte) {
	positions := bf.hashValues(data)
	for _, pos := range positions {
		bf.bits[pos] = true
	}
	bf.count++
}

// AddString은 문자열을 블룸 필터에 추가한다.
func (bf *BloomFilter) AddString(s string) {
	bf.Add([]byte(s))
}

// Test는 원소가 블룸 필터에 존재하는지 확인한다.
// true: 아마도 존재 (false positive 가능)
// false: 확실히 미존재 (false negative 불가)
func (bf *BloomFilter) Test(data []byte) bool {
	positions := bf.hashValues(data)
	for _, pos := range positions {
		if !bf.bits[pos] {
			return false // 하나라도 0이면 확실히 없음
		}
	}
	return true // 모두 1이면 "아마도 있음"
}

// TestString은 문자열이 블룸 필터에 존재하는지 확인한다.
func (bf *BloomFilter) TestString(s string) bool {
	return bf.Test([]byte(s))
}

// FillRatio는 현재 비트 채움 비율을 반환한다.
func (bf *BloomFilter) FillRatio() float64 {
	set := 0
	for _, b := range bf.bits {
		if b {
			set++
		}
	}
	return float64(set) / float64(bf.size)
}

// EstimatedFPRate는 현재 상태에서의 예상 false positive 확률을 계산한다.
// p = (1 - e^(-kn/m))^k
func (bf *BloomFilter) EstimatedFPRate() float64 {
	exp := math.Exp(-float64(bf.hashCount) * float64(bf.count) / float64(bf.size))
	return math.Pow(1-exp, float64(bf.hashCount))
}

// Stats는 블룸 필터의 통계를 출력한다.
func (bf *BloomFilter) Stats() {
	fmt.Printf("    비트 수(m): %d, 해시 함수 수(k): %d\n", bf.size, bf.hashCount)
	fmt.Printf("    삽입 원소 수: %d, 채움 비율: %.2f%%\n", bf.count, bf.FillRatio()*100)
	fmt.Printf("    예상 FP 확률: %.6f (%.4f%%)\n", bf.EstimatedFPRate(), bf.EstimatedFPRate()*100)
	fmt.Printf("    메모리 사용: %d bytes (%.1f KB)\n", bf.size/8, float64(bf.size)/8/1024)
}

// Visualize는 블룸 필터의 비트 배열을 시각적으로 표시한다.
func (bf *BloomFilter) Visualize(maxWidth int) string {
	if int(bf.size) <= maxWidth {
		var sb strings.Builder
		for _, b := range bf.bits {
			if b {
				sb.WriteByte('#')
			} else {
				sb.WriteByte('.')
			}
		}
		return sb.String()
	}

	// 축소 표시
	ratio := float64(bf.size) / float64(maxWidth)
	var sb strings.Builder
	for i := 0; i < maxWidth; i++ {
		start := int(float64(i) * ratio)
		end := int(float64(i+1) * ratio)
		if end > int(bf.size) {
			end = int(bf.size)
		}
		set := 0
		total := 0
		for j := start; j < end; j++ {
			total++
			if bf.bits[j] {
				set++
			}
		}
		if total > 0 && float64(set)/float64(total) > 0.5 {
			sb.WriteByte('#')
		} else {
			sb.WriteByte('.')
		}
	}
	return sb.String()
}

// =============================================================================
// 2. Scalable Bloom Filter (자동 확장)
// =============================================================================
// 원소 수를 미리 알 수 없을 때, 블룸 필터를 자동으로 확장한다.
// 내부적으로 여러 블룸 필터를 연결하고, 현재 필터가 가득 차면 새 필터를 추가한다.
//
// 핵심 아이디어:
//   - 각 레벨의 FP 확률을 기하급수적으로 줄임 (r^i, r=0.5)
//   - 전체 FP 확률 = 1 - (1-p0)(1-p0*r)(1-p0*r^2)... ≈ p0 / (1-r)

// ScalableBloomFilter는 자동 확장 블룸 필터이다.
type ScalableBloomFilter struct {
	filters       []*BloomFilter
	fpRate        float64 // 초기 FP 확률
	growthRatio   float64 // FP 확률 감소 비율 (0.5)
	initialSize   uint    // 초기 예상 원소 수
	fillThreshold float64 // 새 필터 추가 임계치 (채움 비율)
	totalCount    uint    // 전체 삽입 원소 수
}

// NewScalableBloomFilter는 자동 확장 블룸 필터를 생성한다.
func NewScalableBloomFilter(initialSize uint, fpRate float64) *ScalableBloomFilter {
	sbf := &ScalableBloomFilter{
		fpRate:        fpRate,
		growthRatio:   0.5, // 각 레벨마다 FP 확률을 절반으로
		initialSize:   initialSize,
		fillThreshold: 0.5, // 50% 채워지면 새 필터 추가
	}

	// 첫 번째 필터 생성
	sbf.addFilter()
	return sbf
}

// addFilter는 새로운 블룸 필터를 추가한다.
func (sbf *ScalableBloomFilter) addFilter() {
	level := len(sbf.filters)
	// 각 레벨의 FP 확률: fpRate * growthRatio^level
	levelFP := sbf.fpRate * math.Pow(sbf.growthRatio, float64(level))
	// 각 레벨의 예상 원소 수: 이전 레벨과 동일 (또는 증가)
	expectedItems := sbf.initialSize * uint(math.Pow(2, float64(level)))

	filter := NewBloomFilter(expectedItems, levelFP)
	sbf.filters = append(sbf.filters, filter)
}

// Add는 원소를 추가한다. 현재 필터가 임계치를 초과하면 새 필터를 추가한다.
func (sbf *ScalableBloomFilter) Add(data []byte) {
	current := sbf.filters[len(sbf.filters)-1]

	// 채움 비율이 임계치를 초과하면 새 필터 추가
	if current.FillRatio() >= sbf.fillThreshold {
		sbf.addFilter()
		current = sbf.filters[len(sbf.filters)-1]
	}

	current.Add(data)
	sbf.totalCount++
}

// AddString은 문자열을 추가한다.
func (sbf *ScalableBloomFilter) AddString(s string) {
	sbf.Add([]byte(s))
}

// Test는 원소 존재 여부를 확인한다.
// 모든 내부 필터를 검사한다.
func (sbf *ScalableBloomFilter) Test(data []byte) bool {
	for _, filter := range sbf.filters {
		if filter.Test(data) {
			return true
		}
	}
	return false
}

// TestString은 문자열 존재 여부를 확인한다.
func (sbf *ScalableBloomFilter) TestString(s string) bool {
	return sbf.Test([]byte(s))
}

// Stats는 통계를 출력한다.
func (sbf *ScalableBloomFilter) Stats() {
	fmt.Printf("    필터 레벨 수: %d, 총 원소 수: %d\n", len(sbf.filters), sbf.totalCount)
	totalBits := uint(0)
	for i, f := range sbf.filters {
		totalBits += f.size
		fmt.Printf("    레벨 %d: bits=%d, items=%d, fill=%.1f%%, FP=%.6f\n",
			i, f.size, f.count, f.FillRatio()*100, f.EstimatedFPRate())
	}
	fmt.Printf("    총 메모리: %d bits (%.1f KB)\n", totalBits, float64(totalBits)/8/1024)
}

// =============================================================================
// 3. Chunk Filtering 시뮬레이션 (Loki 활용 사례)
// =============================================================================
// Loki는 블룸 필터를 사용하여 청크에 특정 검색어가 포함되어 있는지
// 빠르게 판별한다. 이를 통해 불필요한 청크 읽기를 건너뛸 수 있다.

// Chunk는 로그 청크를 시뮬레이션한다.
type Chunk struct {
	ID     string
	Labels string
	Lines  []string       // 실제 로그 라인들
	Bloom  *BloomFilter   // 이 청크의 블룸 필터
}

// NewChunk는 로그 라인들로 청크를 생성하고 블룸 필터를 구축한다.
func NewChunk(id, labels string, lines []string) *Chunk {
	// 블룸 필터 생성 (각 라인의 단어를 등록)
	// 예상 원소 수: 라인 수 * 평균 단어 수
	expectedWords := uint(len(lines) * 5)
	bloom := NewBloomFilter(expectedWords, 0.01) // 1% FP

	for _, line := range lines {
		// 각 라인의 단어를 블룸 필터에 추가
		words := strings.Fields(line)
		for _, word := range words {
			bloom.AddString(strings.ToLower(word))
		}
		// 전체 라인도 추가
		bloom.AddString(strings.ToLower(line))
	}

	return &Chunk{
		ID:     id,
		Labels: labels,
		Lines:  lines,
		Bloom:  bloom,
	}
}

// MayContain은 블룸 필터를 사용하여 검색어가 이 청크에 있을 수 있는지 확인한다.
func (c *Chunk) MayContain(keyword string) bool {
	return c.Bloom.TestString(strings.ToLower(keyword))
}

// ActuallyContains는 실제로 검색어가 포함되어 있는지 확인한다 (전체 스캔).
func (c *Chunk) ActuallyContains(keyword string) bool {
	lower := strings.ToLower(keyword)
	for _, line := range c.Lines {
		if strings.Contains(strings.ToLower(line), lower) {
			return true
		}
	}
	return false
}

// =============================================================================
// 4. 유틸리티
// =============================================================================

// measureFPRate는 블룸 필터의 실제 false positive 비율을 측정한다.
func measureFPRate(bf *BloomFilter, insertedItems []string, testCount int) (float64, int, int) {
	// 존재하지 않는 항목으로 테스트
	inserted := make(map[string]bool)
	for _, item := range insertedItems {
		inserted[item] = true
	}

	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	fp := 0
	tn := 0

	for i := 0; i < testCount; i++ {
		// 존재하지 않는 랜덤 항목 생성
		testItem := fmt.Sprintf("nonexistent-item-%d-%d", i, r.Intn(1000000))
		if inserted[testItem] {
			continue
		}

		if bf.TestString(testItem) {
			fp++ // False Positive
		} else {
			tn++ // True Negative
		}
	}

	rate := float64(fp) / float64(fp+tn)
	return rate, fp, tn
}

// =============================================================================
// 5. 메인 함수 - 블룸 필터 시연
// =============================================================================

func main() {
	fmt.Println("=== Loki PoC #12: 블룸 필터 - 블룸 필터 기반 로그 검색 가속 ===")
	fmt.Println()

	// =========================================================================
	// 시연 1: 블룸 필터 기본 동작
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("[시연 1] 블룸 필터 기본 동작")
	fmt.Println()

	// 작은 블룸 필터로 동작 원리 시연
	bf := NewBloomFilterWithParams(32, 3)
	fmt.Println("  설정: 32 bits, 3 hash functions")
	fmt.Println()

	// 원소 추가 및 비트 배열 변화 관찰
	words := []string{"error", "timeout", "connection"}
	for _, word := range words {
		bf.AddString(word)
		vis := bf.Visualize(32)
		fmt.Printf("  Add(%q):\n", word)
		fmt.Printf("    비트: [%s]\n", vis)
		fmt.Printf("    해시 위치: %v\n", bf.hashValues([]byte(word)))
		fmt.Println()
	}

	// 조회 테스트
	testWords := []string{"error", "timeout", "success", "failure", "connection"}
	fmt.Println("  조회 결과:")
	for _, word := range testWords {
		result := bf.TestString(word)
		actual := false
		for _, w := range words {
			if w == word {
				actual = true
				break
			}
		}
		status := "True Positive"
		if result && !actual {
			status = "FALSE POSITIVE!"
		} else if !result && actual {
			status = "FALSE NEGATIVE (이건 버그!)"
		} else if !result && !actual {
			status = "True Negative"
		}
		fmt.Printf("    Test(%q): %v → %s\n", word, result, status)
	}
	fmt.Println()

	// =========================================================================
	// 시연 2: 파라미터와 False Positive 관계
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("[시연 2] 파라미터와 False Positive Rate 관계")
	fmt.Println()

	// 다양한 설정으로 FP 확률 측정
	configs := []struct {
		items  uint
		fpRate float64
	}{
		{1000, 0.1},    // 10% FP
		{1000, 0.01},   // 1% FP
		{1000, 0.001},  // 0.1% FP
		{1000, 0.0001}, // 0.01% FP
	}

	fmt.Println("  n=1000 원소, 다양한 FP 확률 목표:")
	fmt.Printf("  %-12s %-10s %-8s %-12s %-12s %-12s\n",
		"목표 FP", "bits(m)", "hash(k)", "메모리", "이론 FP", "실측 FP")
	fmt.Println("  " + strings.Repeat("-", 70))

	for _, cfg := range configs {
		filter := NewBloomFilter(cfg.items, cfg.fpRate)

		// 1000개 항목 삽입
		items := make([]string, cfg.items)
		for i := uint(0); i < cfg.items; i++ {
			item := fmt.Sprintf("item-%d", i)
			items[i] = item
			filter.AddString(item)
		}

		// 실제 FP 측정
		actualFP, _, _ := measureFPRate(filter, items, 10000)

		fmt.Printf("  %-12.4f %-10d %-8d %-12s %-12.6f %-12.6f\n",
			cfg.fpRate, filter.size, filter.hashCount,
			fmt.Sprintf("%.1f KB", float64(filter.size)/8/1024),
			filter.EstimatedFPRate(), actualFP)
	}
	fmt.Println()
	fmt.Println("  관찰:")
	fmt.Println("    - FP 확률을 낮추려면 더 많은 비트(메모리)와 해시 함수가 필요")
	fmt.Println("    - 10배 FP 감소 ≈ 4.8배 메모리 증가 (ln(10)/ln(2)^2)")
	fmt.Println()

	// =========================================================================
	// 시연 3: Scalable Bloom Filter (자동 확장)
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("[시연 3] Scalable Bloom Filter (자동 확장)")
	fmt.Println()

	sbf := NewScalableBloomFilter(100, 0.01)
	fmt.Println("  초기 설정: 100 예상 원소, 1% FP")
	fmt.Println()

	// 500개 항목을 단계적으로 추가하며 확장 관찰
	milestones := []int{50, 100, 200, 300, 500}
	current := 0
	for _, target := range milestones {
		for i := current; i < target; i++ {
			sbf.AddString(fmt.Sprintf("scalable-item-%d", i))
		}
		current = target
		fmt.Printf("  %d개 원소 추가 후:\n", target)
		sbf.Stats()
		fmt.Println()
	}

	// 조회 테스트
	fmt.Println("  조회 테스트:")
	for _, idx := range []int{0, 100, 499, -1} {
		var item string
		var expected bool
		if idx >= 0 {
			item = fmt.Sprintf("scalable-item-%d", idx)
			expected = true
		} else {
			item = "nonexistent-item"
			expected = false
		}
		result := sbf.TestString(item)
		fmt.Printf("    Test(%q): %v (expected=%v)\n", item, result, expected)
	}
	fmt.Println()

	// =========================================================================
	// 시연 4: Loki 청크 필터링 시뮬레이션
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("[시연 4] Loki 청크 필터링 시뮬레이션")
	fmt.Println()

	// 여러 청크 생성 (각 청크는 다른 로그 내용을 가짐)
	chunks := []*Chunk{
		NewChunk("chunk-001", `{app="api"}`, []string{
			"Starting API server on port 8080",
			"Connected to database successfully",
			"Processing request from 192.168.1.1",
			"Response sent in 150ms",
			"Error: connection timeout to redis",
		}),
		NewChunk("chunk-002", `{app="api"}`, []string{
			"Received POST /api/users",
			"Validating user input",
			"User created successfully",
			"Sending welcome email notification",
			"Request completed in 200ms",
		}),
		NewChunk("chunk-003", `{app="web"}`, []string{
			"Serving static files from /public",
			"Error: 404 not found /favicon.ico",
			"Cache miss for index.html",
			"Error: connection refused to backend",
			"Retry attempt 3 for backend connection",
		}),
		NewChunk("chunk-004", `{app="worker"}`, []string{
			"Starting background job processor",
			"Processing job queue batch-001",
			"Job completed successfully in 5s",
			"Scheduling next batch in 30s",
			"Memory usage: 256MB / 1GB",
		}),
		NewChunk("chunk-005", `{app="api"}`, []string{
			"Error: null pointer exception in handler",
			"Stack trace: main.go:42 -> handler.go:15",
			"Panic recovery activated",
			"Error: database connection pool exhausted",
			"Circuit breaker opened for database",
		}),
	}

	// 검색어로 청크 필터링
	searchTerms := []string{"error", "database", "queue", "timeout", "kubernetes"}

	fmt.Println("  청크 목록:")
	for _, c := range chunks {
		fmt.Printf("    %s (%s): %d lines, bloom bits=%d\n",
			c.ID, c.Labels, len(c.Lines), c.Bloom.size)
	}
	fmt.Println()

	for _, term := range searchTerms {
		fmt.Printf("  검색어: %q\n", term)
		bloomHits := 0
		actualHits := 0
		falsePositives := 0
		skipped := 0

		for _, chunk := range chunks {
			mayContain := chunk.MayContain(term)
			actualContain := chunk.ActuallyContains(term)

			var status string
			if mayContain && actualContain {
				status = "BLOOM HIT + ACTUAL HIT (정상 - 읽기 필요)"
				bloomHits++
				actualHits++
			} else if mayContain && !actualContain {
				status = "BLOOM HIT + ACTUAL MISS (False Positive - 불필요한 읽기)"
				bloomHits++
				falsePositives++
			} else if !mayContain {
				status = "BLOOM SKIP (건너뛰기 - I/O 절약)"
				skipped++
			}

			fmt.Printf("    %s: %s\n", chunk.ID, status)
		}

		fmt.Printf("    → 블룸 히트: %d, 실제 히트: %d, FP: %d, 건너뛰기: %d\n",
			bloomHits, actualHits, falsePositives, skipped)
		if skipped > 0 {
			savings := float64(skipped) / float64(len(chunks)) * 100
			fmt.Printf("    → I/O 절약: %.0f%% (%d/%d 청크 건너뛰기)\n",
				savings, skipped, len(chunks))
		}
		fmt.Println()
	}

	// =========================================================================
	// 시연 5: False Positive Rate 실측
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("[시연 5] False Positive Rate 실측 (대규모)")
	fmt.Println()

	testSizes := []uint{100, 1000, 10000}
	targetFP := 0.01 // 1%

	for _, n := range testSizes {
		filter := NewBloomFilter(n, targetFP)

		// n개 항목 삽입
		items := make([]string, n)
		for i := uint(0); i < n; i++ {
			item := fmt.Sprintf("element-%d", i)
			items[i] = item
			filter.AddString(item)
		}

		// FP 측정 (10,000번 테스트)
		actualRate, fpCount, tnCount := measureFPRate(filter, items, 10000)

		fmt.Printf("  n=%5d: bits=%6d, k=%d\n", n, filter.size, filter.hashCount)
		fmt.Printf("    이론 FP: %.4f%%, 실측 FP: %.4f%% (%d FP / %d TN)\n",
			filter.EstimatedFPRate()*100, actualRate*100, fpCount, tnCount)
		fmt.Printf("    채움 비율: %.1f%%\n", filter.FillRatio()*100)
		fmt.Println()
	}

	// =========================================================================
	// 시연 6: 비트 배열 시각화
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("[시연 6] 비트 배열 시각화")
	fmt.Println()

	visBF := NewBloomFilterWithParams(64, 3)
	fmt.Println("  빈 블룸 필터 (64 bits):")
	fmt.Printf("    [%s]\n", visBF.Visualize(64))
	fmt.Println()

	vizItems := []string{"apple", "banana", "cherry", "date", "elderberry"}
	for _, item := range vizItems {
		visBF.AddString(item)
		fmt.Printf("  Add(%q):\n", item)
		fmt.Printf("    [%s]  (채움: %.0f%%)\n",
			visBF.Visualize(64), visBF.FillRatio()*100)
	}

	// =========================================================================
	// 구조 요약
	// =========================================================================
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("=== 블룸 필터 구조 요약 ===")
	fmt.Println()
	fmt.Println("  Loki의 블룸 필터 기반 로그 검색 가속:")
	fmt.Println()
	fmt.Println("  쿼리: {app=\"api\"} |= \"error\"")
	fmt.Println("       │")
	fmt.Println("       ▼")
	fmt.Println("  ┌─────────────────────────────────────────────┐")
	fmt.Println("  │ 1. 인덱스에서 청크 목록 조회               │")
	fmt.Println("  │    chunk-001, chunk-002, ..., chunk-100     │")
	fmt.Println("  └──────────────────┬──────────────────────────┘")
	fmt.Println("                     │")
	fmt.Println("                     ▼")
	fmt.Println("  ┌─────────────────────────────────────────────┐")
	fmt.Println("  │ 2. 블룸 필터로 청크 필터링                  │")
	fmt.Println("  │    chunk-001: bloom.Test(\"error\") → true   │")
	fmt.Println("  │    chunk-002: bloom.Test(\"error\") → false  │ ← SKIP!")
	fmt.Println("  │    chunk-003: bloom.Test(\"error\") → true   │")
	fmt.Println("  │    ...                                      │")
	fmt.Println("  │    100개 → 30개로 축소 (70% I/O 절약)       │")
	fmt.Println("  └──────────────────┬──────────────────────────┘")
	fmt.Println("                     │")
	fmt.Println("                     ▼")
	fmt.Println("  ┌─────────────────────────────────────────────┐")
	fmt.Println("  │ 3. 필터 통과한 청크만 실제 읽기+검색        │")
	fmt.Println("  └─────────────────────────────────────────────┘")
	fmt.Println()
	fmt.Println("  핵심 포인트:")
	fmt.Println("    - False Positive 허용, False Negative 불가")
	fmt.Println("    - 최적 파라미터: m=-n*ln(p)/ln(2)^2, k=(m/n)*ln(2)")
	fmt.Println("    - Double Hashing: 2개 해시로 k개 해시 생성")
	fmt.Println("    - Scalable: 원소 수 증가 시 자동 확장")
	fmt.Println("    - Loki 활용: 청크 필터링으로 I/O 50~90% 절약")
	fmt.Println("    - Loki 실제 코드: pkg/storage/bloom/v1/")
}
