package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// =============================================================================
// Prometheus Fanout Storage PoC
// 원본 코드: prometheus/storage/fanout.go, prometheus/storage/merge.go
//
// Fanout Storage는 하나의 Storage 인터페이스 뒤에 primary(로컬 TSDB)와
// 여러 secondary(원격 스토리지)를 두고, 쓰기는 모두에게 팬아웃하고
// 읽기는 결과를 병합(merge)하는 패턴이다.
//
// 핵심 시맨틱:
//   - Primary 실패 → 전체 실패 (로컬 TSDB는 필수)
//   - Secondary 실패 → 경고만 발생 (원격은 best-effort)
//   - 읽기 시 MergeQuerier가 중복 시리즈를 제거하고 병합
// =============================================================================

// ─── Labels & Matcher ───────────────────────────────────────────────────────

// Label은 시계열 레이블 하나를 나타낸다.
type Label struct {
	Name  string
	Value string
}

// Labels는 정렬된 레이블 집합이다.
type Labels []Label

func (ls Labels) String() string {
	parts := make([]string, len(ls))
	for i, l := range ls {
		parts[i] = fmt.Sprintf("%s=%q", l.Name, l.Value)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func (ls Labels) Key() string {
	return ls.String()
}

func (ls Labels) Matches(matchers []Matcher) bool {
	for _, m := range matchers {
		matched := false
		for _, l := range ls {
			if l.Name == m.Name && l.Value == m.Value {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// Matcher는 레이블 매칭 조건이다.
type Matcher struct {
	Name  string
	Value string
}

// ─── Sample ─────────────────────────────────────────────────────────────────

// Sample은 하나의 타임스탬프-값 쌍이다.
type Sample struct {
	Timestamp int64
	Value     float64
}

// ─── Series & SeriesSet ─────────────────────────────────────────────────────

// Series는 하나의 시계열(레이블 + 샘플들)을 나타낸다.
type Series struct {
	Lbs     Labels
	Samples []Sample
}

// SeriesSet은 시리즈의 반복자 인터페이스이다.
type SeriesSet interface {
	Next() bool
	At() Series
	Err() error
}

// sliceSeriesSet은 슬라이스 기반 SeriesSet 구현이다.
type sliceSeriesSet struct {
	series []Series
	idx    int
}

func newSliceSeriesSet(series []Series) *sliceSeriesSet {
	return &sliceSeriesSet{series: series, idx: -1}
}

func (s *sliceSeriesSet) Next() bool {
	s.idx++
	return s.idx < len(s.series)
}

func (s *sliceSeriesSet) At() Series {
	return s.series[s.idx]
}

func (s *sliceSeriesSet) Err() error { return nil }

// errSeriesSet은 에러를 반환하는 SeriesSet이다.
type errSeriesSet struct {
	err error
}

func (s *errSeriesSet) Next() bool   { return false }
func (s *errSeriesSet) At() Series   { return Series{} }
func (s *errSeriesSet) Err() error   { return s.err }

// ─── Storage Interfaces ─────────────────────────────────────────────────────

// Storage는 Prometheus의 최상위 스토리지 인터페이스이다.
// 원본: prometheus/storage/storage.go
type Storage interface {
	Appender() Appender
	Querier(mint, maxt int64) (Querier, error)
	Close() error
}

// Appender는 데이터를 추가하는 인터페이스이다.
type Appender interface {
	Append(labels Labels, t int64, v float64) error
	Commit() error
	Rollback() error
}

// Querier는 데이터를 조회하는 인터페이스이다.
type Querier interface {
	Select(matchers ...Matcher) SeriesSet
	Close() error
}

// ─── LocalStorage (Primary) ─────────────────────────────────────────────────

// LocalStorage는 로컬 TSDB를 시뮬레이션한다.
// Prometheus에서 head block + 디스크 블록에 해당한다.
type LocalStorage struct {
	mu       sync.RWMutex
	data     map[string]*Series // key: labels.String()
	closed   bool
	failNext bool // 테스트용: 다음 작업을 실패시킴
}

func NewLocalStorage() *LocalStorage {
	return &LocalStorage{
		data: make(map[string]*Series),
	}
}

func (s *LocalStorage) SetFailNext(fail bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext = fail
}

func (s *LocalStorage) Appender() Appender {
	return &localAppender{
		storage: s,
		pending: make(map[string][]Sample),
	}
}

func (s *LocalStorage) Querier(mint, maxt int64) (Querier, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.failNext {
		return nil, fmt.Errorf("local storage error: disk I/O failure")
	}
	return &localQuerier{storage: s, mint: mint, maxt: maxt}, nil
}

func (s *LocalStorage) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// localAppender는 LocalStorage용 Appender이다.
type localAppender struct {
	storage *LocalStorage
	pending map[string][]Sample // labels key → samples
	labels  map[string]Labels   // labels key → Labels
}

func (a *localAppender) Append(labels Labels, t int64, v float64) error {
	a.storage.mu.RLock()
	fail := a.storage.failNext
	a.storage.mu.RUnlock()
	if fail {
		return fmt.Errorf("local storage error: append failed (disk full)")
	}
	key := labels.Key()
	if a.labels == nil {
		a.labels = make(map[string]Labels)
	}
	a.labels[key] = labels
	a.pending[key] = append(a.pending[key], Sample{Timestamp: t, Value: v})
	return nil
}

func (a *localAppender) Commit() error {
	a.storage.mu.Lock()
	defer a.storage.mu.Unlock()
	for key, samples := range a.pending {
		existing, ok := a.storage.data[key]
		if !ok {
			lbs := a.labels[key]
			existing = &Series{Lbs: lbs}
			a.storage.data[key] = existing
		}
		existing.Samples = append(existing.Samples, samples...)
		// 타임스탬프 순으로 정렬
		sort.Slice(existing.Samples, func(i, j int) bool {
			return existing.Samples[i].Timestamp < existing.Samples[j].Timestamp
		})
	}
	a.pending = make(map[string][]Sample)
	return nil
}

func (a *localAppender) Rollback() error {
	a.pending = make(map[string][]Sample)
	return nil
}

// localQuerier는 LocalStorage용 Querier이다.
type localQuerier struct {
	storage *LocalStorage
	mint    int64
	maxt    int64
}

func (q *localQuerier) Select(matchers ...Matcher) SeriesSet {
	q.storage.mu.RLock()
	defer q.storage.mu.RUnlock()

	var result []Series
	for _, s := range q.storage.data {
		if !s.Lbs.Matches(matchers) {
			continue
		}
		// 시간 범위 필터링
		var filtered []Sample
		for _, sample := range s.Samples {
			if sample.Timestamp >= q.mint && sample.Timestamp <= q.maxt {
				filtered = append(filtered, sample)
			}
		}
		if len(filtered) > 0 {
			result = append(result, Series{Lbs: s.Lbs, Samples: filtered})
		}
	}
	// 레이블 기준 정렬 (병합에 필요)
	sort.Slice(result, func(i, j int) bool {
		return result[i].Lbs.Key() < result[j].Lbs.Key()
	})
	return newSliceSeriesSet(result)
}

func (q *localQuerier) Close() error { return nil }

// ─── RemoteStorage (Secondary) ──────────────────────────────────────────────

// RemoteStorage는 원격 스토리지를 시뮬레이션한다.
// Prometheus의 remote_write/remote_read에 해당한다.
type RemoteStorage struct {
	mu       sync.RWMutex
	name     string
	data     map[string]*Series
	closed   bool
	failNext bool
}

func NewRemoteStorage(name string) *RemoteStorage {
	return &RemoteStorage{
		name: name,
		data: make(map[string]*Series),
	}
}

func (s *RemoteStorage) SetFailNext(fail bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext = fail
}

func (s *RemoteStorage) Appender() Appender {
	return &remoteAppender{
		storage: s,
		pending: make(map[string][]Sample),
	}
}

func (s *RemoteStorage) Querier(mint, maxt int64) (Querier, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.failNext {
		return nil, fmt.Errorf("remote storage [%s] error: connection refused", s.name)
	}
	return &remoteQuerier{storage: s, mint: mint, maxt: maxt}, nil
}

func (s *RemoteStorage) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// DirectInsert는 테스트용으로 원격 스토리지에 직접 데이터를 삽입한다.
// (실제로는 다른 Prometheus 인스턴스가 원격에 쓴 데이터를 시뮬레이션)
func (s *RemoteStorage) DirectInsert(labels Labels, samples []Sample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := labels.Key()
	existing, ok := s.data[key]
	if !ok {
		existing = &Series{Lbs: labels}
		s.data[key] = existing
	}
	existing.Samples = append(existing.Samples, samples...)
}

// remoteAppender는 RemoteStorage용 Appender이다.
type remoteAppender struct {
	storage *RemoteStorage
	pending map[string][]Sample
	labels  map[string]Labels
}

func (a *remoteAppender) Append(labels Labels, t int64, v float64) error {
	a.storage.mu.RLock()
	fail := a.storage.failNext
	a.storage.mu.RUnlock()
	if fail {
		return fmt.Errorf("remote storage [%s] error: write failed", a.storage.name)
	}
	key := labels.Key()
	if a.labels == nil {
		a.labels = make(map[string]Labels)
	}
	a.labels[key] = labels
	a.pending[key] = append(a.pending[key], Sample{Timestamp: t, Value: v})
	return nil
}

func (a *remoteAppender) Commit() error {
	a.storage.mu.Lock()
	defer a.storage.mu.Unlock()
	for key, samples := range a.pending {
		existing, ok := a.storage.data[key]
		if !ok {
			lbs := a.labels[key]
			existing = &Series{Lbs: lbs}
			a.storage.data[key] = existing
		}
		existing.Samples = append(existing.Samples, samples...)
		sort.Slice(existing.Samples, func(i, j int) bool {
			return existing.Samples[i].Timestamp < existing.Samples[j].Timestamp
		})
	}
	a.pending = make(map[string][]Sample)
	return nil
}

func (a *remoteAppender) Rollback() error {
	a.pending = make(map[string][]Sample)
	return nil
}

// remoteQuerier는 RemoteStorage용 Querier이다.
type remoteQuerier struct {
	storage *RemoteStorage
	mint    int64
	maxt    int64
}

func (q *remoteQuerier) Select(matchers ...Matcher) SeriesSet {
	q.storage.mu.RLock()
	defer q.storage.mu.RUnlock()

	var result []Series
	for _, s := range q.storage.data {
		if !s.Lbs.Matches(matchers) {
			continue
		}
		var filtered []Sample
		for _, sample := range s.Samples {
			if sample.Timestamp >= q.mint && sample.Timestamp <= q.maxt {
				filtered = append(filtered, sample)
			}
		}
		if len(filtered) > 0 {
			result = append(result, Series{Lbs: s.Lbs, Samples: filtered})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Lbs.Key() < result[j].Lbs.Key()
	})
	return newSliceSeriesSet(result)
}

func (q *remoteQuerier) Close() error { return nil }

// ─── FanoutStorage ──────────────────────────────────────────────────────────

// FanoutStorage는 primary와 여러 secondary 스토리지를 팬아웃한다.
// 원본: prometheus/storage/fanout.go의 fanout struct
//
// 쓰기 시맨틱:
//   - primary에 먼저 쓰고, primary 실패 시 즉시 에러 반환
//   - secondary에도 쓰되, secondary 실패는 경고만 출력
//
// 읽기 시맨틱:
//   - primary Querier 생성 실패 → 전체 실패
//   - secondary Querier 생성 실패 → 경고 출력, 해당 secondary 건너뜀
//   - 모든 Querier 결과를 MergeQuerier로 병합
type FanoutStorage struct {
	primary     Storage
	secondaries []Storage
}

func NewFanoutStorage(primary Storage, secondaries ...Storage) *FanoutStorage {
	return &FanoutStorage{
		primary:     primary,
		secondaries: secondaries,
	}
}

func (f *FanoutStorage) Appender() Appender {
	primary := f.primary.Appender()
	secondaries := make([]Appender, 0, len(f.secondaries))
	for _, s := range f.secondaries {
		secondaries = append(secondaries, s.Appender())
	}
	return &fanoutAppender{
		primary:     primary,
		secondaries: secondaries,
	}
}

func (f *FanoutStorage) Querier(mint, maxt int64) (Querier, error) {
	// 원본 fanout.go:74-98 - primary 실패 시 즉시 에러 반환
	primary, err := f.primary.Querier(mint, maxt)
	if err != nil {
		return nil, fmt.Errorf("primary querier failed: %w", err)
	}

	secondaryQueriers := make([]Querier, 0, len(f.secondaries))
	for _, s := range f.secondaries {
		q, err := s.Querier(mint, maxt)
		if err != nil {
			// 원본: secondary 실패는 경고만 출력하고 계속 진행
			fmt.Printf("  [WARNING] secondary querier failed (skipping): %v\n", err)
			continue
		}
		secondaryQueriers = append(secondaryQueriers, q)
	}

	return NewMergeQuerier(primary, secondaryQueriers), nil
}

func (f *FanoutStorage) Close() error {
	err := f.primary.Close()
	for _, s := range f.secondaries {
		if e := s.Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
}

// ─── FanoutAppender ─────────────────────────────────────────────────────────

// fanoutAppender는 모든 스토리지에 데이터를 팬아웃 쓰기한다.
// 원본: prometheus/storage/fanout.go의 fanoutAppender struct
type fanoutAppender struct {
	primary     Appender
	secondaries []Appender
}

func (a *fanoutAppender) Append(labels Labels, t int64, v float64) error {
	// 원본 fanout.go:181-193 - primary 실패 → 즉시 에러 반환
	if err := a.primary.Append(labels, t, v); err != nil {
		return fmt.Errorf("primary append failed: %w", err)
	}

	// secondary 실패 → 경고만 출력
	for _, sec := range a.secondaries {
		if err := sec.Append(labels, t, v); err != nil {
			fmt.Printf("  [WARNING] secondary append failed (continuing): %v\n", err)
		}
	}
	return nil
}

func (a *fanoutAppender) Commit() error {
	// 원본 fanout.go:265-278 - primary commit 실패 시 secondary는 rollback
	if err := a.primary.Commit(); err != nil {
		// primary 실패 → secondary들 rollback
		for _, sec := range a.secondaries {
			if rbErr := sec.Rollback(); rbErr != nil {
				fmt.Printf("  [WARNING] secondary rollback error on commit: %v\n", rbErr)
			}
		}
		return fmt.Errorf("primary commit failed: %w", err)
	}

	// primary 성공 → secondary들도 commit
	for _, sec := range a.secondaries {
		if err := sec.Commit(); err != nil {
			fmt.Printf("  [WARNING] secondary commit failed: %v\n", err)
		}
	}
	return nil
}

func (a *fanoutAppender) Rollback() error {
	// 원본 fanout.go:280-293 - 모든 appender rollback
	err := a.primary.Rollback()
	for _, sec := range a.secondaries {
		if rbErr := sec.Rollback(); rbErr != nil {
			if err == nil {
				err = rbErr
			}
		}
	}
	return err
}

// ─── MergeQuerier ───────────────────────────────────────────────────────────

// MergeQuerier는 여러 Querier의 결과를 병합한다.
// 원본: prometheus/storage/merge.go의 mergeGenericQuerier
//
// 동작:
//   - 각 Querier에서 Select 결과를 가져옴
//   - 동일 레이블의 시리즈는 샘플을 병합 (중복 제거)
//   - secondary 에러는 경고로 처리
type MergeQuerierImpl struct {
	primary     Querier
	secondaries []Querier
}

func NewMergeQuerier(primary Querier, secondaries []Querier) Querier {
	return &MergeQuerierImpl{
		primary:     primary,
		secondaries: secondaries,
	}
}

func (q *MergeQuerierImpl) Select(matchers ...Matcher) SeriesSet {
	// primary에서 결과 수집
	primarySet := q.primary.Select(matchers...)

	// secondary에서 결과 수집
	secondarySets := make([]SeriesSet, 0, len(q.secondaries))
	for _, sec := range q.secondaries {
		ss := sec.Select(matchers...)
		if ss.Err() != nil {
			// secondary 에러는 경고만 출력
			fmt.Printf("  [WARNING] secondary select error: %v\n", ss.Err())
			continue
		}
		secondarySets = append(secondarySets, ss)
	}

	// 모든 SeriesSet의 결과를 수집
	seriesMap := make(map[string]*Series)

	// primary 결과 먼저 수집 (우선순위 높음)
	collectSeries(primarySet, seriesMap)

	// secondary 결과 병합 (중복 샘플 제거)
	for _, ss := range secondarySets {
		collectSeries(ss, seriesMap)
	}

	// 정렬된 결과 반환
	var result []Series
	for _, s := range seriesMap {
		// 샘플 타임스탬프 기준 정렬 및 중복 제거
		s.Samples = deduplicateSamples(s.Samples)
		result = append(result, *s)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Lbs.Key() < result[j].Lbs.Key()
	})
	return newSliceSeriesSet(result)
}

// collectSeries는 SeriesSet에서 시리즈를 수집하여 map에 병합한다.
func collectSeries(ss SeriesSet, target map[string]*Series) {
	for ss.Next() {
		s := ss.At()
		key := s.Lbs.Key()
		existing, ok := target[key]
		if !ok {
			copied := Series{Lbs: s.Lbs, Samples: make([]Sample, len(s.Samples))}
			copy(copied.Samples, s.Samples)
			target[key] = &copied
		} else {
			// 동일 레이블 시리즈 → 샘플 병합
			existing.Samples = append(existing.Samples, s.Samples...)
		}
	}
}

// deduplicateSamples는 타임스탬프 기준 정렬 후 중복 샘플을 제거한다.
// 원본: merge.go의 ChainedSeriesMerge 함수가 이 역할을 수행
func deduplicateSamples(samples []Sample) []Sample {
	if len(samples) == 0 {
		return samples
	}
	sort.Slice(samples, func(i, j int) bool {
		return samples[i].Timestamp < samples[j].Timestamp
	})
	result := []Sample{samples[0]}
	for i := 1; i < len(samples); i++ {
		if samples[i].Timestamp != result[len(result)-1].Timestamp {
			result = append(result, samples[i])
		}
		// 동일 타임스탬프 → 첫 번째 값 유지 (primary 우선)
	}
	return result
}

func (q *MergeQuerierImpl) Close() error {
	err := q.primary.Close()
	for _, sec := range q.secondaries {
		if e := sec.Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
}

// ─── Helper ─────────────────────────────────────────────────────────────────

func printSeriesSet(ss SeriesSet) {
	count := 0
	for ss.Next() {
		s := ss.At()
		fmt.Printf("    시리즈 %s:\n", s.Lbs)
		for _, sample := range s.Samples {
			fmt.Printf("      t=%d v=%.1f\n", sample.Timestamp, sample.Value)
		}
		count++
	}
	if count == 0 {
		fmt.Println("    (결과 없음)")
	}
	if ss.Err() != nil {
		fmt.Printf("    에러: %v\n", ss.Err())
	}
}

// ─── Main: 데모 시나리오 ────────────────────────────────────────────────────

func main() {
	fmt.Println("==========================================================")
	fmt.Println(" Prometheus Fanout Storage PoC")
	fmt.Println(" 원본: storage/fanout.go, storage/merge.go")
	fmt.Println("==========================================================")

	// 스토리지 생성
	local := NewLocalStorage()
	remote := NewRemoteStorage("remote-cortex")
	fanout := NewFanoutStorage(local, remote)

	cpuLabels := Labels{{Name: "__name__", Value: "cpu_usage"}, {Name: "instance", Value: "server-1"}}
	memLabels := Labels{{Name: "__name__", Value: "mem_usage"}, {Name: "instance", Value: "server-1"}}

	// ─── 데모 1: 정상 쓰기 (팬아웃) ────────────────────────────────────────
	fmt.Println("\n[데모 1] 정상 쓰기 - 팬아웃으로 local + remote 모두에 기록")
	fmt.Println("----------------------------------------------------------")
	app := fanout.Appender()
	for i := int64(0); i < 5; i++ {
		ts := 1000 + i*10
		if err := app.Append(cpuLabels, ts, 45.0+float64(i)*2); err != nil {
			fmt.Printf("  Append 실패: %v\n", err)
		}
	}
	if err := app.Commit(); err != nil {
		fmt.Printf("  Commit 실패: %v\n", err)
	} else {
		fmt.Println("  Commit 성공 → local과 remote 모두에 5개 샘플 기록됨")
	}

	// ─── 데모 2: 정상 읽기 (병합) ──────────────────────────────────────────
	fmt.Println("\n[데모 2] 정상 읽기 - MergeQuerier로 local + remote 결과 병합")
	fmt.Println("----------------------------------------------------------")
	q, err := fanout.Querier(0, 2000)
	if err != nil {
		fmt.Printf("  Querier 생성 실패: %v\n", err)
	} else {
		ss := q.Select(Matcher{Name: "__name__", Value: "cpu_usage"})
		printSeriesSet(ss)
		fmt.Println("  → 양쪽 모두 같은 데이터이므로 중복 제거 후 5개 샘플만 반환")
		q.Close()
	}

	// ─── 데모 3: Remote 실패 시 쓰기 → 경고만 (best-effort) ────────────────
	fmt.Println("\n[데모 3] Remote 실패 시 쓰기 - secondary 실패는 경고만")
	fmt.Println("----------------------------------------------------------")
	remote.SetFailNext(true)
	app = fanout.Appender()
	for i := int64(0); i < 3; i++ {
		ts := 2000 + i*10
		if err := app.Append(memLabels, ts, 70.0+float64(i)); err != nil {
			fmt.Printf("  Append 에러: %v\n", err)
		}
	}
	if err := app.Commit(); err != nil {
		fmt.Printf("  Commit 실패: %v\n", err)
	} else {
		fmt.Println("  Commit 성공 → remote 실패했지만 local에는 기록됨 (best-effort)")
	}
	remote.SetFailNext(false)

	// 로컬에서 mem_usage 확인
	fmt.Println("\n  [확인] 로컬에만 mem_usage 존재:")
	lq, _ := local.Querier(0, 3000)
	printSeriesSet(lq.Select(Matcher{Name: "__name__", Value: "mem_usage"}))
	lq.Close()

	// ─── 데모 4: Local(Primary) 실패 → 전체 실패 ───────────────────────────
	fmt.Println("\n[데모 4] Local(Primary) 실패 → 전체 실패")
	fmt.Println("----------------------------------------------------------")
	local.SetFailNext(true)
	app = fanout.Appender()
	err = app.Append(cpuLabels, 3000, 99.9)
	if err != nil {
		fmt.Printf("  Append 실패: %v\n", err)
		fmt.Println("  → primary 실패 시 즉시 에러 반환 (secondary 시도 안 함)")
	}
	local.SetFailNext(false)

	// Querier도 primary 실패 시 에러
	local.SetFailNext(true)
	_, err = fanout.Querier(0, 3000)
	if err != nil {
		fmt.Printf("  Querier 생성 실패: %v\n", err)
		fmt.Println("  → primary Querier 실패 시 전체 조회 실패")
	}
	local.SetFailNext(false)

	// ─── 데모 5: Remote에만 있는 데이터 조회 ───────────────────────────────
	fmt.Println("\n[데모 5] Remote에만 있는 데이터 조회")
	fmt.Println("----------------------------------------------------------")
	diskLabels := Labels{{Name: "__name__", Value: "disk_usage"}, {Name: "instance", Value: "server-2"}}
	remote.DirectInsert(diskLabels, []Sample{
		{Timestamp: 5000, Value: 80.0},
		{Timestamp: 5010, Value: 82.5},
		{Timestamp: 5020, Value: 85.0},
	})
	fmt.Println("  remote에 disk_usage 3개 샘플 직접 삽입 (다른 인스턴스가 쓴 데이터 시뮬레이션)")

	q, err = fanout.Querier(0, 6000)
	if err != nil {
		fmt.Printf("  Querier 생성 실패: %v\n", err)
	} else {
		ss := q.Select(Matcher{Name: "__name__", Value: "disk_usage"})
		printSeriesSet(ss)
		fmt.Println("  → local에는 없지만 remote에서 가져온 데이터가 반환됨")
		q.Close()
	}

	// ─── 데모 6: 중복 제거 (양쪽에 같은 시리즈, 다른 시간대) ───────────────
	fmt.Println("\n[데모 6] 중복 제거 - 양쪽에 같은 시리즈가 있을 때 병합")
	fmt.Println("----------------------------------------------------------")

	// local에 cpu_usage 추가 데이터 (t=1050~1060)
	lApp := local.Appender()
	lApp.Append(cpuLabels, 1050, 60.0)
	lApp.Append(cpuLabels, 1060, 62.0)
	lApp.Commit()

	// remote에 cpu_usage 일부 겹치는 데이터 (t=1040 겹침, t=1070 새로운)
	remote.DirectInsert(cpuLabels, []Sample{
		{Timestamp: 1040, Value: 53.0}, // local과 겹침 (값 동일 가정)
		{Timestamp: 1070, Value: 65.0}, // remote에만 있는 데이터
	})

	fmt.Println("  Local:  cpu_usage t=1000,1010,1020,1030,1040,1050,1060")
	fmt.Println("  Remote: cpu_usage t=1000,1010,1020,1030,1040 (fanout) + t=1040,1070 (직접 삽입)")
	fmt.Println("\n  MergeQuerier 결과 (중복 제거):")

	q, err = fanout.Querier(0, 2000)
	if err != nil {
		fmt.Printf("  Querier 생성 실패: %v\n", err)
	} else {
		ss := q.Select(Matcher{Name: "__name__", Value: "cpu_usage"})
		printSeriesSet(ss)
		fmt.Println("  → 동일 타임스탬프는 중복 제거되어 하나만 남음")
		q.Close()
	}

	// ─── 데모 7: Remote Querier 실패 시 → primary 결과만 반환 ──────────────
	fmt.Println("\n[데모 7] Remote Querier 실패 시 → primary 결과만 반환")
	fmt.Println("----------------------------------------------------------")
	remote.SetFailNext(true)
	q, err = fanout.Querier(0, 2000)
	if err != nil {
		fmt.Printf("  Querier 생성 실패: %v\n", err)
	} else {
		fmt.Println("  (secondary querier 생성 실패 경고가 위에 출력됨)")
		ss := q.Select(Matcher{Name: "__name__", Value: "cpu_usage"})
		printSeriesSet(ss)
		fmt.Println("  → remote 실패해도 local 결과는 정상 반환")
		q.Close()
	}
	remote.SetFailNext(false)

	fanout.Close()

	fmt.Println("\n==========================================================")
	fmt.Println(" Fanout Storage 핵심 정리")
	fmt.Println("==========================================================")
	fmt.Println(" 1. Primary(로컬 TSDB) 실패 = 전체 실패")
	fmt.Println(" 2. Secondary(원격) 실패 = 경고만 (best-effort)")
	fmt.Println(" 3. 읽기 시 MergeQuerier가 모든 소스의 결과를 병합")
	fmt.Println(" 4. 동일 레이블 시리즈의 중복 샘플은 타임스탬프 기준 제거")
	fmt.Println(" 5. Commit 실패 시 나머지 appender는 Rollback 처리")
	fmt.Println("==========================================================")
}
