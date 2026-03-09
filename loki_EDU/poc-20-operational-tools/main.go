// Package main은 Loki 운영 도구(Chunks Inspector, Migration Tool, Loki Tool)의
// 핵심 개념을 시뮬레이션한다.
//
// 시뮬레이션하는 핵심 개념:
// 1. 청크 바이너리 포맷 인코딩/디코딩 (Varint, CRC32 체크섬)
// 2. 블록 기반 로그 엔트리 저장 (압축 시뮬레이션)
// 3. 시간 범위 분할 기반 병렬 마이그레이션 (ChunkMover)
// 4. 청크 무결성 검증 (체크섬 비교)
// 5. 바이트 크기 포맷팅
//
// 실행: go run main.go
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// ─────────────────────────────────────────────
// 1. 청크 바이너리 포맷
// ─────────────────────────────────────────────

const (
	MagicNumber    = 0x012EE56A
	ChunkFormatV1  = 1
	ChunkFormatV2  = 2
	ChunkFormatV3  = 3
	ChunkFormatV4  = 4
	EncodingNone   = 0
	EncodingGzip   = 1
	EncodingSnappy = 4
)

var encodingNames = map[int]string{
	0: "none", 1: "gzip", 2: "dumb", 3: "lz4",
	4: "snappy", 5: "lz4-256k", 6: "lz4-1M",
	7: "lz4-4M", 8: "flate", 9: "zstd",
}

// castagnoliTable은 CRC32 Castagnoli 테이블이다.
var castagnoliTable = crc32.MakeTable(crc32.Castagnoli)

// LogEntry는 로그 엔트리를 나타낸다.
type LogEntry struct {
	Timestamp int64
	Line      string
}

// Block은 청크 내의 데이터 블록이다.
type Block struct {
	NumEntries   uint64
	MinT         int64
	MaxT         int64
	Entries      []LogEntry
	RawData      []byte // 시뮬레이션된 압축 데이터
	OriginalData []byte // 원본 데이터
	Checksum     uint32
}

// ChunkHeader는 청크 메타데이터이다.
type ChunkHeader struct {
	Fingerprint uint64
	UserID      string
	From        int64
	Through     int64
	Labels      map[string]string
	Encoding    int
}

// LokiChunk는 전체 Loki 청크이다.
type LokiChunk struct {
	Header           ChunkHeader
	Format           byte
	Encoding         int
	Blocks           []Block
	MetadataChecksum uint32
}

// ─────────────────────────────────────────────
// 2. 청크 인코딩/디코딩
// ─────────────────────────────────────────────

// EncodeChunk는 청크를 바이너리로 인코딩한다.
func EncodeChunk(chunk *LokiChunk) []byte {
	var buf bytes.Buffer

	// 매직 넘버 (4B)
	binary.Write(&buf, binary.BigEndian, uint32(MagicNumber))

	// 버전 (1B)
	buf.WriteByte(chunk.Format)

	// 인코딩 (1B)
	buf.WriteByte(byte(chunk.Encoding))

	// 블록 데이터
	blockOffsets := make([]int, len(chunk.Blocks))
	for i, block := range chunk.Blocks {
		blockOffsets[i] = buf.Len()
		buf.Write(block.RawData)
		// 블록 체크섬 (4B)
		binary.Write(&buf, binary.BigEndian, block.Checksum)
	}

	// 메타데이터 테이블 시작 오프셋
	metaOffset := uint64(buf.Len())

	// 메타데이터: 블록 수
	metaBuf := make([]byte, binary.MaxVarintLen64)
	n := binary.PutUvarint(metaBuf, uint64(len(chunk.Blocks)))
	buf.Write(metaBuf[:n])

	// 각 블록의 메타데이터
	for i, block := range chunk.Blocks {
		// NumEntries
		n = binary.PutUvarint(metaBuf, block.NumEntries)
		buf.Write(metaBuf[:n])
		// MinT
		n = binary.PutVarint(metaBuf, block.MinT)
		buf.Write(metaBuf[:n])
		// MaxT
		n = binary.PutVarint(metaBuf, block.MaxT)
		buf.Write(metaBuf[:n])
		// Offset
		n = binary.PutUvarint(metaBuf, uint64(blockOffsets[i]))
		buf.Write(metaBuf[:n])
		// UncompSize (V3+)
		if chunk.Format >= ChunkFormatV3 {
			n = binary.PutUvarint(metaBuf, uint64(len(block.OriginalData)))
			buf.Write(metaBuf[:n])
		}
		// DataLength
		n = binary.PutUvarint(metaBuf, uint64(len(block.RawData)))
		buf.Write(metaBuf[:n])
	}

	// 메타데이터 체크섬
	metaData := buf.Bytes()[metaOffset:]
	metaChecksum := crc32.Checksum(metaData, castagnoliTable)
	binary.Write(&buf, binary.BigEndian, metaChecksum)
	chunk.MetadataChecksum = metaChecksum

	// 메타 오프셋 (마지막 8B)
	binary.Write(&buf, binary.BigEndian, metaOffset)

	return buf.Bytes()
}

// DecodeAndInspect는 청크를 디코딩하고 내용을 출력한다.
func DecodeAndInspect(data []byte, printBlocks, printLines bool) {
	if len(data) < 6 {
		fmt.Println("  오류: 데이터가 너무 짧음")
		return
	}

	// 매직 넘버 검증
	magic := binary.BigEndian.Uint32(data[0:4])
	if magic != MagicNumber {
		fmt.Printf("  오류: 잘못된 매직 넘버 0x%08x (기대: 0x%08x)\n", magic, MagicNumber)
		return
	}

	format := data[4]
	encoding := data[5]

	fmt.Printf("  Format (Version): %d\n", format)
	fmt.Printf("  Encoding: %s (%d)\n", encodingNames[int(encoding)], encoding)

	// 메타 오프셋 읽기 (마지막 8바이트)
	metaOffset := binary.BigEndian.Uint64(data[len(data)-8:])
	fmt.Printf("  Meta Offset: %d\n", metaOffset)

	// 메타데이터 체크섬 읽기 (마지막 8+4=12 바이트 전)
	storedChecksum := binary.BigEndian.Uint32(data[len(data)-12 : len(data)-8])

	// 메타데이터 영역
	metaEnd := uint64(len(data)) - 12 // checksum(4) + offset(8)
	metadata := data[metaOffset:metaEnd]

	computedChecksum := crc32.Checksum(metadata, castagnoliTable)
	if storedChecksum == computedChecksum {
		fmt.Printf("  Metadata Checksum: %08x OK\n", storedChecksum)
	} else {
		fmt.Printf("  Metadata Checksum: %08x BAD (computed: %08x)\n",
			storedChecksum, computedChecksum)
	}

	// 블록 수 읽기
	blockCount, n := binary.Uvarint(metadata)
	if n <= 0 {
		fmt.Println("  오류: 블록 수 읽기 실패")
		return
	}
	metadata = metadata[n:]
	fmt.Printf("  Found %d block(s)\n", blockCount)

	// 블록 메타데이터 파싱
	for i := 0; i < int(blockCount); i++ {
		numEntries, n := binary.Uvarint(metadata)
		metadata = metadata[n:]
		minT, n := binary.Varint(metadata)
		metadata = metadata[n:]
		maxT, n := binary.Varint(metadata)
		metadata = metadata[n:]
		offset, n := binary.Uvarint(metadata)
		metadata = metadata[n:]

		var uncompSize uint64
		if format >= ChunkFormatV3 {
			uncompSize, n = binary.Uvarint(metadata)
			metadata = metadata[n:]
		}

		dataLen, n := binary.Uvarint(metadata)
		metadata = metadata[n:]

		// 블록 데이터 체크섬 검증
		blockData := data[offset : offset+dataLen]
		blockChecksum := binary.BigEndian.Uint32(data[offset+dataLen : offset+dataLen+4])
		computedBlockCS := crc32.Checksum(blockData, castagnoliTable)

		if printBlocks {
			csStatus := "OK"
			if blockChecksum != computedBlockCS {
				csStatus = fmt.Sprintf("BAD (computed: %08x)", computedBlockCS)
			}
			ratio := float64(uncompSize) / float64(dataLen)
			if dataLen == 0 {
				ratio = 0
			}
			fmt.Printf("  Block %2d: entries=%d, minT=%s, maxT=%s, stored=%d, uncomp=%d, ratio=%.2f, checksum=%08x %s\n",
				i, numEntries,
				time.Unix(0, minT).UTC().Format("15:04:05"),
				time.Unix(0, maxT).UTC().Format("15:04:05"),
				dataLen, uncompSize, ratio,
				blockChecksum, csStatus)
		}

		if printLines {
			fmt.Printf("    (로그 라인 출력은 실제 압축 해제 필요)\n")
		}
	}
}

// ─────────────────────────────────────────────
// 3. 마이그레이션 도구 시뮬레이션
// ─────────────────────────────────────────────

// SyncRange는 마이그레이션 시간 범위이다.
type SyncRange struct {
	Number int
	From   int64
	To     int64
}

// Store는 청크 저장소를 시뮬레이션한다.
type Store struct {
	Name   string
	chunks map[string]*LokiChunk
	mu     sync.Mutex
}

func NewStore(name string) *Store {
	return &Store{Name: name, chunks: make(map[string]*LokiChunk)}
}

func (s *Store) GetChunks(userID string, from, to int64) []*LokiChunk {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []*LokiChunk
	for _, chunk := range s.chunks {
		if chunk.Header.UserID == userID &&
			chunk.Header.From <= to && chunk.Header.Through >= from {
			result = append(result, chunk)
		}
	}
	return result
}

func (s *Store) Put(chunks []*LokiChunk) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, chunk := range chunks {
		key := fmt.Sprintf("%s_%d_%d", chunk.Header.UserID, chunk.Header.Fingerprint, chunk.Header.From)
		s.chunks[key] = chunk
	}
	return nil
}

// MigrationStats는 마이그레이션 통계이다.
type MigrationStats struct {
	TotalChunks uint64
	TotalBytes  uint64
}

// ChunkMover는 청크 마이그레이션 엔진이다.
type ChunkMover struct {
	Source     *Store
	Dest      *Store
	SourceUID string
	DestUID   string
	BatchSize int
}

func (cm *ChunkMover) MoveChunks(sr SyncRange) (MigrationStats, error) {
	chunks := cm.Source.GetChunks(cm.SourceUID, sr.From, sr.To)
	var stats MigrationStats

	for i := 0; i < len(chunks); i += cm.BatchSize {
		end := i + cm.BatchSize
		if end > len(chunks) {
			end = len(chunks)
		}
		batch := chunks[i:end]

		// 테넌트 변경 시 재인코딩
		output := make([]*LokiChunk, 0, len(batch))
		for _, chk := range batch {
			if cm.SourceUID != cm.DestUID {
				// 새 청크 생성 (테넌트 변경)
				newChunk := *chk
				newChunk.Header.UserID = cm.DestUID
				output = append(output, &newChunk)
			} else {
				output = append(output, chk)
			}
			stats.TotalChunks++
			stats.TotalBytes += uint64(len(chk.Blocks[0].RawData))
		}

		if err := cm.Dest.Put(output); err != nil {
			return stats, err
		}
	}
	return stats, nil
}

// CalcSyncRanges는 시간 범위를 분할한다.
func CalcSyncRanges(from, to, shardBy int64) []SyncRange {
	var ranges []SyncRange
	currentFrom := from
	currentTo := from + shardBy
	number := 0

	for currentFrom < to && currentTo <= to {
		ranges = append(ranges, SyncRange{
			Number: number,
			From:   currentFrom,
			To:     currentTo,
		})
		number++
		currentFrom = currentTo + 1
		currentTo = currentTo + shardBy
		if currentTo > to {
			currentTo = to
		}
	}
	return ranges
}

// ByteCountDecimal은 바이트를 읽기 좋은 형식으로 변환한다.
func ByteCountDecimal(b uint64) string {
	const unit = 1000
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "kMGTPE"[exp])
}

// ─────────────────────────────────────────────
// 4. Loki Tool 커맨드 시뮬레이션
// ─────────────────────────────────────────────

// RuleCommand는 규칙 관리 커맨드이다.
type RuleCommand struct {
	Rules []AlertRule
}

type AlertRule struct {
	Name   string
	Expr   string
	For    string
	Labels map[string]string
}

func (rc *RuleCommand) Sync() {
	fmt.Printf("  규칙 동기화: %d개 규칙\n", len(rc.Rules))
	for _, r := range rc.Rules {
		fmt.Printf("    - %s: %s (for: %s)\n", r.Name, r.Expr, r.For)
	}
}

func (rc *RuleCommand) Diff(remote []AlertRule) {
	localMap := make(map[string]AlertRule)
	for _, r := range rc.Rules {
		localMap[r.Name] = r
	}
	remoteMap := make(map[string]AlertRule)
	for _, r := range remote {
		remoteMap[r.Name] = r
	}

	for name := range localMap {
		if _, ok := remoteMap[name]; !ok {
			fmt.Printf("    + 추가: %s\n", name)
		}
	}
	for name := range remoteMap {
		if _, ok := localMap[name]; !ok {
			fmt.Printf("    - 삭제: %s\n", name)
		}
	}
}

// ─────────────────────────────────────────────
// 메인 함수
// ─────────────────────────────────────────────

func main() {
	fmt.Println("╔══════════════════════════════════════════════════╗")
	fmt.Println("║  Loki 운영 도구 시뮬레이션                       ║")
	fmt.Println("║  (Chunks Inspector + Migration + Loki Tool)      ║")
	fmt.Println("╚══════════════════════════════════════════════════╝")
	fmt.Println()

	// === 1. 청크 생성 및 인코딩 ===
	fmt.Println("━━━ 1. 청크 바이너리 포맷 인코딩 ━━━")
	baseTime := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC).UnixNano()

	blocks := make([]Block, 3)
	for i := range blocks {
		entries := make([]LogEntry, 5)
		minT := baseTime + int64(i)*int64(20*time.Minute)
		maxT := minT + int64(19*time.Minute)
		for j := range entries {
			entries[j] = LogEntry{
				Timestamp: minT + int64(j)*int64(4*time.Minute),
				Line:      fmt.Sprintf("[INFO] Request processed in %dms", rand.Intn(100)),
			}
		}

		// 원본 데이터 시뮬레이션
		var origBuf bytes.Buffer
		for _, e := range entries {
			origBuf.WriteString(fmt.Sprintf("%d:%s\n", e.Timestamp, e.Line))
		}
		origData := origBuf.Bytes()

		// "압축" 시뮬레이션 (실제로는 원본의 60%로 줄임)
		compressedSize := len(origData) * 60 / 100
		rawData := make([]byte, compressedSize)
		copy(rawData, origData[:compressedSize])

		blocks[i] = Block{
			NumEntries:   uint64(len(entries)),
			MinT:         minT,
			MaxT:         maxT,
			Entries:      entries,
			RawData:      rawData,
			OriginalData: origData,
			Checksum:     crc32.Checksum(rawData, castagnoliTable),
		}
	}

	chunk := &LokiChunk{
		Header: ChunkHeader{
			Fingerprint: 12345678,
			UserID:      "tenant-1",
			From:        blocks[0].MinT,
			Through:     blocks[len(blocks)-1].MaxT,
			Labels:      map[string]string{"app": "web-server", "env": "production"},
			Encoding:    EncodingSnappy,
		},
		Format:   ChunkFormatV3,
		Encoding: EncodingSnappy,
		Blocks:   blocks,
	}

	encoded := EncodeChunk(chunk)
	fmt.Printf("  청크 인코딩 완료: %d 바이트\n", len(encoded))
	fmt.Printf("  매직 넘버: 0x%08X\n", MagicNumber)
	fmt.Printf("  포맷: v%d, 인코딩: %s\n", chunk.Format, encodingNames[chunk.Encoding])
	fmt.Printf("  블록 수: %d\n", len(blocks))
	fmt.Printf("  테넌트: %s\n", chunk.Header.UserID)
	fmt.Printf("  레이블: %v\n", chunk.Header.Labels)
	fmt.Printf("  메타데이터 체크섬: %08x\n", chunk.MetadataChecksum)
	fmt.Println()

	// === 2. 청크 검사 (Inspector) ===
	fmt.Println("━━━ 2. Chunks Inspector 시뮬레이션 ━━━")
	DecodeAndInspect(encoded, true, false)

	// 체크섬 무결성 테스트
	fmt.Println("\n  [무결성 테스트] 데이터 변조 후 검사:")
	corrupted := make([]byte, len(encoded))
	copy(corrupted, encoded)
	corrupted[10] ^= 0xFF // 데이터 1바이트 변조
	DecodeAndInspect(corrupted, true, false)
	fmt.Println()

	// === 3. 마이그레이션 도구 시뮬레이션 ===
	fmt.Println("━━━ 3. Migration Tool 시뮬레이션 ━━━")

	sourceStore := NewStore("source-s3")
	destStore := NewStore("dest-gcs")

	// 소스에 청크 추가
	for i := 0; i < 10; i++ {
		minT := baseTime + int64(i)*int64(time.Hour)
		maxT := minT + int64(time.Hour)
		c := &LokiChunk{
			Header: ChunkHeader{
				Fingerprint: uint64(1000 + i),
				UserID:      "tenant-old",
				From:        minT,
				Through:     maxT,
				Labels:      map[string]string{"app": "service"},
			},
			Blocks: []Block{{
				NumEntries: 100,
				MinT:       minT,
				MaxT:       maxT,
				RawData:    make([]byte, 1024+rand.Intn(4096)),
			}},
		}
		sourceStore.Put([]*LokiChunk{c})
	}

	// syncRange 계산
	fromNano := baseTime
	toNano := baseTime + int64(10*time.Hour)
	shardByNano := int64(3 * time.Hour)

	syncRanges := CalcSyncRanges(fromNano, toNano, shardByNano)
	fmt.Printf("  소스: %s (%d 청크)\n", sourceStore.Name, len(sourceStore.chunks))
	fmt.Printf("  대상: %s\n", destStore.Name)
	fmt.Printf("  시간 범위: %s ~ %s\n",
		time.Unix(0, fromNano).UTC().Format("15:04:05"),
		time.Unix(0, toNano).UTC().Format("15:04:05"))
	fmt.Printf("  샤드 단위: %v → %d개 범위\n", 3*time.Hour, len(syncRanges))

	// 병렬 마이그레이션
	mover := &ChunkMover{
		Source:    sourceStore,
		Dest:      destStore,
		SourceUID: "tenant-old",
		DestUID:   "tenant-new",
		BatchSize: 3,
	}

	var totalStats MigrationStats
	start := time.Now()

	var wg sync.WaitGroup
	statsCh := make(chan MigrationStats, len(syncRanges))

	for _, sr := range syncRanges {
		wg.Add(1)
		go func(sr SyncRange) {
			defer wg.Done()
			stats, err := mover.MoveChunks(sr)
			if err != nil {
				fmt.Printf("  워커: 범위 %d 오류: %v\n", sr.Number, err)
				return
			}
			statsCh <- stats
		}(sr)
	}

	go func() {
		wg.Wait()
		close(statsCh)
	}()

	for s := range statsCh {
		totalStats.TotalChunks += s.TotalChunks
		totalStats.TotalBytes += s.TotalBytes
	}

	elapsed := time.Since(start)
	fmt.Printf("  마이그레이션 완료:\n")
	fmt.Printf("    청크: %d개\n", totalStats.TotalChunks)
	fmt.Printf("    바이트: %s\n", ByteCountDecimal(totalStats.TotalBytes))
	fmt.Printf("    소요: %v\n", elapsed)
	fmt.Printf("    대상 청크 수: %d\n", len(destStore.chunks))
	fmt.Printf("    테넌트 변경: tenant-old → tenant-new\n")
	fmt.Println()

	// === 4. ByteCountDecimal 포맷팅 테스트 ===
	fmt.Println("━━━ 4. 바이트 크기 포맷팅 ━━━")
	testSizes := []uint64{0, 500, 1500, 1500000, 1500000000, 1500000000000}
	for _, size := range testSizes {
		fmt.Printf("  %15d → %s\n", size, ByteCountDecimal(size))
	}
	fmt.Println()

	// === 5. Loki Tool (규칙 관리) 시뮬레이션 ===
	fmt.Println("━━━ 5. Loki Tool 시뮬레이션 ━━━")

	rc := &RuleCommand{
		Rules: []AlertRule{
			{Name: "HighErrorRate", Expr: `rate({level="error"}[5m]) > 0.1`, For: "5m",
				Labels: map[string]string{"severity": "critical"}},
			{Name: "SlowQueries", Expr: `histogram_quantile(0.99, {job="loki"}) > 10`, For: "10m",
				Labels: map[string]string{"severity": "warning"}},
			{Name: "DiskUsage", Expr: `loki_disk_usage_bytes > 1e12`, For: "30m",
				Labels: map[string]string{"severity": "warning"}},
		},
	}

	fmt.Println("  [rules sync]")
	rc.Sync()

	remoteRules := []AlertRule{
		{Name: "HighErrorRate", Expr: `rate({level="error"}[5m]) > 0.1`},
		{Name: "OldRule", Expr: `{job="old"}`},
	}

	fmt.Println("\n  [rules diff]")
	rc.Diff(remoteRules)

	fmt.Println()

	// === 6. 인코딩 테이블 ===
	fmt.Println("━━━ 6. 지원 인코딩 목록 ━━━")
	fmt.Printf("  %-5s %-12s\n", "코드", "이름")
	fmt.Println("  " + strings.Repeat("─", 20))
	for code := 0; code <= 9; code++ {
		fmt.Printf("  %-5d %-12s\n", code, encodingNames[code])
	}

	fmt.Println()
	fmt.Println("시뮬레이션 완료.")
}
