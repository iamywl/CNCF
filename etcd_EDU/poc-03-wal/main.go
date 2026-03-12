// poc-03-wal: etcd Write-Ahead Log(WAL) 시뮬레이션
//
// etcd의 WAL은 모든 상태 변경을 디스크에 먼저 기록하여 장애 복구를 보장한다.
// 각 레코드는 Type + CRC32 + Data로 구성되며, 파일이 일정 크기를 초과하면
// 새 세그먼트 파일을 생성한다.
//
// 실제 구현 참조:
//   - server/storage/wal/wal.go: WAL 구조체, Create, ReadAll, Save
//   - server/storage/wal/encoder.go: encoder (CRC 계산, 8바이트 정렬 패딩)
//   - server/storage/wal/decoder.go: decoder (CRC 검증, 프레임 크기 디코딩)
//   - server/storage/wal/walpb/record.go: Record protobuf 정의
//
// 이 PoC는 WAL의 핵심 원리(레코드 기록, CRC 검증, 세그먼트 관리, 크래시 복구)를 재현한다.
package main

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ============================================================
// 레코드 타입 상수
// etcd 소스: wal.go → const MetadataType, EntryType, StateType, CrcType, SnapshotType
// ============================================================

const (
	RecordTypeEntry    int64 = 1 // Raft 엔트리 (etcd: EntryType)
	RecordTypeState    int64 = 2 // Raft HardState (etcd: StateType)
	RecordTypeSnapshot int64 = 3 // 스냅샷 참조 (etcd: SnapshotType)
	RecordTypeCRC      int64 = 4 // CRC 체크포인트 (etcd: CrcType)
)

func recordTypeName(t int64) string {
	switch t {
	case RecordTypeEntry:
		return "Entry"
	case RecordTypeState:
		return "State"
	case RecordTypeSnapshot:
		return "Snapshot"
	case RecordTypeCRC:
		return "CRC"
	default:
		return "Unknown"
	}
}

// crcTable: etcd와 동일하게 Castagnoli CRC32 테이블 사용
// etcd 소스: crcTable = crc32.MakeTable(crc32.Castagnoli)
var crcTable = crc32.MakeTable(crc32.Castagnoli)

// ============================================================
// Record: WAL 레코드 구조
// etcd 소스: walpb/record.proto → message Record { int64 type; uint32 crc; bytes data; }
// ============================================================

type Record struct {
	Type int64  // 레코드 타입
	CRC  uint32 // 데이터의 CRC32 체크섬
	Data []byte // 직렬화된 데이터
}

// ============================================================
// 레코드 직렬화/역직렬화
// etcd 실제: protobuf Marshal/Unmarshal 사용
// 이 PoC: 간단한 바이너리 인코딩 사용
//
// 프레임 형식 (etcd encoder.go 기반):
//   [8바이트: 프레임크기(하위56비트=데이터크기, 상위8비트=패딩정보)]
//   [N바이트: 레코드 데이터]
//   [P바이트: 8바이트 정렬 패딩]
// ============================================================

func marshalRecord(rec *Record) []byte {
	// 레이아웃: type(8) + crc(4) + dataLen(4) + data(N)
	buf := make([]byte, 8+4+4+len(rec.Data))
	binary.LittleEndian.PutUint64(buf[0:8], uint64(rec.Type))
	binary.LittleEndian.PutUint32(buf[8:12], rec.CRC)
	binary.LittleEndian.PutUint32(buf[12:16], uint32(len(rec.Data)))
	copy(buf[16:], rec.Data)
	return buf
}

func unmarshalRecord(data []byte) (*Record, error) {
	if len(data) < 16 {
		return nil, fmt.Errorf("레코드 데이터가 너무 짧음: %d바이트", len(data))
	}
	recType := int64(binary.LittleEndian.Uint64(data[0:8]))
	crc := binary.LittleEndian.Uint32(data[8:12])
	dataLen := binary.LittleEndian.Uint32(data[12:16])
	if len(data) < int(16+dataLen) {
		return nil, fmt.Errorf("데이터 길이 불일치: 예상 %d, 실제 %d", 16+dataLen, len(data))
	}
	recData := make([]byte, dataLen)
	copy(recData, data[16:16+dataLen])
	return &Record{Type: recType, CRC: crc, Data: recData}, nil
}

// ============================================================
// 프레임 인코딩: etcd encoder.go의 encodeFrameSize + write 기반
// 8바이트 정렬을 위한 패딩 처리
//
// etcd 소스:
//   func encodeFrameSize(dataBytes int) (lenField uint64, padBytes int)
//     lenField = uint64(dataBytes)
//     padBytes = (8 - (dataBytes % 8)) % 8
//     if padBytes != 0 { lenField |= uint64(0x80|padBytes) << 56 }
// ============================================================

func encodeFrameSize(dataBytes int) (lenField uint64, padBytes int) {
	lenField = uint64(dataBytes)
	padBytes = (8 - (dataBytes % 8)) % 8
	if padBytes != 0 {
		lenField |= uint64(0x80|padBytes) << 56
	}
	return lenField, padBytes
}

func decodeFrameSize(lenField uint64) (recBytes int, padBytes int) {
	// 하위 56비트가 레코드 크기
	recBytes = int(lenField & ^(uint64(0xff) << 56))
	// MSB가 설정되어 있으면 패딩 있음
	if lenField>>63 == 1 {
		padBytes = int((lenField >> 56) & 0x7)
	}
	return recBytes, padBytes
}

// writeFrame: 프레임 헤더(8바이트) + 데이터 + 패딩을 파일에 기록
func writeFrame(w io.Writer, data []byte) error {
	lenField, padBytes := encodeFrameSize(len(data))

	// 프레임 크기 기록 (8바이트, little-endian)
	sizeBuf := make([]byte, 8)
	binary.LittleEndian.PutUint64(sizeBuf, lenField)
	if _, err := w.Write(sizeBuf); err != nil {
		return err
	}

	// 데이터 + 패딩 기록
	if _, err := w.Write(data); err != nil {
		return err
	}
	if padBytes > 0 {
		pad := make([]byte, padBytes)
		if _, err := w.Write(pad); err != nil {
			return err
		}
	}
	return nil
}

// readFrame: 프레임 하나를 읽어 데이터 반환
func readFrame(r io.Reader) ([]byte, error) {
	// 프레임 크기 읽기
	sizeBuf := make([]byte, 8)
	if _, err := io.ReadFull(r, sizeBuf); err != nil {
		return nil, err
	}
	lenField := binary.LittleEndian.Uint64(sizeBuf)

	// 0이면 preallocated 공간의 끝 (etcd: hit end of preallocated space)
	if lenField == 0 {
		return nil, io.EOF
	}

	recBytes, padBytes := decodeFrameSize(lenField)
	data := make([]byte, recBytes+padBytes)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, err
	}

	return data[:recBytes], nil
}

// ============================================================
// WAL: Write-Ahead Log 구현
// etcd 소스: wal.go → type WAL struct
//
// 핵심 필드:
//   - dir: WAL 파일 디렉토리
//   - encoder: 레코드 인코더 (CRC 체인 유지)
//   - locks: 세그먼트 파일 목록
//   - enti: 마지막 저장된 엔트리 인덱스
// ============================================================

type WAL struct {
	dir            string   // WAL 디렉토리
	segmentFiles   []string // 세그먼트 파일 목록
	currentFile    *os.File // 현재 기록 중인 파일
	currentSize    int64    // 현재 파일 크기
	maxSegmentSize int64    // 세그먼트 최대 크기 (etcd 기본: 64MB)
	crc            uint32   // 연쇄 CRC 값 (이전 레코드의 CRC가 다음 레코드 CRC 계산에 사용)
	recordCount    int      // 전체 레코드 수
}

// SegmentSizeBytes: etcd 기본값은 64MB, 데모에서는 작은 값 사용
// etcd 소스: var SegmentSizeBytes int64 = 64 * 1000 * 1000
const DefaultSegmentSize = 512 // 데모용 작은 크기

// Create: 새 WAL을 생성한다.
// etcd 소스: func Create(lg *zap.Logger, dirpath string, metadata []byte) (*WAL, error)
//
// 실제 etcd의 생성 흐름:
// 1. 임시 디렉토리 생성 (.tmp)
// 2. 첫 번째 세그먼트 파일 생성 (0000000000000000-0000000000000000.wal)
// 3. 메타데이터 레코드 기록
// 4. 임시 → 최종 디렉토리로 atomic rename
func Create(dir string, maxSegSize int64) (*WAL, error) {
	if err := os.MkdirAll(dir, 0750); err != nil {
		return nil, fmt.Errorf("WAL 디렉토리 생성 실패: %w", err)
	}

	w := &WAL{
		dir:            dir,
		maxSegmentSize: maxSegSize,
	}

	// 첫 번째 세그먼트 파일 생성
	if err := w.createNewSegment(); err != nil {
		return nil, err
	}

	return w, nil
}

// createNewSegment: 새 세그먼트 파일을 생성한다.
// etcd 소스: wal.go → cut() 메서드 (세그먼트 전환)
// 파일명 형식: seq-index.wal (etcd: walName 함수)
func (w *WAL) createNewSegment() error {
	if w.currentFile != nil {
		// CRC 체크포인트 레코드를 기록 (세그먼트 간 CRC 연쇄)
		// etcd: 새 세그먼트 시작 시 이전 CRC를 CrcType 레코드로 기록
		w.currentFile.Sync()
		w.currentFile.Close()
	}

	segNum := len(w.segmentFiles)
	fileName := fmt.Sprintf("%08d.wal", segNum)
	filePath := filepath.Join(w.dir, fileName)

	f, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("세그먼트 파일 생성 실패: %w", err)
	}

	w.currentFile = f
	w.currentSize = 0
	w.segmentFiles = append(w.segmentFiles, filePath)

	// 새 세그먼트 시작 시 CRC 체크포인트 기록
	// etcd: encoder.go에서 prevCrc를 CrcType 레코드로 기록
	if segNum > 0 {
		crcRec := &Record{
			Type: RecordTypeCRC,
			Data: make([]byte, 4),
		}
		binary.LittleEndian.PutUint32(crcRec.Data, w.crc)
		crcRec.CRC = w.crc
		data := marshalRecord(crcRec)
		if err := writeFrame(f, data); err != nil {
			return err
		}
		w.currentSize += int64(8 + len(data))
	}

	return nil
}

// Append: 레코드를 WAL에 추가한다.
// etcd 소스: wal.go → Save() 메서드
//
// 핵심 흐름:
// 1. 데이터의 CRC32 계산 (이전 CRC와 연쇄)
// 2. 프레임 형식으로 파일에 기록
// 3. 세그먼트 크기 초과 시 새 세그먼트 생성
func (w *WAL) Append(rec *Record) error {
	// CRC 계산: etcd encoder.go → e.crc.Write(rec.Data)
	// 연쇄 CRC: 이전 CRC 값을 시드로 사용하여 데이터 무결성 체인 형성
	h := crc32.New(crcTable)
	// 이전 CRC를 시드에 포함 (연쇄)
	seedBuf := make([]byte, 4)
	binary.LittleEndian.PutUint32(seedBuf, w.crc)
	h.Write(seedBuf)
	h.Write(rec.Data)
	rec.CRC = h.Sum32()
	w.crc = rec.CRC

	data := marshalRecord(rec)
	if err := writeFrame(w.currentFile, data); err != nil {
		return err
	}

	w.currentSize += int64(8 + len(data) + ((8 - len(data)%8) % 8))
	w.recordCount++

	// 세그먼트 크기 초과 시 새 파일 생성
	// etcd: wal.go → cut() 호출 조건
	if w.currentSize >= w.maxSegmentSize {
		if err := w.createNewSegment(); err != nil {
			return err
		}
	}

	return nil
}

// Sync: 현재 파일을 디스크에 강제 동기화한다.
// etcd 소스: wal.go → sync() → f.Sync() (fsync 시스템콜)
// 실제 etcd는 warnSyncDuration(1초) 초과 시 경고 로그 출력
func (w *WAL) Sync() error {
	if w.currentFile != nil {
		return w.currentFile.Sync()
	}
	return nil
}

// Close: WAL을 닫는다.
func (w *WAL) Close() error {
	if w.currentFile != nil {
		w.currentFile.Sync()
		return w.currentFile.Close()
	}
	return nil
}

// ============================================================
// ReadAll: WAL의 모든 레코드를 읽고 CRC를 검증한다.
// etcd 소스: wal.go → ReadAll() 메서드
//
// 실제 흐름:
// 1. 모든 세그먼트 파일을 순서대로 읽기
// 2. 각 레코드의 CRC 검증 (연쇄 CRC)
// 3. 손상된 레코드 발견 시 에러 반환
// ============================================================

type ReadResult struct {
	Records      []*Record
	TotalRecords int
	Segments     int
	CRCErrors    int
}

func ReadAll(dir string) (*ReadResult, error) {
	result := &ReadResult{}

	// 세그먼트 파일 목록 수집 (정렬)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("WAL 디렉토리 읽기 실패: %w", err)
	}

	var walFiles []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".wal") {
			walFiles = append(walFiles, filepath.Join(dir, entry.Name()))
		}
	}
	sort.Strings(walFiles)
	result.Segments = len(walFiles)

	var runningCRC uint32

	// 각 세그먼트 파일 순서대로 읽기
	// etcd: decoder.go → Decode() 메서드
	for _, walFile := range walFiles {
		f, err := os.Open(walFile)
		if err != nil {
			return nil, fmt.Errorf("세그먼트 파일 열기 실패: %w", err)
		}

		for {
			data, err := readFrame(f)
			if err == io.EOF {
				break
			}
			if err != nil {
				f.Close()
				return nil, fmt.Errorf("프레임 읽기 실패: %w", err)
			}

			rec, err := unmarshalRecord(data)
			if err != nil {
				f.Close()
				return nil, fmt.Errorf("레코드 역직렬화 실패: %w", err)
			}

			// CRC 타입은 새 세그먼트의 시작점 CRC 복원용
			// etcd: decoder.go → CrcType 레코드 처리
			if rec.Type == RecordTypeCRC {
				prevCRC := binary.LittleEndian.Uint32(rec.Data)
				runningCRC = prevCRC
				continue
			}

			// CRC 검증: 연쇄 CRC 재계산
			// etcd: decoder.go → d.crc.Write(rec.Data) + rec.Validate(d.crc.Sum32())
			h := crc32.New(crcTable)
			seedBuf := make([]byte, 4)
			binary.LittleEndian.PutUint32(seedBuf, runningCRC)
			h.Write(seedBuf)
			h.Write(rec.Data)
			expectedCRC := h.Sum32()

			if rec.CRC != expectedCRC {
				result.CRCErrors++
				fmt.Printf("  [CRC 오류] 레코드 타입=%s, 예상CRC=%08x, 실제CRC=%08x\n",
					recordTypeName(rec.Type), expectedCRC, rec.CRC)
			}
			runningCRC = rec.CRC

			result.Records = append(result.Records, rec)
			result.TotalRecords++
		}
		f.Close()
	}

	return result, nil
}

// ============================================================
// 메인: WAL의 핵심 동작을 시연
// ============================================================

func main() {
	fmt.Println("=== etcd Write-Ahead Log (WAL) 시뮬레이션 ===")
	fmt.Println()

	// 임시 디렉토리 생성
	tmpDir, err := os.MkdirTemp("", "etcd-wal-poc-*")
	if err != nil {
		fmt.Printf("임시 디렉토리 생성 실패: %v\n", err)
		return
	}
	defer os.RemoveAll(tmpDir)

	// ----------------------------------------
	// 시나리오 1: WAL 기록
	// ----------------------------------------
	fmt.Println("--- 시나리오 1: WAL 레코드 기록 ---")
	wal, err := Create(tmpDir, DefaultSegmentSize)
	if err != nil {
		fmt.Printf("WAL 생성 실패: %v\n", err)
		return
	}

	// Raft 엔트리 기록 (실제 etcd에서는 raftpb.Entry를 직렬화)
	entries := []string{
		"PUT key1=value1",
		"PUT key2=value2",
		"PUT key3=value3",
		"DELETE key1",
		"PUT key2=value2_updated",
		"PUT key4=value4",
		"PUT key5=value5",
		"PUT key6=value6",
	}

	for i, entry := range entries {
		rec := &Record{
			Type: RecordTypeEntry,
			Data: []byte(entry),
		}
		if err := wal.Append(rec); err != nil {
			fmt.Printf("레코드 기록 실패: %v\n", err)
			return
		}
		fmt.Printf("  기록: [Entry #%d] %s (CRC: %08x)\n", i+1, entry, rec.CRC)
	}

	// HardState 기록
	stateRec := &Record{
		Type: RecordTypeState,
		Data: []byte("Term:5 Vote:1 Commit:8"),
	}
	wal.Append(stateRec)
	fmt.Printf("  기록: [State] %s (CRC: %08x)\n", string(stateRec.Data), stateRec.CRC)

	// fsync
	wal.Sync()
	wal.Close()

	fmt.Printf("\n세그먼트 파일 수: %d\n", len(wal.segmentFiles))
	for _, f := range wal.segmentFiles {
		info, _ := os.Stat(f)
		fmt.Printf("  %s: %d 바이트\n", filepath.Base(f), info.Size())
	}
	fmt.Println()

	// ----------------------------------------
	// 시나리오 2: WAL 재생 (프로세스 재시작 시뮬레이션)
	// ----------------------------------------
	fmt.Println("--- 시나리오 2: WAL 재생 (크래시 복구) ---")
	fmt.Println("프로세스 재시작 시뮬레이션: WAL에서 모든 레코드 읽기")
	fmt.Println()

	result, err := ReadAll(tmpDir)
	if err != nil {
		fmt.Printf("WAL 읽기 실패: %v\n", err)
		return
	}

	fmt.Printf("읽은 세그먼트: %d개\n", result.Segments)
	fmt.Printf("읽은 레코드: %d개\n", result.TotalRecords)
	fmt.Printf("CRC 오류: %d개\n", result.CRCErrors)
	fmt.Println()

	// 복구된 상태 재구성
	fmt.Println("복구된 레코드:")
	state := make(map[string]string) // 복구된 KV 상태
	for _, rec := range result.Records {
		switch rec.Type {
		case RecordTypeEntry:
			data := string(rec.Data)
			fmt.Printf("  [Entry] %s\n", data)
			// KV 상태 재구성
			if strings.HasPrefix(data, "PUT ") {
				parts := strings.SplitN(data[4:], "=", 2)
				if len(parts) == 2 {
					state[parts[0]] = parts[1]
				}
			} else if strings.HasPrefix(data, "DELETE ") {
				delete(state, data[7:])
			}
		case RecordTypeState:
			fmt.Printf("  [State] %s\n", string(rec.Data))
		}
	}
	fmt.Println()

	fmt.Println("복구된 KV 상태:")
	keys := make([]string, 0, len(state))
	for k := range state {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %s = %s\n", k, state[k])
	}
	fmt.Println()

	// ----------------------------------------
	// 시나리오 3: CRC 손상 감지
	// ----------------------------------------
	fmt.Println("--- 시나리오 3: CRC 손상 감지 ---")
	corruptDir, _ := os.MkdirTemp("", "etcd-wal-corrupt-*")
	defer os.RemoveAll(corruptDir)

	wal2, _ := Create(corruptDir, 4096)
	wal2.Append(&Record{Type: RecordTypeEntry, Data: []byte("PUT good=data")})
	wal2.Append(&Record{Type: RecordTypeEntry, Data: []byte("PUT will=be_corrupted")})
	wal2.Append(&Record{Type: RecordTypeEntry, Data: []byte("PUT after=corruption")})
	wal2.Close()

	// 파일 직접 손상시키기 (중간 레코드의 데이터 변조)
	corruptFile := filepath.Join(corruptDir, "00000000.wal")
	fileData, _ := os.ReadFile(corruptFile)
	// 두 번째 레코드 영역의 데이터 바이트를 변조
	if len(fileData) > 100 {
		fileData[90] ^= 0xFF // 비트 뒤집기
		os.WriteFile(corruptFile, fileData, 0644)
	}

	fmt.Println("손상된 WAL 읽기 시도:")
	result2, err := ReadAll(corruptDir)
	if err != nil {
		fmt.Printf("  읽기 에러: %v\n", err)
	} else {
		fmt.Printf("  읽은 레코드: %d개\n", result2.TotalRecords)
		fmt.Printf("  CRC 오류 감지: %d개\n", result2.CRCErrors)
		if result2.CRCErrors > 0 {
			fmt.Println("  → CRC 체크섬 불일치로 데이터 손상 감지!")
		}
	}
	fmt.Println()

	// ----------------------------------------
	// 시나리오 4: 세그먼트 파일 관리
	// ----------------------------------------
	fmt.Println("--- 시나리오 4: 세그먼트 파일 관리 ---")
	segDir, _ := os.MkdirTemp("", "etcd-wal-segment-*")
	defer os.RemoveAll(segDir)

	// 작은 세그먼트 크기로 여러 파일 생성
	smallSegSize := int64(128)
	fmt.Printf("세그먼트 최대 크기: %d 바이트\n", smallSegSize)

	wal3, _ := Create(segDir, smallSegSize)
	for i := 0; i < 15; i++ {
		data := fmt.Sprintf("entry-%03d: %s", i, strings.Repeat("x", 20))
		wal3.Append(&Record{Type: RecordTypeEntry, Data: []byte(data)})
	}
	wal3.Close()

	fmt.Printf("생성된 세그먼트 파일: %d개\n", len(wal3.segmentFiles))
	for _, f := range wal3.segmentFiles {
		info, _ := os.Stat(f)
		fmt.Printf("  %s: %d 바이트\n", filepath.Base(f), info.Size())
	}
	fmt.Println()

	// 세그먼트 분할 후 전체 재생
	result3, _ := ReadAll(segDir)
	fmt.Printf("전체 재생: %d개 세그먼트에서 %d개 레코드 복구\n",
		result3.Segments, result3.TotalRecords)
	fmt.Printf("CRC 오류: %d개\n", result3.CRCErrors)

	fmt.Println()
	fmt.Println("=== WAL 핵심 원리 ===")
	fmt.Println("1. Write-Ahead: 상태 변경 전에 먼저 로그에 기록 → 크래시 시 복구 보장")
	fmt.Println("2. CRC32 연쇄: 각 레코드의 CRC가 다음 레코드에 전파 → 누락/손상 감지")
	fmt.Println("3. 8바이트 정렬: 프레임 크기를 8바이트 정렬 → torn write 감지 용이")
	fmt.Println("4. 세그먼트 관리: 일정 크기 초과 시 새 파일 생성 → 스냅샷 이전 파일 삭제 가능")
	fmt.Println("5. fsync: 레코드 기록 후 명시적 디스크 동기화 → 내구성 보장")
}
