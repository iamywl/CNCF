package main

import (
	"fmt"
	"strings"
	"time"
)

// =============================================================================
// Loki MemChunk(인메모리 청크) 시뮬레이션
// =============================================================================
//
// MemChunk는 Loki Ingester에서 로그 데이터를 메모리에 보관하는 핵심 자료구조이다.
// 로그 스트림은 여러 개의 청크로 분할되며, 각 청크는 시간순으로 정렬된 로그 엔트리를 담는다.
//
// MemChunk의 핵심 특성:
//   1. Append-only: 시간순으로만 추가 가능 (out-of-order는 별도 처리)
//   2. Head Block + Blocks: 현재 쓰기 중인 head block과 이미 sealed된 block들
//   3. Iterator 패턴: Forward/Backward 반복자로 읽기 접근
//   4. Time Range: Bounds()로 청크의 시간 범위를 O(1)로 확인
//   5. Size 관리: 청크 크기가 임계값에 도달하면 새 청크로 교체
//
// Loki 실제 구현 참조:
//   - pkg/chunkenc/interface.go: Chunk 인터페이스 정의
//   - pkg/chunkenc/memchunk.go: MemChunk 구현
//   - pkg/chunkenc/memchunk_test.go: 테스트 케이스
//   - pkg/ingester/stream.go: 스트림에서 MemChunk 사용
// =============================================================================

// Entry는 하나의 로그 엔트리이다.
type Entry struct {
	Timestamp time.Time
	Line      string
}

// IterDirection은 반복자의 방향이다.
type IterDirection int

const (
	Forward  IterDirection = iota // 시간순 (오래된 것부터)
	Backward                      // 역시간순 (최신 것부터)
)

// EntryIterator는 로그 엔트리를 순회하는 반복자 인터페이스이다.
// Loki의 iter.EntryIterator와 동일한 패턴이다.
type EntryIterator interface {
	// Next는 다음 엔트리로 이동한다. 더 이상 엔트리가 없으면 false를 반환한다.
	Next() bool
	// Entry는 현재 엔트리를 반환한다.
	Entry() Entry
	// Close는 반복자를 닫는다.
	Close()
	// Err는 반복 중 발생한 에러를 반환한다.
	Err() error
}

// Block은 sealed된 블록이다 (읽기 전용).
// 실제 Loki에서는 gzip 압축되지만, 이 PoC에서는 반복자 패턴에 집중한다.
type Block struct {
	entries []Entry
	mint    int64 // 최소 타임스탬프 (나노초)
	maxt    int64 // 최대 타임스탬프 (나노초)
}

// HeadBlock은 현재 쓰기 중인 블록이다.
type HeadBlock struct {
	entries []Entry
	size    int   // 추정 크기 (바이트)
	mint    int64
	maxt    int64
}

// MemChunk는 Loki의 메모리 내 청크를 나타낸다.
// pkg/chunkenc/memchunk.go의 MemChunk와 동일한 구조이다.
type MemChunk struct {
	blocks        []*Block
	head          *HeadBlock
	blockSize     int // head block → block 전환 임계값 (바이트)
	maxChunkSize  int // 전체 청크 최대 크기
	format        string
	totalSize     int // 전체 추정 크기
	totalEntries  int
	closed        bool // 더 이상 쓰기 불가
}

// NewMemChunk는 새 MemChunk를 생성한다.
func NewMemChunk(blockSize, maxChunkSize int) *MemChunk {
	return &MemChunk{
		head:         &HeadBlock{},
		blockSize:    blockSize,
		maxChunkSize: maxChunkSize,
		format:       "v3",
	}
}

// Append는 엔트리를 청크에 추가한다.
// Loki의 MemChunk.Append()와 동일한 동작이다.
//
// 규칙:
//   1. 타임스탬프는 마지막 엔트리보다 크거나 같아야 한다 (시간순 보장)
//   2. head block 크기가 임계값을 넘으면 block으로 전환 (cut)
//   3. 전체 청크 크기가 최대값을 넘으면 에러 (새 청크 생성 필요)
func (mc *MemChunk) Append(ts time.Time, line string) error {
	if mc.closed {
		return fmt.Errorf("청크가 닫혀있음 (더 이상 쓰기 불가)")
	}

	nsec := ts.UnixNano()

	// 시간순 검증: 마지막 엔트리보다 이전 타임스탬프는 거부
	if mc.totalEntries > 0 {
		lastTs := mc.lastTimestamp()
		if nsec < lastTs {
			return fmt.Errorf("out-of-order 엔트리: %s < %s",
				ts.Format("15:04:05.000"),
				time.Unix(0, lastTs).Format("15:04:05.000"))
		}
	}

	// 전체 크기 검사
	entrySize := 8 + 4 + len(line) // timestamp(8) + lineLen(4) + line
	if mc.totalSize+entrySize > mc.maxChunkSize {
		return fmt.Errorf("청크 크기 초과 (%d + %d > %d)",
			mc.totalSize, entrySize, mc.maxChunkSize)
	}

	// head block에 추가
	entry := Entry{Timestamp: ts, Line: line}
	mc.head.entries = append(mc.head.entries, entry)
	mc.head.size += entrySize

	if len(mc.head.entries) == 1 {
		mc.head.mint = nsec
	}
	if nsec < mc.head.mint {
		mc.head.mint = nsec
	}
	mc.head.maxt = nsec

	mc.totalSize += entrySize
	mc.totalEntries++

	// head block 크기가 임계값을 넘으면 cut
	if mc.head.size >= mc.blockSize {
		mc.cut()
	}

	return nil
}

// cut은 현재 head block을 sealed block으로 전환한다.
func (mc *MemChunk) cut() {
	if len(mc.head.entries) == 0 {
		return
	}

	block := &Block{
		entries: make([]Entry, len(mc.head.entries)),
		mint:    mc.head.mint,
		maxt:    mc.head.maxt,
	}
	copy(block.entries, mc.head.entries)
	mc.blocks = append(mc.blocks, block)

	// head block 초기화
	mc.head.entries = mc.head.entries[:0]
	mc.head.size = 0
	mc.head.mint = 0
	mc.head.maxt = 0
}

// lastTimestamp은 마지막으로 추가된 엔트리의 타임스탬프를 반환한다.
func (mc *MemChunk) lastTimestamp() int64 {
	// head block에 엔트리가 있으면 head의 마지막
	if len(mc.head.entries) > 0 {
		return mc.head.maxt
	}
	// 아니면 마지막 block의 마지막
	if len(mc.blocks) > 0 {
		return mc.blocks[len(mc.blocks)-1].maxt
	}
	return 0
}

// Bounds는 청크의 시간 범위를 반환한다.
// Loki의 Chunk.Bounds()와 동일하다. O(1) 시간 복잡도.
func (mc *MemChunk) Bounds() (time.Time, time.Time) {
	var mint, maxt int64

	// 최소 타임스탬프: 첫 번째 블록 또는 head block
	if len(mc.blocks) > 0 {
		mint = mc.blocks[0].mint
	} else if len(mc.head.entries) > 0 {
		mint = mc.head.mint
	}

	// 최대 타임스탬프: head block 또는 마지막 블록
	if len(mc.head.entries) > 0 {
		maxt = mc.head.maxt
	} else if len(mc.blocks) > 0 {
		maxt = mc.blocks[len(mc.blocks)-1].maxt
	}

	return time.Unix(0, mint), time.Unix(0, maxt)
}

// Size는 청크의 추정 크기를 반환한다 (바이트).
func (mc *MemChunk) Size() int {
	return mc.totalSize
}

// NumEntries는 총 엔트리 수를 반환한다.
func (mc *MemChunk) NumEntries() int {
	return mc.totalEntries
}

// NumBlocks는 sealed 블록 수를 반환한다 (head block 제외).
func (mc *MemChunk) NumBlocks() int {
	return len(mc.blocks)
}

// Close는 청크를 닫는다 (더 이상 쓰기 불가).
// 남은 head block을 flush한다.
func (mc *MemChunk) Close() {
	if !mc.closed {
		mc.cut() // 남은 head block을 block으로 전환
		mc.closed = true
	}
}

// Iterator는 지정된 시간 범위와 방향으로 엔트리를 순회하는 반복자를 반환한다.
// Loki의 Chunk.Iterator()와 동일한 인터페이스이다.
func (mc *MemChunk) Iterator(from, through time.Time, direction IterDirection) EntryIterator {
	fromNs := from.UnixNano()
	throughNs := through.UnixNano()

	// 모든 블록에서 시간 범위에 해당하는 엔트리 수집
	var entries []Entry

	// sealed blocks
	for _, block := range mc.blocks {
		// 블록의 시간 범위가 쿼리 범위와 겹치는지 빠르게 확인
		if block.maxt < fromNs || block.mint >= throughNs {
			continue // 이 블록은 쿼리 범위 밖
		}
		for _, entry := range block.entries {
			ts := entry.Timestamp.UnixNano()
			if ts >= fromNs && ts < throughNs {
				entries = append(entries, entry)
			}
		}
	}

	// head block
	if len(mc.head.entries) > 0 {
		if !(mc.head.maxt < fromNs || mc.head.mint >= throughNs) {
			for _, entry := range mc.head.entries {
				ts := entry.Timestamp.UnixNano()
				if ts >= fromNs && ts < throughNs {
					entries = append(entries, entry)
				}
			}
		}
	}

	// 방향에 따라 정렬
	if direction == Backward {
		// 역순 정렬
		for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
			entries[i], entries[j] = entries[j], entries[i]
		}
	}

	return &sliceIterator{
		entries: entries,
		pos:     -1,
	}
}

// sliceIterator는 엔트리 슬라이스를 순회하는 단순 반복자이다.
type sliceIterator struct {
	entries []Entry
	pos     int
}

func (it *sliceIterator) Next() bool {
	it.pos++
	return it.pos < len(it.entries)
}

func (it *sliceIterator) Entry() Entry {
	return it.entries[it.pos]
}

func (it *sliceIterator) Close() {
	// 정리할 리소스 없음
}

func (it *sliceIterator) Err() error {
	return nil
}

// =============================================================================
// 유틸리티 함수
// =============================================================================

func printEntry(e Entry) string {
	line := e.Line
	if len(line) > 65 {
		line = line[:65] + "..."
	}
	return fmt.Sprintf("[%s] %s", e.Timestamp.Format("15:04:05.000"), line)
}

func main() {
	fmt.Println("=================================================================")
	fmt.Println("  Loki MemChunk (인메모리 청크) 시뮬레이션")
	fmt.Println("  - Append-only 쓰기")
	fmt.Println("  - Head Block → Sealed Block 전환")
	fmt.Println("  - Forward/Backward Iterator")
	fmt.Println("  - Time Range 필터링")
	fmt.Println("=================================================================")
	fmt.Println()

	// =========================================================================
	// 시나리오 1: 기본 Append 및 Block Cut
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println(" 시나리오 1: 기본 Append 및 Block Cut")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	// 블록 크기 200바이트, 최대 청크 크기 2000바이트
	mc := NewMemChunk(200, 2000)

	baseTime := time.Date(2024, 6, 15, 14, 0, 0, 0, time.UTC)

	logLines := []string{
		`level=info msg="server started" port=3100`,
		`level=info msg="ring joined" tokens=128 state=ACTIVE`,
		`level=info msg="ingester ready" wal_replay=done`,
		`level=info msg="GET /ready 200" duration=1ms`,
		`level=warn msg="slow query" duration=5.2s query="{app=nginx}"`,
		`level=error msg="chunk flush failed" err="context deadline exceeded"`,
		`level=info msg="compaction started" table=index_19500`,
		`level=info msg="compaction finished" duration=12.3s merged=45`,
		`level=warn msg="high memory usage" heap=3.2GB limit=4GB`,
		`level=info msg="WAL segment rotated" segment=000042 size=128MB`,
		`level=error msg="query timeout" user=admin duration=30s`,
		`level=info msg="chunk stored" key=fake/abc123 size=1.5MB`,
	}

	for i, line := range logLines {
		ts := baseTime.Add(time.Duration(i*500) * time.Millisecond)
		err := mc.Append(ts, line)
		if err != nil {
			fmt.Printf("  Append 에러: %v\n", err)
			continue
		}

		blocksBefore := mc.NumBlocks()
		_ = blocksBefore
	}

	fmt.Printf("  총 엔트리: %d\n", mc.NumEntries())
	fmt.Printf("  Sealed 블록: %d\n", mc.NumBlocks())
	fmt.Printf("  Head block 엔트리: %d\n", len(mc.head.entries))
	fmt.Printf("  추정 크기: %d 바이트\n", mc.Size())

	mint, maxt := mc.Bounds()
	fmt.Printf("  시간 범위: %s ~ %s\n",
		mint.Format("15:04:05.000"), maxt.Format("15:04:05.000"))
	fmt.Println()

	// 블록별 상세
	for i, block := range mc.blocks {
		fmt.Printf("  블록 %d: 엔트리 %d개, %s ~ %s\n", i, len(block.entries),
			time.Unix(0, block.mint).Format("15:04:05.000"),
			time.Unix(0, block.maxt).Format("15:04:05.000"))
	}
	if len(mc.head.entries) > 0 {
		fmt.Printf("  Head:   엔트리 %d개, %s ~ %s (미 sealed)\n",
			len(mc.head.entries),
			time.Unix(0, mc.head.mint).Format("15:04:05.000"),
			time.Unix(0, mc.head.maxt).Format("15:04:05.000"))
	}

	// =========================================================================
	// 시나리오 2: Forward Iterator
	// =========================================================================
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println(" 시나리오 2: Forward Iterator (전체 범위)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	// 전체 범위 Forward
	fwdIter := mc.Iterator(
		baseTime.Add(-1*time.Second),
		baseTime.Add(1*time.Hour),
		Forward,
	)
	defer fwdIter.Close()

	count := 0
	for fwdIter.Next() {
		count++
		e := fwdIter.Entry()
		fmt.Printf("  %2d. %s\n", count, printEntry(e))
	}
	fmt.Printf("\n  총 %d개 엔트리 (Forward)\n", count)

	// =========================================================================
	// 시나리오 3: Backward Iterator
	// =========================================================================
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println(" 시나리오 3: Backward Iterator (전체 범위)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	bwdIter := mc.Iterator(
		baseTime.Add(-1*time.Second),
		baseTime.Add(1*time.Hour),
		Backward,
	)
	defer bwdIter.Close()

	count = 0
	for bwdIter.Next() {
		count++
		e := bwdIter.Entry()
		fmt.Printf("  %2d. %s\n", count, printEntry(e))
	}
	fmt.Printf("\n  총 %d개 엔트리 (Backward)\n", count)

	// =========================================================================
	// 시나리오 4: 시간 범위 필터링
	// =========================================================================
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println(" 시나리오 4: 시간 범위 필터링")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	// 2초 ~ 4초 범위만 조회
	from := baseTime.Add(2 * time.Second)
	through := baseTime.Add(4 * time.Second)

	fmt.Printf("  쿼리 범위: %s ~ %s\n",
		from.Format("15:04:05.000"), through.Format("15:04:05.000"))
	fmt.Println()

	rangeIter := mc.Iterator(from, through, Forward)
	defer rangeIter.Close()

	count = 0
	for rangeIter.Next() {
		count++
		e := rangeIter.Entry()
		fmt.Printf("  %2d. %s\n", count, printEntry(e))
	}
	fmt.Printf("\n  범위 내 %d개 엔트리\n", count)

	// =========================================================================
	// 시나리오 5: Out-of-Order 거부
	// =========================================================================
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println(" 시나리오 5: Out-of-Order 엔트리 거부")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	// 마지막 엔트리보다 이전 타임스탬프로 추가 시도
	_, lastTs := mc.Bounds()
	pastTs := lastTs.Add(-5 * time.Second)
	err := mc.Append(pastTs, "this should fail - out of order")
	if err != nil {
		fmt.Printf("  예상된 거부: %v\n", err)
	}

	// 같은 타임스탬프는 허용 (동일 시각에 여러 로그)
	err = mc.Append(lastTs, "same timestamp is allowed")
	if err != nil {
		fmt.Printf("  예상치 못한 에러: %v\n", err)
	} else {
		fmt.Println("  동일 타임스탬프 허용: 성공")
	}

	// 미래 타임스탬프는 허용
	futureTs := lastTs.Add(1 * time.Second)
	err = mc.Append(futureTs, "future timestamp is allowed")
	if err != nil {
		fmt.Printf("  예상치 못한 에러: %v\n", err)
	} else {
		fmt.Println("  미래 타임스탬프 허용: 성공")
	}

	// =========================================================================
	// 시나리오 6: 청크 크기 초과 및 Close
	// =========================================================================
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println(" 시나리오 6: 청크 크기 초과 및 Close")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	// 작은 청크 (최대 500바이트)
	smallChunk := NewMemChunk(100, 500)
	smallBase := time.Date(2024, 6, 15, 15, 0, 0, 0, time.UTC)

	fmt.Printf("  최대 청크 크기: %d 바이트\n\n", 500)

	for i := 0; i < 20; i++ {
		ts := smallBase.Add(time.Duration(i) * time.Second)
		line := fmt.Sprintf("log entry number %d with some padding data to fill up the chunk", i)
		err := smallChunk.Append(ts, line)
		if err != nil {
			fmt.Printf("  엔트리 %2d: 거부 - %v\n", i, err)
			fmt.Printf("            현재 크기: %d 바이트, 엔트리: %d개\n",
				smallChunk.Size(), smallChunk.NumEntries())
			break
		} else {
			fmt.Printf("  엔트리 %2d: 추가 (크기: %d/%d 바이트)\n",
				i, smallChunk.Size(), 500)
		}
	}

	fmt.Println()

	// Close 후 쓰기 시도
	smallChunk.Close()
	err = smallChunk.Append(smallBase.Add(100*time.Second), "after close")
	if err != nil {
		fmt.Printf("  Close 후 쓰기 시도: %v\n", err)
	}

	// Close 후에도 읽기는 가능
	closeIter := smallChunk.Iterator(
		smallBase.Add(-1*time.Second),
		smallBase.Add(1*time.Hour),
		Forward,
	)
	count = 0
	for closeIter.Next() {
		count++
	}
	closeIter.Close()
	fmt.Printf("  Close 후 읽기: %d개 엔트리 (정상)\n", count)

	// =========================================================================
	// 시나리오 7: 여러 청크를 연결하여 스트림 구성
	// =========================================================================
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println(" 시나리오 7: 여러 청크를 연결하여 스트림 구성")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	// Loki에서는 스트림이 여러 청크로 구성된다.
	// 청크가 가득 차면 새 청크를 생성한다.
	maxChunkSize := 300
	streamBase := time.Date(2024, 6, 15, 16, 0, 0, 0, time.UTC)

	var chunks []*MemChunk
	current := NewMemChunk(100, maxChunkSize)
	chunks = append(chunks, current)

	totalAdded := 0
	for i := 0; i < 15; i++ {
		ts := streamBase.Add(time.Duration(i) * time.Second)
		line := fmt.Sprintf("stream log %d: %s", i, strings.Repeat(".", 20))

		err := current.Append(ts, line)
		if err != nil {
			// 청크가 가득 차면 새 청크 생성
			current.Close()
			current = NewMemChunk(100, maxChunkSize)
			chunks = append(chunks, current)
			current.Append(ts, line)
		}
		totalAdded++
	}

	fmt.Printf("  청크 수: %d (최대 크기: %d 바이트)\n", len(chunks), maxChunkSize)
	fmt.Printf("  총 추가 엔트리: %d\n\n", totalAdded)

	for i, c := range chunks {
		mint, maxt := c.Bounds()
		fmt.Printf("  청크 %d: 엔트리 %d개, 크기 %d B, %s ~ %s\n",
			i, c.NumEntries(), c.Size(),
			mint.Format("15:04:05"), maxt.Format("15:04:05"))
	}

	// 전체 스트림 범위로 Multi-Chunk Iterator 시뮬레이션
	fmt.Println()
	fmt.Println("  전체 스트림 조회 (Multi-Chunk Forward):")
	totalRead := 0
	for _, c := range chunks {
		iter := c.Iterator(
			streamBase.Add(-1*time.Second),
			streamBase.Add(1*time.Hour),
			Forward,
		)
		for iter.Next() {
			totalRead++
			e := iter.Entry()
			fmt.Printf("    %2d. %s\n", totalRead, printEntry(e))
		}
		iter.Close()
	}

	fmt.Println()
	fmt.Println("=================================================================")
	fmt.Println("  시뮬레이션 완료")
	fmt.Println()
	fmt.Println("  MemChunk 라이프사이클:")
	fmt.Println("  ┌─────────┐   Append   ┌───────────┐   cut    ┌──────────┐")
	fmt.Println("  │  Empty  │──────────→ │ Head Block │───────→ │  Block   │")
	fmt.Println("  │         │            │ (쓰기 중)  │  (크기   │ (sealed) │")
	fmt.Println("  └─────────┘            └───────────┘  초과)   └──────────┘")
	fmt.Println("                               │                     │")
	fmt.Println("                               │ (최대 크기 도달)      │")
	fmt.Println("                               ▼                     │")
	fmt.Println("                         ┌──────────┐               │")
	fmt.Println("                         │  Close   │←──────────────┘")
	fmt.Println("                         │ (읽기만) │")
	fmt.Println("                         └──────────┘")
	fmt.Println()
	fmt.Println("  핵심 포인트:")
	fmt.Println("  1. Append-only 구조로 시간순 쓰기만 허용")
	fmt.Println("  2. Head Block이 크기 임계값에 도달하면 sealed Block으로 전환")
	fmt.Println("  3. Forward/Backward Iterator로 유연한 읽기 접근")
	fmt.Println("  4. Bounds()로 O(1) 시간에 시간 범위 확인")
	fmt.Println("  5. 청크 크기 초과 시 새 청크로 전환 (스트림 = 청크 목록)")
	fmt.Println("=================================================================")
}
