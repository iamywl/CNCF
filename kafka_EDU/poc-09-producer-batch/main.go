package main

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Kafka Producer RecordAccumulator Batching Simulation
// Based on: RecordAccumulator.java, Sender.java
//
// Kafka의 프로듀서는 레코드를 즉시 전송하지 않고, 파티션별 배치에 축적(accumulate)한 뒤
// 배치 크기 임계치 도달 OR linger.ms 타임아웃 중 먼저 충족되는 조건에 의해 전송한다.
// 이를 통해 처리량(throughput)과 지연(latency) 간의 트레이드오프를 제어한다.
//
// 핵심 구조:
//   RecordAccumulator: 파티션별 Deque<ProducerBatch>를 관리
//   BufferPool: 고정 크기 메모리 풀에서 배치 버퍼를 할당/반환
//   Sender 스레드: ready() -> drain() -> send() 루프를 실행
// =============================================================================

// Record는 프로듀서가 전송하는 개별 레코드를 나타낸다.
type Record struct {
	Key       string
	Value     string
	Timestamp time.Time
	SizeBytes int
}

// ProducerBatch는 동일 파티션에 축적되는 레코드 배치를 나타낸다.
// Kafka의 ProducerBatch 클래스에 대응한다.
type ProducerBatch struct {
	Partition    int
	Records      []Record
	CreatedAt    time.Time
	SizeBytes    int
	MaxSizeBytes int
	Closed       bool
}

// NewProducerBatch는 새 배치를 생성한다.
func NewProducerBatch(partition int, maxSize int) *ProducerBatch {
	return &ProducerBatch{
		Partition:    partition,
		Records:      make([]Record, 0),
		CreatedAt:    time.Now(),
		MaxSizeBytes: maxSize,
		SizeBytes:    0,
		Closed:       false,
	}
}

// TryAppend는 배치에 레코드 추가를 시도한다. 공간이 부족하면 false를 반환한다.
// RecordAccumulator.tryAppend()에 대응한다.
func (b *ProducerBatch) TryAppend(record Record) bool {
	if b.Closed || b.SizeBytes+record.SizeBytes > b.MaxSizeBytes {
		return false
	}
	b.Records = append(b.Records, record)
	b.SizeBytes += record.SizeBytes
	return true
}

// IsFull은 배치가 가득 찼는지 확인한다.
func (b *ProducerBatch) IsFull() bool {
	return b.SizeBytes >= b.MaxSizeBytes
}

// BufferPool은 고정 크기 메모리 풀을 시뮬레이션한다.
// Kafka의 BufferPool 클래스에 대응한다. buffer.memory 설정으로 총 메모리를 제한한다.
type BufferPool struct {
	mu            sync.Mutex
	totalMemory   int
	usedMemory    int
	waiters       int
	availableCond *sync.Cond
}

// NewBufferPool은 지정된 총 메모리 크기의 버퍼 풀을 생성한다.
func NewBufferPool(totalMemory int) *BufferPool {
	bp := &BufferPool{
		totalMemory: totalMemory,
		usedMemory:  0,
	}
	bp.availableCond = sync.NewCond(&bp.mu)
	return bp
}

// Allocate는 버퍼 풀에서 지정 크기의 메모리를 할당한다.
// 메모리가 부족하면 해제될 때까지 대기한다.
func (bp *BufferPool) Allocate(size int) bool {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	bp.waiters++
	for bp.usedMemory+size > bp.totalMemory {
		// 타임아웃을 시뮬레이션하기 위해 1회만 대기
		bp.availableCond.Wait()
	}
	bp.waiters--
	bp.usedMemory += size
	return true
}

// Deallocate는 메모리를 풀에 반환한다.
func (bp *BufferPool) Deallocate(size int) {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	bp.usedMemory -= size
	if bp.usedMemory < 0 {
		bp.usedMemory = 0
	}
	bp.availableCond.Signal()
}

// AvailableMemory는 사용 가능한 메모리를 반환한다.
func (bp *BufferPool) AvailableMemory() int {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	return bp.totalMemory - bp.usedMemory
}

// RecordAccumulator는 파티션별 배치 큐를 관리하는 핵심 구조체이다.
// Kafka의 RecordAccumulator 클래스에 대응한다.
type RecordAccumulator struct {
	mu        sync.Mutex
	batches   map[int][]*ProducerBatch // partition -> deque of batches
	batchSize int                      // batch.size 설정
	lingerMs  int                      // linger.ms 설정
	pool      *BufferPool
	closed    bool
}

// NewRecordAccumulator는 새 RecordAccumulator를 생성한다.
func NewRecordAccumulator(batchSize, lingerMs, totalMemory int) *RecordAccumulator {
	return &RecordAccumulator{
		batches:   make(map[int][]*ProducerBatch),
		batchSize: batchSize,
		lingerMs:  lingerMs,
		pool:      NewBufferPool(totalMemory),
	}
}

// Append는 레코드를 해당 파티션의 배치에 추가한다.
// RecordAccumulator.append() 메서드에 대응한다.
// 1) 기존 배치에 append 시도 → 성공하면 반환
// 2) 실패하면 BufferPool에서 메모리 할당 → 새 배치 생성 → append
func (ra *RecordAccumulator) Append(partition int, record Record) (batchFull bool, newBatch bool) {
	ra.mu.Lock()
	defer ra.mu.Unlock()

	deque := ra.batches[partition]

	// 기존 배치의 마지막에 추가 시도 (tryAppend)
	if len(deque) > 0 {
		last := deque[len(deque)-1]
		if last.TryAppend(record) {
			return last.IsFull() || len(deque) > 1, false
		}
		// 기존 배치가 가득 찼으므로 닫기
		last.Closed = true
	}

	// 새 배치를 위한 메모리 할당
	ra.pool.Allocate(ra.batchSize)

	// 새 배치 생성 및 레코드 추가
	batch := NewProducerBatch(partition, ra.batchSize)
	batch.TryAppend(record)
	ra.batches[partition] = append(deque, batch)

	return len(ra.batches[partition]) > 1, true
}

// Ready는 전송 준비가 된 노드(여기서는 파티션) 목록을 반환한다.
// RecordAccumulator.ready() 메서드에 대응한다.
// 조건: (1) 배치가 가득 참 OR (2) linger.ms 타임아웃 초과
func (ra *RecordAccumulator) Ready(nowMs time.Time) (readyPartitions []int, nextReadyCheckDelayMs int) {
	ra.mu.Lock()
	defer ra.mu.Unlock()

	nextDelay := ra.lingerMs
	for partition, deque := range ra.batches {
		if len(deque) == 0 {
			continue
		}

		first := deque[0]
		waitedMs := int(nowMs.Sub(first.CreatedAt).Milliseconds())
		batchIsFull := first.IsFull() || len(deque) > 1
		lingerExpired := waitedMs >= ra.lingerMs

		if batchIsFull || lingerExpired {
			readyPartitions = append(readyPartitions, partition)
		} else {
			remaining := ra.lingerMs - waitedMs
			if remaining < nextDelay {
				nextDelay = remaining
			}
		}
	}
	return readyPartitions, nextDelay
}

// Drain은 전송 준비된 파티션에서 배치를 꺼낸다.
// RecordAccumulator.drain() 메서드에 대응한다.
func (ra *RecordAccumulator) Drain(partitions []int) []*ProducerBatch {
	ra.mu.Lock()
	defer ra.mu.Unlock()

	var drained []*ProducerBatch
	for _, p := range partitions {
		deque := ra.batches[p]
		if len(deque) > 0 {
			batch := deque[0]
			batch.Closed = true
			ra.batches[p] = deque[1:]
			drained = append(drained, batch)
		}
	}
	return drained
}

// SendResult는 전송 결과를 나타낸다.
type SendResult struct {
	Partition   int
	RecordCount int
	SizeBytes   int
	Latency     time.Duration
}

// Sender는 배경 스레드에서 배치를 전송하는 역할을 한다.
// Kafka의 Sender 클래스에 대응한다.
type Sender struct {
	accumulator *RecordAccumulator
	results     []SendResult
	mu          sync.Mutex
	running     bool
	wg          sync.WaitGroup
}

// NewSender는 새 Sender를 생성한다.
func NewSender(acc *RecordAccumulator) *Sender {
	return &Sender{
		accumulator: acc,
		running:     true,
	}
}

// Run은 Sender의 메인 루프를 실행한다.
// Sender.run() -> sendProducerData() 흐름에 대응한다.
func (s *Sender) Run() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for s.running {
			now := time.Now()
			readyPartitions, nextDelay := s.accumulator.Ready(now)

			if len(readyPartitions) > 0 {
				batches := s.accumulator.Drain(readyPartitions)
				for _, batch := range batches {
					// 네트워크 전송 시뮬레이션 (1-3ms)
					sendTime := time.Duration(1+rand.Intn(3)) * time.Millisecond
					time.Sleep(sendTime)

					result := SendResult{
						Partition:   batch.Partition,
						RecordCount: len(batch.Records),
						SizeBytes:   batch.SizeBytes,
						Latency:     time.Since(batch.CreatedAt),
					}

					s.mu.Lock()
					s.results = append(s.results, result)
					s.mu.Unlock()

					// 버퍼 풀에 메모리 반환
					s.accumulator.pool.Deallocate(batch.MaxSizeBytes)
				}
			} else {
				// 다음 배치가 준비될 때까지 대기 (최대 10ms 폴링)
				sleepMs := nextDelay
				if sleepMs > 10 {
					sleepMs = 10
				}
				if sleepMs <= 0 {
					sleepMs = 1
				}
				time.Sleep(time.Duration(sleepMs) * time.Millisecond)
			}
		}
	}()
}

// Stop은 Sender를 중지한다.
func (s *Sender) Stop() {
	s.running = false
	s.wg.Wait()
}

// GetResults는 전송 결과를 반환한다.
func (s *Sender) GetResults() []SendResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	results := make([]SendResult, len(s.results))
	copy(results, s.results)
	return results
}

func main() {
	fmt.Println("=============================================================")
	fmt.Println("  Kafka Producer RecordAccumulator Batching Simulation")
	fmt.Println("  Based on: RecordAccumulator.java, Sender.java")
	fmt.Println("=============================================================")

	// =========================================================================
	// 시나리오 1: 배치 크기에 의한 전송 (batch.size 트리거)
	// =========================================================================
	fmt.Println("\n--- 시나리오 1: batch.size에 의한 전송 트리거 ---")
	fmt.Println("설정: batch.size=500 bytes, linger.ms=5000 (5초)")
	fmt.Println("레코드가 배치 크기를 채우면 linger.ms를 기다리지 않고 즉시 전송\n")

	acc1 := NewRecordAccumulator(500, 5000, 10000)
	sender1 := NewSender(acc1)
	sender1.Run()

	// 큰 레코드를 빠르게 전송 -> batch.size 트리거
	for i := 0; i < 10; i++ {
		record := Record{
			Key:       fmt.Sprintf("key-%d", i),
			Value:     fmt.Sprintf("value-%s", strings.Repeat("x", 80)),
			Timestamp: time.Now(),
			SizeBytes: 100,
		}
		partition := i % 2
		batchFull, newBatch := acc1.Append(partition, record)
		fmt.Printf("  Append(P%d): key=%s, batchFull=%v, newBatch=%v\n",
			partition, record.Key, batchFull, newBatch)
	}

	time.Sleep(100 * time.Millisecond)
	sender1.Stop()

	results1 := sender1.GetResults()
	fmt.Printf("\n  전송된 배치 수: %d\n", len(results1))
	for _, r := range results1 {
		fmt.Printf("  -> P%d: %d records, %d bytes, latency=%v\n",
			r.Partition, r.RecordCount, r.SizeBytes, r.Latency.Round(time.Millisecond))
	}

	// =========================================================================
	// 시나리오 2: linger.ms에 의한 전송 (타임아웃 트리거)
	// =========================================================================
	fmt.Println("\n--- 시나리오 2: linger.ms에 의한 전송 트리거 ---")
	fmt.Println("설정: batch.size=10000 bytes, linger.ms=50ms")
	fmt.Println("배치가 가득 차지 않아도 linger.ms 후 전송\n")

	acc2 := NewRecordAccumulator(10000, 50, 50000)
	sender2 := NewSender(acc2)
	sender2.Run()

	// 작은 레코드를 소량 전송 -> linger.ms 트리거
	for i := 0; i < 3; i++ {
		record := Record{
			Key:       fmt.Sprintf("key-%d", i),
			Value:     fmt.Sprintf("small-value-%d", i),
			Timestamp: time.Now(),
			SizeBytes: 50,
		}
		acc2.Append(0, record)
		fmt.Printf("  Append(P0): key=%s (50 bytes, 배치 사용률: %.1f%%)\n",
			record.Key, float64(50*(i+1))/100.0)
	}

	fmt.Println("  ... linger.ms(50ms) 대기 중 ...")
	time.Sleep(200 * time.Millisecond)
	sender2.Stop()

	results2 := sender2.GetResults()
	fmt.Printf("\n  전송된 배치 수: %d\n", len(results2))
	for _, r := range results2 {
		fmt.Printf("  -> P%d: %d records, %d bytes, latency=%v\n",
			r.Partition, r.RecordCount, r.SizeBytes, r.Latency.Round(time.Millisecond))
	}

	// =========================================================================
	// 시나리오 3: 버퍼 풀 메모리 제한
	// =========================================================================
	fmt.Println("\n--- 시나리오 3: BufferPool 메모리 제한 ---")
	fmt.Println("설정: batch.size=200, buffer.memory=500")
	fmt.Println("메모리가 부족하면 producer.send()가 블로킹됨\n")

	pool := NewBufferPool(500)
	fmt.Printf("  초기 가용 메모리: %d bytes\n", pool.AvailableMemory())

	pool.Allocate(200)
	fmt.Printf("  200 bytes 할당 후: %d bytes 남음\n", pool.AvailableMemory())

	pool.Allocate(200)
	fmt.Printf("  200 bytes 추가 할당 후: %d bytes 남음\n", pool.AvailableMemory())

	// 비동기로 할당 시도 (블로킹될 것)
	done := make(chan bool)
	go func() {
		fmt.Println("  200 bytes 할당 시도 (메모리 부족, 블로킹 예상)...")
		pool.Allocate(200)
		fmt.Printf("  할당 성공! 가용 메모리: %d bytes\n", pool.AvailableMemory())
		done <- true
	}()

	time.Sleep(50 * time.Millisecond)
	fmt.Println("  200 bytes 해제 -> 블로킹된 할당이 진행됨")
	pool.Deallocate(200)
	<-done

	// =========================================================================
	// 시나리오 4: 처리량 vs 지연 트레이드오프
	// =========================================================================
	fmt.Println("\n--- 시나리오 4: linger.ms에 따른 처리량 vs 지연 트레이드오프 ---")
	fmt.Println("동일한 레코드를 linger.ms=0, 10, 100으로 각각 전송하여 비교\n")

	lingerValues := []int{0, 10, 100}
	recordCount := 50

	for _, linger := range lingerValues {
		acc := NewRecordAccumulator(200, linger, 50000)
		sender := NewSender(acc)
		sender.Run()

		start := time.Now()
		for i := 0; i < recordCount; i++ {
			record := Record{
				Key:       fmt.Sprintf("k%d", i),
				Value:     fmt.Sprintf("v%d", i),
				Timestamp: time.Now(),
				SizeBytes: 30,
			}
			acc.Append(i%3, record)
			time.Sleep(1 * time.Millisecond)
		}

		// 모든 배치가 전송될 때까지 대기
		time.Sleep(time.Duration(linger+100) * time.Millisecond)
		sender.Stop()

		results := sender.GetResults()
		elapsed := time.Since(start)

		totalRecords := 0
		totalBatches := len(results)
		var totalLatency time.Duration
		for _, r := range results {
			totalRecords += r.RecordCount
			totalLatency += r.Latency
		}

		avgLatency := time.Duration(0)
		avgBatchSize := 0.0
		if totalBatches > 0 {
			avgLatency = totalLatency / time.Duration(totalBatches)
			avgBatchSize = float64(totalRecords) / float64(totalBatches)
		}

		fmt.Printf("  linger.ms=%3d: 배치 수=%2d, 평균 배치 크기=%.1f records, 평균 지연=%v, 총 소요=%v\n",
			linger, totalBatches, avgBatchSize, avgLatency.Round(time.Millisecond), elapsed.Round(time.Millisecond))
	}

	// =========================================================================
	// 핵심 알고리즘 요약
	// =========================================================================
	fmt.Println("\n=============================================================")
	fmt.Println("  핵심 알고리즘 요약")
	fmt.Println("=============================================================")
	fmt.Println(`
  RecordAccumulator의 동작 흐름:

  1. append(partition, record):
     - 파티션별 Deque<ProducerBatch>에서 마지막 배치를 찾음
     - tryAppend() 시도 -> 성공하면 반환
     - 실패(가득 참)하면 BufferPool.allocate()로 메모리 할당
     - 새 ProducerBatch 생성 후 Deque에 추가

  2. Sender 스레드 루프:
     ready():  각 파티션의 첫 번째 배치를 검사
               - batch.isFull() OR deque.size() > 1 -> 즉시 전송
               - waitedTime >= lingerMs           -> 즉시 전송
     drain():  ready 파티션에서 첫 번째 배치를 꺼냄
     send():   drain된 배치를 브로커에 전송 후 BufferPool에 반환

  3. 트레이드오프:
     - linger.ms 증가 -> 더 큰 배치 -> 높은 처리량, 높은 지연
     - linger.ms 감소 -> 더 작은 배치 -> 낮은 처리량, 낮은 지연
     - batch.size 증가 -> 더 많은 레코드 축적 가능

  4. BufferPool (buffer.memory):
     - 총 메모리 제한 (기본값: 32MB)
     - 메모리 부족 시 producer.send()가 max.block.ms까지 블로킹
     - 배치 전송 완료 후 메모리 반환 -> 대기 중인 스레드 깨움
`)
}
