package main

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// =============================================================================
// Kafka 성능 벤치마크 시뮬레이션
//
// 이 PoC는 Kafka의 성능 측정 핵심 개념을 시뮬레이션한다:
//   1. Producer 처리량 벤치마크 (ProducerPerformance)
//   2. Consumer 처리량 벤치마크 (ConsumerPerformance)
//   3. End-to-End 레이턴시 측정
//   4. Throttle 기반 처리량 제한
//   5. Histogram 기반 백분위수 계산
//
// 참조 소스:
//   tools/src/main/java/org/apache/kafka/tools/ProducerPerformance.java
//   tools/src/main/java/org/apache/kafka/tools/ConsumerPerformance.java
//   tools/src/main/java/org/apache/kafka/tools/EndToEndLatency.java
// =============================================================================

// --- Histogram: 레이턴시 분포 ---

type Histogram struct {
	mu      sync.Mutex
	values  []float64
	count   int64
	sum     float64
	min     float64
	max     float64
}

func NewHistogram() *Histogram {
	return &Histogram{min: math.MaxFloat64}
}

func (h *Histogram) Record(value float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.values = append(h.values, value)
	h.count++
	h.sum += value
	if value < h.min {
		h.min = value
	}
	if value > h.max {
		h.max = value
	}
}

func (h *Histogram) Percentile(p float64) float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.values) == 0 {
		return 0
	}
	sorted := make([]float64, len(h.values))
	copy(sorted, h.values)
	sort.Float64s(sorted)
	idx := int(float64(len(sorted)-1) * p / 100.0)
	return sorted[idx]
}

func (h *Histogram) Average() float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.count == 0 {
		return 0
	}
	return h.sum / float64(h.count)
}

func (h *Histogram) Summary() string {
	return fmt.Sprintf("avg=%.2fms, p50=%.2fms, p95=%.2fms, p99=%.2fms, min=%.2fms, max=%.2fms, count=%d",
		h.Average(), h.Percentile(50), h.Percentile(95), h.Percentile(99),
		h.min, h.max, h.count)
}

// --- Throttle: 처리량 제한 ---
// 실제: trogdor/src/main/java/.../workload/Throttle.java

type Throttle struct {
	mu          sync.Mutex
	maxPerPeriod int
	periodMs     int64
	count        int
	prevPeriod   int64
}

func NewThrottle(maxPerPeriod int, periodMs int64) *Throttle {
	return &Throttle{
		maxPerPeriod: maxPerPeriod,
		periodMs:     periodMs,
		prevPeriod:   -1,
	}
}

// Increment는 카운터를 증가시키고 필요시 대기한다.
func (t *Throttle) Increment() bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	throttled := false
	for {
		if t.count < t.maxPerPeriod {
			t.count++
			return throttled
		}
		nowMs := time.Now().UnixMilli()
		curPeriod := nowMs / t.periodMs
		if curPeriod <= t.prevPeriod {
			nextPeriodMs := (curPeriod + 1) * t.periodMs
			sleepMs := nextPeriodMs - nowMs
			if sleepMs > 0 {
				t.mu.Unlock()
				time.Sleep(time.Duration(sleepMs) * time.Millisecond)
				t.mu.Lock()
			}
			throttled = true
		} else {
			t.prevPeriod = curPeriod
			t.count = 0
		}
	}
}

// --- 시뮬레이션 토픽/파티션 ---

type Partition struct {
	mu       sync.Mutex
	Topic    string
	ID       int
	Messages []Message
}

type Message struct {
	Key       []byte
	Value     []byte
	Timestamp time.Time
	Offset    int64
}

type Topic struct {
	Name       string
	Partitions []*Partition
}

func NewTopic(name string, numPartitions int) *Topic {
	t := &Topic{Name: name}
	for i := 0; i < numPartitions; i++ {
		t.Partitions = append(t.Partitions, &Partition{Topic: name, ID: i})
	}
	return t
}

func (t *Topic) Produce(key, value []byte) int64 {
	partIdx := rand.Intn(len(t.Partitions))
	part := t.Partitions[partIdx]
	part.mu.Lock()
	defer part.mu.Unlock()
	offset := int64(len(part.Messages))
	part.Messages = append(part.Messages, Message{
		Key: key, Value: value,
		Timestamp: time.Now(),
		Offset:    offset,
	})
	return offset
}

func (t *Topic) TotalMessages() int64 {
	var total int64
	for _, p := range t.Partitions {
		p.mu.Lock()
		total += int64(len(p.Messages))
		p.mu.Unlock()
	}
	return total
}

// --- ProducerPerformance 벤치마크 ---

type ProducerBenchConfig struct {
	NumRecords      int64
	RecordSizeBytes int
	TargetMsgPerSec int
	Topic           *Topic
}

type ProducerBenchResult struct {
	TotalRecords   int64
	TotalBytes     int64
	DurationMs     int64
	ThroughputRec  float64 // records/sec
	ThroughputMB   float64 // MB/sec
	Latency        *Histogram
}

func RunProducerBench(config ProducerBenchConfig) ProducerBenchResult {
	latency := NewHistogram()
	throttle := NewThrottle(config.TargetMsgPerSec, 1000)
	value := make([]byte, config.RecordSizeBytes)
	for i := range value {
		value[i] = byte(rand.Intn(256))
	}

	start := time.Now()
	var totalRecords int64
	var totalBytes int64

	for i := int64(0); i < config.NumRecords; i++ {
		throttle.Increment()
		sendStart := time.Now()

		key := []byte(fmt.Sprintf("key-%d", i))
		config.Topic.Produce(key, value)

		sendLatency := time.Since(sendStart).Seconds() * 1000
		latency.Record(sendLatency)

		totalRecords++
		totalBytes += int64(len(key) + len(value))
	}

	duration := time.Since(start)
	return ProducerBenchResult{
		TotalRecords:  totalRecords,
		TotalBytes:    totalBytes,
		DurationMs:    duration.Milliseconds(),
		ThroughputRec: float64(totalRecords) / duration.Seconds(),
		ThroughputMB:  float64(totalBytes) / 1024 / 1024 / duration.Seconds(),
		Latency:       latency,
	}
}

// --- ConsumerPerformance 벤치마크 ---

type ConsumerBenchConfig struct {
	Topic           *Topic
	MaxMessages     int64
	FetchSizeBytes  int
}

type ConsumerBenchResult struct {
	TotalRecords   int64
	TotalBytes     int64
	DurationMs     int64
	ThroughputRec  float64
	ThroughputMB   float64
	FetchLatency   *Histogram
}

func RunConsumerBench(config ConsumerBenchConfig) ConsumerBenchResult {
	fetchLatency := NewHistogram()
	start := time.Now()
	var totalRecords int64
	var totalBytes int64

	for _, part := range config.Topic.Partitions {
		part.mu.Lock()
		for _, msg := range part.Messages {
			if config.MaxMessages > 0 && totalRecords >= config.MaxMessages {
				part.mu.Unlock()
				goto done
			}
			fetchStart := time.Now()

			totalRecords++
			totalBytes += int64(len(msg.Key) + len(msg.Value))

			lat := time.Since(fetchStart).Seconds() * 1000
			fetchLatency.Record(lat)
		}
		part.mu.Unlock()
	}
done:
	duration := time.Since(start)
	return ConsumerBenchResult{
		TotalRecords:  totalRecords,
		TotalBytes:    totalBytes,
		DurationMs:    duration.Milliseconds(),
		ThroughputRec: float64(totalRecords) / math.Max(duration.Seconds(), 0.001),
		ThroughputMB:  float64(totalBytes) / 1024 / 1024 / math.Max(duration.Seconds(), 0.001),
		FetchLatency:  fetchLatency,
	}
}

// --- End-to-End 레이턴시 ---

func RunEndToEndLatency(topic *Topic, numMessages int) *Histogram {
	latency := NewHistogram()

	// Producer와 Consumer를 동시에 실행
	var produced int64
	var consumed int64

	var wg sync.WaitGroup

	// Producer
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < numMessages; i++ {
			key := []byte(fmt.Sprintf("e2e-%d", i))
			value := []byte(fmt.Sprintf("ts:%d", time.Now().UnixNano()))
			topic.Produce(key, value)
			atomic.AddInt64(&produced, 1)
			time.Sleep(time.Millisecond)
		}
	}()

	// Consumer (지연 시작)
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(5 * time.Millisecond)
		for atomic.LoadInt64(&consumed) < int64(numMessages) {
			for _, part := range topic.Partitions {
				part.mu.Lock()
				idx := int(atomic.LoadInt64(&consumed))
				for _, msg := range part.Messages {
					if idx >= numMessages {
						break
					}
					// 레이턴시 = 소비 시점 - 생산 시점
					lat := time.Since(msg.Timestamp).Seconds() * 1000
					latency.Record(lat)
					atomic.AddInt64(&consumed, 1)
					idx++
				}
				part.mu.Unlock()
			}
			time.Sleep(time.Millisecond)
		}
	}()

	wg.Wait()
	return latency
}

// --- 메인 함수 ---

func main() {
	fmt.Println("=== Kafka 성능 벤치마크 시뮬레이션 ===")
	fmt.Println()

	// 1. Producer 벤치마크
	fmt.Println("--- 1단계: Producer 처리량 벤치마크 ---")
	topic := NewTopic("perf-test", 6)
	result := RunProducerBench(ProducerBenchConfig{
		NumRecords:      500,
		RecordSizeBytes: 1024,
		TargetMsgPerSec: 5000,
		Topic:           topic,
	})
	fmt.Printf("  레코드: %d, 바이트: %d\n", result.TotalRecords, result.TotalBytes)
	fmt.Printf("  처리량: %.0f records/sec, %.2f MB/sec\n", result.ThroughputRec, result.ThroughputMB)
	fmt.Printf("  레이턴시: %s\n", result.Latency.Summary())

	// 2. Consumer 벤치마크
	fmt.Println()
	fmt.Println("--- 2단계: Consumer 처리량 벤치마크 ---")
	cResult := RunConsumerBench(ConsumerBenchConfig{
		Topic:       topic,
		MaxMessages: 500,
	})
	fmt.Printf("  레코드: %d, 바이트: %d\n", cResult.TotalRecords, cResult.TotalBytes)
	fmt.Printf("  처리량: %.0f records/sec, %.2f MB/sec\n", cResult.ThroughputRec, cResult.ThroughputMB)

	// 3. Throttle 테스트
	fmt.Println()
	fmt.Println("--- 3단계: Throttle (처리량 제한) ---")
	throttle := NewThrottle(100, 100) // 100ms당 100개
	throttleStart := time.Now()
	throttledCount := 0
	for i := 0; i < 250; i++ {
		if throttle.Increment() {
			throttledCount++
		}
	}
	throttleDuration := time.Since(throttleStart)
	fmt.Printf("  250개 메시지 전송: %dms (스로틀됨: %d회)\n",
		throttleDuration.Milliseconds(), throttledCount)
	fmt.Printf("  실효 처리량: %.0f msg/sec\n",
		250.0/throttleDuration.Seconds())

	// 4. End-to-End 레이턴시
	fmt.Println()
	fmt.Println("--- 4단계: End-to-End 레이턴시 ---")
	e2eTopic := NewTopic("e2e-test", 1)
	e2eLatency := RunEndToEndLatency(e2eTopic, 50)
	fmt.Printf("  E2E 레이턴시: %s\n", e2eLatency.Summary())

	// 5. 파티션별 분포 확인
	fmt.Println()
	fmt.Println("--- 5단계: 파티션별 메시지 분포 ---")
	for _, p := range topic.Partitions {
		p.mu.Lock()
		fmt.Printf("  파티션 %d: %d 메시지\n", p.ID, len(p.Messages))
		p.mu.Unlock()
	}

	// 6. 다양한 레코드 크기 벤치마크
	fmt.Println()
	fmt.Println("--- 6단계: 레코드 크기별 처리량 비교 ---")
	sizes := []int{64, 256, 1024, 4096}
	for _, size := range sizes {
		t := NewTopic(fmt.Sprintf("size-%d", size), 3)
		r := RunProducerBench(ProducerBenchConfig{
			NumRecords:      200,
			RecordSizeBytes: size,
			TargetMsgPerSec: 10000,
			Topic:           t,
		})
		fmt.Printf("  %4d bytes: %.0f records/sec, %.2f MB/sec, avg_lat=%.3fms\n",
			size, r.ThroughputRec, r.ThroughputMB, r.Latency.Average())
	}

	// 요약
	fmt.Println()
	fmt.Println("=== 요약 ===")
	fmt.Println("  - Histogram: P50/P95/P99 백분위수 레이턴시 측정")
	fmt.Println("  - Throttle: 주기 기반 처리량 제한 (wait 활용)")
	fmt.Println("  - Producer/Consumer 벤치마크: 처리량(rec/s, MB/s) + 레이턴시")
	fmt.Println("  - End-to-End 레이턴시: 생산→소비 전체 경로")

	_ = strings.Join(nil, "")
}
