package main

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// =============================================================================
// Prometheus WAL (Write-Ahead Log) PoC
// =============================================================================
//
// Prometheus TSDB의 WAL은 크래시 복구를 위한 핵심 메커니즘이다.
// 실제 구현: tsdb/wlog/wlog.go, tsdb/record/record.go
//
// WAL 핵심 설계:
//   1. 세그먼트 파일: 고정 크기(기본 128MB) 세그먼트로 분할
//   2. 레코드 포맷: [type(1B)] [length(4B, big-endian)] [data] [CRC32(4B)]
//   3. CRC32 체크섬: Castagnoli 다항식 사용 (데이터 무결성 검증)
//   4. 세그먼트 로테이션: 크기 초과 시 새 세그먼트 생성
//   5. 레코드 타입: Series(시리즈 정의), Samples(샘플 데이터) 등
//
// 실제 Prometheus에서는 32KB 페이지 단위로 버퍼링하고,
// 레코드가 페이지 경계를 넘으면 분할(First/Middle/Last)한다.
// 이 PoC에서는 핵심 원리인 레코드 포맷 + CRC + 세그먼트 로테이션에 집중한다.

// =============================================================================
// 레코드 타입 정의 (실제: tsdb/record/record.go의 Type)
// =============================================================================

const (
	RecordTypeSeries  byte = 1 // 시리즈 정의 레코드
	RecordTypeSamples byte = 2 // 샘플 데이터 레코드
)

// CRC32 Castagnoli 테이블 (실제 Prometheus와 동일)
var castagnoliTable = crc32.MakeTable(crc32.Castagnoli)

// =============================================================================
// 데이터 구조체
// =============================================================================

// Label은 메트릭 레이블 (name=value 쌍)
type Label struct {
	Name  string
	Value string
}

// SeriesRecord는 시리즈 정의를 나타낸다.
// 실제: record.RefSeries { Ref chunks.HeadSeriesRef, Labels labels.Labels }
type SeriesRecord struct {
	Ref    uint64  // 시리즈 참조 ID
	Labels []Label // 레이블 셋
}

// SampleRecord는 개별 샘플을 나타낸다.
// 실제: record.RefSample { Ref chunks.HeadSeriesRef, T int64, V float64 }
type SampleRecord struct {
	Ref       uint64  // 시리즈 참조 ID
	Timestamp int64   // 타임스탬프 (밀리초)
	Value     float64 // 값
}

// =============================================================================
// 레코드 인코딩/디코딩
// =============================================================================

// encodeSeriesRecord는 시리즈 레코드를 바이트로 인코딩한다.
// 포맷: ref(8B) + labelCount(2B) + [nameLen(2B) + name + valueLen(2B) + value]...
func encodeSeriesRecord(s SeriesRecord) []byte {
	buf := make([]byte, 0, 128)

	// ref (8 bytes)
	tmp := make([]byte, 8)
	binary.BigEndian.PutUint64(tmp, s.Ref)
	buf = append(buf, tmp...)

	// label count (2 bytes)
	tmp2 := make([]byte, 2)
	binary.BigEndian.PutUint16(tmp2, uint16(len(s.Labels)))
	buf = append(buf, tmp2...)

	// labels
	for _, l := range s.Labels {
		// name length + name
		binary.BigEndian.PutUint16(tmp2, uint16(len(l.Name)))
		buf = append(buf, tmp2...)
		buf = append(buf, []byte(l.Name)...)

		// value length + value
		binary.BigEndian.PutUint16(tmp2, uint16(len(l.Value)))
		buf = append(buf, tmp2...)
		buf = append(buf, []byte(l.Value)...)
	}

	return buf
}

// decodeSeriesRecord는 바이트에서 시리즈 레코드를 디코딩한다.
func decodeSeriesRecord(data []byte) (SeriesRecord, error) {
	if len(data) < 10 {
		return SeriesRecord{}, fmt.Errorf("series record too short: %d bytes", len(data))
	}

	s := SeriesRecord{}
	s.Ref = binary.BigEndian.Uint64(data[0:8])
	labelCount := binary.BigEndian.Uint16(data[8:10])
	offset := 10

	s.Labels = make([]Label, 0, labelCount)
	for i := 0; i < int(labelCount); i++ {
		if offset+2 > len(data) {
			return s, fmt.Errorf("truncated label name length at offset %d", offset)
		}
		nameLen := int(binary.BigEndian.Uint16(data[offset : offset+2]))
		offset += 2
		if offset+nameLen > len(data) {
			return s, fmt.Errorf("truncated label name at offset %d", offset)
		}
		name := string(data[offset : offset+nameLen])
		offset += nameLen

		if offset+2 > len(data) {
			return s, fmt.Errorf("truncated label value length at offset %d", offset)
		}
		valueLen := int(binary.BigEndian.Uint16(data[offset : offset+2]))
		offset += 2
		if offset+valueLen > len(data) {
			return s, fmt.Errorf("truncated label value at offset %d", offset)
		}
		value := string(data[offset : offset+valueLen])
		offset += valueLen

		s.Labels = append(s.Labels, Label{Name: name, Value: value})
	}

	return s, nil
}

// encodeSampleRecord는 샘플 레코드를 바이트로 인코딩한다.
// 포맷: ref(8B) + timestamp(8B) + value(8B) = 24 bytes
func encodeSampleRecord(s SampleRecord) []byte {
	buf := make([]byte, 24)
	binary.BigEndian.PutUint64(buf[0:8], s.Ref)
	binary.BigEndian.PutUint64(buf[8:16], uint64(s.Timestamp))
	binary.BigEndian.PutUint64(buf[16:24], math.Float64bits(s.Value))
	return buf
}

// decodeSampleRecord는 바이트에서 샘플 레코드를 디코딩한다.
func decodeSampleRecord(data []byte) (SampleRecord, error) {
	if len(data) != 24 {
		return SampleRecord{}, fmt.Errorf("sample record must be 24 bytes, got %d", len(data))
	}
	return SampleRecord{
		Ref:       binary.BigEndian.Uint64(data[0:8]),
		Timestamp: int64(binary.BigEndian.Uint64(data[8:16])),
		Value:     math.Float64frombits(binary.BigEndian.Uint64(data[16:24])),
	}, nil
}

// =============================================================================
// WAL 구현
// =============================================================================
//
// 실제 Prometheus WAL (tsdb/wlog/wlog.go의 WL 구조체) 주요 필드:
//   - dir: WAL 디렉토리
//   - segment: 현재 활성 세그먼트 (*Segment)
//   - segmentSize: 세그먼트 최대 크기 (기본 128MB)
//   - page: 32KB 페이지 버퍼 (배치 쓰기 최적화)
//   - donePages: 현재 세그먼트에 쓴 페이지 수
//
// 실제 레코드 헤더(recordHeaderSize=7):
//   [type(1B)] [length(2B)] [CRC32(4B)]
// 이 PoC에서는 가변 길이 레코드를 지원하기 위해:
//   [type(1B)] [length(4B)] [data(N B)] [CRC32(4B)]

const (
	// 데모용 세그먼트 크기 (4KB) - 로테이션을 빠르게 관찰하기 위함
	// 실제 Prometheus: DefaultSegmentSize = 128 * 1024 * 1024 (128MB)
	defaultSegmentSize = 4 * 1024

	// 레코드 헤더: type(1) + length(4) = 5 bytes
	recordHeaderSize = 5
	// 레코드 푸터: CRC32(4) = 4 bytes
	recordFooterSize = 4
	// 레코드 오버헤드 = 헤더 + 푸터
	recordOverhead = recordHeaderSize + recordFooterSize
)

// WAL은 Write-Ahead Log 구현이다.
type WAL struct {
	dir          string   // WAL 디렉토리 경로
	segment      *os.File // 현재 활성 세그먼트 파일
	segmentIndex int      // 현재 세그먼트 인덱스
	segmentSize  int      // 세그먼트 최대 크기 (바이트)
	bytesWritten int      // 현재 세그먼트에 기록된 바이트 수

	// 통계
	totalBytes     int64 // 전체 기록된 바이트
	totalRecords   int   // 전체 기록된 레코드 수
	segmentsCount  int   // 생성된 세그먼트 수
	seriesRecords  int   // 시리즈 레코드 수
	sampleRecords  int   // 샘플 레코드 수
	rotationEvents int   // 세그먼트 로테이션 횟수
}

// NewWAL은 새 WAL 인스턴스를 생성한다.
// 실제 Prometheus: wlog.NewSize() 함수 참조
func NewWAL(dir string, segmentSize int) (*WAL, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("WAL 디렉토리 생성 실패: %w", err)
	}

	w := &WAL{
		dir:         dir,
		segmentSize: segmentSize,
	}

	// 첫 번째 세그먼트 생성
	// 실제: CreateSegment(w.Dir(), writeSegmentIndex)
	if err := w.createSegment(0); err != nil {
		return nil, err
	}

	return w, nil
}

// OpenWAL은 기존 WAL 디렉토리를 열어 이어서 쓸 수 있게 한다.
// 실제: wlog.Open() + NewSize()에서 마지막 세그먼트 이후 새 세그먼트 생성
func OpenWAL(dir string, segmentSize int) (*WAL, error) {
	w := &WAL{
		dir:         dir,
		segmentSize: segmentSize,
	}

	// 기존 세그먼트 목록 확인
	segments, err := listSegmentFiles(dir)
	if err != nil {
		return nil, err
	}

	if len(segments) == 0 {
		// 세그먼트 없으면 새로 시작
		if err := w.createSegment(0); err != nil {
			return nil, err
		}
	} else {
		// 마지막 세그먼트 다음 번호로 새 세그먼트 생성
		// 실제 Prometheus: writeSegmentIndex = last + 1
		lastIdx := segments[len(segments)-1]
		nextIdx := lastIdx + 1
		if err := w.createSegment(nextIdx); err != nil {
			return nil, err
		}
	}

	return w, nil
}

// segmentName은 세그먼트 파일 이름을 생성한다.
// 실제: wlog.SegmentName() → fmt.Sprintf("%08d", i)
func (w *WAL) segmentName(index int) string {
	return filepath.Join(w.dir, fmt.Sprintf("%08d", index))
}

// createSegment는 새 세그먼트 파일을 생성한다.
// 실제: wlog.CreateSegment()
func (w *WAL) createSegment(index int) error {
	name := w.segmentName(index)
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("세그먼트 %d 생성 실패: %w", index, err)
	}

	w.segment = f
	w.segmentIndex = index
	w.bytesWritten = 0
	w.segmentsCount++

	return nil
}

// rotateSegment는 현재 세그먼트를 닫고 새 세그먼트를 생성한다.
// 실제: wlog.nextSegment() → CreateSegment(w.Dir(), w.segment.Index()+1)
func (w *WAL) rotateSegment() error {
	// 현재 세그먼트 sync + close
	if err := w.segment.Sync(); err != nil {
		return fmt.Errorf("세그먼트 sync 실패: %w", err)
	}
	if err := w.segment.Close(); err != nil {
		return fmt.Errorf("세그먼트 close 실패: %w", err)
	}

	w.rotationEvents++

	// 새 세그먼트 생성
	return w.createSegment(w.segmentIndex + 1)
}

// Log는 레코드를 WAL에 기록한다.
// 레코드 포맷: [type(1B)] [length(4B, big-endian)] [data(NB)] [CRC32(4B)]
//
// 실제 Prometheus WAL은 페이지(32KB) 기반으로 레코드를 분할하여 기록한다:
//   - recFull: 레코드가 한 페이지에 들어가는 경우
//   - recFirst/recMiddle/recLast: 페이지 경계를 넘는 경우 분할
// 각 조각의 헤더: [type(1B)] [length(2B)] [CRC32(4B)]
//
// 이 PoC에서는 레코드 분할 없이 전체 레코드를 한 번에 기록한다.
func (w *WAL) Log(recordType byte, data []byte) error {
	recordLen := recordOverhead + len(data)

	// 세그먼트 크기 초과 시 로테이션
	// 실제: if w.donePages >= w.pagesPerSegment() { w.nextSegment() }
	if w.bytesWritten+recordLen > w.segmentSize {
		if err := w.rotateSegment(); err != nil {
			return err
		}
	}

	// 헤더 쓰기: type(1B) + length(4B)
	header := make([]byte, recordHeaderSize)
	header[0] = recordType
	binary.BigEndian.PutUint32(header[1:5], uint32(len(data)))

	if _, err := w.segment.Write(header); err != nil {
		return fmt.Errorf("헤더 쓰기 실패: %w", err)
	}

	// 데이터 쓰기
	if _, err := w.segment.Write(data); err != nil {
		return fmt.Errorf("데이터 쓰기 실패: %w", err)
	}

	// CRC32 체크섬 쓰기 (Castagnoli)
	// 실제 Prometheus도 crc32.Castagnoli 사용
	checksum := crc32.Checksum(data, castagnoliTable)
	crcBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(crcBuf, checksum)
	if _, err := w.segment.Write(crcBuf); err != nil {
		return fmt.Errorf("CRC 쓰기 실패: %w", err)
	}

	w.bytesWritten += recordLen
	w.totalBytes += int64(recordLen)
	w.totalRecords++

	switch recordType {
	case RecordTypeSeries:
		w.seriesRecords++
	case RecordTypeSamples:
		w.sampleRecords++
	}

	return nil
}

// LogSeries는 시리즈 레코드를 WAL에 기록한다.
func (w *WAL) LogSeries(series SeriesRecord) error {
	data := encodeSeriesRecord(series)
	return w.Log(RecordTypeSeries, data)
}

// LogSample는 샘플 레코드를 WAL에 기록한다.
func (w *WAL) LogSample(sample SampleRecord) error {
	data := encodeSampleRecord(sample)
	return w.Log(RecordTypeSamples, data)
}

// Close는 WAL을 닫는다.
func (w *WAL) Close() error {
	if w.segment != nil {
		if err := w.segment.Sync(); err != nil {
			return err
		}
		return w.segment.Close()
	}
	return nil
}

// Stats는 WAL 통계를 반환한다.
func (w *WAL) Stats() string {
	return fmt.Sprintf(
		"WAL 통계:\n"+
			"  세그먼트 생성: %d개\n"+
			"  세그먼트 로테이션: %d회\n"+
			"  전체 레코드: %d개 (시리즈: %d, 샘플: %d)\n"+
			"  전체 바이트: %s",
		w.segmentsCount, w.rotationEvents,
		w.totalRecords, w.seriesRecords, w.sampleRecords,
		formatBytes(w.totalBytes),
	)
}

// =============================================================================
// WAL Reader
// =============================================================================
//
// 실제 Prometheus WAL Reader (tsdb/wlog/reader.go):
//   - 페이지 단위로 읽으면서 레코드 조각을 조합
//   - recFull → 즉시 반환
//   - recFirst → 버퍼에 추가, recMiddle 계속, recLast에서 반환
//   - recPageTerm → 페이지 나머지 건너뜀
//   - CRC32 검증 실패 시 CorruptionErr 반환

// WALReader는 WAL 디렉토리에서 레코드를 읽는다.
type WALReader struct {
	dir string

	// 복원된 데이터
	Series  map[uint64]SeriesRecord
	Samples []SampleRecord

	// 통계
	SegmentsRead  int
	RecordsRead   int
	BytesRead     int64
	CRCErrors     int
	SeriesCount   int
	SamplesCount  int
}

// NewWALReader는 새 WALReader를 생성한다.
func NewWALReader(dir string) *WALReader {
	return &WALReader{
		dir:    dir,
		Series: make(map[uint64]SeriesRecord),
	}
}

// listSegmentFiles는 디렉토리에서 세그먼트 파일 인덱스 목록을 반환한다.
// 실제: wlog.listSegments() → 디렉토리 스캔 후 파일명 파싱
func listSegmentFiles(dir string) ([]int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var indices []int
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		idx, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // 세그먼트 파일이 아닌 것은 무시
		}
		indices = append(indices, idx)
	}

	sort.Ints(indices)
	return indices, nil
}

// ReadAll은 WAL 디렉토리의 모든 세그먼트에서 레코드를 읽는다.
// 실제 Prometheus에서는 Head 블록 복구 시 WAL을 처음부터 끝까지 읽는다.
// (tsdb/head_wal.go의 Replay 함수)
func (r *WALReader) ReadAll() error {
	indices, err := listSegmentFiles(r.dir)
	if err != nil {
		return fmt.Errorf("세그먼트 목록 조회 실패: %w", err)
	}

	for _, idx := range indices {
		segPath := filepath.Join(r.dir, fmt.Sprintf("%08d", idx))
		if err := r.readSegment(segPath); err != nil {
			return fmt.Errorf("세그먼트 %08d 읽기 실패: %w", idx, err)
		}
		r.SegmentsRead++
	}

	return nil
}

// readSegment는 단일 세그먼트 파일에서 모든 레코드를 읽는다.
func (r *WALReader) readSegment(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	for {
		// 헤더 읽기: type(1B) + length(4B)
		header := make([]byte, recordHeaderSize)
		_, err := io.ReadFull(f, header)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break // 세그먼트 끝
		}
		if err != nil {
			return fmt.Errorf("헤더 읽기 실패: %w", err)
		}
		r.BytesRead += int64(recordHeaderSize)

		recordType := header[0]
		dataLen := binary.BigEndian.Uint32(header[1:5])

		// 데이터 읽기
		data := make([]byte, dataLen)
		_, err = io.ReadFull(f, data)
		if err != nil {
			return fmt.Errorf("데이터 읽기 실패 (길이 %d): %w", dataLen, err)
		}
		r.BytesRead += int64(dataLen)

		// CRC32 읽기 및 검증
		crcBuf := make([]byte, 4)
		_, err = io.ReadFull(f, crcBuf)
		if err != nil {
			return fmt.Errorf("CRC 읽기 실패: %w", err)
		}
		r.BytesRead += 4

		expectedCRC := binary.BigEndian.Uint32(crcBuf)
		actualCRC := crc32.Checksum(data, castagnoliTable)

		if expectedCRC != actualCRC {
			r.CRCErrors++
			fmt.Printf("  [경고] CRC 불일치! 기대: 0x%08X, 실제: 0x%08X\n", expectedCRC, actualCRC)
			continue // 손상된 레코드 건너뜀
		}

		// 레코드 디코딩
		switch recordType {
		case RecordTypeSeries:
			series, err := decodeSeriesRecord(data)
			if err != nil {
				return fmt.Errorf("시리즈 디코딩 실패: %w", err)
			}
			r.Series[series.Ref] = series
			r.SeriesCount++

		case RecordTypeSamples:
			sample, err := decodeSampleRecord(data)
			if err != nil {
				return fmt.Errorf("샘플 디코딩 실패: %w", err)
			}
			r.Samples = append(r.Samples, sample)
			r.SamplesCount++

		default:
			fmt.Printf("  [경고] 알 수 없는 레코드 타입: %d\n", recordType)
		}

		r.RecordsRead++
	}

	return nil
}

// =============================================================================
// 유틸리티
// =============================================================================

func formatBytes(b int64) string {
	const (
		KB = 1024
		MB = 1024 * KB
	)
	switch {
	case b >= MB:
		return fmt.Sprintf("%.2f MB", float64(b)/float64(MB))
	case b >= KB:
		return fmt.Sprintf("%.2f KB", float64(b)/float64(KB))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func printSeparator(title string) {
	line := strings.Repeat("=", 70)
	fmt.Printf("\n%s\n  %s\n%s\n", line, title, line)
}

// =============================================================================
// 메인: 데모 시나리오
// =============================================================================

func main() {
	fmt.Println("Prometheus WAL (Write-Ahead Log) PoC")
	fmt.Println("기반: tsdb/wlog/wlog.go, tsdb/record/record.go")
	fmt.Println()

	// 임시 디렉토리 생성
	tmpDir, err := os.MkdirTemp("", "prometheus-wal-poc-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "임시 디렉토리 생성 실패: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmpDir)
	fmt.Printf("WAL 디렉토리: %s\n", tmpDir)

	// =========================================================================
	// 시나리오 1: WAL 쓰기 + 세그먼트 로테이션
	// =========================================================================
	printSeparator("시나리오 1: WAL 쓰기 + 세그먼트 로테이션")

	walDir := filepath.Join(tmpDir, "wal")
	wal, err := NewWAL(walDir, defaultSegmentSize)
	if err != nil {
		fmt.Fprintf(os.Stderr, "WAL 생성 실패: %v\n", err)
		os.Exit(1)
	}

	// 시리즈 정의 (10개 시리즈)
	seriesCount := 10
	fmt.Printf("\n[1] %d개 시리즈 레코드 기록 중...\n", seriesCount)
	for i := 0; i < seriesCount; i++ {
		series := SeriesRecord{
			Ref: uint64(i + 1),
			Labels: []Label{
				{Name: "__name__", Value: fmt.Sprintf("http_requests_total_%d", i)},
				{Name: "method", Value: "GET"},
				{Name: "handler", Value: fmt.Sprintf("/api/v1/endpoint_%d", i)},
				{Name: "instance", Value: fmt.Sprintf("node-%d:9090", i%3)},
			},
		}
		if err := wal.LogSeries(series); err != nil {
			fmt.Fprintf(os.Stderr, "시리즈 기록 실패: %v\n", err)
			os.Exit(1)
		}
	}
	fmt.Printf("  시리즈 레코드 %d개 기록 완료\n", seriesCount)

	// 샘플 기록 (1000개 이상)
	sampleCount := 1500
	fmt.Printf("\n[2] %d개 샘플 레코드 기록 중...\n", sampleCount)

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	baseTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()

	for i := 0; i < sampleCount; i++ {
		sample := SampleRecord{
			Ref:       uint64((i % seriesCount) + 1),
			Timestamp: baseTime + int64(i)*15000, // 15초 간격
			Value:     rng.Float64() * 1000,
		}
		if err := wal.LogSample(sample); err != nil {
			fmt.Fprintf(os.Stderr, "샘플 기록 실패: %v\n", err)
			os.Exit(1)
		}
	}
	fmt.Printf("  샘플 레코드 %d개 기록 완료\n", sampleCount)

	// WAL 통계
	fmt.Printf("\n%s\n", wal.Stats())

	// 세그먼트 파일 확인
	fmt.Printf("\n[3] 세그먼트 파일 목록:\n")
	segments, _ := listSegmentFiles(walDir)
	for _, idx := range segments {
		segPath := filepath.Join(walDir, fmt.Sprintf("%08d", idx))
		info, _ := os.Stat(segPath)
		fmt.Printf("  세그먼트 %08d: %s\n", idx, formatBytes(info.Size()))
	}

	// WAL 닫기
	if err := wal.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "WAL 닫기 실패: %v\n", err)
	}

	// =========================================================================
	// 시나리오 2: WAL 읽기 + 데이터 검증
	// =========================================================================
	printSeparator("시나리오 2: WAL 읽기 + CRC 검증 + 데이터 복원")

	reader := NewWALReader(walDir)
	if err := reader.ReadAll(); err != nil {
		fmt.Fprintf(os.Stderr, "WAL 읽기 실패: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\n복원 결과:\n")
	fmt.Printf("  세그먼트 읽음: %d개\n", reader.SegmentsRead)
	fmt.Printf("  레코드 읽음: %d개\n", reader.RecordsRead)
	fmt.Printf("  바이트 읽음: %s\n", formatBytes(reader.BytesRead))
	fmt.Printf("  CRC 오류: %d개\n", reader.CRCErrors)
	fmt.Printf("  시리즈 복원: %d개\n", reader.SeriesCount)
	fmt.Printf("  샘플 복원: %d개\n", reader.SamplesCount)

	// 데이터 검증
	fmt.Printf("\n데이터 무결성 검증:\n")
	if reader.SeriesCount == seriesCount {
		fmt.Printf("  [OK] 시리즈 수 일치: %d == %d\n", reader.SeriesCount, seriesCount)
	} else {
		fmt.Printf("  [FAIL] 시리즈 수 불일치: %d != %d\n", reader.SeriesCount, seriesCount)
	}
	if reader.SamplesCount == sampleCount {
		fmt.Printf("  [OK] 샘플 수 일치: %d == %d\n", reader.SamplesCount, sampleCount)
	} else {
		fmt.Printf("  [FAIL] 샘플 수 불일치: %d != %d\n", reader.SamplesCount, sampleCount)
	}
	if reader.CRCErrors == 0 {
		fmt.Printf("  [OK] CRC 오류 없음 — 모든 레코드 무결\n")
	}

	// 복원된 시리즈 샘플 출력
	fmt.Printf("\n복원된 시리즈 정보 (처음 3개):\n")
	count := 0
	for ref, s := range reader.Series {
		if count >= 3 {
			break
		}
		labels := make([]string, 0, len(s.Labels))
		for _, l := range s.Labels {
			labels = append(labels, fmt.Sprintf("%s=%q", l.Name, l.Value))
		}
		fmt.Printf("  ref=%d: {%s}\n", ref, strings.Join(labels, ", "))
		count++
	}

	fmt.Printf("\n복원된 샘플 데이터 (처음 5개):\n")
	for i := 0; i < 5 && i < len(reader.Samples); i++ {
		s := reader.Samples[i]
		ts := time.UnixMilli(s.Timestamp).Format("2006-01-02 15:04:05")
		fmt.Printf("  ref=%d, ts=%s, value=%.4f\n", s.Ref, ts, s.Value)
	}

	// =========================================================================
	// 시나리오 3: 크래시 복구 시뮬레이션
	// =========================================================================
	printSeparator("시나리오 3: 크래시 복구 시뮬레이션")

	crashDir := filepath.Join(tmpDir, "wal-crash")
	fmt.Println("\n[1단계] WAL에 데이터 기록 후 '크래시' 시뮬레이션")

	crashWAL, err := NewWAL(crashDir, defaultSegmentSize)
	if err != nil {
		fmt.Fprintf(os.Stderr, "크래시 WAL 생성 실패: %v\n", err)
		os.Exit(1)
	}

	// 시리즈 5개 기록
	crashSeriesCount := 5
	for i := 0; i < crashSeriesCount; i++ {
		if err := crashWAL.LogSeries(SeriesRecord{
			Ref: uint64(i + 1),
			Labels: []Label{
				{Name: "__name__", Value: fmt.Sprintf("cpu_usage_%d", i)},
				{Name: "host", Value: fmt.Sprintf("server-%d", i)},
			},
		}); err != nil {
			fmt.Fprintf(os.Stderr, "크래시 시리즈 기록 실패: %v\n", err)
			os.Exit(1)
		}
	}

	// 샘플 200개 기록
	crashSampleCount := 200
	for i := 0; i < crashSampleCount; i++ {
		if err := crashWAL.LogSample(SampleRecord{
			Ref:       uint64((i % crashSeriesCount) + 1),
			Timestamp: baseTime + int64(i)*30000,
			Value:     float64(i) * 0.5,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "크래시 샘플 기록 실패: %v\n", err)
			os.Exit(1)
		}
	}

	fmt.Printf("  기록: 시리즈 %d개, 샘플 %d개\n", crashSeriesCount, crashSampleCount)

	// 크래시 시뮬레이션: Close() 없이 파일 핸들만 닫음
	// 실제 크래시에서는 flush되지 않은 버퍼 데이터가 유실될 수 있다.
	// 이 PoC에서는 Sync()를 호출하여 디스크에 확실히 기록된 상태에서 종료한다.
	crashWAL.segment.Sync()
	crashWAL.segment.Close()
	fmt.Println("  '크래시' 발생! (비정상 종료 시뮬레이션)")

	// 복구 시도
	fmt.Println("\n[2단계] WAL에서 데이터 복구")
	crashReader := NewWALReader(crashDir)
	if err := crashReader.ReadAll(); err != nil {
		fmt.Fprintf(os.Stderr, "크래시 WAL 읽기 실패: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("  복구된 시리즈: %d개 (원본: %d개)\n", crashReader.SeriesCount, crashSeriesCount)
	fmt.Printf("  복구된 샘플: %d개 (원본: %d개)\n", crashReader.SamplesCount, crashSampleCount)
	fmt.Printf("  CRC 오류: %d개\n", crashReader.CRCErrors)

	if crashReader.SeriesCount == crashSeriesCount && crashReader.SamplesCount == crashSampleCount {
		fmt.Println("  [OK] 크래시 복구 성공! 모든 데이터가 무결하게 복원됨")
	}

	// 복구 후 새 데이터 추가
	fmt.Println("\n[3단계] 복구 후 WAL 재개 (새 세그먼트에 이어쓰기)")
	resumeWAL, err := OpenWAL(crashDir, defaultSegmentSize)
	if err != nil {
		fmt.Fprintf(os.Stderr, "WAL 재개 실패: %v\n", err)
		os.Exit(1)
	}

	newSamples := 50
	for i := 0; i < newSamples; i++ {
		if err := resumeWAL.LogSample(SampleRecord{
			Ref:       uint64((i % crashSeriesCount) + 1),
			Timestamp: baseTime + int64(crashSampleCount+i)*30000,
			Value:     float64(crashSampleCount+i) * 0.5,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "재개 샘플 기록 실패: %v\n", err)
			os.Exit(1)
		}
	}
	resumeWAL.Close()
	fmt.Printf("  새 샘플 %d개 추가 기록\n", newSamples)

	// 전체 데이터 다시 읽기
	finalReader := NewWALReader(crashDir)
	if err := finalReader.ReadAll(); err != nil {
		fmt.Fprintf(os.Stderr, "최종 WAL 읽기 실패: %v\n", err)
		os.Exit(1)
	}

	expectedTotal := crashSampleCount + newSamples
	fmt.Printf("\n최종 복원 결과:\n")
	fmt.Printf("  세그먼트: %d개\n", finalReader.SegmentsRead)
	fmt.Printf("  시리즈: %d개\n", finalReader.SeriesCount)
	fmt.Printf("  샘플: %d개 (기대: %d개)\n", finalReader.SamplesCount, expectedTotal)
	if finalReader.SamplesCount == expectedTotal && finalReader.CRCErrors == 0 {
		fmt.Println("  [OK] 크래시 복구 + 재개 성공! 데이터 무결성 확인")
	}

	// =========================================================================
	// 시나리오 4: CRC 손상 감지
	// =========================================================================
	printSeparator("시나리오 4: CRC 손상 감지 (의도적 데이터 훼손)")

	corruptDir := filepath.Join(tmpDir, "wal-corrupt")
	corruptWAL, err := NewWAL(corruptDir, defaultSegmentSize)
	if err != nil {
		fmt.Fprintf(os.Stderr, "손상 테스트 WAL 생성 실패: %v\n", err)
		os.Exit(1)
	}

	// 데이터 기록
	for i := 0; i < 20; i++ {
		corruptWAL.LogSample(SampleRecord{
			Ref:       uint64(i%3 + 1),
			Timestamp: baseTime + int64(i)*15000,
			Value:     float64(i) * 1.1,
		})
	}
	corruptWAL.Close()

	// 세그먼트 파일 의도적으로 손상
	corruptSegs, _ := listSegmentFiles(corruptDir)
	if len(corruptSegs) > 0 {
		segPath := filepath.Join(corruptDir, fmt.Sprintf("%08d", corruptSegs[0]))
		data, _ := os.ReadFile(segPath)

		// 세 번째 레코드의 데이터 부분을 손상시킴 (오프셋 계산)
		// 각 샘플 레코드: header(5) + data(24) + crc(4) = 33 bytes
		// 세 번째 레코드 데이터 시작: 33*2 + 5 = 71
		if len(data) > 75 {
			fmt.Printf("  세그먼트 %08d의 71번째 바이트 손상시킴 (원본: 0x%02X)\n",
				corruptSegs[0], data[71])
			data[71] ^= 0xFF // 비트 반전
			os.WriteFile(segPath, data, 0o644)
		}
	}

	// 손상된 WAL 읽기
	corruptReader := NewWALReader(corruptDir)
	if err := corruptReader.ReadAll(); err != nil {
		fmt.Printf("  [INFO] WAL 읽기 중 오류: %v\n", err)
	}

	fmt.Printf("\n손상 감지 결과:\n")
	fmt.Printf("  읽은 레코드: %d개\n", corruptReader.RecordsRead)
	fmt.Printf("  CRC 오류: %d개\n", corruptReader.CRCErrors)
	fmt.Printf("  복구된 샘플: %d개 (원본: 20개)\n", corruptReader.SamplesCount)
	if corruptReader.CRCErrors > 0 {
		fmt.Println("  [OK] CRC32 체크섬으로 데이터 손상 감지 성공!")
		fmt.Println("  → 실제 Prometheus: CorruptionErr 반환 후 Repair() 호출")
	}

	// =========================================================================
	// 요약
	// =========================================================================
	printSeparator("WAL PoC 요약")
	fmt.Println(`
  Prometheus WAL 핵심 설계 원리:

  1. 순차 쓰기 (Sequential Write)
     → 디스크 I/O 최적화. 랜덤 쓰기 대비 10~100배 빠름
     → 실제: wlog.Log() → page 버퍼 → segment 파일

  2. CRC32 체크섬 (Castagnoli)
     → 모든 레코드에 CRC32 체크섬 부착
     → 읽기 시 검증하여 비트 오류, 부분 쓰기 감지
     → 실제: castagnoliTable = crc32.MakeTable(crc32.Castagnoli)

  3. 세그먼트 로테이션
     → 고정 크기 세그먼트로 분할 (기본 128MB)
     → 오래된 세그먼트 삭제로 디스크 공간 회수
     → 실제: wlog.nextSegment() → CreateSegment()

  4. 크래시 복구
     → 프로세스 재시작 시 WAL 처음부터 순차 읽기
     → CRC 검증 실패 레코드는 건너뛰거나 Repair()
     → 실제: head_wal.go Replay() → 시리즈/샘플 복원

  5. 페이지 기반 버퍼링 (이 PoC에서는 생략)
     → 32KB 페이지 단위 배치 쓰기
     → 레코드가 페이지 경계를 넘으면 First/Middle/Last로 분할
     → 실제: recordHeaderSize=7, pageSize=32KB
`)
	fmt.Println("임시 디렉토리 정리 완료.")
}
