// poc-16-fingerprint: Jenkins Fingerprint(아티팩트 추적) 시스템 시뮬레이션
//
// Jenkins 소스코드 참조:
//   - hudson.model.Fingerprint (MD5 기반 파일 추적, BuildPtr, RangeSet, Range)
//   - hudson.model.Fingerprint.BuildPtr (빌드 참조 포인터: name, number)
//   - hudson.model.Fingerprint.Range (빌드 번호 범위 [start, end), 불변)
//   - hudson.model.Fingerprint.RangeSet (Range의 정렬된 집합, 가변)
//   - hudson.model.FingerprintMap (KeyedDataStorage<Fingerprint>, 전역 캐시)
//   - jenkins.fingerprints.FingerprintStorage (플러거블 저장소 API)
//   - jenkins.fingerprints.FileFingerprintStorage (파일시스템 기반 구현)
//   - hudson.model.FingerprintCleanupThread (주기적 정리 스레드)
//
// 핵심 원리:
//   1) Fingerprint는 MD5 해시로 파일을 식별하며, 해당 파일이 어떤 빌드에서
//      생성(original)되고 어떤 빌드들에서 사용(usages)되었는지를 추적한다.
//   2) RangeSet은 빌드 번호들을 범위(Range)의 정렬된 리스트로 효율적으로 표현한다.
//      예: 빌드 1,2,3,5,7,8,9 → "[1,4),[5,6),[7,10)" = "1-3,5,7-9"
//   3) FingerprintMap은 MD5 해시 → Fingerprint 매핑의 전역 캐시이다.
//      KeyedDataStorage를 상속하여 WeakReference 기반 GC를 지원한다.
//   4) FileFingerprintStorage는 fingerprints/ab/cd/abcdef...xml 경로에 XML로 저장한다.
//   5) isAlive()는 usages에 하나라도 살아있는 빌드가 있으면 true를 반환한다.
//   6) FingerprintCleanupThread는 매일 실행되어 dead fingerprint를 정리한다.
//
// 실행: go run main.go

package main

import (
	"crypto/md5"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// 1. Range: 빌드 번호 범위 [start, end) — 불변
// =============================================================================
// Jenkins: hudson.model.Fingerprint.Range
// - start 이상 end 미만의 정수 범위를 나타낸다.
// - includes(i): start <= i < end
// - isSmallerThan(i): end <= i (범위의 모든 값이 i보다 작음)
// - isBiggerThan(i): i < start
// - combine(that): 두 범위의 합집합 (겹치거나 인접해야 함)
// - intersect(that): 두 범위의 교집합
// - isIndependent(that): 두 범위를 하나로 합칠 수 없음
// - isDisjoint(that): 두 범위가 공통 정수를 공유하지 않음
// - isAdjacentTo(that): this.end == that.start

// Range는 [start, end) 범위의 빌드 번호를 나타낸다.
// Jenkins의 Fingerprint.Range에 대응한다.
type Range struct {
	Start int // 시작 (포함)
	End   int // 끝 (미포함)
}

// NewRange는 새 Range를 생성한다. start < end 이어야 한다.
func NewRange(start, end int) Range {
	if start >= end {
		panic(fmt.Sprintf("Range: start(%d) >= end(%d)", start, end))
	}
	return Range{Start: start, End: end}
}

// Includes는 정수 i가 이 범위에 포함되는지 반환한다.
// Jenkins: Range.includes(int) → start <= i && i < end
func (r Range) Includes(i int) bool {
	return r.Start <= i && i < r.End
}

// IsSmallerThan은 이 범위의 모든 값이 i보다 작은지 반환한다.
// Jenkins: Range.isSmallerThan(int) → end <= i
func (r Range) IsSmallerThan(i int) bool {
	return r.End <= i
}

// IsBiggerThan은 이 범위의 모든 값이 i보다 큰지 반환한다.
// Jenkins: Range.isBiggerThan(int) → i < start
func (r Range) IsBiggerThan(i int) bool {
	return i < r.Start
}

// IsSingle은 이 범위가 단일 숫자를 나타내는지 반환한다.
// Jenkins: Range.isSingle() → end - 1 == start
func (r Range) IsSingle() bool {
	return r.End-1 == r.Start
}

// ExpandRight는 end를 1 늘린 새 Range를 반환한다.
// Jenkins: Range.expandRight() → new Range(start, end+1)
func (r Range) ExpandRight() Range {
	return Range{Start: r.Start, End: r.End + 1}
}

// ExpandLeft는 start를 1 줄인 새 Range를 반환한다.
// Jenkins: Range.expandLeft() → new Range(start-1, end)
func (r Range) ExpandLeft() Range {
	return Range{Start: r.Start - 1, End: r.End}
}

// IsAdjacentTo는 이 범위가 that과 인접하는지 반환한다.
// Jenkins: Range.isAdjacentTo(Range) → this.end == that.start
func (r Range) IsAdjacentTo(that Range) bool {
	return r.End == that.Start
}

// IsIndependent는 두 범위를 하나로 합칠 수 없는지 반환한다.
// Jenkins: Range.isIndependent(Range) → this.end < that.start || that.end < this.start
func (r Range) IsIndependent(that Range) bool {
	return r.End < that.Start || that.End < r.Start
}

// IsDisjoint는 두 범위가 공통 정수를 공유하지 않는지 반환한다.
// Jenkins: Range.isDisjoint(Range) → this.end <= that.start || that.end <= this.start
func (r Range) IsDisjoint(that Range) bool {
	return r.End <= that.Start || that.End <= r.Start
}

// Contains는 이 범위가 that 범위를 완전히 포함하는지 반환한다.
// Jenkins: Range.contains(Range) → this.start <= that.start && that.end <= this.end
func (r Range) Contains(that Range) bool {
	return r.Start <= that.Start && that.End <= r.End
}

// Combine은 두 범위를 합친 새 Range를 반환한다.
// 두 범위가 겹치거나 인접해야 한다 (IsIndependent가 false).
// Jenkins: Range.combine(Range) → new Range(min(starts), max(ends))
func (r Range) Combine(that Range) Range {
	start := r.Start
	if that.Start < start {
		start = that.Start
	}
	end := r.End
	if that.End > end {
		end = that.End
	}
	return Range{Start: start, End: end}
}

// Intersect는 두 범위의 교집합을 반환한다.
// 두 범위가 겹쳐야 한다 (IsDisjoint가 false).
// Jenkins: Range.intersect(Range) → new Range(max(starts), min(ends))
func (r Range) Intersect(that Range) Range {
	start := r.Start
	if that.Start > start {
		start = that.Start
	}
	end := r.End
	if that.End < end {
		end = that.End
	}
	return Range{Start: start, End: end}
}

// String은 Jenkins 형식으로 Range를 표현한다.
// Jenkins: Range.toString() → "[start,end)"
func (r Range) String() string {
	return fmt.Sprintf("[%d,%d)", r.Start, r.End)
}

// =============================================================================
// 2. RangeSet: Range의 정렬된 집합 — 가변
// =============================================================================
// Jenkins: hudson.model.Fingerprint.RangeSet
// - ranges: 정렬된 Range 리스트 (겹치지 않고, 인접하지 않음)
// - add(int): 단일 빌드 번호 추가, 인접 범위 자동 병합
// - add(RangeSet): 다른 RangeSet의 모든 범위 병합
// - includes(int): 특정 빌드 번호 포함 여부
// - retainAll(RangeSet): 교집합으로 갱신
// - removeAll(RangeSet): 차집합으로 갱신
// - fromString("1-3,5,7-9"): 문자열 파싱
// - 직렬화: "1-3,5,7-9" 형식 (ConverterImpl.serialize)

// RangeSet은 정렬된 Range들의 집합이다.
// Jenkins의 Fingerprint.RangeSet에 대응한다.
type RangeSet struct {
	mu     sync.Mutex // Jenkins에서는 synchronized 메서드로 동시성 제어
	ranges []Range    // 정렬된, 겹치지 않는 Range 리스트
}

// NewRangeSet은 빈 RangeSet을 생성한다.
func NewRangeSet() *RangeSet {
	return &RangeSet{ranges: make([]Range, 0)}
}

// Add는 단일 빌드 번호를 추가한다.
// 인접한 범위가 있으면 자동으로 병합한다.
// Jenkins: RangeSet.add(int n)
// - 이미 포함되어 있으면 no-op
// - r.end == n 이면 expandRight + checkCollapse
// - r.start == n+1 이면 expandLeft + checkCollapse
// - r.isBiggerThan(n) 이면 새 Range(n, n+1) 삽입
// - 모두 아니면 끝에 추가
func (rs *RangeSet) Add(n int) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	for i := 0; i < len(rs.ranges); i++ {
		r := rs.ranges[i]

		if r.Includes(n) {
			return // 이미 포함
		}
		if r.End == n {
			// 오른쪽으로 확장
			rs.ranges[i] = r.ExpandRight()
			rs.checkCollapse(i)
			return
		}
		if r.Start == n+1 {
			// 왼쪽으로 확장
			rs.ranges[i] = r.ExpandLeft()
			rs.checkCollapse(i - 1)
			return
		}
		if r.IsBiggerThan(n) {
			// 이 위치에 새 단일 범위 삽입
			rs.ranges = append(rs.ranges[:i+1], rs.ranges[i:]...)
			rs.ranges[i] = NewRange(n, n+1)
			return
		}
	}

	// 모든 기존 범위보다 큼 → 끝에 추가
	rs.ranges = append(rs.ranges, NewRange(n, n+1))
}

// checkCollapse는 인접한 두 범위를 병합한다.
// Jenkins: RangeSet.checkCollapse(int i)
// - i번째와 i+1번째 Range가 인접하면 합친다.
func (rs *RangeSet) checkCollapse(i int) {
	if i < 0 || i >= len(rs.ranges)-1 {
		return
	}
	lhs := rs.ranges[i]
	rhs := rs.ranges[i+1]
	if lhs.IsAdjacentTo(rhs) {
		// 병합
		merged := Range{Start: lhs.Start, End: rhs.End}
		rs.ranges[i] = merged
		rs.ranges = append(rs.ranges[:i+1], rs.ranges[i+2:]...)
	}
}

// AddAll은 여러 빌드 번호를 추가한다.
// Jenkins: RangeSet.addAll(int... n)
func (rs *RangeSet) AddAll(nums ...int) {
	for _, n := range nums {
		rs.Add(n)
	}
}

// AddRangeSet은 다른 RangeSet의 모든 범위를 병합한다.
// Jenkins: RangeSet.add(RangeSet that) — O(n+m) 병합 알고리즘
// - 두 정렬된 범위 리스트를 투 포인터로 순회하며 병합
func (rs *RangeSet) AddRangeSet(that *RangeSet) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	that.mu.Lock()
	defer that.mu.Unlock()

	lhs, rhs := 0, 0
	for lhs < len(rs.ranges) && rhs < len(that.ranges) {
		lr := rs.ranges[lhs]
		rr := that.ranges[rhs]

		// 겹치지 않음: lr이 rr보다 완전히 왼쪽
		if lr.End < rr.Start {
			lhs++
			continue
		}
		// 겹치지 않음: rr이 lr보다 완전히 왼쪽
		if rr.End < lr.Start {
			// rr을 lhs 위치에 삽입
			newRanges := make([]Range, len(rs.ranges)+1)
			copy(newRanges, rs.ranges[:lhs])
			newRanges[lhs] = rr
			copy(newRanges[lhs+1:], rs.ranges[lhs:])
			rs.ranges = newRanges
			lhs++
			rhs++
			continue
		}

		// 겹침 → 병합
		m := lr.Combine(rr)
		rhs++

		// 확장된 범위가 다음 범위와도 겹칠 수 있으므로 계속 병합
		for lhs+1 < len(rs.ranges) && !m.IsIndependent(rs.ranges[lhs+1]) {
			m = m.Combine(rs.ranges[lhs+1])
			rs.ranges = append(rs.ranges[:lhs+1], rs.ranges[lhs+2:]...)
		}
		rs.ranges[lhs] = m
	}

	// that에 남은 범위들 추가
	if rhs < len(that.ranges) {
		rs.ranges = append(rs.ranges, that.ranges[rhs:]...)
	}
}

// Includes는 특정 빌드 번호가 포함되어 있는지 반환한다.
// Jenkins: RangeSet.includes(int i)
func (rs *RangeSet) Includes(i int) bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	for _, r := range rs.ranges {
		if r.Includes(i) {
			return true
		}
	}
	return false
}

// RetainAll은 이 RangeSet을 that과의 교집합으로 갱신한다.
// Jenkins: RangeSet.retainAll(RangeSet that) — O(n+m) 교집합 알고리즘
// - 두 정렬된 범위 리스트를 투 포인터로 순회
// - 겹치는 부분만 intersect로 추출
// - 수정되었으면 true 반환
func (rs *RangeSet) RetainAll(that *RangeSet) bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	that.mu.Lock()
	defer that.mu.Unlock()

	var intersection []Range
	lhs, rhs := 0, 0

	for lhs < len(rs.ranges) && rhs < len(that.ranges) {
		lr := rs.ranges[lhs]
		rr := that.ranges[rhs]

		if lr.End <= rr.Start {
			lhs++
			continue
		}
		if rr.End <= lr.Start {
			rhs++
			continue
		}

		// 겹침 → 교집합 추출
		v := lr.Intersect(rr)
		intersection = append(intersection, v)

		if lr.End < rr.End {
			lhs++
		} else {
			rhs++
		}
	}

	same := rangesEqual(rs.ranges, intersection)
	if !same {
		rs.ranges = intersection
		return true
	}
	return false
}

// RemoveAll은 이 RangeSet에서 that의 모든 범위를 제거한다.
// Jenkins: RangeSet.removeAll(RangeSet that) — O(n+m) 차집합 알고리즘
// - 겹치는 부분을 잘라내고 나머지를 유지
func (rs *RangeSet) RemoveAll(that *RangeSet) bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	that.mu.Lock()
	defer that.mu.Unlock()

	modified := false
	var sub []Range
	lhs, rhs := 0, 0

	for lhs < len(rs.ranges) && rhs < len(that.ranges) {
		lr := rs.ranges[lhs]
		rr := that.ranges[rhs]

		if lr.End <= rr.Start {
			// lr은 rr과 겹치지 않음 → 유지
			sub = append(sub, lr)
			lhs++
			continue
		}
		if rr.End <= lr.Start {
			// rr은 lr과 겹치지 않음
			rhs++
			continue
		}

		// 겹침
		modified = true

		if rr.Contains(lr) {
			// lr이 완전히 제거됨
			lhs++
			continue
		}

		// 왼쪽 잔여부분 (A)
		if lr.Start < rr.Start {
			sub = append(sub, Range{Start: lr.Start, End: rr.Start})
		}

		// 오른쪽 잔여부분 (B)
		if rr.End < lr.End {
			// 아직 처리해야 할 부분이 남음
			rs.ranges[lhs] = Range{Start: rr.End, End: lr.End}
			rhs++
		} else {
			lhs++
		}
	}

	if !modified {
		return false
	}

	// 남은 lr들 추가
	for lhs < len(rs.ranges) {
		sub = append(sub, rs.ranges[lhs])
		lhs++
	}

	rs.ranges = sub
	return true
}

// IsEmpty는 RangeSet이 비어있는지 반환한다.
// Jenkins: RangeSet.isEmpty()
func (rs *RangeSet) IsEmpty() bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return len(rs.ranges) == 0
}

// IsSmallerThan은 모든 값이 n보다 작은지 반환한다.
// Jenkins: RangeSet.isSmallerThan(int n)
// - 비어있으면 true (Jenkins: "Note that {} is smaller than any n")
// - 마지막 Range의 end <= n
func (rs *RangeSet) IsSmallerThan(n int) bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	if len(rs.ranges) == 0 {
		return true
	}
	return rs.ranges[len(rs.ranges)-1].IsSmallerThan(n)
}

// Serialize는 RangeSet을 "1-3,5,7-9" 형식의 문자열로 직렬화한다.
// Jenkins: RangeSet.ConverterImpl.serialize(RangeSet)
// - 단일값이면 숫자만 출력, 범위이면 start-end (end는 포함, 즉 end-1)
func (rs *RangeSet) Serialize() string {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	var parts []string
	for _, r := range rs.ranges {
		if r.IsSingle() {
			parts = append(parts, fmt.Sprintf("%d", r.Start))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", r.Start, r.End-1))
		}
	}
	return strings.Join(parts, ",")
}

// String은 Jenkins의 RangeSet.toString() 형식으로 표현한다.
// Jenkins: "[1,4),[5,6),[7,10)"
func (rs *RangeSet) String() string {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	var parts []string
	for _, r := range rs.ranges {
		parts = append(parts, r.String())
	}
	return strings.Join(parts, ",")
}

// GetRanges는 범위 리스트의 복사본을 반환한다.
// Jenkins: RangeSet.getRanges() — synchronized, 복사본 반환
func (rs *RangeSet) GetRanges() []Range {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	result := make([]Range, len(rs.ranges))
	copy(result, rs.ranges)
	return result
}

// rangesEqual은 두 Range 슬라이스가 동일한지 비교한다.
func rangesEqual(a, b []Range) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// =============================================================================
// 3. BuildPtr: 빌드 참조 포인터
// =============================================================================
// Jenkins: hudson.model.Fingerprint.BuildPtr
// - name: Job의 fullName (예: "folder/my-job")
// - number: 빌드 번호
// - getJob(): Jenkins 인스턴스에서 Job 조회
// - getRun(): Job에서 해당 번호의 Run 조회
// - isAlive(): getRun() != null

// BuildPtr은 특정 빌드를 가리키는 포인터이다.
// Jenkins의 Fingerprint.BuildPtr에 대응한다.
type BuildPtr struct {
	Name   string // Job 이름 (Jenkins: fullName)
	Number int    // 빌드 번호
}

// String은 "JobName #Number" 형식으로 표현한다.
// Jenkins: BuildPtr.toString() → name + " #" + number
func (bp BuildPtr) String() string {
	return fmt.Sprintf("%s #%d", bp.Name, bp.Number)
}

// =============================================================================
// 4. Fingerprint: MD5 기반 파일 추적
// =============================================================================
// Jenkins: hudson.model.Fingerprint (implements ModelObject, Saveable)
// - md5sum: 파일의 MD5 해시 (byte[])
// - fileName: 파일 이름 (경로 없이, 예: "artifact.jar")
// - timestamp: Fingerprint 생성 시각
// - original: 이 파일을 처음 생성한 빌드 (BuildPtr, null 가능)
// - usages: Hashtable<String, RangeSet> — Job별 사용 빌드 번호 집합
// - facets: 확장 가능한 메타데이터

// Fingerprint는 MD5 해시로 식별되는 파일의 추적 정보이다.
// Jenkins의 hudson.model.Fingerprint에 대응한다.
type Fingerprint struct {
	mu        sync.Mutex
	md5sum    string              // MD5 해시 (hex string)
	fileName  string              // 파일 이름
	timestamp time.Time           // 생성 시각
	original  *BuildPtr           // 이 파일을 처음 생성한 빌드 (nil이면 외부 생성)
	usages    map[string]*RangeSet // Job 이름 → 사용 빌드 번호 범위
}

// NewFingerprint는 새 Fingerprint를 생성한다.
// Jenkins: new Fingerprint(Run build, String fileName, byte[] md5sum)
// - original = build != null ? new BuildPtr(build) : null
// - timestamp = new Date()
// - save() 호출
func NewFingerprint(original *BuildPtr, fileName string, md5hex string) *Fingerprint {
	fp := &Fingerprint{
		md5sum:    md5hex,
		fileName:  fileName,
		timestamp: time.Now(),
		original:  original,
		usages:    make(map[string]*RangeSet),
	}
	// original이 있으면 첫 사용으로 기록
	if original != nil {
		rs := NewRangeSet()
		rs.Add(original.Number)
		fp.usages[original.Name] = rs
	}
	return fp
}

// GetHashString은 MD5 해시 문자열을 반환한다.
// Jenkins: Fingerprint.getHashString() → Util.toHexString(md5sum)
func (fp *Fingerprint) GetHashString() string {
	return fp.md5sum
}

// GetFileName은 파일 이름을 반환한다.
// Jenkins: Fingerprint.getFileName()
func (fp *Fingerprint) GetFileName() string {
	return fp.fileName
}

// GetOriginal은 이 파일을 처음 생성한 빌드를 반환한다.
// Jenkins: Fingerprint.getOriginal() — 권한 확인 후 반환
func (fp *Fingerprint) GetOriginal() *BuildPtr {
	return fp.original
}

// GetTimestamp는 Fingerprint 생성 시각을 반환한다.
func (fp *Fingerprint) GetTimestamp() time.Time {
	return fp.timestamp
}

// Add는 특정 Job의 빌드에서 이 파일을 사용했음을 기록한다.
// Jenkins: Fingerprint.add(String jobFullName, int n) — synchronized
// - addWithoutSaving()을 호출하여 usages에 기록
// - usages가 없으면 새 RangeSet 생성
func (fp *Fingerprint) Add(jobFullName string, buildNumber int) {
	fp.mu.Lock()
	defer fp.mu.Unlock()

	rs, exists := fp.usages[jobFullName]
	if !exists {
		rs = NewRangeSet()
		fp.usages[jobFullName] = rs
	}
	rs.Add(buildNumber)
}

// GetRangeSet은 특정 Job의 사용 빌드 번호 범위를 반환한다.
// Jenkins: Fingerprint.getRangeSet(String jobFullName)
// - 없으면 빈 RangeSet 반환
func (fp *Fingerprint) GetRangeSet(jobFullName string) *RangeSet {
	fp.mu.Lock()
	defer fp.mu.Unlock()

	rs, exists := fp.usages[jobFullName]
	if !exists {
		return NewRangeSet()
	}
	return rs
}

// GetJobs는 이 파일을 사용한 모든 Job 이름을 정렬하여 반환한다.
// Jenkins: Fingerprint.getJobs() — synchronized, 정렬된 리스트
func (fp *Fingerprint) GetJobs() []string {
	fp.mu.Lock()
	defer fp.mu.Unlock()

	jobs := make([]string, 0, len(fp.usages))
	for k := range fp.usages {
		jobs = append(jobs, k)
	}
	sort.Strings(jobs)
	return jobs
}

// GetUsages는 전체 사용 정보를 반환한다.
func (fp *Fingerprint) GetUsages() map[string]*RangeSet {
	fp.mu.Lock()
	defer fp.mu.Unlock()

	result := make(map[string]*RangeSet)
	for k, v := range fp.usages {
		result[k] = v
	}
	return result
}

// IsAlive는 이 Fingerprint가 아직 유효한지 (살아있는 빌드가 있는지) 반환한다.
// Jenkins: Fingerprint.isAlive() — synchronized
// - original이 살아있으면 true
// - usages의 어떤 Job에라도 살아있는 빌드가 있으면 true
// - 모두 죽었으면 false → FingerprintCleanupThread가 삭제
//
// 이 PoC에서는 JobRegistry를 통해 Job의 존재 여부와 첫 빌드 번호를 확인한다.
func (fp *Fingerprint) IsAlive(registry *JobRegistry) bool {
	fp.mu.Lock()
	defer fp.mu.Unlock()

	// original 빌드가 살아있는지 확인
	if fp.original != nil {
		if registry.IsBuildAlive(fp.original.Name, fp.original.Number) {
			return true
		}
	}

	// usages를 순회하며 살아있는 빌드 확인
	// Jenkins: Job을 조회 → firstBuild 확인 → RangeSet.isSmallerThan(oldest)이면 dead
	for jobName, rangeSet := range fp.usages {
		firstBuild, exists := registry.GetFirstBuild(jobName)
		if !exists {
			continue // Job이 없으면 건너뜀
		}
		// RangeSet의 모든 빌드가 firstBuild보다 작으면 dead
		if !rangeSet.IsSmallerThan(firstBuild) {
			return true
		}
	}
	return false
}

// Trim은 죽은 빌드/Job 참조를 정리한다.
// Jenkins: Fingerprint.trim() — synchronized
// - 존재하지 않는 Job 제거
// - 빌드가 없는 Job 제거
// - 오래된 빌드 번호 정리
func (fp *Fingerprint) Trim(registry *JobRegistry) bool {
	fp.mu.Lock()
	defer fp.mu.Unlock()

	modified := false
	for jobName, rangeSet := range fp.usages {
		firstBuild, exists := registry.GetFirstBuild(jobName)
		if !exists {
			// Job이 존재하지 않음 → 제거
			delete(fp.usages, jobName)
			modified = true
			continue
		}

		// firstBuild 이전의 빌드 번호 제거
		discarding := NewRangeSet()
		discarding.ranges = []Range{{Start: 0, End: firstBuild}}
		if rangeSet.RemoveAll(discarding) {
			modified = true
		}

		if rangeSet.IsEmpty() {
			delete(fp.usages, jobName)
			modified = true
		}
	}
	return modified
}

// String은 Fingerprint 요약을 반환한다.
func (fp *Fingerprint) String() string {
	fp.mu.Lock()
	defer fp.mu.Unlock()

	orig := "외부"
	if fp.original != nil {
		orig = fp.original.String()
	}
	return fmt.Sprintf("Fingerprint[%s] file=%s, original=%s, jobs=%d",
		fp.md5sum[:12]+"...", fp.fileName, orig, len(fp.usages))
}

// =============================================================================
// 5. FingerprintMap: 전역 Fingerprint 캐시 (MD5 해시 → Fingerprint)
// =============================================================================
// Jenkins: hudson.model.FingerprintMap extends KeyedDataStorage<Fingerprint, FingerprintParams>
// - getOrCreate(Run build, String fileName, String md5sum)
// - MD5 해시를 키로 사용 (32자 hex, 소문자)
// - 동일 해시에 대해 하나의 Fingerprint 인스턴스만 유지
// - WeakReference 기반으로 GC 가능
//
// Jenkins: jenkins.fingerprints.FingerprintStorage (추상 클래스)
// - save(Fingerprint), load(String id), delete(String id)
// - FileFingerprintStorage: fingerprints/ab/cd/abcdef...xml 경로에 저장

// FingerprintMap은 MD5 해시를 키로 하는 전역 Fingerprint 저장소이다.
// Jenkins의 FingerprintMap에 대응한다.
type FingerprintMap struct {
	mu           sync.RWMutex
	fingerprints map[string]*Fingerprint // MD5 hex → Fingerprint
}

// NewFingerprintMap은 새 FingerprintMap을 생성한다.
func NewFingerprintMap() *FingerprintMap {
	return &FingerprintMap{
		fingerprints: make(map[string]*Fingerprint),
	}
}

// GetOrCreate는 MD5 해시에 대한 Fingerprint를 조회하거나 생성한다.
// Jenkins: FingerprintMap.getOrCreate(Run build, String fileName, String md5sum)
// - MD5 해시 길이가 32가 아니면 nil (Jenkins: sanity check)
// - 기존 Fingerprint가 있으면 반환
// - 없으면 새로 생성하여 저장
func (fm *FingerprintMap) GetOrCreate(build *BuildPtr, fileName string, md5hex string) *Fingerprint {
	// Jenkins: md5sum.length() != 32 → return null
	if len(md5hex) != 32 {
		fmt.Printf("  [경고] 잘못된 MD5 해시 길이: %d (기대값: 32)\n", len(md5hex))
		return nil
	}
	md5hex = strings.ToLower(md5hex)

	fm.mu.Lock()
	defer fm.mu.Unlock()

	if fp, exists := fm.fingerprints[md5hex]; exists {
		return fp
	}

	fp := NewFingerprint(build, fileName, md5hex)
	fm.fingerprints[md5hex] = fp
	return fp
}

// Get은 MD5 해시로 Fingerprint를 조회한다.
func (fm *FingerprintMap) Get(md5hex string) *Fingerprint {
	fm.mu.RLock()
	defer fm.mu.RUnlock()

	return fm.fingerprints[strings.ToLower(md5hex)]
}

// GetStoragePath는 파일시스템 저장 경로를 반환한다.
// Jenkins: FileFingerprintStorage — fingerprints/ab/cd/abcdef1234...xml
// - 해시의 첫 2자 → 첫 번째 디렉토리
// - 해시의 3~4자 → 두 번째 디렉토리
// - 전체 해시.xml → 파일
func GetStoragePath(md5hex string) string {
	if len(md5hex) < 4 {
		return "fingerprints/invalid"
	}
	return fmt.Sprintf("fingerprints/%s/%s/%s.xml", md5hex[:2], md5hex[2:4], md5hex)
}

// Delete는 Fingerprint를 삭제한다.
// Jenkins: FingerprintStorage.delete(String id)
func (fm *FingerprintMap) Delete(md5hex string) bool {
	fm.mu.Lock()
	defer fm.mu.Unlock()

	md5hex = strings.ToLower(md5hex)
	if _, exists := fm.fingerprints[md5hex]; exists {
		delete(fm.fingerprints, md5hex)
		return true
	}
	return false
}

// Size는 저장된 Fingerprint 수를 반환한다.
func (fm *FingerprintMap) Size() int {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	return len(fm.fingerprints)
}

// All은 모든 Fingerprint를 반환한다.
func (fm *FingerprintMap) All() []*Fingerprint {
	fm.mu.RLock()
	defer fm.mu.RUnlock()

	result := make([]*Fingerprint, 0, len(fm.fingerprints))
	for _, fp := range fm.fingerprints {
		result = append(result, fp)
	}
	return result
}

// =============================================================================
// 6. JobRegistry: Job/빌드 존재 여부 확인 (isAlive 판단용)
// =============================================================================
// 실제 Jenkins에서는 Jenkins.get().getItemByFullName()으로 Job을 조회하고,
// Job.getFirstBuild()으로 가장 오래된 빌드를 확인한다.
// 이 PoC에서는 단순화된 레지스트리로 시뮬레이션한다.

// JobRegistry는 Job과 빌드 정보를 관리한다.
type JobRegistry struct {
	mu   sync.RWMutex
	jobs map[string]*JobInfo // Job 이름 → 빌드 정보
}

// JobInfo는 Job의 빌드 정보를 담는다.
type JobInfo struct {
	Name       string
	FirstBuild int   // 가장 오래된 빌드 번호
	Builds     []int // 존재하는 빌드 번호 목록
}

// NewJobRegistry는 새 JobRegistry를 생성한다.
func NewJobRegistry() *JobRegistry {
	return &JobRegistry{
		jobs: make(map[string]*JobInfo),
	}
}

// RegisterJob은 Job을 등록한다.
func (jr *JobRegistry) RegisterJob(name string, firstBuild int, builds []int) {
	jr.mu.Lock()
	defer jr.mu.Unlock()
	jr.jobs[name] = &JobInfo{Name: name, FirstBuild: firstBuild, Builds: builds}
}

// RemoveJob은 Job을 제거한다.
func (jr *JobRegistry) RemoveJob(name string) {
	jr.mu.Lock()
	defer jr.mu.Unlock()
	delete(jr.jobs, name)
}

// GetFirstBuild는 Job의 첫 빌드 번호를 반환한다.
func (jr *JobRegistry) GetFirstBuild(name string) (int, bool) {
	jr.mu.RLock()
	defer jr.mu.RUnlock()
	job, exists := jr.jobs[name]
	if !exists {
		return 0, false
	}
	return job.FirstBuild, true
}

// IsBuildAlive는 특정 빌드가 존재하는지 반환한다.
func (jr *JobRegistry) IsBuildAlive(jobName string, buildNumber int) bool {
	jr.mu.RLock()
	defer jr.mu.RUnlock()
	job, exists := jr.jobs[jobName]
	if !exists {
		return false
	}
	for _, b := range job.Builds {
		if b == buildNumber {
			return true
		}
	}
	return false
}

// =============================================================================
// 7. FingerprintCleanupThread: 주기적 정리 (시뮬레이션)
// =============================================================================
// Jenkins: hudson.model.FingerprintCleanupThread extends AsyncPeriodicWork
// - getRecurrencePeriod() → DAY (24시간마다 실행)
// - execute(): FingerprintStorage.get().iterateAndCleanupFingerprints()
// - cleanFingerprint(): isAlive() false이면 delete, true이면 trim()

// CleanupFingerprints는 FingerprintCleanupThread의 동작을 시뮬레이션한다.
// Jenkins: FingerprintStorage.cleanFingerprint(Fingerprint, TaskListener)
// - isAlive() == false → 삭제
// - isAlive() == true → trim()으로 오래된 참조 정리
func CleanupFingerprints(fm *FingerprintMap, registry *JobRegistry) (deleted, trimmed int) {
	for _, fp := range fm.All() {
		if !fp.IsAlive(registry) {
			// dead fingerprint → 삭제
			if fm.Delete(fp.GetHashString()) {
				fmt.Printf("  [정리] 삭제: %s (파일: %s) — 모든 빌드가 사라짐\n",
					fp.GetHashString()[:12]+"...", fp.GetFileName())
				deleted++
			}
		} else {
			// alive → trim
			if fp.Trim(registry) {
				fmt.Printf("  [정리] 트림: %s (파일: %s) — 오래된 참조 제거\n",
					fp.GetHashString()[:12]+"...", fp.GetFileName())
				trimmed++
			}
		}
	}
	return
}

// =============================================================================
// 헬퍼: MD5 해시 계산
// =============================================================================

// ComputeMD5 는 데이터의 MD5 해시를 hex 문자열로 반환한다.
func ComputeMD5(data []byte) string {
	hash := md5.Sum(data)
	return fmt.Sprintf("%x", hash)
}

// =============================================================================
// 메인: 데모 실행
// =============================================================================
func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║   Jenkins Fingerprint(아티팩트 추적) 시스템 시뮬레이션     ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()

	// =========================================================================
	// 데모 1: RangeSet 기본 동작
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("1. RangeSet: 빌드 번호 범위의 효율적 표현")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	rs := NewRangeSet()
	fmt.Println("빌드 번호를 하나씩 추가하며 범위 병합 관찰:")
	fmt.Println()

	// 빌드 1, 2, 3 추가 → [1,4)
	for _, n := range []int{1, 2, 3} {
		rs.Add(n)
		fmt.Printf("  Add(%d) → 내부: %-20s  직렬화: %s\n", n, rs.String(), rs.Serialize())
	}

	// 빌드 5 추가 → [1,4),[5,6)
	rs.Add(5)
	fmt.Printf("  Add(%d) → 내부: %-20s  직렬화: %s\n", 5, rs.String(), rs.Serialize())

	// 빌드 4 추가 → [1,6) (병합 발생!)
	rs.Add(4)
	fmt.Printf("  Add(%d) → 내부: %-20s  직렬화: %s  ← 인접 범위 병합!\n", 4, rs.String(), rs.Serialize())

	// 빌드 7, 8, 9, 10, 12 추가
	for _, n := range []int{7, 8, 9, 10, 12} {
		rs.Add(n)
	}
	fmt.Printf("  Add(7,8,9,10,12) → 내부: %s\n", rs.String())
	fmt.Printf("                     직렬화: %s\n", rs.Serialize())

	// includes 테스트
	fmt.Println()
	fmt.Println("Includes 테스트:")
	for _, n := range []int{3, 6, 10, 11, 12} {
		fmt.Printf("  includes(%d) = %v\n", n, rs.Includes(n))
	}

	fmt.Println()

	// =========================================================================
	// 데모 2: RangeSet 집합 연산
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("2. RangeSet 집합 연산: AddRangeSet, RetainAll, RemoveAll")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	// AddRangeSet: 합집합
	rsA := NewRangeSet()
	rsA.AddAll(1, 2, 3, 7, 8)
	rsB := NewRangeSet()
	rsB.AddAll(3, 4, 5, 10, 11, 12)

	fmt.Printf("  A = %s (%s)\n", rsA.String(), rsA.Serialize())
	fmt.Printf("  B = %s (%s)\n", rsB.String(), rsB.Serialize())

	rsA.AddRangeSet(rsB)
	fmt.Printf("  A.AddRangeSet(B) = %s (%s)\n", rsA.String(), rsA.Serialize())
	fmt.Println()

	// RetainAll: 교집합
	rsC := NewRangeSet()
	rsC.AddAll(1, 2, 3, 4, 5, 6, 7, 8)
	rsD := NewRangeSet()
	rsD.AddAll(3, 4, 5, 10, 11)

	fmt.Printf("  C = %s (%s)\n", rsC.String(), rsC.Serialize())
	fmt.Printf("  D = %s (%s)\n", rsD.String(), rsD.Serialize())

	rsC.RetainAll(rsD)
	fmt.Printf("  C.RetainAll(D) = %s (%s)  ← 교집합\n", rsC.String(), rsC.Serialize())
	fmt.Println()

	// RemoveAll: 차집합
	rsE := NewRangeSet()
	rsE.AddAll(1, 2, 3, 4, 5, 6, 7, 8, 9, 10)
	rsF := NewRangeSet()
	rsF.AddAll(3, 4, 5, 8, 9)

	fmt.Printf("  E = %s (%s)\n", rsE.String(), rsE.Serialize())
	fmt.Printf("  F = %s (%s)\n", rsF.String(), rsF.Serialize())

	rsE.RemoveAll(rsF)
	fmt.Printf("  E.RemoveAll(F) = %s (%s)  ← 차집합\n", rsE.String(), rsE.Serialize())
	fmt.Println()

	// =========================================================================
	// 데모 3: Fingerprint 생성 및 아티팩트 추적 흐름
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("3. Fingerprint 아티팩트 추적 흐름")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	// 전역 저장소 생성
	fpMap := NewFingerprintMap()

	// 아티팩트 콘텐츠 시뮬레이션
	artifactContent := []byte("com.example.myapp-1.0.0.jar-contents")
	md5hex := ComputeMD5(artifactContent)

	fmt.Println("아티팩트 추적 시나리오:")
	fmt.Println()
	fmt.Printf("  파일: app-1.0.0.jar\n")
	fmt.Printf("  MD5 : %s\n", md5hex)
	fmt.Printf("  저장 경로: %s\n", GetStoragePath(md5hex))
	fmt.Println()

	// Step 1: Job A 빌드 #5에서 아티팩트 생성
	fmt.Println("  Step 1: Job A (빌드 #5)에서 아티팩트 생성")
	originalBuild := &BuildPtr{Name: "my-team/build-job", Number: 5}
	fp := fpMap.GetOrCreate(originalBuild, "app-1.0.0.jar", md5hex)
	fmt.Printf("    → Fingerprint 생성: original = %s\n", fp.GetOriginal())
	fmt.Printf("    → usages: my-team/build-job = %s\n",
		fp.GetRangeSet("my-team/build-job").Serialize())
	fmt.Println()

	// Step 2: Job B 빌드 #3에서 같은 아티팩트 사용 (Copy Artifact 플러그인)
	fmt.Println("  Step 2: Job B (빌드 #3)에서 아티팩트 복사 → MD5 일치 → 사용 기록")
	fp2 := fpMap.GetOrCreate(nil, "app-1.0.0.jar", md5hex) // 이미 존재하므로 동일 Fingerprint 반환
	fp2.Add("my-team/deploy-staging", 3)
	fmt.Printf("    → 동일 Fingerprint? %v (같은 객체)\n", fp == fp2)
	fmt.Printf("    → usages: my-team/deploy-staging = %s\n",
		fp.GetRangeSet("my-team/deploy-staging").Serialize())
	fmt.Println()

	// Step 3: Job C 빌드 #1에서도 사용
	fmt.Println("  Step 3: Job C (빌드 #1)에서도 아티팩트 사용")
	fp.Add("my-team/deploy-production", 1)
	fmt.Println()

	// Step 4: Job B에서 여러 빌드에서 사용
	fmt.Println("  Step 4: Job B (빌드 #4, #5, #7, #8)에서도 사용")
	for _, n := range []int{4, 5, 7, 8} {
		fp.Add("my-team/deploy-staging", n)
	}
	fmt.Println()

	// Step 5: Job A에서 추가 빌드
	fmt.Println("  Step 5: Job A (빌드 #6, #7)에서도 아티팩트 재생성")
	fp.Add("my-team/build-job", 6)
	fp.Add("my-team/build-job", 7)
	fmt.Println()

	// 현재 상태 출력
	fmt.Println("  ┌─────────────────────────────────────────────────────────┐")
	fmt.Println("  │ 현재 Fingerprint 상태                                  │")
	fmt.Println("  ├─────────────────────────────────────────────────────────┤")
	fmt.Printf("  │ 파일: %-50s│\n", fp.GetFileName())
	fmt.Printf("  │ MD5 : %-50s│\n", fp.GetHashString())
	fmt.Printf("  │ 원본: %-50s│\n", fp.GetOriginal())
	fmt.Println("  ├─────────────────────────────────────────────────────────┤")
	fmt.Println("  │ Job별 사용 빌드:                                       │")
	for _, job := range fp.GetJobs() {
		rs := fp.GetRangeSet(job)
		line := fmt.Sprintf("%s: %s", job, rs.Serialize())
		fmt.Printf("  │   %-54s│\n", line)
	}
	fmt.Println("  └─────────────────────────────────────────────────────────┘")
	fmt.Println()

	// =========================================================================
	// 데모 4: 여러 아티팩트의 Fingerprint 생성
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("4. 여러 아티팩트의 Fingerprint 관리")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	// 추가 아티팩트들
	artifacts := []struct {
		name    string
		content string
		job     string
		build   int
	}{
		{"config.yaml", "config-v2-contents", "infra/config-generator", 12},
		{"schema.sql", "create-table-users-v3", "backend/db-migration", 45},
		{"frontend.js", "react-app-bundle-v5", "frontend/build", 100},
	}

	for _, a := range artifacts {
		hash := ComputeMD5([]byte(a.content))
		bp := &BuildPtr{Name: a.job, Number: a.build}
		newFp := fpMap.GetOrCreate(bp, a.name, hash)
		fmt.Printf("  생성: %-15s MD5=%s... (원본: %s)\n",
			a.name, hash[:12], bp)

		// 일부 아티팩트에 추가 사용 기록
		if a.name == "config.yaml" {
			newFp.Add("infra/deploy-k8s", 5)
			newFp.Add("infra/deploy-k8s", 6)
			newFp.Add("infra/deploy-k8s", 7)
		}
	}

	fmt.Printf("\n  전역 FingerprintMap 크기: %d\n", fpMap.Size())
	fmt.Println()

	// =========================================================================
	// 데모 5: isAlive 확인 및 FingerprintCleanupThread
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("5. isAlive 확인 및 Fingerprint 정리")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	// Job 레지스트리 생성 — 어떤 빌드가 아직 살아있는지 시뮬레이션
	registry := NewJobRegistry()
	registry.RegisterJob("my-team/build-job", 5, []int{5, 6, 7})
	registry.RegisterJob("my-team/deploy-staging", 3, []int{3, 4, 5, 7, 8})
	registry.RegisterJob("my-team/deploy-production", 1, []int{1})
	registry.RegisterJob("infra/config-generator", 12, []int{12})
	registry.RegisterJob("infra/deploy-k8s", 5, []int{5, 6, 7})
	// backend/db-migration은 등록하지 않음 → Job 삭제 시뮬레이션
	// frontend/build도 등록하지 않음 → Job 삭제 시뮬레이션

	fmt.Println("Job 상태:")
	fmt.Println("  [존재] my-team/build-job        (빌드: 5,6,7)")
	fmt.Println("  [존재] my-team/deploy-staging    (빌드: 3,4,5,7,8)")
	fmt.Println("  [존재] my-team/deploy-production (빌드: 1)")
	fmt.Println("  [존재] infra/config-generator    (빌드: 12)")
	fmt.Println("  [존재] infra/deploy-k8s          (빌드: 5,6,7)")
	fmt.Println("  [삭제] backend/db-migration      ← Job 삭제됨")
	fmt.Println("  [삭제] frontend/build            ← Job 삭제됨")
	fmt.Println()

	// isAlive 확인
	fmt.Println("isAlive 확인:")
	for _, f := range fpMap.All() {
		alive := f.IsAlive(registry)
		status := "ALIVE"
		if !alive {
			status = "DEAD"
		}
		fmt.Printf("  [%5s] %s (파일: %s)\n", status, f.GetHashString()[:12]+"...", f.GetFileName())
	}
	fmt.Println()

	// 정리 실행 (FingerprintCleanupThread 시뮬레이션)
	fmt.Println("FingerprintCleanupThread 실행:")
	deleted, trimmed := CleanupFingerprints(fpMap, registry)
	fmt.Printf("\n  결과: 삭제=%d, 트림=%d, 남은 Fingerprint=%d\n", deleted, trimmed, fpMap.Size())
	fmt.Println()

	// =========================================================================
	// 데모 6: 저장 경로 체계
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("6. 파일시스템 저장 경로 체계 (FileFingerprintStorage)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	// Jenkins: FileFingerprintStorage
	// fingerprints/ab/cd/abcdef1234...xml
	// 해시의 처음 2글자/다음 2글자/전체해시.xml
	fmt.Println("Jenkins는 Fingerprint를 파일시스템에 XML로 저장한다:")
	fmt.Println("  JENKINS_HOME/fingerprints/{hash[0:2]}/{hash[2:4]}/{hash}.xml")
	fmt.Println()

	sampleHashes := []string{
		"a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6",
		"ff0011223344556677889900aabbccdd",
		md5hex,
	}
	for _, h := range sampleHashes {
		fmt.Printf("  %s → %s\n", h[:16]+"...", GetStoragePath(h))
	}
	fmt.Println()

	// =========================================================================
	// 데모 7: 추적 조회 — "이 아티팩트를 누가 사용하는가?"
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("7. 추적 조회: 아티팩트의 전체 사용 이력")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	// app-1.0.0.jar의 Fingerprint 조회
	trackedFp := fpMap.Get(md5hex)
	if trackedFp != nil {
		fmt.Printf("  파일: %s\n", trackedFp.GetFileName())
		fmt.Printf("  MD5 : %s\n", trackedFp.GetHashString())
		if trackedFp.GetOriginal() != nil {
			fmt.Printf("  원본 빌드: %s\n", trackedFp.GetOriginal())
		}
		fmt.Println()

		fmt.Println("  이 아티팩트를 사용한 빌드:")
		fmt.Println("  ┌──────────────────────────────┬────────────────────┐")
		fmt.Println("  │ Job                          │ 빌드 번호          │")
		fmt.Println("  ├──────────────────────────────┼────────────────────┤")
		for _, job := range trackedFp.GetJobs() {
			rs := trackedFp.GetRangeSet(job)
			fmt.Printf("  │ %-28s │ %-18s │\n", job, rs.Serialize())
		}
		fmt.Println("  └──────────────────────────────┴────────────────────┘")
	}
	fmt.Println()

	// =========================================================================
	// 데모 8: RangeSet 직렬화/역직렬화
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("8. RangeSet 직렬화 형식 (XML 저장용)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	fmt.Println("Jenkins XML에서 RangeSet은 두 가지 형식으로 저장된다:")
	fmt.Println()
	fmt.Println("  [구 형식] <ranges> 요소 안에 <range> 중첩:")
	fmt.Println("    <ranges>")
	fmt.Println("      <range><start>1</start><end>4</end></range>")
	fmt.Println("      <range><start>7</start><end>10</end></range>")
	fmt.Println("    </ranges>")
	fmt.Println()
	fmt.Println("  [신 형식] 쉼표와 대시로 직렬화:")
	fmt.Println("    <ranges>1-3,7-9</ranges>")
	fmt.Println()

	demo := NewRangeSet()
	demo.AddAll(1, 2, 3, 7, 8, 9, 15, 20, 21, 22, 23, 24, 25)
	fmt.Printf("  내부 표현: %s\n", demo.String())
	fmt.Printf("  직렬화:    %s\n", demo.Serialize())
	fmt.Println()

	// 개별 Range 정보
	fmt.Println("  Range 상세:")
	for i, r := range demo.GetRanges() {
		fmt.Printf("    [%d] %s → 단일값=%v\n", i, r, r.IsSingle())
	}
	fmt.Println()

	// =========================================================================
	// 데모 9: 전체 흐름 다이어그램
	// =========================================================================
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("9. 전체 Fingerprint 추적 흐름")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()
	fmt.Println("  ┌─────────────┐    아티팩트 생성     ┌──────────────┐")
	fmt.Println("  │  Job A #5   │ ─── artifact.jar ──→ │ MD5 해시 계산 │")
	fmt.Println("  │ (build-job) │                      │  a1b2c3d4... │")
	fmt.Println("  └─────────────┘                      └──────┬───────┘")
	fmt.Println("                                              │")
	fmt.Println("                                              ▼")
	fmt.Println("                                    ┌──────────────────┐")
	fmt.Println("                                    │ FingerprintMap   │")
	fmt.Println("                                    │ .getOrCreate()   │")
	fmt.Println("                                    │                  │")
	fmt.Println("                                    │ MD5 → Fingerprint│")
	fmt.Println("                                    └────────┬─────────┘")
	fmt.Println("                                             │")
	fmt.Println("                           ┌─────────────────┼──────────────────┐")
	fmt.Println("                           ▼                 ▼                  ▼")
	fmt.Println("                    ┌────────────┐  ┌──────────────┐  ┌──────────────┐")
	fmt.Println("                    │ Fingerprint │  │   usages     │  │ RangeSet     │")
	fmt.Println("                    │ original=   │  │ job→RangeSet │  │ [1,4),[5,6)  │")
	fmt.Println("                    │  A#5        │  │              │  │ = 빌드 1,2,3,5│")
	fmt.Println("                    │ fileName=   │  │ build-job:   │  └──────────────┘")
	fmt.Println("                    │  artifact   │  │   5-7        │")
	fmt.Println("                    │  .jar       │  │ staging:     │")
	fmt.Println("                    └────────────┘  │   3-5,7-8    │")
	fmt.Println("                                    │ production:  │")
	fmt.Println("                                    │   1          │")
	fmt.Println("                                    └──────────────┘")
	fmt.Println()
	fmt.Println("  ┌──────────────┐   같은 MD5 해시    ┌──────────────────┐")
	fmt.Println("  │  Job B #3    │ ── artifact.jar ──→│ FingerprintMap   │")
	fmt.Println("  │ (staging)    │  (Copy Artifact)   │ .getOrCreate()   │")
	fmt.Println("  └──────────────┘                    │                  │")
	fmt.Println("                                      │ 기존 Fingerprint │")
	fmt.Println("                                      │ 에 사용 기록 추가 │")
	fmt.Println("                                      └──────────────────┘")
	fmt.Println()

	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("시뮬레이션 완료")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
}
