package main

import (
	"archive/tar"
	"bytes"
	"compress/flate"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// =============================================================================
// PoC-15: VM 아카이브(내보내기/가져오기) 시뮬레이션
// =============================================================================
// Tart의 VMDirectory+Archive.swift를 Go로 재현한다.
// 핵심 개념:
//   - 3단계 파이프라인: 파일시스템 → 인코딩 → 압축 → 파일
//   - LZFSE 압축 (Apple 권장) → Go에서는 flate로 시뮬레이션
//   - AppleArchive 형식 → Go에서는 tar 형식으로 시뮬레이션
//   - 메타데이터 보존: TYP,PAT,LNK,DEV,DAT,UID,GID,MOD,FLG,MTM,BTM,CTM
//   - 무결성 검증: SHA-256 체크섬
//
// 실제 소스: Sources/tart/VMDirectory+Archive.swift
// =============================================================================

// ---------------------------------------------------------------------------
// 1. VMBundle — VM 번들 파일 구조
// ---------------------------------------------------------------------------

// VMBundle은 VM 번들 디렉토리를 나타낸다.
// VMDirectory.swift에서 정의된 파일 구조:
//   config.json, disk.img, nvram.bin, (optional) state.vzvmsave, manifest.json
type VMBundle struct {
	BasePath string
	Files    []BundleFile
}

// BundleFile은 번들 내 개별 파일을 나타낸다.
type BundleFile struct {
	Name     string    // 파일 이름 (상대 경로)
	Data     []byte    // 파일 내용
	Mode     os.FileMode
	ModTime  time.Time
}

// NewVMBundle은 새 VM 번들을 생성한다.
func NewVMBundle(basePath string) *VMBundle {
	return &VMBundle{
		BasePath: basePath,
		Files:    make([]BundleFile, 0),
	}
}

// AddFile은 번들에 파일을 추가한다.
func (vb *VMBundle) AddFile(name string, data []byte) {
	vb.Files = append(vb.Files, BundleFile{
		Name:    name,
		Data:    data,
		Mode:    0644,
		ModTime: time.Now(),
	})
}

// TotalSize는 번들의 전체 크기를 반환한다.
func (vb *VMBundle) TotalSize() int {
	total := 0
	for _, f := range vb.Files {
		total += len(f.Data)
	}
	return total
}

// Checksum은 번들 전체의 SHA-256 체크섬을 계산한다.
func (vb *VMBundle) Checksum() string {
	h := sha256.New()
	for _, f := range vb.Files {
		h.Write([]byte(f.Name))
		h.Write(f.Data)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ---------------------------------------------------------------------------
// 2. 내보내기 — VMDirectory.exportToArchive() 시뮬레이션
// ---------------------------------------------------------------------------

// ExportToArchive는 VM 번들을 압축 아카이브로 내보낸다.
// VMDirectory+Archive.swift:
//   func exportToArchive(path: String) throws {
//     // 1. 파일 스트림 생성
//     let fileStream = ArchiveByteStream.fileStream(path:, mode: .writeOnly, ...)
//     // 2. LZFSE 압축 스트림
//     let compressionStream = ArchiveByteStream.compressionStream(using: .lzfse, writingTo: fileStream)
//     // 3. 인코딩 스트림
//     let encodeStream = ArchiveStream.encodeStream(writingTo: compressionStream)
//     // 4. 메타데이터 키셋 정의
//     let keySet = ArchiveHeader.FieldKeySet("TYP,PAT,LNK,DEV,DAT,UID,GID,MOD,FLG,MTM,BTM,CTM")
//     // 5. 디렉토리 내용 쓰기
//     try encodeStream.writeDirectoryContents(archiveFrom: FilePath(baseURL.path), keySet: keySet)
//   }
func ExportToArchive(bundle *VMBundle, archivePath string) error {
	fmt.Printf("  [내보내기] 시작: %s -> %s\n", bundle.BasePath, archivePath)

	// 1단계: 출력 파일 생성 (ArchiveByteStream.fileStream)
	outFile, err := os.Create(archivePath)
	if err != nil {
		return fmt.Errorf("파일 생성 실패: %w", err)
	}
	defer outFile.Close()

	// 2단계: 압축 스트림 (LZFSE -> flate로 시뮬레이션)
	// ArchiveByteStream.compressionStream(using: .lzfse, writingTo: fileStream)
	compressor, err := flate.NewWriter(outFile, flate.BestCompression)
	if err != nil {
		return fmt.Errorf("압축기 생성 실패: %w", err)
	}
	defer compressor.Close()

	// 3단계: tar 인코딩 (ArchiveStream.encodeStream)
	tw := tar.NewWriter(compressor)
	defer tw.Close()

	// 4단계: 메타데이터 키셋 — Apple Archive 필드에 대응
	// TYP=타입, PAT=경로, DAT=데이터, UID/GID=소유자, MOD=권한, MTM=수정시간
	fmt.Println("  메타데이터 키셋: TYP,PAT,LNK,DEV,DAT,UID,GID,MOD,FLG,MTM,BTM,CTM")

	// 5단계: 각 파일을 아카이브에 쓰기
	// encodeStream.writeDirectoryContents(archiveFrom:, keySet:)
	for _, f := range bundle.Files {
		header := &tar.Header{
			Name:    f.Name,
			Size:    int64(len(f.Data)),
			Mode:    int64(f.Mode),
			ModTime: f.ModTime,
			Uid:     501,
			Gid:     20,
		}

		if err := tw.WriteHeader(header); err != nil {
			return fmt.Errorf("헤더 쓰기 실패 (%s): %w", f.Name, err)
		}

		if _, err := tw.Write(f.Data); err != nil {
			return fmt.Errorf("데이터 쓰기 실패 (%s): %w", f.Name, err)
		}

		fmt.Printf("    [추가] %s (%d bytes, mode=%o)\n", f.Name, len(f.Data), f.Mode)
	}

	fmt.Println("  [내보내기] 완료")
	return nil
}

// ---------------------------------------------------------------------------
// 3. 가져오기 — VMDirectory.importFromArchive() 시뮬레이션
// ---------------------------------------------------------------------------

// ImportFromArchive는 압축 아카이브에서 VM 번들을 복원한다.
// VMDirectory+Archive.swift:
//   func importFromArchive(path: String) throws {
//     let fileStream = ArchiveByteStream.fileStream(path:, mode: .readOnly, ...)
//     let decompressionStream = ArchiveByteStream.decompressionStream(readingFrom: fileStream)
//     let decodeStream = ArchiveStream.decodeStream(readingFrom: decompressionStream)
//     let extractStream = ArchiveStream.extractStream(extractingTo: FilePath(baseURL.path),
//                                                      flags: [.ignoreOperationNotPermitted])
//     _ = try ArchiveStream.process(readingFrom: decodeStream, writingTo: extractStream)
//   }
func ImportFromArchive(archivePath string) (*VMBundle, error) {
	fmt.Printf("  [가져오기] 시작: %s\n", archivePath)

	// 1단계: 파일 읽기 (ArchiveByteStream.fileStream, readOnly)
	inFile, err := os.Open(archivePath)
	if err != nil {
		return nil, fmt.Errorf("파일 열기 실패: %w", err)
	}
	defer inFile.Close()

	// 2단계: 압축 해제 (ArchiveByteStream.decompressionStream)
	decompressor := flate.NewReader(inFile)
	defer decompressor.Close()

	// 3단계: tar 디코딩 (ArchiveStream.decodeStream)
	tr := tar.NewReader(decompressor)

	// 4단계: 파일 추출 (ArchiveStream.extractStream + process)
	bundle := NewVMBundle("restored")

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("아카이브 읽기 실패: %w", err)
		}

		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("파일 데이터 읽기 실패 (%s): %w", header.Name, err)
		}

		bundle.Files = append(bundle.Files, BundleFile{
			Name:    header.Name,
			Data:    data,
			Mode:    os.FileMode(header.Mode),
			ModTime: header.ModTime,
		})

		fmt.Printf("    [추출] %s (%d bytes)\n", header.Name, len(data))
	}

	fmt.Println("  [가져오기] 완료")
	return bundle, nil
}

// ---------------------------------------------------------------------------
// 4. 무결성 검증
// ---------------------------------------------------------------------------

// VerifyIntegrity는 원본과 복원된 번들의 무결성을 검증한다.
func VerifyIntegrity(original, restored *VMBundle) bool {
	if len(original.Files) != len(restored.Files) {
		fmt.Printf("  [무결성] 파일 수 불일치: 원본=%d, 복원=%d\n",
			len(original.Files), len(restored.Files))
		return false
	}

	allMatch := true
	for i, origFile := range original.Files {
		restFile := restored.Files[i]

		nameMatch := origFile.Name == restFile.Name
		dataMatch := bytes.Equal(origFile.Data, restFile.Data)

		status := "일치"
		if !nameMatch || !dataMatch {
			status = "불일치"
			allMatch = false
		}

		origHash := sha256.Sum256(origFile.Data)
		restHash := sha256.Sum256(restFile.Data)

		fmt.Printf("    %s: %s (SHA-256: %s...)\n",
			origFile.Name, status, hex.EncodeToString(origHash[:8]))
		if !dataMatch {
			fmt.Printf("      원본: %s...\n", hex.EncodeToString(origHash[:8]))
			fmt.Printf("      복원: %s...\n", hex.EncodeToString(restHash[:8]))
		}
	}

	return allMatch
}

// ---------------------------------------------------------------------------
// 5. 출력 헬퍼
// ---------------------------------------------------------------------------

func printSeparator(title string) {
	fmt.Printf("\n--- %s ---\n", title)
}

func formatBytes(b int) string {
	if b >= 1024*1024 {
		return fmt.Sprintf("%.1f MB", float64(b)/1024/1024)
	}
	if b >= 1024 {
		return fmt.Sprintf("%.1f KB", float64(b)/1024)
	}
	return fmt.Sprintf("%d B", b)
}

// ---------------------------------------------------------------------------
// 6. 메인 함수
// ---------------------------------------------------------------------------

func main() {
	fmt.Println("=== PoC-15: VM 아카이브(내보내기/가져오기) 시뮬레이션 ===")
	fmt.Println("  소스: VMDirectory+Archive.swift")
	fmt.Println()

	// 임시 디렉토리 생성
	tmpDir, err := os.MkdirTemp("", "tart-poc-15-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "임시 디렉토리 생성 실패: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmpDir)

	// =========================================================================
	// 데모 1: VM 번들 생성
	// =========================================================================
	printSeparator("데모 1: VM 번들 생성")

	bundle := NewVMBundle("macos-sonoma")

	// config.json
	configData := []byte(`{
  "version": 1,
  "os": "darwin",
  "arch": "arm64",
  "cpuCount": 4,
  "memorySize": 8589934592,
  "macAddress": "7a:65:e4:3f:b2:01",
  "display": {"width": 1920, "height": 1200}
}`)
	bundle.AddFile("config.json", configData)

	// nvram.bin (2KB 시뮬레이션)
	nvramData := make([]byte, 2048)
	for i := range nvramData {
		nvramData[i] = byte(i % 256)
	}
	bundle.AddFile("nvram.bin", nvramData)

	// disk.img (128KB 시뮬레이션 — 실제로는 수십 GB)
	diskData := make([]byte, 128*1024)
	for i := range diskData {
		diskData[i] = byte(i % 256)
	}
	bundle.AddFile("disk.img", diskData)

	fmt.Printf("  VM 번들: %s\n", bundle.BasePath)
	for _, f := range bundle.Files {
		fmt.Printf("    %s: %s\n", f.Name, formatBytes(len(f.Data)))
	}
	fmt.Printf("  전체 크기: %s\n", formatBytes(bundle.TotalSize()))
	fmt.Printf("  체크섬: %s\n", bundle.Checksum())

	// =========================================================================
	// 데모 2: 내보내기 (Export)
	// =========================================================================
	printSeparator("데모 2: 내보내기 (exportToArchive)")

	archivePath := filepath.Join(tmpDir, "macos-sonoma.aar")

	fmt.Println("  VMDirectory+Archive.swift 파이프라인:")
	fmt.Println("    파일시스템 -> encodeStream -> compressionStream(LZFSE) -> fileStream")
	fmt.Println("    (Go에서는 tar -> flate로 시뮬레이션)")
	fmt.Println()

	startExport := time.Now()
	if err := ExportToArchive(bundle, archivePath); err != nil {
		fmt.Fprintf(os.Stderr, "내보내기 실패: %v\n", err)
		os.Exit(1)
	}
	exportElapsed := time.Since(startExport)

	// 아카이브 크기 확인
	archiveInfo, _ := os.Stat(archivePath)
	originalSize := bundle.TotalSize()
	archiveSize := int(archiveInfo.Size())
	compressionRatio := float64(archiveSize) / float64(originalSize) * 100

	fmt.Printf("\n  원본 크기: %s\n", formatBytes(originalSize))
	fmt.Printf("  아카이브 크기: %s (%.1f%%)\n", formatBytes(archiveSize), compressionRatio)
	fmt.Printf("  압축률: %.1f%%\n", 100-compressionRatio)
	fmt.Printf("  소요 시간: %v\n", exportElapsed)

	// =========================================================================
	// 데모 3: 가져오기 (Import)
	// =========================================================================
	printSeparator("데모 3: 가져오기 (importFromArchive)")

	fmt.Println("  VMDirectory+Archive.swift 파이프라인:")
	fmt.Println("    fileStream -> decompressionStream -> decodeStream -> extractStream")
	fmt.Println()

	startImport := time.Now()
	restored, err := ImportFromArchive(archivePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "가져오기 실패: %v\n", err)
		os.Exit(1)
	}
	importElapsed := time.Since(startImport)

	fmt.Printf("\n  복원된 파일: %d개\n", len(restored.Files))
	fmt.Printf("  복원 크기: %s\n", formatBytes(restored.TotalSize()))
	fmt.Printf("  소요 시간: %v\n", importElapsed)

	// =========================================================================
	// 데모 4: 무결성 검증
	// =========================================================================
	printSeparator("데모 4: 무결성 검증")

	fmt.Printf("  원본 체크섬: %s\n", bundle.Checksum())
	fmt.Printf("  복원 체크섬: %s\n", restored.Checksum())
	fmt.Println()

	fmt.Println("  파일별 검증:")
	integrity := VerifyIntegrity(bundle, restored)
	fmt.Printf("\n  전체 무결성: %v\n", integrity)

	// =========================================================================
	// 데모 5: 손상 감지
	// =========================================================================
	printSeparator("데모 5: 손상 감지 시뮬레이션")

	// 아카이브 파일 손상 시뮬레이션
	corruptPath := filepath.Join(tmpDir, "corrupted.aar")
	corruptData, _ := os.ReadFile(archivePath)

	// 중간 바이트 변경
	if len(corruptData) > 100 {
		corruptData[50] ^= 0xFF
		corruptData[51] ^= 0xFF
	}
	os.WriteFile(corruptPath, corruptData, 0644)

	fmt.Println("  손상된 아카이브 가져오기 시도...")
	_, err = ImportFromArchive(corruptPath)
	if err != nil {
		fmt.Printf("  [감지] 손상 감지됨: %v\n", err)
	} else {
		fmt.Println("  [경고] 손상이 감지되지 않음 (부분 손상)")
	}

	// =========================================================================
	// 데모 6: Apple Archive 파이프라인 구조
	// =========================================================================
	printSeparator("데모 6: Apple Archive 파이프라인 구조")

	fmt.Println("  내보내기 (exportToArchive):")
	fmt.Println("  ┌───────────────────┐")
	fmt.Println("  │ VM Directory      │  config.json, disk.img, nvram.bin")
	fmt.Println("  └────────┬──────────┘")
	fmt.Println("           │ writeDirectoryContents(archiveFrom:, keySet:)")
	fmt.Println("  ┌────────▼──────────┐")
	fmt.Println("  │ EncodeStream      │  필드별 인코딩 (TYP,PAT,DAT,MOD,...)")
	fmt.Println("  └────────┬──────────┘")
	fmt.Println("           │")
	fmt.Println("  ┌────────▼──────────┐")
	fmt.Println("  │ CompressionStream │  LZFSE 압축 (Apple 권장 알고리즘)")
	fmt.Println("  └────────┬──────────┘")
	fmt.Println("           │")
	fmt.Println("  ┌────────▼──────────┐")
	fmt.Println("  │ FileStream        │  .aar 파일 출력")
	fmt.Println("  └───────────────────┘")
	fmt.Println()
	fmt.Println("  가져오기 (importFromArchive):")
	fmt.Println("  ┌───────────────────┐")
	fmt.Println("  │ FileStream        │  .aar 파일 입력")
	fmt.Println("  └────────┬──────────┘")
	fmt.Println("           │")
	fmt.Println("  ┌────────▼──────────┐")
	fmt.Println("  │ DecompressionStream│  LZFSE 해제")
	fmt.Println("  └────────┬──────────┘")
	fmt.Println("           │")
	fmt.Println("  ┌────────▼──────────┐")
	fmt.Println("  │ DecodeStream      │  필드별 디코딩")
	fmt.Println("  └────────┬──────────┘")
	fmt.Println("           │ ArchiveStream.process(readingFrom:, writingTo:)")
	fmt.Println("  ┌────────▼──────────┐")
	fmt.Println("  │ ExtractStream     │  flags: [.ignoreOperationNotPermitted]")
	fmt.Println("  └────────┬──────────┘")
	fmt.Println("           │")
	fmt.Println("  ┌────────▼──────────┐")
	fmt.Println("  │ VM Directory      │  복원된 config.json, disk.img, nvram.bin")
	fmt.Println("  └───────────────────┘")

	// =========================================================================
	// 데모 7: Apple Archive 메타데이터 필드
	// =========================================================================
	printSeparator("데모 7: Apple Archive 메타데이터 필드")

	fields := []struct {
		key  string
		desc string
	}{
		{"TYP", "파일 타입 (일반 파일, 디렉토리, 심볼릭 링크)"},
		{"PAT", "파일 경로 (상대 경로)"},
		{"LNK", "심볼릭 링크 대상"},
		{"DEV", "디바이스 번호"},
		{"DAT", "파일 데이터"},
		{"UID", "소유자 사용자 ID"},
		{"GID", "소유자 그룹 ID"},
		{"MOD", "파일 권한 모드 (0644 등)"},
		{"FLG", "파일 플래그"},
		{"MTM", "수정 시간 (Modification Time)"},
		{"BTM", "생성 시간 (Birth Time)"},
		{"CTM", "변경 시간 (Change Time)"},
	}

	fmt.Printf("  %-5s %s\n", "키", "설명")
	fmt.Printf("  %s\n", strings.Repeat("-", 55))
	for _, f := range fields {
		fmt.Printf("  %-5s %s\n", f.key, f.desc)
	}
	fmt.Println()
	fmt.Println("  Tart 코드:")
	fmt.Println("    let keySet = ArchiveHeader.FieldKeySet(\"TYP,PAT,LNK,DEV,DAT,UID,GID,MOD,FLG,MTM,BTM,CTM\")")

	// =========================================================================
	// 요약
	// =========================================================================
	printSeparator("아카이브 설계 요약")
	fmt.Println("  1. 스트리밍 파이프라인: 파일 -> 인코딩 -> 압축 -> 출력 (메모리 효율적)")
	fmt.Println("  2. LZFSE 압축: Apple 플랫폼 최적화 알고리즘, zlib보다 빠르고 압축률 유사")
	fmt.Println("  3. 메타데이터 보존: 12개 필드로 파일 속성 완전 보존")
	fmt.Println("  4. 에러 처리: 각 스트림 단계별 Errno 기반 에러 보고")
	fmt.Println("  5. ignoreOperationNotPermitted: 추출 시 권한 에러 무시 (사용자 환경 호환)")
	fmt.Println("  6. 활용: tart export/import 명령으로 VM 백업/복원/이동")
}
