package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ============================================================================
// Prometheus TSDB 시계열 저장소 PoC
// ============================================================================
// Prometheus의 Head Block(인메모리 저장소)의 핵심 구조를 시뮬레이션한다.
//
// 실제 소스 참조:
//   - tsdb/head.go         → memSeries 구조체 (ref, lset, chunks)
//   - tsdb/index/postings.go → MemPostings (역색인: label→seriesRef)
//   - model/labels/labels_stringlabels.go → Labels (정렬된 이름-값 쌍)
//
// 핵심 설계 원리:
//   1. Labels는 정렬된 key-value 쌍으로, 시계열의 고유 식별자 역할을 한다.
//   2. MemPostings는 역색인(inverted index)으로, 각 label pair가
//      어떤 시계열(SeriesRef)에 속하는지 O(1) 룩업을 제공한다.
//   3. 쿼리 시 여러 matcher의 결과를 교집합(intersection)하여 시계열을 찾는다.
// ============================================================================

// ---------------------------------------------------------------------------
// Label & Labels: 시계열의 고유 식별자
// ---------------------------------------------------------------------------
// Prometheus에서 시계열은 metric name + label set으로 식별된다.
// 예: http_requests_total{method="GET", status="200"}
// 실제 구현(model/labels/labels_stringlabels.go)은 단일 string에 인코딩하지만,
// 여기서는 이해하기 쉽게 []Label 슬라이스로 표현한다.

// Label은 하나의 이름-값 쌍이다.
type Label struct {
	Name  string
	Value string
}

// Labels는 이름순으로 정렬된 Label 슬라이스다.
// Prometheus에서 Labels의 정렬은 필수 - Hash 비교, 동등성 판단에 사용된다.
type Labels []Label

// NewLabels는 key-value 쌍을 받아 정렬된 Labels를 생성한다.
func NewLabels(pairs ...string) Labels {
	if len(pairs)%2 != 0 {
		panic("labels: odd number of arguments")
	}
	ls := make(Labels, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		ls = append(ls, Label{Name: pairs[i], Value: pairs[i+1]})
	}
	sort.Slice(ls, func(i, j int) bool {
		return ls[i].Name < ls[j].Name
	})
	return ls
}

// String은 Labels를 Prometheus 형식으로 출력한다.
// 예: {__name__="http_requests_total", method="GET"}
func (ls Labels) String() string {
	var b strings.Builder
	b.WriteByte('{')
	for i, l := range ls {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s=%q", l.Name, l.Value)
	}
	b.WriteByte('}')
	return b.String()
}

// Hash는 Labels의 고유 키를 생성한다.
// 실제 Prometheus는 xxhash를 사용하지만, 여기서는 문자열 연결로 단순화한다.
func (ls Labels) Hash() string {
	var b strings.Builder
	for _, l := range ls {
		b.WriteString(l.Name)
		b.WriteByte(0xFF) // separator
		b.WriteString(l.Value)
		b.WriteByte(0xFF)
	}
	return b.String()
}

// Get은 주어진 이름의 label 값을 반환한다.
func (ls Labels) Get(name string) string {
	for _, l := range ls {
		if l.Name == name {
			return l.Value
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Sample: 하나의 데이터 포인트 (타임스탬프 + 값)
// ---------------------------------------------------------------------------

// Sample은 시계열의 단일 데이터 포인트다.
// Prometheus는 밀리초 단위 Unix 타임스탬프와 float64 값을 사용한다.
type Sample struct {
	Timestamp int64   // Unix 밀리초
	Value     float64
}

// ---------------------------------------------------------------------------
// SeriesRef: 시계열 참조 ID
// ---------------------------------------------------------------------------

// SeriesRef는 시계열의 내부 참조 번호다.
// 실제 Prometheus(tsdb/chunks/head_chunks.go)에서는 HeadSeriesRef 타입을 사용한다.
type SeriesRef uint64

// ---------------------------------------------------------------------------
// MemSeries: 인메모리 시계열
// ---------------------------------------------------------------------------
// 실제 구현(tsdb/head.go의 memSeries)은 chunks, OOO(Out-of-Order) 처리,
// mmapped chunks 등 복잡한 구조를 갖지만, 핵심은 labels + samples이다.
//
// 실제 필드:
//   ref        chunks.HeadSeriesRef  // 고유 참조 번호
//   lset       labels.Labels         // 레이블 셋
//   headChunks *memChunk             // 최신 청크 (linked list)
//   mmappedChunks []*mmappedChunk    // 디스크에 mmap된 청크들

type MemSeries struct {
	ref     SeriesRef
	lset    Labels
	samples []Sample // 실제로는 chunk 인코딩을 사용하지만 단순화
}

// ---------------------------------------------------------------------------
// MemPostings: 역색인 (Inverted Index)
// ---------------------------------------------------------------------------
// 핵심 자료구조: map[labelName]map[labelValue][]SeriesRef
//
// 실제 구현(tsdb/index/postings.go):
//   type MemPostings struct {
//       m   map[string]map[string][]storage.SeriesRef
//       lvs map[string][]string  // label values 캐시
//   }
//
// 예시:
//   "__name__" → "http_requests_total" → [1, 2, 3]
//   "method"   → "GET"                 → [1, 3]
//   "method"   → "POST"                → [2]
//
// 쿼리: __name__="http_requests_total" AND method="GET"
//   → intersection([1,2,3], [1,3]) = [1, 3]

type MemPostings struct {
	m map[string]map[string][]SeriesRef
}

// NewMemPostings는 빈 MemPostings를 생성한다.
func NewMemPostings() *MemPostings {
	return &MemPostings{
		m: make(map[string]map[string][]SeriesRef),
	}
}

// Add는 시계열의 모든 label pair에 대해 역색인을 추가한다.
// 실제 구현(postings.go:403)과 동일한 패턴:
//
//	func (p *MemPostings) Add(id storage.SeriesRef, lset labels.Labels) {
//	    lset.Range(func(l labels.Label) {
//	        p.addFor(id, l)
//	    })
//	    p.addFor(id, allPostingsKey)  // 전체 시계열 목록에도 추가
//	}
func (p *MemPostings) Add(ref SeriesRef, lset Labels) {
	for _, l := range lset {
		p.addFor(ref, l.Name, l.Value)
	}
	// allPostingsKey: 모든 시계열을 추적하는 특수 키
	p.addFor(ref, "", "")
}

func (p *MemPostings) addFor(ref SeriesRef, name, value string) {
	nm, ok := p.m[name]
	if !ok {
		nm = make(map[string][]SeriesRef)
		p.m[name] = nm
	}
	nm[value] = append(nm[value], ref)
}

// Get은 특정 label pair에 매칭되는 시계열 참조 목록을 반환한다.
func (p *MemPostings) Get(name, value string) []SeriesRef {
	nm, ok := p.m[name]
	if !ok {
		return nil
	}
	return nm[value]
}

// ---------------------------------------------------------------------------
// Matcher: 쿼리 매처
// ---------------------------------------------------------------------------

// MatchType은 매칭 방식을 정의한다.
type MatchType int

const (
	MatchEqual      MatchType = iota // =
	MatchNotEqual                    // !=
	MatchRegexp                      // =~
	MatchNotRegexp                   // !~
)

func (mt MatchType) String() string {
	switch mt {
	case MatchEqual:
		return "="
	case MatchNotEqual:
		return "!="
	case MatchRegexp:
		return "=~"
	case MatchNotRegexp:
		return "!~"
	default:
		return "?"
	}
}

// Matcher는 하나의 label 매칭 조건이다.
type Matcher struct {
	Type  MatchType
	Name  string
	Value string
	re    *regexp.Regexp // 정규식 매처용
}

// NewMatcher는 Matcher를 생성한다.
func NewMatcher(mt MatchType, name, value string) *Matcher {
	m := &Matcher{Type: mt, Name: name, Value: value}
	if mt == MatchRegexp || mt == MatchNotRegexp {
		// 정규식을 전체 매치로 컴파일 (Prometheus와 동일하게 ^...$)
		m.re = regexp.MustCompile("^(?:" + value + ")$")
	}
	return m
}

// Matches는 주어진 값이 매처 조건에 맞는지 확인한다.
func (m *Matcher) Matches(v string) bool {
	switch m.Type {
	case MatchEqual:
		return v == m.Value
	case MatchNotEqual:
		return v != m.Value
	case MatchRegexp:
		return m.re.MatchString(v)
	case MatchNotRegexp:
		return !m.re.MatchString(v)
	}
	return false
}

func (m *Matcher) String() string {
	return fmt.Sprintf("%s%s%q", m.Name, m.Type, m.Value)
}

// ---------------------------------------------------------------------------
// TSDB: 인메모리 시계열 데이터베이스
// ---------------------------------------------------------------------------
// 실제 Prometheus의 Head 구조(tsdb/head.go)를 단순화한 것이다.
//
// 실제 Head는 다음을 포함한다:
//   - series      *stripeSeries       // 시계열 맵 (샤딩된)
//   - postings    *index.MemPostings  // 역색인
//   - chunkPool   chunkenc.Pool       // 청크 풀
//   - wal         *wlog.WL            // Write-Ahead Log

type TSDB struct {
	series     map[SeriesRef]*MemSeries  // ref → 시계열
	hashToRef  map[string]SeriesRef      // labels hash → ref (중복 방지)
	postings   *MemPostings              // 역색인
	nextRef    SeriesRef                 // 다음 할당할 참조 번호
}

// NewTSDB는 빈 TSDB를 생성한다.
func NewTSDB() *TSDB {
	return &TSDB{
		series:   make(map[SeriesRef]*MemSeries),
		hashToRef: make(map[string]SeriesRef),
		postings: NewMemPostings(),
		nextRef:  1,
	}
}

// Append는 시계열에 샘플을 추가한다.
// 이미 존재하는 시계열이면 샘플만 추가하고, 없으면 새로 생성한다.
//
// 실제 Prometheus의 Append 흐름:
//   1. headAppender.Append() → labels hash 계산
//   2. head.getOrCreate() → 기존 시계열 찾기 또는 생성
//   3. memSeries에 샘플 추가 (chunk 인코딩)
//   4. postings에 역색인 추가
func (db *TSDB) Append(lset Labels, ts int64, v float64) SeriesRef {
	hash := lset.Hash()

	// 기존 시계열이 있으면 샘플만 추가
	if ref, ok := db.hashToRef[hash]; ok {
		s := db.series[ref]
		s.samples = append(s.samples, Sample{Timestamp: ts, Value: v})
		return ref
	}

	// 새 시계열 생성
	ref := db.nextRef
	db.nextRef++

	s := &MemSeries{
		ref:     ref,
		lset:    lset,
		samples: []Sample{{Timestamp: ts, Value: v}},
	}

	db.series[ref] = s
	db.hashToRef[hash] = ref

	// 역색인에 추가 - 모든 label pair에 대해 posting 등록
	db.postings.Add(ref, lset)

	return ref
}

// Select는 매처 조건에 맞는 시계열을 조회한다.
//
// 쿼리 실행 흐름:
//   1. 각 Equal 매처에 대해 postings.Get()으로 후보 시계열 목록 획득
//   2. 후보 목록들의 교집합(intersection) 계산
//   3. NotEqual, Regex 매처로 추가 필터링
//
// 실제 Prometheus에서는 postingsForMatcher() 함수가 이 역할을 한다.
// Regex 매처의 경우, 해당 label name의 모든 values를 순회하며
// 매칭되는 value들의 postings를 합집합(union)한다.
func (db *TSDB) Select(matchers ...*Matcher) []*MemSeries {
	if len(matchers) == 0 {
		// 매처가 없으면 모든 시계열 반환
		result := make([]*MemSeries, 0, len(db.series))
		for _, s := range db.series {
			result = append(result, s)
		}
		return result
	}

	// 1단계: 각 매처에 대해 후보 SeriesRef 집합을 구한다
	var sets []map[SeriesRef]struct{}

	for _, m := range matchers {
		candidates := make(map[SeriesRef]struct{})

		switch m.Type {
		case MatchEqual:
			// 역색인에서 직접 조회 - O(1) 룩업
			for _, ref := range db.postings.Get(m.Name, m.Value) {
				candidates[ref] = struct{}{}
			}

		case MatchNotEqual:
			// 해당 label name의 모든 값을 순회하되, 매치되지 않는 것만 포함
			if nm, ok := db.postings.m[m.Name]; ok {
				for val, refs := range nm {
					if val != m.Value {
						for _, ref := range refs {
							candidates[ref] = struct{}{}
						}
					}
				}
			}

		case MatchRegexp, MatchNotRegexp:
			// 정규식: 해당 label name의 모든 값을 순회하며 매칭
			// 실제 Prometheus도 동일한 방식 (postingsForMatcher 참조)
			if nm, ok := db.postings.m[m.Name]; ok {
				for val, refs := range nm {
					if m.Matches(val) {
						for _, ref := range refs {
							candidates[ref] = struct{}{}
						}
					}
				}
			}
		}

		sets = append(sets, candidates)
	}

	// 2단계: 모든 후보 집합의 교집합 계산
	// 실제 Prometheus는 sorted posting list의 merge를 사용하지만,
	// 여기서는 set intersection으로 단순화
	if len(sets) == 0 {
		return nil
	}

	intersection := sets[0]
	for i := 1; i < len(sets); i++ {
		next := make(map[SeriesRef]struct{})
		for ref := range intersection {
			if _, ok := sets[i][ref]; ok {
				next[ref] = struct{}{}
			}
		}
		intersection = next
	}

	// 3단계: 결과를 ref 순서로 정렬하여 반환
	refs := make([]SeriesRef, 0, len(intersection))
	for ref := range intersection {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i] < refs[j] })

	result := make([]*MemSeries, 0, len(refs))
	for _, ref := range refs {
		result = append(result, db.series[ref])
	}
	return result
}

// ---------------------------------------------------------------------------
// 유틸리티 함수
// ---------------------------------------------------------------------------

func printSeries(label string, series []*MemSeries) {
	fmt.Printf("\n%s (%d개 시계열)\n", label, len(series))
	fmt.Println(strings.Repeat("-", 70))
	for _, s := range series {
		fmt.Printf("  ref=%d  %s\n", s.ref, s.lset)
		for _, sp := range s.samples {
			t := time.UnixMilli(sp.Timestamp).Format("15:04:05.000")
			fmt.Printf("    @ %s → %.2f\n", t, sp.Value)
		}
	}
}

func printPostingsIndex(db *TSDB) {
	fmt.Println("\n역색인 (MemPostings) 내부 구조")
	fmt.Println(strings.Repeat("=", 70))
	fmt.Println("구조: map[labelName] → map[labelValue] → []SeriesRef")
	fmt.Println()

	// label name 정렬
	names := make([]string, 0, len(db.postings.m))
	for name := range db.postings.m {
		if name == "" {
			continue // allPostingsKey 스킵
		}
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		values := db.postings.m[name]
		fmt.Printf("  [%s]\n", name)
		vnames := make([]string, 0, len(values))
		for v := range values {
			vnames = append(vnames, v)
		}
		sort.Strings(vnames)
		for _, v := range vnames {
			refs := values[v]
			fmt.Printf("    %q → refs=%v\n", v, refs)
		}
	}

	// allPostingsKey
	if all, ok := db.postings.m[""]; ok {
		if refs, ok := all[""]; ok {
			fmt.Printf("\n  [allPostingsKey] → refs=%v (전체 시계열 %d개)\n", refs, len(refs))
		}
	}
}

// ---------------------------------------------------------------------------
// 메인 함수: 데모 실행
// ---------------------------------------------------------------------------

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║     Prometheus TSDB 시계열 저장소 PoC (In-Memory Head Block)        ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════════╝")

	db := NewTSDB()
	now := time.Now().UnixMilli()

	// ================================================================
	// 1. 데이터 삽입: http_requests_total 메트릭
	// ================================================================
	fmt.Println("\n[1] 데이터 삽입: http_requests_total")
	fmt.Println(strings.Repeat("=", 70))

	// 다양한 label 조합으로 시계열 생성
	httpLabels := []Labels{
		NewLabels("__name__", "http_requests_total", "method", "GET", "status", "200", "handler", "/api/v1/query"),
		NewLabels("__name__", "http_requests_total", "method", "GET", "status", "404", "handler", "/api/v1/query"),
		NewLabels("__name__", "http_requests_total", "method", "POST", "status", "200", "handler", "/api/v1/write"),
		NewLabels("__name__", "http_requests_total", "method", "POST", "status", "500", "handler", "/api/v1/write"),
		NewLabels("__name__", "http_requests_total", "method", "DELETE", "status", "200", "handler", "/api/v1/series"),
	}

	for i, lset := range httpLabels {
		// 시계열마다 3개의 샘플 추가 (15초 간격 - Prometheus 기본 scrape interval)
		for j := 0; j < 3; j++ {
			ts := now + int64(j*15000)
			val := float64((i+1)*100 + j*10)
			ref := db.Append(lset, ts, val)
			if j == 0 {
				fmt.Printf("  시계열 생성: ref=%d %s\n", ref, lset)
			}
		}
	}

	// ================================================================
	// 2. 데이터 삽입: cpu_usage 메트릭
	// ================================================================
	fmt.Println("\n[2] 데이터 삽입: cpu_usage")
	fmt.Println(strings.Repeat("=", 70))

	cpuLabels := []Labels{
		NewLabels("__name__", "cpu_usage", "instance", "node-1", "cpu", "0", "mode", "user"),
		NewLabels("__name__", "cpu_usage", "instance", "node-1", "cpu", "0", "mode", "system"),
		NewLabels("__name__", "cpu_usage", "instance", "node-1", "cpu", "1", "mode", "user"),
		NewLabels("__name__", "cpu_usage", "instance", "node-2", "cpu", "0", "mode", "user"),
	}

	for i, lset := range cpuLabels {
		for j := 0; j < 3; j++ {
			ts := now + int64(j*15000)
			val := float64(30+i*10) + float64(j)*0.5
			ref := db.Append(lset, ts, val)
			if j == 0 {
				fmt.Printf("  시계열 생성: ref=%d %s\n", ref, lset)
			}
		}
	}

	// ================================================================
	// 3. 역색인 구조 출력
	// ================================================================
	printPostingsIndex(db)

	// ================================================================
	// 4. 쿼리 테스트
	// ================================================================
	fmt.Println("\n")
	fmt.Println("╔══════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║                        쿼리 테스트                                  ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════════╝")

	// 쿼리 1: 정확한 매칭 (Equal)
	// http_requests_total{method="GET"}
	fmt.Println("\n쿼리 1: __name__=\"http_requests_total\" AND method=\"GET\"")
	fmt.Println("  → 역색인 룩업: postings[__name__][http_requests_total] ∩ postings[method][GET]")
	result := db.Select(
		NewMatcher(MatchEqual, "__name__", "http_requests_total"),
		NewMatcher(MatchEqual, "method", "GET"),
	)
	printSeries("  결과", result)

	// 쿼리 2: 정확한 매칭 (다중 조건)
	// http_requests_total{method="POST", status="500"}
	fmt.Println("\n쿼리 2: __name__=\"http_requests_total\" AND method=\"POST\" AND status=\"500\"")
	fmt.Println("  → 3개 posting list의 교집합")
	result = db.Select(
		NewMatcher(MatchEqual, "__name__", "http_requests_total"),
		NewMatcher(MatchEqual, "method", "POST"),
		NewMatcher(MatchEqual, "status", "500"),
	)
	printSeries("  결과", result)

	// 쿼리 3: 부정 매칭 (NotEqual)
	// http_requests_total{status!="200"}
	fmt.Println("\n쿼리 3: __name__=\"http_requests_total\" AND status!=\"200\"")
	fmt.Println("  → status의 모든 값 중 200이 아닌 posting list를 합집합 후 교집합")
	result = db.Select(
		NewMatcher(MatchEqual, "__name__", "http_requests_total"),
		NewMatcher(MatchNotEqual, "status", "200"),
	)
	printSeries("  결과", result)

	// 쿼리 4: 정규식 매칭 (Regexp)
	// http_requests_total{status=~"4.."}  → 4xx 에러만
	fmt.Println("\n쿼리 4: __name__=\"http_requests_total\" AND status=~\"4..\"")
	fmt.Println("  → status의 모든 값을 정규식 ^(4..)$로 매칭하여 posting list 합집합")
	result = db.Select(
		NewMatcher(MatchEqual, "__name__", "http_requests_total"),
		NewMatcher(MatchRegexp, "status", "4.."),
	)
	printSeries("  결과", result)

	// 쿼리 5: 정규식 매칭 (handler 패턴)
	// http_requests_total{handler=~"/api/v1/(query|write)"}
	fmt.Println("\n쿼리 5: __name__=\"http_requests_total\" AND handler=~\"/api/v1/(query|write)\" AND method=\"GET\"")
	result = db.Select(
		NewMatcher(MatchEqual, "__name__", "http_requests_total"),
		NewMatcher(MatchRegexp, "handler", "/api/v1/(query|write)"),
		NewMatcher(MatchEqual, "method", "GET"),
	)
	printSeries("  결과", result)

	// 쿼리 6: cpu_usage 메트릭 조회
	// cpu_usage{instance="node-1", mode="user"}
	fmt.Println("\n쿼리 6: __name__=\"cpu_usage\" AND instance=\"node-1\" AND mode=\"user\"")
	result = db.Select(
		NewMatcher(MatchEqual, "__name__", "cpu_usage"),
		NewMatcher(MatchEqual, "instance", "node-1"),
		NewMatcher(MatchEqual, "mode", "user"),
	)
	printSeries("  결과", result)

	// 쿼리 7: 부정 정규식 (NotRegexp)
	// cpu_usage{mode!~"system"}
	fmt.Println("\n쿼리 7: __name__=\"cpu_usage\" AND mode!~\"system\"")
	result = db.Select(
		NewMatcher(MatchEqual, "__name__", "cpu_usage"),
		NewMatcher(MatchNotRegexp, "mode", "system"),
	)
	printSeries("  결과", result)

	// ================================================================
	// 5. 저장소 통계
	// ================================================================
	fmt.Println("\n")
	fmt.Println("╔══════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║                       저장소 통계                                    ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════════╝")

	totalSamples := 0
	for _, s := range db.series {
		totalSamples += len(s.samples)
	}

	labelNames := make(map[string]struct{})
	labelPairs := 0
	for name, values := range db.postings.m {
		if name == "" {
			continue
		}
		labelNames[name] = struct{}{}
		labelPairs += len(values)
	}

	fmt.Printf("\n  총 시계열 수:     %d\n", len(db.series))
	fmt.Printf("  총 샘플 수:       %d\n", totalSamples)
	fmt.Printf("  고유 label 이름:  %d\n", len(labelNames))
	fmt.Printf("  역색인 엔트리:    %d (label name-value 쌍)\n", labelPairs)
	fmt.Printf("  다음 SeriesRef:   %d\n", db.nextRef)

	fmt.Println("\n[완료] Prometheus TSDB 시계열 저장소 PoC 실행 완료")
}
