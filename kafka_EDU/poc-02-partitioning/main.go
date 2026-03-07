package main

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"sync/atomic"
)

// Kafka Partitioning PoC
//
// 실제 Kafka 소스 참조:
//   - BuiltInPartitioner.java: sticky 파티셔닝 (KIP-794)
//   - Utils.java: murmur2() 해시 함수
//
// Kafka의 파티셔닝 전략:
//   1) 키가 있는 경우: murmur2(key) % numPartitions -> 동일 키는 항상 같은 파티션
//   2) 키가 없는 경우 (round-robin, 구버전): 순차적으로 파티션 분배
//   3) 키가 없는 경우 (sticky, KIP-794): stickyBatchSize까지 같은 파티션에 전송 후 전환

const numPartitions = 6

// --- Murmur2 해시 함수 ---
// Kafka의 Utils.java에서 직접 가져온 murmur2 구현.
// seed = 0x9747b28c, m = 0x5bd1e995, r = 24
func murmur2(data []byte) int32 {
	length := len(data)
	seed := int32(-1756908916) // 0x9747b28c as signed int32
	m := int32(0x5bd1e995)
	r := uint32(24)

	h := seed ^ int32(length)
	length4 := length >> 2

	for i := 0; i < length4; i++ {
		i4 := i << 2
		k := int32(binary.LittleEndian.Uint32(data[i4 : i4+4]))
		k *= m
		k ^= int32(uint32(k) >> r)
		k *= m
		h *= m
		h ^= k
	}

	index := length4 << 2
	switch length - index {
	case 3:
		h ^= int32(data[index+2]&0xff) << 16
		fallthrough
	case 2:
		h ^= int32(data[index+1]&0xff) << 8
		fallthrough
	case 1:
		h ^= int32(data[index] & 0xff)
		h *= m
	}

	h ^= int32(uint32(h) >> 13)
	h *= m
	h ^= int32(uint32(h) >> 15)

	return h
}

// toPositive는 Kafka의 Utils.toPositive()를 구현한다.
// 음수를 0x7FFFFFFF로 마스킹하여 양수로 변환.
func toPositive(n int32) int32 {
	return n & 0x7FFFFFFF
}

// --- 전략 1: Hash-Based Partitioning ---
// BuiltInPartitioner.partitionForKey():
//   Utils.toPositive(Utils.murmur2(serializedKey)) % numPartitions
func hashPartition(key []byte, numPartitions int) int {
	return int(toPositive(murmur2(key))) % numPartitions
}

// --- 전략 2: Round-Robin Partitioning ---
// Kafka 2.3 이전의 기본 전략 (키가 없을 때)
type RoundRobinPartitioner struct {
	counter int32
}

func (p *RoundRobinPartitioner) partition(numPartitions int) int {
	next := atomic.AddInt32(&p.counter, 1)
	return int(toPositive(next)) % numPartitions
}

// --- 전략 3: Sticky Partitioning (KIP-794) ---
// BuiltInPartitioner의 핵심 알고리즘:
//   - stickyBatchSize만큼 하나의 파티션에 전송
//   - 임계값 도달 시 다음 파티션으로 전환
//   - adaptive: 파티션 큐 크기에 기반한 가중 확률 분배
type StickyPartitioner struct {
	stickyBatchSize   int
	currentPartition  int
	producedBytes     int
	partitionLoadStats *PartitionLoadStats
}

// PartitionLoadStats는 BuiltInPartitioner.PartitionLoadStats를 시뮬레이션한다.
// 큐 크기에 기반한 누적 빈도 테이블(CFT)을 유지한다.
type PartitionLoadStats struct {
	cumulativeFrequencyTable []int
	partitionIds             []int
	length                   int
}

func newStickyPartitioner(batchSize int) *StickyPartitioner {
	return &StickyPartitioner{
		stickyBatchSize:  batchSize,
		currentPartition: rand.Intn(numPartitions),
	}
}

// updatePartitionLoadStats는 BuiltInPartitioner.updatePartitionLoadStats()를 구현한다.
// Kafka 원본의 CFT 구축 알고리즘:
//   1) 큐 크기를 역전 (maxSize+1 - queueSize) -> 큐가 작은 파티션이 더 높은 빈도
//   2) 누적합(running sum)으로 변환
//   3) 무작위 값으로 이진 탐색하여 파티션 선택
func (sp *StickyPartitioner) updatePartitionLoadStats(queueSizes []int, partitionIds []int) {
	length := len(queueSizes)
	if length < 2 {
		sp.partitionLoadStats = nil
		return
	}

	// 최대 큐 크기 + 1 계산
	maxSizePlus1 := queueSizes[0]
	allEqual := true
	for i := 1; i < length; i++ {
		if queueSizes[i] != maxSizePlus1 {
			allEqual = false
		}
		if queueSizes[i] > maxSizePlus1 {
			maxSizePlus1 = queueSizes[i]
		}
	}
	maxSizePlus1++

	if allEqual {
		sp.partitionLoadStats = nil
		return
	}

	// 역전 + 누적합 (Kafka의 CFT 구축)
	// 원본: queueSizes[0] = maxSizePlus1 - queueSizes[0]
	//        queueSizes[i] = maxSizePlus1 - queueSizes[i] + queueSizes[i-1]
	cft := make([]int, length)
	cft[0] = maxSizePlus1 - queueSizes[0]
	for i := 1; i < length; i++ {
		cft[i] = maxSizePlus1 - queueSizes[i] + cft[i-1]
	}

	ids := make([]int, length)
	copy(ids, partitionIds)

	sp.partitionLoadStats = &PartitionLoadStats{
		cumulativeFrequencyTable: cft,
		partitionIds:             ids,
		length:                   length,
	}
}

// nextPartition은 BuiltInPartitioner.nextPartition()을 구현한다.
func (sp *StickyPartitioner) nextPartition() int {
	random := int(toPositive(int32(rand.Int31())))

	if sp.partitionLoadStats == nil {
		return random % numPartitions
	}

	stats := sp.partitionLoadStats
	cft := stats.cumulativeFrequencyTable
	weightedRandom := random % cft[stats.length-1]

	// 이진 탐색으로 CFT에서 파티션 찾기
	lo, hi := 0, stats.length-1
	for lo < hi {
		mid := (lo + hi) / 2
		if cft[mid] <= weightedRandom {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return stats.partitionIds[lo]
}

// partition은 레코드를 파티셔닝한다. 바이트 수에 기반한 sticky 전환 포함.
// Kafka 원본: updatePartitionInfo()
//   if (producedBytes >= stickyBatchSize && enableSwitch || producedBytes >= stickyBatchSize * 2)
//       switch partition
func (sp *StickyPartitioner) partition(recordSize int) int {
	partition := sp.currentPartition
	sp.producedBytes += recordSize

	if sp.producedBytes >= sp.stickyBatchSize {
		sp.currentPartition = sp.nextPartition()
		sp.producedBytes = 0
	}

	return partition
}

// --- 분포 분석 헬퍼 ---
func printDistribution(name string, counts []int, total int) {
	fmt.Printf("\n  [%s] 분포 (total=%d):\n", name, total)
	maxCount := 0
	for _, c := range counts {
		if c > maxCount {
			maxCount = c
		}
	}
	for i, c := range counts {
		pct := float64(c) / float64(total) * 100
		barLen := int(float64(c) / float64(maxCount) * 30)
		bar := ""
		for j := 0; j < barLen; j++ {
			bar += "#"
		}
		fmt.Printf("    P%d: %5d (%5.1f%%) %s\n", i, c, pct, bar)
	}

	// 표준편차 계산
	mean := float64(total) / float64(len(counts))
	sumSq := 0.0
	for _, c := range counts {
		diff := float64(c) - mean
		sumSq += diff * diff
	}
	stddev := math.Sqrt(sumSq / float64(len(counts)))
	fmt.Printf("    평균=%.1f, 표준편차=%.1f, CV=%.2f%%\n", mean, stddev, stddev/mean*100)
}

func main() {
	fmt.Println("========================================")
	fmt.Println(" Kafka Partitioning PoC")
	fmt.Println(" Based on: BuiltInPartitioner.java, Utils.murmur2()")
	fmt.Println("========================================")

	// --- 1. Murmur2 해시 파티셔닝 (키 있는 경우) ---
	fmt.Println("\n[1] Murmur2 해시 파티셔닝 (키 있는 경우)")
	fmt.Println("    Utils.toPositive(Utils.murmur2(key)) %% numPartitions")
	fmt.Println()

	hashCounts := make([]int, numPartitions)
	totalHash := 10000
	for i := 0; i < totalHash; i++ {
		key := []byte(fmt.Sprintf("user-%d", i))
		p := hashPartition(key, numPartitions)
		hashCounts[p]++
	}
	printDistribution("Hash", hashCounts, totalHash)

	// 동일 키 -> 동일 파티션 확인
	fmt.Println("\n  동일 키 일관성 확인:")
	keys := []string{"user-alice", "order-12345", "sensor-temp-01", "session-abc"}
	for _, k := range keys {
		p := hashPartition([]byte(k), numPartitions)
		p2 := hashPartition([]byte(k), numPartitions)
		fmt.Printf("    key=%q -> partition=%d (재호출=%d, 일치=%v)\n", k, p, p2, p == p2)
	}

	// --- 2. Round-Robin 파티셔닝 (키 없는 경우, 구버전) ---
	fmt.Println("\n[2] Round-Robin 파티셔닝 (키 없는 경우)")
	fmt.Println("    Kafka 2.3 이전의 기본 전략")

	rrPartitioner := &RoundRobinPartitioner{}
	rrCounts := make([]int, numPartitions)
	totalRR := 10000
	for i := 0; i < totalRR; i++ {
		p := rrPartitioner.partition(numPartitions)
		rrCounts[p]++
	}
	printDistribution("Round-Robin", rrCounts, totalRR)

	// --- 3. Sticky 파티셔닝 (KIP-794) ---
	fmt.Println("\n[3] Sticky 파티셔닝 (KIP-794)")
	fmt.Println("    stickyBatchSize=1000 bytes, 한 파티션에 배치 크기만큼 보낸 후 전환")

	stickyPartitioner := newStickyPartitioner(1000)
	stickyCounts := make([]int, numPartitions)
	totalSticky := 10000
	recordSize := 100 // 레코드 크기 100B

	// sticky 패턴 추적
	prevPartition := -1
	switchCount := 0
	samePartitionRun := 0
	maxRun := 0

	for i := 0; i < totalSticky; i++ {
		p := stickyPartitioner.partition(recordSize)
		stickyCounts[p]++

		if p == prevPartition {
			samePartitionRun++
		} else {
			if samePartitionRun > maxRun {
				maxRun = samePartitionRun
			}
			samePartitionRun = 1
			if prevPartition >= 0 {
				switchCount++
			}
		}
		prevPartition = p
	}
	printDistribution("Sticky", stickyCounts, totalSticky)
	fmt.Printf("\n  Sticky 전환 분석:\n")
	fmt.Printf("    총 파티션 전환 횟수: %d (round-robin이면 %d)\n", switchCount, totalSticky-1)
	fmt.Printf("    최대 연속 동일 파티션: %d 레코드 (stickyBatchSize/recordSize=%d)\n",
		maxRun, 1000/recordSize)
	fmt.Printf("    배치 효율: round-robin 대비 %.0fx 적은 전환\n",
		float64(totalSticky-1)/float64(switchCount))

	// --- 4. Adaptive Sticky (큐 기반 가중치) ---
	fmt.Println("\n[4] Adaptive Sticky 파티셔닝 (큐 로드 기반)")
	fmt.Println("    BuiltInPartitioner.updatePartitionLoadStats() CFT 알고리즘")

	adaptivePartitioner := newStickyPartitioner(500)

	// 큐 크기: 파티션 0,1은 혼잡(큐=10), 나머지는 여유(큐=1)
	queueSizes := []int{10, 10, 1, 1, 1, 1}
	partitionIds := []int{0, 1, 2, 3, 4, 5}
	fmt.Printf("\n  큐 크기: %v\n", queueSizes)

	adaptivePartitioner.updatePartitionLoadStats(queueSizes, partitionIds)

	// CFT 출력
	if adaptivePartitioner.partitionLoadStats != nil {
		stats := adaptivePartitioner.partitionLoadStats
		fmt.Printf("  CFT 구축 과정:\n")
		fmt.Printf("    maxSizePlus1 = %d\n", 11)
		fmt.Printf("    역전: [%d, %d, %d, %d, %d, %d]\n",
			11-10, 11-10, 11-1, 11-1, 11-1, 11-1)
		fmt.Printf("    누적합(CFT): %v\n", stats.cumulativeFrequencyTable)
		fmt.Printf("    -> 큐가 작은 파티션(2~5)이 더 높은 확률로 선택됨\n")
	}

	adaptiveCounts := make([]int, numPartitions)
	totalAdaptive := 10000
	for i := 0; i < totalAdaptive; i++ {
		p := adaptivePartitioner.partition(recordSize)
		adaptiveCounts[p]++
	}
	printDistribution("Adaptive", adaptiveCounts, totalAdaptive)

	// --- 5. 설계 비교 ---
	fmt.Println("\n[5] 파티셔닝 전략 비교")
	fmt.Println("  +-------------------+------------------+------------------+")
	fmt.Println("  | 전략              | 장점             | 단점             |")
	fmt.Println("  +-------------------+------------------+------------------+")
	fmt.Println("  | Hash (키 있음)    | 키 기반 순서 보장 | 핫스팟 가능      |")
	fmt.Println("  | Round-Robin       | 완벽한 균등 분배  | 배치 비효율      |")
	fmt.Println("  | Sticky (KIP-794)  | 배치 효율 극대화  | 단기 불균형      |")
	fmt.Println("  | Adaptive Sticky   | 로드 인식 분배    | 복잡도 증가      |")
	fmt.Println("  +-------------------+------------------+------------------+")
}
