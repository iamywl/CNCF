package main

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"strings"
	"time"
)

// =============================================================================
// Kafka Sparse Offset Index Simulation
// Based on: OffsetIndex.java, AbstractIndex.java
//
// Kafka의 오프셋 인덱스는 메시지 오프셋을 물리적 파일 위치로 매핑하는 희소(sparse) 인덱스이다.
// 모든 메시지가 아닌 N바이트마다 하나의 엔트리만 저장하여 메모리 효율을 높인다.
//
// 인덱스 파일 형식:
//   - 고정 크기 8바이트 엔트리: 4바이트 상대 오프셋 + 4바이트 물리적 위치
//   - 상대 오프셋 = 절대 오프셋 - baseOffset (4바이트로 충분)
//   - mmap으로 메모리 매핑하여 빠른 조회
//
// 조회 알고리즘:
//   - 캐시 친화적 이진 검색 (warm section 우선)
//   - warmEntries = 8192/8 = 1024개 (마지막 ~1024개 엔트리)
//   - 대부분의 조회가 warm section에서 완료됨
// =============================================================================

const (
	// ENTRY_SIZE는 인덱스 엔트리 크기이다 (4바이트 오프셋 + 4바이트 위치).
	// OffsetIndex.ENTRY_SIZE = 8에 대응한다.
	ENTRY_SIZE = 8

	// WARM_ENTRIES는 캐시 친화적 검색을 위한 warm section 크기이다.
	// AbstractIndex.warmEntries() = 8192 / entrySize()에 대응한다.
	WARM_ENTRIES = 8192 / ENTRY_SIZE
)

// OffsetPosition은 오프셋과 물리적 위치의 쌍이다.
// OffsetPosition 레코드에 대응한다.
type OffsetPosition struct {
	Offset   int64
	Position int
}

func (op OffsetPosition) String() string {
	return fmt.Sprintf("(offset=%d, position=%d)", op.Offset, op.Position)
}

// OffsetIndex는 Kafka의 희소 오프셋 인덱스를 시뮬레이션한다.
// OffsetIndex.java에 대응한다.
type OffsetIndex struct {
	baseOffset int64
	// mmap 시뮬레이션: 고정 크기 바이트 슬라이스
	// 실제 Kafka는 MappedByteBuffer로 파일을 메모리 매핑한다.
	data       []byte
	entries    int
	maxEntries int
	lastOffset int64
}

// NewOffsetIndex는 새 오프셋 인덱스를 생성한다.
// 실제 Kafka에서는 파일을 mmap으로 매핑한다.
func NewOffsetIndex(baseOffset int64, maxIndexSize int) *OffsetIndex {
	// maxIndexSize를 ENTRY_SIZE의 배수로 내림
	roundedSize := (maxIndexSize / ENTRY_SIZE) * ENTRY_SIZE
	maxEntries := roundedSize / ENTRY_SIZE

	return &OffsetIndex{
		baseOffset: baseOffset,
		data:       make([]byte, roundedSize),
		entries:    0,
		maxEntries: maxEntries,
		lastOffset: baseOffset,
	}
}

// Append는 오프셋-위치 쌍을 인덱스에 추가한다.
// OffsetIndex.append(long offset, int position)에 대응한다.
func (idx *OffsetIndex) Append(offset int64, position int) error {
	if idx.entries >= idx.maxEntries {
		return fmt.Errorf("인덱스가 가득 참 (entries=%d)", idx.entries)
	}

	if idx.entries > 0 && offset <= idx.lastOffset {
		return fmt.Errorf("오프셋은 증가해야 함: %d <= %d", offset, idx.lastOffset)
	}

	// 상대 오프셋 계산 (4바이트로 충분)
	relativeOffset := int32(offset - idx.baseOffset)

	// mmap에 기록 (4바이트 상대 오프셋 + 4바이트 위치)
	pos := idx.entries * ENTRY_SIZE
	binary.BigEndian.PutUint32(idx.data[pos:pos+4], uint32(relativeOffset))
	binary.BigEndian.PutUint32(idx.data[pos+4:pos+8], uint32(position))

	idx.entries++
	idx.lastOffset = offset

	return nil
}

// parseEntry는 n번째 엔트리를 파싱한다.
// OffsetIndex.parseEntry(ByteBuffer buffer, int n)에 대응한다.
func (idx *OffsetIndex) parseEntry(n int) OffsetPosition {
	pos := n * ENTRY_SIZE
	relativeOffset := int32(binary.BigEndian.Uint32(idx.data[pos : pos+4]))
	physical := int(binary.BigEndian.Uint32(idx.data[pos+4 : pos+8]))
	return OffsetPosition{
		Offset:   idx.baseOffset + int64(relativeOffset),
		Position: physical,
	}
}

// Lookup은 주어진 targetOffset 이하의 가장 큰 오프셋과 그 위치를 반환한다.
// OffsetIndex.lookup(long targetOffset)에 대응한다.
// 캐시 친화적 이진 검색을 사용한다.
func (idx *OffsetIndex) Lookup(targetOffset int64) OffsetPosition {
	if idx.entries == 0 {
		return OffsetPosition{Offset: idx.baseOffset, Position: 0}
	}

	slot := idx.largestLowerBoundSlot(targetOffset)
	if slot == -1 {
		return OffsetPosition{Offset: idx.baseOffset, Position: 0}
	}
	return idx.parseEntry(slot)
}

// largestLowerBoundSlot은 targetOffset 이하의 가장 큰 엔트리의 슬롯을 찾는다.
// AbstractIndex.indexSlotRangeFor()에 대응한다.
// 캐시 친화적 이진 검색: warm section을 먼저 확인한다.
func (idx *OffsetIndex) largestLowerBoundSlot(targetOffset int64) int {
	if idx.entries == 0 {
		return -1
	}

	// warm section의 첫 엔트리 (캐시에 있을 가능성이 높은 영역)
	firstHotEntry := idx.entries - 1 - WARM_ENTRIES
	if firstHotEntry < 0 {
		firstHotEntry = 0
	}

	// 1. 타겟이 warm section에 있는지 확인
	hotEntry := idx.parseEntry(firstHotEntry)
	if hotEntry.Offset < targetOffset {
		// warm section에서 검색 (대부분의 in-sync 조회는 여기)
		return idx.binarySearch(targetOffset, firstHotEntry, idx.entries-1)
	}

	// 2. 타겟이 전체 인덱스의 첫 엔트리보다 작은지 확인
	firstEntry := idx.parseEntry(0)
	if firstEntry.Offset > targetOffset {
		return -1
	}

	// 3. cold section에서 검색
	return idx.binarySearch(targetOffset, 0, firstHotEntry)
}

// binarySearch는 [lo, hi] 범위에서 targetOffset 이하의 가장 큰 엔트리를 찾는다.
// AbstractIndex.binarySearch()에 대응한다.
func (idx *OffsetIndex) binarySearch(targetOffset int64, lo, hi int) int {
	for lo < hi {
		mid := (lo + hi + 1) / 2
		entry := idx.parseEntry(mid)
		if entry.Offset > targetOffset {
			hi = mid - 1
		} else if entry.Offset < targetOffset {
			lo = mid
		} else {
			return mid // 정확히 일치
		}
	}
	return lo
}

// SequentialScan은 순차 스캔으로 targetOffset 이하의 가장 큰 엔트리를 찾는다.
// 인덱스 조회와 성능을 비교하기 위한 용도이다.
func (idx *OffsetIndex) SequentialScan(targetOffset int64) OffsetPosition {
	result := OffsetPosition{Offset: idx.baseOffset, Position: 0}
	for i := 0; i < idx.entries; i++ {
		entry := idx.parseEntry(i)
		if entry.Offset <= targetOffset {
			result = entry
		} else {
			break
		}
	}
	return result
}

// Entry는 n번째 엔트리를 반환한다.
func (idx *OffsetIndex) Entry(n int) OffsetPosition {
	if n >= idx.entries {
		panic(fmt.Sprintf("엔트리 %d 요청, 총 %d개", n, idx.entries))
	}
	return idx.parseEntry(n)
}

// SizeInBytes는 인덱스의 실제 사용 크기를 반환한다.
func (idx *OffsetIndex) SizeInBytes() int {
	return idx.entries * ENTRY_SIZE
}

// buildTestLog는 테스트용 로그 세그먼트를 생성한다.
// 각 메시지의 위치는 가변 크기이다.
func buildTestLog(baseOffset int64, messageCount int) (offsets []int64, positions []int) {
	position := 0
	for i := 0; i < messageCount; i++ {
		offset := baseOffset + int64(i)
		offsets = append(offsets, offset)
		positions = append(positions, position)
		// 메시지 크기: 50~200 바이트 (가변)
		messageSize := 50 + rand.Intn(150)
		position += messageSize
	}
	return
}

func main() {
	fmt.Println("=============================================================")
	fmt.Println("  Kafka Sparse Offset Index Simulation")
	fmt.Println("  Based on: OffsetIndex.java, AbstractIndex.java")
	fmt.Println("=============================================================")

	// =========================================================================
	// 시나리오 1: 희소 인덱스 구축 및 조회
	// =========================================================================
	fmt.Println("\n--- 시나리오 1: 희소 인덱스 구축 ---")
	fmt.Println("N 바이트마다 하나의 인덱스 엔트리 생성 (모든 메시지를 인덱싱하지 않음)\n")

	baseOffset := int64(1000)
	indexIntervalBytes := 500 // index.interval.bytes 설정 (기본값: 4096)

	offsets, positions := buildTestLog(baseOffset, 100)

	// 인덱스 구축 (매 indexIntervalBytes마다)
	idx := NewOffsetIndex(baseOffset, 8*1024) // 8KB 인덱스 파일
	lastIndexedPosition := 0

	for i := 0; i < len(offsets); i++ {
		if positions[i]-lastIndexedPosition >= indexIntervalBytes {
			idx.Append(offsets[i], positions[i])
			lastIndexedPosition = positions[i]
		}
	}

	fmt.Printf("  세그먼트: baseOffset=%d, 총 메시지=%d\n", baseOffset, len(offsets))
	fmt.Printf("  index.interval.bytes=%d\n", indexIntervalBytes)
	fmt.Printf("  인덱스 엔트리 수: %d (전체 메시지의 %.1f%%만 인덱싱)\n",
		idx.entries, float64(idx.entries)/float64(len(offsets))*100)
	fmt.Printf("  인덱스 파일 크기: %d bytes (%d entries x %d bytes)\n",
		idx.SizeInBytes(), idx.entries, ENTRY_SIZE)

	fmt.Println("\n  인덱스 엔트리 (상대 오프셋 + 물리적 위치):")
	fmt.Println("  " + strings.Repeat("-", 50))
	fmt.Printf("  %-6s  %-12s  %-12s  %-10s\n", "Slot", "AbsOffset", "RelOffset", "Position")
	fmt.Println("  " + strings.Repeat("-", 50))
	for i := 0; i < idx.entries; i++ {
		entry := idx.Entry(i)
		relOffset := entry.Offset - baseOffset
		fmt.Printf("  %-6d  %-12d  %-12d  %-10d\n", i, entry.Offset, relOffset, entry.Position)
	}

	// =========================================================================
	// 시나리오 2: 이진 검색 조회
	// =========================================================================
	fmt.Println("\n--- 시나리오 2: 오프셋 조회 (이진 검색) ---")
	fmt.Println("targetOffset 이하의 가장 큰 인덱스 엔트리를 찾아 파일 위치 반환\n")

	testTargets := []int64{1000, 1010, 1025, 1050, 1075, 1099}
	for _, target := range testTargets {
		result := idx.Lookup(target)
		fmt.Printf("  Lookup(offset=%d) -> %s\n", target, result)
		fmt.Printf("    -> 이 위치부터 순차 스캔하여 정확한 offset=%d를 찾음\n", target)
	}

	fmt.Println("\n  * 인덱스가 희소(sparse)하므로 정확한 오프셋이 아닌")
	fmt.Println("    lower bound를 반환하고, 그 위치부터 순차 스캔이 필요함")

	// =========================================================================
	// 시나리오 3: mmap 스타일 고정 크기 엔트리
	// =========================================================================
	fmt.Println("\n--- 시나리오 3: 고정 크기 엔트리 포맷 ---")
	fmt.Println("각 엔트리: 4바이트 상대 오프셋 + 4바이트 위치 = 8바이트\n")

	demoIdx := NewOffsetIndex(5000, 256)
	demoIdx.Append(5000, 0)
	demoIdx.Append(5010, 1024)
	demoIdx.Append(5020, 2048)
	demoIdx.Append(5030, 3072)

	fmt.Println("  baseOffset = 5000")
	fmt.Println()
	fmt.Println("  바이트 레이아웃 (Big-Endian):")
	fmt.Println("  " + strings.Repeat("-", 60))
	fmt.Printf("  %-8s  %-20s  %-20s\n", "Slot", "상대 오프셋 (4B)", "위치 (4B)")
	fmt.Println("  " + strings.Repeat("-", 60))

	for i := 0; i < demoIdx.entries; i++ {
		pos := i * ENTRY_SIZE
		relOff := binary.BigEndian.Uint32(demoIdx.data[pos : pos+4])
		phys := binary.BigEndian.Uint32(demoIdx.data[pos+4 : pos+8])
		absOff := int64(relOff) + demoIdx.baseOffset

		fmt.Printf("  %-8d  0x%08X (rel=%d, abs=%d)  0x%08X (pos=%d)\n",
			i, relOff, relOff, absOff, phys, phys)
	}

	fmt.Println("\n  * 상대 오프셋을 사용하여 4바이트로 최대 2^31개 오프셋 표현")
	fmt.Printf("    (baseOffset %d ~ %d 범위 커버)\n", demoIdx.baseOffset,
		demoIdx.baseOffset+int64(^uint32(0)>>1))

	// =========================================================================
	// 시나리오 4: 캐시 친화적 이진 검색 (warm section)
	// =========================================================================
	fmt.Println("\n--- 시나리오 4: 캐시 친화적 이진 검색 ---")
	fmt.Println("AbstractIndex의 warm section 최적화\n")

	// 큰 인덱스를 만들어 warm section 효과를 시연
	bigBase := int64(0)
	bigIdx := NewOffsetIndex(bigBase, 100*1024) // 100KB (12800 entries)

	position := 0
	for i := 0; i < 10000; i++ {
		bigIdx.Append(bigBase+int64(i*10), position)
		position += 4096
	}

	fmt.Printf("  인덱스 엔트리 수: %d\n", bigIdx.entries)
	fmt.Printf("  WARM_ENTRIES: %d\n", WARM_ENTRIES)
	firstHot := bigIdx.entries - 1 - WARM_ENTRIES
	if firstHot < 0 {
		firstHot = 0
	}
	fmt.Printf("  Cold section: 엔트리 [0, %d)\n", firstHot)
	fmt.Printf("  Warm section: 엔트리 [%d, %d)\n", firstHot, bigIdx.entries)

	hotEntry := bigIdx.parseEntry(firstHot)
	fmt.Printf("  Warm section 시작 오프셋: %d\n", hotEntry.Offset)

	fmt.Println("\n  조회 성능 비교 (warm vs cold):")
	fmt.Println("  " + strings.Repeat("-", 65))
	fmt.Printf("  %-20s  %-12s  %-15s  %-15s\n", "Target", "Section", "BinarySearch", "SeqScan")
	fmt.Println("  " + strings.Repeat("-", 65))

	targets := []int64{
		bigBase + 99990, // warm section (마지막 근처)
		bigBase + 90000, // warm section
		bigBase + 50000, // cold section (중간)
		bigBase + 100,   // cold section (시작 근처)
	}

	for _, target := range targets {
		section := "warm"
		if target < hotEntry.Offset {
			section = "cold"
		}

		// 이진 검색 성능 측정
		start := time.Now()
		var result OffsetPosition
		for j := 0; j < 100000; j++ {
			result = bigIdx.Lookup(target)
		}
		bsTime := time.Since(start)

		// 순차 스캔 성능 측정
		start = time.Now()
		for j := 0; j < 100000; j++ {
			bigIdx.SequentialScan(target)
		}
		seqTime := time.Since(start)

		_ = result
		fmt.Printf("  offset=%-12d  %-12s  %-15v  %-15v\n",
			target, section, bsTime.Round(time.Microsecond), seqTime.Round(time.Microsecond))
	}

	fmt.Println("\n  * Warm section: 최근 엔트리 ~1024개, 페이지 캐시에 있을 확률 높음")
	fmt.Println("  * In-sync 팔로워/컨슈머의 조회는 대부분 warm section에서 완료")
	fmt.Println("  * 이진 검색은 O(log N), 순차 스캔은 O(N)")

	// =========================================================================
	// 시나리오 5: 실제 인덱스 조회 흐름
	// =========================================================================
	fmt.Println("\n--- 시나리오 5: 실제 메시지 조회 흐름 ---")
	fmt.Println("FetchRequest -> 인덱스 조회 -> 파일 읽기 -> 순차 스캔\n")

	fetchOffset := int64(1042)
	fmt.Printf("  FetchRequest: offset=%d\n", fetchOffset)
	fmt.Println()

	lookupResult := idx.Lookup(fetchOffset)
	fmt.Printf("  1단계: OffsetIndex.lookup(%d)\n", fetchOffset)
	fmt.Printf("    -> 결과: %s\n", lookupResult)
	fmt.Printf("    -> offset %d 이하의 가장 큰 인덱스 엔트리\n", fetchOffset)
	fmt.Println()

	fmt.Printf("  2단계: 파일 position=%d부터 순차 스캔\n", lookupResult.Position)
	fmt.Println("    -> 각 레코드의 오프셋을 확인하면서 읽기")
	scannedCount := 0
	for i, off := range offsets {
		if int64(positions[i]) >= int64(lookupResult.Position) && off <= fetchOffset {
			scannedCount++
		}
		if off == fetchOffset {
			fmt.Printf("    -> offset=%d 발견! (position=%d, %d개 레코드 스캔)\n",
				fetchOffset, positions[i], scannedCount)
			break
		}
	}

	fmt.Println()
	fmt.Println("  * 인덱스 없이는 세그먼트 시작부터 순차 스캔 필요 (느림)")
	fmt.Println("  * 희소 인덱스로 조회 시작점을 빠르게 찾아 순차 스캔 범위 최소화")

	// =========================================================================
	// 핵심 알고리즘 요약
	// =========================================================================
	fmt.Println("\n=============================================================")
	fmt.Println("  핵심 알고리즘 요약")
	fmt.Println("=============================================================")
	fmt.Println(`
  Kafka Offset Index 동작 원리:

  1. 인덱스 구조 (OffsetIndex.java):
     - 고정 크기 8바이트 엔트리: [4B 상대오프셋][4B 파일위치]
     - 상대 오프셋 = 절대 오프셋 - baseOffset
     - mmap으로 메모리 매핑 -> OS 페이지 캐시 활용
     - 모든 메시지가 아닌 index.interval.bytes마다 하나만 저장 (희소)

  2. 캐시 친화적 이진 검색 (AbstractIndex.java):
     warmEntries = 8192 / ENTRY_SIZE = 1024

     if target > index[end - warmEntries]:
       binarySearch(end - warmEntries, end)   // warm section
     else:
       binarySearch(0, end - warmEntries)      // cold section

     - warm section: 최근 ~1024개 엔트리 (3 페이지 이하)
     - 매 조회마다 warm section 페이지를 터치 -> LRU 캐시에 유지
     - cold section 페이지 폴트 방지

  3. 조회 흐름:
     FetchRequest(offset=N)
     -> OffsetIndex.lookup(N): lower bound 찾기
     -> LogSegment: 해당 위치부터 순차 스캔
     -> 정확한 오프셋 N의 메시지 반환

  4. 설정:
     - log.index.size.max.bytes: 인덱스 파일 최대 크기 (기본 10MB)
     - log.index.interval.bytes: 인덱스 엔트리 생성 간격 (기본 4096)
`)
}
