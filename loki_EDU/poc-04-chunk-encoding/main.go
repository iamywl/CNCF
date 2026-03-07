package main

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"strings"
	"time"
)

// =============================================================================
// Loki 청크 인코딩 시뮬레이션
// =============================================================================
//
// Loki는 로그 데이터를 "청크(Chunk)"라는 단위로 묶어 저장한다.
// 각 청크는 여러 개의 "블록(Block)"으로 구성되며, 각 블록은 압축된 로그 데이터를 담는다.
//
// 청크 구조:
//   ┌─────────────────────────────────┐
//   │          Chunk Header           │
//   │  (매직넘버, 버전, 인코딩 타입)     │
//   ├─────────────────────────────────┤
//   │         Block 1 (gzip)          │
//   │  ┌──────────┬──────────┐       │
//   │  │ entries  │ checksum │       │
//   │  └──────────┴──────────┘       │
//   ├─────────────────────────────────┤
//   │         Block 2 (gzip)          │
//   ├─────────────────────────────────┤
//   │         ...                     │
//   ├─────────────────────────────────┤
//   │         Block N (gzip)          │
//   ├─────────────────────────────────┤
//   │     Block Metadata Section      │
//   │  (각 블록의 오프셋, 크기, 엔트리 수) │
//   ├─────────────────────────────────┤
//   │          Chunk Footer           │
//   │  (메타 오프셋, 전체 체크섬)        │
//   └─────────────────────────────────┘
//
// Loki 실제 구현 참조:
//   - pkg/chunkenc/memchunk.go: MemChunk (메모리 내 청크)
//   - pkg/chunkenc/dumb_chunk.go: 단순 청크 (참고용)
//   - pkg/chunkenc/interface.go: Chunk 인터페이스 정의
// =============================================================================

const (
	// 매직 넘버: 청크 파일의 시작을 식별
	ChunkMagicNumber uint32 = 0x4C4F4B49 // "LOKI" in ASCII

	// 청크 포맷 버전
	ChunkFormatV1 byte = 1

	// 인코딩 타입
	EncodingNone byte = 0
	EncodingGzip byte = 1

	// 블록 크기 제한: head block이 이 크기를 넘으면 압축하여 block으로 전환
	DefaultBlockSize = 256 * 1024 // 256KB
)

// LogEntry는 하나의 로그 엔트리이다.
type LogEntry struct {
	Timestamp time.Time
	Line      string
}

// Block은 압축된 로그 블록을 나타낸다.
// head block에서 크기 임계값에 도달하면 압축되어 Block이 된다.
type Block struct {
	Data         []byte // 압축된 데이터
	RawSize      int    // 압축 전 크기
	NumEntries   int    // 엔트리 수
	MinTimestamp int64  // 블록 내 최소 타임스탬프 (나노초)
	MaxTimestamp int64  // 블록 내 최대 타임스탬프 (나노초)
	Checksum     uint32 // CRC32 체크섬
}

// HeadBlock은 아직 압축되지 않은 현재 쓰기 중인 블록이다.
// 엔트리가 추가될 때마다 이 블록에 먼저 쓰고,
// 크기 임계값을 넘으면 압축하여 Block으로 전환(cut)한다.
type HeadBlock struct {
	entries []LogEntry
	size    int // 현재 크기 (바이트 추정)
	mint    int64
	maxt    int64
}

// NewHeadBlock은 새 HeadBlock을 생성한다.
func NewHeadBlock() *HeadBlock {
	return &HeadBlock{}
}

// Append는 엔트리를 head block에 추가한다.
func (hb *HeadBlock) Append(ts time.Time, line string) {
	nsec := ts.UnixNano()
	if len(hb.entries) == 0 {
		hb.mint = nsec
		hb.maxt = nsec
	}
	if nsec < hb.mint {
		hb.mint = nsec
	}
	if nsec > hb.maxt {
		hb.maxt = nsec
	}

	hb.entries = append(hb.entries, LogEntry{Timestamp: ts, Line: line})
	// 크기 추정: 타임스탬프(8바이트) + 라인 길이(4바이트) + 라인 데이터
	hb.size += 8 + 4 + len(line)
}

// Compress는 head block의 엔트리를 gzip 압축하여 Block을 생성한다.
// Loki의 memchunk.go에서 headBlock.serialise()와 동일한 역할이다.
func (hb *HeadBlock) Compress() (*Block, error) {
	if len(hb.entries) == 0 {
		return nil, fmt.Errorf("head block이 비어있음")
	}

	// 원시 데이터 직렬화: [timestamp(8) | lineLen(4) | lineData(N)] ...
	var rawBuf bytes.Buffer
	for _, entry := range hb.entries {
		// 타임스탬프 (int64, 나노초)
		if err := binary.Write(&rawBuf, binary.BigEndian, entry.Timestamp.UnixNano()); err != nil {
			return nil, err
		}
		// 라인 길이
		lineBytes := []byte(entry.Line)
		if err := binary.Write(&rawBuf, binary.BigEndian, uint32(len(lineBytes))); err != nil {
			return nil, err
		}
		// 라인 데이터
		rawBuf.Write(lineBytes)
	}
	rawSize := rawBuf.Len()

	// gzip 압축
	var compBuf bytes.Buffer
	gzWriter := gzip.NewWriter(&compBuf)
	if _, err := gzWriter.Write(rawBuf.Bytes()); err != nil {
		return nil, err
	}
	if err := gzWriter.Close(); err != nil {
		return nil, err
	}

	compData := compBuf.Bytes()

	// CRC32 체크섬 계산
	checksum := crc32.ChecksumIEEE(compData)

	return &Block{
		Data:         compData,
		RawSize:      rawSize,
		NumEntries:   len(hb.entries),
		MinTimestamp: hb.mint,
		MaxTimestamp: hb.maxt,
		Checksum:     checksum,
	}, nil
}

// Reset은 head block을 초기화한다.
func (hb *HeadBlock) Reset() {
	hb.entries = hb.entries[:0]
	hb.size = 0
	hb.mint = 0
	hb.maxt = 0
}

// Chunk는 여러 블록으로 구성된 로그 청크이다.
// Loki의 pkg/chunkenc/memchunk.go의 MemChunk에 해당한다.
type Chunk struct {
	blocks       []*Block
	head         *HeadBlock
	encoding     byte
	blockSize    int   // head block → block 전환 임계값
	totalEntries int
}

// NewChunk는 새 Chunk를 생성한다.
func NewChunk(encoding byte, blockSize int) *Chunk {
	return &Chunk{
		head:      NewHeadBlock(),
		encoding:  encoding,
		blockSize: blockSize,
	}
}

// Append는 엔트리를 청크에 추가한다.
// head block에 추가하고, 크기 임계값을 넘으면 압축하여 block으로 전환한다.
func (c *Chunk) Append(ts time.Time, line string) error {
	c.head.Append(ts, line)
	c.totalEntries++

	// head block 크기가 임계값을 넘으면 블록으로 전환 (cut)
	if c.head.size >= c.blockSize {
		if err := c.cutBlock(); err != nil {
			return err
		}
	}

	return nil
}

// cutBlock은 현재 head block을 압축하여 block 목록에 추가한다.
// Loki의 MemChunk.cut()과 동일한 동작이다.
func (c *Chunk) cutBlock() error {
	block, err := c.head.Compress()
	if err != nil {
		return err
	}
	c.blocks = append(c.blocks, block)
	c.head.Reset()
	return nil
}

// Serialize는 청크 전체를 바이트 슬라이스로 직렬화한다.
// 실제 Loki에서는 이 형태로 Object Storage에 저장된다.
func (c *Chunk) Serialize() ([]byte, error) {
	// 먼저 남은 head block을 flush
	if len(c.head.entries) > 0 {
		if err := c.cutBlock(); err != nil {
			return nil, err
		}
	}

	var buf bytes.Buffer

	// ── Chunk Header ──
	// 매직 넘버 (4바이트)
	binary.Write(&buf, binary.BigEndian, ChunkMagicNumber)
	// 버전 (1바이트)
	buf.WriteByte(ChunkFormatV1)
	// 인코딩 타입 (1바이트)
	buf.WriteByte(c.encoding)

	headerSize := buf.Len()

	// ── Block Data ──
	type blockMeta struct {
		offset    uint32
		length    uint32
		entries   uint32
		mint      int64
		maxt      int64
		checksum  uint32
	}
	metas := make([]blockMeta, len(c.blocks))

	for i, block := range c.blocks {
		offset := uint32(buf.Len())
		buf.Write(block.Data)

		metas[i] = blockMeta{
			offset:   offset,
			length:   uint32(len(block.Data)),
			entries:  uint32(block.NumEntries),
			mint:     block.MinTimestamp,
			maxt:     block.MaxTimestamp,
			checksum: block.Checksum,
		}
	}

	// ── Block Metadata Section ──
	metaOffset := uint32(buf.Len())

	// 블록 수 (4바이트)
	binary.Write(&buf, binary.BigEndian, uint32(len(c.blocks)))

	// 각 블록의 메타데이터
	for _, meta := range metas {
		binary.Write(&buf, binary.BigEndian, meta.offset)
		binary.Write(&buf, binary.BigEndian, meta.length)
		binary.Write(&buf, binary.BigEndian, meta.entries)
		binary.Write(&buf, binary.BigEndian, meta.mint)
		binary.Write(&buf, binary.BigEndian, meta.maxt)
		binary.Write(&buf, binary.BigEndian, meta.checksum)
	}

	// ── Chunk Footer ──
	// 메타데이터 섹션 오프셋 (4바이트)
	binary.Write(&buf, binary.BigEndian, metaOffset)
	// 전체 체크섬 (4바이트)
	data := buf.Bytes()
	totalChecksum := crc32.ChecksumIEEE(data)
	binary.Write(&buf, binary.BigEndian, totalChecksum)

	_ = headerSize // 컴파일러 경고 방지
	return buf.Bytes(), nil
}

// Deserialize는 바이트 슬라이스에서 청크를 역직렬화한다.
func Deserialize(data []byte) (*DeserializedChunk, error) {
	if len(data) < 14 { // 최소 크기: header(6) + footer(8)
		return nil, fmt.Errorf("데이터 크기가 너무 작음: %d바이트", len(data))
	}

	// ── Footer 읽기 (마지막 8바이트) ──
	storedChecksum := binary.BigEndian.Uint32(data[len(data)-4:])
	metaOffset := binary.BigEndian.Uint32(data[len(data)-8 : len(data)-4])

	// 체크섬 검증 (체크섬 자체를 제외한 데이터)
	calculatedChecksum := crc32.ChecksumIEEE(data[:len(data)-4])
	if storedChecksum != calculatedChecksum {
		return nil, fmt.Errorf("체크섬 불일치: 저장=%08X, 계산=%08X", storedChecksum, calculatedChecksum)
	}

	// ── Header 읽기 ──
	magic := binary.BigEndian.Uint32(data[:4])
	if magic != ChunkMagicNumber {
		return nil, fmt.Errorf("잘못된 매직 넘버: %08X (예상: %08X)", magic, ChunkMagicNumber)
	}
	version := data[4]
	encoding := data[5]

	// ── Metadata Section 읽기 ──
	reader := bytes.NewReader(data[metaOffset : len(data)-8])
	var numBlocks uint32
	binary.Read(reader, binary.BigEndian, &numBlocks)

	result := &DeserializedChunk{
		Version:       version,
		Encoding:      encoding,
		NumBlocks:     int(numBlocks),
		TotalSize:     len(data),
		Checksum:      storedChecksum,
		ChecksumValid: true,
	}

	for i := 0; i < int(numBlocks); i++ {
		var meta struct {
			Offset   uint32
			Length   uint32
			Entries  uint32
			MinT     int64
			MaxT     int64
			Checksum uint32
		}
		binary.Read(reader, binary.BigEndian, &meta.Offset)
		binary.Read(reader, binary.BigEndian, &meta.Length)
		binary.Read(reader, binary.BigEndian, &meta.Entries)
		binary.Read(reader, binary.BigEndian, &meta.MinT)
		binary.Read(reader, binary.BigEndian, &meta.MaxT)
		binary.Read(reader, binary.BigEndian, &meta.Checksum)

		// 블록 데이터 추출 및 체크섬 검증
		blockData := data[meta.Offset : meta.Offset+meta.Length]
		blockChecksum := crc32.ChecksumIEEE(blockData)
		checksumOK := blockChecksum == meta.Checksum

		// gzip 해제
		gzReader, err := gzip.NewReader(bytes.NewReader(blockData))
		if err != nil {
			return nil, fmt.Errorf("블록 %d gzip 열기 실패: %w", i, err)
		}
		rawData, err := io.ReadAll(gzReader)
		gzReader.Close()
		if err != nil {
			return nil, fmt.Errorf("블록 %d gzip 읽기 실패: %w", i, err)
		}

		// 엔트리 역직렬화
		var entries []LogEntry
		entryReader := bytes.NewReader(rawData)
		for j := 0; j < int(meta.Entries); j++ {
			var ts int64
			binary.Read(entryReader, binary.BigEndian, &ts)
			var lineLen uint32
			binary.Read(entryReader, binary.BigEndian, &lineLen)
			lineData := make([]byte, lineLen)
			entryReader.Read(lineData)
			entries = append(entries, LogEntry{
				Timestamp: time.Unix(0, ts),
				Line:      string(lineData),
			})
		}

		result.Blocks = append(result.Blocks, DeserializedBlock{
			Index:         i,
			CompressedSize: int(meta.Length),
			RawSize:       len(rawData),
			NumEntries:    int(meta.Entries),
			MinTimestamp:  time.Unix(0, meta.MinT),
			MaxTimestamp:  time.Unix(0, meta.MaxT),
			ChecksumOK:   checksumOK,
			Entries:       entries,
		})
	}

	return result, nil
}

// DeserializedChunk는 역직렬화된 청크이다.
type DeserializedChunk struct {
	Version       byte
	Encoding      byte
	NumBlocks     int
	TotalSize     int
	Checksum      uint32
	ChecksumValid bool
	Blocks        []DeserializedBlock
}

// DeserializedBlock은 역직렬화된 블록이다.
type DeserializedBlock struct {
	Index          int
	CompressedSize int
	RawSize        int
	NumEntries     int
	MinTimestamp    time.Time
	MaxTimestamp    time.Time
	ChecksumOK     bool
	Entries        []LogEntry
}

func main() {
	fmt.Println("=================================================================")
	fmt.Println("  Loki 청크 인코딩 시뮬레이션")
	fmt.Println("  - Head Block → 압축 Block 전환")
	fmt.Println("  - 바이너리 직렬화 + CRC32 체크섬")
	fmt.Println("  - 역직렬화 및 무결성 검증")
	fmt.Println("=================================================================")
	fmt.Println()

	// =========================================================================
	// 시나리오 1: 청크 생성 및 블록 전환
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println(" 시나리오 1: 청크 생성 및 블록 전환")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	// 블록 크기 256바이트 (데모용으로 작게 설정)
	chunk := NewChunk(EncodingGzip, 256)

	baseTime := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	// 로그 엔트리 생성 (충분히 많이 넣어서 여러 블록이 생기게)
	logLines := []string{
		`level=info msg="GET /api/v1/users 200 OK" duration=12ms ip=10.0.1.5`,
		`level=info msg="POST /api/v1/orders 201 Created" duration=45ms ip=10.0.1.8`,
		`level=warn msg="slow query detected" duration=2.3s query="SELECT * FROM orders"`,
		`level=error msg="connection refused" host=db-primary port=5432 retries=3`,
		`level=info msg="cache hit" key=user:1234 ttl=300s`,
		`level=debug msg="request headers" content-type=application/json accept=*/*`,
		`level=info msg="health check passed" component=api uptime=3600s`,
		`level=error msg="out of memory" heap_used=3.8GB heap_limit=4GB gc_runs=127`,
		`level=info msg="batch job completed" items=15000 duration=12.4s errors=2`,
		`level=warn msg="certificate expiring soon" domain=api.example.com days_left=7`,
		`level=info msg="new deployment started" version=v2.3.1 replicas=3`,
		`level=info msg="metrics scraped" series=4521 samples=89034 duration=1.2s`,
		`level=error msg="rate limit exceeded" tenant=org-123 limit=1000 current=1247`,
		`level=info msg="compaction finished" table=index_19234 duration=45s size=2.1GB`,
		`level=debug msg="ring token acquired" token=0x4A3B2C1D ingester=ingester-5`,
		`level=info msg="chunk flushed" stream=abc123 entries=5000 size=1.2MB`,
	}

	fmt.Println("  로그 엔트리 추가 중...")
	fmt.Println()
	for i, line := range logLines {
		ts := baseTime.Add(time.Duration(i) * time.Second)
		if err := chunk.Append(ts, line); err != nil {
			fmt.Printf("  에러: %v\n", err)
		}

		// 블록 전환 발생 여부 확인
		if len(chunk.blocks) > 0 {
			lastBlock := chunk.blocks[len(chunk.blocks)-1]
			_ = lastBlock
		}
	}

	fmt.Printf("  총 엔트리: %d\n", chunk.totalEntries)
	fmt.Printf("  블록 수: %d (cut된 블록) + head block\n", len(chunk.blocks))
	fmt.Printf("  Head block 엔트리: %d, 크기: ~%d 바이트\n",
		len(chunk.head.entries), chunk.head.size)
	fmt.Println()

	// 블록별 상세 정보
	for i, block := range chunk.blocks {
		compressionRatio := float64(block.RawSize) / float64(len(block.Data))
		fmt.Printf("  블록 %d:\n", i)
		fmt.Printf("    엔트리 수: %d\n", block.NumEntries)
		fmt.Printf("    원시 크기: %d 바이트\n", block.RawSize)
		fmt.Printf("    압축 크기: %d 바이트\n", len(block.Data))
		fmt.Printf("    압축률: %.1fx (%.1f%% 절감)\n", compressionRatio,
			(1-float64(len(block.Data))/float64(block.RawSize))*100)
		fmt.Printf("    CRC32: 0x%08X\n", block.Checksum)
		fmt.Printf("    시간 범위: %s ~ %s\n",
			time.Unix(0, block.MinTimestamp).Format("15:04:05"),
			time.Unix(0, block.MaxTimestamp).Format("15:04:05"))
		fmt.Println()
	}

	// =========================================================================
	// 시나리오 2: 직렬화 및 바이너리 포맷 분석
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println(" 시나리오 2: 직렬화 및 바이너리 포맷 분석")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	serialized, err := chunk.Serialize()
	if err != nil {
		fmt.Printf("  직렬화 에러: %v\n", err)
		return
	}

	fmt.Printf("  직렬화 결과:\n")
	fmt.Printf("    전체 크기: %d 바이트\n", len(serialized))
	fmt.Println()

	// 바이너리 헤더 분석
	magic := binary.BigEndian.Uint32(serialized[:4])
	version := serialized[4]
	encoding := serialized[5]

	fmt.Printf("  청크 헤더:\n")
	fmt.Printf("    매직 넘버: 0x%08X", magic)
	if magic == ChunkMagicNumber {
		fmt.Printf(" (\"%c%c%c%c\" = LOKI)\n",
			serialized[0], serialized[1], serialized[2], serialized[3])
	} else {
		fmt.Println(" (알 수 없음)")
	}
	fmt.Printf("    버전: %d\n", version)
	fmt.Printf("    인코딩: %d", encoding)
	switch encoding {
	case EncodingNone:
		fmt.Println(" (무압축)")
	case EncodingGzip:
		fmt.Println(" (gzip)")
	}
	fmt.Println()

	// 바이너리 덤프 (헤더 부분만)
	fmt.Println("  바이너리 덤프 (처음 64바이트):")
	dumpHex(serialized, 64)
	fmt.Println()

	// Footer 분석
	footerChecksum := binary.BigEndian.Uint32(serialized[len(serialized)-4:])
	footerMetaOffset := binary.BigEndian.Uint32(serialized[len(serialized)-8 : len(serialized)-4])
	fmt.Printf("  청크 풋터:\n")
	fmt.Printf("    메타데이터 오프셋: %d\n", footerMetaOffset)
	fmt.Printf("    전체 체크섬: 0x%08X\n", footerChecksum)
	fmt.Println()

	// =========================================================================
	// 시나리오 3: 역직렬화 및 무결성 검증
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println(" 시나리오 3: 역직렬화 및 무결성 검증")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	deserialized, err := Deserialize(serialized)
	if err != nil {
		fmt.Printf("  역직렬화 에러: %v\n", err)
		return
	}

	fmt.Printf("  역직렬화 결과:\n")
	fmt.Printf("    버전: %d\n", deserialized.Version)
	fmt.Printf("    인코딩: %d (gzip)\n", deserialized.Encoding)
	fmt.Printf("    블록 수: %d\n", deserialized.NumBlocks)
	fmt.Printf("    전체 크기: %d 바이트\n", deserialized.TotalSize)
	fmt.Printf("    체크섬 검증: %v\n", statusStr(deserialized.ChecksumValid))
	fmt.Println()

	totalEntries := 0
	totalRaw := 0
	totalCompressed := 0
	for _, block := range deserialized.Blocks {
		totalEntries += block.NumEntries
		totalRaw += block.RawSize
		totalCompressed += block.CompressedSize
		fmt.Printf("  블록 %d:\n", block.Index)
		fmt.Printf("    엔트리 수: %d\n", block.NumEntries)
		fmt.Printf("    압축 크기: %d → 원시 크기: %d (%.1fx)\n",
			block.CompressedSize, block.RawSize,
			float64(block.RawSize)/float64(block.CompressedSize))
		fmt.Printf("    시간 범위: %s ~ %s\n",
			block.MinTimestamp.Format("15:04:05"),
			block.MaxTimestamp.Format("15:04:05"))
		fmt.Printf("    체크섬: %v\n", statusStr(block.ChecksumOK))

		// 처음 2개 엔트리 미리보기
		previewCount := 2
		if previewCount > block.NumEntries {
			previewCount = block.NumEntries
		}
		for j := 0; j < previewCount; j++ {
			entry := block.Entries[j]
			line := entry.Line
			if len(line) > 60 {
				line = line[:60] + "..."
			}
			fmt.Printf("    [%s] %s\n", entry.Timestamp.Format("15:04:05"), line)
		}
		if block.NumEntries > previewCount {
			fmt.Printf("    ... (외 %d개)\n", block.NumEntries-previewCount)
		}
		fmt.Println()
	}

	// =========================================================================
	// 시나리오 4: 압축 효율 분석
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println(" 시나리오 4: 압축 효율 분석")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	fmt.Println("  ┌──────────────────────┬──────────────┐")
	fmt.Println("  │ 항목                 │ 크기         │")
	fmt.Println("  ├──────────────────────┼──────────────┤")
	fmt.Printf("  │ 총 엔트리 수         │ %12d │\n", totalEntries)
	fmt.Printf("  │ 원시 데이터          │ %8d B   │\n", totalRaw)
	fmt.Printf("  │ 압축 데이터          │ %8d B   │\n", totalCompressed)
	fmt.Printf("  │ 헤더+메타+풋터       │ %8d B   │\n", len(serialized)-totalCompressed)
	fmt.Printf("  │ 전체 직렬화 크기     │ %8d B   │\n", len(serialized))
	fmt.Println("  ├──────────────────────┼──────────────┤")
	fmt.Printf("  │ 압축률               │ %10.1fx  │\n", float64(totalRaw)/float64(totalCompressed))
	fmt.Printf("  │ 공간 절감            │ %9.1f%%   │\n", (1-float64(totalCompressed)/float64(totalRaw))*100)
	fmt.Println("  └──────────────────────┴──────────────┘")

	// =========================================================================
	// 시나리오 5: 체크섬 변조 탐지
	// =========================================================================
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println(" 시나리오 5: 체크섬 변조 탐지")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	// 데이터 변조 시뮬레이션
	corrupted := make([]byte, len(serialized))
	copy(corrupted, serialized)
	corrupted[10] ^= 0xFF // 10번째 바이트 변조

	_, err = Deserialize(corrupted)
	if err != nil {
		fmt.Printf("  변조 탐지 성공: %v\n", err)
	} else {
		fmt.Println("  변조 탐지 실패 (예상치 못한 결과)")
	}

	fmt.Println()
	fmt.Println("=================================================================")
	fmt.Println("  시뮬레이션 완료")
	fmt.Println()
	fmt.Println("  청크 구조:")
	fmt.Println("  ┌───────────────────────────────┐")
	fmt.Println("  │ Header: Magic(4)+Ver(1)+Enc(1) │")
	fmt.Println("  ├───────────────────────────────┤")
	fmt.Println("  │ Block 0 (gzip compressed)      │")
	fmt.Println("  │ Block 1 (gzip compressed)      │")
	fmt.Println("  │ ...                             │")
	fmt.Println("  ├───────────────────────────────┤")
	fmt.Println("  │ Metadata (offsets, sizes, CRC)  │")
	fmt.Println("  ├───────────────────────────────┤")
	fmt.Println("  │ Footer: MetaOffset(4)+CRC32(4) │")
	fmt.Println("  └───────────────────────────────┘")
	fmt.Println()
	fmt.Println("  핵심 포인트:")
	fmt.Println("  1. Head Block에 쓰다가 임계값 초과 시 gzip 압축하여 Block 전환")
	fmt.Println("  2. 각 Block별 CRC32 체크섬으로 개별 무결성 검증")
	fmt.Println("  3. 전체 청크에 대한 CRC32 체크섬으로 전송/저장 오류 탐지")
	fmt.Println("  4. 메타데이터 섹션으로 개별 블록에 O(1) 접근 가능")
	fmt.Println("  5. gzip 압축으로 로그 데이터 50~80% 공간 절감")
	fmt.Println("=================================================================")
}

// dumpHex는 바이트 슬라이스를 16진수 덤프로 출력한다.
func dumpHex(data []byte, maxBytes int) {
	if maxBytes > len(data) {
		maxBytes = len(data)
	}
	for i := 0; i < maxBytes; i += 16 {
		fmt.Printf("    %04X: ", i)
		end := i + 16
		if end > maxBytes {
			end = maxBytes
		}

		// 16진수 부분
		for j := i; j < end; j++ {
			fmt.Printf("%02X ", data[j])
		}
		// 패딩
		for j := end; j < i+16; j++ {
			fmt.Print("   ")
		}

		// ASCII 부분
		fmt.Print(" |")
		for j := i; j < end; j++ {
			if data[j] >= 32 && data[j] < 127 {
				fmt.Printf("%c", data[j])
			} else {
				fmt.Print(".")
			}
		}
		fmt.Println("|")
	}
}

// statusStr은 bool을 상태 문자열로 변환한다.
func statusStr(ok bool) string {
	if ok {
		return "통과"
	}
	return strings.ToUpper("실패")
}
