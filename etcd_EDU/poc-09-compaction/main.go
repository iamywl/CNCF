// poc-09-compaction: etcd MVCC 컴팩션 시뮬레이션
//
// etcd는 모든 키 변경을 리비전 기반으로 저장하여 히스토리 조회를 지원한다.
// 하지만 리비전이 계속 쌓이면 저장 공간이 고갈되므로, Compact(rev) 연산으로
// 특정 리비전 이하의 오래된 버전을 제거한다.
//
// 실제 etcd 구현 (server/storage/mvcc/kvstore_compaction.go):
// - scheduleCompaction()이 배치 단위로 old revision을 삭제
// - keyIndex.doCompact()가 generation별로 보존할 revision을 결정
// - CompactionBatchLimit 개씩 처리하며, 사이에 sleep으로 부하 분산
// - 컴팩션 후 해당 revision 이하 조회 시 ErrCompacted 반환
//
// 이 PoC는 keyIndex의 generation 기반 컴팩션과 배치 처리를 재현한다.
//
// 사용법: go run main.go

package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ===== 에러 정의 =====

var (
	ErrCompacted    = errors.New("mvcc: 요청한 리비전이 이미 컴팩션됨")
	ErrFutureRev    = errors.New("mvcc: 요청한 리비전이 아직 존재하지 않음")
	ErrRevNotFound  = errors.New("mvcc: 리비전을 찾을 수 없음")
	ErrKeyNotFound  = errors.New("mvcc: 키를 찾을 수 없음")
	ErrKeyDeleted   = errors.New("mvcc: 키가 삭제됨")
)

// ===== Revision =====
// etcd의 Revision은 Main(전역 트랜잭션 ID)과 Sub(트랜잭션 내 순번)로 구성된다.
// server/storage/mvcc/revision.go 참조

type Revision struct {
	Main int64 // 전역 리비전 번호
	Sub  int64 // 트랜잭션 내 서브 리비전
}

func (r Revision) GreaterThan(other Revision) bool {
	if r.Main != other.Main {
		return r.Main > other.Main
	}
	return r.Sub > other.Sub
}

func (r Revision) String() string {
	return fmt.Sprintf("%d.%d", r.Main, r.Sub)
}

// ===== Generation =====
// keyIndex는 generation 목록을 유지한다. 키가 삭제(tombstone)되면 현재 generation이 종료되고
// 새 빈 generation이 생성된다. 이 구조가 컴팩션의 핵심이다.
// server/storage/mvcc/key_index.go 참조

type generation struct {
	created Revision   // 이 generation에서 키가 처음 생성된 리비전
	ver     int64      // 버전 카운터
	revs    []Revision // 이 generation의 리비전 목록
}

func (g *generation) isEmpty() bool {
	return g == nil || len(g.revs) == 0
}

// walk는 revs를 역순으로 순회하며 조건 함수가 false를 반환하는 지점의 인덱스를 반환한다.
func (g *generation) walk(f func(rev Revision) bool) int {
	l := len(g.revs)
	for i := l - 1; i >= 0; i-- {
		if !f(g.revs[i]) {
			return i
		}
	}
	return -1
}

// ===== KeyIndex =====
// 하나의 키에 대한 모든 리비전 히스토리를 관리한다.
// server/storage/mvcc/key_index.go의 keyIndex 구조체 재현

type keyIndex struct {
	key         string
	modified    Revision
	generations []generation
}

func newKeyIndex(key string) *keyIndex {
	return &keyIndex{
		key:         key,
		generations: []generation{{}},
	}
}

// put은 새 리비전을 현재 generation에 추가한다.
func (ki *keyIndex) put(main, sub int64) {
	rev := Revision{Main: main, Sub: sub}
	if len(ki.generations) == 0 {
		ki.generations = append(ki.generations, generation{})
	}
	g := &ki.generations[len(ki.generations)-1]
	if len(g.revs) == 0 {
		g.created = rev
	}
	g.revs = append(g.revs, rev)
	g.ver++
	ki.modified = rev
}

// tombstone은 삭제 마커를 추가하고 새 빈 generation을 생성한다.
func (ki *keyIndex) tombstone(main, sub int64) error {
	if len(ki.generations) == 0 || ki.generations[len(ki.generations)-1].isEmpty() {
		return ErrRevNotFound
	}
	ki.put(main, sub)
	ki.generations = append(ki.generations, generation{})
	return nil
}

// get은 atRev 시점에서 키의 값 리비전을 조회한다.
func (ki *keyIndex) get(atRev int64) (modified, created Revision, ver int64, err error) {
	g := ki.findGeneration(atRev)
	if g == nil || g.isEmpty() {
		return Revision{}, Revision{}, 0, ErrRevNotFound
	}
	n := g.walk(func(rev Revision) bool { return rev.Main > atRev })
	if n != -1 {
		return g.revs[n], g.created, g.ver - int64(len(g.revs)-n-1), nil
	}
	return Revision{}, Revision{}, 0, ErrRevNotFound
}

// findGeneration은 주어진 rev가 속하는 generation을 찾는다.
func (ki *keyIndex) findGeneration(rev int64) *generation {
	for i := len(ki.generations) - 1; i >= 0; i-- {
		g := &ki.generations[i]
		if g.isEmpty() {
			continue
		}
		// generation의 첫 리비전이 rev보다 크면 이전 generation 탐색
		if g.revs[0].Main > rev {
			continue
		}
		return g
	}
	return nil
}

// compact는 atRev 이하의 오래된 리비전을 제거하되, 각 generation에서 가장 큰 리비전은 보존한다.
// etcd의 keyIndex.compact() + doCompact() 로직을 재현한다.
func (ki *keyIndex) compact(atRev int64, available map[Revision]struct{}) {
	if len(ki.generations) == 0 {
		return
	}

	genIdx, revIndex := ki.doCompact(atRev, available)

	g := &ki.generations[genIdx]
	if !g.isEmpty() {
		if revIndex != -1 {
			g.revs = g.revs[revIndex:]
		}
	}
	// 이전 generation들을 제거
	ki.generations = ki.generations[genIdx:]
}

// doCompact는 컴팩션의 핵심 로직이다.
// atRev 이하에서 보존할 revision을 available 맵에 추가하고, 해당 위치를 반환한다.
func (ki *keyIndex) doCompact(atRev int64, available map[Revision]struct{}) (genIdx int, revIndex int) {
	f := func(rev Revision) bool {
		if rev.Main <= atRev {
			available[rev] = struct{}{}
			return false
		}
		return true
	}

	genIdx = 0
	g := &ki.generations[0]
	// atRev를 포함하거나 이후에 생성된 첫 번째 generation을 찾는다
	for genIdx < len(ki.generations)-1 {
		if len(g.revs) > 0 {
			tomb := g.revs[len(g.revs)-1].Main
			if tomb >= atRev {
				break
			}
		}
		genIdx++
		g = &ki.generations[genIdx]
	}

	revIndex = g.walk(f)
	return genIdx, revIndex
}

func (ki *keyIndex) isEmpty() bool {
	return len(ki.generations) == 1 && ki.generations[0].isEmpty()
}

// ===== KeyValue =====

type KeyValue struct {
	Key            string
	Value          string
	CreateRevision int64
	ModRevision    int64
	Version        int64
}

// ===== MVCCStore =====
// MVCC 스토어: 리비전 기반 키-값 저장소 + 컴팩션

type MVCCStore struct {
	mu             sync.RWMutex
	currentRev     int64
	compactMainRev int64
	index          map[string]*keyIndex      // 키 → keyIndex
	store          map[Revision]*KeyValue     // 리비전 → KV 데이터
	batchLimit     int                        // 배치 처리 단위
}

func NewMVCCStore(batchLimit int) *MVCCStore {
	return &MVCCStore{
		currentRev: 0,
		index:      make(map[string]*keyIndex),
		store:      make(map[Revision]*KeyValue),
		batchLimit: batchLimit,
	}
}

// Put은 키-값을 저장하고 새 리비전을 생성한다.
func (s *MVCCStore) Put(key, value string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.currentRev++
	rev := Revision{Main: s.currentRev, Sub: 0}

	ki, ok := s.index[key]
	if !ok {
		ki = newKeyIndex(key)
		s.index[key] = ki
	}

	ki.put(rev.Main, rev.Sub)

	var createRev int64
	var ver int64
	// generation에서 created 리비전 추출
	if g := &ki.generations[len(ki.generations)-1]; !g.isEmpty() {
		createRev = g.created.Main
		ver = g.ver
	}

	s.store[rev] = &KeyValue{
		Key:            key,
		Value:          value,
		CreateRevision: createRev,
		ModRevision:    rev.Main,
		Version:        ver,
	}

	return s.currentRev
}

// Delete는 키를 삭제(tombstone)한다.
func (s *MVCCStore) Delete(key string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ki, ok := s.index[key]
	if !ok {
		return 0, ErrKeyNotFound
	}

	s.currentRev++
	rev := Revision{Main: s.currentRev, Sub: 0}

	if err := ki.tombstone(rev.Main, rev.Sub); err != nil {
		return 0, ErrKeyDeleted
	}

	// tombstone 리비전도 store에 기록 (value 없음)
	s.store[rev] = &KeyValue{
		Key:         key,
		Value:       "", // 삭제 마커
		ModRevision: rev.Main,
	}

	return s.currentRev, nil
}

// Get은 지정 리비전에서 키를 조회한다.
func (s *MVCCStore) Get(key string, atRev int64) (*KeyValue, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// 컴팩션된 리비전 요청 시 ErrCompacted 반환
	if atRev > 0 && atRev <= s.compactMainRev {
		return nil, ErrCompacted
	}

	if atRev == 0 {
		atRev = s.currentRev
	}

	ki, ok := s.index[key]
	if !ok {
		return nil, ErrKeyNotFound
	}

	modified, _, _, err := ki.get(atRev)
	if err != nil {
		return nil, err
	}

	kv, ok := s.store[modified]
	if !ok {
		return nil, ErrRevNotFound
	}

	if kv.Value == "" {
		return nil, ErrKeyDeleted
	}

	return kv, nil
}

// Compact는 지정 리비전 이하의 오래된 데이터를 제거한다.
// etcd의 store.Compact() → scheduleCompaction() 흐름을 재현한다.
func (s *MVCCStore) Compact(rev int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 유효성 검증
	if rev <= s.compactMainRev {
		return ErrCompacted
	}
	if rev > s.currentRev {
		return ErrFutureRev
	}

	prevCompactRev := s.compactMainRev
	s.compactMainRev = rev

	fmt.Printf("\n=== 컴팩션 시작: rev %d → %d ===\n", prevCompactRev, rev)

	// 1단계: 인덱스 컴팩션 — 보존할 리비전 결정
	// etcd의 treeIndex.Compact()에 해당
	keep := make(map[Revision]struct{})
	toRemove := make([]string, 0)

	for key, ki := range s.index {
		ki.compact(rev, keep)
		if ki.isEmpty() {
			toRemove = append(toRemove, key)
		}
	}

	// 빈 keyIndex 제거
	for _, key := range toRemove {
		delete(s.index, key)
	}

	fmt.Printf("  인덱스 컴팩션 완료: 보존할 리비전 %d개\n", len(keep))

	// 2단계: 스토어 컴팩션 — 배치 단위로 old revision 삭제
	// etcd의 scheduleCompaction()에 해당: batchLimit 개씩 처리
	deletedCount := 0
	batchCount := 0

	for storeRev := range s.store {
		if storeRev.Main > rev {
			continue // 컴팩션 대상 아님
		}
		if _, ok := keep[storeRev]; ok {
			continue // 보존 대상
		}

		delete(s.store, storeRev)
		deletedCount++
		batchCount++

		// 배치 단위 처리 시뮬레이션
		if batchCount >= s.batchLimit {
			fmt.Printf("  배치 처리: %d개 리비전 삭제 (배치 %d)\n",
				batchCount, deletedCount/s.batchLimit)
			batchCount = 0
			// etcd는 여기서 ForceCommit() + sleep으로 부하를 분산한다
			time.Sleep(1 * time.Millisecond)
		}
	}

	if batchCount > 0 {
		fmt.Printf("  마지막 배치: %d개 리비전 삭제\n", batchCount)
	}

	fmt.Printf("=== 컴팩션 완료: 총 %d개 리비전 삭제 ===\n", deletedCount)

	return nil
}

// CurrentRev는 현재 리비전을 반환한다.
func (s *MVCCStore) CurrentRev() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.currentRev
}

// CompactRev는 컴팩션된 리비전을 반환한다.
func (s *MVCCStore) CompactRev() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.compactMainRev
}

// StoreSize는 저장된 리비전 수를 반환한다.
func (s *MVCCStore) StoreSize() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.store)
}

// DumpRevisions는 모든 저장된 리비전을 출력한다 (디버그용).
func (s *MVCCStore) DumpRevisions() {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fmt.Printf("\n  저장된 리비전 목록 (총 %d개):\n", len(s.store))
	for rev, kv := range s.store {
		marker := ""
		if kv.Value == "" {
			marker = " [삭제됨]"
		}
		fmt.Printf("    rev=%s  key=%s  value=%q%s\n", rev, kv.Key, kv.Value, marker)
	}
}

// ===== 메인 =====

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════╗")
	fmt.Println("║  etcd 컴팩션(Compaction) PoC                            ║")
	fmt.Println("║  MVCC 리비전 히스토리 압축 시뮬레이션                   ║")
	fmt.Println("╚══════════════════════════════════════════════════════════╝")

	// 배치 제한: 3개씩 처리 (실제 etcd 기본값은 1000)
	store := NewMVCCStore(3)

	// ========================================
	// 1. 리비전 생성
	// ========================================
	fmt.Println("\n" + strings.Repeat("─", 50))
	fmt.Println("1단계: 리비전 10개 생성")
	fmt.Println(strings.Repeat("─", 50))

	// 키 "foo"에 5번 업데이트 (rev 1~5)
	for i := 1; i <= 5; i++ {
		rev := store.Put("foo", fmt.Sprintf("foo-v%d", i))
		fmt.Printf("  PUT foo=foo-v%d → rev=%d\n", i, rev)
	}

	// 키 "bar"에 3번 업데이트 (rev 6~8)
	for i := 1; i <= 3; i++ {
		rev := store.Put("bar", fmt.Sprintf("bar-v%d", i))
		fmt.Printf("  PUT bar=bar-v%d → rev=%d\n", i, rev)
	}

	// 키 "baz" 생성 후 삭제 (rev 9~10)
	rev := store.Put("baz", "baz-v1")
	fmt.Printf("  PUT baz=baz-v1 → rev=%d\n", rev)
	rev2, _ := store.Delete("baz")
	fmt.Printf("  DEL baz → rev=%d (tombstone)\n", rev2)

	fmt.Printf("\n  현재 리비전: %d, 저장소 크기: %d\n", store.CurrentRev(), store.StoreSize())
	store.DumpRevisions()

	// ========================================
	// 2. 컴팩션 전 히스토리 조회
	// ========================================
	fmt.Println("\n" + strings.Repeat("─", 50))
	fmt.Println("2단계: 컴팩션 전 히스토리 조회")
	fmt.Println(strings.Repeat("─", 50))

	for _, atRev := range []int64{1, 3, 5} {
		kv, err := store.Get("foo", atRev)
		if err != nil {
			fmt.Printf("  GET foo @rev=%d → 에러: %v\n", atRev, err)
		} else {
			fmt.Printf("  GET foo @rev=%d → value=%q (ver=%d)\n", atRev, kv.Value, kv.Version)
		}
	}

	// ========================================
	// 3. Compact(5) 실행
	// ========================================
	fmt.Println("\n" + strings.Repeat("─", 50))
	fmt.Println("3단계: Compact(5) — 리비전 5 이하 압축")
	fmt.Println(strings.Repeat("─", 50))

	if err := store.Compact(5); err != nil {
		fmt.Printf("  컴팩션 실패: %v\n", err)
	}

	fmt.Printf("\n  컴팩션 후 저장소 크기: %d (컴팩션 리비전: %d)\n",
		store.StoreSize(), store.CompactRev())
	store.DumpRevisions()

	// ========================================
	// 4. 컴팩션 후 조회 — ErrCompacted
	// ========================================
	fmt.Println("\n" + strings.Repeat("─", 50))
	fmt.Println("4단계: 컴팩션 후 조회 결과")
	fmt.Println(strings.Repeat("─", 50))

	// 컴팩션된 리비전 조회 → ErrCompacted
	for _, atRev := range []int64{1, 3, 5} {
		_, err := store.Get("foo", atRev)
		fmt.Printf("  GET foo @rev=%d → %v\n", atRev, err)
	}

	// 최신 리비전 조회 → 정상
	kv, err := store.Get("foo", 0)
	if err != nil {
		fmt.Printf("  GET foo @latest → 에러: %v\n", err)
	} else {
		fmt.Printf("  GET foo @latest → value=%q (최신 버전 보존)\n", kv.Value)
	}

	// bar는 rev 6~8이므로 rev 5 이후 데이터 보존
	kv, err = store.Get("bar", 0)
	if err != nil {
		fmt.Printf("  GET bar @latest → 에러: %v\n", err)
	} else {
		fmt.Printf("  GET bar @latest → value=%q (최신 버전 보존)\n", kv.Value)
	}

	// ========================================
	// 5. 추가 컴팩션: Compact(8)
	// ========================================
	fmt.Println("\n" + strings.Repeat("─", 50))
	fmt.Println("5단계: Compact(8) — 리비전 8 이하 추가 압축")
	fmt.Println(strings.Repeat("─", 50))

	if err := store.Compact(8); err != nil {
		fmt.Printf("  컴팩션 실패: %v\n", err)
	}

	fmt.Printf("\n  컴팩션 후 저장소 크기: %d\n", store.StoreSize())
	store.DumpRevisions()

	// 삭제된 "baz"는 컴팩션 후에도 tombstone이 남아있을 수 있다
	_, err = store.Get("baz", 0)
	fmt.Printf("  GET baz @latest → %v (삭제+컴팩션)\n", err)

	// ========================================
	// 6. 에러 케이스
	// ========================================
	fmt.Println("\n" + strings.Repeat("─", 50))
	fmt.Println("6단계: 에러 케이스")
	fmt.Println(strings.Repeat("─", 50))

	// 이미 컴팩션된 리비전으로 재컴팩션
	err = store.Compact(3)
	fmt.Printf("  Compact(3) [이미 컴팩션됨] → %v\n", err)

	// 미래 리비전으로 컴팩션
	err = store.Compact(100)
	fmt.Printf("  Compact(100) [미래 리비전] → %v\n", err)

	// ========================================
	// 7. Generation 동작 확인
	// ========================================
	fmt.Println("\n" + strings.Repeat("─", 50))
	fmt.Println("7단계: Generation 기반 컴팩션 동작")
	fmt.Println(strings.Repeat("─", 50))
	fmt.Println("  etcd의 keyIndex는 generation으로 키 수명을 관리한다:")
	fmt.Println("  - 키 생성 → generation 시작")
	fmt.Println("  - 키 삭제(tombstone) → generation 종료 + 새 빈 generation")
	fmt.Println("  - 컴팩션 시 빈 generation과 오래된 리비전 제거")
	fmt.Println()

	store2 := NewMVCCStore(100)
	// put(1) → put(2) → delete(3) → put(4) → put(5)
	store2.Put("key1", "v1")  // rev 1, gen[0]
	store2.Put("key1", "v2")  // rev 2, gen[0]
	store2.Delete("key1")     // rev 3, gen[0] 종료 → gen[1] 시작
	store2.Put("key1", "v3")  // rev 4, gen[1]
	store2.Put("key1", "v4")  // rev 5, gen[1]

	fmt.Println("  key1 히스토리: put(1) → put(2) → delete(3) → put(4) → put(5)")

	// compact(2): gen[0]에서 rev 1 제거, rev 2 보존
	store2.Compact(2)
	kv, _ = store2.Get("key1", 0)
	fmt.Printf("  compact(2) 후 GET key1 @latest → %q\n", kv.Value)

	// compact(4): gen[0] 전체 제거 (tombstone 포함), gen[1]에서 rev 4 보존
	store2.Compact(4)
	kv, _ = store2.Get("key1", 0)
	fmt.Printf("  compact(4) 후 GET key1 @latest → %q\n", kv.Value)

	fmt.Println("\n" + strings.Repeat("─", 50))
	fmt.Println("✓ 컴팩션 PoC 완료")
	fmt.Println("  - 리비전 기반 히스토리 압축 동작 확인")
	fmt.Println("  - 배치 처리 시뮬레이션 확인")
	fmt.Println("  - ErrCompacted 에러 처리 확인")
	fmt.Println("  - Generation 기반 keyIndex 컴팩션 확인")
}
