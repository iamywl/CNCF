package main

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Loki PoC #10: WAL - Write-Ahead Log 기반 내구성 보장
// =============================================================================
//
// Loki의 Ingester는 메모리에 로그를 버퍼링한 뒤, 주기적으로 청크로 플러시한다.
// 이 과정에서 프로세스가 비정상 종료되면 메모리의 데이터가 손실된다.
// WAL(Write-Ahead Log)은 이 문제를 해결한다:
//   1. 모든 쓰기 요청을 먼저 WAL 세그먼트 파일에 순차 기록
//   2. 메모리에 데이터 적용
//   3. 장애 시 WAL 세그먼트를 리플레이하여 복구
//
// 핵심 구성 요소:
//   - Segment: 순차 쓰기 전용 파일 (일정 크기 초과 시 새 세그먼트)
//   - Record: WAL에 기록되는 단위 (시리즈 정보 + 로그 엔트리)
//   - Checkpoint: 주기적 전체 스냅샷 (복구 시 시작점)
//   - Recovery: Checkpoint 로드 + 이후 세그먼트 리플레이
//
// 실제 Loki 코드: pkg/ingester/wal/, pkg/ingester/checkpoint.go
//
// 실행: go run main.go

// =============================================================================
// 1. WAL 레코드 정의
// =============================================================================
// WAL에 기록되는 레코드 타입과 인코딩 형식

// RecordType은 WAL 레코드의 종류를 나타낸다.
type RecordType byte

const (
	RecordSeries  RecordType = 1 // 시리즈(스트림) 정의: ID → 레이블 매핑
	RecordEntries RecordType = 2 // 로그 엔트리: 시리즈 ID + 타임스탬프 + 로그 라인
)

func (r RecordType) String() string {
	switch r {
	case RecordSeries:
		return "SERIES"
	case RecordEntries:
		return "ENTRIES"
	default:
		return "UNKNOWN"
	}
}

// SeriesRecord는 시리즈(스트림) 정의 레코드이다.
type SeriesRecord struct {
	SeriesID uint64 // 시리즈 고유 ID
	Labels   string // 레이블 문자열 (예: {app="api", env="prod"})
}

// EntryRecord는 로그 엔트리 레코드이다.
type EntryRecord struct {
	SeriesID  uint64    // 시리즈 ID
	Timestamp time.Time // 타임스탬프
	Line      string    // 로그 라인
	Counter   uint64    // 엔트리 카운터 (중복 제거용)
}

// =============================================================================
// 2. WAL 세그먼트
// =============================================================================
// WAL 세그먼트는 순차 쓰기 전용 파일이다.
// Loki 실제 코드: prometheus/tsdb의 WAL 구현을 사용

const (
	maxSegmentSize = 4 * 1024 // 4KB (시연용, 실제로는 128MB)
	segmentPrefix  = "segment-"
)

// Segment는 WAL의 하나의 세그먼트 파일이다.
type Segment struct {
	index    int    // 세그먼트 인덱스 (0, 1, 2, ...)
	path     string // 파일 경로
	file     *os.File
	size     int    // 현재 크기
	maxSize  int    // 최대 크기
}

// WALRecord는 파일에 기록/읽기되는 레코드의 바이너리 형식이다.
// 형식: [타입(1)] [데이터길이(4)] [데이터(가변)] [CRC32(4)]
type WALRecord struct {
	Type RecordType
	Data []byte
}

// encodeSeriesRecord는 시리즈 레코드를 바이트로 인코딩한다.
func encodeSeriesRecord(rec SeriesRecord) []byte {
	// SeriesID(8) + Labels 문자열
	data := make([]byte, 8+len(rec.Labels))
	binary.LittleEndian.PutUint64(data[:8], rec.SeriesID)
	copy(data[8:], rec.Labels)
	return data
}

// decodeSeriesRecord는 바이트에서 시리즈 레코드를 디코딩한다.
func decodeSeriesRecord(data []byte) SeriesRecord {
	return SeriesRecord{
		SeriesID: binary.LittleEndian.Uint64(data[:8]),
		Labels:   string(data[8:]),
	}
}

// encodeEntryRecord는 엔트리 레코드를 바이트로 인코딩한다.
func encodeEntryRecord(rec EntryRecord) []byte {
	// SeriesID(8) + Timestamp(8) + Counter(8) + Line
	data := make([]byte, 24+len(rec.Line))
	binary.LittleEndian.PutUint64(data[:8], rec.SeriesID)
	binary.LittleEndian.PutUint64(data[8:16], uint64(rec.Timestamp.UnixNano()))
	binary.LittleEndian.PutUint64(data[16:24], rec.Counter)
	copy(data[24:], rec.Line)
	return data
}

// decodeEntryRecord는 바이트에서 엔트리 레코드를 디코딩한다.
func decodeEntryRecord(data []byte) EntryRecord {
	return EntryRecord{
		SeriesID:  binary.LittleEndian.Uint64(data[:8]),
		Timestamp: time.Unix(0, int64(binary.LittleEndian.Uint64(data[8:16]))),
		Counter:   binary.LittleEndian.Uint64(data[16:24]),
		Line:      string(data[24:]),
	}
}

// encodeWALRecord는 WAL 레코드를 바이트로 인코딩한다.
// 형식: [타입(1)] [데이터길이(4)] [데이터(가변)] [CRC32(4)]
func encodeWALRecord(rec WALRecord) []byte {
	buf := make([]byte, 1+4+len(rec.Data)+4)
	buf[0] = byte(rec.Type)
	binary.LittleEndian.PutUint32(buf[1:5], uint32(len(rec.Data)))
	copy(buf[5:5+len(rec.Data)], rec.Data)
	// CRC32 체크섬
	checksum := crc32.ChecksumIEEE(buf[:5+len(rec.Data)])
	binary.LittleEndian.PutUint32(buf[5+len(rec.Data):], checksum)
	return buf
}

// decodeWALRecord는 바이트에서 WAL 레코드를 디코딩한다.
func decodeWALRecord(buf []byte) (WALRecord, int, error) {
	if len(buf) < 9 { // 최소: 타입(1) + 길이(4) + CRC(4)
		return WALRecord{}, 0, fmt.Errorf("레코드가 너무 짧음: %d bytes", len(buf))
	}

	recType := RecordType(buf[0])
	dataLen := int(binary.LittleEndian.Uint32(buf[1:5]))
	totalLen := 1 + 4 + dataLen + 4

	if len(buf) < totalLen {
		return WALRecord{}, 0, fmt.Errorf("불완전한 레코드: 필요 %d, 실제 %d", totalLen, len(buf))
	}

	// CRC32 검증
	expectedCRC := binary.LittleEndian.Uint32(buf[5+dataLen : totalLen])
	actualCRC := crc32.ChecksumIEEE(buf[:5+dataLen])
	if expectedCRC != actualCRC {
		return WALRecord{}, 0, fmt.Errorf("CRC 불일치: expected=0x%08x, actual=0x%08x", expectedCRC, actualCRC)
	}

	data := make([]byte, dataLen)
	copy(data, buf[5:5+dataLen])

	return WALRecord{Type: recType, Data: data}, totalLen, nil
}

// =============================================================================
// 3. WAL 관리자
// =============================================================================

// WAL은 Write-Ahead Log 관리자이다.
type WAL struct {
	mu         sync.Mutex
	dir        string      // WAL 디렉토리
	segment    *Segment    // 현재 활성 세그먼트
	segmentIdx int         // 현재 세그먼트 인덱스
	counter    uint64      // 전역 엔트리 카운터 (중복 제거용)
}

// NewWAL은 새로운 WAL을 생성한다.
func NewWAL(dir string) (*WAL, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("WAL 디렉토리 생성 실패: %w", err)
	}

	wal := &WAL{
		dir: dir,
	}

	// 첫 번째 세그먼트 생성
	if err := wal.newSegment(); err != nil {
		return nil, err
	}

	return wal, nil
}

// newSegment는 새로운 세그먼트 파일을 생성한다.
func (w *WAL) newSegment() error {
	path := filepath.Join(w.dir, fmt.Sprintf("%s%06d", segmentPrefix, w.segmentIdx))
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("세그먼트 생성 실패: %w", err)
	}

	w.segment = &Segment{
		index:   w.segmentIdx,
		path:    path,
		file:    f,
		maxSize: maxSegmentSize,
	}
	w.segmentIdx++

	return nil
}

// writeRecord는 WAL 레코드를 현재 세그먼트에 기록한다.
func (w *WAL) writeRecord(rec WALRecord) error {
	encoded := encodeWALRecord(rec)

	// 세그먼트 크기 초과 시 새 세그먼트 생성
	if w.segment.size+len(encoded) > w.segment.maxSize {
		w.segment.file.Close()
		if err := w.newSegment(); err != nil {
			return err
		}
	}

	n, err := w.segment.file.Write(encoded)
	if err != nil {
		return fmt.Errorf("WAL 쓰기 실패: %w", err)
	}
	w.segment.size += n

	return nil
}

// LogSeries는 시리즈 정의를 WAL에 기록한다.
func (w *WAL) LogSeries(seriesID uint64, labels string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	data := encodeSeriesRecord(SeriesRecord{
		SeriesID: seriesID,
		Labels:   labels,
	})

	return w.writeRecord(WALRecord{Type: RecordSeries, Data: data})
}

// LogEntry는 로그 엔트리를 WAL에 기록한다.
func (w *WAL) LogEntry(seriesID uint64, ts time.Time, line string) (uint64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.counter++
	data := encodeEntryRecord(EntryRecord{
		SeriesID:  seriesID,
		Timestamp: ts,
		Line:      line,
		Counter:   w.counter,
	})

	err := w.writeRecord(WALRecord{Type: RecordEntries, Data: data})
	return w.counter, err
}

// Close는 WAL을 닫는다.
func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.segment != nil && w.segment.file != nil {
		return w.segment.file.Close()
	}
	return nil
}

// =============================================================================
// 4. 체크포인트 (Checkpoint)
// =============================================================================
// 체크포인트는 현재 상태의 전체 스냅샷이다.
// 복구 시 체크포인트를 먼저 로드하고, 이후의 WAL 세그먼트만 리플레이하면 된다.
// 이를 통해 복구 시간을 크게 단축할 수 있다.

const checkpointPrefix = "checkpoint."

// Checkpoint는 체크포인트 데이터이다.
type Checkpoint struct {
	Series  map[uint64]string      // 시리즈 ID → 레이블
	Entries []EntryRecord          // 모든 엔트리
	Counter uint64                 // 마지막 카운터 값
	SegmentIndex int               // 이 체크포인트 이후의 세그먼트 인덱스
}

// WriteCheckpoint는 현재 상태를 체크포인트 파일로 기록한다.
func WriteCheckpoint(dir string, cp *Checkpoint) error {
	path := filepath.Join(dir, fmt.Sprintf("%s%06d", checkpointPrefix, cp.SegmentIndex))
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("체크포인트 생성 실패: %w", err)
	}
	defer f.Close()

	// 시리즈 기록
	for id, labels := range cp.Series {
		data := encodeSeriesRecord(SeriesRecord{SeriesID: id, Labels: labels})
		encoded := encodeWALRecord(WALRecord{Type: RecordSeries, Data: data})
		f.Write(encoded)
	}

	// 엔트리 기록
	for _, entry := range cp.Entries {
		data := encodeEntryRecord(entry)
		encoded := encodeWALRecord(WALRecord{Type: RecordEntries, Data: data})
		f.Write(encoded)
	}

	return nil
}

// =============================================================================
// 5. 복구 (Recovery)
// =============================================================================
// 복구 과정:
// 1. 체크포인트가 있으면 로드
// 2. 체크포인트 이후의 WAL 세그먼트를 순서대로 리플레이
// 3. 카운터 기반 중복 제거

// RecoveredState는 복구된 상태이다.
type RecoveredState struct {
	Series      map[uint64]string // 시리즈 ID → 레이블
	Entries     []EntryRecord     // 복구된 엔트리
	LastCounter uint64            // 마지막 카운터 값
}

// readRecordsFromFile는 파일에서 모든 WAL 레코드를 읽는다.
func readRecordsFromFile(path string) ([]WALRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var records []WALRecord
	offset := 0
	for offset < len(data) {
		rec, consumed, err := decodeWALRecord(data[offset:])
		if err != nil {
			break // 불완전한 레코드는 무시 (비정상 종료 시)
		}
		records = append(records, rec)
		offset += consumed
	}

	return records, nil
}

// Recover는 WAL에서 상태를 복구한다.
func Recover(dir string) (*RecoveredState, []string, error) {
	var log []string
	state := &RecoveredState{
		Series: make(map[uint64]string),
	}
	seenCounters := make(map[uint64]bool) // 중복 제거용

	// 1. 체크포인트 찾기
	checkpointIdx := -1
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("디렉토리 읽기 실패: %w", err)
	}

	for _, entry := range dirEntries {
		if strings.HasPrefix(entry.Name(), checkpointPrefix) {
			var idx int
			fmt.Sscanf(entry.Name(), checkpointPrefix+"%d", &idx)
			if idx > checkpointIdx {
				checkpointIdx = idx
			}
		}
	}

	// 2. 체크포인트 로드
	if checkpointIdx >= 0 {
		cpPath := filepath.Join(dir, fmt.Sprintf("%s%06d", checkpointPrefix, checkpointIdx))
		log = append(log, fmt.Sprintf("체크포인트 로드: %s", filepath.Base(cpPath)))
		records, err := readRecordsFromFile(cpPath)
		if err != nil {
			return nil, log, fmt.Errorf("체크포인트 읽기 실패: %w", err)
		}

		for _, rec := range records {
			switch rec.Type {
			case RecordSeries:
				sr := decodeSeriesRecord(rec.Data)
				state.Series[sr.SeriesID] = sr.Labels
			case RecordEntries:
				er := decodeEntryRecord(rec.Data)
				state.Entries = append(state.Entries, er)
				seenCounters[er.Counter] = true
				if er.Counter > state.LastCounter {
					state.LastCounter = er.Counter
				}
			}
		}
		log = append(log, fmt.Sprintf("  → 시리즈 %d개, 엔트리 %d개 복구",
			len(state.Series), len(state.Entries)))
	}

	// 3. 세그먼트 파일 목록 수집 (정렬된 순서)
	var segmentFiles []string
	for _, entry := range dirEntries {
		if strings.HasPrefix(entry.Name(), segmentPrefix) {
			var idx int
			fmt.Sscanf(entry.Name(), segmentPrefix+"%d", &idx)
			// 체크포인트 이후의 세그먼트만 리플레이
			if idx > checkpointIdx {
				segmentFiles = append(segmentFiles, entry.Name())
			}
		}
	}
	sort.Strings(segmentFiles)

	// 4. 세그먼트 리플레이
	newEntries := 0
	dupEntries := 0
	for _, segFile := range segmentFiles {
		segPath := filepath.Join(dir, segFile)
		log = append(log, fmt.Sprintf("세그먼트 리플레이: %s", segFile))

		records, err := readRecordsFromFile(segPath)
		if err != nil {
			log = append(log, fmt.Sprintf("  → 읽기 실패 (건너뛰기): %v", err))
			continue
		}

		for _, rec := range records {
			switch rec.Type {
			case RecordSeries:
				sr := decodeSeriesRecord(rec.Data)
				state.Series[sr.SeriesID] = sr.Labels
			case RecordEntries:
				er := decodeEntryRecord(rec.Data)
				// 카운터 기반 중복 제거
				if seenCounters[er.Counter] {
					dupEntries++
					continue
				}
				seenCounters[er.Counter] = true
				state.Entries = append(state.Entries, er)
				newEntries++
				if er.Counter > state.LastCounter {
					state.LastCounter = er.Counter
				}
			}
		}

		log = append(log, fmt.Sprintf("  → 새 엔트리 %d개 (중복 %d개 제거)", newEntries, dupEntries))
	}

	return state, log, nil
}

// =============================================================================
// 6. Ingester 시뮬레이션 (WAL 사용)
// =============================================================================

// Ingester는 WAL을 사용하는 Ingester를 시뮬레이션한다.
type Ingester struct {
	wal     *WAL
	series  map[uint64]string      // 메모리 내 시리즈 매핑
	entries map[uint64][]EntryRecord // 메모리 내 엔트리 버퍼
	nextID  uint64
}

// NewIngester는 새로운 Ingester를 생성한다.
func NewIngester(walDir string) (*Ingester, error) {
	wal, err := NewWAL(walDir)
	if err != nil {
		return nil, err
	}

	return &Ingester{
		wal:     wal,
		series:  make(map[uint64]string),
		entries: make(map[uint64][]EntryRecord),
	}, nil
}

// Push는 로그 엔트리를 수집한다.
func (ing *Ingester) Push(labels string, ts time.Time, line string) error {
	// 시리즈 ID 조회 또는 생성
	var seriesID uint64
	found := false
	for id, l := range ing.series {
		if l == labels {
			seriesID = id
			found = true
			break
		}
	}

	if !found {
		ing.nextID++
		seriesID = ing.nextID
		ing.series[seriesID] = labels

		// WAL에 시리즈 기록
		if err := ing.wal.LogSeries(seriesID, labels); err != nil {
			return err
		}
	}

	// WAL에 엔트리 기록 (먼저 WAL에 쓰고, 그 다음 메모리에 적용)
	counter, err := ing.wal.LogEntry(seriesID, ts, line)
	if err != nil {
		return err
	}

	// 메모리에 적용
	entry := EntryRecord{
		SeriesID:  seriesID,
		Timestamp: ts,
		Line:      line,
		Counter:   counter,
	}
	ing.entries[seriesID] = append(ing.entries[seriesID], entry)

	return nil
}

// CreateCheckpoint는 현재 상태의 체크포인트를 생성한다.
func (ing *Ingester) CreateCheckpoint() error {
	cp := &Checkpoint{
		Series:       ing.series,
		Counter:      ing.wal.counter,
		SegmentIndex: ing.wal.segmentIdx - 1,
	}

	for _, entries := range ing.entries {
		cp.Entries = append(cp.Entries, entries...)
	}

	return WriteCheckpoint(ing.wal.dir, cp)
}

// Close는 Ingester를 닫는다.
func (ing *Ingester) Close() error {
	return ing.wal.Close()
}

// Stats는 현재 상태를 출력한다.
func (ing *Ingester) Stats() {
	totalEntries := 0
	for _, entries := range ing.entries {
		totalEntries += len(entries)
	}
	fmt.Printf("    시리즈: %d개, 엔트리: %d개, WAL 세그먼트: %d개\n",
		len(ing.series), totalEntries, ing.wal.segmentIdx)
}

// =============================================================================
// 7. 메인 함수 - WAL 시연
// =============================================================================

func main() {
	fmt.Println("=== Loki PoC #10: WAL - Write-Ahead Log 기반 내구성 보장 ===")
	fmt.Println()

	// 임시 디렉토리 생성
	walDir, err := os.MkdirTemp("", "loki-wal-poc-*")
	if err != nil {
		fmt.Printf("임시 디렉토리 생성 실패: %v\n", err)
		return
	}
	defer os.RemoveAll(walDir) // 종료 시 정리

	fmt.Printf("WAL 디렉토리: %s\n\n", walDir)

	// =========================================================================
	// 시연 1: WAL 기본 기록
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("[시연 1] WAL 기본 기록")
	fmt.Println()

	ing, err := NewIngester(filepath.Join(walDir, "wal1"))
	if err != nil {
		fmt.Printf("Ingester 생성 실패: %v\n", err)
		return
	}

	baseTime := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	// 로그 엔트리 Push
	testEntries := []struct {
		labels string
		line   string
	}{
		{`{app="api", env="prod"}`, "Starting API server on port 8080"},
		{`{app="api", env="prod"}`, "Connected to database"},
		{`{app="api", env="prod"}`, "Error: connection timeout"},
		{`{app="web", env="prod"}`, "Serving static files"},
		{`{app="web", env="prod"}`, "Cache miss for index.html"},
		{`{app="api", env="staging"}`, "Debug mode enabled"},
	}

	fmt.Println("  Push 요청 (WAL에 먼저 기록 → 메모리에 적용):")
	for i, e := range testEntries {
		ts := baseTime.Add(time.Duration(i) * time.Second)
		if err := ing.Push(e.labels, ts, e.line); err != nil {
			fmt.Printf("    Push 실패: %v\n", err)
			continue
		}
		fmt.Printf("    [%d] %s → %s\n", i+1, e.labels, e.line)
	}

	fmt.Println()
	fmt.Print("  현재 상태: ")
	ing.Stats()

	// WAL 파일 확인
	fmt.Println()
	fmt.Println("  WAL 세그먼트 파일:")
	walEntries, _ := os.ReadDir(filepath.Join(walDir, "wal1"))
	for _, entry := range walEntries {
		info, _ := entry.Info()
		fmt.Printf("    %s (%d bytes)\n", entry.Name(), info.Size())
	}

	ing.Close()
	fmt.Println()

	// =========================================================================
	// 시연 2: WAL 복구 (Checkpoint 없이)
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("[시연 2] WAL 복구 (Checkpoint 없이)")
	fmt.Println()
	fmt.Println("  시나리오: Ingester가 비정상 종료 → WAL에서 복구")
	fmt.Println()

	state, logs, err := Recover(filepath.Join(walDir, "wal1"))
	if err != nil {
		fmt.Printf("복구 실패: %v\n", err)
		return
	}

	fmt.Println("  복구 과정:")
	for _, log := range logs {
		fmt.Printf("    %s\n", log)
	}

	fmt.Println()
	fmt.Println("  복구된 시리즈:")
	for id, labels := range state.Series {
		fmt.Printf("    ID=%d → %s\n", id, labels)
	}

	fmt.Println()
	fmt.Println("  복구된 엔트리:")
	for _, entry := range state.Entries {
		labels := state.Series[entry.SeriesID]
		fmt.Printf("    [counter=%d] %s %s: %s\n",
			entry.Counter, entry.Timestamp.Format("15:04:05"), labels, entry.Line)
	}
	fmt.Printf("  총 %d개 엔트리 복구 (마지막 카운터: %d)\n",
		len(state.Entries), state.LastCounter)
	fmt.Println()

	// =========================================================================
	// 시연 3: Checkpoint + Recovery
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("[시연 3] Checkpoint + Recovery")
	fmt.Println()

	walDir2 := filepath.Join(walDir, "wal2")
	ing2, _ := NewIngester(walDir2)

	// Phase 1: 초기 데이터 Push
	fmt.Println("  Phase 1: 초기 데이터 Push (5개)")
	for i := 0; i < 5; i++ {
		ts := baseTime.Add(time.Duration(i) * time.Second)
		ing2.Push(`{app="api"}`, ts, fmt.Sprintf("Phase1 log #%d", i+1))
	}
	ing2.Stats()

	// Checkpoint 생성
	fmt.Println()
	fmt.Println("  ★ Checkpoint 생성 (현재 상태 스냅샷)")
	ing2.CreateCheckpoint()

	// Phase 2: 추가 데이터 Push
	fmt.Println()
	fmt.Println("  Phase 2: 추가 데이터 Push (3개, Checkpoint 이후)")
	for i := 5; i < 8; i++ {
		ts := baseTime.Add(time.Duration(i) * time.Second)
		ing2.Push(`{app="api"}`, ts, fmt.Sprintf("Phase2 log #%d", i+1))
	}
	ing2.Stats()
	ing2.Close()

	// 복구
	fmt.Println()
	fmt.Println("  ★ 비정상 종료 시뮬레이션 → 복구 시작")
	fmt.Println()

	state2, logs2, _ := Recover(walDir2)
	fmt.Println("  복구 과정:")
	for _, log := range logs2 {
		fmt.Printf("    %s\n", log)
	}

	fmt.Println()
	fmt.Println("  복구된 엔트리:")
	for _, entry := range state2.Entries {
		fmt.Printf("    [counter=%d] %s: %s\n",
			entry.Counter, entry.Timestamp.Format("15:04:05"), entry.Line)
	}
	fmt.Printf("  총 %d개 엔트리 복구 완료\n", len(state2.Entries))
	fmt.Println()

	// =========================================================================
	// 시연 4: 세그먼트 로테이션
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("[시연 4] 세그먼트 로테이션")
	fmt.Println()
	fmt.Printf("  세그먼트 최대 크기: %d bytes\n", maxSegmentSize)
	fmt.Println()

	walDir3 := filepath.Join(walDir, "wal3")
	ing3, _ := NewIngester(walDir3)

	// 많은 엔트리를 Push하여 세그먼트 로테이션 유발
	fmt.Println("  대량 Push (세그먼트 로테이션 유발):")
	for i := 0; i < 30; i++ {
		ts := baseTime.Add(time.Duration(i) * 100 * time.Millisecond)
		line := fmt.Sprintf("Log entry #%03d: This is a test message with some padding to fill the segment", i+1)
		ing3.Push(`{app="bulk"}`, ts, line)
	}
	ing3.Stats()

	fmt.Println()
	fmt.Println("  세그먼트 파일 목록:")
	entries3, _ := os.ReadDir(walDir3)
	totalSize := 0
	for _, entry := range entries3 {
		info, _ := entry.Info()
		size := int(info.Size())
		totalSize += size
		fmt.Printf("    %s (%d bytes)\n", entry.Name(), size)
	}
	fmt.Printf("    총 WAL 크기: %d bytes\n", totalSize)

	ing3.Close()

	// 복구 검증
	fmt.Println()
	state3, _, _ := Recover(walDir3)
	fmt.Printf("  복구 검증: %d개 엔트리 복구 (원본: 30개)\n", len(state3.Entries))
	fmt.Println()

	// =========================================================================
	// 시연 5: CRC32 무결성 검증
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("[시연 5] CRC32 무결성 검증")
	fmt.Println()

	// 정상 레코드 인코딩/디코딩
	testRecord := WALRecord{
		Type: RecordEntries,
		Data: encodeEntryRecord(EntryRecord{
			SeriesID:  42,
			Timestamp: baseTime,
			Line:      "Test integrity check",
			Counter:   999,
		}),
	}

	encoded := encodeWALRecord(testRecord)
	fmt.Printf("  원본 레코드: %d bytes, CRC32: 0x%08x\n",
		len(encoded), crc32.ChecksumIEEE(encoded[:len(encoded)-4]))

	// 정상 디코딩
	decoded, _, err := decodeWALRecord(encoded)
	if err != nil {
		fmt.Printf("  정상 디코딩: 실패 (%v)\n", err)
	} else {
		er := decodeEntryRecord(decoded.Data)
		fmt.Printf("  정상 디코딩: 성공 (SeriesID=%d, Line=%q)\n", er.SeriesID, er.Line)
	}

	// 손상된 레코드 (1바이트 변조)
	corrupted := make([]byte, len(encoded))
	copy(corrupted, encoded)
	corrupted[10] ^= 0xFF // 데이터 영역 변조
	_, _, err = decodeWALRecord(corrupted)
	if err != nil {
		fmt.Printf("  손상 디코딩: 실패 (예상대로) → %v\n", err)
	} else {
		fmt.Println("  손상 디코딩: 성공 (예상과 다름!)")
	}

	// =========================================================================
	// 구조 요약
	// =========================================================================
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("=== WAL 구조 요약 ===")
	fmt.Println()
	fmt.Println("  Loki Ingester의 WAL 동작 흐름:")
	fmt.Println()
	fmt.Println("  Push 요청 ──────────────────────────────────────────")
	fmt.Println("       │")
	fmt.Println("       ├─→ [1] WAL 세그먼트에 순차 기록 (fsync)")
	fmt.Println("       │     segment-000000 → segment-000001 → ...")
	fmt.Println("       │")
	fmt.Println("       └─→ [2] 메모리에 적용 (읽기/쿼리용)")
	fmt.Println()
	fmt.Println("  주기적 ──────────────────────────────────────────────")
	fmt.Println("       │")
	fmt.Println("       ├─→ [3] Checkpoint 생성 (전체 스냅샷)")
	fmt.Println("       │     checkpoint.000003 (세그먼트 3까지의 상태)")
	fmt.Println("       │")
	fmt.Println("       └─→ [4] 오래된 세그먼트 삭제")
	fmt.Println("             segment-000000 ~ segment-000003 삭제")
	fmt.Println()
	fmt.Println("  복구 ────────────────────────────────────────────────")
	fmt.Println("       │")
	fmt.Println("       ├─→ [5] 최신 Checkpoint 로드")
	fmt.Println("       │")
	fmt.Println("       ├─→ [6] 이후 세그먼트 리플레이")
	fmt.Println("       │")
	fmt.Println("       └─→ [7] 카운터 기반 중복 제거")
	fmt.Println()
	fmt.Println("  핵심 포인트:")
	fmt.Println("    - WAL-first: 항상 WAL에 먼저 기록 → 메모리에 적용")
	fmt.Println("    - 순차 쓰기: 랜덤 I/O 없이 append-only → 높은 쓰기 성능")
	fmt.Println("    - CRC32: 각 레코드에 체크섬 → 데이터 무결성 보장")
	fmt.Println("    - Checkpoint: 주기적 스냅샷 → 복구 시간 단축")
	fmt.Println("    - Counter dedup: 전역 카운터 → 체크포인트/세그먼트 중복 방지")
	fmt.Println("    - Loki 실제 코드: pkg/ingester/wal/, pkg/ingester/checkpoint.go")
}
