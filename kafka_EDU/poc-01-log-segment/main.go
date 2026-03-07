package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Kafka LogSegment PoC
//
// 실제 Kafka 소스 참조:
//   - LogSegment.java: append-only 로그 세그먼트, indexIntervalBytes 기반 인덱스 엔트리 추가
//   - OffsetIndex.java: 8바이트 엔트리(상대 오프셋 4B + 물리 위치 4B), 이진 탐색 조회
//   - AbstractIndex.java: largestLowerBoundSlotFor() 이진 탐색 알고리즘
//
// 핵심 설계:
//   1) .log 파일: append-only, 레코드를 순차적으로 기록
//   2) .index 파일: sparse 인덱스, indexIntervalBytes마다 (offset, position) 엔트리 기록
//   3) 읽기: 인덱스에서 이진 탐색 -> 물리 위치 확인 -> 로그에서 순차 스캔
//   4) 세그먼트 롤링: maxSegmentBytes 초과 시 새 세그먼트 생성

const (
	indexEntrySize    = 8  // 4 bytes relative offset + 4 bytes physical position
	indexIntervalBytes = 64 // Kafka 기본값은 4096, PoC에서는 64로 축소
	maxSegmentBytes   = 512 // Kafka 기본값은 1GB, PoC에서는 512B로 축소
	recordHeaderSize  = 12  // 4B offset + 4B timestamp + 4B payload length
)

// Record는 Kafka의 단일 레코드를 나타낸다.
type Record struct {
	Offset    uint32
	Timestamp uint32
	Key       []byte
	Value     []byte
}

// IndexEntry는 OffsetIndex의 한 엔트리를 나타낸다.
// Kafka의 OffsetIndex.java: relativeOffset(4B) + physical position(4B) = 8B
type IndexEntry struct {
	RelativeOffset uint32 // offset - baseOffset
	Position       uint32 // .log 파일 내 물리적 바이트 위치
}

// LogSegment는 Kafka의 LogSegment.java를 시뮬레이션한다.
// 하나의 .log 파일과 .index 파일로 구성된다.
type LogSegment struct {
	baseOffset          uint32
	dir                 string
	logFile             *os.File
	indexFile           *os.File
	size                int // 현재 .log 파일 크기
	bytesSinceLastIndex int // 마지막 인덱스 엔트리 이후 누적 바이트
	indexEntries        []IndexEntry
	nextOffset          uint32
}

// Log는 여러 세그먼트로 구성된 전체 로그를 나타낸다.
type Log struct {
	dir      string
	segments []*LogSegment
	active   *LogSegment
}

func newLogSegment(dir string, baseOffset uint32) (*LogSegment, error) {
	logPath := filepath.Join(dir, fmt.Sprintf("%020d.log", baseOffset))
	indexPath := filepath.Join(dir, fmt.Sprintf("%020d.index", baseOffset))

	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	indexFile, err := os.Create(indexPath)
	if err != nil {
		return nil, err
	}

	return &LogSegment{
		baseOffset: baseOffset,
		dir:        dir,
		logFile:    logFile,
		indexFile:  indexFile,
		nextOffset: baseOffset,
	}, nil
}

// append는 LogSegment.java의 append() 메서드를 시뮬레이션한다.
// 핵심 로직:
//   1) 레코드를 .log 파일에 기록
//   2) bytesSinceLastIndexEntry > indexIntervalBytes이면 인덱스 엔트리 추가
//   3) Kafka 원본: offsetIndex().append(batchLastOffset, physicalPosition)
func (s *LogSegment) append(key, value []byte) (uint32, error) {
	offset := s.nextOffset
	physicalPosition := s.size

	// 레코드 인코딩: [offset:4B][timestamp:4B][payloadLen:4B][key...][value...]
	ts := uint32(time.Now().UnixMilli() & 0xFFFFFFFF)
	payloadLen := uint32(len(key) + len(value))
	header := make([]byte, recordHeaderSize)
	binary.BigEndian.PutUint32(header[0:4], offset)
	binary.BigEndian.PutUint32(header[4:8], ts)
	binary.BigEndian.PutUint32(header[8:12], payloadLen)

	if _, err := s.logFile.Write(header); err != nil {
		return 0, err
	}
	if _, err := s.logFile.Write(key); err != nil {
		return 0, err
	}
	if _, err := s.logFile.Write(value); err != nil {
		return 0, err
	}

	recordSize := recordHeaderSize + len(key) + len(value)
	s.size += recordSize
	s.bytesSinceLastIndex += recordSize

	// Kafka의 인덱스 간격 로직 (LogSegment.java:270-274):
	//   if (bytesSinceLastIndexEntry > indexIntervalBytes) {
	//       offsetIndex().append(batchLastOffset, physicalPosition);
	//       bytesSinceLastIndexEntry = 0;
	//   }
	if s.bytesSinceLastIndex > indexIntervalBytes {
		entry := IndexEntry{
			RelativeOffset: offset - s.baseOffset,
			Position:       uint32(physicalPosition),
		}
		s.indexEntries = append(s.indexEntries, entry)

		// .index 파일에 8바이트 엔트리 기록
		buf := make([]byte, indexEntrySize)
		binary.BigEndian.PutUint32(buf[0:4], entry.RelativeOffset)
		binary.BigEndian.PutUint32(buf[4:8], entry.Position)
		if _, err := s.indexFile.Write(buf); err != nil {
			return 0, err
		}
		s.bytesSinceLastIndex = 0
	}

	s.nextOffset = offset + 1
	return offset, nil
}

// lookup은 OffsetIndex.java의 lookup() + AbstractIndex.java의 binarySearch()를 시뮬레이션한다.
// 주어진 targetOffset 이하의 가장 큰 인덱스 엔트리를 이진 탐색으로 찾는다.
// Kafka 원본: largestLowerBoundSlotFor(idx, targetOffset, IndexSearchType.KEY)
func (s *LogSegment) lookup(targetOffset uint32) (position uint32, found bool) {
	if len(s.indexEntries) == 0 {
		return 0, false
	}

	targetRelative := targetOffset - s.baseOffset

	// 이진 탐색: AbstractIndex.java의 binarySearch() 구현과 동일한 로직
	// lo는 항상 target 이하, hi는 target 이상의 슬롯을 가리킨다
	idx := sort.Search(len(s.indexEntries), func(i int) bool {
		return s.indexEntries[i].RelativeOffset > targetRelative
	})

	if idx == 0 {
		// 모든 인덱스 엔트리가 target보다 크면 baseOffset 위치(0)에서 시작
		return 0, false
	}
	// idx-1이 target 이하의 가장 큰 엔트리
	return s.indexEntries[idx-1].Position, true
}

// readAt는 특정 offset의 레코드를 읽는다.
// Kafka의 LogSegment.read()와 동일한 흐름:
//   1) translateOffset: 인덱스에서 이진 탐색으로 물리 위치 확인
//   2) 해당 위치에서 순차 스캔하여 정확한 offset 레코드 찾기
func (s *LogSegment) readAt(targetOffset uint32) (*Record, error) {
	// 1단계: 인덱스 조회 (translateOffset)
	startPos, _ := s.lookup(targetOffset)

	// 2단계: .log 파일에서 순차 스캔
	file, err := os.Open(s.logFile.Name())
	if err != nil {
		return nil, err
	}
	defer file.Close()

	if _, err := file.Seek(int64(startPos), 0); err != nil {
		return nil, err
	}

	header := make([]byte, recordHeaderSize)
	for {
		n, err := file.Read(header)
		if n == 0 || err != nil {
			return nil, fmt.Errorf("offset %d not found in segment (base=%d)", targetOffset, s.baseOffset)
		}

		offset := binary.BigEndian.Uint32(header[0:4])
		timestamp := binary.BigEndian.Uint32(header[4:8])
		payloadLen := binary.BigEndian.Uint32(header[8:12])
		payload := make([]byte, payloadLen)
		if _, err := file.Read(payload); err != nil {
			return nil, err
		}

		if offset == targetOffset {
			// 이 PoC에서는 key=payload의 전반, value=payload의 후반으로 간주
			return &Record{
				Offset:    offset,
				Timestamp: timestamp,
				Value:     payload,
			}, nil
		}
		if offset > targetOffset {
			return nil, fmt.Errorf("offset %d not found (passed offset %d)", targetOffset, offset)
		}
	}
}

func (s *LogSegment) shouldRoll(additionalSize int) bool {
	return s.size+additionalSize > maxSegmentBytes
}

func (s *LogSegment) close() {
	s.logFile.Close()
	s.indexFile.Close()
}

// --- Log (여러 세그먼트 관리) ---

func newLog(dir string) (*Log, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	seg, err := newLogSegment(dir, 0)
	if err != nil {
		return nil, err
	}
	return &Log{
		dir:      dir,
		segments: []*LogSegment{seg},
		active:   seg,
	}, nil
}

// roll은 새 세그먼트를 생성한다.
// Kafka의 UnifiedLog.roll() -> LogSegment.open() 흐름을 시뮬레이션한다.
func (l *Log) roll() error {
	newBase := l.active.nextOffset
	seg, err := newLogSegment(l.dir, newBase)
	if err != nil {
		return err
	}
	fmt.Printf("  [ROLL] 세그먼트 롤링: 이전 base=%d (size=%dB) -> 새 base=%d\n",
		l.active.baseOffset, l.active.size, newBase)
	l.segments = append(l.segments, seg)
	l.active = seg
	return nil
}

func (l *Log) append(key, value []byte) (uint32, error) {
	recordSize := recordHeaderSize + len(key) + len(value)

	// LogSegment.shouldRoll() 확인
	if l.active.shouldRoll(recordSize) {
		if err := l.roll(); err != nil {
			return 0, err
		}
	}
	return l.active.append(key, value)
}

// findSegment는 주어진 offset이 속하는 세그먼트를 찾는다.
func (l *Log) findSegment(offset uint32) *LogSegment {
	// 세그먼트는 baseOffset 순으로 정렬되어 있다
	for i := len(l.segments) - 1; i >= 0; i-- {
		if l.segments[i].baseOffset <= offset {
			return l.segments[i]
		}
	}
	return l.segments[0]
}

func (l *Log) read(offset uint32) (*Record, error) {
	seg := l.findSegment(offset)
	return seg.readAt(offset)
}

func (l *Log) close() {
	for _, seg := range l.segments {
		seg.close()
	}
}

func main() {
	fmt.Println("========================================")
	fmt.Println(" Kafka Log Segment PoC")
	fmt.Println(" Based on: LogSegment.java, OffsetIndex.java, AbstractIndex.java")
	fmt.Println("========================================")
	fmt.Println()

	dir := filepath.Join(os.TempDir(), "kafka-log-segment-poc")
	os.RemoveAll(dir)

	log, err := newLog(dir)
	if err != nil {
		fmt.Printf("ERROR: %v\n", err)
		return
	}
	defer func() {
		log.close()
		os.RemoveAll(dir)
	}()

	// --- 1. 레코드 추가 (세그먼트 롤링 포함) ---
	fmt.Println("[1] 레코드 추가 (30개, maxSegmentBytes=512)")
	fmt.Println("    indexIntervalBytes=64 -> 약 2-3 레코드마다 인덱스 엔트리 생성")
	fmt.Println()

	for i := 0; i < 30; i++ {
		key := []byte(fmt.Sprintf("key-%03d", i))
		value := []byte(fmt.Sprintf("value-for-record-%03d-with-some-padding", i))
		offset, err := log.append(key, value)
		if err != nil {
			fmt.Printf("  ERROR appending: %v\n", err)
			return
		}
		if i%10 == 0 || i == 29 {
			fmt.Printf("  offset=%d, segment.base=%d, segment.size=%dB\n",
				offset, log.active.baseOffset, log.active.size)
		}
	}

	// --- 2. 세그먼트 현황 ---
	fmt.Println()
	fmt.Println("[2] 세그먼트 현황")
	for _, seg := range log.segments {
		fmt.Printf("  세그먼트 base=%d, size=%dB, indexEntries=%d, offsets=[%d..%d)\n",
			seg.baseOffset, seg.size, len(seg.indexEntries),
			seg.baseOffset, seg.nextOffset)
		for j, entry := range seg.indexEntries {
			fmt.Printf("    index[%d]: relOffset=%d (abs=%d) -> position=%d\n",
				j, entry.RelativeOffset, seg.baseOffset+entry.RelativeOffset, entry.Position)
		}
	}

	// --- 3. 파일 시스템 확인 ---
	fmt.Println()
	fmt.Println("[3] 파일 시스템 (.log + .index 파일)")
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		info, _ := e.Info()
		fmt.Printf("  %s (%d bytes)\n", e.Name(), info.Size())
	}

	// --- 4. 오프셋으로 읽기 (이진 탐색 + 순차 스캔) ---
	fmt.Println()
	fmt.Println("[4] 오프셋으로 읽기 (인덱스 이진 탐색 -> 순차 스캔)")
	testOffsets := []uint32{0, 5, 10, 15, 20, 25, 29}
	for _, off := range testOffsets {
		rec, err := log.read(off)
		if err != nil {
			fmt.Printf("  offset=%d -> ERROR: %v\n", off, err)
			continue
		}
		seg := log.findSegment(off)
		lookupPos, hasIndex := seg.lookup(off)
		fmt.Printf("  offset=%d -> segment.base=%d, indexLookup=%d(found=%v), value=%q\n",
			off, seg.baseOffset, lookupPos, hasIndex, string(rec.Value))
	}

	// --- 5. 인덱스 이진 탐색 상세 ---
	fmt.Println()
	fmt.Println("[5] 인덱스 이진 탐색 상세 (첫 번째 세그먼트)")
	if len(log.segments) > 0 {
		seg := log.segments[0]
		fmt.Printf("  세그먼트 base=%d, 인덱스 엔트리 수=%d\n", seg.baseOffset, len(seg.indexEntries))
		for _, target := range []uint32{0, 3, 5, 7, 9} {
			pos, found := seg.lookup(target)
			fmt.Printf("  lookup(offset=%d): position=%d, found=%v\n", target, pos, found)
		}
	}

	// --- 6. 설계 요약 ---
	fmt.Println()
	fmt.Println("[6] Kafka Log Segment 설계 요약")
	fmt.Println("  +---------------------+     +-------------------+")
	fmt.Println("  |   .index (sparse)    |     |    .log (data)    |")
	fmt.Println("  |---------------------|     |-------------------|")
	fmt.Println("  | relOff=2, pos=0     |---->| [off=0][ts][data] |")
	fmt.Println("  | relOff=5, pos=192   |---->| [off=1][ts][data] |")
	fmt.Println("  | relOff=8, pos=384   |---->| [off=2][ts][data] |")
	fmt.Println("  |                     |     |       ...         |")
	fmt.Println("  +---------------------+     +-------------------+")
	fmt.Println()
	fmt.Println("  읽기 흐름 (LogSegment.translateOffset):")
	fmt.Println("  1) 인덱스에서 이진 탐색 -> target 이하의 가장 큰 엔트리의 물리 위치")
	fmt.Println("  2) 해당 위치에서 .log 파일 순차 스캔 -> 정확한 offset 레코드 찾기")
	fmt.Println()
	fmt.Println("  쓰기 흐름 (LogSegment.append):")
	fmt.Println("  1) .log 파일에 append-only 기록")
	fmt.Println("  2) bytesSinceLastIndexEntry > indexIntervalBytes이면 인덱스에 엔트리 추가")
	fmt.Println("  3) size > maxSegmentBytes이면 새 세그먼트로 롤링")
}
