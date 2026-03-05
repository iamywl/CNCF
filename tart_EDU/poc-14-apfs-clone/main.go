package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// PoC-14: APFS Copy-on-Write 클론 시뮬레이션
// =============================================================================
// Tart의 VMDirectory.clone() 메서드와 Clone.swift의 APFS CoW 클론을 Go로 재현한다.
// 핵심 개념:
//   - APFS clonefile(): 파일을 복사하지 않고 참조만 생성 (즉각적 완료)
//   - Copy-on-Write: 원본/클론 모두 같은 블록을 참조, 쓰기 시에만 새 블록 할당
//   - 참조 카운팅: 블록별 참조 수 추적, 모든 참조가 사라져야 블록 해제
//   - MAC 주소 재생성: 클론 시 네트워크 충돌 방지를 위해 새 MAC 생성
//   - 크기 절약: sizeBytes (논리적) vs allocatedSizeBytes (물리적) 차이
//
// 실제 소스:
//   - Sources/tart/VMDirectory.swift: clone(to:generateMAC:) 메서드
//   - Sources/tart/Commands/Clone.swift: APFS CoW + 자동 프루닝
// =============================================================================

// ---------------------------------------------------------------------------
// 1. 블록 스토리지 — APFS 블록 레벨 시뮬레이션
// ---------------------------------------------------------------------------

// Block은 디스크 블록 하나를 나타낸다.
type Block struct {
	ID       int    // 블록 고유 ID
	Data     []byte // 블록 데이터
	RefCount int    // 참조 카운팅 (APFS는 블록별로 추적)
}

// BlockStore는 APFS의 블록 저장소를 시뮬레이션한다.
// 실제 APFS는 B-tree 기반으로 블록을 관리한다.
type BlockStore struct {
	mu        sync.RWMutex
	blocks    map[int]*Block
	nextID    int
	blockSize int // 블록 크기 (바이트)
}

func NewBlockStore(blockSize int) *BlockStore {
	return &BlockStore{
		blocks:    make(map[int]*Block),
		nextID:    1,
		blockSize: blockSize,
	}
}

// Allocate는 새 블록을 할당하고 데이터를 기록한다.
func (bs *BlockStore) Allocate(data []byte) int {
	bs.mu.Lock()
	defer bs.mu.Unlock()

	id := bs.nextID
	bs.nextID++

	block := &Block{
		ID:       id,
		Data:     make([]byte, len(data)),
		RefCount: 1,
	}
	copy(block.Data, data)
	bs.blocks[id] = block

	return id
}

// AddRef는 블록의 참조 카운트를 증가시킨다 (clone 시).
func (bs *BlockStore) AddRef(id int) {
	bs.mu.Lock()
	defer bs.mu.Unlock()

	if block, ok := bs.blocks[id]; ok {
		block.RefCount++
	}
}

// Release는 블록의 참조 카운트를 감소시킨다. 0이 되면 블록을 해제한다.
func (bs *BlockStore) Release(id int) {
	bs.mu.Lock()
	defer bs.mu.Unlock()

	if block, ok := bs.blocks[id]; ok {
		block.RefCount--
		if block.RefCount <= 0 {
			delete(bs.blocks, id)
		}
	}
}

// CopyOnWrite는 블록에 쓰기가 발생할 때 호출된다.
// 참조 카운트 > 1이면 새 블록을 할당하고, 기존 참조를 감소시킨다.
func (bs *BlockStore) CopyOnWrite(id int, newData []byte) int {
	bs.mu.Lock()
	defer bs.mu.Unlock()

	block, ok := bs.blocks[id]
	if !ok {
		return -1
	}

	if block.RefCount == 1 {
		// 참조가 1이면 그냥 덮어쓰기 (CoW 불필요)
		block.Data = make([]byte, len(newData))
		copy(block.Data, newData)
		return id
	}

	// 참조 > 1: 새 블록 할당 (Copy-on-Write 발생!)
	block.RefCount--

	newID := bs.nextID
	bs.nextID++
	newBlock := &Block{
		ID:       newID,
		Data:     make([]byte, len(newData)),
		RefCount: 1,
	}
	copy(newBlock.Data, newData)
	bs.blocks[newID] = newBlock

	return newID
}

// Read는 블록 데이터를 읽는다.
func (bs *BlockStore) Read(id int) []byte {
	bs.mu.RLock()
	defer bs.mu.RUnlock()

	if block, ok := bs.blocks[id]; ok {
		result := make([]byte, len(block.Data))
		copy(result, block.Data)
		return result
	}
	return nil
}

// Stats는 블록 저장소 통계를 반환한다.
func (bs *BlockStore) Stats() (totalBlocks, totalRefs, uniqueBlocks int) {
	bs.mu.RLock()
	defer bs.mu.RUnlock()

	for _, block := range bs.blocks {
		uniqueBlocks++
		totalRefs += block.RefCount
	}
	totalBlocks = uniqueBlocks
	return
}

// AllocatedBytes는 실제 디스크 사용량을 반환한다.
func (bs *BlockStore) AllocatedBytes() int {
	bs.mu.RLock()
	defer bs.mu.RUnlock()

	total := 0
	for _, block := range bs.blocks {
		total += len(block.Data)
	}
	return total
}

// ---------------------------------------------------------------------------
// 2. VMFile — VM 파일 (디스크, 설정 등) 시뮬레이션
// ---------------------------------------------------------------------------

// VMFile은 VM 번들의 개별 파일을 나타낸다.
// 블록 ID 목록으로 파일 내용을 참조한다.
type VMFile struct {
	Name     string
	BlockIDs []int // 이 파일이 사용하는 블록 ID 목록
	store    *BlockStore
}

// SizeBytes는 파일의 논리적 크기를 반환한다 (sizeBytes).
func (f *VMFile) SizeBytes() int {
	total := 0
	for _, id := range f.BlockIDs {
		data := f.store.Read(id)
		total += len(data)
	}
	return total
}

// AllocatedSizeBytes는 파일의 물리적 디스크 사용량을 반환한다 (allocatedSizeBytes).
// CoW 공유 블록은 1/RefCount만 카운트한다.
func (f *VMFile) AllocatedSizeBytes() int {
	f.store.mu.RLock()
	defer f.store.mu.RUnlock()

	total := 0
	for _, id := range f.BlockIDs {
		if block, ok := f.store.blocks[id]; ok {
			// 공유 블록은 비례 배분
			total += len(block.Data) / block.RefCount
		}
	}
	return total
}

// Clone은 파일을 CoW 클론한다 (참조 카운트만 증가).
// VMDirectory.clone(): try FileManager.default.copyItem(at: diskURL, to: to.diskURL)
// APFS에서 copyItem은 자동으로 clonefile()을 사용한다.
func (f *VMFile) Clone() *VMFile {
	cloned := &VMFile{
		Name:     f.Name,
		BlockIDs: make([]int, len(f.BlockIDs)),
		store:    f.store,
	}
	copy(cloned.BlockIDs, f.BlockIDs)

	// 모든 블록의 참조 카운트 증가 (clonefile 시뮬레이션)
	for _, id := range f.BlockIDs {
		f.store.AddRef(id)
	}

	return cloned
}

// WriteBlock은 특정 블록에 데이터를 쓴다 (CoW 트리거).
func (f *VMFile) WriteBlock(index int, data []byte) {
	if index >= len(f.BlockIDs) {
		return
	}
	oldID := f.BlockIDs[index]
	newID := f.store.CopyOnWrite(oldID, data)
	f.BlockIDs[index] = newID
}

// Release는 파일이 사용하는 모든 블록의 참조를 해제한다.
func (f *VMFile) Release() {
	for _, id := range f.BlockIDs {
		f.store.Release(id)
	}
	f.BlockIDs = nil
}

// ---------------------------------------------------------------------------
// 3. VMDirectory — Tart의 VMDirectory 클론 시뮬레이션
// ---------------------------------------------------------------------------

// VMDirectory는 VM 번들 디렉토리를 나타낸다.
// VMDirectory.swift:
//   struct VMDirectory {
//     var baseURL: URL
//     var configURL, diskURL, nvramURL, stateURL: URL
//     func clone(to: VMDirectory, generateMAC: Bool) throws { ... }
//   }
type VMDirectory struct {
	Name       string
	Config     *VMFile // config.json
	Disk       *VMFile // disk.img
	NVRAM      *VMFile // nvram.bin
	MACAddress string
	CreatedAt  time.Time
}

// Clone은 VM을 CoW 클론한다.
// VMDirectory.swift:
//   func clone(to: VMDirectory, generateMAC: Bool) throws {
//     try FileManager.default.copyItem(at: configURL, to: to.configURL)
//     try FileManager.default.copyItem(at: nvramURL, to: to.nvramURL)
//     try FileManager.default.copyItem(at: diskURL, to: to.diskURL)
//     if generateMAC { try to.regenerateMACAddress() }
//   }
func (vd *VMDirectory) Clone(newName string, generateMAC bool) *VMDirectory {
	cloned := &VMDirectory{
		Name:       newName,
		Config:     vd.Config.Clone(),
		Disk:       vd.Disk.Clone(),
		NVRAM:      vd.NVRAM.Clone(),
		MACAddress: vd.MACAddress,
		CreatedAt:  time.Now(),
	}

	// MAC 주소 재생성 (네트워크 충돌 방지)
	// Clone.swift:
	//   let generateMAC = try localStorage.hasVMsWithMACAddress(macAddress: sourceVM.macAddress())
	//     && sourceVM.state() != .Suspended
	if generateMAC {
		cloned.MACAddress = generateRandomMAC()
	}

	return cloned
}

// SizeBytes는 VM의 논리적 전체 크기를 반환한다.
func (vd *VMDirectory) SizeBytes() int {
	return vd.Config.SizeBytes() + vd.Disk.SizeBytes() + vd.NVRAM.SizeBytes()
}

// AllocatedSizeBytes는 VM의 실제 디스크 사용량을 반환한다.
func (vd *VMDirectory) AllocatedSizeBytes() int {
	return vd.Config.AllocatedSizeBytes() + vd.Disk.AllocatedSizeBytes() + vd.NVRAM.AllocatedSizeBytes()
}

// Release는 VM이 사용하는 모든 블록을 해제한다.
func (vd *VMDirectory) Release() {
	vd.Config.Release()
	vd.Disk.Release()
	vd.NVRAM.Release()
}

// ---------------------------------------------------------------------------
// 4. 헬퍼 함수
// ---------------------------------------------------------------------------

func generateRandomMAC() string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x",
		0x52, 0x54, 0x00, h[0], h[1], h[2])
}

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
// 5. 메인 함수
// ---------------------------------------------------------------------------

func main() {
	fmt.Println("=== PoC-14: APFS Copy-on-Write 클론 시뮬레이션 ===")
	fmt.Println("  소스: VMDirectory.swift (clone 메서드), Commands/Clone.swift")
	fmt.Println()

	// 블록 저장소 생성 (4KB 블록)
	store := NewBlockStore(4096)

	// =========================================================================
	// 데모 1: 원본 VM 생성
	// =========================================================================
	printSeparator("데모 1: 원본 VM 생성")

	// config.json (1블록 = 4KB)
	configBlock := store.Allocate([]byte(`{"version":1,"os":"darwin","cpuCount":4,"memorySize":8589934592}`))
	config := &VMFile{Name: "config.json", BlockIDs: []int{configBlock}, store: store}

	// disk.img (8블록 = 32KB — 실제로는 수십 GB)
	diskBlockIDs := make([]int, 8)
	for i := 0; i < 8; i++ {
		data := make([]byte, 4096)
		for j := range data {
			data[j] = byte(i) // 각 블록에 다른 데이터
		}
		diskBlockIDs[i] = store.Allocate(data)
	}
	disk := &VMFile{Name: "disk.img", BlockIDs: diskBlockIDs, store: store}

	// nvram.bin (1블록)
	nvramBlock := store.Allocate(make([]byte, 2048))
	nvram := &VMFile{Name: "nvram.bin", BlockIDs: []int{nvramBlock}, store: store}

	original := &VMDirectory{
		Name:       "macos-sonoma-base",
		Config:     config,
		Disk:       disk,
		NVRAM:      nvram,
		MACAddress: "7a:65:e4:3f:b2:01",
		CreatedAt:  time.Now(),
	}

	fmt.Printf("  VM: %s\n", original.Name)
	fmt.Printf("  MAC: %s\n", original.MACAddress)
	fmt.Printf("  논리적 크기: %s\n", formatBytes(original.SizeBytes()))
	fmt.Printf("  물리적 크기: %s\n", formatBytes(original.AllocatedSizeBytes()))
	totalBlocks, totalRefs, _ := store.Stats()
	fmt.Printf("  블록 저장소: %d 블록, %d 참조\n", totalBlocks, totalRefs)

	// =========================================================================
	// 데모 2: CoW 클론 (즉각적 완료)
	// =========================================================================
	printSeparator("데모 2: CoW 클론 -- APFS clonefile() 시뮬레이션")

	fmt.Println("  VMDirectory.clone() 호출:")
	fmt.Println("    try FileManager.default.copyItem(at: configURL, to: to.configURL)")
	fmt.Println("    try FileManager.default.copyItem(at: nvramURL, to: to.nvramURL)")
	fmt.Println("    try FileManager.default.copyItem(at: diskURL, to: to.diskURL)")
	fmt.Println("    // APFS에서 copyItem은 clonefile()을 사용 -> 참조만 증가")
	fmt.Println()

	start := time.Now()
	clone1 := original.Clone("my-sonoma-1", true)
	elapsed := time.Since(start)

	fmt.Printf("  클론 완료: %s (소요: %v)\n", clone1.Name, elapsed)
	fmt.Printf("  원본 MAC: %s\n", original.MACAddress)
	fmt.Printf("  클론 MAC: %s (재생성됨)\n", clone1.MACAddress)
	fmt.Println()

	// 디스크 사용량 비교
	fmt.Println("  === 디스크 사용량 비교 ===")
	fmt.Printf("  %-20s %-15s %-15s\n", "VM", "논리적 크기", "물리적 크기")
	fmt.Printf("  %s\n", strings.Repeat("-", 50))
	fmt.Printf("  %-20s %-15s %-15s\n", original.Name, formatBytes(original.SizeBytes()), formatBytes(original.AllocatedSizeBytes()))
	fmt.Printf("  %-20s %-15s %-15s\n", clone1.Name, formatBytes(clone1.SizeBytes()), formatBytes(clone1.AllocatedSizeBytes()))
	fmt.Println()

	totalBlocks, totalRefs, _ = store.Stats()
	fmt.Printf("  블록 저장소: %d 블록 (변화 없음!), %d 참조 (2배)\n", totalBlocks, totalRefs)
	fmt.Printf("  실제 디스크 사용: %s (1VM 분량만 사용)\n", formatBytes(store.AllocatedBytes()))
	fmt.Println("  -> clonefile()은 블록을 복사하지 않고 참조만 증가시킴")

	// =========================================================================
	// 데모 3: 쓰기 시 Copy-on-Write 발생
	// =========================================================================
	printSeparator("데모 3: 쓰기 시 Copy-on-Write 발생")

	fmt.Println("  클론 VM의 디스크 블록 0에 데이터 쓰기...")
	beforeBlocks, _, _ := store.Stats()
	beforeAllocated := store.AllocatedBytes()

	// 클론 VM의 디스크에 쓰기 -> CoW 발생
	newData := make([]byte, 4096)
	for i := range newData {
		newData[i] = 0xFF // 새 데이터
	}
	clone1.Disk.WriteBlock(0, newData)

	afterBlocks, _, _ := store.Stats()
	afterAllocated := store.AllocatedBytes()

	fmt.Printf("  블록 수: %d -> %d (+1, 새 블록 할당됨)\n", beforeBlocks, afterBlocks)
	fmt.Printf("  디스크 사용: %s -> %s (+4KB)\n",
		formatBytes(beforeAllocated), formatBytes(afterAllocated))
	fmt.Println()

	// 원본과 클론의 블록 비교
	fmt.Println("  블록 공유 상태:")
	fmt.Printf("  %-12s", "블록 인덱스")
	for i := 0; i < len(original.Disk.BlockIDs); i++ {
		fmt.Printf("  [%d]", i)
	}
	fmt.Println()
	fmt.Printf("  %-12s", "원본 ID")
	for _, id := range original.Disk.BlockIDs {
		fmt.Printf("  %3d", id)
	}
	fmt.Println()
	fmt.Printf("  %-12s", "클론 ID")
	for _, id := range clone1.Disk.BlockIDs {
		fmt.Printf("  %3d", id)
	}
	fmt.Println()
	fmt.Printf("  %-12s", "공유 여부")
	for i := range original.Disk.BlockIDs {
		if original.Disk.BlockIDs[i] == clone1.Disk.BlockIDs[i] {
			fmt.Printf("   공유")
		} else {
			fmt.Printf("   분리")
		}
	}
	fmt.Println("\n  -> 블록 0만 분리됨 (CoW), 나머지는 여전히 공유")

	// =========================================================================
	// 데모 4: 다중 클론 — 참조 카운팅
	// =========================================================================
	printSeparator("데모 4: 다중 클론 -- 참조 카운팅")

	clone2 := original.Clone("my-sonoma-2", true)
	clone3 := original.Clone("my-sonoma-3", true)

	vms := []*VMDirectory{original, clone1, clone2, clone3}

	fmt.Println("  === 4개 VM의 디스크 사용량 ===")
	fmt.Printf("  %-20s %-15s %-15s %-10s\n", "VM", "논리적", "물리적(비례)", "MAC")
	fmt.Printf("  %s\n", strings.Repeat("-", 65))
	totalLogical := 0
	totalPhysical := 0
	for _, vm := range vms {
		logical := vm.SizeBytes()
		physical := vm.AllocatedSizeBytes()
		totalLogical += logical
		totalPhysical += physical
		fmt.Printf("  %-20s %-15s %-15s %s\n",
			vm.Name, formatBytes(logical), formatBytes(physical), vm.MACAddress)
	}
	fmt.Printf("  %s\n", strings.Repeat("-", 65))
	fmt.Printf("  %-20s %-15s %-15s\n", "합계", formatBytes(totalLogical), formatBytes(totalPhysical))
	fmt.Printf("  %-20s %-15s\n", "실제 디스크 사용", formatBytes(store.AllocatedBytes()))
	fmt.Println()

	savingPercent := float64(totalLogical-store.AllocatedBytes()) / float64(totalLogical) * 100
	fmt.Printf("  절약률: %.1f%% (논리적 %s 중 실제 %s만 사용)\n",
		savingPercent, formatBytes(totalLogical), formatBytes(store.AllocatedBytes()))

	// =========================================================================
	// 데모 5: 클론 삭제 — 참조 카운트 감소
	// =========================================================================
	printSeparator("데모 5: 클론 삭제 -- 참조 카운트 감소")

	beforeBlocks, beforeRefs, _ := store.Stats()
	fmt.Printf("  삭제 전: %d 블록, %d 참조\n", beforeBlocks, beforeRefs)

	clone3.Release()
	fmt.Printf("  %s 삭제 (참조 카운트 감소)\n", clone3.Name)

	afterBlocks2, afterRefs2, _ := store.Stats()
	fmt.Printf("  삭제 후: %d 블록, %d 참조\n", afterBlocks2, afterRefs2)
	fmt.Println("  -> 공유 블록은 참조 카운트만 감소, 아직 다른 VM이 사용 중이므로 해제되지 않음")

	// 원본도 삭제
	clone2.Release()
	clone1.Release()
	fmt.Printf("  %s, %s 삭제\n", clone2.Name, clone1.Name)

	afterBlocks3, afterRefs3, _ := store.Stats()
	fmt.Printf("  삭제 후: %d 블록, %d 참조\n", afterBlocks3, afterRefs3)
	fmt.Println("  -> 원본만 남아 참조 카운트가 1로 복귀")

	// =========================================================================
	// 데모 6: Clone.swift의 전체 흐름
	// =========================================================================
	printSeparator("데모 6: Clone.swift 전체 흐름")

	fmt.Println("  Clone.run() 실행 흐름:")
	fmt.Println("  1. OCI 원격 이미지면 pull 수행")
	fmt.Println("     -> if let remoteName = try? RemoteName(sourceName), !ociStorage.exists(remoteName)")
	fmt.Println("  2. VMStorageHelper.open(sourceName) — 원본 VM 디렉토리 열기")
	fmt.Println("  3. VMDirectory.temporary() — 임시 디렉토리 생성")
	fmt.Println("  4. FileLock(lockURL: tmpVMDir.baseURL) — 임시 디렉토리 GC 방지 잠금")
	fmt.Println("  5. FileLock(lockURL: tartHomeDir) — 전역 clone 잠금 획득")
	fmt.Println("  6. sourceVM.clone(to: tmpVMDir, generateMAC:) — APFS CoW 클론")
	fmt.Println("     -> copyItem (config.json, nvram.bin, disk.img)")
	fmt.Println("     -> MAC 주소 충돌 시 재생성")
	fmt.Println("  7. localStorage.move(newName, from: tmpVMDir) — 최종 위치로 이동")
	fmt.Println("  8. 전역 잠금 해제")
	fmt.Println("  9. Prune.reclaimIfNeeded() — 미할당 공간 확보")
	fmt.Println("     -> unallocatedBytes = sizeBytes - allocatedSizeBytes")
	fmt.Println("     -> CoW로 인해 실제 할당은 나중에 발생하므로 미리 공간 확보")
	fmt.Println()

	// sizeBytes vs allocatedSizeBytes 차이 시뮬레이션
	fmt.Println("  === sizeBytes vs allocatedSizeBytes ===")
	fmt.Println("  Clone.swift에서 미할당 바이트 계산:")
	fmt.Println("    let unallocatedBytes = try sourceVM.sizeBytes() - sourceVM.allocatedSizeBytes()")
	fmt.Println("    let reclaimBytes = min(unallocatedBytes, pruneLimit * 1024^3)")
	fmt.Println()

	// 시뮬레이션
	store2 := NewBlockStore(4096)
	diskBlocks2 := make([]int, 100)
	for i := 0; i < 100; i++ {
		data := make([]byte, 4096)
		diskBlocks2[i] = store2.Allocate(data)
	}
	disk2 := &VMFile{Name: "disk.img", BlockIDs: diskBlocks2, store: store2}

	logicalSize := disk2.SizeBytes()
	allocatedSize := disk2.AllocatedSizeBytes()

	fmt.Printf("  원본 VM: sizeBytes=%s, allocatedSizeBytes=%s\n",
		formatBytes(logicalSize), formatBytes(allocatedSize))

	clonedDisk := disk2.Clone()
	clonedFile := &VMFile{Name: "disk.img (clone)", BlockIDs: clonedDisk.BlockIDs, store: clonedDisk.store}

	fmt.Printf("  클론 VM: sizeBytes=%s, allocatedSizeBytes=%s\n",
		formatBytes(clonedFile.SizeBytes()), formatBytes(clonedFile.AllocatedSizeBytes()))

	unallocated := logicalSize - clonedFile.AllocatedSizeBytes()
	fmt.Printf("  미할당 공간 (확보 필요): %s\n", formatBytes(unallocated))
	fmt.Println("  -> 이 공간은 VM 실행 시 쓰기 발생할 때 실제 할당됨")
	fmt.Println("  -> Prune.reclaimIfNeeded()로 미리 디스크 공간 확보")

	// 정리
	disk2.Release()
	clonedFile.Release()
	original.Release()

	// =========================================================================
	// 요약
	// =========================================================================
	printSeparator("APFS Clone 설계 요약")
	fmt.Println("  1. clonefile(): 블록 복사 없이 참조만 증가 -> 즉각적 완료")
	fmt.Println("  2. Copy-on-Write: 쓰기 시에만 새 블록 할당 -> 디스크 절약")
	fmt.Println("  3. 참조 카운팅: 모든 참조가 0이 되어야 블록 해제")
	fmt.Println("  4. sizeBytes vs allocatedSizeBytes: CoW 상태에서의 크기 차이")
	fmt.Println("  5. MAC 주소 재생성: 네트워크 충돌 방지")
	fmt.Println("  6. 자동 프루닝: 미할당 공간을 미리 확보하여 런타임 에러 방지")

	// 임시 디렉토리 생성하여 실제 파일 시스템에서 테스트
	printSeparator("보너스: 실제 파일 시스템 클론 비교")
	tmpDir, err := os.MkdirTemp("", "tart-poc-14-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "임시 디렉토리 생성 실패: %v\n", err)
		return
	}
	defer os.RemoveAll(tmpDir)

	// 원본 파일 생성
	origPath := filepath.Join(tmpDir, "original.dat")
	f, _ := os.Create(origPath)
	data := make([]byte, 1024*1024) // 1MB
	for i := range data {
		data[i] = byte(i % 256)
	}
	f.Write(data)
	f.Close()

	// 일반 복사
	copyPath := filepath.Join(tmpDir, "copy.dat")
	startCopy := time.Now()
	srcData, _ := os.ReadFile(origPath)
	os.WriteFile(copyPath, srcData, 0644)
	copyElapsed := time.Since(startCopy)

	// clonefile 시뮬레이션 (macOS에서는 실제 clonefile 사용 가능)
	// Go 표준 라이브러리에는 clonefile이 없으므로 하드링크로 시뮬레이션
	linkPath := filepath.Join(tmpDir, "clone.dat")
	startClone := time.Now()
	os.Link(origPath, linkPath)
	cloneElapsed := time.Since(startClone)

	origInfo, _ := os.Stat(origPath)
	copyInfo, _ := os.Stat(copyPath)
	linkInfo, _ := os.Stat(linkPath)

	fmt.Printf("  원본: %s (%d bytes)\n", origPath, origInfo.Size())
	fmt.Printf("  복사: %s (%d bytes, 소요: %v)\n", copyPath, copyInfo.Size(), copyElapsed)
	fmt.Printf("  클론: %s (%d bytes, 소요: %v)\n", linkPath, linkInfo.Size(), cloneElapsed)
	if cloneElapsed < copyElapsed {
		fmt.Printf("  클론이 %.1fx 빠름 (참조만 생성)\n", float64(copyElapsed)/float64(cloneElapsed))
	}
}
