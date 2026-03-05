package main

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"
)

// =============================================================================
// tart 캐시 프루닝(Prune) 시뮬레이션
// =============================================================================
//
// tart는 OCI 캐시와 IPSW 캐시를 관리하며, 디스크 용량 부족 시 자동으로 오래된
// 캐시 항목을 삭제한다. 세 가지 전략을 제공한다:
//
// 1. pruneOlderThan: 기간 기반 삭제 (N일 이상 미접근 항목)
// 2. pruneSpaceBudget: LRU 정렬 후 용량 예산 초과 항목 삭제
// 3. reclaimIfNeeded: 디스크 가용 용량 부족 시 자동 LRU 삭제
//
// 실제 소스: Sources/tart/Commands/Prune.swift, Prunable.swift, URL+Prunable.swift

// =============================================================================
// Prunable 인터페이스 — Sources/tart/Prunable.swift 참조
// =============================================================================

// Prunable은 삭제 가능한 캐시 항목을 나타내는 인터페이스이다.
// tart의 Prunable 프로토콜:
//
//	protocol Prunable {
//	  var url: URL { get }
//	  func delete() throws
//	  func accessDate() throws -> Date
//	  func sizeBytes() throws -> Int        // 논리적 크기 (빈 블록 포함)
//	  func allocatedSizeBytes() throws -> Int // 실제 디스크 할당 크기
//	}
type Prunable interface {
	Name() string
	AccessDate() time.Time
	SizeBytes() int       // 논리적 크기
	AllocatedSize() int   // 실제 디스크 사용량
	Delete()
	IsDeleted() bool
}

// PrunableStorage는 Prunable 항목의 저장소를 나타낸다.
// tart에서 VMStorageOCI, IPSWCache, VMStorageLocal이 이 프로토콜을 구현한다.
type PrunableStorage interface {
	Prunables() []Prunable
	Name() string
}

// =============================================================================
// CacheEntry — 캐시 항목 구현
// =============================================================================

// CacheEntry는 하나의 캐시 항목을 나타낸다.
// tart의 URL+Prunable.swift에서 URL 확장으로 구현된 Prunable을 시뮬레이션한다.
type CacheEntry struct {
	name          string
	accessDate    time.Time
	sizeBytes     int    // 논리적 크기
	allocatedSize int    // 실제 디스크 할당 크기 (CoW로 인해 다를 수 있음)
	deleted       bool
	entryType     string // "oci" 또는 "ipsw" 또는 "vm"
}

func (c *CacheEntry) Name() string           { return c.name }
func (c *CacheEntry) AccessDate() time.Time   { return c.accessDate }
func (c *CacheEntry) SizeBytes() int          { return c.sizeBytes }
func (c *CacheEntry) AllocatedSize() int      { return c.allocatedSize }
func (c *CacheEntry) IsDeleted() bool         { return c.deleted }

func (c *CacheEntry) Delete() {
	if !c.deleted {
		fmt.Printf("    [삭제] %s (%.1f MB, 접근: %s)\n",
			c.name, float64(c.allocatedSize)/1024/1024,
			c.accessDate.Format("2006-01-02 15:04"))
		c.deleted = true
	}
}

// =============================================================================
// OCIStorage / IPSWStorage — PrunableStorage 구현
// =============================================================================

// OCIStorage는 OCI 레이어 캐시 저장소이다.
// tart의 VMStorageOCI.prunables()를 시뮬레이션한다.
type OCIStorage struct {
	entries []*CacheEntry
}

func (s *OCIStorage) Name() string { return "OCI 캐시" }

func (s *OCIStorage) Prunables() []Prunable {
	var result []Prunable
	for _, e := range s.entries {
		if !e.deleted {
			result = append(result, e)
		}
	}
	return result
}

// IPSWStorage는 IPSW (macOS 복원 이미지) 캐시 저장소이다.
// tart의 IPSWCache.prunables()를 시뮬레이션한다.
type IPSWStorage struct {
	entries []*CacheEntry
}

func (s *IPSWStorage) Name() string { return "IPSW 캐시" }

func (s *IPSWStorage) Prunables() []Prunable {
	var result []Prunable
	for _, e := range s.entries {
		if !e.deleted {
			result = append(result, e)
		}
	}
	return result
}

// VMStorage는 로컬 VM 저장소이다.
// tart의 VMStorageLocal.prunables()를 시뮬레이션한다.
type VMStorage struct {
	entries []*CacheEntry
}

func (s *VMStorage) Name() string { return "로컬 VM" }

func (s *VMStorage) Prunables() []Prunable {
	var result []Prunable
	for _, e := range s.entries {
		if !e.deleted {
			result = append(result, e)
		}
	}
	return result
}

// =============================================================================
// pruneOlderThan — Sources/tart/Commands/Prune.swift:77 참조
// =============================================================================

// pruneOlderThan은 지정된 날짜보다 오래된 캐시 항목을 삭제한다.
//
// tart 원본:
//
//	static func pruneOlderThan(prunableStorages: [PrunableStorage], olderThanDate: Date) throws {
//	  let prunables: [Prunable] = try prunableStorages.flatMap { try $0.prunables() }
//	  try prunables.filter { try $0.accessDate() <= olderThanDate }.forEach { try $0.delete() }
//	}
func pruneOlderThan(storages []PrunableStorage, olderThan time.Time) int {
	count := 0
	for _, storage := range storages {
		for _, p := range storage.Prunables() {
			if !p.AccessDate().After(olderThan) {
				p.Delete()
				count++
			}
		}
	}
	return count
}

// =============================================================================
// pruneSpaceBudget — Sources/tart/Commands/Prune.swift:83 참조
// =============================================================================

// pruneSpaceBudget은 LRU 순서로 정렬한 뒤, 용량 예산을 초과하는 항목을 삭제한다.
// 가장 최근에 접근한 항목을 우선 유지하고, 예산 내에 들지 않는 항목을 삭제한다.
//
// tart 원본:
//
//	static func pruneSpaceBudget(..., spaceBudgetBytes: UInt64) throws {
//	  let prunables = ... .sorted { try $0.accessDate() > $1.accessDate() }  // 최신순 정렬
//	  var spaceBudgetBytes = spaceBudgetBytes
//	  for prunable in prunables {
//	    let size = UInt64(try prunable.allocatedSizeBytes())
//	    if size <= spaceBudgetBytes {
//	      spaceBudgetBytes -= size  // 예산 내 → 유지
//	    } else {
//	      prunablesToDelete.append(prunable)  // 예산 초과 → 삭제 대상
//	    }
//	  }
//	}
func pruneSpaceBudget(storages []PrunableStorage, budgetBytes int) int {
	// 모든 저장소에서 prunable 항목 수집
	var all []Prunable
	for _, storage := range storages {
		all = append(all, storage.Prunables()...)
	}

	// 최신 접근 순으로 정렬 (가장 최근 접근이 먼저)
	sort.Slice(all, func(i, j int) bool {
		return all[i].AccessDate().After(all[j].AccessDate())
	})

	remaining := budgetBytes
	var toDelete []Prunable

	for _, p := range all {
		size := p.AllocatedSize()
		if size <= remaining {
			// 예산 내에 들어감 → 유지
			remaining -= size
		} else {
			// 예산 초과 → 삭제 대상
			toDelete = append(toDelete, p)
		}
	}

	for _, p := range toDelete {
		p.Delete()
	}

	return len(toDelete)
}

// =============================================================================
// reclaimIfNeeded — Sources/tart/Commands/Prune.swift:107 참조
// =============================================================================

// DiskInfo는 디스크 용량 정보를 시뮬레이션한다.
type DiskInfo struct {
	TotalCapacity     int // 전체 디스크 용량
	AvailableCapacity int // 사용 가능한 용량
}

// reclaimIfNeeded는 디스크 가용 용량이 부족할 때 자동으로 LRU 삭제를 수행한다.
// tart에서는 TART_NO_AUTO_PRUNE 환경변수로 비활성화할 수 있다.
//
// tart 원본 흐름:
// 1. 디스크 가용 용량 확인 (volumeAvailableCapacityForImportantUsage)
// 2. requiredBytes < availableCapacity이면 리턴 (충분함)
// 3. 부족하면 reclaimIfPossible 호출 → LRU 순서로 삭제
// 4. initiator는 삭제 대상에서 제외 (자기 자신 보호)
func reclaimIfNeeded(storages []PrunableStorage, disk *DiskInfo, requiredBytes int, initiatorName string) int {
	fmt.Printf("    필요 용량: %.1f MB\n", float64(requiredBytes)/1024/1024)
	fmt.Printf("    가용 용량: %.1f MB\n", float64(disk.AvailableCapacity)/1024/1024)

	// 가용 용량이 충분하면 리턴
	if requiredBytes < disk.AvailableCapacity {
		fmt.Println("    -> 가용 용량 충분, 프루닝 불필요")
		return 0
	}

	reclaimBytes := requiredBytes - disk.AvailableCapacity
	fmt.Printf("    -> 회수 필요: %.1f MB\n", float64(reclaimBytes)/1024/1024)

	return reclaimIfPossible(storages, reclaimBytes, initiatorName, disk)
}

// reclaimIfPossible — Sources/tart/Commands/Prune.swift:148 참조
// LRU 순서(가장 오래된 것부터)로 삭제하여 용량을 회수한다.
// initiator(현재 작업 중인 항목)는 삭제 대상에서 제외한다.
func reclaimIfPossible(storages []PrunableStorage, reclaimBytes int, initiatorName string, disk *DiskInfo) int {
	// 모든 prunable을 LRU 순으로 정렬 (오래된 것 먼저)
	var all []Prunable
	for _, storage := range storages {
		all = append(all, storage.Prunables()...)
	}

	sort.Slice(all, func(i, j int) bool {
		return all[i].AccessDate().Before(all[j].AccessDate())
	})

	// 캐시 전체 크기가 회수 요구량보다 작으면 의미 없음
	totalCached := 0
	for _, p := range all {
		totalCached += p.AllocatedSize()
	}
	if totalCached < reclaimBytes {
		fmt.Println("    -> 캐시 전체 크기가 회수 요구량보다 작음, 프루닝 불가")
		return 0
	}

	reclaimed := 0
	count := 0

	for _, p := range all {
		if reclaimed >= reclaimBytes {
			break
		}

		// initiator는 삭제하지 않음 (자기 자신 보호)
		if p.Name() == initiatorName {
			fmt.Printf("    [건너뜀] %s (initiator — 자기 보호)\n", p.Name())
			continue
		}

		size := p.AllocatedSize()
		p.Delete()
		reclaimed += size
		disk.AvailableCapacity += size // 삭제 후 가용 용량 증가
		count++
	}

	fmt.Printf("    -> 총 회수: %.1f MB (%d개 항목)\n", float64(reclaimed)/1024/1024, count)
	return count
}

// =============================================================================
// GC (가비지 컬렉션) — 참조 카운트 기반
// =============================================================================

// RefCountEntry는 참조 카운트가 있는 캐시 항목이다.
// tart의 VMStorageOCI.gc()는 OCI 레이어의 참조 카운트를 확인하여
// 어떤 VM에서도 참조하지 않는 레이어를 삭제한다.
type RefCountEntry struct {
	CacheEntry
	refCount int
}

// gc는 참조 카운트가 0인 항목을 삭제한다.
func gc(entries []*RefCountEntry) int {
	count := 0
	for _, e := range entries {
		if e.refCount == 0 && !e.deleted {
			fmt.Printf("    [GC] %s (ref=0, %.1f MB)\n",
				e.name, float64(e.allocatedSize)/1024/1024)
			e.deleted = true
			count++
		}
	}
	return count
}

// =============================================================================
// 테스트 데이터 생성
// =============================================================================

func mb(n int) int { return n * 1024 * 1024 }

func makeTime(daysAgo int) time.Time {
	return time.Now().AddDate(0, 0, -daysAgo)
}

func createOCIStorage() *OCIStorage {
	return &OCIStorage{
		entries: []*CacheEntry{
			{name: "ghcr.io/org/macos-sonoma:latest", accessDate: makeTime(1), sizeBytes: mb(15000), allocatedSize: mb(12000), entryType: "oci"},
			{name: "ghcr.io/org/macos-ventura:latest", accessDate: makeTime(5), sizeBytes: mb(14000), allocatedSize: mb(11000), entryType: "oci"},
			{name: "ghcr.io/org/ubuntu-22:latest", accessDate: makeTime(10), sizeBytes: mb(3000), allocatedSize: mb(2500), entryType: "oci"},
			{name: "ghcr.io/org/debian-12:latest", accessDate: makeTime(15), sizeBytes: mb(2500), allocatedSize: mb(2000), entryType: "oci"},
			{name: "ghcr.io/org/macos-monterey:old", accessDate: makeTime(30), sizeBytes: mb(13000), allocatedSize: mb(10000), entryType: "oci"},
		},
	}
}

func createIPSWStorage() *IPSWStorage {
	return &IPSWStorage{
		entries: []*CacheEntry{
			{name: "UniversalMac_14.2_Restore.ipsw", accessDate: makeTime(2), sizeBytes: mb(13000), allocatedSize: mb(13000), entryType: "ipsw"},
			{name: "UniversalMac_13.6_Restore.ipsw", accessDate: makeTime(20), sizeBytes: mb(12500), allocatedSize: mb(12500), entryType: "ipsw"},
			{name: "UniversalMac_12.7_Restore.ipsw", accessDate: makeTime(45), sizeBytes: mb(12000), allocatedSize: mb(12000), entryType: "ipsw"},
		},
	}
}

func printStorageStatus(storages []PrunableStorage) {
	totalSize := 0
	totalCount := 0
	for _, s := range storages {
		prunables := s.Prunables()
		storageSize := 0
		for _, p := range prunables {
			storageSize += p.AllocatedSize()
		}
		fmt.Printf("  %s: %d개 항목, %.1f GB\n",
			s.Name(), len(prunables), float64(storageSize)/1024/1024/1024)
		totalSize += storageSize
		totalCount += len(prunables)
	}
	fmt.Printf("  합계: %d개 항목, %.1f GB\n", totalCount, float64(totalSize)/1024/1024/1024)
}

func printSeparator(title string) {
	fmt.Printf("\n%s\n", strings.Repeat("=", 70))
	fmt.Printf("  %s\n", title)
	fmt.Printf("%s\n\n", strings.Repeat("=", 70))
}

// =============================================================================
// 메인 함수
// =============================================================================

func main() {
	rand.New(rand.NewSource(time.Now().UnixNano()))

	fmt.Println("tart 캐시 프루닝(Prune) 시뮬레이션")
	fmt.Println("실제 소스: Sources/tart/Commands/Prune.swift, Prunable.swift")

	// =========================================================================
	// 1. pruneOlderThan: 기간 기반 삭제
	// =========================================================================
	printSeparator("1. pruneOlderThan — 기간 기반 삭제 (7일 이상 미접근)")

	oci := createOCIStorage()
	ipsw := createIPSWStorage()
	storages := []PrunableStorage{oci, ipsw}

	fmt.Println("  [삭제 전]")
	printStorageStatus(storages)

	olderThan := time.Now().AddDate(0, 0, -7)
	fmt.Printf("\n  기준: %s 이전 접근 항목 삭제\n\n", olderThan.Format("2006-01-02"))

	deleted := pruneOlderThan(storages, olderThan)
	fmt.Printf("\n  삭제된 항목: %d개\n", deleted)
	fmt.Println("\n  [삭제 후]")
	printStorageStatus(storages)

	// =========================================================================
	// 2. pruneSpaceBudget: LRU 정렬 후 예산 기반 삭제
	// =========================================================================
	printSeparator("2. pruneSpaceBudget — 용량 예산 기반 삭제 (30 GB 제한)")

	oci = createOCIStorage()
	ipsw = createIPSWStorage()
	storages = []PrunableStorage{oci, ipsw}

	fmt.Println("  [삭제 전]")
	printStorageStatus(storages)

	budgetGB := 30
	fmt.Printf("\n  용량 예산: %d GB\n", budgetGB)
	fmt.Println("  (최신 접근 항목부터 유지, 예산 초과 시 삭제)\n")

	deleted = pruneSpaceBudget(storages, budgetGB*1024*1024*1024)
	fmt.Printf("\n  삭제된 항목: %d개\n", deleted)
	fmt.Println("\n  [삭제 후]")
	printStorageStatus(storages)

	// =========================================================================
	// 3. reclaimIfNeeded: 디스크 용량 부족 시 자동 삭제
	// =========================================================================
	printSeparator("3. reclaimIfNeeded — 디스크 용량 부족 시 자동 삭제")

	oci = createOCIStorage()
	ipsw = createIPSWStorage()
	storages = []PrunableStorage{oci, ipsw}

	disk := &DiskInfo{
		TotalCapacity:     500 * 1024 * 1024 * 1024, // 500 GB
		AvailableCapacity: 8 * 1024 * 1024 * 1024,   // 8 GB 남음
	}

	fmt.Println("  [시나리오] 15 GB IPSW 다운로드 필요, 디스크에 8 GB만 남음")
	fmt.Printf("  디스크: 전체 %.0f GB, 가용 %.0f GB\n",
		float64(disk.TotalCapacity)/1024/1024/1024,
		float64(disk.AvailableCapacity)/1024/1024/1024)
	fmt.Println()

	// initiator: 현재 다운로드 중인 IPSW는 삭제하지 않음
	deleted = reclaimIfNeeded(storages, disk, 15*1024*1024*1024, "UniversalMac_14.2_Restore.ipsw")

	fmt.Printf("\n  삭제된 항목: %d개\n", deleted)
	fmt.Printf("  디스크 가용 용량: %.1f GB\n", float64(disk.AvailableCapacity)/1024/1024/1024)

	// =========================================================================
	// 4. reclaimIfNeeded: 용량 충분한 경우
	// =========================================================================
	printSeparator("4. reclaimIfNeeded — 용량 충분한 경우")

	oci = createOCIStorage()
	storages2 := []PrunableStorage{oci}

	disk2 := &DiskInfo{
		TotalCapacity:     500 * 1024 * 1024 * 1024,
		AvailableCapacity: 100 * 1024 * 1024 * 1024, // 100 GB
	}

	fmt.Println("  [시나리오] 5 GB 필요, 디스크에 100 GB 남음")
	reclaimIfNeeded(storages2, disk2, 5*1024*1024*1024, "")

	// =========================================================================
	// 5. GC: 참조 카운트 기반 가비지 컬렉션
	// =========================================================================
	printSeparator("5. GC — 참조 카운트 기반 가비지 컬렉션")

	gcEntries := []*RefCountEntry{
		{CacheEntry: CacheEntry{name: "layer-sha256:abc123", allocatedSize: mb(500)}, refCount: 2},
		{CacheEntry: CacheEntry{name: "layer-sha256:def456", allocatedSize: mb(300)}, refCount: 0},
		{CacheEntry: CacheEntry{name: "layer-sha256:ghi789", allocatedSize: mb(800)}, refCount: 1},
		{CacheEntry: CacheEntry{name: "layer-sha256:jkl012", allocatedSize: mb(200)}, refCount: 0},
		{CacheEntry: CacheEntry{name: "layer-sha256:mno345", allocatedSize: mb(600)}, refCount: 0},
	}

	fmt.Println("  OCI 레이어 참조 카운트:")
	for _, e := range gcEntries {
		fmt.Printf("    %s: ref=%d, %.1f MB\n",
			e.name, e.refCount, float64(e.allocatedSize)/1024/1024)
	}
	fmt.Println()

	gcCount := gc(gcEntries)
	fmt.Printf("\n  GC 삭제: %d개 레이어\n", gcCount)

	// =========================================================================
	// 6. 시간 흐름 시뮬레이션: 캐시 생성 → 접근 → 프루닝 반복
	// =========================================================================
	printSeparator("6. 시간 흐름 시뮬레이션 (캐시 생성/접근/프루닝)")

	simOCI := &OCIStorage{}
	simIPSW := &IPSWStorage{}
	simStorages := []PrunableStorage{simOCI, simIPSW}

	imageNames := []string{
		"ghcr.io/org/macos-14", "ghcr.io/org/macos-13",
		"ghcr.io/org/ubuntu-22", "ghcr.io/org/debian-12",
	}

	simDisk := &DiskInfo{
		TotalCapacity:     200 * 1024 * 1024 * 1024, // 200 GB
		AvailableCapacity: 200 * 1024 * 1024 * 1024,
	}

	// 30일 동안 시뮬레이션
	for day := 30; day >= 0; day-- {
		t := makeTime(day)

		// 매 5일마다 새 캐시 항목 추가
		if day%5 == 0 {
			imgIdx := rand.Intn(len(imageNames))
			size := mb(2000 + rand.Intn(10000))
			entry := &CacheEntry{
				name:          fmt.Sprintf("%s:day%d", imageNames[imgIdx], 30-day),
				accessDate:    t,
				sizeBytes:     size,
				allocatedSize: size,
				entryType:     "oci",
			}
			simOCI.entries = append(simOCI.entries, entry)
			simDisk.AvailableCapacity -= size
			fmt.Printf("  Day %02d: [추가] %s (%.1f GB)\n",
				30-day, entry.name, float64(size)/1024/1024/1024)
		}

		// 매 3일마다 랜덤 접근 (접근 시간 갱신)
		if day%3 == 0 && len(simOCI.entries) > 0 {
			alive := []*CacheEntry{}
			for _, e := range simOCI.entries {
				if !e.deleted {
					alive = append(alive, e)
				}
			}
			if len(alive) > 0 {
				idx := rand.Intn(len(alive))
				alive[idx].accessDate = t
			}
		}

		// 매 10일마다 프루닝 (가용 용량이 20 GB 미만이면)
		if day%10 == 0 && simDisk.AvailableCapacity < 20*1024*1024*1024 {
			fmt.Printf("  Day %02d: [프루닝] 가용 용량 %.1f GB < 20 GB\n",
				30-day, float64(simDisk.AvailableCapacity)/1024/1024/1024)
			reclaimIfNeeded(simStorages, simDisk, 20*1024*1024*1024, "")
		}
	}

	fmt.Println("\n  [최종 상태]")
	printStorageStatus(simStorages)
	fmt.Printf("  디스크 가용 용량: %.1f GB\n", float64(simDisk.AvailableCapacity)/1024/1024/1024)

	fmt.Println("\n[완료] tart 캐시 프루닝 시뮬레이션 종료")
}
