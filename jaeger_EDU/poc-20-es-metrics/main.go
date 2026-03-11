package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"time"
)

// =============================================================================
// Jaeger Elasticsearch Metric Store (SPM) 시뮬레이션
// =============================================================================
//
// Jaeger의 SPM(Service Performance Monitoring)은 Elasticsearch에 저장된
// span 데이터에서 서비스별 성능 메트릭(RED: Rate, Error, Duration)을 추출한다.
//
// 핵심 개념:
//   - SPM Query: 서비스/오퍼레이션별 집계 쿼리
//   - RED Metrics: Rate(요청률), Error(에러율), Duration(지연)
//   - ES Aggregation: terms + date_histogram + avg/percentiles
//   - Time Bucket: 시간대별 메트릭 집계
//
// 실제 코드 참조:
//   - plugin/metrics/es/: ES 메트릭 스토어
// =============================================================================

// --- Span 모델 ---

type SpanRecord struct {
	TraceID     string
	SpanID      string
	Service     string
	Operation   string
	Duration    time.Duration
	StatusCode  int // 0=OK, 1=ERROR
	Timestamp   time.Time
}

// --- ES 인덱스 시뮬레이션 ---

type ESIndex struct {
	spans []SpanRecord
}

func NewESIndex() *ESIndex {
	return &ESIndex{}
}

func (idx *ESIndex) IndexSpan(span SpanRecord) {
	idx.spans = append(idx.spans, span)
}

// --- SPM Query Builder ---

type SPMQuery struct {
	Service     string
	Operation   string
	StartTime   time.Time
	EndTime     time.Time
	BucketSize  time.Duration
}

// --- RED Metrics ---

type REDMetrics struct {
	Service    string
	Operation  string
	Timestamp  time.Time
	Rate       float64 // requests per second
	ErrorRate  float64 // error ratio (0-1)
	P50        time.Duration
	P95        time.Duration
	P99        time.Duration
	AvgDuration time.Duration
	Count      int
	ErrorCount int
}

func (m REDMetrics) String() string {
	return fmt.Sprintf("%s/%s [%s] rate=%.1f/s err=%.1f%% p50=%s p95=%s p99=%s count=%d",
		m.Service, m.Operation, m.Timestamp.Format("15:04"),
		m.Rate, m.ErrorRate*100, m.P50, m.P95, m.P99, m.Count)
}

// --- SPM Aggregation Engine ---

type SPMAggregator struct {
	index *ESIndex
}

func NewSPMAggregator(index *ESIndex) *SPMAggregator {
	return &SPMAggregator{index: index}
}

// QueryRED는 RED 메트릭스를 집계한다 (ES aggregation 시뮬레이션).
func (agg *SPMAggregator) QueryRED(query SPMQuery) []REDMetrics {
	var results []REDMetrics

	// 시간 버킷별 집계
	for t := query.StartTime; t.Before(query.EndTime); t = t.Add(query.BucketSize) {
		bucketEnd := t.Add(query.BucketSize)

		var durations []time.Duration
		errorCount := 0

		for _, span := range agg.index.spans {
			if span.Timestamp.Before(t) || span.Timestamp.After(bucketEnd) {
				continue
			}
			if query.Service != "" && span.Service != query.Service {
				continue
			}
			if query.Operation != "" && span.Operation != query.Operation {
				continue
			}
			durations = append(durations, span.Duration)
			if span.StatusCode == 1 {
				errorCount++
			}
		}

		if len(durations) == 0 {
			continue
		}

		// 정렬하여 percentile 계산
		sortDurations(durations)

		count := len(durations)
		avg := avgDuration(durations)

		metrics := REDMetrics{
			Service:     query.Service,
			Operation:   query.Operation,
			Timestamp:   t,
			Rate:        float64(count) / query.BucketSize.Seconds(),
			ErrorRate:   float64(errorCount) / float64(count),
			P50:         percentile(durations, 50),
			P95:         percentile(durations, 95),
			P99:         percentile(durations, 99),
			AvgDuration: avg,
			Count:       count,
			ErrorCount:  errorCount,
		}
		results = append(results, metrics)
	}
	return results
}

// QueryServiceOperations는 서비스별 오퍼레이션 목록을 반환한다.
func (agg *SPMAggregator) QueryServiceOperations(service string) map[string]int {
	ops := make(map[string]int)
	for _, span := range agg.index.spans {
		if span.Service == service {
			ops[span.Operation]++
		}
	}
	return ops
}

// --- ES Query DSL 시뮬레이션 ---

type ESQueryDSL struct {
	Query map[string]interface{} `json:"query"`
	Aggs  map[string]interface{} `json:"aggs"`
	Size  int                    `json:"size"`
}

func buildSPMQueryDSL(query SPMQuery) ESQueryDSL {
	return ESQueryDSL{
		Size: 0,
		Query: map[string]interface{}{
			"bool": map[string]interface{}{
				"must": []interface{}{
					map[string]interface{}{
						"term": map[string]string{"process.serviceName": query.Service},
					},
					map[string]interface{}{
						"range": map[string]interface{}{
							"startTime": map[string]string{
								"gte": query.StartTime.Format(time.RFC3339),
								"lte": query.EndTime.Format(time.RFC3339),
							},
						},
					},
				},
			},
		},
		Aggs: map[string]interface{}{
			"time_buckets": map[string]interface{}{
				"date_histogram": map[string]interface{}{
					"field":    "startTime",
					"interval": fmt.Sprintf("%ds", int(query.BucketSize.Seconds())),
				},
				"aggs": map[string]interface{}{
					"operations": map[string]interface{}{
						"terms": map[string]interface{}{
							"field": "operationName",
						},
						"aggs": map[string]interface{}{
							"duration_percentiles": map[string]interface{}{
								"percentiles": map[string]interface{}{
									"field":    "duration",
									"percents": []float64{50, 95, 99},
								},
							},
							"error_count": map[string]interface{}{
								"filter": map[string]interface{}{
									"term": map[string]int{"tags.error": 1},
								},
							},
						},
					},
				},
			},
		},
	}
}

// --- 유틸리티 ---

func sortDurations(d []time.Duration) {
	for i := 1; i < len(d); i++ {
		for j := i; j > 0 && d[j] < d[j-1]; j-- {
			d[j], d[j-1] = d[j-1], d[j]
		}
	}
}

func percentile(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := (len(sorted) * p) / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func avgDuration(d []time.Duration) time.Duration {
	if len(d) == 0 {
		return 0
	}
	var total time.Duration
	for _, dur := range d {
		total += dur
	}
	return total / time.Duration(len(d))
}

func main() {
	fmt.Println("=== Jaeger ES Metric Store (SPM) 시뮬레이션 ===")
	fmt.Println()

	r := rand.New(rand.NewSource(42))
	index := NewESIndex()
	baseTime := time.Now().Add(-1 * time.Hour).Truncate(time.Minute)

	// --- Span 데이터 생성 ---
	fmt.Println("[1] Span 데이터 생성 (1000개)")
	fmt.Println(strings.Repeat("-", 60))

	services := map[string][]string{
		"frontend":  {"HTTP GET /", "HTTP GET /products", "HTTP POST /order"},
		"backend":   {"ProcessOrder", "ValidatePayment", "GetInventory"},
		"auth":      {"ValidateToken", "CreateSession"},
	}

	for i := 0; i < 1000; i++ {
		svcNames := []string{"frontend", "backend", "auth"}
		svc := svcNames[r.Intn(len(svcNames))]
		ops := services[svc]
		op := ops[r.Intn(len(ops))]

		baseDur := time.Duration(10+r.Intn(200)) * time.Millisecond
		if op == "ProcessOrder" {
			baseDur = time.Duration(50+r.Intn(500)) * time.Millisecond
		}

		statusCode := 0
		if r.Intn(20) == 0 { // 5% 에러율
			statusCode = 1
		}

		span := SpanRecord{
			TraceID:    fmt.Sprintf("trace-%06d", i),
			SpanID:     fmt.Sprintf("span-%06d", i),
			Service:    svc,
			Operation:  op,
			Duration:   baseDur,
			StatusCode: statusCode,
			Timestamp:  baseTime.Add(time.Duration(r.Intn(3600)) * time.Second),
		}
		index.IndexSpan(span)
	}
	fmt.Printf("  Indexed %d spans\n", len(index.spans))
	fmt.Println()

	// --- SPM 쿼리 ---
	agg := NewSPMAggregator(index)

	fmt.Println("[2] RED Metrics: frontend 서비스 (10분 버킷)")
	fmt.Println(strings.Repeat("-", 60))

	query := SPMQuery{
		Service:    "frontend",
		StartTime:  baseTime,
		EndTime:    baseTime.Add(1 * time.Hour),
		BucketSize: 10 * time.Minute,
	}
	results := agg.QueryRED(query)
	for _, m := range results {
		fmt.Printf("  %s\n", m)
	}
	fmt.Println()

	// --- 오퍼레이션별 ---
	fmt.Println("[3] RED Metrics: backend/ProcessOrder")
	fmt.Println(strings.Repeat("-", 60))

	query2 := SPMQuery{
		Service:    "backend",
		Operation:  "ProcessOrder",
		StartTime:  baseTime,
		EndTime:    baseTime.Add(1 * time.Hour),
		BucketSize: 15 * time.Minute,
	}
	results2 := agg.QueryRED(query2)
	for _, m := range results2 {
		fmt.Printf("  %s\n", m)
	}
	fmt.Println()

	// --- 서비스 오퍼레이션 목록 ---
	fmt.Println("[4] 서비스별 오퍼레이션 (span count)")
	fmt.Println(strings.Repeat("-", 60))
	for svc := range services {
		ops := agg.QueryServiceOperations(svc)
		fmt.Printf("  %s:\n", svc)
		for op, count := range ops {
			fmt.Printf("    %-30s %d spans\n", op, count)
		}
	}
	fmt.Println()

	// --- ES Query DSL ---
	fmt.Println("[5] ES Query DSL (JSON)")
	fmt.Println(strings.Repeat("-", 60))
	dsl := buildSPMQueryDSL(query)
	dslJSON, _ := json.MarshalIndent(dsl, "  ", "  ")
	fmt.Printf("  %s\n", dslJSON)
	fmt.Println()

	fmt.Println("=== 시뮬레이션 완료 ===")
}
