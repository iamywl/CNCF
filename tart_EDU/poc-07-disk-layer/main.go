package main

import (
	"bytes"
	"compress/flate"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// =============================================================================
// LZ4 스타일 청크 분할 병렬 Push/Pull 시뮬레이션
//
// tart의 OCI/Layerizer/DiskV2.swift 구현을 Go로 재현한다.
//
// 핵심 개념:
//   - 디스크 파일 -> N바이트 청크 분할
//   - 각 청크를 압축 (tart: LZ4, 여기서는 compress/flate)
//   - SHA256 다이제스트 계산
//   - 병렬 Push: goroutine + semaphore(buffered channel)로 동시성 제한
//   - 병렬 Pull: goroutine + 오프셋 기반 디스크 쓰기
//   - 제로 스킵 최적화: 바이트가 모두 0인 청크는 쓰기 건너뜀 (희소 파일)
//
// tart 핵심 상수 (DiskV2.swift):
//   - bufferSizeBytes = 4 * 1024 * 1024 (4MB)
//   - layerLimitBytes = 512 * 1024 * 1024 (512MB)
//   - holeGranularityBytes = 4 * 1024 * 1024 (4MB) — 제로 스킵 단위
//
// 참조: tart/Sources/tart/OCI/Layerizer/DiskV2.swift
//       tart/Sources/tart/OCI/Layerizer/Disk.swift
//       tart/Sources/tart/OCI/Manifest.swift
//       tart/Sources/tart/OCI/Digest.swift
// =============================================================================

// --- 상수 (tart DiskV2.swift 참조) ---

const (
	// layerLimitBytes는 하나의 레이어에 들어가는 비압축 데이터 최대 크기이다.
	// tart: private static let layerLimitBytes = 512 * 1024 * 1024
	// 시뮬레이션에서는 작은 값 사용 (256 바이트)
	layerLimitBytes = 256

	// holeGranularityBytes는 제로 스킵의 최소 단위이다.
	// tart: private static let holeGranularityBytes = 4 * 1024 * 1024
	// 시뮬레이션에서는 작은 값 사용 (64 바이트)
	holeGranularityBytes = 64

	// maxConcurrency는 최대 동시 Push/Pull 수이다.
	maxConcurrency = 3
)

// --- 다이제스트 유틸리티 (tart Digest.swift 참조) ---

// computeDigest는 SHA256 다이제스트를 "sha256:..." 형식으로 반환한다.
func computeDigest(data []byte) string {
	h := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", h)
}

// --- 매니페스트 레이어 (tart Manifest.swift OCIManifestLayer 참조) ---

// DiskLayer는 하나의 디스크 레이어 메타데이터이다.
// tart의 OCIManifestLayer 구조체의 핵심 필드를 재현.
type DiskLayer struct {
	Index                    int    // 레이어 순서
	MediaType                string // tart: "application/vnd.cirruslabs.tart.disk.v2"
	Size                     int    // 압축된 크기
	Digest                   string // 압축된 데이터의 SHA256
	UncompressedSize         uint64 // 비압축 크기 (annotation)
	UncompressedContentDigest string // 비압축 데이터의 SHA256 (annotation)
}

// --- 인메모리 레지스트리 (blob 저장소) ---

// BlobStore는 Push된 blob을 저장하는 인메모리 저장소이다.
type BlobStore struct {
	mu    sync.RWMutex
	blobs map[string][]byte // digest -> data
}

func NewBlobStore() *BlobStore {
	return &BlobStore{blobs: make(map[string][]byte)}
}

func (bs *BlobStore) Push(data []byte, digest string) {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	bs.blobs[digest] = make([]byte, len(data))
	copy(bs.blobs[digest], data)
}

func (bs *BlobStore) Pull(digest string) ([]byte, bool) {
	bs.mu.RLock()
	defer bs.mu.RUnlock()
	data, ok := bs.blobs[digest]
	return data, ok
}

func (bs *BlobStore) Exists(digest string) bool {
	bs.mu.RLock()
	defer bs.mu.RUnlock()
	_, ok := bs.blobs[digest]
	return ok
}

// --- 압축/해제 유틸리티 (tart: LZ4, 여기서는 flate) ---

// compress는 데이터를 flate로 압축한다.
// tart: (data as NSData).compressed(using: .lz4)
func compress(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, flate.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// decompress는 flate 압축 데이터를 해제한다.
// tart: OutputFilter(.decompress, using: .lz4, ...)
func decompress(data []byte) ([]byte, error) {
	r := flate.NewReader(bytes.NewReader(data))
	defer r.Close()
	return io.ReadAll(r)
}

// --- 제로 스킵 유틸리티 (tart DiskV2.zeroSkippingWrite 참조) ---

// isZeroChunk는 데이터가 모두 0인지 빠르게 검사한다.
// tart: chunk == zeroChunk (static let zeroChunk = Data(count: holeGranularityBytes))
//
// tart 벤치마크 결과 (DiskV2.swift 주석):
//   Data(...) == zeroChunk:                 18.9초 (73% CPU)
//   Data(...).contains(where: {$0 != 0}):   10분 22초 (99% CPU)
// → 전체 비교(==)가 바이트별 비교보다 ~33배 빠름
func isZeroChunk(data []byte) bool {
	for _, b := range data {
		if b != 0 {
			return false
		}
	}
	return true
}

// zeroSkippingWrite는 제로 청크를 건너뛰며 디스크에 쓴다.
// tart의 DiskV2.zeroSkippingWrite 메서드를 재현:
//   - holeGranularityBytes 단위로 데이터를 분할
//   - 모든 바이트가 0인 청크는 쓰기 건너뜀 (truncate로 이미 0으로 초기화)
//   - 0이 아닌 청크만 해당 오프셋에 seek + write
//
// 반환값: 다음 쓰기 오프셋
func zeroSkippingWrite(file *os.File, offset uint64, data []byte) (uint64, int, int) {
	written := 0
	skipped := 0
	pos := 0

	for pos < len(data) {
		end := pos + holeGranularityBytes
		if end > len(data) {
			end = len(data)
		}
		chunk := data[pos:end]

		if isZeroChunk(chunk) {
			// 제로 청크: 쓰기 건너뜀 (tart: truncate(2)로 이미 0)
			skipped++
		} else {
			// 0이 아닌 청크: seek + write
			file.Seek(int64(offset), io.SeekStart)
			file.Write(chunk)
			written++
		}

		offset += uint64(len(chunk))
		pos = end
	}

	return offset, written, skipped
}

// --- DiskV2 Push (tart DiskV2.push 재현) ---

// pushDisk는 디스크 파일을 청크로 분할하여 병렬 Push한다.
// tart의 DiskV2.push(diskURL:registry:chunkSizeMb:concurrency:progress:) 재현:
//
// 알고리즘:
//   1) 디스크 파일을 layerLimitBytes 크기의 청크로 분할
//   2) 각 청크를 압축 (LZ4/flate)
//   3) SHA256 다이제스트 계산
//   4) 병렬 Push (goroutine + semaphore로 동시성 제한)
//   5) 이미 존재하는 blob은 Push 건너뜀 (blobExists)
//   6) 레이어 메타데이터를 인덱스 순서로 정렬하여 반환
func pushDisk(diskData []byte, store *BlobStore, concurrency int) []DiskLayer {
	var mu sync.Mutex
	var results []DiskLayer

	// 청크 분할 (tart: mappedDisk.chunks(ofCount: layerLimitBytes))
	var chunks [][]byte
	for i := 0; i < len(diskData); i += layerLimitBytes {
		end := i + layerLimitBytes
		if end > len(diskData) {
			end = len(diskData)
		}
		chunks = append(chunks, diskData[i:end])
	}

	fmt.Printf("  디스크 크기: %d 바이트 -> %d개 레이어 (각 최대 %d 바이트)\n",
		len(diskData), len(chunks), layerLimitBytes)

	// semaphore: buffered channel로 동시성 제한
	// tart: withThrowingTaskGroup + "if index >= concurrency { group.next() }"
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	startTime := time.Now()

	for idx, chunk := range chunks {
		wg.Add(1)
		sem <- struct{}{} // semaphore 획득

		go func(index int, data []byte) {
			defer wg.Done()
			defer func() { <-sem }() // semaphore 해제

			// 압축 (tart: (data as NSData).compressed(using: .lz4))
			compressed, err := compress(data)
			if err != nil {
				fmt.Printf("  [Push] 레이어 %d 압축 실패: %v\n", index, err)
				return
			}

			compressedDigest := computeDigest(compressed)
			uncompressedDigest := computeDigest(data)

			// 이미 존재하면 Push 건너뜀 (tart: registry.blobExists)
			if store.Exists(compressedDigest) {
				fmt.Printf("  [Push] 레이어 %d: 이미 존재 (%s...)\n", index, compressedDigest[:30])
			} else {
				store.Push(compressed, compressedDigest)
				ratio := float64(len(compressed)) / float64(len(data)) * 100
				fmt.Printf("  [Push] 레이어 %d: %d -> %d 바이트 (%.1f%%) %s...\n",
					index, len(data), len(compressed), ratio, compressedDigest[:30])
			}

			layer := DiskLayer{
				Index:                     index,
				MediaType:                 "application/vnd.cirruslabs.tart.disk.v2",
				Size:                      len(compressed),
				Digest:                    compressedDigest,
				UncompressedSize:          uint64(len(data)),
				UncompressedContentDigest: uncompressedDigest,
			}

			mu.Lock()
			results = append(results, layer)
			mu.Unlock()
		}(idx, chunk)
	}

	wg.Wait()

	elapsed := time.Since(startTime)
	fmt.Printf("  Push 완료: %d개 레이어, 소요시간: %v\n", len(results), elapsed)

	// 인덱스 순서로 정렬 (tart: pushedLayers.sorted { $0.index < $1.index })
	sorted := make([]DiskLayer, len(results))
	for _, r := range results {
		sorted[r.Index] = r
	}

	return sorted
}

// --- DiskV2 Pull (tart DiskV2.pull 재현) ---

// pullDisk는 레이어를 병렬로 Pull하고 디스크 파일에 기록한다.
// tart의 DiskV2.pull(registry:diskLayers:diskURL:concurrency:progress:...) 재현:
//
// 알고리즘:
//   1) 비압축 디스크 크기 계산 → truncate로 파일 크기 설정
//   2) 각 레이어를 병렬로 Pull (goroutine + semaphore)
//   3) 압축 해제
//   4) 오프셋 기반으로 디스크 파일에 쓰기 (zeroSkippingWrite)
//   5) 비압축 다이제스트 검증
func pullDisk(layers []DiskLayer, store *BlobStore, diskPath string, concurrency int) error {
	// 비압축 디스크 크기 계산
	var uncompressedDiskSize uint64
	for _, layer := range layers {
		uncompressedDiskSize += layer.UncompressedSize
	}
	fmt.Printf("  비압축 디스크 크기: %d 바이트\n", uncompressedDiskSize)

	// 디스크 파일 생성 및 truncate (tart: FileManager.createFile + disk.truncate)
	file, err := os.Create(diskPath)
	if err != nil {
		return fmt.Errorf("디스크 파일 생성 실패: %w", err)
	}
	file.Truncate(int64(uncompressedDiskSize))
	file.Close()

	// 병렬 Pull + 해제 + 쓰기
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var pullMu sync.Mutex
	totalWritten := 0
	totalSkipped := 0

	startTime := time.Now()

	var globalOffset uint64

	for _, layer := range layers {
		wg.Add(1)
		sem <- struct{}{}

		diskOffset := globalOffset
		globalOffset += layer.UncompressedSize

		go func(dl DiskLayer, offset uint64) {
			defer wg.Done()
			defer func() { <-sem }()

			// Pull (tart: registry.pullBlob)
			compressed, ok := store.Pull(dl.Digest)
			if !ok {
				fmt.Printf("  [Pull] 레이어 %d: blob을 찾을 수 없음 (%s)\n", dl.Index, dl.Digest[:30])
				return
			}

			// 압축 해제 (tart: OutputFilter(.decompress, using: .lz4))
			decompressed, err := decompress(compressed)
			if err != nil {
				fmt.Printf("  [Pull] 레이어 %d: 해제 실패: %v\n", dl.Index, err)
				return
			}

			// 비압축 다이제스트 검증
			actualDigest := computeDigest(decompressed)
			if actualDigest != dl.UncompressedContentDigest {
				fmt.Printf("  [Pull] 레이어 %d: 다이제스트 불일치!\n", dl.Index)
				return
			}

			// 제로 스킵 쓰기 (tart: zeroSkippingWrite)
			f, err := os.OpenFile(diskPath, os.O_WRONLY, 0644)
			if err != nil {
				fmt.Printf("  [Pull] 레이어 %d: 파일 열기 실패: %v\n", dl.Index, err)
				return
			}
			defer f.Close()

			_, written, skipped := zeroSkippingWrite(f, offset, decompressed)

			pullMu.Lock()
			totalWritten += written
			totalSkipped += skipped
			pullMu.Unlock()

			fmt.Printf("  [Pull] 레이어 %d: 오프셋 %d, %d 바이트 해제, 쓴 청크=%d, 스킵=%d\n",
				dl.Index, offset, len(decompressed), written, skipped)
		}(layer, diskOffset)
	}

	wg.Wait()

	elapsed := time.Since(startTime)
	fmt.Printf("  Pull 완료: %d개 레이어, 소요시간: %v\n", len(layers), elapsed)
	fmt.Printf("  제로 스킵 통계: 쓴 청크=%d, 스킵한 청크=%d\n", totalWritten, totalSkipped)

	return nil
}

// --- 테스트 디스크 데이터 생성 ---

// createTestDisk는 테스트용 디스크 데이터를 생성한다.
// 실제 데이터와 제로 영역을 혼합하여 제로 스킵 최적화를 검증한다.
func createTestDisk(size int) []byte {
	data := make([]byte, size)

	// 영역 1: 텍스트 데이터 (비압축성 높음)
	msg := []byte("=== macOS VM 디스크 이미지 시뮬레이션 === ")
	for i := 0; i < size/4; i++ {
		data[i] = msg[i%len(msg)]
	}

	// 영역 2: 제로 영역 (희소 파일 — 제로 스킵 대상)
	// 이미 make([]byte, size)로 0으로 초기화됨

	// 영역 3: 반복 패턴 (압축성 높음)
	pattern := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	for i := size / 2; i < size*3/4; i++ {
		data[i] = pattern[i%len(pattern)]
	}

	// 영역 4: 다시 제로 영역

	return data
}

// =============================================================================
// 메인 함수
// =============================================================================

func main() {
	fmt.Println("=== LZ4 스타일 청크 분할 병렬 Push/Pull 시뮬레이션 ===")
	fmt.Println("(tart OCI/Layerizer/DiskV2.swift, Disk.swift 기반)")
	fmt.Println()

	// --- 1. tart DiskV2 구조 설명 ---
	fmt.Println("========== 1. tart DiskV2 아키텍처 ==========")
	fmt.Println()
	fmt.Println("tart DiskV2.swift 핵심 상수:")
	fmt.Println("  bufferSizeBytes      = 4MB     (압축/해제 버퍼)")
	fmt.Println("  layerLimitBytes      = 512MB   (레이어당 비압축 한도)")
	fmt.Println("  holeGranularityBytes = 4MB     (제로 스킵 검사 단위)")
	fmt.Println()
	fmt.Println("시뮬레이션 상수 (축소):")
	fmt.Printf("  layerLimitBytes      = %d 바이트\n", layerLimitBytes)
	fmt.Printf("  holeGranularityBytes = %d 바이트\n", holeGranularityBytes)
	fmt.Printf("  maxConcurrency       = %d\n", maxConcurrency)
	fmt.Println()

	// --- 2. 테스트 디스크 생성 ---
	fmt.Println("========== 2. 테스트 디스크 생성 ==========")
	fmt.Println()

	diskSize := 1024 // 1KB 테스트 디스크
	diskData := createTestDisk(diskSize)
	fmt.Printf("  디스크 크기: %d 바이트\n", len(diskData))
	fmt.Printf("  영역 구성:\n")
	fmt.Printf("    [0, %d): 텍스트 데이터\n", diskSize/4)
	fmt.Printf("    [%d, %d): 제로 영역 (희소)\n", diskSize/4, diskSize/2)
	fmt.Printf("    [%d, %d): 반복 패턴 (0xDEADBEEF)\n", diskSize/2, diskSize*3/4)
	fmt.Printf("    [%d, %d): 제로 영역 (희소)\n", diskSize*3/4, diskSize)
	fmt.Println()

	// --- 3. Push 워크플로우 ---
	fmt.Println("========== 3. 병렬 Push ==========")
	fmt.Println()

	store := NewBlobStore()
	layers := pushDisk(diskData, store, maxConcurrency)
	fmt.Println()

	// 레이어 상세 정보
	fmt.Println("Push된 레이어 상세:")
	var totalCompressed, totalUncompressed int
	for _, layer := range layers {
		fmt.Printf("  레이어 %d: 압축=%d B, 비압축=%d B, 다이제스트=%s...\n",
			layer.Index, layer.Size, layer.UncompressedSize, layer.Digest[:30])
		totalCompressed += layer.Size
		totalUncompressed += int(layer.UncompressedSize)
	}
	ratio := float64(totalCompressed) / float64(totalUncompressed) * 100
	fmt.Printf("  합계: %d -> %d 바이트 (압축률 %.1f%%)\n\n", totalUncompressed, totalCompressed, ratio)

	// --- 4. Pull 워크플로우 ---
	fmt.Println("========== 4. 병렬 Pull + 제로 스킵 ==========")
	fmt.Println()

	tmpDir, _ := os.MkdirTemp("", "disk-layer-poc")
	defer os.RemoveAll(tmpDir)

	pulledDiskPath := filepath.Join(tmpDir, "disk.img")
	if err := pullDisk(layers, store, pulledDiskPath, maxConcurrency); err != nil {
		fmt.Printf("  Pull 실패: %v\n", err)
		return
	}
	fmt.Println()

	// --- 5. 무결성 검증 ---
	fmt.Println("========== 5. 무결성 검증 ==========")
	fmt.Println()

	pulledData, err := os.ReadFile(pulledDiskPath)
	if err != nil {
		fmt.Printf("  Pull된 디스크 읽기 실패: %v\n", err)
		return
	}

	originalDigest := computeDigest(diskData)
	pulledDigest := computeDigest(pulledData)
	match := originalDigest == pulledDigest

	fmt.Printf("  원본 디스크 다이제스트: %s...\n", originalDigest[:40])
	fmt.Printf("  Pull 디스크 다이제스트: %s...\n", pulledDigest[:40])
	fmt.Printf("  일치: %v\n", match)

	if !match {
		// 불일치 원인 분석
		diffCount := 0
		for i := 0; i < len(diskData) && i < len(pulledData); i++ {
			if diskData[i] != pulledData[i] {
				diffCount++
				if diffCount <= 5 {
					fmt.Printf("    오프셋 %d: 원본=0x%02x, Pull=0x%02x\n", i, diskData[i], pulledData[i])
				}
			}
		}
		fmt.Printf("    총 차이 바이트: %d\n", diffCount)
	}
	fmt.Println()

	// --- 6. 제로 스킵 최적화 분석 ---
	fmt.Println("========== 6. 제로 스킵 최적화 분석 ==========")
	fmt.Println()
	fmt.Println("tart DiskV2.zeroSkippingWrite 동작 원리:")
	fmt.Println("  1) holeGranularityBytes(4MB) 단위로 데이터 분할")
	fmt.Println("  2) chunk == zeroChunk (static zero 배열과 비교)")
	fmt.Println("  3) 제로 청크: 쓰기 건너뜀 (truncate로 이미 0)")
	fmt.Println("  4) 비제로 청크: seek(offset) + write(chunk)")
	fmt.Println()

	// 제로 영역 비율 분석
	zeroChunks := 0
	nonZeroChunks := 0
	for i := 0; i < len(diskData); i += holeGranularityBytes {
		end := i + holeGranularityBytes
		if end > len(diskData) {
			end = len(diskData)
		}
		if isZeroChunk(diskData[i:end]) {
			zeroChunks++
		} else {
			nonZeroChunks++
		}
	}
	totalChunks := zeroChunks + nonZeroChunks
	fmt.Printf("  전체 청크 수: %d (각 %d 바이트)\n", totalChunks, holeGranularityBytes)
	fmt.Printf("  제로 청크: %d (%.1f%%) -> 쓰기 건너뜀\n",
		zeroChunks, float64(zeroChunks)/float64(totalChunks)*100)
	fmt.Printf("  비제로 청크: %d (%.1f%%) -> 실제 쓰기\n",
		nonZeroChunks, float64(nonZeroChunks)/float64(totalChunks)*100)
	fmt.Printf("  I/O 절약: %d 바이트 (%.1f%%)\n",
		zeroChunks*holeGranularityBytes,
		float64(zeroChunks)/float64(totalChunks)*100)
	fmt.Println()

	// --- 7. 중복 Push 방지 테스트 ---
	fmt.Println("========== 7. 중복 Push 방지 (blobExists) ==========")
	fmt.Println()
	fmt.Println("같은 디스크를 다시 Push (이미 존재하는 blob은 건너뜀):")
	_ = pushDisk(diskData, store, maxConcurrency)
	fmt.Println()

	// --- 8. LocalLayerCache 개념 ---
	fmt.Println("========== 8. LocalLayerCache (로컬 레이어 캐시) ==========")
	fmt.Println()
	fmt.Println("tart DiskV2.pull에서 localLayerCache 활용:")
	fmt.Println("  1) 기존 Pull된 이미지의 매니페스트 비교")
	fmt.Println("  2) 동일한 레이어(digest 일치)를 로컬에서 복사")
	fmt.Println("  3) deduplicate 모드: 기존 디스크를 clone + 차이만 덮어쓰기")
	fmt.Println("  4) 1GB 이상 절약 가능할 때만 활용")
	fmt.Println()
	fmt.Println("  코드 흐름:")
	fmt.Println("  chooseLocalLayerCache() -> target.intersection(manifest.layers)")
	fmt.Println("  -> deduplicatedBytes > 1GB -> 가장 많은 중복 제거하는 이미지 선택")
	fmt.Println()

	// --- 요약 ---
	fmt.Println("========== 요약 ==========")
	fmt.Println()
	fmt.Println("tart DiskV2 Push 흐름:")
	fmt.Println("  디스크 파일 -> layerLimitBytes 청크 분할")
	fmt.Println("  -> LZ4 압축 -> SHA256 다이제스트")
	fmt.Println("  -> 병렬 Push (TaskGroup + concurrency 제한)")
	fmt.Println("  -> OCIManifestLayer 메타데이터 반환")
	fmt.Println()
	fmt.Println("tart DiskV2 Pull 흐름:")
	fmt.Println("  Manifest에서 레이어 목록 추출")
	fmt.Println("  -> truncate(uncompressedDiskSize)")
	fmt.Println("  -> 병렬 Pull + LZ4 해제")
	fmt.Println("  -> zeroSkippingWrite (제로 청크 건너뜀)")
	fmt.Println("  -> resume 지원 (이미 해제된 레이어 스킵)")
	fmt.Println()
	fmt.Println("핵심 최적화:")
	fmt.Println("  - 병렬 처리: TaskGroup + semaphore 동시성 제한")
	fmt.Println("  - 제로 스킵: 희소 파일에서 제로 영역 쓰기 생략")
	fmt.Println("  - blobExists: 이미 Push된 blob 재전송 방지")
	fmt.Println("  - LocalLayerCache: 동일 레이어 네트워크 다운로드 생략")
	fmt.Println("  - Resumable Pull: 이미 기록된 레이어 다이제스트 비교")
	fmt.Println()
	fmt.Println("[완료] 디스크 레이어 병렬 Push/Pull 시뮬레이션 성공")
}
