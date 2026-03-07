package main

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Loki PoC #18: 카나리 모니터 - Loki Canary 기반 로그 파이프라인 헬스체크
// =============================================================================
//
// Loki Canary(cmd/loki-canary/)는 로그 파이프라인의 건강 상태를 모니터링하는 도구다.
// 알려진 로그 엔트리를 주기적으로 생성(Writer)하고, 이를 쿼리(Reader)하여
// 누락, 지연, 일관성을 측정한다.
//
// 핵심 개념:
// 1. Canary Writer: 시퀀스 번호가 포함된 로그 엔트리를 주기적으로 생성
// 2. Canary Reader: Loki에서 카나리 엔트리를 쿼리하여 수신 확인
// 3. Comparator: 전송/수신 비교 → 지연 시간, 누락 엔트리 감지
// 4. Spot Check: 과거 엔트리를 무작위로 다시 검증
// 5. Metric Test: count_over_time 등 메트릭 쿼리 검증
//
// 참조: cmd/loki-canary/main.go, pkg/canary/writer/, pkg/canary/reader/,
//       pkg/canary/comparator/

// =============================================================================
// CanaryEntry: 카나리 로그 엔트리
// =============================================================================

// CanaryEntry 는 카나리가 생성하는 하나의 로그 엔트리
type CanaryEntry struct {
	Timestamp  time.Time
	SequenceNo int64
	Content    string
}

// Format 은 엔트리를 로그 라인으로 포맷팅한다
// Loki Canary 실제 형식: 타임스탬프(나노초) + 시퀀스 번호
func (e *CanaryEntry) Format() string {
	return fmt.Sprintf("%d %d %s", e.Timestamp.UnixNano(), e.SequenceNo, e.Content)
}

// =============================================================================
// LogPipeline: Loki 로그 파이프라인 시뮬레이션
// =============================================================================
// 실제로는 Loki Distributor → Ingester → Storage → Querier 경로를 거치지만,
// 여기서는 간단히 in-memory 파이프라인으로 시뮬레이션한다.

type LogPipeline struct {
	mu             sync.Mutex
	entries        []CanaryEntry
	dropRate       float64       // 시뮬레이션용 엔트리 드롭 비율 (0~1)
	latencyMin     time.Duration // 시뮬레이션용 최소 지연
	latencyMax     time.Duration // 시뮬레이션용 최대 지연
	ingestDelays   map[int64]time.Duration // 시퀀스 → 실제 수집 시 추가 지연
}

func NewLogPipeline(dropRate float64, latencyMin, latencyMax time.Duration) *LogPipeline {
	return &LogPipeline{
		dropRate:     dropRate,
		latencyMin:   latencyMin,
		latencyMax:   latencyMax,
		ingestDelays: make(map[int64]time.Duration),
	}
}

// Ingest 는 로그 엔트리를 파이프라인에 수집한다
func (lp *LogPipeline) Ingest(entry CanaryEntry) bool {
	lp.mu.Lock()
	defer lp.mu.Unlock()

	// 드롭 시뮬레이션
	if rand.Float64() < lp.dropRate {
		return false // 엔트리 누락
	}

	// 수집 지연 시뮬레이션
	delay := lp.latencyMin + time.Duration(rand.Int63n(int64(lp.latencyMax-lp.latencyMin)))
	lp.ingestDelays[entry.SequenceNo] = delay

	lp.entries = append(lp.entries, entry)
	return true
}

// Query 는 지정된 시간 범위의 엔트리를 반환한다 (Loki 쿼리 시뮬레이션)
func (lp *LogPipeline) Query(from, through time.Time) []CanaryEntry {
	lp.mu.Lock()
	defer lp.mu.Unlock()

	var results []CanaryEntry
	for _, entry := range lp.entries {
		if !entry.Timestamp.Before(from) && entry.Timestamp.Before(through) {
			results = append(results, entry)
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Timestamp.Before(results[j].Timestamp)
	})
	return results
}

// GetDelay 는 특정 시퀀스의 수집 지연을 반환한다
func (lp *LogPipeline) GetDelay(seqNo int64) time.Duration {
	lp.mu.Lock()
	defer lp.mu.Unlock()
	return lp.ingestDelays[seqNo]
}

// =============================================================================
// CanaryWriter: 카나리 로그 생성기
// =============================================================================
// Loki 실제 코드: pkg/canary/writer/writer.go
//
// 설정 가능한 간격으로 시퀀스 번호가 포함된 로그 엔트리를 생성한다.
// 생성된 엔트리의 타임스탬프를 sentChan으로 전송하여 Comparator가 추적할 수 있게 한다.

type CanaryWriter struct {
	mu          sync.Mutex
	pipeline    *LogPipeline
	interval    time.Duration
	sequenceNo  int64
	sentTimes   []time.Time
	sentEntries []CanaryEntry
	size        int    // 로그 라인 크기
}

func NewCanaryWriter(pipeline *LogPipeline, interval time.Duration, size int) *CanaryWriter {
	return &CanaryWriter{
		pipeline: pipeline,
		interval: interval,
		size:     size,
	}
}

// Write 는 카나리 엔트리를 생성하고 파이프라인에 전송한다
// Loki 실제 코드: Writer.run() → 주기적으로 엔트리 생성
func (w *CanaryWriter) Write(now time.Time) (*CanaryEntry, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.sequenceNo++

	// 고정 크기 콘텐츠 생성
	content := strings.Repeat("x", w.size)

	entry := &CanaryEntry{
		Timestamp:  now,
		SequenceNo: w.sequenceNo,
		Content:    content,
	}

	// 파이프라인에 전송
	ingested := w.pipeline.Ingest(*entry)

	// 전송 시간 기록 (Comparator가 추적)
	w.sentTimes = append(w.sentTimes, now)
	w.sentEntries = append(w.sentEntries, *entry)

	return entry, ingested
}

// SentEntries 는 전송된 모든 엔트리를 반환한다
func (w *CanaryWriter) SentEntries() []CanaryEntry {
	w.mu.Lock()
	defer w.mu.Unlock()
	result := make([]CanaryEntry, len(w.sentEntries))
	copy(result, w.sentEntries)
	return result
}

// =============================================================================
// CanaryReader: 카나리 로그 쿼리
// =============================================================================
// Loki 실제 코드: pkg/canary/reader/reader.go
//
// Loki API를 주기적으로 쿼리하여 카나리 엔트리를 확인한다.
// 수신된 엔트리의 타임스탬프를 receivedChan으로 전송한다.

type CanaryReader struct {
	pipeline        *LogPipeline
	receivedEntries map[int64]time.Time // 시퀀스 → 수신 시간
}

func NewCanaryReader(pipeline *LogPipeline) *CanaryReader {
	return &CanaryReader{
		pipeline:        pipeline,
		receivedEntries: make(map[int64]time.Time),
	}
}

// QueryAndReceive 는 파이프라인에서 엔트리를 쿼리하고 수신을 기록한다
func (r *CanaryReader) QueryAndReceive(from, through time.Time) []CanaryEntry {
	entries := r.pipeline.Query(from, through)
	for _, entry := range entries {
		if _, exists := r.receivedEntries[entry.SequenceNo]; !exists {
			r.receivedEntries[entry.SequenceNo] = time.Now()
		}
	}
	return entries
}

// IsReceived 는 특정 시퀀스가 수신되었는지 확인한다
func (r *CanaryReader) IsReceived(seqNo int64) bool {
	_, ok := r.receivedEntries[seqNo]
	return ok
}

// =============================================================================
// Comparator: 전송/수신 비교 및 메트릭 계산
// =============================================================================
// Loki 실제 코드: pkg/canary/comparator/comparator.go
//
// 주요 기능:
// 1. 지연 시간 측정: 전송 → 수신까지의 시간
// 2. 누락 엔트리 감지: 전송했지만 수신되지 않은 엔트리
// 3. Spot Check: 과거 엔트리를 무작위로 다시 검증
// 4. Metric Test: count_over_time 등 집계 쿼리 검증

type CompareResult struct {
	TotalSent     int
	TotalReceived int
	Missing       int
	MissingSeqs   []int64
	AvgLatency    time.Duration
	MaxLatency    time.Duration
	MinLatency    time.Duration
	P99Latency    time.Duration
	Latencies     []time.Duration
}

type Comparator struct {
	writer     *CanaryWriter
	reader     *CanaryReader
	pipeline   *LogPipeline
	wait       time.Duration // 수신 대기 시간
	maxWait    time.Duration // 최대 대기 시간 (이후 누락으로 판정)

	// Spot Check 설정
	spotCheckInterval time.Duration
	spotCheckEntries  []CanaryEntry // 스팟 체크용으로 저장된 엔트리
}

func NewComparator(writer *CanaryWriter, reader *CanaryReader, pipeline *LogPipeline,
	wait, maxWait, spotCheckInterval time.Duration) *Comparator {
	return &Comparator{
		writer:            writer,
		reader:            reader,
		pipeline:          pipeline,
		wait:              wait,
		maxWait:           maxWait,
		spotCheckInterval: spotCheckInterval,
	}
}

// Compare 는 전송/수신 엔트리를 비교하여 결과를 반환한다
// Loki 실제 코드: Comparator.run() → 주기적으로 비교 수행
func (c *Comparator) Compare(queryFrom, queryThrough time.Time) *CompareResult {
	sentEntries := c.writer.SentEntries()
	receivedEntries := c.reader.QueryAndReceive(queryFrom, queryThrough)

	// 수신된 시퀀스 번호 맵
	receivedSet := make(map[int64]bool)
	for _, entry := range receivedEntries {
		receivedSet[entry.SequenceNo] = true
	}

	result := &CompareResult{
		TotalSent:     len(sentEntries),
		TotalReceived: len(receivedEntries),
	}

	// 누락 및 지연 계산
	var latencies []time.Duration
	var missingSeqs []int64

	for _, sent := range sentEntries {
		if receivedSet[sent.SequenceNo] {
			// 수집 지연 계산
			delay := c.pipeline.GetDelay(sent.SequenceNo)
			latencies = append(latencies, delay)
		} else {
			missingSeqs = append(missingSeqs, sent.SequenceNo)
		}
	}

	result.Missing = len(missingSeqs)
	result.MissingSeqs = missingSeqs
	result.Latencies = latencies

	if len(latencies) > 0 {
		sort.Slice(latencies, func(i, j int) bool {
			return latencies[i] < latencies[j]
		})

		var total time.Duration
		for _, l := range latencies {
			total += l
		}
		result.AvgLatency = total / time.Duration(len(latencies))
		result.MinLatency = latencies[0]
		result.MaxLatency = latencies[len(latencies)-1]

		// P99
		p99Idx := int(float64(len(latencies)) * 0.99)
		if p99Idx >= len(latencies) {
			p99Idx = len(latencies) - 1
		}
		result.P99Latency = latencies[p99Idx]
	}

	return result
}

// SpotCheck 는 과거 엔트리 중 하나를 무작위로 선택하여 다시 검증한다
// Loki 실제 코드: Comparator.spotCheckEntries → 주기적으로 과거 엔트리 재검증
func (c *Comparator) SpotCheck(from, through time.Time) *SpotCheckResult {
	// 스팟 체크용 엔트리 수집 (일정 간격으로 하나씩 저장)
	sentEntries := c.writer.SentEntries()
	if len(sentEntries) == 0 {
		return nil
	}

	// 무작위 엔트리 선택
	idx := rand.Intn(len(sentEntries))
	selected := sentEntries[idx]

	// 파이프라인에서 쿼리
	found := false
	queryFrom := selected.Timestamp.Add(-1 * time.Second)
	queryThrough := selected.Timestamp.Add(1 * time.Second)
	results := c.pipeline.Query(queryFrom, queryThrough)

	for _, r := range results {
		if r.SequenceNo == selected.SequenceNo {
			found = true
			break
		}
	}

	return &SpotCheckResult{
		Entry:    selected,
		Found:    found,
		CheckedAt: time.Now(),
		Age:      time.Since(selected.Timestamp),
	}
}

// SpotCheckResult 는 스팟 체크 결과
type SpotCheckResult struct {
	Entry     CanaryEntry
	Found     bool
	CheckedAt time.Time
	Age       time.Duration
}

// =============================================================================
// LatencyHistogram: 지연 시간 히스토그램
// =============================================================================

type LatencyHistogram struct {
	buckets []time.Duration
	counts  []int
}

func NewLatencyHistogram(bucketCount int, maxLatency time.Duration) *LatencyHistogram {
	buckets := make([]time.Duration, bucketCount)
	counts := make([]int, bucketCount)
	step := maxLatency / time.Duration(bucketCount)
	for i := 0; i < bucketCount; i++ {
		buckets[i] = step * time.Duration(i+1)
	}
	return &LatencyHistogram{buckets: buckets, counts: counts}
}

func (h *LatencyHistogram) Add(latency time.Duration) {
	for i, bucket := range h.buckets {
		if latency <= bucket {
			h.counts[i]++
			return
		}
	}
	// 마지막 버킷에 추가
	h.counts[len(h.counts)-1]++
}

func (h *LatencyHistogram) Print() {
	maxCount := 0
	for _, c := range h.counts {
		if c > maxCount {
			maxCount = c
		}
	}

	for i, bucket := range h.buckets {
		barLen := 0
		if maxCount > 0 {
			barLen = h.counts[i] * 30 / maxCount
		}
		bar := strings.Repeat("█", barLen)
		fmt.Printf("    ≤%8s: %3d %s\n", bucket.String(), h.counts[i], bar)
	}
}

// =============================================================================
// 메인 함수
// =============================================================================

func main() {
	fmt.Println("=== Loki Canary 모니터 시뮬레이션 ===")
	fmt.Println()

	// ─────────────────────────────────────────────────────────────
	// 1단계: 파이프라인 및 카나리 설정
	// ─────────────────────────────────────────────────────────────
	fmt.Println("--- [1] 카나리 설정 ---")
	fmt.Println()

	// 파이프라인: 5% 드롭 비율, 10~200ms 지연
	pipeline := NewLogPipeline(0.05, 10*time.Millisecond, 200*time.Millisecond)

	writer := NewCanaryWriter(pipeline, 100*time.Millisecond, 100)
	reader := NewCanaryReader(pipeline)
	comparator := NewComparator(writer, reader, pipeline,
		time.Second, 5*time.Second, 15*time.Second)

	fmt.Println("  파이프라인 설정:")
	fmt.Printf("    드롭 비율: %.0f%%\n", pipeline.dropRate*100)
	fmt.Printf("    수집 지연: %s ~ %s\n", pipeline.latencyMin, pipeline.latencyMax)
	fmt.Println()
	fmt.Println("  카나리 설정:")
	fmt.Printf("    쓰기 간격: %s\n", writer.interval)
	fmt.Printf("    엔트리 크기: %d bytes\n", writer.size)
	fmt.Printf("    수신 대기: %s\n", comparator.wait)
	fmt.Printf("    최대 대기: %s\n", comparator.maxWait)
	fmt.Println()

	// ─────────────────────────────────────────────────────────────
	// 2단계: 카나리 엔트리 생성 (Writer)
	// ─────────────────────────────────────────────────────────────
	fmt.Println("--- [2] 카나리 엔트리 생성 (Writer) ---")
	fmt.Println()

	baseTime := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	totalEntries := 50
	ingestedCount := 0
	droppedCount := 0

	for i := 0; i < totalEntries; i++ {
		ts := baseTime.Add(time.Duration(i) * writer.interval)
		_, ingested := writer.Write(ts)
		if ingested {
			ingestedCount++
		} else {
			droppedCount++
		}
	}

	fmt.Printf("  전송: %d 엔트리\n", totalEntries)
	fmt.Printf("  수집 성공: %d 엔트리\n", ingestedCount)
	fmt.Printf("  드롭: %d 엔트리 (%.1f%%)\n", droppedCount, float64(droppedCount)/float64(totalEntries)*100)
	fmt.Println()

	// ─────────────────────────────────────────────────────────────
	// 3단계: 카나리 엔트리 쿼리 (Reader)
	// ─────────────────────────────────────────────────────────────
	fmt.Println("--- [3] 카나리 엔트리 쿼리 (Reader) ---")
	fmt.Println()

	queryFrom := baseTime.Add(-1 * time.Second)
	queryThrough := baseTime.Add(time.Duration(totalEntries) * writer.interval)
	received := reader.QueryAndReceive(queryFrom, queryThrough)

	fmt.Printf("  쿼리 범위: %s ~ %s\n",
		queryFrom.Format("15:04:05.000"),
		queryThrough.Format("15:04:05.000"))
	fmt.Printf("  수신: %d 엔트리\n", len(received))
	fmt.Println()

	// ─────────────────────────────────────────────────────────────
	// 4단계: 비교 분석 (Comparator)
	// ─────────────────────────────────────────────────────────────
	fmt.Println("--- [4] 비교 분석 (Comparator) ---")
	fmt.Println()

	result := comparator.Compare(queryFrom, queryThrough)

	fmt.Printf("  전송: %d | 수신: %d | 누락: %d\n",
		result.TotalSent, result.TotalReceived, result.Missing)
	fmt.Printf("  누락률: %.1f%%\n", float64(result.Missing)/float64(result.TotalSent)*100)
	fmt.Println()

	if result.Missing > 0 {
		fmt.Println("  누락된 시퀀스 번호:")
		for i, seq := range result.MissingSeqs {
			if i >= 10 {
				fmt.Printf("    ... 총 %d개\n", len(result.MissingSeqs))
				break
			}
			fmt.Printf("    시퀀스 #%d\n", seq)
		}
		fmt.Println()
	}

	if len(result.Latencies) > 0 {
		fmt.Println("  지연 시간 통계:")
		fmt.Printf("    최소: %s\n", result.MinLatency)
		fmt.Printf("    평균: %s\n", result.AvgLatency)
		fmt.Printf("    P99:  %s\n", result.P99Latency)
		fmt.Printf("    최대: %s\n", result.MaxLatency)
		fmt.Println()

		// 히스토그램
		fmt.Println("  지연 시간 분포:")
		histogram := NewLatencyHistogram(5, 250*time.Millisecond)
		for _, lat := range result.Latencies {
			histogram.Add(lat)
		}
		histogram.Print()
		fmt.Println()
	}

	// ─────────────────────────────────────────────────────────────
	// 5단계: 스팟 체크 (Spot Check)
	// ─────────────────────────────────────────────────────────────
	fmt.Println("--- [5] 스팟 체크 (Spot Check) ---")
	fmt.Println()

	fmt.Println("  스팟 체크: 과거 엔트리를 무작위로 선택하여 다시 검증")
	fmt.Println("  Loki 실제: spot-check-interval (기본 15분) 간격으로 하나씩 저장")
	fmt.Println("  Loki 실제: spot-check-max (기본 4시간) 전까지 보존 후 폐기")
	fmt.Println()

	// 5번의 스팟 체크 수행
	for i := 0; i < 5; i++ {
		spotResult := comparator.SpotCheck(queryFrom, queryThrough)
		if spotResult != nil {
			foundStr := "발견됨"
			if !spotResult.Found {
				foundStr = "발견되지 않음 (누락!)"
			}
			fmt.Printf("    스팟 체크 %d: 시퀀스 #%d → %s\n",
				i+1, spotResult.Entry.SequenceNo, foundStr)
		}
	}
	fmt.Println()

	// ─────────────────────────────────────────────────────────────
	// 6단계: 다양한 파이프라인 건강 시나리오
	// ─────────────────────────────────────────────────────────────
	fmt.Println("--- [6] 파이프라인 건강 시나리오 ---")
	fmt.Println()

	scenarios := []struct {
		name       string
		dropRate   float64
		latencyMin time.Duration
		latencyMax time.Duration
	}{
		{"정상 (드롭 0%, 지연 1~10ms)", 0.0, 1 * time.Millisecond, 10 * time.Millisecond},
		{"경미한 문제 (드롭 2%, 지연 10~100ms)", 0.02, 10 * time.Millisecond, 100 * time.Millisecond},
		{"심각한 문제 (드롭 10%, 지연 100~500ms)", 0.10, 100 * time.Millisecond, 500 * time.Millisecond},
		{"장애 상태 (드롭 50%, 지연 500~2000ms)", 0.50, 500 * time.Millisecond, 2000 * time.Millisecond},
	}

	fmt.Println("  ┌──────────────────────────────────────┬──────┬──────┬────────┬──────────┐")
	fmt.Println("  │ 시나리오                             │ 전송 │ 수신 │ 누락   │ 평균지연 │")
	fmt.Println("  ├──────────────────────────────────────┼──────┼──────┼────────┼──────────┤")

	for _, scenario := range scenarios {
		p := NewLogPipeline(scenario.dropRate, scenario.latencyMin, scenario.latencyMax)
		w := NewCanaryWriter(p, 100*time.Millisecond, 50)
		r := NewCanaryReader(p)
		comp := NewComparator(w, r, p, time.Second, 5*time.Second, 15*time.Second)

		for i := 0; i < 100; i++ {
			ts := baseTime.Add(time.Duration(i) * w.interval)
			w.Write(ts)
		}

		qFrom := baseTime.Add(-1 * time.Second)
		qThrough := baseTime.Add(100 * w.interval)
		res := comp.Compare(qFrom, qThrough)

		missRate := ""
		if res.TotalSent > 0 {
			missRate = fmt.Sprintf("%.0f%%", float64(res.Missing)/float64(res.TotalSent)*100)
		}

		latStr := "-"
		if len(res.Latencies) > 0 {
			latStr = res.AvgLatency.String()
		}

		fmt.Printf("  │ %-36s │ %4d │ %4d │ %4s   │ %8s │\n",
			scenario.name, res.TotalSent, res.TotalReceived, missRate, latStr)
	}
	fmt.Println("  └──────────────────────────────────────┴──────┴──────┴────────┴──────────┘")
	fmt.Println()

	// ─────────────────────────────────────────────────────────────
	// 동작 원리 요약
	// ─────────────────────────────────────────────────────────────
	fmt.Println("=== Loki Canary 동작 원리 요약 ===")
	fmt.Println()
	fmt.Println("  1. Writer: 주기적으로 시퀀스 번호가 포함된 로그 생성")
	fmt.Println("     → sentChan으로 전송 시간 기록")
	fmt.Println("  2. Reader: Loki API를 쿼리하여 카나리 엔트리 수신")
	fmt.Println("     → receivedChan으로 수신 시간 기록")
	fmt.Println("  3. Comparator: 전송/수신 비교")
	fmt.Println("     → 누락 감지, 지연 시간 측정, 히스토그램 생성")
	fmt.Println("  4. Spot Check: 과거 엔트리를 무작위로 재검증")
	fmt.Println("     → 장기적 데이터 무결성 확인")
	fmt.Println("  5. Metric Test: count_over_time 등 집계 쿼리 검증")
	fmt.Println("     → 쿼리 엔진의 정확성 확인")
	fmt.Println()
	fmt.Println("  Loki 핵심 코드 경로:")
	fmt.Println("  - cmd/loki-canary/main.go             → 카나리 진입점, 설정")
	fmt.Println("  - pkg/canary/writer/writer.go          → 카나리 로그 생성")
	fmt.Println("  - pkg/canary/reader/reader.go          → 카나리 로그 쿼리")
	fmt.Println("  - pkg/canary/comparator/comparator.go  → 전송/수신 비교, 메트릭")
}
