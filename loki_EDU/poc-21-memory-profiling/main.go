// Package main은 Loki의 메모리 관리(pkg/memory/)와 트레이싱(pkg/tracing/)의
// 핵심 개념을 시뮬레이션한다.
//
// 시뮬레이션하는 핵심 개념:
// 1. Arena 스타일 메모리 할당기 (Allocator + Region)
// 2. Bitmap 비트 패킹 자료구조 (LSB 순서, IterValues)
// 3. 제네릭 Buffer[T] (Allocator 연동)
// 4. Parent-Child 할당기 계층
// 5. 64바이트 메모리 정렬
// 6. 트레이싱 설정 및 KeyValue 변환
//
// 실행: go run main.go
package main

import (
	"fmt"
	"math/bits"
	"strings"
	"sync"
	"sync/atomic"
)

// ─────────────────────────────────────────────
// 1. Region (메모리 영역)
// ─────────────────────────────────────────────

// Region은 Allocator가 소유하는 연속 메모리 영역이다.
type Region struct {
	data []byte
}

func (r *Region) Data() []byte { return r.data }
func (r *Region) Cap() int     { return cap(r.data) }

// ─────────────────────────────────────────────
// 2. Bitmap (비트 패킹 불리언 배열)
// ─────────────────────────────────────────────

// Bitmap은 LSB 순서의 비트 패킹 불리언 배열이다.
type Bitmap struct {
	data []uint8
	len  int
	off  int
}

func NewBitmap(n int) *Bitmap {
	bmap := &Bitmap{}
	if n > 0 {
		bmap.Grow(n)
	}
	return bmap
}

func (bmap *Bitmap) Grow(n int) {
	needed := bmap.len + n
	bytesCap := len(bmap.data) * 8
	if needed <= bytesCap {
		return
	}
	newCap := max(needed, 2*bytesCap)
	newWords := (newCap + 7) / 8
	newData := make([]uint8, newWords)
	copy(newData, bmap.data)
	bmap.data = newData
}

func (bmap *Bitmap) Resize(n int) {
	if n > len(bmap.data)*8 {
		bmap.Grow(n - bmap.len)
	}
	bmap.len = n
}

func (bmap *Bitmap) Set(i int, value bool) {
	byteIdx := (bmap.off + i) / 8
	bitIdx := uint((bmap.off + i) % 8)
	if value {
		bmap.data[byteIdx] |= 1 << bitIdx
	} else {
		bmap.data[byteIdx] &^= 1 << bitIdx
	}
}

func (bmap *Bitmap) Get(i int) bool {
	byteIdx := (bmap.off + i) / 8
	bitIdx := uint((bmap.off + i) % 8)
	return bmap.data[byteIdx]&(1<<bitIdx) != 0
}

func (bmap *Bitmap) SetRange(from, to int, value bool) {
	for i := from; i < to; i++ {
		bmap.Set(i, value)
	}
}

func (bmap *Bitmap) Append(value bool) {
	if bmap.len >= len(bmap.data)*8 {
		bmap.Grow(1)
	}
	bmap.Set(bmap.len, value)
	bmap.len++
}

func (bmap *Bitmap) Len() int { return bmap.len }

// IterValues는 주어진 값과 일치하는 비트의 인덱스를 반환한다.
// bits.TrailingZeros8을 사용하여 효율적으로 순회한다.
func (bmap *Bitmap) IterValues(value bool) []int {
	var result []int
	var start int
	offset := bmap.off

	for i, word := range bmap.data {
		rem := word
		if !value {
			rem = ^rem // NOT으로 0 비트 찾기
		}

		if i == 0 && offset != 0 {
			rem &= ^uint8(0) << offset
		}

		for rem != 0 {
			firstSet := bits.TrailingZeros8(rem)
			index := start + firstSet - offset
			if index >= bmap.len {
				return result
			}
			result = append(result, index)
			rem ^= 1 << firstSet // 처리한 비트 제거
		}
		start += 8
	}
	return result
}

// SetCount는 설정된 비트 수를 반환한다.
func (bmap *Bitmap) SetCount() int {
	count := 0
	for _, word := range bmap.data {
		count += bits.OnesCount8(word)
	}
	return count
}

// ─────────────────────────────────────────────
// 3. Allocator (Arena 스타일 할당기)
// ─────────────────────────────────────────────

// Allocator는 arena 스타일 메모리 할당기이다.
type Allocator struct {
	locked atomic.Bool

	parent  *Allocator
	regions []*Region
	avail   *Bitmap // 사용 가능한 영역 (1=가용)
	used    *Bitmap // 사용된 영역 (1=사용됨)
	empty   *Bitmap // nil 슬롯 (1=nil)
}

func NewAllocator(parent *Allocator) *Allocator {
	return &Allocator{
		parent: parent,
		avail:  NewBitmap(0),
		used:   NewBitmap(0),
		empty:  NewBitmap(0),
	}
}

func (alloc *Allocator) lock() {
	if !alloc.locked.CompareAndSwap(false, true) {
		panic("detected concurrent use of allocator")
	}
}

func (alloc *Allocator) unlock() {
	if !alloc.locked.CompareAndSwap(true, false) {
		panic("detected concurrent use of allocator")
	}
}

// Allocate는 최소 size 바이트를 수용할 수 있는 Region을 반환한다.
func (alloc *Allocator) Allocate(size int) *Region {
	alloc.lock()
	defer alloc.unlock()

	// 1. 가용한 기존 영역 검색
	for _, i := range alloc.avail.IterValues(true) {
		region := alloc.regions[i]
		if region != nil && cap(region.data) >= size {
			alloc.avail.Set(i, false)
			alloc.used.Set(i, true)
			return region
		}
	}

	// 2. 부모에게서 빌려오기
	if alloc.parent != nil {
		alloc.unlock()
		region := alloc.parent.Allocate(size)
		alloc.lock()
		alloc.addRegionLocked(region, false)
		return region
	}

	// 3. 새로 할당 (64바이트 정렬)
	alignedSize := align64(size)
	region := &Region{data: make([]byte, alignedSize)}
	alloc.addRegionLocked(region, false)
	return region
}

func (alloc *Allocator) addRegionLocked(region *Region, free bool) {
	// nil 슬롯 재활용
	freeSlot := -1
	for _, i := range alloc.empty.IterValues(true) {
		freeSlot = i
		break
	}

	if freeSlot == -1 {
		freeSlot = len(alloc.regions)
		alloc.regions = append(alloc.regions, region)
	} else {
		alloc.regions[freeSlot] = region
	}

	n := len(alloc.regions)
	alloc.avail.Resize(n)
	alloc.used.Resize(n)
	alloc.empty.Resize(n)

	alloc.avail.Set(freeSlot, free)
	alloc.used.Set(freeSlot, !free)
	alloc.empty.Set(freeSlot, false)
}

// Reclaim은 모든 영역을 재사용 가능하게 표시한다.
func (alloc *Allocator) Reclaim() {
	alloc.lock()
	defer alloc.unlock()
	alloc.avail.SetRange(0, len(alloc.regions), true)
	alloc.used.SetRange(0, len(alloc.regions), false)
}

// Trim은 미사용 영역을 해제한다.
func (alloc *Allocator) Trim() {
	alloc.lock()
	defer alloc.unlock()

	for _, i := range alloc.used.IterValues(false) {
		if i >= len(alloc.regions) {
			break
		}
		region := alloc.regions[i]
		if region == nil {
			continue
		}

		alloc.regions[i] = nil
		alloc.empty.Set(i, true)

		if alloc.parent != nil {
			alloc.parent.returnRegion(region)
		}
	}
}

func (alloc *Allocator) returnRegion(region *Region) {
	alloc.lock()
	defer alloc.unlock()
	for i, r := range alloc.regions {
		if r == region {
			alloc.avail.Set(i, true)
			break
		}
	}
}

// Reset = Trim + Reclaim
func (alloc *Allocator) Reset() {
	alloc.Trim()
	alloc.Reclaim()
}

// Free = Reclaim + Trim
func (alloc *Allocator) Free() {
	alloc.Reclaim()
	alloc.Trim()
}

// AllocatedBytes는 할당기가 소유한 총 바이트를 반환한다.
func (alloc *Allocator) AllocatedBytes() int {
	alloc.lock()
	defer alloc.unlock()
	sum := 0
	for _, r := range alloc.regions {
		if r != nil {
			sum += cap(r.data)
		}
	}
	return sum
}

// FreeBytes는 재사용 가능한 바이트를 반환한다.
func (alloc *Allocator) FreeBytes() int {
	alloc.lock()
	defer alloc.unlock()
	sum := 0
	for _, i := range alloc.avail.IterValues(true) {
		if i < len(alloc.regions) && alloc.regions[i] != nil {
			sum += cap(alloc.regions[i].data)
		}
	}
	return sum
}

// ─────────────────────────────────────────────
// 4. 64바이트 정렬
// ─────────────────────────────────────────────

func align64(n int) int {
	return (n + 63) &^ 63
}

// ─────────────────────────────────────────────
// 5. 트레이싱 설정
// ─────────────────────────────────────────────

// TracingConfig는 트레이싱 설정이다.
type TracingConfig struct {
	Enabled bool
}

// KeyValue는 OTel 어트리뷰트 키-값 쌍이다.
type KeyValue struct {
	Key   string
	Value interface{}
}

// KeyValuesToAttributes는 Go Kit 스타일 인자를 KeyValue로 변환한다.
func KeyValuesToAttributes(kvps ...interface{}) []KeyValue {
	attrs := make([]KeyValue, 0, len(kvps)/2)
	for i := 0; i < len(kvps); i += 2 {
		if i+1 < len(kvps) {
			key, ok := kvps[i].(string)
			if !ok {
				key = fmt.Sprintf("not_string_key:%v", kvps[i])
			}
			attrs = append(attrs, KeyValue{Key: key, Value: kvps[i+1]})
		}
	}
	return attrs
}

// ─────────────────────────────────────────────
// 메인 함수
// ─────────────────────────────────────────────

func main() {
	fmt.Println("╔══════════════════════════════════════════════════╗")
	fmt.Println("║  Loki 메모리 관리 & 프로파일링 시뮬레이션         ║")
	fmt.Println("╚══════════════════════════════════════════════════╝")
	fmt.Println()

	// === 1. Bitmap 기본 연산 ===
	fmt.Println("━━━ 1. Bitmap 자료구조 ━━━")
	bmap := NewBitmap(16)
	bmap.Resize(16)

	// LSB 순서로 비트 설정
	bmap.Set(0, true)
	bmap.Set(2, true)
	bmap.Set(5, true)
	bmap.Set(8, true)
	bmap.Set(15, true)

	fmt.Printf("  비트맵 (16비트): ")
	for i := 0; i < bmap.Len(); i++ {
		if bmap.Get(i) {
			fmt.Print("1")
		} else {
			fmt.Print("0")
		}
	}
	fmt.Println()

	// IterValues로 설정된 비트 순회
	setIndices := bmap.IterValues(true)
	fmt.Printf("  설정된 비트 인덱스 (IterValues(true)): %v\n", setIndices)
	clearIndices := bmap.IterValues(false)
	fmt.Printf("  해제된 비트 인덱스 (IterValues(false)): %v\n", clearIndices)
	fmt.Printf("  SetCount: %d, ClearCount: %d\n", bmap.SetCount(), bmap.Len()-bmap.SetCount())

	// SetRange 테스트
	bmap2 := NewBitmap(8)
	bmap2.Resize(8)
	bmap2.SetRange(2, 6, true)
	fmt.Printf("  SetRange(2,6): ")
	for i := 0; i < bmap2.Len(); i++ {
		if bmap2.Get(i) {
			fmt.Print("1")
		} else {
			fmt.Print("0")
		}
	}
	fmt.Println()
	fmt.Println()

	// === 2. Arena Allocator 기본 동작 ===
	fmt.Println("━━━ 2. Arena Allocator ━━━")
	alloc := NewAllocator(nil)

	// 세 개의 Region 할당
	r1 := alloc.Allocate(100)
	r2 := alloc.Allocate(200)
	r3 := alloc.Allocate(300)

	fmt.Printf("  Allocate(100) → cap=%d (정렬: %d→%d)\n", r1.Cap(), 100, align64(100))
	fmt.Printf("  Allocate(200) → cap=%d (정렬: %d→%d)\n", r2.Cap(), 200, align64(200))
	fmt.Printf("  Allocate(300) → cap=%d (정렬: %d→%d)\n", r3.Cap(), 300, align64(300))
	fmt.Printf("  AllocatedBytes: %d\n", alloc.AllocatedBytes())
	fmt.Printf("  FreeBytes: %d\n", alloc.FreeBytes())
	fmt.Println()

	// Reclaim 후 재할당
	fmt.Println("  [Reclaim 후 재할당]")
	alloc.Reclaim()
	fmt.Printf("  Reclaim 후 - AllocatedBytes: %d, FreeBytes: %d\n",
		alloc.AllocatedBytes(), alloc.FreeBytes())

	r4 := alloc.Allocate(150)
	fmt.Printf("  Allocate(150) → cap=%d (기존 Region 재사용: %v)\n",
		r4.Cap(), r4.Cap() >= align64(150))
	fmt.Printf("  재할당 후 - AllocatedBytes: %d, FreeBytes: %d\n",
		alloc.AllocatedBytes(), alloc.FreeBytes())
	fmt.Println()

	// === 3. Parent-Child 할당기 계층 ===
	fmt.Println("━━━ 3. Parent-Child 할당기 ━━━")
	root := NewAllocator(nil)

	// 루트에 큰 영역 미리 할당
	rootR1 := root.Allocate(1024)
	rootR2 := root.Allocate(2048)
	root.Reclaim() // 루트의 영역을 가용으로 전환
	fmt.Printf("  루트: AllocatedBytes=%d, FreeBytes=%d\n",
		root.AllocatedBytes(), root.FreeBytes())

	_ = rootR1
	_ = rootR2

	// 자식 할당기 생성
	child := NewAllocator(root)

	// 자식이 루트에서 빌려옴
	childR1 := child.Allocate(500)
	fmt.Printf("  자식 Allocate(500) → cap=%d\n", childR1.Cap())
	fmt.Printf("  루트 FreeBytes: %d (하나를 자식에게 빌려줌)\n", root.FreeBytes())

	// 자식이 반환
	child.Free()
	fmt.Printf("  자식 Free() 후:\n")
	fmt.Printf("    자식: AllocatedBytes=%d, FreeBytes=%d\n",
		child.AllocatedBytes(), child.FreeBytes())
	fmt.Printf("    루트: AllocatedBytes=%d, FreeBytes=%d\n",
		root.AllocatedBytes(), root.FreeBytes())
	fmt.Println()

	// === 4. 64바이트 정렬 데모 ===
	fmt.Println("━━━ 4. 64바이트 메모리 정렬 ━━━")
	testSizes := []int{1, 32, 63, 64, 65, 100, 128, 200, 256, 1000}
	fmt.Printf("  %-10s → %-10s\n", "요청", "정렬됨")
	fmt.Println("  " + strings.Repeat("─", 25))
	for _, size := range testSizes {
		aligned := align64(size)
		fmt.Printf("  %-10d → %-10d", size, aligned)
		if aligned%64 == 0 {
			fmt.Printf(" (64의 배수)")
		}
		fmt.Println()
	}
	fmt.Println()

	// === 5. 동시성 안전성 데모 ===
	fmt.Println("━━━ 5. 동시성 안전성 (CAS 기반) ━━━")
	safeAlloc := NewAllocator(nil)

	// 각 goroutine이 자체 할당기를 사용하는 올바른 패턴
	var wg sync.WaitGroup
	results := make([]int, 4)

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			// 각 goroutine은 자체 자식 할당기 사용
			localAlloc := NewAllocator(safeAlloc)
			region := localAlloc.Allocate(64 * (id + 1))
			results[id] = region.Cap()
			localAlloc.Free()
		}(i)
	}
	wg.Wait()

	fmt.Printf("  4개 goroutine이 각각 자체 할당기 사용:\n")
	for i, cap := range results {
		fmt.Printf("    goroutine %d: cap=%d\n", i, cap)
	}
	fmt.Println()

	// === 6. 트레이싱 설정 ===
	fmt.Println("━━━ 6. 트레이싱 설정 & KeyValue 변환 ━━━")
	cfg := TracingConfig{Enabled: true}
	fmt.Printf("  트레이싱 활성화: %v\n", cfg.Enabled)

	// Go Kit 스타일 → OTel KeyValue 변환
	attrs := KeyValuesToAttributes(
		"method", "GET",
		"path", "/loki/api/v1/query",
		"status", 200,
		"duration_ms", 45.2,
		42, "non-string-key", // 비정상 키 처리
	)

	fmt.Printf("  KeyValue 변환 결과 (%d개):\n", len(attrs))
	for _, attr := range attrs {
		fmt.Printf("    %s = %v\n", attr.Key, attr.Value)
	}
	fmt.Println()

	// === 7. Allocator 수명 주기 비교 ===
	fmt.Println("━━━ 7. Reset vs Free 비교 ━━━")

	// Reset 시나리오 (반복 사용 최적)
	resetAlloc := NewAllocator(nil)
	resetAlloc.Allocate(128) // 사용
	resetAlloc.Allocate(256) // 사용

	fmt.Printf("  [Reset 전] AllocatedBytes=%d, FreeBytes=%d\n",
		resetAlloc.AllocatedBytes(), resetAlloc.FreeBytes())

	resetAlloc.Reset() // Trim(미사용 해제) + Reclaim(모두 가용)
	fmt.Printf("  [Reset 후] AllocatedBytes=%d, FreeBytes=%d\n",
		resetAlloc.AllocatedBytes(), resetAlloc.FreeBytes())

	// Free 시나리오 (완전 정리)
	freeAlloc := NewAllocator(nil)
	freeAlloc.Allocate(128)
	freeAlloc.Allocate(256)

	fmt.Printf("  [Free 전]  AllocatedBytes=%d, FreeBytes=%d\n",
		freeAlloc.AllocatedBytes(), freeAlloc.FreeBytes())

	freeAlloc.Free() // Reclaim(모두 가용) + Trim(모두 해제)
	fmt.Printf("  [Free 후]  AllocatedBytes=%d, FreeBytes=%d\n",
		freeAlloc.AllocatedBytes(), freeAlloc.FreeBytes())

	fmt.Println()

	// === 8. 메모리 사용 통계 ===
	fmt.Println("━━━ 8. 메모리 사용 통계 ━━━")
	statsAlloc := NewAllocator(nil)

	for i := 0; i < 5; i++ {
		statsAlloc.Allocate(100 * (i + 1))
	}

	total := statsAlloc.AllocatedBytes()
	free := statsAlloc.FreeBytes()
	used := total - free
	utilization := float64(used) / float64(total) * 100

	fmt.Printf("  총 소유 메모리: %d 바이트\n", total)
	fmt.Printf("  사용 중: %d 바이트\n", used)
	fmt.Printf("  재사용 가능: %d 바이트\n", free)
	fmt.Printf("  활용률: %.1f%%\n", utilization)

	// Reclaim 후 통계 변화
	statsAlloc.Reclaim()
	free = statsAlloc.FreeBytes()
	fmt.Printf("  Reclaim 후 재사용 가능: %d 바이트 (100%%)\n", free)

	fmt.Println()
	fmt.Println("시뮬레이션 완료.")
}
