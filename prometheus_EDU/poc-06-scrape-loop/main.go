package main

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Prometheus Scrape Loop PoC
// =============================================================================
// Prometheus scrape loop의 핵심 동작을 시뮬레이션한다:
// 1. MetricsServer: /metrics 엔드포인트로 Prometheus text format 메트릭 노출
// 2. TextParser: Prometheus text format 파싱
// 3. ScrapeLoop: 주기적 스크레이핑 + offset 분산 + timeout + stale marker
// 4. Storage: 인메모리 append-only 스토리지
//
// 참고: prometheus/scrape/scrape.go - scrapeLoop.run(), scrapeAndReport()
//       prometheus/scrape/target.go - Target.offset()
//       prometheus/model/value/value.go - StaleNaN
// =============================================================================

// StaleNaN은 Prometheus가 시계열 소멸을 표시하는 특수 NaN 값이다.
// 실제 구현: model/value/value.go의 StaleNaN = 0x7ff0000000000002
// IEEE 754 signaling NaN으로, 일반 NaN과 구별된다.
var StaleNaN = math.Float64frombits(0x7ff0000000000002)

// IsStaleNaN은 값이 stale marker인지 확인한다.
func IsStaleNaN(v float64) bool {
	return math.Float64bits(v) == 0x7ff0000000000002
}

// =============================================================================
// 1. Storage — 인메모리 시계열 저장소
// =============================================================================

// Sample은 하나의 시계열 데이터 포인트를 나타낸다.
type Sample struct {
	MetricName string
	Labels     map[string]string
	Value      float64
	Timestamp  time.Time
}

// Storage는 append-only 인메모리 스토리지이다.
// 실제 Prometheus에서는 storage.Appender 인터페이스를 통해 TSDB에 기록한다.
type Storage struct {
	mu      sync.Mutex
	samples []Sample
}

func NewStorage() *Storage {
	return &Storage{}
}

// Append는 새 샘플을 저장소에 추가한다.
func (s *Storage) Append(name string, labels map[string]string, value float64, ts time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.samples = append(s.samples, Sample{
		MetricName: name,
		Labels:     labels,
		Value:      value,
		Timestamp:  ts,
	})
}

// Dump는 저장된 모든 샘플을 출력한다.
func (s *Storage) Dump() {
	s.mu.Lock()
	defer s.mu.Unlock()

	fmt.Println("\n========== Storage Dump ==========")
	fmt.Printf("Total samples: %d\n\n", len(s.samples))

	// 메트릭 이름별로 그룹핑
	groups := make(map[string][]Sample)
	for _, sample := range s.samples {
		key := sample.MetricName
		groups[key] = append(groups[key], sample)
	}

	// 정렬된 키로 출력
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		samples := groups[key]
		fmt.Printf("--- %s ---\n", key)
		for _, s := range samples {
			if IsStaleNaN(s.Value) {
				fmt.Printf("  [%s] labels=%v value=STALE_NaN\n",
					s.Timestamp.Format("15:04:05.000"), s.Labels)
			} else {
				fmt.Printf("  [%s] labels=%v value=%.2f\n",
					s.Timestamp.Format("15:04:05.000"), s.Labels, s.Value)
			}
		}
	}
	fmt.Println("==================================")
}

// =============================================================================
// 2. TextParser — Prometheus text format 파서
// =============================================================================

// ParsedMetric은 파싱된 하나의 메트릭 라인을 나타낸다.
type ParsedMetric struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// ParseTextFormat은 Prometheus text format을 파싱한다.
// 형식: metric_name{label1="value1",label2="value2"} 123.45
// # HELP, # TYPE 라인은 건너뛴다.
//
// 실제 Prometheus에서는 model/textparse 패키지의 파서를 사용한다.
// OpenMetrics, Protobuf 등 여러 형식을 지원하지만, 여기서는 text format만 처리한다.
func ParseTextFormat(body string) []ParsedMetric {
	var metrics []ParsedMetric
	lines := strings.Split(body, "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)

		// 빈 줄이나 주석(# HELP, # TYPE) 건너뛰기
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		metric := parseLine(line)
		if metric != nil {
			metrics = append(metrics, *metric)
		}
	}
	return metrics
}

// parseLine은 한 줄의 메트릭을 파싱한다.
// 예: http_requests_total{method="GET",code="200"} 1027
func parseLine(line string) *ParsedMetric {
	m := &ParsedMetric{
		Labels: make(map[string]string),
	}

	// 레이블이 있는 경우: metric_name{...} value
	if idx := strings.Index(line, "{"); idx >= 0 {
		m.Name = line[:idx]

		endIdx := strings.Index(line, "}")
		if endIdx < 0 {
			return nil
		}
		labelStr := line[idx+1 : endIdx]
		// 레이블 파싱: key="value", key2="value2"
		if labelStr != "" {
			pairs := splitLabels(labelStr)
			for _, pair := range pairs {
				kv := strings.SplitN(pair, "=", 2)
				if len(kv) == 2 {
					key := strings.TrimSpace(kv[0])
					val := strings.Trim(strings.TrimSpace(kv[1]), "\"")
					m.Labels[key] = val
				}
			}
		}

		rest := strings.TrimSpace(line[endIdx+1:])
		_, err := fmt.Sscanf(rest, "%f", &m.Value)
		if err != nil {
			return nil
		}
	} else {
		// 레이블이 없는 경우: metric_name value
		parts := strings.Fields(line)
		if len(parts) < 2 {
			return nil
		}
		m.Name = parts[0]
		_, err := fmt.Sscanf(parts[1], "%f", &m.Value)
		if err != nil {
			return nil
		}
	}

	return m
}

// splitLabels는 쉼표로 레이블 쌍을 분리한다.
// 값 내부의 쉼표를 올바르게 처리하기 위해 따옴표를 추적한다.
func splitLabels(s string) []string {
	var result []string
	var current strings.Builder
	inQuotes := false

	for _, c := range s {
		switch {
		case c == '"':
			inQuotes = !inQuotes
			current.WriteRune(c)
		case c == ',' && !inQuotes:
			result = append(result, current.String())
			current.Reset()
		default:
			current.WriteRune(c)
		}
	}
	if current.Len() > 0 {
		result = append(result, current.String())
	}
	return result
}

// =============================================================================
// 3. MetricsServer — /metrics 엔드포인트 HTTP 서버
// =============================================================================

// MetricsServer는 Prometheus가 스크레이핑할 대상 서버를 시뮬레이션한다.
type MetricsServer struct {
	mu       sync.Mutex
	metrics  string
	server   *http.Server
	addr     string
}

func NewMetricsServer(addr string) *MetricsServer {
	ms := &MetricsServer{addr: addr}
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", ms.handleMetrics)
	ms.server = &http.Server{Addr: addr, Handler: mux}
	return ms
}

// SetMetrics는 /metrics에서 반환할 메트릭 텍스트를 설정한다.
func (ms *MetricsServer) SetMetrics(text string) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	ms.metrics = text
}

func (ms *MetricsServer) handleMetrics(w http.ResponseWriter, r *http.Request) {
	ms.mu.Lock()
	text := ms.metrics
	ms.mu.Unlock()

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprint(w, text)
}

func (ms *MetricsServer) Start() {
	go ms.server.ListenAndServe()
}

func (ms *MetricsServer) Shutdown(ctx context.Context) {
	ms.server.Shutdown(ctx)
}

// =============================================================================
// 4. ScrapeTarget — 스크레이핑 대상 정의
// =============================================================================

// ScrapeTarget은 스크레이핑할 대상을 나타낸다.
// 실제 Prometheus에서는 scrape/target.go의 Target 구조체에 해당한다.
type ScrapeTarget struct {
	URL      string
	Labels   map[string]string // job, instance 등 외부 레이블
}

// Hash는 타겟의 해시값을 계산한다.
// 실제: Target.hash()는 레이블의 해시로 offset을 결정한다.
func (t *ScrapeTarget) Hash() uint64 {
	h := fnv.New64a()
	h.Write([]byte(t.URL))
	for k, v := range t.Labels {
		h.Write([]byte(k))
		h.Write([]byte(v))
	}
	return h.Sum64()
}

// =============================================================================
// 5. ScrapeLoop — 핵심 스크레이핑 루프
// =============================================================================

// ScrapeLoop은 하나의 타겟에 대한 주기적 스크레이핑을 수행한다.
//
// 실제 Prometheus scrapeLoop의 핵심 동작을 재현:
// 1. offset 기반 시작 시점 분산 (target.go의 offset() 함수)
// 2. interval 기반 주기적 스크레이핑 (scrapeLoop.run())
// 3. timeout 처리 (context.WithTimeout)
// 4. stale marker 생성 (사라진 메트릭 감지)
// 5. report 메트릭 (up, scrape_duration_seconds, scrape_samples_scraped)
type ScrapeLoop struct {
	target   *ScrapeTarget
	storage  *Storage
	interval time.Duration
	timeout  time.Duration

	// scrapeCache: 이전 스크레이프에서 본 메트릭을 추적한다.
	// 실제 Prometheus에서는 scrapeCache의 seriesCur/seriesPrev로 관리한다.
	prevMetrics map[string]bool // 이전 스크레이프에서 본 메트릭 키 집합
	currMetrics map[string]bool // 현재 스크레이프에서 본 메트릭 키 집합

	scrapeCount int
	ctx         context.Context
	cancel      context.CancelFunc
	stopped     chan struct{}
}

func NewScrapeLoop(target *ScrapeTarget, storage *Storage, interval, timeout time.Duration) *ScrapeLoop {
	ctx, cancel := context.WithCancel(context.Background())
	return &ScrapeLoop{
		target:      target,
		storage:     storage,
		interval:    interval,
		timeout:     timeout,
		prevMetrics: make(map[string]bool),
		currMetrics: make(map[string]bool),
		ctx:         ctx,
		cancel:      cancel,
		stopped:     make(chan struct{}),
	}
}

// metricKey는 메트릭 이름 + 레이블의 고유 키를 생성한다.
// 실제: scrapeCache의 series map 키로 사용되는 문자열과 유사하다.
func metricKey(name string, labels map[string]string) string {
	var sb strings.Builder
	sb.WriteString(name)
	sb.WriteByte('{')
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(k)
		sb.WriteString(`="`)
		sb.WriteString(labels[k])
		sb.WriteByte('"')
	}
	sb.WriteByte('}')
	return sb.String()
}

// offset는 스크레이핑 시작 시점의 오프셋을 계산한다.
//
// 실제 구현 (scrape/target.go:155):
//
//	base   = int64(interval) - now%int64(interval)
//	offset = (t.hash() ^ offsetSeed) % uint64(interval)
//	next   = base + int64(offset)
//
// 이렇게 하면 같은 타겟은 항상 같은 오프셋을 가지며,
// 여러 타겟의 스크레이핑이 시간적으로 분산된다.
func (sl *ScrapeLoop) offset() time.Duration {
	now := time.Now().UnixNano()
	interval := int64(sl.interval)

	base := interval - now%interval
	offset := int64(sl.target.Hash() % uint64(interval))
	next := base + offset

	if next > interval {
		next -= interval
	}
	return time.Duration(next)
}

// Run은 스크레이프 루프를 시작한다.
//
// 실제 scrapeLoop.run()의 흐름:
// 1. offset만큼 대기 (시간 분산)
// 2. ticker로 interval마다 scrapeAndReport() 호출
// 3. 컨텍스트 취소 시 endOfRunStaleness() 호출
func (sl *ScrapeLoop) Run() {
	defer close(sl.stopped)

	// --- Phase 1: Offset 대기 ---
	// 실제: scrapeLoop.run()에서 time.After(sl.scraper.offset(...))로 대기
	offsetDuration := sl.offset()
	fmt.Printf("[ScrapeLoop] Target: %s | Offset: %v (hash-based time distribution)\n",
		sl.target.URL, offsetDuration.Truncate(time.Millisecond))

	select {
	case <-time.After(offsetDuration):
		// offset 대기 완료
	case <-sl.ctx.Done():
		return
	}

	// --- Phase 2: 주기적 스크레이핑 ---
	ticker := time.NewTicker(sl.interval)
	defer ticker.Stop()

	var lastScrapeTime time.Time

	for {
		select {
		case <-sl.ctx.Done():
			// --- Phase 3: End-of-run stale markers ---
			// 실제: endOfRunStaleness()는 2 interval + 10% 대기 후 stale marker 기록
			// PoC에서는 즉시 stale marker를 기록한다.
			if !lastScrapeTime.IsZero() {
				sl.writeEndOfRunStaleMarkers(lastScrapeTime)
			}
			return
		default:
		}

		lastScrapeTime = sl.scrapeAndReport()

		select {
		case <-sl.ctx.Done():
			if !lastScrapeTime.IsZero() {
				sl.writeEndOfRunStaleMarkers(lastScrapeTime)
			}
			return
		case <-ticker.C:
		}
	}
}

// Stop은 스크레이프 루프를 중지한다.
func (sl *ScrapeLoop) Stop() {
	sl.cancel()
	<-sl.stopped
}

// scrapeAndReport는 한 번의 스크레이핑을 수행하고 결과를 저장한다.
//
// 실제 scrapeLoop.scrapeAndReport()의 핵심 흐름:
// 1. HTTP GET으로 /metrics 가져오기 (timeout 적용)
// 2. text format 파싱
// 3. 각 메트릭을 storage에 append
// 4. 사라진 메트릭에 stale marker 기록
// 5. report 메트릭 (up, scrape_duration_seconds, scrape_samples_scraped) 기록
func (sl *ScrapeLoop) scrapeAndReport() time.Time {
	sl.scrapeCount++
	scrapeTime := time.Now()
	start := time.Now()

	fmt.Printf("\n[Scrape #%d] %s at %s\n", sl.scrapeCount,
		sl.target.URL, scrapeTime.Format("15:04:05.000"))

	// --- HTTP GET with timeout ---
	// 실제: context.WithTimeout(sl.ctx, sl.timeout)
	ctx, cancel := context.WithTimeout(sl.ctx, sl.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", sl.target.URL+"/metrics", nil)
	if err != nil {
		fmt.Printf("  ERROR creating request: %v\n", err)
		sl.reportUp(scrapeTime, 0, time.Since(start), 0)
		return scrapeTime
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Printf("  ERROR scraping: %v\n", err)
		// 스크레이핑 실패 시: 이전 메트릭에 stale marker 기록
		sl.markAllStale(scrapeTime)
		sl.reportUp(scrapeTime, 0, time.Since(start), 0)
		return scrapeTime
	}
	defer resp.Body.Close()

	// 응답 읽기
	buf := new(strings.Builder)
	_, err = fmt.Fscanf(resp.Body, "%s", buf)
	// 전체 body를 읽기 위해 직접 처리
	bodyBytes := make([]byte, 0, 4096)
	tmp := make([]byte, 1024)
	for {
		n, readErr := resp.Body.Read(tmp)
		if n > 0 {
			bodyBytes = append(bodyBytes, tmp[:n]...)
		}
		if readErr != nil {
			break
		}
	}
	body := string(bodyBytes)

	// --- 파싱 ---
	parsed := ParseTextFormat(body)
	fmt.Printf("  Parsed %d metrics\n", len(parsed))

	// --- 현재 스크레이프의 메트릭 집합 구성 ---
	sl.currMetrics = make(map[string]bool)

	for _, m := range parsed {
		// 타겟 레이블(job, instance)을 메트릭 레이블에 병합
		allLabels := make(map[string]string)
		for k, v := range sl.target.Labels {
			allLabels[k] = v
		}
		for k, v := range m.Labels {
			allLabels[k] = v
		}

		key := metricKey(m.Name, allLabels)
		sl.currMetrics[key] = true

		sl.storage.Append(m.Name, allLabels, m.Value, scrapeTime)
	}

	// --- Stale marker: 이전에 있었지만 현재 없는 메트릭 감지 ---
	// 실제: scrapeCache.forEachStale()에서 seriesPrev에 있지만 seriesCur에 없는 시리즈 탐지
	staleCount := 0
	for key := range sl.prevMetrics {
		if !sl.currMetrics[key] {
			staleCount++
			// stale marker 기록 — StaleNaN 값 사용
			name, labels := parseMetricKey(key)
			sl.storage.Append(name, labels, StaleNaN, scrapeTime)
			fmt.Printf("  STALE: %s (metric disappeared → StaleNaN marker)\n", key)
		}
	}

	// 이전/현재 교체 (실제: scrapeCache.iterDone()에서 cur↔prev swap)
	sl.prevMetrics = sl.currMetrics

	duration := time.Since(start)
	sl.reportUp(scrapeTime, 1, duration, len(parsed))
	fmt.Printf("  Duration: %v | Samples: %d | Stale: %d\n", duration.Truncate(time.Microsecond), len(parsed), staleCount)

	return scrapeTime
}

// reportUp은 스크레이핑 결과에 대한 report 메트릭을 기록한다.
//
// 실제 scrapeLoop.report()에서 기록하는 메트릭:
// - up: 타겟 상태 (1=정상, 0=실패)
// - scrape_duration_seconds: 스크레이핑 소요 시간
// - scrape_samples_scraped: 수집된 샘플 수
// - scrape_samples_post_metric_relabeling: relabel 후 샘플 수
// - scrape_series_added: 새로 추가된 시리즈 수
func (sl *ScrapeLoop) reportUp(ts time.Time, up float64, duration time.Duration, samplesScraped int) {
	reportLabels := make(map[string]string)
	for k, v := range sl.target.Labels {
		reportLabels[k] = v
	}

	sl.storage.Append("up", reportLabels, up, ts)
	sl.storage.Append("scrape_duration_seconds", reportLabels, duration.Seconds(), ts)
	sl.storage.Append("scrape_samples_scraped", reportLabels, float64(samplesScraped), ts)
}

// markAllStale은 모든 이전 메트릭에 stale marker를 기록한다.
// 스크레이핑이 완전히 실패했을 때 호출된다.
func (sl *ScrapeLoop) markAllStale(ts time.Time) {
	for key := range sl.prevMetrics {
		name, labels := parseMetricKey(key)
		sl.storage.Append(name, labels, StaleNaN, ts)
		fmt.Printf("  STALE (scrape failed): %s\n", key)
	}
	sl.prevMetrics = make(map[string]bool)
}

// writeEndOfRunStaleMarkers는 스크레이프 루프 종료 시 stale marker를 기록한다.
//
// 실제 endOfRunStaleness() (scrape.go:1446):
// - 루프 종료 후 2 interval + 10% 대기
// - 타겟이 재생성되지 않으면 stale marker 기록
// - 이렇게 하면 타겟이 다른 Prometheus 인스턴스로 이동해도
//   기존 인스턴스에서 stale 처리가 된다
func (sl *ScrapeLoop) writeEndOfRunStaleMarkers(lastScrape time.Time) {
	staleTime := time.Now()
	fmt.Printf("\n[End-of-run Staleness] Writing stale markers for all active series at %s\n",
		staleTime.Format("15:04:05.000"))

	for key := range sl.prevMetrics {
		name, labels := parseMetricKey(key)
		sl.storage.Append(name, labels, StaleNaN, staleTime)
		fmt.Printf("  END-STALE: %s\n", key)
	}

	// report 메트릭도 stale 처리
	reportLabels := make(map[string]string)
	for k, v := range sl.target.Labels {
		reportLabels[k] = v
	}
	sl.storage.Append("up", reportLabels, StaleNaN, staleTime)
	sl.storage.Append("scrape_duration_seconds", reportLabels, StaleNaN, staleTime)
	sl.storage.Append("scrape_samples_scraped", reportLabels, StaleNaN, staleTime)
	fmt.Println("  END-STALE: report metrics (up, scrape_duration_seconds, scrape_samples_scraped)")
}

// parseMetricKey는 metricKey()로 생성된 키를 다시 이름+레이블로 분해한다.
func parseMetricKey(key string) (string, map[string]string) {
	labels := make(map[string]string)

	idx := strings.Index(key, "{")
	if idx < 0 {
		return key, labels
	}
	name := key[:idx]
	labelStr := key[idx+1 : len(key)-1] // {...} 제거

	if labelStr == "" {
		return name, labels
	}

	pairs := splitLabels(labelStr)
	for _, pair := range pairs {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) == 2 {
			k := strings.TrimSpace(kv[0])
			v := strings.Trim(strings.TrimSpace(kv[1]), "\"")
			labels[k] = v
		}
	}
	return name, labels
}

// =============================================================================
// 6. Demo — 전체 동작 시연
// =============================================================================

func main() {
	fmt.Println("=== Prometheus Scrape Loop PoC ===")
	fmt.Println()

	// --- 스토리지 초기화 ---
	store := NewStorage()

	// --- 메트릭 서버 시작 ---
	metricsAddr := "127.0.0.1:19090"
	metricsServer := NewMetricsServer(metricsAddr)

	// 초기 메트릭: counter + gauge (레이블 포함)
	initialMetrics := `# HELP http_requests_total The total number of HTTP requests.
# TYPE http_requests_total counter
http_requests_total{method="GET",code="200"} 1027
http_requests_total{method="POST",code="200"} 342
# HELP process_cpu_seconds_total Total user and system CPU time spent in seconds.
# TYPE process_cpu_seconds_total counter
process_cpu_seconds_total 45.23
# HELP go_goroutines Number of goroutines that currently exist.
# TYPE go_goroutines gauge
go_goroutines 42
# HELP temperature_celsius Current temperature.
# TYPE temperature_celsius gauge
temperature_celsius{location="server_room"} 23.5
`
	metricsServer.SetMetrics(initialMetrics)
	metricsServer.Start()
	defer metricsServer.Shutdown(context.Background())

	// 서버 시작 대기
	time.Sleep(100 * time.Millisecond)

	fmt.Printf("Metrics server started at http://%s/metrics\n", metricsAddr)

	// --- 스크레이프 루프 설정 ---
	target := &ScrapeTarget{
		URL: "http://" + metricsAddr,
		Labels: map[string]string{
			"job":      "demo-app",
			"instance": metricsAddr,
		},
	}

	// interval=500ms, timeout=200ms (데모용으로 짧게 설정)
	scrapeLoop := NewScrapeLoop(target, store, 500*time.Millisecond, 200*time.Millisecond)

	// --- Phase 1: 정상 스크레이핑 3회 ---
	fmt.Println("\n--- Phase 1: Normal scraping (3 iterations) ---")

	go scrapeLoop.Run()

	// 3회 스크레이핑 대기 (offset + 3 intervals)
	time.Sleep(2200 * time.Millisecond)

	// --- Phase 2: 메트릭 일부 제거 (temperature_celsius 사라짐) ---
	fmt.Println("\n--- Phase 2: Metric disappears (temperature_celsius removed) ---")

	reducedMetrics := `# HELP http_requests_total The total number of HTTP requests.
# TYPE http_requests_total counter
http_requests_total{method="GET",code="200"} 2054
http_requests_total{method="POST",code="200"} 684
# HELP process_cpu_seconds_total Total user and system CPU time spent in seconds.
# TYPE process_cpu_seconds_total counter
process_cpu_seconds_total 90.46
# HELP go_goroutines Number of goroutines that currently exist.
# TYPE go_goroutines gauge
go_goroutines 38
`
	metricsServer.SetMetrics(reducedMetrics)

	// 1회 스크레이핑 대기 — stale marker 확인
	time.Sleep(800 * time.Millisecond)

	// --- Phase 3: 스크레이프 루프 중지 (end-of-run stale markers) ---
	fmt.Println("\n--- Phase 3: Stopping scrape loop (end-of-run stale markers) ---")

	scrapeLoop.Stop()

	// --- 결과 출력 ---
	store.Dump()

	// --- 파싱 데모 ---
	fmt.Println("\n=== Text Format Parsing Demo ===")
	sampleText := `# HELP http_requests_total Total requests
# TYPE http_requests_total counter
http_requests_total{method="GET",code="200"} 100
http_requests_total{method="POST",code="500"} 5
up 1
`
	parsed := ParseTextFormat(sampleText)
	for _, m := range parsed {
		fmt.Printf("  name=%-35s labels=%v  value=%.0f\n", m.Name, m.Labels, m.Value)
	}

	// --- StaleNaN 데모 ---
	fmt.Println("\n=== StaleNaN Demo ===")
	fmt.Printf("  StaleNaN bits:   0x%016x\n", math.Float64bits(StaleNaN))
	fmt.Printf("  Regular NaN bits: 0x%016x\n", math.Float64bits(math.NaN()))
	fmt.Printf("  IsStaleNaN(StaleNaN): %v\n", IsStaleNaN(StaleNaN))
	fmt.Printf("  IsStaleNaN(NaN):      %v\n", IsStaleNaN(math.NaN()))
	fmt.Printf("  IsStaleNaN(1.0):      %v\n", IsStaleNaN(1.0))

	fmt.Println("\n=== Done ===")
}
