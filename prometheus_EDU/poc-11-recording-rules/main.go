package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Prometheus Recording Rules PoC
//
// 실제 소스 참조:
//   - rules/recording.go  : RecordingRule.Eval() - 쿼리 실행 후 메트릭 이름/라벨 재작성
//   - rules/group.go      : Group.Eval() - 규칙 평가, seriesInPreviousEval로 stale 감지
//   - model/value/value.go : StaleNaN - 시리즈 소멸 마커
//
// Recording Rule 핵심 동작:
//   1. PromQL 표현식 실행 → 결과 벡터 획득
//   2. __name__ 을 recording rule 이름으로 교체
//   3. 추가 라벨(labels) 병합
//   4. 결과를 TSDB에 Append
//   5. 이전 평가 시리즈와 비교하여 사라진 시리즈에 StaleNaN 기록
// =============================================================================

// StaleNaN은 Prometheus가 시리즈 소멸을 표시하는 특수 NaN 값.
// 실제 구현: math.Float64frombits(value.StaleNaN) — 특정 비트 패턴의 NaN
var StaleNaN = math.Float64frombits(0x7ff0000000000002)

// =============================================================================
// Sample: 단일 시계열 데이터 포인트
// =============================================================================

// Labels는 정렬된 key-value 쌍으로 시리즈를 고유하게 식별한다.
type Labels map[string]string

func (l Labels) String() string {
	if len(l) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%q", k, l[k])
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// Hash는 라벨 집합의 동일성 비교를 위한 키를 반환한다.
func (l Labels) Hash() string {
	return l.String()
}

// Copy는 라벨의 깊은 복사본을 생성한다.
func (l Labels) Copy() Labels {
	cp := make(Labels, len(l))
	for k, v := range l {
		cp[k] = v
	}
	return cp
}

type Sample struct {
	Metric    Labels
	Timestamp time.Time
	Value     float64
}

func (s Sample) String() string {
	if math.IsNaN(s.Value) && math.Float64bits(s.Value) == math.Float64bits(StaleNaN) {
		return fmt.Sprintf("%s %s StaleNaN", s.Metric, s.Timestamp.Format("15:04:05"))
	}
	return fmt.Sprintf("%s %s %.1f", s.Metric, s.Timestamp.Format("15:04:05"), s.Value)
}

// =============================================================================
// InMemoryStorage: 간단한 인메모리 TSDB
// =============================================================================

type InMemoryStorage struct {
	mu      sync.RWMutex
	samples []Sample
}

func NewInMemoryStorage() *InMemoryStorage {
	return &InMemoryStorage{}
}

func (s *InMemoryStorage) Append(sample Sample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.samples = append(s.samples, sample)
}

// Query는 주어진 메트릭 이름으로 특정 시각의 최신 샘플을 반환한다.
// labelMatch가 nil이 아니면 해당 라벨도 일치해야 한다.
func (s *InMemoryStorage) Query(metricName string, ts time.Time, labelMatch Labels) []Sample {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// 각 고유 라벨셋의 가장 최근(ts 이하) 샘플을 찾는다.
	latest := make(map[string]Sample)
	for _, sample := range s.samples {
		if sample.Metric["__name__"] != metricName {
			continue
		}
		if sample.Timestamp.After(ts) {
			continue
		}
		// StaleNaN 샘플은 시리즈 소멸을 의미 — 결과에서 제외
		if math.IsNaN(sample.Value) && math.Float64bits(sample.Value) == math.Float64bits(StaleNaN) {
			delete(latest, sample.Metric.Hash())
			continue
		}
		// labelMatch 필터
		if labelMatch != nil {
			match := true
			for k, v := range labelMatch {
				if sample.Metric[k] != v {
					match = false
					break
				}
			}
			if !match {
				continue
			}
		}
		key := sample.Metric.Hash()
		existing, ok := latest[key]
		if !ok || sample.Timestamp.After(existing.Timestamp) {
			latest[key] = sample
		}
	}

	result := make([]Sample, 0, len(latest))
	for _, s := range latest {
		result = append(result, s)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Metric.Hash() < result[j].Metric.Hash()
	})
	return result
}

// QueryAll은 특정 메트릭의 전체 시계열을 반환한다 (디버그용).
func (s *InMemoryStorage) QueryAll(metricName string) []Sample {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []Sample
	for _, sample := range s.samples {
		if sample.Metric["__name__"] == metricName {
			result = append(result, sample)
		}
	}
	return result
}

// =============================================================================
// QueryFunc: 간소화된 PromQL 쿼리 함수
// =============================================================================

// QueryFunc는 PromQL 표현식을 평가하여 벡터를 반환한다.
// 실제 Prometheus에서는 promql.Engine.Query()를 사용한다.
type QueryFunc func(ts time.Time) []Sample

// =============================================================================
// RecordingRule: 쿼리 결과를 새 시리즈로 기록
//
// 실제 소스 (rules/recording.go:84-122):
//   func (rule *RecordingRule) Eval(...) {
//       vector, err := query(ctx, rule.vector.String(), ts)
//       lb := labels.NewBuilder(labels.EmptyLabels())
//       for i := range vector {
//           lb.Reset(sample.Metric)
//           lb.Set(labels.MetricName, rule.name)  // 이름 교체
//           rule.labels.Range(func(l labels.Label) {
//               lb.Set(l.Name, l.Value)            // 추가 라벨 병합
//           })
//           sample.Metric = lb.Labels()
//       }
//   }
// =============================================================================

type RecordingRule struct {
	name       string     // 새 메트릭 이름 (예: job:http_requests_total:sum)
	queryFunc  QueryFunc  // 표현식 평가 함수
	labels     Labels     // 추가/오버라이드할 라벨
	exprString string     // 표현식 문자열 (로깅용)
}

func NewRecordingRule(name string, expr string, queryFunc QueryFunc, labels Labels) *RecordingRule {
	return &RecordingRule{
		name:       name,
		queryFunc:  queryFunc,
		labels:     labels,
		exprString: expr,
	}
}

// Eval은 쿼리를 실행하고 결과의 메트릭 이름과 라벨을 재작성한다.
// 실제 RecordingRule.Eval()의 핵심 로직을 재현한다.
func (r *RecordingRule) Eval(ts time.Time) []Sample {
	// 1. 쿼리 실행 → 원본 벡터 획득
	vector := r.queryFunc(ts)

	// 2. 메트릭 이름과 라벨 재작성
	result := make([]Sample, 0, len(vector))
	seen := make(map[string]bool)

	for _, s := range vector {
		newMetric := s.Metric.Copy()

		// __name__을 recording rule 이름으로 교체
		newMetric["__name__"] = r.name

		// 추가 라벨 병합 (오버라이드 가능)
		for k, v := range r.labels {
			newMetric[k] = v
		}

		// 중복 라벨셋 검사 (실제 코드: vector.ContainsSameLabelset())
		hash := newMetric.Hash()
		if seen[hash] {
			fmt.Printf("  [WARNING] 중복 라벨셋 감지: %s\n", hash)
			continue
		}
		seen[hash] = true

		result = append(result, Sample{
			Metric:    newMetric,
			Timestamp: ts,
			Value:     s.Value,
		})
	}

	return result
}

// =============================================================================
// Rule 인터페이스
// =============================================================================

type Rule interface {
	Name() string
	Eval(ts time.Time) []Sample
}

func (r *RecordingRule) Name() string { return r.name }

// =============================================================================
// RuleGroup: 규칙 그룹 — 주기적 평가 + Stale 시리즈 처리
//
// 실제 소스 (rules/group.go:45-77):
//   type Group struct {
//       name                 string
//       interval             time.Duration
//       rules                []Rule
//       seriesInPreviousEval []map[string]labels.Labels  // 이전 평가 시리즈 추적
//   }
//
// Stale 시리즈 처리 (rules/group.go:620-639):
//   for metric, lset := range g.seriesInPreviousEval[i] {
//       if _, ok := seriesReturned[metric]; !ok {
//           app.Append(0, lset, ts, math.Float64frombits(value.StaleNaN))
//       }
//   }
// =============================================================================

type RuleGroup struct {
	name     string
	interval time.Duration
	rules    []Rule
	storage  *InMemoryStorage

	// seriesInPreviousEval: 각 Rule 인덱스별로 이전 평가에서 반환된 시리즈 추적.
	// 실제 구현과 동일한 구조: []map[string]Labels
	seriesInPreviousEval []map[string]Labels
}

func NewRuleGroup(name string, interval time.Duration, storage *InMemoryStorage, rules ...Rule) *RuleGroup {
	prevEval := make([]map[string]Labels, len(rules))
	for i := range prevEval {
		prevEval[i] = make(map[string]Labels)
	}
	return &RuleGroup{
		name:                 name,
		interval:             interval,
		rules:                rules,
		storage:              storage,
		seriesInPreviousEval: prevEval,
	}
}

// EvalTimestamp는 평가 시각을 interval에 정렬한다.
// 실제 구현 (rules/group.go:422-445): hash 기반 offset으로 그룹 간 분산.
// PoC에서는 단순 정렬만 수행한다.
func (g *RuleGroup) EvalTimestamp(now time.Time) time.Time {
	intervalNs := g.interval.Nanoseconds()
	aligned := now.UnixNano() - (now.UnixNano() % intervalNs)
	return time.Unix(0, aligned).UTC()
}

// Eval은 그룹 내 모든 규칙을 평가하고 결과를 저장한다.
// 핵심: 이전 평가와 비교하여 사라진 시리즈에 StaleNaN을 기록.
func (g *RuleGroup) Eval(ts time.Time) {
	for i, rule := range g.rules {
		// 1. 규칙 평가
		vector := rule.Eval(ts)

		// 2. 현재 평가에서 반환된 시리즈 추적
		seriesReturned := make(map[string]Labels, len(vector))

		for _, s := range vector {
			// 결과를 저장소에 Append
			g.storage.Append(s)
			seriesReturned[s.Metric.Hash()] = s.Metric.Copy()
		}

		// 3. Stale 시리즈 처리
		// 이전 평가에서 있었지만 현재 평가에서 사라진 시리즈 → StaleNaN 기록
		// 실제 코드 (group.go:620-639):
		//   for metric, lset := range g.seriesInPreviousEval[i] {
		//       if _, ok := seriesReturned[metric]; !ok {
		//           app.Append(0, lset, ts, StaleNaN)
		//       }
		//   }
		staleCount := 0
		for metric, lset := range g.seriesInPreviousEval[i] {
			if _, ok := seriesReturned[metric]; !ok {
				// 시리즈가 사라짐 → StaleNaN 마커 기록
				g.storage.Append(Sample{
					Metric:    lset,
					Timestamp: ts,
					Value:     StaleNaN,
				})
				staleCount++
			}
		}
		if staleCount > 0 {
			fmt.Printf("  [STALE] %s: %d개 시리즈에 StaleNaN 기록\n", rule.Name(), staleCount)
		}

		// 4. 현재 시리즈를 다음 평가를 위해 저장
		g.seriesInPreviousEval[i] = seriesReturned
	}
}

// Run은 지정된 횟수만큼 평가 사이클을 실행한다.
func (g *RuleGroup) Run(baseTime time.Time, cycles int, preEvalHook func(cycle int, ts time.Time)) {
	for cycle := 0; cycle < cycles; cycle++ {
		ts := baseTime.Add(time.Duration(cycle) * g.interval)
		aligned := g.EvalTimestamp(ts)

		fmt.Printf("\n=== 평가 사이클 %d | %s (정렬: %s) ===\n",
			cycle+1, ts.Format("15:04:05"), aligned.Format("15:04:05"))

		// 평가 전 후크 (소스 메트릭 변경 등)
		if preEvalHook != nil {
			preEvalHook(cycle, ts)
		}

		g.Eval(ts)

		// 각 규칙의 결과 출력
		for _, rule := range g.rules {
			results := g.storage.Query(rule.Name(), ts, nil)
			fmt.Printf("  [%s] %d개 시리즈:\n", rule.Name(), len(results))
			for _, r := range results {
				fmt.Printf("    %s\n", r)
			}
		}
	}
}

// =============================================================================
// 집계 함수: 간소화된 PromQL 연산
// =============================================================================

// SumBy는 "sum by (groupLabels) (metric)" 을 시뮬레이션한다.
func SumBy(storage *InMemoryStorage, metricName string, groupLabels []string) QueryFunc {
	return func(ts time.Time) []Sample {
		raw := storage.Query(metricName, ts, nil)
		groups := make(map[string]*Sample)

		for _, s := range raw {
			// 그룹 키 생성
			keyLabels := make(Labels)
			for _, gl := range groupLabels {
				if v, ok := s.Metric[gl]; ok {
					keyLabels[gl] = v
				}
			}
			key := keyLabels.Hash()

			if existing, ok := groups[key]; ok {
				existing.Value += s.Value
			} else {
				newSample := Sample{
					Metric: keyLabels,
					Value:  s.Value,
				}
				groups[key] = &newSample
			}
		}

		result := make([]Sample, 0, len(groups))
		for _, s := range groups {
			result = append(result, *s)
		}
		sort.Slice(result, func(i, j int) bool {
			return result[i].Metric.Hash() < result[j].Metric.Hash()
		})
		return result
	}
}

// RateSimulation은 "rate(metric[5m])" 을 시뮬레이션한다.
// 실제로는 두 시점의 차이를 기간으로 나누지만, PoC에서는 현재값 / 300(5분) 으로 근사한다.
func RateSimulation(storage *InMemoryStorage, metricName string, groupLabels []string) QueryFunc {
	return func(ts time.Time) []Sample {
		raw := storage.Query(metricName, ts, nil)
		groups := make(map[string]*Sample)

		for _, s := range raw {
			keyLabels := make(Labels)
			for _, gl := range groupLabels {
				if v, ok := s.Metric[gl]; ok {
					keyLabels[gl] = v
				}
			}
			key := keyLabels.Hash()

			if existing, ok := groups[key]; ok {
				existing.Value += s.Value / 300.0 // rate = counter / 5min
			} else {
				newSample := Sample{
					Metric: keyLabels,
					Value:  s.Value / 300.0,
				}
				groups[key] = &newSample
			}
		}

		result := make([]Sample, 0, len(groups))
		for _, s := range groups {
			result = append(result, *s)
		}
		sort.Slice(result, func(i, j int) bool {
			return result[i].Metric.Hash() < result[j].Metric.Hash()
		})
		return result
	}
}

// =============================================================================
// main: 데모 시나리오
// =============================================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║    Prometheus Recording Rules PoC                           ║")
	fmt.Println("║    참조: rules/recording.go, rules/group.go                 ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")

	storage := NewInMemoryStorage()
	baseTime := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)

	// =========================================================================
	// 1. 원본 메트릭 투입: http_requests_total (고카디널리티)
	// =========================================================================
	fmt.Println("\n[1] 원본 메트릭 투입: http_requests_total")
	fmt.Println("    (job, method, status, handler 라벨 → 고카디널리티)")
	fmt.Println(strings.Repeat("-", 60))

	rawMetrics := []struct {
		job     string
		method  string
		status  string
		handler string
		value   float64
	}{
		{"api-server", "GET", "200", "/users", 15000},
		{"api-server", "GET", "200", "/orders", 8500},
		{"api-server", "GET", "404", "/users", 120},
		{"api-server", "POST", "200", "/users", 3200},
		{"api-server", "POST", "500", "/orders", 45},
		{"web-frontend", "GET", "200", "/index", 42000},
		{"web-frontend", "GET", "200", "/static", 95000},
		{"web-frontend", "GET", "304", "/static", 12000},
		{"web-frontend", "POST", "200", "/login", 5600},
		{"payment-svc", "POST", "200", "/charge", 1800},
		{"payment-svc", "POST", "502", "/charge", 23},
		{"payment-svc", "GET", "200", "/status", 9500},
	}

	for _, m := range rawMetrics {
		storage.Append(Sample{
			Metric: Labels{
				"__name__": "http_requests_total",
				"job":      m.job,
				"method":   m.method,
				"status":   m.status,
				"handler":  m.handler,
			},
			Timestamp: baseTime,
			Value:     m.value,
		})
	}
	fmt.Printf("  투입: %d개 시리즈 (12개 고유 라벨 조합)\n", len(rawMetrics))

	// =========================================================================
	// 2. Recording Rule 정의
	// =========================================================================
	fmt.Println("\n[2] Recording Rule 정의")
	fmt.Println(strings.Repeat("-", 60))

	// Rule 1: sum by (job) (http_requests_total)
	// 12개 시리즈 → 3개 시리즈로 카디널리티 감소
	rule1 := NewRecordingRule(
		"job:http_requests_total:sum",
		"sum by (job) (http_requests_total)",
		SumBy(storage, "http_requests_total", []string{"job"}),
		nil, // 추가 라벨 없음
	)
	fmt.Printf("  Rule 1: %s\n", rule1.name)
	fmt.Printf("    표현식: %s\n", rule1.exprString)
	fmt.Printf("    카디널리티: 12 → 3 (75%% 감소)\n")

	// Rule 2: sum by (job, method) (rate(http_requests_total[5m]))
	// 추가 라벨로 환경 정보 태그
	rule2 := NewRecordingRule(
		"job_method:http_requests:rate5m",
		"sum by (job, method) (rate(http_requests_total[5m]))",
		RateSimulation(storage, "http_requests_total", []string{"job", "method"}),
		Labels{"env": "production"}, // 추가 라벨
	)
	fmt.Printf("  Rule 2: %s\n", rule2.name)
	fmt.Printf("    표현식: %s\n", rule2.exprString)
	fmt.Printf("    추가 라벨: env=production\n")
	fmt.Printf("    카디널리티: 12 → 5 (58%% 감소)\n")

	// =========================================================================
	// 3. RuleGroup 생성 및 평가 실행
	// =========================================================================
	fmt.Println("\n[3] RuleGroup 평가 실행 (5 사이클, 1분 간격)")
	fmt.Println(strings.Repeat("-", 60))

	group := NewRuleGroup(
		"http_recording_rules",
		1*time.Minute,
		storage,
		rule1, rule2,
	)

	// 메트릭 변경 시나리오를 위한 후크
	preEvalHook := func(cycle int, ts time.Time) {
		switch cycle {
		case 2:
			// 사이클 3에서 카운터 증가 (정상 동작)
			fmt.Println("  [EVENT] 카운터 값 증가 (트래픽 증가)")
			for _, m := range rawMetrics {
				storage.Append(Sample{
					Metric: Labels{
						"__name__": "http_requests_total",
						"job":      m.job,
						"method":   m.method,
						"status":   m.status,
						"handler":  m.handler,
					},
					Timestamp: ts,
					Value:     m.value * 1.5, // 50% 트래픽 증가
				})
			}

		case 3:
			// 사이클 4에서 payment-svc 제거 → stale 시리즈 발생
			fmt.Println("  [EVENT] payment-svc 종료 → 해당 시리즈 소멸 예정")
			// payment-svc를 제외한 메트릭만 갱신
			for _, m := range rawMetrics {
				if m.job == "payment-svc" {
					continue // payment-svc 메트릭 미갱신 → 소멸
				}
				storage.Append(Sample{
					Metric: Labels{
						"__name__": "http_requests_total",
						"job":      m.job,
						"method":   m.method,
						"status":   m.status,
						"handler":  m.handler,
					},
					Timestamp: ts,
					Value:     m.value * 2.0,
				})
			}
			// payment-svc 시리즈에 StaleNaN 기록하여 소멸 표시
			for _, m := range rawMetrics {
				if m.job != "payment-svc" {
					continue
				}
				storage.Append(Sample{
					Metric: Labels{
						"__name__": "http_requests_total",
						"job":      m.job,
						"method":   m.method,
						"status":   m.status,
						"handler":  m.handler,
					},
					Timestamp: ts,
					Value:     StaleNaN,
				})
			}

		case 4:
			// 사이클 5에서는 payment-svc 없이 정상 동작
			fmt.Println("  [EVENT] payment-svc 없이 정상 동작 계속")
			for _, m := range rawMetrics {
				if m.job == "payment-svc" {
					continue
				}
				storage.Append(Sample{
					Metric: Labels{
						"__name__": "http_requests_total",
						"job":      m.job,
						"method":   m.method,
						"status":   m.status,
						"handler":  m.handler,
					},
					Timestamp: ts,
					Value:     m.value * 2.5,
				})
			}
		}
	}

	group.Run(baseTime, 5, preEvalHook)

	// =========================================================================
	// 4. 사전 계산된 결과 조회 vs 원본 비교
	// =========================================================================
	fmt.Println("\n" + strings.Repeat("=", 60))
	fmt.Println("[4] Recording Rule 결과 vs 원본 데이터 비교")
	fmt.Println(strings.Repeat("=", 60))

	queryTime := baseTime // 첫 번째 사이클 시점

	fmt.Println("\n--- 원본: http_requests_total (12개 시리즈) ---")
	rawResults := storage.Query("http_requests_total", queryTime, nil)
	fmt.Printf("  시리즈 수: %d\n", len(rawResults))
	for _, r := range rawResults {
		fmt.Printf("    %s\n", r)
	}

	fmt.Println("\n--- Recording Rule: job:http_requests_total:sum (3개 시리즈) ---")
	sumResults := storage.Query("job:http_requests_total:sum", queryTime, nil)
	fmt.Printf("  시리즈 수: %d\n", len(sumResults))
	for _, r := range sumResults {
		fmt.Printf("    %s\n", r)
	}

	fmt.Printf("\n  카디널리티 감소: %d → %d (%.0f%% 감소)\n",
		len(rawResults), len(sumResults),
		(1.0-float64(len(sumResults))/float64(len(rawResults)))*100)

	fmt.Println("\n--- Recording Rule: job_method:http_requests:rate5m (5개 시리즈) ---")
	rateResults := storage.Query("job_method:http_requests:rate5m", queryTime, nil)
	fmt.Printf("  시리즈 수: %d\n", len(rateResults))
	for _, r := range rateResults {
		fmt.Printf("    %s\n", r)
	}

	// =========================================================================
	// 5. Stale 시리즈 추적 확인
	// =========================================================================
	fmt.Println("\n" + strings.Repeat("=", 60))
	fmt.Println("[5] Stale 시리즈 추적")
	fmt.Println(strings.Repeat("=", 60))

	fmt.Println("\n--- job:http_requests_total:sum 전체 시계열 ---")
	allSum := storage.QueryAll("job:http_requests_total:sum")
	for _, s := range allSum {
		marker := ""
		if math.IsNaN(s.Value) && math.Float64bits(s.Value) == math.Float64bits(StaleNaN) {
			marker = " ← STALE"
		}
		fmt.Printf("  %s%s\n", s, marker)
	}

	fmt.Println("\n--- Stale 시리즈 동작 설명 ---")
	fmt.Println("  사이클 4에서 payment-svc가 사라지면:")
	fmt.Println("  1. 이전 평가(seriesInPreviousEval)에 payment-svc 시리즈가 있었음")
	fmt.Println("  2. 현재 평가(seriesReturned)에 payment-svc 시리즈가 없음")
	fmt.Println("  3. 차이 감지 → StaleNaN 마커 기록 (group.go:620-639)")
	fmt.Println("  4. 이후 쿼리에서 해당 시리즈는 결과에서 제외됨")

	// =========================================================================
	// 6. 평가 시각 정렬 데모
	// =========================================================================
	fmt.Println("\n" + strings.Repeat("=", 60))
	fmt.Println("[6] 평가 시각 정렬 (EvalTimestamp)")
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println("\n  실제 구현 (group.go:422-445):")
	fmt.Println("    offset = hash(group) % interval")
	fmt.Println("    aligned = now - (now % interval) + offset")
	fmt.Println("  → 그룹 간 평가 시각이 분산되어 리소스 경쟁 완화")

	testTimes := []time.Time{
		time.Date(2024, 1, 1, 12, 0, 13, 0, time.UTC),
		time.Date(2024, 1, 1, 12, 0, 47, 0, time.UTC),
		time.Date(2024, 1, 1, 12, 1, 5, 0, time.UTC),
		time.Date(2024, 1, 1, 12, 1, 59, 0, time.UTC),
	}
	for _, t := range testTimes {
		aligned := group.EvalTimestamp(t)
		fmt.Printf("  입력: %s → 정렬: %s (간격: 1분)\n",
			t.Format("15:04:05"), aligned.Format("15:04:05"))
	}

	// =========================================================================
	// 7. 요약
	// =========================================================================
	fmt.Println("\n" + strings.Repeat("=", 60))
	fmt.Println("[요약] Recording Rules 핵심 메커니즘")
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println(`
  ┌─────────────────────────────────────────────────────────┐
  │                  Recording Rule 평가 흐름                │
  ├─────────────────────────────────────────────────────────┤
  │                                                         │
  │  1. RuleGroup.Eval(ts)                                  │
  │     ├── 각 Rule에 대해:                                  │
  │     │   ├── rule.Eval(ts)                               │
  │     │   │   ├── QueryFunc 실행 (PromQL 평가)             │
  │     │   │   ├── __name__ → recording rule 이름으로 교체   │
  │     │   │   └── 추가 라벨 병합                            │
  │     │   ├── 결과를 Storage에 Append                      │
  │     │   ├── seriesReturned 추적                          │
  │     │   └── Stale 감지:                                  │
  │     │       seriesInPreviousEval - seriesReturned        │
  │     │       → 차이에 StaleNaN 기록                       │
  │     └── seriesInPreviousEval 갱신                        │
  │                                                         │
  │  효과:                                                   │
  │  - 카디널리티 감소: 12 시리즈 → 3 시리즈 (75%)            │
  │  - 쿼리 성능: 사전 계산으로 실시간 집계 불필요              │
  │  - 시리즈 라이프사이클: StaleNaN으로 정확한 소멸 추적       │
  └─────────────────────────────────────────────────────────┘`)
	fmt.Println()
}
