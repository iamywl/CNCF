package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
	"time"
)

// =============================================================================
// Loki PoC #17: DataObj 컬럼나 데이터 포맷 인코딩/디코딩
// =============================================================================
//
// Loki의 DataObj(pkg/dataobj/)는 로그 데이터를 컬럼나 형식으로 저장하는
// 차세대 스토리지 포맷이다. 기존 행 기반 저장 대비 압축률과 쿼리 성능을
// 크게 향상시킨다.
//
// 핵심 개념:
// 1. 컬럼나 저장: timestamp, labels, line을 별도 컬럼에 저장
// 2. 섹션 기반 레이아웃: Header → Sections(metadata+data) → Tailer
// 3. 딕셔너리 인코딩: 반복되는 레이블 값을 딕셔너리로 압축
// 4. 컬럼 프루닝: 쿼리에 필요한 컬럼만 선택적으로 읽기
//
// 참조: pkg/dataobj/encoder.go, decoder.go, dataobj.go

// =============================================================================
// 파일 포맷 상수
// =============================================================================
// Loki 실제 코드: pkg/dataobj/encoder.go

var (
	// Magic 바이트: 파일 시작과 끝에 위치하여 포맷 식별
	// Loki 실제 코드: magic = []byte("DOBJ"), legacyMagic = []byte("THOR")
	Magic          = []byte("DOBJ")
	FormatVersion  = uint32(1)
)

// =============================================================================
// SectionType: 섹션 유형
// =============================================================================
// Loki 실제 코드: pkg/dataobj/dataobj.go → SectionType

type SectionType struct {
	Namespace string // 예: "dataobj"
	Kind      string // 예: "logs", "streams"
	Version   uint32
}

var (
	SectionTypeLogs    = SectionType{Namespace: "dataobj", Kind: "logs", Version: 1}
	SectionTypeStreams = SectionType{Namespace: "dataobj", Kind: "streams", Version: 1}
)

func (st SectionType) String() string {
	return fmt.Sprintf("%s.%s.v%d", st.Namespace, st.Kind, st.Version)
}

// =============================================================================
// Column: 컬럼 데이터
// =============================================================================
// Loki 실제 코드: pkg/dataobj/internal/dataset/column.go
//
// 컬럼나 저장에서 각 필드(timestamp, labels, line)는 별도의 컬럼에 저장된다.
// 이를 통해:
// - 같은 타입의 데이터끼리 모아서 압축률 향상
// - 쿼리에 필요한 컬럼만 읽어서 I/O 감소

type ColumnType int

const (
	ColumnTypeTimestamp ColumnType = iota
	ColumnTypeLabel
	ColumnTypeLine
)

func (ct ColumnType) String() string {
	switch ct {
	case ColumnTypeTimestamp:
		return "timestamp"
	case ColumnTypeLabel:
		return "label"
	case ColumnTypeLine:
		return "line"
	default:
		return "unknown"
	}
}

// Column 은 하나의 컬럼 데이터를 나타낸다
type Column struct {
	Type   ColumnType
	Name   string   // 컬럼 이름 (예: "timestamp", "level", "message")
	Values []string // 문자열화된 값 목록
}

// EncodedSize 는 인코딩된 컬럼의 크기를 반환한다
func (c *Column) EncodedSize() int {
	size := 0
	for _, v := range c.Values {
		size += len(v)
	}
	return size
}

// =============================================================================
// DictionaryEncoder: 딕셔너리 인코딩
// =============================================================================
// Loki 실제 코드: pkg/dataobj/encoder.go → dictionary, dictionaryLookup
//
// 반복되는 문자열 값을 정수 인덱스로 치환하여 저장 공간을 절약한다.
// 예: "error" → 0, "info" → 1, "warn" → 2
//     원본: ["error", "info", "error", "error", "info"]
//     인코딩: [0, 1, 0, 0, 1] + 딕셔너리: {0:"error", 1:"info"}

type DictionaryEncoder struct {
	dictionary map[string]uint32 // 값 → 인덱스
	entries    []string          // 인덱스 → 값
}

func NewDictionaryEncoder() *DictionaryEncoder {
	return &DictionaryEncoder{
		dictionary: make(map[string]uint32),
		entries:    []string{""},  // 인덱스 0은 빈 문자열 (Loki 실제 코드와 동일)
	}
}

// Encode 는 문자열을 딕셔너리 인덱스로 변환한다
// Loki 실제 코드: encoder.getDictionaryKey()
func (de *DictionaryEncoder) Encode(value string) uint32 {
	if idx, ok := de.dictionary[value]; ok {
		return idx
	}
	idx := uint32(len(de.entries))
	de.dictionary[value] = idx
	de.entries = append(de.entries, value)
	return idx
}

// Decode 는 딕셔너리 인덱스를 문자열로 변환한다
func (de *DictionaryEncoder) Decode(idx uint32) string {
	if int(idx) < len(de.entries) {
		return de.entries[idx]
	}
	return ""
}

// Size 는 딕셔너리의 크기를 반환한다
func (de *DictionaryEncoder) Size() int {
	return len(de.entries) - 1 // 빈 문자열 제외
}

// Entries 는 딕셔너리 항목을 반환한다
func (de *DictionaryEncoder) Entries() []string {
	return de.entries[1:] // 빈 문자열 제외
}

// =============================================================================
// Section: DataObj의 섹션
// =============================================================================
// Loki 실제 코드: pkg/dataobj/encoder.go → sectionInfo
//
// 각 섹션은 데이터와 메타데이터로 구성된다.
// 메타데이터 영역은 파일 앞쪽에, 데이터 영역은 뒤쪽에 배치하여
// 메타데이터를 먼저 읽어 필요한 데이터만 선택적으로 접근할 수 있다.

type Section struct {
	Type         SectionType
	Tenant       string
	Columns      []Column
	MetadataSize int
	DataSize     int
}

// =============================================================================
// DataObjEncoder: DataObj 인코더
// =============================================================================
// Loki 실제 코드: pkg/dataobj/encoder.go → encoder

type DataObjEncoder struct {
	sections   []Section
	dictionary *DictionaryEncoder
	totalBytes int
}

func NewDataObjEncoder() *DataObjEncoder {
	return &DataObjEncoder{
		dictionary: NewDictionaryEncoder(),
	}
}

// AddLogsSection 은 로그 데이터를 컬럼나 형식으로 변환하여 섹션을 추가한다
func (enc *DataObjEncoder) AddLogsSection(tenant string, entries []LogEntry) {
	// 로그 엔트리를 컬럼별로 분리
	// 컬럼나 저장의 핵심: 행(row) → 열(column) 변환

	n := len(entries)

	// 1. 타임스탬프 컬럼
	tsColumn := Column{
		Type:   ColumnTypeTimestamp,
		Name:   "timestamp",
		Values: make([]string, n),
	}

	// 2. 로그 라인 컬럼
	lineColumn := Column{
		Type:   ColumnTypeLine,
		Name:   "line",
		Values: make([]string, n),
	}

	// 3. 레이블 컬럼들 (레이블 키별로 별도 컬럼)
	labelColumns := make(map[string]*Column)

	for i, entry := range entries {
		tsColumn.Values[i] = fmt.Sprintf("%d", entry.Timestamp.UnixNano())
		lineColumn.Values[i] = entry.Line

		for k, v := range entry.Labels {
			if _, ok := labelColumns[k]; !ok {
				labelColumns[k] = &Column{
					Type:   ColumnTypeLabel,
					Name:   k,
					Values: make([]string, n),
				}
			}
			labelColumns[k].Values[i] = v
			// 딕셔너리에 등록 (반복되는 레이블 값 압축)
			enc.dictionary.Encode(v)
		}
	}

	// 컬럼 목록 구성
	columns := []Column{tsColumn, lineColumn}
	// 레이블 컬럼을 이름순으로 정렬하여 추가
	labelKeys := make([]string, 0, len(labelColumns))
	for k := range labelColumns {
		labelKeys = append(labelKeys, k)
	}
	sort.Strings(labelKeys)
	for _, k := range labelKeys {
		columns = append(columns, *labelColumns[k])
	}

	// 크기 계산
	dataSize := 0
	for _, col := range columns {
		dataSize += col.EncodedSize()
	}
	metadataSize := len(columns) * 32 // 메타데이터 오버헤드 추정

	section := Section{
		Type:         SectionTypeLogs,
		Tenant:       tenant,
		Columns:      columns,
		MetadataSize: metadataSize,
		DataSize:     dataSize,
	}

	enc.sections = append(enc.sections, section)
	enc.totalBytes += dataSize + metadataSize
}

// Encode 는 DataObj를 바이트 스트림으로 인코딩한다
// Loki 실제 코드: encoder.Flush() → snapshot
//
// 파일 레이아웃:
// ┌──────────────────────┐
// │ Header               │
// │  - Magic ("DOBJ")    │
// │  - Metadata Size     │
// │  - Format Version    │
// │  - File Metadata     │
// ├──────────────────────┤
// │ Body                 │
// │  - Metadata Regions  │ ← 먼저 배치 (prefetch 최적화)
// │  - Data Regions      │
// ├──────────────────────┤
// │ Tailer               │
// │  - Magic ("DOBJ")    │
// └──────────────────────┘
func (enc *DataObjEncoder) Encode() ([]byte, *FileMetadata) {
	var buf bytes.Buffer

	// Header: Magic + 메타데이터 크기 + 버전
	buf.Write(Magic)

	// 메타데이터 크기 (placeholder)
	metadataSizeOffset := buf.Len()
	binary.Write(&buf, binary.LittleEndian, uint32(0)) // placeholder

	// 포맷 버전
	binary.Write(&buf, binary.LittleEndian, FormatVersion)

	// 파일 메타데이터
	metadata := enc.buildMetadata()
	metadataBytes := encodeMetadata(metadata)
	metadataSize := len(metadataBytes)
	buf.Write(metadataBytes)

	// 메타데이터 크기 업데이트
	copy(buf.Bytes()[metadataSizeOffset:], encodeUint32(uint32(metadataSize)+4)) // +4 for version

	// Body: 각 섹션의 데이터
	for _, section := range enc.sections {
		for _, col := range section.Columns {
			for _, v := range col.Values {
				buf.Write([]byte(v))
				buf.WriteByte(0) // null separator
			}
		}
	}

	// Tailer: Magic
	buf.Write(Magic)

	return buf.Bytes(), metadata
}

// buildMetadata 는 파일 메타데이터를 구성한다
func (enc *DataObjEncoder) buildMetadata() *FileMetadata {
	sections := make([]SectionMetadata, len(enc.sections))
	offset := 0

	for i, section := range enc.sections {
		sections[i] = SectionMetadata{
			Type:           section.Type,
			Tenant:         section.Tenant,
			MetadataOffset: offset,
			MetadataSize:   section.MetadataSize,
			ColumnCount:    len(section.Columns),
			ColumnNames:    getColumnNames(section.Columns),
		}
		offset += section.MetadataSize
	}

	// 데이터 오프셋 업데이트
	for i, section := range enc.sections {
		sections[i].DataOffset = offset
		sections[i].DataSize = section.DataSize
		offset += section.DataSize
	}

	return &FileMetadata{
		Sections:   sections,
		Dictionary: enc.dictionary.Entries(),
		TypeRefs:   getUniqueTypes(enc.sections),
	}
}

// =============================================================================
// FileMetadata: 파일 메타데이터
// =============================================================================

type FileMetadata struct {
	Sections   []SectionMetadata
	Dictionary []string
	TypeRefs   []SectionType
}

type SectionMetadata struct {
	Type           SectionType
	Tenant         string
	MetadataOffset int
	MetadataSize   int
	DataOffset     int
	DataSize       int
	ColumnCount    int
	ColumnNames    []string
}

// =============================================================================
// DataObjDecoder: DataObj 디코더 (선택적 컬럼 읽기)
// =============================================================================
// Loki 실제 코드: pkg/dataobj/decoder.go
//
// 컬럼 프루닝: 쿼리에 필요한 컬럼만 선택적으로 읽어 I/O를 최소화

type DataObjDecoder struct {
	data     []byte
	metadata *FileMetadata
}

// NewDataObjDecoder 는 디코더를 생성한다
func NewDataObjDecoder(data []byte, metadata *FileMetadata) (*DataObjDecoder, error) {
	// Magic 바이트 검증
	if len(data) < len(Magic)*2 {
		return nil, fmt.Errorf("데이터가 너무 작습니다")
	}

	// Header Magic 확인
	if !bytes.Equal(data[:len(Magic)], Magic) {
		return nil, fmt.Errorf("잘못된 파일 형식: header magic 불일치")
	}

	// Tailer Magic 확인
	if !bytes.Equal(data[len(data)-len(Magic):], Magic) {
		return nil, fmt.Errorf("잘못된 파일 형식: tailer magic 불일치")
	}

	return &DataObjDecoder{
		data:     data,
		metadata: metadata,
	}, nil
}

// ListSections 은 파일에 포함된 섹션 목록을 반환한다
func (dec *DataObjDecoder) ListSections() []SectionMetadata {
	return dec.metadata.Sections
}

// ReadColumns 은 지정된 컬럼만 선택적으로 읽는다 (컬럼 프루닝)
// 전체 섹션을 읽지 않고 필요한 컬럼만 읽어 I/O 최소화
func (dec *DataObjDecoder) ReadColumns(sectionIdx int, columnNames []string) map[string][]string {
	if sectionIdx >= len(dec.metadata.Sections) {
		return nil
	}

	// 실제 구현에서는 오프셋 기반으로 특정 컬럼만 읽지만,
	// 시뮬레이션에서는 메타데이터의 컬럼 이름으로 필터링
	result := make(map[string][]string)
	section := dec.metadata.Sections[sectionIdx]

	// 요청된 컬럼만 포함
	requestedSet := make(map[string]bool)
	for _, name := range columnNames {
		requestedSet[name] = true
	}

	for _, name := range section.ColumnNames {
		if requestedSet[name] {
			result[name] = []string{} // 실제로는 데이터를 디코딩
		}
	}

	return result
}

// =============================================================================
// 로그 엔트리
// =============================================================================

type LogEntry struct {
	Timestamp time.Time
	Labels    map[string]string
	Line      string
}

// =============================================================================
// 헬퍼 함수
// =============================================================================

func getColumnNames(columns []Column) []string {
	names := make([]string, len(columns))
	for i, col := range columns {
		names[i] = col.Name
	}
	return names
}

func getUniqueTypes(sections []Section) []SectionType {
	seen := make(map[string]bool)
	var types []SectionType
	for _, s := range sections {
		key := s.Type.String()
		if !seen[key] {
			seen[key] = true
			types = append(types, s.Type)
		}
	}
	return types
}

func encodeMetadata(md *FileMetadata) []byte {
	// 간단한 메타데이터 직렬화
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "sections:%d,dict:%d", len(md.Sections), len(md.Dictionary))
	return buf.Bytes()
}

func encodeUint32(v uint32) []byte {
	buf := make([]byte, 4)
	binary.LittleEndian.PutUint32(buf, v)
	return buf
}

// =============================================================================
// 메인 함수
// =============================================================================

func main() {
	fmt.Println("=== Loki DataObj 컬럼나 포맷 시뮬레이션 ===")
	fmt.Println()

	// ─────────────────────────────────────────────────────────────
	// 1단계: 파일 레이아웃 설명
	// ─────────────────────────────────────────────────────────────
	fmt.Println("--- [1] DataObj 파일 레이아웃 ---")
	fmt.Println()
	fmt.Println("  ┌──────────────────────────────────┐")
	fmt.Println("  │ Header                           │")
	fmt.Println("  │  Magic: \"DOBJ\" (4 bytes)         │")
	fmt.Println("  │  Metadata Size (4 bytes)         │")
	fmt.Println("  │  Format Version (varint)         │")
	fmt.Println("  │  File Metadata (protobuf)        │")
	fmt.Println("  │    - Section Types               │")
	fmt.Println("  │    - Dictionary                  │")
	fmt.Println("  │    - Section Offsets              │")
	fmt.Println("  ├──────────────────────────────────┤")
	fmt.Println("  │ Body                             │")
	fmt.Println("  │  [Metadata Regions]              │ ← prefetch 최적화")
	fmt.Println("  │  [Data Regions]                  │")
	fmt.Println("  ├──────────────────────────────────┤")
	fmt.Println("  │ Tailer                           │")
	fmt.Println("  │  Magic: \"DOBJ\" (4 bytes)         │")
	fmt.Println("  └──────────────────────────────────┘")
	fmt.Println()

	// ─────────────────────────────────────────────────────────────
	// 2단계: 행 기반 vs 컬럼나 비교
	// ─────────────────────────────────────────────────────────────
	fmt.Println("--- [2] 행 기반 vs 컬럼나 저장 비교 ---")
	fmt.Println()

	baseTime := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	entries := []LogEntry{
		{Timestamp: baseTime, Labels: map[string]string{"service": "api", "level": "info"}, Line: "Request received from 10.0.0.1"},
		{Timestamp: baseTime.Add(1 * time.Second), Labels: map[string]string{"service": "api", "level": "info"}, Line: "Processing request for /users"},
		{Timestamp: baseTime.Add(2 * time.Second), Labels: map[string]string{"service": "api", "level": "error"}, Line: "Database connection timeout"},
		{Timestamp: baseTime.Add(3 * time.Second), Labels: map[string]string{"service": "worker", "level": "info"}, Line: "Job completed successfully"},
		{Timestamp: baseTime.Add(4 * time.Second), Labels: map[string]string{"service": "api", "level": "info"}, Line: "Request received from 10.0.0.2"},
		{Timestamp: baseTime.Add(5 * time.Second), Labels: map[string]string{"service": "worker", "level": "error"}, Line: "Job failed: resource exhausted"},
	}

	// 행 기반 저장 (기존)
	fmt.Println("  [행 기반 저장] 각 행에 모든 필드 포함:")
	fmt.Println("  ┌───────────────────┬─────────┬───────┬────────────────────────────────┐")
	fmt.Println("  │ timestamp         │ service │ level │ line                           │")
	fmt.Println("  ├───────────────────┼─────────┼───────┼────────────────────────────────┤")
	rowBytes := 0
	for _, e := range entries {
		ts := e.Timestamp.Format("15:04:05")
		line := e.Line
		if len(line) > 30 {
			line = line[:27] + "..."
		}
		row := fmt.Sprintf("  │ %17s │ %7s │ %5s │ %-30s │", ts, e.Labels["service"], e.Labels["level"], line)
		fmt.Println(row)
		rowBytes += len(ts) + len(e.Labels["service"]) + len(e.Labels["level"]) + len(e.Line)
	}
	fmt.Println("  └───────────────────┴─────────┴───────┴────────────────────────────────┘")
	fmt.Printf("  총 크기: %d bytes\n", rowBytes)
	fmt.Println()

	// 컬럼나 저장 (DataObj)
	fmt.Println("  [컬럼나 저장] 각 컬럼에 같은 타입의 데이터만:")
	fmt.Println()

	// 타임스탬프 컬럼
	fmt.Println("  timestamp 컬럼:")
	fmt.Print("    [")
	for i, e := range entries {
		if i > 0 {
			fmt.Print(", ")
		}
		fmt.Print(e.Timestamp.Format("15:04:05"))
	}
	fmt.Println("]")

	// 서비스 컬럼
	fmt.Println("  service 컬럼:")
	fmt.Print("    [")
	for i, e := range entries {
		if i > 0 {
			fmt.Print(", ")
		}
		fmt.Printf("%q", e.Labels["service"])
	}
	fmt.Println("]")

	// 레벨 컬럼
	fmt.Println("  level 컬럼:")
	fmt.Print("    [")
	for i, e := range entries {
		if i > 0 {
			fmt.Print(", ")
		}
		fmt.Printf("%q", e.Labels["level"])
	}
	fmt.Println("]")

	// 라인 컬럼
	fmt.Println("  line 컬럼:")
	fmt.Print("    [")
	for i, e := range entries {
		if i > 0 {
			fmt.Print(", ")
		}
		line := e.Line
		if len(line) > 25 {
			line = line[:22] + "..."
		}
		fmt.Printf("%q", line)
	}
	fmt.Println("]")
	fmt.Println()

	// ─────────────────────────────────────────────────────────────
	// 3단계: 딕셔너리 인코딩
	// ─────────────────────────────────────────────────────────────
	fmt.Println("--- [3] 딕셔너리 인코딩 ---")
	fmt.Println()

	dict := NewDictionaryEncoder()

	// 레이블 값들을 딕셔너리에 등록
	fmt.Println("  원본 레이블 값:")
	allLabelValues := make([]string, 0)
	for _, e := range entries {
		for _, v := range e.Labels {
			allLabelValues = append(allLabelValues, v)
		}
	}
	fmt.Printf("    %v\n", allLabelValues)
	fmt.Printf("    원본 크기: %d bytes\n", totalStringSize(allLabelValues))
	fmt.Println()

	// 딕셔너리 인코딩
	encodedIndices := make([]uint32, len(allLabelValues))
	for i, v := range allLabelValues {
		encodedIndices[i] = dict.Encode(v)
	}

	fmt.Println("  딕셔너리:")
	for i, entry := range dict.Entries() {
		fmt.Printf("    %d → %q\n", i+1, entry)
	}
	fmt.Println()

	fmt.Println("  인코딩 결과:")
	fmt.Printf("    인덱스: %v\n", encodedIndices)
	dictSize := 0
	for _, e := range dict.Entries() {
		dictSize += len(e)
	}
	encodedSize := len(encodedIndices) * 4 // uint32 = 4 bytes
	fmt.Printf("    딕셔너리 크기: %d bytes\n", dictSize)
	fmt.Printf("    인코딩 크기: %d bytes (인덱스 %d x 4)\n", encodedSize, len(encodedIndices))
	fmt.Printf("    총 인코딩 크기: %d bytes\n", dictSize+encodedSize)
	fmt.Printf("    압축률: %.1f%% 절약\n",
		float64(totalStringSize(allLabelValues)-dictSize-encodedSize)/float64(totalStringSize(allLabelValues))*100)
	fmt.Println()

	// 디코딩 검증
	fmt.Println("  디코딩 검증:")
	for i, idx := range encodedIndices {
		decoded := dict.Decode(idx)
		match := "OK"
		if decoded != allLabelValues[i] {
			match = "FAIL"
		}
		fmt.Printf("    인덱스 %d → %q [%s]\n", idx, decoded, match)
	}
	fmt.Println()

	// ─────────────────────────────────────────────────────────────
	// 4단계: DataObj 인코딩
	// ─────────────────────────────────────────────────────────────
	fmt.Println("--- [4] DataObj 인코딩 ---")
	fmt.Println()

	encoder := NewDataObjEncoder()

	// 테넌트별 로그 섹션 추가
	tenant1Entries := entries[:4]
	tenant2Entries := entries[4:]

	encoder.AddLogsSection("tenant-alpha", tenant1Entries)
	encoder.AddLogsSection("tenant-beta", tenant2Entries)

	// 인코딩
	encodedData, metadata := encoder.Encode()

	fmt.Printf("  인코딩된 DataObj 크기: %d bytes\n", len(encodedData))
	fmt.Printf("  Magic: %q (header), %q (tailer)\n",
		string(encodedData[:4]), string(encodedData[len(encodedData)-4:]))
	fmt.Println()

	fmt.Println("  섹션 정보:")
	for i, section := range metadata.Sections {
		fmt.Printf("    [섹션 %d] %s\n", i+1, section.Type.String())
		fmt.Printf("      테넌트: %s\n", section.Tenant)
		fmt.Printf("      컬럼 수: %d\n", section.ColumnCount)
		fmt.Printf("      컬럼: %v\n", section.ColumnNames)
		fmt.Printf("      메타데이터: offset=%d, size=%d\n", section.MetadataOffset, section.MetadataSize)
		fmt.Printf("      데이터: offset=%d, size=%d\n", section.DataOffset, section.DataSize)
		fmt.Println()
	}

	fmt.Println("  딕셔너리:")
	for i, entry := range metadata.Dictionary {
		fmt.Printf("    %d: %q\n", i, entry)
	}
	fmt.Println()

	// ─────────────────────────────────────────────────────────────
	// 5단계: 컬럼 프루닝 (선택적 읽기)
	// ─────────────────────────────────────────────────────────────
	fmt.Println("--- [5] 컬럼 프루닝 (선택적 읽기) ---")
	fmt.Println()

	decoder, err := NewDataObjDecoder(encodedData, metadata)
	if err != nil {
		fmt.Printf("  디코더 오류: %s\n", err)
		return
	}

	fmt.Println("  사용 가능한 섹션:")
	for i, section := range decoder.ListSections() {
		fmt.Printf("    [%d] %s (테넌트: %s, 컬럼: %v)\n",
			i, section.Type.String(), section.Tenant, section.ColumnNames)
	}
	fmt.Println()

	// 시나리오 1: timestamp + level만 읽기 (라인 스킵)
	fmt.Println("  쿼리 1: {level=\"error\"} → timestamp + level 컬럼만 읽기")
	pruned1 := decoder.ReadColumns(0, []string{"timestamp", "level"})
	fmt.Printf("    읽은 컬럼: %v\n", getKeys(pruned1))
	fmt.Printf("    → line 컬럼을 읽지 않아 I/O 절약!\n")
	fmt.Println()

	// 시나리오 2: line만 읽기 (grep)
	fmt.Println("  쿼리 2: grep \"timeout\" → line 컬럼만 읽기")
	pruned2 := decoder.ReadColumns(0, []string{"line"})
	fmt.Printf("    읽은 컬럼: %v\n", getKeys(pruned2))
	fmt.Printf("    → timestamp, label 컬럼을 읽지 않아 I/O 절약!\n")
	fmt.Println()

	// 시나리오 3: 전체 읽기 (full scan)
	fmt.Println("  쿼리 3: 전체 스캔 → 모든 컬럼 읽기")
	allCols := metadata.Sections[0].ColumnNames
	pruned3 := decoder.ReadColumns(0, allCols)
	fmt.Printf("    읽은 컬럼: %v\n", getKeys(pruned3))
	fmt.Println()

	// I/O 비교
	fmt.Println("  컬럼 프루닝 효과:")
	allColSize := 0
	for _, sec := range encoder.sections {
		for _, col := range sec.Columns {
			allColSize += col.EncodedSize()
		}
	}
	section0 := encoder.sections[0]
	tsLevelSize := 0
	lineOnlySize := 0
	for _, col := range section0.Columns {
		if col.Name == "timestamp" || col.Name == "level" {
			tsLevelSize += col.EncodedSize()
		}
		if col.Name == "line" {
			lineOnlySize += col.EncodedSize()
		}
	}

	fmt.Printf("    전체 읽기:          %d bytes (100%%)\n", allColSize)
	fmt.Printf("    timestamp+level:    %d bytes (%.1f%%)\n", tsLevelSize,
		float64(tsLevelSize)/float64(allColSize)*100)
	fmt.Printf("    line only:          %d bytes (%.1f%%)\n", lineOnlySize,
		float64(lineOnlySize)/float64(allColSize)*100)
	fmt.Println()

	// ─────────────────────────────────────────────────────────────
	// 6단계: 컬럼나 인코딩 기법 비교
	// ─────────────────────────────────────────────────────────────
	fmt.Println("--- [6] 컬럼나 인코딩 기법 ---")
	fmt.Println()

	// 딕셔너리 인코딩 (레이블)
	fmt.Println("  1. 딕셔너리 인코딩 (레이블 값):")
	fmt.Println("     원본: [\"api\", \"api\", \"api\", \"worker\", \"api\", \"worker\"]")
	fmt.Println("     딕셔너리: {0:\"api\", 1:\"worker\"}")
	fmt.Println("     인코딩: [0, 0, 0, 1, 0, 1]")
	fmt.Println("     → 반복되는 문자열을 정수로 치환하여 압축")
	fmt.Println()

	// 델타 인코딩 (타임스탬프)
	fmt.Println("  2. 델타 인코딩 (타임스탬프):")
	fmt.Println("     원본: [1000, 1001, 1002, 1003, 1004, 1005]")
	fmt.Println("     델타: [1000, +1, +1, +1, +1, +1]")
	fmt.Println("     → 연속 값의 차이만 저장하여 작은 수치로 압축")
	fmt.Println()

	// 비트맵 인코딩 (존재 여부)
	fmt.Println("  3. 비트맵 인코딩 (NULL 체크):")
	fmt.Println("     값 존재: [1, 1, 1, 0, 1, 0] → 0b110101 (1 byte)")
	fmt.Println("     → 존재 여부를 비트 단위로 저장하여 공간 절약")
	fmt.Println()

	// ─────────────────────────────────────────────────────────────
	// 동작 원리 요약
	// ─────────────────────────────────────────────────────────────
	fmt.Println("=== DataObj 컬럼나 포맷 동작 원리 요약 ===")
	fmt.Println()
	fmt.Println("  1. 컬럼나 저장: timestamp, labels, line을 별도 컬럼에 저장")
	fmt.Println("     → 같은 타입 데이터 모음 → 압축률 향상")
	fmt.Println("  2. 섹션 기반 레이아웃: Header → Metadata → Data → Tailer")
	fmt.Println("     → 메타데이터를 앞에 배치하여 prefetch 최적화")
	fmt.Println("  3. 딕셔너리 인코딩: 반복되는 레이블을 정수 인덱스로 치환")
	fmt.Println("     → 저장 공간 절약")
	fmt.Println("  4. 컬럼 프루닝: 쿼리에 필요한 컬럼만 선택적으로 읽기")
	fmt.Println("     → I/O 최소화")
	fmt.Println()
	fmt.Println("  Loki 핵심 코드 경로:")
	fmt.Println("  - pkg/dataobj/encoder.go    → DataObj 인코더, 딕셔너리, 섹션")
	fmt.Println("  - pkg/dataobj/decoder.go    → DataObj 디코더, 메타데이터 파싱")
	fmt.Println("  - pkg/dataobj/dataobj.go    → SectionType, Magic 바이트")
	fmt.Println("  - pkg/dataobj/internal/dataset/  → 컬럼 빌더, 인코딩 전략")
}

// =============================================================================
// 유틸 함수
// =============================================================================

func totalStringSize(ss []string) int {
	total := 0
	for _, s := range ss {
		total += len(s)
	}
	return total
}

func getKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func formatPercent(a, b int) string {
	if b == 0 {
		return "0%"
	}
	return fmt.Sprintf("%.1f%%", float64(a)/float64(b)*100)
}

func repeatStr(s string, n int) string {
	return strings.Repeat(s, n)
}
