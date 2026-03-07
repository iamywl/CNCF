package main

import (
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// =============================================================================
// Loki TSDB 인덱스 시뮬레이션
// =============================================================================
//
// Loki는 TSDB(Time Series Database) 스타일의 역인덱스(Inverted Index)를 사용하여
// 레이블 기반으로 로그 스트림을 빠르게 검색한다. Prometheus의 TSDB 인덱스 설계를
// 기반으로 하며, 로그 스트림에 최적화되어 있다.
//
// 핵심 구조:
//   1. Series(시리즈): 고유한 레이블 세트를 가진 로그 스트림
//   2. Fingerprint: 레이블 세트의 해시값 (시리즈 식별자)
//   3. Posting List: 특정 레이블 값을 가진 시리즈들의 fingerprint 목록
//   4. Inverted Index: 레이블 이름:값 → posting list 매핑
//   5. Label Matchers: 쿼리 시 사용하는 레이블 매칭 조건
//
// 쿼리 처리:
//   {app="nginx", level="error"} 쿼리는:
//   1. app="nginx"의 posting list: [fp1, fp3, fp7]
//   2. level="error"의 posting list: [fp2, fp3, fp9]
//   3. 교집합(intersection): [fp3]
//   4. fp3의 시리즈 메타데이터 조회
//
// Loki 실제 구현 참조:
//   - pkg/storage/stores/shipper/indexshipper/tsdb/index/index.go
//   - pkg/storage/stores/shipper/indexshipper/tsdb/index/postings.go
//   - pkg/storage/stores/shipper/indexshipper/tsdb/single_file_index.go
// =============================================================================

// Fingerprint는 시리즈의 고유 식별자이다.
// 레이블 세트를 해시하여 생성한다.
type Fingerprint uint64

// Labels는 정렬된 레이블 쌍의 목록이다.
type Labels []Label

// Label은 이름-값 쌍이다.
type Label struct {
	Name  string
	Value string
}

// String은 Labels를 문자열로 변환한다.
func (ls Labels) String() string {
	parts := make([]string, len(ls))
	for i, l := range ls {
		parts[i] = fmt.Sprintf(`%s="%s"`, l.Name, l.Value)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// Fingerprint는 Labels의 해시값을 계산한다.
func (ls Labels) Fingerprint() Fingerprint {
	h := md5.New()
	for _, l := range ls {
		h.Write([]byte(l.Name))
		h.Write([]byte{0})
		h.Write([]byte(l.Value))
		h.Write([]byte{0})
	}
	sum := h.Sum(nil)
	return Fingerprint(binary.BigEndian.Uint64(sum[:8]))
}

// Series는 고유한 레이블 세트를 가진 하나의 시계열(로그 스트림)이다.
type Series struct {
	Labels      Labels
	Fingerprint Fingerprint
	ChunkRefs   []ChunkRef // 이 시리즈에 속한 청크 참조들
}

// ChunkRef는 청크에 대한 참조이다.
type ChunkRef struct {
	From    time.Time // 청크 시작 시간
	Through time.Time // 청크 종료 시간
	ChunkID string    // 청크 식별자
}

// =============================================================================
// Posting List — 역인덱스의 핵심
// =============================================================================

// PostingList는 특정 레이블 값을 가진 시리즈들의 fingerprint 목록이다.
// 정렬된 상태를 유지하여 교집합/합집합 연산을 O(n+m)으로 수행할 수 있다.
type PostingList struct {
	fps []Fingerprint // 정렬된 fingerprint 목록
}

// NewPostingList는 새 PostingList를 생성한다.
func NewPostingList() *PostingList {
	return &PostingList{}
}

// Add는 fingerprint를 posting list에 추가한다.
func (pl *PostingList) Add(fp Fingerprint) {
	// 이미 존재하는지 확인 (이진 검색)
	idx := sort.Search(len(pl.fps), func(i int) bool {
		return pl.fps[i] >= fp
	})
	if idx < len(pl.fps) && pl.fps[idx] == fp {
		return // 이미 존재
	}
	// 정렬된 위치에 삽입
	pl.fps = append(pl.fps, 0)
	copy(pl.fps[idx+1:], pl.fps[idx:])
	pl.fps[idx] = fp
}

// Contains는 fingerprint가 posting list에 포함되어 있는지 확인한다.
func (pl *PostingList) Contains(fp Fingerprint) bool {
	idx := sort.Search(len(pl.fps), func(i int) bool {
		return pl.fps[i] >= fp
	})
	return idx < len(pl.fps) && pl.fps[idx] == fp
}

// Len은 posting list의 크기를 반환한다.
func (pl *PostingList) Len() int {
	return len(pl.fps)
}

// Intersect는 두 posting list의 교집합을 반환한다.
// 두 리스트가 정렬되어 있으므로 O(n+m) 시간에 수행 가능하다.
// Loki의 postings.go Intersect()와 동일한 알고리즘이다.
func Intersect(a, b *PostingList) *PostingList {
	result := NewPostingList()
	i, j := 0, 0
	for i < len(a.fps) && j < len(b.fps) {
		if a.fps[i] == b.fps[j] {
			result.fps = append(result.fps, a.fps[i])
			i++
			j++
		} else if a.fps[i] < b.fps[j] {
			i++
		} else {
			j++
		}
	}
	return result
}

// Union은 두 posting list의 합집합을 반환한다.
func Union(a, b *PostingList) *PostingList {
	result := NewPostingList()
	i, j := 0, 0
	for i < len(a.fps) && j < len(b.fps) {
		if a.fps[i] == b.fps[j] {
			result.fps = append(result.fps, a.fps[i])
			i++
			j++
		} else if a.fps[i] < b.fps[j] {
			result.fps = append(result.fps, a.fps[i])
			i++
		} else {
			result.fps = append(result.fps, b.fps[j])
			j++
		}
	}
	for ; i < len(a.fps); i++ {
		result.fps = append(result.fps, a.fps[i])
	}
	for ; j < len(b.fps); j++ {
		result.fps = append(result.fps, b.fps[j])
	}
	return result
}

// Subtract는 a에서 b를 빼는 차집합을 반환한다 (a - b).
func Subtract(a, b *PostingList) *PostingList {
	result := NewPostingList()
	i, j := 0, 0
	for i < len(a.fps) {
		if j >= len(b.fps) || a.fps[i] < b.fps[j] {
			result.fps = append(result.fps, a.fps[i])
			i++
		} else if a.fps[i] == b.fps[j] {
			i++
			j++
		} else {
			j++
		}
	}
	return result
}

// =============================================================================
// Label Matchers — 쿼리 조건
// =============================================================================

// MatchType은 레이블 매칭 유형이다.
type MatchType int

const (
	MatchEqual    MatchType = iota // =  정확히 일치
	MatchNotEqual                  // != 불일치
	MatchRegexp                    // =~ 정규식 매칭
	MatchNotRegexp                 // !~ 정규식 불일치
)

// String은 MatchType을 문자열로 변환한다.
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

// Matcher는 레이블 매칭 조건이다.
type Matcher struct {
	Type  MatchType
	Name  string
	Value string
	re    *regexp.Regexp // 정규식 매칭용
}

// NewMatcher는 새 Matcher를 생성한다.
func NewMatcher(t MatchType, name, value string) *Matcher {
	m := &Matcher{Type: t, Name: name, Value: value}
	if t == MatchRegexp || t == MatchNotRegexp {
		m.re = regexp.MustCompile("^(?:" + value + ")$")
	}
	return m
}

// Matches는 주어진 값이 매처 조건에 부합하는지 확인한다.
func (m *Matcher) Matches(value string) bool {
	switch m.Type {
	case MatchEqual:
		return value == m.Value
	case MatchNotEqual:
		return value != m.Value
	case MatchRegexp:
		return m.re.MatchString(value)
	case MatchNotRegexp:
		return !m.re.MatchString(value)
	}
	return false
}

// String은 Matcher를 문자열로 변환한다.
func (m *Matcher) String() string {
	return fmt.Sprintf(`%s%s"%s"`, m.Name, m.Type, m.Value)
}

// =============================================================================
// TSDB Index — 역인덱스 + 시리즈 저장소
// =============================================================================

// TSDBIndex는 Loki의 TSDB 인덱스를 구현한다.
type TSDBIndex struct {
	// 역인덱스: "labelName\xlabelValue" → PostingList
	invertedIndex map[string]*PostingList

	// 시리즈 저장소: Fingerprint → Series
	series map[Fingerprint]*Series

	// 레이블 이름 목록 (유니크)
	labelNames map[string]bool

	// 레이블 값 목록: labelName → 가능한 값들
	labelValues map[string]map[string]bool

	// 통계
	totalSeries  int
	totalChunks  int
}

// NewTSDBIndex는 새 TSDBIndex를 생성한다.
func NewTSDBIndex() *TSDBIndex {
	return &TSDBIndex{
		invertedIndex: make(map[string]*PostingList),
		series:        make(map[Fingerprint]*Series),
		labelNames:    make(map[string]bool),
		labelValues:   make(map[string]map[string]bool),
	}
}

// postingKey는 역인덱스의 키를 생성한다.
func postingKey(name, value string) string {
	return name + "\x00" + value
}

// AddSeries는 시리즈를 인덱스에 추가한다.
func (idx *TSDBIndex) AddSeries(labels Labels, chunks []ChunkRef) {
	// 레이블 정렬
	sort.Slice(labels, func(i, j int) bool {
		return labels[i].Name < labels[j].Name
	})

	fp := labels.Fingerprint()

	// 이미 존재하면 청크만 추가
	if s, ok := idx.series[fp]; ok {
		s.ChunkRefs = append(s.ChunkRefs, chunks...)
		idx.totalChunks += len(chunks)
		return
	}

	// 새 시리즈 등록
	s := &Series{
		Labels:      labels,
		Fingerprint: fp,
		ChunkRefs:   chunks,
	}
	idx.series[fp] = s
	idx.totalSeries++
	idx.totalChunks += len(chunks)

	// 역인덱스에 추가
	for _, l := range labels {
		key := postingKey(l.Name, l.Value)
		pl, ok := idx.invertedIndex[key]
		if !ok {
			pl = NewPostingList()
			idx.invertedIndex[key] = pl
		}
		pl.Add(fp)

		// 레이블 이름/값 추적
		idx.labelNames[l.Name] = true
		if _, ok := idx.labelValues[l.Name]; !ok {
			idx.labelValues[l.Name] = make(map[string]bool)
		}
		idx.labelValues[l.Name][l.Value] = true
	}
}

// Lookup은 매처 목록으로 시리즈를 검색한다.
// 각 매처에 대한 posting list를 구하고 교집합을 취한다.
func (idx *TSDBIndex) Lookup(matchers ...*Matcher) []*Series {
	if len(matchers) == 0 {
		return nil
	}

	var resultPostings *PostingList

	for _, m := range matchers {
		var matchPostings *PostingList

		switch m.Type {
		case MatchEqual:
			// 정확히 일치: 해당 posting list를 직접 가져옴
			key := postingKey(m.Name, m.Value)
			if pl, ok := idx.invertedIndex[key]; ok {
				matchPostings = pl
			} else {
				// 일치하는 값 없음 → 빈 결과
				return nil
			}

		case MatchNotEqual:
			// 불일치: 전체 시리즈에서 일치하는 것을 빼기
			// 1. 해당 레이블 이름을 가진 모든 시리즈
			allWithLabel := NewPostingList()
			if values, ok := idx.labelValues[m.Name]; ok {
				for val := range values {
					key := postingKey(m.Name, val)
					if pl, ok := idx.invertedIndex[key]; ok {
						allWithLabel = Union(allWithLabel, pl)
					}
				}
			}
			// 2. 일치하는 시리즈
			equalKey := postingKey(m.Name, m.Value)
			equalPostings := idx.invertedIndex[equalKey]
			if equalPostings == nil {
				equalPostings = NewPostingList()
			}
			// 3. 차집합
			matchPostings = Subtract(allWithLabel, equalPostings)

		case MatchRegexp:
			// 정규식: 매칭되는 모든 값의 posting list 합집합
			matchPostings = NewPostingList()
			if values, ok := idx.labelValues[m.Name]; ok {
				for val := range values {
					if m.Matches(val) {
						key := postingKey(m.Name, val)
						if pl, ok := idx.invertedIndex[key]; ok {
							matchPostings = Union(matchPostings, pl)
						}
					}
				}
			}

		case MatchNotRegexp:
			// 부정 정규식: 전체에서 매칭되는 것을 빼기
			allWithLabel := NewPostingList()
			regexMatched := NewPostingList()
			if values, ok := idx.labelValues[m.Name]; ok {
				for val := range values {
					key := postingKey(m.Name, val)
					if pl, ok := idx.invertedIndex[key]; ok {
						allWithLabel = Union(allWithLabel, pl)
						if m.re.MatchString(val) {
							regexMatched = Union(regexMatched, pl)
						}
					}
				}
			}
			matchPostings = Subtract(allWithLabel, regexMatched)
		}

		// 교집합 계산
		if resultPostings == nil {
			resultPostings = matchPostings
		} else {
			resultPostings = Intersect(resultPostings, matchPostings)
		}

		// 조기 종료: 교집합이 비어있으면 더 이상 진행할 필요 없음
		if resultPostings.Len() == 0 {
			return nil
		}
	}

	// Fingerprint로 시리즈 조회
	result := make([]*Series, 0, resultPostings.Len())
	for _, fp := range resultPostings.fps {
		if s, ok := idx.series[fp]; ok {
			result = append(result, s)
		}
	}
	return result
}

// LabelNames는 인덱스에 있는 모든 레이블 이름을 반환한다.
func (idx *TSDBIndex) LabelNames() []string {
	names := make([]string, 0, len(idx.labelNames))
	for name := range idx.labelNames {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// LabelValues는 특정 레이블 이름의 모든 값을 반환한다.
func (idx *TSDBIndex) LabelValues(name string) []string {
	values, ok := idx.labelValues[name]
	if !ok {
		return nil
	}
	result := make([]string, 0, len(values))
	for v := range values {
		result = append(result, v)
	}
	sort.Strings(result)
	return result
}

// PrintStats는 인덱스 통계를 출력한다.
func (idx *TSDBIndex) PrintStats() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║                    TSDB 인덱스 통계                          ║")
	fmt.Println("╠══════════════════════════════════════════════════════════════╣")
	fmt.Printf("║  시리즈 수:          %-38d ║\n", idx.totalSeries)
	fmt.Printf("║  청크 참조 수:       %-38d ║\n", idx.totalChunks)
	fmt.Printf("║  역인덱스 항목 수:   %-38d ║\n", len(idx.invertedIndex))
	fmt.Printf("║  레이블 이름 수:     %-38d ║\n", len(idx.labelNames))
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
}

func main() {
	fmt.Println("=================================================================")
	fmt.Println("  Loki TSDB 인덱스 시뮬레이션")
	fmt.Println("  - 역인덱스 (Inverted Index)")
	fmt.Println("  - Posting List 교집합/합집합")
	fmt.Println("  - Label Matchers (=, !=, =~, !~)")
	fmt.Println("=================================================================")
	fmt.Println()

	// =========================================================================
	// 인덱스 구축
	// =========================================================================
	idx := NewTSDBIndex()
	baseTime := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)

	// 시리즈 데이터 생성 (실제 Loki 환경을 모사)
	type seriesData struct {
		labels Labels
		chunks []ChunkRef
	}

	testSeries := []seriesData{
		{
			labels: Labels{
				{Name: "app", Value: "nginx"},
				{Name: "level", Value: "info"},
				{Name: "namespace", Value: "production"},
				{Name: "pod", Value: "nginx-abc12"},
			},
			chunks: []ChunkRef{
				{From: baseTime, Through: baseTime.Add(1 * time.Hour), ChunkID: "chunk-001"},
				{From: baseTime.Add(1 * time.Hour), Through: baseTime.Add(2 * time.Hour), ChunkID: "chunk-002"},
			},
		},
		{
			labels: Labels{
				{Name: "app", Value: "nginx"},
				{Name: "level", Value: "error"},
				{Name: "namespace", Value: "production"},
				{Name: "pod", Value: "nginx-abc12"},
			},
			chunks: []ChunkRef{
				{From: baseTime, Through: baseTime.Add(1 * time.Hour), ChunkID: "chunk-003"},
			},
		},
		{
			labels: Labels{
				{Name: "app", Value: "nginx"},
				{Name: "level", Value: "warn"},
				{Name: "namespace", Value: "staging"},
				{Name: "pod", Value: "nginx-def34"},
			},
			chunks: []ChunkRef{
				{From: baseTime, Through: baseTime.Add(2 * time.Hour), ChunkID: "chunk-004"},
			},
		},
		{
			labels: Labels{
				{Name: "app", Value: "api-server"},
				{Name: "level", Value: "info"},
				{Name: "namespace", Value: "production"},
				{Name: "pod", Value: "api-xyz99"},
			},
			chunks: []ChunkRef{
				{From: baseTime, Through: baseTime.Add(1 * time.Hour), ChunkID: "chunk-005"},
				{From: baseTime.Add(1 * time.Hour), Through: baseTime.Add(3 * time.Hour), ChunkID: "chunk-006"},
			},
		},
		{
			labels: Labels{
				{Name: "app", Value: "api-server"},
				{Name: "level", Value: "error"},
				{Name: "namespace", Value: "production"},
				{Name: "pod", Value: "api-xyz99"},
			},
			chunks: []ChunkRef{
				{From: baseTime.Add(30 * time.Minute), Through: baseTime.Add(1 * time.Hour), ChunkID: "chunk-007"},
			},
		},
		{
			labels: Labels{
				{Name: "app", Value: "worker"},
				{Name: "level", Value: "info"},
				{Name: "namespace", Value: "production"},
				{Name: "pod", Value: "worker-qqq11"},
			},
			chunks: []ChunkRef{
				{From: baseTime, Through: baseTime.Add(4 * time.Hour), ChunkID: "chunk-008"},
			},
		},
		{
			labels: Labels{
				{Name: "app", Value: "worker"},
				{Name: "level", Value: "error"},
				{Name: "namespace", Value: "staging"},
				{Name: "pod", Value: "worker-rrr22"},
			},
			chunks: []ChunkRef{
				{From: baseTime, Through: baseTime.Add(1 * time.Hour), ChunkID: "chunk-009"},
			},
		},
		{
			labels: Labels{
				{Name: "app", Value: "db"},
				{Name: "level", Value: "warn"},
				{Name: "namespace", Value: "production"},
				{Name: "pod", Value: "postgres-main"},
			},
			chunks: []ChunkRef{
				{From: baseTime, Through: baseTime.Add(6 * time.Hour), ChunkID: "chunk-010"},
			},
		},
		{
			labels: Labels{
				{Name: "app", Value: "cache"},
				{Name: "level", Value: "info"},
				{Name: "namespace", Value: "production"},
				{Name: "pod", Value: "redis-master"},
			},
			chunks: []ChunkRef{
				{From: baseTime, Through: baseTime.Add(2 * time.Hour), ChunkID: "chunk-011"},
			},
		},
		{
			labels: Labels{
				{Name: "app", Value: "cache"},
				{Name: "level", Value: "error"},
				{Name: "namespace", Value: "staging"},
				{Name: "pod", Value: "redis-slave"},
			},
			chunks: []ChunkRef{
				{From: baseTime, Through: baseTime.Add(1 * time.Hour), ChunkID: "chunk-012"},
			},
		},
	}

	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println(" 인덱스 구축")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	for _, sd := range testSeries {
		idx.AddSeries(sd.labels, sd.chunks)
		fmt.Printf("  추가: %s (청크 %d개)\n", sd.labels.String(), len(sd.chunks))
	}
	fmt.Println()
	idx.PrintStats()

	// =========================================================================
	// 시나리오 1: 역인덱스 구조 확인
	// =========================================================================
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println(" 시나리오 1: 역인덱스 구조 확인")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	// 레이블 이름 목록
	fmt.Println("  레이블 이름:", strings.Join(idx.LabelNames(), ", "))
	fmt.Println()

	// 각 레이블의 값과 posting list 크기
	for _, name := range idx.LabelNames() {
		values := idx.LabelValues(name)
		fmt.Printf("  %s:\n", name)
		for _, val := range values {
			key := postingKey(name, val)
			pl := idx.invertedIndex[key]
			fmt.Printf("    %-20s → posting list 크기: %d\n",
				fmt.Sprintf(`"%s"`, val), pl.Len())
		}
		fmt.Println()
	}

	// =========================================================================
	// 시나리오 2: 기본 쿼리 (Equal 매처)
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println(" 시나리오 2: 기본 쿼리 (= 매처)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	queries := []struct {
		name     string
		matchers []*Matcher
	}{
		{
			name: `{app="nginx"}`,
			matchers: []*Matcher{
				NewMatcher(MatchEqual, "app", "nginx"),
			},
		},
		{
			name: `{app="nginx", level="error"}`,
			matchers: []*Matcher{
				NewMatcher(MatchEqual, "app", "nginx"),
				NewMatcher(MatchEqual, "level", "error"),
			},
		},
		{
			name: `{namespace="production", level="error"}`,
			matchers: []*Matcher{
				NewMatcher(MatchEqual, "namespace", "production"),
				NewMatcher(MatchEqual, "level", "error"),
			},
		},
		{
			name: `{app="nonexistent"}`,
			matchers: []*Matcher{
				NewMatcher(MatchEqual, "app", "nonexistent"),
			},
		},
	}

	for _, q := range queries {
		results := idx.Lookup(q.matchers...)
		fmt.Printf("  쿼리: %s\n", q.name)
		if len(results) == 0 {
			fmt.Println("    결과: (없음)")
		} else {
			fmt.Printf("    결과: %d개 시리즈\n", len(results))
			for _, s := range results {
				fmt.Printf("      fp=0x%016X  %s  (청크 %d개)\n",
					s.Fingerprint, s.Labels.String(), len(s.ChunkRefs))
			}
		}
		fmt.Println()
	}

	// =========================================================================
	// 시나리오 3: NotEqual 매처
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println(" 시나리오 3: NotEqual (!=) 매처")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	neQueries := []struct {
		name     string
		matchers []*Matcher
	}{
		{
			name: `{app="nginx", level!="info"}`,
			matchers: []*Matcher{
				NewMatcher(MatchEqual, "app", "nginx"),
				NewMatcher(MatchNotEqual, "level", "info"),
			},
		},
		{
			name: `{namespace!="staging"}`,
			matchers: []*Matcher{
				NewMatcher(MatchNotEqual, "namespace", "staging"),
			},
		},
	}

	for _, q := range neQueries {
		results := idx.Lookup(q.matchers...)
		fmt.Printf("  쿼리: %s\n", q.name)
		fmt.Printf("    결과: %d개 시리즈\n", len(results))
		for _, s := range results {
			fmt.Printf("      %s\n", s.Labels.String())
		}
		fmt.Println()
	}

	// =========================================================================
	// 시나리오 4: 정규식 매처
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println(" 시나리오 4: 정규식 (=~, !~) 매처")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	reQueries := []struct {
		name     string
		matchers []*Matcher
	}{
		{
			name: `{app=~"nginx|api-server"}`,
			matchers: []*Matcher{
				NewMatcher(MatchRegexp, "app", "nginx|api-server"),
			},
		},
		{
			name: `{app=~".*er", level="error"}`,
			matchers: []*Matcher{
				NewMatcher(MatchRegexp, "app", ".*er"),
				NewMatcher(MatchEqual, "level", "error"),
			},
		},
		{
			name: `{app!~"nginx|cache", namespace="production"}`,
			matchers: []*Matcher{
				NewMatcher(MatchNotRegexp, "app", "nginx|cache"),
				NewMatcher(MatchEqual, "namespace", "production"),
			},
		},
	}

	for _, q := range reQueries {
		results := idx.Lookup(q.matchers...)
		fmt.Printf("  쿼리: %s\n", q.name)
		fmt.Printf("    결과: %d개 시리즈\n", len(results))
		for _, s := range results {
			fmt.Printf("      %s\n", s.Labels.String())
		}
		fmt.Println()
	}

	// =========================================================================
	// 시나리오 5: Posting List 연산 시각화
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println(" 시나리오 5: Posting List 연산 시각화")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	// 예시: {app="nginx", namespace="production"} 쿼리 과정
	fmt.Println(`  쿼리: {app="nginx", namespace="production"}`)
	fmt.Println()

	plApp := idx.invertedIndex[postingKey("app", "nginx")]
	plNs := idx.invertedIndex[postingKey("namespace", "production")]

	fmt.Printf("  1단계: app=\"nginx\" → posting list (%d개):\n", plApp.Len())
	for _, fp := range plApp.fps {
		s := idx.series[fp]
		fmt.Printf("    fp=0x%016X  %s\n", fp, s.Labels.String())
	}
	fmt.Println()

	fmt.Printf("  2단계: namespace=\"production\" → posting list (%d개):\n", plNs.Len())
	for _, fp := range plNs.fps {
		s := idx.series[fp]
		fmt.Printf("    fp=0x%016X  %s\n", fp, s.Labels.String())
	}
	fmt.Println()

	intersected := Intersect(plApp, plNs)
	fmt.Printf("  3단계: 교집합 (Intersect) → 결과 (%d개):\n", intersected.Len())
	for _, fp := range intersected.fps {
		s := idx.series[fp]
		fmt.Printf("    fp=0x%016X  %s\n", fp, s.Labels.String())
	}

	// =========================================================================
	// 시나리오 6: 쿼리 성능 특성
	// =========================================================================
	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println(" 시나리오 6: 인덱스 쿼리 성능 특성")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	fmt.Println("  ┌────────────────────────┬────────────────────────────────────┐")
	fmt.Println("  │ 연산                   │ 시간 복잡도                        │")
	fmt.Println("  ├────────────────────────┼────────────────────────────────────┤")
	fmt.Println("  │ Posting list 조회      │ O(1) - 해시맵 조회                 │")
	fmt.Println("  │ Posting list 교집합    │ O(n+m) - 정렬된 리스트 병합         │")
	fmt.Println("  │ Posting list 합집합    │ O(n+m) - 정렬된 리스트 병합         │")
	fmt.Println("  │ 시리즈 조회 (by fp)    │ O(1) - 해시맵 조회                 │")
	fmt.Println("  │ 레이블 값 목록         │ O(k) - k = 고유 값 수              │")
	fmt.Println("  │ 정규식 매칭            │ O(v*r) - v=값 수, r=정규식 비용     │")
	fmt.Println("  └────────────────────────┴────────────────────────────────────┘")
	fmt.Println()

	// 구체적인 크기 정보
	fmt.Println("  현재 인덱스 크기:")
	totalPostings := 0
	maxPostingSize := 0
	for _, pl := range idx.invertedIndex {
		totalPostings += pl.Len()
		if pl.Len() > maxPostingSize {
			maxPostingSize = pl.Len()
		}
	}
	fmt.Printf("    역인덱스 항목: %d\n", len(idx.invertedIndex))
	fmt.Printf("    전체 posting 수: %d\n", totalPostings)
	fmt.Printf("    최대 posting list 크기: %d\n", maxPostingSize)
	fmt.Printf("    시리즈 수: %d\n", idx.totalSeries)
	fmt.Printf("    청크 참조 수: %d\n", idx.totalChunks)

	fmt.Println()
	fmt.Println("=================================================================")
	fmt.Println("  시뮬레이션 완료")
	fmt.Println()
	fmt.Println("  TSDB 역인덱스 구조:")
	fmt.Println()
	fmt.Println("  레이블 쌍              Posting List        시리즈")
	fmt.Println("  ─────────────────      ────────────        ──────")
	fmt.Println(`  app="nginx"        →  [fp1, fp2, fp3]  →  Series 정보`)
	fmt.Println(`  app="api-server"   →  [fp4, fp5]       →  Series 정보`)
	fmt.Println(`  level="error"      →  [fp2, fp5, fp7]  →  Series 정보`)
	fmt.Println(`  level="info"       →  [fp1, fp4, fp6]  →  Series 정보`)
	fmt.Println()
	fmt.Println(`  쿼리 {app="nginx", level="error"}:`)
	fmt.Println("    1. app=nginx    → [fp1, fp2, fp3]")
	fmt.Println("    2. level=error  → [fp2, fp5, fp7]")
	fmt.Println("    3. Intersect    → [fp2]")
	fmt.Println("    4. Series 조회 → 해당 시리즈 반환")
	fmt.Println()
	fmt.Println("  핵심 포인트:")
	fmt.Println("  1. 역인덱스로 레이블 값 → 시리즈를 O(1)로 조회")
	fmt.Println("  2. Posting list가 정렬되어 교집합/합집합이 O(n+m)")
	fmt.Println("  3. 매처 유형(=, !=, =~, !~)에 따라 최적화된 검색 전략")
	fmt.Println("  4. 여러 매처의 교집합으로 검색 범위를 점진적 축소")
	fmt.Println("  5. Cardinality가 낮은 매처를 먼저 평가하면 더 효율적")
	fmt.Println("=================================================================")
}
