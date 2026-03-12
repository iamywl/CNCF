// poc-02-btree-index: etcd B-tree 인덱스 시뮬레이션
//
// etcd의 treeIndex는 키에서 리비전으로의 매핑을 B-tree로 관리한다.
// 각 키는 keyIndex 구조체로 표현되며, 키의 생명주기를 generation 단위로 추적한다.
//
// 실제 구현 참조:
//   - server/storage/mvcc/key_index.go: keyIndex, generation 구조체
//   - server/storage/mvcc/index.go: treeIndex (google/btree 기반)
//
// 이 PoC는 정렬된 맵(슬라이스 기반)으로 B-tree를 시뮬레이션하고,
// keyIndex/generation의 핵심 알고리즘을 충실히 재현한다.
package main

import (
	"fmt"
	"sort"
	"strings"
)

// ============================================================
// Revision: etcd의 server/storage/mvcc/revision.go 기반
// ============================================================

type Revision struct {
	Main int64
	Sub  int64
}

func (a Revision) GreaterThan(b Revision) bool {
	if a.Main > b.Main {
		return true
	}
	if a.Main < b.Main {
		return false
	}
	return a.Sub > b.Sub
}

func (r Revision) String() string {
	return fmt.Sprintf("%d.%d", r.Main, r.Sub)
}

// ============================================================
// generation: 키의 한 생명주기
// etcd 소스: key_index.go → type generation struct
//
// 키가 생성(put)되면 generation이 시작되고, 삭제(tombstone)되면 종료된다.
// 삭제 후 같은 키를 다시 생성하면 새 generation이 시작된다.
//
// 예: put(1.0);put(2.0);tombstone(3.0);put(4.0);tombstone(5.0)
//   generation[0]: {1.0, 2.0, 3.0(t)} ← 첫 번째 생명주기
//   generation[1]: {4.0, 5.0(t)}       ← 두 번째 생명주기
//   generation[2]: {empty}              ← 현재 (빈 세대 = 삭제 상태)
// ============================================================

type generation struct {
	ver     int64      // 이 세대에서의 버전 (수정 횟수)
	created Revision   // 세대가 생성된 리비전 (첫 번째 put)
	revs    []Revision // 이 세대의 모든 리비전 목록
}

// isEmpty: 빈 세대인지 확인
// etcd 소스: func (g *generation) isEmpty() bool { return g == nil || len(g.revs) == 0 }
func (g *generation) isEmpty() bool {
	return g == nil || len(g.revs) == 0
}

// walk: 리비전을 역순으로 순회하며 조건 함수가 false를 반환하면 멈춤
// etcd 소스: func (g *generation) walk(f func(rev Revision) bool) int
// 반환값: 멈춘 위치 인덱스, 전부 순회하면 -1
func (g *generation) walk(f func(rev Revision) bool) int {
	l := len(g.revs)
	for i := range g.revs {
		ok := f(g.revs[l-i-1]) // 역순 순회
		if !ok {
			return l - i - 1
		}
	}
	return -1
}

func (g *generation) String() string {
	revStrs := make([]string, len(g.revs))
	for i, r := range g.revs {
		revStrs[i] = r.String()
	}
	return fmt.Sprintf("  세대: 생성=%s, 버전=%d, 리비전=[%s]",
		g.created, g.ver, strings.Join(revStrs, ", "))
}

// ============================================================
// keyIndex: 하나의 키에 대한 모든 리비전 이력
// etcd 소스: key_index.go → type keyIndex struct
//
// keyIndex는 generations 슬라이스를 관리한다.
// 마지막 generation이 비어있으면(isEmpty) 현재 키가 삭제된 상태.
// ============================================================

type keyIndex struct {
	key         string
	modified    Revision     // 마지막 수정 리비전
	generations []generation // 키의 모든 세대
}

// newKeyIndex: 새 keyIndex를 생성한다.
func newKeyIndex(key string) *keyIndex {
	return &keyIndex{
		key:         key,
		generations: []generation{{}}, // 빈 세대 하나로 시작
	}
}

// put: 새 리비전을 현재 세대에 추가한다.
// etcd 소스: func (ki *keyIndex) put(lg *zap.Logger, main int64, sub int64)
//
// 핵심 로직:
// 1. 현재 세대가 비어있으면 → 새 키 생성 (created 설정)
// 2. 리비전을 revs에 추가, ver++
// 3. modified를 새 리비전으로 업데이트
func (ki *keyIndex) put(main, sub int64) {
	rev := Revision{Main: main, Sub: sub}

	if len(ki.generations) == 0 {
		ki.generations = append(ki.generations, generation{})
	}
	g := &ki.generations[len(ki.generations)-1]
	if len(g.revs) == 0 {
		// 새 키 생성 (etcd: keysGauge.Inc())
		g.created = rev
	}
	g.revs = append(g.revs, rev)
	g.ver++
	ki.modified = rev
}

// tombstone: 현재 세대를 종료하고 새 빈 세대를 추가한다.
// etcd 소스: func (ki *keyIndex) tombstone(lg *zap.Logger, main int64, sub int64) error
//
// 핵심 로직:
// 1. 현재 세대가 비어있으면 에러 (이미 삭제된 상태)
// 2. put으로 톰스톤 리비전 추가
// 3. 새 빈 세대 추가 (다음 put을 위한 준비)
func (ki *keyIndex) tombstone(main, sub int64) error {
	if ki.isEmpty() {
		return fmt.Errorf("keyIndex가 비어있음")
	}
	if ki.generations[len(ki.generations)-1].isEmpty() {
		return fmt.Errorf("이미 삭제된 키")
	}
	ki.put(main, sub)
	ki.generations = append(ki.generations, generation{})
	return nil
}

// get: 지정된 리비전(atRev) 시점의 수정 리비전, 생성 리비전, 버전을 반환한다.
// etcd 소스: func (ki *keyIndex) get(lg *zap.Logger, atRev int64) (modified, created Revision, ver int64, err error)
//
// 핵심 알고리즘:
// 1. findGeneration으로 해당 리비전이 속하는 세대를 찾음
// 2. 세대 내에서 walk하며 atRev 이하의 가장 최신 리비전 반환
func (ki *keyIndex) get(atRev int64) (modified, created Revision, ver int64, err error) {
	if ki.isEmpty() {
		return Revision{}, Revision{}, 0, fmt.Errorf("keyIndex가 비어있음")
	}
	g := ki.findGeneration(atRev)
	if g.isEmpty() {
		return Revision{}, Revision{}, 0, fmt.Errorf("리비전을 찾을 수 없음")
	}

	// walk: atRev보다 큰 리비전은 건너뛰고, 이하인 첫 리비전에서 멈춤
	n := g.walk(func(rev Revision) bool { return rev.Main > atRev })
	if n != -1 {
		return g.revs[n], g.created, g.ver - int64(len(g.revs)-n-1), nil
	}

	return Revision{}, Revision{}, 0, fmt.Errorf("리비전을 찾을 수 없음")
}

// since: 지정된 리비전 이후의 모든 리비전을 반환한다.
// etcd 소스: func (ki *keyIndex) since(lg *zap.Logger, rev int64) []Revision
// Watch에서 과거 이벤트를 따라잡을 때 사용
func (ki *keyIndex) since(rev int64) []Revision {
	if ki.isEmpty() {
		return nil
	}
	since := Revision{Main: rev}
	var gi int
	// 시작할 세대 찾기
	for gi = len(ki.generations) - 1; gi > 0; gi-- {
		g := ki.generations[gi]
		if g.isEmpty() {
			continue
		}
		if since.GreaterThan(g.created) {
			break
		}
	}

	var revs []Revision
	var last int64
	for ; gi < len(ki.generations); gi++ {
		for _, r := range ki.generations[gi].revs {
			if since.GreaterThan(r) {
				continue
			}
			if r.Main == last {
				// 같은 Main 리비전은 마지막 Sub만 유지
				revs[len(revs)-1] = r
				continue
			}
			revs = append(revs, r)
			last = r.Main
		}
	}
	return revs
}

// findGeneration: 주어진 리비전이 속하는 세대를 찾는다.
// etcd 소스: func (ki *keyIndex) findGeneration(rev int64) *generation
//
// 핵심 로직:
// 1. 마지막 세대부터 역순 탐색
// 2. 현재 세대가 아닌 경우: 톰스톤 리비전이 rev 이하면 → nil (세대 간 갭)
// 3. 첫 리비전이 rev 이하면 → 해당 세대 반환
func (ki *keyIndex) findGeneration(rev int64) *generation {
	lastg := len(ki.generations) - 1
	cg := lastg

	for cg >= 0 {
		if len(ki.generations[cg].revs) == 0 {
			cg--
			continue
		}
		g := ki.generations[cg]
		if cg != lastg {
			// 이전 세대: 톰스톤(마지막 리비전)이 rev 이하면 해당 시점에서 키가 삭제된 상태
			if tomb := g.revs[len(g.revs)-1].Main; tomb <= rev {
				return nil
			}
		}
		if g.revs[0].Main <= rev {
			return &ki.generations[cg]
		}
		cg--
	}
	return nil
}

// isEmpty: 세대가 하나이고 비어있으면 true
func (ki *keyIndex) isEmpty() bool {
	return len(ki.generations) == 1 && ki.generations[0].isEmpty()
}

// compact: 지정된 리비전 이하의 오래된 리비전을 정리한다.
// etcd 소스: func (ki *keyIndex) compact(lg *zap.Logger, atRev int64, available map[Revision]struct{})
func (ki *keyIndex) compact(atRev int64) []Revision {
	if ki.isEmpty() {
		return nil
	}

	var kept []Revision

	// 각 세대에서 atRev 이하의 리비전 중 마지막 하나만 유지
	genIdx := 0
	for genIdx < len(ki.generations)-1 {
		g := ki.generations[genIdx]
		if len(g.revs) == 0 {
			genIdx++
			continue
		}
		tomb := g.revs[len(g.revs)-1].Main
		if tomb >= atRev {
			break
		}
		genIdx++
	}

	g := &ki.generations[genIdx]
	if !g.isEmpty() {
		// atRev 이하에서 가장 큰 리비전을 찾아 유지
		n := g.walk(func(rev Revision) bool {
			if rev.Main <= atRev {
				kept = append(kept, rev)
				return false
			}
			return true
		})
		if n != -1 {
			g.revs = g.revs[n:]
		}
	}

	// 이전 세대 제거
	ki.generations = ki.generations[genIdx:]
	return kept
}

// ============================================================
// TreeIndex: 정렬된 슬라이스로 B-tree를 시뮬레이션
// etcd 소스: index.go → type treeIndex struct (google/btree.BTreeG 기반)
//
// 실제 etcd는 google/btree 라이브러리를 사용하지만,
// 여기서는 정렬된 슬라이스 + 이진 탐색으로 동일한 O(log n) 검색을 구현한다.
// ============================================================

type TreeIndex struct {
	items []*keyIndex // 키 이름 순으로 정렬
}

func NewTreeIndex() *TreeIndex {
	return &TreeIndex{}
}

// findPos: 이진 탐색으로 키의 위치를 찾는다.
func (ti *TreeIndex) findPos(key string) (int, bool) {
	pos := sort.Search(len(ti.items), func(i int) bool {
		return ti.items[i].key >= key
	})
	if pos < len(ti.items) && ti.items[pos].key == key {
		return pos, true
	}
	return pos, false
}

// getOrCreate: 키의 keyIndex를 찾거나 새로 생성한다.
func (ti *TreeIndex) getOrCreate(key string) *keyIndex {
	pos, found := ti.findPos(key)
	if found {
		return ti.items[pos]
	}
	ki := newKeyIndex(key)
	// 정렬 순서를 유지하며 삽입
	ti.items = append(ti.items, nil)
	copy(ti.items[pos+1:], ti.items[pos:])
	ti.items[pos] = ki
	return ki
}

// Put: 키에 새 리비전을 추가한다.
// etcd 소스: func (ti *treeIndex) Put(key []byte, rev Revision)
func (ti *TreeIndex) Put(key string, rev Revision) {
	ki := ti.getOrCreate(key)
	ki.put(rev.Main, rev.Sub)
}

// Get: 지정된 리비전 시점의 키 정보를 반환한다.
// etcd 소스: func (ti *treeIndex) Get(key []byte, atRev int64) (modified, created Revision, ver int64, err error)
func (ti *TreeIndex) Get(key string, atRev int64) (modified, created Revision, ver int64, err error) {
	pos, found := ti.findPos(key)
	if !found {
		return Revision{}, Revision{}, 0, fmt.Errorf("키 '%s'를 찾을 수 없음", key)
	}
	return ti.items[pos].get(atRev)
}

// Tombstone: 키를 삭제 처리한다.
func (ti *TreeIndex) Tombstone(key string, rev Revision) error {
	pos, found := ti.findPos(key)
	if !found {
		return fmt.Errorf("키 '%s'를 찾을 수 없음", key)
	}
	return ti.items[pos].tombstone(rev.Main, rev.Sub)
}

// Range: 키 범위 [startKey, endKey)에 해당하는 키의 최신 리비전을 반환한다.
// etcd 소스: func (ti *treeIndex) Range(key, end []byte, atRev int64) (keys [][]byte, revs []Revision)
func (ti *TreeIndex) Range(startKey, endKey string, atRev int64) []RangeResult {
	var results []RangeResult

	for _, ki := range ti.items {
		if ki.key < startKey {
			continue
		}
		if endKey != "" && ki.key >= endKey {
			break
		}
		modified, created, ver, err := ki.get(atRev)
		if err != nil {
			continue // 해당 리비전에서 키가 없음
		}
		results = append(results, RangeResult{
			Key:      ki.key,
			Modified: modified,
			Created:  created,
			Version:  ver,
		})
	}
	return results
}

// Since: 특정 리비전 이후의 모든 변경을 반환한다.
func (ti *TreeIndex) Since(key string, rev int64) []Revision {
	pos, found := ti.findPos(key)
	if !found {
		return nil
	}
	return ti.items[pos].since(rev)
}

// Compact: 지정된 리비전까지 오래된 리비전을 정리한다.
func (ti *TreeIndex) Compact(atRev int64) map[string][]Revision {
	kept := make(map[string][]Revision)
	var toRemove []int

	for i, ki := range ti.items {
		k := ki.compact(atRev)
		if len(k) > 0 {
			kept[ki.key] = k
		}
		if ki.isEmpty() {
			toRemove = append(toRemove, i)
		}
	}

	// 빈 keyIndex 제거 (역순)
	for i := len(toRemove) - 1; i >= 0; i-- {
		idx := toRemove[i]
		ti.items = append(ti.items[:idx], ti.items[idx+1:]...)
	}

	return kept
}

type RangeResult struct {
	Key      string
	Modified Revision
	Created  Revision
	Version  int64
}

// PrintKeyIndex: 키의 내부 구조를 출력한다.
func (ti *TreeIndex) PrintKeyIndex(key string) {
	pos, found := ti.findPos(key)
	if !found {
		fmt.Printf("키 '%s': 없음\n", key)
		return
	}
	ki := ti.items[pos]
	fmt.Printf("키 '%s' (최종수정: %s)\n", ki.key, ki.modified)
	for i, g := range ki.generations {
		if g.isEmpty() {
			fmt.Printf("  세대[%d]: {비어있음}\n", i)
		} else {
			fmt.Printf("  세대[%d]: %s\n", i, g.String())
		}
	}
}

// ============================================================
// 메인: B-tree 인덱스의 핵심 동작을 시연
// ============================================================

func main() {
	fmt.Println("=== etcd B-tree 인덱스 시뮬레이션 ===")
	fmt.Println()

	idx := NewTreeIndex()

	// ----------------------------------------
	// 시나리오 1: 기본 Put/Get
	// etcd 문서의 예제: put(1.0);put(2.0);tombstone(3.0);put(4.0);tombstone(5.0) on key "foo"
	// ----------------------------------------
	fmt.Println("--- 시나리오 1: keyIndex 생명주기 ---")
	fmt.Println("etcd 소스 주석의 예제 재현:")
	fmt.Println("  put(1.0);put(2.0);tombstone(3.0);put(4.0);tombstone(5.0) on key \"foo\"")
	fmt.Println()

	idx.Put("foo", Revision{1, 0})
	idx.Put("foo", Revision{2, 0})
	idx.Tombstone("foo", Revision{3, 0})
	idx.Put("foo", Revision{4, 0})
	idx.Tombstone("foo", Revision{5, 0})

	idx.PrintKeyIndex("foo")
	fmt.Println()

	// ----------------------------------------
	// 시나리오 2: 특정 리비전 시점 조회
	// ----------------------------------------
	fmt.Println("--- 시나리오 2: 리비전별 조회 ---")
	for atRev := int64(1); atRev <= 6; atRev++ {
		modified, created, ver, err := idx.Get("foo", atRev)
		if err != nil {
			fmt.Printf("  Get(\"foo\", atRev=%d) → 에러: %v\n", atRev, err)
		} else {
			fmt.Printf("  Get(\"foo\", atRev=%d) → 수정=%s, 생성=%s, 버전=%d\n",
				atRev, modified, created, ver)
		}
	}
	fmt.Println()

	// ----------------------------------------
	// 시나리오 3: 다중 키 + Range 검색
	// ----------------------------------------
	fmt.Println("--- 시나리오 3: 다중 키 + Range 검색 ---")
	idx2 := NewTreeIndex()

	idx2.Put("app/config", Revision{1, 0})
	idx2.Put("app/name", Revision{2, 0})
	idx2.Put("app/version", Revision{3, 0})
	idx2.Put("db/host", Revision{4, 0})
	idx2.Put("db/port", Revision{5, 0})
	idx2.Put("app/config", Revision{6, 0}) // config 업데이트

	fmt.Println("전체 키 (atRev=6):")
	results := idx2.Range("", "", 6) // 전체 범위
	for _, r := range results {
		fmt.Printf("  %s: 수정=%s, 생성=%s, 버전=%d\n",
			r.Key, r.Modified, r.Created, r.Version)
	}
	fmt.Println()

	fmt.Println("범위 검색 [\"app/\", \"app0\") (atRev=6):")
	results = idx2.Range("app/", "app0", 6) // app/ 접두사 검색
	for _, r := range results {
		fmt.Printf("  %s: 수정=%s, 버전=%d\n", r.Key, r.Modified, r.Version)
	}
	fmt.Println()

	fmt.Println("과거 시점 범위 검색 (atRev=2):")
	results = idx2.Range("", "", 2)
	for _, r := range results {
		fmt.Printf("  %s: 수정=%s\n", r.Key, r.Modified)
	}
	fmt.Println()

	// ----------------------------------------
	// 시나리오 4: since() - 특정 리비전 이후의 변경
	// ----------------------------------------
	fmt.Println("--- 시나리오 4: since() - Watch 지원 ---")
	idx3 := NewTreeIndex()
	idx3.Put("key1", Revision{1, 0})
	idx3.Put("key1", Revision{3, 0})
	idx3.Put("key1", Revision{5, 0})
	idx3.Tombstone("key1", Revision{7, 0})
	idx3.Put("key1", Revision{9, 0})

	fmt.Println("key1의 since(rev=2) - rev 2 이후의 모든 변경:")
	revs := idx3.Since("key1", 2)
	for _, r := range revs {
		fmt.Printf("  리비전: %s\n", r)
	}
	fmt.Println()

	// ----------------------------------------
	// 시나리오 5: Compact (컴팩션)
	// ----------------------------------------
	fmt.Println("--- 시나리오 5: Compact (컴팩션) ---")
	idx4 := NewTreeIndex()
	idx4.Put("foo", Revision{1, 0})
	idx4.Put("foo", Revision{2, 0})
	idx4.Tombstone("foo", Revision{3, 0})
	idx4.Put("foo", Revision{4, 0})
	idx4.Put("foo", Revision{5, 0})

	fmt.Println("컴팩션 전:")
	idx4.PrintKeyIndex("foo")
	fmt.Println()

	kept := idx4.Compact(2)
	fmt.Println("compact(2) 후:")
	idx4.PrintKeyIndex("foo")
	fmt.Printf("  유지된 리비전: %v\n", kept["foo"])
	fmt.Println()

	kept = idx4.Compact(4)
	fmt.Println("compact(4) 후:")
	idx4.PrintKeyIndex("foo")
	fmt.Printf("  유지된 리비전: %v\n", kept["foo"])
	fmt.Println()

	// ----------------------------------------
	// 시나리오 6: findGeneration 동작 상세
	// ----------------------------------------
	fmt.Println("--- 시나리오 6: findGeneration 상세 ---")
	idx5 := NewTreeIndex()
	idx5.Put("bar", Revision{2, 0})
	idx5.Put("bar", Revision{4, 0})
	idx5.Tombstone("bar", Revision{6, 0})
	idx5.Put("bar", Revision{8, 0})

	fmt.Println("bar의 keyIndex:")
	idx5.PrintKeyIndex("bar")
	fmt.Println()
	fmt.Println("각 리비전에서의 findGeneration 결과:")
	for _, rev := range []int64{1, 2, 3, 5, 6, 7, 8, 9} {
		_, _, ver, err := idx5.Get("bar", rev)
		if err != nil {
			fmt.Printf("  rev=%d → 찾을 수 없음 (%v)\n", rev, err)
		} else {
			fmt.Printf("  rev=%d → 존재 (버전=%d)\n", rev, ver)
		}
	}

	fmt.Println()
	fmt.Println("=== B-tree 인덱스 핵심 원리 ===")
	fmt.Println("1. keyIndex: 하나의 키에 대한 모든 리비전을 generation 단위로 관리")
	fmt.Println("2. generation: 키의 한 생명주기 (put→수정→tombstone)")
	fmt.Println("3. get(atRev): findGeneration + walk로 O(log n) 조회")
	fmt.Println("4. Range: B-tree의 범위 검색으로 접두사 쿼리 지원")
	fmt.Println("5. compact: 오래된 리비전 정리, 빈 세대 제거")
}
