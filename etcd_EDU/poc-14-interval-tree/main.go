package main

import (
	"fmt"
	"sort"
	"strings"
)

// ============================================================================
// etcd IntervalTree PoC - 범위 워처 관리 자료구조
// ============================================================================
//
// etcd의 pkg/adt/interval_tree.go에서 영감을 받은 구현.
// etcd는 Red-Black 기반 IntervalTree를 사용하여 범위 워치(watch)를 관리한다.
// 키 이벤트 발생 시 Stab 쿼리로 해당 키를 포함하는 모든 워처를 O(log N + K)에 찾는다.
//
// 이 PoC는 정렬 기반 단순 구현 (O(N) stab)으로 핵심 개념을 시연한다.
//
// 핵심 자료구조:
//   type Interval struct { Begin, End Comparable }
//   type IntervalValue struct { Ivl Interval; Val any }
//   type IntervalTree interface {
//       Insert(ivl Interval, val any)
//       Delete(ivl Interval) bool
//       Stab(iv Interval) []*IntervalValue
//       Visit(ivl Interval, ivv IntervalVisitor)
//   }
//
// 참조: pkg/adt/interval_tree.go
// ============================================================================

// ---- Comparable 인터페이스 (etcd의 adt.Comparable) ----

// StringKey는 문자열 기반 비교 가능한 키이다.
type StringKey string

func (s StringKey) Compare(other StringKey) int {
	if s < other {
		return -1
	}
	if s > other {
		return 1
	}
	return 0
}

// ---- Interval (구간) ----

// Interval은 [Begin, End) 반개방 구간을 나타낸다.
// etcd의 Interval 구조체와 동일한 의미론.
type Interval struct {
	Begin StringKey
	End   StringKey
}

// Contains는 주어진 점(point)이 구간에 포함되는지 검사한다.
func (ivl Interval) Contains(point StringKey) bool {
	return ivl.Begin.Compare(point) <= 0 && point.Compare(ivl.End) < 0
}

// Overlaps는 두 구간이 겹치는지 검사한다.
func (ivl Interval) Overlaps(other Interval) bool {
	return ivl.Begin.Compare(other.End) < 0 && other.Begin.Compare(ivl.End) < 0
}

func (ivl Interval) String() string {
	return fmt.Sprintf("[%s, %s)", ivl.Begin, ivl.End)
}

// ---- IntervalValue ----

// IntervalValue는 구간과 연결된 값을 나타낸다.
// etcd의 IntervalValue 구조체에 대응.
type IntervalValue struct {
	Ivl Interval
	Val interface{}
}

// ---- Watcher (워처 메타데이터) ----

// Watcher는 범위 워치의 메타데이터이다.
type Watcher struct {
	ID       int
	KeyRange string // "begin-end" 형태 설명
}

// ---- IntervalTree (정렬 기반 단순 구현) ----

// IntervalTree는 구간을 관리하는 자료구조이다.
// etcd는 Red-Black 트리 기반이지만, 이 PoC는 정렬된 슬라이스로 핵심 개념을 보여준다.
//
// etcd의 IntervalTree 인터페이스:
//   Insert(ivl Interval, val any)
//   Delete(ivl Interval) bool
//   Stab(iv Interval) []*IntervalValue
//   Visit(ivl Interval, ivv IntervalVisitor)
//   Len() int
type IntervalTree struct {
	items []IntervalValue
}

func NewIntervalTree() *IntervalTree {
	return &IntervalTree{
		items: make([]IntervalValue, 0),
	}
}

// Insert는 구간-값 쌍을 트리에 삽입한다.
// 삽입 후 Begin 기준으로 정렬 유지.
func (t *IntervalTree) Insert(ivl Interval, val interface{}) {
	t.items = append(t.items, IntervalValue{Ivl: ivl, Val: val})
	// Begin 기준 정렬 유지
	sort.Slice(t.items, func(i, j int) bool {
		return t.items[i].Ivl.Begin.Compare(t.items[j].Ivl.Begin) < 0
	})
}

// Delete는 정확히 일치하는 구간을 삭제한다.
func (t *IntervalTree) Delete(ivl Interval) bool {
	for i, item := range t.items {
		if item.Ivl.Begin == ivl.Begin && item.Ivl.End == ivl.End {
			t.items = append(t.items[:i], t.items[i+1:]...)
			return true
		}
	}
	return false
}

// Stab은 주어진 점을 포함하는 모든 구간을 반환한다 (stabbing query).
// etcd에서 키 이벤트 발생 시 해당 키를 워치하는 모든 워처를 찾는 데 사용.
//
// etcd의 visit() 메서드가 재귀적으로 트리를 순회하며 겹치는 구간을 찾는 것과 유사.
func (t *IntervalTree) Stab(point StringKey) []*IntervalValue {
	var results []*IntervalValue
	for i := range t.items {
		if t.items[i].Ivl.Contains(point) {
			results = append(results, &t.items[i])
		}
	}
	return results
}

// StabRange는 주어진 구간과 겹치는 모든 구간을 반환한다.
func (t *IntervalTree) StabRange(ivl Interval) []*IntervalValue {
	var results []*IntervalValue
	for i := range t.items {
		if t.items[i].Ivl.Overlaps(ivl) {
			results = append(results, &t.items[i])
		}
	}
	return results
}

// Visit은 주어진 구간과 겹치는 모든 구간에 대해 visitor를 호출한다.
func (t *IntervalTree) Visit(ivl Interval, visitor func(*IntervalValue) bool) {
	for i := range t.items {
		if t.items[i].Ivl.Overlaps(ivl) {
			if !visitor(&t.items[i]) {
				return
			}
		}
	}
}

// Len은 트리의 구간 수를 반환한다.
func (t *IntervalTree) Len() int {
	return len(t.items)
}

// Find는 정확히 일치하는 구간을 찾는다.
func (t *IntervalTree) Find(ivl Interval) *IntervalValue {
	for i := range t.items {
		if t.items[i].Ivl.Begin == ivl.Begin && t.items[i].Ivl.End == ivl.End {
			return &t.items[i]
		}
	}
	return nil
}

// ---- WatcherGroup (워처 그룹) ----

// WatcherGroup은 IntervalTree를 사용하여 범위 워처를 관리한다.
// etcd의 watcherGroup에서 범위 워처 관리에 IntervalTree를 사용:
//   server/storage/mvcc/watcher_group.go
type WatcherGroup struct {
	tree    *IntervalTree
	nextID  int
}

func NewWatcherGroup() *WatcherGroup {
	return &WatcherGroup{
		tree: NewIntervalTree(),
	}
}

// AddWatcher는 범위 워처를 등록한다.
func (wg *WatcherGroup) AddWatcher(begin, end StringKey) *Watcher {
	wg.nextID++
	w := &Watcher{
		ID:       wg.nextID,
		KeyRange: fmt.Sprintf("%s-%s", begin, end),
	}
	wg.tree.Insert(Interval{Begin: begin, End: end}, w)
	return w
}

// RemoveWatcher는 워처를 제거한다.
func (wg *WatcherGroup) RemoveWatcher(begin, end StringKey) bool {
	return wg.tree.Delete(Interval{Begin: begin, End: end})
}

// NotifyKey는 키 이벤트 발생 시 매칭되는 워처를 찾는다.
func (wg *WatcherGroup) NotifyKey(key StringKey) []*Watcher {
	results := wg.tree.Stab(key)
	watchers := make([]*Watcher, 0, len(results))
	for _, iv := range results {
		watchers = append(watchers, iv.Val.(*Watcher))
	}
	return watchers
}

// ============================================================================
// 데모
// ============================================================================

func main() {
	fmt.Println("=== etcd IntervalTree (범위 워처 관리) PoC ===")
	fmt.Println()

	// ---- 1. 기본 Interval 연산 ----
	fmt.Println("--- 1. Interval 기본 연산 ---")
	ivl1 := Interval{Begin: "a", End: "d"}
	ivl2 := Interval{Begin: "c", End: "f"}
	ivl3 := Interval{Begin: "g", End: "z"}

	fmt.Printf("  %s contains 'b': %v\n", ivl1, ivl1.Contains("b"))
	fmt.Printf("  %s contains 'd': %v (반개방, End 미포함)\n", ivl1, ivl1.Contains("d"))
	fmt.Printf("  %s overlaps %s: %v\n", ivl1, ivl2, ivl1.Overlaps(ivl2))
	fmt.Printf("  %s overlaps %s: %v\n", ivl1, ivl3, ivl1.Overlaps(ivl3))
	fmt.Println()

	// ---- 2. IntervalTree Stab 쿼리 ----
	fmt.Println("--- 2. IntervalTree Stab(점 쿼리) ---")
	tree := NewIntervalTree()
	tree.Insert(Interval{Begin: "a", End: "d"}, "watcher-1: [a,d)")
	tree.Insert(Interval{Begin: "b", End: "f"}, "watcher-2: [b,f)")
	tree.Insert(Interval{Begin: "e", End: "h"}, "watcher-3: [e,h)")
	tree.Insert(Interval{Begin: "a", End: "z"}, "watcher-4: [a,z) (전체)")

	fmt.Printf("  트리에 %d개 구간 등록\n", tree.Len())

	testPoints := []StringKey{"a", "c", "e", "g", "z"}
	for _, pt := range testPoints {
		results := tree.Stab(pt)
		names := make([]string, 0, len(results))
		for _, r := range results {
			names = append(names, r.Val.(string))
		}
		fmt.Printf("  Stab('%s') → %d개 매칭: %s\n",
			pt, len(results), strings.Join(names, ", "))
	}
	fmt.Println()

	// ---- 3. 범위 워처 시나리오 (etcd Watch) ----
	fmt.Println("--- 3. 범위 워처 시나리오 (etcd Watch) ---")
	wg := NewWatcherGroup()

	// 워처 등록
	w1 := wg.AddWatcher("/users/", "/users0")  // /users/ 프리픽스
	w2 := wg.AddWatcher("/config/", "/config0") // /config/ 프리픽스
	w3 := wg.AddWatcher("/", "0")               // 전체 키 범위
	w4 := wg.AddWatcher("/users/admin", "/users/admin0") // 특정 키

	fmt.Printf("  등록된 워처:\n")
	fmt.Printf("    워처#%d: /users/ 프리픽스\n", w1.ID)
	fmt.Printf("    워처#%d: /config/ 프리픽스\n", w2.ID)
	fmt.Printf("    워처#%d: / 전체 범위\n", w3.ID)
	fmt.Printf("    워처#%d: /users/admin 특정 키\n", w4.ID)
	fmt.Println()

	// 키 이벤트 발생 시 매칭 워처 찾기
	events := []struct {
		key    StringKey
		action string
	}{
		{"/users/alice", "PUT"},
		{"/users/admin", "PUT"},
		{"/config/db-host", "PUT"},
		{"/logs/app1", "PUT"},
		{"/users/bob", "DELETE"},
	}

	fmt.Printf("  키 이벤트 발생 및 워처 매칭:\n")
	for _, ev := range events {
		watchers := wg.NotifyKey(ev.key)
		ids := make([]string, 0, len(watchers))
		for _, w := range watchers {
			ids = append(ids, fmt.Sprintf("#%d", w.ID))
		}
		fmt.Printf("    %s %s → 매칭 워처: [%s]\n",
			ev.action, ev.key, strings.Join(ids, ", "))
	}
	fmt.Println()

	// ---- 4. 워처 삭제 ----
	fmt.Println("--- 4. 워처 삭제 ---")
	fmt.Printf("  삭제 전 트리 크기: %d\n", wg.tree.Len())
	removed := wg.RemoveWatcher("/config/", "/config0")
	fmt.Printf("  /config/ 워처 삭제: %v\n", removed)
	fmt.Printf("  삭제 후 트리 크기: %d\n", wg.tree.Len())

	// 삭제 후 /config/db-host 이벤트 재확인
	watchers := wg.NotifyKey("/config/db-host")
	ids := make([]string, 0, len(watchers))
	for _, w := range watchers {
		ids = append(ids, fmt.Sprintf("#%d", w.ID))
	}
	fmt.Printf("  PUT /config/db-host → 매칭 워처: [%s] (/config/ 워처 제거됨)\n",
		strings.Join(ids, ", "))
	fmt.Println()

	// ---- 5. StabRange (범위 쿼리) ----
	fmt.Println("--- 5. StabRange (범위 겹침 쿼리) ---")
	tree2 := NewIntervalTree()
	tree2.Insert(Interval{Begin: "a", End: "c"}, "seg-1")
	tree2.Insert(Interval{Begin: "b", End: "e"}, "seg-2")
	tree2.Insert(Interval{Begin: "d", End: "g"}, "seg-3")
	tree2.Insert(Interval{Begin: "h", End: "k"}, "seg-4")

	queryRange := Interval{Begin: "c", End: "f"}
	results := tree2.StabRange(queryRange)
	fmt.Printf("  쿼리 범위: %s\n", queryRange)
	for _, r := range results {
		fmt.Printf("    겹치는 구간: %s → %s\n", r.Ivl, r.Val)
	}
	fmt.Println()

	// ---- 6. Visit 패턴 ----
	fmt.Println("--- 6. Visit 패턴 (방문자 콜백) ---")
	tree3 := NewIntervalTree()
	for i := 0; i < 10; i++ {
		begin := StringKey(fmt.Sprintf("key-%02d", i*3))
		end := StringKey(fmt.Sprintf("key-%02d", i*3+5))
		tree3.Insert(Interval{Begin: begin, End: end},
			fmt.Sprintf("범위#%d", i+1))
	}
	fmt.Printf("  트리에 %d개 구간 등록\n", tree3.Len())

	queryIvl := Interval{Begin: "key-10", End: "key-15"}
	fmt.Printf("  Visit(%s) 결과:\n", queryIvl)
	count := 0
	tree3.Visit(queryIvl, func(iv *IntervalValue) bool {
		count++
		fmt.Printf("    [%d] %s → %s\n", count, iv.Ivl, iv.Val)
		return true // 계속 방문
	})
	fmt.Println()

	fmt.Println("=== 핵심 정리 ===")
	fmt.Println("1. IntervalTree는 범위 워치를 효율적으로 관리하는 자료구조이다")
	fmt.Println("2. Stab(점 쿼리): 키 이벤트 발생 시 해당 키를 포함하는 모든 워처 탐색")
	fmt.Println("3. etcd는 Red-Black 트리 기반 O(log N + K) 탐색 (이 PoC는 O(N))")
	fmt.Println("4. 범위 워치의 begin/end는 etcd의 키 범위 프리픽스 매칭에 활용")
	fmt.Println("5. watcherGroup이 IntervalTree를 사용하여 범위 워처를 조직화")
}
