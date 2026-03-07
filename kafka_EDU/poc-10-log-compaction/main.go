package main

import (
	"fmt"
	"hash/fnv"
	"strings"
	"time"
)

// =============================================================================
// Kafka Log Compaction Simulation
// Based on: Cleaner.java, OffsetMap concept
//
// Kafka의 로그 컴팩션은 키 기반 중복 제거(deduplication)를 수행한다.
// 동일한 키의 레코드 중 가장 최신(offset이 가장 큰) 값만 유지하고 나머지를 제거한다.
// Tombstone(value=null)은 삭제 마커로 사용되며, delete.retention.ms 이후 제거된다.
//
// 핵심 흐름:
//   1. buildOffsetMap: dirty 세그먼트를 스캔하여 key -> latest offset 매핑 구축
//   2. cleanInto: 세그먼트를 스캔하면서 OffsetMap을 참조하여 최신 값만 유지
//   3. Tombstone 처리: value=null인 레코드는 delete horizon 이전이면 제거
// =============================================================================

// LogRecord는 Kafka 로그의 개별 레코드를 나타낸다.
type LogRecord struct {
	Offset    int64
	Key       string
	Value     *string // nil = tombstone (삭제 마커)
	Timestamp time.Time
}

// IsTombstone은 이 레코드가 삭제 마커인지 확인한다.
func (r *LogRecord) IsTombstone() bool {
	return r.Value == nil
}

// String은 레코드의 문자열 표현을 반환한다.
func (r *LogRecord) String() string {
	if r.IsTombstone() {
		return fmt.Sprintf("[offset=%d] key=%s value=<TOMBSTONE>", r.Offset, r.Key)
	}
	return fmt.Sprintf("[offset=%d] key=%s value=%s", r.Offset, r.Key, *r.Value)
}

// LogSegment는 Kafka 로그 세그먼트를 나타낸다.
type LogSegment struct {
	BaseOffset   int64
	Records      []LogRecord
	LastModified time.Time
}

// OffsetMap은 key -> latest offset 매핑을 해시 기반으로 저장한다.
// Kafka의 OffsetMap (SkimpyOffsetMap) 클래스에 대응한다.
// 메모리 효율을 위해 키의 해시값을 사용하며, 고정 크기 슬롯을 사용한다.
type OffsetMap struct {
	slots        int
	hashes       []uint64 // 키 해시 저장
	offsets      []int64  // 최신 오프셋 저장
	occupied     []bool   // 슬롯 사용 여부
	size         int      // 현재 저장된 항목 수
	latestOffset int64    // 가장 큰 오프셋
}

// NewOffsetMap은 지정된 슬롯 수의 OffsetMap을 생성한다.
func NewOffsetMap(slots int) *OffsetMap {
	return &OffsetMap{
		slots:        slots,
		hashes:       make([]uint64, slots),
		offsets:      make([]int64, slots),
		occupied:     make([]bool, slots),
		latestOffset: -1,
	}
}

// hashKey는 키의 해시값을 계산한다.
func hashKey(key string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(key))
	return h.Sum64()
}

// Put은 키의 최신 오프셋을 저장한다.
// 선형 탐색(linear probing) 방식의 오픈 어드레싱을 사용한다.
func (om *OffsetMap) Put(key string, offset int64) bool {
	hash := hashKey(key)
	slot := int(hash % uint64(om.slots))

	for i := 0; i < om.slots; i++ {
		idx := (slot + i) % om.slots
		if !om.occupied[idx] {
			// 빈 슬롯에 삽입
			om.hashes[idx] = hash
			om.offsets[idx] = offset
			om.occupied[idx] = true
			om.size++
			if offset > om.latestOffset {
				om.latestOffset = offset
			}
			return true
		}
		if om.hashes[idx] == hash {
			// 같은 키 → 오프셋 업데이트 (더 큰 값으로)
			if offset > om.offsets[idx] {
				om.offsets[idx] = offset
			}
			if offset > om.latestOffset {
				om.latestOffset = offset
			}
			return true
		}
	}
	return false // 맵이 가득 참
}

// Get은 키의 최신 오프셋을 조회한다. 없으면 -1을 반환한다.
func (om *OffsetMap) Get(key string) int64 {
	hash := hashKey(key)
	slot := int(hash % uint64(om.slots))

	for i := 0; i < om.slots; i++ {
		idx := (slot + i) % om.slots
		if !om.occupied[idx] {
			return -1
		}
		if om.hashes[idx] == hash {
			return om.offsets[idx]
		}
	}
	return -1
}

// Clear는 맵을 초기화한다.
func (om *OffsetMap) Clear() {
	for i := range om.occupied {
		om.occupied[i] = false
	}
	om.size = 0
	om.latestOffset = -1
}

// Utilization은 슬롯 사용률을 반환한다.
func (om *OffsetMap) Utilization() float64 {
	return float64(om.size) / float64(om.slots)
}

// Cleaner는 로그 컴팩션을 수행하는 구조체이다.
// Kafka의 Cleaner 클래스에 대응한다.
type Cleaner struct {
	offsetMap          *OffsetMap
	deleteRetentionMs  int64
	dupBufferLoadFactor float64
}

// NewCleaner는 새 Cleaner를 생성한다.
func NewCleaner(mapSlots int, deleteRetentionMs int64) *Cleaner {
	return &Cleaner{
		offsetMap:          NewOffsetMap(mapSlots),
		deleteRetentionMs:  deleteRetentionMs,
		dupBufferLoadFactor: 0.75,
	}
}

// CleanStats는 컴팩션 통계를 저장한다.
type CleanStats struct {
	MessagesRead     int
	MessagesRetained int
	TombstonesRemoved int
	DuplicatesRemoved int
}

// BuildOffsetMap은 dirty 세그먼트를 스캔하여 key -> latest offset 매핑을 구축한다.
// Cleaner.buildOffsetMap() 메서드에 대응한다.
func (c *Cleaner) BuildOffsetMap(segments []LogSegment, startOffset, endOffset int64) {
	c.offsetMap.Clear()

	for _, seg := range segments {
		for _, record := range seg.Records {
			if record.Offset >= startOffset && record.Offset < endOffset {
				if record.Key != "" {
					c.offsetMap.Put(record.Key, record.Offset)
				}
			}
		}
	}
}

// CleanSegments는 세그먼트를 컴팩션하여 최신 값만 유지한 새 세그먼트를 반환한다.
// Cleaner.cleanInto() 메서드의 shouldRetainRecord 로직에 대응한다.
func (c *Cleaner) CleanSegments(segments []LogSegment, currentTime time.Time) ([]LogRecord, CleanStats) {
	var cleaned []LogRecord
	var stats CleanStats

	deleteHorizon := currentTime.Add(-time.Duration(c.deleteRetentionMs) * time.Millisecond)

	for _, seg := range segments {
		for _, record := range seg.Records {
			stats.MessagesRead++

			if record.Key == "" {
				// 키가 없는 레코드는 무효 (Kafka 컴팩션은 키가 필수)
				continue
			}

			// 이 키의 최신 오프셋을 조회
			latestOffset := c.offsetMap.Get(record.Key)

			// 이 레코드가 해당 키의 최신 오프셋인지 확인
			isLatestForKey := record.Offset >= latestOffset

			if !isLatestForKey {
				// 최신이 아닌 레코드는 제거
				stats.DuplicatesRemoved++
				continue
			}

			// Tombstone 처리
			if record.IsTombstone() {
				// delete.retention.ms 이전의 tombstone만 제거
				if record.Timestamp.Before(deleteHorizon) {
					stats.TombstonesRemoved++
					continue
				}
				// 아직 유지해야 할 tombstone
			}

			cleaned = append(cleaned, record)
			stats.MessagesRetained++
		}
	}

	return cleaned, stats
}

func strPtr(s string) *string {
	return &s
}

func main() {
	fmt.Println("=============================================================")
	fmt.Println("  Kafka Log Compaction Simulation")
	fmt.Println("  Based on: Cleaner.java, OffsetMap concept")
	fmt.Println("=============================================================")

	// =========================================================================
	// 시나리오 1: 기본 로그 컴팩션 (key 기반 중복 제거)
	// =========================================================================
	fmt.Println("\n--- 시나리오 1: 기본 로그 컴팩션 ---")
	fmt.Println("동일 키의 여러 버전 중 최신 값만 유지\n")

	now := time.Now()
	segment1 := LogSegment{
		BaseOffset:   0,
		LastModified: now.Add(-2 * time.Hour),
		Records: []LogRecord{
			{Offset: 0, Key: "user-1", Value: strPtr("Alice-v1"), Timestamp: now.Add(-2 * time.Hour)},
			{Offset: 1, Key: "user-2", Value: strPtr("Bob-v1"), Timestamp: now.Add(-2 * time.Hour)},
			{Offset: 2, Key: "user-1", Value: strPtr("Alice-v2"), Timestamp: now.Add(-90 * time.Minute)},
			{Offset: 3, Key: "user-3", Value: strPtr("Charlie-v1"), Timestamp: now.Add(-80 * time.Minute)},
			{Offset: 4, Key: "user-2", Value: strPtr("Bob-v2"), Timestamp: now.Add(-70 * time.Minute)},
		},
	}

	segment2 := LogSegment{
		BaseOffset:   5,
		LastModified: now.Add(-1 * time.Hour),
		Records: []LogRecord{
			{Offset: 5, Key: "user-1", Value: strPtr("Alice-v3"), Timestamp: now.Add(-60 * time.Minute)},
			{Offset: 6, Key: "user-3", Value: strPtr("Charlie-v2"), Timestamp: now.Add(-50 * time.Minute)},
			{Offset: 7, Key: "user-4", Value: strPtr("Dave-v1"), Timestamp: now.Add(-40 * time.Minute)},
			{Offset: 8, Key: "user-2", Value: strPtr("Bob-v3"), Timestamp: now.Add(-30 * time.Minute)},
		},
	}

	allSegments := []LogSegment{segment1, segment2}

	fmt.Println("  [컴팩션 전] 전체 레코드:")
	for _, seg := range allSegments {
		for _, r := range seg.Records {
			fmt.Printf("    %s\n", r.String())
		}
	}

	cleaner := NewCleaner(64, 3600000) // 64 슬롯, delete.retention.ms=1시간

	// 1단계: OffsetMap 구축
	fmt.Println("\n  [1단계] OffsetMap 구축 (key -> latest offset):")
	cleaner.BuildOffsetMap(allSegments, 0, 9)
	keys := []string{"user-1", "user-2", "user-3", "user-4"}
	for _, k := range keys {
		offset := cleaner.offsetMap.Get(k)
		fmt.Printf("    %s -> offset %d\n", k, offset)
	}
	fmt.Printf("    OffsetMap 사용률: %.1f%% (%d/%d 슬롯)\n",
		cleaner.offsetMap.Utilization()*100, cleaner.offsetMap.size, cleaner.offsetMap.slots)

	// 2단계: 컴팩션 수행
	cleaned, stats := cleaner.CleanSegments(allSegments, now)

	fmt.Println("\n  [2단계] 컴팩션 수행:")
	fmt.Printf("    읽은 레코드: %d\n", stats.MessagesRead)
	fmt.Printf("    유지된 레코드: %d\n", stats.MessagesRetained)
	fmt.Printf("    제거된 중복: %d\n", stats.DuplicatesRemoved)

	fmt.Println("\n  [컴팩션 후] 유지된 레코드:")
	for _, r := range cleaned {
		fmt.Printf("    %s\n", r.String())
	}

	// =========================================================================
	// 시나리오 2: Tombstone 처리 (삭제 마커)
	// =========================================================================
	fmt.Println("\n--- 시나리오 2: Tombstone 처리 ---")
	fmt.Println("value=null (tombstone)은 키 삭제를 나타냄")
	fmt.Println("delete.retention.ms 이내의 tombstone은 유지, 이후는 제거\n")

	tombstoneSegment := LogSegment{
		BaseOffset:   0,
		LastModified: now.Add(-3 * time.Hour),
		Records: []LogRecord{
			{Offset: 0, Key: "config-a", Value: strPtr("value-1"), Timestamp: now.Add(-5 * time.Hour)},
			{Offset: 1, Key: "config-b", Value: strPtr("value-2"), Timestamp: now.Add(-4 * time.Hour)},
			// config-a 삭제 (오래된 tombstone -> 제거 대상)
			{Offset: 2, Key: "config-a", Value: nil, Timestamp: now.Add(-3 * time.Hour)},
			{Offset: 3, Key: "config-c", Value: strPtr("value-3"), Timestamp: now.Add(-2 * time.Hour)},
			// config-b 삭제 (최근 tombstone -> 유지)
			{Offset: 4, Key: "config-b", Value: nil, Timestamp: now.Add(-30 * time.Minute)},
			{Offset: 5, Key: "config-d", Value: strPtr("value-4"), Timestamp: now.Add(-10 * time.Minute)},
		},
	}

	fmt.Println("  [컴팩션 전]:")
	for _, r := range tombstoneSegment.Records {
		fmt.Printf("    %s\n", r.String())
	}

	// delete.retention.ms = 1시간 -> 1시간 이전의 tombstone만 제거
	cleaner2 := NewCleaner(32, 3600000)
	cleaner2.BuildOffsetMap([]LogSegment{tombstoneSegment}, 0, 6)
	cleaned2, stats2 := cleaner2.CleanSegments([]LogSegment{tombstoneSegment}, now)

	fmt.Println("\n  [컴팩션 후] (delete.retention.ms=1시간):")
	for _, r := range cleaned2 {
		fmt.Printf("    %s\n", r.String())
	}
	fmt.Printf("\n    삭제된 tombstone: %d (delete horizon 이전)\n", stats2.TombstonesRemoved)
	fmt.Printf("    유지된 tombstone: config-b (아직 delete.retention.ms 이내)\n")

	// =========================================================================
	// 시나리오 3: OffsetMap 충돌과 용량
	// =========================================================================
	fmt.Println("\n--- 시나리오 3: OffsetMap 해시 기반 저장 ---")
	fmt.Println("메모리 효율을 위해 키의 해시값만 저장 (SkimpyOffsetMap 패턴)\n")

	smallMap := NewOffsetMap(8) // 아주 작은 맵

	testKeys := []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta"}
	for i, k := range testKeys {
		ok := smallMap.Put(k, int64(i*10))
		fmt.Printf("  Put(%s, offset=%d): success=%v\n", k, i*10, ok)
	}

	fmt.Printf("\n  OffsetMap 용량: %d/%d (사용률: %.1f%%)\n",
		smallMap.size, smallMap.slots, smallMap.Utilization()*100)

	fmt.Println("\n  조회 결과:")
	for _, k := range testKeys {
		offset := smallMap.Get(k)
		fmt.Printf("    Get(%s) -> offset=%d\n", k, offset)
	}

	// 같은 키 업데이트
	fmt.Println("\n  동일 키 업데이트:")
	smallMap.Put("alpha", 100)
	smallMap.Put("beta", 200)
	fmt.Printf("    Get(alpha) -> offset=%d (10->100으로 업데이트)\n", smallMap.Get("alpha"))
	fmt.Printf("    Get(beta) -> offset=%d (10->200으로 업데이트)\n", smallMap.Get("beta"))
	fmt.Printf("    OffsetMap 크기 변화 없음: %d (기존 슬롯 재사용)\n", smallMap.size)

	// =========================================================================
	// 시나리오 4: 컴팩션 전후 공간 절약
	// =========================================================================
	fmt.Println("\n--- 시나리오 4: 컴팩션 전후 공간 비교 ---")

	var largeRecords []LogRecord
	offset := int64(0)
	// 100개 키에 대해 각각 10번 업데이트 (총 1000 레코드)
	for round := 0; round < 10; round++ {
		for i := 0; i < 100; i++ {
			key := fmt.Sprintf("sensor-%03d", i)
			value := fmt.Sprintf("reading-round%d-%s", round, strings.Repeat(".", 50))
			largeRecords = append(largeRecords, LogRecord{
				Offset:    offset,
				Key:       key,
				Value:     strPtr(value),
				Timestamp: now.Add(-time.Duration(1000-offset) * time.Minute),
			})
			offset++
		}
	}

	largeSeg := LogSegment{
		BaseOffset:   0,
		LastModified: now,
		Records:      largeRecords,
	}

	cleaner3 := NewCleaner(256, 3600000)
	cleaner3.BuildOffsetMap([]LogSegment{largeSeg}, 0, offset)
	cleaned3, stats3 := cleaner3.CleanSegments([]LogSegment{largeSeg}, now)

	beforeSize := len(largeRecords)
	afterSize := len(cleaned3)
	reductionPct := float64(beforeSize-afterSize) / float64(beforeSize) * 100

	fmt.Printf("  컴팩션 전 레코드 수: %d\n", beforeSize)
	fmt.Printf("  컴팩션 후 레코드 수: %d\n", afterSize)
	fmt.Printf("  제거된 중복: %d\n", stats3.DuplicatesRemoved)
	fmt.Printf("  공간 절약률: %.1f%%\n", reductionPct)
	fmt.Printf("  OffsetMap 사용률: %.1f%%\n", cleaner3.offsetMap.Utilization()*100)

	// =========================================================================
	// 핵심 알고리즘 요약
	// =========================================================================
	fmt.Println("\n=============================================================")
	fmt.Println("  핵심 알고리즘 요약")
	fmt.Println("=============================================================")
	fmt.Println(`
  Log Compaction 동작 흐름 (Cleaner.doClean):

  1. buildOffsetMap(dirty segments):
     - dirty 세그먼트를 순회하며 key의 해시 -> latest offset 매핑 구축
     - OffsetMap은 메모리 효율을 위해 키 자체가 아닌 해시값만 저장
     - map.slots * dupBufferLoadFactor까지만 채움

  2. cleanInto(source, dest, offsetMap):
     각 레코드에 대해 shouldRetainRecord() 호출:
     - record.offset > map.latestOffset -> 유지 (아직 맵에 없는 새 레코드)
     - record.offset >= map.get(key) -> 최신 값, 유지
     - record.offset < map.get(key) -> 이전 값, 제거
     - tombstone(value=null):
       * deleteHorizon 이전 -> 제거
       * deleteHorizon 이후 -> 유지 (아직 컨슈머가 읽어야 할 수 있음)

  3. Tombstone 라이프사이클:
     produce(key, null) -> tombstone 생성
     -> delete.retention.ms 동안 유지 (컨슈머가 삭제 인지 가능)
     -> 이후 컴팩션에서 제거

  4. 설정:
     - log.cleanup.policy=compact (토픽 레벨)
     - log.cleaner.dedupe.buffer.size: OffsetMap 크기
     - delete.retention.ms: tombstone 보존 기간
     - min.compaction.lag.ms: 최소 컴팩션 지연
`)
}
