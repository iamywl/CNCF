package main

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"time"
)

// =============================================================================
// Prometheus TSDB Block Compaction PoC
// =============================================================================
//
// Prometheus TSDB는 시계열 데이터를 시간 범위별 "블록"으로 나누어 저장한다.
// 블록은 불변(immutable)이며, 작은 블록들은 LeveledCompactor에 의해
// 더 큰 블록으로 병합(compaction)된다.
//
// 실제 코드 참조:
//   - tsdb/db.go: DefaultBlockDuration = 2h
//   - tsdb/compact.go: ExponentialBlockRanges(), LeveledCompactor.selectDirs()
//   - tsdb/compact.go: splitByRange() — 시간 범위별 블록 그룹핑
//   - tsdb/compact.go: CompactBlockMetas() — 블록 메타 병합
//   - tsdb/block.go: BlockMeta 구조체 (ULID, MinTime, MaxTime, Compaction.Level)

// =============================================================================
// 데이터 구조
// =============================================================================

// Sample은 하나의 시계열 데이터 포인트를 나타낸다.
// Prometheus의 모든 메트릭은 (timestamp, value) 쌍으로 기록된다.
type Sample struct {
	Timestamp int64   // 밀리초 단위 Unix 타임스탬프
	Value     float64 // 메트릭 값
}

// Block은 특정 시간 범위의 시계열 데이터를 담는 불변(immutable) 저장 단위이다.
// 실제 Prometheus에서 블록은 디스크상의 디렉토리이며 meta.json, chunks/, index/ 파일로 구성된다.
//
// 참조: tsdb/block.go — type BlockMeta struct {
//   ULID, MinTime, MaxTime, Compaction{Level, Sources, Parents}
// }
type Block struct {
	ID      string                // 고유 식별자 (실제로는 ULID)
	MinTime int64                 // 블록 내 최소 타임스탬프
	MaxTime int64                 // 블록 내 최대 타임스탬프
	Level   int                   // 컴팩션 레벨 (0=Head에서 직접 생성, 1=1차 컴팩션, ...)
	Series  map[string][]Sample   // 메트릭명 → 샘플 목록
	Sources []string              // 이 블록을 만드는 데 사용된 원본 블록 ID들
}

// sampleCount는 블록 내 전체 샘플 수를 반환한다.
func (b *Block) sampleCount() int {
	count := 0
	for _, samples := range b.Series {
		count += len(samples)
	}
	return count
}

// duration은 블록의 시간 범위를 반환한다.
func (b *Block) duration() int64 {
	return b.MaxTime - b.MinTime
}

// Head는 현재 수집 중인 인메모리 버퍼이다.
// Prometheus에서 Head는 WAL(Write-Ahead Log)과 함께 최근 데이터를 보관하며,
// 일정 시간이 지나면 블록으로 플러시된다.
//
// 참조: tsdb/head.go — Head 구조체의 Append(), truncateMemory()
type Head struct {
	MinTime int64                 // 헤드 내 최소 타임스탬프
	MaxTime int64                 // 헤드 내 최대 타임스탬프
	Series  map[string][]Sample   // 메트릭명 → 인메모리 샘플
}

// NewHead는 빈 Head를 생성한다.
func NewHead() *Head {
	return &Head{
		MinTime: math.MaxInt64,
		MaxTime: math.MinInt64,
		Series:  make(map[string][]Sample),
	}
}

// Add는 Head에 샘플을 추가한다.
func (h *Head) Add(metric string, ts int64, value float64) {
	h.Series[metric] = append(h.Series[metric], Sample{Timestamp: ts, Value: value})
	if ts < h.MinTime {
		h.MinTime = ts
	}
	if ts > h.MaxTime {
		h.MaxTime = ts
	}
}

// sampleCount는 Head 내 전체 샘플 수를 반환한다.
func (h *Head) sampleCount() int {
	count := 0
	for _, samples := range h.Series {
		count += len(samples)
	}
	return count
}

// Flush는 Head의 데이터를 블록으로 변환한다.
// 실제 Prometheus에서는 Head의 데이터가 chunkRange(기본 2시간)를 초과하면
// 자동으로 블록으로 컴팩팅된다. (tsdb/db.go compactHead())
func (h *Head) Flush(blockID string) *Block {
	block := &Block{
		ID:      blockID,
		MinTime: h.MinTime,
		MaxTime: h.MaxTime,
		Level:   0, // Head에서 직접 생성된 블록은 레벨 0
		Series:  make(map[string][]Sample),
		Sources: []string{blockID},
	}
	// 시리즈 데이터 복사
	for metric, samples := range h.Series {
		copied := make([]Sample, len(samples))
		copy(copied, samples)
		// 타임스탬프 순으로 정렬 보장
		sort.Slice(copied, func(i, j int) bool {
			return copied[i].Timestamp < copied[j].Timestamp
		})
		block.Series[metric] = copied
	}
	return block
}

// Reset은 Head를 초기 상태로 되돌린다.
func (h *Head) Reset() {
	h.MinTime = math.MaxInt64
	h.MaxTime = math.MinInt64
	h.Series = make(map[string][]Sample)
}

// =============================================================================
// TSDB
// =============================================================================

// TSDB는 Prometheus의 시계열 데이터베이스를 시뮬레이션한다.
//
// 핵심 설계:
//   - blockDuration: 기본 블록 시간 범위 (기본값 2시간 = 7,200,000ms)
//   - ranges: 컴팩션 레벨별 시간 범위 (ExponentialBlockRanges 함수로 계산)
//     예: [2h, 6h, 18h, 54h] — 각 단계에서 3배씩 증가
//
// 참조: tsdb/compact.go — ExponentialBlockRanges(minSize, steps, stepSize)
//       tsdb/db.go — DefaultOptions().RetentionDuration = 15일
type TSDB struct {
	head          *Head
	blocks        []*Block
	blockDuration int64   // 기본 블록 시간 범위 (밀리초)
	ranges        []int64 // 레벨별 컴팩션 범위
	blockCounter  int     // 블록 ID 생성용 카운터
	retentionMs   int64   // 보존 기간 (밀리초)
}

// NewTSDB는 새로운 TSDB를 생성한다.
// blockDuration은 Level 0 블록의 시간 범위이다.
// 실제 Prometheus는 ExponentialBlockRanges(2h, 3, 5)로 [2h, 6h, 18h, 54h, 162h]를 사용한다.
// 여기서는 시연을 위해 분 단위로 축소한다.
func NewTSDB(blockDurationMin int64, retentionMin int64) *TSDB {
	blockDurationMs := blockDurationMin * 60 * 1000 // 분 → 밀리초

	// ExponentialBlockRanges 시뮬레이션
	// 실제 코드: tsdb/compact.go line 41-50
	//   func ExponentialBlockRanges(minSize int64, steps, stepSize int) []int64 {
	//       curRange := minSize
	//       for range steps { ranges = append(ranges, curRange); curRange *= stepSize }
	//   }
	// Prometheus 기본값: ExponentialBlockRanges(2h, 3, 5) → [2h, 10h, 50h]
	// 여기서는 stepSize=3을 사용하여 더 직관적으로 시연: [2h, 6h, 18h]
	ranges := exponentialBlockRanges(blockDurationMs, 3, 3)

	return &TSDB{
		head:          NewHead(),
		blocks:        make([]*Block, 0),
		blockDuration: blockDurationMs,
		ranges:        ranges,
		blockCounter:  0,
		retentionMs:   retentionMin * 60 * 1000,
	}
}

// exponentialBlockRanges는 Prometheus의 ExponentialBlockRanges 함수를 재현한다.
// tsdb/compact.go:41 참조
func exponentialBlockRanges(minSize int64, steps, stepSize int) []int64 {
	ranges := make([]int64, 0, steps)
	curRange := minSize
	for i := 0; i < steps; i++ {
		ranges = append(ranges, curRange)
		curRange *= int64(stepSize)
	}
	return ranges
}

// generateBlockID는 시뮬레이션용 블록 ID를 생성한다.
// 실제 Prometheus는 ulid.New()를 사용하여 시간 순서가 보장되는 ULID를 생성한다.
func (db *TSDB) generateBlockID() string {
	db.blockCounter++
	return fmt.Sprintf("BLOCK-%04d", db.blockCounter)
}

// Append는 시계열 데이터를 TSDB에 추가한다.
// Head에 데이터를 쓰고, blockDuration을 초과하면 자동으로 블록으로 플러시한다.
//
// 참조: tsdb/db.go — DB.compactHead()
//   "Compacts first if the head has a compaction range worth of data"
func (db *TSDB) Append(metric string, ts int64, value float64) {
	db.head.Add(metric, ts, value)

	// Head의 시간 범위가 blockDuration을 초과하면 자동 플러시
	if db.head.MaxTime-db.head.MinTime >= db.blockDuration {
		db.flushHead()
	}
}

// flushHead는 Head 데이터를 블록으로 변환하여 blocks에 추가한다.
func (db *TSDB) flushHead() {
	if db.head.sampleCount() == 0 {
		return
	}

	blockID := db.generateBlockID()
	block := db.head.Flush(blockID)

	db.blocks = append(db.blocks, block)
	db.head.Reset()

	fmt.Printf("  [FLUSH] Head → %s (범위: %s ~ %s, 레벨: %d, 샘플: %d)\n",
		block.ID,
		formatTime(block.MinTime),
		formatTime(block.MaxTime),
		block.Level,
		block.sampleCount(),
	)
}

// ForceFlush는 Head에 남은 데이터를 강제로 블록으로 변환한다.
func (db *TSDB) ForceFlush() {
	if db.head.sampleCount() > 0 {
		db.flushHead()
	}
}

// =============================================================================
// Compaction — 핵심 알고리즘
// =============================================================================

// Compact는 LeveledCompactor의 컴팩션 전략을 시뮬레이션한다.
//
// 실제 Prometheus의 컴팩션 전략 (tsdb/compact.go):
//   1. 블록을 MinTime 순으로 정렬
//   2. 겹치는(overlapping) 블록을 먼저 병합 (selectOverlappingDirs)
//   3. 최신 블록을 제외하고 (아직 백업되지 않았을 수 있으므로)
//   4. ranges[1:]부터 순회하며 splitByRange로 블록 그룹핑
//   5. 한 그룹 내 블록이 2개 이상이고, 범위를 채우면 컴팩션 대상으로 선정
//
// splitByRange 핵심 로직 (tsdb/compact.go:400):
//   - 시간 범위 tr에 대해, 블록의 MinTime을 tr로 나누어 정렬된 그룹을 만듦
//   - t0 = tr * (minTime / tr)  ← 시간 범위의 시작점 정렬
//   - MaxTime > t0+tr 이면 해당 범위에 맞지 않으므로 스킵
func (db *TSDB) Compact() int {
	totalCompactions := 0

	for {
		plan := db.planCompaction()
		if len(plan) == 0 {
			break
		}

		// plan에 있는 블록들을 하나의 블록으로 병합
		db.compactBlocks(plan)
		totalCompactions++
	}

	return totalCompactions
}

// planCompaction은 LeveledCompactor.plan()을 시뮬레이션한다.
// tsdb/compact.go:279 참조
func (db *TSDB) planCompaction() []*Block {
	if len(db.blocks) < 2 {
		return nil
	}

	// 블록을 MinTime 순으로 정렬
	sort.Slice(db.blocks, func(i, j int) bool {
		return db.blocks[i].MinTime < db.blocks[j].MinTime
	})

	// 1. 겹치는 블록 먼저 처리 (selectOverlappingDirs 시뮬레이션)
	overlapping := db.selectOverlapping()
	if len(overlapping) > 0 {
		return overlapping
	}

	// 2. 최신 블록 제외 (WAL에서 방금 생성된 블록은 컴팩션 대상에서 제외)
	// tsdb/compact.go:302: dms = dms[:len(dms)-1]
	candidates := db.blocks[:len(db.blocks)-1]
	if len(candidates) < 2 {
		return nil
	}

	// 3. selectDirs 시뮬레이션 — ranges[1:]부터 순회
	// tsdb/compact.go:332-367
	if len(db.ranges) < 2 {
		return nil
	}

	highTime := candidates[len(candidates)-1].MinTime

	for _, iv := range db.ranges[1:] {
		groups := db.splitByRange(candidates, iv)

		for _, group := range groups {
			if len(group) < 2 {
				continue
			}

			mint := group[0].MinTime
			maxt := group[len(group)-1].MaxTime

			// 범위를 완전히 채우거나, 최고 시간 이전에 있으면 컴팩션 대상
			// tsdb/compact.go:360:
			//   if (maxt-mint == iv || maxt <= highTime) && len(p) > 1
			if (maxt-mint == iv || maxt <= highTime) && len(group) > 1 {
				return group
			}
		}
	}

	return nil
}

// splitByRange는 블록들을 지정된 시간 범위로 그룹핑한다.
// tsdb/compact.go:400-437 참조
//
// 핵심 원리:
//   - 시간 범위 tr에 대해 정렬된 시간 축을 tr 단위로 분할
//   - 각 블록의 MinTime을 기준으로 어느 슬롯에 속하는지 계산
//   - t0 = tr * (minTime / tr) → 가장 가까운 정렬된 시간 범위 시작점
//   - 블록의 MaxTime이 t0+tr을 초과하면 이 범위에 맞지 않으므로 스킵
func (db *TSDB) splitByRange(blocks []*Block, tr int64) [][]*Block {
	var result [][]*Block

	for i := 0; i < len(blocks); {
		m := blocks[i]

		// 정렬된 시간 범위의 시작점 계산
		var t0 int64
		if m.MinTime >= 0 {
			t0 = tr * (m.MinTime / tr)
		} else {
			t0 = tr * ((m.MinTime - tr + 1) / tr)
		}

		// 블록이 이 범위에 들어가지 않으면 스킵
		if m.MaxTime > t0+tr {
			i++
			continue
		}

		// 이 범위에 속하는 모든 블록을 그룹에 추가
		var group []*Block
		for ; i < len(blocks); i++ {
			if blocks[i].MaxTime > t0+tr {
				break
			}
			group = append(group, blocks[i])
		}

		if len(group) > 0 {
			result = append(result, group)
		}
	}

	return result
}

// selectOverlapping은 시간 범위가 겹치는 블록들을 찾는다.
// tsdb/compact.go:371 참조
func (db *TSDB) selectOverlapping() []*Block {
	if len(db.blocks) < 2 {
		return nil
	}

	var overlapping []*Block
	for i := 0; i < len(db.blocks)-1; i++ {
		if db.blocks[i].MaxTime > db.blocks[i+1].MinTime {
			if len(overlapping) == 0 {
				overlapping = append(overlapping, db.blocks[i])
			}
			overlapping = append(overlapping, db.blocks[i+1])
		} else if len(overlapping) > 0 {
			break
		}
	}

	return overlapping
}

// compactBlocks는 여러 블록을 하나로 병합한다.
// tsdb/compact.go:441 — CompactBlockMetas 참조
//
// 병합 시:
//   - 새 블록의 MinTime = min(모든 블록의 MinTime)
//   - 새 블록의 MaxTime = max(모든 블록의 MaxTime)
//   - 새 블록의 Level = max(모든 블록의 Level) + 1
//   - Sources는 모든 원본 블록의 Sources를 합집합
func (db *TSDB) compactBlocks(plan []*Block) {
	newBlock := &Block{
		ID:      db.generateBlockID(),
		MinTime: math.MaxInt64,
		MaxTime: math.MinInt64,
		Level:   0,
		Series:  make(map[string][]Sample),
		Sources: make([]string, 0),
	}

	// 원본 블록 ID들을 수집하고, 데이터 병합
	planIDs := make(map[string]bool)
	sourceSet := make(map[string]bool)

	for _, block := range plan {
		planIDs[block.ID] = true

		if block.MinTime < newBlock.MinTime {
			newBlock.MinTime = block.MinTime
		}
		if block.MaxTime > newBlock.MaxTime {
			newBlock.MaxTime = block.MaxTime
		}
		if block.Level > newBlock.Level {
			newBlock.Level = block.Level
		}

		// Sources 합집합
		for _, src := range block.Sources {
			sourceSet[src] = true
		}

		// 시리즈 데이터 병합
		for metric, samples := range block.Series {
			newBlock.Series[metric] = append(newBlock.Series[metric], samples...)
		}
	}

	// Level은 기존 최대 레벨 + 1
	newBlock.Level++

	// Sources 정리
	for src := range sourceSet {
		newBlock.Sources = append(newBlock.Sources, src)
	}
	sort.Strings(newBlock.Sources)

	// 시리즈 내 샘플 정렬 + 중복 제거
	for metric, samples := range newBlock.Series {
		sort.Slice(samples, func(i, j int) bool {
			return samples[i].Timestamp < samples[j].Timestamp
		})
		// 중복 제거
		deduped := make([]Sample, 0, len(samples))
		for i, s := range samples {
			if i == 0 || s.Timestamp != samples[i-1].Timestamp {
				deduped = append(deduped, s)
			}
		}
		newBlock.Series[metric] = deduped
	}

	// 원본 블록 제거, 새 블록 추가
	remaining := make([]*Block, 0, len(db.blocks)-len(plan)+1)
	for _, b := range db.blocks {
		if !planIDs[b.ID] {
			remaining = append(remaining, b)
		}
	}
	remaining = append(remaining, newBlock)
	db.blocks = remaining

	// 정렬 유지
	sort.Slice(db.blocks, func(i, j int) bool {
		return db.blocks[i].MinTime < db.blocks[j].MinTime
	})

	fmt.Printf("  [COMPACT] %s + ... (%d개) → %s (범위: %s ~ %s, 레벨: %d, 샘플: %d)\n",
		plan[0].ID,
		len(plan),
		newBlock.ID,
		formatTime(newBlock.MinTime),
		formatTime(newBlock.MaxTime),
		newBlock.Level,
		newBlock.sampleCount(),
	)
}

// =============================================================================
// Retention
// =============================================================================

// Retention은 지정된 시간 이전의 블록을 삭제한다.
// 실제 Prometheus: tsdb/db.go — DB.beyondTimeRetention()
//   "Blocks that are completely outside of the specified retention window will be removed."
func (db *TSDB) Retention(currentTime int64) int {
	cutoff := currentTime - db.retentionMs
	removed := 0

	remaining := make([]*Block, 0)
	for _, b := range db.blocks {
		if b.MaxTime <= cutoff {
			fmt.Printf("  [DELETE] %s (범위: %s ~ %s, 레벨: %d) — 보존 기간 초과\n",
				b.ID,
				formatTime(b.MinTime),
				formatTime(b.MaxTime),
				b.Level,
			)
			removed++
		} else {
			remaining = append(remaining, b)
		}
	}
	db.blocks = remaining
	return removed
}

// =============================================================================
// Query
// =============================================================================

// Query는 지정된 시간 범위에서 메트릭 데이터를 조회한다.
// 실제 Prometheus는 블록의 MinTime/MaxTime으로 관련 블록만 스캔하고,
// 인덱스(postings)를 통해 해당 시리즈를 빠르게 찾는다.
func (db *TSDB) Query(metric string, startTime, endTime int64) []Sample {
	var results []Sample

	blocksScanned := 0

	// 관련 블록만 스캔 (시간 범위 겹침 확인)
	for _, block := range db.blocks {
		// 블록이 쿼리 범위와 겹치지 않으면 스킵
		if block.MaxTime <= startTime || block.MinTime >= endTime {
			continue
		}
		blocksScanned++

		samples, exists := block.Series[metric]
		if !exists {
			continue
		}

		for _, s := range samples {
			if s.Timestamp >= startTime && s.Timestamp < endTime {
				results = append(results, s)
			}
		}
	}

	// Head에서도 조회
	if db.head.sampleCount() > 0 {
		if samples, exists := db.head.Series[metric]; exists {
			for _, s := range samples {
				if s.Timestamp >= startTime && s.Timestamp < endTime {
					results = append(results, s)
				}
			}
		}
	}

	// 타임스탬프 순 정렬
	sort.Slice(results, func(i, j int) bool {
		return results[i].Timestamp < results[j].Timestamp
	})

	fmt.Printf("  [QUERY] metric=%s, 범위=%s~%s, 스캔한 블록=%d, 결과=%d건\n",
		metric,
		formatTime(startTime),
		formatTime(endTime),
		blocksScanned,
		len(results),
	)

	return results
}

// =============================================================================
// 시각화
// =============================================================================

// PrintBlocks는 현재 블록 목록을 테이블로 출력한다.
func (db *TSDB) PrintBlocks() {
	fmt.Println()
	fmt.Println("┌─────────────┬────────────────┬────────────────┬───────┬────────┬──────────┐")
	fmt.Println("│ Block ID    │ MinTime        │ MaxTime        │ Level │ 샘플수 │ 범위     │")
	fmt.Println("├─────────────┼────────────────┼────────────────┼───────┼────────┼──────────┤")

	for _, b := range db.blocks {
		durationStr := formatDuration(b.duration())
		fmt.Printf("│ %-11s │ %-14s │ %-14s │   %d   │ %6d │ %-8s │\n",
			b.ID,
			formatTime(b.MinTime),
			formatTime(b.MaxTime),
			b.Level,
			b.sampleCount(),
			durationStr,
		)
	}

	fmt.Println("└─────────────┴────────────────┴────────────────┴───────┴────────┴──────────┘")
	fmt.Printf("총 블록 수: %d\n", len(db.blocks))
}

// PrintTimeline은 블록을 시간축 위에 시각화한다.
func (db *TSDB) PrintTimeline() {
	if len(db.blocks) == 0 {
		fmt.Println("(블록 없음)")
		return
	}

	// 전체 시간 범위 계산
	globalMin := db.blocks[0].MinTime
	globalMax := db.blocks[0].MaxTime
	for _, b := range db.blocks {
		if b.MinTime < globalMin {
			globalMin = b.MinTime
		}
		if b.MaxTime > globalMax {
			globalMax = b.MaxTime
		}
	}

	totalRange := globalMax - globalMin
	if totalRange == 0 {
		return
	}

	width := 72
	fmt.Println()
	fmt.Printf("블록 타임라인 (%s ~ %s)\n", formatTime(globalMin), formatTime(globalMax))
	fmt.Println(strings.Repeat("─", width+2))

	// 레벨별로 그룹핑
	maxLevel := 0
	for _, b := range db.blocks {
		if b.Level > maxLevel {
			maxLevel = b.Level
		}
	}

	for level := maxLevel; level >= 0; level-- {
		line := make([]byte, width)
		for i := range line {
			line[i] = ' '
		}

		for _, b := range db.blocks {
			if b.Level != level {
				continue
			}
			startPos := int(float64(b.MinTime-globalMin) / float64(totalRange) * float64(width-1))
			endPos := int(float64(b.MaxTime-globalMin) / float64(totalRange) * float64(width-1))
			if startPos < 0 {
				startPos = 0
			}
			if endPos >= width {
				endPos = width - 1
			}
			if endPos <= startPos {
				endPos = startPos + 1
				if endPos >= width {
					endPos = width - 1
				}
			}

			// 블록 표시: [====]
			char := byte('=')
			for i := startPos; i <= endPos; i++ {
				if i == startPos {
					line[i] = '['
				} else if i == endPos {
					line[i] = ']'
				} else {
					line[i] = char
				}
			}
		}

		fmt.Printf("L%d │%s│\n", level, string(line))
	}

	fmt.Printf("   └%s┘\n", strings.Repeat("─", width))

	// 시간축 레이블
	fmt.Printf("    %-*s%s\n", width-len(formatTime(globalMax)), formatTime(globalMin), formatTime(globalMax))
}

// =============================================================================
// 유틸리티
// =============================================================================

// 시뮬레이션 기준 시각: 2024-01-01 00:00:00 UTC
var baseTime = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()

func formatTime(ms int64) string {
	t := time.UnixMilli(ms).UTC()
	return t.Format("01/02 15:04")
}

func formatDuration(ms int64) string {
	hours := ms / (3600 * 1000)
	mins := (ms % (3600 * 1000)) / (60 * 1000)
	if hours > 0 && mins > 0 {
		return fmt.Sprintf("%dh%dm", hours, mins)
	} else if hours > 0 {
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dm", mins)
}

// =============================================================================
// 데모
// =============================================================================

func main() {
	fmt.Println("=================================================================")
	fmt.Println("Prometheus TSDB Block Compaction PoC")
	fmt.Println("=================================================================")
	fmt.Println()
	fmt.Println("Prometheus TSDB는 시계열 데이터를 시간 범위별 블록으로 나누어 저장한다.")
	fmt.Println("블록은 불변이며, LeveledCompactor가 작은 블록들을 병합하여")
	fmt.Println("더 큰 블록을 만든다.")
	fmt.Println()
	fmt.Println("실제 Prometheus 기본 설정:")
	fmt.Println("  - Level 0 블록: 2시간")
	fmt.Println("  - 컴팩션 범위: ExponentialBlockRanges(2h, 3, 3) → [2h, 6h, 18h]")
	fmt.Println("  - 보존 기간: 15일 (기본값)")
	fmt.Println()
	fmt.Println("이 PoC에서는 2시간 블록을 그대로 사용하되, 24시간 데이터를 시뮬레이션한다.")
	fmt.Println()

	// TSDB 생성: 블록 기본 범위 120분(2시간), 보존 기간 720분(12시간)
	db := NewTSDB(120, 720)

	fmt.Printf("설정: blockDuration=%s, ranges=%v, retention=%s\n",
		formatDuration(db.blockDuration),
		func() []string {
			s := make([]string, len(db.ranges))
			for i, r := range db.ranges {
				s[i] = formatDuration(r)
			}
			return s
		}(),
		formatDuration(db.retentionMs),
	)
	fmt.Println()

	// -----------------------------------------------------------------
	// 1단계: 24시간 데이터 생성 (15분 간격으로 샘플링)
	// -----------------------------------------------------------------
	fmt.Println("=== 1단계: 데이터 수집 (24시간, 15분 간격) ===")
	fmt.Println()

	rng := rand.New(rand.NewSource(42))
	metrics := []string{"cpu_usage", "memory_bytes", "http_requests_total"}

	// 24시간 = 1440분, 15분 간격 → 96 포인트
	for minute := int64(0); minute < 1440; minute += 15 {
		ts := baseTime + minute*60*1000

		for _, metric := range metrics {
			var value float64
			switch metric {
			case "cpu_usage":
				// CPU: 0~100% 사이 노이즈 포함
				value = 30.0 + 20.0*math.Sin(float64(minute)/180.0*math.Pi) + rng.Float64()*10
			case "memory_bytes":
				// 메모리: 서서히 증가하는 패턴
				value = 1e9 + float64(minute)*1e6 + rng.Float64()*1e8
			case "http_requests_total":
				// 요청 수: 단조 증가 카운터
				value = float64(minute) * 10.0
			}

			db.Append(metric, ts, value)
		}
	}
	db.ForceFlush()

	fmt.Printf("\n총 생성된 블록: %d개\n", len(db.blocks))
	db.PrintBlocks()
	db.PrintTimeline()

	// -----------------------------------------------------------------
	// 2단계: 컴팩션 실행
	// -----------------------------------------------------------------
	fmt.Println()
	fmt.Println("=== 2단계: 컴팩션 실행 ===")
	fmt.Println()
	fmt.Println("LeveledCompactor 전략:")
	fmt.Println("  1. 블록을 MinTime 순으로 정렬")
	fmt.Println("  2. 겹치는 블록이 있으면 먼저 병합")
	fmt.Println("  3. ranges[1:]부터 순회하며 splitByRange로 그룹핑")
	fmt.Println("  4. 그룹 내 2개 이상 블록이 범위를 채우면 병합")
	fmt.Println("  5. 최신 블록은 제외 (백업 윈도우 보장)")
	fmt.Println()

	compactions := db.Compact()

	fmt.Printf("\n컴팩션 완료: %d회 수행\n", compactions)
	db.PrintBlocks()
	db.PrintTimeline()

	// -----------------------------------------------------------------
	// 3단계: Retention 적용
	// -----------------------------------------------------------------
	fmt.Println()
	fmt.Println("=== 3단계: Retention 적용 ===")
	fmt.Println()
	fmt.Printf("보존 기간: %s, 현재 시각 기준: %s\n",
		formatDuration(db.retentionMs),
		formatTime(baseTime+1440*60*1000),
	)
	fmt.Println("→ MaxTime이 보존 기간 이전인 블록을 삭제한다.")
	fmt.Println()

	currentTime := baseTime + 1440*60*1000 // 24시간 후
	removed := db.Retention(currentTime)

	fmt.Printf("\n삭제된 블록: %d개\n", removed)
	db.PrintBlocks()
	db.PrintTimeline()

	// -----------------------------------------------------------------
	// 4단계: 쿼리
	// -----------------------------------------------------------------
	fmt.Println()
	fmt.Println("=== 4단계: 쿼리 실행 ===")
	fmt.Println()
	fmt.Println("블록 기반 쿼리: 시간 범위가 겹치는 블록만 스캔한다.")
	fmt.Println()

	// 최근 6시간 cpu_usage 조회
	queryStart := baseTime + 1080*60*1000 // 18시간 후 시점
	queryEnd := baseTime + 1440*60*1000   // 24시간 후 시점
	fmt.Printf("쿼리: cpu_usage, %s ~ %s (최근 6시간)\n", formatTime(queryStart), formatTime(queryEnd))

	results := db.Query("cpu_usage", queryStart, queryEnd)
	if len(results) > 0 {
		fmt.Printf("\n  결과 (%d건 중 처음 5건):\n", len(results))
		limit := 5
		if len(results) < limit {
			limit = len(results)
		}
		for i := 0; i < limit; i++ {
			fmt.Printf("    %s: %.2f\n", formatTime(results[i].Timestamp), results[i].Value)
		}
		if len(results) > 5 {
			fmt.Printf("    ... 외 %d건\n", len(results)-5)
		}
	}

	// 전체 시간 범위 http_requests_total 조회
	fmt.Println()
	fmt.Printf("쿼리: http_requests_total, 전체 시간 범위\n")
	allResults := db.Query("http_requests_total", baseTime, baseTime+1440*60*1000)
	if len(allResults) > 0 {
		fmt.Printf("\n  결과: %d건 (첫 값: %.0f, 마지막 값: %.0f)\n",
			len(allResults),
			allResults[0].Value,
			allResults[len(allResults)-1].Value,
		)
	}

	// -----------------------------------------------------------------
	// 요약
	// -----------------------------------------------------------------
	fmt.Println()
	fmt.Println("=================================================================")
	fmt.Println("요약: Prometheus TSDB Block Compaction 핵심 원리")
	fmt.Println("=================================================================")
	fmt.Println()
	fmt.Println("1. Head → Block (Level 0)")
	fmt.Println("   - Head의 데이터가 blockDuration(2h)을 채우면 블록으로 플러시")
	fmt.Println("   - 블록은 불변(immutable) — 한번 생성되면 수정 불가")
	fmt.Println()
	fmt.Println("2. LeveledCompactor")
	fmt.Println("   - ExponentialBlockRanges로 컴팩션 범위 결정: [2h, 6h, 18h]")
	fmt.Println("   - splitByRange: 시간축을 범위 단위로 분할, 블록을 그룹핑")
	fmt.Println("   - selectDirs: 한 그룹 내 2개 이상 블록이 범위를 채우면 병합")
	fmt.Println("   - 최신 블록은 제외하여 백업 윈도우 보장")
	fmt.Println()
	fmt.Println("3. Block 병합 (CompactBlockMetas)")
	fmt.Println("   - 새 블록 Level = max(원본 Level) + 1")
	fmt.Println("   - MinTime = min(원본), MaxTime = max(원본)")
	fmt.Println("   - Sources = 원본 블록들의 합집합")
	fmt.Println()
	fmt.Println("4. Retention")
	fmt.Println("   - MaxTime이 보존 기간을 초과한 블록을 통째로 삭제")
	fmt.Println("   - 개별 샘플이 아닌 블록 단위 삭제 → 효율적")
	fmt.Println()
	fmt.Println("5. 쿼리 최적화")
	fmt.Println("   - 블록의 MinTime/MaxTime으로 관련 블록만 스캔")
	fmt.Println("   - 컴팩션으로 블록 수가 줄어 I/O 감소")
}
