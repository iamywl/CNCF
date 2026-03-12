package main

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"
)

// =============================================================================
// Prometheus TSDB Inverted Index (MemPostings) PoC
// =============================================================================
//
// Prometheus TSDB는 시계열 데이터를 레이블(label) 기반으로 검색하기 위해
// 역인덱스(inverted index)를 사용한다. 핵심 구조체는 MemPostings이며,
// map[labelName]map[labelValue][]SeriesRef 형태로 구성된다.
//
// 각 레이블 쌍(name=value)에 대해 해당 레이블을 가진 시리즈의 참조(ref) 목록을
// 정렬된 상태로 유지한다. 쿼리 시 여러 레이블 조건을 Intersect/Merge/Without
// 연산으로 조합하여 O(n) 시간에 결과를 도출한다.
//
// 실제 코드 참조:
//   - tsdb/index/postings.go: MemPostings 구조체
//   - tsdb/index/postings.go: Add(), Postings(), Intersect(), Merge(), Without()
//   - tsdb/index/postings.go: Postings 인터페이스 (Next, Seek, At, Err)
//   - tsdb/index/postings.go: intersectPostings — Seek 기반 교집합
//   - tsdb/index/postings.go: mergedPostings — loser tree 기반 합집합
//   - tsdb/index/postings.go: removedPostings — 차집합

// =============================================================================
// 데이터 구조
// =============================================================================

// SeriesRef는 시리즈의 고유 참조 ID이다.
// 실제 Prometheus에서는 storage.SeriesRef (uint64) 타입이다.
type SeriesRef uint64

// Label은 레이블의 이름과 값 쌍이다.
type Label struct {
	Name  string
	Value string
}

// Labels는 하나의 시리즈를 식별하는 레이블 집합이다.
type Labels []Label

func (ls Labels) String() string {
	parts := make([]string, len(ls))
	for i, l := range ls {
		parts[i] = fmt.Sprintf("%s=%q", l.Name, l.Value)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// =============================================================================
// Postings 인터페이스 및 구현
// =============================================================================
//
// Prometheus의 Postings 인터페이스는 정렬된 SeriesRef 목록에 대한
// 이터레이터(iterator) 패턴을 정의한다. Next()로 순회하고, Seek()으로
// 특정 값 이상의 위치로 건너뛸 수 있다.
//
// 참조: tsdb/index/postings.go — type Postings interface {
//   Next() bool; Seek(v SeriesRef) bool; At() SeriesRef; Err() error
// }

// Postings는 정렬된 SeriesRef 목록에 대한 이터레이터 인터페이스이다.
type Postings interface {
	Next() bool
	Seek(v SeriesRef) bool
	At() SeriesRef
}

// listPostings는 정렬된 슬라이스 기반 Postings 구현이다.
// 참조: tsdb/index/postings.go — type listPostings struct { list []SeriesRef, cur SeriesRef }
type listPostings struct {
	list []SeriesRef
	cur  SeriesRef
	idx  int
}

func newListPostings(list []SeriesRef) *listPostings {
	return &listPostings{list: list, idx: -1}
}

func (p *listPostings) Next() bool {
	p.idx++
	if p.idx < len(p.list) {
		p.cur = p.list[p.idx]
		return true
	}
	return false
}

// Seek는 v 이상의 값으로 전진한다. 이미 v 이상이면 현재 위치를 유지한다.
// 정렬된 리스트에서 이진 검색 대신 선형 스캔을 사용하는데,
// 실제 Prometheus도 listPostings에서 선형 스캔을 사용한다.
// (intersectPostings의 Seek 루프에서 호출되므로 이미 근처에 있는 경우가 많다)
func (p *listPostings) Seek(v SeriesRef) bool {
	// 현재 위치가 이미 v 이상이면 그대로 반환
	if p.idx >= 0 && p.idx < len(p.list) && p.cur >= v {
		return true
	}

	// 아직 시작 전이면 idx를 0부터 시작
	start := p.idx + 1
	if start < 0 {
		start = 0
	}

	for i := start; i < len(p.list); i++ {
		if p.list[i] >= v {
			p.idx = i
			p.cur = p.list[i]
			return true
		}
	}
	p.idx = len(p.list)
	return false
}

func (p *listPostings) At() SeriesRef {
	return p.cur
}

// emptyPostings는 항상 비어있는 Postings이다.
type emptyPostings struct{}

func (emptyPostings) Next() bool            { return false }
func (emptyPostings) Seek(SeriesRef) bool    { return false }
func (emptyPostings) At() SeriesRef          { return 0 }

// =============================================================================
// MemPostings — 역인덱스 핵심 구조체
// =============================================================================
//
// MemPostings는 레이블 쌍별로 시리즈 참조 목록을 관리하는 인메모리 역인덱스이다.
//
// 구조: map[labelName]map[labelValue][]SeriesRef
//
// 특수 키 allPostingsKey("")=""는 모든 시리즈의 참조를 담는다.
// Add() 시 각 레이블 쌍과 allPostingsKey에 대해 정렬을 유지하면서 삽입한다.
//
// 참조: tsdb/index/postings.go
//   type MemPostings struct {
//       mtx sync.RWMutex
//       m   map[string]map[string][]storage.SeriesRef
//       lvs map[string][]string  // 레이블명별 값 목록 (append-only)
//       ordered bool
//   }

const allPostingsName = ""
const allPostingsValue = ""

// MemPostings는 레이블 쌍별 역인덱스를 관리한다.
type MemPostings struct {
	// m은 핵심 인덱스: m[labelName][labelValue] = sorted []SeriesRef
	m map[string]map[string][]SeriesRef

	// lvs는 레이블명별 값 목록 (빠른 LabelValues 조회용)
	// 실제 Prometheus에서도 lvs map[string][]string으로 관리한다.
	lvs map[string][]string
}

// NewMemPostings는 새로운 MemPostings를 생성한다.
func NewMemPostings() *MemPostings {
	return &MemPostings{
		m:   make(map[string]map[string][]SeriesRef),
		lvs: make(map[string][]string),
	}
}

// Add는 시리즈 참조와 레이블 집합을 인덱스에 추가한다.
// 각 레이블 쌍에 대해 정렬을 유지하면서 ref를 삽입한다.
//
// 참조: tsdb/index/postings.go — func (p *MemPostings) Add(id storage.SeriesRef, lset labels.Labels)
//   lset.Range(func(l labels.Label) { p.addFor(id, l) })
//   p.addFor(id, allPostingsKey)  // 모든 시리즈를 allPostingsKey에도 등록
func (p *MemPostings) Add(ref SeriesRef, lset Labels) {
	for _, l := range lset {
		p.addFor(ref, l.Name, l.Value)
	}
	// allPostingsKey에도 추가 — 전체 시리즈 목록 유지
	p.addFor(ref, allPostingsName, allPostingsValue)
}

// addFor는 특정 레이블 쌍에 ref를 추가한다.
// 정렬된 상태를 유지하기 위해 삽입 후 끝에서부터 정렬 위반을 수정한다.
//
// 참조: tsdb/index/postings.go — func (p *MemPostings) addFor(id storage.SeriesRef, l labels.Label)
//   list = appendWithExponentialGrowth(vm, id)
//   // 끝에서부터 insertion sort로 정렬 유지
//   for i := len(list)-1; i >= 1; i-- {
//       if list[i] >= list[i-1] { break }
//       list[i], list[i-1] = list[i-1], list[i]
//   }
func (p *MemPostings) addFor(ref SeriesRef, name, value string) {
	nm, ok := p.m[name]
	if !ok {
		nm = map[string][]SeriesRef{}
		p.m[name] = nm
	}

	_, exists := nm[value]
	if !exists {
		// 새로운 값이면 lvs에도 추가
		p.lvs[name] = append(p.lvs[name], value)
	}

	list := append(nm[value], ref)
	nm[value] = list

	// 정렬 위반 수정: 끝에서부터 insertion sort
	// 대부분의 경우 ID가 증가하므로 바로 break되어 O(1)
	for i := len(list) - 1; i >= 1; i-- {
		if list[i] >= list[i-1] {
			break
		}
		list[i], list[i-1] = list[i-1], list[i]
	}
}

// Postings는 특정 레이블 쌍에 해당하는 Postings 이터레이터를 반환한다.
//
// 참조: tsdb/index/postings.go — func (p *MemPostings) Postings(ctx, name, values...)
func (p *MemPostings) Postings(name, value string) Postings {
	nm, ok := p.m[name]
	if !ok {
		return emptyPostings{}
	}
	refs, ok := nm[value]
	if !ok {
		return emptyPostings{}
	}
	return newListPostings(refs)
}

// AllPostings는 모든 시리즈의 Postings를 반환한다.
func (p *MemPostings) AllPostings() Postings {
	return p.Postings(allPostingsName, allPostingsValue)
}

// LabelValues는 특정 레이블 이름에 대한 모든 값을 반환한다.
//
// 참조: tsdb/index/postings.go — func (p *MemPostings) LabelValues(ctx, name, hints)
//   values := p.lvs[name]   // append-only 슬라이스에서 바로 반환
func (p *MemPostings) LabelValues(name string) []string {
	vals := make([]string, len(p.lvs[name]))
	copy(vals, p.lvs[name])
	sort.Strings(vals)
	return vals
}

// LabelNames는 모든 레이블 이름을 반환한다.
func (p *MemPostings) LabelNames() []string {
	names := make([]string, 0, len(p.m))
	for name := range p.m {
		if name != allPostingsName {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// SeriesCount는 전체 시리즈 수를 반환한다.
func (p *MemPostings) SeriesCount() int {
	if refs, ok := p.m[allPostingsName][allPostingsValue]; ok {
		return len(refs)
	}
	return 0
}

// =============================================================================
// Postings 집합 연산: Intersect, Merge, Without
// =============================================================================
//
// Prometheus 쿼리 엔진은 여러 매처(matcher)의 결과를 조합할 때
// 이 세 가지 집합 연산을 사용한다.
//
// 예: {job="api", method="GET", status!="500"}
//   1. Postings("job", "api")     → refs_a
//   2. Postings("method", "GET")  → refs_b
//   3. Postings("status", "500")  → refs_c
//   4. Intersect(refs_a, refs_b)  → 교집합 (job=api AND method=GET)
//   5. Without(result, refs_c)    → 차집합 (status != 500 제외)

// ExpandPostings는 Postings 이터레이터를 슬라이스로 변환한다.
//
// 참조: tsdb/index/postings.go — func ExpandPostings(p Postings) ([]SeriesRef, error)
func ExpandPostings(p Postings) []SeriesRef {
	var res []SeriesRef
	for p.Next() {
		res = append(res, p.At())
	}
	return res
}

// Intersect는 여러 Postings의 교집합을 반환한다.
// 모든 입력 리스트가 정렬되어 있으므로 O(n) 시간에 교집합을 구할 수 있다.
//
// 알고리즘: Seek 기반 교차 검증
//   1. 첫 번째 리스트에서 Next()로 타겟을 얻음
//   2. 나머지 리스트에서 Seek(target)으로 전진
//   3. 모든 리스트의 At()이 같으면 교집합에 포함
//   4. 다르면 가장 큰 값을 새 타겟으로 설정하고 반복
//
// 참조: tsdb/index/postings.go — type intersectPostings struct
//   func (it *intersectPostings) Next() bool — 첫 번째 리스트 Next 후 Seek 루프
//   func (it *intersectPostings) Seek(target) bool — 모든 리스트에서 Seek 후 allEqual 확인
func Intersect(postings ...Postings) Postings {
	if len(postings) == 0 {
		return emptyPostings{}
	}
	if len(postings) == 1 {
		return postings[0]
	}
	return &intersectPostings{postings: postings}
}

type intersectPostings struct {
	postings []Postings
	cur      SeriesRef
}

func (it *intersectPostings) At() SeriesRef { return it.cur }

// Next는 다음 교집합 원소로 전진한다.
// 첫 번째 포스팅에서 타겟을 가져온 후, 나머지 포스팅에서 해당 값을 Seek한다.
// 모두 같은 값을 가리키면 교집합에 포함된다.
func (it *intersectPostings) Next() bool {
	if !it.postings[0].Next() {
		return false
	}
	target := it.postings[0].At()

	allEqual := true
	for _, p := range it.postings[1:] {
		if !p.Next() {
			return false
		}
		at := p.At()
		if at > target {
			target = at
			allEqual = false
		} else if at < target {
			allEqual = false
		}
	}
	if allEqual {
		it.cur = target
		return true
	}
	return it.Seek(target)
}

// Seek는 모든 포스팅을 target 이상의 위치로 전진시키고,
// 모두 같은 값을 가리킬 때까지 반복한다.
func (it *intersectPostings) Seek(target SeriesRef) bool {
	for {
		allEqual := true
		for _, p := range it.postings {
			if !p.Seek(target) {
				return false
			}
			if p.At() > target {
				target = p.At()
				allEqual = false
			}
		}
		if allEqual {
			it.cur = target
			return true
		}
	}
}

// Merge는 여러 Postings의 합집합을 반환한다 (중복 제거).
// 실제 Prometheus에서는 loser tree를 사용하지만, 이 PoC에서는
// 간단한 k-way merge 방식을 사용한다.
//
// 참조: tsdb/index/postings.go — func Merge[T Postings](ctx, its ...T) Postings
//   내부적으로 go-loser 라이브러리의 loser tree를 사용하여 O(n log k) 합집합
func Merge(postings ...Postings) Postings {
	if len(postings) == 0 {
		return emptyPostings{}
	}
	if len(postings) == 1 {
		return postings[0]
	}
	return &mergedPostings{postings: postings}
}

type mergedPostings struct {
	postings []Postings
	cur      SeriesRef
	started  bool
}

func (m *mergedPostings) At() SeriesRef { return m.cur }

// Next는 합집합의 다음 원소로 전진한다.
// 모든 포스팅 중 가장 작은 값을 선택하고, 중복을 건너뛴다.
// 실제 Prometheus는 loser tree로 O(log k)에 최소값을 찾지만,
// 이 PoC에서는 선형 스캔으로 단순하게 구현한다.
func (m *mergedPostings) Next() bool {
	if !m.started {
		// 초기화: 모든 포스팅에서 첫 번째 값을 가져옴
		m.started = true
		alive := m.postings[:0]
		for _, p := range m.postings {
			if p.Next() {
				alive = append(alive, p)
			}
		}
		m.postings = alive
		if len(m.postings) == 0 {
			return false
		}
		m.cur = m.findMin()
		return true
	}

	for {
		// 현재 값과 같은 포스팅들을 전진시킴 (중복 제거)
		alive := m.postings[:0]
		for _, p := range m.postings {
			if p.At() == m.cur {
				if !p.Next() {
					continue
				}
			}
			alive = append(alive, p)
		}
		m.postings = alive

		if len(m.postings) == 0 {
			return false
		}
		m.cur = m.findMin()
		return true
	}
}

func (m *mergedPostings) findMin() SeriesRef {
	min := m.postings[0].At()
	for _, p := range m.postings[1:] {
		if p.At() < min {
			min = p.At()
		}
	}
	return min
}

func (m *mergedPostings) Seek(v SeriesRef) bool {
	alive := m.postings[:0]
	for _, p := range m.postings {
		if p.Seek(v) {
			alive = append(alive, p)
		}
	}
	m.postings = alive
	if len(m.postings) == 0 {
		return false
	}
	m.started = true
	m.cur = m.findMin()
	return true
}

// Without는 full에서 drop에 포함된 원소를 제거한 차집합을 반환한다.
// 두 입력이 모두 정렬되어 있으므로 O(n)에 처리 가능하다.
//
// 참조: tsdb/index/postings.go — type removedPostings struct
//   func (rp *removedPostings) Next() bool
//   full과 remove를 동시에 순회하며 full에만 있는 값을 반환
func Without(full, drop Postings) Postings {
	return &removedPostings{full: full, drop: drop}
}

type removedPostings struct {
	full, drop  Postings
	cur         SeriesRef
	initialized bool
	fok, dok    bool
}

func (rp *removedPostings) At() SeriesRef { return rp.cur }

func (rp *removedPostings) Next() bool {
	if !rp.initialized {
		rp.fok = rp.full.Next()
		rp.dok = rp.drop.Next()
		rp.initialized = true
	}

	for {
		if !rp.fok {
			return false
		}
		if !rp.dok {
			// drop이 소진됨 — full의 나머지는 모두 결과에 포함
			rp.cur = rp.full.At()
			rp.fok = rp.full.Next()
			return true
		}

		fcur := rp.full.At()
		dcur := rp.drop.At()

		if fcur < dcur {
			// full의 현재 값이 drop보다 작으므로 결과에 포함
			rp.cur = fcur
			rp.fok = rp.full.Next()
			return true
		} else if fcur > dcur {
			// drop의 현재 값이 더 작으므로 drop을 전진
			rp.dok = rp.drop.Seek(fcur)
		} else {
			// 같은 값 — 제외하고 둘 다 전진
			rp.fok = rp.full.Next()
			rp.dok = rp.drop.Next()
		}
	}
}

func (rp *removedPostings) Seek(v SeriesRef) bool {
	rp.initialized = true
	rp.fok = rp.full.Seek(v)
	rp.dok = rp.drop.Seek(v)
	// Seek 후 Next와 같은 로직으로 drop에 없는 값을 찾아야 함
	if rp.fok && rp.dok && rp.full.At() == rp.drop.At() {
		return rp.Next()
	}
	if rp.fok {
		rp.cur = rp.full.At()
	}
	return rp.fok
}

// =============================================================================
// 헬퍼: 슬라이스 기반 집합 연산 (벤치마크 비교용)
// =============================================================================

// SliceIntersect는 두 정렬된 슬라이스의 교집합을 구한다 (투 포인터).
func SliceIntersect(a, b []SeriesRef) []SeriesRef {
	var result []SeriesRef
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i] == b[j] {
			result = append(result, a[i])
			i++
			j++
		} else if a[i] < b[j] {
			i++
		} else {
			j++
		}
	}
	return result
}

// SliceMerge는 두 정렬된 슬라이스의 합집합을 구한다 (중복 제거).
func SliceMerge(a, b []SeriesRef) []SeriesRef {
	var result []SeriesRef
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i] == b[j] {
			result = append(result, a[i])
			i++
			j++
		} else if a[i] < b[j] {
			result = append(result, a[i])
			i++
		} else {
			result = append(result, b[j])
			j++
		}
	}
	for ; i < len(a); i++ {
		result = append(result, a[i])
	}
	for ; j < len(b); j++ {
		result = append(result, b[j])
	}
	return result
}

// SliceWithout는 정렬된 full에서 drop에 포함된 원소를 제거한다.
func SliceWithout(full, drop []SeriesRef) []SeriesRef {
	var result []SeriesRef
	i, j := 0, 0
	for i < len(full) && j < len(drop) {
		if full[i] == drop[j] {
			i++
			j++
		} else if full[i] < drop[j] {
			result = append(result, full[i])
			i++
		} else {
			j++
		}
	}
	for ; i < len(full); i++ {
		result = append(result, full[i])
	}
	return result
}

// =============================================================================
// 데모: 시리즈 생성 및 쿼리 실행
// =============================================================================

func main() {
	fmt.Println("==========================================================")
	fmt.Println(" Prometheus TSDB Inverted Index (MemPostings) PoC")
	fmt.Println("==========================================================")
	fmt.Println()

	mp := NewMemPostings()

	// ─────────────────────────────────────────────────────────────────────
	// 1. 시리즈 등록: 10,000개의 다양한 레이블 조합
	// ─────────────────────────────────────────────────────────────────────
	fmt.Println("[1] 시리즈 등록 (10,000개)")
	fmt.Println("─────────────────────────────────────────────────────────")

	jobs := []string{"api-server", "web-frontend", "payment-service", "auth-service", "data-pipeline"}
	methods := []string{"GET", "POST", "PUT", "DELETE", "PATCH"}
	statuses := []string{"200", "201", "301", "400", "401", "403", "404", "500", "502", "503"}
	instances := []string{}
	for i := 0; i < 20; i++ {
		instances = append(instances, fmt.Sprintf("10.0.%d.%d:8080", i/10, i%10))
	}
	paths := []string{"/api/users", "/api/orders", "/api/products", "/api/health", "/api/metrics",
		"/api/login", "/api/logout", "/api/search", "/api/config", "/api/events"}

	rng := rand.New(rand.NewSource(42))
	totalSeries := 10000

	start := time.Now()
	for i := 0; i < totalSeries; i++ {
		ref := SeriesRef(i + 1)
		lset := Labels{
			{Name: "__name__", Value: "http_requests_total"},
			{Name: "job", Value: jobs[rng.Intn(len(jobs))]},
			{Name: "method", Value: methods[rng.Intn(len(methods))]},
			{Name: "status", Value: statuses[rng.Intn(len(statuses))]},
			{Name: "instance", Value: instances[rng.Intn(len(instances))]},
			{Name: "path", Value: paths[rng.Intn(len(paths))]},
		}
		mp.Add(ref, lset)
	}
	addDuration := time.Since(start)

	fmt.Printf("  등록 완료: %d개 시리즈, 소요시간: %v\n", totalSeries, addDuration)
	fmt.Printf("  전체 시리즈 수 (AllPostings): %d\n", mp.SeriesCount())
	fmt.Println()

	// ─────────────────────────────────────────────────────────────────────
	// 2. 인덱스 구조 통계
	// ─────────────────────────────────────────────────────────────────────
	fmt.Println("[2] 인덱스 구조 통계")
	fmt.Println("─────────────────────────────────────────────────────────")

	labelNames := mp.LabelNames()
	fmt.Printf("  레이블 이름 수: %d\n", len(labelNames))
	fmt.Println()
	fmt.Println("  레이블명          | 고유값 수 | 포스팅 리스트 수")
	fmt.Println("  ──────────────────┼──────────┼─────────────────")
	for _, name := range labelNames {
		vals := mp.LabelValues(name)
		totalPostings := 0
		for _, v := range vals {
			refs := ExpandPostings(mp.Postings(name, v))
			totalPostings += len(refs)
		}
		fmt.Printf("  %-18s│ %8d │ %15d\n", name, len(vals), totalPostings)
	}
	fmt.Println()

	// ─────────────────────────────────────────────────────────────────────
	// 3. LabelValues 조회
	// ─────────────────────────────────────────────────────────────────────
	fmt.Println("[3] LabelValues 조회")
	fmt.Println("─────────────────────────────────────────────────────────")
	for _, name := range []string{"job", "method", "status"} {
		vals := mp.LabelValues(name)
		fmt.Printf("  %s: %v\n", name, vals)
	}
	fmt.Println()

	// ─────────────────────────────────────────────────────────────────────
	// 4. 단일 레이블 매칭 쿼리
	// ─────────────────────────────────────────────────────────────────────
	fmt.Println("[4] 단일 레이블 매칭 쿼리")
	fmt.Println("─────────────────────────────────────────────────────────")

	queries := []struct {
		name, value string
	}{
		{"job", "api-server"},
		{"method", "GET"},
		{"status", "500"},
		{"instance", "10.0.0.0:8080"},
		{"path", "/api/users"},
	}

	for _, q := range queries {
		start := time.Now()
		refs := ExpandPostings(mp.Postings(q.name, q.value))
		dur := time.Since(start)
		fmt.Printf("  %s=%q → %d개 시리즈 (%v)\n", q.name, q.value, len(refs), dur)
	}
	fmt.Println()

	// ─────────────────────────────────────────────────────────────────────
	// 5. Intersect (교집합) — AND 쿼리
	// ─────────────────────────────────────────────────────────────────────
	fmt.Println("[5] Intersect (교집합) — AND 쿼리")
	fmt.Println("─────────────────────────────────────────────────────────")
	fmt.Println("  쿼리: {job=\"api-server\", method=\"GET\"}")
	fmt.Println()

	start = time.Now()
	result := ExpandPostings(Intersect(
		mp.Postings("job", "api-server"),
		mp.Postings("method", "GET"),
	))
	dur := time.Since(start)
	fmt.Printf("  결과: %d개 시리즈 (%v)\n", len(result), dur)
	if len(result) > 5 {
		fmt.Printf("  처음 5개 ref: %v ...\n", result[:5])
	}
	fmt.Println()

	// 3개 레이블 교집합
	fmt.Println("  쿼리: {job=\"api-server\", method=\"GET\", status=\"200\"}")
	start = time.Now()
	result = ExpandPostings(Intersect(
		mp.Postings("job", "api-server"),
		mp.Postings("method", "GET"),
		mp.Postings("status", "200"),
	))
	dur = time.Since(start)
	fmt.Printf("  결과: %d개 시리즈 (%v)\n", len(result), dur)
	fmt.Println()

	// 4개 레이블 교집합
	fmt.Println("  쿼리: {job=\"api-server\", method=\"GET\", status=\"200\", path=\"/api/users\"}")
	start = time.Now()
	result = ExpandPostings(Intersect(
		mp.Postings("job", "api-server"),
		mp.Postings("method", "GET"),
		mp.Postings("status", "200"),
		mp.Postings("path", "/api/users"),
	))
	dur = time.Since(start)
	fmt.Printf("  결과: %d개 시리즈 (%v)\n", len(result), dur)
	fmt.Println()

	// ─────────────────────────────────────────────────────────────────────
	// 6. Merge (합집합) — OR 쿼리
	// ─────────────────────────────────────────────────────────────────────
	fmt.Println("[6] Merge (합집합) — OR 쿼리")
	fmt.Println("─────────────────────────────────────────────────────────")
	fmt.Println("  쿼리: {method=\"GET\"} OR {method=\"POST\"}")
	fmt.Println()

	start = time.Now()
	result = ExpandPostings(Merge(
		mp.Postings("method", "GET"),
		mp.Postings("method", "POST"),
	))
	dur = time.Since(start)
	fmt.Printf("  결과: %d개 시리즈 (%v)\n", len(result), dur)
	fmt.Println()

	// 5개 합집합
	fmt.Println("  쿼리: {status=~\"4..|5..\"} (400, 401, 403, 404, 500, 502, 503 합집합)")
	start = time.Now()
	result = ExpandPostings(Merge(
		mp.Postings("status", "400"),
		mp.Postings("status", "401"),
		mp.Postings("status", "403"),
		mp.Postings("status", "404"),
		mp.Postings("status", "500"),
		mp.Postings("status", "502"),
		mp.Postings("status", "503"),
	))
	dur = time.Since(start)
	fmt.Printf("  결과: %d개 시리즈 (%v)\n", len(result), dur)
	fmt.Println()

	// ─────────────────────────────────────────────────────────────────────
	// 7. Without (차집합) — NOT 쿼리
	// ─────────────────────────────────────────────────────────────────────
	fmt.Println("[7] Without (차집합) — NOT 쿼리")
	fmt.Println("─────────────────────────────────────────────────────────")
	fmt.Println("  쿼리: {job=\"api-server\", status!=\"500\"}")
	fmt.Println()

	start = time.Now()
	jobRefs := mp.Postings("job", "api-server")
	status500 := mp.Postings("status", "500")
	result = ExpandPostings(Without(jobRefs, status500))
	dur = time.Since(start)

	// 검증: job=api-server 전체와 비교
	allJobRefs := ExpandPostings(mp.Postings("job", "api-server"))
	jobAnd500 := ExpandPostings(Intersect(
		mp.Postings("job", "api-server"),
		mp.Postings("status", "500"),
	))
	fmt.Printf("  job=\"api-server\" 전체: %d개\n", len(allJobRefs))
	fmt.Printf("  job=\"api-server\" AND status=\"500\": %d개\n", len(jobAnd500))
	fmt.Printf("  Without 결과: %d개 (%v)\n", len(result), dur)
	fmt.Printf("  검증: %d - %d = %d (일치: %v)\n",
		len(allJobRefs), len(jobAnd500), len(allJobRefs)-len(jobAnd500),
		len(result) == len(allJobRefs)-len(jobAnd500))
	fmt.Println()

	// ─────────────────────────────────────────────────────────────────────
	// 8. 복합 쿼리: Intersect + Without
	// ─────────────────────────────────────────────────────────────────────
	fmt.Println("[8] 복합 쿼리: Intersect + Without")
	fmt.Println("─────────────────────────────────────────────────────────")
	fmt.Println("  쿼리: {job=\"api-server\", method=\"GET\", status!=\"500\", status!=\"502\", status!=\"503\"}")
	fmt.Println()

	start = time.Now()
	// 1. job=api-server AND method=GET
	baseResult := Intersect(
		mp.Postings("job", "api-server"),
		mp.Postings("method", "GET"),
	)
	// 2. status=500 OR status=502 OR status=503 (제외할 집합)
	errorStatuses := Merge(
		mp.Postings("status", "500"),
		mp.Postings("status", "502"),
		mp.Postings("status", "503"),
	)
	// 3. Without으로 제외
	result = ExpandPostings(Without(baseResult, errorStatuses))
	dur = time.Since(start)
	fmt.Printf("  결과: %d개 시리즈 (%v)\n", len(result), dur)
	fmt.Println()

	// ─────────────────────────────────────────────────────────────────────
	// 9. 성능 프로파일링
	// ─────────────────────────────────────────────────────────────────────
	fmt.Println("[9] 성능 프로파일링")
	fmt.Println("─────────────────────────────────────────────────────────")
	fmt.Println()

	// 9a. Postings 조회 성능 (카디널리티별 비교)
	fmt.Println("  9a. Postings 조회 — 카디널리티별 비교")
	fmt.Println("  ──────────────────────────────────────────")
	benchQueries := []struct {
		name, value string
		desc        string
	}{
		{"__name__", "http_requests_total", "높은 카디널리티 (전체)"},
		{"job", "api-server", "중간 카디널리티 (~20%)"},
		{"status", "500", "낮은 카디널리티 (~10%)"},
	}

	for _, q := range benchQueries {
		const iters = 1000
		start := time.Now()
		var count int
		for i := 0; i < iters; i++ {
			refs := ExpandPostings(mp.Postings(q.name, q.value))
			count = len(refs)
		}
		elapsed := time.Since(start)
		fmt.Printf("  %-35s %5d개 시리즈, %d회 반복: %v (회당 %v)\n",
			q.desc, count, iters, elapsed, elapsed/time.Duration(iters))
	}
	fmt.Println()

	// 9b. Intersect vs Merge 성능 비교
	fmt.Println("  9b. Intersect vs Merge — 연산별 성능 비교")
	fmt.Println("  ──────────────────────────────────────────")

	// Intersect 벤치마크
	const benchIters = 1000
	start = time.Now()
	var intersectCount int
	for i := 0; i < benchIters; i++ {
		refs := ExpandPostings(Intersect(
			mp.Postings("job", "api-server"),
			mp.Postings("method", "GET"),
		))
		intersectCount = len(refs)
	}
	intersectDur := time.Since(start)

	// Merge 벤치마크
	start = time.Now()
	var mergeCount int
	for i := 0; i < benchIters; i++ {
		refs := ExpandPostings(Merge(
			mp.Postings("job", "api-server"),
			mp.Postings("method", "GET"),
		))
		mergeCount = len(refs)
	}
	mergeDur := time.Since(start)

	// Without 벤치마크
	start = time.Now()
	var withoutCount int
	for i := 0; i < benchIters; i++ {
		refs := ExpandPostings(Without(
			mp.Postings("job", "api-server"),
			mp.Postings("status", "500"),
		))
		withoutCount = len(refs)
	}
	withoutDur := time.Since(start)

	fmt.Printf("  Intersect (job∩method): %d개 → %v (회당 %v)\n",
		intersectCount, intersectDur, intersectDur/benchIters)
	fmt.Printf("  Merge     (job∪method): %d개 → %v (회당 %v)\n",
		mergeCount, mergeDur, mergeDur/benchIters)
	fmt.Printf("  Without   (job\\status): %d개 → %v (회당 %v)\n",
		withoutCount, withoutDur, withoutDur/benchIters)
	fmt.Println()

	// 9c. 슬라이스 연산 vs 이터레이터 연산 비교
	fmt.Println("  9c. 슬라이스 직접 연산 vs Postings 이터레이터 비교")
	fmt.Println("  ──────────────────────────────────────────")

	sliceA := ExpandPostings(mp.Postings("job", "api-server"))
	sliceB := ExpandPostings(mp.Postings("method", "GET"))

	start = time.Now()
	for i := 0; i < benchIters; i++ {
		SliceIntersect(sliceA, sliceB)
	}
	sliceIntersectDur := time.Since(start)

	start = time.Now()
	for i := 0; i < benchIters; i++ {
		ExpandPostings(Intersect(
			mp.Postings("job", "api-server"),
			mp.Postings("method", "GET"),
		))
	}
	iterIntersectDur := time.Since(start)

	start = time.Now()
	for i := 0; i < benchIters; i++ {
		SliceMerge(sliceA, sliceB)
	}
	sliceMergeDur := time.Since(start)

	start = time.Now()
	for i := 0; i < benchIters; i++ {
		ExpandPostings(Merge(
			mp.Postings("job", "api-server"),
			mp.Postings("method", "GET"),
		))
	}
	iterMergeDur := time.Since(start)

	fmt.Printf("  Intersect  슬라이스: %v (회당 %v)\n", sliceIntersectDur, sliceIntersectDur/benchIters)
	fmt.Printf("  Intersect  이터레이터: %v (회당 %v)\n", iterIntersectDur, iterIntersectDur/benchIters)
	fmt.Printf("  Merge      슬라이스: %v (회당 %v)\n", sliceMergeDur, sliceMergeDur/benchIters)
	fmt.Printf("  Merge      이터레이터: %v (회당 %v)\n", iterMergeDur, iterMergeDur/benchIters)
	fmt.Println()

	// ─────────────────────────────────────────────────────────────────────
	// 10. 역인덱스가 없는 경우와 비교 (브루트포스)
	// ─────────────────────────────────────────────────────────────────────
	fmt.Println("[10] 역인덱스 vs 브루트포스 비교")
	fmt.Println("─────────────────────────────────────────────────────────")

	// 브루트포스: 모든 시리즈를 순회하면서 레이블 매칭
	type SeriesEntry struct {
		ref    SeriesRef
		labels Labels
	}
	allSeries := make([]SeriesEntry, 0, totalSeries)
	rng2 := rand.New(rand.NewSource(42)) // 동일한 시드로 재생성
	for i := 0; i < totalSeries; i++ {
		ref := SeriesRef(i + 1)
		lset := Labels{
			{Name: "__name__", Value: "http_requests_total"},
			{Name: "job", Value: jobs[rng2.Intn(len(jobs))]},
			{Name: "method", Value: methods[rng2.Intn(len(methods))]},
			{Name: "status", Value: statuses[rng2.Intn(len(statuses))]},
			{Name: "instance", Value: instances[rng2.Intn(len(instances))]},
			{Name: "path", Value: paths[rng2.Intn(len(paths))]},
		}
		allSeries = append(allSeries, SeriesEntry{ref: ref, labels: lset})
	}

	// 브루트포스: {job="api-server", method="GET"}
	start = time.Now()
	var bruteResult []SeriesRef
	for iter := 0; iter < benchIters; iter++ {
		bruteResult = bruteResult[:0]
		for _, s := range allSeries {
			jobMatch, methodMatch := false, false
			for _, l := range s.labels {
				if l.Name == "job" && l.Value == "api-server" {
					jobMatch = true
				}
				if l.Name == "method" && l.Value == "GET" {
					methodMatch = true
				}
			}
			if jobMatch && methodMatch {
				bruteResult = append(bruteResult, s.ref)
			}
		}
	}
	bruteDur := time.Since(start)

	// 역인덱스: 동일 쿼리
	start = time.Now()
	var indexResult []SeriesRef
	for iter := 0; iter < benchIters; iter++ {
		indexResult = ExpandPostings(Intersect(
			mp.Postings("job", "api-server"),
			mp.Postings("method", "GET"),
		))
	}
	indexDur := time.Since(start)

	fmt.Printf("  쿼리: {job=\"api-server\", method=\"GET\"}\n")
	fmt.Printf("  브루트포스: %d개 결과, %d회 반복 → %v (회당 %v)\n",
		len(bruteResult), benchIters, bruteDur, bruteDur/benchIters)
	fmt.Printf("  역인덱스:   %d개 결과, %d회 반복 → %v (회당 %v)\n",
		len(indexResult), benchIters, indexDur, indexDur/benchIters)
	if indexDur > 0 {
		fmt.Printf("  속도 향상: %.1fx 빠름\n", float64(bruteDur)/float64(indexDur))
	}
	fmt.Println()

	// ─────────────────────────────────────────────────────────────────────
	// 11. 카디널리티 분석
	// ─────────────────────────────────────────────────────────────────────
	fmt.Println("[11] 카디널리티 분석 (포스팅 리스트 크기 분포)")
	fmt.Println("─────────────────────────────────────────────────────────")
	fmt.Println()

	type PostingStat struct {
		label    string
		value    string
		count    int
	}

	var stats []PostingStat
	for _, name := range labelNames {
		for _, val := range mp.LabelValues(name) {
			refs := ExpandPostings(mp.Postings(name, val))
			stats = append(stats, PostingStat{label: name, value: val, count: len(refs)})
		}
	}

	sort.Slice(stats, func(i, j int) bool {
		return stats[i].count > stats[j].count
	})

	fmt.Println("  상위 10개 (가장 큰 포스팅 리스트):")
	fmt.Println("  ─────────────────────────────────────────────")
	for i := 0; i < 10 && i < len(stats); i++ {
		s := stats[i]
		barLen := s.count * 40 / totalSeries
		bar := strings.Repeat("█", barLen) + strings.Repeat("░", 40-barLen)
		fmt.Printf("  %-25s %5d │%s│\n",
			fmt.Sprintf("%s=%q", s.label, s.value), s.count, bar)
	}
	fmt.Println()

	fmt.Println("  하위 10개 (가장 작은 포스팅 리스트):")
	fmt.Println("  ─────────────────────────────────────────────")
	for i := len(stats) - 10; i < len(stats) && i >= 0; i++ {
		s := stats[i]
		barLen := s.count * 40 / totalSeries
		if barLen == 0 && s.count > 0 {
			barLen = 1
		}
		bar := strings.Repeat("█", barLen) + strings.Repeat("░", 40-barLen)
		fmt.Printf("  %-25s %5d │%s│\n",
			fmt.Sprintf("%s=%q", s.label, s.value), s.count, bar)
	}
	fmt.Println()

	// ─────────────────────────────────────────────────────────────────────
	// 12. 메모리 구조 시각화
	// ─────────────────────────────────────────────────────────────────────
	fmt.Println("[12] 역인덱스 메모리 구조 시각화")
	fmt.Println("─────────────────────────────────────────────────────────")
	fmt.Println()
	fmt.Println(`  MemPostings.m 구조:
  ┌─────────────────────────────────────────────────────────────────────────┐
  │ m["__name__"]["http_requests_total"] → [1, 2, 3, ..., 10000]          │
  │                                        (전체 시리즈 = AllPostings과 동일)│
  ├─────────────────────────────────────────────────────────────────────────┤
  │ m["job"]["api-server"]       → [3, 7, 12, 18, ...]  (정렬된 ref 목록)  │
  │ m["job"]["web-frontend"]     → [1, 5, 9, 15, ...]                     │
  │ m["job"]["payment-service"]  → [2, 8, 14, 22, ...]                    │
  │ m["job"]["auth-service"]     → [4, 11, 19, 25, ...]                   │
  │ m["job"]["data-pipeline"]    → [6, 10, 16, 23, ...]                   │
  ├─────────────────────────────────────────────────────────────────────────┤
  │ m["method"]["GET"]    → [1, 4, 8, 13, ...]                            │
  │ m["method"]["POST"]   → [2, 6, 11, 17, ...]                           │
  │ m["method"]["PUT"]    → [3, 9, 15, 21, ...]                           │
  │ ...                                                                    │
  ├─────────────────────────────────────────────────────────────────────────┤
  │ m["status"]["200"]  → [1, 3, 7, 12, ...]                              │
  │ m["status"]["500"]  → [5, 18, 33, 47, ...]                            │
  │ ...                                                                    │
  ├─────────────────────────────────────────────────────────────────────────┤
  │ m[""][""] (allPostingsKey) → [1, 2, 3, 4, 5, ..., 10000]              │
  └─────────────────────────────────────────────────────────────────────────┘

  쿼리 실행 흐름: {job="api-server", method="GET"}
  ┌─────────────────────────────────────────────────────────────────────────┐
  │ 1. Postings("job", "api-server") → [3, 7, 12, 18, 25, 31, ...]       │
  │ 2. Postings("method", "GET")     → [1, 4, 8, 13, 18, 22, ...]       │
  │ 3. Intersect(1, 2)              → [18, ...]  (Seek으로 O(n) 교집합)   │
  │                                                                        │
  │    리스트1:  3  7  12 [18] 25 31 ...                                   │
  │    리스트2:  1  4  8  13 [18] 22 ...                                   │
  │                         ↑↑                                             │
  │                      두 포인터가 같은 값 → 교집합에 포함                    │
  └─────────────────────────────────────────────────────────────────────────┘`)
	fmt.Println()

	// ─────────────────────────────────────────────────────────────────────
	fmt.Println("==========================================================")
	fmt.Println(" PoC 완료")
	fmt.Println("==========================================================")
}
